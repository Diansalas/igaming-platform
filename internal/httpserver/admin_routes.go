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
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// canActOnTenant implements the ADR 0011 rule: a platform-scoped caller
// (nil tenant_id - only platform_admin can reach these routes at all,
// via PermTenantWrite/PermBrandWrite) may act on any target tenant; a
// tenant-scoped caller may only act on their own.
func canActOnTenant(tc tenant.Context, targetTenantID uuid.UUID) bool {
	return tc.TenantID == uuid.Nil || tc.TenantID == targetTenantID
}

// --- Tenant provisioning (platform_admin only) ---

type createTenantRequest struct {
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	LicensingModel string `json:"licensing_model"`
}

type tenantResponse struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	LicensingModel string `json:"licensing_model"`
	Status         string `json:"status"`
}

func newCreateTenantHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var req createTenantRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("name", req.Name)
		v.RequireNonEmpty("slug", req.Slug)
		v.RequireOneOf("licensing_model", req.LicensingModel, "under_platform_licence", "own_licence")
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var t identity.Tenant
		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			t, err = identity.CreateTenant(ctx, tx, req.Name, req.Slug, req.LicensingModel)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "tenant.created", TargetType: "tenant", TargetID: t.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
		})
		if errors.Is(err, identity.ErrSlugTaken) {
			apierror.Write(w, requestID, apierror.CodeConflict, "a tenant with this slug already exists")
			return
		}
		if err != nil {
			logger.Error("create_tenant_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create tenant")
			return
		}

		writeJSON(w, http.StatusCreated, tenantResponse{
			ID: t.ID.String(), Name: t.Name, Slug: t.Slug, LicensingModel: t.LicensingModel, Status: t.Status,
		})
	}
}

// --- Brand provisioning ---

type createBrandRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type brandResponse struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Status   string `json:"status"`
}

func newCreateBrandHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		targetTenantID, err := uuid.Parse(r.PathValue("tenantID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid tenant id")
			return
		}
		if !canActOnTenant(tc, targetTenantID) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "cannot act on a different tenant")
			return
		}

		var req createBrandRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("name", req.Name)
		v.RequireNonEmpty("slug", req.Slug)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var brand identity.Brand
		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithTenant(r.Context(), targetTenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			brand, err = identity.CreateBrand(ctx, tx, targetTenantID, req.Name, req.Slug)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: targetTenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "brand.created", TargetType: "brand", TargetID: brand.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
		})
		if errors.Is(err, identity.ErrSlugTaken) {
			apierror.Write(w, requestID, apierror.CodeConflict, "a brand with this slug already exists")
			return
		}
		if err != nil {
			logger.Error("create_brand_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create brand")
			return
		}

		writeJSON(w, http.StatusCreated, brandResponse{
			ID: brand.ID.String(), TenantID: brand.TenantID.String(), Name: brand.Name, Slug: brand.Slug, Status: brand.Status,
		})
	}
}

// --- Staff administration ---

type createStaffRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
	// PersonID is optional and almost always omitted - see
	// identity.StaffUser.PersonID's doc comment. Pass it only when this
	// staff member is a KNOWN, deliberately identified dual-role
	// individual (also a player at some brand) - it must name an
	// existing persons.id or the request fails validation.
	PersonID string `json:"person_id,omitempty"`
}

type staffResponse struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	PersonID string `json:"person_id,omitempty"`
}

// newCreateStaffHandler creates a staff user for a specific tenant.
// Platform_admin creation is deliberately NOT exposed here - it goes
// through cmd/seed-admin only, since an HTTP endpoint that can mint a
// platform-wide administrator would itself need to be guarded by an
// existing platform_admin, and the one-time bootstrap problem that
// creates is exactly what the CLI tool avoids (see
// docs/decisions/0014-service-identity-pattern.md).
//
// role = "finance" may ONLY be created by a platform-scoped caller
// (platform_admin) - Stage 3D specialist review (security/ledger-finance/
// architect, converging independently) found that removing withdrawal
// permissions from RoleTenantAdmin's OWN role definition (permission.go)
// does not, by itself, achieve business decision #4/#5's "staff-
// management and withdrawal-approval authority must be permission-
// separated": a tenant_admin still holds PermStaffManage, and
// PermStaffManage alone was sufficient to mint a BRAND NEW finance staff
// account (with a password and person_id entirely of the tenant_admin's
// own choosing) and immediately log in as it - a self-service
// escalation from "can manage staff" to "can approve withdrawals" that
// no person-linkage check catches, since the shell account can be linked
// to any existing person the tenant_admin can reference. This is the
// same class of attack directive item 2 names explicitly: "staff admin
// must not be able to silently grant withdrawal approval via account
// creation." Restricting finance-role creation to platform_admin closes
// it at the one point that actually matters (who can mint the
// credential), not merely at the role-definition level - see ADR 0024's
// residual-findings section for the full analysis. tenant_admin remains
// able to create tenant_admin/support/compliance staff for its own
// tenant, since none of those roles hold any withdrawal permission.
func newCreateStaffHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		targetTenantID, err := uuid.Parse(r.PathValue("tenantID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid tenant id")
			return
		}
		if !canActOnTenant(tc, targetTenantID) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "cannot act on a different tenant")
			return
		}

		var req createStaffRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("email", req.Email)
		if len(req.Password) < minPasswordLen {
			apierror.Write(w, requestID, apierror.CodeValidation, "password too short")
			return
		}
		v.RequireOneOf("role", req.Role, "tenant_admin", "support", "compliance", "finance", "risk_manager", "promotions_manager", "bonus_operations")
		var personID *uuid.UUID
		if req.PersonID != "" {
			parsed, err := uuid.Parse(req.PersonID)
			if err != nil {
				v.Add("person_id", "must be a valid UUID")
			} else {
				personID = &parsed
			}
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		if identity.StaffRole(req.Role) == identity.StaffRoleFinance && tc.TenantID != uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeForbidden, "the finance role may only be created by a platform administrator")
			return
		}
		// Stage 4G: identical reasoning to the finance restriction just
		// above - a tenant_admin's own PermStaffManage would otherwise let
		// it mint a risk_manager account (of a password/person_id entirely
		// of its own choosing) and self-escalate into Risk & Limits
		// configuration authority, the exact class of attack Stage 3D's
		// specialist review found for finance.
		if identity.StaffRole(req.Role) == identity.StaffRoleRiskManager && tc.TenantID != uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeForbidden, "the risk_manager role may only be created by a platform administrator")
			return
		}
		// Stage 4H-B1 Wave 2 (security-architecture.md's P1 finding,
		// "the staff-creation allowlist must be extended, or constraint 1
		// and 2 are both defeated on day one"): identical reasoning to
		// finance/risk_manager above - a tenant_admin's own
		// PermStaffManage would otherwise let it mint a bonus_operations
		// account (of a password/person_id entirely of its own choosing)
		// and self-escalate into bonus_grant:issue/bonus_adjustment:write/
		// bonus_bulk:execute/bonus_held_disposition:resolve authority it
		// was never meant to hold, and a promotions_manager account would
		// let it self-escalate into Campaign/Offer authoring authority.
		// Both roles are platform-admin-creatable only, mirroring
		// finance/risk_manager's own precedent exactly - this is a real
		// operational cost (a tenant cannot self-serve its own promotions
		// staffing), not a free win, but it is the correct trade per the
		// security doc's own analysis.
		if (identity.StaffRole(req.Role) == identity.StaffRolePromotionsManager || identity.StaffRole(req.Role) == identity.StaffRoleBonusOperations) && tc.TenantID != uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeForbidden, "the promotions_manager and bonus_operations roles may only be created by a platform administrator")
			return
		}

		passwordHash, err := auth.HashPassword(req.Password)
		if err != nil {
			logger.Error("create_staff_hash_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create staff user")
			return
		}

		var staff identity.StaffUser
		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithTenant(r.Context(), targetTenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			staff, err = identity.CreateStaffUser(ctx, tx, targetTenantID, req.Email, passwordHash, identity.StaffRole(req.Role), personID)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: targetTenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "staff.created", TargetType: "staff_user", TargetID: staff.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"role": req.Role, "person_id": req.PersonID},
			})
		})
		if errors.Is(err, identity.ErrEmailTaken) {
			apierror.Write(w, requestID, apierror.CodeConflict, "a staff user with this email already exists for this tenant")
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "person_id does not reference an existing person")
			return
		}
		if err != nil {
			logger.Error("create_staff_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create staff user")
			return
		}

		resp := staffResponse{
			ID: staff.ID.String(), TenantID: targetTenantID.String(), Email: staff.Email, Role: string(staff.Role), Status: staff.Status,
		}
		if staff.PersonID != nil {
			resp.PersonID = staff.PersonID.String()
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// --- Staff-person linkage remediation (Stage 3D) ---

type linkStaffPersonRequest struct {
	PersonID string `json:"person_id"`
}

// newLinkStaffPersonHandler sets a staff user's person_id, ONLY when it is
// currently unset - Stage 3D's remediation path (docs/decisions/0024 §1)
// for a legacy staff account that predates the mandatory-Person-linkage
// withdrawal-governance policy, or any account an admin simply never
// linked at creation. This is the ONE sanctioned way to add a link after
// the fact; identity.LinkStaffPersonID's own `WHERE person_id IS NULL`
// clause and migration 0034's staff_users_person_id_append_only trigger
// both independently refuse to let an EXISTING link be changed to a
// different person - so this endpoint can remediate an unlinked account
// but can never be used to launder an approval trail by relinking staff
// to someone else. Same platform_admin-may-target-any-tenant,
// tenant_admin-only-their-own rule as staff creation, and the same
// PermStaffManage gate: staff-administration authority is what covers
// this, per business decision #7's "use the existing identity
// architecture" - it is deliberately NOT gated by any withdrawal
// permission, since fixing a linkage is a staff-management action, not a
// withdrawal decision.
func newLinkStaffPersonHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		targetTenantID, err := uuid.Parse(r.PathValue("tenantID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid tenant id")
			return
		}
		if !canActOnTenant(tc, targetTenantID) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "cannot act on a different tenant")
			return
		}
		staffID, err := uuid.Parse(r.PathValue("staffID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid staff id")
			return
		}

		var req linkStaffPersonRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		personID, err := uuid.Parse(req.PersonID)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "person_id must be a valid UUID")
			return
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithTenant(r.Context(), targetTenantID, func(ctx context.Context, tx pgx.Tx) error {
			// Confirm staffID actually belongs to targetTenantID before
			// attempting the link - staff_users' own RLS (dual-mode:
			// tenant-scoped rows only visible under this tenant's setting)
			// already prevents cross-tenant reads/writes; this is
			// belt-and-braces so a not-found in the wrong tenant reports
			// clearly rather than via a generic "already linked or not
			// found" from LinkStaffPersonID.
			if _, err := identity.GetStaffUserByID(ctx, tx, staffID); err != nil {
				return err
			}
			if err := identity.LinkStaffPersonID(ctx, tx, staffID, personID); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: targetTenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "staff.person_linked", TargetType: "staff_user", TargetID: staffID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"person_id": personID.String()},
			})
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "staff user not found")
			return
		}
		if errors.Is(err, identity.ErrPersonNotFound) {
			apierror.Write(w, requestID, apierror.CodeValidation, "person_id does not reference an existing person")
			return
		}
		if errors.Is(err, identity.ErrAlreadyLinkedOrNotFound) {
			apierror.Write(w, requestID, apierror.CodeConflict, "staff user not found, or already linked to a person")
			return
		}
		if err != nil {
			logger.Error("link_staff_person_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to link staff person")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// newLinkPlatformStaffPersonHandler is the platform-scoped counterpart to
// newLinkStaffPersonHandler above, for the one class of staff account that
// handler can never reach: platform_admin (tenant_id IS NULL). Stage
// 4H-B0-R6 security review traced a real, confirmed gap - no code path
// anywhere in the platform could ever set person_id on a platform_admin
// account: cmd/seed-admin always created one with person_id = nil, and the
// tenant-scoped remediation route above runs inside
// db.WithTenant(targetTenantID, ...), which staff_users' own dual_scope_
// isolation policy (migration 0011) makes structurally blind to
// tenant_id IS NULL rows - a platform_admin account could never be
// remediated at all, by any caller, through any existing route. This
// mirrors newLinkStaffPersonHandler's semantics exactly (same
// identity.LinkStaffPersonID call, same NULL-to-value-only contract, same
// migration 0034 append-only trigger as the ultimate backstop) but scopes
// the connection with db.WithoutTenant instead of db.WithTenant, per the
// same dual-scope pattern GetStaffUserByID's own doc comment describes.
//
// Deliberately restricted to a platform-scoped CALLER (tc.TenantID ==
// uuid.Nil), not merely anyone holding PermStaffManage: PermStaffManage is
// also held by tenant_admin (see internal/auth/permission.go), and a
// tenant_admin has no legitimate reason to touch a platform-wide staff
// account. RequirePermission alone cannot express that distinction (it
// only sees the permission, not the caller's tenant scope), so this
// handler checks it explicitly, the same way newCreateTenantHandler's own
// PermTenantWrite-is-platform_admin-only invariant is actually enforced by
// permission assignment rather than an explicit check here - this route
// needs the explicit check because PermStaffManage is broader than
// PermTenantWrite.
//
// Route is deliberately NOT parameterized by tenantID (there is no
// tenant to target - that is the entire point), unlike the sibling route.
func newLinkPlatformStaffPersonHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		if tc.TenantID != uuid.Nil {
			// A tenant-scoped caller (e.g. tenant_admin, who also holds
			// PermStaffManage) has no legitimate reason to link a
			// platform-wide staff account's person_id.
			apierror.Write(w, requestID, apierror.CodeForbidden, "only a platform-scoped caller may act on a platform-wide staff account")
			return
		}
		staffID, err := uuid.Parse(r.PathValue("staffID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid staff id")
			return
		}

		var req linkStaffPersonRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		personID, err := uuid.Parse(req.PersonID)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "person_id must be a valid UUID")
			return
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			// staff_users' dual_scope_isolation policy (migration 0011)
			// already confines this connection to tenant_id IS NULL rows,
			// so a successful lookup here is, by construction, always a
			// platform-wide staff account - never a tenant-scoped one.
			if _, err := identity.GetStaffUserByID(ctx, tx, staffID); err != nil {
				return err
			}
			if err := identity.LinkStaffPersonID(ctx, tx, staffID, personID); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "staff.platform_person_linked", TargetType: "staff_user", TargetID: staffID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"person_id": personID.String()},
			})
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "staff user not found")
			return
		}
		if errors.Is(err, identity.ErrPersonNotFound) {
			apierror.Write(w, requestID, apierror.CodeValidation, "person_id does not reference an existing person")
			return
		}
		if errors.Is(err, identity.ErrAlreadyLinkedOrNotFound) {
			apierror.Write(w, requestID, apierror.CodeConflict, "staff user not found, or already linked to a person")
			return
		}
		if err != nil {
			logger.Error("link_platform_staff_person_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to link staff person")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- Player administration (tenant-scoped) ---

type playerAccountResponse struct {
	ID      string `json:"id"`
	BrandID string `json:"brand_id"`
	Email   string `json:"email"`
	Status  string `json:"status"`
	KYCTier int    `json:"kyc_tier"`
}

func toPlayerAccountResponse(a identity.PlayerAccount) playerAccountResponse {
	return playerAccountResponse{ID: a.ID.String(), BrandID: a.BrandID.String(), Email: a.Email, Status: string(a.Status), KYCTier: a.KYCTier}
}

const defaultPlayerListLimit = 50

func newListPlayersHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var resp []playerAccountResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			accounts, err := identity.ListPlayerAccounts(ctx, tx, defaultPlayerListLimit)
			if err != nil {
				return err
			}
			resp = make([]playerAccountResponse, 0, len(accounts))
			for _, a := range accounts {
				resp = append(resp, toPlayerAccountResponse(a))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_players_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list players")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func newGetPlayerHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid player id")
			return
		}

		var account identity.PlayerAccount
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			account, err = identity.GetPlayerAccountByID(ctx, tx, playerID)
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player not found")
			return
		}
		if err != nil {
			logger.Error("get_player_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load player")
			return
		}
		writeJSON(w, http.StatusOK, toPlayerAccountResponse(account))
	}
}

type suspendPlayerRequest struct {
	Reason string `json:"reason"`
}

func newSuspendPlayerHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid player id")
			return
		}

		var req suspendPlayerRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason", req.Reason)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := identity.SetPlayerAccountStatus(ctx, tx, playerID, identity.PlayerStatusSuspended); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "player.suspended", TargetType: "player_account", TargetID: playerID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"reason": req.Reason},
			})
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player not found")
			return
		}
		if err != nil {
			logger.Error("suspend_player_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to suspend player")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type clearIdentityReviewRequest struct {
	ReasonCode string `json:"reason_code"`
}

// newClearIdentityReviewHandler is Stage 4E's admin counterpart to
// registration landing a player_account in 'identity_review_required'
// (ADR 0027 §6/§8) - the only way such an account can ever become
// gambling-capable again (internal/rg.EvaluateEligibility denies every
// non-'active' status with zero special-casing for this one). Gated by
// PermIdentityReviewManage (RoleCompliance only - see that permission's
// own doc comment for why this is not bundled into PermPlayerSuspend).
//
// Adversarial-safety requirement (directive §8's own "do not invent a
// manual-review workflow beyond what is necessary" balanced against not
// letting this become a generic status-reset button): this handler
// verifies the account's CURRENT status is actually
// identity_review_required before transitioning it to active. Without
// this check, the exact same endpoint could be misused to reactivate a
// suspended or self-excluded account by any RoleCompliance principal -
// this is a distinct, narrower capability than PermPlayerSuspend's own
// suspend/reinstate authority, and must never silently become a general
// "set player active" action.
//
// This uses identity.SetPlayerAccountStatusIfCurrent - a single atomic
// "UPDATE ... WHERE status = identity_review_required" - rather than a
// separate read-then-write, precisely to close a TOCTOU security specialist
// review found in an earlier version of this handler: a read-then-write
// has a window in which a CONCURRENT transaction (e.g. a staff suspend
// racing this exact request) can change the account's status between the
// read and the write, and a plain write would then silently clobber that
// intervening change (e.g. reactivating an account that was JUST
// suspended by someone else). The atomic conditional update makes that
// impossible: if the status is no longer identity_review_required by the
// moment this statement runs, nothing is written, full stop.
func newClearIdentityReviewHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid player id")
			return
		}

		var req clearIdentityReviewRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		var alreadyCleared bool
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			applied, err := identity.SetPlayerAccountStatusIfCurrent(ctx, tx, playerID,
				identity.PlayerStatusIdentityReviewRequired, identity.PlayerStatusActive)
			if err != nil {
				return err
			}
			if !applied {
				// Either the account does not exist, or its status was no
				// longer identity_review_required by the time this ran (a
				// concurrent change, or it was never in review) - this
				// read is for choosing the right response code ONLY, it
				// never drives the mutation decision above.
				if _, err := identity.GetPlayerAccountByID(ctx, tx, playerID); err != nil {
					return err
				}
				alreadyCleared = true
				return nil
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "identity_review.cleared", TargetType: "player_account", TargetID: playerID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"reason_code": req.ReasonCode, "previous_status": string(identity.PlayerStatusIdentityReviewRequired)},
			})
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player not found")
			return
		}
		if err != nil {
			logger.Error("clear_identity_review_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to clear identity review")
			return
		}
		if alreadyCleared {
			apierror.Write(w, requestID, apierror.CodeConflict, "player account is not in identity_review_required status")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- Audit trail (read-only) ---

type auditEntryResponse struct {
	ID         string `json:"id"`
	ActorType  string `json:"actor_type"`
	ActorID    string `json:"actor_id,omitempty"`
	Action     string `json:"action"`
	TargetType string `json:"target_type,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	Outcome    string `json:"outcome"`
	CreatedAt  string `json:"created_at"`
}

const defaultAuditListLimit = 100

func newListAuditLogHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var resp []auditEntryResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx,
				`SELECT id, actor_type, actor_id, action, target_type, target_id, outcome, created_at
				 FROM audit_log WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT $2`,
				tc.TenantID, defaultAuditListLimit,
			)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var (
					id                         uuid.UUID
					actorType, action, outcome string
					actorID                    *uuid.UUID
					targetType, targetID       *string
					createdAt                  time.Time
				)
				if err := rows.Scan(&id, &actorType, &actorID, &action, &targetType, &targetID, &outcome, &createdAt); err != nil {
					return err
				}
				entry := auditEntryResponse{ID: id.String(), ActorType: actorType, Action: action, Outcome: outcome, CreatedAt: createdAt.Format(time.RFC3339)}
				if actorID != nil {
					entry.ActorID = actorID.String()
				}
				if targetType != nil {
					entry.TargetType = *targetType
				}
				if targetID != nil {
					entry.TargetID = *targetID
				}
				resp = append(resp, entry)
			}
			return rows.Err()
		})
		if err != nil {
			logger.Error("list_audit_log_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load audit log")
			return
		}
		if resp == nil {
			resp = []auditEntryResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
