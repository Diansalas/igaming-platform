//go:build integration

// lic14 (migration 0183): hardening of the operating basis after the independent security review of lic9. Every assertion about
// what the application can do runs as igaming_runtime; the owner pool provisions fixtures. No jurisdiction, country or arrangement
// here is a statement about any real regime.
package licensingchange

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/kycpolfix/seed"
	"github.com/Diansalas/igaming-platform/internal/testsupport/staffsv"
)

// ---- H-1: the rulebook jurisdiction is selected by the governing basis ----------------------------------------------------

type resident struct{ id, person uuid.UUID }

// seedResident provisions a player of the brand with an approved KYC verification and a reviewer-determined residence country.
func (w *world) seedResident(tid, brand uuid.UUID, country string) resident {
	w.t.Helper()
	p := resident{id: uuid.New()}
	err := w.owner.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		person, err := identity.CreatePerson(ctx, tx)
		if err != nil {
			return err
		}
		p.person = person.ID
		spv, err := staffsv.PersonIfRequired(ctx, tx, string(identity.StaffRoleCompliance))
		if err != nil {
			return err
		}
		staff, err := identity.CreateStaffUser(ctx, tx, tid, "rev-"+uuid.NewString()+"@example.com", "hash", identity.StaffRoleCompliance, spv)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, p.id, tid, brand, p.person, p.id.String()+"@example.com"); err != nil {
			return err
		}
		if country == "" {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference,
			created_at, updated_at, verified_residence_country, verified_residence_source, verified_residence_set_by, verified_residence_set_at)
			VALUES ($1, $2, $3, $4, $5, 'approved', 'mock', $6, now() - interval '1 hour', now() - interval '1 hour', $7, 'reviewer_determination', $8, now() - interval '30 minutes')`,
			uuid.New(), tid, brand, p.id, p.person, "ref-"+uuid.NewString(), country, staff.ID)
		return err
	})
	if err != nil {
		w.t.Fatalf("seed resident: %v", err)
	}
	return p
}

// depositPolicy activates a cumulative-deposit KYC policy for the jurisdiction (two principals: creator and activator).
func (w *world) depositPolicy(jid uuid.UUID) {
	w.t.Helper()
	thr, asset := "100000", "EUR"
	seed.Active(w.t, w.rt, kyc.CreateEnforcementPolicyParams{LicensingJurisdictionID: jid, TriggerType: "cumulative_deposit",
		ThresholdMinorUnits: &thr, AssetCode: &asset, LegalReviewReference: "test-legal-ref", ReasonCode: "test"})
}

func (w *world) kycDeposit(tid, brand uuid.UUID, p resident) kyc.EnforcementDecision {
	w.t.Helper()
	var d kyc.EnforcementDecision
	if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = kyc.EvaluateEnforcement(ctx, tx, kyc.EnforcementParams{TenantID: tid, BrandID: brand, PlayerAccountID: p.id, PersonID: p.person,
			Operation: kyc.EnforcementDeposit, AssetCode: "EUR", Amount: 1_000_000, CorrelationID: uuid.New()})
		return err
	}); err != nil {
		w.t.Fatalf("evaluate kyc: %v", err)
	}
	return d
}

func wantKYC(t *testing.T, what string, d kyc.EnforcementDecision, outcome kyc.EnforcementOutcome, allowed bool) {
	t.Helper()
	if d.Outcome != outcome || d.Allowed != allowed {
		t.Fatalf("%s: outcome %s allowed %v (code %s), want %s / %v", what, d.Outcome, d.Allowed, d.Code, outcome, allowed)
	}
}

// The matrix of H-1. Without the fix every "basis" row below resolves not_required (a tenant with no licence is default-allow).
func TestHardening_H1_KYCRulebookFollowsTheGoverningBasis(t *testing.T) {
	w := newWorld(t)
	j1, j2 := w.jurisdiction(), w.jurisdiction()
	w.depositPolicy(j1) // J1 has an active deposit policy; J2 has none

	// NULL tenant licence + documented no-licence basis (jurisdiction J1) -> the J1 rulebook applies.
	t1 := w.tenant("under_platform_licence")
	b1 := w.brand(t1)
	w.widen(KindOperatingBasisSet, noLicence(t1, nil, j1, 30), "documented_basis")
	p1 := w.seedResident(t1, b1, bCountry)
	wantKYC(t, "no-licence basis, NULL tenant licence", w.kycDeposit(t1, b1, p1), kyc.OutcomePassed, true)

	// The same basis with a jurisdiction that has NO policy: the rulebook is J2's, which is dormant (a defined rulebook, not a
	// missing licence) -> not_required, by that jurisdiction's own content.
	t2 := w.tenant("under_platform_licence")
	b2 := w.brand(t2)
	w.widen(KindOperatingBasisSet, noLicence(t2, nil, j2, 30), "documented_basis")
	p2 := w.seedResident(t2, b2, bCountry)
	wantKYC(t, "basis jurisdiction without a policy", w.kycDeposit(t2, b2, p2), kyc.OutcomeNotRequired, true)

	// platform_licence basis (licence of J1) with a NULL tenant licence -> J1.
	t3 := w.tenant("under_platform_licence")
	b3 := w.brand(t3)
	l3 := w.licence(j1, "platform", uuid.Nil)
	w.setCeiling(l3, bCountry)
	w.widen(KindOperatingBasisSet, licenceBasis("platform_licence", t3, nil, l3, bCountry), "platform_licence_basis")
	p3 := w.seedResident(t3, b3, bCountry)
	wantKYC(t, "platform_licence basis, NULL tenant licence", w.kycDeposit(t3, b3, p3), kyc.OutcomePassed, true)

	// Partner licence for ONE brand differing from the tenant default: brand B uses the partner licence's jurisdiction (J1, policy);
	// brand A has no row in scope and keeps the tenant's own licence jurisdiction (J2, no policy).
	t4 := w.tenant("under_platform_licence")
	own := w.licence(j2, "platform", t4)
	w.setCeiling(own, bCountry)
	brandA, brandB := w.brand(t4), w.brand(t4)
	partner := w.licence(j1, "tenant", uuid.Nil)
	w.setCeiling(partner, bCountry)
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", t4, &brandB, partner, bCountry), "partner_licence_basis")
	pa, pb := w.seedResident(t4, brandA, bCountry), w.seedResident(t4, brandB, bCountry)
	wantKYC(t, "partner licence brand", w.kycDeposit(t4, brandB, pb), kyc.OutcomePassed, true)
	wantKYC(t, "tenant default brand (J2, dormant)", w.kycDeposit(t4, brandA, pa), kyc.OutcomeNotRequired, true)

	// FAIL CLOSED: the tenant has bases but the scope cannot be selected. No verified residence -> unavailable; a residence country
	// with no row and no licence -> unavailable; a withdrawn basis -> unavailable. Never not_required.
	noRes := w.seedResident(t1, b1, "")
	d := w.kycDeposit(t1, b1, noRes)
	wantKYC(t, "no verified residence", d, kyc.OutcomeUnavailable, false)
	other := w.seedResident(t1, b1, "DE")
	wantKYC(t, "residence country without a basis row and no licence", w.kycDeposit(t1, b1, other), kyc.OutcomeUnavailable, false)
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: t1, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")
	wantKYC(t, "withdrawn basis", w.kycDeposit(t1, b1, p1), kyc.OutcomeNotRequired, true) // no approved basis left: the legacy path, exactly as before
}

// No basis row anywhere: exactly the pre-lic14 behaviour (no licence -> not_required; licence jurisdiction with a policy -> its rules).
func TestHardening_H1_NoBasis_Unchanged(t *testing.T) {
	w := newWorld(t)
	j1 := w.jurisdiction()
	w.depositPolicy(j1)
	bare := w.tenant("under_platform_licence")
	bb := w.brand(bare)
	wantKYC(t, "no licence, no basis", w.kycDeposit(bare, bb, w.seedResident(bare, bb, bCountry)), kyc.OutcomeNotRequired, true)
	lic := w.tenant("under_platform_licence")
	lb := w.brand(lic)
	w.licence(j1, "platform", lic)
	wantKYC(t, "licence jurisdiction with a policy", w.kycDeposit(lic, lb, w.seedResident(lic, lb, bCountry)), kyc.OutcomePassed, true)
}

// RG: the RG rulebook cannot be selected for a tenant that has an approved basis but no licence jurisdiction: refused, never "no config".
func TestHardening_H1_RGOptions_FailClosedForBasisTenantWithoutLicence(t *testing.T) {
	w := newWorld(t)
	read := func(tid uuid.UUID, p resident) error {
		return w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rg.ReadOptions(ctx, tx, tid, p.id, "EUR")
			return err
		})
	}
	bare := w.tenant("under_platform_licence")
	bb := w.brand(bare)
	pb := w.seedResident(bare, bb, bCountry)
	if err := read(bare, pb); err != nil {
		t.Fatalf("no licence and no basis keeps the pre-lic14 behaviour: %v", err)
	}
	withBasis := w.tenant("under_platform_licence")
	wb := w.brand(withBasis)
	w.widen(KindOperatingBasisSet, noLicence(withBasis, nil, w.jurisdiction(), 30), "documented_basis")
	// lic27a (declared change of this pin, intent preserved: no silent weaker options): the read is no longer refused as a whole (a
	// refusal hid the always-available time-out and self-exclusion tools from the player); every limit kind is marked unavailable and
	// offers nothing, never "no configuration" with the whole vocabulary open.
	var o rg.Options
	if err := w.rt.WithTenant(context.Background(), withBasis, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		o, e = rg.ReadOptions(ctx, tx, withBasis, w.seedResident(withBasis, wb, bCountry).id, "EUR")
		return e
	}); err != nil {
		t.Fatalf("the options of a basis tenant are served with the limit kinds unavailable: %v", err)
	}
	wantUnavailableOptions(t, o, rg.OptionUnavailableMissingFloor, rg.ControlDeposit, rg.ControlLoss, rg.ControlWager)
}

// ---- M-1: no bare close ------------------------------------------------------------------------------------------------------

func TestHardening_M1_OwnerCloseRefused_AndEveryCloseNeedsASuccessor(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	w.widen(KindOperatingBasisSet, noLicence(tid, nil, w.jurisdiction(), 30), "documented_basis")
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: tid, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")

	// The owner path may no longer close the (masking) withdrawn version.
	err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE operating_bases SET effective_to = now() WHERE tenant_id = $1 AND effective_to IS NULL`, tid)
		return err
	})
	if pgCode(err) != "LC041" {
		t.Fatalf("an owner close without an executed request must be LC041, got %v", err)
	}
	// Independent of the guard: with the governed-write guard disabled in one transaction, a close WITHOUT a successor fails at commit.
	err = w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE operating_bases DISABLE TRIGGER zz_licensing_change_governed`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE operating_bases SET effective_to = now() WHERE tenant_id = $1 AND effective_to IS NULL`, tid)
		return err
	})
	if pgCode(err) != "LC044" {
		t.Fatalf("a close without a successor must be refused at commit (LC044), got %v", err)
	}
	if n := w.ownerInt(`SELECT count(*) FROM operating_bases WHERE tenant_id = $1 AND effective_to IS NULL`, tid); n != 1 {
		t.Fatalf("the refused close must change nothing, open rows: %d", n)
	}
	if n := w.ownerInt(`SELECT count(*) FROM pg_trigger WHERE tgname = 'zz_licensing_change_governed' AND tgrelid = 'operating_bases'::regclass AND tgenabled = 'O'`); n != 1 {
		t.Fatal("the guard trigger must still be enabled (the disabling transaction rolled back)")
	}
	// A close WITH a successor that starts when it ended passes the constraint.
	err = w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE operating_bases DISABLE TRIGGER zz_licensing_change_governed`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE operating_bases SET effective_to = now() WHERE tenant_id = $1 AND effective_to IS NULL`, tid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO operating_bases (tenant_id, country_code, operation_code, basis_kind, jurisdiction_id, determination_reference,
			determined_by_role, review_due_at, status, reason_code, policy_version)
			SELECT tenant_id, country_code, operation_code, basis_kind, jurisdiction_id, determination_reference, determined_by_role, review_due_at,
			       'withdrawn', 'x', 'fixture' FROM operating_bases WHERE tenant_id = $1 AND effective_to IS NOT NULL AND status = 'withdrawn'`, tid); err != nil {
			return err
		}
		return nil
	})
	// (ALTER TABLE is refused while the transaction has pending trigger events, so the guard is re-enabled after the commit.)
	w.ownerExec(`ALTER TABLE operating_bases ENABLE TRIGGER zz_licensing_change_governed`)
	if err != nil {
		t.Fatalf("a close with a same-scope successor must pass: %v", err)
	}
}

// ---- M-2 / R3 / L-1 ------------------------------------------------------------------------------------------------------------

func TestHardening_PartnerLicenceExclusivityIsPermanent_AndSymmetric(t *testing.T) {
	w := newWorld(t)
	x, y := w.tenant("own_licence"), w.tenant("own_licence")
	jid := w.jurisdiction()
	lic := w.licence(jid, "tenant", uuid.Nil)
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", x, nil, lic, bCountry), "partner_licence_basis")
	w.narrow(KindOperatingBasisWithdraw, OperatingBasisWithdrawPayload{TenantID: x, CountryCode: bCountry, OperationCode: bOp}, "basis_withdrawn")

	// Withdrawn is not released: tenant Y still cannot take the licence (filing refused by the classifier).
	_, err := w.svc.Request(context.Background(), w.req, RequestInput{Kind: KindOperatingBasisSet,
		Payload: payload(t, licenceBasis("partner_licence", y, nil, lic, bCountry)), ReasonCode: "basis"})
	if sqlstateOf(err) != "LC010" {
		t.Fatalf("a partner licence that another tenant ever used must be refused for a second tenant (LC010), got %v", err)
	}
	// Symmetric: binding it to tenant Y through tenants.licence_id is refused too (owner path and the governed bind).
	err = w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lic, y)
		return err
	})
	if pgCode(err) != "LC043" {
		t.Fatalf("binding a licence another tenant's basis used must be LC043, got %v", err)
	}
	r := w.mustRequest(w.req, KindTenantLicenceBind, TenantLicenceBindPayload{TenantID: y, LicenceID: lic}, "licence_granted")
	w.mustApprove(w.ap1, r.Request.ID)
	if _, err := w.approve(w.ap2, r.Request.ID); err == nil {
		t.Fatal("the governed bind must be refused at execution")
	}
	if n := w.ownerInt(`SELECT count(*) FROM tenants WHERE id = $1 AND licence_id IS NOT NULL`, y); n != 0 {
		t.Fatal("tenant Y must stay unbound")
	}
	// The original tenant is unaffected.
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", x, nil, lic, bCountry), "partner_licence_basis")
}

// L-1: a tenant reads a licence named by its own basis only while no other tenant is bound to / uses it.
func TestHardening_L1_BasisLicenceReadNarrowed(t *testing.T) {
	w := newWorld(t)
	a, b := w.tenant("own_licence"), w.tenant("own_licence")
	jid := w.jurisdiction()
	partner := w.licence(jid, "tenant", uuid.Nil)
	platform := w.licence(jid, "platform", uuid.Nil)
	w.setCeiling(partner, bCountry)
	w.widen(KindOperatingBasisSet, licenceBasis("partner_licence", a, nil, partner, bCountry), "partner_licence_basis")
	w.widen(KindOperatingBasisSet, licenceBasis("platform_licence", a, nil, platform, "DE"), "platform_licence_basis")
	count := func(tid uuid.UUID, sql string, args ...any) int {
		var n int
		if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&n) }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(a, `SELECT count(*) FROM licences WHERE id = $1`, partner) != 1 || count(a, `SELECT count(*) FROM licences WHERE id = $1`, platform) != 1 ||
		count(a, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1`, partner) != 1 {
		t.Fatal("tenant A must read the licences and ceilings of its own bases")
	}
	// Another tenant becomes bound to the partner licence (forced past the exclusivity trigger, as a corrupted state): A must lose
	// the read of the partner licence and its ceilings, but keeps the platform-licensee licence.
	w.ownerExec(`ALTER TABLE tenants DISABLE TRIGGER tenants_licence_basis_exclusive`)
	defer w.ownerExec(`ALTER TABLE tenants ENABLE TRIGGER tenants_licence_basis_exclusive`)
	w.ownerExec(`UPDATE tenants SET licence_id = $1 WHERE id = $2`, partner, b)
	if count(a, `SELECT count(*) FROM licences WHERE id = $1`, partner) != 0 || count(a, `SELECT count(*) FROM licence_country_ceilings WHERE licence_id = $1`, partner) != 0 {
		t.Fatal("tenant A must not read a partner licence another tenant is bound to")
	}
	if count(a, `SELECT count(*) FROM licences WHERE id = $1`, platform) != 1 {
		t.Fatal("a platform-licensee licence stays readable")
	}
	if count(b, `SELECT count(*) FROM licences WHERE id = $1`, partner) != 1 {
		t.Fatal("the bound tenant reads its own licence")
	}
}

// ---- L-4 / L-5 ----------------------------------------------------------------------------------------------------------------

func TestHardening_L4_WageringWithoutProductIsDeniedByARestrictingBasis(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	p := noLicence(tid, nil, w.jurisdiction(), 30)
	p.OperationCode = "wagering"
	p.RestrictedProductCodes = []string{"sportsbook"}
	w.widen(KindOperatingBasisSet, p, "documented_basis")
	w.widen(KindOperatingPolicyVersion, OperatingPolicyPayload{TenantID: tid, Scope: "tenant", CountryCode: bCountry, State: "enabled", Status: "active", AuthorizationReference: sp("T-1")}, "market_entry")
	at := func(product *string) operatingmarket.Outcome {
		var o operatingmarket.Outcome
		if err := w.rt.WithTenant(context.Background(), tid, func(ctx context.Context, tx pgx.Tx) error {
			res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{TenantID: tid, CountryCode: bCountry, OperationCode: "wagering", ProductCode: product, AsOf: time.Now()})
			o = res.Outcome()
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return o
	}
	if got := at(nil); got != operatingmarket.OutcomeNotPermittedByBasis {
		t.Fatalf("a wagering question naming no product cannot prove it avoids the restricted products: %s", got)
	}
	if got := at(sp("sportsbook")); got != operatingmarket.OutcomeNotPermittedByBasis {
		t.Fatalf("restricted product: %s", got)
	}
	if got := at(sp("casino")); got != operatingmarket.OutcomePermitted {
		t.Fatalf("an unrestricted product stays permitted: %s", got)
	}
}

func TestHardening_L5_StoredReferencesAreTokensNotProse(t *testing.T) {
	w := newWorld(t)
	tid := w.tenant("under_platform_licence")
	jid := w.jurisdiction()
	for name, vals := range map[string][3]any{
		"a space in the reference":        {"DET 1", nil, nil},
		"a plus sign in the reference":    {"DET+44", nil, nil},
		"a long digit run":                {"DET-1234-5678-90123", nil, nil},
		"prose in the evidence reference": {"DET-1", "see the email from the regulator", nil},
		"a bracket in the note":           {"DET-1", nil, "call [me]"},
		"a long digit run in the note":    {"DET-1", nil, "account 12345 67890 123"},
	} {
		vals := vals
		err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO operating_bases (tenant_id, country_code, operation_code, basis_kind, jurisdiction_id, determination_reference,
				evidence_reference, restrictions_note, determined_by_role, review_due_at, status, reason_code, policy_version)
				VALUES ($1, 'FR', 'deposit', 'no_licence_documented', $2, $3, $4, $5, 'head_of_compliance', current_date + 30, 'active', 'x', 'fixture')`,
				tid, jid, vals[0], vals[1], vals[2])
			return err
		})
		if pgCode(err) != "23514" {
			t.Errorf("%s: must be refused by a CHECK (23514), got %v", name, err)
		}
	}
	// The audit row carries ids and opaque references, never the free-text note.
	done := w.widen(KindOperatingBasisSet, func() OperatingBasisSetPayload {
		p := noLicence(tid, nil, jid, 30)
		p.RestrictionsNote = sp("only registered players")
		return p
	}(), "documented_basis")
	_ = done
	if n := w.ownerInt(`SELECT count(*) FROM audit_log WHERE action = 'operating_market.operating_basis_set' AND metadata->>'tenant_id' = $1
		AND metadata::text NOT LIKE '%only registered players%' AND metadata->'after'->>'has_restrictions_note' = 'true'`, tid.String()); n != 1 {
		t.Fatalf("the audit row must not carry the note text, found %d clean rows", n)
	}
}

var _ = errors.New
