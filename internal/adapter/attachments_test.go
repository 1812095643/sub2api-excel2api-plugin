package adapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

func TestDirectImageUploadMultipartContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer test-token" || request.Header.Get("chatgpt-account-id") != "acct-test" {
			t.Fatalf("unexpected attachment headers: %#v", request.Header)
		}
		reader, err := request.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() != "file" || part.FileName() != "image.png" || part.Header.Get("Content-Type") != "image/png" || string(data) != "image-bytes" {
			t.Fatalf("unexpected multipart file: name=%s filename=%s type=%s data=%q", part.FormName(), part.FileName(), part.Header.Get("Content-Type"), data)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"openai_file_id":"file-uploaded"}`))
	}))
	defer server.Close()

	identity := &pluginv1.ResolveOutboundIdentityResponse{
		Found: true, AccountId: 1, Platform: "openai", AccountType: "oauth", Token: "test-token",
		Headers: map[string]*pluginv1.HeaderValues{"chatgpt-account-id": {Values: []string{"acct-test"}}},
	}
	fileID, err := (&Server{}).uploadDirectImageToURL(context.Background(), server.Client(), server.URL, identity, inlineImage{mediaType: "image/png", data: []byte("image-bytes")})
	if err != nil || fileID != "file-uploaded" {
		t.Fatalf("upload result=%q err=%v", fileID, err)
	}
}

func TestInlineImageDecoderRejectsNonImageAndOversizedData(t *testing.T) {
	if _, err := decodeInlineImage("data:text/plain;base64,aGVsbG8="); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("non-image data URL was accepted: %v", err)
	}
	if _, err := decodeInlineImage("data:image/png;base64," + strings.Repeat("A", maxInlineImageBytes*2)); err == nil || !strings.Contains(err.Error(), "20 MiB") {
		t.Fatalf("oversized data URL was accepted: %v", err)
	}
}
