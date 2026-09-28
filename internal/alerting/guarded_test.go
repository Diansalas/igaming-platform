package alerting

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestClassifySQLState_SwallowAllowlistIsNarrow pins the ADR §7.2(4)
// allowlist exactly: classes 22/23, or exactly 42501/P0001 are swallowed;
// every other class - including every transient class the ADR names -
// propagates. This is the direct kill test for a "swallow everything"
// mutant, without needing to provoke a real transient DB error.
func TestClassifySQLState_SwallowAllowlistIsNarrow(t *testing.T) {
	cases := []struct {
		code    string
		swallow bool
	}{
		{"22001", true},  // class 22 (data exception)
		{"23505", true},  // class 23 (integrity constraint)
		{"42501", true},  // exact code
		{"P0001", true},  // exact code
		{"25P02", false}, // in-failed-sql-transaction
		{"40001", false}, // serialization_failure
		{"40P01", false}, // deadlock_detected
		{"55P03", false}, // lock_not_available
		{"57014", false}, // query_canceled
		{"08006", false}, // class 08 (connection)
		{"53300", false}, // class 53 (resource)
		{"42601", false}, // class 42 but NOT the exact 42501 code (syntax_error)
	}
	for _, c := range cases {
		err := &pgconn.PgError{Code: c.code}
		_, swallow := classifySQLState(err)
		if swallow != c.swallow {
			t.Errorf("classifySQLState(%q): swallow=%v, want %v", c.code, swallow, c.swallow)
		}
	}

	if _, swallow := classifySQLState(nil); swallow {
		t.Error("classifySQLState(nil) must never swallow")
	}
}
