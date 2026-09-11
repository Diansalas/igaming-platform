package validation

import "testing"

func TestRequireNonEmpty(t *testing.T) {
	e := New()
	e.RequireNonEmpty("name", "  ")
	if !e.HasErrors() {
		t.Fatal("expected an error for whitespace-only value")
	}

	e2 := New()
	e2.RequireNonEmpty("name", "Fernando")
	if e2.HasErrors() {
		t.Fatal("did not expect an error for a non-empty value")
	}
}

func TestRequireUUID(t *testing.T) {
	e := New()
	e.RequireUUID("id", "not-a-uuid")
	if !e.HasErrors() {
		t.Fatal("expected an error for an invalid UUID")
	}

	e2 := New()
	e2.RequireUUID("id", "123e4567-e89b-12d3-a456-426614174000")
	if e2.HasErrors() {
		t.Fatal("did not expect an error for a valid UUID")
	}
}

func TestRequireOneOf(t *testing.T) {
	e := New()
	e.RequireOneOf("status", "banana", "active", "suspended")
	if !e.HasErrors() {
		t.Fatal("expected an error for a value not in the allowed set")
	}

	e2 := New()
	e2.RequireOneOf("status", "active", "active", "suspended")
	if e2.HasErrors() {
		t.Fatal("did not expect an error for an allowed value")
	}
}

func TestErrors_ErrorString(t *testing.T) {
	e := New()
	e.Add("field", "is wrong")
	if e.Error() == "" {
		t.Fatal("expected a non-empty error string when errors are present")
	}
}
