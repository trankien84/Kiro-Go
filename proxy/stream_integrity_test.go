package proxy

import "testing"

// classifyStreamIntegrity is the completeness rule: a stream that returned no
// transport error is still incomplete when it carries no terminal signal.
// A stopReason of any value, or a delivered tool call, means complete.
//
// The reasoning-only case deliberately differs from Kiro IDE, which treats it
// as complete. See classifyStreamIntegrity's doc comment for why this proxy is
// stricter.
func TestClassifyStreamIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		content      int
		tools        int
		stopReason   string
		sawReasoning bool
		wantErr      error
	}{
		{"complete with stop", 12, 0, "end_turn", false, nil},
		{"complete with tools", 0, 1, "", false, nil},
		{"complete with tools despite content", 12, 1, "", false, nil},
		{"truncated content", 8, 0, "", false, errUpstreamTruncatedResponse},
		{"reasoning only stricter than ide", 0, 0, "", true, errUpstreamTruncatedResponse},
		{"no signal at all", 0, 0, "", false, errUpstreamTruncatedResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStreamIntegrity(tc.content, tc.tools, tc.stopReason, tc.sawReasoning)
			if tc.wantErr == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if got == nil || got.Error() != tc.wantErr.Error() {
				t.Fatalf("got %v, want %v", got, tc.wantErr)
			}
			if !isStreamIntegrityError(got) {
				t.Fatalf("%v must be recognized as a stream integrity error", got)
			}
		})
	}
}
