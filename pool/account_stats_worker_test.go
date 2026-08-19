package pool

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
)

func TestStatsPersistenceWorkerCoalescesAndWaits(t *testing.T) {
	p := &AccountPool{accounts: []config.Account{{ID: "a", Enabled: true}}}
	var mu sync.Mutex
	var last accountStatsSnapshot
	p.statsPersistFn = func(snapshot accountStatsSnapshot) error {
		mu.Lock()
		last = snapshot
		mu.Unlock()
		return nil
	}

	const updates = 100
	var writers sync.WaitGroup
	writers.Add(updates)
	for i := 0; i < updates; i++ {
		go func() {
			defer writers.Done()
			p.UpdateStats("a", 1, 0.5)
		}()
	}
	writers.Wait()
	p.WaitForPendingStats()

	mu.Lock()
	got := last
	mu.Unlock()
	if got.requestCount != updates || got.totalTokens != updates || got.totalCredits != updates*0.5 {
		t.Fatalf("persisted snapshot=%#v", got)
	}
	p.CloseStatsPersistence()
}

func TestStatsPersistenceCloseDrainsAndRejectsNewAdmission(t *testing.T) {
	p := &AccountPool{accounts: []config.Account{{ID: "a", Enabled: true}}}
	var mu sync.Mutex
	persisted := 0
	p.statsPersistFn = func(snapshot accountStatsSnapshot) error {
		mu.Lock()
		persisted = snapshot.requestCount
		mu.Unlock()
		return nil
	}
	p.UpdateStats("a", 1, 1)
	p.CloseStatsPersistence()
	mu.Lock()
	before := persisted
	mu.Unlock()
	if before != 1 {
		t.Fatalf("close returned before drain: persisted=%d", before)
	}
	p.UpdateStats("a", 1, 1)
	p.WaitForPendingStats()
	mu.Lock()
	after := persisted
	mu.Unlock()
	if after != before {
		t.Fatalf("post-close update was admitted: before=%d after=%d", before, after)
	}
}

func TestModelListDistinguishesColdStartFromKnownEmpty(t *testing.T) {
	p := &AccountPool{modelLists: make(map[string]map[string]bool)}
	if !p.accountHasModel("a", "model-x") {
		t.Fatal("absent cold-start model list should remain optimistic")
	}
	p.SetModelList("a", nil)
	p.mu.RLock()
	emptyDiscoveryAllows := p.accountHasModel("a", "model-x")
	p.mu.RUnlock()
	if !emptyDiscoveryAllows {
		t.Fatal("non-enumerable empty model list should remain optimistic")
	}
	p.SetModelListUnavailable("a")
	p.mu.RLock()
	unavailableAllows := p.accountHasModel("a", "model-x")
	p.mu.RUnlock()
	if unavailableAllows {
		t.Fatal("expired/unavailable model list was treated as cold start")
	}
	p.ClearModelList("a")
	p.mu.RLock()
	coldStartAllows := p.accountHasModel("a", "model-x")
	p.mu.RUnlock()
	if !coldStartAllows {
		t.Fatal("cleared model list did not restore cold-start semantics")
	}
}

func TestStatsPersistenceRetriesBeforeCommit(t *testing.T) {
	p := &AccountPool{accounts: []config.Account{{ID: "a", Enabled: true}}}
	var mu sync.Mutex
	attempts := 0
	p.statsPersistFn = func(snapshot accountStatsSnapshot) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return errors.New("temporary storage failure")
		}
		return nil
	}

	p.UpdateStats("a", 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.FlushStats(ctx); err != nil {
		t.Fatalf("flush after transient failure: %v", err)
	}
	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()
	if gotAttempts != 3 {
		t.Fatalf("attempts=%d want=3", gotAttempts)
	}
	_ = p.Close(context.Background())
}

func TestStatsPersistencePermanentFailureDoesNotAdvanceCommit(t *testing.T) {
	p := &AccountPool{accounts: []config.Account{{ID: "a", Enabled: true}}}
	p.statsPersistFn = func(snapshot accountStatsSnapshot) error {
		return errors.New("permanent storage failure")
	}

	p.UpdateStats("a", 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := p.FlushStats(ctx); err == nil || !strings.Contains(err.Error(), "permanent storage failure") {
		t.Fatalf("expected durable flush error, got %v", err)
	}
	p.statsPersistMu.Lock()
	committed := p.statsPersistCommittedSeq
	attempted := p.statsPersistAttemptedSeq
	p.statsPersistMu.Unlock()
	if committed != 0 || attempted == 0 {
		t.Fatalf("watermarks committed=%d attempted=%d", committed, attempted)
	}
	_ = p.Close(context.Background())
}

func TestStatsFlushWaitsForNewerSnapshotAfterOlderCommit(t *testing.T) {
	p := &AccountPool{accounts: []config.Account{{ID: "a", Enabled: true}}}
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	p.statsPersistFn = func(snapshot accountStatsSnapshot) error {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		switch call {
		case 1:
			close(firstStarted)
			<-releaseFirst
		case 2:
			close(secondStarted)
			<-releaseSecond
		}
		return nil
	}

	p.UpdateStats("a", 1, 1)
	<-firstStarted
	p.UpdateStats("a", 1, 1)

	flushDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		flushDone <- p.FlushStats(ctx)
	}()

	close(releaseFirst)
	<-secondStarted
	select {
	case err := <-flushDone:
		t.Fatalf("flush completed from older snapshot commit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseSecond)
	if err := <-flushDone; err != nil {
		t.Fatalf("flush after newer snapshot: %v", err)
	}
	_ = p.Close(context.Background())
}

func TestStatsReloadDoesNotRegressAcceptedSnapshotBeforePersistenceCommit(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "reload-race", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	p := &AccountPool{}
	p.Reload()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p.statsPersistFn = func(accountStatsSnapshot) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	}

	p.UpdateStats("reload-race", 11, 1.25)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("persistence attempt did not start")
	}

	// Config still contains the old durable counters here. Reload must merge the
	// accepted snapshot instead of replacing the pool with those stale values.
	p.Reload()
	p.mu.RLock()
	accountCount := len(p.accounts)
	if accountCount != 1 {
		p.mu.RUnlock()
		close(release)
		t.Fatalf("accounts=%d want=1", accountCount)
	}
	account := p.accounts[0]
	p.mu.RUnlock()
	if account.RequestCount != 1 || account.TotalTokens != 11 || account.TotalCredits != 1.25 {
		close(release)
		t.Fatalf("reload regressed accepted counters: %#v", account)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.FlushStats(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	_ = p.Close(context.Background())
}

func TestStatsEpochsFollowConfigOwnedGeneration(t *testing.T) {
	p := &AccountPool{}
	p.mu.Lock()
	first := p.syncStatsEpochsLocked([]config.AccountWithStatsGeneration{{
		Account: config.Account{ID: "a"}, StatsGeneration: 7,
	}})
	p.statsVersionByID["a"] = 3
	second := p.syncStatsEpochsLocked([]config.AccountWithStatsGeneration{{
		Account: config.Account{ID: "a"}, StatsGeneration: 8,
	}})
	p.mu.Unlock()
	if first["a"] != 7 || second["a"] != 8 {
		t.Fatalf("first=%v second=%v", first, second)
	}
	if p.statsVersionByID["a"] != 0 {
		t.Fatalf("version was not reset: %d", p.statsVersionByID["a"])
	}
}

func TestStatsReloadMergeKeepsAcceptedCountersUntilDurableVersionCatchesUp(t *testing.T) {
	p := &AccountPool{}
	p.mu.Lock()
	p.syncStatsEpochsLocked([]config.AccountWithStatsGeneration{{
		Account: config.Account{ID: "a", Enabled: true}, StatsGeneration: 4, StatsVersion: 1,
	}})
	p.statsVersionByID["a"] = 3
	p.statsCurrentByID["a"] = accountStatsSnapshot{
		key: accountStatsKey{id: "a", epoch: 4}, version: 3,
		requestCount: 9, totalTokens: 90, totalCredits: 4.5, lastUsed: 9,
	}
	merged := p.mergeCurrentStatsLocked(config.AccountWithStatsGeneration{
		Account:         config.Account{ID: "a", Enabled: true, RequestCount: 2, TotalTokens: 20, LastUsed: 2},
		StatsGeneration: 4,
		StatsVersion:    1,
	})
	p.mu.Unlock()

	if merged.RequestCount != 9 || merged.TotalTokens != 90 || merged.LastUsed != 9 {
		t.Fatalf("pending accepted counters were rolled back: %#v", merged)
	}
}

func TestStatsDurableCatchUpPrunesRetainedSnapshot(t *testing.T) {
	p := &AccountPool{}
	p.mu.Lock()
	p.statsCurrentByID = map[string]accountStatsSnapshot{
		"a": {
			key: accountStatsKey{id: "a", epoch: 4}, version: 3,
			requestCount: 9,
		},
	}
	p.statsEpochByID = map[string]uint64{"a": 4}
	p.statsVersionByID = map[string]uint64{"a": 3}
	p.syncStatsEpochsLocked([]config.AccountWithStatsGeneration{{
		Account: config.Account{ID: "a", RequestCount: 9}, StatsGeneration: 4, StatsVersion: 3,
	}})
	_, retained := p.statsCurrentByID["a"]
	p.mu.Unlock()
	if retained {
		t.Fatal("durable catch-up did not prune retained snapshot")
	}
}

func TestStatsGenerationChangeDropsOldRetainedSnapshot(t *testing.T) {
	p := &AccountPool{}
	p.mu.Lock()
	p.statsEpochByID = map[string]uint64{"a": 4}
	p.statsVersionByID = map[string]uint64{"a": 3}
	p.statsCurrentByID = map[string]accountStatsSnapshot{
		"a": {key: accountStatsKey{id: "a", epoch: 4}, version: 3, requestCount: 9},
	}
	p.syncStatsEpochsLocked([]config.AccountWithStatsGeneration{{
		Account: config.Account{ID: "a"}, StatsGeneration: 5, StatsVersion: 0,
	}})
	_, retained := p.statsCurrentByID["a"]
	version := p.statsVersionByID["a"]
	p.mu.Unlock()
	if retained || version != 0 {
		t.Fatalf("old lifecycle survived generation change: retained=%t version=%d", retained, version)
	}
}

func TestStatsPoolKeepsExactNonBlankAccountIDsDistinct(t *testing.T) {
	p := &AccountPool{}
	inventory := []config.AccountWithStatsGeneration{
		{Account: config.Account{ID: "a"}, StatsGeneration: 3},
		{Account: config.Account{ID: " a "}, StatsGeneration: 7},
	}

	p.mu.Lock()
	active := p.syncStatsEpochsLocked(inventory)
	p.statsCurrentByID[" a "] = accountStatsSnapshot{
		key: accountStatsKey{id: " a ", epoch: 7}, version: 1, requestCount: 11,
	}
	spaced := p.mergeCurrentStatsLocked(config.AccountWithStatsGeneration{
		Account: config.Account{ID: " a ", RequestCount: 1}, StatsGeneration: 7,
	})
	plain := p.mergeCurrentStatsLocked(config.AccountWithStatsGeneration{
		Account: config.Account{ID: "a", RequestCount: 2}, StatsGeneration: 3,
	})
	p.mu.Unlock()

	if len(active) != 2 || active["a"] != 3 || active[" a "] != 7 {
		t.Fatalf("exact IDs collapsed in active generations: %#v", active)
	}
	if spaced.RequestCount != 11 || plain.RequestCount != 2 {
		t.Fatalf("stats crossed exact IDs: spaced=%#v plain=%#v", spaced, plain)
	}
}

func TestStatsRejectedPersistenceAdmissionIsNotRetainedAsCurrent(t *testing.T) {
	p := &AccountPool{
		accounts:         []config.Account{{ID: "a", Enabled: true}},
		statsEpochByID:   map[string]uint64{"a": 1},
		statsVersionByID: map[string]uint64{"a": 0},
	}
	p.statsPersistMu.Lock()
	p.initStatsPersistenceLocked()
	p.statsPersistClosing = true
	p.statsPersistMu.Unlock()

	p.UpdateStats("a", 1, 1)
	p.mu.RLock()
	_, retained := p.statsCurrentByID["a"]
	p.mu.RUnlock()
	if retained {
		t.Fatal("snapshot rejected by persistence admission was retained as current")
	}
}

func TestStatsAcceptanceAndRetentionAreAtomicWithReloadLock(t *testing.T) {
	p := &AccountPool{
		accounts:         []config.Account{{ID: "a", Enabled: true}},
		statsEpochByID:   map[string]uint64{"a": 1},
		statsVersionByID: map[string]uint64{"a": 0},
	}
	p.statsPersistFn = func(accountStatsSnapshot) error { return nil }

	// Block persistence admission. UpdateStats must keep p.mu while waiting so a
	// concurrent Reload cannot replace the account with stale durable counters in
	// the gap between the in-memory update and accepted-snapshot retention.
	p.statsPersistMu.Lock()
	updateDone := make(chan struct{})
	go func() {
		p.UpdateStats("a", 1, 1)
		close(updateDone)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !p.mu.TryLock() {
			break
		}
		p.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if p.mu.TryLock() {
		p.mu.Unlock()
		p.statsPersistMu.Unlock()
		t.Fatal("UpdateStats released p.mu before persistence admission completed")
	}
	p.statsPersistMu.Unlock()

	select {
	case <-updateDone:
	case <-time.After(time.Second):
		t.Fatal("UpdateStats did not complete after admission unblocked")
	}
	p.mu.RLock()
	retained, ok := p.statsCurrentByID["a"]
	p.mu.RUnlock()
	if !ok || retained.requestCount != 1 || retained.version != 1 {
		t.Fatalf("accepted snapshot not retained atomically: %#v ok=%t", retained, ok)
	}
	_ = p.Close(context.Background())
}
