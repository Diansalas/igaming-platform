package main

import (
	"strings"
	"testing"
)

// PRH-2 R2 (security C4): `down` is refused in deployed environments.
func TestCheckDownAllowed(t *testing.T) {
	for _, tc := range []struct {
		env   string
		set   bool
		allow bool
	}{
		{"development", true, true},
		{"staging", true, true},
		{"production", true, false},
		{"", false, false}, // unset counts as production (GuardEnvironment semantics)
		{"", true, false},  // explicitly empty
		{"Production", true, false},
		{"prod", true, false},
		{"anything-else", true, false},
	} {
		err := checkDownAllowed(tc.env, tc.set)
		if tc.allow && err != nil {
			t.Errorf("APP_ENV=%q set=%v: want allowed, got %v", tc.env, tc.set, err)
		}
		if !tc.allow {
			if err == nil {
				t.Errorf("APP_ENV=%q set=%v: want refused, got nil", tc.env, tc.set)
			} else if !strings.Contains(err.Error(), "refusing `down`") {
				t.Errorf("unexpected refusal text: %v", err)
			}
		}
	}
}
