package proxy

// Adaptive throttle tracking for the Bedrock provider.
//
// AWS Service Quotas (per-model RPM/TPM) is not reachable in this deployment, so
// throughput is learned from observed 429 responses instead. When an invoke or
// converse call returns HTTP 429 (ThrottlingException), the (account, model) pair
// is put into a cooldown (honoring Retry-After when present). While cooling down,
// doBedrockInvoke / doBedrockConverseInvoke short-circuit BEFORE the network call
// with errBedrockThrottled — a pre-stream error, so the handler excludes that
// account and fails over to another one instead of repeatedly hitting a throttled
// model. Cooldowns are per (account, model): a throttle on one account's model
// never penalizes the same model on a different account.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
)

// errBedrockThrottled signals a model is in a throttle cooldown. It is a
// pre-stream failure, so callers fail over to another account. The message
// deliberately omits "429" so it is NOT matched by isQuotaErrorMessage (which
// would escalate this per-model advisory skip into an account-wide cooldown).
var errBedrockThrottled = errors.New("bedrock: model in throttle cooldown, failing over")

const (
	// bedrockDefaultThrottleCooldown applies when a 429 carries no Retry-After.
	bedrockDefaultThrottleCooldown = 20 * time.Second
	// bedrockMaxThrottleCooldown caps a (possibly hostile) Retry-After value.
	bedrockMaxThrottleCooldown = 5 * time.Minute
)

type bedrockThrottleKey struct {
	lifecycle accountLifecycleKey
	model     string
}

type bedrockThrottleState struct {
	mu    sync.RWMutex
	until map[bedrockThrottleKey]time.Time
}

var bedrockThrottle = &bedrockThrottleState{until: map[bedrockThrottleKey]time.Time{}}

func newBedrockThrottleKey(account *config.Account, model string) bedrockThrottleKey {
	return bedrockThrottleKey{lifecycle: lifecycleOrLegacyKeyForAccount(account), model: model}
}

// remaining returns the time left in the (account, model) cooldown, or 0.
func (s *bedrockThrottleState) remainingForAccount(account *config.Account, model string) time.Duration {
	key := newBedrockThrottleKey(account, model)
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.until[key]
	if !ok {
		return 0
	}
	if d := time.Until(t); d > 0 {
		return d
	}
	return 0
}

// record starts or extends a cooldown for the (account, model) pair. A
// non-positive duration falls back to the default; oversized values are capped.
// An existing longer cooldown is kept (never shortened by a concurrent 429 that
// carried a smaller Retry-After).
func (s *bedrockThrottleState) recordForAccount(account *config.Account, model string, d time.Duration) {
	if d <= 0 {
		d = bedrockDefaultThrottleCooldown
	}
	if d > bedrockMaxThrottleCooldown {
		d = bedrockMaxThrottleCooldown
	}
	until := time.Now().Add(d)
	key := newBedrockThrottleKey(account, model)
	if !lifecycleRuntimeKeyCanPublish(key.lifecycle) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for existingKey, existingUntil := range s.until {
		if !existingUntil.After(now) {
			delete(s.until, existingKey)
		}
	}
	if existing, ok := s.until[key]; ok && existing.After(until) {
		return
	}
	s.until[key] = until
}

// noteBedrockResponseThrottle records a cooldown when a Bedrock response is a 429.
func noteBedrockResponseThrottleForAccount(account *config.Account, model string, resp *http.Response) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	bedrockThrottle.recordForAccount(account, model, parseRetryAfterSeconds(resp.Header.Get("Retry-After")))
}

func (s *bedrockThrottleState) clearForLifecycle(account *config.Account) bool {
	if s == nil || account == nil || strings.TrimSpace(account.ID) == "" || account.DispatchGeneration == 0 {
		return false
	}
	key := accountLifecycleKey{accountID: account.ID, generation: account.DispatchGeneration}
	s.mu.Lock()
	for throttleKey := range s.until {
		if throttleKey.lifecycle == key {
			delete(s.until, throttleKey)
		}
	}
	s.mu.Unlock()
	return true
}

// Legacy ID-only throttle helpers use generation 0 so unchanged tests/admin code
// keeps its previous semantics without sharing state with dispatched requests.
func (s *bedrockThrottleState) remaining(accountID, model string) time.Duration {
	if accountID == "" {
		return 0
	}
	key := bedrockThrottleKey{lifecycle: legacyBedrockLifecycleKey(accountID), model: model}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.until[key]
	if !ok {
		return 0
	}
	if d := time.Until(t); d > 0 {
		return d
	}
	return 0
}

func (s *bedrockThrottleState) record(accountID, model string, d time.Duration) {
	if accountID == "" {
		return
	}
	if d <= 0 {
		d = bedrockDefaultThrottleCooldown
	}
	if d > bedrockMaxThrottleCooldown {
		d = bedrockMaxThrottleCooldown
	}
	until := time.Now().Add(d)
	key := bedrockThrottleKey{lifecycle: legacyBedrockLifecycleKey(accountID), model: model}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.until[key]; ok && existing.After(until) {
		return
	}
	s.until[key] = until
}

func noteBedrockResponseThrottle(accountID, model string, resp *http.Response) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	bedrockThrottle.record(accountID, model, parseRetryAfterSeconds(resp.Header.Get("Retry-After")))
}

// parseRetryAfterSeconds parses the delta-seconds form of a Retry-After header
// (the form Bedrock uses); returns 0 for absent/unparseable values.
func parseRetryAfterSeconds(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}
