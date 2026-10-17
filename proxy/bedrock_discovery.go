package proxy

// Bedrock model auto-discovery.
//
// Discovery lists the region's on-demand-invocable catalog:
//
//	ListFoundationModels (kept: ACTIVE text models whose inferenceTypesSupported
//	includes ON_DEMAND) UNION ListInferenceProfiles (the callable cross-region
//	profile ids), deduped and sorted.
//
// inferenceTypesSupported is trusted static metadata: it drops PROVISIONED-only and
// INFERENCE_PROFILE-only BARE ids (both 400 on a bare invoke) while the profile ids
// bring the INFERENCE_PROFILE models back under their callable us./eu. id.
//
// We deliberately do NOT gate on GetFoundationModelAvailability. It was tried and
// removed: availability is REGION- and FIELD-misleading. agreementAvailability,
// entitlementAvailability and regionAvailability all report AVAILABLE in regions
// where the very same key gets HTTP 400 "Operation not allowed" on invoke (model
// access is granted per-region and the availability API does not reflect it). The
// only ground truth for "callable" is an actual invoke, which the request path
// already surfaces as a pre-stream failover (and 429s feed the adaptive throttle).
// So discovery lists what the region offers on-demand and lets invoke be the judge.
//
// Results are cached per account so the hot path (resolveBedrockModelID) and
// apiGetAccountModels read the cache, never the network. Auth (SigV4 access key or
// a Bedrock API key bearer token) is attached by authorizeBedrockRequest.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// bedrockDiscoveryTTL bounds how long a discovered callable-model set is reused.
const bedrockDiscoveryTTL = 30 * time.Minute

// bedrockDiscoveryNegativeTTL is the (short) TTL for a failed/empty discovery so a
// transient control-plane error recovers quickly rather than being pinned.
const bedrockDiscoveryNegativeTTL = 2 * time.Minute

// bedrockControlPlaneTimeout bounds a single control-plane request so a hung
// endpoint cannot pin an HTTP handler for the invoke client's long timeout.
const bedrockControlPlaneTimeout = 20 * time.Second

// bedrockControlPlaneBase is the control-plane host (NOT bedrock-runtime).
func bedrockControlPlaneBase(region string) string {
	return fmt.Sprintf("https://bedrock.%s.amazonaws.com", region)
}

// --- response shapes (subset) ----------------------------------------------

type bedrockFoundationModelSummary struct {
	ModelID          string   `json:"modelId"`
	ProviderName     string   `json:"providerName"`
	OutputModalities []string `json:"outputModalities"`
	ModelLifecycle   struct {
		Status string `json:"status"`
	} `json:"modelLifecycle"`
	InferenceTypesSupported []string `json:"inferenceTypesSupported"`
}

type bedrockListModelsResponse struct {
	ModelSummaries []bedrockFoundationModelSummary `json:"modelSummaries"`
}

type bedrockInferenceProfilesResponse struct {
	InferenceProfileSummaries []struct {
		InferenceProfileID string `json:"inferenceProfileId"`
		Status             string `json:"status"`
	} `json:"inferenceProfileSummaries"`
	NextToken string `json:"nextToken"`
}

// --- pure helpers (unit-tested) --------------------------------------------

// onDemandTextModelIDs returns the ids of ACTIVE, text-output foundation models
// that are callable on-demand via their BARE foundation id — i.e. those whose
// inferenceTypesSupported includes "ON_DEMAND".
//
// Two deliberate exclusions (both "listed but unusable" as a bare id):
//   - INFERENCE_PROFILE-only models: the bare foundation id returns HTTP 400
//     "on-demand throughput isn't supported, use an inference profile" on invoke.
//     The callable us./eu. profile id for these is added separately by the caller
//     from ListInferenceProfiles, so the model is still reachable — just not under
//     its bare id.
//   - PROVISIONED-only models: not invocable on-demand at all.
//
// This filter reads inferenceTypesSupported, which is static metadata from
// ListFoundationModels itself — NOT the GetFoundationModelAvailability probe, which
// this gateway reports unreliably (NOT_AUTHORIZED for models that invoke fine). A
// model with an empty/absent inferenceTypesSupported is kept (unknown, don't hide).
func onDemandTextModelIDs(resp bedrockListModelsResponse) []string {
	var ids []string
	for _, m := range resp.ModelSummaries {
		if m.ModelLifecycle.Status != "ACTIVE" {
			continue
		}
		isText := false
		for _, o := range m.OutputModalities {
			if o == "TEXT" {
				isText = true
				break
			}
		}
		if !isText {
			continue
		}
		// Keep only bare ids callable on-demand. Empty list = unknown, keep.
		if len(m.InferenceTypesSupported) > 0 {
			onDemand := false
			for _, t := range m.InferenceTypesSupported {
				if t == "ON_DEMAND" {
					onDemand = true
					break
				}
			}
			if !onDemand {
				continue
			}
		}
		ids = append(ids, m.ModelID)
	}
	return ids
}

// inferenceProfileIDs returns the invocable inference-profile ids (e.g.
// us.anthropic.claude-..., us.amazon.nova-...) from a ListInferenceProfiles
// response — these are the ids that work for on-demand cross-region invoke of
// models that don't support a bare foundation-id call.
func inferenceProfileIDs(resp bedrockInferenceProfilesResponse) []string {
	var ids []string
	for _, p := range resp.InferenceProfileSummaries {
		if p.Status != "" && p.Status != "ACTIVE" {
			continue
		}
		if p.InferenceProfileID != "" {
			ids = append(ids, p.InferenceProfileID)
		}
	}
	return ids
}

// dedupeSortedModelIDs unions foundation ids and inference-profile ids into one
// deduped, sorted list. Pure and unit-tested.
func dedupeSortedModelIDs(foundation, profiles []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range append(append([]string{}, foundation...), profiles...) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// --- per-account cache ------------------------------------------------------

type bedrockDiscoveryEntry struct {
	models  []string
	expires time.Time
	failure string
}

var (
	bedrockDiscoveryMu    sync.RWMutex
	bedrockDiscoveryCache = map[accountLifecycleKey]bedrockDiscoveryEntry{}
)

// getCachedBedrockModelsResult returns a defensive copy and preserves whether a
// negative-cache entry represents a discovery failure. An empty successful list
// is different from a transport/permission failure and must not replace a stale
// provider snapshot as if it were freshly verified.
func getCachedBedrockModelsResultForAccount(account *config.Account) ([]string, bool, error) {
	key := lifecycleOrLegacyKeyForAccount(account)
	if key.accountID == "" {
		return nil, false, nil
	}
	bedrockDiscoveryMu.RLock()
	defer bedrockDiscoveryMu.RUnlock()
	e, ok := bedrockDiscoveryCache[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false, nil
	}
	models := append([]string(nil), e.models...)
	if strings.TrimSpace(e.failure) != "" {
		return models, true, errors.New(e.failure)
	}
	return models, true, nil
}

// getCachedBedrockModels is the resolver-facing compatibility view. Failed
// negative-cache entries remain cached (so the hot path does not retry network)
// but expose no provider evidence.
func getCachedBedrockModelsForAccount(account *config.Account) ([]string, bool) {
	models, cached, _ := getCachedBedrockModelsResultForAccount(account)
	return models, cached
}

func setCachedBedrockModelsForAccount(account *config.Account, models []string) {
	setCachedBedrockModelsTTLForAccount(account, models, bedrockDiscoveryTTL)
}

// clearBedrockModelCache drops any cached discovery for an account so the next
// lookup re-discovers. Used by the explicit "refresh models" admin action.
func clearBedrockModelCache(accountID string) {
	bedrockDiscoveryMu.Lock()
	for key := range bedrockDiscoveryCache {
		if key.accountID == accountID {
			delete(bedrockDiscoveryCache, key)
		}
	}
	bedrockDiscoveryMu.Unlock()
	bedrockDiscoveryInflight.Range(func(rawKey, _ any) bool {
		if key, ok := rawKey.(accountLifecycleKey); ok && key.accountID == accountID {
			bedrockDiscoveryInflight.Delete(key)
		}
		return true
	})
}

func clearBedrockModelCacheForLifecycle(account *config.Account) bool {
	cleared, _ := clearBedrockModelCacheForLifecycleContext(context.Background(), account)
	return cleared
}

// setCachedBedrockModelsTTL stores a COPY of models with an explicit TTL (a short
// TTL is used for negative results so a transient control-plane failure recovers
// quickly instead of being pinned for the full success TTL).
func setCachedBedrockModelsTTLForAccount(account *config.Account, models []string, ttl time.Duration) {
	key := lifecycleOrLegacyKeyForAccount(account)
	if !lifecycleRuntimeKeyCanPublish(key) {
		return
	}
	bedrockDiscoveryMu.Lock()
	defer bedrockDiscoveryMu.Unlock()
	bedrockDiscoveryCache[key] = bedrockDiscoveryEntry{
		models:  append([]string(nil), models...),
		expires: time.Now().Add(ttl),
	}
}

func setCachedBedrockDiscoveryFailureForAccount(account *config.Account, err error, ttl time.Duration) {
	message := "bedrock model discovery failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	key := lifecycleOrLegacyKeyForAccount(account)
	if !lifecycleRuntimeKeyCanPublish(key) {
		return
	}
	bedrockDiscoveryMu.Lock()
	defer bedrockDiscoveryMu.Unlock()
	bedrockDiscoveryCache[key] = bedrockDiscoveryEntry{
		expires: time.Now().Add(ttl),
		failure: message,
	}
}

// Legacy ID-only cache helpers are retained for existing tests/admin seams. They
// intentionally use generation 0, a namespace that request dispatch never uses.
// Request-derived state must go through the *ForAccount variants above so stale
// completions cannot publish into a recreated account with the same textual ID.
func getCachedBedrockModelsResult(accountID string) ([]string, bool, error) {
	if accountID == "" {
		return nil, false, nil
	}
	bedrockDiscoveryMu.RLock()
	defer bedrockDiscoveryMu.RUnlock()
	e, ok := bedrockDiscoveryCache[legacyBedrockLifecycleKey(accountID)]
	if !ok || time.Now().After(e.expires) {
		return nil, false, nil
	}
	models := append([]string(nil), e.models...)
	if strings.TrimSpace(e.failure) != "" {
		return models, true, errors.New(e.failure)
	}
	return models, true, nil
}

func getCachedBedrockModels(accountID string) ([]string, bool) {
	models, cached, _ := getCachedBedrockModelsResult(accountID)
	return models, cached
}

func setCachedBedrockModels(accountID string, models []string) {
	setCachedBedrockModelsTTL(accountID, models, bedrockDiscoveryTTL)
}

func setCachedBedrockModelsTTL(accountID string, models []string, ttl time.Duration) {
	if accountID == "" {
		return
	}
	bedrockDiscoveryMu.Lock()
	bedrockDiscoveryCache[legacyBedrockLifecycleKey(accountID)] = bedrockDiscoveryEntry{
		models:  append([]string(nil), models...),
		expires: time.Now().Add(ttl),
	}
	bedrockDiscoveryMu.Unlock()
}

func setCachedBedrockDiscoveryFailure(accountID string, err error, ttl time.Duration) {
	if accountID == "" {
		return
	}
	message := "bedrock model discovery failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	bedrockDiscoveryMu.Lock()
	bedrockDiscoveryCache[legacyBedrockLifecycleKey(accountID)] = bedrockDiscoveryEntry{
		expires: time.Now().Add(ttl),
		failure: message,
	}
	bedrockDiscoveryMu.Unlock()
}

// --- network discovery ------------------------------------------------------

// signedBedrockControlGet performs a SigV4-signed GET against a control-plane path
// (e.g. "/foundation-models") and returns the status and body.
func (h *Handler) signedBedrockControlGet(account *config.Account, path string) (int, []byte, error) {
	return h.signedBedrockControlGetContext(context.Background(), account, path)
}

func (h *Handler) signedBedrockControlGetContext(ctx context.Context, account *config.Account, path string) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	region := bedrockRegionFor(account)
	req, err := newBedrockRequestForURL(bedrockControlPlaneBase(region)+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Method = http.MethodGet
	req.Body = nil
	req.ContentLength = 0
	req.Header.Set("Accept", "application/json")
	// Bound the call so a hung control plane cannot pin the caller for the invoke
	// client's long timeout, while still inheriting Handler/request cancellation.
	opCtx, cancel := context.WithTimeout(ctx, bedrockControlPlaneTimeout)
	defer cancel()
	req = req.WithContext(opCtx)
	// GET has no body: authorize over the empty payload (bearer token or SigV4).
	if err := authorizeBedrockRequest(account, req, []byte{}, region); err != nil {
		return 0, nil, err
	}
	resp, err := bedrockHTTPClientFor(account).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, readErr := readBedrockControlPlaneBody(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, fmt.Errorf("bedrock control-plane HTTP %d response body: %w", resp.StatusCode, readErr)
	}
	return resp.StatusCode, body, nil
}

// discoverBedrockModels returns the on-demand catalog for an account's region:
// ACTIVE text foundation models that are on-demand-invocable as a bare id, unioned
// with every inference-profile id, deduped and sorted. It does NOT probe
// GetFoundationModelAvailability — availability reports AVAILABLE even in regions
// where the same key gets HTTP 400 "Operation not allowed" on invoke, so it cannot
// gate callability (see the file header). Actual callability is decided at invoke
// time by the request path's failover. Discovery performs one foundation list plus
// a paginated ListInferenceProfiles enumeration. Missing profile-list permission
// degrades to foundation-only evidence; partial/transient profile enumeration fails
// rather than publishing an incomplete catalog. Runs on cold cache / explicit refresh
// only (30-min success TTL). A discovery error lets the caller retain stale/static
// evidence instead of treating partial results as fresh.
func (h *Handler) discoverBedrockModels(account *config.Account) ([]string, error) {
	return h.discoverBedrockModelsContext(context.Background(), account)
}

func (h *Handler) discoverBedrockModelsContext(ctx context.Context, account *config.Account) ([]string, error) {
	evidence, err := h.discoverBedrockModelEvidenceContext(ctx, account)
	if err != nil {
		return nil, err
	}
	return evidence.models, nil
}

// cachedOrDiscoverBedrockModelsResult returns the cached/discovered callable set
// and preserves discovery failures so provider snapshot freshness cannot be
// renewed by a negative cache entry.
func (h *Handler) cachedOrDiscoverBedrockModelsResult(account *config.Account) ([]string, error) {
	return h.cachedOrDiscoverBedrockModelsResultContext(context.Background(), account)
}

func (h *Handler) cachedOrDiscoverBedrockModelsResultContext(ctx context.Context, account *config.Account) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil {
		return nil, errors.New("bedrock account is nil")
	}
	if models, cached, err := getCachedBedrockModelsResultForAccount(account); cached {
		return models, err
	}

	key := lifecycleOrLegacyKeyForAccount(account)
	release, err := acquireBedrockDiscoveryGateContext(ctx, key)
	if err != nil {
		return nil, err
	}
	defer release()
	if models, cached, err := getCachedBedrockModelsResultForAccount(account); cached {
		return models, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	evidence, err := h.discoverBedrockModelEvidenceContext(ctx, account)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		logger.Warnf("[Bedrock] model discovery failed for account %s: %v", account.ID, err)
		setCachedBedrockDiscoveryFailureForAccount(account, err, bedrockDiscoveryNegativeTTL)
		return nil, err
	}
	// Cancellation is not provider evidence. Re-check immediately before cache
	// publication so a cancel that races with the final successful response cannot
	// turn into a fresh success entry.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	setCachedBedrockModelsTTLForAccount(account, evidence.models, evidence.cacheTTL)
	if evidence.profilesRestricted {
		logger.Warnf("[Bedrock] inference-profile listing denied for account %s; caching foundation-only discovery for %s", account.ID, evidence.cacheTTL)
	}
	logger.Infof("[Bedrock] discovered %d models for account %s in %s (%d on-demand text foundation + %d inference profiles)",
		len(evidence.models), account.ID, bedrockRegionFor(account), evidence.foundationCount, evidence.profileCount)
	return evidence.models, nil
}

// cachedOrDiscoverBedrockModels is retained for resolver/admin callers that only
// need the current evidence set. Snapshot publication uses the error-aware form.
func (h *Handler) cachedOrDiscoverBedrockModels(account *config.Account) []string {
	return h.cachedOrDiscoverBedrockModelsContext(context.Background(), account)
}

func (h *Handler) cachedOrDiscoverBedrockModelsContext(ctx context.Context, account *config.Account) []string {
	models, _ := h.cachedOrDiscoverBedrockModelsResultContext(ctx, account)
	return models
}

// bedrockDiscoveryInflight holds a per-account mutex used to collapse concurrent
// cold-cache discovery into a single control-plane fan-out.
var bedrockDiscoveryInflight sync.Map // accountLifecycleKey -> *sync.Mutex

func bedrockDiscoveryLockForAccount(account *config.Account) *sync.Mutex {
	key := lifecycleOrLegacyKeyForAccount(account)
	v, _ := bedrockDiscoveryInflight.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func bedrockDiscoveryLock(accountID string) *sync.Mutex {
	key := legacyBedrockLifecycleKey(accountID)
	v, _ := bedrockDiscoveryInflight.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// --- resolver integration ---------------------------------------------------

// discoveredBedrockModelFor looks up a client-requested model against an account's
// cached callable set: an exact id match, or an alias match where the requested
// token is a suffix of exactly one callable id (e.g. "claude-haiku-4-5" ->
// "anthropic.claude-haiku-4-5-20251001-v1:0"). Returns "" when the cache is empty
// or the match is ambiguous. Reads the cache only — never the network.
func discoveredBedrockModelForAccount(account *config.Account, requested string) string {
	models, ok := getCachedBedrockModelsForAccount(account)
	if !ok {
		return ""
	}
	return discoveredBedrockModelFromList(models, requested)
}

// bedrockModelCore reduces a Bedrock model id to its model-name core by stripping
// the leading region/vendor prefix (everything through the last ".") and the
// trailing version/date suffix. Examples:
//
//	anthropic.claude-haiku-4-5-20251001-v1:0 -> claude-haiku-4-5
//	us.amazon.nova-lite-v1:0                 -> nova-lite
//	meta.llama3-8b-instruct-v1:0             -> llama3-8b-instruct

// discoveredBedrockModelFor is the legacy generation-0 resolver used by older
// tests/admin code. Request routing uses discoveredBedrockModelForAccount.
func discoveredBedrockModelFor(accountID, requested string) string {
	models, ok := getCachedBedrockModels(accountID)
	if !ok {
		return ""
	}
	return discoveredBedrockModelFromList(models, requested)
}

func discoveredBedrockModelFromList(models []string, requested string) string {
	for _, m := range models {
		if m == requested {
			return m
		}
	}
	var matches []string
	for _, m := range models {
		if bedrockModelCore(m) == requested {
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func bedrockModelCore(id string) string {
	core := id
	if k := strings.LastIndex(core, "."); k >= 0 {
		core = core[k+1:]
	}
	// Cut at a "-YYYYMMDD" date segment if present, else at a "-vN" version segment.
	if i := indexDateSegment(core); i >= 0 {
		return core[:i]
	}
	if i := indexVersionSegment(core); i >= 0 {
		return core[:i]
	}
	return core
}

// indexDateSegment returns the index of a "-" that begins an 8-digit date segment
// (e.g. "-20251001"), or -1.
func indexDateSegment(s string) int {
	for i := 0; i+9 <= len(s); i++ {
		if s[i] != '-' {
			continue
		}
		allDigits := true
		for j := i + 1; j < i+9; j++ {
			if s[j] < '0' || s[j] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return i
		}
	}
	return -1
}

// indexVersionSegment returns the index of a "-" that begins a "vN" version
// segment (e.g. "-v1", "-v2:0"), or -1.
func indexVersionSegment(s string) int {
	for i := 0; i+3 <= len(s); i++ {
		if s[i] == '-' && s[i+1] == 'v' && s[i+2] >= '0' && s[i+2] <= '9' {
			return i
		}
	}
	return -1
}
