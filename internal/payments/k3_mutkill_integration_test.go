//go:build integration

package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tests added after the first mutation run (ADR 0101 12 mutant table): each one
// pins a database guard that a surviving mutant showed was not exercised
// directly. They attack the guards with direct statements, because the service
// never produces the shape.

// S09b: the entries fence's account-shape test must be NULL-safe. A HOUSE-level
// account of type player_withdrawal_hold (wallet_id NULL) is structurally
// possible; its wallet comparison is NULL, and a NOT (NULL) must not let the leg
// through.
func TestK3_X01_EntriesFenceIsNullSafe_HouseLevelHoldAccount(t *testing.T) {
	for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			wr, a := w.ambiguousPayout(250)
			house := uuid.New()
			w.tx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, tenant_id, account_type, asset_code, status) VALUES ($1, $2, 'player_withdrawal_hold', 'EUR', 'active')`,
					house, w.f.tenantID)
				return err
			})
			r := w.mustRequest(w.acting, w.m2In(a.ID, kind))
			typ, key := "withdrawal_completed", ""
			var provider, ptx *string
			if kind == ResolutionM2DeclarePaid {
				provider = &w.provider
				p := ReservedDeclaredTxID(r.ID)
				ptx = &p
				key = w.provider + ":" + p
			} else {
				typ, key = "withdrawal_failed", wr.ID.String()+":failed"
			}
			err := w.inExecutingActing(r, w.acting2, func(ctx context.Context, tx pgx.Tx) error {
				err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
					id := uuid.New()
					if err := k3InsertTx(ctx, tx, w.f.tenantID, id, typ, key, provider, ptx, wr.ID); err != nil {
						return err
					}
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, house, "debit", wr.Amount)
				})
				if k3Code(err) != "CG030" {
					t.Errorf("a leg on a wallet-less hold account: want CG030, got %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// S07b: an `executed` M2 that does not match its outcome cannot COMMIT (the
// deferred verifier), even when every statement passed the row guards.
func TestK3_X02_ExecutedWithoutItsOutcomeCannotCommit(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	anyTx := w.sysQuery(`SELECT id FROM ledger_transactions WHERE tenant_id = $1 LIMIT 1`, w.f.tenantID)[0]["id"]
	err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f2.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := k3InsertApproval(ctx, tx, w.f.tenantID, r, "approve"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		// No attempt/withdrawal move, no posting: just claim the link.
		_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, r.ID, anyTx)
		return err
	})
	k3RequireCode(t, err, "MR041")
	if got := w.resolution(r.ID).State; got != ResolutionPending {
		t.Fatalf("a refused commit left the resolution %s", got)
	}
}

// S12b: evidence cannot be BACK-FILLED for an attempt that was parked earlier
// without it (the BEFORE INSERT guard admits only a live deposit attempt).
func TestK3_X03_EvidenceCannotBeBackfilledForAnEarlierPark(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	a := w.parkedDepositWithReason(TerminalReasonPollReferenceMismatch) // fixture park: no evidence row
	if n := len(w.evidenceRows(a.ID)); n != 0 {
		t.Fatalf("setup: %d evidence rows", n)
	}
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
			VALUES ($1, $2, $3, 'poll_returned_reference', 'k3-backfill-y')`, w.f.tenantID, a.ID, *a.ProviderID)
		return err
	})
	k3RequireCode(t, err, "MR050")
	if n := len(w.evidenceRows(a.ID)); n != 0 {
		t.Fatalf("a back-filled evidence row survived: %d", n)
	}
}

// S13: the evidence INSERT policy admits only the system shape. A tenant STAFF
// session that parks a live attempt in the same transaction and then inserts the
// evidence passes the guard and the deferred check; only the policy refuses it.
func TestK3_X04_EvidenceInsertPolicyRefusesAStaffSessionEvenWithAValidPark(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	live, _ := w.depositAmbiguousBound()
	err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		// The real writer's order: the evidence row first (while the attempt is
		// live), then the park in the same transaction.
		if _, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
			VALUES ($1, $2, $3, 'poll_returned_reference', 'k3-staff-y')`, w.f.tenantID, live.ID, *live.ProviderID); err != nil {
			return err
		}
		return ApplyDisputeFromNonTerminal(ctx, tx, live.ID, EvidenceQueryStatus, TerminalReasonPollReferenceMismatch)
	})
	if err == nil {
		t.Fatal("a tenant staff session inserted evidence (policy widened)")
	}
	if k3Code(err) != "42501" {
		t.Fatalf("want an RLS refusal (42501), got %v", err)
	}
}

// S17d: a tenant-scope approval recorded while the tenant was open is NOT counted
// at execution once the tenant is closed (recount), and the resolution does not
// execute on the strength of it.
func TestK3_X05_ClosedAfterApproval_TenantScopeApprovalIsNotCounted(t *testing.T) {
	w := newK3World(t, k3Opts{base: 2})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || out.Executed || out.Counted != 1 {
		t.Fatalf("first approval (tenant scope, open tenant): %v %+v", err, out)
	}
	w.setTenantStatus("closed")
	out, err = w.decide(w.acting2, r, ResolutionApprove)
	if err != nil {
		t.Fatalf("second approval: %v", err)
	}
	if out.Executed {
		t.Fatalf("executed on a tenant-scope approval that no longer counts in a closed tenant: %+v", out)
	}
	if got := w.attempt(a.ID); got.State != AttemptAmbiguous {
		t.Fatalf("attempt %s", got.State)
	}
}

// S18a: the payload hash covers the pinned attempt state and terminal reason
// (R-6): an independent oracle recomputes it from the stored columns.
func TestK3_X06_PayloadHashCoversThePinnedFactualBasis(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.disputedPayout(100, "provider_reference_mismatch")
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	var withPins, withoutPins bool
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
			payload_hash = k2_sha256_hex(k2_canonical(tenant_id::text, attempt_id::text, kind, target_state, finding_code, basis_code,
				context_code, evidence_ref_hash, amount::text, asset_code, reason_code, attempt_state_at_submission, terminal_reason_at_submission)),
			payload_hash = k2_sha256_hex(k2_canonical(tenant_id::text, attempt_id::text, kind, target_state, finding_code, basis_code,
				context_code, evidence_ref_hash, amount::text, asset_code, reason_code))
			FROM payment_manual_resolutions WHERE id = $1`, r.ID).Scan(&withPins, &withoutPins)
	}); err != nil {
		t.Fatal(err)
	}
	if !withPins || withoutPins {
		t.Fatalf("payload hash does not cover the pinned fields (with pins match=%v, without pins match=%v)", withPins, withoutPins)
	}
}

// S23a/S23b: the all-sessions ledger prefix trigger. (a) another transaction TYPE
// reusing the executing M2's exact keys is refused; (b) the exact Step B keys
// with only a PENDING resolution (not executing) are refused.
func TestK3_X07_LedgerPrefixTriggerBindsTypeAndExecutingState(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))
	reserved := ReservedDeclaredTxID(r.ID)
	key := w.provider + ":" + reserved

	// (b) pending only.
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), "withdrawal_completed", key, &w.provider, &reserved, wr.ID)
		})
		if k3Code(err) != "MR020" {
			t.Errorf("a withdrawal_completed with the reserved id and only a PENDING resolution: want MR020, got %v", err)
		}
		return nil
	})
	// (a) executing, another type with the same keys.
	err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
		for _, typ := range []string{"casino_win", "manual_adjustment", "deposit"} {
			err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), typ, key, &w.provider, &reserved, wr.ID)
			})
			if k3Code(err) != "MR020" {
				t.Errorf("type %s reusing the executing M2's keys: want MR020, got %v", typ, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// S26 / S27 / S37 / S38 / S35 / S36 / S32: direct attacks on the resolution and
// approval guards that the service never produces.
func TestK3_X08_ResolutionGuardsAgainstDirectStatements(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	ctx := context.Background()

	// S26: a client-chosen id is refused (the id is server-forced).
	if err := w.pool.WithPrincipalScope(ctx, w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO payment_manual_resolutions
					(id, tenant_id, attempt_id, operation, kind, basis_code, evidence_ref_hash, amount, asset_code, brand_id, reason_code,
					 attempt_state_at_submission, ever_possibly_sent_at_submission, payload_hash, requested_by, requested_by_scope,
					 requested_by_person_id, tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at)
				VALUES ($1, $2, $3, 'deposit', 'm2_declare_not_paid', 'provider_confirmed_out_of_band', $4, 1, '-', $5, 'x', '-', false, '-', $5, 'tenant', $5, '-', 1, '{}', now())`,
				uuid.New(), w.f.tenantID, a.ID, k3EvidenceHash(), uuid.Nil)
			return err
		})
		if k3Code(err) != "MR030" {
			t.Errorf("a client-chosen resolution id: want MR030, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// S27: an approval with a wrong payload hash is refused at insert.
	if err := w.pool.WithPrincipalScope(ctx, w.f.tenantID, w.f2.ID, func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_manual_resolution_approvals
				(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
				VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'k3')`, w.f.tenantID, r.ID, strings.Repeat("0", 64), uuid.Nil)
			return err
		})
		if k3Code(err) != "MR031" {
			t.Errorf("an approval with a stale payload hash: want MR031, got %v", err)
		}
		// S37: a direct reject with no reject decision in this transaction.
		err = k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'rejected' WHERE id = $1`, r.ID)
			return err
		})
		if k3Code(err) != "MR030" {
			t.Errorf("a direct state = rejected: want MR030, got %v", err)
		}
		// S32: only the requester may cancel (f2 is not the requester).
		err = k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'cancelled' WHERE id = $1`, r.ID)
			return err
		})
		if k3Code(err) != "MR030" {
			t.Errorf("cancel by a non-requester: want MR030, got %v", err)
		}
		// A pending resolution cannot be marked expired before it expires.
		err = k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'expired' WHERE id = $1`, r.ID)
			return err
		})
		if k3Code(err) != "MR030" {
			t.Errorf("a direct state = expired before expiry: want MR030, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Cancel(k3Ctx(w.f2), w.target(w.f2), r.ID, ResolutionMeta{}); err == nil {
		t.Error("the service let a non-requester cancel")
	}

	// S38: execution happens only in the final approval's own transaction: an
	// approval committed EARLIER (here by a direct insert) does not let another
	// session move the resolution to executing.
	if err := w.pool.WithPrincipalScope(ctx, w.f.tenantID, w.f2.ID, func(ctx context.Context, tx pgx.Tx) error {
		return k3InsertApproval(ctx, tx, w.f.tenantID, r, "approve")
	}); err != nil {
		t.Fatalf("setup: committed approval: %v", err)
	}
	if err := w.pool.WithPrincipalScope(ctx, w.f.tenantID, w.f3.ID, func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, r.ID)
			return err
		})
		if k3Code(err) != "MR030" {
			t.Errorf("executing without an approval of this transaction: want MR030, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// S31: an EXPIRED pending resolution takes no further approval (the approvals
// guard) and does not execute (the service). Expiry is 24 h and tests do not
// wait (plan 5.0 T-1), so this runs in a throwaway scratch database where the
// owner disables the resolution guard for ONE statement to backdate expires_at.
func TestK3_X10_ExpiredResolutionTakesNoApproval(t *testing.T) {
	pool, _ := scratchThrough(t, "k3exp_", migration0115Version)
	w := newK3WorldOn(t, pool, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	ctx := context.Background()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE payment_manual_resolutions DISABLE TRIGGER payment_manual_resolutions_guard`)
		return err
	}); err != nil {
		t.Fatalf("scratch DB: cannot disable the guard to backdate expiry: %v", err)
	}
	if err := pool.WithPrincipalScope(ctx, w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET expires_at = now() - interval '1 hour' WHERE id = $1`, r.ID)
		if err == nil && tag.RowsAffected() != 1 {
			t.Fatalf("backdate matched %d rows", tag.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE payment_manual_resolutions ENABLE TRIGGER payment_manual_resolutions_guard`)
		return err
	}); err != nil {
		t.Fatalf("re-enable the guard: %v", err)
	}
	// The approvals guard refuses a direct approval of an expired resolution.
	if err := pool.WithPrincipalScope(ctx, w.f.tenantID, w.f2.ID, func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			return k3InsertApproval(ctx, tx, w.f.tenantID, r, "approve")
		})
		if k3Code(err) != "MR031" {
			t.Errorf("approval of an expired resolution: want MR031, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The service does not execute it.
	if out, err := w.decide(w.f2, r, ResolutionApprove); err == nil && out.Executed {
		t.Fatalf("an expired resolution executed: %+v", out)
	}
	if got := w.attempt(a.ID); got.State != AttemptAmbiguous {
		t.Fatalf("attempt %s", got.State)
	}
}

// S35 / S36: M1 applies only to a disputed deposit, at insert; an executed M1
// never links a ledger transaction.
func TestK3_X09_M1GuardsAtInsertAndAtExecution(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	pending := rvInit(t, w.pool, w.orch, w.f.orchFixture, 5000, "k3-x09-"+uuid.NewString())
	if pending.Attempt.State == AttemptDisputed {
		t.Fatal("setup: the deposit is already disputed")
	}
	_, err := w.request(w.f1, w.m1In(pending.Attempt.ID, "awaiting_psp_refund"))
	k3RequireCode(t, err, "MR010")

	dep := w.disputedDeposit(5000)
	r := w.mustRequest(w.f1, w.m1In(dep.ID, "awaiting_psp_refund"))
	anyTx := w.sysQuery(`SELECT id FROM ledger_transactions WHERE tenant_id = $1 LIMIT 1`, w.f.tenantID)[0]["id"]
	if err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, r.ID, anyTx)
			return err
		})
		if err == nil {
			t.Error("an executed M1 linked a ledger transaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
