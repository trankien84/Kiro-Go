package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func FuzzCompletionGuardRetainedBudgetRelease(f *testing.F) {
	f.Add(1024, 100, 50, 50)
	f.Add(16, 16, 1, 100)
	f.Add(0, 100, 100, 100)
	f.Fuzz(func(t *testing.T, limit, first, release, second int) {
		nonNegative := func(value int) int {
			if value < 0 {
				value = -(value + 1)
			}
			return value
		}
		limit = nonNegative(limit)
		if limit > 1<<20 {
			limit %= 1 << 20
		}
		for _, value := range []*int{&first, &release, &second} {
			*value = nonNegative(*value)
			if *value > 1<<20 {
				*value %= 1 << 20
			}
		}

		budget := newCompletionGuardRetainedBudget(limit)
		_ = budget.reserve(first)
		budget.release(release)
		_ = budget.reserve(second)
		if budget.retained < 0 {
			t.Fatalf("negative retained bytes: %d", budget.retained)
		}
		if limit > 0 && budget.retained > limit {
			t.Fatalf("retained=%d exceeds limit=%d", budget.retained, limit)
		}
		if budget.Err() != nil && budget.reserve(1) {
			t.Fatal("budget accepted data after terminal limit error")
		}
	})
}

func FuzzCompletionGuardSSEFragmentation(f *testing.F) {
	f.Add([]byte{1, 2, 3, 5, 8, 13})
	f.Add([]byte{255})
	f.Fuzz(func(t *testing.T, cuts []byte) {
		stream := strings.Join([]string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}, "")
		recorder := httptest.NewRecorder()
		writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
		if !ok {
			t.Fatal("streaming writer requires flusher")
		}
		defer writer.release()

		position := 0
		cutIndex := 0
		for position < len(stream) {
			size := 1
			if len(cuts) > 0 {
				size = int(cuts[cutIndex%len(cuts)])%31 + 1
				cutIndex++
			}
			end := position + size
			if end > len(stream) {
				end = len(stream)
			}
			if _, err := writer.Write([]byte(stream[position:end])); err != nil {
				t.Fatal(err)
			}
			position = end
		}
		if err := writer.finish(); err != nil {
			t.Fatal(err)
		}
		if !writer.protocolComplete() {
			t.Fatalf("fragmented stream incomplete: %s", writer.protocolReason())
		}
		response := writer.response()
		if len(response.Content) != 1 || response.Content[0].Text != "hello" {
			t.Fatalf("response=%#v", response)
		}
	})
}

func FuzzCompletionGuardMalformedDeltaAndDuplicateIndex(f *testing.F) {
	f.Add("text_delta", "hello", false)
	f.Add("input_json_delta", `{"path":"a.go"}`, false)
	f.Add("input_json_delta", "", false)
	f.Add("input_json_delta", "0", false)
	f.Add("unknown_delta", "x", false)
	f.Add("input_json_delta", `{}`, true)
	f.Fuzz(func(t *testing.T, deltaType, payload string, duplicate bool) {
		if len(deltaType) > 64 {
			deltaType = deltaType[:64]
		}
		if len(payload) > 4096 {
			payload = payload[:4096]
		}
		recorder := httptest.NewRecorder()
		writer, ok := newCompletionGuardStreamingWriter(recorder, "claude-opus-5")
		if !ok {
			t.Fatal("streaming writer requires flusher")
		}
		frames := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read\",\"input\":{}}}\n\n",
		}
		if duplicate {
			frames = append(frames,
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			)
		} else {
			deltaFrame, err := json.Marshal(map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]interface{}{
					"type":         deltaType,
					"partial_json": payload,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, "event: content_block_delta\ndata: "+string(deltaFrame)+"\n\n")
		}
		frames = append(frames,
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		)
		var writeErr error
		for _, frame := range frames {
			if _, err := writer.Write([]byte(frame)); err != nil {
				writeErr = err
				break
			}
		}
		finishErr := writer.finish()
		var toolInput map[string]interface{}
		validToolInput := json.Unmarshal([]byte(payload), &toolInput) == nil &&
			toolInput != nil
		validByConstruction := !duplicate &&
			deltaType == "input_json_delta" &&
			validToolInput
		if validByConstruction {
			if writeErr != nil || finishErr != nil || !writer.protocolComplete() {
				t.Fatalf(
					"valid stream failed: write=%v finish=%v reason=%q payload=%q",
					writeErr,
					finishErr,
					writer.protocolReason(),
					payload,
				)
			}
		} else {
			if writer.protocolComplete() {
				t.Fatalf(
					"malformed stream was accepted: duplicate=%v delta=%q payload=%q",
					duplicate,
					deltaType,
					payload,
				)
			}
			if writer.protocolReason() == "" {
				t.Fatal("malformed stream had no protocol reason")
			}
		}
		if writer.bufferedBytes < 0 || writer.semanticBytes < 0 {
			t.Fatalf("negative accounting buffered=%d semantic=%d", writer.bufferedBytes, writer.semanticBytes)
		}
		writer.release()
		if writer.bufferedBytes != 0 || writer.semanticBytes != 0 || len(writer.openBlocks) != 0 {
			t.Fatalf(
				"release leaked parser state: buffered=%d semantic=%d open=%d",
				writer.bufferedBytes,
				writer.semanticBytes,
				len(writer.openBlocks),
			)
		}
	})
}
