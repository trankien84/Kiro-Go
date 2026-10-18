package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompletionGuardPlanUsesCatalogAsSingleSemanticSource(t *testing.T) {
	allowedCode, _ := json.Marshal([]string{completionGuardCodeCallerType})
	req := &ClaudeRequest{
		Extra: map[string]json.RawMessage{
			"mcp_servers": json.RawMessage(`[{"type":"url","name":"docs","url":"https://mcp.example/sse"}]`),
		},
		Tools: []ClaudeTool{
			{Name: "read", InputSchema: map[string]interface{}{"type": "object"}},
			{Type: "web_search_20260209", Name: "web_search", Extra: map[string]json.RawMessage{"allowed_callers": allowedCode}},
			{Type: "mcp_toolset", Extra: map[string]json.RawMessage{"mcp_server_name": json.RawMessage(`"docs"`)}},
		},
	}
	plan := buildAgentCompletionGuardPlanForModel(req, false, "claude-opus-5")
	if plan.ValidationReason != "" {
		t.Fatalf("validation=%q", plan.ValidationReason)
	}
	if plan.Catalog == nil {
		t.Fatal("plan catalog is nil")
	}
	if _, ok := plan.Catalog.ClientTool("read"); !ok {
		t.Fatal("client tool missing from normalized catalog")
	}
	if _, ok := plan.Catalog.ExplicitTool("web_search"); !ok {
		t.Fatal("explicit server tool missing from normalized catalog")
	}
	if _, ok := plan.Catalog.ServerCapability("web_search"); !ok {
		t.Fatal("server capability missing from normalized catalog")
	}
	if !plan.Catalog.HasMCPServer("docs") || !plan.Catalog.HasMCPToolset("docs") {
		t.Fatal("MCP indexes are inconsistent")
	}
	if !plan.Catalog.ProgrammaticRequested || !plan.Catalog.ProgrammaticToolCalling {
		t.Fatalf("programmatic flags=%#v", plan.Catalog)
	}
}

func TestCompletionGuardCatalogAccessorsFailClosed(t *testing.T) {
	var catalog *completionGuardNormalizedCatalog
	if _, ok := catalog.ClientTool("read"); ok {
		t.Fatal("nil catalog exposed client tool")
	}
	if _, ok := catalog.ExplicitTool("read"); ok {
		t.Fatal("nil catalog exposed explicit tool")
	}
	if _, ok := catalog.ServerCapability("web_search"); ok {
		t.Fatal("nil catalog exposed server capability")
	}
	if catalog.HasMCPServer("docs") || catalog.HasMCPToolset("docs") {
		t.Fatal("nil catalog exposed MCP state")
	}
}

func TestCompletionGuardCatalogBuilderOwnsDeclarationValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  ClaudeRequest
		want string
	}{
		{
			name: "duplicate explicit name",
			req:  ClaudeRequest{Tools: []ClaudeTool{{Name: "read"}, {Name: "read"}}},
			want: "duplicate tool name",
		},
		{
			name: "invalid explicit name",
			req:  ClaudeRequest{Tools: []ClaudeTool{{Name: "bad name"}}},
			want: "must match",
		},
		{
			name: "unsupported server family version",
			req:  ClaudeRequest{Tools: []ClaudeTool{{Type: "web_search_20990101", Name: "web_search"}}},
			want: "invalid versioned server tool type",
		},
		{
			name: "deferred cache conflict",
			req: ClaudeRequest{Tools: []ClaudeTool{{
				Name: "read",
				Extra: map[string]json.RawMessage{
					"defer_loading": json.RawMessage(`true`),
					"cache_control": json.RawMessage(`{"type":"ephemeral"}`),
				},
			}}},
			want: "cannot combine defer_loading=true with cache_control",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, reason := completionGuardBuildNormalizedCatalog(&tc.req)
			if reason == "" || !strings.Contains(reason, tc.want) {
				t.Fatalf("reason=%q want substring %q", reason, tc.want)
			}
		})
	}
}

func TestCompletionGuardCatalogBuilderOwnsCapabilitiesAndFeatureFlags(t *testing.T) {
	codeOnly, err := json.Marshal([]string{completionGuardCodeCallerType})
	if err != nil {
		t.Fatal(err)
	}
	req := &ClaudeRequest{Tools: []ClaudeTool{
		{Type: "code_execution_20260120", Name: "code_execution"},
		{Name: "read", Extra: map[string]json.RawMessage{"allowed_callers": codeOnly}},
		{
			Type: "advisor_20260301",
			Name: "advisor",
			Extra: map[string]json.RawMessage{
				"model": json.RawMessage(`"claude-opus-4-8"`),
			},
		},
	}}
	catalog, reason := completionGuardBuildNormalizedCatalog(req)
	if reason != "" {
		t.Fatal(reason)
	}
	if !catalog.ProgrammaticRequested || !catalog.ProgrammaticToolCalling {
		t.Fatalf("programmatic flags=%#v", catalog)
	}
	if !catalog.HasAdvisor {
		t.Fatal("advisor feature was not recorded by catalog builder")
	}
	if _, ok := catalog.ServerCapability("code_execution"); !ok {
		t.Fatal("code-execution capability was not derived by catalog builder")
	}
	entry, ok := catalog.ClientEntry("read")
	if !ok || !entry.CodeCallable || entry.DirectCallable {
		t.Fatalf("client entry=%#v present=%t", entry, ok)
	}
}
