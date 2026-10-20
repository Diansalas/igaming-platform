package licensingchange

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Lic9 (migration 0180): DB-free tests of the operating-basis kinds' decode (shape, lint, closed vocabulary).

func validSet() map[string]any {
	tid, jid := uuid.New(), uuid.New()
	return map[string]any{
		"tenant_id": tid.String(), "brand_id": nil, "country_code": "MT", "operation_code": "deposit", "product_code": nil,
		"basis_kind": "no_licence_documented", "licence_id": nil, "jurisdiction_id": jid.String(),
		"determination_reference": "DET-1", "determined_by_role": "head_of_compliance", "basis_evidence_reference": nil,
		"restrictions_note": nil, "restricted_product_codes": []string{}, "review_due_at": time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02"),
	}
}

func decodeOf(t *testing.T, k Kind, m map[string]any) error {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = kinds[k].decode(b)
	return err
}

func TestOperatingBasisSet_Decode_AcceptsTheValidShapeAndRefusesTheRest(t *testing.T) {
	if err := decodeOf(t, KindOperatingBasisSet, validSet()); err != nil {
		t.Fatalf("the valid payload must decode: %v", err)
	}
	mut := func(f func(m map[string]any)) map[string]any {
		m := validSet()
		f(m)
		return m
	}
	lic := uuid.New().String()
	cases := map[string]map[string]any{
		"missing key":                          mut(func(m map[string]any) { delete(m, "restrictions_note") }),
		"unknown key":                          mut(func(m map[string]any) { m["blanket_exemption"] = true }),
		"unknown basis kind":                   mut(func(m map[string]any) { m["basis_kind"] = "licence_exempt" }),
		"no licence needs a jurisdiction":      mut(func(m map[string]any) { m["jurisdiction_id"] = nil }),
		"no licence needs a review date":       mut(func(m map[string]any) { m["review_due_at"] = nil }),
		"no licence carries no licence":        mut(func(m map[string]any) { m["licence_id"] = lic }),
		"licence kind needs a licence":         mut(func(m map[string]any) { m["basis_kind"] = "platform_licence"; m["jurisdiction_id"] = nil }),
		"licence kind carries no jurisdiction": mut(func(m map[string]any) { m["basis_kind"] = "partner_licence"; m["licence_id"] = lic }),
		"lower-case country":                   mut(func(m map[string]any) { m["country_code"] = "mt" }),
		"unassigned country":                   mut(func(m map[string]any) { m["country_code"] = "ZZ" }),
		"empty operation":                      mut(func(m map[string]any) { m["operation_code"] = "" }),
		"a name as the role":                   mut(func(m map[string]any) { m["determined_by_role"] = "Jane Doe" }),
		"an e-mail in the reference":           mut(func(m map[string]any) { m["determination_reference"] = "x@example.com" }),
		"a padded reference":                   mut(func(m map[string]any) { m["determination_reference"] = " DET-1 " }),
		"a newline in the note":                mut(func(m map[string]any) { m["restrictions_note"] = "a\nb" }),
		"a phone number in the evidence":       mut(func(m map[string]any) { m["basis_evidence_reference"] = "+44 20 7946 0958" }),
		"a long digit run":                     mut(func(m map[string]any) { m["determination_reference"] = "ref 1234567890123" }),
		"a bad review date":                    mut(func(m map[string]any) { m["review_due_at"] = "2027-13-45" }),
		"duplicated restricted codes":          mut(func(m map[string]any) { m["restricted_product_codes"] = []string{"casino", "casino"} }),
	}
	for name, m := range cases {
		if err := decodeOf(t, KindOperatingBasisSet, m); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: must be ErrInvalidInput, got %v", name, err)
		}
	}
	// A licence basis decodes with a licence and no jurisdiction; its review date is optional.
	ok := validSet()
	ok["basis_kind"], ok["licence_id"], ok["jurisdiction_id"], ok["review_due_at"] = "partner_licence", lic, nil, nil
	if err := decodeOf(t, KindOperatingBasisSet, ok); err != nil {
		t.Fatalf("a partner licence basis must decode: %v", err)
	}
}

func TestOperatingBasisWithdrawAndReview_Decode(t *testing.T) {
	tid := uuid.New().String()
	w := map[string]any{"tenant_id": tid, "brand_id": nil, "country_code": "MT", "operation_code": "deposit", "product_code": nil}
	if err := decodeOf(t, KindOperatingBasisWithdraw, w); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	extra := map[string]any{"tenant_id": tid, "brand_id": nil, "country_code": "MT", "operation_code": "deposit", "product_code": nil, "reason": "x"}
	if err := decodeOf(t, KindOperatingBasisWithdraw, extra); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("an unknown key must be refused: %v", err)
	}
	rr := map[string]any{"tenant_id": tid, "brand_id": nil, "country_code": "MT", "operation_code": "deposit", "product_code": nil,
		"review_due_at": "2027-06-01", "needs_rereview": false, "review_reference": "REV-1"}
	if err := decodeOf(t, KindOperatingBasisReviewRecord, rr); err != nil {
		t.Fatalf("review: %v", err)
	}
	for name, mutate := range map[string]func(m map[string]any){
		"missing flag":    func(m map[string]any) { delete(m, "needs_rereview") },
		"bad date":        func(m map[string]any) { m["review_due_at"] = "tomorrow" },
		"empty reference": func(m map[string]any) { m["review_reference"] = "" },
		"e-mail":          func(m map[string]any) { m["review_reference"] = "a@b.co" },
	} {
		m := map[string]any{}
		for k, v := range rr {
			m[k] = v
		}
		mutate(m)
		if err := decodeOf(t, KindOperatingBasisReviewRecord, m); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: must be ErrInvalidInput, got %v", name, err)
		}
	}
}

// The three kinds are platform-only (tenant staff never request a basis change) and none ships a default.
func TestOperatingBasisKinds_ArePlatformOnly(t *testing.T) {
	for _, k := range []Kind{KindOperatingBasisSet, KindOperatingBasisWithdraw, KindOperatingBasisReviewRecord} {
		spec, ok := kinds[k]
		if !ok || !spec.platformOnly || spec.platformPerm == "" || spec.tenantPerm(nil) != "" {
			t.Errorf("%s must be registered platform-only with a platform permission and no tenant permission", k)
		}
	}
}
