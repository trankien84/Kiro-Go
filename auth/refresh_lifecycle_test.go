package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

func TestRefreshTokenRejectsResultFromRecreatedLifecycle(t *testing.T) {
	origSocialURL := socialTokenURL
	defer func() { socialTokenURL = origSocialURL }()

	requestStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requestStarted) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "old-lifecycle-access-new",
			"refreshToken": "old-lifecycle-refresh-rotated",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acct",
		AuthMethod:   "social",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	old, ok := config.GetAccountByID("acct")
	if !ok || old.DispatchGeneration == 0 {
		t.Fatalf("old lifecycle not stamped: %#v", old)
	}

	errCh := make(chan error, 1)
	go func() {
		_, _, _, _, err := RefreshToken(&old)
		errCh <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh request did not reach test IdP")
	}

	if err := config.DeleteAccount("acct"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acct",
		AuthMethod:   "social",
		AccessToken:  "replacement-access",
		RefreshToken: "replacement-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	replacementBefore, ok := config.GetAccountByID("acct")
	if !ok || replacementBefore.DispatchGeneration == old.DispatchGeneration {
		t.Fatalf("replacement lifecycle did not advance: old=%d new=%d", old.DispatchGeneration, replacementBefore.DispatchGeneration)
	}

	close(release)
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrAccountLifecycleChanged) {
			t.Fatalf("refresh error = %v, want ErrAccountLifecycleChanged", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not return after IdP response")
	}

	replacementAfter, ok := config.GetAccountByID("acct")
	if !ok {
		t.Fatal("replacement account disappeared")
	}
	if replacementAfter.AccessToken != "replacement-access" || replacementAfter.RefreshToken != "replacement-refresh" {
		t.Fatalf("old lifecycle poisoned replacement credentials: %#v", replacementAfter)
	}
}

func TestRefreshTokenDifferentLifecyclesRefreshConcurrently(t *testing.T) {
	origSocialURL := socialTokenURL
	defer func() { socialTokenURL = origSocialURL }()

	var arrived int32
	bothArrived := make(chan struct{})
	var closeOnce sync.Once
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&arrived, 1) == 2 {
			closeOnce.Do(func() { close(bothArrived) })
		}
		var body struct {
			RefreshToken string `json:"refreshToken"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "fresh-access-" + body.RefreshToken,
			"refreshToken": "fresh-" + body.RefreshToken,
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := config.AddAccount(config.Account{
			ID:           id,
			AuthMethod:   "social",
			AccessToken:  "expired-" + id,
			RefreshToken: "refresh-" + id,
			ExpiresAt:    1,
			Enabled:      true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := config.GetAccountByID("a")
	b, _ := config.GetAccountByID("b")

	errCh := make(chan error, 2)
	go func() { _, _, _, _, err := RefreshToken(&a); errCh <- err }()
	go func() { _, _, _, _, err := RefreshToken(&b); errCh <- err }()

	select {
	case <-bothArrived:
		// Both upstream requests were simultaneously in flight. A process-global
		// refresh mutex would make this impossible.
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("different account lifecycles did not refresh concurrently; arrived=%d", atomic.LoadInt32(&arrived))
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRefreshTokenIfExpiringWithinCollapsesWithRequestRefresh(t *testing.T) {
	origSocialURL := socialTokenURL
	defer func() { socialTokenURL = origSocialURL }()

	requestStarted := make(chan struct{})
	release := make(chan struct{})
	var postCount int32
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&postCount, 1)
		once.Do(func() { close(requestStarted) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "fresh-access",
			"refreshToken": "fresh-refresh",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acct",
		AuthMethod:   "social",
		AccessToken:  "still-request-usable",
		RefreshToken: "refresh-old",
		ExpiresAt:    time.Now().Add(4 * time.Minute).Unix(),
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	background, _ := config.GetAccountByID("acct")
	request, _ := config.GetAccountByID("acct")

	backgroundErr := make(chan error, 1)
	go func() {
		_, _, _, _, err := RefreshTokenIfExpiringWithin(&background, 5*time.Minute)
		backgroundErr <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("proactive refresh did not honor the five-minute minimum validity")
	}

	requestErr := make(chan error, 1)
	go func() {
		_, _, _, _, err := RefreshToken(&request)
		requestErr <- err
	}()

	close(release)
	if err := <-backgroundErr; err != nil {
		t.Fatalf("proactive refresh: %v", err)
	}
	if err := <-requestErr; err != nil {
		t.Fatalf("request refresh: %v", err)
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("proactive + request refresh issued %d IdP POSTs, want exactly 1", got)
	}
	if request.AccessToken != "fresh-access" || request.RefreshToken != "fresh-refresh" {
		t.Fatalf("request caller did not adopt proactive winner: %#v", request)
	}
}

func TestRefreshTokenAllowsDetachedIDBearingAccountWithoutLifecycle(t *testing.T) {
	origSocialURL := socialTokenURL
	defer func() { socialTokenURL = origSocialURL }()

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "detached-access-new",
			"refreshToken": "detached-refresh-new",
			"expiresIn":    3600,
			"profileArn":   "arn:aws:codewhisperer:profile/detached",
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	account := config.Account{
		ID:           "detached-but-named",
		AuthMethod:   "social",
		AccessToken:  "detached-access-old",
		RefreshToken: "detached-refresh-old",
		ExpiresAt:    1,
	}
	if _, ok := config.GetAccountByID(account.ID); ok {
		t.Fatal("test account unexpectedly persisted before refresh")
	}

	access, refresh, _, profileArn, err := RefreshTokenForce(&account)
	if err != nil {
		t.Fatalf("detached refresh failed: %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("detached refresh issued %d IdP requests, want 1", got)
	}
	if access != "detached-access-new" || refresh != "detached-refresh-new" ||
		profileArn != "arn:aws:codewhisperer:profile/detached" {
		t.Fatalf("unexpected detached refresh result: access=%q refresh=%q profileArn=%q", access, refresh, profileArn)
	}
	if account.AccessToken != access || account.RefreshToken != refresh || account.ProfileArn != profileArn {
		t.Fatalf("caller-owned detached account not updated: %#v", account)
	}
	if _, ok := config.GetAccountByID(account.ID); ok {
		t.Fatal("detached refresh must not create/publish a config lifecycle")
	}
}

func TestRefreshTokenReturnsTypedPersistenceFailure(t *testing.T) {
	origSocialURL := socialTokenURL
	defer func() { socialTokenURL = origSocialURL }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "fresh-access",
			"refreshToken": "fresh-refresh",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	configPath := t.TempDir() + "/config.json"
	if err := config.Init(configPath); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "persist-failure",
		AuthMethod:   "social",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	account, ok := config.GetAccountByID("persist-failure")
	if !ok {
		t.Fatal("persisted test account missing")
	}

	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := RefreshTokenForce(&account)
	var persistenceErr *AccountCredentialPersistenceError
	if !errors.As(err, &persistenceErr) {
		t.Fatalf("refresh error = %v, want AccountCredentialPersistenceError", err)
	}
	if persistenceErr.AccountID != "persist-failure" || persistenceErr.Published || persistenceErr.Err == nil {
		t.Fatalf("unexpected typed persistence error: %#v", persistenceErr)
	}
	if account.AccessToken != "old-access" || account.RefreshToken != "old-refresh" {
		t.Fatalf("caller observed uncommitted credential: %#v", account)
	}
}

func TestRefreshTokenFailureReturnsCredentialActuallyAttemptedAfterLockWait(t *testing.T) {
	origSocialURL := socialTokenURL
	defer func() { socialTokenURL = origSocialURL }()

	requestRefreshToken := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refreshToken"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		requestRefreshToken <- body.RefreshToken
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acct-observation",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	caller, ok := config.GetAccountByID("acct-observation")
	if !ok {
		t.Fatal("test account missing")
	}

	release := acquireRefreshLock(refreshLockKey{id: caller.ID, generation: caller.DispatchGeneration})
	errCh := make(chan error, 1)
	go func() {
		_, _, _, _, err := RefreshTokenForce(&caller)
		errCh <- err
	}()

	// Wait until the refresh goroutine has actually joined the same lifecycle
	// lock. Without this barrier the test could pass merely because the goroutine
	// started after T2 was published instead of proving the lock-wait re-read.
	key := refreshLockKey{id: caller.ID, generation: caller.DispatchGeneration}
	waitDeadline := time.Now().Add(time.Second)
	for {
		refreshMu.Lock()
		entry := refreshLocks[key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		refreshMu.Unlock()
		if refs >= 2 {
			break
		}
		if time.Now().After(waitDeadline) {
			release()
			t.Fatal("refresh goroutine did not wait on lifecycle lock")
		}
		time.Sleep(time.Millisecond)
	}

	// The queued refresh was admitted with T1. While it waits for the lifecycle
	// lock, publish T2 without changing DispatchGeneration. The refresh must then
	// re-read T2 under the lock, send refresh-2, and expose T2 back to its caller
	// when that exact attempt fails so downstream failure fencing cannot compare
	// the 401/invalid_grant against stale T1.
	if applied, err := config.UpdateAccountCredentialStateIfLifecycle(
		caller.ID,
		caller.DispatchGeneration,
		"access-2",
		"refresh-2",
		1,
		caller.ProfileArn,
	); err != nil || !applied {
		release()
		t.Fatalf("publish T2: applied=%t err=%v", applied, err)
	}
	release()

	select {
	case got := <-requestRefreshToken:
		if got != "refresh-2" {
			t.Fatalf("IdP received refresh token %q, want refresh-2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh attempt did not reach test IdP")
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("refresh unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed refresh did not return")
	}
	if caller.AccessToken != "access-2" || caller.RefreshToken != "refresh-2" {
		t.Fatalf("caller retained stale T1 after T2 attempt failed: %#v", caller)
	}
}

func TestRefreshTokenRetriesPendingCredentialWithoutSecondIdPPost(t *testing.T) {
	origSocialURL := socialTokenURL
	origPublish := publishRefreshedCredentialState
	defer func() {
		socialTokenURL = origSocialURL
		publishRefreshedCredentialState = origPublish
	}()

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "access-2",
			"refreshToken": "refresh-2",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "pending-local-retry",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	account, _ := config.GetAccountByID("pending-local-retry")

	storageErr := errors.New("injected pre-publication failure")
	var publishCalls int32
	publishRefreshedCredentialState = func(
		observed config.Account,
		accessToken, refreshToken string,
		expiresAt int64,
		profileArn string,
	) (bool, error) {
		if atomic.AddInt32(&publishCalls, 1) == 1 {
			return false, storageErr
		}
		return origPublish(observed, accessToken, refreshToken, expiresAt, profileArn)
	}

	_, _, _, _, err := RefreshTokenForce(&account)
	var persistenceErr *AccountCredentialPersistenceError
	if !errors.As(err, &persistenceErr) || persistenceErr.Published {
		t.Fatalf("first refresh error=%v persistence=%#v, want pre-publication failure", err, persistenceErr)
	}
	if account.AccessToken != "access-1" || account.RefreshToken != "refresh-1" {
		t.Fatalf("caller adopted uncommitted C2: %+v", account)
	}
	canonical, _ := config.GetAccountByID(account.ID)
	if canonical.AccessToken != "access-1" || canonical.RefreshToken != "refresh-1" {
		t.Fatalf("canonical state changed before publication: %+v", canonical)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("first refresh IdP POSTs=%d, want 1", got)
	}

	// Even Force must finish the pending local publication instead of replaying R1.
	_, _, _, _, err = RefreshTokenForce(&account)
	if err != nil {
		t.Fatalf("pending local retry: %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("pending recovery replayed IdP: POSTs=%d, want 1", got)
	}
	if account.AccessToken != "access-2" || account.RefreshToken != "refresh-2" {
		t.Fatalf("caller did not adopt recovered C2: %+v", account)
	}
	canonical, _ = config.GetAccountByID(account.ID)
	if canonical.AccessToken != "access-2" || canonical.RefreshToken != "refresh-2" {
		t.Fatalf("canonical C2 not published: %+v", canonical)
	}
}

func TestRefreshTokenPublishedDurabilityErrorAdoptsCanonicalCredential(t *testing.T) {
	origSocialURL := socialTokenURL
	origPublish := publishRefreshedCredentialState
	defer func() {
		socialTokenURL = origSocialURL
		publishRefreshedCredentialState = origPublish
	}()

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "durable-access-2",
			"refreshToken": "durable-refresh-2",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "published-durability",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	account, _ := config.GetAccountByID("published-durability")

	durabilityErr := errors.New("injected directory sync failure")
	publishRefreshedCredentialState = func(
		observed config.Account,
		accessToken, refreshToken string,
		expiresAt int64,
		profileArn string,
	) (bool, error) {
		applied, err := origPublish(observed, accessToken, refreshToken, expiresAt, profileArn)
		if err != nil || !applied {
			return applied, err
		}
		return true, durabilityErr
	}

	_, _, _, _, err := RefreshTokenForce(&account)
	var persistenceErr *AccountCredentialPersistenceError
	if !errors.As(err, &persistenceErr) || !persistenceErr.Published {
		t.Fatalf("refresh error=%v persistence=%#v, want published durability error", err, persistenceErr)
	}
	if !errors.Is(err, durabilityErr) {
		t.Fatalf("durability cause lost: %v", err)
	}
	if account.AccessToken != "durable-access-2" || account.RefreshToken != "durable-refresh-2" {
		t.Fatalf("caller did not adopt published C2: %+v", account)
	}
	canonical, _ := config.GetAccountByID(account.ID)
	if canonical.AccessToken != account.AccessToken || canonical.RefreshToken != account.RefreshToken {
		t.Fatalf("caller/canonical split after published error: caller=%+v canonical=%+v", account, canonical)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("IdP POSTs=%d, want 1", got)
	}
}

func TestRefreshTokenConcurrentWaitersRecoverOnePendingCandidate(t *testing.T) {
	origSocialURL := socialTokenURL
	origPublish := publishRefreshedCredentialState
	defer func() {
		socialTokenURL = origSocialURL
		publishRefreshedCredentialState = origPublish
	}()

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "waiter-access-2",
			"refreshToken": "waiter-refresh-2",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "pending-waiters",
		AuthMethod:   "social",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	leader, _ := config.GetAccountByID("pending-waiters")

	var failOnce int32
	publishRefreshedCredentialState = func(
		observed config.Account,
		accessToken, refreshToken string,
		expiresAt int64,
		profileArn string,
	) (bool, error) {
		if atomic.CompareAndSwapInt32(&failOnce, 0, 1) {
			return false, errors.New("first local publication fails")
		}
		return origPublish(observed, accessToken, refreshToken, expiresAt, profileArn)
	}
	if _, _, _, _, err := RefreshToken(&leader); err == nil {
		t.Fatal("leader refresh unexpectedly persisted on first attempt")
	}

	const waiters = 8
	var wg sync.WaitGroup
	errCh := make(chan error, waiters)
	wg.Add(waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			defer wg.Done()
			caller, _ := config.GetAccountByID("pending-waiters")
			_, _, _, _, err := RefreshToken(&caller)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("waiter refresh: %v", err)
		}
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("concurrent recovery issued %d IdP POSTs, want 1", got)
	}
}

func TestRefreshTokenPendingCandidateDoesNotCrossRecreatedLifecycle(t *testing.T) {
	origSocialURL := socialTokenURL
	origPublish := publishRefreshedCredentialState
	defer func() {
		socialTokenURL = origSocialURL
		publishRefreshedCredentialState = origPublish
	}()

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "old-lifecycle-c2",
			"refreshToken": "old-lifecycle-r2",
			"expiresIn":    3600,
		})
	}))
	defer srv.Close()
	socialTokenURL = func() string { return srv.URL }

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "pending-recreate",
		AuthMethod:   "social",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    1,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	old, _ := config.GetAccountByID("pending-recreate")
	oldKey := refreshLockKey{id: old.ID, generation: old.DispatchGeneration}
	publishRefreshedCredentialState = func(
		observed config.Account,
		accessToken, refreshToken string,
		expiresAt int64,
		profileArn string,
	) (bool, error) {
		return false, errors.New("keep C2 pending")
	}
	if _, _, _, _, err := RefreshToken(&old); err == nil {
		t.Fatal("expected pre-publication persistence failure")
	}

	if err := config.DeleteAccount(old.ID); err != nil {
		t.Fatal(err)
	}
	if err := config.AddAccount(config.Account{
		ID:           old.ID,
		AuthMethod:   "social",
		AccessToken:  "replacement-access",
		RefreshToken: "replacement-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	publishRefreshedCredentialState = origPublish
	replacement, _ := config.GetAccountByID(old.ID)
	if _, _, _, _, err := RefreshToken(&replacement); err != nil {
		t.Fatalf("replacement refresh admission: %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("replacement unexpectedly replayed old/new IdP token: POSTs=%d", got)
	}
	current, _ := config.GetAccountByID(old.ID)
	if current.AccessToken != "replacement-access" || current.RefreshToken != "replacement-refresh" {
		t.Fatalf("old pending C2 crossed lifecycle: %+v", current)
	}
	refreshMu.Lock()
	_, oldEntryStillPresent := refreshLocks[oldKey]
	refreshMu.Unlock()
	if oldEntryStillPresent {
		t.Fatal("stale pending lifecycle was not pruned from refresh registry")
	}
}
