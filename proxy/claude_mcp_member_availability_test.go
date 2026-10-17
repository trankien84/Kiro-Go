package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const mcpTestServer = `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/mcp"}],`

func mcpMemberTestBody(tools, messages string) string {
	return fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, mcpTestServer, tools, messages)
}

func mcpHistoryUse(member string) string {
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"mcp_tool_use","id":"m1","server_name":"calendar","name":%q,"input":{}},{"type":"mcp_tool_result","tool_use_id":"m1","content":[]}]}`, member)
}

func requireMCPPlan(t *testing.T, body string, want bool, betas ...string) {
	t.Helper()
	plan, err := inlineProtocolPlan(body, betas...)
	if (err == nil) != want {
		t.Fatalf("planner: wanted accept=%v, error=%v", want, err)
	}
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlan(&req, false)
	if (guard.ValidationReason == "") != want {
		t.Fatalf("standalone guard: wanted accept=%v, reason=%q", want, guard.ValidationReason)
	}
	if want && plan.Catalog == nil {
		t.Fatal("accepted planner returned nil catalog")
	}
}

func TestMCPMemberPinnedCatalogRejectsUnknownAndEmpty(t *testing.T) {
	messages := func(member string) string {
		return `[ {"role":"user","content":"go"},` + mcpHistoryUse(member) + `,{"role":"user","content":"done"}]`
	}
	for _, tc := range []struct {
		tools    string
		member   string
		accepted bool
	}{
		{`[{"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}]`, "events", true},
		{`[{"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}]`, "DELETE_ALL", false},
		{`[{"type":"mcp_toolset","mcp_server_name":"calendar","tools":[]}]`, "events", false},
		{`[{"type":"mcp_toolset","mcp_server_name":"calendar"}]`, "dynamic", true},
	} {
		t.Run(fmt.Sprintf("%s_%t", tc.member, tc.accepted), func(t *testing.T) {
			requireMCPPlan(t, mcpMemberTestBody(tc.tools, messages(tc.member)), tc.accepted)
		})
	}
}

func TestMCPMemberPinnedListDuplicateAndInlineValidation(t *testing.T) {
	duplicate := `[{"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{}},{"name":"events","input_schema":{}}]}]`
	requireMCPPlan(t, mcpMemberTestBody(duplicate, `[{"role":"user","content":"hi"}]`), false)
	inline := `[{"name":"echo","input_schema":{"type":"object"}}]`
	prefix := `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}}}]},`
	requireMCPPlan(t, mcpMemberTestBody(inline, prefix+mcpHistoryUse("events")+` ,{"role":"user","content":"done"}]`), true, "inline-tools-2026-09-15", "mcp-client-2026-09-15")
	requireMCPPlan(t, mcpMemberTestBody(inline, prefix+mcpHistoryUse("delete_all")+` ,{"role":"user","content":"done"}]`), false, "inline-tools-2026-09-15", "mcp-client-2026-09-15")
}

func TestMCPMemberListingNotAppliedRetroactively(t *testing.T) {
	tools := `[{"type":"mcp_toolset","mcp_server_name":"calendar"}]`
	listing := `{"role":"assistant","content":[{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}]}`
	before := `[{"role":"user","content":"go"},` + mcpHistoryUse("DELETE_ALL") + `,` + listing + `,{"role":"user","content":"next"}]`
	// A listing appearing later must not be applied retroactively. A dynamic
	// toolset with no prior listing cannot be presumed to have a fixed set.
	requireMCPPlan(t, mcpMemberTestBody(tools, before), true, "mcp-client-2026-09-15")
	after := `[{"role":"user","content":"go"},` + listing + `,` + mcpHistoryUse("DELETE_ALL") + `,{"role":"user","content":"next"}]`
	requireMCPPlan(t, mcpMemberTestBody(tools, after), false, "mcp-client-2026-09-15")
	valid := strings.Replace(after, `"DELETE_ALL"`, `"events"`, 1)
	requireMCPPlan(t, mcpMemberTestBody(tools, valid), true, "mcp-client-2026-09-15")
}

func TestMCPDeferredMemberRequiresLoadingEvidence(t *testing.T) {
	tools := `[{"name":"echo","input_schema":{"type":"object"}},{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}}]`
	before := `[{"role":"user","content":"go"},` + mcpHistoryUse("events") + `,{"role":"user","content":"next"}]`
	requireMCPPlan(t, mcpMemberTestBody(tools, before), false)
	search := `{"role":"assistant","content":[{"type":"server_tool_use","id":"search-1","name":"tool_search_tool_regex","input":{"pattern":"events"}},{"type":"tool_search_tool_result","tool_use_id":"search-1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"events"}]}}]}`
	after := `[{"role":"user","content":"go"},` + search + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"next"}]`
	requireMCPPlan(t, mcpMemberTestBody(tools, after), true)
}

func TestMCPDeferredMemberOverridesAndDisabled(t *testing.T) {
	messages := `[{"role":"user","content":"go"},` + mcpHistoryUse("events") + `,{"role":"user","content":"next"}]`
	visible := `[{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true},"configs":{"events":{"defer_loading":false}}}]`
	requireMCPPlan(t, mcpMemberTestBody(visible, messages), true)
	disabled := `[{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true},"configs":{"events":{"enabled":false,"defer_loading":false}}}]`
	requireMCPPlan(t, mcpMemberTestBody(disabled, messages), false)
}

func TestMCPDeferredMemberSearchDoesNotGrantOtherServer(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/a"},{"type":"url","name":"storage","url":"https://example.com/b"}],`
	tools := `[{"name":"echo","input_schema":{"type":"object"}},{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}},{"type":"mcp_toolset","mcp_server_name":"storage","default_config":{"defer_loading":true}}]`
	search := `{"role":"assistant","content":[{"type":"server_tool_use","id":"search-1","name":"tool_search_tool_regex","input":{"pattern":"events"}},{"type":"tool_search_tool_result","tool_use_id":"search-1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"events"}]}}]}`
	messages := `[{"role":"user","content":"go"},` + search + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"next"}]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, false)
	// Only calendar is eligible for events in a pinned storage catalog. The
	// unqualified search reference now resolves unambiguously to calendar.
	tools = strings.Replace(tools, `"mcp_server_name":"storage","default_config":{"defer_loading":true}`, `"mcp_server_name":"storage","tools":[{"name":"files","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}`, 1)
	body = fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, true)
}

func TestMCPToolsetReadditionClearsStaleMemberOverridesAndLoad(t *testing.T) {
	tools := `[{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}}]`
	prefix := `[{"role":"user","content":"go"},` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"mcp_tool_reference","server_name":"calendar","name":"events"}}]},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":"continue"},` +
		`{"role":"system","content":[{"type":"tool_removal","tool":{"type":"mcp_toolset_reference","server_name":"calendar"}}]},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":"continue"},` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"mcp_toolset_reference","server_name":"calendar"}}]},`
	body := mcpMemberTestBody(tools, prefix+mcpHistoryUse("events")+`,{"role":"user","content":"next"}]`)
	requireMCPPlan(t, body, false, "inline-tools-2026-09-15") // loading from the prior epoch is stale
	body = mcpMemberTestBody(tools, prefix+
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"mcp_tool_reference","server_name":"calendar","name":"events"}}]},`+
		mcpHistoryUse("events")+`,{"role":"user","content":"next"}]`)
	requireMCPPlan(t, body, true, "inline-tools-2026-09-15")
}

func TestMCPFinalAvailabilityConstrainsSubsequentGuardToolCalls(t *testing.T) {
	tools := `[{"type":"mcp_toolset","mcp_server_name":"calendar"}]`
	listing := `{"role":"assistant","content":[{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}]}`
	body := mcpMemberTestBody(tools, `[{"role":"user","content":"go"},`+listing+`,{"role":"user","content":"next"}]`)
	plan, err := inlineProtocolPlan(body, "mcp-client-2026-09-15")
	if err != nil {
		t.Fatal(err)
	}
	if !completionGuardMCPToolAllowed(plan.Catalog, "calendar", "events") || completionGuardMCPToolAllowed(plan.Catalog, "calendar", "DELETE_ALL") {
		t.Fatal("guard response lookup ignored final MCP listing")
	}
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlan(&req, false)
	if guard.ValidationReason != "" {
		t.Fatal(guard.ValidationReason)
	}
	if completionGuardMCPToolAllowed(guard.Catalog, "calendar", "DELETE_ALL") {
		t.Fatal("standalone guard response lookup ignored final MCP listing")
	}
}

func TestMCPResponseListingAndSearchLoadMemberInSameResponse(t *testing.T) {
	tools := `[{"name":"echo","input_schema":{"type":"object"}},{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":true}}]`
	body := mcpMemberTestBody(tools, `[{"role":"user","content":"go"}]`)
	plan, err := inlineProtocolPlan(body, "mcp-client-2026-09-15")
	if err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatal(guard.ValidationReason)
	}
	content := `[{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]},` +
		`{"type":"server_tool_use","id":"search-1","name":"tool_search_tool_regex","input":{"pattern":"events"}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"search-1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"events"}]}},` +
		`{"type":"mcp_tool_use","id":"mcp-1","server_name":"calendar","name":"events","input":{}},` +
		`{"type":"mcp_tool_result","tool_use_id":"mcp-1","content":[]},` +
		`{"type":"text","text":"done"}]`
	var response ClaudeResponse
	wire := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":%s}`, content)
	if err := json.Unmarshal([]byte(wire), &response); err != nil {
		t.Fatal(err)
	}
	outcome := classifyCompletionGuardResponseWithRequestHistory(&response, guard, plan.Request)
	if !outcome.Accepted {
		t.Fatalf("valid listing/search/use response rejected: %+v", outcome)
	}
	if completionGuardMCPToolAllowed(plan.Catalog, "calendar", "events") {
		t.Fatal("response classification mutated the shared request snapshot")
	}
	// The listing must prohibit other members even when they are not deferred.
	noSearch := strings.Replace(content, `"name":"events","input":{}`, `"name":"DELETE_ALL","input":{}`, 1)
	if err := json.Unmarshal([]byte(strings.Replace(wire, content, noSearch, 1)), &response); err != nil {
		t.Fatal(err)
	}
	outcome = classifyCompletionGuardResponseWithRequestHistory(&response, guard, plan.Request)
	if outcome.Accepted {
		t.Fatalf("unlisted MCP member accepted: %+v", outcome)
	}
}
