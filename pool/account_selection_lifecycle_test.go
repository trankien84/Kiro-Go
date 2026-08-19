package pool

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
)

func newDispatchLifecycleTestPool(t *testing.T, accounts ...config.Account) *AccountPool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := config.AddAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	p := &AccountPool{
		cooldowns:            make(map[string]time.Time),
		lastDispatchSeq:      make(map[string]uint64),
		errorCounts:          make(map[string]int),
		modelLists:           make(map[string]map[string]bool),
		modelListUnavailable: make(map[string]bool),
		reprobeBackoff:       make(map[string]time.Duration),
		reprobeNext:          make(map[string]time.Time),
		circuitState:         make(map[string]*circuitBreaker),
		healthStats:          make(map[string]*accountHealth),
		apiKeyAffinity:       make(map[string]apiKeyBinding),
	}
	p.Reload()
	return p
}

func TestSelectionOwnsDeepAccountSnapshot(t *testing.T) {
	p := newDispatchLifecycleTestPool(t, config.Account{
		ID:              "snapshot",
		Enabled:         true,
		Tags:            []string{"one"},
		BedrockRegions:  []string{"us-east-1"},
		BedrockModelMap: map[string]string{"claude": "model-a"},
	})
	selection := p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil {
		t.Fatal("expected selection")
	}
	selection.Account.Tags[0] = "two"
	selection.Account.BedrockRegions[0] = "us-west-2"
	selection.Account.BedrockModelMap["claude"] = "model-b"

	current := p.GetByID("snapshot")
	if current == nil {
		t.Fatal("account disappeared")
	}
	if current.Tags[0] != "one" || current.BedrockRegions[0] != "us-east-1" || current.BedrockModelMap["claude"] != "model-a" {
		t.Fatalf("selection shared mutable backing storage with pool: %#v", current)
	}
}

func TestInflightStatsSurviveDisableReload(t *testing.T) {
	p := newDispatchLifecycleTestPool(t, config.Account{ID: "stats", Enabled: true})
	selection := p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil {
		t.Fatal("expected selection")
	}
	if err := config.SetAccountEnabled("stats", false); err != nil {
		t.Fatal(err)
	}
	p.Reload()
	if p.GetByID("stats") != nil {
		t.Fatal("disabled account remained routable")
	}
	if !p.UpdateStatsForSelection(*selection, 7, 1.5) {
		t.Fatal("in-flight completion stats were dropped after disable/reload")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.FlushStats(ctx); err != nil {
		t.Fatal(err)
	}
	account, ok := config.GetAccountByID("stats")
	if !ok {
		t.Fatal("disabled account disappeared from config inventory")
	}
	if account.RequestCount != 1 || account.TotalTokens != 7 || account.TotalCredits != 1.5 {
		t.Fatalf("unexpected persisted counters: requests=%d tokens=%d credits=%v", account.RequestCount, account.TotalTokens, account.TotalCredits)
	}
}

func TestOldSelectionCannotContaminateRecreatedAccount(t *testing.T) {
	p := newDispatchLifecycleTestPool(t, config.Account{
		ID:           "reuse",
		Enabled:      true,
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
	})
	selection := p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil {
		t.Fatal("expected selection")
	}
	if err := config.DeleteAccount("reuse"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "reuse",
		Enabled:      true,
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	p.Reload()

	if p.UpdateStatsForSelection(*selection, 9, 2) {
		t.Fatal("old stats lease was accepted after delete/recreate")
	}
	if p.RecordErrorForSelection(*selection, true) {
		t.Fatal("old runtime failure mutated recreated lifecycle")
	}
	if p.UpdateTokenForSelection(*selection, "stale-access", "stale-refresh", time.Now().Add(time.Hour).Unix()) {
		t.Fatal("old refresh result overwrote recreated lifecycle")
	}
	p.SetModelList("reuse", []string{"current-model"})
	if p.ClearModelListForLifecycle("reuse", selection.LifecycleGeneration) {
		t.Fatal("old lifecycle cleared recreated account model state")
	}
	if got := p.GetModelList("reuse"); len(got) != 1 || got[0] != "current-model" {
		t.Fatalf("recreated model state was contaminated: %#v", got)
	}

	account, ok := config.GetAccountByID("reuse")
	if !ok {
		t.Fatal("recreated account missing")
	}
	if account.RequestCount != 0 || account.TotalTokens != 0 || account.TotalCredits != 0 {
		t.Fatalf("recreated account inherited old usage: %#v", account)
	}
	if account.AccessToken != "new-access" || account.RefreshToken != "new-refresh" {
		t.Fatalf("recreated credentials overwritten: access=%q refresh=%q", account.AccessToken, account.RefreshToken)
	}
	p.mu.RLock()
	_, hasCooldown := p.cooldowns["reuse"]
	_, hasErrors := p.errorCounts["reuse"]
	p.mu.RUnlock()
	if hasCooldown || hasErrors {
		t.Fatal("recreated account inherited stale runtime health state")
	}
}

func TestExactAccountIDExclusionDoesNotCanonicalize(t *testing.T) {
	p := newDispatchLifecycleTestPool(t, config.Account{ID: " a ", Enabled: true})
	selection := p.GetNextSelectionForModelExcluding("", map[string]bool{"a": true})
	if selection == nil || selection.Account.ID != " a " {
		t.Fatalf("exact ID was collapsed by exclusion: %#v", selection)
	}
}

func TestSelectionStatsRejectedAdmissionDoesNotAdvanceLedger(t *testing.T) {
	p := newDispatchLifecycleTestPool(t, config.Account{ID: "closing-stats", Enabled: true})
	selection := p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil {
		t.Fatal("expected selection")
	}

	p.statsPersistMu.Lock()
	p.initStatsPersistenceLocked()
	p.statsPersistClosing = true
	p.statsPersistMu.Unlock()

	if p.UpdateStatsForSelection(*selection, 5, 1) {
		t.Fatal("stats update was accepted after persistence admission closed")
	}
	p.mu.RLock()
	ledger := p.statsLedgerByID[selection.Account.ID]
	_, retained := p.statsCurrentByID[selection.Account.ID]
	p.mu.RUnlock()
	if ledger.requestCount != 0 || ledger.totalTokens != 0 || ledger.totalCredits != 0 || retained {
		t.Fatalf("rejected stats update advanced cumulative state: ledger=%#v retained=%t", ledger, retained)
	}
}
