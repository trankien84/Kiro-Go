package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func completionGuardMCPTestRequest(defaultConfig string) ClaudeRequest {
	toolExtra := map[string]json.RawMessage{
		"mcp_server_name": json.RawMessage(`"example-mcp"`),
	}
	if defaultConfig != "" {
		toolExtra["default_config"] = json.RawMessage(defaultConfig)
	}
	return ClaudeRequest{
		Model: "claude-opus-4-8",
		Extra: map[string]json.RawMessage{
			"mcp_servers": json.RawMessage(`[{"type":"url","url":"https://mcp.example/sse","name":"example-mcp"}]`),
		},
		Tools: []ClaudeTool{{Type: "mcp_toolset", Extra: toolExtra}},
	}
}

func TestCompletionGuardMixedContinuationUsesImmediateAssistantTurn(t *testing.T) {
	clientTool := ClaudeTool{Name: "read", InputSchema: map[string]interface{}{"type": "object"}}
	serverTool := ClaudeTool{Type: "web_search_20250305", Name: "web_search"}

	mixed := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{clientTool, serverTool},
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "server_tool_use", "id": "server-1", "name": "web_search", "input": map[string]interface{}{}},
				map[string]interface{}{"type": "tool_use", "id": "client-1", "name": "read", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "client-1", "content": "ok"},
				map[string]interface{}{"type": "text", "text": "continue elsewhere"},
			}},
		},
	}
	mixedPlan := buildAgentCompletionGuardPlanForModel(&mixed, false, mixed.Model)
	if _, reason := completionGuardBuildHistoricalToolGraph(&mixed, mixedPlan); !strings.Contains(reason, "must contain only tool_result") {
		t.Fatalf("mixed continuation reason=%q", reason)
	}

	clientOnly := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{clientTool},
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "client-1", "name": "read", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "client-1", "content": "ok"},
				map[string]interface{}{"type": "text", "text": "now summarize"},
			}},
		},
	}
	clientPlan := buildAgentCompletionGuardPlanForModel(&clientOnly, false, clientOnly.Model)
	if _, reason := completionGuardBuildHistoricalToolGraph(&clientOnly, clientPlan); reason != "" {
		t.Fatalf("client-only text after results rejected: %q", reason)
	}

	serverOnly := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{serverTool},
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "server_tool_use", "id": "server-1", "name": "web_search", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{map[string]interface{}{"type": "text", "text": "do something else"}}},
		},
	}
	serverPlan := buildAgentCompletionGuardPlanForModel(&serverOnly, false, serverOnly.Model)
	if _, reason := completionGuardBuildHistoricalToolGraph(&serverOnly, serverPlan); !strings.Contains(reason, "cannot be a user continuation") {
		t.Fatalf("server-only user continuation reason=%q", reason)
	}
}

func TestCompletionGuardMCPHistoryKeepsServerIdentity(t *testing.T) {
	req := completionGuardMCPTestRequest("")
	req.Messages = []ClaudeMessage{{Role: "assistant", Content: []interface{}{
		map[string]interface{}{"type": "mcp_tool_use", "id": "mcp-1", "name": "echo", "input": map[string]interface{}{}},
	}}}
	plan := buildAgentCompletionGuardPlanForModel(&req, false, req.Model)
	if _, reason := completionGuardBuildHistoricalToolGraph(&req, plan); !strings.Contains(reason, "server_name") {
		t.Fatalf("missing historical server_name reason=%q", reason)
	}

	resolved := ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "mcp_tool_use", "id": "mcp-1", "name": "echo", "server_name": "old-server", "input": map[string]interface{}{}},
		}},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "mcp_tool_result", "tool_use_id": "mcp-1", "content": "ok"},
		}},
	}}
	if graph, reason := completionGuardBuildHistoricalToolGraph(&resolved, agentCompletionGuardPlan{}); reason != "" || graph.Nodes["mcp-1"].ServerName != "old-server" {
		t.Fatalf("resolved historical MCP rejected or lost identity: reason=%q node=%#v", reason, graph.Nodes["mcp-1"])
	}

	pending := ClaudeRequest{Messages: resolved.Messages[:1]}
	catalogReq := completionGuardMCPTestRequest("")
	catalogPlan := buildAgentCompletionGuardPlanForModel(&catalogReq, false, catalogReq.Model)
	if _, reason := completionGuardBuildHistoricalToolGraph(&pending, catalogPlan); !strings.Contains(reason, "missing or disabled") {
		t.Fatalf("pending MCP on wrong server reason=%q", reason)
	}
}

func TestCompletionGuardToolReferencesUseSourceSpecificResolution(t *testing.T) {
	customVisibleReq := ClaudeRequest{Tools: []ClaudeTool{{Name: "weather"}}}
	customVisiblePlan := buildAgentCompletionGuardPlanForModel(&customVisibleReq, false, "claude-opus-4-8")
	refs, reason := completionGuardParseCustomToolSearchReferences(
		[]interface{}{map[string]interface{}{"type": "tool_reference", "tool_name": "weather"}},
		"search-1",
		1,
		customVisiblePlan.Catalog,
	)
	if reason != "" || len(refs) != 1 || refs[0].Resolution.Kind != completionGuardReferenceExplicitTool || refs[0].Resolution.Deferred {
		t.Fatalf("visible custom reference refs=%#v reason=%q", refs, reason)
	}

	if _, reason := completionGuardParseBuiltinToolSearchReferences(
		map[string]interface{}{
			"type": "tool_search_tool_search_result",
			"tool_references": []interface{}{
				map[string]interface{}{"type": "tool_reference", "tool_name": "weather"},
			},
		},
		"builtin-visible",
		-1,
		customVisiblePlan.Catalog,
	); !strings.Contains(reason, "defer_loading=true") {
		t.Fatalf("built-in visible explicit tool reason=%q", reason)
	}

	explicitDeferredReq := ClaudeRequest{Tools: []ClaudeTool{
		{Name: "weather", Extra: map[string]json.RawMessage{"defer_loading": json.RawMessage(`true`)}},
		{Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex"},
	}}
	explicitDeferredPlan := buildAgentCompletionGuardPlanForModel(&explicitDeferredReq, false, "claude-opus-4-8")
	refs, reason = completionGuardParseBuiltinToolSearchReferences(
		map[string]interface{}{
			"type": "tool_search_tool_search_result",
			"tool_references": []interface{}{
				map[string]interface{}{"type": "tool_reference", "tool_name": "weather"},
			},
		},
		"builtin-deferred",
		-1,
		explicitDeferredPlan.Catalog,
	)
	if reason != "" || len(refs) != 1 || refs[0].Resolution.Kind != completionGuardReferenceExplicitTool || !refs[0].Resolution.Deferred {
		t.Fatalf("built-in deferred explicit reference refs=%#v reason=%q", refs, reason)
	}

	if _, reason := completionGuardParseCustomToolSearchReferences(
		[]interface{}{map[string]interface{}{"type": "tool_reference", "tool_name": "dynamic.echo"}},
		"search-2",
		1,
		explicitDeferredPlan.Catalog,
	); !strings.Contains(reason, "no corresponding top-level tool definition") {
		t.Fatalf("custom MCP-only reference reason=%q", reason)
	}

	serverReq := ClaudeRequest{Tools: []ClaudeTool{{
		Type: "web_fetch_20250910", Name: "web_fetch",
		Extra: map[string]json.RawMessage{"defer_loading": json.RawMessage(`true`)},
	}, {
		Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex",
	}}}
	serverPlan := buildAgentCompletionGuardPlanForModel(&serverReq, false, "claude-opus-4-8")
	refs, reason = completionGuardParseCustomToolSearchReferences(
		[]interface{}{map[string]interface{}{"type": "tool_reference", "tool_name": "web_fetch"}},
		"search-server",
		1,
		serverPlan.Catalog,
	)
	if reason != "" || len(refs) != 1 || refs[0].Resolution.Kind != completionGuardReferenceExplicitTool || !refs[0].Resolution.Deferred {
		t.Fatalf("explicit server-tool reference refs=%#v reason=%q", refs, reason)
	}

	mcpDeferred := completionGuardMCPTestRequest(`{"enabled":true,"defer_loading":true}`)
	mcpDeferred.Tools = append(mcpDeferred.Tools, ClaudeTool{
		Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex",
	})
	mcpDeferredPlan := buildAgentCompletionGuardPlanForModel(&mcpDeferred, false, mcpDeferred.Model)
	refs, reason = completionGuardParseBuiltinToolSearchReferences(
		map[string]interface{}{
			"type": "tool_search_tool_search_result",
			"tool_references": []interface{}{
				map[string]interface{}{"type": "tool_reference", "tool_name": "dynamic.echo"},
			},
		},
		"builtin-1",
		-1,
		mcpDeferredPlan.Catalog,
	)
	if reason != "" || len(refs) != 1 || refs[0].Resolution.Kind != completionGuardReferencePotentialDynamicMCP ||
		!refs[0].Resolution.Deferred || len(refs[0].Resolution.CandidateMCPs) != 1 || refs[0].Resolution.CandidateMCPs[0] != "example-mcp" {
		t.Fatalf("built-in dynamic MCP reference refs=%#v reason=%q", refs, reason)
	}

	mcpVisible := completionGuardMCPTestRequest(`{"enabled":true,"defer_loading":false}`)
	mcpVisible.Tools = append(mcpVisible.Tools, ClaudeTool{
		Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex",
	})
	mcpVisiblePlan := buildAgentCompletionGuardPlanForModel(&mcpVisible, false, mcpVisible.Model)
	if _, reason := completionGuardParseBuiltinToolSearchReferences(
		map[string]interface{}{
			"type": "tool_search_tool_search_result",
			"tool_references": []interface{}{
				map[string]interface{}{"type": "tool_reference", "tool_name": "dynamic.echo"},
			},
		},
		"builtin-visible-mcp",
		-1,
		mcpVisiblePlan.Catalog,
	); !strings.Contains(reason, "deferred tool definition") {
		t.Fatalf("visible MCP default allowed dynamic reference: %q", reason)
	}

	defaultDisabled := completionGuardMCPTestRequest(`{"enabled":false}`)
	defaultDisabled.Tools = append(defaultDisabled.Tools, ClaudeTool{
		Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex",
	})
	disabledPlan := buildAgentCompletionGuardPlanForModel(&defaultDisabled, false, defaultDisabled.Model)
	if _, reason := completionGuardParseBuiltinToolSearchReferences(
		map[string]interface{}{
			"type": "tool_search_tool_search_result",
			"tool_references": []interface{}{
				map[string]interface{}{"type": "tool_reference", "tool_name": "dynamic.echo"},
			},
		},
		"builtin-disabled",
		-1,
		disabledPlan.Catalog,
	); !strings.Contains(reason, "deferred tool definition") {
		t.Fatalf("disabled MCP default allowed an unknown dynamic reference: %q", reason)
	}

	explicitMCP := completionGuardMCPTestRequest(`{"enabled":false}`)
	explicitMCP.Tools[0].Extra["configs"] = json.RawMessage(`{"dynamic.echo":{"enabled":true,"defer_loading":true}}`)
	explicitMCP.Tools = append(explicitMCP.Tools, ClaudeTool{
		Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex",
	})
	explicitMCPPlan := buildAgentCompletionGuardPlanForModel(&explicitMCP, false, explicitMCP.Model)
	refs, reason = completionGuardParseBuiltinToolSearchReferences(
		map[string]interface{}{
			"type": "tool_search_tool_search_result",
			"tool_references": []interface{}{
				map[string]interface{}{"type": "tool_reference", "tool_name": "dynamic.echo"},
			},
		},
		"builtin-explicit-mcp",
		-1,
		explicitMCPPlan.Catalog,
	)
	if reason != "" || len(refs) != 1 || refs[0].Resolution.Kind != completionGuardReferencePotentialDynamicMCP ||
		len(refs[0].Resolution.CandidateMCPs) != 1 || refs[0].Resolution.CandidateMCPs[0] != "example-mcp" {
		t.Fatalf("explicit deferred MCP reference refs=%#v reason=%q", refs, reason)
	}
}

func TestCompletionGuardTypedMCPVisibility(t *testing.T) {
	invalidTopLevel := completionGuardMCPTestRequest("")
	invalidTopLevel.Tools[0].Extra["strict"] = json.RawMessage(`true`)
	if plan := buildAgentCompletionGuardPlanForModel(&invalidTopLevel, false, invalidTopLevel.Model); !strings.Contains(plan.ValidationReason, "strict is not valid") {
		t.Fatalf("invalid top-level MCP shape validation=%q", plan.ValidationReason)
	}

	allDeferred := completionGuardMCPTestRequest(`{"enabled":true,"defer_loading":true}`)
	if plan := buildAgentCompletionGuardPlanForModel(&allDeferred, false, allDeferred.Model); !strings.Contains(plan.ValidationReason, "at least one tool") {
		t.Fatalf("all-deferred MCP validation=%q", plan.ValidationReason)
	}

	overrideVisible := completionGuardMCPTestRequest(`{"enabled":false}`)
	overrideVisible.Tools[0].Extra["configs"] = json.RawMessage(`{"echo":{"enabled":true,"defer_loading":false}}`)
	if plan := buildAgentCompletionGuardPlanForModel(&overrideVisible, false, overrideVisible.Model); plan.ValidationReason != "" {
		t.Fatalf("visible MCP override rejected: %q", plan.ValidationReason)
	}

	searchKeepsCatalogVisible := completionGuardMCPTestRequest(`{"enabled":true,"defer_loading":true}`)
	searchKeepsCatalogVisible.Tools = append(searchKeepsCatalogVisible.Tools, ClaudeTool{
		Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex",
	})
	if plan := buildAgentCompletionGuardPlanForModel(&searchKeepsCatalogVisible, false, searchKeepsCatalogVisible.Model); plan.ValidationReason != "" {
		t.Fatalf("non-deferred Tool Search should keep catalog visible: %q", plan.ValidationReason)
	}
}

func TestCompletionGuardSemanticBetaNormalization(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"mcp_servers":[{"type":"url","url":"https://mcp.example/sse","name":"example-mcp"}],
		"tools":[{"type":"mcp_toolset","mcp_server_name":"example-mcp"}],
		"betas":["mcp-client-2025-11-20","other-beta"]
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	headers := make(http.Header)
	headers.Add("Anthropic-Beta", "other-beta, advisor-tool-2026-03-01")
	headers.Add("Anthropic-Beta", "third-beta")
	semantic, reason := completionGuardBuildSemanticRequest(&req, headers, raw)
	if reason != "" {
		t.Fatal(reason)
	}
	if got := semantic.Headers.Get("Anthropic-Beta"); got != "other-beta,advisor-tool-2026-03-01,third-beta,mcp-client-2025-11-20" {
		t.Fatalf("normalized beta header=%q", got)
	}
	if _, exists := req.Extra["betas"]; exists {
		t.Fatal("compatibility body betas remained in typed request")
	}
	if bytes.Contains(semantic.Body, []byte(`"betas"`)) {
		t.Fatalf("compatibility body betas leaked to raw outbound body: %s", semantic.Body)
	}
	catalog, catalogReason := completionGuardBuildNormalizedCatalog(&req)
	if catalogReason != "" {
		t.Fatal(catalogReason)
	}
	if reason := completionGuardValidateSemanticHeaders(catalog, semantic.Headers); reason != "" {
		t.Fatalf("normalized MCP beta rejected: %q", reason)
	}
	requirements, validationErr := analyzeClaudeTransportRequirements(&req, semantic, req.Model)
	if validationErr != nil || requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("requirements=%#v validation=%v", requirements, validationErr)
	}
	cloned, err := cloneCompletionGuardRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	clonedWire, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(clonedWire, []byte(`"betas"`)) {
		t.Fatalf("guard clone restored compatibility beta field: %s", clonedWire)
	}

	bad := req
	bad.Extra = map[string]json.RawMessage{"betas": json.RawMessage(`"mcp-client-2025-11-20"`)}
	if _, reason := completionGuardBuildSemanticRequest(&bad, nil, []byte(`{"betas":"mcp-client-2025-11-20"}`)); !strings.Contains(reason, "array") {
		t.Fatalf("invalid body betas reason=%q", reason)
	}
}

func TestCompletionGuardSemanticBetasPreserveExactWireValues(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"mcp_servers":[{"type":"url","url":"https://mcp.example/sse","name":"example-mcp"}],
		"tools":[{"type":"mcp_toolset","mcp_server_name":"example-mcp"}],
		"betas":["mcp-client-2025-11-20","Vendor-Preview-2026-07-30"]
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	headers := make(http.Header)
	headers.Add("Anthropic-Beta", "Vendor-Preview-2026-07-30,other-beta")
	headers.Add("Anthropic-Beta", "vendor-preview-2026-07-30,other-beta")
	semantic, reason := completionGuardBuildSemanticRequest(&req, headers, raw)
	if reason != "" {
		t.Fatal(reason)
	}
	want := "Vendor-Preview-2026-07-30,other-beta,vendor-preview-2026-07-30,mcp-client-2025-11-20"
	if got := semantic.Headers.Get("Anthropic-Beta"); got != want {
		t.Fatalf("exact beta header=%q want %q", got, want)
	}
	if !semantic.Betas.Contains("Vendor-Preview-2026-07-30") ||
		!semantic.Betas.Contains("vendor-preview-2026-07-30") {
		t.Fatalf("case-distinct beta values collapsed: %#v", semantic.Betas.Values())
	}
	catalog, catalogReason := completionGuardBuildNormalizedCatalog(&req)
	if catalogReason != "" {
		t.Fatal(catalogReason)
	}

	wrongCase := make(http.Header)
	wrongCase.Set("Anthropic-Beta", "MCP-CLIENT-2025-11-20")
	if reason := completionGuardValidateSemanticHeaders(catalog, wrongCase); !strings.Contains(reason, "requires anthropic-beta") {
		t.Fatalf("wrong-case required beta unexpectedly accepted: %q", reason)
	}
	correct := make(http.Header)
	correct.Set("Anthropic-Beta", "MCP-CLIENT-2025-11-20,mcp-client-2025-11-20")
	if reason := completionGuardValidateSemanticHeaders(catalog, correct); reason != "" {
		t.Fatalf("exact required beta rejected: %q", reason)
	}
}

func TestCompletionGuardRecursiveSchemaOnlyForCodeCallableTools(t *testing.T) {
	selfRef := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"next": map[string]interface{}{"$ref": "#"},
		},
	}
	direct := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{{Name: "walk", InputSchema: selfRef}},
	}, false, "claude-opus-4-8")
	if direct.ValidationReason != "" {
		t.Fatalf("direct-only recursive schema rejected: %q", direct.ValidationReason)
	}

	codeCallable := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "walk", InputSchema: selfRef, Extra: map[string]json.RawMessage{
				"allowed_callers": json.RawMessage(`["code_execution_20260120"]`),
			}},
		},
	}, false, "claude-opus-4-8")
	if !strings.Contains(codeCallable.ValidationReason, "circular local $ref") {
		t.Fatalf("code-callable recursion validation=%q", codeCallable.ValidationReason)
	}

	unresolved := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "walk", InputSchema: map[string]interface{}{"type": "object", "$ref": "#/missing"}, Extra: map[string]json.RawMessage{
				"allowed_callers": json.RawMessage(`["code_execution_20260120"]`),
			}},
		},
	}, false, "claude-opus-4-8")
	if !strings.Contains(unresolved.ValidationReason, "unresolved local $ref") {
		t.Fatalf("unresolved local ref validation=%q", unresolved.ValidationReason)
	}

	external := buildAgentCompletionGuardPlanForModel(&ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{
			{Type: "code_execution_20260120", Name: "code_execution"},
			{Name: "walk", InputSchema: map[string]interface{}{"type": "object", "$ref": "https://example.com/schema.json"}, Extra: map[string]json.RawMessage{
				"allowed_callers": json.RawMessage(`["code_execution_20260120"]`),
			}},
		},
	}, false, "claude-opus-4-8")
	if external.ValidationReason != "" {
		t.Fatalf("external ref should be left to upstream: %q", external.ValidationReason)
	}
}

func TestCompletionGuardMixedTurnAllowsTextAfterResolvedServerCall(t *testing.T) {
	clientTool := ClaudeTool{Name: "read", InputSchema: map[string]interface{}{"type": "object"}}
	serverTool := ClaudeTool{Type: "web_search_20250305", Name: "web_search"}
	req := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{clientTool, serverTool},
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "server_tool_use", "id": "server-1", "name": "web_search", "input": map[string]interface{}{}},
				map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "server-1", "content": []interface{}{}},
				map[string]interface{}{"type": "tool_use", "id": "client-1", "name": "read", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "client-1", "content": "ok"},
				map[string]interface{}{"type": "text", "text": "now summarize"},
			}},
		},
	}
	plan := buildAgentCompletionGuardPlanForModel(&req, false, req.Model)
	if _, reason := completionGuardBuildHistoricalToolGraph(&req, plan); reason != "" {
		t.Fatalf("resolved server call incorrectly made continuation result-only: %q", reason)
	}
}

func TestCompletionGuardToolSearchErrorResultIsValidHistory(t *testing.T) {
	req := ClaudeRequest{
		Model: "claude-opus-4-8",
		Tools: []ClaudeTool{{
			Type: "tool_search_tool_regex_20251119",
			Name: "tool_search_tool_regex",
		}},
		Messages: []ClaudeMessage{{Role: "assistant", Content: []interface{}{
			map[string]interface{}{
				"type": "server_tool_use", "id": "search-1", "name": "tool_search_tool_regex",
				"input": map[string]interface{}{"pattern": "("},
			},
			map[string]interface{}{
				"type": "tool_search_tool_result", "tool_use_id": "search-1",
				"content": map[string]interface{}{
					"type": "tool_search_tool_result_error", "error_code": "invalid_tool_input",
					"error_message": "invalid regular expression",
				},
			},
		}}},
	}
	plan := buildAgentCompletionGuardPlanForModel(&req, false, req.Model)
	if _, reason := completionGuardBuildHistoricalToolGraph(&req, plan); reason != "" {
		t.Fatalf("official Tool Search error result rejected: %q", reason)
	}

	bad := map[string]interface{}{
		"type":       "tool_search_tool_result_error",
		"error_code": "future_unknown_code",
	}
	if _, reason := completionGuardParseBuiltinToolSearchReferences(bad, "search-2", 0, plan.Catalog); !strings.Contains(reason, "unsupported") {
		t.Fatalf("unknown Tool Search error code reason=%q", reason)
	}
}

func TestCompletionGuardAllDisabledMCPDoesNotPretendToolsAreDeferred(t *testing.T) {
	req := completionGuardMCPTestRequest(`{"enabled":false}`)
	plan := buildAgentCompletionGuardPlanForModel(&req, false, req.Model)
	if plan.ValidationReason != "" {
		t.Fatalf("all-disabled MCP catalog should not be classified as all-deferred: %q", plan.ValidationReason)
	}
}

func TestCompletionGuardSchemaTraversalUsesOnlySchemaLocations(t *testing.T) {
	annotationSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"payload": map[string]interface{}{
				"type":     "object",
				"examples": []interface{}{map[string]interface{}{"$ref": "#"}},
				"default":  map[string]interface{}{"$ref": "#"},
				"enum":     []interface{}{map[string]interface{}{"$ref": "#"}},
				"const":    map[string]interface{}{"$ref": "#"},
				"x-vendor-annotation": map[string]interface{}{
					"$ref": "#",
				},
			},
		},
	}
	if cycle, reason := completionGuardInputSchemaHasReferenceCycle(annotationSchema); cycle || reason != "" {
		t.Fatalf("annotation data was interpreted as a schema: cycle=%v reason=%q", cycle, reason)
	}

	booleanChildren := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"allow": true,
			"deny":  false,
		},
	}
	if cycle, reason := completionGuardInputSchemaHasReferenceCycle(booleanChildren); cycle || reason != "" {
		t.Fatalf("boolean subschema rejected: cycle=%v reason=%q", cycle, reason)
	}
}

func TestCompletionGuardSchemaReferenceResolutionCoversLocalResources(t *testing.T) {
	tests := []struct {
		name       string
		schema     map[string]interface{}
		wantCycle  bool
		wantReason string
	}{
		{
			name: "cycle_through_items",
			schema: map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"parent": map[string]interface{}{"$ref": "#"},
					},
				},
			},
			wantCycle: true,
		},
		{
			name: "mutual_defs_cycle",
			schema: map[string]interface{}{
				"$defs": map[string]interface{}{
					"a": map[string]interface{}{"$ref": "#/$defs/b"},
					"b": map[string]interface{}{"$ref": "#/$defs/a"},
				},
				"$ref": "#/$defs/a",
			},
			wantCycle: true,
		},
		{
			name: "cycle_through_one_of",
			schema: map[string]interface{}{
				"oneOf": []interface{}{
					map[string]interface{}{"type": "string"},
					map[string]interface{}{"$ref": "#"},
				},
			},
			wantCycle: true,
		},
		{
			name: "unused_definition_refers_to_root_without_cycle",
			schema: map[string]interface{}{
				"type": "string",
				"$defs": map[string]interface{}{
					"unused": map[string]interface{}{"$ref": "#"},
				},
			},
		},
		{
			name: "then_without_if_does_not_apply",
			schema: map[string]interface{}{
				"type": "string",
				"then": map[string]interface{}{"$ref": "#"},
			},
		},
		{
			name: "shared_non_cyclic_definition",
			schema: map[string]interface{}{
				"$defs": map[string]interface{}{
					"leaf": map[string]interface{}{"type": "string"},
				},
				"type": "object",
				"properties": map[string]interface{}{
					"left":  map[string]interface{}{"$ref": "#/$defs/leaf"},
					"right": map[string]interface{}{"$ref": "#/$defs/leaf"},
				},
			},
		},
		{
			name: "escaped_json_pointer",
			schema: map[string]interface{}{
				"$defs": map[string]interface{}{
					"a/b~c": map[string]interface{}{"type": "string"},
				},
				"$ref": "#/$defs/a~1b~0c",
			},
		},
		{
			name: "percent_encoded_anchor_cycle",
			schema: map[string]interface{}{
				"$anchor": "node",
				"type":    "object",
				"properties": map[string]interface{}{
					"next": map[string]interface{}{"$ref": "#n%6Fde"},
				},
			},
			wantCycle: true,
		},
		{
			name: "nested_id_resource_cycle",
			schema: map[string]interface{}{
				"$id": "https://schemas.example/root",
				"$defs": map[string]interface{}{
					"node": map[string]interface{}{
						"$id":     "node",
						"$anchor": "self",
						"type":    "object",
						"properties": map[string]interface{}{
							"next": map[string]interface{}{"$ref": "#self"},
						},
					},
				},
				"$ref": "node#self",
			},
			wantCycle: true,
		},
		{
			name: "unresolved_local_pointer",
			schema: map[string]interface{}{
				"$ref": "#/$defs/missing",
			},
			wantReason: "unresolved local $ref",
		},
		{
			name: "external_reference_left_to_upstream",
			schema: map[string]interface{}{
				"$ref": "https://schemas.example/external#node",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cycle, reason := completionGuardInputSchemaHasReferenceCycle(tc.schema)
			if cycle != tc.wantCycle {
				t.Fatalf("cycle=%v want %v reason=%q", cycle, tc.wantCycle, reason)
			}
			if tc.wantReason == "" {
				if reason != "" {
					t.Fatalf("unexpected reason=%q", reason)
				}
			} else if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason=%q want substring %q", reason, tc.wantReason)
			}
		})
	}
}

func TestCompletionGuardSchemaTraversalHandlesDeepSchemasIteratively(t *testing.T) {
	root := map[string]interface{}{"type": "object"}
	current := root
	for index := 0; index < 750; index++ {
		next := map[string]interface{}{"type": "object"}
		current["properties"] = map[string]interface{}{"next": next}
		current = next
	}
	if cycle, reason := completionGuardInputSchemaHasReferenceCycle(root); cycle || reason != "" {
		t.Fatalf("deep acyclic schema rejected: cycle=%v reason=%q", cycle, reason)
	}
}
