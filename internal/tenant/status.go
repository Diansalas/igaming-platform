package tenant

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ADR 0112 SLICE 3: tenant.ChangeStatus was REMOVED. A tenant (or brand) status now moves only
// through the governed launch authorisation service (internal/launchgov), whose executor is the
// ONLY non-test writer of tenants.status / brands.status (pinned by
// internal/launchgov/status_writer_static_test.go) and absorbs the closure semantics below
// (GP020, lock_timeout, the separate-transaction refusal audit). This file keeps the closure
// refusal's typed error and the SQLSTATE translation the executor uses.

// CodeCloseBlockedOpenRounds is the stable machine-readable code of a refused
// tenant closure (owner decision Q-GP-1, 2026-10-06, ADR 0095 section 40.6).
// An HTTP surface answers it 409 with apierror.CodeTenantCloseBlockedOpenRounds.
const CodeCloseBlockedOpenRounds = "TENANT_CLOSE_BLOCKED_OPEN_ROUNDS"

// sqlStateCloseBlocked is raised by the tenants_status_change_gate trigger
// (migration 0121) for a transition INTO 'closed' while open gaming rounds
// exist. The trigger is the authority: it runs inside the status-changing
// transaction under the same advisory lock pair the gameplay gate uses, so
// there is no check-then-act window, and it covers every writer of
// tenants.status.
const sqlStateCloseBlocked = "GP020"

// ErrCloseBlockedOpenRounds is the sentinel for a refused closure.
var ErrCloseBlockedOpenRounds = errors.New("tenant: closure refused: open gaming rounds exist")

// CloseBlockedError carries the COUNTS (only) behind a refused closure. The
// counts are the closing tenant's own (the database function counts under that
// tenant's scope); no round, bet or player identifier is ever included.
//
// Casino: a casino round has no representable open state in the data model
// (see ADR 0095 section 40.6, Q-GP-6), so only sportsbook open bets are
// counted today.
type CloseBlockedError struct {
	SportsbookOpenBets int64
}

func (e *CloseBlockedError) Error() string {
	return fmt.Sprintf("%s: sportsbook_open_bets=%d", ErrCloseBlockedOpenRounds, e.SportsbookOpenBets)
}

// Is makes errors.Is(err, ErrCloseBlockedOpenRounds) true for every
// *CloseBlockedError.
func (e *CloseBlockedError) Is(target error) bool { return target == ErrCloseBlockedOpenRounds }

// TranslateStatusChangeError maps the closure trigger's SQLSTATE GP020 (from
// ANY writer of tenants.status) into a *CloseBlockedError. Every other error
// is returned unchanged. A GP020 whose DETAIL cannot be parsed still maps to
// the typed error (counts zero): the refusal itself is never lost.
func TranslateStatusChangeError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code != sqlStateCloseBlocked {
		return err
	}
	out := &CloseBlockedError{}
	for _, kv := range strings.Split(pgErr.Detail, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			continue
		}
		if k == "sportsbook_open_bets" {
			out.SportsbookOpenBets = n
		}
	}
	return out
}
