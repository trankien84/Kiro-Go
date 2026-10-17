package proxy

import (
	"fmt"
	"strings"
)

// claudeOpus55RequestViolation checks the native Messages contract without
// rewriting client intent. It is applied only after the actual provider model
// is resolved, and only to official native Anthropic/Bedrock transports.
// Transparent compatible deployments retain ownership of their API contract.
func claudeOpus55RequestViolation(req *ClaudeRequest) string {
	if req == nil {
		return ""
	}
	if req.Thinking != nil {
		if !strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "adaptive") {
			return "Claude Opus 5.5 requires adaptive thinking; thinking.type enabled/disabled is unsupported"
		}
		if req.Thinking.BudgetTokens != 0 {
			return "Claude Opus 5.5 does not support manual thinking budgets"
		}
		if _, hasBudget := req.Thinking.presentKnownFields["budget_tokens"]; hasBudget {
			return "Claude Opus 5.5 does not accept thinking.budget_tokens"
		}
	}
	if req.ToolChoice != nil {
		choice, ok := req.ToolChoice.(map[string]interface{})
		if ok {
			kind, _ := choice["type"].(string)
			switch strings.ToLower(strings.TrimSpace(kind)) {
			case "any", "tool":
				return "Claude Opus 5.5 does not support forced tool_choice; use auto or none"
			}
		}
	}
	if req.hasKnownJSONField("temperature") && req.Temperature != 1 {
		return "Claude Opus 5.5 accepts temperature only at its default of 1"
	}
	if req.hasKnownJSONField("top_p") && (req.TopP < 0.99 || req.TopP > 1) {
		return "Claude Opus 5.5 accepts top_p only at its backwards-compatible default (>= 0.99)"
	}
	if _, exists := req.Extra["top_k"]; exists {
		return "Claude Opus 5.5 does not support top_k"
	}
	// Validate explicit effort regardless of whether the selected provider's
	// discovery endpoint includes capability metadata yet. Omission must keep
	// the upstream default (medium), not synthesize a field.
	effort := strings.TrimSpace(req.ReasoningEffort)
	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		effort = strings.TrimSpace(req.OutputConfig.Effort)
	}
	if effort != "" {
		switch canonicalEffort(effort) {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Sprintf("Claude Opus 5.5 does not support effort %q", effort)
		}
	}
	return ""
}
