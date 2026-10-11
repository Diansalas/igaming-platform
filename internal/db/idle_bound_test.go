package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The refusal happens before any connection is attempted (DB-free).
func TestConnectWithRuntimeParams_URLCannotDisableTheTimeout(t *testing.T) {
	for _, v := range []string{"0", "2h"} {
		u := "postgres://u:p@127.0.0.1:1/x?sslmode=disable&idle_in_transaction_session_timeout=" + v
		_, err := ConnectWithRuntimeParams(context.Background(), u, 1, time.Second, map[string]string{"idle_in_transaction_session_timeout": "30000"})
		if err == nil || !strings.Contains(err.Error(), "idle_in_transaction_session_timeout") {
			t.Errorf("url value %q must be refused before connecting: %v", v, err)
		}
	}
}

// Review round 3: the bound cannot be bypassed by the case of the key, a second spelling, or the
// `options` startup parameter (DB-free: refused before any connection).
func TestConnectWithRuntimeParams_URLBoundIsCaseAndOptionsProof(t *testing.T) {
	base := "postgres://u:p@127.0.0.1:1/x?sslmode=disable&"
	for _, q := range []string{
		"IDLE_IN_TRANSACTION_SESSION_TIMEOUT=0",
		"Idle_In_Transaction_Session_Timeout=0",
		"idle_in_transaction_session_timeout=30s&IDLE_IN_TRANSACTION_SESSION_TIMEOUT=0",
		"IDLE_IN_TRANSACTION_SESSION_TIMEOUT=2h",
		"options=-c%20idle_in_transaction_session_timeout%3D0",
		"options=-c%20IDLE_IN_TRANSACTION_SESSION_TIMEOUT%3D0",
		"options=--idle-in-transaction-session-timeout%3D0",
	} {
		for i := 0; i < 20; i++ { // map iteration order is random: the answer must not be
			_, err := ConnectWithRuntimeParams(context.Background(), base+q, 1, time.Second, map[string]string{"idle_in_transaction_session_timeout": "30000"})
			if err == nil || !strings.Contains(err.Error(), "idle_in_transaction_session_timeout") {
				t.Fatalf("%q must be refused before connecting (try %d): %v", q, i, err)
			}
		}
	}
	// A legitimate mixed-case value is folded to the one lower-case setting.
	rp := map[string]string{"IDLE_IN_TRANSACTION_SESSION_TIMEOUT": "30s", "application_name": "x"}
	if err := normaliseGuardedURLParams(rp); err != nil || rp[guardedIdleParam] != "30s" || len(rp) != 2 {
		t.Fatalf("fold: %v %v", err, rp)
	}
}

// A DATABASE_URL may tighten idle_in_transaction_session_timeout but never disable or loosen it
// beyond the hour bound (ADR 0112 security C4 review condition).
func TestCheckIdleInTxBound(t *testing.T) {
	for _, ok := range []string{"1", "700ms", "30s", "30000", "59min", "1h", "3600s", "3600000"} {
		if err := checkIdleInTxBound(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "0s", "0ms", "", "-1", "abc", "2h", "3601s", "3600001", "1d", "5 parsecs", "99999999999999999999"} {
		if err := checkIdleInTxBound(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
