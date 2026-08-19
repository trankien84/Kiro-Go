//go:build !windows

package proxy

import "os"

// publishResponseStateNoReplace publishes source at destination without
// replacing an existing destination. If published is true and err is non-nil,
// the destination exists and err only describes failure to remove the now-stale
// source link; callers must still perform the directory durability barrier.
func publishResponseStateNoReplace(source, destination string) (published bool, err error) {
	if err := os.Link(source, destination); err != nil {
		return false, err
	}
	if err := os.Remove(source); err != nil {
		return true, err
	}
	return true, nil
}

func syncResponseStateDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
