# PRH-2 — Provider-Independent Hardening Round + Continuous Handover Readiness — Planning Gate

Status: **PLANNING GATE, revision 2 — NOT AUTHORIZED FOR IMPLEMENTATION.**
Author: architect. Date: 2026-09-28.

| Rev | Base | Change |
|---|---|---|
| 1 | `559483a` (commit `a3547f5`) | Initial plan |
| 2 | `e3008d3` | Incorporates all six plan reviews under `reviews/`: security, ledger-finance, qa, payments, identity-compliance and product-owner-proxy, each **APPROVE WITH CONDITIONS**. Every finding is mapped in §10. The ledger-finance rulings LF-1, -2, -3, -9, -10, -11, -13, -14 and -15 are binding design. |

**Inputs:**
- CLAUDE.md and MASTER-BUILD-PROMPT.md;
- `docs/active-stage.md` (the 2026-09-28 note);
- `docs/governance/payment-readiness-completion-report.md` §§3, 5, 7, 8 and 11;
- ADR 0098 (the human decisions HD-0095-1 and LEDGER-MANUAL-ADJ-4EYES-1);
- `docs/governance/task-registry.md`;
- the review records in `docs/plans/prh2-hardening-round/reviews/`.

**Verification:**
- Read-only grep and read of the code at `559483a`. The code is identical at `e3008d3`, which only
  adds docs.
- `go build ./...` and `go vet` passed on the payments, casino, auth and `cmd` packages.
- For revision 2, I re-checked the corrected facts in code: the HR-9 removal (`ledger/ledger.go:26-29`),
  `lockorder.go:355-357`, the legacy call-site count, and the alert sites at `orchestrator.go:989,1021`.
- No DB access; no tests were run. Scope is MOCK and local only. Nothing in this plan is a regulatory
  or licensing claim.

**Binding principle (human, ADR 0098 §4):**
- Jurisdiction-specific requirements are explicit configuration: versioned, effective-dated, audited,
  and scoped by jurisdiction, tenant or brand. They are never global invariants and never hard-coded
  thresholds.
- Absent policy or evidence means **fail closed**.
- Regulatory configuration stays separate from ledger correctness.

**Precondition (QA F6):** before W0 sign-off, `code-reviewer` independently re-verifies the §1
evidence table.

---

## 1. Verified state (A–L)

Every item below is verified as still open in the code at `559483a`.

| WS | Registry id | State | Evidence (file:line) |
|---|---|---|---|
| A | CAS-REVOKE-CONSUMED-1 | **OPEN** | `0042_…up.sql:58-60`: no change out of `consumed/expired/revoked`. `casino/launch.go:377-383` revokes `active` only and records no `prior_status`. The characterization test asserting a bet on a consumed session is accepted is at `casino/launch_two_phase_integration_test.go:703-795` (QA F1). |
| B | CAS-PLAY-BOOTSTRAP-1 | **OPEN** | `ResolveLaunchToken` (`launch.go:279`) has no non-test caller. It consumes on the token hash alone, with no provider/mode/asset predicate, and its lazy expiry writes `expired` (`:290-325`, `:311`) (S-5). |
| C | PAY-DEP-REF-VALIDATE-1 | **OPEN** | Three gaps: <br>• The reference is not validated: `drive.go:250-273` / `:283-310`, whereas payout validates at `payout.go:392-395,1178-1181`. <br>• There is no amount evidence on a sync success: `DepositResult` has no `Amount`/`AssetCode` (`types.go:471-495`), a sync success posts `attempt.Amount` (`drive.go:260-264`), and the intent comparison at `orchestrator.go:1108` is tautological (LF-5). <br>• A Pending result with an empty reference reaches the 0099 CHECK (`0099:136-137`) as an untyped error (S-9). |
| D | PAY-POLL-AMOUNT-1 + FH7-06 | **OPEN** | `sweeper.go:513` posts without an amount/asset/reference comparison; the only check is at `:471`, in the already-succeeded branch. The posting and the tombstone lookup both use the **echoed** `res.ProviderReference` (`:~497,513`). With an empty echo, the tombstone check is skipped and the posting key becomes `"<provider>:"`, because `ledger.Post` accepts an empty `ProviderTxID` (`lockorder.go:346`) (LF-4, S-6). |
| E1 | KYC-SUBMIT-OUTBOX-1 | **OPEN** (deferred, accepted) | `kyc/document_service.go:285-292`; there is no outbox table. |
| E2 | PROV-OUTBOUND-CRED-1-LEGACY-PATH | **OPEN** | `orchestrator.go:555` → `:674` → `:700` → `provider.Deposit` at **`:714`**, and → `:762` → `:793` → `provider.QueryStatus` at **`:797`**. Both calls hold a `pgx.Tx`, and there is no non-test caller. There are **20 test call sites in 10 files**, including `migration_0107_integration_test.go:1867` (LF-16). This chain is the only path that reaches `ErrDepositAlreadyPostedForIntent`. `RecordDepositMultipleSuccessRefusal` (`:1501`) has this chain as its only caller. |
| E3 | Provider call in a financial tx: sweep | **Done (read-only)** | Only E2's `:714` and `:797`. The other adapter call sites are all txscope-guarded or outside a tx: `drive.go:251`, `payout.go:385,1174`, `sweeper.go:354`, `casino/orchestrator.go:632`, `kyc/verification_service.go:335` and `kyc/document_service.go:425`; email `credential_handlers.go:159,321`. The sweep also found a **non-financial** case: `sportsbook/catalogue.go:52-56` calls `provider.Catalogue()` inside a tx → register **SB-CATALOGUE-IO-1** (Low; before a real sportsbook adapter; not planned). |
| F | KYC-ENF-OUTAGE-1, -DECISION-ROWS-1, -TESTPINS-1 | **OPEN** | `kyc/enforcement.go:465-468` → `withdrawal.go:397-417` (a 25P02 in the aborted tx). `payments/kycgate.go:40-68`, `payout.go:285-323` and `payout_sweep.go:99-130` record no decision. `withdrawal.go:1046-1051` has no B6 pin and no N5 guard. |
| G | KS-AUDIT-TENANT-1, KS-CAS-DISCRIM-TEST-1 | **OPEN** | `payments_kill_switch_handlers.go:246-251` writes the audit tenant as `uuid.Nil`, and `0014:32-40` is one `FOR ALL` dual-scope policy. The untested branch is `drive.go:157-163`. |
| H | CP-W1 | **OPEN** | `NewSweeper` (`sweeper.go:102`) has no caller. `main.go:394-447` wires other loops only. **`Sweeper` also drives payout T2/T12/resolve** (`payout_sweep.go`, dispatched via `RunOnce` at `sweeper.go:124`), so H activates payout dispatch as well as deposits (payments F1). |
| I | PAY-P1-MULTISUCCESS-ALERT-1 + alert delivery | **OPEN** | Every alert is log-only. Live sites (LF-7): <br>• `auditMultipleSuccessForIntent` (`orchestrator.go:989-1024`, log at `:1018`), inside the T10/T13d evidence tx; <br>• `payments_deposit_intent_index_backstop_fired` (`:1021`); <br>• the drift P1 (`reconciliation/scheduler.go:333-341`); <br>• the kill-switch engage alert (`payments_kill_switch_handlers.go:360`); <br>• the `*_integrity_alert_*` lines in the deposit, casino, casino-play and sportsbook-settlement handlers. <br>The `:1509` site dies with E2. There is no alert table. |
| J | PRH-I4-METRICS-1 | **OPEN** | `observability/metrics.go:17` exists; there is no admission counter. |
| K | HD-0095-1, LEDGER-MANUAL-ADJ-4EYES-1, ADR 0095 M1/M2 | **DECIDED (0098); NOT IMPLEMENTED** | There is no API and no table. `TxManualAdjustment` and `AccountManualAdjustment` (`ledger.go:76,115`) are used only by tests. `lockorder.go:355-357` is the manual-adjustment **reason-code validation** at the `ledger.Post` boundary (mirroring 0051's CHECK), not a lock-order rule (LF-17). The RBAC inventory is in §5-K. |
| L | HANDOVER-1 (new) | **OPEN** | There is no root README, no docs index, no mock-vs-real matrix and no secret-NAMES inventory. |

**Registered and not planned:**
- PAY-SEC-LAUNCH-1, M5, WEBHOOK-EDGE-1, DEVOPS-0107-INDEX-WINDOW-1, PAY-RECON-N1, TEST-SCRATCH-LEAK-1,
  CI-BILLING-1/F-POOL-1 K1, BRANCH-PROTECTION-1, SB-CATALOGUE-IO-1.
- **CAS-REVOKE-BET-RACE-1** and **PAY-SEC-LAUNCH-1** stay launch-blocking and must be listed as open
  in the W5 report (security §5).

**Folded in as ride-alongs (payments F2):**
- PAY-F3SM-TEST-1 and PAY-SWEEP-CAS-NOISE-1 go into D.
- RECON-PAYOUT-LIVE-TEST-1 goes into H's DoD.

## 2. Dependencies, waves and blocking conditions

**Hard edges:**
- A → B (security gate (i); ADR 0103 accepted).
- The K lane is K1 → K2 → K3. M1 is **evidence-only** (LF-2), so K3's dependency on K2 is for the
  capability, policy and approval framework only, not for posting.
- F-pay → H: the payout-gate fixes are a hard precondition for H's payout half (payments F1).
- E2 → I-wire: the live P1 site must be the post-E2 one.
- C and D must land before any real PSP; E2 before real PSP registration; E1 and F before any real
  KYC vendor. I-core → I-wire must land before relying on any P1; H before any payout launch.

```
W0 docs/ADRs/registry ──► W1 ──► W2 ──► W3 ──► W4 ──► W5 stop
[CAS]  A ─────────────► B (ADR 0103)
[PAY]  E2 ► C(+G2) ► D ► F-pay ► H ► I-wire ► K3
[KYC]  F-kyc ────────────────► E1 (after H)
[SEC]  G1
[OBS]  I-core ► J
[RBAC] K1 ► K2 ──────────────────────────────► (K3 in PAY lane)
        ▲ BLOCKED on ADR 0099/0100 + HD-PRH2-1/2/6/7
```

| Wave | Content | Entry / blocking conditions | Exit |
|---|---|---|---|
| **W0** | ADR drafts 0099–0104 (§6), HANDOVER skeleton, registry pass | — | The QA W0 checklist (`reviews/qa.md` "Per-wave gate checklist"), plus: <br>• security reviews every ADR, and ledger-finance reviews 0099–0101; <br>• §1 is re-verified by code-reviewer (QA F6); <br>• the registry pass: add SB-CATALOGUE-IO-1, HANDOVER-1, CAP-GRANT-1 and ALERT-DELIVERY-1; correct **HD-KYC-1..7 → HD-KYC-1..8** (IC F2); fix the KYC-ENFORCE-1 prose (DenyForCompliance is wired); mark the ride-alongs folded. |
| **W1** | A, E2, F-kyc, G1, I-core, K1* | *K1 **blocked** until: <br>• HD-PRH2-2 is answered in its re-framed form; <br>• HD-PRH2-6 is answered, or platform scope is limited to platform-owned objects; <br>• ADR 0099 is accepted with S-1, S-3, S-4 and S-11; <br>• product-owner-proxy confirms the four-capability closed enum. | QA W1 checklist |
| **W2** | B, C+G2, J, K2* | B: A merged, ADR 0103 accepted, and **no as-is reuse of `ResolveLaunchToken`**. <br>*K2 **blocked** until K1 is merged, HD-PRH2-1 and HD-PRH2-7 are answered, and ADR 0100 is accepted with S-2, S-12, S-13 and LF-9..LF-14. | QA W2 checklist |
| **W3** | D, F-pay, H, E1 | H: F-pay merged. E1: H merged. | QA W3 checklist, plus H's no-tx-during-provider-call test |
| **W4** | I-wire, K3* | I-wire: E2 and I-core merged; S-7 item 2 settled. <br>*K3 **blocked** until K2 is merged and ADR 0101 is accepted with LF-1, LF-2, LF-3, LF-15, LF-18 and S-12. | QA W4 checklist; ledger-finance and security sign-off |
| **W5** | Final gate and completion report | — | QA W5 checklist. Every result is PASS/FAIL/FLAKE/NOT RUN/BLOCKED; CI-BILLING-1 is stated plainly, and local runs are never labelled CI. **Stop.** |

The launch flags are unchanged. PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking until HD-PRH2-4
routes P1s to a human, even after I merges.

## 3. Ownership, touched files and merge order

- **Rule 1:** one writer per critical file at a time. The critical files are:
  - `payments/{orchestrator,receipt,sweeper,drive,payout,payout_sweep,attempt,deposit_v2,kycgate,types,mock}.go`
  - `internal/ledger/*`
  - `withdrawal/withdrawal.go`
  - `casino/{launch,orchestrator}.go`
  - `auth/permission.go`
  - `httpserver/{routes,server}.go`
  - `cmd/platform-api/{main,wiring,registrations}.go`
  - `deploy/init-app-role.sql`
  - migration numbering
  - `task-registry.md`, `active-stage.md`, `progress.md`
  - the ADR amendment sections

  The PAY lane is strictly serial.
- **Rule 2:** the orchestrator is the only writer of the registry, active-stage, progress, the
  HANDOVER index rows and the ADR amendment sections.
- **Rule 3:** migration numbers are fixed at merge time, gap-free. An unplanned migration takes the
  next free number, and unmerged downstream branches renumber.
- **Rule 4:** `init-app-role.sql` gets append-only least-privilege grants. No role, password or
  attribute change.
- **Rule 5:** `main.go` is owned by H. E1's worker and I's dispatcher are wired only after H merges
  (E1 at its merge; the dispatcher in I-wire).

| WS | Owner (reviewers) | Touches | Lane / wave |
|---|---|---|---|
| A | casino (SEC, CR, QA) | `0108`, `casino/launch.go`, `casino/orchestrator.go`, casino tests incl. `launch_two_phase_integration_test.go` | CAS / W1 |
| B | casino (SEC, ARCH, CR, QA, POP) | new `casino/bootstrap.go`, `casino_routes.go`, `casino/mock.go`, `routes.go` (1 line) | CAS / W2 |
| C | payments (SEC, LF, CR, QA) | `drive.go`, `types.go` (`DepositResult` contract), `mock.go`, a new shared comparison helper, deposit tests | PAY / W2 |
| G2 | payments + qa (CR) | `payments/*_test.go` only | PAY, with C |
| D | payments (LF, SEC, CR, QA) | `sweeper.go`, `ledger/lockorder.go` (empty `ProviderTxID` refusal only), poll tests | PAY / W3 |
| E1 | identity-compliance (SEC, ARCH, CR, QA) | `0113`, `kyc/{document_service,verification_service}.go`, new `kyc/outbox.go`, `kyc_handlers.go`, 1 line in `main.go` | KYC / W3 |
| E2 | payments (LF, SEC, CR, QA) | `orchestrator.go` (delete the chain and `RecordDepositMultipleSuccessRefusal`), 10 test files | PAY / W1 |
| F-kyc | identity-compliance (SEC, CR, QA) | `kyc/enforcement.go`, `withdrawal.go:397-417,1046-1051`, `withdrawal_handlers.go`, ADR 0096 N2 row | KYC / W1 |
| F-pay | payments + identity-compliance (SEC, CR, **QA**) | `kycgate.go`, `payout.go` (T1p allow), `payout_sweep.go:99-130` | PAY / W3 |
| G1 | architect + security (payments, CR, QA) | `0109`, `payments_kill_switch_handlers.go`, `audit/audit.go`, audit read handler | SEC / W1 |
| H | payments + devops (LF, SEC, CR, QA) | new `payments/sweeper_loop.go`, `main.go`, `config/config.go`, `cmd/platform-api/*_ast_test.go`, prod config checklist | PAY / W3 |
| I-core | devops + architect (SEC, LF, CR, QA) | new `internal/alerting/` (dispatcher function unwired), `0110` | OBS / W1 |
| I-wire | payments + casino + devops (SEC, LF, CR, QA) | `orchestrator.go:989-1024,1021`, `reconciliation/scheduler.go:333-341`, `payments_kill_switch_handlers.go:360`, integrity-alert sites, dispatcher line in `main.go` | PAY / W4 |
| J | security + devops (CR) | `webhook_admission.go`, `internal/observability` | OBS / W2 |
| K1 | architect → backend + security (SEC, LF, CR, QA, POP) | `0111`, `auth/permission.go`, new `internal/capability/`, `capability_routes.go`, `routes.go` (1 line) | RBAC / W1 |
| K2 | ledger-finance (SEC, CR, QA, ARCH) | `0112`, new `internal/adjustment/` (calls `ledger.Post` and `LockProjectionsForPosting`; no `ledger` edits), new route file | RBAC / W2 |
| K3 | payments + ledger-finance (SEC, CR, QA, ARCH) | `0114`, `payments/attempt.go`, new `payments/manual_resolution.go`. **No edits to `withdrawal.go`**: it only calls `withdrawal.Complete`/`Fail`. If a helper is needed there, `withdrawal.go` joins K3's Touches and Rule 1 applies (payments F3). | PAY / W4 |
| L | architect + orchestrator | `docs/HANDOVER.md`, links only | W0, then every merge |

**Merge order (one at a time, full local verification between merges):**
W0 → **A (0108)** → **G1 (0109)** → E2 → F-kyc → **I-core (0110)** → **K1 (0111)** → C+G2 → B → J →
**K2 (0112)** → D → F-pay → H → **E1 (0113)** → I-wire → **K3 (0114)** → W5.

**Order under K blocking.** If the K lane is still blocked when its slot comes, the other lanes
proceed and the K migrations take the next free numbers at merge (Rule 3). The numbers in §4 assume
the nominal order.

## 4. Migration allocation (nominal, gap-free in merge order)

| No. | WS | Content | RLS family (QA F4) | Down |
|---|---|---|---|---|
| **0108** | A | Security spec verbatim: keep 0042's column block; allow exactly `consumed → revoked` with `(to_jsonb(NEW)-'status') = (to_jsonb(OLD)-'status')`. | Unchanged | Restore 0042's body verbatim |
| **0109** | G1 | `audit_log.subject_tenant_id UUID NULL` (FK tenants) with `CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL)` and an index. An extra **`FOR SELECT`** policy lets a tenant session also read rows with `tenant_id IS NULL AND subject_tenant_id = NULLIF(current_setting('app.tenant_id',true),'')::uuid`; player-scope sessions are excluded. A BEFORE INSERT trigger: when `subject_tenant_id` is set, `actor_id` must equal the validated `app.platform_admin_principal_id` (S-10). | Write side unchanged (0014 dual-scope); SELECT only ORs in | Refuse while any `subject_tenant_id` is set |
| **0110** | I-core | `alerts`: UNIQUE `(tenant_id, dedup_key)` NULLS NOT DISTINCT; severity; `tenant_id NULL` means platform, and integrity P1s carry `subject_tenant_id`; state `open/acked/resolved`; occurrence counters. `alert_deliveries`: append-only; `last_error_class` is an **enum** (S-7). | FORCE RLS, dual-scope for reads; subject-tenant read-only exposure (the G1 pattern). The dispatcher runs under the validated platform-service GUC (0106), with read on alerts and insert on deliveries only. | Refuse while rows exist |
| **0111** | K1 | `staff_capability_grants` (+ approvals). Closed capability enum with a scope map. Triggers: <br>• actor from the session GUC (S-4); <br>• grantor ≠ grantee, with distinct non-NULL `person_id`; <br>• the grantee is active and in the same tenant; <br>• grantee eligibility per HD-PRH2-2 (e.g. platform-minted only under option (d)); <br>• platform capabilities only under the validated platform GUC; <br>• append-only, except a one-way revoke under whole-row equality. | FORCE RLS, tenant + validated platform GUC (the 0105/0106 two-family pattern). **No** platform-acting-in-tenant family unless HD-PRH2-6 requires one and ADR 0099 designs it (S-3). | Refuse while rows exist |
| **0112** | K2 | `financial_approval_policies` (append-only, effective-dated; `above_threshold` requires `asset_code`, LF-12) plus a policy-change approval table (S-2). `ledger_adjustment_requests`: an immutable payload (wallet, account type, asset, direction, amount ≤ int64 max, closed reason code); the posting-shape CHECK (`player_cash` ↔ `manual_adjustment` only, LF-9/S-13); the pinned policy. `ledger_adjustment_approvals`: `payload_hash`, `decided_txid`. Triggers: <br>• distinct non-NULL Person floor; <br>• beneficiary-Person exclusion (S-12); <br>• the linked `ledger_transaction_id` is a same-tenant `manual_adjustment` (composite FK 0021:51), UNIQUE, with entries equal to the payload (LF-10); <br>• all less-specific policy rows act as a DB-enforced floor (S-2). | FORCE RLS, tenant family. A platform family only per HD-PRH2-6 and S-3. | Refuse while rows exist |
| **0113** | E1 | `kyc_submission_outbox`: tenant, verification_id, a content-derived key UNIQUE, state `pending/claimed/sent/failed_terminal`, claim_token, attempts, `next_attempt_at`. | FORCE RLS, tenant; the worker uses the validated platform-service GUC | Refuse while non-terminal rows exist |
| **0114** | K3 | `payment_manual_resolutions` (+ approvals, UNIQUE per attempt, `decided_txid`). Amend `payment_attempts_guard()` for **payout attempts only**: `{ambiguous, disputed} → succeeded` ("declare paid") or `→ declined` ("declare not paid"), with `last_evidence_kind='operator'`, admitted only with an approved resolution for that attempt and that target state in the same tx (`decided_txid = txid_current()`, 0105:243-247,324). **The deposit branches and both 0107 indexes stay byte-identical, with no state-CHECK change** (LF-1). | FORCE RLS, tenant family (platform family per HD-PRH2-6) | Restore the 0107 guard verbatim; refuse while resolutions exist. Guard tests run on a HEAD-migrated scratch DB (LF-18). |

Expected to need no migration: B (the idempotency record may need one → next free number), C, D, E2,
F, G2, H and J. Payments confirmed this for C, D, E2, F-pay and H.

## 5. Per-workstream design

**Legends:**
- Tests: **R** regression · **ADV** adversarial · **CON** concurrency · **TI** tenant isolation ·
  **AZ** authz · **FL** failure · **IDM** idempotency · **RB** rollback/recovery · **AU** audit ·
  **RLS** · **MIG** · **MUT** mutants.
- Invariants: **I1** INV-DEP-1 · **I2** append-only double-entry · **I3** DB idempotency · **I4** RLS
  and server-side tenant · **I5** no direct balance mutation · **I6** HD-LEDGER-UNALLOC-1 A-now · **I7**
  no provider I/O in a financial tx.

The detailed test and mutant lists in `reviews/ledger-finance.md` ("Required tests and mutants") and
`reviews/qa.md` (per-wave checklist) are **incorporated by reference**. The bullets below add only
what those lists don't already cover.

### 5.0 Cross-cutting test rules (binding)

- **T-1 (QA F2):** time-dependent behaviour is driven through an injectable clock, or by writing lease
  and `next_attempt_at` timestamps into fixtures. This covers the sweeper interval and lease, the
  outbox backoff, the alert-dispatcher backoff, grant `valid_until` and request expiry. Never
  `time.Sleep` and hope.
- **T-2 (QA F3):** no new wall-clock assertion. Assert on order or outcome instead. An unavoidable
  wall-clock bound is escalated as a CI-config decision before merge and never appended silently.
- **T-3:** the `pipefail` + FAIL-grep verification rule applies. Local runs are never labelled CI.
- **T-4:** no skip or quarantine without an orchestrator-recorded decision.

### A — CAS-REVOKE-CONSUMED-1

**Design:** the security spec, exactly (`rv-prh-i2-casino-security.md` FH-7). `RevokeLaunchSession`
does `SELECT … FOR UPDATE`, then `UPDATE … WHERE status IN ('active','consumed')`, returns the prior
status, and audits `prior_status`.

**Tests:**
- The spec's trigger matrix.
- Replay after revoke → `ErrLaunchSessionNotActive`.
- Pre-revoke bets still settle.
- TI: tenant B cannot revoke A's session.
- The four spec mutants.

**QA F1:** the DoD explicitly **inverts** the characterization test
`launch_two_phase_integration_test.go:703-795`. After the change the session is `revoked`, the audit
shows `prior_status=consumed` and `revoked=true`, the bet is refused, and the balance and ledger are
unchanged. The test must never be left failing or skipped.

**Reviewers:** SEC, CR, QA.

**DoD:** casino architecture note, registry, HANDOVER casino row.

### B — CAS-PLAY-BOOTSTRAP-1 (after A)

**Design:** ADR 0103, in security's S-5 order, all in one tx:
1. Authenticate with `webhookauth` **outside** the tx. The tenant comes from the credential or route,
   never the body.
2. Look up the token hash `FOR UPDATE`.
3. Check provider, tenant, mode, asset and expiry, refusing with a **uniform** error (no existence
   oracle).
4. Re-check the kill switch, capability and RG.
5. CAS `active → consumed` with the provider binding in the predicate.
6. Write the idempotency record `(tenant, provider, request_id)`, bound to `token_hash`.
7. Audit, then commit.

**Rules:**
- A refusal never consumes. ADR 0103 states whether a gate denial leaves the session `active` or
  `revoked`.
- A replay returns the stored response only when the hash matches and the session is still
  `consumed`.
- The raw token is never logged or audited. The player ref is opaque and provider-scoped.
- **`ResolveLaunchToken` is not reused as-is.**
- The MOCK vendor goes over HTTP; there is no in-process shortcut. The never-consumed TTL rule and the
  outbound tripwire are unchanged.

**Tests:**
- ADV per S-5 step (cross-provider, cross-tenant, expired, revoked, replay after A's revoke, reused
  request id with a different hash, refusal-does-not-consume).
- CON: exactly one of two concurrent consumes, under `-race`.
- IDM, AU, TI.
- MUT on the CAS predicate and the provider binding.

**Reviewers:** SEC (hard gate), ARCH, CR, QA, POP.

**DoD:** ADR 0103, the `docs/integrations/` contract page, the HANDOVER mock-vs-real row.

### C — PAY-DEP-REF-VALIDATE-1 (+ sync amount evidence)

**Design:**
- `depositAdapterCall` validates the reference before the outcome switch; an invalid reference on
  **any** outcome → `ErrorClassProviderRefInvalid` → park via `ApplyDisputeFromNonTerminal`.
- An empty or invalid reference on **Pending** also parks (S-9).
- An empty reference on a sync success keeps the existing ambiguous path.
- **LF-5 contract change:** `DepositResult` gains the echoed `Amount` and `AssetCode`, and the MOCK is
  updated. On a sync success, a missing amount → `ErrorClassAmbiguous` (the poll decides); a mismatch
  → T10 `sync_amount_mismatch`, with no posting. One comparison helper is shared with D.
- **LF-6 binding pre-check:** in-tx, under the intent lock and before posting, a same-tenant reference
  already bound to another deposit **or payout** attempt → T10 `provider_reference_conflict`. It must
  not become an error loop.
- **Cross-tenant expectation (S-9, LF-6):** there is no cross-tenant read; the index is per tenant
  (0101:131). The test asserts that tenant B's use of the same string has no effect on tenant A's
  attempt or ledger. It does not assert that B "detects" A's reference.

**Invariants:** I1, I3, I4, I6.

**Tests:** the LF C list; ADV per S-9; the callback-vs-sync race; A7 stays green; no T10 produces a
ledger tx.

**G2 — KS-CAS-DISCRIM-TEST-1** (PO F2). Owners payments + qa; reviewer code-reviewer. Add a test that
forces a non-kill-switch T2 CAS conflict at `drive.go:157-163`, and show that mutant K1 is killed.

**Reviewers:** SEC, LF, CR, QA.

**DoD:** ADR 0095 §8 amendment (the deposit half of C1, and the sync amount evidence in the
`DepositResult` contract), the PRH-REF record, and PAY-PSP-CONTRACT-INVDEP1 criteria updated to
require an amount echo.

### D — PAY-POLL-AMOUNT-1 + FH7-06 (merged S-6/LF-4)

**Design** (in the live-attempt success branch):
1. The shared helper compares amount and asset → T10 `poll_amount_mismatch`.
2. A **non-empty** echo that differs from `*attempt.ProviderReference` → T10 `poll_reference_mismatch`.
   An empty echo is allowed.
3. The tombstone lookup and the posting (`provider_tx_id`, idempotency key) **always** use the bound
   `*attempt.ProviderReference`, never the echo.
4. Defence in depth: `ledger.Post` rejects an empty `ProviderTxID` with `ErrInvalidEntry`, after a
   grep confirms no caller relies on `""`.

The order is the same as the callback path: mismatch → tombstone → INV-DEP-1 → post.

**Ride-alongs** (payments F2), in the same success-branch code: PAY-F3SM-TEST-1 and
PAY-SWEEP-CAS-NOISE-1.

**Tests:**
- The LF D list: the empty echo, a tombstone on the bound reference, the `-count=50` race.
- **QA F5:** a reconciliation-impact test showing the new dispute reasons are classified correctly and
  drift stays zero.
- MUT: post with the echo; skip the tombstone on empty; drop each comparison.

**Reviewers:** LF, SEC, CR, QA.

**DoD:** ADR 0095 §4.4 poll-row amendment, registry (including the two ride-alongs).

### E1 — KYC-SUBMIT-OUTBOX-1 (after H)

**Design:**
- Phase A writes the outbox row in the same tx as the verification/document rows.
- The worker claims with `FOR UPDATE SKIP LOCKED` and a claim token, commits, and calls
  `SubmitVerification` outside any tx (txscope-guarded). Phase C runs in a new tx under the claim
  check.
- An ambiguous result → retry with backoff (T-1 clock), then `failed_terminal` plus an alert. Status
  is never inferred.

**IC F3 (DoD):** the ADR 0095 §15.3 amendment and tests show that a `pending` or `claimed` (not yet
`sent`) outbox row still reads as the phase-A **orphan** that ADR 0096 §2.6(g)/§19 excludes from
"latest decided row". A vendor outage can never manufacture a `failed`.

**Tests:** crash between A→B and B→C; two workers; duplicate submit; TI/RLS; AU; MIG; MUT (claim
token, SKIP LOCKED).

**Reviewers:** SEC, ARCH, CR, QA.

**DoD:** ADR 0095 §15.3, the runbook entry "KYC outbox stuck", HANDOVER.

### E2 — Legacy deposit path (delete)

**Design:**
- Delete `InitiateDeposit`, `InitiateDepositAudited`, `attemptDeposit`, `handleDecline`,
  `resolveAmbiguous` and **`RecordDepositMultipleSuccessRefusal`** (LF-16).
- **Keep** `RecordDepositReversalRejection`; it has a live caller at `deposit_handlers.go:484`.
- Migrate the **20 call sites in 10 files**. Tests that are *about* something else keep their subject
  through direct fixtures.
- **Mutant parity (LF-16):** `ErrDepositAlreadyPostedForIntent` stays killed through a direct
  `ledger.Post` fixture.

**Tests:** a parity report signed by ledger-finance covering every INV-DEP-1, A7 and reconciliation
mutant; the txscope static test.

**Reviewers:** LF, SEC, CR, QA.

**DoD:** ADR 0095 §29 (legacy CLOSED), the PROV-OUTBOUND-CRED-1 precondition re-assessed.

### F — KYC enforcement

**F-kyc (OUTAGE-1):**
- Wrap the evaluator reads in a savepoint (the precedent is `db/idempotency.go:45`). On a DB error,
  roll back to it, record the `unavailable` decision and audit in the valid tx, and return
  `ErrKYCUnavailable` → 503.
- **LF-20:** the `unavailable` path commits **only** the decision and audit rows. There is no
  `withdrawal_requests` row and no posting (LF-I3-3).
- The separate-tx fallback is acceptable as a contingency.

**TESTPINS-1:** pin B6 (MB6) and MPLAYREC, and refuse `Outcome=unavailable` in `DenyForCompliance`
(N5).

**F-pay (DECISION-ROWS-1):** `RecordDecision` is called at the deposit gate (the phase-A tx), at payout
T1p allow, and at T2/T12 deny. None of these transactions has provider I/O (IC F4).

No KYC or RG threshold is seeded; HD-KYC-1..8 are unchanged.

**Tests:** lock-timeout fault injection; one decision row per evaluation per call site; MUT MB1, MB6
and MPLAYREC; TI/RLS.

**Reviewers:** SEC, CR, QA.

**DoD:** ADR 0096 §16 rows, the missing N2 mutation-kill addendum, the KYC-ENFORCE-1 prose.

### G1 — KS-AUDIT-TENANT-1

**Design:** option (c), `subject_tenant_id`, per the 0109 row. Options (a), an INSERT into tenant
scope, and (b), a non-atomic dual write, are rejected.

**S-10 items:**
1. The CHECK and FK.
2. The `NULLIF` form.
3. Player-scope sessions excluded.
4. The actor trigger.
5. The target taken from the route-validated `canActOnTenant` target, never the body.
6. The tenant projection **never** exposes platform staff IP, user agent or free-form metadata.

Until HD-PRH2-5 is answered, the actor is shown **pseudonymously**; that is the fail-closed choice on
disclosure. The same shape in `provider_credential_handlers.go` is surveyed and registered, and is not
extended without the orchestrator's go-ahead.

**Tests:**
- Tenant A sees platform actions on A's switches, never on B's.
- A tenant cannot write `subject_tenant_id`.
- The existing tenant audit readers still filter explicitly (`admin_routes.go:1167`,
  `kyc/provider.go:672`, `noeffect.go:81,197`).
- UPDATE is refused; MIG with refuse-down; a mutant on the predicate.

**Reviewers:** SEC, CR, QA.

**DoD:** ADR 0104, `12-audit-reporting-architecture.md`, security-architecture.

### H — CP-W1 sweeper process

**Design:** `payments.RunSweeperLoop`, mirroring `reconciliation.RunSchedulerLoop`.
- **It activates the deposit poll AND payout dispatch, resend and resolve** (payments F1). H is a
  payout go-live gate, and F-pay is a hard precondition.
- **Correctness (merged S-8/LF-8)** rests on the row lease with SKIP LOCKED
  (`SweeperBatchLeaseOwner`, `sweeper.go:74,169-208`), the claim-token CAS, ledger idempotency and the
  0107 indexes.
- An optional per-tenant `pg_try_advisory_xact_lock` (two-key or `hashtextextended` form) is **only an
  efficiency hint** that de-duplicates phase A. It is never held across a provider call; no tx is open
  during `QueryStatus`, `Deposit` or `Withdraw`.
- Tenants with non-terminal attempts are swept **regardless of tenant status**, because in-flight
  money must resolve. The kill switch is the brake. This is an engineering proposal for security and
  ledger-finance to confirm in the ADR 0095 §7.3 amendment.
- Config: `PAYMENTS_SWEEP_INTERVAL_SECONDS`. OTel counters. Per-tenant panic recovery; shutdown
  drain. MOCK adapters only (tripwire).

**Tests:**
- The LF H list: lease expiry mid-pass → one posting and one payout dispatch; the advisory-lock-removed
  mutant still passes; the claim-token-CAS-removed mutant fails.
- No tx is open during provider calls (S-8).
- Kill switch; tenant fault isolation; the AST tests; a full local `-race -tags integration` run.
- T-1 clock.

**Reviewers:** LF, SEC, CR, QA, devops.

**DoD:**
- **RECON-PAYOUT-LIVE-TEST-1** (payments F2).
- ADR 0095 §7.3 (S-8 wording).
- A prod config checklist row.
- Runbook entries "sweeper stalled" and "payout escalations".
- `observability-and-alerting.md`.

### I — Alert delivery (merged S-7/LF-7)

**I-core:**
- `Raise(ctx, tx, Alert)` upserts into `alerts` in the caller's tx, keyed UNIQUE on
  `(tenant_id, dedup_key)`, with the tenant in any derived key.
- The dispatcher is outside any tx, under the platform-service GUC. It uses a provider-neutral
  `AlertSink` with a `MockSink` and a log sink, bounded retry with backoff (T-1), then `dead` plus a
  meta-log.
- **Attribute contract:** a per-Kind allowlist. Provider refs appear only as
  `providerref.Fingerprint`. No player email, name or IP; no credential or token material; no raw error
  strings.
- **Ownership:** ledger and payment integrity P1s are **platform-owned** (`tenant_id NULL` +
  `subject_tenant_id`) and read-only for the tenant. A tenant ack never suppresses the platform's view
  or delivery (settled before I-wire).
- Ack and resolve are audited staff actions.

**Raise failure semantics:**
- **Binding (LF-7):** a `Raise` inside a financial evidence or posting tx (T10, T13d, any posting) runs
  under a **savepoint**. A failure there is logged and swallowed, and never aborts the dispute, the
  receipt or the posting.
- **Failure-path P1s whose business tx rolls back** (§28.8) use a **detached** `Raise` in a fresh tx.
  "A rolled-back event raises no alert" applies to event alerts only.
- **Open for ADR 0102:** security's S-7 item 5 recommends aborting the business tx on a `Raise`
  failure for integrity P1s. For financial evidence transactions, this plan adopts ledger-finance's
  binding LF-7 instead, because aborting would lose the dispute/receipt record that is itself the
  fail-closed outcome. ADR 0102 may apply abort-on-failure to **non-financial** integrity alerts. If
  security does not accept this split in the ADR 0102 review, the orchestrator escalates. It is not
  overruled here.

**I-wire:** in order:
1. The post-E2 multiple-success site (`orchestrator.go:989-1024`).
2. `index_backstop_fired` (`:1021`).
3. The drift P1 (`reconciliation/scheduler.go:333-341`).
4. The kill-switch engage alert.
5. The integrity-alert sites.

Logs are retained, and the dispatcher is wired into `main.go`.

**Tests:**
- The LF I list: failure injected inside T10/T13d → the dispute and receipt commit with a uniform 200;
  a detached P1 persists; dedup under N raisers; drift wired; the no-savepoint mutant.
- TI on dedup: tenant A's key never collides with or reveals B's.
- AU.

**Reviewers:** SEC, LF, CR, QA, devops.

**DoD:** ADR 0102, `observability-and-alerting.md` (the routing matrix is a placeholder pending
HD-PRH2-4), runbooks.

### J — PRH-I4-METRICS-1

**Design:** the counter `webhook_admission_decisions_total{decision,reason,provider_kind}` and an
in-flight gauge. **No tenant label** (HD-PRH-1 is open).

**Tests:** a no-op meter and a failing exporter leave admission decisions identical.

**Reviewers:** SEC, CR.

**DoD:** ADR 0097 §8, observability doc.

### K — Capabilities (force-resolve, manual adjustments)

**Inventory (verified):**

| Element | Where |
|---|---|
| Static role→permission map | `auth/permission.go:587-880` (DB-driven custom roles deferred, `:584-586`) |
| One role per staff user | `0011:12`, `0064:13-15`; `tenant_id NULL` ⇔ platform_admin (`0011:23-26`); dual-scope RLS (`0011:39-48`) |
| Route gates | `RequirePermission` reads the role from the JWT (`permission.go:899-914`). Access tokens are non-revocable and live 15 minutes (`jwt.go:98-102`, `config.go:448`); role and status are re-checked only on refresh (`auth_routes.go:346-349`) (S-4). |
| Staff↔Person link | `0029:18`. Optional (`0029:12-17`); Persons are platform-wide and unverified (`0009:13-19`); one is minted per NO_MATCH registration (`player_account.go:108-113`) (S-1) |
| Staff creation | A tenant caller can create `tenant_admin` and `support` staff with a chosen password and `person_id` (`admin_routes.go:491-550,656`). Only `finance`, `risk_manager`, `promotions_manager` and `bonus_operations` are platform-only (S-1). |
| Four-eyes patterns | withdrawal (`0026:107`), assets (`0044`), bonus + `bonus_approval_policies` (`0063:32-84`, payload match), casino catalogue (`0086`), provider credentials (`0096`), kill-switch release (`0105:150-167,243-247`, `decided_txid`) |
| Platform principal | `WithPlatformAdmin` (`db/tenant_rls.go:112-133`) never sets `app.tenant_id`; the 0105 resolver raises on mixed platform+tenant sessions (`0105:27-50`) (S-3) |
| Ledger | `ledger.Post` (the only entry); `LockProjectionsForPosting` (`lockorder.go:103-124`); the reason-code check (`lockorder.go:355-357`); BONUS_SET mirror legs auto-generated (HR-9 **removed**, `ledger.go:26-29`; HR-17 forbids hand-built mirrors) (LF-9) |
| Audit | `audit_log` (`0014`, `0016`) |

**Security fact (S-1), stated in ADR 0099.** Under the current unverified identity model,
distinct-principal and distinct-Person checks do **not** prove two humans. The distinct-Person
triggers stay as defence in depth. Structural prevention depends on HD-PRH2-2 (c) or (d). A person
link counts as trustworthy only once verified staff identity exists: `persons.status='verified'`
means something only with real KYC.

**K1 — grants (ADR 0099).** A governed action needs both the static permission and an in-force grant
row read inside the action's own tx. The closed enum is `payment_force_resolve:{request,approve}` and
`ledger_adjustment:{initiate,approve}`; a JWT is never trusted for grants.

- *S-4, in every K mutation tx:*
  - The actor is derived from the DB session GUC and required by the triggers.
  - The actor's `staff_users` row is re-read: active, tenant, eligible role.
  - At execution, the initiator's and every counted approver's grant and status are re-checked. A
    revoked or suspended approver does not count.
  - AZ tests: a suspended actor with an unexpired token is refused; an actor demoted mid-token is
    refused.
- *S-11 lifecycle:* stale approvals are voided by the execution-time re-check. Grants from a later
  demoted or suspended grantor remain, but are surfaced for re-attestation. Expiry between approve and
  execute → refused.
- *S-3:*
  - Platform scope covers platform-owned objects only, until HD-PRH2-6 is answered.
  - If the answer is (a) or (c), ADR 0099/0100 designs a "platform principal acting in tenant X" RLS
    family explicitly: validated platform GUC plus target tenant, trigger-validated, audited with
    both, and limited to the named K tables and the ledger post.
  - A threat model is required before K1 code.
- *Grantee eligibility and grant approval* follow the answer to HD-PRH2-2.

**K2 — manual adjustment (ADR 0100; ledger-finance owns).** Binding rulings:
- **Posting-shape catalogue (LF-9, S-13).** The only allowed shape is the tenant's
  `manual_adjustment` counter-account ↔ one player's `player_cash` in the request's tenant, same
  asset. It is enforced in the executor **and** by a CHECK. Refused shapes: `player_withdrawal_hold`,
  `player_locked_*`, `player_bonus*`, `psp_*`, `provider_payable`, `promo_liability`,
  `bonus_expense` and `jackpot_contribution`. Bonus corrections go through bonus-engine.
- **Payload-bound approvals (LF-10).** The payload is immutable after submission, and each approval
  pins a payload hash (the 0063 precedent). The executor posts exactly that payload. The linked
  transaction is verified by trigger. "Same tx" means `decided_txid`, never `xmin`.
- **Independence floor (LF-11).** Whenever four-eyes applies, a distinct non-NULL `person_id` is
  required. This is **non-configurable**; there is no `distinct_principal` option. The S-12
  beneficiary exclusion also applies: neither the initiator nor any approver may be the target
  player's Person.
- **No negative balance (LF-13).** A debit adjustment may not drive a player-owned account negative.
  This is checked under `LockProjectionsForPosting` in the execution tx. If funds are insufficient,
  the request ends terminal with nothing posted. Execution happens **in the final approval's tx**, so
  there is no approved-but-unexecuted window. An ADR 0082 amendment places the request/resolution row
  lock before L3.
- **Counting and keys (LF-14):**
  - An approval counts only if the approver's grant is in force at execution (timestamp check).
  - `idempotency_key='manual_adjustment:'||request_id`, UNIQUE `(tenant_id, idempotency_key)`.
  - `correlation_id` = the request; `causation_id` = the compensated transaction, if any.
  - The closed reason-code catalogue is copied to `ledger_transactions.reason_code`.
  - The catalogue has **no deposit-allocation code** (LF-2).
- **Per-asset thresholds (LF-12).** `above_threshold` requires an `asset_code`. Amounts above int64
  max are refused at submission.
- **Policy authoring (S-2; human input HD-PRH2-7).** ADR 0100 specifies:
  - (i) the permission and principal scope for inserting policy rows at each level;
  - (ii) that **every** less-specific row, the platform default included, is a floor enforced in the
    DB;
  - (iii) that a policy change is itself four-eyes and audited, and cannot be made by the Person who
    initiates under it;
  - (iv) that a loosening row fails closed at DB level.
- **No in-force policy → disabled.** No threshold is seeded; fixtures use synthetic, test-only values.
- **HD-PRH2-6:** until it is answered, K2 fails closed: platform scope is refused on tenant ledgers.

**K3 — force-resolve (ADR 0101).** Binding rulings:
- **Deposits never leave `disputed` (LF-1).** M1 is a `payment_manual_resolutions` row on a
  still-disputed attempt, **evidence-only** (LF-2). There is no M1 credit of any type for a financially
  resolved intent: the funds leave only via a PSP refund (reversal → tombstone), or via
  LEDGER-SUSPENSE-B-1 later. The K3 executor refuses to link a K2 transaction to a deposit resolution.
  An M1 credit for an *unresolved* intent is out of scope; if ever built, it goes through
  `postDepositSuccess`.
- **LF-3:** an M1 resolution on `multiple_success_for_intent` **never clears or suppresses**
  `pay_captured_unposted`. It only annotates the finding as acknowledged, which keeps reporting and
  ageing.
- **Payout only (M2):**
  - `{ambiguous, disputed} → succeeded | declined` via the 0114 guard, with `last_evidence_kind='operator'`.
  - Only when the withdrawal is `submitted`; T14 disputes are excluded (LF-15).
  - "Declare paid" uses a **reserved provider-tx namespace** that `providerref.Validate` refuses for
    real references, so it cannot collide.
  - ADR 0101 records the risk of "declare not paid" followed by a late real success: a double payout,
    which surfaces as a T14 P1.
  - The S-12 beneficiary check applies on top of the withdrawal guards.
  - `withdrawal.go` is not edited (payments F3).

**Tests (K1–K3):** the LF K2 and K3 lists and the QA W1/W2/W4 K items, plus the S-4 AZ cases, the
S-11 races, the S-12 beneficiary refusal, and sock-puppet cases documented as *not structurally
prevented* unless HD-PRH2-2 is (c) or (d).

**Reviewers:** SEC (all), LF (K1–K3; financial invariants), CR, QA, ARCH, POP (K1 enum).

**DoD:**
- ADRs 0099, 0100 and 0101, plus amendments to ADR 0095 §4.8 and ADR 0082.
- `05-identity-architecture.md` and security-architecture.
- `backoffice/src/auth/permissions.ts`.
- A runbook for manual adjustment and force-resolve.

### L — Handover readiness

**Inventory:**
- There is no root README.
- `docs/runbooks/README.md` (ops index plus local setup) and `docs/api/README.md`.
- `docs/security/*`, `docs/testing/testing-strategy.md`, `docs/architecture/*`.
- The governance docs and `deploy/docker/README.md`.

**Proposal:** a **links-only** `docs/HANDOVER.md`, with no reorganisation. Its sections:
- Start here.
- Architecture map.
- Decisions index.
- Build, test and run.
- Security.
- Runbooks.
- **Mock-vs-real matrix (new):** capability × adapter × status × gating precondition.
- **Secret NAMES inventory (new):** names only, never values, in
  `docs/security/secret-names-inventory.md`.
- Open blockers (a registry filter).
- The DoD checklist.

**Handover DoD for every workstream:**
- The registry row.
- The ADR or amendment.
- A runbook entry if the change is operational.
- The HANDOVER mock-vs-real and secret-name rows if an adapter, secret or config changed.
- A production config checklist row for any new env var.
- The review records filed under `docs/plans/prh2-hardening-round/`.

## 6. ADRs

The highest existing ADR is 0098. Security reviews every ADR; ledger-finance also reviews 0099–0101.

| ADR | Title | Must incorporate |
|---|---|---|
| **0099** | Scoped staff capability grants on static RBAC | S-1 (identity fact), S-3, S-4, S-11; HD-PRH2-2/6 answers |
| **0100** | Governed manual adjustments and approval policies | S-2, S-12, S-13, LF-9..LF-14; HD-PRH2-1/7 answers |
| **0101** | Payment force-resolution M1/M2 (amends 0095 §4.8) | LF-1, LF-2, LF-3, LF-15, LF-18, S-12 |
| **0102** | Durable alerting and provider-neutral delivery | S-7 items 1–4, LF-7, the Raise-failure split (§5-I) |
| **0103** | Casino launch-token bootstrap contract | S-5 order |
| **0104** | Tenant-visible audit of platform actions | S-10 |

Amendments:
- ADR 0095: §4.4 (D), §8 (C, including the `DepositResult` amount echo), §15.3 (E1 + IC F3),
  §29 (E2), §7.3 (H + S-8).
- ADR 0096 §16 (F).
- ADR 0097 §8 (J).
- **ADR 0082:** lock order for the K2/K3 request rows before L3 (LF-13).
- Any "(a)" answer to HD-PRH2-1 needs a **CLAUDE.md amendment record**, not just an ADR note (PO F1,
  security §4).

## 7. Human decisions (not decided here)

| ID | Question | Options | Consequences |
|---|---|---|---|
| **HD-PRH2-1** | **The floor today (CLAUDE.md, permanent):** "Manual balance adjustments require a reason code and four-eyes approval **above a configurable threshold**". The threshold is configurable and can be zero, which means "always". The four-eyes capability itself cannot be removed. **Unless the human amends CLAUDE.md, (b) is the engineering floor.** The question: may a policy switch four-eyes off entirely for manual adjustments and/or force-resolve? And for `above_threshold`, is there a maximum (a ceiling) a threshold may be set to? | (a) `never` allowed per tenant/jurisdiction with a legal-review reference — **needs an explicit CLAUDE.md amendment**; (b) no `never`; thresholded four-eyes only (the current rule); (c) force-resolve always four-eyes, adjustments per (a) or (b) | **Combined consequence:** (a) together with HD-PRH2-2 (a) or (b) lets one human move money alone. **Without a ceiling, (b) is (a) in disguise**, because a huge threshold equals no four-eyes. Distinct-Person is **not** part of this question: ADR 0098 §2 already requires an independent approver, so it is a non-configurable floor (LF-11). |
| **HD-PRH2-2** | Does granting a money-moving capability itself need further control? **The fact (S-1):** a `tenant_admin` can create `tenant_admin` and `support` accounts, choosing their password and `person_id`, and Persons are unverified self-registrations. So today's identity model cannot tell two approvers from one human. | (a) a single authorized admin grants; (b) grants need a second, tenant-local approver (configurable); (c) a platform co-approval is required for tenant grants of money-moving capabilities; (d) tenant admins grant, but only to staff accounts a **platform caller created** (extending the Stage 3D `finance` precedent) | **Only (c) or (d) structurally prevent unilateral money movement by one tenant admin until verified staff identity exists.** (b) does not close it: a second minted tenant admin satisfies it. (c) adds platform approval load for every B2B grant. (d) adds platform involvement in onboarding each B2B approver. K1 stays blocked until this is answered. |
| **HD-PRH2-3** | Confirmation: is any monetary threshold needed **before implementation**? The architect and ledger-finance say no. | Confirm, or name a value requirement | `above_threshold` rows are **per-asset** (minor units differ). No migration seeds a policy row, and fixtures use synthetic, test-only values. Real values are set later, per tenant and jurisdiction, under legal review. |
| **HD-PRH2-4** | Alert routing: destinations, the severity→channel matrix, on-call ownership, ack expectations; later, a paging vendor | Mock sink now; the matrix stays a placeholder | Until decided, P1s are durable and visible but nobody is paged. PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking. |
| **HD-PRH2-5** | Do tenant-visible platform actions show platform staff identity? | (a) full identity; (b) pseudonymous "platform operator" plus an internal reference; (c) per tenant contract | A privacy and B2B-expectation question. **Engineering rule under every option:** platform staff IP, user agent and free-form metadata are never disclosed to tenants. Until answered, the read API shows (b) (fail closed on disclosure). |
| **HD-PRH2-6** | May a **platform**-scope capability holder adjust or force-resolve money in a tenant operating under **its own licence** (ADR 0006)? | (a) yes, all tenants; (b) only tenants under the platform licence; (c) per-tenant opt-in | (a) and (c) need a new "platform principal acting in tenant X" RLS family (S-3), which is new privileged power with its own threat model. (b) needs a reliable per-tenant licensing-mode attribute: `tenants.licensing_model` is referenced at `permission.go:348-349`, but its fitness for this was not verified. **Until answered, K2/K3 fail closed: platform scope is refused on tenant ledgers.** |
| **HD-PRH2-7** (new, S-2) | Who may author or **loosen** `financial_approval_policies` at tenant and brand level, and does loosening need platform approval? | (a) platform-only authoring; tenants may only tighten (**security's recommended default until decided**); (b) tenant authoring with four-eyes, with all less-specific rows enforced as floors; (c) tenant authoring, but loosening requires platform co-approval | (a) is the simplest and safest, and puts load on the platform for every B2B policy. (b) is exposed to the same sock-puppet fact as HD-PRH2-2. (c) is a middle path. K2 stays blocked until this is answered. |
| Existing | HD-PRH-1 and HD-KYC-1..8 | Not re-asked | J has no tenant label; F seeds no thresholds |
| CI must-pass names | — | **RESOLVED: no.** No CI change in PRH-2 (PO F3; out of scope per the human). | — |

## 8. Out of scope

- Real vendors of any kind (PSP, casino, KYC, custodian, paging or alert), vendor selection and
  contracts.
- AWS, Terraform, `deploy/aws`, staging, WEBHOOK-EDGE-1.
- Production credentials or data.
- CI, billing and branch-protection changes (CI-BILLING-1, BRANCH-PROTECTION-1).
- DB role, password or test-infrastructure changes.
- Bonus Wave 4; AI agents.
- Custom roles or a DB-driven role catalogue: only the four named capabilities.
- The M1 credit path (LF-2).
- PAY-SEC-LAUNCH-1, M5, LEDGER-SUSPENSE-B-1, SB-CATALOGUE-IO-1 (registered only).
- Any threshold or jurisdiction-rule value.
- Repo reorganisation.

## 9. Risks and authorization order

| Risk | Mitigation |
|---|---|
| The PAY lane is the critical path (7 items, now with C's contract change) | E2 first. Keep items small. The other lanes run in parallel. |
| The K lane stalls on HD-PRH2-1/2/6/7 | K is blocked explicitly and the other lanes proceed. The K migrations renumber at merge (Rule 3). |
| Sock-puppet approvers (S-1) | Stated as a fact. Structural closure only via HD-PRH2-2 (c) or (d). Distinct-Person checks are defence in depth only. |
| A loosening policy defeats four-eyes (S-2) | DB-enforced floors; policy changes are four-eyes; HD-PRH2-7 |
| Alerting aborts a financial tx | LF-7 savepoint rule; the split with S-7 goes through the ADR 0102 review |
| H activates payouts on a latent bug | F-pay first; lease-expiry and mutant tests; the kill switch as brake; MOCK only |
| E2 loses coverage | LF-16 parity, including the sentinel fixture |
| Timing-dependent flakes | T-1 and T-2 |
| No CI evidence | T-3; CI-BILLING-1 stated at W5 |

**Recommended authorization order:**
1. W0 (the ADRs, HANDOVER, the registry pass), together with answers to HD-PRH2-1, -2, -6 and -7.
2. The provider-independent lanes, which don't depend on §7: A, E2, F, G1, I-core, C(+G2), D, B, E1,
   J, H.
3. K1 → K2 → K3, once the HD answers and the ADRs are in.
4. I-wire, then W5 and **stop**.

## 10. Review disposition

"Adopted" means the plan text now carries the finding. Info items are recorded as noted.

| Finding | Where addressed |
|---|---|
| **Security** | |
| S-1 High | §5-K "Security fact", K1; §7 HD-PRH2-2 (re-framed, option (d)); §2 K1 block; §9 |
| S-2 High | §5-K2 "Policy authoring"; §4 0112; §7 HD-PRH2-7; §2 K2 block |
| S-3 | §5-K1; §4 0111/0112/0114 RLS column; §7 HD-PRH2-6 |
| S-4 | §5-K1; §5-K inventory row |
| S-5 | §5-B; §1-B; §2 W2; ADR 0103 |
| S-6 | Merged with LF-4: §5-D; §1-D |
| S-7 | Merged with LF-7: §5-I; §4 0110; §6 0102. Item 5 is split as described in §5-I (open for the ADR 0102 review, not overruled). |
| S-8 | Merged with LF-8: §5-H |
| S-9 | §5-C; §1-C |
| S-10 | §5-G1; §4 0109; ADR 0104 |
| S-11 | §5-K1 |
| S-12 | §5-K2, K3; §4 0112 |
| S-13 | Merged with LF-9: §5-K2; §4 0112 |
| S-14 Info | Noted (A unchanged) |
| Security §3/§4/§5 | §2 conditions; §7 re-frames; §1 W5 open list |
| **Ledger-finance** | |
| LF-1 High | §5-K3; §4 0114 |
| LF-2 High | §5-K2, K3; §8 |
| LF-3 High | §5-K3 |
| LF-4 High | Merged with S-6: §5-D (incl. the `ledger.Post` empty-`ProviderTxID` refusal) |
| LF-5 High | §5-C (the `DepositResult` contract lives in C); §1-C; §3 C touches |
| LF-6 | §5-C |
| LF-7 High | Merged with S-7: §5-I; §1-I (corrected site inventory) |
| LF-8 | Merged with S-8: §5-H |
| LF-9 High | §5-K2 (HR-9 correction, shape catalogue); §5-K inventory |
| LF-10 High | §5-K2; §4 0112 |
| LF-11 | §5-K2; §7 HD-PRH2-1 (removed from the question) |
| LF-12 | §5-K2; §4 0112; §7 HD-PRH2-3 |
| LF-13 | §5-K2; §6 ADR 0082 amendment |
| LF-14 | §5-K2 |
| LF-15 | §5-K3 |
| LF-16 | §1-E2; §5-E2 |
| LF-17 | §1-K |
| LF-18 | §4 0114 |
| LF-19, LF-21 Info | Noted |
| LF-20 Info | §5-F (adopted as a condition) |
| LF "Corrections to HD list" | §7 HD-PRH2-1/3/6 |
| **QA** | |
| F1 | §5-A; §1-A |
| F2 | §5.0 T-1 |
| F3 | §5.0 T-2 |
| F4 | §4 RLS column |
| F5 | §5-D |
| F6 | Header precondition; §2 W0 |
| F7 Info | §5.0 T-3; §2 W5 |
| QA per-wave checklist | §2 (referenced as the exit criterion per wave) |
| **Payments** | |
| F1 | §1-H; §5-H; §2 edges and W3 |
| F2 | §1 ride-alongs; §5-D; §5-H DoD |
| F3 | §3 K3 row |
| F4 Info | Noted |
| **Identity-compliance** | |
| F1 | §3 F-pay reviewers |
| F2 | §2 W0 registry pass |
| F3 | §5-E1 DoD |
| F4 Info | §5-F |
| **Product-owner-proxy** | |
| F1 | §7 HD-PRH2-1 (floor up front; (a) needs a CLAUDE.md amendment); §6 |
| F2 | §5-C "G2" line |
| F3 | §7 CI row RESOLVED: no |
| F4–F8 Info | Noted. The K scope stays at the minimum, the ADR set is unchanged, alerting is proportionate, HANDOVER is links-only, and the wave order is unchanged. |

Nothing is marked "not adopted". The only partial item is S-7 item 5, where the binding LF-7 governs
financial transactions and the rest is left to the ADR 0102 review. It is recorded as an open split,
not dropped.
