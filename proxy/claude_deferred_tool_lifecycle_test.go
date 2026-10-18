package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func lifecycleGuardResponse(t *testing.T, messages, content string, betas ...string) (completionGuardOutcome, *completionGuardNormalizedCatalog) {
	t.Helper()
	return lifecycleGuardResponseFromBody(t, deferredCustomBody(messages, false), content, betas...)
}

func lifecycleGuardResponseFromBody(t *testing.T, body, content string, betas ...string) (completionGuardOutcome, *completionGuardNormalizedCatalog) {
	t.Helper()
	plan, err := inlineProtocolPlan(body, betas...)
	if err != nil {
		t.Fatalf("request planner rejected history: %+v", err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatalf("guard rejected history: %s", guard.ValidationReason)
	}
	wire := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":%s}`, content)
	var resp ClaudeResponse
	if err := json.Unmarshal([]byte(wire), &resp); err != nil {
		t.Fatal(err)
	}
	return classifyCompletionGuardResponseWithRequestHistory(&resp, guard, plan.Request), plan.Catalog
}

func TestDeferredToolSearchResponseCannotReviveRemovedTool(t *testing.T) {
	removed := `[{"role":"user","content":"start"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"get_weather"}}]}]`
	search := `[{"type":"server_tool_use","id":"search1","name":"tool_search_tool_regex","input":{"pattern":"weather"}},{"type":"tool_search_tool_result","tool_use_id":"search1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"get_weather"}]}},{"type":"tool_use","id":"wx1","name":"get_weather","input":{}}]`
	result, catalog := lifecycleGuardResponse(t, removed, search, "inline-tools-2026-09-15")
	if result.Accepted {
		t.Fatalf("removed tool revived through search: %+v", result)
	}
	if catalog.CustomFinalState["get_weather"].Loaded {
		t.Fatal("shared catalog mutated")
	}
}

func TestDeferredToolSearchHistoricalReferenceUsesDefinitionAtEventTime(t *testing.T) {
	redefine := `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"get_weather","input_schema":{"type":"object"},"defer_loading":false}}}]}`
	messages := `[{"role":"user","content":"start"},` + builtinWeatherSearch + `,` + weatherUse + `,` + weatherResult + `,` + redefine + `,{"role":"assistant","content":"noted"},{"role":"user","content":"continue"}]`
	plan, err := inlineProtocolPlan(deferredCustomBody(messages, false), "inline-tools-2026-09-15")
	if err != nil {
		t.Fatalf("planner rejected historical valid search: %+v", err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatalf("historical reference checked against newer definition: %s", guard.ValidationReason)
	}
}

func TestDeferredToolSearchResponseRejectsRemovedReferenceWithoutClientCall(t *testing.T) {
	removed := `[{"role":"user","content":"start"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"get_weather"}}]}]`
	searchOnly := `[{"type":"server_tool_use","id":"search1","name":"tool_search_tool_regex","input":{"pattern":"weather"}},{"type":"tool_search_tool_result","tool_use_id":"search1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"get_weather"}]}}]`
	outcome, _ := lifecycleGuardResponse(t, removed, searchOnly, "inline-tools-2026-09-15")
	if outcome.Accepted || outcome.Reason != "invalid_tool_reference" {
		t.Fatalf("removed reference should be rejected specifically as invalid_tool_reference: %+v", outcome)
	}
}

func TestDeferredToolSearchHistoricalReferenceSurvivesLaterRemoval(t *testing.T) {
	removed := `{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"get_weather"}}]}`
	messages := `[{"role":"user","content":"start"},` + builtinWeatherSearch + `,` + weatherUse + `,` + weatherResult + `,` + removed + `,{"role":"assistant","content":"noted"},{"role":"user","content":"continue"}]`
	plan, err := inlineProtocolPlan(deferredCustomBody(messages, false), "inline-tools-2026-09-15")
	if err != nil {
		t.Fatalf("planner rejected earlier valid call: %v", err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatalf("guard retroactively invalidated earlier search: %s", guard.ValidationReason)
	}
	if plan.Catalog.CustomFinalState["get_weather"].Offered {
		t.Fatal("removed tool still offered")
	}
}

func TestDeferredToolSearchHistoricalCustomReferenceSurvivesRedefinition(t *testing.T) {
	redefine := `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"get_weather","input_schema":{"type":"object"},"defer_loading":false}}}]}`
	messages := `[{"role":"user","content":"start"},` + customWeatherSearch + `,` + weatherUse + `,` + weatherResult + `,` + redefine + `,{"role":"assistant","content":"noted"},{"role":"user","content":"continue"}]`
	plan, err := inlineProtocolPlan(deferredCustomBody(messages, false), "inline-tools-2026-09-15")
	if err != nil {
		t.Fatalf("planner rejected history: %+v", err)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatalf("guard invalidated earlier custom search: %s", guard.ValidationReason)
	}
}

func TestDeferredToolSearchReaddedDefinitionRequiresFreshLoad(t *testing.T) {
	removed := `{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"get_weather"}}]}`
	readded := `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"get_weather","input_schema":{"type":"object"},"defer_loading":true}}}]}`
	messages := `[{"role":"user","content":"start"},` + builtinWeatherSearch + `,{"role":"user","content":"ok"},` + removed + `,{"role":"assistant","content":"noted"},{"role":"user","content":"ok"},` + readded + `]`
	plan, err := inlineProtocolPlan(deferredCustomBody(messages, false), "inline-tools-2026-09-15")
	if err != nil {
		t.Fatalf("planner rejected remove/readd: %+v", err)
	}
	state := plan.Catalog.CustomFinalState["get_weather"]
	if !state.Offered || state.Loaded || !state.Deferred || state.Epoch == 0 {
		t.Fatalf("unexpected readded epoch state: %+v", state)
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatalf("guard rejected valid history: %s", guard.ValidationReason)
	}
	var call ClaudeResponse
	if err := json.Unmarshal([]byte(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"tool_use","id":"wx1","name":"get_weather","input":{}}]}`), &call); err != nil {
		t.Fatal(err)
	}
	if result := classifyCompletionGuardResponseWithRequestHistory(&call, guard, plan.Request); result.Accepted {
		t.Fatalf("readded deferred tool inherited previous search: %+v", result)
	}
	var loaded ClaudeResponse
	if err := json.Unmarshal([]byte(`{"id":"msg_3","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"server_tool_use","id":"search2","name":"tool_search_tool_regex","input":{"pattern":"weather"}},{"type":"tool_search_tool_result","tool_use_id":"search2","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"get_weather"}]}},{"type":"tool_use","id":"wx2","name":"get_weather","input":{}}]}`), &loaded); err != nil {
		t.Fatal(err)
	}
	if result := classifyCompletionGuardResponseWithRequestHistory(&loaded, guard, plan.Request); !result.Accepted {
		t.Fatalf("fresh search did not load readded tool: %+v", result)
	}
	if result := classifyCompletionGuardResponseWithRequestHistory(&call, guard, plan.Request); result.Accepted {
		t.Fatal("response classification persisted loaded state into shared catalog")
	}
}

func TestDeferredToolSearchMixedRemovedReferenceDoesNotPartiallyLoad(t *testing.T) {
	removed := `[{"role":"user","content":"start"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"get_weather"}}]}]`
	// Both get_weather (removed) and my_search (still offered AND deferred)
	// appear in the same search result. Reject atomically in either order.
	body := strings.Replace(deferredCustomBody(removed, false),
		`"name":"my_search","input_schema"`,
		`"name":"my_search","defer_loading":true,"input_schema"`, 1)
	for _, names := range []string{`[{"type":"tool_reference","tool_name":"my_search"},{"type":"tool_reference","tool_name":"get_weather"}]`, `[{"type":"tool_reference","tool_name":"get_weather"},{"type":"tool_reference","tool_name":"my_search"}]`} {
		content := `[{"type":"server_tool_use","id":"search1","name":"tool_search_tool_regex","input":{"pattern":"weather"}},{"type":"tool_search_tool_result","tool_use_id":"search1","content":{"type":"tool_search_tool_search_result","tool_references":` + names + `}}]`
		outcome, catalog := lifecycleGuardResponseFromBody(t, body, content, "inline-tools-2026-09-15")
		if outcome.Accepted || outcome.Reason != "invalid_tool_reference" {
			t.Fatalf("mixed search must fail due removed reference: %+v", outcome)
		}
		if catalog.CustomFinalState["my_search"].Loaded || catalog.CustomFinalState["get_weather"].Loaded {
			t.Fatal("invalid mixed result mutated shared tool state")
		}
	}
}
