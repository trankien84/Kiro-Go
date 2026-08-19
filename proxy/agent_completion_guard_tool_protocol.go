package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type completionGuardToolFeature string

const (
	completionGuardToolFeatureCodeExecution completionGuardToolFeature = "code_execution"
	completionGuardToolFeatureWebSearch     completionGuardToolFeature = "web_search"
	completionGuardToolFeatureWebFetch      completionGuardToolFeature = "web_fetch"
	completionGuardToolFeatureToolSearch    completionGuardToolFeature = "tool_search"
	completionGuardToolFeatureAdvisor       completionGuardToolFeature = "advisor"
)

type completionGuardWireToolDefinition struct {
	Name            string
	ResultBlockType string
	CanCallTools    bool
}

type completionGuardServerToolDefinition struct {
	ExpectedName string
	Feature      completionGuardToolFeature
	Dynamic      bool
	WireTools    []completionGuardWireToolDefinition
}

var completionGuardServerToolDefinitions = map[string]completionGuardServerToolDefinition{
	"code_execution_20250522": {
		ExpectedName: "code_execution",
		Feature:      completionGuardToolFeatureCodeExecution,
		WireTools: []completionGuardWireToolDefinition{
			{Name: "code_execution", ResultBlockType: "code_execution_tool_result"},
		},
	},
	"code_execution_20250825": {
		ExpectedName: "code_execution",
		Feature:      completionGuardToolFeatureCodeExecution,
		WireTools: []completionGuardWireToolDefinition{
			{Name: "bash_code_execution", ResultBlockType: "bash_code_execution_tool_result"},
			{Name: "text_editor_code_execution", ResultBlockType: "text_editor_code_execution_tool_result"},
		},
	},
	"code_execution_20260120": {
		ExpectedName: "code_execution",
		Feature:      completionGuardToolFeatureCodeExecution,
		WireTools: []completionGuardWireToolDefinition{
			{Name: "bash_code_execution", ResultBlockType: "bash_code_execution_tool_result"},
			{Name: "text_editor_code_execution", ResultBlockType: "text_editor_code_execution_tool_result"},
			{Name: "code_execution", ResultBlockType: "code_execution_tool_result", CanCallTools: true},
		},
	},
	"code_execution_20260521": {
		ExpectedName: "code_execution",
		Feature:      completionGuardToolFeatureCodeExecution,
		WireTools: []completionGuardWireToolDefinition{
			{Name: "bash_code_execution", ResultBlockType: "bash_code_execution_tool_result"},
			{Name: "text_editor_code_execution", ResultBlockType: "text_editor_code_execution_tool_result"},
			{Name: "code_execution", ResultBlockType: "code_execution_tool_result", CanCallTools: true},
		},
	},
	"web_search_20250305": completionGuardSingleWireServerTool(
		"web_search", "web_search_tool_result", completionGuardToolFeatureWebSearch, false,
	),
	"web_search_20260209": completionGuardSingleWireServerTool(
		"web_search", "web_search_tool_result", completionGuardToolFeatureWebSearch, true,
	),
	"web_search_20260318": completionGuardSingleWireServerTool(
		"web_search", "web_search_tool_result", completionGuardToolFeatureWebSearch, true,
	),
	"web_fetch_20250910": completionGuardSingleWireServerTool(
		"web_fetch", "web_fetch_tool_result", completionGuardToolFeatureWebFetch, false,
	),
	"web_fetch_20260209": completionGuardSingleWireServerTool(
		"web_fetch", "web_fetch_tool_result", completionGuardToolFeatureWebFetch, true,
	),
	"web_fetch_20260309": completionGuardSingleWireServerTool(
		"web_fetch", "web_fetch_tool_result", completionGuardToolFeatureWebFetch, true,
	),
	"web_fetch_20260318": completionGuardSingleWireServerTool(
		"web_fetch", "web_fetch_tool_result", completionGuardToolFeatureWebFetch, true,
	),
	"tool_search_tool_regex": completionGuardSingleWireServerTool(
		"tool_search_tool_regex", "tool_search_tool_result", completionGuardToolFeatureToolSearch, false,
	),
	"tool_search_tool_regex_20251119": completionGuardSingleWireServerTool(
		"tool_search_tool_regex", "tool_search_tool_result", completionGuardToolFeatureToolSearch, false,
	),
	"tool_search_tool_bm25": completionGuardSingleWireServerTool(
		"tool_search_tool_bm25", "tool_search_tool_result", completionGuardToolFeatureToolSearch, false,
	),
	"tool_search_tool_bm25_20251119": completionGuardSingleWireServerTool(
		"tool_search_tool_bm25", "tool_search_tool_result", completionGuardToolFeatureToolSearch, false,
	),
	"advisor_20260301": completionGuardSingleWireServerTool(
		"advisor", "advisor_tool_result", completionGuardToolFeatureAdvisor, false,
	),
}

func completionGuardSingleWireServerTool(
	name string,
	resultBlockType string,
	feature completionGuardToolFeature,
	dynamic bool,
) completionGuardServerToolDefinition {
	return completionGuardServerToolDefinition{
		ExpectedName: name,
		Feature:      feature,
		Dynamic:      dynamic,
		WireTools: []completionGuardWireToolDefinition{
			{Name: name, ResultBlockType: resultBlockType},
		},
	}
}

func completionGuardLookupServerToolDefinition(
	toolType string,
) (completionGuardServerToolDefinition, bool) {
	definition, ok := completionGuardServerToolDefinitions[strings.ToLower(strings.TrimSpace(toolType))]
	return definition, ok
}

func completionGuardReservedServerToolFamily(toolType string) (string, bool) {
	toolType = strings.ToLower(strings.TrimSpace(toolType))
	switch {
	case toolType == "code_execution" || strings.HasPrefix(toolType, "code_execution_"):
		return "code_execution", true
	case toolType == "web_search" || strings.HasPrefix(toolType, "web_search_"):
		return "web_search", true
	case toolType == "web_fetch" || strings.HasPrefix(toolType, "web_fetch_"):
		return "web_fetch", true
	case toolType == "tool_search_tool_regex" || strings.HasPrefix(toolType, "tool_search_tool_regex_"):
		return "tool_search_tool_regex", true
	case toolType == "tool_search_tool_bm25" || strings.HasPrefix(toolType, "tool_search_tool_bm25_"):
		return "tool_search_tool_bm25", true
	case toolType == "advisor" || strings.HasPrefix(toolType, "advisor_"):
		return "advisor", true
	default:
		return "", false
	}
}

func completionGuardExpectedServerToolName(toolType string) (string, bool) {
	definition, ok := completionGuardLookupServerToolDefinition(toolType)
	return definition.ExpectedName, ok
}

func completionGuardDefinitionCanCallTools(
	definition completionGuardServerToolDefinition,
) bool {
	for _, wire := range definition.WireTools {
		if wire.CanCallTools {
			return true
		}
	}
	return false
}

func completionGuardAddServerToolDefinitionCapabilities(
	catalog *completionGuardNormalizedCatalog,
	definition completionGuardServerToolDefinition,
	implicit bool,
	allowDirect bool,
	allowCode bool,
) {
	if catalog == nil {
		return
	}
	for _, wire := range definition.WireTools {
		declarationName := definition.ExpectedName
		if implicit {
			declarationName = ""
		}
		catalog.ServerCapabilitiesByName[wire.Name] = completionGuardServerToolCapability{
			ResultBlockType:   wire.ResultBlockType,
			DeclarationName:   declarationName,
			AllowDirectCaller: allowDirect,
			AllowCodeCaller:   allowCode,
			CanCallTools:      wire.CanCallTools,
			Implicit:          implicit,
		}
	}
}

func completionGuardWireToolFeature(
	name string,
) (completionGuardToolFeature, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	var feature completionGuardToolFeature
	found := false
	for _, definition := range completionGuardServerToolDefinitions {
		for _, wire := range definition.WireTools {
			if wire.Name != name {
				continue
			}
			if found && feature != definition.Feature {
				return "", false
			}
			feature = definition.Feature
			found = true
		}
	}
	return feature, found
}

func completionGuardHistoryRequiresCurrentDeclaration(name string) bool {
	feature, ok := completionGuardWireToolFeature(name)
	return ok && (feature == completionGuardToolFeatureToolSearch ||
		feature == completionGuardToolFeatureAdvisor)
}

func completionGuardKnownServerToolResultType(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	resultType := ""
	for _, definition := range completionGuardServerToolDefinitions {
		for _, wire := range definition.WireTools {
			if wire.Name != name {
				continue
			}
			if resultType != "" && resultType != wire.ResultBlockType {
				// A wire name with conflicting result types must never be guessed.
				return "", false
			}
			resultType = wire.ResultBlockType
		}
	}
	return resultType, resultType != ""
}

func completionGuardDynamicWebTool(toolType string) bool {
	definition, ok := completionGuardLookupServerToolDefinition(toolType)
	return ok && definition.Dynamic
}

func completionGuardRawBoolField(
	fields map[string]json.RawMessage,
	key string,
) (value bool, present bool, err error) {
	raw, present := fields[key]
	if !present {
		return false, false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, true, fmt.Errorf("%s must be a boolean", key)
	}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false, true, fmt.Errorf("%s must be a boolean", key)
	}
	return value, true, nil
}

func completionGuardRawStringField(
	fields map[string]json.RawMessage,
	key string,
) (value string, present bool, err error) {
	raw, present := fields[key]
	if !present {
		return "", false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return strings.TrimSpace(value), true, nil
}

func completionGuardRawIntField(
	fields map[string]json.RawMessage,
	key string,
) (value int64, present bool, err error) {
	raw, present := fields[key]
	if !present {
		return 0, false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, true, fmt.Errorf("%s must be an integer", key)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var decoded interface{}
	if err := decoder.Decode(&decoded); err != nil {
		return 0, true, fmt.Errorf("%s must be an integer", key)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return 0, true, fmt.Errorf("%s must be an integer", key)
	}
	number, ok := decoded.(json.Number)
	if !ok || strings.ContainsAny(number.String(), ".eE") {
		return 0, true, fmt.Errorf("%s must be an integer", key)
	}
	parsed, parseErr := strconv.ParseInt(number.String(), 10, 64)
	if parseErr != nil {
		return 0, true, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, true, nil
}

func completionGuardRawFieldIsNonNull(
	fields map[string]json.RawMessage,
	key string,
) bool {
	raw, exists := fields[key]
	if !exists {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func completionGuardValidateToolCatalogExtras(
	plan agentCompletionGuardPlan,
) string {
	// Model/deployment support is evaluated after an account and transport are
	// known. Completion Guard validates the structural Tool Search protocol only;
	// a custom Anthropic-compatible endpoint must not inherit official Anthropic
	// feature matrices merely because it reuses an official-looking model ID.
	return ""
}

func completionGuardHeaderHasBetaToken(headers http.Header, wanted string) bool {
	wanted = strings.TrimSpace(wanted)
	if wanted == "" {
		return false
	}
	for _, value := range headers.Values("Anthropic-Beta") {
		for _, token := range strings.Split(value, ",") {
			if strings.TrimSpace(token) == wanted {
				return true
			}
		}
	}
	return false
}

func completionGuardValidateSemanticHeaders(
	catalog *completionGuardNormalizedCatalog,
	headers http.Header,
) string {
	if catalog != nil && catalog.HasAdvisor &&
		!completionGuardHeaderHasBetaToken(headers, "advisor-tool-2026-03-01") {
		return "advisor_20260301 requires anthropic-beta token advisor-tool-2026-03-01"
	}
	if completionGuardCatalogHasMCP(catalog) &&
		!completionGuardHeaderHasBetaToken(headers, "mcp-client-2025-11-20") {
		return "mcp_toolset requires anthropic-beta token mcp-client-2025-11-20"
	}
	return ""
}
