package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

const (
	directAnthropicPermissionDenyTTL = 30 * time.Minute
	directAnthropicDefaultRateLimit  = time.Minute
	directAnthropicTransientCooldown = 10 * time.Second
	providerModelSnapshotFreshTTL    = 2 * time.Hour
	providerModelSnapshotStaleTTL    = 24 * time.Hour
)

type claudeProviderProfile uint8

const (
	claudeProviderUnknown claudeProviderProfile = iota
	claudeProviderAnthropicOfficial
	claudeProviderAnthropicCompatible
	claudeProviderKiro
	claudeProviderBedrockInvoke
	claudeProviderBedrockConverse
)

func (profile claudeProviderProfile) String() string {
	switch profile {
	case claudeProviderAnthropicOfficial:
		return "anthropic_official"
	case claudeProviderAnthropicCompatible:
		return "anthropic_compatible"
	case claudeProviderKiro:
		return "kiro"
	case claudeProviderBedrockInvoke:
		return "bedrock_invoke"
	case claudeProviderBedrockConverse:
		return "bedrock_converse"
	default:
		return "unknown"
	}
}

type claudeDeploymentKey struct {
	Profile         claudeProviderProfile
	CanonicalOrigin string
	ProviderModelID ProviderModelID
	APIVersion      string
}

func canonicalDirectAnthropicOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") ||
		(parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(hostname, ":") {
			parsed.Host = "[" + hostname + "]"
		} else {
			parsed.Host = hostname
		}
	} else {
		parsed.Host = net.JoinHostPort(hostname, port)
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	path := strings.TrimRight(parsed.Path, "/")
	if len(path) >= 3 && strings.EqualFold(path[len(path)-3:], "/v1") {
		path = strings.TrimRight(path[:len(path)-3], "/")
	}
	parsed.Path = path
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/")
}

func directAnthropicOriginIsOfficial(raw string) bool {
	rawURL, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || rawURL.User != nil || rawURL.RawQuery != "" || rawURL.Fragment != "" {
		return false
	}
	origin := canonicalDirectAnthropicOrigin(raw)
	parsed, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") ||
		!strings.EqualFold(parsed.Hostname(), "api.anthropic.com") ||
		strings.Trim(parsed.Path, "/") != "" {
		return false
	}
	port := parsed.Port()
	return port == "" || port == "443"
}

func claudeProviderProfileForAccount(account *config.Account) claudeProviderProfile {
	if account == nil {
		return claudeProviderUnknown
	}
	switch account.Kind() {
	case config.AccountKindDirectAnthropic:
		if directAnthropicOriginIsOfficial(account.BaseURL) {
			return claudeProviderAnthropicOfficial
		}
		return claudeProviderAnthropicCompatible
	case config.AccountKindBedrock:
		if accountUsesConverse(account) {
			return claudeProviderBedrockConverse
		}
		return claudeProviderBedrockInvoke
	case config.AccountKindKiroOAuth, config.AccountKindKiroAPIKey:
		return claudeProviderKiro
	default:
		return claudeProviderUnknown
	}
}

func claudeDeploymentKeyForAccount(account *config.Account, model string) claudeDeploymentKey {
	profile := claudeProviderProfileForAccount(account)
	key := claudeDeploymentKey{
		Profile:         profile,
		ProviderModelID: exactProviderModelID(model),
	}
	if profile == claudeProviderAnthropicOfficial || profile == claudeProviderAnthropicCompatible {
		key.CanonicalOrigin = canonicalDirectAnthropicOrigin(account.BaseURL)
		key.APIVersion = "2023-06-01"
	}
	return key
}

type directAnthropicModelMetadata struct {
	ID                  string
	DisplayName         string
	CreatedAt           string
	MaxInputTokens      int64
	MaxTokens           int64
	Capabilities        directAnthropicModelCapabilities
	CapabilitiesPresent bool
}

type directAnthropicCapabilitySupport struct {
	Supported bool `json:"supported"`
}

type directAnthropicThinkingTypes struct {
	Adaptive     directAnthropicCapabilitySupport  `json:"adaptive"`
	BetweenTools directAnthropicCapabilitySupport  `json:"between_tools"`
	Disabled     *directAnthropicCapabilitySupport `json:"disabled"`
	Enabled      directAnthropicCapabilitySupport  `json:"enabled"`
}

type directAnthropicThinkingCapability struct {
	Supported bool                         `json:"supported"`
	Types     directAnthropicThinkingTypes `json:"types"`
}

type directAnthropicEffortCapability struct {
	Supported bool                             `json:"supported"`
	Low       directAnthropicCapabilitySupport `json:"low"`
	Medium    directAnthropicCapabilitySupport `json:"medium"`
	High      directAnthropicCapabilitySupport `json:"high"`
	XHigh     directAnthropicCapabilitySupport `json:"xhigh"`
	Max       directAnthropicCapabilitySupport `json:"max"`
}

type directAnthropicContextManagementCapability struct {
	Supported     bool                             `json:"supported"`
	ClearThinking directAnthropicCapabilitySupport `json:"clear_thinking_20251015"`
	ClearToolUses directAnthropicCapabilitySupport `json:"clear_tool_uses_20250919"`
	Compact       directAnthropicCapabilitySupport `json:"compact_20260112"`
}

type directAnthropicModelCapabilities struct {
	Thinking          directAnthropicThinkingCapability          `json:"thinking"`
	Effort            directAnthropicEffortCapability            `json:"effort"`
	ImageInput        directAnthropicCapabilitySupport           `json:"image_input"`
	PDFInput          directAnthropicCapabilitySupport           `json:"pdf_input"`
	StructuredOutputs directAnthropicCapabilitySupport           `json:"structured_outputs"`
	CodeExecution     directAnthropicCapabilitySupport           `json:"code_execution"`
	ContextManagement directAnthropicContextManagementCapability `json:"context_management"`
	Citations         directAnthropicCapabilitySupport           `json:"citations"`
	Batch             directAnthropicCapabilitySupport           `json:"batch"`
}

type providerRuntimeDiagnostics struct {
	ModelCount              int              `json:"modelCount"`
	ModelSnapshotAt         int64            `json:"modelSnapshotAt,omitempty"`
	ModelSnapshotFreshUntil int64            `json:"modelSnapshotFreshUntil,omitempty"`
	ModelSnapshotStaleUntil int64            `json:"modelSnapshotStaleUntil,omitempty"`
	ModelSnapshotStale      bool             `json:"modelSnapshotStale"`
	CooldownUntil           int64            `json:"cooldownUntil,omitempty"`
	ModelDeniedUntil        map[string]int64 `json:"modelDeniedUntil,omitempty"`
	SourceGeneration        uint64           `json:"sourceGeneration"`
}

func (h *Handler) providerRuntimeDiagnostics(accountID string) providerRuntimeDiagnostics {
	result := providerRuntimeDiagnostics{}
	if h == nil || strings.TrimSpace(accountID) == "" {
		return result
	}
	now := time.Now()
	currentLifecycle, _ := config.GetAccountLifecycleGeneration(accountID)
	key := accountLifecycleKey{accountID: accountID, generation: currentLifecycle}
	h.providerStateMu.RLock()
	if snapshot, ok := h.accountModelSnapshots[key]; ok {
		result.ModelCount = len(snapshot.Models)
		if !snapshot.RefreshedAt.IsZero() {
			result.ModelSnapshotAt = snapshot.RefreshedAt.Unix()
		}
		if !snapshot.FreshUntil.IsZero() {
			result.ModelSnapshotFreshUntil = snapshot.FreshUntil.Unix()
		}
		if !snapshot.StaleUntil.IsZero() {
			result.ModelSnapshotStaleUntil = snapshot.StaleUntil.Unix()
		}
		result.ModelSnapshotStale = snapshot.Stale || (!snapshot.FreshUntil.IsZero() && !snapshot.FreshUntil.After(now))
	}
	if until := h.providerCooldownUntil[key]; until.After(now) {
		result.CooldownUntil = until.Unix()
	}
	if denied := h.providerModelDeniedUntil[key]; len(denied) > 0 {
		result.ModelDeniedUntil = make(map[string]int64)
		for modelID, until := range denied {
			if until.After(now) {
				result.ModelDeniedUntil[modelID] = until.Unix()
			}
		}
		if len(result.ModelDeniedUntil) == 0 {
			result.ModelDeniedUntil = nil
		}
	}
	result.SourceGeneration = h.providerSourceGenerations[key]
	h.providerStateMu.RUnlock()
	return result
}

type accountModelSnapshot struct {
	LifecycleGeneration uint64
	Models              []ModelInfo
	Metadata            map[string]directAnthropicModelMetadata
	SourceFingerprint   string
	SourceGeneration    uint64
	RefreshedAt         time.Time
	FreshUntil          time.Time
	StaleUntil          time.Time
	Stale               bool
}

var errAccountModelSourceChanged = errors.New("account model source changed during refresh")

type accountModelSource struct {
	Kind                   string            `json:"kind"`
	LifecycleGeneration    uint64            `json:"lifecycle_generation,omitempty"`
	BaseURL                string            `json:"base_url,omitempty"`
	KiroAPIKey             string            `json:"kiro_api_key,omitempty"`
	Region                 string            `json:"region,omitempty"`
	ProfileARN             string            `json:"profile_arn,omitempty"`
	BedrockAPIKey          string            `json:"bedrock_api_key,omitempty"`
	BedrockAccessKeyID     string            `json:"bedrock_access_key_id,omitempty"`
	BedrockSecretAccessKey string            `json:"bedrock_secret_access_key,omitempty"`
	BedrockSessionToken    string            `json:"bedrock_session_token,omitempty"`
	BedrockRegions         []string          `json:"bedrock_regions,omitempty"`
	BedrockModelMap        map[string]string `json:"bedrock_model_map,omitempty"`
	BedrockUseConverse     bool              `json:"bedrock_use_converse,omitempty"`
}

func accountModelSourceFingerprint(account *config.Account) string {
	if account == nil {
		return ""
	}
	source := accountModelSource{
		Kind:                account.Kind().String(),
		LifecycleGeneration: account.DispatchGeneration,
	}
	switch account.Kind() {
	case config.AccountKindDirectAnthropic, config.AccountKindCustomRelay:
		source.BaseURL = strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
		source.KiroAPIKey = strings.TrimSpace(account.KiroApiKey)
	case config.AccountKindKiroAPIKey:
		source.KiroAPIKey = strings.TrimSpace(account.KiroApiKey)
		source.Region = strings.TrimSpace(account.Region)
		source.ProfileARN = strings.TrimSpace(account.ProfileArn)
	case config.AccountKindKiroOAuth:
		source.Region = strings.TrimSpace(account.Region)
		source.ProfileARN = strings.TrimSpace(account.ProfileArn)
	case config.AccountKindBedrock:
		source.Region = strings.TrimSpace(account.Region)
		source.BedrockAPIKey = strings.TrimSpace(account.BedrockAPIKey)
		source.BedrockAccessKeyID = strings.TrimSpace(account.BedrockAccessKeyID)
		source.BedrockSecretAccessKey = strings.TrimSpace(account.BedrockSecretAccessKey)
		source.BedrockSessionToken = strings.TrimSpace(account.BedrockSessionToken)
		source.BedrockRegions = append([]string(nil), account.BedrockRegions...)
		source.BedrockModelMap = account.BedrockModelMap
		source.BedrockUseConverse = account.BedrockUseConverse
	}
	payload, _ := json.Marshal(source)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func accountModelSourceStillCurrent(account *config.Account, expected string) bool {
	if account == nil || strings.TrimSpace(account.ID) == "" || expected == "" {
		return false
	}
	for _, current := range config.GetAccounts() {
		if current.ID == account.ID {
			return accountModelSourceFingerprint(&current) == expected
		}
	}
	return false
}

func accountReadyForModelDiscovery(account *config.Account) bool {
	if account == nil || !account.Enabled {
		return false
	}
	switch account.Kind() {
	case config.AccountKindBedrock:
		return strings.TrimSpace(account.BedrockAPIKey) != "" ||
			(strings.TrimSpace(account.BedrockAccessKeyID) != "" && strings.TrimSpace(account.BedrockSecretAccessKey) != "")
	case config.AccountKindDirectAnthropic, config.AccountKindCustomRelay:
		return strings.TrimSpace(account.BaseURL) != "" && strings.TrimSpace(account.KiroApiKey) != ""
	case config.AccountKindKiroAPIKey:
		return strings.TrimSpace(account.KiroApiKey) != "" || strings.TrimSpace(account.AccessToken) != ""
	case config.AccountKindKiroOAuth:
		return strings.TrimSpace(account.AccessToken) != ""
	default:
		return false
	}
}

func normalizedProviderModelID(model string) string {
	// Provider IDs are opaque. This helper is only a case-insensitive lookup key;
	// it must never run the client alias mapper or alter the dispatched value.
	return providerModelLookupKey(model)
}

func cloneModelInfoSlice(models []ModelInfo) []ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]ModelInfo, len(models))
	copy(out, models)
	return out
}

func cloneDirectAnthropicMetadata(source map[string]directAnthropicModelMetadata) map[string]directAnthropicModelMetadata {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]directAnthropicModelMetadata, len(source))
	for id, metadata := range source {
		out[id] = metadata
	}
	return out
}

func signalProviderExpiryWake(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// scheduleProviderModelUnionRebuild moves the single handler-owned expiry loop
// to an earlier deadline. The loop, rather than a time.AfterFunc callback, owns
// the timer and therefore has deterministic shutdown semantics.
func (h *Handler) scheduleProviderModelUnionRebuild(at time.Time) {
	if h == nil || at.IsZero() || h.closed.Load() {
		return
	}
	h.providerExpiryMu.Lock()
	if h.providerExpiryAt.IsZero() || at.Before(h.providerExpiryAt) {
		h.providerExpiryAt = at
	}
	h.providerExpiryMu.Unlock()
	signalProviderExpiryWake(h.providerExpiryWake)
}

func (h *Handler) replaceProviderModelUnionRebuild(at time.Time) {
	if h == nil || h.closed.Load() {
		return
	}
	h.providerExpiryMu.Lock()
	h.providerExpiryAt = at
	h.providerExpiryMu.Unlock()
	signalProviderExpiryWake(h.providerExpiryWake)
}

func (h *Handler) stopProviderModelUnionRebuild() {
	if h == nil {
		return
	}
	h.replaceProviderModelUnionRebuild(time.Time{})
}

func stopAndDrainTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (h *Handler) runProviderExpiryLoop() {
	if h == nil {
		return
	}
	timer := time.NewTimer(time.Hour)
	stopAndDrainTimer(timer)
	for {
		h.providerExpiryMu.Lock()
		deadline := h.providerExpiryAt
		h.providerExpiryMu.Unlock()

		if deadline.IsZero() {
			select {
			case <-h.providerExpiryWake:
				continue
			case <-h.providerExpiryStop:
				stopAndDrainTimer(timer)
				return
			}
		}

		delay := time.Until(deadline)
		if delay < 0 {
			delay = 0
		}
		stopAndDrainTimer(timer)
		timer.Reset(delay + time.Millisecond)

		select {
		case <-timer.C:
			h.providerExpiryMu.Lock()
			active := h.providerExpiryAt.Equal(deadline)
			if active {
				h.providerExpiryAt = time.Time{}
			}
			h.providerExpiryMu.Unlock()
			if active && !h.closed.Load() {
				h.rebuildEnabledModelUnion()
			}
		case <-h.providerExpiryWake:
			continue
		case <-h.providerExpiryStop:
			stopAndDrainTimer(timer)
			return
		}
	}
}

func (h *Handler) providerSourceGeneration(account *config.Account) uint64 {
	if h == nil || account == nil || strings.TrimSpace(account.ID) == "" {
		return 0
	}
	key := lifecycleOrLegacyKeyForAccount(account)
	h.providerStateMu.RLock()
	generation := h.providerSourceGenerations[key]
	h.providerStateMu.RUnlock()
	return generation
}

func (h *Handler) replaceAccountModelSnapshot(account *config.Account, expectedFingerprint string, expectedGeneration uint64, models []ModelInfo, metadata map[string]directAnthropicModelMetadata, modelIDs []string) bool {
	if h == nil || account == nil || strings.TrimSpace(account.ID) == "" || !accountModelSourceStillCurrent(account, expectedFingerprint) {
		return false
	}
	key := lifecycleOrLegacyKeyForAccount(account)
	if key.accountID == "" {
		return false
	}
	// The lifecycle check can become stale before this lock. That is harmless:
	// this writer mutates only its exact (ID,generation) key and cannot erase N+1.
	h.providerStateMu.Lock()
	if h.providerSourceGenerations[key] != expectedGeneration {
		h.providerStateMu.Unlock()
		return false
	}
	if h.accountModelSnapshots == nil {
		h.accountModelSnapshots = make(map[accountLifecycleKey]accountModelSnapshot)
	}
	now := time.Now()
	snapshot := accountModelSnapshot{LifecycleGeneration: key.generation, Models: cloneModelInfoSlice(models), Metadata: cloneDirectAnthropicMetadata(metadata), SourceFingerprint: expectedFingerprint, SourceGeneration: expectedGeneration, RefreshedAt: now, FreshUntil: now.Add(providerModelSnapshotFreshTTL), StaleUntil: now.Add(providerModelSnapshotStaleTTL)}
	h.accountModelSnapshots[key] = snapshot
	if h.pool != nil {
		routes := buildAccountRoutes(account, snapshot, now)
		accepted := append([]string(nil), modelIDs...)
		accepted = append(accepted, routes.coarseModelIDs()...)
		if key.generation != 0 {
			h.pool.SetModelListForLifecycle(account.ID, key.generation, accepted)
		} else {
			h.pool.SetModelList(account.ID, accepted)
		}
	}
	h.providerStateMu.Unlock()
	h.rebuildEnabledModelUnion()
	return true
}

func (h *Handler) clearAccountProviderRuntimeState(accountID string) {
	if h == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	// Explicit all-generations semantics for operator/admin cache resets only.
	currentLifecycle, _ := config.GetAccountLifecycleGeneration(accountID)
	currentKey := accountLifecycleKey{accountID: accountID, generation: currentLifecycle}
	h.providerStateMu.Lock()
	if h.providerSourceGenerations == nil {
		h.providerSourceGenerations = make(map[accountLifecycleKey]uint64)
	}
	seenCurrent := false
	for key := range h.providerSourceGenerations {
		if key.accountID == accountID {
			h.providerSourceGenerations[key]++
			if key == currentKey {
				seenCurrent = true
			}
		}
	}
	if !seenCurrent && currentKey.accountID != "" {
		h.providerSourceGenerations[currentKey]++
	}
	for key := range h.accountModelSnapshots {
		if key.accountID == accountID {
			delete(h.accountModelSnapshots, key)
		}
	}
	for key := range h.providerCooldownUntil {
		if key.accountID == accountID {
			delete(h.providerCooldownUntil, key)
		}
	}
	for key := range h.providerModelDeniedUntil {
		if key.accountID == accountID {
			delete(h.providerModelDeniedUntil, key)
		}
	}
	if h.pool != nil {
		h.pool.ClearModelList(accountID)
	}
	h.providerStateMu.Unlock()
}

func (h *Handler) removeAccountProviderRuntimeState(accountID string) {
	h.clearAccountProviderRuntimeState(accountID)
	h.rebuildEnabledModelUnion()
}

func (h *Handler) clearAccountProviderRuntimeStateForLifecycle(account *config.Account) bool {
	if h == nil || account == nil || strings.TrimSpace(account.ID) == "" || account.DispatchGeneration == 0 {
		return false
	}
	key := accountLifecycleKey{accountID: account.ID, generation: account.DispatchGeneration}
	h.providerStateMu.Lock()
	if h.providerSourceGenerations == nil {
		h.providerSourceGenerations = make(map[accountLifecycleKey]uint64)
	}
	// Fence exactly N before deleting it. An in-flight N refresh can no longer
	// republish after cleanup, while N+1 remains untouched.
	h.providerSourceGenerations[key]++
	delete(h.accountModelSnapshots, key)
	delete(h.providerCooldownUntil, key)
	delete(h.providerModelDeniedUntil, key)
	if h.pool != nil {
		h.pool.ClearModelListForLifecycle(account.ID, account.DispatchGeneration)
	}
	h.providerStateMu.Unlock()
	h.rebuildEnabledModelUnion()
	return true
}

func (h *Handler) sweepRetiredProviderLifecycleState() {
	if h == nil {
		return
	}

	// First capture candidate keys without consulting config while providerStateMu
	// is held. Generation numbers are monotonic: once an exact (ID,generation)
	// stops being current, it can never become current again.
	candidates := make(map[accountLifecycleKey]struct{})
	h.providerStateMu.RLock()
	for key := range h.accountModelSnapshots {
		if key.valid() {
			candidates[key] = struct{}{}
		}
	}
	for key := range h.providerCooldownUntil {
		if key.valid() {
			candidates[key] = struct{}{}
		}
	}
	for key := range h.providerModelDeniedUntil {
		if key.valid() {
			candidates[key] = struct{}{}
		}
	}
	for key := range h.providerSourceGenerations {
		if key.valid() {
			candidates[key] = struct{}{}
		}
	}
	h.providerStateMu.RUnlock()

	retired := make([]accountLifecycleKey, 0)
	for key := range candidates {
		if !key.current() {
			retired = append(retired, key)
		}
	}
	if len(retired) == 0 {
		return
	}

	h.providerStateMu.Lock()
	if h.providerSourceGenerations == nil {
		h.providerSourceGenerations = make(map[accountLifecycleKey]uint64)
	}
	for _, key := range retired {
		delete(h.accountModelSnapshots, key)
		delete(h.providerCooldownUntil, key)
		delete(h.providerModelDeniedUntil, key)
		// Retired lifecycle keys can never become current again. It is therefore
		// safe to garbage-collect the exact source fence as well. A refresh that
		// passed its precheck before this sweep can at worst republish the same
		// retired key; its mandatory rebuild immediately sweeps that key again.
		delete(h.providerSourceGenerations, key)
	}
	h.providerStateMu.Unlock()

	if h.pool != nil {
		for _, key := range retired {
			h.pool.ClearModelListForLifecycle(key.accountID, key.generation)
		}
	}
}

func (h *Handler) accountModelSnapshotRoutableForSource(account *config.Account, expectedFingerprint string, expectedGeneration uint64, now time.Time) bool {
	if h == nil || account == nil || strings.TrimSpace(account.ID) == "" {
		return false
	}
	key := lifecycleOrLegacyKeyForAccount(account)
	h.providerStateMu.RLock()
	snapshot, ok := h.accountModelSnapshots[key]
	currentGeneration := h.providerSourceGenerations[key]
	h.providerStateMu.RUnlock()
	return ok && snapshot.LifecycleGeneration == key.generation && currentGeneration == expectedGeneration && snapshot.SourceFingerprint == expectedFingerprint && snapshot.SourceGeneration == expectedGeneration && snapshotRoutable(snapshot, now)
}

func (h *Handler) markAccountModelSnapshotStale(account *config.Account, expectedFingerprint string, expectedGeneration uint64) {
	if h == nil || account == nil || strings.TrimSpace(account.ID) == "" || expectedFingerprint == "" {
		return
	}
	key := lifecycleOrLegacyKeyForAccount(account)
	if !accountModelSourceStillCurrent(account, expectedFingerprint) {
		return
	}
	var staleUntil time.Time
	h.providerStateMu.Lock()
	if snapshot, ok := h.accountModelSnapshots[key]; ok && snapshot.LifecycleGeneration == key.generation && snapshot.SourceFingerprint == expectedFingerprint && snapshot.SourceGeneration == expectedGeneration && h.providerSourceGenerations[key] == expectedGeneration {
		now := time.Now()
		snapshot.Stale = true
		snapshot.FreshUntil = now
		if snapshot.RefreshedAt.IsZero() || snapshot.StaleUntil.IsZero() || !snapshot.StaleUntil.After(now) {
			h.accountModelSnapshots[key] = snapshot
			if h.pool != nil {
				if key.generation != 0 {
					h.pool.SetModelListUnavailableForLifecycle(account.ID, key.generation)
				} else {
					h.pool.SetModelListUnavailable(account.ID)
				}
			}
		} else {
			staleUntil = snapshot.StaleUntil
			h.accountModelSnapshots[key] = snapshot
		}
	}
	h.providerStateMu.Unlock()
	if !staleUntil.IsZero() {
		h.scheduleProviderModelUnionRebuild(staleUntil)
	}
}

func buildEnabledModelUnion(
	enabledIDs []string,
	snapshots map[string]accountModelSnapshot,
	modelDeniedUntil map[string]map[string]time.Time,
	now time.Time,
) []ModelInfo {
	union := make([]ModelInfo, 0)
	for _, id := range enabledIDs {
		snapshot, ok := snapshots[id]
		if !ok {
			continue
		}
		if !snapshotRoutable(snapshot, now) {
			continue
		}
		denied := modelDeniedUntil[id]
		models := make([]ModelInfo, 0, len(snapshot.Models))
		for _, model := range snapshot.Models {
			providerID := exactProviderModelID(model.ModelId)
			if until := denied[exactProviderDenialKey(providerID)]; until.After(now) {
				continue
			}
			if until := denied[normalizedProviderModelID(model.ModelId)]; until.After(now) {
				continue
			}
			models = append(models, model)
		}
		union = mergeUniqueModels(union, models)
	}
	return union
}

func freshestDirectAnthropicDeploymentMetadata(
	target claudeDeploymentKey,
	model string,
	accounts []config.Account,
	snapshots map[accountLifecycleKey]accountModelSnapshot,
	now time.Time,
) (directAnthropicModelMetadata, bool) {
	providerID := exactProviderModelID(model)
	var selected directAnthropicModelMetadata
	var selectedAt time.Time
	found := false
	for index := range accounts {
		account := &accounts[index]
		if account.Kind() != config.AccountKindDirectAnthropic ||
			claudeDeploymentKeyForAccount(account, model) != target {
			continue
		}
		key := accountLifecycleKey{accountID: account.ID, generation: account.DispatchGeneration}
		if key.generation == 0 {
			key = legacyBedrockLifecycleKey(account.ID)
		}
		snapshot, ok := snapshots[key]
		if !ok || snapshot.LifecycleGeneration != key.generation || !snapshotFresh(snapshot, now) {
			continue
		}
		metadata, ok := directAnthropicMetadataForProviderID(snapshot.Metadata, providerID)
		if !ok || !metadata.CapabilitiesPresent {
			continue
		}
		if !found || snapshot.RefreshedAt.After(selectedAt) {
			selected = metadata
			selectedAt = snapshot.RefreshedAt
			found = true
		}
	}
	return selected, found
}

func (h *Handler) directAnthropicModelMetadataForDeployment(
	account *config.Account,
	model string,
	now time.Time,
) (directAnthropicModelMetadata, bool, claudeDeploymentKey) {
	deployment := claudeDeploymentKeyForAccount(account, model)
	if h == nil || account == nil ||
		account.Kind() != config.AccountKindDirectAnthropic {
		return directAnthropicModelMetadata{}, false, deployment
	}
	var accounts []config.Account
	if config.Get() != nil {
		accounts = config.GetAccounts()
	}
	// A just-created account can be queried before the config snapshot is
	// observable to another goroutine. Include the selected account so its own
	// freshly learned deployment metadata remains usable in that narrow window.
	present := false
	for index := range accounts {
		if accounts[index].ID == account.ID {
			present = true
			break
		}
	}
	if !present {
		accounts = append(accounts, *account)
	}

	h.providerStateMu.RLock()
	metadata, found := freshestDirectAnthropicDeploymentMetadata(
		deployment,
		model,
		accounts,
		h.accountModelSnapshots,
		now,
	)
	h.providerStateMu.RUnlock()
	return metadata, found, deployment
}

func appendUniqueCapabilityValue(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}

func mergeOfficialDirectAnthropicReasoningSemantics(
	model string,
	capability ModelReasoningCapability,
	metadata directAnthropicModelMetadata,
) ModelReasoningCapability {
	// Respect the live disabled support value when it is present. The pointer
	// distinguishes explicit false from an older/partial Models API payload
	// where disabled is absent and an exact official fallback may be used.
	static := resolveClaudeProviderReasoningCapability(model)
	if metadata.Capabilities.Thinking.Types.Disabled == nil && !isClaudeSonnet55Model(model) {
		if _, ok := matchThinkingType("disabled", static.ThinkingTypes); ok {
			capability.ThinkingTypes = appendUniqueCapabilityValue(
				capability.ThinkingTypes,
				"disabled",
			)
		}
	}
	return capability
}

func directAnthropicMetadataHasReasoningDetail(
	metadata directAnthropicModelMetadata,
) bool {
	thinking := metadata.Capabilities.Thinking.Types
	effort := metadata.Capabilities.Effort
	return thinking.Adaptive.Supported ||
		thinking.BetweenTools.Supported ||
		thinking.Disabled != nil ||
		thinking.Enabled.Supported ||
		effort.Low.Supported ||
		effort.Medium.Supported ||
		effort.High.Supported ||
		effort.XHigh.Supported ||
		effort.Max.Supported
}

func (h *Handler) directAnthropicReasoningCapabilityForAccount(
	account *config.Account,
	model string,
) (ModelReasoningCapability, bool, claudeDeploymentKey) {
	metadata, fresh, deployment := h.directAnthropicModelMetadataForDeployment(
		account,
		model,
		time.Now(),
	)
	if fresh && metadata.CapabilitiesPresent {
		if deployment.Profile == claudeProviderAnthropicCompatible &&
			!directAnthropicMetadataHasReasoningDetail(metadata) {
			// A compatible endpoint may expose a partial capabilities object. Missing
			// reasoning detail is unknown, not an authoritative negative.
			return ModelReasoningCapability{}, false, deployment
		}
		capability := directAnthropicReasoningCapability(model, metadata)
		if deployment.Profile == claudeProviderAnthropicOfficial {
			capability = mergeOfficialDirectAnthropicReasoningSemantics(model, capability, metadata)
		}
		return capability, strings.TrimSpace(capability.ModelID) != "", deployment
	}
	if deployment.Profile == claudeProviderAnthropicOfficial {
		capability := resolveClaudeProviderReasoningCapability(model)
		return capability, strings.TrimSpace(capability.ModelID) != "", deployment
	}
	// Compatible endpoints without a fresh validated Models response remain
	// unknown. The request is allowed to reach that endpoint for upstream
	// validation instead of borrowing official Anthropic semantics by model ID.
	return ModelReasoningCapability{}, false, deployment
}

func (h *Handler) rebuildEnabledModelUnion() {
	if h == nil {
		return
	}

	// Rebuilds are serialized so an older projection cannot publish after a
	// newer projection. Runtime storage itself is lifecycle-keyed: a projection
	// only reads the exact key belonging to each enabled account and never
	// deletes a different generation based on a possibly stale config snapshot.
	h.providerRebuildMu.Lock()
	defer h.providerRebuildMu.Unlock()

	sweepRetiredBedrockLifecycleState()
	h.sweepRetiredProviderLifecycleState()

	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	enabledIDs := make([]string, 0, len(enabled))
	for i := range enabled {
		enabledIDs = append(enabledIDs, enabled[i].ID)
	}

	now := time.Now()
	snapshots := make(map[string]accountModelSnapshot, len(enabledIDs))
	cooldowns := make(map[string]time.Time, len(enabledIDs))
	deniedUntil := make(map[string]map[string]time.Time, len(enabledIDs))

	h.providerStateMu.Lock()
	// Time-based lazy cleanup is safe because it only removes the exact entry
	// whose own deadline has expired; it never infers staleness from another
	// lifecycle's generation.
	for key, until := range h.providerCooldownUntil {
		if !until.After(now) {
			delete(h.providerCooldownUntil, key)
		}
	}
	for key, denied := range h.providerModelDeniedUntil {
		for modelID, until := range denied {
			if !until.After(now) {
				delete(denied, modelID)
			}
		}
		if len(denied) == 0 {
			delete(h.providerModelDeniedUntil, key)
		}
	}
	for index, id := range enabledIDs {
		account := &enabled[index]
		key := lifecycleOrLegacyKeyForAccount(account)
		if snapshot, ok := h.accountModelSnapshots[key]; ok {
			// Source replacement advances the lifecycle. Keep the fingerprint check
			// as a second fence for legacy generation-0 callers/tests.
			if snapshot.LifecycleGeneration == key.generation &&
				snapshot.SourceFingerprint == accountModelSourceFingerprint(account) {
				snapshot.Models = cloneModelInfoSlice(snapshot.Models)
				snapshot.Metadata = cloneDirectAnthropicMetadata(snapshot.Metadata)
				snapshots[id] = snapshot
			}
		} else if account.Kind() == config.AccountKindBedrock && len(account.BedrockModelMap) > 0 {
			// An explicit Bedrock model map is operator-authoritative routing data; it
			// must remain usable when ListFoundationModels is unavailable or the
			// credential intentionally lacks control-plane discovery permission.
			// Publish a routable empty discovery snapshot so buildAccountRoutes can
			// materialize only those explicit mappings (never guessed defaults).
			snapshots[id] = accountModelSnapshot{
				LifecycleGeneration: key.generation,
				SourceFingerprint:   accountModelSourceFingerprint(account),
				RefreshedAt:         now,
				FreshUntil:          now.Add(providerModelSnapshotFreshTTL),
				StaleUntil:          now.Add(providerModelSnapshotStaleTTL),
			}
		}
		if until := h.providerCooldownUntil[key]; until.After(now) {
			cooldowns[id] = until
		}
		if denied := h.providerModelDeniedUntil[key]; len(denied) > 0 {
			clone := make(map[string]time.Time, len(denied))
			for modelID, until := range denied {
				clone[modelID] = until
			}
			deniedUntil[id] = clone
		}
	}
	h.providerStateMu.Unlock()

	version := h.providerRegistryVersion.Add(1)
	registry := buildProviderRegistry(version, enabled, snapshots, cooldowns, deniedUntil, allowOverUsage, now)
	h.providerRegistry.Store(registry)

	if h.pool != nil {
		for index := range enabled {
			account := &enabled[index]
			accountID := account.ID
			availability, known := registry.Accounts[accountID]
			if !known {
				if account.DispatchGeneration != 0 {
					h.pool.ClearModelListForLifecycle(accountID, account.DispatchGeneration)
				} else {
					h.pool.ClearModelList(accountID)
				}
				continue
			}
			routable := !availability.RefreshedAt.IsZero() &&
				!availability.StaleUntil.IsZero() && availability.StaleUntil.After(now)
			if availability.CooldownUntil.After(now) || !routable {
				if account.DispatchGeneration != 0 {
					h.pool.SetModelListUnavailableForLifecycle(accountID, account.DispatchGeneration)
				} else {
					h.pool.SetModelListUnavailable(accountID)
				}
				continue
			}
			if account.DispatchGeneration != 0 {
				h.pool.SetModelListForLifecycle(accountID, account.DispatchGeneration, availability.Routes.coarseModelIDs())
			} else {
				h.pool.SetModelList(accountID, availability.Routes.coarseModelIDs())
			}
		}
	}

	union := make([]ModelInfo, 0, len(registry.PublicModels))
	for _, public := range registry.PublicModels {
		union = append(union, public.Model)
	}
	h.modelsCacheMu.Lock()
	h.cachedModels = union
	h.modelsCacheTime = now.Unix()
	h.modelsCacheMu.Unlock()

	h.replaceProviderModelUnionRebuild(registry.NextExpiry)
}

func (h *Handler) providerAccountBlocked(accountID, model string) (bool, string) {
	if h == nil || strings.TrimSpace(accountID) == "" {
		return false, ""
	}
	currentLifecycle, _ := config.GetAccountLifecycleGeneration(accountID)
	key := accountLifecycleKey{accountID: accountID, generation: currentLifecycle}
	if currentLifecycle == 0 {
		key = legacyBedrockLifecycleKey(accountID)
	}
	now := time.Now()
	providerID := exactProviderModelID(model)
	keys := []string{
		exactProviderDenialKey(providerID),
		aliasProviderDenialKey(model),
		// Honor legacy lowercase runtime keys until they naturally expire.
		normalizedProviderModelID(model),
	}
	expiredModelView := false

	h.providerStateMu.Lock()
	if until := h.providerCooldownUntil[key]; until.After(now) {
		h.providerStateMu.Unlock()
		return true, "cooldown-until-" + until.UTC().Format(time.RFC3339)
	} else if !until.IsZero() {
		delete(h.providerCooldownUntil, key)
		expiredModelView = true
	}

	if byModel := h.providerModelDeniedUntil[key]; byModel != nil {
		for _, denialKey := range keys {
			if denialKey == "" {
				continue
			}
			if until := byModel[denialKey]; until.After(now) {
				h.providerStateMu.Unlock()
				return true, "model-denied-until-" + until.UTC().Format(time.RFC3339)
			} else if !until.IsZero() {
				delete(byModel, denialKey)
				expiredModelView = true
			}
		}
		if len(byModel) == 0 {
			delete(h.providerModelDeniedUntil, key)
		}
	}
	h.providerStateMu.Unlock()
	if expiredModelView {
		h.rebuildEnabledModelUnion()
	}
	return false, ""
}

// mutateProviderRuntime serializes an overlay mutation and republishes exactly
// one immutable registry generation after releasing providerStateMu.
func (h *Handler) mutateProviderRuntime(mutator func(now time.Time)) {
	if h == nil || mutator == nil || h.closed.Load() {
		return
	}
	h.providerStateMu.Lock()
	mutator(time.Now())
	h.providerStateMu.Unlock()
	h.rebuildEnabledModelUnion()
}

func providerRuntimeGeneration(accountID string, captured uint64) uint64 {
	if captured != 0 {
		return captured
	}
	generation, _ := config.GetAccountLifecycleGeneration(accountID)
	return generation
}

func (h *Handler) setProviderCooldownForGeneration(accountID string, generation uint64, duration time.Duration) {
	if h == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	generation = providerRuntimeGeneration(accountID, generation)
	if generation != 0 && !config.AccountLifecycleMatches(accountID, generation) {
		return
	}
	if duration <= 0 {
		duration = directAnthropicTransientCooldown
	}
	if duration > 24*time.Hour {
		duration = 24 * time.Hour
	}
	key := accountLifecycleKey{accountID: accountID, generation: generation}
	if generation == 0 {
		key = legacyBedrockLifecycleKey(accountID)
	}
	h.mutateProviderRuntime(func(now time.Time) {
		if h.providerCooldownUntil == nil {
			h.providerCooldownUntil = make(map[accountLifecycleKey]time.Time)
		}
		until := now.Add(duration)
		if current := h.providerCooldownUntil[key]; current.Before(until) {
			h.providerCooldownUntil[key] = until
		}
	})
}

func (h *Handler) setProviderCooldownForAccount(account *config.Account, duration time.Duration) {
	if account == nil {
		return
	}
	h.setProviderCooldownForGeneration(account.ID, account.DispatchGeneration, duration)
}

// setProviderCooldown is retained for current-account admin/background callers.
// Request paths should pass the immutable account snapshot instead.
func (h *Handler) setProviderCooldown(accountID string, duration time.Duration) {
	h.setProviderCooldownForGeneration(accountID, 0, duration)
}

func (h *Handler) setProviderModelDeniedForGeneration(accountID string, generation uint64, model string, duration time.Duration) {
	providerID := exactProviderModelID(model)
	if h == nil || strings.TrimSpace(accountID) == "" || providerID == "" {
		return
	}
	generation = providerRuntimeGeneration(accountID, generation)
	if generation != 0 && !config.AccountLifecycleMatches(accountID, generation) {
		return
	}
	if duration <= 0 {
		duration = directAnthropicPermissionDenyTTL
	}
	key := accountLifecycleKey{accountID: accountID, generation: generation}
	if generation == 0 {
		key = legacyBedrockLifecycleKey(accountID)
	}
	h.mutateProviderRuntime(func(now time.Time) {
		if h.providerModelDeniedUntil == nil {
			h.providerModelDeniedUntil = make(map[accountLifecycleKey]map[string]time.Time)
		}
		byModel := h.providerModelDeniedUntil[key]
		if byModel == nil {
			byModel = make(map[string]time.Time)
			h.providerModelDeniedUntil[key] = byModel
		}
		until := now.Add(duration)
		denialKey := exactProviderDenialKey(providerID)
		if current := byModel[denialKey]; current.Before(until) {
			byModel[denialKey] = until
		}
	})
}

func (h *Handler) setProviderModelDeniedForAccount(account *config.Account, model string, duration time.Duration) {
	if account == nil {
		return
	}
	h.setProviderModelDeniedForGeneration(account.ID, account.DispatchGeneration, model, duration)
}

func (h *Handler) setProviderModelDenied(accountID, model string, duration time.Duration) {
	h.setProviderModelDeniedForGeneration(accountID, 0, model, duration)
}

func (h *Handler) setProviderAccountBanForLifecycle(account *config.Account, status, reason string) (bool, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false, nil
	}
	if account.DispatchGeneration != 0 {
		return config.SetAccountBanStatusIfLifecycle(
			account.ID,
			account.DispatchGeneration,
			status,
			reason,
		)
	}
	if err := config.SetAccountBanStatus(account.ID, status, reason); err != nil {
		return false, err
	}
	return true, nil
}

func (h *Handler) handleDirectAnthropicDiscoveryFailure(account *config.Account, err error) {
	if h == nil || account == nil || err == nil {
		return
	}
	var responseErr *directAnthropicResponseError
	if errors.As(err, &responseErr) {
		// Listing permission/not-found failures are endpoint/resource scoped. They
		// do not prove that the API key cannot call /v1/messages for a known model.
		switch responseErr.Scope {
		case directAnthropicErrorCredential,
			directAnthropicErrorBilling,
			directAnthropicErrorRateLimit,
			directAnthropicErrorTransient:
			h.handleDirectAnthropicFailure(account, "", responseErr)
		}
		return
	}
	// Non-response discovery failures are transport, malformed-response, or
	// pagination failures. They receive a short bounded cooldown. Account add and
	// update validate the endpoint before it can enter this runtime path.
	h.setProviderCooldownForAccount(account, directAnthropicTransientCooldown)
}

func (h *Handler) handleDirectAnthropicFailure(account *config.Account, model string, responseErr *directAnthropicResponseError) {
	if h == nil || account == nil || responseErr == nil {
		return
	}
	switch responseErr.Scope {
	case directAnthropicErrorCredential:
		lifecycleScoped := account.DispatchGeneration != 0
		applied, err := h.setProviderAccountBanForLifecycle(account, "BANNED", "Anthropic API key is invalid, revoked, or expired")
		if err != nil {
			logger.Warnf("[DirectAnthropic] failed to disable invalid credential account=%s: %v", account.ID, err)
			return
		}
		if !applied {
			return
		}
		if lifecycleScoped {
			h.clearAccountProviderRuntimeStateForLifecycle(account)
		} else {
			h.removeAccountProviderRuntimeState(account.ID)
		}
		if h.pool != nil {
			h.pool.Reload()
		}
		// Runtime cleanup happens only after the lifecycle CAS publishes. A failed
		// config write therefore cannot unnecessarily discard still-current state,
		// and exact-key cleanup cannot erase a recreated N+1 account.
	case directAnthropicErrorBilling:
		lifecycleScoped := account.DispatchGeneration != 0
		applied, err := h.setProviderAccountBanForLifecycle(account, "BILLING_BLOCKED", "Anthropic billing or payment is unavailable")
		if err != nil {
			logger.Warnf("[DirectAnthropic] failed to mark billing-blocked account=%s: %v", account.ID, err)
			return
		}
		if !applied {
			return
		}
		if lifecycleScoped {
			h.clearAccountProviderRuntimeStateForLifecycle(account)
		} else {
			h.removeAccountProviderRuntimeState(account.ID)
		}
		if h.pool != nil {
			h.pool.Reload()
		}
	case directAnthropicErrorPermission, directAnthropicErrorNotFound:
		// A 403 can be workspace, beta, or server-tool scoped, while 404 can
		// indicate an endpoint-path error. The caller already excludes this
		// account for the current request; persist a model denial only after a
		// separate model-discovery check provides model-level evidence.
		logger.Warnf(
			"[DirectAnthropic] request-local provider exclusion account=%s model=%s scope=%d request_id=%s",
			account.ID,
			model,
			responseErr.Scope,
			responseErr.RequestID,
		)
	case directAnthropicErrorRateLimit:
		delay := responseErr.RetryAfter
		if delay <= 0 {
			delay = directAnthropicDefaultRateLimit
		}
		h.setProviderCooldownForAccount(account, delay)
	case directAnthropicErrorTransient:
		h.setProviderCooldownForAccount(account, directAnthropicTransientCooldown)
	}
}
