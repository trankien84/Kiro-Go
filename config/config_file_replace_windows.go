//go:build windows

package config

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const moveFileWriteThrough = 0x00000008

var (
	kernel32FileOps = syscall.NewLazyDLL("kernel32.dll")
	moveFileExW     = kernel32FileOps.NewProc("MoveFileExW")
	replaceFileW    = kernel32FileOps.NewProc("ReplaceFileW")
)

func win32CallError(callErr error) error {
	if callErr != nil && callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}
func replaceConfigFile(source, destination string) (bool, error) {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return false, err
	}
	to, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return false, err
	}
	if _, statErr := os.Stat(destination); statErr == nil {
		// ReplaceFileW preserves the replaced file's DACL. Harden the existing
		// destination before publication so the replacement inherits a protected
		// owner-private ACL rather than carrying forward a permissive legacy ACL.
		if err := ensureConfigOwnerPrivate(destination); err != nil {
			return false, fmt.Errorf("secure existing configuration before replacement: %w", err)
		}
		result, _, callErr := replaceFileW.Call(uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(from)), 0, 0, 0, 0)
		if result == 0 {
			return false, win32CallError(callErr)
		}
		if err := syncPublishedConfigFile(destination); err != nil {
			return true, err
		}
		return true, nil
	} else if !os.IsNotExist(statErr) {
		return false, statErr
	}
	result, _, callErr := moveFileExW.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), uintptr(moveFileWriteThrough))
	if result == 0 {
		return false, win32CallError(callErr)
	}
	if err := syncPublishedConfigFile(destination); err != nil {
		return true, err
	}
	return true, nil
}
func syncPublishedConfigFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func syncConfigDirectory(string) error { return nil }
