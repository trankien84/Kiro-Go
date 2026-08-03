package proxy

import (
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"testing"
)

// TestParseEventStreamRejectsOversizedTotalLength verifies parseEventStream
// rejects a frame whose totalLength exceeds the sanity cap BEFORE the
// make([]byte, remaining) allocation. A corrupt/malicious (or bit-flipped)
// 32-bit totalLength near 2^32 would otherwise drive a multi-GB allocation:
// either an alloc panic (net/http recovers per-request → connection dropped
// with no terminal event → client hang) or a multi-GB buffer held to the 5-min
// client timeout (memory-pressure DoS). The cap keeps the test itself from
// allocating gigabytes: it feeds totalLength = cap+1.
func TestParseEventStreamRejectsOversizedTotalLength(t *testing.T) {
	over := maxEventStreamMessageBytes + 1
	prelude := make([]byte, 12) // prelude: total_len(4) + headers_len(4) + crc(4)
	prelude[0] = byte(over >> 24)
	prelude[1] = byte(over >> 16)
	prelude[2] = byte(over >> 8)
	prelude[3] = byte(over)
	// headers_len bytes (4-7) and crc (8-11) left zero.

	err := parseEventStream(bytes.NewReader(prelude), &KiroStreamCallback{})
	if !errors.Is(err, errEventStreamFrameTooLarge) {
		t.Fatalf("oversized totalLength must be rejected before the multi-GB make; got %v", err)
	}
}

// closeTrackingBody is an io.ReadCloser whose Close() is observable, for tests
// that assert an upstream body was returned to the transport pool.
type closeTrackingBody struct {
	data   []byte
	offset int
	closed bool
}

func (b *closeTrackingBody) Read(p []byte) (int, error) {
	if b.offset >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.offset:])
	b.offset += n
	return n, nil
}

func (b *closeTrackingBody) Close() error { b.closed = true; return nil }

// buildAssistantResponseEvent builds a minimal AWS event-stream frame carrying
// an assistantResponseEvent with the given content, so a test can drive
// parseEventStream's OnText callback without a live upstream. Both CRC32 fields
// are computed for real: the parser verifies the prelude CRC and the message CRC
// and rejects the frame before dispatch if either is wrong.
func buildAssistantResponseEvent(content string) []byte {
	payload := []byte(`{"content":"` + content + `"}`)
	const eventType = "assistantResponseEvent"
	// Header: nameLen(1) + ":event-type"(11) + type=7 String(1) + valueLen(2 BE) + value
	var h bytes.Buffer
	h.WriteByte(11)
	h.WriteString(":event-type")
	h.WriteByte(7)
	h.WriteByte(byte(len(eventType) >> 8))
	h.WriteByte(byte(len(eventType)))
	h.WriteString(eventType)
	headers := h.Bytes()
	totalLength := 12 + len(headers) + len(payload) + 4

	// Prelude: total_len(4) + headers_len(4) + crc32(first 8 bytes).
	prelude := make([]byte, 12)
	prelude[0] = byte(totalLength >> 24)
	prelude[1] = byte(totalLength >> 16)
	prelude[2] = byte(totalLength >> 8)
	prelude[3] = byte(totalLength)
	prelude[4] = byte(len(headers) >> 24)
	prelude[5] = byte(len(headers) >> 16)
	prelude[6] = byte(len(headers) >> 8)
	prelude[7] = byte(len(headers))
	preludeCRC := crc32.ChecksumIEEE(prelude[:8])
	prelude[8] = byte(preludeCRC >> 24)
	prelude[9] = byte(preludeCRC >> 16)
	prelude[10] = byte(preludeCRC >> 8)
	prelude[11] = byte(preludeCRC)

	// Message CRC covers the full prelude plus headers+payload.
	body := append(append([]byte{}, headers...), payload...)
	messageCRC := eventStreamChecksum(prelude, body)

	var f bytes.Buffer
	f.Write(prelude)
	f.Write(body)
	f.WriteByte(byte(messageCRC >> 24))
	f.WriteByte(byte(messageCRC >> 16))
	f.WriteByte(byte(messageCRC >> 8))
	f.WriteByte(byte(messageCRC))
	return f.Bytes()
}

// TestParseAndStreamClosesBodyOnCallbackPanic verifies parseAndStream returns
// the upstream body to the transport pool (resp.Body.Close()) even when a
// streaming callback panics. The 200 path's old code was
// `err = parseEventStream(resp.Body, callback); resp.Body.Close()` — a plain
// statement, so a panic in OnText/OnToolUse unwound past it and leaked the TCP
// connection (repeated panics exhaust MaxIdleConnsPerHost=20 / FDs). The defer
// in parseAndStream closes on every path including panic.
func TestParseAndStreamClosesBodyOnCallbackPanic(t *testing.T) {
	body := &closeTrackingBody{data: buildAssistantResponseEvent("hello")}
	cb := &KiroStreamCallback{
		OnText: func(string, bool) { panic("callback boom") },
	}
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_ = parseAndStream(body, cb)
	}()
	if !panicked {
		t.Fatal("test setup: OnText did not fire/panic — the fixture did not reach the callback; fix buildAssistantResponseEvent")
	}
	if !body.closed {
		t.Fatal("parseAndStream must close the upstream body even when a callback panics, or the TCP connection leaks / the idle pool exhausts under repeated panics")
	}
}
