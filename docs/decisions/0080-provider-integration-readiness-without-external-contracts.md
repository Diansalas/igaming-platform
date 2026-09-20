# 0080 — Provider Integration Readiness Without External Contracts (Stage 8)

## Status

Accepted.

## Context

Stage 8 was originally scoped to integrate a "Dummy Sportsbook API" and a
"Dummy Casino API" the platform owner said would be available. Neither
API's documentation, base URL, credentials, or contract was actually
available at Stage 8's start (verified by reconnaissance: repo grep,
`.env.example`, `docker-compose.dev.yml`, `/etc/hosts`, filesystem — no
trace of either provider anywhere reachable from this environment). The
platform owner explicitly re-scoped Stage 8 to **provider-integration
readiness without external API calls**: harden the existing provider
boundary and build everything a real adapter will need, without guessing
that adapter's actual shape. This ADR records the concrete decisions that
readiness work required.

Every decision below is additive to the completed Stage 4A (casino) and
Stage 6 (sportsbook) architecture — CLAUDE.md's "do not redesign completed
domain architecture" and this stage's own "this is NOT a redesign stage."

## Decision 1 — Casino provider-round persistence (resolves ADR 0048's residual limitation)

ADR 0048 (Stage 7) documented a known limitation: the casino round read
model (`internal/casino/history.go`) re-derives a round's `correlation_id`
from `roundCorrelationID(tenantID, providerID, roundID)`, a convention the
Stage 7 play-simulation seam introduced by always setting
`CallbackEvent.RoundID = session.ID.String()`. A real provider declares its
own round id, which is never assumed to equal a session id
(`CallbackEvent.RoundID`'s own doc comment) — so a real provider's round
would have nowhere durable to be looked up from.

**New table `casino_provider_rounds`** (migration 0080, additive, no
existing table altered): one row per `(tenant_id, provider_id,
provider_round_id)` first observed, binding it to the platform's own
`launch_session_id`, `player_account_id`, `brand_id`, `game_id`, and the
`correlation_id` its ledger transactions use — durable, provider-neutral,
queryable by provider identifiers alone (not only by session id).

**Uniqueness scope**: `UNIQUE (tenant_id, provider_id, provider_round_id)`.
This is the conservative default — it mirrors `ledger_transactions`'s own
existing `idx_ledger_transactions_tenant_provider_tx` scoping exactly
(tenant + provider, since a round id is that provider's own namespace, and
a provider is never assumed to share round-id namespaces across tenants
it serves). Because each row carries exactly one `player_account_id`, this
single constraint is also what makes a provider round id belong to exactly
one player: a second row attempting to claim the same
`(tenant_id, provider_id, provider_round_id)` for a different player is a
uniqueness violation, not an application-level check. **This scope is a
documented assumption, not a fact about any real provider's contract** —
Stage 8 §5's own instruction ("unless the actual provider contract defines
a different scope") means this must be revisited once real documentation
exists; if a real provider's round ids are scoped more broadly (e.g.
globally unique across all its tenants) or more narrowly (per game
session), this table's unique constraint is additive to relax or tighten,
not a redesign.

**Binding point**: `internal/casino/orchestrator.go`'s `postBet` — the one
event type `CallbackEvent.SessionID` is required and authoritative for
(ADR 0025 §… / CallbackEvent's own doc comment) — upserts a
`casino_provider_rounds` row keyed by `(tenant_id, provider_id,
event.RoundID)`, using the resolved session's own
`player_account_id`/`brand_id`/`game_id`. The bind happens immediately
before `ledger.Post`, i.e. only once RG, Risk, and the insufficient-funds
check have all already passed — an RG- or risk-declined bet (which
`postBet` returns as `(OutcomeDeclined, nil)`, a committed outcome) leaves
no `casino_provider_rounds` row, so the table's population semantics stay
"rounds that were actually bet on," not "rounds a provider merely
attempted." `postWin`/`postRollback` are unchanged by this decision: they
continue to resolve accounts entirely from the ledger's own prior entries
(a stronger anchor than this table, since it is the actual money-moving
history), and do not consult `casino_provider_rounds` at all. Its own read
helper, `LookupProviderRound`, currently has no production call site
outside the admin visibility path (Decision 5) — that is accurate as
shipped, not a gap this ADR is claiming closed.

The ownership-conflict predicate (post-review fix) checks only
`player_account_id` and `brand_id`, not `launch_session_id` — a round
belongs to exactly one player and brand, never to exactly one launch
session, so a same-player/same-brand continuation of one provider round
across two launch sessions (e.g. a free-spins round outliving a session
timeout, an ordinary casino flow) succeeds and advances the binding's
`launch_session_id`/`last_seen_at`, while a genuinely different
player or brand naming the same `(tenant_id, provider_id,
provider_round_id)` is rejected with `ErrProviderRoundOwnershipConflict`
(mapped to an HTTP 409 with a generic, non-revealing message at both the
public webhook and the play-simulation endpoint — never a 500, and never
an echo of the round id or either identity back to the caller).

`casino_provider_rounds` also carries the same `BEFORE UPDATE`
immutability trigger convention every comparable binding/credential table
in this repo already has (`casino_launch_sessions`, `withdrawal_requests`,
`assets`, `bonus_grants`) — only `last_seen_at` and `launch_session_id`
may change on an existing row; every other column, including a NULL→value
transition on `provider_session_id`, is otherwise frozen by the database,
not by application discipline alone.

This does not change `history.go`'s existing correlation-id-recompute read
path, which remains correct (the same deterministic
`roundCorrelationID` function) — it adds a durable, independently-queryable
index alongside it, which is what a real provider integration needs and
what ADR 0048 deferred.

## Decision 2 — Sportsbook provider bet reference (readiness only)

`sportsbook_bets` gains two nullable, additive columns: `provider_id TEXT`
and `provider_bet_reference TEXT`, with the identical symmetric-null constraint
`ledger_transactions` already uses (`CHECK ((provider_id IS NULL) =
(provider_bet_reference IS NULL))`) and a matching partial unique index
`(tenant_id, provider_id, provider_bet_reference) WHERE provider_id IS NOT
NULL`. Both columns are always NULL for every bet placed this stage (bet
placement remains the existing same-process, synchronous `PlaceBet` flow —
no external round-trip exists or is added). This is deliberately the
**minimum additive schema** a future real sportsbook adapter needs to
record its own acceptance reference, without inventing whether that
provider's bet placement is synchronous or asynchronous, what its ack
shape is, or what its own field is called. `sportsbook.Provider` (the
adapter interface) is **not** extended with a bet-placement method this
stage — doing so would require guessing that unknown contract's shape,
which Stage 8 explicitly forbids. Adding it is deferred to whenever a real
sportsbook provider's bet-placement contract is actually known.

## Decision 3 — Generic provider HTTP client + contract-test harness

New package `internal/providers/httpclient`: a transport-agnostic outbound
HTTP client any future real adapter (casino or sportsbook) composes,
implementing exactly the "minimum production-quality" behavior Stage 8
§12 asks for and nothing more (no service mesh, no circuit-breaker
framework):

- Explicit per-call timeout (`context.WithTimeout`, configured, not
  inherited indefinitely).
- Bounded retry **only** for requests the caller marks idempotent
  (`Idempotent: true` on the request) and only for transport-level
  failures (connection refused/reset, timeout) or 5xx — never for a 4xx
  rejection, and never for a non-idempotent operation regardless of
  failure class, per Stage 8 §12's explicit rule.
- Structured error classification into four sentinel categories:
  `ErrProviderTimeout`, `ErrProviderUnavailable` (transport failure or
  5xx), `ErrProviderRejected` (4xx), `ErrProviderMalformedResponse` (body
  fails caller-supplied decode) — so a future adapter's own error mapping
  is a `switch` over four already-distinguished cases, not raw
  `net/http`/`context` errors.
- One OTel span per call (`otel.Tracer("igaming-platform/providers")`),
  attributes: provider, operation, duration, outcome, and (when present)
  the provider's own reference from the response — never a credential,
  API key, or raw `Authorization` header value (Stage 8 §9/§13).

**Post-review hardening (security + QA, same stage, before this ADR was
accepted):** three findings were confirmed by independent reproduction
and fixed in `internal/providers/httpclient/` before commit, and are
recorded here since they are part of what "the shared pattern" means for
every future adapter:

- **No automatic redirect following.** Go's default `*http.Client`
  redirect behavior only strips `Authorization`/`WWW-Authenticate`/
  `Cookie`/`Cookie2` on a cross-domain redirect and forwards any other
  header verbatim — exactly the shape of `ClientConfig.AuthHeaderName`,
  which is deliberately an arbitrary vendor-defined header name (e.g. a
  bespoke `X-API-Key`). A redirecting (or compromised/impersonating)
  provider could otherwise exfiltrate that credential to an
  attacker-controlled host. `New()`'s default-constructed `*http.Client`
  now sets `CheckRedirect` to always return `http.ErrUseLastResponse`, so
  a 3xx response is returned to the caller as-is (never silently
  followed) — refusing redirects entirely is simpler and safer than
  selectively stripping headers, consistent with this Decision's own "no
  cleverness" instruction. A caller supplying its own `ClientConfig.
  HTTPClient` is documented as responsible for setting an equivalent
  policy itself, since `New()` cannot safely mutate a caller-owned
  client.
- **Context-cancellation is not a provider-health signal.** `Do`'s retry
  loop now checks `ctx.Err()` at the top of every iteration (before
  another attempt or backoff wait) and short-circuits immediately if the
  caller's own context is already done, rather than burning the entire
  retry budget in a near-instant loop and misclassifying the result as
  `ErrProviderUnavailable`/an ordinary provider timeout. This is
  deliberately still classified via `ErrProviderTimeout` (not a fifth
  sentinel) — a caller-side cancellation and a provider genuinely being
  slow are both, from a calling adapter's perspective, "we didn't get a
  timely answer" — but the concrete `*TimeoutError` now carries a
  `CallerCanceled bool` field so a caller that cares about the
  distinction (e.g. for its own logging/metrics) can tell them apart
  without a new error category every adapter's mapping would otherwise
  need to add.
- **`Sent`/delivered distinction on `TimeoutError`/`UnavailableError`.**
  The four-category taxonomy alone doesn't tell a future money-moving
  adapter whether a failed call definitely never reached the provider
  (safe to fail fast with no state) or possibly reached it (must write a
  pending/unknown-state record and reconcile). Both error types now carry
  a `Sent bool` field, set by the exact rule: `false` only for a
  request-construction failure (`http.NewRequestWithContext` rejecting
  the method/URL — no connection was ever attempted) or a confirmed dial
  failure (TCP connection never established, detected via
  `*net.OpError{Op: "dial"}`); `true` for everything else this package
  can observe, including a response-body read failure (a response had
  already started coming back), an HTTP status code being present at all
  (an HTTP response, by definition, means the provider received the
  request), and a mid-flight connection reset. The reset case is
  genuinely ambiguous — this package deliberately assumes the
  conservative "might have been processed" reading there, since that is
  the safe assumption for a financial caller.

**Contract-test harness**: `internal/providers/httpclient/conformance`
provides reusable `httptest.Server`-backed scenarios (success, timeout,
transport failure, 4xx, 5xx, malformed body, duplicate response, replay,
retry-then-success, "accepts then the platform's own context is
cancelled") that exercise `httpclient.Client` itself. These are explicitly
**provider-boundary/contract tests against this generic client**, not
tests of any external Dummy API — every test file and doc comment says so
verbatim, per Stage 8 §4's explicit instruction not to let this be
mistaken for an integration test. When a real adapter is built later, the
same harness can be pointed at a fake server shaped like that real
provider's actual documented contract to validate the adapter's own
mapping — the harness itself never encodes a guessed contract.

## Decision 4 — Provider configuration (generic, no real provider named)

`internal/providers/config.go` adds a small, generic
`ProviderConfig{Enabled bool; BaseURL string; APIKeyEnvVar string; Timeout
time.Duration; MaxRetries int}` shape and a `LoadProviderConfig(prefix
string) (ProviderConfig, error)` loader keyed by an env var prefix (e.g.
`CASINO_PROVIDER_DUMMY_*`, `SPORTSBOOK_PROVIDER_DUMMY_*`) — every field
optional, `Enabled` defaulting to `false`. No specific provider is wired
into `cmd/platform-api/main.go`'s `CasinoProvider`/`sportsbook.Provider`
maps this stage (there is nothing real to wire in) — this is scaffolding
only, exercised by its own unit tests (env var present/absent/malformed,
`Enabled=false` short-circuits before any network attempt is even
constructed — the fail-closed behavior Stage 8 §8 requires). No credential
value is ever hardcoded; `APIKeyEnvVar` is a *reference* to another
environment variable's name, resolved at call time, mirroring
`JWTSigningSecret`'s own env-only sourcing convention in
`internal/config/config.go`.

## Decision 5 — Back Office visibility

The existing minimal casino/sportsbook admin visibility (Stage 7 §15,
Stage 6.1) is extended, not rebuilt: the admin casino-rounds response
gains `provider_round_id` (sourced from `casino_provider_rounds` when a
binding exists, else empty), and the admin sportsbook bet list gains
`provider_id`/`provider_bet_reference` (both always empty this stage,
since nothing writes non-NULL values to `sportsbook_bets`' two new
columns yet — no sportsbook adapter exists). The casino side is **not**
empty-by-default in practice: `postBet` binds a `casino_provider_rounds`
row for every successfully-posted casino bet, including one placed
through Stage 7's play-simulation seam (which drives the real `postBet`
path with `RoundID = session.ID.String()`), so any dev/staging
environment that has ever exercised the casino play-simulation flow will
show a real (platform session id, not a provider-declared one) value in
this field. An operator should read a `provider_round_id` that looks like
a UUID as "the play-simulation seam's own session id," not as evidence a
real provider round exists. No new admin screen, no provider-management
console (Stage 8 §10/§17's explicit boundary).

## What this ADR does not do

It does not call any external network endpoint. It does not add a
bet-placement method to `sportsbook.Provider`. It does not change
`postBet`/`postWin`/`postRollback`'s existing account-resolution logic,
the ledger schema, or any RLS policy beyond the two additive tables/
columns above (both gain the same RLS shape as their owning table). It
does not build a reconciliation system (Decision 1's table plus the
existing `ledger_transactions.provider_tx_id`/`correlation_id` already
carry the identifier set a future reconciliation job would need — see the
Stage 8 completion report's reconciliation-readiness section for the full
inventory).

## Removal / extension condition

The moment real Dummy Sportsbook/Casino API documentation is available, a
real adapter is built as a normal additive package implementing
`CasinoProvider`/(an extended) `sportsbook.Provider`, composed with
`internal/providers/httpclient.Client`, configured via
`internal/providers/config.LoadProviderConfig`, and validated with the
`internal/providers/httpclient/conformance` harness pointed at a fake
server shaped like that real contract — none of the scaffolding this ADR
adds needs to change shape to accept it; only the uniqueness scope in
Decision 1 and the bet-placement contract deferred in Decision 2 may need
revisiting against the real contract's own documented behavior.
