package proxy

import (
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newExternalIdpTokenServer stands up a fake IdP token endpoint that answers the
// refresh_token grant with a snake_case OAuth2 token response, mirroring Azure AD.
func newExternalIdpTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-watch","refresh_token":"rt-watch-rotated","expires_in":3600}`)
	}))
}

// bypassExternalIdpValidator installs a no-op IdP endpoint validator for the test.
// h.importCredential allow-lists external_idp token endpoints, and these tests
// point at an httptest server (http + 127.0.0.1) that the real validator
// rejects by design.
func bypassExternalIdpValidator(t *testing.T) {
	t.Helper()
	restore := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	t.Cleanup(func() { auth.SetExternalIdpValidatorForTest(restore) })
}

// backdateForWatcher rewinds a fixture's mtime past importMinFileAgeSeconds so
// scanImportDir's "file still being written" guard (proxy/import_watcher.go) does
// not skip it. Tests write and scan in the same instant, so without this every
// fixture is ~0s old and always deferred to a later poll that never happens.
func backdateForWatcher(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-(importMinFileAgeSeconds + 1) * time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate %s: %v", path, err)
	}
}

// writeHelperFile writes a helper-style external_idp credential JSON into dir and
// backdates its mtime so the watcher's "still being written" guard does not skip it.
func writeHelperFile(t *testing.T, dir, name, tokenEndpoint, refreshToken, email string) {
	t.Helper()
	body := fmt.Sprintf(`{
	  "auth_method": "external_idp",
	  "client_id": "azure-client",
	  "refresh_token": %q,
	  "region": "us-east-1",
	  "token_endpoint": %q,
	  "issuer_url": "https://login.microsoftonline.com/tenant/v2.0",
	  "scopes": "offline_access",
	  "email": %q,
	  "type": "kiro"
	}`, refreshToken, tokenEndpoint, email)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write helper file: %v", err)
	}
	backdateForWatcher(t, path)
}

// TestImportWatcherProcessesValidFile verifies a valid drop is imported and the
// file is moved into processed/.
func TestImportWatcherProcessesValidFile(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	bypassExternalIdpValidator(t)
	idp := newExternalIdpTokenServer(t)
	defer idp.Close()

	dir := t.TempDir()
	writeHelperFile(t, dir, "CLIProxyAPI_user.json", idp.URL, "rt-watch", "user@example.com")

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account imported, got %d", len(accs))
	}
	if accs[0].AuthMethod != "external_idp" {
		t.Fatalf("expected external_idp, got %q", accs[0].AuthMethod)
	}

	// File must have moved into processed/ and be gone from the root.
	if _, err := os.Stat(filepath.Join(dir, "CLIProxyAPI_user.json")); !os.IsNotExist(err) {
		t.Fatalf("expected source file removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "processed", "CLIProxyAPI_user.json")); err != nil {
		t.Fatalf("expected file in processed/, got %v", err)
	}
}

// TestImportWatcherMovesInvalidFileToFailed verifies a malformed file is moved to
// failed/ with an .error.txt sidecar and no account is created.
func TestImportWatcherMovesInvalidFileToFailed(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("write broken file: %v", err)
	}
	backdateForWatcher(t, path)

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no accounts, got %d", len(accs))
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", "broken.json")); err != nil {
		t.Fatalf("expected file in failed/, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", "broken.json"+importErrorSidecarSuffix)); err != nil {
		t.Fatalf("expected .error.txt sidecar, got %v", err)
	}
}

// TestImportWatcherTruncatedFileRoutesToFailed is FIX3's end-to-end regression
// test: a drop-folder file with a valid first object and a malformed second
// object must land in failed/ (with a sidecar naming the break point), not
// processed/ — even though the first object DID import successfully. Before
// the fix, decodeConcatenatedObjects silently discarded the truncation signal,
// importedAny was true, and the whole file was moved to processed/ as if every
// credential in it had been handled.
func TestImportWatcherTruncatedFileRoutesToFailed(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	bypassExternalIdpValidator(t)
	idp := newExternalIdpTokenServer(t)
	defer idp.Close()

	dir := t.TempDir()
	// A valid external_idp object followed by a malformed second object —
	// decodeConcatenatedObjects parses the first, then stops.
	body := fmt.Sprintf(`{"auth_method":"external_idp","client_id":"azure-client","refresh_token":"rt-trunc","token_endpoint":%q,"issuer_url":"https://login.microsoftonline.com/tenant/v2.0","scopes":"offline_access","email":"trunc@example.com","type":"kiro"}

{this is not valid json`, idp.URL)
	path := filepath.Join(dir, "truncated.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write truncated file: %v", err)
	}
	backdateForWatcher(t, path)

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	// The first credential DID import — that part of the fix must not regress
	// (a truncated file still gets what it could out of the valid prefix).
	if accs := config.GetAccounts(); len(accs) != 1 {
		t.Fatalf("expected the one valid object to still import, got %d accounts", len(accs))
	}

	// But the FILE must be in failed/, not processed/ — a truncated drop is not
	// "fully handled" just because its first object happened to parse.
	if _, err := os.Stat(filepath.Join(dir, "processed", "truncated.json")); !os.IsNotExist(err) {
		t.Fatalf("truncated file must NOT be in processed/, stat err=%v", err)
	}
	sidecar := filepath.Join(dir, "failed", "truncated.json"+importErrorSidecarSuffix)
	contents, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("expected file in failed/ with a sidecar, got %v", err)
	}
	if !strings.Contains(string(contents), "object 2") {
		t.Fatalf("sidecar should name the break point, got %q", contents)
	}
}

// TestImportWatcherSurfacesRotatedRefreshTokenOnPersistFailure is FIX5's
// regression test for the drop-folder path: when the IdP rotates the refresh
// token during h.importCredential's mandatory refresh AND THEN
// config.AddAccount fails to persist, the original token is already dead at
// the IdP — the sidecar/log must surface the rotated value so an operator can
// recover the credential without a full interactive re-login, instead of the
// rotated token silently disappearing.
func TestImportWatcherSurfacesRotatedRefreshTokenOnPersistFailure(t *testing.T) {
	cfgPath := t.TempDir() + "/config.json"
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()
	bypassExternalIdpValidator(t)

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"at-rotate","refresh_token":"rotated-watcher","expires_in":3600}`)
	}))
	defer idp.Close()

	dir := t.TempDir()
	writeHelperFile(t, dir, "rotate.json", idp.URL, "orig-watcher", "rotate@example.com")

	// Sabotage the config file AFTER Init (and after the fixture is written) so
	// config.AddAccount's Save fails deterministically.
	if err := os.Remove(cfgPath); err != nil {
		t.Fatalf("remove config file: %v", err)
	}
	if err := os.Mkdir(cfgPath, 0o700); err != nil {
		t.Fatalf("replace config file with directory: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	sidecar := filepath.Join(dir, "failed", "rotate.json"+importErrorSidecarSuffix)
	contents, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("expected file in failed/ with a sidecar, got %v", err)
	}
	if !strings.Contains(string(contents), "rotated-watcher") {
		t.Fatalf("sidecar = %q, want it to surface the rotated refresh token rotated-watcher", contents)
	}
}

// TestMoveImportFileChmodsCredentialTo0600 is FIX6's regression test: a
// dropped credential file's ORIGINAL mode (e.g. 0644, as a host bind mount
// might deliver) must not survive the move into processed/ or failed/ — the
// file holds a refresh token or API key and must land at 0600, matching the
// sidecar's mode.
func TestMoveImportFileChmodsCredentialTo0600(t *testing.T) {
	dir := t.TempDir()
	name := "loose-mode.json"
	src := filepath.Join(dir, name)
	if err := os.WriteFile(src, []byte(`{"refreshToken":"rt"}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	h := &Handler{}
	h.moveImportFile(dir, name, importProcessedSubdir, "")

	dest := filepath.Join(dir, importProcessedSubdir, name)
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("expected moved file at %s, got %v", dest, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("moved file mode = %o, want 0600", got)
	}
}

// TestMoveImportFileTruncatesLongErrorMessage is part of FIX6: an
// attacker-influenced upstream error (e.g. auth/oidc.go's verbatim, unbounded
// external IdP token-exchange response body) must not be written to the
// sidecar/log without a bound.
func TestMoveImportFileTruncatesLongErrorMessage(t *testing.T) {
	dir := t.TempDir()
	name := "huge-error.json"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	huge := strings.Repeat("x", maxImportErrorMessageBytes*2)
	h := &Handler{}
	h.moveImportFile(dir, name, importFailedSubdir, huge)

	sidecar := filepath.Join(dir, importFailedSubdir, name+importErrorSidecarSuffix)
	contents, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if len(contents) >= len(huge) {
		t.Fatalf("sidecar was not truncated: got %d bytes, original was %d", len(contents), len(huge))
	}
	if !strings.Contains(string(contents), "truncated") {
		t.Fatalf("sidecar should mark itself truncated, got %d bytes with no marker", len(contents))
	}
}

// TestDuplicateAccountReasonMatchesApiKeyAccount is FIX8's regression test:
// a dropped file whose raw authMethod is an explicit api_key alias must be
// recognized as a duplicate of a persisted api_key account with the same
// email — before the fix, normalizeImportAuthMethod had no api_key case, so
// bucketedMethod fell through to "social" and never matched the persisted
// account's canonical "api_key" AuthMethod.
func TestDuplicateAccountReasonMatchesApiKeyAccount(t *testing.T) {
	existing := []config.Account{{
		Email:      "keyholder@example.com",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_existing",
	}}
	req := importCredentialRequest{
		Email:      "keyholder@example.com",
		AuthMethod: "kiro_api_key", // raw alias, as an export might spell it
	}
	if reason := duplicateAccountReason(existing, req); reason == "" {
		t.Fatal("expected a duplicate reason for an api_key blob matching an existing api_key account's email, got none")
	}
}

// TestImportWatcherSkipsDuplicate verifies a credential whose refresh token already
// exists is skipped (no duplicate account), and the file is still cleared to processed/.
func TestImportWatcherSkipsDuplicate(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	// Seed an existing account whose refresh token matches the drop.
	if err := config.AddAccount(config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        "dup@example.com",
		AuthMethod:   "external_idp",
		RefreshToken: "rt-dup",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	bypassExternalIdpValidator(t)
	idp := newExternalIdpTokenServer(t)
	defer idp.Close()

	dir := t.TempDir()
	writeHelperFile(t, dir, "dup.json", idp.URL, "rt-dup", "dup@example.com")

	h := &Handler{pool: accountpool.GetPool()}
	h.scanImportDir(dir)

	// Still exactly one account — the duplicate was skipped, not added.
	if accs := config.GetAccounts(); len(accs) != 1 {
		t.Fatalf("expected duplicate skipped (1 account), got %d", len(accs))
	}
	// An all-duplicate file is cleared to processed/ so it is not retried forever.
	if _, err := os.Stat(filepath.Join(dir, "processed", "dup.json")); err != nil {
		t.Fatalf("expected duplicate-only file moved to processed/, got %v", err)
	}
}
