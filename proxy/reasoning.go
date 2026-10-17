package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"kiro-go/logger"
	"sort"
	"strings"
)

type ReasoningSchemaPath string

const (
	ReasoningSchemaNone         ReasoningSchemaPath = ""
	ReasoningSchemaOutputConfig ReasoningSchemaPath = "output_config"
	ReasoningSchemaReasoning    ReasoningSchemaPath = "reasoning"
)

type ModelReasoningCapability struct {
	ModelID string

	SupportsThinking            bool
	ThinkingTypes               []string
	SupportsDisplay             bool
	ThinkingDisplays            []string
	SupportsBudgetTokens        bool
	SupportsInterleavedThinking bool
	DefaultThinkingType         string
	DefaultThinkingDisplay      string
	EffortPath                  ReasoningSchemaPath
	Efforts                     []string
	SchemaParseError            string
}

func (c ModelReasoningCapability) SupportsNativeReasoning() bool {
	return c.SupportsThinking || c.EffortPath != ReasoningSchemaNone
}

// Parse thinking and effort support from the model schema.
func ParseModelReasoningCapability(model ModelInfo) ModelReasoningCapability {
	capability := ModelReasoningCapability{
		ModelID: model.ModelId,
	}

	root, err := decodeModelRequestSchema(
		model.AdditionalModelRequestFieldsSchema,
	)
	if err != nil {
		capability.SchemaParseError = err.Error()
		return capability
	}
	if root == nil {
		return capability
	}

	root = unwrapAdditionalFieldsSchema(root)
	properties := schemaProperties(root)
	if properties == nil {
		return capability
	}

	if thinkingSchema := schemaObject(properties["thinking"]); thinkingSchema != nil {
		capability.SupportsThinking = true

		thinkingProps := schemaProperties(thinkingSchema)
		thinkingTypeSchema := schemaObject(thinkingProps["type"])
		displaySchema := schemaObject(thinkingProps["display"])

		capability.ThinkingTypes = enumStrings(thinkingTypeSchema)
		capability.SupportsDisplay = displaySchema != nil
		capability.ThinkingDisplays = enumStrings(displaySchema)

		if defaultThinking := schemaObject(thinkingSchema["default"]); defaultThinking != nil {
			capability.DefaultThinkingType = stringValue(defaultThinking["type"])
			capability.DefaultThinkingDisplay = stringValue(defaultThinking["display"])
		}
		if capability.DefaultThinkingType == "" {
			capability.DefaultThinkingType = schemaDefaultString(thinkingTypeSchema)
		}
		if capability.DefaultThinkingDisplay == "" {
			capability.DefaultThinkingDisplay = schemaDefaultString(displaySchema)
		}
		capability.SupportsBudgetTokens =
			thinkingProps != nil && thinkingProps["budget_tokens"] != nil
	}

	if outputConfigSchema := schemaObject(properties["output_config"]); outputConfigSchema != nil {
		outputProps := schemaProperties(outputConfigSchema)
		efforts := enumStrings(schemaObject(outputProps["effort"]))
		if len(efforts) > 0 {
			capability.EffortPath = ReasoningSchemaOutputConfig
			capability.Efforts = normalizeEffortList(efforts)
			return capability
		}
	}

	if reasoningSchema := schemaObject(properties["reasoning"]); reasoningSchema != nil {
		reasoningProps := schemaProperties(reasoningSchema)
		efforts := enumStrings(schemaObject(reasoningProps["effort"]))
		if len(efforts) > 0 {
			capability.EffortPath = ReasoningSchemaReasoning
			capability.Efforts = normalizeEffortList(efforts)
		}
	}

	return capability
}

func decodeModelRequestSchema(raw json.RawMessage) (map[string]interface{}, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, nil
	}

	for depth := 0; depth < 3; depth++ {
		if len(data) == 0 {
			return nil, nil
		}

		if data[0] == '"' {
			var encoded string
			if err := json.Unmarshal(data, &encoded); err != nil {
				return nil, fmt.Errorf("decode schema string: %w", err)
			}
			data = []byte(encoded)
			continue
		}

		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("decode schema object: %w", err)
		}
		return result, nil
	}

	return nil, fmt.Errorf("schema nesting is too deep")
}

func unwrapAdditionalFieldsSchema(
	root map[string]interface{},
) map[string]interface{} {
	for _, key := range []string{"schema", "jsonSchema"} {
		if nested := schemaObject(root[key]); nested != nil {
			root = nested
		}
	}

	properties := schemaProperties(root)
	if properties == nil {
		return root
	}

	if nested := schemaObject(properties["additionalModelRequestFields"]); nested != nil {
		return nested
	}

	return root
}

func schemaProperties(
	schema map[string]interface{},
) map[string]interface{} {
	if schema == nil {
		return nil
	}

	properties, _ := schema["properties"].(map[string]interface{})
	return properties
}

func schemaObject(value interface{}) map[string]interface{} {
	object, _ := value.(map[string]interface{})
	return object
}

func stringValue(value interface{}) string {
	text, _ := value.(string)
	return strings.ToLower(strings.TrimSpace(text))
}

func schemaDefaultString(schema map[string]interface{}) string {
	if schema == nil {
		return ""
	}
	return stringValue(schema["default"])
}

func enumStrings(schema map[string]interface{}) []string {
	if schema == nil {
		return nil
	}

	if constant, ok := schema["const"].(string); ok {
		return []string{constant}
	}

	rawValues, ok := schema["enum"].([]interface{})
	if !ok {
		return nil
	}

	values := make([]string, 0, len(rawValues))
	for _, rawValue := range rawValues {
		value, ok := rawValue.(string)
		if !ok {
			continue
		}

		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}

	return values
}

var canonicalEffortOrder = []string{
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
	"max",
}

func canonicalEffort(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))

	switch value {
	case "extra_high", "extra-high":
		return "xhigh"
	default:
		return value
	}
}

func normalizedClaudeModelID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.ReplaceAll(value, "_", "-")
}

func modelHasAnyPrefix(modelID string, prefixes ...string) bool {
	modelID = normalizedClaudeModelID(modelID)
	for _, prefix := range prefixes {
		if strings.HasPrefix(modelID, prefix) {
			return true
		}
	}
	return false
}

func modelCapsDisabledThinkingAtHigh(modelID string) bool {
	// Opus 5.5 cannot disable thinking at *any* effort. This restriction
	// belongs only to Opus 5.0, not a broad 5.x prefix.
	family, version, ok := canonicalClaudeModelFamilyVersion(modelID)
	return ok && (family == "opus" && version == "5" || family == "haiku" && version == "5.5")
}

func validateThinkingEffortCombination(
	modelID string,
	thinkingDisabled bool,
	effort string,
) error {
	if !thinkingDisabled || !modelCapsDisabledThinkingAtHigh(modelID) {
		return nil
	}

	effort = canonicalEffort(effort)
	switch effort {
	case "xhigh", "max":
		return fmt.Errorf(
			"model %s does not support thinking.type disabled with effort %q; use high or lower, or enable thinking",
			modelID,
			effort,
		)
	default:
		return nil
	}
}

func claudeThinkingEnabledByDefault(
	modelID string,
	capability ModelReasoningCapability,
) bool {
	switch strings.ToLower(strings.TrimSpace(capability.DefaultThinkingType)) {
	case "adaptive", "enabled":
		return true
	case "disabled", "none":
		return false
	}

	// Current Claude models whose API contract enables adaptive thinking when
	// the thinking field is omitted. Keep older Claude models opt-in.
	return modelHasAnyPrefix(
		modelID,
		"claude-opus-5",
		"claude-sonnet-5",
		"claude-haiku-5-5",
		"claude-fable-5",
		"claude-mythos-5",
	)
}

func claudeDefaultThinkingDisplay(
	modelID string,
	capability ModelReasoningCapability,
) string {
	if display := strings.ToLower(strings.TrimSpace(capability.DefaultThinkingDisplay)); display != "" {
		return display
	}

	// These models default to omitted thinking display. Older models that
	// expose thinking default to summarized output.
	if modelHasAnyPrefix(
		modelID,
		"claude-opus-5",
		"claude-sonnet-5",
		"claude-haiku-5-5",
		"claude-fable-5",
		"claude-mythos-5",
		"claude-opus-4.8",
		"claude-opus-4-8",
		"claude-opus-4.7",
		"claude-opus-4-7",
	) {
		return "omitted"
	}

	return "summarized"
}

func claudeModelSupportsManualInterleavedThinking(modelID string) bool {
	model := strings.ToLower(strings.TrimSpace(modelID))
	model = strings.ReplaceAll(model, ".", "-")

	// The beta header is accepted broadly, but only these manual-thinking
	// model families actually interleave. Keep this allow-list conservative so
	// a future Claude 4.x model is not assumed compatible by prefix alone.
	switch {
	case strings.HasPrefix(model, "claude-sonnet-4-6"):
		return true
	case strings.HasPrefix(model, "claude-opus-4-5"):
		return true
	case strings.HasPrefix(model, "claude-sonnet-4-5"):
		return true
	case strings.HasPrefix(model, "claude-opus-4-1"):
		return true
	case model == "claude-opus-4" || strings.HasPrefix(model, "claude-opus-4-2025"):
		return true
	case model == "claude-sonnet-4" || strings.HasPrefix(model, "claude-sonnet-4-2025"):
		return true
	default:
		return false
	}
}

func fallbackClaudeReasoningCapability(modelID string) ModelReasoningCapability {
	capability := ModelReasoningCapability{ModelID: modelID}

	// Prefer the live Kiro schema whenever it is available. These fallbacks only
	// cover stable public Claude contracts so
	// requests can still be translated while the model cache is cold or stale.
	if isClaudeOpus55Model(modelID) {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"adaptive"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.DefaultThinkingType = "adaptive"
		capability.DefaultThinkingDisplay = "omitted"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high", "xhigh", "max"}
	} else if isClaudeSonnet55Model(modelID) {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"adaptive", "between_tools"}
		capability.SupportsDisplay = true // adaptive only; between_tools forbids display
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.DefaultThinkingType = "adaptive"
		capability.DefaultThinkingDisplay = "omitted"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high", "xhigh", "max"}
	} else if isClaudeHaiku55Model(modelID) {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"adaptive", "disabled"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.DefaultThinkingType = "adaptive"
		capability.DefaultThinkingDisplay = "omitted"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high", "xhigh", "max"}
	} else if modelHasAnyPrefix(modelID, "claude-opus-5", "claude-sonnet-5") {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"adaptive", "disabled"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.DefaultThinkingType = "adaptive"
		capability.DefaultThinkingDisplay = "omitted"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high", "xhigh", "max"}
	}
	if modelHasAnyPrefix(modelID, "claude-fable-5", "claude-mythos-5") {
		capability.SupportsThinking = true
		// Fable 5 and Mythos 5 use always-on adaptive thinking and reject
		// thinking.type=disabled.
		capability.ThinkingTypes = []string{"adaptive"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.DefaultThinkingType = "adaptive"
		capability.DefaultThinkingDisplay = "omitted"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high", "xhigh", "max"}
	}
	if modelHasAnyPrefix(
		modelID,
		"claude-opus-4.8",
		"claude-opus-4-8",
		"claude-opus-4.7",
		"claude-opus-4-7",
	) {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"adaptive", "disabled"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.DefaultThinkingDisplay = "omitted"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high", "xhigh", "max"}
	}
	if modelHasAnyPrefix(
		modelID,
		"claude-opus-4.6",
		"claude-opus-4-6",
		"claude-sonnet-4.6",
		"claude-sonnet-4-6",
	) {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"enabled", "adaptive", "disabled"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.SupportsBudgetTokens = true
		capability.DefaultThinkingDisplay = "summarized"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high", "max"}
	}
	if modelHasAnyPrefix(
		modelID,
		"claude-opus-4.5",
		"claude-opus-4-5",
	) {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"enabled", "disabled"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.SupportsBudgetTokens = true
		capability.DefaultThinkingDisplay = "summarized"
		capability.EffortPath = ReasoningSchemaOutputConfig
		capability.Efforts = []string{"low", "medium", "high"}
	}
	if modelHasAnyPrefix(
		modelID,
		"claude-sonnet-4.5",
		"claude-sonnet-4-5",
		"claude-haiku-4.5",
		"claude-haiku-4-5",
	) {
		capability.SupportsThinking = true
		capability.ThinkingTypes = []string{"enabled", "disabled"}
		capability.SupportsDisplay = true
		capability.ThinkingDisplays = []string{"summarized", "omitted"}
		capability.SupportsBudgetTokens = true
		capability.DefaultThinkingDisplay = "summarized"
	}

	capability.SupportsInterleavedThinking =
		claudeModelSupportsManualInterleavedThinking(modelID)

	return capability
}

// Normalize aliases while preserving schema-provided values.
func normalizeEffortList(values []string) []string {
	byCanonical := make(map[string]string)

	for _, value := range values {
		canonical := canonicalEffort(value)
		if canonical == "" {
			continue
		}
		if _, exists := byCanonical[canonical]; !exists {
			byCanonical[canonical] = value
		}
	}

	ordered := make([]string, 0, len(byCanonical))

	for _, canonical := range canonicalEffortOrder {
		if original, ok := byCanonical[canonical]; ok {
			ordered = append(ordered, original)
			delete(byCanonical, canonical)
		}
	}

	unknown := make([]string, 0, len(byCanonical))

	for _, original := range byCanonical {
		unknown = append(unknown, original)
	}
	sort.Strings(unknown)
	ordered = append(ordered, unknown...)

	return ordered
}

type reasoningIntent struct {
	Enabled      bool
	Disabled     bool
	ThinkingType string
	Display      string
	Effort       string
	BudgetTokens int
}

func normalizeRequestedThinkingType(requested string, supported []string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(requested))
	if raw == "" {
		return "", nil
	}

	// Normalize the stable enabled/none aliases while preserving schema-defined
	// values we do not know yet. Capability validation happens later in
	// selectThinkingType, so an explicit unsupported value is rejected instead
	// of silently becoming "no reasoning", while future schema-advertised types
	// remain usable without a code change here.
	return normalizeClaudeThinkingType(raw, supported), nil
}

func BuildClaudeAdditionalModelRequestFields(req *ClaudeRequest, capability ModelReasoningCapability) (
	fields map[string]interface{},
	requested bool,
	err error,
) {
	if req == nil {
		return nil, false, nil
	}

	intent := reasoningIntent{}

	if req.Thinking != nil {
		thinkingType, typeErr := normalizeRequestedThinkingType(req.Thinking.Type, capability.ThinkingTypes)
		if typeErr != nil {
			// true because the client sent an explicit reasoning config.
			return nil, true, typeErr
		}
		intent.ThinkingType = thinkingType
		intent.Display = strings.ToLower(strings.TrimSpace(req.Thinking.Display))
		intent.BudgetTokens = req.Thinking.BudgetTokens

		switch intent.ThinkingType {
		case "disabled", "none":
			intent.Disabled = true
		default:
			if intent.ThinkingType != "" {
				intent.Enabled = true
			}
		}
	}

	if req.OutputConfig != nil {
		intent.Effort = req.OutputConfig.Effort
	}
	if strings.TrimSpace(intent.Effort) == "" {
		intent.Effort = req.ReasoningEffort
	}
	if strings.TrimSpace(intent.Effort) != "" && !intent.Disabled {
		intent.Enabled = true
	}

	return buildAdditionalModelRequestFields(intent, capability)
}

func BuildOpenAIAdditionalModelRequestFields(
	req *OpenAIRequest,
	capability ModelReasoningCapability,
) (
	fields map[string]interface{},
	requested bool,
	err error,
) {
	if req == nil {
		return nil, false, nil
	}

	intent := reasoningIntent{}

	if req.Thinking != nil {
		thinkingType, typeErr := normalizeRequestedThinkingType(req.Thinking.Type, capability.ThinkingTypes)

		if typeErr != nil {
			return nil, true, typeErr
		}

		intent.ThinkingType = thinkingType
		intent.Display = strings.ToLower(
			strings.TrimSpace(req.Thinking.Display),
		)
		intent.BudgetTokens = req.Thinking.BudgetTokens

		switch intent.ThinkingType {
		case "disabled", "none":
			intent.Disabled = true
		default:
			if intent.ThinkingType != "" {
				intent.Enabled = true
			}
		}
	}

	if req.OutputConfig != nil {
		intent.Effort = req.OutputConfig.Effort
	}
	if strings.TrimSpace(intent.Effort) == "" && req.Reasoning != nil {
		intent.Effort = req.Reasoning.Effort
	}
	if strings.TrimSpace(intent.Effort) == "" {
		intent.Effort = req.ReasoningEffort
	}

	effort := canonicalEffort(intent.Effort)
	switch effort {
	case "":
		// Client did not request an effort level.

	case "none":
		// OpenAI's "none" means the client did not request reasoning. Do not
		// synthesize a Kiro thinking.type=disabled field merely because the model
		// schema happens to expose one. An explicit thinking.type=disabled request
		// remains an explicit disable through intent.Disabled.
		intent.Enabled = false
		intent.Effort = ""

	default:
		intent.Effort = effort
		if !intent.Disabled {
			intent.Enabled = true
		}
	}

	return buildAdditionalModelRequestFields(intent, capability)
}

// Build only fields advertised by the model schema. Stable, documented
// model-specific incompatibilities are rejected locally; unknown combinations
// remain backend-validated.
func buildAdditionalModelRequestFields(
	intent reasoningIntent,
	capability ModelReasoningCapability,
) (
	fields map[string]interface{},
	requested bool,
	err error,
) {
	effort := strings.TrimSpace(intent.Effort)
	requested = intent.Enabled || intent.Disabled || effort != ""
	if !requested {
		return nil, false, nil
	}

	if err := validateThinkingEffortCombination(
		capability.ModelID,
		intent.Disabled,
		effort,
	); err != nil {
		return nil, true, err
	}

	if !capability.SupportsNativeReasoning() {
		return nil, true, nil
	}

	fields = make(map[string]interface{})

	if capability.SupportsThinking {
		thinking := make(map[string]interface{})

		if intent.Disabled {
			disabledType, ok := matchThinkingType(
				"disabled",
				capability.ThinkingTypes,
			)
			if !ok {
				return nil, true, fmt.Errorf(
					"model %s does not support thinking.type disabled; supported: %s",
					capability.ModelID,
					strings.Join(capability.ThinkingTypes, ", "),
				)
			}
			thinking["type"] = disabledType
		} else {
			thinkingType, ok := selectThinkingType(
				intent.ThinkingType,
				capability.ThinkingTypes,
			)
			if !ok {
				return nil, true, fmt.Errorf(
					"model %s does not support thinking.type %q; supported: %s",
					capability.ModelID,
					intent.ThinkingType,
					strings.Join(capability.ThinkingTypes, ", "),
				)
			}

			if thinkingType != "" {
				thinking["type"] = thinkingType
			}

			if intent.Display != "" {
				if !capability.SupportsDisplay {
					return nil, true, fmt.Errorf(
						"model %s does not support thinking.display",
						capability.ModelID,
					)
				}

				display := intent.Display
				if len(capability.ThinkingDisplays) > 0 {
					var ok bool
					display, ok = matchSupportedDisplay(
						intent.Display,
						capability.ThinkingDisplays,
					)
					if !ok {
						return nil, true, fmt.Errorf(
							"model %s does not support thinking.display %q; supported: %s",
							capability.ModelID,
							intent.Display,
							strings.Join(capability.ThinkingDisplays, ", "),
						)
					}
				}
				thinking["display"] = display
			}

			// Claude Code may still send the legacy enabled+budget_tokens shape.
			// Forward the budget only for models whose Kiro schema advertises it;
			// adaptive-only models receive the mapped type without budget_tokens.
			if capability.SupportsBudgetTokens && intent.BudgetTokens > 0 {
				thinking["budget_tokens"] = intent.BudgetTokens
			} else if intent.BudgetTokens > 0 {
				logger.Warnf(
					"[ReasoningCompatibility] model=%s requestedThinkingType=%s effectiveThinkingType=%s budgetTokens=%d forwarded=false reason=model_schema_does_not_support_budget_tokens",
					capability.ModelID,
					intent.ThinkingType,
					thinkingType,
					intent.BudgetTokens,
				)
			}
		}

		if len(thinking) > 0 {
			fields["thinking"] = thinking
		}
	}

	if effort != "" {
		supportedEffort, ok := matchSupportedEffort(
			effort,
			capability.Efforts,
		)
		if !ok {
			return nil, true, fmt.Errorf(
				"model %s does not support reasoning effort %q; supported: %s",
				capability.ModelID,
				effort,
				strings.Join(capability.Efforts, ", "),
			)
		}

		switch capability.EffortPath {
		case ReasoningSchemaOutputConfig:
			fields["output_config"] = map[string]interface{}{
				"effort": supportedEffort,
			}

		case ReasoningSchemaReasoning:
			fields["reasoning"] = map[string]interface{}{
				"effort": supportedEffort,
			}

		default:
			return nil, true, fmt.Errorf(
				"model %s does not expose a configurable effort field",
				capability.ModelID,
			)
		}
	}

	if len(fields) == 0 {
		return nil, true, nil
	}

	return fields, true, nil
}

func selectThinkingType(requested string, supported []string) (string, bool) {
	requested = strings.TrimSpace(requested)

	if requested != "" {
		for _, supportedType := range supported {
			if strings.EqualFold(
				supportedType,
				requested,
			) {
				return supportedType, true
			}
		}

		return "", false
	}

	for _, preferred := range []string{
		"adaptive",
		"enabled",
	} {
		for _, supportedType := range supported {
			if strings.EqualFold(
				supportedType,
				preferred,
			) {
				return supportedType, true
			}
		}
	}

	if len(supported) > 0 {
		return supported[0], true
	}

	return "", false
}

func matchThinkingType(
	requested string,
	supported []string,
) (string, bool) {
	requested = strings.TrimSpace(requested)

	for _, value := range supported {
		if strings.EqualFold(
			strings.TrimSpace(value),
			requested,
		) {
			return value, true
		}
	}

	return "", false
}

func normalizeClaudeThinkingType(
	requested string,
	supported []string,
) string {
	requested = strings.ToLower(
		strings.TrimSpace(requested),
	)

	switch requested {
	case "enabled":
		// Keep enabled when the model natively supports it.
		if value, ok := matchThinkingType(
			"enabled",
			supported,
		); ok {
			return value
		}

		// Claude Code's enabled maps to Kiro adaptive thinking.
		if value, ok := matchThinkingType(
			"adaptive",
			supported,
		); ok {
			return value
		}

	case "none":
		if value, ok := matchThinkingType(
			"disabled",
			supported,
		); ok {
			return value
		}
	}

	return requested
}

func matchSupportedEffort(
	requested string,
	supported []string,
) (string, bool) {
	requestedCanonical := canonicalEffort(requested)

	for _, supportedEffort := range supported {
		if canonicalEffort(supportedEffort) == requestedCanonical {
			return supportedEffort, true
		}
	}

	return "", false
}

func matchSupportedDisplay(
	requested string,
	supported []string,
) (string, bool) {
	requested = strings.TrimSpace(requested)

	for _, value := range supported {
		if strings.EqualFold(value, requested) {
			return value, true
		}
	}

	return "", false
}

func reasoningFieldsJSON(
	fields map[string]interface{},
) string {
	if len(fields) == 0 {
		return "{}"
	}

	data, err := json.Marshal(fields)
	if err != nil {
		return fmt.Sprintf(
			`{"marshalError":%q}`,
			err.Error(),
		)
	}

	return string(data)
}
