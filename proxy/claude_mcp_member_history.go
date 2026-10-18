package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// completionGuardParseMCPPinnedMembers preserves absent tools (dynamic) versus
// an explicitly empty pinned list. Neither configs nor default_config declare
// a member of a pinned MCP toolset.
func completionGuardParseMCPPinnedMembers(extra map[string]json.RawMessage, path string) (map[string]bool, error) {
	raw, present := extra["tools"]
	if !present {
		return nil, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		// JSON [] is represented by a non-nil empty slice.
		if string(raw) != "[]" {
			return nil, fmt.Errorf("%s.tools must be a pinned tool array", path)
		}
	}
	members := make(map[string]bool, len(items))
	for i, item := range items {
		var name string
		if json.Unmarshal(item["name"], &name) != nil || strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return nil, fmt.Errorf("%s.tools[%d].name must be a non-empty tool name", path, i)
		}
		var schema map[string]interface{}
		if json.Unmarshal(item["input_schema"], &schema) != nil || schema == nil {
			return nil, fmt.Errorf("%s.tools[%d].input_schema must be an object", path, i)
		}
		if members[name] {
			return nil, fmt.Errorf("%s.tools[%d].name duplicates pinned tool %q", path, i, name)
		}
		members[name] = true
	}
	return members, nil
}

func claudeMCPListingMembers(block map[string]interface{}, path string) (map[string]bool, error) {
	items, ok := block["tools"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("%s.tools must be an array", path)
	}
	members := make(map[string]bool, len(items))
	for i, value := range items {
		item, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s.tools[%d] must be an object", path, i)
		}
		name, ok := item["name"].(string)
		if !ok || strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || members[name] {
			return nil, fmt.Errorf("%s.tools[%d] has an invalid or duplicate name", path, i)
		}
		if schema, ok := item["input_schema"].(map[string]interface{}); !ok || schema == nil {
			return nil, fmt.Errorf("%s.tools[%d].input_schema must be an object", path, i)
		}
		members[name] = true
	}
	return members, nil
}

func claudeMCPMemberAllowed(toolset completionGuardMCPToolset, listing map[string]bool, name string) bool {
	if toolset.PinnedMembers != nil && !toolset.PinnedMembers[name] {
		return false
	}
	if toolset.PinnedMembers == nil && listing != nil && !listing[name] {
		return false
	}
	enabled, _, _ := completionGuardMCPConfigEffective(toolset, name)
	return enabled
}

func claudeResetMCPMemberLoad(loaded map[string]bool, server string) {
	prefix := server + "\x00"
	for key := range loaded {
		if strings.HasPrefix(key, prefix) {
			delete(loaded, key)
		}
	}
}

// Claude's MCP connector can expose a tool as {server}_{name} in a built-in
// search reference. Resolve it by comparing complete names derived from known
// server/member pairs; never split on underscores (either component may contain
// them). A reference is not an authorization to invent a dynamic MCP member.
type claudeMCPReferenceIdentity struct {
	Server   string
	Member   string
	Deferred bool
}

type claudeMCPReferenceStatus uint8

const (
	claudeMCPReferenceUnknown claudeMCPReferenceStatus = iota
	claudeMCPReferenceResolved
	claudeMCPReferenceAmbiguous
	claudeMCPReferenceUnavailable
)

func claudeResolveMCPToolSearchReference(
	name string, catalog *completionGuardNormalizedCatalog,
	inline map[string]completionGuardMCPToolset, active map[string]bool,
	listed map[string]map[string]bool,
) (claudeMCPReferenceIdentity, claudeMCPReferenceStatus) {
	if name == "" || catalog == nil {
		return claudeMCPReferenceIdentity{}, claudeMCPReferenceUnknown
	}
	// Explicit tools have their own tool_reference namespace. When an exact
	// explicit declaration exists, it must not load an MCP tool as a side effect.
	if _, explicit := catalog.ExplicitToolsByName[name]; explicit {
		return claudeMCPReferenceIdentity{}, claudeMCPReferenceUnknown
	}
	sets := make(map[string]completionGuardMCPToolset, len(catalog.MCPToolsetsByName)+len(inline))
	for server, toolset := range catalog.MCPToolsetsByName {
		sets[server] = toolset
	}
	for server, toolset := range inline {
		sets[server] = toolset
	}
	servers := make([]string, 0, len(sets))
	for server := range sets {
		servers = append(servers, server)
	}
	sort.Strings(servers)
	// A pinned or historically listed member is evidence of a concrete MCP
	// identity. Exact and {server}_{member} spellings are *not* competing
	// precedence tiers: both can denote different documented members. Never
	// silently select one just because its spelling is exact.
	//
	// An unpinned/unlisted server supplies no evidence for constructing a
	// composed alias. Preserve the legacy unique-dynamic-member fallback only
	// when no documented identity matches; do not let it shadow one.
	documented := make(map[string]claudeMCPReferenceIdentity)
	dynamic := make(map[string]claudeMCPReferenceIdentity)
	for _, server := range servers {
		if active != nil && !active[server] {
			continue
		}
		toolset := sets[server]
		members := toolset.PinnedMembers
		if members == nil {
			members = listed[server]
		}
		if members == nil {
			_, deferred, _ := completionGuardMCPConfigEffective(toolset, name)
			dynamic[server+"\x00"+name] = claudeMCPReferenceIdentity{server, name, deferred}
			continue
		}
		for member := range members {
			if member != name && server+"_"+member != name {
				continue
			}
			_, deferred, _ := completionGuardMCPConfigEffective(toolset, member)
			documented[server+"\x00"+member] = claudeMCPReferenceIdentity{server, member, deferred}
		}
	}
	candidates := documented
	if len(candidates) == 0 {
		candidates = dynamic
	}
	if len(candidates) > 1 {
		return claudeMCPReferenceIdentity{}, claudeMCPReferenceAmbiguous
	}
	for _, match := range candidates {
		if !match.Deferred || !claudeMCPMemberAllowed(sets[match.Server], listed[match.Server], match.Member) {
			return claudeMCPReferenceIdentity{}, claudeMCPReferenceUnavailable
		}
		return match, claudeMCPReferenceResolved
	}
	return claudeMCPReferenceIdentity{}, claudeMCPReferenceUnknown
}

// Load only the uniquely resolved deferred member in the active request epoch.
// Both historical replay and response classification use this same resolver.
func claudeLoadUnambiguousMCPReference(name string, catalog *completionGuardNormalizedCatalog,
	inline map[string]completionGuardMCPToolset, active map[string]bool,
	listed map[string]map[string]bool, loaded map[string]bool) (claudeMCPReferenceIdentity, claudeMCPReferenceStatus) {
	match, status := claudeResolveMCPToolSearchReference(name, catalog, inline, active, listed)
	if status == claudeMCPReferenceResolved {
		loaded[match.Server+"\x00"+match.Member] = true
	}
	return match, status
}

func claudeBuiltinToolSearchNames(block map[string]interface{}) []string {
	content, _ := block["content"].(map[string]interface{})
	if content["type"] != "tool_search_tool_search_result" {
		return nil
	}
	items, _ := content["tool_references"].([]interface{})
	names := make([]string, 0, len(items))
	for _, value := range items {
		ref, _ := value.(map[string]interface{})
		name, _ := ref["tool_name"].(string)
		if ref["type"] == "tool_reference" && name != "" {
			names = append(names, name)
		}
	}
	return names
}

func claudeMCPMemberExplicitlyRemoved(members map[string]bool, server, member string) bool {
	active, defined := members[server+"\x00"+member]
	return defined && !active
}

// Response classification is not allowed to mutate the request's catalog: a
// tool-search result may load a deferred MCP tool later in the same response,
// and overlapping response/stream consumers can inspect the original plan.
func completionGuardResponseMCPPlan(plan agentCompletionGuardPlan) agentCompletionGuardPlan {
	if plan.Catalog == nil {
		return plan
	}
	catalog := *plan.Catalog
	if catalog.CustomFinalState != nil {
		cloned := make(map[string]completionGuardCustomToolState, len(catalog.CustomFinalState))
		for name, state := range catalog.CustomFinalState {
			cloned[name] = state
		}
		catalog.CustomFinalState = cloned
	}
	if catalog.MCPFinalAvailability == nil {
		plan.Catalog = &catalog
		return plan
	}
	final := *catalog.MCPFinalAvailability
	final.Loaded = make(map[string]bool, len(final.Loaded))
	for key, value := range catalog.MCPFinalAvailability.Loaded {
		final.Loaded[key] = value
	}
	final.Listings = make(map[string]map[string]bool, len(final.Listings))
	for key, value := range catalog.MCPFinalAvailability.Listings {
		final.Listings[key] = value
	}
	catalog.MCPFinalAvailability = &final
	plan.Catalog = &catalog
	return plan
}

func completionGuardResponseMCPListing(block ClaudeContentBlock, plan agentCompletionGuardPlan) string {
	if plan.Catalog == nil || plan.Catalog.MCPFinalAvailability == nil {
		return "mcp_listing_without_catalog"
	}
	server := completionGuardContentBlockExtraString(block, "mcp_server_name")
	if _, known := plan.Catalog.MCPToolsetsByName[server]; !known || !plan.Catalog.MCPFinalAvailability.Active[server] {
		return "mcp_listing_without_active_toolset"
	}
	var tools []interface{}
	if err := json.Unmarshal(block.Extra["tools"], &tools); err != nil || tools == nil {
		return "invalid_mcp_tool_listing"
	}
	members, err := claudeMCPListingMembers(map[string]interface{}{"tools": tools}, "response.mcp_tool_listing")
	if err != nil {
		return "invalid_mcp_tool_listing"
	}
	plan.Catalog.MCPFinalAvailability.Listings[server] = members
	return ""
}
