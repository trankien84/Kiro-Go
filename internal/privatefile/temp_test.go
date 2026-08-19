//go:build !windows

package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateTempUsesRequestedUnixPermissions(t *testing.T) {
	dir := t.TempDir()
	file, err := CreateTemp(dir, "state.json", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	defer os.Remove(path)
	defer file.Close()

	if filepath.Dir(path) != dir {
		t.Fatalf("temp dir=%q want=%q", filepath.Dir(path), dir)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode=%#o want=0600", got)
	}
}
