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

func TestBPSRejectsDataImageBeforeUpstream(t *testing.T) {
	server := New(nil)
	_, err := server.bpPrepareResponsesBody(map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}}},
	})
	if err == nil || !strings.Contains(err.Error(), "HTTPS 图片 URL") {
		t.Fatalf("unexpected data image result: %v", err)
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
