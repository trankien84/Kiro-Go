package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
)

type claudeTransportKind uint8

const (
	claudeTransportKiroNative claudeTransportKind = iota
	claudeTransportCustomRelay
	claudeTransportDirectAnthropic
	claudeTransportBedrockInvoke
	claudeTransportBedrockConverse
	claudeTransportUnsupported
)

// claudeTransportCustomAPI is retained as a source-compatibility alias for
// existing tests and internal callers. Its semantics are now explicitly relay.
const claudeTransportCustomAPI = claudeTransportCustomRelay

func (kind claudeTransportKind) String() string {
	switch kind {
	case claudeTransportKiroNative:
		return "kiro_native"
	case claudeTransportCustomRelay:
		return "custom_api_relay"
	case claudeTransportDirectAnthropic:
		return "direct_anthropic"
	case claudeTransportBedrockInvoke:
		return "bedrock_invoke"
	case claudeTransportBedrockConverse:
		return "bedrock_converse"
	case claudeTransportUnsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

type claudeTransportMask uint8

const (
	claudeTransportMaskKiro claudeTransportMask = 1 << iota
	// Relay is a network hop to another Kiro-Go selector. It is intentionally
	// excluded from feature-specific masks because it is not a terminal
	// implementation of native Anthropic Messages semantics.
	claudeTransportMaskRelay
	// Custom retains its historical name in requirement declarations, but now
	// means an explicitly configured direct Anthropic-compatible endpoint.
	claudeTransportMaskCustom
	claudeTransportMaskBedrockInvoke
	claudeTransportMaskBedrockConverse

	claudeTransportMaskAll = claudeTransportMaskKiro |
		claudeTransportMaskRelay |
		claudeTransportMaskCustom |
		claudeTransportMaskBedrockInvoke |
		claudeTransportMaskBedrockConverse
)

func claudeTransportMaskForKind(kind claudeTransportKind) claudeTransportMask {
	switch kind {
	case claudeTransportKiroNative:
		return claudeTransportMaskKiro
	case claudeTransportCustomRelay:
		return claudeTransportMaskRelay
	case claudeTransportDirectAnthropic:
		return claudeTransportMaskCustom
	case claudeTransportBedrockInvoke:
		return claudeTransportMaskBedrockInvoke
	case claudeTransportBedrockConverse:
		return claudeTransportMaskBedrockConverse
	default:
		return 0
	}
}

type claudeTransportFeature struct {
	Name    string
	Reason  string
	Allowed claudeTransportMask
}

type claudeTransportRequirements struct {
	Allowed        claudeTransportMask
	Features       []claudeTransportFeature
	KiroProjection claudeKiroProjection

	// ProtocolCapabilities is the immutable request-level model snapshot. It is
	// recorded once by the request planner and consumed during candidate
	// selection; transports must not re-resolve the model independently.
	ProtocolCapabilities              claudeModelCapabilities
	ProviderReasoning                 ModelReasoningCapability
	ProviderReasoningKnown            bool
	RequiresAssistantPrefill          bool
	RequiresMidConversationSystem     bool
	RequiresMidConversationToolChange bool
	RequiresProgrammaticToolCalling   bool
	RequiresToolSearch                bool
	// RequiresKiroWebSearchMCP marks the narrow pure native web_search fast path.
	// Direct Anthropic can preserve the server tool unchanged; Kiro can execute it
	// through its MCP endpoint. Mixed native web_search + client tools remain
	// direct-only until their multi-turn server-tool semantics are proven safe.
	RequiresKiroWebSearchMCP       bool
	RequiresStructuredOutputFormat bool
	RequiresStrictToolUse          bool
	RequiresStopSequences          bool
	StopSequences                  []string

	// Reasoning intent remains the exact client request. Terminal transports
	// must support it natively; compatibility adapters may derive a separate
	// effective shape only after an account is selected.
	RequestedThinkingType  string
	RequestedBudgetTokens  int
	RequestedEffort        string
	Opus55ContractError    string
	Sonnet55ContractError  string
	Haiku55ContractError   string
	UsesBrowserToolset     bool
	UsesLegacyComputerTool bool
	RequestBodyBytes       int
}

func newClaudeTransportRequirements() claudeTransportRequirements {
	return newClaudeTransportRequirementsWithKiroProjection(newClaudeKiroProjection())
}

func newClaudeTransportRequirementsWithKiroProjection(
	projection claudeKiroProjection,
) claudeTransportRequirements {
	return claudeTransportRequirements{
		Allowed:        claudeTransportMaskAll,
		KiroProjection: projection,
	}
}

func (requirements *claudeTransportRequirements) Require(
	name string,
	allowed claudeTransportMask,
	reason string,
) {
	if requirements == nil {
		return
	}
	name = strings.TrimSpace(name)
	reason = strings.TrimSpace(reason)
	for _, existing := range requirements.Features {
		if existing.Name == name && existing.Allowed == allowed {
			return
		}
	}
	requirements.Features = append(requirements.Features, claudeTransportFeature{
		Name:    name,
		Reason:  reason,
		Allowed: allowed,
	})
	if allowed&claudeTransportMaskKiro == 0 {
		requirements.KiroProjection.add(name, claudeProjectionUnsupported, reason)
	}
	requirements.Allowed &= allowed
}

func (requirements claudeTransportRequirements) Allows(kind claudeTransportKind) bool {
	return requirements.Allowed&claudeTransportMaskForKind(kind) != 0
}

func (requirements claudeTransportRequirements) AllowsKiro() bool {
	return requirements.Allows(claudeTransportKiroNative)
}

func (requirements claudeTransportRequirements) Summary() string {
	if len(requirements.Features) == 0 {
		return "ordinary_messages"
	}
	parts := make([]string, 0, len(requirements.Features))
	for _, feature := range requirements.Features {
		if feature.Reason != "" {
			parts = append(parts, feature.Name+" ("+feature.Reason+")")
		} else {
			parts = append(parts, feature.Name)
		}
	}
	return strings.Join(parts, "; ")
}

type claudeTransportValidationError struct {
	Code       string
	DetailCode string
	Message    string
}

func (err *claudeTransportValidationError) Error() string {
	if err == nil {
		return ""
	}
	return err.Message
}

var (
	errClaudeTransportIncompatible = errors.New(
		"no configured Claude transport can represent the request",
	)
	errClaudeTransportUnavailable = errors.New(
		"no compatible Claude transport is currently available",
	)
)

// Bedrock native Invoke accepts Anthropic beta identifiers in the
// anthropic_beta body array. Keep this registry fail-closed: each entry is backed
// by Anthropic/AWS documentation and can be expanded together with tests.
var claudeBedrockInvokeBetas = map[string]struct{}{
	"compact-2026-01-12":                          {},
	"computer-use-2024-10-22":                     {},
	"computer-use-2025-01-24":                     {},
	"computer-use-2025-11-24":                     {},
	"context-1m-2025-08-07":                       {},
	"context-management-2025-06-27":               {},
	"dev-full-thinking-2025-05-14":                {},
	"effort-2025-11-24":                           {},
	"fallback-credit-2026-06-09":                  {},
	"interleaved-thinking-2025-05-14":             {},
	"mid-conversation-tool-changes-2026-07-01":    {},
	"mid-conversation-system-clear-at-2026-08-21": {},
	"output-128k-2025-02-19":                      {},
	"token-efficient-tools-2025-02-19":            {},
	"tool-examples-2025-10-29":                    {},
	"tool-search-tool-2025-10-19":                 {},
	"thinking-display-updates-2026-08-18":         {},
}

// claudeCompatibilityDeclarationBetas are known activation/declaration markers
// emitted by Anthropic-compatible clients. A marker is not itself proof that the
// request uses every feature in that family: concrete request/body semantics are
// classified separately below. Kiro may therefore omit the marker while still
// failing closed on an actual unsupported feature such as server-side tool
// search or deferred loading. Unknown beta tokens remain fail-closed.
var claudeCompatibilityDeclarationBetas = map[string]struct{}{
	"advanced-tool-use-2025-11-20":             {},
	"advisor-tool-2026-03-01":                  {},
	"afk-mode-2026-01-31":                      {},
	"claude-code-20250219":                     {},
	"context-management-2025-06-27":            {},
	"effort-2025-11-24":                        {},
	"fallback-credit-2026-06-01":               {},
	"fallback-credit-2026-06-09":               {},
	"fallback-credit-2026-07-01":               {},
	"interleaved-thinking-2025-05-14":          {},
	"mid-conversation-system-2026-04-07":       {},
	"mid-conversation-tool-changes-2026-07-01": {},
	"prompt-caching-scope-2026-01-05":          {},
	"structured-outputs-2025-11-13":            {},
	"structured-outputs-2025-12-15":            {},
	"thinking-token-count-2026-05-13":          {},
	"user-profiles-2026-03-24":                 {},
}

// Some Anthropic beta identifiers are provider-specific spellings for the same
// feature. Keep that translation explicit instead of forwarding a raw token to
// another provider.
var claudeBedrockBetaAliases = map[string]string{
	"fallback-credit-2026-06-01": "fallback-credit-2026-06-09",
	"fallback-credit-2026-07-01": "fallback-credit-2026-06-09",
}

func claudeBedrockBetaProjection(token string) (mapped string, supported bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", true
	}
	if mapped, ok := claudeBedrockBetaAliases[token]; ok {
		return mapped, true
	}
	if _, ok := claudeBedrockInvokeBetas[token]; ok {
		return token, true
	}
	if _, ok := claudeCompatibilityDeclarationBetas[token]; ok {
		// Declaration-only compatibility markers do not need to be copied to
		// Bedrock. The request analyzer separately validates the concrete
		// feature (thinking, context management, etc.) before Invoke is chosen.
		return "", true
	}
	return "", false
}

func claudeBetaAllowedTransports(token string) claudeTransportMask {
	allowed := claudeTransportMaskCustom
	if _, ok := claudeBedrockBetaProjection(token); ok {
		allowed |= claudeTransportMaskBedrockInvoke
	}
	if _, ok := claudeCompatibilityDeclarationBetas[token]; ok {
		allowed |= claudeTransportMaskKiro
	}
	return allowed
}

func claudeRawMeaningful(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func claudeRawNonEmptyArray(raw json.RawMessage) bool {
	if !claudeRawMeaningful(raw) {
		return false
	}
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) != nil || len(values) > 0
}

func claudeServiceTier(raw json.RawMessage) (string, *claudeTransportValidationError) {
	if !claudeRawMeaningful(raw) {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "service_tier must be one of auto or standard_only",
		}
	}
	value = strings.TrimSpace(value)
	if value != "auto" && value != "standard_only" {
		return "", &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "service_tier must be one of auto or standard_only",
		}
	}
	return value, nil
}

func claudeAssistantTransportRequirements(
	requirements *claudeTransportRequirements,
	messages []ClaudeMessage,
) {
	for _, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
			continue
		}
		for _, block := range claudeContentBlocks(message.Content) {
			blockType, _ := block["type"].(string)
			blockType = strings.ToLower(strings.TrimSpace(blockType))
			switch blockType {
			case "", "text", "tool_use":
				continue
			case "thinking":
				signature, _ := block["signature"].(string)
				if isKiroThinkingSignature(strings.TrimSpace(signature)) {
					requirements.Require(
						"kiro_synthetic_thinking",
						claudeTransportMaskKiro,
						"assistant history contains a Kiro-local thinking signature",
					)
					continue
				}
				requirements.Require(
					"provider_signed_thinking",
					claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
					"assistant history contains provider-issued thinking state",
				)
			case "redacted_thinking":
				requirements.Require(
					"provider_signed_thinking",
					claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
					"assistant history contains provider-issued redacted thinking state",
				)
			case "server_tool_use", "mcp_tool_use", "mcp_tool_result":
				requirements.Require(
					"claude_server_tool_state",
					claudeTransportMaskCustom,
					"assistant history contains Claude server/MCP tool state",
				)
			default:
				if strings.HasSuffix(blockType, "_tool_result") {
					requirements.Require(
						"claude_server_tool_state",
						claudeTransportMaskCustom,
						"assistant history contains Claude server-tool result state",
					)
					continue
				}
				requirements.Require(
					"transparent_unknown_block:"+blockType,
					claudeTransportMaskCustom,
					"assistant history contains an unmodeled Claude content block",
				)
			}
		}
	}
}

func claudeToolTransportRequirements(
	requirements *claudeTransportRequirements,
	tools []ClaudeTool,
) {
	if requirements == nil {
		return
	}
	// Anthropic/Claude Code's pure server-side web_search request is representable
	// by two terminal transports: direct Messages passthrough, or Kiro's MCP web
	// search adapter. Do not broaden the mixed-tool case: once ordinary client
	// tools coexist with server-side web_search, preserving the full server-tool
	// continuation state requires native Messages semantics.
	if len(tools) == 1 && isNativeWebSearchTool(tools[0]) {
		requirements.RequiresKiroWebSearchMCP = true
		requirements.Require(
			"native_web_search",
			claudeTransportMaskKiro|claudeTransportMaskCustom,
			"native web_search requires direct Messages or Kiro MCP semantics",
		)
		return
	}

	for _, tool := range tools {
		toolType := strings.ToLower(strings.TrimSpace(tool.Type))
		if toolType == "computer_20251124" || toolType == "computer_20250124" {
			requirements.UsesLegacyComputerTool = true
		}
		if toolType == "browser_toolset_20260801" {
			requirements.UsesBrowserToolset = true
			requirements.Require(
				"browser_toolset_20260801",
				claudeTransportMaskCustom,
				"browser member calls and browser_state require native Claude Messages pass-through",
			)
			continue
		}
		if toolType == "computer_toolset_20260801" {
			requirements.Require(
				"computer_toolset_20260801",
				claudeTransportMaskCustom,
				"computer toolset member calls and results require native Claude Messages pass-through",
			)
			continue
		}
		definition, knownDefinition := completionGuardLookupServerToolDefinition(toolType)
		switch {
		case toolType == "", toolType == "function", toolType == "custom":
			continue
		case toolType == "mcp_toolset":
			requirements.Require(
				"mcp_connector",
				claudeTransportMaskCustom,
				"request declares an MCP connector toolset",
			)
		case knownDefinition && definition.Feature == completionGuardToolFeatureToolSearch:
			// Anthropic's native API supports both regex and BM25 server Tool
			// Search. AWS currently documents only the regex variant for Bedrock
			// InvokeModel; Converse has no server Tool Search path.
			allowed := claudeTransportMaskCustom
			reason := "server tool search requires native Messages semantics"
			if definition.ExpectedName == "tool_search_tool_regex" {
				allowed |= claudeTransportMaskBedrockInvoke
				reason = "regex tool search requires native Messages or Bedrock Invoke semantics"
			}
			requirements.Require("tool_search", allowed, reason)
		case claudeToolCanEmitClientToolUse(tool):
			requirements.Require(
				"client_typed_tools",
				claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				"request declares an Anthropic-typed client tool",
			)
		default:
			requirements.Require(
				"server_tools",
				claudeTransportMaskCustom,
				"request declares a Claude server-side tool",
			)
		}
	}
}

func claudeCatalogToolTransportRequirements(
	requirements *claudeTransportRequirements,
	catalog *completionGuardNormalizedCatalog,
	streaming bool,
) {
	if requirements == nil || catalog == nil {
		return
	}
	requirements.RequiresToolSearch = catalog.HasToolSearch
	requirements.RequiresProgrammaticToolCalling = catalog.ProgrammaticRequested
	for _, entry := range catalog.Entries {
		if entry.Kind != completionGuardToolKindClient {
			continue
		}
		if entry.Deferred {
			requirements.Require(
				"deferred_tool:"+entry.Name,
				claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				"defer_loading semantics are not representable by the Kiro or Converse adapters",
			)
		}
		if entry.CodeCallable {
			requirements.Require(
				"programmatic_tool:"+entry.Name,
				claudeTransportMaskCustom,
				"programmatic tool calling is unavailable on Amazon Bedrock and compatibility adapters",
			)
		}

		knownExtras := map[string]struct{}{
			"allowed_callers":       {},
			"cache_control":         {},
			"defer_loading":         {},
			"eager_input_streaming": {},
			"input_examples":        {},
			"strict":                {},
		}
		if entry.Strict {
			requirements.RequiresStrictToolUse = true
			requirements.Require(
				"strict_tool:"+entry.Name,
				claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				"strict constrained-decoding semantics are not reproduced by Kiro or Converse",
			)
		}
		if streaming && entry.EagerInputStreaming {
			requirements.Require(
				"eager_input_streaming:"+entry.Name,
				claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				"fine-grained tool input streaming is not reproduced by Kiro or Converse",
			)
		}
		if raw, exists := entry.Declaration.Extra["cache_control"]; exists && claudeRawMeaningful(raw) {
			requirements.Require(
				"tool_cache_control:"+entry.Name,
				claudeTransportMaskKiro|claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				"Kiro intentionally omits tool-level provider cache directives",
			)
		}
		if entry.InputExamples {
			requirements.Require(
				"tool_input_examples:"+entry.Name,
				claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				"tool input examples affect model-visible tool semantics and are not preserved by Kiro or Converse",
			)
		}
		for key, raw := range entry.Declaration.Extra {
			if _, known := knownExtras[key]; known || !claudeRawMeaningful(raw) {
				continue
			}
			requirements.Require(
				"transparent_tool_field:"+entry.Name+":"+key,
				claudeTransportMaskCustom,
				"tool declaration contains an unmodeled field that must be forwarded unchanged",
			)
		}
	}
}

func analyzeClaudeTransportRequirementsWithInventoryAndCapabilities(
	req *ClaudeRequest,
	semantic claudeSemanticRequest,
	resolvedModel string,
	inventory claudeContentInventory,
	catalog *completionGuardNormalizedCatalog,
	modelCaps claudeModelCapabilities,
	kiroProjection claudeKiroProjection,
) (claudeTransportRequirements, *claudeTransportValidationError) {
	requirements := newClaudeTransportRequirementsWithKiroProjection(kiroProjection)
	requirements.ProtocolCapabilities = modelCaps
	if req == nil {
		return requirements, nil
	}
	requirements.Opus55ContractError = claudeOpus55RequestViolation(req)
	requirements.Sonnet55ContractError = claudeSonnet55RequestViolation(req)
	requirements.Haiku55ContractError = claudeHaiku55RequestViolation(req)
	if req.Thinking != nil {
		requirements.RequestedThinkingType = strings.ToLower(strings.TrimSpace(req.Thinking.Type))
		requirements.RequestedBudgetTokens = req.Thinking.BudgetTokens
	}
	if req.OutputConfig != nil {
		requirements.RequestedEffort = strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort))
	}
	if requirements.RequestedEffort == "" {
		requirements.RequestedEffort = strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	}

	if version := semantic.AnthropicVersion; version != "" && version != "2023-06-01" {
		requirements.Require(
			"anthropic_version:"+version,
			claudeTransportMaskCustom,
			"request uses a non-baseline Anthropic API version",
		)
	}
	for _, beta := range semantic.Betas.Values() {
		requirements.Require(
			"anthropic_beta:"+beta,
			claudeBetaAllowedTransports(beta),
			"beta declaration is native-forwarded; compatibility transports depend on the concrete feature projection",
		)
	}
	if semantic.UserProfileID != "" {
		if !semantic.Betas.Contains("user-profiles-2026-03-24") {
			return requirements, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: "anthropic-user-profile-id requires anthropic-beta user-profiles-2026-03-24",
			}
		}
		requirements.Require(
			"user_profile_id",
			claudeTransportMaskKiro|claudeTransportMaskCustom,
			"Kiro uses its own credential identity and intentionally omits the Anthropic provider profile ID",
		)
	}

	if validationErr := validateClaudeContentBetas(inventory, semantic); validationErr != nil {
		return requirements, validationErr
	}
	claudeContentTransportRequirements(&requirements, inventory)
	claudeAssistantTransportRequirements(&requirements, req.Messages)
	claudeToolTransportRequirements(&requirements, req.Tools)
	claudeCatalogToolTransportRequirements(&requirements, catalog, req.Stream)
	if requirements.RequiresToolSearch && modelCaps.ToolSearch != claudeFeatureSupported {
		// Bedrock Invoke support follows Anthropic's explicit per-model matrix.
		// A known-unsupported or future-unknown model remains eligible only for a
		// transparent direct Messages endpoint, which can validate its own deployment.
		requirements.Require(
			"tool_search_model_matrix",
			claudeTransportMaskCustom,
			"tool search is not documented for this model on Bedrock Invoke",
		)
	}

	if inventory.HasFinalClientToolUse {
		return requirements, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: "a final assistant tool_use must be followed by a user tool_result message",
		}
	}
	if inventory.HasAssistantPrefill {
		requirements.RequiresAssistantPrefill = true
		allowed := claudeTransportMaskCustom
		name := "assistant_prefill_upstream_validation"
		reason := "assistant prefill requires provider-specific native validation"
		if modelCaps.AssistantPrefill == claudeFeatureSupported {
			allowed |= claudeTransportMaskBedrockInvoke
			name = "assistant_prefill"
			reason = "legacy assistant prefill requires a transparent Messages transport"
		}
		requirements.Require(name, allowed, reason)
	}

	if req.Thinking != nil && strings.EqualFold(strings.TrimSpace(req.Thinking.Display), "updates") {
		requirements.Require(
			"thinking_display_updates",
			claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
			"thinking display updates require a native Messages transport with the beta header",
		)
	}
	if req.Thinking != nil {
		// Kiro and native Invoke have explicit thinking adapters. Converse does
		// not preserve the native thinking request/response state losslessly.
		requirements.Require(
			"thinking_configuration",
			claudeTransportMaskKiro|claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
			"request configures Claude thinking",
		)
		keys := make([]string, 0, len(req.Thinking.Extra))
		for key, raw := range req.Thinking.Extra {
			if claudeRawMeaningful(raw) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			requirements.Require(
				"transparent_thinking_field:"+key,
				claudeTransportMaskCustom,
				"thinking contains an unmodeled field that must be forwarded unchanged",
			)
		}
	}
	if req.OutputConfig != nil {
		if strings.TrimSpace(req.OutputConfig.Effort) != "" {
			requirements.Require(
				"output_config_effort",
				claudeTransportMaskKiro|claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
				"effort is not preserved by the Converse adapter",
			)
		}
		keys := make([]string, 0, len(req.OutputConfig.Extra))
		for key, raw := range req.OutputConfig.Extra {
			if claudeRawMeaningful(raw) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			allowed := claudeTransportMaskCustom
			reason := "output_config contains an unmodeled field that must be forwarded unchanged"
			if key == "format" {
				requirements.RequiresStructuredOutputFormat = true
				allowed |= claudeTransportMaskKiro | claudeTransportMaskBedrockInvoke
				reason = "structured output format requires exact transport projection; Kiro support is verified on the exact selected candidate route"
			}
			requirements.Require(
				"output_config_field:"+key,
				allowed,
				reason,
			)
		}
	}
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		requirements.Require(
			"compatibility_reasoning_effort",
			claudeTransportMaskKiro|claudeTransportMaskCustom,
			"reasoning_effort is a Kiro-Go compatibility field, not a native Bedrock Messages field",
		)
	}

	if inventory.HasMidConversationSystemText {
		requirements.RequiresMidConversationSystem = true
		allowed := claudeTransportMaskCustom
		if kiroProjection.AllowsKiro() {
			allowed |= claudeTransportMaskKiro
		}
		name := "mid_conversation_system_upstream_validation"
		reason := "mid-conversation system messages require provider-specific native validation"
		if modelCaps.MidConversationSystem == claudeFeatureSupported {
			allowed |= claudeTransportMaskBedrockInvoke
			name = "mid_conversation_system"
			reason = "request contains model-supported mid-conversation system messages"
		}
		requirements.Require(name, allowed, reason)
	}
	if inventory.HasInlineToolDefinition || inventory.HasMCPToolListing || inventory.HasEffortOnlySystem {
		requirements.Require("native_inline_tools_and_message_metadata", claudeTransportMaskCustom,
			"inline tool definitions and message-scoped native metadata require transparent Claude Messages transport")
	}
	if inventory.HasMidConversationToolChange {
		requirements.RequiresMidConversationToolChange = true
		allowed := claudeTransportMaskCustom
		name := "mid_conversation_tool_changes_upstream_validation"
		reason := "mid-conversation tool changes require provider-specific native validation"
		if modelCaps.MidConversationTools == claudeFeatureSupported && !semantic.Betas.Contains("inline-tools-2026-09-15") {
			allowed |= claudeTransportMaskBedrockInvoke
			name = "mid_conversation_tool_changes"
			reason = "request contains model-supported tool additions or removals"
		}
		requirements.Require(name, allowed, reason)
	}

	knownExtra := map[string]struct{}{
		"betas": {}, "cache_control": {}, "mcp_servers": {}, "metadata": {}, "service_tier": {},
		"stop_sequences": {}, "context_management": {}, "fallback_credit_token": {},
	}
	if raw, ok := req.Extra["cache_control"]; ok && claudeRawMeaningful(raw) {
		requirements.Require(
			"automatic_prompt_caching",
			claudeTransportMaskKiro|claudeTransportMaskCustom,
			"Kiro intentionally omits provider prompt-cache directives and reports an emulated projection",
		)
	}
	if raw, ok := req.Extra["mcp_servers"]; ok && claudeRawNonEmptyArray(raw) {
		requirements.Require(
			"mcp_connector",
			claudeTransportMaskCustom,
			"request contains MCP connector server state",
		)
	}
	if raw, ok := req.Extra["metadata"]; ok && claudeRawMeaningful(raw) {
		requirements.Require(
			"metadata_user_id",
			claudeTransportMaskKiro|claudeTransportMaskCustom,
			"Kiro intentionally omits provider metadata while preserving inference semantics",
		)
	}
	if raw, ok := req.Extra["service_tier"]; ok {
		value, validationErr := claudeServiceTier(raw)
		if validationErr != nil {
			return requirements, validationErr
		}
		if value != "" {
			requirements.Require(
				"service_tier:"+value,
				claudeTransportMaskKiro|claudeTransportMaskCustom,
				"Kiro intentionally omits provider QoS selection while preserving inference semantics",
			)
		}
	}
	if raw, ok := req.Extra["stop_sequences"]; ok && claudeRawMeaningful(raw) {
		sequences, err := parseClaudeStopSequences(raw)
		if err != nil {
			return requirements, &claudeTransportValidationError{
				Code:    "invalid_stop_sequences",
				Message: err.Error(),
			}
		}
		if len(sequences) > 0 {
			requirements.RequiresStopSequences = true
			requirements.StopSequences = append([]string(nil), sequences...)
			requirements.Require(
				"stop_sequences",
				claudeTransportMaskKiro|claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke|claudeTransportMaskBedrockConverse,
				"Kiro emulates custom stop sequences at the response boundary; native transports preserve them upstream",
			)
			requirements.KiroProjection.add(
				"stop_sequences",
				claudeProjectionEmulated,
				"custom stop sequences are enforced on emitted Kiro text, including matches split across stream chunks",
			)
		}
	}
	if raw, ok := req.Extra["context_management"]; ok && claudeRawMeaningful(raw) {
		allowed := claudeTransportMaskCustom | claudeTransportMaskBedrockInvoke
		if kiroProjection.AllowsKiro() && kiroProjection.ContextManagement != nil {
			allowed |= claudeTransportMaskKiro
		}
		requirements.Require(
			"context_management",
			allowed,
			"known Kiro context edits are emulated locally; unsupported edit shapes remain native-only",
		)
	}
	if raw, ok := req.Extra["fallback_credit_token"]; ok && claudeRawMeaningful(raw) {
		requirements.Require(
			"fallback_credit_token",
			claudeTransportMaskCustom|claudeTransportMaskBedrockInvoke,
			"fallback credit token is provider-signed state and requires native passthrough",
		)
	}

	keys := make([]string, 0, len(req.Extra))
	for key, raw := range req.Extra {
		if _, known := knownExtra[key]; known || !claudeRawMeaningful(raw) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		requirements.Require(
			"transparent_unknown_field:"+key,
			claudeTransportMaskCustom,
			"request contains an unmodeled Claude Messages field",
		)
	}

	if !kiroProjection.AllowsKiro() {
		requirements.Require(
			"kiro_projection",
			claudeTransportMaskAll&^claudeTransportMaskKiro,
			kiroProjection.Summary(),
		)
	}

	if requirements.Allowed == 0 {
		return requirements, &claudeTransportValidationError{
			Code:       "invalid_request_error",
			DetailCode: "unsupported_transport_feature",
			Message: "Claude request features require mutually incompatible transports: " +
				requirements.Summary(),
		}
	}
	return requirements, nil
}

func analyzeClaudeTransportRequirementsWithInventory(
	req *ClaudeRequest,
	semantic claudeSemanticRequest,
	resolvedModel string,
	inventory claudeContentInventory,
	catalog *completionGuardNormalizedCatalog,
) (claudeTransportRequirements, *claudeTransportValidationError) {
	return analyzeClaudeTransportRequirementsWithInventoryAndCapabilities(
		req,
		semantic,
		resolvedModel,
		inventory,
		catalog,
		resolveClaudeModelCapabilities(resolvedModel),
		buildClaudeKiroProjection(req, inventory, semantic),
	)
}

func analyzeClaudeTransportRequirements(
	req *ClaudeRequest,
	semantic claudeSemanticRequest,
	resolvedModel string,
) (claudeTransportRequirements, *claudeTransportValidationError) {
	var catalog *completionGuardNormalizedCatalog
	if req != nil {
		var reason string
		catalog, reason = completionGuardBuildNormalizedCatalogWithOptions(
			req,
			completionGuardCatalogBuildOptions{
				AllowAllDeferred: claudeRequestHasMidConversationToolChangesRaw(
					req.Messages,
				),
			},
		)
		if reason != "" {
			return newClaudeTransportRequirements(), &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: reason,
			}
		}
	}
	inventory, validationErr := buildClaudeContentInventory(req, catalog)
	if validationErr != nil {
		return newClaudeTransportRequirements(), validationErr
	}
	return analyzeClaudeTransportRequirementsWithInventory(
		req,
		semantic,
		resolvedModel,
		inventory,
		catalog,
	)
}

func claudeAccountIsDirectAnthropic(account *config.Account) bool {
	return account != nil && account.IsDirectAnthropic()
}

func claudeTransportKindForAccount(account *config.Account) claudeTransportKind {
	if account == nil {
		return claudeTransportUnsupported
	}
	if account.UsesKiroRuntime() {
		return claudeTransportKiroNative
	}
	switch account.Kind() {
	case config.AccountKindCustomRelay:
		return claudeTransportCustomRelay
	case config.AccountKindDirectAnthropic:
		return claudeTransportDirectAnthropic
	case config.AccountKindBedrock:
		if account.BedrockUseConverse {
			return claudeTransportBedrockConverse
		}
		return claudeTransportBedrockInvoke
	default:
		return claudeTransportUnsupported
	}
}

func claudeAccountCompatibleWithRequirements(
	account *config.Account,
	forwarded bool,
	requirements claudeTransportRequirements,
) bool {
	if account != nil && !account.HasKnownKind() {
		return false
	}
	kind := claudeTransportKindForAccount(account)
	if forwarded && kind == claudeTransportCustomRelay {
		return false
	}
	if kind == claudeTransportKiroNative && !requirements.KiroProjection.AllowsKiro() {
		return false
	}
	return requirements.Allows(kind)
}

type claudeRoutingContext struct {
	Semantic          claudeSemanticRequest
	Requirements      claudeTransportRequirements
	RequestedModel    string
	InvocationBody    []byte
	InvocationHeaders http.Header
}

type claudeRoutingContextKey struct{}

func withClaudeRoutingContext(
	ctx context.Context,
	routing claudeRoutingContext,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, claudeRoutingContextKey{}, routing)
}

func claudeRoutingContextFromContext(ctx context.Context) (claudeRoutingContext, bool) {
	if ctx == nil {
		return claudeRoutingContext{}, false
	}
	routing, ok := ctx.Value(claudeRoutingContextKey{}).(claudeRoutingContext)
	return routing, ok
}

func claudeRoutingSemanticHeaderValues(header http.Header, key string) []string {
	if len(header) == 0 {
		return nil
	}
	values := header.Values(key)
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			normalized = append(normalized, value)
		}
	}
	sort.Strings(normalized)
	return normalized
}

func claudeRoutingSemanticHeadersEqual(a, b http.Header) bool {
	for _, key := range []string{
		"Anthropic-Beta",
		"Anthropic-Version",
		"Anthropic-User-Profile-Id",
	} {
		left := claudeRoutingSemanticHeaderValues(a, key)
		right := claudeRoutingSemanticHeaderValues(b, key)
		if len(left) != len(right) {
			return false
		}
		for i := range left {
			if left[i] != right[i] {
				return false
			}
		}
	}
	return true
}

func claudeRoutingContextMatchesInvocation(
	routing claudeRoutingContext,
	rawBody []byte,
	semanticHeaders http.Header,
) bool {
	return bytes.Equal(rawBody, routing.InvocationBody) &&
		claudeRoutingSemanticHeadersEqual(semanticHeaders, routing.InvocationHeaders)
}

func resolveClaudeRoutingContext(
	ctx context.Context,
	rawBody []byte,
	semanticHeaders http.Header,
	model string,
) (claudeRoutingContext, error) {
	if routing, ok := claudeRoutingContextFromContext(ctx); ok &&
		claudeRoutingContextMatchesInvocation(routing, rawBody, semanticHeaders) {
		return routing, nil
	}

	// A Completion Guard/Audit continuation inherits cancellation, deadlines and
	// request-scoped auth values from the outer request context, but its body is a
	// different Claude request. Rebuild semantic routing whenever the invocation
	// body or semantic headers differ instead of reusing the outer turn's plan.
	// Direct unit tests and legacy internal callers without routing context use
	// the same rebuild path.
	thinkingConfig := config.GetThinkingConfig()
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		rawBody,
		semanticHeaders,
		thinkingConfig.Suffix,
		resolveClaudeProviderReasoningCapability,
	)
	if validationErr != nil {
		return claudeRoutingContext{}, validationErr
	}
	if strings.TrimSpace(model) != "" && strings.TrimSpace(plan.ResolvedModel) == "" {
		plan.ResolvedModel = model
	}
	return claudeRoutingContext{
		Semantic:          plan.Semantic,
		Requirements:      plan.Requirements,
		RequestedModel:    stripRequestedModelThinkingSuffix(plan.RequestedModel, thinkingConfig.Suffix),
		InvocationBody:    append([]byte(nil), rawBody...),
		InvocationHeaders: cloneAnthropicSemanticHeaders(semanticHeaders),
	}, nil
}

func claudeUnsupportedRequestedProtocolFeature(
	requirements claudeTransportRequirements,
) string {
	caps := requirements.ProtocolCapabilities
	checks := []struct {
		required bool
		support  claudeFeatureSupport
		name     string
	}{
		{requirements.RequiresAssistantPrefill, caps.AssistantPrefill, "assistant prefill"},
		{requirements.RequiresMidConversationSystem, caps.MidConversationSystem, "mid-conversation system messages"},
		{requirements.RequiresMidConversationToolChange, caps.MidConversationTools, "mid-conversation tool changes"},
		{requirements.RequiresProgrammaticToolCalling, caps.ProgrammaticToolCalling, "programmatic tool calling"},
		{requirements.RequiresToolSearch, caps.ToolSearch, "tool search"},
	}
	for _, check := range checks {
		if check.required && check.support == claudeFeatureUnsupported {
			return check.name
		}
	}
	return ""
}

func (h *Handler) claudeAccountSupportsRequestedProtocol(
	account *config.Account,
	model string,
	requirements claudeTransportRequirements,
) (bool, string) {
	if account == nil {
		return true, ""
	}
	// Validate against the *selected provider model*, not a public alias. A
	// custom-compatible deployment may intentionally define different rules.
	kind := claudeTransportKindForAccount(account)
	if requirements.Opus55ContractError != "" && isClaudeOpus55Model(model) {
		switch kind {
		case claudeTransportDirectAnthropic:
			if claudeProviderProfileForAccount(account) != claudeProviderAnthropicCompatible {
				return false, requirements.Opus55ContractError
			}
		case claudeTransportBedrockInvoke, claudeTransportBedrockConverse:
			return false, requirements.Opus55ContractError
		}
	}
	if requirements.Sonnet55ContractError != "" && isClaudeSonnet55Model(model) {
		switch kind {
		case claudeTransportDirectAnthropic:
			if claudeProviderProfileForAccount(account) != claudeProviderAnthropicCompatible {
				return false, requirements.Sonnet55ContractError
			}
		case claudeTransportBedrockInvoke, claudeTransportBedrockConverse:
			return false, requirements.Sonnet55ContractError
		}
	}
	if requirements.Haiku55ContractError != "" && isClaudeHaiku55Model(model) {
		switch kind {
		case claudeTransportDirectAnthropic:
			if claudeProviderProfileForAccount(account) != claudeProviderAnthropicCompatible {
				return false, requirements.Haiku55ContractError
			}
		case claudeTransportBedrockInvoke, claudeTransportBedrockConverse:
			return false, requirements.Haiku55ContractError
		}
	}
	if requirements.UsesBrowserToolset && kind == claudeTransportDirectAnthropic &&
		claudeProviderProfileForAccount(account) != claudeProviderAnthropicCompatible {
		// Unknown future models are validated by Anthropic; explicitly documented
		// older models that cannot run the browser toolset fail locally.
		family, version, known := canonicalClaudeModelFamilyVersion(model)
		if known && !claudeOfficialBrowserToolsetSupported(family, version) &&
			!claudeVersionAtLeast(version, 5, 0) {
			return false, "browser_toolset_20260801 is unavailable for this Claude model"
		}
	}
	if (isClaudeOpus55Model(model) || isClaudeSonnet55Model(model) || isClaudeHaiku55Model(model)) &&
		(kind == claudeTransportBedrockInvoke || kind == claudeTransportBedrockConverse) &&
		(requirements.RequiresStructuredOutputFormat || requirements.RequiresStrictToolUse) {
		return false, "Claude 5.5 on Amazon Bedrock does not support structured output format or strict tool use"
	}
	if requirements.UsesLegacyComputerTool && (isClaudeOpus55Model(model) || isClaudeSonnet55Model(model) || isClaudeHaiku55Model(model)) &&
		kind == claudeTransportDirectAnthropic &&
		claudeProviderProfileForAccount(account) != claudeProviderAnthropicCompatible {
		if isClaudeOpus55Model(model) {
			return false, "Claude Opus 5.5 on the Claude API requires computer_toolset_20260801 instead of legacy computer tools"
		}
		if isClaudeHaiku55Model(model) {
			return false, "Claude Haiku 5.5 on the Claude API requires computer_toolset_20260801 instead of legacy computer tools"
		}
		return false, "Claude Sonnet 5.5 on the Claude API requires the new computer toolset instead of legacy computer tools"
	}
	if !requirements.RequiresAssistantPrefill &&
		!requirements.RequiresMidConversationSystem &&
		!requirements.RequiresMidConversationToolChange &&
		!requirements.RequiresProgrammaticToolCalling &&
		!requirements.RequiresToolSearch {
		return true, ""
	}

	switch kind {
	case claudeTransportDirectAnthropic:
		profile := claudeProviderProfileForAccount(account)
		if profile == claudeProviderAnthropicCompatible {
			// A compatible endpoint owns its deployment semantics. Structural
			// validation remains local, while unsupported/unknown model features
			// are left for that transparent upstream to validate.
			return true, ""
		}
	case claudeTransportBedrockInvoke:
		// InvokeModel preserves native Anthropic Messages fields, but AWS model
		// availability follows the same documented exact feature matrices.
	default:
		// Kiro, relays and Converse are already constrained by transport masks
		// and adapter-specific requirements.
		return true, ""
	}

	if feature := claudeUnsupportedRequestedProtocolFeature(requirements); feature != "" {
		return false, fmt.Sprintf(
			"deployment=%s model=%s does not support %s",
			claudeProviderProfileForAccount(account).String(),
			strings.TrimSpace(model),
			feature,
		)
	}
	// Unknown future model capabilities are not hard-negative on a transparent
	// native transport; the provider can validate them without lossy adaptation.
	return true, ""
}

func (h *Handler) claudeAccountSupportsRequestedReasoning(
	account *config.Account,
	route resolvedProviderModel,
	requirements claudeTransportRequirements,
) (bool, string) {
	if account == nil {
		return true, ""
	}
	thinkingType := strings.ToLower(strings.TrimSpace(requirements.RequestedThinkingType))
	effort := strings.ToLower(strings.TrimSpace(requirements.RequestedEffort))
	if thinkingType == "" && effort == "" {
		return true, ""
	}

	kind := claudeTransportKindForAccount(account)
	switch kind {
	case claudeTransportKiroNative:
		// Kiro has compatibility mappings (for example legacy enabled ->
		// adaptive) that are not equivalent to native type equality. The exact
		// selected route is validated by buildClaudeKiroCandidateAdditionalFields
		// after selection, using this route's own schema-derived capability.
		return true, ""
	case claudeTransportDirectAnthropic, claudeTransportBedrockInvoke:
		// Transparent/native paths carry the requested reasoning controls as-is,
		// so the exact selected route must prove the requested native shape.
	default:
		// Converse cannot reach this path when thinking is required.
		return true, ""
	}
	capability := route.Reasoning
	known := route.ReasoningKnown
	deployment := claudeDeploymentKeyForAccount(account, string(route.ProviderID))
	if !known {
		// A transparent compatible endpoint may define a future model contract.
		// Official Anthropic and Bedrock candidates must not borrow capability
		// from another account, registry generation, or folded ID.
		if deployment.Profile == claudeProviderAnthropicCompatible {
			return true, ""
		}
		return false, fmt.Sprintf(
			"deployment=%s:%s has no exact reasoning metadata for provider_model=%s",
			deployment.Profile.String(),
			deployment.CanonicalOrigin,
			route.ProviderID,
		)
	}
	if thinkingType != "" && thinkingType != "none" {
		if _, ok := matchThinkingType(thinkingType, capability.ThinkingTypes); !ok {
			if deployment.Profile == claudeProviderAnthropicCompatible && thinkingType == "disabled" {
				return true, ""
			}
			return false, fmt.Sprintf(
				"deployment=%s:%s does not natively support thinking.type=%s",
				deployment.Profile.String(),
				deployment.CanonicalOrigin,
				thinkingType,
			)
		}
		if thinkingType == "enabled" && requirements.RequestedBudgetTokens > 0 &&
			!capability.SupportsBudgetTokens {
			return false, fmt.Sprintf(
				"deployment=%s:%s does not support manual thinking budgets",
				deployment.Profile.String(),
				deployment.CanonicalOrigin,
			)
		}
	}
	if effort != "" {
		if _, ok := matchSupportedEffort(effort, capability.Efforts); !ok {
			return false, fmt.Sprintf(
				"deployment=%s:%s does not support effort=%s",
				deployment.Profile.String(),
				deployment.CanonicalOrigin,
				effort,
			)
		}
	}
	return true, ""
}

func claudeKiroCandidateSupportsProjection(
	account *config.Account,
	route resolvedProviderModel,
	requirements claudeTransportRequirements,
) (bool, string) {
	if account == nil || claudeTransportKindForAccount(account) != claudeTransportKiroNative {
		return true, ""
	}
	if requirements.RequiresStructuredOutputFormat {
		if !route.KiroStructuredOutputKnown {
			return false, fmt.Sprintf(
				"kiro_structured_output_adapter: provider_model=%s has no candidate-local Kiro output_config.format capability evidence",
				route.ProviderID,
			)
		}
		if !route.KiroStructuredOutputFormat {
			return false, fmt.Sprintf(
				"kiro_structured_output_adapter: provider_model=%s does not advertise output_config.format for this Kiro account",
				route.ProviderID,
			)
		}
	}
	return true, ""
}

const (
	claudeDirectAnthropicMaxBodyBytes = 32 * 1024 * 1024
	// AWS' generic InvokeModel API accepts up to 25,000,000 bytes, but the
	// current Claude platform guidance documents a stricter 20 MB Bedrock
	// request limit. Use the lower effective limit for Claude candidates.
	claudeBedrockMaxBodyBytes = 20_000_000
)

func claudeTransportAcceptsBodySize(kind claudeTransportKind, bodyBytes int) (bool, string) {
	if bodyBytes <= 0 {
		return true, ""
	}
	var limit int
	switch kind {
	case claudeTransportDirectAnthropic:
		limit = claudeDirectAnthropicMaxBodyBytes
	case claudeTransportBedrockInvoke, claudeTransportBedrockConverse:
		limit = claudeBedrockMaxBodyBytes
	default:
		return true, ""
	}
	if bodyBytes <= limit {
		return true, ""
	}
	return false, fmt.Sprintf("request-body-bytes=%d exceeds transport limit=%d", bodyBytes, limit)
}

type claudeCandidatePlan struct {
	Selection       *pool.AccountSelection
	Account         *config.Account
	ProviderModelID ProviderModelID
	RegistryVersion uint64
	Route           resolvedProviderModel
	Transport       claudeTransportKind

	// KiroAdditionalModelRequestFields is derived from this exact account/model
	// route, never from the public capability union. It is nil for non-Kiro
	// candidates and for compatibility call sites that do not supply a body.
	KiroAdditionalModelRequestFields map[string]interface{}
}

func claudeSelectionExhaustionError(
	requirements claudeTransportRequirements,
	skipped []string,
	compatibilitySkipped bool,
	availabilitySkipped bool,
) error {
	summary := strings.Join(skipped, ",")
	switch {
	case availabilitySkipped:
		return fmt.Errorf(
			"%w: requirements=%s; skipped=%s",
			errClaudeTransportUnavailable,
			requirements.Summary(),
			summary,
		)
	case compatibilitySkipped:
		return fmt.Errorf(
			"%w: requirements=%s; skipped=%s",
			errClaudeTransportIncompatible,
			requirements.Summary(),
			summary,
		)
	default:
		return nil
	}
}

func (h *Handler) nextCompatibleClaudeCandidate(
	requestedModel string,
	model string,
	excluded map[string]bool,
	apiKeyID string,
	forwarded bool,
	requirements claudeTransportRequirements,
	semanticBody []byte,
) (*claudeCandidatePlan, error) {
	if excluded == nil {
		excluded = make(map[string]bool)
	}
	lookupModel := strings.TrimSpace(requestedModel)
	if lookupModel == "" {
		lookupModel = model
	}
	// Every target/capability decision below is resolved from this one immutable
	// pointer. A concurrent publication can affect the next selection, never half
	// of the current candidate plan.
	registry := h.currentProviderRegistry()
	var skipped []string
	compatibilitySkipped := false
	availabilitySkipped := false
	for {
		// Pool owns ordering, quota, health and affinity only. The immutable
		// provider registry below is the sole model-identity authority.
		selection := h.pool.GetNextSelectionForModelWithApiKey("", excluded, apiKeyID)
		if selection == nil {
			// No account inspected at all is ordinary availability exhaustion.
			// Only inspected candidates can prove semantic incompatibility.
			return nil, claudeSelectionExhaustionError(
				requirements, skipped, compatibilitySkipped, availabilitySkipped,
			)
		}
		account := &selection.Account
		kind := claudeTransportKindForAccount(account)
		accountID := account.ID
		if strings.TrimSpace(accountID) == "" {
			return nil, fmt.Errorf("%w: selected account without an ID", errClaudeTransportUnavailable)
		}
		if !claudeAccountCompatibleWithRequirements(account, forwarded, requirements) {
			compatibilitySkipped = true
			excluded[accountID] = true
			skipped = append(skipped, accountID+":"+kind.String())
			continue
		}
		route, version, resolved := resolveCandidateProviderRoute(registry, account, lookupModel, model)
		if !resolved {
			compatibilitySkipped = true
			excluded[accountID] = true
			skipped = append(skipped, accountID+":"+kind.String()+":model-not-in-registry")
			continue
		}
		if supported, reason := claudeKiroCandidateSupportsProjection(account, route, requirements); !supported {
			compatibilitySkipped = true
			excluded[accountID] = true
			skipped = append(skipped, accountID+":"+kind.String()+":"+reason)
			logger.Warnf("[ClaudeTransport] skipping Kiro projection-incompatible account=%s model=%s transport=%s reason=%s", accountID, route.ProviderID, kind.String(), reason)
			continue
		}
		if accepted, reason := claudeTransportAcceptsBodySize(kind, requirements.RequestBodyBytes); !accepted {
			compatibilitySkipped = true
			excluded[accountID] = true
			skipped = append(skipped, accountID+":"+kind.String()+":"+reason)
			continue
		}
		if blocked, reason := providerCandidateUnavailable(registry, account, lookupModel, route, time.Now()); blocked {
			availabilitySkipped = true
			excluded[accountID] = true
			skipped = append(skipped, accountID+":"+kind.String()+":"+reason)
			logger.Warnf("[ClaudeTransport] skipping provider account=%s model=%s transport=%s registry=%d reason=%s", accountID, route.ProviderID, kind.String(), version, reason)
			continue
		}
		if supported, reason := h.claudeAccountSupportsRequestedProtocol(account, string(route.ProviderID), requirements); !supported {
			compatibilitySkipped = true
			excluded[accountID] = true
			skipped = append(skipped, accountID+":"+kind.String()+":"+reason)
			continue
		}
		if supported, reason := h.claudeAccountSupportsRequestedReasoning(account, route, requirements); !supported {
			compatibilitySkipped = true
			excluded[accountID] = true
			skipped = append(skipped, accountID+":"+kind.String()+":"+reason)
			logger.Warnf("[ClaudeTransport] skipping reasoning-incompatible account=%s model=%s transport=%s reason=%s", accountID, route.ProviderID, kind.String(), reason)
			continue
		}

		var kiroAdditionalFields map[string]interface{}
		if kind == claudeTransportKiroNative && len(semanticBody) > 0 && !requirements.RequiresKiroWebSearchMCP {
			// Pure native web_search never calls generateAssistantResponse: the
			// selected Kiro credential executes the server tool through MCP instead.
			// Do not reject that candidate because its model-request schema cannot
			// represent the Anthropic server-tool declaration that will not be sent.
			fields, buildErr := h.buildClaudeKiroCandidateAdditionalFields(semanticBody, route)
			if buildErr != nil {
				compatibilitySkipped = true
				excluded[accountID] = true
				reason := buildErr.Error()
				skipped = append(skipped, accountID+":"+kind.String()+":"+reason)
				logger.Warnf("[ClaudeTransport] skipping Kiro adapter-incompatible account=%s model=%s transport=%s registry=%d reason=%s", accountID, route.ProviderID, kind.String(), version, reason)
				continue
			}
			kiroAdditionalFields = fields
		}

		return &claudeCandidatePlan{
			Selection:                        selection,
			Account:                          account,
			ProviderModelID:                  route.ProviderID,
			RegistryVersion:                  version,
			Route:                            route,
			Transport:                        kind,
			KiroAdditionalModelRequestFields: kiroAdditionalFields,
		}, nil
	}
}

func (h *Handler) nextCompatibleClaudeAccount(
	model string,
	excluded map[string]bool,
	apiKeyID string,
	forwarded bool,
	requirements claudeTransportRequirements,
) (*config.Account, error) {
	candidate, err := h.nextCompatibleClaudeCandidate(model, model, excluded, apiKeyID, forwarded, requirements, nil)
	if candidate == nil || err != nil {
		return nil, err
	}
	account := candidate.Account.Clone()
	return &account, nil
}
