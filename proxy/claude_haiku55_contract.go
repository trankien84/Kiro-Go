package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// claudeHaiku55RequestViolation checks the exact official Haiku 5.5 Messages
// contract without rewriting client fields. The terminal provider is selected
// before this is enforced; compatible endpoints own their own semantics.
func claudeHaiku55RequestViolation(req *ClaudeRequest) string {
	if req == nil {
		return ""
	}
	if req.Thinking != nil {
		mode := strings.ToLower(strings.TrimSpace(req.Thinking.Type))
		if mode != "adaptive" && mode != "disabled" {
			return "Claude Haiku 5.5 accepts only adaptive or disabled thinking"
		}
		if _, present := req.Thinking.presentKnownFields["budget_tokens"]; present {
			return "Claude Haiku 5.5 does not accept thinking.budget_tokens"
		}
		if req.Thinking.BudgetTokens != 0 {
			return "Claude Haiku 5.5 does not support manual thinking budgets"
		}
		if mode == "disabled" {
			if _, present := req.Thinking.presentKnownFields["display"]; present || req.Thinking.Display != "" {
				return "Claude Haiku 5.5 disabled thinking cannot include display"
			}
		}
	}
	if req.hasKnownJSONField("temperature") && req.Temperature != 1 {
		return "Claude Haiku 5.5 accepts temperature only at 1"
	}
	if req.hasKnownJSONField("top_p") && req.TopP != 0.99 {
		return "Claude Haiku 5.5 accepts top_p only at 0.99"
	}
	if req.hasKnownJSONField("temperature") && req.hasKnownJSONField("top_p") {
		return "Claude Haiku 5.5 cannot combine temperature and top_p"
	}
	if _, exists := req.Extra["top_k"]; exists {
		return "Claude Haiku 5.5 does not support top_k"
	}
	effort := strings.TrimSpace(req.ReasoningEffort)
	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		effort = strings.TrimSpace(req.OutputConfig.Effort)
	}
	if effort != "" {
		switch canonicalEffort(effort) {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Sprintf("Claude Haiku 5.5 does not support effort %q", effort)
		}
	}
	if req.Thinking != nil && strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "disabled") {
		switch canonicalEffort(effort) {
		case "", "low", "medium", "high":
		default:
			return "Claude Haiku 5.5 disabled thinking supports only low/medium/high effort"
		}
		// With disabled thinking the effective effort may not change mid-chat.
		baseEffort := canonicalEffort(effort)
		if baseEffort == "" {
			baseEffort = "medium"
		}
		for _, message := range req.Messages {
			raw, exists := message.Extra["output_config"]
			if !exists {
				continue
			}
			var override struct {
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(raw, &override); err != nil {
				return "Claude Haiku 5.5 disabled thinking has invalid per-message output_config"
			}
			if override.Effort != "" && canonicalEffort(override.Effort) != baseEffort {
				return "Claude Haiku 5.5 disabled thinking does not allow per-message effort changes"
			}
		}
	}
	return ""
}
