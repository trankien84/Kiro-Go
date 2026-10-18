package proxy

import "testing"

func TestResolveClaudeModelCapabilitiesUsesExactFamilies(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  claudeFeatureSupport
	}{
		{"claude-fable-5", claudeFeatureSupported},
		{"claude-mythos-5-20260701", claudeFeatureSupported},
		{"claude-opus-4-8", claudeFeatureSupported},
		{"anthropic/claude-opus-5:0", claudeFeatureSupported},
		{"claude-sonnet-5", claudeFeatureUnsupported},
		{"not-really-opus-5", claudeFeatureUnknown},
		{"not-really-claude-opus-5", claudeFeatureUnknown},
		{"private-opus-alias", claudeFeatureUnknown},
	} {
		got := resolveClaudeModelCapabilities(tc.model).MidConversationSystem
		if got != tc.want {
			t.Fatalf("model=%q got=%v want=%v", tc.model, got, tc.want)
		}
	}
}

func TestClaudeModelCapabilitiesNormalizeProviderQualifiedIDs(t *testing.T) {
	for _, model := range []string{
		"claude-opus-5-0",
		"claude-opus-5-thinking",
		"anthropic/claude-opus-4.8-thinking",
		"anthropic.claude-opus-5",
		"us.anthropic.claude-opus-4-8-20260701-v1:0",
	} {
		if got := resolveClaudeModelCapabilities(model).MidConversationSystem; got != claudeFeatureSupported {
			t.Fatalf("model=%q support=%v", model, got)
		}
	}
	// Arbitrary compatible provider namespaces are opaque, even when their
	// suffix resembles an official Claude ID. Native capabilities require an
	// exact model/provider contract, not a string heuristic.
	if got := resolveClaudeModelCapabilities("kr/claude-opus-4.8-thinking").MidConversationSystem; got != claudeFeatureUnknown {
		t.Fatalf("opaque kr/ model borrowed Claude capability: %v", got)
	}
}

func TestClaudeAssistantPrefillCapabilityBoundary(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  claudeFeatureSupport
	}{
		{"claude-sonnet-4.5", claudeFeatureSupported},
		{"claude-haiku-4.5", claudeFeatureSupported},
		{"claude-3-7-sonnet-20250219", claudeFeatureSupported},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", claudeFeatureSupported},
		{"claude-2.1", claudeFeatureSupported},
		{"claude-sonnet-4.6", claudeFeatureUnsupported},
		{"claude-opus-4.8", claudeFeatureUnsupported},
		{"claude-mythos-5", claudeFeatureUnsupported},
		{"private-sonnet-alias", claudeFeatureUnknown},
	} {
		if got := resolveClaudeModelCapabilities(tc.model).AssistantPrefill; got != tc.want {
			t.Fatalf("model=%q got=%v want=%v", tc.model, got, tc.want)
		}
	}
}

func TestClaudeProtocolCapabilityRegistryFeedsCompletionGuard(t *testing.T) {
	for _, tc := range []struct {
		model        string
		refusal      claudeFeatureSupport
		programmatic claudeFeatureSupport
		toolSearch   claudeFeatureSupport
	}{
		{"claude-opus-4.4", claudeFeatureSupported, claudeFeatureUnsupported, claudeFeatureUnsupported},
		{"claude-opus-4.5", claudeFeatureSupported, claudeFeatureSupported, claudeFeatureSupported},
		{"claude-haiku-4.5", claudeFeatureSupported, claudeFeatureUnsupported, claudeFeatureSupported},
		{"claude-fable-5", claudeFeatureSupported, claudeFeatureSupported, claudeFeatureSupported},
		{"claude-opus-5", claudeFeatureSupported, claudeFeatureSupported, claudeFeatureSupported},
		{"claude-sonnet-5", claudeFeatureSupported, claudeFeatureSupported, claudeFeatureUnsupported},
		{"claude-sonnet-6", claudeFeatureSupported, claudeFeatureUnknown, claudeFeatureUnknown},
		{"claude-sonnet-3.7", claudeFeatureUnsupported, claudeFeatureUnsupported, claudeFeatureUnsupported},
		{"private-model-alias", claudeFeatureUnknown, claudeFeatureUnknown, claudeFeatureUnknown},
	} {
		caps := resolveClaudeModelCapabilities(tc.model)
		if caps.StreamedRefusal != tc.refusal ||
			caps.ProgrammaticToolCalling != tc.programmatic ||
			caps.ToolSearch != tc.toolSearch {
			t.Fatalf("model=%q caps=%#v", tc.model, caps)
		}
	}
}
