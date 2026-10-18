package auth

import (
	"context"
	"errors"
	"kiro-go/config"
)

var ErrAccountCredentialChanged = errors.New("account credential changed during token refresh")

type pendingCredentialPublication struct {
	observed     config.Account
	accessToken  string
	refreshToken string
	expiresAt    int64
	profileArn   string
}

var publishRefreshedCredentialState = config.UpdateAccountCredentialStateIfObserved

func newRefreshLockEntry() *refreshLockEntry {
	entry := &refreshLockEntry{gate: make(chan struct{}, 1)}
	entry.gate <- struct{}{}
	return entry
}

// acquireRefreshEntryContext serializes one exact credential lifecycle while
// allowing a canceled waiter to stop waiting. A canceled caller may still take
// an immediately available gate so refreshTokenLockedContext can publish a
// pending post-IdP C2 before honoring cancellation; it never waits for that gate
// after cancellation has become observable.
func acquireRefreshEntryContext(ctx context.Context, key refreshLockKey) (*refreshLockEntry, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}

	refreshMu.Lock()
	entry := refreshLocks[key]
	if entry == nil {
		entry = newRefreshLockEntry()
		refreshLocks[key] = entry
	}
	entry.refs++
	refreshMu.Unlock()

	cancelReference := func() {
		refreshMu.Lock()
		entry.refs--
		if entry.refs == 0 && entry.pending == nil && refreshLocks[key] == entry {
			delete(refreshLocks, key)
		}
		refreshMu.Unlock()
	}

	// Fast-path an idle gate before consulting ctx. This preserves the existing
	// invariant that an already-staged C2 is published even for a canceled caller.
	select {
	case <-entry.gate:
	default:
		select {
		case <-entry.gate:
		case <-ctx.Done():
			cancelReference()
			return nil, nil, ctx.Err()
		}
	}

	release := func() {
		refreshMu.Lock()
		entry.refs--
		if entry.refs == 0 && entry.pending == nil && refreshLocks[key] == entry {
			// Return the token while the registry is still locked, then remove the
			// entry. A new caller therefore cannot create/acquire a replacement gate
			// until this critical section has actually ended.
			entry.gate <- struct{}{}
			delete(refreshLocks, key)
			refreshMu.Unlock()
			return
		}
		refreshMu.Unlock()
		entry.gate <- struct{}{}
	}
	return entry, release, nil
}

func acquireRefreshEntry(key refreshLockKey) (*refreshLockEntry, func()) {
	entry, release, err := acquireRefreshEntryContext(context.Background(), key)
	if err != nil {
		panic(err) // context.Background cannot be canceled
	}
	return entry, release
}

// tryAcquireExistingRefreshEntry never waits behind an active refresh. The
// expected pointer prevents an older pruning snapshot from inspecting a new
// entry that reused the same lifecycle key.
func tryAcquireExistingRefreshEntry(key refreshLockKey, expected *refreshLockEntry) (*refreshLockEntry, func(), bool) {
	refreshMu.Lock()
	entry := refreshLocks[key]
	if entry == nil || entry != expected || entry.refs != 0 || entry.pending == nil {
		refreshMu.Unlock()
		return nil, nil, false
	}
	select {
	case <-entry.gate:
		entry.refs++
	default:
		refreshMu.Unlock()
		return nil, nil, false
	}
	refreshMu.Unlock()

	release := func() {
		refreshMu.Lock()
		entry.refs--
		if entry.refs == 0 && entry.pending == nil && refreshLocks[key] == entry {
			entry.gate <- struct{}{}
			delete(refreshLocks, key)
			refreshMu.Unlock()
			return
		}
		refreshMu.Unlock()
		entry.gate <- struct{}{}
	}
	return entry, release, true
}

// pruneStaleRefreshEntries opportunistically retires obsolete pending C2.
// A busy gate is skipped: pruning must not delay an unrelated refresh or
// interrupt a committed rotation, and pending C2 is never discarded merely
// because its owner's context was canceled.
func pruneStaleRefreshEntries() {
	type candidate struct {
		key   refreshLockKey
		entry *refreshLockEntry
	}
	refreshMu.Lock()
	candidates := make([]candidate, 0, len(refreshLocks))
	for key, entry := range refreshLocks {
		if entry.refs == 0 && entry.pending != nil {
			candidates = append(candidates, candidate{key: key, entry: entry})
		}
	}
	refreshMu.Unlock()

	for _, candidate := range candidates {
		entry, release, ok := tryAcquireExistingRefreshEntry(candidate.key, candidate.entry)
		if !ok {
			continue
		}
		pending := entry.pending
		if pending != nil && !config.AccountCredentialPublicationObservationMatches(pending.observed.Clone()) {
			entry.pending = nil
		}
		release()
	}
}

func adoptCanonicalCredential(account *config.Account, key refreshLockKey) (config.Account, error) {
	live, ok := config.GetAccountByID(key.id)
	if !ok || live.DispatchGeneration != key.generation {
		return config.Account{}, ErrAccountLifecycleChanged
	}
	*account = live.Clone()
	return live, nil
}

// publishPendingCredential retries only local publication. It never contacts the
// IdP. After the IdP has returned C2, a local write failure must not cause C1 to
// be replayed because providers are allowed to rotate/invalidate refresh tokens.
func publishPendingCredential(
	entry *refreshLockEntry,
	key refreshLockKey,
	account *config.Account,
) (config.Account, error) {
	pending := entry.pending
	if pending == nil {
		return config.Account{}, nil
	}

	applied, persistErr := publishRefreshedCredentialState(
		pending.observed.Clone(),
		pending.accessToken,
		pending.refreshToken,
		pending.expiresAt,
		pending.profileArn,
	)
	if !applied {
		if persistErr != nil {
			// Pre-publication failure: canonical config/caller remain C1 and C2 stays
			// staged in RAM for a later local-only publication retry.
			live, liveErr := adoptCanonicalCredential(account, key)
			if liveErr != nil {
				entry.pending = nil
				return config.Account{}, liveErr
			}
			return live, &AccountCredentialPersistenceError{
				AccountID: key.id,
				Published: false,
				Err:       persistErr,
			}
		}

		// No publication and no storage error means the source observation was
		// superseded by another credential or lifecycle. The candidate is stale.
		entry.pending = nil
		if live, ok := config.GetAccountByID(key.id); ok && live.DispatchGeneration == key.generation {
			*account = live.Clone()
			return live, ErrAccountCredentialChanged
		}
		return config.Account{}, ErrAccountLifecycleChanged
	}

	// Post-publication durability errors still mean C2 is canonical/visible.
	// Adopt it, clear pending, and report durability as an independent dimension.
	entry.pending = nil
	live, liveErr := adoptCanonicalCredential(account, key)
	if liveErr != nil {
		if persistErr != nil {
			return config.Account{}, errors.Join(liveErr, &AccountCredentialPersistenceError{
				AccountID: key.id,
				Published: true,
				Err:       persistErr,
			})
		}
		return config.Account{}, liveErr
	}
	if persistErr != nil {
		return live, &AccountCredentialPersistenceError{
			AccountID: key.id,
			Published: true,
			Err:       persistErr,
		}
	}
	return live, nil
}
