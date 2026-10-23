//go:build integration

// Slice C3 (QA test gaps), part 1: the market-configuration rules that live in the database and the Go service, proven against a
// real migrated PRIVATE database as igaming_runtime (owner pool only for fixtures and reads). Gaps closed here, from
// docs/plans/lic32-e2e-record.md "Not covered":
//
//	(1) licence country CEILING WITHDRAWAL: single actor, DB effect, real resolver effect, audit.
//	(2) BRAND rung enable/disable/withdraw and PRODUCT-SCOPED operation rung, widening needs two distinct Persons, precedence.
//
// Test code only. No production code is changed; nothing here asserts a legal approval of any country.
package licensingchange

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

type c3Market struct {
	tid, lid, jid uuid.UUID
}

// c3NewMarket provisions an ACTIVE tenant bound to a fresh platform licence (owner fixtures).
func (w *world) c3NewMarket() c3Market {
	w.t.Helper()
	tid := w.tenant("under_platform_licence")
	jid := w.jurisdiction()
	return c3Market{tid: tid, jid: jid, lid: w.licence(jid, "platform", tid)}
}

// c3Resolve runs the REAL resolver (the function marketgate calls) for the tenant's own session at the database clock.
func (w *world) c3Resolve(tid uuid.UUID, brand *uuid.UUID, country, op string, product *string) operatingmarket.Outcome {
	w.t.Helper()
	var out operatingmarket.Outcome
	asOf := w.dbNow()
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{
			TenantID: tid, BrandID: brand, CountryCode: country, OperationCode: op, ProductCode: product, AsOf: asOf})
		out = res.Outcome()
		return err
	}); err != nil {
		w.t.Fatalf("resolve %s/%s: %v", country, op, err)
	}
	return out
}

func (w *world) c3Want(got, want operatingmarket.Outcome, why string) {
	w.t.Helper()
	if got != want {
		w.t.Fatalf("%s: resolver outcome %q, want %q", why, got, want)
	}
}

func (w *world) c3Ceilings(lid uuid.UUID, country string) (versions, open int, openStatus, openState string) {
	w.t.Helper()
	versions = w.ownerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2`, lid, country)
	open = w.ownerInt(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2 AND effective_to IS NULL`, lid, country)
	if open == 1 {
		if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status, state FROM licence_country_ceilings WHERE licence_id = $1 AND country_code = $2 AND effective_to IS NULL`, lid, country).Scan(&openStatus, &openState)
		}); err != nil {
			w.t.Fatal(err)
		}
	}
	return
}

func (w *world) c3PolicyRows(tid uuid.UUID, where string, args ...any) int {
	w.t.Helper()
	a := append([]any{tid}, args...)
	return w.ownerTenantInt(tid, `SELECT count(*) FROM operating_country_policies WHERE tenant_id = $1 AND `+where, a...)
}

// ---- (1) ceiling withdrawal ----------------------------------------------------------------------------------------------------

func TestC3_CeilingWithdrawal_NarrowingSingleActor_ClosesTheMarket(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	w.setCeiling(m.lid, "MT")
	w.enableTenantPolicy(m.tid, "MT")
	for _, op := range []string{"registration", "deposit", "wagering", "withdrawal"} {
		w.c3Want(w.c3Resolve(m.tid, nil, "MT", op, nil), operatingmarket.OutcomePermitted, "open market, "+op)
	}
	if v, open, st, state := w.c3Ceilings(m.lid, "MT"); v != 1 || open != 1 || st != "active" || state != "enabled" {
		t.Fatalf("precondition: ceiling versions=%d open=%d %s/%s", v, open, st, state)
	}

	// WITHDRAW (status withdrawn, state disabled): narrowing, so ONE actor, executed in the filing call, zero approvals.
	res := w.mustRequest(w.req, KindLicenceCeilingVersion, LicenceCeilingPayload{LicenceID: m.lid, CountryCode: "MT", State: "disabled", Status: "withdrawn"}, "licence_condition_breach")
	r := res.Request
	if !res.Executed || r.Widening || r.RequiredApprovals != 0 || r.Status != StatusExecuted || r.ExecutedBy == nil || *r.ExecutedBy != w.req.StaffID {
		t.Fatalf("a ceiling withdrawal is single-actor and immediate: executed=%v %+v", res.Executed, r)
	}
	if n := w.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, r.ID); n != 0 {
		t.Fatalf("a narrowing change records no approval, got %d", n)
	}

	// DB effect: append-only, a second version, exactly one open version and it is the withdrawn one; nothing was updated in place.
	if v, open, st, state := w.c3Ceilings(m.lid, "MT"); v != 2 || open != 1 || st != "withdrawn" || state != "disabled" {
		t.Fatalf("after withdrawal: versions=%d open=%d %s/%s, want 2/1 withdrawn/disabled", v, open, st, state)
	}
	// The tenant's own policy row is untouched (the withdrawal narrows the CEILING only) ...
	if n := w.c3PolicyRows(m.tid, `scope_kind = 'tenant' AND country_code = 'MT' AND state = 'enabled' AND effective_to IS NULL`); n != 1 {
		t.Fatalf("the tenant rung must be untouched, found %d open enabled rows", n)
	}
	// ... but the REAL resolver closes the market for every operation (the ceiling is evaluated first).
	for _, op := range []string{"registration", "deposit", "wagering", "withdrawal"} {
		w.c3Want(w.c3Resolve(m.tid, nil, "MT", op, nil), operatingmarket.OutcomeNotPermittedByLicence, "after ceiling withdrawal, "+op)
	}

	// Audit: one 'requested' and one 'executed' for the request (no 'approved'), by the single actor; the domain row carries
	// before/after and the reason code.
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'licensing_change.requested' AND actor_id = $2`, r.ID.String(), w.req.StaffID); n != 1 {
		t.Errorf("audit requested: %d", n)
	}
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'licensing_change.executed' AND actor_id = $2`, r.ID.String(), w.req.StaffID); n != 1 {
		t.Errorf("audit executed: %d", n)
	}
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'licensing_change.approved'`, r.ID.String()); n != 0 {
		t.Errorf("a single-actor change has no approved event, got %d", n)
	}
	vid := res.Outcome["ceiling_version_id"].(string)
	row := w.c3AuditOne("operating_market.licence_ceiling_version_created", vid, uuid.Nil)
	if row.Meta["reason_code"] != "licence_condition_breach" || row.ActorID == nil || *row.ActorID != w.req.StaffID {
		t.Errorf("domain audit actor/reason: %+v", row)
	}
	before, _ := row.Meta["before"].(map[string]any)
	after, _ := row.Meta["after"].(map[string]any)
	if before["status"] != "active" || before["state"] != "enabled" || after["status"] != "withdrawn" || after["state"] != "disabled" {
		t.Errorf("audit before/after: %v -> %v", before, after)
	}

	// Tenant staff can never withdraw (or file) a ceiling; nothing is written.
	co := w.tenantStaff(m.tid, "compliance")
	_, err := w.svc.Request(context.Background(), co, RequestInput{Kind: KindLicenceCeilingVersion, ReasonCode: "x",
		Payload: payload(t, LicenceCeilingPayload{LicenceID: m.lid, CountryCode: "MT", State: "disabled", Status: "withdrawn"})})
	expectErr(t, err, ErrForbidden, "tenant compliance withdraws a ceiling")
	if v, _, _, _ := w.c3Ceilings(m.lid, "MT"); v != 2 {
		t.Fatalf("a refused withdrawal must write nothing, versions=%d", v)
	}

	// A withdrawal of a ceiling that never existed is still single-actor but cannot open anything (no enabled version to narrow):
	// it records a withdrawn row for a country that was never enabled; the market stays closed.
	w.narrow(KindLicenceCeilingVersion, LicenceCeilingPayload{LicenceID: m.lid, CountryCode: "DE", State: "disabled", Status: "withdrawn"}, "licence_condition_breach")
	w.c3Want(w.c3Resolve(m.tid, nil, "DE", "deposit", nil), operatingmarket.OutcomeNotPermittedByLicence, "never-enabled country")

	// RE-ENABLING after a withdrawal is WIDENING again: pending, two distinct Persons, the market stays closed until then.
	en := w.mustRequest(w.req, KindLicenceCeilingVersion, LicenceCeilingPayload{LicenceID: m.lid, CountryCode: "MT", State: "enabled", Status: "active",
		AuthorizationReference: sp("TICKET-REENABLE")}, "market_entry")
	if en.Executed || !en.Request.Widening || en.Request.RequiredApprovals != 2 {
		t.Fatalf("re-enable after withdrawal must be widening: %+v", en.Request)
	}
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", nil), operatingmarket.OutcomeNotPermittedByLicence, "re-enable pending")
	w.mustApprove(w.ap1, en.Request.ID)
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", nil), operatingmarket.OutcomeNotPermittedByLicence, "re-enable, one approval")
	if !w.mustApprove(w.ap2, en.Request.ID).Executed {
		t.Fatal("second distinct Person must execute")
	}
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", nil), operatingmarket.OutcomePermitted, "re-enabled, tenant rung still enabled")
}

// ---- (2) brand rung ------------------------------------------------------------------------------------------------------------

func TestC3_BrandRung_EnableDisableWithdraw_PrecedenceAndTwoPersons(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	bA, bB := w.brand(m.tid), w.brand(m.tid)
	w.setCeiling(m.lid, "MT")
	w.enableTenantPolicy(m.tid, "MT")
	brandA := func(state, status string, ref *string) OperatingPolicyPayload {
		return OperatingPolicyPayload{TenantID: m.tid, Scope: "brand", BrandID: &bA, CountryCode: "MT", State: state, Status: status, AuthorizationReference: ref}
	}
	rows := func() int { return w.c3PolicyRows(m.tid, `scope_kind = 'brand' AND brand_id = $2`, bA) }

	// ENABLE at the brand rung is widening: pending until two DISTINCT Persons approve; the same Person twice does not count.
	en := w.mustRequest(w.req, KindOperatingPolicyVersion, brandA("enabled", "active", sp("TICKET-BRAND-1")), "market_entry")
	if en.Executed || !en.Request.Widening || en.Request.RequiredApprovals != 2 {
		t.Fatalf("brand-rung enable: %+v", en.Request)
	}
	if rows() != 0 {
		t.Fatal("a pending request writes no brand row")
	}
	w.mustApprove(w.ap1, en.Request.ID)
	_, err := w.approve(w.ap1Twin, en.Request.ID)
	expectErr(t, err, ErrAlreadyDecided, "the same Person through a second account")
	_, err = w.approve(w.reqTwin, en.Request.ID)
	expectErr(t, err, ErrSelfApproval, "the requester's Person")
	if rows() != 0 || w.ownerInt(`SELECT count(*) FROM licensing_change_approvals WHERE request_id = $1`, en.Request.ID) != 1 {
		t.Fatal("refused approvals must change nothing")
	}
	if !w.mustApprove(w.ap2, en.Request.ID).Executed || rows() != 1 {
		t.Fatal("the second distinct Person executes the brand-rung enable")
	}
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "deposit", nil), operatingmarket.OutcomePermitted, "brand A enabled")

	// DISABLE at the brand rung is narrowing: single actor, only that brand closes; the sibling brand and the no-brand query inherit.
	dis := w.mustRequest(w.req, KindOperatingPolicyVersion, brandA("disabled", "active", nil), "emergency_stop")
	if !dis.Executed || dis.Request.Widening || dis.Request.RequiredApprovals != 0 {
		t.Fatalf("brand-rung disable: %+v", dis.Request)
	}
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "deposit", nil), operatingmarket.OutcomeDisabledByBrand, "brand A disabled")
	w.c3Want(w.c3Resolve(m.tid, &bB, "MT", "deposit", nil), operatingmarket.OutcomePermitted, "sibling brand inherits the tenant rung")
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", nil), operatingmarket.OutcomePermitted, "no-brand query")

	// WITHDRAW the brand disable: absence inherits, so it WIDENS: a reference is REQUIRED and two Persons approve.
	_, err = w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "market_entry", Payload: payload(t, brandA("disabled", "withdrawn", nil))})
	expectErr(t, err, ErrInvalidInput, "brand withdraw without a reference")
	if n := rows(); n != 2 {
		t.Fatalf("brand rows after refused withdraw: %d, want 2 (enable, disable)", n)
	}
	wd := w.mustRequest(w.req, KindOperatingPolicyVersion, brandA("disabled", "withdrawn", sp("TICKET-BRAND-WD")), "market_entry")
	if wd.Executed || !wd.Request.Widening || wd.Request.RequiredApprovals != 2 {
		t.Fatalf("brand-rung withdraw is widening: %+v", wd.Request)
	}
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "deposit", nil), operatingmarket.OutcomeDisabledByBrand, "withdraw pending: still disabled")
	w.mustApprove(w.ap2, wd.Request.ID)
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "deposit", nil), operatingmarket.OutcomeDisabledByBrand, "withdraw, one approval: still disabled")
	if !w.mustApprove(w.ap3, wd.Request.ID).Executed {
		t.Fatal("withdraw must execute on the second distinct Person")
	}
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "deposit", nil), operatingmarket.OutcomePermitted, "brand withdrawn: inherits again")

	// PRECEDENCE: the tenant rung is terminal; a tenant-rung DISABLE beats a brand-rung ENABLE (and applies to every brand).
	w.widen(KindOperatingPolicyVersion, brandA("enabled", "active", sp("TICKET-BRAND-2")), "market_entry")
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "deposit", nil), operatingmarket.OutcomePermitted, "brand A re-enabled")
	w.narrow(KindOperatingPolicyVersion, OperatingPolicyPayload{TenantID: m.tid, Scope: "tenant", CountryCode: "MT", State: "disabled", Status: "active"}, "emergency_stop")
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "deposit", nil), operatingmarket.OutcomeDisabledByTenant, "tenant disable beats brand enable")
	w.c3Want(w.c3Resolve(m.tid, &bB, "MT", "deposit", nil), operatingmarket.OutcomeDisabledByTenant, "tenant disable applies to the sibling")

	// A brand of ANOTHER tenant is refused (the composite FK / not found) and writes nothing.
	other := w.tenant("under_platform_licence")
	foreign := w.brand(other)
	fp := brandA("disabled", "active", nil)
	fp.BrandID = &foreign
	_, err = w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "x", Payload: payload(t, fp)})
	if err == nil {
		t.Fatal("a foreign brand id must be refused")
	}
	var dom *DomainError
	if !errors.As(err, &dom) && sqlstateOf(err) == "" {
		t.Logf("foreign brand refusal is %T: %v", err, err)
	}
	if n := w.c3PolicyRows(m.tid, `brand_id = $2`, foreign); n != 0 {
		t.Fatalf("a foreign brand must leave no row, found %d", n)
	}
}

// ---- (2b) product-scoped operation rung -----------------------------------------------------------------------------------------

func TestC3_ProductScopedOperationRung(t *testing.T) {
	w := newWorld(t)
	m := w.c3NewMarket()
	bA, bB := w.brand(m.tid), w.brand(m.tid)
	w.setCeiling(m.lid, "MT")
	w.enableTenantPolicy(m.tid, "MT")
	casino, sports := sp("casino"), sp("sportsbook")
	opPol := func(op string, brand *uuid.UUID, product *string, state, status string, ref *string) OperatingPolicyPayload {
		return OperatingPolicyPayload{TenantID: m.tid, Scope: "operation", BrandID: brand, OperationCode: &op, ProductCode: product, CountryCode: "MT",
			State: state, Status: status, AuthorizationReference: ref}
	}

	// DISABLE deposit for the CASINO product only: narrowing, single actor.
	w.narrow(KindOperatingPolicyVersion, opPol("deposit", nil, casino, "disabled", "active", nil), "product_suspension")
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", casino), operatingmarket.OutcomeDisabledByOperation, "casino deposit disabled")
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", sports), operatingmarket.OutcomePermitted, "sportsbook deposit unaffected")
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", nil), operatingmarket.OutcomePermitted, "a product-less query is not blocked by a product-scoped row")
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "withdrawal", casino), operatingmarket.OutcomePermitted, "other operation unaffected")

	// Brand + product scoped disable: only that brand's sportsbook wagering.
	w.narrow(KindOperatingPolicyVersion, opPol("wagering", &bA, sports, "disabled", "active", nil), "product_suspension")
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "wagering", sports), operatingmarket.OutcomeDisabledByOperation, "brand A sportsbook wagering")
	w.c3Want(w.c3Resolve(m.tid, &bB, "MT", "wagering", sports), operatingmarket.OutcomePermitted, "brand B sportsbook wagering")
	w.c3Want(w.c3Resolve(m.tid, &bA, "MT", "wagering", casino), operatingmarket.OutcomePermitted, "brand A casino wagering")

	// ENABLE of a product-scoped row is widening (two Persons) and, per first-disabled-wins, can NEVER unmask a broader disabled row.
	w.narrow(KindOperatingPolicyVersion, opPol("registration", nil, nil, "disabled", "active", nil), "emergency_stop") // operation-wide disable
	en := w.mustRequest(w.req, KindOperatingPolicyVersion, opPol("registration", nil, casino, "enabled", "active", sp("TICKET-PRODUCT")), "market_entry")
	if en.Executed || !en.Request.Widening || en.Request.RequiredApprovals != 2 {
		t.Fatalf("product-scoped enable: %+v", en.Request)
	}
	// The filing is accepted (no precheck for this) and the first Person's approval is recorded, but the DATABASE refuses the final
	// approval's execution: a more-specific operation/product row may only narrow while a broader in-force row is disabled.
	w.mustApprove(w.ap1, en.Request.ID)
	_, err := w.approve(w.ap2, en.Request.ID)
	var dom *DomainError
	if !errors.As(err, &dom) {
		t.Fatalf("a product-scoped enable under a broader disabled row must be refused at execution by the domain/database, got %v", err)
	}
	if w.status(en.Request.ID) != StatusRequested || w.c3ApprovalCount(en.Request.ID) != 1 {
		t.Fatalf("refused execution must roll back the final approval: %s %d", w.status(en.Request.ID), w.c3ApprovalCount(en.Request.ID))
	}
	if n := w.c3PolicyRows(m.tid, `operation_code = 'registration' AND product_code = 'casino'`); n != 0 {
		t.Fatalf("no row may be written, found %d", n)
	}
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "registration", casino), operatingmarket.OutcomeDisabledByOperation, "a more specific ENABLED row never unmasks the broader disabled row")
	_, err = w.svc.Cancel(context.Background(), w.req, en.Request.ID)
	mustNotErr(t, err, "requester cancels the unexecutable request")

	// WITHDRAW the casino-deposit disable: widening, a reference is required, two Persons, resolver reopens only at the end.
	_, err = w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "market_entry",
		Payload: payload(t, opPol("deposit", nil, casino, "disabled", "withdrawn", nil))})
	expectErr(t, err, ErrInvalidInput, "product-scoped withdraw without a reference")
	wd := w.mustRequest(w.req, KindOperatingPolicyVersion, opPol("deposit", nil, casino, "disabled", "withdrawn", sp("TICKET-PRODUCT-WD")), "market_entry")
	if wd.Executed || !wd.Request.Widening || wd.Request.RequiredApprovals != 2 {
		t.Fatalf("product-scoped withdraw: %+v", wd.Request)
	}
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", casino), operatingmarket.OutcomeDisabledByOperation, "withdraw pending")
	w.mustApprove(w.ap1, wd.Request.ID)
	_, err = w.approve(w.ap1Twin, wd.Request.ID)
	expectErr(t, err, ErrAlreadyDecided, "same Person cannot supply the second approval")
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", casino), operatingmarket.OutcomeDisabledByOperation, "one Person only")
	if !w.mustApprove(w.ap3, wd.Request.ID).Executed {
		t.Fatal("must execute")
	}
	w.c3Want(w.c3Resolve(m.tid, nil, "MT", "deposit", casino), operatingmarket.OutcomePermitted, "withdrawn: inherits")

	// An unknown product code is refused and leaves no row.
	bogus := sp("not_a_product")
	before := w.c3PolicyRows(m.tid, `product_code = 'not_a_product'`)
	if _, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingPolicyVersion, ReasonCode: "x",
		Payload: payload(t, opPol("deposit", nil, bogus, "disabled", "active", nil))}); err == nil {
		t.Fatal("an unknown product code must be refused")
	}
	if after := w.c3PolicyRows(m.tid, `product_code = 'not_a_product'`); after != before || after != 0 {
		t.Fatalf("unknown product wrote %d rows", after)
	}
}
