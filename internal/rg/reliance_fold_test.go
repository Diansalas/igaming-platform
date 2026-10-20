package rg

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// lic25: foldCompose is the strictest fold over the compositions of the jurisdictions a tenant relies on, with the rules of
// rg_limit_config_compose. Pure; every rule is pinned by a case that fails if the rule is changed to its opposite.

func bptr(b bool) *bool     { return &b }
func i64ptr(i int64) *int64 { return &i }
func sptr(s string) *string { return &s }

func member(over func(*rawCompose)) rawCompose {
	r := rawCompose{Found: true, Enabled: bptr(false), Mandatory: bptr(false), CeilingNone: bptr(true), Periods: []string{"rolling_24h"},
		CalendarTZ: sptr("UTC"), WeekStart: func() *int16 { v := int16(1); return &v }(), CooldownSecs: i64ptr(10), Scope: sptr("account")}
	if over != nil {
		over(&r)
	}
	return r
}

func TestFoldCompose_OneMemberIsIdentity_AndEmptyIsNotFound(t *testing.T) {
	m := member(func(r *rawCompose) {
		r.Ceiling, r.CeilingNone = sptr("100"), bptr(false)
		r.VersionIDs = []uuid.UUID{uuid.New()}
	})
	got, missing, conflict := foldCompose([]rawCompose{m})
	if !reflect.DeepEqual(got, m) || missing || conflict {
		t.Fatalf("one member must be returned unchanged: %+v", got)
	}
	got, missing, _ = foldCompose(nil)
	if got.Found || missing {
		t.Fatalf("no member: not found, nothing missing: %+v %v", got, missing)
	}
	got, missing, _ = foldCompose([]rawCompose{{}, {}})
	if got.Found || !missing {
		t.Fatalf("members without a floor: not found and missing: %+v %v", got, missing)
	}
}

func TestFoldCompose_MembersWithoutAFloorAreSkippedAndReported(t *testing.T) {
	a := member(func(r *rawCompose) { r.Ceiling, r.CeilingNone = sptr("100"), bptr(false) })
	got, missing, _ := foldCompose([]rawCompose{{}, a})
	if !got.Found || !missing || got.Ceiling == nil || *got.Ceiling != "100" {
		t.Fatalf("the member that has a floor still applies, the other is reported missing: %+v %v", got, missing)
	}
}

func TestFoldCompose_StrictestRules(t *testing.T) {
	id1, id2 := uuid.MustParse("00000000-0000-0000-0000-000000000002"), uuid.MustParse("00000000-0000-0000-0000-000000000001")
	a := member(func(r *rawCompose) {
		r.Ceiling, r.CeilingNone = sptr("1000"), bptr(false)
		r.CooldownSecs, r.BreakSecs = i64ptr(7200), i64ptr(60)
		r.Enabled, r.Mandatory = bptr(true), bptr(false)
		r.Periods, r.Scope = []string{"rolling_24h", "calendar_day"}, sptr("tenant_person")
		r.VersionIDs = []uuid.UUID{id1}
		r.LossNeg, r.CountBonus = bptr(true), bptr(false)
		r.TTLNone = bptr(false)
		r.TTLSecs = i64ptr(3600)
	})
	b := member(func(r *rawCompose) {
		r.Ceiling, r.CeilingNone = sptr("99"), bptr(false)
		r.CooldownSecs, r.BreakSecs = i64ptr(600), i64ptr(120)
		r.Enabled, r.Mandatory = bptr(false), bptr(true)
		r.Periods, r.Scope = []string{"rolling_7d", "rolling_24h"}, sptr("platform_person")
		r.VersionIDs = []uuid.UUID{id2, id1}
		r.LossNeg, r.CountBonus = bptr(false), bptr(true)
		r.TTLNone = bptr(true)
		r.TTLSecs = nil
	})
	got, missing, conflict := foldCompose([]rawCompose{a, b})
	if missing || conflict || !got.Found {
		t.Fatalf("unexpected: %+v", got)
	}
	check := func(name string, ok bool) {
		t.Helper()
		if !ok {
			t.Errorf("%s: %+v", name, got)
		}
	}
	check("numeric ceiling is the MINIMUM (not the first, not the maximum)", got.Ceiling != nil && *got.Ceiling == "99")
	check("ceiling none is the AND (one numeric member makes it false)", got.CeilingNone != nil && !*got.CeilingNone)
	check("cooling-off is the MAXIMUM", got.CooldownSecs != nil && *got.CooldownSecs == 7200)
	check("post-session break is the MAXIMUM", got.BreakSecs != nil && *got.BreakSecs == 120)
	check("enabled is the OR", got.Enabled != nil && *got.Enabled)
	check("mandatory is the OR", got.Mandatory != nil && *got.Mandatory)
	check("count-bonus is the OR", got.CountBonus != nil && *got.CountBonus)
	check("loss may go negative is the AND", got.LossNeg != nil && !*got.LossNeg)
	check("allowed periods are the UNION, sorted", reflect.DeepEqual(got.Periods, []string{"calendar_day", "rolling_24h", "rolling_7d"}))
	check("scope is the broadest", got.Scope != nil && *got.Scope == "platform_person")
	check("version ids are the sorted union", reflect.DeepEqual(got.VersionIDs, []uuid.UUID{id2, id1}))
	check("a none-attested pending-intent ttl dominates", got.TTLSecs == nil && got.TTLNone != nil && *got.TTLNone)
	// Symmetry: the fold does not depend on the member order.
	rev, _, _ := foldCompose([]rawCompose{b, a})
	if !reflect.DeepEqual(rev, got) {
		t.Errorf("the fold must not depend on the order of the members:\n%+v\n%+v", rev, got)
	}
}

func TestFoldCompose_CeilingNoneOnlyWhenEveryMemberAttestsNone(t *testing.T) {
	a, b := member(nil), member(nil)
	got, _, _ := foldCompose([]rawCompose{a, b})
	if got.Ceiling != nil || got.CeilingNone == nil || !*got.CeilingNone {
		t.Fatalf("all none: no numeric ceiling, none attested: %+v", got)
	}
	b.Ceiling, b.CeilingNone = sptr("5"), bptr(false)
	got, _, _ = foldCompose([]rawCompose{a, b})
	if got.Ceiling == nil || *got.Ceiling != "5" {
		t.Fatalf("a numeric member bounds the fold: %+v", got)
	}
}

func TestFoldCompose_UnparsableCeilingIsKeptSoTheCallerFailsClosed(t *testing.T) {
	a := member(func(r *rawCompose) { r.Ceiling, r.CeilingNone = sptr("12x"), bptr(false) })
	b := member(func(r *rawCompose) { r.Ceiling, r.CeilingNone = sptr("5"), bptr(false) })
	got, _, _ := foldCompose([]rawCompose{a, b})
	if got.Ceiling == nil || *got.Ceiling != "12x" {
		t.Fatalf("an unparsable ceiling must not be silently dropped in favour of a parsable one: %+v", got)
	}
}

func TestFoldCompose_CalendarDisagreementIsReported(t *testing.T) {
	a := member(nil)
	b := member(func(r *rawCompose) { r.CalendarTZ = sptr("Europe/X") })
	if _, _, conflict := foldCompose([]rawCompose{a, b}); !conflict {
		t.Fatal("different calendar_tz is not comparable and must be reported")
	}
	c := member(func(r *rawCompose) { w := int16(2); r.WeekStart = &w })
	if _, _, conflict := foldCompose([]rawCompose{a, c}); !conflict {
		t.Fatal("different week_start is not comparable and must be reported")
	}
	if _, _, conflict := foldCompose([]rawCompose{a, member(nil)}); conflict {
		t.Fatal("equal calendar settings are not a conflict")
	}
}

func TestFoldCompose_TimeOutDurations(t *testing.T) {
	a := member(func(r *rawCompose) { r.DurationsSecs = []int64{3600, 86400}; r.DurationsAny = bptr(false) })
	b := member(func(r *rawCompose) { r.DurationsSecs = []int64{86400, 604800}; r.DurationsAny = bptr(false) })
	got, _, _ := foldCompose([]rawCompose{a, b})
	if !reflect.DeepEqual(got.DurationsSecs, []int64{3600, 86400, 604800}) {
		t.Fatalf("durations are the UNION: %+v", got.DurationsSecs)
	}
	b.DurationsAny = bptr(true)
	b.DurationsSecs = nil
	got, _, _ = foldCompose([]rawCompose{a, b})
	if got.DurationsAny == nil || !*got.DurationsAny || got.DurationsSecs != nil {
		t.Fatalf("any dominates: %+v", got)
	}
}

func TestSortedDistinct(t *testing.T) {
	if got := sortedDistinct([]string{"b", "a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("%v", got)
	}
}

func TestScopeOf_MatchesTheKYCMapping(t *testing.T) {
	cases := []struct {
		a        Action
		op, prod string
	}{{ActionDeposit, "deposit", ""}, {ActionCasinoWager, "wagering", "casino"}, {ActionCasinoLaunch, "wagering", "casino"}, {ActionSportsbookWager, "wagering", "sportsbook"}}
	for _, c := range cases {
		op, p := scopeOf(c.a)
		got := ""
		if p != nil {
			got = *p
		}
		if op != c.op || got != c.prod {
			t.Errorf("%s: %s/%s, want %s/%s", c.a, op, got, c.op, c.prod)
		}
	}
}
