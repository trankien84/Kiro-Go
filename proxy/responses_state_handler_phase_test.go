package proxy

import (
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesNonStreamPersistenceFailureDoesNotReportCompleted(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "generated successfully upstream",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	oldSave := saveResponseForCompletion
	saveResponseForCompletion = func(*ResponsesObject) error { return errors.New("forced local persistence failure") }
	t.Cleanup(func() { saveResponseForCompletion = oldSave })

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"persist this",
		"store":true
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 after local persistence failure", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"status":"completed"`) || strings.Contains(body, `"object":"response"`) {
		t.Fatalf("persistence failure was exposed as a completed Responses object: %s", body)
	}
	if !strings.Contains(body, responsesStatePersistenceFailureMessage) {
		t.Fatalf("local persistence failure message missing: %s", body)
	}
	h.pool.WaitForPendingStats()
	account, ok := config.GetAccountByID("test-account")
	if !ok || !account.Enabled || account.ErrorCount != 0 || (account.BanStatus != "" && account.BanStatus != "ACTIVE") {
		t.Fatalf("local persistence failure incorrectly penalized the successful upstream account: %#v", account)
	}
}

func TestResponsesStreamPersistenceFailureTerminatesFailed(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "streamed successfully upstream",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	oldSave := saveResponseForCompletion
	saveResponseForCompletion = func(*ResponsesObject) error { return errors.New("forced local persistence failure") }
	t.Cleanup(func() { saveResponseForCompletion = oldSave })

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"persist this stream",
		"stream":true,
		"store":true
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s, want committed SSE status 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("missing response.failed terminal event:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("persistence failure also emitted response.completed:\n%s", body)
	}
	if strings.Count(body, "event: response.failed") != 1 {
		t.Fatalf("response.failed terminal event count != 1:\n%s", body)
	}
	if !strings.Contains(body, responsesStatePersistenceFailureMessage) {
		t.Fatalf("local persistence failure message missing from failed response:\n%s", body)
	}
	h.pool.WaitForPendingStats()
	account, ok := config.GetAccountByID("test-account")
	if !ok || !account.Enabled || account.ErrorCount != 0 || (account.BanStatus != "" && account.BanStatus != "ACTIVE") {
		t.Fatalf("local persistence failure incorrectly penalized the successful upstream account: %#v", account)
	}
}
