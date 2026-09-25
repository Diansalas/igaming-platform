package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Stage 10 W1 (docs/decisions/0088 §9). This is the ONLY driver of the
// in-house MOCK sportsbook settlement lifecycle (settle/void/rollback) -
// there is no real sportsbook settlement provider or webhook this stage.
// It is a non-production test-support staff route, gated by
// Deps.SportsbookSettlementSimulationEnabled AND Deps.SportsbookEnabled at
// REGISTRATION time (sportsbook_routes.go) - when either is false the
// pattern is never added to the mux at all, so a request against it 404s
// exactly like any other unregistered path, never a 503 from inside a
// handler. Never described as an operator settlement feature or a real
// provider integration.

// simulateSettlementEventRequest is ADR 0088 §9.2's exact request shape.
// Unknown fields are rejected by decodeJSON's DisallowUnknownFields; the
// field-matrix (which fields are required/forbidden per event_type) is
// enforced explicitly below, since encoding/json alone cannot express it.
//
// PayoutAmount is decoded as json.Number, never int64/float64 directly:
// ADR 0088 §2.4 V-5 requires "claim parsed as a JSON integer into int64;
// non-integer, negative, or > math.MaxInt64 rejected AT DECODE" - a plain
// int64 field would let encoding/json silently truncate a fractional
// number's least significant digits is untrue (encoding/json actually
// rejects a non-integer float into an int64 field with its own error), but
// json.Number lets this handler produce ONE explicit, tested rejection
// path (parseClaimPayoutAmount below) for every one of V-5's three failure
// modes, including the explicitly-required overflow case, rather than
// relying on encoding/json's own less specific error text.
type simulateSettlementEventRequest struct {
	EventType    string      `json:"event_type"`
	Generation   int         `json:"generation"`
	Outcome      string      `json:"outcome"`
	PayoutAmount json.Number `json:"payout_amount"`
	AssetCode    string      `json:"asset_code"`
	VoidReason   string      `json:"void_reason"`
}

// simulateSettlementEventResponse is ADR 0088 §9.3's 200 response body.
type simulateSettlementEventResponse struct {
	BetID                string   `json:"bet_id"`
	EventType            string   `json:"event_type"`
	Result               string   `json:"result"`
	BetStatus            string   `json:"bet_status"`
	Generation           int      `json:"generation"`
	LedgerTransactionIDs []string `json:"ledger_transaction_ids"`
	SettlementRecordIDs  []string `json:"settlement_record_ids"`
}

func toSimulateSettlementEventResponse(res sportsbook.SettlementResult) simulateSettlementEventResponse {
	resp := simulateSettlementEventResponse{
		BetID: res.BetID.String(), Result: res.Result, BetStatus: string(res.BetStatus), Generation: res.Generation,
		LedgerTransactionIDs: make([]string, 0, len(res.LedgerTransactionIDs)),
		SettlementRecordIDs:  make([]string, 0, len(res.SettlementRecordIDs)),
	}
	for _, id := range res.LedgerTransactionIDs {
		resp.LedgerTransactionIDs = append(resp.LedgerTransactionIDs, id.String())
	}
	for _, id := range res.SettlementRecordIDs {
		resp.SettlementRecordIDs = append(resp.SettlementRecordIDs, id.String())
	}
	return resp
}

// validateSimulateSettlementEventShape is the HTTP-layer copy of ADR 0088
// §9.2's field matrix (the decode-time half of "fail closed... every
// effect-bearing field is re-derived server-side" - internal/sportsbook's
// own validateSettlementEvent is the service-level copy of the SAME rule,
// run again inside the transaction). Returns a client-safe message on the
// first violation found; ok=false means the caller must respond 400
// VALIDATION_FAILED.
func validateSimulateSettlementEventShape(req simulateSettlementEventRequest) (sportsbook.SettlementEventType, string, bool) {
	switch sportsbook.SettlementEventType(req.EventType) {
	case sportsbook.SettlementEventSettle:
		if req.Generation < 1 {
			return "", "generation is required and must be >= 1 for settle", false
		}
		if req.Outcome != sportsbook.SettlementOutcomeWon && req.Outcome != sportsbook.SettlementOutcomeLost {
			return "", "outcome is required and must be won or lost for settle", false
		}
		if req.AssetCode == "" {
			return "", "asset_code is required for settle", false
		}
		if req.PayoutAmount == "" {
			return "", "payout_amount is required for settle", false
		}
		if req.VoidReason != "" {
			return "", "void_reason is forbidden for settle", false
		}
		return sportsbook.SettlementEventSettle, "", true
	case sportsbook.SettlementEventRollback:
		if req.Generation < 1 {
			return "", "generation is required and must be >= 1 for rollback", false
		}
		if req.Outcome != "" || req.PayoutAmount != "" || req.AssetCode != "" || req.VoidReason != "" {
			return "", "rollback carries only a generation", false
		}
		return sportsbook.SettlementEventRollback, "", true
	case sportsbook.SettlementEventVoid:
		if req.Generation != 0 || req.Outcome != "" || req.PayoutAmount != "" || req.AssetCode != "" {
			return "", "void carries only a void_reason", false
		}
		if req.VoidReason == "" {
			return "", "void_reason is required for void", false
		}
		return sportsbook.SettlementEventVoid, "", true
	default:
		return "", "event_type must be settle, rollback or void", false
	}
}

// parseClaimPayoutAmount is ADR 0088 §2.4 V-5, enforced at decode time: the
// claim must parse as a JSON INTEGER into int64 - non-integer (e.g. "10.5"),
// negative, or overflowing math.MaxInt64/MinInt64 are all rejected here,
// before the claim ever reaches internal/sportsbook. json.Number.Int64
// itself already rejects a fractional literal and anything wider than
// int64 (strconv.ParseInt's own ErrRange) - only the explicit negative
// check is added on top.
func parseClaimPayoutAmount(n json.Number) (int64, bool) {
	if n == "" {
		return 0, true
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	if v < 0 {
		return 0, false
	}
	return v, true
}

// settlementRejectionAPICode maps a sportsbook rejection code (already the
// exact ADR 0088 §9.3 literal, e.g. "SETTLEMENT_PAYLOAD_MISMATCH") to its
// apierror.Code - the two are deliberately the SAME string, declared once
// in each package (see apierror.CodeSettlementPayloadMismatch's own doc
// comment for why), so this is a type conversion, not a translation table.
func settlementRejectionAPICode(rejectionCode string) apierror.Code {
	return apierror.Code(rejectionCode)
}

// settlementIntegrityAlertFields is ADR 0088 §4.4's allow-list, applied
// uniformly by every call site below: tenant_id, bet_id, reason, event_type,
// generation, bet_status, the staff actor id and request_id - NEVER the
// request body, headers, token, or player PII. Mirrors
// casino_play_handlers.go:226's convention (logger.Error(event, kv...))
// exactly, just with a fixed, reviewed field set instead of an ad hoc one.
func logSettlementIntegrityAlert(logger interface {
	Error(string, ...any)
}, reason string, ev sportsbook.SettlementEvent, betStatus string) {
	logger.Error("sportsbook_settlement_integrity_alert_"+reason,
		"tenant_id", ev.TenantID.String(),
		"bet_id", ev.BetID.String(),
		"reason", reason,
		"event_type", string(ev.EventType),
		"generation", ev.Generation,
		"bet_status", betStatus,
		"actor_staff_account_id", ev.ActorStaffID.String(),
		"request_id", ev.RequestID,
	)
}

// alertReasonFromRejectionCode derives the sportsbook_settlement_integrity_
// alert_<reason> suffix from a rejection code - the codes are already
// SCREAMING_SNAKE_CASE (sportsbook.SettlementRejectPayloadMismatch etc.),
// so lower-casing is the entire transform, mirroring
// casino_play_integrity_alert_bet_not_found's own naming shape.
func alertReasonFromRejectionCode(code string) string {
	return strings.ToLower(code)
}

// newSimulateSettlementEventHandler is ADR 0088 §9's test-support staff
// route. See this file's own package doc comment for the registration
// gate; auth is enforced entirely by the middleware chain
// (auth.RequireTenantScope -> auth.RequireStaffPrincipal ->
// auth.RequirePermission(auth.PermSportsbookSettlementSimulate)) in
// sportsbook_routes.go, not here.
func newSimulateSettlementEventHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		// ADR 0088 §9.1: the handler parses tc.Subject as a UUID or fails
		// closed - a non-UUID staff subject must never silently become
		// uuid.Nil (which would, absurdly, mean "no actor").
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		betID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid bet id")
			return
		}

		var req simulateSettlementEventRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeSettlementValidationFailed, "invalid request body")
			return
		}
		eventType, msg, ok := validateSimulateSettlementEventShape(req)
		if !ok {
			apierror.Write(w, requestID, apierror.CodeSettlementValidationFailed, msg)
			return
		}
		payoutAmount, ok := parseClaimPayoutAmount(req.PayoutAmount)
		if !ok {
			apierror.Write(w, requestID, apierror.CodeSettlementValidationFailed,
				"payout_amount must be a non-negative JSON integer that fits in int64")
			return
		}

		ev := sportsbook.SettlementEvent{
			TenantID: tc.TenantID, BetID: betID, ActorStaffID: staffID, EventType: eventType,
			Generation: req.Generation, Outcome: req.Outcome, ClaimPayoutAmount: payoutAmount,
			ClaimAssetCode: req.AssetCode, VoidReason: req.VoidReason,
			RequestID: requestID,
			// ADR 0088 §10 (security review S6): IPAddress is the
			// trusted-proxy-aware client IP (never a raw, caller-controlled
			// X-Forwarded-For value); RemoteAddr is the raw connection
			// remote address, stored only in audit metadata, never copied
			// from X-Forwarded-For.
			IPAddress:  trustedProxyClientIP(r, deps.TrustedProxyCount),
			RemoteAddr: r.RemoteAddr,
		}

		var result sportsbook.SettlementResult
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = sportsbook.SimulateSettlementEvent(ctx, tx, ev)
			return err
		})

		if errors.Is(err, sportsbook.ErrSettlementActorNotActive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "settlement actor is not an active staff user of this tenant")
			return
		}
		if errors.Is(err, sportsbook.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeSettlementValidationFailed, "request rejected: invalid input")
			return
		}
		if errors.Is(err, sportsbook.ErrSettlementIntegrity) {
			// ADR 0088 §4.7: the operation's own transaction has already
			// rolled back (a stored fact contradicted the settlement
			// contract, possibly after a posting was made). The rejection
			// audit is written in a SEPARATE tenant-scoped transaction -
			// the failed one cannot carry it - then the integrity alert is
			// logged and 409 SETTLEMENT_INTEGRITY is returned.
			logSettlementIntegrityAlert(logger, alertReasonFromRejectionCode(sportsbook.SettlementRejectIntegrity), ev, "")
			auditErr := deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return sportsbook.RecordSettlementRejection(ctx, tx, ev, "", sportsbook.SettlementRejectIntegrity)
			})
			if auditErr != nil {
				logger.Error("sportsbook_settlement_rejection_audit_failed", "error", auditErr, "bet_id", betID.String())
			}
			apierror.Write(w, requestID, apierror.CodeSettlementIntegrity, "settlement integrity check failed")
			return
		}
		if err != nil {
			logger.Error("sportsbook_settlement_simulate_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process settlement event")
			return
		}

		if result.Rejected() {
			if result.Alert {
				logSettlementIntegrityAlert(logger, alertReasonFromRejectionCode(result.RejectionCode), ev, string(result.BetStatus))
			}
			message := "settlement event rejected: " + result.RejectionCode
			apierror.Write(w, requestID, settlementRejectionAPICode(result.RejectionCode), message)
			return
		}

		writeJSON(w, http.StatusOK, toSimulateSettlementEventResponse(result))
	}
}
