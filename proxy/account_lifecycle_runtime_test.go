package proxy

import (
	"path/filepath"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

func lifecycleTestAccount(t *testing.T, id string) config.Account {
	t.Helper()
	account, ok := config.GetAccountByID(id)
	if !ok {
		t.Fatalf("account %q not found", id)
	}
	return account
}

func TestProviderRegistryRejectsRouteFromDifferentAccountLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "provider", Enabled: true, AuthMethod: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	oldAccount := lifecycleTestAccount(t, "provider")
	if err := config.DeleteAccount("provider"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "provider", Enabled: true, AuthMethod: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	newAccount := lifecycleTestAccount(t, "provider")
	if oldAccount.DispatchGeneration == newAccount.DispatchGeneration {
		t.Fatal("expected lifecycle generation to advance")
	}

	routes := newProviderRouteIndex()
	providerID := exactProviderModelID("us.anthropic.test-v1:0")
	routes.Exact[providerID] = resolvedProviderModel{
		RequestedID: RequestedModelID(providerID),
		ProviderID:  providerID,
		Evidence:    providerModelEvidenceDiscovered,
	}
	registry := &providerRegistrySnapshot{
		Version: 9,
		Accounts: map[string]providerAccountAvailability{
			"provider": {
				LifecycleGeneration: newAccount.DispatchGeneration,
				Routes:              routes,
				RefreshedAt:         time.Now(),
				FreshUntil:          time.Now().Add(time.Hour),
				StaleUntil:          time.Now().Add(time.Hour),
			},
		},
	}

	if _, _, ok := resolveCandidateProviderRoute(registry, &oldAccount, string(providerID), string(providerID)); ok {
		t.Fatal("old request lifecycle resolved a provider route owned by recreated account")
	}
	resolved, _, ok := resolveCandidateProviderRoute(registry, &newAccount, string(providerID), string(providerID))
	if !ok || resolved.ProviderID != providerID {
		t.Fatalf("current lifecycle failed to resolve its route: %#v ok=%v", resolved, ok)
	}
}

func TestCustomRelayCreditStateIsLifecycleScoped(t *testing.T) {
	oldSelection := &pool.AccountSelection{
		Account:             config.Account{ID: "relay", DispatchGeneration: 4},
		LifecycleGeneration: 4,
		StatsGeneration:     1,
	}
	newSelection := &pool.AccountSelection{
		Account:             config.Account{ID: "relay", DispatchGeneration: 5},
		LifecycleGeneration: 5,
		StatsGeneration:     1,
	}
	oldKey := customApiLifecycleKeyForForward(forwardParams{selection: oldSelection, account: &oldSelection.Account})
	newKey := customApiLifecycleKeyForForward(forwardParams{selection: newSelection, account: &newSelection.Account})
	if oldKey == newKey {
		t.Fatalf("custom relay rate cache collapsed account lifecycles: %#v", oldKey)
	}
}

func TestBedrockRuntimeCachesDoNotCrossDeleteRecreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "bedrock", Enabled: true, AuthMethod: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	oldAccount := lifecycleTestAccount(t, "bedrock")
	setCachedBedrockModelsForAccount(&oldAccount, []string{"old-model"})
	recordBedrockRegionForAccount(&oldAccount, "old-model", "us-east-1", true)
	bedrockThrottle.recordForAccount(&oldAccount, "old-model", time.Minute)

	if err := config.DeleteAccount("bedrock"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "bedrock", Enabled: true, AuthMethod: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	newAccount := lifecycleTestAccount(t, "bedrock")
	if _, ok := getCachedBedrockModelsForAccount(&newAccount); ok {
		t.Fatal("recreated account inherited Bedrock discovery cache")
	}
	if _, ok := getBedrockRouteForAccount(&newAccount, "old-model"); ok {
		t.Fatal("recreated account inherited Bedrock region route")
	}
	if remaining := bedrockThrottle.remainingForAccount(&newAccount, "old-model"); remaining > 0 {
		t.Fatalf("recreated account inherited Bedrock throttle: %v", remaining)
	}

	setCachedBedrockModelsForAccount(&newAccount, []string{"new-model"})
	// A current N+1 publisher must not destructively delete N. Cleanup belongs to
	// an exact-key lifecycle sweeper, otherwise a stale N publisher can race in
	// the opposite direction and erase N+1.
	if _, ok := getCachedBedrockModelsForAccount(&oldAccount); !ok {
		t.Fatal("N+1 publication cross-deleted the retired N cache entry")
	}

	// Simulate the dangerous interleaving directly: N validated before recreate,
	// N+1 published, then N resumed. The late N write may repopulate only N and
	// must leave N+1 intact.
	setCachedBedrockModelsTTLForAccount(&oldAccount, []string{"late-old-model"}, time.Minute)
	models, ok := getCachedBedrockModelsForAccount(&newAccount)
	if !ok || len(models) != 1 || models[0] != "new-model" {
		t.Fatalf("late N publication damaged N+1 cache: models=%#v ok=%v", models, ok)
	}

	sweepRetiredBedrockLifecycleState()
	if _, ok := getCachedBedrockModelsForAccount(&oldAccount); ok {
		t.Fatal("retired N cache survived exact-key lifecycle sweep")
	}
	models, ok = getCachedBedrockModelsForAccount(&newAccount)
	if !ok || len(models) != 1 || models[0] != "new-model" {
		t.Fatalf("lifecycle sweep collateral-deleted N+1: models=%#v ok=%v", models, ok)
	}
}

func TestProviderRuntimeRebuildCannotDeleteNewLifecycleState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(path); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "provider-runtime", Enabled: true, AuthMethod: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	oldAccount := lifecycleTestAccount(t, "provider-runtime")
	if err := config.DeleteAccount("provider-runtime"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{ID: "provider-runtime", Enabled: true, AuthMethod: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	newAccount := lifecycleTestAccount(t, "provider-runtime")
	if oldAccount.DispatchGeneration == newAccount.DispatchGeneration {
		t.Fatal("expected lifecycle generation to advance")
	}

	oldKey := accountLifecycleKey{accountID: oldAccount.ID, generation: oldAccount.DispatchGeneration}
	newKey := accountLifecycleKey{accountID: newAccount.ID, generation: newAccount.DispatchGeneration}
	now := time.Now()
	h := &Handler{
		accountModelSnapshots:     make(map[accountLifecycleKey]accountModelSnapshot),
		providerCooldownUntil:     make(map[accountLifecycleKey]time.Time),
		providerModelDeniedUntil:  make(map[accountLifecycleKey]map[string]time.Time),
		providerSourceGenerations: make(map[accountLifecycleKey]uint64),
	}

	// N+1 publishes first.
	h.providerCooldownUntil[newKey] = now.Add(time.Hour)
	h.providerModelDeniedUntil[newKey] = map[string]time.Time{
		exactProviderDenialKey("claude-new"): now.Add(time.Hour),
	}
	// Then a stale N writer resumes after an earlier lifecycle check. Exact-key
	// storage means its write can only touch N, never steal ownership from N+1.
	h.providerCooldownUntil[oldKey] = now.Add(2 * time.Hour)
	h.providerModelDeniedUntil[oldKey] = map[string]time.Time{
		exactProviderDenialKey("claude-old"): now.Add(2 * time.Hour),
	}
	h.providerSourceGenerations[oldKey] = 3
	h.providerSourceGenerations[newKey] = 7

	h.rebuildEnabledModelUnion()

	if until := h.providerCooldownUntil[newKey]; !until.After(now) {
		t.Fatalf("current lifecycle cooldown was deleted: %v", until)
	}
	if denied := h.providerModelDeniedUntil[newKey]; !denied[exactProviderDenialKey("claude-new")].After(now) {
		t.Fatalf("current lifecycle model denial was deleted: %#v", denied)
	}
	if _, ok := h.providerCooldownUntil[oldKey]; ok {
		t.Fatal("retired lifecycle cooldown survived exact-key sweep")
	}
	if _, ok := h.providerModelDeniedUntil[oldKey]; ok {
		t.Fatal("retired lifecycle denial survived exact-key sweep")
	}
	if _, ok := h.providerSourceGenerations[oldKey]; ok {
		t.Fatal("retired lifecycle source fence survived exact-key sweep")
	}
	if got := h.providerSourceGenerations[newKey]; got != 7 {
		t.Fatalf("current lifecycle source fence was changed: got=%d want=7", got)
	}
}
