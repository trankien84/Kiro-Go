package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"kiro-go/config"
)

// claudeRequestPlan is the immutable semantic view used after request parsing.
// Every validator, router and transport adapter must consume this normalized
// request instead of rebuilding meaning from the original wire body.
type claudeRequestPlan struct {
	Request           ClaudeRequest
	RequestedModel    string
	ResolvedModel     string
	ThinkingRequested bool
	LegacySuffixOnly  bool
	// Capabilities is the single model-capability snapshot for this request.
	// Capability and ProviderCapabilityKnown remain compatibility aliases for
	// existing reasoning call sites while they are migrated incrementally.
	Capabilities            claudeCapabilitySnapshot
	Capability              ModelReasoningCapability
	ProviderCapabilityKnown bool
	Semantic                claudeSemanticRequest
	Content                 claudeContentInventory
	KiroProjection          claudeKiroProjection
	Catalog                 *completionGuardNormalizedCatalog
	Requirements            claudeTransportRequirements
}

type claudeReasoningCapabilityResolver func(string) ModelReasoningCapability

type claudeModelThinkingPlan struct {
	RequestedModel          string
	ResolvedModel           string
	ThinkingRequested       bool
	LegacySuffixOnly        bool
	Capabilities            claudeCapabilitySnapshot
	Capability              ModelReasoningCapability
	ProviderCapabilityKnown bool
}

func normalizeClaudeModelAndThinking(
	req *ClaudeRequest,
	semantic claudeSemanticRequest,
	thinkingSuffix string,
	capabilityForModel claudeReasoningCapabilityResolver,
) (claudeModelThinkingPlan, *claudeTransportValidationError) {
	result := claudeModelThinkingPlan{}
	if req == nil {
		return result, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "request must not be nil",
		}
	}
	result.RequestedModel = req.Model
	resolvedModel, suffixThinking := ParseModelAndThinking(req.Model, thinkingSuffix)
	result.ResolvedModel = resolvedModel
	req.Model = resolvedModel
	result.Capabilities = resolveClaudeCapabilitySnapshotWithReasoning(
		resolvedModel,
		capabilityForModel,
	)
	result.Capability = result.Capabilities.ProviderReasoning
	result.ProviderCapabilityKnown = result.Capabilities.ProviderReasoningKnown

	// The suffix is Kiro-Go syntax, not an Anthropic wire feature. An explicit
	// thinking object always wins. Convert the suffix to adaptive only when the
	// resolved model advertises that exact shape; otherwise retain Kiro-only
	// legacy prompting without leaking the private suffix upstream.
	if suffixThinking && req.Thinking == nil {
		if adaptive, ok := matchThinkingType("adaptive", result.Capability.ThinkingTypes); result.Capability.SupportsThinking && ok {
			req.Thinking = &ClaudeThinkingConfig{Type: adaptive}
		} else {
			result.LegacySuffixOnly = true
		}
	}
	result.ThinkingRequested = result.LegacySuffixOnly || isClaudeThinkingRequested(req.Thinking)

	if result.ProviderCapabilityKnown {
		if reason := validateClaudeThinkingForCapability(
			req.Thinking,
			req.MaxTokens,
			len(req.Tools) > 0,
			claudeManualInterleavedThinkingEnabledForBetas(
				semantic.Betas,
				resolvedModel,
				req.Thinking,
				result.Capability,
			),
			result.Capability,
		); reason != "" {
			return result, &claudeTransportValidationError{Code: "invalid_request_error", Message: reason}
		}
	}
	return result, nil
}

func (h *Handler) buildClaudeRequestPlan(
	rawBody []byte,
	headers http.Header,
) (claudeRequestPlan, *claudeTransportValidationError) {
	thinkingConfig := config.GetThinkingConfig()
	return buildClaudeRequestPlanWithResolver(
		rawBody,
		headers,
		thinkingConfig.Suffix,
		resolveClaudeProviderReasoningCapability,
	)
}

func buildClaudeRequestPlanWithResolver(
	rawBody []byte,
	headers http.Header,
	thinkingSuffix string,
	capabilityForModel claudeReasoningCapabilityResolver,
) (claudeRequestPlan, *claudeTransportValidationError) {
	plan := claudeRequestPlan{}
	if err := json.Unmarshal(rawBody, &plan.Request); err != nil {
		return plan, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "Invalid JSON: " + err.Error(),
		}
	}
	if len(plan.Request.Messages) == 0 {
		return plan, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "messages must not be empty",
		}
	}
	if reason := validateClaudeThinkingConfig(plan.Request.Thinking, plan.Request.MaxTokens); reason != "" {
		return plan, &claudeTransportValidationError{Code: "invalid_request_error", Message: reason}
	}

	semantic, reason := buildClaudeSemanticRequest(&plan.Request, headers, rawBody)
	if reason != "" {
		return plan, &claudeTransportValidationError{Code: "invalid_request_error", Message: reason}
	}
	if reason := validateClaudeMidConversationSystemMessagesWithBetas(plan.Request.Messages, semantic.Betas); reason != "" {
		return plan, &claudeTransportValidationError{Code: "invalid_request_error", Message: reason}
	}
	if plan.Request.Thinking != nil && strings.EqualFold(strings.TrimSpace(plan.Request.Thinking.Display), "updates") {
		if !semantic.Betas.Contains("thinking-display-updates-2026-08-18") {
			return plan, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: "thinking.display updates requires anthropic-beta thinking-display-updates-2026-08-18",
			}
		}
		if !strings.EqualFold(strings.TrimSpace(plan.Request.Thinking.Type), "adaptive") {
			return plan, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: "thinking.display updates requires adaptive thinking",
			}
		}
	}

	modelThinking, validationErr := normalizeClaudeModelAndThinking(
		&plan.Request,
		semantic,
		thinkingSuffix,
		capabilityForModel,
	)
	if validationErr != nil {
		return plan, validationErr
	}
	plan.RequestedModel = modelThinking.RequestedModel
	plan.ResolvedModel = modelThinking.ResolvedModel
	plan.ThinkingRequested = modelThinking.ThinkingRequested
	plan.LegacySuffixOnly = modelThinking.LegacySuffixOnly
	plan.Capabilities = modelThinking.Capabilities
	plan.Capability = modelThinking.Capability
	plan.ProviderCapabilityKnown = modelThinking.ProviderCapabilityKnown

	if validationErr := canonicalizeClaudeCompatibilityContent(&plan.Request); validationErr != nil {
		return plan, validationErr
	}

	plan.Catalog, reason = completionGuardBuildNormalizedCatalogWithOptions(
		&plan.Request,
		completionGuardCatalogBuildOptions{
			AllowAllDeferred: claudeRequestHasMidConversationToolChangesRaw(
				plan.Request.Messages,
			),
		},
	)
	if reason != "" {
		return plan, &claudeTransportValidationError{Code: "invalid_request_error", Message: reason}
	}
	if validationErr := validateClaudeInlineToolHistory(&plan.Request, plan.Catalog, semantic.Betas); validationErr != nil {
		return plan, validationErr
	}
	if reason := validateNativeToolsetHistory(&plan.Request, plan.Catalog); reason != "" {
		return plan, &claudeTransportValidationError{Code: "invalid_request_error", Message: reason}
	}
	logClaudeToolFeatures(plan.Catalog)
	if content, validationErr := buildClaudeContentInventory(&plan.Request, plan.Catalog); validationErr != nil {
		return plan, validationErr
	} else {
		plan.Content = content
	}
	if !plan.Content.HasUserContext {
		return plan, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "at least one non-empty user message is required",
		}
	}
	if validationErr := validateClaudeContentBetas(plan.Content, semantic); validationErr != nil {
		return plan, validationErr
	}
	plan.KiroProjection = buildClaudeKiroProjection(&plan.Request, plan.Content, semantic)

	canonicalBody, err := json.Marshal(&plan.Request)
	if err != nil {
		return plan, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: fmt.Sprintf("Failed to normalize Claude request: %v", err),
		}
	}
	semantic.Body = canonicalBody
	plan.Semantic = semantic

	if requirements, validationErr := analyzeClaudeTransportRequirementsWithInventoryAndCapabilities(
		&plan.Request,
		plan.Semantic,
		plan.ResolvedModel,
		plan.Content,
		plan.Catalog,
		plan.Capabilities.Protocol,
		plan.KiroProjection,
	); validationErr != nil {
		return plan, validationErr
	} else {
		requirements.ProviderReasoning = plan.Capabilities.ProviderReasoning
		requirements.ProviderReasoningKnown = plan.Capabilities.ProviderReasoningKnown
		requirements.RequestBodyBytes = len(semantic.Body)
		plan.Requirements = requirements
		plan.KiroProjection = requirements.KiroProjection
	}
	if plan.LegacySuffixOnly {
		plan.Requirements.Require(
			"kiro_legacy_thinking_suffix",
			claudeTransportMaskKiro,
			"the private model suffix cannot be represented as a valid Anthropic thinking request for this model",
		)
	}
	if plan.Requirements.Allowed == 0 {
		return plan, &claudeTransportValidationError{
			Code:       "invalid_request_error",
			DetailCode: "unsupported_transport_feature",
			Message: "Claude request features require mutually incompatible transports: " +
				plan.Requirements.Summary(),
		}
	}

	return plan, nil
}
