package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// claudeTemporalToolTypeIdentity compares an Anthropic tool's actual family,
// rather than its client/server execution kind. Known dated releases of the
// same built-in tool share an identity; unrelated built-ins and custom tools do
// not. Unknown types retain their exact identifier and are never guessed.
func claudeTemporalToolTypeIdentity(toolType string) string {
	toolType = strings.ToLower(strings.TrimSpace(toolType))
	switch toolType {
	case "", "function", "custom":
		return "custom"
	}
	if server, ok := completionGuardLookupServerToolDefinition(toolType); ok {
		return "server:" + server.ExpectedName
	}
	for _, family := range []string{"bash", "computer", "text_editor", "str_replace_based_edit_tool", "memory"} {
		if toolType == family {
			return "client:" + family
		}
		if suffix, found := strings.CutPrefix(toolType, family+"_"); found && len(suffix) == 8 {
			dated := true
			for _, ch := range suffix {
				if ch < '0' || ch > '9' {
					dated = false
					break
				}
			}
			if dated {
				return "client:" + family
			}
		}
	}
	return "type:" + toolType
}

// claudeTemporalServerToolDeclared checks the actual declaration which grants
// a server_tool_use wire name. A future declaration must not authorize a past
// server call, including a tool whose wire name differs from its tool name.
func claudeTemporalServerToolDeclared(wireName string, declared map[string]completionGuardCatalogTool, active map[string]bool) bool {
	for name, entry := range declared {
		if entry.Kind != completionGuardToolKindServer || !active[name] {
			continue
		}
		if def, ok := completionGuardLookupServerToolDefinition(entry.Type); ok {
			for _, wire := range def.WireTools {
				if wire.Name == wireName {
					return true
				}
			}
		}
	}
	return false
}

// claudeHistoryNeedsToolAvailability enforces deferred custom tool loading even
// in requests without MCP connectors or inline changes.
func claudeHistoryNeedsToolAvailability(messages []ClaudeMessage, catalog *completionGuardNormalizedCatalog) bool {
	if catalog == nil {
		return false
	}
	if claudeRequestHasMidConversationToolChangesRaw(messages) || len(catalog.MCPToolsetsByName) > 0 {
		return true
	}
	for _, entry := range catalog.ExplicitToolsByName {
		if entry.Deferred {
			return true
		}
	}
	return false
}

// A custom Tool Search result can load explicit tools only when its tool_result
// belongs to a client tool_use in the immediately preceding assistant turn.
// The historical graph performs full correlation later; this check prevents
// orphan search references from granting capability during the earlier replay.
func claudeHistoricalSearchResultSource(messages []ClaudeMessage, messageIndex int, callID string) bool {
	if messageIndex == 0 || strings.TrimSpace(callID) == "" ||
		!strings.EqualFold(strings.TrimSpace(messages[messageIndex-1].Role), "assistant") {
		return false
	}
	for _, block := range claudeContentBlocks(messages[messageIndex-1].Content) {
		if block["type"] == "tool_use" && block["id"] == callID {
			return true
		}
	}
	return false
}

// Collect client search references without interpreting any custom-search
// result as an MCP reference. Never change availability on a partial result.
func claudeCustomToolSearchNames(block map[string]interface{}) []string {
	if block["type"] != "tool_result" {
		return nil
	}
	content, ok := block["content"].([]interface{})
	if !ok {
		return nil
	}
	var names []string
	for _, item := range content {
		ref, ok := item.(map[string]interface{})
		if !ok || ref["type"] != "tool_reference" {
			continue
		}
		name, _ := ref["tool_name"].(string)
		names = append(names, strings.TrimSpace(name))
	}
	return names
}

// Historical search identities are recorded at the event's definition epoch.
// The Completion Guard must never reinterpret them using a later inline tool
// definition or a final-state removal.
func claudeRecordHistoricalCustomReference(catalog *completionGuardNormalizedCatalog, callID, name string, identity completionGuardHistoricalCustomReference) {
	if catalog == nil || callID == "" {
		return
	}
	if catalog.HistoricalCustomReferences == nil {
		catalog.HistoricalCustomReferences = make(map[string]map[string]completionGuardHistoricalCustomReference)
	}
	if catalog.HistoricalCustomReferences[callID] == nil {
		catalog.HistoricalCustomReferences[callID] = make(map[string]completionGuardHistoricalCustomReference)
	}
	catalog.HistoricalCustomReferences[callID][name] = identity
}

// validateClaudeInlineToolHistory replays tool availability in message order.
// The public tool catalog remains a request-local declaration index; this
// function never changes wire history or retroactively applies a new schema to
// an earlier call. Inline definitions are indexed for the completion guard only
// after the temporal replay has validated the call sites.
func validateClaudeInlineToolHistory(req *ClaudeRequest, catalog *completionGuardNormalizedCatalog, betas claudeBetaSet) *claudeTransportValidationError {
	if req == nil || catalog == nil {
		return nil
	}
	if !claudeHistoryNeedsToolAvailability(req.Messages, catalog) {
		return nil
	}
	declared := make(map[string]completionGuardCatalogTool, len(catalog.ExplicitToolsByName))
	active := make(map[string]bool, len(catalog.ExplicitToolsByName))
	offered := make(map[string]bool, len(catalog.ExplicitToolsByName))
	epochs := make(map[string]uint64, len(catalog.ExplicitToolsByName))
	for name, entry := range catalog.ExplicitToolsByName {
		declared[name] = entry
		active[name] = !entry.Deferred
		offered[name] = true
	}
	knownMCP := make(map[string]bool, len(catalog.MCPToolsetsByName))
	activeMCP := make(map[string]bool, len(catalog.MCPToolsetsByName))
	activeMCPMember := make(map[string]bool)
	loadedMCPMember := make(map[string]bool)
	listedMCP := make(map[string]map[string]bool)
	for name := range catalog.MCPToolsetsByName {
		knownMCP[name] = true
		activeMCP[name] = true
	}
	inline := betas.Contains("inline-tools-2026-09-15")
	mcpBeta := betas.Contains("mcp-client-2026-09-15")
	introduced := make(map[string]completionGuardCatalogTool)
	inlineMCP := make(map[string]completionGuardMCPToolset)
	invalid := func(message string) *claudeTransportValidationError {
		return &claudeTransportValidationError{Code: "invalid_request_error", Message: message}
	}
	for mi, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		for bi, block := range claudeContentBlocks(message.Content) {
			path := fmt.Sprintf("messages[%d].content[%d]", mi, bi)
			kind, _ := block["type"].(string)
			switch {
			case role == "system" && (kind == "tool_addition" || kind == "tool_removal"):
				tool, ok := block["tool"].(map[string]interface{})
				if !ok || tool == nil {
					return invalid(path + ".tool must be an object")
				}
				referenceType, _ := tool["type"].(string)
				switch referenceType {
				case "tool_definition":
					if kind != "tool_addition" || !inline {
						return invalid(path + ".tool_definition requires tool_addition and anthropic-beta inline-tools-2026-09-15")
					}
					definition, ok := tool["definition"].(map[string]interface{})
					if !ok || definition == nil {
						return invalid(path + ".tool.definition must be an object")
					}
					raw, err := json.Marshal(definition)
					if err != nil {
						return invalid(path + ".tool.definition cannot be encoded")
					}
					var item ClaudeTool
					if err := json.Unmarshal(raw, &item); err != nil {
						return invalid(path + ".tool.definition is malformed")
					}
					if item.Type == "mcp_toolset" {
						if !mcpBeta {
							return invalid(path + " inline mcp_toolset requires anthropic-beta mcp-client-2026-09-15")
						}
						server, _ := definition["mcp_server_name"].(string)
						if strings.TrimSpace(server) == "" {
							return invalid(path + ".tool.definition.mcp_server_name is required")
						}
						if _, exists := catalog.MCPServersByName[server]; !exists {
							return invalid(path + " references undeclared MCP server " + server)
						}
						defaultConfig, _, err := completionGuardParseMCPToolConfigField(item.Extra, "default_config", path+".tool.definition.default_config")
						if err != nil {
							return invalid(err.Error())
						}
						configs := make(map[string]completionGuardMCPToolConfig)
						if raw, present := item.Extra["configs"]; present {
							var values map[string]json.RawMessage
							if json.Unmarshal(raw, &values) != nil || values == nil {
								return invalid(path + ".tool.definition.configs must be an object")
							}
							for name, value := range values {
								if strings.TrimSpace(name) == "" {
									return invalid(path + ".tool.definition.configs has an empty tool name")
								}
								config, err := completionGuardParseMCPToolConfigRaw(value, path+".tool.definition.configs."+name)
								if err != nil {
									return invalid(err.Error())
								}
								configs[name] = config
							}
						}
						pinned, pinnedErr := completionGuardParseMCPPinnedMembers(item.Extra, path+".tool.definition")
						if pinnedErr != nil {
							return invalid(pinnedErr.Error())
						}
						knownMCP[server], activeMCP[server] = true, true
						inlineMCP[server] = completionGuardMCPToolset{ServerName: server, SourceIndex: -1, DefaultConfig: defaultConfig, Configs: configs, PinnedMembers: pinned}
						claudeResetMCPMemberLoad(loadedMCPMember, server)
						claudeResetMCPMemberLoad(activeMCPMember, server)
						delete(listedMCP, server)
						continue
					}
					name := strings.TrimSpace(item.Name)
					if name == "" || !completionGuardValidToolName(name) {
						return invalid(path + ".tool.definition.name must be a valid non-empty tool name")
					}
					deferred, _, deferredErr := completionGuardRawBoolField(item.Extra, "defer_loading")
					if deferredErr != nil {
						return invalid(path + ".tool.definition." + deferredErr.Error())
					}
					if item.Type == "" || item.Type == "custom" || item.Type == "function" {
						if _, ok := item.InputSchema.(map[string]interface{}); !ok {
							return invalid(path + ".tool.definition.input_schema must be an object")
						}
					}
					if previous, exists := declared[name]; exists {
						if claudeTemporalToolTypeIdentity(previous.Type) != claudeTemporalToolTypeIdentity(item.Type) {
							return invalid(path + ".tool.definition cannot change an existing tool name to a different tool type")
						}
					}
					if catalog.NativeToolsets[name] {
						return invalid(path + ".tool.definition.name conflicts with a declared native toolset")
					}
					entry := completionGuardCatalogTool{SourceIndex: -1, Name: name, Type: item.Type, Kind: completionGuardToolKindClient,
						Deferred: deferred, DirectCallable: true, InputSchema: item.InputSchema, Declaration: item}
					if !claudeToolCanEmitClientToolUse(item) {
						entry.Kind = completionGuardToolKindServer
						entry.DirectCallable = false
					}
					declared[name] = entry
					epochs[name]++
					active[name] = !entry.Deferred
					offered[name] = true
					introduced[name] = entry
					if entry.Kind == completionGuardToolKindServer {
						if definition, known := completionGuardLookupServerToolDefinition(item.Type); known {
							completionGuardAddServerToolDefinitionCapabilities(catalog, definition, false, true, false)
							if definition.Feature == completionGuardToolFeatureToolSearch {
								catalog.HasToolSearch = true
							}
						}
					}
				case "tool_reference":
					name, _ := tool["name"].(string)
					if strings.TrimSpace(name) == "" {
						return invalid(path + ".tool.name must be non-empty")
					}
					if _, exists := declared[name]; !exists {
						return invalid(path + " references undeclared tool " + name)
					}
					epochs[name]++
					offered[name] = kind == "tool_addition"
					active[name] = kind == "tool_addition"
				case "mcp_tool_reference", "mcp_toolset_reference":
					server, _ := tool["server_name"].(string)
					if strings.TrimSpace(server) == "" || !knownMCP[server] {
						return invalid(path + " references undeclared MCP server")
					}
					if referenceType == "mcp_tool_reference" {
						name, _ := tool["name"].(string)
						if strings.TrimSpace(name) == "" {
							return invalid(path + ".tool.name must be non-empty")
						}
						toolset, ok := inlineMCP[server]
						if !ok {
							toolset, ok = catalog.MCPToolsetsByName[server]
						}
						if ok {
							if !claudeMCPMemberAllowed(toolset, listedMCP[server], name) {
								return invalid(path + " references unavailable MCP tool " + name)
							}
						}
						activeMCPMember[server+"\x00"+name] = kind == "tool_addition"
						loadedMCPMember[server+"\x00"+name] = kind == "tool_addition"
					} else {
						activeMCP[server] = kind == "tool_addition"
						// A reactivated toolset starts a fresh member epoch. An old
						// removal or search result must not grant/deny a new epoch.
						claudeResetMCPMemberLoad(loadedMCPMember, server)
						claudeResetMCPMemberLoad(activeMCPMember, server)
						delete(listedMCP, server)
					}
				default:
					return invalid(path + ".tool.type must be a tool reference or inline tool_definition")
				}
			case role == "assistant" && kind == "mcp_tool_listing":
				server, _ := block["mcp_server_name"].(string)
				if !knownMCP[server] || !activeMCP[server] {
					return invalid(path + " lists an MCP toolset not active in history")
				}
				listing, listingErr := claudeMCPListingMembers(block, path)
				if listingErr != nil {
					return invalid(listingErr.Error())
				}
				listedMCP[server] = listing
			case role == "assistant" && kind == "tool_search_tool_result":
				callID, _ := block["tool_use_id"].(string)
				// Resolve the entire search result before granting any member's
				// loaded state. One ambiguous identity must not silently load a
				// different reference from the same result or disagree with the
				// Completion Guard's stricter reference validation.
				resolved := make(map[string]claudeMCPReferenceIdentity)
				explicitLoaded := make(map[string]completionGuardHistoricalCustomReference)
				for _, name := range claudeBuiltinToolSearchNames(block) {
					if entry, found := declared[name]; found {
						if !offered[name] {
							return invalid(path + " references a removed explicit tool: " + name)
						}
						if !entry.Deferred {
							return invalid(path + " built-in tool search requires defer_loading=true: " + name)
						}
						explicitLoaded[name] = completionGuardHistoricalCustomReference{Deferred: entry.Deferred, Epoch: epochs[name]}
						continue
					}
					match, status := claudeResolveMCPToolSearchReference(name, catalog, inlineMCP, activeMCP, listedMCP)
					switch status {
					case claudeMCPReferenceAmbiguous:
						return invalid(path + " has an ambiguous MCP tool reference: " + name)
					case claudeMCPReferenceUnavailable:
						return invalid(path + " references an unavailable MCP tool: " + name)
					case claudeMCPReferenceResolved:
						resolved[name] = match
					case claudeMCPReferenceUnknown:
						return invalid(path + " has a tool_reference without a declared tool: " + name)
					}
				}
				for name, historical := range explicitLoaded {
					active[name] = true
					claudeRecordHistoricalCustomReference(catalog, callID, name, historical)
				}
				for name, match := range resolved {
					loadedMCPMember[match.Server+"\x00"+match.Member] = true
					if callID != "" {
						if catalog.HistoricalMCPReferences == nil {
							catalog.HistoricalMCPReferences = make(map[string]map[string]claudeMCPReferenceIdentity)
						}
						if catalog.HistoricalMCPReferences[callID] == nil {
							catalog.HistoricalMCPReferences[callID] = make(map[string]claudeMCPReferenceIdentity)
						}
						catalog.HistoricalMCPReferences[callID][name] = match
					}
				}
			case role == "user" && kind == "tool_result":
				names := claudeCustomToolSearchNames(block)
				if len(names) == 0 {
					continue
				}
				callID, _ := block["tool_use_id"].(string)
				if !claudeHistoricalSearchResultSource(req.Messages, mi, callID) {
					return invalid(path + " tool references require a matching preceding client tool_use")
				}
				for _, name := range names {
					entry, exists := declared[name]
					if name == "" || !exists || entry.Kind != completionGuardToolKindClient || !offered[name] {
						return invalid(path + " tool_reference has no active explicit tool definition: " + name)
					}
				}
				for _, name := range names {
					active[name] = true
					claudeRecordHistoricalCustomReference(catalog, callID, name,
						completionGuardHistoricalCustomReference{Deferred: declared[name].Deferred, Epoch: epochs[name]})
				}
			case role == "assistant" && kind == "tool_use":
				if _, native := block["toolset_name"]; native {
					continue
				}
				name, _ := block["name"].(string)
				if name != "" && !active[name] {
					return invalid(path + " uses a tool not active at this point in history: " + name)
				}
			case role == "assistant" && kind == "server_tool_use":
				name, _ := block["name"].(string)
				if name != "" && !claudeTemporalServerToolDeclared(name, declared, active) {
					return invalid(path + " uses a server tool not active at this point in history: " + name)
				}
			case role == "assistant" && kind == "mcp_tool_use":
				server, _ := block["server_name"].(string)
				if server != "" {
					if !knownMCP[server] || !activeMCP[server] {
						return invalid(path + " uses an MCP toolset not active at this point in history: " + server)
					}
					name, _ := block["name"].(string)
					if active, checked := activeMCPMember[server+"\x00"+name]; checked && !active {
						return invalid(path + " uses a removed MCP tool")
					}
					toolset, ok := inlineMCP[server]
					if !ok {
						toolset, ok = catalog.MCPToolsetsByName[server]
					}
					if ok {
						if !claudeMCPMemberAllowed(toolset, listedMCP[server], name) {
							return invalid(path + " uses an unavailable MCP tool")
						}
						_, deferred, _ := completionGuardMCPConfigEffective(toolset, name)
						if deferred && !loadedMCPMember[server+"\x00"+name] {
							return invalid(path + " uses an MCP tool before it was loaded")
						}
					}
				}
			}
		}
	}
	// The guard receives a request-local union only for lookup of inline tool
	// declarations. Historical availability was checked above before publication.
	// Preserve top-level catalog entry order and counts; no synthetic wire tools.
	for server, toolset := range inlineMCP {
		catalog.MCPToolsetsByName[server] = toolset
	}
	for name, entry := range introduced {
		catalog.ExplicitToolsByName[name] = entry
		if entry.Kind == completionGuardToolKindClient {
			catalog.ClientToolsByName[name] = entry
		} else {
			delete(catalog.ClientToolsByName, name)
		}
	}
	catalog.CustomFinalState = make(map[string]completionGuardCustomToolState, len(declared))
	for name, entry := range declared {
		catalog.CustomFinalState[name] = completionGuardCustomToolState{
			Offered: offered[name], Loaded: active[name], Deferred: entry.Deferred, Epoch: epochs[name],
		}
	}
	// This snapshot is only for future response/pending-tool checks. Do not
	// use it to retroactively validate calls in the request history.
	catalog.MCPFinalAvailability = &completionGuardMCPFinalAvailability{
		Active:   activeMCP,
		Members:  activeMCPMember,
		Loaded:   loadedMCPMember,
		Listings: listedMCP,
	}
	catalog.TemporalHistoryValidated = true
	return nil
}

// claudeHasInlineMCPDefinition permits an MCP connection to be declared now
// and its toolset to become available later in a mid-conversation message.
func claudeHasInlineMCPDefinition(messages []ClaudeMessage, server string) bool {
	for _, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "system") {
			continue
		}
		for _, block := range claudeContentBlocks(message.Content) {
			if block["type"] != "tool_addition" {
				continue
			}
			tool, _ := block["tool"].(map[string]interface{})
			definition, _ := tool["definition"].(map[string]interface{})
			if tool["type"] == "tool_definition" && definition["type"] == "mcp_toolset" && definition["mcp_server_name"] == server {
				return true
			}
		}
	}
	return false
}
