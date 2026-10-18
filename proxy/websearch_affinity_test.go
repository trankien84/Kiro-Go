package proxy

import (
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestPerformWebSearchUsesApiKeyAffinity is the regression test for F5:
// performWebSearch previously called pool.GetNextForModelExcluding, the
// affinity-blind selector, while every other request path uses
// GetNextForModelWithApiKey so a customer key's requests keep landing on the
// same account (a prompt-cache hit-rate win this fork otherwise optimizes
// hard for). This proves performWebSearch now honors an existing affinity
// binding instead of falling through to plain round-robin.
//
// Three accounts, not two: with only two, "the account not just dispatched"
// and "the very next round-robin pick" are the same account, so binding
// happens to coincide with round-robin's next pick too and the test can't
// tell fixed from buggy. With three, after (1) the bind dispatches "bound"
// and (2) one plain GetNextForModelExcluding call (proving that call does
// NOT land on "bound") dispatches a second account, a THIRD account remains
// never-dispatched and is therefore LRU's strict next pick — guaranteed
// distinct from "bound". So performWebSearch(..., apiKeyID) landing on
// "bound" can only mean it consulted the affinity binding, not round-robin.
func TestPerformWebSearchUsesApiKeyAffinity(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetSessionAffinityEnabled(true); err != nil {
		t.Fatalf("enable session affinity: %v", err)
	}
	for _, id := range []string{"acc-a", "acc-b", "acc-c"} {
		if err := config.AddAccount(config.Account{
			ID: id, Email: id + "@test", AuthMethod: "idc", Enabled: true,
			AccessToken: "tok-" + id,
			ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/" + id,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	const apiKeyID = "cust-key"
	bound := p.GetNextForModelWithApiKey("claude-sonnet-4", nil, apiKeyID)
	if bound == nil {
		t.Fatal("expected a bound account")
	}
	// Prove this call alone does not land on "bound" (sanity on the pool's LRU
	// behavior, not on performWebSearch) — and, as a side effect, dispatch a
	// second account so a third remains strictly least-recently-used.
	rotated := p.GetNextForModelExcluding("claude-sonnet-4", nil)
	if rotated == nil || rotated.ID == bound.ID {
		t.Fatalf("test setup broken: rotated=%v, want an account distinct from bound %s", rotated, bound.ID)
	}

	// Record which account(s) were actually queried instead of failing the
	// "wrong" one from inside the stub: performWebSearch retries the next
	// account on an MCP error, so an error-returning stub would let a buggy,
	// affinity-blind implementation quietly succeed on a LATER attempt and
	// mask the bug. Every account gets a real (successful) response here;
	// the assertion below checks the recorder afterward instead.
	var mu sync.Mutex
	var queried []string
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		queried = append(queried, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-"))
		mu.Unlock()
		return mcpSearchResponse(), nil
	})})
	defer kiroHttpStore.Store(oldClient)

	_, _, account, err := h.performWebSearch("claude-sonnet-4", "golang generics", apiKeyID)
	if err != nil {
		t.Fatalf("performWebSearch: %v", err)
	}
	if account == nil || account.ID != bound.ID {
		t.Fatalf("performWebSearch used account %+v, want the apiKeyID-bound account %s", account, bound.ID)
	}
	if len(queried) != 1 || queried[0] != bound.ID {
		t.Fatalf("MCP was queried with accounts %v, want exactly one call to the apiKeyID-bound account %q (a buggy, affinity-blind selector would hit a different account on the first attempt)", queried, bound.ID)
	}
}
