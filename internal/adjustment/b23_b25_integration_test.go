//go:build integration

package adjustment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// openExposure seeds the ledger-side form of the reconciliation
// capturedUnposted predicate for the world's player: a deposit attempt
// driven through the real state machine to disputed /
// multiple_success_for_intent. It returns the attempt's (provider_id,
// provider_reference) so a test can tombstone it. No statement source is
// configured anywhere in this package (B-23 "with no statement source").
func (w *world) openExposure() (providerID, providerRef string) {
	w.t.Helper()
	providerID, providerRef = "k2-mock-psp", "k2ref-"+uuid.NewString()
	intent, attempt := uuid.New(), uuid.New()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, 1000, 'card', $7)`, intent, w.Tenant, w.Brand, w.Player, w.Wallet, w.Asset, "k2-"+intent.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount,
			interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			VALUES ($1, $2, 'deposit', $3, 1, $4, 'card', $5, 1000, false, $6, $7, 'created', 'platform')`,
			attempt, w.Tenant, intent, providerID, w.Asset, "k2m-"+attempt.String()[:20], "k2e-"+attempt.String()); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE payment_attempts SET state = 'submitting', ever_possibly_sent = true, submit_count = 1, first_submitted_at = now(), last_sent_at = now() WHERE id = $1`,
			`UPDATE payment_attempts SET state = 'pending', provider_reference = $2, last_evidence_kind = 'sync' WHERE id = $1`,
			`UPDATE payment_attempts SET state = 'disputed', terminal_reason = 'multiple_success_for_intent', last_evidence_kind = 'callback', next_action_at = NULL WHERE id = $1`,
		} {
			var err error
			if q[len(q)-len("WHERE id = $1"):] == "WHERE id = $1" && contains(q, "$2") {
				_, err = tx.Exec(ctx, q, attempt, providerRef)
			} else {
				_, err = tx.Exec(ctx, q, attempt)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		w.t.Fatalf("seed open exposure: %v", err)
	}
	return providerID, providerRef
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// tombstoneFor posts the refund tombstone keyed on the ORIGINAL reference
// (orchestrator.go:1526-1528; LF K2-a pin).
func (w *world) tombstoneFor(providerID, ref string) {
	w.t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxTombstone,
			IdempotencyKey: providerID + ":" + ref, ProviderID: &providerID, ProviderTxID: &ref, CorrelationID: uuid.New()})
		return err
	}); err != nil {
		w.t.Fatalf("tombstone: %v", err)
	}
}

// B-23 (R), LF ruling 2 / F4 + K2-a: a credit (any reason code) under an
// open captured-unposted exposure is refused at submission (MA020) and at
// execution (refused_at_execution open_payment_exposure), with no statement
// source configured; debits are unaffected; a refund TOMBSTONE keyed on the
// original reference clears it; a deposit_reversal keyed on its own
// reference does NOT (the removed arm).
func TestB23_OpenPaymentExposureRefusesCredits(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	w.fund(5_000)
	bet := w.postCasino(ledger.TxCasinoBet, 1_000)

	// Submitted BEFORE the exposure opens: refused at execution.
	pre, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	pid, ref := w.openExposure()

	for _, in := range []SubmitInput{
		w.credit(10, ReasonOperationalErrorCorrection),
		w.credit(10, ReasonGoodwillCredit),
		w.credit(10, ReasonExternalInstruction),
		func() SubmitInput {
			in := w.credit(10, ReasonCompensatingEntry)
			in.CausationTransactionID = &bet
			return in
		}(),
	} {
		if _, err := w.submit(w.F1, in); pgCode(err) != "MA020" {
			t.Fatalf("credit (%s) under open exposure at submission: expected MA020, got %v", in.ReasonCode, err)
		}
	}
	out, err := w.decide(w.F2, pre, DecisionApprove)
	if err != nil {
		t.Fatal(err)
	}
	if out.Executed || out.Request.State != StateRefusedAtExecution || *out.Request.RefusalCode != "open_payment_exposure" {
		t.Fatalf("credit under exposure at execution: %+v", out.Request)
	}
	// Debits are unaffected.
	d, err := w.submit(w.F1, w.debit(10, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("debit under exposure must be allowed: %v", err)
	}
	if out, err := w.decide(w.F2, d, DecisionApprove); err != nil || !out.Executed {
		t.Fatalf("debit execution: %v %+v", err, out)
	}
	// A deposit_reversal keyed on its OWN reference does not clear it.
	_, _, _ = w.postDepositFamily(50)
	if _, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection)); pgCode(err) != "MA020" {
		t.Fatalf("exposure must stay open after an unrelated reversal, got %v", err)
	}
	// The refund tombstone on the ORIGINAL reference clears it.
	w.tombstoneFor(pid, ref)
	r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("credit after the refund tombstone must be admitted: %v", err)
	}
	if out, err := w.decide(w.F2, r, DecisionApprove); err != nil || !out.Executed {
		t.Fatalf("credit execution after clearing: %v %+v", err, out)
	}
	// The acting session sees the same exposure (ADR 0099 §6.5 SELECT).
	w2 := newWorld(t, worldOpts{base: 1})
	w2.openExposure()
	if _, err := w2.submit(w2.Acting, w2.credit(10, ReasonOperationalErrorCorrection)); pgCode(err) != "MA020" {
		t.Fatalf("acting credit under exposure: expected MA020, got %v", err)
	}
	w.assertInvariants()
}

// B-24 (R), LF ruling 3 + K2-b: in a suspended asset goodwill_credit is
// refused and a compensation succeeds; a suspension between submission and
// execution ends goodwill refused_at_execution; a tenant-disabled asset
// (the 0045 asset_authorizations layer) behaves the same.
func TestB24_SuspendedAsset(t *testing.T) {
	t.Run("platform-suspended asset", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1}) // created, never platform-authorized
		w.fund(2_000)
		bet := w.postCasino(ledger.TxCasinoBet, 500)
		if _, err := w.submit(w.F1, w.credit(10, ReasonGoodwillCredit)); pgCode(err) != "MA021" {
			t.Fatalf("goodwill in a suspended asset: expected MA021, got %v", err)
		}
		in := w.credit(100, ReasonCompensatingEntry)
		in.CausationTransactionID = &bet
		r, err := w.submit(w.F1, in)
		if err != nil {
			t.Fatalf("compensation in a suspended asset must be allowed: %v", err)
		}
		if out, err := w.decide(w.F2, r, DecisionApprove); err != nil || !out.Executed {
			t.Fatalf("compensation execution: %v %+v", err, out)
		}
		w.assertInvariants()
	})
	t.Run("suspended between submission and execution", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
		r, err := w.submit(w.F1, w.credit(10, ReasonGoodwillCredit))
		if err != nil {
			t.Fatalf("goodwill in an authorized asset: %v", err)
		}
		if err := w.pool.WithPlatformAdmin(context.Background(), w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := assetregistry.SetPlatformAuthorized(ctx, tx, w.Asset, false, assetActor(w.AdminA.ID))
			return err
		}); err != nil {
			t.Fatalf("deauthorize: %v", err)
		}
		out, err := w.decide(w.F2, r, DecisionApprove)
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || out.Request.State != StateRefusedAtExecution || *out.Request.RefusalCode != "asset_suspended" {
			t.Fatalf("goodwill after suspension: %+v", out.Request)
		}
		w.assertInvariants()
	})
	t.Run("K2-b tenant-disabled asset (0045 layer)", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
		r, err := w.submit(w.F1, w.credit(10, ReasonGoodwillCredit))
		if err != nil {
			t.Fatalf("goodwill: %v", err)
		}
		if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE asset_authorizations SET eligible = false WHERE tenant_id = $1 AND asset_code = $2 AND scope_kind = 'tenant' AND product IS NULL`,
				w.Tenant, w.Asset)
			if err == nil && tag.RowsAffected() != 1 {
				t.Fatalf("tenant disable: %d rows", tag.RowsAffected())
			}
			return err
		}); err != nil {
			t.Fatalf("tenant-disable: %v", err)
		}
		if _, err := w.submit(w.F1, w.credit(10, ReasonGoodwillCredit)); pgCode(err) != "MA021" {
			t.Fatalf("goodwill in a tenant-disabled asset: expected MA021, got %v", err)
		}
		out, err := w.decide(w.F2, r, DecisionApprove)
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || out.Request.State != StateRefusedAtExecution || *out.Request.RefusalCode != "asset_suspended" {
			t.Fatalf("goodwill after tenant disable: %+v", out.Request)
		}
		// Absent tenant row behaves the same (a second, never-authorized tenant layer).
		if _, err := w.submit(w.Acting, w.credit(10, ReasonGoodwillCredit)); pgCode(err) != "MA021" {
			t.Fatalf("acting goodwill in a tenant-disabled asset: expected MA021, got %v", err)
		}
		w.assertInvariants()
	})
}

// B-25 (R), LF ruling 4 + B-18: a post-cutover manual_adjustment posted by
// a test fixture OUTSIDE any request raises ledger_unlinked_manual_adjustment
// on the next ledger reconciliation run; a request-backed one does not; and
// the run's balance drift is zero.
func TestB25_B18_UnlinkedManualAdjustmentDetectorAndZeroDrift(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("governed execution: %v", err)
	}
	runRecon := func() []reconciliation.Mismatch {
		var ms []reconciliation.Mismatch
		if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			_, ms, err = reconciliation.RunLedgerVsProjection(ctx, tx, w.Tenant, time.Now().Add(-time.Hour), time.Now())
			return err
		}); err != nil {
			t.Fatalf("recon: %v", err)
		}
		return ms
	}
	if ms := runRecon(); len(ms) != 0 {
		t.Fatalf("request-backed adjustment must reconcile clean (B-18 drift = 0): %+v", ms)
	}
	// A fixture posting outside any request (the 19-file pattern).
	var rogue uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.Wallet
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: w.Asset},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: w.Asset})
		if err != nil {
			return err
		}
		reason := "fixture"
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fixture:" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{{LedgerAccountID: ids[1], Direction: ledger.Debit, Amount: 9}, {LedgerAccountID: ids[0], Direction: ledger.Credit, Amount: 9}}})
		rogue = res.TransactionID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // standing: raised again on every run
		ms := runRecon()
		if len(ms) != 1 || ms[0].MismatchKind != reconciliation.MismatchKindLedgerUnlinkedManualAdjustment || ms[0].ReconciliationKey != rogue.String() {
			t.Fatalf("run %d: expected exactly one ledger_unlinked_manual_adjustment for the rogue posting, got %+v", i, ms)
		}
	}
	w.assertInvariants()
}
