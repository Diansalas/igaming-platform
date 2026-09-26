> Stage 10.1 PAY-WH-TENANT-1 — specialist working paper (verbatim, 2026-09-26). The Orchestrator rulings in `11-pay-wh-tenant-1-design.md` §"Review record and Orchestrator rulings" govern.

# Backend/API review — PAY-WH-TENANT-1 + API-DOC-PAYWH design (11-pay-wh-tenant-1-design.md)

## Verdict
APPROVE with 4 required changes before implementation starts. Design is internally
consistent with the verified current state of `deposit_handlers.go` and
`payment_deposit_simulation_handlers.go`. No new service boundary, no new
tenant-isolation mechanism — within backend implementation authority.

## Findings

1. **Two error-mapping call sites will diverge if not unified.** The public webhook
   handler (`newPaymentWebhookHandler`, `deposit_handlers.go:257-362`) has its own
   inline `errors.Is` chain; the simulation route calls a *separate* helper
   `writeDepositCallbackError` (`payment_deposit_simulation_handlers.go:216`), which
   today has no `ErrDepositAlreadyReversed`/`ErrReversalAlreadyExists` branch either.
   §6 says the simulation route must map `ErrCallbackAuthFailed` to 503 (generic
   misconfiguration), which is correct for simulate but must NOT apply to the public
   webhook (which must map it to 401). The design does not explicitly say whether the
   public webhook's inline chain and `writeDepositCallbackError` get one shared
   mapper or stay two hand-maintained chains. Recommend: factor a single
   `mapCallbackError(isSimulation bool, err) (code, msg)` (or two thin wrappers over
   one shared switch) so PAY-REV-1's new 409 branch and PAY-WH-TENANT-1's new 401
   branch are added exactly once, not twice, avoiding drift between the two routes.
   This is an implementation detail the design correctly leaves open, but it should
   be called out to whoever picks up PAY-REV-1 and PAY-WH-TENANT-1 concurrently in
   the same file, since both land in `deposit_handlers.go`/adjacent file in the same
   stage.

2. **404-still-exists check: consistent, no contradiction.** §3.2's table is correct:
   pre-verification unknown tenant/provider/signature ⇒ uniform 401. Post-verification
   `ErrDepositIntentNotFound` ⇒ 404 is a *different* failure (right tenant, right
   provider, right signature, but the referenced deposit intent doesn't exist) and
   stays reachable only by a verified caller, matching current
   `deposit_handlers.go:336-339`. OpenAPI listing both 401 and 404 on the same
   operation is therefore correct, not redundant — confirm the description text says
   "404 is only observable after signature verification" so a reader doesn't assume
   it's another enumeration oracle.

3. **`ErrUnknownProvider` currently maps to 404** (`deposit_handlers.go:332-335`) but
   under the new contract must become part of the uniform 401
   (`provider_unregistered`/`provider_invalid` reasons in §3.2). Confirm this is
   understood as a *removal* of the existing `if errors.Is(err, payments.ErrUnknownProvider)`
   branch in the webhook handler, not an addition alongside it — otherwise the old
   404 branch would shadow the new 401 mapping depending on error-check order (they
   are mutually exclusive today since `ErrUnknownProvider` is returned before
   verification, but after PAY-WH-TENANT-1 lands it must be folded into
   `ErrCallbackAuthFailed` and the standalone branch deleted).

4. **Body-size limit unchanged and correctly placed.** §3.1 keeps the existing
   `maxWebhookBodyBytes` (1 MiB) read *before* header/signature checks
   (`deposit_handlers.go:297-305`). That ordering is fine (a request that fails size
   before signature check still leaks nothing, since size limit is a flat constant
   applied identically to every tenant/provider — no enumeration value). No change
   required; note it in the OpenAPI 400 case as "malformed body after verification,
   or body too large" per §6, which is what's proposed — good.

5. **Header contract format check location unspecified.** §3.1 step 1.5 says "read
   both headers, well-formed" happens in the HTTP handler, before `WithTenant`. Confirm
   the regex checks (`^v1=[0-9a-f]{64}$` for signature, `^[a-z0-9-]{1,32}$` for key id)
   run there too (cheap, no DB/tenant needed) rather than deferred into the
   orchestrator — doing it in the handler lets a malformed-header request fail before
   even the tenant-slug lookup, tightening the timing-oracle argument in §3.2's
   "Residual" note. The design doesn't explicitly forbid deferring it to the
   orchestrator; recommend making it explicit in the implementation PR description so
   `qa`'s T7 (63/65 hex chars, missing header) has an unambiguous place to assert 401
   is reached without a DB round trip.

6. **Simulate route 503 mapping needs its own `ErrCallbackAuthFailed` branch**, since
   `writeDepositCallbackError` is shared code today; if PAY-WH-TENANT-1's public-route
   401 mapping is added to the *same* shared helper (see finding 1) without an
   `isSimulation` flag, the simulate route would incorrectly return 401 instead of
   503 for a resolver misconfiguration. This must be resolved before implementation
   (either two switches, or one switch with a route-kind parameter).

7. **`main.go` wiring is minimal and correct.** `payments.NewOrchestrator` currently
   takes only `map[string]payments.PaymentProvider` (`cmd/platform-api/main.go:140`).
   §6's proposed addition of a second constructor arg
   `map[string]payments.WebhookCredentialResolver` is a signature change to
   `NewOrchestrator` — confirm all other call sites of `NewOrchestrator` (tests) are
   in scope for `qa`'s "~38 call sites" estimate, and that production code has this
   single call site only (verified: one match in main.go, none elsewhere in
   non-test code per this review's grep).

8. **OpenAPI response table completeness.** §6's list (200/400/401/404/409/500/503)
   is correct and matches the handler's reachable outcomes once PAY-REV-1 and
   PAY-WH-TENANT-1 both land. One gap: 409 currently covers two distinct causes
   (F-7 payload mismatch, PAY-REV-1 already-reversed) with an identical generic body
   per design — confirm the OpenAPI `description` for 409 documents both causes are
   collapsed to the same generic message intentionally (matches the "generic body"
   requirement in ADR 0090 item 1), so a future reader doesn't file it as a doc bug.

9. **No RLS/tenant-isolation concerns from backend view.** §5 keeps the only
   pre-verification statement read-only under `app.tenant_id`; this matches the
   existing `WithTenant` transaction pattern used everywhere else in this file. No
   objection.

## Required changes before implementation
- (1) Decide and state explicitly: one shared error-mapper with a route-kind flag,
  or two independently-maintained chains — pick one, document it in the design so
  PAY-REV-1 and PAY-WH-TENANT-1 implementers touching the same file don't hand-edit
  divergent copies.
- (3) Explicitly call out removal of the standalone `ErrUnknownProvider` → 404 branch
  in `newPaymentWebhookHandler`, folded into the uniform 401.
- (5) State explicitly where header-format validation runs (handler vs. orchestrator)
  so tests target the right boundary.
- (6) Resolve the simulate-route/public-route 503-vs-401 collision if a shared helper
  is used (blocks correctness, not just style).

None of these require an architecture change or new tenant-isolation mechanism;
all are within backend/API implementation discretion. Recommend `security` and
`code-reviewer` re-confirm items 1/3/6 at implementation-review time since they
affect the auth-failure code path.
