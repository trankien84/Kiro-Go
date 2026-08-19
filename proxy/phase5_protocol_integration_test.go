package proxy

import "testing"

func TestPhase5ResponsesIncompleteUsesIncompleteTerminalEvent(t *testing.T) {
	resp := &ResponsesObject{Status: "completed"}
	applyResponsesCompletion(resp, "max_tokens")

	if resp.Status != "incomplete" {
		t.Fatalf("status=%q, want incomplete", resp.Status)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete_details=%#v, want max_output_tokens", resp.IncompleteDetails)
	}
	if got := responsesTerminalEventName(resp); got != "response.incomplete" {
		t.Fatalf("terminal event=%q, want response.incomplete", got)
	}
}

func TestPhase5ResponsesCompletedKeepsCompletedTerminalEvent(t *testing.T) {
	resp := &ResponsesObject{Status: "in_progress"}
	applyResponsesCompletion(resp, "end_turn")

	if resp.Status != "completed" {
		t.Fatalf("status=%q, want completed", resp.Status)
	}
	if resp.IncompleteDetails != nil {
		t.Fatalf("incomplete_details=%#v, want nil", resp.IncompleteDetails)
	}
	if got := responsesTerminalEventName(resp); got != "response.completed" {
		t.Fatalf("terminal event=%q, want response.completed", got)
	}
}

func TestPhase5NativeWebSearchIsNotSerializedAsKiroClientTool(t *testing.T) {
	tools, _ := convertClaudeTools([]ClaudeTool{
		{Type: "web_search_20250305", Name: "web_search"},
		{Name: "echo", Description: "Echo input", InputSchema: map[string]interface{}{"type": "object"}},
	})

	if len(tools) != 1 {
		t.Fatalf("converted tool count=%d, want 1", len(tools))
	}
	if got := tools[0].ToolSpecification.Name; got != "echo" {
		t.Fatalf("converted tool=%q, want echo", got)
	}
}

func TestPhase5ClientToolNamedWebSearchIsStillAClientTool(t *testing.T) {
	tools, nameMap := convertClaudeTools([]ClaudeTool{
		{Name: "web_search", Description: "Client-defined search", InputSchema: map[string]interface{}{"type": "object"}},
	})

	if len(tools) != 1 {
		t.Fatalf("converted tool count=%d, want 1", len(tools))
	}
	got := tools[0].ToolSpecification.Name
	if got == "" {
		t.Fatal("client-defined web_search must remain in the Kiro client tool catalog")
	}
	if original := nameMap[got]; original != "web_search" {
		t.Fatalf("tool name mapping %q -> %q, want web_search", got, original)
	}
}

func TestPhase5NativeWebSearchDoesNotQualifyForClientToolSteering(t *testing.T) {
	if hasClaudeClientTool([]ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}}) {
		t.Fatal("native server web_search must not qualify as a client tool for steering")
	}
	if !hasClaudeClientTool([]ClaudeTool{{Name: "web_search"}}) {
		t.Fatal("client-defined tool named web_search must remain eligible for client-tool steering")
	}
}
