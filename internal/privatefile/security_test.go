//go:build !windows

package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureOwnerPrivateHardensExistingUnixFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-state.json")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnerPrivate(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := InspectOwnerPrivate(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("existing file was not hardened to owner-private mode")
	}
}
