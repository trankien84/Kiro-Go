package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestParseEventStreamDefaultsToAssistantDelta(t *testing.T) {
	t.Setenv(assistantChunkModeEnv, "")
	t.Setenv(reasoningChunkModeEnv, "")

	chunks := []string{" đọc file", " đ", "ó"}
	frames := make([][]byte, 0, len(chunks))
	for _, chunk := range chunks {
		frames = append(frames, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": chunk,
		}))
	}

	var output strings.Builder
	err := parseEventStream(bytes.NewReader(bytes.Join(frames, nil)), &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				t.Fatalf("unexpected thinking chunk %q", text)
			}
			output.WriteString(text)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if got, want := output.String(), " đọc file đó"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEventStreamPreservesRepeatedAssistantDelta(t *testing.T) {
	t.Setenv(assistantChunkModeEnv, "delta")

	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ha"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ha"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "!"}),
	}, nil))

	var output strings.Builder
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				t.Fatalf("unexpected thinking chunk %q", text)
			}
			output.WriteString(text)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if got, want := output.String(), "haha!"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEventStreamSupportsExplicitCumulativeAssistantMode(t *testing.T) {
	t.Setenv(assistantChunkModeEnv, "cumulative")

	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "Mình"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "Mình đã"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "Mình đã đọc"}),
	}, nil))

	var output strings.Builder
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				t.Fatalf("unexpected thinking chunk %q", text)
			}
			output.WriteString(text)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if got, want := output.String(), "Mình đã đọc"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEventStreamCumulativeContractViolationDoesNotDropAssistantText(t *testing.T) {
	t.Setenv(assistantChunkModeEnv, "cumulative")

	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": " đọc file"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": " đ"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ó"}),
	}, nil))

	var output strings.Builder
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				t.Fatalf("unexpected thinking chunk %q", text)
			}
			output.WriteString(text)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if got, want := output.String(), " đọc file đó"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEventStreamFinishesPendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "mcpIdaProMcpStatus",
		"input":     `{"server":"ida-pro-mcp"}`,
	}))

	var toolUses []KiroToolUse
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
		OnComplete: func(_, _ int) {
			completed = true
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !completed {
		t.Fatalf("expected stream completion callback")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected pending tool use to be emitted on EOF, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_1" || toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool use: %#v", toolUses[0])
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected parsed tool input, got %#v", toolUses[0].Input)
	}
}

func TestParseEventStreamNilCallbackIsNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpIdaProMcpStatus",
			"input": `{"server":"ida-pro-mcp"}`,
			"stop":  true,
		}),
	}, nil))

	if err := parseEventStream(stream, nil); err != nil {
		t.Fatalf("expected nil callback to be a no-op, got %v", err)
	}
}

func TestParseEventStreamNilCallbackFieldsAreNoOp(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	}))

	if err := parseEventStream(stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected empty callback to be a no-op, got %v", err)
	}
}

func TestHandleToolUseEventGeneratesMissingToolUseID(t *testing.T) {
	var toolUses []KiroToolUse
	pending := &pendingToolUses{}
	err := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":"ida-pro-mcp"}`,
		"stop":  true,
	}, pending, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	if err != nil {
		t.Fatalf("unexpected tool use error: %v", err)
	}

	if len(pending.order) != 0 {
		t.Fatalf("expected stopped tool use to clear pending state, got %d", len(pending.order))
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID == "" {
		t.Fatalf("expected generated tool use id")
	}
	if toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool name: %q", toolUses[0].Name)
	}
}

func TestHandleToolUseEventReplacesGeneratedIDWhenRealIDArrives(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}
	pending := &pendingToolUses{}

	if err := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":`,
	}, pending, callback); err != nil {
		t.Fatalf("unexpected first tool fragment error: %v", err)
	}
	if err := handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_real",
		"name":      "mcpIdaProMcpStatus",
		"input":     `"ida-pro-mcp"}`,
		"stop":      true,
	}, pending, callback); err != nil {
		t.Fatalf("unexpected completed tool error: %v", err)
	}

	if len(pending.order) != 0 {
		t.Fatalf("expected stopped tool use to clear pending state, got %d", len(pending.order))
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_real" {
		t.Fatalf("expected real tool id to replace generated id, got %q", toolUses[0].ToolUseID)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected joined tool input, got %#v", toolUses[0].Input)
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	if os.Getenv("KIRO_TEST_ENV_PROXY_HELPER") == "1" {
		transport := buildKiroTransport("")
		req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

		got, err := transport.Proxy(req)
		if err != nil {
			t.Fatalf("unexpected proxy error: %v", err)
		}
		assertProxyURL(t, got, "http://env-proxy.local:2323")
		return
	}

	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
			continue
		}
		env = append(env, item)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildKiroTransportFallsBackToEnvironmentProxy$")
	cmd.Env = append(env,
		"KIRO_TEST_ENV_PROXY_HELPER=1",
		"HTTPS_PROXY=http://env-proxy.local:2323",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("environment proxy subprocess failed: %v\n%s", err, output)
	}
}

func TestInitKiroHttpClientKeepsSeparateRestTimeout(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 0 {
		t.Fatalf(
			"streaming client must not impose a wall-clock timeout, got %s",
			streamClient.Timeout,
		)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
}

func TestSetPayloadProfileArnForAccountUsesAccountArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/stale"}

	setPayloadProfileArnForAccount(payload, &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/current "})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/current" {
		t.Fatalf("expected current account profile ARN, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountPreservesExplicitPayloadArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: " arn:aws:codewhisperer:profile/explicit "}

	setPayloadProfileArnForAccount(payload, &config.Account{})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/explicit" {
		t.Fatalf("expected explicit payload profile ARN to be preserved, got %q", payload.ProfileArn)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}

func awsEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()
	return awsEventStreamFrameWithHeaders(t, map[string]string{
		":event-type": eventType,
	}, payload)
}

func awsEventStreamFrameWithHeaders(t *testing.T, headers map[string]string, payload map[string]interface{}) []byte {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	headerBytes := make([]byte, 0)
	for name, value := range headers {
		headerBytes = append(headerBytes, byte(len(name)))
		headerBytes = append(headerBytes, name...)
		headerBytes = append(headerBytes, byte(7))
		headerBytes = append(headerBytes, byte(len(value)>>8), byte(len(value)))
		headerBytes = append(headerBytes, value...)
	}

	totalLength := 12 + len(headerBytes) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headerBytes)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headerBytes...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}

// TestCallKiroAPIClassifiesByStatusCode verifies the error CallKiroAPI builds
// from a non-200 upstream response classifies correctly downstream: 401/403 →
// auth failure (digit-boundary safe), 402 → overage (NOT auth), and a
// suspension marker in the body → suspension. CallKiroAPI delegates error
// construction to upstreamError.
func TestCallKiroAPIClassifiesByStatusCode(t *testing.T) {
	// 401 / 403 → auth failure, even when the body carries unrelated digits.
	if !pool.IsAuthFailure(upstreamError(401, "primary", "request req_999 failed")) {
		t.Fatal("401 should classify as auth failure")
	}
	if !pool.IsAuthFailure(upstreamError(403, "primary", "unrelated body")) {
		t.Fatal("403 should classify as auth failure")
	}
	// 402 → overage, NOT auth.
	e402 := upstreamError(402, "primary", "Usage limit exceeded")
	if pool.IsAuthFailure(e402) {
		t.Fatal("402 must NOT classify as auth failure")
	}
	if !isOverageErrorMessage(e402.Error()) {
		t.Fatal("402 should classify as overage")
	}
	// Suspension signalled in the body of a 403 still classifies as suspension.
	if !pool.IsSuspensionError(upstreamError(403, "primary", "TEMPORARILY_SUSPENDED")) {
		t.Fatal("suspension body should classify as suspension")
	}
}

// truncatedReader yields the given bytes then fails, simulating an upstream
// connection that dies mid-stream.
type truncatedReader struct {
	data []byte
	pos  int
	err  error
}

func (r *truncatedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// Regression: an assistant stream whose chunks legitimately repeat must be reassembled
// verbatim. The previous content-based de-duplication turned these exact inputs into
// "666" and "abab" respectively, silently corrupting model output.
func TestParseEventStreamAssistantRepeatedContentIsNotDropped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"repeated equal chunks", []string{"666", "666", "666", "6"}, "6666666666"},
		{"repeated period", []string{"abab", "abab"}, "abababab"},
		{"chunk equal to previous", []string{"ha", "ha", "ha"}, "hahaha"},
		{"prefix shaped chunks", []string{"6", "66"}, "666"},
		{"non repeating control", []string{"123", "4567890"}, "1234567890"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stream bytes.Buffer
			for _, c := range tc.chunks {
				stream.Write(awsEventStreamFrame(t, "assistantResponseEvent",
					map[string]interface{}{"content": c}))
			}

			var got string
			err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
				OnText: func(text string, reasoning bool) {
					if !reasoning {
						got += text
					}
				},
			})
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("assistant text corrupted: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseEventStreamTrackedRejectsInvalidOrUpstreamErrorFrames(t *testing.T) {
	validText := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "partial answer",
	})
	corruptFrame := append([]byte(nil), awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "corrupt",
	})...)
	corruptFrame[len(corruptFrame)-5] ^= 1

	upstreamError := awsEventStreamFrameWithHeaders(t, map[string]string{
		":message-type": "exception",
		":event-type":   "error",
	}, map[string]interface{}{
		"message": "upstream denied request",
	})

	for _, tc := range []struct {
		name   string
		stream []byte
	}{
		{name: "message CRC mismatch", stream: append(validText, corruptFrame...)},
		{name: "upstream exception", stream: append(validText, upstreamError...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var text string
			var completed int
			emitted, err := parseEventStreamTracked(bytes.NewReader(tc.stream), &KiroStreamCallback{
				OnText:     func(chunk string, _ bool) { text += chunk },
				OnComplete: func(int, int) { completed++ },
			})
			if err == nil {
				t.Fatal("expected invalid upstream frame to fail parsing")
			}
			if !emitted || text != "partial answer" {
				t.Fatalf("expected the first text frame to have been emitted, emitted=%v text=%q", emitted, text)
			}
			if completed != 0 {
				t.Fatalf("failed stream must not complete, got %d completion callbacks", completed)
			}
		})
	}
}

// The reasoning stream is passed through verbatim too: it carries the same pure
// incremental deltas as the assistant stream, and de-duplicating it dropped
// legitimate repeated text in exactly the same way.
func TestParseEventStreamReasoningRepeatedContentIsNotDropped(t *testing.T) {
	var stream bytes.Buffer
	for _, c := range []string{"666", "666", "666", "6"} {
		stream.Write(awsEventStreamFrame(t, "reasoningContentEvent",
			map[string]interface{}{"text": c}))
	}

	var got string
	err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
		OnText: func(text string, reasoning bool) {
			if reasoning {
				got += text
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if got != "6666666666" {
		t.Fatalf("reasoning text corrupted: got %q, want %q", got, "6666666666")
	}
}

func TestParseEventStreamPreservesInterleavedToolInputs(t *testing.T) {
	var stream bytes.Buffer
	for _, event := range []map[string]interface{}{
		{"toolUseId": "toolu_a", "name": "lookup", "input": `{"query":"a`},
		{"toolUseId": "toolu_b", "name": "lookup", "input": `{"query":"b`},
		{"toolUseId": "toolu_a", "name": "lookup", "input": `1"}`, "stop": true},
		{"toolUseId": "toolu_b", "name": "lookup", "input": `2"}`, "stop": true},
	} {
		stream.Write(awsEventStreamFrame(t, "toolUseEvent", event))
	}

	var toolUses []KiroToolUse
	err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(toolUses) != 2 {
		t.Fatalf("tool uses=%d, want 2 (%#v)", len(toolUses), toolUses)
	}
	got := map[string]string{}
	for _, toolUse := range toolUses {
		got[toolUse.ToolUseID], _ = toolUse.Input["query"].(string)
	}
	if got["toolu_a"] != "a1" || got["toolu_b"] != "b2" {
		t.Fatalf("interleaved inputs=%#v, want toolu_a=a1 and toolu_b=b2", got)
	}
}

func TestParseEventStreamTrackedReportsNoEmissionOnEarlyFailure(t *testing.T) {
	// Stream dies before any frame completes: nothing reached the client.
	stream := &truncatedReader{data: []byte{0, 0, 1}, err: io.ErrUnexpectedEOF}

	var emittedText int
	emitted, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText:    func(string, bool) { emittedText++ },
		OnToolUse: func(KiroToolUse) { emittedText++ },
	})
	if err == nil {
		t.Fatalf("expected a truncation error")
	}
	if emitted {
		t.Fatalf("expected emitted=false when the stream died before any output")
	}
	if emittedText != 0 {
		t.Fatalf("expected no callbacks, got %d", emittedText)
	}
}

func TestParseEventStreamTrackedReportsEmissionAfterText(t *testing.T) {
	// One complete text frame, then the connection dies.
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "partial answer",
	})
	stream := &truncatedReader{
		data: append(frame, 0, 0, 1),
		err:  io.ErrUnexpectedEOF,
	}

	var got string
	emitted, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(text string, _ bool) { got += text },
	})
	if err == nil {
		t.Fatalf("expected a truncation error")
	}
	if !emitted {
		t.Fatalf("expected emitted=true once text reached the client")
	}
	if got != "partial answer" {
		t.Fatalf("expected the partial text to have been delivered, got %q", got)
	}
}

func TestParseEventStreamTrackedReportsEmissionOnCleanStream(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "done",
	}))

	emitted, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !emitted {
		t.Fatalf("expected emitted=true for a clean parser stream")
	}
}

func TestParseEventStreamTrackedThinkingCountsAsEmission(t *testing.T) {
	frame := awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
		"text": "let me think",
	})
	stream := &truncatedReader{data: append(frame, 0, 0, 1), err: io.ErrUnexpectedEOF}

	emitted, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	if err == nil {
		t.Fatalf("expected a truncation error")
	}
	if !emitted {
		t.Fatalf("thinking text is client-visible, so it must count as emitted")
	}
}

func TestParseEventStreamTrackedRejectsIncompleteToolOnCleanEOF(t *testing.T) {
	frame := awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_partial",
		"name":      "lookup",
		"input":     `{"query":`,
	})

	var toolUses, completed int
	emitted, err := parseEventStreamTracked(bytes.NewReader(frame), &KiroStreamCallback{
		OnToolUse:  func(KiroToolUse) { toolUses++ },
		OnComplete: func(int, int) { completed++ },
	})
	if !errors.Is(err, errIncompleteKiroToolInput) {
		t.Fatalf("expected incomplete tool error, got %v", err)
	}
	if emitted {
		t.Fatal("an incomplete tool use that was not delivered must not count as emitted")
	}
	if toolUses != 0 || completed != 0 {
		t.Fatalf("toolUses=%d completed=%d, want no callbacks", toolUses, completed)
	}
}

func TestSetPayloadProfileArnForAccountClearsAPIKeyProfile(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:us-east-1:123:profile/STALE"}
	setPayloadProfileArnForAccount(payload, &config.Account{
		AuthMethod: "api_key",
		KiroApiKey: "ksk_test",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:123:profile/STALE",
	})
	if payload.ProfileArn != "" {
		t.Fatalf("expected empty profileArn for API key account, got %q", payload.ProfileArn)
	}
}

func TestEndpointsForAccountUsesCLIForAPIKey(t *testing.T) {
	eps := endpointsForAccount(&config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_x"})
	if len(eps) != 1 || eps[0].Name != "Kiro CLI" {
		t.Fatalf("expected single CLI endpoint, got %+v", eps)
	}
	if eps[0].Origin != "KIRO_CLI" {
		t.Fatalf("origin = %q", eps[0].Origin)
	}
	if got := cliRuntimeURL(&config.Account{Region: "eu-central-1"}); got != "https://runtime.eu-central-1.kiro.dev/" {
		t.Fatalf("cli url = %q", got)
	}
}

func TestParseEventStreamForwardsReasoningSignature(t *testing.T) {
	const want = "provider-signature"
	stream := bytes.NewReader(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
		"signature": want,
	}))

	var got string
	if err := parseEventStream(stream, &KiroStreamCallback{
		OnReasoningSignature: func(signature string) {
			got = signature
		},
	}); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if got != want {
		t.Fatalf("got signature %q, want %q", got, want)
	}
}

func TestCallKiroAPIContextCancelsUpstreamRequest(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		close(started)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer server.Close()
	defer server.CloseClientConnections()

	oldEndpoints := kiroEndpoints
	oldClient := kiroHttpStore.Load()
	kiroEndpoints = []kiroEndpoint{
		{URL: server.URL, Origin: "AI_EDITOR", Name: "test-kiro"},
		{URL: server.URL, Origin: "AI_EDITOR", Name: "test-codewhisperer"},
		{URL: server.URL, Origin: "AI_EDITOR", Name: "test-amazonq"},
	}
	kiroHttpStore.Store(server.Client())
	t.Cleanup(func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
	})

	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test"}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- CallKiroAPIContext(ctx, &config.Account{AuthMethod: "social", AccessToken: "token"}, payload, &KiroStreamCallback{})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Kiro request did not reach upstream")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CallKiroAPIContext error=%v", err)
		}
	case <-time.After(2 * time.Second):
		server.CloseClientConnections()
		t.Fatal("Kiro request did not stop after cancellation")
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		server.CloseClientConnections()
		t.Fatal("upstream request context was not canceled")
	}
}
