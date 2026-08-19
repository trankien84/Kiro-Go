package proxy

import (
	"fmt"
	"strings"
)

type completionGuardToolReferenceSource string

type completionGuardToolReferenceResolutionKind string

const (
	completionGuardReferenceBuiltinSearch completionGuardToolReferenceSource = "builtin_search"
	completionGuardReferenceCustomSearch  completionGuardToolReferenceSource = "custom_search"

	completionGuardReferenceExplicitTool        completionGuardToolReferenceResolutionKind = "explicit_tool"
	completionGuardReferencePotentialDynamicMCP completionGuardToolReferenceResolutionKind = "potential_dynamic_mcp"
)

type completionGuardToolReferenceResolution struct {
	Kind          completionGuardToolReferenceResolutionKind
	Deferred      bool
	Explicit      bool
	CandidateMCPs []string
}

type completionGuardToolReference struct {
	ToolName     string
	Source       completionGuardToolReferenceSource
	Resolution   completionGuardToolReferenceResolution
	SourceCallID string
	MessageIndex int
}

func completionGuardResolveToolReference(
	catalog *completionGuardNormalizedCatalog,
	toolName string,
	source completionGuardToolReferenceSource,
) (completionGuardToolReferenceResolution, string) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return completionGuardToolReferenceResolution{}, "tool_reference.tool_name must be a non-empty string"
	}
	if catalog == nil {
		return completionGuardToolReferenceResolution{
			Kind:     completionGuardReferenceExplicitTool,
			Explicit: true,
		}, ""
	}
	if entry, exists := catalog.ExplicitToolsByName[toolName]; exists {
		if source == completionGuardReferenceBuiltinSearch && !entry.Deferred {
			return completionGuardToolReferenceResolution{}, fmt.Sprintf(
				"built-in tool search reference %q must resolve to a tool with defer_loading=true",
				toolName,
			)
		}
		return completionGuardToolReferenceResolution{
			Kind:     completionGuardReferenceExplicitTool,
			Deferred: entry.Deferred,
			Explicit: true,
		}, ""
	}
	if source == completionGuardReferenceBuiltinSearch {
		candidates := completionGuardCatalogMCPToolSearchCandidates(catalog, toolName)
		if len(candidates) > 0 {
			serverNames := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				serverNames = append(serverNames, candidate.ServerName)
			}
			return completionGuardToolReferenceResolution{
				Kind:          completionGuardReferencePotentialDynamicMCP,
				Deferred:      true,
				CandidateMCPs: serverNames,
			}, ""
		}
	}
	if source == completionGuardReferenceCustomSearch {
		return completionGuardToolReferenceResolution{}, fmt.Sprintf(
			"tool_reference %q has no corresponding top-level tool definition",
			toolName,
		)
	}
	return completionGuardToolReferenceResolution{}, fmt.Sprintf(
		"tool_reference %q has no corresponding top-level deferred tool definition",
		toolName,
	)
}

func completionGuardReferenceMap(
	value interface{},
) (map[string]interface{}, bool) {
	mapped, ok := value.(map[string]interface{})
	return mapped, ok && mapped != nil
}

func completionGuardParseReferenceItems(
	items interface{},
	source completionGuardToolReferenceSource,
	sourceCallID string,
	messageIndex int,
	catalog *completionGuardNormalizedCatalog,
) ([]completionGuardToolReference, string) {
	values, ok := items.([]interface{})
	if !ok {
		return nil, "tool_references must be an array"
	}
	references := make([]completionGuardToolReference, 0, len(values))
	for index, value := range values {
		item, ok := completionGuardReferenceMap(value)
		if !ok || completionGuardMapString(item, "type") != "tool_reference" {
			return nil, fmt.Sprintf("tool_references[%d] must be a tool_reference object", index)
		}
		name := completionGuardMapString(item, "tool_name")
		resolution, reason := completionGuardResolveToolReference(catalog, name, source)
		if reason != "" {
			return nil, reason
		}
		references = append(references, completionGuardToolReference{
			ToolName:     name,
			Source:       source,
			Resolution:   resolution,
			SourceCallID: strings.TrimSpace(sourceCallID),
			MessageIndex: messageIndex,
		})
	}
	return references, ""
}

func completionGuardParseBuiltinToolSearchReferences(
	content interface{},
	sourceCallID string,
	messageIndex int,
	catalog *completionGuardNormalizedCatalog,
) ([]completionGuardToolReference, string) {
	result, ok := completionGuardReferenceMap(content)
	if !ok {
		return nil, "tool_search_tool_result.content must be an object"
	}
	switch completionGuardMapString(result, "type") {
	case "tool_search_tool_result_error":
		errorCode := completionGuardMapString(result, "error_code")
		switch errorCode {
		case "invalid_tool_input", "unavailable", "too_many_requests", "execution_time_exceeded":
			return nil, ""
		case "":
			return nil, "tool_search_tool_result error content requires error_code"
		default:
			return nil, fmt.Sprintf("unsupported tool_search_tool_result error_code %q", errorCode)
		}
	case "tool_search_tool_search_result":
		// Parse references below.
	default:
		return nil, "tool_search_tool_result.content must be a tool_search_tool_search_result or tool_search_tool_result_error object"
	}
	items, exists := result["tool_references"]
	if !exists {
		return nil, "tool_search_tool_result.content.tool_references is required"
	}
	return completionGuardParseReferenceItems(
		items,
		completionGuardReferenceBuiltinSearch,
		sourceCallID,
		messageIndex,
		catalog,
	)
}

func completionGuardParseCustomToolSearchReferences(
	content interface{},
	sourceCallID string,
	messageIndex int,
	catalog *completionGuardNormalizedCatalog,
) ([]completionGuardToolReference, string) {
	values, ok := content.([]interface{})
	if !ok {
		return nil, ""
	}
	references := make([]completionGuardToolReference, 0)
	for _, value := range values {
		item, ok := completionGuardReferenceMap(value)
		if !ok || completionGuardMapString(item, "type") != "tool_reference" {
			continue
		}
		name := completionGuardMapString(item, "tool_name")
		resolution, reason := completionGuardResolveToolReference(
			catalog,
			name,
			completionGuardReferenceCustomSearch,
		)
		if reason != "" {
			return nil, reason
		}
		references = append(references, completionGuardToolReference{
			ToolName:     name,
			Source:       completionGuardReferenceCustomSearch,
			Resolution:   resolution,
			SourceCallID: strings.TrimSpace(sourceCallID),
			MessageIndex: messageIndex,
		})
	}
	return references, ""
}
