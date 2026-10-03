//go:build integration

// Package alertinject installs a TEST-ONLY database trigger that makes the
// alert INSERT fail for ONE subject tenant, so a test can prove that an alert
// delivery/raise failure never aborts the business transaction it reports
// (ADR 0102 7.2/7.3, security addendum 1 (a)-(e), LF tests 2/5/6/10).
//
// The trigger is scoped to a single subject tenant id (a fresh uuid per test),
// so a trigger left behind by a crashed test cannot affect any other tenant or
// test. Every Install registers its own t.Cleanup drop.
//
// It lives under internal/testsupport with the integration build tag, so it is
// never compiled into a binary. It changes no role, password or grant; it only
// creates and drops a function and two triggers owned by the test role.
package alertinject

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// Mode selects when the injected failure fires.
type Mode int

const (
	// InTxOnly fails the alert INSERT only inside a transaction that has
	// already written something (a business transaction). A detached raise
	// in a fresh transaction succeeds. Models "the in-tx raise failed, the
	// post-commit detached retry recovers".
	InTxOnly Mode = iota
	// Persistent fails every alert INSERT for the subject, in any
	// transaction. Models a persistent failure: the in-tx raise, the detached
	// retries and (for raised business Kinds) everything but the platform
	// fallback fail.
	Persistent
)

// Install makes alert INSERTs for subjectTenantID raise errcode (a 5-char
// SQLSTATE such as "P0001", or "40P01") according to mode. It targets both
// alerts and alert_occurrences (the dedup-attach path). The trigger is dropped
// on test cleanup.
func Install(t *testing.T, pool *db.Pool, subjectTenantID uuid.UUID, mode Mode, errcode string) {
	t.Helper()
	if len(errcode) != 5 || strings.ContainsAny(errcode, "' ;") {
		t.Fatalf("alertinject: bad errcode %q", errcode)
	}
	name := "iw_inject_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	cond := "subj = '" + subjectTenantID.String() + "'::uuid"
	if mode == InTxOnly {
		cond += " AND pg_current_xact_id_if_assigned() IS NOT NULL"
	}
	fn := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$
DECLARE subj uuid;
BEGIN
  IF TG_TABLE_NAME = 'alerts' THEN
    subj := NEW.subject_tenant_id;
  ELSE
    SELECT subject_tenant_id INTO subj FROM alerts WHERE id = NEW.alert_id;
  END IF;
  IF %s THEN
    RAISE EXCEPTION 'alertinject: injected failure' USING ERRCODE = '%s';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql`, name, cond, errcode)
	// InTxOnly keys on "xid already assigned", which is also true for the
	// occurrence INSERT that follows a successful alert INSERT inside the very
	// same detached transaction - so it is installed on alerts only.
	tables := []string{"alerts", "alert_occurrences"}
	if mode == InTxOnly {
		tables = []string{"alerts"}
	}
	create := func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fn); err != nil {
			return err
		}
		for _, table := range tables {
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				`CREATE TRIGGER %s_%s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, table, table, name)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := pool.WithoutTenant(context.Background(), create); err != nil {
		t.Fatalf("alertinject: install: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			for _, table := range tables {
				if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_%s ON %s`, name, table, table)); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
			return err
		})
	})
}

// Row is one open-or-not alert visible to its subject tenant.
type Row struct {
	Kind          string
	Discriminator string
	Severity      string
	State         string
	Attributes    map[string]any
	Occurrences   int
}

// ForSubject returns every alert whose subject (or owner) is tenantID, read
// through a tenant session (the alerts_subject_tenant_read policy), ordered by
// kind then discriminator. Tests assert on durable rows, never on logs.
func ForSubject(t *testing.T, pool *db.Pool, tenantID uuid.UUID) []Row {
	t.Helper()
	var out []Row
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT a.kind, a.discriminator, a.severity, a.state, a.attributes,
			       (SELECT count(*) FROM alert_occurrences o WHERE o.alert_id = a.id)
			FROM alerts a ORDER BY a.kind, a.discriminator`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Row
			var raw []byte
			if err := rows.Scan(&r.Kind, &r.Discriminator, &r.Severity, &r.State, &raw, &r.Occurrences); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &r.Attributes); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("alertinject: read alerts: %v", err)
	}
	return out
}

// Find returns the rows of a given kind.
func Find(rows []Row, kind string) []Row {
	var out []Row
	for _, r := range rows {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}
