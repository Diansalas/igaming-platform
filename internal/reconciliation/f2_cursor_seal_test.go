package reconciliation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

type f2SealOnly struct{}

func (f2SealOnly) SealStatementImport(statement.ImportSealInput) (string, string, error) {
	return "s", "k", nil
}

type f2SealVerify struct{ f2SealOnly }

func (f2SealVerify) VerifyStatementImport(statement.ImportSealInput, string, string) error {
	return nil
}

// F2 / SR11-10 startup gate: production with registered sources needs a sealer that also verifies.
func TestF2_VerifyPaymentCursorSealing(t *testing.T) {
	prev := DefaultImportSealer()
	t.Cleanup(func() { SetDefaultImportSealer(prev) })
	cases := []struct {
		sealer     statement.ImportSealer
		production bool
		sources    int
		wantErr    bool
	}{
		{nil, true, 1, true},
		{f2SealOnly{}, true, 1, true},
		{f2SealVerify{}, true, 1, false},
		{nil, true, 0, false},
		{nil, false, 1, false},
	}
	for i, c := range cases {
		SetDefaultImportSealer(c.sealer)
		err := VerifyPaymentCursorSealing(c.production, c.sources)
		if (err != nil) != c.wantErr || (err != nil && !errors.Is(err, ErrPaymentCursorSealingUnavailable)) {
			t.Fatalf("case %d: err = %v, wantErr %v", i, err, c.wantErr)
		}
	}
}

// SR11-11: the anomaly tail carries the count and the digest and always fits the discriminator CHECK.
func TestF2_CursorAnomalyPhase(t *testing.T) {
	st := cursorState{unbacked: 3, unbackedDigest: "0123456789abcdef"}
	if got := cursorAnomalyPhase("mock-payments", st); got != "cursor_anomaly:n3:0123456789abcdef" {
		t.Fatalf("got %q", got)
	}
	for _, p := range []string{strings.Repeat("p", 100), "bad id\x1b"} {
		got := cursorAnomalyPhase(p, cursorState{unbacked: 987654321, unbackedDigest: "0123456789abcdef"})
		disc := "stream:payment_statement:provider:" + discPart(p) + ":" + got
		if len(disc) > cursorAnomalyDiscMax || !strings.HasPrefix(got, "cursor_anomaly:") || !strings.Contains(got, "01234567") {
			t.Fatalf("%q -> %q (%d)", p, disc, len(disc))
		}
	}
}

type f2RealSrc struct{}

func (f2RealSrc) Label() string      { return "f2 real test source" }
func (f2RealSrc) ProviderID() string { return "psp-f2" }
func (f2RealSrc) Fetch(context.Context, statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	return statement.PaymentStatement{}, nil
}

type f2LagSrc struct {
	f2RealSrc
	lag time.Duration
}

func (s f2LagSrc) StatementSettleLag() time.Duration { return s.lag }

type f2SynthSrc struct{ f2RealSrc }

func (f2SynthSrc) SyntheticComponent() {}

// LF-A11-2: settle-lag resolution (declared, default for a real source, 0 for a MOCK) and bounds.
func TestF2_PaymentSettleLag(t *testing.T) {
	if lag, err := paymentSettleLag(f2RealSrc{}); err != nil || lag != statement.DefaultStatementSettleLag {
		t.Fatalf("a real source with no declaration gets the default: %s %v", lag, err)
	}
	if lag, err := paymentSettleLag(f2SynthSrc{}); err != nil || lag != 0 {
		t.Fatalf("a MOCK with no declaration gets 0: %s %v", lag, err)
	}
	if lag, err := paymentSettleLag(f2LagSrc{lag: time.Hour}); err != nil || lag != time.Hour {
		t.Fatalf("a declared lag is used: %s %v", lag, err)
	}
	if lag, err := paymentSettleLag(f2LagSrc{lag: 0}); err != nil || lag != 0 {
		t.Fatalf("a declared zero lag is valid: %s %v", lag, err)
	}
	for _, bad := range []time.Duration{-1, statement.MaxStatementSettleLag + 1} {
		if _, err := paymentSettleLag(f2LagSrc{lag: bad}); !errors.Is(err, ErrPaymentStatementInvalid) {
			t.Fatalf("lag %s must be refused: %v", bad, err)
		}
	}
	if _, err := paymentSettleLag(f2LagSrc{lag: statement.MaxStatementSettleLag}); err != nil {
		t.Fatalf("the maximum itself is valid: %v", err)
	}
}

// LF-A11-2a / LF-A11-6 (validateBoundedWindow): the completeness horizon applies in continuation mode only, and a
// truncated page must be in time order.
func TestF2_ValidateBoundedWindow_HorizonAndOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	line := func(at time.Time) statement.PaymentStatementLine {
		return statement.PaymentStatementLine{OccurredAt: at}
	}
	req := statement.PaymentFetchRequest{PeriodEnd: t0.Add(time.Hour)}
	beyond := statement.PaymentStatement{CoverageStart: t0, CoverageEnd: t0.Add(time.Hour + time.Microsecond)}
	if err := validateBoundedWindow(beyond, req, PaymentStatementOptions{Continuation: true}); !errors.Is(err, ErrPaymentStatementCoverageGap) {
		t.Fatalf("continuation: CoverageEnd beyond PeriodEnd must be refused: %v", err)
	}
	if err := validateBoundedWindow(beyond, req, PaymentStatementOptions{}); err != nil {
		t.Fatalf("legacy mode keeps its pre-F2 behaviour (only now + skew is checked): %v", err)
	}
	at := statement.PaymentStatement{CoverageStart: t0, CoverageEnd: t0.Add(time.Hour)}
	if err := validateBoundedWindow(at, req, PaymentStatementOptions{Continuation: true}); err != nil {
		t.Fatalf("CoverageEnd == PeriodEnd is allowed: %v", err)
	}
	ordered := statement.PaymentStatement{CoverageStart: t0, CoverageEnd: t0.Add(time.Minute), Truncated: true,
		Lines: []statement.PaymentStatementLine{line(t0), line(t0), line(t0.Add(time.Second))}}
	if err := validateBoundedWindow(ordered, req, PaymentStatementOptions{Continuation: true}); err != nil {
		t.Fatalf("a time-ordered page (ties allowed) passes: %v", err)
	}
	unordered := ordered
	unordered.Lines = []statement.PaymentStatementLine{line(t0.Add(time.Second)), line(t0)}
	if err := validateBoundedWindow(unordered, req, PaymentStatementOptions{Continuation: true}); !errors.Is(err, ErrPaymentStatementOutOfOrder) {
		t.Fatalf("an out-of-order truncated page must be refused: %v", err)
	}
	notTruncated := unordered
	notTruncated.Truncated = false
	if err := validateBoundedWindow(notTruncated, req, PaymentStatementOptions{Continuation: true}); err != nil {
		t.Fatalf("a complete (non-truncated) window may list lines in any order: %v", err)
	}
}
