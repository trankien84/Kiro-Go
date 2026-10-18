package proxy

import (
	"strings"
	"testing"
)

func TestFallbackClaudeReasoningCapabilityForOpus45(t *testing.T) {
	capability := fallbackClaudeReasoningCapability("claude-opus-4.5")
	if !capability.SupportsThinking {
		t.Fatal("expected Opus 4.5 fallback to support thinking")
	}
	if !capability.SupportsBudgetTokens {
		t.Fatal("expected Opus 4.5 budget_tokens support")
	}
	if capability.EffortPath != ReasoningSchemaOutputConfig {
		t.Fatalf("effort path = %q, want output_config", capability.EffortPath)
	}
	if got := strings.Join(capability.Efforts, ","); got != "low,medium,high" {
		t.Fatalf("efforts = %q, want low,medium,high", got)
	}
	if capability.DefaultThinkingType != "" {
		t.Fatalf(
			"default thinking type = %q, want opt-in",
			capability.DefaultThinkingType,
		)
	}
}
