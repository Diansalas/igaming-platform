# PRH-2 — Provider-Independent Hardening Round + Continuous Handover Readiness — Planning Gate

Status: **PLANNING GATE — NOT AUTHORIZED FOR IMPLEMENTATION.** Author: architect. Date: 2026-09-28.
Base: `origin/claude/focused-wright-jw88w9` @ `559483a` (docs-only on top of final code `20d3ce0`).
Inputs: CLAUDE.md, MASTER-BUILD-PROMPT.md, `docs/active-stage.md` (2026-09-28 status note),
`docs/governance/payment-readiness-completion-report.md` §§3/5/7/8/11, ADR 0098 (human decisions
HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1), `docs/governance/task-registry.md`, the review records
cited below, and the code at `559483a`.

Verification performed for this plan: read-only grep/read of the code at `559483a`, `go build ./...`
(ok) and `go vet` on `internal/payments`, `internal/casino`, `internal/auth`, `cmd/...` (ok). No DB
access, no tests run. Everything below is MOCK/local scope. Nothing here is a regulatory or licensing
claim.

Binding principle (human, ADR 0098 §4): jurisdiction-specific requirements are explicit, versioned,
effective-dated, audited configuration scoped by jurisdiction/tenant/brand. They are never global
invariants and never hard-coded thresholds. Absent policy or evidence means **fail closed**.
Regulatory configuration stays separate from ledger correctness.

---

## 1. Verified state (A–L)

"Open" means verified still open in code at `559483a`.

| WS | Registry id(s) | State | Evidence (file:line at `559483a`) |
|---|---|---|---|
| A | CAS-REVOKE-CONSUMED-1 (registry l.4042) | **OPEN** | `migrations/0042_…up.sql:58-59`: terminal block still refuses every change out of `consumed`. `internal/casino/launch.go:377-383` `RevokeLaunchSession` still CAS-es `status = 'active'` only, with no `prior_status`. |
| B | CAS-PLAY-BOOTSTRAP-1 (l.4043) | **OPEN** | `ResolveLaunchToken` (`internal/casino/launch.go:279`) has no non-test caller; the only matches outside tests are its declaration and comments (`launch.go:264,279,337`). `MockCasinoProvider.Launch` (`internal/casino/mock.go:243`) does not consume. |
| C | PAY-DEP-REF-VALIDATE-1 (l.4033) | **OPEN** | The deposit adapter closure `internal/payments/drive.go:250-273` and phase C `drive.go:283-310` never call `providerref.Validate`. `drive.go` does not import `providerref`. The payout side does (`payout.go:392-395`, `:1178-1181`), and `payout.go:77` says outright that `ErrorClassProviderRefInvalid` "is never returned by a deposit". |
| D | PAY-POLL-AMOUNT-1 (l.4031) + architect FH7-06 | **OPEN** | `internal/payments/sweeper.go:513` posts `attempt.Amount`/`attempt.AssetCode` on a poll success for a live attempt, with no comparison against `res.Amount`/`res.AssetCode`. The only amount check is in the already-`succeeded` branch (`sweeper.go:471`). The echoed `res.ProviderReference` is never compared with `*attempt.ProviderReference` (query at `sweeper.go:354`). |
| E1 | KYC-SUBMIT-OUTBOX-1 (l.3989) | **OPEN** (deferred, accepted) | `internal/kyc/document_service.go:289-292` comment: the outbox "stays deferred". No outbox table exists in `migrations/`. |
| E2 | PROV-OUTBOUND-CRED-1-LEGACY-PATH (l.4002) | **OPEN** | `internal/payments/orchestrator.go:555` `InitiateDeposit(ctx, tx, …)` → `attemptDeposit` (`:701`) → `provider.Deposit(ctx, …)` at **`:714`** and `resolveAmbiguous` → `provider.QueryStatus` at **`:797`**, both with a caller-held `pgx.Tx`. Non-test callers: only `InitiateDepositAudited` (`:674-678`), which itself has no non-test caller. There are 19 test call sites across 9 files. |
| E3 | Sweep: provider call inside a financial tx | **Done for this plan (read-only)** | The only in-tx financial provider calls are E2's `orchestrator.go:714` and `:797`. Every other adapter call site is either phase-B/txscope-guarded or outside any tx: `drive.go:251` (gate), `payout.go:385`, `payout.go:1174`, `sweeper.go:354` (gate), `casino/orchestrator.go:632` (guard at `:628`), `kyc/verification_service.go:335` (guard at `:329`), `kyc/document_service.go:425` (guard at `:422`), and email `httpserver/credential_handlers.go:159,321` (after the tx closure). **New non-financial finding:** `internal/sportsbook/catalogue.go:52-56` `SyncCatalogue(ctx, tx, provider)` calls `provider.Catalogue()` inside a platform-service tx. It is harmless with the in-process MOCK and becomes a pool-exhaustion risk with a real adapter. I propose registering it as **SB-CATALOGUE-IO-1** (Low; before a real sportsbook adapter). It is not in PRH-2 scope. |
| F | KYC-ENF-OUTAGE-1 (l.4035), KYC-ENF-DECISION-ROWS-1 (l.4036), KYC-ENF-TESTPINS-1 (l.4037) | **OPEN** | Outage: `kyc/enforcement.go:465-468` turns a query error into `unavailableDecision` while the caller's tx is aborted. `withdrawal/withdrawal.go:414` then runs `RecordDecision` in that aborted tx (the 25P02 path). Decision rows: `payments/kycgate.go:56-68` reduces the result to `(bool, code)` and has no `RecordDecision`. Pins: `withdrawal.go:1046-1051` (B6 guard) is untested per mutant MB6; N5 (`Outcome=unavailable` accepted) is still in the same guard. |
| G | KS-AUDIT-TENANT-1 (l.4003), KS-CAS-DISCRIM-TEST-1 (l.4038) | **OPEN** | `httpserver/payments_kill_switch_handlers.go:246-251` `auditTenantID()` returns `uuid.Nil` for platform sessions, and the audit_log RLS is still `migrations/0014_create_audit_log.up.sql:32-40` (dual-scope, no platform→tenant family). So a tenant never sees these rows. Discrimination: `payments/drive.go:157-163` (`!engaged` branch) is untested. |
| H | CP-W1 (l.4034) | **OPEN** | `payments.NewSweeper` (`sweeper.go:102`) has no caller in `cmd/` or in non-test `internal/`. `cmd/platform-api/main.go:396-446` wires the reconciliation, RG and bonus loops, but no payments sweeper. |
| I | PAY-P1-MULTISUCCESS-ALERT-1 (l.4026) + general alert delivery | **OPEN** | Every alert is a log line only: `payments/orchestrator.go:1018,1509`, `httpserver/payments_kill_switch_handlers.go:360`, and the `*_integrity_alert_*` lines in `httpserver/{deposit,casino,casino_play,sportsbook_settlement}_handlers.go`. No alert, outbox or notification table exists in `migrations/`. |
| J | PRH-I4-METRICS-1 (l.3966) | **OPEN** | The metrics plumbing exists (`internal/observability/metrics.go:17` `InitMetrics`), but nothing defines `webhook_admission_decisions_total` (the grep for it matches nothing). |
| K | HD-0095-1 (l.3987), LEDGER-MANUAL-ADJ-4EYES-1 (l.3909), ADR 0095 §4.8 M1/M2 | **DECIDED (ADR 0098); NOT IMPLEMENTED** | No API or table exists for either. The ledger primitives exist, but only tests use them: `ledger/ledger.go:76` `AccountManualAdjustment`, `:115` `TxManualAdjustment`, and the lock-order rule `ledger/lockorder.go:355`. ADR 0095 table rows M1/M2 (l.511-512) say BLOCKED. The RBAC inventory is in §5-K. |
| L | (new) HANDOVER-1 | **OPEN** | There is no root README and no docs index. `docs/runbooks/README.md` is the de facto ops index. There is no mock-vs-real matrix and no consolidated secret-NAMES inventory. The secret names that do exist are scattered across `production-configuration-checklist.md` (33 secret/key mentions) and ADR 0093/0094. |

Other open items that PRH-2 does **not** plan (registered, unchanged): PAY-SEC-LAUNCH-1
(S-L1/S-L3/S-L4/destination binding), M5, WEBHOOK-EDGE-1, DEVOPS-0107-INDEX-WINDOW-1, PAY-RECON-N1,
CAS-REVOKE-BET-RACE-1, TEST-SCRATCH-LEAK-1, CI-BILLING-1/F-POOL-1 K1, BRANCH-PROTECTION-1. §9
recommends which of these could be added to PRH-2 if the human wants.

## 2. Dependency graph and waves

Hard edges:
- A → B (security gate (i)).
- K1 (capability grants) → K2 (manual adjustment) → K3 (force-resolve M1/M2). M1 executes through
  K2's governed adjustment.
- C, D → before any real PSP. E2 → before real PSP registration. E1 → before any real KYC adapter.
  F → before real KYC.
- I-core → I-wire (emit sites) → before relying on any P1. H → before any payout launch.
- G is independent. It must land before production or the first B2B tenant.

```
W0 (docs only): ADR 0099-0104 drafts + HANDOVER.md skeleton + registry rows ──┐
                                                                             │ human authorizes
W1: [CAS] A ─────────────► W2: [CAS] B                                       ▼
    [PAY] E2 ─► C(+G2 test) ─► D ─► F-pay ─► H ─► I-wire ─► K3
    [KYC] F-kyc ─────────► E1 (outbox; merges after H, which owns main.go worker wiring)
    [SEC] G1 (audit visibility)
    [OBS] I-core ─► J
    [RBAC] K1 ─────────────► K2 ─────────────────────────────► (K3 in PAY lane)
```

| Wave | Content | Gate to exit |
|---|---|---|
| **W0** | ADR drafts 0099–0104 (§6); `docs/HANDOVER.md` skeleton (§5-L); registry rows for new ids (SB-CATALOGUE-IO-1, HANDOVER-1, CAP-GRANT-1, ALERT-DELIVERY-1); human decisions §7 answered or explicitly deferred | ADRs reviewed: security + ledger-finance for 0099–0101, security for 0102–0104 |
| **W1** (parallel lanes) | A · E2 · F-kyc · G1 · I-core · K1 | Each lane's reviewers (§5); merged in the §3 order |
| **W2** | B · C (+KS-CAS-DISCRIM-TEST-1) · J · K2 | Same |
| **W3** | D · F-pay (KYC-ENF-DECISION-ROWS-1 payments half) · H · E1 (after H) | Same; H needs a full `-race` integration run |
| **W4** | I-wire · K3 | ledger-finance + security sign-off on K3 |
| **W5** | Final gate: QA matrix, handover DoD check, completion report, **stop** | Human |

## 3. Ownership, touched files, and merge order

**Rule 1 — one writer per critical file at a time.** No two concurrently running workstreams may
edit the same critical file. Critical files: `internal/payments/{orchestrator,receipt,sweeper,drive,payout,payout_sweep,attempt,deposit_v2,kycgate}.go`,
`internal/ledger/*`, `internal/withdrawal/withdrawal.go`, `internal/casino/{launch,orchestrator}.go`,
`internal/auth/permission.go`, `internal/httpserver/{routes,server}.go`, `cmd/platform-api/{main,wiring,registrations}.go`,
`deploy/init-app-role.sql`, `migrations/` numbering, `docs/governance/task-registry.md`,
`docs/active-stage.md`, `docs/progress.md`, ADR 0095/0096 amendment sections. The PAY lane is
**strictly serial** for this reason.

**Rule 2 — the orchestrator is the only writer** of the registry, active-stage, progress, HANDOVER
index rows and ADR amendment sections. Workstreams hand over their text, and the orchestrator applies
it at merge time.

**Rule 3 — the migration number is fixed at merge time** in merge order, gap-free. A branch that
finds it needs an unplanned migration takes the next free number when it merges, and any unmerged
downstream branch renumbers. No pre-reserved gaps.

**Rule 4 — `deploy/init-app-role.sql`** changes only by appending least-privilege grants for new
tables, applied at merge. No role, password or attribute change (CLAUDE.md "Environment safety").

| WS | Owner (reviewers) | Touches | Lane / wave |
|---|---|---|---|
| A | casino (SEC, CR, QA) | `migrations/0108_*`, `casino/launch.go`, `casino/orchestrator.go` (revoke caller, audit), casino tests | CAS / W1 |
| B | casino (SEC, ARCH, CR, QA, POP) | new `casino/bootstrap.go`, `httpserver/casino_routes.go`, `casino/mock.go` (the MOCK vendor calls the real endpoint), `routes.go` (1 line) | CAS / W2 |
| C | payments (SEC, LF, CR, QA) | `payments/drive.go`, deposit tests | PAY / W2 |
| G2 | payments + qa | `payments/*_test.go` only (drive T2 kill-switch) | PAY, merged with C |
| D | payments (LF, SEC, CR, QA) | `payments/sweeper.go`, poll tests | PAY / W3 |
| E1 | identity-compliance (SEC, ARCH, CR, QA) | `migrations/0113_*`, `kyc/document_service.go`, `kyc/verification_service.go`, new `kyc/outbox.go`, `httpserver/kyc_handlers.go`, 1 worker line in `main.go` (after H) | KYC / W3 |
| E2 | payments (LF, SEC, CR, QA) | `payments/orchestrator.go` (delete `:555-~800` chain), 9 test files | PAY / W1 |
| F-kyc | identity-compliance (SEC, CR, QA) | `kyc/enforcement.go`, `withdrawal/withdrawal.go` (`:397-417`, `:1046-1051`), `httpserver/withdrawal_handlers.go`, ADR 0096 N2 row | KYC / W1 |
| F-pay | payments + identity-compliance (SEC, CR) | `payments/kycgate.go`, `payments/payout.go` (T1p allow), `payments/payout_sweep.go:99-130` | PAY / W3 |
| G1 | architect + security (payments, CR, QA) | `migrations/0109_*`, `httpserver/payments_kill_switch_handlers.go`, `audit/audit.go`, audit read handler | SEC / W1 |
| H | payments + devops (LF, SEC, CR, QA) | new `payments/sweeper_loop.go`, `cmd/platform-api/main.go`, `config/config.go`, `cmd/platform-api/*_ast_test.go`, prod config checklist | PAY / W3 |
| I-core | devops + architect (SEC, CR, QA) | new `internal/alerting/` (incl. the dispatcher loop function, not wired), `migrations/0110_*` | OBS / W1 |
| I-wire | payments + casino + devops (SEC, CR) | `payments/orchestrator.go:1018,1509`, `httpserver/payments_kill_switch_handlers.go:360`, integrity-alert sites, dispatcher line in `main.go` | PAY / W4 |
| J | security + devops (CR) | `httpserver/webhook_admission.go`, `internal/observability` | OBS / W2 |
| K1 | architect → backend + security (SEC, CR, QA, POP) | `migrations/0111_*`, `auth/permission.go`, new `internal/capability/`, new `httpserver/capability_routes.go`, `routes.go` (1 line) | RBAC / W1 |
| K2 | ledger-finance (SEC, CR, QA, ARCH) | `migrations/0112_*`, new `internal/adjustment/` (it calls `ledger.Post` and never edits `ledger.go`, unless LF rules otherwise), new route file | RBAC / W2 |
| K3 | payments + ledger-finance (SEC, CR, QA, ARCH) | `migrations/0114_*`, `payments/attempt.go` (guard-aware transition), new `payments/manual_resolution.go`, uses `withdrawal.Complete/Fail` and K2's executor | PAY / W4 |
| L | architect + orchestrator | `docs/HANDOVER.md` (new), links only | W0, then every merge |

**Merge order (strict, one merge at a time, full local verification between merges):**
W0 docs → **A (0108)** → **G1 (0109)** → E2 → F-kyc → **I-core (0110)** → **K1 (0111)** → C+G2 → B → J →
**K2 (0112)** → D → F-pay → H → **E1 (0113)** → I-wire → **K3 (0114)** → W5.

`main.go` has a single owner. H introduces the worker-loop wiring. E1's outbox worker and I's
dispatcher loop are wired into `main.go` only **after** H has merged: E1 at its own merge, the alert
dispatcher as part of I-wire. I-core ships the loop function but does not wire it.

Parallelism: at most one PAY-lane item is in flight at any time. The CAS, KYC, SEC, OBS and RBAC
lanes run concurrently with it and with each other, because their file sets are disjoint. The only
shared files are `routes.go` (1-line registrations) and `init-app-role.sql` (append-only grants),
and both are serialized at merge.

## 4. Migration allocation (0108 onward, gap-free in merge order)

| No. | WS | Content (summary) | Down |
|---|---|---|---|
| **0108** | A | Replace `casino_launch_sessions_enforce_immutable_fields()`. Keep 0042's column block, and allow exactly `consumed → revoked` with `(to_jsonb(NEW)-'status') = (to_jsonb(OLD)-'status')`. This is the security spec verbatim. | Restore 0042's body verbatim |
| **0109** | G1 | `audit_log.subject_tenant_id UUID NULL` plus an index, and an additional SELECT policy: tenant sessions also read rows where `tenant_id IS NULL AND subject_tenant_id = app.tenant_id`. Write-side RLS is unchanged, so no platform-writes-into-tenant power is created. An immutability trigger already covers the new column (0014 deny-update). | Refuse while any row has `subject_tenant_id` set (append-only history, B2 precedent); otherwise drop |
| **0110** | I-core | `alerts` (dedup key UNIQUE, severity, tenant_id NULL = platform, state `open/acked/resolved`, occurrence_count, first/last_seen) and `alert_deliveries` (sink, attempt, status, next_retry_at, last_error_class). FORCE RLS, dual-scope plus the 0106 validated platform GUC. Deliveries are append-only. | Refuse while rows exist |
| **0111** | K1 | `staff_capability_grants` (+ optional `staff_capability_grant_approvals`). FORCE RLS in two families (tenant and validated platform GUC, 0105/0106 pattern). Triggers: closed capability enum with a scope map; grantor ≠ grantee **and** distinct `person_id`; the grantee must be active, person-linked, and in the same tenant as a tenant grant; a platform capability can be written only under the validated platform GUC; append-only except a single `revoked_*` transition under whole-row equality. | Refuse while rows exist |
| **0112** | K2 | `financial_approval_policies` (append-only, effective-dated, scoped), `ledger_adjustment_requests`, `ledger_adjustment_approvals`. DB triggers: approver ≠ initiator and distinct Person; approval count checked against the **policy snapshot pinned on the request**; `executed` only with a same-tx `ledger_transaction_id`; UNIQUE idempotency key. | Refuse while rows exist |
| **0113** | E1 | `kyc_submission_outbox` (tenant, verification_id, content-derived key UNIQUE, state `pending/claimed/sent/failed_terminal`, claim_token, attempts, next_attempt_at). FORCE RLS. | Refuse while non-terminal rows exist |
| **0114** | K3 | `payment_manual_resolutions` (+ approvals). Amend `payment_attempts_guard()` so it allows `ambiguous/disputed → {succeeded, failed, resolved_no_action}` **only** with a same-tx approved resolution row (the 0105 `decided_txid` pattern). The payout path goes only through the withdrawal state machine. Whether `resolved_no_action` is a new state (a CHECK change, with the reconciliation kinds `pay_captured_unposted`/`pay_duplicate` taught about it) or a resolution row on a still-`disputed` attempt is ledger-finance's call in ADR 0101. | Restore the 0107 guard verbatim; refuse while resolutions exist |

Expected **no migration**: B (the session `consumed` state already exists; if bootstrap idempotency
needs a column, the next free number is taken at merge, per Rule 3), C, D, E2, F, G2, H (advisory
locks plus the existing claim tokens), J.

## 5. Per-workstream design, invariants, tests, reviewers, handover DoD

Test legend: **R** regression · **ADV** adversarial · **CON** concurrency · **TI** tenant isolation ·
**AZ** authz · **FL** failure/fault injection · **IDM** idempotency · **RB** rollback/recovery ·
**AU** audit · **RLS** · **MIG** up/down/up and pre-flight · **MUT** named mutants.
Invariants legend: **I1** INV-DEP-1 · **I2** append-only double-entry, SUM(D)=SUM(C) · **I3** DB-enforced
idempotency · **I4** FORCE RLS / server-side tenant · **I5** no direct balance mutation · **I6**
HD-LEDGER-UNALLOC-1 A-now (second success → disputed, no posting) · **I7** no provider I/O in a
financial tx (F-POOL-2).

**A — CAS-REVOKE-CONSUMED-1.** Implement the security spec exactly (`rv-prh-i2-casino-security.md`
FH-7). `RevokeLaunchSession`: `SELECT status … FOR UPDATE`, then
`UPDATE … WHERE id=$1 AND status IN ('active','consumed')`. It returns the prior status, and the
`casino.launch_failed` audit records `prior_status` and `revoked`.
- *Invariants:* I2, I4. Token replay unchanged (`ResolveLaunchToken` accepts `active` only).
- *Tests:* the trigger matrix from the spec (allowed and refused cells) [MIG, R]; replay after revoke →
  `ErrLaunchSessionNotActive` [ADV]; inverted characterization test (revoked, `prior_status=consumed`,
  bet refused, balance unchanged, ledger balanced) [R, AU]; pre-revoke bets still settle and roll back
  [R]; tenant B cannot revoke A's session [TI, RLS]; MUT: drop whole-row equality, allow `expired`,
  drop `NEW.status='revoked'`, restore to `active`.
- *Reviewers:* security (spec owner), code-reviewer, qa.
- *DoD docs:* ADR 0025/08-casino note, registry close, HANDOVER casino row.

**B — CAS-PLAY-BOOTSTRAP-1** (after A merged).
- *Mechanism:* a provider-neutral inbound **token-exchange endpoint**:
  - It is authenticated by the provider's existing `webhookauth` scheme and per-tenant credential. It
    is never a platform JWT, and there is no unauthenticated path.
  - It consumes the raw launch token (`active → consumed` CAS in one tx) and returns canonical session
    context (session id, player ref, asset, mode). No balance is embedded, so balance reads stay on the
    wallet path.
  - Replay by the same provider with the same bootstrap request id is idempotent (same response).
    Any other replay → refused and audited.
  - Wrong provider, tenant, mode or asset → refused and audited.
  - The kill switch, capability and RG gates are re-checked at consume.
- *MOCK:* the MOCK vendor exercises the endpoint over HTTP in tests and in the MOCK play route. There
  is no in-process shortcut, and the 2-minute rule for never-consumed sessions is **not** relaxed.
  The tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic` is unchanged.
- *Invariants:* I4, I7 (no outbound call).
- *Tests:* ADV (replay, cross-provider, cross-tenant token, expired token, revoked token), CON (two
  concurrent consumes → exactly one), IDM, AZ (bad signature), AU, TI; a MUT set on the CAS predicate
  and the provider match; plus the regression that an unconsumed session is still refused after TTL.
- *Reviewers:* security (mandatory; gate (i)), architect (the contract becomes part of the
  casino provider interface doc), code-reviewer, qa, product-owner-proxy (scope).
- *DoD:* ADR 0103, `docs/integrations/` vendor contract page, HANDOVER mock-vs-real row.

**C — PAY-DEP-REF-VALIDATE-1.**
- In `depositAdapterCall` (`drive.go:250`), validate `res.ProviderReference` with `providerref.Validate`
  before the outcome switch. Mirror `payoutAdapterCall` exactly: a violation →
  `ErrorClassProviderRefInvalid` → phase C **parks** via `ApplyDisputeFromNonTerminal`
  (`terminal_reason='invalid_provider_reference:<reason>'`). There is no posting and no retry.
- Also verify the deposit **binding**: a sync success whose reference is already bound to another
  attempt, intent, tenant or operation → disputed, never posted.
- *Invariants:* I1, I3, I4, I6.
- *Tests:* ADV (over-long, control characters, empty-on-success, reference of another tenant's
  attempt, reference of a payout attempt, reference replayed from a prior intent); CON (callback and
  sync success racing with conflicting references); IDM; AU (park audited); RB; the A7 suite still
  passes [R]; MUT (drop validation, drop park, reorder before switch).
- *G2:* add the forced non-kill-switch T2 CAS-conflict test and re-run mutant K1.
- *Reviewers:* security, ledger-finance, code-reviewer, qa.
- *DoD:* ADR 0095 §8 amendment row (the deposit half of C1), PRH-REF record, registry.

**D — PAY-POLL-AMOUNT-1 + FH7-06.** In `applyStatusEvidence`'s live-attempt success branch
(`sweeper.go:~483-513`), before `postDepositSuccessOrDispute`:
- If `res.Amount/AssetCode ≠ attempt` → T10 disputed (`terminal_reason='poll_amount_mismatch'`),
  plus audit and an alert (I-wire).
- If `res.ProviderReference` is non-empty, it must equal `*attempt.ProviderReference` (after
  `ValidateOptional`); otherwise → disputed (`poll_reference_mismatch`).
- The check order matches the callback path: mismatch → tombstone → INV-DEP-1 → post.
- *Invariants:* I1, I2, I6.
- *Tests:* ADV (amount lower or higher, asset differs, reference differs, empty reference); CON (poll
  mismatch racing a matching callback → exactly one terminal state, no double post; the D and K race
  tests re-run `-count=50`); the T17 re-drive path [R]; AU; MUT (drop each comparison).
- *Reviewers:* ledger-finance, security, code-reviewer, qa.
- *DoD:* ADR 0095 §4.4 matrix poll row amendment, registry.

**E1 — KYC-SUBMIT-OUTBOX-1.**
- Phase A writes an outbox row in the same tx as the verification/document rows (content-derived key,
  UNIQUE).
- A worker claims rows with `FOR UPDATE SKIP LOCKED` and a claim token, commits, calls
  `SubmitVerification` outside any tx (txscope-guarded), and applies phase C in a new tx under a claim
  check.
- An ambiguous result → retry with backoff. After a bounded maximum → `failed_terminal` plus an alert.
  Status is never inferred.
- *Invariants:* I4, I7; KYC status changes only from a definite provider outcome.
- *Tests:* crash between A and B, and between B and C [RB, FL]; two workers [CON]; duplicate submit
  [IDM]; TI/RLS; AU; MIG; MUT (drop the claim-token check, drop SKIP LOCKED).
- *Reviewers:* security, architect, code-reviewer, qa; identity-compliance owns.
- *DoD:* ADR 0095 §15.3 amendment, runbook "KYC outbox stuck" entry, HANDOVER.

**E2 — Legacy deposit path.**
- **Delete** `InitiateDeposit`, `InitiateDepositAudited`, `attemptDeposit`, `handleDecline` and
  `resolveAmbiguous` (`orchestrator.go:555-~800`). This is recommended over converting: there is no
  production caller, and FH7-04 says "delete before the first real PSP".
- Migrate the 19 test call sites to `InitiateDepositAttempt`. Any test that exists only to exercise the
  legacy chain is deleted with a written justification. Tests whose *subject* is something else (e.g.
  the reconciliation legacy-posting exclusion, `migration_0082` immutability) keep their subject via
  direct fixtures.
- *Invariants:* I1, I7.
- *Tests:* coverage parity: every INV-DEP-1, A7 and reconciliation mutant previously killed is still
  killed [MUT, R]; the static guard `txscope/no_provider_call_in_tx_closure_static_test.go` passes.
- *Reviewers:* ledger-finance (coverage parity), security, code-reviewer, qa.
- *DoD:* ADR 0095 §29 F-POOL-2 state → payments legacy CLOSED; PROV-OUTBOUND-CRED-1 precondition
  re-assessed.

**F — KYC enforcement completion.**
- *OUTAGE-1:* wrap the evaluator reads in a **savepoint**. On a DB error, roll back to the savepoint,
  record the `unavailable` decision and audit in the still-valid tx, and return `ErrKYCUnavailable` →
  the handler maps it to 503. The alternative (a separate tx after rollback) is the fallback if
  ledger-finance objects.
- *DECISION-ROWS-1:* `KYCEnforcementDepositGate` and payout T1p allow and T2/T12 deny call
  `kyc.RecordDecision`. This happens in the same tx for allow and deny, and for deposit it happens in
  the phase-A tx.
- *TESTPINS-1:* pin B6 (MB6), the play-deny decision rows (MPLAYREC), and N5 (refuse
  `Outcome=unavailable` in `DenyForCompliance`).
- **No KYC threshold is introduced.** Policies stay dormant pending HD-KYC-1..8.
- *Tests:* a fault-injection lock-timeout probe (the code-reviewer's N1 probe as a test) [FL]; one
  decision row per evaluation for every call site [AU]; MUT MB1, MB6, MPLAYREC; TI/RLS on decisions.
- *Reviewers:* security, code-reviewer, qa; payments co-owns F-pay.
- *DoD:* ADR 0096 §16 rows + N2 evidence addendum (the missing `prh-i3-mutation-kill.txt` addendum),
  KYC-ENFORCE-1 prose fix.

**G1 — KS-AUDIT-TENANT-1.** Three options:
- (a) An INSERT policy letting a platform session write `tenant_id=<target>`. **Rejected:** it grants
  write power into tenant audit scope.
- (b) A second-tx dual write. **Rejected:** not atomic with the mutation.
- (c) **Recommended:** the platform row keeps `tenant_id NULL` and gains `subject_tenant_id=<target>`,
  written in the same tx. A tenant read policy exposes exactly those rows.

For (c), the audit read API adds `subject_tenant_id` filtering. Actor identity is displayed per
HD-PRH2-5. Apply it to kill-switch mutations now. Survey the same shape in
`provider_credential_handlers.go` (`recordProviderCredentialDenied`) and register it; extending it
there needs the orchestrator's go-ahead.
- *Invariants:* I4 (no audit bypass; audit stays append-only).
- *Tests:* tenant A sees a platform action on A's switch and never on B's [TI, RLS]; a tenant cannot
  write `subject_tenant_id` [ADV]; an UPDATE is refused [AU]; MIG with refuse-down; MUT (drop the
  policy predicate's tenant equality).
- *Reviewers:* security (mandatory), code-reviewer, qa.
- *DoD:* ADR 0104, `12-audit-reporting-architecture.md`, security-architecture audit section.

**H — CP-W1 sweeper process.**
- Add `payments.RunSweeperLoop(ctx, pool, sweeper, interval)`, mirroring
  `reconciliation.RunSchedulerLoop`:
  - active-tenant enumeration;
  - a per-tenant `pg_try_advisory_xact_lock('payments_sweeper:'||tenant)`, as a lease on top of the
    existing claim tokens;
  - bounded batch; per-tenant panic recovery; context-cancel; a shutdown `WaitGroup`.
- New config `PAYMENTS_SWEEP_INTERVAL_SECONDS` (validated positive).
- Kill switch honoured. Only synthetic adapters can be registered (tripwire). OTel counters for
  claimed, processed, errors and escalations, plus a staleness gauge.
- Nothing pays out for real: MOCK adapters only.
- *Invariants:* I1–I7.
- *Tests:*
  - two loops against one DB → no double processing [CON];
  - crash between phase B and C → recovered [RB];
  - kill switch engaged → no dispatch [R];
  - tenant A's failure does not stall B [FL, TI];
  - shutdown drains [FL];
  - `main` construction and run-order AST tests updated [R];
  - full `-race -tags integration`.
- *Reviewers:* ledger-finance, security, code-reviewer, qa, devops.
- *DoD:* production-configuration-checklist "Scheduled jobs" row, operational-runbooks entry "sweeper
  stalled/escalations", observability-and-alerting.

**I — Alert delivery.**
- *I-core:* `alerting.Raise(ctx, tx, Alert{DedupKey, Severity, TenantID?, Kind, Attrs})` inserts into
  or upserts `alerts` **in the caller's tx** (an outbox: the alert is durable iff the event
  committed). Dedup is by UNIQUE key, and an occurrence bumps a counter.
- A dispatcher loop (outside tx) delivers through a provider-neutral `AlertSink` interface. Only a
  `MockSink` (and a log sink) exists.
- Delivery retries with exponential backoff up to a bounded maximum, then goes to `dead`, plus a
  meta-log.
- Acknowledge and resolve are staff actions: audited, and tenant-scoped via RLS (platform alerts are
  visible to platform only).
- Attributes are redacted, following the existing redaction rules.
- *I-wire:* replace the log-only sites listed in §1-I (logs kept). PAY-P1-MULTISUCCESS-ALERT-1 comes
  first: it already runs in a fresh tx (`RecordDepositMultipleSuccessRefusal`), and `Raise` goes into
  that tx.
- *Tests:* IDM/dedup, CON (N raisers → 1 alert, N occurrences), FL (sink failure → retry → dead), RB
  (dispatcher crash), TI/RLS, AU (ack/resolve), and the property that a rolled-back event raises no
  alert; MUT (drop dedup, drop tx-binding).
- *Reviewers:* security, code-reviewer, qa, devops.
- *DoD:* ADR 0102, `observability-and-alerting.md` (the severity→route matrix is a placeholder until
  HD-PRH2-4), runbooks.

**J — PRH-I4-METRICS-1.**
- Add the counter `webhook_admission_decisions_total{decision,reason,provider_kind}` and the in-flight
  gauge. Labels are bounded, with **no tenant label** (HD-PRH-1 is still open).
- Emission is fire-and-forget. A test proves that admission decisions are identical with a no-op
  meter or a failing exporter, so metrics can never become a prerequisite for enforcement.
- *Reviewers:* security, code-reviewer.
- *DoD:* ADR 0097 §8 status, observability doc.

**K — Capabilities (force-resolve, manual adjustment).**

*Inventory of what exists (verified):*

| Element | Where | Notes |
|---|---|---|
| Static role → permission map | `internal/auth/permission.go:587-880` | Deliberately not DB-driven ("Stage 6 custom roles" deferred, `:584-586`) |
| One role per staff user | `migrations/0011:12`, widened in `0064:13-15`; `tenant_id NULL` ⇔ platform_admin (`0011:23-26`) | Dual-scope RLS `0011:39-48` |
| Route gates | `RequirePermission` / `RequireAnyPermission` / `RequireTenantScope` (`permission.go:900-961`) | They read the role from the JWT (`tc.Role`, `:909`), so a revocation needs a DB read to take effect immediately |
| Staff ↔ Person link and self-approval guard | `migrations/0029:18` (`staff_users.person_id`) | Distinct-Person precedent |
| Four-eyes patterns | withdrawal approvals (`0026:107`), asset change requests (`0044:152-187`), bonus change requests + **`bonus_approval_policies`** (`0063:32-84`: append-only, `effective_from`, threshold `NUMERIC(38,0)`, `required_approvals`, brand/asset scope), casino catalogue (`0086`), provider credentials (`0096:143-196`), kill-switch release (`0105:150-167`: approver ≠ requester, same-tx approval, platform-lock) | Reuse these patterns; do not invent a new one |
| Platform principal validation | `db.WithPlatformAdmin` (`internal/db/tenant_rls.go:112`); validated GUC (`0105:27-42`, `0106`) | Two-family RLS precedent |
| Separation-of-duties precedents | `tenant_admin` holds no withdrawal authority, and only a platform caller may create `finance` staff (`permission.go:651-674`) | Tension with ADR 0098; see HD-PRH2-2 |
| Audit | `audit_log` (`0014`), append-only (`0014:43-53`, `0016`) | — |
| Ledger primitives | `TxManualAdjustment` / `AccountManualAdjustment` (`ledger/ledger.go:76,115`) | Used only by test fixtures |

*What must be extended* (ADR 0099, which records the human's "extend, document as ADR"): **per-user,
scoped, DB-held capability grants layered on the static RBAC**. This is not a second authorization
system. A governed action needs **both**:
- (1) a static permission on the principal's role, e.g. `capability_grant:manage` for administrators;
  and
- (2) for the four new grantable capabilities, an in-force grant row read **inside the action's own
  tx**:
  - `payment_force_resolve:request`
  - `payment_force_resolve:approve`
  - `ledger_adjustment:initiate`
  - `ledger_adjustment:approve`

JWT claims are never trusted for grants.

*K1 grant model (migration 0111).*
- *Scope:* `platform` (`tenant_id NULL`, platform principal only, validated GUC) or `tenant` (grantor
  is a tenant principal holding `capability_grant:manage`, or a platform principal where HD-PRH2-6
  allows).
- *Grantor rules:*
  - No self-grant: grantor ≠ grantee, and a distinct Person. This is the human's "actor/subject
    separation".
  - The grantor must administer the scope. A tenant grantor → only their own tenant, never the
    platform scope. Enforced by trigger and RLS, not only in Go.
- *Grantee rules:* active, person-linked, same tenant.
- *Lifecycle:*
  - `valid_from` and optional `valid_until`.
  - Revoke is a one-way transition (whole-row equality), audited with a reason code.
  - Grant requires approval when the tenant's policy says so (HD-PRH2-2). Fail-closed engineering
    default: approval required.
- *API:* `POST/GET /v1/admin/capability-grants`, `POST …/{id}/revoke` (idempotency key; RFC-style
  problem errors). Every mutation writes an audit row with actor, subject, before/after, reason and IP.

*K2 manual adjustment (migration 0112; ledger-finance owns).*
- *Policy:* `financial_approval_policies`, append-only and effective-dated. Scope is `platform
  default → jurisdiction → tenant → brand`, keyed by `operation` ∈ {`ledger_manual_adjustment`,
  `payment_force_resolve_deposit`, `payment_force_resolve_payout`}, with optional `asset_code`. Each
  row carries:
  - `enabled`;
  - `four_eyes_mode` ∈ {`always`, `above_threshold`, `never`*} (*`never` only if HD-PRH2-1 allows);
  - `threshold_minor_units NUMERIC(38,0)`, required iff `above_threshold`;
  - `required_approvals ≥ 1`;
  - `approver_separation` ∈ {`distinct_principal`, `distinct_person`};
  - `legal_review_reference` (required to activate a jurisdiction or tenant row, following the KYC
    policy precedent).
- *Resolution:* deterministic. Take the most specific in-force row, but a jurisdiction row is a
  **floor** that a tenant or brand may only tighten (the ADR 0045 narrowing precedent). No in-force
  row → the operation is **disabled** (fail closed). **No threshold value is seeded anywhere.**
- *Flow:* `requested → approved → executed`, or `rejected / cancelled / expired`.
  - The policy is pinned on the request and re-resolved at execution: stricter wins, and any
    tightening in between needs the additional approvals.
  - The approver must hold `ledger_adjustment:approve` in-tx, must not be the initiator, and must be a
    distinct Person.
  - The reason code comes from a closed catalogue plus a note, and an evidence reference is attached.
  - Execution is one tx: lock the request, verify the approvals, re-check both capabilities
    (revocation fails closed), then `ledger.Post` a balanced `manual_adjustment` transaction with the
    idempotency key = request id (DB UNIQUE), then link `ledger_transaction_id`.
  - Never a balance UPDATE. The HR-9 bonus-set guard still applies. Any overdraft rule is a
    ledger-finance invariant, not config.
- *Invariants:* I2, I3, I4, I5; I1 (an adjustment is never a deposit posting).

*K3 force-resolve M1/M2 (migration 0114).*
- *M1:* a disputed deposit attempt is resolved either with "no ledger action" (evidence only) or with
  a compensating transaction **executed through K2**. It never posts a second deposit for a resolved
  intent: I1 and I6 preserved, and ledger-finance rules whether an M1 credit is allowed at all when
  the intent already posted.
- *M2:* payout ambiguous/disputed → "declare paid" (`withdrawal.Complete`) or "declare not paid"
  (`withdrawal.Fail`, which releases the hold). This goes only through the existing withdrawal state
  machine. The attempt transition is allowed by the 0114 guard only with a same-tx approved resolution.
- Both M1 and M2 are gated by `payment_force_resolve:*` and the `financial_approval_policies` row for
  their operation.

*Tests (K1–K3):*
- AZ matrix: every role × scope × capability; a tenant grantor cannot write the platform scope or
  another tenant; self-grant refused; a sock-puppet same-Person refused.
- CON: two approvals racing; approve racing revoke; execute twice.
- IDM, TI/RLS, AU (every grant, revoke, approve and execute).
- RB: execution failure leaves no partial rows.
- MIG.
- Property: SUM(D)=SUM(C) after random adjustment sequences.
- MUT: drop distinct-Person, drop the in-tx capability read, drop the pinned-policy check, allow
  `executed` without a ledger tx, allow a guard transition without a resolution.

*Reviewers:* security (all), ledger-finance (K2, K3; owner of financial invariants), code-reviewer,
qa, architect (K1, and conformance of K2/K3 to ADR 0099–0101), product-owner-proxy (K1 scope).

*DoD:* ADR 0099/0100/0101, ADR 0095 §4.8 amendment (M1/M2 → IMPLEMENTED/MOCK), `05-identity-architecture.md`
RBAC section, security-architecture, backoffice permission list (`backoffice/src/auth/permissions.ts`),
operational runbook "manual adjustment / force-resolve procedure".

**L — Handover readiness.**
- *Inventory:*
  - no root README;
  - `docs/runbooks/README.md` (ops index plus local setup);
  - `docs/api/README.md`;
  - `docs/security/{security-architecture,runtime-role-separation}.md`;
  - `docs/testing/testing-strategy.md`;
  - `docs/architecture/00-38` plus domain models;
  - `docs/governance/{task-registry,ownership,change-control,agent-registry}.md`;
  - `deploy/docker/README.md`;
  - `backoffice/README.md`.
- *Proposal:* a **links-only** `docs/HANDOVER.md` with no restated content and no repo
  reorganisation. Sections:
  1. start here (CLAUDE.md, MASTER-BUILD-PROMPT, active-stage, progress);
  2. architecture map;
  3. decisions index (the ADR directory, the latest human decisions);
  4. how to build, test and run locally (runbooks/README);
  5. security;
  6. operations/runbooks;
  7. **mock-vs-real matrix** (NEW table: capability × adapter × status MOCK/REAL/none × the gating
     precondition, e.g. the PROV-OUTBOUND-CRED-1 tripwire, KYC outbox, CAS A/B);
  8. **secret NAMES inventory** (NEW table: name, consumer, store/`secret_ref` scheme, rotation owner,
     ADR). **Names only, never values.** It lives in `docs/security/secret-names-inventory.md`, and
     HANDOVER links to it;
  9. open launch blockers → the registry filter;
  10. per-workstream DoD checklist.
- **Handover DoD for every PRH-2 workstream:** registry row updated; the ADR or amendment written;
  runbook entry if the change is operational; HANDOVER mock-vs-real and secret-names rows if an
  adapter, secret or config changed; production-configuration-checklist row for any new env var; the
  review records filed under `docs/plans/prh2-hardening-round/`.

## 6. New ADRs (highest existing: 0098 → next 0099)

| ADR | Title | For | Must be accepted before |
|---|---|---|---|
| **0099** | Scoped staff capability grants layered on static RBAC | K1 (the human requires the extension to be documented) | K1 code |
| **0100** | Governed manual ledger adjustments and configurable, effective-dated approval policies | K2 (+ policy model reused by K3) | K2 code |
| **0101** | Payment force-resolution M1/M2 (amends ADR 0095 §4.8) | K3 | K3 code |
| **0102** | Durable alerting and provider-neutral alert delivery | I | I-core code |
| **0103** | Casino launch-token bootstrap (vendor token exchange) contract | B | B code |
| **0104** | Tenant-visible audit of platform-scoped actions (`subject_tenant_id`) | G1 | G1 code |

These are amendments rather than new ADRs: ADR 0095 (§4.4 poll row [D], §8 C1 deposit [C], §15.3
outbox [E1], §29 legacy path [E2], §7.3 sweeper process [H]), ADR 0096 (§16 [F]), ADR 0097 §8 [J].

## 7. Human decisions surfaced (not decided here)

| ID | Question | Options | Consequences |
|---|---|---|---|
| **HD-PRH2-1** | May a policy switch four-eyes **off** entirely (`never`) for manual adjustments and/or force-resolve, or is there a platform floor? | (a) `never` allowed per tenant/jurisdiction with a legal-review reference; (b) platform floor: four-eyes can never be fully disabled, only thresholded; (c) force-resolve (no provider evidence) always four-eyes, adjustments configurable | (a) is maximally flexible and relies on legal review per config; (b) is the simplest audit story, but some small operators may object; (c) is a split rule. CLAUDE.md says "four-eyes above a configurable threshold", so (a) needs an ADR 0098 reconciliation note. |
| **HD-PRH2-2** | Does granting a money-moving capability itself require a second approver? (Tension: Stage 3D kept withdrawal authority away from `tenant_admin` because a tenant admin can mint staff accounts.) | (a) a single authorized admin grants; (b) configurable per tenant, fail-closed default = approval required; (c) a platform co-approval is always required for tenant grants of money-moving capabilities | (a) reopens the sock-puppet path: one human with `staff:manage` can create two "independent" approvers. Distinct-Person checks limit this only if Person linkage is trustworthy. (b) is the architect's recommendation for the mechanism, but the default needs confirmation. (c) is the strongest and adds platform operational load for B2B. |
| **HD-PRH2-3** | Is any monetary threshold required **before implementation**? | Architect finding: **no**. `always` mode needs no value, and absent policy = disabled. Values are needed only before real-money use per tenant/jurisdiction. | Asks the human only to confirm that values are configuration set later under legal review (ADR 0098 §4) |
| **HD-PRH2-4** | Alert routing: destinations, severity→channel matrix, on-call ownership, acknowledgement expectations; later, a paging vendor | Mock sink only now; the matrix stays a placeholder | Until decided, P1s are durable and visible, but nobody is paged. PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking. |
| **HD-PRH2-5** | Tenant-visible platform actions: show the platform staff identity, or a pseudonymous "platform operator" plus an internal reference? | (a) full identity; (b) pseudonymous; (c) configurable per tenant contract | Privacy and B2B commercial expectation; it affects the G1 read API only |
| **HD-PRH2-6** | May a **platform**-scope capability holder adjust or force-resolve money in a tenant operating under **its own licence** (hybrid model, ADR 0006)? | (a) yes, all tenants; (b) only tenants under the platform licence; (c) per-tenant opt-in configuration | Licensing and jurisdiction question. It fixes whether K1's platform grants reach tenant ledgers. |
| (existing) HD-PRH-1, HD-KYC-1..8 | Not re-asked. J uses no tenant label pending HD-PRH-1; F adds no thresholds. | — | — |

Also for the orchestrator (not a policy decision): whether PRH-2 may append must-PASS test names to
`ci.yml`, as the previous block did. By default this plan makes **no CI change**.

## 8. Out of scope

Real vendors of any kind (PSP, casino, KYC, custodian, paging or alert vendor); vendor selection or
contracts; AWS, Terraform, `deploy/aws`, staging bring-up, WEBHOOK-EDGE-1; production credentials or
data; CI, billing and branch-protection changes (CI-BILLING-1, BRANCH-PROTECTION-1); DB role,
password or test-infrastructure changes; Bonus Wave 4; AI agents; custom roles or a DB-driven role
catalogue (only the four named grantable capabilities); PAY-SEC-LAUNCH-1, M5, LEDGER-SUSPENSE-B-1
(HD-LEDGER-UNALLOC-1 option B), SB-CATALOGUE-IO-1 (registered only); any threshold or jurisdiction
rule value; repo reorganisation.

## 9. Risks and recommended authorization order

| Risk | Mitigation |
|---|---|
| PAY lane serialization makes it the critical path (7 items) | E2 first, because it shrinks `orchestrator.go`. Keep each PAY item small, and run the other lanes in parallel. |
| K becomes an unbounded authorization framework | Four named capabilities, a closed enum, product-owner-proxy review of ADR 0099, and no custom roles |
| Sock-puppet approvers (HD-PRH2-2) | Distinct-Person triggers now; the human decides grant approval |
| G1 read policy over-exposes platform audit rows | Exact `subject_tenant_id = app.tenant_id` predicate; security mutant on the predicate |
| E2 deletion loses test coverage | Mutant-parity requirement signed by ledger-finance |
| H surfaces latent sweeper bugs once it runs continuously | Full `-race` run, the 25-round storm re-run, and the kill switch as the operational brake |
| No CI evidence (CI-BILLING-1) | Local replays labelled as such, never as CI |
| Migration renumbering churn | Rule 3; numbers are final only at merge |

**Recommended order to request authorization:**
1. W0 (ADRs 0099–0104, HANDOVER skeleton, registry rows) plus answers to HD-PRH2-1/2/6. K cannot start
   without these, but other lanes can.
2. W1–W3 for the provider-independent fixes: A, E2, F, G1, I-core, C, D, B, E1, J, H. None of these
   depends on §7.
3. K1 → K2 → K3 (W1–W4 RBAC lane) once HD-PRH2-1/2/6 are answered.
4. I-wire, then the W5 final gate and stop.
