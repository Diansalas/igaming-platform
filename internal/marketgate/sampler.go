package marketgate

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Refusal-audit sampling (SR8-5, hardened by SR8R-2). Every refusal is returned
// to the caller, but the AUDIT ROW is written through a per-key token bucket so
// an anonymous or excluded-market caller cannot flood the append-only audit_log:
// the key is (tenant, brand, operation, reason, player) - the player is uuid.Nil
// at registration, so every anonymous registration refusal of a brand shares one
// bucket and no IP or other identifier is ever hashed or stored. A bucket allows
// refusalAuditBurst rows, then one more per refusalAuditRefill; the next written
// row records how many were suppressed since the previous one
// (suppressed_since_last_row).
//
// Every refusal is ALSO logged by the calling handler (a structured Warn per
// refusal), so sampling the audit row never hides a refusal from the logs.
//
// Memory is bounded, and one tenant cannot erase another's counts (SR8R-2):
//
//   - buckets live in a per-TENANT table capped at refusalAuditMaxKeysPerTenant;
//     when a tenant's table is full, that tenant's own idle buckets are dropped
//     first (a bucket with a full token count and nothing suppressed carries no
//     information), then its least recently used one - never another tenant's;
//   - a global cap (refusalAuditMaxKeys) is enforced the same way: idle buckets
//     anywhere first, then the least recently used bucket of the tenant holding
//     the most buckets (the noisy one);
//   - a bucket evicted while it still holds suppressed refusals is NOT lost
//     silently: its count is added to the tenant's pending evicted total, which
//     the next audit row written for that tenant carries in its OWN field,
//     suppressed_evicted (tenant-wide, never mixed into another key's
//     suppressed_since_last_row, which stays that key's own tally), and a
//     structured log line plus the EvictedSuppressed counter record the
//     eviction at once. The tally is cleared when handed out, before the row
//     commits, so a failed insert loses it (the counter keeps the evicted part);
//     acceptable for sampling.
//
// The state is process-local (N instances = N x the caps; still bounded).
const (
	refusalAuditBurst            = 5
	refusalAuditRefill           = time.Minute
	refusalAuditMaxKeysPerTenant = 500
	refusalAuditMaxKeys          = 10000
)

// EvictedSuppressed counts suppressed-refusal tallies that were evicted from the
// sampler before an audit row could carry them (a cheap metric: expose it on
// the metrics endpoint when one exists).
var EvictedSuppressed atomic.Int64

type refusalKey struct {
	tenant, brand, player uuid.UUID
	op                    Operation
	reason                string
}

type refusalBucket struct {
	tokens     float64
	last       time.Time
	suppressed int
}

type tenantBuckets struct {
	m       map[refusalKey]*refusalBucket
	evicted int // suppressed refusals evicted and not yet reported on a written row
	// evictedSinceLog / lastEvictLog rate-limit the eviction log line to one per
	// minute per tenant (cumulative), so an eviction storm cannot flood the logs.
	evictedSinceLog int
	lastEvictLog    time.Time
}

type refusalSampler struct {
	mu      sync.Mutex
	tenants map[uuid.UUID]*tenantBuckets
	total   int
	now     func() time.Time
}

func newRefusalSampler() *refusalSampler {
	return &refusalSampler{tenants: map[uuid.UUID]*tenantBuckets{}, now: time.Now}
}

func (b *refusalBucket) idle(now time.Time) bool {
	return b.suppressed == 0 && b.tokens+float64(now.Sub(b.last))/float64(refusalAuditRefill) >= refusalAuditBurst
}

// evictLocked removes one bucket from tb, preferring an idle one, else the least
// recently used; it reports any suppressed tally it had to drop.
func (s *refusalSampler) evictLocked(tenant uuid.UUID, tb *tenantBuckets, now time.Time) {
	var victim refusalKey
	found, victimIdle := false, false
	for k, b := range tb.m {
		idle := b.idle(now)
		switch {
		case !found, idle && !victimIdle, idle == victimIdle && b.last.Before(tb.m[victim].last):
			victim, found, victimIdle = k, true, idle
		}
	}
	if !found {
		return
	}
	if n := tb.m[victim].suppressed; n > 0 {
		tb.evicted += n
		tb.evictedSinceLog += n
		EvictedSuppressed.Add(int64(n))
		if now.Sub(tb.lastEvictLog) >= time.Minute {
			slog.Warn("market_refusal_audit_buckets_evicted_with_suppressed_counts",
				"tenant_id", tenant.String(), "suppressed_since_last_log", tb.evictedSinceLog)
			tb.evictedSinceLog, tb.lastEvictLog = 0, now
		}
	}
	delete(tb.m, victim)
	s.total--
}

// take is takeDetailed without the tenant-wide evicted tally (tests of the per-key tally).
func (s *refusalSampler) take(k refusalKey) (write bool, suppressed int) {
	write, suppressed, _ = s.takeDetailed(k)
	return write, suppressed
}

// takeDetailed reports whether an audit row may be written for k now and, if so,
// how many refusals of THIS key were suppressed since its last written row, and
// separately how many refusals of the tenant's evicted buckets are still unreported.
func (s *refusalSampler) takeDetailed(k refusalKey) (write bool, suppressed, evicted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	tb, ok := s.tenants[k.tenant]
	if !ok {
		tb = &tenantBuckets{m: map[refusalKey]*refusalBucket{}}
		s.tenants[k.tenant] = tb
	}
	b, ok := tb.m[k]
	if !ok {
		for len(tb.m) >= refusalAuditMaxKeysPerTenant {
			s.evictLocked(k.tenant, tb, now)
		}
		for s.total >= refusalAuditMaxKeys {
			// Global pressure: drop from the tenant holding the most buckets (never
			// preferring the arriving tenant unless it is the largest).
			var big uuid.UUID
			var bigTB *tenantBuckets
			for id, t := range s.tenants {
				if bigTB == nil || len(t.m) > len(bigTB.m) {
					big, bigTB = id, t
				}
			}
			if bigTB == nil || len(bigTB.m) == 0 {
				break
			}
			s.evictLocked(big, bigTB, now)
		}
		b = &refusalBucket{tokens: refusalAuditBurst, last: now}
		tb.m[k] = b
		s.total++
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += float64(elapsed) / float64(refusalAuditRefill)
		if b.tokens > refusalAuditBurst {
			b.tokens = refusalAuditBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		b.suppressed++
		return false, 0, 0
	}
	b.tokens--
	suppressed, evicted = b.suppressed, tb.evicted
	b.suppressed, tb.evicted = 0, 0
	return true, suppressed, evicted
}

// size is the number of live buckets (tests).
func (s *refusalSampler) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// refusalAudit is the process-wide sampler used by refuse.
var refusalAudit = newRefusalSampler()
