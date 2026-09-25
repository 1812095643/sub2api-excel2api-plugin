package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

const directResponsesURL = "https://bps.openai.com/basispoints/api/responses"

type directRequestError struct {
	status  int
	code    string
	message string
}

func (e *directRequestError) Error() string { return e.message }

// 只匹配显式列出的宿主账号及普通 Responses；compact 和其他接口继续使用原线路。
func (c *healthConfig) directFor(start *pluginv1.ForwardRequestStart) bool {
	if c == nil || !c.DirectEnabled || !eligible(start) {
		return false
	}
	u, err := url.Parse(start.Url)
	if err != nil {
		return false
	}
	switch strings.TrimRight(u.Path, "/") {
	case "/responses", "/v1/responses", "/backend-api/codex/responses":
	default:
		return false
	}
	for _, id := range c.DirectAccountIDs {
		if id == start.AccountId {
			return true
		}
	}
	return false
}

func (s *Server) prepareDirectBody(start *pluginv1.ForwardRequestStart, body []byte) (map[string]any, map[string]any, error) {
	plain, err := decodeBody(body, strings.ToLower(strings.TrimSpace(headerValue(start.Headers, "Content-Encoding"))))
	if err != nil {
		return nil, nil, &directRequestError{http.StatusBadRequest, "invalid_request_body", err.Error()}
	}
	source, err := bpDecodeSource(plain)
	if err != nil {
		return nil, nil, &directRequestError{http.StatusBadRequest, "invalid_request_body", err.Error()}
	}
	prepared, err := s.bpPrepareResponsesBody(source)
	if err != nil {
		return nil, nil, &directRequestError{http.StatusBadRequest, "invalid_request_body", err.Error()}
	}
	return prepared, source, nil
}

// JWT 只用于读取账号标识；不以本地解码结果授予权限，Token 仍由上游校验。
func directAccountID(identity *pluginv1.ResolveOutboundIdentityResponse) (string, error) {
	if value := headerValue(identity.Headers, "chatgpt-account-id"); value != "" {
		if validDirectIdentityValue(value) {
			return value, nil
		}
		return "", errors.New("宿主返回的 ChatGPT 账号标识格式不正确")
	}
	parts := strings.Split(identity.Token, ".")
	if len(parts) != 3 {
		return "", errors.New("当前账号缺少 ChatGPT 账号标识，且 access token 不是普通 ChatGPT JWT")
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil || len(claimsBytes) > 64<<10 {
		return "", errors.New("无法从 access token 读取 ChatGPT 账号标识")
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(claimsBytes, &claims) != nil || !validDirectIdentityValue(claims.Auth.AccountID) {
		return "", errors.New("access token 未包含有效的 chatgpt_account_id")
	}
	return claims.Auth.AccountID, nil
}

func validDirectIdentityValue(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, char := range value {
		if char <= ' ' || char >= 127 || char == ',' {
			return false
		}
	}
	return true
}

func setDirectBPSIdentityHeaders(header http.Header, token, accountID string) {
	header.Set("Origin", "https://bps.openai.com")
	header.Set("Authorization", "Bearer "+token)
	header.Set("chatgpt-account-id", accountID)
	header.Set("x-openai-account-id", accountID)
	header.Set("x-basispoints-auth-mode", "chatgpt")
	header.Set("x-openai-internal-basispoints-client-agent-profile", "excel")
	header.Set("x-openai-internal-basispoints-client-editor", "excel")
	header.Set("x-openai-internal-basispoints-client-host", "office")
	header.Set("x-openai-internal-basispoints-client-platform", "excel")
	header.Set("x-openai-internal-basispoints-client-platform-class", "PC")
	header.Set("x-openai-internal-basispoints-client-product", "basispoints-excel-plugin")
	header.Set("x-openai-internal-basispoints-client-runtime", "desktop")
	header.Set("x-openai-internal-basispoints-office-host", "Excel")
	header.Set("x-openai-internal-basispoints-office-platform", "PC")
	header.Set("User-Agent", "openai-account-health/"+Version)
}

func buildDirectHTTPRequest(ctx context.Context, start *pluginv1.ForwardRequestStart, body []byte, identity *pluginv1.ResolveOutboundIdentityResponse) (*http.Request, error) {
	if identity == nil || !identity.Found || identity.AccountId != start.AccountId || identity.Platform != "openai" || identity.AccountType != "oauth" {
		return nil, &directRequestError{http.StatusBadGateway, "direct_identity_unavailable", "无法取得指定 OpenAI OAuth 账号的出站身份，请检查账号状态。"}
	}
	token := strings.TrimSpace(identity.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, &directRequestError{http.StatusBadGateway, "direct_token_unavailable", "当前账号没有可用的 access token，请先更新账号授权。"}
	}
	accountID, err := directAccountID(identity)
	if err != nil {
		return nil, &directRequestError{http.StatusBadGateway, "direct_account_id_unavailable", err.Error()}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, directResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 新入口只携带 BPS 需要的请求头，避免转发旧入口的 Cookie、路由票据或 Host。
	for _, name := range []string{"Content-Type", "Accept", "Accept-Language"} {
		if value := headerValue(start.Headers, name); value != "" {
			request.Header.Set(name, value)
		}
	}
	if request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "text/event-stream")
	}
	request.Header.Set("Accept-Encoding", "identity")
	setDirectBPSIdentityHeaders(request.Header, token, accountID)
	return request, nil
}

type directTransport struct {
	mu      sync.Mutex
	clients map[string]*http.Client
	closed  bool
}

func newDirectTransport() *directTransport {
	return &directTransport{clients: map[string]*http.Client{}}
}

func (t *directTransport) client(accountID int64, proxyURL string) (*http.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("插件正在停止")
	}
	key := fmt.Sprintf("%d\x00%s", accountID, proxyURL)
	if client := t.clients[key]; client != nil {
		return client, nil
	}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 120 * time.Second,
		MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: time.Minute,
		ForceAttemptHTTP2: true, DisableCompression: true,
	}
	if proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil || proxy.Hostname() == "" {
			return nil, errors.New("账号代理地址不正确，请检查代理配置")
		}
		switch strings.ToLower(proxy.Scheme) {
		case "http", "https", "socks5", "socks5h":
			proxy.Scheme = strings.ToLower(proxy.Scheme)
			transport.Proxy = http.ProxyURL(proxy)
		default:
			return nil, errors.New("账号代理仅支持 HTTP、HTTPS、SOCKS5 或 SOCKS5H")
		}
	}
	if len(t.clients) >= 128 {
		for oldKey, client := range t.clients {
			client.CloseIdleConnections()
			delete(t.clients, oldKey)
			break
		}
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.clients[key] = client
	return client, nil
}

func (t *directTransport) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, client := range t.clients {
		client.CloseIdleConnections()
	}
	t.clients = nil
}

func (s *Server) directResponse(ctx context.Context, start *pluginv1.ForwardRequestStart, body []byte, identity *pluginv1.ResolveOutboundIdentityResponse) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prepared, source, err := s.prepareDirectBody(start, body)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		s.hostMu.Lock()
		host := s.host
		s.hostMu.Unlock()
		if host == nil {
			return nil, &directRequestError{http.StatusBadGateway, "direct_host_unavailable", "宿主账号目录尚未连接，请重新启用插件。"}
		}
		identityCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		identity, err = host.ResolveOutboundIdentity(identityCtx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: start.AccountId})
		cancel()
		if err != nil {
			return nil, &directRequestError{http.StatusBadGateway, "direct_identity_unavailable", "无法读取当前账号的授权信息，请检查账号授权状态。"}
		}
	}
	client, err := s.direct.client(start.AccountId, identity.ProxyUrl)
	if err != nil {
		return nil, &directRequestError{http.StatusBadGateway, "direct_proxy_unavailable", err.Error()}
	}
	if err := rewriteDirectUserImages(prepared, func(image inlineImage) (string, error) {
		return s.uploadDirectImage(ctx, client, identity, image)
	}); err != nil {
		return nil, err
	}
	preparedBody := bpJSON(prepared)
	for attempt := 0; attempt < 2; attempt++ {
		// 新入口发送标准 JSON，不依赖旧线路采用的压缩编码；正文语义保持不变。
		request, requestErr := buildDirectHTTPRequest(ctx, start, preparedBody, identity)
		if requestErr != nil {
			return nil, requestErr
		}
		response, requestErr := client.Do(request)
		if requestErr != nil {
			return nil, requestErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return response, nil
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, &directRequestError{http.StatusBadGateway, "direct_response_read_failed", "BPS 响应读取中断，请检查账号代理与目标接口可用性。"}
		}
		if len(raw) > MaxBodyBytes {
			return nil, &directRequestError{http.StatusBadGateway, "direct_response_too_large", "BPS 响应超过 64 MiB。"}
		}
		transformed, changed, streamBody, transformErr := s.bpTransformResponse(raw, source)
		var emptyContinuation *bpEmptyContinuationError
		if errors.As(transformErr, &emptyContinuation) && attempt == 0 {
			// Kiro.rs 对工具结果后的空回合只重试一次，避免 Codex 被误判为完成。
			continue
		}
		if transformErr != nil {
			return nil, &directRequestError{http.StatusBadGateway, "direct_response_invalid", transformErr.Error()}
		}
		response.Body = io.NopCloser(bytes.NewReader(transformed))
		response.ContentLength = int64(len(transformed))
		response.Header.Del("Content-Encoding")
		if changed && streamBody {
			response.Header.Set("Content-Type", "text/event-stream")
			response.Header.Set("Cache-Control", "no-cache")
		} else if changed {
			response.Header.Set("Content-Type", "application/json")
		}
		return response, nil
	}
	return nil, &directRequestError{http.StatusBadGateway, "direct_response_invalid", "BPS 在工具结果后连续返回空回合，已停止重试。"}
}

func localDirectResponse(problem *directRequestError) *http.Response {
	errorType := "invalid_request_error"
	if problem.status >= 500 {
		errorType = "server_error"
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": errorType, "code": problem.code, "message": problem.message}})
	return &http.Response{StatusCode: problem.status, Status: fmt.Sprintf("%d %s", problem.status, http.StatusText(problem.status)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body))}
}

func (s *Server) forwardDirect(stream pluginv1.TransportPlugin_ForwardServer, start *pluginv1.ForwardRequestStart, body []byte) error {
	began := time.Now()
	response, err := s.directResponse(stream.Context(), start, body, nil)
	if err != nil {
		var problem *directRequestError
		if errors.As(err, &problem) {
			// 返回普通 HTTP 错误，让不支持的档位明确失败，不能悄悄回到原线路。
			response = localDirectResponse(problem)
		} else {
			if stream.Context().Err() != nil {
				return stream.Context().Err()
			}
			return forwardError(stream, "DIRECT_TRANSPORT_ERROR", "直连接口请求未完成，请检查账号代理与目标接口可用性；未自动重试。", true)
		}
	}
	defer response.Body.Close()
	headers := map[string]*pluginv1.HeaderValues{}
	for key, values := range response.Header {
		headers[key] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode: int32(response.StatusCode), Status: response.Status, Protocol: response.Proto, ProtocolMajor: int32(response.ProtoMajor), ProtocolMinor: int32(response.ProtoMinor), Headers: headers, ContentLength: response.ContentLength,
	}}}); err != nil {
		return err
	}
	buffer := make([]byte, 32*1024)
	var received int64
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			received += int64(n)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: append([]byte(nil), buffer[:n]...)}}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: received, DurationMs: time.Since(began).Milliseconds()}}})
		}
		if readErr != nil {
			return forwardError(stream, "DIRECT_RESPONSE_INTERRUPTED", "直连响应读取中断；未自动重试。", true)
		}
	}
}
