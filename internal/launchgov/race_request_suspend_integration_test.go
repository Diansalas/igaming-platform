//go:build integration

package launchgov_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// Security review CR1: a suspension racing a NEW request of the same subject. The request is
// inserted by a concurrent transaction after the suspension read its pending requests; at commit
// the C-3(b) check (LA030) used to fail the suspension. The service now (a) takes the subject row
// FOR SHARE in the request transaction so the two serialise and (b) retries the suspension on LA030.
//
// Invariants for every round: the suspension ALWAYS succeeds (the fail-closed direction is never
// lost to the race); the subject ends suspended; no pending request is stale (a pending request
// whose from_status differs from the subject's status). Two outcomes are legal and BOTH must occur:
//   - the request committed first: it recorded from_status 'active' and was superseded;
//   - the suspension committed first: the request was filed against 'suspended' and stays pending.
//
// raceDelay returns, for round i, how long the delayed side waits and whether the FIRST side is the
// delayed one. The delays run from none (a true race) to well beyond one operation (a strict order),
// and each delay is used in both directions across 50 rounds, so both orderings always occur and the
// near-simultaneous window is exercised as well.
func raceDelay(i int) (d time.Duration, delayFirst bool) {
	pats := []time.Duration{0, time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond,
		16 * time.Millisecond, 32 * time.Millisecond, 48 * time.Millisecond, 0, 3 * time.Millisecond}
	return pats[i%10], (i/2)%2 == 0
}

func TestConcurrency_RequestRacesSuspension_BothOrders_x50(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	supersededRounds, pendingAfterRounds := 0, 0
	for i := 0; i < raceRounds; i++ {
		brand := w.activeBrand(tid)
		sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
		in := launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
		in.AcknowledgedCheckCodes = w.nonPassing(in)

		var wg sync.WaitGroup
		var rres *launchgov.RequestResult
		var rerr, serr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if d, first := raceDelay(i); !first { // the suspension gets the head start
				time.Sleep(d)
			}
			rres, rerr = w.svc.Request(context.Background(), w.ap1, in)
		}()
		go func() {
			defer wg.Done()
			<-start
			if d, first := raceDelay(i); first { // the request gets the head start
				time.Sleep(d)
			}
			_, serr = w.svc.Suspend(context.Background(), w.req, launchgov.SuspendInput{Subject: sub, ReasonCode: "compliance_hold"})
		}()
		close(start)
		wg.Wait()

		if serr != nil {
			t.Fatalf("round %d: the suspension must never be lost to the race: %v", i, serr)
		}
		if got := w.status(sub); got != "suspended" {
			t.Fatalf("round %d: subject status %s", i, got)
		}
		if rerr != nil {
			t.Fatalf("round %d: the request must be filed (close is legal from active and from suspended): %v", i, rerr)
		}
		got, _, err := w.svc.GetRequest(context.Background(), w.ap1, tid, rres.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case got.FromStatus == "active" && got.Status == launchgov.ReqSuperseded:
			supersededRounds++
		case got.FromStatus == "suspended" && got.Status == launchgov.ReqPending:
			pendingAfterRounds++
		default:
			t.Fatalf("round %d: stale or inconsistent request: from=%s status=%s", i, got.FromStatus, got.Status)
		}
	}
	t.Logf("races: request first (superseded) %d, suspension first (pending against suspended) %d", supersededRounds, pendingAfterRounds)
	if supersededRounds == 0 || pendingAfterRounds == 0 {
		t.Fatalf("both orderings must be exercised: request-first %d, suspension-first %d", supersededRounds, pendingAfterRounds)
	}
}
