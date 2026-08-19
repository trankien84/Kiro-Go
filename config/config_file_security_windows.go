//go:build windows

package config

import "kiro-go/internal/privatefile"

func ensureConfigOwnerPrivate(path string) error {
	return privatefile.EnsureOwnerPrivate(path, 0o600)
}
