package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// TestReasoningBuildErrorDoesNotFailAnAllBedrockPool pins F2: a mixed/pure
// Bedrock (or custom_api) pool must not 400 a request whose `thinking` field
// fails validation against a Kiro-derived schema — that schema (cachedModels)
// is populated exclusively by probing Kiro accounts, so it has nothing to do
// with how an all-Bedrock pool would actually serve this request (Bedrock
// speaks native Anthropic Messages directly; thinking.type:"enabled" needs no
// Kiro schema support at all).
//
// Setup: a single Bedrock-only account, and a cached model schema for
// "claude-sonnet-4.5" that (as if warmed from some other, now-gone Kiro
// account) exposes only thinking.type:"disabled". A client request for
// thinking.type:"enabled" against that model must reach the Bedrock dispatch
// branch instead of being rejected pre-dispatch with a 400 "does not support
// thinking.type" error. The Bedrock invoke itself is made to fail
// (intentional transport error, no live AWS creds in a test) so this test
// only proves the reasoning gate was bypassed, not that Bedrock succeeds.
//
// Reverting the `h.reasoningBuildErrorIsFatal()` guard in
// handleClaudeMessagesInternal (handler.go) makes this fail: buildErr would
// unconditionally 400 before the dispatch loop ever runs, and bedrockCalls
// would stay at 0.
func TestReasoningBuildErrorDoesNotFailAnAllBedrockPool(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "bedrock-only", AuthMethod: "bedrock", Enabled: true,
		BedrockAccessKeyID: "AKIA...", BedrockSecretAccessKey: "secret", Region: "us-east-1",
		// resolveBedrockModelID has no built-in default for "claude-sonnet-4.5"
		// (only "claude-sonnet-4-5"), so map it explicitly — otherwise the
		// request fails before reaching the HTTP layer this test stubs, and
		// bedrockCalls would stay 0 for a reason unrelated to what's under test.
		BedrockModelMap: map[string]string{"claude-sonnet-4.5": "us.anthropic.claude-sonnet-4-5-20250929-v1:0"},
	}); err != nil {
		t.Fatalf("add bedrock account: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	// Cache-read-only per reasoningCapabilityForModel's contract (see
	// reasoning_capability_hotpath_test.go): setting h.cachedModels directly
	// simulates an already-warm cache without touching the network.
	h.cachedModels = []ModelInfo{{
		ModelId: "claude-sonnet-4.5",
		AdditionalModelRequestFieldsSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"thinking": {
					"type": "object",
					"properties": {
						"type": { "enum": ["disabled"] }
					}
				}
			}
		}`),
	}}

	// The Bedrock invoke is intentionally made to fail with a plain transport
	// error — this test is only about whether the reasoning-schema gate let
	// the request past the pre-dispatch 400, not about a successful Bedrock
	// round-trip. bedrockCalls proves dispatch actually reached Bedrock.
	var bedrockCalls atomic.Int32
	prevBedrockClient := bedrockHTTPClientFor
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				bedrockCalls.Add(1)
				return nil, fmt.Errorf("bedrock invoke intentionally fails in this test")
			}),
		}
	}
	t.Cleanup(func() { bedrockHTTPClientFor = prevBedrockClient })

	reqBody := `{"model":"claude-sonnet-4.5","stream":false,"max_tokens":20000,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":10000}}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	body := rec.Body.String()
	if rec.Code == 400 && strings.Contains(body, "does not support thinking.type") {
		t.Fatalf("request was rejected pre-dispatch by the Kiro reasoning schema despite an all-Bedrock pool: status=%d body=%s", rec.Code, body)
	}
	if bedrockCalls.Load() == 0 {
		t.Fatalf("expected the request to reach the Bedrock dispatch branch, got 0 Bedrock invoke attempts; body=%s", body)
	}
}

// TestHandleCountTokensDoesNotFailAnAllBedrockPool covers the same defect on
// the /v1/messages/count_tokens entrypoint, which (unlike the main Claude
// handler) has no account-dispatch loop at all — reasoningBuildErrorIsFatal
// is the only place this decision can be made for that endpoint.
func TestHandleCountTokensDoesNotFailAnAllBedrockPool(t *testing.T) {
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

	h.cachedModels = []ModelInfo{{
		ModelId: "claude-sonnet-4.5",
		AdditionalModelRequestFieldsSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"thinking": {
					"type": "object",
					"properties": {
						"type": { "enum": ["disabled"] }
					}
				}
			}
		}`),
	}}

	reqBody := `{"model":"claude-sonnet-4.5","max_tokens":20000,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":10000}}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.handleCountTokens(rec, req)

	if rec.Code == 400 {
		t.Fatalf("count_tokens 400'd against an all-Bedrock pool: body=%s", rec.Body.String())
	}
	var resp map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected a valid input_tokens response, got status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	if resp["input_tokens"] < 1 {
		t.Fatalf("expected a positive input_tokens estimate, got %v", resp)
	}
}
