//go:build integration

package launchgov_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/licensingchange"
)

// Lic9: R-OPERATING-BASIS. The bases are created through the REAL governed path (licensing change requests, two distinct
// approvers) so the rows carry their executed change request; the readiness reads run through the launchgov Service over the
// runtime pool (RLS-correct inside the read-only tenant transaction). Acknowledgeable, never a hard precondition.

func (w *world) lcSvc() *licensingchange.Service {
	w.t.Helper()
	s, err := licensingchange.New(licensingchange.Config{Pool: w.rt, LockTimeout: "3s"})
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) lcActor(id uuid.UUID) licensingchange.Actor {
	return licensingchange.Actor{StaffID: id, Scope: licensingchange.ScopePlatform, Role: string(auth.RolePlatformAdmin), RequestID: "lc9-" + id.String()[:8]}
}

// lcWiden files a widening change and executes it with the two fixture approvers.
func (w *world) lcWiden(kind licensingchange.Kind, payload any, reason string) {
	w.t.Helper()
	ctx := context.Background()
	s := w.lcSvc()
	raw, err := jsonRaw(payload)
	if err != nil {
		w.t.Fatal(err)
	}
	r, err := s.Request(ctx, w.lcActor(w.ops.Requester), licensingchange.RequestInput{Kind: kind, Payload: raw, ReasonCode: reason})
	if err != nil {
		w.t.Fatalf("request %s: %v", kind, err)
	}
	if _, err := s.Approve(ctx, w.lcActor(w.ops.Approver1), r.Request.ID, "reviewed_ok"); err != nil {
		w.t.Fatalf("approve 1: %v", err)
	}
	done, err := s.Approve(ctx, w.lcActor(w.ops.Approver2), r.Request.ID, "reviewed_ok")
	if err != nil || !done.Executed {
		w.t.Fatalf("approve 2 must execute %s: %v", kind, err)
	}
}

func (w *world) lcNarrow(kind licensingchange.Kind, payload any, reason string) {
	w.t.Helper()
	raw, err := jsonRaw(payload)
	if err != nil {
		w.t.Fatal(err)
	}
	r, err := w.lcSvc().Request(context.Background(), w.lcActor(w.ops.Requester), licensingchange.RequestInput{Kind: kind, Payload: raw, ReasonCode: reason})
	if err != nil || !r.Executed {
		w.t.Fatalf("narrowing %s must execute on filing: %v", kind, err)
	}
}

func TestReadiness_OperatingBasis_ReportsExplicitBasesAndReviews(t *testing.T) {
	w := newWorld(t)
	a, b := w.activeTenant(), w.activeTenant()
	ja := w.jurisdiction(a)
	subA, subB := launchgov.Subject{TenantID: a}, launchgov.Subject{TenantID: b}
	due := time.Now().UTC().AddDate(0, 0, 20).Format("2006-01-02")
	far := time.Now().UTC().AddDate(0, 0, 200).Format("2006-01-02")
	ops := []string{"registration", "deposit", "wagering"}

	// 1. Nothing enabled: nothing to report (R-MARKET-CONFIG covers an unconfigured market).
	wantLic7(t, "nothing enabled", lic7CheckOf(t, w.readinessOf(subA, ja), launchgov.CheckOperatingBasis), launchgov.ResultPass, "no_enabled_country")

	// 2. Documented no-licence bases for the three gated operations, a tenant-rung policy for the country.
	for _, op := range ops {
		w.lcWiden(licensingchange.KindOperatingBasisSet, licensingchange.OperatingBasisSetPayload{
			TenantID: a, CountryCode: "MT", OperationCode: op, BasisKind: "no_licence_documented", JurisdictionID: &ja,
			DeterminationReference: "DET-" + op, DeterminedByRole: "head_of_compliance", RestrictedProductCodes: []string{}, ReviewDueAt: &due,
		}, "documented_basis")
	}
	ref := "TICKET-1"
	w.lcWiden(licensingchange.KindOperatingPolicyVersion, licensingchange.OperatingPolicyPayload{TenantID: a, Scope: "tenant", CountryCode: "MT",
		State: "enabled", Status: "active", AuthorizationReference: &ref}, "market_entry")

	r := w.readinessOf(subA, ja)
	wantLic7(t, "review due within 30 days", lic7CheckOf(t, r, launchgov.CheckOperatingBasis), launchgov.ResultFail, "review_due_soon")
	if r.Info[launchgov.InfoBasisCountriesEnabled] != 1 || r.Info[launchgov.InfoBasisCountriesExplicit] != 1 || r.Info[launchgov.InfoBasisNoLicenceInForce] != 3 ||
		r.Info[launchgov.InfoBasisReviewDueSoon] != 3 || len(r.Refs[launchgov.RefsBasisReviewDueSoon]) != 3 {
		t.Fatalf("info %v refs %v", r.Info, r.Refs)
	}
	soonHash := r.Hash()

	// Tenant isolation: another tenant sees none of it.
	rb := w.readinessOf(subB, w.jurisdiction(b))
	wantLic7(t, "another tenant", lic7CheckOf(t, rb, launchgov.CheckOperatingBasis), launchgov.ResultPass, "no_enabled_country")
	if len(rb.Refs[launchgov.RefsBasisReviewDueSoon]) != 0 || rb.Info[launchgov.InfoBasisNoLicenceInForce] != 0 {
		t.Fatalf("tenant B saw tenant A's bases: %v %v", rb.Info, rb.Refs)
	}

	// 3. Recording a review that moves the date far out (widening, two approvals) clears the item.
	for _, op := range ops {
		w.lcWiden(licensingchange.KindOperatingBasisReviewRecord, licensingchange.OperatingBasisReviewPayload{
			TenantID: a, CountryCode: "MT", OperationCode: op, ReviewDueAt: far, NeedsRereview: false, ReviewReference: "REV-" + op}, "review_completed")
	}
	r = w.readinessOf(subA, ja)
	wantLic7(t, "reviews recorded", lic7CheckOf(t, r, launchgov.CheckOperatingBasis), launchgov.ResultPass, "ok")
	if r.Hash() == soonHash {
		t.Fatal("the result is part of the readiness hash (the request must be re-requested after a change)")
	}

	// 4. Marking one basis needs_rereview (narrowing) is reported as overdue, by id only.
	w.lcNarrow(licensingchange.KindOperatingBasisReviewRecord, licensingchange.OperatingBasisReviewPayload{
		TenantID: a, CountryCode: "MT", OperationCode: "deposit", ReviewDueAt: far, NeedsRereview: true, ReviewReference: "REV-flag"}, "rereview_required")
	r = w.readinessOf(subA, ja)
	wantLic7(t, "needs re-review", lic7CheckOf(t, r, launchgov.CheckOperatingBasis), launchgov.ResultFail, "review_overdue")
	if len(r.Refs[launchgov.RefsBasisReviewOverdue]) != 1 {
		t.Fatalf("refs %v", r.Refs)
	}

	// 5. A withdrawn basis no longer counts as explicit coverage: the country falls back to "some via the licence chain" (here the
	// tenant has none, which R-MARKET-CONFIG reports separately); the item itself stays informational.
	w.lcNarrow(licensingchange.KindOperatingBasisWithdraw, licensingchange.OperatingBasisWithdrawPayload{TenantID: a, CountryCode: "MT", OperationCode: "wagering"}, "basis_withdrawn")
	w.lcNarrow(licensingchange.KindOperatingBasisWithdraw, licensingchange.OperatingBasisWithdrawPayload{TenantID: a, CountryCode: "MT", OperationCode: "deposit"}, "basis_withdrawn")
	r = w.readinessOf(subA, ja)
	if c := lic7CheckOf(t, r, launchgov.CheckOperatingBasis); c.Result != launchgov.ResultPass || c.DetailCode != "ok_some_via_licence_chain" || r.Info[launchgov.InfoBasisCountriesLicence] != 1 {
		t.Fatalf("a withdrawn basis is not explicit coverage: %+v %v", c, r.Info)
	}
}

func jsonRaw(v any) ([]byte, error) { return json.Marshal(v) }
