package scratchname

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 8, 30, 5, 0, time.UTC)

func TestBuildNameParseRoundTrip(t *testing.T) {
	name, err := BuildName("ma020_rt", t0, 4242, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if name != "ma020_rt20261016083005p4242xdeadbeef" {
		t.Fatalf("name = %q", name)
	}
	p, ok := ParseName(name)
	if !ok || p.Prefix != "ma020_rt" || !p.Created.Equal(t0) || p.PID != 4242 || p.Rand != "deadbeef" {
		t.Fatalf("parse = %+v ok=%v", p, ok)
	}
}

func TestBuildNameRejects(t *testing.T) {
	for _, tc := range []struct {
		prefix, rand string
		pid          int
	}{
		{"Upper_", "deadbeef", 1},
		{"", "deadbeef", 1},
		{"1abc_", "deadbeef", 1},
		{"a-b_", "deadbeef", 1},
		{"x_", "DEADBEEF", 1},
		{"x_", "dead", 1},
		{"x_", "deadbeef", -1},
		{strings.Repeat("a", 60), "deadbeef", 1},
	} {
		if n, err := BuildName(tc.prefix, t0, tc.pid, tc.rand); err == nil {
			t.Errorf("BuildName(%q,%d,%q) = %q, want error", tc.prefix, tc.pid, tc.rand, n)
		}
	}
}

func TestParseNameRefusesNonScratch(t *testing.T) {
	for _, n := range []string{
		"igaming_platform_ci_local", "igaming_orch_local", "postgres", "template1",
		"m0102_0123456789abcdef",             // legacy prefix+16hex form
		"x_20261016083005p42xDEADBEEF",       // uppercase hex
		"x_20261016083005p42xdeadbee",        // short suffix
		"x_20261016083005pxdeadbeef",         // no pid
		"20261016083005p42xdeadbeef",         // no prefix
		"x_20261316083005p42xdeadbeef",       // month 13
		"x_20261016083005p42xdeadbeef_extra", // trailing junk
		"X_20261016083005p42xdeadbeef",       // uppercase prefix
	} {
		if _, ok := ParseName(n); ok {
			t.Errorf("ParseName(%q) matched, want refusal", n)
		}
	}
}

func TestSelectReapable(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	mk := func(age time.Duration, rnd string) string {
		n, err := BuildName("m0102_", now.Add(-age), 7, rnd)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	old, recent, future, boundary := mk(5*time.Hour, "0a1b2c3d"), mk(30*time.Minute, "0a1b2c3e"), mk(-2*time.Hour, "0a1b2c3f"), mk(2*time.Hour, "0a1b2c40")
	cands := []Candidate{
		{Name: old, Owner: "igaming"},
		{Name: recent, Owner: "igaming"},
		{Name: future, Owner: "igaming"},
		{Name: boundary, Owner: "igaming"},
		{Name: mk(5*time.Hour, "11111111"), Owner: "igaming", Sessions: 1},
		{Name: mk(5*time.Hour, "22222222"), Owner: "someone_else"},
		{Name: mk(5*time.Hour, "33333333"), Owner: "igaming", IsTemplate: true},
		{Name: "igaming_platform_ci_local", Owner: "igaming"},
		{Name: "postgres", Owner: "igaming"},
		{Name: "m0102_0123456789abcdef", Owner: "igaming"},
	}
	got := SelectReapable(now, 2*time.Hour, "igaming", cands)
	if len(got) != 2 || got[0] != old || got[1] != boundary {
		t.Fatalf("got %v, want [%s %s]", got, old, boundary)
	}
	if g := SelectReapable(now, 0, "igaming", cands); g != nil {
		t.Errorf("zero maxAge selected %v", g)
	}
	if g := SelectReapable(now, -time.Hour, "igaming", cands); g != nil {
		t.Errorf("negative maxAge selected %v", g)
	}
	if g := SelectReapable(now, time.Hour, "", cands); g != nil {
		t.Errorf("empty owner selected %v", g)
	}
}
