package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// responsesSemanticRequirements keeps Responses protocol semantics separate from
// the Chat-message representation used by translated transports. In particular,
// top-level instructions remain privileged current-request instructions rather
// than being reinterpreted as a historical late system message.
type responsesSemanticRequirements struct {
	KiroProjection openAIProjection
}

func (r responsesSemanticRequirements) KiroSupported() bool {
	return r.KiroProjection.Supported()
}

func (r responsesSemanticRequirements) KiroExact() bool {
	return r.KiroProjection.Exact()
}

func (r responsesSemanticRequirements) KiroEmulated() bool {
	return r.KiroProjection.Emulated()
}

func (r responsesSemanticRequirements) Reason() string {
	return r.KiroProjection.Reason()
}

var responsesKiroTopLevelFields = map[string]struct{}{
	"model": {}, "input": {}, "instructions": {}, "stream": {},
	"tools": {}, "tool_choice": {}, "parallel_tool_calls": {},
	"previous_response_id": {}, "store": {}, "temperature": {}, "top_p": {},
	"max_output_tokens": {}, "reasoning": {}, "thinking": {},
	"output_config": {}, "metadata": {}, "truncation": {},
}

func analyzeResponsesSemantics(raw []byte) responsesSemanticRequirements {
	var projection openAIProjection

	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		projection.add("", "invalid_json", "invalid Responses JSON")
		return responsesSemanticRequirements{KiroProjection: projection}
	}

	projectionUnknownFields(&projection, "", root, responsesKiroTopLevelFields)

	if instructionsRaw, ok := root["instructions"]; ok && !jsonRawIsNull(instructionsRaw) {
		var instructions string
		if err := json.Unmarshal(instructionsRaw, &instructions); err != nil {
			projection.add("instructions", "unsupported_instructions_shape", "Responses instructions must be a string for the Kiro adapter")
		} else if strings.TrimSpace(instructions) != "" {
			projection.emulate("instructions", "system_priming_emulation", "Kiro represents current privileged Responses instructions as a synthetic conversation priming pair")
		}
	}

	if truncationRaw, ok := root["truncation"]; ok && !jsonRawIsNull(truncationRaw) {
		var truncation string
		if err := json.Unmarshal(truncationRaw, &truncation); err != nil {
			projection.add("truncation", "invalid_truncation", "truncation must be a string")
		} else {
			switch strings.ToLower(strings.TrimSpace(truncation)) {
			case "disabled":
				// Matches the Responses default. The Kiro projector still must reject
				// the request later if its actual payload would require truncation.
			case "auto":
				projection.add("truncation", "responses_auto_truncation_passthrough", "Responses truncation=auto requires transparent passthrough in this phase")
			default:
				projection.add("truncation", "unsupported_truncation", fmt.Sprintf("Responses truncation mode %q is not supported by the Kiro adapter", truncation))
			}
		}
	}

	if reasoningRaw, ok := root["reasoning"]; ok && !jsonRawIsNull(reasoningRaw) {
		mergeResponsesObjectProjection(&projection, reasoningRaw, "reasoning", map[string]struct{}{"effort": {}})
	}

	if thinkingRaw, ok := root["thinking"]; ok && !jsonRawIsNull(thinkingRaw) {
		mergeResponsesObjectProjection(&projection, thinkingRaw, "thinking", map[string]struct{}{
			"type": {}, "budget_tokens": {}, "display": {},
		})
	}

	if outputConfigRaw, ok := root["output_config"]; ok && !jsonRawIsNull(outputConfigRaw) {
		mergeResponsesObjectProjection(&projection, outputConfigRaw, "output_config", map[string]struct{}{"effort": {}})
	}

	if toolsRaw, ok := root["tools"]; ok && !jsonRawIsNull(toolsRaw) {
		analyzeResponsesToolArrayProjection(&projection, toolsRaw, "tools", "", "")
	}

	if inputRaw, ok := root["input"]; ok && !jsonRawIsNull(inputRaw) {
		projection.merge(analyzeResponsesInputProjection(inputRaw))
	}

	return responsesSemanticRequirements{KiroProjection: projection}
}

func mergeResponsesObjectProjection(p *openAIProjection, raw json.RawMessage, field string, allowed map[string]struct{}) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		p.add(field, "unsupported_object_shape", field+" shape requires transparent passthrough")
		return
	}
	projectionUnknownFields(p, field, values, allowed)
}

func unknownJSONFields(values map[string]json.RawMessage, allowed map[string]struct{}) []string {
	unknown := make([]string, 0)
	for key := range values {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func analyzeResponsesInputProjection(raw json.RawMessage) openAIProjection {
	var projection openAIProjection
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || strings.HasPrefix(trimmed, `"`) {
		return projection
	}

	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			projection.add("input", "unsupported_input_shape", "input shape requires transparent passthrough")
			return projection
		}
	case '{':
		items = []json.RawMessage{raw}
	default:
		projection.add("input", "unsupported_input_shape", "input shape requires transparent passthrough")
		return projection
	}

	seenNonSystemTurn := false
	for i, itemRaw := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(itemRaw, &item); err != nil {
			projection.add(fmt.Sprintf("input[%d]", i), "unsupported_input_item", "input item requires transparent passthrough")
			continue
		}
		var typ, role string
		_ = json.Unmarshal(item["type"], &typ)
		_ = json.Unmarshal(item["role"], &role)
		path := fmt.Sprintf("input[%d]", i)
		normalizedRole := strings.ToLower(strings.TrimSpace(role))

		switch {
		case typ == "message" || (typ == "" && role != ""):
			if normalizedRole == "system" {
				if seenNonSystemTurn {
					projection.add(path+".role", "instruction_order_loss", "system message after a non-system turn requires transparent passthrough")
				} else {
					projection.emulate(path+".role", "system_priming_emulation", "Kiro represents privileged system input as a synthetic conversation priming pair")
				}
			} else {
				seenNonSystemTurn = true
			}
			analyzeResponsesMessageProjection(&projection, path, item, role)

		case typ == "function_call_output" || typ == "tool_result":
			seenNonSystemTurn = true
			projectionUnknownFields(&projection, path, item, map[string]struct{}{
				"type": {}, "call_id": {}, "tool_call_id": {}, "output": {}, "content": {}, "id": {}, "status": {},
			})
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			if strings.TrimSpace(callID) == "" {
				_ = json.Unmarshal(item["tool_call_id"], &callID)
			}
			callID = strings.TrimSpace(callID)
			if callID == "" {
				projection.add(path+".call_id", "missing_tool_call_id", "function/tool output requires call_id")
			}
			output := item["output"]
			if len(output) == 0 || jsonRawIsNull(output) {
				output = item["content"]
			}
			analyzeResponsesToolOutputProjection(&projection, path+".output", output, false)

		case typ == "custom_tool_call_output":
			seenNonSystemTurn = true
			projectionUnknownFields(&projection, path, item, map[string]struct{}{
				"type": {}, "call_id": {}, "output": {}, "id": {}, "status": {},
			})
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			if strings.TrimSpace(callID) == "" {
				projection.add(path+".call_id", "missing_tool_call_id", "custom_tool_call_output requires call_id")
			}
			analyzeResponsesToolOutputProjection(&projection, path+".output", item["output"], true)

		case typ == "function_call" || typ == "custom_tool_call":
			seenNonSystemTurn = true
			allowed := map[string]struct{}{
				"type": {}, "call_id": {}, "id": {}, "name": {}, "namespace": {}, "status": {},
			}
			if typ == "custom_tool_call" {
				allowed["input"] = struct{}{}
			} else {
				allowed["arguments"] = struct{}{}
			}
			projectionUnknownFields(&projection, path, item, allowed)
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			callID = strings.TrimSpace(callID)
			if callID == "" {
				projection.add(path+".call_id", "missing_tool_call_id", "function_call call_id is required")
			}
			var name, namespace, arguments string
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["namespace"], &namespace)
			if strings.TrimSpace(name) == "" {
				projection.add(path+".name", "missing_tool_name", typ+" name is required")
			}
			if strings.TrimSpace(namespace) != "" {
				projection.emulate(path+".namespace", "namespace_flattening", "Kiro flattens the namespace into its internal wire name and restores it at the Responses boundary")
			}
			if typ == "custom_tool_call" {
				var input string
				if err := json.Unmarshal(item["input"], &input); err != nil {
					projection.add(path+".input", "invalid_custom_tool_input", "custom_tool_call input must be a string")
				} else {
					projection.emulate(path+".input", "custom_tool_string_schema_emulation", "Kiro carries custom tool input through a fixed {input:string} schema")
				}
				break
			}
			if err := json.Unmarshal(item["arguments"], &arguments); err != nil {
				projection.add(path+".arguments", "invalid_tool_arguments", "function_call arguments require a JSON string")
			} else if strings.TrimSpace(arguments) == "" {
				projection.add(path+".arguments", "blank_tool_arguments", "blank function arguments would be repaired to an empty object by the Kiro adapter")
			} else {
				var args interface{}
				if json.Unmarshal([]byte(arguments), &args) != nil {
					projection.add(path+".arguments", "invalid_tool_arguments", "function_call arguments must contain valid JSON")
				} else if _, ok := args.(map[string]interface{}); !ok {
					projection.add(path+".arguments", "tool_argument_shape_loss", "function_call arguments must contain a JSON object")
				}
			}

		case typ == "input_text" || typ == "text":
			seenNonSystemTurn = true
			projectionUnknownFields(&projection, path, item, map[string]struct{}{"type": {}, "text": {}})

		case typ == "input_image" || typ == "image" || typ == "image_url":
			seenNonSystemTurn = true
			analyzeInlineImageProjection(&projection, path, item, chatProjectionKiro)

		case typ == "output_text":
		case typ == "additional_tools":
			projectionUnknownFields(&projection, path, item, map[string]struct{}{
				"type": {}, "role": {}, "tools": {}, "id": {},
			})
			projection.emulate(path, "responses_lite_additional_tools", "Kiro promotes Responses Lite additional_tools into its request tool catalog")
			if toolsRaw := item["tools"]; len(toolsRaw) > 0 && !jsonRawIsNull(toolsRaw) {
				analyzeResponsesToolArrayProjection(&projection, toolsRaw, path+".tools", "", "")
			} else {
				projection.add(path+".tools", "missing_additional_tools", "additional_tools requires tools")
			}

		case typ == "agent_message":
			seenNonSystemTurn = true
			projectionUnknownFields(&projection, path, item, map[string]struct{}{
				"type": {}, "id": {}, "author": {}, "recipient": {}, "content": {}, "internal_chat_message_metadata_passthrough": {},
			})
			projection.emulate(path, "agent_message_user_emulation", "Kiro renders agent_message as user-visible inter-agent text")
			analyzeResponsesAgentMessageProjection(&projection, path, item["content"])

		default:
			if typ == "" {
				projection.add(path, "unsupported_input_item", "input item has no supported type or role")
			} else {
				projection.add(path+".type", "unsupported_input_type", fmt.Sprintf("input type %q requires transparent passthrough", typ))
			}
		}
	}
	return projection
}

func analyzeResponsesToolOutputProjection(
	projection *openAIProjection,
	path string,
	raw json.RawMessage,
	custom bool,
) {
	if len(raw) == 0 || jsonRawIsNull(raw) {
		return
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		if custom {
			projection.emulate(path, "structured_custom_tool_output", "Kiro serializes structured custom tool output as text")
		} else {
			projection.add(path, "structured_tool_output", "structured function/tool output objects require transparent passthrough")
		}
		return
	}

	projection.emulate(path, "structured_tool_output_items", "Kiro preserves tool-result identity/text and hoists supported inline images to userInputMessage.images")
	for index, item := range items {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "input_text", "text", "output_text":
			projectionUnknownFields(projection, itemPath, item, map[string]struct{}{"type": {}, "text": {}})
		case "input_image", "image", "image_url":
			analyzeInlineImageProjection(projection, itemPath, item, chatProjectionKiro)
		case "encrypted_content":
			projectionUnknownFields(projection, itemPath, item, map[string]struct{}{"type": {}, "encrypted_content": {}})
			var encrypted string
			_ = json.Unmarshal(item["encrypted_content"], &encrypted)
			if responsesEncryptedContentOpaque(encrypted) {
				projection.add(itemPath, "opaque_encrypted_tool_output", "opaque tool-output encrypted_content cannot be safely projected to Kiro")
			} else if strings.TrimSpace(encrypted) != "" {
				projection.emulate(itemPath, "plaintext_encrypted_tool_output", "plaintext-like encrypted_content is projected as tool-result text")
			}
		case "input_audio", "audio", "input_file", "file", "document":
			projection.add(itemPath, "unsupported_tool_output_media", fmt.Sprintf("tool-output content type %q is not representable by the Kiro adapter", typ))
		default:
			projection.emulate(itemPath, "structured_tool_output_item_text_emulation", "unmodeled non-media tool-output content item is serialized as tool-result text")
		}
	}
}

func analyzeResponsesToolArrayProjection(p *openAIProjection, raw json.RawMessage, path, namespace, namespaceDescription string) {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		p.add(path, "invalid_tools", "tools must be an array")
		return
	}
	for index, toolRaw := range tools {
		toolPath := fmt.Sprintf("%s[%d]", path, index)
		var tool map[string]json.RawMessage
		if err := json.Unmarshal(toolRaw, &tool); err != nil || tool == nil {
			p.add(toolPath, "invalid_tool", "tool must be an object")
			continue
		}
		var typ string
		_ = json.Unmarshal(tool["type"], &typ)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "namespace":
			projectionUnknownFields(p, toolPath, tool, map[string]struct{}{
				"type": {}, "name": {}, "description": {}, "tools": {},
			})
			var name, description string
			_ = json.Unmarshal(tool["name"], &name)
			_ = json.Unmarshal(tool["description"], &description)
			if strings.TrimSpace(name) == "" {
				p.add(toolPath+".name", "missing_namespace_name", "namespace tool requires a name")
				continue
			}
			if strings.TrimSpace(namespace) != "" {
				p.add(toolPath, "nested_namespace", "nested tool namespaces require transparent passthrough")
				continue
			}
			p.emulate(toolPath, "namespace_flattening", "Kiro uses a flat internal tool name while Responses preserves namespace identity")
			if strings.TrimSpace(description) != "" {
				p.emulate(toolPath+".description", "namespace_description_flattening", "Kiro prefixes the namespace description into each flattened child tool description")
			}
			analyzeResponsesToolArrayProjection(p, tool["tools"], toolPath+".tools", strings.TrimSpace(name), strings.TrimSpace(description))

		case "function":
			toolForProjection := tool
			if strings.TrimSpace(namespaceDescription) != "" {
				toolForProjection = make(map[string]json.RawMessage, len(tool)+1)
				for key, value := range tool {
					toolForProjection[key] = value
				}
				var childDescription string
				_ = json.Unmarshal(tool["description"], &childDescription)
				flattened := responsesFlattenedToolDescription(namespaceDescription, childDescription)
				toolForProjection["description"], _ = json.Marshal(flattened)
			}
			arrayRaw, _ := json.Marshal([]map[string]json.RawMessage{toolForProjection})
			var toolProjection openAIProjection
			analyzeOpenAIToolsProjection(&toolProjection, arrayRaw, toolPath, true, true)
			p.merge(toolProjection)
			if strings.TrimSpace(namespace) != "" {
				p.emulate(toolPath, "namespace_flattening", "Kiro uses a flat internal tool name while Responses preserves namespace identity")
			}

		case "custom":
			projectionUnknownFields(p, toolPath, tool, map[string]struct{}{
				"type": {}, "name": {}, "description": {}, "format": {}, "allowed_callers": {}, "defer_loading": {},
			})
			var name, description string
			_ = json.Unmarshal(tool["name"], &name)
			_ = json.Unmarshal(tool["description"], &description)
			if strings.TrimSpace(name) == "" {
				p.add(toolPath+".name", "missing_tool_name", "custom tool name must be non-empty")
			}
			flattenedDescription := responsesFlattenedToolDescription(namespaceDescription, description)
			if strings.TrimSpace(flattenedDescription) == "" {
				p.emulate(toolPath+".description", "description_synthesis", "Kiro requires a description, so the adapter synthesizes one for the Responses custom tool")
			} else if len(flattenedDescription) > maxToolDescLen {
				p.add(toolPath+".description", "description_truncation", "flattened namespace/tool description exceeds the Kiro description limit")
			}
			if raw, ok := tool["format"]; ok && !jsonRawIsNull(raw) {
				var format interface{}
				if json.Unmarshal(raw, &format) != nil {
					p.add(toolPath+".format", "invalid_custom_tool_format", "custom tool format must be valid JSON")
				} else {
					p.emulate(toolPath+".format", "custom_tool_format_emulation", "Kiro carries custom tool input as a string but cannot enforce the Responses freeform format")
				}
			}
			if raw, ok := tool["defer_loading"]; ok && !jsonRawIsNull(raw) {
				var deferred bool
				if json.Unmarshal(raw, &deferred) != nil {
					p.add(toolPath+".defer_loading", "invalid_defer_loading", "defer_loading must be a boolean")
				} else if deferred {
					p.add(toolPath+".defer_loading", "deferred_tool_visibility", "defer_loading=true is not representable by the Kiro tool catalog")
				}
			}
			if raw, ok := tool["allowed_callers"]; ok && !jsonRawIsNull(raw) {
				var callers []string
				if json.Unmarshal(raw, &callers) != nil || len(callers) != 0 {
					p.add(toolPath+".allowed_callers", "tool_caller_constraint", "allowed_callers routing semantics require transparent passthrough")
				}
			}
			p.emulate(toolPath, "custom_tool_string_schema_emulation", "Kiro represents a Responses custom tool with a fixed {input:string} schema")
			if strings.TrimSpace(namespace) != "" {
				p.emulate(toolPath, "namespace_flattening", "Kiro uses a flat internal tool name while Responses preserves namespace identity")
			}

		default:
			p.add(toolPath+".type", "unsupported_tool_type", fmt.Sprintf("tool type %q requires transparent passthrough", typ))
		}
	}
}

func analyzeResponsesAgentMessageProjection(p *openAIProjection, path string, raw json.RawMessage) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		p.add(path+".content", "invalid_agent_message", "agent_message content must be an array")
		return
	}
	for index, part := range parts {
		partPath := fmt.Sprintf("%s.content[%d]", path, index)
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		switch typ {
		case "input_text", "text", "output_text":
			var text string
			if json.Unmarshal(part["text"], &text) != nil {
				p.add(partPath+".text", "invalid_agent_message_text", "agent_message text must be a string")
			}
		case "encrypted_content":
			var value string
			if json.Unmarshal(part["encrypted_content"], &value) != nil {
				p.add(partPath+".encrypted_content", "invalid_encrypted_content", "encrypted_content must be a string")
			} else if responsesEncryptedContentOpaque(value) {
				p.add(partPath+".encrypted_content", "opaque_encrypted_content", "opaque agent_message encrypted_content cannot be safely projected to Kiro")
			} else {
				p.emulate(partPath+".encrypted_content", "plaintext_encrypted_content_emulation", "plaintext-like encrypted_content is rendered as text for Kiro")
			}
		default:
			p.add(partPath+".type", "unsupported_agent_message_content", fmt.Sprintf("agent_message content type %q requires transparent passthrough", typ))
		}
	}
}

func analyzeResponsesMessageProjection(p *openAIProjection, path string, item map[string]json.RawMessage, role string) {
	if role == "" {
		_ = json.Unmarshal(item["role"], &role)
	}
	role = strings.ToLower(strings.TrimSpace(role))
	projectionUnknownFields(p, path, item, map[string]struct{}{
		"type": {}, "role": {}, "content": {}, "id": {}, "status": {},
	})

	switch role {
	case "developer":
		p.add(path+".role", "instruction_role_loss", "developer-role precedence requires transparent passthrough")
	case "system", "assistant":
		analyzeTextOnlyContentProjection(p, path+".content", item["content"])
	case "user":
		analyzeUserContentProjection(p, path+".content", item["content"], chatProjectionKiro)
	default:
		p.add(path+".role", "unsupported_role", fmt.Sprintf("role %q requires transparent passthrough", role))
	}
}
