> Stage 10.1 PAY-WH-TENANT-1 — specialist working paper (verbatim, 2026-09-26). The Orchestrator rulings in `11-pay-wh-tenant-1-design.md` §"Review record and Orchestrator rulings" govern.

# PAY-WH-TENANT-1 design — architect review (design only, HEAD cff2eef)

## Verdict: APPROVED WITH CHANGES (C1–C7). Architecturally sound; no blocker.

## Rulings
**R1: ADR 0022 §3 is an engineering item, and the design may close it.**
- ADR 0022 §3 labels it `OPEN DECISION (security/payments, Stage 3B)`, and states the binding constraints: exactly one key is tried; the payload never asserts the tenant.
- It is not in the HDR register. Gate §Q lists ADR 0009, HDR-J-6/7/8/9, HDR-M-1/2, HDR-SB-1, OB-1 and vendor/legal/retail items. Gate §R.2 asked the human only about scope. The human added the item to scope with acceptance text (ADR 0090 amendment item 3).
- The closure must satisfy that text, and candidate 1 does. The URL only selects a candidate. The tenant is established when that tenant's single credential verifies a signature whose input contains the tenant.
- No HDR is reopened.
- **Correction C1.** Design §1 calls ADR 0009 "(OPEN)". ADR 0009 is Accepted. Only its pre-production confirmations and final provider confirmation are open.
- **Secret-store choice.** Picking a store inside ADR 0003/0009's "Vault or cloud KMS" is a `devops`/`security` engineering ADR, not a new HDR. Provisioning it (AWS change) and any real PSP credential need human authorization (Stage 10.1 stop point; CLAUDE.md environment safety). The real resolver is labelled `NOT IMPLEMENTED` / `BLOCKED` on that authorization.

**R2: Keep the resolver. Trim it to what 10.1 uses.**
- **Why credential resolution stays out of the adapter.** Per CLAUDE.md the platform owns per-tenant credentials, and the adapter owns only the vendor scheme. Resolving in the orchestrator and passing exactly one credential to the adapter makes "no cross-tenant key reachability" structural. This is the right split.
- **C2.** Wire one `WebhookCredentialResolver` into `NewOrchestrator`, not `map[providerID]Resolver`. The future real resolver is one platform component (handle table + secret store) keyed by (tenant, provider, key_id), not one per vendor. The MOCK resolver returns `credential_unavailable` for any provider other than its own. A nil resolver fails closed (the T14 equivalent).
- **C3.** Drop `MerchantAccountID` from the 10.1 struct, because nothing consumes it. Record the real-adapter merchant-account rule in the §3 amendment (below). The field is added with the first real adapter.
- **C4.** The tenant-binding tests become part of ADR 0022 §6's parameterized conformance suite, not mock-only tests:
  - A's credential or signature is rejected at B;
  - no resolver or credential means fail-closed;
  - a rejected callback leaves no rows.

  T5 (the same secret for both tenants) stays mock-specific, because a vendor cannot MAC our tenant id.
- **Unchanged.** `InboundCallback` carries raw headers and body into the adapter only. It is input, not a canonical output shape, so ADR 0022 §4.1(3) is not engaged. It must never be persisted or logged (already stated).
- **ADR 0014.** Option 2 does not apply to external PSPs. Its "webhook receiver" example means an internal tenant-scoped service caller.

**R3: Database/RLS. No migration is needed.**
- No policy change and no new table: the 10.1 resolver is a `MOCK` with no DB.
- `GetTenantBySlug` is a platform-level `tenants` read (unchanged).
- `ProviderAcceptsWebhook` runs on the `WithTenant(route tenant)` tx. The `provider_capabilities` policy (0028) requires `app.tenant_id` to match and `app.player_account_id` to be empty, which holds on the webhook path. The explicit `tenant_id=$1` predicate on top of RLS is accepted.
- **Invariants for `qa`/`code-reviewer`:**
  - **I1.** Before (e) succeeds, the only statements permitted are the platform `tenants` lookup and read-only, tenant-scoped configuration or credential-handle reads. That rules out:
    - reading `ledger_*`, `deposit_intents`, `wallets` or projections;
    - locks and writes;
    - `audit_log` rows.
  - **I2.** Credential lookup is scoped (tenant, provider, key_id) and runs under the route tenant's GUC. It never uses `WithoutTenant`/platform scope, never uses a cross-tenant index, and never trial-verifies. `key_id` is namespaced per tenant. The brand is never asserted by a callback; it comes from the intent.
  - **I3.** Every write after (e) uses `t.ID`, which is simultaneously the RLS GUC, the MAC input and `cred.TenantID`. A mismatch fails closed.
  - **I4.** `status='disabled'` does not reject callbacks. I concur with the design: under ADR 0022 §2, `status` is a routing kill-switch. Callback acceptance is revoked by revoking the credential. `payments` must co-sign this. It is recorded in the amendment so "kill-switch" is not misread.
- **C5 (to `security`, not overruling).** The pre-verification `key_material` 401 must still feed ADR 0022 §4.1(2)'s security-alert signal. `reason=key_material` in a log-based alert is sufficient if alerting keys on it.

**R4: Cross-domain consistency. Define the contract once; adopt it per domain later.**
- Casino (`casino/types.go:643`) and KYC (`kyc/provider.go:104`) share the identical `HandleCallback(ctx, rawPayload)` shape and the slug-only flaw.
- Casino callbacks are a posting actor under ADR 0019's "verified provider callback" row, so casino is non-conformant with ADR 0019 today.
- **Ruling.** The tenant-binding contract (below) is recorded once as a platform-wide invariant for all inbound provider callbacks: payments, casino, KYC, and future sportsbook or custody feeds.
- **No shared Go package in 10.1.** Extracting one would touch casino and KYC code, which is out of scope. Extract it when a second domain's fix is authorized.
- **C6.** Register **KYC-WH-1** (High, launch-blocking) and **CAS-WH-TENANT-1** (Medium) in `task-registry.md`. They currently exist only in design §8.
- **Escalation to the human now, via the orchestrator.** KYC-WH-1 is a self-approval path on any running deployment of this binary, and its secret is a committed constant. A scope ruling is the human's.

**R5: Records.** Record set below. Also:
- **C7.** Correct the stale code comments and `payment-orchestration.md` §5/§10 in the same change.
- **Owner concurrence.** ADR 0019's owner is `ledger-finance` (with `security`), so its wording correction needs `ledger-finance` concurrence noted in the amendment.

## Draft: ADR 0022 §3 amendment (append after §3's OPEN DECISION paragraph)
> **Amendment 2026-09-26 (Stage 10.1, PAY-WH-TENANT-1, ADR 0090 item 3) — §3 OPEN DECISION CLOSED (`security`/`payments`, recorded by `architect`; not a human decision, no HDR reopened).**
>
> **Candidate 1 is adopted.** The per-tenant webhook route selects exactly one candidate tenant. The platform resolves at most one credential for (that tenant, the route `provider_id`, the vendor-supplied key id), and that credential must verify.
>
> **Candidate 2 is not adopted.** A provider-supplied account id selecting the tenant is not permitted. Adopting it later requires a further amendment.
>
> **Binding contract** (the reference contract for every inbound provider callback, platform-wide):
> 1. The route is a lookup hint only. The tenant is established by successful verification with a credential bound to that tenant, and a payload field never asserts it.
> 2. Exactly one credential is tried. There is no trial across tenants, and adapters receive only that one credential.
> 3. The verified tenant is bound into what is verified:
>    - **Platform-defined schemes** (MOCK) put `tenant_id` and `provider_id` in the signing input.
>    - **Vendor schemes** must use per-merchant keys and, where the vendor signs a merchant or account id, require it to equal the credential's bound account.
>    - A vendor offering neither is not integrable without a further ADR.
>    - Real adapters enforce the vendor's signed-timestamp tolerance (PAYWH-TS-1).
> 4. Verification completes before any tenant-scoped financial or state read, lock or write. Before it, only read-only, tenant-scoped configuration or credential-handle lookups are allowed.
> 5. Every pre-verification failure is one indistinguishable response (401). Unauthenticated failures write no `audit_log` row and are logged with allow-listed fields only.
> 6. All writes use the verified tenant as both the RLS context and the binding. `(provider_id, provider_tx_id)` is keyed on the route `provider_id` that the credential verified.
>
> **Provider status.** `ProviderCapability.status` governs routing only. Callback acceptance is revoked by revoking the tenant's credential, so in-flight funds are not stranded.
>
> **Status.**
> - `MOCK` resolver only.
> - The real credential resolver (FORCE-RLS handle table + secret store, §2.2) is `NOT IMPLEMENTED`. It is blocked on the secret-store ADR and on human-authorized provisioning.
> - S-6 is closed for the MOCK only and stays launch-blocking for any real PSP.
> - Casino (CAS-WH-TENANT-1) and KYC (KYC-WH-1) callbacks do not yet conform. They are registered, not in scope.
>
> The Consequences clause "§3 leaves open *how* the right key is selected" is superseded by this amendment.

## Draft: ADR 0019 matrix row correction ("Verified provider callback" cell)
> …custodian events — scoped to the tenant **that the callback route resolves and that the callback's single per-(tenant, provider) credential then verifies, with the tenant bound into the verification (ADR 0022 §3 as amended 2026-09-26)**. The route alone never establishes the tenant, and a tenant/player identifier in the payload never does; the scope is **further narrowed to the originating provider** (see below).

Add a note below the table:
> *Amended 2026-09-26 (Stage 10.1, PAY-WH-TENANT-1; `ledger-finance` concurrence). The earlier wording ("tenant resolved from the credential the callback authenticated with") was not true of any implemented webhook: every route took the tenant from its URL slug under a tenant-agnostic key (S-6). Status after 10.1:*
> - *payments conforms with a `MOCK` credential only;*
> - *casino does not conform (CAS-WH-TENANT-1), and this row's entitlement for casino callbacks rests on that follow-up;*
> - *the sportsbook provider-callback entitlement remains unimplemented.*

## Draft: payments architecture docs
- **`07-payments-architecture.md:105-108`.** Replace "the callback path is authenticated … not from a stored per-tenant credential" with:
  > "Callbacks are tenant-bound (ADR 0022 §3 as amended). The per-tenant URL selects one (tenant, provider, key id) credential, and the signature covers the tenant and provider. Only a `MOCK` resolver exists. The real per-tenant credential store is `NOT IMPLEMENTED`, and no real PSP may be connected until it exists."
- **`payment-orchestration.md` §5 (~l.88-100).**
  - New signature: `ReceiveCallback(ctx, tx, tenant_id, provider_id, InboundCallback)`.
  - Add the verification order (a)–(f), the uniform 401, and the credential equality re-check.
  - Delete "There is no stored 'verified callback credential' concept…", replacing it with the MOCK/NOT IMPLEMENTED status.
- **`payment-orchestration.md` §10, third bullet.** Replace "the tenant comes from the key that verified the signature" with the amended contract points 1–4, citing ADR 0022 §3.
- **Code comments.** Update `deposit_handlers.go:244-256` and `orchestrator.go:798-828` (the comment above still says the key-selection question is "unresolved").
- **`docs/integrations/*.md`.** No change; the dummy-casino and dummy-sportsbook checklists already ask for the signature scheme. When CAS-WH-TENANT-1 is scheduled, add the "per-merchant key or signed account id" question there.

## Not decided here
- The secret store and its provisioning (devops/security ADR plus human authorization).
- The KYC-WH-1 and CAS-WH-TENANT-1 scope (human).
- Rate limiting (PAYWH-RL-1).
- Raw-payload retention (ADR 0022 §4.1(4)).
