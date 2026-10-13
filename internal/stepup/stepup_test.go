package stepup

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ADR 0119 S4b: the freshness rule and the context assertion.

func TestFresh_Boundaries(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		mfaAt time.Time
		max   time.Duration
		want  bool
	}{
		{"exactly max-age", now.Add(-5 * time.Minute), 5 * time.Minute, true},
		{"one microsecond past", now.Add(-5*time.Minute - time.Microsecond), 5 * time.Minute, false},
		{"one second inside", now.Add(-4*time.Minute - 59*time.Second), 5 * time.Minute, true},
		{"now", now, 5 * time.Minute, true},
		{"future within skew", now.Add(ClockSkew), 5 * time.Minute, true},
		{"future beyond skew", now.Add(ClockSkew + time.Microsecond), 5 * time.Minute, false},
		{"zero mfa_at", time.Time{}, 5 * time.Minute, false},
		{"zero max-age", now, 0, false},
		{"negative max-age", now, -time.Second, false},
	} {
		if got := Fresh(c.mfaAt, now, c.max); got != c.want {
			t.Errorf("%s: Fresh = %v, want %v", c.name, got, c.want)
		}
	}
	if PolicyMaxAge != 2*time.Minute || ClockSkew != 5*time.Second {
		t.Fatal("ADR 0119 sections 7.3 / 9.3 constants changed")
	}
}

func TestAssertion_PresentFreshForAndContext(t *testing.T) {
	now := time.Now()
	sub := uuid.New()
	a := Assertion{Subject: sub, SessionID: uuid.New(), MFAAt: now, Class: Critical}
	if !a.Present() || !a.FreshFor(sub, now, time.Minute) {
		t.Fatal("a fresh assertion for its subject")
	}
	if a.FreshFor(uuid.New(), now, time.Minute) || a.FreshFor(uuid.Nil, now, time.Minute) {
		t.Fatal("another subject (or none) must never be satisfied")
	}
	for name, b := range map[string]Assertion{
		"no subject": {SessionID: uuid.New(), MFAAt: now}, "no session": {Subject: sub, MFAAt: now}, "no mfa_at": {Subject: sub, SessionID: uuid.New()},
	} {
		if b.Present() || b.FreshFor(sub, now, time.Hour) {
			t.Errorf("%s: not present", name)
		}
		if _, ok := From(ContextWith(context.Background(), b)); ok {
			t.Errorf("%s: From must not return an absent assertion", name)
		}
	}
	if _, ok := From(context.Background()); ok {
		t.Fatal("no assertion in a bare context")
	}
	got, ok := From(ContextWith(context.Background(), a))
	if !ok || got != a {
		t.Fatal("round trip")
	}
	if IsStaffRequest(context.Background()) || !IsStaffRequest(WithStaffRequest(context.Background())) {
		t.Fatal("staff-request marker")
	}
	for _, c := range []Class{None, Financial, Critical} {
		if !c.Valid() {
			t.Errorf("%s must be valid", c)
		}
	}
	if Class("high").Valid() || Class("").Valid() {
		t.Fatal("closed vocabulary")
	}
}
