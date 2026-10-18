package proxy

import (
	"io"
	"net/http"
	"testing"
)

const kiroAPIKeyRegionTestProxyURL = "test://kiro-apikey-region-proxy"

func installKiroAPIKeyRegionTestClient(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	oldClient, hadOldClient := proxyClientCache.Load(kiroAPIKeyRegionTestProxyURL)
	proxyClientCache.Store(kiroAPIKeyRegionTestProxyURL, &http.Client{Transport: transport})
	t.Cleanup(func() {
		if hadOldClient {
			proxyClientCache.Store(kiroAPIKeyRegionTestProxyURL, oldClient)
		} else {
			proxyClientCache.Delete(kiroAPIKeyRegionTestProxyURL)
		}
	})
}

func newKiroAPIKeyRegionTestPayload() *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "test"
	return payload
}

func kiroAPIKeyRegionTestResponse(body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(body),
		Header:     make(http.Header),
	}
}
