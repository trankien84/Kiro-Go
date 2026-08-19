package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func testKiroRecoveryAccount() *config.Account {
	return &config.Account{
		Email:    "recovery-test@example.invalid",
		ProxyURL: "http://proxy.invalid:8080",
	}
}

func truncatedKiroStreamError(stage kiroStreamReadStage) error {
	return &KiroStreamError{
		Stage:         stage,
		ReadBytes:     3,
		ExpectedBytes: 12,
		WireBytes:     3,
		Cause:         io.ErrUnexpectedEOF,
	}
}

func TestKiroTransportRecoveryRetriesOnlyBeforeSemanticOutput(t *testing.T) {
	var calls int
	var output bytes.Buffer
	var completeCalls int
	var gotInput, gotOutput int
	var contextValues []float64
	var credits []float64

	callback := &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
		OnComplete: func(inputTokens, outputTokens int) {
			completeCalls++
			gotInput = inputTokens
			gotOutput = outputTokens
		},
		OnCredits: func(value float64) { credits = append(credits, value) },
		OnContextUsage: func(value float64) {
			contextValues = append(contextValues, value)
		},
	}

	err := callKiroAPIContextWithRecovery(
		context.Background(),
		testKiroRecoveryAccount(),
		&KiroPayload{},
		callback,
		func(_ context.Context, _ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
			calls++
			if calls == 1 {
				// These are bookkeeping callbacks. They must not leak from a failed
				// attempt that is going to be replayed.
				cb.OnContextUsage(11)
				cb.OnCredits(1)
				cb.OnComplete(10, 1)
				return truncatedKiroStreamError(kiroStreamReadPreludeCRC)
			}
			cb.OnContextUsage(22)
			cb.OnText("ok", false)
			cb.OnStopReason("end_turn")
			cb.OnCredits(2)
			cb.OnComplete(20, 2)
			return nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("attempts=%d want=2", calls)
	}
	if output.String() != "ok" {
		t.Fatalf("output=%q", output.String())
	}
	if completeCalls != 1 || gotInput != 20 || gotOutput != 2 {
		t.Fatalf("complete calls=%d input=%d output=%d", completeCalls, gotInput, gotOutput)
	}
	if len(credits) != 1 || credits[0] != 2 {
		t.Fatalf("credits=%v", credits)
	}
	if len(contextValues) != 1 || contextValues[0] != 22 {
		t.Fatalf("context values=%v", contextValues)
	}
}

func TestKiroTransportRecoveryNeverReplaysAfterSemanticCallback(t *testing.T) {
	var calls int
	var output bytes.Buffer
	err := callKiroAPIContextWithRecovery(
		context.Background(),
		testKiroRecoveryAccount(),
		&KiroPayload{},
		&KiroStreamCallback{OnText: func(text string, _ bool) { output.WriteString(text) }},
		func(_ context.Context, _ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
			calls++
			cb.OnText("partial", false)
			return truncatedKiroStreamError(kiroStreamReadFrameBody)
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil {
		t.Fatal("expected terminal transport error")
	}
	if calls != 1 {
		t.Fatalf("attempts=%d want=1", calls)
	}
	if output.String() != "partial" {
		t.Fatalf("output=%q", output.String())
	}
	var transportErr *KiroTransportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("error type=%T want *KiroTransportError: %v", err, err)
	}
	if !transportErr.SemanticEmitted || transportErr.LastSemantic != "text" {
		t.Fatalf("transport error=%#v", transportErr)
	}
	if !isKiroTransientTransportFailure(err) {
		t.Fatal("transport-scoped transient error was not classified")
	}
}

func TestKiroTransportRecoveryContextCancellationStopsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	err := callKiroAPIContextWithRecovery(
		ctx,
		testKiroRecoveryAccount(),
		&KiroPayload{},
		&KiroStreamCallback{},
		func(_ context.Context, _ *config.Account, _ *KiroPayload, _ *KiroStreamCallback) error {
			calls++
			cancel()
			return &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("attempts=%d want=1", calls)
	}
}

func TestApplyKiroAttemptHeadersUsesLogicalAttemptMetadata(t *testing.T) {
	ctx := withKiroAttemptMetadata(context.Background(), kiroAttemptMetadata{
		InvocationID: "logical-invocation",
		Attempt:      2,
		MaxAttempts:  3,
	})
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyKiroAttemptHeaders(req, ctx)
	if got := req.Header.Get("Amz-Sdk-Request"); got != "attempt=2; max=3" {
		t.Fatalf("Amz-Sdk-Request=%q", got)
	}
	if got := req.Header.Get("Amz-Sdk-Invocation-Id"); got != "logical-invocation" {
		t.Fatalf("Amz-Sdk-Invocation-Id=%q", got)
	}
}

func TestKiroTransportRecoveryPreservesInvocationAndIncrementsAttempt(t *testing.T) {
	var calls int
	var metadata []kiroAttemptMetadata
	err := callKiroAPIContextWithRecovery(
		context.Background(),
		testKiroRecoveryAccount(),
		&KiroPayload{},
		&KiroStreamCallback{},
		func(ctx context.Context, _ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
			calls++
			metadata = append(metadata, kiroAttemptMetadataFromContext(ctx))
			if calls == 1 {
				return truncatedKiroStreamError(kiroStreamReadPreludeCRC)
			}
			cb.OnStopReason("end_turn")
			return nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if len(metadata) != 2 {
		t.Fatalf("metadata=%#v", metadata)
	}
	if metadata[0].InvocationID == "" || metadata[0].InvocationID != metadata[1].InvocationID {
		t.Fatalf("invocation IDs=%q %q", metadata[0].InvocationID, metadata[1].InvocationID)
	}
	if metadata[0].Attempt != 1 || metadata[1].Attempt != 2 ||
		metadata[0].MaxAttempts != kiroTransportMaxAttempts ||
		metadata[1].MaxAttempts != kiroTransportMaxAttempts {
		t.Fatalf("attempt metadata=%#v", metadata)
	}
}

func TestClassifyKiroTransportFailureTreatsInternalDeadlineAsRetryable(t *testing.T) {
	classification := classifyKiroTransportFailure(
		context.DeadlineExceeded,
		newKiroTransportTrace(),
	)
	if classification.Kind != "network_timeout" || !classification.Retryable {
		t.Fatalf("classification=%#v", classification)
	}
}

func TestClassifyKiroTransportFailureRecognizesTCPReset(t *testing.T) {
	classification := classifyKiroTransportFailure(
		&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		newKiroTransportTrace(),
	)
	if classification.Kind != "tcp_reset" || !classification.Retryable {
		t.Fatalf("classification=%#v", classification)
	}
	if classification.Layer != "tcp_peer_or_intermediary" {
		t.Fatalf("layer=%q", classification.Layer)
	}
}

func TestGODEBUGHTTP2ClientDisabled(t *testing.T) {
	t.Setenv("GODEBUG", "x=1,http2client=0")
	if !godebugHTTP2ClientDisabled() {
		t.Fatal("http2client=0 was not detected")
	}
	t.Setenv("GODEBUG", "http2client=0,http2client=1")
	if godebugHTTP2ClientDisabled() {
		t.Fatal("last GODEBUG value should win")
	}
}

func TestParseEventStreamReportsPartialPrelude(t *testing.T) {
	err := parseEventStream(bytes.NewReader([]byte{1, 2, 3, 4, 5}), &KiroStreamCallback{})
	var streamErr *KiroStreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error type=%T want *KiroStreamError: %v", err, err)
	}
	if streamErr.Stage != kiroStreamReadPreludeCRC ||
		streamErr.ReadBytes != 5 ||
		streamErr.ExpectedBytes != 12 ||
		streamErr.WireBytes != 5 ||
		!errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error=%#v", streamErr)
	}
}

func TestParseEventStreamReportsPartialFrameBody(t *testing.T) {
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], 20) // 12-byte prelude + 8-byte remainder.
	binary.BigEndian.PutUint32(prelude[4:8], 0)
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[:8]))
	body := append(prelude, []byte{1, 2, 3}...)

	err := parseEventStream(bytes.NewReader(body), &KiroStreamCallback{})
	var streamErr *KiroStreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error type=%T want *KiroStreamError: %v", err, err)
	}
	if streamErr.Stage != kiroStreamReadFrameBody ||
		streamErr.ReadBytes != 3 ||
		streamErr.ExpectedBytes != 8 ||
		streamErr.WireBytes != 15 ||
		!errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error=%#v", streamErr)
	}
}

func TestKiroAttemptIntegrityAcceptsCleanMeteringTerminalEvidence(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*kiroAttemptCallbacks)
		wantReason string
	}{
		{
			name: "assistant content",
			prepare: func(attempt *kiroAttemptCallbacks) {
				attempt.onText("ok", false)
				attempt.onMeteringEvent()
			},
			wantReason: "end_turn",
		},
		{
			name: "tool use",
			prepare: func(attempt *kiroAttemptCallbacks) {
				attempt.onToolUse(KiroToolUse{ToolUseID: "toolu_1", Name: "lookup"})
				attempt.onMeteringEvent()
			},
			wantReason: "tool_use",
		},
		{
			name: "reasoning only",
			prepare: func(attempt *kiroAttemptCallbacks) {
				attempt.onText("reasoning", true)
				attempt.onMeteringEvent()
			},
			wantReason: "end_turn",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotReason string
			attempt := newKiroAttemptCallbacks(&KiroStreamCallback{
				OnStopReason: func(reason string) { gotReason = reason },
			})
			tc.prepare(attempt)
			if err := attempt.integrityError(); err != nil {
				t.Fatalf("integrityError: %v", err)
			}
			attempt.commitSuccess()
			if gotReason != tc.wantReason {
				t.Fatalf("stop reason=%q want=%q", gotReason, tc.wantReason)
			}
		})
	}
}

func TestKiroAttemptIntegrityRealStopReasonWinsOverMeteringFallback(t *testing.T) {
	var gotReason string
	attempt := newKiroAttemptCallbacks(&KiroStreamCallback{
		OnStopReason: func(reason string) { gotReason = reason },
	})
	attempt.onText("partial", false)
	attempt.onStopReason("max_tokens")
	attempt.onMeteringEvent()
	if err := attempt.integrityError(); err != nil {
		t.Fatalf("integrityError: %v", err)
	}
	attempt.commitSuccess()
	if gotReason != "max_tokens" {
		t.Fatalf("real upstream stop reason was replaced: %q", gotReason)
	}
}

func TestKiroAttemptIntegrityContextUsageWithoutMeteringRemainsTruncated(t *testing.T) {
	attempt := newKiroAttemptCallbacks(&KiroStreamCallback{})
	attempt.onText("partial", false)
	attempt.onContextUsage(12.5)
	if err := attempt.integrityError(); !errors.Is(err, errUpstreamTruncatedResponse) {
		t.Fatalf("integrityError=%v want truncated response", err)
	}
}

func TestKiroTransportErrorAfterMeteringStillFails(t *testing.T) {
	var calls int
	var stopReasons []string
	err := callKiroAPIContextWithRecovery(
		context.Background(),
		testKiroRecoveryAccount(),
		&KiroPayload{},
		&KiroStreamCallback{
			OnStopReason: func(reason string) { stopReasons = append(stopReasons, reason) },
		},
		func(_ context.Context, _ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
			calls++
			cb.OnText("partial", false)
			cb.OnMeteringEvent()
			return truncatedKiroStreamError(kiroStreamReadFrameBody)
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil {
		t.Fatal("transport failure after metering was accepted as success")
	}
	if calls != 1 {
		t.Fatalf("attempts=%d want=1 after visible semantic output", calls)
	}
	if len(stopReasons) != 0 {
		t.Fatalf("synthetic stop reason leaked on transport failure: %v", stopReasons)
	}
}

func TestKiroAttemptIntegrityMeteringWithoutSemanticOutputRemainsTruncated(t *testing.T) {
	attempt := newKiroAttemptCallbacks(&KiroStreamCallback{})
	attempt.onMeteringEvent()
	if err := attempt.integrityError(); !errors.Is(err, errUpstreamTruncatedResponse) {
		t.Fatalf("integrityError=%v want truncated response", err)
	}
}
