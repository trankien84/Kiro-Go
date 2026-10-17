package proxy

import (
	"kiro-go/config"
	"testing"
)

func TestAccountIdentityForLog(t *testing.T) {
	if got := accountIdentityForLog(&config.Account{ID: "id-1", Email: " user@example.invalid "}); got != "user@example.invalid" {
		t.Fatalf("email identity = %q", got)
	}
	if got := accountIdentityForLog(&config.Account{ID: "id-1"}); got != "id-1" {
		t.Fatalf("id fallback = %q", got)
	}
	if got := accountIdentityForLog(&config.Account{}); got != "<unknown>" {
		t.Fatalf("unknown fallback = %q", got)
	}
}
