package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/pool"
)

func TestListAvailableModelsContextHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ListAvailableModelsContext(ctx, &config.Account{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListAvailableModelsContext error = %v, want context.Canceled", err)
	}
}

func TestResolveProfileArnContextHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ResolveProfileArnContext(ctx, &config.Account{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveProfileArnContext error = %v, want context.Canceled", err)
	}
}

func TestResolveProfileArnAcrossRegionsContextHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := resolveProfileArnAcrossRegionsContext(ctx, &config.Account{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveProfileArnAcrossRegionsContext error = %v, want context.Canceled", err)
	}
}

func TestEnsureValidTokenForSelectionContextHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := (&Handler{}).ensureValidTokenForSelectionContext(ctx, &pool.AccountSelection{
		Account: config.Account{
			AuthMethod:   "social",
			AccessToken:  "access-c1",
			RefreshToken: "refresh-c1",
			ExpiresAt:    1,
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ensureValidTokenForSelectionContext error = %v, want context.Canceled", err)
	}
}

func TestEnsureValidTokenContextCancellationBeforeRefreshDispatchMakesNoPOST(t *testing.T) {
	const (
		childEnv  = "KIRO_GO_ENSURE_VALID_TOKEN_CONTEXT_CHILD"
		accountID = "ensure-valid-token-context-cancel"
	)
	if os.Getenv(childEnv) != "1" {
		watchdogCtx, watchdogCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer watchdogCancel()

		cmd := exec.CommandContext(
			watchdogCtx,
			os.Args[0],
			"-test.run=^TestEnsureValidTokenContextCancellationBeforeRefreshDispatchMakesNoPOST$",
			"-test.timeout=10s",
			"-test.v",
		)
		cmd.Env = append(os.Environ(), childEnv+"=1")
		cmd.WaitDelay = 2 * time.Second
		output, err := cmd.CombinedOutput()
		if err != nil {
			if watchdogCtx.Err() != nil {
				t.Fatalf("isolated ensure-valid-token context test exceeded outer watchdog: %v\n%s", watchdogCtx.Err(), output)
			}
			t.Fatalf("isolated ensure-valid-token context test failed: %v\n%s", err, output)
		}
		return
	}

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-c2",
			"refresh_token": "refresh-c2",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	validatorEntered := make(chan struct{})
	releaseValidator := make(chan struct{})
	var enterOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseValidator) }) }
	defer release()

	previousValidator := auth.SetExternalIdpValidatorForTest(func(string) error {
		enterOnce.Do(func() { close(validatorEntered) })
		<-releaseValidator
		return nil
	})
	defer auth.SetExternalIdpValidatorForTest(previousValidator)

	if err := config.AddAccount(config.Account{
		ID:            accountID,
		Email:         "ensure-valid-token-context-cancel@example.invalid",
		AuthMethod:    "external_idp",
		Provider:      "generic-oidc",
		AccessToken:   "access-c1",
		RefreshToken:  "refresh-c1",
		ClientID:      "client-id",
		TokenEndpoint: srv.URL,
		Scopes:        "openid offline_access",
		Region:        "us-east-1",
		ExpiresAt:     1,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	defer func() {
		if err := config.DeleteAccount(accountID); err != nil {
			t.Errorf("config.DeleteAccount cleanup: %v", err)
		}
	}()

	account, ok := config.GetAccountByID(accountID)
	if !ok {
		t.Fatal("account not found")
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Handler{}).ensureValidTokenContext(ctx, &account)
	}()

	select {
	case <-validatorEntered:
	case err := <-errCh:
		t.Fatalf("token refresh returned before pre-dispatch validation gate: %v", err)
	case <-time.After(time.Second):
		t.Fatal("token refresh did not reach pre-dispatch validation gate")
	}

	cancel()
	release()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ensureValidTokenContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled token refresh did not return promptly")
	}
	if got := atomic.LoadInt32(&posts); got != 0 {
		t.Fatalf("token endpoint POST count = %d, want 0", got)
	}
}
