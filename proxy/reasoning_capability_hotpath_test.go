package proxy

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// TestReasoningCapabilityForModelDoesNotRefreshInline pins the hot-path contract
// of reasoningCapabilityForModel: it is a CACHE READ, never a synchronous
// refresh.
//
// The lookup runs on every Claude, count_tokens and OpenAI request, ahead of the
// per-account dispatch loop. An inline refreshModelsCache() there would fan out a
// live ListAvailableModels call per enabled account before the request is even
// translated — blocking the customer on unrelated network I/O, and dragging Kiro
// accounts (plus refreshModelsCache's handleAccountFailure call) into requests
// that may target Bedrock or custom_api, which CLAUDE.md requires stay out of
// every Kiro-facing path.
//
// The REST client is stubbed to hang, so a reintroduced inline refresh shows up
// as this test exceeding the deadline rather than as a silent latency
// regression.
func TestReasoningCapabilityForModelDoesNotRefreshInline(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// An enabled account with a token is what makes refreshModelsCache actually
	// attempt a network call — without one it returns early and the test would
	// pass even with the inline refresh restored.
	if err := config.AddAccount(config.Account{
		ID:          "acct-hotpath",
		AuthMethod:  "social",
		Enabled:     true,
		AccessToken: "token-x",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/TEST",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	// Block, don't error: an erroring stub returns instantly, so an inline
	// refresh would still look fast. Hanging is what separates "read the cache"
	// from "went to the network". Restore an inert stub afterward — the async
	// warm goroutine may still be in flight when this test returns, and it must
	// never reach the real ProxyFromEnvironment-backed client (that would cache
	// env vars in a process-wide sync.Once and poison later proxy tests).
	release := make(chan struct{})
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			<-release
			return nil, errTest("network disabled in test")
		}),
	})
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	t.Cleanup(func() {
		close(release)
		waitForWarmToFinish(t, h)
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	// Empty cachedModels = the cold-cache path under test.
	done := make(chan ModelReasoningCapability, 1)
	go func() {
		done <- h.reasoningCapabilityForModel("claude-sonnet-4.5")
	}()

	select {
	case capability := <-done:
		// A cold cache yields the same empty capability an unknown model gives.
		// Callers already treat that as "no reasoning fields", so the request
		// degrades instead of blocking.
		if capability.SupportsThinking {
			t.Fatalf("cold cache must not report thinking support, got %+v", capability)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reasoningCapabilityForModel blocked on a network refresh; it must be a cache read only")
	}
}

// TestWarmModelsCacheAsyncSingleFlights checks the cold-cache warm fans out one
// refresh for a burst of concurrent requests, not one per request. Each refresh
// is a live ListAvailableModels call per enabled account, so losing the
// single-flight turns a cold start into a stampede against Kiro.
func TestWarmModelsCacheAsyncSingleFlights(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "acct-singleflight",
		AuthMethod:  "social",
		Enabled:     true,
		AccessToken: "token-x",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/TEST",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	// Hold every in-flight request open so the CAS flag stays set for the whole
	// burst — otherwise an early completion would legitimately allow a second
	// refresh and the count would be racy rather than wrong.
	release := make(chan struct{})
	var calls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			<-release
			return nil, errTest("network disabled in test")
		}),
	})
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	t.Cleanup(func() {
		close(release)
		waitForWarmToFinish(t, h)
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	for i := 0; i < 8; i++ {
		h.warmModelsCacheAsync()
	}

	// Give the goroutines time to reach the transport before counting.
	time.Sleep(200 * time.Millisecond)

	// Exactly 1, not merely "at most 1": the account is enabled with a token, so
	// a working single-flight must produce one real attempt, not zero (a bug
	// that swallowed every call would satisfy `> 1` just as wrongly as a
	// stampede would).
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("warmModelsCacheAsync fanned out %d refreshes for one cold burst, want exactly 1", got)
	}
}

// waitForWarmToFinish polls until a warm goroutine's single-flight flag
// clears, so tests can deterministically observe post-attempt state instead
// of racing a fixed sleep against the background goroutine.
func waitForWarmToFinish(t *testing.T, h *Handler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&h.modelsWarming) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("warm goroutine did not finish in time")
}

// TestWarmModelsCacheAsyncIsRateLimited pins F5: single-flighting via the
// modelsWarming CAS alone does not protect a pool stuck in total outage.
// refreshModelsCache only commits cachedModels when it recovers at least one
// model, so a total outage (every Kiro account failing) leaves the cache
// permanently empty — and the CAS permanently available to re-arm the instant
// the failed attempt returns. Without lastWarmAttemptNanos, every subsequent
// cold request would kick a fresh live fan-out across every enabled account,
// feeding handleAccountFailure once per account per request and accelerating
// auto-ban during exactly the outage this cache exists to smooth over.
func TestWarmModelsCacheAsyncIsRateLimited(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "acct-ratelimit",
		AuthMethod:  "social",
		Enabled:     true,
		AccessToken: "token-x",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/TEST",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	var calls int32
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return nil, errTest("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	h.warmModelsCacheAsync()
	waitForWarmToFinish(t, h)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 initial warm attempt, got %d", got)
	}

	// The refresh failed (network disabled), so cachedModels is still empty —
	// exactly the total-outage shape the rate limit exists for. A request
	// arriving immediately after must be suppressed, not re-fire the fan-out.
	h.warmModelsCacheAsync()
	waitForWarmToFinish(t, h)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected the immediate re-attempt to be rate-limited, got %d total calls", got)
	}

	// Backdate the last attempt past the rate-limit window: a genuinely new
	// attempt (e.g. after 30s) must still be allowed through.
	atomic.StoreInt64(&h.lastWarmAttemptNanos, time.Now().Add(-minWarmModelsCacheInterval-time.Second).UnixNano())
	h.warmModelsCacheAsync()
	waitForWarmToFinish(t, h)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected a second attempt once the rate-limit window passed, got %d total calls", got)
	}
}
