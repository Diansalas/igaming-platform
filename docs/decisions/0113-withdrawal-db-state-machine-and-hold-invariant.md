# ADR 0113 - Withdrawal database state machine and the hold invariant (#12)

Status: **ACCEPTED - pending implementation** (design only; no code or SQL exists for it). Author: `architect`. Base:
`86b1a0d`. Rev 2 applies both independent reviews in full (§11): `ledger-finance` (REJECT as written -> APPROVE WITH
CONDITIONS once B1/B2 are applied) and `security` (APPROVE WITH CONDITIONS). Reviewers of every slice: `ledger-finance`
(money path; owns invariant #12), `security` (live runtime-credential re-run), `code-reviewer`, `qa`; `payments` owns
the fixture migration. Closes, when built: ADR 0112 §12 item 14(a) (ledger closure arm), the open part of 14(b2)
(state-transition guard, LAUNCH-BLOCKING) and `withdrawal-state-machine.md` §3's invariant-#12 `NOT IMPLEMENTED`.
Everything below is `NOT IMPLEMENTED` until its slice lands.

## 1. Context (verified at `86b1a0d`)

- `withdrawal_requests` (`0026`) has a 9-value `state` CHECK (`0026:17-20`), `hold_ledger_transaction_id`,
  `release_ledger_transaction_id`, `provider_id`, `provider_reference`. UPDATE guards today: immutable identity/amount/
  binding (`0026:53-72`, replaced `0123:513`); governed hold-release guard (`0124:768-801`); approved-hold freeze
  (`0124:809-835`); acting `UPDATE` policy (`0115:1491`, last replaced `0125:1268`). INSERT/DELETE guards: `0131`
  (`WD010`/`WD011` subject active, `WD012` no delete/truncate), `0126` (binding required).
- **No guard on the transition graph or on the inserted state.** In a tenant-staff session (`0026:82-91`) the runtime
  role can set any state, e.g. `requested -> cancelled -> requested` on a suspended brand (live-confirmed by
  `security`), and can INSERT a row directly as `approved`/`submitted`/`completed`. Nothing ties a state to its ledger
  leg, so the `LA023` backstop (`0130:426-447`) counts workflow state the runtime credential can rewrite.
- **Invariant #12** (`ledger-accounting-model.md` row 12; `withdrawal-state-machine.md` §3): every open
  `player_withdrawal_hold` credit matches exactly one non-terminal request of matching amount. Not checked anywhere
  (`internal/reconciliation/reconciliation.go:117-215`); the closure ledger arm is ADR 0112 §5.2 / §12.14(a).

## 2. The legal edges that exist today (derived from code, nothing invented)

Every production writer is in `internal/withdrawal/withdrawal.go` (grep of `UPDATE`/`INSERT INTO withdrawal_requests`
outside `_test.go` and of all `migrations/*.up.sql`: no SQL function writes the table). Callers by grep `withdrawal\.<Fn>(`.

| # | Edge | Writer | Ledger leg in the SAME tx | Columns | Callers / session |
|---|---|---|---|---|---|
| E0 | INSERT `requested` | `RequestWithdrawal` `:542` | none yet (row first) | - | `httpserver/withdrawal_handlers.go:153`, tenant |
| E0b | `requested`, hold NULL->X | `RequestWithdrawal` `:623` | `withdrawal_requested`, key `<id>`, corr `<id>`: Dr `player_cash` / Cr `player_withdrawal_hold`, same wallet, `amount` (`:587-617`) | hold link | same tx as E0 |
| E1 | `requested -> pending_review` | `MoveToPendingReview` `:699` | none | state | `withdrawal_handlers.go:406`, tenant |
| E2 | `requested -> cancelled` | `Cancel` `:1791` | `withdrawal_rejected`, key `<id>:cancelled`, reverses hold, Dr hold / Cr cash | state, release | `withdrawal_handlers.go:1451`, tenant |
| E3 | `pending_review -> approved` | `Approve` `:935` | none; `approve` approvals row first (`:863`) | state | `withdrawal_handlers.go:746`, tenant |
| E4 | `pending_review -> rejected` | `Reject` `:1073` | `withdrawal_rejected`, key `<id>:rejected`, reverses hold; `reject` row first (`:1027`) | state, release | `withdrawal_handlers.go:844`, tenant |
| E5 | `approved -> rejected` (KYC) | `DenyForCompliance` `:1240` | `withdrawal_rejected`, key `<id>:kyc_denied`, reverses hold | state, release | `payments/payout.go:382`, tenant |
| E6 | `approved -> rejected` (governed) | `ReleaseForGovernedResolution` `:1878` | `withdrawal_rejected`, key `<id>:governed_hold_released` (`0124`) | state, release | `payments/withdrawal_hold_resolution.go:628`, **acting** |
| E7 | `approved -> submitted` | `MarkSubmittedPending` `:1428`; `MarkSubmitted` `:1522` (tests only) | none | state, `provider_id` (+`provider_reference` for `MarkSubmitted`) | `payout.go:453`, tenant |
| E8 | `submitted`, reference NULL->X | `AttachProviderReference` `:1477` | none | `provider_reference` | `payout.go:750,783,880,918,1255,1272,1308`; `payout_destination.go:627` |
| E9 | `submitted -> completed` | `Complete` `:1610` | `withdrawal_completed`, key `provider_id:provider_tx_id`, corr `<id>`: Dr hold / Cr `psp_clearing` | state, release | `payout.go:887`; acting M2 `manual_resolution.go:987`; acting M4 `manual_resolution_m4.go:321` |
| E10 | `submitted -> failed` | `Fail` `:1706` | `withdrawal_failed`, key `<id>:failed`, reverses hold | state, release | `payout.go:922`; acting M2 `manual_resolution.go:991`; acting M4 `manual_resolution_m4.go:325` |

**Not legal (refused):** `completed -> reversed` (drawn in the state doc, no writer; `TxWithdrawalReversed`
`ledger.go:114` is never posted; reviewers confirmed nothing needs it), every edge out of a terminal state, backwards
or skipping edges, ADR 0107 §7.2 CT-PRE (design only). Each needs an ADR that amends the guard in the migration adding
its writer. Terminal: `rejected`, `cancelled`, `failed`, `completed`, `reversed`.

## 3. Decision part 1 - the guard (immediate, BEFORE INSERT and BEFORE UPDATE)

Trigger `withdrawal_requests_aa_state_transition_guard`, **BEFORE INSERT OR UPDATE** FOR EACH ROW, no `WHEN` clause
(HN-4 lesson, ADR 0111 §16), `search_path` pinned, not SECURITY DEFINER, no role exempt. `aa_state` sorts before
`aa_subject_active_guard` (`0131`) and `approved_hold_freeze` (`0124`), so an illegal shape is refused before any gate
key is taken; a catalog test pins the order. SQLSTATE class `WD` (only `WD010-012` exist today).

0. **INSERT (B1)** (`WD025`): refuse unless `NEW.state = 'requested'` and `hold_ledger_transaction_id`,
   `release_ledger_transaction_id`, `provider_id`, `provider_reference` are all NULL. A row is born only as E0.
1. **Terminal rows immutable** (`WD020`): `OLD.state` terminal -> refuse any UPDATE, including `updated_at` only.
2. **Edge allowlist** (`WD021`): `(OLD.state, NEW.state)` is one of E1-E10 or equal (same-state).
3. **Column discipline** (`WD022`), via `to_jsonb(NEW) - cols` vs `to_jsonb(OLD) - cols` (`0124:787`) so a future
   column is frozen by default: hold NULL->value only while `OLD.state = NEW.state = 'requested'`, then set-once;
   release NULL->value exactly on an edge into a terminal state (E2, E4, E5, E6, E9, E10), NULL on every non-terminal
   state, never changed; `provider_id` set on E7 and **E7 requires `NEW.provider_id IS NOT NULL`** (no `submitted`
   row with NULL `provider_id`), immutable after; `provider_reference` NULL->value only on E7 or same-state
   `submitted` (E8), set-once; `updated_at` free on a non-terminal row; all else unchanged (`0123:513`).
4. **Workflow record for E3/E4** (`WD023`): E3 needs an `approve` and E4 a `reject` row in `withdrawal_approvals` for
   the request. **This proves a workflow record exists, not who acted** (C-WD023): `0034` admits a caller-named
   principal and automated rows with an unknown principal, so it is **not** a four-eyes control. Binding the decision
   to the `0120` signed actor proof is a tracked follow-up (`security`), not this ADR.
5. **E5 (C5): admitted on edge + ledger shape only.** `DenyForCompliance` inserts the KYC decision row AFTER the
   UPDATE (`withdrawal.go:1268`) with `CorrelationID = uuid.New()` (`payout.go:123`), and `kyc_enforcement_decisions`
   has no txid, so a same-txid evidence check is unenforceable. No `WD024` is built. Any future evidence check goes
   into the deferred check A, after `payments`/`kyc` set the decision `correlation_id` to the request id (follow-up).

**Who may perform each edge.** The database proves edge, columns, workflow record and (part 2) the ledger leg; it does
not identify actors on ordinary edges. Session shape narrows it: acting sessions reach only E6/E9/E10 (`0125:1268`
plus the `0115`/`0124`/`0125` executing-window guards, unchanged); tenant-staff sessions reach E1-E10 except E6
(`0124:768` refuses the governed key outside an executing resolution); player sessions have no INSERT/UPDATE policy
(`0026:96-100`). The freeze still refuses `approved -> *` on a non-active subject.

**Effect on 14(b2).** Revival is `WD020`; a direct `approved`/`submitted`/`completed` INSERT is `WD025`; deletion is
`WD012`. A closure-transaction trick must post a genuine, correctly linked release to `player_cash`, which releases
the funds - a legitimate outcome, not a bypass of `LA023`.

## 4. Decision part 2 - ledger coupling and invariant #12

### 4.1 Options

| Option | What | Verdict |
|---|---|---|
| A. Per-request deferred link check | At COMMIT for each touched request: hold/release links have the exact §2 shape | Adopted |
| B. Hold-entry immediate guard + deferred companion | Every hold-account entry is a pinned leg of a named request in the right pre-state, and at COMMIT that request links the transaction | Adopted (rev 2: B alone was insufficient, §4.2) |
| C. Per-wallet deferred aggregate | At COMMIT the wallet hold balance equals the non-terminal sum | Rejected: old drift would block the very release that repairs it |
| D. Reconciliation (detective) | Hourly standing check from `ledger_entries` | Adopted as backstop |

### 4.2 Why rev 1 was wrong (B2) and the corrected B

Rev 1 claimed A + B imply #12. They did not: A fires only for request rows touched in the transaction, and rev 1's B
accepted any Dr-hold leg of type rejected/failed/completed and any Cr `withdrawal_requested` leg whose correlation
named a request of matching wallet/asset/amount, with no key or link check. Two concrete failures: (i) a standalone
second `withdrawal_rejected`/`withdrawal_failed` (Dr hold / Cr cash, fresh key) against a live `submitted` or a
`completed` request pays the player twice; (ii) a second `withdrawal_requested` leg strands funds in the hold and
makes `LA025` refuse closure forever. Corrected B has two parts.

**B-i, `ledger_entries_withdrawal_hold_guard`** (BEFORE INSERT FOR EACH ROW; C4 mechanics: **no `WHEN` clause**; the
name sorts after `ledger_entries_populate_from_account_trigger` (`0022:73`), which fills `wallet_id`, pinned by a
catalog order test; **fast exit when `NEW.wallet_id IS NULL`**; then **exactly one `ledger_accounts` PK read** to get
the account type, exit unless `player_withdrawal_hold`; **no row locks**; a request or transaction not visible to the
session raises `WD040`, fail closed). For a hold entry it requires (`WD040`/`WD041`):
- transaction type in {`withdrawal_requested`, `withdrawal_rejected`, `withdrawal_failed`, `withdrawal_completed`},
  `correlation_id` names a request of the same tenant with `wallet_id = entry.wallet_id`, equal `asset_code`, equal
  `amount`;
- **idempotency key pinned per type:** `withdrawal_requested` = `<id>`; `withdrawal_rejected` =
  `<id>:{cancelled|rejected|kyc_denied|governed_hold_released}`; `withdrawal_failed` = `<id>:failed`;
  `withdrawal_completed` = `request.provider_id || ':' || tx.provider_tx_id` and `tx.provider_id =
  request.provider_id` (slice 1 confirms M2/M4 pass `res.ProviderID` equal to the request's `provider_id`:
  `manual_resolution.go:987`, `manual_resolution_m4.go:321`);
- **request pre-state:** release link NULL; a Cr hold leg only for `withdrawal_requested` while `state = 'requested'`
  and the hold link is NULL; a Dr hold leg only for the other three types and only while the request is non-terminal.

**B-ii, `ledger_entries_withdrawal_hold_link_at_commit`** (CONSTRAINT TRIGGER AFTER INSERT ON `ledger_entries`,
DEFERRABLE INITIALLY DEFERRED, FOR EACH ROW, same fast exits as B-i): at COMMIT the named request must link this
entry's transaction as its `hold_ledger_transaction_id` (Cr leg) or `release_ledger_transaction_id` (Dr leg), and A's
rules plus `WD034` must hold for that request (`WD042`). This is the companion that catches the standalone second
leg: its request is never re-touched, so A alone would not fire, but B-ii does. **Correction to rev 1:** deferred
triggers fire in event-queue order, not name order, so a `zz_` prefix is irrelevant and no ordering is claimed.

**A, `withdrawal_requests_ledger_link_at_commit`** (CONSTRAINT TRIGGER AFTER INSERT OR UPDATE, DEFERRABLE INITIALLY
DEFERRED, FOR EACH ROW; re-reads the row by id like `0124:589-644`). **Session shape first (C-A):** before any read it
raises `WD030` unless `app.player_account_id` is empty and `app.tenant_id` (or, in an acting session,
`app.acting_tenant_id`) equals the row tenant. A player-scoped session sees the request but only its own ledger
entries, so the "exactly two entries" count could pass blind. Then, raising `WD030` if anything is not visible:
- hold link NOT NULL (`WD031`); hold tx: same tenant, `withdrawal_requested`, key `<id>`, corr `<id>`, exactly two
  entries Dr `player_cash` / Cr `player_withdrawal_hold`, both on the wallet, asset, amount (`WD032`);
- non-terminal: release NULL; terminal: release NOT NULL, same tenant, corr `<id>`, exactly two entries, Dr hold on the
  wallet, and per state (`WD033`) `cancelled` -> `withdrawal_rejected` `:cancelled`; `rejected` -> `withdrawal_rejected`
  `:rejected|:kyc_denied|:governed_hold_released`; `failed` -> `withdrawal_failed` `:failed` (all three reverse the hold
  and credit `player_cash` on the wallet); `completed` -> `withdrawal_completed`, key and provider as in B-i, Cr
  `psp_clearing` (asset);
- `WD034`: the sum of all legs on the wallet's hold account whose transaction has `correlation_id = id` equals `amount`
  if non-terminal and `0` if terminal (`idx_ledger_transactions_correlation`, `0021:54`).

**Why A + B-i + B-ii now imply #12.** From a clean start (asserted by §6): every hold entry is a pinned leg of a named
request in the right pre-state (B-i); every committed hold entry is linked by that request (B-ii); hold and release
links are set-once and only one release can be linked (§3 items 1, 3); so each request contributes exactly `amount`
while non-terminal and `0` once terminal, and nothing else touches a hold account. Hence each wallet's hold balance
equals its non-terminal sum. D covers what triggers cannot (owner `DISABLE TRIGGER`, ADR 0110 §9.4; history).

**Concurrency.** No new lock anywhere. Writers already hold L1 (request `FOR UPDATE`) and `ledger.Post`'s L3
projection locks (ADR 0082), so lock order is unchanged. A and B-ii read at commit (READ COMMITTED); under REPEATABLE
READ a concurrent writer on the same request is serialised by L1. B-i reads a request inserted earlier in the same
transaction (E0 `:542` before the post `:617`). **Hot path (C4):** casino bet/win entries pay only the NULL check or
one PK read; slice 1 adds a casino bet/win latency benchmark gate (regression budget set by `ledger-finance`).

### 4.3 The ledger closure arm (`LA025`)

`launch_subject_status_guard` (`0130` text + one marked block), when `NEW.status = 'closed'`, inside the RLS-bound
read window it already opens (`0130:433-443`): refuse `LA025` if any `wallet_balance_projection` row of the subject
(tenant; for a brand joined through `wallets.brand_id`) has `account_type = 'player_withdrawal_hold'` and
`credit_total <> debit_total`. `LA023` stays alongside it. The Go readiness pre-check (`launchgov/readiness.go:455-464`)
gains the same arm with a typed error.
- **Projection read** is accepted by both reviewers for this refusal-only check: the projection is written by trigger
  in the same transaction as each entry (`0023`) and is already the readiness source (`readiness.go:399,415`); a
  zero-showing drift is caught by `ledger_vs_projection` and D.
- **Index (C3):** `wallet_balance_projection (tenant_id) WHERE account_type = 'player_withdrawal_hold'` only; the
  totals are filtered on the heap. A predicate on `credit_total`/`debit_total` would defeat HOT updates on the
  hottest table.
- **Race:** hold-increasing legs are only E0b (B-i), and E0 takes the tenant and brand gate keys SHARED (`0131:76,84`),
  so a closure (keys EXCLUSIVE) sees the committed hold or the insert sees `closed`. Releases only lower balances.

### 4.4 The reconciliation check (D)

Appended to `RunLedgerVsProjection` as a standing, unwindowed check like `unlinkedManualAdjustments`
(`reconciliation.go:199-203,225`); runs for non-active tenants too (`scheduler.go:265`). Per hold account: balance
rebuilt **from `ledger_entries`** vs the non-terminal sum on that wallet/asset, plus A's link rules read-only. Kinds
`withdrawal_hold_open_mismatch` and `withdrawal_request_ledger_link_invalid` (P1), added to the `mismatch_kind` CHECK
(`0115:1753`). **C7:** the comparison runs as one SQL statement, or under `db.WithTenantSnapshot` (REPEATABLE READ,
`internal/db/tenant_snapshot.go:39`), so an in-flight withdrawal cannot raise a false P1; D **fails closed** (run
error, not `clean`) when the tenant's withdrawal/ledger counts read under its `set_config` are zero while the
owner-side count is not. Supporting index `withdrawal_requests (wallet_id, asset_code) WHERE state IN
('requested','pending_review','approved','submitted')`.

## 5. Writer and fixture compatibility (C6)

**Production:** all 13 Go sites in §2 satisfy the guard, A and B as written; no change in `internal/withdrawal`.
`WD02x`/`WD03x`/`WD04x` map to internal errors (500 + audit), never a client-visible detail.

**Rules for tests:** no adversarial test is deleted. A test that proved `HR0xx`/`MR041` and now meets `WD0xx` first is
re-driven through a legal edge with the deeper fault, and must still raise its original code, shown by a mutant that
drops the deeper guard. The shared legal-path helper (E0 -> E1 -> E3 -> E7 through the Go writers) runs as
`igaming_runtime`, never as owner. No trigger exemption.

**Raw `INSERT INTO withdrawal_requests` in `_test.go` (28 sites, 22 files):**

| Site(s) | Class | Old | New |
|---|---|---|---|
| `httpserver/iwire_webhook_reversal_link:40`; `httpserver/launch_authorisation_harness:326`; `reconciliation/payment_statement_fixround:123`, `payment_statement:714`, `prh2_r9_bound_clear:98,339`, `prh2_r16_res1_tighten:74`; `payments/pay_callback_mismatch_bind:520`, `dep_ref_validate:451`, `b13b_destination:457`, `migration_0101:231`, `rvlf_i1_regression:488`; `launchgov/closure:51` | setup: direct non-`requested` state, no hold | success | `WD025` -> legal-path helper |
| `payoutinstrument/migration_0123:314,400` | migration test seeding legacy states | success | unaffected at its pinned pre-M-A version; else `WD025` -> helper |
| `payoutinstrument/migration_0126:112`; `binding:35` (success cases), `binding:167`; `state:476`; `httpserver/admin_withdrawal_instrument_view:160`; `withdrawal/migration_0082_brand_pinning:78`; `payments/r31_settle_instrument_state:730` (success cases); `launchgov/withdrawal_insert_guard:50` control (`:71`) | setup: `requested`, no hold in the tx | success | `WD031` at commit -> post the hold and link it in the same tx (helper) |
| `payoutinstrument/binding:118` (PI043), `binding:35` refusal cases (PI04x); `withdrawal/b13b_binding:327` (PI046); `withdrawal/adversarial:116` (cross-tenant); `withdrawal/migration_0082_brand_pinning:58` (FK); `payments/r31:551,768` (PI041 / per state); `launchgov/withdrawal_insert_guard` matrix (`WD010`/`WD011`) | adversarial, immediate refusal | as listed | unchanged (fires before deferred A); asserted |
| `payoutinstrument/binding:65` (`plantLegacy`, owner `DISABLE TRIGGER USER`) | owner tamper plant | success | unchanged insert; any later UPDATE of the row hits `WD031` -> plant also writes the hold leg with triggers off |

**Hand-built postings to the hold account in tests (>= 7 files):** `risk/cumulative_leg:101`;
`reconciliation/payment_statement_fixround:107`, `payment_statement:697`, `prh2_r9_bound_clear:82,285,323`,
`prh2_r16_res1_tighten:58`, `prh2_r8_standing1_payout_unbound:69`: old success, new `WD040`/`WD041` (no request, wrong
key or pre-state) -> helper. `ledger/lockorder:126,229,739`, `ledger/replay:152`: the hold account is used as a
generic second account; new `WD040` -> switch to a non-hold account of the same lock class (`ledger-finance` confirms
lock-order coverage is kept). `casino/stage9_concurrency:479` is a read: unaffected. **Slice 1 record (code review LOW):** the ledger lock-order
test no longer posts to a hold account (a hand-built hold leg is `WD040`); hold-account projection locks under
concurrency are covered through the real writers instead: `withdrawal`
`TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds` (concurrent E0b on one wallet) and
`TestADR0113_ConcurrentEdgeRaces` (`-race`, x20: Cancel/MoveToPendingReview, Approve/Reject, Complete/Fail,
DenyForCompliance/MarkSubmittedPending, each with a hold leg), and `casino` stage-9 bet-vs-withdrawal race.

**Raw `UPDATE` fixtures (38 sites, 14 files**: `payments/hsec_hold_resolution_*`, `k3_*`, `m4_resolve1_*`, `r31_*`;
`payoutinstrument/{binding,state,migration_0123}_*`; `httpserver/{hsec_hold_resolution_api,launch_authorisation_http}_*`;
`withdrawal/adversarial_test.go`**):** classified the same way in slice 1, each listed with file:line, class, old and
new SQLSTATE in the slice-1 PR.

## 6. Migration plan and down plan

Numbers are allocated by the orchestrator (next free `0132`).
- **M-A (slice 1):** guard (§3), A, B-i, B-ii, the request index. **Pre-flight, fail loud, never fix:** before creating
  anything, assert per tenant (the `0115:70-90` `set_config` loop; FORCE RLS makes a plain `SELECT` blind, the
  `0048`/`0092`/`0107` lesson) that every row satisfies §3 items 0 and 3 and A; no row is `reversed`; every hold
  entry satisfies B-i and is linked (B-ii); every hold account's rebuilt balance equals its non-terminal sum. On any
  failure `RAISE` with per-class counts; the remedy is a human decision (rows are never deleted or edited). A test
  seeds one dirty row per class and asserts the refusal (proves the check is not blind).
- **M-B (slice 2):** `launch_subject_status_guard` = 0130 text + the `LA025` block; the C3 projection index.
- **M-C (slice 3):** the two mismatch kinds (CHECK = 0115 list + 2).
- **Down:** M-A drops its triggers, functions and index (no data change). M-B restores the 0130 body byte for byte;
  M-C restores the 0115 CHECK and fails loud if rows of the new kinds exist (never deleted to make a down pass).
  Whole-schema snapshot and up/down/up tests per migration.

## 7. Tests (as `igaming_runtime`, private scratch DB, `-race -tags integration`)

- **INSERT matrix (B1):** direct INSERT of each of the 8 non-`requested` states, and `requested` with each of the four
  link/provider columns set: `WD025`. Attack sequences: insert the header first then UPDATE to `approved`; direct
  `approved`, `submitted`, `completed` inserts, in tenant and acting sessions.
- **Graph matrix:** every (from, to) pair x {tenant, acting, player} session: exactly E1-E10 admitted in §2 shapes;
  others refused with the pinned SQLSTATE (incl. `completed -> reversed`, `cancelled -> requested`).
- **14(b2) exploit, live:** `requested -> cancelled -> requested` on a suspended brand refused; same inside a closure
  transaction; closure still refuses `LA023`.
- **Columns:** re-point/clear hold or release; release on a non-terminal edge; E7 with NULL `provider_id`; change
  `provider_id` after E7; overwrite `provider_reference`; `updated_at` on a terminal row; a future column frozen.
- **Workflow record:** E3 without `approve`, E4 without `reject`, E3 with only `reject`: `WD023`.
- **B2 double-pay / strand attacks:** a standalone second `withdrawal_rejected` and `withdrawal_failed` (fresh key)
  against a `submitted` and a `completed` request; a second `withdrawal_requested` leg on a `requested` request with
  a hold link; a correctly keyed leg whose request is never linked: each refused (`WD041` immediately or `WD042` at
  commit). Manual adjustment, deposit and casino postings to a hold account: `WD040`. Every other account type
  unaffected (type-blind mutant).
- **A:** NULL hold at commit; foreign hold or release; wrong type/key/direction; `completed` linked to a
  `withdrawal_rejected`; a third correlated hold leg (`WD034`); `app.player_account_id` set to the row's player ->
  `WD030` (C-A); acting session reads OK; `SET CONSTRAINTS ALL IMMEDIATE` fails closed.
- **B-i mechanics (C4):** catalog order after `ledger_entries_populate_from_account_trigger`; not-visible request ->
  `WD040`; casino bet/win latency benchmark gate.
- **`LA025` (C8):** orphan hold planted by the owner with triggers disabled refuses tenant and brand closure;
  blind-RLS mutants for BOTH the projection read and the `wallets.brand_id` join must fail the tests; `LA023` still
  fires alongside.
- **Concurrency:** Cancel vs MoveToPendingReview; Approve vs Reject; Complete vs Fail (M2 vs callback); E0 vs closure in
  both orders (`LA025`); DenyForCompliance vs governed release. One winner, the loser `ErrStateConflict` or a pinned
  SQLSTATE, the hold nets to zero, no deadlock (lock-order harness unchanged).
- **D (C7):** clean -> clean; planted drift -> both kinds, standing; non-active tenant observed; in-flight withdrawal
  during the run -> no false P1; blind `set_config` read -> run error, not clean.
- **Regression:** full `withdrawal`, `payments`, `launchgov`, `reconciliation`, `ledger`, `casino`, `risk` suites;
  mutants: drop the `aa_` prefix, add a `WHEN` clause, skip a visibility raise, remove the B-ii companion.

## 8. Human/legal versus engineering

- **Orchestrator defaults (reviewer-supported, reversible by the owner):** Q1 - refuse closure on a non-zero hold
  balance even when nothing can resolve it yet (`LA025`); suspension is the lever. Q2 - no pseudonymisation exemption
  now.
- **HD-RET-1 (OPEN, owner/legal):** terminal rows become fully immutable on top of `0131`'s no-delete. Any future
  pseudonymisation exemption from `WD020` is allowed only for a DB-proven governed actor (`0120` signed proof) with an
  audit row, never through a GUC or role flag, and never for state, amount or ledger-link columns.
- **Residual 9.2 is a HIGH launch-risk item for human acceptance** (below).
- **Engineering (decided here):** edge set, SQLSTATEs, A + B-i + B-ii + D, projection closure arm, fixture policy,
  migration split. **Not decided:** `completed -> reversed`, ADR 0107 CT-PRE, PAYOUT-AMOUNT-DISPUTE-1 / HD-CTF-4.

## 9. Residuals (stated honestly)

1. The table owner can `DISABLE TRIGGER` (ADR 0110 §9.4); D detects the result, nothing prevents it.
2. **HIGH, not launch-blocking by itself:** a holder of a compromised runtime credential can still drive a legal edge
   with a forged but well-shaped leg, e.g. E9 to `psp_clearing` (a forged payout completion). It is bounded by the
   detection in payout-statement reconciliation. It is recorded for human acceptance **on condition that statement
   reconciliation is scheduled for every live payout provider**. Moving the check to `payment_attempts` only relocates
   the forgery unless the terminal state is bound in the database to provider evidence.
   **Slice 1 review (security C2, ledger-finance L2), recorded:** the double-pay fix (B-i/B-ii) covers legs on the
   HOLD account. Migration 0132 additionally pins every `withdrawal_*` transaction to a REQUIRED hold leg at COMMIT
   (`WD043`, its hold leg then subject to B-i/B-ii/A), which closes the two non-hold shapes found live: a
   `withdrawal_rejected` Dr `psp_clearing` / Cr `player_cash` with a fresh key against a COMPLETED request, and a
   `withdrawal_completed` Dr `player_cash` / Cr `psp_clearing`; `withdrawal_reversed` (no writer) is refused end to
   end. It also requires every `player_*` ledger account to have a wallet (security C1: a wallet-less
   `player_withdrawal_hold` skipped both hold triggers). What REMAINS, for human acceptance under this residual:
   value can still leave `player_cash` through postings of OTHER types (e.g. a `manual_adjustment` Dr
   `manual_adjustment` / Cr `player_cash` is admitted at the database level with no approval: residual 9.5 /
   ADR 0113 governed adjustments are a Go-level control), and a well-shaped forged E9 leg as above. Detection: payout
   statement reconciliation (scheduled for every live payout provider) and reconciliation D (slice 3).
3. `WD023` is not a four-eyes control (§3 item 4): the runtime credential can write an AUTOMATED approval record
   and satisfy it (security C3, confirmed live; accepted residual). Binding approve/reject records to the `0120`
   signed actor proof stays on the launch list (`security`).
4. E5 carries no decision evidence (§3 item 5).
5. The four-eyes count and threshold remain Go-only (state doc §3).
6. **WD043 lists its types explicitly (ledger-finance, slice 1 focused check).** The `withdrawal_*` transaction
   types are exactly the five `ledger.go` constants (`withdrawal_requested`, `_rejected`, `_failed`, `_completed`,
   `_reversed`), and the WD043 trigger function and pre-flight class 9 name those five. A FUTURE `withdrawal_*`
   type is not caught automatically by the database; the static test
   `internal/ledger/withdrawal_types_wd043_static_test.go` fails when a new `withdrawal_` `TransactionType`
   constant appears without a recorded WD043 decision, or when the latest migration defining the WD043
   function does not list it.
7. **Known constraint on WITHDRAWAL-REVERSAL-1** (provider return after completion): WD043 requires a hold leg on
   `withdrawal_reversed` and B-i refuses every hold leg of that type, so the type is refused end to end. Building
   the reversal flow needs a NEW migration that relaxes or reshapes WD043 (and B-i) for that type, with
   `ledger-finance` sign-off. 0132 is not edited.
8. **Pre-flight class 9 backstop.** The per-tenant RLS loop cannot see a ledger row whose `tenant_id` names no
   `tenants` row (`ledger_transactions.tenant_id` has no FK to `tenants`). Class 9 therefore also has an
   RLS-independent owner count (`withdrawal_tx_without_hold_leg_unscoped`, FORCE RLS lifted and restored inside
   the migration transaction, the 0124 pattern). Classes 1-7 keep the loop-only scope stated in §6 (covered by D);
   class 8 is backstopped by the validated CHECK itself.
9. **Deploy check (role naming).** 0132 revokes EXECUTE on `withdrawal_request_ledger_link_violation` from PUBLIC
   and grants it only to a role named exactly `igaming_runtime`, if one exists. If the production runtime role has
   a different name (the Terraform variable `db_runtime_username` is configurable), A and B-ii fail closed
   (permission denied) for every withdrawal write. Before release, verify the role name against
   `deploy/init-app-role.sql` and `db_runtime_username` (runbook section 17).

## 10. Work slices (ordered; none starts without orchestrator authorisation)

| Slice | Content | Owner | Reviewers |
|---|---|---|---|
| 1 | M-A: guard (INSERT + UPDATE), A, B-i, B-ii, pre-flight; fixture re-inventory and legal-path helper; §7 tests up to concurrency; casino latency gate | `ledger-finance` (SQL), `payments` (fixtures) | `security` (live re-run incl. 14(b2) and B1/B2 attacks), `code-reviewer`, `qa`, `architect` |
| 2 | M-B: `LA025` + readiness arm + C8 tests | `ledger-finance` + `launchgov` owner | `security`, `code-reviewer`, `qa` |
| 3 | M-C + D: reconciliation check, kinds, scheduler, P1 alert kind, C7 | `ledger-finance` | `qa`, `code-reviewer` |
| 4 | Docs: state doc §1/§3, ADR 0112 §12.14, registry, decision register (Q1/Q2 defaults, residual 9.2), HANDOVER | `architect` | orchestrator |

Slice 1 alone closes the LAUNCH-BLOCKING part of 14(b2); slices 2-3 close 14(a) and invariant #12's label.

## 11. Review disposition (rev 2)

| Item | Source | Applied in |
|---|---|---|
| B1 guard also on INSERT (`WD025`), INSERT matrix | both | §3 item 0, §7 |
| B2 A + B did not imply #12; pinned keys, pre-state, deferred companion, ordering claim corrected | both | §4.2, §7 |
| C3 projection index without totals predicate | `ledger-finance` | §4.3 |
| C4 B-i mechanics, benchmark gate | `ledger-finance` | §4.2, §7, §10 |
| C5 E5 shape-only, `WD024` dropped | both | §3 item 5, §9.4 |
| C6 fixture blast radius, no deleted adversarial test, mutants, runtime helper | `ledger-finance` | §5 |
| C7 D consistent snapshot and fail-closed | `ledger-finance` | §4.4, §7 |
| C8 `LA025` blind-RLS mutants, `LA023` kept | `security` | §4.3, §7 |
| C-A A session shape `WD030` | `security` | §4.2, §7 |
| C-WD023 not a four-eyes control | `security` | §3 item 4, §9.3 |
| LOWs: completed provider equality; E7 `provider_id` NOT NULL; HD-RET-1 exemption rule; residual 9.2 HIGH | both | §4.2, §3, §8, §9 |
| Owner questions Q1/Q2 | orchestrator defaults | §8 |

## 12. Implementation record: slices 2 and 3 (`ledger-finance`; IMPLEMENTED against a private local database, pending reviews)

**Slice 2 (M-B = migration `0133`).** `idx_wallet_balance_projection_withdrawal_hold` (`tenant_id`) WHERE
`account_type = 'player_withdrawal_hold'` (C3: no totals predicate; a catalog test pins that no index of the
projection names `credit_total` / `debit_total` / `updated_at`, so the trigger's update stays HOT-eligible).
`launch_subject_status_guard` = 0130's body byte for byte plus one `LA025` block in the closure branch, inside the
existing RLS-bound window, after `LA023` and `LA024` (so existing codes are unchanged where several apply). Down
restores 0130's body verbatim (whole-schema snapshot test). Go arm: hard precondition
`H-6:withdrawal_hold_balance_non_zero` at request and under the executor's subject locks, typed as
`PreconditionError{Base: ErrWithdrawalsHolding}` (same closed API code `LAUNCH_WITHDRAWALS_HOLDING_FUNDS`; the check
code carries the distinction, no new API code); readiness `Info["withdrawal_hold_accounts_non_zero"]` and the
closure-only notice `NoticeFundsHoldBlocksClosure`. **Q1 default applied:** a stranded hold (no hold-bearing
request) refuses the closure; suspension is the lever (tested). Race: a hold-increasing leg is E0b only, whose
insert takes the gate keys SHARED, so in the hold-first order the closure waits and is refused (`LA023` fires first;
with `LA023` disabled by a mutant, `LA025` alone refuses: mutation evidence `S2M08`).

**Slice 3 (M-C = migration `0134` + check D).** Kinds `withdrawal_hold_open_mismatch` and
`withdrawal_request_ledger_link_invalid` (CHECK = 0115 list + 2; down fails loud `WD099` while findings exist).
D is appended to `RunLedgerVsProjection` as specified in s4.4 (so the hourly scheduler, the non-active observation
sweep and the in-tx P1 raise come with it; discriminator `stream:ledger_withdrawal_hold` under the drift Kind,
like K2). ONE SQL statement (C7): per wallet, the hold balance rebuilt from `ledger_entries` vs the non-terminal
sum, plus check A's link rules set-wise (parity-tested against `withdrawal_request_ledger_link_violation`).
**Deviation (fail-closed definition):** a TRUE owner-side count is not available to the runtime credential (no
BYPASSRLS; FORCE RLS binds the owner too; no SECURITY DEFINER per repo policy). D fails closed instead on (i) a
session that is not exactly the tenant scope and (ii) a zero count under the tenant's `set_config` while an
independently policied count is not (requests vs E0b transactions, both directions; hold entries vs hold
projection rows). A policy change blinding all of these tables at once is not detectable at run time (residual;
the whole-schema snapshot tests pin the policies). **Defect fixed on the way (C7):** the pre-existing
ledger_vs_projection compare read the rebuild and the projection in two statements, so a posting committing
between them raised a FALSE `balance_mismatch` P1 under live traffic (reproduced by
`TestADR0113D_ConcurrentLiveWithdrawals_NoFalseP1`); both are now one statement per account (`rebuildAndProject`).
**Not covered by D (residual 9.2):** an unattributed hold-leg pair that nets to zero on its wallet (no balance
left) is not invariant #12 drift; B-ii prevents it while enabled, payout statement reconciliation detects a forged
completion.
