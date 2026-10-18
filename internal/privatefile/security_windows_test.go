//go:build windows

package privatefile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

func TestCreateTempAndEnsureExistingAreOwnerPrivateWindows(t *testing.T) {
	dir := t.TempDir()
	tmp, err := CreateTemp(dir, "state.json", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpPath)
	ok, err := InspectOwnerPrivate(tmpPath, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("new private temp does not have the expected protected current-user-only DACL")
	}
	assertPrivateWindowsDACL(t, tmpPath)

	legacy := filepath.Join(dir, "legacy-state.json")
	if err := os.WriteFile(legacy, []byte("secret"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnerPrivate(legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = InspectOwnerPrivate(legacy, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("existing file was not hardened to the expected protected current-user-only DACL")
	}
	assertPrivateWindowsDACL(t, legacy)
}

func TestEnsureOwnerPrivateDoesNotChangeFilesystemOwnerWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownerBefore, _, descriptorBefore, err := namedSecurity(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerBeforeText, err := sidString(ownerBefore)
	localFree.Call(descriptorBefore)
	if err != nil {
		t.Fatal(err)
	}

	if err := EnsureOwnerPrivate(path, 0o600); err != nil {
		t.Fatal(err)
	}

	ownerAfter, _, descriptorAfter, err := namedSecurity(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerAfterText, err := sidString(ownerAfter)
	localFree.Call(descriptorAfter)
	if err != nil {
		t.Fatal(err)
	}
	if ownerAfterText != ownerBeforeText {
		t.Fatalf("EnsureOwnerPrivate changed owner: before=%s after=%s", ownerBeforeText, ownerAfterText)
	}
}

func assertPrivateWindowsDACL(t *testing.T, path string) {
	t.Helper()
	_, dacl, descriptor, err := namedSecurity(path)
	if err != nil {
		t.Fatal(err)
	}
	defer localFree.Call(descriptor)
	protected, err := descriptorHasProtectedDACL(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if !protected {
		t.Fatal("private-file DACL is not protected")
	}
	aces, sids, err := allowedACEs(dacl)
	if err != nil {
		t.Fatal(err)
	}
	if len(aces) != 2 {
		t.Fatalf("private-file DACL has %d ACEs, want exactly 2", len(aces))
	}
	userSID, err := currentUserSIDString()
	if err != nil {
		t.Fatal(err)
	}
	const ownerRightsSID = "S-1-3-4"
	var userFound, ownerRightsFound bool
	for i, ace := range aces {
		sid, err := sidString(sids[i])
		if err != nil {
			t.Fatal(err)
		}
		switch sid {
		case userSID:
			if userFound {
				t.Fatal("private-file DACL contains duplicate TokenUser ACEs")
			}
			userFound = true
			if ace.Mask != genericAll && ace.Mask != fileAllAccess {
				t.Fatalf("TokenUser mask=%#x, want GENERIC_ALL or FILE_ALL_ACCESS", ace.Mask)
			}
		case ownerRightsSID:
			if ownerRightsFound {
				t.Fatal("private-file DACL contains duplicate OWNER_RIGHTS ACEs")
			}
			ownerRightsFound = true
			if ace.Mask != readControl {
				t.Fatalf("OWNER_RIGHTS mask=%#x, want READ_CONTROL %#x", ace.Mask, readControl)
			}
			if ace.Mask&(0x00040000|0x00080000) != 0 { // WRITE_DAC | WRITE_OWNER
				t.Fatalf("OWNER_RIGHTS unexpectedly retains DACL/owner write rights: %#x", ace.Mask)
			}
		default:
			t.Fatalf("private-file DACL contains unexpected SID %s", sid)
		}
		if ace.Header.AceFlags != 0 {
			t.Fatalf("private-file ACE for %s has flags %#x, want 0", sid, ace.Header.AceFlags)
		}
	}
	if !userFound || !ownerRightsFound {
		t.Fatalf("private-file DACL missing required ACEs: TokenUser=%v OWNER_RIGHTS=%v", userFound, ownerRightsFound)
	}
}

func installLegacyTokenUserOnlyDACL(t *testing.T, path string) {
	t.Helper()
	userSID, err := currentUserSIDString()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := securityDescriptorFromSDDL("D:P(A;;GA;;;" + userSID + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer localFree.Call(descriptor)
	dacl, err := descriptorDACL(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	result, _, _ := setNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(seFileObject),
		uintptr(daclSecurityInformation|protectedDaclSecurityInformation),
		0,
		0,
		dacl,
		0,
	)
	if result != 0 {
		t.Fatal(syscall.Errno(result))
	}
}

func TestEnsureOwnerPrivateMigratesTokenUserOnlyPolicyWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-one-ace.json")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	installLegacyTokenUserOnlyDACL(t, path)
	ok, err := InspectOwnerPrivate(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("legacy one-ACE TokenUser DACL was incorrectly accepted as current private-file policy")
	}
	if err := EnsureOwnerPrivate(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = InspectOwnerPrivate(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("legacy one-ACE TokenUser DACL was not migrated to OWNER_RIGHTS policy")
	}
	assertPrivateWindowsDACL(t, path)
}
