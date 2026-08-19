package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type completionGuardOptionalBool struct {
	Value   bool
	Present bool
}

type completionGuardMCPToolConfig struct {
	Enabled      completionGuardOptionalBool
	DeferLoading completionGuardOptionalBool
	Extra        map[string]json.RawMessage
}

type completionGuardMCPServer struct {
	Name string
	Type string
	URL  string
}

type completionGuardMCPToolset struct {
	ServerName    string
	DefaultConfig completionGuardMCPToolConfig
	Configs       map[string]completionGuardMCPToolConfig
	SourceIndex   int
}

type completionGuardCatalogTool struct {
	SourceIndex         int
	Name                string
	Type                string
	Kind                completionGuardToolKind
	Deferred            bool
	DirectCallable      bool
	CodeCallable        bool
	Strict              bool
	InputExamples       bool
	EagerInputStreaming bool
	InputSchema         interface{}
	Declaration         ClaudeTool
}

type completionGuardVisibility struct {
	ExplicitVisibleCount    int
	DeclaredMCPVisibleCount int
	DynamicMCPPossible      bool
	DeferredEnabledCount    int
}

type completionGuardNormalizedCatalog struct {
	Entries                  []completionGuardCatalogTool
	ExplicitToolsByName      map[string]completionGuardCatalogTool
	ClientToolsByName        map[string]completionGuardCatalogTool
	ServerCapabilitiesByName map[string]completionGuardServerToolCapability
	MCPServersByName         map[string]completionGuardMCPServer
	MCPToolsetsByName        map[string]completionGuardMCPToolset
	DeclaredToolCount        int
	HasToolSearch            bool
	HasAdvisor               bool
	ProgrammaticToolCalling  bool
	ProgrammaticRequested    bool
	Visibility               completionGuardVisibility
}

func completionGuardParseOptionalBool(
	fields map[string]json.RawMessage,
	key string,
) (completionGuardOptionalBool, error) {
	value, present, err := completionGuardRawBoolField(fields, key)
	if err != nil {
		return completionGuardOptionalBool{}, err
	}
	return completionGuardOptionalBool{Value: value, Present: present}, nil
}

func completionGuardParseMCPToolConfigRaw(
	raw json.RawMessage,
	path string,
) (completionGuardMCPToolConfig, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return completionGuardMCPToolConfig{}, fmt.Errorf("%s must be an object", path)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return completionGuardMCPToolConfig{}, fmt.Errorf("%s must be an object", path)
	}
	enabled, err := completionGuardParseOptionalBool(fields, "enabled")
	if err != nil {
		return completionGuardMCPToolConfig{}, fmt.Errorf("%s.%s", path, err)
	}
	deferred, err := completionGuardParseOptionalBool(fields, "defer_loading")
	if err != nil {
		return completionGuardMCPToolConfig{}, fmt.Errorf("%s.%s", path, err)
	}
	extra := make(map[string]json.RawMessage)
	for key, value := range fields {
		if key == "enabled" || key == "defer_loading" {
			continue
		}
		extra[key] = append(json.RawMessage(nil), value...)
	}
	if len(extra) == 0 {
		extra = nil
	}
	return completionGuardMCPToolConfig{
		Enabled:      enabled,
		DeferLoading: deferred,
		Extra:        extra,
	}, nil
}

func completionGuardParseMCPToolConfigField(
	fields map[string]json.RawMessage,
	key string,
	path string,
) (completionGuardMCPToolConfig, bool, error) {
	raw, present := fields[key]
	if !present {
		return completionGuardMCPToolConfig{}, false, nil
	}
	config, err := completionGuardParseMCPToolConfigRaw(raw, path)
	return config, true, err
}

func completionGuardBuildMCPServers(
	req *ClaudeRequest,
) (map[string]completionGuardMCPServer, string) {
	servers := make(map[string]completionGuardMCPServer)
	if req == nil || req.Extra == nil {
		return servers, ""
	}
	raw, present := req.Extra["mcp_servers"]
	if !present || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return servers, ""
	}
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return servers, "mcp_servers must be an array of server objects"
	}
	for index, value := range values {
		var name, serverType, rawURL string
		if err := json.Unmarshal(value["name"], &name); err != nil || strings.TrimSpace(name) == "" {
			return servers, fmt.Sprintf("mcp_servers[%d].name must be a non-empty string", index)
		}
		name = strings.TrimSpace(name)
		if _, exists := servers[name]; exists {
			return servers, fmt.Sprintf("duplicate MCP server name %q", name)
		}
		if err := json.Unmarshal(value["type"], &serverType); err != nil || strings.TrimSpace(serverType) == "" {
			return servers, fmt.Sprintf("mcp_servers[%d].type must be %q", index, "url")
		}
		serverType = strings.ToLower(strings.TrimSpace(serverType))
		if serverType != "url" {
			return servers, fmt.Sprintf("mcp_servers[%d].type must be %q", index, "url")
		}
		if err := json.Unmarshal(value["url"], &rawURL); err != nil || strings.TrimSpace(rawURL) == "" {
			return servers, fmt.Sprintf("mcp_servers[%d].url must be a non-empty HTTPS URL", index)
		}
		parsed, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
			return servers, fmt.Sprintf("mcp_servers[%d].url must be a non-empty HTTPS URL", index)
		}
		servers[name] = completionGuardMCPServer{Name: name, Type: serverType, URL: parsed.String()}
	}
	return servers, ""
}

func completionGuardMCPConfigEffective(
	toolset completionGuardMCPToolset,
	toolName string,
) (enabled bool, deferred bool, explicit bool) {
	enabled = true
	deferred = false
	if toolset.DefaultConfig.Enabled.Present {
		enabled = toolset.DefaultConfig.Enabled.Value
	}
	if toolset.DefaultConfig.DeferLoading.Present {
		deferred = toolset.DefaultConfig.DeferLoading.Value
	}
	if config, exists := toolset.Configs[strings.TrimSpace(toolName)]; exists {
		explicit = true
		if config.Enabled.Present {
			enabled = config.Enabled.Value
		}
		if config.DeferLoading.Present {
			deferred = config.DeferLoading.Value
		}
	}
	return enabled, deferred, explicit
}

func completionGuardMCPToolAllowed(
	catalog *completionGuardNormalizedCatalog,
	serverName string,
	toolName string,
) bool {
	if catalog == nil {
		return true
	}
	toolset, exists := catalog.MCPToolsetsByName[strings.TrimSpace(serverName)]
	if !exists {
		return false
	}
	enabled, _, _ := completionGuardMCPConfigEffective(toolset, toolName)
	return enabled
}

type completionGuardCatalogBuildOptions struct {
	// Mid-conversation tool changes may intentionally declare every tool with
	// defer_loading=true and surface tools later through tool_addition blocks.
	// The Tool Search all-deferred rule must not override that separate feature.
	AllowAllDeferred bool
}

func completionGuardBuildNormalizedCatalog(
	req *ClaudeRequest,
) (*completionGuardNormalizedCatalog, string) {
	return completionGuardBuildNormalizedCatalogWithOptions(
		req,
		completionGuardCatalogBuildOptions{},
	)
}

func completionGuardBuildNormalizedCatalogWithOptions(
	req *ClaudeRequest,
	options completionGuardCatalogBuildOptions,
) (*completionGuardNormalizedCatalog, string) {
	catalog := &completionGuardNormalizedCatalog{
		ExplicitToolsByName:      make(map[string]completionGuardCatalogTool),
		ClientToolsByName:        make(map[string]completionGuardCatalogTool),
		ServerCapabilitiesByName: make(map[string]completionGuardServerToolCapability),
		MCPServersByName:         make(map[string]completionGuardMCPServer),
		MCPToolsetsByName:        make(map[string]completionGuardMCPToolset),
	}
	if req == nil {
		return catalog, ""
	}
	catalog.DeclaredToolCount = len(req.Tools)

	servers, reason := completionGuardBuildMCPServers(req)
	if reason != "" {
		return catalog, reason
	}
	catalog.MCPServersByName = servers

	var explicitCodeExecutionType string
	for index, tool := range req.Tools {
		name := strings.TrimSpace(tool.Name)
		toolType := strings.ToLower(strings.TrimSpace(tool.Type))
		if toolType == "mcp_toolset" {
			for _, forbidden := range []string{"name", "strict", "allowed_callers", "defer_loading"} {
				if forbidden == "name" {
					if name != "" {
						return catalog, fmt.Sprintf("tools[%d].name is not valid on mcp_toolset", index)
					}
					continue
				}
				if completionGuardRawFieldIsNonNull(tool.Extra, forbidden) {
					return catalog, fmt.Sprintf("tools[%d].%s is not valid on mcp_toolset", index, forbidden)
				}
			}
			serverName, present, err := completionGuardRawStringField(tool.Extra, "mcp_server_name")
			if err != nil || !present || serverName == "" {
				return catalog, fmt.Sprintf("tools[%d].mcp_server_name must be a non-empty string", index)
			}
			if _, exists := servers[serverName]; !exists {
				return catalog, fmt.Sprintf("tools[%d].mcp_server_name %q does not match mcp_servers", index, serverName)
			}
			if _, exists := catalog.MCPToolsetsByName[serverName]; exists {
				return catalog, fmt.Sprintf("MCP server %q must be referenced by exactly one mcp_toolset", serverName)
			}
			defaultConfig, _, err := completionGuardParseMCPToolConfigField(
				tool.Extra,
				"default_config",
				fmt.Sprintf("tools[%d].default_config", index),
			)
			if err != nil {
				return catalog, err.Error()
			}
			configs := make(map[string]completionGuardMCPToolConfig)
			if raw, present := tool.Extra["configs"]; present {
				if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					return catalog, fmt.Sprintf("tools[%d].configs must be an object", index)
				}
				var configValues map[string]json.RawMessage
				if err := json.Unmarshal(raw, &configValues); err != nil || configValues == nil {
					return catalog, fmt.Sprintf("tools[%d].configs must be an object", index)
				}
				for configName, configRaw := range configValues {
					configName = strings.TrimSpace(configName)
					if configName == "" {
						return catalog, fmt.Sprintf("tools[%d].configs contains an empty tool name", index)
					}
					config, err := completionGuardParseMCPToolConfigRaw(
						configRaw,
						fmt.Sprintf("tools[%d].configs[%q]", index, configName),
					)
					if err != nil {
						return catalog, err.Error()
					}
					configs[configName] = config
				}
			}
			toolset := completionGuardMCPToolset{
				ServerName:    serverName,
				DefaultConfig: defaultConfig,
				Configs:       configs,
				SourceIndex:   index,
			}
			catalog.MCPToolsetsByName[serverName] = toolset

			defaultEnabled, defaultDeferred, _ := completionGuardMCPConfigEffective(toolset, "")
			if defaultEnabled {
				if defaultDeferred {
					catalog.Visibility.DeferredEnabledCount++
				} else {
					catalog.Visibility.DynamicMCPPossible = true
				}
			}
			for configName := range configs {
				enabled, deferred, _ := completionGuardMCPConfigEffective(toolset, configName)
				if !enabled {
					continue
				}
				if deferred {
					catalog.Visibility.DeferredEnabledCount++
				} else {
					catalog.Visibility.DeclaredMCPVisibleCount++
				}
			}
			continue
		}

		definition, knownServerTool := completionGuardLookupServerToolDefinition(toolType)
		reservedName, reservedServerFamily := completionGuardReservedServerToolFamily(toolType)
		if knownServerTool {
			reservedName = definition.ExpectedName
			reservedServerFamily = true
		}
		if reservedServerFamily && name != reservedName {
			return catalog, fmt.Sprintf(
				"tools[%d].name must be %q for tool type %q",
				index,
				reservedName,
				toolType,
			)
		}
		if reservedServerFamily && !knownServerTool {
			reason := fmt.Sprintf("tools[%d] has invalid versioned server tool type %q", index, toolType)
			if reservedName == "code_execution" && strings.HasPrefix(toolType, "code_execution_") {
				reason = fmt.Sprintf("unsupported code execution tool version %q", toolType)
			}
			return catalog, reason
		}
		if name == "" {
			return catalog, fmt.Sprintf("tools[%d].name must be non-empty", index)
		}
		if !completionGuardValidToolName(name) {
			return catalog, fmt.Sprintf(
				"tools[%d].name %q must match ^[a-zA-Z0-9_-]{1,64}$",
				index,
				name,
			)
		}
		if _, exists := catalog.ExplicitToolsByName[name]; exists {
			return catalog, fmt.Sprintf("duplicate tool name %q", name)
		}

		kind := completionGuardToolKindServer
		if claudeToolCanEmitClientToolUse(tool) {
			kind = completionGuardToolKindClient
		} else if !knownServerTool {
			return catalog, fmt.Sprintf("tools[%d] has unsupported server tool type %q", index, toolType)
		}

		deferred, _, err := completionGuardRawBoolField(tool.Extra, "defer_loading")
		if err != nil {
			return catalog, fmt.Sprintf("tools[%d].%s", index, err)
		}
		strict, _, err := completionGuardRawBoolField(tool.Extra, "strict")
		if err != nil {
			return catalog, fmt.Sprintf("tools[%d].%s", index, err)
		}
		eagerInputStreaming, eagerInputStreamingPresent, err := completionGuardRawBoolField(tool.Extra, "eager_input_streaming")
		if err != nil {
			return catalog, fmt.Sprintf("tools[%d].%s", index, err)
		}
		inputExamples := false
		inputExamplesPresent := false
		if raw, present := tool.Extra["input_examples"]; present && claudeRawMeaningful(raw) {
			inputExamplesPresent = true
			var examples []json.RawMessage
			if err := json.Unmarshal(raw, &examples); err != nil {
				return catalog, fmt.Sprintf("tools[%d].input_examples must be an array", index)
			}
			inputExamples = len(examples) > 0
		}
		if deferred && completionGuardRawFieldIsNonNull(tool.Extra, "cache_control") {
			return catalog, fmt.Sprintf("tools[%d] cannot combine defer_loading=true with cache_control", index)
		}
		if eagerInputStreamingPresent && !(toolType == "" || toolType == "function" || toolType == "custom") {
			return catalog, fmt.Sprintf("tools[%d].eager_input_streaming is only valid on user-defined tools", index)
		}
		if inputExamplesPresent && kind == completionGuardToolKindServer {
			return catalog, fmt.Sprintf("tools[%d].input_examples is not valid on server-side tools", index)
		}
		if knownServerTool && definition.Feature == completionGuardToolFeatureToolSearch {
			catalog.HasToolSearch = true
			if deferred {
				return catalog, fmt.Sprintf("tools[%d] tool search itself cannot use defer_loading=true", index)
			}
		}
		if knownServerTool && definition.Feature == completionGuardToolFeatureAdvisor {
			catalog.HasAdvisor = true
			model, present, err := completionGuardRawStringField(tool.Extra, "model")
			if err != nil {
				return catalog, fmt.Sprintf("tools[%d].%s", index, err)
			}
			if !present || model == "" {
				return catalog, fmt.Sprintf("tools[%d].model is required for advisor", index)
			}
			if maxTokens, present, err := completionGuardRawIntField(tool.Extra, "max_tokens"); err != nil {
				return catalog, fmt.Sprintf("tools[%d].%s", index, err)
			} else if present && maxTokens < 1024 {
				return catalog, fmt.Sprintf("tools[%d].max_tokens must be at least 1024", index)
			}
			if maxUses, present, err := completionGuardRawIntField(tool.Extra, "max_uses"); err != nil {
				return catalog, fmt.Sprintf("tools[%d].%s", index, err)
			} else if present && maxUses < 0 {
				return catalog, fmt.Sprintf("tools[%d].max_uses must not be negative", index)
			}
		}

		allowDirect, allowCode, callerReason := completionGuardAllowedCallers(tool)
		if callerReason != "" {
			return catalog, fmt.Sprintf("tools[%d].%s", index, callerReason)
		}
		if allowCode && !(knownServerTool && definition.Feature == completionGuardToolFeatureCodeExecution) && strict {
			return catalog, fmt.Sprintf("tools[%d] strict=true is incompatible with programmatic tool calling", index)
		}

		entry := completionGuardCatalogTool{
			SourceIndex:         index,
			Name:                name,
			Type:                toolType,
			Kind:                kind,
			Deferred:            deferred,
			DirectCallable:      allowDirect,
			CodeCallable:        allowCode,
			Strict:              strict,
			InputExamples:       inputExamples,
			EagerInputStreaming: eagerInputStreaming,
			InputSchema:         tool.InputSchema,
			Declaration:         tool,
		}
		catalog.Entries = append(catalog.Entries, entry)
		catalog.ExplicitToolsByName[name] = entry
		if kind == completionGuardToolKindClient {
			catalog.ClientToolsByName[name] = entry
		}
		if deferred {
			catalog.Visibility.DeferredEnabledCount++
		} else {
			catalog.Visibility.ExplicitVisibleCount++
		}
		if allowCode {
			cycle, schemaReason := completionGuardInputSchemaHasReferenceCycle(tool.InputSchema)
			if schemaReason != "" {
				return catalog, fmt.Sprintf("tools[%d].input_schema %s", index, schemaReason)
			}
			if cycle {
				return catalog, fmt.Sprintf("tools[%d].input_schema contains a circular local $ref and cannot be used by code execution", index)
			}
		}
		if knownServerTool && definition.Feature == completionGuardToolFeatureCodeExecution {
			explicitCodeExecutionType = toolType
		}
	}

	for serverName := range servers {
		if _, exists := catalog.MCPToolsetsByName[serverName]; !exists {
			return catalog, fmt.Sprintf("MCP server %q must be referenced by exactly one mcp_toolset", serverName)
		}
	}
	if !options.AllowAllDeferred &&
		catalog.Visibility.DeferredEnabledCount > 0 &&
		catalog.Visibility.ExplicitVisibleCount == 0 &&
		catalog.Visibility.DeclaredMCPVisibleCount == 0 &&
		!catalog.Visibility.DynamicMCPPossible {
		return catalog, "at least one tool must have defer_loading=false"
	}

	for _, entry := range catalog.Entries {
		definition, knownServerTool := completionGuardLookupServerToolDefinition(entry.Type)
		if knownServerTool && definition.Feature == completionGuardToolFeatureCodeExecution {
			legacy, modern, canCallTools := completionGuardCodeExecutionVersion(entry.Type)
			if !legacy && !modern {
				return catalog, fmt.Sprintf("unsupported code execution tool version %q", entry.Type)
			}
			completionGuardAddServerToolDefinitionCapabilities(
				catalog,
				definition,
				false,
				entry.DirectCallable,
				entry.CodeCallable,
			)
			if canCallTools {
				catalog.ProgrammaticToolCalling = true
			}
			continue
		}

		if entry.CodeCallable {
			catalog.ProgrammaticRequested = true
		}
		if entry.Kind == completionGuardToolKindServer {
			completionGuardAddServerToolDefinitionCapabilities(
				catalog,
				definition,
				false,
				entry.DirectCallable,
				entry.CodeCallable,
			)
		}
		if completionGuardDynamicWebTool(entry.Type) && entry.CodeCallable {
			if explicitCodeExecutionType == "" {
				catalog.ProgrammaticToolCalling = true
				implicitCodeExecution, _ := completionGuardLookupServerToolDefinition(
					"code_execution_20260120",
				)
				completionGuardAddServerToolDefinitionCapabilities(
					catalog,
					implicitCodeExecution,
					true,
					true,
					false,
				)
			} else {
				_, modern, canCallTools := completionGuardCodeExecutionVersion(explicitCodeExecutionType)
				if !modern || !canCallTools {
					return catalog, fmt.Sprintf(
						"tools[%d] dynamic filtering requires code_execution_20260120 or later",
						entry.SourceIndex,
					)
				}
			}
		}
	}

	for _, entry := range catalog.Entries {
		if !entry.CodeCallable || strings.HasPrefix(entry.Type, "code_execution_") || completionGuardDynamicWebTool(entry.Type) {
			continue
		}
		_, modern, canCallTools := completionGuardCodeExecutionVersion(explicitCodeExecutionType)
		if explicitCodeExecutionType == "" || !modern || !canCallTools {
			return catalog, fmt.Sprintf(
				"tools[%d] programmatic caller requires code_execution_20260120 or later",
				entry.SourceIndex,
			)
		}
	}
	return catalog, ""
}

func completionGuardCatalogClientTool(
	catalog *completionGuardNormalizedCatalog,
	name string,
) (ClaudeTool, bool) {
	if catalog == nil {
		return ClaudeTool{}, false
	}
	entry, exists := catalog.ClientToolsByName[strings.TrimSpace(name)]
	return entry.Declaration, exists
}

func completionGuardCatalogHasMCP(catalog *completionGuardNormalizedCatalog) bool {
	return catalog != nil && len(catalog.MCPToolsetsByName) > 0
}

func (catalog *completionGuardNormalizedCatalog) ClientTool(name string) (ClaudeTool, bool) {
	return completionGuardCatalogClientTool(catalog, name)
}

func (catalog *completionGuardNormalizedCatalog) ClientEntry(
	name string,
) (completionGuardCatalogTool, bool) {
	if catalog == nil {
		return completionGuardCatalogTool{}, false
	}
	entry, exists := catalog.ClientToolsByName[strings.TrimSpace(name)]
	return entry, exists
}

func (catalog *completionGuardNormalizedCatalog) ExplicitEntry(
	name string,
) (completionGuardCatalogTool, bool) {
	if catalog == nil {
		return completionGuardCatalogTool{}, false
	}
	entry, exists := catalog.ExplicitToolsByName[strings.TrimSpace(name)]
	return entry, exists
}

func (catalog *completionGuardNormalizedCatalog) UniqueExplicitNameForBase(base string) string {
	used := make(map[string]bool)
	if catalog != nil {
		used = make(map[string]bool, len(catalog.ExplicitToolsByName))
		for name := range catalog.ExplicitToolsByName {
			used[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	name := base
	for suffix := 2; used[strings.ToLower(name)]; suffix++ {
		name = fmt.Sprintf("%s_%d", base, suffix)
	}
	return name
}

func (catalog *completionGuardNormalizedCatalog) ExplicitTool(name string) (ClaudeTool, bool) {
	if catalog == nil {
		return ClaudeTool{}, false
	}
	entry, exists := catalog.ExplicitToolsByName[strings.TrimSpace(name)]
	return entry.Declaration, exists
}

func (catalog *completionGuardNormalizedCatalog) ServerCapability(
	name string,
) (completionGuardServerToolCapability, bool) {
	if catalog == nil {
		return completionGuardServerToolCapability{}, false
	}
	capability, exists := catalog.ServerCapabilitiesByName[strings.TrimSpace(name)]
	return capability, exists
}

func (catalog *completionGuardNormalizedCatalog) HasMCPServer(name string) bool {
	if catalog == nil {
		return false
	}
	_, exists := catalog.MCPServersByName[strings.TrimSpace(name)]
	return exists
}

func (catalog *completionGuardNormalizedCatalog) HasMCPToolset(name string) bool {
	if catalog == nil {
		return false
	}
	_, exists := catalog.MCPToolsetsByName[strings.TrimSpace(name)]
	return exists
}

type completionGuardMCPToolSearchCandidate struct {
	ServerName     string
	Enabled        bool
	Deferred       bool
	ExplicitConfig bool
	DynamicDefault bool
}

func completionGuardCatalogMCPToolSearchCandidates(
	catalog *completionGuardNormalizedCatalog,
	toolName string,
) []completionGuardMCPToolSearchCandidate {
	if catalog == nil {
		return nil
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil
	}
	names := make([]string, 0, len(catalog.MCPToolsetsByName))
	for serverName := range catalog.MCPToolsetsByName {
		names = append(names, serverName)
	}
	sort.Strings(names)
	candidates := make([]completionGuardMCPToolSearchCandidate, 0, len(names))
	for _, serverName := range names {
		toolset := catalog.MCPToolsetsByName[serverName]
		enabled, deferred, explicit := completionGuardMCPConfigEffective(toolset, toolName)
		if !enabled || !deferred {
			continue
		}
		candidates = append(candidates, completionGuardMCPToolSearchCandidate{
			ServerName:     serverName,
			Enabled:        enabled,
			Deferred:       deferred,
			ExplicitConfig: explicit,
			DynamicDefault: !explicit,
		})
	}
	return candidates
}
