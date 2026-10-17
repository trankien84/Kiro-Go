package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func completionGuardTestPlanWithTools(
	toolChoiceKind string,
	tools ...ClaudeTool,
) agentCompletionGuardPlan {
	req := &ClaudeRequest{Tools: append([]ClaudeTool(nil), tools...)}
	plan := buildAgentCompletionGuardPlan(req, false)
	plan.ToolChoiceKind = toolChoiceKind
	return plan
}

func completionGuardTestPlanWithMCP(
	toolChoiceKind string,
	serverName string,
	tools ...ClaudeTool,
) agentCompletionGuardPlan {
	req := &ClaudeRequest{
		Extra: map[string]json.RawMessage{
			"mcp_servers": json.RawMessage(fmt.Sprintf(
				`[{"type":"url","url":"https://mcp.example/sse","name":%q}]`,
				serverName,
			)),
		},
		Tools: append([]ClaudeTool{{
			Type: "mcp_toolset",
			Extra: map[string]json.RawMessage{
				"mcp_server_name": json.RawMessage(fmt.Sprintf("%q", serverName)),
			},
		}}, tools...),
	}
	plan := buildAgentCompletionGuardPlan(req, false)
	plan.ToolChoiceKind = toolChoiceKind
	return plan
}

func TestCompletionGuardPlanAndProtocolCoverage(
	t *testing.T,
) {
	t.Setenv(completionGuardEnv, "auto")
	t.Setenv(completionGuardAuditEnv, "off")

	plan := buildAgentCompletionGuardPlan(
		&ClaudeRequest{
			Tools: []ClaudeTool{
				{Name: "read"},
			},
		},
		true,
	)

	if !plan.Enabled || plan.ToolChoiceKind != "auto" {
		t.Fatalf("plan=%#v", plan)
	}

	none := buildAgentCompletionGuardPlan(
		&ClaudeRequest{
			Tools: []ClaudeTool{
				{Name: "read"},
			},
			ToolChoice: map[string]interface{}{
				"type": "none",
			},
		},
		false,
	)

	// tool_choice=none only disables tool use.
	// Protocol guard must still operate.
	if !none.Enabled || none.ToolChoiceKind != "none" {
		t.Fatalf("none plan=%#v", none)
	}

	// Semantic audit must not run when tool_choice=none.
	if none.AuditMode != "off" || none.AuditToolName != "" || none.AuditVerifierToolName != "" {
		t.Fatalf("none plan unexpectedly configured audit: %#v", none)
	}
}

func TestCompletionGuardClassifiesProseAndTools(
	t *testing.T,
) {
	autoPlan := agentCompletionGuardPlan{
		ToolChoiceKind: "auto",
	}

	prose := classifyCompletionGuardResponse(
		&ClaudeResponse{
			Model:      "claude-opus-4-8",
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{
					Type: "text",
					Text: "A complete explanatory answer.",
				},
			},
		},
		autoPlan,
	)

	if !prose.Accepted || prose.Kind != "prose" {
		t.Fatalf("prose=%#v", prose)
	}

	realTool := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{
				{
					Type:  "tool_use",
					ID:    "tool-1",
					Name:  "read",
					Input: map[string]interface{}{},
				},
			},
		},
		autoPlan,
	)

	if !realTool.Accepted || realTool.Kind != "real_tool" {
		t.Fatalf("realTool=%#v", realTool)
	}

	requiredPlan := agentCompletionGuardPlan{
		ToolChoiceKind: "any",
	}

	requiredProse := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{
					Type: "text",
					Text: "I recommend editing the file.",
				},
			},
		},
		requiredPlan,
	)

	if requiredProse.Accepted || requiredProse.Reason != "required_tool_not_called" {
		t.Fatalf("requiredProse=%#v", requiredProse)
	}

	truncated := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "max_tokens",
			Content: []ClaudeContentBlock{
				{
					Type: "text",
					Text: "Incomplete",
				},
			},
		},
		autoPlan,
	)

	if !truncated.Accepted || truncated.Kind != "prose" {
		t.Fatalf("terminal max_tokens prose=%#v", truncated)
	}
	missingStopReason := classifyCompletionGuardResponse(
		&ClaudeResponse{
			Content: []ClaudeContentBlock{
				{
					Type: "text",
					Text: "Partial response",
				},
			},
		},
		autoPlan,
	)

	if missingStopReason.Accepted ||
		missingStopReason.Reason != "missing_stop_reason" {

		t.Fatalf(
			"missingStopReason=%#v",
			missingStopReason,
		)
	}
}

func TestCompletionGuardAcceptsRefusalBeforeForcedToolChoice(t *testing.T) {
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "refusal",
			Content: []ClaudeContentBlock{{
				Type: "text",
				Text: "partial output must be discarded",
			}},
		},
		agentCompletionGuardPlan{ToolChoiceKind: "any"},
	)
	if !outcome.Accepted || outcome.Kind != "refusal" {
		t.Fatalf("refusal outcome=%#v", outcome)
	}
	if outcome.NaturalText != "" {
		t.Fatalf("refusal partial output was retained: %q", outcome.NaturalText)
	}
}

func TestCompletionGuardRequiresCompleteWireEnvelope(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		reason string
	}{
		{
			name: "id",
			body: `{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"text","text":"hello"}],
				"model":"claude-opus-5",
				"stop_reason":"end_turn",
				"stop_sequence":null,
				"usage":{"input_tokens":1,"output_tokens":1}
			}`,
			reason: "missing_response_id",
		},
		{
			name: "usage_output_tokens",
			body: `{
				"id":"msg-1",
				"type":"message",
				"role":"assistant",
				"content":[{"type":"text","text":"hello"}],
				"model":"claude-opus-5",
				"stop_reason":"end_turn",
				"stop_sequence":null,
				"usage":{"input_tokens":1}
			}`,
			reason: "missing_response_usage_output_tokens",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response ClaudeResponse
			if err := json.Unmarshal([]byte(test.body), &response); err != nil {
				t.Fatal(err)
			}
			outcome := classifyCompletionGuardResponse(
				&response,
				agentCompletionGuardPlan{ToolChoiceKind: "auto"},
			)
			if outcome.Accepted || outcome.Reason != test.reason {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}
}

func TestEmptyThinkingFieldSurvivesWireRoundTrip(t *testing.T) {
	response := KiroToClaudeResponse(
		"",
		"",
		true,
		nil,
		1,
		1,
		"claude-opus-5",
	)
	response.Content[0].Signature = "provider-signature"
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(`"thinking":""`)) {
		t.Fatalf("empty thinking field was omitted: %s", wire)
	}
	var decoded ClaudeResponse
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	outcome := classifyCompletionGuardResponse(
		&decoded,
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if outcome.Reason == "thinking_missing_content_field" {
		t.Fatalf("round-tripped thinking block rejected: %#v", outcome)
	}
}

func TestCompletionGuardSpecificTool(t *testing.T) {
	plan := agentCompletionGuardPlan{ToolChoiceKind: "specific", RequiredToolName: "read"}
	wrong := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{{
			Type: "tool_use", ID: "tool-1", Name: "write", Input: map[string]interface{}{},
		}},
	}, plan)
	if wrong.Accepted || wrong.Reason != "required_tool_not_called" {
		t.Fatalf("wrong=%#v", wrong)
	}
	right := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{{
			Type: "tool_use", ID: "tool-2", Name: "read", Input: map[string]interface{}{},
		}},
	}, plan)
	if !right.Accepted || right.Kind != "real_tool" {
		t.Fatalf("right=%#v", right)
	}
}

func TestNativeToolChoiceFallbackSafety(t *testing.T) {
	plan := agentCompletionGuardPlan{NativeToolChoiceMode: "auto", ToolChoiceKind: "any"}
	if ok, reason := shouldUseNativeToolChoice(plan, 0); ok || reason != "fallback_only" {
		t.Fatalf("first ok=%t reason=%q", ok, reason)
	}
	if ok, reason := shouldUseNativeToolChoice(plan, 1); ok || !strings.Contains(reason, "unverified") {
		t.Fatalf("auto retry ok=%t reason=%q", ok, reason)
	}
	plan.NativeToolChoiceMode = "always"
	if ok, reason := shouldUseNativeToolChoice(plan, 1); !ok || reason != "" {
		t.Fatalf("always retry ok=%t reason=%q", ok, reason)
	}
	plan.Thinking = true
	if ok, reason := shouldUseNativeToolChoice(plan, 1); ok || !strings.Contains(reason, "thinking") {
		t.Fatalf("thinking ok=%t reason=%q", ok, reason)
	}
}

func TestApplyNativeSpecificToolChoiceUsesSanitizedName(t *testing.T) {
	payload := &KiroPayload{ToolNameMap: map[string]string{"safe_read": "mcp__read"}}
	plan := agentCompletionGuardPlan{ToolChoiceKind: "specific", RequiredToolName: "mcp__read"}
	if !applyNativeToolChoice(payload, plan) {
		t.Fatal("not applied")
	}
	data, _ := json.Marshal(payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolChoice)
	if !strings.Contains(string(data), "safe_read") {
		t.Fatalf("choice=%s", data)
	}
}

func TestCompletionGuardRecorderEnforcesLimitAndReleasesStorage(t *testing.T) {
	t.Setenv(completionGuardMaxBufferedEnv, "1048576")
	recorder := newCompletionGuardRecorder()
	payload := bytes.Repeat([]byte("x"), 1024*1024)
	if _, err := recorder.Write(payload); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if recorder.body.Len() != len(payload) {
		t.Fatalf("body len=%d, want %d", recorder.body.Len(), len(payload))
	}
	if _, err := recorder.Write([]byte("overflow")); err == nil || !errors.Is(err, errCompletionGuardBufferLimit) {
		t.Fatalf("overflow error=%v", err)
	}
	recorder.release()
	if recorder.body.Len() != 0 || recorder.body.Cap() != 0 {
		t.Fatalf("buffer retained after release: len=%d cap=%d", recorder.body.Len(), recorder.body.Cap())
	}
	if recorder.header != nil {
		t.Fatal("header reference retained after release")
	}
}

func TestCompletionGuardRecorderTakeResultTransfersAndReleases(t *testing.T) {
	recorder := newCompletionGuardRecorder()
	recorder.Header().Set("X-Test", "value")
	recorder.WriteHeader(http.StatusCreated)
	if _, err := recorder.Write([]byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := &recorder.body.Bytes()[0]

	status, body, err := recorder.takeResult()
	if err != nil {
		t.Fatalf("take result: %v", err)
	}
	if status != http.StatusCreated || string(body) != "payload" {
		t.Fatalf("status=%d body=%q", status, body)
	}
	if &body[0] != before {
		t.Fatal("takeResult copied the recorder buffer instead of transferring ownership")
	}
	if recorder.body.Len() != 0 || recorder.body.Cap() != 0 {
		t.Fatalf("buffer retained after transfer: len=%d cap=%d", recorder.body.Len(), recorder.body.Cap())
	}
	if recorder.header != nil || recorder.maxBytes != 0 || recorder.wroteHeader {
		t.Fatalf("recorder retained state after transfer: %#v", recorder)
	}
}

func TestCompletionGuardRecorderFirstWriteHeaderWins(t *testing.T) {
	recorder := newCompletionGuardRecorder()
	recorder.WriteHeader(http.StatusAccepted)
	recorder.WriteHeader(http.StatusInternalServerError)
	if recorder.status != http.StatusAccepted {
		t.Fatalf("status=%d", recorder.status)
	}
}

func TestClaudeNonStreamCollectorSharesRecorderBudget(t *testing.T) {
	recorder := newCompletionGuardRecorder()
	recorder.maxBytes = 10
	recorder.budget = newCompletionGuardRetainedBudget(10)
	collector := newClaudeNonStreamBlockCollector(
		"claude-opus-5",
		false,
		recorder.completionGuardBudget(),
	)
	collector.AppendText("123456", false)
	if err := collector.Err(); err != nil {
		t.Fatalf("unexpected collector error: %v", err)
	}
	if _, err := recorder.Write([]byte("12345")); !errors.Is(err, errCompletionGuardBufferLimit) {
		t.Fatalf("shared budget did not reject combined retention: %v", err)
	}
	collector.Release()
	recorder.release()
}

func TestReplayCompletionGuardSSEOrder(t *testing.T) {
	buffer := &completionGuardFlushBuffer{header: make(http.Header)}
	sse, ok := newCompletionGuardSSE(buffer)
	if !ok {
		t.Fatal("no flusher")
	}
	resp := &ClaudeResponse{ID: "m", Model: "claude-opus-5", StopReason: "tool_use", Content: []ClaudeContentBlock{
		{Type: "thinking", Thinking: "summary", Signature: "sig"},
		{Type: "redacted_thinking", Data: "opaque"},
		{Type: "text", Text: "answer"},
		{Type: "tool_use", ID: "t", Name: "read", Input: map[string]interface{}{"path": "x"}},
	}}
	replayCompletionGuardSSE(sse, resp)
	text := buffer.String()
	for _, value := range []string{"thinking_delta", "signature_delta", "redacted_thinking", "text_delta", "input_json_delta", "message_stop"} {
		if !strings.Contains(text, value) {
			t.Fatalf("missing %s", value)
		}
	}
}

func TestCompletionGuardAutoCandidate(t *testing.T) {
	plan := agentCompletionGuardPlan{
		ToolChoiceKind: "auto",
		AuditMode:      "observe",
	}

	prose := &ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{
				Type: "text",
				Text: "A complete explanatory answer.",
			},
		},
	}

	proseOutcome := classifyCompletionGuardResponse(prose, plan)

	if !isCompletionGuardAutoCandidate(
		prose,
		plan,
		proseOutcome,
	) {
		t.Fatalf("prose outcome=%#v", proseOutcome)
	}

	truncated := &ClaudeResponse{
		StopReason: "max_tokens",
		Content: []ClaudeContentBlock{
			{
				Type: "text",
				Text: "An incomplete answer",
			},
		},
	}

	truncatedOutcome := classifyCompletionGuardResponse(truncated, plan)

	if !isCompletionGuardAutoCandidate(
		truncated,
		plan,
		truncatedOutcome,
	) {
		t.Fatalf(
			"terminal max_tokens prose must remain a candidate: %#v",
			truncatedOutcome,
		)
	}

	realTool := &ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{
			{
				Type:  "tool_use",
				ID:    "tool-1",
				Name:  "read",
				Input: map[string]interface{}{},
			},
		},
	}

	realToolOutcome := classifyCompletionGuardResponse(
		realTool,
		plan,
	)

	if isCompletionGuardAutoCandidate(realTool, plan, realToolOutcome) {
		t.Fatalf(
			"real tool must not be candidate: %#v",
			realToolOutcome,
		)
	}
}

func TestParseCompletionGuardAuditDecision(t *testing.T) {
	baseTool := ClaudeContentBlock{
		Type: "tool_use",
		ID:   "tool-audit",
		Name: "audit",
		Input: map[string]interface{}{
			"status": "needs_action",
			"missing_actions": []interface{}{
				"edit proxy/kiro.go",
				"run go test ./...",
			},
		},
	}
	resp := &ClaudeResponse{
		StopReason: "tool_use",
		Content:    []ClaudeContentBlock{baseTool},
	}

	decision, ok := parseCompletionGuardAuditDecision(resp, "audit")
	if !ok || !decision.needsAction() || len(decision.MissingActions) != 2 {
		t.Fatalf("decision=%#v ok=%t", decision, ok)
	}

	t.Run("allows harmless text and thinking around audit tool", func(t *testing.T) {
		withContext := &ClaudeResponse{
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{
				{Type: "thinking", Thinking: "checking evidence", Signature: "sig"},
				{Type: "text", Text: "I will record the audit result."},
				baseTool,
				{Type: "redacted_thinking", Data: "opaque"},
			},
		}
		parsed, diagnostics, ok := parseCompletionGuardAuditDecisionDetailed(
			withContext,
			"audit",
		)
		if !ok || !parsed.needsAction() {
			t.Fatalf("parsed=%#v diagnostics=%s", parsed, diagnostics.summary())
		}
		if diagnostics.TextBlocks != 1 || diagnostics.ThinkingBlocks != 2 ||
			diagnostics.AuditToolMatches != 1 || diagnostics.Reason != "ok" {
			t.Fatalf("diagnostics=%#v", diagnostics)
		}
	})

	for _, status := range []string{"complete", "waiting_for_user", "uncertain"} {
		t.Run("valid "+status, func(t *testing.T) {
			response := &ClaudeResponse{
				StopReason: "tool_use",
				Content: []ClaudeContentBlock{{
					Type: "tool_use",
					ID:   "tool-audit",
					Name: "audit",
					Input: map[string]interface{}{
						"status":          status,
						"missing_actions": []interface{}{},
					},
				}},
			}
			parsed, ok := parseCompletionGuardAuditDecision(response, "audit")
			if !ok || parsed.Status != status || parsed.needsAction() {
				t.Fatalf("parsed=%#v ok=%t", parsed, ok)
			}
		})
	}

	for name, invalid := range map[string]*ClaudeResponse{
		"wrong stop reason": {
			StopReason: "end_turn",
			Content:    []ClaudeContentBlock{baseTool},
		},
		"missing tool id": {
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{{
				Type: "tool_use",
				Name: "audit",
				Input: map[string]interface{}{
					"status":          "complete",
					"missing_actions": []interface{}{},
				},
			}},
		},
		"extra input field": {
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{{
				Type: "tool_use",
				ID:   "tool-audit",
				Name: "audit",
				Input: map[string]interface{}{
					"status":          "complete",
					"missing_actions": []interface{}{},
					"extra":           true,
				},
			}},
		},
		"needs action without missing actions": {
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{{
				Type: "tool_use", ID: "tool-audit", Name: "audit",
				Input: map[string]interface{}{
					"status":          "needs_action",
					"missing_actions": []interface{}{},
				},
			}},
		},
		"non action status with missing actions": {
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{{
				Type: "tool_use", ID: "tool-audit", Name: "audit",
				Input: map[string]interface{}{
					"status":          "waiting_for_user",
					"missing_actions": []interface{}{"ask for approval"},
				},
			}},
		},
		"invalid status": {
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{{
				Type: "tool_use", ID: "tool-audit", Name: "audit",
				Input: map[string]interface{}{
					"status":          "maybe",
					"missing_actions": []interface{}{},
				},
			}},
		},
		"unexpected tool": {
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{
				baseTool,
				{Type: "tool_use", ID: "tool-other", Name: "read", Input: map[string]interface{}{}},
			},
		},
		"duplicate audit tool": {
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{
				baseTool,
				{Type: "tool_use", ID: "tool-audit-2", Name: "audit", Input: map[string]interface{}{
					"status": "complete", "missing_actions": []interface{}{},
				}},
			},
		},
		"unsupported content block": {
			StopReason: "tool_use",
			Content:    []ClaudeContentBlock{baseTool, {Type: "unknown_future_block"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseCompletionGuardAuditDecision(invalid, "audit"); ok {
				t.Fatalf("invalid audit response was accepted: %#v", invalid)
			}
		})
	}
}

func TestParseCompletionGuardAuditVerificationDecision(t *testing.T) {
	for _, decision := range []string{"continue_autonomously", "do_not_continue"} {
		t.Run(decision, func(t *testing.T) {
			response := &ClaudeResponse{
				StopReason: "tool_use",
				Content: []ClaudeContentBlock{{
					Type:  "tool_use",
					ID:    "verify-1",
					Name:  "verify",
					Input: map[string]interface{}{"decision": decision},
				}},
			}
			parsed, diagnostics, ok := parseCompletionGuardAuditVerificationDecisionDetailed(
				response,
				"verify",
			)
			if !ok || parsed.Decision != decision || diagnostics.Reason != "ok" {
				t.Fatalf("parsed=%#v diagnostics=%#v ok=%t", parsed, diagnostics, ok)
			}
		})
	}
	invalid := &ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{{
			Type: "tool_use", ID: "verify-1", Name: "verify",
			Input: map[string]interface{}{"decision": "continue_autonomously", "extra": true},
		}},
	}
	if _, _, ok := parseCompletionGuardAuditVerificationDecisionDetailed(invalid, "verify"); ok {
		t.Fatal("verification with extra input field was accepted")
	}
}

func TestCompletionGuardAuditParseDiagnostics(t *testing.T) {
	resp := &ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "checking"},
			{
				Type:  "tool_use",
				ID:    "tool-other",
				Name:  "read",
				Input: map[string]interface{}{},
			},
		},
	}
	_, diagnostics, ok := parseCompletionGuardAuditDecisionDetailed(resp, "audit")
	if ok {
		t.Fatal("unexpected tool response was accepted")
	}
	if diagnostics.Reason != "unexpected_tool_call" ||
		diagnostics.OtherToolBlocks != 1 ||
		diagnostics.TextBlocks != 1 ||
		len(diagnostics.ToolNames) != 1 || diagnostics.ToolNames[0] != "read" {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func TestCompletionGuardAuditCorrectiveRetryRequest(t *testing.T) {
	context := completionGuardAuditContext{
		CurrentUserRequest: "Fix the file and run tests.",
	}
	first := buildCompletionGuardAuditRequestForAttempt(
		ClaudeRequest{Model: "claude-opus-5"},
		context,
		"I found the issue.",
		"audit",
		0,
	)
	retry := buildCompletionGuardAuditRequestForAttempt(
		ClaudeRequest{Model: "claude-opus-5"},
		context,
		"I found the issue.",
		"audit",
		1,
	)
	firstSystem, ok := first.System.(string)
	if !ok {
		t.Fatalf("first system type=%T", first.System)
	}
	retrySystem, ok := retry.System.(string)
	if !ok {
		t.Fatalf("retry system type=%T", retry.System)
	}
	if strings.Contains(firstSystem, "previous audit attempt") {
		t.Fatalf("first attempt unexpectedly contains corrective prompt: %q", firstSystem)
	}
	for _, text := range []string{
		"previous audit attempt",
		"exactly one call",
		"Do not emit explanatory text",
	} {
		if !strings.Contains(retrySystem, text) {
			t.Fatalf("retry system prompt missing %q: %q", text, retrySystem)
		}
	}
	if len(retry.Tools) != 1 || retry.Tools[0].Name != "audit" || retry.ToolChoice != nil {
		t.Fatalf("retry request changed audit-tool isolation: %#v", retry)
	}
}

func TestCompletionGuardAuditRequestHasOnlyAuditTool(
	t *testing.T,
) {
	original := ClaudeRequest{
		Model: "claude-opus-5",
		Tools: []ClaudeTool{
			{Name: "read"},
			{Name: "edit"},
			{Name: "bash"},
		},
		Thinking: &ClaudeThinkingConfig{
			Type: "adaptive",
		},
	}

	auditReq := buildCompletionGuardAuditRequest(
		original,
		completionGuardAuditContext{
			CurrentUserRequest: "Fix the file and run tests.",
			Tools: []completionGuardObservedTool{
				{
					ToolUseID: "read-1",
					Name:      "read",
					Input:     `{"path":"proxy/kiro.go"}`,
					HasResult: true,
					Result:    "file contents",
				},
			},
		},
		"I recommend changing the condition.",
		"audit",
	)

	if len(auditReq.Tools) != 1 ||
		auditReq.Tools[0].Name != "audit" {
		t.Fatalf("tools=%#v", auditReq.Tools)
	}

	if auditReq.Thinking != nil ||
		auditReq.OutputConfig != nil ||
		auditReq.ReasoningEffort != "" {
		t.Fatalf("audit must not use thinking: %#v", auditReq)
	}

	if auditReq.ToolChoice != nil {
		t.Fatalf("audit request must not force tool choice directly: %#v", auditReq.ToolChoice)
	}

	for _, forbidden := range []string{
		"read",
		"edit",
		"bash",
	} {
		if auditReq.Tools[0].Name == forbidden {
			t.Fatalf("real tool leaked into audit: %s", forbidden)
		}
	}
}

func TestCompletionGuardAuditDefaultsOff(t *testing.T) {
	t.Setenv(completionGuardAuditEnv, "")

	if got := completionGuardAuditModeFromEnv(); got != "off" {
		t.Fatalf("audit mode=%q, want off", got)
	}
}

func TestCompletionGuardAuditDecisionPolicy(t *testing.T) {
	enforce := agentCompletionGuardPlan{AuditMode: "enforce"}
	observe := agentCompletionGuardPlan{AuditMode: "observe"}
	needsAction := completionGuardAuditDecision{
		Status:         "needs_action",
		MissingActions: []string{"run tests"},
	}

	if completionGuardAuditRequiresVerification(observe, true, needsAction) {
		t.Fatal("observe mode must never trigger autonomous verification")
	}
	if completionGuardAuditRequiresVerification(enforce, false, needsAction) {
		t.Fatal("invalid audit must never trigger autonomous verification")
	}
	for _, status := range []string{"complete", "waiting_for_user", "uncertain"} {
		if completionGuardAuditRequiresVerification(
			enforce,
			true,
			completionGuardAuditDecision{Status: status},
		) {
			t.Fatalf("status %q unexpectedly triggered verification", status)
		}
	}
	if !completionGuardAuditRequiresVerification(enforce, true, needsAction) {
		t.Fatal("enforced needs_action must trigger verification")
	}

	continueDecision := completionGuardAuditVerificationDecision{Decision: "continue_autonomously"}
	stopDecision := completionGuardAuditVerificationDecision{Decision: "do_not_continue"}
	if completionGuardAuditContinuationApproved(enforce, true, needsAction, false, continueDecision) {
		t.Fatal("invalid verification must fail open")
	}
	if completionGuardAuditContinuationApproved(enforce, true, needsAction, true, stopDecision) {
		t.Fatal("do_not_continue must not authorize continuation")
	}
	if !completionGuardAuditContinuationApproved(enforce, true, needsAction, true, continueDecision) {
		t.Fatal("verified autonomous work should authorize continuation")
	}
}

func TestCompletionGuardAuditVerificationRequestHasOnlyVerificationTool(t *testing.T) {
	original := ClaudeRequest{
		Model:    "claude-opus-5",
		Tools:    []ClaudeTool{{Name: "read"}, {Name: "edit"}},
		Thinking: &ClaudeThinkingConfig{Type: "adaptive"},
	}
	context := completionGuardAuditContext{
		CurrentUserRequest: "(b)",
		RecentConversation: []completionGuardAuditTurn{{
			Role: "assistant",
			Text: "Choose (a) update the manifest or (b) find which side is stale.",
		}},
	}
	req := buildCompletionGuardAuditVerificationRequestForAttempt(
		original,
		context,
		"Understood. I will find which side is stale.",
		[]string{"inspect the manifest and generated artifact"},
		"verify",
		1,
	)
	if len(req.Tools) != 1 || req.Tools[0].Name != "verify" || req.ToolChoice != nil {
		t.Fatalf("verification request leaked client tools: %#v", req)
	}
	if req.Thinking != nil || req.OutputConfig != nil || req.ReasoningEffort != "" {
		t.Fatalf("verification request must not use client reasoning fields: %#v", req)
	}
	system, ok := req.System.(string)
	if !ok || !strings.Contains(system, "previous verification attempt") {
		t.Fatalf("corrective verification prompt missing: %#v", req.System)
	}
	payload := fmt.Sprint(req.Messages[0].Content)
	for _, want := range []string{"current_user_request", "recent_conversation", "primary_missing_actions"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("verification payload missing %q: %s", want, payload)
		}
	}
}

func TestCompletionGuardAuditContextAfterToolResult(t *testing.T) {
	req := ClaudeRequest{
		Messages: []ClaudeMessage{
			{
				Role:    "user",
				Content: "Fix proxy/kiro.go and run tests.",
			},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type": "tool_use",
						"id":   "tool-1",
						"name": "read",
						"input": map[string]interface{}{
							"path": "proxy/kiro.go",
						},
					},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool-1",
						"content":     "file contents",
					},
				},
			},
		},
	}

	context := completionGuardAuditContextFromRequest(req)

	if context.CurrentUserRequest != "Fix proxy/kiro.go and run tests." {
		t.Fatalf("user request=%q", context.CurrentUserRequest)
	}

	if len(context.Tools) != 1 {
		t.Fatalf("tools=%#v", context.Tools)
	}

	tool := context.Tools[0]
	if tool.ToolUseID != "tool-1" ||
		tool.Name != "read" ||
		!tool.HasResult ||
		tool.IsError ||
		!strings.Contains(tool.Input, "proxy/kiro.go") ||
		tool.Result != "file contents" {
		t.Fatalf("tool=%#v", tool)
	}
}

func TestCompletionGuardAuditContextPreservesRepeatedTools(
	t *testing.T,
) {
	req := ClaudeRequest{
		Messages: []ClaudeMessage{
			{
				Role:    "user",
				Content: "Edit both files and run tests.",
			},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type": "tool_use",
						"id":   "edit-1",
						"name": "edit",
						"input": map[string]interface{}{
							"path": "a.go",
						},
					},
					map[string]interface{}{
						"type": "tool_use",
						"id":   "edit-2",
						"name": "edit",
						"input": map[string]interface{}{
							"path": "b.go",
						},
					},
					map[string]interface{}{
						"type": "tool_use",
						"id":   "test-1",
						"name": "execute",
						"input": map[string]interface{}{
							"command": "go test ./...",
						},
					},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "edit-1",
						"content":     "updated a.go",
					},
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "edit-2",
						"is_error":    true,
						"content":     "permission denied",
					},
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "test-1",
						"content":     "ok",
					},
				},
			},
		},
	}

	context := completionGuardAuditContextFromRequest(req)
	if len(context.Tools) != 3 {
		t.Fatalf("tools=%d, want 3: %#v", len(context.Tools), context.Tools)
	}

	if context.Tools[0].Name != "edit" || context.Tools[1].Name != "edit" {
		t.Fatalf("repeated edit calls were lost: %#v", context.Tools)
	}
	if !context.Tools[0].HasResult || context.Tools[0].IsError {
		t.Fatalf("first edit result=%#v", context.Tools[0])
	}
	if !context.Tools[1].HasResult || !context.Tools[1].IsError {
		t.Fatalf("second edit result=%#v", context.Tools[1])
	}
	if !strings.Contains(context.Tools[2].Input, "go test ./...") {
		t.Fatalf("test command was lost: %#v", context.Tools[2])
	}

	resultCount, errorCount := completionGuardAuditToolStats(context.Tools)
	if resultCount != 3 || errorCount != 1 {
		t.Fatalf("resultCount=%d errorCount=%d", resultCount, errorCount)
	}
}

func TestCompletionGuardAuditContextPreservesReferentialAntecedent(t *testing.T) {
	req := ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "Review the digest mismatch and decide what to do."},
		{Role: "assistant", Content: "Two choices: (a) rewrite the manifest to current bytes; (b) inspect which side became stale first. Which do you want?"},
		{Role: "user", Content: "(b)"},
	}}

	context := completionGuardAuditContextFromRequest(req)
	if context.CurrentUserRequest != "(b)" {
		t.Fatalf("current request=%q", context.CurrentUserRequest)
	}
	if len(context.RecentConversation) != 2 {
		t.Fatalf("recent conversation=%#v", context.RecentConversation)
	}
	if context.RecentConversation[0].Role != "user" ||
		!strings.Contains(context.RecentConversation[0].Text, "digest mismatch") ||
		context.RecentConversation[1].Role != "assistant" ||
		!strings.Contains(context.RecentConversation[1].Text, "(b) inspect") {
		t.Fatalf("antecedent lost: %#v", context.RecentConversation)
	}
}

func TestCompletionGuardAuditContextIncludesAntecedentToolEvidence(t *testing.T) {
	req := ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "Inspect the repository and offer two fixes."},
		{Role: "assistant", Content: []interface{}{map[string]interface{}{
			"type": "tool_use", "id": "read-1", "name": "read",
			"input": map[string]interface{}{"path": "manifest.yaml"},
		}}},
		{Role: "user", Content: []interface{}{map[string]interface{}{
			"type": "tool_result", "tool_use_id": "read-1", "content": "manifest contents",
		}}},
		{Role: "assistant", Content: "Choose (a) rewrite now or (b) verify which artifact is stale first."},
		{Role: "user", Content: "(b)"},
	}}

	context := completionGuardAuditContextFromRequest(req)
	if len(context.Tools) != 1 || context.Tools[0].ToolUseID != "read-1" || !context.Tools[0].HasResult {
		t.Fatalf("antecedent tool evidence lost: %#v", context.Tools)
	}
}

func TestCompletionGuardAuditCurrentRequestIsBounded(t *testing.T) {
	text := strings.Repeat("x", completionGuardAuditMaxCurrentUserRunes+2048)
	context := completionGuardAuditContextFromRequest(ClaudeRequest{Messages: []ClaudeMessage{{
		Role: "user", Content: text,
	}}})
	if !context.CurrentUserRequestTruncated {
		t.Fatal("oversized current request was not marked truncated")
	}
	if got := len([]rune(context.CurrentUserRequest)); got != completionGuardAuditMaxCurrentUserRunes {
		t.Fatalf("bounded request runes=%d", got)
	}
	if !strings.Contains(context.CurrentUserRequest, "…") {
		t.Fatal("bounded request does not carry a truncation marker")
	}
}

func TestCompletionGuardAuditContextIsBounded(t *testing.T) {
	messages := []ClaudeMessage{{Role: "user", Content: strings.Repeat("u", completionGuardAuditMaxCurrentUserRunes+1024)}}
	for index := 0; index < completionGuardAuditMaxRecentConversationTurns+4; index++ {
		messages = append(messages,
			ClaudeMessage{Role: "assistant", Content: strings.Repeat("a", 4096)},
			ClaudeMessage{Role: "user", Content: fmt.Sprintf("follow-up-%d", index)},
		)
	}
	// The final short request is the current request. Add more than the tool cap
	// in the retained conversational window so first/last evidence is preserved.
	messages = append(messages, ClaudeMessage{Role: "assistant", Content: "Choose the next action."})
	for index := 0; index < completionGuardAuditMaxObservedTools+8; index++ {
		id := fmt.Sprintf("tool-%03d", index)
		messages = append(messages,
			ClaudeMessage{Role: "assistant", Content: []interface{}{map[string]interface{}{
				"type": "tool_use", "id": id, "name": "execute",
				"input": map[string]interface{}{"command": strings.Repeat("x", 8192)},
			}}},
			ClaudeMessage{Role: "user", Content: []interface{}{map[string]interface{}{
				"type": "tool_result", "tool_use_id": id, "content": strings.Repeat("r", 8192),
			}}},
		)
	}
	messages = append(messages, ClaudeMessage{Role: "user", Content: "continue"})

	context := completionGuardAuditContextFromRequest(ClaudeRequest{Messages: messages})
	if context.CurrentUserRequest != "continue" {
		t.Fatalf("current request=%q", context.CurrentUserRequest)
	}
	if len(context.RecentConversation) > completionGuardAuditMaxRecentConversationTurns {
		t.Fatalf("recent turns=%d", len(context.RecentConversation))
	}
	conversationRunes := 0
	for _, turn := range context.RecentConversation {
		conversationRunes += len([]rune(turn.Text))
	}
	if conversationRunes > completionGuardAuditMaxRecentConversationRunes {
		t.Fatalf("conversation runes=%d", conversationRunes)
	}
	if len(context.Tools) > completionGuardAuditMaxObservedTools {
		t.Fatalf("tools=%d", len(context.Tools))
	}
	toolRunes := 0
	for _, tool := range context.Tools {
		toolRunes += len([]rune(tool.Input)) + len([]rune(tool.Result))
	}
	if toolRunes > completionGuardAuditMaxToolEvidenceRunes {
		t.Fatalf("tool evidence runes=%d", toolRunes)
	}
	if !context.evidenceTruncated() {
		t.Fatal("large context did not advertise truncation")
	}
}

func TestCompletionGuardRetryRequestDropsTruncatedText(
	t *testing.T,
) {
	current := ClaudeRequest{
		Messages: []ClaudeMessage{
			{
				Role:    "user",
				Content: "Explain the issue.",
			},
		},
	}

	resp := &ClaudeResponse{
		StopReason: "max_tokens",
		Content: []ClaudeContentBlock{
			{
				Type: "text",
				Text: "Partial answer",
			},
		},
	}

	next, err := completionGuardRetryRequest(
		current,
		resp,
		agentCompletionGuardPlan{
			ToolChoiceKind: "auto",
		},
		"truncated_response",
	)
	if err != nil {
		t.Fatal(err)
	}

	// Only the original user request and the recovery user message.
	// Do not add "Partial answer" with the assistant role.
	if len(next.Messages) != 2 {
		t.Fatalf("messages=%d, want 2", len(next.Messages))
	}

	if next.Messages[1].Role != "user" {
		t.Fatalf("last role=%q, want user", next.Messages[1].Role)
	}

	if strings.Contains(
		fmt.Sprint(next.Messages[1].Content),
		"Continue from the previous content",
	) {
		t.Fatal("truncated recovery must rewrite from the beginning")
	}
}

func TestCompletionGuardPauseTurnPreservesContent(t *testing.T) {
	current := ClaudeRequest{
		Messages: []ClaudeMessage{
			{
				Role:    "user",
				Content: "Search for the result.",
			},
		},
	}

	resp := &ClaudeResponse{
		StopReason: "pause_turn",
		Content: []ClaudeContentBlock{
			{
				Type: "text",
				Text: "Searching...",
			},
			{
				Type: "tool_use",
				ID:   "tool-1",
				Name: "web_search",
			},
		},
	}

	next, err := completionGuardPauseTurnRequest(current, resp)
	if err != nil {
		t.Fatal(err)
	}

	if len(next.Messages) != 2 {
		t.Fatalf("messages=%d, want 2", len(next.Messages))
	}

	content, ok := next.Messages[1].Content.([]ClaudeContentBlock)

	if !ok {
		t.Fatalf("assistant content type=%T", next.Messages[1].Content)
	}

	if len(content) != 2 {
		t.Fatalf("content blocks=%d, want 2", len(content))
	}
}

func TestCompletionGuardContinuationCandidateReachesKiroHistory(t *testing.T) {
	original := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{
				Role:    "user",
				Content: "Fix the file.",
			},
		},
		Tools: []ClaudeTool{
			{
				Name:        "edit",
				Description: "Edit a file",
				InputSchema: map[string]interface{}{
					"type": "object",
				},
			},
		},
	}

	candidate := ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{
				Type: "text",
				Text: "I found the required change.",
			},
		},
	}

	next, err := buildCompletionGuardContinuationRequest(original, candidate,
		[]string{
			"edit the requested file",
		},
	)

	if err != nil {
		t.Fatal(err)
	}

	payload := ClaudeToKiro(&next, false)

	found := false

	for _, message := range payload.ConversationState.History {

		if message.AssistantResponseMessage == nil {
			continue
		}

		if strings.Contains(message.AssistantResponseMessage.Content, "I found the required change.") {
			found = true
			break
		}
	}

	if !found {
		t.Fatal("candidate assistant text was lost during ClaudeToKiro translation")
	}
}

func TestCompletionGuardEnforcePlanConfiguresAuditAndVerifierTools(t *testing.T) {
	t.Setenv(completionGuardEnv, "strict")
	t.Setenv(completionGuardAuditEnv, "enforce")

	plan := buildAgentCompletionGuardPlan(
		&ClaudeRequest{
			Model:    "claude-opus-5",
			Messages: []ClaudeMessage{{Role: "user", Content: "Inspect and fix the file."}},
			Tools: []ClaudeTool{
				{Name: "read"},
				{Name: completionGuardAuditToolBase},
				{Name: completionGuardVerifyToolBase},
			},
		},
		false,
	)
	if plan.AuditMode != "enforce" {
		t.Fatalf("audit mode=%q", plan.AuditMode)
	}
	if plan.AuditToolName == "" || plan.AuditVerifierToolName == "" {
		t.Fatalf("audit tools not configured: %#v", plan)
	}
	if plan.AuditToolName == plan.AuditVerifierToolName {
		t.Fatalf("audit and verifier tool names collided: %#v", plan)
	}
	for _, clientTool := range []string{
		"read",
		completionGuardAuditToolBase,
		completionGuardVerifyToolBase,
	} {
		if plan.AuditToolName == clientTool || plan.AuditVerifierToolName == clientTool {
			t.Fatalf("internal audit tool collided with client tool %q: %#v", clientTool, plan)
		}
	}
}

func TestCompletionGuardProtocolPlanWithoutTools(t *testing.T) {
	t.Setenv(completionGuardEnv, "strict")

	plan := buildAgentCompletionGuardPlan(
		&ClaudeRequest{
			Model: "claude-opus-5",
			Messages: []ClaudeMessage{
				{
					Role:    "user",
					Content: "Explain this.",
				},
			},
		},
		false,
	)

	if !plan.Enabled {
		t.Fatal("protocol guard must remain enabled for requests without tools")
	}

	if plan.AuditToolName != "" || plan.AuditVerifierToolName != "" {
		t.Fatalf("no-tool request unexpectedly configured audit tools: %#v", plan)
	}
}

func TestCompletionGuardObservePlan(t *testing.T) {
	t.Setenv(completionGuardEnv, "observe")

	t.Setenv(completionGuardAuditEnv, "enforce")

	plan := buildAgentCompletionGuardPlan(
		&ClaudeRequest{
			Model: "claude-opus-5",

			Messages: []ClaudeMessage{
				{
					Role:    "user",
					Content: "Explain this.",
				},
			},
		},
		false,
	)

	if !plan.Enabled {
		t.Fatal("observe plan must run the guard")
	}

	if !plan.ObserveOnly {
		t.Fatal("observe plan must be observe-only")
	}

	if plan.MaxRetries != 0 {
		t.Fatalf("observe retries=%d, want 0", plan.MaxRetries)
	}

	if plan.AuditMode != "off" {
		t.Fatalf("observe audit mode=%q, want off", plan.AuditMode)
	}

	if plan.AuditToolName != "" || plan.AuditVerifierToolName != "" {
		t.Fatalf("observe audit tools unexpectedly configured: %#v", plan)
	}
}

func TestCompletionGuardDeliveryModeFromEnv(t *testing.T) {
	tests := []struct {
		value string
		want  completionGuardDeliveryMode
	}{
		{value: "", want: completionGuardDeliveryAuto},
		{value: "auto", want: completionGuardDeliveryAuto},
		{value: "buffered", want: completionGuardDeliveryBuffered},
		{value: "strict-buffered", want: completionGuardDeliveryBuffered},
		{value: "deferred-stream", want: completionGuardDeliveryDeferredStream},
		{value: "streaming", want: completionGuardDeliveryDeferredStream},
	}

	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(completionGuardDeliveryEnv, test.value)

			if got := completionGuardDeliveryModeFromEnv(); got != test.want {
				t.Fatalf(
					"delivery mode=%q, want %q",
					got,
					test.want,
				)
			}
		})
	}
}

func TestCompletionGuardPlanIncludesDeliveryMode(t *testing.T) {
	t.Setenv(completionGuardEnv, "strict")
	t.Setenv(completionGuardDeliveryEnv, "buffered")

	plan := buildAgentCompletionGuardPlan(
		&ClaudeRequest{
			Model:  "claude-opus-5",
			Stream: true,
		},
		false,
	)

	if plan.DeliveryMode != completionGuardDeliveryBuffered {
		t.Fatalf(
			"delivery mode=%q, want buffered",
			plan.DeliveryMode,
		)
	}
}

type completionGuardFlushBuffer struct {
	bytes.Buffer
	header http.Header
}

func (b *completionGuardFlushBuffer) Header() http.Header { return b.header }
func (b *completionGuardFlushBuffer) WriteHeader(int)     {}
func (b *completionGuardFlushBuffer) Flush()              {}

func TestCompletionGuardStreamingWriterDefersOnlyTerminalEvents(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"stop_sequence\",\"stop_sequence\":\"DONE\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	for _, frame := range frames {
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		writer.Flush()
	}
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, "text_delta") || !strings.Contains(body, "hello") {
		t.Fatalf("streamed content missing: %s", body)
	}
	if strings.Contains(body, "message_delta") || strings.Contains(body, "message_stop") {
		t.Fatalf("terminal events leaked before finalization: %s", body)
	}

	candidate := writer.response()
	if candidate.StopReason != "stop_sequence" ||
		candidate.StopSequence == nil ||
		*candidate.StopSequence != "DONE" ||
		len(candidate.Content) != 1 ||
		candidate.Content[0].Text != "hello" {
		t.Fatalf("candidate=%#v", candidate)
	}
	if !writer.protocolComplete() {
		t.Fatalf(
			"terminal sequence was not captured: reason=%s error=%v",
			writer.protocolReason(),
			writer.protocolErr,
		)
	}
}

func TestEmitCompletionGuardTerminalClosesDeferredStream(t *testing.T) {
	recorder := &completionGuardFlushBuffer{header: make(http.Header)}
	sse, ok := newCompletionGuardSSE(recorder)
	if !ok {
		t.Fatal("no flusher")
	}
	stopSequence := "done"
	if err := emitCompletionGuardTerminal(
		sse,
		"tool_use",
		&stopSequence,
		ClaudeUsage{OutputTokens: 9},
	); err != nil {
		t.Fatal(err)
	}
	body := recorder.String()
	if !strings.Contains(body, `"stop_reason":"tool_use"`) ||
		!strings.Contains(body, `"stop_sequence":"done"`) ||
		!strings.Contains(body, "event: message_stop") {
		t.Fatalf("terminal output=%s", body)
	}
}

func writeCompletionGuardTestFrames(
	t *testing.T,
	writer *completionGuardStreamingWriter,
	frames ...string,
) {
	t.Helper()
	for _, frame := range frames {
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		writer.Flush()
	}
}

func TestCompletionGuardRejectsToolUseTruncatedByMaxTokens(
	t *testing.T,
) {
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "max_tokens",
			Content: []ClaudeContentBlock{
				{
					Type:  "tool_use",
					ID:    "tool-1",
					Name:  "edit",
					Input: map[string]interface{}{"path": "a.go"},
				},
			},
		},
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)

	if outcome.Accepted || outcome.Reason != "truncated_response" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardStreamingWriterHoldsClientToolUntilFinalization(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"edit\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":8}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)

	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	if !writer.protocolComplete() {
		t.Fatalf("protocol error=%v", writer.protocolErr)
	}
	if strings.Contains(recorder.Body.String(), "tool-1") {
		t.Fatalf(
			"client tool leaked before finalization: %s",
			recorder.Body.String(),
		)
	}

	outcome := classifyCompletionGuardResponse(
		func() *ClaudeResponse {
			response := writer.response()
			return &response
		}(),
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if !outcome.Accepted || outcome.Kind != "real_tool" {
		t.Fatalf("outcome=%#v", outcome)
	}

	if err := writer.flushHeldFrames(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "tool-1") ||
		!strings.Contains(recorder.Body.String(), "a.go") {
		t.Fatalf("final tool block missing: %s", recorder.Body.String())
	}
}

func TestCompletionGuardStreamingWriterRejectsInvalidToolJSON(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"edit\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}

	if writer.protocolComplete() || writer.protocolErr == nil {
		t.Fatalf("expected invalid tool JSON protocol error")
	}
	if strings.Contains(recorder.Body.String(), "tool-1") {
		t.Fatalf(
			"invalid tool block was forwarded: %s",
			recorder.Body.String(),
		)
	}
	if outcome := classifyCompletionGuardResponse(
		func() *ClaudeResponse {
			response := writer.response()
			return &response
		}(),
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	); outcome.RealTools != 0 {
		t.Fatalf("invalid tool counted as real: %#v", outcome)
	}
}

func TestCompletionGuardStreamingWriterRejectsIncompleteTrailingFrame(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
	)
	partial := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"broken"
	if _, err := writer.Write([]byte(partial)); err != nil {
		t.Fatal(err)
	}
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}

	if writer.protocolComplete() || writer.protocolErr == nil {
		t.Fatal("expected incomplete trailing frame error")
	}
	if strings.Contains(recorder.Body.String(), "broken") {
		t.Fatalf(
			"partial SSE frame leaked downstream: %s",
			recorder.Body.String(),
		)
	}
}

func TestCompletionGuardStreamingWriterValidatesTerminalOrder(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	if writer.protocolComplete() || writer.protocolErr == nil {
		t.Fatal("out-of-order message_stop was accepted")
	}
}

func TestCompletionGuardRequiredToolRecoveryPrompt(
	t *testing.T,
) {
	original := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Edit the file."},
		},
	}
	candidate := ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "I would edit the file."},
		},
	}
	plan := agentCompletionGuardPlan{ToolChoiceKind: "any"}

	outcome := classifyCompletionGuardResponse(&candidate, plan)
	if outcome.Reason != "required_tool_not_called" {
		t.Fatalf("outcome=%#v", outcome)
	}

	next, err := buildCompletionGuardStreamRecoveryRequest(
		original,
		candidate,
		candidate.Content,
		outcome.Reason,
		plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	last := next.Messages[len(next.Messages)-1]
	if last.Role != "user" ||
		!strings.Contains(
			fmt.Sprint(last.Content),
			"Call an appropriate real provided tool now",
		) {
		t.Fatalf("recovery prompt=%#v", last)
	}
}

func TestCompletionGuardVisibleRecoveryOmitsThinkingAndTrimsOverlap(
	t *testing.T,
) {
	blocks := []ClaudeContentBlock{
		{Type: "thinking", Thinking: "hidden"},
		{Type: "text", Text: "root cause is fixed"},
		{
			Type:  "tool_use",
			ID:    "tool-1",
			Name:  "edit",
			Input: map[string]interface{}{"path": "a.go"},
		},
	}

	prepared, visible := prepareCompletionGuardVisibleRecovery(
		blocks,
		"The root cause is",
		false,
	)
	if visible != "The root cause is fixed" {
		t.Fatalf("visible=%q", visible)
	}
	if len(prepared) != 2 ||
		prepared[0].Type != "text" ||
		prepared[0].Text != " fixed" ||
		prepared[1].Type != "tool_use" {
		t.Fatalf("prepared=%#v", prepared)
	}

	recorder := &completionGuardFlushBuffer{
		header: make(http.Header),
	}
	sse, ok := newCompletionGuardSSE(recorder)
	if !ok {
		t.Fatal("no flusher")
	}
	emitCompletionGuardBlocks(
		sse,
		prepared,
		1,
		completionGuardEmitVisibleRecovery,
	)
	if strings.Contains(recorder.String(), "thinking_delta") ||
		strings.Contains(recorder.String(), "hidden") {
		t.Fatalf("recovery thinking leaked: %s", recorder.String())
	}
}

func TestCompletionGuardVisibleRecoveryPreservesServerToolBlocks(t *testing.T) {
	prepared, _ := prepareCompletionGuardVisibleRecovery(
		[]ClaudeContentBlock{
			{Type: "server_tool_use", ID: "srv-1", Name: "web_search", Input: map[string]interface{}{"query": "x"}},
			{Type: "web_search_tool_result", ToolUseID: "srv-1", Content: "done"},
		},
		"",
		false,
	)
	if len(prepared) != 2 || prepared[0].Type != "server_tool_use" ||
		prepared[1].Type != "web_search_tool_result" {
		t.Fatalf("prepared=%#v", prepared)
	}
}

func TestEmitCompletionGuardTerminalRequiresStopReason(
	t *testing.T,
) {
	recorder := &completionGuardFlushBuffer{
		header: make(http.Header),
	}
	sse, ok := newCompletionGuardSSE(recorder)
	if !ok {
		t.Fatal("no flusher")
	}
	if err := emitCompletionGuardTerminal(
		sse,
		"",
		nil,
		ClaudeUsage{},
	); err == nil {
		t.Fatal("empty stop reason was accepted")
	}
	if recorder.Len() != 0 {
		t.Fatalf("terminal data was emitted: %s", recorder.String())
	}
}

func TestCompletionGuardTerminalUsageExcludesHiddenInputUsage(
	t *testing.T,
) {
	usage := completionGuardTerminalUsageMap(
		ClaudeUsage{
			InputTokens:              100,
			OutputTokens:             12,
			CacheCreationInputTokens: 50,
			CacheReadInputTokens:     25,
		},
	)
	if len(usage) != 1 || usage["output_tokens"] != 12 {
		t.Fatalf("terminal usage=%#v", usage)
	}
}

func TestCompletionGuardStreamingWriterPreservesServerToolBlocks(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srv-1\",\"name\":\"web_search\",\"server_name\":\"web-search\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"srv-1\",\"content\":{\"type\":\"web_search_result\"}}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":4}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	if !writer.protocolComplete() {
		t.Fatalf("protocol error=%v", writer.protocolErr)
	}

	candidate := writer.response()
	request := ClaudeRequest{Tools: []ClaudeTool{{
		Type: "web_search_20250305",
		Name: "web_search",
	}}}
	plan := buildAgentCompletionGuardPlan(&request, false)
	outcome := classifyCompletionGuardResponse(&candidate, plan)
	if !outcome.Accepted ||
		outcome.Kind != "server_tool" ||
		outcome.ServerTools != 1 ||
		outcome.ServerToolResults != 1 {
		t.Fatalf("outcome=%#v candidate=%#v", outcome, candidate)
	}

	raw, ok := writer.assistantContent().([]interface{})
	if !ok || len(raw) != 2 {
		t.Fatalf("raw content=%#v", writer.assistantContent())
	}
	serverBlock, _ := raw[0].(map[string]interface{})
	if serverBlock["server_name"] != "web-search" {
		t.Fatalf("server metadata was not preserved: %#v", serverBlock)
	}
	if len(candidate.Content) != 2 || string(candidate.Content[0].Extra["server_name"]) != `"web-search"` {
		t.Fatalf("typed server metadata was not preserved: %#v", candidate.Content)
	}
	resultBlock, _ := raw[1].(map[string]interface{})
	if resultBlock["tool_use_id"] != "srv-1" ||
		resultBlock["content"] == nil {
		t.Fatalf("server result was not preserved: %#v", resultBlock)
	}
}

func TestCompletionGuardPauseTurnRecoveryPreservesServerToolHistory(t *testing.T) {
	original := ClaudeRequest{
		Model:    "claude-opus-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "Search the web."}},
	}
	raw := []interface{}{map[string]interface{}{
		"type": "server_tool_use", "id": "srv-1", "name": "web_search",
		"server_name": "web-search", "input": map[string]interface{}{"query": "go"},
	}}

	next, err := buildCompletionGuardStreamRecoveryRequest(
		original,
		ClaudeResponse{StopReason: "pause_turn"},
		raw,
		"pause_turn_requires_continuation",
		agentCompletionGuardPlan{},
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := ClaudeToKiro(&next, false)
	assistant := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if assistant == nil || len(assistant.ClaudeContentBlocks) != 1 {
		t.Fatalf("assistant=%#v", assistant)
	}
	if string(assistant.ClaudeContentBlocks[0].Extra["server_name"]) != `"web-search"` {
		t.Fatalf("server metadata lost: %#v", assistant.ClaudeContentBlocks)
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&next, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&next, semantic, next.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
}

func TestCompletionGuardStreamingWriterSupportsCRLFFrames(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\r\n\r\n",
		"event: content_block_start\r\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\r\n\r\n",
		"event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\r\n\r\n",
		"event: content_block_stop\r\ndata: {\"type\":\"content_block_stop\",\"index\":0}\r\n\r\n",
		"event: message_delta\r\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\r\n\r\n",
		"event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	if !writer.protocolComplete() {
		t.Fatalf("protocol error=%v", writer.protocolErr)
	}
	if !strings.Contains(recorder.Body.String(), "hello") {
		t.Fatalf("CRLF content was not forwarded: %s", recorder.Body.String())
	}
}

func TestCompletionGuardRecoveryDropsUncommittedClientTool(
	t *testing.T,
) {
	original := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Edit a.go."},
		},
	}
	raw := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": "I started the edit.",
		},
		map[string]interface{}{
			"type": "tool_use",
			"id":   "tool-1",
			"name": "edit",
			"input": map[string]interface{}{
				"path": "a.go",
			},
		},
	}

	next, err := buildCompletionGuardStreamRecoveryRequest(
		original,
		ClaudeResponse{StopReason: "max_tokens"},
		raw,
		"truncated_response",
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Messages) != 3 {
		t.Fatalf("messages=%#v", next.Messages)
	}
	assistant, ok := next.Messages[1].Content.([]interface{})
	if !ok {
		t.Fatalf("assistant content type=%T", next.Messages[1].Content)
	}
	for _, value := range assistant {
		block, _ := value.(map[string]interface{})
		if block["type"] == "tool_use" {
			t.Fatalf("uncommitted client tool survived recovery: %#v", assistant)
		}
	}
}

func TestCompletionGuardStreamingWriterStopsAfterProtocolError(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"invalid\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"must-not-leak\"}}\n\n",
	)
	if writer.protocolErr == nil {
		t.Fatal("expected protocol error")
	}
	if strings.Contains(recorder.Body.String(), "must-not-leak") {
		t.Fatalf("content after protocol error leaked: %s", recorder.Body.String())
	}
}

func TestCompletionGuardStreamingWriterAcceptsMultipleMessageDeltaEvents(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	if !writer.protocolComplete() {
		t.Fatalf("protocol error=%v", writer.protocolErr)
	}
	response := writer.response()
	if response.StopReason != "end_turn" ||
		response.Usage.OutputTokens != 2 {
		t.Fatalf("response=%#v", response)
	}
}

func TestCompletionGuardStreamingWriterRejectsWrongToolDeltaType(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"edit\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"bad\"}}\n\n",
	)
	if writer.protocolErr == nil {
		t.Fatal("expected invalid tool delta to fail")
	}
	if strings.Contains(recorder.Body.String(), "tool-1") {
		t.Fatalf("invalid tool frame leaked: %s", recorder.Body.String())
	}
}

func TestCompletionGuardStreamingWriterRejectsResultDelta(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"srv-1\",\"content\":[]}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
	)
	if writer.protocolErr == nil {
		t.Fatal("server-tool result blocks must not receive deltas")
	}
}

func TestCompletionGuardStreamingWriterRejectsBlockReuseAndParallelOpen(
	t *testing.T,
) {
	t.Run("parallel", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		writer, _ := newCompletionGuardStreamingWriter(
			recorder,
			"claude-opus-5",
		)
		defer writer.release()
		writeCompletionGuardTestFrames(
			t,
			writer,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		)
		if writer.protocolErr == nil {
			t.Fatal("parallel content blocks must fail")
		}
	})

	t.Run("reuse", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		writer, _ := newCompletionGuardStreamingWriter(
			recorder,
			"claude-opus-5",
		)
		defer writer.release()
		writeCompletionGuardTestFrames(
			t,
			writer,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		)
		if writer.protocolErr == nil {
			t.Fatal("reused content-block index must fail")
		}
	})
}

func TestCompletionGuardStreamingWriterTreatsErrorAsTerminal(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad request\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"must-not-leak\"}}\n\n",
	)
	if !writer.sawError || writer.streamErrorRetryable {
		t.Fatalf(
			"error state type=%q retryable=%t",
			writer.streamErrorType,
			writer.streamErrorRetryable,
		)
	}
	if strings.Contains(recorder.Body.String(), "must-not-leak") {
		t.Fatalf("frame after error leaked: %s", recorder.Body.String())
	}
}

func TestCompletionGuardServerToolPairing(
	t *testing.T,
) {
	programmaticCallers, _ := json.Marshal([]string{
		"direct",
		completionGuardCodeCallerType,
	})
	plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{
			{
				Name:        "read",
				InputSchema: map[string]interface{}{"type": "object"},
				Extra: map[string]json.RawMessage{
					"allowed_callers": programmaticCallers,
				},
			},
			{Type: "web_search_20250305", Name: "web_search"},
			{Type: "code_execution_20260120", Name: "code_execution"},
		},
	}, false)
	if plan.ValidationReason != "" {
		t.Fatalf("plan validation=%q", plan.ValidationReason)
	}

	complete := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "srv-1", Name: "web_search", Input: map[string]interface{}{"query": "x"}},
				{Type: "web_search_tool_result", ToolUseID: "srv-1"},
			},
		},
		plan,
	)
	if !complete.Accepted || complete.Kind != "server_tool" {
		t.Fatalf("complete server tool=%#v", complete)
	}

	unmatched := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "web_search_tool_result", ToolUseID: "missing"},
			},
		},
		plan,
	)
	if unmatched.Accepted ||
		unmatched.Reason != "unmatched_server_tool_result" {
		t.Fatalf("unmatched result=%#v", unmatched)
	}

	mixed := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "srv-2", Name: "web_search", Input: map[string]interface{}{"query": "x"}},
				{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{}},
			},
		},
		plan,
	)
	if !mixed.Accepted || mixed.Kind != "real_tool" {
		t.Fatalf("mixed server/client turn=%#v", mixed)
	}

	continued := classifyCompletionGuardResponseWithExpectedServerTools(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "web_search_tool_result", ToolUseID: "srv-prev"},
			},
		},
		plan,
		map[string]string{"srv-prev": "web_search"},
	)
	if !continued.Accepted || continued.Kind != "server_tool" {
		t.Fatalf("continued server result=%#v", continued)
	}

	unrelatedClientTool := classifyCompletionGuardResponseWithExpectedServerTools(
		&ClaudeResponse{
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{{
				Type:  "tool_use",
				ID:    "tool-next",
				Name:  "read",
				Input: map[string]interface{}{},
			}},
		},
		plan,
		map[string]string{"srv-prev": "web_search"},
	)
	if unrelatedClientTool.Accepted ||
		unrelatedClientTool.Reason != "unresolved_server_tool_use" {
		t.Fatalf("unrelated client tool outcome=%#v", unrelatedClientTool)
	}

	caller, _ := json.Marshal(map[string]interface{}{
		"type":    "code_execution_20260120",
		"tool_id": "srv-code",
	})
	programmatic := classifyCompletionGuardResponseWithExpectedServerTools(
		&ClaudeResponse{
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{{
				Type:  "tool_use",
				ID:    "tool-programmatic",
				Name:  "read",
				Input: map[string]interface{}{},
				Extra: map[string]json.RawMessage{"caller": caller},
			}},
		},
		plan,
		map[string]string{"srv-code": "code_execution"},
	)
	if !programmatic.Accepted || programmatic.Kind != "real_tool" {
		t.Fatalf("programmatic continuation outcome=%#v", programmatic)
	}

	reusedServerID := classifyCompletionGuardResponseWithExpectedServerTools(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "web_search_tool_result", ToolUseID: "srv-reused"},
				{
					Type:  "server_tool_use",
					ID:    "srv-reused",
					Name:  "web_search",
					Input: map[string]interface{}{"query": "again"},
				},
			},
		},
		plan,
		map[string]string{"srv-reused": "web_search"},
	)
	if reusedServerID.Accepted ||
		reusedServerID.Reason != "duplicate_server_tool_id" {
		t.Fatalf("reused server id outcome=%#v", reusedServerID)
	}
}

func TestCompletionGuardRetryabilityClassification(
	t *testing.T,
) {
	for _, status := range []int{408, 429, 500, 503, 599} {
		if !completionGuardRetryableHTTPStatus(status) {
			t.Fatalf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422} {
		if completionGuardRetryableHTTPStatus(status) {
			t.Fatalf("status %d must not be retried", status)
		}
	}
	for _, errorType := range []string{
		"overloaded_error",
		"rate_limit_error",
		"api_error",
	} {
		if !completionGuardRetryableSSEErrorType(errorType) {
			t.Fatalf("SSE error %q should be retryable", errorType)
		}
	}
	if completionGuardRetryableSSEErrorType("invalid_request_error") {
		t.Fatal("invalid_request_error must not be retried")
	}
}

func TestCompletionGuardPauseTurnPreservesNativeClaudeBlocks(t *testing.T) {
	serverName, _ := json.Marshal("web-search")
	next, err := completionGuardPauseTurnRequest(
		ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "continue"}}},
		&ClaudeResponse{
			StopReason: "pause_turn",
			Content: []ClaudeContentBlock{
				{Type: "thinking", Thinking: "summary", Signature: "sig"},
				{Type: "server_tool_use", ID: "srv-1", Name: "web_search", Input: map[string]interface{}{"query": "go"}, Extra: map[string]json.RawMessage{"server_name": serverName}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := ClaudeToKiro(&next, false)
	history := payload.ConversationState.History
	if len(history) != 2 || history[1].AssistantResponseMessage == nil {
		t.Fatalf("history=%#v", history)
	}
	blocks := history[1].AssistantResponseMessage.ClaudeContentBlocks
	if len(blocks) != 2 || string(blocks[1].Extra["server_name"]) != `"web-search"` {
		t.Fatalf("lossless blocks=%#v", blocks)
	}
}

func TestCompletionGuardStreamingWriterDefersMessageStartUntilContent(
	t *testing.T,
) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
	)
	if recorder.Body.Len() != 0 || writer.hasCommittedContent() {
		t.Fatalf("message_start committed too early: %s", recorder.Body.String())
	}

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hello\"}}\n\n",
	)
	if !writer.hasCommittedContent() ||
		!strings.Contains(recorder.Body.String(), "message_start") {
		t.Fatalf("message_start was not flushed with content: %s", recorder.Body.String())
	}
}

func TestReplayCompletionGuardSSEPreservesServerAndMCPBlocks(t *testing.T) {
	buffer := &completionGuardFlushBuffer{header: make(http.Header)}
	sse, ok := newCompletionGuardSSE(buffer)
	if !ok {
		t.Fatal("no flusher")
	}
	serverName, _ := json.Marshal("web-search")
	isError, _ := json.Marshal(true)
	resp := &ClaudeResponse{
		ID:         "m",
		Model:      "claude-opus-5",
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{
				Type:  "server_tool_use",
				ID:    "srv-1",
				Name:  "web_search",
				Input: map[string]interface{}{"query": "go"},
				Extra: map[string]json.RawMessage{"server_name": serverName},
			},
			{
				Type:      "web_search_tool_result",
				ToolUseID: "srv-1",
				Content:   map[string]interface{}{"type": "web_search_result"},
			},
			{
				Type:      "mcp_tool_result",
				ToolUseID: "mcp-1",
				Content:   "done",
				Extra:     map[string]json.RawMessage{"server_name": serverName, "is_error": isError},
			},
		},
	}

	replayCompletionGuardSSE(sse, resp)
	text := buffer.String()
	for _, value := range []string{
		"server_tool_use",
		"web_search_tool_result",
		"mcp_tool_result",
		"input_json_delta",
		`"server_name":"web-search"`,
		`"is_error":true`,
		"message_stop",
	} {
		if !strings.Contains(text, value) {
			t.Fatalf("missing %q in %s", value, text)
		}
	}
}

func TestCompletionGuardStreamingWriterHoldsRetryableErrorForRecovery(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"retry later\"}}\n\n",
	)

	if !writer.hasCommittedContent() || !writer.sawError || !writer.streamErrorRetryable {
		t.Fatalf(
			"committed=%t sawError=%t retryable=%t",
			writer.hasCommittedContent(),
			writer.sawError,
			writer.streamErrorRetryable,
		)
	}
	if strings.Contains(recorder.Body.String(), "event: error") ||
		strings.Contains(recorder.Body.String(), "overloaded_error") {
		t.Fatalf("terminal error leaked before recovery: %s", recorder.Body.String())
	}
}

func TestCompletionGuardStreamingWriterReplaysPrecommitHTTPError(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Upstream-Error", "yes")
	writer.WriteHeader(http.StatusServiceUnavailable)
	if _, err := writer.Write([]byte(`{"error":{"type":"overloaded_error"}}`)); err != nil {
		t.Fatal(err)
	}
	_ = writer.finish()

	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("headers leaked before replay: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := writer.writeOriginalHTTPResponse(); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type=%q", got)
	}
	if recorder.Header().Get("X-Upstream-Error") != "yes" ||
		!strings.Contains(recorder.Body.String(), "overloaded_error") {
		t.Fatalf("response headers/body not preserved: headers=%v body=%s", recorder.Header(), recorder.Body.String())
	}
}

func TestCompletionGuardStreamingWriterFirstWriteHeaderWins(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writer.WriteHeader(http.StatusAccepted)
	writer.WriteHeader(http.StatusInternalServerError)
	if writer.status != http.StatusAccepted {
		t.Fatalf("status=%d", writer.status)
	}
}

func TestCompletionGuardRetryFallbackRefusesMalformedToolState(t *testing.T) {
	malformed := ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "partial answer"},
		},
	}
	if completionGuardNormalizeRetryFallback(
		&malformed,
		completionGuardOutcome{
			Reason:      "invalid_tool_input",
			NaturalText: "partial answer",
		},
	) {
		t.Fatal("malformed tool state was accepted")
	}

	semantic := malformed
	if !completionGuardNormalizeRetryFallback(
		&semantic,
		completionGuardOutcome{
			Reason:      "required_tool_not_called",
			NaturalText: "partial answer",
		},
	) {
		t.Fatal("safe prose fallback was rejected")
	}
	if semantic.StopReason != "end_turn" || semantic.StopSequence != nil {
		t.Fatalf("fallback=%#v", semantic)
	}
}

func TestCompletionGuardVisibleRecoveryUsageIncludesEmittedOutput(t *testing.T) {
	usage := ClaudeUsage{OutputTokens: 5}
	addCompletionGuardVisibleOutputUsage(
		&usage,
		ClaudeUsage{OutputTokens: 10},
		[]ClaudeContentBlock{{Type: "text", Text: strings.Repeat("x", 20)}},
		completionGuardEmitVisibleRecovery,
	)
	if usage.OutputTokens != 10 {
		t.Fatalf("output tokens=%d", usage.OutputTokens)
	}
}

func TestCompletionGuardPauseTurnSupportedBlocksReachKiroHistory(t *testing.T) {
	next, err := completionGuardPauseTurnRequest(
		ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "continue"}}},
		&ClaudeResponse{
			StopReason: "pause_turn",
			Content: []ClaudeContentBlock{
				{Type: "text", Text: "partial"},
				{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{"path": "a.go"}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	assistant := next.Messages[len(next.Messages)-1]
	text, tools := extractClaudeAssistantContent(assistant.Content)
	if text != "partial" || len(tools) != 1 || tools[0].ToolUseID != "tool-1" {
		t.Fatalf("text=%q tools=%#v content=%#v", text, tools, assistant.Content)
	}
}

func TestCompletionGuardClientToolValidation(t *testing.T) {
	readTool := ClaudeTool{
		Name: "read",
		InputSchema: map[string]interface{}{
			"type":     "object",
			"required": []interface{}{"path"},
		},
	}
	basePlan := completionGuardTestPlanWithTools("auto", readTool)

	tests := []struct {
		name   string
		plan   agentCompletionGuardPlan
		block  ClaudeContentBlock
		stop   string
		reason string
		ok     bool
	}{
		{
			name: "missing id", plan: basePlan, stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", Name: "read", Input: map[string]interface{}{"path": "a.go"}},
			reason: "client_tool_without_id",
		},
		{
			name: "missing name", plan: basePlan, stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Input: map[string]interface{}{"path": "a.go"}},
			reason: "client_tool_without_name",
		},
		{
			name: "non object input", plan: basePlan, stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "read", Input: []interface{}{"a.go"}},
			reason: "client_tool_invalid_input",
		},
		{
			name: "unknown tool", plan: basePlan, stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "write", Input: map[string]interface{}{}},
			reason: "unknown_client_tool",
		},
		{
			name: "no catalog", plan: completionGuardTestPlanWithTools("auto"), stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{}},
			reason: "unknown_client_tool",
		},
		{
			name: "server tool emitted as client tool", plan: completionGuardTestPlanWithTools("auto", ClaudeTool{Type: "web_search_20250305", Name: "web_search"}), stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "web_search", Input: map[string]interface{}{}},
			reason: "client_tool_not_callable",
		},
		{
			name: "anthropic bash client tool", plan: completionGuardTestPlanWithTools("auto", ClaudeTool{Type: "bash_20250124", Name: "bash"}), stop: "tool_use",
			block: ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "bash", Input: map[string]interface{}{"command": "go test ./..."}},
			ok:    true,
		},
		{
			name: "missing required", plan: basePlan, stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{}},
			reason: "client_tool_missing_required_input",
		},
		{
			name: "invalid stop reason", plan: basePlan, stop: "end_turn",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{"path": "a.go"}},
			reason: "client_tool_with_invalid_stop_reason",
		},
		{
			name: "tool choice none", plan: completionGuardTestPlanWithTools("none", readTool), stop: "tool_use",
			block:  ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{"path": "a.go"}},
			reason: "client_tool_not_allowed",
		},
		{
			name: "valid", plan: basePlan, stop: "tool_use",
			block: ClaudeContentBlock{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{"path": "a.go"}},
			ok:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: tc.stop,
				Content:    []ClaudeContentBlock{tc.block},
			}, tc.plan)
			if tc.ok {
				if !outcome.Accepted || outcome.Kind != "real_tool" {
					t.Fatalf("outcome=%#v", outcome)
				}
				return
			}
			if outcome.Accepted || outcome.Reason != tc.reason {
				t.Fatalf("outcome=%#v, want reason=%q", outcome, tc.reason)
			}
		})
	}
}

func TestCompletionGuardServerToolValidation(t *testing.T) {
	plan := agentCompletionGuardPlan{ToolChoiceKind: "auto"}
	tests := []struct {
		name   string
		block  ClaudeContentBlock
		reason string
	}{
		{
			name: "missing id",
			block: ClaudeContentBlock{
				Type: "server_tool_use", Name: "web_search",
				Input: map[string]interface{}{"query": "go"},
			},
			reason: "server_tool_without_id",
		},
		{
			name: "missing name",
			block: ClaudeContentBlock{
				Type: "server_tool_use", ID: "srv-1",
				Input: map[string]interface{}{"query": "go"},
			},
			reason: "server_tool_without_name",
		},
		{
			name: "non object input",
			block: ClaudeContentBlock{
				Type: "server_tool_use", ID: "srv-1", Name: "web_search",
				Input: []interface{}{"go"},
			},
			reason: "server_tool_invalid_input",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(
				&ClaudeResponse{
					StopReason: "pause_turn",
					Content:    []ClaudeContentBlock{tc.block},
				},
				plan,
			)
			if outcome.Accepted || outcome.Reason != tc.reason {
				t.Fatalf("outcome=%#v, want reason=%q", outcome, tc.reason)
			}
		})
	}
}

func TestCompletionGuardServerToolCatalogValidation(t *testing.T) {
	serverBlock := ClaudeContentBlock{
		Type:  "server_tool_use",
		ID:    "srv-1",
		Name:  "web_search",
		Input: map[string]interface{}{"query": "go"},
	}
	serverResult := ClaudeContentBlock{
		Type:      "web_search_tool_result",
		ToolUseID: "srv-1",
		Content:   []interface{}{},
	}

	tests := []struct {
		name   string
		plan   agentCompletionGuardPlan
		blocks []ClaudeContentBlock
		reason string
		ok     bool
	}{
		{
			name: "known server tool",
			plan: completionGuardTestPlanWithTools(
				"auto",
				ClaudeTool{Type: "web_search_20250305", Name: "web_search"},
			),
			blocks: []ClaudeContentBlock{serverBlock, serverResult},
			ok:     true,
		},
		{
			name:   "unknown server tool",
			plan:   completionGuardTestPlanWithTools("auto"),
			blocks: []ClaudeContentBlock{serverBlock, serverResult},
			reason: "unknown_server_tool",
		},
		{
			name: "client catalog entry emitted as server tool",
			plan: completionGuardTestPlanWithTools(
				"auto",
				ClaudeTool{Name: "web_search"},
			),
			blocks: []ClaudeContentBlock{serverBlock, serverResult},
			reason: "unknown_server_tool",
		},
		{
			name: "tool choice none",
			plan: completionGuardTestPlanWithTools(
				"none",
				ClaudeTool{Type: "web_search_20250305", Name: "web_search"},
			),
			blocks: []ClaudeContentBlock{serverBlock, serverResult},
			reason: "server_tool_not_allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(
				&ClaudeResponse{
					StopReason: "end_turn",
					Content:    tc.blocks,
				},
				tc.plan,
			)
			if tc.ok {
				if !outcome.Accepted || outcome.Kind != "server_tool" {
					t.Fatalf("outcome=%#v", outcome)
				}
				return
			}
			if outcome.Accepted || outcome.Reason != tc.reason {
				t.Fatalf("outcome=%#v, want reason=%q", outcome, tc.reason)
			}
		})
	}
}

func TestCompletionGuardMCPServerCatalogValidation(t *testing.T) {
	mcpBlock := ClaudeContentBlock{
		Type:  "mcp_tool_use",
		ID:    "mcp-1",
		Name:  "search",
		Input: map[string]interface{}{"query": "go"},
		Extra: map[string]json.RawMessage{
			"server_name": json.RawMessage(`"example-mcp"`),
		},
	}
	mcpResult := ClaudeContentBlock{
		Type:      "mcp_tool_result",
		ToolUseID: "mcp-1",
		Content:   "done",
	}

	known := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content:    []ClaudeContentBlock{mcpBlock, mcpResult},
		},
		completionGuardTestPlanWithMCP("auto", "example-mcp"),
	)
	if !known.Accepted || known.Kind != "server_tool" {
		t.Fatalf("known MCP outcome=%#v", known)
	}

	unknown := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content:    []ClaudeContentBlock{mcpBlock, mcpResult},
		},
		completionGuardTestPlanWithMCP("auto", "other-mcp"),
	)
	if unknown.Accepted || unknown.Reason != "unknown_mcp_server_tool" {
		t.Fatalf("unknown MCP outcome=%#v", unknown)
	}
}

func TestClaudeContentBlockRoundTripPreservesUnknownFields(t *testing.T) {
	original := []byte(`{"type":"mcp_tool_result","tool_use_id":"mcp-1","content":"failed","server_name":"repo","is_error":true,"future_metadata":{"v":2}}`)
	var block ClaudeContentBlock
	if err := json.Unmarshal(original, &block); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got["server_name"] != "repo" || got["is_error"] != true {
		t.Fatalf("metadata lost: %s", encoded)
	}
	future, _ := got["future_metadata"].(map[string]interface{})
	if future["v"] != float64(2) {
		t.Fatalf("future metadata lost: %s", encoded)
	}

	toolJSON := []byte(`{"type":"web_search_20250305","name":"web_search","max_uses":5,"allowed_domains":["go.dev"]}`)
	var tool ClaudeTool
	if err := json.Unmarshal(toolJSON, &tool); err != nil {
		t.Fatal(err)
	}
	cloned, err := cloneCompletionGuardRequest(ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "assistant", Content: []ClaudeContentBlock{block}}},
		Tools:    []ClaudeTool{tool},
	})
	if err != nil {
		t.Fatal(err)
	}
	clonedJSON, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"server_name":"repo"`, `"is_error":true`, `"max_uses":5`, `"allowed_domains":["go.dev"]`} {
		if !bytes.Contains(clonedJSON, []byte(value)) {
			t.Fatalf("clone lost %s: %s", value, clonedJSON)
		}
	}
}

func TestClaudeResponseRoundTripAndSSEReplayPreserveRootMetadata(t *testing.T) {
	raw := []byte(`{
		"id":"msg-1",
		"type":"message",
		"role":"assistant",
		"model":"claude-opus-5",
		"content":[{"type":"text","text":"declined"}],
		"stop_reason":"refusal",
		"stop_sequence":null,
		"usage":{"input_tokens":1,"output_tokens":2},
		"container":{"id":"container-1"},
		"stop_details":{"type":"refusal","category":"cyber"}
	}`)
	var response ClaudeResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`"container":{"id":"container-1"}`,
		`"stop_details":`,
		`"category":"cyber"`,
		`"type":"refusal"`,
	} {
		if !bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("response round-trip lost %s: %s", value, encoded)
		}
	}

	continuation, err := completionGuardPauseTurnRequest(
		ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "continue"}}},
		&response,
	)
	if err != nil {
		t.Fatal(err)
	}
	continuationJSON, err := json.Marshal(continuation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(continuationJSON, []byte(`"container":{"id":"container-1"}`)) {
		t.Fatalf("pause_turn continuation lost container: %s", continuationJSON)
	}
	if bytes.Contains(continuationJSON, []byte(`"stop_details"`)) {
		t.Fatalf("response-only stop_details leaked into request: %s", continuationJSON)
	}

	streamRecorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(streamRecorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-2\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0},\"container\":{\"id\":\"container-2\"}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"declined\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_sequence\":null,\"stop_details\":{\"type\":\"refusal\",\"category\":\"cyber\"}},\"usage\":{\"output_tokens\":2}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	candidate := writer.takeResponse()
	writer.release()
	if !bytes.Contains(candidate.Extra["container"], []byte("container-2")) ||
		!bytes.Contains(candidate.StreamDeltaExtra["stop_details"], []byte("cyber")) {
		t.Fatalf("stream metadata not captured: extra=%s delta=%s", candidate.Extra["container"], candidate.StreamDeltaExtra["stop_details"])
	}

	replayRecorder := httptest.NewRecorder()
	sse, ok := newCompletionGuardSSE(replayRecorder)
	if !ok {
		t.Fatal("replay recorder requires flusher")
	}
	replayCompletionGuardSSE(sse, &candidate)
	replayed := replayRecorder.Body.String()
	startPos := strings.Index(replayed, `"container":{"id":"container-2"}`)
	deltaPos := strings.Index(replayed, `"stop_details":`)
	if startPos < 0 || deltaPos < 0 || startPos >= deltaPos {
		t.Fatalf("metadata replayed in wrong event: %s", replayed)
	}
}

func TestClaudeUsageRoundTripAndSSEReplayPreserveExtendedMetadata(t *testing.T) {
	raw := []byte(`{
		"id":"msg-usage",
		"type":"message",
		"role":"assistant",
		"model":"claude-opus-5",
		"content":[{"type":"text","text":"done"}],
		"stop_reason":"end_turn",
		"stop_sequence":null,
		"usage":{
			"input_tokens":10,
			"output_tokens":4,
			"inference_geo":"us",
			"service_tier":"standard_only",
			"server_tool_use":{"web_search_requests":2},
			"output_tokens_details":{"reasoning_tokens":3},
			"iterations":2
		}
	}`)
	var response ClaudeResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`"inference_geo":"us"`,
		`"service_tier":"standard_only"`,
		`"server_tool_use":{"web_search_requests":2}`,
		`"output_tokens_details":{"reasoning_tokens":3}`,
		`"iterations":2`,
	} {
		if !bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("usage round-trip lost %s: %s", value, encoded)
		}
	}

	recorder := httptest.NewRecorder()
	sse, ok := newCompletionGuardSSE(recorder)
	if !ok {
		t.Fatal("replay recorder requires flusher")
	}
	replayCompletionGuardSSE(sse, &response)
	replayed := recorder.Body.String()
	startEnd := strings.Index(replayed, "event: message_start")
	deltaStart := strings.Index(replayed, "event: message_delta")
	if startEnd < 0 || deltaStart < 0 {
		t.Fatalf("missing replay events: %s", replayed)
	}
	startFrame := replayed[startEnd:deltaStart]
	deltaFrame := replayed[deltaStart:]
	if strings.Contains(startFrame, `"iterations"`) ||
		strings.Contains(startFrame, `"server_tool_use"`) {
		t.Fatalf("terminal usage metadata leaked into message_start: %s", startFrame)
	}
	for _, value := range []string{
		`"iterations":2`,
		`"server_tool_use":{"web_search_requests":2}`,
		`"output_tokens_details":{"reasoning_tokens":3}`,
	} {
		if !strings.Contains(deltaFrame, value) {
			t.Fatalf("terminal usage replay lost %s: %s", value, deltaFrame)
		}
	}
}

func TestCompletionGuardStreamingUsageMetadataKeepsEventProvenance(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-usage-stream\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"inference_geo\":\"us\"}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"done\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":4,\"iterations\":2,\"server_tool_use\":{\"web_search_requests\":1}}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	candidate := writer.takeResponse()
	writer.release()

	if !bytes.Contains(candidate.StreamStartUsageExtra["inference_geo"], []byte("us")) ||
		!bytes.Contains(candidate.StreamDeltaUsageExtra["iterations"], []byte("2")) ||
		!bytes.Contains(candidate.StreamDeltaUsageExtra["server_tool_use"], []byte("web_search_requests")) {
		t.Fatalf("usage provenance missing: start=%v delta=%v", candidate.StreamStartUsageExtra, candidate.StreamDeltaUsageExtra)
	}

	replayRecorder := httptest.NewRecorder()
	sse, ok := newCompletionGuardSSE(replayRecorder)
	if !ok {
		t.Fatal("replay recorder requires flusher")
	}
	replayCompletionGuardSSE(sse, &candidate)
	replayed := replayRecorder.Body.String()
	deltaPos := strings.Index(replayed, "event: message_delta")
	if deltaPos < 0 {
		t.Fatalf("missing message_delta: %s", replayed)
	}
	startFrame := replayed[:deltaPos]
	deltaFrame := replayed[deltaPos:]
	if !strings.Contains(startFrame, `"inference_geo":"us"`) ||
		strings.Contains(startFrame, `"iterations"`) {
		t.Fatalf("start usage provenance corrupted: %s", startFrame)
	}
	if !strings.Contains(deltaFrame, `"iterations":2`) ||
		!strings.Contains(deltaFrame, `"server_tool_use":{"web_search_requests":1}`) ||
		strings.Contains(deltaFrame, `"inference_geo"`) {
		t.Fatalf("delta usage provenance corrupted: %s", deltaFrame)
	}
}

func TestCompletionGuardCommittedPrefixUsageExcludesHeldOutput(t *testing.T) {
	upstream := ClaudeUsage{
		InputTokens:  10,
		OutputTokens: 100,
		Extra: map[string]json.RawMessage{
			"server_tool_use": json.RawMessage(`{"web_search_requests":1}`),
		},
	}
	blocks := []ClaudeContentBlock{
		{Type: "text", Text: "visible"},
		{
			Type:  "server_tool_use",
			ID:    "srv-hidden",
			Name:  "web_search",
			Input: map[string]interface{}{"query": "hidden"},
		},
	}
	visible := completionGuardUsageForCommittedPrefix(
		upstream,
		blocks,
		0,
	)
	if visible.OutputTokens <= 0 || visible.OutputTokens >= upstream.OutputTokens {
		t.Fatalf("visible output tokens=%d", visible.OutputTokens)
	}
	if len(visible.Extra) != 0 {
		t.Fatalf("hidden terminal usage leaked: %#v", visible.Extra)
	}
	full := completionGuardUsageForCommittedPrefix(
		upstream,
		blocks,
		len(blocks)-1,
	)
	if full.OutputTokens != upstream.OutputTokens ||
		len(full.Extra) == 0 {
		t.Fatalf("full usage was not preserved: %#v", full)
	}
}

func TestClaudeRequestRoundTripPreservesMCPConnectorFields(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"Use MCP."}],
		"mcp_servers":[{"type":"url","url":"https://mcp.example/sse","name":"example","authorization_token":"secret"}],
		"betas":["mcp-client-2025-11-20"],
		"metadata":{"user_id":"u-1"},
		"tools":[{"type":"mcp_toolset","mcp_server_name":"example"}]
	}`)

	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, raw)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("MCP requirements=%#v validation=%v", requirements, validationErr)
	}

	cloned, err := cloneCompletionGuardRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`"mcp_servers":[`,
		`"authorization_token":"secret"`,
		`"metadata":{"user_id":"u-1"}`,
		`"mcp_server_name":"example"`,
	} {
		if !bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("request clone lost %s: %s", value, encoded)
		}
	}
	if bytes.Contains(encoded, []byte(`"betas"`)) {
		t.Fatalf("compatibility beta field survived normalization: %s", encoded)
	}
	if got := semantic.Headers.Get("Anthropic-Beta"); got != "mcp-client-2025-11-20" {
		t.Fatalf("beta header=%q", got)
	}

	payload := ClaudeToKiro(&cloned, false)
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("authorization_token")) || bytes.Contains(wire, []byte("mcp_servers")) {
		t.Fatalf("MCP credentials leaked into Kiro wire payload: %s", wire)
	}
}

func TestClaudeRequestUnknownTopLevelFieldRequiresDirectPassthrough(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-4.8",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"stop_sequences":["DONE"],
		"future_control":{"mode":"strict"}
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, raw)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
	if !strings.Contains(requirements.Summary(), "future_control") || !strings.Contains(requirements.Summary(), "stop_sequences") {
		t.Fatalf("summary=%q", requirements.Summary())
	}
}

func TestClaudeToKiroPreservesMixedServerClientTurnForNativePassthrough(t *testing.T) {
	serverName, _ := json.Marshal("web-search")
	req := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Research and edit."},
			{Role: "assistant", Content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "srv-1", Name: "web_search", Input: map[string]interface{}{"query": "go"}, Extra: map[string]json.RawMessage{"server_name": serverName}},
				{Type: "tool_use", ID: "tool-1", Name: "edit", Input: map[string]interface{}{"path": "a.go"}},
			}},
			{Role: "user", Content: []ClaudeContentBlock{{Type: "tool_result", ToolUseID: "tool-1", Content: "done"}}},
		},
		Tools: []ClaudeTool{
			{Type: "web_search_20250305", Name: "web_search", Extra: map[string]json.RawMessage{"max_uses": json.RawMessage(`5`)}},
			{Name: "edit", InputSchema: map[string]interface{}{"type": "object"}},
		},
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}

	payload := ClaudeToKiro(&req, false)
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("history=%#v", payload.ConversationState.History)
	}
	assistant := payload.ConversationState.History[len(payload.ConversationState.History)-1].AssistantResponseMessage
	if assistant == nil || len(assistant.ClaudeContentBlocks) != 2 {
		t.Fatalf("assistant sidecar=%#v", assistant)
	}
	if string(assistant.ClaudeContentBlocks[0].Extra["server_name"]) != `"web-search"` {
		t.Fatalf("server metadata lost: %#v", assistant.ClaudeContentBlocks)
	}
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("server_name")) {
		t.Fatalf("native-only sidecar leaked into Kiro wire payload: %s", wire)
	}
}

func TestClaudeToKiroUnknownAssistantBlockRequiresNativePassthrough(t *testing.T) {
	futureValue, _ := json.Marshal("kept")
	req := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "continue"},
			{Role: "assistant", Content: []ClaudeContentBlock{{Type: "future_structured_block", Extra: map[string]json.RawMessage{"future_field": futureValue}}}},
			{Role: "user", Content: "next"},
		},
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
	payload := ClaudeToKiro(&req, false)
	history := payload.ConversationState.History
	blocks := history[len(history)-1].AssistantResponseMessage.ClaudeContentBlocks
	if len(blocks) != 1 || blocks[0].Type != "future_structured_block" {
		t.Fatalf("sidecar=%#v", blocks)
	}
}

func TestClaudeToKiroMCPHistoryRequiresDirectAPI(t *testing.T) {
	serverName, _ := json.Marshal("example-mcp")
	req := ClaudeRequest{
		Model: "claude-opus-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Use MCP."},
			{Role: "assistant", Content: []ClaudeContentBlock{{Type: "mcp_tool_use", ID: "mcp-1", Name: "echo", Input: map[string]interface{}{"text": "hi"}, Extra: map[string]json.RawMessage{"server_name": serverName}}}},
			{Role: "user", Content: "continue"},
		},
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
}

func TestClaudeToKiroTypedClientToolCanUseBedrockPassthrough(t *testing.T) {
	req := ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []ClaudeMessage{{Role: "user", Content: "Run tests."}},
		Tools:    []ClaudeTool{{Type: "bash_20250124", Name: "bash"}},
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	want := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
	if validationErr != nil || requirements.Allowed != want {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
}

func TestClaudeToKiroDoesNotRetainDuplicateSidecarForOrdinaryTurn(t *testing.T) {
	req := ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Read a file."},
			{Role: "assistant", Content: []ClaudeContentBlock{
				{Type: "text", Text: "Reading."},
				{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{"path": "a.go"}},
			}},
			{Role: "user", Content: []ClaudeContentBlock{
				{Type: "tool_result", ToolUseID: "tool-1", Content: "contents"},
			}},
		},
		Tools: []ClaudeTool{{Name: "read", InputSchema: map[string]interface{}{"type": "object"}}},
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || !requirements.AllowsKiro() {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
	payload := ClaudeToKiro(&req, false)
	history := payload.ConversationState.History
	var assistant *KiroAssistantResponseMessage
	for i := range history {
		if history[i].AssistantResponseMessage != nil {
			assistant = history[i].AssistantResponseMessage
		}
	}
	if assistant == nil {
		t.Fatalf("history=%#v", history)
	}
	if got := assistant.ClaudeContentBlocks; len(got) != 0 {
		t.Fatalf("ordinary assistant turn retained duplicate sidecar: %#v", got)
	}
}

func TestCompletionGuardRecoveryStartsValidSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	sse, ok := newCompletionGuardSSE(recorder)
	if !ok {
		t.Fatal("SSE writer requires flusher")
	}
	if err := writer.ensureRecoveryStreamStarted(sse); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content-type=%q", got)
	}
	if !strings.Contains(recorder.Body.String(), "event: message_start") {
		t.Fatalf("missing synthesized message_start: %s", recorder.Body.String())
	}
}

func TestCompletionGuardStreamingWriterDropsConsumedPendingCapacity(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	frame := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n"
	if _, err := writer.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	if writer.pending.Len() != 0 || writer.pending.Cap() != 0 {
		t.Fatalf("consumed pending buffer retained capacity: len=%d cap=%d", writer.pending.Len(), writer.pending.Cap())
	}
}

func TestCompletionGuardStreamingWriterUsesSharedRetainedBudget(t *testing.T) {
	t.Setenv(completionGuardMaxBufferedEnv, "1048576")
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	data := bytes.Repeat([]byte("x"), 600*1024)
	if _, err := writer.Write(data); !errors.Is(err, errCompletionGuardBufferLimit) {
		t.Fatalf("write error=%v", err)
	}
	if writer.rawBody.Len() == 0 || writer.bufferedBytes == 0 {
		t.Fatalf("expected retained pre-message body before release: bytes=%d raw=%d", writer.bufferedBytes, writer.rawBody.Len())
	}
	writer.release()
	if writer.bufferedBytes != 0 || writer.semanticBytes != 0 || writer.rawBody.Len() != 0 || writer.pending.Len() != 0 {
		t.Fatalf("buffers not released: buffered=%d semantic=%d raw=%d pending=%d", writer.bufferedBytes, writer.semanticBytes, writer.rawBody.Len(), writer.pending.Len())
	}
}

func TestCompletionGuardToolInputBuilderParticipatesInSharedBudget(t *testing.T) {
	t.Setenv(completionGuardMaxBufferedEnv, "1048576")
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"read\",\"input\":{}}}\n\n",
	)
	before := writer.bufferedBytes
	writeCompletionGuardTestFrames(t, writer,
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}\n\n",
	)
	if writer.bufferedBytes <= before {
		t.Fatalf("tool input builder was not included in retained-byte accounting: before=%d after=%d", before, writer.bufferedBytes)
	}
	writeCompletionGuardTestFrames(t, writer,
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	)
	if writer.openBlocks[0] != nil {
		t.Fatalf("closed tool block still retained: %#v", writer.openBlocks[0])
	}
	writer.discardHeldFrames()
	if writer.bufferedBytes != writer.startFrame.Len() {
		t.Fatalf(
			"closed tool builder retained bytes: accounted=%d startFrame=%d",
			writer.bufferedBytes,
			writer.startFrame.Len(),
		)
	}
}

func TestCompletionGuardEmptyResponseDoesNotForceTool(t *testing.T) {
	plan := agentCompletionGuardPlan{ToolChoiceKind: "auto"}
	if completionGuardRecoveryRequiresTool("empty_response_without_tool", plan, 0) {
		t.Fatal("empty response without tools must not force a tool call")
	}
	next, err := buildCompletionGuardStreamRecoveryRequest(
		ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "hello"}}},
		ClaudeResponse{},
		nil,
		"empty_response_without_tool",
		plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	prompt, _ := next.Messages[len(next.Messages)-1].Content.(string)
	if strings.Contains(prompt, "Call an appropriate real provided tool now") {
		t.Fatalf("empty-response prompt forced a tool: %s", prompt)
	}
}

func TestCompletionGuardAuditContinuationRequiresAcceptedRealTool(t *testing.T) {
	for _, outcome := range []completionGuardOutcome{
		{Accepted: false, Kind: "real_tool", RealTools: 1},
		{Accepted: true, Kind: "violation", RealTools: 1},
		{Accepted: true, Kind: "real_tool", RealTools: 0},
	} {
		if completionGuardAcceptAuditContinuation(outcome) {
			t.Fatalf("invalid continuation accepted: %#v", outcome)
		}
	}
	if !completionGuardAcceptAuditContinuation(completionGuardOutcome{
		Accepted:  true,
		Kind:      "real_tool",
		RealTools: 1,
	}) {
		t.Fatal("valid audit continuation rejected")
	}
}

func TestCompletionGuardDuplicateToolIDsRejected(t *testing.T) {
	readTool := ClaudeTool{Name: "read", InputSchema: map[string]interface{}{"type": "object"}}
	plan := completionGuardTestPlanWithTools(
		"auto",
		readTool,
		ClaudeTool{Type: "web_search_20250305", Name: "web_search"},
	)

	tests := []struct {
		name     string
		content  []ClaudeContentBlock
		expected map[string]string
		reason   string
	}{
		{
			name: "duplicate client id",
			content: []ClaudeContentBlock{
				{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{}},
				{Type: "tool_use", ID: "tool-1", Name: "read", Input: map[string]interface{}{}},
			},
			reason: "duplicate_client_tool_id",
		},
		{
			name: "duplicate server id",
			content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "srv-1", Name: "web_search", Input: map[string]interface{}{}},
				{Type: "server_tool_use", ID: "srv-1", Name: "web_search", Input: map[string]interface{}{}},
			},
			reason: "duplicate_server_tool_id",
		},
		{
			name: "client server collision",
			content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "same-1", Name: "web_search", Input: map[string]interface{}{}},
				{Type: "tool_use", ID: "same-1", Name: "read", Input: map[string]interface{}{}},
			},
			reason: "tool_id_kind_collision",
		},
		{
			name: "duplicate server result",
			content: []ClaudeContentBlock{
				{Type: "web_search_tool_result", ToolUseID: "srv-prev", Content: "one"},
				{Type: "web_search_tool_result", ToolUseID: "srv-prev", Content: "two"},
			},
			expected: map[string]string{"srv-prev": "web_search"},
			reason:   "duplicate_server_tool_result",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyCompletionGuardResponseWithExpectedServerTools(
				&ClaudeResponse{StopReason: "tool_use", Content: tc.content},
				plan,
				tc.expected,
			)
			if outcome.Accepted || outcome.Reason != tc.reason {
				t.Fatalf("outcome=%#v, want %q", outcome, tc.reason)
			}
		})
	}
}

func TestCompletionGuardSpecificServerToolChoiceIsRecognized(t *testing.T) {
	plan := completionGuardTestPlanWithTools(
		"specific",
		ClaudeTool{Type: "web_search_20250305", Name: "web_search"},
	)
	plan.RequiredToolName = "web_search"
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "srv-1", Name: "web_search", Input: map[string]interface{}{"query": "go"}},
				{Type: "web_search_tool_result", ToolUseID: "srv-1", Content: "result"},
			},
		},
		plan,
	)
	if !outcome.Accepted || !outcome.RequiredMatch || outcome.Kind != "server_tool" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardRejectsClientToolResultInAssistantResponse(t *testing.T) {
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{
					Type:      "tool_result",
					ToolUseID: "tool-1",
					Content:   "unexpected",
				},
			},
		},
		agentCompletionGuardPlan{},
	)
	if outcome.Accepted || outcome.Reason != "client_tool_result_in_assistant_response" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardMCPToolRequiresServerName(t *testing.T) {
	withoutServer := ClaudeContentBlock{
		Type:  "mcp_tool_use",
		ID:    "mcp-1",
		Name:  "search",
		Input: map[string]interface{}{"query": "go"},
	}
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content:    []ClaudeContentBlock{withoutServer},
		},
		agentCompletionGuardPlan{},
	)
	if outcome.Accepted || outcome.Reason != "mcp_tool_without_server_name" {
		t.Fatalf("missing server_name outcome=%#v", outcome)
	}

	withServer := withoutServer
	withServer.Extra = map[string]json.RawMessage{
		"server_name": json.RawMessage(`"example-mcp"`),
	}
	outcome = classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{
				withServer,
				{
					Type:  "tool_use",
					ID:    "client-1",
					Name:  "read",
					Input: map[string]interface{}{},
				},
			},
		},
		completionGuardTestPlanWithMCP(
			"auto",
			"example-mcp",
			ClaudeTool{Name: "read"},
		),
	)
	if !outcome.Accepted || outcome.Kind != "real_tool" || outcome.PendingServerTools != 1 {
		t.Fatalf("valid mixed MCP outcome=%#v", outcome)
	}
}

func TestCompletionGuardRequiredInputRejectsNull(t *testing.T) {
	plan := completionGuardTestPlanWithTools("auto", ClaudeTool{
		Name: "read",
		InputSchema: map[string]interface{}{
			"type":     "object",
			"required": []interface{}{"path"},
		},
	})
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "tool_use",
			Content: []ClaudeContentBlock{
				{
					Type:  "tool_use",
					ID:    "tool-1",
					Name:  "read",
					Input: map[string]interface{}{"path": nil},
				},
			},
		},
		plan,
	)
	if outcome.Accepted || outcome.Reason != "client_tool_missing_required_input" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardToolChoiceRequestValidation(t *testing.T) {
	t.Setenv(completionGuardEnv, "strict")

	missing := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools:      []ClaudeTool{{Name: "read"}},
		ToolChoice: map[string]interface{}{"type": "tool"},
	}, false)
	if !strings.Contains(missing.ValidationReason, "non-empty tool name") {
		t.Fatalf("missing-name validation=%q", missing.ValidationReason)
	}

	unknown := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools:      []ClaudeTool{{Name: "read"}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "write"},
	}, false)
	if !strings.Contains(unknown.ValidationReason, "unknown tool") {
		t.Fatalf("unknown-tool validation=%q", unknown.ValidationReason)
	}

	malformedType := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools:      []ClaudeTool{{Name: "read"}},
		ToolChoice: map[string]interface{}{"type": 7},
	}, false)
	if !strings.Contains(malformedType.ValidationReason, "must be a string") {
		t.Fatalf("malformed-type validation=%q", malformedType.ValidationReason)
	}

	unsupportedType := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools:      []ClaudeTool{{Name: "read"}},
		ToolChoice: map[string]interface{}{"type": "future"},
	}, false)
	if !strings.Contains(unsupportedType.ValidationReason, "unsupported") {
		t.Fatalf("unsupported-type validation=%q", unsupportedType.ValidationReason)
	}

	missingType := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools:      []ClaudeTool{{Name: "read"}},
		ToolChoice: map[string]interface{}{"name": "read"},
	}, false)
	if !strings.Contains(missingType.ValidationReason, "type is required") {
		t.Fatalf("missing-type validation=%q", missingType.ValidationReason)
	}

	duplicateTool := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Name: "read"}, {Name: "read"}},
	}, false)
	if !strings.Contains(duplicateTool.ValidationReason, "duplicate tool name") {
		t.Fatalf("duplicate-tool validation=%q", duplicateTool.ValidationReason)
	}

	unnamedTool := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "custom"}},
	}, false)
	if !strings.Contains(unnamedTool.ValidationReason, "name must be non-empty") {
		t.Fatalf("unnamed-tool validation=%q", unnamedTool.ValidationReason)
	}

	mcpMissingServer := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "mcp_toolset"}},
	}, false)
	if !strings.Contains(mcpMissingServer.ValidationReason, "mcp_server_name") {
		t.Fatalf("MCP missing-server validation=%q", mcpMissingServer.ValidationReason)
	}

	mcpAny := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Extra: map[string]json.RawMessage{
			"mcp_servers": json.RawMessage(`[
				{"type":"url","url":"https://mcp.example/sse","name":"example-mcp"}
			]`),
		},
		Tools: []ClaudeTool{
			{
				Type: "mcp_toolset",
				Extra: map[string]json.RawMessage{
					"mcp_server_name": json.RawMessage(`"example-mcp"`),
				},
			},
		},
		ToolChoice: map[string]interface{}{"type": "any"},
	}, false)
	if mcpAny.ValidationReason != "" {
		t.Fatalf("MCP any validation=%q", mcpAny.ValidationReason)
	}

	forcedChoice := map[string]interface{}{"type": "any"}
	manualThinking := &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 1024}
	if got := completionGuardValidateThinkingToolChoice(
		manualThinking,
		forcedChoice,
		true,
		false,
	); !strings.Contains(got, "manual thinking") {
		t.Fatalf("manual-thinking validation=%q", got)
	}
	if got := completionGuardValidateThinkingToolChoice(
		manualThinking,
		forcedChoice,
		false,
		true,
	); got != "" {
		t.Fatalf("adaptive mapping rejected=%q", got)
	}
	if got := completionGuardValidateThinkingToolChoice(
		&ClaudeThinkingConfig{Type: "adaptive"},
		forcedChoice,
		false,
		true,
	); got != "" {
		t.Fatalf("adaptive thinking rejected=%q", got)
	}

	withoutTools := buildAgentCompletionGuardPlan(&ClaudeRequest{
		ToolChoice: map[string]interface{}{"type": "any"},
	}, false)
	if !strings.Contains(withoutTools.ValidationReason, "at least one tool") {
		t.Fatalf("tool-less validation=%q", withoutTools.ValidationReason)
	}

	valid := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools:      []ClaudeTool{{Name: "read"}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "read"},
	}, false)
	if valid.ValidationReason != "" {
		t.Fatalf("valid choice rejected: %q", valid.ValidationReason)
	}
}

func TestCompletionGuardResponseEnvelopeAndStopSequenceValidation(t *testing.T) {
	sequence := "END"
	tests := []struct {
		name     string
		response ClaudeResponse
		reason   string
	}{
		{
			name:     "invalid type",
			response: ClaudeResponse{Type: "error", Role: "assistant", StopReason: "end_turn", Content: []ClaudeContentBlock{{Type: "text", Text: "x"}}},
			reason:   "invalid_response_type",
		},
		{
			name:     "invalid role",
			response: ClaudeResponse{Type: "message", Role: "user", StopReason: "end_turn", Content: []ClaudeContentBlock{{Type: "text", Text: "x"}}},
			reason:   "invalid_response_role",
		},
		{
			name:     "missing sequence",
			response: ClaudeResponse{StopReason: "stop_sequence", Content: []ClaudeContentBlock{{Type: "text", Text: "x"}}},
			reason:   "missing_stop_sequence",
		},
		{
			name:     "unexpected sequence",
			response: ClaudeResponse{StopReason: "end_turn", StopSequence: &sequence, Content: []ClaudeContentBlock{{Type: "text", Text: "x"}}},
			reason:   "unexpected_stop_sequence",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(&tc.response, agentCompletionGuardPlan{})
			if outcome.Accepted || outcome.Reason != tc.reason {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}

	valid := classifyCompletionGuardResponse(
		&ClaudeResponse{
			Type:         "message",
			Role:         "assistant",
			StopReason:   "stop_sequence",
			StopSequence: &sequence,
			Content:      []ClaudeContentBlock{{Type: "text", Text: "x"}},
		},
		agentCompletionGuardPlan{},
	)
	if !valid.Accepted || valid.Kind != "prose" {
		t.Fatalf("valid outcome=%#v", valid)
	}
}

func TestCompletionGuardIntegerIndexRejectsFractionalAndOverflow(t *testing.T) {
	for _, value := range []interface{}{
		0.9,
		-0.1,
		math.Inf(1),
		json.Number("1.5"),
		json.Number("999999999999999999999999"),
	} {
		if got := completionGuardInt(value); got != -1 {
			t.Fatalf("completionGuardInt(%v)=%d, want -1", value, got)
		}
	}
	if got := completionGuardInt(float64(7)); got != 7 {
		t.Fatalf("integer float=%d", got)
	}
}

func TestCompletionGuardOverlapDoesNotTrimCoincidentalShortMatch(t *testing.T) {
	if got := trimCompletionGuardOverlap("alpha", "and beta"); got != "and beta" {
		t.Fatalf("coincidental one-rune overlap trimmed: %q", got)
	}
	if got := trimCompletionGuardOverlap("The root cause is", "root cause is fixed"); got != " fixed" {
		t.Fatalf("meaningful overlap not trimmed: %q", got)
	}
}

func TestCompletionGuardRecoveryUsesPendingIDsFromBuiltRequest(t *testing.T) {
	candidate := ClaudeResponse{Content: []ClaudeContentBlock{
		{Type: "server_tool_use", ID: "srv-new", Name: "web_search", Input: map[string]interface{}{"query": "go"}},
	}}
	next, err := buildCompletionGuardStreamRecoveryRequest(
		ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "search"}}},
		candidate,
		candidate.Content,
		"pause_turn_requires_continuation",
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := completionGuardPendingServerTools(next)
	if expected["srv-new"] != "web_search" {
		t.Fatalf("pending IDs=%v", expected)
	}
	outcome := classifyCompletionGuardResponseWithExpectedServerTools(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "web_search_tool_result", ToolUseID: "srv-new", Content: "ok"},
				{Type: "text", Text: "Done."},
			},
		},
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
		expected,
	)
	if !outcome.Accepted || outcome.UnmatchedServerToolResults != 0 {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardStructuredTurnIsAtomic(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srv-1\",\"name\":\"web_search\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	)
	if strings.Contains(recorder.Body.String(), "server_tool_use") {
		t.Fatalf("server tool leaked before turn validation: %s", recorder.Body.String())
	}
	if writer.heldFrames.Len() == 0 {
		t.Fatal("server tool frames were not retained atomically")
	}

	writeCompletionGuardTestFrames(t, writer,
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"read\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{broken\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
	)
	if writer.protocolReason() != "invalid_tool_input" {
		t.Fatalf("reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
	}
	writer.discardHeldFrames()
	if strings.Contains(recorder.Body.String(), "server_tool_use") || writer.heldFrames.Len() != 0 {
		t.Fatalf("invalid mixed turn leaked frames: body=%s held=%d", recorder.Body.String(), writer.heldFrames.Len())
	}
}

func TestCompletionGuardRecoveryHistoryExcludesHeldText(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"visible\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"read\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\",\"text\":\"hidden-after-tool\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
	)

	content := writer.takeAssistantContent(false)
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("visible")) || bytes.Contains(encoded, []byte("hidden-after-tool")) || bytes.Contains(encoded, []byte("tool-1")) {
		t.Fatalf("recovery history contains uncommitted content: %s", encoded)
	}
}

func TestClaudeRequestAncillaryMetadataUsesExplicitKiroProjection(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":128,
		"messages":[{"role":"user","content":"hello"}],
		"metadata":{"user_id":"u-1"},
		"service_tier":"auto"
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	semantic, reason := buildClaudeSemanticRequest(&req, nil, raw)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	wantMetadata := claudeTransportMaskKiro | claudeTransportMaskCustom
	if validationErr != nil || requirements.Allowed != wantMetadata || !requirements.AllowsKiro() {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}

	stopRaw, _ := json.Marshal([]string{"DONE"})
	req.Extra = map[string]json.RawMessage{"stop_sequences": stopRaw}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	semantic, reason = buildClaudeSemanticRequest(&req, nil, encoded)
	if reason != "" {
		t.Fatal(reason)
	}
	requirements, validationErr = analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	want := claudeTransportMaskKiro | claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke | claudeTransportMaskBedrockConverse
	if validationErr != nil || requirements.Allowed != want {
		t.Fatalf("stop requirements=%#v validation=%v", requirements, validationErr)
	}
}

func TestCompletionGuardTakeResponseTransfersOwnership(t *testing.T) {
	writer := &completionGuardStreamingWriter{
		candidate:             ClaudeResponse{Content: []ClaudeContentBlock{{Type: "text", Text: "hello"}}},
		rawContent:            map[int]map[string]interface{}{0: {"type": "text", "text": "hello"}},
		maxIndex:              0,
		highestForwardedIndex: 0,
	}
	response := writer.takeResponse()
	if len(response.Content) != 1 || len(writer.candidate.Content) != 0 {
		t.Fatalf("response=%#v writer=%#v", response, writer.candidate)
	}
	content := writer.takeAssistantContent(false)
	if content == nil || len(writer.rawContent) != 0 {
		t.Fatalf("content=%#v raw=%#v", content, writer.rawContent)
	}
}

func TestCompletionGuardValidMixedTurnFlushesAtomicallyAndReleasesBuffers(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srv-1\",\"name\":\"web_search\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"read\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":4}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	candidate := writer.response()
	outcome := classifyCompletionGuardResponse(&candidate, completionGuardTestPlanWithTools(
		"auto",
		ClaudeTool{Name: "read", InputSchema: map[string]interface{}{"type": "object"}},
		ClaudeTool{Type: "web_search_20250305", Name: "web_search"},
	))
	if !writer.protocolComplete() || !outcome.Accepted || outcome.Kind != "real_tool" {
		t.Fatalf("complete=%t outcome=%#v err=%v", writer.protocolComplete(), outcome, writer.protocolErr)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("mixed turn leaked before acceptance: %s", recorder.Body.String())
	}
	if err := writer.flushHeldFrames(); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	serverPos := strings.Index(body, "server_tool_use")
	clientPos := strings.Index(body, `"id":"tool-1"`)
	if serverPos < 0 || clientPos < 0 || serverPos > clientPos {
		t.Fatalf("mixed turn order was not preserved: %s", body)
	}
	if writer.heldFrames.Len() != 0 || writer.heldFrames.Cap() != 0 || writer.bufferedBytes != 0 {
		t.Fatalf("retained frames not released: heldLen=%d heldCap=%d buffered=%d", writer.heldFrames.Len(), writer.heldFrames.Cap(), writer.bufferedBytes)
	}
}

func TestCompletionGuardRejectedRecoveryDoesNotAccumulateHiddenContent(t *testing.T) {
	candidate := ClaudeResponse{Content: []ClaudeContentBlock{{Type: "text", Text: "visible"}}}
	var assistantContent interface{} = []ClaudeContentBlock{{Type: "text", Text: "visible"}}
	recovery := ClaudeResponse{Content: []ClaudeContentBlock{
		{Type: "text", Text: "never-emitted"},
		{Type: "tool_use", ID: "broken", Name: "read", Input: map[string]interface{}{}},
	}}
	completionGuardAdvanceRejectedRecovery(
		&candidate,
		&assistantContent,
		recovery,
		completionGuardOutcome{Reason: "invalid_tool_input"},
	)
	encoded, _ := json.Marshal(assistantContent)
	if bytes.Contains(encoded, []byte("never-emitted")) || len(candidate.Content) != 1 {
		t.Fatalf("hidden recovery accumulated: candidate=%#v content=%s", candidate, encoded)
	}

	pause := ClaudeResponse{Content: []ClaudeContentBlock{{
		Type: "server_tool_use", ID: "srv-2", Name: "web_search", Input: map[string]interface{}{"query": "go"},
	}}}
	completionGuardAdvanceRejectedRecovery(
		&candidate,
		&assistantContent,
		pause,
		completionGuardOutcome{Reason: "pause_turn_requires_continuation"},
	)
	encoded, _ = json.Marshal(assistantContent)
	if !bytes.Contains(encoded, []byte("srv-2")) || candidate.Content[0].ID != "srv-2" {
		t.Fatalf("pause_turn state was not preserved: candidate=%#v content=%s", candidate, encoded)
	}
}

func TestCompletionGuardRetryRequestNeverPreservesHiddenRejectedText(t *testing.T) {
	current := ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "Finish the task."}}}
	resp := &ClaudeResponse{
		StopReason: "end_turn",
		Content:    []ClaudeContentBlock{{Type: "text", Text: "hidden partial"}},
	}
	next, err := completionGuardRetryRequest(
		current,
		resp,
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
		"empty_response_without_tool",
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(next.Messages)
	if bytes.Contains(encoded, []byte("hidden partial")) {
		t.Fatalf("hidden rejected text entered retry history: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte("complete replacement response from the beginning")) {
		t.Fatalf("retry prompt does not require a complete rewrite: %s", encoded)
	}
}

func TestCompletionGuardCompactionLifecycle(t *testing.T) {
	plan := agentCompletionGuardPlan{ToolChoiceKind: "auto"}
	valid := classifyCompletionGuardResponse(&ClaudeResponse{
		Type:       "message",
		Role:       "assistant",
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "compaction", Content: "<summary>state</summary>"},
			{Type: "text", Text: "Finished after compaction."},
		},
	}, plan)
	if !valid.Accepted || valid.Kind != "prose" || valid.Compactions != 1 {
		t.Fatalf("valid compaction outcome=%#v", valid)
	}

	paused := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "compaction",
		Content: []ClaudeContentBlock{{
			Type: "compaction", Content: "<summary>paused</summary>",
		}},
	}, plan)
	if paused.Accepted || paused.Reason != "compaction_requires_continuation" {
		t.Fatalf("paused compaction outcome=%#v", paused)
	}

	onlyMetadata := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{{
			Type: "compaction", Content: "<summary>only</summary>",
		}},
	}, plan)
	if onlyMetadata.Accepted || onlyMetadata.Reason != "compaction_without_completion" {
		t.Fatalf("compaction-only outcome=%#v", onlyMetadata)
	}

	for name, response := range map[string]ClaudeResponse{
		"null content": {
			StopReason: "end_turn",
			Content:    []ClaudeContentBlock{{Type: "compaction", Content: nil}},
		},
		"not first": {
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "text", Text: "before"},
				{Type: "compaction", Content: "summary"},
			},
		},
		"compaction stop with text": {
			StopReason: "compaction",
			Content: []ClaudeContentBlock{
				{Type: "compaction", Content: "summary"},
				{Type: "text", Text: "unexpected"},
			},
		},
		"unknown block": {
			StopReason: "end_turn",
			Content:    []ClaudeContentBlock{{Type: "future_block"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(&response, plan)
			if outcome.Accepted || outcome.Reason == "" {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}
}

func TestCompletionGuardStreamingCompactionIsAtomicAndReplayable(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"compaction\",\"content\":\"\",\"cache_control\":{\"type\":\"ephemeral\"}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"compaction_delta\",\"content\":\"<summary>state</summary>\"}}\n\n",
	)
	if recorder.Body.Len() != 0 {
		t.Fatalf("incomplete compaction block leaked: %s", recorder.Body.String())
	}
	writeCompletionGuardTestFrames(t, writer,
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":4,\"iterations\":[{\"type\":\"compaction\",\"input_tokens\":100,\"output_tokens\":20},{\"type\":\"message\",\"input_tokens\":10,\"output_tokens\":4}]}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	candidate := writer.response()
	outcome := classifyCompletionGuardResponse(&candidate, agentCompletionGuardPlan{ToolChoiceKind: "auto"})
	if !writer.protocolComplete() || !outcome.Accepted || outcome.Kind != "prose" {
		t.Fatalf("complete=%t outcome=%#v err=%v", writer.protocolComplete(), outcome, writer.protocolErr)
	}
	if len(candidate.Content) != 2 || candidate.Content[0].Content != "<summary>state</summary>" {
		t.Fatalf("candidate=%#v", candidate.Content)
	}
	encoded, _ := json.Marshal(candidate.Content[0])
	if !bytes.Contains(encoded, []byte(`"cache_control":{"type":"ephemeral"}`)) {
		t.Fatalf("compaction metadata lost: %s", encoded)
	}
	if writer.bufferedBytes != 0 {
		t.Fatalf("retained raw frame budget not released: %d", writer.bufferedBytes)
	}

	replayed := httptest.NewRecorder()
	sse, ok := newCompletionGuardSSE(replayed)
	if !ok {
		t.Fatal("SSE unavailable")
	}
	replayCompletionGuardSSE(sse, &candidate)
	body := replayed.Body.String()
	if !strings.Contains(body, `"type":"compaction_delta"`) ||
		!strings.Contains(body, `summary`) ||
		!strings.Contains(body, `"iterations"`) {
		t.Fatalf("compaction replay incomplete: %s", body)
	}
}

func TestCompletionGuardInvalidStreamingCompactionDoesNotLeak(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"compaction\",\"content\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"compaction_delta\",\"content\":null}}\n\n",
	)
	if writer.protocolErr == nil || recorder.Body.Len() != 0 {
		t.Fatalf("err=%v leaked=%s", writer.protocolErr, recorder.Body.String())
	}
}

func TestCompletionGuardExactCompactionContinuationPreservesRequestAndUsage(t *testing.T) {
	contextManagement := json.RawMessage(`{"edits":[{"type":"compact_20260112","pause_after_compaction":true}]}`)
	current := ClaudeRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 4096,
		Messages:  []ClaudeMessage{{Role: "user", Content: "continue task"}},
		Extra: map[string]json.RawMessage{
			"context_management": contextManagement,
			"betas":              json.RawMessage(`["compact-2026-01-12"]`),
		},
	}
	paused := &ClaudeResponse{
		StopReason: "compaction",
		Content: []ClaudeContentBlock{{
			Type: "compaction", Content: "<summary>state</summary>",
			Extra: map[string]json.RawMessage{"cache_control": json.RawMessage(`{"type":"ephemeral"}`)},
		}},
	}
	next, err := completionGuardExactContinuationRequest(
		current,
		paused,
		"compaction_requires_continuation",
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(next)
	for _, expected := range [][]byte{
		[]byte(`"context_management"`),
		[]byte(`"compact-2026-01-12"`),
		[]byte(`"type":"compaction"`),
		[]byte(`"cache_control":{"type":"ephemeral"}`),
	} {
		if !bytes.Contains(encoded, expected) {
			t.Fatalf("missing %s in %s", expected, encoded)
		}
	}

	prefixUsage := ClaudeUsage{
		InputTokens: 100, OutputTokens: 20,
		Extra: map[string]json.RawMessage{
			"iterations": json.RawMessage(`[{"type":"compaction","input_tokens":100,"output_tokens":20}]`),
		},
	}
	final := ClaudeResponse{
		Content:    []ClaudeContentBlock{{Type: "text", Text: "done"}},
		StopReason: "end_turn",
		Usage: ClaudeUsage{
			InputTokens: 10, OutputTokens: 4,
			Extra: map[string]json.RawMessage{
				"iterations": json.RawMessage(`[{"type":"message","input_tokens":10,"output_tokens":4}]`),
			},
		},
	}
	completionGuardApplyVisiblePrefix(&final, paused.Content, prefixUsage, true)
	if final.Usage.InputTokens != 110 || final.Usage.OutputTokens != 24 || len(final.Content) != 2 {
		t.Fatalf("merged final=%#v", final)
	}
	if !bytes.Contains(final.Usage.Extra["iterations"], []byte(`"compaction"`)) ||
		!bytes.Contains(final.Usage.Extra["iterations"], []byte(`"message"`)) {
		t.Fatalf("iterations=%s", final.Usage.Extra["iterations"])
	}
}

func TestCompletionGuardMCPServerAndToolNameValidation(t *testing.T) {
	toolset := ClaudeTool{
		Type: "mcp_toolset",
		Extra: map[string]json.RawMessage{
			"mcp_server_name": json.RawMessage(`"example-mcp"`),
		},
	}
	missingServer := buildAgentCompletionGuardPlan(&ClaudeRequest{Tools: []ClaudeTool{toolset}}, false)
	if !strings.Contains(missingServer.ValidationReason, "does not match mcp_servers") {
		t.Fatalf("missing server validation=%q", missingServer.ValidationReason)
	}

	validRequest := ClaudeRequest{
		Extra: map[string]json.RawMessage{
			"mcp_servers": json.RawMessage(`[{"type":"url","url":"https://mcp.example/sse","name":"example-mcp"}]`),
		},
		Tools: []ClaudeTool{toolset},
	}
	validPlan := buildAgentCompletionGuardPlan(&validRequest, false)
	if validPlan.ValidationReason != "" || !validPlan.Catalog.HasMCPToolset("example-mcp") {
		t.Fatalf("valid MCP plan=%#v", validPlan)
	}
	dynamicName := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{
				Type: "mcp_tool_use", ID: "mcp-dynamic", Name: "jira.search/issues", Input: map[string]interface{}{},
				Extra: map[string]json.RawMessage{"server_name": json.RawMessage(`"example-mcp"`)},
			},
			{Type: "mcp_tool_result", ToolUseID: "mcp-dynamic", Content: "ok"},
		},
	}, validPlan)
	if !dynamicName.Accepted || dynamicName.Kind != "server_tool" {
		t.Fatalf("dynamic MCP tool name rejected: %#v", dynamicName)
	}

	mcpOutcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{
				Type: "mcp_tool_use", ID: "mcp-1", Name: "lookup", Input: map[string]interface{}{},
				Extra: map[string]json.RawMessage{"server_name": json.RawMessage(`"other-mcp"`)},
			},
		},
	}, validPlan)
	if mcpOutcome.Reason != "unknown_mcp_server_tool" {
		t.Fatalf("MCP outcome=%#v", mcpOutcome)
	}

	for _, name := range []string{"bad name", strings.Repeat("a", 129), "tool!"} {
		plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
			Tools: []ClaudeTool{{Name: name}},
		}, false)
		if !strings.Contains(plan.ValidationReason, "must match") {
			t.Fatalf("name=%q validation=%q", name, plan.ValidationReason)
		}
	}
}

func TestCompletionGuardStreamingCitationsRoundTrip(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m-citations\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":8,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\",\"citations\":[]}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"The grass is green\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"char_location\",\"cited_text\":\"The grass is green.\",\"document_index\":0,\"document_title\":\"Example\",\"start_char_index\":0,\"end_char_index\":19}}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	candidate := writer.response()
	outcome := classifyCompletionGuardResponse(
		&candidate,
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if !writer.protocolComplete() || !outcome.Accepted {
		t.Fatalf(
			"complete=%t outcome=%#v protocolErr=%v",
			writer.protocolComplete(),
			outcome,
			writer.protocolErr,
		)
	}
	if len(candidate.Content) != 1 || candidate.Content[0].Text != "The grass is green" {
		t.Fatalf("candidate=%#v", candidate.Content)
	}
	var citations []map[string]interface{}
	if err := json.Unmarshal(candidate.Content[0].Extra["citations"], &citations); err != nil {
		t.Fatalf("citations metadata invalid: %v", err)
	}
	if len(citations) != 1 || citations[0]["type"] != "char_location" {
		t.Fatalf("citations=%#v", citations)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"citations_delta"`) {
		t.Fatalf("live stream lost citation delta: %s", recorder.Body.String())
	}

	replayed := httptest.NewRecorder()
	sse, ok := newCompletionGuardSSE(replayed)
	if !ok {
		t.Fatal("SSE unavailable")
	}
	replayCompletionGuardSSE(sse, &candidate)
	body := replayed.Body.String()
	for _, expected := range []string{
		`"citations":[]`,
		`"type":"citations_delta"`,
		`"cited_text":"The grass is green."`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("citation replay missing %s: %s", expected, body)
		}
	}
}

func TestCompletionGuardFallbackAndConnectorTextLifecycle(t *testing.T) {
	fallback := ClaudeContentBlock{
		Type: "fallback",
		Extra: map[string]json.RawMessage{
			"from": json.RawMessage(`{"model":"claude-fable-5"}`),
			"to":   json.RawMessage(`{"model":"claude-opus-4-8"}`),
		},
	}
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			Model:      "claude-opus-4-8",
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				fallback,
				{Type: "connector_text", Text: "Switching models. "},
				{Type: "text", Text: "Done."},
			},
		},
		agentCompletionGuardPlan{
			ToolChoiceKind: "auto",
			ResolvedModel:  "claude-fable-5",
		},
	)
	if !outcome.Accepted || outcome.Kind != "prose" || outcome.Fallbacks != 1 {
		t.Fatalf("fallback outcome=%#v", outcome)
	}

	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-fable-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()

	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m-fallback\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-fable-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"fallback\",\"from\":{\"model\":\"claude-fable-5\"},\"to\":{\"model\":\"claude-opus-4-8\"}}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"connector_text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Switching models. \"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"text_delta\",\"text\":\"Done.\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7,\"iterations\":[{\"type\":\"message\",\"model\":\"claude-fable-5\",\"output_tokens\":1},{\"type\":\"fallback_message\",\"model\":\"claude-opus-4-8\",\"output_tokens\":6}]}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	candidate := writer.response()
	streamOutcome := classifyCompletionGuardResponse(
		&candidate,
		agentCompletionGuardPlan{
			ToolChoiceKind: "auto",
			ResolvedModel:  "claude-fable-5",
		},
	)
	if !writer.protocolComplete() || !streamOutcome.Accepted || streamOutcome.Fallbacks != 1 {
		t.Fatalf(
			"complete=%t outcome=%#v protocolErr=%v candidate=%#v",
			writer.protocolComplete(),
			streamOutcome,
			writer.protocolErr,
			candidate,
		)
	}
	if len(candidate.Content) != 3 ||
		candidate.Content[0].Type != "fallback" ||
		candidate.Content[1].Type != "connector_text" ||
		candidate.Content[1].Text != "Switching models. " ||
		candidate.Content[2].Text != "Done." {
		t.Fatalf("candidate content=%#v", candidate.Content)
	}
	if !bytes.Contains(candidate.Content[0].Extra["from"], []byte(`"claude-fable-5"`)) ||
		!bytes.Contains(candidate.Content[0].Extra["to"], []byte(`"claude-opus-4-8"`)) {
		t.Fatalf("fallback metadata=%#v", candidate.Content[0].Extra)
	}
	if !bytes.Contains(candidate.Usage.Extra["iterations"], []byte(`"fallback_message"`)) {
		t.Fatalf("iterations=%s", candidate.Usage.Extra["iterations"])
	}

	replayed := httptest.NewRecorder()
	sse, ok := newCompletionGuardSSE(replayed)
	if !ok {
		t.Fatal("SSE unavailable")
	}
	replayCompletionGuardSSE(sse, &candidate)
	body := replayed.Body.String()
	for _, expected := range []string{
		`"type":"fallback"`,
		`"model":"claude-fable-5"`,
		`"model":"claude-opus-4-8"`,
		`"type":"connector_text"`,
		`"fallback_message"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("fallback replay missing %s: %s", expected, body)
		}
	}
}

func TestCompletionGuardFallbackRejectsInvalidShapeAndDelta(t *testing.T) {
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{{
				Type: "fallback",
				Extra: map[string]json.RawMessage{
					"from": json.RawMessage(`{"model":"claude-fable-5"}`),
				},
			}},
		},
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if outcome.Reason != "invalid_fallback_block" {
		t.Fatalf("invalid fallback outcome=%#v", outcome)
	}

	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-fable-5")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-fable-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"fallback\",\"from\":{\"model\":\"claude-fable-5\"},\"to\":{\"model\":\"claude-opus-4-8\"}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"invalid\"}}\n\n",
	)
	if writer.protocolErr == nil || recorder.Body.Len() != 0 {
		t.Fatalf("err=%v leaked=%s", writer.protocolErr, recorder.Body.String())
	}
}

func TestCompletionGuardFallbackRecoverySanitizesBoundaryHistory(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "partial answer"},
		map[string]interface{}{"type": "thinking", "thinking": "old", "signature": "sig-old"},
		map[string]interface{}{"type": "connector_text", "text": "old narration"},
		map[string]interface{}{"type": "server_tool_use", "id": "srv-paired", "name": "web_search", "input": map[string]interface{}{}},
		map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srv-paired", "content": "ok"},
		map[string]interface{}{"type": "server_tool_use", "id": "srv-unpaired", "name": "web_fetch", "input": map[string]interface{}{}},
		map[string]interface{}{"type": "tool_use", "id": "tool-old", "name": "read", "input": map[string]interface{}{}},
		map[string]interface{}{"type": "fallback", "from": map[string]interface{}{"model": "claude-fable-5"}, "to": map[string]interface{}{"model": "claude-opus-4-8"}},
		map[string]interface{}{"type": "thinking", "thinking": "new", "signature": "sig-new"},
		map[string]interface{}{"type": "connector_text", "text": "new narration"},
		map[string]interface{}{"type": "text", "text": "continued"},
	}

	filtered := completionGuardRecoveryAssistantContent(
		content,
		"invalid_stream_protocol",
	)
	encoded, err := json.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, expected := range []string{
		`partial answer`,
		`srv-paired`,
		`web_search_tool_result`,
		`"type":"fallback"`,
		`sig-new`,
		`new narration`,
		`continued`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("sanitized fallback history missing %q: %s", expected, text)
		}
	}
	for _, forbidden := range []string{
		`sig-old`,
		`old narration`,
		`srv-unpaired`,
		`tool-old`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sanitized fallback history retained %q: %s", forbidden, text)
		}
	}

	next, err := buildCompletionGuardStreamRecoveryRequest(
		ClaudeRequest{
			Model:    "claude-fable-5",
			Messages: []ClaudeMessage{{Role: "user", Content: "finish"}},
		},
		ClaudeResponse{},
		content,
		"invalid_stream_protocol",
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Messages) != 3 || next.Messages[1].Role != "assistant" || next.Messages[2].Role != "user" {
		t.Fatalf("recovery messages=%#v", next.Messages)
	}
	assistantJSON, _ := json.Marshal(next.Messages[1].Content)
	if !bytes.Contains(assistantJSON, []byte(`"type":"fallback"`)) ||
		!bytes.Contains(assistantJSON, []byte(`sig-new`)) {
		t.Fatalf("recovery lost fallback boundary=%s", assistantJSON)
	}
}

func TestCompletionGuardDropsIncompleteThinkingFromRecoveryHistory(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"unfinished\"}}\n\n",
	)
	if content := writer.takeAssistantContent(true); content != nil {
		t.Fatalf("incomplete thinking entered recovery history: %#v", content)
	}
}

func TestCompletionGuardRefusalDiscardsPartialOutputButKeepsFallbackBoundary(t *testing.T) {
	response := ClaudeResponse{
		Model:      "claude-opus-5",
		StopReason: "refusal",
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "partial and incomplete"},
			{
				Type: "fallback",
				Extra: map[string]json.RawMessage{
					"from": json.RawMessage(`{"model":"claude-fable-5"}`),
					"to":   json.RawMessage(`{"model":"claude-opus-5"}`),
				},
			},
			{Type: "connector_text", Text: "also partial"},
		},
	}
	outcome := classifyCompletionGuardResponse(
		&response,
		agentCompletionGuardPlan{
			ToolChoiceKind: "auto",
			ResolvedModel:  "claude-fable-5",
		},
	)
	if !outcome.Accepted || outcome.Kind != "refusal" || outcome.NaturalText != "" {
		t.Fatalf("refusal outcome=%#v", outcome)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "fallback" {
		t.Fatalf("refusal partial output was not discarded: %#v", response.Content)
	}
}

func TestCompletionGuardForcesTerminalBufferForRefusalCapableStreams(t *testing.T) {
	for _, test := range []struct {
		name  string
		req   ClaudeRequest
		model string
		want  bool
	}{
		{name: "fable", req: ClaudeRequest{Model: "claude-fable-5"}, want: true},
		{name: "opus", req: ClaudeRequest{Model: "provider/claude-opus-5"}, want: true},
		{
			name: "fallbacks",
			req: ClaudeRequest{
				Model: "claude-opus-4-8",
				Extra: map[string]json.RawMessage{
					"fallbacks": json.RawMessage(`"default"`),
				},
			},
			want: true,
		},
		{
			name: "null-fallbacks",
			req: ClaudeRequest{
				Model: "claude-opus-4-8",
				Extra: map[string]json.RawMessage{
					"fallbacks": json.RawMessage(`null`),
				},
			},
			want: true,
		},
		{name: "opus-4.8", req: ClaudeRequest{Model: "claude-opus-4-8"}, want: true},
		{name: "sonnet-4.6", req: ClaudeRequest{Model: "claude-sonnet-4-6"}, want: true},
		{name: "unknown-alias", req: ClaudeRequest{Model: "provider/custom-claude"}, want: true},
		{name: "known-pre-4", req: ClaudeRequest{Model: "claude-opus-3.5"}, want: false},
		{
			name: "known-pre-4-with-fallback",
			req: ClaudeRequest{
				Model: "claude-opus-3.5",
				Extra: map[string]json.RawMessage{
					"fallbacks": json.RawMessage(`[{"model":"claude-opus-4-8"}]`),
				},
			},
			want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := completionGuardRequiresTerminalBuffer(test.req, test.model); got != test.want {
				t.Fatalf("got=%t want=%t", got, test.want)
			}
		})
	}
}

func TestCompletionGuardRecoverableReasonTableCoversStructuralViolations(t *testing.T) {
	for _, reason := range []string{
		"missing_content_block_type",
		"unsupported_content_block",
		"invalid_content_block",
		"invalid_compaction_block",
		"invalid_fallback_block",
		"thinking_missing_content_field",
		"thinking_missing_signature",
		"redacted_thinking_missing_data",
		"invalid_thinking_block",
		"invalid_redacted_thinking_block",
		"compaction_not_at_start",
		"compaction_stop_without_block",
		"compaction_stop_with_other_content",
		"client_tool_invalid_name",
		"server_tool_invalid_name",
		"compaction_requires_continuation",
		"compaction_without_completion",
	} {
		if !completionGuardRecoverableReason(reason, false) {
			t.Fatalf("reason %q is not recoverable", reason)
		}
	}
	if completionGuardRecoverableReason("upstream_stream_error", false) {
		t.Fatal("non-retryable SSE error was marked recoverable")
	}
	if !completionGuardRecoverableReason("upstream_stream_error", true) {
		t.Fatal("retryable SSE error was not recoverable")
	}
}

func TestClaudeRequestClonePreservesKnownZeroValueFieldPresence(t *testing.T) {
	var request ClaudeRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"","signature":"sig-omitted"},
				{"type":"text","text":""}
			]},
			{"role":"user","content":"continue"}
		],
		"max_tokens":32,
		"temperature":0,
		"top_p":0,
		"stream":false,
		"system":null
	}`), &request); err != nil {
		t.Fatal(err)
	}

	cloned, err := cloneCompletionGuardRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range [][]byte{
		[]byte(`"temperature":0`),
		[]byte(`"top_p":0`),
		[]byte(`"stream":false`),
		[]byte(`"system":null`),
		[]byte(`"thinking":""`),
		[]byte(`"text":""`),
	} {
		if !bytes.Contains(encoded, fragment) {
			t.Fatalf("clone dropped %s: %s", fragment, encoded)
		}
	}

	payload := ClaudeToKiro(&cloned, false)
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(`"temperature":0`)) ||
		!bytes.Contains(wire, []byte(`"topP":0`)) {
		t.Fatalf("Kiro inference config dropped explicit zero: %s", wire)
	}
}

func TestCompletionGuardValidatesThinkingBlockIntegrity(t *testing.T) {
	var omitted ClaudeContentBlock
	if err := json.Unmarshal([]byte(`{
		"type":"thinking",
		"thinking":"",
		"signature":"sig-omitted"
	}`), &omitted); err != nil {
		t.Fatal(err)
	}
	accepted := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				omitted,
				{Type: "text", Text: "done"},
			},
		},
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if !accepted.Accepted {
		t.Fatalf("valid omitted thinking rejected: %#v", accepted)
	}

	for _, test := range []struct {
		name   string
		block  ClaudeContentBlock
		reason string
	}{
		{
			name:   "missing signature",
			block:  ClaudeContentBlock{Type: "thinking", Thinking: "summary"},
			reason: "thinking_missing_signature",
		},
		{
			name:   "redacted missing data",
			block:  ClaudeContentBlock{Type: "redacted_thinking"},
			reason: "redacted_thinking_missing_data",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := classifyCompletionGuardResponse(
				&ClaudeResponse{
					StopReason: "end_turn",
					Content: []ClaudeContentBlock{
						test.block,
						{Type: "text", Text: "done"},
					},
				},
				agentCompletionGuardPlan{ToolChoiceKind: "auto"},
			)
			if outcome.Accepted || outcome.Reason != test.reason {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}

	var missingThinkingField ClaudeContentBlock
	if err := json.Unmarshal([]byte(`{
		"type":"thinking",
		"signature":"sig"
	}`), &missingThinkingField); err != nil {
		t.Fatal(err)
	}
	outcome := classifyCompletionGuardResponse(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				missingThinkingField,
				{Type: "text", Text: "done"},
			},
		},
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if outcome.Accepted || outcome.Reason != "thinking_missing_content_field" {
		t.Fatalf("missing thinking field outcome=%#v", outcome)
	}
}

func TestCompletionGuardRejectsMalformedSSEEnvelopeTypes(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame string
	}{
		{
			name:  "content string",
			frame: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":\"invalid\",\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		},
		{
			name:  "stop reason number",
			frame: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":123,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		},
		{
			name:  "stop sequence object",
			frame: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":{},\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
			if !ok {
				t.Fatal("streaming writer requires flusher")
			}
			defer writer.release()
			writeCompletionGuardTestFrames(t, writer, test.frame)
			if writer.protocolErr == nil || writer.protocolReason() != "invalid_stream_protocol" {
				t.Fatalf("reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
			}
		})
	}
}

func TestCompletionGuardRejectsMalformedMessageDeltaTypes(t *testing.T) {
	for _, delta := range []string{
		`{"stop_reason":123,"stop_sequence":null}`,
		`{"stop_reason":"end_turn","stop_sequence":{}}`,
	} {
		recorder := httptest.NewRecorder()
		writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
		if !ok {
			t.Fatal("streaming writer requires flusher")
		}
		writeCompletionGuardTestFrames(t, writer,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
			fmt.Sprintf(
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":%s,\"usage\":{}}\n\n",
				delta,
			),
		)
		if writer.protocolErr == nil || writer.protocolReason() != "invalid_stream_protocol" {
			writer.release()
			t.Fatalf("delta=%s reason=%q err=%v", delta, writer.protocolReason(), writer.protocolErr)
		}
		writer.release()
	}
}

func TestCompletionGuardRejectsIncompleteOrDuplicateThinkingSignature(t *testing.T) {
	for _, test := range []struct {
		name   string
		frames []string
		reason string
	}{
		{
			name: "missing signature",
			frames: []string{
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			},
			reason: "invalid_thinking_block",
		},
		{
			name: "duplicate signature delta",
			frames: []string{
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-1\"}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-2\"}}\n\n",
			},
			reason: "invalid_stream_protocol",
		},
		{
			name: "signature supplied only in block start",
			frames: []string{
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"sig-start\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			},
			reason: "invalid_thinking_block",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
			if !ok {
				t.Fatal("streaming writer requires flusher")
			}
			defer writer.release()
			writeCompletionGuardTestFrames(t, writer,
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
			)
			writeCompletionGuardTestFrames(t, writer, test.frames...)
			if writer.protocolErr == nil || writer.protocolReason() != test.reason {
				t.Fatalf("reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
			}
		})
	}
}

func TestCompletionGuardFailsClosedOnUnknownStateBearingSSEEvent(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: future_state\ndata: {\"type\":\"future_state\",\"state\":{\"opaque\":true}}\n\n",
	)
	if writer.protocolErr == nil || writer.protocolReason() != "unsupported_stream_event" {
		t.Fatalf("reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
	}
}

func TestCompletionGuardAcceptsOmittedThinkingStreamingBlock(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-omitted\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if !writer.protocolComplete() {
		t.Fatalf("reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
	}
	candidate := writer.response()
	outcome := classifyCompletionGuardResponse(
		&candidate,
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if !outcome.Accepted {
		t.Fatalf("outcome=%#v candidate=%#v", outcome, candidate)
	}
	if len(candidate.Content) < 1 ||
		!candidate.Content[0].hasKnownJSONField("thinking") ||
		candidate.Content[0].Signature != "sig-omitted" {
		t.Fatalf("omitted thinking presence lost: %#v", candidate.Content)
	}
	if writer.heldFrames.Len() == 0 {
		t.Fatal("thinking turn was not held atomically until terminal validation")
	}
}

func TestCompletionGuardAllowsAnthropicPingEvent(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
	)
	if writer.protocolErr != nil {
		t.Fatalf("ping rejected: %v", writer.protocolErr)
	}
}

func TestCompletionGuardRejectsMalformedUsageEnvelopeTypes(t *testing.T) {
	for _, frame := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":[]}}\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":null}}\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":null}}}\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":[]}}\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":null}\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":\"invalid\"}}}\n\n",
	} {
		recorder := httptest.NewRecorder()
		writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
		if !ok {
			t.Fatal("streaming writer requires flusher")
		}
		if _, err := writer.Write([]byte(frame)); err != nil {
			writer.release()
			t.Fatal(err)
		}
		if writer.protocolErr == nil || writer.protocolReason() != "invalid_stream_protocol" {
			writer.release()
			t.Fatalf("reason=%q err=%v", writer.protocolReason(), writer.protocolErr)
		}
		writer.release()
	}
}

func TestClaudeJSONRejectsNullForNonNullableKnownFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-opus-4-8","messages":[],"max_tokens":1,"temperature":null}`,
		`{"model":"claude-opus-4-8","messages":[],"max_tokens":1,"top_p":null}`,
		`{"model":"claude-opus-4-8","messages":[],"max_tokens":1,"stream":null}`,
	} {
		var request ClaudeRequest
		if err := json.Unmarshal([]byte(body), &request); err == nil {
			t.Fatalf("request accepted non-nullable null: %s", body)
		}
	}

	for _, body := range []string{
		`{"type":"thinking","thinking":null,"signature":"sig"}`,
		`{"type":"text","text":null}`,
		`{"type":"redacted_thinking","data":null}`,
	} {
		var block ClaudeContentBlock
		if err := json.Unmarshal([]byte(body), &block); err == nil {
			t.Fatalf("content block accepted non-nullable null: %s", body)
		}
	}
}

func TestClaudeUsageRejectsNullKnownCounters(t *testing.T) {
	for _, body := range []string{
		`{"input_tokens":null,"output_tokens":0}`,
		`{"input_tokens":1,"output_tokens":null}`,
		`{"input_tokens":1,"output_tokens":0,"cache_read_input_tokens":null}`,
	} {
		var usage ClaudeUsage
		if err := json.Unmarshal([]byte(body), &usage); err == nil {
			t.Fatalf("usage accepted null counter: %s", body)
		}
	}
}

func TestCompletionGuardUnknownStateEventIsNotRecoverable(t *testing.T) {
	if completionGuardRecoverableReason("unsupported_stream_event", false) {
		t.Fatal("unknown state-bearing SSE events must fail closed instead of retrying without their semantics")
	}
}

func TestCompletionGuardReleaseClearsRetainedThinkingState(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-4-8")
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	writeCompletionGuardTestFrames(t, writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"held\"}}\n\n",
	)
	if writer.heldFrames.Len() == 0 || writer.bufferedBytes == 0 {
		writer.release()
		t.Fatal("test did not retain thinking frames before release")
	}
	writer.release()
	if writer.bufferedBytes != 0 || writer.semanticBytes != 0 ||
		writer.pending.Len() != 0 || writer.startFrame.Len() != 0 ||
		writer.heldFrames.Len() != 0 || len(writer.openBlocks) != 0 ||
		len(writer.rawContent) != 0 || len(writer.candidate.Content) != 0 {
		t.Fatalf(
			"release retained state: buffered=%d semantic=%d pending=%d start=%d held=%d open=%d raw=%d content=%d",
			writer.bufferedBytes,
			writer.semanticBytes,
			writer.pending.Len(),
			writer.startFrame.Len(),
			writer.heldFrames.Len(),
			len(writer.openBlocks),
			len(writer.rawContent),
			len(writer.candidate.Content),
		)
	}
}

func TestClaudeRequestJSONRoundTripPreservesGuardedProtocolShape(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"","signature":"sig-omitted"},
				{"type":"text","text":""},
				{"type":"server_tool_use","id":"srv-1","name":"web_search","input":{},"caller":{"type":"direct"}}
			]},
			{"role":"user","content":"continue"}
		],
		"max_tokens":64,
		"temperature":0,
		"top_p":0,
		"stream":false,
		"system":null,
		"output_config":{"effort":"low"},
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}],
		"tool_choice":{"type":"auto"},
		"metadata":{"user_id":"guard-roundtrip"},
		"stop_sequences":[]
	}`)

	var request ClaudeRequest
	if err := json.Unmarshal(input, &request); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	var before interface{}
	var after interface{}
	if err := json.Unmarshal(input, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("round-trip changed request\nbefore=%s\nafter=%s", input, encoded)
	}
}

func TestCompletionGuardValidatesStopDetailsConsistency(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		reason   string
		accepted bool
	}{
		{
			name: "refusal missing details",
			body: `{
				"id":"msg-1","type":"message","role":"assistant",
				"content":[],"model":"claude-fable-5",
				"stop_reason":"refusal","stop_sequence":null,
				"usage":{"input_tokens":1,"output_tokens":0}
			}`,
			reason: "missing_refusal_stop_details",
		},
		{
			name: "refusal wrong details type",
			body: `{
				"id":"msg-1","type":"message","role":"assistant",
				"content":[],"model":"claude-fable-5",
				"stop_reason":"refusal","stop_sequence":null,
				"stop_details":{"type":"other"},
				"usage":{"input_tokens":1,"output_tokens":0}
			}`,
			reason: "invalid_refusal_stop_details",
		},
		{
			name: "normal stop with refusal details",
			body: `{
				"id":"msg-1","type":"message","role":"assistant",
				"content":[{"type":"text","text":"done"}],
				"model":"claude-opus-5",
				"stop_reason":"end_turn","stop_sequence":null,
				"stop_details":{"type":"refusal"},
				"usage":{"input_tokens":1,"output_tokens":1}
			}`,
			reason: "unexpected_stop_details",
		},
		{
			name: "valid refusal",
			body: `{
				"id":"msg-1","type":"message","role":"assistant",
				"content":[],"model":"claude-fable-5",
				"stop_reason":"refusal","stop_sequence":null,
				"stop_details":{
					"type":"refusal",
					"category":null,
					"explanation":null,
					"future_field":{"preserved":true}
				},
				"usage":{"input_tokens":1,"output_tokens":0}
			}`,
			accepted: true,
		},
		{
			name: "valid normal null details",
			body: `{
				"id":"msg-1","type":"message","role":"assistant",
				"content":[{"type":"text","text":"done"}],
				"model":"claude-opus-5",
				"stop_reason":"end_turn","stop_sequence":null,
				"stop_details":null,
				"usage":{"input_tokens":1,"output_tokens":1}
			}`,
			accepted: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response ClaudeResponse
			if err := json.Unmarshal(
				[]byte(test.body),
				&response,
			); err != nil {
				t.Fatal(err)
			}
			outcome := classifyCompletionGuardResponse(
				&response,
				agentCompletionGuardPlan{
					ToolChoiceKind: "auto",
				},
			)
			if outcome.Accepted != test.accepted ||
				outcome.Reason != test.reason {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}
}

func TestCompletionGuardAllowsTerminalRefusalStopDetailsTransition(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-fable-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-fable-5\",\"stop_reason\":null,\"stop_sequence\":null,\"stop_details\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null,\"stop_sequence\":null,\"stop_details\":null},\"usage\":{\"output_tokens\":0}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_sequence\":null,\"stop_details\":{\"type\":\"refusal\",\"category\":null,\"explanation\":null}},\"usage\":{\"output_tokens\":0}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if !writer.protocolComplete() {
		t.Fatalf(
			"valid stop_details transition rejected: reason=%q err=%v",
			writer.protocolReason(),
			writer.protocolErr,
		)
	}
	response := writer.response()
	outcome := classifyCompletionGuardResponse(
		&response,
		agentCompletionGuardPlan{ToolChoiceKind: "auto"},
	)
	if !outcome.Accepted || outcome.Kind != "refusal" {
		t.Fatalf("outcome=%#v response=%#v", outcome, response)
	}
}

func TestCompletionGuardRejectsTextBlocksMissingWireText(t *testing.T) {
	for _, blockType := range []string{"text", "connector_text"} {
		t.Run(blockType, func(t *testing.T) {
			body := fmt.Sprintf(`{
				"id":"msg-1","type":"message","role":"assistant",
				"content":[{"type":%q}],
				"model":"claude-opus-5",
				"stop_reason":"end_turn","stop_sequence":null,
				"usage":{"input_tokens":1,"output_tokens":1}
			}`, blockType)
			var response ClaudeResponse
			if err := json.Unmarshal(
				[]byte(body),
				&response,
			); err != nil {
				t.Fatal(err)
			}
			outcome := classifyCompletionGuardResponse(
				&response,
				agentCompletionGuardPlan{
					ToolChoiceKind: "auto",
				},
			)
			if outcome.Accepted ||
				outcome.Reason !=
					blockType+"_missing_content_field" {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}
}

func TestCompletionGuardRejectsMalformedTextStartBeforeForwarding(t *testing.T) {
	for _, block := range []string{
		`{"type":"text"}`,
		`{"type":"connector_text","text":123}`,
	} {
		recorder := httptest.NewRecorder()
		writer, ok := newCompletionGuardStreamingWriter(
			recorder,
			"claude-opus-5",
		)
		if !ok {
			t.Fatal("streaming writer requires flusher")
		}
		writeCompletionGuardTestFrames(
			t,
			writer,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			fmt.Sprintf(
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":%s}\n\n",
				block,
			),
		)
		if writer.protocolErr == nil {
			writer.release()
			t.Fatalf("malformed block accepted: %s", block)
		}
		if recorder.Body.Len() != 0 {
			writer.release()
			t.Fatalf(
				"malformed start leaked downstream: %s",
				recorder.Body.String(),
			)
		}
		writer.release()
	}
}

func TestCompletionGuardRequiresCumulativeMessageDeltaUsage(t *testing.T) {
	for _, test := range []struct {
		name  string
		start int
		usage string
	}{
		{
			name:  "missing usage",
			start: 0,
			usage: "",
		},
		{
			name:  "missing output tokens",
			start: 0,
			usage: `,"usage":{"input_tokens":1}`,
		},
		{
			name:  "decreasing output tokens",
			start: 3,
			usage: `,"usage":{"output_tokens":2}`,
		},
		{
			name:  "decreasing input tokens",
			start: 1,
			usage: `,"usage":{"input_tokens":4,"output_tokens":2}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputTokens := 1
			if test.name == "decreasing input tokens" {
				inputTokens = 5
			}
			recorder := httptest.NewRecorder()
			writer, ok := newCompletionGuardStreamingWriter(
				recorder,
				"claude-opus-5",
			)
			if !ok {
				t.Fatal("streaming writer requires flusher")
			}
			writeCompletionGuardTestFrames(
				t,
				writer,
				fmt.Sprintf(
					"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}}\n\n",
					inputTokens,
					test.start,
				),
				fmt.Sprintf(
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}%s}\n\n",
					test.usage,
				),
			)
			if writer.protocolErr == nil {
				writer.release()
				t.Fatal("invalid cumulative usage was accepted")
			}
			writer.release()
		})
	}

	recorder := httptest.NewRecorder()
	writer, ok := newCompletionGuardStreamingWriter(
		recorder,
		"claude-opus-5",
	)
	if !ok {
		t.Fatal("streaming writer requires flusher")
	}
	defer writer.release()
	writeCompletionGuardTestFrames(
		t,
		writer,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null,\"stop_sequence\":null},\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if !writer.protocolComplete() ||
		writer.candidate.Usage.OutputTokens != 3 {
		t.Fatalf(
			"valid cumulative usage rejected: reason=%q err=%v usage=%#v",
			writer.protocolReason(),
			writer.protocolErr,
			writer.candidate.Usage,
		)
	}
}

func TestCompletionGuardPauseTurnSanitizesFallbackBoundary(t *testing.T) {
	fallbackFrom := json.RawMessage(
		`{"model":"claude-fable-5"}`,
	)
	fallbackTo := json.RawMessage(
		`{"model":"claude-opus-4-8"}`,
	)
	response := &ClaudeResponse{
		StopReason: "pause_turn",
		Content: []ClaudeContentBlock{
			{
				Type:      "thinking",
				Thinking:  "old thinking",
				Signature: "old-signature",
			},
			{
				Type: "connector_text",
				Text: "old connector narration",
			},
			{
				Type: "server_tool_use",
				ID:   "paired-server",
				Name: "web_search",
				Input: map[string]interface{}{
					"query": "go",
				},
			},
			{
				Type:      "web_search_tool_result",
				ToolUseID: "paired-server",
			},
			{
				Type: "server_tool_use",
				ID:   "unpaired-server",
				Name: "web_search",
				Input: map[string]interface{}{
					"query": "drop",
				},
			},
			{
				Type: "server_tool_use",
				ID:   "cross-boundary-server",
				Name: "web_search",
				Input: map[string]interface{}{
					"query": "keep",
				},
			},
			{
				Type:  "tool_use",
				ID:    "client-before",
				Name:  "read",
				Input: map[string]interface{}{},
			},
			{
				Type: "fallback",
				Extra: map[string]json.RawMessage{
					"from": fallbackFrom,
					"to":   fallbackTo,
				},
			},
			{
				Type:  "tool_use",
				ID:    "client-after",
				Name:  "read",
				Input: map[string]interface{}{},
			},
			{
				Type:      "web_search_tool_result",
				ToolUseID: "cross-boundary-server",
			},
			{
				Type:      "thinking",
				Thinking:  "new thinking",
				Signature: "new-signature",
			},
			{
				Type: "text",
				Text: "new answer",
			},
		},
	}

	next, err := completionGuardPauseTurnRequest(
		ClaudeRequest{
			Messages: []ClaudeMessage{{
				Role:    "user",
				Content: "continue",
			}},
		},
		response,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(
		next.Messages[len(next.Messages)-1],
	)
	for _, expected := range []string{
		"paired-server",
		"cross-boundary-server",
		`"type":"fallback"`,
		"client-after",
		"new thinking",
		"new answer",
	} {
		if !bytes.Contains(encoded, []byte(expected)) {
			t.Fatalf(
				"sanitized continuation lost %q: %s",
				expected,
				encoded,
			)
		}
	}
	for _, forbidden := range []string{
		"old thinking",
		"old connector narration",
		"unpaired-server",
		"client-before",
	} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf(
				"sanitized continuation retained %q: %s",
				forbidden,
				encoded,
			)
		}
	}
}

func TestCompletionGuardCodeExecutionCapabilityMapping(t *testing.T) {
	legacyPlan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "Run code."}},
		Tools: []ClaudeTool{{
			Type: completionGuardLegacyCodeExecutionType,
			Name: "code_execution",
		}},
	}, false)
	if legacyPlan.ValidationReason != "" {
		t.Fatalf("legacy validation=%q", legacyPlan.ValidationReason)
	}
	if _, ok := legacyPlan.Catalog.ServerCapability("code_execution"); !ok {
		t.Fatalf("legacy capability missing: %#v", legacyPlan.Catalog.ServerCapabilitiesByName)
	}
	for _, modernName := range []string{"bash_code_execution", "text_editor_code_execution"} {
		if _, ok := legacyPlan.Catalog.ServerCapability(modernName); ok {
			t.Fatalf("legacy request exposed modern capability %q", modernName)
		}
	}
	legacy := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "server_tool_use", ID: "legacy-1", Name: "code_execution", Input: map[string]interface{}{}},
			{Type: "code_execution_tool_result", ToolUseID: "legacy-1"},
		},
	}, legacyPlan)
	if !legacy.Accepted || legacy.Kind != "server_tool" {
		t.Fatalf("legacy outcome=%#v", legacy)
	}
	legacyRejectsModern := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{{
			Type: "server_tool_use", ID: "legacy-2", Name: "bash_code_execution", Input: map[string]interface{}{},
		}},
	}, legacyPlan)
	if legacyRejectsModern.Accepted || legacyRejectsModern.Reason != "unknown_server_tool" {
		t.Fatalf("legacy accepted modern wire shape: %#v", legacyRejectsModern)
	}

	for _, toolType := range []string{
		"code_execution_20250825",
		"code_execution_20260120",
		"code_execution_20260521",
	} {
		t.Run(toolType, func(t *testing.T) {
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Messages: []ClaudeMessage{{Role: "user", Content: "Run code."}},
				Tools:    []ClaudeTool{{Type: toolType, Name: "code_execution"}},
				ToolChoice: map[string]interface{}{
					"type": "tool",
					"name": "code_execution",
				},
			}, false)
			if plan.ValidationReason != "" {
				t.Fatalf("validation=%q", plan.ValidationReason)
			}
			programmaticCapability, hasProgrammaticCapability :=
				plan.Catalog.ServerCapability("code_execution")
			if toolType == "code_execution_20250825" {
				if hasProgrammaticCapability {
					t.Fatalf("20250825 exposed programmatic code-execution capability: %#v", programmaticCapability)
				}
			} else {
				if !hasProgrammaticCapability || !programmaticCapability.CanCallTools ||
					programmaticCapability.ResultBlockType != "code_execution_tool_result" {
					t.Fatalf("programmatic capability=%#v present=%t", programmaticCapability, hasProgrammaticCapability)
				}
				programmatic := classifyCompletionGuardResponse(&ClaudeResponse{
					StopReason: "end_turn",
					Content: []ClaudeContentBlock{
						{Type: "server_tool_use", ID: "srv-programmatic", Name: "code_execution", Input: map[string]interface{}{"code": "print(1)"}},
						{Type: "code_execution_tool_result", ToolUseID: "srv-programmatic"},
					},
				}, plan)
				if !programmatic.Accepted || programmatic.Kind != "server_tool" || !programmatic.RequiredMatch {
					t.Fatalf("programmatic outcome=%#v", programmatic)
				}
			}

			for _, tc := range []struct {
				name       string
				serverName string
				resultType string
			}{
				{name: "bash", serverName: "bash_code_execution", resultType: "bash_code_execution_tool_result"},
				{name: "text editor", serverName: "text_editor_code_execution", resultType: "text_editor_code_execution_tool_result"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					outcome := classifyCompletionGuardResponse(&ClaudeResponse{
						StopReason: "end_turn",
						Content: []ClaudeContentBlock{
							{Type: "server_tool_use", ID: "srv-1", Name: tc.serverName, Input: map[string]interface{}{}},
							{Type: tc.resultType, ToolUseID: "srv-1"},
						},
					}, plan)
					if !outcome.Accepted || outcome.Kind != "server_tool" || !outcome.RequiredMatch {
						t.Fatalf("outcome=%#v", outcome)
					}
				})
			}

			mismatched := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content: []ClaudeContentBlock{
					{Type: "server_tool_use", ID: "srv-3", Name: "bash_code_execution", Input: map[string]interface{}{}},
					{Type: "text_editor_code_execution_tool_result", ToolUseID: "srv-3"},
				},
			}, plan)
			if mismatched.Accepted || mismatched.Reason != "server_tool_result_type_mismatch" {
				t.Fatalf("mismatched result was accepted: %#v", mismatched)
			}
		})
	}

	unknownPlan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "code_execution_20990101", Name: "code_execution"}},
	}, false)
	if !strings.Contains(unknownPlan.ValidationReason, "unsupported code execution tool version") {
		t.Fatalf("future version was guessed: %#v", unknownPlan)
	}

	wrongHistoricalResult := completionGuardPendingServerTools(ClaudeRequest{
		Messages: []ClaudeMessage{{
			Role: "assistant",
			Content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "srv-history", Name: "bash_code_execution"},
				{Type: "text_editor_code_execution_tool_result", ToolUseID: "srv-history"},
			},
		}},
	})
	if wrongHistoricalResult["srv-history"] != "bash_code_execution" {
		t.Fatalf("mismatched historical result cleared pending tool: %v", wrongHistoricalResult)
	}

	for _, tc := range []struct {
		name       string
		serverName string
		resultType string
	}{
		{name: "legacy", serverName: "code_execution", resultType: "code_execution_tool_result"},
		{name: "modern", serverName: "bash_code_execution", resultType: "bash_code_execution_tool_result"},
	} {
		t.Run("history "+tc.name, func(t *testing.T) {
			pending := completionGuardPendingServerTools(ClaudeRequest{
				Messages: []ClaudeMessage{{
					Role: "assistant",
					Content: []ClaudeContentBlock{
						{Type: "server_tool_use", ID: "srv-history", Name: tc.serverName},
						{Type: tc.resultType, ToolUseID: "srv-history"},
					},
				}},
			})
			if len(pending) != 0 {
				t.Fatalf("matching historical result did not clear pending tool: %v", pending)
			}
		})
	}
}

func TestCompletionGuardStreamingCodeExecutionProtocols(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		writer, ok := newCompletionGuardStreamingWriter(
			httptest.NewRecorder(),
			"claude-opus-4-8",
		)
		if !ok {
			t.Fatal("streaming writer requires flusher")
		}
		defer writer.release()

		writeCompletionGuardTestFrames(t, writer,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m-legacy\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"legacy-code\",\"name\":\"code_execution\",\"input\":{}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"code\\\":\\\"print(1)\\\"}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"code_execution_tool_result\",\"tool_use_id\":\"legacy-code\",\"content\":{\"type\":\"code_execution_result\",\"stdout\":\"1\"}}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
		if err := writer.finish(); err != nil {
			t.Fatal(err)
		}
		plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
			Tools: []ClaudeTool{{
				Type: completionGuardLegacyCodeExecutionType,
				Name: "code_execution",
			}},
		}, false)
		candidate := writer.response()
		outcome := classifyCompletionGuardResponse(&candidate, plan)
		if !writer.protocolComplete() || !outcome.Accepted || outcome.Kind != "server_tool" {
			t.Fatalf(
				"complete=%t outcome=%#v protocolErr=%v candidate=%#v",
				writer.protocolComplete(),
				outcome,
				writer.protocolErr,
				candidate,
			)
		}
	})

	t.Run("dynamic-web", func(t *testing.T) {
		writer, ok := newCompletionGuardStreamingWriter(
			httptest.NewRecorder(),
			"claude-opus-4-8",
		)
		if !ok {
			t.Fatal("streaming writer requires flusher")
		}
		defer writer.release()

		writeCompletionGuardTestFrames(t, writer,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m-dynamic\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"code-1\",\"name\":\"code_execution\",\"input\":{}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"code\\\":\\\"search()\\\"}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"web-1\",\"name\":\"web_search\",\"input\":{},\"caller\":{\"type\":\"code_execution_20260120\",\"tool_id\":\"code-1\"}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"go context cancellation\\\"}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"web-1\",\"content\":[]}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"code_execution_tool_result\",\"tool_use_id\":\"code-1\",\"content\":{\"type\":\"code_execution_result\",\"stdout\":\"ok\"}}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":3}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":4}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
		if err := writer.finish(); err != nil {
			t.Fatal(err)
		}
		plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
			Tools: []ClaudeTool{{
				Type: "web_search_20260209",
				Name: "web_search",
			}},
		}, false)
		candidate := writer.response()
		outcome := classifyCompletionGuardResponse(&candidate, plan)
		if !writer.protocolComplete() || !outcome.Accepted || outcome.Kind != "server_tool" {
			t.Fatalf(
				"complete=%t outcome=%#v protocolErr=%v candidate=%#v",
				writer.protocolComplete(),
				outcome,
				writer.protocolErr,
				candidate,
			)
		}
	})
}

func completionGuardTestCaller(callerType, toolID string) map[string]json.RawMessage {
	raw, _ := json.Marshal(map[string]string{
		"type":    callerType,
		"tool_id": toolID,
	})
	return map[string]json.RawMessage{"caller": raw}
}

func TestCompletionGuardDynamicWebFilteringCapabilities(t *testing.T) {
	for _, tc := range []struct {
		toolType   string
		toolName   string
		resultType string
	}{
		{toolType: "web_search_20260209", toolName: "web_search", resultType: "web_search_tool_result"},
		{toolType: "web_fetch_20260318", toolName: "web_fetch", resultType: "web_fetch_tool_result"},
	} {
		t.Run(tc.toolType, func(t *testing.T) {
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Tools: []ClaudeTool{{Type: tc.toolType, Name: tc.toolName}},
			}, false)
			if plan.ValidationReason != "" {
				t.Fatalf("validation=%q", plan.ValidationReason)
			}
			webCapability, _ := plan.Catalog.ServerCapability(tc.toolName)
			if webCapability.AllowDirectCaller || !webCapability.AllowCodeCaller {
				t.Fatalf("dynamic web caller policy=%#v", webCapability)
			}
			codeCapability, _ := plan.Catalog.ServerCapability("code_execution")
			if !codeCapability.Implicit || !codeCapability.CanCallTools ||
				codeCapability.ResultBlockType != "code_execution_tool_result" {
				t.Fatalf("implicit code capability=%#v", codeCapability)
			}
			if bashCapability, _ := plan.Catalog.ServerCapability("bash_code_execution"); !bashCapability.Implicit || bashCapability.CanCallTools {
				t.Fatalf("implicit bash capability=%#v", bashCapability)
			}

			valid := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content: []ClaudeContentBlock{
					{Type: "server_tool_use", ID: "code-1", Name: "code_execution", Input: map[string]interface{}{"code": "search()"}},
					{
						Type: "server_tool_use", ID: "web-1", Name: tc.toolName,
						Input: map[string]interface{}{},
						Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
					},
					{Type: tc.resultType, ToolUseID: "web-1"},
					{Type: "code_execution_tool_result", ToolUseID: "code-1"},
				},
			}, plan)
			if !valid.Accepted || valid.Kind != "server_tool" {
				t.Fatalf("valid dynamic filtering outcome=%#v", valid)
			}

			direct := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content: []ClaudeContentBlock{
					{Type: "server_tool_use", ID: "web-direct", Name: tc.toolName, Input: map[string]interface{}{}},
					{Type: tc.resultType, ToolUseID: "web-direct"},
				},
			}, plan)
			if !direct.Accepted || direct.Kind != "server_tool" {
				t.Fatalf("structurally valid direct fallback was rejected: %#v", direct)
			}

			unknownCaller := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content: []ClaudeContentBlock{{
					Type: "server_tool_use", ID: "web-unknown", Name: tc.toolName,
					Input: map[string]interface{}{},
					Extra: completionGuardTestCaller(completionGuardCodeCallerType, "missing-code"),
				}},
			}, plan)
			if unknownCaller.Accepted || unknownCaller.Reason != "server_tool_unknown_caller" {
				t.Fatalf("unknown caller accepted: %#v", unknownCaller)
			}

			completedCaller := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content: []ClaudeContentBlock{
					{Type: "server_tool_use", ID: "code-done", Name: "code_execution", Input: map[string]interface{}{"code": "search()"}},
					{Type: "code_execution_tool_result", ToolUseID: "code-done"},
					{
						Type: "server_tool_use", ID: "web-late", Name: tc.toolName,
						Input: map[string]interface{}{},
						Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-done"),
					},
				},
			}, plan)
			if completedCaller.Accepted || completedCaller.Reason != "server_tool_unknown_caller" {
				t.Fatalf("completed caller remained active: %#v", completedCaller)
			}
		})
	}
}

func TestCompletionGuardDynamicWebDirectOnlyDisablesImplicitCodeExecution(t *testing.T) {
	allowedDirect, _ := json.Marshal([]string{"direct"})
	plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{
			Type:  "web_search_20260209",
			Name:  "web_search",
			Extra: map[string]json.RawMessage{"allowed_callers": allowedDirect},
		}},
	}, false)
	if plan.ValidationReason != "" {
		t.Fatalf("validation=%q", plan.ValidationReason)
	}
	for _, name := range []string{"code_execution", "bash_code_execution", "text_editor_code_execution"} {
		if _, ok := plan.Catalog.ServerCapability(name); ok {
			t.Fatalf("direct-only dynamic web provisioned code execution capability %q", name)
		}
	}
	outcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "server_tool_use", ID: "web-1", Name: "web_search", Input: map[string]interface{}{}},
			{Type: "web_search_tool_result", ToolUseID: "web-1"},
		},
	}, plan)
	if !outcome.Accepted || outcome.Kind != "server_tool" {
		t.Fatalf("direct-only outcome=%#v", outcome)
	}
	codeCaller := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "server_tool_use", ID: "code-1", Name: "code_execution", Input: map[string]interface{}{"code": "search()"}},
			{
				Type: "server_tool_use", ID: "web-code", Name: "web_search", Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
		},
	}, plan)
	if codeCaller.Accepted || codeCaller.Reason != "unknown_server_tool" {
		t.Fatalf("direct-only request accepted implicit code execution: %#v", codeCaller)
	}
}

func TestCompletionGuardValidatesFixedServerToolNames(t *testing.T) {
	for _, test := range []struct {
		toolType string
		name     string
		expected string
	}{
		{toolType: "code_execution_20260120", name: "run_code", expected: "code_execution"},
		{toolType: "web_search_20260209", name: "search", expected: "web_search"},
		{toolType: "web_fetch_20260318", name: "fetch", expected: "web_fetch"},
	} {
		t.Run(test.toolType, func(t *testing.T) {
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Tools: []ClaudeTool{{Type: test.toolType, Name: test.name}},
			}, false)
			if !strings.Contains(plan.ValidationReason, `name must be "`+test.expected+`"`) {
				t.Fatalf("validation=%q", plan.ValidationReason)
			}
		})
	}
}

func TestCompletionGuardRejectsMalformedReservedServerToolTypes(t *testing.T) {
	for _, test := range []ClaudeTool{
		{Type: "code_execution", Name: "code_execution"},
		{Type: "web_search_not-a-version", Name: "web_search"},
		{Type: "web_fetch_20261340", Name: "web_fetch"},
	} {
		t.Run(test.Type, func(t *testing.T) {
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Tools: []ClaudeTool{test},
			}, false)
			if !strings.Contains(plan.ValidationReason, "invalid versioned server tool type") {
				t.Fatalf("validation=%q", plan.ValidationReason)
			}
		})
	}

	wrongName := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "code_execution_future", Name: "run_code"}},
	}, false)
	if !strings.Contains(wrongName.ValidationReason, `name must be "code_execution"`) {
		t.Fatalf("reserved family escaped fixed-name validation: %q", wrongName.ValidationReason)
	}
}

func TestCompletionGuardValidatesProgrammaticClientToolCallers(t *testing.T) {
	codeOnly, _ := json.Marshal([]string{completionGuardCodeCallerType})
	plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{
				Name:        "read",
				InputSchema: map[string]interface{}{"type": "object"},
				Extra: map[string]json.RawMessage{
					"allowed_callers": codeOnly,
				},
			},
		},
	}, false)
	if plan.ValidationReason != "" {
		t.Fatalf("validation=%q", plan.ValidationReason)
	}

	direct := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{{
			Type: "tool_use", ID: "read-direct", Name: "read", Input: map[string]interface{}{},
		}},
	}, plan)
	if !direct.Accepted || direct.Kind != "real_tool" {
		t.Fatalf("structurally valid direct fallback was rejected: %#v", direct)
	}

	valid := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{
			{Type: "server_tool_use", ID: "code-1", Name: "code_execution", Input: map[string]interface{}{"code": "read()"}},
			{
				Type: "tool_use", ID: "read-code", Name: "read", Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
		},
	}, plan)
	if !valid.Accepted || valid.Kind != "real_tool" {
		t.Fatalf("valid programmatic caller outcome=%#v", valid)
	}

	unknown := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{{
			Type: "tool_use", ID: "read-unknown", Name: "read", Input: map[string]interface{}{},
			Extra: completionGuardTestCaller(completionGuardCodeCallerType, "missing-code"),
		}},
	}, plan)
	if unknown.Accepted || unknown.Reason != "client_tool_unknown_caller" {
		t.Fatalf("unknown programmatic caller accepted: %#v", unknown)
	}

	unsupported := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{
			{Type: "server_tool_use", ID: "code-2", Name: "code_execution", Input: map[string]interface{}{"code": "read()"}},
			{
				Type: "tool_use", ID: "read-unsupported", Name: "read", Input: map[string]interface{}{},
				Extra: completionGuardTestCaller("code_execution_20260521", "code-2"),
			},
		},
	}, plan)
	if unsupported.Accepted || unsupported.Reason != "client_tool_unsupported_caller" {
		t.Fatalf("non-canonical response caller accepted: %#v", unsupported)
	}

	malformedCaller := map[string]json.RawMessage{
		"caller": json.RawMessage(`{"type":"code_execution_20260120"}`),
	}
	malformed := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{{
			Type: "tool_use", ID: "read-malformed", Name: "read", Input: map[string]interface{}{},
			Extra: malformedCaller,
		}},
	}, plan)
	if malformed.Accepted || malformed.Reason != "client_tool_invalid_caller" {
		t.Fatalf("malformed caller accepted: %#v", malformed)
	}
}

func TestCompletionGuardRejectsToolChoiceForCodeOnlyTool(t *testing.T) {
	codeOnly, _ := json.Marshal([]string{completionGuardCodeCallerType})
	for _, test := range []struct {
		name string
		tool ClaudeTool
	}{
		{
			name: "client tool",
			tool: ClaudeTool{
				Name:  "read",
				Extra: map[string]json.RawMessage{"allowed_callers": codeOnly},
			},
		},
		{
			name: "dynamic web default",
			tool: ClaudeTool{Type: "web_search_20260209", Name: "web_search"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools := []ClaudeTool{test.tool}
			if test.name == "client tool" {
				tools = append([]ClaudeTool{{
					Type: "code_execution_20260120",
					Name: "code_execution",
				}}, tools...)
			}
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Tools: tools,
				ToolChoice: map[string]interface{}{
					"type": "tool",
					"name": test.tool.Name,
				},
			}, false)
			if !strings.Contains(plan.ValidationReason, "allowed_callers does not include direct") {
				t.Fatalf("validation=%q", plan.ValidationReason)
			}
		})
	}

	directAndCode, _ := json.Marshal([]string{"direct", completionGuardCodeCallerType})
	valid := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{
				Name:  "read",
				Extra: map[string]json.RawMessage{"allowed_callers": directAndCode},
			},
		},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "read"},
	}, false)
	if valid.ValidationReason != "" {
		t.Fatalf("direct-callable tool choice validation=%q", valid.ValidationReason)
	}
}

func TestCompletionGuardRejectsInvalidAllowedCallersOnClientTool(t *testing.T) {
	invalid, _ := json.Marshal([]string{"unknown_caller"})
	plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{
			Name: "read",
			Extra: map[string]json.RawMessage{
				"allowed_callers": invalid,
			},
		}},
	}, false)
	if !strings.Contains(plan.ValidationReason, "unsupported allowed_callers") {
		t.Fatalf("validation=%q", plan.ValidationReason)
	}
}

func TestCompletionGuardDynamicWebRejectsIncompatibleExplicitCodeExecution(t *testing.T) {
	plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{
			{Type: "web_search_20260209", Name: "web_search"},
			{Type: "code_execution_20250825", Name: "code_execution"},
		},
	}, false)
	if !strings.Contains(plan.ValidationReason, "requires code_execution_20260120 or later") {
		t.Fatalf("validation=%q", plan.ValidationReason)
	}
}

func TestCompletionGuardRepeatedPauseTurnReplacesPriorContinuation(t *testing.T) {
	base := ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "Complete the task."}},
	}
	pauseOne := ClaudeResponse{
		StopReason: "pause_turn",
		Content:    []ClaudeContentBlock{{Type: "text", Text: "pause-one"}},
	}
	pauseTwo := ClaudeResponse{
		StopReason: "pause_turn",
		Content:    []ClaudeContentBlock{{Type: "text", Text: "pause-two"}},
	}

	first, err := completionGuardPauseTurnRequest(base, &pauseOne)
	if err != nil {
		t.Fatal(err)
	}
	second, err := completionGuardPauseTurnRequest(base, &pauseTwo)
	if err != nil {
		t.Fatal(err)
	}

	if len(first.Messages) != 2 || len(second.Messages) != 2 {
		t.Fatalf("first=%d second=%d", len(first.Messages), len(second.Messages))
	}
	wire, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("pause-one")) || bytes.Count(wire, []byte("pause-two")) != 1 {
		t.Fatalf("pause replacement transcript=%s", wire)
	}
}

func TestCompletionGuardAuditContinuationContainsVisiblePrefixOnce(t *testing.T) {
	base := ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "Edit the file."}},
	}
	candidate := ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "visible-prefix"},
			{Type: "text", Text: " newest-output"},
		},
		Extra: map[string]json.RawMessage{
			"container": json.RawMessage(`{"id":"container-latest"}`),
		},
	}

	next, err := buildCompletionGuardContinuationRequest(
		base,
		candidate,
		[]string{"edit the requested file"},
	)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(wire, []byte("visible-prefix")); got != 1 {
		t.Fatalf("visible prefix count=%d request=%s", got, wire)
	}
	if got := string(next.Extra["container"]); got != `{"id":"container-latest"}` {
		t.Fatalf("latest continuation container was lost: %q", got)
	}
}

func TestCompletionGuardToolSearchAndAdvisorDefinitions(t *testing.T) {
	for _, tc := range []struct {
		toolType   string
		toolName   string
		resultType string
		extra      map[string]json.RawMessage
	}{
		{toolType: "tool_search_tool_regex_20251119", toolName: "tool_search_tool_regex", resultType: "tool_search_tool_result"},
		{toolType: "tool_search_tool_bm25", toolName: "tool_search_tool_bm25", resultType: "tool_search_tool_result"},
		{
			toolType: "advisor_20260301", toolName: "advisor", resultType: "advisor_tool_result",
			extra: map[string]json.RawMessage{"model": json.RawMessage(`"claude-fable-5"`)},
		},
	} {
		t.Run(tc.toolType, func(t *testing.T) {
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Tools: []ClaudeTool{{Type: tc.toolType, Name: tc.toolName, Extra: tc.extra}},
			}, false)
			if plan.ValidationReason != "" {
				t.Fatalf("validation=%q", plan.ValidationReason)
			}
			capability, ok := plan.Catalog.ServerCapability(tc.toolName)
			if !ok || capability.ResultBlockType != tc.resultType {
				t.Fatalf("capability=%#v present=%t", capability, ok)
			}
			result := ClaudeContentBlock{Type: tc.resultType, ToolUseID: "srv-1"}
			if tc.resultType == "tool_search_tool_result" {
				result.Content = map[string]interface{}{
					"type":            "tool_search_tool_search_result",
					"tool_references": []interface{}{},
				}
			}
			outcome := classifyCompletionGuardResponse(&ClaudeResponse{
				StopReason: "end_turn",
				Content: []ClaudeContentBlock{
					{Type: "server_tool_use", ID: "srv-1", Name: tc.toolName, Input: map[string]interface{}{}},
					result,
				},
			}, plan)
			if !outcome.Accepted || outcome.Kind != "server_tool" {
				t.Fatalf("outcome=%#v", outcome)
			}
		})
	}

	wrongName := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "tool_search_tool_regex_20251119", Name: "search"}},
	}, false)
	if !strings.Contains(wrongName.ValidationReason, `name must be "tool_search_tool_regex"`) {
		t.Fatalf("wrong tool-search name validation=%q", wrongName.ValidationReason)
	}
	wrongAdvisor := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Type: "advisor_20260301", Name: "consult"}},
	}, false)
	if !strings.Contains(wrongAdvisor.ValidationReason, `name must be "advisor"`) {
		t.Fatalf("wrong advisor name validation=%q", wrongAdvisor.ValidationReason)
	}
}

func TestCompletionGuardProgrammaticRequestValidation(t *testing.T) {
	codeOnly, _ := json.Marshal([]string{completionGuardCodeCallerType})
	invalidHistorical, _ := json.Marshal([]string{"code_execution_20250825"})

	invalidCaller := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": invalidHistorical}}},
	}, false)
	if !strings.Contains(invalidCaller.ValidationReason, "unsupported allowed_callers") {
		t.Fatalf("historical caller validation=%q", invalidCaller.ValidationReason)
	}

	missingCode := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}}},
	}, false)
	if !strings.Contains(missingCode.ValidationReason, "requires code_execution_20260120 or later") {
		t.Fatalf("missing code execution validation=%q", missingCode.ValidationReason)
	}

	oldCode := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20250825", Name: "code_execution"},
			{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}},
		},
	}, false)
	if !strings.Contains(oldCode.ValidationReason, "requires code_execution_20260120 or later") {
		t.Fatalf("old code execution validation=%q", oldCode.ValidationReason)
	}

	strictRaw := json.RawMessage(`true`)
	strict := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly, "strict": strictRaw}},
		},
	}, false)
	if !strings.Contains(strict.ValidationReason, "strict=true is incompatible") {
		t.Fatalf("strict validation=%q", strict.ValidationReason)
	}

	disableParallel := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}},
		},
		ToolChoice: map[string]interface{}{"type": "auto", "disable_parallel_tool_use": true},
	}, false)
	if !strings.Contains(disableParallel.ValidationReason, "disable_parallel_tool_use=true is incompatible") {
		t.Fatalf("parallel validation=%q", disableParallel.ValidationReason)
	}

	unsupported := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Model: "alias",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}},
		},
	}, false, "claude-haiku-4-5-20251001")
	if unsupported.ValidationReason != "" {
		t.Fatalf("model support must be evaluated after deployment selection: %q", unsupported.ValidationReason)
	}

	legacyDateID := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Model: "alias",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}},
		},
	}, false, "claude-opus-4-20250514")
	if legacyDateID.ValidationReason != "" {
		t.Fatalf("model support must be evaluated after deployment selection: %q", legacyDateID.ValidationReason)
	}

	supported := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Model: "alias",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}},
		},
	}, false, "claude-opus-4-8")
	if supported.ValidationReason != "" {
		t.Fatalf("supported validation=%q", supported.ValidationReason)
	}
}

func TestCompletionGuardProgrammaticContinuationValidation(t *testing.T) {
	codeOnly, _ := json.Marshal([]string{completionGuardCodeCallerType})
	container := json.RawMessage(`"container-1"`)
	base := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}},
		},
		Extra: map[string]json.RawMessage{"container": container},
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "code-1", Name: "code_execution", Input: map[string]interface{}{"code": "await query({})"}},
				{Type: "tool_use", ID: "tool-1", Name: "query", Input: map[string]interface{}{}, Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1")},
			}},
			{Role: "user", Content: []ClaudeContentBlock{{Type: "tool_result", ToolUseID: "tool-1", Content: "ok"}}},
		},
	}
	valid := buildAgentCompletionGuardPlan(&base, false)
	if valid.ValidationReason != "" {
		t.Fatalf("valid continuation=%q", valid.ValidationReason)
	}

	missingContainer := base
	missingContainer.Extra = nil
	if got := buildAgentCompletionGuardPlan(&missingContainer, false).ValidationReason; !strings.Contains(got, "requires a non-empty container") {
		t.Fatalf("missing container validation=%q", got)
	}

	mixed := base
	mixed.Messages = append([]ClaudeMessage(nil), base.Messages...)
	mixed.Messages[1].Content = []ClaudeContentBlock{
		{Type: "tool_result", ToolUseID: "tool-1", Content: "ok"},
		{Type: "text", Text: "continue"},
	}
	if got := buildAgentCompletionGuardPlan(&mixed, false).ValidationReason; !strings.Contains(got, "only tool_result") {
		t.Fatalf("mixed content validation=%q", got)
	}

	badContent := base
	badContent.Messages = append([]ClaudeMessage(nil), base.Messages...)
	badContent.Messages[1].Content = []ClaudeContentBlock{{
		Type: "tool_result", ToolUseID: "tool-1",
		Content: []ClaudeContentBlock{{Type: "image"}},
	}}
	if got := buildAgentCompletionGuardPlan(&badContent, false).ValidationReason; !strings.Contains(got, "string or text blocks") {
		t.Fatalf("bad content validation=%q", got)
	}
}

func TestCompletionGuardInheritedProgrammaticCodeMayCompleteAfterDescendants(t *testing.T) {
	plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Type: "web_search_20260209", Name: "web_search"},
		},
	}, false)
	if plan.ValidationReason != "" {
		t.Fatalf("plan validation=%q", plan.ValidationReason)
	}
	outcome := classifyCompletionGuardResponseWithExpectedServerTools(
		&ClaudeResponse{
			StopReason: "end_turn",
			Content: []ClaudeContentBlock{
				{Type: "server_tool_use", ID: "web-1", Name: "web_search", Input: map[string]interface{}{}, Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1")},
				{Type: "web_search_tool_result", ToolUseID: "web-1"},
				{Type: "code_execution_tool_result", ToolUseID: "code-1"},
			},
		},
		plan,
		map[string]string{"code-1": "code_execution"},
	)
	if !outcome.Accepted || outcome.Kind != "server_tool" {
		t.Fatalf("programmatic descendant ordering outcome=%#v", outcome)
	}
}
