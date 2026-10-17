package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/diagnostics"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

const (
	microsoftProfileSelectionTTL          = 10 * time.Minute
	microsoftMaxPendingProfileSelections  = 64
	microsoftCanceledSessionTTL           = 10 * time.Minute
	microsoftMaxCanceledSessionTombstones = 128
	microsoftProfileDiscoveryTimeout      = 30 * time.Second
	minWarmModelsCacheInterval            = 30 * time.Second
)

var errStaleAccountSelection = errors.New("selected account lifecycle changed during request")

func promptCacheAccountKey(account *config.Account) string {
	if account == nil {
		return ""
	}
	generation := account.DispatchGeneration
	if generation == 0 {
		generation, _ = config.GetAccountLifecycleGeneration(account.ID)
	}
	// Length-prefix the exact textual ID so leading/trailing whitespace remains
	// significant and no delimiter choice can collapse two configured identities.
	return fmt.Sprintf("%d:%s:%d", len(account.ID), account.ID, generation)
}

// RequestLog stores details about a single API request (success or failure).
type RequestLog struct {
	Time      int64   `json:"time"`               // Unix timestamp
	Endpoint  string  `json:"endpoint"`           // claude/openai/responses
	Model     string  `json:"model"`              // Requested model
	AccountID string  `json:"accountId"`          // Account used
	ApiKeyID  string  `json:"apiKeyId,omitempty"` // API key entry that made the request (empty on legacy/unauthenticated paths)
	Status    string  `json:"status"`             // "success" or "error"
	Error     string  `json:"error"`              // Error message (empty on success)
	ErrorType string  `json:"errorType"`          // Error category (empty on success)
	Tokens    int     `json:"tokens"`             // Total tokens (input+output, 0 on failure)
	Credits   float64 `json:"credits"`            // Credits consumed (0 on failure)
	Duration  int64   `json:"duration"`           // Request duration in ms
}

const requestLogsMaxSize = 500

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// 模型缓存
	cachedModels         []ModelInfo
	modelsCacheMu        sync.RWMutex
	modelsCacheTime      int64
	modelsWarming        int32
	lastWarmAttemptNanos int64

	// Provider runtime state is kept separately from Kiro's legacy string-based
	// account failure classifier. It owns per-account model snapshots, dynamic
	// cooldowns, and model-scoped permission denials for terminal providers.
	providerStateMu           sync.RWMutex
	providerRebuildMu         sync.Mutex
	providerRegistry          atomic.Pointer[providerRegistrySnapshot]
	providerRegistryVersion   atomic.Uint64
	providerExpiryMu          sync.Mutex
	providerExpiryAt          time.Time
	providerExpiryWake        chan struct{}
	providerExpiryStop        chan struct{}
	accountModelSnapshots     map[accountLifecycleKey]accountModelSnapshot
	providerCooldownUntil     map[accountLifecycleKey]time.Time
	providerModelDeniedUntil  map[accountLifecycleKey]map[string]time.Time
	providerSourceGenerations map[accountLifecycleKey]uint64

	// backgroundTaskOwner is set only for Handler instances whose dynamic
	// goroutines are owned by a real/test lifecycle and therefore can be joined.
	// Zero-value/manual fixtures stay synchronous unless they explicitly opt in.
	backgroundTaskOwner bool
	backgroundWG        sync.WaitGroup
	backgroundStartMu   sync.Mutex
	backgroundCancels   map[uint64]context.CancelFunc
	backgroundNextID    uint64
	closeOnce           sync.Once
	closed              atomic.Bool
	shutdownDone        chan struct{}

	promptCache        *promptCacheTracker
	credentialImportMu sync.Mutex
	// Process-local observed reconciler state for the live Accounts UI. This is
	// intentionally separate from config.json: it contains ephemeral scheduling
	// and in-flight state, not desired configuration.
	accountRuntimeMu       sync.RWMutex
	accountRuntimeInstance string
	accountRuntimeRevision uint64
	accountRuntimeAccounts []accountRuntimeView
	// 请求日志 (环形缓冲区，包含成功和失败)
	requestLogs   []RequestLog
	requestLogsMu sync.RWMutex
	// custom_api (pool-linking) real-cost billing: tracks the last known upstream
	// creditsUsed per account so each forwarded request is billed the real delta
	// the upstream pool deducted, not a flat token-derived price.
	customApiLedger *customApiCreditLedger
	// proxyRotator rotates the global outbound proxy through a configured pool on a
	// timer (round-robin). Inert when no pool is configured.
	proxyRotator *proxyRotator

	// Microsoft Enterprise SSO state is Handler-owned and canceled/scrubbed on shutdown.
	microsoftSelections   map[string]*microsoftProfileSelection
	microsoftSelectionsMu sync.Mutex
	microsoftFlowMu       sync.Mutex
	microsoftCanceled     map[string]time.Time
	microsoftDiscoveries  map[string]*microsoftProfileDiscovery
}

type microsoftProfileSelection struct {
	SessionID string
	Account   config.Account
	Profiles  []KiroProfile
	ExpiresAt time.Time
	timer     *time.Timer
	mu        sync.Mutex
	canceled  atomic.Bool
}

type microsoftProfileDiscovery struct {
	cancel context.CancelFunc
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, _ int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		// Model-specific budget semantics are validated by Kiro. Claude Code and
		// older compatible clients may still send enabled+budget_tokens, while
		// adaptive-only models need that legacy shape mapped without the budget.
		if thinking.BudgetTokens < 0 {
			return "thinking.budget_tokens must not be negative"
		}
	case "adaptive", "disabled", "between_tools":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is only supported when thinking.type is enabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled, between_tools"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" && display != "updates" {
		return "thinking.display must be one of: summarized, omitted, updates"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

// validateClaudeThinkingForCapability applies manual-budget rules only when
// the selected model directly supports thinking.type=enabled. Claude Code may
// send the legacy enabled shape even when the effective model is adaptive-only;
// that case is normalized to adaptive later and must not be rejected here.
func validateClaudeThinkingForCapability(
	thinking *ClaudeThinkingConfig,
	maxTokens int,
	hasTools bool,
	interleavedThinking bool,
	capability ModelReasoningCapability,
) string {
	if thinking == nil ||
		!strings.EqualFold(strings.TrimSpace(thinking.Type), "enabled") {
		return ""
	}

	_, supportsEnabled := matchThinkingType("enabled", capability.ThinkingTypes)
	_, supportsAdaptive := matchThinkingType("adaptive", capability.ThinkingTypes)

	if capability.SupportsThinking && !supportsEnabled && supportsAdaptive {
		// Compatibility mapping: enabled -> adaptive; budget_tokens is omitted.
		return ""
	}

	// Unknown capability and models that directly support enabled retain the
	// manual-thinking contract.
	if thinking.BudgetTokens <= 0 {
		return "thinking.budget_tokens is required when thinking.type is enabled for this model"
	}
	if thinking.BudgetTokens < 1024 {
		return "thinking.budget_tokens must be at least 1024"
	}
	if maxTokens == 0 {
		return "thinking.type enabled cannot be used with max_tokens=0"
	}
	// budget_tokens may exceed max_tokens only when interleaved thinking is
	// actually active. Merely attaching tools is insufficient: manual-thinking
	// models require the interleaved-thinking beta and Haiku does not support it.
	if maxTokens > 0 && thinking.BudgetTokens >= maxTokens &&
		!(hasTools && interleavedThinking) {
		return "thinking.budget_tokens must be less than max_tokens"
	}
	return ""
}

func claudeManualInterleavedThinkingEnabledForBetas(
	betas claudeBetaSet,
	_ string,
	thinking *ClaudeThinkingConfig,
	capability ModelReasoningCapability,
) bool {
	if thinking == nil ||
		!strings.EqualFold(strings.TrimSpace(thinking.Type), "enabled") {
		return false
	}
	if !capability.SupportsInterleavedThinking {
		return false
	}
	_, supportsEnabled := matchThinkingType("enabled", capability.ThinkingTypes)
	if capability.SupportsThinking && !supportsEnabled {
		return false
	}
	return betas.Contains("interleaved-thinking-2025-05-14")
}

func claudeManualInterleavedThinkingEnabled(
	r *http.Request,
	model string,
	thinking *ClaudeThinkingConfig,
	capability ModelReasoningCapability,
) bool {
	if r == nil {
		return false
	}
	betas := newClaudeBetaSet()
	betas.AddValues(r.Header.Values("Anthropic-Beta"))
	return claudeManualInterleavedThinkingEnabledForBetas(
		betas,
		model,
		thinking,
		capability,
	)
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(
	thinking *ClaudeThinkingConfig,
	defaultFormat string,
	model string,
	capability ModelReasoningCapability,
) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}

	display := ""
	if thinking != nil {
		display = strings.ToLower(strings.TrimSpace(thinking.Display))
	}
	if display == "" {
		if claudeDefaultThinkingDisplay(model, capability) != "omitted" {
			return opts
		}
		display = "omitted"
	}

	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:                      pool.GetPool(),
		totalRequests:             int64(totalReq),
		successRequests:           int64(successReq),
		failedRequests:            int64(failedReq),
		totalTokens:               int64(totalTokens),
		totalCredits:              totalCredits,
		startTime:                 time.Now().Unix(),
		stopRefresh:               make(chan struct{}),
		stopStatsSaver:            make(chan struct{}),
		promptCache:               newPromptCacheTracker(defaultPromptCacheTTL),
		customApiLedger:           newCustomApiCreditLedger(),
		accountModelSnapshots:     make(map[accountLifecycleKey]accountModelSnapshot),
		providerCooldownUntil:     make(map[accountLifecycleKey]time.Time),
		providerModelDeniedUntil:  make(map[accountLifecycleKey]map[string]time.Time),
		providerSourceGenerations: make(map[accountLifecycleKey]uint64),
		providerExpiryWake:        make(chan struct{}, 1),
		providerExpiryStop:        make(chan struct{}),
		backgroundTaskOwner:       true,
		shutdownDone:              make(chan struct{}),
		accountRuntimeInstance:    uuid.NewString(),
		microsoftSelections:       make(map[string]*microsoftProfileSelection),
		microsoftCanceled:         make(map[string]time.Time),
		microsoftDiscoveries:      make(map[string]*microsoftProfileDiscovery),
	}
	// 启动时应用代理配置
	// Outbound proxy: a rotator applies the global proxy, cycling a pool when one is
	// configured. When no pool is set it just applies the single ProxyURL once.
	h.proxyRotator = newProxyRotator(applyProxyConfig)
	h.proxyRotator.configure(config.GetProxyURL(), config.GetProxyURLs(), config.GetProxyRotateMinutes())

	cachePath := filepath.Join(config.GetConfigDir(), "prompt_cache.json")
	h.promptCache.Load(cachePath)
	h.promptCache.startSaveLoop(cachePath, 30*time.Second)
	// Start every handler-owned background task under one lifecycle group.
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		h.runProviderExpiryLoop()
	}()
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		h.backgroundRefresh()
	}()
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		h.backgroundStatsSaver()
	}()
	h.backgroundWG.Add(1)
	go func() {
		defer h.backgroundWG.Done()
		purgeExpiredResponses(responsesDefaultTTL)
	}()
	// Phase 2 made the import watcher lifecycle-aware; start it only after the
	// Handler lifecycle group is fully initialized.
	h.startImportWatcher()
	return h
}

func (h *Handler) cancelMicrosoftFlowsForShutdown() {
	if h == nil {
		return
	}
	var discoveries []*microsoftProfileDiscovery
	h.microsoftFlowMu.Lock()
	for sessionID, discovery := range h.microsoftDiscoveries {
		if discovery != nil {
			discoveries = append(discoveries, discovery)
		}
		delete(h.microsoftDiscoveries, sessionID)
	}
	h.microsoftFlowMu.Unlock()
	for _, discovery := range discoveries {
		discovery.cancel()
	}

	var selections []*microsoftProfileSelection
	h.microsoftSelectionsMu.Lock()
	for selectionID, selection := range h.microsoftSelections {
		delete(h.microsoftSelections, selectionID)
		if selection == nil {
			continue
		}
		selection.canceled.Store(true)
		if selection.timer != nil {
			selection.timer.Stop()
			selection.timer = nil
		}
		selections = append(selections, selection)
	}
	h.microsoftSelectionsMu.Unlock()
	discardDetachedMicrosoftProfileSelections(selections)
}

func (h *Handler) startBackgroundTask(fn func()) bool {
	if h == nil || fn == nil {
		return false
	}
	return h.startBackgroundTaskContext(func(context.Context) { fn() })
}

func (h *Handler) startBackgroundTaskContext(fn func(context.Context)) bool {
	if h == nil || fn == nil {
		return false
	}
	h.backgroundStartMu.Lock()
	if h.closed.Load() || !h.backgroundTaskOwner {
		h.backgroundStartMu.Unlock()
		return false
	}
	if h.backgroundCancels == nil {
		h.backgroundCancels = make(map[uint64]context.CancelFunc)
	}
	h.backgroundNextID++
	taskID := h.backgroundNextID
	taskCtx, cancel := context.WithCancel(context.Background())
	h.backgroundCancels[taskID] = cancel
	h.backgroundWG.Add(1)
	h.backgroundStartMu.Unlock()

	go func() {
		defer h.backgroundWG.Done()
		defer func() {
			cancel()
			h.backgroundStartMu.Lock()
			delete(h.backgroundCancels, taskID)
			h.backgroundStartMu.Unlock()
		}()
		fn(taskCtx)
	}()
	return true
}

// Shutdown stops handler-owned background work. Process-global resources such as
// AccountPool are closed by the application root after the HTTP server drains.
// Shutdown initiation is idempotent, while each caller may wait with its own
// deadline and retry later if an earlier deadline expires.
func (h *Handler) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.closeOnce.Do(func() {
		h.backgroundStartMu.Lock()
		h.closed.Store(true)
		cancels := make([]context.CancelFunc, 0, len(h.backgroundCancels))
		for _, cancel := range h.backgroundCancels {
			cancels = append(cancels, cancel)
		}
		h.backgroundStartMu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		h.cancelMicrosoftFlowsForShutdown()
		if h.shutdownDone == nil {
			h.shutdownDone = make(chan struct{})
		}
		if h.stopRefresh != nil {
			close(h.stopRefresh)
		}
		if h.stopStatsSaver != nil {
			close(h.stopStatsSaver)
		}
		if h.providerExpiryStop != nil {
			close(h.providerExpiryStop)
		}
		if h.promptCache != nil {
			h.promptCache.Stop()
		}
		if h.proxyRotator != nil {
			h.proxyRotator.stopRotation()
		}

		done := h.shutdownDone
		go func() {
			h.backgroundWG.Wait()
			close(done)
		}()
	})

	select {
	case <-h.shutdownDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close is retained for compatibility with tests and embedders that do not need
// a deadline. It never closes the process-global account pool.
func (h *Handler) Close() {
	_ = h.Shutdown(context.Background())
}

// backgroundRefresh owns the two independent refresh schedules introduced by
// the account reconciler. Account credential/info work runs on its short scan
// cadence while model discovery keeps the historical 30-minute cadence. The
// parent waits for both children so Handler.Shutdown retains one lifecycle gate.
func (h *Handler) backgroundRefresh() {
	if h.stopRefresh == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.backgroundAccountReconciler(ctx)
	}()
	go func() {
		defer wg.Done()
		h.backgroundModelRefresh(ctx)
	}()

	<-h.stopRefresh
	cancel()
	wg.Wait()
}

// refreshAllAccounts 刷新所有账户信息
type accountRefreshMode uint8

const (
	accountRefreshBackground accountRefreshMode = iota
	accountRefreshExplicit
)

type accountRefreshOutcome struct {
	Info    interface{}
	Message string
}

type accountTokenRefreshError struct {
	err error
}

func (e *accountTokenRefreshError) Error() string {
	if e == nil || e.err == nil {
		return "token refresh failed"
	}
	return "token refresh failed: " + e.err.Error()
}

func (e *accountTokenRefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// refreshAccountByKind is the single account-lifecycle dispatcher used by
// background, single-account, and batch refresh flows. Terminal/static provider
// credentials never fall through to Kiro OAuth, account-info, or quota APIs.
func (h *Handler) refreshAccountByKind(
	ctx context.Context,
	account *config.Account,
	mode accountRefreshMode,
) (*accountRefreshOutcome, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	switch {
	case account.IsBedrock():
		return &accountRefreshOutcome{Message: "Bedrock account; nothing to refresh"}, nil

	case account.IsDirectAnthropic():
		if mode == accountRefreshBackground {
			return &accountRefreshOutcome{Message: "Direct Anthropic uses a static API key"}, nil
		}
		if err := h.fetchAndCacheAccountModelsContext(ctx, account); err != nil {
			return nil, err
		}
		return &accountRefreshOutcome{Message: "Direct Anthropic model list refreshed"}, nil

	case account.IsCustomApi():
		outcome, err := h.refreshObservedAccountState(ctx, account)
		if err != nil {
			return nil, err
		}
		if mode == accountRefreshExplicit {
			if err := h.fetchAndCacheAccountModelsContext(ctx, account); err != nil {
				logger.Warnf("[Refresh] custom_api model reload failed for %s: %v", account.ID, err)
			}
		}
		return outcome, nil
	}

	if !account.SupportsKiroAccountInfo() {
		return nil, fmt.Errorf("account kind %q does not support Kiro account refresh", account.AuthMethod)
	}
	return h.refreshObservedAccountState(ctx, account)
}

func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled || !account.HasKnownKind() {
			continue
		}
		switch account.Kind() {
		case config.AccountKindDirectAnthropic, config.AccountKindBedrock:
			// Static terminal providers have no periodic account-info lifecycle.
			continue
		case config.AccountKindCustomRelay:
			if !accountReadyForModelDiscovery(account) {
				continue
			}
		default:
			if strings.TrimSpace(account.AccessToken) == "" &&
				strings.TrimSpace(account.KiroApiKey) == "" &&
				strings.TrimSpace(account.RefreshToken) == "" {
				continue
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		outcome, err := h.refreshAccountByKind(ctx, account, accountRefreshBackground)
		cancel()
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			// Preserve the previous policy: only an actual OAuth refresh failure
			// enters account-health classification. Account-info probe failures are
			// logged and retried on the next cycle without an account-wide penalty.
			var tokenErr *accountTokenRefreshError
			if errors.As(err, &tokenErr) {
				h.handleAccountFailure(account, err)
			}
			continue
		}
		if outcome != nil && outcome.Info != nil {
			logger.Infof("[BackgroundRefresh] Refreshed %s", account.Email)
		}
	}
	h.pool.Reload()
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, anthropic-user-profile-id, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", strings.Join([]string{"x-request-id", "request-id", "retry-after", "x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "anthropic-ratelimit-requests-limit", "anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-requests-reset", "anthropic-ratelimit-input-tokens-limit", "anthropic-ratelimit-input-tokens-remaining", "anthropic-ratelimit-input-tokens-reset", "anthropic-ratelimit-output-tokens-limit", "anthropic-ratelimit-output-tokens-remaining", "anthropic-ratelimit-output-tokens-reset", "anthropic-ratelimit-tokens-limit", "anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-reset"}, ", "))

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 客户自助端点（用客户自己的 API Key 鉴权，只暴露该 Key 的数据）
	// 只读自省端点：同时接受 GET 与 POST，方便机器人用任一动词查询（POST 请求体忽略）。
	case path == "/api/stats" && (r.Method == "GET" || r.Method == "POST"):
		h.handleCustomerStats(w, r)
	case path == "/api/me" && (r.Method == "GET" || r.Method == "POST"):
		h.handleCustomerMe(w, r)
	case path == "/api/logs" && (r.Method == "GET" || r.Method == "POST"):
		h.handleCustomerLogs(w, r)

	// 机器集成管理端点（Telegram 机器人等；管理密钥鉴权）。
	// 必须放在通用 /admin/ 静态文件路由之前，否则会被误当作静态资源。
	case path == "/admin/new_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminNewApiKey(w, r)
	case path == "/admin/delete_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminDeleteApiKey(w, r)
	case path == "/admin/recharge_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminRechargeApiKey(w, r)
	case path == "/admin/stats" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminBotStats(w, r)
	case path == "/admin/pool" && r.Method == "GET":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminPool(w, r)
	case path == "/admin/add_kiro_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddKiroApiKey(w, r)
	case path == "/admin/add_kiro_account" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddKiroAccount(w, r)
	case path == "/admin/add_custom_api_account" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddCustomApiAccount(w, r)
	case path == "/admin/add_bedrock_account" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddBedrockAccount(w, r)

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"cache":           h.promptCache.Stats(),
		// Customer-safe latency distribution (no account identities): with session
		// affinity on, warm accounts pull the mean/min down over time.
		"dispatchLatency": h.pool.LatencyAggregate(),
		"sessionAffinity": config.GetSessionAffinityEnabled(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	h.rebuildEnabledModelUnion()
	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		h.refreshModelsCacheContext(ctx)
		cancel()
		h.modelsCacheMu.RLock()
		cached = h.cachedModels
		h.modelsCacheMu.RUnlock()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	// The public view is generated from the same immutable registry generation
	// used for account routing. Logical aliases and -thinking variants are only
	// advertised when at least one current candidate can serve them.
	registry := h.currentProviderRegistry()
	models := buildProviderRegistryModelsResponse(registry, cached, thinkingSuffix)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

func buildProviderRegistryModelsResponse(registry *providerRegistrySnapshot, cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if registry == nil {
		return buildAnthropicModelsResponse(cached, "")
	}
	models := make([]map[string]interface{}, 0, len(registry.PublicModels)*2)
	for _, item := range registry.PublicModels {
		owner := "anthropic"
		if item.Alias {
			owner = "kiro-proxy"
		}
		supportsImage := modelSupportsImage(item.Model.InputTypes)
		models = append(models, buildModelInfo(item.Model.ModelId, owner, supportsImage))
		if strings.TrimSpace(thinkingSuffix) != "" && item.AnyThinking && item.AnyFreshCandidate {
			models = append(models, buildModelInfo(item.Model.ModelId+thinkingSuffix, owner, supportsImage))
		}
	}
	return models
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			models = append(models, buildModelInfo(m.ModelId, "anthropic", supportsImage))
			// Advertise the compatibility suffix only when the enabled-account
			// capability intersection proves that thinking is supported.
			if strings.TrimSpace(thinkingSuffix) != "" &&
				resolveClaudeProviderReasoningCapability(m.ModelId).SupportsThinking {
				models = append(models, buildModelInfo(m.ModelId+thinkingSuffix, "anthropic", supportsImage))
			}
		}
	}
	return models
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	return map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}
}

// refreshModelsCache refreshes all enabled account snapshots with a bounded
// background context. Request handlers use refreshModelsCacheContext directly.
func (h *Handler) refreshModelsCache() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	h.refreshModelsCacheContext(ctx)
}

func (h *Handler) refreshModelsCacheContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	accounts := config.GetEnabledAccounts()
	for i := range accounts {
		if err := ctx.Err(); err != nil {
			logger.Warnf("[ModelsCache] refresh cancelled: %v", err)
			break
		}
		account := &accounts[i]
		if !account.HasKnownKind() {
			logger.Warnf("[ModelsCache] skipping account=%s with unknown authMethod=%q", account.ID, account.AuthMethod)
			continue
		}
		if err := h.fetchAndCacheAccountModelsContext(ctx, account); err != nil {
			logger.Warnf("[ModelsCache] account=%s model refresh failed: %v", account.ID, err)
			if account.SupportsKiroAccountInfo() {
				h.handleAccountFailure(account, err)
			}
		}
	}
	// Always rebuild, even when no provider returned models. This removes models
	// from disabled/deleted accounts and prevents stale merge-only advertisements.
	h.rebuildEnabledModelUnion()

	h.modelsCacheMu.RLock()
	count := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	logger.Infof("[ModelsCache] Rebuilt enabled-account union with %d models", count)
}

// fetchAndCacheAccountModels is retained for background/legacy callers. Request
// handlers should use fetchAndCacheAccountModelsContext so cancellation reaches
// the provider probe.
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	return h.fetchAndCacheAccountModelsContext(context.Background(), account)
}

// fetchAndCacheAccountModelsContext replaces the complete per-account model
// snapshot on success. It never merge-appends into the global list; the global
// union is rebuilt from enabled-account snapshots so removed models disappear.
func (h *Handler) fetchAndCacheAccountModelsContext(ctx context.Context, account *config.Account) error {
	if account == nil {
		return errors.New("account is nil")
	}
	// Admin/create call sites can hold a pre-dispatch account copy with generation
	// 0. Stamp the current lifecycle once before deriving source fingerprints so
	// the refresh is fenced exactly like request-dispatched snapshots. Never
	// rewrite a non-zero generation: it may intentionally represent stale N.
	if account.DispatchGeneration == 0 {
		if generation, ok := config.GetAccountLifecycleGeneration(account.ID); ok {
			stamped := account.Clone()
			stamped.DispatchGeneration = generation
			account = &stamped
		}
	}
	if !account.HasKnownKind() {
		return fmt.Errorf("unknown account auth method %q", account.AuthMethod)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sourceFingerprint := accountModelSourceFingerprint(account)
	sourceGeneration := h.providerSourceGeneration(account)

	if account.IsBedrock() {
		ids, discoveryErr := h.cachedOrDiscoverBedrockModelsResultContext(ctx, account)
		explicitIDs := make([]string, 0, len(account.BedrockModelMap))
		for _, value := range account.BedrockModelMap {
			if value = strings.TrimSpace(value); value != "" {
				explicitIDs = mergeStringLists(explicitIDs, []string{value})
			}
		}
		if discoveryErr != nil {
			h.markAccountModelSnapshotStale(account, sourceFingerprint, sourceGeneration)
			h.rebuildEnabledModelUnion()
			// Explicit operator mappings remain usable even when the control-plane
			// list permission is unavailable. Do not replace a still-routable stale
			// discovery snapshot; it already includes the same configured targets.
			if len(explicitIDs) > 0 && !h.accountModelSnapshotRoutableForSource(
				account, sourceFingerprint, sourceGeneration, time.Now(),
			) {
				models := make([]ModelInfo, 0, len(explicitIDs))
				for _, id := range explicitIDs {
					models = append(models, ModelInfo{ModelId: id, ModelName: id})
				}
				if !h.replaceAccountModelSnapshot(account, sourceFingerprint, sourceGeneration, models, nil, explicitIDs) {
					return errAccountModelSourceChanged
				}
			}
			return discoveryErr
		}
		ids = mergeStringLists(ids, explicitIDs)
		models := make([]ModelInfo, 0, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id != "" {
				models = append(models, ModelInfo{ModelId: id, ModelName: id})
			}
		}
		if !h.replaceAccountModelSnapshot(account, sourceFingerprint, sourceGeneration, models, nil, ids) {
			return errAccountModelSourceChanged
		}
		return nil
	}

	var (
		models   []ModelInfo
		metadata map[string]directAnthropicModelMetadata
		err      error
	)
	switch account.Kind() {
	case config.AccountKindDirectAnthropic:
		models, metadata, err = probeDirectAnthropicModelsDetailed(ctx, account.BaseURL, account.KiroApiKey)
	case config.AccountKindCustomRelay:
		models, err = probeCustomApiModelsWithContext(ctx, account.BaseURL, account.KiroApiKey)
	case config.AccountKindKiroOAuth, config.AccountKindKiroAPIKey:
		if err = h.ensureValidTokenContext(ctx, account); err == nil {
			models, err = ListAvailableModelsContext(ctx, account)
		}
	default:
		err = fmt.Errorf("account kind %s does not support model discovery", account.Kind())
	}
	if err != nil {
		h.markAccountModelSnapshotStale(account, sourceFingerprint, sourceGeneration)
		h.rebuildEnabledModelUnion()
		if account.IsDirectAnthropic() && ctx.Err() == nil {
			h.handleDirectAnthropicDiscoveryFailure(account, err)
		}
		return err
	}

	modelIDs := make([]string, 0, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ModelId); id != "" {
			modelIDs = append(modelIDs, id)
		}
	}
	if !h.replaceAccountModelSnapshot(account, sourceFingerprint, sourceGeneration, models, metadata, modelIDs) {
		return errAccountModelSourceChanged
	}
	logger.Infof("[ModelsCache] Replaced %d models for account %s", len(models), account.ID)
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 refreshModelsCache 逻辑一致）
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	// An explicit refresh should re-run Bedrock discovery, not reuse the cache, and
	// re-learn which region each model is callable in.
	if account.IsBedrock() {
		if _, err := clearBedrockModelCacheForLifecycleContext(r.Context(), account); err != nil {
			if r.Context().Err() != nil {
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		clearBedrockRegionRoutesForLifecycle(account)
		bedrockThrottle.clearForLifecycle(account)
	}
	if err := h.fetchAndCacheAccountModelsContext(r.Context(), account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if account.IsBedrock() {
		// Prewarm only after foreground discovery has published a complete accepted
		// catalog. This prevents a canceled/failed refresh from launching duplicate
		// discovery or paid canary work against partial evidence.
		acc := account.Clone()
		h.startBackgroundTaskContext(func(ctx context.Context) { h.prewarmBedrockRegionsContext(ctx, &acc) })
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 refreshModelsCache，为所有已启用账号刷新模型路由缓存。
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	h.refreshModelsCacheContext(r.Context())
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    0,
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	if len(base.AdditionalModelRequestFieldsSchema) == 0 &&
		len(extra.AdditionalModelRequestFieldsSchema) > 0 {
		base.AdditionalModelRequestFieldsSchema =
			append(
				json.RawMessage(nil),
				extra.AdditionalModelRequestFieldsSchema...,
			)
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func (h *Handler) warmModelsCacheAsync() {
	if h == nil || h.closed.Load() {
		return
	}
	if last := atomic.LoadInt64(&h.lastWarmAttemptNanos); last != 0 &&
		time.Since(time.Unix(0, last)) < minWarmModelsCacheInterval {
		return
	}
	if !atomic.CompareAndSwapInt32(&h.modelsWarming, 0, 1) {
		return
	}
	atomic.StoreInt64(&h.lastWarmAttemptNanos, time.Now().UnixNano())
	if !h.startBackgroundTaskContext(func(taskCtx context.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Errorf("[ModelsCache] warm goroutine panicked: %v", recovered)
			}
		}()
		defer atomic.StoreInt32(&h.modelsWarming, 0)
		ctx, cancel := context.WithTimeout(taskCtx, 2*time.Minute)
		defer cancel()
		h.refreshModelsCacheContext(ctx)
	}) {
		atomic.StoreInt32(&h.modelsWarming, 0)
	}
}

// reasoningBuildErrorIsFatal is retained for upstream regression/source compatibility.
// Candidate-specific WIP routing is authoritative and does not use this helper as
// a pre-dispatch hard gate.
func (h *Handler) reasoningBuildErrorIsFatal() bool {
	return h.hasKiroCapableAccount()
}

// Read reasoning capabilities from the cached model schema.
func (h *Handler) reasoningCapabilityForModel(modelID string) ModelReasoningCapability {
	h.rebuildEnabledModelUnion()
	normalizedID := strings.ToLower(
		strings.TrimSpace(MapModel(modelID)),
	)

	h.modelsCacheMu.RLock()
	models := append([]ModelInfo(nil), h.cachedModels...)
	h.modelsCacheMu.RUnlock()

	if len(models) == 0 {
		// Never turn a request hot path into a synchronous Kiro-account fan-out.
		// The request planner may still choose a direct/Bedrock provider while this
		// best-effort cache warm runs in the Handler lifecycle group.
		h.warmModelsCacheAsync()
	}

	for _, model := range models {
		currentID := strings.ToLower(
			strings.TrimSpace(model.ModelId),
		)
		if currentID != normalizedID {
			continue
		}
		capability := ParseModelReasoningCapability(model)
		// The Kiro schema advertises thinking and budget fields, but not whether
		// the manual interleaved-thinking beta has an effect. Preserve that
		// model-specific capability from the conservative built-in table.
		capability.SupportsInterleavedThinking =
			fallbackClaudeReasoningCapability(modelID).SupportsInterleavedThinking

		if diagnostics.Reasoning() && capability.SchemaParseError != "" {
			logger.Warnf(
				"[KiroReasoning] model=%s schema parse failed: %s",
				modelID,
				capability.SchemaParseError,
			)
		}

		return capability
	}

	// No candidate-local Kiro model schema means the Kiro adapter capability is
	// unknown. Do not borrow the static Claude capability table here: this helper
	// is queried before account selection, and doing so would let a direct/Bedrock
	// model capability authorize fields for an unrelated Kiro credential.
	return ModelReasoningCapability{}
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, ok := h.readLimitedJSONBody(w, r, requestBodyErrorClaude)
	if !ok {
		return
	}

	plan, planErr := h.buildClaudeRequestPlan(body, r.Header)
	if planErr != nil {
		h.sendClaudeValidationError(w, http.StatusBadRequest, planErr)
		return
	}
	// Count the canonical provider request. This endpoint must not use Kiro's
	// additionalModelRequestFieldsSchema as the source of Claude model validity.
	effectiveReq := &plan.Request

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func claudeThinkingType(req *ClaudeRequest) string {
	if req == nil || req.Thinking == nil {
		return ""
	}

	return strings.TrimSpace(req.Thinking.Type)
}

func claudeThinkingBudget(req *ClaudeRequest) int {
	if req == nil || req.Thinking == nil {
		return 0
	}

	return req.Thinking.BudgetTokens
}

func claudeReasoningEffort(req *ClaudeRequest) string {
	if req == nil {
		return ""
	}

	if req.OutputConfig != nil {
		if effort := strings.TrimSpace(req.OutputConfig.Effort); effort != "" {
			return effort
		}
	}

	return strings.TrimSpace(req.ReasoningEffort)
}

func claudeThinkingDisplay(req *ClaudeRequest) string {
	if req == nil || req.Thinking == nil {
		return ""
	}
	return strings.TrimSpace(req.Thinking.Display)
}

// logClaudeThinkingInbound records the exact reasoning controls received from
// the Claude client before model alias resolution or enabled -> adaptive mapping.
func logClaudeThinkingInbound(endpoint string, req *ClaudeRequest) {
	if !diagnostics.Reasoning() || req == nil {
		return
	}

	logger.Infof(
		"[ClaudeThinkingInbound] endpoint=%s model=%q stream=%t type=%q display=%q budgetTokens=%d effort=%q tools=%d",
		endpoint,
		req.Model,
		req.Stream,
		claudeThinkingType(req),
		claudeThinkingDisplay(req),
		claudeThinkingBudget(req),
		claudeReasoningEffort(req),
		len(req.Tools),
	)
}

// claudeThinkingRoundTripStats summarizes the most recent assistant thinking
// block and the following tool-result turn without logging opaque signatures.
type claudeThinkingRoundTripStats struct {
	LastAssistantFound bool
	ThinkingBlocks     int
	SignatureCount     int
	SignatureTotalLen  int
	SignatureHashes    []string
	RedactedBlocks     int
	ToolUseBlocks      int
	ToolResultBlocks   int
}

func shortThinkingSignatureHash(signature string) string {
	if signature == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(signature))
	return fmt.Sprintf("%x", sum[:8])
}

func inspectClaudeThinkingRoundTrip(messages []ClaudeMessage) claudeThinkingRoundTripStats {
	var stats claudeThinkingRoundTripStats

	lastAssistantIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			lastAssistantIndex = i
			stats.LastAssistantFound = true
			break
		}
	}

	if lastAssistantIndex >= 0 {
		for _, block := range claudeContentBlocks(messages[lastAssistantIndex].Content) {
			blockType, _ := block["type"].(string)
			switch blockType {
			case "thinking":
				stats.ThinkingBlocks++
				if signature, _ := block["signature"].(string); signature != "" {
					stats.SignatureCount++
					stats.SignatureTotalLen += len(signature)
					stats.SignatureHashes = append(
						stats.SignatureHashes,
						shortThinkingSignatureHash(signature),
					)
				}
			case "redacted_thinking":
				stats.RedactedBlocks++
			case "tool_use":
				stats.ToolUseBlocks++
			}
		}
	}

	for i := lastAssistantIndex + 1; i < len(messages); i++ {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		for _, block := range claudeContentBlocks(messages[i].Content) {
			if blockType, _ := block["type"].(string); blockType == "tool_result" {
				stats.ToolResultBlocks++
			}
		}
	}

	return stats
}

func claudeContentBlocks(content interface{}) []map[string]interface{} {
	switch blocks := content.(type) {
	case []interface{}:
		result := make([]map[string]interface{}, 0, len(blocks))
		for _, raw := range blocks {
			if block, ok := raw.(map[string]interface{}); ok {
				result = append(result, block)
			}
		}
		return result
	case []map[string]interface{}:
		return blocks
	case []ClaudeContentBlock:
		result := make([]map[string]interface{}, 0, len(blocks))
		for _, block := range blocks {
			encoded, err := json.Marshal(block)
			if err != nil {
				continue
			}
			var decoded map[string]interface{}
			if json.Unmarshal(encoded, &decoded) == nil {
				result = append(result, decoded)
			}
		}
		return result
	default:
		return nil
	}
}

func resolveClaudeEffectiveThinking(
	req *ClaudeRequest,
	explicitOrSuffixThinking bool,
	nativeRequested bool,
	capability ModelReasoningCapability,
) bool {
	if req != nil && req.Thinking != nil {
		switch strings.ToLower(strings.TrimSpace(req.Thinking.Type)) {
		case "disabled", "none":
			return false
		case "enabled", "adaptive":
			return true
		}
	}

	if explicitOrSuffixThinking {
		return true
	}
	// output_config.effort is a sampling/reasoning control, not a request for
	// visible thinking blocks. Response shape changes only when the client asks
	// for thinking (or the selected provider semantics require it).
	if nativeRequested {
		return true
	}

	model := ""
	if req != nil {
		model = req.Model
	}
	return claudeThinkingEnabledByDefault(model, capability)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, ok := h.readLimitedJSONBody(w, r, requestBodyErrorClaude)
	if !ok {
		return
	}

	invocationBody := append([]byte(nil), body...)
	plan, planErr := h.buildClaudeRequestPlan(body, r.Header)
	if planErr != nil {
		h.sendClaudeValidationError(w, http.StatusBadRequest, planErr)
		return
	}
	req := plan.Request
	body = plan.Semantic.Body
	semanticRequest := plan.Semantic
	applyClaudeSemanticHeaders(r.Header, semanticRequest.Headers)

	logClaudeThinkingInbound("messages", &req)

	var roundTripStats claudeThinkingRoundTripStats
	if diagnostics.Reasoning() {
		roundTripStats = inspectClaudeThinkingRoundTrip(req.Messages)
		if roundTripStats.ThinkingBlocks > 0 ||
			roundTripStats.RedactedBlocks > 0 ||
			roundTripStats.ToolResultBlocks > 0 {

			logger.Infof(
				"[ClaudeThinkingRoundTrip] lastAssistant=%t thinkingBlocks=%d signatureCount=%d signatureTotalLen=%d signatureHashes=%v redactedBlocks=%d toolUseBlocks=%d toolResultBlocks=%d",
				roundTripStats.LastAssistantFound,
				roundTripStats.ThinkingBlocks,
				roundTripStats.SignatureCount,
				roundTripStats.SignatureTotalLen,
				roundTripStats.SignatureHashes,
				roundTripStats.RedactedBlocks,
				roundTripStats.ToolUseBlocks,
				roundTripStats.ToolResultBlocks,
			)
		}
	}

	thinkingCfg := config.GetThinkingConfig()
	requestedModel := plan.RequestedModel
	actualModel := plan.ResolvedModel
	legacyOrClientThinking := plan.ThinkingRequested
	transportRequirements := plan.Requirements
	kiroProjection := plan.KiroProjection
	providerCapability := plan.Capability
	kiroCapability := h.reasoningCapabilityForModel(actualModel)

	// Protocol/model validation uses provider capability, never Kiro's adapter
	// schema. Unknown aliases remain transparent-upstream validated.
	if plan.ProviderCapabilityKnown {
		_, supportsManualThinking := matchThinkingType("enabled", providerCapability.ThinkingTypes)
		_, supportsAdaptiveThinking := matchThinkingType("adaptive", providerCapability.ThinkingTypes)
		if msg := completionGuardValidateThinkingToolChoice(
			req.Thinking,
			req.ToolChoice,
			supportsManualThinking,
			supportsAdaptiveThinking,
		); msg != "" {
			h.sendClaudeError(w, 400, "invalid_request_error", msg)
			return
		}
	}

	// This aggregate Kiro capability is only a diagnostics/payload preview. The
	// authoritative additionalModelRequestFields are rebuilt later from the exact
	// selected account/model route, so one credential can never authorize another.
	additionalFields, nativeRequested, kiroPreviewErr := BuildClaudeAdditionalModelRequestFields(&req, kiroCapability)
	if kiroPreviewErr != nil {
		additionalFields = nil
		if diagnostics.Reasoning() {
			logger.Infof(
				"[KiroAdapterPreview] model=%s authoritative=false error=%q",
				actualModel,
				kiroPreviewErr.Error(),
			)
		}
	}
	if compatibilityErr := claudeKiroContentCompatibilityError(plan.Content); compatibilityErr != nil {
		transportRequirements.Require(
			"kiro_content_adapter",
			claudeTransportMaskAll&^claudeTransportMaskKiro,
			compatibilityErr.Error(),
		)
	}
	kiroProjection = transportRequirements.KiroProjection
	if transportRequirements.Allowed == 0 {
		h.sendClaudeErrorWithDetailCode(
			w,
			http.StatusBadRequest,
			"invalid_request_error",
			"No configured Claude transport can represent the request: "+transportRequirements.Summary(),
			"unsupported_transport_feature",
		)
		return
	}

	// Effective response thinking derives from client/provider intent. The
	// aggregate Kiro adapter preview is not an authoritative capability source.
	thinking := resolveClaudeEffectiveThinking(
		&req,
		legacyOrClientThinking,
		false,
		providerCapability,
	)

	if diagnostics.Reasoning() {
		logger.Infof(
			"[ClaudeThinking] requested=%t type=%q display=%q budgetTokens=%d effort=%q requestedModel=%q actualModel=%q",
			thinking,
			claudeThinkingType(&req),
			claudeThinkingDisplay(&req),
			claudeThinkingBudget(&req),
			claudeReasoningEffort(&req),
			requestedModel,
			actualModel,
		)
		logger.Infof(
			"[KiroReasoningPreview] model=%s authoritative=false requested=%t schemaPath=%s supportsThinking=%t thinkingTypes=%v thinkingDisplays=%v supportedEfforts=%v fields=%s",
			actualModel,
			nativeRequested,
			kiroCapability.EffortPath,
			kiroCapability.SupportsThinking,
			kiroCapability.ThinkingTypes,
			kiroCapability.ThinkingDisplays,
			kiroCapability.Efforts,
			reasoningFieldsJSON(additionalFields),
		)
	}

	if diagnostics.Reasoning() && kiroProjection.Fidelity != claudeProjectionExact {
		logger.Infof(
			"[ClaudeKiroProjection] fidelity=%s summary=%q",
			kiroProjection.Fidelity.String(),
			kiroProjection.Summary(),
		)
	}

	// Legacy prompting is private Kiro-Go suffix compatibility only. Explicit
	// Claude reasoning controls are candidate-validated and must never fall back
	// to a prompt merely because the aggregate capability preview is narrower.
	useLegacyThinkingPrompt := plan.LegacySuffixOnly && transportRequirements.AllowsKiro()

	effectiveReq := cloneClaudeRequestForThinking(&req, useLegacyThinkingPrompt)

	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(
		req.Thinking,
		thinkingCfg.ClaudeFormat,
		actualModel,
		providerCapability,
	)

	routing := claudeRoutingContext{
		Semantic:          semanticRequest,
		Requirements:      transportRequirements,
		RequestedModel:    stripRequestedModelThinkingSuffix(requestedModel, thinkingCfg.Suffix),
		InvocationBody:    invocationBody,
		InvocationHeaders: cloneAnthropicSemanticHeaders(semanticRequest.Headers),
	}
	r = r.WithContext(withClaudeRoutingContext(r.Context(), routing))

	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)

	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	guardPlan := buildAgentCompletionGuardPlanForModelWithCatalogAndCapabilities(
		&req,
		thinking,
		actualModel,
		plan.Catalog,
		plan.Capabilities,
	)
	if guardPlan.ValidationReason == "" {
		guardPlan.ValidationReason = completionGuardValidateSemanticHeaders(
			guardPlan.Catalog,
			r.Header,
		)
	}
	if guardPlan.Enabled &&
		!guardPlan.ObserveOnly &&
		guardPlan.ValidationReason != "" {
		h.sendClaudeError(
			w,
			http.StatusBadRequest,
			"invalid_request_error",
			guardPlan.ValidationReason,
		)
		return
	}

	if guardPlan.Enabled {
		if guardPlan.ObserveOnly {
			logger.Infof(
				"[CompletionGuardObserved] model=%s stream=%t action=transparent_passthrough",
				req.Model,
				req.Stream,
			)
		} else if transportRequirements.RequiresKiroWebSearchMCP {
			// Native server-side web_search is executed by the selected transport
			// (Kiro MCP or direct Anthropic), not by hidden completion-guard turns.
			logger.Infof(
				"[CompletionGuardBypass] model=%s stream=%t native_web_search=true",
				req.Model,
				req.Stream,
			)
		} else if !transportRequirements.AllowsKiro() {
			// Completion Guard retries through Kiro. A request whose semantics
			// require another transport must be forwarded unchanged instead.
			logger.Infof(
				"[CompletionGuardBypass] model=%s stream=%t requirements=%q",
				req.Model,
				req.Stream,
				transportRequirements.Summary(),
			)
		} else {
			replaySafety, replayReason :=
				completionGuardClaudeProjectionReplaySafety(kiroProjection)
			if replaySafety == completionGuardProjectionReplayUnsafe {
				// Guard retries synthesize private Claude turns. Keep fail-closed
				// behavior for projections whose emulation depends on the exact
				// original history (for example context management). New emulation
				// features also land here until explicitly reviewed.
				logger.Infof(
					"[CompletionGuardBypass] model=%s stream=%t "+
						"kiroProjection=%q replaySafety=%s reason=%s",
					req.Model,
					req.Stream,
					kiroProjection.Summary(),
					replaySafety.String(),
					replayReason,
				)
			} else {
				if replaySafety == completionGuardProjectionReplaySafe {
					logger.Infof(
						"[CompletionGuardProjection] model=%s stream=%t "+
							"replaySafety=%s kiroProjection=%q",
						req.Model,
						req.Stream,
						replaySafety.String(),
						kiroProjection.Summary(),
					)
				}
				h.handleClaudeCompletionGuard(w, r, req, req.Model, thinking, thinkingResponseOpts, useLegacyThinkingPrompt, additionalFields, guardPlan)
				return
			}
		}
	}

	var kiroPayload *KiroPayload
	if transportRequirements.AllowsKiro() && !transportRequirements.RequiresKiroWebSearchMCP {
		var translationErr error
		kiroPayload, translationErr = ClaudeToKiroCheckedWithProjection(
			&req,
			useLegacyThinkingPrompt,
			plan.Content,
			kiroProjection,
		)
		if translationErr != nil {
			h.sendClaudeError(
				w,
				http.StatusInternalServerError,
				"claude_translation_invariant",
				translationErr.Error(),
			)
			return
		}
		kiroPayload.AdditionalModelRequestFields = additionalFields
	}

	if diagnostics.Reasoning() && roundTripStats.SignatureCount > 0 {
		logger.Infof(
			"[KiroReasoningRoundTrip] inboundThinkingBlocks=%d inboundSignatures=%d signatureForwarded=false reason=%q",
			roundTripStats.ThinkingBlocks,
			roundTripStats.SignatureCount,
			"Claude thinking/signature blocks are not serialized into Kiro assistant history",
		)
	}

	// Stream or non-stream
	apiKeyID := apiKeyIDFromContext(r.Context())
	semanticHeaders := cloneAnthropicSemanticHeaders(semanticRequest.Headers)
	// forwarded marks a request that already passed through one Kiro-Go pool, so a
	// custom_api account cannot add another hop (loop guard, see forwardToUpstream).
	forwarded := r.Header.Get(forwardHeader) != ""
	if req.Stream {
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, invocationBody, forwarded, semanticHeaders, r.Context())
	} else {
		h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, invocationBody, forwarded, semanticHeaders, r.Context())
	}
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, rawBody []byte, forwarded bool, semanticHeaders http.Header, requestContext context.Context) {
	routing, routingErr := resolveClaudeRoutingContext(
		requestContext,
		rawBody,
		semanticHeaders,
		model,
	)
	if routingErr != nil {
		errorType := "invalid_request_error"
		var validationErr *claudeTransportValidationError
		if errors.As(routingErr, &validationErr) && validationErr.Code != "" {
			errorType = validationErr.Code
		}
		h.sendClaudeError(w, http.StatusBadRequest, errorType, routingErr.Error())
		return
	}
	semanticHeaders = routing.Semantic.Headers
	rawBody = routing.Semantic.Body

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	reqStart := time.Now()
	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	excluded := make(map[string]bool)
	var lastErr error
	messageStarted := false
	var messageStartUsage promptCacheUsage

	ensureMessageStart := func() {
		if messageStarted {
			return
		}
		h.sendSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		candidate, selectionErr := h.nextCompatibleClaudeCandidate(
			routing.RequestedModel,
			model,
			excluded,
			apiKeyID,
			forwarded,
			routing.Requirements,
			rawBody,
		)
		if selectionErr != nil {
			// Preserve the last concrete upstream/provider response. Selection
			// exhaustion is less informative and must not replace a 401/403/429/5xx
			// response that can still be returned to the client verbatim.
			if lastErr == nil {
				lastErr = selectionErr
			}
			break
		}
		if candidate == nil || candidate.Selection == nil || candidate.Account == nil {
			break
		}
		selection := candidate.Selection
		account := candidate.Account
		providerModelID := string(candidate.ProviderModelID)
		if err := h.ensureValidTokenForSelectionContext(requestContext, selection); err != nil {
			lastErr = err
			if errors.Is(err, errStaleAccountSelection) {
				continue
			}
			excluded[account.ID] = true
			h.handleTokenValidationFailureForSelection(selection, err)
			continue
		}
		if routing.Requirements.RequiresKiroWebSearchMCP && candidate.Transport == claudeTransportKiroNative {
			webErr := h.handleSelectedKiroWebSearch(
				requestContext, w, rawBody, model, estimatedInputTokens, apiKeyID, selection, account,
			)
			if webErr == nil {
				return
			}
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			lastErr = webErr
			if errors.Is(webErr, errStaleAccountSelection) || !h.pool.IsSelectionCurrent(*selection) {
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailureForSelection(*selection, webErr)
			continue
		}
		// Custom API accounts are transparent proxies to another Kiro-Go pool: forward
		// the raw request instead of translating to Kiro. A successful forward ends the
		// request; any pre-reply failure falls over to the next account like a Kiro error.
		if account.IsCustomApi() || claudeAccountIsDirectAnthropic(account) {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded && account.IsCustomApi() {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, flusher, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "anthropic", streaming: true,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				semanticHeaders: semanticHeaders,
				requestContext:  requestContext,
			}); fwdErr != nil {
				if requestContext != nil && requestContext.Err() != nil {
					return
				}
				lastErr = fwdErr
				// The upstream result belongs to the lifecycle captured at dispatch.
				// If that lifecycle was replaced while the request was in flight,
				// retry selection without excluding or mutating the new account.
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true

				var transportErr *directAnthropicTransportError
				if errors.As(fwdErr, &transportErr) {
					h.setProviderCooldownForAccount(account, directAnthropicTransientCooldown)
					continue
				}

				var directErr *directAnthropicResponseError
				if errors.As(fwdErr, &directErr) {
					if !directErr.Retryable {
						h.recordFailureWithDetails("claude", model, account.ID, apiKeyID, directErr)
						writeDirectAnthropicResponseError(w, directErr)
						return
					}
					h.handleDirectAnthropicFailure(account, model, directErr)
					continue
				}

				h.handleAccountFailureForSelection(*selection, fwdErr)
				continue
			}
			return
		}
		// Native Bedrock accounts call the Bedrock Runtime invoke endpoint directly
		// and re-emit the native Anthropic events. Like custom_api this is a
		// transparent passthrough that ends the request on success; a pre-stream
		// failure falls over to the next account.
		if account.IsBedrock() {
			bedrockBody, bodyErr := buildBedrockBodyWithSemanticHeaders(
				rawBody,
				semanticHeaders,
			)
			if bodyErr != nil {
				lastErr = bodyErr
				break
			}
			if accepted, reason := claudeTransportAcceptsBodySize(candidate.Transport, len(bedrockBody)); !accepted {
				lastErr = fmt.Errorf("%w: %s", errClaudeTransportIncompatible, reason)
				excluded[account.ID] = true
				continue
			}
			if bErr := h.invokeBedrockStream(w, flusher, forwardParams{
				selection: selection, account: account, body: bedrockBody, endpoint: "anthropic", streaming: true,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				semanticHeaders: semanticHeaders,
				requestContext:  requestContext,
			}); bErr != nil {
				lastErr = bErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				// A throttle-cooldown skip is advisory (per-model, short); don't
				// escalate it into an account-wide failure/cooldown.
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailureForSelection(*selection, bErr)
				}
				continue
			}
			return
		}
		cacheUsage := h.promptCache.Compute(promptCacheAccountKey(account), cacheProfile)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUseCount int
		var toolTokenCount int
		var nextContentIndex int
		var contentTokenCounter approximateTokenCounter
		var thinkingTokenCounter approximateTokenCounter
		var activeThinkingAccumulator *kiroThinkingSignatureAccumulator
		var activeThinkingSignatureSent bool
		activeBlockIndex := -1
		activeBlockType := ""

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}

			if activeBlockType == "thinking" {
				// If Kiro emitted no provider signature for this block, keep the
				// proxy-owned signature as a configurable compatibility fallback.
				if !activeThinkingSignatureSent {
					signature, synthetic :=
						activeThinkingAccumulator.Resolve("")
					if signature != "" {
						h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
							"type":  "content_block_delta",
							"index": activeBlockIndex,
							"delta": map[string]string{
								"type":      "signature_delta",
								"signature": signature,
							},
						})
					}
					if synthetic && diagnostics.Reasoning() {
						logger.Warnf("[ClaudeThinkingSignatureFallback] model=%s stream=true mode=%s", model, configuredSyntheticThinkingSignatureMode())
					}
				}
				activeThinkingAccumulator = nil
				activeThinkingSignatureSent = false
			}

			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				if activeThinkingAccumulator == nil {
					activeThinkingAccumulator =
						newKiroThinkingSignatureAccumulator(model)
				}
				activeThinkingSignatureSent = false
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				contentTokenCounter.Add(text)
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}
			if text != "" {
				thinkingTokenCounter.Add(text)
			}

			if thinkingFormat != "think" && thinkingFormat != "reasoning_content" {
				if thinkingState == 1 {
					activeThinkingAccumulator =
						newKiroThinkingSignatureAccumulator(model)
				}
				if text != "" && activeThinkingAccumulator != nil {
					activeThinkingAccumulator.WriteString(text)
				}
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		var upstreamStopReason string
		var stopController *claudeKiroStopController
		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				processClaudeText(text, isThinking, false)
			},
			OnReasoningSignature: func(signature string) {
				if diagnostics.Reasoning() && signature != "" {
					logger.Infof(
						"[ClaudeThinkingSignatureOut] model=%s stream=true len=%d hash=%s",
						model,
						len(signature),
						shortThinkingSignatureHash(signature),
					)
				}
				if !thinking || signature == "" {
					return
				}
				// The compatibility text formats intentionally do not expose native
				// Anthropic thinking blocks, so there is nowhere valid to attach a
				// signature_delta in those modes.
				if thinkingFormat == "think" || thinkingFormat == "reasoning_content" {
					return
				}
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				// Kiro does not serialize provider-signed thinking state back into its
				// assistant history. Expose a proxy-owned provenance marker instead so
				// the next tool-result turn stays on the Kiro transport.
				signature, _ = resolveKiroThinkingSignature(model, "", signature)

				// For summarized display, this reuses the already-open thinking block.
				// For omitted display, it creates an empty thinking block first.
				startContentBlock("thinking")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{
						"type":      "signature_delta",
						"signature": signature,
					},
				})
				activeThinkingSignatureSent = true
				closeActiveBlock()
				eventThinkingOpen = false
				thinkingStarted = false
			},
			OnReasoningRedacted: func(data string) {
				if diagnostics.Reasoning() && data != "" {
					logger.Infof(
						"[ClaudeThinkingRedactedOut] model=%s stream=true len=%d hash=%s",
						model,
						len(data),
						shortThinkingSignatureHash(data),
					)
				}
				if !thinking || data == "" {
					return
				}
				if thinkingFormat == "think" || thinkingFormat == "reasoning_content" {
					return
				}
				if !allowReasoningSource(&thinkingSource) {
					return
				}

				ensureMessageStart()
				closeActiveBlock()
				idx := nextContentIndex
				nextContentIndex++
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "redacted_thinking",
						"data": data,
					},
				})
				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
			},
			OnToolUse: func(tu KiroToolUse) {
				processClaudeText("", false, true)
				toolUseCount++
				toolTokenCount += estimateApproxTokens(tu.Name)
				ensureMessageStart()
				closeActiveBlock()

				idx := nextContentIndex
				nextContentIndex++

				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tu.ToolUseID,
						"name":  tu.Name,
						"input": map[string]interface{}{},
					},
				})

				inputJSON, _ := json.Marshal(tu.Input)
				toolTokenCount += estimateJSONTokens(tu.Input)
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})

				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
			},
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = h.usageInputFromContextPercentage(account, providerModelID, model, pct)
			},
		}
		if guardWriter, ok := w.(*completionGuardStreamingWriter); ok {
			callback.ReserveRetained = func(size int) error {
				if err := guardWriter.reserveBufferedBytes(size); err != nil {
					guardWriter.setProtocolViolation("response_too_large", err)
					return err
				}
				return nil
			}
			callback.ReleaseRetained = guardWriter.releaseBufferedBytes
			callback.Err = func() error {
				if guardWriter.protocolErr != nil {
					return guardWriter.protocolErr
				}
				return guardWriter.writeErr
			}
		}

		if payload == nil {
			lastErr = fmt.Errorf("%w: Kiro candidate selected without a Kiro payload", errClaudeTransportIncompatible)
			break
		}
		payload.AdditionalModelRequestFields = candidate.KiroAdditionalModelRequestFields
		if diagnostics.Reasoning() {
			logger.Infof(
				"[KiroCandidateAdapter] account=%s providerModel=%s registry=%d fields=%s",
				account.ID,
				candidate.ProviderModelID,
				candidate.RegistryVersion,
				reasoningFieldsJSON(candidate.KiroAdditionalModelRequestFields),
			)
		}
		stopController = applyClaudeStopSequencesToKiroCallback(callback, routing.Requirements.StopSequences)
		err := CallKiroAPIContext(requestContext, account, payload, callback)
		if err != nil {
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			lastErr = err
			if errors.Is(err, errCompletionGuardBufferLimit) {
				return
			}
			if !h.pool.IsSelectionCurrent(*selection) && !messageStarted {
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailureForSelection(*selection, err)
			if !messageStarted {
				continue
			}
			h.recordFailureWithDetails("claude", model, account.ID, apiKeyID, err)
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": err.Error()},
			})
			return
		}
		stopController.Finish()

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

		if realInputTokens > 0 && inputTokens <= 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Re-anchor the cache split to the real upstream input total so
		// input+creation+read stays consistent (cacheUsage was computed against
		// the pre-call token estimate).
		if cacheProfile != nil && realInputTokens > 0 {
			cacheUsage = cacheUsage.splitAgainstTotal(cacheProfile.TotalInputTokens, inputTokens)
		}
		outputTokens = contentTokenCounter.Tokens() + toolTokenCount
		if thinking {
			outputTokens += thinkingTokenCounter.Tokens()
		}

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccessForSelection(*selection)
		h.pool.RecordLatencyForSelection(*selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*selection, inputTokens+outputTokens, credits)
		h.promptCache.Update(promptCacheAccountKey(account), cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		var stopSequence interface{}
		stopReason := resolveClaudeHandlerStopReason(upstreamStopReason, toolUseCount)
		if matchedStop, ok := stopController.Matched(); ok {
			stopReason = "stop_sequence"
			stopSequence = matchedStop
		}

		if diagnostics.Stream() {
			logger.Infof(
				"[ClaudeStream] completed model=%s account=%s stopReason=%s toolUses=%d outputChars=%d inputTokens=%d outputTokens=%d durationMs=%d",
				model,
				account.Email,
				stopReason,
				toolUseCount,
				contentTokenCounter.runes,
				inputTokens,
				outputTokens,
				time.Since(reqStart).Milliseconds(),
			)
		}

		ensureMessageStart()
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   stopReason,
				"stop_sequence": stopSequence,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	var transportErr *directAnthropicTransportError
	if errors.As(lastErr, &transportErr) {
		h.recordFailureWithDetails("claude", model, "", apiKeyID, transportErr)
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", "Direct Anthropic upstream transport failed")
		return
	}

	var directErr *directAnthropicResponseError
	if errors.As(lastErr, &directErr) {
		h.recordFailureWithDetails("claude", model, "", apiKeyID, directErr)
		writeDirectAnthropicResponseError(w, directErr)
		return
	}

	if errors.Is(lastErr, errClaudeTransportIncompatible) {
		h.sendClaudeErrorWithDetailCode(
			w,
			http.StatusBadRequest,
			"invalid_request_error",
			lastErr.Error(),
			"unsupported_transport_feature",
		)
		return
	}
	if errors.Is(lastErr, errClaudeTransportUnavailable) {
		h.sendClaudeError(
			w,
			http.StatusServiceUnavailable,
			"claude_transport_unavailable",
			lastErr.Error(),
		)
		return
	}
	h.recordFailureWithDetails("claude", model, "", apiKeyID, lastErr)
	h.sendClaudeError(w, 500, "api_error", lastErr.Error())
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(apiKeyID string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

// recordFailureWithDetails records a failure and stores it in the request logs.
// apiKeyID attributes the failed request to the API key entry that issued it so
// customer-facing log endpoints can show per-key failures; empty on legacy paths.
func (h *Handler) recordFailureWithDetails(endpoint, model, accountID, apiKeyID string, err error) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)

	if err == nil {
		return
	}

	errMsg := err.Error()
	errType := classifyError(errMsg)

	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		ApiKeyID:  apiKeyID,
		Status:    "error",
		Error:     errMsg,
		ErrorType: errType,
	}

	h.appendRequestLog(entry)
}

// recordSuccessLog records a successful request in the request logs.
// apiKeyID attributes the request to the API key entry that issued it (see above).
func (h *Handler) recordSuccessLog(endpoint, model, accountID, apiKeyID string, tokens int, credits float64, durationMs int64) {
	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		ApiKeyID:  apiKeyID,
		Status:    "success",
		Tokens:    tokens,
		Credits:   credits,
		Duration:  durationMs,
	}

	h.appendRequestLog(entry)
}

func (h *Handler) appendRequestLog(entry RequestLog) {
	h.requestLogsMu.Lock()
	if h.requestLogs == nil {
		h.requestLogs = make([]RequestLog, 0, requestLogsMaxSize)
	}
	if len(h.requestLogs) >= requestLogsMaxSize {
		h.requestLogs = h.requestLogs[1:]
	}
	h.requestLogs = append(h.requestLogs, entry)
	h.requestLogsMu.Unlock()
}

// classifyError categorizes an error message into a type for display.
func classifyError(msg string) string {
	switch {
	case isQuotaErrorMessage(msg):
		return "quota"
	case isOverageErrorMessage(msg):
		return "overage"
	case isSuspensionErrorMessage(msg):
		return "suspended"
	case pool.IsAuthFailure(errors.New(msg)):
		return "auth"
	case isProfileUnavailableErrorMessage(msg):
		return "profile"
	default:
		return "unknown"
	}
}

// getRequestLogs returns a copy of request logs (newest first).
func (h *Handler) getRequestLogs() []RequestLog {
	h.requestLogsMu.RLock()
	defer h.requestLogsMu.RUnlock()
	if len(h.requestLogs) == 0 {
		return []RequestLog{}
	}
	result := make([]RequestLog, len(h.requestLogs))
	for i, e := range h.requestLogs {
		result[len(h.requestLogs)-1-i] = e
	}
	return result
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, rawBody []byte, forwarded bool, semanticHeaders http.Header, requestContext context.Context) {
	routing, routingErr := resolveClaudeRoutingContext(
		requestContext,
		rawBody,
		semanticHeaders,
		model,
	)
	if routingErr != nil {
		errorType := "invalid_request_error"
		var validationErr *claudeTransportValidationError
		if errors.As(routingErr, &validationErr) && validationErr.Code != "" {
			errorType = validationErr.Code
		}
		h.sendClaudeError(w, http.StatusBadRequest, errorType, routingErr.Error())
		return
	}
	semanticHeaders = routing.Semantic.Headers
	rawBody = routing.Semantic.Body

	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()
	var retainedBudget *completionGuardRetainedBudget
	if provider, ok := w.(completionGuardRetainedBudgetProvider); ok {
		retainedBudget = provider.completionGuardBudget()
	}
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		candidate, selectionErr := h.nextCompatibleClaudeCandidate(
			routing.RequestedModel,
			model,
			excluded,
			apiKeyID,
			forwarded,
			routing.Requirements,
			rawBody,
		)
		if selectionErr != nil {
			// Preserve the last concrete upstream/provider response. Selection
			// exhaustion is less informative and must not replace a 401/403/429/5xx
			// response that can still be returned to the client verbatim.
			if lastErr == nil {
				lastErr = selectionErr
			}
			break
		}
		if candidate == nil || candidate.Selection == nil || candidate.Account == nil {
			break
		}
		selection := candidate.Selection
		account := candidate.Account
		providerModelID := string(candidate.ProviderModelID)
		if err := h.ensureValidTokenForSelectionContext(requestContext, selection); err != nil {
			lastErr = err
			if errors.Is(err, errStaleAccountSelection) {
				continue
			}
			excluded[account.ID] = true
			h.handleTokenValidationFailureForSelection(selection, err)
			continue
		}
		if routing.Requirements.RequiresKiroWebSearchMCP && candidate.Transport == claudeTransportKiroNative {
			webErr := h.handleSelectedKiroWebSearch(
				requestContext, w, rawBody, model, estimatedInputTokens, apiKeyID, selection, account,
			)
			if webErr == nil {
				return
			}
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			lastErr = webErr
			if errors.Is(webErr, errStaleAccountSelection) || !h.pool.IsSelectionCurrent(*selection) {
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailureForSelection(*selection, webErr)
			continue
		}
		// Custom API accounts proxy to another Kiro-Go pool (see handleClaudeStream).
		if account.IsCustomApi() || claudeAccountIsDirectAnthropic(account) {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded && account.IsCustomApi() {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, nil, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "anthropic", streaming: false,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				semanticHeaders: semanticHeaders,
				requestContext:  requestContext,
			}); fwdErr != nil {
				if requestContext != nil && requestContext.Err() != nil {
					return
				}
				lastErr = fwdErr
				// The upstream result belongs to the lifecycle captured at dispatch.
				// If that lifecycle was replaced while the request was in flight,
				// retry selection without excluding or mutating the new account.
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true

				var transportErr *directAnthropicTransportError
				if errors.As(fwdErr, &transportErr) {
					h.setProviderCooldownForAccount(account, directAnthropicTransientCooldown)
					continue
				}

				var directErr *directAnthropicResponseError
				if errors.As(fwdErr, &directErr) {
					if !directErr.Retryable {
						h.recordFailureWithDetails("claude", model, account.ID, apiKeyID, directErr)
						writeDirectAnthropicResponseError(w, directErr)
						return
					}
					h.handleDirectAnthropicFailure(account, model, directErr)
					continue
				}

				h.handleAccountFailureForSelection(*selection, fwdErr)
				continue
			}
			return
		}
		// Native Bedrock non-streaming invoke (see streaming counterpart above).
		if account.IsBedrock() {
			bedrockBody, bodyErr := buildBedrockBodyWithSemanticHeaders(
				rawBody,
				semanticHeaders,
			)
			if bodyErr != nil {
				lastErr = bodyErr
				break
			}
			if accepted, reason := claudeTransportAcceptsBodySize(candidate.Transport, len(bedrockBody)); !accepted {
				lastErr = fmt.Errorf("%w: %s", errClaudeTransportIncompatible, reason)
				excluded[account.ID] = true
				continue
			}
			if bErr := h.invokeBedrockNonStream(w, forwardParams{
				selection: selection, account: account, body: bedrockBody, endpoint: "anthropic", streaming: false,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				semanticHeaders: semanticHeaders,
				requestContext:  requestContext,
			}); bErr != nil {
				lastErr = bErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailureForSelection(*selection, bErr)
				}
				continue
			}
			return
		}
		cacheUsage := h.promptCache.Compute(promptCacheAccountKey(account), cacheProfile)

		var reasoningSignature string
		var toolUses []KiroToolUse
		blockCollector := newClaudeNonStreamBlockCollector(
			model,
			thinkingOpts.OmitDisplay,
			retainedBudget,
		)
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamStopReason string
		var stopController *claudeKiroStopController

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking && !thinking {
					return
				}
				blockCollector.AppendText(text, isThinking)
			},
			OnReasoningSignature: func(signature string) {
				if !thinking {
					return
				}
				blockCollector.AppendSignature(signature)
				reasoningSignature = signature
				if diagnostics.Reasoning() && signature != "" {
					logger.Infof(
						"[ClaudeThinkingSignatureOut] model=%s stream=false len=%d hash=%s",
						model,
						len(signature),
						shortThinkingSignatureHash(signature),
					)
				}
			},
			OnReasoningRedacted: func(data string) {
				if thinking {
					blockCollector.AppendRedacted(data)
				}
				if diagnostics.Reasoning() && data != "" {
					logger.Infof(
						"[ClaudeThinkingRedactedOut] model=%s stream=false len=%d hash=%s",
						model,
						len(data),
						shortThinkingSignatureHash(data),
					)
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				if blockCollector.AppendToolUse(tu) {
					toolUses = append(toolUses, tu)
				}
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = h.usageInputFromContextPercentage(account, providerModelID, model, pct)
			},
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			CanRetry:     func() bool { return true },
			ResetForRetry: func() {
				blockCollector.Release()
				blockCollector = newClaudeNonStreamBlockCollector(model, thinkingOpts.OmitDisplay, retainedBudget)
				reasoningSignature = ""
				toolUses = nil
				inputTokens, outputTokens = 0, 0
				credits = 0
				realInputTokens = 0
				upstreamStopReason = ""
				if stopController != nil {
					stopController.ResetForRetry()
				}
			},
			Err: func() error { return blockCollector.Err() },
		}
		if retainedBudget != nil {
			callback.ReserveRetained = func(size int) error {
				if retainedBudget.reserve(size) {
					return nil
				}
				return retainedBudget.Err()
			}
			callback.ReleaseRetained = retainedBudget.release
		}

		if payload == nil {
			lastErr = fmt.Errorf("%w: Kiro candidate selected without a Kiro payload", errClaudeTransportIncompatible)
			break
		}
		payload.AdditionalModelRequestFields = candidate.KiroAdditionalModelRequestFields
		if diagnostics.Reasoning() {
			logger.Infof(
				"[KiroCandidateAdapter] account=%s providerModel=%s registry=%d fields=%s",
				account.ID,
				candidate.ProviderModelID,
				candidate.RegistryVersion,
				reasoningFieldsJSON(candidate.KiroAdditionalModelRequestFields),
			)
		}
		stopController = applyClaudeStopSequencesToKiroCallback(callback, routing.Requirements.StopSequences)
		err := CallKiroAPIContext(requestContext, account, payload, callback)
		if err != nil {
			blockCollector.Release()
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			lastErr = err
			if errors.Is(err, errCompletionGuardBufferLimit) {
				break
			}
			if !h.pool.IsSelectionCurrent(*selection) {
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailureForSelection(*selection, err)
			continue
		}
		stopController.Finish()

		thinkingFormat := thinkingOpts.Format
		content := blockCollector.Text()
		thinkingContent := blockCollector.RawThinking()
		if collectorErr := blockCollector.Err(); collectorErr != nil {
			lastErr = collectorErr
			blockCollector.Release()
			break
		}
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 && inputTokens <= 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Re-anchor the cache split to the real upstream input total so
		// input+creation+read stays consistent (cacheUsage was computed against
		// the pre-call token estimate).
		if cacheProfile != nil && realInputTokens > 0 {
			cacheUsage = cacheUsage.splitAgainstTotal(cacheProfile.TotalInputTokens, inputTokens)
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccessForSelection(*selection)
		h.pool.RecordLatencyForSelection(*selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*selection, inputTokens+outputTokens, credits)
		h.promptCache.Update(promptCacheAccountKey(account), cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && (rawThinkingContent != "" || reasoningSignature != "")
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		if thinkingOpts.Format == "thinking" && blockCollector.HasNativeReasoning() {
			resp.Content = blockCollector.Blocks()
		} else {
			attachKiroThinkingSignature(resp, model, rawThinkingContent, reasoningSignature)
		}
		resp.StopReason = resolveClaudeHandlerStopReason(upstreamStopReason, len(toolUses))
		if matchedStop, ok := stopController.Matched(); ok {
			resp.StopReason = "stop_sequence"
			resp.StopSequence = &matchedStop
		}
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(resp)
		blockCollector.Release()
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	var transportErr *directAnthropicTransportError
	if errors.As(lastErr, &transportErr) {
		h.recordFailureWithDetails("claude", model, "", apiKeyID, transportErr)
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", "Direct Anthropic upstream transport failed")
		return
	}

	var directErr *directAnthropicResponseError
	if errors.As(lastErr, &directErr) {
		h.recordFailureWithDetails("claude", model, "", apiKeyID, directErr)
		writeDirectAnthropicResponseError(w, directErr)
		return
	}

	if errors.Is(lastErr, errClaudeTransportIncompatible) {
		h.sendClaudeErrorWithDetailCode(
			w,
			http.StatusBadRequest,
			"invalid_request_error",
			lastErr.Error(),
			"unsupported_transport_feature",
		)
		return
	}
	if errors.Is(lastErr, errClaudeTransportUnavailable) {
		h.sendClaudeError(
			w,
			http.StatusServiceUnavailable,
			"claude_transport_unavailable",
			lastErr.Error(),
		)
		return
	}
	h.recordFailureWithDetails("claude", model, "", apiKeyID, lastErr)
	h.sendClaudeError(w, 500, "api_error", lastErr.Error())
}

func (h *Handler) sendClaudeValidationError(
	w http.ResponseWriter,
	status int,
	err *claudeTransportValidationError,
) {
	if err == nil {
		h.sendClaudeError(w, status, "invalid_request_error", "Invalid Claude request")
		return
	}
	h.sendClaudeErrorWithDetailCode(w, status, err.Code, err.Message, err.DetailCode)
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	h.sendClaudeErrorWithDetailCode(w, status, errType, message, "")
}

func (h *Handler) sendClaudeErrorWithDetailCode(
	w http.ResponseWriter,
	status int,
	errType,
	message,
	detailCode string,
) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	errorBody := map[string]interface{}{
		"type":    errType,
		"message": message,
	}
	if strings.TrimSpace(detailCode) != "" {
		errorBody["code"] = detailCode
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":  "error",
		"error": errorBody,
	})
}

type openAIKiroPrepared struct {
	Payload              *KiroPayload
	Projection           openAIProjection
	Thinking             bool
	EstimatedInputTokens int
}

type openAIKiroPrepareFunc func() (*openAIKiroPrepared, error)

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, ok := h.readLimitedJSONBody(w, r, requestBodyErrorOpenAI)
	if !ok {
		return
	}

	// Parse only the routing envelope eagerly. Transport-specific decoding,
	// validation, and conversion are deliberately lazy so a transparent custom
	// relay can receive the original OpenAI request without Kiro/Bedrock DTO
	// semantics rejecting or mutating it first.
	var envelope struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		Stream   bool            `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	var messages []json.RawMessage
	if len(envelope.Messages) == 0 || json.Unmarshal(envelope.Messages, &messages) != nil || len(messages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "messages must be a non-empty array")
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, suffixThinking := ParseModelAndThinking(envelope.Model, thinkingCfg.Suffix)

	var prepared *openAIKiroPrepared
	var prepareErr error
	preparedOnce := false
	prepareKiro := func() (*openAIKiroPrepared, error) {
		if preparedOnce {
			return prepared, prepareErr
		}
		preparedOnce = true

		var localReq OpenAIRequest
		if err := json.Unmarshal(body, &localReq); err != nil {
			prepareErr = fmt.Errorf("decode OpenAI request for Kiro: %w", err)
			return nil, prepareErr
		}
		localReq.Model = actualModel
		if msg := validateOpenAIRequestShape(&localReq); msg != "" {
			prepareErr = errors.New(msg)
			return nil, prepareErr
		}

		projection := analyzeOpenAIChatKiroProjection(body)
		if !projection.Supported() {
			prepareErr = errors.New(projection.Reason())
			return nil, prepareErr
		}

		capability := h.reasoningCapabilityForModel(actualModel)
		analyzeOpenAIKiroReasoningCapabilityProjection(&projection, &localReq, capability)
		if !projection.Supported() {
			prepareErr = errors.New(projection.Reason())
			return nil, prepareErr
		}
		additionalFields, nativeRequested, err := BuildOpenAIAdditionalModelRequestFields(&localReq, capability)
		if err != nil {
			prepareErr = err
			return nil, prepareErr
		}
		thinking := suffixThinking || nativeRequested
		useLegacyThinkingPrompt := thinking && len(additionalFields) == 0
		if useLegacyThinkingPrompt {
			projection.emulate("reasoning", "legacy_thinking_prompt_emulation", "Kiro emulates requested reasoning through an injected priming prompt")
		}
		payload, runtimeProjection := projectOpenAIToKiro(&localReq, useLegacyThinkingPrompt)
		projection.merge(runtimeProjection)
		if !projection.Supported() {
			prepareErr = errors.New(projection.Reason())
			return nil, prepareErr
		}
		payload.AdditionalModelRequestFields = additionalFields
		if !payloadFits(payload, maxPayloadTokens(actualModel)) {
			prepareErr = errors.New("unsupported_transport_semantics: Kiro projection would require history truncation")
			return nil, prepareErr
		}
		prepared = &openAIKiroPrepared{
			Payload:              payload,
			Projection:           projection,
			Thinking:             thinking,
			EstimatedInputTokens: estimateOpenAIRequestInputTokens(&localReq),
		}
		return prepared, nil
	}

	apiKeyID := apiKeyIDFromContext(r.Context())
	// forwarded marks a request that already passed through one Kiro-Go pool, so a
	// custom_api account cannot add another hop (loop guard, see forwardToUpstream).
	forwarded := r.Header.Get(forwardHeader) != ""
	if envelope.Stream {
		h.handleOpenAIStreamProjected(w, actualModel, apiKeyID, body, forwarded, r.Context(), prepareKiro)
	} else {
		h.handleOpenAINonStreamProjected(w, actualModel, apiKeyID, body, forwarded, r.Context(), prepareKiro)
	}
}

// handleOpenAIStream preserves the existing internal call contract used by
// focused stream tests and any same-package callers. New HTTP routing uses the
// projection-aware implementation below so payload creation remains lazy.
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, rawBody []byte, forwarded bool, requestContext context.Context) {
	prepareKiro := func() (*openAIKiroPrepared, error) {
		return &openAIKiroPrepared{Payload: payload, Thinking: thinking, EstimatedInputTokens: estimatedInputTokens}, nil
	}
	h.handleOpenAIStreamProjected(w, model, apiKeyID, rawBody, forwarded, requestContext, prepareKiro)
}

// handleOpenAIStreamProjected serves OpenAI streaming requests after choosing a
// transport family and only materializes a Kiro payload for a Kiro candidate.
func (h *Handler) handleOpenAIStreamProjected(w http.ResponseWriter, model, apiKeyID string, rawBody []byte, forwarded bool, requestContext context.Context, prepareKiro openAIKiroPrepareFunc) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	excluded := make(map[string]bool)
	var lastErr error
	var preparationErr error
	var incompatibility string
	reqStart := time.Now()
	requestedModel := stripRequestedModelThinkingSuffix(
		requestedModelFromJSON(rawBody, model),
		config.GetThinkingConfig().Suffix,
	)

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		selection := h.pool.GetNextSelectionForModelWithApiKey(requestedModel, excluded, apiKeyID)
		if selection == nil {
			break
		}
		account := &selection.Account
		providerID, _, resolved := h.resolveCandidateProviderModel(account, requestedModel, model)
		if !resolved {
			excluded[account.ID] = true
			attempt--
			continue
		}
		providerModelID := string(providerID)

		// Determine transport compatibility before token refresh/account mutation.
		// Semantic incompatibility is a routing decision, not an account failure.
		if account.IsBedrock() {
			projection := analyzeOpenAIChatBedrockProjection(rawBody, bedrockSpecificToolChoiceSupported(account, providerModelID))
			if !projection.Supported() {
				incompatibility = projection.Reason()
				excluded[account.ID] = true
				attempt--
				continue
			}
		} else if claudeAccountIsDirectAnthropic(account) {
			incompatibility = "direct Anthropic has no OpenAI Chat transport"
			excluded[account.ID] = true
			attempt--
			continue
		} else if account.IsCustomApi() {
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
		}

		var payload *KiroPayload
		var thinking bool
		var estimatedInputTokens int
		if !account.IsBedrock() && !account.IsCustomApi() {
			kiro, err := prepareKiro()
			if err != nil {
				preparationErr = err
				incompatibility = err.Error()
				excluded[account.ID] = true
				attempt--
				continue
			}
			payload = kiro.Payload
			thinking = kiro.Thinking
			estimatedInputTokens = kiro.EstimatedInputTokens
		}

		if err := h.ensureValidTokenForSelectionContext(requestContext, selection); err != nil {
			lastErr = err
			if errors.Is(err, errStaleAccountSelection) {
				continue
			}
			excluded[account.ID] = true
			h.handleTokenValidationFailureForSelection(selection, err)
			continue
		}

		// Native Bedrock accounts serve the OpenAI wire format by converting the
		// request to Anthropic Messages, invoking Bedrock, and converting the
		// Anthropic SSE back to OpenAI chunks. Same passthrough/failover contract
		// as custom_api: success ends the request; a pre-stream error fails over.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockOpenAIStream(w, flusher, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "openai", streaming: true,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
			}); bErr != nil {
				lastErr = bErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailureForSelection(*selection, bErr)
				}
				continue
			}
			return
		}
		// Custom API accounts proxy to another Kiro-Go pool (see handleClaudeStream).
		if account.IsCustomApi() {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if fwdErr := h.forwardToUpstream(w, flusher, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "openai", streaming: true,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				requestContext: requestContext,
			}); fwdErr != nil {
				if requestContext != nil &&
					requestContext.Err() != nil {
					return
				}
				lastErr = fwdErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				h.handleAccountFailureForSelection(*selection, fwdErr)
				continue
			}
			return
		}

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		responseStarted := false

		sendStreamError := func(err error) {
			if err == nil {
				return
			}
			data, _ := json.Marshal(map[string]interface{}{
				"error": map[string]string{
					"message": err.Error(),
					"type":    "server_error",
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			responseStarted = true
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendChunk(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		var upstreamStopReason string
		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				responseStarted = true
			},
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = h.usageInputFromContextPercentage(account, string(providerID), model, pct)
			},
		}

		err := CallKiroAPIContext(requestContext, account, payload, callback)
		if err != nil {
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			lastErr = err
			if !h.pool.IsSelectionCurrent(*selection) && !responseStarted {
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailureForSelection(*selection, err)
			if !responseStarted {
				continue
			}
			// Stream already started: replay and a normal completion sentinel are both
			// unsafe. Emit an explicit SSE error frame for clients that understand the
			// OpenAI error envelope, followed by finish_reason:error for chunk-only
			// consumers. Never emit [DONE] on this failure path.
			sendStreamError(err)
			errChunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "error",
				}},
			}
			errData, _ := json.Marshal(errChunk)
			fmt.Fprintf(w, "data: %s\n\n", string(errData))
			// Do not send [DONE] after an integrity/transport failure. [DONE] is the
			// normal terminal sentinel and would turn a truncated stream into apparent success.
			flusher.Flush()
			h.recordFailureWithDetails("openai", model, account.ID, apiKeyID, err)
			return
		}

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		if realInputTokens > 0 && inputTokens <= 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccessForSelection(*selection)
		h.pool.RecordLatencyForSelection(*selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*selection, inputTokens+outputTokens, credits)
		h.recordSuccessLog("openai", model, account.ID, apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		finishReason := resolveOpenAIHandlerFinishReason(upstreamStopReason, len(toolCalls))

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": map[string]int{
				"prompt_tokens":     inputTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      inputTokens + outputTokens,
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		if preparationErr != nil || incompatibility != "" {
			message := incompatibility
			if message == "" && preparationErr != nil {
				message = preparationErr.Error()
			}
			h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "No compatible OpenAI transport is available: "+message)
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("openai", model, "", apiKeyID, lastErr)
	h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", lastErr.Error())
}

// handleOpenAINonStream preserves the existing same-package call contract.
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, rawBody []byte, forwarded bool, requestContext context.Context) {
	prepareKiro := func() (*openAIKiroPrepared, error) {
		return &openAIKiroPrepared{Payload: payload, Thinking: thinking, EstimatedInputTokens: estimatedInputTokens}, nil
	}
	h.handleOpenAINonStreamProjected(w, model, apiKeyID, rawBody, forwarded, requestContext, prepareKiro)
}

// handleOpenAINonStreamProjected is the projection-aware non-stream dispatcher.
func (h *Handler) handleOpenAINonStreamProjected(w http.ResponseWriter, model, apiKeyID string, rawBody []byte, forwarded bool, requestContext context.Context, prepareKiro openAIKiroPrepareFunc) {
	excluded := make(map[string]bool)
	var lastErr error
	var preparationErr error
	var incompatibility string
	reqStart := time.Now()
	requestedModel := stripRequestedModelThinkingSuffix(
		requestedModelFromJSON(rawBody, model),
		config.GetThinkingConfig().Suffix,
	)

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		selection := h.pool.GetNextSelectionForModelWithApiKey(requestedModel, excluded, apiKeyID)
		if selection == nil {
			break
		}
		account := &selection.Account
		providerID, _, resolved := h.resolveCandidateProviderModel(account, requestedModel, model)
		if !resolved {
			excluded[account.ID] = true
			attempt--
			continue
		}
		providerModelID := string(providerID)

		// Determine transport compatibility before token refresh/account mutation.
		// Semantic incompatibility is a routing decision, not an account failure.
		if account.IsBedrock() {
			projection := analyzeOpenAIChatBedrockProjection(rawBody, bedrockSpecificToolChoiceSupported(account, providerModelID))
			if !projection.Supported() {
				incompatibility = projection.Reason()
				excluded[account.ID] = true
				attempt--
				continue
			}
		} else if claudeAccountIsDirectAnthropic(account) {
			incompatibility = "direct Anthropic has no OpenAI Chat transport"
			excluded[account.ID] = true
			attempt--
			continue
		} else if account.IsCustomApi() {
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
		}

		var payload *KiroPayload
		var thinking bool
		var estimatedInputTokens int
		if !account.IsBedrock() && !account.IsCustomApi() {
			kiro, err := prepareKiro()
			if err != nil {
				preparationErr = err
				incompatibility = err.Error()
				excluded[account.ID] = true
				attempt--
				continue
			}
			payload = kiro.Payload
			thinking = kiro.Thinking
			estimatedInputTokens = kiro.EstimatedInputTokens
		}

		if err := h.ensureValidTokenForSelectionContext(requestContext, selection); err != nil {
			lastErr = err
			if errors.Is(err, errStaleAccountSelection) {
				continue
			}
			excluded[account.ID] = true
			h.handleTokenValidationFailureForSelection(selection, err)
			continue
		}

		// Native Bedrock accounts serve OpenAI by converting to Anthropic, invoking
		// Bedrock, and converting the Anthropic JSON response back to an OpenAI
		// chat.completion. Success ends the request; a pre-reply error fails over.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockOpenAINonStream(w, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "openai", streaming: false,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
			}); bErr != nil {
				lastErr = bErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailureForSelection(*selection, bErr)
				}
				continue
			}
			return
		}
		// Custom API accounts proxy to another Kiro-Go pool (see handleClaudeStream).
		// The forwarded loop guard was already applied before token validation.
		if account.IsCustomApi() {
			if fwdErr := h.forwardToUpstream(w, nil, forwardParams{
				selection: selection, account: account, body: rawBody, endpoint: "openai", streaming: false,
				model: model, providerModelID: providerModelID, apiKeyID: apiKeyID, forwarded: forwarded,
				requestContext: requestContext,
			}); fwdErr != nil {
				if requestContext != nil &&
					requestContext.Err() != nil {
					return
				}
				lastErr = fwdErr
				if !h.pool.IsSelectionCurrent(*selection) {
					continue
				}
				excluded[account.ID] = true
				h.handleAccountFailureForSelection(*selection, fwdErr)
				continue
			}
			return
		}

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamStopReason string

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = h.usageInputFromContextPercentage(account, string(providerID), model, pct)
			},
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			CanRetry:     func() bool { return true },
			ResetForRetry: func() {
				content = ""
				reasoningContent = ""
				toolUses = nil
				inputTokens, outputTokens = 0, 0
				credits = 0
				realInputTokens = 0
				upstreamStopReason = ""
			},
		}

		err := CallKiroAPIContext(requestContext, account, payload, callback)
		if err != nil {
			if requestContext != nil && requestContext.Err() != nil {
				return
			}
			lastErr = err
			if !h.pool.IsSelectionCurrent(*selection) {
				continue
			}
			excluded[account.ID] = true
			h.handleAccountFailureForSelection(*selection, err)
			continue
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 && inputTokens <= 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccessForSelection(*selection)
		h.pool.RecordLatencyForSelection(*selection, float64(time.Since(reqStart).Milliseconds()))
		h.pool.UpdateStatsForSelection(*selection, inputTokens+outputTokens, credits)
		h.recordSuccessLog("openai", model, account.ID, apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
		if choices, ok := resp["choices"].([]map[string]interface{}); ok && len(choices) > 0 {
			choices[0]["finish_reason"] = resolveOpenAIHandlerFinishReason(upstreamStopReason, len(toolUses))
			resp["choices"] = choices
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		if preparationErr != nil || incompatibility != "" {
			message := incompatibility
			if message == "" && preparationErr != nil {
				message = preparationErr.Error()
			}
			h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "No compatible OpenAI transport is available: "+message)
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailureWithDetails("openai", model, "", apiKeyID, lastErr)
	h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", lastErr.Error())
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

type unsupportedAccountKindError struct {
	AuthMethod string
}

func (e *unsupportedAccountKindError) Error() string {
	if e == nil {
		return "unsupported account kind"
	}
	return fmt.Sprintf("unsupported account auth method %q", e.AuthMethod)
}

// handleTokenValidationFailureForSelection applies request-derived failure state
// only to the lifecycle that actually attempted the refresh.
func (h *Handler) handleTokenValidationFailureForSelection(selection *pool.AccountSelection, err error) {
	if selection == nil || err == nil || errors.Is(err, errStaleAccountSelection) {
		return
	}
	var kindErr *unsupportedAccountKindError
	if errors.As(err, &kindErr) {
		logger.Warnf("[AccountKind] skipping account=%s: %v", selection.Account.ID, err)
		return
	}
	h.handleAccountFailureForSelection(*selection, err)
}

// ensureValidTokenForSelection refreshes a token through a generation-fenced
// compare-and-swap. It never re-reads credentials from a newer lifecycle with
// the same textual ID and never publishes a stale refresh result into it.
func (h *Handler) ensureValidTokenForSelection(selection *pool.AccountSelection) error {
	return h.ensureValidTokenForSelectionContext(context.Background(), selection)
}

func (h *Handler) ensureValidTokenForSelectionContext(ctx context.Context, selection *pool.AccountSelection) error {
	if selection == nil {
		return errors.New("account selection is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	account := &selection.Account
	if !account.HasKnownKind() {
		return &unsupportedAccountKindError{AuthMethod: account.AuthMethod}
	}
	if !account.SupportsKiroTokenRefresh() {
		return nil
	}
	if selection.LifecycleGeneration != 0 &&
		account.DispatchGeneration != selection.LifecycleGeneration {
		return errStaleAccountSelection
	}
	needsRefresh := strings.TrimSpace(account.AccessToken) == "" ||
		(account.ExpiresAt > 0 && time.Now().Unix() >= account.ExpiresAt-tokenRefreshSkewSeconds)
	if !needsRefresh {
		return nil
	}
	_, err := h.refreshAccountTokenContext(ctx, account, false)
	return err
}

func (h *Handler) handleTokenValidationFailure(account *config.Account, err error) {
	var kindErr *unsupportedAccountKindError
	if errors.As(err, &kindErr) {
		logger.Warnf("[AccountKind] skipping account=%s: %v", account.ID, err)
		return
	}
	h.handleAccountFailure(account, err)
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	return h.ensureValidTokenContext(context.Background(), account)
}

func (h *Handler) ensureValidTokenContext(ctx context.Context, account *config.Account) error {
	if account == nil {
		return errors.New("account is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !account.HasKnownKind() {
		return &unsupportedAccountKindError{AuthMethod: account.AuthMethod}
	}
	// Static provider credentials do not have a refreshable Kiro OAuth token.
	if !account.SupportsKiroTokenRefresh() {
		return nil
	}
	needsRefresh := strings.TrimSpace(account.AccessToken) == "" ||
		(account.ExpiresAt > 0 && time.Now().Unix() >= account.ExpiresAt-tokenRefreshSkewSeconds)
	if !needsRefresh {
		return nil
	}
	_, err := h.refreshAccountTokenContext(ctx, account, false)
	return err
}

// ==================== 管理 API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// 验证密码
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		cookie, _ := r.Cookie("admin_password")
		if cookie != nil {
			password = cookie.Value
		}
	}

	if password != config.GetPassword() {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts/runtime" && r.Method == "GET":
		h.apiGetAccountsRuntime(w, r)
	case path == "/account-routing" && r.Method == "GET":
		h.apiGetAccountRouting(w, r)
	case path == "/account-routing" && r.Method == "PUT":
		h.apiUpdateAccountRouting(w, r)
	case path == "/accounts/order" && r.Method == "PUT":
		h.apiReorderAccounts(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/routing") && r.Method == "PUT":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/routing")
		h.apiUpdateAccountRoutingPriority(w, r, id)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	// Kiro profile discovery/switch for an existing account (external_idp multi-region).
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiListAccountKiroProfiles(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiSwitchAccountKiroProfile(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
	case path == "/auth/kiro-sso/select-profile" && r.Method == "POST":
		h.apiSelectKiroSsoProfile(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/auth/microsoft-sso/start" && r.Method == "POST":
		h.apiStartMicrosoftSSO(w, r)
	case path == "/auth/microsoft-sso/complete" && r.Method == "POST":
		h.apiCompleteMicrosoftSSO(w, r)
	case path == "/auth/microsoft-sso/select-profile" && r.Method == "POST":
		h.apiSelectMicrosoftSSOProfile(w, r)
	case path == "/auth/microsoft-sso/cancel" && r.Method == "POST":
		h.apiCancelMicrosoftSSO(w, r)
	case path == "/auth/import-cli-json" && r.Method == "POST":
		h.apiImportCliJson(w, r)
	case path == "/auth/import-ide-cache" && r.Method == "POST":
		h.apiImportIdeCache(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/logs" && r.Method == "DELETE":
		h.apiClearLogs(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"generation":        a.DispatchGeneration,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"priority":          a.Priority,
			"effectivePriority": config.EffectiveAccountPriority(a.Priority),
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
			"providerRuntime":   h.providerRuntimeDiagnostics(a.ID),
		}
	}
	json.NewEncoder(w).Encode(result)
}

func setDirectAnthropicStaticAPIKey(account *config.Account, value string) error {
	if account == nil {
		return errors.New("account is nil")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("kiroApiKey is required for direct Anthropic accounts")
	}
	account.KiroApiKey = value
	// Direct Anthropic static credentials belong exclusively to the x-api-key
	// trust domain. Never duplicate them into the generic bearer-token field:
	// doing so makes an accidental Kiro-transport fallthrough capable of sending
	// the Anthropic secret as Authorization: Bearer.
	account.AccessToken = ""
	account.ExpiresAt = 0
	return nil
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}

	if claudeAccountIsDirectAnthropic(&account) {
		baseURL, err := normalizeDirectAnthropicBaseURL(account.BaseURL)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		account.BaseURL = baseURL
		if err := setDirectAnthropicStaticAPIKey(&account, account.KiroApiKey); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if account.Provider == "" {
			account.Provider = "Anthropic"
		}
	}

	// Kiro API-key accounts: the key IS the credential. Validate it, normalize the
	// auth method, and mirror it into AccessToken so pool routing / model refresh
	// (which gate on a non-empty AccessToken) treat the account as ready. ExpiresAt
	// stays 0 so the token-refresh paths skip it — API keys are never refreshed.
	// Detection is case-insensitive (matching IsApiKeyCredential) and also triggers
	// when a bare kiroApiKey is supplied without an explicit authMethod.
	account.KiroApiKey = strings.TrimSpace(account.KiroApiKey)
	if !claudeAccountIsDirectAnthropic(&account) && (account.IsApiKeyCredential() || account.KiroApiKey != "") {
		if account.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}
		// Reject a contradictory payload: a key plus a different, explicit OAuth
		// method would otherwise be silently rewritten to api_key.
		if am := strings.TrimSpace(account.AuthMethod); am != "" && !account.IsApiKeyCredential() {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey cannot be combined with authMethod " + am})
			return
		}
		account.AuthMethod = "api_key"
		account.ExpiresAt = 0
		account.AccessToken = account.KiroApiKey
		// Discover the region the key actually serves. The panel deliberately omits
		// region for api_key adds, so without this an EU-provisioned key would inherit
		// the us-east-1 default below and 403 on every upstream call, permanently.
		// A transient upstream failure must not be reported as a bad key, so it maps
		// to 502 (retry) rather than 400 (caller error).
		region, info, retryable, err := resolveApiKeyRegion(account.KiroApiKey, account.Region)
		if err != nil {
			status := 400
			if retryable {
				status = 502
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		account.Region = region
		// The probe already paid for the identity round-trip; keep it so the account
		// shows a real email in the panel and can be deduplicated by UserId later.
		if info != nil {
			if account.Email == "" {
				account.Email = info.Email
			}
			if account.UserId == "" {
				account.UserId = info.UserId
			}
		}
	}

	if !account.HasKnownKind() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "unsupported authMethod: " + strings.TrimSpace(account.AuthMethod)})
		return
	}

	if account.Region == "" && !claudeAccountIsDirectAnthropic(&account) {
		account.Region = "us-east-1"
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if accountReadyForModelDiscovery(&account) {
		acc := account.Clone()
		h.startBackgroundTaskContext(func(taskCtx context.Context) {
			ctx, cancel := context.WithTimeout(taskCtx, 2*time.Minute)
			defer cancel()
			if err := h.fetchAndCacheAccountModelsContext(ctx, &acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", accountIdentityForLog(&acc), err)
			}
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	observed, ok := config.GetAccountByID(id)
	if !ok {
		// Preserve the historical idempotent DELETE contract. The stale-recreate
		// protection below applies once this request actually observed a lifecycle.
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}

	deleted, applied, err := config.DeleteAccountIfLifecycle(id, observed.DispatchGeneration)
	if !applied {
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account changed while delete was pending; reload and retry"})
		return
	}

	// Cleanup is scoped to the lifecycle that was actually retired. A recreate
	// with the same textual ID may already have published N+1 runtime state.
	if deleted.IsBedrock() {
		clearBedrockModelCacheForLifecycle(&deleted)
		clearBedrockRegionRoutesForLifecycle(&deleted)
		bedrockThrottle.clearForLifecycle(&deleted)
	}
	h.clearAccountProviderRuntimeStateForLifecycle(&deleted)
	h.pool.Reload()

	if err != nil {
		// A post-publication durability error still means the delete is visible and
		// memory was committed; report the durability failure after aligning runtime.
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func applyAdminAccountUpdates(account *config.Account, updates map[string]interface{}) error {
	if account == nil {
		return errors.New("account is nil")
	}
	if v, ok := updates["enabled"].(bool); ok {
		account.Enabled = v
	}
	if v, ok := updates["nickname"].(string); ok {
		account.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		account.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		account.Weight = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		account.ProxyURL = v
	}

	if claudeAccountIsDirectAnthropic(account) {
		if v, ok := updates["baseUrl"].(string); ok {
			baseURL, err := normalizeDirectAnthropicBaseURL(v)
			if err != nil {
				return err
			}
			account.BaseURL = baseURL
		}
		if v, ok := updates["kiroApiKey"].(string); ok {
			if err := setDirectAnthropicStaticAPIKey(account, v); err != nil {
				return err
			}
		}
	}

	if account.IsBedrock() {
		if v, ok := updates["region"].(string); ok {
			account.Region = strings.TrimSpace(v)
		}
		if v, ok := updates["bedrockApiKey"].(string); ok {
			account.BedrockAPIKey = strings.TrimSpace(v)
		}
		if v, ok := updates["bedrockAccessKeyId"].(string); ok {
			account.BedrockAccessKeyID = strings.TrimSpace(v)
		}
		if v, ok := updates["bedrockSecretAccessKey"].(string); ok {
			account.BedrockSecretAccessKey = strings.TrimSpace(v)
		}
		if v, ok := updates["bedrockUseConverse"].(bool); ok {
			account.BedrockUseConverse = v
		}
		if v, ok := updates["bedrockRegions"].([]interface{}); ok {
			regions := make([]string, 0, len(v))
			for _, item := range v {
				if region, ok := item.(string); ok && strings.TrimSpace(region) != "" {
					regions = append(regions, strings.TrimSpace(region))
				}
			}
			account.BedrockRegions = regions
		}
		if account.BedrockAPIKey == "" &&
			(account.BedrockAccessKeyID == "" || account.BedrockSecretAccessKey == "") {
			return errors.New("bedrock account needs either an API key or an access key + secret")
		}
	}
	return nil
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	observed, ok := config.GetAccountByID(id)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !observed.HasKnownKind() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "unsupported authMethod: " + strings.TrimSpace(observed.AuthMethod)})
		return
	}

	var validationErr error
	before, after, applied, err := config.MutateAccountIfLifecycle(
		id,
		observed.DispatchGeneration,
		func(current *config.Account) error {
			validationErr = applyAdminAccountUpdates(current, updates)
			return validationErr
		},
	)
	if !applied {
		if validationErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": validationErr.Error()})
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account changed while update was pending; reload and retry"})
		return
	}

	lifecycleChanged := before.DispatchGeneration != after.DispatchGeneration
	h.pool.Reload()
	if lifecycleChanged {
		// Retire only N. Never clear runtime state by account ID here: a concurrent
		// recreate may already have published N+1 under the same textual ID.
		if before.IsBedrock() {
			clearBedrockModelCacheForLifecycle(&before)
			clearBedrockRegionRoutesForLifecycle(&before)
			bedrockThrottle.clearForLifecycle(&before)
		}
		h.clearAccountProviderRuntimeStateForLifecycle(&before)
	} else {
		h.rebuildEnabledModelUnion()
	}

	// Re-enable/source changes need a model refresh using the committed account,
	// which carries the current lifecycle generation and any concurrent refresh
	// fields that were intentionally preserved by the partial mutation.
	if accountReadyForModelDiscovery(&after) && (!before.Enabled || lifecycleChanged) {
		acc := after.Clone()
		h.startBackgroundTaskContext(func(taskCtx context.Context) {
			ctx, cancel := context.WithTimeout(taskCtx, 2*time.Minute)
			defer cancel()
			if refreshErr := h.fetchAndCacheAccountModelsContext(ctx, &acc); refreshErr != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", accountIdentityForLog(&acc), refreshErr)
			}
		})
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !account.SupportsOverageAPI() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Overage API is not supported for this account kind"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if !account.SupportsOverageAPI() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Overage API is not supported for this account kind"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		observedByID := make(map[string]config.Account)
		for _, account := range config.GetAccounts() {
			observedByID[account.ID] = account
		}

		toRefreshModels := make([]config.Account, 0)
		staleIDs := make([]string, 0)
		appliedCount := 0
		var persistErr error

		seenIDs := make(map[string]struct{}, len(req.IDs))
		for _, id := range req.IDs {
			if _, duplicate := seenIDs[id]; duplicate {
				continue
			}
			seenIDs[id] = struct{}{}
			observed, ok := observedByID[id]
			if !ok {
				continue
			}
			before, after, applied, err := config.MutateAccountIfLifecycle(
				id,
				observed.DispatchGeneration,
				func(current *config.Account) error {
					current.Enabled = enabled
					if enabled && current.BanStatus != "" && current.BanStatus != "ACTIVE" {
						current.BanStatus = "ACTIVE"
						current.BanReason = ""
						current.BanTime = 0
					}
					return nil
				},
			)
			if !applied {
				if err != nil && persistErr == nil {
					persistErr = err
				} else if err == nil {
					staleIDs = append(staleIDs, id)
				}
				continue
			}
			appliedCount++
			if err != nil && persistErr == nil {
				persistErr = err
			}
			if enabled && !before.Enabled && accountReadyForModelDiscovery(&after) {
				toRefreshModels = append(toRefreshModels, after)
			}
		}

		h.pool.Reload()
		h.rebuildEnabledModelUnion()
		for _, acc := range toRefreshModels {
			a := acc.Clone()
			h.startBackgroundTaskContext(func(taskCtx context.Context) {
				ctx, cancel := context.WithTimeout(taskCtx, 2*time.Minute)
				defer cancel()
				if err := h.fetchAndCacheAccountModelsContext(ctx, &a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", accountIdentityForLog(&a), err)
				}
			})
		}

		if persistErr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"count":   appliedCount,
				"error":   persistErr.Error(),
			})
			return
		}
		if len(staleIDs) > 0 {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":  false,
				"count":    appliedCount,
				"staleIds": staleIDs,
				"error":    "One or more accounts changed while the batch update was pending; reload and retry",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			if _, err := h.refreshAccountByKind(r.Context(), account, accountRefreshExplicit); err != nil {
				logger.Warnf("[BatchRefresh] Failed to refresh account %s: %v", id, err)
				failCount++
				continue
			}
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"count":   successCount,
			"failed":  failCount,
		})

	default:
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action"})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiStartKiroSso starts the Kiro hosted-portal sign-in (Enterprise SSO — Microsoft 365 /
// Entra ID, plus Google/GitHub). It binds the loopback callback listener and returns the
// sign-in URL the operator opens in a browser ON THE SAME HOST as the proxy (the OAuth
// redirect targets 127.0.0.1:3128). The browser is driven through the enterprise external-IdP
// leg automatically; the front end polls /auth/kiro-sso/poll until completion.
func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	// Region is optional (defaults to us-east-1 in StartKiroSsoLogin), so a decode
	// error (including an empty body) is intentionally tolerated — mirrors
	// apiStartBuilderIdLogin.
	json.NewDecoder(r.Body).Decode(&req)

	session, signInURL, err := auth.StartKiroSsoLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"signInUrl": signInURL,
		"interval":  2,
	})
}

// apiCancelKiroSso tears down an in-flight hosted-portal sign-in (operator closed or
// cancelled the modal), freeing the loopback callback port immediately instead of
// waiting for the deadline. It also drops any tokens parked awaiting a profile
// choice, so a dismissed picker doesn't leave credentials in memory for the TTL.
func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		auth.CancelKiroSsoLogin(req.SessionID)
		dropPendingKiroSsoChoice(req.SessionID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// --- Deferred profile choice (external_idp multi-region) ---------------------
//
// When the eager probe finds 2+ Kiro profiles for a freshly-exchanged
// external_idp credential, the account is NOT created yet: the exchanged tokens
// and the discovered profile list are parked here (keyed by the SSO session id)
// until the operator picks a profile via /auth/kiro-sso/select-profile, cancels,
// or the TTL expires. The TTL keeps unclaimed tokens from lingering in memory.

// kiroSsoChoiceTTL bounds how long exchanged tokens wait for a profile choice.
const kiroSsoChoiceTTL = 5 * time.Minute

// pendingKiroSsoChoice parks one exchanged credential awaiting a profile pick.
type pendingKiroSsoChoice struct {
	result    *auth.KiroSsoResult
	machineId string
	profiles  []KiroProfile
	// expiresAt is the ACCESS TOKEN's absolute expiry, stamped at exchange time.
	// It must not be recomputed from ExpiresIn at finalize time: the operator can
	// sit on the picker for minutes, and an expiry overstated by that gap would
	// make the proactive refresh (tokenRefreshSkewSeconds) miss the real deadline.
	expiresAt int64
	// deadline is when this stash self-destructs. A re-stash (invalid pick keeps
	// the entry alive for another attempt) reuses the ORIGINAL deadline so
	// repeated invalid picks cannot extend how long tokens sit in memory.
	deadline time.Time
	timer    *time.Timer
}

var (
	pendingKiroSsoChoices   = make(map[string]*pendingKiroSsoChoice)
	pendingKiroSsoChoicesMu sync.Mutex
)

// stashPendingKiroSsoChoice parks an exchanged credential plus its discovered
// profiles under the SSO session id, self-expiring at pending.deadline.
func stashPendingKiroSsoChoice(sessionID string, pending *pendingKiroSsoChoice) {
	// Identity-checked expiry: only delete the entry if it is still THIS stash.
	// A plain delete-by-key could race a re-stash — Stop() on an already-fired
	// timer is a no-op, and the fired callback would then destroy the fresh entry.
	pending.timer = time.AfterFunc(time.Until(pending.deadline), func() {
		pendingKiroSsoChoicesMu.Lock()
		if cur, ok := pendingKiroSsoChoices[sessionID]; ok && cur == pending {
			delete(pendingKiroSsoChoices, sessionID)
			logger.Debugf("[KiroSSO] Pending profile choice for session %s expired", sessionID)
		}
		pendingKiroSsoChoicesMu.Unlock()
	})
	pendingKiroSsoChoicesMu.Lock()
	// A repeated stash for the same session replaces the previous one; stop the
	// superseded timer (best-effort — the identity check above covers the rest).
	if prev, ok := pendingKiroSsoChoices[sessionID]; ok && prev.timer != nil {
		prev.timer.Stop()
	}
	pendingKiroSsoChoices[sessionID] = pending
	pendingKiroSsoChoicesMu.Unlock()
}

// takePendingKiroSsoChoice removes and returns the parked credential, or nil.
func takePendingKiroSsoChoice(sessionID string) *pendingKiroSsoChoice {
	pendingKiroSsoChoicesMu.Lock()
	defer pendingKiroSsoChoicesMu.Unlock()
	pending, ok := pendingKiroSsoChoices[sessionID]
	if !ok {
		return nil
	}
	delete(pendingKiroSsoChoices, sessionID)
	if pending.timer != nil {
		pending.timer.Stop()
	}
	return pending
}

// dropPendingKiroSsoChoice discards a parked credential (cancel / TTL expiry).
func dropPendingKiroSsoChoice(sessionID string) {
	if pending := takePendingKiroSsoChoice(sessionID); pending != nil {
		logger.Debugf("[KiroSSO] Dropped pending profile choice for session %s", sessionID)
	}
}

// apiSelectKiroSsoProfile finishes a deferred hosted-portal sign-in: the operator
// picked one of the discovered profiles, so pin it and create the account.
func (h *Handler) apiSelectKiroSsoProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"sessionId"`
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.SessionID == "" || req.ProfileArn == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and profileArn are required"})
		return
	}

	pending := takePendingKiroSsoChoice(req.SessionID)
	if pending == nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profile choice expired or already completed; sign in again"})
		return
	}

	// Only an ARN that was actually offered may be pinned — reject anything else
	// and re-park the stash so the operator can pick again.
	valid := false
	for _, p := range pending.profiles {
		if p.ARN == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		// Re-park with the ORIGINAL deadline: an invalid pick must not reset the TTL.
		stashPendingKiroSsoChoice(req.SessionID, pending)
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}

	pending.result.ProfileArn = req.ProfileArn
	h.finalizeKiroSsoAccount(w, pending.result, pending.machineId, pending.expiresAt)
}

// apiListAccountKiroProfiles GET /accounts/{id}/kiro-profiles
// Runs the multi-region profile discovery for an EXISTING account so the
// operator can see every Kiro profile the credential can reach (e.g. a US and
// an EU profile) and re-pin via POST. external_idp only: other auth methods
// carry an authoritative region already.
func (h *Handler) apiListAccountKiroProfiles(w http.ResponseWriter, r *http.Request, id string) {
	account, status, errMsg := h.lookupAccountForProfileOps(id)
	if account == nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	profiles, err := DiscoverKiroProfiles(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"profiles": profiles,
		"current":  strings.TrimSpace(account.ProfileArn),
	})
}

// apiSwitchAccountKiroProfile POST /accounts/{id}/kiro-profiles {profileArn}
// Re-pins an existing external_idp account to another discovered profile. The
// requested ARN is validated against a fresh discovery (stateless: no stash to
// expire) before overwriting the cached ProfileArn; the pool reload makes the
// data-plane region switch take effect on the next request.
func (h *Handler) apiSwitchAccountKiroProfile(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.ProfileArn == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is required"})
		return
	}

	observed, status, errMsg := h.lookupAccountForProfileOps(id)
	if observed == nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	profiles, err := DiscoverKiroProfiles(observed)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	valid := false
	for _, profile := range profiles {
		if profile.ARN == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}

	before, after, applied, err := config.MutateAccountIfLifecycle(
		id,
		observed.DispatchGeneration,
		func(current *config.Account) error {
			current.ProfileArn = req.ProfileArn
			return nil
		},
	)
	if !applied {
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account changed while profile discovery was pending; reload and retry"})
		return
	}

	h.pool.Reload()
	if before.DispatchGeneration != after.DispatchGeneration {
		h.clearAccountProviderRuntimeStateForLifecycle(&before)
	} else {
		h.rebuildEnabledModelUnion()
	}

	// The model list is region-scoped. Refresh using the committed lifecycle.
	modelsRefreshed := true
	if refreshErr := h.fetchAndCacheAccountModelsContext(r.Context(), &after); refreshErr != nil {
		modelsRefreshed = false
		logger.Warnf("[ProfileArn] Model refresh after profile switch failed for %s: %v", accountEmailForLog(&after), refreshErr)
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"profileArn":      req.ProfileArn,
		"modelsRefreshed": modelsRefreshed,
	})
}

// lookupAccountForProfileOps fetches an account copy (with the pool's freshest
// tokens) for profile discovery/switch, restricted to external_idp accounts.
// Returns (nil, httpStatus, message) when not found (404) or not eligible (400).
func (h *Handler) lookupAccountForProfileOps(id string) (*config.Account, int, string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		return nil, 404, "Account not found"
	}
	if !strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
		return nil, 400, "profile switching is only supported for external_idp accounts"
	}
	// 与 apiRefreshAccountModels 一致：用 pool 中运行时最新 token 探测，避免用到
	// 已被刷新淘汰的磁盘态 token。
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	return account, 200, ""
}

// apiPollKiroSso reports the hosted-portal sign-in status. While the user is signing in it
// returns completed=false; once the listener captures the authorization code it exchanges it,
// persists the account (AuthMethod "external_idp" for an Azure tenant, "social" otherwise), and
// returns completed=true. The profileArn is resolved lazily on first use (the EXTERNAL_IDP
// token type header is now sent on CodeWhisperer calls), so it is not required here.
func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	result, status, err := auth.PollKiroSsoAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    "pending",
		})
		return
	}

	// 授权完成。external_idp（Azure 租户）账号的 home region 在登录时未知，且同一
	// 账号可能在多个 region 各有一个 Kiro profile（例如 US 与 EU）。这里在建号前
	// 主动跨 region 探测全部 profile：
	//   0 个 → 维持旧行为，ProfileArn 留空、首次使用时懒解析；
	//   1 个 → 直接固定该 profile（免去日后懒探测）；
	//   2+ 个 → 暂不建号，把兑换好的 token 与 profile 列表暂存，让前端弹出选择器,
	//           由 /auth/kiro-sso/select-profile 完成建号。
	machineId := config.GenerateMachineId()
	// Token 过期时刻在“兑换完成”的此刻定格。choose_profile 分支可能让 token 在
	// 暂存区躺上几分钟，若到建号时才用 ExpiresIn 计算，过期时刻会被高估，导致
	// 主动刷新（tokenRefreshSkewSeconds）错过真实截止时间。
	tokenExpiresAt := time.Now().Unix() + int64(result.ExpiresIn)
	if result.AuthMethod == "external_idp" && strings.TrimSpace(result.ProfileArn) == "" {
		// 探测用临时账号：仅承载调用 ListAvailableProfiles 所需的凭证字段，不落盘。
		probe := &config.Account{
			Email:       result.Email,
			AccessToken: result.AccessToken,
			AuthMethod:  result.AuthMethod,
			Provider:    result.Provider,
			Region:      result.Region,
			MachineId:   machineId,
		}
		profiles, err := DiscoverKiroProfiles(probe)
		if err != nil {
			// 探测失败不阻断建号：回退到与旧版一致的懒解析路径。
			logger.Warnf("[KiroSSO] Profile discovery failed for %s, falling back to lazy resolution: %v", result.Email, err)
		}
		switch {
		case len(profiles) == 1:
			result.ProfileArn = profiles[0].ARN
		case len(profiles) >= 2:
			stashPendingKiroSsoChoice(req.SessionID, &pendingKiroSsoChoice{
				result:    result,
				machineId: machineId,
				profiles:  profiles,
				expiresAt: tokenExpiresAt,
				deadline:  time.Now().Add(kiroSsoChoiceTTL),
			})
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success":   true,
				"completed": true,
				"status":    "choose_profile",
				"profiles":  profiles,
			})
			return
		}
	}

	h.finalizeKiroSsoAccount(w, result, machineId, tokenExpiresAt)
}

// buildKiroSsoAccount assembles the persisted account record from an exchanged
// hosted-portal credential. Shared by the immediate path (0/1 profile) and the
// deferred path (operator picked one of several profiles), so both create
// byte-identical accounts. expiresAt is the absolute token expiry stamped at
// exchange time — never derived from ExpiresIn here, because on the deferred
// path minutes may have passed since the exchange.
func buildKiroSsoAccount(result *auth.KiroSsoResult, machineId string, expiresAt int64) config.Account {
	return config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    result.AuthMethod,
		Provider:      result.Provider,
		Region:        result.Region,
		ProfileArn:    result.ProfileArn,
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     machineId,
	}
}

// finalizeKiroSsoAccount persists the account and writes the completed response.
func (h *Handler) finalizeKiroSsoAccount(w http.ResponseWriter, result *auth.KiroSsoResult, machineId string, expiresAt int64) {
	account := buildKiroSsoAccount(result, machineId, expiresAt)

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	// Cap the body: accessToken becomes attacker-influenced input that is base64- and
	// JSON-decoded twice (issuerFromAccessTokenJWT / ExpFromAccessTokenJWT). Without a
	// limit an oversized token is a memory-amplification DoS. Mirrors the io.LimitReader
	// guard on outbound IdP responses in auth/kiro_sso.go's oidcDiscover.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	// decodeImportRequest (cli_json_import.go) accepts both the legacy camelCase
	// payload this endpoint has always taken AND the kiro-login-helper.py
	// snake_case shape (token_endpoint, issuer_url, profile_arn, ...), so a raw
	// helper export can be pasted here unmodified. It intentionally does NOT
	// collapse authMethod onto a canonical bucket — see importCredentialRequest's
	// doc comment: the classifier below needs the raw alias (e.g. "AzureAD") to
	// detect an explicit Microsoft declaration before any bucketing happens.
	req, err := decodeImportRequest(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// h.importCredential is the single core every import path (this endpoint,
	// /auth/import-cli-json, /auth/import-ide-cache, and the directory watcher)
	// funnels through, so all of them get identical classification, SSRF
	// allow-listing, and trust-on-import behavior — see its doc comment.
	result, err := h.importCredential(req)
	if err != nil {
		if _, ok := err.(*importValidationError); ok {
			w.WriteHeader(importErrorStatus(err))
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Persistence failure (duplicate id/refresh-token/api-key, or a config
		// write error). A refresh may have already rotated the IdP refresh token
		// before persistence failed; surface it so the operator can retry without
		// a full interactive re-login (writeAddAccountError's existing contract).
		h.writeAddAccountError(w, err, result.RotatedRefreshToken)
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    result.Account.ID,
			"email": result.Account.Email,
		},
	})
}

// externalIdpAuthMethodAliases are lower-cased authMethod values (or Kiro Account
// Manager provider labels) that mean "external IdP / enterprise SSO" and must
// normalize to "external_idp".
var externalIdpAuthMethodAliases = map[string]bool{
	"external_idp": true,
	"azuread":      true,
	"azure":        true,
	"entra":        true,
	"entra-id":     true,
	"entra_id":     true,
	"microsoft":    true,
	"m365":         true,
	"office365":    true,
	"external":     true,
}

// normalizeImportAuthMethod maps a pasted credential JSON's authMethod (plus its
// clientId/clientSecret/tokenEndpoint) onto one of the three canonical methods
// ("external_idp" | "idc" | "social"). external_idp MUST be detected before the
// clientId+clientSecret→idc inference, because external_idp accounts carry clientId
// but NO clientSecret, so the old default branch misclassified them as "social" and
// refresh hit the wrong endpoint.
//
// It preserves the pre-existing idc/social heuristics:
//   - empty authMethod + clientId present             -> idc
//   - empty authMethod, no clientId                   -> social
//   - "enterprise" (Kiro Account Manager IdC label)   -> idc
//   - unrecognized non-empty + clientId+clientSecret  -> idc, else social
func normalizeImportAuthMethod(authMethod, clientID, clientSecret, tokenEndpoint string) string {
	am := strings.ToLower(strings.TrimSpace(authMethod))
	switch {
	case externalIdpAuthMethodAliases[am]:
		return "external_idp"
	case am == "api_key" || am == "kiro_api_key" || am == "apikey" || am == "kiro-api-key":
		return "api_key"
	case am == "social" || am == "google" || am == "github":
		return "social"
	case am == "idc" || am == "builderid" || am == "enterprise":
		return "idc"
	}

	// Endpoint-based inference is deliberately weaker than an explicit authMethod.
	// Mixed/exported credential objects can carry tokenEndpoint as ancillary data;
	// that must not silently turn an explicitly declared IdC/social credential into
	// external_idp. Only an undeclared method may be inferred from the endpoint.
	if am == "" {
		if strings.TrimSpace(tokenEndpoint) != "" {
			return "external_idp"
		}
		if clientID != "" {
			return "idc"
		}
		return "social"
	}
	if clientID != "" && clientSecret != "" {
		return "idc"
	}
	return "social"
}

func looksLikeKiroAPIKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	// Convenience form: ksk_xxx|region
	if idx := strings.IndexByte(value, '|'); idx > 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return strings.HasPrefix(value, "ksk_")
}

func (h *Handler) apiStartMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	sessionID, authorizeURL, expiresIn, err := auth.StartMicrosoftSSOLogin()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeURL,
		"expiresIn":    expiresIn,
		"stage":        "kiro",
	})
}

func (h *Handler) apiCompleteMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackURL string `json:"callbackUrl"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.CallbackURL) == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and callbackUrl are required"})
		return
	}

	progress, err := auth.ContinueMicrosoftSSOLogin(req.SessionID, req.CallbackURL)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if h.microsoftSessionCanceled(req.SessionID) {
		auth.CancelMicrosoftSSOLogin(req.SessionID)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if progress.AuthorizationURL != "" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":      true,
			"stage":        "microsoft",
			"authorizeUrl": progress.AuthorizationURL,
		})
		return
	}
	if progress.Result == nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft SSO returned no credential"})
		return
	}

	result := progress.Result
	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		UserId:        result.UserID,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    auth.MicrosoftSSOAuthMethod,
		Provider:      auth.MicrosoftSSOProvider,
		Region:        "us-east-1",
		ExpiresAt:     result.ExpiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
	}

	discoveryContext, discovery, ok := h.beginMicrosoftProfileDiscovery(r.Context(), req.SessionID)
	if !ok {
		clearMicrosoftAccountCredential(&account)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	profiles, profileErr := DiscoverKiroProfilesContext(discoveryContext, &account)
	discoveryErr := discoveryContext.Err()
	h.endMicrosoftProfileDiscovery(req.SessionID, discovery)

	h.microsoftFlowMu.Lock()
	if h.microsoftSessionCanceledLocked(req.SessionID, time.Now()) {
		h.microsoftFlowMu.Unlock()
		clearMicrosoftAccountCredential(&account)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if discoveryErr != nil {
		h.microsoftFlowMu.Unlock()
		clearMicrosoftAccountCredential(&account)
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile discovery was canceled or timed out"})
		return
	}
	if len(profiles) > 1 {
		selectionID, expiredSelections, err := h.storeMicrosoftProfileSelection(req.SessionID, account, profiles)
		h.microsoftFlowMu.Unlock()
		discardDetachedMicrosoftProfileSelections(expiredSelections)
		if err != nil {
			clearMicrosoftAccountCredential(&account)
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":                  true,
			"stage":                    "profile",
			"requiresProfileSelection": true,
			"selectionId":              selectionID,
			"profiles":                 profiles,
		})
		return
	}
	if len(profiles) == 1 {
		account.ProfileArn = profiles[0].ARN
	}
	if err := config.AddAccount(account); err != nil {
		h.microsoftFlowMu.Unlock()
		h.writeAddAccountError(w, err)
		return
	}
	delete(h.microsoftCanceled, strings.TrimSpace(req.SessionID))
	h.microsoftFlowMu.Unlock()
	h.pool.Reload()

	response := map[string]interface{}{
		"success": true,
		"stage":   "complete",
		"account": map[string]interface{}{"id": account.ID, "email": account.Email},
	}
	if profileErr != nil {
		response["warning"] = "The account was added, but its Kiro profile could not be resolved yet"
	}
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) apiSelectMicrosoftSSOProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SelectionID string `json:"selectionId"`
		ProfileARN  string `json:"profileArn"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	selectionID := strings.TrimSpace(req.SelectionID)
	selection := h.getMicrosoftProfileSelection(selectionID)
	if selection == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile selection not found or expired"})
		return
	}

	selection.mu.Lock()
	if selection.canceled.Load() || !time.Now().Before(selection.ExpiresAt) {
		selection.mu.Unlock()
		h.removeMicrosoftProfileSelection(selectionID, selection)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile selection not found or expired"})
		return
	}

	profileARN := strings.TrimSpace(req.ProfileARN)
	allowed := false
	for _, profile := range selection.Profiles {
		if profile.ARN == profileARN {
			allowed = true
			break
		}
	}
	if !allowed {
		selection.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Selected Kiro profile was not offered for this login"})
		return
	}

	account := selection.Account
	account.ProfileArn = profileARN
	h.microsoftFlowMu.Lock()
	now := time.Now()
	if selection.canceled.Load() ||
		!now.Before(selection.ExpiresAt) ||
		h.microsoftSessionCanceledLocked(selection.SessionID, now) {
		h.microsoftFlowMu.Unlock()
		h.detachMicrosoftProfileSelection(selectionID, selection)
		selection.canceled.Store(true)
		selection.Account = config.Account{}
		selection.Profiles = nil
		selection.mu.Unlock()
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if err := config.AddAccount(account); err != nil {
		h.microsoftFlowMu.Unlock()
		selection.mu.Unlock()
		h.writeAddAccountError(w, err)
		return
	}
	h.detachMicrosoftProfileSelection(selectionID, selection)
	selection.canceled.Store(true)
	selection.Account = config.Account{}
	selection.Profiles = nil
	delete(h.microsoftCanceled, selection.SessionID)
	h.microsoftFlowMu.Unlock()
	selection.mu.Unlock()
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"stage":   "complete",
		"account": map[string]interface{}{"id": account.ID, "email": account.Email},
	})
}

func (h *Handler) apiCancelMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		SelectionID string `json:"selectionId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)
	selectionID := strings.TrimSpace(req.SelectionID)
	if sessionID == "" && selectionID != "" {
		if selection := h.getMicrosoftProfileSelection(selectionID); selection != nil {
			sessionID = selection.SessionID
		}
	}
	if sessionID != "" {
		h.markMicrosoftSessionCanceled(sessionID)
	}
	auth.CancelMicrosoftSSOLogin(sessionID)
	if selectionID != "" {
		h.removeMicrosoftProfileSelection(selectionID, nil)
	}
	if sessionID != "" {
		h.removeMicrosoftProfileSelectionsForSession(sessionID)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) storeMicrosoftProfileSelection(
	sessionID string,
	account config.Account,
	profiles []KiroProfile,
) (string, []*microsoftProfileSelection, error) {
	now := time.Now()
	expiresAt := now.Add(microsoftProfileSelectionTTL)
	tokenExpiry := time.Unix(account.ExpiresAt, 0)
	if account.ExpiresAt > 0 && tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	if !expiresAt.After(now) {
		return "", nil, fmt.Errorf("Microsoft credential expired before profile selection")
	}
	selectionID := uuid.NewString()
	selection := &microsoftProfileSelection{
		SessionID: strings.TrimSpace(sessionID),
		Account:   account,
		Profiles:  append([]KiroProfile(nil), profiles...),
		ExpiresAt: expiresAt,
	}
	var expired []*microsoftProfileSelection

	h.microsoftSelectionsMu.Lock()
	if h.microsoftSelections == nil {
		h.microsoftSelections = make(map[string]*microsoftProfileSelection)
	}
	for id, current := range h.microsoftSelections {
		if !now.Before(current.ExpiresAt) {
			delete(h.microsoftSelections, id)
			current.canceled.Store(true)
			if current.timer != nil {
				current.timer.Stop()
				current.timer = nil
			}
			expired = append(expired, current)
		}
	}
	if len(h.microsoftSelections) >= microsoftMaxPendingProfileSelections {
		h.microsoftSelectionsMu.Unlock()
		return "", expired, fmt.Errorf("too many pending Microsoft profile selections; cancel one and try again")
	}
	h.microsoftSelections[selectionID] = selection
	selection.timer = time.AfterFunc(time.Until(expiresAt), func() {
		h.removeMicrosoftProfileSelection(selectionID, selection)
	})
	h.microsoftSelectionsMu.Unlock()
	return selectionID, expired, nil
}

func (h *Handler) getMicrosoftProfileSelection(selectionID string) *microsoftProfileSelection {
	if selectionID == "" {
		return nil
	}
	h.microsoftSelectionsMu.Lock()
	selection := h.microsoftSelections[selectionID]
	if selection != nil && !time.Now().Before(selection.ExpiresAt) {
		delete(h.microsoftSelections, selectionID)
		selection.canceled.Store(true)
		if selection.timer != nil {
			selection.timer.Stop()
			selection.timer = nil
		}
		h.microsoftSelectionsMu.Unlock()
		discardDetachedMicrosoftProfileSelection(selection)
		return nil
	}
	h.microsoftSelectionsMu.Unlock()
	return selection
}

func (h *Handler) detachMicrosoftProfileSelection(
	selectionID string,
	expected *microsoftProfileSelection,
) *microsoftProfileSelection {
	selectionID = strings.TrimSpace(selectionID)
	if selectionID == "" {
		return nil
	}
	h.microsoftSelectionsMu.Lock()
	current := h.microsoftSelections[selectionID]
	if current != nil && (expected == nil || current == expected) {
		delete(h.microsoftSelections, selectionID)
		current.canceled.Store(true)
		if current.timer != nil {
			current.timer.Stop()
			current.timer = nil
		}
	} else {
		current = nil
	}
	h.microsoftSelectionsMu.Unlock()
	return current
}

func (h *Handler) removeMicrosoftProfileSelection(selectionID string, expected *microsoftProfileSelection) {
	if selection := h.detachMicrosoftProfileSelection(selectionID, expected); selection != nil {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func (h *Handler) removeMicrosoftProfileSelectionsForSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	var removed []*microsoftProfileSelection
	h.microsoftSelectionsMu.Lock()
	for selectionID, selection := range h.microsoftSelections {
		if selection.SessionID == sessionID {
			delete(h.microsoftSelections, selectionID)
			selection.canceled.Store(true)
			if selection.timer != nil {
				selection.timer.Stop()
				selection.timer = nil
			}
			removed = append(removed, selection)
		}
	}
	h.microsoftSelectionsMu.Unlock()
	for _, selection := range removed {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func discardDetachedMicrosoftProfileSelection(selection *microsoftProfileSelection) {
	selection.mu.Lock()
	selection.Account = config.Account{}
	selection.Profiles = nil
	selection.mu.Unlock()
}

func discardDetachedMicrosoftProfileSelections(selections []*microsoftProfileSelection) {
	for _, selection := range selections {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func clearMicrosoftAccountCredential(account *config.Account) {
	account.AccessToken = ""
	account.RefreshToken = ""
	account.ClientSecret = ""
}

func (h *Handler) markMicrosoftSessionCanceled(sessionID string) {
	now := time.Now()
	h.microsoftFlowMu.Lock()
	if h.microsoftCanceled == nil {
		h.microsoftCanceled = make(map[string]time.Time)
	}
	h.cleanupMicrosoftCanceledLocked(now)
	if len(h.microsoftCanceled) >= microsoftMaxCanceledSessionTombstones {
		var oldestID string
		var oldestExpiry time.Time
		for id, expiry := range h.microsoftCanceled {
			if oldestID == "" || expiry.Before(oldestExpiry) {
				oldestID = id
				oldestExpiry = expiry
			}
		}
		delete(h.microsoftCanceled, oldestID)
	}
	h.microsoftCanceled[sessionID] = now.Add(microsoftCanceledSessionTTL)
	if discovery := h.microsoftDiscoveries[sessionID]; discovery != nil {
		discovery.cancel()
	}
	h.microsoftFlowMu.Unlock()
}

func (h *Handler) beginMicrosoftProfileDiscovery(
	parent context.Context,
	sessionID string,
) (context.Context, *microsoftProfileDiscovery, bool) {
	sessionID = strings.TrimSpace(sessionID)
	h.microsoftFlowMu.Lock()
	defer h.microsoftFlowMu.Unlock()
	if h.microsoftSessionCanceledLocked(sessionID, time.Now()) {
		return nil, nil, false
	}
	if h.microsoftDiscoveries == nil {
		h.microsoftDiscoveries = make(map[string]*microsoftProfileDiscovery)
	}
	if h.microsoftDiscoveries[sessionID] != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(parent, microsoftProfileDiscoveryTimeout)
	discovery := &microsoftProfileDiscovery{cancel: cancel}
	h.microsoftDiscoveries[sessionID] = discovery
	return ctx, discovery, true
}

func (h *Handler) endMicrosoftProfileDiscovery(sessionID string, expected *microsoftProfileDiscovery) {
	expected.cancel()
	h.microsoftFlowMu.Lock()
	if h.microsoftDiscoveries[strings.TrimSpace(sessionID)] == expected {
		delete(h.microsoftDiscoveries, strings.TrimSpace(sessionID))
	}
	h.microsoftFlowMu.Unlock()
}

func (h *Handler) microsoftSessionCanceled(sessionID string) bool {
	h.microsoftFlowMu.Lock()
	defer h.microsoftFlowMu.Unlock()
	return h.microsoftSessionCanceledLocked(sessionID, time.Now())
}

func (h *Handler) microsoftSessionCanceledLocked(sessionID string, now time.Time) bool {
	h.cleanupMicrosoftCanceledLocked(now)
	expiry, exists := h.microsoftCanceled[strings.TrimSpace(sessionID)]
	return exists && now.Before(expiry)
}

func (h *Handler) cleanupMicrosoftCanceledLocked(now time.Time) {
	for sessionID, expiry := range h.microsoftCanceled {
		if !now.Before(expiry) {
			delete(h.microsoftCanceled, sessionID)
		}
	}
}

func (h *Handler) writeMicrosoftSSOCanceled(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft SSO login was canceled"})
}

func (h *Handler) writeAddAccountError(w http.ResponseWriter, err error, rotatedRefreshToken ...string) {
	if errors.Is(err, config.ErrDuplicateAccountID) ||
		errors.Is(err, config.ErrDuplicateRefreshToken) ||
		errors.Is(err, config.ErrDuplicateAPIKey) {
		w.WriteHeader(http.StatusConflict)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	payload := map[string]string{"error": err.Error()}
	if len(rotatedRefreshToken) > 0 {
		if rotated := strings.TrimSpace(rotatedRefreshToken[0]); rotated != "" {
			// Microsoft may have already invalidated the original refresh token.
			// Surface the rotated value so operators can retry import without a
			// full interactive re-login.
			payload["rotatedRefreshToken"] = rotated
			payload["hint"] = "The identity provider rotated the refresh token before persistence failed; retry import with rotatedRefreshToken"
		}
	}
	json.NewEncoder(w).Encode(payload)
}

// genericExternalIdpTokenResponse is the OAuth2 token-endpoint response shape
// shared by any enterprise IdP that speaks the standard refresh_token grant
// (RFC 6749 §6) — the same wire format Microsoft's endpoint uses, but this
// path is NOT Microsoft-specific: see refreshGenericExternalIdpToken.
type genericExternalIdpTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// genericExternalIdpMaxResponseBytes bounds the token-endpoint response body:
// an attacker-controlled (but allow-listed-host) IdP could otherwise stream an
// unbounded body at a still-open connection.
const genericExternalIdpMaxResponseBytes = 256 << 10

func refreshGenericExternalIdpToken(refreshToken, clientID, tokenEndpoint, scopes string) (accessToken, newRefreshToken string, expiresAt int64, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	clientID = strings.TrimSpace(clientID)
	tokenEndpoint = strings.TrimSpace(tokenEndpoint)
	if refreshToken == "" || clientID == "" || tokenEndpoint == "" {
		return "", "", 0, fmt.Errorf("external IdP refresh requires refreshToken, clientId, and tokenEndpoint")
	}
	if err := auth.ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return "", "", 0, fmt.Errorf("external IdP token endpoint rejected: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)
	if strings.TrimSpace(scopes) != "" {
		form.Set("scope", scopes)
	}

	request, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, fmt.Errorf("build external IdP token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	client := auth.GetAuthClientForProxy(config.GetProxyURL())
	noRedirect := *client
	if noRedirect.Timeout == 0 {
		noRedirect.Timeout = 30 * time.Second
	}
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	response, err := noRedirect.Do(request)
	if err != nil {
		return "", "", 0, fmt.Errorf("external IdP token request failed: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, genericExternalIdpMaxResponseBytes+1))
	if err != nil {
		return "", "", 0, fmt.Errorf("read external IdP token response: %w", err)
	}
	if len(body) > genericExternalIdpMaxResponseBytes {
		return "", "", 0, fmt.Errorf("external IdP token response exceeds %d bytes", genericExternalIdpMaxResponseBytes)
	}

	var token genericExternalIdpTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return "", "", 0, fmt.Errorf("parse external IdP token response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if token.Error != "" {
			msg := token.Error
			if token.ErrorDescription != "" {
				msg += ": " + token.ErrorDescription
			}
			return "", "", 0, fmt.Errorf("HTTP %d: %s", response.StatusCode, msg)
		}
		return "", "", 0, fmt.Errorf("external IdP token endpoint returned HTTP %d", response.StatusCode)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return "", "", 0, fmt.Errorf("external IdP token response is missing access_token")
	}
	if token.ExpiresIn <= 0 {
		return "", "", 0, fmt.Errorf("external IdP token response has invalid expires_in")
	}
	newRefreshToken = token.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = refreshToken
	}
	return token.AccessToken, newRefreshToken, time.Now().Unix() + token.ExpiresIn, nil
}

type importCredentialResult struct {
	Account             config.Account
	RotatedRefreshToken string
}

type importValidationError struct{ msg string }

func (e *importValidationError) Error() string { return e.msg }

func importErrorStatus(err error) int {
	if _, ok := err.(*importValidationError); ok {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (h *Handler) importCredential(req importCredentialRequest) (importCredentialResult, error) {
	req.RefreshToken = strings.TrimSpace(req.RefreshToken)
	req.KiroApiKey = strings.TrimSpace(req.KiroApiKey)
	req.AccessToken = strings.TrimSpace(req.AccessToken)
	methodHint := strings.ToLower(strings.TrimSpace(req.AuthMethod))
	isAPIKeyImport := req.KiroApiKey != "" ||
		methodHint == "api_key" || methodHint == "apikey" || methodHint == "kiro_api_key" ||
		(req.RefreshToken == "" && looksLikeKiroAPIKey(req.AccessToken))
	if isAPIKeyImport && req.KiroApiKey == "" {
		// Allow plain-text / AccessToken-only API key imports.
		req.KiroApiKey = req.AccessToken
	}
	if !isAPIKeyImport && req.RefreshToken == "" {
		return importCredentialResult{}, &importValidationError{"refreshToken or kiroApiKey is required"}
	}
	if len(req.RefreshToken) > 512<<10 || len(req.AccessToken) > 512<<10 || len(req.KiroApiKey) > 512<<10 {
		return importCredentialResult{}, &importValidationError{"credential token is too long"}
	}
	originalRefreshToken := req.RefreshToken
	h.credentialImportMu.Lock()
	defer h.credentialImportMu.Unlock()
	accountID := strings.TrimSpace(req.ID)
	if accountID != "" {
		if _, err := uuid.Parse(accountID); err != nil {
			return importCredentialResult{}, &importValidationError{"id must be a UUID"}
		}
		if config.AccountIDExists(accountID) {
			return importCredentialResult{}, config.ErrDuplicateAccountID
		}
	}

	// API Key import path: no OAuth refresh, no profileArn.
	if isAPIKeyImport {
		if accountID == "" {
			accountID = auth.GenerateAccountID()
		}
		// Split any "key|region" suffix up front (same convenience form
		// config.NormalizeAPIKeyAccount parses below) so the probe runs against the
		// bare key and an inline region is honored even when req.Region is empty.
		bareKey, embeddedRegion, splitErr := config.SplitKiroAPIKeyAndRegion(req.KiroApiKey)
		if splitErr != nil {
			return importCredentialResult{}, &importValidationError{splitErr.Error()}
		}
		req.KiroApiKey = bareKey
		if strings.TrimSpace(req.Region) == "" {
			req.Region = embeddedRegion
		}
		// Region trap (mirrors apiAddAccount): probe live so a stale or mistyped
		// region doesn't restore as a permanently-403ing pool slot. BEST-EFFORT
		// ONLY — a probe failure (network unreachable, transient upstream outage,
		// or a test/offline harness with egress disabled) must not block the
		// import; trust-on-import is the fallback contract this route's callers
		// (CLI credential export/restore) rely on. When the probe can't confirm a
		// region, NormalizeAPIKeyAccount below still applies the usual
		// "key|region" / default-region rules.
		region, info, _, probeErr := resolveApiKeyRegion(req.KiroApiKey, req.Region)
		if probeErr != nil {
			logger.Warnf("[ImportCredentials] api_key region probe failed, importing without live verification: %v", probeErr)
			// H1: a failed probe means we have NO live confirmation that this key
			// actually belongs to the caller-supplied Email/UserId — yet those are
			// exactly what findAccountForKiroIdentity (admin_bot_api.go) later
			// matches on to recognize "the same Kiro account". Trusting them here
			// unverified would let an import with a wrong/mistyped key plant a slot
			// carrying a REAL account's identity; a later legitimate
			// POST /admin/add_kiro_account for that same identity would then either
			// get discarded as a false "duplicate" or, worse, overwrite this
			// unverified slot's live credential into the planted one. Region/key
			// still import (trust-on-import stays the contract for the CLI
			// export/restore path this route serves), but identity fields that
			// were never live-verified must not ride along.
			req.Email = ""
			req.UserID = ""
		} else {
			req.Region = region
			// Prefer the identity supplied by the caller (a full account record),
			// falling back to what the probe just fetched. UserId matters beyond
			// display: it is how findAccountForKiroIdentity recognizes two keys
			// minted by the same Kiro account, so an imported slot without it gets
			// duplicated by a later supply-side add (double routing weight,
			// double-counted quota) until a refresh backfills it.
			if info != nil {
				if strings.TrimSpace(req.Email) == "" {
					req.Email = info.Email
				}
				if strings.TrimSpace(req.UserID) == "" {
					req.UserID = info.UserId
				}
			}
		}
		account := config.Account{
			ID:         accountID,
			Email:      strings.TrimSpace(req.Email),
			UserId:     strings.TrimSpace(req.UserID),
			Nickname:   strings.TrimSpace(req.Nickname),
			KiroApiKey: req.KiroApiKey,
			AuthMethod: "api_key",
			Provider:   strings.TrimSpace(req.Provider),
			Region:     strings.TrimSpace(req.Region),
			ProfileArn: strings.TrimSpace(req.ProfileARN),
			Enabled:    true,
		}
		if err := config.NormalizeAPIKeyAccount(&account); err != nil {
			return importCredentialResult{}, &importValidationError{err.Error()}
		}
		if config.AccountAPIKeyExists(account.KiroApiKey) {
			return importCredentialResult{}, config.ErrDuplicateAPIKey
		}
		if err := config.AddAccount(account); err != nil {
			return importCredentialResult{}, err
		}
		if account.Enabled && account.AccessToken != "" {
			acc := account.Clone()
			h.startBackgroundTaskContext(func(taskCtx context.Context) {
				ctx, cancel := context.WithTimeout(taskCtx, 2*time.Minute)
				defer cancel()
				if err := h.fetchAndCacheAccountModelsContext(ctx, &acc); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for new API key account %s: %v", accountIdentityForLog(&acc), err)
				}
			})
		}
		return importCredentialResult{Account: account}, nil
	}

	if config.AccountCredentialExists(originalRefreshToken) {
		return importCredentialResult{}, config.ErrDuplicateRefreshToken
	}

	// 设置默认值
	req.Region = strings.TrimSpace(req.Region)
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	// 标准化 authMethod。external_idp 必须先于 clientId+clientSecret→idc 的推断被识别
	//（external_idp 带 clientId 但没有 clientSecret），否则会被误判成 social 而 refresh 到错误端点。
	// This alias pre-pass collapses the pasted method onto one of external_idp/idc/
	// social; upstream's classifier below then layers its Microsoft/Entra *provider*
	// aliases on top (a blob labelled social but sourced from provider "entra" still
	// lands on the external-IdP path).
	method := normalizeImportAuthMethod(req.AuthMethod, req.ClientID, req.ClientSecret, req.TokenEndpoint)
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	// Resolve Azure endpoints from userId (Kiro export, account level) or the
	// accessToken JWT issuer (bare blobs: clientId + token only).
	derivedTokenEndpoint, derivedIssuer, derivedScopes := auth.DeriveExternalIdpEndpoints(
		req.UserID, req.ClientID, req.AccessToken,
	)
	// Explicit AWS/social methods win over incidental tokenEndpoint/issuerUrl
	// fields so mixed JSON templates cannot force the external-IdP path.
	am := strings.ToLower(strings.TrimSpace(req.AuthMethod))
	explicitMicrosoft := am == "azuread" || am == "azure" || am == "azure-ad" || am == "azure_ad" ||
		am == "entra" || am == "entra-id" || am == "entra_id" || am == "microsoft" || am == "m365" || am == "office365" ||
		provider == "external" || provider == "microsoft" || provider == "m365" || provider == "office365" ||
		provider == "azure" || provider == "azuread" || provider == "azure-ad" || provider == "azure_ad" ||
		provider == "entra" || provider == "entra-id"
	switch {
	case explicitMicrosoft || method == "external_idp":
		req.AuthMethod = auth.MicrosoftSSOAuthMethod
	case method == "idc":
		req.AuthMethod = "idc"
	case method == "social":
		req.AuthMethod = "social"
	case req.ClientID != "" && req.ClientSecret != "":
		req.AuthMethod = "idc"
	default:
		req.AuthMethod = "social"
	}
	// A derivation that also clears the allow-list is itself proof the credential is
	// external_idp (Microsoft-shaped or not) — IdC/social access tokens are not
	// microsoftonline-tenant-shaped JWTs, so a bare IdC blob (its iss is an AWS
	// host) won't clear the list and won't be misclassified. This catches Kiro
	// exports that carry only userId + accessToken. NOTE: this only decides
	// AuthMethod (external_idp vs idc/social); it does NOT imply "genuinely
	// Microsoft" — see microsoftClassified below.
	explicitNonExternal := am == "idc" || am == "builderid" || am == "enterprise" ||
		am == "social" || am == "google" || am == "github"
	if !explicitNonExternal && req.AuthMethod != auth.MicrosoftSSOAuthMethod && derivedTokenEndpoint != "" &&
		auth.ValidateExternalIdpEndpoint(derivedTokenEndpoint) == nil {
		req.AuthMethod = auth.MicrosoftSSOAuthMethod
	}
	// auth.MicrosoftSSOAuthMethod is literally the string "external_idp" — the SAME
	// persisted AuthMethod this route's older, non-Microsoft external_idp contract
	// also uses (arbitrary enterprise IdP, allow-list gated only). So req.AuthMethod
	// alone can never distinguish "genuinely Microsoft" (strict client_id/tenant
	// validation, mandatory live refresh) from "generic" (loose allow-list only,
	// trust-on-import) below — both features must keep working post-merge.
	//
	// client_id UUID shape is NOT a usable signal here: both a genuine Microsoft
	// app registration AND a generic IdP's client_id can be a UUID (the fixture
	// covering this exact ambiguity uses a UUID client_id with a bare {iss,exp}
	// JWT and must classify as generic). Two reliable signals remain:
	//   - the accessToken's own JWT claims: only Entra issues oid/tid/preferred_username.
	//   - the (caller-supplied, falling back to derived) tokenEndpoint's host: a
	//     credential can omit accessToken entirely (e.g. a full external_idp record
	//     pasted with tokenEndpoint/issuerUrl but no token yet) and still be
	//     genuinely Microsoft. auth.IsMicrosoftLoginHost checks the REAL Entra host
	//     allow-list directly — not the swappable auth.ValidateExternalIdpEndpoint,
	//     which tests deliberately relax so an httptest server can stand in for a
	//     generic IdP; using the swappable one here would let that test-only SSRF
	//     bypass masquerade as "genuinely Microsoft".
	hasMicrosoftClaims := req.AccessToken != "" && auth.IsMicrosoftAccessTokenJWT(req.AccessToken)
	effectiveTokenEndpoint := strings.TrimSpace(req.TokenEndpoint)
	if effectiveTokenEndpoint == "" {
		effectiveTokenEndpoint = derivedTokenEndpoint
	}
	isMicrosoftHost := effectiveTokenEndpoint != "" && auth.IsMicrosoftLoginHost(effectiveTokenEndpoint)
	microsoftClassified := req.AuthMethod == auth.MicrosoftSSOAuthMethod &&
		(explicitMicrosoft || hasMicrosoftClaims || isMicrosoftHost)

	// 用 refreshToken 刷新获取新的 accessToken。导入必须以一次成功的刷新为前提：
	// 本地缓存里的 accessToken 不携带可信的过期时间，盲猜短 TTL 会让账号在选号时
	// 永远被跳过，导致后台/按需刷新都无法触发（详见 ensureValidToken 与 Pick 的过期判定）。
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.TokenEndpoint = strings.TrimSpace(req.TokenEndpoint)
	req.IssuerURL = strings.TrimRight(strings.TrimSpace(req.IssuerURL), "/")
	req.Scopes = strings.TrimSpace(req.Scopes)

	if req.AuthMethod == auth.MicrosoftSSOAuthMethod && microsoftClassified {
		// Genuine Microsoft/Entra app registration: full tenant discovery (endpoint
		// <-> issuer cross-derivation) plus the strict client_id (UUID) + tenant-bound
		// issuer/endpoint match, matching upstream's contract for this path.
		if req.IssuerURL == "" {
			req.IssuerURL = derivedIssuer
		}
		if req.IssuerURL == "" && req.TokenEndpoint != "" {
			normalizedTokenEndpoint, tokenIssuer, tokenScopes := auth.ExternalIdpConfigurationFromTokenEndpoint(
				req.TokenEndpoint, req.ClientID,
			)
			if normalizedTokenEndpoint != "" {
				req.TokenEndpoint = normalizedTokenEndpoint
				req.IssuerURL = tokenIssuer
				if req.Scopes == "" {
					req.Scopes = tokenScopes
				}
			}
		}
		if req.IssuerURL != "" {
			builtTokenEndpoint, normalizedIssuer, builtScopes := auth.ExternalIdpConfigurationFromIssuer(req.IssuerURL, req.ClientID)
			if req.TokenEndpoint == "" {
				req.TokenEndpoint = builtTokenEndpoint
			}
			if normalizedIssuer != "" {
				req.IssuerURL = normalizedIssuer
			}
			if req.Scopes == "" {
				req.Scopes = builtScopes
			}
		}
		if req.TokenEndpoint == "" {
			req.TokenEndpoint = derivedTokenEndpoint
		}
		if req.Scopes == "" {
			req.Scopes = derivedScopes
		}
		normalizedScopes, err := auth.NormalizeExternalIdpScopes(req.Scopes, req.ClientID)
		if err != nil {
			return importCredentialResult{}, &importValidationError{err.Error()}
		}
		req.Scopes = normalizedScopes
		if err := auth.ValidateExternalIdpConfiguration(req.ClientID, req.TokenEndpoint, req.IssuerURL, req.Scopes); err != nil {
			return importCredentialResult{}, &importValidationError{err.Error()}
		}
		req.Provider = auth.MicrosoftSSOProvider
		req.ClientSecret = ""
	} else if req.AuthMethod == auth.MicrosoftSSOAuthMethod {
		// Generic external IdP (pre-merge "ours" contract): no Microsoft tenant
		// discovery or client_id shape requirement — just fill from whatever the
		// caller/derivation gave us, then enforce the same allow-list-only SSRF
		// guard (auth.ValidateExternalIdpEndpoint) this route always had. A
		// non-UUID client_id can never satisfy the strict Microsoft branch above,
		// so this is the only path such a credential can import through.
		if req.TokenEndpoint == "" {
			req.TokenEndpoint = derivedTokenEndpoint
		}
		if req.IssuerURL == "" {
			req.IssuerURL = derivedIssuer
		}
		if req.Scopes == "" {
			req.Scopes = derivedScopes
		}
		if req.ClientID == "" || req.TokenEndpoint == "" {
			return importCredentialResult{}, &importValidationError{
				"external_idp requires clientId and token_endpoint (or userId/accessToken to derive it)",
			}
		}
		if err := auth.ValidateExternalIdpEndpoint(req.TokenEndpoint); err != nil {
			return importCredentialResult{}, &importValidationError{"external IdP endpoint rejected: " + err.Error()}
		}
		if req.IssuerURL != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.IssuerURL); err != nil {
				return importCredentialResult{}, &importValidationError{"external IdP issuer rejected: " + err.Error()}
			}
		}
		if strings.TrimSpace(req.Provider) == "" {
			req.Provider = auth.MicrosoftSSOProvider // "AzureAD" — this route's pre-existing default
		}
	}

	profileARN := strings.TrimSpace(req.ProfileARN)
	if profileARN != "" {
		canonicalARN, _, ok := parseKiroProfileArn(profileARN)
		if !ok {
			return importCredentialResult{}, &importValidationError{"profileArn is invalid"}
		}
		profileARN = canonicalARN
	}

	tempAccount := &config.Account{
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Region:        req.Region,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		Scopes:        req.Scopes,
	}

	// Resolve the access token to persist. For external_idp we prefer TRUST-ON-IMPORT:
	// when the pasted JSON carries an Azure AD access token (a JWT with a real exp),
	// persist it directly WITHOUT a live refresh round-trip. The JSON can then be
	// imported repeatedly / into multiple instances without each import consuming
	// (rotating) the refresh token, and without requiring egress to Microsoft at
	// import time. The runtime background refresh (backgroundRefresh /
	// ensureValidToken) renews it later when the account is actually used. Falls
	// back to refresh-at-import for idc/social and for external_idp credentials
	// carrying only a refreshToken (so the regression gate — reject when refresh
	// fails — still holds there).
	var (
		accessToken   string
		expiresAt     int64
		newProfileArn string
	)
	if req.AuthMethod == auth.MicrosoftSSOAuthMethod && !microsoftClassified && req.AccessToken != "" {
		if exp := auth.ExpFromAccessTokenJWT(req.AccessToken); exp > 0 {
			accessToken = req.AccessToken
			expiresAt = exp
		}
	}
	if accessToken == "" && req.AuthMethod == auth.MicrosoftSSOAuthMethod && !microsoftClassified {
		// Generic (non-Microsoft) external IdP: auth.RefreshToken would route this
		// through auth/oidc.go's Microsoft-only dispatch, which unconditionally
		// requires a UUID client_id and tenant-bound issuer/endpoint match — a
		// generic enterprise IdP has neither. Use the plain OAuth2 refresh_token
		// grant instead (same allow-list guard, no Microsoft-specific validation).
		a, newRefreshToken, ea, err := refreshGenericExternalIdpToken(req.RefreshToken, req.ClientID, req.TokenEndpoint, req.Scopes)
		if err != nil {
			return importCredentialResult{}, &importValidationError{"Token refresh failed: " + err.Error()}
		}
		accessToken = a
		expiresAt = ea
		if newRefreshToken != "" {
			req.RefreshToken = newRefreshToken
		}
	}
	if accessToken == "" {
		a, newRefreshToken, ea, pa, err := auth.RefreshToken(tempAccount)
		if err != nil {
			return importCredentialResult{}, &importValidationError{"Token refresh failed: " + err.Error()}
		}
		accessToken = a
		expiresAt = ea
		newProfileArn = pa
		if newRefreshToken != "" {
			req.RefreshToken = newRefreshToken
		}
	}
	rotatedRefreshToken := ""
	if req.RefreshToken != "" && req.RefreshToken != originalRefreshToken {
		rotatedRefreshToken = req.RefreshToken
	}

	// 获取用户信息
	email := strings.TrimSpace(req.Email)
	userID := strings.TrimSpace(req.UserID)
	if microsoftClassified {
		tokenEmail, tokenUserID := auth.ExternalIdpTokenIdentity(accessToken)
		if tokenEmail != "" {
			email = tokenEmail
		}
		if tokenUserID != "" {
			userID = tokenUserID
		}
	} else if tokenEmail, _, _ := auth.GetUserInfo(accessToken); tokenEmail != "" {
		email = tokenEmail
	}

	if accountID == "" {
		accountID = auth.GenerateAccountID()
	}
	if profileARN == "" {
		profileARN = newProfileArn
	}
	// Interactive Microsoft login only accepts profiles discovered for the
	// refreshed token. Import keeps the same trust boundary so a client cannot
	// pin an arbitrary data-plane ARN onto a working credential.
	if microsoftClassified && profileARN != "" {
		probeAccount := *tempAccount
		probeAccount.AccessToken = accessToken
		probeAccount.RefreshToken = req.RefreshToken
		probeAccount.ExpiresAt = expiresAt
		offeredProfiles, discoverErr := DiscoverKiroProfiles(&probeAccount)
		if discoverErr != nil {
			return importCredentialResult{}, &importValidationError{
				"Unable to verify profileArn against Kiro profiles: " + discoverErr.Error(),
			}
		}
		offered := false
		for _, profile := range offeredProfiles {
			if profile.ARN == profileARN {
				offered = true
				break
			}
		}
		if !offered {
			return importCredentialResult{}, &importValidationError{"profileArn was not offered for this credential"}
		}
	}

	// 创建账号
	// external_idp records imported without an explicit provider still need one for
	// the UI badge; the Microsoft branch above already stamps auth.MicrosoftSSOProvider,
	// so this only backfills hand-written blobs.
	if strings.TrimSpace(req.Provider) == "" && req.AuthMethod == auth.MicrosoftSSOAuthMethod {
		req.Provider = auth.MicrosoftSSOProvider
	}
	account := config.Account{
		ID:                      accountID,
		Email:                   email,
		UserId:                  userID,
		Nickname:                strings.TrimSpace(req.Nickname),
		AccessToken:             accessToken,
		RefreshToken:            req.RefreshToken,
		RefreshTokenFingerprint: config.RefreshTokenFingerprint(originalRefreshToken),
		ClientID:                req.ClientID,
		ClientSecret:            req.ClientSecret,
		AuthMethod:              req.AuthMethod,
		Provider:                req.Provider,
		Region:                  req.Region,
		ExpiresAt:               expiresAt,
		Enabled:                 true,
		MachineId:               config.GenerateMachineId(),
		ProfileArn:              profileARN,
		TokenEndpoint:           req.TokenEndpoint,
		IssuerURL:               req.IssuerURL,
		Scopes:                  req.Scopes,
	}

	if err := config.AddAccount(account); err != nil {
		return importCredentialResult{RotatedRefreshToken: rotatedRefreshToken}, err
	}

	return importCredentialResult{Account: account}, nil
}

func (h *Handler) apiImportCliJson(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}

	reqs, warnings, err := normalizeCliJson(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  false,
			"error":    err.Error(),
			"warnings": warnings,
		})
		return
	}

	var imported []map[string]interface{}
	var errs []string
	for i, req := range reqs {
		result, impErr := h.importCredential(req)
		if impErr != nil {
			// FIX5: a persistence failure (duplicate id/refresh-token/api-key, or a
			// config write error) can land AFTER the IdP already rotated the
			// refresh token — the original is now dead at the IdP and, without
			// surfacing the rotated value here, the credential would be
			// permanently bricked (needs a full interactive re-login). The other
			// two import routes (apiImportCredentials, apiImportIdeCache) already
			// surface this via writeAddAccountError; do the same here for the
			// batch/per-item shape.
			msg := fmt.Sprintf("item %d: %s", i+1, impErr.Error())
			if rotated := strings.TrimSpace(result.RotatedRefreshToken); rotated != "" {
				msg = fmt.Sprintf("%s (identity provider rotated the refresh token before persistence failed; retry with rotatedRefreshToken=%s)", msg, rotated)
			}
			errs = append(errs, msg)
			continue
		}
		imported = append(imported, map[string]interface{}{
			"id":         result.Account.ID,
			"email":      result.Account.Email,
			"authMethod": result.Account.AuthMethod,
		})
	}

	if len(imported) > 0 {
		h.pool.Reload()
	}

	// Match the batch convention in apiImportSsoToken: 500 only when nothing landed.
	if len(imported) == 0 {
		w.WriteHeader(500)
		errMsg := "no credentials imported"
		if len(errs) > 0 {
			errMsg = strings.Join(errs, "; ")
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  false,
			"error":    errMsg,
			"warnings": warnings,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"imported": imported,
		"errors":   errs,
		"warnings": warnings,
	})
}

func (h *Handler) apiImportIdeCache(w http.ResponseWriter, r *http.Request) {
	// Cap the body like every other import route (apiImportCredentials,
	// apiImportCliJson) — even though its one field is now ignored (see
	// below), a client can still send an oversized body that would otherwise
	// be read into memory unbounded.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Path string `json:"path"`
	}
	// Body is optional; ignore a decode error (including an empty body). Decoding
	// is kept only so a client posting a (now-inert) legacy body still gets a
	// tolerant-of-garbage 200 path rather than a hard decode error.
	//
	// body.Path is deliberately NEVER read below. It used to override the cache
	// file location via ideCachePath(body.Path), but that field is
	// REQUEST-controlled, not operator-controlled: handleAdminAPI accepts a
	// cookie with no CSRF token, so a browser-delivered cross-site POST can
	// drive this route. {"path":"/dev/zero"} was an uncapped os.ReadFile -> OOM
	// (concrete DoS); {"path":"/etc/shadow"} was a file-existence/readability
	// oracle via distinct error strings. The cache path is now resolved only
	// from KIRO_IDE_CACHE / the default — see ideCachePath.
	_ = json.NewDecoder(r.Body).Decode(&body)

	path := ideCachePath("")
	req, err := readIdeCacheCredential(path)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	result, err := h.importCredential(req)
	if err != nil {
		if _, ok := err.(*importValidationError); ok {
			w.WriteHeader(importErrorStatus(err))
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.writeAddAccountError(w, err, result.RotatedRefreshToken)
		return
	}

	h.pool.Reload()
	logger.Infof("[Import] %s (account %s)", describeIdeCacheImport(path, result.Account.AuthMethod), result.Account.Email)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"source":  path,
		"account": map[string]interface{}{
			"id":         result.Account.ID,
			"email":      result.Account.Email,
			"authMethod": result.Account.AuthMethod,
		},
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   h.totalRequests,
		"successRequests": h.successRequests,
		"failedRequests":  h.failedRequests,
		"totalTokens":     h.totalTokens,
		"totalCredits":    h.totalCredits,
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	responseLanguage := config.GetResponseLanguageConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":                 config.GetApiKey(),
		"requireApiKey":          config.IsApiKeyRequired(),
		"port":                   config.GetPort(),
		"host":                   config.GetHost(),
		"allowOverUsage":         config.GetAllowOverUsage(),
		"agentToolSteering":      config.GetAgentToolSteering(),
		"responseLanguagePolicy": responseLanguage.Policy,
		"responseLanguage":       responseLanguage.Language,
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey                 *string `json:"apiKey,omitempty"`
		RequireApiKey          *bool   `json:"requireApiKey,omitempty"`
		Password               string  `json:"password,omitempty"`
		AllowOverUsage         *bool   `json:"allowOverUsage,omitempty"`
		AgentToolSteering      *bool   `json:"agentToolSteering,omitempty"`
		ResponseLanguagePolicy *string `json:"responseLanguagePolicy,omitempty"`
		ResponseLanguage       *string `json:"responseLanguage,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	var responseLanguageUpdate *config.ResponseLanguageConfig
	if req.ResponseLanguagePolicy != nil || req.ResponseLanguage != nil {
		current := config.GetResponseLanguageConfig()
		policy := current.Policy
		language := current.Language
		if req.ResponseLanguagePolicy != nil {
			policy = *req.ResponseLanguagePolicy
		}
		if req.ResponseLanguage != nil {
			language = *req.ResponseLanguage
		}
		normalized, err := config.NormalizeResponseLanguageConfig(policy, language)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		responseLanguageUpdate = &normalized
	}

	if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}

	if req.AgentToolSteering != nil {
		if err := config.UpdateAgentToolSteering(*req.AgentToolSteering); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if responseLanguageUpdate != nil {
		if err := config.UpdateResponseLanguageConfig(responseLanguageUpdate.Policy, responseLanguageUpdate.Language); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": h.getRequestLogs(),
	})
}

func (h *Handler) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	h.requestLogsMu.Lock()
	h.requestLogs = h.requestLogs[:0]
	h.requestLogsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts have no Kiro credential to exercise; test them by sending a
	// real minimal chat request THROUGH the linked upstream pool and returning its reply.
	if account.IsBedrock() {
		var tReq struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&tReq)
		// An explicit test must reflect LIVE state, not a cached verdict: drop the
		// learned region routes so the test re-sweeps every candidate region. Bedrock
		// per-region access can flap, so a stale "not callable anywhere" negative-cache
		// entry would otherwise mask a region whose access just reopened.
		clearBedrockRegionRoutesForLifecycle(account)
		bedrockThrottle.clearForLifecycle(account)
		reply, err := h.bedrockTestReply(account, tReq.Model)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reply": reply})
		return
	}
	if claudeAccountIsDirectAnthropic(account) {
		var tReq struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&tReq)
		testModel := strings.TrimSpace(tReq.Model)
		if testModel == "" {
			testModel = directAnthropicDefaultTestModel
		}
		reply, err := directAnthropicTestReply(r.Context(), account.BaseURL, account.KiroApiKey, testModel)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			var responseErr *directAnthropicResponseError
			if errors.As(err, &responseErr) {
				h.handleDirectAnthropicFailure(account, testModel, responseErr)
			} else {
				h.setProviderCooldownForAccount(account, directAnthropicTransientCooldown)
			}
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reply": reply})
		return
	}
	if account.IsCustomApi() {
		var tReq struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&tReq)
		reply, err := customApiTestReply(
			r.Context(),
			account.BaseURL,
			account.KiroApiKey,
			tReq.Model,
		)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reply": reply})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPIContext(r.Context(), account, kiroPayload, callback)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	outcome, err := h.refreshAccountByKind(r.Context(), account, accountRefreshExplicit)
	if err != nil {
		status := http.StatusInternalServerError
		if account.IsCustomApi() || account.IsDirectAnthropic() {
			status = http.StatusBadGateway
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	response := map[string]interface{}{"success": true}
	if outcome != nil {
		if outcome.Info != nil {
			response["info"] = outcome.Info
		}
		if outcome.Message != "" {
			response["message"] = outcome.Message
		}
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(response)

}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"region":            account.Region,
		"profileArn":        account.ProfileArn,
		"tokenEndpoint":     account.TokenEndpoint,
		"issuerUrl":         account.IssuerURL,
		"scopes":            account.Scopes,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts serve whatever the linked pool serves: load the model list
	// from the upstream provider's /v1/models, not from Kiro/AWS.
	if account.IsBedrock() {
		// Prefer live-discovered callable models. Explicit account mappings are
		// operator evidence; built-in defaults are resolver conveniences only and
		// must not manufacture availability in the admin/public model view.
		ids := h.cachedOrDiscoverBedrockModels(account)
		for _, value := range account.BedrockModelMap {
			ids = mergeStringLists(ids, []string{value})
		}
		// The panel expects {success, models:[{modelId}]} (same shape as custom_api),
		// not a bare string array. Annotate each model with its learned callable
		// region (from lazy routing / prewarm) when known, so the UI can show where a
		// model actually works. "region": "" means not yet probed.
		models := make([]map[string]interface{}, 0, len(ids))
		for _, mid := range ids {
			region := ""
			if r, ok := getBedrockRouteForAccount(account, mid); ok && r.callable {
				region = r.region
			}
			models = append(models, map[string]interface{}{"modelId": mid, "region": region})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "models": models, "candidateRegions": candidateRegions(account)})
		return
	}
	if account.IsCustomApi() || account.IsDirectAnthropic() {
		var models []ModelInfo
		var err error
		if account.IsDirectAnthropic() {
			models, err = probeDirectAnthropicModels(r.Context(), account.BaseURL, account.KiroApiKey)
		} else {
			models, err = probeCustomApiModelsWithContext(r.Context(), account.BaseURL, account.KiroApiKey)
		}
		if err != nil {
			if account.IsDirectAnthropic() && r.Context().Err() == nil {
				h.handleDirectAnthropicDiscoveryFailure(account, err)
			}
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "models": models})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置 (single proxy + rotation pool + active proxy)
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	active := config.GetProxyURL()
	if h.proxyRotator != nil {
		active = h.proxyRotator.activeURL()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"proxyURL":           config.GetProxyURL(),
		"proxyURLs":          config.GetProxyURLs(),
		"proxyRotateMinutes": config.GetProxyRotateMinutes(),
		"activeProxyURL":     active,
	})
}

// isValidProxyScheme reports whether a proxy URL starts with a supported scheme.
func isValidProxyScheme(u string) bool {
	return strings.HasPrefix(u, "http://") ||
		strings.HasPrefix(u, "https://") ||
		strings.HasPrefix(u, "socks5://") ||
		strings.HasPrefix(u, "socks5h://")
}

// apiUpdateProxy 更新代理配置并立即生效. Accepts a single proxyURL plus an optional
// rotation pool (proxyURLs) and interval (proxyRotateMinutes). When the pool is
// non-empty the rotator cycles it; otherwise the single proxyURL is applied.
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL           string   `json:"proxyURL"`
		ProxyURLs          []string `json:"proxyURLs"`
		ProxyRotateMinutes int      `json:"proxyRotateMinutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" && !isValidProxyScheme(req.ProxyURL) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
		return
	}
	// Validate and clean every pool entry.
	cleaned := make([]string, 0, len(req.ProxyURLs))
	for _, u := range req.ProxyURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !isValidProxyScheme(u) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "each proxy must start with http://, https://, socks5://, or socks5h://: " + u})
			return
		}
		cleaned = append(cleaned, u)
	}
	if req.ProxyRotateMinutes < 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "proxyRotateMinutes must be >= 0"})
		return
	}

	if err := config.UpdateProxySettings(req.ProxyURL, cleaned, req.ProxyRotateMinutes); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置 (also (re)starts or stops rotation).
	if h.proxyRotator != nil {
		h.proxyRotator.configure(req.ProxyURL, cleaned, config.GetProxyRotateMinutes())
	} else {
		applyProxyConfig(req.ProxyURL)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken   string `json:"accessToken"`
		CsrfToken     string `json:"csrfToken"`
		RefreshToken  string `json:"refreshToken"`
		ClientID      string `json:"clientId,omitempty"`
		ClientSecret  string `json:"clientSecret,omitempty"`
		KiroApiKey    string `json:"kiroApiKey,omitempty"` // Present for api_key accounts so backup/restore round-trips
		Region        string `json:"region,omitempty"`
		ExpiresAt     int64  `json:"expiresAt"`
		AuthMethod    string `json:"authMethod,omitempty"`
		Provider      string `json:"provider,omitempty"`
		TokenEndpoint string `json:"tokenEndpoint,omitempty"`
		IssuerURL     string `json:"issuerUrl,omitempty"`
		Scopes        string `json:"scopes,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		ProfileArn   string             `json:"profileArn,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}
		// api_key accounts have no OAuth material; keep the lowercase token so the
		// importer round-trips it back into the same normalization path.
		if a.IsApiKeyCredential() {
			authMethod = "api_key"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:         a.ID,
			Email:      a.Email,
			Nickname:   a.Nickname,
			Idp:        idp,
			UserId:     a.UserId,
			ProfileArn: a.ProfileArn,
			MachineId:  a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:   a.AccessToken,
				CsrfToken:     "",
				RefreshToken:  a.RefreshToken,
				ClientID:      a.ClientID,
				ClientSecret:  a.ClientSecret,
				KiroApiKey:    a.KiroApiKey,
				Region:        a.Region,
				ExpiresAt:     a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:    authMethod,
				Provider:      a.Provider,
				TokenEndpoint: a.TokenEndpoint,
				IssuerURL:     a.IssuerURL,
				Scopes:        a.Scopes,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
