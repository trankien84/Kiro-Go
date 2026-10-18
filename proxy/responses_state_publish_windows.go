//go:build windows

package proxy

import (
	"os"
	"syscall"
	"unsafe"
)

const responseMoveFileWriteThrough = 0x00000008

var (
	kernel32ResponseState = syscall.NewLazyDLL("kernel32.dll")
	moveFileExResponseW   = kernel32ResponseState.NewProc("MoveFileExW")
)

func publishResponseStateNoReplace(source, destination string) (bool, error) {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return false, err
	}
	to, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return false, err
	}
	result, _, callErr := moveFileExResponseW.Call(
		uintptr(unsafe.Pointer(from)),
		uintptr(unsafe.Pointer(to)),
		uintptr(responseMoveFileWriteThrough),
	)
	if result != 0 {
		return true, nil
	}
	if callErr == syscall.ERROR_FILE_EXISTS || callErr == syscall.ERROR_ALREADY_EXISTS {
		return false, os.ErrExist
	}
	if callErr != nil && callErr != syscall.Errno(0) {
		return false, callErr
	}
	return false, syscall.EINVAL
}

func syncResponseStateDirectory(string) error { return nil }
