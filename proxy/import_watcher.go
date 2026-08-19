package proxy

import (
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Import watcher: a dependency-free, opt-in directory poller that auto-ingests
// CLIProxyAPI_*.json credentials produced by kiro-login-helper.py. It is gated
// behind KIRO_IMPORT_WATCH so a bare local build never mutates files in the
// background; docker-compose.yml sets the flag so `docker compose up -d` gives a
// zero-touch drop-folder. Every import goes through h.importCredential ->
// config.AddAccount (the same persisted path the live server owns, and the same
// classifier apiImportCredentials uses), so the watcher can never race the
// in-memory config and never diverges on Microsoft-vs-generic classification.
//
// This loop does NOT only POST to the credential's own IdP token endpoint —
// that was true of an earlier, OAuth-only version of h.importCredential but is
// no longer accurate: social/idc blobs still go through auth.RefreshToken
// (AWS/Kiro OIDC), but api_key blobs hit resolveApiKeyRegion -> probeKiroApiKey
// (a real Kiro endpoint) plus a background fetchAndCacheAccountModels call. The
// conclusion below still holds, just not for the reason originally stated:
// imported accounts are never Bedrock (Bedrock accounts are added through a
// separate admin route, never through this credential-import path), and none
// of those probe failures feed handleAccountFailure, so this loop needs no
// IsBedrock() guard (unlike a loop that walks config.GetAccounts() to talk to
// existing Kiro accounts). duplicateAccountReason below does read
// config.GetAccounts(), but only in memory for a local dedupe comparison — no
// request is sent for it.

const (
	defaultImportWatchDir    = "data/imports"
	importWatchInterval      = 15 * time.Second
	importMinFileAgeSeconds  = 2 // skip files still being written / copied
	importProcessedSubdir    = "processed"
	importFailedSubdir       = "failed"
	importErrorSidecarSuffix = ".error.txt"
)

// importWatchEnabled reports whether the auto-ingest watcher should run. It is
// off unless KIRO_IMPORT_WATCH is a truthy value (1/true/yes/on).
func importWatchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KIRO_IMPORT_WATCH"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// importWatchDir returns the directory the watcher scans, honoring KIRO_IMPORT_DIR.
func importWatchDir() string {
	if dir := strings.TrimSpace(os.Getenv("KIRO_IMPORT_DIR")); dir != "" {
		return dir
	}
	return defaultImportWatchDir
}

// startImportWatcher launches the watcher goroutine when enabled. Called from
// NewHandler. Returns immediately when the watcher is disabled.
func (h *Handler) startImportWatcher() {
	if !importWatchEnabled() {
		return
	}
	dir := importWatchDir()
	logger.Infof("[Import] auto-ingest watcher enabled, watching %s every %s", dir, importWatchInterval)

	// The watcher is Handler-owned background work. Register it before launching
	// so Shutdown's backgroundWG.Wait cannot race a late Add.
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		h.importWatchLoop(dir)
	}()
}

// importWatchLoop scans once after an interruptible startup settle delay, then
// on a fixed interval until stopRefresh.
func (h *Handler) importWatchLoop(dir string) {
	// A freshly-started container may mount a file while it is still being copied.
	// Unlike time.Sleep, this timer lets Handler.Shutdown stop the watcher without
	// waiting for the full settle delay.
	startup := time.NewTimer(3 * time.Second)
	defer startup.Stop()
	select {
	case <-startup.C:
	case <-h.stopRefresh:
		return
	}
	h.scanImportDir(dir)

	ticker := time.NewTicker(importWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.scanImportDir(dir)
		case <-h.stopRefresh:
			return
		}
	}
}

// scanImportDir reads the watch directory and imports each eligible *.json file.
// processed/ and failed/ subdirectories are skipped so handled files are never
// re-imported.
func (h *Handler) scanImportDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing dir is normal (operator hasn't created it yet); create it so a
		// later drop is picked up, and return quietly.
		if os.IsNotExist(err) {
			_ = os.MkdirAll(dir, 0o755)
		}
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// Skip files still being written (mtime too recent) to avoid racing a copy.
		if time.Since(info.ModTime()) < importMinFileAgeSeconds*time.Second {
			continue
		}
		h.processImportFile(dir, name)
	}
}

// processImportFile imports one credential file, then moves it to processed/ or
// failed/ (with a .error.txt sidecar) so it is handled exactly once.
func (h *Handler) processImportFile(dir, name string) {
	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		logger.Warnf("[Import] cannot read %s: %v", name, err)
		return
	}

	reqs, warnings, err := normalizeCliJson(raw)
	// A truncation warning (isTruncationWarning) means decodeConcatenatedObjects
	// stopped partway through the blob — some credentials after the break point
	// were never even parsed, let alone imported. Track it (FIX3) so this file
	// is never routed to processed/ as if every credential in it had been
	// handled; a truncated file staying visible in failed/ is the point.
	var truncationReason string
	for _, wmsg := range warnings {
		logger.Debugf("[Import] %s: %s", name, wmsg)
		if isTruncationWarning(wmsg) {
			truncationReason = wmsg
		}
	}
	if err != nil {
		h.moveImportFile(dir, name, importFailedSubdir, err.Error())
		return
	}

	existing := config.GetAccounts()
	var importedAny bool
	var failures []string
	for i, req := range reqs {
		if dup := duplicateAccountReason(existing, req); dup != "" {
			logger.Infof("[Import] skipping item %d in %s: %s", i+1, name, dup)
			continue
		}
		result, impErr := h.importCredential(req)
		if impErr != nil {
			// FIX5: surface a rotated-but-unpersisted refresh token the same way
			// apiImportCredentials/apiImportIdeCache do — a persistence failure
			// (duplicate id/refresh-token/api-key, or a config write error) can
			// land AFTER the IdP already rotated the token, and without this the
			// credential is permanently bricked (dead at the IdP, new token
			// dropped on the floor, needs an interactive re-login).
			msg := fmt.Sprintf("item %d: %s", i+1, impErr.Error())
			if rotated := strings.TrimSpace(result.RotatedRefreshToken); rotated != "" {
				msg = fmt.Sprintf("%s (identity provider rotated the refresh token before persistence failed; rotatedRefreshToken=%s)", msg, rotated)
			}
			failures = append(failures, msg)
			continue
		}
		importedAny = true
		// Track the just-added account so a multi-credential file dedupes within itself.
		existing = append(existing, result.Account)
		logger.Infof("[Import] added %s (%s) from %s", result.Account.Email, result.Account.AuthMethod, name)
	}

	if importedAny {
		h.pool.Reload()
	}

	if truncationReason != "" {
		// Even when the objects that DID parse imported successfully, a
		// truncated blob still has un-parsed credentials past the break point
		// that need operator attention — never let that read as "fully
		// handled" by landing quietly in processed/.
		reason := truncationReason
		if len(failures) > 0 {
			reason = truncationReason + "; " + strings.Join(failures, "; ")
		}
		h.moveImportFile(dir, name, importFailedSubdir, reason)
		return
	}

	if importedAny {
		h.moveImportFile(dir, name, importProcessedSubdir, "")
		return
	}

	// Nothing imported. If every item was a duplicate (no failures), treat it as
	// processed so we don't keep retrying a file whose accounts already exist.
	if len(failures) == 0 {
		h.moveImportFile(dir, name, importProcessedSubdir, "")
		return
	}
	h.moveImportFile(dir, name, importFailedSubdir, strings.Join(failures, "; "))
}

// duplicateAccountReason returns a non-empty reason when an account matching this
// request already exists (same refresh token, or same email + canonical auth
// method), so the watcher can skip it on repeated mounts. Returns "" when the
// credential is new.
//
// req.AuthMethod is the RAW caller-supplied alias (see importCredentialRequest's
// doc comment), while persisted accounts always carry the CANONICAL bucket
// (external_idp/idc/social/api_key) — h.importCredential is what does that
// bucketing. Run req.AuthMethod through the same normalizeImportAuthMethod
// bucketing here before comparing, or a dropped file spelling its method as
// e.g. "AzureAD" would never match an existing "external_idp" account and this
// soft dedupe would under-detect (a real duplicate would still be caught by
// config.AddAccount's own refresh-token/id uniqueness checks; this is a
// best-effort pre-filter, not the source of truth).
func duplicateAccountReason(existing []config.Account, req importCredentialRequest) string {
	rt := strings.TrimSpace(req.RefreshToken)
	email := strings.TrimSpace(strings.ToLower(req.Email))
	bucketedMethod := normalizeImportAuthMethod(req.AuthMethod, req.ClientID, req.ClientSecret, req.TokenEndpoint)
	for _, a := range existing {
		if rt != "" && strings.TrimSpace(a.RefreshToken) == rt {
			return "refresh token already imported"
		}
		if email != "" && strings.EqualFold(strings.TrimSpace(a.Email), email) &&
			strings.EqualFold(strings.TrimSpace(a.AuthMethod), bucketedMethod) {
			return "account with same email + auth method already exists"
		}
	}
	return ""
}

// moveImportFile relocates a handled file into the processed/ or failed/ subdir.
// On failure it also writes a <name>.error.txt sidecar with the reason. Best-effort:
// any move error is logged but never crashes the watcher.
// maxImportErrorMessageBytes bounds what moveImportFile writes to the
// .error.txt sidecar and the warning log line (FIX6). A generic external_idp
// refresh failure can embed auth/oidc.go's VERBATIM, UNBOUNDED upstream error
// response body (external IdP token exchange failed (status %d): %s), so
// without a cap a hostile or misbehaving token endpoint can blow up both the
// on-disk sidecar and the log with attacker-controlled bytes.
const maxImportErrorMessageBytes = 4096

// truncateImportErrorMessage bounds msg to maxImportErrorMessageBytes,
// appending a marker so a truncated message is never mistaken for a complete
// one (see maxImportErrorMessageBytes).
func truncateImportErrorMessage(msg string) string {
	if len(msg) <= maxImportErrorMessageBytes {
		return msg
	}
	return msg[:maxImportErrorMessageBytes] + fmt.Sprintf("... [truncated, %d bytes total]", len(msg))
}

func (h *Handler) moveImportFile(dir, name, subdir, errMsg string) {
	destDir := filepath.Join(dir, subdir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		logger.Warnf("[Import] cannot create %s: %v", destDir, err)
		return
	}
	src := filepath.Join(dir, name)
	dest := filepath.Join(destDir, name)
	// If a same-named file already lives in the destination, disambiguate with a
	// unix-nanosecond suffix so we never clobber a prior import's record.
	if _, err := os.Stat(dest); err == nil {
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		dest = filepath.Join(destDir, base+"."+time.Now().Format("20060102T150405.000000000")+ext)
	}
	if err := os.Rename(src, dest); err != nil {
		logger.Warnf("[Import] cannot move %s to %s: %v", name, subdir, err)
		return
	}
	// FIX6: os.Rename preserves the dropped file's ORIGINAL mode (e.g. a
	// 0644 drop from a host bind mount) into processed/ or failed/, where a
	// refresh token or API key then sits indefinitely at a looser mode than
	// the sidecar (which is always written 0600 below). Re-chmod the
	// credential file itself to 0600. Best-effort: a chmod failure (e.g. a
	// read-only bind mount) is logged, not fatal — the move already succeeded.
	if err := os.Chmod(dest, 0o600); err != nil {
		logger.Warnf("[Import] cannot chmod %s to 0600: %v", dest, err)
	}
	if errMsg != "" {
		errMsg = truncateImportErrorMessage(errMsg)
		sidecar := dest + importErrorSidecarSuffix
		_ = os.WriteFile(sidecar, []byte(errMsg+"\n"), 0o600)
		logger.Warnf("[Import] %s failed: %s", name, errMsg)
	}
}
