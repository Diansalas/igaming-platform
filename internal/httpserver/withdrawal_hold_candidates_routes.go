package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// A10c (ADR 0114 R3 / G9): the hold-candidate list - the approved withdrawals of the PATH tenant whose
// tenant or brand is not active (the only withdrawals the HSEC release flow can act on). READ-ONLY.
//
//   - Gate: withdrawal_hold_resolution:read (platform_admin only) AND a platform token; a tenant-scoped
//     token is 403 even if it somehow held the permission. The path tenant is validated by canActOnTenant
//     (a platform caller names any tenant) and the platform read is committed to the tenant-visible
//     platform-action audit log BEFORE the tenant rows are read (fail closed).
//   - Data path: ONE tenant-scoped read-only RLS transaction (app.tenant_id = the path tenant) with an
//     explicit tenant_id predicate, on the existing withdrawal_requests tenant_staff_scope policy. No new
//     policy, no SECURITY DEFINER function and no platform-wide read of withdrawal_requests is added: the
//     ADR's "narrow function" was only needed if the read had to cross tenants, and it does not.
//   - Returned: withdrawal id, brand id, amount (minor units, string), asset code and exponent, the time the
//     row was last changed, tenant and brand status. Never a player id, wallet id, destination, provider
//     reference or idempotency key. open_resolution_id is NOT returned: hold resolutions are readable only
//     by an acting session (migration 0124), which a read never opens; the console cross-checks the
//     existing resolution list instead.

func registerWithdrawalHoldCandidateRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/admin/tenants/{tenantID}/withdrawal-hold-candidates",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermWithdrawalHoldResolutionRead)(newListHoldCandidatesHandler(deps))))
}

type holdCandidateDTO struct {
	WithdrawalRequestID string `json:"withdrawal_request_id"`
	BrandID             string `json:"brand_id"`
	AmountMinorUnits    string `json:"amount_minor_units"`
	AssetCode           string `json:"asset_code"`
	Exponent            int16  `json:"exponent"`
	StateChangedAt      string `json:"state_changed_at"`
	TenantStatus        string `json:"tenant_status"`
	BrandStatus         string `json:"brand_status"`
}

func newListHoldCandidatesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginStaffRead(deps, w, r)
		if !ok {
			return
		}
		if !c.platform() {
			apierror.Write(w, c.requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
			return
		}
		q := r.URL.Query()
		limit, offset, pok := strictPage(q)
		if !pok || !onlyParams(q, "limit", "offset") || len(q["limit"]) > 1 || len(q["offset"]) > 1 {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid query parameter")
			return
		}
		exists, err := c.auditPlatformRead(r.Context(), deps, "withdrawal_hold_candidates.platform_read", "withdrawal_request", "",
			map[string]any{"op": "list"})
		if err != nil {
			c.logger.Error("hold_candidates_audit_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list hold candidates")
			return
		}
		if !exists {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		const from = `
		  FROM withdrawal_requests w
		  JOIN tenants t ON t.id = w.tenant_id
		  JOIN brands b ON b.id = w.brand_id AND b.tenant_id = w.tenant_id
		 WHERE w.tenant_id = $1 AND w.state = 'approved' AND (t.status <> 'active' OR b.status <> 'active')`
		var items []holdCandidateDTO
		var total int
		err = deps.DB.WithTenantReadOnly(r.Context(), c.target, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*)`+from, c.target).Scan(&total); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `
				SELECT w.id, w.brand_id, w.amount::text, w.asset_code, w.updated_at, t.status, b.status`+from+`
				 ORDER BY w.updated_at ASC, w.id ASC
				 LIMIT $2 OFFSET $3`, c.target, limit, offset)
			if err != nil {
				return err
			}
			type row struct {
				id, brand      uuid.UUID
				amount, asset  string
				changed        time.Time
				tenantS, brndS string
			}
			var got []row
			for rows.Next() {
				var x row
				if err := rows.Scan(&x.id, &x.brand, &x.amount, &x.asset, &x.changed, &x.tenantS, &x.brndS); err != nil {
					rows.Close()
					return err
				}
				got = append(got, x)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			exps := map[string]int16{}
			items = make([]holdCandidateDTO, 0, len(got))
			for _, x := range got {
				if _, seen := exps[x.asset]; !seen {
					a, err := assetregistry.GetAsset(ctx, tx, x.asset)
					if err != nil {
						return err
					}
					exps[x.asset] = a.DecimalExponent
				}
				items = append(items, holdCandidateDTO{
					WithdrawalRequestID: x.id.String(), BrandID: x.brand.String(), AmountMinorUnits: x.amount, AssetCode: x.asset,
					Exponent: exps[x.asset], StateChangedAt: x.changed.UTC().Format(rfc3339), TenantStatus: x.tenantS, BrandStatus: x.brndS,
				})
			}
			return nil
		})
		if err != nil {
			c.logger.Error("hold_candidates_failed", "err", err.Error())
			apierror.Write(w, c.requestID, apierror.CodeInternal, "failed to list hold candidates")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, pageParams{Limit: limit, Offset: offset}, total))
	}
}
