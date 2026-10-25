//go:build integration

package licensingchange

// d1 (D7, part c/d): the sanctions list cannot be removed or weakened by the governed loosening at ANY stage, even when the request was forged
// into the executed state by someone who could write the request table (the executor re-validates against the state NOW).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestD7_SanctionsRemoval_IsRefusedByTheExecutorEvenForAForgedExecutedRequest(t *testing.T) {
	l := newLW(t)
	l.enable(l.tenant, 0)
	before := l.policies()
	// Filing: refused with the closed code.
	_, err := l.request(KindAMLScreeningPolicyLoosen, l.loosenPayload(20000, 7200, "pep"))
	wantHint(t, err, "sanctions_list_removal_refused", "filing a removal of the sanctions list")
	// The executor: a request of THIS transaction, executed by the acting officer, with zero required approvals and a payload that removes the list
	// (the owner path forges it; every approval rule is therefore out of the way) still cannot write the version.
	body := `{"tenant_id":"` + l.tenant.String() + `","jurisdiction_id":"` + l.jur.String() + `","lists":["pep"],"rescreen_interval_seconds":20000,"backfill_grace_seconds":7200,"effective_offset_seconds":0,"legal_review_reference":"R","evidence_reference":"E"}`
	var applyErr error
	_ = l.owner.WithPlatformAdmin(context.Background(), l.ap2.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `ALTER TABLE licensing_change_requests DISABLE TRIGGER licensing_change_requests_guard`); e != nil {
			return e
		}
		var id uuid.UUID
		if e := tx.QueryRow(ctx, `INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code, widening, required_approvals, requested_by, requested_by_person_id,
				requested_scope, status, created_at, created_txid, expires_at, executed_by, executed_at, executed_txid)
			VALUES ('aml_screening_policy_loosen', $1::jsonb, encode(sha256(gen_random_uuid()::text::bytea), 'hex'), 'x', true, 2, $2, $3, 'platform', 'executed', now(), txid_current(), now() + interval '1 day', $4, now(), txid_current())
			RETURNING id`, body, l.req.StaffID, l.ops.Persons[0], l.ap2.StaffID).Scan(&id); e != nil {
			t.Fatalf("forging the executed request: %v", e)
		}
		// Two fresh approvals by two OTHER officers (the owner forges them: the approval guards are off for these statements), so that every
		// approval and officer rule is satisfied and only the content of the request is left to judge.
		for _, st := range []string{`ALTER TABLE licensing_change_approvals DISABLE TRIGGER licensing_change_approvals_guard`, `ALTER TABLE licensing_change_approvals DISABLE TRIGGER licensing_change_approvals_screening_officer`} {
			if _, e := tx.Exec(ctx, st); e != nil {
				t.Fatalf("%s: %v", st, e)
			}
		}
		for _, ap := range []Actor{l.ap1, l.ap3} {
			if _, e := tx.Exec(ctx, `INSERT INTO licensing_change_approvals (request_id, approver_id, approver_person_id, payload_hash, reason_code, decided_at)
				SELECT $1, s.id, s.person_id, r.payload_hash, 'x', now() FROM staff_users s, licensing_change_requests r WHERE s.id = $2 AND r.id = $1`, id, ap.StaffID); e != nil {
				t.Fatalf("forging an approval: %v", e)
			}
		}
		_, applyErr = tx.Exec(ctx, `SELECT screening_policy_loosen_apply($1)`, id)
		return context.Canceled // roll everything back
	})
	var pe *pgconn.PgError
	if !errors.As(applyErr, &pe) || pe.Code != "LC050" || pe.Hint != "sanctions_list_removal_refused" {
		t.Fatalf("the executor must refuse a removal of the sanctions list with the closed code: %v", applyErr)
	}
	if got := l.policies(); len(got) != len(before) {
		t.Fatalf("no version was written: %d -> %d", len(before), len(got))
	}
}
