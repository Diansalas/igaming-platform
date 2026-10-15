package marketgate

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// SR8R-2: bounded memory, idle-first eviction, per-tenant isolation, reported (not silently lost) counts.

func flood(s *refusalSampler, tenant uuid.UUID, n int) {
	for i := 0; i < n; i++ {
		s.take(refusalKey{tenant: tenant, brand: uuid.New(), op: OpDeposit, reason: ReasonNotPermitted, player: uuid.New()})
	}
}

func TestRefusalSampler_OneTenantFloodIsBoundedByItsOwnCap(t *testing.T) {
	s := newRefusalSampler()
	a := uuid.New()
	flood(s, a, 12000) // beyond both caps: 12k distinct keys cycling through
	if got := len(s.tenants[a].m); got > refusalAuditMaxKeysPerTenant {
		t.Fatalf("tenant table grew to %d (cap %d)", got, refusalAuditMaxKeysPerTenant)
	}
	if s.size() > refusalAuditMaxKeys {
		t.Fatalf("global size %d exceeds %d", s.size(), refusalAuditMaxKeys)
	}
}

func TestRefusalSampler_AnotherTenantsCountsSurviveAFlood(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newRefusalSampler()
	s.now = func() time.Time { return now }
	b := uuid.New()
	kb := refusalKey{tenant: b, brand: uuid.New(), op: OpRegistration, reason: ReasonNotPermitted}
	for i := 0; i < refusalAuditBurst+7; i++ { // 7 suppressed
		s.take(kb)
	}
	flood(s, uuid.New(), 12000)
	flood(s, uuid.New(), 12000)
	now = now.Add(refusalAuditRefill)
	w, sup := s.take(kb)
	if !w || sup != 7 {
		t.Fatalf("tenant B's pending suppressed count was disturbed by others' floods: write=%v suppressed=%d (want 7)", w, sup)
	}
}

func TestRefusalSampler_EvictedSuppressedCountsAreReportedOnTheNextRow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newRefusalSampler()
	s.now = func() time.Time { return now }
	tenant := uuid.New()
	hot := refusalKey{tenant: tenant, brand: uuid.New(), op: OpLaunch, reason: ReasonNotPermitted}
	for i := 0; i < refusalAuditBurst+9; i++ { // 9 suppressed, bucket not idle
		s.take(hot)
	}
	before := EvictedSuppressed.Load()
	reported := 0
	// Fill the tenant's table with fresher, equally non-idle buckets so the hot one is the LRU victim.
	for i := 0; i < refusalAuditMaxKeysPerTenant+5; i++ {
		now = now.Add(time.Millisecond)
		k := refusalKey{tenant: tenant, brand: uuid.New(), op: OpLaunch, reason: ReasonNotPermitted}
		for j := 0; j < refusalAuditBurst+1; j++ {
			_, sup := s.take(k)
			reported += sup
		}
	}
	if _, still := s.tenants[tenant].m[hot]; still {
		t.Fatal("setup: the hot bucket should have been evicted")
	}
	if got := EvictedSuppressed.Load() - before; got < 9 {
		t.Fatalf("eviction of a bucket holding 9 suppressed refusals must be counted, counter moved by %d", got)
	}
	// The evicted tally rides on the next audit row(s) written for this tenant, exactly once in total.
	now = now.Add(refusalAuditRefill)
	for i := 0; i < 5; i++ {
		_, sup := s.take(refusalKey{tenant: tenant, brand: uuid.New(), op: OpLaunch, reason: ReasonNotPermitted})
		reported += sup
	}
	if reported < 9 {
		t.Fatalf("the evicted count must be reported on subsequent rows, reported %d", reported)
	}
	if s.tenants[tenant].evicted != 0 {
		t.Fatal("an evicted tally must be cleared once reported")
	}
}

func TestRefusalSampler_IdleBucketsAreEvictedBeforeBusyOnes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newRefusalSampler()
	s.now = func() time.Time { return now }
	tenant := uuid.New()
	busy := refusalKey{tenant: tenant, brand: uuid.New(), op: OpDeposit, reason: ReasonNotPermitted}
	for i := 0; i < refusalAuditBurst+3; i++ {
		s.take(busy)
	}
	// Many one-shot (idle once refilled) buckets, older than 'busy' in last-use terms are NOT needed:
	// time passes so they refill to full and become idle.
	for i := 0; i < refusalAuditMaxKeysPerTenant-1; i++ {
		s.take(refusalKey{tenant: tenant, brand: uuid.New(), op: OpDeposit, reason: ReasonNotPermitted})
	}
	now = now.Add(time.Hour) // every one-shot bucket is idle again; 'busy' is idle too after the hour, so re-dirty it
	for i := 0; i < refusalAuditBurst+3; i++ {
		s.take(busy)
	}
	before := EvictedSuppressed.Load()
	for i := 0; i < 50; i++ {
		s.take(refusalKey{tenant: tenant, brand: uuid.New(), op: OpDeposit, reason: ReasonNotPermitted})
	}
	if _, ok := s.tenants[tenant].m[busy]; !ok {
		t.Fatal("a bucket holding suppressed counts was evicted while idle buckets were available")
	}
	if EvictedSuppressed.Load() != before {
		t.Fatal("idle evictions must not lose any tally")
	}
}
