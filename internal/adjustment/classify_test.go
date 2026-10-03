package adjustment

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Code review R-2/R-3: the error classes the HTTP layer maps.
func TestClassifyError_K2Review(t *testing.T) {
	pg := func(code, msg string) error {
		return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code, Message: msg})
	}
	for _, c := range []struct {
		name string
		err  error
		want ErrClass
	}{
		{"expired", fmt.Errorf("x: %w", ErrRequestExpired), ErrClassExpired},
		{"serialization failure", pg("40001", "could not serialize"), ErrClassRetryable},
		{"deadlock", pg("40P01", "deadlock detected"), ErrClassRetryable},
		{"MA021", pg("MA021", "ledger_adjustment_requests: refused: asset_mismatch"), ErrClassRefusedInvalid},
		{"MA022", pg("MA022", "ledger_adjustment_requests: refused: causation_required"), ErrClassRefusedInvalid},
		{"MA025", pg("MA025", "ledger_adjustment: note must be 1-1000 bytes"), ErrClassRefusedInvalid},
		{"MA030 stays conflict", pg("MA030", "x"), ErrClassConflict},
		{"MA014 stays disabled", pg("MA014", "x"), ErrClassDisabled},
	} {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// RefusalToken exposes only the closed token set, never message text.
func TestRefusalToken_ClosedSet(t *testing.T) {
	pg := func(code, msg string) error { return &pgconn.PgError{Code: code, Message: msg} }
	for _, c := range []struct {
		err  error
		want string
	}{
		{pg("MA022", "ledger_adjustment_requests: refused: compensation_cap_exceeded"), "compensation_cap_exceeded"},
		{pg("MA021", "ledger_adjustment_requests: refused: asset_suspended"), "asset_suspended"},
		{pg("MA021", "ledger_adjustment_requests: wallet 123 not found in tenant"), "asset_rule"},
		{pg("MA022", "ledger_adjustment_requests: refused: <script>secret</script>"), "reason_code_rule"},
		{pg("MA025", "ledger_adjustment: note must be 1-1000 bytes"), "note_invalid"},
	} {
		if got := RefusalToken(c.err); got != c.want {
			t.Errorf("%v: got %q want %q", c.err, got, c.want)
		}
	}
}
