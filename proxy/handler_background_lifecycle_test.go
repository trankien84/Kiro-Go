package proxy

import (
	"context"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"testing"
	"time"
)

func installNoNetworkKiroRestClientForTest() func() {
	previousRestClient := kiroRestHttpStore.Load()
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	return func() {
		kiroRestHttpStore.Store(previousRestClient)
	}
}

func installNoNetworkBedrockHTTPClientForTest() func() {
	previousBedrockHTTPClientFor := bedrockHTTPClientFor
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("network disabled in test")
			}),
		}
	}
	return func() {
		bedrockHTTPClientFor = previousBedrockHTTPClientFor
	}
}

func cleanupHandlerBeforeRestore(t *testing.T, h *Handler, restore func()) {
	t.Helper()
	// Tests that deliberately exercise production handlers through this helper
	// own the dynamic work they start and always join it before restoring globals.
	if h != nil {
		h.backgroundStartMu.Lock()
		h.backgroundTaskOwner = true
		h.backgroundStartMu.Unlock()
	}
	// accountpool.GetPool is a process-wide singleton. config.Init swaps the
	// backing config image between tests, but it cannot reset that singleton.
	// Reload here, before the test uses h, so a Handler never inherits accounts
	// from the previous test's config image.
	if h != nil && h.pool != nil {
		h.pool.Reload()
	}
	t.Cleanup(func() {
		if h != nil {
			h.Close()
		}
		if restore != nil {
			restore()
		}
	})
}

func TestHandlerCloseWaitsForTrackedBackgroundTask(t *testing.T) {
	h := &Handler{backgroundTaskOwner: true}

	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	workerDone := make(chan struct{})

	if !h.startBackgroundTask(func() {
		close(workerStarted)
		<-releaseWorker
		close(workerDone)
	}) {
		t.Fatal("expected background task to start")
	}
	<-workerStarted

	closeDone := make(chan struct{})
	go func() {
		h.Close()
		close(closeDone)
	}()

	deadline := time.Now().Add(time.Second)
	for !h.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !h.closed.Load() {
		close(releaseWorker)
		<-closeDone
		t.Fatal("Handler.Close did not begin shutdown")
	}

	select {
	case <-closeDone:
		close(releaseWorker)
		t.Fatal("Handler.Close returned while a tracked background task was still blocked")
	default:
	}

	close(releaseWorker)

	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("background task did not finish after release")
	}

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Handler.Close did not return after the tracked background task finished")
	}

	if h.startBackgroundTask(func() {}) {
		t.Fatal("Handler accepted a new background task after Close")
	}
}

func TestManualHandlerRejectsUnownedBackgroundTask(t *testing.T) {
	h := &Handler{}
	ran := make(chan struct{}, 1)

	if h.startBackgroundTask(func() { ran <- struct{}{} }) {
		t.Fatal("manual Handler unexpectedly accepted lifecycle-owned background work")
	}
	select {
	case <-ran:
		t.Fatal("background function ran even though the manual Handler did not own it")
	default:
	}
}

func TestCleanupHandlerBeforeRestoreReloadsSingletonPool(t *testing.T) {
	staleConfig := t.TempDir() + "/stale-config.json"
	if err := config.Init(staleConfig); err != nil {
		t.Fatalf("config.Init(stale): %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:            "stale-bedrock",
		AuthMethod:    "bedrock",
		Enabled:       true,
		Region:        "us-east-1",
		BedrockAPIKey: "ABSK-stale",
	}); err != nil {
		t.Fatalf("config.AddAccount(stale): %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	if got := p.GetAllAccounts(); len(got) != 1 || got[0].ID != "stale-bedrock" {
		t.Fatalf("failed to prime singleton pool with stale account: %+v", got)
	}

	// A new config image does not itself reset accountpool.GetPool(). The test
	// isolation helper must reload that singleton before the Handler can launch
	// any model-discovery work against it.
	freshConfig := t.TempDir() + "/fresh-config.json"
	if err := config.Init(freshConfig); err != nil {
		t.Fatalf("config.Init(fresh): %v", err)
	}
	if got := config.GetAccounts(); len(got) != 0 {
		t.Fatalf("fresh config unexpectedly contains accounts: %+v", got)
	}

	h := &Handler{pool: p}
	cleanupHandlerBeforeRestore(t, h, nil)
	if got := h.pool.GetAllAccounts(); len(got) != 0 {
		t.Fatalf("Handler inherited stale accounts from the previous config image: %+v", got)
	}

	// cleanupHandlerBeforeRestore is the explicit opt-in for tests that need
	// production-style dynamic work. Keep that contract pinned as the inverse of
	// TestManualHandlerRejectsUnownedBackgroundTask.
	done := make(chan struct{})
	if !h.startBackgroundTask(func() { close(done) }) {
		t.Fatal("lifecycle-owned test Handler rejected background work")
	}
	<-done
}

func TestHandlerShutdownCancelsTrackedDynamicTaskContext(t *testing.T) {
	h := &Handler{backgroundTaskOwner: true}
	started := make(chan struct{})
	taskErr := make(chan error, 1)
	if !h.startBackgroundTaskContext(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		taskErr <- ctx.Err()
	}) {
		t.Fatal("expected context-aware background task to start")
	}
	<-started

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Handler.Shutdown: %v", err)
	}
	select {
	case err := <-taskErr:
		if err != context.Canceled {
			t.Fatalf("task context error = %v, want context.Canceled", err)
		}
	default:
		t.Fatal("shutdown returned before the canceled dynamic task exited")
	}
}
