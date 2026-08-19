package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

type responsesTransportKind uint8

const (
	responsesTransportUnsupported responsesTransportKind = iota
	responsesTransportKiro
	responsesTransportCustomRelay
)

type responsesToolChoiceKind uint8

const (
	responsesToolChoiceAuto responsesToolChoiceKind = iota
	responsesToolChoiceNone
	responsesToolChoiceRequired
	responsesToolChoiceSpecific
	responsesToolChoicePassthroughOnly
)

type responsesToolChoicePlan struct {
	Kind      responsesToolChoiceKind
	ToolName  string
	Namespace string
	ToolType  string
}

func parseResponsesToolChoice(raw json.RawMessage, tools []OpenAITool) (responsesToolChoicePlan, error) {
	plan := responsesToolChoicePlan{Kind: responsesToolChoiceAuto}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return plan, nil
	}

	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return plan, fmt.Errorf("invalid tool_choice: %w", err)
	}

	resolveTool := func(namespace, name, kind string) (responsesToolIdentity, error) {
		matches := make([]responsesToolIdentity, 0, 2)
		for _, tool := range tools {
			if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) != name {
				continue
			}
			identity := responsesToolIdentity{
				Namespace: strings.TrimSpace(tool.ResponsesNamespace),
				Name:      strings.TrimSpace(tool.Function.Name),
				Kind:      strings.TrimSpace(tool.ResponsesKind),
			}
			if identity.Kind == "" {
				identity.Kind = responsesToolKindFunction
			}
			if namespace != "" && identity.Namespace != namespace {
				continue
			}
			if kind != "" && kind != "tool" && identity.Kind != kind {
				continue
			}
			matches = append(matches, identity)
		}
		if len(matches) == 0 {
			return responsesToolIdentity{}, fmt.Errorf("tool_choice references undeclared tool %q", name)
		}
		if namespace == "" && len(matches) > 1 {
			return responsesToolIdentity{}, fmt.Errorf("tool_choice name %q is ambiguous across namespaces; specify namespace", name)
		}
		return matches[0], nil
	}

	switch choice := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "", "auto":
			return plan, nil
		case "none":
			plan.Kind = responsesToolChoiceNone
			return plan, nil
		case "required", "any":
			// A continuation may inherit its catalog from previous_response_id.
			// Defer the "at least one tool" check until the Kiro request has
			// loaded that persisted catalog.
			plan.Kind = responsesToolChoiceRequired
			return plan, nil
		default:
			// A linked OpenAI-compatible upstream may understand future tool-choice
			// strings. Keep the raw Responses request eligible for transparent relay,
			// but never silently reinterpret it on the Kiro transport.
			plan.Kind = responsesToolChoicePassthroughOnly
			return plan, nil
		}
	case map[string]interface{}:
		kind, _ := choice["type"].(string)
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "", "auto":
			return plan, nil
		case "none":
			plan.Kind = responsesToolChoiceNone
			return plan, nil
		case "required", "any":
			plan.Kind = responsesToolChoiceRequired
			return plan, nil
		case "function", "custom", "tool":
			name, _ := choice["name"].(string)
			namespace, _ := choice["namespace"].(string)
			if name == "" {
				if fn, ok := choice["function"].(map[string]interface{}); ok {
					name, _ = fn["name"].(string)
					if namespace == "" {
						namespace, _ = fn["namespace"].(string)
					}
				}
			}
			name = strings.TrimSpace(name)
			namespace = strings.TrimSpace(namespace)
			if name == "" {
				return plan, errors.New("specific tool_choice requires a tool name")
			}
			requestedKind := strings.ToLower(strings.TrimSpace(kind))
			identity := responsesToolIdentity{Namespace: namespace, Name: name, Kind: requestedKind}
			if len(tools) != 0 {
				var err error
				identity, err = resolveTool(namespace, name, requestedKind)
				if err != nil {
					return plan, err
				}
			}
			plan.Kind = responsesToolChoiceSpecific
			plan.ToolName = identity.Name
			plan.Namespace = identity.Namespace
			plan.ToolType = identity.Kind
			return plan, nil
		default:
			plan.Kind = responsesToolChoicePassthroughOnly
			return plan, nil
		}
	default:
		plan.Kind = responsesToolChoicePassthroughOnly
		return plan, nil
	}
}

func responsesParallelToolCalls(req *ResponsesRequest) bool {
	if req != nil && req.ParallelToolCalls != nil {
		return *req.ParallelToolCalls
	}
	return true
}

func responsesRequiresSerialToolGuard(req *ResponsesRequest, plan responsesToolChoicePlan) bool {
	return req != nil && req.ParallelToolCalls != nil && !*req.ParallelToolCalls && len(req.Tools) > 0 && plan.Kind != responsesToolChoiceNone
}

func responsesToolChoiceForOutput(req *ResponsesRequest) json.RawMessage {
	if req == nil || len(strings.TrimSpace(string(req.ToolChoice))) == 0 || strings.TrimSpace(string(req.ToolChoice)) == "null" {
		return json.RawMessage(`"auto"`)
	}
	return append(json.RawMessage(nil), req.ToolChoice...)
}

func responsesToolsForOutput(req *ResponsesRequest) json.RawMessage {
	if req == nil || len(strings.TrimSpace(string(req.RawTools))) == 0 || strings.TrimSpace(string(req.RawTools)) == "null" {
		return json.RawMessage(`[]`)
	}
	return append(json.RawMessage(nil), req.RawTools...)
}

func responsesKiroCompatibility(req *ResponsesRequest, toolPlan responsesToolChoicePlan, requirements ...responsesSemanticRequirements) (bool, string) {
	if len(requirements) > 0 && !requirements[0].KiroSupported() {
		return false, requirements[0].Reason()
	}
	// Kiro has no native serial-tool switch. For parallel_tool_calls=false the
	// local Responses adapter buffers the complete candidate and commits it only
	// when the candidate contains at most one tool call. This is protocol
	// emulation, not a claim that the upstream natively supports serial tools.
	switch toolPlan.Kind {
	case responsesToolChoiceAuto, responsesToolChoiceNone:
		return true, ""
	case responsesToolChoiceRequired, responsesToolChoiceSpecific:
		if nativeToolChoiceModeFromEnv() != "always" {
			return false, "required/specific tool_choice needs KIRO_NATIVE_TOOL_CHOICE=always for the Kiro Responses adapter"
		}
		return true, ""
	case responsesToolChoicePassthroughOnly:
		return false, "tool_choice shape requires transparent OpenAI-compatible passthrough"
	default:
		return false, "unsupported tool_choice"
	}
}

func responsesTransportForAccount(account *config.Account, req *ResponsesRequest, toolPlan responsesToolChoicePlan, requirements ...responsesSemanticRequirements) (responsesTransportKind, string) {
	return responsesTransportForAccountWithOwnership(account, req, toolPlan, responseStateNoPrevious, requirements...)
}

func responsesTransportForAccountWithOwnership(account *config.Account, req *ResponsesRequest, toolPlan responsesToolChoicePlan, ownership responseStateOwnership, requirements ...responsesSemanticRequirements) (responsesTransportKind, string) {
	if account == nil {
		return responsesTransportUnsupported, "nil account"
	}
	if account.IsCustomApi() {
		if ownership == responseStateLocal {
			return responsesTransportUnsupported, "previous_response_id belongs to local Kiro state"
		}
		return responsesTransportCustomRelay, ""
	}
	switch account.Kind() {
	case config.AccountKindKiroOAuth, config.AccountKindKiroAPIKey:
		if ownership == responseStateExternal {
			return responsesTransportUnsupported, "previous_response_id belongs to an external Responses transport"
		}
		if ok, reason := responsesKiroCompatibility(req, toolPlan, requirements...); !ok {
			return responsesTransportUnsupported, reason
		}
		return responsesTransportKiro, ""
	case config.AccountKindDirectAnthropic:
		return responsesTransportUnsupported, "direct Anthropic has no native OpenAI Responses transport"
	case config.AccountKindBedrock:
		return responsesTransportUnsupported, "Bedrock has no native OpenAI Responses adapter in this phase"
	default:
		return responsesTransportUnsupported, "unknown account kind"
	}
}

func applyResponsesToolChoiceToKiroPayload(payload *KiroPayload, req *ResponsesRequest, plan responsesToolChoicePlan) {
	if payload == nil || req == nil {
		return
	}
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	switch plan.Kind {
	case responsesToolChoiceNone:
		if ctx == nil {
			return
		}
		ctx.Tools = nil
		ctx.ToolChoice = nil
		if len(ctx.ToolResults) == 0 {
			payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = nil
		}
	case responsesToolChoiceRequired:
		if ctx != nil {
			ctx.ToolChoice = map[string]interface{}{"any": map[string]interface{}{}}
		}
	case responsesToolChoiceSpecific:
		if ctx != nil {
			name := kiroToolNameForIdentity(payload, plan.Namespace, plan.ToolName)
			ctx.ToolChoice = map[string]interface{}{"tool": map[string]interface{}{"name": name}}
		}
	}
}

type responsesPrepareError struct {
	Status  int
	Message string
}

func (e *responsesPrepareError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type responsesKiroPrepared struct {
	Request              *ResponsesRequest
	Payload              *KiroPayload
	Projection           openAIProjection
	ResponseID           string
	Thinking             bool
	EstimatedInputTokens int
	StoredInput          json.RawMessage
	StoreResponse        bool
}

type responsesKiroPrepareFunc func() (*responsesKiroPrepared, error)

func (h *Handler) prepareResponsesKiroRequest(
	rawBody []byte,
	toolChoicePlan responsesToolChoicePlan,
	actualModel string,
	suffixThinking bool,
) (*responsesKiroPrepared, error) {
	var req ResponsesRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: "Invalid Responses request: " + err.Error()}
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}
	storedInputCopy := append(json.RawMessage(nil), req.Input...)
	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	var historyMessages []OpenAIMessage
	var historyProjection openAIProjection
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponse(req.PreviousResponseID)
		if loadErr != nil {
			return nil, &responsesPrepareError{
				Status:  http.StatusNotFound,
				Message: fmt.Sprintf("previous_response_id not found: %v", loadErr),
			}
		}
		var expandErr error
		historyMessages, historyProjection, expandErr = expandPreviousResponseHistoryWithProjection(prev)
		if expandErr != nil {
			return nil, &responsesPrepareError{
				Status:  http.StatusConflict,
				Message: "previous_response_state_unavailable: " + expandErr.Error(),
			}
		}
		if inheritErr := responsesInheritStoredTools(&req, prev); inheritErr != nil {
			return nil, &responsesPrepareError{
				Status:  http.StatusConflict,
				Message: "previous_response_state_unavailable: " + inheritErr.Error(),
			}
		}
	}

	effectiveToolChoicePlan, choiceErr := parseResponsesToolChoice(req.ToolChoice, req.Tools)
	if choiceErr != nil {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: choiceErr.Error()}
	}
	if effectiveToolChoicePlan.Kind == responsesToolChoiceRequired && len(req.Tools) == 0 {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: "tool_choice required needs at least one tool"}
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: err.Error()}
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	// Responses instructions are privileged instructions for this request, not a
	// historical message appended after previous_response_id expansion. The Kiro
	// adapter can only emulate that privilege through its initial priming pair,
	// so keep the instruction first in the adapter-facing message sequence.
	if strings.TrimSpace(req.Instructions) != "" {
		finalMessages = append(finalMessages, OpenAIMessage{Role: "system", Content: req.Instructions})
	}
	finalMessages = append(finalMessages, historyMessages...)
	finalMessages = append(finalMessages, inputMessages...)
	if len(finalMessages) == 0 {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: "input must contain at least one message"}
	}
	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: "input must contain at least one user message"}
	}

	openaiReq := &OpenAIRequest{
		Model:        actualModel,
		Messages:     finalMessages,
		Stream:       req.Stream,
		Tools:        req.Tools,
		Thinking:     req.Thinking,
		OutputConfig: req.OutputConfig,
	}
	if req.Temperature != nil {
		openaiReq.Temperature = *req.Temperature
		openaiReq.markTemperaturePresent()
	}
	if req.TopP != nil {
		openaiReq.TopP = *req.TopP
		openaiReq.markTopPPresent()
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}
	if req.Reasoning != nil {
		openaiReq.Reasoning = &OpenAIReasoningConfig{Effort: req.Reasoning.Effort}
	}

	capability := h.reasoningCapabilityForModel(actualModel)
	var capabilityProjection openAIProjection
	analyzeOpenAIKiroReasoningCapabilityProjection(&capabilityProjection, openaiReq, capability)
	if !capabilityProjection.Supported() {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: "Kiro Responses projection unsupported: " + capabilityProjection.Reason()}
	}
	additionalFields, nativeRequested, buildErr := BuildOpenAIAdditionalModelRequestFields(openaiReq, capability)
	if buildErr != nil {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: buildErr.Error()}
	}
	thinking := suffixThinking || nativeRequested
	useLegacyThinkingPrompt := thinking && len(additionalFields) == 0
	estimatedInputTokens := estimateOpenAIRequestInputTokens(openaiReq)

	canonicalRaw, marshalErr := json.Marshal(openaiReq)
	if marshalErr != nil {
		return nil, &responsesPrepareError{Status: http.StatusInternalServerError, Message: "encode Kiro Responses projection: " + marshalErr.Error()}
	}
	projection := analyzeOpenAIChatKiroProjection(canonicalRaw)
	projection.merge(historyProjection)
	projection.merge(capabilityProjection)
	if !projection.Supported() {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: "Kiro Responses projection unsupported: " + projection.Reason()}
	}
	if useLegacyThinkingPrompt {
		projection.emulate("reasoning", "legacy_thinking_prompt_emulation", "Kiro emulates requested reasoning through an injected priming prompt")
	}

	kiroPayload, runtimeProjection := projectOpenAIToKiro(openaiReq, useLegacyThinkingPrompt)
	projection.merge(runtimeProjection)
	if !projection.Supported() {
		return nil, &responsesPrepareError{Status: http.StatusBadRequest, Message: "Kiro Responses projection unsupported: " + projection.Reason()}
	}
	kiroPayload.AdditionalModelRequestFields = additionalFields
	applyResponsesToolChoiceToKiroPayload(kiroPayload, &req, effectiveToolChoicePlan)
	if !payloadFits(kiroPayload, maxPayloadTokens(actualModel)) {
		return nil, &responsesPrepareError{
			Status:  http.StatusBadRequest,
			Message: "unsupported_transport_semantics: Kiro projection would require history truncation",
		}
	}
	responseID, idErr := generateResponseIDForStore(storeResponse)
	if idErr != nil {
		return nil, &responsesPrepareError{Status: http.StatusInternalServerError, Message: "response_state_unavailable: " + idErr.Error()}
	}

	return &responsesKiroPrepared{
		Request:              &req,
		Payload:              kiroPayload,
		Projection:           projection,
		ResponseID:           responseID,
		Thinking:             thinking,
		EstimatedInputTokens: estimatedInputTokens,
		StoredInput:          storedInputCopy,
		StoreResponse:        storeResponse,
	}, nil
}

func parseResponsesRoutingEnvelope(raw []byte) (*ResponsesRequest, error) {
	// Routing must stay deliberately permissive. A linked OpenAI-compatible
	// account may understand future Responses fields/input items/tool schemas that
	// the local Kiro projector does not. Decode only the fields needed to choose a
	// transport here; the Kiro path performs the strict protocol decode lazily in
	// prepareResponsesKiroRequest after a Kiro account has actually been selected.
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("request body must be a JSON object")
	}

	req := &ResponsesRequest{}
	if modelRaw, ok := root["model"]; ok && !jsonRawIsNull(modelRaw) {
		if err := json.Unmarshal(modelRaw, &req.Model); err != nil {
			return nil, errors.New("model must be a string")
		}
	}
	if streamRaw, ok := root["stream"]; ok && !jsonRawIsNull(streamRaw) {
		if err := json.Unmarshal(streamRaw, &req.Stream); err != nil {
			return nil, errors.New("stream must be a boolean")
		}
	}
	if parallelRaw, ok := root["parallel_tool_calls"]; ok && !jsonRawIsNull(parallelRaw) {
		var parallel bool
		if err := json.Unmarshal(parallelRaw, &parallel); err == nil {
			req.ParallelToolCalls = &parallel
		}
	}
	if previousRaw, ok := root["previous_response_id"]; ok && !jsonRawIsNull(previousRaw) {
		_ = json.Unmarshal(previousRaw, &req.PreviousResponseID)
	}
	if toolsRaw, ok := root["tools"]; ok {
		req.RawTools = append(json.RawMessage(nil), toolsRaw...)
		req.Tools = append(req.Tools, responsesRoutingTools(toolsRaw, "")...)
	}
	if inputRaw, ok := root["input"]; ok && !jsonRawIsNull(inputRaw) {
		req.Tools = append(req.Tools, responsesRoutingAdditionalTools(inputRaw)...)
	}
	req.Tools = dedupeResponsesRoutingTools(req.Tools)
	if choiceRaw, ok := root["tool_choice"]; ok {
		req.ToolChoice = append(json.RawMessage(nil), choiceRaw...)
	}
	return req, nil
}

func responsesRoutingAdditionalTools(raw json.RawMessage) []OpenAITool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || strings.HasPrefix(trimmed, `"`) {
		return nil
	}
	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
	case '{':
		items = []json.RawMessage{raw}
	default:
		return nil
	}

	tools := make([]OpenAITool, 0)
	for _, item := range items {
		var obj map[string]json.RawMessage
		if json.Unmarshal(item, &obj) != nil || obj == nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(obj["type"], &typ)
		if strings.ToLower(strings.TrimSpace(typ)) != "additional_tools" {
			continue
		}
		tools = append(tools, responsesRoutingTools(obj["tools"], "")...)
	}
	return tools
}

func responsesRoutingTools(raw json.RawMessage, namespace string) []OpenAITool {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	tools := make([]OpenAITool, 0, len(items))
	for _, item := range items {
		var obj map[string]json.RawMessage
		if json.Unmarshal(item, &obj) != nil || obj == nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(obj["type"], &typ)
		typ = strings.ToLower(strings.TrimSpace(typ))
		if typ == "namespace" {
			var childNamespace string
			_ = json.Unmarshal(obj["name"], &childNamespace)
			childNamespace = strings.TrimSpace(childNamespace)
			if childNamespace == "" {
				continue
			}
			if strings.TrimSpace(namespace) != "" {
				childNamespace = strings.TrimSpace(namespace) + "." + childNamespace
			}
			tools = append(tools, responsesRoutingTools(obj["tools"], childNamespace)...)
			continue
		}
		if typ != responsesToolKindFunction && typ != responsesToolKindCustom {
			continue
		}

		var tool OpenAITool
		tool.Type = "function"
		tool.ResponsesNamespace = strings.TrimSpace(namespace)
		tool.ResponsesKind = typ
		_ = json.Unmarshal(obj["name"], &tool.Function.Name)
		if typ == responsesToolKindFunction && strings.TrimSpace(tool.Function.Name) == "" {
			var nested map[string]json.RawMessage
			if json.Unmarshal(obj["function"], &nested) == nil {
				_ = json.Unmarshal(nested["name"], &tool.Function.Name)
			}
		}
		tool.Function.Name = strings.TrimSpace(tool.Function.Name)
		if tool.Function.Name == "" {
			continue
		}
		tools = append(tools, tool)
	}
	return tools
}

func dedupeResponsesRoutingTools(tools []OpenAITool) []OpenAITool {
	if len(tools) < 2 {
		return tools
	}
	seen := make(map[string]struct{}, len(tools))
	result := make([]OpenAITool, 0, len(tools))
	for _, tool := range tools {
		kind := strings.TrimSpace(tool.ResponsesKind)
		if kind == "" {
			kind = responsesToolKindFunction
		}
		key := strings.TrimSpace(tool.ResponsesNamespace) + "\x00" + strings.TrimSpace(tool.Function.Name) + "\x00" + kind
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, tool)
	}
	return result
}

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, ok := h.readLimitedJSONBody(w, r, requestBodyErrorOpenAI)
	if !ok {
		return
	}

	req, err := parseResponsesRoutingEnvelope(body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	// Routing classifies the tool_choice shape but deliberately does not resolve it
	// against the partially decoded catalog. A transparent Responses upstream may
	// understand hosted/future tool declarations that the Kiro routing envelope
	// intentionally ignores. Kiro re-resolves the choice against the strict unified
	// catalog inside prepareResponsesKiroRequest before projection.
	toolChoicePlan, err := parseResponsesToolChoice(req.ToolChoice, nil)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	semantics := analyzeResponsesSemantics(body)
	ownership, ownershipErr := resolveResponseStateOwnership(req.PreviousResponseID)
	switch ownership {
	case responseStateLocalMissing:
		h.sendOpenAIError(w, http.StatusConflict, "invalid_request_error", "previous_response_state_unavailable: local previous_response_id is no longer available")
		return
	case responseStateUnavailable:
		message := "previous_response_state_unavailable"
		if ownershipErr != nil {
			message += ": " + ownershipErr.Error()
		}
		h.sendOpenAIError(w, http.StatusConflict, "invalid_request_error", message)
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, suffixThinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	var prepared *responsesKiroPrepared
	var prepareErr error
	preparedOnce := false
	prepareKiro := func() (*responsesKiroPrepared, error) {
		if preparedOnce {
			return prepared, prepareErr
		}
		preparedOnce = true
		prepared, prepareErr = h.prepareResponsesKiroRequest(body, toolChoicePlan, actualModel, suffixThinking)
		return prepared, prepareErr
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	// Local response IDs are allocated lazily inside the Kiro projector. Requests
	// that do not need local ownership resolution or a new local ID can therefore
	// continue to use a transparent custom relay even if local state is unavailable.
	respID := ""
	forwarded := r.Header.Get(forwardHeader) != ""

	if req.Stream {
		h.handleResponsesStream(w, actualModel, apiKeyID, respID, req, toolChoicePlan, ownership,
			semantics, prepareKiro, body, forwarded, r.Context())
		return
	}
	h.handleResponsesNonStream(w, actualModel, apiKeyID, respID, req, toolChoicePlan, ownership,
		semantics, prepareKiro, body, forwarded, r.Context())
}

func (h *Handler) handleResponsesNonStream(
	w http.ResponseWriter, model, apiKeyID, respID string,
	req *ResponsesRequest, toolPlan responsesToolChoicePlan, ownership responseStateOwnership, semantics responsesSemanticRequirements,
	prepareKiro responsesKiroPrepareFunc, rawBody []byte, forwarded bool, requestContext context.Context,
) {
	excluded := make(map[string]bool)
	var lastErr error
	var preparationErr error
	var incompatibility string
	reqStart := time.Now()
	requestedModel := stripRequestedModelThinkingSuffix(requestedModelFromJSON(rawBody, model), config.GetThinkingConfig().Suffix)

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		selection := h.pool.GetNextSelectionForModelWithApiKey(requestedModel, excluded, apiKeyID)
		if selection == nil {
			break
		}
		account := &selection.Account
		providerID, _, resolved := h.resolveCandidateProviderModel(account, requestedModel, model)
		if !resolved {
			excluded[account.ID] = true
			attempt--
			continue
		}
		providerModelID := string(providerID)
		transport, reason := responsesTransportForAccountWithOwnership(account, req, toolPlan, ownership, semantics)
		if transport == responsesTransportUnsupported {
			if reason != "" {
				incompatibility = reason
			}
			excluded[account.ID] = true
			attempt--
			continue
		}
		if forwarded && transport == responsesTransportCustomRelay {
			excluded[account.ID] = true
			attempt--
			continue
		}

		var kiro *responsesKiroPrepared
		if transport == responsesTransportKiro {
			var err error
			kiro, err = prepareKiro()
			if err == nil && respID == "" && kiro != nil {
				respID = kiro.ResponseID
			}
			if err != nil {
				preparationErr = err
				incompatibility = err.Error()
				excluded[account.ID] = true
				attempt--
				continue
			}
		}

		if err := h.ensureValidTokenForSelection(selection); err != nil {
			lastErr = err
			if errors.Is(err, errStaleAccountSelection) {
				continue
			}
			excluded[account.ID] = true
			h.handleTokenValidationFailureForSelection(selection, err)
			continue
		}

		if transport == responsesTransportCustomRelay {
			if fwdErr := h.forwardToUpstream(w, nil, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "responses", streaming: false,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				requestContext: requestContext,
			}); fwdErr != nil {
				if requestContext != nil && requestContext.Err() != nil {
					return
				}
				lastErr = fwdErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				h.handleAccountFailureForSelection(*selection, fwdErr)
				continue
			}
			return
		}

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamStopReason string
		resetBufferedAttempt := func() {
			content = ""
			reasoningContent = ""
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			upstreamStopReason = ""
		}
		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnStopReason:  func(reason string) { upstreamStopReason = reason },
			CanRetry:      func() bool { return true },
			ResetForRetry: resetBufferedAttempt,
		}

		if err := CallKiroAPIContext(requestContext, account, kiro.Payload, callback); err != nil {
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			lastErr = err
			if !h.pool.IsSelectionCurrent(*selection) {
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailureForSelection(*selection, err)
			continue
		}
		if responsesRequiresSerialToolGuard(kiro.Request, toolPlan) && len(toolUses) > 1 {
			incompatibility = fmt.Sprintf("parallel_tool_calls=false candidate emitted %d tool calls", len(toolUses))
			// This is a semantic candidate rejection, not an account/transport
			// failure. Nothing has been committed to the client yet.
			excluded[account.ID] = true
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
		if !kiro.Thinking {
			reasoningContent = ""
		}
		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = kiro.EstimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccessForSelection(*selection)
		h.pool.RecordLatencyForSelection(*selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*selection, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		localReq := kiro.Request
		respObj := buildResponsesObjectWithReasoning(respID, model, finalContent, reasoningContent, toolUses, inputTokens, outputTokens, localReq)
		applyResponsesCompletion(respObj, upstreamStopReason)
		respObj.StoredInput = kiro.StoredInput
		respObj.Instructions = localReq.Instructions
		if saveErr := persistResponsesCompletion(respObj, kiro.StoreResponse); saveErr != nil {
			h.sendOpenAIError(w, http.StatusInternalServerError, "server_error", responsesStatePersistenceFailureMessage)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if lastErr != nil {
		h.recordFailureWithDetails("responses", model, "", apiKeyID, lastErr)
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", lastErr.Error())
		return
	}
	if preparationErr != nil {
		if prep, ok := preparationErr.(*responsesPrepareError); ok {
			errType := "invalid_request_error"
			if prep.Status >= http.StatusInternalServerError {
				errType = "server_error"
			}
			h.sendOpenAIError(w, prep.Status, errType, prep.Message)
			return
		}
		h.sendOpenAIError(w, 400, "invalid_request_error", preparationErr.Error())
		return
	}
	if !semantics.KiroSupported() || toolPlan.Kind == responsesToolChoicePassthroughOnly {
		message := "Responses request requires transparent OpenAI-compatible passthrough"
		if incompatibility != "" {
			message += ": " + incompatibility
		}
		h.sendOpenAIError(w, 400, "invalid_request_error", message)
		return
	}
	if incompatibility != "" {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "No compatible Responses transport is available: "+incompatibility)
		return
	}
	h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available accounts")
}

func mapResponsesCompletion(reason string) (status, incompleteReason string) {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens", "max_output_tokens", "length", "model_context_window_exceeded", "context_window_exceeded":
		return "incomplete", "max_output_tokens"
	case "refusal", "content_filter", "content_filtered", "guardrail_intervened":
		return "incomplete", "content_filter"
	default:
		return "completed", ""
	}
}

func applyResponsesCompletion(resp *ResponsesObject, upstreamStopReason string) {
	if resp == nil {
		return
	}
	status, incompleteReason := mapResponsesCompletion(upstreamStopReason)
	resp.Status = status
	resp.IncompleteDetails = nil
	if incompleteReason != "" {
		resp.IncompleteDetails = &ResponsesIncompleteDetails{Reason: incompleteReason}
	}
}

func responsesTerminalEventName(resp *ResponsesObject) string {
	if resp != nil && resp.Status == "incomplete" {
		return "response.incomplete"
	}
	return "response.completed"
}

func responseMessageOutputItem(id, text string) ResponseOutputItem {
	if id == "" {
		id = generateOutputItemID("msg")
	}
	return ResponseOutputItem{
		ID: id, Type: "message", Role: "assistant", Status: "completed",
		Content: []ResponseContentPart{{Type: "output_text", Text: text, Annotations: []interface{}{}, Logprobs: []interface{}{}}},
	}
}

func responseReasoningOutputItem(id, reasoningText string) ResponseOutputItem {
	if id == "" {
		id = generateOutputItemID("rs")
	}
	return ResponseOutputItem{
		ID: id, Type: "reasoning", Status: "completed",
		Summary: []ResponseReasoningSummaryPart{},
		Content: []ResponseContentPart{{Type: "reasoning_text", Text: reasoningText}},
	}
}

func responseToolOutputItem(id string, tu KiroToolUse) ResponseOutputItem {
	if tu.ToolType == responsesToolKindCustom {
		if id == "" {
			id = generateOutputItemID("ctc")
		}
		input, _ := tu.Input["input"].(string)
		return ResponseOutputItem{
			ID: id, Type: "custom_tool_call", Status: "completed",
			CallID: tu.ToolUseID, Name: tu.Name, Namespace: tu.Namespace, Input: input,
		}
	}
	if id == "" {
		id = generateOutputItemID("fc")
	}
	args, _ := json.Marshal(tu.Input)
	return ResponseOutputItem{
		ID: id, Type: "function_call", Status: "completed",
		CallID: tu.ToolUseID, Name: tu.Name, Namespace: tu.Namespace, Arguments: string(args),
	}
}

func buildResponsesObjectWithOutput(
	id, model string, output []ResponseOutputItem, inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	if len(output) == 0 {
		output = []ResponseOutputItem{responseMessageOutputItem("", "")}
	}
	return &ResponsesObject{
		ID: id, Object: "response", CreatedAt: time.Now().Unix(), Status: "completed", Model: model,
		Output: output, Usage: ResponsesUsage{
			InputTokens: inputTokens, InputTokensDetails: ResponsesInputTokensDetails{},
			OutputTokens: outputTokens, OutputTokensDetails: ResponsesOutputTokensDetails{},
			TotalTokens: inputTokens + outputTokens,
		},
		ToolChoice: responsesToolChoiceForOutput(req), Tools: responsesToolsForOutput(req),
		ParallelToolCalls: responsesParallelToolCalls(req), PreviousResponseID: req.PreviousResponseID, Metadata: req.Metadata,
		StoredToolCatalog: responsesStoredToolCatalogFromRequest(req),
	}
}

func buildResponsesObject(
	id, model, content string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	return buildResponsesObjectWithReasoning(id, model, content, "", toolUses, inputTokens, outputTokens, req)
}

func buildResponsesObjectWithReasoning(
	id, model, content, reasoningText string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 2+len(toolUses))
	if strings.TrimSpace(reasoningText) != "" {
		output = append(output, responseReasoningOutputItem("", reasoningText))
	}
	if strings.TrimSpace(content) != "" {
		output = append(output, responseMessageOutputItem("", content))
	}
	for _, tu := range toolUses {
		output = append(output, responseToolOutputItem("", tu))
	}
	return buildResponsesObjectWithOutput(id, model, output, inputTokens, outputTokens, req)
}

func buildResponsesFailureObject(id, model, message string, createdAt int64, req *ResponsesRequest) *ResponsesObject {
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	return &ResponsesObject{
		ID: id, Object: "response", CreatedAt: createdAt, Status: "failed", Model: model,
		Output: []ResponseOutputItem{}, Usage: ResponsesUsage{},
		ToolChoice: responsesToolChoiceForOutput(req), Tools: responsesToolsForOutput(req),
		ParallelToolCalls: responsesParallelToolCalls(req), PreviousResponseID: req.PreviousResponseID, Metadata: req.Metadata,
		Instructions: req.Instructions, Error: &ResponsesError{Code: "server_error", Message: message},
	}
}

type responsesSSEWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	sequence int
	terminal bool
}

func newResponsesSSEWriter(w http.ResponseWriter) (*responsesSSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("Streaming not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return &responsesSSEWriter{w: w, flusher: flusher}, nil
}

func (s *responsesSSEWriter) send(eventName string, payload map[string]interface{}) {
	if s == nil || s.terminal {
		return
	}
	s.sequence++
	payload["type"] = eventName
	payload["sequence_number"] = s.sequence
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventName, string(data))
	s.flusher.Flush()
}

func (s *responsesSSEWriter) terminalEvent(eventName string, payload map[string]interface{}) {
	if s == nil || s.terminal {
		return
	}
	s.send(eventName, payload)
	s.terminal = true
}

func responseOutputTextPart(text string) map[string]interface{} {
	return map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}, "logprobs": []interface{}{}}
}

func (h *Handler) handleResponsesStream(
	w http.ResponseWriter, model, apiKeyID, respID string,
	req *ResponsesRequest, toolPlan responsesToolChoicePlan, ownership responseStateOwnership, semantics responsesSemanticRequirements,
	prepareKiro responsesKiroPrepareFunc, rawBody []byte, forwarded bool, requestContext context.Context,
) {
	excluded := make(map[string]bool)
	var lastErr error
	var preparationErr error
	var incompatibility string
	var stream *responsesSSEWriter
	var initial *ResponsesObject
	var createdAt int64
	protocolCommitted := false
	activeReq := req
	reqStart := time.Now()
	requestedModel := stripRequestedModelThinkingSuffix(requestedModelFromJSON(rawBody, model), config.GetThinkingConfig().Suffix)

	// Validate the local streaming surface before contacting Kiro, but do not
	// emit response.created yet. Keeping the client protocol uncommitted allows a
	// Kiro attempt that fails before its first callback to fail over to a raw
	// OpenAI-compatible relay without mixing two protocols.
	if _, ok := w.(http.Flusher); !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	startLocalProtocol := func() bool {
		if protocolCommitted {
			return true
		}
		if stream == nil {
			var err error
			stream, err = newResponsesSSEWriter(w)
			if err != nil {
				return false
			}
		}
		createdAt = time.Now().Unix()
		initial = &ResponsesObject{
			ID: respID, Object: "response", CreatedAt: createdAt, Status: "in_progress", Model: model,
			Output: []ResponseOutputItem{}, Usage: ResponsesUsage{}, ToolChoice: responsesToolChoiceForOutput(activeReq),
			Tools: responsesToolsForOutput(activeReq), ParallelToolCalls: responsesParallelToolCalls(activeReq),
			PreviousResponseID: activeReq.PreviousResponseID, Metadata: activeReq.Metadata, Instructions: activeReq.Instructions,
		}
		stream.send("response.created", map[string]interface{}{"response": initial})
		stream.send("response.in_progress", map[string]interface{}{"response": initial})
		protocolCommitted = true
		return true
	}

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		selection := h.pool.GetNextSelectionForModelWithApiKey(requestedModel, excluded, apiKeyID)
		if selection == nil {
			break
		}
		account := &selection.Account
		providerID, _, resolved := h.resolveCandidateProviderModel(account, requestedModel, model)
		if !resolved {
			excluded[account.ID] = true
			attempt--
			continue
		}
		providerModelID := string(providerID)
		transport, reason := responsesTransportForAccountWithOwnership(account, req, toolPlan, ownership, semantics)
		if transport == responsesTransportUnsupported {
			if reason != "" {
				incompatibility = reason
			}
			excluded[account.ID] = true
			attempt--
			continue
		}
		if forwarded && transport == responsesTransportCustomRelay {
			excluded[account.ID] = true
			attempt--
			continue
		}
		if protocolCommitted && transport == responsesTransportCustomRelay {
			excluded[account.ID] = true
			attempt--
			continue
		}

		var kiro *responsesKiroPrepared
		if transport == responsesTransportKiro {
			var err error
			kiro, err = prepareKiro()
			if err == nil && kiro != nil {
				if respID == "" {
					respID = kiro.ResponseID
				}
				if kiro.Request != nil {
					activeReq = kiro.Request
				}
			}
			if err != nil {
				preparationErr = err
				incompatibility = err.Error()
				excluded[account.ID] = true
				attempt--
				continue
			}
		}

		if err := h.ensureValidTokenForSelection(selection); err != nil {
			lastErr = err
			if errors.Is(err, errStaleAccountSelection) {
				continue
			}
			excluded[account.ID] = true
			h.handleTokenValidationFailureForSelection(selection, err)
			continue
		}

		if transport == responsesTransportCustomRelay {
			if fwdErr := h.forwardToUpstream(w, nil, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "responses", streaming: true,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				requestContext: requestContext,
			}); fwdErr != nil {
				if requestContext != nil && requestContext.Err() != nil {
					return
				}
				lastErr = fwdErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				h.handleAccountFailureForSelection(*selection, fwdErr)
				continue
			}
			return
		}

		var fullText strings.Builder
		var currentMessageText strings.Builder
		var reasoningText strings.Builder
		var currentReasoningText strings.Builder
		var toolUses []KiroToolUse
		var streamOutputItems []ResponseOutputItem
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamStopReason string
		serialGuard := responsesRequiresSerialToolGuard(activeReq, toolPlan)
		type bufferedResponsesStreamEvent struct {
			text       string
			isThinking bool
			toolUse    *KiroToolUse
		}
		var bufferedEvents []bufferedResponsesStreamEvent
		resetBufferedStreamAttempt := func() {
			bufferedEvents = nil
			toolUses = nil
			inputTokens = 0
			outputTokens = 0
			credits = 0
			realInputTokens = 0
			upstreamStopReason = ""
		}
		messageItemID := ""
		messageStarted := false
		reasoningItemID := ""
		reasoningStarted := false
		outputIndex := 0
		contentIndex := 0

		var finishMessage func(string)
		var finishReasoning func(string)

		ensureReasoningStarted := func() bool {
			if reasoningStarted {
				return true
			}
			if !startLocalProtocol() {
				return false
			}
			reasoningStarted = true
			reasoningItemID = generateOutputItemID("rs")
			stream.send("response.output_item.added", map[string]interface{}{
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id": reasoningItemID, "type": "reasoning", "status": "in_progress",
					"summary": []map[string]interface{}{}, "content": []map[string]interface{}{},
				},
			})
			return true
		}

		ensureMessageStarted := func() bool {
			if messageStarted {
				return true
			}
			if !startLocalProtocol() {
				return false
			}
			messageStarted = true
			messageItemID = generateOutputItemID("msg")
			stream.send("response.output_item.added", map[string]interface{}{
				"output_index": outputIndex,
				"item":         map[string]interface{}{"id": messageItemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []map[string]interface{}{}},
			})
			stream.send("response.content_part.added", map[string]interface{}{
				"item_id": messageItemID, "output_index": outputIndex, "content_index": contentIndex,
				"part": responseOutputTextPart(""),
			})
			return true
		}

		finishReasoning = func(text string) {
			if !reasoningStarted {
				return
			}
			item := responseReasoningOutputItem(reasoningItemID, text)
			stream.send("response.reasoning_text.done", map[string]interface{}{
				"item_id": reasoningItemID, "output_index": outputIndex, "content_index": 0,
				"text": text,
			})
			stream.send("response.output_item.done", map[string]interface{}{
				"output_index": outputIndex, "item": item,
			})
			streamOutputItems = append(streamOutputItems, item)
			reasoningStarted = false
			currentReasoningText.Reset()
			outputIndex++
		}

		finishMessage = func(text string) {
			if !messageStarted {
				return
			}
			item := responseMessageOutputItem(messageItemID, text)
			stream.send("response.output_text.done", map[string]interface{}{
				"item_id": messageItemID, "output_index": outputIndex, "content_index": contentIndex,
				"text": text, "logprobs": []interface{}{},
			})
			stream.send("response.content_part.done", map[string]interface{}{
				"item_id": messageItemID, "output_index": outputIndex, "content_index": contentIndex,
				"part": responseOutputTextPart(text),
			})
			stream.send("response.output_item.done", map[string]interface{}{
				"output_index": outputIndex, "item": item,
			})
			streamOutputItems = append(streamOutputItems, item)
			messageStarted = false
			currentMessageText.Reset()
			outputIndex++
		}

		emitText := func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				if !kiro.Thinking {
					return
				}
				finishMessage(currentMessageText.String())
				reasoningText.WriteString(text)
				currentReasoningText.WriteString(text)
				if !ensureReasoningStarted() {
					return
				}
				stream.send("response.reasoning_text.delta", map[string]interface{}{
					"item_id": reasoningItemID, "output_index": outputIndex, "content_index": 0,
					"delta": text,
				})
				return
			}
			finishReasoning(currentReasoningText.String())
			fullText.WriteString(text)
			currentMessageText.WriteString(text)
			if !ensureMessageStarted() {
				return
			}
			stream.send("response.output_text.delta", map[string]interface{}{
				"item_id": messageItemID, "output_index": outputIndex, "content_index": contentIndex,
				"delta": text, "logprobs": []interface{}{},
			})
		}

		emitToolUse := func(tu KiroToolUse) {
			if !startLocalProtocol() {
				return
			}
			finishReasoning(currentReasoningText.String())
			finishMessage(currentMessageText.String())
			item := responseToolOutputItem("", tu)
			addedItem := map[string]interface{}{
				"id": item.ID, "type": item.Type, "status": "in_progress", "call_id": item.CallID,
				"name": item.Name,
			}
			if item.Namespace != "" {
				addedItem["namespace"] = item.Namespace
			}
			if item.Type == "custom_tool_call" {
				addedItem["input"] = ""
				stream.send("response.output_item.added", map[string]interface{}{
					"output_index": outputIndex, "item": addedItem,
				})
				stream.send("response.custom_tool_call_input.delta", map[string]interface{}{
					"item_id": item.ID, "call_id": item.CallID, "output_index": outputIndex, "delta": item.Input,
				})
				stream.send("response.custom_tool_call_input.done", map[string]interface{}{
					"item_id": item.ID, "call_id": item.CallID, "output_index": outputIndex, "input": item.Input,
				})
			} else {
				addedItem["arguments"] = ""
				stream.send("response.output_item.added", map[string]interface{}{
					"output_index": outputIndex, "item": addedItem,
				})
				stream.send("response.function_call_arguments.delta", map[string]interface{}{
					"item_id": item.ID, "output_index": outputIndex, "delta": item.Arguments,
				})
				done := map[string]interface{}{
					"item_id": item.ID, "output_index": outputIndex, "name": item.Name, "arguments": item.Arguments,
				}
				if item.Namespace != "" {
					done["namespace"] = item.Namespace
				}
				stream.send("response.function_call_arguments.done", done)
			}
			stream.send("response.output_item.done", map[string]interface{}{
				"output_index": outputIndex, "item": item,
			})
			streamOutputItems = append(streamOutputItems, item)
			outputIndex++
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if serialGuard {
					bufferedEvents = append(bufferedEvents, bufferedResponsesStreamEvent{text: text, isThinking: isThinking})
					return
				}
				emitText(text, isThinking)
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
				if serialGuard {
					copyTU := tu
					bufferedEvents = append(bufferedEvents, bufferedResponsesStreamEvent{toolUse: &copyTU})
					return
				}
				emitToolUse(tu)
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			CanRetry: func() bool {
				return serialGuard && !protocolCommitted
			},
			ResetForRetry: func() {
				if serialGuard {
					resetBufferedStreamAttempt()
				}
			},
		}

		if err := CallKiroAPIContext(requestContext, account, kiro.Payload, callback); err != nil {
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			if !protocolCommitted {
				lastErr = err
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				h.handleAccountFailureForSelection(*selection, err)
				continue
			}
			stream.terminalEvent("response.failed", map[string]interface{}{
				"response": buildResponsesFailureObject(respID, model, err.Error(), createdAt, activeReq),
			})
			h.recordFailureWithDetails("responses", model, account.ID, apiKeyID, err)
			return
		}
		if serialGuard {
			if len(toolUses) > 1 {
				incompatibility = fmt.Sprintf("parallel_tool_calls=false candidate emitted %d tool calls", len(toolUses))
				excluded[account.ID] = true
				continue
			}
			for _, event := range bufferedEvents {
				if event.toolUse != nil {
					emitToolUse(*event.toolUse)
				} else {
					emitText(event.text, event.isThinking)
				}
			}
		}

		finalContent, _ := extractThinkingFromContent(fullText.String())
		reasoning := reasoningText.String()
		if !kiro.Thinking {
			reasoning = ""
		}
		if reasoningStarted {
			finishReasoning(currentReasoningText.String())
		}
		if messageStarted {
			finishMessage(currentMessageText.String())
		}
		if len(streamOutputItems) == 0 {
			if !ensureMessageStarted() {
				h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
				return
			}
			finishMessage("")
		}
		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = kiro.EstimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccessForSelection(*selection)
		h.pool.RecordLatencyForSelection(*selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*selection, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		respObj := buildResponsesObjectWithOutput(respID, model, streamOutputItems, inputTokens, outputTokens, activeReq)
		applyResponsesCompletion(respObj, upstreamStopReason)
		respObj.CreatedAt = createdAt
		respObj.StoredInput = kiro.StoredInput
		respObj.Instructions = activeReq.Instructions
		if saveErr := persistResponsesCompletion(respObj, kiro.StoreResponse); saveErr != nil {
			stream.terminalEvent("response.failed", map[string]interface{}{
				"response": buildResponsesFailureObject(respID, model, responsesStatePersistenceFailureMessage, createdAt, activeReq),
			})
			return
		}
		stream.terminalEvent(responsesTerminalEventName(respObj), map[string]interface{}{"response": respObj})
		return
	}

	message := "No available accounts"
	status := http.StatusServiceUnavailable
	errType := "server_error"
	if lastErr != nil {
		message = lastErr.Error()
		h.recordFailureWithDetails("responses", model, "", apiKeyID, lastErr)
	} else if preparationErr != nil {
		status = http.StatusBadRequest
		errType = "invalid_request_error"
		message = preparationErr.Error()
		if prep, ok := preparationErr.(*responsesPrepareError); ok {
			status = prep.Status
			message = prep.Message
			if prep.Status >= http.StatusInternalServerError {
				errType = "server_error"
			}
		}
	} else if !semantics.KiroSupported() || toolPlan.Kind == responsesToolChoicePassthroughOnly {
		status = http.StatusBadRequest
		errType = "invalid_request_error"
		message = "Responses request requires transparent OpenAI-compatible passthrough"
		if incompatibility != "" {
			message += ": " + incompatibility
		}
	} else if incompatibility != "" {
		status = http.StatusBadRequest
		errType = "invalid_request_error"
		message = "No compatible Responses transport is available: " + incompatibility
	}

	if !protocolCommitted {
		h.sendOpenAIError(w, status, errType, message)
		return
	}
	stream.terminalEvent("response.failed", map[string]interface{}{
		"response": buildResponsesFailureObject(respID, model, message, createdAt, activeReq),
	})
}
