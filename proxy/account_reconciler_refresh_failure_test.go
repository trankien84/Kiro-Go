package proxy

import (
	"testing"

	"kiro-go/auth"
	"kiro-go/config"
)

func TestAccountReconcilerStructuredSlowDownRemainsRetryable(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	healthChanged, superseded, err := publishReconciledCredentialFailure(
		&account,
		&auth.OAuthTokenEndpointError{StatusCode: 400, Code: "slow_down", Description: "too frequent"},
	)
	if err != nil || healthChanged || superseded {
		t.Fatalf("slow_down publication: healthChanged=%t superseded=%t err=%v", healthChanged, superseded, err)
	}
	current, _ := config.GetAccountByID(account.ID)
	if !current.Enabled || current.BanStatus == "BANNED" {
		t.Fatalf("slow_down incorrectly banned account: %+v", current)
	}
}

func TestAccountReconcilerStructuredInvalidClientIsTerminal(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	healthChanged, superseded, err := publishReconciledCredentialFailure(
		&account,
		&auth.OAuthTokenEndpointError{StatusCode: 401, Code: "invalid_client", Description: "client secret expired"},
	)
	if err != nil || !healthChanged || superseded {
		t.Fatalf("invalid_client publication: healthChanged=%t superseded=%t err=%v", healthChanged, superseded, err)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "BANNED" {
		t.Fatalf("invalid_client did not converge to BANNED: %+v", current)
	}
}
