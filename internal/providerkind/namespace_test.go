package providerkind

import (
	"strings"
	"testing"
)

func TestRefuseProviderIDCollisions_CrossFamilyRefused(t *testing.T) {
	err := RefuseProviderIDCollisions([]IDClaim{
		{"payments", "acme", "payments/provider"},
		{"casino", "ACME ", "casino/provider"},
		{"kyc", "other", "kyc/provider"},
	})
	if err == nil {
		t.Fatal("expected collision error")
	}
	for _, want := range []string{"acme", "payments (payments/provider)", "casino (casino/provider)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "other") {
		t.Errorf("non-colliding id named: %v", err)
	}
}

func TestRefuseProviderIDCollisions_DistinctAndSameFamilyPass(t *testing.T) {
	if err := RefuseProviderIDCollisions([]IDClaim{
		{"payments", "p1", "provider"},
		{"payments", "p1", "statement_source"},
		{"casino", "c1", "provider"},
		{"sportsbook", "s1", "provider"},
		{"kyc", "k1", "provider"},
		{"kyc", "", "empty ignored"},
		{"casino", "", "empty ignored"},
	}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if err := RefuseProviderIDCollisions(nil); err != nil {
		t.Fatal(err)
	}
}

func TestRefuseProviderIDCollisions_NamesEveryCollision(t *testing.T) {
	err := RefuseProviderIDCollisions([]IDClaim{
		{"payments", "a", "x"}, {"casino", "a", "y"},
		{"kyc", "b", "x"}, {"sportsbook", "b", "y"},
	})
	if err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("both ids must be named: %v", err)
	}
}
