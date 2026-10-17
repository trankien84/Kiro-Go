package pool

import (
	"strings"
	"time"

	"kiro-go/config"
)

// accountCredentialDispatchable reports whether an account may proceed to the
// request-side credential guard. It deliberately preserves the historical
// selector contract when no explicit expiry is known: this helper is an expiry
// gate, not a general credential-completeness validator. For an explicit expiry
// inside the safety skew, only refreshable Kiro OAuth credentials with a refresh
// token remain eligible so ensureValidTokenForSelection can refresh them before
// upstream dispatch. Static/non-refreshable credentials stay fail-closed there.
func accountCredentialDispatchable(account *config.Account, now time.Time) bool {
	if account == nil {
		return false
	}
	if account.ExpiresAt <= 0 || now.Unix() <= account.ExpiresAt-tokenRefreshSkewSeconds {
		return true
	}
	return account.SupportsKiroTokenRefresh() && strings.TrimSpace(account.RefreshToken) != ""
}
