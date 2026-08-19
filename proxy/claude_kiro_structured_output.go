package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// claudeKiroStructuredOutputFormat extracts output_config.format exactly as the
// client sent it. JSON structured output is a generation constraint, not
// advisory metadata, so Kiro may only accept it when the live model schema
// advertises the same additionalModelRequestFields field.
func claudeKiroStructuredOutputFormat(req *ClaudeRequest) (json.RawMessage, bool) {
	if req == nil || req.OutputConfig == nil || req.OutputConfig.Extra == nil {
		return nil, false
	}

	raw, ok := req.OutputConfig.Extra["format"]
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}
	return append(json.RawMessage(nil), trimmed...), true
}

func modelAdvertisesKiroStructuredOutputFormat(model ModelInfo) (bool, error) {
	root, err := decodeModelRequestSchema(model.AdditionalModelRequestFieldsSchema)
	if err != nil {
		return false, err
	}
	if root == nil {
		return false, nil
	}

	root = unwrapAdditionalFieldsSchema(root)
	properties := schemaProperties(root)
	if properties == nil {
		return false, nil
	}
	outputConfig := schemaObject(properties["output_config"])
	if outputConfig == nil {
		return false, nil
	}
	outputProperties := schemaProperties(outputConfig)
	return outputProperties != nil && outputProperties["format"] != nil, nil
}

func mergeKiroOutputConfigField(
	fields map[string]interface{},
	key string,
	value interface{},
) map[string]interface{} {
	if fields == nil {
		fields = make(map[string]interface{})
	}

	outputConfig, _ := fields["output_config"].(map[string]interface{})
	if outputConfig == nil {
		outputConfig = make(map[string]interface{})
		fields["output_config"] = outputConfig
	}
	outputConfig[key] = value
	return fields
}

// projectClaudeStructuredOutputToKiro performs only the lossless request-shape
// projection. Whether Kiro accepts output_config.format is candidate-local and
// is checked later against the exact immutable provider route selected for the
// account. Keeping support gating out of this step prevents one Kiro account's
// model snapshot from authorizing another account.
func (h *Handler) projectClaudeStructuredOutputToKiro(
	req *ClaudeRequest,
	modelID string,
	fields map[string]interface{},
) (map[string]interface{}, error) {
	_ = h
	_ = modelID
	rawFormat, requested := claudeKiroStructuredOutputFormat(req)
	if !requested {
		return fields, nil
	}

	var format interface{}
	if err := json.Unmarshal(rawFormat, &format); err != nil {
		return nil, fmt.Errorf("invalid output_config.format: %w", err)
	}

	return mergeKiroOutputConfigField(fields, "format", format), nil
}
