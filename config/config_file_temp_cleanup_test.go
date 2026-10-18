package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPurgeStaleConfigTempFilesIsScopedAndAgeGuarded(t *testing.T) {
	dir := t.TempDir()
	base := "config.json"
	now := time.Now()
	stale := filepath.Join(dir, ".config.json.tmp-stale")
	recent := filepath.Join(dir, ".config.json.tmp-recent")
	foreign := filepath.Join(dir, ".other.json.tmp-stale")
	for _, path := range []string{stale, recent, foreign} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-configTempCleanupGrace - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}

	purgeStaleConfigTempFiles(dir, base, now)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale own temp was not removed: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent temp was removed: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign temp was removed: %v", err)
	}
}
