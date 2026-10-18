package proxy

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/config"
	"testing"
	"time"
)

func TestManualRefreshDropsOldToken401AfterConcurrentCredentialAdvance(t *testing.T) {
	account := addReconcileOAuthAccount(t, time.Now().Add(-time.Hour).Unix())
	withAccountInfoFetchForTest(t, func(_ context.Context, observed *config.Account) (*config.AccountInfo, error) {
		if observed.AccessToken != "access-1" {
			t.Fatalf("quota request used unexpected credential: %+v", observed)
		}
		applied, err := config.UpdateAccountCredentialStateIfLifecycle(
			account.ID,
			account.DispatchGeneration,
			"access-2",
			"refresh-2",
			time.Now().Add(2*time.Hour).Unix(),
			observed.ProfileArn,
		)
		if err != nil || !applied {
			t.Fatalf("publish concurrent T2: applied=%t err=%v", applied, err)
		}
		return nil, fmt.Errorf("HTTP 401: late T1 unauthorized")
	})

	_, err := (&Handler{}).refreshAccountByKind(context.Background(), &account, accountRefreshExplicit)
	if !errors.Is(err, errStaleAccountSelection) {
		t.Fatalf("manual stale T1 result=%v, want stale selection", err)
	}
	current, _ := config.GetAccountByID(account.ID)
	if !current.Enabled || current.BanStatus == "BANNED" || current.AccessToken != "access-2" || current.RefreshToken != "refresh-2" {
		t.Fatalf("late T1 401 contaminated T2: %+v", current)
	}
}

func TestManualRefreshDoesNotOverwriteNewerAccountInfoPublication(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	withAccountInfoFetchForTest(t, func(_ context.Context, observed *config.Account) (*config.AccountInfo, error) {
		applied, err := config.UpdateAccountInfoIfObserved(observed.Clone(), config.AccountInfo{
			UsageCurrent: 80,
			UsageLimit:   100,
			UsagePercent: 0.8,
			LastRefresh:  20,
		})
		if err != nil || !applied {
			t.Fatalf("publish newer info B: applied=%t err=%v", applied, err)
		}
		return &config.AccountInfo{
			UsageCurrent: 20,
			UsageLimit:   100,
			UsagePercent: 0.2,
			LastRefresh:  30,
		}, nil
	})

	_, err := (&Handler{}).refreshAccountByKind(context.Background(), &account, accountRefreshExplicit)
	if !errors.Is(err, errStaleAccountSelection) {
		t.Fatalf("late manual info A result=%v, want stale selection", err)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.UsageCurrent != 80 || current.LastRefresh != 20 {
		t.Fatalf("late manual A overwrote newer B: %+v", current)
	}
}

func TestManualRefreshRecoveryClearsBanWithoutReenabling(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	if applied, err := config.SetAccountBanStatusIfLifecycle(
		account.ID,
		account.DispatchGeneration,
		"BANNED",
		"old auth failure",
	); err != nil || !applied {
		t.Fatalf("seed ban: applied=%t err=%v", applied, err)
	}
	account, _ = config.GetAccountByID(account.ID)
	if account.Enabled {
		t.Fatal("seed ban did not disable account")
	}

	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		return &config.AccountInfo{
			UsageCurrent: 25,
			UsageLimit:   100,
			UsagePercent: 0.25,
			LastRefresh:  50,
		}, nil
	})

	outcome, err := (&Handler{}).refreshAccountByKind(context.Background(), &account, accountRefreshExplicit)
	if err != nil {
		t.Fatalf("manual recovery: %v", err)
	}
	if outcome == nil || outcome.Info == nil {
		t.Fatalf("manual recovery outcome=%#v", outcome)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled {
		t.Fatal("manual recovery implicitly re-enabled account")
	}
	if current.BanStatus != "ACTIVE" || current.BanReason != "" || current.BanTime != 0 {
		t.Fatalf("manual recovery did not clear legacy ban fields: %+v", current)
	}
}

func TestManualFreshCredentialInfo401DoesNotForceOAuthRotation(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	originalAccess := account.AccessToken
	originalRefresh := account.RefreshToken
	withAccountInfoFetchForTest(t, func(_ context.Context, observed *config.Account) (*config.AccountInfo, error) {
		if observed.AccessToken != originalAccess || observed.RefreshToken != originalRefresh {
			t.Fatalf("fresh credential rotated before quota probe: %+v", observed)
		}
		return nil, fmt.Errorf("HTTP 401: unauthorized")
	})

	_, err := (&Handler{}).refreshAccountByKind(context.Background(), &account, accountRefreshExplicit)
	if err == nil {
		t.Fatal("manual 401 unexpectedly reported success")
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.AccessToken != originalAccess || current.RefreshToken != originalRefresh {
		t.Fatalf("manual info failure force-rotated a fresh credential: %+v", current)
	}
}

func TestManualSuspensionPublicationPreservesHistoricalSuccessResponse(t *testing.T) {
	account := addReconcileOAuthAccount(t, 10)
	withAccountInfoFetchForTest(t, func(context.Context, *config.Account) (*config.AccountInfo, error) {
		return nil, errors.New("TEMPORARILY_SUSPENDED")
	})

	outcome, err := (&Handler{}).refreshAccountByKind(context.Background(), &account, accountRefreshExplicit)
	if err != nil {
		t.Fatalf("manual suspension refresh: %v", err)
	}
	if outcome == nil || outcome.Message != "Account status updated" {
		t.Fatalf("manual suspension outcome=%#v", outcome)
	}
	current, _ := config.GetAccountByID(account.ID)
	if current.Enabled || current.BanStatus != "BANNED" {
		t.Fatalf("suspension publication did not disable account: %+v", current)
	}
}
