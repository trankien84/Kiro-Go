package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadMigratesLegacyDirectAnthropicBearerMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
		"password":"changeme",
		"port":8080,
		"host":"127.0.0.1",
		"accounts":[{
			"id":"anthropic-legacy",
			"authMethod":"anthropic_api",
			"provider":"Anthropic",
			"baseUrl":"https://api.anthropic.com",
			"kiroApiKey":"sk-ant-static",
			"accessToken":"sk-ant-static",
			"expiresAt":123456789,
			"enabled":true
		}]
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	account, ok := GetAccountByID("anthropic-legacy")
	if !ok {
		t.Fatal("migrated direct Anthropic account missing")
	}
	if account.KiroApiKey != "sk-ant-static" {
		t.Fatalf("static key changed during migration: %q", account.KiroApiKey)
	}
	if account.AccessToken != "" || account.ExpiresAt != 0 {
		t.Fatalf("legacy bearer mirror survived migration: access=%q expires=%d", account.AccessToken, account.ExpiresAt)
	}

	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk Config
	if err := json.Unmarshal(persisted, &disk); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	if len(disk.Accounts) != 1 || disk.Accounts[0].AccessToken != "" || disk.Accounts[0].ExpiresAt != 0 {
		t.Fatalf("migration was not persisted: %s", persisted)
	}
	// The static key remains only in its provider-specific field. The AccessToken
	// JSON field is currently not omitempty, so assert value separation rather
	// than absence of the field name.
	if strings.Count(string(persisted), "sk-ant-static") != 1 {
		t.Fatalf("static Anthropic key persisted in more than one state slot: %s", persisted)
	}
}

func TestFirstCreateConfigIsOwnerOnlyOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows DACL is covered by the Windows-specific creation path and cross-compile validation")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("first-created config mode=%#o, want 0600", got)
	}
}

func TestUpdateAccountStatsResetAdvancesGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "stats-reset", Enabled: true, RequestCount: 4, TotalTokens: 40, LastUsed: 10}); err != nil {
		t.Fatal(err)
	}
	oldGeneration := generationForTestAccount(t, "stats-reset")
	if err := UpdateAccountStats("stats-reset", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	newGeneration := generationForTestAccount(t, "stats-reset")
	if newGeneration != oldGeneration+1 {
		t.Fatalf("stats reset generation old=%d new=%d", oldGeneration, newGeneration)
	}
}

func TestLoadPreservesLegacyDirectAnthropicBearerOnlyCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
		"password":"changeme",
		"port":8080,
		"host":"127.0.0.1",
		"accounts":[{
			"id":"anthropic-bearer-only",
			"authMethod":"anthropic_api",
			"provider":"Anthropic",
			"baseUrl":"https://api.anthropic.com",
			"accessToken":"sk-ant-legacy-only",
			"expiresAt":123456789,
			"enabled":true
		}]
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	account, ok := GetAccountByID("anthropic-bearer-only")
	if !ok {
		t.Fatal("migrated direct Anthropic account missing")
	}
	if account.KiroApiKey != "sk-ant-legacy-only" {
		t.Fatalf("legacy credential was not promoted to provider-specific slot: %q", account.KiroApiKey)
	}
	if account.AccessToken != "" || account.ExpiresAt != 0 {
		t.Fatalf("legacy bearer state survived migration: access=%q expires=%d", account.AccessToken, account.ExpiresAt)
	}

	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(persisted), "sk-ant-legacy-only") != 1 {
		t.Fatalf("legacy direct Anthropic credential was lost or duplicated: %s", persisted)
	}
}
