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
	t.Cleanup(func() {
		close(release)
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

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
	t.Cleanup(func() {
		close(release)
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	for i := 0; i < 8; i++ {
		h.warmModelsCacheAsync()
	}

	// Give the goroutines time to reach the transport before counting.
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&calls); got > 1 {
		t.Fatalf("warmModelsCacheAsync fanned out %d refreshes for one cold burst, want at most 1", got)
	}
}
