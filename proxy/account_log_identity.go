package proxy

import (
	"kiro-go/config"
	"strings"
)

func accountIdentityForLog(account *config.Account) string {
	if account == nil {
		return "<unknown>"
	}
	if email := strings.TrimSpace(account.Email); email != "" {
		return email
	}
	if id := strings.TrimSpace(account.ID); id != "" {
		return id
	}
	return "<unknown>"
}
