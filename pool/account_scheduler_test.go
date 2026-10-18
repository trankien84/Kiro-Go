package pool

import (
	"path/filepath"
	"reflect"
	"testing"

	"kiro-go/config"
)

func schedulerTestCandidates(priorities ...int) ([]config.Account, []accountSelectionCandidate) {
	accounts := make([]config.Account, len(priorities))
	candidates := make([]accountSelectionCandidate, len(priorities))
	for i, priority := range priorities {
		accounts[i] = config.Account{
			ID:       string(rune('a' + i)),
			Enabled:  true,
			Priority: priority,
		}
		candidates[i] = accountSelectionCandidate{acc: &accounts[i]}
	}
	return accounts, candidates
}

func schedulerCandidateIDs(candidates []accountSelectionCandidate) []string {
	ids := make([]string, len(candidates))
	for i := range candidates {
		ids[i] = candidates[i].acc.ID
	}
	return ids
}

func TestAccountSelectionStrategyRoundRobinPreservesEligibleCandidates(t *testing.T) {
	_, candidates := schedulerTestCandidates(30, 10, 20)
	got := applyAccountSelectionStrategy(candidates, config.AccountSelectionStrategyRoundRobin)

	if !reflect.DeepEqual(schedulerCandidateIDs(got), []string{"a", "b", "c"}) {
		t.Fatalf("round-robin candidates = %v, want [a b c]", schedulerCandidateIDs(got))
	}
	if len(got) != len(candidates) || &got[0] != &candidates[0] {
		t.Fatal("round-robin did not preserve the original candidate slice")
	}
}

func TestAccountSelectionStrategyPriorityKeepsBestEligibleTier(t *testing.T) {
	_, candidates := schedulerTestCandidates(20, 10, 10, 30)
	got := applyAccountSelectionStrategy(candidates, config.AccountSelectionStrategyPriority)

	if !reflect.DeepEqual(schedulerCandidateIDs(got), []string{"b", "c"}) {
		t.Fatalf("priority candidates = %v, want [b c]", schedulerCandidateIDs(got))
	}
}

func TestAccountSelectionStrategyPriorityTreatsUnsetAsDefaultTier(t *testing.T) {
	_, candidates := schedulerTestCandidates(0, 50, -1, 50)
	got := applyAccountSelectionStrategy(candidates, config.AccountSelectionStrategyPriority)

	if !reflect.DeepEqual(schedulerCandidateIDs(got), []string{"b", "d"}) {
		t.Fatalf("priority candidates = %v, want [b d]", schedulerCandidateIDs(got))
	}
}

func TestAccountSelectionStrategyPriorityFallsBackToBestSurvivingTier(t *testing.T) {
	// Eligibility filtering has already removed any unavailable priority-10
	// account before this helper runs. Among the surviving candidates, priority
	// 20 is therefore the best usable tier.
	_, candidates := schedulerTestCandidates(30, 20, 20)
	got := applyAccountSelectionStrategy(candidates, config.AccountSelectionStrategyPriority)

	if !reflect.DeepEqual(schedulerCandidateIDs(got), []string{"b", "c"}) {
		t.Fatalf("priority fallback candidates = %v, want [b c]", schedulerCandidateIDs(got))
	}
}

func TestAccountSelectionStrategyFillFirstUsesFirstAccountInBestTier(t *testing.T) {
	_, candidates := schedulerTestCandidates(20, 10, 10, 30)
	got := applyAccountSelectionStrategy(candidates, config.AccountSelectionStrategyFillFirst)

	if !reflect.DeepEqual(schedulerCandidateIDs(got), []string{"b"}) {
		t.Fatalf("fill-first candidates = %v, want [b]", schedulerCandidateIDs(got))
	}
}

func TestAccountSelectionStrategyFillFirstUsesPoolOrderAtDefaultTier(t *testing.T) {
	_, candidates := schedulerTestCandidates(0, 0, 0)
	got := applyAccountSelectionStrategy(candidates, config.AccountSelectionStrategyFillFirst)

	if !reflect.DeepEqual(schedulerCandidateIDs(got), []string{"a"}) {
		t.Fatalf("fill-first candidates = %v, want [a]", schedulerCandidateIDs(got))
	}
}

func TestAccountSelectionStrategyUnknownFailsClosedToRoundRobin(t *testing.T) {
	_, candidates := schedulerTestCandidates(30, 10, 20)
	got := applyAccountSelectionStrategy(candidates, "future-strategy")

	if !reflect.DeepEqual(schedulerCandidateIDs(got), []string{"a", "b", "c"}) {
		t.Fatalf("unknown strategy changed candidate set: %v", schedulerCandidateIDs(got))
	}
}

func initAccountSelectionStrategyTestConfig(t *testing.T, strategy string) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateAccountSelectionStrategy(strategy); err != nil {
		t.Fatalf("UpdateAccountSelectionStrategy(%q): %v", strategy, err)
	}
}

func TestAccountSelectionStrategyPrioritySelectorRoundRobinsInsideBestTier(t *testing.T) {
	initAccountSelectionStrategyTestConfig(t, config.AccountSelectionStrategyPriority)
	p := newTestPool(
		config.Account{ID: "secondary", Priority: 20},
		config.Account{ID: "primary-a", Priority: 10},
		config.Account{ID: "primary-b", Priority: 10},
		config.Account{ID: "legacy", Priority: 0},
	)

	var previous string
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		account := p.GetNextForModelExcluding("", nil)
		if account == nil {
			t.Fatal("priority selector returned nil")
		}
		if account.ID != "primary-a" && account.ID != "primary-b" {
			t.Fatalf("priority selector escaped best tier with %q", account.ID)
		}
		if previous != "" && account.ID == previous {
			t.Fatalf("priority selector did not round-robin inside tier: repeated %q", account.ID)
		}
		seen[account.ID] = true
		previous = account.ID
	}
	if !seen["primary-a"] || !seen["primary-b"] {
		t.Fatalf("priority selector did not use both best-tier accounts: %#v", seen)
	}
}

func TestAccountSelectionStrategyFillFirstSelectorKeepsFirstBestTierAccount(t *testing.T) {
	initAccountSelectionStrategyTestConfig(t, config.AccountSelectionStrategyFillFirst)
	p := newTestPool(
		config.Account{ID: "secondary", Priority: 20},
		config.Account{ID: "primary-a", Priority: 10},
		config.Account{ID: "primary-b", Priority: 10},
	)

	for i := 0; i < 8; i++ {
		account := p.GetNextForModelExcluding("", nil)
		if account == nil || account.ID != "primary-a" {
			t.Fatalf("fill-first selection %d = %#v, want primary-a", i, account)
		}
	}
}
