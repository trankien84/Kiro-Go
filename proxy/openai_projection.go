package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Keep the OpenAI names as source-compatible aliases while both OpenAI and
// Claude/Kiro consume the same request-side fidelity primitive.
type openAIRequestProjectionFidelity = requestProjectionFidelity

const (
	openAIRequestProjectionExact       = requestProjectionExact
	openAIRequestProjectionEmulated    = requestProjectionEmulated
	openAIRequestProjectionUnsupported = requestProjectionUnsupported
)

type openAIProjectionIssue struct {
	Path     string
	Code     string
	Reason   string
	Fidelity openAIRequestProjectionFidelity
}

type openAIProjection struct {
	Fidelity openAIRequestProjectionFidelity
	Issues   []openAIProjectionIssue
}

func (p *openAIProjection) record(fidelity openAIRequestProjectionFidelity, path, code, reason string) {
	if p == nil {
		return
	}
	if fidelity > p.Fidelity {
		p.Fidelity = fidelity
	}
	for _, existing := range p.Issues {
		if existing.Fidelity == fidelity && existing.Path == path && existing.Code == code && existing.Reason == reason {
			return
		}
	}
	p.Issues = append(p.Issues, openAIProjectionIssue{
		Path: path, Code: code, Reason: reason, Fidelity: fidelity,
	})
}

// add is intentionally the fail-closed shorthand used by existing semantic
// checks. New intentional compatibility transformations must use emulate.
func (p *openAIProjection) add(path, code, reason string) {
	p.record(openAIRequestProjectionUnsupported, path, code, reason)
}

func (p *openAIProjection) emulate(path, code, reason string) {
	p.record(openAIRequestProjectionEmulated, path, code, reason)
}

func (p *openAIProjection) merge(other openAIProjection) {
	if p == nil {
		return
	}
	for _, issue := range other.Issues {
		p.record(issue.Fidelity, issue.Path, issue.Code, issue.Reason)
	}
	if other.Fidelity > p.Fidelity {
		p.Fidelity = other.Fidelity
	}
}

func (p openAIProjection) Supported() bool {
	return p.Fidelity != openAIRequestProjectionUnsupported
}

func (p openAIProjection) Exact() bool {
	return p.Fidelity == openAIRequestProjectionExact
}

func (p openAIProjection) Emulated() bool {
	return p.Fidelity == openAIRequestProjectionEmulated
}

func (p openAIProjection) Reason() string {
	if len(p.Issues) == 0 {
		return ""
	}
	// Prefer an unsupported issue when one exists so callers never surface a
	// harmless emulation note instead of the actual reason dispatch is blocked.
	issue := p.Issues[0]
	for _, candidate := range p.Issues {
		if candidate.Fidelity == openAIRequestProjectionUnsupported {
			issue = candidate
			break
		}
	}
	if issue.Path == "" {
		return issue.Reason
	}
	return fmt.Sprintf("%s: %s", issue.Path, issue.Reason)
}

func decodeJSONObject(raw []byte) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return root, nil
}

func projectionUnknownFields(p *openAIProjection, path string, values map[string]json.RawMessage, allowed map[string]struct{}) {
	unknown := make([]string, 0)
	for key := range values {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		p.add(joinJSONPath(path, key), "unsupported_field", "field requires transparent OpenAI-compatible passthrough")
	}
}

func joinJSONPath(base, field string) string {
	if base == "" {
		return field
	}
	if strings.HasPrefix(field, "[") {
		return base + field
	}
	return base + "." + field
}

var openAIChatKiroTopLevelFields = map[string]struct{}{
	"model": {}, "messages": {}, "max_tokens": {}, "temperature": {}, "top_p": {},
	"stream": {}, "tools": {}, "tool_choice": {}, "parallel_tool_calls": {},
	"thinking": {}, "output_config": {}, "reasoning": {}, "reasoning_effort": {},
}

var openAIChatBedrockTopLevelFields = map[string]struct{}{
	"model": {}, "messages": {}, "tools": {}, "tool_choice": {}, "max_tokens": {},
	"max_completion_tokens": {}, "temperature": {}, "top_p": {}, "stop": {}, "stream": {},
}

func analyzeOpenAIChatKiroProjection(raw []byte) openAIProjection {
	var p openAIProjection
	root, err := decodeJSONObject(raw)
	if err != nil {
		p.add("", "invalid_json", err.Error())
		return p
	}
	projectionUnknownFields(&p, "", root, openAIChatKiroTopLevelFields)
	analyzeOpenAIKiroReasoningControlProjection(&p, root)
	analyzeKiroTokenLimitProjection(&p, root["max_tokens"], "max_tokens")
	analyzeChatMessagesProjection(&p, root["messages"], chatProjectionKiro)
	analyzeOpenAIToolsProjection(&p, root["tools"], "tools", true, false)
	analyzeKiroToolChoiceProjection(&p, root["tool_choice"])
	if rawParallel, ok := root["parallel_tool_calls"]; ok && !jsonRawIsNull(rawParallel) {
		var parallel bool
		if json.Unmarshal(rawParallel, &parallel) != nil {
			p.add("parallel_tool_calls", "invalid_shape", "must be a boolean")
		} else if !parallel {
			p.add("parallel_tool_calls", "unsupported_value", "parallel_tool_calls=false cannot be guaranteed by Kiro")
		}
	}
	return p
}

func analyzeOpenAIKiroReasoningControlProjection(p *openAIProjection, root map[string]json.RawMessage) {
	controls := []struct {
		field   string
		allowed map[string]struct{}
	}{
		{field: "thinking", allowed: map[string]struct{}{"type": {}, "budget_tokens": {}, "display": {}}},
		{field: "output_config", allowed: map[string]struct{}{"effort": {}}},
		{field: "reasoning", allowed: map[string]struct{}{"effort": {}}},
	}
	for _, control := range controls {
		raw, ok := root[control.field]
		if !ok || len(raw) == 0 || jsonRawIsNull(raw) {
			continue
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			p.add(control.field, "invalid_reasoning_control", control.field+" must be an object")
			continue
		}
		projectionUnknownFields(p, control.field, values, control.allowed)
	}

	if raw, ok := root["reasoning_effort"]; ok && len(raw) > 0 && !jsonRawIsNull(raw) {
		var effort string
		if err := json.Unmarshal(raw, &effort); err != nil {
			p.add("reasoning_effort", "invalid_reasoning_effort", "reasoning_effort must be a string")
		}
	}

	if raw, ok := root["thinking"]; ok && len(raw) > 0 && !jsonRawIsNull(raw) {
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) == nil && values != nil {
			for _, field := range []string{"type", "display"} {
				if valueRaw, present := values[field]; present && !jsonRawIsNull(valueRaw) {
					var value string
					if err := json.Unmarshal(valueRaw, &value); err != nil {
						p.add("thinking."+field, "invalid_reasoning_control", "thinking."+field+" must be a string")
					}
				}
			}
			if budgetRaw, present := values["budget_tokens"]; present && !jsonRawIsNull(budgetRaw) {
				var budget int
				if err := json.Unmarshal(budgetRaw, &budget); err != nil || budget <= 0 {
					p.add("thinking.budget_tokens", "invalid_budget_tokens", "explicit thinking.budget_tokens must be a positive integer")
				}
			}
		}
	}

	// OpenAI compatibility clients may send effort through several aliases. The
	// production builder intentionally has a precedence order, so conflicting
	// non-empty values would otherwise make lower-priority client intent vanish.
	efforts := make(map[string]string)
	collectEffort := func(path string, raw json.RawMessage) {
		if len(raw) == 0 || jsonRawIsNull(raw) {
			return
		}
		var effort string
		if err := json.Unmarshal(raw, &effort); err != nil {
			p.add(path, "invalid_reasoning_effort", path+" must be a string")
			return
		}
		if canonical := canonicalEffort(effort); canonical != "" {
			efforts[path] = canonical
		}
	}
	for _, field := range []string{"output_config", "reasoning"} {
		var object map[string]json.RawMessage
		if raw, ok := root[field]; ok && json.Unmarshal(raw, &object) == nil && object != nil {
			collectEffort(field+".effort", object["effort"])
		}
	}
	collectEffort("reasoning_effort", root["reasoning_effort"])
	orderedEffortPaths := []string{"output_config.effort", "reasoning.effort", "reasoning_effort"}
	var firstPath, firstValue string
	for _, path := range orderedEffortPaths {
		value, ok := efforts[path]
		if !ok {
			continue
		}
		if firstPath == "" {
			firstPath, firstValue = path, value
			continue
		}
		if value != firstValue {
			p.add(path, "conflicting_reasoning_effort", fmt.Sprintf("conflicting reasoning effort %q; %s requested %q", value, firstPath, firstValue))
		}
	}
}

func analyzeOpenAIKiroReasoningCapabilityProjection(p *openAIProjection, req *OpenAIRequest, capability ModelReasoningCapability) {
	if p == nil || req == nil || req.Thinking == nil {
		return
	}
	thinkingType := strings.ToLower(strings.TrimSpace(req.Thinking.Type))
	effort := ""
	if req.OutputConfig != nil {
		effort = canonicalEffort(req.OutputConfig.Effort)
	}
	if effort == "" && req.Reasoning != nil {
		effort = canonicalEffort(req.Reasoning.Effort)
	}
	if effort == "" {
		effort = canonicalEffort(req.ReasoningEffort)
	}
	reasoningActive := thinkingType == "enabled" || thinkingType == "adaptive" || (effort != "" && effort != "none")
	reasoningDisabled := thinkingType == "disabled" || thinkingType == "none" || effort == "none"

	if req.Thinking.BudgetTokens > 0 {
		switch {
		case reasoningDisabled:
			p.add("thinking.budget_tokens", "budget_tokens_dropped", "Kiro reasoning builder drops budget_tokens when thinking is disabled")
		case !reasoningActive:
			p.add("thinking.budget_tokens", "budget_tokens_dropped", "thinking.budget_tokens alone does not activate Kiro reasoning and would be dropped")
		case !capability.SupportsBudgetTokens:
			p.add("thinking.budget_tokens", "unsupported_budget_tokens", "resolved Kiro model does not support thinking.budget_tokens")
		}
	}
	if strings.TrimSpace(req.Thinking.Display) != "" {
		switch {
		case reasoningDisabled:
			p.add("thinking.display", "thinking_display_dropped", "Kiro reasoning builder drops thinking.display when thinking is disabled")
		case !reasoningActive:
			p.add("thinking.display", "thinking_display_dropped", "thinking.display alone does not activate Kiro reasoning and would be dropped")
		}
	}
}

func analyzeKiroTokenLimitProjection(p *openAIProjection, raw json.RawMessage, path string) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		p.add(path, "invalid_token_limit", path+" must be an integer")
		return
	}
	if value <= 0 {
		p.add(path, "invalid_token_limit", path+" must be greater than zero for the Kiro adapter")
	}
}

func analyzeOpenAIChatBedrockProjection(raw []byte, specificToolChoiceSupported bool) openAIProjection {
	var p openAIProjection
	root, err := decodeJSONObject(raw)
	if err != nil {
		p.add("", "invalid_json", err.Error())
		return p
	}
	projectionUnknownFields(&p, "", root, openAIChatBedrockTopLevelFields)
	analyzeChatMessagesProjection(&p, root["messages"], chatProjectionBedrock)
	analyzeOpenAIToolsProjection(&p, root["tools"], "tools", false, false)
	analyzeBedrockToolChoiceProjection(&p, root["tool_choice"], specificToolChoiceSupported, declaredOpenAIFunctionToolNames(root["tools"]))
	analyzeBedrockSamplingProjection(&p, root, "temperature")
	analyzeBedrockSamplingProjection(&p, root, "top_p")
	analyzeBedrockTokenLimitProjection(&p, root, "max_tokens")
	analyzeBedrockTokenLimitProjection(&p, root, "max_completion_tokens")
	if maxTokens, ok := root["max_tokens"]; ok && !jsonRawIsNull(maxTokens) {
		if maxCompletion, ok := root["max_completion_tokens"]; ok && !jsonRawIsNull(maxCompletion) {
			p.add("max_completion_tokens", "conflicting_token_limits", "Bedrock adapter would ignore max_completion_tokens when max_tokens is also present")
		}
	}
	analyzeBedrockStopProjection(&p, root["stop"])
	return p
}

type chatProjectionTarget uint8

const (
	chatProjectionKiro chatProjectionTarget = iota
	chatProjectionBedrock
)

type projectionToolCorrelation struct {
	rootPath      string
	pending       map[string]string
	seenCalls     map[string]struct{}
	seenResults   map[string]struct{}
	resultStarted bool
	completedTurn bool
}

func newProjectionToolCorrelation(rootPath string) *projectionToolCorrelation {
	return &projectionToolCorrelation{
		rootPath:    rootPath,
		pending:     make(map[string]string),
		seenCalls:   make(map[string]struct{}),
		seenResults: make(map[string]struct{}),
	}
}

func (c *projectionToolCorrelation) beforeNonToolTurn(p *openAIProjection, path string, target chatProjectionTarget) {
	if c == nil {
		return
	}
	if len(c.pending) > 0 {
		p.add(path, "missing_tool_results", "a new conversation turn appears before all pending tool calls have results")
	}
	if c.completedTurn && target == chatProjectionKiro {
		p.emulate(path, "historical_tool_state_emulation", "Kiro flattens completed historical tool calls/results into conversational text")
	}
	c.completedTurn = false
	c.resultStarted = false
}

func (c *projectionToolCorrelation) addCalls(p *openAIProjection, calls []assistantToolCallProjection) {
	if c == nil {
		return
	}
	if len(c.pending) > 0 {
		p.add(c.rootPath, "overlapping_tool_calls", "new assistant tool calls appear before prior tool calls are resolved")
	}
	for _, call := range calls {
		if call.ID == "" {
			continue
		}
		if _, exists := c.seenCalls[call.ID]; exists {
			p.add(call.Path+".id", "duplicate_tool_call_id", fmt.Sprintf("tool call id %q is repeated in the request history", call.ID))
			continue
		}
		c.seenCalls[call.ID] = struct{}{}
		c.pending[call.ID] = call.Path
	}
	c.completedTurn = false
	c.resultStarted = false
}

func (c *projectionToolCorrelation) addParallelCall(p *openAIProjection, call assistantToolCallProjection) {
	if c == nil || call.ID == "" {
		return
	}
	if c.resultStarted && len(c.pending) > 0 {
		p.add(c.rootPath, "interleaved_tool_call", "a new tool call appears after results for the current parallel tool-call turn have started")
	}
	if _, exists := c.seenCalls[call.ID]; exists {
		p.add(call.Path+".id", "duplicate_tool_call_id", fmt.Sprintf("tool call id %q is repeated in the request history", call.ID))
		return
	}
	c.seenCalls[call.ID] = struct{}{}
	c.pending[call.ID] = call.Path
	c.completedTurn = false
}

func (c *projectionToolCorrelation) addResult(p *openAIProjection, path, callID string) {
	if c == nil || callID == "" {
		return
	}
	c.resultStarted = true
	if _, duplicate := c.seenResults[callID]; duplicate {
		p.add(path, "duplicate_tool_result", fmt.Sprintf("tool call %q has more than one result", callID))
		return
	}
	c.seenResults[callID] = struct{}{}
	if _, exists := c.pending[callID]; !exists {
		p.add(path, "unknown_tool_call_id", fmt.Sprintf("tool result references unknown or inactive tool call %q", callID))
		return
	}
	delete(c.pending, callID)
	if len(c.pending) == 0 {
		c.completedTurn = true
	}
}

func (c *projectionToolCorrelation) finish(p *openAIProjection) {
	if c == nil || len(c.pending) == 0 {
		return
	}
	p.add(c.rootPath, "missing_tool_results", "request history ends before all assistant tool calls have corresponding tool results")
}

func analyzeChatMessagesProjection(p *openAIProjection, raw json.RawMessage, target chatProjectionTarget) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		p.add("messages", "missing_messages", "messages must not be empty")
		return
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		p.add("messages", "invalid_shape", "messages must be an array of objects")
		return
	}
	if len(messages) == 0 {
		p.add("messages", "missing_messages", "messages must not be empty")
		return
	}

	seenNonSystem := false
	toolState := newProjectionToolCorrelation("messages")
	for i, msg := range messages {
		path := fmt.Sprintf("messages[%d]", i)
		var role string
		if err := json.Unmarshal(msg["role"], &role); err != nil || strings.TrimSpace(role) == "" {
			p.add(joinJSONPath(path, "role"), "invalid_role", "role must be a non-empty string")
			continue
		}
		role = strings.ToLower(strings.TrimSpace(role))

		if role != "tool" {
			toolState.beforeNonToolTurn(p, path, target)
		}

		if role == "system" {
			if seenNonSystem {
				p.add(joinJSONPath(path, "role"), "instruction_order_loss", "system messages after non-system turns would be hoisted by this translated transport")
			} else if target == chatProjectionKiro {
				p.emulate(joinJSONPath(path, "role"), "system_priming_emulation", "Kiro represents privileged system instructions as a synthetic conversation priming pair")
			}
		} else {
			seenNonSystem = true
		}

		allowed := map[string]struct{}{"role": {}, "content": {}}
		switch role {
		case "assistant":
			allowed["tool_calls"] = struct{}{}
		case "tool":
			allowed["tool_call_id"] = struct{}{}
		}
		projectionUnknownFields(p, path, msg, allowed)

		switch role {
		case "system":
			if len(msg["content"]) == 0 || jsonRawIsNull(msg["content"]) {
				p.add(joinJSONPath(path, "content"), "missing_content", "system messages require content")
			} else {
				analyzeChatTextOnlyContentProjection(p, joinJSONPath(path, "content"), msg["content"], target)
			}
		case "developer":
			p.add(joinJSONPath(path, "role"), "instruction_role_loss", "developer-role precedence is not preserved by this translated transport")
		case "user":
			analyzeUserContentProjection(p, joinJSONPath(path, "content"), msg["content"], target)
		case "assistant":
			analyzeChatTextOnlyContentProjection(p, joinJSONPath(path, "content"), msg["content"], target)
			calls := analyzeAssistantToolCallsProjection(p, joinJSONPath(path, "tool_calls"), msg["tool_calls"])
			toolState.addCalls(p, calls)
			if target == chatProjectionKiro &&
				(len(msg["content"]) == 0 || jsonRawIsNull(msg["content"])) &&
				(len(msg["tool_calls"]) == 0 || jsonRawIsNull(msg["tool_calls"])) {
				p.add(path, "empty_assistant_message", "Kiro cannot preserve an empty assistant message")
			}
		case "tool":
			if len(msg["content"]) == 0 || jsonRawIsNull(msg["content"]) {
				p.add(joinJSONPath(path, "content"), "missing_content", "tool messages require content")
			} else {
				analyzeChatTextOnlyContentProjection(p, joinJSONPath(path, "content"), msg["content"], target)
			}
			var toolCallID string
			if err := json.Unmarshal(msg["tool_call_id"], &toolCallID); err != nil || strings.TrimSpace(toolCallID) == "" {
				p.add(joinJSONPath(path, "tool_call_id"), "missing_tool_call_id", "tool messages require tool_call_id")
			} else {
				toolState.addResult(p, joinJSONPath(path, "tool_call_id"), strings.TrimSpace(toolCallID))
			}
		default:
			p.add(joinJSONPath(path, "role"), "unsupported_role", fmt.Sprintf("role %q requires transparent passthrough", role))
		}
	}
	toolState.finish(p)
}

func analyzeChatTextOnlyContentProjection(p *openAIProjection, path string, raw json.RawMessage, target chatProjectionTarget) {
	if target != chatProjectionBedrock {
		analyzeTextOnlyContentProjection(p, path, raw)
		return
	}
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		p.add(path, "unsupported_content", "content is not losslessly representable by the Bedrock adapter")
		return
	}
	for i, part := range parts {
		partPath := fmt.Sprintf("%s[%d]", path, i)
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		if typ != "text" {
			p.add(joinJSONPath(partPath, "type"), "unsupported_content_type", fmt.Sprintf("Bedrock adapter does not preserve content type %q", typ))
			continue
		}
		projectionUnknownFields(p, partPath, part, map[string]struct{}{"type": {}, "text": {}})
		var value string
		if err := json.Unmarshal(part["text"], &value); err != nil {
			p.add(joinJSONPath(partPath, "text"), "invalid_text", "text must be a string")
		}
	}
}

func analyzeTextOnlyContentProjection(p *openAIProjection, path string, raw json.RawMessage) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		p.add(path, "unsupported_content", "content is not losslessly representable as text")
		return
	}
	for i, part := range parts {
		partPath := fmt.Sprintf("%s[%d]", path, i)
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		if typ != "text" && typ != "input_text" && typ != "output_text" {
			p.add(joinJSONPath(partPath, "type"), "unsupported_content_type", fmt.Sprintf("content type %q is not text-only", typ))
			continue
		}
		allowed := map[string]struct{}{"type": {}, "text": {}}
		projectionUnknownFields(p, partPath, part, allowed)
		var value string
		if err := json.Unmarshal(part["text"], &value); err != nil {
			p.add(joinJSONPath(partPath, "text"), "invalid_text", "text must be a string")
		}
	}
}

func analyzeUserContentProjection(p *openAIProjection, path string, raw json.RawMessage, target chatProjectionTarget) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		p.add(path, "missing_content", "user messages require content")
		return
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			if target == chatProjectionKiro {
				p.add(path, "empty_user_content", "Kiro cannot preserve an empty user message")
			}
			// The Bedrock/Anthropic adapter deliberately drops an empty turn; it
			// carries no semantic content and Anthropic rejects empty text blocks.
			return
		}
		if target == chatProjectionKiro {
			trimmed := strings.TrimSpace(text)
			if trimmed == "" {
				p.add(path, "blank_user_content", "Kiro would replace a whitespace-only current user message with fallback content")
			} else if trimmed != text {
				p.emulate(path, "user_text_trim_emulation", "Kiro trims leading/trailing whitespace from user text")
			}
		}
		return
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		p.add(path, "unsupported_content", "user content shape requires transparent passthrough")
		return
	}

	hasImage := false
	hasText := false
	var projectedText strings.Builder
	for i, part := range parts {
		partPath := fmt.Sprintf("%s[%d]", path, i)
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		switch typ {
		case "text", "input_text":
			if target == chatProjectionBedrock && typ != "text" {
				p.add(joinJSONPath(partPath, "type"), "unsupported_content_type", fmt.Sprintf("Bedrock adapter does not preserve content type %q", typ))
				continue
			}
			allowed := map[string]struct{}{"type": {}, "text": {}}
			projectionUnknownFields(p, partPath, part, allowed)
			var value string
			if json.Unmarshal(part["text"], &value) != nil {
				p.add(joinJSONPath(partPath, "text"), "invalid_text", "text must be a string")
				continue
			}
			hasText = true
			projectedText.WriteString(value)
		case "image_url", "input_image", "image":
			hasImage = true
			analyzeInlineImageProjection(p, partPath, part, target)
		default:
			p.add(joinJSONPath(partPath, "type"), "unsupported_content_type", fmt.Sprintf("content type %q requires transparent passthrough", typ))
		}
	}

	if !hasImage && (!hasText || projectedText.Len() == 0) {
		p.add(path, "empty_user_content", "translated transport would drop or synthesize content for an empty user message")
		return
	}
	if target != chatProjectionKiro {
		return
	}

	originalText := projectedText.String()
	convertedText := originalText
	if hasImage {
		convertedText = sanitizeImagePlaceholders(convertedText)
	}
	normalized := normalizeUserContent(convertedText, hasImage)
	if hasImage && originalText == "" && normalized != "" {
		p.emulate(path, "image_prompt_synthesis", "Kiro synthesizes text when a user message contains only images")
	} else if normalized != originalText {
		code := "user_text_trim_emulation"
		reason := "Kiro trims leading/trailing whitespace from user text"
		if hasImage {
			code = "image_text_normalization"
			reason = "Kiro removes image placeholders and normalizes surrounding user text for multimodal input"
		}
		p.emulate(path, code, reason)
	}
}

func analyzeInlineImageProjection(p *openAIProjection, path string, part map[string]json.RawMessage, target chatProjectionTarget) {
	allowed := map[string]struct{}{"type": {}, "image_url": {}, "url": {}, "data": {}, "image_base64": {}, "b64_json": {}, "media_type": {}, "mime_type": {}, "mime": {}, "file_id": {}, "detail": {}}
	projectionUnknownFields(p, path, part, allowed)
	if _, ok := part["file_id"]; ok {
		p.add(joinJSONPath(path, "file_id"), "remote_image_reference", "file-backed images are not fetched by translated transports")
	}
	if _, ok := part["detail"]; ok {
		p.add(joinJSONPath(path, "detail"), "image_detail_loss", "image detail policy is not preserved")
	}
	if imageURLRaw, ok := part["image_url"]; ok && !jsonRawIsNull(imageURLRaw) {
		var imageURL map[string]json.RawMessage
		if json.Unmarshal(imageURLRaw, &imageURL) == nil {
			projectionUnknownFields(p, joinJSONPath(path, "image_url"), imageURL, map[string]struct{}{"url": {}, "detail": {}})
			if _, hasDetail := imageURL["detail"]; hasDetail {
				p.add(joinJSONPath(path, "image_url.detail"), "image_detail_loss", "image detail policy is not preserved")
			}
		}
	}

	var normalized map[string]interface{}
	encoded, _ := json.Marshal(part)
	_ = json.Unmarshal(encoded, &normalized)
	if target == chatProjectionKiro {
		if extractImageFromOpenAIPart(normalized) == nil {
			p.add(path, "remote_or_invalid_image", "only inline image data supported by Kiro can be projected losslessly")
		}
		return
	}

	// Bedrock OpenAI bridge supports only Chat-style image_url data URLs.
	if typ, _ := normalized["type"].(string); typ != "image_url" {
		p.add(joinJSONPath(path, "type"), "unsupported_image_shape", "Bedrock OpenAI projection only supports image_url data URLs")
		return
	}
	if openAIImageURLToAnthropicBlock(normalized["image_url"]) == nil {
		p.add(path, "remote_or_invalid_image", "Bedrock OpenAI projection cannot fetch remote image URLs")
	}
}

type assistantToolCallProjection struct {
	ID   string
	Path string
}

func analyzeAssistantToolCallsProjection(p *openAIProjection, path string, raw json.RawMessage) []assistantToolCallProjection {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return nil
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		p.add(path, "invalid_tool_calls", "tool_calls must be an array")
		return nil
	}
	projected := make([]assistantToolCallProjection, 0, len(calls))
	for i, call := range calls {
		callPath := fmt.Sprintf("%s[%d]", path, i)
		projectionUnknownFields(p, callPath, call, map[string]struct{}{"id": {}, "type": {}, "function": {}})
		var callID string
		if json.Unmarshal(call["id"], &callID) != nil || strings.TrimSpace(callID) == "" {
			p.add(joinJSONPath(callPath, "id"), "missing_tool_call_id", "assistant tool calls require a non-empty id")
		} else {
			callID = strings.TrimSpace(callID)
			projected = append(projected, assistantToolCallProjection{ID: callID, Path: callPath})
		}
		var typ string
		_ = json.Unmarshal(call["type"], &typ)
		if typ != "" && typ != "function" {
			p.add(joinJSONPath(callPath, "type"), "unsupported_tool_call_type", fmt.Sprintf("tool call type %q is unsupported", typ))
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(call["function"], &fn); err != nil {
			p.add(joinJSONPath(callPath, "function"), "invalid_function_call", "function must be an object")
			continue
		}
		projectionUnknownFields(p, joinJSONPath(callPath, "function"), fn, map[string]struct{}{"name": {}, "arguments": {}})
		var name string
		if json.Unmarshal(fn["name"], &name) != nil || strings.TrimSpace(name) == "" {
			p.add(joinJSONPath(callPath, "function.name"), "missing_tool_name", "function name must be non-empty")
		}
		var arguments string
		if json.Unmarshal(fn["arguments"], &arguments) != nil {
			p.add(joinJSONPath(callPath, "function.arguments"), "invalid_tool_arguments", "function arguments must be a JSON string")
			continue
		}
		if strings.TrimSpace(arguments) == "" {
			p.add(joinJSONPath(callPath, "function.arguments"), "blank_tool_arguments", "blank function arguments would be repaired to an empty object by translated transports")
			continue
		}
		var parsed interface{}
		if json.Unmarshal([]byte(arguments), &parsed) != nil {
			p.add(joinJSONPath(callPath, "function.arguments"), "invalid_tool_arguments", "function arguments must contain valid JSON")
		} else if _, ok := parsed.(map[string]interface{}); !ok {
			p.add(joinJSONPath(callPath, "function.arguments"), "tool_argument_shape_loss", "translated tool input requires a JSON object")
		}
	}
	return projected
}

func analyzeKiroToolChoiceProjection(p *openAIProjection, raw json.RawMessage) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var value string
	if json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), "auto") {
		return
	}
	p.add("tool_choice", "tool_choice_loss", "OpenAI Chat tool_choice is not preserved by the Kiro adapter")
}

func analyzeBedrockToolChoiceProjection(p *openAIProjection, raw json.RawMessage, specificToolChoiceSupported bool, declaredTools map[string]struct{}) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "auto":
			return
		case "required":
			if len(declaredTools) == 0 {
				p.add("tool_choice", "tool_choice_without_tools", "tool_choice=required has no declared function tools")
			}
			return
		case "none":
			p.add("tool_choice", "tool_choice_none_loss", "tool_choice=none would otherwise fall back to auto")
			return
		default:
			p.add("tool_choice", "unsupported_tool_choice", fmt.Sprintf("tool_choice %q is unsupported by the Bedrock projection", value))
			return
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil || strings.TrimSpace(obj.Function.Name) == "" || !strings.EqualFold(strings.TrimSpace(obj.Type), "function") {
		p.add("tool_choice", "unsupported_tool_choice", "specific tool_choice shape is not representable")
		return
	}
	name := strings.TrimSpace(obj.Function.Name)
	if _, ok := declaredTools[name]; !ok {
		p.add("tool_choice", "undeclared_forced_tool", fmt.Sprintf("forced tool %q is not present in tools", name))
	}
	if !specificToolChoiceSupported {
		p.add("tool_choice", "model_tool_choice_loss", "the resolved Bedrock model does not support forcing a specific tool")
	}
}

func declaredOpenAIFunctionToolNames(raw json.RawMessage) map[string]struct{} {
	names := map[string]struct{}{}
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return names
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return names
	}
	for _, tool := range tools {
		var typ string
		_ = json.Unmarshal(tool["type"], &typ)
		if typ != "function" {
			continue
		}
		var fn map[string]json.RawMessage
		if json.Unmarshal(tool["function"], &fn) != nil {
			continue
		}
		var name string
		_ = json.Unmarshal(fn["name"], &name)
		name = strings.TrimSpace(name)
		if name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

func analyzeBedrockSamplingProjection(p *openAIProjection, root map[string]json.RawMessage, field string) {
	raw, ok := root[field]
	if !ok || jsonRawIsNull(raw) {
		return
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil {
		p.add(field, "invalid_sampling_value", "sampling value must be numeric")
		return
	}
	if value < 0 || value > 1 {
		p.add(field, "sampling_domain_loss", "Bedrock translated transport accepts only values in [0,1]; value will not be clamped")
	}
}

func analyzeBedrockTokenLimitProjection(p *openAIProjection, root map[string]json.RawMessage, field string) {
	raw, ok := root[field]
	if !ok || jsonRawIsNull(raw) {
		return
	}
	var value int
	if json.Unmarshal(raw, &value) != nil || value <= 0 {
		p.add(field, "token_limit_loss", "explicit token limit must be a positive integer")
	}
}

func analyzeBedrockStopProjection(p *openAIProjection, raw json.RawMessage) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			p.add("stop", "stop_sequence_loss", "empty stop sequence would be dropped")
		}
		return
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		p.add("stop", "invalid_stop", "stop must be a string or array of strings")
		return
	}
	for i, value := range many {
		if value == "" {
			p.add(fmt.Sprintf("stop[%d]", i), "stop_sequence_loss", "empty stop sequence would be dropped")
		}
	}
}

// analyzeOpenAIToolsProjection checks exactly the mutations performed by the
// existing tool converters. Kiro's sanitizer may remove schema keywords or
// synthesize descriptions/types to satisfy upstream validation; those operations
// remain useful, but a request that needs them is not a lossless Kiro candidate.
func analyzeOpenAIToolsProjection(p *openAIProjection, raw json.RawMessage, path string, kiro, allowFlat bool) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		p.add(path, "invalid_tools", "tools must be an array")
		return
	}
	for i, tool := range tools {
		toolPath := fmt.Sprintf("%s[%d]", path, i)
		var typ string
		_ = json.Unmarshal(tool["type"], &typ)
		if typ != "function" {
			p.add(joinJSONPath(toolPath, "type"), "unsupported_tool_type", fmt.Sprintf("tool type %q requires transparent passthrough", typ))
			continue
		}

		fnRaw := tool["function"]
		var fn map[string]json.RawMessage
		fnPath := joinJSONPath(toolPath, "function")
		if len(fnRaw) > 0 && !jsonRawIsNull(fnRaw) {
			projectionUnknownFields(p, toolPath, tool, map[string]struct{}{"type": {}, "function": {}})
			if err := json.Unmarshal(fnRaw, &fn); err != nil {
				p.add(fnPath, "invalid_function_tool", "function must be an object")
				continue
			}
		} else {
			if !allowFlat {
				p.add(joinJSONPath(toolPath, "function"), "missing_function_tool_body", "Chat Completions function tools require a nested function object")
				continue
			}
			// Responses function tools use a flat shape. The Responses semantic
			// analyzer explicitly opts into this form.
			fn = tool
			fnPath = toolPath
			projectionUnknownFields(p, toolPath, tool, map[string]struct{}{"type": {}, "name": {}, "description": {}, "parameters": {}})
		}
		if fnPath != toolPath {
			projectionUnknownFields(p, fnPath, fn, map[string]struct{}{"name": {}, "description": {}, "parameters": {}})
		}

		var name string
		if json.Unmarshal(fn["name"], &name) != nil || strings.TrimSpace(name) == "" {
			p.add(joinJSONPath(fnPath, "name"), "missing_tool_name", "function tool name must be non-empty")
		}

		if kiro {
			var description string
			descRaw, hasDescription := fn["description"]
			if !hasDescription || jsonRawIsNull(descRaw) || json.Unmarshal(descRaw, &description) != nil || strings.TrimSpace(description) == "" {
				p.add(joinJSONPath(fnPath, "description"), "description_synthesis", "Kiro converter would synthesize a tool description")
			} else if len(description) > maxToolDescLen {
				p.add(joinJSONPath(fnPath, "description"), "description_truncation", "Kiro converter would truncate the tool description")
			}
		}

		paramsRaw, hasParams := fn["parameters"]
		if !hasParams || jsonRawIsNull(paramsRaw) {
			p.add(joinJSONPath(fnPath, "parameters"), "schema_synthesis", "translated transport would synthesize an object schema")
			continue
		}
		var schema interface{}
		if json.Unmarshal(paramsRaw, &schema) != nil {
			p.add(joinJSONPath(fnPath, "parameters"), "invalid_schema", "parameters must be valid JSON Schema")
			continue
		}
		schemaObj, ok := schema.(map[string]interface{})
		if !ok {
			p.add(joinJSONPath(fnPath, "parameters"), "schema_shape_loss", "parameters must be a JSON object")
			continue
		}
		if kiro {
			analyzeKiroSchemaMutation(p, joinJSONPath(fnPath, "parameters"), schemaObj, true)
		}
	}
}

func analyzeKiroSchemaMutation(p *openAIProjection, path string, schema map[string]interface{}, root bool) {
	if root {
		typ, ok := schema["type"].(string)
		if !ok || typ != "object" {
			p.add(joinJSONPath(path, "type"), "schema_type_synthesis", "Kiro converter would synthesize type=object")
		}
	}
	if value, ok := schema["additionalProperties"]; ok {
		if b, ok := value.(bool); !ok || !b {
			p.add(joinJSONPath(path, "additionalProperties"), "schema_keyword_loss", "Kiro sanitizer removes behavior-bearing additionalProperties")
		}
	}
	if _, ok := schema["encrypted"]; ok {
		p.emulate(joinJSONPath(path, "encrypted"), "client_schema_encrypted_keyword_removed", "Kiro schema sanitizer removes the client-only encrypted keyword")
	}
	if required, ok := schema["required"]; ok {
		if arr, ok := required.([]interface{}); !ok {
			p.add(joinJSONPath(path, "required"), "schema_required_loss", "Kiro sanitizer would remove malformed required")
		} else {
			for _, item := range arr {
				if _, ok := item.(string); !ok {
					p.add(joinJSONPath(path, "required"), "schema_required_loss", "Kiro sanitizer would remove non-string required entries")
					break
				}
			}
		}
	}
	for key, value := range schema {
		switch child := value.(type) {
		case map[string]interface{}:
			analyzeKiroSchemaMutation(p, joinJSONPath(path, key), child, false)
		case []interface{}:
			for i, item := range child {
				if object, ok := item.(map[string]interface{}); ok {
					analyzeKiroSchemaMutation(p, fmt.Sprintf("%s.%s[%d]", path, key, i), object, false)
				}
			}
		}
	}
}
