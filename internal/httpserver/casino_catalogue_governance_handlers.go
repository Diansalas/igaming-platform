// Stage 9.2, Workstream A: closes ARCH-DB-2 Phase 2 (docs/decisions/0081
// §5.2) - the admin API for filing and deciding a casino_games catalogue
// change request (jurisdiction_unblock / status_activate four-eyes,
// migration 0086).
//
// Both handlers run under deps.DB.WithPlatformAdmin, gated by the new
// platform-only PermCasinoCatalogueGovern permission, and never accept the
// acting principal from anything but the verified token's own subject -
// mirrors newUpsertCasinoGameHandler's and asset_registry_handlers.go's
// identical conventions exactly.
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// writeCasinoGovernanceError maps this package's sentinels onto HTTP
// codes, mirroring writeAssetRegistryError's identical rationale: a
// four-eyes refusal or a self-approval attempt is never reported as a
// generic 500.
func writeCasinoGovernanceError(w http.ResponseWriter, requestID string, logger interface {
	Error(msg string, args ...any)
}, action string, err error) {
	switch {
	case errors.Is(err, casino.ErrInvalidInput):
		apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
	case errors.Is(err, casino.ErrGameNotFound), errors.Is(err, casino.ErrChangeRequestNotFound):
		apierror.Write(w, requestID, apierror.CodeNotFound, err.Error())
	case errors.Is(err, casino.ErrDualControlRequired),
		errors.Is(err, casino.ErrSelfApproval),
		errors.Is(err, casino.ErrDuplicateDecision),
		errors.Is(err, casino.ErrChangeRequestNotPending):
		apierror.Write(w, requestID, apierror.CodeConflict, err.Error())
	case errors.Is(err, casino.ErrPrincipalNotEligible):
		apierror.Write(w, requestID, apierror.CodeForbidden, err.Error())
	case errors.Is(err, casino.ErrTransactionScope):
		apierror.Write(w, requestID, apierror.CodeInternal, err.Error())
	default:
		logger.Error(action+"_failed", "error", err)
		apierror.Write(w, requestID, apierror.CodeInternal, "casino catalogue governance operation failed")
	}
}

type fileCasinoCatalogueChangeRequestBody struct {
	Operation string `json:"operation"`
	// RemovedCodes is required (and each code must currently be blocked)
	// only for operation = jurisdiction_unblock.
	RemovedCodes []string `json:"removed_codes,omitempty"`
	ReasonCode   string   `json:"reason_code"`
}

// newFileCasinoCatalogueChangeRequestHandler handles
// POST /v1/admin/casino/games/{gameID}/change-requests.
func newFileCasinoCatalogueChangeRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		gameID, err := uuid.Parse(r.PathValue("gameID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid game id")
			return
		}

		var body fileCasinoCatalogueChangeRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		ops := casino.ChangeOperations()
		allowedOps := make([]string, 0, len(ops))
		for _, op := range ops {
			allowedOps = append(allowedOps, string(op))
		}
		v.RequireOneOf("operation", body.Operation, allowedOps...)
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if casino.ChangeOperation(body.Operation) == casino.ChangeJurisdictionUnblock && len(body.RemovedCodes) == 0 {
			v.Add("removed_codes", "is required and must be non-empty for operation=jurisdiction_unblock")
		} else if casino.ChangeOperation(body.Operation) != casino.ChangeJurisdictionUnblock && len(body.RemovedCodes) > 0 {
			// Refused rather than ignored - the same "never silently drop a
			// field an approver may read" discipline
			// newFileAssetChangeRequestHandler applies to
			// eligibility_operation/eligibility_product.
			v.Add("removed_codes", "is only valid for operation=jurisdiction_unblock")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		// Mirrors newUpsertCasinoGameHandler's explicit parse-error surface
		// (migration 0084/ADR 0081 §7.1): a swallowed parse failure would
		// silently become uuid.Nil and get rejected deep inside
		// WithPlatformAdmin with an opaque error.
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var req casino.ChangeRequest
		err = deps.DB.WithPlatformAdmin(r.Context(), subjectID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			req, err = casino.FileChangeRequest(ctx, tx, casino.FileChangeRequestParams{
				Operation: casino.ChangeOperation(body.Operation), GameID: gameID,
				RemovedCodes: body.RemovedCodes, ReasonCode: body.ReasonCode,
				RequestedByPrincipalID: subjectID,
			})
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "casino_catalogue_change_request.filed", TargetType: "casino_catalogue_change_request", TargetID: req.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{
					"game_id": gameID.String(), "operation": string(req.Operation),
					"payload": req.Payload, "reason_code": req.ReasonCode,
				},
			})
		})
		if err != nil {
			writeCasinoGovernanceError(w, requestID, logger, "file_casino_catalogue_change_request", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": req.ID.String(), "operation": string(req.Operation), "game_id": req.GameID.String(),
			"state": req.State, "requested_at": req.RequestedAt.Format(rfc3339),
		})
	}
}

type decideCasinoCatalogueChangeRequestBody struct {
	Decision   string `json:"decision"`
	ReasonCode string `json:"reason_code"`
}

// newDecideCasinoCatalogueChangeRequestHandler handles
// POST /v1/admin/casino/change-requests/{requestID}/approvals.
func newDecideCasinoCatalogueChangeRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		reqID, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid change request id")
			return
		}
		var body decideCasinoCatalogueChangeRequestBody
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("decision", body.Decision, "approve", "reject")
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var ap casino.ChangeApproval
		err = deps.DB.WithPlatformAdmin(r.Context(), subjectID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ap, err = casino.DecideChangeRequest(ctx, tx, casino.DecideChangeRequestParams{
				RequestID: reqID, Approve: body.Decision == "approve",
				ReasonCode: body.ReasonCode, ApproverPrincipalID: subjectID,
			})
			if err != nil {
				return err
			}
			action := "casino_catalogue_change_request.rejected"
			if body.Decision == "approve" {
				action = "casino_catalogue_change_request.approved"
			}
			return audit.Record(ctx, tx, audit.Entry{
				ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: action, TargetType: "casino_catalogue_change_request", TargetID: reqID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"decision": ap.Decision, "approval_id": ap.ID.String()},
			})
		})
		if err != nil {
			writeCasinoGovernanceError(w, requestID, logger, "decide_casino_catalogue_change_request", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": ap.ID.String(), "request_id": ap.RequestID.String(),
			"decision": ap.Decision, "decided_at": ap.DecidedAt.Format(rfc3339),
		})
	}
}
