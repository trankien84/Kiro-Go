package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHasWebSearchTool_SingleNativeTool(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		Tools: []ClaudeTool{
			{Type: "web_search_20250305", Name: "web_search"},
		},
	}
	if !hasWebSearchTool(req) {
		t.Fatal("expected single native web_search tool to be detected")
	}
	if hasWebSearchAmongTools(req) {
		t.Fatal("single native tool must not be treated as mixed")
	}
}

func TestHasWebSearchTool_MultipleTools(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		Tools: []ClaudeTool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Name: "other_tool", Description: "Other"},
		},
	}
	if hasWebSearchTool(req) {
		t.Fatal("multiple tools must not be treated as pure web_search request")
	}
	if !hasWebSearchAmongTools(req) {
		t.Fatal("expected mixed tools with native web_search")
	}
}

func TestHasWebSearchTool_RegularToolNamedWebSearch(t *testing.T) {
	// Client-defined tools named web_search without type web_search_* must not
	// take the native path.
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		Tools: []ClaudeTool{
			{Name: "web_search", Description: "Regular client-side search tool"},
		},
	}
	if hasWebSearchTool(req) {
		t.Fatal("regular tool named web_search must not trigger native websearch")
	}
	if isNativeWebSearchTool(req.Tools[0]) {
		t.Fatal("regular tool must not be native")
	}
}

func TestHasWebSearchTool_NoTools(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-sonnet-4"}
	if hasWebSearchTool(req) {
		t.Fatal("request without tools must not be treated as web_search request")
	}
	if hasWebSearchAmongTools(req) {
		t.Fatal("request without tools must not be mixed web_search")
	}
}

func TestExtractSearchQuery_StringContentWithPrefix(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Perform a web search for the query: rust latest version 2026"},
		},
	}
	got := extractSearchQuery(req)
	want := "rust latest version 2026"
	if got != want {
		t.Fatalf("query = %q, want %q", got, want)
	}
}

func TestExtractSearchQuery_StringContentWithoutPrefix(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "typescript 5.7 features"},
		},
	}
	if got := extractSearchQuery(req); got != "typescript 5.7 features" {
		t.Fatalf("query = %q", got)
	}
}

func TestExtractSearchQuery_LastUserTurn(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Perform a web search for the query: first"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "Perform a web search for the query: second"},
		},
	}
	if got := extractSearchQuery(req); got != "second" {
		t.Fatalf("query = %q, want second", got)
	}
}

func TestExtractSearchQuery_ArrayContent(t *testing.T) {
	// JSON decode yields []interface{} for content arrays.
	raw := `{
		"model": "claude-sonnet-4",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "Perform a web search for the query: golang generics"}
			]}
		]
	}`
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := extractSearchQuery(&req); got != "golang generics" {
		t.Fatalf("query = %q", got)
	}
}

func TestExtractSearchQuery_Empty(t *testing.T) {
	req := &ClaudeRequest{}
	if got := extractSearchQuery(req); got != "" {
		t.Fatalf("expected empty query, got %q", got)
	}
}

func TestCreateMcpRequest_Format(t *testing.T) {
	toolUseID, mcpReq := createMcpRequest("hello")

	if !strings.HasPrefix(toolUseID, "srvtoolu_") {
		t.Errorf("toolUseID prefix wrong: %q", toolUseID)
	}
	if len(toolUseID) != len("srvtoolu_")+32 {
		t.Errorf("toolUseID length = %d", len(toolUseID))
	}
	if !strings.HasPrefix(mcpReq.ID, "web_search_tooluse_") {
		t.Errorf("mcp request ID prefix wrong: %q", mcpReq.ID)
	}
	if mcpReq.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", mcpReq.JSONRPC)
	}
	if mcpReq.Method != "tools/call" {
		t.Errorf("method = %q", mcpReq.Method)
	}
	if mcpReq.Params.Name != "web_search" {
		t.Errorf("params.name = %q", mcpReq.Params.Name)
	}
	if mcpReq.Params.Arguments.Query != "hello" {
		t.Errorf("query = %q", mcpReq.Params.Arguments.Query)
	}
}

func TestParseSearchResults(t *testing.T) {
	inner := `{"results":[{"title":"T1","url":"https://e.com","snippet":"s1","publishedDate":1732299319000,"id":"1","domain":"e.com"}],"totalResults":1,"query":"q"}`
	mcpResp := &McpResponse{
		Result: &McpResult{
			Content: []McpContent{{Type: "text", Text: inner}},
		},
	}
	results := parseSearchResults(mcpResp)
	if results == nil {
		t.Fatal("expected results")
	}
	if len(results.Results) != 1 {
		t.Fatalf("results len = %d", len(results.Results))
	}
	r := results.Results[0]
	if r.Title != "T1" || r.URL != "https://e.com" {
		t.Errorf("unexpected result: %+v", r)
	}
	if r.Snippet == nil || *r.Snippet != "s1" {
		t.Errorf("snippet mismatch: %+v", r.Snippet)
	}
}

func TestParseSearchResults_EmbeddedError(t *testing.T) {
	inner := `{"results":[],"error":"upstream failed"}`
	mcpResp := &McpResponse{
		Result: &McpResult{
			Content: []McpContent{{Type: "text", Text: inner}},
		},
	}
	if parseSearchResults(mcpResp) != nil {
		t.Fatal("embedded error must yield nil")
	}
}

func TestParseSearchResults_IsErrorFlag(t *testing.T) {
	mcpResp := &McpResponse{
		Result: &McpResult{
			IsError: true,
			Content: []McpContent{{Type: "text", Text: `{"results":[]}`}},
		},
	}
	if parseSearchResults(mcpResp) != nil {
		t.Fatal("isError must yield nil")
	}
}

func TestParseSearchResults_NonTextContent(t *testing.T) {
	mcpResp := &McpResponse{
		Result: &McpResult{Content: []McpContent{{Type: "image", Text: ""}}},
	}
	if parseSearchResults(mcpResp) != nil {
		t.Fatal("non-text content must yield nil")
	}
}

func TestParseSearchResults_NilResult(t *testing.T) {
	if parseSearchResults(&McpResponse{}) != nil {
		t.Fatal("nil result must yield nil")
	}
}

func TestGenerateSearchSummary_WithResults(t *testing.T) {
	s1 := "a snippet"
	results := &WebSearchResults{
		Results: []WebSearchResult{
			{Title: "Title One", URL: "https://one.com", Snippet: &s1},
		},
	}
	summary := generateSearchSummary("myquery", results)
	for _, want := range []string{"myquery", "Title One", "https://one.com", "a snippet"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestGenerateSearchSummary_NoResults(t *testing.T) {
	summary := generateSearchSummary("q", nil)
	if !strings.Contains(summary, "No results found") {
		t.Errorf("expected 'No results found':\n%s", summary)
	}
}

func TestGenerateSearchSummary_LongSnippetTruncated(t *testing.T) {
	long := strings.Repeat("x", 300)
	results := &WebSearchResults{
		Results: []WebSearchResult{{Title: "T", URL: "https://e.com", Snippet: &long}},
	}
	summary := generateSearchSummary("q", results)
	if !strings.Contains(summary, "...") {
		t.Error("expected long snippet to be truncated with ellipsis")
	}
}

func TestChunkByRunes(t *testing.T) {
	chunks := chunkByRunes("abcdef", 2)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %v", chunks)
	}
	if strings.Join(chunks, "") != "abcdef" {
		t.Errorf("rejoined = %q", strings.Join(chunks, ""))
	}
}

func TestChunkByRunes_Multibyte(t *testing.T) {
	in := "あいうえおかきくけこ"
	chunks := chunkByRunes(in, 3)
	if strings.Join(chunks, "") != in {
		t.Errorf("multibyte rejoin failed: %q", strings.Join(chunks, ""))
	}
	for _, c := range chunks {
		if !json.Valid([]byte(`"` + c + `"`)) {
			t.Errorf("chunk not valid utf-8: %q", c)
		}
	}
}

func TestShouldSearchRound(t *testing.T) {
	web := []KiroToolUse{{Name: "web_search"}, {Name: "web_search"}}
	if !shouldSearchRound(0, web, maxWebSearchRounds) {
		t.Fatal("pure web_search should continue")
	}
	if shouldSearchRound(maxWebSearchRounds, web, maxWebSearchRounds) {
		t.Fatal("at limit should stop")
	}
	if shouldSearchRound(1, web, 1) {
		t.Fatal("max_uses=1 must stop after first search round index")
	}
	mixed := []KiroToolUse{{Name: "web_search"}, {Name: "Bash"}}
	if shouldSearchRound(0, mixed, maxWebSearchRounds) {
		t.Fatal("mixed tools should not continue loop")
	}
	if shouldSearchRound(0, nil, maxWebSearchRounds) {
		t.Fatal("empty tool uses should not continue")
	}
}

func TestResolveWebSearchMaxUses(t *testing.T) {
	if got := resolveWebSearchMaxUses(nil); got != maxWebSearchRounds {
		t.Fatalf("nil tools: got %d", got)
	}
	if got := resolveWebSearchMaxUses([]ClaudeTool{
		{Type: "web_search_20250305", Name: "web_search", MaxUses: 2},
		{Name: "Bash"},
	}); got != 2 {
		t.Fatalf("max_uses=2: got %d", got)
	}
	if got := resolveWebSearchMaxUses([]ClaudeTool{
		{Type: "web_search_20250305", Name: "web_search", MaxUses: 99},
	}); got != maxWebSearchRounds {
		t.Fatalf("max_uses must be capped at %d, got %d", maxWebSearchRounds, got)
	}
	if got := resolveWebSearchMaxUses([]ClaudeTool{
		{Type: "web_search_20250305", Name: "web_search"},
	}); got != maxWebSearchRounds {
		t.Fatalf("omitted max_uses: got %d", got)
	}
}

func TestResolveFlushStopReason(t *testing.T) {
	if got := resolveFlushStopReason("max_tokens", nil, nil); got != "max_tokens" {
		t.Fatalf("override: %q", got)
	}
	client := []KiroToolUse{{Name: "Bash"}}
	if got := resolveFlushStopReason("", client, nil); got != "tool_use" {
		t.Fatalf("client tool: %q", got)
	}
	webOnly := []KiroToolUse{{Name: "web_search"}}
	if got := resolveFlushStopReason("", webOnly, nil); got != "end_turn" {
		t.Fatalf("web only: %q", got)
	}
	contentWithClient := []map[string]interface{}{
		{"type": "tool_use", "name": "Bash"},
	}
	if got := resolveFlushStopReason("", nil, contentWithClient); got != "tool_use" {
		t.Fatalf("content client tool: %q", got)
	}
}

func TestBuildFlushContent_WebSearchAsServerTool(t *testing.T) {
	snippet := "hello"
	results := &WebSearchResults{
		Results: []WebSearchResult{{Title: "T", URL: "https://e.com", Snippet: &snippet}},
	}
	content := buildFlushContent(nil, "done", []KiroToolUse{
		{ToolUseID: "toolu_1", Name: "web_search", Input: map[string]interface{}{"query": "q"}},
		{ToolUseID: "toolu_2", Name: "Bash", Input: map[string]interface{}{"command": "ls"}},
	}, []*WebSearchResults{results, nil})

	var sawServer, sawClient, sawRawWeb bool
	for _, b := range content {
		switch b["type"] {
		case "server_tool_use":
			sawServer = true
			if b["name"] != "web_search" {
				t.Errorf("server tool name = %v", b["name"])
			}
		case "web_search_tool_result":
			// ok
		case "tool_use":
			if b["name"] == "web_search" {
				sawRawWeb = true
			}
			if b["name"] == "Bash" {
				sawClient = true
			}
		}
	}
	if !sawServer {
		t.Error("expected server_tool_use for web_search")
	}
	if !sawClient {
		t.Error("expected raw tool_use for Bash")
	}
	if sawRawWeb {
		t.Error("web_search must not be flushed as raw tool_use")
	}
}

func TestConvertClaudeTools_SkipsNativeWebSearchAndKeepsClientTools(t *testing.T) {
	tools := []ClaudeTool{
		{Type: "web_search_20250305", Name: "web_search"},
		{Name: "Bash", Description: "run", InputSchema: map[string]interface{}{"type": "object"}},
	}
	kiroTools, nameMap := convertClaudeTools(tools)
	var sawWeb, sawClient bool
	for _, tspec := range kiroTools {
		name := tspec.ToolSpecification.Name
		switch {
		case name == "web_search":
			sawWeb = true
		case name == "Bash" || name == "bash" || nameMap[name] == "Bash":
			sawClient = true
		}
	}
	if sawWeb {
		t.Error("native server web_search must not be serialized as a Kiro client function")
	}
	if !sawClient {
		t.Errorf("expected client Bash tool in %+v (nameMap=%v)", kiroTools, nameMap)
	}
}

func TestConvertClaudeTools_PureNativeWebSearchSkipped(t *testing.T) {
	// Pure path never calls convertClaudeTools. If it did, native-only must not
	// inject a Kiro web_search (no client tools remain after filtering).
	tools := []ClaudeTool{
		{Type: "web_search_20250305", Name: "web_search"},
	}
	kiroTools, _ := convertClaudeTools(tools)
	if len(kiroTools) != 0 {
		t.Fatalf("pure native web_search must not produce Kiro tools, got %+v", kiroTools)
	}
}

func TestClaudeToolTypeJSONRoundTrip(t *testing.T) {
	raw := `{"type":"web_search_20250305","name":"web_search","max_uses":8}`
	var tool ClaudeTool
	if err := json.Unmarshal([]byte(raw), &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Type != "web_search_20250305" || tool.Name != "web_search" || tool.MaxUses != 8 {
		t.Fatalf("tool = %+v", tool)
	}
	if !isNativeWebSearchTool(tool) {
		t.Fatal("expected native")
	}
}

func TestBuildWebSearchContentBlocks(t *testing.T) {
	s := "snip"
	results := &WebSearchResults{
		Results: []WebSearchResult{{Title: "T", URL: "https://e.com", Snippet: &s}},
	}
	blocks := buildWebSearchContentBlocks("q", "srvtoolu_abc", results)
	if len(blocks) != 4 {
		t.Fatalf("blocks len = %d", len(blocks))
	}
	if blocks[1]["type"] != "server_tool_use" {
		t.Errorf("block1 type = %v", blocks[1]["type"])
	}
	if blocks[2]["type"] != "web_search_tool_result" {
		t.Errorf("block2 type = %v", blocks[2]["type"])
	}
}

// Regression: appendSearchRound must store content in a shape ClaudeToKiro can
// extract (tool_use + tool_result pairing). Previously []map was dropped.
func TestAppendSearchRound_ContentSurvivesClaudeToKiro(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "search something"},
		},
		Tools: []ClaudeTool{
			{Type: "web_search_20250305", Name: "web_search", MaxUses: 3},
			{Name: "Bash", Description: "run", InputSchema: map[string]interface{}{"type": "object"}},
		},
	}
	round := &webSearchRoundOutcome{
		text: "Looking it up.",
		toolUses: []KiroToolUse{
			{
				ToolUseID: "toolu_ws_1",
				Name:      "web_search",
				Input:     map[string]interface{}{"query": "golang generics"},
			},
		},
	}
	snippet := "Go 1.18 introduced generics"
	searched := []*WebSearchResults{
		{Results: []WebSearchResult{{Title: "Generics", URL: "https://go.dev", Snippet: &snippet}}},
	}
	presentation := make([]map[string]interface{}, 0)
	appendSearchRound(req, round, searched, &presentation)

	if len(req.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3 (user + assistant + tool_result user)", len(req.Messages))
	}
	if len(presentation) != 2 {
		t.Fatalf("presentation len = %d, want server_tool_use + web_search_tool_result", len(presentation))
	}

	// Direct extract on the in-memory shapes.
	aText, aUses := extractClaudeAssistantContent(req.Messages[1].Content)
	if aText != "Looking it up." || len(aUses) != 1 || aUses[0].ToolUseID != "toolu_ws_1" {
		t.Fatalf("assistant extract failed: text=%q uses=%+v", aText, aUses)
	}
	_, _, uResults := extractClaudeUserContent(req.Messages[2].Content)
	if len(uResults) != 1 || uResults[0].ToolUseID != "toolu_ws_1" {
		t.Fatalf("user tool_result extract failed: %+v", uResults)
	}
	if len(uResults[0].Content) == 0 || !strings.Contains(uResults[0].Content[0].Text, "golang generics") {
		t.Fatalf("tool_result summary missing query: %+v", uResults[0].Content)
	}

	// Full ClaudeToKiro must keep the active tool turn (history tool_use ↔ current toolResults).
	payload := ClaudeToKiro(req, false)
	if payload == nil {
		t.Fatal("nil payload")
	}
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) == 0 {
		t.Fatal("ClaudeToKiro dropped tool_results after appendSearchRound")
	}
	if ctx.ToolResults[0].ToolUseID != "toolu_ws_1" {
		t.Fatalf("tool result id = %q", ctx.ToolResults[0].ToolUseID)
	}
	// History last assistant must carry matching tool_use.
	hist := payload.ConversationState.History
	if len(hist) == 0 {
		t.Fatal("expected history")
	}
	last := hist[len(hist)-1]
	if last.AssistantResponseMessage == nil || len(last.AssistantResponseMessage.ToolUses) == 0 {
		t.Fatalf("last history assistant missing tool_use: %+v", last.AssistantResponseMessage)
	}
	if last.AssistantResponseMessage.ToolUses[0].ToolUseID != "toolu_ws_1" {
		t.Fatalf("history tool_use id = %q", last.AssistantResponseMessage.ToolUses[0].ToolUseID)
	}
}

func TestExtractClaudeContent_MapSliceShape(t *testing.T) {
	// Defense in depth: []map[string]interface{} must work even without []interface{}.
	user := []map[string]interface{}{
		{"type": "tool_result", "tool_use_id": "id1", "content": "ok"},
	}
	_, _, results := extractClaudeUserContent(user)
	if len(results) != 1 || results[0].ToolUseID != "id1" {
		t.Fatalf("map-slice user content: %+v", results)
	}
	asst := []map[string]interface{}{
		{"type": "text", "text": "hi"},
		{"type": "tool_use", "id": "id1", "name": "web_search", "input": map[string]interface{}{"query": "q"}},
	}
	text, uses := extractClaudeAssistantContent(asst)
	if text != "hi" || len(uses) != 1 || uses[0].Name != "web_search" {
		t.Fatalf("map-slice assistant content: text=%q uses=%+v", text, uses)
	}
}

func TestParseSearchResults_EmptyResultsArrayIsValid(t *testing.T) {
	// Empty results is a legitimate MCP success; only unparseable/error payloads are nil.
	inner := `{"results":[],"totalResults":0,"query":"q"}`
	mcpResp := &McpResponse{
		Result: &McpResult{
			Content: []McpContent{{Type: "text", Text: inner}},
		},
	}
	results := parseSearchResults(mcpResp)
	if results == nil {
		t.Fatal("empty results array must parse as non-nil success")
	}
	if len(results.Results) != 0 {
		t.Fatalf("results len = %d", len(results.Results))
	}
}

// TestHasKiroCapableAccount verifies the capability gate used to decide whether
// the web_search fast path may run at all: it must see through account type
// (Bedrock/custom_api have no MCP relay) and Enabled state.
func TestHasKiroCapableAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	if h.hasKiroCapableAccount() {
		t.Fatal("empty pool must report no Kiro-capable account")
	}

	if err := config.AddAccount(config.Account{
		ID: "bedrock-1", AuthMethod: "bedrock", Enabled: true,
		BedrockAccessKeyID: "AKIA...", BedrockSecretAccessKey: "secret", Region: "us-east-1",
	}); err != nil {
		t.Fatalf("add bedrock account: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "custom-1", AuthMethod: "custom_api", Enabled: true, BaseURL: "https://x", KiroApiKey: "k",
	}); err != nil {
		t.Fatalf("add custom_api account: %v", err)
	}
	if h.hasKiroCapableAccount() {
		t.Fatal("Bedrock + custom_api only pool must report no Kiro-capable account")
	}

	if err := config.AddAccount(config.Account{
		ID: "kiro-disabled", AuthMethod: "idc", Enabled: false, AccessToken: "tok",
	}); err != nil {
		t.Fatalf("add disabled kiro account: %v", err)
	}
	if h.hasKiroCapableAccount() {
		t.Fatal("a disabled Kiro account must not count as capable")
	}

	if err := config.AddAccount(config.Account{
		ID: "kiro-1", AuthMethod: "idc", Enabled: true, AccessToken: "tok",
	}); err != nil {
		t.Fatalf("add kiro account: %v", err)
	}
	if !h.hasKiroCapableAccount() {
		t.Fatal("an enabled non-Bedrock/custom_api account must count as capable")
	}
}

// TestWebSearchSkipsNonKiroAccountsWithoutBan verifies the CLAUDE.md invariant
// that every Kiro-facing account loop excludes Bedrock/custom_api accounts:
// performWebSearch (the MCP relay) must skip a Bedrock account rather than
// call it, and must not report that skip as a failure that bans the account.
func TestWebSearchSkipsNonKiroAccountsWithoutBan(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "bedrock-only", AuthMethod: "bedrock", Enabled: true,
		BedrockAccessKeyID: "AKIA...", BedrockSecretAccessKey: "secret", Region: "us-east-1",
	}); err != nil {
		t.Fatalf("add bedrock account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// No stub client is installed at all: if performWebSearch ever tried to reach
	// the Bedrock account via MCP, it would attempt a real network call and this
	// test would hang/fail on a live DNS lookup rather than skip cleanly.
	_, _, account, err := h.performWebSearch("claude-sonnet-4", "test query", "")
	if err == nil {
		t.Fatal("expected an error: pool has no Kiro-capable account")
	}
	if account != nil {
		t.Fatalf("expected no account returned on total failure, got %+v", account)
	}
	if !strings.Contains(err.Error(), "no available accounts") {
		t.Fatalf("expected a no-available-accounts error, got %v", err)
	}

	got, ok := config.GetAccountByID("bedrock-only")
	if !ok {
		t.Fatal("bedrock account disappeared")
	}
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf("skipped Bedrock account must not be banned: enabled=%v ban=%q", got.Enabled, got.BanStatus)
	}
}

// TestWebSearchFastPathFallsThroughWithNoKiroCapableAccount verifies the fix for
// the gap in handleClaudeMessagesInternal: previously the web_search short-circuit
// keyed off the request only, so an all-Bedrock pool would still take the MCP
// fast path, exhaust the (skipped) account, and return a hard error instead of
// falling through to the normal dispatch that could have served a Bedrock
// account directly. It must now issue ZERO requests to Kiro's MCP endpoint and
// must not disable the healthy Bedrock account.
func TestWebSearchFastPathFallsThroughWithNoKiroCapableAccount(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "bedrock-only", AuthMethod: "bedrock", Enabled: true,
		BedrockAccessKeyID: "AKIA...", BedrockSecretAccessKey: "secret", Region: "us-east-1",
	}); err != nil {
		t.Fatalf("add bedrock account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	// kiroHttpStore backs GetClientForProxy, which callMcpAPI uses. Fail the test
	// outright if web_search's MCP relay is ever reached — that is exactly the
	// hijack this test guards against — and never re-arm the real
	// ProxyFromEnvironment-backed client afterward (see the identically-shaped
	// hazard comment in TestApiImportCredentialsAPIKeySuccess).
	var mcpCalls atomic.Int32
	kiroHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mcpCalls.Add(1)
			return nil, fmt.Errorf("unexpected Kiro/MCP request: %s", r.URL)
		}),
	})
	t.Cleanup(func() {
		kiroHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	// The Bedrock invoke itself is allowed to fail (no live AWS creds here) — this
	// test is only about whether web_search's MCP relay was hijacked, not whether
	// the Bedrock round-trip succeeds. A plain transport error lands in
	// handleAccountFailure's default branch (soft RecordError), never a ban.
	prevBedrockClient := bedrockHTTPClientFor
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("bedrock invoke intentionally fails in this test")
			}),
		}
	}
	t.Cleanup(func() { bedrockHTTPClientFor = prevBedrockClient })

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if mcpCalls.Load() != 0 {
		t.Fatalf("web_search MCP relay was called %d times against a Bedrock-only pool", mcpCalls.Load())
	}
	got, ok := config.GetAccountByID("bedrock-only")
	if !ok {
		t.Fatal("bedrock account disappeared")
	}
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf("Bedrock account must not be banned by a skipped/failed web_search dispatch: enabled=%v ban=%q", got.Enabled, got.BanStatus)
	}
}
