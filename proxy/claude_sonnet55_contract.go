package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// claudeSonnet55RequestViolation validates the official Sonnet 5.5 contract
// without mutating the request. Enforcement is candidate/provider-specific:
// custom compatible endpoints retain their own (possibly different) contract.
func claudeSonnet55RequestViolation(req *ClaudeRequest) string {
	if req == nil {
		return ""
	}
	if req.Thinking != nil {
		mode := strings.ToLower(strings.TrimSpace(req.Thinking.Type))
		if mode != "adaptive" && mode != "between_tools" {
			return "Claude Sonnet 5.5 accepts only adaptive or between_tools thinking"
		}
		if _, present := req.Thinking.presentKnownFields["budget_tokens"]; present {
			return "Claude Sonnet 5.5 does not accept thinking.budget_tokens"
		}
		if req.Thinking.BudgetTokens != 0 {
			return "Claude Sonnet 5.5 does not accept manual thinking budgets"
		}
		if mode == "between_tools" {
			// Presence is forbidden even when the field is empty/zero.
			if _, present := req.Thinking.presentKnownFields["display"]; present {
				return "Claude Sonnet 5.5 between_tools does not accept thinking.display"
			}
			if req.Thinking.Display != "" {
				return "Claude Sonnet 5.5 between_tools does not accept thinking.display"
			}
			if _, present := req.Thinking.Extra["block_binding"]; present {
				return "Claude Sonnet 5.5 between_tools does not accept thinking.block_binding"
			}
		}
	}
	if choice, ok := req.ToolChoice.(map[string]interface{}); ok {
		kind, _ := choice["type"].(string)
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "any" || kind == "tool" {
			return "Claude Sonnet 5.5 does not support forced tool_choice"
		}
	}
	if req.hasKnownJSONField("temperature") && req.Temperature != 1 {
		return "Claude Sonnet 5.5 accepts only default temperature"
	}
	if req.hasKnownJSONField("top_p") && req.TopP != 1 {
		return "Claude Sonnet 5.5 accepts only default top_p"
	}
	if _, present := req.Extra["top_k"]; present {
		// Official native request rejects top_k, including null/zero.
		return "Claude Sonnet 5.5 does not support top_k"
	}
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		effort = strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort))
	}
	if effort != "" {
		switch canonicalEffort(effort) {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Sprintf("Claude Sonnet 5.5 does not support effort %q", effort)
		}
	}
	if req.Thinking != nil && strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "between_tools") {
		switch canonicalEffort(effort) {
		case "", "low", "medium", "high":
		default:
			return "Claude Sonnet 5.5 between_tools supports only low/medium/high effort"
		}
		// Changes to effort in message-scoped output_config are not allowed in
		// between_tools. They cannot be silently dropped or rewritten.
		baseEffort := canonicalEffort(effort)
		if baseEffort == "" {
			baseEffort = "high"
		} // documented upstream default
		for _, message := range req.Messages {
			if message.Extra == nil {
				continue
			}
			raw, exists := message.Extra["output_config"]
			if !exists {
				continue
			}
			var override struct {
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(raw, &override); err != nil {
				return "Claude Sonnet 5.5 between_tools has invalid per-message output_config"
			}
			if override.Effort != "" && canonicalEffort(override.Effort) != baseEffort {
				return "Claude Sonnet 5.5 between_tools does not allow per-message effort changes"
			}
		}
	}
	return ""
}
