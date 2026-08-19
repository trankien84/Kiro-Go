//go:build !windows

package config

import "os"

func replaceConfigFile(source, destination string) (bool, error) {
	if err := os.Rename(source, destination); err != nil {
		return false, err
	}
	return true, nil
}
func syncConfigDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
