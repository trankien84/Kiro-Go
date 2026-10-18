package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseResponsesInput(raw json.RawMessage) ([]OpenAIMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("invalid input string: %w", err)
		}
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []OpenAIMessage{{Role: "user", Content: s}}, nil
	}

	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("invalid input array: %w", err)
		}
		return convertResponsesInputItems(items)
	}

	if trimmed[0] == '{' {
		return convertResponsesInputItems([]json.RawMessage{raw})
	}

	return nil, fmt.Errorf("unsupported input shape")
}

func convertResponsesInputItems(items []json.RawMessage) ([]OpenAIMessage, error) {
	messages := make([]OpenAIMessage, 0, len(items))
	pendingUserParts := []interface{}{}

	flushPendingUser := func() {
		if len(pendingUserParts) == 0 {
			return
		}
		messages = append(messages, OpenAIMessage{
			Role:    "user",
			Content: pendingUserParts,
		})
		pendingUserParts = nil
	}

	for _, item := range items {
		var obj map[string]interface{}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}

		typ, _ := obj["type"].(string)
		role, _ := obj["role"].(string)

		switch {
		case typ == "additional_tools":
			// Declaration-only Responses Lite input item. The unified tool parser
			// handles it before message projection; it is not conversational history.
			continue

		case typ == "message" || (typ == "" && role != ""):
			flushPendingUser()
			msg := buildMessageFromInputItem(obj, role)
			if msg != nil {
				messages = append(messages, *msg)
			}

		case typ == "function_call_output" || typ == "custom_tool_call_output" || typ == "tool_result":
			flushPendingUser()
			callID, _ := obj["call_id"].(string)
			if callID == "" {
				callID, _ = obj["tool_call_id"].(string)
			}
			out, err := responsesToolOutputContent(obj["output"])
			if err != nil {
				return nil, err
			}
			if responsesToolOutputEmpty(out) {
				out, err = responsesToolOutputContent(obj["content"])
				if err != nil {
					return nil, err
				}
			}
			messages = append(messages, OpenAIMessage{
				Role:       "tool",
				Content:    out,
				ToolCallID: callID,
			})

		case typ == "function_call" || typ == "custom_tool_call":
			flushPendingUser()
			tc := ToolCall{
				ID:   stringField(obj, "call_id", "id"),
				Type: "function",
			}
			tc.Function.Name, _ = obj["name"].(string)
			tc.Namespace, _ = obj["namespace"].(string)
			if typ == "custom_tool_call" {
				tc.ResponsesKind = responsesToolKindCustom
				input, _ := obj["input"].(string)
				encoded, _ := json.Marshal(map[string]interface{}{"input": input})
				tc.Function.Arguments = string(encoded)
			} else {
				tc.ResponsesKind = responsesToolKindFunction
				tc.Function.Arguments = stringifyArbitrary(obj["arguments"])
			}
			// Merge consecutive function_call items into a single assistant
			// message so parallel tool calls stay grouped in one turn. The
			// Responses API emits each parallel call as a separate input item;
			// keeping them in one assistant message preserves the tool_use /
			// tool_result pairing that Kiro requires.
			if n := len(messages); n > 0 &&
				messages[n-1].Role == "assistant" &&
				len(messages[n-1].ToolCalls) > 0 &&
				strings.TrimSpace(extractOpenAIMessageText(messages[n-1].Content)) == "" {
				messages[n-1].ToolCalls = append(messages[n-1].ToolCalls, tc)
			} else {
				messages = append(messages, OpenAIMessage{
					Role:      "assistant",
					Content:   "",
					ToolCalls: []ToolCall{tc},
				})
			}

		case typ == "input_text" || typ == "text":
			text, _ := obj["text"].(string)
			if text != "" {
				pendingUserParts = append(pendingUserParts, map[string]interface{}{
					"type": "input_text",
					"text": text,
				})
			}

		case typ == "input_image", typ == "image", typ == "image_url":
			pendingUserParts = append(pendingUserParts, map[string]interface{}(obj))

		case typ == "output_text":
			flushPendingUser()
			text, _ := obj["text"].(string)
			if text != "" {
				messages = append(messages, OpenAIMessage{Role: "assistant", Content: text})
			}

		case typ == "agent_message":
			flushPendingUser()
			agentText, err := responsesAgentMessageText(obj)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(agentText) != "" {
				messages = append(messages, OpenAIMessage{Role: "user", Content: agentText})
			}

		default:
			if role != "" {
				flushPendingUser()
				msg := buildMessageFromInputItem(obj, role)
				if msg != nil {
					messages = append(messages, *msg)
				}
			}
		}
	}

	flushPendingUser()
	return messages, nil
}

func responsesToolOutputEmpty(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []interface{}:
		return len(typed) == 0
	default:
		return false
	}
}

// responsesToolOutputContent preserves the multimodal Responses output shape
// that OpenAIToKiro already understands. String/object outputs keep the legacy
// text projection, while content-item arrays retain input_image blocks so Kiro
// can hoist inline image bytes to the current user turn. Unknown array items are
// converted to text rather than silently dropped.
func responsesToolOutputContent(value interface{}) (interface{}, error) {
	switch typed := value.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case []interface{}:
		parts := make([]interface{}, 0, len(typed))
		for _, raw := range typed {
			part, ok := raw.(map[string]interface{})
			if !ok {
				if text := stringifyArbitrary(raw); text != "" {
					parts = append(parts, map[string]interface{}{"type": "input_text", "text": text})
				}
				continue
			}
			typ, _ := part["type"].(string)
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "input_text", "text", "output_text", "input_image", "image", "image_url":
				parts = append(parts, part)
			case "encrypted_content":
				encrypted, _ := part["encrypted_content"].(string)
				if encrypted == "" {
					continue
				}
				if responsesEncryptedContentOpaque(encrypted) {
					return nil, fmt.Errorf("tool output encrypted_content is opaque and cannot be safely projected to Kiro")
				}
				parts = append(parts, map[string]interface{}{"type": "input_text", "text": encrypted})
			default:
				if text := stringifyArbitrary(part); text != "" {
					parts = append(parts, map[string]interface{}{"type": "input_text", "text": text})
				}
			}
		}
		return parts, nil
	default:
		return stringifyArbitrary(value), nil
	}
}

func responsesAgentMessageText(obj map[string]interface{}) (string, error) {
	content, _ := obj["content"].([]interface{})
	var out strings.Builder
	for _, value := range content {
		part, _ := value.(map[string]interface{})
		if part == nil {
			continue
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "input_text", "text", "output_text":
			if text, _ := part["text"].(string); text != "" {
				out.WriteString(text)
			}
		case "encrypted_content":
			value, _ := part["encrypted_content"].(string)
			if value == "" {
				continue
			}
			if responsesEncryptedContentOpaque(value) {
				return "", fmt.Errorf("agent_message encrypted_content is opaque and cannot be safely projected to Kiro")
			}
			out.WriteString(value)
		}
	}
	return out.String(), nil
}

func responsesEncryptedContentOpaque(value string) bool {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "ocx1:") || strings.HasPrefix(lower, "fernet:") || strings.HasPrefix(trimmed, "gAAAA") {
		return true
	}
	// Long token-like blobs with no whitespace are safer to treat as opaque.
	if len(trimmed) >= 96 && !strings.ContainsAny(trimmed, " \t\r\n") {
		for _, r := range trimmed {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_+/=:.", r) {
				continue
			}
			return false
		}
		return true
	}
	return false
}

func buildMessageFromInputItem(obj map[string]interface{}, role string) *OpenAIMessage {
	if role == "" {
		role = "user"
	}

	if content, ok := obj["content"]; ok {
		switch v := content.(type) {
		case string:
			return &OpenAIMessage{Role: role, Content: v}
		case []interface{}:
			parts := make([]interface{}, 0, len(v))
			textOnly := strings.Builder{}
			anyNonText := false
			for _, p := range v {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				ptype, _ := part["type"].(string)
				switch ptype {
				case "input_text", "text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "output_text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "input_image", "image", "image_url":
					anyNonText = true
					parts = append(parts, part)
				default:
					if t, ok := part["text"].(string); ok && t != "" {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				}
			}
			if !anyNonText {
				return &OpenAIMessage{Role: role, Content: textOnly.String()}
			}
			return &OpenAIMessage{Role: role, Content: parts}
		case map[string]interface{}:
			return buildMessageFromInputItem(v, role)
		}
	}

	if text, ok := obj["text"].(string); ok && text != "" {
		return &OpenAIMessage{Role: role, Content: text}
	}

	return nil
}

func stringifyArbitrary(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func stringField(obj map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
