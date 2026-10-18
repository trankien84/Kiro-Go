package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func inlineProtocolPlan(body string, betas ...string) (claudeRequestPlan, *claudeTransportValidationError) {
	header := http.Header{}
	if len(betas) > 0 {
		header.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
	return buildClaudeRequestPlanWithResolver([]byte(body), header, "-thinking", resolveClaudeProviderReasoningCapability)
}

func TestInlineToolDefinitionTemporalHistoryAndPassthrough(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"search_db","description":"search","input_schema":{"type":"object","required":["query"]}}}}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"search_db","input":{"query":"a"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"search_db","input_schema":{"type":"object","required":["version"]}}}}]},` +
		`{"role":"assistant","content":"done"},{"role":"user","content":"next"}]}`
	plan, err := inlineProtocolPlan(body, "inline-tools-2026-09-15")
	if err != nil {
		t.Fatalf("inline schema update rejected: %+v", err)
	}
	if !plan.Requirements.Allows(claudeTransportDirectAnthropic) || plan.Requirements.Allows(claudeTransportKiroNative) || plan.Requirements.Allows(claudeTransportBedrockInvoke) {
		t.Fatalf("invalid provider routing: %s", plan.Requirements.Summary())
	}
	guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(
		&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
	if guard.ValidationReason != "" {
		t.Fatalf("completion guard rejects valid inline history: %s", guard.ValidationReason)
	}
	entry, ok := plan.Catalog.ClientEntry("search_db")
	if !ok {
		t.Fatal("inline tool not available to guard")
	}
	schema, _ := entry.InputSchema.(map[string]interface{})
	required, _ := schema["required"].([]interface{})
	if len(required) != 1 || required[0] != "version" {
		t.Fatalf("latest schema not indexed: %#v", schema)
	}
	var a, b interface{}
	if json.Unmarshal([]byte(body), &a) != nil || json.Unmarshal(plan.Semantic.Body, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("history changed: %s", plan.Semantic.Body)
	}
}

func TestInlineToolRemovedBeforeUseRejectedWithoutRetroactiveRemoval(t *testing.T) {
	prefix := `{"model":"claude-opus-5-5","max_tokens":1024,"tools":[{"name":"echo","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"start"},`
	remove := `{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"echo"}}]},`
	use := `{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"echo","input":{}}]},`
	result := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]},`
	valid := prefix + use + result + remove + `{"role":"assistant","content":"done"}]}`
	if _, err := inlineProtocolPlan(valid, "inline-tools-2026-09-15"); err != nil {
		t.Fatalf("retroactive removal rejected history: %+v", err)
	}
	invalid := prefix + remove + use + result + `{"role":"user","content":"next"}]}`
	if _, err := inlineProtocolPlan(invalid, "inline-tools-2026-09-15"); err == nil || !strings.Contains(err.Message, "not active") {
		t.Fatalf("removed tool use allowed: %+v", err)
	}
}

func TestLegacyMidConversationToolChangeAndBetaValidation(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"tools":[{"name":"echo","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"echo"}}]}]}`
	if _, err := inlineProtocolPlan(body, "mid-conversation-tool-changes-2026-07-01"); err != nil {
		t.Fatalf("old beta rejected: %+v", err)
	}
	if _, err := inlineProtocolPlan(body, "inline-tools-2026-09-15"); err != nil {
		t.Fatalf("new beta rejected: %+v", err)
	}
	if _, err := inlineProtocolPlan(body); err == nil || !strings.Contains(err.Message, "mid-conversation") {
		t.Fatalf("missing beta accepted: %+v", err)
	}
	definition := strings.Replace(body, `{"type":"tool_reference","name":"echo"}`, `{"type":"tool_definition","definition":{"name":"echo","input_schema":{"type":"object"}}}`, 1)
	definition = strings.Replace(definition, "tool_removal", "tool_addition", 1)
	if _, err := inlineProtocolPlan(definition, "mid-conversation-tool-changes-2026-07-01"); err == nil || !strings.Contains(err.Message, "inline-tools") {
		t.Fatalf("old beta unexpectedly allows definition: %+v", err)
	}
}

func TestInlineMCPToolsetAndListingRequireBothBetas(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/mcp"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"mcp_toolset","mcp_server_name":"calendar"}}}]},{"role":"assistant","content":[{"type":"mcp_tool_listing","mcp_server_name":"calendar","tools":[{"name":"events","input_schema":{"type":"object"}}]}]},{"role":"user","content":"thanks"}]}`
	for _, tc := range []struct {
		betas []string
		ok    bool
	}{
		{[]string{"inline-tools-2026-09-15", "mcp-client-2026-09-15"}, true},
		{[]string{"inline-tools-2026-09-15"}, false},
		{[]string{"mcp-client-2026-09-15"}, false},
	} {
		plan, err := inlineProtocolPlan(body, tc.betas...)
		if tc.ok {
			if err != nil {
				t.Fatalf("MCP inline rejected: %+v", err)
			}
			if !plan.Requirements.Allows(claudeTransportDirectAnthropic) || plan.Requirements.Allows(claudeTransportBedrockInvoke) {
				t.Fatalf("MCP routing invalid: %s", plan.Requirements.Summary())
			}
			if !strings.Contains(string(plan.Semantic.Body), "mcp_tool_listing") {
				t.Fatal("listing not preserved")
			}
		} else if err == nil {
			t.Fatalf("invalid MCP beta combination accepted: %v", tc.betas)
		}
	}
}

func TestMidConversationEffortOnlyAndTurnScopedValidation(t *testing.T) {
	effort := `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"system","content":[],"output_config":{"effort":"high"}},{"role":"user","content":"hi"}]}`
	plan, err := inlineProtocolPlan(effort, "mid-conversation-output-config-2026-07-01")
	if err != nil {
		t.Fatalf("effort-only system first rejected: %+v", err)
	}
	if plan.Requirements.Allows(claudeTransportKiroNative) {
		t.Fatal("effort-only must be native")
	}
	if _, err = inlineProtocolPlan(effort); err == nil || !strings.Contains(err.Message, "mid-conversation-output-config") {
		t.Fatalf("effort beta missing accepted: %+v", err)
	}
	scoped := `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"},{"role":"system","clear_at":"next_user_message","content":"For this turn"}]}`
	if scopedPlan, scopedErr := inlineProtocolPlan(scoped, "mid-conversation-system-clear-at-2026-08-21"); scopedErr != nil {
		t.Fatalf("clear_at rejected: %+v", scopedErr)
	} else if !scopedPlan.Requirements.Allows(claudeTransportBedrockInvoke) {
		t.Fatalf("native Bedrock Invoke should preserve clear_at: %s", scopedPlan.Requirements.Summary())
	}
	invalid := strings.Replace(scoped, `"content":"For this turn"`, `"content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"x"}}]`, 1)
	if _, err = inlineProtocolPlan(invalid, "mid-conversation-system-clear-at-2026-08-21", "inline-tools-2026-09-15"); err == nil || !strings.Contains(err.Message, "text only") {
		t.Fatalf("invalid turn-scoped tool change accepted: %+v", err)
	}
}

func TestInlineToolsCompatibilityBodyBetasAndPausedTurn(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"betas":["inline-tools-2026-09-15","inline-tools-2026-09-15"],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"new_tool","input_schema":{"type":"object"}}}}]}]}`
	plan, err := inlineProtocolPlan(body)
	if err != nil {
		t.Fatalf("body beta projection: %+v", err)
	}
	if plan.Semantic.Headers.Get("Anthropic-Beta") != "inline-tools-2026-09-15" {
		t.Fatalf("beta header not deduplicated: %v", plan.Semantic.Headers)
	}
	if strings.Contains(string(plan.Semantic.Body), `"betas"`) {
		t.Fatal("client beta compatibility field leaked upstream")
	}
	paused := `{"model":"claude-opus-5-5","max_tokens":1024,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"search"},{"role":"assistant","content":[{"type":"web_search_tool_result","tool_use_id":"s1","content":[]} ]},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"lookup"}}]}]}`
	if _, err := inlineProtocolPlan(paused, "inline-tools-2026-09-15"); err == nil || !strings.Contains(err.Message, "paused") {
		t.Fatalf("tool change after paused server turn accepted: %+v", err)
	}
}

func TestInlineMCPDefinitionRejectsInvalidConfigAndPinnedSchema(t *testing.T) {
	common := `{"model":"claude-opus-5-5","max_tokens":1024,"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/mcp"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"mcp_toolset","mcp_server_name":"calendar",%s}}}]}]}`
	for _, field := range []string{
		`"default_config":{"enabled":"yes"}`,
		`"configs":{"events":{"defer_loading":"yes"}}`,
		`"tools":[{"name":"events"}]`,
	} {
		body := fmt.Sprintf(common, field)
		if _, err := inlineProtocolPlan(body, "inline-tools-2026-09-15", "mcp-client-2026-09-15"); err == nil {
			t.Fatalf("invalid MCP toolset was accepted: %s", field)
		}
	}
}
