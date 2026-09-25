> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# p101 backend/API review — PAY-REV-1, SB-T1-XMIN, ADR 0089

## 1. PAY-REV-1 API impact

**Current 500 confirmed.** `internal/httpserver/deposit_handlers.go:340-353`:
explicit branches exist for `ErrCallbackSignatureInvalid` (4xx),
`ErrUnknownProvider`/`ErrDepositIntentNotFound` (404),
`ErrCallbackPayloadMismatch` (409) — but none for
`payments.ErrDepositAlreadyReversed`. It falls through to the generic
`err != nil` branch → `apierror.CodeInternal` → HTTP 500, logged as
`payment_webhook_failed`. Confirmed by direct read, matches p101-payrev.md.

**Proposed fix, and it's the right one.** Add
`errors.Is(err, payments.ErrDepositAlreadyReversed)` → `apierror.CodeConflict`
(409), placed with the existing `ErrCallbackPayloadMismatch` branch. This is
consistent with a pattern this codebase already documents explicitly at
`deposit_handlers.go:324-327`: signature failures get a 4xx, "not a 500 ...
a real PSP retrying a 500 forever would otherwise never learn its
[error]" — the same retry-semantics logic applies directly here. A 500
tells a real PSP's retry logic "try again later," which for an
already-resolved-terminal-state reversal is wrong forever, not just wrong
now — it would retry indefinitely against a request that can never
succeed. 409 (not 200-with-ignored) is correct: this is not the
same-reference idempotent-replay case (that already correctly returns 200
via `ledger.Post`'s `AlreadyPosted` path, unchanged) — it is a genuinely
different, distinct-reference reversal attempt against an
already-consumed original, which is a real conflict the PSP's own
operator should see and stop retrying, not a delivery duplicate to
silently swallow. Response body: no new fields, same generic
`{code, message}` error shape every other mapped error uses — no
PII/reference echo, consistent with the file's own stated policy
throughout.

**Mock PSP / integration docs expectation.** No dedicated retry-policy doc
exists in this repo for the mock PSP found in this pass; the only codified
retry-semantics reasoning is the comment cited above (`deposit_handlers.go
:324-327`), and it already establishes 4xx-not-5xx as the house convention
for "caller/integrity" failures that must not be retried. The proposed
409 is consistent with, not a new convention beyond, that.

**Simulation handler: confirmed no impact.**
`payment_deposit_simulation_handlers.go` hardcodes
`CallbackEventDeposit`/`OutcomeSucceeded` only (grep confirms no
`CallbackEventDepositReversal` construction anywhere in `internal/httpserver`).
Its `writeDepositCallbackError` also lacks an `ErrDepositAlreadyReversed`
branch, but the route cannot emit a reversal, so this is an inert,
pre-existing gap — correctly out of scope, may be left as a documented
latent inconsistency only.

## 2. OpenAPI impact

**No `/v1/webhooks/payments/{tenantSlug}/{providerID}` path exists in
`docs/api/openapi/platform-api.yaml` at all** — confirmed by direct
grep/read: the only two occurrences of that string in the whole file are
prose references inside the `/v1/me/deposits/{id}/simulate-callback`
description (lines ~1580), not a path definition. (Contrast:
`/v1/webhooks/kyc/{tenantSlug}/{providerID}` *is* a documented path,
line 2455.) So strictly, "no OpenAPI changes" for PAY-REV-1 is correct in
the narrow sense that there is no existing documented response schema to
edit for the new 409 — but this also means the payment webhook endpoint's
contract (request/response shapes, all status codes including this new
409) has never been documented at all. Recommend flagging as a small,
pre-existing documentation gap independent of PAY-REV-1: add the missing
path (mirroring the KYC webhook's shape: no bearer auth, tenant from URL
slug, signature verification is the adapter's responsibility, response
codes 200/400/404/409/500) as a follow-up, not blocking this fix. Not
required to land with PAY-REV-1 itself.

## 3. Error-type plumbing

`payments.ErrDepositAlreadyReversed` already exists as a named sentinel
(`internal/payments`, confirmed present in `orchestrator.go`/`types.go`);
no new error type needs to be introduced, only the missing `errors.Is`
branch in the HTTP handler. Confirms p101-payrev.md's §3.3/§6.2. This is
ordinary, low-risk, in-scope caller-side plumbing — ownable by
`payments`/backend without an architect escalation.

## 4. SB-T1-XMIN: confirmed no API impact

Confirmed independently: the fix is a `CREATE OR REPLACE FUNCTION`
rewrite of `sportsbook_bet_settlements_validate`'s trigger body only
(a Postgres trigger, not an HTTP surface). No handler, route, request/
response shape, or error mapping in `internal/httpserver` touches this
function or `xmin`/`pg_xact_status` at all — the only writer chain is
`internal/sportsbook/settlement.go` `insertSettlementRecord` →
`writeTransition` → `voidBet`, all internal, called from
`SimulateSettlementEvent` which is itself only reachable from staff/
test-support driven settlement simulation, not a public webhook. The Go
error surface on the reject branch (`ErrSettlementIntegrity` mapping in
`lockAndPost`) is untouched — the DB check only gets more permissive
(accepts the savepoint case), no new Go-observable error shape. Confirms
no OpenAPI, no new error type, no new endpoint. Agree: no API impact.

## 5. ADR 0089 backend/API accuracy findings

Spot-checked the ADR's concrete API/tooling claims against code:

- **"`PermBonusReportRead` is defined, but no route uses it"** (§4.1) —
  confirmed: `PermBonusReportRead` exists in `internal/auth/permission.go`
  and is referenced in `internal/bonus/suggestion.go` and
  `internal/auth/bonus_permission_test.go`, but grep of
  `internal/httpserver/bonus_routes.go` shows no route gated by it.
  Accurate.
- **"Missing: an authenticated non-human route for `CreateSuggestion`"**
  (§4.1) — confirmed: `internal/bonus/suggestion.go` defines the
  suggestion object/lifecycle, but no `POST` route constructing a
  suggestion via a non-human/agent credential exists in
  `internal/httpserver`; only review routes (`POST /v1/admin/bonus/
  suggestions/{id}/{claim,decide}`) are wired, matching the ADR's own
  §4.1 table statement that only the review lifecycle is exposed today.
  Accurate.
- **"`bonus_suggestions.originating_model_version` exists (migration
  0056)"** — confirmed: `OriginatingModel`/model-version field is present
  in `internal/bonus/suggestion.go`. Accurate, and correctly scoped as "a
  precedent, not a full solution" (it doesn't record tool-registry
  version, prompt hash, or reproducibility inputs — all still missing).
- **"No `ai_agent` principal or actor type, permission, role, route,
  migration or OpenAPI change is made by this ADR"** (§7) — confirmed:
  no `ai_agent` string appears anywhere outside this ADR's own prose
  (not in `permission.go`, `jwt.go`, `audit.go`, or the OpenAPI file).
  The ADR is correctly self-consistent as a paper-only decision.
- **API/tooling layering claim (§4, "Agent/Tool API is a thin,
  policy-enforcing façade... owns no business rule")** — this is
  aspirational/future design, not a claim about existing code, and is
  labeled as such throughout (`NOT IMPLEMENTED`, "Missing" in §4.1). No
  accuracy issue: it does not claim any of this exists today.
- **One precision gap worth flagging to `architect`, not blocking:** §4.1
  lists `POST /v1/admin/bonus/suggestions/{id}/{claim,decide}` as "Exists"
  without stating whether `claim` is a real, separately-tested route or
  bundled with `decide` — a minor completeness note, not an accuracy
  error; did not verify route-by-route in this pass beyond confirming the
  file exists and is grep-referenced. Recommend `architect`/`bonus-engine`
  do a route-level pass if this ADR is ever used to scope real
  implementation work (out of scope for this planning-gate review).
- **No missing prerequisite found that the ADR fails to name.** §4.1's
  own "Missing" column already covers every gap this backend-owner pass
  independently spotted (reporting read models, liability read model,
  suggestion-create route, delegating-principal audit field, rate
  limiter, brand RLS). No correction needed.

**Overall verdict on ADR 0089's backend/API claims: accurate.** Every
concrete "exists" vs. "missing" claim checked against
`internal/auth/permission.go`, `internal/bonus/suggestion.go`,
`internal/httpserver/bonus_routes.go`, and the OpenAPI file matches actual
repo state at `56f5135`. No fabricated integration, no upgraded label, no
claim of a route/permission that isn't actually there.
