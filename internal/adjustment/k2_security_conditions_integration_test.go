//go:build integration

package adjustment

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// Security K2 review pre-merge conditions K2-C1, K2-C2 and K2-C3
// (docs/plans/prh2-hardening-round/reviews/k2-security.md). Every case
// bypasses the Go executor and drives the database directly, because the
// DB fences and the -> executing recount are the stated defence for a bug
// in, or compromise of, internal/adjustment.

// actingForge runs, in ONE acting session (w.Acting, valid G-P2 grants):
// the final approval (raw insert; the guard forces the actor columns), the
// pending -> executing transition (the DB recount passes at base 1), and
// then fn. It returns fn's error (the probe's verdict); the transaction is
// rolled back whenever fn fails.
func (w *world) actingForge(t *testing.T, r Request, fn func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error) error {
	t.Helper()
	var probeErr error
	err := w.pool.WithPlatformActingInTenant(context.Background(), w.Acting.ID, w.Tenant, uuid.New(), OperationKind, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash); err != nil {
			t.Fatalf("setup: attach proof: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'platform_acting', $4, 0, 'k2-c1-probe')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil); err != nil {
			t.Fatalf("setup: final approval: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			t.Fatalf("setup: -> executing: %v", err)
		}
		wallet := r.WalletID
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: r.AssetCode},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: r.AssetCode})
		if err != nil {
			t.Fatalf("setup: accounts: %v", err)
		}
		probeErr = fn(ctx, tx, ids[0], ids[1])
		return probeErr
	})
	if probeErr != nil {
		return probeErr
	}
	return err
}

func governedPost(ctx context.Context, tx pgx.Tx, w *world, r Request, entries ...ledger.EntryInput) (uuid.UUID, error) {
	reason := string(r.ReasonCode)
	res, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
		IdempotencyKey: "manual_adjustment:" + r.ID.String(), CorrelationID: r.ID, ReasonCode: &reason, Entries: entries})
	return res.TransactionID, err
}

// K2-C1: the acting ledger fences enforce the closed §4 shape per entry,
// and no non-executed exit may leave a governed-key posting behind.
func TestK2C1_ActingFenceEnforcesClosedShape(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	_, _, otherWallet := w.newPlayer()
	var cashBaseline int64

	assertUntouched := func(t *testing.T, r Request) {
		t.Helper()
		if got := w.request(r.ID); got.State != StatePending || got.LedgerTransactionID != nil {
			t.Fatalf("forged execution committed: %+v", got)
		}
		if got := w.playerCash(); got != cashBaseline {
			t.Fatalf("player cash moved: %d -> %d", cashBaseline, got)
		}
	}

	// Security's probe, verbatim shape: approved credit 500, posted 5000
	// under the request's governed key, then the refused exit.
	t.Run("probe: over-credit then refused_insufficient_funds", func(t *testing.T) {
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
			if _, err := governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Debit, Amount: 5_000},
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 5_000}); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'refused_insufficient_funds' WHERE id = $1`, r.ID)
			return err
		})
		if pgCode(err) != "CG030" {
			t.Fatalf("over-credit under the governed key: expected CG030, got %v", err)
		}
		assertUntouched(t, r)
	})

	// Fix (i) in isolation: the posting is EXACTLY the approved shape (so
	// the entries fence admits it); the refused exit must still refuse.
	t.Run("correct shape posted, then refused_insufficient_funds", func(t *testing.T) {
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
			if _, err := governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Debit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 500}); err != nil {
				t.Fatalf("the exact §4 shape must pass the entries fence: %v", err)
			}
			_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'refused_insufficient_funds' WHERE id = $1`, r.ID)
			return err
		})
		if pgCode(err) != "MA040" {
			t.Fatalf("refused exit with a governed-key posting: expected MA040, got %v", err)
		}
		assertUntouched(t, r)
	})

	t.Run("wrong wallet: another player's player_cash", func(t *testing.T) {
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, _, house uuid.UUID) error {
			ow := otherWallet
			other, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, &ow, ledger.AccountPlayerCash, r.AssetCode)
			if err != nil {
				t.Fatalf("setup: other player's account: %v", err)
			}
			_, err = governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Debit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: other, Direction: ledger.Credit, Amount: 500})
			return err
		})
		if pgCode(err) != "CG030" {
			t.Fatalf("wrong wallet: expected CG030, got %v", err)
		}
		assertUntouched(t, r)
	})

	t.Run("wrong direction: credit request posted as a debit", func(t *testing.T) {
		w.fund(1_000)
		cashBaseline = w.playerCash()
		before := cashBaseline
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
			_, err := governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Debit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Credit, Amount: 500})
			return err
		})
		if pgCode(err) != "CG030" {
			t.Fatalf("wrong direction: expected CG030, got %v", err)
		}
		if got := w.request(r.ID); got.State != StatePending {
			t.Fatalf("forged execution committed: %s", got.State)
		}
		if got := w.playerCash(); got != before {
			t.Fatalf("player cash moved: %d -> %d", before, got)
		}
	})

	t.Run("split legs: two player legs in one direction", func(t *testing.T) {
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, pc, _ uuid.UUID) error {
			// Two half-amount player credits (unbalanced, wrong amount, and a
			// duplicated direction); refused before any balance check.
			_, err := governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 250},
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 250})
			return err
		})
		if pgCode(err) != "CG030" {
			t.Fatalf("split legs: expected CG030, got %v", err)
		}
		assertUntouched(t, r)
	})

	// Every entry individually matches a §4 leg (right account, asset,
	// amount, direction) and the posting balances, but the shape is doubled:
	// only the at-most-two / one-per-direction arm refuses it.
	t.Run("duplicated legs, each individually valid and balanced", func(t *testing.T) {
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
			_, err := governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Debit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Debit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 500})
			return err
		})
		if pgCode(err) != "CG030" {
			t.Fatalf("duplicated legs: expected CG030, got %v", err)
		}
		assertUntouched(t, r)
	})
	w.assertInvariants()
}

// K2-C2: the fence's state arm. (a) Security's required case: execute,
// then append an extra entry to the linked transaction in the same acting
// transaction -> CG030. With K2-C1 (ii) in place this case is ALSO refused
// by the entry-count arm, so it does not isolate the state arm; (b) does:
// after a legitimate refused_insufficient_funds exit (nothing posted), a
// governed-key posting of EXACTLY the approved shape in the same
// transaction must be refused. Without `r.state = 'executing'` the fence
// admits it (executed_txid still equals txid_current()).
func TestK2C2_FenceStateArmAfterExit(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})

	t.Run("extra entry after executed", func(t *testing.T) {
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
			txID, err := governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Debit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 500})
			if err != nil {
				t.Fatalf("setup: exact posting: %v", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, r.ID, txID); err != nil {
				t.Fatalf("setup: -> executed: %v", err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
				VALUES ($1, $2, $3, $4, 'credit', 500)`, txID, pc, w.Tenant, r.AssetCode)
			return err
		})
		if pgCode(err) != "CG030" {
			t.Fatalf("extra entry after executed: expected CG030, got %v", err)
		}
		if got := w.request(r.ID); got.State != StatePending {
			t.Fatalf("forged execution committed: %s", got.State)
		}
	})

	t.Run("governed posting after the refused exit", func(t *testing.T) {
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		err = w.actingForge(t, r, func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) error {
			if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'refused_insufficient_funds' WHERE id = $1`, r.ID); err != nil {
				t.Fatalf("setup: legitimate refused exit (nothing posted): %v", err)
			}
			_, err := governedPost(ctx, tx, w, r,
				ledger.EntryInput{LedgerAccountID: house, Direction: ledger.Debit, Amount: 500},
				ledger.EntryInput{LedgerAccountID: pc, Direction: ledger.Credit, Amount: 500})
			return err
		})
		if pgCode(err) != "CG030" {
			t.Fatalf("posting after the refused exit: expected CG030, got %v", err)
		}
		if got := w.playerCash(); got != 0 {
			t.Fatalf("player cash moved: %d", got)
		}
	})
	w.assertInvariants()
}

// K2-C3: the DB recount on -> executing is the HD-PRH2-1 four-eyes
// backstop. Driven directly (no Go executor): the UPDATE itself must fail
// with MA030 - the error of the UPDATE is asserted, not the commit's (a
// committed 'executing' would also fail later with MA041, which must not
// mask a missing recount).
func TestK2C3_DBRecountOnExecuting(t *testing.T) {
	driveExecuting := func(t *testing.T, w *world, r Request, approver staffMember) error {
		t.Helper()
		var updErr error
		_ = w.tenantTx(approver, func(ctx context.Context, tx pgx.Tx) error {
			if err := prooftest.AttachForSession(ctx, tx, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash); err != nil {
				t.Fatalf("setup: attach proof: %v", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
				VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'k2-c3')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil); err != nil {
				t.Fatalf("setup: approval insert must be accepted (the recount is under test, not the insert guard): %v", err)
			}
			_, updErr = tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, r.ID)
			return updErr
		})
		return updErr
	}

	t.Run("one valid approval where two are required", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		if err := driveExecuting(t, w, r, w.F2); pgCode(err) != "MA030" {
			t.Fatalf("1 of 2 approvals: expected MA030 from the -> executing recount, got %v", err)
		}
		if got := w.request(r.ID); got.State != StatePending {
			t.Fatalf("state: %s", got.State)
		}
	})

	t.Run("invalid initiator (initiate grant revoked after submission)", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1})
		r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		w.revokeGrant(w.F1.ID, capability.CapabilityLedgerAdjustmentInitiate)
		if err := driveExecuting(t, w, r, w.F2); pgCode(err) != "MA030" {
			t.Fatalf("invalid initiator: expected MA030 from the -> executing recount, got %v", err)
		}
		if got := w.request(r.ID); got.State != StatePending {
			t.Fatalf("state: %s", got.State)
		}
	})
}

// Code review R-1 (with K2-C1 (i)), B-12 style, in a TENANT (non-acting)
// session - which has no ledger fence at all (LEDGER-MANUAL-ADJ-LINK-1 is
// deferred), so the request guard is the only control: once a ledger
// transaction carries the request's governed key, no non-executed exit is
// accepted (MA040).
func TestK2R1_TenantSessionNonExecutedExitRefusedAfterGovernedPosting(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	post := func(ctx context.Context, tx pgx.Tx, r Request) {
		t.Helper()
		wallet := r.WalletID
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: r.AssetCode},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: r.AssetCode})
		if err != nil {
			t.Fatalf("setup: accounts: %v", err)
		}
		if _, err := governedPost(ctx, tx, w, r,
			ledger.EntryInput{LedgerAccountID: ids[1], Direction: ledger.Debit, Amount: r.Amount},
			ledger.EntryInput{LedgerAccountID: ids[0], Direction: ledger.Credit, Amount: r.Amount}); err != nil {
			t.Fatalf("setup: governed posting in a tenant session: %v", err)
		}
	}
	approveRaw := func(ctx context.Context, tx pgx.Tx, r Request) {
		t.Helper()
		if err := prooftest.AttachForSession(ctx, tx, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash); err != nil {
			t.Fatalf("setup: attach proof: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'r1')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil); err != nil {
			t.Fatalf("setup: approval: %v", err)
		}
	}
	for _, c := range []struct {
		name  string
		actor staffMember
		run   func(ctx context.Context, tx pgx.Tx, r Request) error
	}{
		{"executing -> refused_insufficient_funds", w.F2, func(ctx context.Context, tx pgx.Tx, r Request) error {
			approveRaw(ctx, tx, r)
			if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
				t.Fatalf("setup: -> executing: %v", err)
			}
			post(ctx, tx, r)
			_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'refused_insufficient_funds' WHERE id = $1`, r.ID)
			return err
		}},
		{"pending -> refused_at_execution", w.F2, func(ctx context.Context, tx pgx.Tx, r Request) error {
			approveRaw(ctx, tx, r)
			post(ctx, tx, r)
			_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'refused_at_execution', refusal_code = 'x' WHERE id = $1`, r.ID)
			return err
		}},
		{"pending -> cancelled (initiator)", w.F1, func(ctx context.Context, tx pgx.Tx, r Request) error {
			post(ctx, tx, r)
			_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'cancelled' WHERE id = $1`, r.ID)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := w.submit(w.F1, w.credit(400, ReasonOperationalErrorCorrection))
			if err != nil {
				t.Fatal(err)
			}
			var exitErr error
			_ = w.tenantTx(c.actor, func(ctx context.Context, tx pgx.Tx) error {
				exitErr = c.run(ctx, tx, r)
				return exitErr
			})
			if pgCode(exitErr) != "MA040" {
				t.Fatalf("%s after a governed posting: expected MA040, got %v", c.name, exitErr)
			}
			if got := w.request(r.ID); got.State != StatePending {
				t.Fatalf("state committed: %s", got.State)
			}
		})
	}
	if got := w.playerCash(); got != 0 {
		t.Fatalf("player cash moved: %d", got)
	}
	w.assertInvariants()
}
