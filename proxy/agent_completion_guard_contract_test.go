package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestCompletionGuardContractObservedCopilotBenignProjectionIsReplaySafe(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":4096,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh"},
		"system":[{"type":"text","text":"agent system","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"implement and test","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"name":"run_in_terminal","description":"run commands","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]
	}`)
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		http.Header{"Anthropic-Beta": []string{"interleaved-thinking-2025-05-14,advanced-tool-use-2025-11-20,prompt-caching-scope-2026-01-05,effort-2025-11-24"}},
		"-thinking",
		adaptiveClaudeCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if !plan.Requirements.AllowsKiro() || plan.KiroProjection.Fidelity != claudeProjectionEmulated {
		t.Fatalf("requirements=%s projection=%s", plan.Requirements.Summary(), plan.KiroProjection.Summary())
	}
	safety, reason := completionGuardClaudeProjectionReplaySafety(plan.KiroProjection)
	if safety != completionGuardProjectionReplaySafe || reason != "" {
		t.Fatalf("safety=%s reason=%q projection=%s", safety.String(), reason, plan.KiroProjection.Summary())
	}
}

func TestCompletionGuardContractProjectionReplaySafetyAllowsBenignEmulation(t *testing.T) {
	projection := newClaudeKiroProjection()
	for _, feature := range []string{
		"thinking_effort",
		"content_cache_control",
		"tool_cache_control",
		"nested_tool_result_image",
	} {
		projection.add(feature, claudeProjectionEmulated, "test")
	}

	safety, reason := completionGuardClaudeProjectionReplaySafety(projection)
	if safety != completionGuardProjectionReplaySafe || reason != "" {
		t.Fatalf("safety=%s reason=%q projection=%s", safety.String(), reason, projection.Summary())
	}
}

func TestCompletionGuardContractProjectionReplaySafetyRejectsHistoryDependentEmulation(t *testing.T) {
	for _, feature := range []string{"mid_conversation_system", "context_management", "future_emulation"} {
		t.Run(feature, func(t *testing.T) {
			projection := newClaudeKiroProjection()
			projection.add("thinking_effort", claudeProjectionEmulated, "safe")
			projection.add(feature, claudeProjectionEmulated, "unsafe")

			safety, reason := completionGuardClaudeProjectionReplaySafety(projection)
			if safety != completionGuardProjectionReplayUnsafe || reason == "" {
				t.Fatalf("safety=%s reason=%q projection=%s", safety.String(), reason, projection.Summary())
			}
		})
	}
}

func TestCompletionGuardContractProjectionReplaySafetyExactAndUnsupported(t *testing.T) {
	exact := newClaudeKiroProjection()
	if safety, reason := completionGuardClaudeProjectionReplaySafety(exact); safety != completionGuardProjectionReplayExact || reason != "" {
		t.Fatalf("exact safety=%s reason=%q", safety.String(), reason)
	}

	unsupported := newClaudeKiroProjection()
	unsupported.add("unsupported_feature", claudeProjectionUnsupported, "test")
	if safety, reason := completionGuardClaudeProjectionReplaySafety(unsupported); safety != completionGuardProjectionReplayUnsafe || reason != "projection_unsupported" {
		t.Fatalf("unsupported safety=%s reason=%q", safety.String(), reason)
	}
}

func TestCompletionGuardContractNaturalLanguageCompletionDelegatesToSemanticAudit(t *testing.T) {
	plan := completionGuardTestPlanWithTools("auto", ClaudeTool{Name: "run_in_terminal"})

	cases := []string{
		"Extractor verified. Now the acquisition CLI that retains verbatim fragments and writes receipts.",
		"The parser is ready. I'll run the tests next.",
		"Đã kiểm tra parser. Tiếp theo mình sẽ chạy test.",
		"Please confirm the destructive change before I continue.",
		"Thay đổi này có thể phá backward compatibility. Mình sẽ tiếp tục sau khi bạn xác nhận.",
	}

	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content:    []ClaudeContentBlock{{Type: "text", Text: text}},
			}, plan)
			if !outcome.Accepted || outcome.Kind != "prose" || outcome.Reason != "" {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}
}

func TestCompletionGuardContractAuditPreservesHumanWait(t *testing.T) {
	for _, fragment := range []string{
		"waiting_for_user",
		"safe progress requires clarification, confirmation",
		"approval, a user choice, credentials",
		"from the user",
	} {
		if !strings.Contains(completionGuardAuditSystemPrompt, fragment) {
			t.Fatalf("audit prompt missing human-wait rule %q", fragment)
		}
	}

	description := completionGuardAuditTool("audit").Description
	if !strings.Contains(description, "waiting_for_user") ||
		!strings.Contains(description, "clarification, confirmation, approval") ||
		!strings.Contains(description, "user-provided prerequisite") {
		t.Fatalf("audit tool description does not preserve human wait: %q", description)
	}
}
