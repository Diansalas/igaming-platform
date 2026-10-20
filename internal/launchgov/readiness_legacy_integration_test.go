//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

// Lic7: R-RISK-RULES-LEGACY (ledger-finance condition C1) and R-KYC-POLICY (audit G9). Every readiness read below runs through
// the launchgov Service over the RUNTIME pool (igaming_runtime, non-BYPASSRLS, asserted by newWorld), so the reads are proven
// RLS-correct inside the launchgov read-only tenant transaction. The risk tests use a private scratch database because
// platform-wide risk rules are visible to every tenant and the shared database may hold other packages' legacy rules.

func (w *world) readinessOf(sub launchgov.Subject, jid uuid.UUID) launchgov.Readiness {
	w.t.Helper()
	r, err := w.svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: sub, Action: launchgov.ActionActivate,
		Licensing: &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional,
			DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel"},
		JurisdictionIDs: []uuid.UUID{jid}})
	if err != nil {
		w.t.Fatalf("readiness: %v", err)
	}
	return r
}

func lic7CheckOf(t *testing.T, r launchgov.Readiness, code string) launchgov.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Code == code {
			return c
		}
	}
	t.Fatalf("the readiness has no %s check", code)
	return launchgov.Check{}
}

func wantLic7(t *testing.T, what string, c launchgov.Check, result, detail string) {
	t.Helper()
	if c.Result != result || c.DetailCode != detail {
		t.Fatalf("%s: want %s/%s, got %s/%s", what, result, detail, c.Result, c.DetailCode)
	}
}

// legacyRule seeds a stored rule through the test-only bypass of the lic3 authoring refusals (as the risk package's own
// fixtures do). tenant nil = platform-wide.
func (w *world) legacyRule(tenant *uuid.UUID, op risk.Operation, jurisdictionCode string) risk.Rule {
	w.t.Helper()
	exp := int16(2)
	p := risk.CreateRuleParams{
		TenantID: tenant, JurisdictionCode: jurisdictionCode, Operation: op,
		LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 100, ThresholdExponent: &exp,
		CreatedByActorType: "staff", CreatedByActorID: uuid.New(), TestOnlyBypassAuthoringRefusals: true,
	}
	var r risk.Rule
	run := func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = risk.CreateRule(ctx, tx, p)
		return err
	}
	var err error
	if tenant == nil {
		err = w.owner.WithoutTenant(context.Background(), run)
	} else {
		err = w.owner.WithTenant(context.Background(), *tenant, run)
	}
	if err != nil {
		w.t.Fatalf("seed legacy rule: %v", err)
	}
	return r
}

func (w *world) ruleSQL(tenant *uuid.UUID, sql string, args ...any) {
	w.t.Helper()
	run := func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }
	var err error
	if tenant == nil {
		err = w.owner.WithoutTenant(context.Background(), run)
	} else {
		err = w.owner.WithTenant(context.Background(), *tenant, run)
	}
	if err != nil {
		w.t.Fatalf("rule update: %v", err)
	}
}

func TestReadiness_RiskRulesLegacy_PassFailTenantIsolationAndPlatformScope(t *testing.T) {
	w := newScratchWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	ja, jb := w.jurisdiction(a), w.jurisdiction(b)
	subA, subB := launchgov.Subject{TenantID: a}, launchgov.Subject{TenantID: b}

	// pass arm: nothing stored; a plain (non-jurisdiction, non-deposit) rule is not legacy
	wantLic7(t, "no rules", lic7CheckOf(t, w.readinessOf(subA, ja), launchgov.CheckRiskRulesLegacy), launchgov.ResultPass, "ok")
	w.legacyRule(&a, risk.OperationWithdrawal, "")
	r := w.readinessOf(subA, ja)
	wantLic7(t, "an ordinary withdrawal rule", lic7CheckOf(t, r, launchgov.CheckRiskRulesLegacy), launchgov.ResultPass, "ok")
	if len(r.Refs) != 0 || r.Info[launchgov.InfoRiskLegacyJurisdictionScoped] != 0 {
		t.Fatalf("a pass carries no ids: %v %v", r.Refs, r.Info)
	}

	// fail arm (a): jurisdiction-scoped withdrawal rule of tenant A
	jr := w.legacyRule(&a, risk.OperationWithdrawal, w.jurCode(ja))
	r = w.readinessOf(subA, ja)
	wantLic7(t, "jurisdiction-scoped rule", lic7CheckOf(t, r, launchgov.CheckRiskRulesLegacy), launchgov.ResultFail, "jurisdiction_scoped_rules_present")
	if r.Info[launchgov.InfoRiskLegacyJurisdictionScoped] != 1 || r.Info[launchgov.InfoRiskLegacyDepositOperation] != 0 {
		t.Fatalf("counts: %v", r.Info)
	}
	if ids := r.Refs[launchgov.RefsRiskLegacyJurisdictionScoped]; len(ids) != 1 || ids[0] != jr.ID.String() {
		t.Fatalf("refs: %v", r.Refs)
	}

	// tenant isolation: tenant B does not see A's rules
	rb := w.readinessOf(subB, jb)
	wantLic7(t, "another tenant's rules do not count", lic7CheckOf(t, rb, launchgov.CheckRiskRulesLegacy), launchgov.ResultPass, "ok")
	if len(rb.Refs) != 0 {
		t.Fatalf("tenant B saw tenant A's rule ids: %v", rb.Refs)
	}

	// fail arm (b): a deposit rule, and both kinds together
	dr := w.legacyRule(&a, risk.OperationDeposit, "")
	r = w.readinessOf(subA, ja)
	wantLic7(t, "both kinds", lic7CheckOf(t, r, launchgov.CheckRiskRulesLegacy), launchgov.ResultFail, "both_present")
	if r.Info[launchgov.InfoRiskLegacyDepositOperation] != 1 || len(r.Refs[launchgov.RefsRiskLegacyDepositOperation]) != 1 || r.Refs[launchgov.RefsRiskLegacyDepositOperation][0] != dr.ID.String() {
		t.Fatalf("deposit counts/refs: %v %v", r.Info, r.Refs)
	}

	// a disabled rule does not count (DisableRule is the remedy); so does an expired one
	w.ruleSQL(&a, `UPDATE risk_rules SET status = 'disabled' WHERE id = $1`, jr.ID)
	r = w.readinessOf(subA, ja)
	wantLic7(t, "jurisdiction rule disabled", lic7CheckOf(t, r, launchgov.CheckRiskRulesLegacy), launchgov.ResultFail, "deposit_rules_present")
	w.ruleSQL(&a, `UPDATE risk_rules SET effective_until = now() WHERE id = $1`, dr.ID)
	wantLic7(t, "deposit rule expired", lic7CheckOf(t, w.readinessOf(subA, ja), launchgov.CheckRiskRulesLegacy), launchgov.ResultPass, "ok")

	// platform-wide rules apply to every tenant
	pr := w.legacyRule(nil, risk.OperationCasinoBet, w.jurCode(ja))
	for name, c := range map[string]struct {
		sub launchgov.Subject
		jid uuid.UUID
	}{"A": {subA, ja}, "B": {subB, jb}} {
		r := w.readinessOf(c.sub, c.jid)
		wantLic7(t, "platform-wide rule, tenant "+name, lic7CheckOf(t, r, launchgov.CheckRiskRulesLegacy), launchgov.ResultFail, "jurisdiction_scoped_rules_present")
		if ids := r.Refs[launchgov.RefsRiskLegacyJurisdictionScoped]; len(ids) != 1 || ids[0] != pr.ID.String() {
			t.Fatalf("tenant %s refs: %v", name, r.Refs)
		}
	}
	// a brand subject of tenant A sees the same rules (the risk engine's missing-scope gate is brand-agnostic)
	brand := w.activeBrand(a)
	wantLic7(t, "brand subject", lic7CheckOf(t, w.readinessOf(launchgov.Subject{TenantID: a, BrandID: &brand}, ja), launchgov.CheckRiskRulesLegacy),
		launchgov.ResultFail, "jurisdiction_scoped_rules_present")
}

func TestReadiness_RiskRulesLegacy_ChangesTheHashAndIsAcknowledgeableNotHard(t *testing.T) {
	w := newScratchWorld(t)
	a := w.activeTenant()
	ja := w.jurisdiction(a)
	brand := w.pendingBrand(a) // a request needs a legal transition: pending_launch -> active
	sub := launchgov.Subject{TenantID: a, BrandID: &brand}
	before := w.readinessOf(sub, ja)
	if before.Version != launchgov.SnapshotVersion || launchgov.SnapshotVersion != 7 {
		t.Fatalf("snapshot version %d", before.Version)
	}
	w.legacyRule(&a, risk.OperationDeposit, "")
	after := w.readinessOf(sub, ja)
	if before.Hash() == after.Hash() {
		t.Fatal("a legacy rule must change the readiness hash")
	}

	in := w.activation(sub, ja)
	has := func(codes []string, c string) bool {
		for _, x := range codes {
			if x == c {
				return true
			}
		}
		return false
	}
	if !has(in.AcknowledgedCheckCodes, launchgov.CheckRiskRulesLegacy) || !has(in.AcknowledgedCheckCodes, launchgov.CheckKYCPolicy) {
		t.Fatalf("both failing items are in the non-passing set: %v", in.AcknowledgedCheckCodes)
	}
	// not acknowledged: refused as unacknowledged (H-5), not as a failed hard precondition
	missing := in
	missing.AcknowledgedCheckCodes = nil
	for _, c := range in.AcknowledgedCheckCodes {
		if c != launchgov.CheckRiskRulesLegacy {
			missing.AcknowledgedCheckCodes = append(missing.AcknowledgedCheckCodes, c)
		}
	}
	if _, err := w.svc.Request(context.Background(), w.req, missing); !errors.Is(err, launchgov.ErrUnacknowledged) {
		t.Fatalf("a request that skips R-RISK-RULES-LEGACY must be refused as unacknowledged, got %v", err)
	}
	// acknowledged: filed (never a hard precondition)
	w.mustRequest(w.req, in)
}

func (w *world) kycPolicy(jid uuid.UUID, trigger string, playOp *string) uuid.UUID {
	w.t.Helper()
	p := kyc.CreateEnforcementPolicyParams{LicensingJurisdictionID: jid, TriggerType: trigger, PlayOperation: playOp,
		LegalReviewReference: "lic7-test-ref", ReasonCode: "lic7_test"}
	if trigger == "cumulative_deposit" {
		th, asset := "100000", "EUR"
		p.ThresholdMinorUnits, p.AssetCode = &th, &asset
	}
	var id uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = kyc.CreateEnforcementPolicy(ctx, tx, p)
		return err
	}); err != nil {
		w.t.Fatalf("create kyc policy: %v", err)
	}
	if err := w.owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return kyc.ActivateEnforcementPolicy(ctx, tx, id)
	}); err != nil {
		w.t.Fatalf("activate kyc policy: %v", err)
	}
	return id
}

func TestReadiness_KYCPolicy_DormantUntilAnActivePolicyExistsForTheLicenceJurisdiction(t *testing.T) {
	w := newWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	ja, jb := w.jurisdiction(a), w.jurisdiction(b)
	subA, subB := launchgov.Subject{TenantID: a}, launchgov.Subject{TenantID: b}

	// no licence bound: no policy can apply at all
	wantLic7(t, "no licence", lic7CheckOf(t, w.readinessOf(subA, ja), launchgov.CheckKYCPolicy), launchgov.ResultFail, "no_licence_jurisdiction")

	// licensed, zero policies: dormant
	w.licence(a, ja, "platform", "active")
	w.licence(b, jb, "platform", "active")
	r := w.readinessOf(subA, ja)
	wantLic7(t, "zero policies", lic7CheckOf(t, r, launchgov.CheckKYCPolicy), launchgov.ResultFail, "no_active_policy")
	if r.Info[launchgov.InfoKYCActiveDeposit] != 0 || r.Info[launchgov.InfoKYCActiveCasinoPlay] != 0 || r.Info[launchgov.InfoKYCActiveSportsbookPlay] != 0 {
		t.Fatalf("counts: %v", r.Info)
	}

	// a DRAFT policy is not active: still dormant
	casino, sports := "casino_play", "sportsbook_play"
	draft := kyc.CreateEnforcementPolicyParams{LicensingJurisdictionID: ja, TriggerType: "play", PlayOperation: &casino, LegalReviewReference: "x", ReasonCode: "lic7_test"}
	if err := w.owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := kyc.CreateEnforcementPolicy(ctx, tx, draft)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wantLic7(t, "draft only", lic7CheckOf(t, w.readinessOf(subA, ja), launchgov.CheckKYCPolicy), launchgov.ResultFail, "no_active_policy")

	// another jurisdiction's active policy does not count for tenant A; tenant B (that jurisdiction) is satisfied
	w.kycPolicy(jb, "cumulative_deposit", nil)
	wantLic7(t, "another tenant's jurisdiction policy", lic7CheckOf(t, w.readinessOf(subA, ja), launchgov.CheckKYCPolicy), launchgov.ResultFail, "no_active_policy")
	rb := w.readinessOf(subB, jb)
	wantLic7(t, "tenant B with its own policy", lic7CheckOf(t, rb, launchgov.CheckKYCPolicy), launchgov.ResultPass, "ok_partial_operations_dormant")
	if rb.Info[launchgov.InfoKYCActiveDeposit] != 1 || rb.Info[launchgov.InfoKYCActiveCasinoPlay] != 0 {
		t.Fatalf("tenant B counts: %v", rb.Info)
	}

	// tenant A: policies for its licence jurisdiction, per operation
	w.kycPolicy(ja, "cumulative_deposit", nil)
	r = w.readinessOf(subA, ja)
	wantLic7(t, "deposit only", lic7CheckOf(t, r, launchgov.CheckKYCPolicy), launchgov.ResultPass, "ok_partial_operations_dormant")
	if r.Info[launchgov.InfoKYCActiveDeposit] != 1 || r.Info[launchgov.InfoKYCActiveCasinoPlay] != 0 || r.Info[launchgov.InfoKYCActiveSportsbookPlay] != 0 {
		t.Fatalf("counts: %v", r.Info)
	}
	w.kycPolicy(ja, "play", &casino)
	wantLic7(t, "sportsbook play still dormant", lic7CheckOf(t, w.readinessOf(subA, ja), launchgov.CheckKYCPolicy), launchgov.ResultPass, "ok_partial_operations_dormant")
	w.kycPolicy(ja, "play", &sports)
	r = w.readinessOf(subA, ja)
	wantLic7(t, "every wired operation covered", lic7CheckOf(t, r, launchgov.CheckKYCPolicy), launchgov.ResultPass, "ok")
	if r.Info[launchgov.InfoKYCActiveCasinoPlay] != 1 || r.Info[launchgov.InfoKYCActiveSportsbookPlay] != 1 {
		t.Fatalf("counts: %v", r.Info)
	}
}

func TestReadiness_KYCPolicy_DormantItemIsAcknowledgeableNotHard(t *testing.T) {
	w := newWorld(t)
	a := w.activeTenant()
	ja := w.jurisdiction(a)
	w.licence(a, ja, "platform", "active")
	brand := w.pendingBrand(a) // a request needs a legal transition: pending_launch -> active
	sub := launchgov.Subject{TenantID: a, BrandID: &brand}
	in := w.activation(sub, ja)
	found := false
	for _, c := range in.AcknowledgedCheckCodes {
		if c == launchgov.CheckKYCPolicy {
			found = true
		}
	}
	if !found {
		t.Fatalf("R-KYC-POLICY fails with no policy and must be in the acknowledged set: %v", in.AcknowledgedCheckCodes)
	}
	skip := in
	skip.AcknowledgedCheckCodes = nil
	for _, c := range in.AcknowledgedCheckCodes {
		if c != launchgov.CheckKYCPolicy {
			skip.AcknowledgedCheckCodes = append(skip.AcknowledgedCheckCodes, c)
		}
	}
	if _, err := w.svc.Request(context.Background(), w.req, skip); !errors.Is(err, launchgov.ErrUnacknowledged) {
		t.Fatalf("skipping R-KYC-POLICY must be refused as unacknowledged, got %v", err)
	}
	w.mustRequest(w.req, in) // acknowledged: the request is filed, the item never refuses by itself
}
