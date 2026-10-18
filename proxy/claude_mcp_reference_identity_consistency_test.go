package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The same wire reference must not load an MCP member when two documented
// server/member identities produce it by exact and composed spelling.
func TestMCPToolSearchExactAndComposedMemberCollisionIsAmbiguous(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	tools := `[ {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"storage","tools":[{"name":"calendar_events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	for _, tc := range []struct{ server, member string }{{"calendar", "events"}, {"storage", "calendar_events"}} {
		t.Run(tc.server, func(t *testing.T) {
			use := fmt.Sprintf(`{"role":"assistant","content":[{"type":"mcp_tool_use","id":"m1","server_name":%q,"name":%q,"input":{}},{"type":"mcp_tool_result","tool_use_id":"m1","content":[]}]}`, tc.server, tc.member)
			messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage("calendar_events") + `,` + use + `,{"role":"user","content":"done"}]`
			body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
			requireMCPPlan(t, body, false)
		})
	}
}

func TestMCPToolSearchExactComposedCollisionRejectsEvenWhenOneDisabled(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	tools := `[ {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true},"configs":{"events":{"enabled":false}}}, {"type":"mcp_toolset","mcp_server_name":"storage","tools":[{"name":"calendar_events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage("calendar_events") + `,` + strings.Replace(mcpHistoryUse("calendar_events"), `"server_name":"calendar"`, `"server_name":"storage"`, 1) + `,{"role":"user","content":"done"}]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, false)
}

func TestMCPToolSearchComposedCollisionDoesNotShadowDynamicFallback(t *testing.T) {
	// A dynamic unpinned server is not evidence of a composed alias, whereas
	// an actual pinned server/member pair is authoritative.
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	tools := `[ {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"storage","default_config":{"defer_loading":true}} ]`
	messages := `[ {"role":"user","content":"go"},` + mcpQualifiedSearchMessage("calendar_events") + `,` + mcpHistoryUse("events") + `,{"role":"user","content":"done"}]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, true)
}

func TestMCPToolSearchCollisionDoesNotMutateLoadedState(t *testing.T) {
	// Use the actual deferred configurations built by the planner instead of
	// a hand-written catalog to verify that ambiguity never sets loaded bits.
	tools := `[ {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"storage","tools":[{"name":"calendar_events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":[{"role":"user","content":"go"}]}`, servers, tools)
	plan, err := inlineProtocolPlan(body)
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[string]bool{}
	match, status := claudeLoadUnambiguousMCPReference("calendar_events", plan.Catalog, nil, nil, nil, loaded)
	if status != claudeMCPReferenceAmbiguous || match.Server != "" || len(loaded) != 0 {
		t.Fatalf("ambiguous reference must not load any member: match=%+v status=%d loaded=%v", match, status, loaded)
	}
}

func TestMCPToolSearchMultipleReferencesDoNotAcceptAmbiguousSibling(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	tools := `[ {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"files","input_schema":{"type":"object"}},{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"storage","tools":[{"name":"calendar_events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	search := strings.Replace(mcpQualifiedSearchMessage("calendar_files"), `{"type":"tool_reference","tool_name":"calendar_files"}`, `{"type":"tool_reference","tool_name":"calendar_files"},{"type":"tool_reference","tool_name":"calendar_events"}`, 1)
	use := strings.Replace(mcpHistoryUse("events"), `"events"`, `"files"`, 1)
	messages := `[ {"role":"user","content":"go"},` + search + `,` + use + `,{"role":"user","content":"done"}]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":%s}`, servers, tools, messages)
	requireMCPPlan(t, body, false)
}

func TestMCPToolSearchGuardRejectsResponseWithCollidingReference(t *testing.T) {
	servers := `"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/calendar"},{"type":"url","name":"storage","url":"https://example.com/storage"}],`
	tools := `[ {"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}, {"type":"mcp_toolset","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}}, {"type":"mcp_toolset","mcp_server_name":"storage","tools":[{"name":"calendar_events","input_schema":{"type":"object"}}],"default_config":{"defer_loading":true}} ]`
	body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,%s"tools":%s,"messages":[{"role":"user","content":"go"}]}`, servers, tools)
	plan, err := inlineProtocolPlan(body)
	if err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatal(guard.ValidationReason)
	}
	content := `[ {"type":"server_tool_use","id":"search-1","name":"tool_search_tool_regex","input":{"pattern":"events"}}, {"type":"tool_search_tool_result","tool_use_id":"search-1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"calendar_events"}]}}, {"type":"mcp_tool_use","id":"m1","server_name":"storage","name":"calendar_events","input":{}}, {"type":"mcp_tool_result","tool_use_id":"m1","content":[]}, {"type":"text","text":"done"}]`
	wire := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":%s}`, content)
	var response ClaudeResponse
	if err := json.Unmarshal([]byte(wire), &response); err != nil {
		t.Fatal(err)
	}
	outcome := classifyCompletionGuardResponseWithRequestHistory(&response, guard, plan.Request)
	if outcome.Accepted {
		t.Fatalf("ambiguous response Tool Search reference accepted: %+v", outcome)
	}
	if completionGuardMCPToolAllowed(plan.Catalog, "storage", "calendar_events") {
		t.Fatal("response mutated shared MCP catalog")
	}
}
