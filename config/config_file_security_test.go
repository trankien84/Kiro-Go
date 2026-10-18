package config

import (
	"kiro-go/internal/privatefile"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureConfigOwnerPrivateHardensExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"password":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfigOwnerPrivate(path); err != nil {
		t.Fatal(err)
	}
	ok, err := privatefile.InspectOwnerPrivate(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("existing config was not hardened to owner-private protection")
	}
}
