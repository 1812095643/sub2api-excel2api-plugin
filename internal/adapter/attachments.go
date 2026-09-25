package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

const (
	directAttachmentsURL  = "https://bps.openai.com/basispoints/api/attachments"
	maxInlineImageBytes   = 20 << 20
	maxAttachmentCacheLen = 256
)

type inlineImage struct {
	mediaType string
	data      []byte
}

func decodeInlineImage(value string) (inlineImage, error) {
	if !strings.HasPrefix(strings.ToLower(value), "data:") {
		return inlineImage{}, &directRequestError{http.StatusBadRequest, "invalid_image", "图片 URL 必须是 data URL 或 HTTPS URL。"}
	}
	metadata, encoded, found := strings.Cut(value[5:], ",")
	if !found {
		return inlineImage{}, &directRequestError{http.StatusBadRequest, "invalid_image", "图片 data URL 缺少数据分隔符。"}
	}
	isBase64 := strings.HasSuffix(strings.ToLower(metadata), ";base64")
	if isBase64 {
		metadata = metadata[:len(metadata)-len(";base64")]
	}
	mediaType, _, err := mime.ParseMediaType(metadata)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return inlineImage{}, &directRequestError{http.StatusBadRequest, "invalid_image", "图片 data URL 必须声明 image/* 类型。"}
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return inlineImage{}, &directRequestError{http.StatusBadRequest, "invalid_image", "图片 data URL 编码无效。"}
	}
	var data []byte
	if isBase64 {
		data, err = base64.StdEncoding.DecodeString(decoded)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(decoded)
		}
	} else {
		data = []byte(decoded)
	}
	if err != nil || len(data) == 0 {
		return inlineImage{}, &directRequestError{http.StatusBadRequest, "invalid_image", "图片 data URL 包含空数据或无效数据。"}
	}
	if len(data) > maxInlineImageBytes {
		return inlineImage{}, &directRequestError{http.StatusRequestEntityTooLarge, "image_too_large", "单张图片超过 20 MiB，无法上传到 Excel2API。"}
	}
	return inlineImage{mediaType: mediaType, data: data}, nil
}

func imageURLFromPart(part map[string]any) string {
	value := part["image_url"]
	if object := bpObject(value); object != nil {
		return bpString(object["url"])
	}
	return bpString(value)
}

func imageFromPart(part map[string]any) (inlineImage, bool, error) {
	if strings.EqualFold(bpString(part["type"]), "input_image") {
		if base64Value := bpString(part["image_base64"]); base64Value != "" {
			mediaType := bpString(part["media_type"])
			if mediaType == "" {
				mediaType = "image/png"
			}
			image, err := decodeInlineImage("data:" + mediaType + ";base64," + base64Value)
			return image, true, err
		}
		value := imageURLFromPart(part)
		if strings.HasPrefix(strings.ToLower(value), "data:") {
			if bpString(part["file_id"]) != "" {
				return inlineImage{}, true, &directRequestError{http.StatusBadRequest, "invalid_image", "图片输入不能同时包含 image_url 和 file_id。"}
			}
			image, err := decodeInlineImage(value)
			return image, true, err
		}
		if value == "" {
			if bpString(part["file_id"]) != "" {
				return inlineImage{}, false, nil
			}
			return inlineImage{}, true, &directRequestError{http.StatusBadRequest, "invalid_image", "图片输入缺少 image_url 或 file_id。"}
		}
		if !strings.HasPrefix(strings.ToLower(value), "https://") && bpString(part["file_id"]) == "" {
			return inlineImage{}, true, &directRequestError{http.StatusBadRequest, "invalid_image", "Excel2API 图片只接受 HTTPS URL 或上传后的 file_id。"}
		}
	}
	return inlineImage{}, false, nil
}

func rewriteDirectUserImages(body map[string]any, upload func(inlineImage) (string, error)) error {
	items, ok := body["input"].([]any)
	if !ok {
		return nil
	}
	for itemIndex, raw := range items {
		item := bpObject(raw)
		if item == nil || !strings.EqualFold(bpString(item["role"]), "user") {
			continue
		}
		kind := bpString(item["type"])
		if kind != "" && kind != "message" {
			continue
		}
		parts, ok := item["content"].([]any)
		if !ok {
			continue
		}
		updated := append([]any(nil), parts...)
		changed := false
		for partIndex, rawPart := range parts {
			part := bpObject(rawPart)
			if part == nil || !strings.EqualFold(bpString(part["type"]), "input_image") {
				continue
			}
			image, needsUpload, err := imageFromPart(part)
			if err != nil {
				return err
			}
			if !needsUpload {
				continue
			}
			fileID, err := upload(image)
			if err != nil {
				return err
			}
			if strings.TrimSpace(fileID) == "" {
				return &directRequestError{http.StatusBadGateway, "invalid_attachment_response", "图片上传未返回有效的 openai_file_id。"}
			}
			replacement := bpCloneObject(part)
			delete(replacement, "image_url")
			delete(replacement, "image_base64")
			delete(replacement, "media_type")
			replacement["file_id"] = fileID
			if _, exists := replacement["detail"]; !exists {
				replacement["detail"] = "auto"
			}
			updated[partIndex] = replacement
			changed = true
		}
		if changed {
			replacement := bpCloneObject(item)
			replacement["content"] = updated
			items[itemIndex] = replacement
		}
	}
	return nil
}

func attachmentFilename(mediaType string) string {
	extensions, _ := mime.ExtensionsByType(mediaType)
	if len(extensions) > 0 {
		return "image" + extensions[0]
	}
	return "image"
}

func attachmentErrorMessage(raw []byte, token, accountID string, image inlineImage) string {
	message := string(raw)
	for _, secret := range []string{token, accountID, base64.StdEncoding.EncodeToString(image.data), string(image.data)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func (s *Server) uploadDirectImage(ctx context.Context, client *http.Client, identity *pluginv1.ResolveOutboundIdentityResponse, image inlineImage) (string, error) {
	accountID, err := directAccountID(identity)
	if err != nil {
		return "", &directRequestError{http.StatusBadGateway, "direct_account_id_unavailable", err.Error()}
	}
	token := strings.TrimSpace(identity.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", &directRequestError{http.StatusBadGateway, "direct_token_unavailable", "当前账号没有可用的 access token。"}
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(accountID + "\x00" + image.mediaType + "\x00"))
	_, _ = hash.Write(image.data)
	var key [sha256.Size]byte
	copy(key[:], hash.Sum(nil))
	s.attachmentMu.Lock()
	if fileID := s.attachmentCache[key]; fileID != "" {
		s.attachmentMu.Unlock()
		return fileID, nil
	}
	s.attachmentMu.Unlock()
	fileID, err := s.uploadDirectImageToURL(ctx, client, directAttachmentsURL, identity, image)
	if err != nil {
		return "", err
	}
	s.attachmentMu.Lock()
	if s.attachmentCache == nil {
		s.attachmentCache = make(map[[sha256.Size]byte]string)
	}
	if len(s.attachmentCache) >= maxAttachmentCacheLen {
		for oldKey := range s.attachmentCache {
			delete(s.attachmentCache, oldKey)
			break
		}
	}
	s.attachmentCache[key] = strings.TrimSpace(fileID)
	s.attachmentMu.Unlock()
	return strings.TrimSpace(fileID), nil
}

func (s *Server) uploadDirectImageToURL(ctx context.Context, client *http.Client, endpoint string, identity *pluginv1.ResolveOutboundIdentityResponse, image inlineImage) (string, error) {
	accountID, err := directAccountID(identity)
	if err != nil {
		return "", &directRequestError{http.StatusBadGateway, "direct_account_id_unavailable", err.Error()}
	}
	token := strings.TrimSpace(identity.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", &directRequestError{http.StatusBadGateway, "direct_token_unavailable", "当前账号没有可用的 access token。"}
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeaders := make(textproto.MIMEHeader)
	partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": attachmentFilename(image.mediaType)}))
	partHeaders.Set("Content-Type", image.mediaType)
	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		return "", &directRequestError{http.StatusInternalServerError, "attachment_encoding", "无法构造图片上传请求。"}
	}
	if _, err := part.Write(image.data); err != nil {
		return "", &directRequestError{http.StatusInternalServerError, "attachment_encoding", "无法写入图片上传请求。"}
	}
	if err := writer.Close(); err != nil {
		return "", &directRequestError{http.StatusInternalServerError, "attachment_encoding", "无法完成图片上传请求。"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	setDirectBPSIdentityHeaders(request.Header, token, accountID)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		return "", &directRequestError{http.StatusBadGateway, "attachment_transport", "图片上传请求未完成，请检查账号代理与 BPS 可用性。"}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", &directRequestError{http.StatusBadGateway, "attachment_response_read_failed", "图片上传响应读取中断。"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &directRequestError{response.StatusCode, "attachment_upload_error", fmt.Sprintf("BPS 图片上传 HTTP %d: %s", response.StatusCode, attachmentErrorMessage(raw, token, accountID, image))}
	}
	var result struct {
		FileID string `json:"openai_file_id"`
	}
	if json.Unmarshal(raw, &result) != nil || strings.TrimSpace(result.FileID) == "" {
		return "", &directRequestError{http.StatusBadGateway, "invalid_attachment_response", "BPS 图片上传没有返回有效的 openai_file_id。"}
	}
	return strings.TrimSpace(result.FileID), nil
}
