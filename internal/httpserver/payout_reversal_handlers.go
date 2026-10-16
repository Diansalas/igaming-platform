package httpserver

// A5 / WITHDRAWAL-REVERSAL-1: POST /v1/admin/withdrawals/{id}/payout-reversal records ONE staff decision
// on a governed payout reversal (payments.DecidePayoutReversal). Tenant-scoped finance staff with
// withdrawal:approve; step-up class critical (ADR 0119 rule 8.1 (d): each half of a four-eyes money
// action). The decider is ALWAYS the caller's authenticated subject (never a body field); the tenant is
// the token's. The body is the payload every approver must agree on; the reversal executes in the call
// that completes the quorum (two distinct Persons, more if the withdrawal policy asks). Providers are MOCK.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type payoutReversalRequest struct {
	ProviderID                string `json:"provider_id"`
	OriginalProviderReference string `json:"original_provider_reference"`
	ReturnReference           string `json:"return_reference"`
	// Amount is a decimal string of minor units (never a float).
	Amount          string `json:"amount"`
	AssetCode       string `json:"asset_code"`
	EvidenceRefHash string `json:"evidence_ref_hash"`
}

type payoutReversalResponse struct {
	Outcome             string  `json:"outcome"`
	CountedApprovals    int     `json:"counted_approvals"`
	RequiredApprovals   int     `json:"required_approvals"`
	LedgerTransactionID *string `json:"ledger_transaction_id,omitempty"`
}

// parseMinorUnits parses a positive decimal string of minor units that fits int64 (no sign, no fraction).
func parseMinorUnits(s string) (int64, error) {
	if s == "" || len(s) > 19 {
		return 0, errors.New("amount must be a decimal string of minor units")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("amount must be a decimal string of minor units")
		}
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || !v.IsInt64() || v.Sign() <= 0 {
		return 0, errors.New("amount must be a positive integer of minor units")
	}
	return v.Int64(), nil
}

func newPayoutReversalHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal id")
			return
		}
		var req payoutReversalRequest
		if err := decodeJSONStrict(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		amount, err := parseMinorUnits(req.Amount)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		payload := withdrawal.PayoutReversal{
			ProviderID: req.ProviderID, OriginalProviderReference: req.OriginalProviderReference, ReturnReference: req.ReturnReference,
			Amount: amount, AssetCode: req.AssetCode, EvidenceRefHash: req.EvidenceRefHash,
		}
		if err := payload.Validate(); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid payout reversal payload")
			return
		}
		stepUp, _ := auth.StepUpAssertionFrom(r.Context())
		res, err := payments.DecidePayoutReversal(r.Context(), deps.DB, tc.TenantID, id, payments.PayoutReversalInput{
			ApproverID: staffID, Payload: payload, StepUp: stepUp,
			Checks: func(ctx context.Context, tx pgx.Tx) (withdrawal.BeneficiaryCheck, withdrawal.ApproverEligibility) {
				benef := func(approver uuid.UUID) (bool, error) {
					is, found, err := staffIsWithdrawalBeneficiary(ctx, tx, id, approver)
					if err == nil && !found {
						return false, fmt.Errorf("httpserver: payout reversal beneficiary check: request not found")
					}
					return is, err
				}
				return benef, approverEligibilityCheckLocked(ctx, tx, staffID, tc.Role, tc.StaffVersion)
			},
			Audit: func(ctx context.Context, tx pgx.Tx, res payments.PayoutReversalResult) error {
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
					Action: "withdrawal.payout_reversal.http", TargetType: "withdrawal_request", TargetID: id.String(),
					Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
					Metadata: map[string]any{"outcome": string(res.Outcome), "counted": res.Counted, "required": res.Required,
						"payload_digest": payload.Digest(id)},
				})
			},
		})
		switch {
		case err == nil:
		case errors.Is(err, withdrawal.ErrNotFound):
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal not found")
			return
		case errors.Is(err, withdrawal.ErrInvalidInput):
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid payout reversal payload")
			return
		case errors.Is(err, withdrawal.ErrStateConflict):
			apierror.Write(w, requestID, apierror.CodeConflict, "the withdrawal is not a completed or submitted payout")
			return
		case errors.Is(err, withdrawal.ErrDuplicateApproval):
			apierror.Write(w, requestID, apierror.CodeConflict, "you have already recorded a reversal decision for this withdrawal")
			return
		case errors.Is(err, withdrawal.ErrReversalPayloadMismatch):
			apierror.Write(w, requestID, apierror.CodeConflict, "the reversal does not match the payout (provider, reference, amount or asset)")
			return
		case errors.Is(err, withdrawal.ErrReversalReturnReferenceInUse):
			apierror.Write(w, requestID, apierror.CodeConflict, "the return reference is already held by the platform for this provider")
			return
		case errors.Is(err, withdrawal.ErrReversalCapabilityRequired):
			apierror.Write(w, requestID, apierror.CodeForbidden, "a reversal of a submitted payout requires the payment_force_resolve:approve capability")
			return
		case errors.Is(err, withdrawal.ErrReversalAlreadyExists):
			apierror.Write(w, requestID, apierror.CodeConflict, "the payout has already been reversed")
			return
		case errors.Is(err, payments.ErrPayoutReversalAttemptInFlight):
			apierror.Write(w, requestID, apierror.CodeConflict, "the payout has an attempt in flight; retry once it settles")
			return
		case errors.Is(err, payments.ErrPayoutReversalAttemptInconsistent):
			apierror.Write(w, requestID, apierror.CodeConflict, "the payout attempts do not allow a reversal; investigate")
			return
		case errors.Is(err, withdrawal.ErrSelfApproval):
			auditBeneficiaryRefusal(r, deps, logger, tc.TenantID, staffID, id, "withdrawal.payout_reversal.http", requestID)
			apierror.Write(w, requestID, apierror.CodeForbidden, "cannot decide a reversal that credits yourself")
			return
		case errors.Is(err, withdrawal.ErrApproverNotLinked):
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account has no confirmed Person linkage")
			return
		case errors.Is(err, withdrawal.ErrApproverInactive):
			apierror.Write(w, requestID, apierror.CodeForbidden, "this staff account is not active")
			return
		case errors.Is(err, withdrawal.ErrStepUpRequired):
			auth.WriteStepUpRequired(w, requestID, auth.StepUpPolicyMaxAge)
			return
		case errors.Is(err, db.ErrActorStaffNotLive):
			writeSessionRevoked(w, requestID)
			return
		default:
			logger.Error("payout_reversal_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to record the payout reversal decision")
			return
		}
		out := payoutReversalResponse{Outcome: string(res.Outcome), CountedApprovals: res.Counted, RequiredApprovals: res.Required}
		if res.LedgerTransactionID != nil {
			s := res.LedgerTransactionID.String()
			out.LedgerTransactionID = &s
		}
		writeJSON(w, http.StatusOK, out)
	}
}
