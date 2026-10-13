package main

import (
	"bytes"
	"testing"
)

// ADR 0118 security review F2: each server notice is printed as one line, severity and primary
// message only.
func TestNoticePrinter_FormatsSeverityAndMessage(t *testing.T) {
	var b bytes.Buffer
	h := noticePrinter(&b)
	h("NOTICE", "migration 0136 pre-flight (informational, ADR 0118 s5 class 4): player_withdrawal_hold=1 player_cash=0")
	h("WARNING", "w")
	want := "migrate: NOTICE: migration 0136 pre-flight (informational, ADR 0118 s5 class 4): player_withdrawal_hold=1 player_cash=0\n" +
		"migrate: WARNING: w\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}
