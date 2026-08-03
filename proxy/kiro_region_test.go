package proxy

import (
	"kiro-go/config"
	"testing"
)

// TestRegionalizeURLForRegion asserts that a non-us-east-1 region collapses BOTH
// hardcoded us-east-1 hosts (q.* and codewhisperer.*) onto q.{region} — there is no
// codewhisperer.{region} host — and that us-east-1/empty are no-ops.
func TestRegionalizeURLForRegion(t *testing.T) {
	cases := []struct {
		name   string
		rawURL string
		region string
		want   string
	}{
		{
			name:   "codewhisperer host to q.eu-central-1",
			rawURL: "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
			region: "eu-central-1",
			want:   "https://q.eu-central-1.amazonaws.com/ListAvailableProfiles",
		},
		{
			name:   "q host to q.eu-central-1",
			rawURL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
			region: "eu-central-1",
			want:   "https://q.eu-central-1.amazonaws.com/generateAssistantResponse",
		},
		{
			name:   "us-east-1 is a no-op (codewhisperer host kept)",
			rawURL: "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
			region: "us-east-1",
			want:   "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
		},
		{
			name:   "empty region is a no-op",
			rawURL: "https://q.us-east-1.amazonaws.com/x",
			region: "",
			want:   "https://q.us-east-1.amazonaws.com/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := regionalizeURLForRegion(tc.rawURL, tc.region)
			if got != tc.want {
				t.Fatalf("regionalizeURLForRegion(%q, %q) = %q, want %q", tc.rawURL, tc.region, got, tc.want)
			}
		})
	}
}

// TestRegionalizeURLForRegionNoCodewhispererRegionalHost guards the user-stated
// invariant directly: a regionalized URL must never produce codewhisperer.{region}.
func TestRegionalizeURLForRegionNoCodewhispererRegionalHost(t *testing.T) {
	got := regionalizeURLForRegion("https://codewhisperer.us-east-1.amazonaws.com/GetUserInfo", "eu-central-1")
	if want := "https://q.eu-central-1.amazonaws.com/GetUserInfo"; got != want {
		t.Fatalf("got %q, want %q (must not be codewhisperer.eu-central-1)", got, want)
	}
}

// TestKiroProfileRegionCandidatesLedByProfileArn pins the auth-region/profile-region
// split: the ARN's own region leads the probe order, and the built-in data planes
// follow as fallbacks.
func TestKiroProfileRegionCandidatesLedByProfileArn(t *testing.T) {
	got := kiroProfileRegionCandidates(&config.Account{
		AuthMethod: "external_idp",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",
	})
	assertOrder(t, got, []string{"eu-central-1", "us-east-1"})
}

// TestKiroProfileRegionCandidatesIgnoresAuthRegion guards the invariant that
// Account.Region (the OIDC/SSO authentication region) never seeds the data-plane
// probe order. Seeding it made an account that authenticated in a region with no
// Amazon Q profile probe only that region and never find its real profile.
func TestKiroProfileRegionCandidatesIgnoresAuthRegion(t *testing.T) {
	for _, method := range []string{"idc", "social", "builderId", "external_idp", ""} {
		got := kiroProfileRegionCandidates(&config.Account{AuthMethod: method, Region: "ap-southeast-2"})
		assertOrder(t, got, []string{"us-east-1", "eu-central-1"})
	}
}

// TestKiroProfileRegionCandidatesNoRegion checks an account with nothing set at all
// still probes both known data planes.
func TestKiroProfileRegionCandidatesNoRegion(t *testing.T) {
	got := kiroProfileRegionCandidates(&config.Account{})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})
}

// TestKiroProfileRegionCandidatesEnvOverride checks KIRO_PROFILE_REGIONS replaces
// the built-in fallbacks while the profile ARN's region still leads, and that
// env-supplied entries are trimmed, lower-cased and de-duplicated like any other.
func TestKiroProfileRegionCandidatesEnvOverride(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "eu-west-1, AP-South-1 ,eu-west-1")
	got := kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp"})
	assertOrder(t, got, []string{"eu-west-1", "ap-south-1"})

	got = kiroProfileRegionCandidates(&config.Account{
		AuthMethod: "external_idp",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/ABCDEF",
	})
	assertOrder(t, got, []string{"eu-central-1", "eu-west-1", "ap-south-1"})
}

// TestKiroProfileRegionCandidatesRejectsMalformedEnvRegion checks a hostile or
// malformed KIRO_PROFILE_REGIONS entry can never become an outbound hostname.
func TestKiroProfileRegionCandidatesRejectsMalformedEnvRegion(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "evil.amazonaws.com,../../etc,eu-west-1")
	got := kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp"})
	assertOrder(t, got, []string{"eu-west-1"})
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate regions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate regions = %v, want %v", got, want)
		}
	}
}
