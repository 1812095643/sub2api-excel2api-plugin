package adapter

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

const (
	bpTransportName  = "run_officejs"
	bpTransportAlias = "functions.run_officejs"
)

type bpEmptyContinuationError struct{}

func (*bpEmptyContinuationError) Error() string {
	return "BPS 在工具结果后返回了没有文本或工具调用的空回合"
}

type bpToolSpec struct {
	Key       string
	Name      string
	Namespace string
	Type      string
	Spec      map[string]any
}

// Codex CLI 的工具目录来自开源仓库的固定提交。插件复用声明和 schema，
// 工具执行仍由客户端完成；插件只负责把调用封装进 BPS 隧道。
//
//go:embed codex_tool_catalog.json
var codexToolCatalogRaw []byte

var codexToolCatalogOnce sync.Once
var codexToolCatalogSpecs map[string]bpToolSpec

func bpCodexToolCatalog() map[string]bpToolSpec {
	codexToolCatalogOnce.Do(func() {
		codexToolCatalogSpecs = map[string]bpToolSpec{}
		var document struct {
			Tools []any `json:"tools"`
		}
		if err := json.Unmarshal(codexToolCatalogRaw, &document); err != nil {
			return
		}
		for _, raw := range document.Tools {
			bpIterTools([]any{raw}, "", func(spec bpToolSpec) {
				codexToolCatalogSpecs[spec.Key] = spec
			})
		}
	})
	return codexToolCatalogSpecs
}

func bpJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func bpString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func bpObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func bpCloneObject(object map[string]any) map[string]any {
	if object == nil {
		return nil
	}
	copy := make(map[string]any, len(object))
	for key, value := range object {
		copy[key] = value
	}
	return copy
}

func bpFirstMap(object map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value := bpObject(object[key]); value != nil {
			return value
		}
	}
	return nil
}

func bpIterTools(tools any, namespace string, callback func(bpToolSpec)) {
	list, ok := tools.([]any)
	if !ok {
		return
	}
	for _, raw := range list {
		tool := bpObject(raw)
		if tool == nil {
			continue
		}
		kind := strings.ToLower(bpString(tool["type"]))
		name := bpString(tool["name"])
		if name == "" {
			if nested := bpObject(tool["function"]); nested != nil {
				name = bpString(nested["name"])
				if kind == "" {
					kind = "function"
				}
			}
		}
		if (kind == "function" || kind == "custom") && name != "" {
			key := name
			if namespace != "" {
				key = namespace + "." + name
			}
			callback(bpToolSpec{Key: key, Name: name, Namespace: namespace, Type: kind, Spec: tool})
		}
		if kind == "namespace" && name != "" {
			bpIterTools(tool["tools"], name, callback)
		}
	}
}

func bpClientToolSpecs(source map[string]any) map[string]bpToolSpec {
	result := map[string]bpToolSpec{}
	if strings.EqualFold(bpString(source["tool_choice"]), "none") {
		return result
	}
	for key, spec := range bpCodexToolCatalog() {
		result[key] = spec
	}
	bpIterTools(source["tools"], "", func(spec bpToolSpec) { result[spec.Key] = spec })
	if input, ok := source["input"].([]any); ok {
		for _, raw := range input {
			item := bpObject(raw)
			if item == nil || bpString(item["type"]) != "additional_tools" {
				continue
			}
			bpIterTools(item["tools"], "", func(spec bpToolSpec) { result[spec.Key] = spec })
		}
	}
	return result
}

func bpToolLookup(specs map[string]bpToolSpec, name, namespace string) (bpToolSpec, bool) {
	name = strings.TrimSpace(name)
	namespace = strings.TrimSpace(namespace)
	keys := []string{}
	if namespace != "" {
		keys = append(keys, namespace+"."+name, namespace+"__"+name)
	}
	keys = append(keys, name)
	if strings.Contains(name, "__") {
		keys = append(keys, strings.Replace(name, "__", ".", 1))
	}
	for _, key := range keys {
		if spec, ok := specs[key]; ok {
			return spec, true
		}
	}
	return bpToolSpec{}, false
}

func bpMessageItem(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": []any{map[string]any{"type": contentType, "text": text}},
	}
}

func bpDescribeParameters(parameters map[string]any) string {
	properties := bpObject(parameters["properties"])
	if len(properties) == 0 {
		return "the arguments required by the client"
	}
	required := map[string]bool{}
	if values, ok := parameters["required"].([]any); ok {
		for _, value := range values {
			required[bpString(value)] = true
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		kind := "optional"
		if required[name] {
			kind = "required"
		}
		names = append(names, name+" ("+kind+")")
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func bpToolName(spec bpToolSpec) string {
	if spec.Namespace != "" {
		return spec.Namespace + "." + spec.Name
	}
	return spec.Name
}

func bpClientToolProtocolInstructions(source map[string]any) string {
	specs := bpClientToolSpecs(source)
	if len(specs) == 0 {
		return "This request is relayed by an external Responses API client, not by the live Excel workbook. The native run_officejs function is a proxy transport endpoint and is never executed. No client tools are available for this request; return the answer as assistant text."
	}
	keys := make([]string, 0, len(specs))
	for key := range specs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		spec := specs[key]
		line := "- " + key + " (" + spec.Type + ")"
		if description := bpString(spec.Spec["description"]); description != "" {
			line += ": " + description
		}
		if spec.Type == "function" {
			if parameters := bpFirstMap(spec.Spec, "parameters", "inputSchema", "input_schema"); parameters != nil {
				line += ". Arguments: " + bpDescribeParameters(parameters) + "."
			}
		} else {
			line += ". It receives raw text in input."
		}
		lines = append(lines, line)
	}
	return "This request is relayed by an external Responses API client, not by the live Excel workbook. The native run_officejs function is a proxy transport endpoint owned by this plugin. The plugin intercepts it before execution, so it never runs Office code or changes a workbook. Server-injected Excel, Office, connector, workbook, list_skills, and web-search tools are unavailable. To call a client tool, call the outer native run_officejs once and put exactly one compact JSON object as JSON text in its code field. The code field is not JavaScript. Use this inner shape for a function tool: {\"tool\":\"tool_name\",\"args\":{\"argument\":\"value\"}}. For a custom tool, use {\"tool\":\"tool_name\",\"args\":\"raw input\"}. Do not put a second run_officejs wrapper in code. The available client tools are:\n" + strings.Join(lines, "\n") + "\nThe proxy converts the native call into the selected client tool call, then replays the original run_officejs item together with the client result on the next request."
}

func bpClientToolProtocolReminder(source map[string]any) string {
	specs := bpClientToolSpecs(source)
	if len(specs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(specs))
	for key := range specs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return "Reminder: use the outer native run_officejs transport exactly once. Its code value must be JSON text for one catalog tool object, for example {\"tool\":\"" + keys[0] + "\",\"args\":{}}. Never execute OfficeJS and never put run_officejs or functions.run_officejs inside code. Available client tools: " + strings.Join(keys, ", ") + "."
}

func bpStripClientMetadata(item map[string]any) map[string]any {
	if _, exists := item["internal_chat_message_metadata_passthrough"]; !exists {
		return item
	}
	copy := bpCloneObject(item)
	delete(copy, "internal_chat_message_metadata_passthrough")
	return copy
}

func bpFunctionItemID(callID string) string {
	if callID == "" {
		return ""
	}
	if strings.HasPrefix(callID, "fc_") {
		return callID
	}
	return "fc_" + callID
}

func (s *Server) bpRememberNativeCall(item map[string]any) {
	callID := bpString(item["call_id"])
	if callID == "" {
		return
	}
	copy := bpCloneObject(item)
	s.basispointsMu.Lock()
	defer s.basispointsMu.Unlock()
	if s.basispointsCalls == nil {
		s.basispointsCalls = map[string]map[string]any{}
	}
	if _, exists := s.basispointsCalls[callID]; !exists {
		s.basispointsCallIDs = append(s.basispointsCallIDs, callID)
	}
	s.basispointsCalls[callID] = copy
	for len(s.basispointsCallIDs) > 512 {
		oldest := s.basispointsCallIDs[0]
		s.basispointsCallIDs = s.basispointsCallIDs[1:]
		delete(s.basispointsCalls, oldest)
	}
}

func (s *Server) bpRememberedNativeCall(callID string) map[string]any {
	s.basispointsMu.Lock()
	defer s.basispointsMu.Unlock()
	return bpCloneObject(s.basispointsCalls[callID])
}

func bpItemText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if values, ok := value.([]any); ok {
		var builder strings.Builder
		for _, value := range values {
			if text := bpString(value); text != "" {
				builder.WriteString(text)
				continue
			}
			if object := bpObject(value); object != nil {
				builder.WriteString(bpString(object["text"]))
			}
		}
		return builder.String()
	}
	return ""
}

func bpFallbackTransportCall(item map[string]any) map[string]any {
	name := bpString(item["name"])
	callID := bpString(item["call_id"])
	if callID == "" {
		callID = "call_bp_" + bpShortHash(string(bpJSON(item)))[:24]
	}
	inner := map[string]any{"tool": name}
	if namespace := bpString(item["namespace"]); namespace != "" {
		inner["namespace"] = namespace
	}
	if item["type"] == "custom_tool_call" {
		inner["args"] = bpString(item["input"])
	} else {
		arguments := map[string]any{}
		if raw := bpString(item["arguments"]); raw != "" {
			_ = json.Unmarshal([]byte(raw), &arguments)
		}
		inner["args"] = arguments
	}
	outer := map[string]any{
		"summary":          "Run client tool " + name,
		"extended_summary": "Relay " + name + " through the external client",
		"code":             string(bpJSON(inner)),
		"destructive":      false,
		"references":       []any{},
	}
	return map[string]any{
		"type":      "function_call",
		"id":        bpFunctionItemID(callID),
		"call_id":   callID,
		"name":      bpTransportName,
		"arguments": string(bpJSON(outer)),
		"status":    "completed",
	}
}

func (s *Server) bpTranslateInputItems(rawInput any, allowed map[string]bpToolSpec) []any {
	if text, ok := rawInput.(string); ok {
		return []any{bpMessageItem("user", text)}
	}
	items, ok := rawInput.([]any)
	if !ok {
		return []any{}
	}
	result := make([]any, 0, len(items))
	for _, raw := range items {
		item := bpObject(raw)
		if item == nil {
			continue
		}
		item = bpStripClientMetadata(item)
		kind := strings.ToLower(bpString(item["type"]))
		if kind == "function_call" || kind == "custom_tool_call" {
			callID := bpString(item["call_id"])
			if native := s.bpRememberedNativeCall(callID); native != nil {
				result = append(result, native)
				continue
			}
			name := bpString(item["name"])
			namespace := bpString(item["namespace"])
			if name == bpTransportName || name == bpTransportAlias {
				s.bpRememberNativeCall(item)
				result = append(result, item)
				continue
			}
			if _, exists := bpToolLookup(allowed, name, namespace); exists {
				result = append(result, bpFallbackTransportCall(item))
				continue
			}
			result = append(result, item)
			continue
		}
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			callID := bpString(item["call_id"])
			if s.bpRememberedNativeCall(callID) != nil {
				copy := bpCloneObject(item)
				copy["type"] = "function_call_output"
				copy["id"] = bpFunctionItemID(callID)
				if strings.TrimSpace(bpItemText(copy["output"])) == "" {
					copy["output"] = "(tool call succeeded with no output)"
				}
				result = append(result, copy)
				continue
			}
			result = append(result, item)
			continue
		}
		if kind == "reasoning" {
			if encrypted := bpString(item["encrypted_content"]); encrypted != "" {
				result = append(result, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted})
			}
			continue
		}
		if kind == "item_reference" {
			continue
		}
		result = append(result, item)
	}
	return result
}

func bpExplicitConversationKey(source map[string]any) string {
	for _, key := range []string{"prompt_cache_key", "promptCacheKey", "session_id", "sessionId"} {
		if value := bpString(source[key]); value != "" {
			return value
		}
	}
	if metadata := bpObject(source["client_metadata"]); metadata != nil {
		for _, key := range []string{"session_id", "sessionId"} {
			if value := bpString(metadata[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func bpShortHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func bpConversationFingerprint(items []any) string {
	for _, value := range items {
		if object := bpObject(value); object != nil {
			return bpShortHash(string(bpJSON(object)))
		}
	}
	return "anonymous"
}

func bpTurnState(rawInput any) (string, string) {
	items, ok := rawInput.([]any)
	if !ok {
		return bpShortHash(string(bpJSON(rawInput))), "1"
	}
	lastUser := -1
	for index, value := range items {
		if object := bpObject(value); object != nil && strings.EqualFold(bpString(object["role"]), "user") {
			lastUser = index
		}
	}
	if lastUser < 0 {
		lastUser = 0
	}
	prefix := items[:lastUser+1]
	iteration := 1
	for _, value := range items[lastUser+1:] {
		if object := bpObject(value); object != nil {
			kind := bpString(object["type"])
			if kind == "function_call_output" || kind == "custom_tool_call_output" {
				iteration++
			}
		}
	}
	return bpShortHash(string(bpJSON(prefix))), fmt.Sprintf("%d", iteration)
}

var bpUUIDNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

func bpUUIDV5(name string) string {
	hash := sha1.New()
	_, _ = hash.Write(bpUUIDNamespace[:])
	_, _ = hash.Write([]byte(name))
	digest := hash.Sum(nil)
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func bpNormalizeEffort(value any) string {
	level := strings.ToLower(strings.TrimSpace(bpString(value)))
	switch level {
	case "x-high", "extra-high", "extra_high", "max":
		level = "xhigh"
	}
	switch level {
	case "low", "medium", "high", "xhigh", "ultra":
		return level
	default:
		return "medium"
	}
}

func bpReasoningEffort(source map[string]any) string {
	if reasoning := bpObject(source["reasoning"]); reasoning != nil {
		return bpNormalizeEffort(reasoning["effort"])
	}
	return bpNormalizeEffort(source["reasoning_effort"])
}

func bpContextManagement(source map[string]any) []any {
	if value, ok := source["context_management"].([]any); ok {
		return value
	}
	return []any{map[string]any{"type": "compaction", "compact_threshold": 200000}}
}

func bpRequestHasToolResult(source map[string]any) bool {
	items, ok := source["input"].([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item := bpObject(raw)
		if item == nil {
			continue
		}
		kind := bpString(item["type"])
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			return true
		}
	}
	return false
}

func bpResponseHasUsefulOutput(response map[string]any) bool {
	items, ok := response["output"].([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item := bpObject(raw)
		if item == nil {
			continue
		}
		switch bpString(item["type"]) {
		case "function_call", "custom_tool_call", "web_search_call":
			return true
		case "message":
			if strings.TrimSpace(bpItemText(item["content"])) != "" || strings.TrimSpace(bpString(item["output_text"])) != "" {
				return true
			}
		}
	}
	return false
}

func (s *Server) bpPrepareResponsesBody(source map[string]any) (map[string]any, error) {
	model := strings.TrimSpace(bpString(source["model"]))
	if model == "" {
		return nil, fmt.Errorf("直连请求缺少 model")
	}
	allowed := bpClientToolSpecs(source)
	inputItems := s.bpTranslateInputItems(source["input"], allowed)
	historyRoot := bpConversationFingerprint(inputItems)
	prologue := make([]any, 0, 3)
	if instructions := bpString(source["instructions"]); instructions != "" {
		prologue = append(prologue, bpMessageItem("developer", instructions))
	}
	prologue = append(prologue, bpMessageItem("developer", bpClientToolProtocolInstructions(source)))
	if reminder := bpClientToolProtocolReminder(source); reminder != "" {
		prologue = append(prologue, bpMessageItem("developer", reminder))
	}
	input := append(prologue, inputItems...)

	output := map[string]any{
		"model":              model,
		"model_selection":    "explicit",
		"stream":             source["stream"] == true,
		"store":              false,
		"input":              input,
		"reasoning_effort":   bpReasoningEffort(source),
		"context_management": bpContextManagement(source),
	}
	if cacheKey := bpExplicitConversationKey(source); cacheKey != "" {
		output["prompt_cache_key"] = cacheKey
	}
	metadata := map[string]any{}
	if rawMetadata := bpObject(source["metadata"]); rawMetadata != nil {
		for key, value := range rawMetadata {
			if key == "turn_id" || key == "task_id" || key == "agent_iteration" {
				continue
			}
			switch value.(type) {
			case string, bool, json.Number, float64, int, int64:
				metadata[key[:min(len(key), 64)]] = fmt.Sprint(value)[:min(len(fmt.Sprint(value)), 512)]
			}
		}
	}
	turnFingerprint, iteration := bpTurnState(source["input"])
	conversation := bpExplicitConversationKey(source)
	if conversation == "" {
		conversation = historyRoot
	}
	metadata["task_id"] = bpUUIDV5("sub2api-basispoints/" + conversation)
	metadata["turn_id"] = bpUUIDV5("sub2api-basispoints/" + conversation + "/turn/" + turnFingerprint)
	metadata["agent_iteration"] = iteration
	output["metadata"] = metadata
	return output, nil
}

func bpDecodeTransportCode(value any) map[string]any {
	if object := bpObject(value); object != nil {
		return object
	}
	text := bpString(value)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	}
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) == nil && object != nil {
		return object
	}
	return nil
}

func bpIsTransportName(name string) bool {
	return name == bpTransportName || name == bpTransportAlias
}

func bpParseArguments(value any) map[string]any {
	if object := bpObject(value); object != nil {
		return object
	}
	text := bpString(value)
	if text == "" {
		return nil
	}
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) != nil {
		return nil
	}
	return object
}

func bpTransportEnvelope(native map[string]any) map[string]any {
	if bpString(native["type"]) != "function_call" || !bpIsTransportName(bpString(native["name"])) {
		return nil
	}
	arguments := bpParseArguments(native["arguments"])
	if arguments == nil {
		return nil
	}
	envelope := bpDecodeTransportCode(arguments["code"])
	for depth := 0; depth < 2 && envelope != nil && bpIsTransportName(bpString(envelope["name"])); depth++ {
		nested := bpParseArguments(envelope["arguments"])
		if nested == nil {
			return nil
		}
		envelope = bpDecodeTransportCode(nested["code"])
	}
	if envelope != nil && bpIsTransportName(bpString(envelope["name"])) {
		return nil
	}
	return envelope
}

func bpSchemaMatches(value any, schema map[string]any) bool {
	if len(schema) == 0 {
		return true
	}
	if alternatives, ok := schema["type"].([]any); ok {
		for _, alternative := range alternatives {
			copy := bpCloneObject(schema)
			copy["type"] = alternative
			if bpSchemaMatches(value, copy) {
				return true
			}
		}
		return false
	}
	switch bpString(schema["type"]) {
	case "object":
		object := bpObject(value)
		if object == nil {
			return false
		}
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if _, exists := object[bpString(name)]; !exists {
					return false
				}
			}
		}
		properties := bpObject(schema["properties"])
		for key, nested := range object {
			if properties == nil {
				continue
			}
			nestedSchema := bpObject(properties[key])
			if nestedSchema == nil {
				if schema["additionalProperties"] == false {
					return false
				}
				continue
			}
			if !bpSchemaMatches(nested, nestedSchema) {
				return false
			}
		}
	case "array":
		values, ok := value.([]any)
		if !ok {
			return false
		}
		if nested := bpObject(schema["items"]); nested != nil {
			for _, item := range values {
				if !bpSchemaMatches(item, nested) {
					return false
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return false
		}
	case "integer", "number":
		switch value.(type) {
		case json.Number, float64, int, int64:
		default:
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		if value != nil {
			return false
		}
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		matched := false
		for _, option := range values {
			if fmt.Sprint(option) == fmt.Sprint(value) {
				matched = true
				break
			}
		}
		return matched
	}
	return true
}

func (s *Server) bpExtractNativeClientToolCall(response map[string]any, source map[string]any) (map[string]any, bool, error) {
	output, ok := response["output"].([]any)
	if !ok {
		return nil, false, nil
	}
	var native map[string]any
	count := 0
	for _, raw := range output {
		item := bpObject(raw)
		if item == nil {
			continue
		}
		kind := bpString(item["type"])
		if (kind == "function_call" || kind == "custom_tool_call") && bpIsTransportName(bpString(item["name"])) {
			native = item
			count++
		}
	}
	if native == nil || count != 1 {
		return nil, false, nil
	}
	specs := bpClientToolSpecs(source)
	inner := bpTransportEnvelope(native)
	toolName := ""
	if inner != nil {
		toolName = bpString(inner["tool"])
		if toolName == "" {
			toolName = bpString(inner["name"])
		}
	}
	if toolName == "" || bpIsTransportName(toolName) {
		return nil, false, fmt.Errorf("BPS 返回了无法识别的 run_officejs 工具目录调用")
	}
	namespace := ""
	if inner != nil {
		namespace = bpString(inner["namespace"])
	}
	spec, exists := bpToolLookup(specs, toolName, namespace)
	if !exists {
		return nil, false, fmt.Errorf("BPS 请求了未声明的客户端工具 %q", toolName)
	}
	callID := bpString(native["call_id"])
	if callID == "" {
		callID = "call_bp_" + bpShortHash(string(bpJSON(native)))[:24]
	}
	result := map[string]any{
		"type":    "function_call",
		"id":      bpString(native["id"]),
		"call_id": callID,
		"name":    bpToolName(spec),
	}
	if spec.Namespace != "" {
		result["name"] = spec.Name
		result["namespace"] = spec.Namespace
	}
	if bpString(result["id"]) == "" {
		result["id"] = bpFunctionItemID(callID)
	}
	if spec.Type == "custom" || bpString(native["type"]) == "custom_tool_call" {
		var input any
		if inner != nil {
			input = inner["args"]
			if input == nil {
				input = inner["input"]
			}
		}
		if input == nil {
			input = native["input"]
		}
		if input == nil {
			return nil, false, fmt.Errorf("BPS custom 工具 %q 缺少 input/args", toolName)
		}
		if _, ok := input.(string); !ok {
			input = string(bpJSON(input))
		}
		result["type"] = "custom_tool_call"
		result["input"] = input
	} else {
		var arguments any
		if inner != nil {
			arguments = inner["args"]
			if arguments == nil {
				arguments = inner["arguments"]
			}
		}
		if arguments == nil {
			arguments = native["arguments"]
		}
		parsed := bpParseArguments(arguments)
		if parsed == nil || !bpSchemaMatches(parsed, bpFirstMap(spec.Spec, "parameters", "inputSchema", "input_schema")) {
			return nil, false, fmt.Errorf("BPS 工具 %q 返回的 arguments 不是有效的 JSON 或不符合 Codex schema", toolName)
		}
		result["arguments"] = string(bpJSON(parsed))
	}
	s.bpRememberNativeCall(native)
	return result, true, nil
}

func bpReplaceNativeCall(response map[string]any, toolCall map[string]any) {
	output, _ := response["output"].([]any)
	callID := bpString(toolCall["call_id"])
	replaced := make([]any, 0, len(output))
	done := false
	for _, raw := range output {
		item := bpObject(raw)
		if !done && item != nil && bpString(item["call_id"]) == callID {
			copy := bpCloneObject(toolCall)
			copy["status"] = "completed"
			replaced = append(replaced, copy)
			done = true
			continue
		}
		replaced = append(replaced, raw)
	}
	if !done {
		replaced = append([]any{toolCall}, replaced...)
	}
	response["output"] = replaced
	response["status"] = "completed"
}

func bpParseSSEFinal(raw []byte) (map[string]any, error) {
	var completed map[string]any
	var started map[string]any
	finishedItems := map[int]map[string]any{}
	finishedOrder := make([]int, 0)
	textByIndex := map[int]string{}
	finalTextByIndex := map[int]string{}
	terminalFailure := ""
	reader := bufio.NewScanner(strings.NewReader(string(raw)))
	reader.Buffer(make([]byte, 1024), 8<<20)
	dataLines := make([]string, 0, 2)
	eventName := ""
	isSuccessTerminalEvent := func(eventType string) bool {
		switch eventType {
		case "response.completed", "response.done", "response.finished":
			return true
		default:
			return false
		}
	}
	isFailureTerminalEvent := func(eventType string) bool {
		switch eventType {
		case "response.failed", "response.incomplete", "response.cancelled", "response.error":
			return true
		default:
			return false
		}
	}
	isFailureStatus := func(status string) bool {
		switch status {
		case "failed", "incomplete", "cancelled", "error":
			return true
		default:
			return false
		}
	}
	isSuccessStatus := func(status string) bool {
		switch status {
		case "completed", "complete", "done", "finished":
			return true
		default:
			return false
		}
	}
	flushItem := func(object map[string]any, eventType string) {
		item := bpObject(object["item"])
		if item == nil || eventType != "response.output_item.done" {
			return
		}
		itemStatus := strings.ToLower(bpString(item["status"]))
		if isFailureStatus(itemStatus) {
			terminalFailure = itemStatus
			return
		}
		index := len(finishedOrder)
		if value, ok := object["output_index"].(float64); ok && value >= 0 {
			index = int(value)
		}
		if _, exists := finishedItems[index]; !exists {
			finishedOrder = append(finishedOrder, index)
		}
		finishedItems[index] = bpCloneObject(item)
	}
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if strings.TrimSpace(data) == "[DONE]" {
			return
		}
		var object map[string]any
		if json.Unmarshal([]byte(data), &object) != nil || object == nil {
			return
		}
		typeName := strings.ToLower(strings.TrimSpace(eventName))
		if typeName == "" {
			typeName = strings.ToLower(bpString(object["type"]))
		}
		flushItem(object, typeName)
		if typeName == "response.output_text.delta" {
			if delta := bpString(object["delta"]); delta != "" {
				index := 0
				if value, ok := object["output_index"].(float64); ok && value >= 0 {
					index = int(value)
				}
				textByIndex[index] += delta
			}
		}
		if typeName == "response.output_text.done" {
			text := bpString(object["text"])
			if text == "" {
				text = bpString(object["delta"])
			}
			if text != "" {
				index := 0
				if value, ok := object["output_index"].(float64); ok && value >= 0 {
					index = int(value)
				}
				finalTextByIndex[index] = text
			}
		}
		response := bpObject(object["response"])
		directResponse := false
		if response == nil && (object["status"] != nil || object["output"] != nil || object["id"] != nil) {
			response = object
			directResponse = true
		}
		if response == nil {
			if isFailureTerminalEvent(typeName) {
				terminalFailure = typeName
			}
			return
		}
		status := strings.ToLower(bpString(response["status"]))
		if started == nil || typeName == "response.created" || typeName == "response.in_progress" {
			started = bpCloneObject(response)
		}
		if isFailureTerminalEvent(typeName) || isFailureStatus(status) {
			terminalFailure = status
			if isFailureTerminalEvent(typeName) {
				terminalFailure = typeName
			}
			if terminalFailure == "" {
				terminalFailure = typeName
			}
			return
		}
		if isSuccessTerminalEvent(typeName) || (directResponse && (typeName == "" || typeName == "response") && isSuccessStatus(status)) {
			completed = bpCloneObject(response)
			if isSuccessTerminalEvent(typeName) || bpString(completed["status"]) == "" || completed["status"] == "done" || completed["status"] == "finished" {
				completed["status"] = "completed"
			}
		}
	}
	for reader.Scan() {
		line := strings.TrimSuffix(reader.Text(), "\r")
		if line == "" {
			flush()
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	if err := reader.Err(); err != nil {
		return nil, err
	}
	if terminalFailure != "" {
		return nil, fmt.Errorf("BPS 流响应以 %s 结束", terminalFailure)
	}
	if completed == nil && (len(finishedItems) > 0 || len(textByIndex) > 0 || len(finalTextByIndex) > 0) {
		if started == nil {
			started = map[string]any{}
		}
		completed = bpCloneObject(started)
		completed["status"] = "completed"
		outputByIndex := map[int]any{}
		for index, item := range finishedItems {
			outputByIndex[index] = item
		}
		textIndices := map[int]struct{}{}
		for index := range textByIndex {
			textIndices[index] = struct{}{}
		}
		for index := range finalTextByIndex {
			textIndices[index] = struct{}{}
		}
		for index := range textIndices {
			if _, exists := outputByIndex[index]; exists {
				continue
			}
			text := finalTextByIndex[index]
			if text == "" {
				text = textByIndex[index]
			}
			if text != "" {
				outputByIndex[index] = map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
			}
		}
		indices := append([]int(nil), finishedOrder...)
		for index := range outputByIndex {
			found := false
			for _, existing := range indices {
				if existing == index {
					found = true
					break
				}
			}
			if !found {
				indices = append(indices, index)
			}
		}
		if len(indices) > 0 {
			sort.Ints(indices)
			output := make([]any, 0, len(indices))
			for _, index := range indices {
				if item, exists := outputByIndex[index]; exists {
					output = append(output, item)
				}
			}
			completed["output"] = output
		}
	}
	if completed == nil {
		return nil, fmt.Errorf("BPS 流响应没有可完成的响应对象")
	}
	return completed, nil
}

func bpSyntheticStream(response map[string]any) []byte {
	if response == nil {
		return nil
	}
	created := bpCloneObject(response)
	created["status"] = "in_progress"
	created["output"] = []any{}
	var builder strings.Builder
	bpWriteSSE(&builder, "response.created", map[string]any{"type": "response.created", "response": created})
	bpWriteSSE(&builder, "response.in_progress", map[string]any{"type": "response.in_progress", "response": created})
	if output, ok := response["output"].([]any); ok {
		for index, raw := range output {
			item := bpObject(raw)
			if item == nil {
				continue
			}
			bpWriteSSE(&builder, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
			if kind := bpString(item["type"]); kind == "function_call" || kind == "custom_tool_call" {
				if arguments := bpString(item["arguments"]); arguments != "" {
					bpWriteSSE(&builder, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": bpString(item["id"]), "arguments": arguments})
				}
			}
			bpWriteSSE(&builder, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		}
	}
	completed := bpCloneObject(response)
	completed["status"] = "completed"
	bpWriteSSE(&builder, "response.completed", map[string]any{"type": "response.completed", "response": completed})
	builder.WriteString("data: [DONE]\n\n")
	return []byte(builder.String())
}

func bpWriteSSE(builder *strings.Builder, event string, value any) {
	builder.WriteString("event: ")
	builder.WriteString(event)
	builder.WriteString("\ndata: ")
	builder.Write(bpJSON(value))
	builder.WriteString("\n\n")
}

func (s *Server) bpTransformResponse(raw []byte, source map[string]any) ([]byte, bool, bool, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return raw, false, false, fmt.Errorf("BPS 返回空响应")
	}
	isStream := strings.Contains(trimmed, "data:")
	var response map[string]any
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &response); err != nil || response == nil {
			return raw, false, false, fmt.Errorf("BPS 返回的 JSON 无法解析")
		}
	} else {
		var err error
		response, err = bpParseSSEFinal(raw)
		if err != nil {
			return raw, false, true, err
		}
		isStream = true
	}
	if bpRequestHasToolResult(source) && bpString(response["status"]) == "completed" && !bpResponseHasUsefulOutput(response) {
		return raw, false, isStream, &bpEmptyContinuationError{}
	}
	toolCall, ok, extractErr := s.bpExtractNativeClientToolCall(response, source)
	if extractErr != nil {
		return raw, false, isStream, extractErr
	}
	if !ok {
		return raw, false, isStream, nil
	}
	bpReplaceNativeCall(response, toolCall)
	if isStream {
		return bpSyntheticStream(response), true, true, nil
	}
	return bpJSON(response), true, false, nil
}

func bpDecodeSource(body []byte) (map[string]any, error) {
	var source map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&source); err != nil || source == nil {
		return nil, fmt.Errorf("直连请求体必须是完整的 JSON 对象")
	}
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("直连请求体必须是完整的 JSON 对象")
	}
	return source, nil
}
