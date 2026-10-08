# Decision brief: H(8) BRAND-SWEEPER-GATE (brand status at cascade-child creation and at sweeper dispatch)

Status: DOCUMENT ONLY. This brief decides nothing, implements nothing and invents no option. It changes no code, no tests, no
migration and no registry. Baseline `86a5439` (branch `gate-r14-c`). Every statement is sourced from the record or from code at
that baseline; every gap is marked "not established by the record". All payment behaviour described is `IMPLEMENTED` against `MOCK`
only (no real PSP exists; nothing here is a statement about a real provider).

Owner register rows: `docs/governance/human-decision-register.md:52` (H(8) BRAND-SWEEPER-GATE, "AWAITING SECURITY/OWNER",
"Real providers", security + owner) and `:58` (HD-PRH2 H(8), OPEN, security).
Registry: `docs/governance/task-registry.md:4122` PAY-H-FOLLOWUPS-1 item (8) ("brand suspended/closed not gated by resolution-only
(security to decide)"); `:4178` HUMAN-DECISIONS-ROUND3 H(8) ("must a suspended/closed BRAND be gated like a non-active tenant in
resolution-only sweeping (security to rule)"); `:4216` H-SEC-5-11-IMPLEMENTED-2026-10-08 item (b).
ADR: `docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md` section 43.1 (item (d) and "sweeper branch is unchanged ...
brand status for the sweeper stays the separate item (8)"), section 43.2 "Cascade-child residual (linked to registry item H(8);
security/owner ruling pending, NOT decided)", and the earlier flag at `0095:7223-7224` ("resolution-only is tenant-scoped only (flagged to security)"),
LF F7 at `0095:7219-7220`. Also ADR 0107 (closed-tenant funds, DESIGN ONLY) and R3 gameplay gate (migration 0118, ADR 0095 section 40.5).

## 1. The question, as the record states it

ADR 0095 section 43.2: "The phase-C cascade-child insert and the sweeper T2 check read the TENANT only, never the brand. A brand-refused HTTP
cascade child is therefore deferred by the HTTP T2 gate and may later be sent by the sweeper after backoff. Behaviour deliberately unchanged here."

The owner/security question (H(8)): should a suspended or closed BRAND be gated like a non-active tenant on the deposit cascade-child path
(creation and/or sweeper dispatch)? Nothing is decided.

## 2. Code facts (file:line, `internal/payments/` unless stated)

### 2.1 The three checks, and that tenant policy and brand policy are not conflated

| Check | Where | What it reads | Tenant | Brand |
|---|---|---|---|---|
| `tenantResolutionOnly` (B8 / H) | `sweeper_resolution_only.go:35-48` | plain `SELECT status FROM tenants WHERE id = $1` in the caller's tx; missing row = resolution-only (fail closed); `status != 'active'` | yes | NO. No brand read at all. No advisory lock. |
| `tenant.RequireActiveForPaymentInitiation` (H-SEC-5/11) | `internal/tenant/payment_initiation_gate.go:35-62` | tenant half: `GameplayStatus` = `pg_advisory_xact_lock_shared(tenant_status_gate_key)` (migration 0118) then a fresh `tenants.status` read (`internal/tenant/gameplay_gate.go:61-77`); brand half: `SELECT status FROM public.brands WHERE id=$1 AND tenant_id=$2 FOR SHARE` | yes | yes |
| R3 gameplay gate (0118, ADR 0095 section 40.5) | `internal/tenant/gameplay_gate.go` | the same advisory-lock-then-status primitive, tenant only | yes | no |

Confirmed:
- The sweeper and resolution-only path is TENANT-ONLY: `tenantResolutionOnly` is called at `drive.go:146` (T2 sweeper branch), `drive.go:702`
  (phase-C cascade child), `sweeper.go:805` (poll-path cascade child), `sweeper_resolution_only.go:92` (`deferIfResolutionOnly`) and `:112`
  (`checkPayoutResolutionOnly`, payout T2/T12). None reads a brand.
- The sweeper's tenant read is a plain status read, NOT the 0118 advisory-lock read. The record says so in effect: "a plain status read in THIS
  claim transaction" (`drive.go` comment at `:137-144`); the task statement's phrase "tenant check uses the 0118 advisory-lock/status read" applies to
  the HTTP gate (`RequireActiveForPaymentInitiation`), not to `tenantResolutionOnly`. Whether the plain read has any race window the advisory-lock
  read does not is not analysed in the record beyond "money-safe; recorded" for ADR 0107 R-CT-3; not established by the record for this path.
- HTTP initiation is both tenant and brand: `RequireActiveForPaymentInitiation` at `deposit_v2.go` phase A (first statement), `withdrawal.RequestWithdrawal`,
  `ClaimForDispatch` (`payout.go:307`, brand = the request's `brand_id`), and the HTTP cascade-child T2 claim (`drive.go:158-175`, brand = `intent.BrandID`).
  Brand is "the brand the player account belongs to", resolved server-side (ADR 0095 43.1).

### 2.2 Where the child comes from, and where each gate sits

1. **Phase C (cascade-child CREATION)**, `applyDepositCallResult` `ErrorClassDefiniteDecline` branch (`drive.go:668-720`): after the decline is final
   (`finalizeDeclined` then `ApplyDecline`), `cascadeEligible(...)` is true, then `tenantResolutionOnly(...)` is read; if non-active it records
   `recordResolutionOnlyBlock("deposit_cascade_child")` and audit `payment.cascade_skipped_resolution_only` and returns with NO child (`:702-713`);
   otherwise `insertCascadeAttemptIfEligible` inserts the child (`:715`). No brand check. The poll-path twin is `sweeper.go:797-814` (same audit
   action, same tenant-only read). The callback/receipt path also creates children (`cascade.go` comment: "the callback/receipt path, phase C's
   synchronous cascade loop, and the sweeper"); I did not audit whether its creation point carries any tenant or brand gate. Not established by the
   record for the receipt path.
2. **HTTP T2 (player-request) claim** `driveCreatedAttempt(..., sweeperDriven=false)` (`drive.go:158-175`): `RequireActiveForPaymentInitiation`
   (tenant + brand) after the intent lock and before `ClaimCreatedForSubmission`. A refusal calls `rescheduleCreatedForResolutionOnly(...)` and sets
   `deferredResolutionOnly`; no claim, no provider call. A non-`ErrNotActiveForPaymentInitiation` error is returned as an error.
3. **Sweeper T2 claim** `driveCreatedAttempt(..., sweeperDriven=true)` (`drive.go:145-157`): `tenantResolutionOnly` only; defers via
   `rescheduleCreatedForResolutionOnly`. Plus the sweeper's pre-read `deferIfResolutionOnly` (`sweeper.go:337`, "only a cheap early skip; it is no
   longer the safeguard", `sweeper_resolution_only.go:19-21`), also tenant-only.

### 2.3 What the code does TODAY to a created child, by tenant status and brand status

`rescheduleCreatedForResolutionOnly` (`sweeper_resolution_only.go:78-86`): `UPDATE payment_attempts SET next_action_at=$2, poll_count=poll_count+1 ...
WHERE id=$1 AND state='created' AND next_action_at IS NOT NULL`. Zero rows is not an error. State is unchanged. The backoff is exponential from
`SweeperDefaultPollBackoffBase` up to `SweeperDefaultPollBackoffCap` (30 min, per `0095:7223`).

| Situation | Child CREATION (phase C) | HTTP T2 claim (`sweeperDriven=false`) | Sweeper T2 claim (`sweeperDriven=true`) | Net effect on the child |
|---|---|---|---|---|
| Tenant active, brand active | child inserted | claimed and sent | claimed and sent | normal |
| **Tenant non-active** (suspended/closed) | NO child inserted (decline stands; audit `payment.cascade_skipped_resolution_only`) | If a child already exists: deferred (the gate sees the tenant) | deferred (reschedule, no claim, no call) | No new money-moving call. An already-existing non-interactive child stays `created` and keeps being rescheduled. Interactive child: see below. |
| **Tenant active, BRAND non-active** | child IS inserted (phase C never reads the brand) | deferred (reschedule, no claim, no call); intent unchanged by the gate | NOT deferred: tenant is active, so the sweeper claims it and CAN call the provider | A brand-refused HTTP child is not terminal; after backoff the sweeper may send it. This is the residual in 0095 43.2. |
| Tenant non-active AND brand non-active | no child | deferred | deferred | same as the tenant row |

Interactive children: a cascade child inherits `Interactive` from its parent (`cascade.go` `Interactive: prev.Interactive`). In `processCreated`
(`sweeper.go:299-327`), an interactive `created` attempt is not dispatched by the sweeper; after the presence window it is expired:
`RejectCreated(..., "expired_before_submission")` then `finalizeDeclined(..., "expired_before_submission")`, with NO status check at all (T3 "makes no provider
call", `sweeper_resolution_only.go:3-9` lists it as allowed for a non-active tenant). So for an interactive child the brand-refused outcome today is
eventual terminal expiry (attempt `rejected`, intent `declined`), not dispatch. For a non-interactive child it is deferral and possible sweeper dispatch.

Other T2 outcomes in the same claim tx that are TERMINAL today (precedent, not a recommendation): RG ineligible, KYC deposit deny, no routable
provider, and an engaged kill switch each do `RejectCreated` + `finalizeDeclined` (`drive.go:98-139`, `:185-230`); i.e. the same T2 transaction already
has both patterns, "defer" (non-active tenant, per the H design "exactly like an engaged kill switch for payouts") and "reject terminally" (the gates
above). Which one the owner wants for a non-active brand is not established by the record.

### 2.4 Reversible versus irreversible (as the code and ADR describe)

| Action | Reversible? | Basis |
|---|---|---|
| Defer (reschedule) a `created` child | Reversible: state unchanged, `poll_count` bumped; reactivation resumes it (after up to the 30 min cap). | `sweeper_resolution_only.go:11-14, 78-86`; `0095:7130, 7223` |
| Skip creating a child (tenant non-active, phase C) | Not reversible for that decline: the decline is already final and "stands"; no child is created later by any code I read for that decline. The intent has no live attempt. | `drive.go:696-713` |
| `RejectCreated` of a child (terminal reject) | Irreversible: terminal attempt state (`rejected`); a later success evidence would be a mismatch/T15, not a resurrection (ADR 0107 section 8 describes this for released attempts). | `drive.go` RejectCreated sites; `cascade.go` |
| Sending a brand-refused child via the sweeper (today) | Irreversible once the provider call is made (in-flight money that resolves); same class as the post-claim exposure recorded at `0095:7985-7986` and section 40, item 4. | ADR 0095 43.2 |

### 2.5 Effect on the deposit intent, and money effects (verified against the record)

- Intent status is a projection of its attempts (`0095` section 5.1): "any other live attempt -> `pending`; none live -> `declined`". `finalizeDeclined`
  (`orchestrator.go:507`) writes `declined` when a decline is applied, BEFORE the phase-C child insert (`drive.go:679-697`). I did not locate code that
  re-projects the intent back to `pending` when the child is inserted; the exact stored intent status while a `created` child exists is therefore not
  established by the record. The ADR text says the live child makes it `pending`.
- Skipped child (tenant non-active): intent stays as finalized by the decline (`declined`), no live attempt (consistent with "none live -> declined").
- Deferred child (existing, tenant or brand non-active): the intent is untouched by the deferral itself (the deferral only writes the attempt row's
  `next_action_at` and `poll_count`). The HTTP gate comment: "the intent stays pending and the sweeper resolves/expires it" (`drive.go:160-163`).
- Non-interactive deferred child of a non-active tenant: ADR 0095 LF F7: "stays `created` forever and the intent never becomes terminal; no funds
  are involved" (`0095:7219-7220`). That statement is the record's own description of a tenant-refused child; the same would hold for a
  deferred brand-refused child if the sweeper also deferred it (it does not today).
- Money effects: none by a refused, skipped, deferred or rejected deposit child. The deposit ledger posting occurs only on success evidence
  (`ApplySuccess` / `deposit.posted`); a decline, reject or defer posts nothing (the wallet summary hold/cash accounts are not touched by deposit declines
  in the code read). The only money exposure is the one H(8) is about: a brand-refused child that the sweeper dispatches and which the provider
  captures, giving a real charge and a posted credit for a suspended or closed brand's player. With the mock provider this is `MOCK` only.
  Withdrawals and holds are not part of the cascade-child path.

## 3. Open questions, with the record's considerations (not decisions)

### Q1. Should brand eligibility be checked at cascade-child CREATION (phase C, `drive.go:697-718`; and the poll-path twin `sweeper.go:797-814`)?

Considerations from the record:
- For: the existing creation gate is explicitly the tenant version of this ("a cascade child is a NEW money-moving attempt, so a non-active tenant gets
  none"); ADR 0095 43.2 names the missing brand read as the residual; H-SEC-5 / H-SEC-11 already require tenant AND brand `active` for NEW payment
  creation on the HTTP paths, and `RequireActiveForPaymentInitiation` is described as "the ONE shared check".
- Against / caution: the decline that precedes the child is already final and committed in the same tx and "stands" (`drive.go:699-701`); skipping the
  child changes the intent's outcome from `pending` to `declined` for that decline (a player-visible outcome change). The record does not say whether a
  brand suspension should end an in-progress cascade or let it finish.
- The H-SEC-5/11 "do not broaden" instruction is why this was left unchanged (43.1/43.2); it is a deliberate non-decision, not an oversight.
- The receipt/callback creation path's gating is not established by the record (section 2.2).

### Q2. Should brand eligibility ALSO be checked when the sweeper CLAIMS/DISPATCHES the child (sweeper T2, `drive.go:145-157`; pre-read `sweeper.go:337`)?

Considerations from the record:
- For: this is the only point at which a brand-refused child reaches the provider today (section 2.3 table). The HTTP T2 gate already checks the brand
  and the record states the sweeper branch is "unchanged ... brand status for the sweeper stays the separate item (8)". A check at T2 is the authoritative
  point (in the claim tx, after the intent lock, before the CAS; the sweeper pre-read is only an early skip).
- Against / caution: the sweeper's tenant read is a plain status read, not the 0118 advisory-lock read; the brand read in the HTTP gate is `FOR SHARE`
  (takes a row lock and requires the runtime role's UPDATE privilege on `brands`, which ADR 0095 43.2 follow-up (b) calls "now load-bearing" and wants a
  grant pin test). Using the HTTP helper in the sweeper would inherit that lock and grant dependency; using a plain read would be a different (weaker)
  primitive. Which is wanted is not established by the record.
- Sweeper scope: resolution-only is defined in the H addendum as TENANT-scoped (`sweeper_resolution_only.go:1-5` and `0095:7224`, "flagged to security");
  extending it to brands changes a security-reviewed definition.
- A brand-level analogue for payouts (`checkPayoutResolutionOnly`) and the poll-path is a further question the record does not raise (payout T2 re-claim
  concerns a `submitted` withdrawal, whose brand is the request's `brand_id`). Not established by the record whether H(8) is meant to cover it.

### Q3. For a child that exists but becomes brand-refused before dispatch: stay deferred (reschedule), be reclassified, or be terminally refused?

What the record and code give (no option beyond these is introduced here):
- **Stay deferred (reschedule).** This is what the code does for a TENANT-refused child (section 2.3), and for a brand-refused child at the HTTP T2 gate. It is
  reversible and resumes on reactivation; the cost recorded is LF F7: a non-interactive child may sit `created` and the intent non-terminal indefinitely,
  with a backoff of up to 30 min after reactivation. It also keeps the child dispatchable by the sweeper if only the brand is non-active and the
  sweeper stays tenant-only (the open residual).
- **Terminally refused (reject).** The code already terminally rejects a created child for RG, KYC deposit deny, no routable provider and an engaged kill
  switch (section 2.3). A terminal reject is irreversible and would finalize the intent `declined` (with `finalizeDeclined`). Whether a brand suspension
  warrants a permanent decline of a possibly legitimate in-progress deposit is not established by the record.
- **Reclassified.** The record names no brand-specific reclassification, reason code or audit action; the only existing precedents are the tenant audit action
  `payment.cascade_skipped_resolution_only` and the metric site label `deposit_dispatch_claim_tx` (reused for HTTP and sweeper deferrals; ADR 0095 43.2 follow-up
  (c)). A new reason code or audit action would be a new design item. Not established by the record.

Today's behaviour summary by cell:
- tenant-refused child: never created in phase C; if it already exists, deferred by both gates; interactive ones expire via T3 anyway.
- brand-refused child: created in phase C; deferred by the HTTP T2 gate; NOT deferred by the sweeper (dispatchable after backoff if non-interactive;
  expired by T3 if interactive).

## 4. Implementation sketch (NOT implemented; only if approved)

"If approved, the minimal change would touch", without writing it:
- `internal/payments/drive.go` phase C (`applyDepositCallResult`, around `:697-718`): add a brand-aware eligibility read next to the existing
  `tenantResolutionOnly` read, and the poll-path twin in `internal/payments/sweeper.go` (around `:797-814`), reusing the audit action or a new one (a
  design choice for security).
- `internal/payments/drive.go` sweeper T2 branch (`:145-157`): add a brand read after the intent lock and before `ClaimCreatedForSubmission`, deferring through the
  existing `rescheduleCreatedForResolutionOnly`; and, if wanted, the pre-read `deferIfResolutionOnly` in `internal/payments/sweeper_resolution_only.go`
  (`:90-104`) as the cheap early skip.
- A brand-aware helper in `internal/payments/sweeper_resolution_only.go` (or reuse of `tenant.RequireActiveForPaymentInitiation`), choosing between the plain
  read and the `FOR SHARE` / 0118 advisory-lock read (a security decision, see Q2).
- Doc updates: the header of `sweeper_resolution_only.go` (definition of resolution-only), ADR 0095 section 43.1/43.2 and `0095:7223-7224`.
- No migration is implied by the record for a plain read; the `FOR SHARE` variant relies on the existing runtime-role UPDATE grant on `brands` (43.2 follow-up (b)).
- The tenant-only paths (`checkPayoutResolutionOnly`, the withdrawal side) are not part of this sketch unless the decision extends H(8) to them.

Required tests if approved (to be written by the implementing workstream; none exist for this):
- Phase C: brand `suspended`/`closed` with tenant `active`: a cascade-eligible decline creates NO child, the audit row is written once, the intent ends
  `declined`; tenant non-active still creates none (regression); brand `active`: child created.
- Sweeper T2 and pre-read: a `created` non-interactive child of a non-active brand is rescheduled (state `created`, `poll_count+1`, no provider call, no claim),
  and after the brand is `active` again it is dispatched; same for tenant non-active (regression).
- Race: a brand status change committed between the sweeper pre-read and the claim tx (use the `testHookAfterDispatchStatusPreRead` seam,
  `sweeper_resolution_only.go:50-54`) is honoured by the in-claim-tx read; with a `FOR SHARE` read, an uncommitted brand UPDATE blocks then refuses.
- Fail closed: missing brand row, brand of another tenant, nil brand id, read error (error returned, not read as active).
- HTTP gate (`sweeperDriven=false`) unchanged; interactive child still expires via T3; kill-switch ordering unchanged.
- Ledger/money: no ledger entry, hold or intent `succeeded` results from any refused path; SUM(DEBITS)=SUM(CREDITS) and projection recompute unchanged.
- A static guard analogous to the existing AST guards (`attempt_tenant_predicate_integration_test.go`, `hsec5_11_integration_test.go`) that every sweeper
  dispatch site carries the brand read; mutation evidence per site (as for `prh2-r13-hsec5-11-mutation-kill.txt`).
- If `FOR SHARE` is used: the grant pin test (43.2 follow-up (b)).
- `ledger-finance` review is required for any path that posts a ledger entry; the sketch above posts none (security review applies).

## 5. Decisions required (security, with the owner)

1. Brand eligibility at cascade-child CREATION (phase C and its poll-path twin): should a suspended or closed brand (tenant active) cause no child to be
   created? Yes or no.
2. Brand eligibility at sweeper CLAIM/DISPATCH (sweeper T2 and its pre-read): should a suspended or closed brand be treated as resolution-only for new
   deposit dispatch? Yes or no. If yes, plain status read or the `FOR SHARE`/0118-style locking read (security's choice)?
3. For a created child that becomes brand-refused before dispatch, which treatment is wanted: keep deferring (reschedule, resumes on reactivation, may leave a
   non-terminal intent per LF F7), terminally reject (irreversible, intent `declined`), or something else the owner names? If "reclassified", which reason
   code and audit action?
4. Is the existing behaviour for an INTERACTIVE brand-refused child (expiry through T3 after the presence window, no brand read) acceptable as is?
5. Does H(8) also cover the payout T2 re-claim/T12 resend and the receipt/callback cascade creation path (not established by the record whether they are in scope),
   or only the deposit cascade child?
6. Is the sweeper's "resolution-only" definition (tenant-scoped, ADR 0095 H addendum) to be amended to include brand status, with the ADR 0095 text updated accordingly?

## 6. What is NOT being decided here

- No code, test, migration or ADR amendment is made by this brief; the sketch is not approved work.
- No change to H-SEC-5/H-SEC-11 (HTTP gates stay as merged `3643cb0`), to the kill switch, or to the tenant resolution-only behaviour.
- Nothing about withdrawals, holds, release/cancel (that is HSEC-APPROVED-HOLD-RELEASE-1, `hsec-approved-hold-release-1-decision-brief.md`) or ADR 0107
  closed-tenant funds (DESIGN ONLY; note ADR 0107 section 6.3 hooks into the same sweeper T2 re-claim position for payouts and does not touch brand).
- No real-provider readiness claim; no PSP or commercial decision.
- The ADR 0095 43.2 LOW follow-ups ((a) refused-submit audit row, (b) brand `FOR SHARE` contention/grant pin, (c) metric label, (d) warn log tenant id,
  (e) per-brand advisory lock) are not decided here, except that (b) and (e) are noted where they bear on Q2.

## 7. Facts not establishable from the record

- Whether the receipt/callback path's cascade creation carries any tenant or brand gate.
- The stored `deposit_intents.status` while a phase-C `created` child exists (the code sets `declined` before the child insert and I found no re-projection to
  `pending`; the ADR says a live attempt gives `pending`).
- Whether the plain tenant read in the sweeper has a race window that the 0118 advisory-lock read closes.
- Which brand-refusal treatment (defer, reject, reclassify) the owner or security wants; no reason code or audit action for a brand exists.
- How many cascade children, interactive or not, a brand suspension would affect in production; and any player communication duty.
- Whether a brand suspended/closed status can be set through any code path (I found no non-test Go code that updates `brands.status`).
