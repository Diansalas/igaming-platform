//go:build integration

package adjustment

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// B-3 (RLS), positive half + A-3: the acting family (ADR 0099 §6) completes
// a governed post - acting initiator with a tenant approver, a tenant
// initiator with an acting approver, and acting-initiator/acting-approver
// (two distinct platform principals and Persons). Each goes through the
// Service's real session dispatch (a platform token => the sole setter).
func TestB3_ActingFamilyCompletesGovernedPost(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(50_000)

	cases := []struct {
		name               string
		initiator, decider staffMember
	}{
		{"acting initiates, tenant approves", w.Acting, w.F2},
		{"tenant initiates, acting approves", w.F1, w.Acting},
		{"acting initiates, second acting approves", w.Acting, w.Acting2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := w.playerCash()
			r, err := w.submit(c.initiator, w.credit(1_000, ReasonOperationalErrorCorrection))
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			wantScope := ScopeTenant
			if c.initiator.TenantID == uuid.Nil {
				wantScope = ScopePlatformActing
			}
			if r.InitiatedByScope != wantScope || r.InitiatedBy != c.initiator.ID || r.InitiatedByPersonID != c.initiator.PersonID {
				t.Fatalf("forced actor wrong: %+v", r)
			}
			out, err := w.decide(c.decider, r, DecisionApprove)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			if !out.Executed {
				t.Fatalf("not executed: %+v", out)
			}
			if got := w.playerCash(); got != before+1_000 {
				t.Fatalf("balance: want %d got %d", before+1_000, got)
			}
		})
	}
	w.assertInvariants()
}

// B-3 negatives: the plain platform session has NO policy on any K2
// request table (ADR 0100 §6.6), and an acting session for tenant Y cannot
// reach tenant X's requests.
func TestB3_PlainPlatformAndForeignActingRefused(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	ctx := context.Background()

	// Plain platform: cannot see, insert or decide.
	err = w.pool.WithPlatformAdmin(ctx, w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_requests WHERE id = $1`, r.ID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("plain platform session sees a K2 request")
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT p`); err != nil {
			return err
		}
		// Raw SQL (not SubmitInTx): the Go signer now refuses to sign a K2 proof for a
		// "platform" scope (SIGNED-ACTOR-PROOF R1), so the DATABASE's own refusal of a
		// plain platform session is exercised directly.
		err := attackSubmitSQL(ctx, tx, w, uuid.New(), w.credit(1, ReasonOperationalErrorCorrection))
		if err == nil {
			return fmt.Errorf("plain platform session inserted a K2 request")
		}
		if code := pgCode(err); code != "42501" && code != "MA001" && code != "CG001" {
			return fmt.Errorf("plain platform insert refused with an unexpected code: %v", err)
		}
		_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT p`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.svc.Decide(tenant.WithContext(ctx, tenant.Context{TenantID: uuid.Nil, Subject: w.AdminA.ID.String()}), w.target(), r.ID,
		DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "x"}, Meta{})
	if code := pgCode(err); code != "CG020" {
		t.Fatalf("a platform principal without a grant for X must be refused at the acting open (CG020), got %v", err)
	}

	// Acting Y on X: a principal whose grants are for tenant Y.
	y := newWorld(t, worldOpts{base: 1})
	err = y.pool.WithPlatformActingInTenant(ctx, y.Acting.ID, y.Tenant, uuid.Nil, OperationKind, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_requests WHERE id = $1`, r.ID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("acting session for Y sees X's request")
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT p`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'platform_acting', $4, 0, 'x')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil)
		if err == nil {
			return fmt.Errorf("acting session for Y approved X's request")
		}
		_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT p`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// And Y's acting principal targeting X through the Service: CG020.
	_, err = y.svc.Decide(ctxFor(y.Acting), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "x"}, Meta{})
	if code := pgCode(err); code != "CG020" {
		t.Fatalf("acting Y on X must be refused CG020, got %v", err)
	}
	if got := w.request(r.ID); got.State != StatePending {
		t.Fatalf("request changed state: %s", got.State)
	}
}

// B-3 K1-1 negatives (C-100-6; security confirmation C-2): INSIDE K2's
// real acting executor transaction - after a real governed execution in
// the same transaction - the acting session still cannot insert a
// platform_admin, read another principal's password_hash, write a platform
// audit row, write sessions, or touch persons. And B-22/A-19: the rows the
// executor locked expose only the disciplined columns.
func TestB3_K11NegativesInsideRealActingExecutorTx(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(5_000)
	r, err := w.submit(w.F1, w.credit(700, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	var probes []string
	err = w.svc.runSession(ctxFor(w.Acting), w.target(), r.ID, Meta{}, func(ctx context.Context, tx pgx.Tx, call Call) error {
		out, err := DecideInTx(ctx, tx, call, r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "k2"})
		if err != nil {
			return err
		}
		if !out.Executed {
			return fmt.Errorf("expected the acting approval to execute")
		}
		try := func(name, sql string, args ...any) {
			if _, err := tx.Exec(ctx, `SAVEPOINT k11`); err != nil {
				probes = append(probes, name+": savepoint: "+err.Error())
				return
			}
			tag, err := tx.Exec(ctx, sql, args...)
			if err == nil && tag.RowsAffected() > 0 {
				probes = append(probes, name+": SUCCEEDED (must be refused)")
			}
			_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT k11`)
		}
		try("insert platform_admin", `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active')`,
			uuid.New(), "k11-"+uuid.NewString()+"@x.invalid")
		// A supplied NULL (platform) tenant is re-forced to the acting
		// tenant by audit_log_acting_actor: never a platform audit row.
		if _, err := tx.Exec(ctx, `SAVEPOINT k11a`); err != nil {
			return err
		}
		var auditTenant *uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, outcome) VALUES (NULL, 'staff', $1, 'k11.probe', 'success') RETURNING tenant_id`,
			w.AdminA.ID).Scan(&auditTenant); err == nil && (auditTenant == nil || *auditTenant != w.Tenant) {
			probes = append(probes, "platform audit row: written with a non-acting tenant")
		}
		_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT k11a`)
		try("session row", `INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, expires_at) VALUES ($1, 'staff', $2, NULL, $3, now() + interval '1 hour')`,
			uuid.New(), w.Acting.ID, uuid.NewString())
		try("persons insert", `INSERT INTO persons (id) VALUES ($1)`, uuid.New())
		try("persons update", `UPDATE persons SET id = id WHERE id = $1`, w.PlayerPerson)
		try("update staff", `UPDATE staff_users SET status = 'active' WHERE id = $1`, w.F1.ID)

		var visible int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE id IN ($1, $2)`, w.AdminA.ID, w.Acting2.ID).Scan(&visible); err != nil {
			return err
		}
		if visible != 0 {
			probes = append(probes, "read another platform principal's staff row: visible")
		}
		var personRows int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM persons`).Scan(&personRows); err != nil {
			return err
		}
		if personRows != 0 {
			probes = append(probes, "persons readable")
		}
		var audits int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&audits); err != nil {
			return err
		}
		if audits != 0 {
			probes = append(probes, "audit_log readable")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("acting executor tx: %v", err)
	}
	if len(probes) > 0 {
		t.Fatalf("K1-1 negatives failed inside the real acting executor tx: %v", probes)
	}
	w.assertInvariants()
}
