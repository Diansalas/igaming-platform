//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 10.5, identity-compliance C6): IC F3 and the
// compliance tests T-A..T-K. Enforcement is a pure function of the verification
// rows; the outbox state of a verification's create row must never change an
// enforcement decision (INV-KYC-OB-1..7).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

var allEnforcementOps = []EnforcementOperation{
	EnforcementDeposit, EnforcementWithdrawalHold, EnforcementWithdrawalPayout, EnforcementCasinoPlay, EnforcementSportsbookPlay,
}

func evalOp(t *testing.T, pool *db.Pool, f fixture, op EnforcementOperation) EnforcementDecision {
	t.Helper()
	var d EnforcementDecision
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = EvaluateEnforcement(ctx, tx, EnforcementParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: op, AssetCode: "EUR", Amount: 1000, CorrelationID: uuid.New(),
		})
		return err
	}); err != nil {
		t.Fatalf("EvaluateEnforcement(%s): %v", op, err)
	}
	return d
}

// outboxStates are the create-row states the F3 tests drive a verification's
// create row into WITHOUT writing the verification.
var outboxStates = []string{"pending", "claimed", "failed_terminal", "cancelled"}

// putCreateRowInState runs phase A for r's player and drives the create row into
// state; the verification stays the phase-A orphan in every state.
func putCreateRowInState(t *testing.T, r *rig, state string) uuid.UUID {
	t.Helper()
	v := r.create()
	switch state {
	case "pending":
	case "claimed":
		claimOne(t, r.w)
	case "failed_terminal":
		passUntilQuiet(t, workerFor(r.pool, mismatchedKYCOutboundResolver{}, r.spy))
	case "cancelled":
		orch := NewOrchestrator(map[string]KYCProvider{"mock": r.spy, "mock2": renamedMock{NewMockKYCProvider(), "mock2"}}, nil)
		passUntilQuiet(t, newScopedWorker(r.pool, orch, NewMockOutboundResolver())) // provider_deconfigured
	default:
		t.Fatalf("unknown state %q", state)
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); string(got.State) != state {
		t.Fatalf("create row = %s, want %s", got.State, state)
	}
	if got := r.reload(v.ID); got.Status != StatusUnverified || got.ProviderReference != "" {
		t.Fatalf("the verification must remain the phase-A orphan in state %s, got %q / %q", state, got.Status, got.ProviderReference)
	}
	return v.ID
}

// 35 / 6. T-A (gate level): with the player's create row in each of pending /
// claimed / failed_terminal / cancelled, every enforcement operation decides
// exactly as if no outbox row existed; an approved player stays Allowed; an
// orphan-only account is OutcomeFailed; never OutcomeUnavailable from outbox
// state alone. Payout gate, deposit gate, withdrawal request, casino and
// sportsbook play all call EvaluateEnforcement with these operations.
func TestOutboxCompliance_35_TA_EnforcementIndependentOfOutboxState(t *testing.T) {
	for _, state := range outboxStates {
		t.Run(state, func(t *testing.T) {
			r := newRig(t)
			control := seedFixture(t, r.pool)
			noVerificationControl := seedFixture(t, r.pool)

			// (a) an approved player who starts a re-verification.
			setVerification(t, r.pool, r.f, StatusApproved, nil)
			setVerification(t, r.pool, control, StatusApproved, nil)
			putCreateRowInState(t, r, state)
			for _, op := range allEnforcementOps {
				got, want := evalOp(t, r.pool, r.f, op), evalOp(t, r.pool, control, op)
				if got != want {
					t.Errorf("op %s, approved player, create row %s: decision %+v differs from the no-outbox-row control %+v", op, state, got, want)
				}
				if got.Outcome == OutcomeUnavailable {
					t.Errorf("op %s: OutcomeUnavailable from outbox state alone", op)
				}
			}
			if d := evalOp(t, r.pool, r.f, EnforcementWithdrawalHold); !d.Allowed || d.Outcome != OutcomePassed {
				t.Errorf("an approved player must stay Allowed with a create row %s, got %+v", state, d)
			}

			// (b) an orphan-only account evaluates exactly like an account with no verification.
			r2 := newRig(t)
			putCreateRowInState(t, r2, state)
			for _, op := range allEnforcementOps {
				got, want := evalOp(t, r2.pool, r2.f, op), evalOp(t, r2.pool, noVerificationControl, op)
				if got != want {
					t.Errorf("op %s, orphan-only account, create row %s: %+v differs from the no-verification control %+v", op, state, got, want)
				}
				if got.Outcome == OutcomeUnavailable {
					t.Errorf("op %s: OutcomeUnavailable from outbox state alone", op)
				}
			}
			if d := evalOp(t, r2.pool, r2.f, EnforcementWithdrawalHold); d.Allowed || d.Outcome != OutcomeFailed {
				t.Errorf("an orphan-only account must be OutcomeFailed, got %+v", d)
			}
		})
	}
}

// 36. T-B: the DECISION-ROWS-1 row (and its audit row) in the orphan-only cases
// equals the no-verification case (R14 recorded: a gate denial during a
// platform-side stall is indistinguishable from "never submitted" on the row).
func TestOutboxCompliance_36_TB_DecisionRowsIdenticalToNoVerificationCase(t *testing.T) {
	type rowShape struct {
		Operation, Outcome, Trigger, Policy string
		Allowed                             bool
	}
	record := func(r *rig, f fixture, op EnforcementOperation) rowShape {
		t.Helper()
		params := EnforcementParams{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: op, AssetCode: "EUR", Amount: 1000, CorrelationID: uuid.New()}
		var out rowShape
		if err := r.pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			d, err := EvaluateEnforcement(ctx, tx, params)
			if err != nil {
				return err
			}
			if err := RecordDecision(ctx, tx, params, d); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT operation, outcome, allowed, coalesce(matched_trigger, ''), policy_version
			                          FROM kyc_enforcement_decisions WHERE correlation_id = $1`, params.CorrelationID).
				Scan(&out.Operation, &out.Outcome, &out.Allowed, &out.Trigger, &out.Policy)
		}); err != nil {
			t.Fatalf("record decision: %v", err)
		}
		return out
	}
	for _, state := range outboxStates {
		r := newRig(t)
		putCreateRowInState(t, r, state)
		control := seedFixture(t, r.pool)
		for _, op := range allEnforcementOps {
			if got, want := record(r, r.f, op), record(r, control, op); got != want {
				t.Errorf("state %s op %s: decision row %+v differs from the no-verification case %+v", state, op, got, want)
			}
		}
	}
}

// 38. T-D: a staff ReviewVerification of the orphan racing create phase C: the
// CAS misses, the row ends cancelled / decided_concurrently, the staff decision
// is intact, and the audit row carries vendor_reference_unbound=true (never the
// reference value). The vendor DID accept: the reference is unbound (runbook).
func TestOutboxCompliance_38_TD_StaffReviewRacingCreatePhaseC(t *testing.T) {
	r := newRig(t)
	const vendorRef = "mock-ref-RACE-UNBOUND-7777"
	var staffID uuid.UUID
	r.createImpl = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		verificationID := uuid.MustParse(strings.TrimPrefix(in.Call.IdempotencyKey, "kv:"))
		staffID = r.staffReview(verificationID, StatusRejected)
		return ProviderResult{ProviderReference: vendorRef, Outcome: ProviderPending, Reason: "created"}, nil
	}
	v := r.create()
	// A document uploaded while the create is live enqueues a pending submit row
	// behind it (N8): the decided_concurrently cascade must cancel it in the SAME
	// transaction, before any worker can claim it.
	seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
	passUntilQuiet(t, r.w)

	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	if row.State != OutboxCancelled || row.CancelReason != string(CancelDecidedConcurrently) {
		t.Fatalf("create row = %s / %q, want cancelled / decided_concurrently (M35: never a re-send loop)", row.State, row.CancelReason)
	}
	sub := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpSubmit)
	if sub.State != OutboxCancelled || sub.CancelReason != string(CancelVerificationNotSubmitted) || sub.Claims != 0 {
		t.Fatalf("the pending submit behind a decided create must be cancelled by the cascade, never claimed (N8): %+v", sub)
	}
	if _, s := r.calls(); s != 0 {
		t.Fatalf("no submit may reach the vendor for a decided orphan, got %d", s)
	}
	if c, _ := r.calls(); c != 1 {
		t.Fatalf("the vendor must be called exactly once (no re-send), got %d", c)
	}
	got := r.reload(v.ID)
	if got.Status != StatusRejected || got.ReviewedBy != staffID || got.ProviderReference != "" {
		t.Fatalf("the staff decision must be intact and the reference unbound: %q reviewed_by %s ref %q", got.Status, got.ReviewedBy, got.ProviderReference)
	}
	a := r.auditFor(v.ID)
	var cancelled []auditRow
	for _, ar := range auditByAction(a, auditActionSubmissionCancelled) {
		if ar.Meta["cancel_reason"] == "decided_concurrently" {
			cancelled = append(cancelled, ar)
		}
	}
	if len(cancelled) != 1 || cancelled[0].Meta["vendor_reference_unbound"] != true {
		t.Fatalf("expected one decided_concurrently cancelled audit row with vendor_reference_unbound=true, got %+v", cancelled)
	}
	if n := len(auditByAction(a, auditActionSubmissionCancelled)); n != 2 {
		t.Fatalf("expected the create's cancel row plus one cascade row for the pending submit, got %d", n)
	}
	if auditCount(a, "kyc.verification_submitted") != 0 {
		t.Fatal("no kyc.verification_submitted row may be written for a decided orphan")
	}
	for _, ar := range a {
		for k, val := range ar.Meta {
			if s, ok := val.(string); ok && strings.Contains(s, vendorRef) {
				t.Fatalf("audit %s.%s carries the vendor reference value", ar.Action, k)
			}
		}
	}
}

// 41. T-G: when the create ends failed_terminal (and, separately, cancelled),
// every pending submit row of that verification is cancelled /
// verification_not_submitted with one audit row each; the documents stay stored.
func TestOutboxCompliance_41_TG_CreateTerminalCancelsPendingSubmits(t *testing.T) {
	for _, mode := range []string{"failed_terminal", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			v := r.create()
			seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png")
			seedDocument(t, r.pool, r.f, v.ID, DocumentSelfie, "s.png")
			if n := len(readOutbox(t, r.pool, r.f.tenantID, v.ID)); n != 3 {
				t.Fatalf("setup: expected create + two submit rows, got %d", n)
			}
			if mode == "failed_terminal" {
				passUntilQuiet(t, workerFor(r.pool, mismatchedKYCOutboundResolver{}, r.spy))
			} else {
				orch := NewOrchestrator(map[string]KYCProvider{"mock": r.spy, "mock2": renamedMock{NewMockKYCProvider(), "mock2"}}, nil)
				passUntilQuiet(t, newScopedWorker(r.pool, orch, NewMockOutboundResolver()))
			}
			create := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
			if string(create.State) != mode {
				t.Fatalf("create row = %s, want %s", create.State, mode)
			}
			cancelled := 0
			for _, row := range readOutbox(t, r.pool, r.f.tenantID, v.ID) {
				if row.Operation != OpSubmit {
					continue
				}
				if row.State != OutboxCancelled || row.CancelReason != string(CancelVerificationNotSubmitted) {
					t.Fatalf("submit row = %s / %q, want cancelled / verification_not_submitted", row.State, row.CancelReason)
				}
				cancelled++
			}
			if cancelled != 2 {
				t.Fatalf("expected both submit rows cancelled, got %d", cancelled)
			}
			if n := auditCount(r.auditFor(v.ID), auditActionSubmissionCancelled); n < 2 {
				t.Fatalf("expected one cancelled audit row per cancelled submit row, got %d", n)
			}
			var docs int
			if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_documents WHERE verification_id = $1`, v.ID).Scan(&docs)
			}); err != nil || docs != 2 {
				t.Fatalf("documents uploaded onto the orphan stay stored (retention HQ-E1-4): %d %v", docs, err)
			}
			if c, s := r.calls(); c != 0 || s != 0 {
				t.Fatalf("no vendor call, got %d/%d", c, s)
			}
		})
	}
}

// 44. T-J: a delayed `sent` whose definitive result is `pending` on a NEW
// verification becomes the latest decided row and moves an approved player to
// OutcomePending (deliberate, pinned: ADR 0095 section 38.2 / R13).
func TestOutboxCompliance_44_TJ_DelayedSentPendingMovesApprovedPlayerToPending(t *testing.T) {
	r := newRig(t)
	setVerification(t, r.pool, r.f, StatusApproved, nil)
	v := r.create()
	if d := evalOp(t, r.pool, r.f, EnforcementWithdrawalHold); !d.Allowed || d.Outcome != OutcomePassed {
		t.Fatalf("while the create waits the approved player stays Allowed, got %+v", d)
	}
	passUntilQuiet(t, r.w) // the MOCK's definitive create result is pending
	if got := r.reload(v.ID); got.Status != StatusPending {
		t.Fatalf("setup: expected pending, got %q", got.Status)
	}
	if d := evalOp(t, r.pool, r.f, EnforcementWithdrawalHold); d.Allowed || d.Outcome != OutcomePending {
		t.Fatalf("R13 pinned: a delayed sent+pending supersedes the approval, got %+v", d)
	}
}

// 45. T-K: every obligation-relevant transition has exactly one audit row with
// the section 3.5 shape, and EVERY worker-written audit row carries
// metadata.platform_service (Q-S3).
func TestOutboxCompliance_45_TK_AuditCompleteness(t *testing.T) {
	r := newRig(t)
	r.w.Config.MaxFailedAttempts = 4
	attempt := 0
	r.createImpl = func(ctx context.Context, in CreateVerificationInput) (ProviderResult, error) {
		attempt++
		if attempt <= 2 {
			return ProviderResult{}, fmt.Errorf("vendor down")
		}
		return ProviderResult{ProviderReference: "tk-ref-" + uuid.NewString()[:8], Outcome: ProviderPending, Reason: "created"}, nil
	}
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	r.pass()
	makeDueNow(t, row.ID)
	r.pass()
	makeDueNow(t, row.ID)
	r.pass()

	a := r.auditFor(v.ID)
	if auditCount(a, auditActionSubmissionEnqueued) != 1 {
		t.Errorf("enqueue: want exactly 1 audit row, got %d", auditCount(a, auditActionSubmissionEnqueued))
	}
	if auditCount(a, auditActionSubmissionRetryScheduled) != 2 {
		t.Errorf("retry: want exactly 2 audit rows, got %d", auditCount(a, auditActionSubmissionRetryScheduled))
	}
	if auditCount(a, "kyc.verification_submitted") != 1 {
		t.Errorf("sent: want exactly 1 audit row, got %d", auditCount(a, "kyc.verification_submitted"))
	}
	for _, x := range a {
		if x.Action == "kyc.verification_requested" || x.Action == auditActionSubmissionEnqueued {
			continue // phase A, written by the player's request
		}
		if x.Actor != "system" || x.Meta["platform_service"] != workerPlatformService || x.Meta["outbox_id"] != row.ID.String() {
			t.Errorf("worker-written audit row %s lacks the system actor / platform_service / outbox_id: %+v", x.Action, x)
		}
	}
	// A terminal and a cancel row, one audit row each.
	r2 := newRig(t)
	r2.w.Outbound = mismatchedKYCOutboundResolver{}
	v2 := r2.create()
	r2.pass()
	a2 := r2.auditFor(v2.ID)
	if auditCount(a2, auditActionSubmissionFailedTerminal) != 1 {
		t.Errorf("terminal: want exactly 1 audit row, got %d", auditCount(a2, auditActionSubmissionFailedTerminal))
	}
	for _, x := range a2 {
		if x.Action == auditActionSubmissionFailedTerminal && (x.Meta["platform_service"] != workerPlatformService || x.Meta["error_class"] != "credential_binding_mismatch") {
			t.Errorf("terminal audit row shape: %+v", x)
		}
	}
	r3 := newRig(t)
	v3 := r3.create()
	r3.staffReview(v3.ID, StatusRejected)
	r3.pass()
	a3 := r3.auditFor(v3.ID)
	if auditCount(a3, auditActionSubmissionCancelled) != 1 {
		t.Errorf("cancel: want exactly 1 audit row, got %d", auditCount(a3, auditActionSubmissionCancelled))
	}
	for _, x := range auditByAction(a3, auditActionSubmissionCancelled) {
		if x.Meta["platform_service"] != workerPlatformService || x.Meta["cancel_reason"] != "decided_concurrently" {
			t.Errorf("cancel audit row shape: %+v", x)
		}
	}
}
