package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"kiro-go/config"
)

type failingBedrockResponseBody struct{}

func (failingBedrockResponseBody) Read([]byte) (int, error) {
	return 0, errors.New("injected body-read failure")
}
func (failingBedrockResponseBody) Close() error { return nil }

func TestBedrockForbiddenReadErrorIsNotPermissionEvidence(t *testing.T) {
	previous := bedrockHTTPClientFor
	t.Cleanup(func() { bedrockHTTPClientFor = previous })
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: failingBedrockResponseBody{}, Request: req}, nil
		})}
	}
	profiles, restricted, err := (&Handler{}).listBedrockInferenceProfilesContext(context.Background(), phase66F2BedrockAccount("forbidden-body-error"))
	if err == nil || !strings.Contains(err.Error(), "injected body-read failure") || restricted || len(profiles) != 0 {
		t.Fatalf("profiles=%v restricted=%t error=%v", profiles, restricted, err)
	}
}

func TestBedrockForbiddenStatusRemainsPermissionLimited(t *testing.T) {
	previous := bedrockHTTPClientFor
	t.Cleanup(func() { bedrockHTTPClientFor = previous })
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: phase66F2BedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"denied"}`)), Request: req}, nil
		})}
	}
	_, restricted, err := (&Handler{}).listBedrockInferenceProfilesContext(context.Background(), phase66F2BedrockAccount("forbidden-permission-limited"))
	if err != nil || !restricted {
		t.Fatalf("restricted=%t error=%v", restricted, err)
	}
}
