package pool

import (
	"time"

	"kiro-go/config"
)

// AccountSelection is the immutable dispatch lease captured while AccountPool's
// selection lock is held. Account is deep-cloned so nested slices/maps cannot be
// mutated through config/pool-owned backing storage after dispatch.
type AccountSelection struct {
	Account             config.Account
	LifecycleGeneration uint64
	StatsGeneration     uint64
}

func (s AccountSelection) ID() string { return s.Account.ID }

func (p *AccountPool) selectionForAccountLocked(account *config.Account) *AccountSelection {
	if account == nil {
		return nil
	}
	id := account.ID
	lifecycleGeneration := p.lifecycleGenerationByID[id]
	if lifecycleGeneration == 0 {
		// Preserve legacy/test pools that populate p.accounts directly without a
		// Reload. Generation zero is deliberately non-authoritative: request-side
		// fenced mutations reject it, while account-only selector wrappers can still
		// return the same deep snapshot they historically returned.
		lifecycleGeneration = account.DispatchGeneration
	}
	statsGeneration := p.statsEpochByID[id]
	accountSnapshot := account.Clone()
	accountSnapshot.DispatchGeneration = lifecycleGeneration
	return &AccountSelection{
		Account:             accountSnapshot,
		LifecycleGeneration: lifecycleGeneration,
		StatsGeneration:     statsGeneration,
	}
}

func (p *AccountPool) selectionCurrentLocked(selection AccountSelection) bool {
	id := selection.Account.ID
	return id != "" && selection.LifecycleGeneration != 0 &&
		p.lifecycleGenerationByID[id] == selection.LifecycleGeneration
}

// IsSelectionCurrent reports whether the pool still exposes the logical account
// lifecycle captured by the dispatch lease. It does not require the account to
// remain routable; disable/quota reloads preserve the same lifecycle generation.
func (p *AccountPool) IsSelectionCurrent(selection AccountSelection) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.selectionCurrentLocked(selection)
}

// GetSelectionByID returns a dispatch lease for a currently routable account.
func (p *AccountPool) GetSelectionByID(id string) *AccountSelection {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return p.selectionForAccountLocked(&p.accounts[i])
		}
	}
	return nil
}

// clearRuntimeLifecycleLocked removes all runtime state associated with an ID
// when the same textual account ID starts a new logical lifecycle. This prevents the new
// account from inheriting cooldown/circuit/health/model/affinity state.
func (p *AccountPool) clearRuntimeLifecycleLocked(id string) {
	delete(p.cooldowns, id)
	delete(p.errorCounts, id)
	delete(p.circuitState, id)
	delete(p.healthStats, id)
	delete(p.lastDispatchSeq, id)
	delete(p.modelLists, id)
	delete(p.modelListUnavailable, id)
	// Recovery scheduling is lifecycle-keyed. Clearing a textual ID must retire
	// every generation for that ID while leaving unrelated accounts untouched.
	for key := range p.reprobeBackoff {
		if key.id == id {
			delete(p.reprobeBackoff, key)
		}
	}
	for key := range p.reprobeNext {
		if key.id == id {
			delete(p.reprobeNext, key)
		}
	}
	for key := range p.recoveryPending {
		if key.id == id {
			delete(p.recoveryPending, key)
		}
	}
	for key, binding := range p.apiKeyAffinity {
		if binding.accountID == id {
			delete(p.apiKeyAffinity, key)
		}
	}
}

// syncLifecycleGenerationsLocked imports config-owned dispatch generations and
// invalidates runtime state on delete/recreate or source replacement.
func (p *AccountPool) syncLifecycleGenerationsLocked(inventory []config.AccountWithStatsGeneration) {
	if p.lifecycleGenerationByID == nil {
		p.lifecycleGenerationByID = make(map[string]uint64)
	}
	active := make(map[string]uint64, len(inventory))
	for _, item := range inventory {
		id := item.Account.ID
		generation := item.LifecycleGeneration
		if id == "" || generation == 0 {
			continue
		}
		if previous := p.lifecycleGenerationByID[id]; previous != 0 && previous != generation {
			p.clearRuntimeLifecycleLocked(id)
		}
		p.lifecycleGenerationByID[id] = generation
		active[id] = generation
	}
	for id := range p.lifecycleGenerationByID {
		if _, ok := active[id]; ok {
			continue
		}
		p.clearRuntimeLifecycleLocked(id)
		delete(p.lifecycleGenerationByID, id)
	}
	for key, binding := range p.apiKeyAffinity {
		if active[binding.accountID] != binding.lifecycleGeneration {
			delete(p.apiKeyAffinity, key)
		}
	}
}

func (p *AccountPool) recordSuccessLocked(id string) {
	if cd, ok := p.cooldowns[id]; ok && time.Until(cd) > cooldownClearThreshold {
		// Preserve a hard cooldown established by quota/disable handling.
	} else {
		delete(p.cooldowns, id)
	}
	p.errorCounts[id] = 0
	if cb := p.circuitState[id]; cb != nil {
		cb.reset()
	}
	p.recordHealthObservation(id, false)
}

// RecordSuccessForSelection fences late completions from mutating the runtime
// state of a newly recreated account with the same ID.
func (p *AccountPool) RecordSuccessForSelection(selection AccountSelection) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.selectionCurrentLocked(selection) {
		return false
	}
	p.recordSuccessLocked(selection.Account.ID)
	return true
}

func (p *AccountPool) recordErrorLocked(id string, isQuotaError bool) {
	now := time.Now()
	p.errorCounts[id]++
	if isQuotaError {
		setCooldownIfLater(p.cooldowns, id, now.Add(time.Hour))
	} else if p.errorCounts[id] >= 3 {
		setCooldownIfLater(p.cooldowns, id, now.Add(time.Minute))
	}
	cb := p.circuitState[id]
	if cb == nil {
		cb = &circuitBreaker{}
		if p.circuitState == nil {
			p.circuitState = make(map[string]*circuitBreaker)
		}
		p.circuitState[id] = cb
	}
	cb.recordError(now)
	p.recordHealthObservation(id, true)
}

func (p *AccountPool) RecordErrorForSelection(selection AccountSelection, isQuotaError bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.selectionCurrentLocked(selection) {
		return false
	}
	p.recordErrorLocked(selection.Account.ID, isQuotaError)
	return true
}

func (p *AccountPool) recordLatencyLocked(id string, latencyMs float64) {
	if p.healthStats == nil {
		p.healthStats = make(map[string]*accountHealth)
	}
	h := p.healthStats[id]
	if h == nil {
		h = &accountHealth{}
		p.healthStats[id] = h
	}
	if h.ewmaLatencyMs == 0 {
		h.ewmaLatencyMs = latencyMs
	} else {
		h.ewmaLatencyMs = healthEWMAAlpha*latencyMs + (1-healthEWMAAlpha)*h.ewmaLatencyMs
	}
	if h.samples == 0 {
		h.samples = 1
	}
}

func (p *AccountPool) RecordLatencyForSelection(selection AccountSelection, latencyMs float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.selectionCurrentLocked(selection) {
		return false
	}
	p.recordLatencyLocked(selection.Account.ID, latencyMs)
	return true
}

// DisableAccountForSelection persists the disable only if config still exposes
// the lifecycle that produced the failure.
func (p *AccountPool) DisableAccountForSelection(selection AccountSelection, reason string) bool {
	applied, err := config.SetAccountBanStatusIfLifecycle(
		selection.Account.ID,
		selection.LifecycleGeneration,
		"DISABLED",
		reason,
	)
	if err != nil || !applied {
		return false
	}
	p.mu.Lock()
	if p.selectionCurrentLocked(selection) {
		p.cooldowns[selection.Account.ID] = time.Now().Add(24 * time.Hour)
	}
	p.mu.Unlock()
	p.Reload()
	return true
}

// UpdateTokenForSelection is a CAS-style refresh publication. A refresh result
// from lifecycle N is discarded after delete/recreate to N+1.
func (p *AccountPool) UpdateTokenForSelection(selection AccountSelection, accessToken, refreshToken string, expiresAt int64) bool {
	applied, err := config.UpdateAccountTokenIfLifecycle(
		selection.Account.ID,
		selection.LifecycleGeneration,
		accessToken,
		refreshToken,
		expiresAt,
	)
	if err != nil || !applied {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.selectionCurrentLocked(selection) {
		return false
	}
	for i := range p.accounts {
		if p.accounts[i].ID != selection.Account.ID {
			continue
		}
		p.accounts[i].AccessToken = accessToken
		if refreshToken != "" {
			p.accounts[i].RefreshToken = refreshToken
		}
		p.accounts[i].ExpiresAt = expiresAt
	}
	return true
}

// UpdateProfileArnForSelection publishes a lazily resolved profile ARN only for
// the lifecycle that initiated the lookup.
func (p *AccountPool) UpdateProfileArnForSelection(selection AccountSelection, profileArn string) bool {
	applied, err := config.UpdateAccountProfileArnIfLifecycle(
		selection.Account.ID,
		selection.LifecycleGeneration,
		profileArn,
	)
	if err != nil || !applied {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.selectionCurrentLocked(selection) {
		return false
	}
	for i := range p.accounts {
		if p.accounts[i].ID == selection.Account.ID {
			p.accounts[i].ProfileArn = profileArn
		}
	}
	return true
}
