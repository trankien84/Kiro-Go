package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	responsesToolKindFunction = "function"
	responsesToolKindCustom   = "custom"
)

type responsesToolIdentity struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
}

func (i responsesToolIdentity) key() string {
	// Namespace and bare name form the client-visible identity. Kind is semantics:
	// declaring the same {namespace,name} once as function and once as custom is
	// therefore a conflict rather than two overloads.
	return strings.TrimSpace(i.Namespace) + "\x00" + strings.TrimSpace(i.Name)
}

func (i responsesToolIdentity) display() string {
	if ns := strings.TrimSpace(i.Namespace); ns != "" {
		return ns + "." + strings.TrimSpace(i.Name)
	}
	return strings.TrimSpace(i.Name)
}

type responsesToolDeclaration struct {
	Identity             responsesToolIdentity `json:"identity"`
	NamespaceDescription string                `json:"namespace_description,omitempty"`
	Description          string                `json:"description,omitempty"`
	Parameters           interface{}           `json:"parameters,omitempty"`
	Format               interface{}           `json:"format,omitempty"`
	Strict               *bool                 `json:"strict,omitempty"`
	Semantic             json.RawMessage       `json:"semantic"`
}

func (d responsesToolDeclaration) toOpenAITool() OpenAITool {
	var tool OpenAITool
	tool.Type = "function"
	tool.Function.Name = d.Identity.Name
	tool.Function.Description = responsesFlattenedToolDescription(d.NamespaceDescription, d.Description)
	if d.Identity.Kind == responsesToolKindCustom && strings.TrimSpace(tool.Function.Description) == "" {
		// Responses custom-tool descriptions are optional, while Kiro requires a
		// non-empty description. Synthesize a deterministic adapter description
		// and let the Responses semantic classifier record this as emulation.
		tool.Function.Description = "Responses custom tool " + d.Identity.display()
	}
	if d.Identity.Kind == responsesToolKindCustom {
		tool.Function.Parameters = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"input": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"input"},
		}
	} else {
		tool.Function.Parameters = d.Parameters
	}
	tool.ResponsesNamespace = d.Identity.Namespace
	tool.ResponsesKind = d.Identity.Kind
	return tool
}

func responsesFlattenedToolDescription(namespaceDescription, toolDescription string) string {
	namespaceDescription = strings.TrimSpace(namespaceDescription)
	toolDescription = strings.TrimSpace(toolDescription)
	switch {
	case namespaceDescription == "":
		return toolDescription
	case toolDescription == "":
		return namespaceDescription
	default:
		return namespaceDescription + "\n\n" + toolDescription
	}
}

func responsesToolDeclarationFromOpenAITool(tool OpenAITool) responsesToolDeclaration {
	kind := strings.TrimSpace(tool.ResponsesKind)
	if kind == "" {
		kind = responsesToolKindFunction
	}
	d := responsesToolDeclaration{
		Identity: responsesToolIdentity{
			Namespace: strings.TrimSpace(tool.ResponsesNamespace),
			Name:      strings.TrimSpace(tool.Function.Name),
			Kind:      kind,
		},
		Description: tool.Function.Description,
		Parameters:  tool.Function.Parameters,
	}
	semantic, _ := json.Marshal(map[string]interface{}{
		"type":        kind,
		"name":        d.Identity.Name,
		"namespace":   d.Identity.Namespace,
		"description": d.Description,
		"parameters":  d.Parameters,
	})
	d.Semantic = semantic
	return d
}

func parseResponsesUnifiedTools(body []byte) ([]responsesToolDeclaration, []OpenAITool, json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, nil, nil, err
	}
	if root == nil {
		return nil, nil, nil, fmt.Errorf("request body must be a JSON object")
	}

	declarations := make([]responsesToolDeclaration, 0)
	byIdentity := make(map[string]int)
	appendDeclaration := func(d responsesToolDeclaration) error {
		key := d.Identity.key()
		if strings.TrimSpace(d.Identity.Name) == "" {
			return fmt.Errorf("tool declaration has an empty name")
		}
		if index, ok := byIdentity[key]; ok {
			if string(declarations[index].Semantic) == string(d.Semantic) {
				return nil
			}
			return fmt.Errorf("conflicting tool declarations for %q", d.Identity.display())
		}
		byIdentity[key] = len(declarations)
		declarations = append(declarations, d)
		return nil
	}

	if raw, ok := root["tools"]; ok && !jsonRawIsNull(raw) {
		if err := parseResponsesToolDeclarationArray(raw, "", "", "tools", appendDeclaration); err != nil {
			return nil, nil, nil, err
		}
	}

	if raw, ok := root["input"]; ok && !jsonRawIsNull(raw) {
		items, err := responsesInputRawItems(raw)
		if err != nil {
			return nil, nil, nil, err
		}
		for index, itemRaw := range items {
			var item map[string]json.RawMessage
			if err := json.Unmarshal(itemRaw, &item); err != nil || item == nil {
				continue
			}
			var typ string
			_ = json.Unmarshal(item["type"], &typ)
			if typ != "additional_tools" {
				continue
			}
			toolsRaw, ok := item["tools"]
			if !ok || jsonRawIsNull(toolsRaw) {
				return nil, nil, nil, fmt.Errorf("input[%d] additional_tools requires tools", index)
			}
			if err := parseResponsesToolDeclarationArray(toolsRaw, "", "", fmt.Sprintf("input[%d].tools", index), appendDeclaration); err != nil {
				return nil, nil, nil, err
			}
		}
	}

	tools := make([]OpenAITool, 0, len(declarations))
	for _, d := range declarations {
		tools = append(tools, d.toOpenAITool())
	}
	stored, err := json.Marshal(declarations)
	if err != nil {
		return nil, nil, nil, err
	}
	return declarations, tools, stored, nil
}

func responsesInputRawItems(raw json.RawMessage) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || strings.HasPrefix(trimmed, `"`) {
		return nil, nil
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("invalid input array: %w", err)
		}
		return items, nil
	case '{':
		return []json.RawMessage{append(json.RawMessage(nil), raw...)}, nil
	default:
		return nil, fmt.Errorf("unsupported input shape")
	}
}

func parseResponsesToolDeclarationArray(
	raw json.RawMessage,
	namespace string,
	namespaceDescription string,
	path string,
	appendDeclaration func(responsesToolDeclaration) error,
) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("%s must be an array", path)
	}
	for index, item := range items {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil || obj == nil {
			return fmt.Errorf("%s must be an object", itemPath)
		}
		var typ string
		_ = json.Unmarshal(obj["type"], &typ)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "namespace":
			var name, description string
			if err := json.Unmarshal(obj["name"], &name); err != nil || strings.TrimSpace(name) == "" {
				return fmt.Errorf("%s namespace requires a name", itemPath)
			}
			_ = json.Unmarshal(obj["description"], &description)
			if strings.TrimSpace(namespace) != "" {
				return fmt.Errorf("%s nested namespaces are not supported by the Kiro Responses adapter", itemPath)
			}
			children, ok := obj["tools"]
			if !ok || jsonRawIsNull(children) {
				return fmt.Errorf("%s namespace requires tools", itemPath)
			}
			if err := parseResponsesToolDeclarationArray(children, strings.TrimSpace(name), strings.TrimSpace(description), itemPath+".tools", appendDeclaration); err != nil {
				return err
			}

		case responsesToolKindFunction:
			d, err := parseResponsesFunctionDeclaration(obj, namespace, namespaceDescription, itemPath)
			if err != nil {
				return err
			}
			if err := appendDeclaration(d); err != nil {
				return fmt.Errorf("%s: %w", itemPath, err)
			}

		case responsesToolKindCustom:
			d, err := parseResponsesCustomDeclaration(obj, namespace, namespaceDescription, itemPath)
			if err != nil {
				return err
			}
			if err := appendDeclaration(d); err != nil {
				return fmt.Errorf("%s: %w", itemPath, err)
			}

		default:
			// Keep the routing envelope permissive so a custom OpenAI-compatible
			// relay can receive hosted/future tool types unchanged. The Responses
			// semantic classifier separately marks them as non-Kiro-compatible.
			continue
		}
	}
	return nil
}

func parseResponsesFunctionDeclaration(obj map[string]json.RawMessage, namespace, namespaceDescription, path string) (responsesToolDeclaration, error) {
	fn := obj
	if nested, ok := obj["function"]; ok && !jsonRawIsNull(nested) {
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(nested, &decoded); err != nil || decoded == nil {
			return responsesToolDeclaration{}, fmt.Errorf("%s.function must be an object", path)
		}
		fn = decoded
	}
	var name, description string
	_ = json.Unmarshal(fn["name"], &name)
	_ = json.Unmarshal(fn["description"], &description)
	name = strings.TrimSpace(name)
	if name == "" {
		return responsesToolDeclaration{}, fmt.Errorf("%s function tool requires a name", path)
	}
	var params interface{}
	if raw, ok := fn["parameters"]; ok && !jsonRawIsNull(raw) {
		if err := json.Unmarshal(raw, &params); err != nil {
			return responsesToolDeclaration{}, fmt.Errorf("%s function parameters must be valid JSON", path)
		}
	}
	var strict *bool
	if raw, ok := fn["strict"]; ok && !jsonRawIsNull(raw) {
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return responsesToolDeclaration{}, fmt.Errorf("%s function strict must be a boolean", path)
		}
		strict = &value
	} else if raw, ok := obj["strict"]; ok && !jsonRawIsNull(raw) {
		var value bool
		if err := json.Unmarshal(raw, &value); err == nil {
			strict = &value
		}
	}
	semanticMap := map[string]interface{}{
		"type":                  responsesToolKindFunction,
		"name":                  name,
		"namespace":             strings.TrimSpace(namespace),
		"namespace_description": strings.TrimSpace(namespaceDescription),
		"description":           description,
		"parameters":            params,
	}
	for _, key := range []string{"allowed_callers", "defer_loading"} {
		if raw, ok := obj[key]; ok && !jsonRawIsNull(raw) {
			var value interface{}
			if json.Unmarshal(raw, &value) == nil {
				semanticMap[key] = value
			}
		}
	}
	if strict != nil {
		semanticMap["strict"] = *strict
	}
	semantic, _ := json.Marshal(semanticMap)
	return responsesToolDeclaration{
		Identity:             responsesToolIdentity{Namespace: strings.TrimSpace(namespace), Name: name, Kind: responsesToolKindFunction},
		NamespaceDescription: strings.TrimSpace(namespaceDescription),
		Description:          description,
		Parameters:           params,
		Strict:               strict,
		Semantic:             semantic,
	}, nil
}

func parseResponsesCustomDeclaration(obj map[string]json.RawMessage, namespace, namespaceDescription, path string) (responsesToolDeclaration, error) {
	var name, description string
	_ = json.Unmarshal(obj["name"], &name)
	_ = json.Unmarshal(obj["description"], &description)
	name = strings.TrimSpace(name)
	if name == "" {
		return responsesToolDeclaration{}, fmt.Errorf("%s custom tool requires a name", path)
	}
	var format interface{}
	if raw, ok := obj["format"]; ok && !jsonRawIsNull(raw) {
		if err := json.Unmarshal(raw, &format); err != nil {
			return responsesToolDeclaration{}, fmt.Errorf("%s custom tool format must be valid JSON", path)
		}
	}
	semanticMap := map[string]interface{}{
		"type":                  responsesToolKindCustom,
		"name":                  name,
		"namespace":             strings.TrimSpace(namespace),
		"namespace_description": strings.TrimSpace(namespaceDescription),
		"description":           description,
		"format":                format,
	}
	for _, key := range []string{"allowed_callers", "defer_loading"} {
		if raw, ok := obj[key]; ok && !jsonRawIsNull(raw) {
			var value interface{}
			if json.Unmarshal(raw, &value) == nil {
				semanticMap[key] = value
			}
		}
	}
	semantic, _ := json.Marshal(semanticMap)
	return responsesToolDeclaration{
		Identity:             responsesToolIdentity{Namespace: strings.TrimSpace(namespace), Name: name, Kind: responsesToolKindCustom},
		NamespaceDescription: strings.TrimSpace(namespaceDescription),
		Description:          description,
		Format:               format,
		Semantic:             semantic,
	}, nil
}

func responsesToolDeclarationsFromStored(raw json.RawMessage) ([]responsesToolDeclaration, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var declarations []responsesToolDeclaration
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(declarations))
	for index := range declarations {
		d := &declarations[index]
		d.Identity.Namespace = strings.TrimSpace(d.Identity.Namespace)
		d.Identity.Name = strings.TrimSpace(d.Identity.Name)
		d.Identity.Kind = strings.TrimSpace(d.Identity.Kind)
		if d.Identity.Kind != responsesToolKindFunction && d.Identity.Kind != responsesToolKindCustom {
			return nil, fmt.Errorf("tool_catalog[%d] has unsupported kind %q", index, d.Identity.Kind)
		}
		if d.Identity.Name == "" {
			return nil, fmt.Errorf("tool_catalog[%d] has an empty name", index)
		}
		key := d.Identity.key()
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("tool_catalog has duplicate identity %q", d.Identity.display())
		}
		seen[key] = struct{}{}
	}
	return declarations, nil
}

func responsesOpenAIToolsFromDeclarations(declarations []responsesToolDeclaration) []OpenAITool {
	tools := make([]OpenAITool, 0, len(declarations))
	for _, d := range declarations {
		tools = append(tools, d.toOpenAITool())
	}
	return tools
}

func responsesStoredToolCatalogFromRequest(req *ResponsesRequest) json.RawMessage {
	if req == nil || len(req.StoredToolCatalog) == 0 {
		return json.RawMessage(`[]`)
	}
	return append(json.RawMessage(nil), req.StoredToolCatalog...)
}

func responsesInheritStoredTools(req *ResponsesRequest, prev *ResponsesObject) error {
	if req == nil || prev == nil || len(req.ToolDeclarations) != 0 {
		return nil
	}
	raw := prev.StoredToolCatalog
	if len(raw) == 0 && prev.StoredStateVersion <= 1 {
		// V1 stored the (function-only) catalog in the public tools field.
		var legacy []OpenAITool
		if len(prev.Tools) > 0 && strings.TrimSpace(string(prev.Tools)) != "null" {
			if err := json.Unmarshal(prev.Tools, &legacy); err != nil {
				return fmt.Errorf("decode inherited v1 tools: %w", err)
			}
			declarations := make([]responsesToolDeclaration, 0, len(legacy))
			for _, tool := range legacy {
				if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" {
					continue
				}
				declarations = append(declarations, responsesToolDeclarationFromOpenAITool(tool))
			}
			raw, _ = json.Marshal(declarations)
		}
	}
	declarations, err := responsesToolDeclarationsFromStored(raw)
	if err != nil {
		return err
	}
	if len(declarations) == 0 {
		return nil
	}
	req.ToolDeclarations = declarations
	req.Tools = responsesOpenAIToolsFromDeclarations(declarations)
	req.StoredToolCatalog = append(json.RawMessage(nil), raw...)
	return nil
}
