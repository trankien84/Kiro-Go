package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxResponsesHistoryDepth caps how far back we walk the previous_response_id
// chain when expanding history. A referenced chain must be complete: silently
// dropping an unavailable ancestor changes the model context while presenting
// the request as a valid continuation.
const maxResponsesHistoryDepth = 64

// expandPreviousResponseHistory rebuilds the conversation history that led up
// to prev. It walks the previous_response_id chain backwards (oldest -> newest)
// and emits stored inputs and outputs for every ancestor.
//
// Top-level Responses instructions are intentionally NOT copied from ancestors.
// OpenAI's Responses semantics require clients to resend stable instructions on
// each request; a previous response's top-level instructions do not carry over.
func expandPreviousResponseHistory(prev *ResponsesObject) ([]OpenAIMessage, error) {
	messages, _, err := expandPreviousResponseHistoryWithProjection(prev)
	return messages, err
}

// expandPreviousResponseHistoryWithProjection also reports state that the Kiro
// adapter cannot replay structurally. In particular, a Responses reasoning
// item is useful client-visible output but is not equivalent to Kiro's opaque
// reasoning state, so continuation omits it and is explicitly Emulated
// instead of being misclassified as Exact.
func expandPreviousResponseHistoryWithProjection(prev *ResponsesObject) ([]OpenAIMessage, openAIProjection, error) {
	var projection openAIProjection
	if err := responseStateHealthError(); err != nil {
		return nil, projection, err
	}
	if prev == nil {
		return nil, projection, nil
	}

	chain, err := collectAncestorChain(prev)
	if err != nil {
		return nil, projection, err
	}

	messages := make([]OpenAIMessage, 0)
	for _, node := range chain {
		prior, parseErr := parseStoredResponsesInput(node.StoredInput)
		if parseErr != nil {
			return nil, projection, fmt.Errorf("responses state %s has invalid stored input: %w", node.ID, parseErr)
		}
		messages = append(messages, prior...)

		catalogRaw := node.Tools
		if node.StoredStateVersion >= 2 {
			catalogRaw = node.StoredToolCatalog
		}
		catalog, catalogErr := responsesToolCatalogFromStoredRaw(catalogRaw, node.Output, node.StoredStateVersion)
		if catalogErr != nil {
			return nil, projection, fmt.Errorf("responses state %s has invalid stored tools: %w", node.ID, catalogErr)
		}
		turn, outputErr := projectStoredResponsesOutputTurnVersion(
			node.Output,
			catalog,
			&projection,
			fmt.Sprintf("$.previous_response_id[%s].output", node.ID),
			node.StoredStateVersion,
		)
		if outputErr != nil {
			return nil, projection, fmt.Errorf("responses state %s has invalid stored output: %w", node.ID, outputErr)
		}
		messages = append(messages, turn...)
	}

	return messages, projection, nil
}

// collectAncestorChain walks previous_response_id backwards, returning the
// chain in oldest-first order: [root, ..., parent, prev]. Missing ancestors,
// cycles, and over-depth chains are state-integrity errors rather than partial
// history: continuing with truncated state can corrupt agent/tool semantics.
func collectAncestorChain(prev *ResponsesObject) ([]*ResponsesObject, error) {
	if prev == nil {
		return nil, nil
	}

	stack := []*ResponsesObject{prev}
	visited := map[string]bool{}
	if prev.ID != "" {
		visited[prev.ID] = true
	}

	cursor := prev
	for depth := 0; ; depth++ {
		parentID := cursor.PreviousResponseID
		if parentID == "" {
			break
		}
		if depth >= maxResponsesHistoryDepth {
			return nil, fmt.Errorf("responses state chain exceeds maximum depth %d at %s", maxResponsesHistoryDepth, parentID)
		}
		if visited[parentID] {
			return nil, fmt.Errorf("responses state chain contains a cycle at %s", parentID)
		}

		ancestor, err := loadResponse(parentID)
		if err != nil || ancestor == nil {
			if err != nil {
				return nil, fmt.Errorf("responses state ancestor %s is unavailable: %w", parentID, err)
			}
			return nil, fmt.Errorf("responses state ancestor %s is unavailable", parentID)
		}

		visited[parentID] = true
		if ancestor.ID != "" {
			visited[ancestor.ID] = true
		}
		stack = append(stack, ancestor)
		cursor = ancestor
	}

	// Reverse to oldest-first.
	for i, j := 0, len(stack)-1; i < j; i, j = i+1, j-1 {
		stack[i], stack[j] = stack[j], stack[i]
	}
	return stack, nil
}

func outputToMessages(items []ResponseOutputItem) []OpenAIMessage {
	identity := openAIToolNameCatalog{
		OriginalToKiro: make(map[string]string),
		KiroToOriginal: make(map[string]string),
		IdentityToKiro: make(map[string]string),
		KiroToIdentity: make(map[string]responsesToolIdentity),
	}
	for _, item := range items {
		if (item.Type == "function_call" || item.Type == "custom_tool_call") && item.Name != "" {
			identity.OriginalToKiro[item.Name] = item.Name
			identity.KiroToOriginal[item.Name] = item.Name
			id := responsesToolIdentity{Namespace: item.Namespace, Name: item.Name, Kind: responsesToolKindFunction}
			if item.Type == "custom_tool_call" {
				id.Kind = responsesToolKindCustom
			}
			identity.IdentityToKiro[id.key()] = item.Name
			identity.KiroToIdentity[item.Name] = id
		}
	}
	return outputToMessagesWithToolCatalog(items, identity)
}

func responsesToolCatalogFromRaw(raw []byte) openAIToolNameCatalog {
	if len(raw) == 0 {
		return openAIToolNameCatalog{}
	}
	var tools []OpenAITool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return openAIToolNameCatalog{}
	}
	return buildOpenAIToolNameCatalog(tools)
}

func responsesToolCatalogFromStoredRaw(
	raw []byte,
	items []ResponseOutputItem,
	stateVersion int,
) (openAIToolNameCatalog, error) {
	if len(raw) > 0 && strings.TrimSpace(string(raw)) != "null" {
		if stateVersion >= 2 {
			declarations, err := responsesToolDeclarationsFromStored(raw)
			if err != nil {
				return openAIToolNameCatalog{}, fmt.Errorf("decode unified tool catalog: %w", err)
			}
			return buildOpenAIToolNameCatalog(responsesOpenAIToolsFromDeclarations(declarations)), nil
		}
		var tools []OpenAITool
		if err := json.Unmarshal(raw, &tools); err != nil {
			return openAIToolNameCatalog{}, fmt.Errorf("decode tool catalog: %w", err)
		}
		for index, tool := range tools {
			if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" {
				return openAIToolNameCatalog{}, fmt.Errorf("tool[%d] is not a valid persisted function declaration", index)
			}
		}
		return buildOpenAIToolNameCatalog(tools), nil
	}

	identity := openAIToolNameCatalog{
		OriginalToKiro: make(map[string]string),
		KiroToOriginal: make(map[string]string),
		IdentityToKiro: make(map[string]string),
		KiroToIdentity: make(map[string]responsesToolIdentity),
	}
	for _, item := range items {
		if item.Type != "function_call" && item.Type != "custom_tool_call" {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return openAIToolNameCatalog{}, fmt.Errorf("function_call has an empty tool name")
		}
		if stateVersion >= 1 {
			return openAIToolNameCatalog{}, fmt.Errorf("version %d state is missing the tool catalog required by function_call %q", stateVersion, name)
		}
		// Version 0 predates persisted tool catalogs. Preserve exact historical
		// names rather than inventing a new canonicalization after an upgrade.
		identity.OriginalToKiro[name] = name
		identity.KiroToOriginal[name] = name
		id := responsesToolIdentity{Name: name, Namespace: item.Namespace, Kind: responsesToolKindFunction}
		if item.Type == "custom_tool_call" {
			id.Kind = responsesToolKindCustom
		}
		identity.IdentityToKiro[id.key()] = name
		identity.KiroToIdentity[name] = id
	}
	return identity, nil
}

func outputToMessagesWithToolCatalog(items []ResponseOutputItem, catalog openAIToolNameCatalog) []OpenAIMessage {
	return outputToMessagesWithToolCatalogAndProjection(items, catalog, nil, "$.output")
}

func outputToMessagesWithToolCatalogAndProjection(
	items []ResponseOutputItem,
	catalog openAIToolNameCatalog,
	projection *openAIProjection,
	path string,
) []OpenAIMessage {
	messages, _ := projectResponsesOutputTurn(items, catalog, projection, path, false, 0)
	return messages
}

func projectStoredResponsesOutputTurn(
	items []ResponseOutputItem,
	catalog openAIToolNameCatalog,
	projection *openAIProjection,
	path string,
) ([]OpenAIMessage, error) {
	return projectStoredResponsesOutputTurnVersion(items, catalog, projection, path, storedResponseStateVersion)
}

func projectStoredResponsesOutputTurnVersion(
	items []ResponseOutputItem,
	catalog openAIToolNameCatalog,
	projection *openAIProjection,
	path string,
	stateVersion int,
) ([]OpenAIMessage, error) {
	return projectResponsesOutputTurn(items, catalog, projection, path, true, stateVersion)
}

// projectResponsesOutputTurn projects only the output family emitted by the
// local Kiro Responses adapter. A stream may split visible text/reasoning into
// multiple output items as it switches modes; all visible assistant segments
// and function calls still belong to one semantic model turn.
func projectResponsesOutputTurn(
	items []ResponseOutputItem,
	catalog openAIToolNameCatalog,
	projection *openAIProjection,
	path string,
	strict bool,
	stateVersion int,
) ([]OpenAIMessage, error) {
	if len(items) == 0 {
		return nil, nil
	}

	var visible strings.Builder
	toolCalls := make([]ToolCall, 0)
	for index, item := range items {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		switch item.Type {
		case "message":
			role := strings.TrimSpace(item.Role)
			if role == "" {
				role = "assistant"
			}
			if strict && role != "assistant" {
				return nil, fmt.Errorf("%s role %q is not a locally generated assistant output", itemPath, role)
			}
			if strict {
				for contentIndex, part := range item.Content {
					if stateVersion > 0 && part.Type != "output_text" {
						return nil, fmt.Errorf("%s.content[%d] type %q is not a version %d locally generated assistant output", itemPath, contentIndex, part.Type, stateVersion)
					}
					if stateVersion == 0 {
						switch part.Type {
						case "output_text", "text", "input_text":
						default:
							return nil, fmt.Errorf("%s.content[%d] type %q is unsupported in legacy stored output", itemPath, contentIndex, part.Type)
						}
					}
				}
			}
			visible.WriteString(joinTextParts(item.Content))

		case "reasoning":
			if projection != nil {
				projection.emulate(
					itemPath,
					"reasoning_state_not_replayed",
					"stored Responses reasoning item cannot be replayed as structured Kiro reasoning state",
				)
			}

		case "function_call", "custom_tool_call":
			callID := strings.TrimSpace(item.CallID)
			if callID == "" && (!strict || stateVersion == 0) {
				callID = strings.TrimSpace(item.ID)
			}
			name := strings.TrimSpace(item.Name)
			if strict && (callID == "" || name == "") {
				requiredID := "call_id"
				if stateVersion == 0 {
					requiredID = "call_id/id"
				}
				return nil, fmt.Errorf("%s function_call requires %s and name", itemPath, requiredID)
			}
			kind := responsesToolKindFunction
			arguments := strings.TrimSpace(item.Arguments)
			if item.Type == "custom_tool_call" {
				kind = responsesToolKindCustom
				encoded, _ := json.Marshal(map[string]interface{}{"input": item.Input})
				arguments = string(encoded)
			} else if strict {
				var object map[string]interface{}
				if arguments == "" || json.Unmarshal([]byte(arguments), &object) != nil || object == nil {
					return nil, fmt.Errorf("%s function_call arguments must be a JSON object", itemPath)
				}
			}
			kiroName := catalog.kiroNameFor(item.Namespace, name)
			if strict {
				identity := responsesToolIdentity{Namespace: item.Namespace, Name: name}.key()
				if safe, ok := catalog.IdentityToKiro[identity]; ok {
					declared := catalog.KiroToIdentity[safe]
					if stateVersion >= 2 && declared.Kind != "" && declared.Kind != kind {
						return nil, fmt.Errorf("%s %s identity %q conflicts with persisted %s declaration", itemPath, item.Type, (responsesToolIdentity{Namespace: item.Namespace, Name: name}).display(), declared.Kind)
					}
					kiroName = safe
				} else if stateVersion < 2 {
					if _, ok := catalog.KiroToOriginal[name]; !ok {
						return nil, fmt.Errorf("%s function_call name %q has no matching persisted tool declaration", itemPath, name)
					}
					// V1 locally generated output carried the Kiro wire name. Keep
					// accepting that historical representation. V2 persists explicit
					// {namespace,name} identity and must match it exactly.
					kiroName = name
				} else {
					return nil, fmt.Errorf("%s function_call identity %q has no matching persisted tool declaration", itemPath, (responsesToolIdentity{Namespace: item.Namespace, Name: name}).display())
				}
			}
			tc := ToolCall{ID: callID, Type: "function", Namespace: item.Namespace, ResponsesKind: kind}
			if strict && stateVersion >= 2 {
				// V2 inherits the persisted declarations into the current request, so
				// keep the client identity and let the current catalog choose the same
				// Kiro wire name. V1 keeps its historical canonical-name replay.
				tc.Function.Name = name
			} else {
				tc.Function.Name = kiroName
			}
			tc.Function.Arguments = arguments
			toolCalls = append(toolCalls, tc)

		default:
			if strict {
				return nil, fmt.Errorf("%s output type %q is not part of the locally persisted Kiro output family", itemPath, item.Type)
			}
		}
	}

	if visible.Len() == 0 && len(toolCalls) == 0 {
		return nil, nil
	}
	return []OpenAIMessage{{
		Role:      "assistant",
		Content:   visible.String(),
		ToolCalls: toolCalls,
	}}, nil
}

func parseStoredResponsesInput(raw json.RawMessage) ([]OpenAIMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if trimmed[0] == '"' {
		return parseResponsesInput(raw)
	}

	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("decode stored input array: %w", err)
		}
	case '{':
		items = []json.RawMessage{raw}
	default:
		return nil, fmt.Errorf("unsupported stored input shape")
	}
	for index, item := range items {
		if err := validateStoredResponsesInputItem(item); err != nil {
			return nil, fmt.Errorf("item[%d]: %w", index, err)
		}
	}
	return parseResponsesInput(raw)
}

func validateStoredResponsesInputItem(raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		if err == nil {
			err = fmt.Errorf("item is not an object")
		}
		return err
	}
	var typ, role string
	_ = json.Unmarshal(obj["type"], &typ)
	_ = json.Unmarshal(obj["role"], &role)
	knownRole := role == "user" || role == "assistant" || role == "system"

	switch typ {
	case "additional_tools":
		if _, ok := obj["tools"]; !ok {
			return fmt.Errorf("additional_tools requires tools")
		}
		return nil
	case "", "message":
		if !knownRole {
			return fmt.Errorf("message role %q is unsupported in persisted local state", role)
		}
		return validateStoredMessageContent(obj["content"])
	case "function_call", "custom_tool_call":
		if rawStringField(obj, "call_id", "id") == "" || rawStringField(obj, "name") == "" {
			return fmt.Errorf("%s requires call_id/id and name", typ)
		}
		if typ == "custom_tool_call" {
			if _, ok := obj["input"]; !ok {
				return fmt.Errorf("custom_tool_call requires input")
			}
			var input string
			if json.Unmarshal(obj["input"], &input) != nil {
				return fmt.Errorf("custom_tool_call input must be a string")
			}
			return nil
		}
		var arguments string
		if err := json.Unmarshal(obj["arguments"], &arguments); err != nil || strings.TrimSpace(arguments) == "" {
			return fmt.Errorf("function_call arguments must be a JSON string")
		}
		var decoded map[string]interface{}
		if json.Unmarshal([]byte(arguments), &decoded) != nil || decoded == nil {
			return fmt.Errorf("function_call arguments must encode a JSON object")
		}
		return nil
	case "function_call_output", "custom_tool_call_output", "tool_result":
		if rawStringField(obj, "call_id", "tool_call_id") == "" {
			return fmt.Errorf("tool result requires call_id/tool_call_id")
		}
		if _, ok := obj["output"]; !ok {
			if _, ok := obj["content"]; !ok {
				return fmt.Errorf("tool result requires output/content")
			}
		}
		return nil
	case "input_text", "text", "output_text":
		if rawStringField(obj, "text") == "" {
			return fmt.Errorf("%s requires text", typ)
		}
		return nil
	case "input_image", "image", "image_url":
		return nil
	case "agent_message":
		var decoded map[string]interface{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		_, err := responsesAgentMessageText(decoded)
		return err
	default:
		return fmt.Errorf("input type %q is unsupported in persisted local state", typ)
	}
}

func validateStoredMessageContent(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("message content is missing")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return fmt.Errorf("message content must be a string or array: %w", err)
	}
	for index, part := range parts {
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		switch typ {
		case "input_text", "text", "output_text":
			if rawStringField(part, "text") == "" {
				return fmt.Errorf("content[%d] type %q requires text", index, typ)
			}
		case "input_image", "image", "image_url":
		default:
			return fmt.Errorf("content[%d] type %q is unsupported in persisted local state", index, typ)
		}
	}
	return nil
}

func rawStringField(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if json.Unmarshal(obj[key], &value) == nil && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func joinTextParts(parts []ResponseContentPart) string {
	if len(parts) == 0 {
		return ""
	}
	out := ""
	for _, p := range parts {
		if p.Type == "output_text" || p.Type == "text" || p.Type == "input_text" {
			out += p.Text
		}
	}
	return out
}
