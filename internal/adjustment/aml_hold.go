package adjustment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// holdCheck is the AML verdict for a wallet's player plus the ids needed to record the real reason in the AML-scoped evidence.
type holdCheck struct {
	verdict   aml.HoldVerdict
	tenantID  uuid.UUID
	accountID uuid.UUID
	personID  uuid.UUID
}

func (h holdCheck) denied() bool { return h.verdict.Denied() }

// refusal is the error a hold-caused refusal returns. It renders EXACTLY like an ordinary conflict (ErrClassConflict, no SQLSTATE,
// no AML token anywhere: security F4, option (b), a reversible orchestrator default, not owner-approved). The real reason travels in
// the error value only so the Service can write it to the owner-only AML decision log AFTER the rolled-back transaction.
func (h holdCheck) refusal(requestID uuid.UUID) error {
	return &amlRefusal{holdCheck: h, requestID: requestID}
}

// amlRefusal satisfies errors.Is(err, ErrAccountUnderReview) (an internal sentinel, never rendered) and carries the evidence.
type amlRefusal struct {
	holdCheck
	requestID uuid.UUID
}

func (*amlRefusal) Error() string        { return ErrAccountUnderReview.Error() }
func (*amlRefusal) Is(target error) bool { return target == ErrAccountUnderReview }

// walletHoldCheck evaluates the AML hold for the player that owns walletID, for the operation manual_adjustment (ANY active hold
// refuses: ADR 0116 s5.1 "Held balances"). It runs inside the caller's transaction and NEVER raises the evaluator-failure alert
// there: every caller refuses by returning an error (its transaction rolls back), so the alert is raised, and the evidence recorded,
// by the Service after the rollback (ledger-finance C3). A wallet that does not resolve is left to the existing payload rules
// (wallet_not_found): no verdict is invented for it.
func walletHoldCheck(ctx context.Context, tx pgx.Tx, tenantID, walletID uuid.UUID) (holdCheck, error) {
	h := holdCheck{tenantID: tenantID}
	err := tx.QueryRow(ctx, `SELECT w.player_account_id, pa.person_id
		FROM wallets w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
		WHERE w.id = $1 AND w.tenant_id = $2`, walletID, tenantID).Scan(&h.accountID, &h.personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return holdCheck{}, nil
	}
	if err != nil {
		h.verdict = aml.HoldVerdict{EvalFailed: true}
		return h, fmt.Errorf("adjustment: resolve the wallet's player for the AML hold check: %w", err)
	}
	h.verdict, err = aml.EvaluateHoldDeferringAlert(ctx, tx, tenantID, h.personID, h.accountID, aml.OpManualAdjustment)
	return h, err
}

// sideRecordTimeout bounds the post-rollback side writes (evidence row, evaluator alert): an ENGINEERING DEFAULT (the deniedAuditCtx
// pattern). The refusal has already been decided; a starved pool must delay the response by at most this long per write, never hang it.
const sideRecordTimeout = 2 * time.Second

var meter = otel.Meter("github.com/Diansalas/igaming-platform/internal/adjustment")

// sideRecordFailuresTotal counts failed post-refusal side writes. The name and labels are deliberately generic (no AML wording: an
// operator dashboard or a log reader on the tenant side must not learn what the side write was).
var sideRecordFailuresTotal, _ = meter.Int64Counter(
	"adjustment_side_record_failures_total",
	metric.WithDescription("Failed best-effort side writes after a refused adjustment request."),
)

func sideRecordFailed(ctx context.Context, step string, correlation uuid.UUID, err error) {
	// Only a SQLSTATE or a coarse kind is logged: the error text of a failed AML write names AML objects.
	kind := "other"
	switch {
	case SQLState(err) != "":
		kind = "sqlstate:" + SQLState(err)
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	}
	slog.Default().Error("adjustment_side_record_failed", "step", step, "correlation_id", correlation.String(), "failure", kind)
	if sideRecordFailuresTotal != nil {
		sideRecordFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("step", step)))
	}
}

// recordAMLRefusal writes the REAL reason of a hold-caused refusal to the owner-only AML decision log (and raises the evaluator-failure
// alert), each in its own bounded transaction after the refusing transaction ended. Best effort: the refusal itself stands either way,
// and a failure is logged (generic event name, correlation id) and counted, never swallowed silently. TIMING: it runs on the request
// goroutine before the response is written (each write bounded by sideRecordTimeout); the response time of a hold-caused refusal is
// therefore longer than an ordinary conflict's (recorded as a residual in ADR 0116 12.4).
func (s *Service) recordAMLRefusal(ctx context.Context, err error) {
	var r *amlRefusal
	if !errors.As(err, &r) || r.tenantID == uuid.Nil {
		return
	}
	outcome := aml.HoldOutcomeHeld
	if !r.verdict.Held {
		outcome = aml.HoldOutcomeEvalError
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sideRecordTimeout)
	defer cancel()
	if werr := s.pool.WithTenant(wctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return aml.RecordEnforcementDecision(ctx, tx, aml.EnforcementRecord{
			TenantID: r.tenantID, PlayerAccountID: r.accountID, PersonID: r.personID, Operation: "manual_adjustment",
			KYCOutcome: "not_evaluated", HoldOutcome: outcome, PublicOutcome: "review", Allowed: false,
			HoldID: r.verdict.HoldID, CorrelationID: r.requestID,
		})
	}); werr != nil {
		sideRecordFailed(wctx, "record", r.requestID, werr)
	}
	if r.verdict.EvalFailed && !r.verdict.Held {
		actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), sideRecordTimeout)
		defer acancel()
		if aerr := aml.RaiseHoldEvalFailedDetached(actx, s.pool, r.tenantID, aml.OpManualAdjustment); aerr != nil {
			sideRecordFailed(actx, "notify", r.requestID, aerr)
		}
	}
}
