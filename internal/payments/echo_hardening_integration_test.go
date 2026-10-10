//go:build integration

package payments

// Security/ledger-finance conditions on gov-r33-echo (ADR 0111 24.7): frozen declaration (LOW-1), unknown provider (LOW-2),
// capped terminal-signal audit rows (LOW-3) and the reconciliation hint for an unexpected-echo park (MEDIUM-1).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// LOW-1: the declaration the evidence relies on is the one frozen at wiring. A live manifest that later says Unsupported or
// Unset must not turn a Supported adapter's success-without-echo into an accepted one; a live value that differs in any
// direction makes every success under it ambiguous (even one carrying a matching echo).
func TestEchoHardening_LOW1_DriftFromTheFrozenDeclarationFailsClosed(t *testing.T) {
	for _, drift := range []string{"supported->unsupported", "supported->unset", "unsupported->supported"} {
		for _, withEcho := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/echo=%v", drift, withEcho), func(t *testing.T) {
				frozenSupported := strings.HasPrefix(drift, "supported")
				w := newB13bW(t, "d1", func(p *b13bProvider) { p.declared = frozenSupported })
				// the orchestrator froze the declaration at construction; now the live manifest drifts
				switch drift {
				case "supported->unsupported":
					w.prov.declared = false
				case "supported->unset":
					w.prov.unset = true
				case "unsupported->supported":
					w.prov.declared = true
				}
				mk := noEcho
				if withEcho {
					mk = goodEchoOf
				}
				wr, cl := driveEcho(t, w, "sync", "d1", OutcomeSucceeded, mk)
				w.wantNotSettled(wr, cl)
				// Frozen Unsupported + an echo is an unexpected echo (parks, fail closed); every other drifted shape is ambiguous.
				want := AttemptAmbiguous
				if drift == "unsupported->supported" && withEcho {
					want = AttemptDisputed
				}
				if a := w.attempt(cl.Attempt.ID); a.State != want {
					t.Fatalf("attempt = %s, want %s", a.State, want)
				}
			})
		}
	}
}

// LOW-2: a payout success from a provider the registry no longer knows is ambiguous, not accepted.
func TestEchoHardening_LOW2_UnknownProviderSuccessIsAmbiguous(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(fmt.Sprintf("declared=%v", declared), func(t *testing.T) {
			w := newB13bW(t, "d2", func(p *b13bProvider) { p.declared = declared })
			wr := w.approved(500, "d2")
			cl := w.mustClaim(wr)
			w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-d2"}, StatusResult{})
			gr := w.dispatch(cl)
			opts := append(w.orch.PayoutOptions(), WithProviderLookup(func(string) (PaymentProvider, bool) { return nil, false }))
			if err := ApplyPayoutResult(w.ctx(), w.pool, w.f.tenantID, wr.ID, cl.Attempt, gr, EvidenceSync, opts...); err != nil {
				t.Fatal(err)
			}
			w.wantNotSettled(wr, cl)
			if a := w.attempt(cl.Attempt.ID); a.State != AttemptAmbiguous {
				t.Fatalf("attempt = %s, want ambiguous", a.State)
			}
		})
	}
}

// LOW-3: a provider varying malformed bytes on every callback cannot create unbounded audit rows; the alert still raises
// every time (one open alert, occurrences keep counting).
func TestEchoHardening_LOW3_TerminalSignalAuditRowsAreCapped(t *testing.T) {
	w := newB13bW(t, "d3", b13bEchoSupported)
	wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-d3"}, "d3")
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-d3", w.goodEcho(cl.Attempt.ID)); err != nil {
		t.Fatal(err)
	}
	const n = terminalSignalAuditMaxDistinctEchoes + 4
	for i := 0; i < n; i++ {
		echo := &payoutinstrument.DestinationEcho{Fingerprint: fmt.Sprintf("not-hex-%d", i), Kid: "bad kid"}
		if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-d3", echo); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.count(`SELECT count(*) FROM audit_log WHERE action='payments.payout_destination_mismatch_terminal' AND target_id=$1`, cl.Attempt.ID.String()); got != terminalSignalAuditMaxDistinctEchoes {
		t.Fatalf("terminal audit rows = %d, want the cap %d", got, terminalSignalAuditMaxDistinctEchoes)
	}
	al, ok := w.alertFor(cl.Attempt.ID, alertReasonPayoutDestinationMismatchOnTerminal)
	if !ok || al.Occurrences < n {
		t.Fatalf("alert missing or occurrences = %d, want >= %d (the alert raises every time)", al.Occurrences, n)
	}
}

// MEDIUM-1: the reconciliation hint for a destination_mismatch park caused by an unexpected echo from an Unsupported adapter
// must not claim the destination differed; a real mismatch keeps its wording.
func TestEchoHardening_MEDIUM1_ReconciliationHintDistinguishesUnexpectedEcho(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fp           func(snap payoutinstrument.Snapshot) string
		wantUnsupExp bool
	}{
		{"unexpected echo (equal to the snapshot, adapter declares Unsupported)", func(s payoutinstrument.Snapshot) string { return s.Fingerprint }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			wr, pend := w.payout(300)
			x := *pend.ProviderReference
			var snap payoutinstrument.Snapshot
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				snap, err = pitest.Shared().LoadSnapshot(ctx, tx, w.f.tenantID, pend.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(w.pool, w.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
				_, e := ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{
					EventType: "payout", ProviderReference: x, MerchantReference: pend.MerchantReference,
					Outcome: OutcomeSucceeded, Amount: wr.Amount, AssetCode: "EUR",
					DestinationEcho: &payoutinstrument.DestinationEcho{Fingerprint: tc.fp(snap), Kid: snap.FingerprintKID}})
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			pending.Flush(context.Background())
			if a := w.attempt(pend.ID); a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != TerminalReasonDestinationMismatch {
				t.Fatalf("setup: %s %v", a.State, a.TerminalReason)
			}
			got := w.b11CU(w.b11Recon(w.source(false, w.payoutLine(x, pend.MerchantReference, "succeeded", wr.Amount))), pend.ID)
			if len(got) != 1 {
				t.Fatalf("want one finding, got %d", len(got))
			}
			text := got[0].ExpectedValue + " | " + got[0].ActualValue
			if strings.Contains(text, "reported to a destination other than the bound one") {
				t.Fatalf("the hint claims a destination mismatch for an unexpected echo: %s", text)
			}
			if !strings.Contains(text, "declares no destination echo") || !strings.Contains(text, "no governed completion route") {
				t.Fatalf("the unexpected-echo hint is missing: %s", text)
			}
		})
	}
}

var _ = reconciliation.MismatchKindPayCapturedUnposted
