package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"kiro-go/config"
)

type requestBodyErrorProtocol uint8

const (
	requestBodyErrorOpenAI requestBodyErrorProtocol = iota
	requestBodyErrorClaude
)

const claudeFrontDoorMaxRequestBodyBytes int64 = 32 * 1024 * 1024

func requestBodyLimitFor(r *http.Request, protocol requestBodyErrorProtocol) int64 {
	limit := config.GetMaxRequestBodyBytes()
	if protocol != requestBodyErrorClaude || r == nil {
		return limit
	}
	path := strings.TrimSpace(r.URL.Path)
	if path == "/v1/messages" || path == "/v1/messages/count_tokens" {
		if limit <= 0 || limit > claudeFrontDoorMaxRequestBodyBytes {
			return claudeFrontDoorMaxRequestBodyBytes
		}
	}
	return limit
}

type limitedBodyReadFinalizer struct {
	h        *Handler
	w        http.ResponseWriter
	protocol requestBodyErrorProtocol
	maxBytes int64
	err      error
	done     bool
}

func (f *limitedBodyReadFinalizer) finalize() {
	if f == nil || f.done || f.err == nil {
		return
	}
	// A response writer may be reused by middleware/tests. Never let a stale
	// successful response length survive into the protocol error payload.
	f.w.Header().Del("Content-Length")

	var maxErr *http.MaxBytesError
	if errors.As(f.err, &maxErr) {
		message := fmt.Sprintf("Request body exceeds the maximum size of %d bytes", f.maxBytes)
		if f.protocol == requestBodyErrorClaude {
			f.h.sendClaudeError(f.w, http.StatusRequestEntityTooLarge, "request_too_large", message)
		} else {
			f.h.sendOpenAIError(f.w, http.StatusRequestEntityTooLarge, "invalid_request_error", message)
		}
		return
	}

	if f.protocol == requestBodyErrorClaude {
		f.h.sendClaudeError(f.w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
	} else {
		f.h.sendOpenAIError(f.w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
	}
}

// readLimitedJSONBody applies one shared body-size contract to public JSON
// endpoints. The deferred finalizer is deliberately inside this helper: even a
// chunked request that fails while the handler returns immediately is guaranteed
// to receive a non-200 protocol response.
func (h *Handler) readLimitedJSONBody(
	w http.ResponseWriter,
	r *http.Request,
	protocol requestBodyErrorProtocol,
) ([]byte, bool) {
	maxBytes := requestBodyLimitFor(r, protocol)
	finalizer := &limitedBodyReadFinalizer{
		h: h, w: w, protocol: protocol, maxBytes: maxBytes,
	}
	defer finalizer.finalize()

	if r.ContentLength > maxBytes {
		finalizer.err = &http.MaxBytesError{Limit: maxBytes}
		return nil, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		finalizer.err = err
		return nil, false
	}
	finalizer.done = true
	return body, true
}
