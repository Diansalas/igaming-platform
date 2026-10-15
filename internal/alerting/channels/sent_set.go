package channels

import "sync"

// sentSet is a small bounded, process-local record of delivery keys that were already delivered
// successfully, so a RETRY of the same dedup key (for example after the dispatcher reclaimed a stale
// claim, or a partial multi-recipient send) does not notify again. It is an optimisation of the
// at-least-once contract, not the contract: delivery across processes and restarts is deduplicated by the
// receiver on the Idempotency-Key / dedup key the channel sends. Oldest keys are evicted first once the cap
// is reached; an evicted key can at worst be delivered one more time (at-least-once, never lost).
type sentSet struct {
	mu    sync.Mutex
	cap   int
	order []string
	set   map[string]struct{}
}

func newSentSet(capacity int) *sentSet {
	return &sentSet{cap: capacity, set: make(map[string]struct{}, capacity)}
}

func (s *sentSet) has(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.set[k]
	return ok
}

func (s *sentSet) add(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[k]; ok {
		return
	}
	if len(s.order) >= s.cap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.set, oldest)
	}
	s.order = append(s.order, k)
	s.set[k] = struct{}{}
}
