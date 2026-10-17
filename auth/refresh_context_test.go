package auth

import (
	"context"
	"encoding/json"
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshContextSemantics(t *testing.T) {
	const childEnv = "KIRO_GO_REFRESH_CONTEXT_CHILD"
	if !runBoundedTestSubprocess(t, childEnv, "^TestRefreshContextSemantics$", 20*time.Second, nil) {
		return
	}

	origURL := socialTokenURL
	origPublish := publishRefreshedCredentialState
	origTimeout := oauthRefreshAttemptTimeout
	defer func() {
		socialTokenURL = origURL
		publishRefreshedCredentialState = origPublish
		oauthRefreshAttemptTimeout = origTimeout
	}()

	addExpired := func(t *testing.T, id string) config.Account {
		t.Helper()
		if err := config.Init(t.TempDir() + "/config.json"); err != nil {
			t.Fatalf("config.Init: %v", err)
		}
		if err := config.AddAccount(config.Account{
			ID: id, AuthMethod: "social", AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: 1, Enabled: true,
		}); err != nil {
			t.Fatalf("config.AddAccount: %v", err)
		}
		account, ok := config.GetAccountByID(id)
		if !ok {
			t.Fatal("account not found")
		}
		return account
	}

	t.Run("canceled before dispatch makes no POST", func(t *testing.T) {
		var posts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&posts, 1)
		}))
		defer srv.Close()
		socialTokenURL = func() string { return srv.URL }
		account := addExpired(t, "ctx-before-dispatch")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, _, _, err := RefreshTokenContext(ctx, &account)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RefreshTokenContext error = %v, want context.Canceled", err)
		}
		if got := atomic.LoadInt32(&posts); got != 0 {
			t.Fatalf("POST count = %d, want 0", got)
		}
	})

	t.Run("cancel after dispatch still publishes rotated credential", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(started) })
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "access-c2", "refreshToken": "refresh-c2", "expiresIn": 3600})
		}))
		defer srv.Close()
		var releaseOnce sync.Once
		releaseServer := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseServer()
		socialTokenURL = func() string { return srv.URL }
		account := addExpired(t, "ctx-after-dispatch")
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { _, _, _, _, err := RefreshTokenContext(ctx, &account); errCh <- err }()
		waitRefreshContextSignal(t, started, "refresh POST dispatch")
		cancel()
		releaseServer()
		if err := waitRefreshContextError(t, errCh, "refresh completion after caller cancel"); err != nil {
			t.Fatalf("refresh after caller cancel = %v", err)
		}
		canonical, _ := config.GetAccountByID(account.ID)
		if canonical.RefreshToken != "refresh-c2" || canonical.AccessToken != "access-c2" {
			t.Fatalf("canonical credential = access:%q refresh:%q, want C2", canonical.AccessToken, canonical.RefreshToken)
		}
	})

	t.Run("canceled follower never replays refresh token", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var posts int32
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&posts, 1)
			once.Do(func() { close(started) })
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "access-winner", "refreshToken": "refresh-winner", "expiresIn": 3600})
		}))
		defer srv.Close()
		var releaseOnce sync.Once
		releaseServer := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseServer()
		socialTokenURL = func() string { return srv.URL }
		leader := addExpired(t, "ctx-follower")
		follower := leader.Clone()
		leaderErr := make(chan error, 1)
		go func() { _, _, _, _, err := RefreshTokenContext(context.Background(), &leader); leaderErr <- err }()
		waitRefreshContextSignal(t, started, "leader refresh POST dispatch")
		ctx, cancel := context.WithCancel(context.Background())
		followerErr := make(chan error, 1)
		go func() { _, _, _, _, err := RefreshTokenContext(ctx, &follower); followerErr <- err }()
		key := refreshLockKey{id: leader.ID, generation: leader.DispatchGeneration}
		waitRefreshContextCondition(t, "follower waiting on refresh gate", func() bool {
			refreshMu.Lock()
			defer refreshMu.Unlock()
			entry := refreshLocks[key]
			return entry != nil && entry.refs >= 2
		})
		cancel()
		// The follower must stop waiting while the leader is still blocked at the
		// IdP. Releasing the leader first would only prove zero replay, not
		// cancellation-aware lock acquisition.
		if err := waitRefreshContextError(t, followerErr, "canceled follower while leader remains blocked"); !errors.Is(err, context.Canceled) {
			t.Fatalf("follower error = %v, want context.Canceled", err)
		}
		if got := atomic.LoadInt32(&posts); got != 1 {
			t.Fatalf("POST count before leader release = %d, want 1", got)
		}
		releaseServer()
		if err := waitRefreshContextError(t, leaderErr, "leader refresh completion"); err != nil {
			t.Fatalf("leader refresh: %v", err)
		}
	})

	t.Run("pending C2 publishes even when caller is canceled", func(t *testing.T) {
		var posts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&posts, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "pending-access", "refreshToken": "pending-refresh", "expiresIn": 3600})
		}))
		defer srv.Close()
		socialTokenURL = func() string { return srv.URL }
		account := addExpired(t, "ctx-pending")
		storageErr := errors.New("injected pre-publication failure")
		publishRefreshedCredentialState = func(config.Account, string, string, int64, string) (bool, error) { return false, storageErr }
		_, _, _, _, err := RefreshTokenForce(&account)
		var persistenceErr *AccountCredentialPersistenceError
		if !errors.As(err, &persistenceErr) || persistenceErr.Published {
			t.Fatalf("first refresh error = %v, want pre-publication persistence error", err)
		}
		publishRefreshedCredentialState = origPublish
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, _, _, err = RefreshTokenContext(ctx, &account)
		if err != nil {
			t.Fatalf("pending publication under canceled caller: %v", err)
		}
		canonical, _ := config.GetAccountByID(account.ID)
		if canonical.RefreshToken != "pending-refresh" {
			t.Fatalf("pending C2 not published: refresh=%q", canonical.RefreshToken)
		}
		if got := atomic.LoadInt32(&posts); got != 1 {
			t.Fatalf("POST count = %d, want 1", got)
		}
	})

	t.Run("refresh POST does not follow redirect", func(t *testing.T) {
		var sourcePosts int32
		var redirectedPosts int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&redirectedPosts, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer target.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&sourcePosts, 1)
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer source.Close()
		socialTokenURL = func() string { return source.URL }
		account := addExpired(t, "ctx-no-redirect")
		_, _, _, _, err := RefreshTokenContext(context.Background(), &account)
		if err == nil {
			t.Fatal("expected redirect token response to fail")
		}
		if got := atomic.LoadInt32(&sourcePosts); got != 1 {
			t.Fatalf("source POST count = %d, want 1", got)
		}
		if got := atomic.LoadInt32(&redirectedPosts); got != 0 {
			t.Fatalf("redirect target POST count = %d, want 0", got)
		}
	})

	t.Run("internal attempt timeout bounds refresh", func(t *testing.T) {
		oauthRefreshAttemptTimeout = 40 * time.Millisecond
		requestStarted := make(chan struct{})
		previousClient := httpClientStore.Load()
		httpClientStore.Store(&http.Client{
			Transport: refreshContextRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(requestStarted)
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		})
		t.Cleanup(func() { httpClientStore.Store(previousClient) })

		socialTokenURL = func() string { return "https://refresh.invalid/token" }
		account := addExpired(t, "ctx-timeout")
		started := time.Now()
		_, _, _, _, err := RefreshTokenContext(context.Background(), &account)
		if err == nil {
			t.Fatal("expected internal attempt timeout")
		}
		if time.Since(started) > time.Second {
			t.Fatalf("internal attempt timeout took too long: %v", time.Since(started))
		}
		select {
		case <-requestStarted:
		default:
			t.Fatal("timeout test never dispatched request")
		}
	})
}

const refreshContextWaitTimeout = 2 * time.Second

type refreshContextRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn refreshContextRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func waitRefreshContextSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(refreshContextWaitTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitRefreshContextError(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(refreshContextWaitTimeout):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func waitRefreshContextCondition(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(refreshContextWaitTimeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
