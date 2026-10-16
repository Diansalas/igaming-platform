//go:build integration

// ADR 0113 slice 1 (migration 0132): the withdrawal database state machine and the hold invariant,
// exercised LIVE as the RUNTIME role (igaming_runtime, TEST_RUNTIME_DATABASE_URL) - the credential the
// guards exist to constrain. Fixtures (tenant, brand, player, wallet, KYC, payout instrument, policy,
// approver) are provisioned by the table owner exactly as the rest of this package does; every write
// the assertions are about runs on the runtime pool, through the Go writers where the edge is legal.
//
// Coverage map (ADR 0113 s7): INSERT matrix (B1, WD025) and the header-first attacks; the full
// (from, to) graph matrix in a tenant session plus the player and acting session shapes; the 14(b2)
// revival exploit (plain, on a suspended brand, and inside a closure transaction, which still refuses
// LA023); column discipline (WD022); the workflow record (WD023); the B2 double-pay / strand attacks
// (WD041 immediately, WD042 at commit); WD040 for non-withdrawal postings to a hold account; check A
// (WD030-WD034, SET CONSTRAINTS ALL IMMEDIATE); concurrency races. Catalog pins and the migration's
// pre-flight / up-down-up are in migration_0132_integration_test.go.
package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

const a13PSP = "a13-psp"

func a13Code(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func a13RuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role integration test (a skip is NOT evidence)")
	}
	p, err := db.Connect(context.Background(), u, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime pool: %v", err)
	}
	t.Cleanup(p.Close)
	var who string
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text`).Scan(&who)
	}); err != nil {
		t.Fatal(err)
	}
	if who != "igaming_runtime" {
		t.Fatalf("runtime pool connects as %q, want igaming_runtime", who)
	}
	return p
}

type a13World struct {
	owner, rt   *db.Pool
	f           fixture
	approver    uuid.UUID
	fingerprint string
	n           int
}

func newA13(t *testing.T) *a13World {
	t.Helper()
	return newA13On(t, testPool(t), a13RuntimePool(t))
}

// newA13On builds the world on any database: owner provisions, rt (the runtime role, or the scratch
// owner on a throwaway migration database that grants the runtime role nothing) drives.
func newA13On(t *testing.T, owner, rt *db.Pool) *a13World {
	t.Helper()
	w := &a13World{owner: owner, rt: rt}
	w.f = seedFixture(t, owner, 100_000_000)
	mustSetWithdrawalPolicy(t, owner, w.f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))
	w.approver = mustCreateApprover(t, owner, w.f.tenantID)
	mustRunTx(t, owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT fingerprint FROM payout_instruments WHERE id = $1`, w.f.instrumentID).Scan(&w.fingerprint)
	})
	return w
}

// rtTx runs fn in one runtime tenant transaction and returns its error (COMMIT included).
func (w *a13World) rtTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.rt.WithTenant(context.Background(), w.f.tenantID, fn)
}

func (w *a13World) rtExec(sql string, args ...any) error {
	return w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

func (w *a13World) get(t *testing.T, id uuid.UUID) WithdrawalRequest {
	t.Helper()
	var wr WithdrawalRequest
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = GetByID(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatalf("read request %s: %v", id, err)
	}
	return wr
}

// at returns a fresh request in state s, driven by the REAL Go writers as the runtime role.
func (w *a13World) at(t *testing.T, s State) WithdrawalRequest {
	t.Helper()
	w.n++
	wr := mustRequestWithdrawal(t, w.rt, w.f, 1000+int64(w.n), fmt.Sprintf("a13-%s-%d-%s", s, w.n, uuid.NewString()[:8]))
	step := func(fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := w.rtTx(fn); err != nil {
			t.Fatalf("drive %s to %s: %v", wr.ID, s, err)
		}
	}
	toPending := func() {
		step(func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
	}
	toApproved := func() {
		toPending()
		step(func(ctx context.Context, tx pgx.Tx) error {
			ok, err := Approve(ctx, tx, wr.ID, w.approver, false, nil, alwaysEligible, auth.StepUpAssertion{})
			if err == nil && !ok {
				err = errors.New("not approved")
			}
			return err
		})
	}
	toSubmitted := func() {
		toApproved()
		step(func(ctx context.Context, tx pgx.Tx) error { return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP) })
	}
	switch s {
	case StateRequested:
	case StatePendingReview:
		toPending()
	case StateApproved:
		toApproved()
	case StateSubmitted:
		toSubmitted()
	case StateCompleted:
		toSubmitted()
		step(func(ctx context.Context, tx pgx.Tx) error {
			return Complete(ctx, tx, wr.ID, a13PSP, "ptx-"+wr.ID.String())
		})
	case StateFailed:
		toSubmitted()
		step(func(ctx context.Context, tx pgx.Tx) error { return Fail(ctx, tx, wr.ID, "a13_failed") })
	case StateCancelled:
		step(func(ctx context.Context, tx pgx.Tx) error { return Cancel(ctx, tx, wr.ID) })
	case StateRejected:
		toPending()
		step(func(ctx context.Context, tx pgx.Tx) error {
			return Reject(ctx, tx, wr.ID, w.approver, "a13_rejected", alwaysEligible)
		})
	default:
		t.Fatalf("a13World.at: no legal path to %s", s)
	}
	got := w.get(t, wr.ID)
	if got.State != s {
		t.Fatalf("drove %s to %s, got %s", wr.ID, s, got.State)
	}
	return got
}

// accounts returns (cash, hold, clearing) for the fixture wallet.
func (w *a13World) accounts() (uuid.UUID, uuid.UUID, uuid.UUID) {
	return w.f.cashAccountID, w.f.holdAccountID, w.clearing()
}

func (w *a13World) clearing() uuid.UUID {
	var id uuid.UUID
	_ = w.owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		return err
	})
	return id
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got := a13Code(err); got != code {
		t.Fatalf("%s: want SQLSTATE %s, got %q (%v)", what, code, got, err)
	}
}

func wantMsg(t *testing.T, what string, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("%s: want an error containing %q, got %v", what, sub, err)
	}
}

// ---------------------------------------------------------------------------------------------
// INSERT (B1)
// ---------------------------------------------------------------------------------------------

func TestADR0113_InsertMatrix_OnlyE0IsAdmitted(t *testing.T) {
	w := newA13(t)
	ins := func(state string, hold, release *uuid.UUID, providerID, providerRef *string) error {
		return w.rtExec(`INSERT INTO withdrawal_requests
			(tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, state,
			 hold_ledger_transaction_id, release_ledger_transaction_id, provider_id, provider_reference,
			 payout_instrument_id, payout_instrument_fingerprint)
			VALUES ($1, $2, $3, $4, 'EUR', 500, $5, $6, $7, $8, $9, $10, $11, $12)`,
			w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, "a13-ins-"+uuid.NewString(), state,
			hold, release, providerID, providerRef, w.f.instrumentID, w.fingerprint)
	}
	for _, s := range []string{"pending_review", "approved", "rejected", "submitted", "completed", "failed", "cancelled", "reversed"} {
		wantCode(t, "direct INSERT as "+s, ins(s, nil, nil, nil, nil), "WD025")
	}
	donor := w.at(t, StateRequested)
	x := "x"
	wantCode(t, "requested + hold link", ins("requested", donor.HoldLedgerTransactionID, nil, nil, nil), "WD025")
	wantCode(t, "requested + release link", ins("requested", nil, donor.HoldLedgerTransactionID, nil, nil), "WD025")
	wantCode(t, "requested + provider_id", ins("requested", nil, nil, &x, nil), "WD025")
	wantCode(t, "requested + provider_reference", ins("requested", nil, nil, nil, &x), "WD025")

	// Control: the legal E0 header alone passes every immediate check and is refused at COMMIT
	// because no hold is linked (A, WD031) - a header can never be committed without its hold.
	wantCode(t, "E0 header without a hold", ins("requested", nil, nil, nil, nil), "WD031")

	// Acting-shaped and player-scoped sessions: the BEFORE trigger refuses the shape before any
	// policy is consulted.
	for _, s := range []string{"approved", "submitted", "completed"} {
		err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true), set_config('app.acting_platform_principal_id', $2, true)`,
				w.f.tenantID.String(), uuid.NewString()); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests
				(tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, state, payout_instrument_id, payout_instrument_fingerprint)
				VALUES ($1, $2, $3, $4, 'EUR', 500, $5, $6, $7, $8)`,
				w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, "a13-act-"+uuid.NewString(), s, w.f.instrumentID, w.fingerprint)
			return err
		})
		wantCode(t, "acting-session INSERT as "+s, err, "WD025")
		err = w.rt.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests
				(tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, state, payout_instrument_id, payout_instrument_fingerprint)
				VALUES ($1, $2, $3, $4, 'EUR', 500, $5, $6, $7, $8)`,
				w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, "a13-pl-"+uuid.NewString(), s, w.f.instrumentID, w.fingerprint)
			return err
		})
		wantCode(t, "player-session INSERT as "+s, err, "WD025")
	}
}

// Header first, then UPDATE straight to approved / submitted / completed: every shortcut is an
// illegal edge; walking the legal edges without the evidence fails on the evidence.
func TestADR0113_HeaderFirstAttacks(t *testing.T) {
	w := newA13(t)
	header := func(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
		id := uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests
			(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
			VALUES ($1, $2, $3, $4, $5, 'EUR', 700, $6, $7, $8)`,
			id, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, "a13-hdr-"+id.String(), w.f.instrumentID, w.fingerprint)
		return id, err
	}
	for _, to := range []string{"approved", "submitted", "completed"} {
		err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			id, err := header(ctx, tx)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = $2, provider_id = CASE WHEN $2 = 'approved' THEN NULL ELSE 'p' END WHERE id = $1`, id, to)
			return err
		})
		wantCode(t, "header then requested -> "+to, err, "WD021")
	}
	// requested -> pending_review -> approved with no approve record: WD023.
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		id, err := header(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'pending_review' WHERE id = $1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, id)
		return err
	})
	wantCode(t, "header -> pending_review -> approved without a record", err, "WD023")
	// ... with a forged automated approve record, every immediate check passes, and the COMMIT is
	// refused because nothing holds the funds (A, WD031).
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		id, err := header(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'pending_review' WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, threshold_amount_at_decision, request_amount_at_decision)
			VALUES ($1, $2, $3, true, 'approve', 0, 700)`, w.f.tenantID, id, uuid.New()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'submitted', provider_id = 'p' WHERE id = $1`, id)
		return err
	})
	wantCode(t, "header walked to submitted with no hold", err, "WD031")
}

// ---------------------------------------------------------------------------------------------
// The graph matrix
// ---------------------------------------------------------------------------------------------

var a13States = []State{StateRequested, StatePendingReview, StateApproved, StateRejected, StateSubmitted,
	StateCompleted, StateFailed, StateCancelled, StateReversed}

func a13Terminal(s State) bool {
	switch s {
	case StateRejected, StateCancelled, StateFailed, StateCompleted, StateReversed:
		return true
	}
	return false
}

// legalEdge performs (from -> to) through its Go writer (E5 through DenyForCompliance) on a fresh
// request; ok=false when (from, to) is not a legal edge.
func (w *a13World) legalEdge(t *testing.T, from, to State) (bool, error) {
	t.Helper()
	type edge struct{ from, to State }
	switch (edge{from, to}) {
	case edge{StateRequested, StatePendingReview}, edge{StateRequested, StateCancelled}, edge{StatePendingReview, StateApproved},
		edge{StatePendingReview, StateRejected}, edge{StateApproved, StateRejected}, edge{StateApproved, StateSubmitted},
		edge{StateSubmitted, StateCompleted}, edge{StateSubmitted, StateFailed}:
	default:
		return false, nil
	}
	wr := w.at(t, from)
	return true, w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		switch (edge{from, to}) {
		case edge{StateRequested, StatePendingReview}:
			return MoveToPendingReview(ctx, tx, wr.ID)
		case edge{StateRequested, StateCancelled}:
			return Cancel(ctx, tx, wr.ID)
		case edge{StatePendingReview, StateApproved}:
			_, err := Approve(ctx, tx, wr.ID, w.approver, false, nil, alwaysEligible, auth.StepUpAssertion{})
			return err
		case edge{StatePendingReview, StateRejected}:
			return Reject(ctx, tx, wr.ID, w.approver, "a13_rejected", alwaysEligible)
		case edge{StateApproved, StateRejected}:
			params, decision := denialDecision(w.f, wr.ID)
			if _, err := LockApprovedForSubmission(ctx, tx, wr.ID); err != nil {
				return err
			}
			_, err := DenyForCompliance(ctx, tx, wr.ID, decision, params)
			return err
		case edge{StateApproved, StateSubmitted}:
			return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
		case edge{StateSubmitted, StateCompleted}:
			return Complete(ctx, tx, wr.ID, a13PSP, "ptx-"+wr.ID.String())
		default:
			return Fail(ctx, tx, wr.ID, "a13_failed")
		}
	})
}

func TestADR0113_GraphMatrix_TenantSession(t *testing.T) {
	w := newA13(t)
	// One request per reachable from-state, reused by every refused pair (a refusal changes nothing).
	rows := map[State]WithdrawalRequest{}
	for _, s := range a13States {
		if s != StateReversed {
			rows[s] = w.at(t, s)
		}
	}
	admitted := map[string]bool{}
	for _, from := range a13States {
		if from == StateReversed {
			continue // no legal path reaches it; covered by TestADR0113_ReversedIsTerminal (owner plant)
		}
		for _, to := range a13States {
			pair := string(from) + "->" + string(to)
			if from == to && !a13Terminal(from) {
				// same-state: admitted (updated_at only)
				if err := w.rtExec(`UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`, rows[from].ID); err != nil {
					t.Fatalf("%s same-state touch: %v", pair, err)
				}
				admitted[pair] = true
				continue
			}
			legal, err := w.legalEdge(t, from, to)
			if legal {
				if err != nil {
					t.Fatalf("%s legal edge refused: %v", pair, err)
				}
				admitted[pair] = true
				continue
			}
			// Illegal: the raw transition, with every column a writer would set, is refused.
			err = w.rtExec(`UPDATE withdrawal_requests SET state = $2, updated_at = now() WHERE id = $1`, rows[from].ID, string(to))
			want := "WD021"
			if a13Terminal(from) {
				want = "WD020"
			}
			if from == StateCompleted && to == StateReversed {
				// A5 (migration 0162): E11 is a legal edge now, but only with the reversal link and the
				// two-Person decisions (covered by internal/withdrawal a5_* and internal/payments a5_*); a
				// bare state change is refused by the column discipline first (WD022). Before 0162: WD020.
				want = "WD022"
			}
			wantCode(t, pair, err, want)
			if got := w.get(t, rows[from].ID); got.State != from {
				t.Fatalf("%s: refused UPDATE changed the state to %s", pair, got.State)
			}
		}
	}
	want := []string{
		"requested->requested", "pending_review->pending_review", "approved->approved", "submitted->submitted",
		"requested->pending_review", "requested->cancelled", "pending_review->approved", "pending_review->rejected",
		"approved->rejected", "approved->submitted", "submitted->completed", "submitted->failed",
	}
	if len(admitted) != len(want) {
		t.Fatalf("admitted %d pairs, want exactly %d: %v", len(admitted), len(want), admitted)
	}
	for _, p := range want {
		if !admitted[p] {
			t.Fatalf("legal pair %s was not admitted", p)
		}
	}
}

// A `reversed` row cannot be produced by any legal path (WD025 on INSERT, WD021 into it). The owner
// plants one inside a transaction that is always rolled back, with the guard AND check A disabled
// for the plant only (ADR 0110 9.4 owner tamper), re-enables both, and then: the guard (no role is
// exempt) refuses every UPDATE of the row, updated_at included (WD020); and check A's function
// refuses the row. A5 (migration 0162): the release (the completion) is now the legal release shape of
// a `reversed` row, so check A reports the missing reversal link (WD035); before 0162 it was WD033.
func TestADR0113_ReversedIsTerminal(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateCompleted)
	ctx := context.Background()
	plant := func(tx pgx.Tx) {
		t.Helper()
		for _, q := range []string{
			`SELECT set_config('app.tenant_id', '` + w.f.tenantID.String() + `', true)`,
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_ledger_link_at_commit`,
			`UPDATE withdrawal_requests SET state = 'reversed' WHERE id = '` + wr.ID.String() + `'`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_ledger_link_at_commit`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				_ = tx.Rollback(ctx) // never leave an aborted transaction holding the table lock
				t.Fatalf("plant %q: %v", q, err)
			}
		}
	}
	for _, q := range []string{
		`UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`,
		`UPDATE withdrawal_requests SET state = 'completed' WHERE id = $1`,
		`UPDATE withdrawal_requests SET state = 'requested', release_ledger_transaction_id = NULL WHERE id = $1`,
	} {
		func() {
			tx, err := w.owner.Raw().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			plant(tx)
			_, err = tx.Exec(ctx, q, wr.ID)
			_ = tx.Rollback(ctx)
			wantCode(t, "reversed row: "+q, err, "WD020")
		}()
	}
	tx, err := w.owner.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plant(tx)
	var v []string
	if err := tx.QueryRow(ctx, `SELECT withdrawal_request_ledger_link_violation($1, $2)`, wr.ID, w.f.tenantID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if len(v) != 2 || v[0] != "WD035" {
		t.Fatalf("check A on a reversed row without a reversal link: %v, want WD035", v)
	}
}

func TestADR0113_PlayerAndActingSessionsCannotTransition(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateRequested)
	ctx := context.Background()
	for _, to := range []string{"pending_review", "cancelled", "approved"} {
		var n int64
		err := w.rt.WithPlayerScope(ctx, w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = $2 WHERE id = $1`, wr.ID, to)
			n = tag.RowsAffected()
			return err
		})
		if err != nil || n != 0 {
			t.Fatalf("player session requested -> %s: rows=%d err=%v (want 0 rows: no player UPDATE policy)", to, n, err)
		}
		err = w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true), set_config('app.acting_platform_principal_id', $2, true)`,
				w.f.tenantID.String(), uuid.NewString()); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = $2 WHERE id = $1`, wr.ID, to)
			n = tag.RowsAffected()
			return err
		})
		if err != nil || n != 0 {
			t.Fatalf("acting-shaped session without an executing window requested -> %s: rows=%d err=%v", to, n, err)
		}
	}
	if got := w.get(t, wr.ID); got.State != StateRequested {
		t.Fatalf("state changed to %s", got.State)
	}
}

// ---------------------------------------------------------------------------------------------
// ADR 0112 14(b2): revival
// ---------------------------------------------------------------------------------------------

func TestADR0113_RevivalExploit_14b2(t *testing.T) {
	w := newA13(t)
	ctx := context.Background()
	wr := w.at(t, StateCancelled)
	open := w.at(t, StateApproved) // the hold the closure below must still see
	// On an active brand.
	wantCode(t, "cancelled -> requested", w.rtExec(`UPDATE withdrawal_requests SET state = 'requested' WHERE id = $1`, wr.ID), "WD020")
	wantCode(t, "cancelled -> requested clearing the release", w.rtExec(
		`UPDATE withdrawal_requests SET state = 'requested', release_ledger_transaction_id = NULL WHERE id = $1`, wr.ID), "WD020")
	wantCode(t, "terminal updated_at", w.rtExec(`UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`, wr.ID), "WD020")

	// On a suspended brand (the live-confirmed shape of the exploit).
	launchfix.SetBrandStatus(t, w.f.tenantID, w.f.brandID, "suspended")
	wantCode(t, "revival on a suspended brand", w.rtExec(`UPDATE withdrawal_requests SET state = 'requested' WHERE id = $1`, wr.ID), "WD020")

	// Inside a closure transaction: an open (requested) request on the suspended tenant; the closure
	// tries to make it vanish from LA023's count with a raw cancel (no release leg): WD022. A raw
	// cancel of an approved hold is an illegal edge: WD021. The closure without the trick: LA023.
	launchfix.SetTenantStatus(t, w.f.tenantID, "suspended")
	tricks := map[string]string{
		`UPDATE withdrawal_requests SET state = 'cancelled' WHERE tenant_id = $1 AND state = 'approved'`:  "WD021",
		`UPDATE withdrawal_requests SET state = 'requested' WHERE tenant_id = $1 AND state = 'cancelled'`: "WD020",
	}
	for q, code := range tricks {
		err := launchfix.TrySetTenantStatusHooked(ctx, w.f.tenantID, "closed", launchfix.Hooks{
			BeforeUpdate: func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, w.f.tenantID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, q, w.f.tenantID)
				return err
			},
		})
		wantCode(t, "closure-transaction trick "+q, err, code)
	}
	wantCode(t, "closure with an open hold", launchfix.TrySetTenantStatus(ctx, w.f.tenantID, "closed"), "LA023")
	if got := w.get(t, open.ID); got.State != StateApproved {
		t.Fatalf("the open hold moved to %s", got.State)
	}
}

// ---------------------------------------------------------------------------------------------
// Column discipline (WD022)
// ---------------------------------------------------------------------------------------------

func TestADR0113_ColumnDiscipline(t *testing.T) {
	w := newA13(t)
	other := w.at(t, StateRequested)
	req := w.at(t, StateRequested)
	cases := []struct {
		name, sql string
		id        uuid.UUID
		arg       any
		want      string
	}{
		{"re-point the hold", `UPDATE withdrawal_requests SET hold_ledger_transaction_id = $2 WHERE id = $1`, req.ID, *other.HoldLedgerTransactionID, "WD022"},
		{"clear the hold", `UPDATE withdrawal_requests SET hold_ledger_transaction_id = NULL WHERE id = $1 AND $2::uuid IS NOT NULL`, req.ID, uuid.New(), "WD022"},
		{"release on a non-terminal edge (E1)", `UPDATE withdrawal_requests SET state = 'pending_review', release_ledger_transaction_id = $2 WHERE id = $1`, req.ID, *other.HoldLedgerTransactionID, "WD022"},
		{"release on a same-state touch", `UPDATE withdrawal_requests SET release_ledger_transaction_id = $2 WHERE id = $1`, req.ID, *other.HoldLedgerTransactionID, "WD022"},
		{"E2 without a release", `UPDATE withdrawal_requests SET state = 'cancelled' WHERE id = $1 AND $2::uuid IS NOT NULL`, req.ID, uuid.New(), "WD022"},
		{"provider_id on requested", `UPDATE withdrawal_requests SET provider_id = $2 WHERE id = $1`, req.ID, "p", "WD022"},
		{"amount (frozen)", `UPDATE withdrawal_requests SET amount = amount + 1 WHERE id = $1 AND $2::int IS NOT NULL`, req.ID, 1, "WD022"},
		{"requested_at (frozen)", `UPDATE withdrawal_requests SET requested_at = now() - interval '1 day' WHERE id = $1 AND $2::int IS NOT NULL`, req.ID, 1, "WD022"},
		{"payout binding (frozen)", `UPDATE withdrawal_requests SET payout_instrument_fingerprint = repeat('a', 64) WHERE id = $1 AND $2::int IS NOT NULL`, req.ID, 1, "WD022"},
	}
	appr := w.at(t, StateApproved)
	cases = append(cases,
		struct {
			name, sql string
			id        uuid.UUID
			arg       any
			want      string
		}{"E7 with NULL provider_id", `UPDATE withdrawal_requests SET state = 'submitted' WHERE id = $1 AND $2::int IS NOT NULL`, appr.ID, 1, "WD022"},
		struct {
			name, sql string
			id        uuid.UUID
			arg       any
			want      string
		}{"provider_reference on approved", `UPDATE withdrawal_requests SET provider_reference = $2 WHERE id = $1`, appr.ID, "r", "WD022"},
	)
	sub := w.at(t, StateSubmitted)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error { return AttachProviderReference(ctx, tx, sub.ID, "ref-1") }); err != nil {
		t.Fatalf("E8 (legal): %v", err)
	}
	cases = append(cases,
		struct {
			name, sql string
			id        uuid.UUID
			arg       any
			want      string
		}{"change provider_id after E7", `UPDATE withdrawal_requests SET provider_id = $2 WHERE id = $1`, sub.ID, "other-psp", "WD022"},
		struct {
			name, sql string
			id        uuid.UUID
			arg       any
			want      string
		}{"clear provider_id after E7", `UPDATE withdrawal_requests SET provider_id = NULL WHERE id = $1 AND $2::int IS NOT NULL`, sub.ID, 1, "WD022"},
		struct {
			name, sql string
			id        uuid.UUID
			arg       any
			want      string
		}{"overwrite provider_reference", `UPDATE withdrawal_requests SET provider_reference = $2 WHERE id = $1`, sub.ID, "ref-2", "WD022"},
		struct {
			name, sql string
			id        uuid.UUID
			arg       any
			want      string
		}{"clear provider_reference", `UPDATE withdrawal_requests SET provider_reference = NULL WHERE id = $1 AND $2::int IS NOT NULL`, sub.ID, 1, "WD022"},
	)
	done := w.at(t, StateCompleted)
	cases = append(cases, struct {
		name, sql string
		id        uuid.UUID
		arg       any
		want      string
	}{"updated_at on a terminal row", `UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1 AND $2::int IS NOT NULL`, done.ID, 1, "WD020"})
	for _, c := range cases {
		wantCode(t, c.name, w.rtExec(c.sql, c.id, c.arg), c.want)
	}
	// Legal same-state column writes still pass: updated_at on a non-terminal row.
	if err := w.rtExec(`UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`, appr.ID); err != nil {
		t.Fatalf("updated_at on approved: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Workflow record (WD023)
// ---------------------------------------------------------------------------------------------

func TestADR0113_WorkflowRecord(t *testing.T) {
	w := newA13(t)
	pr := w.at(t, StatePendingReview)
	wantCode(t, "E3 without an approve record", w.rtExec(`UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, pr.ID), "WD023")

	// E4 with the genuine `:rejected` release leg but no reject record.
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		post, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRejected, IdempotencyKey: pr.ID.String() + ":rejected",
			CorrelationID: pr.ID, ReversesTransactionID: pr.HoldLedgerTransactionID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.holdAccountID, Direction: ledger.Debit, Amount: pr.Amount},
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: pr.Amount},
			},
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected', release_ledger_transaction_id = $2 WHERE id = $1`, pr.ID, post.TransactionID)
		return err
	})
	wantCode(t, "E4 without a reject record", err, "WD023")

	// E3 with only a reject record.
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code, threshold_amount_at_decision, request_amount_at_decision)
			VALUES ($1, $2, $3, true, 'reject', 'x', 0, $4)`, w.f.tenantID, pr.ID, uuid.New(), pr.Amount); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, pr.ID)
		return err
	})
	wantCode(t, "E3 with only a reject record", err, "WD023")
}

// ---------------------------------------------------------------------------------------------
// B2: double pay and strand (B-i immediate WD041 / WD040, B-ii at commit WD042)
// ---------------------------------------------------------------------------------------------

func (w *a13World) postHold(ctx context.Context, tx pgx.Tx, typ ledger.TransactionType, key string, corr uuid.UUID, holdDir ledger.Direction, amount int64, counter uuid.UUID, provider *string) error {
	other := ledger.Credit
	if holdDir == ledger.Credit {
		other = ledger.Debit
	}
	in := ledger.TransactionInput{
		TenantID: w.f.tenantID, TransactionType: typ, IdempotencyKey: key, CorrelationID: corr,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: w.f.holdAccountID, Direction: holdDir, Amount: amount},
			{LedgerAccountID: counter, Direction: other, Amount: amount},
		},
	}
	if provider != nil {
		ptx := strings.TrimPrefix(key, *provider+":")
		in.ProviderID, in.ProviderTxID = provider, &ptx
	}
	_, err := ledger.Post(ctx, tx, in)
	return err
}

func TestADR0113_DoublePayAndStrandAttacks(t *testing.T) {
	w := newA13(t)
	cash, _, clearing := w.accounts()
	sub := w.at(t, StateSubmitted)
	done := w.at(t, StateCompleted)
	req := w.at(t, StateRequested)
	psp := a13PSP

	type attack struct {
		name string
		typ  ledger.TransactionType
		key  string
		corr uuid.UUID
		dir  ledger.Direction
		amt  int64
		ctr  uuid.UUID
		prov *string
		want string
	}
	attacks := []attack{
		{"second withdrawal_rejected, fresh key, vs submitted", ledger.TxWithdrawalRejected, sub.ID.String() + ":again", sub.ID, ledger.Debit, sub.Amount, cash, nil, "WD041"},
		{"withdrawal_rejected :rejected vs submitted", ledger.TxWithdrawalRejected, sub.ID.String() + ":rejected", sub.ID, ledger.Debit, sub.Amount, cash, nil, "WD041"},
		{"withdrawal_rejected :kyc_denied vs submitted", ledger.TxWithdrawalRejected, sub.ID.String() + ":kyc_denied", sub.ID, ledger.Debit, sub.Amount, cash, nil, "WD041"},
		{"second withdrawal_failed, fresh key, vs submitted", ledger.TxWithdrawalFailed, sub.ID.String() + ":failed2", sub.ID, ledger.Debit, sub.Amount, cash, nil, "WD041"},
		{"second withdrawal_rejected, fresh key, vs completed", ledger.TxWithdrawalRejected, done.ID.String() + ":again", done.ID, ledger.Debit, done.Amount, cash, nil, "WD041"},
		{"withdrawal_failed :failed vs completed", ledger.TxWithdrawalFailed, done.ID.String() + ":failed", done.ID, ledger.Debit, done.Amount, cash, nil, "WD041"},
		{"withdrawal_rejected :cancelled vs completed", ledger.TxWithdrawalRejected, done.ID.String() + ":cancelled", done.ID, ledger.Debit, done.Amount, cash, nil, "WD041"},
		{"second completion (other provider tx) vs completed", ledger.TxWithdrawalCompleted, psp + ":ptx2-" + done.ID.String(), done.ID, ledger.Debit, done.Amount, clearing, &psp, "WD041"},
		{"completion under a different provider vs submitted", ledger.TxWithdrawalCompleted, "evil-psp:ptx-" + sub.ID.String(), sub.ID, ledger.Debit, sub.Amount, clearing, ptr("evil-psp"), "WD041"},
		{"second withdrawal_requested leg (strand)", ledger.TxWithdrawalRequested, req.ID.String() + ":2", req.ID, ledger.Credit, req.Amount, cash, nil, "WD041"},
		{"hold leg of the wrong amount", ledger.TxWithdrawalFailed, sub.ID.String() + ":failed", sub.ID, ledger.Debit, sub.Amount - 1, cash, nil, "WD041"},
		{"Cr hold on a withdrawal_failed", ledger.TxWithdrawalFailed, sub.ID.String() + ":failed", sub.ID, ledger.Credit, sub.Amount, cash, nil, "WD041"},
		{"hold leg naming no request", ledger.TxWithdrawalRejected, uuid.NewString() + ":rejected", uuid.New(), ledger.Debit, 5, cash, nil, "WD040"},
		{"deposit into the hold account", ledger.TxDeposit, "a13-dep-" + uuid.NewString(), uuid.New(), ledger.Credit, 5, clearing, ptr("a13-dep"), "WD040"},
		// The same non-withdrawal types CORRELATED to a live request (matching wallet, asset, amount):
		// refused on the type itself (WD040), not merely because no request was found.
		{"deposit correlated to a live request", ledger.TxDeposit, "a13-dep-" + uuid.NewString(), req.ID, ledger.Credit, req.Amount, clearing, ptr("a13-dep"), "WD040"},
		{"casino_bet correlated to a live request", ledger.TxCasinoBet, "a13-bet-" + uuid.NewString(), req.ID, ledger.Credit, req.Amount, cash, nil, "WD040"},
	}
	for _, a := range attacks {
		err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			return w.postHold(ctx, tx, a.typ, a.key, a.corr, a.dir, a.amt, a.ctr, a.prov)
		})
		wantCode(t, a.name, err, a.want)
	}

	// Correctly keyed, correctly pre-stated legs whose request is never linked: B-i admits the leg,
	// B-ii refuses the COMMIT (A alone would not fire: the request row is never touched).
	unlinked := []attack{
		{"genuine :failed leg, request left submitted", ledger.TxWithdrawalFailed, sub.ID.String() + ":failed", sub.ID, ledger.Debit, sub.Amount, cash, nil, "WD042"},
		{"genuine completion leg, request left submitted", ledger.TxWithdrawalCompleted, psp + ":ptx-" + sub.ID.String(), sub.ID, ledger.Debit, sub.Amount, clearing, &psp, "WD042"},
		{"genuine :cancelled leg, request left requested", ledger.TxWithdrawalRejected, req.ID.String() + ":cancelled", req.ID, ledger.Debit, req.Amount, cash, nil, "WD042"},
	}
	for _, a := range unlinked {
		err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			return w.postHold(ctx, tx, a.typ, a.key, a.corr, a.dir, a.amt, a.ctr, a.prov)
		})
		wantCode(t, a.name, err, a.want)
		wantMsg(t, a.name, err, "does not link") // B-ii's own link check, not only A's WD034 behind it
	}

	// Nothing moved: every request still nets exactly its own state's hold.
	for _, r := range []WithdrawalRequest{sub, done, req} {
		if got := w.get(t, r.ID); got.State != r.State || !uuidPtrEq(got.ReleaseLedgerTransactionID, r.ReleaseLedgerTransactionID) {
			t.Fatalf("%s changed: %+v", r.ID, got)
		}
	}
	if got, want := holdBalance(t, w.owner, w.f), sub.Amount+req.Amount; got != want {
		t.Fatalf("hold balance %d, want %d (submitted + requested)", got, want)
	}

	// Every other account type is unaffected (type-blind mutant control): a deposit to player_cash.
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		p := "a13-dep"
		k := "a13-dep-ok-" + uuid.NewString()
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: k,
			ProviderID: &p, ProviderTxID: &k, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: 5}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 5}}})
		return err
	}); err != nil {
		t.Fatalf("a deposit to player_cash must be unaffected: %v", err)
	}
}

func ptr(s string) *string { return &s }

// a13PlantE0 is this package's copy of internal/testsupport/wdpath.Plant (wdpath imports this package,
// so the package's own tests cannot import it): E0 + E0b in tx, exactly the database effect of
// RequestWithdrawal without its Go-layer gates. A nil instrument plants the legacy NULL binding (only
// inside pitest.WithoutBindingGuard, owner).
func a13PlantE0(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerID, walletID uuid.UUID, amount int64, key string, instrumentID *uuid.UUID, fingerprint *string) (uuid.UUID, error) {
	id := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests
			(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
		 VALUES ($1, $2, $3, $4, $5, 'EUR', $6, $7, $8, $9)`,
		id, tenantID, brandID, playerID, walletID, amount, key, instrumentID, fingerprint); err != nil {
		return id, err
	}
	accts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"})
	if err != nil {
		return id, err
	}
	post, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: tenantID, TransactionType: ledger.TxWithdrawalRequested,
		IdempotencyKey: id.String(), CorrelationID: id,
		Entries: []ledger.EntryInput{{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: amount}, {LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: amount}}})
	if err != nil {
		return id, err
	}
	_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET hold_ledger_transaction_id = $1, updated_at = now() WHERE id = $2`, post.TransactionID, id)
	return id, err
}

func uuidPtrEq(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ---------------------------------------------------------------------------------------------
// Check A (WD030-WD034)
// ---------------------------------------------------------------------------------------------

func TestADR0113_CheckA(t *testing.T) {
	w := newA13(t)
	ctx := context.Background()
	a := w.at(t, StateRequested)
	b := w.at(t, StateRequested)

	// A foreign hold link (another request's hold): WD032 at commit.
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		id := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
			VALUES ($1, $2, $3, $4, $5, 'EUR', $6, $7, $8, $9)`, id, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, a.Amount, "a13-forhold-"+id.String(), w.f.instrumentID, w.fingerprint); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET hold_ledger_transaction_id = $2 WHERE id = $1`, id, *a.HoldLedgerTransactionID)
		return err
	})
	wantCode(t, "foreign hold link", err, "WD032")

	// E2 linking the HOLD transaction as the release (wrong type/key/direction): WD033.
	wantCode(t, "release = the hold transaction", w.rtExec(
		`UPDATE withdrawal_requests SET state = 'cancelled', release_ledger_transaction_id = hold_ledger_transaction_id WHERE id = $1`, a.ID), "WD033")
	// E2 linking ANOTHER request's genuine cancel release: WD033.
	cb := w.at(t, StateCancelled)
	wantCode(t, "foreign release", w.rtExec(
		`UPDATE withdrawal_requests SET state = 'cancelled', release_ledger_transaction_id = $2 WHERE id = $1`, b.ID, *cb.ReleaseLedgerTransactionID), "WD033")

	// completed linked to a withdrawal_failed leg (B-i admits the :failed leg on a submitted
	// request; at COMMIT the completed row does not have the completion shape). B-ii fires first in
	// event order and reports A's verdict (WD033) inside WD042.
	sub := w.at(t, StateSubmitted)
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		post, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalFailed,
			IdempotencyKey: sub.ID.String() + ":failed", CorrelationID: sub.ID, ReversesTransactionID: sub.HoldLedgerTransactionID,
			Entries: []ledger.EntryInput{{LedgerAccountID: w.f.holdAccountID, Direction: ledger.Debit, Amount: sub.Amount}, {LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: sub.Amount}}})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'completed', release_ledger_transaction_id = $2 WHERE id = $1`, sub.ID, post.TransactionID)
		return err
	})
	wantCode(t, "completed linked to a withdrawal_failed", err, "WD042")
	wantMsg(t, "completed linked to a withdrawal_failed", err, "WD033")

	// C-A: the session is player-scoped at COMMIT (the request is visible to the player, the
	// ledger only partly): WD030 on the session shape, before any read.
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := MoveToPendingReview(ctx, tx, b.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, w.f.playerAccountID.String())
		return err
	})
	wantCode(t, "player-scoped session at commit", err, "WD030")
	wantMsg(t, "player-scoped session at commit", err, "session")
	// ... and a session of the wrong tenant scope.
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := MoveToPendingReview(ctx, tx, b.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, uuid.NewString())
		return err
	})
	wantCode(t, "foreign tenant scope at commit", err, "WD030")
	// ... and an acting-SHAPED session of the right tenant with no valid acting grant: the session shape
	// passes, the request is not visible (acting_read needs a valid grant): fail closed, WD030.
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := MoveToPendingReview(ctx, tx, b.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true), set_config('app.acting_tenant_id', $1, true),
			set_config('app.acting_platform_principal_id', $2, true)`, w.f.tenantID.String(), uuid.NewString())
		return err
	})
	wantCode(t, "acting-shaped session without a grant at commit", err, "WD030")
	wantMsg(t, "acting-shaped session without a grant at commit", err, "not visible")

	// Release shapes that B-i already refuses at the posting (key pinned to the pre-state, reversal
	// link) are re-checked by A at COMMIT: planted by the owner with B-i / B-ii disabled for the
	// posting only (ADR 0110 9.4 history), then linked by a legal-looking E4. A refuses (WD033).
	for name, c := range map[string]struct {
		key      func(id uuid.UUID) string
		reverses bool
	}{
		"E4 linked to a `:cancelled` key":       {func(id uuid.UUID) string { return id.String() + ":cancelled" }, true},
		"E4 linked to a release reversing none": {func(id uuid.UUID) string { return id.String() + ":rejected" }, false},
	} {
		pr := w.at(t, StatePendingReview)
		err := func() error {
			tx, err := w.owner.Raw().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, s := range []string{
				`SELECT set_config('app.tenant_id', '` + w.f.tenantID.String() + `', true)`,
				`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
				`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
			} {
				if _, err := tx.Exec(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			in := ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRejected,
				IdempotencyKey: c.key(pr.ID), CorrelationID: pr.ID,
				Entries: []ledger.EntryInput{{LedgerAccountID: w.f.holdAccountID, Direction: ledger.Debit, Amount: pr.Amount}, {LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: pr.Amount}}}
			if c.reverses {
				in.ReversesTransactionID = pr.HoldLedgerTransactionID
			}
			post, err := ledger.Post(ctx, tx, in)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{`SET CONSTRAINTS ledger_entries_balanced IMMEDIATE`, `SET CONSTRAINTS ledger_entries_balanced DEFERRED`,
				`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
				`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`} {
				if _, err := tx.Exec(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code, threshold_amount_at_decision, request_amount_at_decision)
				VALUES ($1, $2, $3, true, 'reject', 'x', 0, $4)`, w.f.tenantID, pr.ID, uuid.New(), pr.Amount); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected', release_ledger_transaction_id = $2 WHERE id = $1`, pr.ID, post.TransactionID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		wantCode(t, name, err, "WD033")
	}

	// SET CONSTRAINTS ALL IMMEDIATE between the header and the hold fails closed (WD031).
	err = w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		id := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
			VALUES ($1, $2, $3, $4, $5, 'EUR', 10, $6, $7, $8)`, id, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, "a13-imm-"+id.String(), w.f.instrumentID, w.fingerprint); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`)
		return err
	})
	wantCode(t, "SET CONSTRAINTS ALL IMMEDIATE before the hold", err, "WD031")

	// WD034: a third correlated hold leg can only come from history (the owner with B-i/B-ii
	// disabled, ADR 0110 9.4); the next touch of the request is refused at COMMIT.
	tx, err := w.owner.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []string{
		`SELECT set_config('app.tenant_id', '` + w.f.tenantID.String() + `', true)`,
		`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
		`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.postHold(ctx, tx, ledger.TxWithdrawalRequested, a.ID.String()+":planted", a.ID, ledger.Credit, a.Amount, w.f.cashAccountID, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
		`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			if a13Code(err) == "55006" { // the balanced-check event of the planted legs is pending
				if _, err2 := tx.Exec(ctx, `SET CONSTRAINTS ledger_entries_balanced IMMEDIATE`); err2 != nil {
					t.Fatal(err2)
				}
				if _, err = tx.Exec(ctx, s); err == nil {
					continue
				}
			}
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "a third correlated hold leg", tx.Commit(ctx), "WD034")
}

// ---------------------------------------------------------------------------------------------
// Concurrency: one winner, the loser ErrStateConflict or a pinned SQLSTATE, the hold nets, no
// deadlock. Repeated 20 times per pair (run under -race).
// ---------------------------------------------------------------------------------------------

func TestADR0113_ConcurrentEdgeRaces(t *testing.T) {
	w := newA13(t)
	type pair struct {
		name  string
		from  State
		a, b  func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
		final map[State]bool
	}
	pairs := []pair{
		{"Cancel vs MoveToPendingReview", StateRequested,
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error { return Cancel(ctx, tx, id) },
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error { return MoveToPendingReview(ctx, tx, id) },
			map[State]bool{StateCancelled: true, StatePendingReview: true}},
		{"Approve vs Reject", StatePendingReview,
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				_, err := Approve(ctx, tx, id, w.approver, false, nil, alwaysEligible, auth.StepUpAssertion{})
				return err
			},
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return Reject(ctx, tx, id, w.approver, "a13_race", alwaysEligible)
			},
			map[State]bool{StateApproved: true, StateRejected: true}},
		{"Complete vs Fail", StateSubmitted,
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return Complete(ctx, tx, id, a13PSP, "ptx-"+id.String())
			},
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error { return Fail(ctx, tx, id, "a13_race") },
			map[State]bool{StateCompleted: true, StateFailed: true}},
		{"DenyForCompliance vs MarkSubmittedPending", StateApproved,
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				if _, err := LockApprovedForSubmission(ctx, tx, id); err != nil {
					return err
				}
				params, decision := denialDecision(w.f, id)
				_, err := DenyForCompliance(ctx, tx, id, decision, params)
				return err
			},
			func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
				return MarkSubmittedPending(ctx, tx, id, a13PSP)
			},
			map[State]bool{StateRejected: true, StateSubmitted: true}},
	}
	for _, p := range pairs {
		for i := 0; i < 20; i++ {
			wr := w.at(t, p.from)
			before := holdBalance(t, w.owner, w.f)
			var wg sync.WaitGroup
			errs := make([]error, 2)
			start := make(chan struct{})
			for j, fn := range []func(context.Context, pgx.Tx, uuid.UUID) error{p.a, p.b} {
				wg.Add(1)
				go func(j int, fn func(context.Context, pgx.Tx, uuid.UUID) error) {
					defer wg.Done()
					<-start
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					errs[j] = w.rt.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx, wr.ID) })
				}(j, fn)
			}
			close(start)
			wg.Wait()
			wins := 0
			for _, e := range errs {
				switch {
				case e == nil:
					wins++
				case errors.Is(e, ErrStateConflict):
				default:
					t.Fatalf("%s #%d: loser error is neither ErrStateConflict nor nil: %v (SQLSTATE %q)", p.name, i, e, a13Code(e))
				}
			}
			if wins != 1 {
				t.Fatalf("%s #%d: %d winners (%v)", p.name, i, wins, errs)
			}
			got := w.get(t, wr.ID)
			if !p.final[got.State] {
				t.Fatalf("%s #%d: final state %s", p.name, i, got.State)
			}
			after := holdBalance(t, w.owner, w.f)
			delta := before - after
			if a13Terminal(got.State) && delta != wr.Amount || !a13Terminal(got.State) && delta != 0 {
				t.Fatalf("%s #%d: hold moved by %d for final state %s (amount %d)", p.name, i, delta, got.State, wr.Amount)
			}
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Review round (security C1 / C2): the hold-account bypasses found live, as the runtime role.
// ---------------------------------------------------------------------------------------------

// C1: a wallet-less ledger account of a player type would skip B-i / B-ii (both fast-exit on a NULL
// wallet_id); the reviewer's exploit created one as igaming_runtime and posted manual_adjustment
// Dr house_gaming / Cr it. The account itself is now refused (23514), for every player_* type.
func TestADR0113_WalletlessPlayerAccountRefused_C1(t *testing.T) {
	w := newA13(t)
	for _, typ := range []string{"player_withdrawal_hold", "player_cash", "player_bonus", "player_locked_cash", "player_locked_bonus", "player_bonus_held"} {
		err := w.rtExec(`INSERT INTO ledger_accounts (id, tenant_id, account_type, asset_code, status) VALUES ($1, $2, $3, 'EUR', 'active')`,
			uuid.New(), w.f.tenantID, typ)
		wantCode(t, "wallet-less "+typ+" account", err, "23514")
		wantMsg(t, "wallet-less "+typ+" account", err, "ledger_accounts_player_types_have_wallet")
	}
	// House-level accounts (no wallet) are unaffected.
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
		return err
	}); err != nil {
		t.Fatalf("a house account must stay creatable: %v", err)
	}
}

// C2: a withdrawal_* posting that never touches a hold account (so B-i / B-ii never see it) is refused
// at COMMIT (WD043): the double-pay shape Dr psp_clearing / Cr player_cash under withdrawal_rejected with
// a fresh key against a COMPLETED request, and a withdrawal_completed Dr player_cash / Cr psp_clearing.
// withdrawal_reversed (no writer) is refused at its hold leg (B-i, WD040).
func TestADR0113_WithdrawalTypesRequireAHoldLeg_C2(t *testing.T) {
	w := newA13(t)
	cash, _, clearing := w.accounts()
	done := w.at(t, StateCompleted)
	sub := w.at(t, StateSubmitted)
	post := func(typ ledger.TransactionType, key string, corr uuid.UUID, dr, cr uuid.UUID, amt int64, prov *string) error {
		return w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			in := ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: typ, IdempotencyKey: key, CorrelationID: corr,
				Entries: []ledger.EntryInput{{LedgerAccountID: dr, Direction: ledger.Debit, Amount: amt}, {LedgerAccountID: cr, Direction: ledger.Credit, Amount: amt}}}
			if prov != nil {
				ptx := strings.TrimPrefix(key, *prov+":")
				in.ProviderID, in.ProviderTxID = prov, &ptx
			}
			_, err := ledger.Post(ctx, tx, in)
			return err
		})
	}
	psp := a13PSP
	wantCode(t, "withdrawal_rejected Dr psp_clearing / Cr cash vs completed, fresh key",
		post(ledger.TxWithdrawalRejected, done.ID.String()+":again", done.ID, clearing, cash, done.Amount, nil), "WD043")
	wantCode(t, "withdrawal_completed Dr cash / Cr psp_clearing (no hold leg)",
		post(ledger.TxWithdrawalCompleted, psp+":nohold-"+sub.ID.String(), sub.ID, cash, clearing, sub.Amount, &psp), "WD043")
	wantCode(t, "withdrawal_failed Dr psp_clearing / Cr cash vs submitted",
		post(ledger.TxWithdrawalFailed, sub.ID.String()+":failed", sub.ID, clearing, cash, sub.Amount, nil), "WD043")
	wantCode(t, "withdrawal_reversed through the hold",
		post(ledger.TxWithdrawalReversed, done.ID.String()+":reversed", done.ID, clearing, w.f.holdAccountID, done.Amount, nil), "WD040")
	if got := w.get(t, sub.ID); got.State != StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		t.Fatalf("the submitted request moved: %+v", got)
	}
	// Control: other types are unaffected (a deposit to player_cash commits).
	k := "a13-c2-dep-" + uuid.NewString()
	if err := post(ledger.TxDeposit, "a13-dep:"+k, uuid.New(), clearing, cash, 3, ptr("a13-dep")); err != nil {
		t.Fatalf("a deposit must be unaffected: %v", err)
	}
}
