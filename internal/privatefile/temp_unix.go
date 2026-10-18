//go:build !windows

package privatefile

import (
	"os"
	"path/filepath"
)

// CreateTemp creates a unique same-directory temporary file with the requested
// Unix permissions. Callers own closing and removing the returned file.
func CreateTemp(dir, base string, perm os.FileMode) (*os.File, error) {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(base)+".tmp-*")
	if err != nil {
		return nil, err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, err
	}
	return tmp, nil
}
