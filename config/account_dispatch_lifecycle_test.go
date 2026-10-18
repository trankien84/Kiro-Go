package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountCloneDeepCopiesNestedState(t *testing.T) {
	original := Account{
		ID:             "clone",
		Tags:           []string{"one"},
		BedrockRegions: []string{"us-east-1"},
		BedrockModelMap: map[string]string{
			"claude": "model-a",
		},
	}
	clone := original.Clone()
	clone.Tags[0] = "two"
	clone.BedrockRegions[0] = "us-west-2"
	clone.BedrockModelMap["claude"] = "model-b"

	if original.Tags[0] != "one" {
		t.Fatalf("Tags backing storage was shared: %#v", original.Tags)
	}
	if original.BedrockRegions[0] != "us-east-1" {
		t.Fatalf("BedrockRegions backing storage was shared: %#v", original.BedrockRegions)
	}
	if original.BedrockModelMap["claude"] != "model-a" {
		t.Fatalf("BedrockModelMap was shared: %#v", original.BedrockModelMap)
	}
}

func TestAccountLifecycleFencesDeleteRecreateAndPreservesExactID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", " a "} {
		if err := AddAccount(Account{ID: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	plain, ok := GetAccountByID("a")
	if !ok || plain.DispatchGeneration == 0 {
		t.Fatalf("missing lifecycle for exact id a: %#v", plain)
	}
	spaced, ok := GetAccountByID(" a ")
	if !ok || spaced.DispatchGeneration == 0 {
		t.Fatalf("missing lifecycle for exact spaced id: %#v", spaced)
	}
	if plain.ID == spaced.ID {
		t.Fatal("exact account IDs were canonicalized")
	}

	oldGeneration := plain.DispatchGeneration
	if err := DeleteAccount("a"); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	current, ok := GetAccountByID("a")
	if !ok || current.DispatchGeneration == oldGeneration {
		t.Fatalf("delete/recreate did not advance lifecycle: old=%d current=%d", oldGeneration, current.DispatchGeneration)
	}
	applied, err := SetAccountBanStatusIfLifecycle("a", oldGeneration, "BANNED", "stale")
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("stale lifecycle mutation was applied to recreated account")
	}
	current, _ = GetAccountByID("a")
	if !current.Enabled || current.BanStatus == "BANNED" {
		t.Fatalf("recreated account was contaminated by stale mutation: %#v", current)
	}
	infoApplied, err := UpdateAccountInfoIfLifecycle("a", oldGeneration, AccountInfo{UsageCurrent: 99, LastRefresh: 123})
	if err != nil {
		t.Fatal(err)
	}
	if infoApplied {
		t.Fatal("stale account-info refresh was applied to recreated account")
	}
	current, _ = GetAccountByID("a")
	if current.UsageCurrent != 0 || current.LastRefresh != 0 {
		t.Fatalf("recreated account inherited stale account-info state: %#v", current)
	}
}

func TestWriteConfigBytesDoesNotTruncateDestinationWhenReplaceFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"stable":true}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}

	previous := replaceConfigFileForWrite
	replaceConfigFileForWrite = func(string, string) (bool, error) { return false, errors.New("replace failed") }
	defer func() { replaceConfigFileForWrite = previous }()

	if _, err := writeConfigBytes(path, []byte(`{"new":true}`), 0600); err == nil {
		t.Fatal("expected replace failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("destination changed after failed publish: got=%q want=%q", got, original)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("destination became invalid JSON: %v", err)
	}
}

func TestStatsBatchDirectorySyncFailureKeepsMemoryAlignedAndRetriesVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "stats-durable", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	inventory := GetAccountsWithStatsGeneration()
	if len(inventory) != 1 || inventory[0].StatsGeneration == 0 {
		t.Fatalf("unexpected inventory: %#v", inventory)
	}
	write := AccountStatsWrite{
		AccountID:  "stats-durable",
		Generation: inventory[0].StatsGeneration,
		Version:    inventory[0].StatsVersion + 1,
		Counters: AccountStatsCounters{
			RequestCount: 1,
			TotalTokens:  7,
			TotalCredits: 1.5,
			LastUsed:     123,
		},
	}

	previousSync := syncConfigDirectoryForWrite
	syncConfigDirectoryForWrite = func(string) error { return errors.New("directory sync failed") }
	_, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{write})
	syncConfigDirectoryForWrite = previousSync
	if err == nil || !configFileWasReplaced(err) {
		t.Fatalf("expected post-replace durability error, got %v", err)
	}

	current, ok := GetAccountByID("stats-durable")
	if !ok || current.RequestCount != 1 || current.TotalTokens != 7 || current.TotalCredits != 1.5 {
		t.Fatalf("in-memory config diverged after replacement: %#v ok=%t", current, ok)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var disk Config
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("replacement left invalid JSON: %v", err)
	}
	if len(disk.Accounts) != 1 || disk.Accounts[0].RequestCount != 1 || disk.Accounts[0].TotalTokens != 7 {
		t.Fatalf("disk image does not match published counters: %#v", disk.Accounts)
	}

	// The applied version was intentionally not advanced on the failed durability
	// barrier, so retrying the same cumulative snapshot must perform another write
	// and commit the version once the barrier succeeds.
	if _, err := UpdateAccountStatsBatchIfGeneration([]AccountStatsWrite{write}); err != nil {
		t.Fatalf("same-version durability retry failed: %v", err)
	}
	inventory = GetAccountsWithStatsGeneration()
	if len(inventory) != 1 || inventory[0].StatsVersion != write.Version {
		t.Fatalf("durable retry did not commit version: %#v", inventory)
	}
}

func TestConfigCloneDeepCopiesReferenceBackedState(t *testing.T) {
	fallback := true
	autoRecover := false
	original := &Config{
		ApiKeys: []ApiKeyEntry{{ID: "k1", Name: "one"}},
		Accounts: []Account{{
			ID:              "a",
			Tags:            []string{"tag-a"},
			BedrockRegions:  []string{"us-east-1"},
			BedrockModelMap: map[string]string{"claude": "model-a"},
		}},
		EndpointFallback:   &fallback,
		AutoRecoverEnabled: &autoRecover,
		ProxyURLs:          []string{"http://proxy-a"},
		PromptFilterRules:  []PromptFilterRule{{Match: "one"}},
	}
	clone := original.Clone()
	clone.ApiKeys[0].Name = "two"
	clone.Accounts[0].Tags[0] = "tag-b"
	clone.Accounts[0].BedrockRegions[0] = "us-west-2"
	clone.Accounts[0].BedrockModelMap["claude"] = "model-b"
	*clone.EndpointFallback = false
	*clone.AutoRecoverEnabled = true
	clone.ProxyURLs[0] = "http://proxy-b"
	clone.PromptFilterRules[0].Match = "two"
	if original.ApiKeys[0].Name != "one" || original.Accounts[0].Tags[0] != "tag-a" ||
		original.Accounts[0].BedrockRegions[0] != "us-east-1" || original.Accounts[0].BedrockModelMap["claude"] != "model-a" ||
		!*original.EndpointFallback || *original.AutoRecoverEnabled || original.ProxyURLs[0] != "http://proxy-a" ||
		original.PromptFilterRules[0].Match != "one" {
		t.Fatalf("Config.Clone shared mutable backing state: original=%#v clone=%#v", original, clone)
	}
}

func TestConfigTransactionPrePublishFailureLeavesMemoryAndDiskUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatal(err)
	}
	beforeDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previousReplace := replaceConfigFileForWrite
	replaceConfigFileForWrite = func(string, string) (bool, error) { return false, errors.New("injected replace failure") }
	defer func() { replaceConfigFileForWrite = previousReplace }()
	if err := UpdatePreferredEndpoint("amazonq"); err == nil {
		t.Fatal("expected injected pre-publication failure")
	}
	if got := GetPreferredEndpoint(); got != "kiro" {
		t.Fatalf("failed transaction leaked staged memory: got endpoint %q", got)
	}
	afterDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDisk) != string(beforeDisk) {
		t.Fatalf("failed transaction changed disk image\nbefore=%s\nafter=%s", beforeDisk, afterDisk)
	}
}

func TestConfigTransactionPostPublishDurabilityFailureKeepsMemoryAligned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatal(err)
	}
	previousSync := syncConfigDirectoryForWrite
	syncConfigDirectoryForWrite = func(string) error { return errors.New("injected directory sync failure") }
	err := UpdatePreferredEndpoint("amazonq")
	syncConfigDirectoryForWrite = previousSync
	if err == nil || !configFileWasReplaced(err) {
		t.Fatalf("expected post-publication durability error, got %v", err)
	}
	if got := GetPreferredEndpoint(); got != "amazonq" {
		t.Fatalf("memory did not follow published disk image: got endpoint %q", got)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var disk Config
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.PreferredEndpoint != "amazonq" {
		t.Fatalf("disk did not contain published transaction: %#v", disk)
	}
}

func TestAdminLifecycleMutationPreservesConcurrentRefreshFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "admin-patch", AuthMethod: "social", Enabled: true, Nickname: "old-name", AccessToken: "token-old", RefreshToken: "refresh-old", UsageCurrent: 1}); err != nil {
		t.Fatal(err)
	}
	observed, ok := GetAccountByID("admin-patch")
	if !ok || observed.DispatchGeneration == 0 {
		t.Fatalf("missing observed lifecycle: %#v", observed)
	}
	generation := observed.DispatchGeneration
	if applied, err := UpdateAccountTokenIfLifecycle(observed.ID, generation, "token-new", "refresh-new", 12345); err != nil || !applied {
		t.Fatalf("background token refresh failed: applied=%t err=%v", applied, err)
	}
	if applied, err := UpdateAccountInfoIfLifecycle(observed.ID, generation, AccountInfo{UsageCurrent: 42, UsageLimit: 100, LastRefresh: 999}); err != nil || !applied {
		t.Fatalf("background account-info refresh failed: applied=%t err=%v", applied, err)
	}
	before, after, applied, err := MutateAccountIfLifecycle(observed.ID, generation, func(account *Account) error { account.Nickname = "admin-name"; return nil })
	if err != nil || !applied {
		t.Fatalf("admin patch failed: applied=%t err=%v", applied, err)
	}
	if before.AccessToken != "token-new" || before.UsageCurrent != 42 {
		t.Fatalf("admin mutation did not stage from canonical current state: %#v", before)
	}
	if after.Nickname != "admin-name" || after.AccessToken != "token-new" || after.RefreshToken != "refresh-new" || after.UsageCurrent != 42 || after.LastRefresh != 999 {
		t.Fatalf("admin patch overwrote concurrent refresh fields: %#v", after)
	}
}

func TestAdminLifecycleMutationAndDeleteRejectRecreatedAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "reused", AuthMethod: "social", Nickname: "old", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	old, ok := GetAccountByID("reused")
	if !ok || old.DispatchGeneration == 0 {
		t.Fatalf("missing old lifecycle: %#v", old)
	}
	if err := DeleteAccount("reused"); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "reused", AuthMethod: "social", Nickname: "new", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	current, ok := GetAccountByID("reused")
	if !ok || current.DispatchGeneration == old.DispatchGeneration {
		t.Fatalf("expected replacement lifecycle: old=%d current=%#v", old.DispatchGeneration, current)
	}
	_, _, applied, err := MutateAccountIfLifecycle(old.ID, old.DispatchGeneration, func(account *Account) error { account.Nickname = "stale-admin"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("stale admin update applied to recreated account")
	}
	if _, applied, err := DeleteAccountIfLifecycle(old.ID, old.DispatchGeneration); err != nil {
		t.Fatal(err)
	} else if applied {
		t.Fatal("stale admin delete removed recreated account")
	}
	current, ok = GetAccountByID("reused")
	if !ok || current.Nickname != "new" {
		t.Fatalf("recreated account was changed by stale admin operation: %#v ok=%t", current, ok)
	}
}

func TestAccountLifecycleMutationPrePublishFailureDoesNotAdvanceGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "source", AuthMethod: "anthropic_api", BaseURL: "https://api.anthropic.com", KiroApiKey: "one", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before, ok := GetAccountByID("source")
	if !ok || before.DispatchGeneration == 0 {
		t.Fatalf("missing source lifecycle: %#v", before)
	}
	previousReplace := replaceConfigFileForWrite
	replaceConfigFileForWrite = func(string, string) (bool, error) { return false, errors.New("injected replace failure") }
	_, _, applied, err := MutateAccountIfLifecycle(before.ID, before.DispatchGeneration, func(account *Account) error { account.KiroApiKey = "two"; return nil })
	replaceConfigFileForWrite = previousReplace
	if err == nil || applied {
		t.Fatalf("expected failed unpublished lifecycle mutation: applied=%t err=%v", applied, err)
	}
	after, ok := GetAccountByID("source")
	if !ok || after.KiroApiKey != "one" || after.DispatchGeneration != before.DispatchGeneration {
		t.Fatalf("failed publication changed account lifecycle: before=%#v after=%#v", before, after)
	}
}

func TestConfigGetReturnsDetachedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{
		ID:              "snapshot",
		Enabled:         true,
		Tags:            []string{"live-tag"},
		BedrockRegions:  []string{"us-east-1"},
		BedrockModelMap: map[string]string{"claude": "live-model"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateProxySettings("", []string{"http://live-proxy"}, 10); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePromptFilterConfig(true, true, true, []PromptFilterRule{{ID: "r1", Match: "live"}}); err != nil {
		t.Fatal(err)
	}

	snapshot := Get()
	if snapshot == nil {
		t.Fatal("Get returned nil snapshot")
	}
	snapshot.ProxyURLs[0] = "http://mutated-proxy"
	snapshot.PromptFilterRules[0].Match = "mutated"
	snapshot.Accounts[0].Tags[0] = "mutated-tag"
	snapshot.Accounts[0].BedrockRegions[0] = "eu-west-1"
	snapshot.Accounts[0].BedrockModelMap["claude"] = "mutated-model"

	current := Get()
	if current.ProxyURLs[0] != "http://live-proxy" || current.PromptFilterRules[0].Match != "live" ||
		current.Accounts[0].Tags[0] != "live-tag" || current.Accounts[0].BedrockRegions[0] != "us-east-1" ||
		current.Accounts[0].BedrockModelMap["claude"] != "live-model" {
		t.Fatalf("mutating Get snapshot changed authoritative config: %#v", current)
	}
}

func TestInitMigrationPrePublishFailureKeepsPreviousConfigAndPath(t *testing.T) {
	dir := t.TempDir()
	stablePath := filepath.Join(dir, "stable.json")
	if err := Init(stablePath); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "stable", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	migrationPath := filepath.Join(dir, "legacy.json")
	legacy := []byte(`{"password":"legacy","apiKey":"legacy-key","requireApiKey":true,"accounts":[]}`)
	if err := os.WriteFile(migrationPath, legacy, 0600); err != nil {
		t.Fatal(err)
	}

	previousReplace := replaceConfigFileForWrite
	replaceConfigFileForWrite = func(string, string) (bool, error) {
		return false, errors.New("injected migration publish failure")
	}
	defer func() { replaceConfigFileForWrite = previousReplace }()

	if err := Init(migrationPath); err == nil {
		t.Fatal("expected migration publication failure")
	}
	if cfgPath != stablePath {
		t.Fatalf("failed migration published new config path: got=%q want=%q", cfgPath, stablePath)
	}
	if _, ok := GetAccountByID("stable"); !ok {
		t.Fatal("failed migration replaced previous in-memory config")
	}
	got, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(legacy) {
		t.Fatalf("failed migration changed destination file:\ngot=%s\nwant=%s", got, legacy)
	}
}

func TestSaveCannotOverwriteNewerTransactionWithStaleSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatal(err)
	}

	previousReplace := replaceConfigFileForWrite
	var calls atomic.Int32
	saveReachedReplace := make(chan struct{})
	releaseSave := make(chan struct{})
	secondReplace := make(chan struct{}, 1)
	replaceConfigFileForWrite = func(source, destination string) (bool, error) {
		call := calls.Add(1)
		if call == 1 {
			close(saveReachedReplace)
			<-releaseSave
		} else if call == 2 {
			secondReplace <- struct{}{}
		}
		return previousReplace(source, destination)
	}
	defer func() { replaceConfigFileForWrite = previousReplace }()

	saveDone := make(chan error, 1)
	go func() { saveDone <- Save() }()
	<-saveReachedReplace

	updateDone := make(chan error, 1)
	go func() { updateDone <- UpdatePreferredEndpoint("amazonq") }()

	// With Save serialized by cfgLock, the update cannot reach the replacement
	// hook until Save publishes and releases the lock. Without that serialization,
	// it becomes call #2 and publishes first, after which the stale Save overwrites it.
	select {
	case <-secondReplace:
		close(releaseSave)
		<-saveDone
		<-updateDone
		t.Fatal("newer transaction reached file publication while Save was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("unexpected replacement count before releasing Save: %d", got)
	}
	close(releaseSave)
	if err := <-saveDone; err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if err := <-updateDone; err != nil {
		t.Fatalf("newer transaction failed: %v", err)
	}

	if got := GetPreferredEndpoint(); got != "amazonq" {
		t.Fatalf("memory lost newer transaction: %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk Config
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.PreferredEndpoint != "amazonq" {
		t.Fatalf("stale Save overwrote newer disk image: %#v", disk)
	}
}
