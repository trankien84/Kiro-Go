package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func completionGuardTestRaw(value string) json.RawMessage {
	return json.RawMessage(value)
}

func TestCompletionGuardProtocolRegistryExactMappings(t *testing.T) {
	tests := []struct {
		toolType string
		name     string
		result   string
		canCall  bool
	}{
		{"code_execution_20250522", "code_execution", "code_execution_tool_result", false},
		{"code_execution_20250825", "bash_code_execution", "bash_code_execution_tool_result", false},
		{"code_execution_20250825", "text_editor_code_execution", "text_editor_code_execution_tool_result", false},
		{"code_execution_20260120", "code_execution", "code_execution_tool_result", true},
		{"code_execution_20260521", "code_execution", "code_execution_tool_result", true},
		{"tool_search_tool_regex", "tool_search_tool_regex", "tool_search_tool_result", false},
		{"tool_search_tool_regex_20251119", "tool_search_tool_regex", "tool_search_tool_result", false},
		{"tool_search_tool_bm25", "tool_search_tool_bm25", "tool_search_tool_result", false},
		{"tool_search_tool_bm25_20251119", "tool_search_tool_bm25", "tool_search_tool_result", false},
		{"advisor_20260301", "advisor", "advisor_tool_result", false},
	}

	for _, tc := range tests {
		t.Run(tc.toolType+"/"+tc.name, func(t *testing.T) {
			definition, ok := completionGuardLookupServerToolDefinition(tc.toolType)
			if !ok {
				t.Fatalf("missing definition for %q", tc.toolType)
			}
			found := false
			for _, wire := range definition.WireTools {
				if wire.Name != tc.name {
					continue
				}
				found = true
				if wire.ResultBlockType != tc.result || wire.CanCallTools != tc.canCall {
					t.Fatalf("wire=%#v want result=%q canCall=%t", wire, tc.result, tc.canCall)
				}
			}
			if !found {
				t.Fatalf("wire name %q missing from %#v", tc.name, definition)
			}
		})
	}

	if _, ok := completionGuardLookupServerToolDefinition("future_tool_20990101"); ok {
		t.Fatal("unknown server tool was guessed instead of failing closed")
	}
}

func TestCompletionGuardToolSearchCatalogValidation(t *testing.T) {
	searchTypes := []string{
		"tool_search_tool_regex",
		"tool_search_tool_regex_20251119",
		"tool_search_tool_bm25",
		"tool_search_tool_bm25_20251119",
	}
	for _, toolType := range searchTypes {
		t.Run("alias/"+toolType, func(t *testing.T) {
			name, _ := completionGuardExpectedServerToolName(toolType)
			plan := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
				Tools: []ClaudeTool{{Type: toolType, Name: name}},
			}, false, "claude-haiku-4-5-20251001")
			if plan.ValidationReason != "" {
				t.Fatalf("valid alias rejected: %q", plan.ValidationReason)
			}
		})
	}

	deferredSearch := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Tools: []ClaudeTool{{
			Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex",
			Extra: map[string]json.RawMessage{"defer_loading": completionGuardTestRaw(`true`)},
		}},
	}, false, "claude-opus-4-8")
	if !strings.Contains(deferredSearch.ValidationReason, "tool search itself cannot use defer_loading=true") {
		t.Fatalf("deferred search validation=%q", deferredSearch.ValidationReason)
	}

	badType := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Tools: []ClaudeTool{
			{Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex"},
			{Name: "query", Extra: map[string]json.RawMessage{"defer_loading": completionGuardTestRaw(`"true"`)}},
		},
	}, false, "claude-opus-4-8")
	if !strings.Contains(badType.ValidationReason, "defer_loading must be a boolean") {
		t.Fatalf("typed defer validation=%q", badType.ValidationReason)
	}

	deferredCache := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Tools: []ClaudeTool{
			{Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex"},
			{Name: "query", Extra: map[string]json.RawMessage{
				"defer_loading": completionGuardTestRaw(`true`),
				"cache_control": completionGuardTestRaw(`{"type":"ephemeral"}`),
			}},
		},
	}, false, "claude-opus-4-8")
	if !strings.Contains(deferredCache.ValidationReason, "cannot combine defer_loading=true with cache_control") {
		t.Fatalf("deferred cache validation=%q", deferredCache.ValidationReason)
	}

	unsupported := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex"}},
	}, false, "claude-opus-4-1")
	if unsupported.ValidationReason != "" {
		t.Fatalf("model support must be evaluated after deployment selection: %q", unsupported.ValidationReason)
	}

	legacyDateID := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex"}},
	}, false, "claude-opus-4-20250514")
	if legacyDateID.ValidationReason != "" {
		t.Fatalf("model support must be evaluated after deployment selection: %q", legacyDateID.ValidationReason)
	}

	unknownAlias := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex"}},
	}, false, "router/custom-latest")
	if unknownAlias.ValidationReason != "" {
		t.Fatalf("unknown routed alias must not be hard rejected: %q", unknownAlias.ValidationReason)
	}
}

func TestCompletionGuardAdvisorDefinitionAndHeaders(t *testing.T) {
	validTool := ClaudeTool{
		Type: "advisor_20260301", Name: "advisor",
		Extra: map[string]json.RawMessage{
			"model":      completionGuardTestRaw(`"claude-opus-4-8"`),
			"max_tokens": completionGuardTestRaw(`1024`),
			"max_uses":   completionGuardTestRaw(`0`),
		},
	}
	valid := buildAgentCompletionGuardPlan(&ClaudeRequest{Tools: []ClaudeTool{validTool}}, false)
	if valid.ValidationReason != "" {
		t.Fatalf("valid advisor rejected: %q", valid.ValidationReason)
	}

	for _, tc := range []struct {
		name  string
		extra map[string]json.RawMessage
		want  string
	}{
		{"missing_model", nil, "model is required"},
		{"model_wrong_type", map[string]json.RawMessage{"model": completionGuardTestRaw(`123`)}, "model must be a string"},
		{"max_tokens_too_small", map[string]json.RawMessage{"model": completionGuardTestRaw(`"claude-opus-4-8"`), "max_tokens": completionGuardTestRaw(`1023`)}, "at least 1024"},
		{"max_tokens_not_integer", map[string]json.RawMessage{"model": completionGuardTestRaw(`"claude-opus-4-8"`), "max_tokens": completionGuardTestRaw(`1024.5`)}, "must be an integer"},
		{"max_uses_negative", map[string]json.RawMessage{"model": completionGuardTestRaw(`"claude-opus-4-8"`), "max_uses": completionGuardTestRaw(`-1`)}, "must not be negative"},
		{"max_uses_wrong_type", map[string]json.RawMessage{"model": completionGuardTestRaw(`"claude-opus-4-8"`), "max_uses": completionGuardTestRaw(`"2"`)}, "must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Tools: []ClaudeTool{{Type: "advisor_20260301", Name: "advisor", Extra: tc.extra}},
			}, false)
			if !strings.Contains(plan.ValidationReason, tc.want) {
				t.Fatalf("validation=%q want substring %q", plan.ValidationReason, tc.want)
			}
		})
	}

	req := &ClaudeRequest{Tools: []ClaudeTool{validTool}}
	semanticPlan := buildAgentCompletionGuardPlan(req, false)
	if semanticPlan.ValidationReason != "" {
		t.Fatalf("semantic plan validation=%q", semanticPlan.ValidationReason)
	}
	if got := completionGuardValidateSemanticHeaders(semanticPlan.Catalog, nil); !strings.Contains(got, "requires anthropic-beta") {
		t.Fatalf("missing beta validation=%q", got)
	}
	headers := make(http.Header)
	headers.Add("Anthropic-Beta", "other-beta, advisor-tool-2026-03-01")
	headers.Add("Anthropic-Beta", "another-beta")
	if got := completionGuardValidateSemanticHeaders(semanticPlan.Catalog, headers); got != "" {
		t.Fatalf("valid beta headers rejected: %q", got)
	}
}
