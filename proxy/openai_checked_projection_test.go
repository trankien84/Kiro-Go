package proxy

import (
	"strings"
	"testing"
)

func TestOpenAIToKiroWithoutTruncationLeavesOversizedHistoryIntact(t *testing.T) {
	const model = "claude-sonnet-4.5"
	messages := make([]OpenAIMessage, 0, 65)
	large := strings.Repeat("x", 48*1024)
	for i := 0; i < 32; i++ {
		messages = append(messages,
			OpenAIMessage{Role: "user", Content: large},
			OpenAIMessage{Role: "assistant", Content: "ack"},
		)
	}
	messages = append(messages, OpenAIMessage{Role: "user", Content: "final"})
	req := &OpenAIRequest{Model: model, Messages: messages}

	strict := OpenAIToKiroWithoutTruncation(req, false)
	if strict == nil {
		t.Fatal("strict OpenAI projection returned nil payload")
	}
	if payloadFits(strict, maxPayloadTokens(model)) {
		t.Fatal("fixture unexpectedly fits; test no longer exercises the truncation boundary")
	}

	legacy := OpenAIToKiro(req, false)
	if legacy == nil {
		t.Fatal("legacy OpenAI projection returned nil payload")
	}
	if !payloadFits(legacy, maxPayloadTokens(model)) {
		t.Fatal("legacy converter did not reduce the oversized fixture to its configured budget")
	}
	if len(strict.ConversationState.History) <= len(legacy.ConversationState.History) {
		t.Fatalf("strict history=%d legacy history=%d; strict projection must not silently drop history", len(strict.ConversationState.History), len(legacy.ConversationState.History))
	}
}

func TestKiroToOpenAIResponsePreservesTextWithToolCalls(t *testing.T) {
	resp := KiroToOpenAIResponse(
		"visible text",
		[]KiroToolUse{{ToolUseID: "call_1", Name: "lookup", Input: map[string]interface{}{"q": "x"}}},
		3,
		4,
		"model",
	)
	if resp == nil || len(resp.Choices) != 1 {
		t.Fatalf("unexpected response: %#v", resp)
	}
	message := resp.Choices[0].Message
	if got, ok := message.Content.(string); !ok || got != "visible text" {
		t.Fatalf("mixed tool response lost visible content: %#v", message.Content)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("mixed tool response lost tool call: %#v", message.ToolCalls)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q want tool_calls", resp.Choices[0].FinishReason)
	}
}

func TestKiroToOpenAIResponseWithReasoningPreservesMixedFields(t *testing.T) {
	resp := KiroToOpenAIResponseWithReasoning(
		"visible text",
		"reasoning text",
		[]KiroToolUse{{ToolUseID: "call_1", Name: "lookup", Input: map[string]interface{}{"q": "x"}}},
		3,
		4,
		"model",
		"reasoning_content",
	)
	choices, ok := resp["choices"].([]map[string]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("unexpected choices: %#v", resp["choices"])
	}
	message, ok := choices[0]["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected message: %#v", choices[0]["message"])
	}
	if message["content"] != "visible text" {
		t.Fatalf("mixed reasoning/tool response lost content: %#v", message["content"])
	}
	if message["reasoning_content"] != "reasoning text" {
		t.Fatalf("mixed reasoning/tool response lost reasoning: %#v", message["reasoning_content"])
	}
	toolCalls, ok := message["tool_calls"].([]map[string]interface{})
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("mixed reasoning/tool response lost tool calls: %#v", message["tool_calls"])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason=%#v want tool_calls", choices[0]["finish_reason"])
	}
}

func TestProjectOpenAIToKiroReportsRuntimeHistoryMutations(t *testing.T) {
	cases := []struct {
		name string
		req  *OpenAIRequest
		code string
	}{
		{
			name: "legacy tool-call narration scrub",
			req: &OpenAIRequest{Model: "x", Messages: []OpenAIMessage{
				{Role: "user", Content: "start"},
				{Role: "assistant", Content: "prefix [Called tool lookup with input {}] suffix"},
				{Role: "user", Content: "final"},
			}},
			code: "assistant_history_sanitization",
		},
		{
			name: "duplicate history users collapse",
			req: &OpenAIRequest{Model: "x", Messages: []OpenAIMessage{
				{Role: "user", Content: "same"},
				{Role: "user", Content: "same"},
				{Role: "assistant", Content: "ack"},
				{Role: "user", Content: "final"},
			}},
			code: "duplicate_history_turn_collapsed",
		},
		{
			name: "leading assistant trimmed",
			req: &OpenAIRequest{Model: "x", Messages: []OpenAIMessage{
				{Role: "assistant", Content: "preface"},
				{Role: "user", Content: "final"},
			}},
			code: "leading_assistant_history_trimmed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, projection := projectOpenAIToKiro(tc.req, false)
			if !projection.Emulated() {
				t.Fatalf("runtime mutation was not classified as emulation: %+v", projection)
			}
			found := false
			for _, issue := range projection.Issues {
				if issue.Code == tc.code {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("runtime report missing %s: %+v", tc.code, projection)
			}
		})
	}
}

func TestProjectOpenAIToKiroReportsActiveToolResultNarration(t *testing.T) {
	req := &OpenAIRequest{Model: "x", Messages: []OpenAIMessage{
		{Role: "user", Content: "call lookup"},
		{Role: "assistant", Content: nil, ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "lookup", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: "result"},
	}}
	_, projection := projectOpenAIToKiro(req, false)
	if !projection.Emulated() {
		t.Fatalf("active tool-result compatibility narration must be explicit emulation: %+v", projection)
	}
	for _, issue := range projection.Issues {
		if issue.Code == "active_tool_result_narration" {
			return
		}
	}
	t.Fatalf("runtime report missed active tool-result narration: %+v", projection)
}

func TestProjectOpenAIToKiroFailsClosedOnRuntimeToolStateRepair(t *testing.T) {
	badArguments := &OpenAIRequest{Model: "x", Messages: []OpenAIMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_bad", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "lookup", Arguments: `[]`},
		}}},
		{Role: "tool", ToolCallID: "call_bad", Content: "result"},
	}}
	_, projection := projectOpenAIToKiro(badArguments, false)
	if projection.Supported() {
		t.Fatalf("runtime projector repaired non-object tool arguments instead of failing closed: %+v", projection)
	}
	found := false
	for _, issue := range projection.Issues {
		if issue.Code == "invalid_tool_arguments" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("runtime projector did not report repaired tool arguments: %+v", projection)
	}

	orphanResult := &OpenAIRequest{Model: "x", Messages: []OpenAIMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", Content: "no structured call"},
		{Role: "tool", ToolCallID: "call_missing", Content: "result"},
	}}
	_, projection = projectOpenAIToKiro(orphanResult, false)
	if projection.Supported() {
		t.Fatalf("runtime projector narrated an orphan tool result instead of failing closed: %+v", projection)
	}
	found = false
	for _, issue := range projection.Issues {
		if issue.Code == "orphan_tool_result" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("runtime projector did not report orphan tool-result narration: %+v", projection)
	}
}
