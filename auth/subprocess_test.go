package auth

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

const isolatedTestWaitDelay = 2 * time.Second

// runBoundedTestSubprocess reruns one top-level test in a fresh copy of the test
// binary. It returns true in the child process. Parent callers should return
// after it returns false; failures are reported through t.Fatalf here.
func runBoundedTestSubprocess(
	t *testing.T,
	childEnv string,
	runPattern string,
	childTimeout time.Duration,
	env []string,
) bool {
	t.Helper()
	if os.Getenv(childEnv) == "1" {
		return true
	}
	if childTimeout <= 0 {
		childTimeout = 20 * time.Second
	}
	if env == nil {
		env = os.Environ()
	}

	watchdogCtx, watchdogCancel := context.WithTimeout(context.Background(), childTimeout+5*time.Second)
	defer watchdogCancel()

	cmd := exec.CommandContext(
		watchdogCtx,
		os.Args[0],
		"-test.run="+runPattern,
		"-test.timeout="+childTimeout.String(),
		"-test.v",
	)
	cmd.Env = append(append([]string(nil), env...), childEnv+"=1")
	cmd.WaitDelay = isolatedTestWaitDelay
	output, err := cmd.CombinedOutput()
	if err != nil {
		if watchdogCtx.Err() != nil {
			t.Fatalf("isolated test exceeded outer watchdog: %v\n%s", watchdogCtx.Err(), output)
		}
		t.Fatalf("isolated test failed: %v\n%s", err, output)
	}
	return false
}
