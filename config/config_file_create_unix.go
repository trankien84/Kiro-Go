//go:build !windows

package config

import (
	"kiro-go/internal/privatefile"
	"os"
)

func createConfigTempFile(dir, base string, perm os.FileMode) (*os.File, error) {
	return privatefile.CreateTemp(dir, base, perm)
}
