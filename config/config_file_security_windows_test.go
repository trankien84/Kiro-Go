//go:build windows

package config

import (
	"kiro-go/internal/privatefile"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceExistingConfigEndsOwnerPrivateWindows(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "config.json")
	if err := os.WriteFile(destination, []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}

	tmp, err := createConfigTempFile(dir, filepath.Base(destination), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	source := tmp.Name()
	if _, err := tmp.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(source)

	published, err := replaceConfigFile(source, destination)
	if err != nil || !published {
		t.Fatalf("replaceConfigFile published=%v err=%v", published, err)
	}
	ok, err := privatefile.InspectOwnerPrivate(destination, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ReplaceFileW path did not preserve a hardened owner-private final DACL")
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement data=%q err=%v", data, err)
	}
}
