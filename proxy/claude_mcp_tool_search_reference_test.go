package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A built-in tool-search reference may use the MCP connector's composed
// {server}_{name} identity, while mcp_tool_use retains separate fields.
func TestMCPToolSearchQualifiedReferenceLoadsCorrectDeferredMember(t *testing.T) {
	tools := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	for _, reference := range []string{"events", "calendar_events"} {
		t.Run(reference, func(t *testing.T) {
			messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage(reference) + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"done"}]`
			requireMCPPlan(t, mcpMemberTestBody(tools, messages), true)
		})
	}
}

func mcpQualifiedSearchMessage(name string) string {
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"server_tool_use","id":"search-1","name":"tool_search_tool_regex","input":{"pattern":"events"}},{"type":"tool_search_tool_result","tool_use_id":"search-1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":%q}]}}]}`, name)
}

func TestMCPToolSearchQualifiedReferenceRejectsUnknownAndDisabledMembers(t *testing.T) {
	base := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}`
	for _, tc := range []struct {
		name      string
		tools     string
		reference string
		want      bool
	}{
		{"unknown_dynamic_composed", base + `}]`, "calendar_events", false},
		{"wrong_member", base + `,"tools":[{"name":"files","input_schema":{"type":"object"}}]}]`, "calendar_events", false},
		{"disabled_member", base + `,"configs":{"events":{"enabled":false}},"tools":[{"name":"events","input_schema":{"type":"object"}}]}]`, "calendar_events", false},
		{"unverified_mcp_double_underscore", base + `,"tools":[{"name":"events","input_schema":{"type":"object"}}]}]`, "mcp__calendar__events", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage(tc.reference) + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"done"}]`
			requireMCPPlan(t, mcpMemberTestBody(tc.tools, messages), tc.want)
		})
	}
}

func TestMCPToolSearchQualifiedReferenceDoesNotGrantOtherServer(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	tools := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"storage","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	search := mcpQualifiedSearchMessage("calendar_events")
	messages := `[ {"role":"user","content":"go"},` + search + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"done"}]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, true)
	// The exact same search reference must not load storage.events.
	storageUse := strings.Replace(mcpHistoryUse("events"), `"server_name":"calendar"`, `"server_name":"storage"`, 1)
	messages = strings.Replace(messages, mcpHistoryUse("events"), storageUse, 1)
	body = fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, false)
}

func TestMCPToolSearchAmbiguousComposedNameNeverGrantsMember(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"a_b","url":"https://example.com/one"},{"type":"url","name":"a","url":"https://example.com/two"}],`
	tools := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"a_b","tools":[{"name":"c","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"a","tools":[{"name":"b_c","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	for _, tc := range []struct{ server, member string }{{"a_b", "c"}, {"a", "b_c"}} {
		use := fmt.Sprintf(`{"role":"assistant","content":[{"type":"mcp_tool_use","id":"m1","server_name":%q,"name":%q,"input":{}},{"type":"mcp_tool_result","tool_use_id":"m1","content":[]}]}`, tc.server, tc.member)
		messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage("a_b_c") + `,` + use + `,{"role":"user","content":"done"}]`
		body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
		requireMCPPlan(t, body, false)
	}
}

func TestMCPToolSearchExplicitToolWinsOverQualifiedMCPAlias(t *testing.T) {
	tools := `[ {"name":"calendar_events","input_schema":{"type":"object"},"defer_loading":true}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage("calendar_events") + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"done"}]`
	requireMCPPlan(t, mcpMemberTestBody(tools, messages), false)
}

func TestMCPToolSearchQualifiedReferenceLoadsFromHistoricalListing(t *testing.T) {
	tools := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}} ]`
	listing := `{"role":"assistant","content":[{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}]}`
	messages := `[ {"role":"user","content":"go"},` + listing + `,` + mcpQualifiedSearchMessage("calendar_events") + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"done"}]`
	requireMCPPlan(t, mcpMemberTestBody(tools, messages), true, "mcp-client-2026-09-15")
	// A listing after the search/call must never retroactively resolve the alias.
	messages = `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage("calendar_events") + `,` + mcpHistoryUse("events") + `,` + listing + `,{"role":"user","content":"done"}]`
	requireMCPPlan(t, mcpMemberTestBody(tools, messages), false, "mcp-client-2026-09-15")
}

func TestMCPToolSearchQualifiedReferenceLoadsInSameResponse(t *testing.T) {
	tools := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	body := mcpMemberTestBody(tools, `[{"role":"user","content":"go"}]`)
	plan, err := inlineProtocolPlan(body)
	if err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatal(guard.ValidationReason)
	}
	content := `[{"type":"server_tool_use","id":"search-1","name":"tool_search_tool_regex","input":{"pattern":"events"}},{"type":"tool_search_tool_result","tool_use_id":"search-1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"calendar_events"}]}},{"type":"mcp_tool_use","id":"mcp-1","server_name":"calendar","name":"events","input":{}},{"type":"mcp_tool_result","tool_use_id":"mcp-1","content":[]},{"type":"text","text":"done"}]`
	responseWire := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":%s}`, content)
	var response ClaudeResponse
	if err := json.Unmarshal([]byte(responseWire), &response); err != nil {
		t.Fatal(err)
	}
	outcome := classifyCompletionGuardResponseWithRequestHistory(&response, guard, plan.Request)
	if !outcome.Accepted {
		t.Fatalf("qualified same-response MCP member rejected: %+v", outcome)
	}
	if completionGuardMCPToolAllowed(plan.Catalog, "calendar", "events") {
		t.Fatal("response mutated request-local catalog")
	}
}

func TestMCPToolSearchQualifiedReferenceWithOneHistoricalListingAndOtherDynamicServer(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	tools := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"storage","default_config":{"defer_loading":true}} ]`
	listing := `{"role":"assistant","content":[{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}]}`
	messages := `[ {"role":"user","content":"go"},` + listing + `,` + mcpQualifiedSearchMessage("calendar_events") + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"done"}]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, true, "mcp-client-2026-09-15")
}

// Compare complete composed names rather than splitting on underscores.
func TestMCPToolSearchQualifiedReferencePreservesUnderscoresInBothComponents(t *testing.T) {
	server := `"mcp_servers":[{"type":"url","name":"calendar_service","url":"https://example.com/calendar"}],`
	tools := `[ {"name":"echo","input_schema":{"type":"object"}}, {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar_service","tools":[{"name":"events_search","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	use := `{"role":"assistant","content":[{"type":"mcp_tool_use","id":"m1","server_name":"calendar_service","name":"events_search","input":{}},{"type":"mcp_tool_result","tool_use_id":"m1","content":[]}]}`
	messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage("calendar_service_events_search") + `,` + use + `,{"role":"user","content":"done"}]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, server, tools, messages)
	requireMCPPlan(t, body, true)
}
