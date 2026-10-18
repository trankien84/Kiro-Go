package config

// accountInfoRevisionByID is a process-local monotonic publication sequence for
// account-info/quota state. It is deliberately not persisted: observations only
// need ordering within the current process, while DispatchGeneration separately
// fences delete/recreate lifecycles.
//
// Every accessor below requires cfgLock to be held. Keeping the sequence outside
// Account's canonical persisted image prevents a config reload/admin struct
// replacement from resetting the counter and creating an ABA window.
var accountInfoRevisionByID = make(map[string]uint64)

func accountInfoRevisionLocked(id string) uint64 {
	id = normalizeAccountLifecycleID(id)
	if id == "" {
		return 0
	}
	return accountInfoRevisionByID[id]
}

func bumpAccountInfoRevisionLocked(id string) uint64 {
	id = normalizeAccountLifecycleID(id)
	if id == "" {
		return 0
	}
	next := accountInfoRevisionByID[id] + 1
	if next == 0 {
		// Reserve zero as the initial/unpublished value even after the practically
		// unreachable uint64 wraparound.
		next = 1
	}
	accountInfoRevisionByID[id] = next
	return next
}

func forgetAccountInfoRevisionLocked(id string) {
	id = normalizeAccountLifecycleID(id)
	if id != "" {
		delete(accountInfoRevisionByID, id)
	}
}
