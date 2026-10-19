package httpserver

// Licensing slice 1 (docs/plans/lic1-record.md): the licence lifecycle write API. Platform-admin only
// (PermJurisdictionRegistryManage, no RequireTenantScope), step-up critical on every write, reason code required,
// audited with before/after in the same transaction (jurisdiction.SetLicence*). The licence id comes from the path;
// no tenant id is read from anywhere. NO maker-checker in this slice: widening changes are single-actor + step-up
// (residual G-1/G-2, owned by slice 2); the response carries the change_kind so a client can see which class applied.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

type licenceChangeResponse struct {
	Licence    licenceResponse `json:"licence"`
	ChangeKind string          `json:"change_kind"`
}

type licenceVersionResponse struct {
	VersionNo         int       `json:"version_no"`
	Operation         string    `json:"operation"`
	ChangedAt         string    `json:"changed_at"`
	ChangedBy         *staffRef `json:"changed_by"`
	Status            string    `json:"status"`
	IssuedAt          *string   `json:"issued_at"`
	ExpiresAt         *string   `json:"expires_at"`
	ReviewDueAt       *string   `json:"review_due_at"`
	EvidenceReference *string   `json:"licence_evidence_reference"`
	LicenceNumber     string    `json:"licence_number"`
}

// parseRequiredNullableDate reads a required JSON key that is a "YYYY-MM-DD" string or null.
func parseRequiredNullableDate(raw json.RawMessage) (*time.Time, bool) {
	if raw == nil {
		return nil, false
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// licenceLifecycleHandler factors the shared shape: path id, body decode, reason code, platform tx, response.
func licenceLifecycleHandler[B any](deps Deps, action string, reason func(*B) string, validate func(*B, *validation.Errors),
	apply func(ctx context.Context, tx pgx.Tx, id uuid.UUID, b *B, actor jurisdiction.ActorContext) (jurisdiction.LicenceChange, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		id, err := uuid.Parse(r.PathValue("licenceID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "licenceID must be a valid UUID")
			return
		}
		var body B
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason_code", reason(&body))
		validate(&body, v)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := jurisdictionActorFromRequest(r, reason(&body))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var res jurisdiction.LicenceChange
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = apply(ctx, tx, id, &body, actor)
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, action, err)
			return
		}
		writeJSON(w, http.StatusOK, licenceChangeResponse{Licence: toLicenceResponse(res.Licence), ChangeKind: string(res.Kind)})
	}
}

type setLicenceStatusRequest struct {
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code"`
}

// newSetLicenceStatusHandler is PUT /v1/admin/licences/{licenceID}/status.
func newSetLicenceStatusHandler(deps Deps) http.HandlerFunc {
	return licenceLifecycleHandler(deps, "set_licence_status",
		func(b *setLicenceStatusRequest) string { return b.ReasonCode },
		func(b *setLicenceStatusRequest, v *validation.Errors) {
			v.RequireOneOf("status", b.Status, "active", "suspended", "expired")
		},
		func(ctx context.Context, tx pgx.Tx, id uuid.UUID, b *setLicenceStatusRequest, a jurisdiction.ActorContext) (jurisdiction.LicenceChange, error) {
			return jurisdiction.SetLicenceStatus(ctx, tx, id, b.Status, a)
		})
}

type setLicenceValidityDatesRequest struct {
	// Both keys are REQUIRED (a JSON null clears): omitting one is almost certainly a mistake.
	IssuedAt   json.RawMessage `json:"issued_at"`
	ExpiresAt  json.RawMessage `json:"expires_at"`
	ReasonCode string          `json:"reason_code"`
}

// newSetLicenceValidityDatesHandler is PUT /v1/admin/licences/{licenceID}/validity-dates.
func newSetLicenceValidityDatesHandler(deps Deps) http.HandlerFunc {
	return licenceLifecycleHandler(deps, "set_licence_validity_dates",
		func(b *setLicenceValidityDatesRequest) string { return b.ReasonCode },
		func(b *setLicenceValidityDatesRequest, v *validation.Errors) {
			if _, ok := parseRequiredNullableDate(b.IssuedAt); !ok {
				v.Add("issued_at", "is required: a YYYY-MM-DD date or null")
			}
			if _, ok := parseRequiredNullableDate(b.ExpiresAt); !ok {
				v.Add("expires_at", "is required: a YYYY-MM-DD date or null")
			}
		},
		func(ctx context.Context, tx pgx.Tx, id uuid.UUID, b *setLicenceValidityDatesRequest, a jurisdiction.ActorContext) (jurisdiction.LicenceChange, error) {
			issued, _ := parseRequiredNullableDate(b.IssuedAt)
			expires, _ := parseRequiredNullableDate(b.ExpiresAt)
			return jurisdiction.SetLicenceValidityDates(ctx, tx, id, issued, expires, a)
		})
}

type setLicenceReviewDueRequest struct {
	ReviewDueAt json.RawMessage `json:"review_due_at"`
	ReasonCode  string          `json:"reason_code"`
}

// newSetLicenceReviewDueHandler is PUT /v1/admin/licences/{licenceID}/review-due.
func newSetLicenceReviewDueHandler(deps Deps) http.HandlerFunc {
	return licenceLifecycleHandler(deps, "set_licence_review_due",
		func(b *setLicenceReviewDueRequest) string { return b.ReasonCode },
		func(b *setLicenceReviewDueRequest, v *validation.Errors) {
			if _, ok := parseRequiredNullableDate(b.ReviewDueAt); !ok {
				v.Add("review_due_at", "is required: a YYYY-MM-DD date or null")
			}
		},
		func(ctx context.Context, tx pgx.Tx, id uuid.UUID, b *setLicenceReviewDueRequest, a jurisdiction.ActorContext) (jurisdiction.LicenceChange, error) {
			d, _ := parseRequiredNullableDate(b.ReviewDueAt)
			return jurisdiction.SetLicenceReviewDue(ctx, tx, id, d, a)
		})
}

type setLicenceEvidenceReferenceRequest struct {
	EvidenceReference json.RawMessage `json:"licence_evidence_reference"`
	ReasonCode        string          `json:"reason_code"`
}

// newSetLicenceEvidenceReferenceHandler is PUT /v1/admin/licences/{licenceID}/evidence-reference.
func newSetLicenceEvidenceReferenceHandler(deps Deps) http.HandlerFunc {
	parse := func(raw json.RawMessage) (*string, bool) {
		if raw == nil {
			return nil, false
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, true
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false
		}
		return &s, true
	}
	return licenceLifecycleHandler(deps, "set_licence_evidence_reference",
		func(b *setLicenceEvidenceReferenceRequest) string { return b.ReasonCode },
		func(b *setLicenceEvidenceReferenceRequest, v *validation.Errors) {
			if _, ok := parse(b.EvidenceReference); !ok {
				v.Add("licence_evidence_reference", "is required: a string or null")
			}
		},
		func(ctx context.Context, tx pgx.Tx, id uuid.UUID, b *setLicenceEvidenceReferenceRequest, a jurisdiction.ActorContext) (jurisdiction.LicenceChange, error) {
			ref, _ := parse(b.EvidenceReference)
			return jurisdiction.SetLicenceEvidenceReference(ctx, tx, id, ref, a)
		})
}

// newListLicenceVersionsHandler is GET /v1/admin/licences/{licenceID}/versions.
func newListLicenceVersionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		id, err := uuid.Parse(r.PathValue("licenceID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "licenceID must be a valid UUID")
			return
		}
		actor, err := jurisdictionActorFromRequest(r, "list")
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var list []jurisdiction.LicenceVersion
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			list, err = jurisdiction.ListLicenceVersions(ctx, tx, id)
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "list_licence_versions", err)
			return
		}
		resp := make([]licenceVersionResponse, 0, len(list))
		for _, v := range list {
			item := licenceVersionResponse{VersionNo: v.VersionNo, Operation: v.Operation, ChangedAt: v.ChangedAt.Format(rfc3339),
				Status: v.Status, IssuedAt: dateStringPtr(v.IssuedAt), ExpiresAt: dateStringPtr(v.ExpiresAt),
				ReviewDueAt: dateStringPtr(v.ReviewDueAt), EvidenceReference: evidencePtr(v.EvidenceReference), LicenceNumber: v.LicenceNumber}
			platform := staffScopePlatform
			item.ChangedBy = platformStaffViewer().optRef(&platform, v.ChangedBy)
			resp = append(resp, item)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
