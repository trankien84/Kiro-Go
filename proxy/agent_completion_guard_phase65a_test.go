package proxy

import (
	"net/http"
	"testing"
)

func TestPhase65AObservedCopilotBenignProjectionIsReplaySafe(t *testing.T) {
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

func TestPhase65AProjectionReplaySafetyAllowsBenignEmulation(t *testing.T) {
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

func TestPhase65AProjectionReplaySafetyRejectsHistoryDependentEmulation(t *testing.T) {
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

func TestPhase65AProjectionReplaySafetyExactAndUnsupported(t *testing.T) {
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

func TestPhase65ACompletionGuardRejectsAnnouncedPendingAction(t *testing.T) {
	plan := completionGuardTestPlanWithTools("auto", ClaudeTool{Name: "run_in_terminal"})

	cases := []string{
		"Extractor verified. Now the acquisition CLI that retains verbatim fragments and writes receipts.",
		"The parser is ready. I'll run the tests next.",
		"Enough context. Starting with the fragment extractor.",
		"Đã kiểm tra parser. Tiếp theo mình sẽ chạy test.",
		"I'll run the tests next, and if they fail I'll fix them.",
		"Mình sẽ chạy test tiếp theo, và nếu test lỗi mình sẽ sửa.",
	}

	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content:    []ClaudeContentBlock{{Type: "text", Text: text}},
			}, plan)
			if outcome.Accepted || outcome.Reason != completionGuardPendingActionReason {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}
}

func TestPhase65ACompletionGuardDoesNotOverreachOnFinalOrUserDirectedProse(t *testing.T) {
	plan := completionGuardTestPlanWithTools("auto", ClaudeTool{Name: "run_in_terminal"})

	cases := []string{
		"Extractor verified. The implementation is complete.",
		"Now the parser is ready for use.",
		"Next, you can run the integration tests locally if you want an additional check.",
		"The next steps are documented in README.md.",
		"I will run the tests during implementation. The tests passed and the work is complete.",
		"The implementation is complete. Next steps are documented in README.md.",
		"Let me know if you want another example.",
		"The implementation is complete. I'll review it if you want.",
		"Everything is done. I will continue to help if needed.",
		"The tests pass. I'll check anything else if you'd like.",
		"Mọi thứ đã hoàn tất. Mình sẽ kiểm tra thêm nếu bạn muốn.",
		"Đã xong. Tôi sẽ tiếp tục hỗ trợ nếu cần.",
		"Now the parser handles images correctly.",
		"Then the validator reports the final status.",
	}

	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content:    []ClaudeContentBlock{{Type: "text", Text: text}},
			}, plan)
			if !outcome.Accepted || outcome.Kind != "prose" {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}
}

func TestPhase65ACompletionGuardPendingActionRequiresCallableClientTool(t *testing.T) {
	plan := agentCompletionGuardPlan{ToolChoiceKind: "auto"}
	outcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{{
			Type: "text",
			Text: "I'll run the tests next.",
		}},
	}, plan)
	if !outcome.Accepted || outcome.Kind != "prose" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestPhase65ACompletionGuardPendingActionPreservesNonTerminalStopSemantics(t *testing.T) {
	plan := completionGuardTestPlanWithTools("auto", ClaudeTool{Name: "run_in_terminal"})
	outcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "max_tokens",
		Content: []ClaudeContentBlock{{
			Type: "text",
			Text: "I'll run the tests next.",
		}},
	}, plan)
	if !outcome.Accepted || outcome.Kind != "prose" || outcome.Reason != "" {
		t.Fatalf("outcome=%#v", outcome)
	}
}
