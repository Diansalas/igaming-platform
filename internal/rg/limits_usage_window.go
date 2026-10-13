package rg

// ADR 0115 SLICE S3: usage-window starts (section 1.2).
//
// Status: IMPLEMENTED (read side only). Nothing in this file refuses a deposit, a wager, a launch or
// a login (S4).
//
// Every window start is computed IN SQL from one statement-level instant (clock_timestamp(), or the
// caller's replay instant); Go local time is never used:
//
//   - rolling windows subtract fixed HOUR intervals (24 / 168 / 720 hours), never '1 day', which is
//     DST-sensitive;
//   - calendar windows are date_trunc(unit, asof AT TIME ZONE tz) AT TIME ZONE tz. The week start is
//     configuration (week_start 1..7, ISO day of week: 1 = Monday ... 7 = Sunday).
//
// DST: `timestamp AT TIME ZONE tz` resolves an AMBIGUOUS local midnight (the clock repeats it, e.g.
// America/Havana on 2024-11-03) to the LATER occurrence, which can lie AFTER the current instant or
// skip the first hour of the local day - an under-count of the window, i.e. a fail-OPEN. The start is
// therefore the EARLIEST instant (probed in 30-minute steps up to 2 hours back) whose local time is
// still at or after the local boundary. A boundary inside a spring-forward GAP (Africa/Cairo
// 2024-04-26 00:00) resolves to the transition instant, which is the first instant of that local day.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxUsageWindows bounds the number of windows of one measurement (one per allowed period kind).
const MaxUsageWindows = 12

// WindowSpec names one window: the period kind and, for calendar windows, the timezone and week
// start of the composed configuration (rg_limit_config_compose: the floor's calendar_tz / week_start).
type WindowSpec struct {
	Period     PeriodKind
	CalendarTZ string // IANA name; ignored for rolling windows
	WeekStart  int    // 1..7 ISO day of week the calendar week starts on; ignored unless calendar_week
}

func (w WindowSpec) isCalendar() bool {
	return w.Period == PeriodCalendarDay || w.Period == PeriodCalendarWeek || w.Period == PeriodCalendarMonth
}

func (w WindowSpec) validate() error {
	known := false
	for _, p := range PeriodKinds {
		if p == w.Period {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("rg: unknown period kind %q", w.Period)
	}
	if w.isCalendar() {
		if w.CalendarTZ == "" || len(w.CalendarTZ) > 64 {
			return fmt.Errorf("rg: calendar window %s needs a calendar_tz", w.Period)
		}
		if w.Period == PeriodCalendarWeek && (w.WeekStart < 1 || w.WeekStart > 7) {
			return fmt.Errorf("rg: calendar_week needs week_start 1..7, got %d", w.WeekStart)
		}
	}
	return nil
}

// windowArgs flattens specs into the three array arguments of windowStartsSQL. A rolling window
// carries tz 'UTC' and week start 1 (unused) so no NULL reaches AT TIME ZONE.
func windowArgs(specs []WindowSpec) (periods, tzs []string, weekStarts []int32, err error) {
	if len(specs) == 0 || len(specs) > MaxUsageWindows {
		return nil, nil, nil, fmt.Errorf("rg: a measurement needs 1..%d windows, got %d", MaxUsageWindows, len(specs))
	}
	for _, s := range specs {
		if err := s.validate(); err != nil {
			return nil, nil, nil, err
		}
		tz, ws := "UTC", int32(1)
		if s.isCalendar() {
			tz = s.CalendarTZ
		}
		if s.Period == PeriodCalendarWeek {
			ws = int32(s.WeekStart)
		}
		periods = append(periods, string(s.Period))
		tzs = append(tzs, tz)
		weekStarts = append(weekStarts, ws)
	}
	return periods, tzs, weekStarts, nil
}

// windowStartExpr is the SQL expression of window number i (1-based) as a timestamptz, over the
// instant column `a.t` of the CTE `asof`, with the three window arrays at parameters pP / pTZ / pWS.
// It is ONE definition used by every usage query and by WindowStarts, so a boundary test exercises
// the production expression.
func windowStartExpr(i, pP, pTZ, pWS int) string {
	return fmt.Sprintf(`(SELECT
		CASE w.p
		  WHEN 'rolling_24h' THEN a.t - interval '24 hours'
		  WHEN 'rolling_7d'  THEN a.t - interval '168 hours'
		  WHEN 'rolling_30d' THEN a.t - interval '720 hours'
		  WHEN 'calendar_day' THEN cal.s
		  WHEN 'calendar_week' THEN cal.s
		  WHEN 'calendar_month' THEN cal.s
		END
	  FROM (SELECT ($%[2]d::text[])[%[1]d] AS p, ($%[3]d::text[])[%[1]d] AS tz, ($%[4]d::int[])[%[1]d] AS ws) w
	  CROSS JOIN LATERAL (SELECT a.t AT TIME ZONE w.tz AS l) loc
	  CROSS JOIN LATERAL (SELECT CASE w.p
	        WHEN 'calendar_day'   THEN date_trunc('day', loc.l)
	        WHEN 'calendar_month' THEN date_trunc('month', loc.l)
	        WHEN 'calendar_week'  THEN date_trunc('day', loc.l) - (((extract(isodow FROM loc.l)::int - w.ws + 7) %% 7) * interval '1 day')
	      END AS m) mid
	  CROSS JOIN LATERAL (SELECT min(c) AS s FROM generate_series(0, 4) k
	        CROSS JOIN LATERAL (SELECT (mid.m AT TIME ZONE w.tz) - k * interval '30 minutes' AS c) x
	       WHERE mid.m IS NOT NULL AND (x.c AT TIME ZONE w.tz) >= mid.m) cal)`, i, pP, pTZ, pWS)
}

// windowsCTE is the CTE pair `asof` (the one statement instant, parameter pAt, NULL = clock_timestamp())
// and `win` (columns s1..sN).
func windowsCTE(n, pAt, pP, pTZ, pWS int) string {
	cols := make([]string, n)
	for i := 1; i <= n; i++ {
		cols[i-1] = windowStartExpr(i, pP, pTZ, pWS) + fmt.Sprintf(" AS s%d", i)
	}
	return fmt.Sprintf(`asof AS MATERIALIZED (SELECT COALESCE($%d::timestamptz, clock_timestamp()) AS t),
	win AS MATERIALIZED (SELECT %s FROM asof a)`, pAt, strings.Join(cols, ",\n\t  "))
}

// WindowStarts evaluates the window start of every spec at the database instant `at` (nil = the
// current clock_timestamp()), returning the instant used. It is the exported probe of the production
// expression (DST / boundary tests, denial-audit replay).
func WindowStarts(ctx context.Context, tx pgx.Tx, specs []WindowSpec, at *time.Time) (asOf time.Time, starts []time.Time, err error) {
	periods, tzs, wss, err := windowArgs(specs)
	if err != nil {
		return time.Time{}, nil, err
	}
	cols := make([]string, len(specs))
	for i := range specs {
		cols[i] = fmt.Sprintf("win.s%d", i+1)
	}
	q := "WITH " + windowsCTE(len(specs), 1, 2, 3, 4) + " SELECT (SELECT t FROM asof), " + strings.Join(cols, ", ") + " FROM win"
	dest := make([]any, 0, len(specs)+1)
	dest = append(dest, &asOf)
	starts = make([]time.Time, len(specs))
	nullable := make([]*time.Time, len(specs))
	for i := range specs {
		dest = append(dest, &nullable[i])
	}
	if err := tx.QueryRow(ctx, q, at, periods, tzs, wss).Scan(dest...); err != nil {
		return time.Time{}, nil, fmt.Errorf("rg: window starts: %w", err)
	}
	for i, p := range nullable {
		if p == nil {
			return time.Time{}, nil, fmt.Errorf("rg: window %d (%s) has no start", i, specs[i].Period)
		}
		starts[i] = *p
	}
	return asOf, starts, nil
}
