# ADR 0113 - Withdrawal database state machine and the hold invariant (#12)

Status: **PROPOSED** (design only; no code or SQL in this change). Author: `architect`. Base: `86b1a0d`.
Reviewers required before any slice merges: `ledger-finance` (money path; owns invariant #12), `security`
(runtime-credential attack re-run), `code-reviewer`, `qa`. `payments` owns the fixture migration in slice 1.
Closes, when built: ADR 0112 §12 item 14(a) (ledger closure arm) and the open part of 14(b2) (state-transition guard,
tracked LAUNCH-BLOCKING), and `withdrawal-state-machine.md` §3's `NOT IMPLEMENTED` invariant-#12 check.
Labels below: everything here is `NOT IMPLEMENTED` until the matching slice lands.

## 1. Context (verified at `86b1a0d`)

- `withdrawal_requests` (migration `0026`) carries `state` (9-value CHECK, `0026:17-20`), `hold_ledger_transaction_id`,
  `release_ledger_transaction_id`, `provider_id`, `provider_reference`. The only DB guards on UPDATE are:
  immutable identity/amount/binding columns (`0026:53-72`, replaced by `0123:513`); the governed hold-release guard
  (`0124:768-801`, fires on every UPDATE, pins the executing-window shape); the approved-hold freeze (`0124:809-835`,
  `approved -> *` refused on a non-active subject except the governed release); the acting `UPDATE` policy (`0115:1491`,
  last replaced `0125:1268`) that limits acting sessions to an executing M2/M4/hold-release. Guards on INSERT/DELETE:
  `0131` (`WD010`/`WD011` subject active, `WD012` no delete/truncate) and `0126` (binding required).
- **No guard on the transition graph.** In a tenant-staff session (`tenant_staff_scope`, `0026:82-91`) the runtime role
  may set any state, e.g. `requested -> cancelled -> requested` on a suspended brand (live-confirmed by `security`;
  ADR 0112 §13 last review round). Nothing ties a state to its ledger leg in the database, so the `LA023` closure
  backstop (`0130:426-447`) counts workflow state that the runtime credential can rewrite.
- **Invariant #12** (`ledger-accounting-model.md` table row 12; `withdrawal-state-machine.md` §3): every open
  `player_withdrawal_hold` credit matches exactly one non-terminal request with a matching amount. `NOT IMPLEMENTED`:
  `internal/reconciliation` runs `ledger_vs_projection` plus the standing `unlinkedManualAdjustments` check only
  (`reconciliation.go:117-215`). The closure ledger arm is a recorded residual (ADR 0112 §5.2 `:303-309`, §12.14(a)).

## 2. The legal edges that exist today (derived from code, nothing invented)

Every production writer of `withdrawal_requests` is in `internal/withdrawal/withdrawal.go` (grep of `UPDATE
withdrawal_requests` / `INSERT INTO withdrawal_requests` outside `_test.go`, and of every `migrations/*.up.sql`: no SQL
function writes the table). Callers are listed by grep of `withdrawal\.<Fn>(`.

| # | Edge | Writer (line) | Ledger leg in the SAME tx | Columns changed | Callers / session |
|---|---|---|---|---|---|
| E0 | INSERT `requested` | `RequestWithdrawal` `:542` | none yet (row first) | - | `httpserver/withdrawal_handlers.go:153`, tenant session |
| E0b | `requested`, hold link NULL->X | `RequestWithdrawal` `:623` | `withdrawal_requested`, key `<id>`, corr `<id>`: Dr `player_cash` / Cr `player_withdrawal_hold`, same wallet, `amount` (`:587-617`) | `hold_ledger_transaction_id` | same tx as E0 |
| E1 | `requested -> pending_review` | `MoveToPendingReview` `:699` | none | state | `withdrawal_handlers.go:406`, tenant |
| E2 | `requested -> cancelled` | `Cancel` `:1791` | `withdrawal_rejected`, key `<id>:cancelled`, reverses hold: Dr hold / Cr cash | state, release | `withdrawal_handlers.go:1451`, tenant (player actor) |
| E3 | `pending_review -> approved` | `Approve` `:935` | none; an `approve` `withdrawal_approvals` row is inserted first (`:863`) | state | `withdrawal_handlers.go:746`, tenant |
| E4 | `pending_review -> rejected` | `Reject` `:1073` | `withdrawal_rejected`, key `<id>:rejected`, reverses hold; a `reject` approvals row first (`:1027`) | state, release | `withdrawal_handlers.go:844`, tenant |
| E5 | `approved -> rejected` (KYC) | `DenyForCompliance` `:1240` | `withdrawal_rejected`, key `<id>:kyc_denied`, reverses hold | state, release | `payments/payout.go:382`, tenant |
| E6 | `approved -> rejected` (governed) | `ReleaseForGovernedResolution` `:1878` | `withdrawal_rejected`, key `<id>:governed_hold_released` (guarded by `0124`) | state, release | `payments/withdrawal_hold_resolution.go:628`, **acting**, executing hold resolution |
| E7 | `approved -> submitted` | `MarkSubmittedPending` `:1428` (dispatch); `MarkSubmitted` `:1522` (tests only) | none | state, `provider_id` NULL->X (+`provider_reference` for `MarkSubmitted`) | `payments/payout.go:453`, tenant |
| E8 | `submitted`, reference NULL->X | `AttachProviderReference` `:1477` | none | `provider_reference` | `payout.go:750,783,880,918,1255,1272,1308`, `payout_destination.go:627` |
| E9 | `submitted -> completed` | `Complete` `:1610` | `withdrawal_completed`, corr `<id>`, provider ids set: Dr hold / Cr `psp_clearing` (asset) | state, release | `payout.go:887`; acting M2 `manual_resolution.go:987`; acting M4 `manual_resolution_m4.go:321` |
| E10 | `submitted -> failed` | `Fail` `:1706` | `withdrawal_failed`, key `<id>:failed`, reverses hold | state, release | `payout.go:922`; acting M2 `manual_resolution.go:991`; acting M4 `manual_resolution_m4.go:325` |

**Not legal today (refused by the guard):** `completed -> reversed` (drawn in `withdrawal-state-machine.md` §1, but
no writer exists; `TxWithdrawalReversed` is declared at `internal/ledger/ledger.go:114` and never posted), any edge out
of a terminal state, any backwards edge, any edge skipping a step, and ADR 0107 §7.2's CT-PRE
`requested|pending_review -> rejected` (`:closed_tenant_released`; design only). Each of these requires an ADR that
amends the guard in the same migration that adds its writer. Terminal states: `rejected`, `cancelled`, `failed`,
`completed`, `reversed`.

## 3. Decision part 1 - transition guard (immediate, BEFORE UPDATE)

New trigger `withdrawal_requests_aa_state_transition_guard`, BEFORE UPDATE FOR EACH ROW, no `WHEN` clause (the HN-4
lesson, ADR 0111 §16), function pins `search_path`, not SECURITY DEFINER, binds every role. The `aa_` infix makes it
fire before `withdrawal_requests_approved_hold_freeze` (name order), so an illegal edge is refused before the freeze
takes the tenant gate key. New SQLSTATEs in the unused-elsewhere `WD` class (grep: only `WD010-012` exist):

1. **Terminal rows are immutable** (`WD020`): if `OLD.state` is terminal, refuse any UPDATE, including an
   `updated_at`-only touch. No production writer updates a terminal row (every writer CASes on a non-terminal state).
2. **Edge allowlist** (`WD021`): `(OLD.state, NEW.state)` must be one of E1-E10, or equal (same-state update).
3. **Column discipline per edge** (`WD022`), checked with `to_jsonb(NEW) - cols` vs `to_jsonb(OLD) - cols` (the
   `0124:787` technique) so a future column is frozen by default:
   - `hold_ledger_transaction_id`: NULL -> value only while `OLD.state = NEW.state = 'requested'`; never changed or
     nulled afterwards (set-once, `withdrawal-state-machine.md` §3).
   - `release_ledger_transaction_id`: NULL -> value exactly on an edge into a terminal state (E2, E4, E5, E6, E9,
     E10); NULL on every non-terminal state; never changed.
   - `provider_id`: NULL -> value only on E7; immutable after.
   - `provider_reference`: NULL -> value only on E7 or on a same-state `submitted` update (E8); set-once.
   - `updated_at`: free on a non-terminal row. Every other column: unchanged (already enforced by `0123:513`).
4. **Decision evidence for the two human edges** (`WD023`): E3 requires an `approve` row and E4 a `reject` row in
   `withdrawal_approvals` for this request. Both writers insert it before the UPDATE (`:863`, `:1027`), and that insert
   is itself guarded by `withdrawal_approvals_enforce_governance` (`0034`: linked, active staff Person; not the
   beneficiary). The threshold/count rule stays Go-only (§3 of the state-machine doc, unchanged). This turns a raw
   state forge into "forge a governed approval row", which `0034` refuses for an unlinked or self identity.
5. **E5 evidence** (`WD024`, conditional on verification in slice 1): `DenyForCompliance` writes a denied
   `kyc_enforcement_decisions` row in the same tx (`withdrawal.go:1268`). If that row carries the request
   correlation, E5 requires it (same-txid); if it does not, E5 is admitted on edge + ledger shape only and this is
   recorded as a residual. Not invented here; `security` confirms the column in slice 1.

**Who may perform each edge.** The database does not identify actors on ordinary edges; it proves edge legality,
column discipline, evidence rows and (part 2) the ledger leg. Session shape still narrows it: acting sessions reach
only E6/E9/E10 through `acting_update` (`0125:1268`) plus the `0115`/`0124`/`0125` executing-window guards (unchanged);
tenant-staff sessions reach E1-E10 except E6 (`0124:768` refuses the governed key outside an executing resolution);
player-scoped sessions have no UPDATE policy (`0026:96-100`). The `0124` freeze keeps refusing `approved -> *` on a
non-active subject, so on a suspended or closed subject the runtime role can only move `requested`/`pending_review`
rows, and only along E1/E2/E3/E4 with real evidence and a real release leg.

**Effect on 14(b2).** `requested -> cancelled -> requested` is refused (`WD020`); a closure-transaction trick must now
post a genuine, correctly shaped release to `player_cash` (actually releasing the funds), which is not a bypass of
`LA023` but a legitimate outcome. Deletion is already refused (`0131`).

## 4. Decision part 2 - ledger coupling and invariant #12

### 4.1 Options

| Option | What | Benefit | Cost / risk (`ledger-finance` to rule) |
|---|---|---|---|
| A. Per-request deferred check (DB) | Constraint trigger at COMMIT on each touched request: hold and release links have the exact shape of §2 | Local, O(entries of one request) via `idx_ledger_transactions_correlation` (`0021:54`); refuses at the source | Does not by itself see a hold posting that names no request |
| B. Hold-account entry guard (DB) | BEFORE INSERT on `ledger_entries` for `player_withdrawal_hold` accounts: posting must be a withdrawal leg of an existing request on the same wallet/asset/amount | Closes "hold without request" at write time; cheap (one PK read) | Adds a branch to a hot-path trigger set; must stay type-blind for every other account |
| C. Per-wallet deferred aggregate (DB) | At COMMIT, hold balance of the wallet == sum of non-terminal amounts | Literal invariant #12 | Any pre-existing drift then blocks every later posting on the wallet, including the release that would repair it (stranding amplifier); cross-row check in commit path |
| D. Reconciliation (detective) | Hourly standing check, every tenant incl. non-active | Catches owner bypass, disabled triggers, history; no hot-path cost | Detects, does not prevent |

### 4.2 Decision (recommended): A + B in the database, D as the detective backstop; C rejected

By induction from a clean start (asserted by the migration, §6), A + B make invariant #12 hold for every commit made
with the triggers enabled: B means only withdrawal legs of a real request ever touch a hold account; A means each
request has exactly one hold leg while non-terminal and exactly one matching release leg once terminal, so the
per-wallet hold balance equals the non-terminal sum. D covers what triggers cannot: the table owner disabling them
(ADR 0110 §9.4, ADR 0112 §12.3), rows created before the migration and projection drift. C is rejected for the
stranding-amplifier reason; it is strictly implied by A + B and verified by D.

**A, `withdrawal_requests_zz_ledger_link_at_commit`** (CONSTRAINT TRIGGER AFTER INSERT OR UPDATE, DEFERRABLE INITIALLY
DEFERRED, FOR EACH ROW; re-reads the row by id at commit like `0124:589-644`; **raises `WD030` if the row or its ledger
rows are not visible to the committing session** - the 0124 C-1 lesson; never returns silently):
- `hold_ledger_transaction_id` NOT NULL (`WD031`): this closes "a `requested` row with no hold" (state doc §4).
- Hold tx: same tenant, type `withdrawal_requested`, `idempotency_key = id::text`, `correlation_id = id`, exactly two
  entries: Dr `player_cash` and Cr `player_withdrawal_hold`, both on `wallet_id`, `asset_code`, `amount` (`WD032`).
- Non-terminal: release NULL. Terminal: release NOT NULL, same tenant, `correlation_id = id`, exactly two entries,
  Dr `player_withdrawal_hold` (wallet, asset, amount), and by state (`WD033`):
  `cancelled` -> `withdrawal_rejected`, key `<id>:cancelled`, reverses the hold tx, Cr `player_cash` (wallet);
  `rejected` -> `withdrawal_rejected`, key in {`:rejected`, `:kyc_denied`, `:governed_hold_released`}, reverses hold,
  Cr `player_cash`; `failed` -> `withdrawal_failed`, key `:failed`, reverses hold, Cr `player_cash`;
  `completed` -> `withdrawal_completed`, provider ids NOT NULL, Cr `psp_clearing` (asset). The key is deliberately
  not pinned for `completed` (`provider:txid`, M2 `platform-operator-declared:<res>`, M4 `provider:R` all differ).
- Per-request hold net (`WD034`): sum of this request's legs on the wallet's hold account = `amount` if non-terminal,
  `0` if terminal (one statement over the two linked transactions plus any other tx with `correlation_id = id`
  touching the hold account - the latter must be none).

**B, `ledger_entries_withdrawal_hold_guard`** (BEFORE INSERT, `WHEN` account type is resolved inside; type-blind
fast exit for every other account, `WD040`): an entry on a `player_withdrawal_hold` account requires its transaction
type in {`withdrawal_requested`, `withdrawal_rejected`, `withdrawal_failed`, `withdrawal_completed`} and
`correlation_id` naming a `withdrawal_requests` row of the same tenant with `wallet_id = entry.wallet_id`,
`asset_code` and `amount` equal; Cr only for `withdrawal_requested`, Dr only for the other three. Verified compatible:
no Go writer other than `withdrawal.go` names the hold account (grep `AccountPlayerWithdrawalHold`: `ledger.go:69`
declaration, `wallet.go:175` read-only), and governed manual adjustments allow only `player_cash`/`manual_adjustment`
(`0113:1557`). It runs alongside, not instead of, `ledger_entries_governed_fence` (`0124:879`).

**Concurrency.** Neither A nor B takes a lock. Every writer already holds L1 (request `FOR UPDATE`) and the L3
projection locks of `ledger.Post` (ADR 0082), so the lock order is unchanged and the lock-order static tests need no
new class. A reads at commit under READ COMMITTED; under REPEATABLE READ a concurrent writer on the same request is
already serialised by L1 (serialization failure, not a stale pass). B reads the request row inserted earlier in the
same tx (E0 precedes the hold post, `:542` before `:617`).

### 4.3 The ledger closure arm for `LA023` (new `LA025`)

`launch_subject_status_guard` (`0130`, `CREATE OR REPLACE` from 0130's exact text plus one marked block), when
`NEW.status = 'closed'`, inside the same RLS-bound read window it already opens (`0130:433-443`): refuse `LA025` if any
`wallet_balance_projection` row of the subject (tenant; for a brand, joined through `wallets.brand_id`) has
`account_type = 'player_withdrawal_hold' AND credit_total <> debit_total`. The Go readiness pre-check
(`launchgov/readiness.go:455-464`) gains the same arm with a typed error.

- **Projection, not a ledger rebuild, under the gate lock.** The projection is written by trigger in the same
  transaction as every entry (`0023`), it is already the readiness source (`readiness.go:399,415`), and a full
  per-wallet `ledger_entries` aggregate under the gate lock is the cost ADR 0112 §5.2 rejected. Drift between the two
  is the `ledger_vs_projection` P1 already scheduled hourly; D (below) re-checks holds from entries. Direction of
  failure: a projection showing non-zero refuses (safe); a projection wrongly showing zero is caught by D.
  **`ledger-finance` must rule this explicitly** (condition of slice 2).
- **Performance.** Partial index `wallet_balance_projection (tenant_id) WHERE account_type = 'player_withdrawal_hold'
  AND credit_total <> debit_total`: the read is O(open holds of the tenant), index upkeep only on hold postings.
- **Race.** Hold-increasing postings are only E0b (enforced by B), and E0 takes the tenant and brand gate keys SHARED
  (`0131:76,84`) before the post, so a closure (keys EXCLUSIVE) either sees the committed hold or the insert sees
  `closed`. Releases only lower the balance. No new lock.
- `LA023` (request-state arm) stays; with part 1 its count can no longer be rewritten by the runtime role. `LA025`
  adds the orphan-hold population (ADR 0112 §12.14(c), HD-CTF-4) that `LA023` cannot see.

### 4.4 The reconciliation check (D)

Appended to `RunLedgerVsProjection` as a standing, unwindowed check, exactly like `unlinkedManualAdjustments`
(`reconciliation.go:199-203, 225`); runs for non-active tenants too (`scheduler.go:265` observation). Per hold account,
balance rebuilt **from `ledger_entries`** vs the sum of non-terminal request amounts on that wallet/asset, plus a
per-request link check (A's rules, read-only). New mismatch kinds `withdrawal_hold_open_mismatch` and
`withdrawal_request_ledger_link_invalid` (P1), added to the `mismatch_kind` CHECK last replaced in `0115:1753`. Cost:
O(hold entries of the tenant) per hourly run; supporting index `withdrawal_requests (wallet_id, asset_code) WHERE state
IN ('requested','pending_review','approved','submitted')`. `withdrawal-state-machine.md` §3 is relabelled only when D
lands and runs in the scheduler.

## 5. Writer compatibility

- **Production (Go):** all 13 sites in §2 satisfy the guard and A/B as written; no Go change is required in
  `internal/withdrawal`. Error mapping: `WD02x`/`WD03x`/`WD040` map to `ErrStateConflict`-class internal errors
  (500 + audit), never to a client-visible detail. `MarkSubmitted` (tests only) remains legal (E7).
- **Governed paths:** E6 (0124), M2 E9/E10 (0115), M4 E9/E10 (0125) keep their own guards; part 1 adds nothing that
  conflicts (verified against `0124:785-790`: E6 changes only state, release, `updated_at`).
- **Test fixtures that shortcut state with raw SQL** (38 sites in 14 files: `internal/payments/hsec_hold_resolution_*`,
  `k3_*`, `m4_resolve1_*`, `r31_*`; `internal/payoutinstrument/{binding,state,migration_0123}_*`;
  `internal/httpserver/{hsec_hold_resolution_api,launch_authorisation_http}_*`; `internal/withdrawal/adversarial_test.go`).
  Classify each: (a) adversarial, expects refusal -> keep, re-pin the expected SQLSTATE (some now fail at `WD02x`
  before reaching `HR0xx`/`MR041`; where the original guard must still be proven, drive a legal edge with a malformed
  link so the deeper guard is the one that fires); (b) setup shortcut (e.g. `SET state = 'approved'`, the
  `launch_authorisation_http` `SET state = 'rejected' WHERE tenant_id`) -> replace with a shared test helper that
  walks the real Go transitions. **No owner exemption** in the trigger (the `0131` rule: a scratch database is
  recreated, not patched).

## 6. Migration plan and down plan

Two migrations (numbers allocated by the orchestrator; next free is `0132`):

- **M-A (slice 1):** guard (§3), A, B, the two indexes. **Pre-flight, fail loud, never fix:** before creating anything,
  assert over every tenant (the `0115:70-90` per-tenant `set_config` loop, because FORCE RLS makes a plain `SELECT`
  blind - the `0048`/`0092`/`0107` lesson) that: every row has a valid hold link (A rules); every terminal row has a
  valid release link and every non-terminal row has none; no row is `reversed`; no hold-account entry violates B;
  every hold account's rebuilt balance equals its non-terminal sum. On any failure `RAISE` with per-class counts and
  the operator diagnostic query in a comment; remedy is a human decision (CLAUDE.md: rows are never deleted or edited).
  A test seeds one dirty row per class on a scratch DB and asserts the refusal, proving the check is not blind.
- **M-B (slice 2):** `launch_subject_status_guard` = 0130 text + the `LA025` block; projection partial index; the two
  mismatch kinds (CHECK = 0115 list + 2).
- **Down:** M-A drops its triggers, functions and indexes (no data change). M-B restores the 0130 function body and the
  0115 CHECK byte for byte (whole-schema snapshot test, the 0124 practice); re-adding the CHECK fails loud if
  rows of the new kinds exist (never deleted to make a down pass). Up/down/up is tested.

## 7. Tests (as `igaming_runtime`, private scratch DB, `-race -tags integration`)

- **Graph matrix:** every (from, to) pair of the 9 states x {tenant session, acting session, player session}: exactly
  E1-E10 admitted in the shapes of §2; all others refused with the pinned SQLSTATE. Includes `completed -> reversed`,
  `cancelled -> requested`, `rejected -> approved`, `submitted -> approved`, `requested -> approved`.
- **The 14(b2) exploit, live:** `requested -> cancelled -> requested` on a suspended brand is refused; the same in a
  closure transaction; closure then still refuses `LA023`.
- **Column discipline:** re-point hold, re-point/clear release, set release on a non-terminal edge, change
  `provider_id` after E7, overwrite `provider_reference`, `updated_at` touch on a terminal row, a future column added
  in a test schema is frozen (jsonb technique).
- **Evidence:** E3 without an `approve` row, E4 without a `reject` row, E3 with only a `reject` row: refused.
- **Ledger coupling (A):** commit with NULL hold; hold tx of another request/wallet/amount/asset; release of the wrong
  type/key/direction; `completed` linked to a `withdrawal_rejected`; terminal with a third correlated hold leg;
  invisible row at commit (player-scoped session) raises `WD030`; `SET CONSTRAINTS ALL IMMEDIATE` fails closed.
- **Hold guard (B):** manual adjustment, deposit, casino and raw `withdrawal_requested` postings to a hold account
  with no/foreign/mismatched request: refused; every other account type unaffected (type-blind mutant).
- **Closure arm:** orphan hold (planted with triggers disabled by the owner on a scratch DB) refuses `LA025` for tenant
  and brand; zero holds admit; blind-RLS mutant must fail the test.
- **Concurrency:** Cancel vs MoveToPendingReview; Approve vs Reject; Complete vs Fail (M2 vs callback); E0 vs closure in
  both orders with `LA025`; DenyForCompliance vs governed release. Exactly one wins, the loser is `ErrStateConflict`
  or a SQLSTATE refusal, hold nets to zero, no deadlock (lock-order harness unchanged).
- **Regression:** the full `internal/withdrawal`, `internal/payments`, `internal/launchgov`, `internal/reconciliation`
  integration suites; mutants: drop the `aa_` prefix (freeze fires first), add a `WHEN` clause, skip the visibility
  raise.
- **Reconciliation (D):** clean tenant -> clean; planted drift -> both kinds raised, standing across runs, non-active
  tenant observed.

## 8. Human/legal versus engineering

- **HD-RET-1 (OPEN, owner/legal; unchanged):** terminal rows become fully immutable, on top of `0131`'s no-delete. If
  counsel later requires pseudonymisation, it needs a governed, audited exemption added by ADR to this guard; none is
  built or invented now. The table holds no direct PII (ids, amounts, client `idempotency_key`, provider ids).
- **Engineering (decided here, reversible):** the edge set, SQLSTATEs, A + B + D, projection-based closure arm (subject
  to the `ledger-finance` ruling), fixture policy, migration split.
- **Not decided here:** `completed -> reversed` and ADR 0107 CT-PRE edges (need their own ADRs and writers);
  PAYOUT-AMOUNT-DISPUTE-1 / HD-CTF-4 (closure blocked indefinitely by a stranded hold - the existing ADR 0112
  §12.14(b3) posture extended to `LA025`; suspension remains the lever).

## 9. Residuals (stated honestly)

1. The table owner can `DISABLE TRIGGER` (ADR 0110 §9.4); D detects the result, nothing prevents it.
2. A runtime-credential holder can still drive a **legal** edge with a correctly shaped leg, e.g. E9 to `psp_clearing`
   (a forged payout completion). Bounded by `psp_clearing` provider-statement reconciliation, not by this ADR.
   Deferred engineering follow-up (not built): require E9/E10 to be accompanied by a terminal `payment_attempts` row or
   an executing M2/M4 resolution in the same txid (`payments` + `ledger-finance`).
3. E5 may be admitted without decision-row evidence if §3 item 5 finds no correlation column.
4. The four-eyes count and threshold remain Go-only (state doc §3).

## 10. Work slices (ordered; none starts without orchestrator authorisation)

| Slice | Content | Owner | Reviewers |
|---|---|---|---|
| 1 | M-A (guard, A, B, pre-flight), fixture classification and helper, §7 graph/column/evidence/coupling/B/concurrency tests | `ledger-finance` (SQL), `payments` (fixtures) | `security` (live runtime re-run incl. 14(b2)), `code-reviewer`, `qa`, `architect` (cross-domain) |
| 2 | M-B closure arm `LA025` + readiness Go arm + tests; `ledger-finance` ruling on projection use | `ledger-finance` + `launchgov` owner | `security`, `code-reviewer`, `qa` |
| 3 | D: reconciliation check, mismatch kinds, scheduler coverage, P1 alert kind | `ledger-finance` | `qa`, `code-reviewer` |
| 4 | Docs: `withdrawal-state-machine.md` §1/§3 (DB-enforced, `reversed` not legal yet), ADR 0112 §12.14 updated, registry, HANDOVER | `architect` | orchestrator |

Slice 1 alone closes the LAUNCH-BLOCKING part of 14(b2); slices 2-3 close 14(a) and invariant #12's label.
