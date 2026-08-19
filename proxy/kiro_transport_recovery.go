package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"kiro-go/config"
	"kiro-go/diagnostics"
	"kiro-go/logger"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const kiroTransportMaxAttempts = 3

type kiroStreamReadStage string

const (
	kiroStreamReadPreludeCRC kiroStreamReadStage = "prelude_crc"
	kiroStreamReadFrameBody  kiroStreamReadStage = "frame_body"
)

// KiroStreamError preserves where an AWS Event Stream was truncated. A plain
// io.ErrUnexpectedEOF loses whether the peer disappeared while reading the 8-byte
// prelude plus its 4-byte CRC, or in the remaining message body.
type KiroStreamError struct {
	Stage          kiroStreamReadStage
	ReadBytes      int
	ExpectedBytes  int
	WireBytes      int64
	CompleteFrames int
	LastEventType  string
	Cause          error
}

func (e *KiroStreamError) Error() string {
	if e == nil {
		return "kiro event stream error"
	}
	return fmt.Sprintf(
		"kiro event stream truncated stage=%s read=%d/%d wireBytes=%d frames=%d last=%q: %v",
		e.Stage,
		e.ReadBytes,
		e.ExpectedBytes,
		e.WireBytes,
		e.CompleteFrames,
		e.LastEventType,
		e.Cause,
	)
}

func (e *KiroStreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func newKiroStreamReadError(
	stage kiroStreamReadStage,
	readBytes int,
	expectedBytes int,
	wireBytes int64,
	completeFrames int,
	lastEventType string,
	cause error,
) error {
	if cause == nil {
		return nil
	}
	return &KiroStreamError{
		Stage:          stage,
		ReadBytes:      readBytes,
		ExpectedBytes:  expectedBytes,
		WireBytes:      wireBytes,
		CompleteFrames: completeFrames,
		LastEventType:  lastEventType,
		Cause:          cause,
	}
}

// KiroTransportError is returned only for transport-scoped failures. Account
// health code can therefore distinguish a broken stream/socket from credential,
// quota, suspension, or deterministic request errors.
type KiroTransportError struct {
	Kind            string
	Layer           string
	InvocationID    string
	Attempt         int
	MaxAttempts     int
	Retryable       bool
	SemanticEmitted bool
	LastSemantic    string
	Cause           error
}

func (e *KiroTransportError) Error() string {
	if e == nil {
		return "kiro transport error"
	}
	return fmt.Sprintf(
		"kiro transport kind=%s layer=%s invocation=%s attempt=%d/%d semantic=%t: %v",
		e.Kind,
		e.Layer,
		e.InvocationID,
		e.Attempt,
		e.MaxAttempts,
		e.SemanticEmitted,
		e.Cause,
	)
}

func (e *KiroTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func isKiroTransientTransportFailure(err error) bool {
	var transportErr *KiroTransportError
	return errors.As(err, &transportErr) && transportErr.Retryable
}

type kiroAttemptContextKey struct{}

type kiroAttemptMetadata struct {
	InvocationID string
	Attempt      int
	MaxAttempts  int
	Trace        *kiroTransportTrace
}

func withKiroAttemptMetadata(
	ctx context.Context,
	metadata kiroAttemptMetadata,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, kiroAttemptContextKey{}, metadata)
}

func kiroAttemptMetadataFromContext(ctx context.Context) kiroAttemptMetadata {
	if ctx != nil {
		if metadata, ok := ctx.Value(kiroAttemptContextKey{}).(kiroAttemptMetadata); ok {
			if metadata.Attempt <= 0 {
				metadata.Attempt = 1
			}
			if metadata.MaxAttempts <= 0 {
				metadata.MaxAttempts = 1
			}
			if strings.TrimSpace(metadata.InvocationID) == "" {
				metadata.InvocationID = uuid.New().String()
			}
			return metadata
		}
	}
	return kiroAttemptMetadata{
		InvocationID: uuid.New().String(),
		Attempt:      1,
		MaxAttempts:  1,
	}
}

func applyKiroAttemptHeaders(req *http.Request, ctx context.Context) {
	if req == nil {
		return
	}
	metadata := kiroAttemptMetadataFromContext(ctx)
	req.Header.Set(
		"Amz-Sdk-Request",
		fmt.Sprintf("attempt=%d; max=%d", metadata.Attempt, metadata.MaxAttempts),
	)
	req.Header.Set("Amz-Sdk-Invocation-Id", metadata.InvocationID)
}

type kiroAttemptCallbacks struct {
	original        *KiroStreamCallback
	semanticEmitted bool
	lastSemantic    string
	committed       bool

	contentChars     int
	toolCount        int
	stopReason       string
	sawReasoning     bool
	sawMeteringEvent bool

	pendingStopReason  bool
	pendingComplete    bool
	pendingInput       int
	pendingOutput      int
	pendingCredits     bool
	credits            float64
	contextPercentages []float64
}

func newKiroAttemptCallbacks(original *KiroStreamCallback) *kiroAttemptCallbacks {
	if original == nil {
		original = &KiroStreamCallback{}
	}
	return &kiroAttemptCallbacks{original: original}
}

func (s *kiroAttemptCallbacks) flushPending() {
	if s == nil || s.committed {
		return
	}
	// Terminal/accounting callbacks remain attempt-local until semantic integrity
	// has been validated. Semantic output may already be visible, but a clean EOF
	// without stopReason must still end as a failure, never as OnComplete.
	s.committed = true
	if s.pendingStopReason && s.original.OnStopReason != nil {
		s.original.OnStopReason(s.stopReason)
	}
	if s.original.OnContextUsage != nil {
		for _, percentage := range s.contextPercentages {
			s.original.OnContextUsage(percentage)
		}
	}
	if s.pendingCredits && s.original.OnCredits != nil {
		s.original.OnCredits(s.credits)
	}
	if s.pendingComplete && s.original.OnComplete != nil {
		s.original.OnComplete(s.pendingInput, s.pendingOutput)
	}
	s.pendingStopReason = false
	s.contextPercentages = nil
	s.pendingCredits = false
	s.pendingComplete = false
}

func (s *kiroAttemptCallbacks) markSemantic(kind string) {
	if s == nil {
		return
	}
	s.semanticEmitted = true
	s.lastSemantic = kind
}

func (s *kiroAttemptCallbacks) onText(text string, isThinking bool) {
	if s == nil {
		return
	}
	if isThinking {
		s.sawReasoning = s.sawReasoning || text != ""
	} else if text != "" {
		s.contentChars += len([]rune(text))
	}
	s.markSemantic("text")
	if s.original.OnText != nil {
		s.original.OnText(text, isThinking)
	}
}

func (s *kiroAttemptCallbacks) onReasoningSignature(signature string) {
	if s == nil {
		return
	}
	if signature != "" {
		s.sawReasoning = true
	}
	s.markSemantic("reasoning_signature")
	if s.original.OnReasoningSignature != nil {
		s.original.OnReasoningSignature(signature)
	}
}

func (s *kiroAttemptCallbacks) onReasoningRedacted(data string) {
	if s == nil {
		return
	}
	if data != "" {
		s.sawReasoning = true
	}
	s.markSemantic("reasoning_redacted")
	if s.original.OnReasoningRedacted != nil {
		s.original.OnReasoningRedacted(data)
	}
}

func (s *kiroAttemptCallbacks) onToolUse(toolUse KiroToolUse) {
	if s == nil {
		return
	}
	s.toolCount++
	s.markSemantic("tool_use")
	if s.original.OnToolUse != nil {
		s.original.OnToolUse(toolUse)
	}
}

func (s *kiroAttemptCallbacks) onStopReason(reason string) {
	if s == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	s.stopReason = reason
	if s.committed {
		if s.original.OnStopReason != nil {
			s.original.OnStopReason(reason)
		}
		return
	}
	s.pendingStopReason = true
}

func (s *kiroAttemptCallbacks) onComplete(inputTokens, outputTokens int) {
	if s == nil {
		return
	}
	if s.committed {
		if s.original.OnComplete != nil {
			s.original.OnComplete(inputTokens, outputTokens)
		}
		return
	}
	s.pendingComplete = true
	s.pendingInput = inputTokens
	s.pendingOutput = outputTokens
}

func (s *kiroAttemptCallbacks) onCredits(credits float64) {
	if s == nil {
		return
	}
	if s.committed {
		if s.original.OnCredits != nil {
			s.original.OnCredits(credits)
		}
		return
	}
	s.pendingCredits = true
	s.credits = credits
}

func (s *kiroAttemptCallbacks) onContextUsage(percentage float64) {
	if s == nil {
		return
	}
	if s.committed {
		if s.original.OnContextUsage != nil {
			s.original.OnContextUsage(percentage)
		}
		return
	}
	s.contextPercentages = append(s.contextPercentages, percentage)
}

func (s *kiroAttemptCallbacks) onMeteringEvent() {
	if s == nil {
		return
	}
	// A metering frame is terminal evidence for IdC / Enterprise streams that
	// intentionally end without metadataEvent. It is bookkeeping, not semantic
	// output, so seeing it must not by itself cross the replay boundary.
	s.sawMeteringEvent = true
}

func (s *kiroAttemptCallbacks) callback() *KiroStreamCallback {
	if s == nil {
		return &KiroStreamCallback{}
	}
	original := s.original
	return &KiroStreamCallback{
		OnText:               s.onText,
		OnReasoningSignature: s.onReasoningSignature,
		OnReasoningRedacted:  s.onReasoningRedacted,
		OnToolUse:            s.onToolUse,
		OnStopReason:         s.onStopReason,
		OnError: func(err error) {
			s.markSemantic("upstream_error")
			if original.OnError != nil {
				original.OnError(err)
			}
		},
		OnComplete:      s.onComplete,
		OnCredits:       s.onCredits,
		OnContextUsage:  s.onContextUsage,
		OnMeteringEvent: s.onMeteringEvent,
		Err:             original.Err,
		ReserveRetained: original.ReserveRetained,
		ReleaseRetained: original.ReleaseRetained,
	}
}

func (s *kiroAttemptCallbacks) integrityError() error {
	if s == nil {
		return errUpstreamTruncatedResponse
	}

	// Some IAM Identity Center / Enterprise Kiro backends close a successful
	// stream as assistant/tool output -> contextUsageEvent -> meteringEvent ->
	// clean EOF, without metadataEvent. The recovery layer is the right authority
	// to interpret that terminal evidence because integrityError is reached only
	// after the transport attempt itself returned nil. A socket/parser error still
	// fails even if a metering frame was observed. Real upstream stopReason always
	// wins, and contextUsageEvent alone deliberately remains insufficient.
	if strings.TrimSpace(s.stopReason) == "" && s.sawMeteringEvent {
		synthetic := ""
		switch {
		case s.toolCount > 0:
			synthetic = "tool_use"
		case s.contentChars > 0 || s.sawReasoning:
			synthetic = "end_turn"
		}
		if synthetic != "" {
			s.stopReason = synthetic
			s.pendingStopReason = true
		}
	}

	return classifyStreamIntegrity(s.contentChars, s.toolCount, s.stopReason, s.sawReasoning)
}

func (s *kiroAttemptCallbacks) replaySafe() bool {
	if s == nil || !s.semanticEmitted {
		return true
	}
	// A caller that buffers semantic callbacks may opt into retry only when it
	// can both prove the attempt is still invisible and reset all per-attempt
	// state before the next attempt. Streaming callers omit these hooks and keep
	// the conservative no-replay-after-semantic-output rule.
	return s.original.CanRetry != nil &&
		s.original.ResetForRetry != nil &&
		s.original.CanRetry()
}

func (s *kiroAttemptCallbacks) resetForRetry() {
	if s == nil || !s.semanticEmitted || s.original.ResetForRetry == nil {
		return
	}
	s.original.ResetForRetry()
}

func (s *kiroAttemptCallbacks) commitSuccess() {
	if s != nil {
		s.flushPending()
	}
}

type kiroRequestTrace struct {
	HostPort            string
	DNSStart            time.Time
	DNSDuration         time.Duration
	DNSAddrs            []string
	DNSErr              string
	ConnectStart        time.Time
	ConnectDuration     time.Duration
	ConnectNetwork      string
	ConnectAddr         string
	ConnectErr          string
	TLSStart            time.Time
	TLSDuration         time.Duration
	TLSNegotiated       string
	TLSErr              string
	Reused              bool
	WasIdle             bool
	IdleTime            time.Duration
	LocalAddr           string
	RemoteAddr          string
	WroteRequestAt      time.Duration
	WriteErr            string
	FirstResponseByteAt time.Duration
	HTTPProto           string
	HTTPStatus          int
	ContentLength       int64
	TransferEncoding    string
	ResponseClose       bool
	ConnectionHeader    string
	RequestID           string
	ServerHeader        string
	ViaHeader           string
	CacheHeader         string
	CloudFrontID        string
	TraceID             string
	FirstBodyByteAt     time.Duration
	LastBodyProgressAt  time.Duration
	BodyBytes           int64
	BodyReads           int
	BodyReadErr         string
	BodyReadErrType     string
}

type kiroTransportTrace struct {
	mu       sync.Mutex
	started  time.Time
	requests []kiroRequestTrace
	current  int
}

func newKiroTransportTrace() *kiroTransportTrace {
	return &kiroTransportTrace{started: time.Now(), current: -1}
}

func (t *kiroTransportTrace) elapsed(now time.Time) time.Duration {
	if t == nil || t.started.IsZero() {
		return 0
	}
	return now.Sub(t.started)
}

func (t *kiroTransportTrace) currentRequestLocked() *kiroRequestTrace {
	if t == nil || t.current < 0 || t.current >= len(t.requests) {
		return nil
	}
	return &t.requests[t.current]
}

func (t *kiroTransportTrace) startRequest(hostPort string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.requests = append(t.requests, kiroRequestTrace{HostPort: hostPort})
	t.current = len(t.requests) - 1
	t.mu.Unlock()
}

func (t *kiroTransportTrace) clientTrace() *httptrace.ClientTrace {
	if t == nil {
		return nil
	}
	return &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			t.startRequest(hostPort)
		},
		DNSStart: func(httptrace.DNSStartInfo) {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				request.DNSStart = time.Now()
			}
			t.mu.Unlock()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				if !request.DNSStart.IsZero() {
					request.DNSDuration = time.Since(request.DNSStart)
				}
				request.DNSAddrs = request.DNSAddrs[:0]
				for _, addr := range info.Addrs {
					request.DNSAddrs = append(request.DNSAddrs, addr.String())
				}
				if info.Err != nil {
					request.DNSErr = info.Err.Error()
				}
			}
			t.mu.Unlock()
		},
		ConnectStart: func(network, addr string) {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				request.ConnectStart = time.Now()
				request.ConnectNetwork = network
				request.ConnectAddr = addr
			}
			t.mu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				if !request.ConnectStart.IsZero() {
					request.ConnectDuration = time.Since(request.ConnectStart)
				}
				request.ConnectNetwork = network
				request.ConnectAddr = addr
				if err != nil {
					request.ConnectErr = err.Error()
				}
			}
			t.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				request.TLSStart = time.Now()
			}
			t.mu.Unlock()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				if !request.TLSStart.IsZero() {
					request.TLSDuration = time.Since(request.TLSStart)
				}
				request.TLSNegotiated = state.NegotiatedProtocol
				if err != nil {
					request.TLSErr = err.Error()
				}
			}
			t.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				request.Reused = info.Reused
				request.WasIdle = info.WasIdle
				request.IdleTime = info.IdleTime
				if info.Conn != nil {
					if addr := info.Conn.LocalAddr(); addr != nil {
						request.LocalAddr = addr.String()
					}
					if addr := info.Conn.RemoteAddr(); addr != nil {
						request.RemoteAddr = addr.String()
					}
				}
			}
			t.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				request.WroteRequestAt = t.elapsed(time.Now())
				if info.Err != nil {
					request.WriteErr = info.Err.Error()
				}
			}
			t.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			if request := t.currentRequestLocked(); request != nil {
				request.FirstResponseByteAt = t.elapsed(time.Now())
			}
			t.mu.Unlock()
		},
	}
}

func (t *kiroTransportTrace) currentRequestIndex() int {
	if t == nil {
		return -1
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.current
}

func (t *kiroTransportTrace) recordResponse(index int, resp *http.Response) {
	if t == nil || resp == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if index < 0 || index >= len(t.requests) {
		index = t.current
	}
	if index < 0 || index >= len(t.requests) {
		return
	}
	request := &t.requests[index]
	request.HTTPProto = resp.Proto
	request.HTTPStatus = resp.StatusCode
	request.ContentLength = resp.ContentLength
	request.TransferEncoding = strings.Join(resp.TransferEncoding, ",")
	request.ResponseClose = resp.Close
	request.ConnectionHeader = strings.TrimSpace(resp.Header.Get("Connection"))
	for _, header := range []string{"X-Amzn-Requestid", "X-Amz-Request-Id"} {
		if value := strings.TrimSpace(resp.Header.Get(header)); value != "" {
			request.RequestID = value
			break
		}
	}
	request.ServerHeader = strings.TrimSpace(resp.Header.Get("Server"))
	request.ViaHeader = strings.TrimSpace(resp.Header.Get("Via"))
	request.CacheHeader = strings.TrimSpace(resp.Header.Get("X-Cache"))
	request.CloudFrontID = strings.TrimSpace(resp.Header.Get("X-Amz-Cf-Id"))
	request.TraceID = strings.TrimSpace(resp.Header.Get("X-Amzn-Trace-Id"))
}

func (t *kiroTransportTrace) recordBodyRead(index int, n int, err error) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if index < 0 || index >= len(t.requests) {
		index = t.current
	}
	if index < 0 || index >= len(t.requests) {
		return
	}
	request := &t.requests[index]
	if n > 0 {
		request.BodyReads++
		request.BodyBytes += int64(n)
		if request.FirstBodyByteAt == 0 {
			request.FirstBodyByteAt = t.elapsed(now)
		}
		request.LastBodyProgressAt = t.elapsed(now)
	}
	if err != nil {
		request.BodyReadErrType = fmt.Sprintf("%T", err)
		if errors.Is(err, io.EOF) {
			request.BodyReadErr = "EOF"
		} else {
			request.BodyReadErr = err.Error()
		}
	}
}

func (t *kiroTransportTrace) lastHTTPProto() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.requests) - 1; i >= 0; i-- {
		if t.requests[i].HTTPProto != "" {
			return t.requests[i].HTTPProto
		}
		if t.requests[i].TLSNegotiated != "" {
			return t.requests[i].TLSNegotiated
		}
	}
	return ""
}

func (t *kiroTransportTrace) summary() string {
	if t == nil {
		return "requests=[]"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := make([]string, 0, len(t.requests))
	for i, request := range t.requests {
		parts = append(parts, fmt.Sprintf(
			"r%d{host=%q proto=%q alpn=%q status=%d contentLength=%d transferEncoding=%q responseClose=%t connection=%q requestId=%q server=%q via=%q cache=%q cfId=%q traceId=%q reused=%t idle=%t idleMs=%d local=%q remote=%q connectNetwork=%q connectAddr=%q dnsAddrs=%q dnsMs=%d connectMs=%d tlsMs=%d wroteMs=%d firstHeaderMs=%d firstBodyMs=%d lastBodyMs=%d bodyBytes=%d bodyReads=%d dnsErr=%q connectErr=%q tlsErr=%q writeErr=%q bodyErrType=%q bodyErr=%q}",
			i+1,
			request.HostPort,
			request.HTTPProto,
			request.TLSNegotiated,
			request.HTTPStatus,
			request.ContentLength,
			request.TransferEncoding,
			request.ResponseClose,
			request.ConnectionHeader,
			request.RequestID,
			request.ServerHeader,
			request.ViaHeader,
			request.CacheHeader,
			request.CloudFrontID,
			request.TraceID,
			request.Reused,
			request.WasIdle,
			request.IdleTime.Milliseconds(),
			request.LocalAddr,
			request.RemoteAddr,
			request.ConnectNetwork,
			request.ConnectAddr,
			strings.Join(request.DNSAddrs, ","),
			request.DNSDuration.Milliseconds(),
			request.ConnectDuration.Milliseconds(),
			request.TLSDuration.Milliseconds(),
			request.WroteRequestAt.Milliseconds(),
			request.FirstResponseByteAt.Milliseconds(),
			request.FirstBodyByteAt.Milliseconds(),
			request.LastBodyProgressAt.Milliseconds(),
			request.BodyBytes,
			request.BodyReads,
			request.DNSErr,
			request.ConnectErr,
			request.TLSErr,
			request.WriteErr,
			request.BodyReadErrType,
			request.BodyReadErr,
		))
	}
	return "requests=[" + strings.Join(parts, ",") + "]"
}

func recordKiroHTTPResponseTrace(ctx context.Context, resp *http.Response) {
	metadata := kiroAttemptMetadataFromContext(ctx)
	if metadata.Trace == nil || resp == nil {
		return
	}
	metadata.Trace.recordResponse(metadata.Trace.currentRequestIndex(), resp)
}

type kiroTraceReadCloser struct {
	body  io.ReadCloser
	trace *kiroTransportTrace
	index int
}

func newKiroTraceReadCloser(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return body
	}
	metadata := kiroAttemptMetadataFromContext(ctx)
	if metadata.Trace == nil {
		return body
	}
	return &kiroTraceReadCloser{
		body:  body,
		trace: metadata.Trace,
		index: metadata.Trace.currentRequestIndex(),
	}
}

func (r *kiroTraceReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if r.trace != nil {
		r.trace.recordBodyRead(r.index, n, err)
	}
	return n, err
}

func (r *kiroTraceReadCloser) Close() error {
	if r == nil || r.body == nil {
		return nil
	}
	return r.body.Close()
}

func (t *kiroTransportTrace) failureLayerHint() string {
	if t == nil {
		return "network"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.requests) - 1; i >= 0; i-- {
		request := t.requests[i]
		switch {
		case request.DNSErr != "":
			return "dns"
		case request.ConnectErr != "":
			return "tcp_connect"
		case request.TLSErr != "":
			return "tls_handshake"
		case request.WriteErr != "":
			return "request_write"
		case request.BodyReadErr != "":
			return "response_body"
		}
	}
	return "network"
}

type kiroFailureClassification struct {
	Kind      string
	Layer     string
	Retryable bool
}

func classifyKiroTransportFailure(
	err error,
	trace *kiroTransportTrace,
) kiroFailureClassification {
	if err == nil {
		return kiroFailureClassification{}
	}
	if errors.Is(err, context.Canceled) {
		return kiroFailureClassification{Kind: "context_canceled", Layer: "request_context"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// The caller checks its parent context before classification. If that
		// context is still live, DeadlineExceeded here came from the HTTP
		// transport (for example ResponseHeaderTimeout) and is retryable.
		return kiroFailureClassification{Kind: "network_timeout", Layer: "http_transport", Retryable: true}
	}
	if errors.Is(err, errUpstreamBodyIdleTimeout) {
		return kiroFailureClassification{Kind: "body_idle_timeout", Layer: "local_idle_watchdog", Retryable: true}
	}
	if errors.Is(err, errUpstreamTruncatedResponse) {
		return kiroFailureClassification{Kind: "semantic_truncation", Layer: "event_stream_semantics", Retryable: true}
	}
	if errors.Is(err, errEmptyKiroStream) {
		return kiroFailureClassification{Kind: "empty_stream", Layer: "event_stream_semantics", Retryable: true}
	}
	if errors.Is(err, errIncompleteKiroToolInput) {
		return kiroFailureClassification{Kind: "incomplete_tool_input", Layer: "event_stream_semantics", Retryable: true}
	}
	if errors.Is(err, errInvalidKiroEventStream) {
		return kiroFailureClassification{Kind: "invalid_event_stream", Layer: "event_stream_integrity", Retryable: true}
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return kiroFailureClassification{Kind: "tcp_reset", Layer: "tcp_peer_or_intermediary", Retryable: true}
	}
	if errors.Is(err, syscall.EPIPE) {
		return kiroFailureClassification{Kind: "broken_pipe", Layer: "tcp_peer_or_intermediary", Retryable: true}
	}
	var streamErr *KiroStreamError
	if errors.As(err, &streamErr) && errors.Is(streamErr.Cause, io.ErrUnexpectedEOF) {
		kind := "event_stream_truncated"
		if strings.HasPrefix(strings.ToUpper(trace.lastHTTPProto()), "HTTP/2") || trace.lastHTTPProto() == "h2" {
			kind = "event_stream_truncated_http2"
		} else if strings.HasPrefix(strings.ToUpper(trace.lastHTTPProto()), "HTTP/1") {
			kind = "event_stream_truncated_http1"
		}
		return kiroFailureClassification{Kind: kind, Layer: "upstream_or_intermediary_body", Retryable: true}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return kiroFailureClassification{Kind: "unexpected_eof", Layer: "upstream_or_intermediary_body", Retryable: true}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return kiroFailureClassification{Kind: "network_timeout", Layer: "network", Retryable: true}
		}
		return kiroFailureClassification{Kind: "network_io", Layer: trace.failureLayerHint(), Retryable: true}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return kiroFailureClassification{Kind: "url_transport", Layer: trace.failureLayerHint(), Retryable: true}
	}

	proto := strings.ToLower(trace.lastHTTPProto())
	message := strings.ToLower(err.Error())
	if (proto == "h2" || strings.HasPrefix(proto, "http/2")) &&
		(strings.Contains(message, "rst_stream") || strings.Contains(message, "stream error")) {
		return kiroFailureClassification{Kind: "http2_stream_reset", Layer: "http2_peer", Retryable: true}
	}
	return kiroFailureClassification{Kind: "non_transport", Layer: "application"}
}

func kiroRetryBackoff(invocationID string, attempt int) time.Duration {
	bases := []time.Duration{250 * time.Millisecond, 750 * time.Millisecond}
	if attempt <= 0 {
		attempt = 1
	}
	idx := attempt - 1
	if idx >= len(bases) {
		idx = len(bases) - 1
	}
	base := bases[idx]
	h := fnv.New32a()
	_, _ = h.Write([]byte(invocationID))
	_, _ = h.Write([]byte{byte(attempt)})
	// 80..120 percent, deterministic per logical invocation/attempt. This spreads
	// concurrent retries without introducing a package-global PRNG dependency.
	percent := 80 + int(h.Sum32()%41)
	return time.Duration(int64(base) * int64(percent) / 100)
}

func waitKiroRetryBackoff(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func godebugHTTP2ClientDisabled() bool {
	parts := strings.Split(os.Getenv("GODEBUG"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(strings.TrimSpace(parts[i]), "=")
		if ok && key == "http2client" {
			return value == "0"
		}
	}
	return false
}

func kiroProxyTraceDescriptor(account *config.Account) string {
	proxyURL := strings.TrimSpace(ResolveAccountProxyURL(account))
	if proxyURL == "" {
		if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" ||
			os.Getenv("HTTP_PROXY") != "" || os.Getenv("http_proxy") != "" {
			return "environment"
		}
		return "none"
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return "configured-invalid"
	}
	if parsed.Scheme == "" {
		return parsed.Host
	}
	return parsed.Scheme + "://" + parsed.Host
}

func kiroErrorChain(err error) string {
	if err == nil {
		return ""
	}
	parts := make([]string, 0, 6)
	for depth := 0; err != nil && depth < 8; depth++ {
		parts = append(parts, fmt.Sprintf("%T:%v", err, err))
		err = errors.Unwrap(err)
	}
	return strings.Join(parts, " <- ")
}

func logKiroTransportAttempt(
	account *config.Account,
	metadata kiroAttemptMetadata,
	callbacks *kiroAttemptCallbacks,
	classification kiroFailureClassification,
	err error,
	duration time.Duration,
) {
	stage := ""
	readBytes := 0
	expectedBytes := 0
	wireBytes := int64(0)
	events := 0
	lastEvent := ""
	var streamErr *KiroStreamError
	if errors.As(err, &streamErr) {
		stage = string(streamErr.Stage)
		readBytes = streamErr.ReadBytes
		expectedBytes = streamErr.ExpectedBytes
		wireBytes = streamErr.WireBytes
		events = streamErr.CompleteFrames
		lastEvent = streamErr.LastEventType
	}
	semantic := false
	lastSemantic := ""
	if callbacks != nil {
		semantic = callbacks.semanticEmitted
		lastSemantic = callbacks.lastSemantic
	}
	logger.Warnf(
		"[KiroTransportTrace] account=%s invocation=%s attempt=%d/%d class=%s layer=%s retryable=%t semantic=%t lastSemantic=%q durationMs=%d stage=%q read=%d/%d parserWireBytes=%d completeFrames=%d lastEvent=%q godebugHttp2ClientOff=%t proxy=%q errorType=%T error=%q errorChain=%q %s",
		accountEmailForLog(account),
		metadata.InvocationID,
		metadata.Attempt,
		metadata.MaxAttempts,
		classification.Kind,
		classification.Layer,
		classification.Retryable,
		semantic,
		lastSemantic,
		duration.Milliseconds(),
		stage,
		readBytes,
		expectedBytes,
		wireBytes,
		events,
		lastEvent,
		godebugHTTP2ClientDisabled(),
		kiroProxyTraceDescriptor(account),
		err,
		err.Error(),
		kiroErrorChain(err),
		metadata.Trace.summary(),
	)
}

type kiroAttemptFunc func(
	context.Context,
	*config.Account,
	*KiroPayload,
	*KiroStreamCallback,
) error

func callKiroAPIContextWithRecovery(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	callback *KiroStreamCallback,
	doAttempt kiroAttemptFunc,
	waitRetry func(context.Context, time.Duration) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if doAttempt == nil {
		return errors.New("kiro transport: nil attempt function")
	}
	if waitRetry == nil {
		waitRetry = waitKiroRetryBackoff
	}
	invocationID := uuid.New().String()
	started := time.Now()

	for attempt := 1; attempt <= kiroTransportMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		traceState := newKiroTransportTrace()
		metadata := kiroAttemptMetadata{
			InvocationID: invocationID,
			Attempt:      attempt,
			MaxAttempts:  kiroTransportMaxAttempts,
			Trace:        traceState,
		}
		attemptCtx := withKiroAttemptMetadata(ctx, metadata)
		attemptCtx = httptrace.WithClientTrace(attemptCtx, traceState.clientTrace())
		attemptCallbacks := newKiroAttemptCallbacks(callback)
		attemptStarted := time.Now()
		err := doAttempt(attemptCtx, account, payload, attemptCallbacks.callback())
		if err == nil {
			err = attemptCallbacks.integrityError()
		}
		if err == nil {
			attemptCallbacks.commitSuccess()
			if attempt > 1 {
				logger.Infof(
					"[KiroTransportRecovered] account=%s invocation=%s attempts=%d durationMs=%d %s",
					accountEmailForLog(account),
					invocationID,
					attempt,
					time.Since(started).Milliseconds(),
					traceState.summary(),
				)
			} else if diagnostics.Stream() {
				logger.Infof(
					"[KiroTransportTrace] account=%s invocation=%s attempt=1/3 class=success durationMs=%d godebugHttp2ClientOff=%t proxy=%q %s",
					accountEmailForLog(account),
					invocationID,
					time.Since(attemptStarted).Milliseconds(),
					godebugHTTP2ClientDisabled(),
					kiroProxyTraceDescriptor(account),
					traceState.summary(),
				)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		classification := classifyKiroTransportFailure(err, traceState)
		if classification.Retryable || diagnostics.Stream() {
			logKiroTransportAttempt(
				account,
				metadata,
				attemptCallbacks,
				classification,
				err,
				time.Since(attemptStarted),
			)
		}
		if !classification.Retryable {
			return err
		}
		if !attemptCallbacks.replaySafe() || attempt == kiroTransportMaxAttempts {
			return &KiroTransportError{
				Kind:            classification.Kind,
				Layer:           classification.Layer,
				InvocationID:    invocationID,
				Attempt:         attempt,
				MaxAttempts:     kiroTransportMaxAttempts,
				Retryable:       true,
				SemanticEmitted: attemptCallbacks.semanticEmitted,
				LastSemantic:    attemptCallbacks.lastSemantic,
				Cause:           err,
			}
		}

		delay := kiroRetryBackoff(invocationID, attempt)
		logger.Warnf(
			"[KiroTransportRetry] account=%s invocation=%s attempt=%d/%d nextAttempt=%d backoffMs=%d class=%s layer=%s semantic=%t",
			accountEmailForLog(account),
			invocationID,
			attempt,
			kiroTransportMaxAttempts,
			attempt+1,
			delay.Milliseconds(),
			classification.Kind,
			classification.Layer,
			attemptCallbacks.semanticEmitted,
		)
		// The previous attempt has been declared discardable. Reset buffered
		// protocol state before the backoff so cancellation during the wait cannot
		// leave an abandoned partial attempt in caller-owned memory.
		attemptCallbacks.resetForRetry()
		if err := waitRetry(ctx, delay); err != nil {
			return err
		}
	}
	return errors.New("kiro transport: retry loop exhausted")
}

// CallKiroAPIContext retries transient Kiro transport failures only while the
// attempt is still semantically invisible. Once text, thinking/signature,
// redacted thinking, a tool call, or an upstream error callback has been
// observed, replay is forbidden because it could duplicate client-visible state.
func CallKiroAPIContext(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	callback *KiroStreamCallback,
) error {
	return callKiroAPIContextWithRecovery(
		ctx,
		account,
		payload,
		callback,
		callKiroAPIContextOnce,
		waitKiroRetryBackoff,
	)
}
