//go:build windows

package privatefile

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	seFileObject                     = 1
	ownerSecurityInformation         = 0x00000001
	daclSecurityInformation          = 0x00000004
	protectedDaclSecurityInformation = 0x80000000
	seDaclProtected                  = 0x1000
	tokenQuery                       = 0x0008
	tokenUserInformationClass        = 1
	fileAllAccess                    = 0x001F01FF
	genericAll                       = 0x10000000
	readControl                      = 0x00020000
)

var (
	getNamedSecurityInfoW        = advapi32Security.NewProc("GetNamedSecurityInfoW")
	setNamedSecurityInfoW        = advapi32Security.NewProc("SetNamedSecurityInfoW")
	getSecurityDescriptorDacl    = advapi32Security.NewProc("GetSecurityDescriptorDacl")
	getSecurityDescriptorControl = advapi32Security.NewProc("GetSecurityDescriptorControl")
	openProcessToken             = advapi32Security.NewProc("OpenProcessToken")
	getTokenInformation          = advapi32Security.NewProc("GetTokenInformation")
	convertSidToStringSidW       = advapi32Security.NewProc("ConvertSidToStringSidW")
	equalSid                     = advapi32Security.NewProc("EqualSid")
	getCurrentProcess            = kernel32Create.NewProc("GetCurrentProcess")
	getAce                       = advapi32Security.NewProc("GetAce")
)

type sidAndAttributes struct {
	Sid        uintptr
	Attributes uint32
}

type tokenUser struct {
	User sidAndAttributes
}

func namedSecurity(path string) (owner, dacl, descriptor uintptr, err error) {
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, err
	}
	result, _, _ := getNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(seFileObject),
		uintptr(ownerSecurityInformation|daclSecurityInformation),
		uintptr(unsafe.Pointer(&owner)),
		0,
		uintptr(unsafe.Pointer(&dacl)),
		0,
		uintptr(unsafe.Pointer(&descriptor)),
	)
	if result != 0 {
		return 0, 0, 0, syscall.Errno(result)
	}
	return owner, dacl, descriptor, nil
}

func descriptorDACL(descriptor uintptr) (uintptr, error) {
	var present int32
	var defaulted int32
	var dacl uintptr
	result, _, callErr := getSecurityDescriptorDacl.Call(
		descriptor,
		uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&defaulted)),
	)
	if result == 0 {
		return 0, win32CallError(callErr)
	}
	if present == 0 || dacl == 0 {
		return 0, fmt.Errorf("security descriptor has no DACL")
	}
	return dacl, nil
}

type aceHeader struct {
	AceType  uint8
	AceFlags uint8
	AceSize  uint16
}

type accessAllowedACE struct {
	Header   aceHeader
	Mask     uint32
	SidStart uint32
}

func allowedACEs(dacl uintptr) ([]*accessAllowedACE, []uintptr, error) {
	if dacl == 0 {
		return nil, nil, fmt.Errorf("nil DACL")
	}
	// ACL.AceCount is the uint16 at byte offset 4 in the Win32 ACL header.
	aceCount := *(*uint16)(unsafe.Pointer(dacl + 4))
	aces := make([]*accessAllowedACE, 0, int(aceCount))
	sids := make([]uintptr, 0, int(aceCount))
	for i := uint16(0); i < aceCount; i++ {
		var acePtr uintptr
		result, _, callErr := getAce.Call(
			dacl,
			uintptr(i),
			uintptr(unsafe.Pointer(&acePtr)),
		)
		if result == 0 {
			return nil, nil, win32CallError(callErr)
		}
		ace := (*accessAllowedACE)(unsafe.Pointer(acePtr))
		if ace.Header.AceType != 0 { // ACCESS_ALLOWED_ACE_TYPE
			return nil, nil, fmt.Errorf(
				"current-user-private DACL ACE %d type=%d, want access-allowed",
				i,
				ace.Header.AceType,
			)
		}
		aces = append(aces, ace)
		sids = append(sids, acePtr+unsafe.Offsetof(accessAllowedACE{}.SidStart))
	}
	return aces, sids, nil
}

func privateAccessMaskEquivalent(actual, desired uint32) bool {
	if actual == desired {
		return true
	}
	// Some filesystem/security APIs map GENERIC_ALL to FILE_ALL_ACCESS when
	// materializing an ACL. This mapping is acceptable only for the TokenUser
	// full-access ACE. OWNER_RIGHTS is deliberately READ_CONTROL-only and must
	// continue to match exactly so filesystem ownership never retains WRITE_DAC.
	return desired == genericAll && actual == fileAllAccess
}

func equivalentPrivateDACL(actual, desired uintptr) (bool, error) {
	actualACEs, actualSIDs, err := allowedACEs(actual)
	if err != nil {
		return false, err
	}
	desiredACEs, desiredSIDs, err := allowedACEs(desired)
	if err != nil {
		return false, err
	}
	if len(actualACEs) != len(desiredACEs) {
		return false, nil
	}
	matched := make([]bool, len(actualACEs))
	for desiredIndex, desiredACE := range desiredACEs {
		found := false
		for actualIndex, actualACE := range actualACEs {
			if matched[actualIndex] {
				continue
			}
			equal, _, _ := equalSid.Call(actualSIDs[actualIndex], desiredSIDs[desiredIndex])
			if equal == 0 {
				continue
			}
			if actualACE.Header.AceFlags != desiredACE.Header.AceFlags {
				continue
			}
			if !privateAccessMaskEquivalent(actualACE.Mask, desiredACE.Mask) {
				continue
			}
			matched[actualIndex] = true
			found = true
			break
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

func descriptorHasProtectedDACL(descriptor uintptr) (bool, error) {
	var control uint16
	var revision uint32
	result, _, callErr := getSecurityDescriptorControl.Call(
		descriptor,
		uintptr(unsafe.Pointer(&control)),
		uintptr(unsafe.Pointer(&revision)),
	)
	if result == 0 {
		return false, win32CallError(callErr)
	}
	return control&seDaclProtected != 0, nil
}

func currentUserSID() (uintptr, []byte, error) {
	process, _, _ := getCurrentProcess.Call()
	var token syscall.Handle
	result, _, callErr := openProcessToken.Call(
		process,
		uintptr(tokenQuery),
		uintptr(unsafe.Pointer(&token)),
	)
	if result == 0 {
		return 0, nil, win32CallError(callErr)
	}
	defer syscall.CloseHandle(token)

	var needed uint32
	result, _, callErr = getTokenInformation.Call(
		uintptr(token),
		uintptr(tokenUserInformationClass),
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
	)
	if result == 0 && callErr != syscall.ERROR_INSUFFICIENT_BUFFER {
		return 0, nil, win32CallError(callErr)
	}
	if needed == 0 {
		return 0, nil, fmt.Errorf("query process token user returned an empty buffer size")
	}
	buf := make([]byte, needed)
	result, _, callErr = getTokenInformation.Call(
		uintptr(token),
		uintptr(tokenUserInformationClass),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&needed)),
	)
	if result == 0 {
		return 0, nil, win32CallError(callErr)
	}
	sid := (*tokenUser)(unsafe.Pointer(&buf[0])).User.Sid
	if sid == 0 {
		return 0, nil, fmt.Errorf("process token has no user SID")
	}
	return sid, buf, nil
}

func sidString(sid uintptr) (string, error) {
	if sid == 0 {
		return "", fmt.Errorf("nil SID")
	}
	var value uintptr
	result, _, callErr := convertSidToStringSidW.Call(
		sid,
		uintptr(unsafe.Pointer(&value)),
	)
	if result == 0 {
		return "", win32CallError(callErr)
	}
	defer localFree.Call(value)
	var units []uint16
	for offset := uintptr(0); ; offset += 2 {
		unit := *(*uint16)(unsafe.Pointer(value + offset))
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return syscall.UTF16ToString(units), nil
}

func currentUserSIDString() (string, error) {
	sid, buf, err := currentUserSID()
	if err != nil {
		return "", err
	}
	value, err := sidString(sid)
	// Keep the GetTokenInformation buffer alive until sidString has consumed the
	// SID pointer stored inside it.
	runtime.KeepAlive(buf)
	return value, err
}

// InspectOwnerPrivate reports whether path has the protected private-file DACL:
// TokenUser receives full access and OWNER_RIGHTS receives READ_CONTROL only.
// The OWNER_RIGHTS ACE suppresses the filesystem owner's implicit WRITE_DAC,
// so filesystem ownership can remain unchanged without granting a second
// principal the ability to rewrite the DACL.
func InspectOwnerPrivate(path string, perm os.FileMode) (bool, error) {
	_ = perm
	_, dacl, descriptor, err := namedSecurity(path)
	if err != nil {
		return false, err
	}
	defer localFree.Call(descriptor)

	protected, err := descriptorHasProtectedDACL(descriptor)
	if err != nil || !protected {
		return false, err
	}

	desiredDescriptor, err := privateSecurityDescriptor()
	if err != nil {
		return false, err
	}
	defer localFree.Call(desiredDescriptor)
	desiredDACL, err := descriptorDACL(desiredDescriptor)
	if err != nil {
		return false, err
	}
	return equivalentPrivateDACL(dacl, desiredDACL)
}

// EnsureOwnerPrivate hardens an existing sensitive file without changing its
// filesystem owner. TokenUser receives full access while OWNER_RIGHTS receives
// READ_CONTROL only; the OWNER_RIGHTS ACE suppresses the owner's otherwise
// implicit WRITE_DAC. SetNamedSecurityInfo still requires the caller to be
// authorized to change the existing DACL; this function never requests
// WRITE_OWNER or changes OWNER_SECURITY_INFORMATION.
func EnsureOwnerPrivate(path string, perm os.FileMode) error {
	if ok, err := InspectOwnerPrivate(path, perm); err == nil && ok {
		return nil
	}
	desiredDescriptor, err := privateSecurityDescriptor()
	if err != nil {
		return fmt.Errorf("create current-user-private security descriptor: %w", err)
	}
	defer localFree.Call(desiredDescriptor)
	desiredDACL, err := descriptorDACL(desiredDescriptor)
	if err != nil {
		return err
	}
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	result, _, _ := setNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(seFileObject),
		uintptr(daclSecurityInformation|protectedDaclSecurityInformation),
		0,
		0,
		desiredDACL,
		0,
	)
	if result != 0 {
		return syscall.Errno(result)
	}
	ok, err := InspectOwnerPrivate(path, perm)
	if err != nil {
		return fmt.Errorf("verify current-user-private security: %w", err)
	}
	if !ok {
		return fmt.Errorf("current-user-private DACL verification failed")
	}
	return nil
}
