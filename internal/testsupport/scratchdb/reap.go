//go:build integration

package scratchdb

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// Reap drops scratch databases leaked by timed-out or killed test runs
// (t.Cleanup does not run on a go test timeout or SIGKILL). It connects with
// TEST_ADMIN_DATABASE_URL (igaming_test_admin, the same credentials New
// uses; no role is created or altered) and drops ONLY databases that
//
//   - match the exact New name pattern (BuildName/ParseName),
//   - were created at least maxAge ago per the timestamp in the name,
//   - are owned by ownerURL's role (TEST_DATABASE_URL),
//   - are not templates and have no session in pg_stat_activity,
//
// using a plain DROP DATABASE (never WITH (FORCE)), so a database that gained
// a session in the meantime is left alone. It returns the dropped names.
// Drop failures are reported in the error but do not stop the sweep.
func Reap(ctx context.Context, adminURL, ownerURL string, maxAge time.Duration) (dropped []string, err error) {
	if maxAge <= 0 {
		return nil, fmt.Errorf("scratchdb: Reap requires a positive maxAge, got %s", maxAge)
	}
	ou, err := url.Parse(ownerURL)
	if err != nil || ou.User == nil || ou.User.Username() == "" {
		return nil, fmt.Errorf("scratchdb: owner URL has no user")
	}
	owner := ou.User.Username()

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("scratchdb: connect as test admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, `
		SELECT d.datname, pg_get_userbyid(d.datdba), d.datistemplate,
		       (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname)
		  FROM pg_database d
		 WHERE d.datname <> current_database()`)
	if err != nil {
		return nil, fmt.Errorf("scratchdb: list databases: %w", err)
	}
	var cands []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.Name, &c.Owner, &c.IsTemplate, &c.Sessions); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scratchdb: scan: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var firstErr error
	for _, name := range SelectReapable(time.Now(), maxAge, owner, cands) {
		if _, derr := conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); derr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("scratchdb: drop %s: %w", name, derr)
			}
			continue
		}
		dropped = append(dropped, name)
	}
	return dropped, firstErr
}
