//go:build integration

// ADR 0116 slice S1: the AML hold composed into kyc.EvaluateEnforcement. The properties under test:
//
//   - public equivalence (C4): a KYC `pending`, an AML `held` and a hold-evaluator error give byte-identical gate results
//     (outcome, allowed, code, message) at ALL FIVE operations, and byte-identical rows in the tenant-visible decision log
//     and audit log (outcome `review`, no matched trigger);
//   - the AML-scoped log (aml_enforcement_decisions) carries the TRUE outcome, and only for held / evaluator-error rows;
//   - L1 / C6: the hold is evaluated even when the KYC path ends in the no-licence `not_required` early return;
//   - fail closed: an evaluator error or timeout denies exactly the evaluated operation and raises aml.hold_eval_failed;
//   - a KYC failed / unavailable keeps its own public result even when a hold also matches.
package kyc

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

var allKYCOps = []EnforcementOperation{EnforcementDeposit, EnforcementWithdrawalHold, EnforcementWithdrawalPayout, EnforcementCasinoPlay, EnforcementSportsbookPlay}

// armedFixture is a licensed tenant whose five operations all require a passed verification (a deposit threshold of 1 EUR
// minor unit, play policies for both products), so a non-passed verification yields `pending` / `failed` at every operation.
func armedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	f := seedFixture(t, pool)
	j := mustLicenseTenant(t, pool, f)
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{LicensingJurisdictionID: j, TriggerType: "cumulative_deposit", ThresholdMinorUnits: strPtr("1"), AssetCode: strPtr("EUR")})
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{LicensingJurisdictionID: j, TriggerType: "play", PlayOperation: strPtr("casino_play")})
	mustActivePolicy(t, pool, CreateEnforcementPolicyParams{LicensingJurisdictionID: j, TriggerType: "play", PlayOperation: strPtr("sportsbook_play")})
	return f
}

func evalAndRecord(t *testing.T, pool *db.Pool, f fixture, op EnforcementOperation) EnforcementDecision {
	t.Helper()
	var d EnforcementDecision
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p := EnforcementParams{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: op, AssetCode: "EUR", Amount: 1000, CorrelationID: uuid.New()}
		var err error
		if d, err = EvaluateEnforcement(ctx, tx, p); err != nil {
			return err
		}
		return RecordDecision(ctx, tx, p, d)
	})
	if err != nil {
		t.Fatalf("evaluate+record %s: %v", op, err)
	}
	return d
}

type gateView struct {
	Outcome EnforcementOutcome
	Allowed bool
	Code    string
	Message string
	Policy  string
}

func view(d EnforcementDecision) gateView {
	return gateView{d.Outcome, d.Allowed, d.Code, d.Message, d.PolicyVersion}
}

type logRow struct {
	Operation, Outcome, PolicyVersion string
	Allowed, HasTrigger               bool
}

func tenantLog(t *testing.T, pool *db.Pool, f fixture) map[string]logRow {
	t.Helper()
	out := map[string]logRow{}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT operation, outcome, allowed, matched_trigger IS NOT NULL, policy_version
			FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, f.playerID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r logRow
			if err := rows.Scan(&r.Operation, &r.Outcome, &r.Allowed, &r.HasTrigger, &r.PolicyVersion); err != nil {
				return err
			}
			out[r.Operation] = r
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// auditView returns the (action, outcome, kyc_outcome metadata) of the player's enforcement audit rows, per operation.
func auditView(t *testing.T, pool *db.Pool, f fixture) map[string][3]string {
	t.Helper()
	out := map[string][3]string{}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata->>'operation', action, outcome, metadata->>'kyc_outcome' FROM audit_log
			WHERE tenant_id = $1 AND target_id = $2 AND action LIKE 'kyc.enforcement\_%'`, f.tenantID, f.playerID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var op, action, oc, ko string
			if err := rows.Scan(&op, &action, &oc, &ko); err != nil {
				return err
			}
			out[op] = [3]string{action, oc, ko}
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

type amlRow struct {
	Hold, KYC, Public string
	Allowed, HasHold  bool
}

func amlLog(t *testing.T, pool *db.Pool, f fixture) map[string]amlRow {
	t.Helper()
	out := map[string]amlRow{}
	if err := launchfix.OwnerFor(t, pool).WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT operation, hold_outcome, kyc_outcome, public_outcome, allowed, hold_id IS NOT NULL
			FROM aml_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, f.playerID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var op string
			var r amlRow
			if err := rows.Scan(&op, &r.Hold, &r.KYC, &r.Public, &r.Allowed, &r.HasHold); err != nil {
				return err
			}
			out[op] = r
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func installProofs(t *testing.T) {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	prooftest.Install(t, u)
}

// holdFor places a hold for the fixture's person through the real officer API.
func holdFor(t *testing.T, pool *db.Pool, f fixture, scope aml.HoldScope) {
	t.Helper()
	installProofs(t)
	amlfix.HoldFor(t, launchfix.OwnerFor(t, pool), pool, f.tenantID, f.personID, nil, scope)
}

func TestHoldEquivalence_PendingHeldEvalError_AllFiveOperations(t *testing.T) {
	installProofs(t)
	pool := testPool(t)

	pending := armedFixture(t, pool)
	setVerification(t, pool, pending, StatusPending, nil)

	held := armedFixture(t, pool)
	setVerification(t, pool, held, StatusApproved, nil)
	holdFor(t, pool, held, aml.ScopeAllValueMovement)

	broken := armedFixture(t, pool)
	setVerification(t, pool, broken, StatusApproved, nil)

	for _, op := range allKYCOps {
		dp := evalAndRecord(t, pool, pending, op)
		dh := evalAndRecord(t, pool, held, op)
		restore := aml.SetHoldProbeForTest(func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (bool, error) {
			return false, errors.New("injected evaluator failure")
		})
		de := evalAndRecord(t, pool, broken, op)
		restore()

		want := gateView{OutcomePending, false, AccountReviewCode, accountReviewMessage, PolicyVersion}
		for name, d := range map[string]EnforcementDecision{"kyc pending": dp, "aml held": dh, "evaluator error": de} {
			if view(d) != want {
				t.Errorf("%s at %s: gate view %+v, want the shared %+v", name, op, view(d), want)
			}
		}
		if dp.Hold.Denied() || !dh.Hold.Held || dh.Hold.HoldID == uuid.Nil || !de.Hold.EvalFailed {
			t.Errorf("%s: internal verdicts pending=%+v held=%+v err=%+v", op, dp.Hold, dh.Hold, de.Hold)
		}
		if dh.TrueOutcome != OutcomeHeld {
			t.Errorf("%s: the held decision's internal outcome is %q", op, dh.TrueOutcome)
		}
	}

	// The tenant-visible logs are indistinguishable.
	lp, lh, le := tenantLog(t, pool, pending), tenantLog(t, pool, held), tenantLog(t, pool, broken)
	ap, ah, ae := auditView(t, pool, pending), auditView(t, pool, held), auditView(t, pool, broken)
	if len(lp) != 5 || len(lh) != 5 || len(le) != 5 {
		t.Fatalf("one decision row per operation: %d %d %d", len(lp), len(lh), len(le))
	}
	for _, op := range allKYCOps {
		k := string(op)
		wantRow := logRow{Operation: k, Outcome: "review", PolicyVersion: PolicyVersion, Allowed: false, HasTrigger: false}
		for name, m := range map[string]map[string]logRow{"kyc pending": lp, "aml held": lh, "evaluator error": le} {
			if m[k] != wantRow {
				t.Errorf("%s decision log at %s: %+v, want %+v", name, op, m[k], wantRow)
			}
		}
		wantAudit := [3]string{"kyc.enforcement_denied", "failure", "review"}
		for name, m := range map[string]map[string][3]string{"kyc pending": ap, "aml held": ah, "evaluator error": ae} {
			if m[k] != wantAudit {
				t.Errorf("%s audit log at %s: %v, want %v", name, op, m[k], wantAudit)
			}
		}
	}

	// The AML-scoped log: the true outcome, only for held / evaluator-error evaluations.
	if got := amlLog(t, pool, pending); len(got) != 0 {
		t.Errorf("a plain KYC pending writes no AML row: %+v", got)
	}
	for _, op := range allKYCOps {
		r := amlLog(t, pool, held)[string(op)]
		if r != (amlRow{Hold: "held", KYC: "passed", Public: "review", Allowed: false, HasHold: true}) && r != (amlRow{Hold: "held", KYC: "not_required", Public: "review", Allowed: false, HasHold: true}) {
			t.Errorf("held AML row at %s: %+v", op, r)
		}
		r = amlLog(t, pool, broken)[string(op)]
		if r.Hold != "eval_error" || r.Public != "review" || r.Allowed || r.HasHold {
			t.Errorf("evaluator-error AML row at %s: %+v", op, r)
		}
	}
}

// L1 / C6: the no-licence `not_required` early return of the KYC path never skips the hold.
func TestHold_EvaluatedEvenWhenKYCIsNotRequired(t *testing.T) {
	installProofs(t)
	pool := testPool(t)
	f := seedFixture(t, pool) // no licence bound: deposit and play are not_required
	holdFor(t, pool, f, aml.ScopeAllValueMovement)
	for _, op := range []EnforcementOperation{EnforcementDeposit, EnforcementCasinoPlay, EnforcementSportsbookPlay} {
		d := evalPlay(t, pool, f, op)
		if d.Allowed || d.Outcome != OutcomePending || d.Code != AccountReviewCode || !d.Hold.Held || d.KYCOutcome != OutcomeNotRequired {
			t.Errorf("%s: a not_required KYC result must still be overridden by the hold: %+v", op, d)
		}
	}
	// A withdrawal-only hold does not gate deposit / play (D9: only the named operations).
	g := seedFixture(t, pool)
	holdFor(t, pool, g, aml.ScopeWithdrawalAndPayout)
	for _, op := range []EnforcementOperation{EnforcementDeposit, EnforcementCasinoPlay, EnforcementSportsbookPlay} {
		if d := evalPlay(t, pool, g, op); !d.Allowed || d.Hold.Denied() {
			t.Errorf("%s must NOT be gated by a withdrawal_and_payout hold: %+v", op, d)
		}
	}
	setVerification(t, pool, g, StatusApproved, nil)
	for _, op := range []EnforcementOperation{EnforcementWithdrawalHold, EnforcementWithdrawalPayout} {
		if d := evalPlay(t, pool, g, op); d.Allowed || d.Outcome != OutcomePending || !d.Hold.Held {
			t.Errorf("%s must be gated by a withdrawal_and_payout hold even with a passed KYC: %+v", op, d)
		}
	}
}

// A KYC failed / unavailable keeps its own public result (the player already sees a deny), the hold is still recorded, and
// nothing about the hold leaks into the decision.
func TestHold_KYCFailedKeepsItsOwnPublicResult_ButIsRecorded(t *testing.T) {
	installProofs(t)
	pool := testPool(t)
	f := armedFixture(t, pool) // no verification: failed
	holdFor(t, pool, f, aml.ScopeAllValueMovement)
	d := evalAndRecord(t, pool, f, EnforcementCasinoPlay)
	if d.Outcome != OutcomeFailed || d.Allowed || d.Code != "kyc_casino_play:failed" {
		t.Fatalf("a failed KYC keeps its public result: %+v", d)
	}
	if !d.Hold.Held || d.TrueOutcome != OutcomeHeld {
		t.Fatalf("the hold is still recorded internally: %+v", d)
	}
	r := amlLog(t, pool, f)["casino_play"]
	if r.Hold != "held" || r.KYC != "failed" || r.Public != "failed" || r.Allowed {
		t.Fatalf("AML row: %+v", r)
	}
	if l := tenantLog(t, pool, f)["casino_play"]; l.Outcome != "failed" {
		t.Fatalf("the tenant log shows the public KYC outcome only: %+v", l)
	}
}

// Fail closed with a REAL evaluator error (no injection): a session with no tenant binding makes aml_hold_active raise AM026.
// The operation is denied (here the tenant-less session also sees no KYC rows, so the KYC half is `failed` and keeps its own
// public result), the verdict is recorded as an evaluator failure, and the caller's transaction stays usable.
func TestHold_RealEvaluatorError_DeniesOnlyThatOperation_TransactionStaysUsable(t *testing.T) {
	installProofs(t)
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)
	var d EnforcementDecision
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = EvaluateEnforcement(ctx, tx, EnforcementParams{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			Operation: EnforcementWithdrawalHold, AssetCode: "EUR", Amount: 1, CorrelationID: uuid.New()})
		if err != nil {
			return err
		}
		// The caller's transaction is still usable after the contained failure.
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
	})
	if err != nil {
		t.Fatalf("a contained evaluator failure must not break the caller's transaction: %v", err)
	}
	if d.Allowed || !d.Hold.EvalFailed || d.Hold.Held {
		t.Fatalf("the real evaluator error must deny and be recorded as an evaluator failure: %+v", d)
	}
}

// The alert row for the injected failure carries the operation only: no ids, no match category.
func TestHold_EvalFailedAlert_PayloadIsOperationOnly(t *testing.T) {
	installProofs(t)
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusApproved, nil)
	restore := aml.SetHoldProbeForTest(func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (bool, error) {
		return false, context.DeadlineExceeded
	})
	d := evalAndRecord(t, pool, f, EnforcementWithdrawalPayout)
	restore()
	if d.Allowed || !d.Hold.EvalFailed {
		t.Fatalf("a timeout must deny: %+v", d)
	}
	var kind, attrs, disc string
	var subj uuid.UUID
	launchfix.SeedOperators(t)
	if err := launchfix.OwnerFor(t, pool).WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT kind, attributes::text, discriminator, subject_tenant_id FROM alerts WHERE kind = 'aml.hold_eval_failed' AND subject_tenant_id = $1`, f.tenantID).
			Scan(&kind, &attrs, &disc, &subj)
	}); err != nil {
		t.Fatalf("the failed evaluation must raise aml.hold_eval_failed: %v", err)
	}
	if attrs != `{"operation": "payout"}` || disc != "hold_eval:payout" || subj != f.tenantID {
		t.Fatalf("alert payload: attrs=%s discriminator=%s", attrs, disc)
	}
}

// The reader maps a legacy `pending` row to the public vocabulary and shows new rows as `review`.
func TestDecisionReader_MapsPendingToReview(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	setVerification(t, pool, f, StatusPending, nil)
	evalAndRecord(t, pool, f, EnforcementWithdrawalHold)
	// A legacy row, as written before migration 0141.
	if err := launchfix.OwnerFor(t, pool).WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_enforcement_decisions (tenant_id, brand_id, player_account_id, operation, outcome, allowed, policy_version, correlation_id)
			VALUES ($1, $2, $3, 'deposit', 'pending', false, $4, $5)`, f.tenantID, f.brandID, f.playerID, PolicyVersion, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := ListEnforcementDecisionsForPlayer(ctx, tx, ListEnforcementDecisionsParams{TenantID: f.tenantID, PlayerAccountID: f.playerID})
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			t.Fatalf("rows: %+v", rows)
		}
		for _, r := range rows {
			if r.Outcome != "review" {
				t.Errorf("the reader must show review, got %q (%s)", r.Outcome, r.Operation)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
