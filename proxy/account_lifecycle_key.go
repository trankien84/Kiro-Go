package proxy

import "kiro-go/config"

// accountLifecycleKey scopes request-derived runtime state to the exact logical
// account instance that produced it. Account IDs remain exact identities; the
// generation fences delete/recreate and source-replacement reuse of the same ID.
type accountLifecycleKey struct {
	accountID  string
	generation uint64
}

func lifecycleKeyForAccount(account *config.Account) accountLifecycleKey {
	if account == nil {
		return accountLifecycleKey{}
	}
	generation := account.DispatchGeneration
	if generation == 0 {
		generation, _ = config.GetAccountLifecycleGeneration(account.ID)
	}
	return accountLifecycleKey{accountID: account.ID, generation: generation}
}

func (k accountLifecycleKey) valid() bool {
	return k.accountID != "" && k.generation != 0
}

func (k accountLifecycleKey) current() bool {
	return k.valid() && config.AccountLifecycleMatches(k.accountID, k.generation)
}

// legacyBedrockLifecycleKey reserves generation 0 for pre-lease admin/test call
// sites. Dispatched requests always carry a non-zero generation, so legacy state
// can never contaminate lifecycle-scoped request state.
func legacyBedrockLifecycleKey(accountID string) accountLifecycleKey {
	return accountLifecycleKey{accountID: accountID, generation: 0}
}

func lifecycleOrLegacyKeyForAccount(account *config.Account) accountLifecycleKey {
	key := lifecycleKeyForAccount(account)
	if key.valid() || account == nil || account.ID == "" {
		return key
	}
	return legacyBedrockLifecycleKey(account.ID)
}

func lifecycleRuntimeKeyCanPublish(key accountLifecycleKey) bool {
	if key.accountID == "" {
		return false
	}
	if key.generation == 0 {
		return true
	}
	return key.current()
}
