//go:build integration

package rg

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestRGWindowStartsS3 exercises the PRODUCTION window expression (WindowStarts) across DST
// transitions. Expected instants are derived by hand from the zones' transition rules (comments).
func TestRGWindowStartsS3(t *testing.T) {
	w := newS1World(t)
	utc := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm.UTC()
	}
	start := func(sessionTZ string, spec WindowSpec, at time.Time) time.Time {
		t.Helper()
		var got []time.Time
		wantOK(t, "window starts", do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			if sessionTZ != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('TimeZone', $1, true)`, sessionTZ); err != nil {
					return err
				}
			}
			_, s, err := WindowStarts(ctx, tx, []WindowSpec{spec}, &at)
			got = s
			return err
		}))
		return got[0].UTC()
	}
	day := func(tz string) WindowSpec { return WindowSpec{Period: PeriodCalendarDay, CalendarTZ: tz} }
	cases := []struct {
		name string
		spec WindowSpec
		at   string
		want string
	}{
		// Europe/London springs forward 2025-03-30 01:00 GMT -> 02:00 BST: that local day is 23 hours long.
		{"london spring-forward day (GMT at 00:00)", day("Europe/London"), "2025-03-30T12:00:00Z", "2025-03-30T00:00:00Z"},
		// The next day starts at 00:00 BST = 23:00Z the day before.
		{"london day after spring-forward (BST)", day("Europe/London"), "2025-03-31T00:30:00Z", "2025-03-30T23:00:00Z"},
		// America/New_York falls back 2025-11-02 02:00 EDT -> 01:00 EST: the day is 25 hours long; it starts 00:00 EDT = 04:00Z.
		{"new york fall-back day (25h)", day("America/New_York"), "2025-11-02T20:00:00Z", "2025-11-02T04:00:00Z"},
		{"new york day after fall-back (EST)", day("America/New_York"), "2025-11-03T20:00:00Z", "2025-11-03T05:00:00Z"},
		// Africa/Cairo springs forward AT midnight 2024-04-26 00:00 -> 01:00: local 00:00 does not exist; the first
		// instant of that day is the transition, 2024-04-25T22:00Z (00:00 at +02:00).
		{"cairo midnight gap", day("Africa/Cairo"), "2024-04-26T10:00:00Z", "2024-04-25T22:00:00Z"},
		// America/Havana falls back AT 01:00 -> 00:00 on 2024-11-03: local 00:00 occurs TWICE (04:00Z in CDT, 05:00Z in
		// CST). The day starts at the FIRST: 04:00Z. A naive `local AT TIME ZONE` returns 05:00Z, which is AFTER an
		// instant of 04:30Z (window start in the future = under-count, fail open) and skips the first hour at 05:30Z.
		{"havana ambiguous midnight, first occurrence", day("America/Havana"), "2024-11-03T04:30:00Z", "2024-11-03T04:00:00Z"},
		{"havana ambiguous midnight, second occurrence", day("America/Havana"), "2024-11-03T05:30:00Z", "2024-11-03T04:00:00Z"},
		{"havana later that day", day("America/Havana"), "2024-11-03T12:00:00Z", "2024-11-03T04:00:00Z"},
		// Calendar week across the Berlin spring-forward (Sunday 2025-03-30 02:00 CET -> 03:00 CEST).
		// 2025-03-31 is a Monday. Week starting Monday: Monday 00:00 CEST = 2025-03-30T22:00Z.
		{"berlin week starting Monday", WindowSpec{Period: PeriodCalendarWeek, CalendarTZ: "Europe/Berlin", WeekStart: 1}, "2025-03-31T10:00:00Z", "2025-03-30T22:00:00Z"},
		// Week starting Sunday (7): Sunday 2025-03-30 00:00 CET (+01:00) = 2025-03-29T23:00Z.
		{"berlin week starting Sunday (the DST day)", WindowSpec{Period: PeriodCalendarWeek, CalendarTZ: "Europe/Berlin", WeekStart: 7}, "2025-03-31T10:00:00Z", "2025-03-29T23:00:00Z"},
		// Wednesday (3) week start, asked on the Wednesday itself and on the Tuesday before it.
		{"week start = today", WindowSpec{Period: PeriodCalendarWeek, CalendarTZ: "UTC", WeekStart: 3}, "2025-03-12T09:00:00Z", "2025-03-12T00:00:00Z"},
		{"week start 6 days back", WindowSpec{Period: PeriodCalendarWeek, CalendarTZ: "UTC", WeekStart: 3}, "2025-03-11T09:00:00Z", "2025-03-05T00:00:00Z"},
		// Calendar month: Berlin month start 2025-03-01 00:00 CET = 2025-02-28T23:00Z; April starts in CEST (+02:00).
		{"berlin month, March", WindowSpec{Period: PeriodCalendarMonth, CalendarTZ: "Europe/Berlin"}, "2025-03-31T10:00:00Z", "2025-02-28T23:00:00Z"},
		{"berlin month, April (after DST)", WindowSpec{Period: PeriodCalendarMonth, CalendarTZ: "Europe/Berlin"}, "2025-04-15T10:00:00Z", "2025-03-31T22:00:00Z"},
		// Month start on a half-hour zone: Asia/Kolkata (+05:30) 2025-03-01 00:00 = 2025-02-28T18:30Z.
		{"kolkata month", WindowSpec{Period: PeriodCalendarMonth, CalendarTZ: "Asia/Kolkata"}, "2025-03-12T00:00:00Z", "2025-02-28T18:30:00Z"},
		// Year boundary.
		{"new year in Tokyo", day("Asia/Tokyo"), "2025-01-01T00:30:00Z", "2024-12-31T15:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := utc(tc.at)
			if got := start("", tc.spec, at); !got.Equal(utc(tc.want)) {
				t.Fatalf("%s: window start at %s = %s, want %s", tc.name, tc.at, got.Format(time.RFC3339), tc.want)
			}
			// The result never depends on the SESSION time zone (Go local time / session TimeZone are never used).
			for _, tz := range []string{"Pacific/Kiritimati", "America/Los_Angeles", "Europe/London"} {
				if got := start(tz, tc.spec, at); !got.Equal(utc(tc.want)) {
					t.Fatalf("%s: with session TimeZone %s the start moved to %s", tc.name, tz, got.Format(time.RFC3339))
				}
			}
			if got := start("", tc.spec, at); got.After(at) {
				t.Fatalf("%s: window start %s is after the instant %s", tc.name, got, at)
			}
		})
	}

	t.Run("rolling windows are fixed hour intervals even when the session zone has a DST jump", func(t *testing.T) {
		// 2025-03-31T00:30Z is 01:30 BST; 24 real hours earlier is 2025-03-30T00:30Z. `- interval '1 day'` in a London
		// session would give 2025-03-29T23:30Z-equivalent local arithmetic (23 hours) - the rolling kinds must not.
		at := utc("2025-03-31T00:30:00Z")
		for _, c := range []struct {
			p    PeriodKind
			back time.Duration
		}{{PeriodRolling24h, 24 * time.Hour}, {PeriodRolling7d, 168 * time.Hour}, {PeriodRolling30d, 720 * time.Hour}} {
			if got := start("Europe/London", WindowSpec{Period: c.p}, at); !got.Equal(at.Add(-c.back)) {
				t.Fatalf("%s: start %s, want exactly %s earlier (%s)", c.p, got, c.back, at.Add(-c.back))
			}
		}
	})

	t.Run("invalid specs are refused before any SQL", func(t *testing.T) {
		for _, s := range []WindowSpec{
			{Period: "rolling_1d"}, {Period: PeriodCalendarDay}, {Period: PeriodCalendarWeek, CalendarTZ: "UTC", WeekStart: 0},
			{Period: PeriodCalendarWeek, CalendarTZ: "UTC", WeekStart: 8},
		} {
			if err := do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := WindowStarts(ctx, tx, []WindowSpec{s}, nil)
				return err
			}); err == nil {
				t.Fatalf("%+v must be refused", s)
			}
		}
		if err := do(w.rt, sess{}, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := WindowStarts(ctx, tx, nil, nil)
			return err
		}); err == nil {
			t.Fatal("zero windows must be refused")
		}
	})
}
