//go:build integration

package adjustment

// MA020-SYNC-MISMATCH-1 (migration 0119, Class-B B4) in the REAL K2 execution
// sessions: every Submit/Decide below goes through Service on a pool connected
// as the runtime role (TEST_RUNTIME_DATABASE_URL, asserted neither superuser
// nor BYPASSRLS), so the widened player_open_payment_exposure runs exactly as
// in production - inside ledger_adjustment_payload_refusal (submission and the
// -> executing guard) and the execute.go audit read, in the tenant staff
// session (WithPrincipalScope) and in the platform acting session
// (WithPlatformActingInTenant). Fixtures are written through the owner pool.
//
// RLS visibility (the fail-open hazard of section 6): the typed Y evidence is
// INVISIBLE in both K2 sessions (0115 R-4 system-shape policy) and the
// statement imports/lines are invisible in the acting session. The visibility
// pin below asserts that premise; the no-fail-open tests prove the SQL fails
// closed under it (F-VIS).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

const ma020Provider = "k2-mock-psp"

// park writes a disputed deposit attempt for the world's player through the
// payment_attempts guard (as openExposure does), optionally reference-less and
// optionally with typed Y evidence written in the park's own transaction.
func (w *world) park(reason, ref, y string) uuid.UUID {
	w.t.Helper()
	intent, attempt := uuid.New(), uuid.New()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, 1000, 'card', $7)`, intent, w.Tenant, w.Brand, w.Player, w.Wallet, w.Asset, "ma020-"+intent.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount,
			interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			VALUES ($1, $2, 'deposit', $3, 1, $4, 'card', $5, 1000, false, $6, $7, 'created', 'platform')`,
			attempt, w.Tenant, intent, ma020Provider, w.Asset, "ma020m-"+attempt.String()[:20], "ma020e-"+attempt.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'submitting', ever_possibly_sent = true, submit_count = 1, first_submitted_at = now(), last_sent_at = now() WHERE id = $1`, attempt); err != nil {
			return err
		}
		if ref != "" {
			if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'pending', provider_reference = $2, last_evidence_kind = 'sync' WHERE id = $1`, attempt, ref); err != nil {
				return err
			}
		}
		if y != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'poll_returned_reference', $4)`, w.Tenant, attempt, ma020Provider, y); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'disputed', terminal_reason = $2, last_evidence_kind = 'callback', next_action_at = NULL WHERE id = $1`, attempt, reason)
		return err
	}); err != nil {
		w.t.Fatalf("park %s: %v", reason, err)
	}
	return attempt
}

// ma020Source is a fixed statement source; synthetic (MOCK) unless real.
type ma020Source struct {
	stmt statement.PaymentStatement
}

func (ma020Source) SyntheticComponent() {}
func (ma020Source) Label() string       { return "MOCK ma020 k2 test statement (test-only)" }
func (ma020Source) ProviderID() string  { return ma020Provider }
func (s ma020Source) Fetch(context.Context, statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	return s.stmt, nil
}

// importLines stores one MOCK statement through the real fetch + ingest path.
func (w *world) importLines(lines ...statement.PaymentStatementLine) {
	w.t.Helper()
	now := time.Now().UTC()
	src := ma020Source{stmt: statement.PaymentStatement{CoverageStart: now.Add(-time.Hour), CoverageEnd: now, Lines: lines}}
	stmt, err := reconciliation.FetchPaymentStatement(context.Background(), src, w.Tenant, now.Add(-time.Hour), now, reconciliation.PaymentStatementOptions{})
	if err != nil {
		w.t.Fatalf("fetch: %v", err)
	}
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := reconciliation.IngestPaymentStatement(ctx, tx, w.Tenant, src, stmt, now)
		return err
	}); err != nil {
		w.t.Fatalf("ingest: %v", err)
	}
}

func ma020Line(kind, ref, original string) statement.PaymentStatementLine {
	return statement.PaymentStatementLine{ProviderID: ma020Provider, Kind: kind, ProviderReference: ref, OriginalProviderReference: original,
		Status: statement.PaymentStatusSucceeded, Amount: 1000, AssetCode: "EUR", OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}
}

func ma020Succ(ref string) statement.PaymentStatementLine {
	return ma020Line(statement.PaymentLineDeposit, ref, "")
}

func ma020Rev(original string) statement.PaymentStatementLine {
	return ma020Line(statement.PaymentLineDepositReversal, "ma020-rev-"+uuid.NewString()[:12], original)
}

func ma020Ref(tag string) string { return "ma020-" + tag + "-" + uuid.NewString()[:13] }

func (w *world) ma020Tombstone(ref string) { w.tombstoneFor(ma020Provider, ref) }

// sysExposure is the SQL value in the system session shape (the only shape
// that sees every input), through the runtime role.
func sysExposure(t *testing.T, rt *db.Pool, w *world) bool {
	t.Helper()
	var v bool
	if err := rt.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT player_open_payment_exposure($1, $2)`, w.Tenant, w.Player).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	return v
}

// creditOutcome submits a credit as actor through the runtime-role Service and
// returns "MA020" when refused for exposure, "" when admitted. An admitted
// credit is then decided by approver and must execute exactly once.
func creditOutcome(t *testing.T, svc *Service, w *world, actor, approver staffMember) string {
	t.Helper()
	in := w.credit(10, ReasonOperationalErrorCorrection)
	r, err := svc.Submit(ctxFor(actor), w.target(), in, Meta{RequestID: "ma020"})
	if pgCode(err) == "MA020" {
		return "MA020"
	}
	if err != nil {
		t.Fatalf("submit: unexpected %v", err)
	}
	out, err := svc.Decide(ctxFor(approver), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "ma020"}, Meta{RequestID: "ma020"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !out.Executed {
		if out.Request.RefusalCode != nil && *out.Request.RefusalCode == "open_payment_exposure" {
			return "MA020@execution"
		}
		t.Fatalf("admitted credit did not execute: %+v", out.Request)
	}
	return ""
}

// Visibility pin (the F-VIS premise; section 6 "prove it sees the rows"): in
// the system shape the runtime role sees the Y row, the statement lines and
// the tombstone; in the tenant K2 session it sees lines and tombstone but NOT
// the Y row; in the acting K2 session it sees the tombstone only. If a later
// policy change makes Y visible to K2, this test fails and F-VIS must be
// revisited (the fail-closed rule can then be relaxed to the full port).
func TestMA020_K2_RLSVisibilityPin(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	x, y := ma020Ref("vx"), ma020Ref("vy")
	w.park("poll_reference_mismatch", x, y)
	w.importLines(ma020Succ(x))
	w.ma020Tombstone(x)
	count := func(ctx context.Context, tx pgx.Tx) (ev, lines, imports, tombs, attempts int, err error) {
		for _, q := range []struct {
			sql string
			arg string
			dst *int
		}{
			{`SELECT count(*) FROM payment_attempt_reference_evidence WHERE reference = $1`, y, &ev},
			{`SELECT count(*) FROM payment_statement_lines WHERE provider_reference = $1`, x, &lines},
			{`SELECT count(*) FROM payment_statement_imports WHERE provider_id = $1`, ma020Provider, &imports},
			{`SELECT count(*) FROM ledger_transactions WHERE transaction_type = 'tombstone' AND provider_tx_id = $1`, x, &tombs},
			{`SELECT count(*) FROM payment_attempts WHERE provider_reference = $1`, x, &attempts},
		} {
			if err = tx.QueryRow(ctx, q.sql, q.arg).Scan(q.dst); err != nil {
				return
			}
		}
		return
	}
	type vis struct{ ev, lines, imports, tombs, attempts int }
	var sys, ten, act vis
	if err := rt.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sys.ev, sys.lines, sys.imports, sys.tombs, sys.attempts, err = count(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.WithPrincipalScope(context.Background(), w.Tenant, w.F1.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ten.ev, ten.lines, ten.imports, ten.tombs, ten.attempts, err = count(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.WithPlatformActingInTenant(context.Background(), w.Acting.ID, w.Tenant, uuid.Nil, OperationKind, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		act.ev, act.lines, act.imports, act.tombs, act.attempts, err = count(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if sys != (vis{1, 1, 1, 1, 1}) {
		t.Fatalf("system shape must see every input (non-vacuity), got %+v", sys)
	}
	if ten != (vis{0, 1, 1, 1, 1}) {
		t.Fatalf("tenant K2 session visibility changed (F-VIS premise): %+v", ten)
	}
	if act != (vis{0, 0, 0, 1, 1}) {
		t.Fatalf("acting K2 session visibility changed (F-VIS premise): %+v", act)
	}
}

// Every R-MA-1 reason blocks credits in BOTH K2 sessions while uncleared, and a
// tombstone on the bound reference clears it (normal path; one execution each).
func TestMA020_K2_BoundReasonsBlockUntilTombstone(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	svc := NewService(rt)
	for _, reason := range []string{"multiple_success_for_intent", "sync_amount_mismatch", "poll_amount_mismatch",
		"callback_amount_asset_mismatch", "provider_reference_conflict", "success_for_never_sent_attempt"} {
		t.Run(reason, func(t *testing.T) {
			w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
			x := ma020Ref("b")
			w.park(reason, x, "")
			if got := creditOutcome(t, svc, w, w.F1, w.F2); got != "MA020" {
				t.Fatalf("tenant session: want MA020, got %q", got)
			}
			if got := creditOutcome(t, svc, w, w.Acting, w.Acting2); got != "MA020" {
				t.Fatalf("acting session: want MA020, got %q", got)
			}
			w.ma020Tombstone(x)
			before := w.adjustmentLedgerTxCount()
			if got := creditOutcome(t, svc, w, w.F1, w.F2); got != "" {
				t.Fatalf("tenant session after tombstone: want admitted, got %q", got)
			}
			if got := creditOutcome(t, svc, w, w.Acting, w.Acting2); got != "" {
				t.Fatalf("acting session after tombstone: want admitted, got %q", got)
			}
			if after := w.adjustmentLedgerTxCount(); after != before+2 {
				t.Fatalf("want exactly two postings, got %d -> %d", before, after)
			}
			w.assertInvariants()
		})
	}
}

// R-MA-3: an eligible reversal line clears in the tenant session; the acting
// session cannot read statement lines and stays refused (fail closed, the
// documented acting-session residual: tombstone-only clearing there).
func TestMA020_K2_ReversalLineClearsInTenantSessionOnly(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	svc := NewService(rt)
	w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	x := ma020Ref("rl")
	w.park("sync_amount_mismatch", x, "")
	w.importLines(ma020Rev(x))
	if sysExposure(t, rt, w) {
		t.Fatal("system shape: an eligible reversal line must clear")
	}
	if got := creditOutcome(t, svc, w, w.F1, w.F2); got != "" {
		t.Fatalf("tenant session: want admitted, got %q", got)
	}
	if got := creditOutcome(t, svc, w, w.Acting, w.Acting2); got != "MA020" {
		t.Fatalf("acting session (lines invisible): want MA020 (fail closed), got %q", got)
	}
	w.assertInvariants()
}

// No fail-open (G-Y2 under invisible Y): X and Y both evidenced, X tombstoned,
// Y not cleared. The recon rule keeps the finding; the K2 sessions must refuse.
// After Y is cleared too, the system shape admits but the K2 sessions still
// refuse (F-VIS: Y cannot be checked there) - the pinned stranding residual.
func TestMA020_K2_NoFailOpenWhenYIsInvisible(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	svc := NewService(rt)
	w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	x, y := ma020Ref("gx"), ma020Ref("gy")
	w.park("poll_reference_mismatch", x, y)
	w.importLines(ma020Succ(x), ma020Succ(y))
	w.ma020Tombstone(x)
	if !sysExposure(t, rt, w) {
		t.Fatal("system shape: G-Y2 must keep the exposure open while Y is uncleared")
	}
	for _, s := range []struct {
		name            string
		actor, approver staffMember
	}{{"tenant", w.F1, w.F2}, {"acting", w.Acting, w.Acting2}} {
		if got := creditOutcome(t, svc, w, s.actor, s.approver); got != "MA020" {
			t.Fatalf("%s session: G-Y2 with invisible Y must refuse (no fail-open), got %q", s.name, got)
		}
	}
	w.ma020Tombstone(y)
	if sysExposure(t, rt, w) {
		t.Fatal("system shape: both cleared must admit")
	}
	for _, s := range []struct {
		name            string
		actor, approver staffMember
	}{{"tenant", w.F1, w.F2}, {"acting", w.Acting, w.Acting2}} {
		if got := creditOutcome(t, svc, w, s.actor, s.approver); got != "MA020" {
			t.Fatalf("%s session: F-VIS keeps a poll_reference_mismatch park open where Y is invisible, got %q", s.name, got)
		}
	}
	if n := w.adjustmentLedgerTxCount(); n != 0 {
		t.Fatalf("no credit may have posted, got %d", n)
	}
	w.assertInvariants()
}

// R-MA-2 (security review: relaxes 0113's fail-closed case): reference-less
// parks - the legacy pre-B3 multiple_success/callback mismatch form and the
// unbound invalid_provider_reference forms - and the excluded net-zero reason
// do not block credits. Their detective control is the reconciliation
// standing finding, not MA020.
func TestMA020_K2_ReferenceLessAndExcludedParksDoNotBlock(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	svc := NewService(rt)
	w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	w.park("multiple_success_for_intent", "", "")
	w.park("callback_amount_asset_mismatch", "", "")
	w.park("provider_reference_conflict", "", "")
	w.park("invalid_provider_reference:too_long", "", "")
	w.park("invalid_provider_reference", ma020Ref("ipr"), "")
	w.park("reversal_tombstone_precedes_success", ma020Ref("rtp"), "")
	if got := creditOutcome(t, svc, w, w.F1, w.F2); got != "" {
		t.Fatalf("tenant session: want admitted, got %q", got)
	}
	if got := creditOutcome(t, svc, w, w.Acting, w.Acting2); got != "" {
		t.Fatalf("acting session: want admitted, got %q", got)
	}
	// Debits are never affected; a bound park added later blocks again.
	w.park("sync_amount_mismatch", ma020Ref("late"), "")
	if got := creditOutcome(t, svc, w, w.F1, w.F2); got != "MA020" {
		t.Fatalf("a bound park must still block: %q", got)
	}
	w.assertInvariants()
}

// Concurrency: the exposure is read in the execute tx after the L1/L2 locks
// (Step 6), then re-checked by the -> executing guard. (a) a tombstone that
// commits after submission but before Step 6 admits, and the executed audit
// row records open_payment_exposure_at_execution=false; (b) a bound park that
// commits at the same point refuses at execution with no posting; (c) a park
// committing after the -> executing guard does not undo a consistent admit,
// and the posting happens exactly once.
func TestMA020_K2_ConcurrentClearOrParkDuringExecution(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	svc := NewService(rt)
	decide := func(w *world, r Request) Outcome {
		out, err := svc.Decide(ctxFor(w.F2), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "ma020"}, Meta{RequestID: "ma020"})
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		return out
	}
	t.Run("tombstone commits before the execution re-check", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
		r, err := svc.Submit(ctxFor(w.F1), w.target(), w.credit(10, ReasonOperationalErrorCorrection), Meta{RequestID: "ma020"})
		if err != nil {
			t.Fatal(err)
		}
		x := ma020Ref("cc")
		w.park("sync_amount_mismatch", x, "")
		testHookAfterShareLocks = func(context.Context, uuid.UUID) { w.ma020Tombstone(x) }
		defer func() { testHookAfterShareLocks = nil }()
		out := decide(w, r)
		testHookAfterShareLocks = nil
		if !out.Executed {
			t.Fatalf("cleared before Step 6: want executed, got %+v", out.Request)
		}
		var at string
		if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT metadata->>'open_payment_exposure_at_execution' FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND action = 'ledger_adjustment.executed'`,
				w.Tenant, r.ID.String()).Scan(&at)
		}); err != nil || at != "false" {
			t.Fatalf("executed audit must record exposure=false, got %q %v", at, err)
		}
		if n := w.adjustmentLedgerTxCount(); n != 1 {
			t.Fatalf("want one posting, got %d", n)
		}
		w.assertInvariants()
	})
	t.Run("bound park commits before the execution re-check", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
		r, err := svc.Submit(ctxFor(w.F1), w.target(), w.credit(10, ReasonGoodwillCredit), Meta{RequestID: "ma020"})
		if err != nil {
			t.Fatal(err)
		}
		testHookAfterShareLocks = func(context.Context, uuid.UUID) { w.park("poll_amount_mismatch", ma020Ref("cp"), "") }
		defer func() { testHookAfterShareLocks = nil }()
		out := decide(w, r)
		testHookAfterShareLocks = nil
		if out.Executed || out.Request.State != StateRefusedAtExecution || *out.Request.RefusalCode != "open_payment_exposure" {
			t.Fatalf("want refused_at_execution open_payment_exposure, got %+v", out.Request)
		}
		if n := w.adjustmentLedgerTxCount(); n != 0 {
			t.Fatalf("no posting allowed, got %d", n)
		}
		// Duplicate / retry: deciding again is refused, nothing posts.
		if _, err := svc.Decide(ctxFor(w.F3), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "ma020"}, Meta{RequestID: "ma020"}); !errors.Is(err, ErrNotPending) {
			t.Fatalf("re-decide of a refused request: want ErrNotPending, got %v", err)
		}
		if n := w.adjustmentLedgerTxCount(); n != 0 {
			t.Fatalf("no posting allowed after retry, got %d", n)
		}
		w.assertInvariants()
	})
	t.Run("park commits after the executing guard", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
		r, err := svc.Submit(ctxFor(w.F1), w.target(), w.credit(10, ReasonGoodwillCredit), Meta{RequestID: "ma020"})
		if err != nil {
			t.Fatal(err)
		}
		testHookBeforePost = func(context.Context, uuid.UUID) error {
			w.park("sync_amount_mismatch", ma020Ref("cl"), "")
			return nil
		}
		defer func() { testHookBeforePost = nil }()
		out := decide(w, r)
		testHookBeforePost = nil
		if !out.Executed {
			t.Fatalf("admitted at the guard: want executed, got %+v", out.Request)
		}
		if _, err := svc.Decide(ctxFor(w.F3), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "ma020"}, Meta{RequestID: "ma020"}); !errors.Is(err, ErrNotPending) {
			t.Fatalf("duplicate decide: want ErrNotPending, got %v", err)
		}
		if n := w.adjustmentLedgerTxCount(); n != 1 {
			t.Fatalf("want exactly one posting, got %d", n)
		}
		if got := creditOutcome(t, svc, w, w.F1, w.F2); got != "MA020" {
			t.Fatalf("the later park blocks the next credit: %q", got)
		}
		w.assertInvariants()
	})
}

// Tenant isolation: tenant B's identical strings (a tombstone on A's X, a park
// on the same string) neither clear A nor block B beyond B's own data.
func TestMA020_K2_TenantIsolation(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	svc := NewService(rt)
	a := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	b := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	x := ma020Ref("ti")
	a.park("sync_amount_mismatch", x, "")
	b.ma020Tombstone(x)
	if got := creditOutcome(t, svc, a, a.F1, a.F2); got != "MA020" {
		t.Fatalf("tenant A: a foreign tombstone must not clear, got %q", got)
	}
	if got := creditOutcome(t, svc, b, b.F1, b.F2); got != "" {
		t.Fatalf("tenant B: A's park must not block, got %q", got)
	}
	a.ma020Tombstone(x)
	if got := creditOutcome(t, svc, a, a.F1, a.F2); got != "" {
		t.Fatalf("tenant A after its own tombstone: %q", got)
	}
	a.assertInvariants()
	b.assertInvariants()
}
