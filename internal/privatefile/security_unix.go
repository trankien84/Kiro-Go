//go:build !windows

package privatefile

import "os"

// EnsureOwnerPrivate applies the owner-private mode used for sensitive state.
// Unix ownership itself is left unchanged; callers must already own or have
// sufficient permission to chmod the file.
func EnsureOwnerPrivate(path string, perm os.FileMode) error {
	ok, err := InspectOwnerPrivate(path, perm)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return os.Chmod(path, perm.Perm())
}

func InspectOwnerPrivate(path string, perm os.FileMode) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm() == perm.Perm(), nil
}
