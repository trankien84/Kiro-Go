package proxy

import (
	"strings"
	"testing"
)

func TestStreamTextDecoderDeltaPreservesVietnamesePrefixCollision(t *testing.T) {
	decoder := newStreamTextDecoder(streamTextModeDelta)
	chunks := []string{" đọc file", " đ", "ó"}

	var output strings.Builder
	for _, chunk := range chunks {
		output.WriteString(decoder.Push(chunk).Text)
	}

	if got, want := output.String(), " đọc file đó"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStreamTextDecoderDeltaPreservesRepeatedChunks(t *testing.T) {
	decoder := newStreamTextDecoder(streamTextModeDelta)
	chunks := []string{"ha", "ha", "!"}

	var output strings.Builder
	for _, chunk := range chunks {
		output.WriteString(decoder.Push(chunk).Text)
	}

	if got, want := output.String(), "haha!"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStreamTextDecoderCumulativeProgression(t *testing.T) {
	decoder := newStreamTextDecoder(streamTextModeCumulative)
	chunks := []string{"Mình", "Mình đã", "Mình đã đọc"}

	var output strings.Builder
	for _, chunk := range chunks {
		output.WriteString(decoder.Push(chunk).Text)
	}

	if got, want := output.String(), "Mình đã đọc"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStreamTextDecoderCumulativeIgnoresExactReplay(t *testing.T) {
	decoder := newStreamTextDecoder(streamTextModeCumulative)

	if got := decoder.Push("thinking").Text; got != "thinking" {
		t.Fatalf("expected first snapshot, got %q", got)
	}
	if got := decoder.Push("thinking").Text; got != "" {
		t.Fatalf("expected exact replay to be ignored, got %q", got)
	}
}

func TestStreamTextDecoderCumulativeViolationFallsBackWithoutDroppingChunk(t *testing.T) {
	decoder := newStreamTextDecoder(streamTextModeCumulative)
	_ = decoder.Push(" đọc file")

	result := decoder.Push(" đ")
	if !result.ContractViolation {
		t.Fatal("expected cumulative contract violation")
	}
	if result.ModeBefore != streamTextModeCumulative || result.ModeAfter != streamTextModeDelta {
		t.Fatalf("unexpected mode transition: %s -> %s", result.ModeBefore, result.ModeAfter)
	}
	if got, want := result.Text, " đ"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if got, want := decoder.Push("ó").Text, "ó"; got != want {
		t.Fatalf("delta fallback got %q, want %q", got, want)
	}
}

func TestStreamTextDecoderLegacyPreservesPreviousBehavior(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		chunk    string
		want     string
		wantPrev string
	}{
		{
			name:     "cumulative progression",
			previous: "abc",
			chunk:    "abcde",
			want:     "de",
			wantPrev: "abcde",
		},
		{
			name:     "prefix rewind",
			previous: "abcde",
			chunk:    "abc",
			want:     "",
			wantPrev: "abcde",
		},
		{
			name:     "suffix overlap",
			previous: "hello world",
			chunk:    "world!!!",
			want:     "!!!",
			wantPrev: "world!!!",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoder := newStreamTextDecoder(streamTextModeLegacy)
			decoder.previous = tt.previous

			result := decoder.Push(tt.chunk)
			if result.Text != tt.want {
				t.Fatalf("got %q, want %q", result.Text, tt.want)
			}
			if decoder.previous != tt.wantPrev {
				t.Fatalf("previous got %q, want %q", decoder.previous, tt.wantPrev)
			}
		})
	}
}

func TestConfiguredStreamTextMode(t *testing.T) {
	tests := []struct {
		value string
		want  streamTextMode
	}{
		{"delta", streamTextModeDelta},
		{"raw", streamTextModeDelta},
		{"cumulative", streamTextModeCumulative},
		{"snapshot", streamTextModeCumulative},
		{"legacy", streamTextModeLegacy},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("KIRO_TEST_CHUNK_MODE", tt.value)
			got, invalid := configuredStreamTextMode("KIRO_TEST_CHUNK_MODE", streamTextModeDelta)
			if invalid != "" {
				t.Fatalf("unexpected invalid value %q", invalid)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfiguredStreamTextModeInvalidUsesFallback(t *testing.T) {
	t.Setenv("KIRO_TEST_CHUNK_MODE", "unknown-mode")

	got, invalid := configuredStreamTextMode("KIRO_TEST_CHUNK_MODE", streamTextModeCumulative)
	if got != streamTextModeCumulative {
		t.Fatalf("got %q, want fallback %q", got, streamTextModeCumulative)
	}
	if invalid != "unknown-mode" {
		t.Fatalf("unexpected invalid value %q", invalid)
	}
}

func FuzzStreamTextDecoderDeltaNeverDropsInput(f *testing.F) {
	f.Add(" đọc file", " đ")
	f.Add("ha", "ha")
	f.Add("日本", "日本語")
	f.Add("🙂", "🙂")

	f.Fuzz(func(t *testing.T, first, second string) {
		decoder := newStreamTextDecoder(streamTextModeDelta)
		got := decoder.Push(first).Text + decoder.Push(second).Text
		want := first + second
		if got != want {
			t.Fatalf("delta decoder changed input: got %q, want %q", got, want)
		}
	})
}
