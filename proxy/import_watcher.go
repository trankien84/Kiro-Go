package proxy

import (
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
// This loop only ever reads a local directory and POSTs to the credential's OWN
// IdP token endpoint (via h.importCredential) — it does not iterate Kiro/Bedrock
// accounts or call any Kiro-facing endpoint, so it needs no IsBedrock() guard
// (unlike a loop that walks config.GetAccounts() to talk to Kiro itself).
// duplicateAccountReason below does read config.GetAccounts(), but only in
// memory for a local dedupe comparison — no request is sent for it.

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
	go h.importWatchLoop(dir)
}

// importWatchLoop scans once at startup, then on a fixed interval until stopRefresh.
func (h *Handler) importWatchLoop(dir string) {
	// Initial settle delay so a freshly-started container that mounts the folder
	// doesn't race a half-finished copy.
	time.Sleep(3 * time.Second)
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
	for _, wmsg := range warnings {
		logger.Debugf("[Import] %s: %s", name, wmsg)
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
			failures = append(failures, impErr.Error())
			continue
		}
		importedAny = true
		// Track the just-added account so a multi-credential file dedupes within itself.
		existing = append(existing, result.Account)
		logger.Infof("[Import] added %s (%s) from %s", result.Account.Email, result.Account.AuthMethod, name)
	}

	if importedAny {
		h.pool.Reload()
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
	if errMsg != "" {
		sidecar := dest + importErrorSidecarSuffix
		_ = os.WriteFile(sidecar, []byte(errMsg+"\n"), 0o600)
		logger.Warnf("[Import] %s failed: %s", name, errMsg)
	}
}
