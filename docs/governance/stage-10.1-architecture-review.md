# Stage 10.1 — Final architecture review (ADR 0090)

- **Reviewer:** `architect`
- **Date:** 2026-09-26
- **Range reviewed:** `git diff 8561ac2..HEAD` (HEAD `250828b`), covering commits `9fb1aa2` through `250828b`
- **Inputs:**
  - `CLAUDE.md`;
  - ADR 0090;
  - the planning report `docs/plans/stage-10.1-planning-gate-proposal.md`;
  - my planning papers `docs/plans/stage-10.1-planning/05-review-architect.md` and `15-pay-wh-review-architect-db.md`;
  - the binding Orchestrator rulings in `11-pay-wh-tenant-1-design.md`;
  - ADRs 0019, 0020, 0022, 0082 and 0088 as amended;
  - the code, the migrations 0092/0093, the tests and the OpenAPI.
- **Scope:** review only. No code or record was changed by this review, and I ran no tests here. The DB-level claims below rest on code inspection and on PostgreSQL's documented semantics, not on a fresh run.

## Summary of verdicts

| # | Item | Verdict |
|---|---|---|
| 1 | SB-T1-XMIN deviation from R-2 (epoch anchored to `pg_current_xact_id()`) | **ACCEPTED.** The deviation is correct; my own G2 premise was wrong. Record corrections are required (X-1 to X-3). |
| 2 | PAY-WH-TENANT-1 conformance with rulings 1–12 | **CONFORMANT WITH REQUIRED CHANGES.** One contract defect must be fixed before the stage closes (PW-1). |
| 3 | ADR 0082 A5 and the ADR 0020 amendment as applied | **ACCEPTED**, with two editorial corrections to A5. |
| 4 | G5 records completeness (ADR, architecture docs, OpenAPI) | **NOT COMPLETE.** R-1 to R-8 are required before G5 can be signed. |

The stage is architecturally sound. No design has to be reopened. The required changes are one small code change (PW-1, with its test) plus record corrections.

---

## 1. SB-T1-XMIN — anchor deviation from ruling R-2

### Verdict: ACCEPTED

**The deviation is correct.** R-2 took its construction from my planning ruling G2, and the implementer is right that G2 was wrong.

- **Why G2 was wrong.** G2 said that `pg_snapshot_xmax(pg_current_snapshot())` "exceeds every xid assigned in the tree including subxids". PostgreSQL defines a snapshot's `xmax` as *one past the highest **completed** transaction id*. It is not the next xid to be assigned. The current transaction and all of its subtransactions are by definition not completed, so any of their xids can be ≥ `xmax`. The "largest candidate not exceeding the reference" arithmetic then subtracts one whole epoch and rebuilds the wrong xid8. That reproduces SB-T1-XMIN for exactly the savepoint case the fix exists for. The implementer's empirical observation (a savepoint row's xmin was greater than `pg_snapshot_xmax`) is what the documented semantics predict.
- **Why anchoring to `pg_current_xact_id()` is sound.**
  - PostgreSQL assigns a parent's xid before any child's, so every xid in the current tree is ≥ the top-level xid.
  - The implemented predicate is: reconstructed ≥ top-level, and `pg_xact_status(...) IS NOT DISTINCT FROM 'in progress'`, with the whole block in `EXCEPTION WHEN OTHERS` → reject.
  - This accepts a row from the current tree whenever the tree has not crossed a 32-bit epoch boundary.
  - It rejects a same-epoch row from an earlier committed transaction by the `< xact_ref` test.
  - A concurrent uncommitted row is invisible, so it takes the `cause.id IS NULL` branch.
  - G1 (NULL-safe comparison), G2 (errors are caught and rejected) and G3 (xmin is immutable because of the 0091 deny trigger) are all present and commented.
- **The body-only constraints hold.**
  - SECURITY INVOKER is kept.
  - Every other branch is byte-identical, pinned by `TestMigration0093_UpChangesOnlyFunctionBody`.
  - The down migration restores 0091 verbatim, pinned by `TestMigration0093_DownRestoresExactPriorFunctionBody`.

### Two claims in the record are not accurate

They must be corrected in the records (X-1). Neither changes the verdict.

- **(a) "A single transaction cannot itself span an epoch wraparound" is false.** A transaction whose top-level xid is assigned just before the 32-bit counter wraps can have a subtransaction assigned just after it.
  - In that case the reconstruction yields `epoch(T) | x_low`, which is less than T, so the row is **rejected**.
  - This fails closed: the composed void gets a 409, no money moves, and a retry in a new transaction succeeds.
  - It can happen at most once per ~4.29 billion xids, and only for a composed void written under a savepoint that straddles the boundary.
  - **Acceptable as a residual.**
- **(b) The ancient-row edge is not purely fail-closed.**
  - Suppose a visible, committed rollback row from the previous epoch has low 32 bits `c` with `t ≤ c < next_xid_low`.
  - Its reconstruction lands on a real xid assigned during the current transaction's lifetime. That xid is not "in the future", so `pg_xact_status` does not raise.
  - If that xid is still in progress (the checker's own subxact or a concurrent transaction), the check **accepts**.
  - Preconditions for this:
    - the row is almost exactly 2^32 xids old (frozen);
    - it is a rollback of the **same bet**;
    - its low bits collide with an xid assigned inside the current transaction's short lifetime.
  - The only thing that can go wrong is the void's `causation_record_id` link. A void with a NULL causation is already permitted, and every money-bearing check (has_void, has_unreversed, ledger type and correlation) runs independently.
  - Every 32→64-bit reconstruction has this ambiguity, and it existed in 0091's equality check and in R-2's construction as well.
  - **Accepted as a documented residual (negligible probability; impact limited to the causation link; no ledger effect).** It must not be described as "fails CLOSED".

### Required (X-1 to X-3)

- **X-1 — correct the records, not the migration.**
  - Append a short correction to ADR 0088's §3.3 follow-up note. It must:
    - state that the architect accepted the deviation (this document);
    - replace the "cannot span an epoch" claim with (a) above;
    - record (b) as an accepted residual.
  - Also remove the line in the follow-up note that says re-review is still required, or mark it satisfied.
  - **Do not edit `0093_*.up.sql` for comments alone.** Migrations are treated as checksum-immutable once applied, and dev/test databases have applied 0093. Its header will stay slightly inaccurate. The ADR note is the authoritative record, and it should say so.
- **X-2 — ADR 0090.** Append an "Implementation record" note. Do not edit the accepted decision text. The note must state that item 2's anchor ("relative to `pg_snapshot_xmax(pg_current_snapshot())`") was replaced by the `pg_current_xact_id()` anchor, with the architect's acceptance and a pointer here. The human approved ADR 0090 with the R-2 wording, so the stage completion report must also list this deviation explicitly for the human's visibility. It is a reversible engineering correction within the approved intent (a fail-closed `pg_xact_status` check, body-only), not a new human decision.
- **X-3 — task registry.** The SB-T1-XMIN row must record "anchor deviation accepted by architect (stage-10.1-architecture-review.md §1)". `ledger-finance` concurrence is still needed: R-2 came from their P2-2 as well.

### Recommended, non-blocking (for `sportsbook`/`ledger-finance`/`qa` to decide)

- **X-R1 — remove residual (a) entirely.** When `x_low < t_low`, reconstruct in epoch+1 instead of the same epoch.
  - For a tree that straddles the boundary, this rebuilds the true subxid, which is in progress, so the row is accepted.
  - For an earlier same-epoch committed row with `c < t`, it produces a future xid. `pg_xact_status` raises, the error is caught, and the row is rejected. Nothing is newly accepted.
  - If adopted, it must land before 0093 is applied to any shared environment, with a probe test covering both epochs. Otherwise defer it.
- **X-R2 — make the guard test drift-proof.** `TestSBT1XMIN_ReconstructionGuard_NullAndErrorCasesRejectClosed` tests a hand-copied probe function, not the trigger body. Add an assertion that `pg_proc.prosrc` of `sportsbook_bet_settlements_validate` contains the exact reconstruction and predicate expression, so the probe cannot silently diverge.

---

## 2. PAY-WH-TENANT-1 — conformance with rulings 1–12

### Verdict: CONFORMANT WITH REQUIRED CHANGES (PW-1 is required before the stage closes)

### Ruling-by-ruling

| Ruling | Status | Notes |
|---|---|---|
| 1 — ADR 0022 §3 closed (candidate 1); ADR 0019 wording | Applied; ADR 0019 concurrence **not recorded** | The ADR 0022 amendment is my paper-15 text verbatim, plus a correct status pointer. The ADR 0019 cell and note are verbatim, but the note says "`ledger-finance` concurrence **pending** final review". Ruling 1 requires the concurrence to be recorded (R-3). |
| 2 — C1 / MOCK only | Conforms | The resolver is `MockWebhookCredentials`. The real resolver is labelled NOT IMPLEMENTED and blocked on the secret-store ADR plus provisioning. `main.go` does not provision secrets. |
| 3 — C2/C3 single resolver, no `MerchantAccountID` | Conforms (see C2 below) | `NewOrchestrator(providers, resolver WebhookCredentialResolver)` takes one interface, and `WebhookCredential` has no `MerchantAccountID`. |
| 4 — merge order | Conforms | PAY-REV-1 (`e9e0ad8`) precedes PAY-WH (`250828b`). In the reversal path the S2 lock and the tombstone both come after (e), `HandleCallback`. |
| 5 — backend changes | Conforms | One shared `mapReceiveCallbackError` with a route kind (401 on the public route, 503 on simulate). The `ErrUnknownProvider` 404 is folded into the 401. Header format is validated in the handler before `WithTenant`. The only remaining 404 is the post-verification `ErrDepositIntentNotFound`. |
| 6 — I1–I3 | Conforms by inspection; one gap in mechanical verification | See the invariants section below. |
| 7 — I4 (`disabled` still accepted) | Conforms | `ProviderAcceptsWebhook` has no `status` predicate and documents why. Pinned by `TestWebhook_DisabledCapability_StillAccepted`. |
| 8 — C5 key-material alert | Conforms | Mapped to `reason=key_material` in the allow-listed `payment_webhook_auth_failed` line. |
| 9 — tests | Mostly conforms; gaps for `qa` | Pre-fix evidence saved (`evidence/pay-wh-tenant-1-cross-tenant-prefix.txt`); T2–T5, T9, T10, T11b and T14 present; an OpenAPI contract test present. **I found no T11a** (the statement-capturing wrapper proving no INSERT, UPDATE or `FOR UPDATE` before verification) and **no T12** (a test of the log-line allow-list and of no `audit_log` row for any 401). `qa` must confirm or add them (PW-5). |
| 10 — C4 conformance-suite placement | Partially conforms | See C4 below (PW-4). |
| 11 — registered items | Conforms | KYC-WH-1, CAS-WH-TENANT-1, PAYWH-BRAND-1, PAYWH-RL-1 and PAYWH-TS-1 are in the registry. |
| 12 — doc updates (C7) | Partially conforms | `payment-orchestration.md` §5/§10 and the code comments are updated. `07-payments-architecture.md` now contradicts itself (R-4). The OpenAPI 400 description is wrong (PW-2). |

### C2 — is `MultiWebhookCredentialResolver` an acceptable reading? Yes, with conditions.

C2 was meant to make the **Orchestrator** depend on one resolver abstraction and not on a provider-keyed map. The reason: the future real resolver is one platform component, keyed by (tenant, provider, key_id) over a handle table and a secret store, and not one resolver per vendor. The implementation meets that intent:

- The Orchestrator holds exactly one `WebhookCredentialResolver`, and a nil resolver fails closed (`ReasonNoResolver`).
- The composite is an implementation *of* that interface. A missing provider key fails closed with `ErrWebhookCredentialUnavailable`, and there is no fallback to another entry.
- The mock resolver independently refuses any provider or key id other than its own, as C2 required.
- The Orchestrator re-checks `cred.TenantID`/`cred.ProviderID` equality (I3), so a mis-wired composite still cannot cross tenants or providers.

**Conditions (PW-6):**

1. **Guard nil entries.** `MultiWebhookCredentialResolver{"x": nil}` panics on `r.Resolve`. Change the lookup to `if !ok || r == nil { return …, ErrWebhookCredentialUnavailable }`.
2. **Doc limit.** The type's doc comment and the ADR 0022 amendment's Status list must say that the composite exists only for MOCK and test wiring. The real resolver is one platform component and must not be built as per-vendor resolvers composed through this type.
3. **Simplify production wiring.** `main.go` registers one provider, so injecting `NewMockWebhookCredentials(mockPaymentsProvider)` directly would be simpler. That change is **optional**. The composite is acceptable as wired.

### Invariants

- **I1 (only config and credential reads before verification): conforms by inspection.**
  - Before the call to `provider.HandleCallback`, the handler performs only `GetTenantBySlug`. That is a platform `tenants` read, done before `WithTenant`.
  - Inside `WithTenant(t.ID)`, `ReceiveCallback` runs only the read-only `ProviderAcceptsWebhook` `EXISTS` and the in-memory mock `Resolve`. There is no lock, write or audit row.
  - Mechanical proof (T11a) appears to be missing (PW-5).
- **I2 (credential lookup scoped to the route tenant): conforms.**
  - `Resolve(ctx, tenantID, providerID, keyID)` receives only the route tenant, and there is no trial across tenants.
  - The MOCK has no DB access. When the real resolver arrives, it must read under the route tenant's GUC and never use `WithoutTenant`. That is already stated in the amendment's Status and §2.2.
- **I3 (one tenant id for RLS, signature input and credential): conforms.**
  - `ReceiveCallback` overwrites `in.TenantID`/`in.ProviderID` with the route values, never the body's.
  - The credential equality check fails closed.
  - `WithTenant(t.ID)` is the RLS context, and the MAC input is `signingInput(req.TenantID, req.ProviderID, keyID, body)`.

### PW-1 (REQUIRED) — pre-verification parse failure breaks contract point 5

Contract point 5 (ADR 0022 §3 amendment) says every pre-verification failure produces one indistinguishable 401. The implementation breaks this:

- `MockProvider.HandleCallback` calls `json.Unmarshal(req.Body, &generic)` for the key-material scan before verifying the HMAC.
- A non-JSON body with well-formed headers therefore returns a plain parse error, not `ErrCallbackSignatureInvalid`.
- `ReceiveCallback` wraps that error, and the handler falls through to **500** `failed to process callback`.
- Result: an unauthenticated caller who sends a garbage body with syntactically valid headers gets **500** when the tenant is active, the provider is configured for webhooks and the key id resolves. In every other case it gets **401**. That is exactly the oracle point 5 exists to prevent: it reveals webhook configuration per tenant.
- In addition, the 500 branch logs `"error", err`. `encoding/json` syntax errors quote the offending byte, so a fragment of the unauthenticated body reaches the logs. ADR 0022 §4.1 forbids logging payload content.
- The T9 matrix does not cover this case.

Required fix (owner `payments`, reviewed by `security`):

- **(a) Adapter contract.** In `HandleCallback`, any failure before signature verification succeeds, including an unparseable body, must be returned as `ErrCallbackSignatureInvalid`, or as `ErrInboundKeyMaterial` when the scan hits. The simplest option in the mock: if the generic unmarshal fails, return `ErrCallbackSignatureInvalid`.
- **(b) ADR 0022 amendment rule.** Record this as an explicit adapter error-contract point in the amendment (see R-2 below). The `types.go` doc comment already cites a "§3 amendment point on real-adapter errors" that does not exist.
- **(c) T9 case.** Add a "non-JSON body, well-formed headers, configured tenant" case to T9, asserting a byte-identical 401 and no body fragment in any log line.

### PW-2 (REQUIRED) — OpenAPI 400 description is wrong

The spec says 400 means "Malformed body (after signature verification), or body exceeds 1 MiB".

- In the code, 400 only covers an unreadable or oversized body. A malformed body after verification returns 500 through the default branch.
- Either document the post-verification malformed body as 500, or map it to 400 through the shared mapper. Mapping it is preferred: a verified PSP should get a 4xx and not retry.
- After PW-1, the pre-verification malformed body must appear only under 401.
- Extend the OpenAPI contract test to match.

### PW-3 (REQUIRED, editorial) — fix dangling references in the `types.go` comment

The `HandleCallback` interface comment in `internal/payments/types.go` cites "the amendment's §9 requirements". The amendment has no §9. Point it at contract point 3 (vendor schemes) and at the new error-contract point from PW-1(b).

### PW-4 (REQUIRED, record) — C4 conformance-suite placement

The tenant-binding case was added to `RunProviderConformanceSuite`, but it `t.Skip`s for any non-mock adapter. As it stands, a future real adapter passes the suite without proving tenant binding.

- This is acceptable for 10.1, because no real adapter exists.
- ADR 0022 §6, or the §3 amendment's Status, must state that the first real adapter must supply a per-tenant signed-fixture hook, and that for non-mock adapters this case becomes a failure, not a skip.
- The other two C4 properties, "no resolver fails closed" and "a rejected callback leaves no rows", are Orchestrator properties, not adapter properties. They correctly live in the Orchestrator integration tests (`TestOrchestrator_NoResolver_FailsClosed` and the no-effect checklist). I accept that placement.

### PW-5 (for `qa`) — confirm or add T11a and T12

These tests are listed in the binding plan (ruling 9) but I could not find them. This is `qa`'s gate call. Architecturally, I1 is not mechanically verified until T11a exists.

---

## 3. ADR 0082 A5 and ADR 0020 amendment, as applied

### ADR 0082 A5 — ACCEPTED, with two editorial corrections (R-5)

- **Matches the planning ruling.**
  - It is inventory and pointer only: the §1.6 row is amended with the original text struck through, §4.5 gets a pointer, and the amendment states no rule or class change and no E-5.
  - The code matches the amended row: `SELECT transaction_type FROM ledger_transactions WHERE id = $1 AND tenant_id = $2 FOR UPDATE` runs after the unlocked intent read and before `GetOrCreateAccounts`, L3 and `Post`.
  - A type or existence mismatch returns `ErrDepositReversalIntegrity` and never reaches the tombstone branch.
  - The post-lock EXISTS re-check is a fresh statement.
  - The deadlock note is correct. Only reversal postings reference a deposit's row through `reverses_transaction_id` (KEY SHARE), and every such path now takes `FOR UPDATE` first, so no new cycle arises.
- **Corrections:**
  1. **Date inconsistency.** The inline §1.6 and §4.5 notes say "2026-09-25", but the A5 heading says "2026-09-26". Use 2026-09-26, the date ADR 0090 was accepted and the fix was implemented.
  2. **Pointer location.** A5 says the full S0–S7 sequence "is recorded in ADR 0090". ADR 0090 only references it ("sequence S0–S7, report §E"). Change the pointer to "ADR 0090 → `docs/plans/stage-10.1-planning-gate-proposal.md` §E".

### ADR 0020 amendment — ACCEPTED as applied

- It states the required doctrine:
  - key idempotency dedupes deliveries, never semantic duplicates;
  - any "at most one X per original" rule requires an L2 lock on the original with a post-lock re-check, plus a type-scoped, tenant-leading DB unique index;
  - check-then-insert without both is forbidden.
- It binds future `withdrawal_reversed`/`bonus_reversal` posters to add both controls in the same change, and it defers REV-UNIQ-CASINO.
- Its consequences match the code. `ledger.Post` routes a violation of `ledger_transactions_one_deposit_reversal` by constraint name to `ErrReversalAlreadyExists`, through `db.UniqueViolationConstraintName` and the extended `IdempotentInsert` return value.
- The index name differs from my planning draft (`ux_…`), but it is used consistently in migration 0092, `ledger.go`, ADR 0020 and Flow 2. No change needed.
- **Note (non-blocking; `ledger-finance` owns it):** migration 0092 does not add the optional `CHECK (transaction_type <> 'deposit_reversal' OR reverses_transaction_id IS NOT NULL)`. That leaves open a NULL-reverses `deposit_reversal` escaping the partial index. My planning ruling made the CHECK optional. If `ledger-finance` declined it, add one line to the ADR 0020 amendment saying so, so the gap is a recorded choice and not an omission.

---

## 4. G5 — records completeness

### Verdict: NOT COMPLETE

| # | Record | State | Required action |
|---|---|---|---|
| R-1 | ADR 0090 | Accepted text still specifies the `pg_snapshot_xmax` anchor. Nothing records the implementation deviations. | Append an "Implementation record (2026-09-26)" note: (i) the SB-T1-XMIN anchor deviation, accepted (X-2); (ii) C2 implemented as a single injected resolver with a MOCK-only composite (§2). Do not edit the accepted decision text. |
| R-2 | ADR 0022 §3 amendment | Applied verbatim; accepted. Missing two points found in this review. | Add: **7. Adapter error contract.** Any failure before signature verification succeeds, including an unparseable body, is reported only as the signature-invalid or key-material sentinel (PW-1b). Also add: the conformance tenant-binding case becomes mandatory for real adapters (PW-4), and the composite resolver is MOCK/test wiring only (PW-6). |
| R-3 | ADR 0019 matrix and note | Wording applied verbatim; concurrence recorded as "pending". | Record `ledger-finance` concurrence (ruling 1) and change "pending final review" to the concurrence reference. |
| R-4 | `docs/architecture/07-payments-architecture.md` | Contradicts itself. The new "Stage 10.1" paragraph says PAY-WH-TENANT-1 "remains open … a separate, later workstream", while the paragraph above it (and ADR 0090) says it was implemented in 10.1. | Rewrite that sentence: PAY-WH-TENANT-1 was implemented in Stage 10.1 with a MOCK resolver only. The real resolver is NOT IMPLEMENTED, and the item stays launch-blocking for any real PSP. |
| R-5 | ADR 0082 A5 | Accepted with editorial issues. | Apply the date and pointer corrections (§3). |
| R-6 | ADR 0088 §3.3 follow-up | Records the deviation but says re-review is still required, and contains the two inaccurate claims. | Apply X-1. |
| R-7 | `docs/api/openapi/platform-api.yaml` (API-DOC-PAYWH) | Present, with a contract test. The 400 semantics are wrong. | Apply PW-2 and update the contract test. |
| R-8 | `docs/governance/task-registry.md` | Stale. The section preamble still says "stage definition ADR 0090 (PROPOSED) … Nothing below is started". The PAY-REV-1 and SB-T1-XMIN rows still list "human approval" as a blocker. | Update the preamble (ADR 0090 ACCEPTED, in progress). Clear the stale blockers. Record the architect acceptance of the SB-T1-XMIN deviation (X-3) and PW-1/PW-2 as open items on PAY-WH-TENANT-1 until they are fixed. |

These records are complete and I accept them as applied:

- ADR 0020 amendment;
- `financial-transaction-flows.md` Flow 2 (its negative-cash OPEN DECISION is untouched);
- `payment-orchestration.md` §5/§10;
- the updated code comments in `deposit_handlers.go` and `orchestrator.go`;
- migration headers 0092/0093 (0093 is subject to the X-1 note);
- `docs/active-stage.md`.

Still required by the stage-gate rule and expected at the gate: the stage completion report, the `docs/progress.md` update, and the post-implementation `security` diff review of PAY-WH-TENANT-1. The report must list the SB-T1-XMIN deviation (X-2) and the open human item KYC-WH-1.

---

## Required changes (consolidated)

**Code (owner `payments`; reviewed by `security` and `code-reviewer`):**
- **PW-1.** In `HandleCallback`, return an unparseable body before verification as `ErrCallbackSignatureInvalid`. Add a T9 case. Make sure no body fragment reaches the logs.
- **PW-6.1.** Add a nil-entry guard to `MultiWebhookCredentialResolver.Resolve`. Add the MOCK/test-only doc limit to the type.
- **PW-2 (if mapped).** Map a malformed body after verification to 400.

**Tests (`qa`):**
- **PW-5.** Confirm or add T11a (statement capture) and T12 (log allow-list, no `audit_log` row on a 401).
- **PW-1c.** Add the T9 malformed-body case.
- **PW-2.** Extend the OpenAPI contract test.

**Records (orchestrator, with the named owners):**
- **R-1 to R-8** as tabled above.
- `ledger-finance` concurrence for R-3 and for the SB-T1-XMIN deviation (X-3).

**Recommended, non-blocking:**
- **X-R1.** Epoch+1 reconstruction when `x_low < t_low` (decide before 0093 reaches any shared environment, or defer).
- **X-R2.** Drift-proof check of the 0093 guard expression against `prosrc`.
- **PW-6.3.** Inject the mock resolver directly in `main.go`.
- **§3 note.** Record the optional `deposit_reversal` NULL-reverses CHECK decision.

**Not for this review:** stage transition, AWS/staging deployment (the human gate), and the KYC-WH-1 scope ruling (human).
