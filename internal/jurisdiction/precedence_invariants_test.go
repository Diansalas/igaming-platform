package jurisdiction

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These are pure unit tests, mirroring resolver_test.go's own stated
// convention: no build tag, no database, no fixture.

// --- Purity ---

func TestDeterminePlayerJurisdiction_IsPureNoDatabaseHandle(t *testing.T) {
	typ := reflect.TypeOf(DetermineParams{})
	if typ.NumField() != 4 {
		t.Fatalf("DetermineParams must have exactly 4 fields (Purpose/Evidence/Policy/AsOf), got %d", typ.NumField())
	}
	wantFields := map[string]bool{"Purpose": true, "Evidence": true, "Policy": true, "AsOf": true}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !wantFields[f.Name] {
			t.Fatalf("unexpected field %q on DetermineParams", f.Name)
		}
		assertNoDBHandleType(t, f.Type, f.Name)
	}

	fn := reflect.TypeOf(DeterminePlayerJurisdiction)
	if fn.NumIn() != 1 {
		t.Fatalf("DeterminePlayerJurisdiction must take exactly one argument, got %d", fn.NumIn())
	}
	if fn.In(0) != reflect.TypeOf(DetermineParams{}) {
		t.Fatalf("DeterminePlayerJurisdiction's argument must be DetermineParams")
	}
}

// assertNoDBHandleType recursively walks a type (and, for struct/pointer
// fields, its nested fields) looking for any context.Context, pgx.Tx/
// pgx.Rows, ReadOnlyQuerier, or database/sql type - none may appear
// anywhere inside DetermineParams.
func assertNoDBHandleType(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	forbidden := map[reflect.Type]bool{
		reflect.TypeOf((*context.Context)(nil)).Elem(): true,
		reflect.TypeOf((*pgx.Tx)(nil)).Elem():          true,
		reflect.TypeOf((*pgx.Rows)(nil)).Elem():        true,
		reflect.TypeOf((*ReadOnlyQuerier)(nil)).Elem(): true,
		reflect.TypeOf(sql.DB{}):                       true,
		reflect.TypeOf(sql.Tx{}):                       true,
	}
	if forbidden[typ] {
		t.Fatalf("%s must not carry a database/context handle type (%s)", path, typ)
	}
	if typ == reflect.TypeOf(time.Time{}) || typ == reflect.TypeOf(uuid.UUID{}) {
		return // known-safe leaf types; no need to walk their internals
	}
	if typ.Kind() == reflect.Struct {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			assertNoDBHandleType(t, ft, path+"."+f.Name)
		}
	}
}

// --- Determinism ---

func TestDeterminePlayerJurisdiction_IsDeterministicAcrossRepeatedCalls(t *testing.T) {
	dur := 24 * time.Hour
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	params := DetermineParams{
		Purpose: PurposeMarketAccessControl,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: asOf.Add(-time.Hour), VerificationID: uuid.New()},
			DeclaredResidence: &DeclaredResidenceEvidence{CountryCode: "DE", CapturedAt: asOf.Add(-2 * time.Hour), PlayerAccountID: uuid.New()},
			LocationSignal:    &LocationSignalEvidence{State: LocationObserved, CountryCode: "MT", ObservedAt: asOf.Add(-time.Minute), ProviderID: "prov", ProviderReference: "ref"},
		},
		Policy: EvaluationPolicy{LocationSignalRequirement: LocationRequired, MaxLocationSignalAge: &dur},
		AsOf:   asOf,
	}

	var results []PlayerJurisdictionResult
	for i := 0; i < 3; i++ {
		res, err := DeterminePlayerJurisdiction(params)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		results = append(results, res)
	}
	for i := 1; i < len(results); i++ {
		if !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("call 0 and call %d produced different results:\n%+v\nvs\n%+v", i, results[0], results[i])
		}
	}
}

// --- Exhaustive purpose handling ---

var allDeclaredPurposes = []Purpose{PurposeIdentityDetermination, PurposeMarketAccessControl, PurposeHistoricalReporting}

func TestDetermine_EveryDeclaredPurposeIsHandledAndUnknownPurposesFailClosed(t *testing.T) {
	dur := 24 * time.Hour
	asOf := time.Now().UTC()
	basePolicy := EvaluationPolicy{LocationSignalRequirement: LocationAdvisory, MaxLocationSignalAge: &dur}

	for _, purpose := range allDeclaredPurposes {
		_, err := DeterminePlayerJurisdiction(DetermineParams{Purpose: purpose, Policy: basePolicy, AsOf: asOf})
		if purpose == PurposeHistoricalReporting {
			if !errors.Is(err, ErrHistoricalPurposeNotComputable) {
				t.Errorf("%s: expected ErrHistoricalPurposeNotComputable, got %v", purpose, err)
			}
			continue
		}
		if errors.Is(err, ErrUnsupportedPurpose) {
			t.Errorf("%s: a declared purpose must never return ErrUnsupportedPurpose", purpose)
		}
	}

	_, err := DeterminePlayerJurisdiction(DetermineParams{Purpose: Purpose("something_else"), Policy: basePolicy, AsOf: asOf})
	if !errors.Is(err, ErrUnsupportedPurpose) {
		t.Fatalf("expected ErrUnsupportedPurpose for a bogus purpose, got %v", err)
	}
}

// --- No tenant/fallback basis ever emitted ---

func TestDetermine_NeverEmitsATenantOrFallbackBasisOnAnyInput(t *testing.T) {
	permittedBases := map[Basis]bool{
		BasisPlayerVerifiedResidence: true,
		BasisPlayerDeclaredResidence: true,
		BasisGeoSignal:               true,
	}

	verifiedOptions := []*VerifiedResidenceEvidence{
		nil,
		{CountryCode: "MT", SetAt: time.Now().Add(-time.Hour), VerificationID: uuid.New()},
		{CountryCode: "ZZ", SetAt: time.Now().Add(-time.Hour), VerificationID: uuid.New()}, // invalid
	}
	declaredOptions := []*DeclaredResidenceEvidence{
		nil,
		{CountryCode: "DE", CapturedAt: time.Now().Add(-time.Hour), PlayerAccountID: uuid.New()},
		{CountryCode: "ZZ", CapturedAt: time.Now().Add(-time.Hour), PlayerAccountID: uuid.New()}, // invalid
	}
	asOf := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	fresh := asOf.Add(-time.Minute)
	stale := asOf.Add(-48 * time.Hour)
	maxAge := 24 * time.Hour

	locationOptions := []*LocationSignalEvidence{
		nil,
		{State: LocationObserved, CountryCode: "MT", ObservedAt: fresh, ProviderID: "p", ProviderReference: "r"},
		{State: LocationObserved, CountryCode: "MT", ObservedAt: stale, ProviderID: "p", ProviderReference: "r"},
		{State: LocationObserved, CountryCode: "ZZ", ObservedAt: fresh, ProviderID: "p", ProviderReference: "r"},
		{State: LocationInconclusive, ObservedAt: fresh, ProviderID: "p", ProviderReference: "r"},
		{State: LocationUnavailable, ObservedAt: fresh, ProviderID: "p", ProviderReference: "r"},
		{State: LocationProviderError, ObservedAt: fresh, ProviderID: "p", ProviderReference: "r"},
	}
	requirements := []LocationRequirement{LocationRequired, LocationAdvisory}

	for _, purpose := range []Purpose{PurposeIdentityDetermination, PurposeMarketAccessControl} {
		for _, v := range verifiedOptions {
			for _, d := range declaredOptions {
				for _, l := range locationOptions {
					for _, req := range requirements {
						policy := EvaluationPolicy{LocationSignalRequirement: req, MaxLocationSignalAge: &maxAge}
						res, err := DeterminePlayerJurisdiction(DetermineParams{
							Purpose:  purpose,
							Evidence: EvidenceSet{VerifiedResidence: v, DeclaredResidence: d, LocationSignal: l},
							Policy:   policy,
							AsOf:     asOf,
						})
						if errors.Is(err, ErrPolicyUnset) {
							continue // expected for some market-access combinations, not a failure
						}
						if err != nil {
							t.Fatalf("%s v=%v d=%v l=%v req=%s: unexpected error: %v", purpose, v, d, l, req, err)
						}
						if res.Reason() == ReasonIrreconcilableBases {
							t.Fatalf("%s: ReasonIrreconcilableBases must never be produced by this algorithm", purpose)
						}
						if res.Outcome() != Resolved {
							continue
						}
						cands, cerr := res.Candidates()
						if cerr != nil {
							t.Fatalf("%s: Resolved result must expose Candidates(): %v", purpose, cerr)
						}
						for _, c := range cands {
							if !permittedBases[c.Basis()] {
								t.Fatalf("%s: candidate carries forbidden basis %q (v=%v d=%v l=%v)", purpose, c.Basis(), v, d, l)
							}
						}
						if v == nil {
							primary, perr := res.PrimaryCandidate()
							if perr != nil {
								t.Fatalf("%s: resolved result must have a primary candidate: %v", purpose, perr)
							}
							if primary.Basis() == BasisPlayerDeclaredResidence {
								t.Fatalf("%s: declared residence alone must never resolve as the primary determination (v=%v d=%v)", purpose, v, d)
							}
						}
					}
				}
			}
		}
	}
}

// --- EvidenceSet shape ---

func TestEvidenceSet_ExposesNoTenantBrandOrLicenceInput(t *testing.T) {
	typ := reflect.TypeOf(EvidenceSet{})
	if typ.NumField() != 3 {
		t.Fatalf("EvidenceSet must have EXACTLY three fields, got %d", typ.NumField())
	}
	want := []string{"VerifiedResidence", "DeclaredResidence", "LocationSignal"}
	for i, name := range want {
		if typ.Field(i).Name != name {
			t.Fatalf("EvidenceSet field %d = %q, want %q", i, typ.Field(i).Name, name)
		}
	}
}

// --- Zero-value result ---

func TestPlayerJurisdictionResult_ZeroValueIsNotResolvedAndYieldsNoCode(t *testing.T) {
	var r PlayerJurisdictionResult
	if r.Outcome() == Resolved {
		t.Fatal("the zero-value PlayerJurisdictionResult must never report Resolved")
	}
	if _, err := r.Candidates(); !errors.Is(err, ErrNotResolved) {
		t.Fatalf("Candidates() on a zero-value result must return ErrNotResolved, got %v", err)
	}
	if _, err := r.PrimaryCandidate(); !errors.Is(err, ErrNotResolved) {
		t.Fatalf("PrimaryCandidate() on a zero-value result must return ErrNotResolved, got %v", err)
	}
}

// --- No leaked country codes ---

func TestPlayerJurisdictionResult_FormattingAndJSONNeverLeakACountryCode(t *testing.T) {
	res, err := DeterminePlayerJurisdiction(DetermineParams{
		Purpose: PurposeIdentityDetermination,
		Evidence: EvidenceSet{
			VerifiedResidence: &VerifiedResidenceEvidence{CountryCode: "MT", SetAt: time.Now(), VerificationID: uuid.New()},
		},
		AsOf: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome() != Resolved {
		t.Fatalf("expected Resolved, got %s", res.Outcome())
	}

	check := func(label, s string) {
		if strings.Contains(s, "MT") {
			t.Fatalf("%s leaked the country code: %q", label, s)
		}
	}
	check("%v(result)", fmt.Sprintf("%v", res))
	check("%+v(result)", fmt.Sprintf("%+v", res))
	check("%s(result)", fmt.Sprintf("%s", res)) //nolint:staticcheck // S1025: %s is the verb under test

	jb, jerr := json.Marshal(res) //nolint:staticcheck // SA9005: marshaling a type with no exported fields is the point
	if jerr != nil {
		t.Fatalf("json.Marshal(result): %v", jerr)
	}
	check("json(result)", string(jb))
	if string(jb) != "{}" {
		t.Fatalf("json.Marshal(result) must be {} (all fields unexported), got %s", jb)
	}

	primary, perr := res.PrimaryCandidate()
	if perr != nil {
		t.Fatalf("PrimaryCandidate: %v", perr)
	}
	check("%v(candidate)", fmt.Sprintf("%v", primary))
	check("%+v(candidate)", fmt.Sprintf("%+v", primary))
	check("%s(candidate)", fmt.Sprintf("%s", primary)) //nolint:staticcheck // S1025: %s is the verb under test
	cjb, cjerr := json.Marshal(primary)                //nolint:staticcheck // SA9005: marshaling a type with no exported fields is the point
	if cjerr != nil {
		t.Fatalf("json.Marshal(candidate): %v", cjerr)
	}
	check("json(candidate)", string(cjb))
	if string(cjb) != "{}" {
		t.Fatalf("json.Marshal(candidate) must be {} (all fields unexported), got %s", cjb)
	}

	cands, cerr := res.Candidates()
	if cerr != nil {
		t.Fatalf("Candidates: %v", cerr)
	}
	check("%v(candidates)", fmt.Sprintf("%v", cands))
	check("%+v(candidates)", fmt.Sprintf("%+v", cands))
}
