// B13 (ADR 0111 2.9): the payout instrument API.
//
// Player (RequirePlayerPrincipal; the player is ALWAYS the authenticated
// subject, never a body field):
//
//	POST /v1/me/payout-instruments            register (kind, rail, asset_codes, detail)
//	GET  /v1/me/payout-instruments            list (masks only)
//	POST /v1/me/payout-instruments/{id}/revoke
//
// Staff (tenant-scoped; the tenant comes from the verified token only):
//
//	GET  /v1/admin/players/{playerID}/payout-instruments   payout_instrument:read
//	POST /v1/admin/payout-instruments/{id}/suspend          payout_instrument:suspend
//
// There is NO staff create, verify, unsuspend or edit route (owner decision
// 7). No response ever carries the detail, the ciphertext, the fingerprint or a
// seal - the only detail-derived value is display_mask. Request bodies of the
// registration route are never logged: nothing in this file logs a body, an
// error from decoding a body, or a service error that could carry one (only
// closed reason tokens are logged).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Registration rate limit (ADR 0111 2.9, ADR 0097 admission pattern): per
// player, burst 5, refilling one registration attempt per 6 minutes (10 an
// hour). Refused attempts consume budget too.
const (
	payoutInstrumentRegisterRate  = 10.0 / 3600.0
	payoutInstrumentRegisterBurst = 5
)

// NewPayoutInstrumentRegisterLimiter builds the production registration
// limiter (tests inject a FakeClock one through Deps).
func NewPayoutInstrumentRegisterLimiter(clock admission.Clock) *admission.GCRALimiter {
	if clock == nil {
		clock = admission.RealClock()
	}
	return admission.NewGCRALimiter(payoutInstrumentRegisterRate, payoutInstrumentRegisterBurst, 100000, time.Hour, "overflow", clock)
}

func registerPayoutInstrumentRoutes(mux *http.ServeMux, deps Deps) {
	limiter := deps.PayoutInstrumentRegisterLimiter
	if limiter == nil {
		limiter = NewPayoutInstrumentRegisterLimiter(nil)
	}
	player := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(h))
	}
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	mux.Handle("POST /v1/me/payout-instruments", player(newRegisterPayoutInstrumentHandler(deps, limiter)))
	mux.Handle("GET /v1/me/payout-instruments", player(newListPayoutInstrumentsHandler(deps)))
	mux.Handle("POST /v1/me/payout-instruments/{id}/revoke", player(newRevokePayoutInstrumentHandler(deps)))
	mux.Handle("GET /v1/admin/players/{playerID}/payout-instruments",
		staff(auth.RequirePermission(auth.PermPayoutInstrumentRead)(newStaffListPayoutInstrumentsHandler(deps))))
	mux.Handle("POST /v1/admin/payout-instruments/{id}/suspend",
		staff(auth.RequirePermission(auth.PermPayoutInstrumentSuspend)(newSuspendPayoutInstrumentHandler(deps))))
}

type registerPayoutInstrumentRequest struct {
	Kind                   string          `json:"kind"`
	Rail                   string          `json:"rail"`
	AssetCodes             []string        `json:"asset_codes"`
	Detail                 json.RawMessage `json:"detail"`
	SupersedesInstrumentID *uuid.UUID      `json:"supersedes_instrument_id,omitempty"`
}

const payoutInstrumentNotAcceptedMsg = "payout instrument could not be accepted"

// refusalReason maps a registration error to the CLOSED audit token. The
// player never sees it.
func refusalReason(err error) string {
	switch {
	case errors.Is(err, payoutinstrument.ErrFingerprintConflict):
		return "fingerprint_conflict"
	case errors.Is(err, payoutinstrument.ErrDestinationBlocked):
		return "destination_blocked"
	case errors.Is(err, payoutinstrument.ErrPANRefused):
		return "pan_refused"
	case errors.Is(err, payoutinstrument.ErrInvalidDetail):
		return "invalid_detail"
	case errors.Is(err, payoutinstrument.ErrUnknownKind):
		return "unknown_kind"
	case errors.Is(err, payoutinstrument.ErrInvalidRegistration):
		return "invalid_registration"
	}
	return ""
}

func payoutService(deps Deps, w http.ResponseWriter, requestID string) (*payoutinstrument.Service, bool) {
	if deps.PayoutInstruments == nil {
		apierror.Write(w, requestID, apierror.CodeUnavailable, "payout instruments are not available")
		return nil, false
	}
	return deps.PayoutInstruments, true
}

func newRegisterPayoutInstrumentHandler(deps Deps, limiter *admission.GCRALimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}
		svc, ok := payoutService(deps, w, requestID)
		if !ok {
			return
		}
		// Per-player admission before any work (and before the body is read
		// further): refused attempts consume budget.
		if admitted, retry := limiter.Allow(tc.TenantID.String() + ":" + playerID.String()); !admitted {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
			apierror.Write(w, requestID, apierror.CodeRateLimited, "too many payout instrument registrations, retry later")
			return
		}
		var req registerPayoutInstrumentRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.Kind == "" || req.Rail == "" || len(req.AssetCodes) == 0 || len(req.Detail) == 0 {
			apierror.Write(w, requestID, apierror.CodeValidation, "kind, rail, asset_codes and detail are required")
			return
		}

		var res payoutinstrument.RegisterResult
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var rerr error
			res, rerr = svc.Register(ctx, tx, payoutinstrument.RegisterParams{
				TenantID: tc.TenantID, PlayerAccountID: playerID, Kind: req.Kind, Rail: req.Rail,
				AssetCodes: req.AssetCodes, Detail: req.Detail, Supersedes: req.SupersedesInstrumentID,
			})
			return rerr
		})
		if err != nil {
			reason := refusalReason(err)
			if reason == "" {
				logger.Error("payout_instrument_register_failed", "error_class", "internal")
				apierror.Write(w, requestID, apierror.CodeInternal, "failed to register payout instrument")
				return
			}
			// The specific reason goes to the audit row only (a fingerprint
			// conflict is the compliance-visible case); the player gets the
			// generic response in every refusal case.
			action := "payout_instrument.registration_refused"
			if reason == "fingerprint_conflict" {
				action = "payout_instrument.registration_conflict"
			}
			if aerr := deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tc.TenantID, ActorType: audit.ActorPlayer, ActorID: playerID, Action: action,
					TargetType: "payout_instrument", Outcome: audit.OutcomeDenied, IPAddress: clientIP(r),
					UserAgent: r.UserAgent(), RequestID: requestID,
					Metadata: map[string]any{"reason": reason},
				})
			}); aerr != nil {
				logger.Error("payout_instrument_refusal_audit_failed", "error_class", "internal")
			}
			apierror.Write(w, requestID, apierror.CodePayoutInstrumentNotAccepted, payoutInstrumentNotAcceptedMsg)
			return
		}

		// Start verification (synchronous, best effort): the instrument stays
		// pending_verification when no verifier is available or a precondition
		// (e.g. KYC) is not met.
		status := http.StatusCreated
		if res.Existing {
			status = http.StatusOK
		} else if _, verr := svc.Verify(r.Context(), deps.DB, tc.TenantID, res.Instrument.ID); verr != nil {
			logger.Info("payout_instrument_verification_not_completed", "instrument_id", res.Instrument.ID.String(), "reason", verifyFailureToken(verr))
		}
		var views []payoutinstrument.View
		_ = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerID, func(ctx context.Context, tx pgx.Tx) error {
			var lerr error
			views, lerr = svc.ListForPlayer(ctx, tx, tc.TenantID, playerID, false)
			return lerr
		})
		for _, v := range views {
			if v.ID == res.Instrument.ID {
				writeJSON(w, status, v)
				return
			}
		}
		writeJSON(w, status, payoutinstrument.ViewOf(res.Instrument))
	}
}

// verifyFailureToken is a closed token for logs.
func verifyFailureToken(err error) string {
	switch {
	case errors.Is(err, payoutinstrument.ErrNoVerifier):
		return "no_verifier"
	case errors.Is(err, payoutinstrument.ErrKYCNotVerified):
		return "kyc_not_verified"
	case errors.Is(err, payoutinstrument.ErrMaxAgeNotConfigured):
		return "max_age_not_configured"
	case errors.Is(err, payoutinstrument.ErrKindDisabled):
		return "kind_disabled"
	case errors.Is(err, payoutinstrument.ErrIntegrity):
		return "integrity"
	case errors.Is(err, payoutinstrument.ErrNotVerifiable):
		return "not_verifiable"
	}
	return "verifier_error"
}

func newListPayoutInstrumentsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}
		svc, ok := payoutService(deps, w, requestID)
		if !ok {
			return
		}
		var views []payoutinstrument.View
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerID, func(ctx context.Context, tx pgx.Tx) error {
			var lerr error
			views, lerr = svc.ListForPlayer(ctx, tx, tc.TenantID, playerID, false)
			return lerr
		})
		if err != nil {
			logger.Error("payout_instrument_list_failed", "error_class", "internal")
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list payout instruments")
			return
		}
		writeJSON(w, http.StatusOK, views)
	}
}

type revokePayoutInstrumentRequest struct {
	ReasonCode string `json:"reason_code,omitempty"`
}

func newRevokePayoutInstrumentHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid payout instrument id")
			return
		}
		svc, ok := payoutService(deps, w, requestID)
		if !ok {
			return
		}
		var req revokePayoutInstrumentRequest
		if r.ContentLength != 0 {
			if err := decodeJSON(r, &req); err != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
				return
			}
		}
		reason := "player_revoked"
		if req.ReasonCode != "" {
			reason = req.ReasonCode
		}
		var inst payoutinstrument.Instrument
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var rerr error
			inst, rerr = svc.Revoke(ctx, tx, payoutinstrument.BlockParams{
				TenantID: tc.TenantID, InstrumentID: id, PlayerAccountID: playerID,
				Actor:      payoutinstrument.Actor{Type: payoutinstrument.ActorPlayer, ID: playerID.String()},
				ReasonCode: reason, IP: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return rerr
		})
		switch {
		case errors.Is(err, payoutinstrument.ErrNotFound):
			apierror.Write(w, requestID, apierror.CodeNotFound, "payout instrument not found")
		case errors.Is(err, payoutinstrument.ErrInUse):
			apierror.Write(w, requestID, apierror.CodePayoutInstrumentInUse, "payout instrument is used by a withdrawal in progress")
		case errors.Is(err, payoutinstrument.ErrInvalidRegistration):
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid reason code")
		case errors.Is(err, payoutinstrument.ErrBadTransition):
			apierror.Write(w, requestID, apierror.CodeConflict, "payout instrument can no longer be changed")
		case err != nil:
			logger.Error("payout_instrument_revoke_failed", "error_class", "internal")
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to revoke payout instrument")
		default:
			writeJSON(w, http.StatusOK, payoutinstrument.ViewOf(inst))
		}
	}
}

// staffTenant resolves the tenant staff caller. A platform caller (no tenant
// in the token) has no reach: these routes are tenant-scoped and a platform
// session has no tenant context to read instruments in.
func staffTenant(w http.ResponseWriter, r *http.Request, requestID string) (tenant.Context, uuid.UUID, bool) {
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
		return tc, uuid.Nil, false
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
		return tc, uuid.Nil, false
	}
	if tc.TenantID == uuid.Nil {
		apierror.Write(w, requestID, apierror.CodeForbidden, "a tenant-scoped session is required")
		return tc, uuid.Nil, false
	}
	return tc, subject, true
}

func newStaffListPayoutInstrumentsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, _, ok := staffTenant(w, r, requestID)
		if !ok {
			return
		}
		playerID, err := uuid.Parse(r.PathValue("playerID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid player id")
			return
		}
		svc, ok := payoutService(deps, w, requestID)
		if !ok {
			return
		}
		var views []payoutinstrument.View
		err = deps.DB.WithTenantReadOnly(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var lerr error
			views, lerr = svc.ListForPlayer(ctx, tx, tc.TenantID, playerID, true)
			return lerr
		})
		if err != nil {
			logger.Error("payout_instrument_staff_list_failed", "error_class", "internal")
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list payout instruments")
			return
		}
		writeJSON(w, http.StatusOK, views)
	}
}

type suspendPayoutInstrumentRequest struct {
	ReasonCode string `json:"reason_code"`
}

func newSuspendPayoutInstrumentHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, subject, ok := staffTenant(w, r, requestID)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid payout instrument id")
			return
		}
		var req suspendPayoutInstrumentRequest
		if err := decodeJSON(r, &req); err != nil || req.ReasonCode == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "reason_code is required")
			return
		}
		svc, ok := payoutService(deps, w, requestID)
		if !ok {
			return
		}
		var inst payoutinstrument.Instrument
		err = deps.DB.WithPrincipalScope(r.Context(), tc.TenantID, subject, func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			inst, serr = svc.Suspend(ctx, tx, payoutinstrument.BlockParams{
				TenantID: tc.TenantID, InstrumentID: id,
				Actor:      payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: subject.String()},
				ReasonCode: req.ReasonCode, IP: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return serr
		})
		switch {
		case errors.Is(err, payoutinstrument.ErrNotFound):
			apierror.Write(w, requestID, apierror.CodeNotFound, "payout instrument not found")
		case errors.Is(err, payoutinstrument.ErrInvalidRegistration):
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid reason code")
		case errors.Is(err, payoutinstrument.ErrBadTransition):
			apierror.Write(w, requestID, apierror.CodeConflict, "payout instrument cannot be suspended in its current state")
		case err != nil:
			logger.Error("payout_instrument_suspend_failed", "error_class", "internal")
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to suspend payout instrument")
		default:
			writeJSON(w, http.StatusOK, payoutinstrument.ViewOf(inst))
		}
	}
}
