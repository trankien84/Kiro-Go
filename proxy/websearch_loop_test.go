package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestWebSearchLoopBillsSumOfInputTokensAcrossRounds is the regression test for
// F5: runWebSearchLoop billed only the TERMINAL round's input tokens even
// though every round (search or terminal) is a separate real upstream call
// that resends a growing conversation. A token-budgeted customer key
// (TokenLimit set, CreditLimit 0 — the documented resale pattern) running a
// 3-round web search must have TokensUsed grow by the SUM of all 3 rounds'
// input tokens, not just the last one.
//
// Round 1 and 2 return a web_search tool_use (continue searching); round 3
// returns plain text with no tool_use (terminal flush). Each round reports a
// distinct inputTokens figure via its metadataEvent so under-accumulation is
// unambiguous: sum=1000+2000+3000=6000, last-only would report 3000.
func TestWebSearchLoopBillsSumOfInputTokensAcrossRounds(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	account := config.Account{
		ID:         "acc-1",
		Email:      "acc1@test",
		AuthMethod: "idc",
		Enabled:    true,
		// Non-empty so CallKiroAPIContext / callMcpAPI never need to resolve a
		// profile ARN over the network (which the stub transport below would
		// reject as an unexpected request).
		AccessToken: "tok",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/ABCDEF",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	keyEntry, err := config.AddApiKey(config.ApiKeyEntry{
		ID: "test-key-id", Key: "sk-test", Enabled: true, TokenLimit: 1_000_000,
	})
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	// runWebSearchLoop records account stats through the pool's asynchronous
	// persistence worker. Flush any writes admitted by this test before testing's
	// TempDir cleanup removes the config directory; otherwise the worker can race
	// with RemoveAll and intermittently recreate a file while cleanup is running.
	t.Cleanup(func() {
		if err := p.FlushStats(context.Background()); err != nil {
			t.Errorf("flush account stats: %v", err)
		}
	})
	h := &Handler{pool: p}

	// generateAssistantResponse calls (Kiro native stream) and MCP web_search
	// calls both route through GetClientForProxy -> kiroHttpStore when the
	// account carries no per-account proxy URL, so a single stub Transport on
	// kiroHttpStore intercepts every outbound request this test makes — no
	// real network, no httptest.Server needed for the Kiro/MCP endpoints
	// themselves.
	var kiroCalls atomic.Int32
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/mcp") {
			return mcpSearchResponse(), nil
		}
		if !strings.Contains(r.URL.Path, "/generateAssistantResponse") {
			return nil, fmt.Errorf("unexpected request to %s", r.URL)
		}
		n := kiroCalls.Add(1)
		switch n {
		case 1:
			return kiroToolUseRoundResponse(t, "toolu_1", 1000), nil
		case 2:
			return kiroToolUseRoundResponse(t, "toolu_2", 2000), nil
		case 3:
			return kiroFinalRoundResponse(t, 3000), nil
		default:
			return nil, fmt.Errorf("unexpected round %d", n)
		}
	})})
	defer kiroHttpStore.Store(oldClient)

	req := &ClaudeRequest{
		Model:  "claude-sonnet-4",
		Stream: false,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "search for golang generics"},
		},
		Tools: []ClaudeTool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Name: "Bash", Description: "run", InputSchema: map[string]interface{}{"type": "object"}},
		},
	}

	w := httptest.NewRecorder()
	h.runWebSearchLoop(context.Background(), w, req, false, 500, keyEntry.ID)

	if w.Code != 0 && w.Code != http.StatusOK {
		t.Fatalf("unexpected status %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, w.Body.String())
	}
	usage, _ := resp["usage"].(map[string]interface{})
	if usage == nil {
		t.Fatalf("no usage in response: %+v", resp)
	}
	gotInput, _ := usage["input_tokens"].(float64)
	if int(gotInput) != 6000 {
		t.Fatalf("response usage.input_tokens = %v, want 6000 (1000+2000+3000, not just the terminal round's 3000)", gotInput)
	}

	// The billing side effect (config.RecordApiKeyUsage via recordSuccessForApiKey)
	// must reflect the same sum, not the terminal round alone.
	updated := config.GetApiKeyEntry(keyEntry.ID)
	if updated == nil {
		t.Fatal("api key vanished")
	}
	if updated.TokensUsed < 6000 {
		t.Fatalf("TokensUsed = %d, want >= 6000 (input alone; output tokens add on top)", updated.TokensUsed)
	}
}

// kiroToolUseRoundResponse builds one generateAssistantResponse round that
// asks for a web_search tool_use and reports inputTokens via metadataEvent.
func kiroToolUseRoundResponse(t *testing.T, toolUseID string, inputTokens int) *http.Response {
	t.Helper()
	var body []byte
	body = append(body, awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": toolUseID, "name": "web_search", "input": `{"query":"golang generics"}`, "stop": true,
	})...)
	body = append(body, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
		"inputTokens": inputTokens,
	})...)
	return newFakeResponse(body)
}

// kiroFinalRoundResponse builds the terminal round: plain text + end_turn.
func kiroFinalRoundResponse(t *testing.T, inputTokens int) *http.Response {
	t.Helper()
	var body []byte
	body = append(body, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "Generics let you write reusable code.",
	})...)
	body = append(body, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
		"stopReason":  "end_turn",
		"inputTokens": inputTokens,
	})...)
	return newFakeResponse(body)
}

// mcpSearchResponse is a well-formed empty-results MCP JSON-RPC envelope —
// enough for performWebSearch/searchAllWebUses to succeed without needing
// real search content for this billing-focused test.
func mcpSearchResponse() *http.Response {
	payload := `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"{\"results\":[]}"}],"isError":false}}`
	return newFakeResponse([]byte(payload))
}

// newFakeResponse wraps a raw body in a minimal 200 OK http.Response, as
// returned by a stub http.RoundTripper.
func newFakeResponse(body []byte) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
