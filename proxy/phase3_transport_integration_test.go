package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"kiro-go/config"
	"reflect"
	"testing"
	"time"
)

func phase3StringHeader(name, value string) []byte {
	buf := make([]byte, 0, 1+len(name)+1+2+len(value))
	buf = append(buf, byte(len(name)))
	buf = append(buf, name...)
	buf = append(buf, 7) // AWS Event Stream string header.
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(value)))
	buf = append(buf, size[:]...)
	buf = append(buf, value...)
	return buf
}

func phase3EventFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()
	headers := append(
		phase3StringHeader(":message-type", "event"),
		phase3StringHeader(":event-type", eventType)...,
	)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	totalLength := 12 + len(headers) + len(body) + 4
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[:8]))

	messageWithoutCRC := append(append([]byte{}, headers...), body...)
	checksum := crc32.Update(0, crc32.IEEETable, prelude)
	checksum = crc32.Update(checksum, crc32.IEEETable, messageWithoutCRC)
	var messageCRC [4]byte
	binary.BigEndian.PutUint32(messageCRC[:], checksum)

	frame := append([]byte{}, prelude...)
	frame = append(frame, messageWithoutCRC...)
	frame = append(frame, messageCRC[:]...)
	return frame
}

func TestPhase3StreamIntegrityClassifier(t *testing.T) {
	if err := classifyStreamIntegrity(5, 0, "end_turn", false); err != nil {
		t.Fatalf("terminal stop reason rejected: %v", err)
	}
	if err := classifyStreamIntegrity(0, 1, "", false); err != nil {
		t.Fatalf("tool call without stop reason rejected: %v", err)
	}
	for name, err := range map[string]error{
		"partial text": classifyStreamIntegrity(5, 0, "", false),
		"reasoning":    classifyStreamIntegrity(0, 0, "", true),
		"empty":        classifyStreamIntegrity(0, 0, "", false),
	} {
		if !errors.Is(err, errUpstreamTruncatedResponse) {
			t.Fatalf("%s: error=%v want errUpstreamTruncatedResponse", name, err)
		}
	}
}

func TestPhase3MajorOnlyClaudeFiveUsesLargeContext(t *testing.T) {
	if !isLargeContextModel("claude-opus-5") {
		t.Fatal("major-only claude-opus-5 must use the 1M context class")
	}
	if isLargeContextModel("claude-sonnet-4") {
		t.Fatal("major-only claude-sonnet-4 must remain in the 200K context class")
	}
	if !isLargeContextModel("claude-sonnet-4-6") {
		t.Fatal("claude-sonnet-4-6 must use the 1M context class")
	}
}

func TestPhase3ValidatedTerminalCallbacksFlushInOrder(t *testing.T) {
	var order []string
	callback := &KiroStreamCallback{
		OnText:       func(text string, _ bool) { order = append(order, "text:"+text) },
		OnStopReason: func(reason string) { order = append(order, "stop:"+reason) },
		OnComplete:   func(_, _ int) { order = append(order, "complete") },
	}
	err := callKiroAPIContextWithRecovery(
		context.Background(),
		testKiroRecoveryAccount(),
		&KiroPayload{},
		callback,
		func(_ context.Context, _ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
			cb.OnText("hello", false)
			cb.OnStopReason("end_turn")
			cb.OnComplete(1, 2)
			return nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"text:hello", "stop:end_turn", "complete"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("callback order=%v want=%v", order, want)
	}
}

func TestPhase3VisibleTruncationNeverCompletesOrReplays(t *testing.T) {
	var calls, completes int
	callback := &KiroStreamCallback{
		OnText:     func(string, bool) {},
		OnComplete: func(_, _ int) { completes++ },
	}
	err := callKiroAPIContextWithRecovery(
		context.Background(),
		testKiroRecoveryAccount(),
		&KiroPayload{},
		callback,
		func(_ context.Context, _ *config.Account, _ *KiroPayload, cb *KiroStreamCallback) error {
			calls++
			cb.OnText("partial", false)
			cb.OnComplete(1, 1)
			return nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	var transportErr *KiroTransportError
	if !errors.As(err, &transportErr) || !transportErr.SemanticEmitted {
		t.Fatalf("error=%v want semantic KiroTransportError", err)
	}
	if calls != 1 {
		t.Fatalf("attempts=%d want=1", calls)
	}
	if completes != 0 {
		t.Fatalf("normal completion leaked for truncated stream: %d", completes)
	}
}

func TestPhase3BufferedCallerCanResetAndRetrySemanticTruncation(t *testing.T) {
	var calls, resets, completes int
	var buffered []string
	callback := &KiroStreamCallback{
		OnText:     func(text string, _ bool) { buffered = append(buffered, text) },
		OnComplete: func(_, _ int) { completes++ },
		CanRetry:   func() bool { return true },
		ResetForRetry: func() {
			resets++
			buffered = nil
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
				cb.OnText("partial", false)
				cb.OnComplete(1, 1)
				return nil
			}
			cb.OnText("complete", false)
			cb.OnStopReason("end_turn")
			cb.OnComplete(2, 3)
			return nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || resets != 1 || completes != 1 {
		t.Fatalf("calls=%d resets=%d completes=%d", calls, resets, completes)
	}
	if !reflect.DeepEqual(buffered, []string{"complete"}) {
		t.Fatalf("buffered=%v want=[complete]", buffered)
	}
}

func TestPhase3EventStreamValidatesCRCAndStopReason(t *testing.T) {
	stream := append(
		phase3EventFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		phase3EventFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"})...,
	)
	var text, stop string
	var completed int
	err := parseEventStream(bytes.NewReader(stream), &KiroStreamCallback{
		OnText:       func(value string, _ bool) { text += value },
		OnStopReason: func(value string) { stop = value },
		OnComplete:   func(_, _ int) { completed++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello" || stop != "end_turn" || completed != 1 {
		t.Fatalf("text=%q stop=%q completed=%d", text, stop, completed)
	}

	bad := phase3EventFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "x"})
	bad[8] ^= 0xff
	if err := parseEventStream(bytes.NewReader(bad), nil); !errors.Is(err, errInvalidKiroEventStream) {
		t.Fatalf("bad prelude CRC error=%v", err)
	}
}

func TestPhase3ParallelToolFragmentsStayIndependent(t *testing.T) {
	frames := [][]byte{
		phase3EventFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "t1", "name": "one", "input": `{"x":`}),
		phase3EventFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "t2", "name": "two", "input": `{"y":`}),
		phase3EventFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "t1", "input": `1}`, "stop": true}),
		phase3EventFrame(t, "toolUseEvent", map[string]interface{}{"toolUseId": "t2", "input": `2}`, "stop": true}),
		phase3EventFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}),
	}
	var stream []byte
	for _, frame := range frames {
		stream = append(stream, frame...)
	}
	var calls []KiroToolUse
	if err := parseEventStream(bytes.NewReader(stream), &KiroStreamCallback{
		OnToolUse: func(tool KiroToolUse) { calls = append(calls, tool) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls=%#v", calls)
	}
	if calls[0].ToolUseID != "t1" || calls[0].Name != "one" || calls[0].Input["x"] != float64(1) {
		t.Fatalf("first tool=%#v", calls[0])
	}
	if calls[1].ToolUseID != "t2" || calls[1].Name != "two" || calls[1].Input["y"] != float64(2) {
		t.Fatalf("second tool=%#v", calls[1])
	}
}

func TestPhase3IncompleteToolInputIsRetryableIntegrityFailure(t *testing.T) {
	stream := phase3EventFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "t1",
		"name":      "one",
		"input":     `{"x":`,
	})
	err := parseEventStream(bytes.NewReader(stream), &KiroStreamCallback{OnToolUse: func(KiroToolUse) {}})
	if !errors.Is(err, errIncompleteKiroToolInput) {
		t.Fatalf("error=%v want errIncompleteKiroToolInput", err)
	}
	classification := classifyKiroTransportFailure(err, newKiroTransportTrace())
	if !classification.Retryable || classification.Kind != "incomplete_tool_input" {
		t.Fatalf("classification=%#v", classification)
	}
}
