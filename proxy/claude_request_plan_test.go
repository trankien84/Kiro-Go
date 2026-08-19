package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func adaptiveClaudeCapability(model string) ModelReasoningCapability {
	return ModelReasoningCapability{
		ModelID:          model,
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive", "disabled"},
	}
}

func noClaudeReasoningCapability(model string) ModelReasoningCapability {
	return ModelReasoningCapability{ModelID: model}
}

func TestClaudeRequestPlanNormalizesThinkingSuffixBeforeRouting(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-thinking","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		adaptiveClaudeCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if plan.RequestedModel != "claude-opus-5-thinking" || plan.ResolvedModel != "claude-opus-5" {
		t.Fatalf("models requested=%q resolved=%q", plan.RequestedModel, plan.ResolvedModel)
	}
	if plan.Request.Thinking == nil || plan.Request.Thinking.Type != "adaptive" {
		t.Fatalf("thinking=%#v", plan.Request.Thinking)
	}
	if plan.Requirements.Allows(claudeTransportBedrockConverse) {
		t.Fatalf("Converse accepted normalized thinking: %#v", plan.Requirements)
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(plan.Semantic.Body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "claude-opus-5" {
		t.Fatalf("wire model=%#v", wire["model"])
	}
	thinking, _ := wire["thinking"].(map[string]interface{})
	if thinking["type"] != "adaptive" {
		t.Fatalf("wire thinking=%#v body=%s", thinking, plan.Semantic.Body)
	}
}

func TestClaudeRequestPlanKeepsUnrepresentableSuffixKiroOnly(t *testing.T) {
	body := []byte(`{"model":"private-model-thinking","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !plan.LegacySuffixOnly || plan.Request.Model != "private-model" {
		t.Fatalf("plan=%#v", plan)
	}
	if !plan.Requirements.AllowsKiro() || plan.Requirements.Allowed != claudeTransportMaskKiro {
		t.Fatalf("requirements=%#v", plan.Requirements)
	}
	if strings.Contains(string(plan.Semantic.Body), "-thinking") {
		t.Fatalf("private suffix leaked: %s", plan.Semantic.Body)
	}
}

func TestClaudeRequestPlanBuildsCanonicalSemanticBodyOnce(t *testing.T) {
	body := []byte(`{"model":"private-model","max_tokens":64,"betas":["tool-search-tool-2025-10-19"],"future_field":{"enabled":true},"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(plan.Semantic.Body, &wire); err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["betas"]; exists {
		t.Fatalf("compatibility betas leaked into canonical body: %s", plan.Semantic.Body)
	}
	if _, exists := wire["future_field"]; !exists {
		t.Fatalf("unknown field was lost: %s", plan.Semantic.Body)
	}
	if got := plan.Semantic.Headers.Get("Anthropic-Beta"); got != "tool-search-tool-2025-10-19" {
		t.Fatalf("beta header=%q", got)
	}
	if plan.Requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("allowed=%08b requirements=%s", plan.Requirements.Allowed, plan.Requirements.Summary())
	}
}

func TestClaudeRequestPlanExplicitDisabledOverridesThinkingSuffix(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-thinking","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		adaptiveClaudeCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if plan.LegacySuffixOnly || plan.ThinkingRequested {
		t.Fatalf("explicit disabled did not override suffix: %#v", plan)
	}
	if plan.Request.Model != "claude-opus-5" || plan.Request.Thinking == nil || plan.Request.Thinking.Type != "disabled" {
		t.Fatalf("normalized request=%#v", plan.Request)
	}
}

func TestClaudeRequestPlanPreservesExplicitThinking(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		adaptiveClaudeCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if plan.LegacySuffixOnly || !plan.ThinkingRequested || plan.Request.Thinking.Type != "adaptive" {
		t.Fatalf("plan=%#v", plan)
	}
}

func TestClaudeRequestPlanAssistantPrefillUsesModelCapability(t *testing.T) {
	body := func(model string) []byte {
		return []byte(`{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"Complete JSON"},{"role":"assistant","content":"{\"ok\":"}]}`)
	}
	oldPlan, validationErr := buildClaudeRequestPlanWithResolver(body("claude-sonnet-4.5"), nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
	if oldPlan.Requirements.Allowed != want {
		t.Fatalf("old prefill allowed=%08b want=%08b", oldPlan.Requirements.Allowed, want)
	}

	newPlan, validationErr := buildClaudeRequestPlanWithResolver(body("claude-sonnet-4.6"), nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if newPlan.Requirements.Allowed != claudeTransportMaskCustom ||
		!newPlan.Requirements.RequiresAssistantPrefill {
		t.Fatalf("4.6 requirements=%#v", newPlan.Requirements)
	}

	unknownPlan, validationErr := buildClaudeRequestPlanWithResolver(body("private-alias"), nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if unknownPlan.Requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("unknown allowed=%08b", unknownPlan.Requirements.Allowed)
	}
}

func TestClaudeRequestPlanRejectsFinalClientToolUseWithoutResult(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"call it"},{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"echo","input":{}}]}]}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr == nil || !strings.Contains(validationErr.Message, "tool_result") {
		t.Fatalf("validation=%#v", validationErr)
	}
}

func TestClaudeRequestPlanDoesNotConfusePauseTurnReplayWithPrefill(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4.6","max_tokens":64,"messages":[{"role":"user","content":"search"},{"role":"assistant","content":[{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"x"}}]}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if plan.Content.HasAssistantPrefill {
		t.Fatalf("pause_turn replay classified as prefill: %#v", plan.Content)
	}
	if plan.Requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("pause replay allowed=%08b", plan.Requirements.Allowed)
	}
}

func TestClaudeMidConversationToolChangesUseCatalogAndCapability(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"get_weather"}}]}]}`)
	_, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", noClaudeReasoningCapability)
	if validationErr == nil || !strings.Contains(validationErr.Message, "mid-conversation-tool-changes") {
		t.Fatalf("missing beta validation=%#v", validationErr)
	}

	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{"Anthropic-Beta": []string{"mid-conversation-tool-changes-2026-07-01"}},
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
	if plan.Requirements.Allowed != want {
		t.Fatalf("allowed=%08b want=%08b %s", plan.Requirements.Allowed, want, plan.Requirements.Summary())
	}

	bad := []byte(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"missing"}}]}]}`)
	_, validationErr = buildClaudeRequestPlanWithResolver(
		bad,
		http.Header{"Anthropic-Beta": []string{"mid-conversation-tool-changes-2026-07-01"}},
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr == nil || !strings.Contains(validationErr.Message, "undeclared tool") {
		t.Fatalf("missing reference validation=%#v body=%s", validationErr, string(bad))
	}
}

func TestClaudeInterleavedThinkingBetaIsCaseSensitive(t *testing.T) {
	body := `{"model":"claude-opus-4.5","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`
	req, semantic := testClaudeSemantic(t, body, http.Header{
		"Anthropic-Beta": []string{"Interleaved-thinking-2025-05-14"},
	})
	requirements, validationErr := analyzeClaudeTransportRequirements(req, semantic, req.Model)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("uppercase alias allowed=%08b", requirements.Allowed)
	}
}

func TestClaudeMidConversationToolChangesAllowInitiallyDeferredCatalog(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{}},"defer_loading":true}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"get_weather"}}]}]}`)

	var strictReq ClaudeRequest
	if err := json.Unmarshal(body, &strictReq); err != nil {
		t.Fatal(err)
	}
	if _, reason := completionGuardBuildNormalizedCatalog(&strictReq); !strings.Contains(reason, "defer_loading=false") {
		t.Fatalf("strict Tool Search catalog reason=%q", reason)
	}

	_, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		nil,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr == nil || !strings.Contains(validationErr.Message, "mid-conversation-tool-changes") {
		t.Fatalf("missing beta validation=%#v", validationErr)
	}

	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{"Anthropic-Beta": []string{"mid-conversation-tool-changes-2026-07-01"}},
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
	if plan.Requirements.Allowed != want {
		t.Fatalf("allowed=%08b want=%08b %s", plan.Requirements.Allowed, want, plan.Requirements.Summary())
	}
}

func TestClaudeRequestPlanClassifiesToolDeclarationFeaturesByFidelity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		field    string
		allowed  claudeTransportMask
		fidelity claudeProjectionFidelity
	}{
		{
			name:     "strict",
			field:    `"strict":true`,
			allowed:  claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke,
			fidelity: claudeProjectionUnsupported,
		},
		{
			name:     "input examples",
			field:    `"input_examples":[{"value":"x"}]`,
			allowed:  claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke,
			fidelity: claudeProjectionUnsupported,
		},
		{
			name:     "cache control",
			field:    `"cache_control":{"type":"ephemeral"}`,
			allowed:  claudeTransportMaskKiro | claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke,
			fidelity: claudeProjectionEmulated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"echo","description":"echo","input_schema":{"type":"object","properties":{}},` + tc.field + `}],"messages":[{"role":"user","content":"hi"}]}`)
			plan, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", noClaudeReasoningCapability)
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			if plan.Requirements.Allowed != tc.allowed {
				t.Fatalf("allowed=%08b want=%08b %s", plan.Requirements.Allowed, tc.allowed, plan.Requirements.Summary())
			}
			if plan.KiroProjection.Fidelity != tc.fidelity {
				t.Fatalf("projection=%s want=%s summary=%s", plan.KiroProjection.Fidelity, tc.fidelity, plan.KiroProjection.Summary())
			}
		})
	}
}

func TestClaudeRequestPlanRoutesUnknownTopLevelSystemBlocksTransparently(t *testing.T) {
	body := []byte(`{"model":"private-model","max_tokens":64,"system":[{"type":"future_system_block","value":"x"}],"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if plan.Requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("allowed=%08b requirements=%s", plan.Requirements.Allowed, plan.Requirements.Summary())
	}
}

func TestClaudeRequestPlanAllowsKiroToIntentionallyOmitAutomaticPromptCaching(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskKiro | claudeTransportMaskCustom
	if plan.Requirements.Allowed != want {
		t.Fatalf("allowed=%08b want=%08b requirements=%s", plan.Requirements.Allowed, want, plan.Requirements.Summary())
	}
	if plan.KiroProjection.Fidelity != claudeProjectionEmulated ||
		!strings.Contains(plan.KiroProjection.Summary(), "cache_control") {
		t.Fatalf("projection=%s summary=%s", plan.KiroProjection.Fidelity, plan.KiroProjection.Summary())
	}
}

func TestClaudeRequestPlanPreservesUnknownMessageFieldsTransparently(t *testing.T) {
	body := []byte(`{"model":"private-model","max_tokens":64,"messages":[{"role":"user","content":"hi","future_message_field":{"enabled":true}}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if plan.Requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("allowed=%08b requirements=%s", plan.Requirements.Allowed, plan.Requirements.Summary())
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(plan.Semantic.Body, &wire); err != nil {
		t.Fatal(err)
	}
	messages, _ := wire["messages"].([]interface{})
	message, _ := messages[0].(map[string]interface{})
	if _, exists := message["future_message_field"]; !exists {
		t.Fatalf("message field was lost: %s", plan.Semantic.Body)
	}
}

func TestClaudeRequestPlanPreservesNestedThinkingAndOutputConfigFields(t *testing.T) {
	thinkingBody := []byte(`{"model":"claude-opus-5","max_tokens":64,"thinking":{"type":"adaptive","future_thinking":true},"messages":[{"role":"user","content":"hi"}]}`)
	thinkingPlan, validationErr := buildClaudeRequestPlanWithResolver(thinkingBody, nil, "-thinking", adaptiveClaudeCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if thinkingPlan.Requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("thinking allowed=%08b requirements=%s", thinkingPlan.Requirements.Allowed, thinkingPlan.Requirements.Summary())
	}
	if !strings.Contains(string(thinkingPlan.Semantic.Body), `"future_thinking":true`) {
		t.Fatalf("thinking field was lost: %s", thinkingPlan.Semantic.Body)
	}

	outputBody := []byte(`{"model":"claude-opus-5","max_tokens":64,"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`)
	outputPlan, validationErr := buildClaudeRequestPlanWithResolver(outputBody, nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskKiro | claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
	if outputPlan.Requirements.Allowed != want {
		t.Fatalf("output allowed=%08b want=%08b requirements=%s", outputPlan.Requirements.Allowed, want, outputPlan.Requirements.Summary())
	}
	if !strings.Contains(string(outputPlan.Semantic.Body), `"format"`) {
		t.Fatalf("output_config.format was lost: %s", outputPlan.Semantic.Body)
	}
}

func TestClaudeRequestPlanKeepsCompatibilityReasoningEffortOffBedrock(t *testing.T) {
	body := []byte(`{"model":"private-model","max_tokens":64,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", noClaudeReasoningCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskKiro | claudeTransportMaskCustom
	if plan.Requirements.Allowed != want {
		t.Fatalf("allowed=%08b want=%08b requirements=%s", plan.Requirements.Allowed, want, plan.Requirements.Summary())
	}
}

func TestClaudeRequestPlanPreservesExplicitNestedZeroValues(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","max_tokens":64,"thinking":{"type":"disabled","budget_tokens":0,"display":""},"output_config":{"effort":""},"messages":[{"role":"user","content":"hi"}]}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(body, nil, "-thinking", adaptiveClaudeCapability)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(plan.Semantic.Body, &wire); err != nil {
		t.Fatal(err)
	}
	thinking, _ := wire["thinking"].(map[string]interface{})
	if value, exists := thinking["budget_tokens"]; !exists || value != float64(0) {
		t.Fatalf("thinking budget presence=%t value=%#v body=%s", exists, value, plan.Semantic.Body)
	}
	if value, exists := thinking["display"]; !exists || value != "" {
		t.Fatalf("thinking display presence=%t value=%#v body=%s", exists, value, plan.Semantic.Body)
	}
	output, _ := wire["output_config"].(map[string]interface{})
	if value, exists := output["effort"]; !exists || value != "" {
		t.Fatalf("output effort presence=%t value=%#v body=%s", exists, value, plan.Semantic.Body)
	}
}

func TestClaudeRequestPlanResolvesOneCapabilitySnapshot(t *testing.T) {
	calls := 0
	body := []byte(`{
		"model":"claude-opus-4.8",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}]
	}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		make(http.Header),
		"-thinking",
		func(model string) ModelReasoningCapability {
			calls++
			return ModelReasoningCapability{
				ModelID:          model,
				SupportsThinking: true,
				ThinkingTypes:    []string{"adaptive"},
			}
		},
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if calls != 1 {
		t.Fatalf("reasoning resolver calls=%d want=1", calls)
	}
	if plan.Capabilities.ModelID != "claude-opus-4.8" ||
		!plan.Capabilities.ProviderReasoningKnown ||
		plan.Capabilities.Protocol.StreamedRefusal != claudeFeatureSupported {
		t.Fatalf("snapshot=%#v", plan.Capabilities)
	}
	if plan.Capability.ModelID != plan.Capabilities.ProviderReasoning.ModelID ||
		plan.ProviderCapabilityKnown != plan.Capabilities.ProviderReasoningKnown {
		t.Fatalf("compatibility aliases drifted: plan=%#v", plan)
	}
}

func TestClaudeRequestPlanAllowsCopilotStyleAdvancedDeclarationOnKiro(t *testing.T) {
	tools := make([]map[string]interface{}, 64)
	for index := range tools {
		tool := map[string]interface{}{
			"name":        "vscode_tool_" + strings.Repeat("x", index%8) + string(rune('a'+index%26)),
			"description": "client-side VS Code or MCP tool",
			"input_schema": map[string]interface{}{
				"type": "object",
			},
		}
		if index == len(tools)-1 {
			tool["cache_control"] = map[string]interface{}{"type": "ephemeral"}
		}
		tools[index] = tool
	}
	body, err := json.Marshal(map[string]interface{}{
		"model":      "claude-opus-5",
		"max_tokens": 16000,
		"stream":     true,
		"output_config": map[string]interface{}{
			"effort": "xhigh",
		},
		"tools": tools,
		"messages": []map[string]interface{}{{
			"role": "user", "content": "continue the task",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{
			"Anthropic-Beta": []string{"interleaved-thinking-2025-05-14,advanced-tool-use-2025-11-20"},
		},
		"-thinking",
		adaptiveClaudeCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !plan.Requirements.AllowsKiro() {
		t.Fatalf("Copilot-style declaration-only request excluded Kiro: %s", plan.Requirements.Summary())
	}
	if plan.Requirements.RequiresToolSearch {
		t.Fatalf("request plan inferred Tool Search from a beta marker alone: %#v", plan.Requirements)
	}
	if plan.KiroProjection.Fidelity != claudeProjectionEmulated {
		t.Fatalf("projection=%s summary=%s", plan.KiroProjection.Fidelity, plan.KiroProjection.Summary())
	}
}
