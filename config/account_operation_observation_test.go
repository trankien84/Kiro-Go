package config

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAccountInfoObservedPublicationRejectsCredentialAdvance(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "acct",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    1000,
		LastRefresh:  10,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, ok := GetAccountByID("acct")
	if !ok || observed.DispatchGeneration == 0 {
		t.Fatalf("GetAccountByID: ok=%t account=%+v", ok, observed)
	}
	if !AccountInfoObservationMatches(observed) {
		t.Fatal("fresh observation unexpectedly did not match")
	}
	applied, err := UpdateAccountCredentialStateIfLifecycle(
		observed.ID,
		observed.DispatchGeneration,
		"access-2",
		"refresh-2",
		2000,
		"",
	)
	if err != nil || !applied {
		t.Fatalf("credential advance: applied=%t err=%v", applied, err)
	}
	if AccountInfoObservationMatches(observed) {
		t.Fatal("old-token observation still matched after credential refresh")
	}
	banned, err := SetAccountBanStatusIfObserved(observed, "BANNED", "stale 401")
	if err != nil {
		t.Fatalf("stale ban: %v", err)
	}
	if banned {
		t.Fatal("stale token observation disabled refreshed account")
	}
	current, _ := GetAccountByID("acct")
	if !current.Enabled || current.AccessToken != "access-2" {
		t.Fatalf("refreshed account was contaminated: %+v", current)
	}
}

func TestAccountInfoObservedPublicationRejectsNewerInfoInSameSecond(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "acct",
		AuthMethod:   "social",
		AccessToken:  "access",
		ExpiresAt:    2000,
		UsageCurrent: 10,
		UsageLimit:   100,
		UsagePercent: 0.1,
		LastRefresh:  20,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, _ := GetAccountByID("acct")
	if applied, err := UpdateAccountInfoIfLifecycle(observed.ID, observed.DispatchGeneration, AccountInfo{
		UsageCurrent: 80,
		UsageLimit:   100,
		UsagePercent: 0.8,
		// Deliberately the same Unix second as the observed snapshot. LastRefresh
		// alone cannot order these two publications.
		LastRefresh: 20,
	}); err != nil || !applied {
		t.Fatalf("publish newer info: applied=%t err=%v", applied, err)
	}
	applied, err := UpdateAccountInfoIfObserved(
		observed,
		AccountInfo{UsageCurrent: 20, UsageLimit: 100, UsagePercent: 0.2, LastRefresh: 21},
	)
	if err != nil {
		t.Fatalf("stale info publish: %v", err)
	}
	if applied {
		t.Fatal("older observation overwrote newer account-info state")
	}
	current, _ := GetAccountByID("acct")
	if current.UsageCurrent != 80 || current.LastRefresh != 20 {
		t.Fatalf("newer info was overwritten: %+v", current)
	}
}

func TestCredentialObservedBanRejectsCredentialAdvance(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := Init(configPath); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "credential-fence",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    100,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, ok := GetAccountByID("credential-fence")
	if !ok {
		t.Fatal("missing account")
	}
	applied, err := UpdateAccountCredentialStateIfLifecycle(
		observed.ID,
		observed.DispatchGeneration,
		"access-2",
		"refresh-2",
		200,
		"",
	)
	if err != nil || !applied {
		t.Fatalf("credential advance: applied=%t err=%v", applied, err)
	}

	banned, err := SetAccountBanStatusIfCredentialObserved(observed, "BANNED", "old refresh failed")
	if err != nil {
		t.Fatalf("stale credential ban: %v", err)
	}
	if banned {
		t.Fatal("stale credential failure banned the refreshed credential")
	}
	current, _ := GetAccountByID(observed.ID)
	if !current.Enabled || current.AccessToken != "access-2" || current.RefreshToken != "refresh-2" {
		t.Fatalf("stale credential failure contaminated current account: %+v", current)
	}
}

func TestAccountInfoObservedPublicationRejectsIdenticalNewerInfoInSameSecond(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "identical-info",
		AuthMethod:   "social",
		AccessToken:  "access",
		ProfileArn:   "arn:old",
		ExpiresAt:    2000,
		UsageCurrent: 10,
		UsageLimit:   100,
		UsagePercent: 0.1,
		LastRefresh:  20,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, ok := GetAccountByID("identical-info")
	if !ok {
		t.Fatal("missing observed account")
	}
	if observed.AccountInfoRevision != 0 {
		t.Fatalf("initial runtime revision = %d, want 0", observed.AccountInfoRevision)
	}

	// This publication is intentionally byte-for-byte identical to the observed
	// account-info projection and uses the same LastRefresh Unix second. Only the
	// process-local revision can prove that it happened after the old operation
	// started.
	applied, err := UpdateAccountInfoIfLifecycle(observed.ID, observed.DispatchGeneration, AccountInfo{
		UsageCurrent: 10,
		UsageLimit:   100,
		UsagePercent: 0.1,
		LastRefresh:  20,
	})
	if err != nil || !applied {
		t.Fatalf("identical newer publication: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.AccountInfoRevision == observed.AccountInfoRevision {
		t.Fatalf("identical publication did not advance runtime revision: old=%d new=%d", observed.AccountInfoRevision, current.AccountInfoRevision)
	}

	applied, err = UpdateAccountInfoIfObserved(observed, AccountInfo{
		UsageCurrent: 99,
		UsageLimit:   100,
		UsagePercent: 0.99,
		LastRefresh:  21,
	})
	if err != nil {
		t.Fatalf("stale observed publication: %v", err)
	}
	if applied {
		t.Fatal("old operation overwrote an identical-but-newer publication")
	}
	current, _ = GetAccountByID(observed.ID)
	if current.UsageCurrent != 10 || current.LastRefresh != 20 {
		t.Fatalf("identical newer publication was overwritten: %+v", current)
	}
}

func TestAccountInfoObservedPublicationRejectsProfileAdvance(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:          "profile-fence",
		AuthMethod:  "social",
		AccessToken: "access",
		ProfileArn:  "arn:old",
		ExpiresAt:   2000,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, _ := GetAccountByID("profile-fence")
	applied, err := UpdateAccountProfileArnIfLifecycle(observed.ID, observed.DispatchGeneration, "arn:new")
	if err != nil || !applied {
		t.Fatalf("profile advance: applied=%t err=%v", applied, err)
	}
	if AccountInfoObservationMatches(observed) {
		t.Fatal("old profile observation remained current after profile advance")
	}
}

func TestAccountInfoRevisionDoesNotCrossDeletedLifecycle(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{ID: "reused", AuthMethod: "social", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	first, _ := GetAccountByID("reused")
	if applied, err := UpdateAccountInfoIfLifecycle(first.ID, first.DispatchGeneration, AccountInfo{LastRefresh: 10}); err != nil || !applied {
		t.Fatalf("first info publication: applied=%t err=%v", applied, err)
	}
	firstCurrent, _ := GetAccountByID("reused")
	if firstCurrent.AccountInfoRevision == 0 {
		t.Fatal("first lifecycle did not receive an account-info revision")
	}
	if err := DeleteAccount("reused"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if err := AddAccount(Account{ID: "reused", AuthMethod: "social", Enabled: true}); err != nil {
		t.Fatalf("recreate account: %v", err)
	}
	second, _ := GetAccountByID("reused")
	if second.DispatchGeneration == first.DispatchGeneration {
		t.Fatalf("recreated lifecycle reused generation %d", second.DispatchGeneration)
	}
	if second.AccountInfoRevision != 0 {
		t.Fatalf("recreated lifecycle inherited account-info revision %d", second.AccountInfoRevision)
	}
}

func TestObservedHealthPublicationRejectsConcurrentDisable(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "disable-fence",
		AuthMethod:   "social",
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    2000,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, _ := GetAccountByID("disable-fence")
	if err := SetAccountEnabled(observed.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	if AccountInfoObservationMatches(observed) {
		t.Fatal("account-info observation remained current after operator disable")
	}
	if AccountCredentialObservationMatches(observed) {
		t.Fatal("credential observation remained current after operator disable")
	}
	if applied, err := SetAccountBanStatusIfObserved(observed, "BANNED", "stale info failure"); err != nil || applied {
		t.Fatalf("stale info failure overwrote operator disable: applied=%t err=%v", applied, err)
	}
	if applied, err := SetAccountBanStatusIfCredentialObserved(observed, "BANNED", "stale credential failure"); err != nil || applied {
		t.Fatalf("stale credential failure overwrote operator disable: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.Enabled || current.BanStatus != "DISABLED" {
		t.Fatalf("operator disable was overwritten: %+v", current)
	}
}

func TestAccountInfoObservationRejectsRefreshTokenOnlyAdvance(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "refresh-token-fence",
		AuthMethod:   "social",
		AccessToken:  "same-access",
		RefreshToken: "refresh-1",
		ExpiresAt:    2000,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, _ := GetAccountByID("refresh-token-fence")
	applied, err := UpdateAccountCredentialStateIfLifecycle(
		observed.ID,
		observed.DispatchGeneration,
		observed.AccessToken,
		"refresh-2",
		observed.ExpiresAt,
		"",
	)
	if err != nil || !applied {
		t.Fatalf("refresh-token-only advance: applied=%t err=%v", applied, err)
	}
	if AccountInfoObservationMatches(observed) {
		t.Fatal("old account-info operation ignored refresh-token-only credential progress")
	}
}

func TestCredentialPublicationObservationAllowsHealthAdvance(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "credential-health-independent",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    100,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, _ := GetAccountByID("credential-health-independent")
	if applied, err := SetAccountBanStatusIfLifecycle(
		observed.ID,
		observed.DispatchGeneration,
		"BANNED",
		"concurrent health result",
	); err != nil || !applied {
		t.Fatalf("health advance: applied=%t err=%v", applied, err)
	}

	applied, err := UpdateAccountCredentialStateIfObserved(
		observed,
		"access-2",
		"refresh-2",
		200,
		"",
	)
	if err != nil || !applied {
		t.Fatalf("credential publication after health advance: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.AccessToken != "access-2" || current.RefreshToken != "refresh-2" || current.ExpiresAt != 200 {
		t.Fatalf("rotated credential was not published: %+v", current)
	}
	if current.Enabled || current.BanStatus != "BANNED" || current.BanReason != "concurrent health result" {
		t.Fatalf("credential publication overwrote concurrent health state: %+v", current)
	}
}

func TestCredentialPublicationObservationPreservesNewerProfile(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "credential-profile-merge",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    100,
		ProfileArn:   "arn:profile:observed",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, _ := GetAccountByID("credential-profile-merge")
	if applied, err := UpdateAccountProfileArnIfLifecycle(
		observed.ID,
		observed.DispatchGeneration,
		"arn:profile:newer",
	); err != nil || !applied {
		t.Fatalf("profile advance: applied=%t err=%v", applied, err)
	}

	applied, err := UpdateAccountCredentialStateIfObserved(
		observed,
		"access-2",
		"refresh-2",
		200,
		"arn:profile:oauth-response",
	)
	if err != nil || !applied {
		t.Fatalf("credential publication: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.AccessToken != "access-2" || current.RefreshToken != "refresh-2" {
		t.Fatalf("credential publication did not advance credential: %+v", current)
	}
	if current.ProfileArn != "arn:profile:newer" {
		t.Fatalf("stale OAuth profile overwrote newer profile: %+v", current)
	}
}

func TestAccountInfoRecoveryPublicationClearsOnlyExistingBanState(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:          "manual-recovery",
		AuthMethod:  "social",
		AccessToken: "access",
		ExpiresAt:   2000,
		Enabled:     false,
		BanStatus:   "BANNED",
		BanReason:   "old reason",
		BanTime:     123,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, _ := GetAccountByID("manual-recovery")
	applied, err := UpdateAccountInfoAndRecoverIfObserved(observed, AccountInfo{
		UsageCurrent: 25,
		UsageLimit:   100,
		UsagePercent: 0.25,
		LastRefresh:  1000,
	})
	if err != nil || !applied {
		t.Fatalf("recovery publication: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.Enabled {
		t.Fatal("successful recovery must not implicitly re-enable an account")
	}
	if current.BanStatus != "ACTIVE" || current.BanReason != "" || current.BanTime != 0 {
		t.Fatalf("legacy recovery fields not cleared: %+v", current)
	}
}

func addFailureDrivenRecoveryAccount(t *testing.T, id string) Account {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{
		ID:           id,
		AuthMethod:   "social",
		AccessToken:  "access-2",
		RefreshToken: "refresh-2",
		ExpiresAt:    2000,
		Enabled:      false,
		BanStatus:    "DISABLED",
		BanReason:    "authentication failed",
		BanTime:      123,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	observed, ok := GetAccountByID(id)
	if !ok || observed.DispatchGeneration == 0 {
		t.Fatalf("GetAccountByID: ok=%t account=%+v", ok, observed)
	}
	return observed
}

func TestRecoverDisabledAccountIfCredentialObservedPublishesExactHealthyState(t *testing.T) {
	observed := addFailureDrivenRecoveryAccount(t, "recover")
	applied, err := RecoverDisabledAccountIfCredentialObserved(observed)
	if err != nil || !applied {
		t.Fatalf("recover: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if !current.Enabled || current.BanStatus != "ACTIVE" || current.BanReason != "" || current.BanTime != 0 {
		t.Fatalf("recovered state is not canonical healthy state: %+v", current)
	}
	if current.AccessToken != observed.AccessToken || current.RefreshToken != observed.RefreshToken || current.ExpiresAt != observed.ExpiresAt {
		t.Fatalf("recovery changed validated credential: %+v", current)
	}
}

func TestRecoverDisabledAccountIfCredentialObservedRejectsCredentialAdvance(t *testing.T) {
	observed := addFailureDrivenRecoveryAccount(t, "credential-advance")
	if applied, err := UpdateAccountCredentialStateIfLifecycle(
		observed.ID,
		observed.DispatchGeneration,
		"access-3",
		"refresh-3",
		3000,
		"",
	); err != nil || !applied {
		t.Fatalf("credential advance: applied=%t err=%v", applied, err)
	}

	applied, err := RecoverDisabledAccountIfCredentialObserved(observed)
	if err != nil || applied {
		t.Fatalf("stale recovery: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.Enabled || current.AccessToken != "access-3" || current.BanStatus != "DISABLED" {
		t.Fatalf("stale recovery changed newer credential/state: %+v", current)
	}
}

func TestRecoverDisabledAccountIfCredentialObservedRejectsHealthAdvance(t *testing.T) {
	observed := addFailureDrivenRecoveryAccount(t, "health-advance")
	if applied, err := SetAccountBanStatusIfLifecycle(
		observed.ID,
		observed.DispatchGeneration,
		"BANNED",
		"invalid grant",
	); err != nil || !applied {
		t.Fatalf("ban account: applied=%t err=%v", applied, err)
	}

	applied, err := RecoverDisabledAccountIfCredentialObserved(observed)
	if err != nil || applied {
		t.Fatalf("stale recovery: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.Enabled || current.BanStatus != "BANNED" || current.BanReason != "invalid grant" {
		t.Fatalf("stale recovery overwrote terminal health: %+v", current)
	}
}

func TestRecoverDisabledAccountIfCredentialObservedRejectsRecreatedLifecycle(t *testing.T) {
	observed := addFailureDrivenRecoveryAccount(t, "recreated")
	if _, applied, err := DeleteAccountIfLifecycle(observed.ID, observed.DispatchGeneration); err != nil || !applied {
		t.Fatalf("delete old lifecycle: applied=%t err=%v", applied, err)
	}
	if err := AddAccount(Account{
		ID:           observed.ID,
		AuthMethod:   "social",
		AccessToken:  "replacement-access",
		RefreshToken: "replacement-refresh",
		ExpiresAt:    4000,
		Enabled:      false,
		BanStatus:    "DISABLED",
		BanReason:    "replacement reason",
		BanTime:      456,
	}); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	applied, err := RecoverDisabledAccountIfCredentialObserved(observed)
	if err != nil || applied {
		t.Fatalf("old lifecycle recovery: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if current.Enabled || current.AccessToken != "replacement-access" || current.BanReason != "replacement reason" {
		t.Fatalf("old lifecycle recovery mutated replacement: %+v", current)
	}
}

func TestRecoverDisabledAccountIfCredentialObservedPublishedDurabilityErrorIsCanonical(t *testing.T) {
	observed := addFailureDrivenRecoveryAccount(t, "durability")
	previousSync := syncConfigDirectoryForWrite
	syncConfigDirectoryForWrite = func(string) error { return errors.New("directory sync failed") }
	t.Cleanup(func() { syncConfigDirectoryForWrite = previousSync })

	applied, err := RecoverDisabledAccountIfCredentialObserved(observed)
	if !applied || err == nil || !configFileWasReplaced(err) {
		t.Fatalf("durability publication: applied=%t err=%v", applied, err)
	}
	current, _ := GetAccountByID(observed.ID)
	if !current.Enabled || current.BanStatus != "ACTIVE" || current.BanReason != "" || current.BanTime != 0 {
		t.Fatalf("published recovered state was not adopted after durability error: %+v", current)
	}
}

func TestRecoverDisabledAccountIfCredentialObservedRejectsManualOrStaticCredentials(t *testing.T) {
	tests := []Account{
		{
			ID: "manual", AuthMethod: "social", AccessToken: "a", RefreshToken: "r", ExpiresAt: 1,
			Enabled: false, BanStatus: "DISABLED", BanReason: "",
		},
		{
			ID: "static", AuthMethod: "api_key", KiroApiKey: "ksk-test",
			Enabled: false, BanStatus: "DISABLED", BanReason: "authentication failed",
		},
	}
	for _, account := range tests {
		t.Run(account.ID, func(t *testing.T) {
			if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := AddAccount(account); err != nil {
				t.Fatalf("AddAccount: %v", err)
			}
			observed, _ := GetAccountByID(account.ID)
			applied, err := RecoverDisabledAccountIfCredentialObserved(observed)
			if err != nil || applied {
				t.Fatalf("ineligible recovery: applied=%t err=%v", applied, err)
			}
		})
	}
}
