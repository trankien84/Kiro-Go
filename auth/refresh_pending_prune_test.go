package auth

import (
	"testing"
	"time"
)

func TestRefreshPruneNeverWaitsOnBusyGate(t *testing.T) {
	key := refreshLockKey{id: "prune-busy-refresh", generation: 10337}
	entry := newRefreshLockEntry()
	entry.pending = &pendingCredentialPublication{}
	<-entry.gate // Simulate a long-running committed refresh.
	refreshMu.Lock()
	if _, exists := refreshLocks[key]; exists {
		refreshMu.Unlock()
		t.Fatal("test key collided with existing registry entry")
	}
	refreshLocks[key] = entry
	refreshMu.Unlock()
	t.Cleanup(func() {
		refreshMu.Lock()
		delete(refreshLocks, key)
		refreshMu.Unlock()
	})
	finished := make(chan struct{})
	go func() { pruneStaleRefreshEntries(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		// Ensure cleanup cannot deadlock even if the regression resurfaces.
		entry.gate <- struct{}{}
		t.Fatal("pruning blocked on an occupied gate")
	}
	if entry.pending == nil {
		t.Fatal("an active C2 was discarded during pruning")
	}
	entry.gate <- struct{}{}
}
