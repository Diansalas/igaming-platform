package httpserver

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Security S-8: a failed ack/resolve logs the two-character SQLSTATE class (never
// the error text), or "non_pg".
func TestAlertAdminSQLStateClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&pgconn.PgError{Code: "55P03"}, "55"},
		{fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23505"}), "23"},
		{errors.New("not a pg error"), "non_pg"},
		{nil, "non_pg"},
	} {
		if got := alertAdminSQLStateClass(tc.err); got != tc.want {
			t.Errorf("alertAdminSQLStateClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
