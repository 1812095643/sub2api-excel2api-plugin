package adapter

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"
)

const MaxBodyBytes = 64 << 20

func eligible(start *pluginv1.ForwardRequestStart) bool {
	if start == nil || start.Platform != "openai" || start.AccountType != "oauth" || !strings.EqualFold(strings.TrimSpace(start.Method), http.MethodPost) || !start.HasBody {
		return false
	}
	u, err := url.Parse(start.Url)
	if err != nil {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/responses/compact")
}

func headerValue(headers map[string]*pluginv1.HeaderValues, name string) string {
	var values []string
	for key, value := range headers {
		if strings.EqualFold(key, name) && value != nil {
			values = append(values, value.Values...)
		}
	}
	return strings.Join(values, ",")
}

func decodeBody(body []byte, encoding string) ([]byte, error) {
	var reader io.ReadCloser
	switch encoding {
	case "", "identity":
		return body, nil
	case "gzip":
		value, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, errors.New("gzip 请求体无法解压")
		}
		reader = value
	case "deflate":
		value, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, errors.New("deflate 请求体无法解压")
		}
		reader = value
	case "zstd":
		value, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxBodyBytes))
		if err != nil {
			return nil, errors.New("zstd 请求体无法解压")
		}
		reader = value.IOReadCloser()
	default:
		return nil, errors.New("请求体编码不支持时区处理")
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, MaxBodyBytes+1))
	if err != nil || len(data) > MaxBodyBytes {
		return nil, errors.New("请求体解压失败或超过 64 MiB")
	}
	return data, nil
}

func encodeBody(body []byte, encoding string) ([]byte, error) {
	if encoding == "" || encoding == "identity" {
		return body, nil
	}
	var out bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&out)
	case "deflate":
		writer = zlib.NewWriter(&out)
	case "zstd":
		value, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)), zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		writer = value
	default:
		return nil, errors.New("请求体编码不支持重新编码")
	}
	if _, err := writer.Write(body); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func cloneHeaders(headers map[string]*pluginv1.HeaderValues) map[string]*pluginv1.HeaderValues {
	cloned := make(map[string]*pluginv1.HeaderValues, len(headers))
	for key, value := range headers {
		if value == nil {
			cloned[key] = nil
			continue
		}
		cloned[key] = &pluginv1.HeaderValues{Values: append([]string(nil), value.Values...)}
	}
	return cloned
}

func prepareLocaleRequest(ctx context.Context, start *pluginv1.ForwardRequestStart, body []byte, config *healthConfig) (*pluginv1.ForwardRequestStart, []byte, timezoneNormalization, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, timezoneNormalization{}, false, err
	}
	copyStart := proto.Clone(start).(*pluginv1.ForwardRequestStart)
	copyStart.Headers = cloneHeaders(start.Headers)
	languageChanged := normalizeAcceptLanguage(copyStart.Headers)
	if !eligible(start) {
		return copyStart, body, timezoneNormalization{Reason: "not_openai_oauth_responses"}, languageChanged, nil
	}
	encoding := strings.ToLower(strings.TrimSpace(headerValue(copyStart.Headers, "Content-Encoding")))
	plain, err := decodeBody(body, encoding)
	if err != nil {
		return nil, nil, timezoneNormalization{}, false, err
	}
	originalPlain := append([]byte(nil), plain...)
	var normalized timezoneNormalization
	plain, normalized = normalizeRequestLocale(plain, start.AccountId, config.timezoneForAccount(start.AccountId), timeNow())
	bodyChanged := !bytes.Equal(originalPlain, plain)
	encoded := body
	if bodyChanged {
		encoded, err = encodeBody(plain, encoding)
		if err != nil {
			return nil, nil, timezoneNormalization{}, false, err
		}
	}
	changed := bodyChanged || languageChanged
	if changed {
		copyStart.ContentLength = int64(len(encoded))
		for key := range copyStart.Headers {
			if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Content-MD5") || strings.EqualFold(key, "Digest") {
				delete(copyStart.Headers, key)
			}
		}
	}
	return copyStart, encoded, normalized, changed, nil
}

var timeNow = func() time.Time { return time.Now() }

func readBody(stream pluginv1.TransportPlugin_ForwardServer) ([]byte, error) {
	var out bytes.Buffer
	for {
		frame, err := stream.Recv()
		if err != nil {
			return nil, errors.New("请求体未完整传入")
		}
		switch frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			chunk := frame.GetBodyChunk()
			if len(chunk) > MaxBodyBytes-out.Len() {
				return nil, errors.New("请求体超过 64 MiB")
			}
			_, _ = out.Write(chunk)
		case *pluginv1.ForwardRequest_BodyEnd:
			if !frame.GetBodyEnd() {
				return nil, errors.New("请求体结束标志不正确")
			}
			return out.Bytes(), nil
		default:
			return nil, errors.New("请求帧顺序不正确")
		}
	}
}

func sendRequest(child pluginv1.TransportPlugin_ForwardClient, parent pluginv1.TransportPlugin_ForwardServer, first *pluginv1.ForwardRequest, buffered []byte, bufferedRequest bool) error {
	if err := child.Send(first); err != nil {
		return err
	}
	if bufferedRequest {
		for len(buffered) > 0 {
			n := len(buffered)
			if n > 32*1024 {
				n = 32 * 1024
			}
			if err := child.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: buffered[:n]}}); err != nil {
				return err
			}
			buffered = buffered[n:]
		}
		if err := child.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}); err != nil {
			return err
		}
		return child.CloseSend()
	}
	for {
		frame, err := parent.Recv()
		if err != nil {
			if err == io.EOF {
				return errors.New("请求缺少结束帧")
			}
			return err
		}
		if frame.GetStart() != nil {
			return errors.New("重复请求元数据")
		}
		if err := child.Send(frame); err != nil {
			return err
		}
		if frame.GetBodyEnd() {
			return child.CloseSend()
		}
	}
}

func forwardError(stream pluginv1.TransportPlugin_ForwardServer, code, message string, sent bool) error {
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{Code: code, Message: message, RequestSent: sent}}})
}
