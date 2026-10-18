package proxy

import (
	"bytes"
	"testing"
)

func TestParseEventStreamReportsMeteringEvent(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "ok",
	}))
	stream.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
		"contextUsagePercentage": 2.0,
	}))
	stream.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
		"usage": 0.0173,
	}))

	var text string
	var meteringEvents int
	var stopReasons []string
	err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
		OnText: func(chunk string, _ bool) {
			text += chunk
		},
		OnMeteringEvent: func() {
			meteringEvents++
		},
		OnStopReason: func(reason string) {
			stopReasons = append(stopReasons, reason)
		},
	})
	if err != nil {
		t.Fatalf("parseEventStream: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text=%q want=ok", text)
	}
	if meteringEvents != 1 {
		t.Fatalf("metering events=%d want=1", meteringEvents)
	}
	if len(stopReasons) != 0 {
		t.Fatalf("parser must not invent terminal semantics, got stop reasons=%v", stopReasons)
	}
}
