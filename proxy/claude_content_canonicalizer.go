package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// canonicalizeClaudeCompatibilityContent rewrites the small set of
// OpenAI-compatible aliases Kiro-Go intentionally accepts into native
// Anthropic Messages content blocks. Every semantic parser and transport sees
// only the canonical representation after this function returns.
func canonicalizeClaudeCompatibilityContent(req *ClaudeRequest) *claudeTransportValidationError {
	if req == nil {
		return nil
	}
	for i := range req.Messages {
		content, validationErr := canonicalizeClaudeContentValue(
			req.Messages[i].Content,
			fmt.Sprintf("messages[%d].content", i),
		)
		if validationErr != nil {
			return validationErr
		}
		req.Messages[i].Content = content
	}
	if req.System != nil {
		content, validationErr := canonicalizeClaudeContentValue(req.System, "system")
		if validationErr != nil {
			return validationErr
		}
		req.System = content
	}
	return nil
}

func canonicalizeClaudeContentValue(
	content interface{},
	path string,
) (interface{}, *claudeTransportValidationError) {
	switch typed := content.(type) {
	case nil, string:
		return content, nil
	case []interface{}:
		out := make([]interface{}, 0, len(typed))
		for index, raw := range typed {
			block, ok := raw.(map[string]interface{})
			if !ok {
				return nil, &claudeTransportValidationError{
					Code:    "invalid_request_error",
					Message: fmt.Sprintf("%s[%d] must be an object", path, index),
				}
			}
			canonical, validationErr := canonicalizeClaudeContentBlock(
				block,
				fmt.Sprintf("%s[%d]", path, index),
			)
			if validationErr != nil {
				return nil, validationErr
			}
			out = append(out, canonical)
		}
		return out, nil
	default:
		// Internal callers and tests may construct []ClaudeContentBlock. Convert
		// through JSON so the canonicalizer has one deterministic representation.
		data, err := json.Marshal(content)
		if err != nil {
			return nil, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: fmt.Sprintf("%s cannot be encoded: %v", path, err),
			}
		}
		var generic interface{}
		if err := json.Unmarshal(data, &generic); err != nil {
			return nil, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: fmt.Sprintf("%s cannot be normalized: %v", path, err),
			}
		}
		// Only the wire shapes accepted above may be re-entered after the JSON
		// round-trip. In particular, a map remains a map after marshal/unmarshal;
		// recursing on it would never make progress and eventually stack overflow.
		switch generic.(type) {
		case nil, string, []interface{}:
			return canonicalizeClaudeContentValue(generic, path)
		default:
			return nil, &claudeTransportValidationError{
				Code:    "invalid_request_error",
				Message: path + " must be a string or an array of content blocks",
			}
		}
	}
}

var knownClaudeContentBlockTypes = map[string]struct{}{
	// Native Anthropic Messages content blocks handled by the semantic catalog
	// or preserved for a terminal provider. Unknown future provider blocks remain
	// opaque and direct-only; only known discriminators are wire-case strict.
	"text": {}, "image": {}, "document": {}, "search_result": {},
	"tool_use": {}, "tool_result": {}, "thinking": {}, "redacted_thinking": {},
	"server_tool_use": {}, "mcp_tool_use": {}, "mcp_tool_result": {},
	// OpenAI-compatible aliases intentionally accepted by this canonicalizer.
	"input_text": {}, "image_url": {}, "input_image": {},
}

func isKnownClaudeContentBlockType(blockType string) bool {
	_, ok := knownClaudeContentBlockTypes[blockType]
	return ok
}

func canonicalizeClaudeContentBlock(
	block map[string]interface{},
	path string,
) (map[string]interface{}, *claudeTransportValidationError) {
	rawBlockType, _ := block["type"].(string)
	trimmedBlockType := strings.TrimSpace(rawBlockType)
	blockType := strings.ToLower(trimmedBlockType)
	if rawBlockType != blockType && isKnownClaudeContentBlockType(blockType) {
		return nil, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + ".type must use the canonical lowercase discriminator " + blockType,
		}
	}
	switch blockType {
	case "input_text":
		if validationErr := rejectClaudeCompatibilityFields(
			block,
			path,
			map[string]struct{}{"type": {}, "text": {}, "cache_control": {}},
		); validationErr != nil {
			return nil, validationErr
		}
		text, ok := block["text"].(string)
		if !ok {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".text must be a string"}
		}
		out := cloneClaudeGenericMap(block)
		out["type"] = "text"
		out["text"] = text
		return out, nil

	case "image_url", "input_image":
		return canonicalizeClaudeImageAlias(block, path)

	case "tool_result":
		out := cloneClaudeGenericMap(block)
		if nested, exists := block["content"]; exists {
			canonical, validationErr := canonicalizeClaudeContentValue(nested, path+".content")
			if validationErr != nil {
				return nil, validationErr
			}
			out["content"] = canonical
		}
		return out, nil
	default:
		return cloneClaudeGenericMap(block), nil
	}
}

func canonicalizeClaudeImageAlias(
	block map[string]interface{},
	path string,
) (map[string]interface{}, *claudeTransportValidationError) {
	allowed := map[string]struct{}{
		"type": {}, "image_url": {}, "url": {}, "data": {}, "file_id": {},
		"source": {}, "media_type": {}, "cache_control": {},
	}
	if validationErr := rejectClaudeCompatibilityFields(block, path, allowed); validationErr != nil {
		return nil, validationErr
	}

	type imageInput struct {
		kind      string
		value     string
		mediaType string
	}
	var candidates []imageInput

	if raw, exists := block["source"]; exists {
		rawSource, ok := raw.(map[string]interface{})
		if !ok {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source must be an object"}
		}
		sourceType, _ := rawSource["type"].(string)
		sourceType = strings.ToLower(strings.TrimSpace(sourceType))
		var allowedSource map[string]struct{}
		var value string
		var mediaType string
		switch sourceType {
		case "url":
			allowedSource = map[string]struct{}{"type": {}, "url": {}}
			value, _ = rawSource["url"].(string)
		case "base64":
			allowedSource = map[string]struct{}{"type": {}, "media_type": {}, "data": {}}
			value, _ = rawSource["data"].(string)
			mediaType, _ = rawSource["media_type"].(string)
		case "file":
			allowedSource = map[string]struct{}{"type": {}, "file_id": {}}
			value, _ = rawSource["file_id"].(string)
		default:
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".source.type must be url, base64, or file"}
		}
		if validationErr := rejectClaudeCompatibilityFields(rawSource, path+".source", allowedSource); validationErr != nil {
			return nil, validationErr
		}
		candidates = append(candidates, imageInput{kind: sourceType, value: value, mediaType: mediaType})
	}

	if raw, exists := block["image_url"]; exists {
		var value string
		switch typed := raw.(type) {
		case string:
			value = typed
		case map[string]interface{}:
			if validationErr := rejectClaudeCompatibilityFields(
				typed,
				path+".image_url",
				map[string]struct{}{"url": {}},
			); validationErr != nil {
				return nil, validationErr
			}
			value, _ = typed["url"].(string)
		default:
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".image_url must be a string or object"}
		}
		candidates = append(candidates, imageInput{kind: "url_or_data", value: value})
	}
	if raw, exists := block["url"]; exists {
		value, ok := raw.(string)
		if !ok {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".url must be a string"}
		}
		candidates = append(candidates, imageInput{kind: "url_or_data", value: value})
	}
	if raw, exists := block["data"]; exists {
		value, ok := raw.(string)
		if !ok {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".data must be a string"}
		}
		mediaType, _ := block["media_type"].(string)
		candidates = append(candidates, imageInput{kind: "base64", value: value, mediaType: mediaType})
	}
	if raw, exists := block["file_id"]; exists {
		value, ok := raw.(string)
		if !ok {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".file_id must be a string"}
		}
		candidates = append(candidates, imageInput{kind: "file", value: value})
	}

	if len(candidates) != 1 {
		return nil, &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: path + " must specify exactly one image source",
		}
	}
	candidate := candidates[0]
	candidate.value = strings.TrimSpace(candidate.value)
	candidate.mediaType = strings.TrimSpace(candidate.mediaType)
	if candidate.value == "" {
		return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " requires a non-empty image source"}
	}

	source := map[string]interface{}{}
	switch candidate.kind {
	case "file":
		source["type"] = "file"
		source["file_id"] = candidate.value
	case "base64":
		if candidate.mediaType == "" {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " base64 image data requires media_type"}
		}
		if !claudeBase64PayloadValid(candidate.value) {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " contains invalid base64 image data"}
		}
		source["type"] = "base64"
		source["media_type"] = candidate.mediaType
		source["data"] = candidate.value
	case "url", "url_or_data":
		if parsedType, data, ok := parseClaudeDataURL(candidate.value); ok {
			source["type"] = "base64"
			source["media_type"] = parsedType
			source["data"] = data
		} else if strings.HasPrefix(strings.ToLower(candidate.value), "https://") || strings.HasPrefix(strings.ToLower(candidate.value), "http://") {
			source["type"] = "url"
			source["url"] = candidate.value
		} else {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " must contain an HTTP(S) URL or base64 data URL"}
		}
	default:
		return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + " has an unsupported image source"}
	}

	// Top-level media_type belongs only to the top-level data compatibility
	// shape. Keeping it beside source/image_url/file_id would silently discard a
	// client field even when the selected source itself is base64.
	if _, exists := block["media_type"]; exists {
		if _, dataExists := block["data"]; !dataExists {
			return nil, &claudeTransportValidationError{Code: "invalid_request_error", Message: path + ".media_type has no lossless mapping for this image source"}
		}
	}

	out := map[string]interface{}{"type": "image", "source": source}
	if cache, exists := block["cache_control"]; exists {
		out["cache_control"] = cache
	}
	return out, nil
}

func rejectClaudeCompatibilityFields(
	object map[string]interface{},
	path string,
	allowed map[string]struct{},
) *claudeTransportValidationError {
	for key := range object {
		if _, ok := allowed[key]; ok {
			continue
		}
		return &claudeTransportValidationError{
			Code:    "invalid_request_error",
			Message: fmt.Sprintf("%s.%s has no lossless Anthropic Messages mapping", path, key),
		}
	}
	return nil
}

func cloneClaudeGenericMap(source map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func parseClaudeDataURL(value string) (mediaType string, data string, ok bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(value), "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return "", "", false
	}
	header := value[len("data:"):comma]
	parts := strings.Split(header, ";")
	if len(parts) < 2 || !strings.EqualFold(parts[len(parts)-1], "base64") {
		return "", "", false
	}
	mediaType = strings.TrimSpace(parts[0])
	data = strings.TrimSpace(value[comma+1:])
	if mediaType == "" || data == "" {
		return "", "", false
	}
	if !claudeBase64PayloadValid(data) {
		return "", "", false
	}
	return mediaType, data, true
}
