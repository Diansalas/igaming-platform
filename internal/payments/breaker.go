// PRH-I1 step (c): the orchestrator-owned breaker, ADR 0095 §9.6. A
// minimal, in-memory, per-(tenant, provider) circuit breaker: fed by
// ErrorClass (a consecutive Ambiguous/NotSent transport failure counts;
// DefiniteDecline never does), consulted by RankRoutingCandidates
// OUTSIDE any transaction (no lock, no DB read - a plain in-memory map
// read/write), and never shared across tenants (a breaker open for
// tenant A's use of provider P never affects tenant B's use of the same
// P).
//
// This is intentionally the smallest correct version of §9.6's breaker:
// closed -> open on N consecutive Ambiguous/NotSent results; open ->
// half-open after a cooldown; half-open's probe is literally "the next
// real call is allowed through" (§9.6's own words), and that single
// call's outcome decides closed (success-class outcome) or open again
// (another Ambiguous/NotSent). A credential-store outage
// (ErrorClassNotSent from the gate's own credential-resolution refusal)
// still counts here, same as any other NotSent - PROV-OUTBOUND-CRED-1's
// own "a credential-store outage is not counted as a provider failure"
// rule (§9.6) is the CALLER's responsibility: RecordResult must only be
// called for a result that actually reached, or attempted to reach, the
// provider's own transport, never for a pre-flight refusal the gate
// itself made before any adapter code ran. deposit_v2.go's phase C
// upholds this by only calling RecordFromDeposit for gate results whose
// Class came from the adapter call itself.
package payments

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// breakerConsecutiveFailureThreshold and breakerCooldown are the two
// tunables this minimal breaker needs. Making these tenant/provider-
// configurable is future work (not required by ADR 0095 §9.6, which
// specifies the state machine, not the thresholds).
const (
	breakerConsecutiveFailureThreshold = 5
	breakerCooldown                    = 30 * time.Second
)

type breakerEntry struct {
	state               CircuitState
	consecutiveFailures int
	openedAt            time.Time
	// halfOpenProbeInFlight prevents two concurrent callers from both
	// treating themselves as "the" half-open probe; only the first one
	// through gets to decide the transition, everyone else sees Open
	// until it resolves.
	halfOpenProbeInFlight bool
}

// Breaker is safe for concurrent use. The zero value is ready to use
// (every provider starts closed).
type Breaker struct {
	mu      sync.Mutex
	entries map[string]*breakerEntry
}

// NewBreaker returns a ready-to-use Breaker with every provider closed.
func NewBreaker() *Breaker { return &Breaker{entries: map[string]*breakerEntry{}} }

func breakerKey(tenantID uuid.UUID, providerID string) string {
	return tenantID.String() + ":" + providerID
}

// Allow reports whether tenantID may currently attempt providerID: true
// when closed, true (exactly once, as the probe) when half-open, false
// when open and still within its cooldown. Takes no transaction, no
// tx - this is a plain in-memory read/write, callable outside any
// transaction (ADR 0095 §9.6: "an open breaker removes the candidate
// from routing without a DB transaction being held").
func (b *Breaker) Allow(tenantID uuid.UUID, providerID string) bool {
	if b == nil {
		return true // a nil breaker (not configured) never blocks routing.
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[breakerKey(tenantID, providerID)]
	if e == nil || e.state == CircuitClosed {
		return true
	}
	if e.state == CircuitOpen {
		if time.Since(e.openedAt) < breakerCooldown {
			return false
		}
		e.state = CircuitHalfOpen
		e.halfOpenProbeInFlight = false
	}
	// CircuitHalfOpen: exactly one caller gets to be the probe.
	if e.halfOpenProbeInFlight {
		return false
	}
	e.halfOpenProbeInFlight = true
	return true
}

// RecordResult feeds one outbound call's outcome for (tenantID,
// providerID). class must be the class the GATE actually returned for a
// call that reached, or attempted to reach, the adapter's own transport
// - never a pre-flight refusal (see the package doc comment).
func (b *Breaker) RecordResult(tenantID uuid.UUID, providerID string, class ErrorClass) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := breakerKey(tenantID, providerID)
	e := b.entries[key]
	if e == nil {
		e = &breakerEntry{state: CircuitClosed}
		b.entries[key] = e
	}

	isTransportFailure := class == ErrorClassAmbiguous || class == ErrorClassNotSent
	if !isTransportFailure {
		// DefiniteDecline, Pending, Succeeded: a real, business-level
		// response from the provider - the transport works. Reset fully,
		// including out of half-open back to closed.
		e.state = CircuitClosed
		e.consecutiveFailures = 0
		e.halfOpenProbeInFlight = false
		return
	}

	if e.state == CircuitHalfOpen {
		// The probe itself failed: back to open, fresh cooldown.
		e.state = CircuitOpen
		e.openedAt = time.Now()
		e.halfOpenProbeInFlight = false
		return
	}

	e.consecutiveFailures++
	if e.consecutiveFailures >= breakerConsecutiveFailureThreshold {
		e.state = CircuitOpen
		e.openedAt = time.Now()
	}
}

// State returns the current breaker state for (tenantID, providerID),
// for tests and observability only - never consulted by routing directly
// (routing calls Allow).
func (b *Breaker) State(tenantID uuid.UUID, providerID string) CircuitState {
	if b == nil {
		return CircuitClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[breakerKey(tenantID, providerID)]
	if e == nil {
		return CircuitClosed
	}
	return e.state
}
