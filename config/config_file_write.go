package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const configTempCleanupGrace = 24 * time.Hour

var (
	replaceConfigFileForWrite   = replaceConfigFile
	syncConfigDirectoryForWrite = syncConfigDirectory
)

type configFileDurabilityError struct{ err error }

func (e *configFileDurabilityError) Error() string {
	return fmt.Sprintf("configuration file was published but durability sync failed: %v", e.err)
}
func (e *configFileDurabilityError) Unwrap() error { return e.err }
func configFileWasReplaced(err error) bool {
	var e *configFileDurabilityError
	return errors.As(err, &e)
}

func purgeStaleConfigTempFiles(dir, base string, now time.Time) {
	prefix := "." + filepath.Base(base) + ".tmp-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-configTempCleanupGrace)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || len(entry.Name()) == len(prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

func writeConfigBytes(path string, data []byte, perm os.FileMode) (published bool, err error) {
	if path == "" {
		return false, fmt.Errorf("configuration path is empty")
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	purgeStaleConfigTempFiles(dir, base, time.Now())
	tmp, err := createConfigTempFile(dir, base, perm)
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	keepTemp := true
	defer func() {
		_ = tmp.Close()
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	published, err = replaceConfigFileForWrite(tmpPath, path)
	if err != nil {
		if published {
			keepTemp = false
			return true, &configFileDurabilityError{err: err}
		}
		return false, err
	}
	if !published {
		return false, fmt.Errorf("configuration replacement returned without publishing destination")
	}
	keepTemp = false
	if err := syncConfigDirectoryForWrite(dir); err != nil {
		return true, &configFileDurabilityError{err: err}
	}
	return true, nil
}
