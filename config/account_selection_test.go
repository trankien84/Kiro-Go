package config

import (
	"path/filepath"
	"testing"
)

func TestAccountSelectionStrategyNormalization(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "unset defaults to round robin", input: "", want: AccountSelectionStrategyRoundRobin},
		{name: "round robin", input: "round-robin", want: AccountSelectionStrategyRoundRobin},
		{name: "round robin normalized", input: "  ROUND-ROBIN  ", want: AccountSelectionStrategyRoundRobin},
		{name: "priority", input: "priority", want: AccountSelectionStrategyPriority},
		{name: "fill first", input: "fill-first", want: AccountSelectionStrategyFillFirst},
		{name: "least used excluded", input: "least-used", wantErr: true},
		{name: "random excluded", input: "random", wantErr: true},
		{name: "p2c excluded", input: "p2c", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeAccountSelectionStrategy(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeAccountSelectionStrategy(%q) unexpectedly returned %q", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAccountSelectionStrategy(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeAccountSelectionStrategy(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestAccountSelectionStrategyUpdatePersistsAndRejectsInvalidWithoutMutation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := Init(configPath); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := GetAccountSelectionStrategy(); got != AccountSelectionStrategyRoundRobin {
		t.Fatalf("default strategy = %q, want %q", got, AccountSelectionStrategyRoundRobin)
	}

	if err := UpdateAccountSelectionStrategy(AccountSelectionStrategyPriority); err != nil {
		t.Fatalf("UpdateAccountSelectionStrategy(priority): %v", err)
	}
	if got := GetAccountSelectionStrategy(); got != AccountSelectionStrategyPriority {
		t.Fatalf("live strategy = %q, want %q", got, AccountSelectionStrategyPriority)
	}
	if err := Load(); err != nil {
		t.Fatalf("Load persisted config: %v", err)
	}
	if got := GetAccountSelectionStrategy(); got != AccountSelectionStrategyPriority {
		t.Fatalf("reloaded strategy = %q, want %q", got, AccountSelectionStrategyPriority)
	}

	if err := UpdateAccountSelectionStrategy("random"); err == nil {
		t.Fatal("invalid strategy unexpectedly persisted")
	}
	if got := GetAccountSelectionStrategy(); got != AccountSelectionStrategyPriority {
		t.Fatalf("invalid update mutated strategy to %q", got)
	}
}

func TestAccountSelectionEffectivePriority(t *testing.T) {
	tests := []struct {
		input int
		want  int
	}{
		{input: -10, want: DefaultAccountPriority},
		{input: 0, want: DefaultAccountPriority},
		{input: 1, want: 1},
		{input: 10, want: 10},
		{input: DefaultAccountPriority, want: DefaultAccountPriority},
		{input: 250, want: 250},
	}

	for _, tt := range tests {
		if got := EffectiveAccountPriority(tt.input); got != tt.want {
			t.Fatalf("EffectiveAccountPriority(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
