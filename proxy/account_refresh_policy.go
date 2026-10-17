package proxy

import (
	"kiro-go/config"
	"strings"
)

func accountCredentialNeedsRefresh(account *config.Account, nowUnix int64) bool {
	if account == nil {
		return false
	}
	if strings.TrimSpace(account.AccessToken) == "" {
		return true
	}
	return account.ExpiresAt > 0 && nowUnix > account.ExpiresAt-tokenRefreshSkewSeconds
}
