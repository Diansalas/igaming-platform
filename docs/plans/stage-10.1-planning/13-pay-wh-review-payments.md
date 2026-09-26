> Stage 10.1 PAY-WH-TENANT-1 — specialist working paper (verbatim, 2026-09-26). The Orchestrator rulings in `11-pay-wh-tenant-1-design.md` §"Review record and Orchestrator rulings" govern.

# PAY-WH-TENANT-1 design review — payments specialist

**Verdict: APPROVE, with 3 required design changes before implementation (none blocking a
re-review cycle; all are clarifications/additions, not redesigns).**

## Verification performed
Cross-checked design doc §0/§2/§3/§6 against HEAD code:
- `internal/payments/mock.go` (72-233, 306-418): confirmed one per-process `signingSecret`,
  MAC over event_type/references/outcome/amount/asset/decline_reason/cascadable only (no
  tenant/provider), `Signature` embedded in body, no NUL-guard, `HandleCallback(ctx, rawPayload
  []byte)`. Matches doc's "current state" exactly.
- `internal/payments/orchestrator.go` (829-1045): `ReceiveCallback(ctx, tx, tenantID,
  providerID, rawPayload)` calls `provider.HandleCallback(ctx, rawPayload)` with no tenant
  passed to the adapter; `receiveDepositReversalCallback` tombstones an unseen reference
  (932-952) before any reversal-specific check — confirms S-6 as described.
- `internal/httpserver/deposit_handlers.go` (257-339): tenant from `GetTenantBySlug` only,
  404 on unknown/inactive tenant, 400-class on `ErrCallbackSignatureInvalid`/`ErrInboundKeyMaterial`
  — confirms the 404/400 split the design replaces with uniform 401.
- `internal/payments/capability.go` (1-90): `provider_capabilities` keyed by
  `(tenant_id, provider_id, brand_id nullable)`, `status` and `callback_capabilities` both
  present; no existing webhook-capability gate anywhere — the proposed `ProviderAcceptsWebhook`
  check is net-new and strictly additive, not a loosening of an existing control.
- `internal/httpserver/payment_deposit_simulation_handlers.go` (260-324): tenant already comes
  from `tc.TenantID` (JWT) only, never a request field — confirms §6's simulate-route claim;
  the only change needed is threading `tc.TenantID` into `mock.CallbackPayload` and building an
  `InboundCallback`.
- `NewOrchestrator(providers map[string]PaymentProvider)` (orchestrator.go:46): confirmed a
  one-arg constructor today; adding a resolver map is the only signature change needed there.
- Call-site count: 7 payments-package + 5 httpserver test files use `.CallbackPayload(`, 72
  combined `.CallbackPayload(`/`ReceiveCallback(ctx` occurrences — consistent with the doc's
  "~38/~5" estimate (that count likely also includes individual call expressions within those
  files, not just files).

## Findings

1. **Interface/adapter correctness — sound.** Moving tenant binding into the signed content
   (`signing_input` prefixed with a NUL-delimited, route-resolved `tenant_id`/`provider_id`/
   `key_id`) and rebuilding it server-side rather than trusting any body field is the correct
   fix for S-6, and it composes correctly with the existing single-adapter-registry model — no
   PSP SDK type crosses into `HandleCallback`'s new `InboundCallback` struct, so the "never let
   a PSP SDK type leak into core domain logic" rule holds.
2. **`status='disabled'` bypass for webhooks — AGREE.** Accepting callbacks for a capability
   row with `status='disabled'` but `callback_capabilities IN ('webhook','both')` is correct:
   disabling a provider is a *routing* decision (stop sending new business to it), not a
   revocation of trust in in-flight settlement. Rejecting those callbacks would strand money
   already committed to that PSP with no way to resolve it except a manual ledger correction —
   worse than accepting a still-cryptographically-verified callback. The design correctly keeps
   this orthogonal to compromise handling (credential removal from the resolver is the actual
   kill switch). This matches ADR-level intent that adapters and routing are separate concerns.
3. **Idempotency/replay — unchanged, correctly so.** The ledger `(tenant, provider,
   provider_tx_id)` unique key, `deposit_intents` uniqueness, and the tombstone are untouched;
   the only new gate is *before* those are ever reached (verification happens pre-write). This
   is the right layering: PAY-WH-TENANT-1 closes the cross-tenant path, PAY-REV-1 (independently)
   hardens the same-tenant concurrent-reversal race. No interaction that weakens either.
4. **Simulate route — correct and appropriately narrow.** Sourcing the tenant only from
   `tc.TenantID` and never exposing signed bytes in the response closes the obvious "use the
   simulate route to mint a replayable cross-tenant callback" concern before it exists.
5. **Test migration — the inventory is accurate but underestimates a subtlety.** Several
   existing tests likely assert on `mockCallbackBody`'s JSON shape *including* the `signature`
   field (e.g., malformed-payload/tamper tests that flip one byte in the JSON `signature`
   string). Removing `Signature` from the wire body means those tests need to move to
   corrupting the `X-Payments-Signature` header instead, not just updating call sites
   mechanically. Flag this explicitly in the QA handoff so it isn't found only at compile time.
6. **PAY-REV-1 interplay — low conflict surface, but ordering matters.** PAY-REV-1 only touches
   the body of `receiveDepositReversalCallback` (adds an L2 lock, re-checks after it); it does
   not change `ReceiveCallback`'s outer signature. PAY-WH-TENANT-1 changes `ReceiveCallback`'s
   signature (`rawPayload []byte` → `InboundCallback`) and `HandleCallback`'s signature, and
   touches the *same file* (`orchestrator.go`) and the *same handler file*
   (`deposit_handlers.go`, which both wrap in `WithTenant` and both need their error-mapping
   sections edited — PAY-REV-1 adds a 409 branch, PAY-WH-TENANT-1 replaces the 404/400 branches
   with a uniform 401). **Recommend: land PAY-REV-1 first** (smaller, self-contained diff scoped
   to one function + migration 0092), then implement/rebase PAY-WH-TENANT-1 on top, so the new
   L2-lock code sits inside the already-updated call signature rather than the reverse. Whoever
   lands second must re-verify the design doc's §3.1(f) ordering claim ("PAY-REV-1's L2 lock and
   the tombstone stay after (e)") against the actual merged diff, not just the design.
7. **Minor gap, not blocking: brand scoping of the webhook-capability check.** The proposed
   `ProviderAcceptsWebhook(ctx, tx, tenantID, providerID)` has no `brandID` parameter, so (per
   the design's own §3.1(b) note) "a row for any brand counts." Given a callback carries no
   brand identity today, this is the only workable choice, but it means a tenant cannot disable
   webhook acceptance for one brand's use of a shared provider while keeping it enabled for
   another. Acceptable for 10.1; record as a deferred limitation rather than silently accepting
   it as permanent, in case B2B tenants need per-brand webhook isolation later.

## Required design changes before implementation

1. Add an explicit note in §6/§7 that removing the body `signature` field requires updating any
   test that tampers with the JSON `signature` value to instead tamper with
   `X-Payments-Signature`/`X-Payments-Key-Id` headers — not just a mechanical call-site rename
   (finding 5).
2. Add an explicit merge-order instruction for the two concurrent workstreams: PAY-REV-1 lands
   first; PAY-WH-TENANT-1 rebases on it and re-verifies the (e)-then-lock ordering claim against
   the merged code, not the design doc alone (finding 6).
3. Record the no-brand-scoping limitation of `ProviderAcceptsWebhook` as a named, deferred
   follow-up item (e.g. `PAYWH-BRAND-1`) rather than leaving it as an implicit consequence
   buried in prose (finding 7).

No other changes needed. The credential-resolver interface, signing_input framing, uniform-401
error handling, alerting allow-list, RLS untouched, and OpenAPI additions are all correct and
consistent with the actual callback pipeline and adapter interface as they exist today.
