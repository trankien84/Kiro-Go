package proxy

// sweepRetiredBedrockLifecycleState removes only exact generations that are no
// longer current. It never uses "current=N" as justification to delete a
// different generation while holding a runtime-cache lock, avoiding the stale
// snapshot TOCTOU that could otherwise remove state published by N+1.
func sweepRetiredBedrockLifecycleState() {
	candidates := make(map[accountLifecycleKey]struct{})

	bedrockDiscoveryMu.RLock()
	for key := range bedrockDiscoveryCache {
		if key.valid() {
			candidates[key] = struct{}{}
		}
	}
	bedrockDiscoveryMu.RUnlock()

	bedrockRegionMu.RLock()
	for key := range bedrockRegionCache {
		if key.valid() {
			candidates[key] = struct{}{}
		}
	}
	bedrockRegionMu.RUnlock()

	bedrockThrottle.mu.RLock()
	for key := range bedrockThrottle.until {
		if key.lifecycle.valid() {
			candidates[key.lifecycle] = struct{}{}
		}
	}
	bedrockThrottle.mu.RUnlock()

	retired := make([]accountLifecycleKey, 0)
	for key := range candidates {
		if !key.current() {
			retired = append(retired, key)
		}
	}
	if len(retired) == 0 {
		return
	}

	for _, key := range retired {
		bedrockDiscoveryMu.Lock()
		delete(bedrockDiscoveryCache, key)
		bedrockDiscoveryMu.Unlock()
		bedrockDiscoveryInflight.Delete(key)

		bedrockRegionMu.Lock()
		delete(bedrockRegionCache, key)
		bedrockRegionMu.Unlock()

		bedrockThrottle.mu.Lock()
		for throttleKey := range bedrockThrottle.until {
			if throttleKey.lifecycle == key {
				delete(bedrockThrottle.until, throttleKey)
			}
		}
		bedrockThrottle.mu.Unlock()
	}
}
