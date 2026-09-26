> Stage 10.2 design review — specialist working paper (verbatim, recorded 2026-09-26 against 69e80c1). Where it differs from the Orchestrator rulings in `01-webhook-trust-design.md` §J, the rulings govern.

# Stage 10.2 architect review: 01-webhook-trust-design.md (HEAD d76bdd3)

**Overall: APPROVED WITH CONDITIONS (R1–R9).** No code changed. Checked against ADR 0091, 0022 §3 (+10.1 amendment points 1–7), 0019, 0025, 0028 and 0085, plus the code.

## 1. `internal/webhookauth` extraction: APPROVED, right level
- The move is correct. KYC and casino importing `internal/payments` would invert domain dependencies. A copy would be the "second, subtly different implementation" ADR 0091 forbids. The contents are only the 10.1 contract primitives, with no registry, orchestrator or plug-in model. That is not speculative, because there are three concrete consumers today.
- **R1. Scheme is only the platform-defined (MOCK) wire scheme.** It must not become a vendor wire format. Doc 08 says "the platform never invents a generic signature scheme". Real adapters verify with the vendor's own scheme. They still consume `Credential` and `Inbound`, and they must obey points 1–7. Put this in the package doc.
- **Per-domain prefixes: approved.** Distinct `Prefix`, distinct mock KDF label and distinct headers give three independent separations. Together they make ADR 0019 per-provider point 4 (disjoint originator sets) cryptographic for platform schemes. Keep P2's "cross-scheme under equal key → reject" test mandatory.
- **Shared `Reason` enum: approved.** Keep one closed log vocabulary. Each domain documents the subset it emits. `provider_not_configured` and `key_material` stay payments-only (ADR 0022 §4.1). Do not split the enum.
- **Mock helpers** (`NewMockMaster`, `DeriveMockKey`, `MockResolver`) go in `webhookauth/mock.go` with a MOCK banner. A subpackage is not required.
- **R2. The payments preamble adoption is mandatory.** The design currently makes it optional. Two preambles are the divergence ADR 0091 forbids. If the T9 byte-identity or payments OpenAPI tests go red, fix the shared preamble until it is byte-identical to payments. Never fork it.
  - The escape hatch applies only if byte-identity is provably impossible.
  - If used, it is registered as `WH-PREAMBLE-1` and disclosed in the completion report.
  - The payments test files must stay unedited except for imports (P1).

## 2. Database/RLS: APPROVED, no migration, no policy change
- **I1 for KYC and casino is stricter than point 4.** Before the adapter's MAC succeeds, the only permitted statements are:
  - the platform-scoped `GetTenantBySlug`;
  - the `set_config` inside `WithTenant`.

  No tenant-scoped config read is allowed either. KYC has no `ProviderAcceptsWebhook`, and casino `LoadCapability` stays post-verification (it already is, `orchestrator.go:577`). K7 and C7 statement capture enforce this and are gate evidence for G3.
- **R3. Tenant-id single source.** The `ReceiveCallback` signature is `(tenantID, providerID, in Inbound)`, which is redundant. Its first statements MUST overwrite `in.TenantID = tenantID; in.ProviderID = providerID`, exactly as payments does (`payments/orchestrator.go:857-858`). The handler passes the same `t.ID` to `WithTenant` and `ReceiveCallback`. The chain is then: route slug → `t.ID` → RLS context = resolver key = signing input = `cred.TenantID` check (I3) = every write. The payload never asserts the tenant, player or provider.
- **R4. KYC lookup and CAS.**
  - The lookup is `WHERE tenant_id=$1 AND provider_id=$2 AND provider_reference=$3`, matching unique index `idx_kyc_verifications_provider_ref` exactly. Today's query has no tenant predicate (`verification_service.go:256-262`).
  - The CAS is `UPDATE … WHERE id=$ AND tenant_id=$ AND status=$cur`. It is valid under READ COMMITTED.
  - A lost race gets a **bounded** re-read, at most 3 loops, then an error.
  - The transition and its audit row are in the same transaction.
- The casino money path is unchanged. The ledger key `(tenant, provider_id, provider_tx_id)` uses the route `provider_id` the credential verified (point 6). The composite FKs and FORCE RLS on `kyc_verifications` (migration 0040:84-92) are sufficient.

## 3. Env-gating matrix: APPROVED, fail-closed
- All mock gating flows from the single `cfg.TestSupportRoutesEnabled()` (ADR 0085 §1), through one testable `mockProviderWiring`. Production is structurally off: `Load()` refuses the flag there.
- **KYC:** the route is absent in `prod` and `TS-off`. That satisfies ADR 0091's "absent in production" literally. The 503 on self-service create is the disclosed consequence ADR 0091 already accepted. The nil-orchestrator paths are already handled (`kyc_handlers.go:97`, `kyc_admin_handlers.go:412`).
- **R5.** Route registration must be driven by the same value the wiring returns. Either:
  - drop `KYCWebhookEnabled` and register iff the resolver is non-nil; or
  - keep the flag, but set both it and the resolver from one `mockProviderWiring` result.

  K11 must prove they cannot diverge.
- **Real-vendor note:** once a real KYC vendor exists, its route follows the casino pattern: always registered, nil or real resolver, fail-closed 401. It is no longer gated by test support. Record this in the ADR 0028 amendment.
- **Casino:** the route is present everywhere, and a nil resolver in `prod`/`TS-off` gives 401 `no_resolver`. That is correct for a real-provider-facing route with no real aggregator.
- **Casino mock adapter still registered in production:** accepted for 10.2. With no resolver, nothing verifies, so no money moves. Register it as **MOCK-ADAPTER-PROD-1** (mock payments and casino adapters registered in production for initiation, catalogue and launch). It is a pre-launch checklist item, not 10.2 scope.

## 4. Deferrals
- **PAYWH-BRAND-1: defer (agree).** It is brand routing, orthogonal to forgery and cross-tenant effect. KYC has no per-brand provider config, and the casino check is tenant-wide and post-verification.
- **PAYWH-RL-1: defer (agree).** It is an availability concern. The pre-verification cost is one platform lookup plus one HMAC, with no writes, the same profile already accepted for payments.
- **PAYWH-TS-1: defer (agree).** Replay is already inert:
  - KYC: monotonic rank, terminal no-op, no audit append;
  - casino: `provider_tx_id` idempotency, F-7 409, tombstones.

  Point 3 already makes a timestamp tolerance mandatory for real adapters.
- **PAYWH-GATE-1: INCLUDE in 10.2.** It goes in a separate commit after the extraction commit, inside the same `mockProviderWiring`.
  - **Rationale:** it is the same class of fix ADR 0091 requires for casino ("production-safe gating of mock behaviour"), at near-zero cost. It makes the matrix uniform: payments `prod`/`TS-off` = nil resolver = all 401. Payments already fails closed on a nil resolver (`orchestrator.go:890`). It is reversible and adds no new requirement, so no human gate applies.
  - **Condition:** the payments tests stay green unedited, because they wire resolvers explicitly. If any non-wiring payments test needs editing, drop the commit and register `PAYWH-GATE-1` as deferred.
- **Other items.**
  - Casino capability-as-callback-kill-switch diverges from 0022 "status governs routing only". A disabled capability 503s a verified rollback, which can strand a debited stake. It is pre-existing (ADR 0025 review P1) and not a 10.2 blocker. Register it as **CAS-CAP-ROLLBACK-1** for `casino`/`ledger-finance`.
  - `kyc_handlers.go:101` hard-codes `Provider("mock")`. Pre-existing; note only.

## 5. ADR amendment set (architect records; D requires `ledger-finance` concurrence)
The amendments are A (0022), B (0028), C (0025), D (0019) and E (0085).

Also update:
- `docs/architecture/08-casino-integration-architecture.md` lines 176-195, whose steps 1 and 2 describe the superseded trust model;
- `payment-orchestration.md` §10, a pointer only.

ADR 0091 itself is not amended.

**A. ADR 0022 §3, appended to the 10.1 amendment**
> **Amendment 2026-09-XX (Stage 10.2, ADR 0091): contract extracted and extended.** The contract primitives (`Credential`, `Resolver`, `Inbound`, `Reason`, `AuthError`, the auth sentinels, `Scheme`) now live in provider-neutral `internal/webhookauth`. `internal/payments` keeps type aliases, with unchanged behaviour. Points 1–7 bind KYC (`/v1/webhooks/kyc/…`) and casino (`/v1/webhooks/casino/…`) callbacks exactly as they bind payments.
> 8. **Domain separation.** Each platform-defined (MOCK) scheme signs `Prefix‖0x00‖tenant_id‖0x00‖provider_id‖0x00‖key_id‖0x00‖raw body`, with a domain-unique prefix (`igaming.{payments|kyc|casino}.webhook.v1`), domain-unique headers and a domain-unique mock key label. A signature valid in one domain never verifies in another, even under an equal key. `Scheme` is not a vendor wire format: real adapters verify with the vendor's scheme under points 1–7.
> 9. **KYC and casino I1 is strict:** before verification succeeds, no tenant-scoped statement runs other than `WithTenant`'s `set_config`. That is stricter than point 4's configuration-lookup allowance, which only payments uses (`ProviderAcceptsWebhook`).
>
> **Status update.** KYC-WH-1 and CAS-WH-TENANT-1 conform for the **MOCK only**. The mock resolvers are wired only when `TestSupportRoutesEnabled()` is true (ADR 0085), including payments (PAYWH-GATE-1). A nil resolver fails closed (401 `no_resolver`). Real KYC and casino resolvers are `NOT IMPLEMENTED`, blocked as the payments resolver is. The casino and KYC tenant-binding conformance cases are mandatory, skip-to-fail, for the first real adapter. The bullet "Casino … and KYC … do not yet conform" is superseded.

**B. ADR 0028, new "Amendment (Stage 10.2)" after §6**
> §4's `HandleCallback(ctx, rawPayload)` becomes `HandleCallback(ctx, in webhookauth.Inbound, cred webhookauth.Credential)`. The adapter verifies raw bytes before parsing (0022 §3 point 7). `outcome` is validated against §5's closed enum after verification.
>
> §6's "tenant from the URL's tenant slug only" and "HMAC-signed with a dev-only secret" are superseded. The slug is a lookup hint. The tenant is established by a per-tenant credential whose tenant is in the signing input (0022 §3). The mock key is per-process random and derived per tenant. No secret exists in source.
>
> **Gate:**
> - The mock KYC provider, its resolver and the webhook route exist only when `TestSupportRoutesEnabled()`. Otherwise the route is absent (404) and player self-service creation returns 503.
> - A future real vendor's route is always registered and fails closed without a resolver.
> - No simulate route exists. A future one needs a further amendment: test-support-gated, `PermVerificationReview`, tenant from the JWT, in-process signing, signed bytes never returned.
>
> **Provider-driven transitions are monotonic** over §2: `unverified 0 < pending 1 < review_required 2 < approved|rejected|expired 3`.
> - Only rank-increasing callbacks change state, via compare-and-set, with one audit row.
> - Equal or lower rank, anything after terminal, and anything after a staff decision are no-ops: 204, no audit row.
> - `error` still never changes state and writes one failure audit row.
> - `identity-compliance` concurrence: [ref].
>
> **Other changes:** player responses omit `provider_reference`, which stays staff-only. The webhook success response is 204 with no body.

**C. ADR 0025, new "Amendment (Stage 10.2)" after §5**
> §5's "the tenant comes from the URL's tenant slug" is superseded by 0022 §3 as amended: the slug selects one candidate tenant, and the tenant is established only by a per-(tenant, provider) credential bound into the signing input. §1's `HandleCallback` takes `(webhookauth.Inbound, webhookauth.Credential)`, and `NewOrchestrator` takes a `webhookauth.Resolver`. The adapter MACs raw bytes, then parses, and a verified-but-malformed body is a distinct 400. §7: the mock's per-process key and the NUL-joined field MAC are replaced by the derived per-tenant key over raw bytes.
>
> **Capability check.** The tenant capability check (review P1) stays post-verification only: a verified caller gets 503, and an unverified caller never observes capability state. This knowingly differs from 0022 "status governs routing only" (follow-up CAS-CAP-ROLLBACK-1).
>
> **Resolver gating.** The mock resolver is wired only under `TestSupportRoutesEnabled()`. Otherwise every callback returns 401 and no casino money moves.
>
> **Play simulation.** Play-simulation signs in-process for `tc.TenantID`. An `AuthError` there is 503 "misconfigured", and signed bytes are never returned.

**D. ADR 0019 actor matrix** (replace the second 10.1 status bullet; add to per-provider point 4)
> *Amended 2026-09-XX (Stage 10.2; `ledger-finance` concurrence [ref]):* casino conforms with a `MOCK` credential only. This row's casino entitlement no longer rests on a follow-up. Casino callbacks now originate postings only for the tenant bound into the verified signature; a cross-tenant callback is rejected before any read, so it writes no tombstone.
>
> Point 4 addition: "KYC-provider callbacks originate **no** `LedgerTransaction` of any type. Platform-defined schemes carry a domain prefix (0022 §3 point 8), so payments, casino and KYC credentials cannot verify each other's callbacks."

**No KYC row is added to the matrix.** The matrix governs who may originate *postings*, and KYC callbacks post nothing. A row would be a category error; the point-4 sentence is the correct placement.

**E. ADR 0085 §1, one paragraph**
> Stage 10.2 also derives mock-provider *wiring* from `TestSupportRoutesEnabled()` through `cmd/platform-api` `mockProviderWiring`, which is unit-tested for production and non-production × flag. That covers the KYC mock and route, and the casino and payments mock webhook resolvers.

## 6. Human decisions: NONE (checked strictly)
- **Production self-service KYC loss** was decided by ADR 0091's "absent in production".
- **Staging refresh** is already reserved to the human.
- **Secret store and real resolvers** sit behind an existing gate.
- **No git-history rewrite:** declining an irreversible operation is the safe default, and the value authenticates nothing post-fix.
- **PAYWH-GATE-1** and the other rulings are reversible and in scope.

Disclose in the completion report:
- staging is forgeable until the refresh, and its KYC rows are untrusted synthetic data;
- both closures are MOCK-only;
- MOCK-ADAPTER-PROD-1;
- CAS-CAP-ROLLBACK-1.
