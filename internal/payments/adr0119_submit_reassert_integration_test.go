//go:build integration

package payments

// ADR 0119 S5 + S6, the S4b-R3 / ADR 0082 A9 HARD GATE for withdrawal SUBMIT: inside ClaimForDispatch's T1p
// transaction, right after the payment_attempts row, the submitter's own staff row is read FOR SHARE and compared with
// the verified token's role and staff_version (reassertSubmitter). Exercised through the REAL ClaimForDispatch, in
// both orders:
//
//   - the staff change first (committed before the claim, or in flight and committed while the claim waits on the
//     row): the claim is refused (ErrSubmitterNotLive) and NOTHING is written - no attempt, the request stays approved;
//   - the claim first (holding the row FOR SHARE): a suspension cannot commit until the claim does (55P03 under a
//     short lock_timeout), the claim stands, and the suspension completes afterwards.
//
// The "claim first" pause is a scratch-database-only waiting point: a BEFORE INSERT trigger on audit_log that waits on
// an advisory lock for exactly the claim's success audit row (written after the re-assertion), so the claim
// transaction is held open at a point where the FOR SHARE has already been taken. Nothing else is changed.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const submitPauseLock = 4_119_005

type submitWorld struct {
	t     *testing.T
	pool  *db.Pool
	f     payoutFixture
	orch  *Orchestrator
	staff uuid.UUID
	// base is the write counts taken right before the latest claim (ledger-finance review LF-7).
	base claimWrites
}

// claimWrites are the rows a claim may write besides the attempt and the state change: the KYC enforcement decision
// rows of the tenant, the withdrawal.submit.http audit rows of the request, and the destination snapshots of the
// request (ledger-finance review LF-7: a refusal must commit NONE of them).
type claimWrites struct {
	decisions, audits, snapshots int
}

func (w *submitWorld) writes(wrID uuid.UUID) claimWrites {
	w.t.Helper()
	var c claimWrites
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1`, w.f.tenantID).Scan(&c.decisions); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.submit.http' AND target_id = $1`, wrID.String()).Scan(&c.audits); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM payout_attempt_destination_snapshots WHERE withdrawal_request_id = $1`, wrID).Scan(&c.snapshots)
	}); err != nil {
		w.t.Fatal(err)
	}
	return c
}

func newSubmitWorld(t *testing.T) *submitWorld {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("s56-submit", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"s56-submit": provider},
		MultiWebhookCredentialResolver{"s56-submit": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())
	w := &submitWorld{t: t, pool: pool, f: f, orch: orch, staff: uuid.New()}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `WITH p AS (SELECT gen_random_uuid() AS id), pi AS (INSERT INTO persons (id) SELECT id FROM p)
			INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'x', 'finance', 'active', (SELECT id FROM p))`,
			w.staff, f.tenantID, "s56-submit-"+w.staff.String()+"@fixture.invalid")
		return err
	}); err != nil {
		t.Fatalf("seed submitter: %v", err)
	}
	return w
}

func (w *submitWorld) actor(role string, sv int64) SubmitActor {
	return SubmitActor{StaffID: w.staff, IPAddress: "127.0.0.1", UserAgent: "s56", RequestID: uuid.NewString(), Role: role, StaffVersion: sv}
}

func (w *submitWorld) claim(wr withdrawal.WithdrawalRequest, a SubmitActor) (ClaimResult, error) {
	w.base = w.writes(wr.ID)
	return w.orch.ClaimForDispatch(context.Background(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, "bank_transfer", a)
}

// change runs one statement on the submitter's row in its own tenant transaction (finance: no last-admin rule).
func (w *submitWorld) change(sql, lockTimeout string) error {
	return w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if lockTimeout != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, lockTimeout); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, sql, w.staff)
		return err
	})
}

func (w *submitWorld) state(wr withdrawal.WithdrawalRequest) withdrawal.State {
	w.t.Helper()
	var got withdrawal.WithdrawalRequest
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = withdrawal.GetByID(ctx, tx, wr.ID)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return got.State
}

func (w *submitWorld) assertNothingWritten(wr withdrawal.WithdrawalRequest) {
	w.t.Helper()
	if n := countAttempts(w.t, w.pool, w.f.tenantID, wr.ID); n != 0 {
		w.t.Fatalf("a refused claim left %d payment attempt(s)", n)
	}
	if s := w.state(wr); s != withdrawal.StateApproved {
		w.t.Fatalf("a refused claim moved the request to %s", s)
	}
	// LF-7: no ALLOW decision row, no B6 audit, no destination snapshot committed by the refused claim.
	if got := w.writes(wr.ID); got != w.base {
		w.t.Fatalf("a refused claim committed rows: before %+v, after %+v", w.base, got)
	}
}

// waitForLockWaiter polls until some backend of this database is waiting on a lock (the claim, parked).
func (w *submitWorld) waitForLockWaiter() {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := w.pool.Raw().QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			w.t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.t.Fatal("the claim never parked on a lock")
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func TestSubmitReassertion_LiveSubmitterClaims(t *testing.T) {
	w := newSubmitWorld(t)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "s56-live")
	res, err := w.claim(wr, w.actor("finance", 1))
	if err != nil || res.Denied || res.Attempt.ID == uuid.Nil {
		t.Fatalf("a live submitter must claim: %+v %v", res, err)
	}
}

// The staff change committed BEFORE the claim's re-assertion: every arm refuses and writes nothing.
func TestSubmitReassertion_ChangeCommittedFirst_Refused(t *testing.T) {
	for _, c := range []struct {
		name, sql string
		role      string
		sv        int64
	}{
		{"suspended", `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, "finance", 1},
		{"role_changed", `UPDATE staff_users SET role = 'support' WHERE id = $1`, "finance", 1},
		// sv only: role and status unchanged, the password changed (staff_version 1 -> 2).
		{"staff_version_only", `UPDATE staff_users SET password_hash = 'y' WHERE id = $1`, "finance", 1},
		// the token's role is stale even though the row is live.
		{"token_role_mismatch", ``, "tenant_admin", 1},
		// the token's sv is ahead of the row.
		{"token_sv_mismatch", ``, "finance", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newSubmitWorld(t)
			wr := approvedWithdrawal(t, w.pool, w.f, 500, "s56-"+c.name)
			if c.sql != "" {
				if err := w.change(c.sql, ""); err != nil {
					t.Fatalf("change: %v", err)
				}
			}
			_, err := w.claim(wr, w.actor(c.role, c.sv))
			if !errors.Is(err, ErrSubmitterNotLive) {
				t.Fatalf("want ErrSubmitterNotLive, got %v", err)
			}
			w.assertNothingWritten(wr)
		})
	}
}

// No staff row at all, and an empty token role with the integration-only fallback OFF (the production behaviour).
func TestSubmitReassertion_UnknownStaffAndEmptyRole_Refused(t *testing.T) {
	w := newSubmitWorld(t)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "s56-unknown")
	a := w.actor("finance", 1)
	a.StaffID = uuid.New()
	if _, err := w.claim(wr, a); !errors.Is(err, ErrSubmitterNotLive) {
		t.Fatalf("unknown staff: want ErrSubmitterNotLive, got %v", err)
	}
	w.assertNothingWritten(wr)

	SetTestSubmitReassertionFallback(false)
	t.Cleanup(func() { SetTestSubmitReassertionFallback(true) })
	if _, err := w.claim(wr, w.actor("", 1)); !errors.Is(err, ErrSubmitterNotLive) {
		t.Fatalf("empty role, fallback off: want ErrSubmitterNotLive, got %v", err)
	}
	w.assertNothingWritten(wr)
}

// The suspension is IN FLIGHT first (row updated, not committed) when the claim reaches the re-assertion: the claim's
// FOR SHARE waits for it, re-reads the committed row, and refuses.
func TestSubmitReassertion_SuspensionInFlightFirst_ClaimWaitsThenRefused(t *testing.T) {
	w := newSubmitWorld(t)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "s56-inflight")
	ctx := context.Background()
	tx, err := w.pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, w.f.tenantID.String()); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, w.staff); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("in-flight suspension: %v (%d rows)", err, tag.RowsAffected())
	}
	var claimErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, claimErr = w.claim(wr, w.actor("finance", 1))
	}()
	w.waitForLockWaiter()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !errors.Is(claimErr, ErrSubmitterNotLive) {
		t.Fatalf("the claim that waited on the suspension must be refused, got %v", claimErr)
	}
	w.assertNothingWritten(wr)
}

// The claim FIRST: parked after the re-assertion (holding the submitter's row FOR SHARE), a suspension cannot commit
// (55P03 under a 300ms lock_timeout); the claim then commits and the suspension completes afterwards.
func TestSubmitReassertion_ClaimFirst_SuspensionWaits(t *testing.T) {
	w := newSubmitWorld(t)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "s56-claimfirst")
	ctx := context.Background()
	for _, q := range []string{
		`CREATE FUNCTION s56_submit_pause() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		     IF NEW.action = 'withdrawal.submit.http' AND NEW.outcome = 'success' THEN
		         PERFORM pg_advisory_xact_lock(4119005);
		     END IF;
		     RETURN NEW;
		 END $$`,
		`CREATE TRIGGER aa_s56_submit_pause BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION s56_submit_pause()`,
	} {
		if _, err := w.pool.Raw().Exec(ctx, q); err != nil {
			t.Fatalf("scratch waiting point: %v", err)
		}
	}
	holder, err := w.pool.Raw().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, submitPauseLock); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, submitPauseLock); err != nil {
				t.Error(err)
			}
		}
	}
	defer release()

	var claimErr error
	var res ClaimResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, claimErr = w.claim(wr, w.actor("finance", 1))
	}()
	w.waitForLockWaiter()
	waitErr := w.change(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, "300ms")
	release()
	wg.Wait()
	if sqlState(waitErr) != "55P03" {
		t.Fatalf("the suspension must wait on the claim's FOR SHARE (55P03), got %v", waitErr)
	}
	if claimErr != nil || res.Attempt.ID == uuid.Nil {
		t.Fatalf("the claim holding the row first must stand: %+v %v", res, claimErr)
	}
	if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 1 {
		t.Fatalf("attempts %d, want 1", n)
	}
	if err := w.change(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, "2s"); err != nil {
		t.Fatalf("the suspension after the claim commits: %v", err)
	}
}

// Ledger-finance review LF-7: a refused claim (stale sv) followed by a retry with a valid token claims EXACTLY once,
// and a replay after that is refused (the request is no longer approved) without a second attempt.
func TestSubmitReassertion_RefusedThenRetried_ClaimsOnce(t *testing.T) {
	w := newSubmitWorld(t)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "s56-retry")
	if _, err := w.claim(wr, w.actor("finance", 2)); !errors.Is(err, ErrSubmitterNotLive) {
		t.Fatalf("stale sv: want ErrSubmitterNotLive, got %v", err)
	}
	w.assertNothingWritten(wr)
	res, err := w.claim(wr, w.actor("finance", 1))
	if err != nil || res.Denied || res.Attempt.ID == uuid.Nil {
		t.Fatalf("the retry with a valid token must claim: %+v %v", res, err)
	}
	after := w.writes(wr.ID)
	if after.audits != w.base.audits+1 || after.decisions != w.base.decisions+1 {
		t.Fatalf("the successful claim must write one decision row and one audit row: before %+v, after %+v", w.base, after)
	}
	if _, err := w.claim(wr, w.actor("finance", 1)); err == nil {
		t.Fatal("a replay of the claim must be refused")
	}
	if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 1 {
		t.Fatalf("attempts %d, want exactly 1", n)
	}
	if s := w.state(wr); s == withdrawal.StateApproved {
		t.Fatal("the claimed request must have left approved")
	}
}
