package adapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBPSPrepareBodyUsesExcelShapeAndRemovesClientTools(t *testing.T) {
	server := New(nil)
	source := map[string]any{
		"model":        "gpt-6-astra",
		"stream":       true,
		"instructions": "Answer clearly.",
		"reasoning":    map[string]any{"effort": "max"},
		"input": []any{map[string]any{
			"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		}},
		"tools": []any{map[string]any{
			"type": "function", "name": "exec_command", "description": "Run a command",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}},
		}},
		"tool_choice": "auto",
	}
	prepared, err := server.bpPrepareResponsesBody(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := prepared["tools"]; exists {
		t.Fatal("BPS body must not contain tools")
	}
	if _, exists := prepared["tool_choice"]; exists {
		t.Fatal("BPS body must not contain tool_choice")
	}
	if prepared["model_selection"] != "explicit" || prepared["store"] != false || prepared["reasoning_effort"] != "xhigh" {
		t.Fatalf("unexpected BPS envelope: %#v", prepared)
	}
	input, ok := prepared["input"].([]any)
	if !ok || len(input) < 3 {
		t.Fatalf("developer catalog was not injected: %#v", prepared["input"])
	}
	catalog := false
	for _, raw := range input {
		item := bpObject(raw)
		if item != nil && strings.Contains(bpItemText(item["content"]), "exec_command") {
			catalog = true
		}
	}
	if !catalog {
		t.Fatal("client tool catalog missing from developer messages")
	}
	metadata := bpObject(prepared["metadata"])
	if bpString(metadata["turn_id"]) == "" || bpString(metadata["task_id"]) == "" || metadata["agent_iteration"] != "1" {
		t.Fatalf("unstable BPS metadata: %#v", metadata)
	}
}

func TestBPSNativeRunOfficeJSRoundTripKeepsOriginalItem(t *testing.T) {
	server := New(nil)
	source := map[string]any{
		"model":  "gpt-6-astra",
		"stream": true,
		"input":  []any{map[string]any{"role": "user", "content": "check"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "exec_command",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}},
		}},
	}
	inner := map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "pwd"}}
	native := map[string]any{
		"type": "function_call", "id": "fc_native", "call_id": "call_native", "name": bpTransportName,
		"status": "completed", "summary": "Inspect repository", "references": []any{"workspace"},
		"arguments": string(bpJSON(map[string]any{"summary": "Inspect repository", "code": string(bpJSON(inner)), "destructive": false, "references": []any{"workspace"}})),
	}
	response := map[string]any{"status": "completed", "output": []any{native}}
	transformed, changed, streamBody, err := server.bpTransformResponse(bpJSON(response), source)
	if err != nil || !changed || streamBody {
		t.Fatalf("transform failed: changed=%v stream=%v err=%v", changed, streamBody, err)
	}
	var clientResponse map[string]any
	if err := json.Unmarshal(transformed, &clientResponse); err != nil {
		t.Fatal(err)
	}
	output := clientResponse["output"].([]any)
	call := bpObject(output[0])
	if call["name"] != "exec_command" || call["call_id"] != "call_native" || call["arguments"] != `{"cmd":"pwd"}` {
		t.Fatalf("unexpected client tool call: %#v", call)
	}

	next := map[string]any{
		"model": "gpt-6-astra", "stream": true,
		"input": []any{
			map[string]any{"role": "user", "content": "check"},
			map[string]any{"type": "function_call", "id": "fc_native", "call_id": "call_native", "name": "exec_command", "arguments": `{"cmd":"pwd"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_native", "output": "C:/repo"},
		},
		"tools": source["tools"],
	}
	prepared, err := server.bpPrepareResponsesBody(next)
	if err != nil {
		t.Fatal(err)
	}
	preparedInput := prepared["input"].([]any)
	var restored, restoredOutput map[string]any
	for _, raw := range preparedInput {
		item := bpObject(raw)
		if item == nil {
			continue
		}
		if item["call_id"] == "call_native" && item["name"] == bpTransportName {
			restored = item
		}
		if item["call_id"] == "call_native" && item["type"] == "function_call_output" {
			restoredOutput = item
		}
	}
	if !reflect.DeepEqual(restored, native) {
		t.Fatalf("original native item was not restored: got=%#v want=%#v", restored, native)
	}
	if restoredOutput == nil || restoredOutput["id"] != "fc_call_native" || restoredOutput["output"] != "C:/repo" {
		t.Fatalf("tool result was not replayed as a complete Responses output: %#v", restoredOutput)
	}
	metadata := bpObject(prepared["metadata"])
	if metadata["agent_iteration"] != "2" {
		t.Fatalf("agent_iteration=%v, want 2", metadata["agent_iteration"])
	}
	firstPrepared, _ := server.bpPrepareResponsesBody(source)
	if bpObject(firstPrepared["metadata"])["turn_id"] != metadata["turn_id"] {
		t.Fatal("turn_id changed while replaying a tool result")
	}
}

func TestBPSPrepareBodyKeepsInlineImageUntilAttachmentUpload(t *testing.T) {
	server := New(nil)
	prepared, err := server.bpPrepareResponsesBody(map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bpJSON(prepared["input"])), "data:image/png") {
		t.Fatal("inline image should remain until the attachment upload step")
	}
}

func TestCodexCatalogProvidesBuiltInFunctionAndCustomTools(t *testing.T) {
	server := New(nil)
	prepared, err := server.bpPrepareResponsesBody(map[string]any{
		"model":  "gpt-6-astra",
		"stream": true,
		"input":  []any{map[string]any{"role": "user", "content": "use a tool"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := prepared["input"].([]any)
	joined := ""
	for _, raw := range input {
		joined += bpItemText(bpObject(raw)["content"])
	}
	for _, name := range []string{"exec_command", "apply_patch", "view_image", "request_user_input", "collaboration.spawn_agent"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("Codex catalog omitted %q", name)
		}
	}
	if spec, ok := bpToolLookup(bpClientToolSpecs(map[string]any{}), "apply_patch", ""); !ok || spec.Type != "custom" {
		t.Fatalf("apply_patch must retain Codex custom tool type: %#v %v", spec, ok)
	}
}

func TestCodexCatalogRestoresNamespacedToolThroughTunnel(t *testing.T) {
	server := New(nil)
	source := map[string]any{
		"model": "gpt-6-astra", "stream": true,
		"input": []any{map[string]any{"role": "user", "content": "delegate"}},
	}
	inner := map[string]any{"tool": "spawn_agent", "namespace": "collaboration", "args": map[string]any{"task_name": "scan", "message": "inspect"}}
	native := map[string]any{
		"type": "function_call", "id": "fc_ns", "call_id": "call_ns", "name": bpTransportName,
		"arguments": string(bpJSON(map[string]any{"code": string(bpJSON(inner)), "summary": "Delegate"})),
	}
	transformed, changed, _, err := server.bpTransformResponse(bpJSON(map[string]any{"status": "completed", "output": []any{native}}), source)
	if err != nil || !changed {
		t.Fatalf("namespaced tunnel decode failed: changed=%v err=%v", changed, err)
	}
	var response map[string]any
	if err := json.Unmarshal(transformed, &response); err != nil {
		t.Fatal(err)
	}
	call := bpObject(response["output"].([]any)[0])
	if call["type"] != "function_call" || call["name"] != "spawn_agent" || call["namespace"] != "collaboration" {
		t.Fatalf("namespaced Codex item was not restored: %#v", call)
	}
}

func TestBPSMalformedTunnelCallIsRejected(t *testing.T) {
	server := New(nil)
	native := map[string]any{
		"type": "function_call", "id": "fc_bad", "call_id": "call_bad", "name": bpTransportName,
		"arguments": `{"code":"{not-json"}`,
	}
	_, _, _, err := server.bpTransformResponse(bpJSON(map[string]any{"status": "completed", "output": []any{native}}), map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": "x"}}})
	if err == nil || !strings.Contains(err.Error(), "run_officejs") {
		t.Fatalf("malformed tunnel call was not rejected: %v", err)
	}
}

func TestDirectUserImageIsReplacedWithFileIDAndToolScreenshotIsPreserved(t *testing.T) {
	dataURL := "data:image/png;base64,aGVsbG8="
	body := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_image", "image_url": dataURL, "detail": "high"}},
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call-shot",
				"output": []any{map[string]any{"type": "input_image", "image_url": dataURL}},
			},
		},
	}
	var uploaded inlineImage
	err := rewriteDirectUserImages(body, func(image inlineImage) (string, error) {
		uploaded = image
		return "file_uploaded", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.mediaType != "image/png" || string(uploaded.data) != "hello" {
		t.Fatalf("decoded image mismatch: %#v", uploaded)
	}
	userContent := bpObject(bpObject(body["input"].([]any)[0])["content"].([]any)[0])
	if userContent["file_id"] != "file_uploaded" || userContent["image_url"] != nil || userContent["detail"] != "high" {
		t.Fatalf("user image was not rewritten: %#v", userContent)
	}
	toolOutput := bpObject(body["input"].([]any)[1])
	if !strings.Contains(string(bpJSON(toolOutput["output"])), dataURL) {
		t.Fatal("tool result screenshot should remain available for BPS replay")
	}
}

func TestDirectUserImageRejectsInvalidDataURL(t *testing.T) {
	err := rewriteDirectUserImages(map[string]any{
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,%%%"}},
		}},
	}, func(inlineImage) (string, error) { return "file", nil })
	if err == nil || !strings.Contains(err.Error(), "data URL") {
		t.Fatalf("invalid data URL was accepted: %v", err)
	}
}

func TestBPSStreamAcceptsResponseDoneEnvelope(t *testing.T) {
	raw := []byte("event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_done\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err != nil || response["id"] != "resp_done" || response["status"] != "completed" {
		t.Fatalf("response.done was not accepted: %#v err=%v", response, err)
	}
}

func TestBPSStreamAcceptsDirectCompletedResponse(t *testing.T) {
	raw := []byte("data: {\"id\":\"resp_direct\",\"status\":\"completed\",\"output\":[]}\n\n")
	response, err := bpParseSSEFinal(raw)
	if err != nil || response["id"] != "resp_direct" || response["status"] != "completed" {
		t.Fatalf("direct completed response was not accepted: %#v err=%v", response, err)
	}
}

func TestBPSStreamSynthesizesCompletionFromFinishedOutputItems(t *testing.T) {
	raw := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_items\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if response["status"] != "completed" || len(response["output"].([]any)) != 1 {
		t.Fatalf("finished output item was not synthesized: %#v", response)
	}
}

func TestBPSStreamSynthesizesCompletionFromOutputTextDeltas(t *testing.T) {
	raw := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_text\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hello\"}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bpJSON(response["output"])), "hello") {
		t.Fatalf("output text delta was not synthesized: %#v", response)
	}
}

func TestBPSStreamSynthesizesCompletionFromOutputTextDone(t *testing.T) {
	raw := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_text_done\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"text\":\"final text\"}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bpJSON(response["output"])), "final text") {
		t.Fatalf("output text done was not synthesized: %#v", response)
	}
}

func TestBPSStreamRejectsFailureEnvelope(t *testing.T) {
	raw := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"status\":\"failed\",\"error\":{\"message\":\"upstream unavailable\"}}}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err == nil || response != nil || !strings.Contains(err.Error(), "response.failed") {
		t.Fatalf("failure envelope was accepted: response=%#v err=%v", response, err)
	}
}

func TestBPSStreamRejectsIncompleteEnvelope(t *testing.T) {
	raw := []byte("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_incomplete\",\"status\":\"incomplete\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err == nil || response != nil || !strings.Contains(err.Error(), "response.incomplete") {
		t.Fatalf("incomplete envelope was accepted: response=%#v err=%v", response, err)
	}
}

func TestBPSStreamRejectsIncompleteFinishedOutputItem(t *testing.T) {
	raw := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_item_incomplete\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"status\":\"incomplete\",\"role\":\"assistant\",\"content\":[]}}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err == nil || response != nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete output item was accepted: response=%#v err=%v", response, err)
	}
}

func TestBPSStreamDoesNotTreatDoneMarkerAloneAsCompletion(t *testing.T) {
	raw := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\",\"status\":\"in_progress\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	response, err := bpParseSSEFinal(raw)
	if err == nil || response != nil || !strings.Contains(err.Error(), "没有可完成的响应对象") {
		t.Fatalf("empty stream was accepted: response=%#v err=%v", response, err)
	}
}
