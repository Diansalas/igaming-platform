# Integration Protocol

Permanent project governance document (Stage 4G, Part A). Defines how
cross-domain dependencies are requested, how interfaces are contracted,
the integration sequence, conflict resolution, and required tests/review
before one domain's work may be assumed to exist by another's.

## Dependency requests

When a specialist (or the Orchestrator acting on a specialist's behalf)
needs a change in a domain it does not own:

1. **Create a documented change/dependency request** — a short, explicit
   statement of: what is needed, why, the exact interface/behavior
   expected, and which of the requester's own tasks it blocks.
2. **Record it as a row in `task-registry.md`'s Dependency Request Log**
   (see that file — a single, append-only, cross-stage table, not a free
   -text note buried on an arbitrary task row). This is the literal
   answer to "how are dependency requests recorded": one table, one row
   per request, never edited after the fact except to fill in the
   **Resolved** column.
3. **Send it to the Master Orchestrator** — in this project's current
   single-session operating mode, this means the Orchestrator (already
   holding full context) evaluates the request directly rather than
   relaying it to a separate human/agent inbox.
4. **The Orchestrator assigns it to the owning specialist** per
   `ownership.md` — in practice, the Orchestrator performs the owning
   domain's implementation itself (see `agent-registry.md`'s "Working
   pattern" note) or dispatches a scoped `Agent` task when independent
   specialist judgment is what's actually needed (e.g., a security review
   of a proposed RLS policy before it's written).
5. **The owning specialist implements the change.**
6. **The owning specialist reports the resulting interface/behavior** —
   the exact Go signature, migration shape, or API contract that now
   exists, not just "it's done."
7. **The Orchestrator closes the Dependency Request Log row**, filling in
   **Resolved** with the commit/interface that satisfied it.
8. **The dependent work integrates against the reported change** — never
   against an assumption of what the change "probably" looks like.

No specialist may write code against another domain's file, or assume a
change exists, before step 8. This is an absolute prohibition, not a
default that a specialist may depart from under time pressure — see
`agent-registry.md`'s "Absolute constraint on every specialist."

## Interface contracts

A cross-domain interface (e.g. `internal/risk.KYCProvider`-style
boundary, or a call from `internal/casino` into `internal/risk.Evaluate`)
is contracted the same way this codebase already contracts provider
interfaces (`CasinoProvider`, `PaymentProvider`, `KYCProvider`,
`PersonResolver`):

- The interface is defined ONCE, by its owning domain, in that domain's
  own package.
- A caller depends on the interface type, never on the owning domain's
  internal implementation details.
- A change to the interface's signature is itself a dependency request
  flowing through this same protocol — the owning domain does not change
  a shared interface without notifying every known caller (found via
  `grep` across the repository, not assumed from memory).

## Integration sequence

For a stage introducing a new cross-domain capability (e.g. Stage 4G's
Risk & Limits engine consumed by `internal/casino`):

1. Architecture/interface design (Orchestrator, or `architect` for a
   genuinely cross-cutting decision) — recorded as an ADR before
   implementation starts for anything non-trivial.
2. Owning domain implements the new package/migration in isolation
   (compiles, unit-tested, without yet being called by any consumer).
3. Owning domain's own tests pass (`go test`, `go test -race`,
   `go test -tags=integration`, `go test -race -tags=integration`) before
   any consumer is wired in.
4. Consumer domain integrates against the now-stable interface.
5. A NEW integration test exercises the consumer calling the real
   (non-mock, where applicable) interface end-to-end.
6. Specialist review (per `docs/governance/change-control.md`'s
   requirements for the kind of change made).
7. The Orchestrator marks the capability "integrated" only after steps
   1-6 above all pass — no agent may assume another agent's work exists
   or is correct before this point. (Corrected per documentation review:
   an earlier version of this line said "steps 3-6," inconsistent with
   the "How integration approval is recorded" section below, which
   correctly cites all of 1-6.)

## Conflict resolution

- **Two rules/policies matching the same request with equal
  specificity**: this is a Risk & Limits *configuration* conflict, not an
  integration-protocol conflict — resolved by `internal/risk.Evaluate`
  itself failing closed (see `docs/decisions/0031`), never by this
  document.
- **Two specialists disagree about an interface shape**: the Orchestrator
  decides, informed by both specialists' written rationale, and records
  the decision (with reasoning) in the relevant ADR.
- **A specialist's finding implies another domain's prior work was
  wrong**: filed as a dependency request against the owning domain, not
  fixed directly by the finder unless the Orchestrator explicitly assigns
  it that way for a small, well-understood fix (mirroring how Stage 4F's
  cross-specialist findings were triaged and fixed by the Orchestrator
  directly after each review, rather than each reviewing agent patching
  code itself).

## Test and review requirements before integration is considered done

- The owning domain's own package tests pass in isolation.
- The consumer's integration test passes against the REAL interface
  (never only against a hand-rolled stub the consumer wrote itself,
  unless a mock implementation is what the owning domain officially
  ships — e.g. `MockKYCProvider`).
- Any RLS/tenant-isolation-relevant table introduced or touched has an
  adversarial cross-tenant test.
- `security` has reviewed anything touching auth, RBAC, PII, or secrets.
- `code-reviewer` has reviewed any change the Orchestrator judges
  "significant" (new package, new migration, new API surface).

## Nothing is assumed integrated until the Orchestrator says so

Per Part A's own governing rule: no agent may assume another agent's
work exists until the Orchestrator marks it integrated. In practice this
means: do not call a new type/function from another domain's
in-progress package based on its name or an earlier conversation turn —
re-read the actual current file, or ask the Orchestrator to confirm its
current shape, before depending on it.

## How integration approval is recorded

Per Part A's requirement that "Integration status" in `task-registry.md`
is only ever set by the Orchestrator, never by the implementing
specialist: when steps 1-6 of the Integration sequence above all pass
for a given capability, the Orchestrator records the approval as a row
in `task-registry.md`'s Integration Approval Log (a second permanent,
append-only, cross-stage table, alongside the Dependency Request Log),
citing the exact tests and specialist reviews that were the basis for
the approval — not merely "looks done." Only after that row exists does
the Orchestrator update the consuming task's own **Integration** column
from blank/"Pending" to "Integrated." A capability with no Integration
Approval Log row is not integrated, regardless of how confident its
implementer is.
