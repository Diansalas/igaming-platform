//go:build integration

package payments

// Destination-echo conformance suite (ADR 0111 24, owner decision 5 of ADR 0095 48).
//
// runDestinationEchoConformance is the REUSABLE contract any payout adapter (MOCK today, every real adapter later) must
// pass. It does not assume a state: it reads the adapter's own DestinationEchoSemantics declaration and runs the matching
// cell group, so an adapter that declares Supported cannot pass by behaving like an Unsupported one or the reverse.
// A new adapter's test supplies a constructor returning a harness whose subject is the adapter under test (scripted
// through its vendor stub); the cells themselves live here. Everything in this file runs against MOCK providers: it is a
// statement about the platform's evidence handling, never about a real PSP.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// echoSubject builds a fresh harness (own scratch tenant) around the adapter under test.
type echoSubject func(t *testing.T, id string) *b13bW

func mockEchoSubject(sem payoutinstrument.DestinationEchoSemantics) echoSubject {
	return func(t *testing.T, id string) *b13bW {
		return newB13bW(t, id, func(p *b13bProvider) { p.declared = sem == payoutinstrument.DestinationEchoSupported })
	}
}

// malformedEchoes are the malformed shapes (bad kid grammar, wrong length, wrong charset). Each is mismatch-class.
func malformedEchoes(snap payoutinstrument.Snapshot) map[string]*payoutinstrument.DestinationEcho {
	good := snap.Fingerprint
	return map[string]*payoutinstrument.DestinationEcho{
		"kid with a space":                {Fingerprint: good, Kid: "bad kid"},
		"kid with markup":                 {Fingerprint: good, Kid: "<script>"},
		"empty kid":                       {Fingerprint: good, Kid: ""},
		"kid too long":                    {Fingerprint: good, Kid: strings.Repeat("k", 33)},
		"fingerprint too short":           {Fingerprint: good[:63], Kid: snap.FingerprintKID},
		"fingerprint too long":            {Fingerprint: good + "0", Kid: snap.FingerprintKID},
		"fingerprint upper-case charset":  {Fingerprint: strings.ToUpper(good), Kid: snap.FingerprintKID},
		"fingerprint non-hex charset":     {Fingerprint: strings.Repeat("z", 64), Kid: snap.FingerprintKID},
		"empty fingerprint":               {Fingerprint: "", Kid: snap.FingerprintKID},
		"fingerprint with embedded space": {Fingerprint: good[:10] + " " + good[11:], Kid: snap.FingerprintKID},
	}
}

func runDestinationEchoConformance(t *testing.T, subject echoSubject) {
	t.Helper()
	probe := subject(t, "cf0")
	sem := probe.prov.Capabilities().Manifest.DestinationEchoSemantics
	if !sem.Valid() {
		t.Fatalf("the adapter under test declares %s: an unset destination-echo declaration is invalid", sem)
	}
	if err := validateManifest(probe.prov, probe.prov.Capabilities()); err != nil {
		t.Fatalf("registration refused the adapter: %v", err)
	}
	if sem == payoutinstrument.DestinationEchoSupported {
		runSupportedCells(t, subject)
	} else {
		runUnsupportedCells(t, subject)
	}
	runCommonCells(t, subject)
}

// ---- cells common to both declarations -----------------------------------------------------------------

func runCommonCells(t *testing.T, subject echoSubject) {
	// A callback can never set or change a destination: the attempt snapshot, the instrument binding on the withdrawal
	// and the snapshot/instrument row counts are byte-identical before and after any callback, whatever it carries.
	t.Run("callback cannot set the destination", func(t *testing.T) {
		w := subject(t, "cf-cb")
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-cf-cb"}, "cf-cb")
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatal(err)
		}
		snapBefore := w.snapshot(cl.Attempt.ID)
		bindBefore := w.wr(wr.ID)
		snapsN := w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots`)
		instrN := w.count(`SELECT count(*) FROM payout_instruments`)
		foreign := &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("ef", 32), Kid: snapBefore.FingerprintKID}
		if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-cf-cb", foreign); err != nil {
			t.Fatalf("callback: %v", err)
		}
		if got := w.snapshot(cl.Attempt.ID); !reflect.DeepEqual(got, snapBefore) {
			t.Fatal("a provider callback changed the attempt destination snapshot")
		}
		after := w.wr(wr.ID)
		if !reflect.DeepEqual(after.PayoutInstrumentID, bindBefore.PayoutInstrumentID) ||
			!reflect.DeepEqual(after.PayoutInstrumentFingerprint, bindBefore.PayoutInstrumentFingerprint) {
			t.Fatal("a provider callback changed the withdrawal's destination binding")
		}
		if w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots`) != snapsN || w.count(`SELECT count(*) FROM payout_instruments`) != instrN {
			t.Fatal("a provider callback wrote a destination row")
		}
		w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
	})

	// A mismatch-class echo can never progress the payout, on every channel, for every outcome that could settle it.
	t.Run("a mismatch cannot progress the payout", func(t *testing.T) {
		for _, ch := range []string{"sync", "poll", "callback"} {
			t.Run(ch, func(t *testing.T) {
				w := subject(t, "cf-np-"+ch)
				bad := &payoutinstrument.DestinationEcho{Fingerprint: strings.Repeat("ab", 32), Kid: "no-such-kid"}
				wr, cl := driveEcho(t, w, ch, "cf-np-"+ch, bad)
				ledger := w.ledgerTx()
				a := w.attempt(cl.Attempt.ID)
				if a.State != AttemptDisputed {
					t.Fatalf("attempt = %s, want disputed", a.State)
				}
				if got := w.wr(wr.ID); got.State == withdrawal.StateCompleted || got.ReleaseLedgerTransactionID != nil {
					t.Fatalf("withdrawal = %s release=%v: a mismatch progressed the payout", got.State, got.ReleaseLedgerTransactionID)
				}
				if w.ledgerTx() != ledger {
					t.Fatal("ledger moved")
				}
				w.balanced()
			})
		}
	})
}

// driveEcho runs one withdrawal (identified by key) to the point where the provider reports success carrying echo, via
// the given channel. The echo is scripted verbatim: the harness never corrects it.
func driveEcho(t *testing.T, w *b13bW, channel, key string, echo *payoutinstrument.DestinationEcho) (withdrawal.WithdrawalRequest, ClaimResult) {
	t.Helper()
	ref := "ref-" + key
	var wrOut withdrawal.WithdrawalRequest
	var clOut ClaimResult
	switch channel {
	case "sync":
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: ref, DestinationEcho: echo}, key)
		wrOut, clOut = wr, cl
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatalf("apply: %v", err)
		}
	case "poll":
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, key)
		wrOut, clOut = wr, cl
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatal(err)
		}
		w.prov.set(WithdrawResult{}, StatusResult{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", DestinationEcho: echo})
		if err := PollPayoutStatus(w.ctx(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, w.attempt(cl.Attempt.ID), time.Now().Add(time.Minute), nil); err != nil {
			t.Fatalf("poll: %v", err)
		}
	case "callback":
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, key)
		wrOut, clOut = wr, cl
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatal(err)
		}
		if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, ref, echo); err != nil {
			t.Fatalf("callback: %v", err)
		}
	default:
		t.Fatalf("unknown channel %q", channel)
	}
	return wrOut, clOut
}

// ---- Supported ----------------------------------------------------------------------------------------

func runSupportedCells(t *testing.T, subject echoSubject) {
	t.Run("supported echo settles on every channel", func(t *testing.T) {
		for _, ch := range []string{"sync", "poll", "callback"} {
			t.Run(ch, func(t *testing.T) {
				w := subject(t, "cf-ok-"+ch)
				// The good echo needs the snapshot, which T1p writes at claim time: claim first.
				key := "cf-ok-" + ch
				wr := w.approved(500, key)
				cl := w.mustClaim(wr)
				good := w.goodEcho(cl.Attempt.ID)
				ref := "ref-" + key
				switch ch {
				case "sync":
					w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: ref, DestinationEcho: good}, StatusResult{})
					if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
						t.Fatal(err)
					}
				case "poll":
					w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, StatusResult{})
					if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
						t.Fatal(err)
					}
					w.prov.set(WithdrawResult{}, StatusResult{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", DestinationEcho: good})
					if err := PollPayoutStatus(w.ctx(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, w.attempt(cl.Attempt.ID), time.Now().Add(time.Minute), nil); err != nil {
						t.Fatal(err)
					}
				case "callback":
					w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, StatusResult{})
					if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
						t.Fatal(err)
					}
					if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, ref, good); err != nil {
						t.Fatal(err)
					}
				}
				if a := w.attempt(cl.Attempt.ID); a.State != AttemptSucceeded {
					t.Fatalf("attempt = %s, want succeeded", a.State)
				}
				if got := w.wr(wr.ID); got.State != withdrawal.StateCompleted {
					t.Fatalf("withdrawal = %s, want completed", got.State)
				}
				w.balanced()
			})
		}
	})

	t.Run("a success without the echo is ambiguous, never a success", func(t *testing.T) {
		w := subject(t, "cf-abs")
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-cf-abs"}, "cf-abs")
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatal(err)
		}
		if a := w.attempt(cl.Attempt.ID); a.State != AttemptAmbiguous {
			t.Fatalf("attempt = %s, want ambiguous", a.State)
		}
		if got := w.wr(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
			t.Fatal("no completion without the declared echo")
		}
		w.balanced()
	})

	t.Run("mismatched and unknown-kid echo park destination_mismatch", func(t *testing.T) {
		for _, kind := range []string{"different fingerprint", "unknown kid"} {
			t.Run(kind, func(t *testing.T) {
				w := subject(t, "cf-mm")
				wr := w.approved(500, "cf-mm")
				cl := w.mustClaim(wr)
				echo := w.badEcho(cl.Attempt.ID)
				if kind == "unknown kid" {
					echo = &payoutinstrument.DestinationEcho{Fingerprint: w.snapshot(cl.Attempt.ID).Fingerprint, Kid: "no-such-kid"}
				}
				w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-cf-mm", DestinationEcho: echo}, StatusResult{})
				if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
					t.Fatal(err)
				}
				w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
			})
		}
	})

	t.Run("malformed echo fails closed and its text is not stored", func(t *testing.T) {
		w0 := subject(t, "cf-mal0")
		wr0 := w0.approved(500, "cf-mal0")
		cl0 := w0.mustClaim(wr0)
		names := malformedEchoes(w0.snapshot(cl0.Attempt.ID))
		i := 0
		for name := range names {
			i++
			t.Run(name, func(t *testing.T) {
				w := subject(t, "cf-mal")
				wr := w.approved(500, "cf-mal")
				cl := w.mustClaim(wr)
				echo := malformedEchoes(w.snapshot(cl.Attempt.ID))[name]
				w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-cf-mal", DestinationEcho: echo}, StatusResult{})
				if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
					t.Fatal(err)
				}
				w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
				w.wantNoRawEchoInAudit(cl.Attempt.ID, echo)
			})
		}
	})

	t.Run("callback replay is idempotent: one audit row, one alert occurrence", func(t *testing.T) {
		w := subject(t, "cf-rp")
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomePending, ProviderReference: "ref-cf-rp"}, "cf-rp")
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatal(err)
		}
		bad := w.badEcho(cl.Attempt.ID)
		for i := 0; i < 3; i++ {
			if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-cf-rp", bad); err != nil {
				t.Fatalf("delivery %d: %v", i, err)
			}
		}
		w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
		if al, _ := w.alertFor(cl.Attempt.ID, TerminalReasonDestinationMismatch); al.Occurrences != 1 {
			t.Fatalf("alert occurrences = %d, want 1", al.Occurrences)
		}
	})
}

// ---- Unsupported --------------------------------------------------------------------------------------

func runUnsupportedCells(t *testing.T, subject echoSubject) {
	t.Run("no echo is expected: a success settles on ordinary evidence and is not ambiguous", func(t *testing.T) {
		w := subject(t, "cf-u-abs")
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-cf-u-abs"}, "cf-u-abs")
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatal(err)
		}
		if a := w.attempt(cl.Attempt.ID); a.State != AttemptSucceeded {
			t.Fatalf("attempt = %s, want succeeded", a.State)
		}
		if n := w.count(`SELECT count(*) FROM audit_log WHERE action='payments.payout_parked_destination'`); n != 0 {
			t.Fatalf("park audit rows = %d: an absent echo from an Unsupported adapter is not an anomaly", n)
		}
		w.balanced()
	})

	// An echo from an Unsupported adapter is untrusted evidence: it is NEVER a match (not even when it equals the snapshot,
	// since the adapter declared it cannot compute one), and it is not silently ignored. It fails closed as a mismatch-class
	// anomaly (ADR 0111 24).
	t.Run("an echo from an unsupported adapter is untrusted: matching, mismatching and malformed all park", func(t *testing.T) {
		for _, kind := range []string{"equal to the snapshot", "different fingerprint", "malformed"} {
			for _, ch := range []string{"sync", "poll", "callback"} {
				t.Run(kind+"/"+ch, func(t *testing.T) {
					w := subject(t, "cf-u-echo")
					key := "cf-u-echo"
					wr := w.approved(500, key)
					cl := w.mustClaim(wr)
					snap := w.snapshot(cl.Attempt.ID)
					echo := &payoutinstrument.DestinationEcho{Fingerprint: snap.Fingerprint, Kid: snap.FingerprintKID}
					switch kind {
					case "different fingerprint":
						echo = w.badEcho(cl.Attempt.ID)
					case "malformed":
						echo = &payoutinstrument.DestinationEcho{Fingerprint: "NOT-HEX", Kid: "bad kid"}
					}
					ref := "ref-" + key
					switch ch {
					case "sync":
						w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: ref, DestinationEcho: echo}, StatusResult{})
						if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
							t.Fatal(err)
						}
					case "poll":
						w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, StatusResult{})
						if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
							t.Fatal(err)
						}
						w.prov.set(WithdrawResult{}, StatusResult{Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR", DestinationEcho: echo})
						if err := PollPayoutStatus(w.ctx(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, w.attempt(cl.Attempt.ID), time.Now().Add(time.Minute), nil); err != nil {
							t.Fatal(err)
						}
					case "callback":
						w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, StatusResult{})
						if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
							t.Fatal(err)
						}
						for i := 0; i < 2; i++ { // replay
							if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, ref, echo); err != nil {
								t.Fatal(err)
							}
						}
						if al, _ := w.alertFor(cl.Attempt.ID, TerminalReasonDestinationMismatch); al.Occurrences != 1 {
							t.Fatalf("replay alert occurrences = %d, want 1", al.Occurrences)
						}
					}
					w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
					w.wantNoRawEchoInAudit(cl.Attempt.ID, echo)
				})
			}
		}
	})

	t.Run("an echo on an already-succeeded unsupported payout is signal-only", func(t *testing.T) {
		w := subject(t, "cf-u-term")
		wr, cl, gr := w.submittedWithResult(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "ref-cf-u-term"}, "cf-u-term")
		if err := w.apply(wr, cl, gr); err != nil {
			t.Fatal(err)
		}
		ledger := w.ledgerTx()
		snap := w.snapshot(cl.Attempt.ID)
		eq := &payoutinstrument.DestinationEcho{Fingerprint: snap.Fingerprint, Kid: snap.FingerprintKID}
		for i := 0; i < 2; i++ {
			if err := w.callback(w.attempt(cl.Attempt.ID), OutcomeSucceeded, "ref-cf-u-term", eq); err != nil {
				t.Fatal(err)
			}
		}
		a := w.attempt(cl.Attempt.ID)
		if a.State != AttemptSucceeded || w.wr(wr.ID).State != withdrawal.StateCompleted || w.ledgerTx() != ledger {
			t.Fatal("a terminal payout must not change")
		}
		if n := w.count(`SELECT count(*) FROM audit_log WHERE action='payments.payout_destination_mismatch_terminal' AND target_id=$1`, a.ID.String()); n != 1 {
			t.Fatalf("terminal audit rows = %d, want 1", n)
		}
		if _, ok := w.alertFor(a.ID, alertReasonPayoutDestinationMismatchOnTerminal); !ok {
			t.Fatal("terminal signal missing")
		}
		w.balanced()
	})
}

// wantNoRawEchoInAudit asserts no audit row for the attempt carries provider-controlled echo text (kid or fingerprint):
// a malformed echo must never be copied into the audit trail, and a well-formed one only as an 8-character prefix.
func (w *b13bW) wantNoRawEchoInAudit(attemptID uuid.UUID, echo *payoutinstrument.DestinationEcho) {
	w.t.Helper()
	if echo == nil {
		return
	}
	for _, needle := range []string{echo.Kid, echo.Fingerprint} {
		if len(needle) < 9 {
			continue // short values cannot be told apart from the closed vocabulary and prefixes
		}
		if n := w.count(`SELECT count(*) FROM audit_log WHERE target_id=$1 AND metadata::text LIKE '%' || $2 || '%'`, attemptID.String(), needle); n != 0 {
			w.t.Fatalf("audit rows carry the raw echo text %q (%d)", needle, n)
		}
	}
}

// ---- the suite, run on the MOCK adapter under both declarations -----------------------------------------

func TestEchoConformance_MockSupported(t *testing.T) {
	runDestinationEchoConformance(t, mockEchoSubject(payoutinstrument.DestinationEchoSupported))
}

func TestEchoConformance_MockUnsupported(t *testing.T) {
	runDestinationEchoConformance(t, mockEchoSubject(payoutinstrument.DestinationEchoUnsupported))
}
