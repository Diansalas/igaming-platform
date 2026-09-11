// Package eventbus defines the platform's event-publishing interface and
// a Stage 1 in-memory implementation.
//
// STATUS: STUB. Per docs/decisions/0003-technology-stack.md's "Stage 1
// validation" section and the human's explicit instruction not to
// introduce distributed-systems complexity without a concrete reason,
// Stage 1 does NOT deploy Kafka or NATS - there is no consumer yet that
// needs cross-process delivery, ordering guarantees, or durability. What
// Stage 1 establishes is the Publisher interface every future producer
// (bonus engine, wallet, reporting CDC) will code against, so swapping
// the in-memory implementation for a real broker later is a
// configuration change, not a rewrite of call sites.
package eventbus

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event is the platform's envelope for a domain event. Payload is
// intentionally untyped (json.RawMessage-shaped []byte) at this
// foundation layer - per-event schemas belong to the owning domain
// (e.g. bonus-engine defines what player.registered carries), not to the
// bus itself. EventID exists from the start (even though the in-memory
// stub does nothing with it) because a durable, at-least-once broker
// introduced later will redeliver, and every consumer will need it for
// idempotency - the same reasoning CLAUDE.md applies to the ledger.
type Event struct {
	EventID    uuid.UUID
	Type       string
	TenantID   uuid.UUID
	Payload    []byte
	OccurredAt time.Time
}

// Publisher is what producers depend on. Domain code should accept a
// Publisher, never the concrete in-memory type, so it keeps working
// unchanged when a real broker-backed implementation is introduced.
type Publisher interface {
	Publish(ctx context.Context, event Event) error
}

// Subscriber lets a handler receive events of a given type. The
// in-memory implementation delivers synchronously, in-process only - it
// has no durability and does not survive a process restart, which is why
// it must never be used for anything the platform cannot afford to lose
// (i.e. never a substitute for the ledger).
type Subscriber interface {
	Subscribe(eventType string, handler func(context.Context, Event) error)
}

// InMemoryBus is a STUB implementation for Stage 1 development and
// testing. It is explicitly not durable, not ordered across types, and
// not cross-process - none of the properties the bonus engine will
// eventually depend on (Blueprint §4.5's per-player ordering) are
// guaranteed here.
type InMemoryBus struct {
	mu       sync.RWMutex
	handlers map[string][]func(context.Context, Event) error
}

func NewInMemoryBus() *InMemoryBus {
	return &InMemoryBus{handlers: map[string][]func(context.Context, Event) error{}}
}

func (b *InMemoryBus) Subscribe(eventType string, handler func(context.Context, Event) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], handler)
}

// Publish invokes every handler registered for event.Type, synchronously,
// in registration order. The first handler error is returned; later
// handlers for the same event are not invoked - callers needing
// independent handler failure isolation should not rely on this stub for
// that guarantee.
func (b *InMemoryBus) Publish(ctx context.Context, event Event) error {
	b.mu.RLock()
	handlers := append([]func(context.Context, Event) error(nil), b.handlers[event.Type]...)
	b.mu.RUnlock()

	for _, h := range handlers {
		if err := h(ctx, event); err != nil {
			return err
		}
	}
	return nil
}
