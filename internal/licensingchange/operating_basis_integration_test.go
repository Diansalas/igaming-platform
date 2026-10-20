//go:build integration

// Migration 0180 integration tests (lic9): the explicit operating basis (platform licence, partner licence, documented no-licence
// basis) per tenant / brand / country / operation, governed by the licensing change primitive and decided by the operating-market
// resolver. Every application-level assertion runs as igaming_runtime (non-superuser, non-BYPASSRLS); the owner pool only
// provisions fixtures. Requires TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL (a skip is NOT evidence).
//
// No jurisdiction, country or arrangement here is a statement about any real regime: country codes are arbitrary ISO codes used as
// opaque keys, jurisdictions are throwaway fixtures, and nothing is asserted to be lawful.
package licensingchange

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

const (
	bOp      = "deposit"
	bCountry = "MT"
)

func (w *world) dbNow() time.Time {
	w.t.Helper()
	var now time.Time
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	}); err != nil {
		w.t.Fatal(err)
	}
	return now
}

// widen files a WIDENING change and carries it through two distinct approvals; it returns the executed result.
func (w *world) widen(kind Kind, p any, reason string) *Result {
	w.t.Helper()
	r := w.mustRequest(w.req, kind, p, reason)
	if r.Executed || !r.Request.Widening || r.Request.RequiredApprovals != 2 {
		w.t.Fatalf("%s must be filed as widening with two required approvals: %+v", kind, r.Request)
	}
	w.mustApprove(w.ap1, r.Request.ID)
	done := w.mustApprove(w.ap2, r.Request.ID)
	if !done.Executed {
		w.t.Fatalf("%s must execute on the second approval", kind)
	}
	return done
}

// narrow files a NARROWING change; the platform requester's single actor path executes it in the same call.
func (w *world) narrow(kind Kind, p any, reason string) *Result {
	w.t.Helper()
	r := w.mustRequest(w.req, kind, p, reason)
	if !r.Executed || r.Request.Widening || r.Request.RequiredApprovals != 0 {
		w.t.Fatalf("%s must execute as a single-actor narrowing: %+v", kind, r.Request)
	}
	return r
}

func dueIn(days int) *string {
	s := time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02")
	return &s
}

// noLicence is a valid documented no-licence set payload for (tenant, brand, country, deposit).
func noLicence(tid uuid.UUID, brand *uuid.UUID, jid uuid.UUID, days int) OperatingBasisSetPayload {
	return OperatingBasisSetPayload{
		TenantID: tid, BrandID: brand, CountryCode: bCountry, OperationCode: bOp, BasisKind: "no_licence_documented",
		JurisdictionID: &jid, DeterminationReference: "DET-" + uuid.NewString()[:8], DeterminedByRole: "head_of_compliance",
		RestrictedProductCodes: []string{}, ReviewDueAt: dueIn(days),
	}
}

func licenceBasis(kind string, tid uuid.UUID, brand *uuid.UUID, lid uuid.UUID, country string) OperatingBasisSetPayload {
	return OperatingBasisSetPayload{
		TenantID: tid, BrandID: brand, CountryCode: country, OperationCode: bOp, BasisKind: kind, LicenceID: &lid,
		DeterminationReference: "DET-" + uuid.NewString()[:8], DeterminedByRole: "head_of_compliance", RestrictedProductCodes: []string{},
	}
}

func (w *world) setCeiling(lid uuid.UUID, country string) {
	w.t.Helper()
	w.widen(KindLicenceCeilingVersion, LicenceCeilingPayload{LicenceID: lid, CountryCode: country, State: "enabled", Status: "active",
		AuthorizationReference: sp("TICKET-" + uuid.NewString()[:6])}, "market_entry")
}

func (w *world) enableTenantPolicy(tid uuid.UUID, country string) {
	w.t.Helper()
	w.widen(KindOperatingPolicyVersion, OperatingPolicyPayload{TenantID: tid, Scope: "tenant", CountryCode: country, State: "enabled", Status: "active",
		AuthorizationReference: sp("TICKET-" + uuid.NewString()[:6])}, "market_entry")
}

type resolved struct {
	outcome operatingmarket.Outcome
	exp     operatingmarket.Explanation
}

// resolveAt runs the resolver and the explainer for the tenant's own session (the production path) at asOf.
func (w *world) resolveAt(tid uuid.UUID, brand *uuid.UUID, country string, asOf time.Time) resolved {
	w.t.Helper()
	var out resolved
	q := operatingmarket.Query{TenantID: tid, BrandID: brand, CountryCode: country, OperationCode: bOp, AsOf: asOf}
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, q)
		if err != nil {
			return err
		}
		exp, err := operatingmarket.ExplainOperatingCountryPolicy(ctx, tx, q)
		if err != nil {
			return err
		}
		if exp.Outcome != res.Outcome() {
			w.t.Fatalf("explain and resolve disagree: %s vs %s", exp.Outcome, res.Outcome())
		}
		out = resolved{outcome: res.Outcome(), exp: exp}
		return nil
	}); err != nil {
		w.t.Fatalf("resolve: %v", err)
	}
	return out
}

func (w *world) resolveNow(tid uuid.UUID, brand *uuid.UUID, country string) resolved {
	return w.resolveAt(tid, brand, country, w.dbNow())
}

func (w *world) wantOutcome(r resolved, want operatingmarket.Outcome, why string) {
	w.t.Helper()
	if r.outcome != want {
		w.t.Fatalf("%s: outcome %s, want %s (basis kind %q reason %q)", why, r.outcome, want, r.exp.BasisKind, r.exp.BasisReason)
	}
}

func (w *world) wantBlocked(r resolved, reason operatingmarket.BasisReason, why string) {
	w.t.Helper()
	w.wantOutcome(r, operatingmarket.OutcomeNotPermittedByBasis, why)
	if r.exp.BasisReason != reason {
		w.t.Fatalf("%s: basis reason %q, want %q", why, r.exp.BasisReason, reason)
	}
}

// ---- (c) no basis row: the legacy behaviour is unchanged -----------------------------------------------------------------

func TestOperatingBasis_NoBasisRow_LegacyBehaviourUnchanged(t *testing.T) {
	w := newWorld(t)
	// A tenant with NO licence and NO basis: licensing_unknown (never a default permission, never a default "no licence needed").
	bare := w.tenant("under_platform_licence")
	r := w.resolveNow(bare, nil, bCountry)
	w.wantOutcome(r, operatingmarket.OutcomeLicensingUnknown, "no licence, no basis")
	if r.exp.BasisKind != "" || r.exp.BasisID != nil {
		t.Fatalf("no basis row must leave the basis fields empty: %+v", r.exp)
	}
	// ... and enabling a country for it is still refused at filing (no licence, no basis): the 0180 relaxation needs a basis.
	_, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "market_entry",
		Payload: payload(t, OperatingPolicyPayload{TenantID: bare, Scope: "tenant", CountryCode: bCountry, State: "enabled", Status: "active", AuthorizationReference: sp("T-1")})})
	var dom *DomainError
	if !errors.As(err, &dom) || !errors.Is(err, operatingmarket.ErrCeilingExceeded) {
		t.Fatalf("enabling a country with no licence and no basis must still be refused, got %v", err)
	}

	// A tenant on its own licence chain: ceiling + tenant policy -> permitted; another country -> not permitted by the licence.
	tid := w.tenant("under_platform_licence")
	lid := w.licence(w.jurisdiction(), "platform", tid)
	w.setCeiling(lid, bCountry)
	w.enableTenantPolicy(tid, bCountry)
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomePermitted, "legacy permitted")
	w.wantOutcome(w.resolveNow(tid, nil, "DE"), operatingmarket.OutcomeNotPermittedByLicence, "legacy other country")
}

// ---- (a) documented no-licence basis: only with ALL conditions ----------------------------------------------------------

func TestOperatingBasis_NoLicenceDocumented_Matrix(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence") // NULL licence
	jid := w.jurisdiction()

	// Pending (filed, not approved) is nothing: no row, so the legacy answer for a NULL licence.
	req := w.mustRequest(w.req, KindOperatingBasisSet, noLicence(tid, nil, jid, 30), "documented_basis")
	if req.Executed || !req.Request.Widening {
		t.Fatalf("a basis set is widening and not executed on filing: %+v", req.Request)
	}
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomeLicensingUnknown, "pending request")
	w.mustApprove(w.ap1, req.Request.ID)
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomeLicensingUnknown, "one approval is not execution")
	if done := w.mustApprove(w.ap2, req.Request.ID); !done.Executed {
		t.Fatal("the second approval must execute the basis")
	}

	// Approved, in force, review in the future, but the policy rungs still must permit: no tenant policy -> not_configured.
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomeNotConfigured, "a basis alone is not a market opening")
	w.enableTenantPolicy(tid, bCountry)
	r := w.resolveNow(tid, nil, bCountry)
	w.wantOutcome(r, operatingmarket.OutcomePermitted, "approved no-licence basis + tenant policy")
	if r.exp.BasisKind != operatingmarket.BasisNoLicenceDocumented || r.exp.BasisID == nil {
		t.Fatalf("explain must expose the basis kind and id: %+v", r.exp)
	}
	// Another country, another operation: no basis row, NULL licence -> licensing_unknown (country config is not a basis).
	w.wantOutcome(w.resolveNow(tid, nil, "DE"), operatingmarket.OutcomeLicensingUnknown, "an unrelated country")

	// Not yet effective (AsOf before the row existed, rows exist): denied by the basis, NOT the legacy path.
	w.wantBlocked(w.resolveAt(tid, nil, bCountry, w.dbNow().Add(-2*time.Hour)), operatingmarket.BasisReasonNotYetEffective, "not yet effective")

	// Review overdue: the day after review_due_at the basis stops permitting (fail closed); the due date itself still stands.
	due, _ := time.Parse("2006-01-02", *dueIn(30))
	w.wantOutcome(w.resolveAt(tid, nil, bCountry, due.Add(12*time.Hour)), operatingmarket.OutcomePermitted, "on the review due date")
	w.wantBlocked(w.resolveAt(tid, nil, bCountry, due.Add(36*time.Hour)), operatingmarket.BasisReasonReviewOverdue, "review overdue")

	// A review record: marking needs_rereview is NARROWING (single actor) and blocks immediately.
	rr := OperatingBasisReviewPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp, ReviewDueAt: *dueIn(30), NeedsRereview: true, ReviewReference: "REV-1"}
	w.narrow(KindOperatingBasisReviewRecord, rr, "rereview_required")
	w.wantBlocked(w.resolveNow(tid, nil, bCountry), operatingmarket.BasisReasonNeedsRereview, "needs_rereview")
	// Clearing it / extending the date is WIDENING: two approvals; the date must be in the future.
	ext := OperatingBasisReviewPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp, ReviewDueAt: *dueIn(60), NeedsRereview: false, ReviewReference: "REV-2"}
	w.widen(KindOperatingBasisReviewRecord, ext, "review_completed")
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomePermitted, "review recorded")
	// A shorter date is narrowing again (single actor).
	short := ext
	short.ReviewDueAt, short.ReviewReference = *dueIn(10), "REV-3"
	w.narrow(KindOperatingBasisReviewRecord, short, "review_shortened")

	// Withdrawn: not permitted, and NOT the legacy path (the tenant has no licence anyway; with one it must still be denied).
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")
	w.wantBlocked(w.resolveNow(tid, nil, bCountry), operatingmarket.BasisReasonWithdrawn, "withdrawn")
}

// A documented no-licence basis and a licence ceiling for the same country are TWO bases: configuration_conflict, fail closed.
func TestOperatingBasis_NoLicenceDocumented_ConflictsWithLicenceCeiling(t *testing.T) {
	w := newWorld(t)
	jid := w.jurisdiction()

	// Filing is refused early when the ceiling is already there.
	t1 := w.tenant("under_platform_licence")
	l1 := w.licence(w.jurisdiction(), "platform", t1)
	w.setCeiling(l1, bCountry)
	_, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingBasisSet,
		Payload: payload(t, noLicence(t1, nil, jid, 30)), ReasonCode: "documented_basis"})
	var dom *DomainError
	if !errors.As(err, &dom) {
		t.Fatalf("a no-licence basis beside an existing ceiling must be refused at filing, got %v", err)
	}

	// A ceiling added AFTER the basis makes the resolver fail closed with a conflict (the resolver is the decider).
	t2 := w.tenant("under_platform_licence")
	l2 := w.licence(w.jurisdiction(), "platform", t2)
	w.widen(KindOperatingBasisSet, noLicence(t2, nil, jid, 30), "documented_basis")
	w.enableTenantPolicy(t2, bCountry)
	w.wantOutcome(w.resolveNow(t2, nil, bCountry), operatingmarket.OutcomePermitted, "no-licence basis beside a licence without a ceiling")
	w.setCeiling(l2, bCountry)
	r := w.resolveNow(t2, nil, bCountry)
	w.wantOutcome(r, operatingmarket.OutcomeConfigurationConflict, "basis and ceiling both present")
	if r.exp.BasisReason != operatingmarket.BasisReasonCeilingConflict {
		t.Fatalf("the conflict reason must be exposed, got %q", r.exp.BasisReason)
	}
}

// ---- (b) licence kinds: the row's licence governs, the existing validity and ceiling logic applies to it -----------------

// A partner licence for one brand differs from the tenant default; a brand-specific withdrawn row masks the tenant-wide one.
func TestOperatingBasis_PartnerLicencePerBrand_AndBrandOverride(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	platformLic := w.licence(w.jurisdiction(), "platform", tid) // the tenant default (legacy chain)
	w.setCeiling(platformLic, bCountry)
	brandA, brandB := w.brand(tid), w.brand(tid)

	// The partner's own licence (licensee tenant) for brand B in DE only. It is NOT bound through tenants.licence_id.
	partner := w.licence(w.jurisdiction(), "tenant", uuid.Nil)
	w.setCeiling(partner, "DE")
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", tid, &brandB, partner, "DE"), "partner_licence_basis")
	w.enableTenantPolicy(tid, bCountry)
	w.enableTenantPolicy(tid, "DE")

	// Brand B, DE: governed by the partner licence -> permitted. Brand A and the tenant default, DE: no basis row for them, the
	// tenant's platform licence has no DE ceiling -> not permitted by the licence.
	r := w.resolveNow(tid, &brandB, "DE")
	w.wantOutcome(r, operatingmarket.OutcomePermitted, "brand B under its partner licence")
	if r.exp.BasisKind != operatingmarket.BasisPartnerLicence || r.exp.LicenceID == nil || *r.exp.LicenceID != partner {
		t.Fatalf("explain must show the partner basis and licence: %+v", r.exp)
	}
	w.wantOutcome(w.resolveNow(tid, &brandA, "DE"), operatingmarket.OutcomeNotPermittedByLicence, "brand A has no basis in DE")
	w.wantOutcome(w.resolveNow(tid, nil, "DE"), operatingmarket.OutcomeNotPermittedByLicence, "the tenant default has no basis in DE")
	// Brand B in MT: no basis row for (brand B, MT): the legacy chain (the tenant's platform licence) still permits.
	w.wantOutcome(w.resolveNow(tid, &brandB, bCountry), operatingmarket.OutcomePermitted, "brand B in MT stays on the tenant chain")

	// A tenant-wide platform_licence basis for MT, and a brand-specific partner row for brand B in MT: brand B uses ITS row.
	w.widen(KindOperatingBasisSet, licenceBasis("platform_licence", tid, nil, platformLic, bCountry), "platform_licence_basis")
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", tid, &brandB, partner, bCountry), "partner_licence_basis")
	// The partner licence has no MT ceiling: brand B is denied by the licence ceiling of ITS basis, brand A is permitted.
	w.wantOutcome(w.resolveNow(tid, &brandB, bCountry), operatingmarket.OutcomeNotPermittedByLicence, "brand B's own basis has no MT ceiling")
	w.wantOutcome(w.resolveNow(tid, &brandA, bCountry), operatingmarket.OutcomePermitted, "brand A uses the tenant-wide row")
	// Withdrawing brand B's row masks the tenant-wide row for that brand (fail closed), it does not fall back.
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, BrandID: &brandB, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")
	w.wantBlocked(w.resolveNow(tid, &brandB, bCountry), operatingmarket.BasisReasonWithdrawn, "brand row withdrawn")
	w.wantOutcome(w.resolveNow(tid, &brandA, bCountry), operatingmarket.OutcomePermitted, "brand A is untouched")
}

// A suspended licence of a basis denies through the existing licence logic; a tenant with a NULL licence still resolves through a
// platform_licence basis (the row's licence overrides tenants.licence_id).
func TestOperatingBasis_PlatformLicenceBasis_NullTenantLicence_AndSuspension(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence") // NULL licence
	lid := w.licence(w.jurisdiction(), "platform", uuid.Nil)
	w.setCeiling(lid, bCountry)
	w.widen(KindOperatingBasisSet, licenceBasis("platform_licence", tid, nil, lid, bCountry), "platform_licence_basis")
	w.enableTenantPolicy(tid, bCountry)
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomePermitted, "platform licence basis, tenant licence NULL")

	// Suspending the licence (narrowing, single actor) denies through the existing licence logic.
	w.narrow(KindLicenceStatusChange, LicenceStatusPayload{LicenceID: lid, Status: "suspended"}, "licence_suspended")
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomeNotPermittedByLicence, "suspended licence of the basis")
}

// ---- maker-checker per kind ---------------------------------------------------------------------------------------------

func TestOperatingBasis_MakerChecker_PerKind(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	jid := w.jurisdiction()

	// set: the requester cannot approve; one approver cannot execute alone, nor twice (same Person, even through a second account).
	r := w.mustRequest(w.req, KindOperatingBasisSet, noLicence(tid, nil, jid, 30), "documented_basis")
	if _, err := w.approve(w.req, r.Request.ID); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("the requester must not approve their own basis, got %v", err)
	}
	if _, err := w.approve(w.reqTwin, r.Request.ID); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("the requester's Person (second account) must not approve, got %v", err)
	}
	w.mustApprove(w.ap1, r.Request.ID)
	if _, err := w.approve(w.ap1, r.Request.ID); err == nil {
		t.Fatal("the same approver must not approve twice")
	}
	if _, err := w.approve(w.ap1Twin, r.Request.ID); err == nil {
		t.Fatal("a second account of the same Person must not supply the second approval")
	}
	if got := w.status(r.Request.ID); got != StatusRequested && got != StatusApproved {
		t.Fatalf("one Person's approvals must never execute the basis, status %s", got)
	}
	if n := w.ownerInt(`SELECT count(*) FROM operating_bases WHERE tenant_id = $1`, tid); n != 0 {
		t.Fatalf("no basis row may exist before two distinct approvals, found %d", n)
	}
	w.mustApprove(w.ap2, r.Request.ID)
	if n := w.ownerInt(`SELECT count(*) FROM operating_bases WHERE tenant_id = $1 AND change_request_id = $2`, tid, r.Request.ID); n != 1 {
		t.Fatalf("the executed basis must carry its change request, found %d", n)
	}

	// withdraw: narrowing, single actor, executed in the requester's transaction.
	wd := w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")
	if wd.Request.ExecutedBy == nil || *wd.Request.ExecutedBy != w.req.StaffID {
		t.Fatalf("a withdrawal executes by its requester: %+v", wd.Request)
	}
	// ... and there is nothing left to withdraw: refused, nothing written.
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingBasisWithdraw,
		Payload: payload(t, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp}), ReasonCode: "again"}); err == nil {
		t.Fatal("withdrawing a withdrawn basis must be refused")
	}

	// review record on a fresh basis: extension widens (two approvals), shortening narrows (single actor), a no-op is refused.
	t2 := w.tenant("under_platform_licence")
	w.widen(KindOperatingBasisSet, noLicence(t2, nil, jid, 30), "documented_basis")
	same := OperatingBasisReviewPayload{TenantID: t2, CountryCode: bCountry, OperationCode: bOp, ReviewDueAt: *dueIn(30), ReviewReference: "REV-X"}
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingBasisReviewRecord, Payload: payload(t, same), ReasonCode: "noop"}); err == nil {
		t.Fatal("a review record that changes nothing must be refused")
	}
	later := same
	later.ReviewDueAt = *dueIn(90)
	lr := w.mustRequest(w.req, KindOperatingBasisReviewRecord, later, "review_completed")
	if lr.Executed || !lr.Request.Widening || lr.Request.RequiredApprovals != 2 {
		t.Fatalf("a later review date widens and needs two approvals: %+v", lr.Request)
	}
	w.narrow(KindOperatingBasisReviewRecord, OperatingBasisReviewPayload{TenantID: t2, CountryCode: bCountry, OperationCode: bOp, ReviewDueAt: *dueIn(5), ReviewReference: "REV-Y"}, "review_shortened")

	// Tenant staff may never request a basis change.
	cs := w.tenantStaff(t2, "compliance")
	if _, err := w.svc.Request(context.Background(), cs, RequestInput{Kind: KindOperatingBasisWithdraw,
		Payload: payload(t, OperatingBasisWithdrawPayload{TenantID: t2, CountryCode: bCountry, OperationCode: bOp}), ReasonCode: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant staff must be forbidden to request a basis change, got %v", err)
	}
}

// ---- payload validation, licensee / kind, cross-tenant licence ----------------------------------------------------------

func TestOperatingBasis_Refusals_LicenseeKind_CrossTenant_Shapes(t *testing.T) {
	w := newWorld(t)
	tX := w.tenant("own_licence")
	tY := w.tenant("own_licence")
	jid := w.jurisdiction()
	platformLic := w.licence(jid, "platform", uuid.Nil)
	partnerLic := w.licence(jid, "tenant", tX) // bound to tenant X through tenants.licence_id
	other := w.licence(jid, "tenant", uuid.Nil)

	file := func(p OperatingBasisSetPayload) error {
		_, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingBasisSet, Payload: payload(t, p), ReasonCode: "basis"})
		return err
	}
	guard := func(err error, code string) bool { return sqlstateOf(err) == code }

	if err := file(licenceBasis("platform_licence", tY, nil, partnerLic, bCountry)); !guard(err, "LC010") {
		t.Fatalf("platform_licence with a tenant-licensee licence must be refused (LC010), got %v", err)
	}
	if err := file(licenceBasis("partner_licence", tY, nil, platformLic, bCountry)); !guard(err, "LC010") {
		t.Fatalf("partner_licence with a platform-licensee licence must be refused (LC010), got %v", err)
	}
	if err := file(licenceBasis("partner_licence", tY, nil, partnerLic, bCountry)); !guard(err, "LC010") {
		t.Fatalf("a partner licence bound to another tenant must be refused for this tenant (LC010), got %v", err)
	}
	if err := file(licenceBasis("partner_licence", tY, nil, uuid.New(), bCountry)); !guard(err, "LC010") {
		t.Fatalf("an unknown licence must be refused (LC010), got %v", err)
	}
	// Tenant X itself may use the licence bound to it; and a free partner licence goes to ONE tenant only.
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", tX, nil, partnerLic, bCountry), "basis")
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", tY, nil, other, bCountry), "basis")
	if err := file(licenceBasis("partner_licence", tX, nil, other, "DE")); !guard(err, "LC010") {
		t.Fatalf("a partner licence already used by another tenant's basis must be refused (LC010), got %v", err)
	}
	// A foreign brand is refused by the composite foreign key at execution.
	foreignBrand := w.brand(tX)
	bad := licenceBasis("platform_licence", tY, &foreignBrand, platformLic, bCountry)
	r := w.mustRequest(w.req, KindOperatingBasisSet, bad, "basis")
	w.mustApprove(w.ap1, r.Request.ID)
	if _, err := w.approve(w.ap2, r.Request.ID); err == nil {
		t.Fatal("a brand of another tenant must be refused at execution")
	}

	// Shape and content refusals before anything is stored.
	cases := map[string]func(p *OperatingBasisSetPayload){
		"no_licence without review date":     func(p *OperatingBasisSetPayload) { p.ReviewDueAt = nil },
		"no_licence with a licence":          func(p *OperatingBasisSetPayload) { p.LicenceID = &platformLic },
		"no_licence without jurisdiction":    func(p *OperatingBasisSetPayload) { p.JurisdictionID = nil },
		"review date in the past":            func(p *OperatingBasisSetPayload) { p.ReviewDueAt = dueIn(-1) },
		"a person's name as the role":        func(p *OperatingBasisSetPayload) { p.DeterminedByRole = "Jane Doe" },
		"an e-mail address in the reference": func(p *OperatingBasisSetPayload) { p.DeterminationReference = "jane@example.com" },
		"a phone number in the note":         func(p *OperatingBasisSetPayload) { p.RestrictionsNote = sp("call +44 20 7946 0958") },
		"unknown basis kind":                 func(p *OperatingBasisSetPayload) { p.BasisKind = "exempt" },
		"lower-case country":                 func(p *OperatingBasisSetPayload) { p.CountryCode = "mt" },
		"unknown operation":                  func(p *OperatingBasisSetPayload) { p.OperationCode = "teleport" },
		"unknown restricted product":         func(p *OperatingBasisSetPayload) { p.RestrictedProductCodes = []string{"no_such_product"} },
	}
	// Control: the unmutated payload is accepted for filing (so every refusal below is attributable to its mutation).
	control := noLicence(tY, nil, jid, 30)
	control.CountryCode = "FR" // a scope no other step of this test used
	if err := file(control); err != nil {
		t.Fatalf("the control payload must be accepted for filing: %v", err)
	}
	for name, mutate := range cases {
		p := control
		p.DeterminationReference = "DET-" + uuid.NewString()[:8] // a distinct request each time
		mutate(&p)
		err := file(p)
		if err == nil {
			t.Errorf("%s: must be refused", name)
			continue
		}
		t.Logf("%s: refused: %.140v", name, err)
	}
	if n := w.ownerInt(`SELECT count(*) FROM operating_bases WHERE tenant_id = $1 AND country_code = 'FR'`, tY); n != 0 {
		t.Fatalf("a refused filing must write nothing, found %d rows", n)
	}
	// Missing keys are refused (a missing key is never read as "none").
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingBasisSet,
		Payload: []byte(`{"tenant_id":"` + tY.String() + `","country_code":"MT"}`), ReasonCode: "x"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a partial payload must be ErrInvalidInput, got %v", err)
	}
}

// ---- direct writes, tenant isolation, owner path ------------------------------------------------------------------------

func TestOperatingBasis_DirectWrites_Refused_AndOwnerRowIsNotAnApprovedBasis(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	jid := w.jurisdiction()
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, jid, 30), "documented_basis")

	insert := `INSERT INTO operating_bases (tenant_id, country_code, operation_code, basis_kind, jurisdiction_id, determination_reference,
		determined_by_role, review_due_at, status, reason_code, policy_version)
		VALUES ($1, 'DE', 'deposit', 'no_licence_documented', $2, 'DET-X', 'head_of_compliance', current_date + 30, 'active', 'x', 'forged')`

	// The runtime role in a platform session (passes RLS) is refused by the governed-write guard.
	err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, insert, tid, jid); return err })
	if pgCode(err) != "LC040" {
		t.Fatalf("a direct INSERT by the runtime role must be LC040, got %v", err)
	}
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operating_bases SET effective_to = now() WHERE tenant_id = $1`, tid)
		return err
	})
	if pgCode(err) != "LC040" {
		t.Fatalf("a direct close by the runtime role must be LC040, got %v", err)
	}
	for _, sql := range []string{
		`UPDATE operating_bases SET status = 'withdrawn' WHERE tenant_id = $1`,
		`UPDATE operating_bases SET review_due_at = current_date + 3000 WHERE tenant_id = $1`,
		`DELETE FROM operating_bases WHERE tenant_id = $1`,
	} {
		sql := sql
		err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, tid); return err })
		if pgCode(err) != "42501" {
			t.Fatalf("%q by the runtime role must be a privilege error (42501), got %v", sql, err)
		}
	}
	err = w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE operating_bases`)
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("TRUNCATE by the runtime role must be a privilege error, got %v", err)
	}
	// A tenant-scoped session cannot write at all (no tenant write policy).
	err = w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, insert, tid, jid); return err })
	// Two independent layers: the BEFORE INSERT guard (LC040) fires before the row-level-security WITH CHECK (42501); either refuses.
	if c := pgCode(err); c != "42501" && c != "LC040" {
		t.Fatalf("a tenant-scoped INSERT must be refused (LC040 / row-level security), got %v", err)
	}
	if n := w.ownerInt(`SELECT count(*) FROM pg_policies WHERE tablename = 'operating_bases' AND cmd IN ('INSERT', 'UPDATE', 'DELETE', 'ALL') AND qual || coalesce(with_check, '') LIKE '%app.tenant_id%' AND qual || coalesce(with_check, '') NOT LIKE '%platform_admin_principal_id%'`); n != 0 {
		t.Fatalf("no tenant write policy may exist on operating_bases, found %d", n)
	}
	if n := w.ownerInt(`SELECT count(*) FROM operating_bases WHERE tenant_id = $1`, tid); n != 1 {
		t.Fatalf("no forged row may exist, found %d", n)
	}

	// The owner path (provisioning) may insert, but the row carries no change request and is NOT an approved basis; the owner can
	// neither edit it, delete it nor truncate the table.
	t2 := w.tenant("under_platform_licence")
	w.ownerExec(insert, t2, jid)
	if n := w.ownerInt(`SELECT count(*) FROM operating_bases WHERE tenant_id = $1 AND change_request_id IS NULL AND created_by_actor_id IS NULL`, t2); n != 1 {
		t.Fatal("an owner-path row must carry NULL change_request_id and created_by_actor_id")
	}
	w.ownerPolicyFixture(t2, "DE")
	r := w.resolveNow(t2, nil, "DE")
	w.wantBlocked(r, operatingmarket.BasisReasonNotApproved, "owner-path row")
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operating_bases SET status = 'withdrawn' WHERE tenant_id = $1`, t2)
		return err
	}); pgCode(err) != "LC041" {
		t.Fatalf("the owner may not edit a basis row (LC041), got %v", err)
	}
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE operating_bases`)
		return err
	}); pgCode(err) != "LC041" {
		t.Fatalf("TRUNCATE must be LC041 for the owner, got %v", err)
	}
	w.ownerExec(`DELETE FROM operating_bases WHERE tenant_id = $1`, t2)
	if n := w.ownerInt(`SELECT count(*) FROM operating_bases WHERE tenant_id = $1`, t2); n != 1 {
		t.Fatal("a DELETE has no policy, even for the owner: the row must remain")
	}
}

// ownerPolicyFixture writes an enabled tenant-rung policy on the owner path. The 0180 policy-write trigger skips the licence
// ceiling only for an APPROVED basis, so a tenant whose only basis row is an owner-path row cannot enable through the trigger:
// that is the intended behaviour, and the fixture therefore disables the trigger for this one insert.
func (w *world) ownerPolicyFixture(tid uuid.UUID, country string) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE operating_country_policies DISABLE TRIGGER operating_country_policies_ceiling`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO operating_country_policies (tenant_id, scope_kind, country_code, state, status, authorization_reference,
			reason_code, policy_version, created_by_actor_type, created_by_actor_id) VALUES ($1, 'tenant', $2, 'enabled', 'active', 'T', 'fixture', 'x', 'system', $3)`,
			tid, country, uuid.New()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE operating_country_policies ENABLE TRIGGER operating_country_policies_ceiling`)
		return err
	}); err != nil {
		w.t.Fatalf("owner policy fixture: %v", err)
	}
}

func TestOperatingBasis_TenantIsolation(t *testing.T) {
	w := newWorld(t)
	a, b := w.tenant("under_platform_licence"), w.tenant("own_licence")
	jid := w.jurisdiction()
	partner := w.licence(jid, "tenant", uuid.Nil)
	w.widen(KindOperatingBasisSet, noLicence(a, nil, jid, 30), "documented_basis")
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", b, nil, partner, bCountry), "partner_basis")

	count := func(tenantScope uuid.UUID, sql string, args ...any) int {
		var n int
		if err := w.rt.WithTenant(context.Background(), tenantScope, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(a, `SELECT count(*) FROM operating_bases`); n != 1 {
		t.Fatalf("tenant A must see exactly its own basis, saw %d", n)
	}
	if n := count(a, `SELECT count(*) FROM operating_bases WHERE tenant_id = $1`, b); n != 0 {
		t.Fatalf("tenant A must not read tenant B's basis, saw %d", n)
	}
	// B's partner licence is readable by B (through its basis) and by nobody else.
	if n := count(b, `SELECT count(*) FROM licences WHERE id = $1`, partner); n != 1 {
		t.Fatal("tenant B must read the licence of its own basis")
	}
	if n := count(a, `SELECT count(*) FROM licences WHERE id = $1`, partner); n != 0 {
		t.Fatal("tenant A must not read tenant B's partner licence")
	}
	// The resolver refuses a mis-scoped call (tenant B's question in tenant A's session) rather than answering.
	err := w.rt.WithTenant(context.Background(), a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{TenantID: b, CountryCode: bCountry, OperationCode: bOp, AsOf: time.Now()})
		return err
	})
	if !errors.Is(err, operatingmarket.ErrTransactionScope) {
		t.Fatalf("a mis-scoped resolution must be ErrTransactionScope, got %v", err)
	}
}

// ---- fail closed on read errors ------------------------------------------------------------------------------------------

type failingQuerier struct {
	operatingmarket.ReadOnlyQuerier
	needle string
}

func (f failingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, f.needle) {
		return nil, errors.New("injected read failure")
	}
	return f.ReadOnlyQuerier.Query(ctx, sql, args...)
}

func TestOperatingBasis_ReadError_FailsClosed(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, w.jurisdiction(), 30), "documented_basis")
	w.enableTenantPolicy(tid, bCountry)
	w.wantOutcome(w.resolveNow(tid, nil, bCountry), operatingmarket.OutcomePermitted, "control")
	err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, failingQuerier{tx, "FROM operating_bases"},
			operatingmarket.Query{TenantID: tid, CountryCode: bCountry, OperationCode: bOp, AsOf: time.Now()})
		if err == nil || res.Permitted() {
			t.Fatalf("a basis read failure must be an error and never a permission: %v %v", res.Outcome(), err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---- audit and decision record ---------------------------------------------------------------------------------------------

func TestOperatingBasis_AuditRows_AndDecisionRecord(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	done := w.widen(KindOperatingBasisSet, noLicence(tid, nil, w.jurisdiction(), 30), "documented_basis")
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = 'operating_market.operating_basis_set' AND metadata->>'tenant_id' = $1 AND metadata->>'reason_code' = 'documented_basis' AND metadata->>'widening_capable' = 'true'`, tid.String()); n != 1 {
		t.Fatalf("the set must write one audit row with its reason code and widening_capable, found %d", n)
	}
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = 'operating_market.operating_basis_withdrawn' AND metadata->>'tenant_id' = $1 AND metadata->>'reason_code' = 'basis_withdrawn' AND metadata->'before' IS NOT NULL`, tid.String()); n != 1 {
		t.Fatalf("the withdrawal must write one audit row with before state, found %d", n)
	}
	// The decision-makers are derivable from the executed request (no personal data is in the row).
	var rec operatingmarket.BasisRecord
	var dec *operatingmarket.BasisDecision
	var id uuid.UUID
	if err := w.ownerQuery(`SELECT id FROM operating_bases WHERE tenant_id = $1 AND change_request_id = $2`, []any{tid, done.Request.ID}, &id); err != nil {
		t.Fatal(err)
	}
	if err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, dec, err = operatingmarket.GetOperatingBasis(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec.ChangeRequestID == nil || *rec.ChangeRequestID != done.Request.ID || dec == nil || len(dec.ApproverIDs) != 2 || dec.RequestedBy != w.req.StaffID {
		t.Fatalf("the decision record must show the requester and two approvers: %+v %+v", rec, dec)
	}
	if rec.CreatedByActorID == nil || *rec.CreatedByActorID != w.ap2.StaffID {
		t.Fatalf("created_by is the executing approver (stamped by the guard): %+v", rec.CreatedByActorID)
	}
	// History is append-only and ordered: the open withdrawal first, then the closed set.
	var recs []operatingmarket.BasisRecord
	var total int
	if err := w.rtPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recs, total, err = operatingmarket.ListOperatingBasesPage(ctx, tx, tid, bCountry, false, 50, 0)
		return err
	}); err != nil || total != 2 || len(recs) != 2 || recs[0].Status != operatingmarket.StatusWithdrawn || recs[1].EffectiveTo == nil {
		t.Fatalf("the history must hold the open withdrawal and the closed set: %v %d %+v", err, total, recs)
	}
}

func (w *world) ownerQuery(sql string, args []any, dst ...any) error {
	w.t.Helper()
	return w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(dst...)
	})
}

// ---- consumers: the brand-level "is the market open" scan (marketgate) honours explicit bases -------------------------------

// The scan pre-filters countries by the tenant's own licence ceiling; a country governed by an explicit basis must survive it,
// otherwise a tenant with a (different) licence could never open a basis-governed country.
func TestOperatingBasis_MarketGateBrandOpen_HonoursBasisGovernedCountries(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	brand := w.brand(tid)
	own := w.licence(w.jurisdiction(), "platform", tid)
	w.setCeiling(own, "MT") // the tenant's own licence permits MT only; no tenant policy for MT is enabled
	partner := w.licence(w.jurisdiction(), "tenant", uuid.Nil)
	w.setCeiling(partner, "DE")
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", tid, nil, partner, "DE"), "partner_licence_basis")
	w.enableTenantPolicy(tid, "DE")

	open := func() bool {
		var ok bool
		if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ok, err = marketgate.BrandOpen(ctx, tx, tid, brand, marketgate.OpDeposit)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !open() {
		t.Fatal("the brand must be open for deposit through the basis-governed country DE")
	}
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: "DE", OperationCode: bOp}, "basis_withdrawn")
	if open() {
		t.Fatal("after the withdrawal no country permits deposit: the brand must be closed (fail closed)")
	}
}

// A basis can exclude products: a query naming a restricted product is denied by the basis, an unrestricted product and a
// product-less query are not.
func TestOperatingBasis_RestrictedProduct_Denied(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	p := noLicence(tid, nil, w.jurisdiction(), 30)
	p.RestrictedProductCodes = []string{"sportsbook"}
	p.RestrictionsNote = sp("sportsbook not covered by this determination")
	w.widen(KindOperatingBasisSet, p, "documented_basis")
	w.enableTenantPolicy(tid, bCountry)
	at := func(product *string) resolved {
		var out resolved
		q := operatingmarket.Query{TenantID: tid, CountryCode: bCountry, OperationCode: bOp, ProductCode: product, AsOf: w.dbNow()}
		if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, q)
			if err != nil {
				return err
			}
			exp, err := operatingmarket.ExplainOperatingCountryPolicy(ctx, tx, q)
			out = resolved{outcome: res.Outcome(), exp: exp}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	w.wantBlocked(at(sp("sportsbook")), operatingmarket.BasisReasonProductRestricted, "restricted product")
	w.wantOutcome(at(sp("casino")), operatingmarket.OutcomePermitted, "an unrestricted product")
	w.wantOutcome(at(nil), operatingmarket.OutcomePermitted, "no product named")
}
