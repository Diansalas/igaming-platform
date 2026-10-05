//go:build integration

package alerting

// PRH-2 E1 (ADR 0106 section 4.3, test 50): FULL-COLUMN parity between the Go
// kindDefs mirror and the database vocabulary, in BOTH directions, for every
// Kind (not only the KYC Kind): severity, scope, simulation, requires_subject,
// in_tx_raisable_by_tenant, allowed_keys (as a set) and raise_mode. Every DB
// Kind exists in Go and every Go Kind exists in the database. The KYC Kind is
// pinned explicitly.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type dbKindRow struct {
	kind, severity, scope, raiseMode string
	simulation, requiresSubject      bool
	inTxRaisable                     bool
	allowedKeys                      []string
}

func loadDBKinds(t *testing.T) map[string]dbKindRow {
	t.Helper()
	pool := testPool(t)
	out := map[string]dbKindRow{}
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode FROM alert_kinds`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r dbKindRow
			if err := rows.Scan(&r.kind, &r.severity, &r.scope, &r.simulation, &r.requiresSubject, &r.inTxRaisable, &r.allowedKeys, &r.raiseMode); err != nil {
				return err
			}
			sort.Strings(r.allowedKeys)
			out[r.kind] = r
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read alert_kinds: %v", err)
	}
	return out
}

func TestKindParity_FullColumn_BothDirections_50(t *testing.T) {
	db := loadDBKinds(t)
	if len(db) < 18 {
		t.Fatalf("expected the full vocabulary (>= 18 Kinds), got %d", len(db))
	}
	// Direction 1: every Go Kind exists in the database with identical columns.
	for _, k := range Kinds() {
		def := MustDef(k)
		row, ok := db[string(k)]
		if !ok {
			t.Errorf("Go Kind %q has no alert_kinds row", k)
			continue
		}
		var goKeys []string
		for key := range def.AllowedKeys {
			goKeys = append(goKeys, key)
		}
		sort.Strings(goKeys)
		switch {
		case row.severity != string(def.Severity):
			t.Errorf("%q severity: DB=%s Go=%s", k, row.severity, def.Severity)
		case row.scope != string(def.Scope):
			t.Errorf("%q scope: DB=%s Go=%s", k, row.scope, def.Scope)
		case row.simulation != def.Simulation:
			t.Errorf("%q simulation: DB=%v Go=%v", k, row.simulation, def.Simulation)
		case row.requiresSubject != def.RequiresSubject:
			t.Errorf("%q requires_subject: DB=%v Go=%v", k, row.requiresSubject, def.RequiresSubject)
		case row.inTxRaisable != def.InTxRaisableByTenant:
			t.Errorf("%q in_tx_raisable_by_tenant: DB=%v Go=%v", k, row.inTxRaisable, def.InTxRaisableByTenant)
		case row.raiseMode != string(def.RaiseMode):
			t.Errorf("%q raise_mode: DB=%s Go=%s", k, row.raiseMode, def.RaiseMode)
		case strings.Join(row.allowedKeys, ",") != strings.Join(goKeys, ","):
			t.Errorf("%q allowed_keys: DB=%v Go=%v", k, row.allowedKeys, goKeys)
		}
	}
	// Direction 2: every database Kind has a Go definition (a migration-seeded
	// Kind with no Go mirror would be un-raisable and silently drifting).
	for kind := range db {
		if _, ok := Def(Kind(kind)); !ok {
			t.Errorf("alert_kinds row %q has no Go kindDefs entry", kind)
		}
	}
	// The KYC Kind, pinned explicitly.
	row := db["kyc.submission_failed_terminal"]
	if row.severity != "p2" || row.scope != "platform" || row.simulation || !row.requiresSubject || !row.inTxRaisable || row.raiseMode != "in_tx" ||
		strings.Join(row.allowedKeys, ",") != "last_error_class,operation,outbox_id,provider_id" {
		t.Errorf("KYC Kind row = %+v", row)
	}
	if string(KindKYCSubmissionFailedTerminal) != "kyc.submission_failed_terminal" {
		t.Errorf("Go constant = %q", KindKYCSubmissionFailedTerminal)
	}
}
