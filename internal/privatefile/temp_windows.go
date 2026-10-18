//go:build windows

package privatefile

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	genericRead       = 0x80000000
	genericWrite      = 0x40000000
	createNew         = 1
	fileAttributeNorm = 0x00000080
	sddlRevision1     = 1
	tempCreateRetries = 64
)

type securityAttributes struct {
	Length             uint32
	SecurityDescriptor uintptr
	InheritHandle      int32
}

var (
	advapi32Security = syscall.NewLazyDLL("advapi32.dll")
	kernel32Create   = syscall.NewLazyDLL("kernel32.dll")

	convertStringSecurityDescriptorToSecurityDescriptorW = advapi32Security.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	createFileW                                          = kernel32Create.NewProc("CreateFileW")
	localFree                                            = kernel32Create.NewProc("LocalFree")
)

func win32CallError(callErr error) error {
	if callErr != nil && callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}

func securityDescriptorFromSDDL(value string) (uintptr, error) {
	sddl, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return 0, err
	}
	var descriptor uintptr
	result, _, callErr := convertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddl)),
		uintptr(sddlRevision1),
		uintptr(unsafe.Pointer(&descriptor)),
		0,
	)
	if result == 0 {
		return 0, win32CallError(callErr)
	}
	return descriptor, nil
}

func privateSecurityDescriptor() (uintptr, error) {
	// TokenUser is stable across normal and elevated tokens for the same Windows
	// account. OWNER_RIGHTS (S-1-3-4) is intentionally present with READ_CONTROL
	// only: its presence suppresses the filesystem owner's implicit READ_CONTROL
	// and WRITE_DAC, while the explicit TokenUser ACE retains full access.
	userSID, err := currentUserSIDString()
	if err != nil {
		return 0, err
	}
	const ownerRightsSID = "S-1-3-4"
	return securityDescriptorFromSDDL(
		"D:P(A;;GA;;;" + userSID + ")(A;;RC;;;" + ownerRightsSID + ")",
	)
}

func randomTempName(dir, base string) (string, error) {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, "."+filepath.Base(base)+".tmp-"+hex.EncodeToString(suffix[:])), nil
}

// CreateTemp creates a unique same-directory file protected by the private-file
// DACL: TokenUser has full access and OWNER_RIGHTS has READ_CONTROL only. Windows
// mode bits do not establish ACLs, so perm is retained only for API symmetry
// with Unix callers.
func CreateTemp(dir, base string, perm os.FileMode) (*os.File, error) {
	_ = perm

	descriptor, err := privateSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("create current-user-private temp security descriptor: %w", err)
	}
	defer localFree.Call(descriptor)

	security := securityAttributes{
		Length:             uint32(unsafe.Sizeof(securityAttributes{})),
		SecurityDescriptor: descriptor,
		InheritHandle:      0,
	}

	for attempt := 0; attempt < tempCreateRetries; attempt++ {
		name, err := randomTempName(dir, base)
		if err != nil {
			return nil, err
		}
		nameW, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}

		handle, _, callErr := createFileW.Call(
			uintptr(unsafe.Pointer(nameW)),
			uintptr(genericRead|genericWrite),
			0,
			uintptr(unsafe.Pointer(&security)),
			uintptr(createNew),
			uintptr(fileAttributeNorm),
			0,
		)
		if handle == ^uintptr(0) {
			if callErr == syscall.ERROR_FILE_EXISTS || callErr == syscall.ERROR_ALREADY_EXISTS {
				continue
			}
			return nil, win32CallError(callErr)
		}

		file := os.NewFile(handle, name)
		if file == nil {
			syscall.CloseHandle(syscall.Handle(handle))
			return nil, fmt.Errorf("create current-user-private temp file: os.NewFile returned nil")
		}
		return file, nil
	}

	return nil, fmt.Errorf("create current-user-private temp file: exhausted %d unique-name attempts", tempCreateRetries)
}
