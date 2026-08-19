package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadLimitedJSONBodyAcceptsExactBoundary(t *testing.T) {
	t.Setenv("KIRO_MAX_REQUEST_BODY_BYTES", "8")
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("12345678"))
	rec := httptest.NewRecorder()
	body, ok := h.readLimitedJSONBody(rec, req, requestBodyErrorClaude)
	if !ok || string(body) != "12345678" {
		t.Fatalf("body=%q ok=%t", body, ok)
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("unexpected response code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestReadLimitedJSONBodyChunkedOverflowAlwaysWrites413(t *testing.T) {
	t.Setenv("KIRO_MAX_REQUEST_BODY_BYTES", "8")
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("123456789"))
	req.ContentLength = -1 // force the read-time/chunked limit path
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Length", "999")

	if body, ok := h.readLimitedJSONBody(rec, req, requestBodyErrorClaude); ok || body != nil {
		t.Fatalf("overflow unexpectedly succeeded: body=%q ok=%t", body, ok)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got == "999" {
		t.Fatalf("stale Content-Length survived: %q", got)
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if payload.Type != "error" || payload.Error.Type != "request_too_large" {
		t.Fatalf("unexpected Claude error shape: %#v", payload)
	}
}

func TestReadLimitedJSONBodyKnownLengthOverflowOpenAIShape(t *testing.T) {
	t.Setenv("KIRO_MAX_REQUEST_BODY_BYTES", "4")
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("12345"))
	rec := httptest.NewRecorder()

	if _, ok := h.readLimitedJSONBody(rec, req, requestBodyErrorOpenAI); ok {
		t.Fatal("known oversized body unexpectedly succeeded")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("unexpected OpenAI error type: %q", payload.Error.Type)
	}
}

func TestClaudeFrontDoorLimitCapsLargerGlobalSetting(t *testing.T) {
	t.Setenv("KIRO_MAX_REQUEST_BODY_BYTES", "67108864")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if got := requestBodyLimitFor(req, requestBodyErrorClaude); got != claudeFrontDoorMaxRequestBodyBytes {
		t.Fatalf("Claude body limit=%d want=%d", got, claudeFrontDoorMaxRequestBodyBytes)
	}
	openAIReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := requestBodyLimitFor(openAIReq, requestBodyErrorOpenAI); got != 67108864 {
		t.Fatalf("OpenAI-specific configured limit was capped: %d", got)
	}
}
