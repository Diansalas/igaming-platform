package jurisdiction

import (
	"errors"
	"testing"
)

func TestComposeRestrictions_MostRestrictiveOutcomePrevails(t *testing.T) {
	t.Run("three different severities - highest wins alone", func(t *testing.T) {
		in := []AppliedRestriction{
			{Outcome: RestrictionAllowed, GateID: "g1"},
			{Outcome: RestrictionBlocked, GateID: "g2"},
			{Outcome: RestrictionRestricted, GateID: "g3"},
		}
		composed, err := ComposeRestrictions(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if composed.Outcome() != RestrictionBlocked {
			t.Fatalf("expected blocked, got %s", composed.Outcome())
		}
		if len(composed.Contributors()) != 1 || composed.Contributors()[0].GateID != "g2" {
			t.Fatalf("expected exactly the blocked contributor, got %+v", composed.Contributors())
		}
	})

	t.Run("tied at max severity - both appear in input order", func(t *testing.T) {
		in := []AppliedRestriction{
			{Outcome: RestrictionAllowed, GateID: "g0"},
			{Outcome: RestrictionBlocked, GateID: "first"},
			{Outcome: RestrictionRestricted, GateID: "g1"},
			{Outcome: RestrictionBlocked, GateID: "second"},
		}
		composed, err := ComposeRestrictions(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if composed.Outcome() != RestrictionBlocked {
			t.Fatalf("expected blocked, got %s", composed.Outcome())
		}
		contributors := composed.Contributors()
		if len(contributors) != 2 {
			t.Fatalf("expected exactly 2 tied contributors, got %d: %+v", len(contributors), contributors)
		}
		if contributors[0].GateID != "first" || contributors[1].GateID != "second" {
			t.Fatalf("expected tied contributors in original input order, got %+v", contributors)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		_, err := ComposeRestrictions(nil)
		if !errors.Is(err, ErrNoRestrictionsToCompose) {
			t.Fatalf("expected ErrNoRestrictionsToCompose, got %v", err)
		}
	})

	t.Run("unknown outcome fails the whole call", func(t *testing.T) {
		in := []AppliedRestriction{
			{Outcome: RestrictionBlocked, GateID: "g1"},
			{Outcome: RestrictionOutcome(""), GateID: "g2"},
		}
		_, err := ComposeRestrictions(in)
		if !errors.Is(err, ErrUnknownRestrictionOutcome) {
			t.Fatalf("expected ErrUnknownRestrictionOutcome for a zero-value outcome, got %v", err)
		}

		in2 := []AppliedRestriction{
			{Outcome: RestrictionAllowed, GateID: "g1"},
			{Outcome: RestrictionOutcome("bogus"), GateID: "g2"},
		}
		_, err2 := ComposeRestrictions(in2)
		if !errors.Is(err2, ErrUnknownRestrictionOutcome) {
			t.Fatalf("expected ErrUnknownRestrictionOutcome for an unrecognized outcome, got %v", err2)
		}
	})
}

func TestRestrictionOutcome_Severity(t *testing.T) {
	cases := []struct {
		outcome RestrictionOutcome
		want    int
		wantErr bool
	}{
		{RestrictionBlocked, 3, false},
		{RestrictionRestricted, 2, false},
		{RestrictionAllowed, 1, false},
		{RestrictionOutcome(""), 0, true},
		{RestrictionOutcome("bogus"), 0, true},
	}
	for _, c := range cases {
		got, err := c.outcome.Severity()
		if c.wantErr {
			if err == nil {
				t.Errorf("Severity(%q): expected an error, got nil", c.outcome)
			}
			continue
		}
		if err != nil {
			t.Errorf("Severity(%q): unexpected error: %v", c.outcome, err)
		}
		if got != c.want {
			t.Errorf("Severity(%q) = %d, want %d", c.outcome, got, c.want)
		}
	}
}
