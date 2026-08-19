package proxy

import (
	"encoding/json"
	"strings"
)

type completionGuardModelTransition struct {
	Index int
	From  string
	To    string
}

type completionGuardModelTransitionReport struct {
	Valid              bool
	RequestedModel     string
	ResponseModel      string
	Transitions        []completionGuardModelTransition
	FinalBoundaryIndex int
	Reason             string
}

func completionGuardFallbackEndpointModel(
	block ClaudeContentBlock,
	key string,
) (string, bool) {
	raw, exists := block.Extra[key]
	if !exists || len(raw) == 0 {
		return "", false
	}
	var endpoint struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &endpoint) != nil {
		return "", false
	}
	endpoint.Model = strings.TrimSpace(endpoint.Model)
	return endpoint.Model, endpoint.Model != ""
}

func completionGuardModelTransitions(
	content []ClaudeContentBlock,
) ([]completionGuardModelTransition, string) {
	transitions := make([]completionGuardModelTransition, 0)
	seenModels := make(map[string]struct{})
	for index, block := range content {
		if strings.TrimSpace(block.Type) != "fallback" {
			continue
		}
		from, fromOK := completionGuardFallbackEndpointModel(block, "from")
		to, toOK := completionGuardFallbackEndpointModel(block, "to")
		if !fromOK || !toOK {
			return nil, "invalid_fallback_block"
		}
		if from == to {
			return nil, "fallback_without_model_transition"
		}
		if len(transitions) > 0 && transitions[len(transitions)-1].To != from {
			return nil, "discontinuous_fallback_chain"
		}
		if len(transitions) == 0 {
			seenModels[from] = struct{}{}
		}
		key := to
		if _, exists := seenModels[key]; exists {
			return nil, "cyclic_fallback_chain"
		}
		seenModels[key] = struct{}{}
		transitions = append(transitions, completionGuardModelTransition{
			Index: index,
			From:  from,
			To:    to,
		})
	}
	return transitions, ""
}

// completionGuardValidateModelTransitionForRequest validates both the internal
// fallback chain and its external anchors. Anthropic defines the first
// from.model as the model sent in the request and the final to.model as the
// top-level response.model that produced the final answer.
func completionGuardValidateModelTransitionForRequest(
	requestedModel string,
	resp *ClaudeResponse,
) completionGuardModelTransitionReport {
	report := completionGuardModelTransitionReport{
		Valid:              true,
		RequestedModel:     strings.TrimSpace(requestedModel),
		FinalBoundaryIndex: -1,
	}
	if resp == nil {
		return report
	}
	report.ResponseModel = strings.TrimSpace(resp.Model)
	transitions, reason := completionGuardModelTransitions(resp.Content)
	report.Transitions = transitions
	if len(transitions) > 0 {
		report.FinalBoundaryIndex = transitions[len(transitions)-1].Index
	}
	if reason != "" {
		report.Valid = false
		report.Reason = reason
		return report
	}
	if len(transitions) == 0 {
		return report
	}
	if report.RequestedModel != "" && transitions[0].From != report.RequestedModel {
		report.Valid = false
		report.Reason = "fallback_request_model_mismatch"
		return report
	}
	if report.ResponseModel == "" || transitions[len(transitions)-1].To != report.ResponseModel {
		report.Valid = false
		report.Reason = "fallback_response_model_mismatch"
		return report
	}
	return report
}

// completionGuardValidateModelTransition keeps the internal-chain validator
// available to focused unit tests and compatibility call sites. Production
// response classification uses the anchored validator above.
func completionGuardValidateModelTransition(resp *ClaudeResponse) string {
	if resp == nil {
		return ""
	}
	_, reason := completionGuardModelTransitions(resp.Content)
	return reason
}
