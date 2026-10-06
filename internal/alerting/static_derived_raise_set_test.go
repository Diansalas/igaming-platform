package alerting

import (
	"sort"
	"strings"
	"testing"
)

// Code review CR2-S4: the hand-maintained staticEvidenceFuncs list could miss a
// new function that reaches RaiseGuarded. This guard DERIVES the set instead: R
// is every function that reaches alerting.RaiseGuarded through a chain of calls
// that is NOT inside an alerting.InTx closure (a call inside an InTx closure
// belongs to a transaction owner, which is the sanctioned stop). The set is
// computed over internal/payments (the package whose evidence functions raise
// in-transaction) by callee base name (conservative: a name
// collision only adds a member). It must equal the reviewed set below exactly,
// so a NEW function that raises (directly or transitively) outside an InTx owner
// fails here and has to be reviewed, and a vanished one is noticed too.

func staticBase(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// staticDerivedRaiseSet returns the set of "file:fn" that reach RaiseGuarded
// outside an InTx closure.
func staticDerivedRaiseSet(calls []staticCall) map[string]bool {
	byFn := map[string][]staticCall{} // file:fn -> calls
	fnsByName := map[string][]string{}
	for _, c := range calls {
		k := c.file + ":" + c.fn
		if _, ok := byFn[k]; !ok {
			fnsByName[c.fn] = append(fnsByName[c.fn], k)
		}
		byFn[k] = append(byFn[k], c)
	}
	inR := map[string]bool{}
	rNames := map[string]bool{}
	for k, cs := range byFn {
		for _, c := range cs {
			if c.name == "alerting.RaiseGuarded" && !c.inTx {
				inR[k] = true
				rNames[staticBase(strings.SplitN(k, ":", 2)[1])] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for k, cs := range byFn {
			if inR[k] {
				continue
			}
			for _, c := range cs {
				if !c.inTx && rNames[staticBase(c.name)] {
					inR[k] = true
					rNames[staticBase(strings.SplitN(k, ":", 2)[1])] = true
					changed = true
					break
				}
			}
		}
	}
	return inR
}

// Reviewed: each of these is only reached from a transaction owner that opens
// through alerting.InTx (the owners themselves are pinned by
// TestStaticWiring_EvidenceTransactionOwnersOpenThroughInTxAndFlush).
var staticDerivedRaiseSetReviewed = map[string]bool{
	"internal/payments/alerts.go:alertAfterDispute":                     true,
	"internal/payments/alerts.go:raiseDepositEscalationAlert":           true,
	"internal/payments/alerts.go:raiseDepositParkAlert":                 true,
	"internal/payments/alerts.go:raiseMultipleSuccessAlert":             true,
	"internal/payments/alerts.go:raisePollContradictionAlert":           true,
	"internal/payments/drive.go:applyDepositCallResult":                 true,
	"internal/payments/drive.go:parkDepositAttempt":                     true,
	"internal/payments/orchestrator.go:ReceiveVerifiedCallback":         true,
	"internal/payments/orchestrator.go:auditMultipleSuccessForIntent":   true,
	"internal/payments/orchestrator.go:postDepositSuccessOrDispute":     true,
	"internal/payments/orchestrator.go:receiveCallbackViaReceiptPath":   true,
	"internal/payments/poll_evidence.go:auditPollTerminalContradiction": true,
	"internal/payments/poll_evidence.go:checkPollSuccessEvidence":       true,
	"internal/payments/receipt.go:ApplyDeferredReceiptsForAttempt":      true,
	"internal/payments/receipt.go:ApplyReceiptEvidence":                 true,
	"internal/payments/receipt.go:applyDepositSuccessAndPost":           true,
	"internal/payments/receipt.go:applyResolvedReceiptEvidence":         true,
	"internal/payments/sweeper.go:applyStatusEvidence":                  true,
	"internal/payments/sweeper.go:applyPollPending":                     true,
	"internal/payments/sweeper.go:escalateDepositIfDue":                 true,
}

func TestStaticWiring_DerivedRaiseReachingSetIsReviewed_CR2S4(t *testing.T) {
	var payCalls []staticCall
	for _, c := range staticCollectCalls(t) {
		if strings.HasPrefix(c.file, "internal/payments/") { // the package whose evidence functions raise in-tx
			payCalls = append(payCalls, c)
		}
	}
	got := staticDerivedRaiseSet(payCalls)
	var extra, missing []string
	for k := range got {
		if !staticDerivedRaiseSetReviewed[k] {
			extra = append(extra, k)
		}
	}
	for k := range staticDerivedRaiseSetReviewed {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) != 0 || len(missing) != 0 {
		t.Fatalf("the set of functions that reach RaiseGuarded outside an InTx closure changed.\nnew (review that each is reached only from an InTx owner, then add it): %s\nvanished (remove it): %s",
			strings.Join(extra, "\n  "), strings.Join(missing, "\n  "))
	}
	if len(got) < 8 { // seen-count: the derivation must really find the known chain
		t.Fatalf("derived set suspiciously small (%d): the derivation is not seeing the payments raise chain", len(got))
	}
}

func TestStaticWiring_DerivedRaiseSet_NegativeControl(t *testing.T) {
	src := `package p
func helper(ctx, tx any) error { return alerting.RaiseGuarded(ctx, tx, a) }
func middle(ctx, tx any) error { return helper(ctx, tx) }
func owner(ctx any) { alerting.InTx(ctx, r, func(ctx, tx any) error { return middle(ctx, tx) }) }
func rogue(ctx any) { pool.WithTenant(ctx, id, func(ctx, tx any) error { return middle(ctx, tx) }) }
func unrelated() { other() }
`
	got := staticDerivedRaiseSet(staticCollectFromSource(t, "internal/x/fix.go", src))
	for _, want := range []string{"internal/x/fix.go:helper", "internal/x/fix.go:middle", "internal/x/fix.go:rogue"} {
		if !got[want] {
			t.Errorf("derivation missed %s: %v", want, got)
		}
	}
	for _, not := range []string{"internal/x/fix.go:owner", "internal/x/fix.go:unrelated"} {
		if got[not] {
			t.Errorf("derivation wrongly included %s (an InTx owner / an unrelated function): %v", not, got)
		}
	}
}
