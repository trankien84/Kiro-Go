// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

const (
	circuitClosed         = 0
	circuitOpen           = 1
	circuitHalfOpen       = 2
	circuitErrorThreshold = 5
	circuitOpenDuration   = 30 * time.Second
	sessionAffinityTTL    = 10 * time.Minute

	// healthEWMAAlpha is the smoothing factor for the per-account EWMA health
	// signals (error rate and latency). Higher = react faster to recent events.
	healthEWMAAlpha = 0.3

	// maxAffinityEntries bounds the session-affinity map; when exceeded, expired
	// bindings are swept before inserting a new one.
	maxAffinityEntries = 1024
)

// circuitBreaker is a per-account 3-state breaker. Its own mutex guards the
// state transitions so it can be evaluated from the selection path (which only
// holds the pool's RLock) and still persist open->half-open correctly, instead
// of silently un-blocking every request once the open window elapses.
type circuitBreaker struct {
	mu             sync.Mutex
	state          int
	consecutiveErr int
	openedAt       time.Time
}

// isOpen reports whether the breaker is currently blocking requests. After the
// open window elapses it persists the open->half-open transition and returns
// false to let a single probe through.
func (cb *circuitBreaker) isOpen(now time.Time) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case circuitOpen:
		if now.Sub(cb.openedAt) >= circuitOpenDuration {
			cb.state = circuitHalfOpen // persist transition; allow one probe
			return false
		}
		return true
	case circuitHalfOpen:
		return false
	default:
		return false
	}
}

// recordError advances the breaker on a failed request.
func (cb *circuitBreaker) recordError(now time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveErr++
	if cb.state == circuitHalfOpen {
		cb.state = circuitOpen // probe failed → re-open
		cb.openedAt = now
	} else if cb.consecutiveErr >= circuitErrorThreshold && cb.state == circuitClosed {
		cb.state = circuitOpen
		cb.openedAt = now
	}
}

// reset closes the breaker after a successful request.
func (cb *circuitBreaker) reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = circuitClosed
	cb.consecutiveErr = 0
}

// accountHealth tracks per-account health signals used by score-weighted
// selection: EWMA latency and a decaying EWMA error rate.
type accountHealth struct {
	ewmaLatencyMs float64 // EWMA latency (α=healthEWMAAlpha)
	ewmaErrorRate float64 // EWMA error rate in [0,1]; 1 per error, 0 per success
	samples       int     // number of recorded observations
}

// apiKeyBinding binds an API key to a preferred account for session affinity.
type apiKeyBinding struct {
	accountID           string
	lifecycleGeneration uint64
	lastUsed            time.Time
}

// isCircuitOpen reports whether an account's circuit breaker is currently open
// (and should be skipped). It transitions open→half-open after the open
// duration elapses, allowing a single probe through. Safe to call without
// holding p.mu — it takes its own RLock for the map read, and the breaker's own
// mutex guards the state transition.
func (p *AccountPool) isCircuitOpen(id string, now time.Time) bool {
	p.mu.RLock()
	cb := p.circuitState[id]
	p.mu.RUnlock()
	return cb != nil && cb.isOpen(now)
}

// accountStatsKey separates account lifecycles. Reusing an account ID after a
// reset/delete/recreate must not inherit cumulative state from the old account.
type accountStatsKey struct {
	id    string
	epoch uint64
}

// accountStatsSnapshot is cumulative within one account lifecycle. version is
// assigned while p.mu is held, so persistence admission can reject goroutines
// that arrive out of order without comparing counter values.
type accountStatsSnapshot struct {
	seq          uint64
	key          accountStatsKey
	version      uint64
	skipPersist  bool
	requestCount int
	errorCount   int
	totalTokens  int
	totalCredits float64
	lastUsed     int64
}

const statsPersistMaxRetries = 6

func (snapshot accountStatsSnapshot) accountID() string { return snapshot.key.id }

func (p *AccountPool) initStatsPersistenceLocked() {
	if p.statsPersistCond == nil {
		p.statsPersistCond = sync.NewCond(&p.statsPersistMu)
	}
	if p.statsPersistPending == nil {
		p.statsPersistPending = make(map[accountStatsKey]accountStatsSnapshot)
	}
	if p.statsPersistLatest == nil {
		p.statsPersistLatest = make(map[accountStatsKey]accountStatsSnapshot)
	}
	if p.statsPersistCommittedVersion == nil {
		p.statsPersistCommittedVersion = make(map[accountStatsKey]uint64)
	}
	if p.statsPersistFn == nil && p.statsPersistBatchFn == nil {
		p.statsPersistBatchFn = persistAccountStatsBatch
	}
}

func (p *AccountPool) ensureStatsPersistenceWorkerLocked() {
	p.initStatsPersistenceLocked()
	if p.statsPersistRunning || p.statsPersistClosing {
		return
	}
	p.statsPersistRunning = true
	p.statsPersistDone = make(chan struct{})
	go p.runStatsPersistenceWorker()
}

func (p *AccountPool) enqueueStatsPersistence(snapshot accountStatsSnapshot) bool {
	if p == nil || snapshot.accountID() == "" || snapshot.key.epoch == 0 || snapshot.version == 0 {
		return false
	}
	p.statsPersistMu.Lock()
	p.initStatsPersistenceLocked()
	if p.statsPersistClosing {
		p.statsPersistMu.Unlock()
		return false
	}
	p.ensureStatsPersistenceWorkerLocked()
	p.statsPersistSeq++
	snapshot.seq = p.statsPersistSeq

	latestVersion := p.statsPersistCommittedVersion[snapshot.key]
	if latest, ok := p.statsPersistLatest[snapshot.key]; ok && latest.version > latestVersion {
		latestVersion = latest.version
	}
	if snapshot.version > latestVersion {
		// New cumulative state re-opens a batch that previously exhausted retries.
		p.statsPersistErr = nil
		p.statsPersistRetryCount = 0
	}
	if committed := p.statsPersistCommittedVersion[snapshot.key]; committed >= snapshot.version {
		// A newer version committed before this goroutine reached admission. Keep a
		// no-op sequence marker so FlushStats still observes a contiguous barrier.
		snapshot.skipPersist = true
	}
	if latest, ok := p.statsPersistLatest[snapshot.key]; ok && latest.version > snapshot.version {
		latest.seq = snapshot.seq
		latest.skipPersist = latest.version <= p.statsPersistCommittedVersion[snapshot.key]
		snapshot = latest
	}
	p.statsPersistLatest[snapshot.key] = snapshot
	if pending, ok := p.statsPersistPending[snapshot.key]; !ok || snapshot.version >= pending.version {
		p.statsPersistPending[snapshot.key] = snapshot
	} else {
		pending.seq = snapshot.seq
		p.statsPersistPending[snapshot.key] = pending
	}
	p.statsPersistCond.Signal()
	p.statsPersistMu.Unlock()
	return true
}

func statsPersistBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > statsPersistMaxRetries {
		attempt = statsPersistMaxRetries
	}
	base := time.Duration(1<<(attempt-1)) * 25 * time.Millisecond
	// Add bounded jitter so several processes recovering from the same storage
	// outage do not retry in lockstep. math/rand's package functions are safe for
	// concurrent use and exact timing is not part of the persistence contract.
	span := base / 5
	if span <= 0 {
		return base
	}
	return base - span/2 + time.Duration(rand.Int63n(int64(span)+1))
}

func maxUint64(left, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}

func (p *AccountPool) requeueStatsBatchLocked(batch []accountStatsSnapshot) {
	for _, snapshot := range batch {
		if pending, ok := p.statsPersistPending[snapshot.key]; ok && pending.version >= snapshot.version {
			continue
		}
		if latest, ok := p.statsPersistLatest[snapshot.key]; ok && latest.version >= snapshot.version {
			latest.seq = maxUint64(latest.seq, snapshot.seq)
			latest.skipPersist = latest.version <= p.statsPersistCommittedVersion[snapshot.key]
			p.statsPersistPending[snapshot.key] = latest
			continue
		}
		p.statsPersistPending[snapshot.key] = snapshot
	}
}

func (p *AccountPool) runStatsPersistenceWorker() {
	defer func() {
		p.statsPersistMu.Lock()
		p.statsPersistRunning = false
		if p.statsPersistDone != nil {
			close(p.statsPersistDone)
			p.statsPersistDone = nil
		}
		p.statsPersistCond.Broadcast()
		p.statsPersistMu.Unlock()
	}()

	for {
		p.statsPersistMu.Lock()
		for len(p.statsPersistPending) == 0 && !p.statsPersistClosing {
			p.statsPersistCond.Wait()
		}
		if len(p.statsPersistPending) == 0 && p.statsPersistClosing {
			p.statsPersistMu.Unlock()
			return
		}
		if p.statsPersistErr != nil && p.statsPersistRetryCount >= statsPersistMaxRetries {
			if p.statsPersistClosing {
				p.statsPersistMu.Unlock()
				return
			}
			p.statsPersistCond.Wait()
			p.statsPersistMu.Unlock()
			continue
		}

		batch := make([]accountStatsSnapshot, 0, len(p.statsPersistPending))
		var batchMaxSeq uint64
		for key, snapshot := range p.statsPersistPending {
			batch = append(batch, snapshot)
			batchMaxSeq = maxUint64(batchMaxSeq, snapshot.seq)
			delete(p.statsPersistPending, key)
		}
		// An account ID can be recreated with a new epoch while an old snapshot is
		// still pending. Persist old lifecycles first so the newest epoch is always
		// the final durable write for that account.
		sort.Slice(batch, func(i, j int) bool {
			if batch[i].key.id != batch[j].key.id {
				return batch[i].key.id < batch[j].key.id
			}
			if batch[i].key.epoch != batch[j].key.epoch {
				return batch[i].key.epoch < batch[j].key.epoch
			}
			return batch[i].version < batch[j].version
		})
		persistOne := p.statsPersistFn
		persistBatch := p.statsPersistBatchFn
		p.statsPersistAttemptedSeq = maxUint64(p.statsPersistAttemptedSeq, batchMaxSeq)
		p.statsPersistMu.Unlock()

		var batchErr error
		if persistOne != nil {
			for _, snapshot := range batch {
				if snapshot.skipPersist {
					continue
				}
				if err := persistOne(snapshot); err != nil {
					batchErr = errors.Join(batchErr, fmt.Errorf(
						"account=%s epoch=%d version=%d: %w",
						snapshot.accountID(), snapshot.key.epoch, snapshot.version, err,
					))
				}
			}
		} else if persistBatch != nil {
			batchErr = persistBatch(batch)
		}

		p.statsPersistMu.Lock()
		if batchErr == nil {
			for _, snapshot := range batch {
				if snapshot.version > p.statsPersistCommittedVersion[snapshot.key] {
					p.statsPersistCommittedVersion[snapshot.key] = snapshot.version
				}
				if latest, ok := p.statsPersistLatest[snapshot.key]; ok &&
					latest.version <= snapshot.version && latest.seq <= snapshot.seq {
					delete(p.statsPersistLatest, snapshot.key)
				}
			}
			p.statsPersistCommittedSeq = maxUint64(p.statsPersistCommittedSeq, batchMaxSeq)
			p.statsPersistErr = nil
			p.statsPersistRetryCount = 0
			p.statsPersistCond.Broadcast()
			p.statsPersistMu.Unlock()
			continue
		}

		p.requeueStatsBatchLocked(batch)
		p.statsPersistErr = batchErr
		p.statsPersistRetryCount++
		attempt := p.statsPersistRetryCount
		closing := p.statsPersistClosing
		p.statsPersistCond.Broadcast()
		p.statsPersistMu.Unlock()
		logger.Warnf("[AccountPool] failed to persist account stats attempt=%d/%d: %v", attempt, statsPersistMaxRetries, batchErr)
		if attempt >= statsPersistMaxRetries && closing {
			return
		}
		time.Sleep(statsPersistBackoff(attempt))
	}
}

func (p *AccountPool) pruneStatsPersistenceEpochs(active map[string]uint64) {
	p.statsPersistMu.Lock()
	defer p.statsPersistMu.Unlock()
	p.initStatsPersistenceLocked()
	for key := range p.statsPersistLatest {
		if active[key.id] != key.epoch {
			delete(p.statsPersistLatest, key)
		}
	}
	for key := range p.statsPersistCommittedVersion {
		if active[key.id] != key.epoch {
			delete(p.statsPersistCommittedVersion, key)
		}
	}
	// Pending snapshots are intentionally retained: they were accepted by the old
	// lifecycle and FlushStats must either commit them or report their error.
}

// AccountPool 账号池
type AccountPool struct {
	mu                           sync.RWMutex
	accounts                     []config.Account
	totalAccounts                int
	lastDispatchSeq              map[string]uint64          // accountID → last dispatch sequence (lower = used longer ago; LRU clock)
	dispatchSeq                  uint64                     // monotonically increases per dispatch (stable LRU ordering)
	cooldowns                    map[string]time.Time       // 账号冷却时间
	errorCounts                  map[string]int             // 连续错误计数
	modelLists                   map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	modelListUnavailable         map[string]bool            // accountID → expired/unsafe provider snapshot
	reprobeBackoff               map[accountRecoveryKey]time.Duration
	reprobeNext                  map[accountRecoveryKey]time.Time
	recoveryPending              map[accountRecoveryKey]pendingAccountRecoveryPublication
	stopRecover                  chan struct{}
	circuitState                 map[string]*circuitBreaker // accountID → circuit breaker state
	healthStats                  map[string]*accountHealth  // accountID → EWMA latency + error/success counts
	apiKeyAffinity               map[string]apiKeyBinding   // apiKeyID → preferred account (sticky routing)
	lifecycleGenerationByID      map[string]uint64
	statsEpochByID               map[string]uint64
	statsVersionByID             map[string]uint64
	statsCurrentByID             map[string]accountStatsSnapshot
	statsLedgerByID              map[string]accountStatsSnapshot
	statsPersistMu               sync.Mutex
	statsPersistCond             *sync.Cond
	statsPersistPending          map[accountStatsKey]accountStatsSnapshot
	statsPersistLatest           map[accountStatsKey]accountStatsSnapshot
	statsPersistCommittedVersion map[accountStatsKey]uint64
	statsPersistSeq              uint64
	statsPersistAttemptedSeq     uint64
	statsPersistCommittedSeq     uint64
	statsPersistRetryCount       int
	statsPersistErr              error
	statsPersistRunning          bool
	statsPersistClosing          bool
	statsPersistDone             chan struct{}
	statsPersistFn               func(accountStatsSnapshot) error
	statsPersistBatchFn          func([]accountStatsSnapshot) error
	recoverCloseOnce             sync.Once
	closeOnce                    sync.Once
	closeDone                    chan struct{}
	closeErr                     error
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:            make(map[string]time.Time),
			lastDispatchSeq:      make(map[string]uint64),
			errorCounts:          make(map[string]int),
			modelLists:           make(map[string]map[string]bool),
			modelListUnavailable: make(map[string]bool),
			reprobeBackoff:       make(map[accountRecoveryKey]time.Duration),
			reprobeNext:          make(map[accountRecoveryKey]time.Time),
			circuitState:         make(map[string]*circuitBreaker),
			healthStats:          make(map[string]*accountHealth),
			apiKeyAffinity:       make(map[string]apiKeyBinding),
		}
		pool.Reload()
		if config.GetAutoRecoverEnabled() {
			pool.startAutoRecover()
		}
	})
	return pool
}

// Reload rebuilds the account list from config (one entry per account; weight is
// handled as selection probability by healthScore, not as duplicated slots).
// Over-quota accounts are dropped unless either the per-account upstream
// Overages switch (OverageStatus=ENABLED) or the global AllowOverUsage
// setting permits over-quota routing.
func (p *AccountPool) Reload() {
	// Capture accounts and config-owned stats generations under one cfgLock before
	// taking p.mu; the pool must not combine account data from different snapshots.
	inventory := config.GetAccountsWithStatsGeneration()
	allowOverUsage := config.GetAllowOverUsage()

	p.mu.Lock()
	p.syncLifecycleGenerationsLocked(inventory)
	activeEpochs := p.syncStatsEpochsLocked(inventory)
	var accounts []config.Account
	totalEnabled := 0
	for _, item := range inventory {
		a := p.mergeCurrentStatsLocked(item)
		if !a.Enabled {
			continue
		}
		totalEnabled++
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		accounts = append(accounts, a) // one entry per account (weight handled by score)
	}
	p.accounts = accounts
	p.totalAccounts = totalEnabled
	p.mu.Unlock()

	p.pruneStatsPersistenceEpochs(activeEpochs)
}

// GetNext 获取下一个可用账号（加权轮询）
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（LRU 轮询，最少最近使用），并跳过指定账号。
// Delegates to GetNextForModelExcluding with model="" (any model).
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	return p.GetNextForModelExcluding("", excluded)
}

// SetModelList publishes the latest known model set for an account. An empty
// list remains optimistic because several compatible relays return an empty
// discovery list while still accepting explicitly requested models. Expired or
// otherwise unsafe snapshots use SetModelListUnavailable instead.
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		if key := strings.ToLower(strings.TrimSpace(id)); key != "" {
			set[key] = true
		}
	}
	p.mu.Lock()
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.modelListUnavailable == nil {
		p.modelListUnavailable = make(map[string]bool)
	}
	p.modelLists[accountID] = set
	delete(p.modelListUnavailable, accountID)
	p.mu.Unlock()
}

func (p *AccountPool) SetModelListForLifecycle(accountID string, lifecycleGeneration uint64, modelIDs []string) bool {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		if key := strings.ToLower(strings.TrimSpace(id)); key != "" {
			set[key] = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if accountID == "" || lifecycleGeneration == 0 || p.lifecycleGenerationByID[accountID] != lifecycleGeneration {
		return false
	}
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.modelListUnavailable == nil {
		p.modelListUnavailable = make(map[string]bool)
	}
	p.modelLists[accountID] = set
	delete(p.modelListUnavailable, accountID)
	return true
}

// SetModelListUnavailable marks an account fail-closed for model routing until a
// new source snapshot is published. This is distinct from a successful empty
// discovery result.
func (p *AccountPool) SetModelListUnavailable(accountID string) {
	p.mu.Lock()
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.modelListUnavailable == nil {
		p.modelListUnavailable = make(map[string]bool)
	}
	p.modelLists[accountID] = map[string]bool{}
	p.modelListUnavailable[accountID] = true
	p.mu.Unlock()
}

func (p *AccountPool) SetModelListUnavailableForLifecycle(accountID string, lifecycleGeneration uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if accountID == "" || lifecycleGeneration == 0 || p.lifecycleGenerationByID[accountID] != lifecycleGeneration {
		return false
	}
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.modelListUnavailable == nil {
		p.modelListUnavailable = make(map[string]bool)
	}
	p.modelLists[accountID] = map[string]bool{}
	p.modelListUnavailable[accountID] = true
	return true
}

// ClearModelList returns an account to the unknown/cold-start state. It is used
// when the account source itself changes.
func (p *AccountPool) ClearModelList(accountID string) {
	p.mu.Lock()
	delete(p.modelLists, accountID)
	delete(p.modelListUnavailable, accountID)
	p.mu.Unlock()
}

// ClearModelListForLifecycle clears request-derived provider evidence only when
// the pool still exposes the lifecycle that owns it. A stale request from N must
// not erase model state already published for a recreated N+1 account.
func (p *AccountPool) ClearModelListForLifecycle(accountID string, lifecycleGeneration uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if accountID == "" || lifecycleGeneration == 0 || p.lifecycleGenerationByID[accountID] != lifecycleGeneration {
		return false
	}
	delete(p.modelLists, accountID)
	delete(p.modelListUnavailable, accountID)
	return true
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	if p.modelListUnavailable[accountID] {
		return false
	}
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // cold start or compatible provider with non-enumerable models
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextSelectionForModelExcluding selects an eligible account using the
// configured global strategy. The default round-robin strategy preserves the
// existing LRU dispatch sequence; priority narrows to the best eligible hard
// priority tier before that LRU step; fill-first narrows to the first account in
// the best eligible tier. Existing model, exclusion, cooldown, circuit,
// credential, and quota gates always run before strategy selection. model=""
// means "any model".
func (p *AccountPool) GetNextSelectionForModelExcluding(model string, excluded map[string]bool) *AccountSelection {
	// Read config (takes cfgLock) BEFORE acquiring p.mu so the pool lock never
	// nests cfgLock. Holding p.mu while blocking on cfgLock would freeze every
	// other pool operation behind the write lock if a config writer is mid-Save
	// (synchronous config-file publication). Config reads stay outside p.mu;
	// credential refresh lifecycle locks never nest the pool lock.
	allowOverUsage := config.GetAllowOverUsage()
	strategy := config.GetAccountSelectionStrategy()

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.accounts) == 0 {
		return nil
	}

	now := time.Now()

	// Build candidate list: each eligible account paired with its last
	// dispatch sequence (LRU key) and health score (tie-breaker).
	var candidates []accountSelectionCandidate
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if cb := p.circuitState[acc.ID]; cb != nil && cb.isOpen(now) {
			continue
		}
		if !accountCredentialDispatchable(acc, now) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		candidates = append(candidates, accountSelectionCandidate{
			acc:     acc,
			lastSeq: p.lastDispatchSeq[acc.ID],
			score:   p.healthScore(acc.ID, effectiveWeight(acc.Weight)),
		})
	}

	if len(candidates) == 0 {
		// Fallback: return the account with the earliest cooldown. Stamp the
		// LRU clock so a later healthy pick doesn't immediately re-select this
		// account (every other dispatch path stamps; the fallback must too, or
		// a degraded account's stale seq wins the next pick and gets a burst).
		acc := p.fallbackEarliestCooldown(model, excluded, allowOverUsage)
		if acc != nil {
			p.dispatchSeq++
			if p.lastDispatchSeq == nil {
				p.lastDispatchSeq = make(map[string]uint64)
			}
			p.lastDispatchSeq[acc.ID] = p.dispatchSeq
		}
		return p.selectionForAccountLocked(acc)
	}

	// Strategy is deliberately applied only after all normal eligibility gates.
	// The degraded earliest-cooldown fallback above remains unchanged.
	candidates = applyAccountSelectionStrategy(candidates, strategy)

	// Least-recently-used: pick the candidate dispatched longest ago (lowest
	// sequence; never-dispatched accounts share the zero value). Ties are broken
	// by health score (higher preferred), then at random so a cold pool spreads
	// its first picks instead of always hitting the first account. A monotonic
	// sequence counter (not wall-clock) keys the ordering, so two accounts can
	// never share a key — LRU stays exact round-robin regardless of clock
	// resolution.
	oldest := candidates[0].lastSeq
	for _, c := range candidates[1:] {
		if c.lastSeq < oldest {
			oldest = c.lastSeq
		}
	}
	var top []accountSelectionCandidate
	topScore := -1.0
	for _, c := range candidates {
		if c.lastSeq != oldest {
			continue
		}
		if len(top) == 0 || c.score > topScore {
			top = []accountSelectionCandidate{c}
			topScore = c.score
		} else if c.score == topScore {
			top = append(top, c)
		}
	}
	chosen := top[rand.Intn(len(top))].acc

	// Advance the monotonic LRU clock and stamp the chosen account so the next
	// pick goes to a different one.
	p.dispatchSeq++
	if p.lastDispatchSeq == nil {
		p.lastDispatchSeq = make(map[string]uint64)
	}
	p.lastDispatchSeq[chosen.ID] = p.dispatchSeq
	return p.selectionForAccountLocked(chosen)
}

// GetNextForModelExcluding is the legacy account-only selector. It now returns a
// deep copy rather than a pointer into p.accounts. New request dispatch code
// should use GetNextSelectionForModelExcluding so lifecycle fencing is retained.
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	selection := p.GetNextSelectionForModelExcluding(model, excluded)
	if selection == nil {
		return nil
	}
	account := selection.Account.Clone()
	return &account
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			account := p.accounts[i].Clone()
			return &account
		}
	}
	return nil
}

// GetNextForModelWithApiKey selects an account for a request, preferring the one
// already bound to the API key (session affinity). Falls back to health-aware
// scoring if the bound account is unavailable or affinity is disabled.
func (p *AccountPool) GetNextSelectionForModelWithApiKey(model string, excluded map[string]bool, apiKey string) *AccountSelection {
	allowOverUsage := config.GetAllowOverUsage()
	affinityEnabled := apiKey != "" && config.GetSessionAffinityEnabled()

	if affinityEnabled {
		now := time.Now()
		p.mu.Lock()
		if binding, ok := p.apiKeyAffinity[apiKey]; ok && now.Sub(binding.lastUsed) < sessionAffinityTTL &&
			p.lifecycleGenerationByID[binding.accountID] == binding.lifecycleGeneration {
			for i := range p.accounts {
				acc := &p.accounts[i]
				if acc.ID != binding.accountID {
					continue
				}
				isExcluded := excluded != nil && excluded[acc.ID]
				cooldown, hasCooldown := p.cooldowns[acc.ID]
				hasModel := model == "" || p.accountHasModel(acc.ID, model)
				credentialDispatchable := accountCredentialDispatchable(acc, now)
				quotaBlocked := isQuotaBlocked(*acc, allowOverUsage)
				cb := p.circuitState[acc.ID]
				circuitOpen := cb != nil && cb.isOpen(now)
				if !isExcluded && !(hasCooldown && now.Before(cooldown)) && !circuitOpen && hasModel && credentialDispatchable && !quotaBlocked {
					binding.lastUsed = now
					p.dispatchSeq++
					if p.lastDispatchSeq == nil {
						p.lastDispatchSeq = make(map[string]uint64)
					}
					p.lastDispatchSeq[acc.ID] = p.dispatchSeq
					p.apiKeyAffinity[apiKey] = binding
					selection := p.selectionForAccountLocked(acc)
					p.mu.Unlock()
					return selection
				}
				break
			}
		}
		p.mu.Unlock()
	}

	selection := p.GetNextSelectionForModelExcluding(model, excluded)
	if selection != nil && affinityEnabled {
		p.mu.Lock()
		if len(p.apiKeyAffinity) >= maxAffinityEntries {
			p.pruneExpiredAffinityLocked(time.Now())
		}
		p.apiKeyAffinity[apiKey] = apiKeyBinding{
			accountID:           selection.Account.ID,
			lifecycleGeneration: selection.LifecycleGeneration,
			lastUsed:            time.Now(),
		}
		p.mu.Unlock()
	}
	return selection
}

func (p *AccountPool) GetNextForModelWithApiKey(model string, excluded map[string]bool, apiKey string) *config.Account {
	selection := p.GetNextSelectionForModelWithApiKey(model, excluded, apiKey)
	if selection == nil {
		return nil
	}
	account := selection.Account.Clone()
	return &account
}

// pruneExpiredAffinityLocked removes session-affinity bindings whose TTL has
// elapsed, keeping the map from growing unbounded with one entry per distinct
// API key ever seen. Caller must hold p.mu (write lock).
func (p *AccountPool) pruneExpiredAffinityLocked(now time.Time) {
	for key, binding := range p.apiKeyAffinity {
		if now.Sub(binding.lastUsed) >= sessionAffinityTTL {
			delete(p.apiKeyAffinity, key)
		}
	}
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordSuccessLocked(id)
}

// cooldownClearThreshold is the remaining duration below which RecordSuccess
// will clear a cooldown. Transient (3-error) cooldowns are +1min; quota/overage
// backoffs are +1h. A late in-flight success must not wipe a hard backoff —
// only short transient ones — or the exhausted upstream gets re-selected.
const cooldownClearThreshold = 10 * time.Minute

// setCooldownIfLater sets the cooldown to newExpiry only if it extends the
// existing one (or none exists), so a transient short backoff (the 3-error
// +1min cooldown) can never shorten a longer quota/overage backoff (+1h) for
// the same account. Caller must hold p.mu.
func setCooldownIfLater(cooldowns map[string]time.Time, id string, newExpiry time.Time) {
	if ex, ok := cooldowns[id]; !ok || newExpiry.After(ex) {
		cooldowns[id] = newExpiry
	}
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordErrorLocked(id, isQuotaError)
}

// recordHealthObservation updates the account's EWMA error rate. isError=true
// pushes the rate toward 1, false decays it toward 0. Caller must hold p.mu.
func (p *AccountPool) recordHealthObservation(id string, isError bool) {
	if p.healthStats == nil {
		p.healthStats = make(map[string]*accountHealth)
	}
	h := p.healthStats[id]
	if h == nil {
		h = &accountHealth{}
		p.healthStats[id] = h
	}
	h.samples++
	sample := 0.0
	if isError {
		sample = 1.0
	}
	h.ewmaErrorRate = healthEWMAAlpha*sample + (1-healthEWMAAlpha)*h.ewmaErrorRate
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if HasStatusToken(msg, "401") || HasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// HasStatusToken returns true when status appears in s with non-alphanumeric
// boundaries on both sides, so "401" matches "HTTP 401 from ..." but not
// "4011", "14013", or an alphanumeric token like "request_401abc". Exported so
// the proxy package can match upstream status codes by the same boundary rule
// (quota/overage classifiers) instead of bare strings.Contains on the body.
func HasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isAlphaNum(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isAlphaNum(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isAlphaNum(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

type accountRecoveryKey struct {
	id         string
	generation uint64
}

type accountRecoveryPublicationKind uint8

const (
	accountRecoveryPublicationRecover accountRecoveryPublicationKind = iota + 1
	accountRecoveryPublicationBan
)

type pendingAccountRecoveryPublication struct {
	kind     accountRecoveryPublicationKind
	observed config.Account
	status   string
	reason   string
}

func (publication pendingAccountRecoveryPublication) publish() (bool, error) {
	switch publication.kind {
	case accountRecoveryPublicationRecover:
		return config.RecoverDisabledAccountIfCredentialObserved(publication.observed)
	case accountRecoveryPublicationBan:
		return config.SetAccountBanStatusIfCredentialObserved(
			publication.observed,
			publication.status,
			publication.reason,
		)
	default:
		return false, fmt.Errorf("unknown account recovery publication kind %d", publication.kind)
	}
}

func accountRecoveryKeyFor(account config.Account) (accountRecoveryKey, bool) {
	key := accountRecoveryKey{id: account.ID, generation: account.DispatchGeneration}
	if strings.TrimSpace(key.id) == "" || key.generation == 0 {
		return accountRecoveryKey{}, false
	}
	return key, true
}

// accountUsesLegacyAutoRecovery identifies only failure-driven, refreshable
// OAuth DISABLED state. Manual disable has no reason, while static credentials
// have no OAuth refresh capability and must never be sent through token recovery.
func accountUsesLegacyAutoRecovery(account config.Account) bool {
	return !account.Enabled &&
		account.SupportsKiroTokenRefresh() &&
		strings.TrimSpace(account.RefreshToken) != "" &&
		strings.EqualFold(strings.TrimSpace(account.BanStatus), "DISABLED") &&
		strings.TrimSpace(account.BanReason) != ""
}

func nextRecoveryBackoff(previous time.Duration) time.Duration {
	if previous <= 0 {
		return time.Minute
	}
	next := previous * 5
	if next > 2*time.Hour {
		return 2 * time.Hour
	}
	return next
}

func (p *AccountPool) pruneRecoveryState(active map[accountRecoveryKey]struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.reprobeBackoff {
		if _, ok := active[key]; !ok {
			delete(p.reprobeBackoff, key)
		}
	}
	for key := range p.reprobeNext {
		if _, ok := active[key]; !ok {
			delete(p.reprobeNext, key)
		}
	}
	for key := range p.recoveryPending {
		if _, ok := active[key]; !ok {
			delete(p.recoveryPending, key)
		}
	}
}

func (p *AccountPool) clearRecoveryState(key accountRecoveryKey) {
	p.mu.Lock()
	delete(p.reprobeBackoff, key)
	delete(p.reprobeNext, key)
	delete(p.recoveryPending, key)
	p.mu.Unlock()
}

func (p *AccountPool) pendingRecoveryPublication(key accountRecoveryKey) (pendingAccountRecoveryPublication, bool) {
	p.mu.RLock()
	pending, ok := p.recoveryPending[key]
	p.mu.RUnlock()
	return pending, ok
}

func (p *AccountPool) storePendingRecoveryPublication(key accountRecoveryKey, pending pendingAccountRecoveryPublication) {
	p.mu.Lock()
	if p.recoveryPending == nil {
		p.recoveryPending = make(map[accountRecoveryKey]pendingAccountRecoveryPublication)
	}
	p.recoveryPending[key] = pending
	p.mu.Unlock()
}

func (p *AccountPool) recoveryRetryReady(key accountRecoveryKey, now time.Time) bool {
	p.mu.RLock()
	next, ok := p.reprobeNext[key]
	p.mu.RUnlock()
	return !ok || !now.Before(next)
}

func (p *AccountPool) scheduleRecoveryRetry(key accountRecoveryKey, completedAt time.Time) {
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reprobeBackoff == nil {
		p.reprobeBackoff = make(map[accountRecoveryKey]time.Duration)
	}
	if p.reprobeNext == nil {
		p.reprobeNext = make(map[accountRecoveryKey]time.Time)
	}
	backoff := nextRecoveryBackoff(p.reprobeBackoff[key])
	p.reprobeBackoff[key] = backoff
	p.reprobeNext[key] = completedAt.Add(backoff)
}

func legacyRecoveryTerminalReason(disposition auth.RefreshFailureDisposition) string {
	switch disposition {
	case auth.RefreshFailureCredentialTerminal:
		return "Authentication failed - token invalid, expired, or revoked"
	case auth.RefreshFailureConfigurationTerminal:
		return "OAuth refresh rejected - re-authentication or configuration update required"
	default:
		return ""
	}
}

// publishRecoveryPublication is the local commit stage after a network
// observation already exists. A pre-publication storage failure is retained in
// memory and retried locally after backoff; it must never cause another IdP POST.
func (p *AccountPool) publishRecoveryPublication(key accountRecoveryKey, publication pendingAccountRecoveryPublication) {
	applied, err := publication.publish()
	completedAt := time.Now()
	if applied {
		// published=true is authoritative even when only the trailing durability
		// barrier failed. Align runtime from the canonical visible image.
		p.Reload()
		p.clearRecoveryState(key)
		if err != nil {
			logger.Warnf("[AccountPool] auto-recovery local publication durability warning account=%s: %v", key.id, err)
		}
		return
	}
	if err != nil {
		p.storePendingRecoveryPublication(key, publication)
		p.scheduleRecoveryRetry(key, completedAt)
		logger.Warnf("[AccountPool] auto-recovery local publication failed account=%s: %v", key.id, err)
		return
	}

	// Lifecycle, credential, or health was superseded. The observed result no
	// longer belongs to the canonical account, so retire it rather than retrying.
	p.clearRecoveryState(key)
}
// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if strings.TrimSpace(reason) == "" {
		reason = "automatic account disable"
	}
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.UpdateCredentialState(nil, id, accessToken, refreshToken, expiresAt, "")
}

// UpdateCredentialState publishes one persisted refresh result to both the
// pool and an optional caller-owned account while holding the pool lock. The
// target may itself point into the pool.
func (p *AccountPool) UpdateCredentialState(
	target *config.Account,
	id string,
	accessToken string,
	refreshToken string,
	expiresAt int64,
	profileArn string,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
			if profileArn != "" {
				p.accounts[i].ProfileArn = profileArn
			}
		}
	}
	if target != nil {
		target.AccessToken = accessToken
		if refreshToken != "" {
			target.RefreshToken = refreshToken
		}
		target.ExpiresAt = expiresAt
		if profileArn != "" {
			target.ProfileArn = profileArn
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		count++
	}
	return count
}

// UpdateStatsForSelection updates cumulative accounting for the lifecycle that
// actually served a request. statsLedgerByID is the full-inventory cumulative
// base, independent of routable p.accounts, so an in-flight completion is still counted after
// disable/quota filtering but cannot cross a stats-generation reset/recreate.
func (p *AccountPool) UpdateStatsForSelection(selection AccountSelection, tokens int, credits float64) bool {
	id := poolStatsAccountID(selection.Account.ID)
	if id == "" || selection.StatsGeneration == 0 {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.statsEpochByID[id] != selection.StatsGeneration {
		return false
	}
	if p.statsLedgerByID == nil {
		p.statsLedgerByID = make(map[string]accountStatsSnapshot)
	}

	snapshot, ok := p.statsLedgerByID[id]
	if !ok || snapshot.key.epoch != selection.StatsGeneration {
		// Normal production selections are always preceded by Reload(), which seeds
		// the ledger from the full config inventory. Keep a safe fallback for small
		// manually-constructed pools without making routability authoritative.
		if retained, retainedOK := p.statsCurrentByID[id]; retainedOK && retained.key.epoch == selection.StatsGeneration {
			snapshot = retained
		} else {
			snapshot = accountStatsSnapshot{
				key:          accountStatsKey{id: id, epoch: selection.StatsGeneration},
				version:      p.statsVersionByID[id],
				requestCount: selection.Account.RequestCount,
				errorCount:   selection.Account.ErrorCount,
				totalTokens:  selection.Account.TotalTokens,
				totalCredits: selection.Account.TotalCredits,
				lastUsed:     selection.Account.LastUsed,
			}
		}
	}

	snapshot.requestCount++
	snapshot.totalTokens += tokens
	snapshot.totalCredits += credits
	snapshot.lastUsed = time.Now().Unix()
	baseVersion := p.statsVersionByID[id]
	if baseVersion < snapshot.version {
		baseVersion = snapshot.version
	}
	snapshot.version = baseVersion + 1

	// Admission and publication stay under p.mu, matching the existing Reload
	// lock order. A rejected admission must not become the cumulative base for a
	// later completion, otherwise an unpersistable request could be resurrected.
	if !p.enqueueStatsPersistence(snapshot) {
		return false
	}

	p.statsVersionByID[id] = snapshot.version
	p.statsLedgerByID[id] = snapshot
	if p.statsCurrentByID == nil {
		p.statsCurrentByID = make(map[string]accountStatsSnapshot)
	}
	p.statsCurrentByID[id] = snapshot
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			applyStatsSnapshot(&p.accounts[i], snapshot)
		}
	}
	return true
}

// UpdateStats is retained for non-request/legacy callers. It intentionally
// targets the pool's CURRENT account view and therefore has no delete/recreate
// fence. All request-completion paths use UpdateStatsForSelection instead.
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	var updated bool
	var snapshot accountStatsSnapshot
	for i := range p.accounts {
		if p.accounts[i].ID != id {
			continue
		}
		if !updated {
			p.accounts[i].RequestCount++
			p.accounts[i].TotalTokens += tokens
			p.accounts[i].TotalCredits += credits
			p.accounts[i].LastUsed = time.Now().Unix()
			if p.statsEpochByID == nil {
				p.statsEpochByID = make(map[string]uint64)
			}
			if p.statsVersionByID == nil {
				p.statsVersionByID = make(map[string]uint64)
			}
			epoch := p.statsEpochByID[id]
			if epoch == 0 {
				epoch = 1
				p.statsEpochByID[id] = epoch
			}
			p.statsVersionByID[id]++
			snapshot = accountStatsSnapshot{
				key:          accountStatsKey{id: id, epoch: epoch},
				version:      p.statsVersionByID[id],
				requestCount: p.accounts[i].RequestCount,
				errorCount:   p.accounts[i].ErrorCount,
				totalTokens:  p.accounts[i].TotalTokens,
				totalCredits: p.accounts[i].TotalCredits,
				lastUsed:     p.accounts[i].LastUsed,
			}
			updated = true
			continue
		}
		applyStatsSnapshot(&p.accounts[i], snapshot)
	}
	if updated {
		if p.enqueueStatsPersistence(snapshot) {
			if p.statsCurrentByID == nil {
				p.statsCurrentByID = make(map[string]accountStatsSnapshot)
			}
			if p.statsLedgerByID == nil {
				p.statsLedgerByID = make(map[string]accountStatsSnapshot)
			}
			p.statsCurrentByID[id] = snapshot
			p.statsLedgerByID[id] = snapshot
		}
	}
	p.mu.Unlock()
}

// FlushStats waits until every sequence accepted before this call has committed.
// Retry exhaustion is returned to the lifecycle owner instead of being logged as
// if persistence had completed.
func (p *AccountPool) FlushStats(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.statsPersistMu.Lock()
	p.initStatsPersistenceLocked()
	target := p.statsPersistSeq
	p.statsPersistMu.Unlock()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.statsPersistMu.Lock()
		committed := p.statsPersistCommittedSeq
		err := p.statsPersistErr
		retriesExhausted := p.statsPersistRetryCount >= statsPersistMaxRetries
		p.statsPersistMu.Unlock()
		if committed >= target {
			return nil
		}
		if err != nil && retriesExhausted {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

// WaitForPendingStats is retained for compatibility. New lifecycle code should
// use FlushStats with a deadline and handle the returned error.
func (p *AccountPool) WaitForPendingStats() {
	_ = p.FlushStats(context.Background())
}

// Close closes process-global pool admission exactly once and reports a flush
// failure to the application root. A Handler must never call this method. Close
// initiation is idempotent; a caller whose context expires may call it again to
// wait for the same shutdown operation.
func (p *AccountPool) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.closeOnce.Do(func() {
		p.closeDone = make(chan struct{})
		p.recoverCloseOnce.Do(func() {
			if p.stopRecover != nil {
				close(p.stopRecover)
			}
		})

		p.statsPersistMu.Lock()
		p.initStatsPersistenceLocked()
		p.statsPersistClosing = true
		p.statsPersistCond.Broadcast()
		workerDone := p.statsPersistDone
		p.statsPersistMu.Unlock()

		go func() {
			if workerDone != nil {
				<-workerDone
			}
			p.closeErr = p.FlushStats(context.Background())
			close(p.closeDone)
		}()
	})

	select {
	case <-p.closeDone:
		return p.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CloseStatsPersistence is a compatibility wrapper for old callers/tests.
func (p *AccountPool) CloseStatsPersistence() {
	_ = p.Close(context.Background())
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	for i := range p.accounts {
		result[i] = p.accounts[i].Clone()
	}
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped:
// the per-account upstream Overages switch (OverageStatus=ENABLED) and the
// global allowOverUsage setting are the two ways to keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}

// RecordLatency records an observed request latency into the account's EWMA
// (α=healthEWMAAlpha). Called from request handlers after a response completes
// so health-aware selection can prefer faster accounts.
func (p *AccountPool) RecordLatency(id string, latencyMs float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordLatencyLocked(id, latencyMs)
}

// healthScore returns a tie-breaker weight for the account: higher = preferred.
// score = effectiveWeight × (1 - ewmaErrorRate) × (1 / (1 + latency/1000))
// Used only to break ties in LRU selection. Lock-free: the caller holds p.mu.
func (p *AccountPool) healthScore(id string, weight int) float64 {
	w := float64(weight)
	if w < 1 {
		w = 1
	}
	h := p.healthStats[id]
	if h == nil || h.samples == 0 {
		return w // no data → default weight
	}
	latencyFactor := 1.0 / (1.0 + h.ewmaLatencyMs/1000.0)
	return w * (1.0 - h.ewmaErrorRate) * latencyFactor
}

// fallbackEarliestCooldown returns the account with the earliest cooldown
// (or one with no cooldown at all) when no fully-healthy candidate exists.
// model="" means "any model". Caller must hold p.mu (at least RLock).
func (p *AccountPool) fallbackEarliestCooldown(model string, excluded map[string]bool, allowOverUsage bool) *config.Account {
	now := time.Now()
	var best *config.Account
	var earliest time.Time
	for i := range p.accounts {
		acc := &p.accounts[i]
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if !accountCredentialDispatchable(acc, now) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

// startAutoRecover launches a background goroutine that periodically refreshes
// disabled accounts' tokens. If a refresh succeeds, the account is re-enabled.
// Exponential backoff per account: 1m → 5m → 25m → max 2h.
func (p *AccountPool) startAutoRecover() {
	p.stopRecover = make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.reprobeDisabled()
			case <-p.stopRecover:
				return
			}
		}
	}()
}

// reprobeDisabled iterates disabled accounts whose reprobe time has arrived,
// validates the credential at the token endpoint, and atomically publishes a
// lifecycle/credential/health-observed recovery transition on success.
func (p *AccountPool) reprobeDisabled() {
	if !config.GetAutoRecoverEnabled() {
		return
	}
	all := config.GetAccounts()
	candidates := make([]accountRecoveryKey, 0, len(all))
	active := make(map[accountRecoveryKey]struct{})
	for _, acc := range all {
		if !accountUsesLegacyAutoRecovery(acc) {
			continue
		}
		key, ok := accountRecoveryKeyFor(acc)
		if !ok {
			continue
		}
		candidates = append(candidates, key)
		active[key] = struct{}{}
	}
	p.pruneRecoveryState(active)

	for _, key := range candidates {
		// Re-read the canonical account at this candidate's actual execution
		// boundary. A prior candidate may have spent time in network I/O while an
		// operator disabled/banned/replaced this account.
		acc, ok := config.GetAccountByID(key.id)
		currentKey, currentKeyOK := accountRecoveryKeyFor(acc)
		if !ok || !currentKeyOK || currentKey != key || !accountUsesLegacyAutoRecovery(acc) {
			p.clearRecoveryState(key)
			continue
		}
		if !p.recoveryRetryReady(key, time.Now()) {
			continue
		}

		// Once a token-endpoint result exists, storage recovery is a local commit
		// stage. Retry it before any network work so a local filesystem failure can
		// never turn into a second refresh-token exchange/rotation.
		if pending, ok := p.pendingRecoveryPublication(key); ok {
			p.publishRecoveryPublication(key, pending)
			continue
		}

		// Keep H1 from immediately before the token round-trip. RefreshTokenForce
		// adopts C2, but an operator/health edit racing the round-trip must still
		// supersede successful recovery rather than being folded into H1.
		recoveryObservation := acc.Clone()
		_, _, _, _, refreshErr := auth.RefreshTokenForce(&acc)
		refreshCompletedAt := time.Now()
		if refreshErr != nil {
			if errors.Is(refreshErr, auth.ErrAccountLifecycleChanged) ||
				errors.Is(refreshErr, auth.ErrAccountCredentialChanged) {
				p.clearRecoveryState(key)
				continue
			}
			var persistenceErr *auth.AccountCredentialPersistenceError
			if errors.As(refreshErr, &persistenceErr) && persistenceErr.Published {
				logger.Warnf("[AccountPool] auto-recovery credential durability warning account=%s: %v", acc.ID, refreshErr)
			} else {
				disposition := auth.ClassifyRefreshFailure(refreshErr)
				if disposition == auth.RefreshFailureRetryable {
					p.scheduleRecoveryRetry(key, refreshCompletedAt)
					continue
				}

				// RefreshTokenForce re-reads canonical credential state while holding
				// the per-lifecycle refresh lock. Use the snapshot it actually attempted,
				// not H1 captured before waiting for that lock.
				attempted := acc.Clone()
				p.publishRecoveryPublication(key, pendingAccountRecoveryPublication{
					kind:     accountRecoveryPublicationBan,
					observed: attempted,
					status:   "BANNED",
					reason:   legacyRecoveryTerminalReason(disposition),
				})
				continue
			}
		}

		// C2 is canonical, including after a post-publication durability warning.
		// Pair C2 with H1 so the config primitive can reject a concurrent health edit.
		recoveryObservation.AccessToken = acc.AccessToken
		recoveryObservation.RefreshToken = acc.RefreshToken
		recoveryObservation.ExpiresAt = acc.ExpiresAt

		p.publishRecoveryPublication(key, pendingAccountRecoveryPublication{
			kind:     accountRecoveryPublicationRecover,
			observed: recoveryObservation,
		})
	}
}

// AccountHealthSnapshot is a read-only view of one account's dispatch/health
// signals, surfaced to operators (admin /admin/pool) so the effect of session
// affinity and health-aware routing is observable: a warm, stuck-to account
// shows lower LatencyMsEWMA than a cold one that keeps getting re-picked.
type AccountHealthSnapshot struct {
	ID              string  `json:"id"`
	Email           string  `json:"email,omitempty"`
	LatencyMsEWMA   float64 `json:"latencyMsEwma"`
	ErrorRateEWMA   float64 `json:"errorRateEwma"`
	Samples         int     `json:"samples"`
	Circuit         string  `json:"circuit"` // closed | open | half-open
	CooldownActive  bool    `json:"cooldownActive"`
	LastDispatchSeq uint64  `json:"lastDispatchSeq"` // monotonic LRU clock; higher = more recently dispatched
}

// HealthSnapshots returns a per-account health view for every account currently
// in the pool. Lock order is p.mu (RLock) then each breaker's own mutex — the
// same order the dispatch path uses (p.mu held while calling cb.isOpen) — so
// this introduces no new deadlock risk.
func (p *AccountPool) HealthSnapshots() []AccountHealthSnapshot {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AccountHealthSnapshot, 0, len(p.accounts))
	for i := range p.accounts {
		acc := &p.accounts[i]
		snap := AccountHealthSnapshot{
			ID:              acc.ID,
			Email:           acc.Email,
			Circuit:         "closed",
			LastDispatchSeq: p.lastDispatchSeq[acc.ID],
		}
		if h := p.healthStats[acc.ID]; h != nil {
			snap.LatencyMsEWMA = h.ewmaLatencyMs
			snap.ErrorRateEWMA = h.ewmaErrorRate
			snap.Samples = h.samples
		}
		if cb := p.circuitState[acc.ID]; cb != nil {
			cb.mu.Lock()
			switch cb.state {
			case circuitOpen:
				snap.Circuit = "open"
			case circuitHalfOpen:
				snap.Circuit = "half-open"
			}
			cb.mu.Unlock()
		}
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd) {
			snap.CooldownActive = true
		}
		out = append(out, snap)
	}
	return out
}

// LatencyAggregate is a customer-safe summary of dispatch latency across the
// pool: no account identities, just the distribution. Surfaced in /v1/stats so
// the effect of session affinity is visible without leaking pool internals.
type LatencyAggregate struct {
	AccountsWithData int     `json:"accountsWithData"`
	LatencyMsMean    float64 `json:"latencyMsMean"`
	LatencyMsMin     float64 `json:"latencyMsMin"`
	LatencyMsMax     float64 `json:"latencyMsMax"`
}

// LatencyAggregate summarizes per-account EWMA latency without exposing which
// account is which. Accounts with no recorded latency are ignored (includes
// accounts that only logged errors, since they have no latency to average).
func (p *AccountPool) LatencyAggregate() LatencyAggregate {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var agg LatencyAggregate
	var sum float64
	for i := range p.accounts {
		h := p.healthStats[p.accounts[i].ID]
		if h == nil || h.samples == 0 || h.ewmaLatencyMs <= 0 {
			continue
		}
		l := h.ewmaLatencyMs
		if agg.AccountsWithData == 0 || l < agg.LatencyMsMin {
			agg.LatencyMsMin = l
		}
		if l > agg.LatencyMsMax {
			agg.LatencyMsMax = l
		}
		sum += l
		agg.AccountsWithData++
	}
	if agg.AccountsWithData > 0 {
		agg.LatencyMsMean = sum / float64(agg.AccountsWithData)
	}
	return agg
}
