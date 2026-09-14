package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// Stage 3D directive item 4: docs/architecture/withdrawal-policy-
// configuration.md §5's open decision #4 ("no admin API exists yet to
// write withdrawal_policies rows") is what this file closes - the
// MINIMAL safe boundary the directive asks for, not a Back Office UI or
// Partner Console. Gated by PermWithdrawalPolicyWrite, held only by
// RoleTenantAdmin (internal/auth/permission.go) - deliberately NOT
// RoleFinance, so the role that approves withdrawals can never also
// loosen the policy gating its own approvals (§5's open decision #3).
//
// Deliberately insert-only: a POST always creates a NEW row, never
// UPDATEs an existing one. withdrawal_policies rows are a versioned
// history of policy-in-force-over-time (policy_version/effective_from),
// exactly like every other financial configuration this codebase treats
// as append-only rather than mutated in place - and
// threshold_amount_at_decision already snapshots the policy onto each
// withdrawal_approvals row at decision time, so deleting or superseding a
// withdrawal_policies row can never rewrite what a past decision actually
// applied.
//
// jurisdiction_code and required_approver_roles are NEVER accepted from
// the request body - migration 0033's own CHECK constraints
// (withdrawal_policies_jurisdiction_not_yet_resolvable,
// withdrawal_policies_approver_roles_not_yet_enforced) require both NULL
// today, since neither is actually resolved/enforced yet
// (docs/architecture/withdrawal-policy-configuration.md §5.2/§6) - this
// API must not let an admin write a value that would silently never take
// effect (the exact "false sense of enforcement" specialist review
// flagged those columns for).

type withdrawalPolicyResponse struct {
	ID                          string `json:"id"`
	TenantID                    string `json:"tenant_id"`
	BrandID                     string `json:"brand_id,omitempty"`
	AssetCode                   string `json:"asset_code"`
	ApprovalThresholdMinorUnits int64  `json:"approval_threshold_minor_units"`
	RequiredApprovals           int    `json:"required_approvals"`
	RequireStepUp               bool   `json:"require_step_up"`
	PolicyVersion               int    `json:"policy_version"`
	EffectiveFrom               string `json:"effective_from"`
	CreatedAt                   string `json:"created_at"`
}

// newListWithdrawalPoliciesHandler lists every withdrawal_policies row for
// the caller's own tenant - tenant isolation is enforced by RLS
// (migration 0033's tenant_isolation policy) on top of RequireTenantScope
// excluding platform_admin's nil-tenant token, exactly like every other
// tenant-scoped admin read in this codebase.
func newListWithdrawalPoliciesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var resp []withdrawalPolicyResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx,
				`SELECT id, tenant_id, brand_id, asset_code, approval_threshold_minor_units, required_approvals,
					require_step_up, policy_version, effective_from, created_at
				 FROM withdrawal_policies
				 ORDER BY asset_code ASC, effective_from DESC, policy_version DESC, created_at DESC`,
			)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var (
					id, tid                  uuid.UUID
					brandID                  *uuid.UUID
					assetCode                string
					threshold                int64
					requiredApprovals        int16
					requireStepUp            bool
					policyVersion            int32
					effectiveFrom, createdAt time.Time
				)
				if err := rows.Scan(&id, &tid, &brandID, &assetCode, &threshold, &requiredApprovals,
					&requireStepUp, &policyVersion, &effectiveFrom, &createdAt); err != nil {
					return err
				}
				item := withdrawalPolicyResponse{
					ID: id.String(), TenantID: tid.String(), AssetCode: assetCode,
					ApprovalThresholdMinorUnits: threshold, RequiredApprovals: int(requiredApprovals),
					RequireStepUp: requireStepUp, PolicyVersion: int(policyVersion),
					EffectiveFrom: effectiveFrom.UTC().Format(rfc3339), CreatedAt: createdAt.UTC().Format(rfc3339),
				}
				if brandID != nil {
					item.BrandID = brandID.String()
				}
				resp = append(resp, item)
			}
			return rows.Err()
		})
		if err != nil {
			logger.Error("list_withdrawal_policies_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list withdrawal policies")
			return
		}
		if resp == nil {
			resp = []withdrawalPolicyResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type writeWithdrawalPolicyRequest struct {
	// BrandID is optional - omitted/empty means the policy applies to
	// every brand under the caller's tenant (a NULL brand_id row).
	BrandID                     string `json:"brand_id,omitempty"`
	AssetCode                   string `json:"asset_code"`
	ApprovalThresholdMinorUnits int64  `json:"approval_threshold_minor_units"`
	RequiredApprovals           int    `json:"required_approvals"`
	RequireStepUp               bool   `json:"require_step_up"`
	// PolicyVersion is optional; defaults to 1. Only meaningful as a
	// tiebreaker among rows sharing the same effective_from - see
	// ResolveApprovalPolicy's own doc comment.
	PolicyVersion int `json:"policy_version,omitempty"`
	// EffectiveFrom is optional RFC3339; defaults to now(). Accepting a
	// future timestamp is deliberate - it is how a policy change is
	// scheduled ahead of time without an immediate cutover.
	EffectiveFrom string `json:"effective_from,omitempty"`
}

// newWriteWithdrawalPolicyHandler creates a new withdrawal_policies row.
// Never updates or deletes an existing one - see this file's package
// doc comment for why.
//
// tenant_id is always the caller's own authenticated tenant (from
// RequireTenantScope, never from the request body); brand_id, if
// supplied, is validated by the database's own composite
// (brand_id, tenant_id) -> brands(id, tenant_id) foreign key (migration
// 0033) - a brand_id naming a real brand under a DIFFERENT tenant fails
// exactly like an unknown brand_id, closing the "valid policy ownership"
// requirement (directive item 4) without a separate cross-tenant lookup
// query. Likewise asset_code is validated by its own FK into the assets
// registry (migration 0032).
func newWriteWithdrawalPolicyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var req writeWithdrawalPolicyRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}

		v := validation.New()
		v.RequireNonEmpty("asset_code", req.AssetCode)
		if req.ApprovalThresholdMinorUnits < 0 {
			v.Add("approval_threshold_minor_units", "must not be negative")
		}
		if req.RequiredApprovals < 1 {
			v.Add("required_approvals", "must be at least 1")
		}
		if req.PolicyVersion < 0 {
			v.Add("policy_version", "must not be negative")
		}
		var brandID *uuid.UUID
		if req.BrandID != "" {
			parsed, err := uuid.Parse(req.BrandID)
			if err != nil {
				v.Add("brand_id", "must be a valid UUID")
			} else {
				brandID = &parsed
			}
		}
		now := time.Now().UTC()
		effectiveFrom := now
		if req.EffectiveFrom != "" {
			parsed, err := time.Parse(time.RFC3339, req.EffectiveFrom)
			if err != nil {
				v.Add("effective_from", "must be a valid RFC3339 timestamp")
			} else if parsed.Before(now) {
				// Stage 3D specialist review (ledger-finance): a
				// backdated effective_from lets an admin assert, after
				// the fact, that a more permissive (or more restrictive)
				// policy was in force at some past instant - undermining
				// the "insert-only history" this table is documented to
				// be. Past withdrawal_approvals decisions are unaffected
				// either way (threshold_amount_at_decision is a snapshot,
				// never re-derived from this table), but a policy's OWN
				// history must only ever be extended forward, never
				// rewritten backward.
				v.Add("effective_from", "must not be in the past")
			} else {
				effectiveFrom = parsed
			}
		}
		policyVersion := req.PolicyVersion
		if policyVersion == 0 {
			policyVersion = 1
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		id := uuid.New()
		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			// jurisdiction_code and required_approver_roles are always
			// NULL - see this file's package doc comment.
			_, err := tx.Exec(ctx,
				`INSERT INTO withdrawal_policies
					(id, tenant_id, brand_id, asset_code, approval_threshold_minor_units, required_approvals,
					 require_step_up, policy_version, effective_from)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				id, tc.TenantID, brandID, req.AssetCode, req.ApprovalThresholdMinorUnits, req.RequiredApprovals,
				req.RequireStepUp, policyVersion, effectiveFrom,
			)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "withdrawal_policy.created", TargetType: "withdrawal_policy", TargetID: id.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{
					"asset_code": req.AssetCode, "brand_id": req.BrandID,
					"approval_threshold_minor_units": req.ApprovalThresholdMinorUnits,
					"required_approvals":             req.RequiredApprovals,
					"require_step_up":                req.RequireStepUp,
					"policy_version":                 policyVersion,
					"effective_from":                 effectiveFrom.Format(rfc3339),
				},
			})
		})
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "asset_code or brand_id does not reference a valid record for this tenant")
			return
		}
		if db.IsCheckViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "policy violates a database constraint (e.g. jurisdiction/approver-role fields are not yet supported)")
			return
		}
		if err != nil {
			logger.Error("write_withdrawal_policy_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create withdrawal policy")
			return
		}

		resp := withdrawalPolicyResponse{
			ID: id.String(), TenantID: tc.TenantID.String(), AssetCode: req.AssetCode,
			ApprovalThresholdMinorUnits: req.ApprovalThresholdMinorUnits, RequiredApprovals: req.RequiredApprovals,
			RequireStepUp: req.RequireStepUp, PolicyVersion: policyVersion,
			EffectiveFrom: effectiveFrom.UTC().Format(rfc3339),
		}
		if brandID != nil {
			resp.BrandID = brandID.String()
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// newDeleteWithdrawalPolicyHandler removes a misconfigured
// withdrawal_policies row (e.g. wrong asset/threshold entered by mistake).
// This is safe precisely because the insert-only design above never lets
// a past DECISION depend on this row still existing: Approve/Reject
// snapshot threshold_amount_at_decision onto the withdrawal_approvals row
// itself at decision time (policy.go's own doc comment), so deleting a
// withdrawal_policies row can only change what a FUTURE decision resolves
// to, never rewrite what a past one actually applied. Tenant isolation is
// enforced the same way as every other tenant-scoped write here: RLS
// scopes the DELETE to the caller's own tenant, so a row id belonging to
// a different tenant simply matches zero rows rather than ever being
// visible to delete.
//
// Requires a ?reason_code= query parameter (a DELETE conventionally
// carries no body, unlike every other mutating handler in this codebase,
// which take one in a JSON POST body) and captures the deleted row's own
// values via DELETE ... RETURNING into the audit record's Metadata (a
// before-image) - Stage 3D specialist review (ledger-finance/security)
// found the original version recorded only the id, making a deletion's
// actual financial-control impact (e.g. removing a require_step_up =
// true row) unreconstructable from the audit trail alone, contrary to
// CLAUDE.md's "every mutating administrative/financial action writes an
// audit record... reason code" rule.
func newDeleteWithdrawalPolicyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid withdrawal policy id")
			return
		}
		reasonCode := r.URL.Query().Get("reason_code")
		v := validation.New()
		v.RequireNonEmpty("reason_code", reasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var before withdrawalPolicyResponse
			var beforeBrandID *uuid.UUID
			var threshold int64
			var requiredApprovals int16
			var requireStepUp bool
			var policyVersion int32
			var effectiveFrom time.Time
			err := tx.QueryRow(ctx,
				`DELETE FROM withdrawal_policies WHERE id = $1
				 RETURNING brand_id, asset_code, approval_threshold_minor_units, required_approvals, require_step_up, policy_version, effective_from`,
				id,
			).Scan(&beforeBrandID, &before.AssetCode, &threshold, &requiredApprovals, &requireStepUp, &policyVersion, &effectiveFrom)
			if errors.Is(err, pgx.ErrNoRows) {
				return errWithdrawalPolicyNotFound
			}
			if err != nil {
				return err
			}
			metadata := map[string]any{
				"reason_code": reasonCode, "asset_code": before.AssetCode,
				"approval_threshold_minor_units": threshold, "required_approvals": requiredApprovals,
				"require_step_up": requireStepUp, "policy_version": policyVersion,
				"effective_from": effectiveFrom.UTC().Format(rfc3339),
			}
			if beforeBrandID != nil {
				metadata["brand_id"] = beforeBrandID.String()
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "withdrawal_policy.deleted", TargetType: "withdrawal_policy", TargetID: id.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: metadata,
			})
		})
		if errors.Is(err, errWithdrawalPolicyNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "withdrawal policy not found")
			return
		}
		if err != nil {
			logger.Error("delete_withdrawal_policy_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to delete withdrawal policy")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

var errWithdrawalPolicyNotFound = errors.New("httpserver: withdrawal policy not found")
