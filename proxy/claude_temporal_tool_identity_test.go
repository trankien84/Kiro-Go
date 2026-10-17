package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTemporalInlineToolRedefinitionRejectsDifferentToolType(t *testing.T) {
	prefix := `{"model":"claude-opus-5-5","max_tokens":1024,"tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"start"},`
	replacement := `{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"bash_20250124","name":"bash"}}}]}]}`
	if _, err := inlineProtocolPlan(prefix+replacement, "inline-tools-2026-09-15"); err == nil || !strings.Contains(err.Message, "type") {
		t.Fatalf("custom-to-built-in type change accepted: %+v", err)
	}
	for _, definition := range []string{
		`{"name":"bash","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}`,
		`{"type":"custom","name":"bash","input_schema":{"type":"object"}}`,
	} {
		body := prefix + fmt.Sprintf(`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":%s}}]}]}`, definition)
		if _, err := inlineProtocolPlan(body, "inline-tools-2026-09-15"); err != nil {
			t.Fatalf("valid custom schema change rejected: %+v", err)
		}
	}
}

func TestTemporalInlineServerToolVersionUpgradeAndTypeConflict(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"tools":[{"name":"web_search","type":"web_search_20260209"}],"messages":[{"role":"user","content":"start"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"web_search","type":"web_search_20260318"}}}]}]}`
	if _, err := inlineProtocolPlan(body, "inline-tools-2026-09-15"); err != nil {
		t.Fatalf("same server tool version upgrade rejected: %+v", err)
	}
	invalid := strings.Replace(body, `"web_search_20260318"`, `"bash_20250124"`, 1)
	if _, err := inlineProtocolPlan(invalid, "inline-tools-2026-09-15"); err == nil {
		t.Fatal("server-to-client change accepted")
	}
}

func TestTemporalMCPUseBeforeInlineAdditionRejected(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/mcp"}],"messages":[{"role":"user","content":"start"},{"role":"assistant","content":[{"type":"mcp_tool_use","id":"mc1","server_name":"calendar","name":"events","input":{}},{"type":"mcp_tool_result","tool_use_id":"mc1","content":[]} ]},{"role":"user","content":"continue"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"mcp_toolset","mcp_server_name":"calendar"}}}]}]}`
	if _, err := inlineProtocolPlan(body, "inline-tools-2026-09-15", "mcp-client-2026-09-15"); err == nil || !strings.Contains(err.Message, "MCP") {
		t.Fatalf("MCP call before toolset activation accepted: %+v", err)
	}
}

func TestTemporalServerUseBeforeInlineAdditionRejected(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"start"},{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]} ]},{"role":"user","content":"continue"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"web_search_20260318","name":"web_search"}}}]}]}`
	if _, err := inlineProtocolPlan(body, "inline-tools-2026-09-15"); err == nil || !strings.Contains(err.Message, "not active") {
		t.Fatalf("server tool used before definition accepted: %+v", err)
	}
}

func TestTemporalCustomToolNameLength128(t *testing.T) {
	for _, tc := range []struct {
		size int
		ok   bool
	}{{64, true}, {65, true}, {128, true}, {129, false}} {
		name := strings.Repeat("a", tc.size)
		body := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,"tools":[{"name":%q,"input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`, name)
		_, err := inlineProtocolPlan(body)
		if (err == nil) != tc.ok {
			t.Fatalf("length=%d err=%+v", tc.size, err)
		}
		inline := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":%q,"input_schema":{"type":"object"}}}}]}]}`, name)
		_, err = inlineProtocolPlan(inline, "inline-tools-2026-09-15")
		if (err == nil) != tc.ok {
			t.Fatalf("inline length=%d err=%+v", tc.size, err)
		}
	}
}

func TestTemporalInlineServerAndMCPToolsRemainValidAfterActivation(t *testing.T) {
	cases := []struct {
		name, body string
		betas      []string
	}{
		{
			name:  "server tool added before call and removed after resolved result",
			body:  `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"search"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"web_search_20260318","name":"web_search"}}}]},{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}]},{"role":"user","content":"thanks"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"web_search"}}]}]}`,
			betas: []string{"inline-tools-2026-09-15"},
		},
		{
			name:  "MCP toolset introduced before call and removed after resolved result",
			body:  `{"model":"claude-opus-5-5","max_tokens":1024,"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/mcp"}],"messages":[{"role":"user","content":"events"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"mcp_toolset","mcp_server_name":"calendar"}}}]},{"role":"assistant","content":[{"type":"mcp_tool_use","id":"m1","server_name":"calendar","name":"events","input":{}},{"type":"mcp_tool_result","tool_use_id":"m1","content":[]}]},{"role":"user","content":"thanks"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"mcp_toolset_reference","server_name":"calendar"}}]}]}`,
			betas: []string{"inline-tools-2026-09-15", "mcp-client-2026-09-15"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := inlineProtocolPlan(tc.body, tc.betas...)
			if err != nil {
				t.Fatalf("valid historical tool use rejected: %+v", err)
			}
			guard := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(&plan.Request, false, plan.ResolvedModel, plan.Catalog, plan.Capabilities)
			if guard.ValidationReason != "" {
				t.Fatalf("guard rejected resolved historical tool: %s", guard.ValidationReason)
			}
		})
	}
}

func TestTemporalStandaloneGuardDoesNotAuthorizeFutureDeclarations(t *testing.T) {
	cases := []struct{ name, body string }{
		{"MCP", `{"model":"claude-opus-5-5","max_tokens":1024,"mcp_servers":[{"type":"url","name":"calendar","url":"https://example.com/mcp"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"mcp_tool_use","id":"m1","server_name":"calendar","name":"events","input":{}},{"type":"mcp_tool_result","tool_use_id":"m1","content":[]}]},{"role":"user","content":"continue"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"mcp_toolset","mcp_server_name":"calendar"}}}]}]}`},
		{"server", `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{}},{"type":"web_search_tool_result","tool_use_id":"s1","content":[]}]},{"role":"user","content":"continue"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"type":"web_search_20260318","name":"web_search"}}}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req ClaudeRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			guard := buildAgentCompletionGuardPlan(&req, false)
			if !strings.Contains(guard.ValidationReason, "not active") {
				t.Fatalf("standalone guard accepted future declaration: %s", guard.ValidationReason)
			}
		})
	}
}
