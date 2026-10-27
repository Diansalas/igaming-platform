package operatingmarket

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Lic9: DB-free invariants of the operating-basis step.

// Only Outcome "permitted" permits: every other value, including one this build has never heard of, is a refusal for every consumer
// (marketgate, readiness, registration, deposit, casino, sportsbook all branch on Permitted()).
func TestResult_OnlyPermittedPermits_UnknownOutcomeFailsClosed(t *testing.T) {
	for _, o := range []Outcome{OutcomeNotConfigured, OutcomeDisabledByTenant, OutcomeDisabledByBrand, OutcomeDisabledByOperation,
		OutcomeNotPermittedByLicence, OutcomeLicensingUnknown, OutcomeConfigurationConflict, OutcomeInvalidConfiguration,
		OutcomePolicyNotYetEffective, OutcomePolicyExpired, OutcomeNotPermittedByBasis, Outcome(""), Outcome("some_future_outcome")} {
		if (Result{outcome: o}).Permitted() {
			t.Errorf("outcome %q must not permit", o)
		}
	}
	if !(Result{outcome: OutcomePermitted}).Permitted() {
		t.Fatal("permitted must permit")
	}
}

func TestParseBasisKind_ClosedVocabulary(t *testing.T) {
	for _, k := range []BasisKind{BasisPlatformLicence, BasisPartnerLicence, BasisNoLicenceDocumented} {
		if got, err := parseBasisKind(string(k)); err != nil || got != k {
			t.Errorf("%s: %v %v", k, got, err)
		}
	}
	for _, bad := range []string{"", "licence_exempt", "none", "exempt", "PLATFORM_LICENCE", "no_licence"} {
		if _, err := parseBasisKind(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q must be refused, got %v", bad, err)
		}
	}
}

// ScopeBasis is a diagnostic scope only, never a writable policy scope.
func TestScopeBasis_IsNotWritable(t *testing.T) {
	if validWritableScope(ScopeBasis) || validWritableScope(ScopeLicence) {
		t.Fatal("basis and licence are diagnostic scopes, not operating_country_policies scope kinds")
	}
}

func TestBasisScope_Validate(t *testing.T) {
	good := BasisScope{TenantID: uuid.New(), CountryCode: "MT", OperationCode: "deposit"}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]BasisScope{
		"no tenant":      {CountryCode: "MT", OperationCode: "deposit"},
		"lower country":  {TenantID: uuid.New(), CountryCode: "mt", OperationCode: "deposit"},
		"unassigned":     {TenantID: uuid.New(), CountryCode: "ZZ", OperationCode: "deposit"},
		"empty operaton": {TenantID: uuid.New(), CountryCode: "MT", OperationCode: " "},
	} {
		if err := s.validate(); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The write API takes no tenant / change-request / actor stamps from a caller beyond the scope: the guard trigger stamps them.
func TestSetOperatingBasisParams_NoApprovalOrTimeFields(t *testing.T) {
	src, err := os.ReadFile("basis_admin.go")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(src), "type SetOperatingBasisParams struct")
	j := strings.Index(string(src)[i:], "\n}\n")
	body := strings.ToLower(string(src)[i : i+j])
	for _, forbidden := range []string{"changerequest", "effectivefrom", "effectiveto", "createdby", "approved", "policyversion"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("SetOperatingBasisParams must not accept %q (the database stamps it)", forbidden)
		}
	}
}

// resolve.go keeps the no-clock invariant after the lic9 edit (the basis step lives in resolve_basis.go: also clock-free).
func TestResolveBasis_SourceHasNoClock(t *testing.T) {
	data, err := os.ReadFile("resolve_basis.go")
	if err != nil {
		t.Fatal(err)
	}
	var code strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line + "\n")
	}
	lower := strings.ToLower(code.String())
	for _, f := range []string{"time.now()", "now()", "clock_timestamp()", "current_timestamp", "current_date"} {
		if strings.Contains(lower, f) {
			t.Errorf("resolve_basis.go must not read a clock (%q): AsOf is a parameter (INV-M-3)", f)
		}
	}
}
