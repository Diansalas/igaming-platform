package httpserver

import (
	"os"
	"strings"
	"testing"
)

// Code review CR2-S1: the lock-level regression test exercises the
// alertTransitionLockSQL constant, so it would still pass if the handler stopped
// using that constant and inlined a stronger lock. Pin the handler to the
// constant, and the constant to FOR NO KEY UPDATE.
func TestAlertAdmin_HandlerLocksThroughTheNoKeyUpdateConstant(t *testing.T) {
	b, err := os.ReadFile("alert_admin_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, "QueryRow(ctx, alertTransitionLockSQL,"); n != 1 {
		t.Fatalf("the ack/resolve handler must read the alert through alertTransitionLockSQL exactly once, found %d", n)
	}
	if !strings.HasSuffix(alertTransitionLockSQL, "FOR NO KEY UPDATE") {
		t.Fatalf("alertTransitionLockSQL must end in FOR NO KEY UPDATE: %q", alertTransitionLockSQL)
	}
	// No other FOR UPDATE row lock may appear in the handler file (a second,
	// stronger lock would reintroduce the S-1 blocking). Comments are stripped so
	// the explanatory text about FOR UPDATE does not count.
	var code []string
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			code = append(code, line)
		}
	}
	if n := strings.Count(strings.Join(code, "\n"), "FOR UPDATE"); n != 0 {
		t.Fatalf("alert_admin_handlers.go must not take FOR UPDATE on alerts, found %d", n)
	}
}
