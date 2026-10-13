//go:build integration

package aml_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// Re-review 4: the hold-change mark is FORCED by the table guards (under the per-person lock), so an owner-level insert cannot choose
// it: an explicit change_seq of 1 on a hold or a lift is overwritten by the sequence.
func TestHoldGuards_ForceTheChangeMark(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A := c.Q.Officers[0]
	// make sure the sequence is well past 1
	c.Q.PlaceHold(t, c.Svc, c.w().player().PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	var seq, liftSeq int64
	var holdID uuid.UUID
	err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO aml_holds (tenant_id, owner_scope, person_id, hold_scope, reason_class, reason_code, placed_by, placed_by_person_id, placed_by_scope, change_seq)
			VALUES ($1, 'tenant', $2, 'withdrawal_and_payout', 'manual', 't', $3, $4, 'tenant', 1) RETURNING id, change_seq`, c.Tenant, c.P.PersonID, A.StaffID, A.PersonID).Scan(&holdID, &seq); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO aml_hold_lifts (hold_id, tenant_id, owner_scope, lifted_by, lifted_by_person_id, lifted_by_scope, reason_code, change_seq)
			VALUES ($1, $2, 'tenant', $3, $4, 'tenant', 't', 1) RETURNING change_seq`, holdID, c.Tenant, A.StaffID, A.PersonID).Scan(&liftSeq)
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq <= 1 || liftSeq <= seq {
		t.Fatalf("the guards must force a fresh, increasing change mark: hold %d, lift %d", seq, liftSeq)
	}
}

// Re-review 5: the four-eyes writes assume READ COMMITTED; REPEATABLE READ is refused, not assumed.
func TestCaseWrites_RefuseRepeatableRead(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A := c.Q.Officers[0]
	id := c.open(t)
	try := func(iso pgx.TxIsoLevel) error {
		tx, err := c.Owner.Raw().BeginTx(bg, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(bg) }()
		if _, err := tx.Exec(bg, `SELECT set_config('app.tenant_id', $1, true), set_config('app.principal_id', $2, true)`, c.Tenant.String(), A.StaffID.String()); err != nil {
			return err
		}
		var o string
		var p uuid.UUID
		return tx.QueryRow(bg, `SELECT o_owner_scope, o_person_id FROM aml_case_lock_for_write($1, $2, 'tenant', $3, 1)`, c.Tenant, id, A.PersonID).Scan(&o, &p)
	}
	if err := try(pgx.ReadCommitted); err != nil {
		t.Fatalf("READ COMMITTED is the supported level: %v", err)
	}
	wantCode(t, "REPEATABLE READ", try(pgx.RepeatableRead), "AM004")
	wantCode(t, "SERIALIZABLE", try(pgx.Serializable), "AM004")
}

// Re-review C3: the runtime role has no privilege on any aml_* sequence (the schema default privileges would otherwise grant USAGE).
func TestCatalog_RuntimeHasNoPrivilegeOnAnyAMLSequence(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	rows := catalogQuery(t, w, `SELECT c.relname, has_sequence_privilege('igaming_runtime', c.oid, 'USAGE') OR has_sequence_privilege('igaming_runtime', c.oid, 'SELECT')
		OR has_sequence_privilege('igaming_runtime', c.oid, 'UPDATE') FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'S' AND c.relname LIKE 'aml\_%'`)
	if len(rows) == 0 {
		t.Fatal("expected at least aml_hold_change_seq")
	}
	for _, r := range rows {
		if r[1].(bool) {
			t.Errorf("the runtime role holds a privilege on sequence %s", r[0])
		}
	}
}
