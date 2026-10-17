package proxy

import (
	"context"
	"errors"
	"io"
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reconcileInfoRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn reconcileInfoRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestAccountInfoReconcileContextCancelsOutgoingUsageRequest(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	requestStarted := make(chan struct{})
	client := &http.Client{Transport: reconcileInfoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(requestStarted)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	previousRestClient := kiroRestHttpStore.Load()
	kiroRestHttpStore.Store(client)
	t.Cleanup(func() { kiroRestHttpStore.Store(previousRestClient) })

	account := &config.Account{
		AuthMethod:  "api_key",
		AccessToken: "ksk_test",
		KiroApiKey:  "ksk_test",
		Region:      "us-east-1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := getUsageLimitsForReconcileContext(ctx, account)
		errCh <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("usage request did not reach test transport")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("usage request error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled usage request did not return promptly")
	}
}

func TestAccountInfoReconcileParsesUsageWithoutPublishing(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acct",
		AuthMethod:   "api_key",
		AccessToken:  "ksk_test",
		KiroApiKey:   "ksk_test",
		Region:       "us-east-1",
		UsageCurrent: 3,
		UsageLimit:   10,
		LastRefresh:  10,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	account, _ := config.GetAccountByID("acct")

	client := &http.Client{Transport: reconcileInfoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"usageBreakdownList":[{"currentUsage":8,"usageLimit":10}],
				"subscriptionInfo":{"subscriptionType":"PRO","subscriptionTitle":"Kiro Pro"},
				"userInfo":{"email":"new@example.invalid","userId":"u1"}
			}`)),
		}, nil
	})}
	previousRestClient := kiroRestHttpStore.Load()
	kiroRestHttpStore.Store(client)
	t.Cleanup(func() { kiroRestHttpStore.Store(previousRestClient) })

	info, err := fetchAccountInfoForReconcileContext(context.Background(), &account)
	if err != nil {
		t.Fatalf("fetch account info: %v", err)
	}
	if info.UsageCurrent != 8 || info.UsageLimit != 10 || info.SubscriptionType != "PRO" {
		t.Fatalf("parsed info = %+v", info)
	}
	canonical, _ := config.GetAccountByID("acct")
	if canonical.UsageCurrent != 3 || canonical.LastRefresh != 10 {
		t.Fatalf("fetch-only helper published account info: %+v", canonical)
	}
}

func TestAccountInfoErrorBodyIsBounded(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", 10<<20))
	got := readBoundedAccountInfoErrorBody(body)
	if !strings.HasSuffix(got, " ... [truncated]") {
		suffix := got
		if len(suffix) > 32 {
			suffix = suffix[len(suffix)-32:]
		}
		t.Fatalf("large error body missing truncation marker: suffix=%q", suffix)
	}
	if len(got) > int(maxAccountInfoErrorBodyBytes)+len(" ... [truncated]") {
		t.Fatalf("bounded body length=%d exceeds cap=%d", len(got), maxAccountInfoErrorBodyBytes)
	}
}

func TestAccountInfoSuccessBodyValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "normal", body: `{"usageBreakdownList":[]}`},
		{name: "unknown additive field", body: `{"usageBreakdownList":[],"futureField":{"enabled":true}}`},
		{name: "second JSON value", body: `{"usageBreakdownList":[]} {"unexpected":true}`, wantErr: true},
		{name: "trailing garbage", body: `{"usageBreakdownList":[]} trailing`, wantErr: true},
		{name: "malformed", body: `{"usageBreakdownList":[}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: reconcileInfoRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			previousRestClient := kiroRestHttpStore.Load()
			kiroRestHttpStore.Store(client)
			t.Cleanup(func() { kiroRestHttpStore.Store(previousRestClient) })
			account := &config.Account{AuthMethod: "api_key", AccessToken: "ksk_test", KiroApiKey: "ksk_test", Region: "us-east-1"}
			_, err := getUsageLimitsForReconcileContext(context.Background(), account)
			if tc.wantErr && err == nil {
				t.Fatal("expected invalid success body to be rejected")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid success body rejected: %v", err)
			}
		})
	}
}

func TestAccountInfoSuccessBodyIsBounded(t *testing.T) {
	body := strings.NewReader(strings.Repeat(" ", int(maxAccountInfoSuccessBodyBytes)+1))
	_, err := readBoundedAccountInfoSuccessBody(body)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized success body error = %v, want size-limit error", err)
	}
}
