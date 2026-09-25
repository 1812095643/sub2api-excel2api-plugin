package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/tidwall/gjson"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"local.sub2api/openai-health/internal/modeltrace"
)

const (
	PluginID = "local.personal.openai-account-health"
	Version  = "0.5.0"
)

type diagnosticResult struct {
	AccountID   int64   `json:"account_id"`
	Model       string  `json:"model"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason,omitempty"`
	Predicted   string  `json:"predicted_model,omitempty"`
	Probability float64 `json:"probability,omitempty"`
	ParsedCount int     `json:"parsed_number_count,omitempty"`
	HTTPStatus  int     `json:"http_status,omitempty"`
	Timezone    string  `json:"timezone,omitempty"`
}

type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	child              *Child
	direct             *directTransport
	config             atomic.Pointer[healthConfig]
	configMu           sync.Mutex
	closed             bool
	broker             *hcplugin.GRPCBroker
	hostMu             sync.Mutex
	hostConn           *grpc.ClientConn
	host               pluginv1.HostServiceClient
	stopServices       func()
	resultMu           sync.Mutex
	lastResults        []diagnosticResult
	diagnosticRunning  bool
	diagnosticStopping bool
	diagnosticError    string
	diagnosticCancel   context.CancelFunc
	scheduleMu         sync.Mutex
	scheduleCancel     context.CancelFunc
	basispointsMu      sync.Mutex
	basispointsCalls   map[string]map[string]any
	basispointsCallIDs []string
}

func New(child *Child) *Server {
	server := &Server{child: child, direct: newDirectTransport(), basispointsCalls: map[string]map[string]any{}}
	server.config.Store(defaultHealthConfig())
	return server
}

func (s *Server) Close() {
	s.configMu.Lock()
	s.closed = true
	s.configureScheduler(nil)
	s.stopDiagnostics()
	s.configMu.Unlock()
	s.hostMu.Lock()
	if s.stopServices != nil {
		s.stopServices()
	}
	if s.hostConn != nil {
		_ = s.hostConn.Close()
	}
	s.hostMu.Unlock()
	s.direct.close()
	if s.child != nil {
		s.child.Close()
	}
}

func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{PluginId: PluginID, PluginVersion: Version, ProtocolVersion: 1, TransportApiVersion: 1, Capabilities: []string{"openai.oauth.outbound_transport.v1"}}, nil
}

func (s *Server) ValidateConfig(ctx context.Context, request *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	base, config, err := parseHealthConfig(request.GetConfigJson())
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Message: err.Error()}, nil
	}
	result, err := s.child.API.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: base})
	if err != nil || result == nil || !result.Valid {
		return result, err
	}
	if len(result.NormalizedConfigJson) > 0 {
		base = result.NormalizedConfigJson
	}
	result.NormalizedConfigJson, err = joinHealthConfig(base, config)
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Message: err.Error()}, nil
	}
	return result, nil
}

func (s *Server) ApplyConfig(ctx context.Context, request *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.closed {
		return &pluginv1.ApplyConfigResponse{Message: "插件正在停止，请重新启用后保存。"}, nil
	}
	base, config, err := parseHealthConfig(request.GetConfigJson())
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	result, err := s.child.API.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: base})
	if err == nil && result != nil && result.Applied {
		// 宿主在检测前也会应用配置，相同配置不重置 Cron 或条件触发状态。
		previous := s.config.Load()
		if !reflect.DeepEqual(previous, config) {
			s.config.Store(config)
			if !config.DiagnosticEnabled {
				s.stopDiagnostics()
			}
			// 调整 Excel2API 或时区不会重复触发“启动后运行一次”；调度器每轮读取最新配置。
			if previous == nil || previous.DiagnosticEnabled != config.DiagnosticEnabled || previous.ScheduleMode != config.ScheduleMode || previous.ScheduleCron != config.ScheduleCron || previous.ScheduleCondition != config.ScheduleCondition {
				s.configureScheduler(config)
			}
		}
	}
	return result, err
}

func (s *Server) Health(ctx context.Context, request *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	if s.child.Client.Exited() {
		return &pluginv1.HealthResponse{Message: "原始 OpenAI 传输进程已退出，请重新启用插件"}, nil
	}
	result, err := s.child.API.Health(ctx, request)
	if err != nil || result == nil {
		return result, err
	}
	var status map[string]any
	if json.Unmarshal([]byte(result.StatusJson), &status) != nil || status == nil {
		status = map[string]any{}
	}
	for key, value := range s.runtimeStatus() {
		status[key] = value
	}
	encoded, _ := json.Marshal(status)
	result.StatusJson = string(encoded)
	return result, nil
}

func (s *Server) TestConfig(ctx context.Context, request *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	_, config, err := parseHealthConfig(request.GetConfigJson())
	if err != nil {
		return &pluginv1.TestConfigResponse{Message: err.Error()}, nil
	}
	started, startErr := s.startDiagnostics(ctx, config)
	statusJSON, _ := json.Marshal(s.runtimeStatus())
	if startErr != nil {
		return &pluginv1.TestConfigResponse{Success: false, Message: startErr.Error(), StatusJson: string(statusJSON)}, nil
	}
	if !started {
		return &pluginv1.TestConfigResponse{Success: true, Message: "检测任务已经在运行中。", StatusJson: string(statusJSON)}, nil
	}
	return &pluginv1.TestConfigResponse{Success: true, Message: "检测任务已开始，页面会自动等待并行结果。", StatusJson: string(statusJSON)}, nil
}

func (s *Server) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return forwardError(stream, "INVALID_REQUEST", "首帧必须是请求元数据", false)
	}
	config := s.config.Load()
	needsBody := eligible(start)
	if needsBody {
		if start.ContentLength > MaxBodyBytes {
			return forwardError(stream, "REQUEST_TOO_LARGE", "请求体超过 64 MiB", false)
		}
		body, readErr := readBody(stream)
		if readErr != nil {
			return forwardError(stream, "INVALID_REQUEST", readErr.Error(), false)
		}
		prepared, preparedBody, _, _, prepareErr := prepareLocaleRequest(stream.Context(), start, body, config)
		if prepareErr != nil {
			return forwardError(stream, "LOCALE_NORMALIZATION_FAILED", prepareErr.Error(), false)
		}
		start = prepared
		if config.directFor(start) {
			return s.forwardDirect(stream, start, preparedBody)
		}
		first = &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: start}}
		return s.forwardChild(stream, first, preparedBody, true)
	}
	copyStart := proto.Clone(start).(*pluginv1.ForwardRequestStart)
	copyStart.Headers = cloneHeaders(start.Headers)
	languageChanged := false
	if start.Platform == "openai" && start.AccountType == "oauth" {
		languageChanged = normalizeAcceptLanguage(copyStart.Headers)
	}
	if languageChanged {
		first = &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: copyStart}}
	}
	return s.forwardChild(stream, first, nil, false)
}

func (s *Server) forwardChild(stream pluginv1.TransportPlugin_ForwardServer, first *pluginv1.ForwardRequest, buffered []byte, bufferedRequest bool) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	childStream, err := s.child.API.Forward(ctx)
	if err != nil {
		return forwardError(stream, "TRANSPORT_UNAVAILABLE", "原始传输程序未接受请求，请检查插件状态", false)
	}
	go func() {
		if sendErr := sendRequest(childStream, stream, first, buffered, bufferedRequest); sendErr != nil {
			cancel()
		}
	}()
	for {
		response, recvErr := childStream.Recv()
		if recvErr != nil {
			if stream.Context().Err() != nil {
				return stream.Context().Err()
			}
			return forwardError(stream, "TRANSPORT_INTERRUPTED", "原始传输连接中断；未自动重试", true)
		}
		if err := stream.Send(response); err != nil {
			return err
		}
		if response.GetEnd() != nil || response.GetError() != nil {
			return nil
		}
	}
}

func (s *Server) SetHostBroker(broker *hcplugin.GRPCBroker) { s.broker = broker }

func (s *Server) InitHostServices(ctx context.Context, request *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if s.hostConn != nil {
		return &pluginv1.InitHostServicesResponse{Ready: true}, nil
	}
	if s.broker == nil || s.child.Broker == nil {
		return &pluginv1.InitHostServicesResponse{Message: "宿主服务连接尚未准备好"}, nil
	}
	conn, err := s.broker.Dial(request.HostServiceId)
	if err != nil {
		return nil, err
	}
	s.hostConn = conn
	s.host = pluginv1.NewHostServiceClient(conn)
	childID := s.child.Broker.NextId()
	relay := &hostRelay{api: s.host}
	ready := make(chan *grpc.Server, 1)
	go s.child.Broker.AcceptAndServe(childID, func(options []grpc.ServerOption) *grpc.Server {
		server := grpc.NewServer(options...)
		pluginv1.RegisterHostServiceServer(server, relay)
		ready <- server
		return server
	})
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := s.child.API.InitHostServices(initCtx, &pluginv1.InitHostServicesRequest{HostServiceId: childID, HostServiceApiVersion: request.HostServiceApiVersion})
	select {
	case server := <-ready:
		s.stopServices = server.Stop
	default:
	}
	return response, err
}

func diagnosticOutput(raw []byte) (string, bool) {
	var output strings.Builder
	completedText := ""
	completed := false
	streamed := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		streamed = true
		kind := gjson.Get(payload, "type").String()
		if kind == "response.output_text.delta" {
			output.WriteString(gjson.Get(payload, "delta").String())
		}
		if kind == "response.completed" {
			completed = true
			completedText = diagnosticResponseText(gjson.Get(payload, "response"))
		}
	}
	if streamed {
		if output.Len() == 0 {
			return completedText, completed
		}
		return output.String(), completed
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || gjson.GetBytes(raw, "status").String() != "completed" {
		return "", false
	}
	return diagnosticResponseText(gjson.ParseBytes(raw)), true
}

func diagnosticResponseText(response gjson.Result) string {
	if text := response.Get("output_text").String(); text != "" {
		return text
	}
	var text strings.Builder
	for _, message := range response.Get("output").Array() {
		for _, part := range message.Get("content").Array() {
			if part.Get("type").String() == "output_text" {
				text.WriteString(part.Get("text").String())
			}
		}
	}
	return text.String()
}

func (s *Server) runDiagnostics(ctx context.Context, config *healthConfig) ([]diagnosticResult, error) {
	s.hostMu.Lock()
	host := s.host
	s.hostMu.Unlock()
	if host == nil {
		return nil, errors.New("宿主账号目录不可用，请重启启用插件")
	}
	accounts := append([]int64(nil), config.DiagnosticAccountIDs...)
	listCtx := ctx
	if config.DiagnosticSchedulable {
		listCtx = metadata.NewOutgoingContext(ctx, metadata.Pairs("sub2api-schedulable-only", "true"))
	}
	if len(accounts) == 0 {
		listed, err := host.ListAccounts(listCtx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
		if err != nil {
			return nil, fmt.Errorf("读取 OpenAI OAuth 账号失败: %w", err)
		}
		accounts = listed.AccountIds
	}
	jobs := make(chan diagnosticJob, config.DiagnosticConcurrency)
	resultsCh := make(chan diagnosticResult, len(accounts)*len(config.DiagnosticModels)+len(accounts))
	workers := config.DiagnosticConcurrency
	if workers < 1 {
		workers = 1
	}
	if workers > maxDiagnosticConcurrency {
		workers = maxDiagnosticConcurrency
	}
	var workerGroup sync.WaitGroup
	for index := 0; index < workers; index++ {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				resultsCh <- s.runDiagnosticJob(ctx, config, job)
			}
		}()
	}
	// 取消后工作协程可以提前退出，结果通道须等待生产者也停止后才能关闭。
	workerGroup.Add(1)
	go func() {
		defer workerGroup.Done()
		defer close(jobs)
		for _, accountID := range accounts {
			if ctx.Err() != nil {
				return
			}
			identityCtx := ctx
			if config.DiagnosticSchedulable {
				identityCtx = metadata.NewOutgoingContext(ctx, metadata.Pairs("sub2api-schedulable-only", "true"))
			}
			identity, err := host.ResolveOutboundIdentity(identityCtx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: accountID})
			if ctx.Err() != nil {
				return
			}
			if err != nil || identity == nil || !identity.Found {
				for _, model := range config.DiagnosticModels {
					resultsCh <- diagnosticResult{AccountID: accountID, Model: model, Status: "skipped", Reason: "账号不可用或未启用调度"}
				}
				continue
			}
			for _, model := range config.DiagnosticModels {
				if ctx.Err() != nil {
					return
				}
				select {
				case jobs <- diagnosticJob{AccountID: accountID, Model: model, Identity: identity}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() {
		workerGroup.Wait()
		close(resultsCh)
	}()
	results := make([]diagnosticResult, 0, len(accounts)*len(config.DiagnosticModels))
	for item := range resultsCh {
		results = append(results, item)
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

type diagnosticJob struct {
	AccountID int64
	Model     string
	Identity  *pluginv1.ResolveOutboundIdentityResponse
}

func (s *Server) runDiagnosticJob(ctx context.Context, config *healthConfig, job diagnosticJob) diagnosticResult {
	item := diagnosticResult{AccountID: job.AccountID, Model: job.Model, Status: "failed", Timezone: config.timezoneForAccount(job.AccountID)}
	if ctx.Err() != nil {
		item.Status, item.Reason = "skipped", "检测已停止"
		return item
	}
	challenge, challengeErr := modeltrace.NewModelTraceChallenge()
	if challengeErr != nil {
		item.Reason = "challenge_generation_failed"
		return item
	}
	body, _ := json.Marshal(map[string]any{"model": job.Model, "stream": true, "store": false, "input": []any{map[string]any{"role": "user", "content": challenge.Prompt}}})
	responseBody, statusCode, requestErr := s.runDiagnosticRequest(ctx, job.Identity, job.AccountID, body, config)
	item.HTTPStatus = statusCode
	if requestErr != nil {
		if ctx.Err() != nil {
			item.Status, item.Reason = "skipped", "检测已停止"
			return item
		}
		item.Reason = requestErr.Error()
		return item
	}
	text, complete := diagnosticOutput(responseBody)
	if !complete {
		item.Reason = "response_incomplete"
		return item
	}
	prediction, predictErr := modeltrace.ModelTracePredict(text, challenge.ExpectedCount)
	item.ParsedCount = prediction.ParsedCount
	item.Predicted = prediction.Model
	item.Probability = prediction.Probability
	if predictErr != nil {
		item.Reason = "insufficient_numbers"
		return item
	}
	if prediction.Model == job.Model {
		item.Status = "normal"
	} else {
		item.Status = "degraded"
		item.Reason = "fingerprint_mismatch"
	}
	return item
}

func (s *Server) startDiagnostics(trigger context.Context, config *healthConfig) (bool, error) {
	// 所有启动入口与配置应用共用一把锁，防止旧定时回调在关闭后重启检测。
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := trigger.Err(); err != nil {
		return false, err
	}
	if s.closed {
		return false, errors.New("插件正在停止，请重新启用后重试。")
	}
	current := s.config.Load()
	if config == nil || !config.DiagnosticEnabled || current == nil || !current.DiagnosticEnabled {
		return false, errors.New("测试总开关已关闭，请开启并保存后再检测。")
	}
	if len(config.DiagnosticModels) == 0 {
		return false, errors.New("请先填写至少一个检测模型并保存。")
	}
	s.hostMu.Lock()
	hostReady := s.host != nil
	s.hostMu.Unlock()
	if !hostReady {
		return false, errors.New("宿主账号目录尚未连接，请保持插件启用后重试。")
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.diagnosticRunning {
		if s.diagnosticStopping {
			return false, errors.New("正在停止上一轮检测，请稍后再开始。")
		}
		return false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.diagnosticRunning = true
	s.diagnosticStopping = false
	s.diagnosticError = ""
	s.diagnosticCancel = cancel
	go func() {
		defer cancel()
		items, err := s.runDiagnostics(ctx, config)
		s.resultMu.Lock()
		s.lastResults = append([]diagnosticResult(nil), items...)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.diagnosticError = err.Error()
		}
		s.diagnosticRunning = false
		s.diagnosticStopping = false
		s.diagnosticCancel = nil
		s.resultMu.Unlock()
	}()
	return true, nil
}

func (s *Server) stopDiagnostics() {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.diagnosticCancel != nil {
		s.diagnosticStopping = true
		s.diagnosticCancel()
	}
}

func (s *Server) runtimeStatus() map[string]any {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	config := s.config.Load()
	return map[string]any{
		"codex_tool_catalog": map[string]any{
			"repository": "openai/codex",
			"commit":     "5c8fc15cc99241d3a1a415a629841540cdff4b28",
			"tool_count": len(bpCodexToolCatalog()),
		},
		"request_locale": map[string]any{
			"default_timezone": config.DefaultTimezone, "timezone_overrides": len(config.AccountTimezones),
			"accept_language": "en-US,en;q=0.9", "diagnostic_concurrency": config.DiagnosticConcurrency,
			"schedule_mode": config.ScheduleMode, "schedule_cron": config.ScheduleCron, "schedule_condition": config.ScheduleCondition,
		},
		"excel2api": map[string]any{"enabled": config.DirectEnabled, "account_count": len(config.DirectAccountIDs), "endpoint": directResponsesURL},
		// 保留旧字段，避免旧版插件页面读取状态时出现空值。
		"direct": map[string]any{"enabled": config.DirectEnabled, "account_count": len(config.DirectAccountIDs), "endpoint": directResponsesURL},
		"diagnostics": map[string]any{
			"enabled":           config.DiagnosticEnabled,
			"running":           s.diagnosticRunning,
			"stopping":          s.diagnosticStopping,
			"error":             s.diagnosticError,
			"last_results":      append([]diagnosticResult(nil), s.lastResults...),
			"modeltrace_commit": modeltrace.ModelTraceBankCommit(),
		},
	}
}

func (s *Server) runDiagnosticRequest(ctx context.Context, identity *pluginv1.ResolveOutboundIdentityResponse, accountID int64, body []byte, config *healthConfig) ([]byte, int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	if err := requestCtx.Err(); err != nil {
		return nil, 0, err
	}
	headers := cloneHeaders(identity.Headers)
	if headers == nil {
		headers = map[string]*pluginv1.HeaderValues{}
	}
	headers["Authorization"] = &pluginv1.HeaderValues{Values: []string{"Bearer " + identity.Token}}
	headers["Content-Type"] = &pluginv1.HeaderValues{Values: []string{"application/json"}}
	headers["Accept"] = &pluginv1.HeaderValues{Values: []string{"text/event-stream"}}
	headers["OpenAI-Beta"] = &pluginv1.HeaderValues{Values: []string{"responses=experimental"}}
	normalizeAcceptLanguage(headers)
	normalized, _ := normalizeRequestLocale(body, accountID, config.timezoneForAccount(accountID), time.Now())
	start := &pluginv1.ForwardRequestStart{RequestId: fmt.Sprintf("diagnostic-%d-%d", accountID, time.Now().UnixNano()), Method: http.MethodPost, Url: "https://chatgpt.com/backend-api/codex/responses", Host: "chatgpt.com", Headers: headers, ProxyUrl: identity.ProxyUrl, AccountId: accountID, Platform: "openai", AccountType: "oauth", ContentLength: int64(len(normalized)), HasBody: true}
	// 检测和业务请求复用同一账号路由与鉴权构造，确保检测的是该账号实际使用的线路。
	if config.directFor(start) {
		response, err := s.directResponse(requestCtx, start, normalized, identity)
		if err != nil {
			var problem *directRequestError
			if errors.As(err, &problem) {
				return nil, problem.status, problem
			}
			if requestCtx.Err() != nil {
				return nil, 0, requestCtx.Err()
			}
			return nil, 0, errors.New("直连接口请求未完成，请检查账号代理与目标接口可用性")
		}
		defer response.Body.Close()
		result, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		if err != nil {
			return nil, response.StatusCode, errors.New("直连响应读取中断")
		}
		if len(result) > 4<<20 {
			return nil, response.StatusCode, errors.New("响应超过 4 MiB")
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return result, response.StatusCode, fmt.Errorf("直连接口 HTTP %d", response.StatusCode)
		}
		return result, response.StatusCode, nil
	}
	stream, err := s.child.API.Forward(requestCtx)
	if err != nil {
		return nil, 0, err
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: start}}); err != nil {
		return nil, 0, err
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: normalized}}); err != nil {
		return nil, 0, err
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}); err != nil {
		return nil, 0, err
	}
	_ = stream.CloseSend()
	var response bytes.Buffer
	statusCode := 0
	for {
		frame, recvErr := stream.Recv()
		if recvErr != nil {
			return response.Bytes(), statusCode, recvErr
		}
		if startFrame := frame.GetStart(); startFrame != nil {
			statusCode = int(startFrame.StatusCode)
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			if response.Len()+len(chunk) > 4<<20 {
				return response.Bytes(), statusCode, errors.New("响应超过 4 MiB")
			}
			_, _ = response.Write(chunk)
		}
		if upstreamError := frame.GetError(); upstreamError != nil {
			return response.Bytes(), statusCode, errors.New(upstreamError.Message)
		}
		if frame.GetEnd() != nil {
			if statusCode < 200 || statusCode >= 300 {
				return response.Bytes(), statusCode, fmt.Errorf("上游 HTTP %d", statusCode)
			}
			return response.Bytes(), statusCode, nil
		}
	}
}
