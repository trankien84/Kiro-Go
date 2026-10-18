package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func initPhase1Config(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
}

func TestPhase1AgentToolSteeringRemainsDefaultOffAndTransactional(t *testing.T) {
	initPhase1Config(t)
	if GetAgentToolSteering() {
		t.Fatal("AgentToolSteering must default to false")
	}
	if err := UpdateAgentToolSteering(true); err != nil {
		t.Fatal(err)
	}
	if !GetAgentToolSteering() {
		t.Fatal("AgentToolSteering update was not published")
	}
}

func TestPhase1APIKeyDuplicateIsScopedToKiroRegion(t *testing.T) {
	initPhase1Config(t)
	if err := AddAccount(Account{ID: "api-east", KiroApiKey: "ksk_phase1|us-east-1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "api-east-dup", KiroApiKey: "ksk_phase1", Region: "us-east-1", Enabled: true}); !errors.Is(err, ErrDuplicateAPIKey) {
		t.Fatalf("same key+region error=%v, want ErrDuplicateAPIKey", err)
	}
	if err := AddAccount(Account{ID: "api-west", KiroApiKey: "ksk_phase1|us-west-2", Enabled: true}); err != nil {
		t.Fatalf("same key in a different Kiro region must remain a separate pool slot: %v", err)
	}
}

func TestPhase1NonKiroProviderWithKiroApiKeyFieldIsNotReclassified(t *testing.T) {
	initPhase1Config(t)
	account := Account{
		ID:         "anthropic",
		AuthMethod: "anthropic_api",
		KiroApiKey: "sk-ant-phase1",
		Enabled:    true,
	}
	if err := AddAccount(account); err != nil {
		t.Fatal(err)
	}
	got, ok := GetAccountByID(account.ID)
	if !ok {
		t.Fatal("account missing after add")
	}
	if got.Kind() != AccountKindDirectAnthropic || got.AuthMethod != "anthropic_api" {
		t.Fatalf("non-Kiro provider was reclassified: %#v", got)
	}
	if got.AccessToken != "" {
		t.Fatalf("direct Anthropic credential leaked into shared Kiro bearer state: %#v", got)
	}
}

func TestPhase1UpdateAccountPreservesCredentialStateButAllowsRegionEdit(t *testing.T) {
	initPhase1Config(t)
	if err := AddAccount(Account{
		ID:           "admin-update",
		AuthMethod:   "social",
		Provider:     "GitHub",
		AccessToken:  "fresh-access",
		RefreshToken: "fresh-refresh",
		Region:       "us-east-1",
		ProfileArn:   "arn:fresh",
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	before, ok := GetAccountByID("admin-update")
	if !ok {
		t.Fatal("missing account")
	}
	stale := before.Clone()
	stale.AccessToken = "stale-access"
	stale.RefreshToken = "stale-refresh"
	stale.ProfileArn = "arn:stale"
	stale.Region = "us-west-2"
	stale.Nickname = "updated"
	if err := UpdateAccount(before.ID, stale); err != nil {
		t.Fatal(err)
	}
	after, _ := GetAccountByID(before.ID)
	if after.AccessToken != "fresh-access" || after.RefreshToken != "fresh-refresh" || after.ProfileArn != "arn:fresh" {
		t.Fatalf("stale administrative snapshot rolled back credentials: %#v", after)
	}
	if after.Region != "us-west-2" || after.Nickname != "updated" {
		t.Fatalf("legitimate admin fields were not applied: %#v", after)
	}
	if after.DispatchGeneration == before.DispatchGeneration {
		t.Fatal("dispatch identity change did not advance lifecycle generation")
	}
	cfgLock.RLock()
	canonicalGeneration := cfg.Accounts[0].DispatchGeneration
	cfgLock.RUnlock()
	if canonicalGeneration != 0 {
		t.Fatalf("request lifecycle fence leaked into canonical config: %d", canonicalGeneration)
	}
}

func TestPhase1StaleLifecycleCannotPersistRegionOrRefreshedCredential(t *testing.T) {
	initPhase1Config(t)
	if err := AddAccount(Account{
		ID:           "reused",
		AuthMethod:   "social",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		Region:       "us-east-1",
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	old, _ := GetAccountByID("reused")
	if err := DeleteAccount("reused"); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{
		ID:           "reused",
		AuthMethod:   "social",
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		Region:       "eu-central-1",
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	if applied, err := UpdateAccountRegionIfLifecycle("reused", old.DispatchGeneration, "ap-southeast-1"); err != nil || applied {
		t.Fatalf("stale region update applied=%t err=%v", applied, err)
	}
	if applied, err := UpdateAccountCredentialStateIfLifecycle("reused", old.DispatchGeneration, "stale-access", "stale-refresh", 123, "arn:stale"); err != nil || applied {
		t.Fatalf("stale credential update applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID("reused")
	if current.Region != "eu-central-1" || current.AccessToken != "new-access" || current.RefreshToken != "new-refresh" {
		t.Fatalf("recreated account was contaminated by stale request state: %#v", current)
	}
}
