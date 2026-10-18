package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func completionGuardTestProgrammaticTools() []ClaudeTool {
	allowed, _ := json.Marshal([]string{completionGuardCodeCallerType})
	return []ClaudeTool{
		{Type: "code_execution_20260120", Name: "code_execution"},
		{Name: "query", Extra: map[string]json.RawMessage{"allowed_callers": allowed}},
	}
}

func completionGuardTestProgrammaticPlan(t *testing.T, extraTools ...ClaudeTool) agentCompletionGuardPlan {
	t.Helper()
	tools := append(completionGuardTestProgrammaticTools(), extraTools...)
	plan := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{Tools: tools}, false, "claude-opus-4-8")
	if plan.ValidationReason != "" {
		t.Fatalf("plan validation=%q", plan.ValidationReason)
	}
	return plan
}

func completionGuardTestCodeUse(id string) ClaudeContentBlock {
	return ClaudeContentBlock{
		Type: "server_tool_use", ID: id, Name: "code_execution",
		Input: map[string]interface{}{"code": "await query({})"},
	}
}

func completionGuardTestProgrammaticUse(id, parentID string) ClaudeContentBlock {
	return ClaudeContentBlock{
		Type: "tool_use", ID: id, Name: "query", Input: map[string]interface{}{},
		Extra: completionGuardTestCaller(completionGuardCodeCallerType, parentID),
	}
}

func TestCompletionGuardToolGraphRejectsParentClosureWithPendingClientChild(t *testing.T) {
	plan := completionGuardTestProgrammaticPlan(t)
	outcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "tool_use",
		Content: []ClaudeContentBlock{
			completionGuardTestCodeUse("code-1"),
			completionGuardTestProgrammaticUse("tool-1", "code-1"),
			{Type: "code_execution_tool_result", ToolUseID: "code-1"},
		},
	}, plan)
	if outcome.Accepted || outcome.Reason != "server_tool_parent_closed_with_pending_children" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardToolGraphRejectsParentClosureWithPendingServerChild(t *testing.T) {
	plan := completionGuardTestProgrammaticPlan(t, ClaudeTool{
		Type: "web_search_20260209", Name: "web_search",
	})
	outcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			completionGuardTestCodeUse("code-1"),
			{
				Type: "server_tool_use", ID: "web-1", Name: "web_search",
				Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
			{Type: "code_execution_tool_result", ToolUseID: "code-1"},
			{Type: "web_search_tool_result", ToolUseID: "web-1"},
		},
	}, plan)
	if outcome.Accepted || outcome.Reason != "server_tool_parent_closed_with_pending_children" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardToolGraphAllowsChildrenBeforeParentClosure(t *testing.T) {
	plan := completionGuardTestProgrammaticPlan(t, ClaudeTool{
		Type: "web_search_20260209", Name: "web_search",
	})
	outcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			completionGuardTestCodeUse("code-1"),
			{
				Type: "server_tool_use", ID: "web-1", Name: "web_search",
				Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
			{Type: "web_search_tool_result", ToolUseID: "web-1"},
			{Type: "code_execution_tool_result", ToolUseID: "code-1"},
		},
	}, plan)
	if !outcome.Accepted || outcome.Kind != "server_tool" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardToolGraphRejectsParentUntilEveryChildResolves(t *testing.T) {
	plan := completionGuardTestProgrammaticPlan(t, ClaudeTool{
		Type: "web_search_20260209", Name: "web_search",
	})
	outcome := classifyCompletionGuardResponse(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			completionGuardTestCodeUse("code-1"),
			{
				Type: "server_tool_use", ID: "web-1", Name: "web_search",
				Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
			{
				Type: "server_tool_use", ID: "web-2", Name: "web_search",
				Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
			{Type: "web_search_tool_result", ToolUseID: "web-1"},
			{Type: "code_execution_tool_result", ToolUseID: "code-1"},
		},
	}, plan)
	if outcome.Accepted || outcome.Reason != "server_tool_parent_closed_with_pending_children" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardHistoricalCallerIntegrity(t *testing.T) {
	baseTools := completionGuardTestProgrammaticTools()
	cases := []struct {
		name    string
		content interface{}
		want    string
	}{
		{
			name: "missing_tool_id",
			content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "code-1", "name": "code_execution"},
				{"type": "tool_use", "id": "tool-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType}},
			},
			want: "invalid client tool caller",
		},
		{
			name: "unknown_parent",
			content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "code-1", "name": "code_execution"},
				{"type": "tool_use", "id": "tool-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "missing"}},
			},
			want: "does not reference active code execution",
		},
		{
			name: "wrong_parent_kind",
			content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "web-1", "name": "web_search"},
				{"type": "tool_use", "id": "tool-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "web-1"}},
			},
			want: "does not reference active code execution",
		},
		{
			name: "duplicate_client_id",
			content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "code-1", "name": "code_execution"},
				{"type": "tool_use", "id": "tool-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "code-1"}},
				{"type": "tool_use", "id": "tool-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "code-1"}},
			},
			want: "duplicate_tool_id",
		},
		{
			name: "client_server_id_collision",
			content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "same-1", "name": "code_execution"},
				{"type": "tool_use", "id": "same-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "same-1"}},
			},
			want: "duplicate_tool_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := append([]ClaudeTool(nil), baseTools...)
			if tc.name == "wrong_parent_kind" {
				tools = append(tools, ClaudeTool{Type: "web_search_20250305", Name: "web_search"})
			}
			plan := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
				Model:    "claude-opus-4-8",
				Tools:    tools,
				Messages: []ClaudeMessage{{Role: "assistant", Content: tc.content}},
			}, false, "claude-opus-4-8")
			if !strings.Contains(plan.ValidationReason, tc.want) {
				t.Fatalf("validation=%q want %q", plan.ValidationReason, tc.want)
			}
		})
	}
}

func TestCompletionGuardHistoricalGraphRejectsUnknownServerTool(t *testing.T) {
	plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
		Messages: []ClaudeMessage{{
			Role: "assistant",
			Content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "future-1", "name": "future_tool"},
				{"type": "future_tool_tool_result", "tool_use_id": "future-1"},
			},
		}},
	}, false)
	if !strings.Contains(plan.ValidationReason, `unknown server tool "future_tool"`) {
		t.Fatalf("validation=%q", plan.ValidationReason)
	}
}

func TestCompletionGuardProgrammaticContinuationAcrossMultipleTurns(t *testing.T) {
	request := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: completionGuardTestProgrammaticTools(),
		Extra: map[string]json.RawMessage{"container": json.RawMessage(`"container-1"`)},
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: []ClaudeContentBlock{
				completionGuardTestCodeUse("code-1"),
				completionGuardTestProgrammaticUse("tool-1", "code-1"),
			}},
			{Role: "user", Content: []ClaudeContentBlock{{Type: "tool_result", ToolUseID: "tool-1", Content: "one"}}},
			{Role: "assistant", Content: []ClaudeContentBlock{
				completionGuardTestProgrammaticUse("tool-2", "code-1"),
			}},
			{Role: "user", Content: []ClaudeContentBlock{{Type: "tool_result", ToolUseID: "tool-2", Content: "two"}}},
		},
	}
	plan := buildAgentCompletionGuardPlanForModel(&request, false, "claude-opus-4-8")
	if plan.ValidationReason != "" {
		t.Fatalf("multi-turn continuation rejected: %q", plan.ValidationReason)
	}
}

func TestCompletionGuardProgrammaticResultRequiresActualTextField(t *testing.T) {
	contentCases := []struct {
		name    string
		content interface{}
		valid   bool
	}{
		{"missing", []map[string]interface{}{{"type": "text"}}, false},
		{"null", []map[string]interface{}{{"type": "text", "text": nil}}, false},
		{"number", []map[string]interface{}{{"type": "text", "text": 42}}, false},
		{"empty_string", []map[string]interface{}{{"type": "text", "text": ""}}, true},
		{"text", []map[string]interface{}{{"type": "text", "text": "ok"}}, true},
	}

	for _, tc := range contentCases {
		t.Run(tc.name, func(t *testing.T) {
			request := ClaudeRequest{
				Model: "claude-opus-4-8",
				Tools: completionGuardTestProgrammaticTools(),
				Extra: map[string]json.RawMessage{"container": json.RawMessage(`"container-1"`)},
				Messages: []ClaudeMessage{
					{Role: "assistant", Content: []map[string]interface{}{
						{"type": "server_tool_use", "id": "code-1", "name": "code_execution"},
						{"type": "tool_use", "id": "tool-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "code-1"}},
					}},
					{Role: "user", Content: []map[string]interface{}{
						{"type": "tool_result", "tool_use_id": "tool-1", "content": tc.content},
					}},
				},
			}
			plan := buildAgentCompletionGuardPlanForModel(&request, false, "claude-opus-4-8")
			if tc.valid && plan.ValidationReason != "" {
				t.Fatalf("valid content rejected: %q", plan.ValidationReason)
			}
			if !tc.valid && !strings.Contains(plan.ValidationReason, "string or text blocks") {
				t.Fatalf("invalid content validation=%q", plan.ValidationReason)
			}
		})
	}
}

func TestCompletionGuardHistoryRequiresToolSearchAndAdvisorDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		useName    string
		resultType string
	}{
		{"tool_search", "tool_search_tool_regex", "tool_search_tool_result"},
		{"advisor", "advisor", "advisor_tool_result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := ClaudeContentBlock{Type: tc.resultType, ToolUseID: "srv-1"}
			if tc.resultType == "tool_search_tool_result" {
				result.Content = map[string]interface{}{
					"type":            "tool_search_tool_search_result",
					"tool_references": []interface{}{},
				}
			}
			plan := buildAgentCompletionGuardPlan(&ClaudeRequest{
				Messages: []ClaudeMessage{{Role: "assistant", Content: []ClaudeContentBlock{
					{Type: "server_tool_use", ID: "srv-1", Name: tc.useName, Input: map[string]interface{}{}},
					result,
				}}},
			}, false)
			if !strings.Contains(plan.ValidationReason, "is missing from the current tool catalog") {
				t.Fatalf("validation=%q", plan.ValidationReason)
			}
		})
	}
}

func TestCompletionGuardDirectDeferredServerResultMustRemainFirst(t *testing.T) {
	req := ClaudeRequest{
		Tools: []ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}},
		Messages: []ClaudeMessage{{Role: "assistant", Content: []ClaudeContentBlock{
			{Type: "server_tool_use", ID: "web-1", Name: "web_search", Input: map[string]interface{}{}},
		}}},
	}
	plan := buildAgentCompletionGuardPlan(&req, false)
	if plan.ValidationReason != "" {
		t.Fatalf("plan validation=%q", plan.ValidationReason)
	}
	outcome := classifyCompletionGuardResponseWithRequestHistory(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "before result"},
			{Type: "web_search_tool_result", ToolUseID: "web-1"},
		},
	}, plan, req)
	if outcome.Accepted || outcome.Reason != "inherited_server_tool_result_not_first" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardHistoricalProgrammaticChildrenMustRemainCodeCallable(t *testing.T) {
	t.Run("client tool missing from catalog", func(t *testing.T) {
		req := ClaudeRequest{
			Model: "claude-opus-4-8",
			Tools: []ClaudeTool{{Type: "code_execution_20260120", Name: "code_execution"}},
			Messages: []ClaudeMessage{{Role: "assistant", Content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "code-1", "name": "code_execution"},
				{"type": "tool_use", "id": "tool-1", "name": "query", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "code-1"}},
			}}},
		}
		plan := buildAgentCompletionGuardPlanForModel(&req, false, "claude-opus-4-8")
		if !strings.Contains(plan.ValidationReason, `programmatic tool "query" is not code-callable`) {
			t.Fatalf("validation=%q", plan.ValidationReason)
		}
	})

	t.Run("server tool is direct only", func(t *testing.T) {
		req := ClaudeRequest{
			Model: "claude-opus-4-8",
			Tools: []ClaudeTool{
				{Type: "code_execution_20260120", Name: "code_execution"},
				{Type: "web_search_20250305", Name: "web_search"},
			},
			Messages: []ClaudeMessage{{Role: "assistant", Content: []map[string]interface{}{
				{"type": "server_tool_use", "id": "code-1", "name": "code_execution"},
				{"type": "server_tool_use", "id": "web-1", "name": "web_search", "caller": map[string]interface{}{"type": completionGuardCodeCallerType, "tool_id": "code-1"}},
			}}},
		}
		plan := buildAgentCompletionGuardPlanForModel(&req, false, "claude-opus-4-8")
		if !strings.Contains(plan.ValidationReason, `server tool "web_search" is not code-callable`) {
			t.Fatalf("validation=%q", plan.ValidationReason)
		}
	})
}

func TestCompletionGuardInheritedCodeExecutionBecomesProgrammaticWhenChildAppears(t *testing.T) {
	req := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Type: "web_search_20260209", Name: "web_search"},
		},
		Messages: []ClaudeMessage{{Role: "assistant", Content: []ClaudeContentBlock{
			completionGuardTestCodeUse("code-1"),
		}}},
	}
	plan := buildAgentCompletionGuardPlanForModel(&req, false, "claude-opus-4-8")
	if plan.ValidationReason != "" {
		t.Fatalf("plan validation=%q", plan.ValidationReason)
	}
	outcome := classifyCompletionGuardResponseWithRequestHistory(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{
				Type: "server_tool_use", ID: "web-1", Name: "web_search",
				Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
			{Type: "web_search_tool_result", ToolUseID: "web-1"},
			{Type: "code_execution_tool_result", ToolUseID: "code-1"},
		},
	}, plan, req)
	if !outcome.Accepted || outcome.Kind != "server_tool" {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestCompletionGuardHistoricalCodeExecutionWithDescendantIsNotDirectDeferred(t *testing.T) {
	req := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Type: "web_search_20260209", Name: "web_search"},
		},
		Messages: []ClaudeMessage{{Role: "assistant", Content: []ClaudeContentBlock{
			completionGuardTestCodeUse("code-1"),
			{
				Type: "server_tool_use", ID: "web-1", Name: "web_search",
				Input: map[string]interface{}{},
				Extra: completionGuardTestCaller(completionGuardCodeCallerType, "code-1"),
			},
			{Type: "web_search_tool_result", ToolUseID: "web-1"},
		}}},
	}
	plan := buildAgentCompletionGuardPlanForModel(&req, false, "claude-opus-4-8")
	if plan.ValidationReason != "" {
		t.Fatalf("plan validation=%q", plan.ValidationReason)
	}
	graph, reason := completionGuardBuildHistoricalToolGraph(&req, plan)
	if reason != "" {
		t.Fatalf("history validation=%q", reason)
	}
	parent, exists := graph.activeNode("code-1")
	if !exists {
		t.Fatal("code execution parent is not pending")
	}
	if parent.DirectDeferred {
		t.Fatal("programmatic parent with a descendant was misclassified as direct deferred")
	}

	outcome := classifyCompletionGuardResponseWithToolGraph(&ClaudeResponse{
		StopReason: "end_turn",
		Content: []ClaudeContentBlock{
			{Type: "text", Text: "resumed code continued"},
			{Type: "code_execution_tool_result", ToolUseID: "code-1"},
		},
	}, plan, graph)
	if !outcome.Accepted || outcome.Kind != "prose" {
		t.Fatalf("outcome=%#v", outcome)
	}
}
