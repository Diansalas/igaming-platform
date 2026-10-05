# PSP prerequisites: WIRING-1, STANDING-1, POLL-REF-CLEAR-1 (ledger-finance, read-only analysis)

HEAD analysed: `95b17c5` (main). Read-only: no edits, tests, builds or DB commands were run.
All line numbers below are at that HEAD.

Summary of verdicts
- PAY-K3-STATEMENT-SOURCE-WIRING-1: OPEN, NOT IMPLEMENTED. Small Go-only change. No migration.
- PAY-RECON-PARKED-CAPTURE-STANDING-1: substantially IMPLEMENTED by K3 (S1/S2/S3/S5, the flip, the docs).
  Formal closure needs: one missing test for the exact TestD2_14 shape (the registry names it), plus one
  ledger-finance ruling on a per-run suppression. No migration.
- PAY-RECON-POLL-REF-CLEAR-1: **PARTIAL**. The registered acceptance text is met against MOCK. The
  "fail closed on ambiguity" invariant is not demonstrably met when Y is a reference that another
  attempt or ledger posting holds. Minimal fix is Go-only, no migration.
- Migration 0118: not needed by any of the three. I recommend leaving it unused for this workstream.

---------------------------------------------------------------------------------------------------

## 1. PAY-K3-STATEMENT-SOURCE-WIRING-1

### 1.1 Acceptance text as registered (task-registry.md:4138)
> OPEN; REAL-MONEY PRECONDITION (not optional). The reconciliation statement source registry is not
> wired in `main.go`, so `m2_declare_not_paid` is refused at submission and execution (fails closed;
> test `noStatementSource`). Before real money: register sources from the same list the scheduler uses
> and also refuse `m2_declare_paid` when no source is scheduled. The MOCK stream is scheduled so (c),
> (c2), S1-S4 run for MOCK.

Related text: ADR 0101 §27.1 row "Registration of a payment statement source ... NOT IMPLEMENTED";
§27.6 "populate `DefaultStatementSources` from the SAME list given to the scheduler, and refuse
`m2_declare_paid` as well when no source is scheduled"; §26.2 O-4: "decides from the in-process source
registry, never from `payment_statement_imports`"; ADR 0095 §39.4 bullet 2.

### 1.2 Implemented today
- Registry type and the global: `internal/payments/manual_resolution.go:161-198`
  (`StatementSourceRegistry.Register/Registered`, `DefaultStatementSources`, nil means fail closed).
- Not-paid refusal at submission: `manual_resolution.go:474-480`. Note it is skipped when
  `GetAttemptByID` errors or `ProviderID` is nil (`err == nil && ...`). Execution still refuses
  (`:875`), so this is not a money hole, but the submission check is fail-open in shape.
- Not-paid refusal at execution: `manual_resolution.go:887-894` (`resolutionRefusedNoSource`,
  `:84`; `refusal_code` is free text 1..64, migration 0115:197, so a new use needs no migration).
- The routes use the global: `internal/httpserver/payment_force_resolution_routes.go:45`.
- The scheduler gets exactly one payment source: `cmd/platform-api/main.go:427`
  (`providers.PaymentsStmt`), built at `cmd/platform-api/registrations.go:204` (MOCK, synthetic) and
  registered with the production guard at `registrations.go:379` (`RefuseSyntheticInProduction`,
  `main.go:71`). So a production-environment binary already refuses to start with the MOCK source.
- Per-source loop: `internal/reconciliation/scheduler.go:365-370`. Tenants swept: ACTIVE only
  (`scheduler.go:177-206`, `status = 'active'`).
- Test: `internal/payments/k3_m2_integration_test.go:459` `TestK3_NotPaidRefusedWithoutStatementSource`
  (world option `noStatementSource`, `k3_world_integration_test.go:68-70, 185-186`).

### 1.3 Missing
1. Nothing calls `DefaultStatementSources.Register` (grep: no caller outside tests). Every
   `m2_declare_not_paid` in the binary is refused, including against the MOCK provider.
2. `m2_declare_paid` is never refused for a missing source (neither `requestInTx` nor
   `executionRefusal`). Its standing kind `pay_declared_paid_unconfirmed` only exists if the
   provider's stream runs.
3. Nothing ties "registered" to "passed to the scheduler" (two separate code paths).
4. Not in the registry text, but relevant to "scheduled": the stream only sweeps ACTIVE tenants
   (`scheduler.go:206`). ADR 0101 R-5 admits M2 on a CLOSED tenant through `platform_acting`. For such
   a tenant, a "registered" provider still has no scheduled run, so (c)/(d) never arise. See H-W1.

### 1.4 Minimal implementation (no migration)
- `cmd/platform-api` (new small helper, e.g. in `registrations.go`):
  `func (b providerBundle) paymentStatementSources() []statement.PaymentStatementSource` returns
  `[]{b.PaymentsStmt}`. This is the ONE list.
- `main.go`: build `paySources := providers.paymentStatementSources()` once. Call
  `registerPaymentStatementSources(payments.DefaultStatementSources, paySources)` BEFORE the HTTP
  server starts serving (right after the `refuseSyntheticInProduction` check at `:71` is the natural
  place). Pass `paySources...` to `RunSchedulerLoop` at `:427`. One variable feeds both, so they cannot
  drift.
- `registerPaymentStatementSources(reg, srcs)`: for each src, refuse a nil src or an empty/invalid
  `ProviderID()` (return an error, so startup fails), then `reg.Register(src.ProviderID())`.
- Registry hardening (Go only, `manual_resolution.go`): make the submission check fail closed for both
  M2 kinds: if `GetAttemptByID` errors, or `ProviderID` is nil, or the provider is not registered,
  refuse with `ErrResolutionNoStatementSource` (or the existing precondition error for the error
  case). Apply this to `ResolutionM2DeclarePaid` as well as `ResolutionM2DeclareNotPaid`.
- `executionRefusal` (`:864-896`): move the `!s.sources.Registered(*att.ProviderID)` check so it
  applies to both M2 kinds and returns `resolutionRefusedNoSource`. The existing order is fine (the
  provider-id nil check at `:875` comes first).
- Optional, preferred over the global: add `StatementSources *payments.StatementSourceRegistry` to
  `httpserver.Deps` and pass it at `payment_force_resolution_routes.go:45`; a nil Deps field refuses
  every M2 (fail closed). This removes the global from tests. It is not required by the acceptance text.

### 1.5 "Keep not-paid fail-closed when there is no non-MOCK source": how, without a real PSP
The registry is keyed by provider id. The MOCK source's `ProviderID()` is the MockProvider's id
(`mock-payments`). Registering it unlocks M2 only for attempts whose `provider_id` is `mock-payments`.
Every attempt of any other provider (any real PSP) stays refused, because no source with that id is
scheduled. So "register from the scheduler's list" is fail-closed for real providers by construction.

The real-money refusal can be a configuration/readiness gate, with no code fork:
- (a) Already in place: `RefuseSyntheticInProduction` (`main.go:71`, `registrations.go:379`) refuses
  to boot a production-environment binary that wires the MOCK payments statement source. In
  production, the registry can therefore only ever contain non-synthetic sources.
- (b) Add one startup coverage check in the same `providerkind` style (pure function, unit-testable
  with a fake ProductionEligible adapter):
  - Every non-synthetic payments adapter wired into the binary must have a non-synthetic statement
    source with the same provider id in the scheduler list. Otherwise refuse startup in production
    (or at least do not register the id).
  - Refuse a synthetic source whose provider id equals a non-synthetic adapter's id, so a MOCK
    statement can never vouch for a real PSP.

  Today this check is vacuous (there is no real adapter), but it is the gate the first real PSP has to
  pass.

Interpretation question (H-W2): if the owner meant "refuse not-paid even for the MOCK provider until
some non-MOCK source exists", then only non-synthetic sources should be registered. In that case M2
(both kinds, once paid is also gated) is unusable in the dev binary and is exercised only by
service-level tests with their own registry. That contradicts the registry text ("register sources
from the same list the scheduler uses"), so I recommend option A (register the MOCK for the MOCK
provider id only) plus gate (b).

### 1.6 Tests (invariant categories) and mutation targets
- Normal: binary wiring unit test. After `registerPaymentStatementSources`, `Registered(id)` is true
  for exactly the ids in `paymentStatementSources()` and false for any other id. A static pin
  (AST/regexp over `main.go`, as other static pins in the repo do) checks that the slice given to
  `RunSchedulerLoop` is the same identifier that is registered.
- Normal (integration, `internal/payments`): with the provider registered, `m2_declare_paid` and
  `m2_declare_not_paid` both execute (existing C-tests). After the run, the standing kinds appear in
  a `payment_statement` run for that provider.
- Refusal: `TestK3_PaidRefusedWithoutStatementSource` (new). It mirrors `:459` for declare paid, at
  submission AND at execution. For execution, register at submission and then use a world with the
  provider unregistered. Expect `refused_at_execution` with `no_statement_source`, no ledger
  transaction, `SUM(debits)=SUM(credits)`, the withdrawal still `submitted`, and the hold intact.
- Refusal: unregistered real-like provider id while the MOCK id is registered. Not-paid and paid are
  both refused (proves per-provider keying).
- Partial failure / rollback: a refusal at execution writes the `refused_at_execution` state plus
  audit and nothing else (no `withdrawal_completed`/`failed`, no `ledger_entries`).
- Duplicate/idempotency: unchanged (the reserved id plus ledger uniqueness). Re-run the C-12 class
  under `-race`.
- Concurrency: registry `Register`/`Registered` under `-race` (it already uses RWMutex). Concurrent
  final approvals with an unregistered provider: both refused, no posting.
- Authorization / actor-subject / tenant isolation: unchanged paths. Re-run T-12/T-14 (routes),
  T-4/T-8 (recount, closed tenant).
- Audit: the `payment.manual_resolution_denied` / refused audit rows carry the closed code.
- Startup gate (b): unit tests for (i) a production env with a ProductionEligible fake adapter and no
  real source, which refuses; (ii) a synthetic source with a real adapter's id, which refuses; (iii)
  dev with MOCK and MOCK, which boots.
- Mutants:
  - drop the `Register` call;
  - register a different slice from the one scheduled;
  - remove the paid-kind check at submission, then at execution;
  - revert the submission check to `err == nil && ...` (fail-open);
  - make `Registered` return true;
  - drop the synthetic-pairing refusal;
  - drop the provider-id keying (a global "any source registered" bool).

### 1.7 Human / orchestrator decisions (not invented here)
- H-W1 (architect/owner; touches ADR 0107 / HD-CTF): ACTIVE-only sweeping versus closed-tenant M2.
  - Option 1: refuse M2 when the target tenant is not in the swept set. This narrows ADR 0101 R-5.
  - Option 2: sweep `payment_statement` for non-active tenants that have executed M2 or disputed
    deposit attempts. This changes the sweep scope.

  Both are fail-closed in direction, but both change an approved design, so they need a decision.
  Until then, record it as a residual on WIRING-1.
- H-W2 (owner clarification): option A versus option B in §1.5.
- Sequencing (orchestrator): wiring makes `m2_declare_not_paid` executable in the dev binary. The
  clearing of (d)/(c2) trusts executed K2 `compensating_entry` rows, and those are forgeable until the
  TEMP/search_path fix lands (TRIGGER-SEARCH-PATH-1; migration 0116 reserved). Land 0116 before or
  with WIRING-1.

---------------------------------------------------------------------------------------------------

## 2. PAY-RECON-PARKED-CAPTURE-STANDING-1

### 2.1 Acceptance text as registered (task-registry.md:4098)
> OPEN; Medium. Also carries (LF D2 confirmation, binding) the requirement that its persisted line
> evidence lets a persisted reversal line naming a bound reference X clear a bound standing finding
> across runs (the TestD2_14 shape: a reference-holding poll F-C4 conflict park cannot be cleared by a
> tombstone because X is already the `withdrawal_completed` key, so today only a reversal line in the
> same run clears it and the finding returns on the next run: noisy, never silent). Also carries LF D2
> T15i (add a `success_for_never_sent_attempt` integration test as soon as a fixture is possible) and
> ships with PAY-RECON-POLL-REF-CLEAR-1 under one schema change. HARD PREREQUISITE to enabling any real
> PSP adapter or statement source, and to the first real-money tenant (QA D2 condition). Keep the ADR
> 0095 §35.4 "NOT IMPLEMENTED" wording, the disclosed-limits comment in `payment_statement.go`, and the
> gap-asserting test (`d2NoCU(..."an unbound park with no line this run")`), which must be flipped
> when this is implemented.
> | Unbound parks (`provider_reference_conflict`, `invalid_provider_reference:*`) are flagged as
> `pay_captured_unposted` only in a run whose statement carries the merchant-resolved `succeeded` line.
> Once that statement ages out, only the `payment.attempt_disputed` audit remains. Standing coverage
> needs persisted `payment_statement_lines` evidence (the same approach as LF ruling 5(c)(d)). Also:
> update `docs/architecture/reconciliation-model.md` with the widened `pay_captured_unposted` kind
> (ADR 0095 §35).

Binding design: ADR 0101 §9.2 S1-S6 and the eligible-evidence rule (D-4/RC-3); ADR 0095 §35.4 gate
item 1.

### 2.2 What "standing" means (from the text)
"Standing" is not a persisted, lifecycle-managed finding row. It is a finding **re-derived and
re-emitted on every `payment_statement` run, unwindowed** (not gated by coverage or age), from
durable evidence: the attempt row (`state='disputed'` plus `terminal_reason`) and append-only
`payment_statement_lines` across ALL imports of (tenant, provider). Each run writes a new
`reconciliation_mismatches` row. It disappears from a run only when a clearing signal exists:
- an eligible `deposit_reversal` line naming the reference, in any persisted import or this run;
- or a `tombstone` ledger row on (provider_id, that reference).

Nothing else clears it. M1 only acknowledges it, and `investigation_status` does not clear it
(ADR 0101 S2; `payment_statement.go:1195-1202`; ADR 0095 §35.4 bullet 1).

### 2.3 Implemented today
- S1 (unbound standing, persisted, every run): `internal/reconciliation/payment_statement_k3.go:401-430`
  (`checkStandingUnbound`), called at `payment_statement.go:653`. Unbound is decided by
  `captureClass`/`unboundPark` (`payment_statement.go:248-269`, D2F-1 runtime rule).
- Persisted lookup, no LIMIT, explicit tenant predicate, dedupe across overlapping imports:
  `payment_statement_k3.go:197-267`.
- S2/S3 clearing from persisted eligible reversal lines plus tombstone: `payment_statement_k3.go:269-305`
  (the `need` set includes bound X), `:376-385` (`clearedRef`), merged into `reversalOriginals` at
  `payment_statement.go:938-947`. Bound standing path: `payment_statement.go:1228-1230` via
  `capturedUnposted` `:1181-1193`.
- Eligibility D-4/RC-3: `payment_statement_k3.go:121-142`.
- S6 one finding per (attempt, ref) per run: `payment_statement_k3.go:387-399`.
- Flip done: `prh2_d2_parked_capture_integration_test.go:568-593`, `prh2_d2_postd1_integration_test.go:291-297`
  (the old string "an unbound park with no line this run" no longer exists).
- T15i: `prh2_k3_recon_integration_test.go:125-160` (`TestK3_C38_T15i_...`).
- S1/S2: `TestK3_C34_*` (`:34`, `:50`). S3: `TestK3_C35_*` (`:62`), using a `sync_amount_mismatch` bound
  park. C-43/C-44 `:164`. Tenant isolation C-14d `:200`. Holder detail C-34b
  (`prh2_k3_c34b_integration_test.go:15`). MOCK eligibility C-45/45b/45c (`internal/payments/k3_recon_integration_test.go:358-438`).
- Docs: ADR 0095 §35.4 rewritten (lines 6560-6611); `docs/architecture/reconciliation-model.md:287-295`
  K3 amendment (S1-S4, N1). The "disclosed limits" comment was updated (`payment_statement.go:125-130`).
- One schema change shared with POLL-REF-CLEAR-1: migration 0115 §9 and §10 (indexes).

### 2.4 Missing / residual
- M-S1 (test gap; named in the acceptance text). The exact TestD2_14 shape (poll F-C4
  `provider_reference_conflict` park holding X, where X is a `withdrawal_completed` key) is only tested
  for a SAME-RUN reversal clear: `prh2_d2_postd1_integration_test.go:221-228`. The cross-run property
  is tested only on a `sync_amount_mismatch` park (C-35). The code path is shared (`capturedUnposted`
  → `clearedRef`), but the registry names this shape. Add a subtest: run 1 with the payout line
  raises the finding; run 2 has a reversal line naming X; runs 3..N use `d2PastSrc()` with no CU; also
  assert the payout side stays clean and no money moved.
- M-S2 (per-run suppression; needs an LF ruling, reporting-only, no money). A BOUND park matched in
  THIS run by a `pending` or `declined` deposit line raises nothing in that run.
  - `matchPayment` falls into the `a.state == "disputed"` no-op (`payment_statement.go:1110`).
  - `checkUnmatchedAttempts` skips matched attempts (`:1207`).
  - So a PSP that keeps listing X as `declined`/`pending` in every window suppresses the bound
    standing finding for as long as it does. This contradicts "nothing else clears it"
    (`:1195-1199`, S2).
  - It is pinned as intended by the D2 control `non_succeeded_line_is_not_flagged_in_run`
    (`prh2_d2_parked_capture_integration_test.go:434-445`).
  - Proposed: for `a.boundCapture()` with line status pending/declined and `m.capturedUnposted(a)`,
    emit the standing finding with the line annotated. Keep `reversed` as the R1 in-run clear: it is a
    prior ruling, and it re-raises next run, so it is noisy, never silent. Flip the pending/declined
    half of that control.
  - Unbound parks are not affected: S1 reads persisted succeeded lines regardless of this run.
- Residuals to disclose (outside the acceptance text; do not block closure, but each is a way a
  standing finding can stop being emitted):
  - R-S1: standing findings are emitted only inside a successful `payment_statement` run for that
    (tenant, provider). A fetch failure that tick emits `run_failed` P1 instead, which is loud. But
    removing a provider's source from the scheduler list makes its standing findings vanish silently.
    The WIRING-1 startup gate (§1.5(b)) can cover this if extended: a provider with any disputed
    deposit attempt must stay scheduled. That needs a DB read at startup or a runbook rule, which is
    an orchestrator choice.
  - R-S2: non-active tenants are not swept (`scheduler.go:206`). Their standing findings stop. Same
    decision as H-W1.
  - R-S3: S1 depends on the PSP statement carrying `merchant_reference`. That is PROVIDER DEPENDENT and
    should be added to the real-PSP contract criteria (ADR 0095 §34.4 PAY-PSP-CONTRACT-INVDEP1).
  - R-S4: a `deposit` line with status `reversed` clears in-run only; the next run re-raises
    (noisy).

### 2.5 Parked capture whose original attempt "later succeeds / is cancelled"
- A deposit attempt in `disputed` is terminal. Late evidence is record-only, with no state change:
  - `internal/payments/receipt.go:758-759` (callback: no-op);
  - `internal/payments/sweeper.go:575-579` (poll: recorded only).

  M1 leaves it `disputed` (ADR 0095 §39.1). M2 is payout-only. So the parked attempt itself can never
  later succeed or be cancelled.
- The matcher keys only on the attempt row (`state='disputed'`, `terminal_reason`,
  `provider_reference`): `payment_statement.go:261-269`, `:1089-1110`, `:1228`, and
  `payment_statement_k3.go:409-430`. It never reads the deposit intent's state or sibling attempts.
  - If a SIBLING attempt of the same intent succeeds and posts (before or after the park), the
    park's finding keeps standing. This is the intended financial rule: the second capture stays
    disputed and unposted, with no automatic posting, receivable or clawback. One posting per intent
    is enforced by the INV-DEP-1 choke point and backstopped by `checkPlatformDuplicates`
    (`:1314-1339`).
  - If the intent is cancelled or expires, the finding still stands. It clears only on a reversal
    line or a tombstone.
  - If the sibling's own posting is later reversed (deposit_reversal under the sibling's
    reference), the park does not clear (different reference). Correct: there is no automatic
    reallocation (LEDGER-SUSPENSE-B-1).
  - The only cross-attempt clearing is the accepted N1 residual: the evidencing line's reference is
    held by another attempt (the holder), and a reversal on it clears the park. Disclosed in ADR 0095
    §35.4, with the holder recorded in the detail (C-34b).
- Test to add (pins this): an unbound park plus a sibling succeeded and posted attempt on the same
  intent, then the intent cancelled or expired. The finding stands on every run, there is exactly one
  posting for the intent, and SUM(debits)=SUM(credits).

### 2.6 Minimal implementation to close formally (no migration)
1. Add the M-S1 subtest to `prh2_d2_postd1_integration_test.go` (TestD2_14 `reversal_line_naming_X_persisted_clears_in_later_runs`).
2. LF ruling on M-S2. If accepted: a small change in `matchPayment`'s switch
   (`payment_statement.go:1089-1110`) plus flipping the pending/declined arms of the D2 control. If
   not accepted, disclose M-S2 in ADR 0095 §35.4 as a residual.
3. Add the §2.5 sibling/cancel pin test.
4. Disclose R-S1..R-S4 in ADR 0095 §35.4 and `reconciliation-model.md`.
5. Then the orchestrator closes STANDING-1 as `IMPLEMENTED (MOCK)`. Real-PSP behaviour stays
   `PROVIDER DEPENDENT`. Gate item 2 (ALERT-DELIVERY-1, migration 0117) still keeps the ADR 0095 §35.4
   gate closed.

### 2.7 Invariant tests required (existing ✓ / new +)
- Normal ✓ C-34, C-35, C-38. Duplicate ✓ C-43/C-44 (overlapping imports, one finding per exposure).
- Retry/idempotent re-run: ✓ C-34 runs 2..4. + rerun the same import (`import_reused`), which gives
  the same finding set.
- Concurrency: ✓ advisory lock and snapshot (existing `payMatchAfterAttemptsHook` snapshot test).
  + a park committed between import and match: the snapshot either sees it or not, and is never
  partial.
- Partial failure: + fetch failure gives `run_failed` P1 and no partial mismatch rows; the next good
  run re-emits.
- Rollback: + a park transaction that rolls back leaves no finding (no attempt row in `disputed`).
- Reconciliation: ✓ `d2AssertNoMoney` / `d2AssertBalanced` / `RunLedgerVsProjection` 0.
- Authorization / tenant isolation: ✓ C-14d. Audit: ✓ C-19+ (stored text).
- + M-S1 and the §2.5 test; + M-S2 if ruled.
- Mutants:
  - drop `need[X]` from the persisted reversal query (kills S3 cross-run on the F-C4 shape);
  - `checkStandingUnbound` reads only this run's lines;
  - drop `l.eligible` / RC-3;
  - drop the dedupe;
  - drop the `tenant_id` predicate (redundant per the K3 G05 classification);
  - M-S2 revert (if implemented).

### 2.8 Human decisions
None needed for closure. H-W1 (non-active tenants) covers R-S2. M-S2 is a ledger-finance ruling
(reporting-only, fail-closed direction), and the orchestrator records it. It does not change money
semantics.

---------------------------------------------------------------------------------------------------

## 3. PAY-RECON-POLL-REF-CLEAR-1

### 3.1 Acceptance text as registered (task-registry.md:4097)
> OPEN; Medium. BINDING before the first real PSP or the first non-MOCK statement source, whichever
> comes first; ships together with PAY-RECON-PARKED-CAPTURE-STANDING-1 under ONE allocated schema
> change that ledger-finance signs off (LF D2 final review, B3). Until then ADR 0095 §35 must state
> that a standing `poll_reference_mismatch` finding whose reversal came under Y needs manual
> verification, and that M1 only acknowledges it. (... LF ruling: reconciliation must not parse audit
> JSON as money evidence)
> | For `poll_reference_mismatch` parks, D1 records the provider's returned reference Y only inside
> the park's audit JSON (...). D2's standing finding clears only on a reversal line or tombstone on
> the BOUND reference X, so a PSP reversal under Y leaves the finding standing (noisy, never silent).
> Needed: persist Y as structured evidence (a column or an evidence row, which needs a migration and
> therefore a number), then let D2's clearing rule accept X or Y for that reason. Not implemented in
> D2: no schema change was allocated.

Status row: task-registry.md:4142 (`PAY-RECON-STATUS-AFTER-K3`): remains OPEN until LF confirms
closure against this text.

### 3.2 Implemented today (evidence)
- Typed table `payment_attempt_reference_evidence` (migration `0115...up.sql:877-948`):
  - closed `evidence_kind`;
  - UNIQUE (tenant, attempt, kind), plain INSERT, a duplicate raises;
  - BEFORE INSERT guard: live deposit, X non-null and different from Y, same provider, `FOR SHARE`;
  - DEFERRABLE check bound to a `poll_reference_mismatch` park in the same transaction;
  - immutable (UPDATE/DELETE/TRUNCATE denied);
  - reserved-prefix CHECK `:986-988`;
  - RLS ENABLE+FORCE with system-shape INSERT/SELECT policies only (`:1460-1475`);
  - grants SELECT, INSERT (`:1770-1771`).
- Writer: `internal/payments/poll_evidence.go:116-129`. Only for a live attempt, only when
  `ValidatePaymentReference(Y)` passes, inserted before `parkDepositAttempt` in the same transaction
  (`insertPollReferenceEvidence` `:184-192`). An invalid Y stays audit-only (length plus hash prefix).
- Reader, typed only and never audit JSON: `payment_statement_k3.go:144-162` (tenant plus provider
  predicate). Y is added to the persisted reversal lookup at `:213-215`, then `:269-305`.
- Clearing "X or Y": `payment_statement.go:1181-1193` (`capturedUnposted`). It is used by both the
  in-run bound case (`:1089`) and the standing bound case (`:1228`). Eligible imports only for
  persisted reversal lines; a tombstone is read from the ledger (`clearedRef`, `k3.go:380-385`).
- Interim wording: ADR 0095 §35.4 bullet at line 6611: "IMPLEMENTED against MOCK ... Without a Y row
  ... only X clears, and the operator rule of manual PSP verification still applies". M1 only
  acknowledges it (§39.3, §39.1).
- Tests:
  - `TestK3_C36_S4_PollReferenceMismatchClearsOnTheReturnedReference`
    (`prh2_k3_recon_integration_test.go:83-104`): real sweeper path, typed row asserted; an unrelated
    reversal does not clear; a reversal on Y clears and stays cleared across runs; no money moved.
  - `TestK3_C36_S4_YComesOnlyFromTheTypedTable_NeverFromAuditJSON` (`:109-123`).
  - Security T-7/T-17 (deferred binding, policies).
  - D2 `poll_reference_mismatch_line_carries_echo` (`prh2_d2_parked_capture_integration_test.go:425-433`).

### 3.3 Missing behaviour (why not CLOSE)
The intended invariant is: a PSP reversal of the capture the PSP attributed to this attempt clears the
finding; nothing else does; fail closed on ambiguity; references bound and validated. The
implementation accepts ANY eligible reversal line or tombstone on Y, with no check that Y is
attributable to this attempt:

- G-Y1 (ambiguity, must fix). Y is validated only as "a valid reference, different from X" (guard
  `0115:905-913`). Nothing checks whether Y is held by ANOTHER attempt (`payment_attempts.provider_reference`)
  or is a non-tombstone ledger key (`deposit` / `withdrawal_completed`) of another posting.
  - poll_evidence step 2 (`:116-129`) parks before the F-C4 foreign-binding check (`:132-142`), and
    that check is applied only to X.
  - So if the PSP's poll for A echoes B's reference Y, then a later `deposit_reversal` line naming Y
    (B's own refund, which B reconciles independently) clears A's finding on X.
  - If X was really captured (for example the statement also carries a succeeded line on X), that
    capture drops out silently. That is exactly the silent-drop class STANDING-1 exists to prevent.
  - Unlike N1 (one reference, which the PSP itself reversed), here two different PSP references are
    conflated.
  - The precedent for the fix is LF F-1 / `resolvesTo` (`k3.go:349-364`): borrowed attribution may
    raise but may not clear.
- G-Y2 (ledger-finance ruling, recommended). When eligible persisted succeeded `deposit` lines exist
  for BOTH X and Y, they are two evidenced captures. A reversal of only one should not clear. The
  B3 rule "X or Y" assumed X and Y name one capture.
- G-Y3 (test gap). No test for a TOMBSTONE on Y clearing. The code path is `clearedRef` (shared), but
  the tombstone-on-Y arm is untested. A tombstone on Y is realistic: a reversal callback naming Y
  with no posted original writes a tombstone on Y.

### 3.4 Minimal fix (Go only, no migration)
In `payment_statement.go` `capturedUnposted` (`:1189`), gate the Y branch:
```go
if y, ok := m.k3.yRef[a.id]; ok && y != "" && m.yAttributable(a, y) && !m.capturedUnpostedRef(y) {
    return false
}
```
`yAttributable(a, y)` is true only if:
- `m.byRef["deposit\x00"+y]` and `m.byRef["payout\x00"+y]` are each nil or `a` itself;
- `m.ledgerByRef["deposit\x00"+y]`, `["withdrawal_completed\x00"+y]` and `["deposit_reversal\x00"+y]` are nil;
- (G-Y2, if ruled) NOT (an eligible persisted succeeded deposit line names X AND one names Y while
  only one of X or Y is cleared). Implement this as "every evidenced reference in {X, Y} must be
  cleared".

When Y is not attributable, only X clears, as for a park without a Y row. The finding detail should
name Y and its holder ("Y held by attempt=… / ledger tx=…; not used for clearing") so that the operator
manual-verification rule applies. All inputs are already loaded in the snapshot (`loadPlatform`,
`loadK3Evidence`), so no new query is needed.

### 3.5 Invariant tests required and mutation targets
- Normal ✓ C-36 (reversal on Y). + tombstone on Y clears (G-Y3).
- + G-Y1a: Y = B's bound reference, with B succeeded and posted. A reversal line naming Y does not
  clear A. B gets its own expected `deposit_reversal` handling. X reversal still clears A.
- + G-Y1b: Y = a ledger `withdrawal_completed` key: not attributable.
- + G-Y2 (if ruled): succeeded lines on X and on Y, a reversal on Y only. Still standing. Reversals on
  both clear it.
- Duplicate: ✓ the UNIQUE constraint plus plain INSERT (T-7/C-37: a second insert raises). Retry: a
  poll retry after a park is record-only (`sweeper.go:575`), so there is no second row.
- Concurrency: ✓ the guard's `FOR SHARE` plus the deferred park binding. + concurrent poll and callback
  on the same live attempt: exactly one park, and at most one Y row.
- Partial failure / rollback: ✓ T-7 (a row without a park is refused at commit). + the park CAS fails,
  so the Y row is rolled back.
- Reconciliation: no money in every case (`d2AssertNoMoney`, balanced ledger).
- Authorization / tenant isolation: ✓ RLS system-only. + a second tenant with the same provider and
  the same Y gets no clearing across tenants (extend C-14d with a Y row).
- Audit: ✓ the C-36 audit-JSON test (Y never read from audit).
- Mutants:
  - drop `yAttributable` (killed by G-Y1a);
  - drop the holder arm or the ledger arm separately;
  - remove the Y branch (killed by C-36);
  - accept Y without a typed row (killed by the C-36 audit test);
  - read Y for non-`poll_reference_mismatch` reasons (killed by the DB binding; add a Go-side test);
  - drop the tombstone arm for Y (killed by G-Y3);
  - G-Y2 revert.

### 3.6 Verdict: PARTIAL
- The registered acceptance text is satisfied against MOCK: Y is persisted as a typed evidence row
  under one LF-signed schema change (0115, shared with STANDING-1), the clearing rule accepts X or Y,
  Y is never read from audit JSON, and the interim ADR wording is in place.
- The intended invariant (fail closed on ambiguity; a clear only on a reversal attributable to this
  capture) is not demonstrably satisfied, because of G-Y1. G-Y3 is a coverage gap.
- Close after G-Y1 and G-Y3 land (Go plus tests, no migration) and LF rules on G-Y2.

### 3.7 Human decisions
None. G-Y1 narrows a clearing rule in the fail-closed direction and moves no money. G-Y2 is a
ledger-finance ruling that amends LF B3; the orchestrator records it. No new financial semantics:
no posting, receivable, clawback or allocation is introduced.

---------------------------------------------------------------------------------------------------

## 4. Related rows (context only)
- PRH-2-D2-MERGE-STATE (4115): D2 in-run parts IMPLEMENTED (MOCK). Standing/unbound/persisted-reversal
  were left to STANDING-1, POLL-REF-CLEAR-1 and MA020-SYNC-MISMATCH-1. K3 delivered most of them (above).
- PRH-2-K3-MERGE-STATE (4137): 0115 merged. Lists the open items alert delivery, real PSP and
  closed-tenant funds. The wiring item is not delivered (WIRING-1).
- MA020-SYNC-MISMATCH-1 (4101): still OPEN and separate.
  - `player_open_payment_exposure` must cover `sync_amount_mismatch` / `poll_amount_mismatch` /
    `poll_reference_mismatch` (clearing on X or Y for the last), using the D2F-1 row rule.
  - LF L-4: an acting MA020 read cannot see the Y table, so it fails closed.
  - It is a real-money-tenant prerequisite, not a real-PSP one. It interacts with G-Y1: MA020's Y
    clearing should use the same attributability rule.
- Migration numbering: 0116 TEMP/search_path, 0117 alert delivery. 0118 is not needed for these three
  items.
