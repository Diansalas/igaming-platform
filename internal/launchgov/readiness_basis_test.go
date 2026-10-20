package launchgov

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// Lic9: pure (DB-free) tests of R-OPERATING-BASIS. The database half is in readiness_basis_integration_test.go.

func basisRow(country, op string, mutate func(*operatingmarket.BasisRecord)) operatingmarket.BasisRecord {
	req := uuid.New()
	b := operatingmarket.BasisRecord{
		ID: uuid.New(), CountryCode: country, OperationCode: op, Kind: operatingmarket.BasisPartnerLicence, Status: operatingmarket.StatusActive,
		ChangeRequestID: &req,
	}
	if mutate != nil {
		mutate(&b)
	}
	return b
}

func allOps(country string, mutate func(*operatingmarket.BasisRecord)) []operatingmarket.BasisRecord {
	return []operatingmarket.BasisRecord{basisRow(country, "registration", mutate), basisRow(country, "deposit", mutate), basisRow(country, "wagering", mutate)}
}

func TestOperatingBasisFrom_Arms(t *testing.T) {
	now := time.Date(2027, 3, 10, 12, 0, 0, 0, time.UTC)
	date := func(days int) *time.Time {
		d := time.Date(2027, 3, 10, 0, 0, 0, 0, time.UTC).AddDate(0, 0, days)
		return &d
	}
	brand := uuid.New()
	for _, c := range []struct {
		name      string
		countries []string
		bases     []operatingmarket.BasisRecord
		result    string
		detail    string
		explicit  int64
		licence   int64
	}{
		{"no enabled country", nil, nil, ResultPass, "no_enabled_country", 0, 0},
		{"enabled country, no basis: the licence chain", []string{"MT"}, nil, ResultPass, "ok_some_via_licence_chain", 0, 1},
		{"every operation covered", []string{"MT"}, allOps("MT", nil), ResultPass, "ok", 1, 0},
		{"one operation missing", []string{"MT"}, allOps("MT", nil)[:2], ResultPass, "ok_some_via_licence_chain", 0, 1},
		{"a withdrawn row does not count", []string{"MT"}, append(allOps("MT", nil)[:2], basisRow("MT", "wagering", func(b *operatingmarket.BasisRecord) { b.Status = operatingmarket.StatusWithdrawn })), ResultPass, "ok_some_via_licence_chain", 0, 1},
		{"an owner-path row (no change request) does not count", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { b.ChangeRequestID = nil }), ResultPass, "ok_some_via_licence_chain", 0, 1},
		{"another brand's row does not count", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { o := uuid.New(); b.BrandID = &o }), ResultPass, "ok_some_via_licence_chain", 0, 1},
		{"the subject brand's own row counts", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { b.BrandID = &brand }), ResultPass, "ok", 1, 0},
		{"a sportsbook-only product row does not cover casino wagering", []string{"MT"}, []operatingmarket.BasisRecord{
			basisRow("MT", "registration", nil), basisRow("MT", "deposit", nil), basisRow("MT", "wagering", func(b *operatingmarket.BasisRecord) { p := "sportsbook"; b.ProductCode = &p })},
			ResultPass, "ok_some_via_licence_chain", 0, 1},
		{"casino restricted by the basis", []string{"MT"}, []operatingmarket.BasisRecord{
			basisRow("MT", "registration", nil), basisRow("MT", "deposit", nil), basisRow("MT", "wagering", func(b *operatingmarket.BasisRecord) { b.RestrictedProductCodes = []string{"casino"} })},
			ResultPass, "ok_some_via_licence_chain", 0, 1},
		{"review due within 30 days", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { b.ReviewDueAt = date(20) }), ResultFail, "review_due_soon", 1, 0},
		{"review due on the due date is not yet overdue", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { b.ReviewDueAt = date(0) }), ResultFail, "review_due_soon", 1, 0},
		{"review overdue", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { b.ReviewDueAt = date(-1) }), ResultFail, "review_overdue", 1, 0},
		{"needs re-review is overdue", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { b.NeedsRereview = true; b.ReviewDueAt = date(200) }), ResultFail, "review_overdue", 1, 0},
		{"review far away passes", []string{"MT"}, allOps("MT", func(b *operatingmarket.BasisRecord) { b.ReviewDueAt = date(200) }), ResultPass, "ok", 1, 0},
	} {
		r := &Readiness{}
		got := operatingBasisFrom(c.countries, c.bases, &brand, now, r)
		if got.Code != CheckOperatingBasis || got.Result != c.result || got.DetailCode != c.detail {
			t.Errorf("%s: %+v, want %s/%s", c.name, got, c.result, c.detail)
		}
		if r.Info[InfoBasisCountriesExplicit] != c.explicit || r.Info[InfoBasisCountriesLicence] != c.licence {
			t.Errorf("%s: info %v", c.name, r.Info)
		}
	}
}

func TestOperatingBasisFrom_RefsAreIDsOnly_AndNoLicenceCounted(t *testing.T) {
	now := time.Date(2027, 3, 10, 12, 0, 0, 0, time.UTC)
	past := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	soon := time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC)
	over := basisRow("MT", "deposit", func(b *operatingmarket.BasisRecord) {
		b.Kind = operatingmarket.BasisNoLicenceDocumented
		b.ReviewDueAt = &past
	})
	dueSoon := basisRow("DE", "deposit", func(b *operatingmarket.BasisRecord) {
		b.Kind = operatingmarket.BasisNoLicenceDocumented
		b.ReviewDueAt = &soon
	})
	r := &Readiness{}
	got := operatingBasisFrom([]string{"MT", "DE"}, []operatingmarket.BasisRecord{over, dueSoon}, nil, now, r)
	if got.Result != ResultFail || got.DetailCode != "review_overdue" {
		t.Fatalf("overdue wins over due soon: %+v", got)
	}
	if r.Info[InfoBasisNoLicenceInForce] != 2 || r.Info[InfoBasisReviewOverdue] != 1 || r.Info[InfoBasisReviewDueSoon] != 1 {
		t.Fatalf("info %v", r.Info)
	}
	if ids := r.Refs[RefsBasisReviewOverdue]; len(ids) != 1 || ids[0] != over.ID.String() {
		t.Fatalf("overdue refs %v", r.Refs)
	}
	if ids := r.Refs[RefsBasisReviewDueSoon]; len(ids) != 1 || ids[0] != dueSoon.ID.String() {
		t.Fatalf("due soon refs %v", r.Refs)
	}
	for _, ids := range r.Refs {
		for _, id := range ids {
			if _, err := uuid.Parse(id); err != nil {
				t.Fatalf("a ref is a bare uuid, got %q", id)
			}
		}
	}
}

// R-OPERATING-BASIS is acknowledgeable, never a hard precondition: the refusing paths never name it, and a non-pass is listed by
// NonPassing (so the request must acknowledge it) without anything refusing by itself.
func TestOperatingBasis_AcknowledgeableNotHard(t *testing.T) {
	for _, f := range []string{"hard.go", "decide.go", "execute.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "CheckOperatingBasis") || strings.Contains(string(b), "R-OPERATING-BASIS") {
			t.Errorf("%s must not name R-OPERATING-BASIS: it is acknowledgeable, never a hard precondition", f)
		}
	}
	if CheckOperatingBasis != "R-OPERATING-BASIS" {
		t.Fatal("the code drifted")
	}
	fail := Readiness{Version: SnapshotVersion, Environment: "test", Checks: []Check{ck(CheckOperatingBasis, ResultFail, "review_overdue")}}
	pass := Readiness{Version: SnapshotVersion, Environment: "test", Checks: []Check{ck(CheckOperatingBasis, ResultPass, "ok")}}
	if np := fail.NonPassing(); len(np) != 1 || np[0] != CheckOperatingBasis {
		t.Fatalf("a failing item must be listed for acknowledgement: %v", np)
	}
	if len(pass.NonPassing()) != 0 || fail.Hash() == pass.Hash() {
		t.Fatal("a pass lists nothing and the result is part of the readiness hash")
	}
}
