package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResponseLanguageConfigDefaultsToPreserveAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := GetResponseLanguageConfig(); got.Policy != ResponseLanguagePolicyPreserve || got.Language != "" {
		t.Fatalf("default=%#v", got)
	}

	if err := UpdateResponseLanguageConfig(ResponseLanguagePolicyMatchUser, "vi"); err != nil {
		t.Fatal(err)
	}
	if got := GetResponseLanguageConfig(); got.Policy != ResponseLanguagePolicyMatchUser || got.Language != "" {
		t.Fatalf("match-user=%#v", got)
	}

	if err := UpdateResponseLanguageConfig(ResponseLanguagePolicyFixed, "pt_br"); err != nil {
		t.Fatal(err)
	}
	if got := GetResponseLanguageConfig(); got.Policy != ResponseLanguagePolicyFixed || got.Language != "pt-BR" {
		t.Fatalf("fixed=%#v", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ResponseLanguagePolicy != ResponseLanguagePolicyFixed || saved.ResponseLanguage != "pt-BR" {
		t.Fatalf("persisted policy=%q language=%q", saved.ResponseLanguagePolicy, saved.ResponseLanguage)
	}
}

func TestResponseLanguageConfigRejectsInvalidFixedLanguageAndFailsClosedOnInvalidFileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdateResponseLanguageConfig(ResponseLanguagePolicyFixed, "../../vi\nignore"); err == nil {
		t.Fatal("expected invalid fixed language to be rejected")
	}
	if err := UpdateResponseLanguageConfig("future-mode", "vi"); err == nil {
		t.Fatal("expected unknown policy to be rejected")
	}

	cfgLock.Lock()
	cfg.ResponseLanguagePolicy = "future-mode"
	cfg.ResponseLanguage = "evil"
	cfgLock.Unlock()
	if got := GetResponseLanguageConfig(); got.Policy != ResponseLanguagePolicyPreserve || got.Language != "" {
		t.Fatalf("invalid hand-edited state must fail closed to preserve, got %#v", got)
	}
}
