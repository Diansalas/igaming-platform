// Stage 4I Phase B: player self-service read/write of DECLARED residence
// (self-declared, unverified - HDR-J-3b). Mirrors newMeHandler's own
// self-access pattern exactly (auth.Middleware only, no RequirePermission -
// a player may always read/write their OWN declared residence; the
// activation boundary, not RBAC, is what gates whether the write is
// accepted at all).
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// errDeclaredResidenceCollectionInactive is returned from inside the
// WithTenant closure below when jurisdiction_evidence_collection_active is
// OFF for (tenant, declared_residence) - mapped to 403, distinct from any
// validation (400) or not-found (404) outcome.
var errDeclaredResidenceCollectionInactive = errors.New("httpserver: declared residence collection is not active for this tenant")

// myResidenceResponse never keys the underlying value ambiguously with
// presence: IsSet=false omits CountryCode/CapturedAt entirely, rather than
// emitting an empty-string country that could be confused with a genuine
// (impossible, but never assume) empty value.
type myResidenceResponse struct {
	IsSet       bool   `json:"is_set"`
	CountryCode string `json:"country_code,omitempty"`
	CapturedAt  string `json:"captured_at,omitempty"`
}

// newGetMyResidenceHandler is GET /v1/me/residence - a player reading
// their own declared residence. Never staff-accessible (this is deliberately
// NOT PermPlayerResidenceRead-gated; that permission gates STAFF reading
// ANOTHER player's value, never a player's own).
func newGetMyResidenceHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil || tc.PrincipalType != string(auth.PrincipalPlayer) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this endpoint is for player accounts only")
			return
		}
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var resp myResidenceResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			country, capturedAt, ok, err := identity.GetDeclaredResidence(ctx, tx, subjectID)
			if err != nil {
				return err
			}
			if ok {
				resp = myResidenceResponse{IsSet: true, CountryCode: country, CapturedAt: capturedAt.Format(rfc3339)}
			} else {
				resp = myResidenceResponse{IsSet: false}
			}
			return nil
		})
		if err != nil {
			logger.Error("get_my_residence_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load residence")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type setMyResidenceRequest struct {
	CountryCode string `json:"country_code"`
}

// newSetMyResidenceHandler is PUT /v1/me/residence - a player declaring
// their own residence. Refuses the write outright (403) when
// jurisdiction_evidence_collection_active is OFF for this tenant/
// declared_residence, checked INSIDE the same transaction as the write -
// Stage 4I Phase B's activation boundary is not optional and is never
// bypassed by a product flow.
func newSetMyResidenceHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil || tc.PrincipalType != string(auth.PrincipalPlayer) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this endpoint is for player accounts only")
			return
		}
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var body setMyResidenceRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireISO3166Alpha2("country_code", body.CountryCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		ip, ua := clientIP(r), r.UserAgent()
		var resp myResidenceResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			active, err := jurisdiction.IsEvidenceCollectionActive(ctx, tx, tc.TenantID, jurisdiction.EvidenceDeclaredResidence)
			if err != nil {
				return err
			}
			if !active {
				return errDeclaredResidenceCollectionInactive
			}
			if _, _, err := identity.SetPlayerAccountDeclaredResidence(ctx, tx, identity.SetDeclaredResidenceParams{
				PlayerAccountID: subjectID, TenantID: tc.TenantID, CountryCode: body.CountryCode,
				ActorType: audit.ActorPlayer, ActorID: subjectID,
				IPAddress: ip, UserAgent: ua, RequestID: requestID,
			}); err != nil {
				return err
			}
			country, capturedAt, ok, err := identity.GetDeclaredResidence(ctx, tx, subjectID)
			if err != nil {
				return err
			}
			if ok {
				resp = myResidenceResponse{IsSet: true, CountryCode: country, CapturedAt: capturedAt.Format(rfc3339)}
			} else {
				resp = myResidenceResponse{IsSet: false}
			}
			return nil
		})
		if errors.Is(err, errDeclaredResidenceCollectionInactive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "declared residence collection is not currently enabled for this account's tenant")
			return
		}
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "account not found")
			return
		}
		if err != nil {
			logger.Error("set_my_residence_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to set residence")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
