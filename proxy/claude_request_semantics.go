package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// claudeBetaSet is an exact, insertion-ordered set. Beta identifiers are opaque
// wire values: do not lowercase, sort, or otherwise rewrite them.
type claudeBetaSet struct {
	ordered []string
	seen    map[string]struct{}
}

func newClaudeBetaSet() claudeBetaSet {
	return claudeBetaSet{seen: make(map[string]struct{})}
}

func (set *claudeBetaSet) AddValues(values []string) {
	if set == nil {
		return
	}
	if set.seen == nil {
		set.seen = make(map[string]struct{})
	}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			token := strings.TrimSpace(part)
			if token == "" {
				continue
			}
			if _, exists := set.seen[token]; exists {
				continue
			}
			set.seen[token] = struct{}{}
			set.ordered = append(set.ordered, token)
		}
	}
}

func (set claudeBetaSet) Contains(token string) bool {
	_, exists := set.seen[strings.TrimSpace(token)]
	return exists
}

func (set claudeBetaSet) Values() []string {
	return append([]string(nil), set.ordered...)
}

// claudeSemanticRequest contains endpoint semantics after normalizing the SDK
// compatibility-only body "betas" field into Anthropic-Beta. The normalized
// body and headers are the single source used by validation and forwarding.
type claudeSemanticRequest struct {
	Headers          http.Header
	Betas            claudeBetaSet
	AnthropicVersion string
	UserProfileID    string
	Body             []byte
}

func claudeCompatibilityBodyBetas(req *ClaudeRequest) ([]string, bool, string) {
	if req == nil || req.Extra == nil {
		return nil, false, ""
	}
	raw, present := req.Extra["betas"]
	if !present {
		return nil, false, ""
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, true, "betas must be an array of non-empty strings"
	}
	var values []string
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, true, "betas must be an array of non-empty strings"
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, true, fmt.Sprintf("betas[%d] must be a non-empty string", index)
		}
	}
	return values, true, ""
}

func claudeBodyWithoutCompatibilityBetas(rawBody []byte) ([]byte, string) {
	if len(bytes.TrimSpace(rawBody)) == 0 {
		return rawBody, ""
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &body); err != nil || body == nil {
		return nil, "Invalid JSON while normalizing beta headers"
	}
	delete(body, "betas")
	normalized, err := json.Marshal(body)
	if err != nil {
		return nil, "Failed to normalize beta headers"
	}
	return normalized, ""
}

func buildClaudeSemanticRequest(
	req *ClaudeRequest,
	source http.Header,
	rawBody []byte,
) (claudeSemanticRequest, string) {
	headers := cloneAnthropicSemanticHeaders(source)
	if headers == nil {
		headers = make(http.Header)
	}
	semantic := claudeSemanticRequest{
		Headers: headers,
		Betas:   newClaudeBetaSet(),
		Body:    rawBody,
	}
	semantic.Betas.AddValues(semantic.Headers.Values("Anthropic-Beta"))
	bodyBetas, present, reason := claudeCompatibilityBodyBetas(req)
	if reason != "" {
		return semantic, reason
	}
	if present {
		semantic.Betas.AddValues(bodyBetas)
		delete(req.Extra, "betas")
		semantic.Body, reason = claudeBodyWithoutCompatibilityBetas(rawBody)
		if reason != "" {
			return semantic, reason
		}
	}

	semantic.Headers.Del("Anthropic-Beta")
	if tokens := semantic.Betas.Values(); len(tokens) > 0 {
		semantic.Headers.Set("Anthropic-Beta", strings.Join(tokens, ","))
	}
	semantic.AnthropicVersion = strings.TrimSpace(
		semantic.Headers.Get("Anthropic-Version"),
	)
	semantic.UserProfileID = strings.TrimSpace(
		semantic.Headers.Get("Anthropic-User-Profile-Id"),
	)
	return semantic, ""
}

// projectClaudeBedrockToolFeatures derives provider-specific beta activation
// from concrete tool semantics instead of blindly forwarding a client beta
// spelling. It also normalizes the documented Bedrock Tool Search wire type.
func projectClaudeBedrockToolFeatures(body map[string]json.RawMessage) (claudeBetaSet, error) {
	betas := newClaudeBetaSet()
	if body == nil {
		return betas, nil
	}
	rawTools, ok := body["tools"]
	if !ok || !claudeRawMeaningful(rawTools) {
		return betas, nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return betas, fmt.Errorf("bedrock: tools must be an array: %w", err)
	}
	changed := false
	for index, fields := range tools {
		var toolType string
		if rawType, exists := fields["type"]; exists && claudeRawMeaningful(rawType) {
			if err := json.Unmarshal(rawType, &toolType); err != nil {
				return betas, fmt.Errorf("bedrock: tools[%d].type must be a string", index)
			}
		}
		if definition, known := completionGuardLookupServerToolDefinition(toolType); known &&
			definition.Feature == completionGuardToolFeatureToolSearch {
			if definition.ExpectedName != "tool_search_tool_regex" {
				return betas, fmt.Errorf("bedrock: tool search variant %q is not documented for InvokeModel", toolType)
			}
			fields["type"], _ = json.Marshal("tool_search_tool_regex")
			betas.AddValues([]string{"tool-search-tool-2025-10-19"})
			changed = true
		}
		if raw, exists := fields["defer_loading"]; exists && claudeRawMeaningful(raw) {
			var deferred bool
			if err := json.Unmarshal(raw, &deferred); err != nil {
				return betas, fmt.Errorf("bedrock: tools[%d].defer_loading must be a boolean", index)
			}
			if deferred {
				betas.AddValues([]string{"tool-search-tool-2025-10-19"})
			}
		}
		if raw, exists := fields["input_examples"]; exists && claudeRawMeaningful(raw) {
			var examples []json.RawMessage
			if err := json.Unmarshal(raw, &examples); err != nil {
				return betas, fmt.Errorf("bedrock: tools[%d].input_examples must be an array", index)
			}
			if len(examples) > 0 {
				betas.AddValues([]string{"tool-examples-2025-10-29"})
			}
		}
	}
	if changed {
		normalized, err := json.Marshal(tools)
		if err != nil {
			return betas, fmt.Errorf("bedrock: encode projected tools: %w", err)
		}
		body["tools"] = normalized
	}
	return betas, nil
}

// buildBedrockBodyWithSemanticHeaders converts normalized Anthropic header
// semantics into the native Bedrock Invoke request body. Keeping this adapter
// outside bedrock.go avoids coupling the generic Bedrock transport to the
// Claude routing analyzer while ensuring selected Invoke requests cannot lose
// beta/version semantics.
func buildBedrockBodyWithSemanticHeaders(
	rawBody []byte,
	semanticHeaders http.Header,
) ([]byte, error) {
	version := strings.TrimSpace(semanticHeaders.Get("Anthropic-Version"))
	if version != "" && version != "2023-06-01" {
		return nil, fmt.Errorf(
			"bedrock: unsupported Anthropic version %q",
			version,
		)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return nil, fmt.Errorf("bedrock: invalid request body: %w", err)
	}
	if body == nil {
		return nil, errors.New("bedrock: request body must be a JSON object")
	}

	betas := newClaudeBetaSet()
	betas.AddValues(semanticHeaders.Values("Anthropic-Beta"))
	if raw, ok := body["anthropic_beta"]; ok && claudeRawMeaningful(raw) {
		var bodyBetas []string
		if err := json.Unmarshal(raw, &bodyBetas); err != nil {
			return nil, errors.New("bedrock: anthropic_beta must be an array of strings")
		}
		for index, beta := range bodyBetas {
			if strings.TrimSpace(beta) == "" {
				return nil, fmt.Errorf(
					"bedrock: anthropic_beta[%d] must be a non-empty string",
					index,
				)
			}
		}
		betas.AddValues(bodyBetas)
	}
	projectedBetas := newClaudeBetaSet()
	for _, beta := range betas.Values() {
		mapped, ok := claudeBedrockBetaProjection(beta)
		if !ok {
			return nil, fmt.Errorf(
				"bedrock: unsupported Anthropic beta %q",
				beta,
			)
		}
		if mapped != "" {
			projectedBetas.AddValues([]string{mapped})
		}
	}
	featureBetas, err := projectClaudeBedrockToolFeatures(body)
	if err != nil {
		return nil, err
	}
	projectedBetas.AddValues(featureBetas.Values())

	delete(body, "model")
	delete(body, "stream")
	delete(body, "betas")
	body["anthropic_version"], _ = json.Marshal(bedrockAnthropicVersion)
	if values := projectedBetas.Values(); len(values) > 0 {
		body["anthropic_beta"], _ = json.Marshal(values)
	} else {
		delete(body, "anthropic_beta")
	}

	normalized, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: encode request body: %w", err)
	}
	return normalized, nil
}

func applyClaudeSemanticHeaders(destination http.Header, semantic http.Header) {
	if destination == nil {
		return
	}
	for _, key := range []string{
		"Anthropic-Beta",
		"Anthropic-Version",
		"Anthropic-User-Profile-Id",
	} {
		destination.Del(key)
		for _, value := range semantic.Values(key) {
			destination.Add(key, value)
		}
	}
}
