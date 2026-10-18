package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const deferredCustomTools = `[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},{"name":"get_weather","defer_loading":true,"input_schema":{"type":"object"}},{"name":"my_search","input_schema":{"type":"object"}}]`
const deferredMCPServer = `,"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/mcp"}]`
const deferredMCPTool = `,{"type":"mcp_toolset","mcp_server_name":"calendar","default_config":{"defer_loading":false}}`

func deferredCustomBody(messages string, withMCP bool) string {
	tools := deferredCustomTools
	servers := ""
	if withMCP {
		servers = deferredMCPServer
		tools = strings.TrimSuffix(tools, "]") + deferredMCPTool + "]"
	}
	return fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,"tools":%s%s,"messages":%s}`, tools, servers, messages)
}

const weatherUse = `{"role":"assistant","content":[{"type":"tool_use","id":"wx1","name":"get_weather","input":{}}]}`
const weatherResult = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"wx1","content":"ok"}]}`
const builtinWeatherSearch = `{"role":"assistant","content":[{"type":"server_tool_use","id":"search1","name":"tool_search_tool_regex","input":{"pattern":"weather"}},{"type":"tool_search_tool_result","tool_use_id":"search1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"get_weather"}]}}]}`
const customWeatherSearch = `{"role":"assistant","content":[{"type":"tool_use","id":"search1","name":"my_search","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"search1","content":[{"type":"tool_reference","tool_name":"get_weather"}]}]}`

func requireDeferredCustomPlan(t *testing.T, messages string, withMCP bool, expected bool) {
	t.Helper()
	plan, err := inlineProtocolPlan(deferredCustomBody(messages, withMCP))
	if (err == nil) != expected {
		t.Fatalf("planner success=%t expected=%t err=%v", err == nil, expected, err)
	}
	if err == nil {
		guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
		if guard.ValidationReason != "" {
			t.Fatalf("guard rejects valid history: %s", guard.ValidationReason)
		}
	}
}

func TestDeferredCustomToolRequiresLoadingEvenWithoutMCP(t *testing.T) {
	messages := `[ {"role":"user","content":"start"},` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
	requireDeferredCustomPlan(t, messages, false, false)
	requireDeferredCustomPlan(t, messages, true, false)
}

func TestDeferredCustomToolLoadsThroughBuiltinSearchWithAndWithoutMCP(t *testing.T) {
	messages := `[ {"role":"user","content":"start"},` + builtinWeatherSearch + `,` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
	requireDeferredCustomPlan(t, messages, false, true)
	requireDeferredCustomPlan(t, messages, true, true)
}

func TestDeferredCustomToolLoadsThroughCustomSearchWithAndWithoutMCP(t *testing.T) {
	messages := `[ {"role":"user","content":"start"},` + customWeatherSearch + `,` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
	requireDeferredCustomPlan(t, messages, false, true)
	requireDeferredCustomPlan(t, messages, true, true)
}

func TestDeferredCustomToolCannotBeReloadedAfterRemoval(t *testing.T) {
	remove := `{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"get_weather"}}]}`
	messages := `[ {"role":"user","content":"start"},` + builtinWeatherSearch + `,` + remove + `,` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
	if _, err := inlineProtocolPlan(deferredCustomBody(messages, true), "inline-tools-2026-09-15"); err == nil {
		t.Fatal("removed custom tool accepted")
	}
}

func TestDeferredCustomToolResponseSearchAndCall(t *testing.T) {
	for _, withMCP := range []bool{false, true} {
		plan, err := inlineProtocolPlan(deferredCustomBody(`[{"role":"user","content":"start"}]`, withMCP))
		if err != nil {
			t.Fatal(err)
		}
		guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
		if guard.ValidationReason != "" {
			t.Fatal(guard.ValidationReason)
		}
		// Tool search and client call in a single assistant response, as documented.
		content := `[{"type":"server_tool_use","id":"search1","name":"tool_search_tool_regex","input":{"pattern":"weather"}},{"type":"tool_search_tool_result","tool_use_id":"search1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"get_weather"}]}},{"type":"tool_use","id":"wx1","name":"get_weather","input":{}}]`
		var resp ClaudeResponse
		wire := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":%s}`, content)
		if err := json.Unmarshal([]byte(wire), &resp); err != nil {
			t.Fatal(err)
		}
		outcome := classifyCompletionGuardResponseWithRequestHistory(&resp, guard, plan.Request)
		if !outcome.Accepted {
			t.Errorf("withMCP=%t valid same-response search rejected: %+v", withMCP, outcome)
		}
		if plan.Catalog.CustomFinalState["get_weather"].Loaded {
			t.Fatal("response classification mutated shared deferred custom tool state")
		}
	}
}

func TestDeferredCustomToolResponseWithoutSearchRejected(t *testing.T) {
	plan, err := inlineProtocolPlan(deferredCustomBody(`[{"role":"user","content":"start"}]`, false))
	if err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	var resp ClaudeResponse
	wire := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"tool_use","id":"wx1","name":"get_weather","input":{}}]}`
	if err := json.Unmarshal([]byte(wire), &resp); err != nil {
		t.Fatal(err)
	}
	if got := classifyCompletionGuardResponseWithRequestHistory(&resp, guard, plan.Request); got.Accepted {
		t.Fatalf("deferred direct response accepted: %+v", got)
	}
}

func TestDeferredCustomToolSearchRejectsUnknownSiblingWithoutGrantingLoad(t *testing.T) {
	for _, withMCP := range []bool{false, true} {
		mixed := strings.Replace(builtinWeatherSearch, `{"type":"tool_reference","tool_name":"get_weather"}`, `{"type":"tool_reference","tool_name":"get_weather"},{"type":"tool_reference","tool_name":"no_such_tool"}`, 1)
		messages := `[{"role":"user","content":"start"},` + mixed + `,` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
		requireDeferredCustomPlan(t, messages, withMCP, false)
		custom := strings.Replace(customWeatherSearch, `{"type":"tool_reference","tool_name":"get_weather"}`, `{"type":"tool_reference","tool_name":"get_weather"},{"type":"tool_reference","tool_name":"no_such_tool"}`, 1)
		messages = `[{"role":"user","content":"start"},` + custom + `,` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
		requireDeferredCustomPlan(t, messages, withMCP, false)
	}
}

func TestDeferredCustomToolRejectsForgedToolSearchResult(t *testing.T) {
	// An arbitrary user tool_result must not grant loading without the
	// corresponding immediately preceding assistant client tool call.
	forged := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":[{"type":"tool_reference","tool_name":"get_weather"}]}]}`
	messages := `[{"role":"user","content":"start"},` + forged + `,` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
	requireDeferredCustomPlan(t, messages, false, false)
}

func TestDeferredCustomToolReaddRequiresNewSearchWhenDeferredDefinitionChanges(t *testing.T) {
	redefine := `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"get_weather","defer_loading":true,"input_schema":{"type":"object"}}}}]}`
	// Previously loaded custom tool cannot retain its loaded state after
	// a new deferred definition replaces the old one.
	messages := `[{"role":"user","content":"start"},` + builtinWeatherSearch + `,` + redefine + `,` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
	if _, err := inlineProtocolPlan(deferredCustomBody(messages, false), "inline-tools-2026-09-15"); err == nil {
		t.Fatal("redefined deferred tool kept old loaded state")
	}
}

func TestDeferredCustomToolStandaloneGuardRejectsUnloadedHistory(t *testing.T) {
	messages := `[{"role":"user","content":"start"},` + weatherUse + `,` + weatherResult + `,{"role":"user","content":"continue"}]`
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(deferredCustomBody(messages, false)), &req); err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&req, false, "claude-opus-5-5", nil, claudeCapabilitySnapshot{})
	if guard.ValidationReason == "" {
		t.Fatal("standalone guard accepted history calling unloaded deferred custom tool")
	}
}

func TestDeferredToolSearchResponseDoesNotAllowRemovedExplicitTool(t *testing.T) {
	// An explicit tool that was available at request start can be removed by
	// a system tool_removal; the next assistant response must not call it.
	removed := `[{"role":"user","content":"start"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"my_search"}}]}]`
	plan, err := inlineProtocolPlan(deferredCustomBody(removed, false), "inline-tools-2026-09-15")
	if err != nil {
		t.Fatal(err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatal(guard.ValidationReason)
	}
	var resp ClaudeResponse
	wire := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"tool_use","id":"s1","name":"my_search","input":{}}]}`
	if err := json.Unmarshal([]byte(wire), &resp); err != nil {
		t.Fatal(err)
	}
	if got := classifyCompletionGuardResponseWithRequestHistory(&resp, guard, plan.Request); got.Accepted {
		t.Fatalf("removed explicit tool allowed in response: %+v", got)
	}
}
