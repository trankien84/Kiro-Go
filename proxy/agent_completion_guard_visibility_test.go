package proxy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCompletionGuardAuditContinuationVisibilityHidesInternalNarration(t *testing.T) {
	candidateText := "Đo được hai hình thái thật. Giờ dùng chính máy đánh giá đã khoá để đo, không dùng regex của tôi."
	candidate := ClaudeResponse{
		Content:    []ClaudeContentBlock{{Type: "text", Text: candidateText}},
		StopReason: "end_turn",
		Usage: ClaudeUsage{
			InputTokens:  11,
			OutputTokens: 17,
		},
	}
	tool := ClaudeContentBlock{
		Type: "tool_use",
		ID:   "tool-locked-evaluator",
		Name: "locked_evaluator",
		Input: map[string]interface{}{
			"target": "dis-g",
		},
		Extra: map[string]json.RawMessage{
			"test_metadata": json.RawMessage(`{"source":"continuation"}`),
		},
	}

	for _, tc := range []struct {
		name      string
		narration string
		connector string
	}{
		{
			name:      "english narration",
			narration: "I'll measure with the locked evaluator instead of my regex probe.",
			connector: "I'll check the locked schema first.",
		},
		{
			name:      "vietnamese narration",
			narration: "Tôi sẽ đo bằng máy đánh giá đã khoá.",
			connector: "Tôi sẽ kiểm tra schema đã khoá trước.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			continuation := ClaudeResponse{
				Content: []ClaudeContentBlock{
					{Type: "text", Text: tc.narration},
					{Type: "connector_text", Text: tc.connector},
					{Type: "thinking", Thinking: "internal reasoning"},
					{Type: "redacted_thinking", Data: "opaque"},
					tool,
				},
				StopReason: "tool_use",
				Usage: ClaudeUsage{
					InputTokens:  13,
					OutputTokens: 1000,
				},
			}
			originalContinuation := append([]ClaudeContentBlock(nil), continuation.Content...)

			got := completionGuardMergeAuditContinuationVisibleResponse(
				candidate,
				continuation,
				candidateText,
			)

			if len(got.Content) != 2 {
				t.Fatalf("visible content blocks=%d, want candidate text + tool_use only: %#v", len(got.Content), got.Content)
			}
			if !reflect.DeepEqual(got.Content[0], candidate.Content[0]) {
				t.Fatalf("candidate content changed: got=%#v want=%#v", got.Content[0], candidate.Content[0])
			}
			if !reflect.DeepEqual(got.Content[1], tool) {
				t.Fatalf("tool_use changed: got=%#v want=%#v", got.Content[1], tool)
			}
			if got.StopReason != "tool_use" {
				t.Fatalf("stop_reason=%q, want tool_use", got.StopReason)
			}
			if !reflect.DeepEqual(continuation.Content, originalContinuation) {
				t.Fatalf("continuation content was mutated: got=%#v want=%#v", continuation.Content, originalContinuation)
			}

			prepared, _ := prepareCompletionGuardVisibleRecovery(
				continuation.Content,
				candidateText,
				true,
			)
			wantUsage := completionGuardCloneUsage(candidate.Usage)
			addCompletionGuardVisibleOutputUsage(
				&wantUsage,
				continuation.Usage,
				prepared,
				completionGuardEmitToolsOnly,
			)
			if !reflect.DeepEqual(got.Usage, wantUsage) {
				t.Fatalf("visible usage drift: got=%#v want=%#v", got.Usage, wantUsage)
			}
			if got.Usage.OutputTokens >= candidate.Usage.OutputTokens+continuation.Usage.OutputTokens {
				t.Fatalf("hidden continuation output was counted as visible: got=%d candidate=%d continuation=%d", got.Usage.OutputTokens, candidate.Usage.OutputTokens, continuation.Usage.OutputTokens)
			}
		})
	}
}

func TestCompletionGuardAuditContinuationVisibilityPreservesCandidateBoundaryExactly(t *testing.T) {
	candidate := ClaudeResponse{
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "Kết luận thứ nhất."},
			{Type: "text", Text: "Kết luận thứ hai không có khoảng trắng cuối."},
		},
		StopReason: "end_turn",
	}
	continuation := ClaudeResponse{
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "I'll continue internally."},
			{Type: "tool_use", ID: "tool-1", Name: "inspect", Input: map[string]interface{}{"path": "schema.json"}},
		},
		StopReason: "tool_use",
	}

	got := completionGuardMergeAuditContinuationVisibleResponse(
		candidate,
		continuation,
		"Kết luận thứ nhất.Kết luận thứ hai không có khoảng trắng cuối.",
	)

	if len(got.Content) != len(candidate.Content)+1 {
		t.Fatalf("visible blocks=%d, want=%d: %#v", len(got.Content), len(candidate.Content)+1, got.Content)
	}
	if !reflect.DeepEqual(got.Content[:len(candidate.Content)], candidate.Content) {
		t.Fatalf("candidate block boundary changed: got=%#v want=%#v", got.Content[:len(candidate.Content)], candidate.Content)
	}
	if got.Content[len(candidate.Content)].Type != "tool_use" {
		t.Fatalf("last block=%#v, want tool_use", got.Content[len(candidate.Content)])
	}
}
