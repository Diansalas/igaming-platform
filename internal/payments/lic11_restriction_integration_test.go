//go:build integration

// lic11 (migration 0182): the per-tenant withdrawal restriction policy at the payout dispatch points (T1p ClaimForDispatch,
// the sweeper's T2 re-claim and T12 resend). A configured hold_for_review / block is a WD056 denial through the EXISTING F4
// paths (T1p: park in `approved` with the hold; T2 / T12: escalate, no resend, no release); a configured allow never skips
// a hard restriction; the default (no row) is the behaviour before 0182, with the provenance recorded. MOCK providers only.
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// l11PaySuspendLicence binds a suspended licence to the tenant (owner path of the scratch database).
func l11PaySuspendLicence(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		jid, lid := uuid.New(), uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'lic11 payout jurisdiction')`, jid, "L11P-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, 'platform', $3, 'suspended')`,
			lid, jid, "L11P-"+lid.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, tenantID)
		return err
	}); err != nil {
		t.Fatalf("suspend licence: %v", err)
	}
}

// l11PayPolicy writes a policy version through the owner path; returns its set version.
func l11PayPolicy(t *testing.T, pool *db.Pool, tenantID uuid.UUID, cause, action string) int64 {
	t.Helper()
	var v int64
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, reason_code) VALUES ($1, $2, $3, 'lic11_fixture') RETURNING set_version`,
			tenantID, cause, action).Scan(&v)
	}); err != nil {
		t.Fatalf("policy %s=%s: %v", cause, action, err)
	}
	return v
}

// DEFAULT PARITY at T1p: a suspended licence with NO policy row claims exactly as before 0182; the allowing decision row
// records source default_unconfigured, the cause and set version 0, and the success audit carries the same provenance.
func TestLic11_T1p_Default_SubmitsAndRecordsTheProvenance(t *testing.T) {
	w := newF4World(t, "l11-def")
	l11PaySuspendLicence(t, w.pool, w.f.tenantID)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "l11-def")
	claim, err := w.claim(t, wr.ID)
	if err != nil || claim.Request.State != withdrawal.StateSubmitted {
		t.Fatalf("the default must claim as before 0182: %v", err)
	}
	if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = 'dispatch' AND allowed
		AND restriction_policy_source = 'default_unconfigured' AND restriction_policy_set_version = 0
		AND restriction_cause = 'licence_suspended' AND restriction_action = 'allow'`, wr.ID); n != 1 {
		t.Fatalf("the allowing decision must carry the default provenance, got %d", n)
	}
	if n := w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.submit.http' AND target_id = $1 AND outcome = 'success'
		AND metadata->>'restriction_policy_source' = 'default_unconfigured' AND metadata->>'restriction_cause' = 'licence_suspended'`, wr.ID.String()); n != 1 {
		t.Fatalf("the success audit must carry the provenance, got %d", n)
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
}

// CONFIGURED hold_for_review / block at T1p: WD056, the payout parks in `approved` with its hold (no attempt, no provider
// call, no KYC decision), the denying decision row carries the configured provenance, the staff-attributed denial audit and
// the withdrawal.payout_gate_denied alert commit. The KYC gate never runs.
func TestLic11_T1p_ConfiguredHoldOrBlock_Parks(t *testing.T) {
	for _, action := range []string{"hold_for_review", "block"} {
		w := newF4World(t, "l11-park-"+action)
		l11PaySuspendLicence(t, w.pool, w.f.tenantID)
		wr := approvedWithdrawal(t, w.pool, w.f, 500, "l11-park-"+action)
		holdBefore := w.holdBalance(t)
		v := l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", action)
		_, err := w.claim(t, wr.ID)
		var denied *withdrawal.GateDeniedError
		if !errors.Is(err, ErrPayoutWithdrawalGateDenied) || !errors.As(err, &denied) || denied.GateCode != withdrawal.GateCodeRestriction || denied.Reason != "licence_suspended" {
			t.Fatalf("%s: want a WD056 dispatch denial, got %v", action, err)
		}
		if got := w.state(t, wr.ID); got.State != withdrawal.StateApproved {
			t.Fatalf("%s: the payout must stay approved, got %s", action, got.State)
		}
		if countAttempts(t, w.pool, w.f.tenantID, wr.ID) != 0 || w.holdBalance(t) != holdBefore {
			t.Fatalf("%s: no attempt, hold unchanged", action)
		}
		if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = 'dispatch' AND NOT allowed
			AND gate_code = 'WD056' AND reason_code = 'licence_suspended' AND restriction_policy_source = 'configured'
			AND restriction_policy_set_version = $2 AND restriction_action = $3`, wr.ID, v, action); n != 1 {
			t.Fatalf("%s: the denying decision row must carry the configured provenance, got %d", action, n)
		}
		if n := w.count(t, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.submit.http' AND target_id = $1 AND outcome = 'denied'
			AND actor_type = 'staff' AND metadata->>'gate_code' = 'WD056' AND metadata->>'restriction_action' = $2`, wr.ID.String(), action); n != 1 {
			t.Fatalf("%s: one staff-attributed denial audit, got %d", action, n)
		}
		if n := w.alerts(t, "withdrawal:"+wr.ID.String()+":gate:WD056"); n != 1 {
			t.Fatalf("%s: one open withdrawal.payout_gate_denied alert, got %d", action, n)
		}
		if n := w.count(t, `SELECT count(*) FROM kyc_enforcement_decisions WHERE player_account_id = $1 AND operation = 'withdrawal_payout'`, w.f.playerAccountID); n != 0 {
			t.Fatalf("%s: the KYC gate must not run after a policy denial, %d decisions", action, n)
		}
		loAssertBalanced(t, w.pool, w.f.tenantID)
		loAssertProjectionMatchesRebuild(t, w.pool, w.f.tenantID)
	}
}

// POLICY CHANGES MID-FLIGHT: a request accepted under the default parks at T1p once a block is configured; a later
// (loosening) version lets the SAME request claim; each decision row records the set version it was decided under, so the
// history replays. The claim is idempotent: a replayed claim of the submitted request is refused as before, nothing re-sent.
func TestLic11_T1p_PolicyChangedMidFlight_DecisionsRecordTheVersionUsed(t *testing.T) {
	w := newF4World(t, "l11-mid")
	l11PaySuspendLicence(t, w.pool, w.f.tenantID)
	wr := approvedWithdrawal(t, w.pool, w.f, 500, "l11-mid")
	v1 := l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", "block")
	if _, err := w.claim(t, wr.ID); !errors.Is(err, ErrPayoutWithdrawalGateDenied) {
		t.Fatalf("parked under v%d: %v", v1, err)
	}
	v2 := l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", "allow")
	claim, err := w.claim(t, wr.ID)
	if err != nil || claim.Request.State != withdrawal.StateSubmitted {
		t.Fatalf("claims under v%d: %v", v2, err)
	}
	if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND NOT allowed AND restriction_policy_set_version = $2`, wr.ID, v1); n != 1 {
		t.Fatalf("the denial records v%d, got %d", v1, n)
	}
	if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND allowed AND restriction_policy_set_version = $2 AND restriction_policy_source = 'configured'`, wr.ID, v2); n != 1 {
		t.Fatalf("the allow records v%d, got %d", v2, n)
	}
	if _, err := w.claim(t, wr.ID); !errors.Is(err, withdrawal.ErrStateConflict) {
		t.Fatalf("a replayed claim of a submitted request is refused, nothing re-sent: %v", err)
	}
	if n := countAttempts(t, w.pool, w.f.tenantID, wr.ID); n != 1 {
		t.Fatalf("exactly one attempt, got %d", n)
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
}

// HARD RESTRICTIONS at T1p are not overridable by a configured allow: an account suspended after approval (WD050), an
// engaged kill switch (rolled back, stays approved).
func TestLic11_T1p_HardRestrictions_NotOverridable(t *testing.T) {
	t.Run("account_suspended", func(t *testing.T) {
		w := newF4World(t, "l11-hard-acct")
		l11PaySuspendLicence(t, w.pool, w.f.tenantID)
		l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", "allow")
		wr := approvedWithdrawal(t, w.pool, w.f, 500, "l11-hard-acct")
		w.setAccount(t, identity.PlayerStatusSuspended)
		_, err := w.claim(t, wr.ID)
		var denied *withdrawal.GateDeniedError
		if !errors.As(err, &denied) || denied.GateCode != withdrawal.GateCodeRG {
			t.Fatalf("want WD050, got %v", err)
		}
	})
	t.Run("kill_switch", func(t *testing.T) {
		w := newF4World(t, "l11-hard-ks")
		l11PaySuspendLicence(t, w.pool, w.f.tenantID)
		l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", "allow")
		wr := approvedWithdrawal(t, w.pool, w.f, 500, "l11-hard-ks")
		w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := EngageKillSwitch(ctx, tx, w.f.tenantID, "*", KillSwitchOperationAny, "lic11-incident")
			return err
		})
		if _, err := w.claim(t, wr.ID); !errors.Is(err, ErrPayoutKillSwitchEngaged) {
			t.Fatalf("the kill switch still refuses, got %v", err)
		}
		if got := w.state(t, wr.ID); got.State != withdrawal.StateApproved || countAttempts(t, w.pool, w.f.tenantID, wr.ID) != 0 {
			t.Fatal("the claim rolls back: approved, no attempt")
		}
	})
}

// T2 re-claim: a configured hold_for_review / block escalates the never-sent attempt (T16: no resend, no release) with a
// WD056 denying re-dispatch decision; with no policy the re-claim proceeds as before and records default_unconfigured.
func TestLic11_T2_ConfiguredPolicyEscalates_DefaultProceeds(t *testing.T) {
	for _, action := range []string{"hold_for_review", "block"} {
		w := newF4World(t, "l11-t2-"+action)
		l11PaySuspendLicence(t, w.pool, w.f.tenantID)
		wr, attempt := notSentPayoutAttempt(t, w.pool, w.orch, w.f, 500, "l11-t2-"+action)
		l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", action)
		if err := w.sweeper().reclaimPayoutCreated(context.Background(), w.f.tenantID, attempt); err != nil {
			t.Fatalf("%s: reclaim: %v", action, err)
		}
		after := w.reread(t, attempt.ID)
		if after.State != AttemptCreated || after.EscalatedAt == nil || after.SubmitCount != attempt.SubmitCount {
			t.Fatalf("%s: want created + escalated + no resend, got %s escalated=%v", action, after.State, after.EscalatedAt != nil)
		}
		if got := w.state(t, wr.ID); got.State != withdrawal.StateSubmitted {
			t.Fatalf("%s: the withdrawal stays submitted (no release), got %s", action, got.State)
		}
		if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = 'redispatch' AND NOT allowed
			AND gate_code = 'WD056' AND restriction_action = $2 AND payment_attempt_id = $3`, wr.ID, action, attempt.ID); n != 1 {
			t.Fatalf("%s: one denying WD056 re-dispatch decision bound to the attempt, got %d", action, n)
		}
		loAssertBalanced(t, w.pool, w.f.tenantID)
	}
	w := newF4World(t, "l11-t2-def")
	l11PaySuspendLicence(t, w.pool, w.f.tenantID)
	wr, attempt := notSentPayoutAttempt(t, w.pool, w.orch, w.f, 500, "l11-t2-def")
	if err := w.sweeper().reclaimPayoutCreated(context.Background(), w.f.tenantID, attempt); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if after := w.reread(t, attempt.ID); after.State != AttemptPending {
		t.Fatalf("the default re-claims as before 0182, got %s", after.State)
	}
	if n := w.count(t, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND stage = 'redispatch' AND allowed
		AND restriction_policy_source = 'default_unconfigured' AND restriction_cause = 'licence_suspended'`, wr.ID); n != 1 {
		t.Fatalf("the allowing re-dispatch decision records the default provenance, got %d", n)
	}
}

// SETTLEMENT is never gated by the policy: a payout already sent completes on its provider evidence under a configured
// block (the platform records the provider's truth); the ledger stays balanced and the projection equals the rebuild.
func TestLic11_Settlement_NotGatedByThePolicy(t *testing.T) {
	w := newF4World(t, "l11-settle")
	l11PaySuspendLicence(t, w.pool, w.f.tenantID)
	wr := approvedWithdrawal(t, w.pool, w.f, MockAmountAmbiguous, "l11-settle")
	claim, err := w.claim(t, wr.ID)
	if err != nil || claim.Denied {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	gr := DispatchWithdraw(context.Background(), w.pool, MockCredentialResolver{}, w.provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	att := w.reread(t, claim.Attempt.ID)
	if att.ProviderReference == nil {
		t.Fatalf("setup: expected a provider reference, state=%s", att.State)
	}
	l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", "block")
	w.tx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, "l11-settle", ReceiptEvidence{
			EventType: "payout", ProviderReference: *att.ProviderReference, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR",
		})
		return err
	})
	if got := w.state(t, wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("a payout already sent completes under a configured block, got %s", got.State)
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
	loAssertProjectionMatchesRebuild(t, w.pool, w.f.tenantID)
}
