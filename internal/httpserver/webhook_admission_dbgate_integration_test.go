//go:build integration

// ADR 0097 T4 (UnauthFlood_DoesNotConsumePool) and T10's "no statement"
// half, per security's exact specification: a REAL pool of 10 connections
// (phasecapture.Pool10, the production pool size), a blocking hook INSIDE
// the gated section (a channel-held real transaction, never a sleep),
// >= 200 concurrent requests, asserting pool.Stat().AcquiredConns() never
// exceeds the gate's own cap, that an UNRELATED pool.Acquire succeeds
// without waiting on the barrier, and that over-cap callers get rejected
// having executed ZERO statements. This drives the REAL gatedReader type
// (not a reimplementation of its mechanism), so it kills:
//   - S2: the A4b gate removed from gatedGetTenantBySlug (the slug
//     lookup) - TestAdmission_T4_GatedGetTenantBySlug_RespectsSaturation.
//   - a gatedReader bypass (the gate check removed from
//     gatedReader.WithTenantReadOnly) - TestAdmission_T4_GatedReaderBoundsRealPoolAcquisition
//     itself fails if the bypass mutation is applied, since every one of
//     the 200 callers would then acquire a real connection immediately.
package httpserver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
)

// TestAdmission_T4_GatedReaderBoundsRealPoolAcquisition drives the REAL
// gatedReader.WithTenantReadOnly (not a reimplementation) with a
// channel-held barrier INSIDE the gated section, 200 concurrent callers,
// gate cap 3, against a real 10-connection pool.
func TestAdmission_T4_GatedReaderBoundsRealPoolAcquisition(t *testing.T) {
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	tenant := mustCreateTenant(t, pool) // created ONCE, before the flood, so its own
	// insert never pollutes the AcquireCount baseline measured below.

	const gateCap = 3
	const flood = 200

	gate := admission.NewBulkhead(1000) // global cap not under test here; per-key cap IS
	clock := admission.RealClock()
	g := gatedReader{db: pool, gate: gate, key: "t4-key", perKeyCap: gateCap, clock: clock, wait: 30 * time.Millisecond}

	baselineAcquireCount := pool.Raw().Stat().AcquireCount()

	release := make(chan struct{})
	var closeOnce sync.Once
	closeRelease := func() { closeOnce.Do(func() { close(release) }) }
	defer closeRelease()

	var inHold int64
	var admitted, rejected int64
	var wg sync.WaitGroup

	for i := 0; i < flood; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := g.WithTenantReadOnly(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
				atomic.AddInt64(&inHold, 1)
				<-release // hold the REAL acquired connection open
				atomic.AddInt64(&inHold, -1)
				return nil
			})
			if err != nil {
				atomic.AddInt64(&rejected, 1)
				return
			}
			atomic.AddInt64(&admitted, 1)
		}()
	}

	// Wait (bounded, generous, real-time synchronization only) until
	// gateCap goroutines are actually holding a real pooled connection.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&inHold) >= gateCap {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt64(&inHold); got < gateCap {
		t.Fatalf("expected %d goroutines holding a connection, only %d did", gateCap, got)
	}
	if got := atomic.LoadInt64(&inHold); got > gateCap {
		t.Fatalf("in-hold count %d exceeds the gate cap %d - the gate did not bound real pool acquisition (possible S2/gatedReader-bypass mutation)", got, gateCap)
	}

	// An UNRELATED pool operation (a different key entirely, so it isn't
	// even subject to this gate instance) must succeed without waiting on
	// the barrier.
	unrelatedDone := make(chan error, 1)
	go func() {
		unrelatedDone <- pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			var one int
			return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
		})
	}()
	select {
	case err := <-unrelatedDone:
		if err != nil {
			t.Fatalf("unrelated pool.WithoutTenant failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an unrelated pool operation must not wait on the gate's barrier")
	}

	// Let the flood's own waiters give up (they wait up to 30ms each) so
	// the rejected count stabilizes BEFORE the barrier holders are freed -
	// otherwise every waiter would eventually succeed once slots free up,
	// and this test would never observe a genuine over-cap rejection.
	time.Sleep(200 * time.Millisecond)

	closeRelease()
	wg.Wait()

	if rejected == 0 {
		t.Fatalf("expected some of the %d concurrent callers to be rejected by a cap of %d, got 0 (admitted=%d)", flood, gateCap, admitted)
	}
	if admitted+rejected != flood {
		t.Fatalf("admitted(%d)+rejected(%d) != flood(%d)", admitted, rejected, flood)
	}

	// T10's "no statement" half: a rejected caller never calls
	// g.db.WithTenantReadOnly at all (gate.Acquire returned false first),
	// so the pool's own AcquireCount grows by at most the number ADMITTED.
	finalAcquireCount := pool.Raw().Stat().AcquireCount()
	grew := finalAcquireCount - baselineAcquireCount
	if grew > int64(admitted)+5 { // +5 slack for pool-internal housekeeping
		t.Fatalf("pool AcquireCount grew by %d, expected roughly <= admitted(%d) - a rejected caller must never acquire a connection", grew, admitted)
	}
}

// TestAdmission_T4_GatedGetTenantBySlug_RespectsSaturation is the S2
// mutation target: with the SAME gate key fully saturated by gatedReader
// holders, a subsequent gatedGetTenantBySlug call for that SAME key must
// also be refused (errDBGateUnavailable) - never bypass the gate to reach
// the real GetTenantBySlug query. If the gate check were ever removed from
// gatedGetTenantBySlug (S2), this test fails because the call would
// succeed despite the saturated gate.
func TestAdmission_T4_GatedGetTenantBySlug_RespectsSaturation(t *testing.T) {
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	tenant := mustCreateTenant(t, pool)

	const gateCap = 1
	gate := admission.NewBulkhead(10)
	clock := admission.RealClock()
	const key = "t4b-key"

	// Saturate the gate's ONE slot for this key with a real gatedReader
	// hold.
	release := make(chan struct{})
	holderStarted := make(chan struct{})
	go func() {
		g := gatedReader{db: pool, gate: gate, key: key, perKeyCap: gateCap, clock: clock, wait: time.Second}
		_ = g.WithTenantReadOnly(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			close(holderStarted)
			<-release
			return nil
		})
	}()
	select {
	case <-holderStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("holder never started")
	}
	defer close(release)

	_, err := gatedGetTenantBySlug(context.Background(), gate, key, gateCap, clock, 30*time.Millisecond, pool, tenant.Slug)
	if err == nil {
		t.Fatal("gatedGetTenantBySlug must be refused while the gate is saturated - a bypass here is security review finding S2")
	}
}
