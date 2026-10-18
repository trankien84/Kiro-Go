package auth

import (
	"context"
	"io"
	"net/http"
	"time"
)

var oauthRefreshAttemptTimeout = 30 * time.Second

type refreshAttemptTransport struct {
	parent context.Context
	base   http.RoundTripper
}

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

func (t refreshAttemptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.parent.Err(); err != nil {
		return nil, err
	}
	// This is the refresh-token commit point. Everything before RoundTrip remains
	// caller-cancelable. Once transport dispatch starts, detach caller cancellation
	// so a rotated refresh token cannot be lost after the IdP accepts the POST.
	attemptCtx, cancel := beginOAuthRefreshAttempt(t.parent)
	resp, err := t.base.RoundTrip(req.Clone(attemptCtx))
	if err != nil {
		cancel()
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		cancel()
		return resp, nil
	}
	resp.Body = &cancelOnCloseReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func bindRefreshAttemptClient(ctx context.Context, client *http.Client) *http.Client {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		client = http.DefaultClient
	}
	bound := *client
	base := bound.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	bound.Transport = refreshAttemptTransport{parent: ctx, base: base}
	// Never forward a refresh-token POST across redirects. Returning the 3xx
	// response lets the token-endpoint parser classify it without exposing C1 to
	// a second origin/path.
	bound.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &bound
}

func beginOAuthRefreshAttempt(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	timeout := oauthRefreshAttemptTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}
