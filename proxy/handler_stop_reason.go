package proxy

import "strings"

// resolveClaudeHandlerStopReason preserves the real Kiro terminal condition
// across the Anthropic compatibility surface. The fallback exists only for
// legacy/test callbacks that predate metadataEvent.stopReason.
func resolveClaudeHandlerStopReason(upstream string, toolCallCount int) string {
	reason := strings.ToLower(strings.TrimSpace(upstream))
	switch reason {
	case "end_turn", "stop_sequence", "tool_use", "max_tokens", "pause_turn", "refusal", "model_context_window_exceeded":
		return reason
	case "max_output_tokens", "length":
		return "max_tokens"
	case "context_window_exceeded", "context_length_exceeded":
		return "model_context_window_exceeded"
	case "content_filter", "content_filtered", "guardrail_intervened":
		return "refusal"
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "":
		if toolCallCount > 0 {
			return "tool_use"
		}
		return "end_turn"
	default:
		// Anthropic stop_reason is an extensible string. Preserve an unfamiliar
		// upstream value instead of silently rewriting it to normal completion.
		return strings.TrimSpace(upstream)
	}
}

// resolveOpenAIHandlerFinishReason maps Kiro/Anthropic terminal reasons onto
// Chat Completions finish_reason without inventing a successful "stop" for a
// max-token, context-window, policy, or tool-use termination.
func resolveOpenAIHandlerFinishReason(upstream string, toolCallCount int) string {
	reason := strings.ToLower(strings.TrimSpace(upstream))
	switch reason {
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "max_tokens", "max_output_tokens", "model_context_window_exceeded", "context_window_exceeded", "context_length_exceeded", "length":
		return "length"
	case "content_filter", "content_filtered", "refusal", "guardrail_intervened":
		return "content_filter"
	case "end_turn", "stop_sequence", "stop":
		return "stop"
	case "":
		if toolCallCount > 0 {
			return "tool_calls"
		}
		return "stop"
	default:
		// OpenAI-compatible clients commonly tolerate provider-specific strings,
		// and preserving an unknown terminal reason is safer than declaring a
		// normal stop. Future adapters can add an explicit mapping when needed.
		return strings.TrimSpace(upstream)
	}
}
