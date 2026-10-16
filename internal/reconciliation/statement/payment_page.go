package statement

import (
	"errors"
	"sort"
	"time"
)

// ErrPaymentStatementUnpageable means a group of lines sharing ONE instant is
// larger than the per-run bound, so no page boundary exists that neither
// splits the group nor stalls: continuing would either skip or double-count
// lines. The stream refuses it (a P1) rather than guess; the operator raises
// the bound or the source disambiguates its timestamps.
var ErrPaymentStatementUnpageable = errors.New("statement: payment statement has more lines at one instant than the per-run bound")

// PagePaymentLines cuts lines (any order) into the first page of at most
// maxLines lines, for a source that implements MaxLines (R3-OBS-BOUND-1):
//
//   - maxLines <= 0: no bound; all lines are returned sorted by
//     (OccurredAt, ProviderReference), truncated=false, cutoff zero;
//   - otherwise, if len(lines) <= maxLines the same, untruncated;
//   - otherwise the page holds the largest prefix, in (OccurredAt,
//     ProviderReference) order, that ends on an instant boundary: lines with
//     OccurredAt < cutoff, where cutoff is the OccurredAt of the first line
//     left out. A group of lines sharing one instant is never split. The
//     caller sets the statement's CoverageEnd to cutoff and Truncated to true.
//
// When even the first instant's group exceeds maxLines it returns
// ErrPaymentStatementUnpageable. The input slice is not modified.
func PagePaymentLines(lines []PaymentStatementLine, maxLines int) (page []PaymentStatementLine, cutoff time.Time, truncated bool, err error) {
	// Instants are compared at microsecond precision - the precision the stream stores and
	// compares (FetchPaymentStatement truncates both OccurredAt and CoverageEnd) - so two
	// lines that the stream sees at one instant are never separated by a page boundary.
	us := func(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
	sorted := append([]PaymentStatementLine(nil), lines...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !us(sorted[i].OccurredAt).Equal(us(sorted[j].OccurredAt)) {
			return us(sorted[i].OccurredAt).Before(us(sorted[j].OccurredAt))
		}
		return sorted[i].ProviderReference < sorted[j].ProviderReference
	})
	if maxLines <= 0 || len(sorted) <= maxLines {
		return sorted, time.Time{}, false, nil
	}
	// sorted[maxLines] is the first candidate left out; back up to the start of its instant group.
	n := maxLines
	for n > 0 && us(sorted[n-1].OccurredAt).Equal(us(sorted[maxLines].OccurredAt)) {
		n--
	}
	if n == 0 {
		return nil, time.Time{}, false, ErrPaymentStatementUnpageable
	}
	return sorted[:n], us(sorted[n].OccurredAt), true, nil
}
