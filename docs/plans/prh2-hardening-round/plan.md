# PRH-2 — Provider-Independent Hardening Round + Continuous Handover Readiness — Planning Gate

Status: **PLANNING GATE, revision 3 — NOT AUTHORIZED FOR IMPLEMENTATION.**
Author: architect. Date: 2026-09-28.

| Rev | Base | Change |
|---|---|---|
| 1 | `559483a` (commit `a3547f5`) | Initial plan |
| 2 | `e3008d3` | Incorporates all six plan reviews under `reviews/`: security, ledger-finance, qa, payments, identity-compliance and product-owner-proxy, each **APPROVE WITH CONDITIONS**. Every finding is mapped in §10. |
| 3 | `0722928` | Incorporates the security addendum (`reviews/security.md`, "Addendum — ruling on plan revision 2"): the alert split is accepted with conditions a–e, and H is accepted as resolution-only for non-active tenants. Also incorporates all 12 corrections in `reviews/code-reviewer-verification.md` (QA F6 fulfilled). |

**Binding ledger-finance set:**
- The HIGH rulings LF-1, -2, -3, -4, -5, -7, -9 and -10, plus the LF-13 ruling (no negative balance
  from a debit adjustment).
- Also adopted as binding by this plan: LF-11 (the distinct-Person floor), LF-14 and LF-15.

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
- For revision 3, I re-checked the `MISMATCH FOUND` P1s at `reconciliation/scheduler.go:420,487,562,719`
  and `migrations/0001:13` (`licensing_model`). The other rev-3 facts are taken from the code-reviewer's
  verification, which was run against unchanged code (`git diff 559483a d2cf040 -- '*.go' '*.sql'` is
  empty).
- No DB access; no tests were run. Scope is MOCK and local only. Nothing in this plan is a regulatory
  or licensing claim.

**Binding principle (human, ADR 0098 §4):**
- Jurisdiction-specific requirements are explicit configuration: versioned, effective-dated, audited,
  and scoped by jurisdiction, tenant or brand. They are never global invariants and never hard-coded
  thresholds.
- Absent policy or evidence means **fail closed**.
- Regulatory configuration stays separate from ledger correctness.

**QA F6: fulfilled.** `code-reviewer` independently verified §1 (`reviews/code-reviewer-verification.md`,
VERIFIED WITH CORRECTIONS), and revision 3 applies all 12 corrections (§10).

---

## 1. Verified state (A–L)

Every item below is verified as still open in the code at `559483a`.

| WS | Registry id | State | Evidence (file:line) |
|---|---|---|---|
| A | CAS-REVOKE-CONSUMED-1 | **OPEN** | `0042_…up.sql:58-60`: no change out of `consumed/expired/revoked`. `casino/launch.go:377-383` revokes `active` only and records no `prior_status`. The characterization test asserting a bet on a consumed session is accepted is at `casino/launch_two_phase_integration_test.go:703-795` (QA F1). |
| B | CAS-PLAY-BOOTSTRAP-1 | **OPEN** | `ResolveLaunchToken` (`launch.go:279`) has no non-test caller. It consumes on the token hash alone, with no provider/mode/asset predicate, and its lazy expiry writes `expired` (`:290-325`, `:311`) (S-5). |
| C | PAY-DEP-REF-VALIDATE-1 | **OPEN** | Three gaps: <br>• The reference is not validated: `drive.go:250-273` / `:283-310`, whereas payout validates at `payout.go:392-395,1178-1181`. <br>• There is no amount evidence on a sync success: `DepositResult` has no `Amount`/`AssetCode` (`types.go:471-495`), `drive.go:260-264` only classifies the outcome; the sync success then posts `attempt.Amount` at `drive.go:354`, after the sync tombstone check at `:329`; and the intent comparison at `orchestrator.go:1108` is tautological (LF-5). <br>• A Pending result with an empty reference goes through `MarkAccepted` (`drive.go:294`), fails the `payment_attempts` CHECK at `0101:90-93` (then 0099:136-137), and surfaces as an untyped error rather than a park (S-9). |
| D | PAY-POLL-AMOUNT-1 + FH7-06 | **OPEN** | `sweeper.go:513` posts without an amount/asset/reference comparison; the only check is at `:471`, in the already-succeeded branch. The posting and the tombstone lookup both use the **echoed** `res.ProviderReference` (`:~497,513`). With an empty echo, the tombstone check is skipped and the posting key becomes `"<provider>:"`, because `ledger.Post` accepts an empty `ProviderTxID` (`lockorder.go:346`) (LF-4, S-6). |
| E1 | KYC-SUBMIT-OUTBOX-1 | **OPEN** (deferred, accepted) | `kyc/document_service.go:285-292`; there is no outbox table. |
| E2 | PROV-OUTBOUND-CRED-1-LEGACY-PATH | **OPEN** | The call direction is `orchestrator.go:674` (`InitiateDepositAudited`) → `:555` (`InitiateDeposit`) → `:700` (`attemptDeposit`) → `provider.Deposit` at **`:714`**, and → `:762` → `:793` (`resolveAmbiguous`) → `provider.QueryStatus` at **`:797`**. Both provider calls hold a `pgx.Tx`, and there is no non-test caller. There are **21 test call sites of the deleted functions in 10 files**: 20 through `InitiateDeposit`/`InitiateDepositAudited` (including `migration_0107_integration_test.go:1867`), plus a direct `orch.resolveAmbiguous(...)` at `migration_0107_integration_test.go:1774` (LF-16; code-reviewer E2c). **Correction (E2d):** the legacy chain is **not** the only path to `ErrDepositAlreadyPostedForIntent`. The live `postDepositSuccess` maps it at `orchestrator.go:1169`; `TestX5_LedgerBackstopMapping` (`:1510-1569`) reaches it through a real race; and a direct `ledger.Post` fixture already exists at `migration_0107_integration_test.go:344`. `RecordDepositMultipleSuccessRefusal` (`:1501`) has this chain as its only caller. |
| E3 | Provider call in a financial tx: sweep | **Done (read-only)** | Only E2's `:714` and `:797`. The other adapter call sites are all txscope-guarded or outside a tx: `drive.go:251`, `payout.go:385,1174`, `sweeper.go:354`, `casino/orchestrator.go:632`, `kyc/verification_service.go:335` and `kyc/document_service.go:425`; email `credential_handlers.go:159,321`. The sweep also found a **non-financial** case: `sportsbook/catalogue.go:52-56` calls `provider.Catalogue()` inside a tx → register **SB-CATALOGUE-IO-1** (Low; before a real sportsbook adapter; not planned). |
| F | KYC-ENF-OUTAGE-1, -DECISION-ROWS-1, -TESTPINS-1 | **OPEN** | `kyc/enforcement.go:465-468` → `withdrawal.go:397-417` (a 25P02 in the aborted tx). No decision row is recorded at the deposit gate (`payments/kycgate.go:40-68`), at the payout **T1p allow** path (`payout.go:285-323`) or at T2/T12 deny (`payout_sweep.go:99-130`). The T1p **deny** path already records one, via `DenyForCompliance` → `withdrawal.go:1114`. `withdrawal.go:1046-1051` has no B6 pin and no N5 guard. |
| G | KS-AUDIT-TENANT-1, KS-CAS-DISCRIM-TEST-1 | **OPEN** | `payments_kill_switch_handlers.go:246-251` writes the audit tenant as `uuid.Nil`, and `0014:32-40` is one `FOR ALL` dual-scope policy. The untested branch is `drive.go:157-163`. |
| H | CP-W1 | **OPEN** | `NewSweeper` (`sweeper.go:102`) has no caller. `main.go:394-447` wires other loops only. **`Sweeper` also drives payout T2/T12/resolve** (`payout_sweep.go`, dispatched via `RunOnce` at `sweeper.go:124`) (payments F1). Payout sweeping runs only if `Sweeper.PayoutKYCGate` is set (`sweeper.go:84-89`, `payout_sweep.go:72`), and `NewSweeper` does not set it. H's wiring **sets it explicitly**, which is what activates the payout half (code-reviewer H3). |
| I | PAY-P1-MULTISUCCESS-ALERT-1 + alert delivery | **OPEN** | Every alert is log-only. Live sites (LF-7): <br>• `auditMultipleSuccessForIntent` (`orchestrator.go:989-1024`, log at `:1018`), inside the T10/T13d evidence tx; <br>• `payments_deposit_intent_index_backstop_fired` (`:1021`); <br>• the drift P1 (`reconciliation/scheduler.go:333-341`) and the other reconciliation `MISMATCH FOUND` P1s at `scheduler.go:420` (sportsbook settlement), `:487` (casino consistency), `:562` (casino statement) and `:719` (payment statement);
<br>• `payment_deposit_simulation_handlers.go:240` (`payment_simulation_integrity_alert_payload_mismatch`); <br>• the kill-switch engage alert (`payments_kill_switch_handlers.go:360`); <br>• the `*_integrity_alert_*` lines in the deposit, casino, casino-play and sportsbook-settlement handlers. <br>The `:1509` site dies with E2. There is no alert table. |
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
| **W0** | ADR drafts 0099–0104 (§6), HANDOVER skeleton, registry pass | — | The QA W0 checklist (`reviews/qa.md` "Per-wave gate checklist"), plus: <br>• security reviews every ADR, and ledger-finance reviews 0099–0101; <br>• §1 re-verified by code-reviewer (QA F6, **done**; corrections applied in rev 3); <br>• the registry pass: add SB-CATALOGUE-IO-1, HANDOVER-1, CAP-GRANT-1 and ALERT-DELIVERY-1; correct **HD-KYC-1..7 → HD-KYC-1..8** (IC F2); fix the KYC-ENFORCE-1 prose (DenyForCompliance is wired); mark the ride-alongs folded. |
| **W1** | A, E2, F-kyc, G1, I-core, K1* | E2: ledger-finance re-confirms the LF-16 parity scope on the corrected premise (§1-E2). <br>*K1 **blocked** until: <br>• HD-PRH2-2 is answered in its re-framed form, and the answer satisfies the K1 go/no-go condition (§5-K). Otherwise K1 goes back to security review; <br>• HD-PRH2-6 is answered, or platform scope is limited to platform-owned objects; <br>• ADR 0099 is accepted with S-1, S-3, S-4 and S-11; <br>• product-owner-proxy confirms the four-capability closed enum. | QA W1 checklist |
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
| I-wire | payments + casino + devops (SEC, LF, CR, QA) | `orchestrator.go:989-1024,1021`, `reconciliation/scheduler.go:333-341,420,487,562,719`, `payments_kill_switch_handlers.go:360`, integrity-alert sites incl. `payment_deposit_simulation_handlers.go:240`, dispatcher line in `main.go` | PAY / W4 |
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
| **0114** | K3 | `payment_manual_resolutions` (+ approvals, UNIQUE per attempt, `decided_txid`). Amend `payment_attempts_guard()` for **payout attempts only**: `{ambiguous, disputed} → succeeded` ("declare paid") or `→ declined` ("declare not paid"), with `last_evidence_kind='operator'`, admitted only with an approved resolution for that attempt and that target state in the same tx (`decided_txid = txid_current()`, 0105:243-247,324). **The deposit branches and both 0107 indexes stay byte-identical, with no state-CHECK change** (LF-1). <br>• Trigger **`payment_manual_resolutions_beneficiary_guard`** (BEFORE INSERT on resolutions and approvals; S-12): neither the requester nor any approver may resolve to the Person owning the attempt's player account. This covers **both** M1 (deposit attempt owner) and M2 (payout attempt owner). | FORCE RLS, tenant family (platform family per HD-PRH2-6) | Restore the 0107 guard verbatim; refuse while resolutions exist. Guard tests run on a HEAD-migrated scratch DB (LF-18). |

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
- Migrate the **21 call sites in 10 files**, including the direct `resolveAmbiguous` call at
  `migration_0107_integration_test.go:1774`. Tests that are *about* something else keep their subject
  through direct fixtures.
- **Mutant parity (LF-16, as corrected by code-reviewer E2d):** the legacy chain is not the only path
  to `ErrDepositAlreadyPostedForIntent`. The live `postDepositSuccess` maps it at
  `orchestrator.go:1169`; `TestX5_LedgerBackstopMapping` (`:1510-1569`) reaches it through a real
  race; and a direct `ledger.Post` fixture already exists at `migration_0107_integration_test.go:344`.
  **Ledger-finance re-confirms the LF-16 parity scope at E2's gate**, on this corrected premise, before
  E2 merges.

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
- **H's wiring sets `Sweeper.PayoutKYCGate` explicitly** (code-reviewer H3). `NewSweeper` leaves it
  nil, and payout sweeping is inert without it (`sweeper.go:84-89`, `payout_sweep.go:72`).
- A test asserts that the constructed production sweeper has a non-nil `PayoutKYCGate`.
- **Correctness (merged S-8/LF-8)** rests on the row lease with SKIP LOCKED
  (`SweeperBatchLeaseOwner`, `sweeper.go:74,169-208`), the claim-token CAS, ledger idempotency and the
  0107 indexes.
- An optional per-tenant `pg_try_advisory_xact_lock` (two-key or `hashtextextended` form) is **only an
  efficiency hint** that de-duplicates phase A. It is never held across a provider call; no tx is open
  during `QueryStatus`, `Deposit` or `Withdraw`.
- **Non-active tenants are resolution-only** (security addendum §2; ACCEPTED with amendments).
  Tenants with non-terminal attempts are swept regardless of tenant status, because in-flight money
  must resolve.
  - *Allowed for a non-active tenant:* `QueryStatus`, evidence application, dispute, and T17 re-drive
    of an already-sent attempt.
  - *Never for a non-active tenant:* a new money-moving call. That means no `created`-attempt
    dispatch, no cascade child and no new payout `Withdraw`. A non-active tenant is treated like an
    engaged kill switch for new dispatch.
  - *Safeguards:*
    - Tenant status is read **in-transaction, per tenant**.
    - The kill switch and the synthetic-adapter tripwire still apply.
    - Credentials come **only** from the per-tenant resolver, never another tenant's or a platform
      credential.
    - Every resolution action is audited exactly as for active tenants.
  - Recorded in the ADR 0095 §7.3 amendment.
- Config: `PAYMENTS_SWEEP_INTERVAL_SECONDS`. OTel counters. Per-tenant panic recovery; shutdown
  drain. MOCK adapters only (tripwire).

**Tests:**
- The LF H list: lease expiry mid-pass → one posting and one payout dispatch; the advisory-lock-removed
  mutant still passes; the claim-token-CAS-removed mutant fails.
- No tx is open during provider calls (S-8).
- Security addendum §2(c):
  - a suspended tenant's pending attempt resolves by poll;
  - a suspended tenant's `created` attempt is **not** dispatched, and neither is a cascade child or a
    new payout `Withdraw`;
  - tenant isolation: one tenant's suspension neither affects nor exposes another tenant's sweep.
- MUT: skipping the in-tx status read makes a suspended tenant's `created` attempt dispatch, and a
  test must fail.
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
**Ruling (security addendum §1): the split is ACCEPTED, with conditions (a)–(e).**
- *Scope:* in financial evidence and posting transactions (T10, T13d, any posting), `Raise` runs under
  a **savepoint** and never aborts the dispute, receipt or posting (LF-7). The dispute/receipt record
  is itself the fail-closed outcome. ADR 0102 may use abort-on-failure for **non-financial** integrity
  alerts.
- *(a) Narrow swallow:* only `Raise`'s own error inside its savepoint is swallowed, and the savepoint
  is rolled back. An already-aborted outer transaction (25P02), a serialization failure or a deadlock
  **propagates**; it is never swallowed.
- *(b) Post-commit detached retry:* after the business transaction commits, a swallowed in-transaction
  `Raise` gets a detached `Raise` in a fresh transaction. This is best-effort, but **mandatory** for any
  Kind without a reconciliation backstop (see (d)).
- *(c) Error log and metric:* log at Error and increment `alert_raise_failures_total{kind}`. Labels are
  bounded, with no tenant label. Neither the log nor the metric is a prerequisite for the money path.
- *(d) Reconciliation backstop:* ADR 0102 lists, per alert Kind, the standing reconciliation check that
  surfaces the condition if its P1 is lost (e.g. `pay_captured_unposted` and `pay_duplicate` for the
  multiple-success and backstop Kinds; the drift sweep itself for drift). A Kind with no backstop makes
  (b) mandatory.
- *Failure-path P1s whose business transaction rolls back* (§28.8) use a detached `Raise` in a fresh
  transaction. "A rolled-back event raises no alert" applies to event alerts only.

**I-wire:** in order:
1. The post-E2 multiple-success site (`orchestrator.go:989-1024`).
2. `index_backstop_fired` (`:1021`).
3. The reconciliation P1s:
   - the drift P1 (`reconciliation/scheduler.go:333-341`);
   - the `MISMATCH FOUND` sites at `scheduler.go:420` (sportsbook settlement), `:487` (casino
     consistency), `:562` (casino statement) and `:719` (payment statement).
4. The kill-switch engage alert.
5. The integrity-alert sites, including `payment_deposit_simulation_handlers.go:240`. It is included
   because it is the same payload-mismatch class as the deposit webhook alert. Its route is
   test-support only, so the alert is Kind-tagged as simulation and never paged as a production P1.

The reconciliation sites raise from the scheduler's own per-tenant transaction and are not financial
evidence transactions, so the ADR 0102 non-financial rule applies to them. Logs are retained, and the
dispatcher is wired into `main.go`.

**Tests:**
- The LF I list: failure injected inside T10/T13d → the dispute and receipt commit with a uniform 200;
  a detached P1 persists; dedup under N raisers; every reconciliation P1 site wired; the no-savepoint
  mutant.
- Security addendum §1(e):
  - an injected `Raise` failure inside T10/T13d commits the dispute or receipt, returns the uniform
    200, **and fires the post-commit detached `Raise`**;
  - an already-aborted outer transaction (25P02) is **not masked**;
  - the metric increments;
  - MUT: no savepoint; swallowing the outer-transaction error.
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
| Staff creation | A tenant caller can create `tenant_admin`, `support` **and `compliance`** staff with a chosen password and `person_id` (`admin_routes.go:491-550,656`; `compliance` per code-reviewer K10). Only `finance`, `risk_manager`, `promotions_manager` and `bonus_operations` are platform-only (S-1). |
| Four-eyes patterns | withdrawal (`0026:107`), assets (`0044`), bonus + `bonus_approval_policies` (`0063:32-84`, payload match), casino catalogue (`0086`), provider credentials (`0096`), kill-switch release (`0105:150-167,243-247`, `decided_txid`) |
| Platform principal | `WithPlatformAdmin` (`db/tenant_rls.go:112-133`) never sets `app.tenant_id`; the 0105 resolver raises on mixed platform+tenant sessions (`0105:27-50`) (S-3) |
| Ledger | `ledger.Post` (the only entry); `LockProjectionsForPosting` (`lockorder.go:103-124`); the reason-code check (`lockorder.go:355-357`); BONUS_SET mirror legs auto-generated (HR-9 **removed**, `ledger.go:26-29`; HR-17 forbids hand-built mirrors) (LF-9) |
| Audit | `audit_log` (`0014`, `0016`) |

**Security fact (S-1), stated in ADR 0099.** Under the current unverified identity model,
distinct-principal and distinct-Person checks do **not** prove two humans. A tenant caller can create
`tenant_admin`, `support` and `compliance` staff with a chosen `person_id`. The distinct-Person
triggers stay as defence in depth. Structural prevention depends on HD-PRH2-2 (c) or (d).

**K1 go/no-go condition (security S-1 item 2, binding).** K1 is **not implemented** on an HD-PRH2-2
answer of (a), or of (b) with only a tenant-local grant approver, **unless** grantees are restricted to
platform-minted accounts or a platform co-approval is required. If the human's answer does not include
one of those, K1 goes back to security review and does not proceed. The K lane stays blocked. A person
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
  - `withdrawal.go` is not edited (payments F3).
- **S-12 beneficiary exclusion, M1 and M2.** Neither the resolution requester nor any approver may
  resolve to the Person who owns the attempt's player account. This covers the deposit attempt owner
  for M1 and the payout attempt owner for M2. It is enforced by trigger
  `payment_manual_resolutions_beneficiary_guard` (0114), on top of the withdrawal guards for M2.

**Tests (K1–K3):**
- The LF K2 and K3 lists and the QA W1/W2/W4 K items.
- The S-4 AZ cases and the S-11 races.
- The S-12 beneficiary refusal, for K2 targets and for **both** M1 and M2 attempt owners.
- **The sock-puppet case is refused** (QA W1), as far as the accepted HD-PRH2-2 option structurally
  allows:
  - under (d), a grant to a tenant-minted account is refused by trigger;
  - under (c), a grant without platform co-approval is refused;
  - the same Person under two principals is refused under every option.

  K1 never ships on an answer that cannot pass this test (see the K1 go/no-go condition).

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
| **0102** | Durable alerting and provider-neutral delivery | S-7 items 1–4, LF-7, and the security addendum §1 ruling with conditions (a)–(e), including the per-Kind reconciliation-backstop list (§5-I) |
| **0103** | Casino launch-token bootstrap contract | S-5 order |
| **0104** | Tenant-visible audit of platform actions | S-10 |

Amendments:
- ADR 0095: §4.4 (D), §8 (C, including the `DepositResult` amount echo), §15.3 (E1 + IC F3),
  §29 (E2), §7.3 (H + S-8 + the security addendum §2: resolution-only sweeping of non-active
  tenants).
- ADR 0096 §16 (F).
- ADR 0097 §8 (J).
- **ADR 0082:** lock order for the K2/K3 request rows before L3 (LF-13).
- Any "(a)" answer to HD-PRH2-1 needs a **CLAUDE.md amendment record**, not just an ADR note (PO F1,
  security §4).

## 7. Human decisions (not decided here)

| ID | Question | Options | Consequences |
|---|---|---|---|
| **HD-PRH2-1** | **The floor today (CLAUDE.md, permanent):** "Manual balance adjustments require a reason code and four-eyes approval **above a configurable threshold**". The threshold is configurable and can be zero, which means "always". The four-eyes capability itself cannot be removed. **Unless the human amends CLAUDE.md, (b) is the engineering floor.** The question: may a policy switch four-eyes off entirely for manual adjustments and/or force-resolve? And for `above_threshold`, is there a maximum (a ceiling) a threshold may be set to? | (a) `never` allowed per tenant/jurisdiction with a legal-review reference — **needs an explicit CLAUDE.md amendment**; (b) no `never`; thresholded four-eyes only (the current rule); (c) force-resolve always four-eyes, adjustments per (a) or (b) | **Combined consequence:** (a) together with HD-PRH2-2 (a) or (b) lets one human move money alone. **Without a ceiling, (b) is (a) in disguise**, because a huge threshold equals no four-eyes. Distinct-Person is **not** part of this question: ADR 0098 §2 already requires an independent approver, so it is a non-configurable floor (LF-11). |
| **HD-PRH2-2** | Does granting a money-moving capability itself need further control? **The fact (S-1, with code-reviewer K10):** a `tenant_admin` can create `tenant_admin`, `support` and `compliance` accounts, choosing their password and `person_id`, and Persons are unverified self-registrations. So today's identity model cannot tell two approvers from one human. | (a) a single authorized admin grants; (b) grants need a second, tenant-local approver (configurable); (c) a platform co-approval is required for tenant grants of money-moving capabilities; (d) tenant admins grant, but only to staff accounts a **platform caller created** (extending the Stage 3D `finance` precedent) | **Only (c) or (d) structurally prevent unilateral money movement by one tenant admin until verified staff identity exists.** (b) does not close it: a second minted tenant admin satisfies it. (c) adds platform approval load for every B2B grant. (d) adds platform involvement in onboarding each B2B approver. K1 stays blocked until this is answered. An answer of (a), or of (b) without platform-minted grantees or platform co-approval, sends K1 back to security review, and K1 does not proceed (S-1 item 2). |
| **HD-PRH2-3** | Confirmation: is any monetary threshold needed **before implementation**? The architect and ledger-finance say no. | Confirm, or name a value requirement | `above_threshold` rows are **per-asset** (minor units differ). No migration seeds a policy row, and fixtures use synthetic, test-only values. Real values are set later, per tenant and jurisdiction, under legal review. |
| **HD-PRH2-4** | Alert routing: destinations, the severity→channel matrix, on-call ownership, ack expectations; later, a paging vendor | Mock sink now; the matrix stays a placeholder | Until decided, P1s are durable and visible but nobody is paged. PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking. |
| **HD-PRH2-5** | Do tenant-visible platform actions show platform staff identity? | (a) full identity; (b) pseudonymous "platform operator" plus an internal reference; (c) per tenant contract | A privacy and B2B-expectation question. **Engineering rule under every option:** platform staff IP, user agent and free-form metadata are never disclosed to tenants. Until answered, the read API shows (b) (fail closed on disclosure). |
| **HD-PRH2-6** | May a **platform**-scope capability holder adjust or force-resolve money in a tenant operating under **its own licence** (ADR 0006)? | (a) yes, all tenants; (b) only tenants under the platform licence; (c) per-tenant opt-in | (a) and (c) need a new "platform principal acting in tenant X" RLS family (S-3), which is new privileged power with its own threat model. (b) needs a reliable per-tenant licensing-mode attribute. **Evidence (code-reviewer K14):** `tenants.licensing_model` exists (`migrations/0001:13`: NOT NULL, CHECK in `('under_platform_licence','own_licence')`), is kept consistent by 0007/0017, and is already read at `casino/orchestrator.go:728-741` and `bonus/eligibility.go:220`. No Go path UPDATEs it. **Whether it is fit to gate authorization is security's call**, not settled here. **Until answered, K2/K3 fail closed: platform scope is refused on tenant ledgers.** |
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
| Alerting aborts a financial tx, or loses a P1 | LF-7 savepoint rule with security addendum (a)–(e): narrow swallow, post-commit detached retry, metric, per-Kind reconciliation backstop |
| Sweeper dispatches money for a suspended tenant | Resolution-only for non-active tenants (security addendum §2), with an in-tx status read and tests |
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
| S-1 High | §5-K "Security fact" (now incl. `compliance`), the "K1 go/no-go condition" (item 2, binding), K1 tests; §7 HD-PRH2-2 (re-framed, option (d), go/no-go); §2 K1 block; §9 |
| S-2 High | §5-K2 "Policy authoring"; §4 0112; §7 HD-PRH2-7; §2 K2 block |
| S-3 | §5-K1; §4 0111/0112/0114 RLS column; §7 HD-PRH2-6 |
| S-4 | §5-K1; §5-K inventory row |
| S-5 | §5-B; §1-B; §2 W2; ADR 0103 |
| S-6 | Merged with LF-4: §5-D; §1-D |
| S-7 | Merged with LF-7: §5-I; §4 0110; §6 0102. Item 5 is **resolved** by the security addendum §1 (split ACCEPTED, conditions (a)–(e)), and is in §5-I "Ruling" and tests |
| S-8 | Merged with LF-8: §5-H |
| Security addendum §1 (alert split, a–e) | §5-I "Ruling" and tests; §6 0102; §9 |
| Security addendum §2 (H, non-active tenants) | §5-H "Non-active tenants are resolution-only" and tests; §6 ADR 0095 §7.3; §9 |
| S-9 | §5-C; §1-C |
| S-10 | §5-G1; §4 0109; ADR 0104 |
| S-11 | §5-K1 |
| S-12 | §5-K2; §5-K3 (M1 **and** M2 attempt owners); §4 0112, and 0114 trigger `payment_manual_resolutions_beneficiary_guard` |
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
| LF-16 | §1-E2; §5-E2, on the corrected premise (code-reviewer E2d). The parity scope is re-confirmed by ledger-finance at E2's gate (§2 W1). |
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
| F6 | **Fulfilled** by `reviews/code-reviewer-verification.md` (VERIFIED WITH CORRECTIONS); all 12 corrections applied (below) |
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

| **Code-reviewer verification (rev 2 → rev 3)** | |
| 1 E2 premise (E2d) | §1-E2, §5-E2: the "only path" claim is removed; `:1169`, `TestX5` and the fixture at `:344` are cited; LF-16 re-confirmation at E2's gate |
| 2 E2 sites (E2c) | §1-E2, §5-E2: 21 sites in 10 files, incl. `:1774` |
| 3 §1-F (F3) | §1-F: only T1p **allow** records no decision; deny records via `withdrawal.go:1114` |
| 4 §1-C (C5, C7) | §1-C: posting at `drive.go:354`, tombstone at `:329`; empty Pending reference via `MarkAccepted` → `0101:90-93`, then 0099 |
| 5 `compliance` (K10) | §5-K inventory, the S-1 fact, §7 HD-PRH2-2 |
| 6 HD-PRH2-6 evidence (K14) | §7 HD-PRH2-6: `0001:13`, 0007/0017, consumers; fitness left to security |
| 7 Alert inventory (I4/I6) | §1-I, §3 I-wire row, §5-I I-wire: `scheduler.go:420,487,562,719` and `payment_deposit_simulation_handlers.go:240` included, simulation Kind-tagged and never paged |
| 8 K1 sock-puppet | §5-K "K1 go/no-go condition", K1–K3 tests aligned with QA W1 (refused); §7 HD-PRH2-2; §2 W1 |
| 9 Header binding set | Header: LF-1, 2, 3, 4, 5, 7, 9, 10 + the LF-13 ruling; LF-11, 14 and 15 labelled as adopted-binding |
| 10 S-12 | §5-K3; §4 0114 (named trigger) |
| 11 `PayoutKYCGate` (H3) | §1-H, §5-H (+ wiring test) |
| 12 Chain order (E2a) | §1-E2: `:674 → :555 → :700` |
| Additional sweep (`HealthStatus`, `HandleCallback`) | Noted. Not a correction; the per-adapter contract check stays with each real-adapter security review (M5 / PROV-OUTBOUND-CRED-1). |

Nothing is marked "not adopted", and no open split remains. S-7 item 5 is resolved by the security
addendum.

## 11. Authorization and human decisions (orchestrator, 2026-09-28)

**The human authorized the plan at `0ea367d`**. Covered: W0; documentation, ADR and registry work; A, C, D, E1, E2, F, G, H, I and J; B after A. K may start only once the decisions below are incorporated into ADRs 0099–0101 and verified by security and ledger-finance. Not authorized: real providers, credentials, AWS, staging, branch protection and billing. The decisions are recorded in `docs/decisions/0098-…` §5 and are **binding on this plan**. Where they differ from §5-K or §7 above, §11 wins.

| Decision | Consequence for the design |
|---|---|
| HD-PRH2-2 = (c) | K1: a tenant admin may **request** a financial capability grant for a user in its tenant. The grant takes effect only after **platform co-approval** by an independent platform principal with the grant-approval capability. A platform grant (to platform or tenant-scoped staff) also needs an independent second platform approver. Always refused: self-grant, self-approval, the approver being the requester or the grantee, and a tenant admin requesting platform scope or another tenant. The capability enum is extensible: new capabilities are added by migration and ADR, not per-tenant custom roles. The distinct-Person check stays as defence in depth (S-1). |
| HD-PRH2-7 | K2: platform financial approval policy rows are written only by platform principals holding the policy-admin capability. Tenant and brand rows may only **tighten**, enforced in the DB resolver/trigger (S-2 ii/iv). Every change is audited, effective-dated and append-only, so historical evaluation is reproducible. |
| HD-PRH2-1 | K2: a standing platform **financial-control classification** marks which operation kinds are in the mandatory four-eyes class (manual adjustment and force-resolve are in it). For that class, policy can set thresholds and profiles but has no `never` mode. An operation outside the class is represented explicitly by the classification, not by a policy switch. No CLAUDE.md amendment. |
| HD-PRH2-6 = yes, explicit grants only | K1/K2/K3: the "platform principal acting in tenant X" session family (S-3) is built. It is valid only when the principal holds an **explicit tenant-scoped grant for X**; platform employment confers nothing. It is trigger-validated and audited with both principal and tenant, and limited to the K tables and the governed ledger post. It is the same framework for platform-licence and own-licence tenants. The interim "refuse platform scope on tenant ledgers" rule is replaced by "refuse unless an explicit tenant grant exists". |
| HD-PRH2-3 | No threshold values; per-operation, tenant, jurisdiction, profile and asset configurability; synthetic values in fixtures only. |
| HD-PRH2-4 | I-core adds a routing-configuration table (routes per severity and scope to channel-kind + recipient **reference**), delivery state, retry, and escalation state (escalation step and next-escalation-at). **No recipients are seeded.** With no route configured, an alert stays `undelivered/unrouted`: visible, counted, and itself raising a platform warning. The real recipients are HD-PRH2-4-OPS. |
| HD-PRH2-5 | G1's tenant read projection shows the **identifiable** platform actor (staff id and display name) with the approval chain, reason and before/after where recorded. IP, user agent and free-form metadata are excluded from the tenant **presentation** by default as a privacy-presentation setting; they stay in the record, and the setting is configurable (orchestrator interpretation in 0098 §5). |

**New workstream E3: SB-CATALOGUE-IO-1** (classified at the human's request; registry row). Owner: sportsbook; reviewers: architect, security, code-reviewer.
- Change the `Provider.Catalogue()` contract to `Catalogue(ctx) (CatalogueResult, error)`.
- Fetch and validate **outside** any transaction, then upsert inside `WithPlatformService`.
- Add a txscope-held refusal (the same guard as the other adapters), plus tests: no transaction is open during the provider call; a fetch error writes nothing; the validation bound still holds.
- No migration.
- Files: `internal/sportsbook/{catalogue.go,types.go,mock.go}` and the three-line call in `cmd/platform-api/main.go`. E3 merges **before H starts**, because H owns `main.go`.

**Dispatch order** (orchestrator decision, dependency-based): workstreams that need no new ADR (A, E2, F-kyc, E3) start in parallel with the W0 ADR drafting. W0's exit conditions gate only the workstreams that depend on those ADRs:

| Workstream | Waits for |
|---|---|
| B | ADR 0103 |
| G1 | ADR 0104 |
| I | ADR 0102 |
| K | ADRs 0099–0101 |

The payments lane stays strictly serial (E2 → C → D → F-pay → H → I-wire → K3). There is one owner per critical file. The orchestrator is the single writer of the registry, `progress.md`, `active-stage.md`, `project-status.md` and `HANDOVER.md`.

**Orchestrator decision (2026-09-28, ADR 0104 §5.4):** the identifiable actor needs a display name, and `staff_users` has none. Option **(a)** is chosen: migration 0109 adds a nullable `staff_users.display_name`, and the projection shows the staff id plus the display name when set. Options (b) (the email, which is more personal data) and (c) (the id only, which falls short of HD-PRH2-5) are rejected. Reversible.

**Touch-list additions (from ADR 0099/0101 drafting, Rule 1):** K1 also touches `internal/db/tenant_rls.go` (the new `WithPlatformActingInTenant` setter); K3 also touches `internal/providerref/providerref.go` (refusing the reserved `platform-operator-declared:` prefix). New human decision HD-PRH2-8 (below-threshold semantics) is registered; the stricter reading (b) is enforced in the interim, so K2 is not blocked.
