//go:build integration

package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func (w *k3World) sweep() SweepStats {
	w.t.Helper()
	sw := NewSweeper(w.pool, w.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	sw.PayoutKYCGate = KYCEnforcementPayoutGate{}
	st := sw.RunOnce(context.Background(), []uuid.UUID{w.f.tenantID})
	return st
}

// depositAmbiguousBound drives a real deposit to `ambiguous` with its reference
// bound (a sync success with no amount echo): the attempt a poll can resolve.
func (w *k3World) depositAmbiguousBound() (PaymentAttempt, string) {
	w.t.Helper()
	ref := "k3-poll-" + uuid.NewString()
	w.prov.setDeposit(func(req DepositRequest) DepositResult {
		return DepositResult{Outcome: OutcomeSucceeded, ProviderReference: ref, Amount: 0, AssetCode: "EUR"}
	})
	res := rvInit(w.t, w.pool, w.orch, w.f.orchFixture, 5000, "k3-amb-"+uuid.NewString())
	w.prov.setDeposit(nil)
	a := w.attempt(res.Attempt.ID)
	if a.State != AttemptAmbiguous || a.ProviderReference == nil || *a.ProviderReference != ref {
		w.t.Fatalf("setup: state=%s ref=%v, want ambiguous with %q bound", a.State, a.ProviderReference, ref)
	}
	return a, ref
}

// pollEcho polls the ambiguous attempt; the provider answers SUCCESS naming
// `echo` instead of the bound reference.
func (w *k3World) pollEcho(a PaymentAttempt, ref, echo string) SweepStats {
	w.t.Helper()
	w.prov.setStatus(ref, StatusResult{ProviderReference: echo, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"})
	setNextActionNow(w.t, w.pool, w.f.tenantID, a.ID)
	return w.sweep()
}

// parkPollMismatch drives the real poll_reference_mismatch park with a valid Y.
func (w *k3World) parkPollMismatch() (PaymentAttempt, string, string) {
	w.t.Helper()
	a, ref := w.depositAmbiguousBound()
	y := "k3-other-" + uuid.NewString()
	st := w.pollEcho(a, ref, y)
	if len(st.Errors) != 0 {
		w.t.Fatalf("sweep errors: %v", st.Errors)
	}
	got := w.attempt(a.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != TerminalReasonPollReferenceMismatch {
		w.t.Fatalf("setup: want disputed/poll_reference_mismatch, got %s %v", got.State, got.TerminalReason)
	}
	return got, ref, y
}

func (w *k3World) evidenceRows(attempt uuid.UUID) []string {
	w.t.Helper()
	var out []string
	rows := w.sysQuery(`SELECT reference FROM payment_attempt_reference_evidence WHERE tenant_id = $1 AND attempt_id = $2`, w.f.tenantID, attempt)
	for _, r := range rows {
		out = append(out, r["reference"].(string))
	}
	return out
}

// C-36 write side / C-37 (R/FL): the Y evidence row is written ONLY for a valid Y,
// in the park's own transaction; a prefixed or otherwise invalid Y stays
// audit-only; the park audit carries evidence_recorded.
func TestK3_C37_YEvidenceWrite(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	a, ref, y := w.parkPollMismatch()
	if got := w.evidenceRows(a.ID); len(got) != 1 || got[0] != y {
		t.Fatalf("Y evidence = %v, want exactly [%s]", got, y)
	}
	if w.attempt(a.ID).ProviderReference == nil || *w.attempt(a.ID).ProviderReference != ref {
		t.Fatal("the bound reference changed")
	}
	var md string
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'evidence_recorded' FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
			w.f.tenantID, a.ID.String()).Scan(&md)
	})
	if md != "true" {
		t.Fatalf("park audit evidence_recorded = %q", md)
	}
	// Invalid Ys are audit-only: a control character, a reserved prefix, an oversize value.
	for name, bad := range map[string]string{
		"control char":    "bad\x01ref",
		"reserved prefix": providerref.ReservedOperatorPrefix + "x",
		"too long":        strings.Repeat("a", 300),
	} {
		a2, ref2 := w.depositAmbiguousBound()
		st := w.pollEcho(a2, ref2, bad)
		if len(st.Errors) != 0 {
			t.Fatalf("%s: sweep errors: %v", name, st.Errors)
		}
		got := w.attempt(a2.ID)
		// A hostile echo parks the attempt (poll_reference_mismatch) or, when the echo
		// itself fails validation upstream, the invalid-reference park; either way no
		// evidence row exists and the echo value is never audited.
		if got.State != AttemptDisputed {
			t.Fatalf("%s: attempt %s, want disputed", name, got.State)
		}
		if rows := w.evidenceRows(a2.ID); len(rows) != 0 {
			t.Fatalf("%s: an invalid Y was persisted: %v", name, rows)
		}
		var meta string
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT metadata::text FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
				w.f.tenantID, a2.ID.String()).Scan(&meta)
		})
		if strings.Contains(meta, "bad\\u0001ref") || strings.Contains(meta, providerref.ReservedOperatorPrefix+"x") {
			t.Fatalf("%s: the hostile echo value reached the audit: %s", name, meta)
		}
	}
	w.assertInvariants()
}

// T-7 (R-4, D-6): tenant staff, player, acting and platform sessions can neither
// INSERT nor SELECT evidence; evidence for a non-parked attempt, or for a parked
// attempt with another reason, is refused at commit; a duplicate plain INSERT
// raises; UPDATE/DELETE/TRUNCATE are refused even for the owner.
func TestK3_T7_EvidenceTablePolicies(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	a, ref, y := w.parkPollMismatch()
	if len(w.evidenceRows(a.ID)) != 1 {
		t.Fatal("setup: no evidence row")
	}
	count := `SELECT count(*) FROM payment_attempt_reference_evidence`
	insert := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
			VALUES ($1, $2, $3, 'poll_returned_reference', 'k3-t7-other')`, w.f.tenantID, a.ID, w.provider)
		return err
	}
	scalar := func(ctx context.Context, tx pgx.Tx) (int, error) {
		var n int
		return n, tx.QueryRow(ctx, count).Scan(&n)
	}
	type session struct {
		name string
		run  func(fn func(ctx context.Context, tx pgx.Tx) error) error
	}
	ctx := context.Background()
	sessions := []session{
		{"tenant staff", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return w.pool.WithPrincipalScope(ctx, w.f.tenantID, w.f1.ID, fn)
		}},
		{"player", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return w.pool.WithPlayerScope(ctx, w.f.tenantID, w.f.playerAccountID, fn)
		}},
		{"platform", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return w.pool.WithPlatformAdmin(ctx, w.adminA.ID, fn)
		}},
		{"acting", func(fn func(ctx context.Context, tx pgx.Tx) error) error {
			return w.pool.WithPlatformActingInTenant(ctx, w.acting.ID, w.f.tenantID, uuid.Nil, OperationKindForceResolve, fn)
		}},
	}
	for _, s := range sessions {
		s := s
		t.Run(s.name, func(t *testing.T) {
			err := s.run(func(ctx context.Context, tx pgx.Tx) error {
				n, err := scalar(ctx, tx)
				if err != nil {
					// no privilege/policy at all is also a refusal
					return nil
				}
				// FLIPPED deliberately by migration 0125 (ADR 0111 §4.3, LF C-3 /
				// security M-5): the acting session reads its own tenant's typed
				// reference evidence (the M4 verdict reads Y); it still never writes.
				want := 0
				if s.name == "acting" {
					want = 1
				}
				if n != want {
					t.Errorf("%s session SELECT sees %d evidence row(s), want %d", s.name, n, want)
				}
				if err := k3Try(ctx, tx, insert); err == nil {
					t.Errorf("%s session INSERT was admitted", s.name)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("session: %v", err)
			}
		})
	}
	// The system shape (the sweeper's WithTenant) DOES read.
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		n, err := scalar(ctx, tx)
		if err != nil || n != 1 {
			t.Errorf("system session sees %d rows (%v), want 1", n, err)
		}
		// A duplicate plain INSERT raises (the BEFORE trigger refuses a parked attempt
		// first; for the UNIQUE shape see the live-attempt case below).
		if err := k3Try(ctx, tx, insert); err == nil {
			t.Error("a second evidence row was admitted")
		}
		// UPDATE/DELETE refused by the owner role too.
		for _, sql := range []string{
			`UPDATE payment_attempt_reference_evidence SET reference = 'z' WHERE attempt_id = $1`,
			`DELETE FROM payment_attempt_reference_evidence WHERE attempt_id = $1`,
		} {
			sql := sql
			// Refused either by an error (the immutability trigger) or by RLS matching
			// no row (no UPDATE/DELETE policy exists); either way nothing changes.
			var affected int64
			err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, sql, a.ID)
				if err == nil {
					affected = tag.RowsAffected()
				}
				return err
			})
			if err == nil && affected != 0 {
				t.Errorf("%q changed %d rows", sql, affected)
			}
		}
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE payment_attempt_reference_evidence`)
			return err
		}); err == nil {
			t.Error("TRUNCATE was admitted")
		}
		return nil
	})

	// Admissibility: a LIVE attempt, evidence inserted but NOT parked -> refused at commit.
	live, liveRef := w.depositAmbiguousBound()
	err := w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
			VALUES ($1, $2, $3, 'poll_returned_reference', 'k3-unparked-y')`, w.f.tenantID, live.ID, w.provider)
		return err
	})
	k3RequireCode(t, err, "MR050") // the DEFERRED park binding, raised at COMMIT
	if rows := w.evidenceRows(live.ID); len(rows) != 0 {
		t.Fatalf("an unparked evidence row survived: %v", rows)
	}
	// ... parked with ANOTHER reason in the same transaction -> refused at commit.
	err = w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
			VALUES ($1, $2, $3, 'poll_returned_reference', 'k3-wrong-reason-y')`, w.f.tenantID, live.ID, w.provider); err != nil {
			return err
		}
		return ApplyDisputeFromNonTerminal(ctx, tx, live.ID, EvidenceQueryStatus, TerminalReasonPollAmountMismatch)
	})
	k3RequireCode(t, err, "MR050")
	// Same reference as the bound one, a payout attempt, a terminal attempt, a
	// provider mismatch: refused by the BEFORE INSERT trigger.
	bad := map[string]func(ctx context.Context, tx pgx.Tx) error{
		"the bound reference itself": func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'poll_returned_reference', $4)`, w.f.tenantID, live.ID, w.provider, liveRef)
			return err
		},
		"another provider": func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, 'someone-else', 'poll_returned_reference', 'k3-p-y')`, w.f.tenantID, live.ID)
			return err
		},
		"an already parked attempt": func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'poll_returned_reference', 'k3-parked-y')`, w.f.tenantID, a.ID, w.provider)
			return err
		},
		"a prefixed reference": func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'poll_returned_reference', $4)`, w.f.tenantID, live.ID, w.provider, providerref.ReservedOperatorPrefix+"y")
			return err
		},
		"an unknown evidence kind": func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'something_else', 'k3-k-y')`, w.f.tenantID, live.ID, w.provider)
			return err
		},
	}
	for name, fn := range bad {
		name, fn := name, fn
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			if err := k3Try(ctx, tx, fn); err == nil {
				t.Errorf("evidence for %s was admitted", name)
			}
			return nil
		})
	}
	// A payout attempt.
	_, pa := w.ambiguousPayout(100)
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'poll_returned_reference', 'k3-payout-y')`, w.f.tenantID, pa.ID, w.provider)
			return err
		}); err == nil {
			t.Error("evidence for a payout attempt was admitted")
		}
		return nil
	})
	_ = ref
	_ = y
	// The UNIQUE shape: two evidence rows for one live attempt in one transaction
	// (a plain INSERT: the second raises 23505 before any commit).
	live2, _ := w.depositAmbiguousBound()
	err = w.pool.WithTenant(ctx, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ins := func(y string) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'poll_returned_reference', $4)`, w.f.tenantID, live2.ID, w.provider, y)
			return err
		}
		if err := ins("k3-dup-1"); err != nil {
			return err
		}
		return ins("k3-dup-2")
	})
	k3RequireCode(t, err, "23505")
}

// T-17 / C-37 fault injection: a failure AFTER the evidence insert rolls the row
// back with the park; a SECOND session verifies the row is absent; the retry
// parks with exactly one row.
func TestK3_T17_YEvidenceRollsBackWithAFailedPark(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	a, ref := w.depositAmbiguousBound()
	y := "k3-fault-" + uuid.NewString()
	// A failing trigger on the park's audit row (owner, scratch DB): everything the
	// park transaction did - the evidence insert included - must roll back.
	if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE FUNCTION k3_fail_park_audit() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'k3 injected failure after the evidence insert' USING ERRCODE = 'P0001'; END $$ LANGUAGE plpgsql`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `CREATE TRIGGER k3_fail_park_audit BEFORE INSERT ON audit_log FOR EACH ROW WHEN (NEW.action = 'payment.attempt_disputed') EXECUTE FUNCTION k3_fail_park_audit()`)
		return err
	}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	st := w.pollEcho(a, ref, y)
	if len(st.Errors) == 0 {
		t.Fatal("the injected failure did not surface")
	}
	// A second session (fresh connection scope) sees nothing persisted.
	if rows := w.evidenceRows(a.ID); len(rows) != 0 {
		t.Fatalf("evidence survived a failed park: %v", rows)
	}
	if got := w.attempt(a.ID); got.State != AttemptAmbiguous {
		t.Fatalf("the attempt moved to %s despite the rollback", got.State)
	}
	// Remove the injection: the retry parks and writes exactly one row.
	if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DROP TRIGGER k3_fail_park_audit ON audit_log`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DROP FUNCTION k3_fail_park_audit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	setNextActionNow(t, w.pool, w.f.tenantID, a.ID)
	if st := w.sweep(); len(st.Errors) != 0 {
		t.Fatalf("retry errors: %v", st.Errors)
	}
	if rows := w.evidenceRows(a.ID); len(rows) != 1 || rows[0] != y {
		t.Fatalf("after the retry the evidence = %v, want [%s]", rows, y)
	}
}

// C-9 / T-13 (ADV): the reserved prefix at every payments ingress.
func TestK3_C9_ReservedPrefixAtEveryIngress(t *testing.T) {
	prefixed := providerref.ReservedOperatorPrefix + "hostile"

	t.Run("deposit_sync_phase_C", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		w.prov.setDeposit(func(req DepositRequest) DepositResult {
			return DepositResult{Outcome: OutcomePending, ProviderReference: prefixed, Amount: req.Amount, AssetCode: req.AssetCode}
		})
		res := rvInit(t, w.pool, w.orch, w.f.orchFixture, 5000, "k3-sync-"+uuid.NewString())
		a := w.attempt(res.Attempt.ID)
		if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != "invalid_provider_reference:reserved_namespace" || a.ProviderReference != nil {
			t.Fatalf("a reserved-prefix sync reference must park unbound: %s %v ref=%v", a.State, a.TerminalReason, a.ProviderReference)
		}
	})

	t.Run("deposit_poll_pending_binding_site_never_errors_and_backs_off", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		// A direct caller of applyStatusEvidence on an attempt with NO bound reference.
		intent, attempt := w.unboundSubmittingDeposit()
		s := NewSweeper(w.pool, w.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
		gr := GateResult[StatusResult]{Class: ErrorClassPending, Value: StatusResult{ProviderReference: prefixed, Outcome: OutcomePending}}
		before := attempt.PollCount
		for i := 0; i < 2; i++ {
			err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				fresh, err := GetAttemptByID(ctx, tx, attempt.ID)
				if err != nil {
					return err
				}
				return s.applyStatusEvidence(ctx, tx, intent, fresh, gr)
			})
			if err != nil {
				t.Fatalf("call %d: a refused echo must NEVER be an error return (hot loop): %v", i, err)
			}
		}
		got := w.attempt(attempt.ID)
		if got.ProviderReference != nil || got.State != attempt.State {
			t.Fatalf("the refused echo was bound or the state moved: %s %v", got.State, got.ProviderReference)
		}
		if got.PollCount != before+2 || got.NextActionAt == nil {
			t.Fatalf("poll_count %d (want %d) next_action_at=%v: no backoff recorded", got.PollCount, before+2, got.NextActionAt)
		}
		if n := w.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payments.poll_echo_reference_refused' AND target_id = $2`, w.f.tenantID, attempt.ID.String()); n != 2 {
			t.Fatalf("refused-echo audits = %d, want one per poll (2)", n)
		}
	})

	t.Run("deposit_poll_decline_adoption_site_declines_with_a_nil_reference", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		intent, attempt := w.unboundSubmittingDeposit()
		s := NewSweeper(w.pool, w.orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
		gr := GateResult[StatusResult]{Class: ErrorClassDefiniteDecline, Value: StatusResult{ProviderReference: prefixed, Outcome: OutcomeDeclined, DeclineReason: "provider_unavailable"}}
		err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			fresh, err := GetAttemptByID(ctx, tx, attempt.ID)
			if err != nil {
				return err
			}
			return s.applyStatusEvidence(ctx, tx, intent, fresh, gr)
		})
		if err != nil {
			t.Fatalf("decline with a refused echo: %v", err)
		}
		got := w.attempt(attempt.ID)
		if got.State != AttemptDeclined || got.ProviderReference != nil {
			t.Fatalf("want declined with no adopted reference, got %s %v", got.State, got.ProviderReference)
		}
		if n := w.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'payments.poll_echo_reference_refused' AND target_id = $2`, w.f.tenantID, attempt.ID.String()); n != 1 {
			t.Fatalf("refused-echo audits = %d, want 1", n)
		}
	})

	t.Run("deposit_poll_echo_audits_record_reason_length_hash_only", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		a, ref := w.depositAmbiguousBound()
		w.pollEcho(a, ref, prefixed)
		var meta string
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT metadata::text FROM audit_log WHERE tenant_id = $1 AND action = 'payment.attempt_disputed' AND target_id = $2`,
				w.f.tenantID, a.ID.String()).Scan(&meta)
		})
		if strings.Contains(meta, "hostile") || strings.Contains(meta, "echoed_provider_reference") ||
			!strings.Contains(meta, `"echo_ref_reason": "reserved_namespace"`) {
			t.Fatalf("the park audit must carry reason/length/hash only: %s", meta)
		}
		// And the succeeded-terminal / decline echo paths (sweeper.go:556, :636): a
		// decline with a bound reference and a prefixed echo.
		b, refB := w.depositAmbiguousBound()
		w.prov.setStatus(refB, StatusResult{ProviderReference: prefixed, Outcome: OutcomeDeclined, DeclineReason: "provider_unavailable"})
		setNextActionNow(t, w.pool, w.f.tenantID, b.ID)
		if st := w.sweep(); len(st.Errors) != 0 {
			t.Fatal(st.Errors)
		}
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT metadata::text FROM audit_log WHERE tenant_id = $1 AND action = 'payments.poll_decline_reference_mismatch' AND target_id = $2`,
				w.f.tenantID, b.ID.String()).Scan(&meta)
		})
		if strings.Contains(meta, "hostile") || !strings.Contains(meta, `"echo_ref_reason": "reserved_namespace"`) {
			t.Fatalf("the decline echo audit must carry reason/length/hash only: %s", meta)
		}
	})

	t.Run("payout_sync", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		w.prov.setWithdraw(func(req WithdrawRequest) WithdrawResult {
			return WithdrawResult{Outcome: OutcomePending, ProviderReference: prefixed}
		})
		wr, a := w.payoutExpect(100)
		if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != "invalid_provider_reference:reserved_namespace" {
			t.Fatalf("a reserved-prefix payout sync reference must park: %s %v", a.State, a.TerminalReason)
		}
		if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted || w.withdrawalOf(wr.ID).ProviderReference != nil {
			t.Fatal("the hold must be kept and no reference bound")
		}
	})

	t.Run("payout_poll", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		_, a := w.payout(100)
		w.prov.setStatus(*a.ProviderReference, StatusResult{ProviderReference: prefixed, Outcome: OutcomePending})
		setNextActionNow(t, w.pool, w.f.tenantID, a.ID)
		if st := w.sweep(); len(st.Errors) != 0 {
			t.Fatalf("sweep errors: %v", st.Errors)
		}
		got := w.attempt(a.ID)
		if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != "invalid_provider_reference:reserved_namespace" {
			t.Fatalf("a reserved-prefix payout poll reference must park: %s %v", got.State, got.TerminalReason)
		}
	})

	t.Run("verified_callbacks_deposit_and_payout_and_validator_fields", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		for _, ev := range []string{"deposit", "payout"} {
			_, err := rvApplyReceipt(w.pool, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{EventType: ev, ProviderReference: prefixed,
				Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"})
			_ = err // ApplyReceiptEvidence itself does not validate; the boundary does (below)
		}
		// The verified boundary: every field is validated.
		for field, ev := range map[string]ReceiptEvidence{
			"provider_reference":          {ProviderReference: prefixed},
			"original_provider_reference": {ProviderReference: "ok", OriginalProviderReference: prefixed},
			"settlement_reference":        {ProviderReference: "ok", SettlementReference: prefixed},
		} {
			err := validateReceiptReferences(ev)
			perr, ok := providerref.AsError(err)
			if !ok || perr.Reason != providerref.ReasonReservedNamespace || perr.Field != field {
				t.Errorf("%s: want a reserved_namespace refusal naming the field, got %v", field, err)
			}
		}
		// Through ReceiveVerifiedCallback (the real signed shape).
		for _, ev := range []CallbackEventType{CallbackEventDeposit, CallbackEventDepositReversal} {
			_, err := rvCallback(w.pool, w.orch, w.f.orchFixture, w.provider,
				w.mock.CallbackPayload(w.f.tenantID, ev, prefixed, "", OutcomeSucceeded, 100, "EUR", "", false))
			if err == nil || !errors.Is(err, ErrProviderReferenceInvalid) && !errors.Is(err, providerref.ErrInvalid) {
				t.Errorf("%s callback with a reserved-prefix reference: want a deterministic invalid-reference error, got %v", ev, err)
			}
		}
		if n := w.countRows(`SELECT count(*) FROM payment_provider_events WHERE tenant_id = $1 AND provider_reference LIKE 'platform-operator-declared:%'`, w.f.tenantID); n != 0 {
			t.Fatalf("a reserved-prefix event row was stored: %d", n)
		}
	})

	t.Run("statement_fetch_refuses_the_whole_statement", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		for field, mutate := range map[string]func(l *statement.PaymentStatementLine){
			"provider_reference":          func(l *statement.PaymentStatementLine) { l.ProviderReference = prefixed },
			"original_provider_reference": func(l *statement.PaymentStatementLine) { l.OriginalProviderReference = prefixed },
			"settlement_reference":        func(l *statement.PaymentStatementLine) { l.SettlementReference = prefixed },
		} {
			l := w.payoutLine("k3-ok-"+uuid.NewString(), "", statement.PaymentStatusSucceeded, 5)
			mutate(&l)
			_, err := reconciliation.FetchPaymentStatement(context.Background(), w.source(false, l), w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), reconciliation.PaymentStatementOptions{})
			if !errors.Is(err, reconciliation.ErrPaymentStatementInvalid) {
				t.Errorf("%s: want ErrPaymentStatementInvalid, got %v", field, err)
			}
		}
		if n := w.countRows(`SELECT count(*) FROM payment_statement_imports WHERE tenant_id = $1`, w.f.tenantID); n != 0 {
			t.Fatalf("a refused statement stored an import: %d", n)
		}
	})

	t.Run("database_backstops_checks_and_the_all_sessions_ledger_trigger", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		wr, a := w.ambiguousPayout(100)
		rvInit(t, w.pool, w.orch, w.f.orchFixture, 5000, "k3-bk-"+uuid.NewString()) // a deposit_intents row to update
		// CHECKs.
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			for name, sql := range map[string]string{
				"deposit_intents":     `UPDATE deposit_intents SET provider_reference = $1 WHERE id = (SELECT id FROM deposit_intents WHERE tenant_id = $2 LIMIT 1)`,
				"withdrawal_requests": `UPDATE withdrawal_requests SET provider_reference = $1 WHERE id = (SELECT id FROM withdrawal_requests WHERE tenant_id = $2 LIMIT 1)`,
			} {
				sql := sql
				err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, sql, prefixed, w.f.tenantID)
					return err
				})
				if k3Code(err) != "23514" {
					t.Errorf("%s CHECK: want 23514, got %v", name, err)
				}
			}
			err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO payment_provider_events (id, tenant_id, provider_id, event_type, provider_reference, outcome, event_fingerprint, disposition_at_receipt)
					VALUES (gen_random_uuid(), $1, 'p', 'payout', $2, 'pending', decode('01', 'hex'), 'anomaly')`, w.f.tenantID, prefixed)
				return err
			})
			if k3Code(err) != "23514" {
				t.Errorf("payment_provider_events CHECK: want 23514, got %v", err)
			}
			return nil
		})
		// The ledger trigger refuses the prefix in a TENANT session on ANY posting that is
		// not an executing m2_declare_paid Step B: another type, no resolution, even
		// withdrawal.Complete itself.
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				return withdrawal.Complete(ctx, tx, wr.ID, w.provider, prefixed)
			})
			if k3Code(err) != "MR020" && !strings.Contains(errString(err), "MR020") {
				t.Errorf("withdrawal.Complete with the reserved prefix and no resolution: want MR020, got %v", err)
			}
			err = k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), "casino_win", "k3-casino:"+prefixed, k3StrPtr("casino-x"), k3StrPtr(prefixed), uuid.New())
			})
			if k3Code(err) != "MR020" {
				t.Errorf("a casino-shaped posting with the reserved prefix: want MR020, got %v", err)
			}
			err = k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), "deposit", "k3-dep:"+prefixed, k3StrPtr("p"), k3StrPtr(prefixed), uuid.New())
			})
			if k3Code(err) != "MR020" {
				t.Errorf("a deposit posting with the reserved prefix: want MR020, got %v", err)
			}
			return nil
		})
		// Inside a GOVERNED execution a resolution for ANOTHER withdrawal, or a not-paid
		// resolution, does not admit it.
		r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
			e := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), "withdrawal_completed", w.provider+":"+prefixed, &w.provider, k3StrPtr(prefixed), wr.ID)
			})
			if k3Code(e) != "MR020" {
				t.Errorf("a not-paid resolution admitted the reserved prefix: %v", e)
			}
			return nil
		})
		k3RequireNoErr(t, err, "governed prefix probe")
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// unboundSubmittingDeposit creates a deposit intent with a `submitting` attempt
// that holds NO provider reference (only a direct caller of applyStatusEvidence
// can poll one; the sweeper never does, sweeper.go:366).
func (w *k3World) unboundSubmittingDeposit() (DepositIntent, PaymentAttempt) {
	w.t.Helper()
	w.prov.setDeposit(func(req DepositRequest) DepositResult {
		return DepositResult{Outcome: OutcomeAmbiguous, Amount: req.Amount, AssetCode: req.AssetCode}
	})
	res := rvInit(w.t, w.pool, w.orch, w.f.orchFixture, 5000, "k3-unb-"+uuid.NewString())
	w.prov.setDeposit(nil)
	a := w.attempt(res.Attempt.ID)
	if a.ProviderReference != nil || a.State != AttemptAmbiguous {
		w.t.Fatalf("setup: want an ambiguous reference-less attempt, got %s %v", a.State, a.ProviderReference)
	}
	var intent DepositIntent
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = GetDepositIntentByID(ctx, tx, *a.DepositIntentID)
		return err
	})
	return intent, a
}

// payoutExpect dispatches a payout through the real claim/dispatch/apply path
// WITHOUT asserting it ends pending (a scripted provider may park it).
func (w *k3World) payoutExpect(amount int64) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	w.t.Helper()
	w.ensureWithdrawalPolicy()
	wr := w.approveWithdrawal(amount, "k3-px-"+uuid.NewString())
	claim, err := w.orch.ClaimForDispatch(context.Background(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		w.t.Fatalf("ClaimForDispatch: %v", err)
	}
	adapter, _ := w.orch.Provider(claim.Capability.ProviderID)
	gr := DispatchWithdraw(context.Background(), nil, MockCredentialResolver{}, adapter, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		w.t.Fatalf("ApplyPayoutResult: %v", err)
	}
	return w.withdrawalOf(wr.ID), w.attempt(claim.Attempt.ID)
}
