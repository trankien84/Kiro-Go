package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestCompletionGuardOfficialPauseTurnPreservesThinkingExactly(t *testing.T) {
	thinking := ClaudeContentBlock{
		Type:      "thinking",
		Thinking:  "reasoning summary",
		Signature: "signed-thinking",
	}
	redacted := ClaudeContentBlock{
		Type: "redacted_thinking",
		Extra: map[string]json.RawMessage{
			"data": json.RawMessage(`"opaque-redacted-payload"`),
		},
	}
	serverUse := ClaudeContentBlock{
		Type:  "server_tool_use",
		ID:    "srvtoolu_fixture",
		Name:  "web_search",
		Input: map[string]interface{}{"query": "official protocol"},
	}
	response := &ClaudeResponse{
		Model:      "claude-opus-5",
		StopReason: "pause_turn",
		Content:    []ClaudeContentBlock{thinking, redacted, serverUse},
	}
	request := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: "search",
		}},
	}

	next, err := completionGuardPauseTurnRequest(request, response)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Messages) != 2 {
		t.Fatalf("messages=%#v", next.Messages)
	}
	encoded, err := json.Marshal(next.Messages[1].Content)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(response.Content)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue interface{}
	var wantValue interface{}
	if json.Unmarshal(encoded, &gotValue) != nil || json.Unmarshal(want, &wantValue) != nil {
		t.Fatal("failed to decode fixture content")
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("pause_turn content changed\n got=%s\nwant=%s", encoded, want)
	}
}

func TestCompletionGuardOfficialFallbackBoundarySanitizesPriorModelState(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{
			"type": "thinking", "thinking": "old", "signature": "old-signature",
		},
		map[string]interface{}{
			"type": "redacted_thinking", "data": "old-redacted",
		},
		map[string]interface{}{
			"type": "fallback",
			"from": map[string]interface{}{"model": "claude-fable-5"},
			"to":   map[string]interface{}{"model": "claude-opus-4.8"},
		},
		map[string]interface{}{
			"type": "thinking", "thinking": "new", "signature": "new-signature",
		},
		map[string]interface{}{"type": "text", "text": "completed"},
	}
	filtered := completionGuardRecoveryAssistantContent(
		content,
		"pause_turn_requires_continuation",
	)
	encoded, err := json.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("old-signature"), []byte("old-redacted")} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("prior-model state survived fallback: %s", encoded)
		}
	}
	for _, required := range [][]byte{[]byte(`"type":"fallback"`), []byte("new-signature"), []byte("completed")} {
		if !bytes.Contains(encoded, required) {
			t.Fatalf("post-fallback state missing %q: %s", required, encoded)
		}
	}
}

func TestCompletionGuardModelTransitionInvariant(t *testing.T) {
	fallback := func(from, to string) ClaudeContentBlock {
		fromJSON, _ := json.Marshal(map[string]string{"model": from})
		toJSON, _ := json.Marshal(map[string]string{"model": to})
		return ClaudeContentBlock{
			Type: "fallback",
			Extra: map[string]json.RawMessage{
				"from": fromJSON,
				"to":   toJSON,
			},
		}
	}
	tests := []struct {
		name    string
		content []ClaudeContentBlock
		want    string
	}{
		{
			name: "continuous chain",
			content: []ClaudeContentBlock{
				fallback("claude-fable-5", "claude-opus-4.8"),
				fallback("claude-opus-4.8", "claude-opus-5"),
			},
		},
		{
			name:    "same model",
			content: []ClaudeContentBlock{fallback("claude-opus-5", "claude-opus-5")},
			want:    "fallback_without_model_transition",
		},
		{
			name: "discontinuous",
			content: []ClaudeContentBlock{
				fallback("claude-fable-5", "claude-opus-4.8"),
				fallback("claude-sonnet-5", "claude-opus-5"),
			},
			want: "discontinuous_fallback_chain",
		},
		{
			name: "cycle",
			content: []ClaudeContentBlock{
				fallback("claude-fable-5", "claude-opus-4.8"),
				fallback("claude-opus-4.8", "claude-fable-5"),
			},
			want: "cyclic_fallback_chain",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := completionGuardValidateModelTransition(&ClaudeResponse{Content: test.content})
			if got != test.want {
				t.Fatalf("reason=%q want=%q", got, test.want)
			}
		})
	}
}

func TestCompletionGuardModelTransitionAnchorsRequestAndResponse(t *testing.T) {
	fallback := func(from, to string) ClaudeContentBlock {
		fromJSON, _ := json.Marshal(map[string]string{"model": from})
		toJSON, _ := json.Marshal(map[string]string{"model": to})
		return ClaudeContentBlock{
			Type: "fallback",
			Extra: map[string]json.RawMessage{
				"from": fromJSON,
				"to":   toJSON,
			},
		}
	}
	tests := []struct {
		name      string
		requested string
		response  ClaudeResponse
		want      string
	}{
		{
			name:      "anchored multi-hop",
			requested: "claude-fable-5",
			response: ClaudeResponse{
				Model: "claude-opus-5",
				Content: []ClaudeContentBlock{
					fallback("claude-fable-5", "claude-opus-4.8"),
					fallback("claude-opus-4.8", "claude-opus-5"),
				},
			},
		},
		{
			name:      "request anchor mismatch",
			requested: "claude-fable-5",
			response: ClaudeResponse{
				Model:   "claude-opus-5",
				Content: []ClaudeContentBlock{fallback("claude-sonnet-5", "claude-opus-5")},
			},
			want: "fallback_request_model_mismatch",
		},
		{
			name:      "response anchor mismatch",
			requested: "claude-fable-5",
			response: ClaudeResponse{
				Model:   "claude-opus-5",
				Content: []ClaudeContentBlock{fallback("claude-fable-5", "claude-opus-4.8")},
			},
			want: "fallback_response_model_mismatch",
		},
		{
			name:      "request anchor is exact",
			requested: "claude-fable-5",
			response: ClaudeResponse{
				Model:   "claude-opus-5",
				Content: []ClaudeContentBlock{fallback("Claude-Fable-5", "claude-opus-5")},
			},
			want: "fallback_request_model_mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := completionGuardValidateModelTransitionForRequest(
				test.requested,
				&test.response,
			)
			if report.Reason != test.want || report.Valid != (test.want == "") {
				t.Fatalf("report=%#v want=%q", report, test.want)
			}
		})
	}
}

func TestCompletionGuardInvalidFallbackRecoveryKeepsOnlyVisibleText(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "thinking", "thinking": "secret", "signature": "sig"},
		map[string]interface{}{"type": "fallback", "from": map[string]interface{}{"model": "a"}, "to": map[string]interface{}{"model": "b"}},
		map[string]interface{}{"type": "tool_use", "id": "tool-1", "name": "read", "input": map[string]interface{}{}},
		map[string]interface{}{"type": "text", "text": "safe visible output"},
		map[string]interface{}{"type": "connector_text", "text": " and narration"},
	}
	filtered := completionGuardRecoveryAssistantContent(
		content,
		"fallback_response_model_mismatch",
	)
	encoded, err := json.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte(`"type":"fallback"`),
		[]byte(`"type":"thinking"`),
		[]byte(`"type":"tool_use"`),
		[]byte("sig"),
	} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("malformed model state was replayed: %s", encoded)
		}
	}
	for _, required := range [][]byte{[]byte("safe visible output"), []byte("and narration")} {
		if !bytes.Contains(encoded, required) {
			t.Fatalf("visible recovery context missing %q: %s", required, encoded)
		}
	}
}

func TestCompletionGuardInvalidFallbackRecoveryFailsClosedOnUnmarshalableContent(t *testing.T) {
	content := []ClaudeContentBlock{{
		Type: "fallback",
		Extra: map[string]json.RawMessage{
			"from": json.RawMessage(`{"model":"a"}`),
			"to":   json.RawMessage(`{"model":`),
		},
	}}
	if recovered := completionGuardRecoveryAssistantContent(
		content,
		"invalid_fallback_block",
	); recovered != nil {
		t.Fatalf("unmarshalable fallback state was replayed: %#v", recovered)
	}
}

func TestCompletionGuardFallbackAnchorMismatchIsRecoverable(t *testing.T) {
	for _, reason := range []string{
		"fallback_request_model_mismatch",
		"fallback_response_model_mismatch",
	} {
		if !completionGuardRecoverableReason(reason, false) {
			t.Fatalf("reason %q was not recoverable", reason)
		}
	}
}

func TestCompletionGuardCapabilitySnapshotFeedsGuardWithoutReclassification(t *testing.T) {
	t.Setenv(completionGuardEnv, "strict")
	request := &ClaudeRequest{
		Model: "private-model-alias",
		Tools: []ClaudeTool{{
			Name: "code_execution",
			Type: "code_execution_20260120",
		}},
	}
	snapshot := claudeCapabilitySnapshot{
		ModelID: "private-model-alias",
		Protocol: claudeModelCapabilities{
			ProgrammaticToolCalling: claudeFeatureUnsupported,
			ToolSearch:              claudeFeatureUnsupported,
		},
	}
	plan := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(
		request,
		false,
		"private-model-alias",
		nil,
		snapshot,
	)
	if plan.Capabilities.ModelID != snapshot.ModelID ||
		!reflect.DeepEqual(plan.Capabilities.Protocol, snapshot.Protocol) {
		t.Fatalf("guard replaced request snapshot: %#v", plan.Capabilities)
	}
}

func TestNormalizeClaudeCapabilitySnapshotMismatchFailsClosed(t *testing.T) {
	snapshot := claudeCapabilitySnapshot{
		ModelID: "claude-opus-4.8",
		Protocol: claudeModelCapabilities{
			ToolSearch: claudeFeatureSupported,
		},
	}
	normalized := normalizeClaudeCapabilitySnapshot("claude-sonnet-5", snapshot)
	if normalized.ModelID != "claude-sonnet-5" {
		t.Fatalf("model=%q", normalized.ModelID)
	}
	if normalized.Protocol.ToolSearch != claudeFeatureUnknown ||
		normalized.ProviderReasoningKnown {
		t.Fatalf("mismatched snapshot was silently reused: %#v", normalized)
	}

	caseMismatch := normalizeClaudeCapabilitySnapshot("CLAUDE-OPUS-4.8", snapshot)
	if caseMismatch.Protocol.ToolSearch != claudeFeatureUnknown {
		t.Fatalf("case-mismatched model reused snapshot: %#v", caseMismatch)
	}
}
