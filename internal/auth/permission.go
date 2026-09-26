package auth

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Permission is a fine-grained, named capability. Routes are gated by
// permission, not by hardcoding a list of roles per route - this is the
// "permission-oriented design" the Stage 2 instructions ask for, kept to
// a small, curated set actually used by something in this codebase
// (per CLAUDE.md's scope-expansion test) rather than a speculative
// enterprise-wide permission catalog.
type Permission string

const (
	PermTenantRead    Permission = "tenant:read"
	PermTenantWrite   Permission = "tenant:write"
	PermBrandRead     Permission = "brand:read"
	PermBrandWrite    Permission = "brand:write"
	PermPlayerRead    Permission = "player:read"
	PermPlayerSuspend Permission = "player:suspend"
	PermAuditRead     Permission = "audit:read"
	PermStaffManage   Permission = "staff:manage"

	// PermWithdrawalReview/Approve/Reject/Submit gate the four distinct
	// withdrawal-governance capabilities the Stage 3D directive requires
	// be separable (business decision #4/#5: "staff-management authority
	// and withdrawal-approval authority must be permission-separated,"
	// and no broad admin role may hold withdrawal authority merely by
	// being an admin - only by explicit grant). Stage 3B originally
	// collapsed all four into one PermWithdrawalApprove; splitting them
	// lets a future role hold, say, review-only visibility without
	// approval power, and makes "which roles can move money" a single,
	// auditable set of map entries rather than one broad permission.
	//
	// Holding one of these still only answers "may this role attempt
	// this action at all" - the approving/rejecting/submitting PRINCIPAL
	// additionally has to satisfy internal/withdrawal's own distinct-
	// approver/non-beneficiary/Person-linkage/active-status checks
	// (withdrawal-state-machine.md §5, ADR 0024) before any specific
	// call actually succeeds.
	PermWithdrawalReview  Permission = "withdrawal:review"
	PermWithdrawalApprove Permission = "withdrawal:approve"
	PermWithdrawalReject  Permission = "withdrawal:reject"
	PermWithdrawalSubmit  Permission = "withdrawal:submit"
	// PermProviderConfigWrite gates writing a tenant's ProviderCapability
	// configuration rows (docs/decisions/0022 §2.1 - a platform-level
	// administrative action, always audited).
	PermProviderConfigWrite Permission = "provider_config:write"

	// PermSportsbookBetRead gates STAFF, tenant-wide Back Office visibility
	// into every player's sportsbook bets (Stage 6) - mirroring
	// PermWithdrawalReview's read-only-visibility shape and naming
	// convention exactly (a player always sees their OWN bet history via
	// the self-service endpoint regardless of this permission - RLS's
	// player_self_scope policy, migration 0078, not RBAC, is what
	// authorizes that). Granted to the same roles that already hold other
	// tenant-wide FINANCIAL read visibility (PermRGRestrictionRead/
	// PermVerificationRead's precedent): RoleTenantAdmin and RoleCompliance,
	// plus RoleSupport (which already holds bare PermPlayerRead for
	// ordinary player-support visibility) and RoleFinance (which already
	// holds every withdrawal-governance permission - a sportsbook bet is
	// exactly the same class of financial fact a Finance role legitimately
	// needs to see when investigating a player's balance history).
	// Deliberately NOT RolePlatformAdmin - identical reasoning to
	// PermRGRestrictionRead's own doc comment: a platform_admin cannot
	// resolve a specific tenant's player_account at all today, so granting
	// it would be exactly the "capability nothing can actually use"
	// CLAUDE.md's "no fake completion" rule warns against.
	PermSportsbookBetRead Permission = "sportsbook_bet:read"

	// PermCasinoTransactionRead gates STAFF, tenant-wide Back Office
	// visibility into every player's casino rounds/transactions (Stage 7) -
	// mirrors PermSportsbookBetRead's shape and grant list exactly, for the
	// identical reason: a player always sees their OWN casino history via
	// the self-service endpoint regardless of this permission (RLS's
	// player_self_scope policy on casino_launch_sessions, not RBAC,
	// authorizes that). Deliberately NOT RolePlatformAdmin, for the same
	// reason PermSportsbookBetRead excludes it.
	PermCasinoTransactionRead Permission = "casino_transaction:read"

	// PermWithdrawalPolicyWrite gates the Stage 3D withdrawal_policies
	// admin API (docs/decisions/0024 §5). Deliberately its own
	// permission, never bundled with PermWithdrawalApprove: the role
	// that approves withdrawals must not also be the role that can
	// loosen the policy gating its own approvals (e.g. lowering a
	// threshold or turning off step-up immediately before approving,
	// then reverting it - withdrawal-state-machine.md §5 bypass #3).
	PermWithdrawalPolicyWrite Permission = "withdrawal_policy:write"

	// PermCasinoConfigWrite gates a tenant's own casino integration
	// configuration (Stage 4A, ADR 0025 §4/§11): writing a
	// CasinoProviderCapability row (which providers/assets/game types this
	// tenant routes to) and toggling a platform-catalogue title's
	// tenant/brand availability. Tenant-level routing configuration, not a
	// money-moving action, so it is bundled with the tenant's other
	// provider/config permissions rather than split out per sub-action -
	// mirrors PermProviderConfigWrite's identical scope for payments.
	PermCasinoConfigWrite Permission = "casino_config:write"

	// PermCasinoCatalogueManage gates registering/updating a title in the
	// PLATFORM-WIDE game catalogue (casino_games - ADR 0025 §2). Deliberately
	// its own, platform-only permission, never granted to RoleTenantAdmin:
	// a tenant may opt into a title the platform has already vetted
	// (PermCasinoConfigWrite) but must never be able to add an unvetted
	// title to the shared catalogue every other tenant can then also see.
	PermCasinoCatalogueManage Permission = "casino_catalogue:manage"

	// PermCasinoCatalogueGovern gates the Stage 9.2 (ADR 0081 §5/§5.2,
	// ARCH-DB-2 Phase 2) four-eyes governance surface for casino_games:
	// filing and deciding a casino_catalogue_change_requests row. Holding
	// PermCasinoCatalogueManage alone is NOT enough to remove a
	// jurisdiction_blocklist code or reactivate a disabled game -
	// migration 0086's casino_games_dual_control trigger additionally
	// requires an approved request filed by a DIFFERENT platform
	// principal, exactly as PermAssetRegistryManage does not by itself
	// satisfy the asset registry's own dual-control checks (migration
	// 0044). Deliberately its own permission rather than folded into
	// PermCasinoCatalogueManage: a role that can request a catalogue
	// widening should not automatically be able to decide (approve/reject)
	// one - separation of duties is enforced by the database's own
	// self-approval trigger regardless, but keeping the permission
	// distinct documents the intent and leaves room for a future role
	// split (e.g. a compliance role that may only decide, never request).
	// Platform-only, granted only to RolePlatformAdmin - same shape as
	// PermAssetRegistryManage/PermCasinoCatalogueManage.
	PermCasinoCatalogueGovern Permission = "casino_catalogue:govern"

	// PermRGRestrictionWrite gates creating a staff-initiated Responsible
	// Gaming restriction (Stage 4D-RG, ADR 0026 §12) - today, only
	// self-exclusion, always scoped to the caller's own tenant/brand (never
	// platform-wide - see internal/rg.CreateStaffRestriction's own doc
	// comment for why). Deliberately its own permission, NEVER bundled into
	// PermPlayerSuspend/PermStaffManage/PermTenantWrite: the directive's own
	// explicit instruction is that this must not be automatically granted to
	// every broad administrator (RoleTenantAdmin does NOT get it, mirroring
	// the withdrawal-approval precedent - ADR 0024's identical separation-
	// of-duties rationale). Granted only to RoleCompliance - migration
	// 0037's staff_insert RLS policy independently enforces the
	// own-tenant-only scope at the database, not just here.
	PermRGRestrictionWrite Permission = "rg_restriction:write"
	// PermRGRestrictionRead gates reading another player's Responsible
	// Gaming restriction history (a player always sees their OWN via the
	// self-service endpoint regardless of this permission - RLS's
	// player_self_read policy, not RBAC, is what authorizes that).
	PermRGRestrictionRead Permission = "rg_restriction:read"

	// PermIdentityReviewManage gates clearing a player_account out of the
	// 'identity_review_required' status Stage 4E introduced (ADR 0027
	// §6/§8) - a security-sensitive decision identical in shape to
	// PermRGRestrictionWrite (Stage 4D-RG, ADR 0026 §12): clearing a
	// review incorrectly is exactly as capable of enabling self-exclusion
	// evasion as writing a restriction incorrectly, so it gets the SAME
	// separation-of-duties treatment - its own dedicated permission,
	// deliberately never bundled into PermPlayerSuspend/PermStaffManage/
	// PermTenantWrite, and NOT granted to RoleTenantAdmin or
	// RolePlatformAdmin. Granted only to RoleCompliance.
	PermIdentityReviewManage Permission = "identity_review:manage"

	// PermVerificationRead gates STAFF read access to another player's
	// KYC verification/document metadata (Stage 4F) - a player always
	// sees their OWN via the self-service endpoints regardless of this
	// permission, exactly like PermRGRestrictionRead's own precedent.
	// Granted to RoleCompliance and RoleTenantAdmin (an operational admin
	// reasonably needs to see a player's verification state), never
	// RolePlatformAdmin (no path to resolve a tenant-scoped
	// player_account at all - identical reasoning to every other
	// platform-admin exclusion in this file) and never RoleFinance
	// (directive §19's explicit "do not grant sensitive verification
	// access to Finance... unless explicitly justified" - no
	// justification exists).
	PermVerificationRead Permission = "verification:read"
	// PermVerificationReview gates APPROVING/REJECTING a verification or
	// document (Stage 4F) - deliberately its own permission, never
	// bundled into PermVerificationRead/PermPlayerSuspend/PermStaffManage,
	// mirroring PermRGRestrictionWrite/PermIdentityReviewManage's
	// identical separation-of-duties precedent exactly. Granted ONLY to
	// RoleCompliance - not RoleTenantAdmin (read-only visibility is
	// reasonable for an operational admin; approving/rejecting identity
	// evidence is a compliance-specific judgment call), not RoleFinance,
	// not RolePlatformAdmin.
	PermVerificationReview Permission = "verification:review"

	// PermRiskConfigRead gates read-only visibility into a tenant's Risk &
	// Limits rules (Stage 4G, ADR 0031). Granted to RoleRiskManager and
	// RoleTenantAdmin (an operational admin reasonably needs to see what
	// limits apply), never RoleCompliance/RoleFinance/RolePlatformAdmin -
	// directive §24's explicit "do not automatically grant risk
	// configuration to broad administrative roles," and no justification
	// exists for Compliance/Finance to hold it either (they consult risk
	// DENIALS via the existing audit trail, not this permission).
	PermRiskConfigRead Permission = "risk_config:read"
	// PermRiskConfigManage gates CREATING/DISABLING a risk_rules row -
	// deliberately its OWN authority, never bundled into
	// PermVerificationReview/PermWithdrawalPolicyWrite/PermStaffManage,
	// mirroring PermVerificationReview's separation-of-duties precedent.
	// Granted ONLY to RoleRiskManager - not RoleTenantAdmin (read-only
	// visibility is reasonable for an operational admin; writing a limit
	// that changes what players/tenants can wager or move is a
	// risk-specific judgment call), not RoleCompliance/RoleFinance/
	// RolePlatformAdmin.
	PermRiskConfigManage Permission = "risk_config:manage"

	// PermAssetRegistryManage gates ADR 0037 layers 1-3 - creating an
	// asset, updating its display metadata, activating/suspending it,
	// granting/revoking its platform authorization, and writing a
	// PLATFORM-WIDE operation-eligibility default. Deliberately its own
	// PLATFORM-ONLY permission, granted only to RolePlatformAdmin and
	// never to any tenant-scoped role: ADR 0037 §C.1's two-tier split is
	// explicit that "registration and platform-level activation are not a
	// tenant concern, and must never be reachable by a tenant-scoped role,
	// full stop". This is the same shape as PermCasinoCatalogueManage
	// (platform-wide game catalogue) versus PermCasinoConfigWrite (a
	// tenant's own routing) - and here the blast radius is larger still,
	// since an asset row is referenced by every tenant's ledger.
	//
	// Holding it only answers "may this role attempt the operation". The
	// three dual-controlled operations (create/activate/platform-authorize,
	// §C.5.3) additionally require an approved asset_change_requests row
	// filed by a DIFFERENT platform principal, enforced by migration
	// 0044's triggers - exactly as holding PermWithdrawalApprove does not
	// by itself satisfy withdrawal's own distinct-approver checks.
	PermAssetRegistryManage Permission = "asset_registry:manage"

	// PermAssetAuthorizationWrite gates ADR 0037 layers 4-7 - which assets
	// this TENANT offers, per brand, per jurisdiction, and per
	// product/operation. Tenant-scoped by design (§C.1): these mutations
	// can only ever narrow within what layers 1-3 already authorized
	// platform-wide, which is what bounds their blast radius to one tenant
	// and is why §C.5.3 deliberately does NOT require dual control for
	// them. Granted to RoleTenantAdmin alongside its other
	// tenant-configuration permissions (PermProviderConfigWrite,
	// PermCasinoConfigWrite), never to RolePlatformAdmin - a platform
	// principal has no tenant scope to write these rows in, and
	// migration 0045's RLS would reject the write anyway.
	PermAssetAuthorizationWrite Permission = "asset_authorization:write"

	// Stage 4H-B1 Wave 2 (Bonus Engine) permissions, matching
	// security-architecture.md §B1.1's table and §W15.4.3/§W15.1.12's
	// amendments EXACTLY (permission names are security's own, already-
	// published names - not invented here). Role-to-permission wiring is
	// in rolePermissions below, restricted to the wiring the security doc
	// explicitly names; no role gets a bonus permission the doc does not
	// list for it.
	PermBonusRead                Permission = "bonus:read"
	PermBonusConfigRead          Permission = "bonus_config:read"
	PermBonusCampaignCreate      Permission = "bonus_campaign:create"
	PermBonusCampaignUpdate      Permission = "bonus_campaign:update"
	PermBonusCampaignActivate    Permission = "bonus_campaign:activate"
	PermBonusCampaignSuspend     Permission = "bonus_campaign:suspend"
	PermBonusOfferManage         Permission = "bonus_offer:manage"
	PermBonusSegmentManage       Permission = "bonus_segment:manage"
	PermBonusGrantIssue          Permission = "bonus_grant:issue"
	PermBonusGrantReview         Permission = "bonus_grant:review"
	PermBonusGrantCancel         Permission = "bonus_grant:cancel"
	PermBonusAdjustmentWrite     Permission = "bonus_adjustment:write"
	PermBonusBulkExecute         Permission = "bonus_bulk:execute"
	PermBonusReportRead          Permission = "bonus_report:read"
	PermBonusApprovalPolicyWrite Permission = "bonus_approval_policy:write"

	// PermBonusSuggestionCreate (§W15.4.3, closing SEC-W15-20): "Service
	// identity only - held by no human role." Deliberately absent from
	// EVERY entry of rolePermissions below, per that section's own binding
	// wiring constraint 1 ("bonus_suggestion:create is granted to no
	// role") - a future edit must not add it to any role's set.
	PermBonusSuggestionCreate Permission = "bonus_suggestion:create"
	// PermBonusSuggestionReview: claim/annotate/edit/approve/reject, or
	// create a manual-origin suggestion (§W15.4.3) - confers no power to
	// activate anything.
	PermBonusSuggestionReview Permission = "bonus_suggestion:review"

	// PermBonusHeldDispositionResolve gates REQ-SEP-BONUS-4 (doc 10
	// §N1.9/§N1.12; security-architecture.md §W15.1.12): resolving a
	// bonus_held_dispositions row (ACTION_REFORFEIT/ACTION_ROUTE_TO_CASH/
	// the manual sub-choice under ACTION_HOLD_FOR_REVIEW). Deliberately
	// its OWN authority, never folded into PermBonusAdjustmentWrite or
	// PermBonusBulkExecute (§W15.1.12's own argument: "a distinct economic
	// act... with its own volume, its own risk profile, and its own
	// reporting need" - folding it in would let existing adjustment-
	// authorized staff resolve deferred G-2 dispositions without any
	// role-wiring decision ever having been made about it). Does NOT gate
	// a HeldDispositionRecord's CREATION (doc 10 N1.8.1: TECHNICAL,
	// "recording-and-parking a fact," never an authorizing write) - only
	// its resolution.
	PermBonusHeldDispositionResolve Permission = "bonus_held_disposition:resolve"

	// PermJurisdictionRegistryManage gates the Stage 4I admin write
	// surface for the PLATFORM-WIDE `jurisdictions` and `licences`
	// registries (docs/governance/stage-4i-canonical-model.md §6.1/§11.1
	// item B-1). Before this permission existed, neither table had ANY
	// application write path at all - rows could only be created by
	// direct database access, which is itself the hard prerequisite gap
	// the canonical model names: no FK to `jurisdictions` can be
	// satisfied by anything the platform's own code produces without it.
	// Deliberately its own PLATFORM-ONLY permission, granted only to
	// RolePlatformAdmin and never to any tenant-scoped role - the EXACT
	// precedent of PermCasinoCatalogueManage (platform-wide game
	// catalogue) and PermAssetRegistryManage (platform-wide asset
	// registry, layers 1-3): a jurisdiction or licence row is shared
	// reference data every tenant reads, and a tenant-scoped role must
	// never be able to add one unilaterally. `jurisdictions`/`licences`
	// carry no row-level security (canonical-model §6.1 - they are
	// platform facts, not tenant-owned rows), so this permission check at
	// the HTTP layer is the entire control on the write side; every write
	// is still audited (jurisdiction_registry.* actions) regardless.
	PermJurisdictionRegistryManage Permission = "jurisdiction_registry:manage"

	// PermJurisdictionResolutionActiveWrite gates writing a tenant's own
	// jurisdiction_resolution_active fact (canonical-model §4.2/§11.1
	// item B-6) - the resolver-owned, tenant-scoped record of whether
	// jurisdiction resolution is genuinely active for a (tenant,
	// operation_class) pair, which RISK §2.4b's future risk.CreateRule
	// precondition (R-2b, NOT implemented by this permission or by
	// internal/jurisdiction - that is risk's own later implementation
	// phase) will read. Tenant-scoped by design, following
	// PermAssetAuthorizationWrite's exact precedent (canonical-model
	// §4.2: "write permission follows PermAssetAuthorizationWrite's
	// precedent: tenant-scoped, granted to RoleTenantAdmin, never
	// RolePlatformAdmin") - a platform principal has no tenant scope to
	// write this fact in, and migration 0071's RLS would reject the
	// write anyway.
	PermJurisdictionResolutionActiveWrite Permission = "jurisdiction_resolution_active:write"

	// PermTenantLicenceAssign gates binding a tenant to the licence it
	// actually operates under (tenants.licence_id) - the write path Stage 4I
	// Phase A adds. Deliberately separate from PermTenantWrite (which only
	// covers name/slug/licensing_model provisioning) and from
	// PermJurisdictionRegistryManage (which only creates jurisdictions/
	// licences REFERENCE rows, never binds a live tenant to one) - binding a
	// live tenant to a licence determines which jurisdiction's rules govern
	// that tenant's operations, a materially different authorizing act.
	// PLATFORM-ONLY, granted only to RolePlatformAdmin, following
	// PermJurisdictionRegistryManage's exact precedent - see
	// docs/plans/stage-4i-jurisdiction-implementation-plan.md Phase A.
	PermTenantLicenceAssign Permission = "tenant_licence:assign"

	// PermPlayerResidenceRead gates reading a player's declared or verified
	// residence VALUE on a staff-facing surface (presence/provenance fields
	// are NOT gated by this - they ride on PermVerificationRead/PermPlayerRead
	// as ordinary metadata). Deliberately separate and narrower than either
	// - HDR-J-3f requires reading these facts to need "its own specific
	// permission, separate from ordinary player-record access"
	// (docs/decisions/0042-human-decision-response.md). Granted ONLY to
	// RoleCompliance - see docs/plans/stage-4i-jurisdiction-implementation-plan.md
	// Phase B.
	PermPlayerResidenceRead Permission = "player_residence:read"

	// PermJurisdictionEvidenceCollectionActivate gates the per-tenant,
	// per-evidence-type activation switch (jurisdiction_evidence_collection_active)
	// that must be ON before any declared-residence write or verified-residence
	// determination is accepted - Stage 4I Phase B's activation boundary.
	// Switching on collection of privacy-sensitive personal data is a
	// lawful-basis act, not a commercial-configuration act, so this sits with
	// RoleCompliance ONLY - deliberately NOT RoleTenantAdmin, even though
	// RoleTenantAdmin holds the analogous PermJurisdictionResolutionActiveWrite
	// for a purely-engineering precondition fact. See
	// docs/plans/stage-4i-jurisdiction-implementation-plan.md Phase B.
	PermJurisdictionEvidenceCollectionActivate Permission = "jurisdiction_evidence_collection:activate"

	// PermJurisdictionEvaluationPolicyWrite is the permission that WILL gate
	// authoring an evaluation policy VERSION for a licensing jurisdiction
	// (jurisdiction_precedence_configs, canonical-model §3.4 as widened by
	// Stage 4I Phase D). It is declared and role-scoped now, but NO caller
	// checks it in this phase - there is no HTTP route, and
	// CreateEvaluationPolicyVersion itself does not check it (its only
	// controls are assertPlatformScope and migration 0075's RLS INSERT/
	// UPDATE policies, both of which are already platform-admin-only).
	// PLATFORM-ONLY, granted only to RolePlatformAdmin, following
	// PermJurisdictionRegistryManage's exact precedent: this is platform-wide
	// reference configuration with no tenant_id, shared by every tenant
	// licensed in that jurisdiction, so a tenant-scoped role must never be
	// able to change it - a tenant admin editing it would be changing another
	// tenant's evaluation policy. Deliberately NOT PermJurisdictionRegistry
	// Manage itself: creating a jurisdiction/licence reference row and
	// authoring the policy that will govern whether players may play are
	// materially different authorizing acts, the same distinction
	// PermTenantLicenceAssign's own doc comment already draws against
	// PermTenantWrite/PermJurisdictionRegistryManage.
	//
	// ACTIVATION is deliberately NOT covered by this permission: writing an
	// active version is refused outright in this phase (ErrActivationNot
	// Authorized) and, when the phase that has HDR-J-8/HDR-J-9 content adds
	// it, it requires its own permission and a dual-control ruling per
	// HDR-J-2's recorded technical consequence.
	PermJurisdictionEvaluationPolicyWrite Permission = "jurisdiction_evaluation_policy:write"

	// --- Stage 4I Phase E: operating market / country policy (ADR 0045
	// Section 6.1). Four named exceptions under canonical-model Section
	// 6.1's amendment ("no other permission without a recorded architect
	// ruling naming it") - this is that record. Declared and role-scoped
	// now; NO HTTP route exists in this phase and no caller checks any of
	// them - the write functions' own controls are assertPlatformScope/
	// assertTenantScope plus migration 0076's RLS, both already scoped
	// identically to what these permissions describe.

	// PermOperatingMarketCeilingManage gates authoring a LICENCE COUNTRY
	// CEILING version (licence_country_ceilings) - the statement of which
	// countries a LICENCE permits at all. PLATFORM-ONLY, granted only to
	// RolePlatformAdmin, following PermJurisdictionRegistryManage/
	// PermTenantLicenceAssign's exact precedent. A ceiling row is shared by
	// every tenant operating under that licence, so a tenant-scoped role
	// must never be able to change it - editing it would change another
	// tenant's ceiling. Deliberately NOT PermJurisdictionRegistryManage
	// itself: creating a licence reference row and declaring the countries
	// that licence permits are materially different authorizing acts, the
	// same distinction PermTenantLicenceAssign already draws.
	PermOperatingMarketCeilingManage Permission = "operating_market_ceiling:manage"

	// PermOperatingMarketTenantPolicyWrite gates authoring a TENANT-scope
	// operating-country policy version - the decision that this tenant
	// operates (or does not operate) in a country at all, within its
	// licence ceiling. TENANT-SCOPED, granted ONLY to RoleCompliance,
	// deliberately NOT RoleTenantAdmin: deciding which countries a tenant
	// serves is a licensing/compliance act, not commercial configuration -
	// the exact reasoning and the exact placement of
	// PermJurisdictionEvidenceCollectionActivate.
	PermOperatingMarketTenantPolicyWrite Permission = "operating_market_tenant_policy:write"

	// PermOperatingMarketBrandPolicyWrite gates authoring a BRAND- or
	// OPERATION-scope policy version. These can ONLY ever narrow within
	// the tenant-scope footprint Compliance has already approved, and
	// cannot widen past a broader in-force disable at their own scope
	// either (ADR 0045 §3.5-A): a more-specific brand/operation row can
	// never resolve `permitted` while a broader, in-force, active
	// `disabled` row applies to it - enforced at write time by migration
	// 0076's ceiling trigger and, independently, by the resolver's own
	// first-disabled-wins evaluation of the full candidate set. This is
	// what bounds their blast radius and is why they are separated from
	// the tenant-scope permission at all. TENANT-SCOPED, granted to
	// RoleTenantAdmin - mirroring PermAssetAuthorizationWrite's placement
	// for ADR 0037 layers 4-7 exactly. Never RolePlatformAdmin (no tenant
	// scope to write in; RLS would reject it).
	PermOperatingMarketBrandPolicyWrite Permission = "operating_market_brand_policy:write"

	// PermOperatingMarketPolicyRead gates the read/diagnostic surface:
	// current state, version history, and the admin-only explain-why.
	// Separate from every write permission above, per ADR 0045's explicit
	// separation requirement. Granted to RoleCompliance and RoleTenantAdmin
	// (tenant-scoped reads) and to RolePlatformAdmin (whose reads reach the
	// licence ceiling ONLY - a tenant's own operating footprint is not
	// platform-readable, see migration 0076's RLS).
	PermOperatingMarketPolicyRead Permission = "operating_market_policy:read"

	// PermSportsbookJurisdictionRestrictionManage gates creating/withdrawing
	// a sb_jurisdiction_restrictions row (Stage 9.2, ADR 0083 §5.2/§9.2) -
	// the sportsbook-owned, deny-only, event/market/selection jurisdiction
	// gate, modelled on PermCasinoCatalogueManage's identical platform-only
	// shape: this table carries no tenant_id column at all (a restriction
	// is a platform-level statement applying identically to every tenant -
	// ADR 0083 §5.2.2), so, exactly like the platform-wide game catalogue,
	// no tenant-scoped role may hold it - a tenant administering its own
	// operation must never be able to widen or narrow a platform-level
	// jurisdiction control. Granted only to RolePlatformAdmin.
	PermSportsbookJurisdictionRestrictionManage Permission = "sportsbook_jurisdiction_restriction:manage"
	// PermSportsbookJurisdictionRestrictionRead gates the admin list
	// endpoint for sb_jurisdiction_restrictions - deliberately separate
	// from PermSportsbookJurisdictionRestrictionManage (mirrors
	// PermRiskConfigRead/PermRiskConfigManage's identical read/write
	// split), even though the underlying table's own RLS read policy is
	// platform-uniform read-open (migration 0087) - the split documents
	// intent and leaves room for a future read-only compliance role.
	// Granted only to RolePlatformAdmin, since only a platform admin can
	// ever hold the write half here.
	PermSportsbookJurisdictionRestrictionRead Permission = "sportsbook_jurisdiction_restriction:read"

	// PermSportsbookExposureLimitManage gates creating/disabling a
	// sb_exposure_limits row (Stage 9.2 Part B2, ADR 0083 §6.2.3/§9.2) -
	// the sportsbook-owned, cross-player, per-(scope_kind, asset_code)
	// trading-book exposure ceiling. UNLIKE
	// PermSportsbookJurisdictionRestrictionManage, this table carries a
	// tenant_id and IS ordinary tenant-owned commercial configuration
	// (identical in kind to a risk_rules max-stake limit, ADR 0083
	// §6.1.3) - so this is a TENANT risk_manager-class permission,
	// mirroring PermRiskConfigManage's identical shape and grant
	// (RoleRiskManager only, never RoleTenantAdmin/RoleCompliance/
	// RoleFinance/RolePlatformAdmin - a risk-specific judgment call, same
	// separation-of-duties rationale as PermRiskConfigManage's own doc
	// comment).
	PermSportsbookExposureLimitManage Permission = "sportsbook_exposure_limit:manage"
	// PermSportsbookExposureLimitRead gates the admin list endpoint for
	// sb_exposure_limits - deliberately separate from
	// PermSportsbookExposureLimitManage (mirrors PermRiskConfigRead/
	// PermRiskConfigManage's identical read/write split). Granted to
	// RoleRiskManager and RoleTenantAdmin (an operational admin reasonably
	// needs to see what exposure ceilings apply, exactly like
	// PermRiskConfigRead's own rationale) - never RoleCompliance/
	// RoleFinance/RolePlatformAdmin: this table has no platform-wide rows
	// at all (unlike risk_rules), and platform_admin has no tenant scope
	// to read it in.
	PermSportsbookExposureLimitRead Permission = "sportsbook_exposure_limit:read"

	// PermSportsbookSettlementSimulate gates the Stage 10 W1 (ADR 0088 §9)
	// non-production test-support staff route (POST /v1/admin/sportsbook/
	// bets/{id}/simulate-settlement-event), which is the ONLY driver of
	// the in-house mock settlement lifecycle (settle/void/rollback) -
	// there is no real sportsbook settlement provider or webhook. Sole
	// grantee: RoleRiskManager (security review S1) - deliberately NEVER
	// RoleFinance (it already holds withdrawal-approval authority; a role
	// that can both approve withdrawals AND drive settlement payouts
	// breaks ADR 0024's separation of duties, the exact same reasoning
	// Stage 3D applied to RoleTenantAdmin/PermStaffManage), and never
	// RoleTenantAdmin, RoleCompliance, RoleSupport, RolePlatformAdmin, or
	// any bonus role. Holding this permission is inert in production:
	// the route itself is never registered there
	// (httpserver.Deps.SportsbookSettlementSimulationEnabled, ADR 0085's
	// two-layer gate). Not added to backoffice/src/auth/permissions.ts -
	// no UI control exists for a non-production test-support route.
	PermSportsbookSettlementSimulate Permission = "sportsbook_settlement:simulate"
)

// Provider credential permissions (Stage 10.3 W2a; ADR 0093 §3 and its
// W2a design-review amendment; security review §6). Four distinct
// permissions, never bundled into PermTenantWrite, PermCasinoConfigWrite
// or PermProviderConfigWrite:
//
//   - PermProviderCredentialRead: list handles (key_id, status, window,
//     fingerprint, secret_ref) - never a secret value. Platform admin and
//     tenant admin.
//   - PermProviderCredentialRequest: file a registration, and apply an
//     approved one. Platform admin only.
//   - PermProviderCredentialApprove: decide a registration. Platform admin
//     only; holding it never bypasses the DB's distinct-Person check.
//   - PermProviderCredentialRevoke: the single-actor transitions
//     (verify_only, shorten, revoke). Platform admin and tenant admin.
const (
	PermProviderCredentialRead    Permission = "provider_credential:read"
	PermProviderCredentialRequest Permission = "provider_credential:request"
	PermProviderCredentialApprove Permission = "provider_credential:approve"
	PermProviderCredentialRevoke  Permission = "provider_credential:revoke"
)

// rolePermissions is a static, in-code role -> permission-set mapping.
// Stage 2 does not make this database-driven/partner-configurable - that
// would be a Stage 6 partner-console feature (custom roles), premature
// before there's a real multi-tenant admin surface to configure it from.
var rolePermissions = map[Role]map[Permission]bool{
	RolePlatformAdmin: permSet(
		PermTenantRead, PermTenantWrite, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
		PermCasinoCatalogueManage,
		// Stage 9.2 (ADR 0081 §5.2): the sole grantee of
		// PermCasinoCatalogueGovern. See that permission's own doc comment.
		PermCasinoCatalogueGovern,
		// Stage 4H-B0-R6: the sole grantee of PermAssetRegistryManage
		// (ADR 0037 layers 1-3). See that permission's own doc comment.
		PermAssetRegistryManage,
		// Stage 4I item B-1: the sole grantee of
		// PermJurisdictionRegistryManage (platform-wide `jurisdictions`/
		// `licences` registries). See that permission's own doc comment -
		// same shape as PermAssetRegistryManage/PermCasinoCatalogueManage.
		PermJurisdictionRegistryManage,
		// Stage 4I Phase A: the sole grantee of PermTenantLicenceAssign
		// (binding a live tenant to the licence it operates under). See
		// that permission's own doc comment for why it is separate from
		// both PermTenantWrite and PermJurisdictionRegistryManage.
		PermTenantLicenceAssign,
		// Stage 4I Phase D: the sole grantee of
		// PermJurisdictionEvaluationPolicyWrite (authoring a
		// jurisdiction_precedence_configs evaluation-policy VERSION -
		// canonical-model section 3.4 as widened by that phase). Canonical-
		// model section 6.1's "no other permission and no new role is
		// authorized" sentence is amended by that phase's architect ruling
		// to "no other permission without a recorded architect ruling
		// naming it" - this permission is that named exception. See that
		// permission's own doc comment for why it is separate from
		// PermJurisdictionRegistryManage.
		PermJurisdictionEvaluationPolicyWrite,
		// Stage 4I Phase E: the sole grantee of PermOperatingMarketCeilingManage
		// (authoring a licence_country_ceilings version), plus
		// PermOperatingMarketPolicyRead (whose reads reach the licence
		// ceiling only - a tenant's own operating footprint is not
		// platform-readable). Never PermOperatingMarketTenantPolicyWrite/
		// PermOperatingMarketBrandPolicyWrite - platform_admin has no
		// tenant scope to write either in; RLS would reject it.
		PermOperatingMarketCeilingManage, PermOperatingMarketPolicyRead,
		// Stage 9.2 (ADR 0083 §5.2/§9.2): the sole grantee of both
		// sportsbook jurisdiction-restriction permissions. See their own
		// doc comments.
		PermSportsbookJurisdictionRestrictionManage, PermSportsbookJurisdictionRestrictionRead,
		// Deliberately NOT PermRGRestrictionWrite/Read (Stage 4D-RG, ADR
		// 0026 §12): platform_admin cannot resolve a specific tenant's
		// player_account at all today (PermPlayerRead is itself
		// RequireTenantScope-gated, and player_accounts' own RLS has no
		// platform-wide read policy), so it has no way to legitimately
		// exercise either permission - granting it would be exactly the
		// "capability nothing can actually use" CLAUDE.md's "no fake
		// completion" rule warns against. See CreateStaffRestriction's own
		// doc comment for the full reasoning and the recorded OPEN DECISION.
		PermProviderCredentialRead, PermProviderCredentialRequest, PermProviderCredentialApprove, PermProviderCredentialRevoke,
	),
	// Stage 3D business decision #4/#5: tenant_admin (a broad
	// administrative role that also holds PermStaffManage) deliberately
	// does NOT get any withdrawal-governance permission. Holding both
	// staff-management and withdrawal-approval authority in one role was
	// exactly the privilege-escalation path Stage 3C's specialist review
	// found (a tenant_admin could mint unlinked "finance" staff accounts
	// via PermStaffManage, then approve through them).
	//
	// Removing the grant here alone does NOT fully close that path -
	// Stage 3D's own specialist review (security/ledger-finance/architect,
	// independently) found that a tenant_admin retaining PermStaffManage
	// can still mint a BRAND NEW finance-role staff account (choosing its
	// password and person_id) and log in as it, since finance's
	// permissions come from ITS OWN role, not tenant_admin's. What
	// actually closes it is internal/httpserver/admin_routes.go's
	// newCreateStaffHandler restricting role="finance" creation to a
	// platform-scoped caller (platform_admin) - see that function's own
	// doc comment and ADR 0024's residual-findings section. This role-
	// permission removal remains necessary (it is what stops a
	// tenant_admin from acting AS tenant_admin on a withdrawal) but is not
	// sufficient on its own; the two together are what achieve the
	// decision. If a tenant genuinely needs one human to both administer
	// staff and approve withdrawals, that requires two separate role
	// grants on two separate accounts, never one role bundling both.
	RoleTenantAdmin: permSet(
		PermTenantRead, PermBrandRead, PermBrandWrite,
		PermPlayerRead, PermPlayerSuspend, PermAuditRead, PermStaffManage,
		PermProviderConfigWrite, PermWithdrawalPolicyWrite, PermCasinoConfigWrite,
		// PermRGRestrictionRead only, deliberately NOT PermRGRestrictionWrite -
		// Stage 4D-RG's own explicit instruction (mirroring Stage 3D business
		// decision #4/#5's withdrawal-approval precedent exactly): a broad
		// tenant administrator must never get RG-restriction WRITE authority
		// automatically just for holding PermStaffManage/PermPlayerSuspend.
		// Read-only visibility into restrictions affecting their own tenant's
		// players remains reasonable for an operational admin role.
		PermRGRestrictionRead,
		// Stage 4F: read-only verification/document visibility, same
		// reasoning as PermRGRestrictionRead immediately above - never
		// PermVerificationReview.
		PermVerificationRead,
		// Stage 4G: read-only Risk & Limits visibility, same reasoning -
		// never PermRiskConfigManage.
		PermRiskConfigRead,
		// Stage 9.2 Part B2 (ADR 0083 §6.2.3/§9.2): read-only sportsbook
		// exposure-limit visibility, identical reasoning to
		// PermRiskConfigRead immediately above - never
		// PermSportsbookExposureLimitManage.
		PermSportsbookExposureLimitRead,
		// Stage 4H-B0-R6: ADR 0037 layers 4-7 (which assets this tenant
		// offers, per brand/jurisdiction/product). Never
		// PermAssetRegistryManage - that is platform-only.
		PermAssetAuthorizationWrite,
		// Stage 4I item B-6: this tenant's own jurisdiction-resolution-
		// active facts. Never PermJurisdictionRegistryManage - that is
		// platform-only, same split as PermAssetAuthorizationWrite versus
		// PermAssetRegistryManage.
		PermJurisdictionResolutionActiveWrite,
		// Stage 4I Phase E: the sole grantee of PermOperatingMarketBrandPolicyWrite
		// (authoring a BRAND- or OPERATION-scope operating-country policy
		// version, which can only ever narrow within the tenant-scope
		// footprint Compliance has already approved, and cannot widen past
		// a broader in-force disable at its own scope either - ADR 0045
		// §3.5-A), plus PermOperatingMarketPolicyRead for this tenant's own
		// footprint.
		// Never PermOperatingMarketTenantPolicyWrite (deciding whether the
		// tenant serves a country at all is a Compliance act, not
		// commercial configuration) and never PermOperatingMarketCeilingManage
		// (platform-only).
		PermOperatingMarketBrandPolicyWrite, PermOperatingMarketPolicyRead,
		// Stage 4H-B1 Wave 2 (security-architecture.md §B1.1): read-only
		// bonus visibility "exactly as it does for risk_config/
		// verification/rg_restriction today" - a tenant admin may see
		// every campaign and every player's grant history, and may not
		// author, activate, issue, adjust, bulk-assign, or cancel
		// anything. PermBonusApprovalPolicyWrite sits HERE (never on
		// either new bonus role) because "the role that approves must not
		// also be the role that can loosen the policy gating its own
		// approvals" (§B1.1 hard constraint 4) - RoleTenantAdmin holds no
		// four-eyes-gated bonus permission, so it satisfies that
		// constraint by construction.
		PermBonusConfigRead, PermBonusRead, PermBonusReportRead, PermBonusApprovalPolicyWrite,
		// Stage 6: tenant-wide sportsbook bet visibility - see that
		// permission's own doc comment.
		PermSportsbookBetRead,
		// Stage 7: see PermCasinoTransactionRead's own doc comment.
		PermCasinoTransactionRead,
		PermProviderCredentialRead, PermProviderCredentialRevoke,
	),
	RoleSupport: permSet(
		PermPlayerRead,
		// Stage 4H-B1 Wave 2 (security-architecture.md §B1.1 table).
		PermBonusRead,
		// Stage 6: see PermSportsbookBetRead's own doc comment.
		PermSportsbookBetRead,
		// Stage 7: see PermCasinoTransactionRead's own doc comment.
		PermCasinoTransactionRead,
	),
	RoleCompliance: permSet(
		PermPlayerRead, PermPlayerSuspend, PermAuditRead,
		// The sole tenant-scoped grantee of PermRGRestrictionWrite (Stage
		// 4D-RG, ADR 0026 §12) - a tenant/brand-scoped restriction only
		// (migration 0037's staff_insert RLS policy rejects a platform-wide
		// row from any tenant-scoped connection); a genuinely platform-wide
		// restriction additionally requires RolePlatformAdmin.
		PermRGRestrictionWrite, PermRGRestrictionRead,
		// Stage 4E: the sole grantee of PermIdentityReviewManage - see that
		// permission's own doc comment for why it mirrors
		// PermRGRestrictionWrite's separation-of-duties treatment exactly.
		PermIdentityReviewManage,
		// Stage 4H-B1 Wave 2 (security-architecture.md §B1.1 table):
		// read-only bonus visibility plus the fail-closed kill-switch
		// (bonus_campaign:suspend is deliberately granted widely - "an
		// emergency kill-switch must not need a second approver").
		PermBonusRead, PermBonusConfigRead, PermBonusCampaignSuspend,
		// Stage 4F: the sole grantee of PermVerificationReview, plus
		// read visibility - see both permissions' own doc comments.
		PermVerificationRead, PermVerificationReview,
		// Stage 4I Phase B: the sole grantee of PermPlayerResidenceRead and
		// PermJurisdictionEvidenceCollectionActivate - see both
		// permissions' own doc comments for why they sit with Compliance
		// alone, never RoleTenantAdmin/RolePlatformAdmin.
		PermPlayerResidenceRead, PermJurisdictionEvidenceCollectionActivate,
		// Stage 4I Phase E: the sole grantee of
		// PermOperatingMarketTenantPolicyWrite (deciding whether a tenant
		// operates in a country at all, within its licence ceiling - a
		// licensing/compliance act, never RoleTenantAdmin), plus
		// PermOperatingMarketPolicyRead for this tenant's own footprint.
		// Never PermOperatingMarketCeilingManage (platform-only).
		PermOperatingMarketTenantPolicyWrite, PermOperatingMarketPolicyRead,
		// Stage 6: see PermSportsbookBetRead's own doc comment.
		PermSportsbookBetRead,
		// Stage 7: see PermCasinoTransactionRead's own doc comment.
		PermCasinoTransactionRead,
	),
	// finance is Stage 3B's own role, dedicated solely to withdrawal
	// governance - it holds all four withdrawal permissions and nothing
	// else, and deliberately does NOT hold PermStaffManage (the other
	// half of Stage 3D's required separation).
	RoleFinance: permSet(
		PermWithdrawalReview, PermWithdrawalApprove, PermWithdrawalReject, PermWithdrawalSubmit,
		// Stage 6: see PermSportsbookBetRead's own doc comment - a
		// sportsbook bet is the same class of financial fact Finance
		// already needs visibility into when investigating a player's
		// balance/withdrawal history.
		PermSportsbookBetRead,
		// Stage 7: see PermCasinoTransactionRead's own doc comment - the
		// same reasoning applies to a casino round.
		PermCasinoTransactionRead,
	),
	// Stage 4G: risk_manager is dedicated solely to Risk & Limits
	// configuration - it holds both risk_config permissions and nothing
	// else, deliberately not PermStaffManage/PermPlayerRead/PermAuditRead,
	// mirroring RoleFinance's own "one role, one narrow authority" shape.
	RoleRiskManager: permSet(
		PermRiskConfigRead, PermRiskConfigManage,
		// Stage 4H-B1 Wave 2 (security-architecture.md §B1.1 table):
		// read-only bonus config visibility plus the fail-closed
		// kill-switch, same reasoning as RoleCompliance above.
		PermBonusConfigRead, PermBonusCampaignSuspend,
		// Stage 9.2 Part B2 (ADR 0083 §6.2.3/§9.2): sportsbook exposure
		// limits are the sportsbook-domain analogue of a risk_rules
		// cumulative cap (identical "ordinary tenant risk configuration"
		// shape, ADR 0083 §6.1.3) - RoleRiskManager is the sole grantee of
		// the MANAGE half, exactly like PermRiskConfigManage above.
		PermSportsbookExposureLimitManage, PermSportsbookExposureLimitRead,
		// Stage 10 W1 (ADR 0088 §9.1, security review S1): the sole
		// grantee of PermSportsbookSettlementSimulate. See that
		// permission's own doc comment for why RoleFinance is deliberately
		// excluded despite otherwise being the "financial" role.
		PermSportsbookSettlementSimulate,
	),
	RolePlayer: permSet(),

	// RolePromotionsManager (Stage 4H-B1 Wave 2, security-architecture.md
	// §B1.1 "Role wiring"): authors and activates Campaigns/Offers/
	// Segments/BonusCodes, and reviews BonusSuggestions - but holds NO
	// grant-issuing/adjustment/bulk-execution/held-disposition-resolution
	// authority. This is what makes hard constraint 1 ("no principal may
	// hold both RolePromotionsManager and RoleBonusOperations - the
	// author of a Campaign must not also be able to hand out its value
	// directly") a real separation rather than a documentation-only one.
	RolePromotionsManager: permSet(
		PermBonusConfigRead, PermBonusRead, PermBonusReportRead,
		PermBonusCampaignCreate, PermBonusCampaignUpdate, PermBonusCampaignActivate, PermBonusCampaignSuspend,
		PermBonusOfferManage, PermBonusSegmentManage,
		// §W15.4.3 binding wiring constraint 2: bonus_suggestion:review ->
		// RolePromotionsManager, never bundled with bonus_bulk:execute or
		// bonus_grant:issue (which stay on RoleBonusOperations, below) -
		// otherwise "one principal can approve a suggestion and then
		// activate it," which the section calls "worse than no control."
		PermBonusSuggestionReview,
	),
	// RoleBonusOperations (Stage 4H-B1 Wave 2): holds the financially
	// material bonus-issuance/adjustment/bulk/held-disposition-resolution
	// set. Deliberately does NOT hold PermBonusOfferManage/
	// PermBonusCampaignActivate (hard constraint 3: "no role may hold
	// both bonus_offer:manage and bonus_adjustment:write - the 'configure
	// it instead of adjusting it' bypass is only closed if authoring and
	// adjusting are separate authorities"; §W15.1.12 extends this
	// verbatim to bonus_held_disposition:resolve) and does NOT hold
	// PermStaffManage (Stage 3D's business decision #4/#5 precedent,
	// applied to both new bonus roles per §B1.1 hard constraint 2).
	RoleBonusOperations: permSet(
		PermBonusConfigRead, PermBonusRead,
		PermBonusGrantIssue, PermBonusGrantReview, PermBonusGrantCancel,
		PermBonusAdjustmentWrite, PermBonusBulkExecute,
		// §W15.1.12's own binding wiring: "granted to RoleBonusOperations
		// only - the same role that holds bonus_adjustment:write and
		// bonus_grant:cancel - never to RolePromotionsManager,
		// RoleTenantAdmin, RoleFinance, or RolePlatformAdmin."
		PermBonusHeldDispositionResolve,
	),
}

func permSet(perms ...Permission) map[Permission]bool {
	m := make(map[Permission]bool, len(perms))
	for _, p := range perms {
		m[p] = true
	}
	return m
}

// RoleHasPermission reports whether role includes perm. An unknown role
// has no permissions (fails closed).
func RoleHasPermission(role Role, perm Permission) bool {
	return rolePermissions[role][perm]
}

// RequirePermission denies a request unless the authenticated caller's
// role includes perm. This replaces Stage 1's role-list-based
// RequireRole - permission checks are enforced server-side and are never
// satisfied by anything the requesting UI does or doesn't show.
func RequirePermission(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := observability.RequestIDFromContext(r.Context())
			tc, err := tenant.FromContext(r.Context())
			if err != nil {
				apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
				return
			}
			if !RoleHasPermission(Role(tc.Role), perm) {
				apierror.Write(w, requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyPermission denies a request unless the caller's role includes
// at least one of perms (Stage 10.3 W2a: the provider-credential request
// read routes accept provider_credential:request OR :approve). An empty
// perms list denies everything.
func RequireAnyPermission(perms ...Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := observability.RequestIDFromContext(r.Context())
			tc, err := tenant.FromContext(r.Context())
			if err != nil {
				apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
				return
			}
			for _, perm := range perms {
				if RoleHasPermission(Role(tc.Role), perm) {
					next.ServeHTTP(w, r)
					return
				}
			}
			apierror.Write(w, requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		})
	}
}

// RequireTenantScope denies a request unless the authenticated caller
// has a specific (non-nil) tenant_id. Platform-scoped principals (see
// docs/decisions/0011-platform-scoped-identity-tokens.md) hold a
// nil-tenant token and must be denied here, before a handler that
// assumes a real tenant id (e.g. to call db.WithTenant) ever runs.
func RequireTenantScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
			return
		}
		if tc.TenantID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this operation requires a tenant-scoped identity")
			return
		}
		next.ServeHTTP(w, r)
	})
}
