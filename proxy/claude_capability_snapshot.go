package proxy

import "strings"

// claudeCapabilitySnapshot is the immutable, provider-agnostic model view
// attached to a parsed Claude request. Protocol facts and maintained public
// model semantics are resolved once. Deployment/account-specific facts are
// evaluated later while selecting a concrete transport candidate.
type claudeCapabilitySnapshot struct {
	ModelID                string
	ProviderReasoning      ModelReasoningCapability
	ProviderReasoningKnown bool
	Protocol               claudeModelCapabilities
}

func resolveClaudeCapabilitySnapshot(model string) claudeCapabilitySnapshot {
	return resolveClaudeCapabilitySnapshotWithReasoning(
		model,
		resolveClaudeProviderReasoningCapability,
	)
}

func resolveClaudeCapabilitySnapshotWithReasoning(
	model string,
	reasoningResolver claudeReasoningCapabilityResolver,
) claudeCapabilitySnapshot {
	model = strings.TrimSpace(model)
	snapshot := claudeCapabilitySnapshot{
		ModelID:  model,
		Protocol: resolveClaudeModelCapabilities(model),
	}
	if reasoningResolver != nil {
		snapshot.ProviderReasoning = reasoningResolver(model)
		snapshot.ProviderReasoningKnown = strings.TrimSpace(
			snapshot.ProviderReasoning.ModelID,
		) != ""
	}
	return snapshot
}

func normalizeClaudeCapabilitySnapshot(
	model string,
	snapshot claudeCapabilitySnapshot,
) claudeCapabilitySnapshot {
	model = strings.TrimSpace(model)
	if snapshot.ModelID == "" || strings.TrimSpace(snapshot.ModelID) != model {
		// A mismatched snapshot is an internal invariant violation. Fail closed
		// instead of silently re-resolving and creating a second capability view
		// inside Completion Guard or transport planning.
		return claudeCapabilitySnapshot{ModelID: model}
	}
	return snapshot
}

func claudeFeatureSupportState(
	capability claudeFeatureSupport,
) (supported bool, known bool) {
	switch capability {
	case claudeFeatureSupported:
		return true, true
	case claudeFeatureUnsupported:
		return false, true
	default:
		return false, false
	}
}
