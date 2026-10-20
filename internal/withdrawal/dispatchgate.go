package withdrawal

// F4 (platform-freeze work list 2026-10-16; A6 SR6 I-2 / residual 4): the WD-RG-1 RG/risk gate RE-RUN at payout
// dispatch, and the recorded decision the database backstop (migration 0169) requires.
//
// Rules: EXACTLY the request-time gate (rggate.go and withdrawalgate/standard): the same injected
// withdrawalgate.Gate, the same server-resolved input (tenant, brand, account, wallet, asset, amount - all
// immutable columns of the request row), so the RG half allows during a self-exclusion / time-out (ADR 0115,
// owner decision) and refuses an account or wallet that is not active or a negative wallet balance, and the
// risk half refuses DENY and REVIEW. An evaluation error is ErrGateUnavailable (fail closed, retryable), never an
// allow. A nil gate is ErrGateUnavailable.
//
// Lock order (ADR 0082 A7-N1, the note in rggate.go): the RG half takes the L0.4 person advisory lock and the risk
// half may take L0.5 cumulative locks, so EvaluateDispatchGate MUST run before the first L1 parent lock
// (withdrawal_requests FOR UPDATE) of its transaction. RecordDispatchGateDecision runs after it (it inserts a row
// whose FK takes KEY SHARE on the request row) and before the transition it authorizes.
//
// An ALLOW row is a SIGNED attestation of the application server (dispatchproof.go, migration 0171): without a proof
// the database refuses the insert, so arbitrary SQL cannot forge one.
//
// The database (0169) refuses approved -> submitted (WD053) and a payout attempt's created|ambiguous -> submitting
// (WD053) unless an ALLOWING decision row of the matching stage was recorded in THE SAME transaction and no denying
// one was; it re-checks the account and wallet STATUS in SQL at the instant of the transition (WD052). The
// negative-balance refusal is applied by the gate here, not mirrored in SQL (record residual R2).

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// DispatchStage names the dispatch point a gate decision was taken at (withdrawal_gate_decisions.stage).
type DispatchStage string

const (
	// DispatchStageDispatch is the T1p claim (approved -> submitted).
	DispatchStageDispatch DispatchStage = "dispatch"
	// DispatchStageRedispatch is a new Withdraw call on an existing payout attempt (T2 re-claim, T12 resend).
	DispatchStageRedispatch DispatchStage = "redispatch"
)

// SQLSTATEs of migration 0169.
const (
	// SQLStateDispatchRGBackstop: the RG half re-checked by the database refused the transition.
	SQLStateDispatchRGBackstop = "WD052"
	// SQLStateDispatchNoDecision: no allowing gate decision of this transaction (or a denying one).
	SQLStateDispatchNoDecision = "WD053"
)

// ErrDispatchGateBackstop wraps a WD052 / WD053 refusal by the 0169 database backstop. Reaching it from the
// governed dispatch code is a bug (the Go gate runs first and records the decision); it fails closed, nothing moves.
var ErrDispatchGateBackstop = errors.New("withdrawal: payout dispatch refused by the database gate backstop")

// TranslateDispatchBackstopError maps a WD052 / WD053 database refusal to ErrDispatchGateBackstop (wrapped with
// the SQLSTATE); any other error is returned unchanged.
func TranslateDispatchBackstopError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == SQLStateDispatchRGBackstop || pgErr.Code == SQLStateDispatchNoDecision) {
		return fmt.Errorf("%w (%s): %w", ErrDispatchGateBackstop, pgErr.Code, err)
	}
	return err
}

// DispatchGateOption configures EvaluateDispatchGate.
type DispatchGateOption func(*dispatchGateOptions)

type dispatchGateOptions struct{ stage RestrictionStage }

// AtRedispatch evaluates the withdrawal restriction policy at the re-dispatch stage (T2 / T12). The default is the T1p
// dispatch stage.
func AtRedispatch() DispatchGateOption {
	return func(o *dispatchGateOptions) { o.stage = RestrictionStageRedispatch }
}

// EvaluateDispatchGate runs, for an existing request at a dispatch point, FIRST the tenant's withdrawal restriction policy
// (lic11, migration 0182; restriction.go) and then the WD-RG-1 gate. It returns the Decision (Allowed, a WD050 / WD051
// denial of the gate, or a WD056 denial of a CONFIGURED restriction policy, which skips the gate) with the restriction
// provenance attached (Decision.Restriction), or an error wrapping ErrGateUnavailable. It writes nothing and takes the
// gate's own L0.x locks only (the policy evaluation is plain reads): call it BEFORE the request's L1 lock (see the file
// comment).
func EvaluateDispatchGate(ctx context.Context, tx pgx.Tx, gate withdrawalgate.Gate, wr WithdrawalRequest, opts ...DispatchGateOption) (withdrawalgate.Decision, error) {
	if gate == nil {
		return withdrawalgate.Decision{}, fmt.Errorf("%w: no gate was supplied at payout dispatch", ErrGateUnavailable)
	}
	o := dispatchGateOptions{stage: RestrictionStageDispatch}
	for _, f := range opts {
		f(&o)
	}
	rd, err := EvaluateRestriction(ctx, tx, wr.TenantID, wr.BrandID, o.stage)
	if err != nil {
		return withdrawalgate.Decision{}, err
	}
	if rd.Refuses() {
		if rd.Source != RestrictionSourceConfigured || rd.DecidingCause == "" {
			// lic17 L-2: only a CONFIGURED hold / block may refuse (the WD056 row shape, 0182); a refusing default would be a
			// registry change nobody reviewed for this path. Fail closed and diagnosable, never a malformed decision row.
			return withdrawalgate.Decision{}, fmt.Errorf("%w: a restriction refusal from source %q cannot be recorded", ErrGateUnavailable, rd.Source)
		}
		return rd.DispatchDenial(), nil
	}
	d, err := gate.Evaluate(ctx, tx, withdrawalgate.Input{
		TenantID: wr.TenantID, BrandID: wr.BrandID, PlayerAccountID: wr.PlayerAccountID, WalletID: wr.WalletID,
		AssetCode: wr.AssetCode, Amount: wr.Amount,
	})
	if err != nil {
		return withdrawalgate.Decision{}, fmt.Errorf("%w: %w", ErrGateUnavailable, err)
	}
	if !d.Allowed && d.GateCode != GateCodeRG && d.GateCode != GateCodeRisk {
		return withdrawalgate.Decision{}, fmt.Errorf("%w: unrecognised gate code %q", ErrGateUnavailable, d.GateCode)
	}
	// The provenance is ours, never the gate implementation's.
	d.Restriction = rd.Provenance()
	return d, nil
}

var decisionReasonShape = regexp.MustCompile(`^[a-z0-9_.:-]{1,64}$`)

// RecordDispatchGateDecision appends the withdrawal_gate_decisions row for d (stage, and the attempt for a
// re-dispatch) in tx. The database sets the row id, decided_at and decided_xact and refuses (WD054) a row whose
// request is not visible, not in the stage's state, or whose attempt is not a payout attempt of the request.
func RecordDispatchGateDecision(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, stage DispatchStage, attemptID *uuid.UUID, d withdrawalgate.Decision) error {
	if stage != DispatchStageDispatch && stage != DispatchStageRedispatch {
		return fmt.Errorf("%w: unknown dispatch stage %q", ErrInvalidInput, stage)
	}
	var gateCode, reason, outcome, restriction *string
	if !d.Allowed {
		gc := d.GateCode
		gateCode = &gc
		r := d.Reason
		if !decisionReasonShape.MatchString(r) {
			r = "unclassified"
		}
		reason = &r
	}
	switch d.Outcome {
	case "allow", "deny", "review":
		o := d.Outcome
		outcome = &o
	}
	switch d.RestrictionInForce {
	case "self_exclusion", "time_out":
		ri := d.RestrictionInForce
		restriction = &ri
	}
	if d.Allowed {
		// F4 residual R1 (migration 0171): an ALLOW is a signed attestation, verified and consumed by the database in this
		// insert (dispatchproof.go). A denial is not proof-bound (it only restricts).
		if err := attachDispatchAllowProof(ctx, tx, wr, stage, attemptID, outcome, restriction); err != nil {
			return fmt.Errorf("withdrawal: record dispatch gate decision: %w", err)
		}
	}
	// lic11 (migration 0182): the restriction policy provenance evaluated with this decision (NULL only for a fixture
	// decision built without one). Not part of the signed allow digest (0171 unchanged): only this function writes an
	// allowing row, and the row is append-only.
	var rSource, rCause, rAction *string
	var rSetVersion *int64
	if p := d.Restriction; p != nil {
		src, sv := p.Source, p.SetVersion
		rSource, rSetVersion = &src, &sv
		if p.Cause != "" {
			c := p.Cause
			rCause = &c
		}
		if p.Action != "" {
			a := p.Action
			rAction = &a
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawal_gate_decisions
		     (tenant_id, withdrawal_request_id, payment_attempt_id, stage, allowed, gate_code, reason_code, risk_outcome, restriction_in_force,
		      restriction_policy_source, restriction_policy_set_version, restriction_cause, restriction_action)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		wr.TenantID, wr.ID, attemptID, string(stage), d.Allowed, gateCode, reason, outcome, restriction,
		rSource, rSetVersion, rCause, rAction,
	); err != nil {
		return fmt.Errorf("withdrawal: record dispatch gate decision: %w", err)
	}
	return nil
}
