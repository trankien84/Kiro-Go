package proxy

import (
	"kiro-go/config"
	"testing"
)

func TestAccountCredentialNeedsRefreshForMissingAndNearExpiryToken(t *testing.T) {
	now := int64(2_000_000_000)
	cases := []struct {
		name    string
		account config.Account
		want    bool
	}{
		{name: "missing access token", account: config.Account{ExpiresAt: now + 3600}, want: true},
		{name: "inside safety skew", account: config.Account{AccessToken: "access", ExpiresAt: now + tokenRefreshSkewSeconds - 1}, want: true},
		{name: "at safety boundary preserves existing strict comparison", account: config.Account{AccessToken: "access", ExpiresAt: now + tokenRefreshSkewSeconds}, want: false},
		{name: "fresh", account: config.Account{AccessToken: "access", ExpiresAt: now + tokenRefreshSkewSeconds + 1}, want: false},
		{name: "non expiring", account: config.Account{AccessToken: "static", ExpiresAt: 0}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountCredentialNeedsRefresh(&tc.account, now); got != tc.want {
				t.Fatalf("accountCredentialNeedsRefresh()=%t, want %t", got, tc.want)
			}
		})
	}
}
