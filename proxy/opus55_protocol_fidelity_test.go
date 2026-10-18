package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"kiro-go/config"
)

func opus55NativePlan(t *testing.T, body string, headers http.Header) claudeRequestPlan {
	t.Helper()
	plan, err := buildClaudeRequestPlanWithResolver([]byte(body), headers, "-thinking", resolveClaudeProviderReasoningCapability)
	if err != nil {
		t.Fatalf("request planner: %v", err)
	}
	return plan
}

func TestOpus55ThinkingUpdatesBetaRoutingAndWire(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"run tools"}]}`
	if _, err := buildClaudeRequestPlanWithResolver([]byte(body), nil, "-thinking", resolveClaudeProviderReasoningCapability); err == nil || !strings.Contains(err.Message, "thinking-display-updates") {
		t.Fatalf("missing beta must be rejected; got %v", err)
	}
	header := http.Header{"Anthropic-Beta": []string{"thinking-display-updates-2026-08-18"}}
	plan := opus55NativePlan(t, body, header)
	if plan.Requirements.AllowsKiro() || plan.Requirements.Allows(claudeTransportBedrockConverse) {
		t.Fatalf("thinking updates must use native Messages path: %s", plan.Requirements.Summary())
	}
	if !plan.Requirements.Allows(claudeTransportDirectAnthropic) || !plan.Requirements.Allows(claudeTransportBedrockInvoke) {
		t.Fatalf("native Anthropic and Bedrock should preserve the beta: %s", plan.Requirements.Summary())
	}
	if got := plan.Semantic.Headers.Get("Anthropic-Beta"); got != "thinking-display-updates-2026-08-18" {
		t.Fatalf("beta was lost: %q", got)
	}
	if !bytes.Contains(plan.Semantic.Body, []byte(`"display":"updates"`)) {
		t.Fatalf("display lost from native request: %s", plan.Semantic.Body)
	}
	bad := strings.Replace(body, `"type":"adaptive"`, `"type":"disabled"`, 1)
	if _, err := buildClaudeRequestPlanWithResolver([]byte(bad), header, "-thinking", resolveClaudeProviderReasoningCapability); err == nil {
		t.Fatal("updates with disabled thinking was accepted")
	}
}

func TestOpus55ComputerToolsetNativeOnlyAndLegacyBoundary(t *testing.T) {
	const toolset = `{"model":"claude-opus-5-5","max_tokens":2048,"tools":[{"type":"computer_toolset_20260801","configs":{"zoom":{"enabled":false}}}],"messages":[{"role":"user","content":"click"}]}`
	plan := opus55NativePlan(t, toolset, nil)
	if plan.Requirements.AllowsKiro() || plan.Requirements.Allows(claudeTransportBedrockInvoke) || !plan.Requirements.Allows(claudeTransportDirectAnthropic) {
		t.Fatalf("new computer toolset must not be translated to old formats: %s", plan.Requirements.Summary())
	}
	if !bytes.Contains(plan.Semantic.Body, []byte(`"computer_toolset_20260801"`)) || !bytes.Contains(plan.Semantic.Body, []byte(`"zoom"`)) {
		t.Fatalf("toolset payload changed: %s", plan.Semantic.Body)
	}
	var h Handler
	official := &config.Account{ID: "official", AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com/v1"}
	compatible := &config.Account{ID: "compatible", AuthMethod: "anthropic_api", BaseURL: "https://compatible.example/v1"}
	bedrock := &config.Account{ID: "bedrock", AuthMethod: "bedrock"}
	const legacy = `{"model":"claude-opus-5-5","max_tokens":2048,"tools":[{"type":"computer_20251124","name":"computer","display_width_px":1024,"display_height_px":768}],"messages":[{"role":"user","content":"click"}]}`
	older := opus55NativePlan(t, legacy, http.Header{"Anthropic-Beta": []string{"computer-use-2025-11-24"}})
	if !older.Requirements.UsesLegacyComputerTool {
		t.Fatal("legacy tool not classified")
	}
	if ok, reason := h.claudeAccountSupportsRequestedProtocol(official, "claude-opus-5-5", older.Requirements); ok || !strings.Contains(reason, "computer_toolset_20260801") {
		t.Fatalf("official Opus 5.5 accepted legacy computer: ok=%v reason=%q", ok, reason)
	}
	if ok, reason := h.claudeAccountSupportsRequestedProtocol(bedrock, "anthropic.claude-opus-5-5:0", older.Requirements); !ok {
		t.Fatalf("Bedrock Opus 5.5 retains legacy computer version: %q", reason)
	}
	if ok, reason := h.claudeAccountSupportsRequestedProtocol(compatible, "claude-opus-5-5", older.Requirements); !ok {
		t.Fatalf("compatible provider owns its tool contract: %q", reason)
	}
	if ok, reason := h.claudeAccountSupportsRequestedProtocol(official, "claude-opus-5", older.Requirements); !ok {
		t.Fatalf("Opus 5 must keep legacy computer support: %q", reason)
	}
	for _, invalid := range []string{
		strings.Replace(toolset, `"configs"`, `"name":"computer","configs"`, 1),
		strings.Replace(toolset, `"configs"`, `"defer_loading":true,"configs"`, 1),
		strings.Replace(toolset, `}],"messages"`, `},{"type":"computer_20251124","name":"computer"}],"messages"`, 1),
	} {
		if _, err := buildClaudeRequestPlanWithResolver([]byte(invalid), nil, "-thinking", resolveClaudeProviderReasoningCapability); err == nil {
			t.Fatalf("invalid computer toolset declaration accepted: %s", invalid)
		}
	}
}

func TestOpus55SignedThinkingToolResultWireRoundTrip(t *testing.T) {
	const original = `{"model":"claude-opus-5-5","max_tokens":2048,"tools":[{"type":"computer_toolset_20260801"}],"messages":[{"role":"user","content":"click"},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"opaque-sig-55"},{"type":"tool_use","id":"toolu-55","name":"left_click","toolset_name":"computer","input":{"coordinate":[10,20]}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-55","toolset_name":"computer","content":[{"type":"text","text":"clicked"}]}]}]}`
	plan := opus55NativePlan(t, original, nil)
	if plan.Requirements.AllowsKiro() || !plan.Requirements.Allows(claudeTransportDirectAnthropic) {
		t.Fatalf("signed toolset history cannot be projected to Kiro: %s", plan.Requirements.Summary())
	}
	var before, after interface{}
	if err := json.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plan.Semantic.Body, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("signed history changed: %s", plan.Semantic.Body)
	}
}

func TestOpus55SSEOmitsThinkingButPreservesSignatureAndRefusal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5-5")
	if !ok {
		t.Fatal("no streaming writer")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"opaque-sig-55\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Unable to assist\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_sequence\":null,\"stop_details\":{\"type\":\"refusal\",\"category\":\"other\"}},\"usage\":{\"output_tokens\":4}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if !writer.protocolComplete() {
		t.Fatalf("SSE incomplete: reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
	}
	response := writer.response()
	if len(response.Content) != 2 || response.Content[0].Thinking != "" || response.Content[0].Signature != "opaque-sig-55" || !response.Content[0].hasKnownJSONField("thinking") {
		t.Fatalf("signed omitted block lost: %#v", response.Content)
	}
	if response.StopReason != "refusal" || !bytes.Contains(response.StreamDeltaExtra["stop_details"], []byte("other")) {
		t.Fatalf("refusal metadata lost: %#v", response)
	}
}

func TestOpus55CompletionGuardFallbackDoesNotReplayOldSignatures(t *testing.T) {
	response := &ClaudeResponse{
		Model: "claude-opus-5",
		Content: []ClaudeContentBlock{
			{Type: "thinking", Thinking: "", Signature: "opus55-old"},
			{Type: "fallback", Extra: map[string]json.RawMessage{
				"from": json.RawMessage(`{"model":"claude-opus-5-5"}`),
				"to":   json.RawMessage(`{"model":"claude-opus-5"}`),
			}},
			{Type: "thinking", Thinking: "", Signature: "opus5-new"},
			{Type: "text", Text: "result"},
		},
	}
	transition := completionGuardValidateModelTransitionForRequest("claude-opus-5-5", response)
	if !transition.Valid || transition.FinalBoundaryIndex != 1 {
		t.Fatalf("bad transition: %#v", transition)
	}
	filtered := completionGuardRecoveryAssistantContent(response.Content, "pause_turn_requires_continuation")
	encoded, err := json.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("opus55-old")) || !bytes.Contains(encoded, []byte("opus5-new")) {
		t.Fatalf("cross-model continuation replayed old signature: %s", encoded)
	}
}

func TestOpus55ThinkingUpdatesBedrockProjectionKeepsBeta(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"progress"}]}`
	header := http.Header{"Anthropic-Beta": []string{"thinking-display-updates-2026-08-18"}}
	plan := opus55NativePlan(t, body, header)
	projected, err := buildBedrockBodyWithSemanticHeaders(plan.Semantic.Body, plan.Semantic.Headers)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(projected, &wire); err != nil {
		t.Fatal(err)
	}
	var beta []string
	if err := json.Unmarshal(wire["anthropic_beta"], &beta); err != nil {
		t.Fatal(err)
	}
	if len(beta) != 1 || beta[0] != "thinking-display-updates-2026-08-18" || !bytes.Contains(wire["thinking"], []byte(`"updates"`)) {
		t.Fatalf("native Bedrock beta/display was lost: %s", projected)
	}
	if _, exists := wire["model"]; exists {
		t.Fatalf("model leaked into Bedrock Invoke payload: %s", projected)
	}
}

func TestOpus55StreamingProgressThinkingPrecedesToolUse(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5-5")
	if !ok {
		t.Fatal("no streaming writer")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Checking the target\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"opaque-progress-signature\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu-55\",\"name\":\"left_click\",\"toolset_name\":\"computer\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"coordinate\\\":[10,20]}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":10}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if !writer.protocolComplete() {
		t.Fatalf("incomplete: reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
	}
	candidate := writer.response()
	if len(candidate.Content) != 2 || candidate.Content[0].Type != "thinking" || candidate.Content[1].Type != "tool_use" ||
		candidate.Content[0].Thinking != "Checking the target" || candidate.Content[0].Signature != "opaque-progress-signature" ||
		candidate.Content[1].Name != "left_click" || candidate.StopReason != "tool_use" {
		t.Fatalf("progress/tool ordering changed: %#v", candidate)
	}
	encoded, err := json.Marshal(candidate.Content[1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"toolset_name":"computer"`)) {
		t.Fatalf("toolset_name lost: %s", encoded)
	}
}

func TestOpus55BedrockRejectsStrictToolsAndStructuredOutputs(t *testing.T) {
	var handler Handler
	bedrock := &config.Account{ID: "bedrock", AuthMethod: "bedrock"}
	official := &config.Account{ID: "official", AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com/v1"}
	compatible := &config.Account{ID: "compatible", AuthMethod: "anthropic_api", BaseURL: "https://compatible.example/v1"}
	for _, tc := range []struct {
		name, extra    string
		strict, format bool
	}{
		{"structured JSON", `,"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}`, false, true},
		{"strict client tool", `,"tools":[{"name":"calculate","input_schema":{"type":"object"},"strict":true}]`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"reply"}]` + tc.extra + `}`
			plan := opus55NativePlan(t, body, nil)
			if plan.Requirements.RequiresStructuredOutputFormat != tc.format || plan.Requirements.RequiresStrictToolUse != tc.strict {
				t.Fatalf("requirements: %#v", plan.Requirements)
			}
			if ok, reason := handler.claudeAccountSupportsRequestedProtocol(bedrock, "anthropic.claude-opus-5-5:0", plan.Requirements); ok || !strings.Contains(reason, "Amazon Bedrock") {
				t.Fatalf("Bedrock must reject: ok=%t reason=%q", ok, reason)
			}
			if ok, reason := handler.claudeAccountSupportsRequestedProtocol(official, "claude-opus-5-5", plan.Requirements); !ok {
				t.Fatalf("official Claude API allows feature: %s", reason)
			}
			if ok, reason := handler.claudeAccountSupportsRequestedProtocol(compatible, "claude-opus-5-5", plan.Requirements); !ok {
				t.Fatalf("compatible provider owns the contract: %s", reason)
			}
			if ok, reason := handler.claudeAccountSupportsRequestedProtocol(bedrock, "anthropic.claude-opus-5:0", plan.Requirements); !ok {
				t.Fatalf("old Opus model must be unaffected: %s", reason)
			}
		})
	}
}
