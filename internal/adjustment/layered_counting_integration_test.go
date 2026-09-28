//go:build integration

package adjustment

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// Layered independence of the EXECUTION-TIME count (ADR 0100 §6.2; LF
// C-K1-3(a); security's K1-C3 layered methodology). Every rule the count
// applies is also enforced when an approval is inserted, so an insert-guard
// refusal would mask a count-level mutant. On a scratch database the
// approvals table's user triggers are disabled ONLY while one bad approval
// row is planted (committed), then re-enabled; a genuine second approval
// must then find that the bad one does not count (required = 2 stays
// unmet). Each case is a distinct §6.2 condition.
func TestLayered_ExecutionCountIgnoresBadApprovals(t *testing.T) {
	pool := scratchPoolThrough(t, "k2layer_", 113)
	ctx := context.Background()

	plant := func(w *world, r Request, decidedBy uuid.UUID, person uuid.UUID, payloadHash string) {
		t.Helper()
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			for _, q := range []string{
				`ALTER TABLE ledger_adjustment_approvals DISABLE TRIGGER USER`,
				`ALTER TABLE ledger_adjustment_approvals NO FORCE ROW LEVEL SECURITY`,
			} {
				if _, err := tx.Exec(ctx, q); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope,
				decided_by_person_id, decided_txid, reason_code) VALUES ($1, $2, 'approve', $3, $4, 'tenant', $5, txid_current(), 'planted')`,
				w.Tenant, r.ID, payloadHash, decidedBy, person); err != nil {
				return err
			}
			for _, q := range []string{
				`ALTER TABLE ledger_adjustment_approvals FORCE ROW LEVEL SECURITY`,
				`ALTER TABLE ledger_adjustment_approvals ENABLE TRIGGER USER`,
			} {
				if _, err := tx.Exec(ctx, q); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("plant approval (scratch): %v", err)
		}
	}
	check := func(t *testing.T, w *world, r Request, what string) {
		t.Helper()
		out, err := w.decide(w.F3, r, DecisionApprove)
		if err != nil {
			t.Fatalf("%s: genuine approval: %v", what, err)
		}
		if out.Executed || out.Counted != 1 || out.Request.State != StatePending {
			t.Fatalf("%s: the planted approval was COUNTED at execution (counted=%d executed=%v)", what, out.Counted, out.Executed)
		}
		w.assertInvariants()
	}

	t.Run("stale payload hash", func(t *testing.T) {
		w := newWorldOn(t, pool, worldOpts{base: 2})
		r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		plant(w, r, w.F2.ID, w.F2.PersonID, "00"+r.PayloadHash[2:])
		check(t, w, r, "stale payload hash")
	})
	t.Run("same Person as the initiator", func(t *testing.T) {
		w := newWorldOn(t, pool, worldOpts{base: 2})
		twin := mkStaff(t, pool, w.Tenant, "finance", w.F1.PersonID)
		ta2 := w.staff(w.Tenant, "tenant_admin")
		w.grantTenantBy(ta2, twin, capability.CapabilityLedgerAdjustmentApprove)
		r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		plant(w, r, twin.ID, twin.PersonID, r.PayloadHash)
		check(t, w, r, "initiator's Person")
	})
	t.Run("the beneficiary", func(t *testing.T) {
		w := newWorldOn(t, pool, worldOpts{base: 2})
		benef := mkStaff(t, pool, w.Tenant, "finance", w.PlayerPerson)
		w.grantTenant(benef, capability.CapabilityLedgerAdjustmentApprove)
		r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		plant(w, r, benef.ID, benef.PersonID, r.PayloadHash)
		check(t, w, r, "beneficiary")
	})
	t.Run("a contributing policy's author (S-2(iii))", func(t *testing.T) {
		w := newWorldOn(t, pool, worldOpts{base: 2})
		author := mkStaff(t, pool, w.Tenant, "finance", w.AdminA.PersonID)
		w.grantTenant(author, capability.CapabilityLedgerAdjustmentApprove)
		r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		plant(w, r, author.ID, author.PersonID, r.PayloadHash)
		check(t, w, r, "policy author")
	})
	t.Run("an approver without any grant", func(t *testing.T) {
		w := newWorldOn(t, pool, worldOpts{base: 2})
		r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		plant(w, r, w.FinanceNoGrant.ID, w.FinanceNoGrant.PersonID, r.PayloadHash)
		check(t, w, r, "no grant")
	})
	t.Run("two approvals, one Person", func(t *testing.T) {
		w := newWorldOn(t, pool, worldOpts{base: 2})
		twin := mkStaff(t, pool, w.Tenant, "finance", w.F3.PersonID)
		ta2 := w.staff(w.Tenant, "tenant_admin")
		w.grantTenantBy(ta2, twin, capability.CapabilityLedgerAdjustmentApprove)
		r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		plant(w, r, twin.ID, twin.PersonID, r.PayloadHash)
		// F3 shares twin's Person: the genuine insert refuses (distinct
		// Person on the request), so decide with F2 instead and require
		// that the planted twin + F2 count as 2 distinct Persons ONLY if
		// they really are distinct - here they are (twin=F3's Person, F2
		// distinct), so this case instead checks the dedupe by planting a
		// SECOND row for F3's Person too.
		plant(w, r, w.F3.ID, w.F3.PersonID, r.PayloadHash)
		out, err := w.decide(w.F2, r, DecisionApprove)
		if err != nil {
			t.Fatal(err)
		}
		// F2 + {twin, F3} (one Person) = 2 distinct Persons = required 2.
		if !out.Executed || out.Counted != 2 {
			t.Fatalf("one Person under two principals must count once (want counted=2 executed), got counted=%d executed=%v", out.Counted, out.Executed)
		}
		w.assertInvariants()
	})
}

// "Same transaction" is txid, never xmin (LF-10; ADR 0100 §6.5; mutant
// "use xmin"). The real executor never uses savepoints around its state
// changes, so an xmin-based check would be equivalent on the ordinary
// path; this test moves the request to 'executing' INSIDE a released
// savepoint (a subtransaction: the row's xmin is then the subtransaction's
// xid, not txid_current()) and requires the rest of the same top-level
// transaction to complete the execution normally.
func TestSameTxMarkerIsTxidNotXmin(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(40, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	err = w.tenantTx(w.F2, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'x')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT s1`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT s1`); err != nil {
			return err
		}
		wallet := w.Wallet
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: w.Asset},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: w.Asset})
		if err != nil {
			return err
		}
		req, err := GetInTx(ctx, tx, w.Tenant, r.ID)
		if err != nil {
			return err
		}
		in := buildPosting(req, ids[0], ids[1])
		res, err := ledger.Post(ctx, tx, in)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, r.ID, res.TransactionID)
		return err
	})
	if err != nil {
		t.Fatalf("an execution whose executing-step ran in a released savepoint must complete in the same top-level tx: %v", err)
	}
	if got := w.request(r.ID); got.State != StateExecuted {
		t.Fatalf("state %s", got.State)
	}
	w.assertInvariants()
}
