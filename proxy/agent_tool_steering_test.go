package proxy

import (
	"kiro-go/config"
	"strings"
	"testing"
)

// toolBearingClaudeRequest builds a minimal Claude request carrying one tool
// and an explicit system prompt, so tests can assert on exactly what
// ClaudeToKiro adds around that baseline content.
func toolBearingClaudeRequest() *ClaudeRequest {
	return &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "please read the file"},
		},
		Tools: []ClaudeTool{
			{
				Name:        "read_file",
				Description: "Reads a file from disk",
				InputSchema: map[string]interface{}{"type": "object"},
			},
		},
	}
}

// systemPrimingContent returns the synthesized system-priming message
// ClaudeToKiro tucks at the front of history (see ClaudeToKiro's "Keep system
// instructions in history instead of user content"), or "" if none was added.
func systemPrimingContent(payload *KiroPayload) string {
	if len(payload.ConversationState.History) == 0 {
		return ""
	}
	first := payload.ConversationState.History[0]
	if first.UserInputMessage == nil {
		return ""
	}
	return first.UserInputMessage.Content
}

// TestAgentToolSteeringDisabledByDefault pins F1: with the new
// config.AgentToolSteering flag left at its zero value (default off), a
// tool-bearing request must translate byte-identically to pre-PR-#6
// behavior — neither agentToolExecutionPrompt (system prompt) nor
// agentToolTurnReminder (final user turn) may appear anywhere in the
// translated payload. Reverting the `&& agentToolSteeringEnabled` guards in
// translator.go (restoring PR #6's unconditional injection) makes this fail.
func TestAgentToolSteeringDisabledByDefault(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if got := config.GetAgentToolSteering(); got {
		t.Fatalf("expected AgentToolSteering to default to false, got %t", got)
	}

	req := toolBearingClaudeRequest()
	payload := ClaudeToKiro(req, false)

	systemPrompt := systemPrimingContent(payload)
	if strings.Contains(systemPrompt, agentToolExecutionPrompt) {
		t.Fatalf("flag off: system prompt must not contain agentToolExecutionPrompt, got %q", systemPrompt)
	}
	if systemPrompt != "You are a helpful assistant." {
		t.Fatalf("flag off: system prompt must be byte-identical to the client's own system text, got %q", systemPrompt)
	}

	finalContent := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if strings.Contains(finalContent, agentToolTurnReminder) {
		t.Fatalf("flag off: final user message must not contain agentToolTurnReminder, got %q", finalContent)
	}
	if finalContent != "please read the file" {
		t.Fatalf("flag off: final user message must be byte-identical to the client's own content, got %q", finalContent)
	}
}

// TestAgentToolSteeringEnabledInjectsPrompts pins the opt-in path: with the
// flag explicitly turned on, both constants must appear in the right places
// — agentToolExecutionPrompt appended to the system prompt, and
// agentToolTurnReminder appended to the current (final) user message.
func TestAgentToolSteeringEnabledInjectsPrompts(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateAgentToolSteering(true); err != nil {
		t.Fatalf("UpdateAgentToolSteering: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateAgentToolSteering(false) })

	req := toolBearingClaudeRequest()
	payload := ClaudeToKiro(req, false)

	systemPrompt := systemPrimingContent(payload)
	if !strings.Contains(systemPrompt, agentToolExecutionPrompt) {
		t.Fatalf("flag on: expected agentToolExecutionPrompt in system prompt, got %q", systemPrompt)
	}
	if !strings.HasPrefix(systemPrompt, "You are a helpful assistant.") {
		t.Fatalf("flag on: expected the client's own system text to be preserved ahead of the injection, got %q", systemPrompt)
	}

	finalContent := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(finalContent, agentToolTurnReminder) {
		t.Fatalf("flag on: expected agentToolTurnReminder in the final user message, got %q", finalContent)
	}
	if !strings.HasPrefix(finalContent, "please read the file") {
		t.Fatalf("flag on: expected the client's own message to be preserved ahead of the injection, got %q", finalContent)
	}
}

// TestAgentToolSteeringEnabledButToolChoiceNoneSkipsInjection pins the
// existing tool_choice:"none" gate: even with the flag on, a request that
// explicitly opts out of tool use must not get either injection.
func TestAgentToolSteeringEnabledButToolChoiceNoneSkipsInjection(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateAgentToolSteering(true); err != nil {
		t.Fatalf("UpdateAgentToolSteering: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateAgentToolSteering(false) })

	req := toolBearingClaudeRequest()
	req.ToolChoice = map[string]interface{}{"type": "none"}
	payload := ClaudeToKiro(req, false)

	systemPrompt := systemPrimingContent(payload)
	if strings.Contains(systemPrompt, agentToolExecutionPrompt) {
		t.Fatalf("tool_choice=none: system prompt must not contain agentToolExecutionPrompt, got %q", systemPrompt)
	}
	finalContent := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if strings.Contains(finalContent, agentToolTurnReminder) {
		t.Fatalf("tool_choice=none: final user message must not contain agentToolTurnReminder, got %q", finalContent)
	}
}

// TestAgentToolSteeringEnabledButNoToolsSkipsInjection covers the other half
// of the existing gate: even with the flag on, a request carrying no tools at
// all must not get either injection.
func TestAgentToolSteeringEnabledButNoToolsSkipsInjection(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateAgentToolSteering(true); err != nil {
		t.Fatalf("UpdateAgentToolSteering: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateAgentToolSteering(false) })

	req := toolBearingClaudeRequest()
	req.Tools = nil
	payload := ClaudeToKiro(req, false)

	systemPrompt := systemPrimingContent(payload)
	if strings.Contains(systemPrompt, agentToolExecutionPrompt) {
		t.Fatalf("no tools: system prompt must not contain agentToolExecutionPrompt, got %q", systemPrompt)
	}
	finalContent := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if strings.Contains(finalContent, agentToolTurnReminder) {
		t.Fatalf("no tools: final user message must not contain agentToolTurnReminder, got %q", finalContent)
	}
}
