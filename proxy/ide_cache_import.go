package proxy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxIdeCacheFileBytes bounds how much of the IDE cache file readIdeCacheCredential
// will read, matching the 1 MiB cap every HTTP import route applies via
// http.MaxBytesReader. Without a cap here, a KIRO_IDE_CACHE (or the default
// path, if an operator's HOME resolves somewhere attacker-influenced) pointed
// at an unbounded source — a device file, a fifo, a huge log — would be read
// into memory in full (see FIX4).
const maxIdeCacheFileBytes = 1 << 20

// IDE-cache import: turn the credential the Kiro IDE already minted on this host
// into a Kiro-Go account, with no browser sign-in. The Kiro IDE caches its SSO
// credential at ~/.aws/sso/cache/kiro-auth-token.json as a flat camelCase object
// (accessToken, refreshToken, authMethod, and — for Microsoft 365 / Entra ID —
// tokenEndpoint, issuerUrl, clientId, scopes). Those are exactly the keys
// decodeImportRequest already understands, so the file maps straight onto an
// importCredentialRequest and flows through the same h.importCredential core
// every other import path uses. The IDE's ISO expiresAt is intentionally
// ignored: h.importCredential performs a mandatory refresh for anything that
// isn't a trust-on-import external_idp access token, so the persisted expiry
// comes from a fresh upstream response rather than a stale cached timestamp.

// defaultIdeCacheRelPath is the IDE credential cache location relative to $HOME.
const defaultIdeCacheRelPath = ".aws/sso/cache/kiro-auth-token.json"

// ideCachePath resolves the Kiro IDE credential cache path. An explicit argument
// wins; then the KIRO_IDE_CACHE env var; then ~/.aws/sso/cache/kiro-auth-token.json.
// Inside Docker the host file must be mounted and KIRO_IDE_CACHE pointed at it.
func ideCachePath(explicit string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	if env := strings.TrimSpace(os.Getenv("KIRO_IDE_CACHE")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		// Fall back to a literal ~ expansion miss; return the rel path so the
		// caller's os.ReadFile produces a clear "no such file" error.
		return defaultIdeCacheRelPath
	}
	return filepath.Join(home, defaultIdeCacheRelPath)
}

// readIdeCacheCredential reads and normalizes the Kiro IDE credential cache into
// an importCredentialRequest. It returns an actionable error when the file is
// missing, unreadable, malformed, or lacks the refresh material a refreshable
// credential needs (the proxy refreshes against refreshToken on every import).
func readIdeCacheCredential(path string) (importCredentialRequest, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return importCredentialRequest{}, &importValidationError{
				"Kiro IDE cache not found at " + path + " — sign in once with the Kiro IDE, " +
					"or set KIRO_IDE_CACHE to the credential file path. In Docker, mount the host " +
					"AWS SSO cache directory with KIRO_AWS_SSO_CACHE_DIR.",
			}
		}
		return importCredentialRequest{}, &importValidationError{
			"cannot read Kiro IDE cache " + path + ": " + err.Error(),
		}
	}
	defer f.Close()
	// Bounded read (maxIdeCacheFileBytes): a device file, fifo, or huge log at
	// this path must not be read into memory in full (see FIX4). Read one byte
	// past the cap so an over-limit file is detected rather than silently
	// truncated into a confusing "invalid JSON" error.
	raw, err := io.ReadAll(io.LimitReader(f, maxIdeCacheFileBytes+1))
	if err != nil {
		return importCredentialRequest{}, &importValidationError{
			"cannot read Kiro IDE cache " + path + ": " + err.Error(),
		}
	}
	if len(raw) > maxIdeCacheFileBytes {
		return importCredentialRequest{}, &importValidationError{
			fmt.Sprintf("Kiro IDE cache %s exceeds the %d byte limit", path, maxIdeCacheFileBytes),
		}
	}

	// The IDE cache is a single flat camelCase object; decodeImportRequest already
	// accepts that casing (and snake_case), so reuse it verbatim. AuthMethod comes
	// through raw/unbucketed, same as every other decodeImportRequest caller, so
	// h.importCredential's classifier sees whatever the IDE actually wrote.
	req, err := decodeImportRequest(raw)
	if err != nil {
		return importCredentialRequest{}, &importValidationError{
			"Kiro IDE cache " + path + " is not valid JSON: " + err.Error(),
		}
	}

	if strings.TrimSpace(req.RefreshToken) == "" {
		return importCredentialRequest{}, &importValidationError{
			"Kiro IDE cache " + path + " has no refreshToken — re-open the Kiro IDE to " +
				"repopulate it (a short-lived access token alone cannot be refreshed)",
		}
	}
	// external_idp credentials need tokenEndpoint+clientId to refresh; surface a
	// clear message here rather than letting the refresh fail opaquely later.
	// AuthMethod is still raw at this point (not yet bucketed by
	// h.importCredential), so also accept the common Microsoft aliases here —
	// this is a friendly pre-check only, not the actual classification.
	am := strings.ToLower(strings.TrimSpace(req.AuthMethod))
	looksExternalIdp := am == "external_idp" || am == "azuread" || am == "azure" || am == "entra" || am == "m365"
	if looksExternalIdp &&
		(strings.TrimSpace(req.TokenEndpoint) == "" || strings.TrimSpace(req.ClientID) == "") {
		return importCredentialRequest{}, &importValidationError{
			"Kiro IDE cache " + path + " looks like external_idp but is missing " +
				"tokenEndpoint/clientId; cannot build a refreshable credential",
		}
	}
	return req, nil
}

// describeIdeCacheImport returns a short, log-friendly summary of what an IDE
// cache import produced (used by both the API response and the watcher log).
func describeIdeCacheImport(path string, authMethod string) string {
	return fmt.Sprintf("imported %s credential from Kiro IDE cache %s", authMethod, path)
}
