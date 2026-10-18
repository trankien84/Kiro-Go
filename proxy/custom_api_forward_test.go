package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

type customApiRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn customApiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type customApiPartialErrorBody struct {
	payload []byte
	err     error
	read    bool
}

func (body *customApiPartialErrorBody) Read(dst []byte) (int, error) {
	if body.read {
		return 0, io.EOF
	}
	body.read = true
	return copy(dst, body.payload), body.err
}

func (*customApiPartialErrorBody) Close() error { return nil }

func customApiResponseWithReadError(status int, payload string, readErr error) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body: &customApiPartialErrorBody{
			payload: []byte(payload),
			err:     readErr,
		},
	}
}

// newCustomApiTestHandler initializes an isolated config store, adds the given
// custom_api account, and returns a Handler wired to the reloaded pool.
func newCustomApiTestHandler(t *testing.T, acc config.Account) *Handler {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if acc.ID != "" {
		if err := config.AddAccount(acc); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}
	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingStats)
	h := &Handler{pool: p, customApiLedger: newCustomApiCreditLedger()}
	t.Cleanup(h.Close)
	return h
}

// A live upstream key with remaining credits passes the quota gate.
func TestProbeCustomApiQuotaAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-up" {
			t.Fatalf("missing bearer, got %q", got)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"creditsRemaining": 12.5, "tokensRemaining": 0,
			"creditLimit": 100.0, "tokenLimit": 0,
		})
	}))
	defer srv.Close()

	q, err := probeCustomApiQuota(srv.URL, "sk-up")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !customApiQuotaAcceptable(q) {
		t.Fatalf("expected acceptable, got %+v", q)
	}
}

// When /api/me is unavailable, the quota probe falls back to POST /checkkey/info
// with {"key": apiKey} and parses the same credit/token fields.
func TestProbeCustomApiQuotaFallsBackToCheckKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			w.WriteHeader(http.StatusNotFound)
		case "/checkkey/info":
			if r.Method != http.MethodPost {
				t.Fatalf("checkkey must be POST, got %s", r.Method)
			}
			var b struct {
				Key string `json:"key"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			if b.Key != "sk-abc" {
				t.Fatalf("checkkey body key=%q", b.Key)
			}
			w.Write([]byte(`{"found":true,"creditsUsed":0,"creditLimit":50000,"creditsRemaining":50000}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	q, err := probeCustomApiQuota(srv.URL, "sk-abc")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !q.OK || q.CreditLimit != 50000 {
		t.Fatalf("checkkey quota not parsed: %+v", q)
	}
}

// checkkey found:false → treated as an invalid key (probe error).
func TestCheckKeyNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"found":false}`))
	}))
	defer srv.Close()
	if _, err := probeCustomApiQuota(srv.URL, "bad"); err == nil {
		t.Fatalf("expected error when checkkey reports found:false")
	}
}

// A key whose limits are set but remaining is zero is rejected.
func TestCustomApiQuotaRejectZero(t *testing.T) {
	q := &customApiQuota{CreditsRemaining: 0, TokensRemaining: 0, CreditLimit: 100, TokenLimit: 0, OK: true}
	if customApiQuotaAcceptable(q) {
		t.Fatalf("expected rejection for zero remaining with a limit")
	}
}

// Unlimited (both limits zero) is accepted even with zero "remaining".
func TestCustomApiQuotaAcceptUnlimited(t *testing.T) {
	q := &customApiQuota{CreditsRemaining: 0, TokensRemaining: 0, CreditLimit: 0, TokenLimit: 0, OK: true}
	if !customApiQuotaAcceptable(q) {
		t.Fatalf("expected unlimited to be accepted")
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	got, err := normalizeBaseURL("https://pool.example.com/")
	if err != nil || got != "https://pool.example.com" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := normalizeBaseURL("pool.example.com"); err == nil {
		t.Fatalf("expected error for scheme-less url")
	}
	if _, err := normalizeBaseURL("http://"); err == nil {
		t.Fatalf("expected error for empty host")
	}
	if _, err := normalizeBaseURL("https://x.com/?a=1"); err == nil {
		t.Fatalf("expected error for query string")
	}
	if got, err := normalizeBaseURL("https://x.com/gw/"); err != nil || got != "https://x.com/gw" {
		t.Fatalf("path preserved: got %q err %v", got, err)
	}
}

// A request already marked as forwarded must NOT be re-forwarded to a custom_api
// account, and the healthy account must not be disabled (loop-guard is not a failure).
func TestForwardedRequestSkipsCustomApiWithoutBan(t *testing.T) {
	// forwardUpstreamRequest must never be called on this path.
	orig := forwardUpstreamRequest
	called := false
	forwardUpstreamRequest = func(ctx context.Context, method, url, apiKey string, body []byte, stream bool, semanticHeaders http.Header) (*http.Response, error) {
		called = true
		return nil, nil
	}
	defer func() { forwardUpstreamRequest = orig }()

	acc := config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	}
	h := newCustomApiTestHandler(t, acc)

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	req.Header.Set(forwardHeader, "1")
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if called {
		t.Fatalf("forwardUpstreamRequest must not run for an already-forwarded request")
	}
	if got, _ := config.GetAccountByID("c1"); got.Enabled != true || got.BanStatus != "" {
		t.Fatalf("healthy custom_api account must not be banned; enabled=%v ban=%q", got.Enabled, got.BanStatus)
	}
}

// custom_api accounts must never enter token refresh — ensureValidToken is a no-op.
func TestEnsureValidTokenSkipsCustomApi(t *testing.T) {
	h := newCustomApiTestHandler(t, config.Account{})
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "k"}
	if err := h.ensureValidToken(acc); err != nil {
		t.Fatalf("expected nil for custom_api, got %v", err)
	}
}

// Non-streaming forward proxies the body to {baseURL}/v1/messages and copies the
// JSON reply back verbatim.
func TestForwardNonStreamPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The post-response real-cost probe hits the quota endpoint; answer it so the
		// test can focus on the chat passthrough assertions below.
		if r.URL.Path == "/api/me" || r.URL.Path == "/checkkey/info" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"found":true,"creditsUsed":0,"creditLimit":1000}`))
			return
		}
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-up" {
			t.Fatalf("auth %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get(forwardHeader) != "1" {
			t.Fatalf("missing forward marker header")
		}
		if got := r.Header.Get("anthropic-beta"); got != "mcp-client-2025-11-20,compact-2026-01-12" {
			t.Fatalf("anthropic-beta=%q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version=%q", got)
		}
		if got := r.Header.Get("anthropic-user-profile-id"); got != "profile-123" {
			t.Fatalf("anthropic-user-profile-id=%q", got)
		}
		if got := r.Header.Get("X-Unrelated"); got != "" {
			t.Fatalf("unrelated header leaked upstream: %q", got)
		}
		if got := r.Header.Get("Anthropic-Internal-Test"); got != "" {
			t.Fatalf("non-allowlisted anthropic header leaked upstream: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("hello")) {
			t.Fatalf("body not forwarded: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer upstream.Close()

	acc := config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: &acc, body: []byte(`{"model":"x","messages":[{"role":"user","content":"hello"}]}`),
		endpoint: "anthropic", streaming: false, model: "x",
		semanticHeaders: http.Header{
			"Anthropic-Beta":            []string{"mcp-client-2025-11-20,compact-2026-01-12"},
			"Anthropic-Version":         []string{"2023-06-01"},
			"Anthropic-User-Profile-Id": []string{"profile-123"},
			"Anthropic-Internal-Test":   []string{"must-not-forward"},
			"X-Unrelated":               []string{"must-not-forward"},
		},
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"m1"`)) {
		t.Fatalf("reply not copied: %s", rec.Body.String())
	}
}

// Custom API traffic bills the customer's key/pool in credits derived from tokens at
// the configured rate (upstream reply has no credit figure). Without this, credit-
// limited keys never exhaust.
func TestCustomApiChargesCredits(t *testing.T) {
	t.Setenv("CUSTOM_API_CREDITS_PER_1K_TOKENS", "2.0")
	if got := customApiCreditsPer1kTokens(); got != 2.0 {
		t.Fatalf("rate override: got %v", got)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","usage":{"input_tokens":300,"output_tokens":700}}`))
	}))
	defer upstream.Close()

	acc := config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	rec := httptest.NewRecorder()
	if err := h.forwardToUpstream(rec, nil, forwardParams{
		account: &acc, body: []byte(`{}`), endpoint: "anthropic", streaming: false, model: "x",
	}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	// 1000 tokens / 1000 * 2.0 = 2.0 credits.
	if got := h.getCredits(); got < 1.99 || got > 2.01 {
		t.Fatalf("expected ~2.0 credits charged, got %v", got)
	}
}

// Real-cost billing: each request is charged its own tokens times the pool's blended
// per-token price (creditsUsed / tokensUsed), NOT a flat token price and NOT a
// shared-account delta. Every request of the same size bills the same amount
// regardless of what other customers on the shared pool key are spending.
func TestCustomApiBillsPoolRate(t *testing.T) {
	t.Setenv("CUSTOM_API_CREDITS_PER_1K_TOKENS", "1.0") // deliberately far from the real rate

	// Pool reports a stable blended price: 3000 credits over 150M tokens = 2e-5 cr/token
	// (0.02 cr/1K). Chat usage is 1000 tokens/request, so each request must bill ~0.02.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			w.WriteHeader(http.StatusNotFound)
		case "/checkkey/info":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"found": true, "creditsUsed": 3000.0, "tokensUsed": 150000000, "creditLimit": 100000.0,
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"m1","usage":{"input_tokens":300,"output_tokens":700}}`))
		}
	}))
	defer upstream.Close()

	acc := config.Account{ID: "cpr1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	forward := func() float64 {
		t.Helper()
		before := h.getCredits()
		if err := h.forwardToUpstream(httptest.NewRecorder(), nil, forwardParams{
			account: &acc, body: []byte(`{}`), endpoint: "anthropic", streaming: false, model: "x",
		}); err != nil {
			t.Fatalf("forward: %v", err)
		}
		return h.getCredits() - before
	}

	// 1000 tokens * (3000/150M) = 0.02. Every request the same, from the first — no seed
	// ramp, no lump. Flat tokens/1000*1.0 would be 1.0 (50x too high).
	for i := 0; i < 5; i++ {
		if got := forward(); got < 0.0199 || got > 0.0201 {
			t.Fatalf("request %d should bill ~0.02 (pool rate), got %v", i, got)
		}
	}
}

// When the rate probe fails and nothing is cached yet, billing falls back to the flat
// token estimate — never zero, never a spike.
func TestCustomApiBillsFallbackWhenNoRate(t *testing.T) {
	t.Setenv("CUSTOM_API_CREDITS_PER_1K_TOKENS", "0.03")

	// Pool answers chats but its quota endpoint is down (both probe paths 500).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me", "/checkkey/info":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"m1","usage":{"input_tokens":300,"output_tokens":700}}`))
		}
	}))
	defer upstream.Close()

	acc := config.Account{ID: "cfb1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	if err := h.forwardToUpstream(httptest.NewRecorder(), nil, forwardParams{
		account: &acc, body: []byte(`{}`), endpoint: "anthropic", streaming: false, model: "x",
	}); err != nil {
		t.Fatalf("forward: %v", err)
	}
	// 1000 tokens / 1000 * 0.03 = 0.03 estimate.
	if got := h.getCredits(); got < 0.0299 || got > 0.0301 {
		t.Fatalf("no-rate fallback should bill the estimate ~0.03, got %v", got)
	}
}

// Upstream /api/me credits map onto the panel's main-quota fields.
func TestCustomApiQuotaToAccountInfo(t *testing.T) {
	q := &customApiQuota{CreditsUsed: 25, CreditLimit: 100, OK: true}
	info := q.toAccountInfo(12345)
	if info.UsageCurrent != 25 || info.UsageLimit != 100 {
		t.Fatalf("usage: %+v", info)
	}
	if info.UsagePercent < 0.249 || info.UsagePercent > 0.251 {
		t.Fatalf("percent: %v", info.UsagePercent)
	}
	if info.LastRefresh != 12345 || info.SubscriptionType != "Custom API" {
		t.Fatalf("meta: %+v", info)
	}
	// Unlimited (limit 0) → no percent, no div-by-zero.
	if u := (&customApiQuota{CreditsUsed: 5, CreditLimit: 0, OK: true}).toAccountInfo(1); u.UsagePercent != 0 {
		t.Fatalf("unlimited percent should be 0, got %v", u.UsagePercent)
	}
}

// Model list comes from the upstream provider's /v1/models, not Kiro.
func TestProbeCustomApiModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-up" {
			t.Fatalf("auth %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"claude-opus-4.8"},{"id":"gpt-4o"},{"id":""}]}`))
	}))
	defer upstream.Close()

	models, err := probeCustomApiModels(upstream.URL, "sk-up")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(models) != 2 || models[0].ModelId != "claude-opus-4.8" || models[1].ModelId != "gpt-4o" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

// The admin test button sends a real chat request through the upstream and returns
// the assistant reply text.
func TestCustomApiTestReply(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok!"}}]}`))
	}))
	defer upstream.Close()

	reply, err := customApiTestReply(context.Background(), upstream.URL, "sk-up", "claude-opus-4.8")
	if err != nil {
		t.Fatalf("test reply: %v", err)
	}
	if reply != "ok!" {
		t.Fatalf("reply %q", reply)
	}
}

func TestCustomApiTestReplyPropagatesCancellation(t *testing.T) {
	original := forwardUpstreamRequest
	defer func() { forwardUpstreamRequest = original }()

	started := make(chan struct{})
	forwardUpstreamRequest = func(
		ctx context.Context,
		method,
		url,
		apiKey string,
		body []byte,
		stream bool,
		semanticHeaders http.Header,
	) (*http.Response, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := customApiTestReply(ctx, "https://upstream.invalid", "sk-up", "model")
		done <- err
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("custom API test did not stop after context cancellation")
	}
}

func TestCustomApiTestReplyRejectsPartialBodyRead(t *testing.T) {
	original := forwardUpstreamRequest
	defer func() { forwardUpstreamRequest = original }()

	readErr := errors.New("synthetic body failure")
	forwardUpstreamRequest = func(
		context.Context,
		string,
		string,
		string,
		[]byte,
		bool,
		http.Header,
	) (*http.Response, error) {
		return customApiResponseWithReadError(
			http.StatusOK,
			`{"choices":[{"message":{"content":"must-not-succeed"}}]}`,
			readErr,
		), nil
	}

	_, err := customApiTestReply(
		context.Background(),
		"https://upstream.invalid",
		"sk-up",
		"model",
	)
	if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "read response body") {
		t.Fatalf("expected body-read error, got %v", err)
	}
}

func TestCustomApiProbesRejectPartialBodyReads(t *testing.T) {
	originalClient := customAPIDirectClient
	defer func() { customAPIDirectClient = originalClient }()

	readErr := errors.New("synthetic probe body failure")
	customAPIDirectClient = &http.Client{
		Transport: customApiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/me":
				return customApiResponseWithReadError(
					http.StatusOK,
					`{"creditsRemaining":1,"creditLimit":10}`,
					readErr,
				), nil
			case "/checkkey/info":
				return customApiResponseWithReadError(
					http.StatusOK,
					`{"found":true,"creditsRemaining":1,"creditLimit":10}`,
					readErr,
				), nil
			case "/v1/models":
				return customApiResponseWithReadError(
					http.StatusOK,
					`{"data":[{"id":"must-not-succeed"}]}`,
					readErr,
				), nil
			default:
				return nil, errors.New("unexpected probe path: " + req.URL.Path)
			}
		}),
	}

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "api-me",
			run: func() error {
				_, err := probeQuotaViaApiMe(context.Background(), "https://upstream.invalid", "sk-up")
				return err
			},
		},
		{
			name: "check-key",
			run: func() error {
				_, err := probeQuotaViaCheckKey(context.Background(), "https://upstream.invalid", "sk-up")
				return err
			},
		},
		{
			name: "models",
			run: func() error {
				_, err := probeCustomApiModels("https://upstream.invalid", "sk-up")
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "read response body") {
				t.Fatalf("expected body-read error, got %v", err)
			}
		})
	}
}

// A request that already carries the forwarded marker is refused (loop guard).
func TestForwardLoopGuard(t *testing.T) {
	h := newCustomApiTestHandler(t, config.Account{})
	acc := &config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: "https://x", KiroApiKey: "k"}
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: acc, body: []byte(`{}`), endpoint: "anthropic", forwarded: true,
	})
	if err == nil {
		t.Fatalf("expected loop-guard error")
	}
}

// Streaming forward copies the upstream SSE bytes straight to the client.
func TestForwardStreamPassthrough(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	acc := config.Account{ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up", Enabled: true}
	h := newCustomApiTestHandler(t, acc)
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: &acc, body: []byte(`{"stream":true}`), endpoint: "anthropic", streaming: true, model: "x",
	})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("message_stop")) {
		t.Fatalf("stream not copied: %s", rec.Body.String())
	}
}

// A request that routes to a custom_api account is proxied end-to-end through the
// public Claude entry handler.
func TestClaudeEntryForwardsCustomApi(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`))
	}))
	defer upstream.Close()

	acc := config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	}
	h := newCustomApiTestHandler(t, acc)

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"m1"`)) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

// When the upstream pool returns 500, the custom_api account is excluded and the
// request fails over. With no other account, the caller sees an error (not a hang).
func TestForwardFailoverOn5xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer upstream.Close()

	acc := config.Account{
		ID: "c1", AuthMethod: "custom_api", BaseURL: upstream.URL, KiroApiKey: "sk-up",
		AccessToken: "sk-up", OrderID: "ORD-1", Enabled: true,
	}
	h := newCustomApiTestHandler(t, acc)

	reqBody := `{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(reqBody)))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 after upstream 5xx with no fallback account")
	}
}

func TestClaudeRequestPlanUsesSemanticProfileHeader(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	headers := http.Header{
		"Anthropic-Beta":            []string{"user-profiles-2026-03-24"},
		"Anthropic-User-Profile-Id": []string{"profile-123"},
	}
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		headers,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	want := claudeTransportMaskKiro | claudeTransportMaskCustom
	if plan.Requirements.Allowed != want ||
		!plan.Requirements.AllowsKiro() ||
		!plan.Requirements.Allows(claudeTransportDirectAnthropic) ||
		!strings.Contains(plan.Requirements.Summary(), "user_profile_id") {
		t.Fatalf("profile requirements=%#v summary=%q", plan.Requirements, plan.Requirements.Summary())
	}
	if plan.KiroProjection.Fidelity != claudeProjectionEmulated ||
		!strings.Contains(plan.KiroProjection.Summary(), "anthropic_user_profile") {
		t.Fatalf("profile projection=%s summary=%q", plan.KiroProjection.Fidelity, plan.KiroProjection.Summary())
	}
	if plan.Semantic.UserProfileID != "profile-123" {
		t.Fatalf("profile id=%q", plan.Semantic.UserProfileID)
	}

	emptyHeaders := http.Header{
		"Anthropic-User-Profile-Id": []string{"   "},
	}
	emptyPlan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		emptyHeaders,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	if emptyPlan.Semantic.UserProfileID != "" ||
		emptyPlan.Requirements.Allowed != claudeTransportMaskAll {
		t.Fatalf("empty profile changed routing: semantic=%#v requirements=%#v", emptyPlan.Semantic, emptyPlan.Requirements)
	}
}

func TestClaudeProfileHeaderKeepsKiroCandidateEligible(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	headers := http.Header{
		"Anthropic-Beta":            []string{"user-profiles-2026-03-24"},
		"Anthropic-User-Profile-Id": []string{"profile-123"},
	}
	plan, validationErr := buildClaudeRequestPlanWithResolver(
		body,
		headers,
		"-thinking",
		noClaudeReasoningCapability,
	)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	account := &config.Account{ID: "kiro-only", AuthMethod: "social", Enabled: true}
	if !claudeAccountCompatibleWithRequirements(account, false, plan.Requirements) {
		t.Fatalf("Kiro account rejected provider-profile metadata: %s", plan.Requirements.Summary())
	}
}

func TestCORSAllowsAnthropicUserProfileHeader(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodOptions, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent ||
		!strings.Contains(
			strings.ToLower(
				rec.Header().Get("Access-Control-Allow-Headers"),
			),
			"anthropic-user-profile-id",
		) {
		t.Fatalf(
			"status=%d allowHeaders=%q",
			rec.Code,
			rec.Header().Get("Access-Control-Allow-Headers"),
		)
	}
}

func TestForwardCustomAPIUsesRequestContext(t *testing.T) {
	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			close(started)
			<-r.Context().Done()
			close(upstreamCanceled)
		},
	))
	defer upstream.Close()
	defer upstream.CloseClientConnections()

	acc := config.Account{
		ID:         "c1",
		AuthMethod: "custom_api",
		BaseURL:    upstream.URL,
		KiroApiKey: "sk-up",
		Enabled:    true,
	}
	h := newCustomApiTestHandler(t, acc)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- h.forwardToUpstream(
			httptest.NewRecorder(),
			nil,
			forwardParams{
				account:        &acc,
				body:           []byte(`{}`),
				endpoint:       "anthropic",
				requestContext: ctx,
			},
		)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("forward error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forward did not stop after client cancellation")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		upstream.CloseClientConnections()
		t.Fatal("upstream request context was not canceled")
	}
}

func TestClaudeCancellationDoesNotPenalizeCustomAPIAccount(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			close(started)
			<-r.Context().Done()
		},
	))
	defer upstream.Close()
	defer upstream.CloseClientConnections()

	acc := config.Account{
		ID:          "c1",
		AuthMethod:  "custom_api",
		BaseURL:     upstream.URL,
		KiroApiKey:  "sk-up",
		AccessToken: "sk-up",
		OrderID:     "ORD-1",
		Enabled:     true,
	}
	h := newCustomApiTestHandler(t, acc)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleClaudeNonStream(
			httptest.NewRecorder(),
			&KiroPayload{},
			"claude-sonnet-4.5",
			false,
			claudeThinkingResponseOptions{},
			1,
			nil,
			"",
			[]byte(`{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`),
			false,
			nil,
			ctx,
		)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		upstream.CloseClientConnections()
		t.Fatal("Claude handler did not stop after cancellation")
	}

	got, exists := config.GetAccountByID("c1")
	if !exists {
		t.Fatal("custom API account disappeared")
	}
	if !got.Enabled || got.BanStatus != "" {
		t.Fatalf(
			"client cancellation penalized account: enabled=%t ban=%q",
			got.Enabled,
			got.BanStatus,
		)
	}
}

func TestForwardNonStreamRejectsOversizedResponseBeforeCommit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat(
				[]byte{'x'},
				(32<<20)+1,
			))
		},
	))
	defer upstream.Close()

	acc := config.Account{
		ID:         "c1",
		AuthMethod: "custom_api",
		BaseURL:    upstream.URL,
		KiroApiKey: "sk-up",
		Enabled:    true,
	}
	h := newCustomApiTestHandler(t, acc)
	rec := httptest.NewRecorder()
	err := h.forwardToUpstream(rec, nil, forwardParams{
		account: &acc, body: []byte(`{}`), endpoint: "anthropic",
		requestContext: context.Background(),
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error=%v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("oversized response was partially committed: %d bytes", rec.Body.Len())
	}
}

type failingForwardResponseWriter struct {
	header http.Header
	status int
}

func (w *failingForwardResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingForwardResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *failingForwardResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestForwardNonStreamDoesNotRecordSuccessAfterClientWriteFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(
				`{"usage":{"input_tokens":10,"output_tokens":20}}`,
			))
		},
	))
	defer upstream.Close()

	acc := config.Account{
		ID:         "c1",
		AuthMethod: "custom_api",
		BaseURL:    upstream.URL,
		KiroApiKey: "sk-up",
		Enabled:    true,
	}
	h := newCustomApiTestHandler(t, acc)
	writer := &failingForwardResponseWriter{}
	if err := h.forwardToUpstream(writer, nil, forwardParams{
		account: &acc, body: []byte(`{}`), endpoint: "anthropic",
		requestContext: context.Background(),
	}); err != nil {
		t.Fatalf("committed downstream failure must be terminal: %v", err)
	}
	if got := h.getCredits(); got != 0 {
		t.Fatalf("write failure recorded success credits=%v", got)
	}
}

func TestReadCustomAPIResponseBodyEnforcesHardLimit(t *testing.T) {
	const limit = int64(16)

	exact, err := readCustomAPIResponseBody(
		bytes.NewReader(bytes.Repeat([]byte("x"), int(limit))),
		limit,
	)
	if err != nil || len(exact) != int(limit) {
		t.Fatalf("exact limit: bytes=%d err=%v", len(exact), err)
	}

	oversized, err := readCustomAPIResponseBody(
		bytes.NewReader(bytes.Repeat([]byte("x"), int(limit+1))),
		limit,
	)
	if len(oversized) != 0 || !errors.Is(err, errCustomAPIResponseTooLarge) {
		t.Fatalf("oversized: bytes=%d err=%v", len(oversized), err)
	}

	validPrefix := append(
		[]byte(`{"data":[]}`),
		bytes.Repeat([]byte(" "), int(limit))...,
	)
	if _, err := readCustomAPIResponseBody(
		bytes.NewReader(validPrefix),
		limit,
	); !errors.Is(err, errCustomAPIResponseTooLarge) {
		t.Fatalf("valid JSON prefix with oversized suffix was accepted: %v", err)
	}
}
