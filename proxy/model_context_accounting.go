package proxy

import (
	"math"
	"strings"
	"time"

	"kiro-go/config"
)

// accountModelContextWindow returns the exact model limit from the selected
// account/lifecycle snapshot only. Never read the public union or another
// account's token limits: they may differ by entitlement or deployment.
func (h *Handler) accountModelContextWindow(account *config.Account, providerModelID string) (int, bool) {
	if h == nil || account == nil || strings.TrimSpace(providerModelID) == "" {
		return 0, false
	}
	key := lifecycleOrLegacyKeyForAccount(account)
	h.providerStateMu.RLock()
	snapshot, ok := h.accountModelSnapshots[key]
	h.providerStateMu.RUnlock()
	if !ok || snapshot.Stale || !time.Now().Before(snapshot.FreshUntil) || snapshot.LifecycleGeneration != key.generation {
		return 0, false
	}
	for _, model := range snapshot.Models {
		if model.ModelId == providerModelID && model.TokenLimits != nil && model.TokenLimits.MaxInputTokens > 0 {
			return model.TokenLimits.MaxInputTokens, true
		}
	}
	if metadata, exists := snapshot.Metadata[providerModelID]; exists && metadata.MaxInputTokens > 0 && metadata.MaxInputTokens <= math.MaxInt {
		return int(metadata.MaxInputTokens), true
	}
	return 0, false
}

// usageInputFromContextPercentage is an *estimate*, not an observed token
// count. It is used only if Kiro did not supply a positive exact input count.
// An exact selected-account model snapshot takes priority over name heuristics.
func (h *Handler) usageInputFromContextPercentage(account *config.Account, providerID, requestedModel string, pct float64) int {
	if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 100 {
		return 0
	}
	window, known := h.accountModelContextWindow(account, providerID)
	if !known {
		window = getContextWindowSize(providerID)
	}
	if window <= 0 {
		window = getContextWindowSize(requestedModel)
	}
	return int(pct * float64(window) / 100.0)
}
