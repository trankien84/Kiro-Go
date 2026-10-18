package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhase4ClaudeStopReasonMappingPreservesTerminalSemantics(t *testing.T) {
	tests := []struct {
		upstream string
		tools    int
		want     string
	}{
		{upstream: "MAX_TOKENS", want: "max_tokens"},
		{upstream: "max_output_tokens", want: "max_tokens"},
		{upstream: "context_window_exceeded", want: "model_context_window_exceeded"},
		{upstream: "TOOL_USE", tools: 1, want: "tool_use"},
		{upstream: "STOP_SEQUENCE", want: "stop_sequence"},
		{upstream: "CONTENT_FILTER", want: "refusal"},
		{upstream: "future_terminal_reason", want: "future_terminal_reason"},
		{upstream: "", tools: 1, want: "tool_use"},
		{upstream: "", want: "end_turn"},
	}
	for _, tt := range tests {
		if got := resolveClaudeHandlerStopReason(tt.upstream, tt.tools); got != tt.want {
			t.Fatalf("resolveClaudeHandlerStopReason(%q, %d)=%q want %q", tt.upstream, tt.tools, got, tt.want)
		}
	}
}

func TestPhase4OpenAIStopReasonMappingPreservesTerminalSemantics(t *testing.T) {
	tests := []struct {
		upstream string
		tools    int
		want     string
	}{
		{upstream: "MAX_TOKENS", want: "length"},
		{upstream: "context_window_exceeded", want: "length"},
		{upstream: "TOOL_USE", tools: 1, want: "tool_calls"},
		{upstream: "CONTENT_FILTER", want: "content_filter"},
		{upstream: "STOP_SEQUENCE", want: "stop"},
		{upstream: "END_TURN", want: "stop"},
		{upstream: "future_terminal_reason", want: "future_terminal_reason"},
		{upstream: "", tools: 1, want: "tool_calls"},
		{upstream: "", want: "stop"},
	}
	for _, tt := range tests {
		if got := resolveOpenAIHandlerFinishReason(tt.upstream, tt.tools); got != tt.want {
			t.Fatalf("resolveOpenAIHandlerFinishReason(%q, %d)=%q want %q", tt.upstream, tt.tools, got, tt.want)
		}
	}
}

func TestPhase4PureNativeWebSearchAllowsKiroMCPOrDirectAnthropic(t *testing.T) {
	requirements := claudeTransportRequirements{Allowed: claudeTransportMaskAll}
	claudeToolTransportRequirements(&requirements, []ClaudeTool{{
		Type: "web_search_20250305",
		Name: webSearchToolName,
	}})
	want := claudeTransportMaskKiro | claudeTransportMaskCustom
	if requirements.Allowed != want {
		t.Fatalf("pure web_search allowed mask=%b want=%b", requirements.Allowed, want)
	}
	if !requirements.RequiresKiroWebSearchMCP {
		t.Fatal("pure native web_search must mark the Kiro-MCP/direct routing path")
	}
}

func TestPhase4MixedNativeWebSearchAndClientToolsRemainDirectOnly(t *testing.T) {
	requirements := claudeTransportRequirements{Allowed: claudeTransportMaskAll}
	claudeToolTransportRequirements(&requirements, []ClaudeTool{
		{Type: "web_search_20250305", Name: webSearchToolName},
		{Name: "edit", InputSchema: map[string]interface{}{"type": "object"}},
	})
	if requirements.Allowed != claudeTransportMaskCustom {
		t.Fatalf("mixed web_search/client tools allowed mask=%b want direct-only=%b", requirements.Allowed, claudeTransportMaskCustom)
	}
	if requirements.RequiresKiroWebSearchMCP {
		t.Fatal("mixed server/client tools must not enter the one-shot Kiro MCP adapter")
	}
}

func TestPhase4RegularClientToolNamedWebSearchIsNotHijacked(t *testing.T) {
	requirements := claudeTransportRequirements{Allowed: claudeTransportMaskAll}
	claudeToolTransportRequirements(&requirements, []ClaudeTool{{
		Name:        webSearchToolName,
		InputSchema: map[string]interface{}{"type": "object"},
	}})
	if requirements.Allowed != claudeTransportMaskAll {
		t.Fatalf("ordinary client tool named web_search unexpectedly narrowed transport mask=%b", requirements.Allowed)
	}
	if requirements.RequiresKiroWebSearchMCP {
		t.Fatal("ordinary client tool named web_search must not be treated as Anthropic native web_search")
	}
}

func TestPhase4ClaudeResponsePreservesUpstreamMaxTokensStopReason(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "test-account", AuthMethod: "social", Enabled: true,
		AccessToken: "token-test", ProfileArn: "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "MAX_TOKENS",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	t.Cleanup(func() {
		h.Close()
		p.WaitForPendingStats()
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if response.StopReason != "max_tokens" {
		t.Fatalf("stop_reason=%q, want max_tokens", response.StopReason)
	}
}

func TestPhase4OpenAIStreamPreservesUpstreamMaxTokensFinishReason(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "test-account", AuthMethod: "social", Enabled: true,
		AccessToken: "token-test", ProfileArn: "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "MAX_TOKENS",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	t.Cleanup(func() {
		h.Close()
		p.WaitForPendingStats()
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected stream status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"length"`) {
		t.Fatalf("expected finish_reason=length, got %s", rec.Body.String())
	}
}
