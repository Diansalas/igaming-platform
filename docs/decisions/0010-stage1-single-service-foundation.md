# ADR 0010 — Stage 1 Ships One Deployable Service, Not the Full Service Map

Status: Accepted. Recorded after Stage 1 specialist review found this
choice was implemented but not documented anywhere under
`docs/architecture/` or `docs/decisions/` — it existed only as a Go
package comment and a paragraph in ADR 0003.

## Context

`docs/architecture/02-domain-and-service-boundaries.md` proposes twelve
logical services grouped under roughly four deployables at full build-out
(Stage 6+). The human's Stage 1 instructions explicitly said: prefer the
simplest architecture that can credibly scale, do not create unnecessary
microservices, and do not create distributed-systems complexity without a
concrete reason. Stage 1 has no real per-domain business logic yet
(identity, wallet, bonus, etc. are all later stages), so there is no
concrete ownership or scaling need to split against.

## Decision

Stage 1 ships exactly one deployable: `platform-api`
(`cmd/platform-api`), containing config, observability, database/RLS
foundation, auth/tenant-context, HTTP foundation, and the event-bus
interface. Internally it is organized by package
(`internal/auth`, `internal/db`, `internal/tenant`, `internal/httpserver`,
`internal/eventbus`, `internal/apierror`, `internal/validation`,
`internal/config`, `internal/observability`) so that splitting a package
into its own deployable later is a build/deploy change, not a rewrite of
the package's internals or its call boundaries.

## Consequences

- `docs/architecture/02-domain-and-service-boundaries.md`'s proposed
  service map is the **target shape for later stages**, not a description
  of what exists after Stage 1. It is not being retracted — Stage 2+ still
  splits work along those lines as real domain logic (identity, wallet,
  bonus engine, etc.) is built — but each split happens when a concrete
  ownership, deployment, or scaling reason exists, not preemptively.
- The convention for a future split: a domain's code should already live
  in its own `internal/<domain>` package with a narrow, interface-shaped
  boundary to the rest of the codebase (mirroring how `internal/db`,
  `internal/auth`, and `internal/tenant` are structured now) before it is
  pulled into its own `cmd/<domain>-service` and given its own deployment.
  Shared code that multiple future services need moves to a `pkg/`
  (cross-module-safe) location at that point, not before.
- `internal/eventbus`'s `Publisher`/`Subscriber` interfaces exist
  precisely so that this split, when it happens, changes which
  implementation is wired in (in-memory → Kafka/NATS-backed), not the
  call sites that publish or subscribe to events.

## Owner

`architect`.
