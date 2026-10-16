package reconciliation

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// R3-OBS-BOUND-1 / A11: the pure rules of a bounded, resumable payment statement fetch.

func us(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func lineAt(ref string, at time.Time) statement.PaymentStatementLine {
	return statement.PaymentStatementLine{ProviderID: "p", ProviderReference: ref, OccurredAt: at}
}

func TestValidateBoundedWindow_Table(t *testing.T) {
	c0 := us("2026-01-01T00:00:00Z")
	c1 := us("2026-01-01T01:00:00Z")
	req := func(cursor time.Time) statement.PaymentFetchRequest {
		r := statement.PaymentFetchRequest{PeriodStart: c0, PeriodEnd: c1.Add(time.Hour)}
		if !cursor.IsZero() {
			r.ContinueFrom, r.PeriodStart = cursor, cursor
		}
		return r
	}
	cases := []struct {
		name    string
		stmt    statement.PaymentStatement
		req     statement.PaymentFetchRequest
		bound   int
		wantErr error
	}{
		{"no bound, no cursor: anything goes", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0), lineAt("b", c0), lineAt("c", c0)}}, req(time.Time{}), 0, nil},
		{"under the bound", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0)}}, req(time.Time{}), 2, nil},
		{"exactly the bound is allowed", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0), lineAt("b", c0)}}, req(time.Time{}), 2, nil},
		{"over the bound without Truncated is refused", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0), lineAt("b", c0), lineAt("c", c0)}}, req(time.Time{}), 2, ErrPaymentStatementTooLarge},
		{"cursor: starts exactly at it", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0)}}, req(c0), 0, nil},
		{"cursor: a GAP (starts after it) skips a window", statement.PaymentStatement{CoverageStart: c0.Add(time.Second), CoverageEnd: c1}, req(c0), 0, ErrPaymentStatementCoverageGap},
		{"cursor: an OVERLAP (starts before it) would count lines twice", statement.PaymentStatement{CoverageStart: c0.Add(-time.Second), CoverageEnd: c1}, req(c0), 0, ErrPaymentStatementCoverageGap},
		{"cursor: a line dated before it was already covered", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0.Add(-time.Microsecond))}}, req(c0), 0, ErrPaymentStatementCoverageGap},
		{"cursor: a line AT the cursor is in this window", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0)}}, req(c0), 0, nil},
		{"truncated: well-formed", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1, Truncated: true,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0), lineAt("b", c1.Add(-time.Microsecond))}}, req(c0), 2, nil},
		{"truncated: no progress past the cursor", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c0, Truncated: true}, req(c0), 2, ErrPaymentStatementCoverageGap},
		{"truncated: ends after the requested window", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1.Add(2 * time.Hour), Truncated: true,
			Lines: []statement.PaymentStatementLine{lineAt("a", c0)}}, req(c0), 2, ErrPaymentStatementCoverageGap},
		{"truncated: a line at the coverage end is not inside the window it vouches for", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1, Truncated: true,
			Lines: []statement.PaymentStatementLine{lineAt("a", c1)}}, req(c0), 2, ErrPaymentStatementCoverageGap},
		{"truncated: a line after the coverage end", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1, Truncated: true,
			Lines: []statement.PaymentStatementLine{lineAt("a", c1.Add(time.Second))}}, req(c0), 2, ErrPaymentStatementCoverageGap},
		{"truncated without a cursor (first run) still needs its lines inside the window", statement.PaymentStatement{CoverageStart: c0, CoverageEnd: c1, Truncated: true,
			Lines: []statement.PaymentStatementLine{lineAt("a", c1)}}, req(time.Time{}), 2, ErrPaymentStatementCoverageGap},
	}
	for _, tc := range cases {
		err := validateBoundedWindow(tc.stmt, tc.req, PaymentStatementOptions{RunLineBound: tc.bound})
		if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestPaymentSweepHelpers_NilSourceAndDiscriminator(t *testing.T) {
	var typedNil *fakeNilSource
	for _, src := range []statement.PaymentStatementSource{nil, typedNil} {
		if !isNilPaymentSource(src) {
			t.Errorf("%T must read as nil", src)
		}
	}
	if isNilPaymentSource(fakePaySource{id: "x"}) {
		t.Error("a real value is not nil")
	}
	if discPart("psp-a_1.x") != "psp-a_1.x" {
		t.Error("a safe provider id is used as is")
	}
	for _, bad := range []string{"", "a b", "a/b", "ü", string(make([]byte, 101))} {
		if discPart(bad) != "invalid_provider_id" {
			t.Errorf("%q must not become a discriminator part", bad)
		}
	}
}

type fakeNilSource struct {
	statement.PaymentStatementSource
}

// A truncated run is not a success (freshness does not advance), keeps counting its mismatches, shows up in the
// unreconciled gauge, and is cleared by the next complete run; a configured provider with no source and a plain
// failed run are unreconciled too, and a clean run is never listed. This is the "never a clean claim" rule.
func TestTracker_TruncatedAndMissingSourceAreNeverClean(t *testing.T) {
	tr, reader, _ := newTestTracker(t)
	tenant := uuid.New()
	sweep := func(outs ...StreamOutcome) {
		gen := tr.beginSweep()
		tr.recordOutcomes(gen, []SweepOutcome{{TenantID: tenant, PaymentStatement: outs}}, nil)
		tr.endSweep(gen, true)
	}
	clean := StreamOutcome{ProviderID: "prov-a", Run: Run{Status: StatusClean}}
	truncated := StreamOutcome{ProviderID: "prov-a", Truncated: true, NotReconciled: NotReconciledTruncated, Run: Run{Status: StatusMismatchesFound}, Mismatches: 2}
	missing := StreamOutcome{ProviderID: "prov-b", NotReconciled: NotReconciledNoSource, Err: errors.New("no source")}
	failed := StreamOutcome{ProviderID: "prov-c", Err: errors.New("fetch failed")}

	sweep(clean, missing, failed)
	got := tr.PaymentUnreconciled()
	if got[NotReconciledNoSource] != 1 || got["run_failed"] != 1 || got[NotReconciledTruncated] != 0 || len(got) != 2 {
		t.Fatalf("unreconciled after clean+missing+failed: %v", got)
	}
	firstSuccess := observed(t, reader)["reconciliation_last_success_unix_seconds"]["payment_statement"]
	if firstSuccess != 0 {
		t.Fatalf("a provider that never succeeded must hold payment_statement freshness at 0, got %v", firstSuccess)
	}

	sweep(truncated, missing, failed)
	got = tr.PaymentUnreconciled()
	if got[NotReconciledTruncated] != 1 || got[NotReconciledNoSource] != 1 || got["run_failed"] != 1 {
		t.Fatalf("unreconciled with a truncated pair: %v", got)
	}
	// The truncated pair's mismatches still count, and the result label is its own.
	if m := observed(t, reader)["reconciliation_mismatches_open"]["payment_statement"]; m != 2 {
		t.Fatalf("a truncated run's mismatches must still be open, got %v", m)
	}
	if n := observed(t, reader)["reconciliation_runs_total"]; n["payment_statement"] < 1 {
		t.Fatalf("runs_total must count the sweeps: %v", n)
	}

	// The next complete run for the pair clears it; the other pairs stay unreconciled.
	sweep(clean, missing, failed)
	got = tr.PaymentUnreconciled()
	if got[NotReconciledTruncated] != 0 || got[NotReconciledNoSource] != 1 || got["run_failed"] != 1 {
		t.Fatalf("a clean run must clear only its own pair: %v", got)
	}
	// ... and the pair that is repaired (source registered, fetch ok) leaves the gauge.
	sweep(clean, StreamOutcome{ProviderID: "prov-b", Run: Run{Status: StatusClean}}, StreamOutcome{ProviderID: "prov-c", Run: Run{Status: StatusClean}})
	if got := tr.PaymentUnreconciled(); len(got) != 0 {
		t.Fatalf("every pair reconciled: want an empty gauge, got %v", got)
	}
}

// A skipped outcome (lock contention or a stale cursor) never changes a pair's state: it neither clears a
// truncated/failed flag nor invents one.
func TestTracker_SkippedKeepsUnreconciledState(t *testing.T) {
	tr, _, _ := newTestTracker(t)
	tenant := uuid.New()
	run := func(o StreamOutcome) {
		gen := tr.beginSweep()
		tr.recordOutcomes(gen, []SweepOutcome{{TenantID: tenant, PaymentStatement: []StreamOutcome{o}}}, nil)
		tr.endSweep(gen, true)
	}
	run(StreamOutcome{ProviderID: "prov-a", Truncated: true, NotReconciled: NotReconciledTruncated, Run: Run{Status: StatusClean}})
	run(StreamOutcome{ProviderID: "prov-a", Skipped: true})
	if got := tr.PaymentUnreconciled(); got[NotReconciledTruncated] != 1 {
		t.Fatalf("a skipped run must not clear a truncated pair: %v", got)
	}
}
