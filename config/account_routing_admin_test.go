package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func seedRoutingAdminAccounts(t *testing.T, accounts []Account) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	published, err := publishConfigAccountsLocked(func(next *Config) error {
		next.Accounts = make([]Account, len(accounts))
		for i := range accounts {
			next.Accounts[i] = accounts[i].Clone()
		}
		return nil
	})
	if err != nil || !published {
		t.Fatalf("seed accounts published=%v err=%v", published, err)
	}
}

func routingObservations(accounts []Account) []AccountRoutingObservation {
	out := make([]AccountRoutingObservation, len(accounts))
	for i, account := range accounts {
		out[i] = AccountRoutingObservation{
			ID:         account.ID,
			Generation: account.DispatchGeneration,
			Priority:   account.Priority,
		}
	}
	return out
}

func useFillFirstForRoutingAdminTest(t *testing.T) {
	t.Helper()
	if err := UpdateAccountSelectionStrategy(AccountSelectionStrategyFillFirst); err != nil {
		t.Fatal(err)
	}
}

func injectRoutingDurabilityFailure(t *testing.T) error {
	t.Helper()
	original := syncConfigDirectoryForWrite
	durabilityErr := errors.New("injected routing directory sync failure")
	syncConfigDirectoryForWrite = func(string) error { return durabilityErr }
	t.Cleanup(func() { syncConfigDirectoryForWrite = original })
	return durabilityErr
}

func TestUpdateAccountSelectionStrategyObservedPublication(t *testing.T) {
	seedRoutingAdminAccounts(t, nil)
	published, superseded, err := UpdateAccountSelectionStrategyIfObserved(AccountSelectionStrategyRoundRobin, AccountSelectionStrategyPriority)
	if err != nil || !published || superseded {
		t.Fatalf("published=%v superseded=%v err=%v", published, superseded, err)
	}
	if got := GetAccountSelectionStrategy(); got != AccountSelectionStrategyPriority {
		t.Fatalf("strategy=%q", got)
	}

	published, superseded, err = UpdateAccountSelectionStrategyIfObserved(AccountSelectionStrategyPriority, AccountSelectionStrategyPriority)
	if err != nil || published || superseded {
		t.Fatalf("no-op strategy write published=%v superseded=%v err=%v", published, superseded, err)
	}

	published, superseded, err = UpdateAccountSelectionStrategyIfObserved(AccountSelectionStrategyRoundRobin, AccountSelectionStrategyFillFirst)
	if err != nil || published || !superseded {
		t.Fatalf("stale strategy write published=%v superseded=%v err=%v", published, superseded, err)
	}
	if got := GetAccountSelectionStrategy(); got != AccountSelectionStrategyPriority {
		t.Fatalf("stale strategy write changed canonical strategy=%q", got)
	}
}

func TestUpdateAccountSelectionStrategyPublishedDurabilityErrorKeepsCanonical(t *testing.T) {
	seedRoutingAdminAccounts(t, nil)
	durabilityErr := injectRoutingDurabilityFailure(t)
	published, superseded, err := UpdateAccountSelectionStrategyIfObserved(AccountSelectionStrategyRoundRobin, AccountSelectionStrategyPriority)
	if !published || superseded || !errors.Is(err, durabilityErr) {
		t.Fatalf("published=%v superseded=%v err=%v, want published durability error", published, superseded, err)
	}
	if got := GetAccountSelectionStrategy(); got != AccountSelectionStrategyPriority {
		t.Fatalf("published strategy was rolled back in memory: %q", got)
	}
}

func TestUpdateAccountPriorityIfObservedPreservesCanonicalStateAndFencesSameLifecycleEdit(t *testing.T) {
	seedRoutingAdminAccounts(t, []Account{{ID: "a", AccessToken: "a1", RefreshToken: "ra", Priority: 10, RequestCount: 3}})
	account := GetAccounts()[0]
	after, published, superseded, err := UpdateAccountPriorityIfObserved(account.ID, account.DispatchGeneration, 10, 20)
	if err != nil || !published || superseded {
		t.Fatalf("published=%v superseded=%v err=%v", published, superseded, err)
	}
	if after.Priority != 20 || after.AccessToken != "a1" || after.RefreshToken != "ra" || after.RequestCount != 3 {
		t.Fatalf("priority update corrupted canonical state: %#v", after)
	}
	if after.DispatchGeneration != account.DispatchGeneration {
		t.Fatalf("priority update changed lifecycle generation: before=%d after=%d", account.DispatchGeneration, after.DispatchGeneration)
	}

	_, published, superseded, err = UpdateAccountPriorityIfObserved(account.ID, account.DispatchGeneration, 10, 30)
	if err != nil || published || !superseded {
		t.Fatalf("same-lifecycle stale edit published=%v superseded=%v err=%v", published, superseded, err)
	}
	if got := GetAccounts()[0].Priority; got != 20 {
		t.Fatalf("stale priority edit changed canonical priority=%d", got)
	}
}

func TestUpdateAccountPriorityIfObservedRepairsLegacyNegativePriority(t *testing.T) {
	seedRoutingAdminAccounts(t, []Account{{ID: "a", Priority: -7}})
	account := GetAccounts()[0]
	after, published, superseded, err := UpdateAccountPriorityIfObserved(account.ID, account.DispatchGeneration, -7, 0)
	if err != nil || !published || superseded {
		t.Fatalf("published=%v superseded=%v err=%v", published, superseded, err)
	}
	if after.Priority != 0 || EffectiveAccountPriority(after.Priority) != DefaultAccountPriority {
		t.Fatalf("legacy priority was not normalized by explicit Admin edit: %#v", after)
	}
}

func TestUpdateAccountPriorityIfObservedDistinguishesPublicationFailures(t *testing.T) {
	t.Run("pre-publication", func(t *testing.T) {
		seedRoutingAdminAccounts(t, []Account{{ID: "a", Priority: 10}})
		account := GetAccounts()[0]
		original := replaceConfigFileForWrite
		persistErr := errors.New("injected routing replace failure")
		replaceConfigFileForWrite = func(string, string) (bool, error) { return false, persistErr }
		t.Cleanup(func() { replaceConfigFileForWrite = original })

		_, published, superseded, err := UpdateAccountPriorityIfObserved(account.ID, account.DispatchGeneration, 10, 20)
		if published || superseded || !errors.Is(err, persistErr) {
			t.Fatalf("published=%v superseded=%v err=%v, want pre-publication failure", published, superseded, err)
		}
		if got := GetAccounts()[0].Priority; got != 10 {
			t.Fatalf("pre-publication failure changed canonical priority=%d", got)
		}
	})

	t.Run("post-publication-durability", func(t *testing.T) {
		seedRoutingAdminAccounts(t, []Account{{ID: "a", Priority: 10}})
		account := GetAccounts()[0]
		durabilityErr := injectRoutingDurabilityFailure(t)
		after, published, superseded, err := UpdateAccountPriorityIfObserved(account.ID, account.DispatchGeneration, 10, 20)
		if !published || superseded || !errors.Is(err, durabilityErr) {
			t.Fatalf("published=%v superseded=%v err=%v, want post-publication durability error", published, superseded, err)
		}
		if after.Priority != 20 || GetAccounts()[0].Priority != 20 {
			t.Fatalf("published priority was not adopted: after=%#v canonical=%#v", after, GetAccounts()[0])
		}
	})
}

func TestReorderAccountsIfObservedPreservesCanonicalStateAndGenerations(t *testing.T) {
	seedRoutingAdminAccounts(t, []Account{
		{ID: "a", AccessToken: "a1", RefreshToken: "ra", Priority: 10, RequestCount: 3},
		{ID: "b", AccessToken: "b1", RefreshToken: "rb", Priority: 10, RequestCount: 7},
		{ID: "c", AccessToken: "c1", RefreshToken: "rc", Priority: 20, RequestCount: 11},
	})
	useFillFirstForRoutingAdminTest(t)
	before := GetAccountsWithStatsGeneration()
	accounts := GetAccounts()
	observed := routingObservations(accounts)
	published, superseded, err := ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, append([]AccountRoutingObservation(nil), observed...))
	if err != nil || published || superseded {
		t.Fatalf("no-op reorder published=%v superseded=%v err=%v", published, superseded, err)
	}
	desired := []AccountRoutingObservation{observed[1], observed[0], observed[2]}
	published, superseded, err = ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, desired)
	if err != nil || !published || superseded {
		t.Fatalf("published=%v superseded=%v err=%v", published, superseded, err)
	}
	afterAccounts := GetAccounts()
	if got := []string{afterAccounts[0].ID, afterAccounts[1].ID, afterAccounts[2].ID}; got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Fatalf("unexpected order: %v", got)
	}
	if afterAccounts[0].AccessToken != "b1" || afterAccounts[0].RefreshToken != "rb" || afterAccounts[0].RequestCount != 7 {
		t.Fatalf("reorder did not preserve canonical b state: %#v", afterAccounts[0])
	}
	after := GetAccountsWithStatsGeneration()
	beforeByID := make(map[string]AccountWithStatsGeneration, len(before))
	for _, item := range before {
		beforeByID[item.Account.ID] = item
	}
	for _, item := range after {
		previous := beforeByID[item.Account.ID]
		if item.LifecycleGeneration != previous.LifecycleGeneration || item.StatsGeneration != previous.StatsGeneration {
			t.Fatalf("routing reorder changed generation for %s: before=%#v after=%#v", item.Account.ID, previous, item)
		}
	}
}

func TestReorderAccountsIfObservedFencesStrategyPriorityOrderAndLifecycle(t *testing.T) {
	t.Run("stale-order", func(t *testing.T) {
		seedRoutingAdminAccounts(t, []Account{{ID: "a", Priority: 10}, {ID: "b", Priority: 10}, {ID: "c", Priority: 20}})
		useFillFirstForRoutingAdminTest(t)
		accounts := GetAccounts()
		observed := routingObservations(accounts)
		firstDesired := []AccountRoutingObservation{observed[1], observed[0], observed[2]}
		published, superseded, err := ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, firstDesired)
		if err != nil || !published || superseded {
			t.Fatalf("first reorder published=%v superseded=%v err=%v", published, superseded, err)
		}

		secondDesired := []AccountRoutingObservation{observed[2], observed[1], observed[0]}
		published, superseded, err = ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, secondDesired)
		if err != nil || published || !superseded {
			t.Fatalf("stale order published=%v superseded=%v err=%v", published, superseded, err)
		}
	})

	t.Run("stale-priority", func(t *testing.T) {
		seedRoutingAdminAccounts(t, []Account{{ID: "a", Priority: 10}, {ID: "b", Priority: 10}})
		useFillFirstForRoutingAdminTest(t)
		observed := routingObservations(GetAccounts())
		account := GetAccounts()[1]
		if _, published, superseded, err := UpdateAccountPriorityIfObserved(account.ID, account.DispatchGeneration, 10, 20); err != nil || !published || superseded {
			t.Fatalf("priority change published=%v superseded=%v err=%v", published, superseded, err)
		}
		desired := []AccountRoutingObservation{observed[1], observed[0]}
		published, superseded, err := ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, desired)
		if err != nil || published || !superseded {
			t.Fatalf("stale-priority reorder published=%v superseded=%v err=%v", published, superseded, err)
		}
	})

	t.Run("stale-strategy", func(t *testing.T) {
		seedRoutingAdminAccounts(t, []Account{{ID: "a", Priority: 10}, {ID: "b", Priority: 10}})
		useFillFirstForRoutingAdminTest(t)
		observed := routingObservations(GetAccounts())
		if err := UpdateAccountSelectionStrategy(AccountSelectionStrategyPriority); err != nil {
			t.Fatal(err)
		}
		published, superseded, err := ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, []AccountRoutingObservation{observed[1], observed[0]})
		if err != nil || published || !superseded {
			t.Fatalf("stale-strategy reorder published=%v superseded=%v err=%v", published, superseded, err)
		}
	})

	t.Run("partial-and-lifecycle", func(t *testing.T) {
		seedRoutingAdminAccounts(t, []Account{{ID: "a"}, {ID: "b"}, {ID: "c"}})
		useFillFirstForRoutingAdminTest(t)
		current := GetAccounts()
		partialObserved := routingObservations(current[:2])
		published, superseded, err := ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, partialObserved, partialObserved)
		if err != nil || published || !superseded {
			t.Fatalf("partial published=%v superseded=%v err=%v", published, superseded, err)
		}

		observed := routingObservations(current)
		staleLifecycle := append([]AccountRoutingObservation(nil), observed...)
		staleLifecycle[0].Generation++
		published, superseded, err = ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, staleLifecycle)
		if err == nil || published || superseded {
			t.Fatalf("desired lifecycle mismatch should be malformed: published=%v superseded=%v err=%v", published, superseded, err)
		}
	})
}

func TestReorderAccountsIfObservedPublishedDurabilityErrorKeepsCanonicalOrder(t *testing.T) {
	seedRoutingAdminAccounts(t, []Account{{ID: "a", Priority: 10}, {ID: "b", Priority: 10}})
	useFillFirstForRoutingAdminTest(t)
	observed := routingObservations(GetAccounts())
	durabilityErr := injectRoutingDurabilityFailure(t)
	published, superseded, err := ReorderAccountsIfObserved(AccountSelectionStrategyFillFirst, observed, []AccountRoutingObservation{observed[1], observed[0]})
	if !published || superseded || !errors.Is(err, durabilityErr) {
		t.Fatalf("published=%v superseded=%v err=%v, want published durability error", published, superseded, err)
	}
	current := GetAccounts()
	if current[0].ID != "b" || current[1].ID != "a" {
		t.Fatalf("published reorder was rolled back in memory: %#v", current)
	}
}

func TestReorderAccountsIfObservedRejectsMalformedOrder(t *testing.T) {
	seedRoutingAdminAccounts(t, []Account{{ID: "a"}, {ID: "b"}})
	useFillFirstForRoutingAdminTest(t)
	accounts := GetAccounts()
	valid := routingObservations(accounts)
	cases := []struct {
		strategy string
		observed []AccountRoutingObservation
		desired  []AccountRoutingObservation
	}{
		{strategy: AccountSelectionStrategyPriority, observed: valid, desired: valid},
		{strategy: AccountSelectionStrategyFillFirst, observed: []AccountRoutingObservation{{ID: "", Generation: valid[0].Generation}, valid[1]}, desired: valid},
		{strategy: AccountSelectionStrategyFillFirst, observed: []AccountRoutingObservation{{ID: "a", Generation: 0}, valid[1]}, desired: valid},
		{strategy: AccountSelectionStrategyFillFirst, observed: []AccountRoutingObservation{valid[0], valid[0]}, desired: valid},
		{strategy: AccountSelectionStrategyFillFirst, observed: valid, desired: valid[:1]},
		{strategy: AccountSelectionStrategyFillFirst, observed: valid, desired: []AccountRoutingObservation{valid[0], {ID: "x", Generation: valid[1].Generation, Priority: valid[1].Priority}}},
		{strategy: AccountSelectionStrategyFillFirst, observed: valid, desired: []AccountRoutingObservation{valid[0], {ID: valid[1].ID, Generation: valid[1].Generation, Priority: valid[1].Priority + 1}}},
	}
	for _, tc := range cases {
		published, superseded, err := ReorderAccountsIfObserved(tc.strategy, tc.observed, tc.desired)
		if err == nil || published || superseded {
			t.Fatalf("malformed strategy=%q observed=%#v desired=%#v published=%v superseded=%v err=%v", tc.strategy, tc.observed, tc.desired, published, superseded, err)
		}
	}
}
