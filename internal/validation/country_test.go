package validation

import "testing"

func TestIsISO3166Alpha2_KnownValidCodes(t *testing.T) {
	for _, code := range []string{"US", "MT", "KM", "CO", "GB"} {
		if !IsISO3166Alpha2(code) {
			t.Errorf("IsISO3166Alpha2(%q) = false, want true", code)
		}
	}
}

func TestIsISO3166Alpha2_KnownInvalidCodes(t *testing.T) {
	for _, code := range []string{"ZZ", "XX", "us", "USA", ""} {
		if IsISO3166Alpha2(code) {
			t.Errorf("IsISO3166Alpha2(%q) = true, want false", code)
		}
	}
}

// TestIsISO3166Alpha2_ExactlyTwoHundredFortyNineEntries is a cheap
// regression guard against a future careless edit accidentally adding or
// removing an entry from the static list.
func TestIsISO3166Alpha2_ExactlyTwoHundredFortyNineEntries(t *testing.T) {
	if got, want := len(iso3166Alpha2), 249; got != want {
		t.Errorf("len(iso3166Alpha2) = %d, want %d", got, want)
	}
}

func TestRequireISO3166Alpha2(t *testing.T) {
	e := New()
	e.RequireISO3166Alpha2("country", "US")
	if e.HasErrors() {
		t.Errorf("unexpected error for valid code: %v", e.Fields)
	}

	e2 := New()
	e2.RequireISO3166Alpha2("country", "ZZ")
	if !e2.HasErrors() {
		t.Error("expected an error for invalid code, got none")
	}
}
