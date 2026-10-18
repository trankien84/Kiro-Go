package proxy

import (
	"bytes"
	"strings"
)

func completionGuardRequiresTerminalBufferWithCapabilities(
	req ClaudeRequest,
	resolvedModel string,
	capabilities claudeCapabilitySnapshot,
) bool {
	// Starting with Claude 4, a classifier refusal can arrive after partial
	// output, and Anthropic requires that partial output to be discarded. A live
	// deferred stream cannot roll bytes back after they reach the client.
	model := strings.TrimSpace(resolvedModel)
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	capabilities = normalizeClaudeCapabilitySnapshot(model, capabilities)
	if capabilities.Protocol.StreamedRefusal != claudeFeatureUnsupported {
		return true
	}

	raw, exists := req.Extra["fallbacks"]
	if !exists {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func completionGuardRequiresTerminalBuffer(
	req ClaudeRequest,
	resolvedModel string,
) bool {
	model := strings.TrimSpace(resolvedModel)
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	return completionGuardRequiresTerminalBufferWithCapabilities(
		req,
		model,
		resolveClaudeCapabilitySnapshot(model),
	)
}
