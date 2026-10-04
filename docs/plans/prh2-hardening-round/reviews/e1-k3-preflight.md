# PRH-2 E1 / K3 — read-only dependency and file-ownership preflight (architect)

- **Reviewer:** `architect`, read-only. No tracked file was changed, no migration created, no DB
  command or test run, and git state was not touched. This file is the only write.
- **Base:** main HEAD `6cbea70` (D2 merged as `271c412`, F-pay merged as `2dc8d10`, K2 migration
  0113 on main; latest migration is `0113_governed_manual_adjustments`).
- **In-flight branches inspected** (`git diff --stat HEAD...<branch>`):
  - `prh2-h-sweeper-process`: worktree `.claude/worktrees/agent-a2a24beace7d586cc` (locked).
    **No commits and a clean worktree.** Branch tip = merge-base = `6cbea70`. H has started but
    has produced nothing yet.
  - `prh2-iwire-alert-delivery`: worktree `agent-a98cd7ccd3cb266f6`, tip `e71879c`, merge-base
    `6836319`, which is 39 commits behind main (not rebased onto D2/F-pay). 46 files, +4058/-81.
    It touches **no** `migrations/`, `cmd/`, `deploy/`, `config/` or `internal/kyc/` file. The
    `main.go` dispatcher line is withheld, as plan §12 requires.
- **Labels:** "Plan" means stated in `docs/plans/prh2-hardening-round/plan.md` or the registry/ADRs.
  "**Inference**" means my architectural reading; it is not a recorded requirement.

---

## 0. Verdict summary

| WS | Safe to run in parallel NOW with H and I-wire? | Earliest safe start |
|---|---|---|
| **E1** | **Per plan: no.** It is HELD until H merges (plan §12 row E1, `plan.md:962`; hard edge `plan.md:97,109`; Rule 5 `plan.md:138-139`). **Architecturally:** the only real collision with H is `cmd/platform-api/main.go` (1 line), plus probably `internal/config/config.go` (inference). There is no file overlap with I-wire, and no schema coupling with either. E1 *could* be implemented now under the I-wire precedent: wiring withheld, merge after H. That would use the third and last implementer slot (cap of 3, `plan.md:965`). **That is a deviation from §12, which only the orchestrator can record.** Two design questions must be settled first: §1.2 (the alert Kind) and §1.5 (the worker identity). | **Plan-conformant:** H merged. **With an orchestrator amendment:** now, with `main.go`/`config.go` withheld until H merges, merge order unchanged (H → E1 → I-wire → K3). |
| **K3** | **No. It must queue.** It overlaps I-wire on `payments/{drive,sweeper,receipt}.go`, `poll_evidence.go` and `httpserver/deposit_handlers.go`, all at the same hunks. It probably overlaps H on `sweeper.go`/`payout_sweep.go` (inference). Its P1 semantics (ADR 0101 C-7, C-20, §9 c2) depend on I-wire's alert wiring. It would also exceed the 3-implementer cap. | **I-wire merged to main** (which already implies H merged). Before code, the stale numbering and line references in ADR 0101 must be refreshed (§2.4), and the orchestrator/LF must decide whether STANDING-1 and POLL-REF-CLEAR-1 fold into 0115 or follow it (§2.10). |

Neither 0114 nor 0115 forces the other's order technically. Their object sets are disjoint (§1.4,
§2.4). The order comes only from Rule 3 (gap-free numbering at merge time) and the plan's merge
order (`plan.md:954`).

---

## 1. E1 — KYC-SUBMIT-OUTBOX-1 (migration 0114)

### 1.1 Dependency on H
- **Plan:** hard edge "E1 (after H)" (`plan.md:97`, `:109`, `:335`, `:962`). Rule 5: `main.go` is
  owned by H, and E1's worker is wired only after H merges (`plan.md:138-139`).
- **Real coupling:** file ownership only. H's touches are a new `payments/sweeper_loop.go`,
  `main.go`, `config/config.go`, the `cmd/platform-api/*_ast_test.go` files and a prod config
  checklist (`plan.md:153`). E1's touches are `kyc/*`, a new `kyc/outbox.go`, `kyc_handlers.go` and
  1 line in `main.go` (`plan.md:148`, `:962`).
- **Inference:** E1's worker loop will also want an interval setting. Every other loop takes its
  interval from `cfg` (`main.go:417`, `:432`, `:447-457`), so E1 likely touches
  `internal/config/config.go` too, and that is H's file.
- There is no semantic dependency. The KYC outbox does not use the payments sweeper or
  `PayoutKYCGate`.

### 1.2 Dependency on I-wire
- **No code or file dependency.** I-wire does not touch `internal/kyc/**`,
  `httpserver/kyc_handlers.go`, `migrations/` or `main.go`.
- **The alert requirement creates a soft dependency.** Plan §5-E1 says "an ambiguous result → retry
  … then `failed_terminal` **plus an alert**" (`plan.md:342-343`). Facts on main:
  - No KYC Kind exists. `alert_kinds` is migration-seeded only (`0110:121-157`, seeds
    `:158-182`), and `alerts.kind` is an FK to it.
  - `alert_kinds` is FORCE RLS with only a SELECT policy, plus deny triggers on UPDATE, DELETE and
    TRUNCATE (`0110:184-218`). The owner-role INSERT that a later migration would need is
    therefore denied unless that migration temporarily lifts FORCE, as `0110:184-190` explains.
    This is a security-relevant pattern change to the alerting vocabulary.
  - `alerting_session_scope()` raises for **any** `app.platform_service_id` other than
    `alert_dispatcher` (`0110:70-73`). A worker running under its own platform-service GUC
    **cannot raise from that transaction**. It must raise from a tenant-scoped transaction
    (`RaiseDetached`/`InTx` with a tenant runner).
  - A new Go Kind in `internal/alerting/kind.go` is walked by `TestMigration0110_KindsSeeded`
    (`internal/alerting/migration_0110_integration_test.go:105-130`) against a HEAD-migrated
    scratch DB. 0114 must therefore seed the Kind with matching flags.
  - **Delivery** of any alert needs I-wire's `alerting/dispatcher_loop.go` (branch only) wired into
    `main.go`. Until then, ALERT-DELIVERY-1 stays OPEN (`task-registry.md:3938`), and an E1 alert
    would be durable but `unrouted` and undelivered.
- **Options (inference; orchestrator and security decide):**
  - (a) 0114 seeds a platform-scope, `requires_subject=true` KYC Kind and E1 raises it via I-core's
    API, which is already on main. This adds `internal/alerting/kind.go` to E1's touches and needs
    security review of the FORCE-lift seeding.
  - (b) E1 emits a structured P1 log line plus an audit row on `failed_terminal`, and registers a
    follow-up next to ALERT-KINDS-DEDICATED-1 (`task-registry.md:4089`).
  - Either way, E1's alert is at most "raised, undelivered" until I-wire merges.
- **Rebase hazard (inference):** E1 merges **before** I-wire (`plan.md:954`). I-wire's
  `internal/alerting/static_wiring_test.go` (branch only) scans the whole module: every
  `RaiseGuarded` must sit inside an `alerting.InTx` closure or in a named helper allowlist. Any E1
  `RaiseGuarded` outside `InTx` will fail I-wire's test when I-wire rebases. E1 should use
  `RaiseDetached`/`RaisePostCommit`, or `RaiseGuarded` strictly inside `InTx`.

### 1.3 Dependency on D2 / F-pay (merged)
- None. D2 touched only reconciliation. F-pay (`kycgate.go`, `payout.go`, `payout_sweep.go`) consumes
  KYC enforcement outcomes but not the submission path.
- F-kyc (merged earlier) owns `kyc/enforcement.go`. E1 should **not** need to edit it: the orphan
  predicate is `orphanRowExclusionSQL = NOT (status = 'unverified' AND provider_reference IS NULL)`
  (`internal/kyc/enforcement.go:381`), and it reads only `kyc_verifications`.
- IC F3 (`reviews/identity-compliance.md:21`, `:31`; `plan.md:345-347`) is satisfied if:
  - a `pending`/`claimed` outbox row for **create** leaves the verification at
    `unverified`/`provider_reference NULL`, so the row stays an excluded orphan; and
  - a submit-outbox row never changes `kyc_verifications.status` before phase C (inference about how
    to meet F3 without touching enforcement).

### 1.4 Migration number and ordering
- **0114** (re-allocation `plan.md:938-946`; §12 `plan.md:954`, `:962`). §4's table still reads
  "0113 | E1" (`plan.md:179`), which is stale nominal numbering superseded by §11.
- **Ordering versus 0115 (K3): no technical constraint.**
  - E1 creates `kyc_submission_outbox`, plus at most an `alert_kinds` row and platform-service
    policies.
  - K3 rewrites `payment_attempts_guard()`, `ledger_transactions_governed_fence()` and
    `reconciliation_mismatches_mismatch_kind_check`, adds `payment_*` CHECKs and new tables.
  - The sets are disjoint. The only coupling is textual: both append a block to
    `deploy/init-app-role.sql`.
- **Rule 3 risk:** if an unplanned migration merges first, E1 and K3 renumber. Candidates:
  MA020-SYNC-MISMATCH-1, the POLL-REF-CLEAR-1 + STANDING-1 schema change, and
  ALERT-KINDS-DEDICATED-1. H and I-wire have no migration (`plan.md:182-183`; I-wire diff confirms
  none).

### 1.5 Tables affected; RLS, grants and roles
- **New:** `kyc_submission_outbox`. Plan columns: tenant, `verification_id`, a content-derived key
  UNIQUE, state `pending/claimed/sent/failed_terminal`, `claim_token`, attempts,
  `next_attempt_at`. Plan posture: **FORCE RLS, tenant family; the worker uses the validated
  platform-service GUC.** Down refuses while non-terminal rows exist (`plan.md:179`).
- **Platform-service identity (inference, but code-documented):**
  - `internal/db/platform_service.go:13-18` says that adding a `PlatformService` member is "an
    ADR-level decision plus a migration that widens the corresponding policy predicate".
  - The allowlist is `:46-49`; the setter is `:81`.
  - If E1 adds e.g. `kyc_submission_worker`, then:
    - `internal/db/platform_service.go` joins E1's touches (no active WS touches it);
    - the ADR 0095 §15.3 amendment must record the identity;
    - security must review it.
  - **Alternative:** a per-tenant `WithTenant` claim loop, as the payments sweeper already does
    (`internal/payments/sweeper.go:170-180`, `FOR UPDATE SKIP LOCKED` under `WithTenant`). This
    needs no new identity, but it contradicts the plan's "platform-service GUC" wording.
    Architect's lean: the per-tenant form is simpler and fits the existing pattern, but the plan
    wording binds unless the orchestrator amends it.
  - Phase C must stay `WithTenant`: `applySubmissionResult` and `kyc_verifications` are
    tenant-RLS. Outbound credential resolution is per tenant (`document_service.go:396-404`).
- **Grants:** append a least-privilege block to `deploy/init-app-role.sql`, in the `DO $$` loop
  pattern of K2's block (`init-app-role.sql:384-418`). A likely shape is `SELECT, INSERT, UPDATE`
  and no DELETE. Rule 4 applies: append-only, no role or attribute change (`plan.md:136-137`). The
  migration's own grant block must match. CLAUDE.md environment safety applies: nothing beyond the
  grant statements in the migration and init file.
- **Existing tables:** none altered (inference). `kyc_verifications` and `kyc_documents` (0040) are
  written in the same phase-A transaction, not altered.

### 1.6 Critical source files
| File | Why |
|---|---|
| `internal/kyc/document_service.go:286-292` (deferral comment), `:351` `SubmitVerification`, `:425` provider call | Phase A writes the outbox row; phases B/C move to the worker |
| `internal/kyc/verification_service.go:131` `insertOrphanVerification`, `:269` `CreateVerification`, `:285` phase A, `:354` phase C | The create path ("create/submit" per `task-registry.md:4033`) |
| `internal/httpserver/kyc_handlers.go:320` | The only production caller of the inline `SubmitVerification` |
| new `internal/kyc/outbox.go` | Claim (SKIP LOCKED + claim token), retry with backoff (T-1 clock, QA F2 `reviews/qa.md:13`), terminal state |
| `cmd/platform-api/main.go` (1 line, after H), probably `internal/config/config.go` | Worker wiring |
| `internal/db/platform_service.go` (conditional, §1.5); `internal/alerting/kind.go` (conditional, §1.2) | |
| `migrations/0114_*.{up,down}.sql`, `deploy/init-app-role.sql` | |

### 1.7 Shared files likely edited by active work
| File | H | I-wire | E1 | Overlap |
|---|---|---|---|---|
| `cmd/platform-api/main.go` | owner (Rule 5) | withheld dispatcher line, after H | 1 line | **Real.** Serialized by plan: H, then E1 |
| `internal/config/config.go` | owner (`plan.md:153`) | — | probably (inference) | **Real** if E1 adds an interval |
| `cmd/platform-api/*_ast_test.go` | edits | — | possibly (`main_construction_ast_test.go:103` pins provider construction in `registrations.go`) | Possible |
| `internal/kyc/**`, `kyc_handlers.go` | — | — | owner | None |
| `internal/alerting/*` | — | edits `dispatcher.go`, `metrics.go`, new `dispatcher_loop.go` | `kind.go` only under option (a) | No same-file overlap: I-wire does not touch `kind.go` |
| `deploy/init-app-role.sql` | — | — | append | K3 also appends later; textual only |

### 1.8 Existing tests and pins a 0114 would trip
- `internal/jurisdiction/migration_0077_integration_test.go:256-277` and `migration_0075_*:508,688,760`
  roll back the **whole chain above 0099** on a scratch DB, with the count derived dynamically.
  **0114 down must succeed on an empty scratch DB.** The refusal must trigger only on non-terminal
  rows.
  - The 0077 exact policy whitelist (`:185-192`, 11 tuples on `tenants`/`licences`/`jurisdictions`)
    trips only if E1 adds a policy to those tables. It should not.
- `internal/alerting/migration_0110_integration_test.go:110` (`TestMigration0110_KindsSeeded`)
  trips if a Go Kind is added without the 0114 seed.
- `internal/alerting/static_dispatcher_identity_test.go` trips if KYC code references
  `db.ServiceAlertDispatcher`.
- `internal/txscope/no_provider_call_in_tx_closure_static_test.go:75` flags `SubmitVerification`
  and `CreateVerification` lexically inside any `pgx.Tx` closure. The worker's phase B must be
  outside every closure.
- Behavioural tests that assume a **synchronous** submit at upload will need deliberate updates:
  - `internal/httpserver/kyc_round3_http_test.go:39-119` (a failing inline submit after upload);
  - `internal/kyc/kyc_two_phase_integration_test.go`;
  - `recording_tx_integration_test.go`.

  These are expected changes, not regressions. They must not be weakened: IC condition 2,
  "ambiguous leaves status unchanged" (ADR 0095 §15.3 row, line 2856), must still be asserted.
- I-wire's `static_wiring_test.go` (branch only; see §1.2) trips at I-wire's rebase.

### 1.9 Verdict (E1)
- **Plan-conformant answer: queue until H merges.**
- **Architectural opinion:**
  - Parallel implementation now is **sound** if:
    - (i) `main.go` and `config.go` edits are withheld until H merges, exactly as I-wire does;
    - (ii) the orchestrator records the §12 amendment;
    - (iii) §1.2 (alert option) and §1.5 (worker identity) are decided up front, because both widen
      E1's touches into `internal/alerting/kind.go` and/or `internal/db/platform_service.go`;
    - (iv) the 3-implementer cap is respected (H + I-wire + E1 = 3).
  - Merge order stays H → E1 → I-wire.

### 1.10 Reviewers and linked follow-ups (E1)
- **Plan reviewers:** SEC, ARCH, CR, QA (`plan.md:148`, `:352`). The owner is identity-compliance,
  and its IC F3 condition must be closed before E1 is marked done
  (`reviews/identity-compliance.md:31`). The QA W3 checklist applies (`reviews/qa.md:51-58`).
- **Inference:** security must explicitly review any new platform-service identity or alert-Kind
  seeding. Devops should review the loop wiring with H's pattern.
- **DoD:** the ADR 0095 §15.3 amendment (including IC F3 and the permanent-503 mechanism noted in
  ADR 0095 around line 3065), the runbook entry "KYC outbox stuck", and HANDOVER (`plan.md:354`).
- **Follow-ups:**
  - KYC-SUBMIT-OUTBOX-1 (`task-registry.md:4033`) closes against MOCK. A real vendor stays
    PROVIDER DEPENDENT.
  - **KYC outbox alert delivery → ALERT-DELIVERY-1** (`:3938`): undelivered until I-wire wires the
    dispatcher. Real recipients wait on HD-PRH2-4-OPS.
  - Under option (b), a dedicated KYC Kind follows ALERT-KINDS-DEDICATED-1 (`:4089`).
  - Not linked to E1: MA020-SYNC-MISMATCH-1, PAY-RECON-POLL-REF-CLEAR-1, STANDING-1.

---

## 2. K3 — capabilities part 3: force-resolve M1/M2 (migration 0115)

### 2.1 Dependency on H
- **Plan:** the PAY lane is strictly serial, E2 → C → D → F-pay → H → I-wire → K3
  (`plan.md:96`, `:920`, `:936`). K3 is last by design (`plan.md:963`).
- **Semantics:** H activates payout dispatch, resend and resolve in production (`plan.md:435-439`).
  K3's C-12 race (M2 against a sweeper or callback success; ADR 0101 §6.3 "Race with a sweeper or
  callback") must be tested against the sweeper H puts into production. The sweeper's `RunOnce`
  already exists, so tests can run pre-H.
- **Files (inference):** H's "non-active tenant: in-tx status read, no new dispatch" rule
  (`plan.md:446-460`, MUT at `:473-474`) almost certainly lands in `payments/sweeper.go`
  (`processCreated`, `:270-300`) and/or `payout_sweep.go`. K3 edits `sweeper.go`, and probably
  payout ingress, for `ValidatePaymentReference` (`plan.md:936`; ADR 0101 §7).

### 2.2 Dependency on I-wire
**Hard, on both files and semantics.**
- **Same files and same hunks.** I-wire edits:
  - `drive.go` (`driveCreatedAttempt` ~210-241, `parkDepositAttempt` ~398-410);
  - `sweeper.go` (`processViaQueryStatus` ~368-414, moved under `alerting.InTx`);
  - `receipt.go` (`applyResolvedReceiptEvidence` ~753-833: the T15, T13 and callback-mismatch
    dispute sites, which include the payout dispute reasons ADR 0101 allow-lists);
  - `poll_evidence.go` (4 lines);
  - `httpserver/deposit_handlers.go`.

  K3 must edit the same functions to add reserved-prefix refusal at every ingress (ADR 0101 C-9,
  `0101:474`) and the M2 allow-list.
- **Static pins.** I-wire's `static_wiring_test.go` pins `sweeper.go:processViaQueryStatus` and
  `drive.go:driveCreatedAttempt` as `InTx` owners. K3 must build on that shape.
- **P1 semantics** need I-wire's wiring:
  - C-7 (T14 P1 after "declare not paid");
  - **C-20** ("late provider decline after declare paid … **P1 raised**", `0101:487`);
  - §9(c2) `pay_declared_paid_compensated_but_paid` "P1" (`0101:400`).

  The new standing reconciliation kinds surface as P1s only through I-wire's
  `reconciliation.payment_statement_mismatch` `RaisePostCommit` site in `scheduler.go` (§12:
  "depend on … I-wire's alert Kinds", `plan.md:963`).

### 2.3 Dependency on D2 / F-pay (merged)
- **D2 (`271c412`).** `reconciliation/payment_statement.go` grew by +224 lines. ADR 0101's facts
  (`0101:58-60`: `:949`, `:950`, `:977-979`, `:1002`) are **stale**; for example, the
  `multiple_success_for_intent` comment is now near `:155`. K3's §4 (LF-3 annotate-only) and
  §9 (a)–(e) must be built on D2's bound/unbound runtime rule and the widened
  `pay_captured_unposted`. D2's gap-asserting test (`d2NoCU`,
  `prh2_d2_parked_capture_integration_test.go:305`) must not be flipped by K3; it belongs to
  STANDING-1.
- **F-pay (`2dc8d10`).** `payout.go` (+81) and `payout_sweep.go` changed. ADR 0101's
  `payout.go:392-395`, `:1118`, `:1151` and `:1178-1181` are stale; `providerref.Validate` now sits
  at `payout.go:453`.
- **Inference, open question for payments/LF:** whether the payout KYC gate (ADR 0096 §24)
  applies to M2 "declare paid". ADR 0101 has no KYC mention, and §12 cites "F-pay's payout gate" as
  a semantic dependency (`plan.md:963`).
- **C/D ingress.** D introduced `poll_evidence.go`, with a `providerref.Validate` call at `:175`.
  It is not in K3's recorded touch list (`plan.md:936`) but is a deposit-poll ingress under C-9.
  **Touch-list gap (inference):** add `payments/poll_evidence.go` to K3's touches.

### 2.4 Migration number and ordering
- **0115** (`plan.md:946`, `:954`, `:963`).
- **ADR 0101 is internally stale.** It says "migration 0114" (`0101:14`, `:23`, `:78`, `:186`,
  `:253`, `:263`, `:338`, `:377`, `:474`, `:482`) and "0112's" kind CHECK and fence (`:377`,
  `:386-389`). Under the re-allocation those are **0115** and **0113** (K2 is 0113 on main:
  `ledger_transactions_governed_fence` at `0113:1437`, kind CHECK at `0113:1850-1851`). §11 says the
  ADR references "mean their new numbers" (`plan.md:948`); the K3 implementer must apply that
  literally. Specifically, **0115 down restores 0113's fence and kind CHECK**, or whatever migration
  immediately precedes it at merge.
- **0114 versus 0115:** no technical ordering constraint (disjoint objects; §1.4).
- **Constraints from other migrations:**
  - 0115's kind CHECK must be "a strict superset of the latest definition at merge" (`0101:377`).
    If the POLL-REF-CLEAR-1 + STANDING-1 schema change, or any other kind-widening migration,
    merges first, 0115 must include its kinds, and 0115 down must restore *that* migration's CHECK.
  - The guard diff is defined against **0107's** body (`0101:338-367`). No later migration has
    touched `payment_attempts_guard` (only `0101` and `0107` define it), so that base is still
    valid at `6cbea70`.

### 2.5 Tables affected; RLS, grants and roles
- **New tables** (FORCE RLS; families T and A only, no P; no `FOR ALL`; TRUNCATE and DELETE
  denied; no SECURITY DEFINER; SQLSTATE class `MR`; `0101:265-267`):
  - `payment_manual_resolution_codes` (family R, reference data);
  - `payment_manual_resolutions`;
  - `payment_manual_resolution_approvals`.
- **Altered:**
  - `payment_attempts_guard()`: the exact F12 diff;
  - new CHECKs on `payment_attempts`, `payment_provider_events` and `payment_statement_lines`
    (reserved prefix);
  - new trigger `ledger_transactions_reserved_prefix_guard`, all sessions;
  - `ledger_transactions_governed_fence` replaced (ADR 0099 §6.6 (a)+(b)+(c));
  - acting-session UPDATE policies on `payment_attempts`, `withdrawal_requests` and
    `deposit_intents` (`0101:236-247`), plus acting SELECT on `withdrawal_requests`;
  - `reconciliation_mismatches_mismatch_kind_check`, which gains `pay_declared_paid_unconfirmed`,
    `pay_declared_not_paid_but_paid` and `pay_declared_paid_compensated_but_paid`;
  - an up-time refusal if any reserved-prefix value exists.
- **Grants:** append an `init-app-role.sql` block (K2 loop pattern, `:384-418`):
  - codes table: SELECT;
  - resolutions: SELECT, INSERT, UPDATE (state machine);
  - approvals: SELECT, INSERT.

  It must match the migration's own grant block. Rule 4 applies.
- **Session family:** acting sessions via `db.WithPlatformActingInTenant` (`internal/db/tenant_rls.go:389`,
  K1). Per HD-PRH2-6 they are valid only with an explicit tenant grant (`plan.md:899`).

### 2.6 Critical source files
- `internal/payments/attempt.go` (866 lines); new `internal/payments/manual_resolution.go`.
- `internal/providerref/providerref.go`: `ValidatePaymentReference` and the reserved
  `platform-operator-declared:` prefix (`plan.md:924`, `:936`). Today's `Validate` is at `:100`.
- `internal/payments/{drive,sweeper,payout,receipt}.go`, plus `poll_evidence.go` (§2.3 gap).
- The callback handlers (`internal/httpserver/deposit_handlers.go`; the withdrawal callback path
  through `ReceiveVerifiedCallback`) and the statement import (`reconciliation/payment_statement.go:340`,
  `:395`).
- `internal/reconciliation/payment_statement.go`: F13 detail text and the §9 rules.
- A new route file. `backoffice/src/auth/permissions.ts`.
- Called, **not edited**: `withdrawal.Complete`/`Fail`/`LockSubmittedForResolution`
  (`plan.md:159`); `internal/capability` (`capability.go:40-41` already defines the
  `payment_force_resolve:{request,approve}` capabilities); K2's
  `financial_policy_required_approvals` (`0113:631`, already special-cases `payment_force_resolve`
  at `:655`).

### 2.7 Shared files likely edited by active work (overlap analysis)
| File | H | I-wire | K3 | Overlap |
|---|---|---|---|---|
| `payments/sweeper.go` | likely (tenant-status gate; inference) | `processViaQueryStatus` → `InTx` | ingress validation | **Three-way** |
| `payments/drive.go` | possibly (no `created` dispatch for a non-active tenant; inference) | `driveCreatedAttempt`, `parkDepositAttempt` | deposit sync ingress | **Real** |
| `payments/receipt.go` | — | payout/deposit dispute sites ~753-833 | the same sites (allow-list, prefix) | **Real, same hunks** |
| `payments/poll_evidence.go` | — | 4 lines | deposit poll ingress | **Real** |
| `payments/payout.go`, `payout_sweep.go` | `payout_sweep.go` likely (inference) | — | payout sync/poll ingress | Possible with H |
| `httpserver/deposit_handlers.go` | — | edited | callback ingress | **Real** |
| `reconciliation/payment_statement.go` | — | — (I-wire edits `scheduler.go`) | §4, §9 | None active. D2 is merged. POLL-REF-CLEAR-1/STANDING-1 would collide later (§2.10) |
| `deploy/init-app-role.sql` | — | — | append | Textual with E1 |
| `main.go` | owner | dispatcher line | none expected | — |

### 2.8 Existing tests and pins a 0115 would trip
- `internal/payments/migration_0107_integration_test.go:406` (`TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD`)
  runs on HEAD. The deposit outcomes must stay identical; ADR 0101 C-17 and C-17b add the
  text-diff test and the exhaustive deposit matrix.
- The `internal/payments/migration_0101_integration_test.go` HEAD behaviour tests (`:293`, `:389`,
  `:554`) assert payout transitions are refused without valid evidence. They must still pass,
  because no executing resolution exists in them.
- Full-chain rollback tests (`jurisdiction/migration_0075`, `0077`) require that 0115 down succeeds
  on an empty scratch DB and restores 0113's objects exactly.
- `internal/adjustment/migration_0113_integration_test.go:81-135` (scratch through 0113) is
  unaffected. HEAD-migrated K2 tests (`acting_policies_integration_test.go`, `b11_b17_*`,
  `layered_*`, and `internal/db/k2_gates_integration_test.go`) exercise the fence and acting
  policies that 0115 **replaces**. Any test asserting the 0113 fence's exact refusal set may need
  updating, which needs LF agreement.
- `internal/reconciliation/migration_0097/0098_integration_test.go` (`:125`, `:117`) match the
  constraint *name* `mismatch_kind_check`, so the name must be kept.
- Validate-shaped expectations (`payout_dispatch_integration_test.go:342`,
  `poll_amount_integration_test.go:955`, `payout_dispatch_fixround_test.go:622`) will change if
  `ValidatePaymentReference` alters error field or text.
- I-wire's `static_wiring_test.go`, once merged: every `RaiseGuarded` K3 adds must be inside `InTx`
  or in the helper allowlist.
- `d2NoCU(... "an unbound park with no line this run")` must **not** be flipped by K3
  (STANDING-1's job, `task-registry.md:4098`).

### 2.9 Verdict (K3)
**It must queue.** Running it in parallel now would put three implementers on `sweeper.go`, and
two each on `drive.go`, `receipt.go`, `poll_evidence.go` and `deposit_handlers.go`. It would test
P1 behaviour (C-7, C-20, c2) against unmerged alert wiring and break the 3-implementer cap.

Earliest safe start:
- **I-wire merged** (which implies H merged);
- ADR 0101's numbering and line references refreshed against post-I-wire main (§2.3, §2.4);
- the §2.10 sequencing decision recorded.

**Optional, not in the plan (inference):** a pre-start of the DB- and domain-only slice could run
once a 3rd slot is free. That slice is 0115, `attempt.go`, `manual_resolution.go`,
`providerref.go` and `payment_statement.go`, none of which is touched by H or I-wire. Ingress edits
would be withheld until I-wire merges. Its value is low against the rebase risk on the guard and
fence, and it competes with E1 for the slot, so I do not recommend it.

### 2.10 Reviewers and linked follow-ups (K3)
- **Plan reviewers:** SEC, LF, CR, QA, ARCH (`plan.md:159`, `:690`). Payments implements, and LF
  owns the financial invariants (`0101:15-17`). The W4 exit requires the QA W4 checklist plus **LF
  and security sign-off** (`plan.md:110`). Guard tests run on a HEAD-migrated scratch DB (LF-18).
  POP is required only for the K1 enum, which is done.
- **DoD:**
  - ADR 0101 §17 docs;
  - the ADR 0095 §4.8, §4.3, §28.9 and INV-IO-7 amendments (orchestrator, `0101:414-437`);
  - ADR 0082 A8;
  - `permissions.ts`;
  - the runbooks.
- **Follow-ups:**
  - **PAY-RECON-POLL-REF-CLEAR-1** (`task-registry.md:4097`). ADR 0095 §35 must say "M1 only
    acknowledges" a standing `poll_reference_mismatch`, so K3's LF-3 annotation must cover that
    reason too. It ships with STANDING-1 under one LF-signed schema change that touches
    `payment_statement.go` and payment statement evidence. **Collision with K3** on the same file
    and on `payment_statement_lines` (K3 adds its prefix CHECK there).
  - **PAY-RECON-PARKED-CAPTURE-STANDING-1** (`:4098`). Its "persisted line evidence" is explicitly
    "the same approach as LF ruling 5(c)(d)", which is ADR 0101 §9(c)/(d)'s "any persisted import"
    rule. K3 and STANDING-1 therefore need the same persisted-evidence substrate. **Decision needed
    (orchestrator with LF):** fold it into 0115 (a K3 scope change), or land it immediately after K3
    as 0116. If it lands before K3, K3 renumbers and rebases its kind CHECK.
  - **MA020-SYNC-MISMATCH-1** (`:4101`). It widens `player_open_payment_exposure` (`0113:706-719`).
    K3 adds the M2-compensating-credit causation arm to K2's catalogue logic (ADR 0100:294-295,
    `:914`). These are different functions, but both are K2-family CREATE OR REPLACE edits;
    whichever lands second must be built on the other's body. MA020 refuses **every** `credit_player`
    while exposure is open (ADR 0100:212). Whether an M2 compensating credit should be subject to
    MA020 is an LF question (inference).
  - **ALERT-DELIVERY-1** (`:3938`): the K3 P1s are undelivered until the dispatcher is wired.
    Recipients wait on HD-PRH2-4-OPS.
  - Also: PAYOUT-AMOUNT-DISPUTE-1, WITHDRAWAL-REVERSAL-1 (`0101:567-568`), and
    PAY-CALLBACK-MISMATCH-BIND-1 (`:4092`), which touches `receipt.go` ~823, the same site as
    I-wire and K3.
  - HD-PRH2-8 interim: at least one independent approver (`0101:572-573`).

---

## 3. Items for the orchestrator (no decision taken here)
1. **E1 parallel start.** Amend §12 to allow it now, with `main.go`/`config.go` withheld, or keep
   it HELD until H merges.
2. **E1 alert mechanism.** Option (a), a new Kind seeded in 0114 (security review of the FORCE-lift
   seed), or option (b), log/audit plus a follow-up.
3. **E1 worker identity.** A new `PlatformService` member (ADR-level, `platform_service.go:13-18`)
   or a per-tenant `WithTenant` loop (a plan-wording change).
4. **K3 touch list.** Add `payments/poll_evidence.go`; consider `payout_sweep.go`.
5. **K3 versus STANDING-1 + POLL-REF-CLEAR-1.** Fold into 0115, or sequence after it (LF to rule).
6. **ADR 0101 refresh.** Apply 0114→0115 and 0112→0113 and refresh the line references before K3
   code starts.
