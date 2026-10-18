package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

func claudeKiroReasoningRequiresNativeField(req *ClaudeRequest) bool {
	if req == nil {
		return false
	}
	if strings.TrimSpace(claudeReasoningEffort(req)) != "" {
		return true
	}
	if req.Thinking == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(req.Thinking.Type)) {
	case "enabled", "adaptive":
		return true
	default:
		return false
	}
}

// buildClaudeKiroCandidateAdditionalFields rebuilds Kiro's model-specific
// request fields from the exact immutable route selected for one account. The
// public/aggregate model view is presentation data and must never authorize
// reasoning or structured-output fields for a different Kiro credential.
func (h *Handler) buildClaudeKiroCandidateAdditionalFields(
	semanticBody []byte,
	route resolvedProviderModel,
) (map[string]interface{}, error) {
	if len(semanticBody) == 0 {
		return nil, nil
	}

	var req ClaudeRequest
	if err := json.Unmarshal(semanticBody, &req); err != nil {
		return nil, fmt.Errorf("decode normalized Claude request for Kiro candidate: %w", err)
	}

	capability := route.Reasoning
	if strings.TrimSpace(capability.ModelID) == "" {
		capability.ModelID = string(route.ProviderID)
	}

	fields, reasoningRequested, err := BuildClaudeAdditionalModelRequestFields(&req, capability)
	if err != nil {
		return nil, fmt.Errorf(
			"provider_model=%s cannot preserve Kiro reasoning controls: %w",
			route.ProviderID,
			err,
		)
	}
	if reasoningRequested && !route.ReasoningKnown {
		return nil, fmt.Errorf(
			"provider_model=%s has no candidate-local Kiro reasoning capability evidence",
			route.ProviderID,
		)
	}
	if reasoningRequested && len(fields) == 0 && claudeKiroReasoningRequiresNativeField(&req) {
		return nil, fmt.Errorf(
			"provider_model=%s does not expose a Kiro reasoning field for the requested controls",
			route.ProviderID,
		)
	}

	if _, formatRequested := claudeKiroStructuredOutputFormat(&req); formatRequested {
		if !route.KiroStructuredOutputKnown {
			return nil, fmt.Errorf(
				"provider_model=%s has no candidate-local Kiro output_config.format capability evidence",
				route.ProviderID,
			)
		}
		if !route.KiroStructuredOutputFormat {
			return nil, fmt.Errorf(
				"provider_model=%s does not advertise output_config.format for this Kiro account",
				route.ProviderID,
			)
		}
	}

	fields, err = h.projectClaudeStructuredOutputToKiro(
		&req,
		string(route.ProviderID),
		fields,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"provider_model=%s cannot preserve Kiro structured output: %w",
			route.ProviderID,
			err,
		)
	}
	return fields, nil
}
