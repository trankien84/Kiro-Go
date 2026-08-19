package config

import (
	"os"
	"path/filepath"
	"testing"
)

func cloneGenerationMap(source map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func clonePresenceMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneAppliedVersionMap(source map[accountStatsVersionKey]uint64) map[accountStatsVersionKey]uint64 {
	result := make(map[accountStatsVersionKey]uint64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func resetAccountStatsGenerationTestState(t *testing.T, accounts []Account) {
	t.Helper()

	cfgLock.Lock()
	previousPath := cfgPath
	previousConfig := cfg
	previousGenerations := cloneGenerationMap(accountStatsGenerationByID)
	previousPresence := clonePresenceMap(accountStatsPresentByID)
	previousVersions := cloneAppliedVersionMap(accountStatsAppliedVersion)

	cfgPath = filepath.Join(t.TempDir(), "config.json")
	cfg = &Config{Accounts: append([]Account(nil), accounts...)}
	accountStatsGenerationByID = make(map[string]uint64)
	accountStatsPresentByID = make(map[string]bool)
	accountStatsAppliedVersion = make(map[accountStatsVersionKey]uint64)
	reconcileAccountStatsGenerationsLocked(nil, cfg.Accounts)
	if err := saveLocked(); err != nil {
		cfgLock.Unlock()
		t.Fatalf("save fixture: %v", err)
	}
	cfgLock.Unlock()

	t.Cleanup(func() {
		cfgLock.Lock()
		cfgPath = previousPath
		cfg = previousConfig
		accountStatsGenerationByID = previousGenerations
		accountStatsPresentByID = previousPresence
		accountStatsAppliedVersion = previousVersions
		cfgLock.Unlock()
	})
}

func generationForTestAccount(t *testing.T, id string) uint64 {
	t.Helper()
	for _, item := range GetAccountsWithStatsGeneration() {
		if item.Account.ID == id {
			return item.StatsGeneration
		}
	}
	t.Fatalf("account %q not found", id)
	return 0
}

func statsWrite(id string, generation, version uint64, requests int) AccountStatsWrite {
	return AccountStatsWrite{
		AccountID:  id,
		Generation: generation,
		Version:    version,
		Counters: AccountStatsCounters{
			RequestCount: requests,
			TotalTokens:  requests * 10,
			TotalCredits: float64(requests),
			LastUsed:     int64(requests),
		},
	}
}

func TestAccountStatsOldGenerationCannotOverwriteRecreatedAccount(t *testing.T) {
	resetAccountStatsGenerationTestState(t, []Account{{
		ID:           "a",
		Enabled:      true,
		RequestCount: 10,
		TotalTokens:  100,
		LastUsed:     10,
	}})
	oldGeneration := generationForTestAccount(t, "a")

	if err := DeleteAccount("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	newGeneration := generationForTestAccount(t, "a")
	if newGeneration <= oldGeneration {
		t.Fatalf("generation did not advance: old=%d new=%d", oldGeneration, newGeneration)
	}

	results, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", oldGeneration, 1, 99),
	})
	if err != nil {
		t.Fatalf("stale write: %v", err)
	}
	if len(results) != 1 || !results[0].Superseded {
		t.Fatalf("stale result=%#v", results)
	}
	account, ok := GetAccountByID("a")
	if !ok || account.RequestCount != 0 || account.TotalTokens != 0 {
		t.Fatalf("stale stats reached recreated account: %#v", account)
	}

	if _, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", newGeneration, 1, 2),
	}); err != nil {
		t.Fatalf("current write: %v", err)
	}
	account, _ = GetAccountByID("a")
	if account.RequestCount != 2 || account.TotalTokens != 20 {
		t.Fatalf("current stats not persisted: %#v", account)
	}
}

func TestAccountStatsDeleteResolvesOldWriteAsSuperseded(t *testing.T) {
	resetAccountStatsGenerationTestState(t, []Account{{ID: "a", Enabled: true}})
	generation := generationForTestAccount(t, "a")
	if err := DeleteAccount("a"); err != nil {
		t.Fatal(err)
	}
	results, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", generation, 1, 1),
	})
	if err != nil {
		t.Fatalf("deleted-account stale write: %v", err)
	}
	if len(results) != 1 || !results[0].Superseded {
		t.Fatalf("result=%#v", results)
	}
}

func TestAccountStatsResetAdvancesGeneration(t *testing.T) {
	original := Account{ID: "a", Enabled: true, RequestCount: 8, TotalTokens: 80, LastUsed: 8}
	resetAccountStatsGenerationTestState(t, []Account{original})
	oldGeneration := generationForTestAccount(t, "a")
	reset := original
	reset.RequestCount = 0
	reset.TotalTokens = 0
	reset.LastUsed = 0
	if err := UpdateAccount("a", reset); err != nil {
		t.Fatalf("reset: %v", err)
	}
	newGeneration := generationForTestAccount(t, "a")
	if newGeneration != oldGeneration+1 {
		t.Fatalf("generation old=%d new=%d", oldGeneration, newGeneration)
	}

	results, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", oldGeneration, 1, 20),
	})
	if err != nil {
		t.Fatalf("stale pre-reset write: %v", err)
	}
	if !results[0].Superseded {
		t.Fatalf("pre-reset write was not superseded: %#v", results[0])
	}
	account, _ := GetAccountByID("a")
	if account.RequestCount != 0 || account.TotalTokens != 0 {
		t.Fatalf("old counters resurrected: %#v", account)
	}
}

func TestAccountStatsSaveFailureDoesNotMutateInMemoryConfig(t *testing.T) {
	resetAccountStatsGenerationTestState(t, []Account{{ID: "a", Enabled: true}})
	generation := generationForTestAccount(t, "a")

	cfgLock.Lock()
	cfgPath = t.TempDir() // os.WriteFile on a directory must fail.
	cfgLock.Unlock()
	if _, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", generation, 1, 7),
	}); err == nil {
		t.Fatal("expected write failure")
	}
	account, _ := GetAccountByID("a")
	if account.RequestCount != 0 || account.TotalTokens != 0 {
		t.Fatalf("failed save mutated in-memory config: %#v", account)
	}

	cfgLock.Lock()
	cfgPath = filepath.Join(t.TempDir(), "retry.json")
	cfgLock.Unlock()
	if _, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", generation, 1, 7),
	}); err != nil {
		t.Fatalf("retry same version after save failure: %v", err)
	}
	account, _ = GetAccountByID("a")
	if account.RequestCount != 7 {
		t.Fatalf("failed save advanced committed version: %#v", account)
	}
}

func TestAccountStatsBatchWritesConfigOnceAfterValidation(t *testing.T) {
	resetAccountStatsGenerationTestState(t, []Account{{ID: "a"}, {ID: "b"}})
	inventory := GetAccountsWithStatsGeneration()
	generations := map[string]uint64{}
	for _, item := range inventory {
		generations[item.Account.ID] = item.StatsGeneration
	}
	if _, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", generations["a"], 1, 2),
		statsWrite("b", generations["b"], 1, 3),
	}); err != nil {
		t.Fatalf("batch write: %v", err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("config file is empty")
	}
	a, _ := GetAccountByID("a")
	b, _ := GetAccountByID("b")
	if a.RequestCount != 2 || b.RequestCount != 3 {
		t.Fatalf("a=%#v b=%#v", a, b)
	}
}

func TestAccountStatsOlderVersionCannotOverwriteCommittedVersion(t *testing.T) {
	resetAccountStatsGenerationTestState(t, []Account{{ID: "a", Enabled: true}})
	generation := generationForTestAccount(t, "a")
	if _, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", generation, 2, 5),
	}); err != nil {
		t.Fatalf("version 2: %v", err)
	}
	results, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", generation, 1, 99),
	})
	if err != nil {
		t.Fatalf("older version: %v", err)
	}
	if len(results) != 1 || !results[0].Superseded {
		t.Fatalf("result=%#v", results)
	}
	account, _ := GetAccountByID("a")
	if account.RequestCount != 5 {
		t.Fatalf("older version overwrote counters: %#v", account)
	}
	inventory := GetAccountsWithStatsGeneration()
	if len(inventory) != 1 || inventory[0].StatsVersion != 2 {
		t.Fatalf("durable version not captured with inventory: %#v", inventory)
	}
}

func TestAccountStatsIdentityKeepsNonBlankIDsExact(t *testing.T) {
	resetAccountStatsGenerationTestState(t, []Account{{ID: "a"}, {ID: " a "}})
	inventory := GetAccountsWithStatsGeneration()
	if len(inventory) != 2 || inventory[0].StatsGeneration == 0 || inventory[1].StatsGeneration == 0 {
		t.Fatalf("inventory=%#v", inventory)
	}
	if len(accountStatsGenerationByID) != 2 {
		t.Fatalf("account IDs were collapsed: %#v", accountStatsGenerationByID)
	}
}

func TestAccountStatsInvalidLaterWriteDoesNotPartiallyPublishEarlierWrite(t *testing.T) {
	resetAccountStatsGenerationTestState(t, []Account{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}})
	inventory := GetAccountsWithStatsGeneration()
	generationA := inventory[0].StatsGeneration

	_, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{
		statsWrite("a", generationA, 1, 5),
		{AccountID: "b", Generation: 0, Version: 1},
	})
	if err == nil {
		t.Fatal("expected invalid batch to fail")
	}
	a, _ := GetAccountByID("a")
	if a.RequestCount != 0 {
		t.Fatalf("invalid batch partially published first write: %#v", a)
	}
}
