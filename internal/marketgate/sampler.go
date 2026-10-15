package marketgate

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Refusal-audit sampling (SR8-5). Every refusal is returned to the caller, but
// the AUDIT ROW is written through a per-key token bucket so an anonymous or
// excluded-market caller cannot flood the append-only audit_log: the key is
// (tenant, brand, operation, reason, player) - the player is uuid.Nil at
// registration, so every anonymous registration refusal of a brand shares one
// bucket and no IP or other identifier is ever hashed or stored. A bucket
// allows refusalAuditBurst rows, then one more per refusalAuditRefill; the next
// written row records how many were suppressed since the previous one
// (suppressed_since_last_row), so volume stays reconstructible.
//
// The bucket map is process-local (a few KB; reset when it reaches
// refusalAuditMaxKeys, which only ever lets rows through again). With N
// instances the cap is N times the rate - still bounded. This is a deliberate
// simplest-sound choice, recorded in docs/plans/a8-market-blocking-record.md.
const (
	refusalAuditBurst   = 5
	refusalAuditRefill  = time.Minute
	refusalAuditMaxKeys = 10000
)

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

type refusalSampler struct {
	mu      sync.Mutex
	buckets map[refusalKey]*refusalBucket
	now     func() time.Time
}

func newRefusalSampler() *refusalSampler {
	return &refusalSampler{buckets: map[refusalKey]*refusalBucket{}, now: time.Now}
}

// take reports whether an audit row may be written for k now and, if so, how
// many refusals were suppressed since the last written row.
func (s *refusalSampler) take(k refusalKey) (write bool, suppressed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b, ok := s.buckets[k]
	if !ok {
		if len(s.buckets) >= refusalAuditMaxKeys {
			s.buckets = map[refusalKey]*refusalBucket{}
		}
		b = &refusalBucket{tokens: refusalAuditBurst, last: now}
		s.buckets[k] = b
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
		return false, 0
	}
	b.tokens--
	suppressed = b.suppressed
	b.suppressed = 0
	return true, suppressed
}

// refusalAudit is the process-wide sampler used by refuse.
var refusalAudit = newRefusalSampler()
