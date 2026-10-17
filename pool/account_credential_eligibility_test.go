package pool

import (
	"testing"
	"time"

	"kiro-go/config"
)

func TestAccountCredentialDispatchableKeepsRefreshableOAuthInRefreshWindow(t *testing.T) {
	now := time.Now()
	account := config.Account{
		ID:           "oauth",
		AuthMethod:   "social",
		AccessToken:  "expiring-access",
		RefreshToken: "refresh-token",
		ExpiresAt:    now.Add(30 * time.Second).Unix(),
	}
	if !accountCredentialDispatchable(&account, now) {
		t.Fatal("refreshable OAuth account was starved before request-side refresh")
	}

	account.AccessToken = ""
	account.ExpiresAt = 0
	if !accountCredentialDispatchable(&account, now) {
		t.Fatal("OAuth account with missing access token but valid refresh token must reach the refresh guard")
	}
}

func TestAccountCredentialDispatchableRejectsNonRefreshableExpiredCredential(t *testing.T) {
	now := time.Now()
	account := config.Account{
		ID:         "static",
		AuthMethod: "anthropic_api",
		ExpiresAt:  now.Add(30 * time.Second).Unix(),
	}
	if accountCredentialDispatchable(&account, now) {
		t.Fatal("non-refreshable credential inside expiry skew remained dispatchable")
	}
}

func TestGetNextSelectionKeepsNearExpiryRefreshableOAuth(t *testing.T) {
	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{{
			ID:           "oauth",
			AuthMethod:   "social",
			AccessToken:  "expiring-access",
			RefreshToken: "refresh-token",
			ExpiresAt:    now.Add(30 * time.Second).Unix(),
			Enabled:      true,
		}},
		cooldowns:       make(map[string]time.Time),
		circuitState:    make(map[string]*circuitBreaker),
		healthStats:     make(map[string]*accountHealth),
		lastDispatchSeq: make(map[string]uint64),
	}
	// This unit test bypasses Reload, so seed the lifecycle table exactly as a
	// normal reload would before selectionForAccountLocked creates the lease.
	p.lifecycleGenerationByID = map[string]uint64{"oauth": 1}

	selection := p.GetNextSelectionForModelExcluding("", nil)
	if selection == nil || selection.Account.ID != "oauth" {
		t.Fatalf("near-expiry refreshable OAuth account was not selectable: %#v", selection)
	}
}

func TestGetNextSelectionFallbackDoesNotResurrectExpiredStaticCredential(t *testing.T) {
	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{{
			ID:         "static",
			AuthMethod: "anthropic_api",
			ExpiresAt:  now.Add(30 * time.Second).Unix(),
			Enabled:    true,
		}},
		cooldowns:       map[string]time.Time{"static": now.Add(time.Minute)},
		circuitState:    make(map[string]*circuitBreaker),
		healthStats:     make(map[string]*accountHealth),
		lastDispatchSeq: make(map[string]uint64),
	}
	p.lifecycleGenerationByID = map[string]uint64{"static": 1}

	if selection := p.GetNextSelectionForModelExcluding("", nil); selection != nil {
		t.Fatalf("degraded fallback resurrected expired static credential: %#v", selection)
	}
}

func TestAccountCredentialDispatchableRejectsExpiredOAuthWithoutRefreshToken(t *testing.T) {
	now := time.Now()
	account := config.Account{
		ID:          "oauth-no-refresh",
		AuthMethod:  "social",
		AccessToken: "expired-access",
		ExpiresAt:   now.Add(-time.Second).Unix(),
	}
	if accountCredentialDispatchable(&account, now) {
		t.Fatal("expired OAuth credential without a refresh token remained dispatchable")
	}
}

func TestAccountCredentialDispatchablePreservesUnknownExpiryEligibility(t *testing.T) {
	now := time.Now()
	account := config.Account{
		ID:         "legacy-unknown-expiry",
		AuthMethod: "social",
		ExpiresAt:  0,
	}
	if !accountCredentialDispatchable(&account, now) {
		t.Fatal("unknown expiry was turned into a new credential-admission failure")
	}
}

func TestAccountUsesLegacyAutoRecoveryOnlyForFailureDrivenRefreshableOAuth(t *testing.T) {
	oauth := func() config.Account {
		return config.Account{
			AuthMethod:   "social",
			RefreshToken: "refresh-token",
		}
	}
	tests := []struct {
		name    string
		account config.Account
		want    bool
	}{
		{
			name: "manual disabled marker has no automatic recovery",
			account: func() config.Account {
				a := oauth()
				a.Enabled = false
				a.BanStatus = "DISABLED"
				return a
			}(),
			want: false,
		},
		{
			name: "failure disabled oauth retains legacy automatic recovery",
			account: func() config.Account {
				a := oauth()
				a.Enabled = false
				a.BanStatus = "DISABLED"
				a.BanReason = "authentication failed"
				return a
			}(),
			want: true,
		},
		{
			name: "missing refresh token is not recoverable",
			account: config.Account{
				AuthMethod: "social", Enabled: false, BanStatus: "DISABLED", BanReason: "authentication failed",
			},
			want: false,
		},
		{
			name: "static api key never enters oauth recovery",
			account: config.Account{
				AuthMethod: "api_key", KiroApiKey: "ksk-test", Enabled: false, BanStatus: "DISABLED", BanReason: "authentication failed",
			},
			want: false,
		},
		{
			name: "custom relay never enters oauth recovery",
			account: config.Account{
				AuthMethod: "custom_api", KiroApiKey: "relay-key", Enabled: false, BanStatus: "DISABLED", BanReason: "authentication failed",
			},
			want: false,
		},
		{
			name: "bedrock never enters oauth recovery",
			account: config.Account{
				AuthMethod: "bedrock", BedrockAPIKey: "bedrock-key", Enabled: false, BanStatus: "DISABLED", BanReason: "authentication failed",
			},
			want: false,
		},
		{
			name: "direct anthropic never enters oauth recovery",
			account: config.Account{
				AuthMethod: "anthropic_api", Enabled: false, BanStatus: "DISABLED", BanReason: "authentication failed",
			},
			want: false,
		},
		{
			name: "enabled account is not reprobed",
			account: func() config.Account {
				a := oauth()
				a.Enabled = true
				a.BanStatus = "DISABLED"
				a.BanReason = "authentication failed"
				return a
			}(),
			want: false,
		},
		{
			name: "banned account is terminal for legacy recovery",
			account: func() config.Account {
				a := oauth()
				a.Enabled = false
				a.BanStatus = "BANNED"
				a.BanReason = "invalid grant"
				return a
			}(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accountUsesLegacyAutoRecovery(tt.account); got != tt.want {
				t.Fatalf("accountUsesLegacyAutoRecovery(%+v)=%t, want %t", tt.account, got, tt.want)
			}
		})
	}
}
