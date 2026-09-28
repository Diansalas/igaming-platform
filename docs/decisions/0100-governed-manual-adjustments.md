# ADR 0100 — Governed manual adjustments and financial approval policies (PRH-2 K2)

- **Status:** PROPOSED (W0-K draft, `architect`, 2026-09-28). NOT IMPLEMENTED. K2 may not start
  until K1 (ADR 0099) is merged, and this ADR is reviewed by `security` and `ledger-finance` and
  recorded as accepted by the orchestrator (plan §2 W2, §11).
- **Decision type:** cross-domain architecture plus financial control (`internal/adjustment` new,
  `internal/ledger` called but **not edited**, `auth`, `httpserver`, migration 0112).
- **Owner:** `architect` (cross-domain shape). **`ledger-finance` owns every financial invariant
  here** (posting shape, sufficiency, keys, reason-code catalogue) and may tighten any of them without
  an architect amendment. **Reviewers:** `security`, `ledger-finance`, `qa`, `code-reviewer`.
- **Registry:** LEDGER-MANUAL-ADJ-4EYES-1. Workstream K2; migration 0112.
- **Binding inputs:**
  - ADR 0098 §2, §4 and §5: HD-PRH2-1, HD-PRH2-3, HD-PRH2-6 and HD-PRH2-7.
  - Plan §11.
  - `reviews/security.md` S-2, S-12 and S-13.
  - `reviews/ledger-finance.md` LF-9, LF-10, LF-11, LF-12, LF-13 and LF-14; "Required tests and
    mutants", K2.
  - `reviews/qa.md` F4 and the W2 K2 items.
  - ADR 0099 (grants, actor derivation, the acting family).
- **Related:** CLAUDE.md "Manual balance adjustments require a reason code and four-eyes approval
  above a configurable threshold"; migration 0063 (`bonus_approval_policies`, payload match);
  migration 0105 (`decided_txid`); ADR 0082 (lock order); ADR 0095 §28 (INV-DEP-1).
- **No threshold value is set anywhere in this ADR or in migration 0112** (HD-PRH2-3). No legal or
  regulatory claim is made.

---

## 1. Context

`TxManualAdjustment` and `AccountManualAdjustment` exist (`ledger.go:76,115`) and are used only by
tests. There is no API and no table. `ledger.Post` validates only that a `manual_adjustment` carries
a reason code (`lockorder.go:355-357`, mirroring 0051's CHECK); it constrains neither the accounts
nor the approval. `ledger.Post` has no generic sufficiency check. The sanctioned pattern is to read
balances from `LockProjectionsForPosting` (`lockorder.go:103-124`).

HR-9 is **removed** (`ledger.go:26-29`, `bonus_mirror.go`). A posting touching a BONUS_SET account
auto-generates mirror legs, and HR-17 forbids hand-built mirrors (LF-9). A "balanced
`manual_adjustment`" is therefore not safe by construction. The shape has to be closed.

## 2. The financial-control classification (HD-PRH2-1)

- A standing platform classification lists operation kinds and their control class:

  | `operation_kind` | Class |
  |---|---|
  | `ledger_adjustment` | `mandatory_four_eyes` |
  | `payment_force_resolve` (ADR 0101) | `mandatory_four_eyes` |

- It is **reference data written only by migrations** (0112 writes these two rows). The app role has
  `SELECT` only. A CHECK pins both kinds to `mandatory_four_eyes`: removing either from the class
  needs a human decision, an ADR and a migration that drops the CHECK. Policy data can never do it.
- **There is no `never` mode.** The policy model (§3) has no mode column. Every policy row carries
  `base_required_approvals`, which is `>= 1` for the mandatory class (see §3.4 for why the interim
  floor is 1).
- An operation kind that genuinely does not need independent approval is represented explicitly by
  a classification row of class `outside_mandatory_class`, added by migration and ADR. It is never a
  policy switch. No such row exists.
- No CLAUDE.md amendment is made or needed (HD-PRH2-1).

## 3. Approval policies (HD-PRH2-7, S-2, HD-PRH2-3)

### 3.1 Levels and who may author them

| Level | Scope key | Authored by | Approved by |
|---|---|---|---|
| `platform` | none | platform, `financial_policy:author` | a **different** platform principal with `financial_policy:author`, different Person |
| `jurisdiction` | `jurisdiction_id` | same as platform | same |
| `profile` | `profile_code` (plus the platform-authored tenant→profile assignment) | same as platform | same |
| `tenant` | `tenant_id` | tenant (`financial_policy:tighten`, **tightening only**) or platform | a different principal and Person: tenant (tightening only) or platform. **A non-tightening row needs a platform requester and a platform approver.** |
| `brand` | `tenant_id`, `brand_id` | same as tenant | same as tenant |

This satisfies S-2 (i) and HD-PRH2-7: only platform principals write or loosen; tenant and brand may
only tighten.

### 3.2 Evaluation: every less-specific row is a floor (S-2 ii/iv), enforced in the DB

- **In-force row per key.** For each `(level, scope key, operation_kind, asset_code)`, the in-force
  row is the one with the latest `effective_from <= now()`. Rows are append-only and effective-dated,
  so historical evaluation is reproducible: evaluate "as of" `t` by using `t` instead of `now()`.
- **Applicable rows.** Rows at every level that match the request: the operation; the tenant; the
  tenant's jurisdiction; the tenant's profile; the brand; and `asset_code` equal to the request's
  asset or NULL.
- **Row requirement.**
  - `base_required_approvals` if the row has no threshold, or if the amount is `<=` the threshold.
  - `required_approvals_above_threshold` if the amount is `>` the threshold.
  - An operation without an amount (M1, ADR 0101) uses `base_required_approvals` only.
- **Required approvals = MAX over all applicable rows.** A more specific row can never lower the
  requirement below a less specific one. That makes "tighten-only" structural, not procedural.
  S-2 (iv) is also enforced on insert: a tenant- or brand-session row that is not a tightening of the
  row it supersedes fails with a DB error (§3.3).
- **Enabled only with a platform baseline.** The operation is enabled only if an in-force
  **platform-level** row exists for the operation (asset NULL or equal to the request's). Otherwise
  submission is refused with `policy_absent`. **No in-force policy means disabled** (fail closed; no
  row is seeded).
- **Jurisdiction key.** The jurisdiction key is the jurisdiction of the licence the tenant operates
  under (`tenants.licence_id → licences.jurisdiction_id`, `migrations/0002:17,29`). If any
  jurisdiction-level row exists for the operation and the tenant's jurisdiction cannot be resolved,
  the request is refused (fail closed; ADR 0098 §4).
- **One implementation.** The evaluation is a single SQL function,
  `financial_policy_required_approvals(operation_kind, tenant_id, brand_id, asset_code, amount,
  as_of)`. It returns `(enabled, required, contributing_policy_ids[])` and is called by the Go
  executor and by the triggers. There is no second implementation in Go.

### 3.3 Policy changes are four-eyes, audited, and not usable by their author (S-2 iii)

- **Every change is a request plus an approval.** Tables: `financial_approval_policy_changes` and
  `…_change_approvals`. The request pins a content hash of the proposed row. The approval re-checks
  the hash and inserts the policy row **in the same transaction** (`decided_txid = txid_current()`).
  Requester ≠ approver, with distinct non-NULL Persons.
- **Tightening, defined** (versus the in-force row of the same key; with no predecessor, any row is
  a tightening under the MAX rule), all of the following:
  - `new.base >= old.base`;
  - if the old row has a threshold: the new row has one, with
    `new.threshold <= old.threshold` and `new.above >= old.above`.

  The trigger refuses a non-tightening tenant or brand row unless both the requester and the
  approver are platform principals.
- **An author may not use its own rule** (S-2 iii). If the initiator's Person is the
  `author_person_id` or `approver_person_id` of any policy row in the request's
  `contributing_policy_ids` (at submission or at execution), the initiation is refused. This is
  enforced by trigger. Whether to extend it to approvers is an open item (§14).
- `legal_review_reference` is optional free text. It is **informational only** and never evaluated.
- The tenant → profile assignment (`tenant_financial_policy_profiles`) is platform-authored and
  effective-dated, and goes through the same change and approval path.

### 3.4 HUMAN DECISION REQUIRED — below-threshold semantics for the mandatory class

**The question.** For an operation in the mandatory four-eyes class, may a policy let an amount at or
below a threshold execute with **no independent approver** (the initiator alone, with a grant, a
reason code and an audit record)?

**Why it is genuinely open.**
- CLAUDE.md reads "four-eyes approval above a configurable threshold".
- The orchestrator's 0098 §5 note treats the threshold as the point where four-eyes begins.
- HD-PRH2-1 says "four-eyes stays mandatory for operations the platform's standing financial-control
  policy classifies as requiring independent approval", with "configurable thresholds and policy
  profiles" allowed.

These two readings produce different systems.

| Option | Meaning | Consequence |
|---|---|---|
| **(a)** Threshold gates four-eyes | `base_required_approvals` may be 0 for the mandatory class; above the threshold, `>= 1` | Small corrections are single-person, reducing load. One human can move sub-threshold amounts alone, and repeated small adjustments can add up (a structuring risk, needing a cumulative-window rule, which would be a further decision). Combined with ADR 0099 §9, the one human must still hold a platform-co-approved grant. |
| **(b)** Always four-eyes; thresholds only add approvers | `base_required_approvals >= 1` always; a threshold raises the count above it (for example to 2 approvers) | Every adjustment needs two distinct Persons. The simplest model, with no structuring gap, and the one closest to HD-PRH2-1's "mandatory". It costs a second person on every correction. |

**Interim (fail closed, reversible), until answered:**
- 0112 enforces **(b)**: a trigger refuses `base_required_approvals < 1` for any mandatory-class
  operation.
- (b) satisfies CLAUDE.md a fortiori: four-eyes applies above any threshold because it applies at
  every amount.
- Moving to (a) later is a one-line trigger change plus an ADR amendment, with no data migration.
- K2 can proceed under the interim, because the interim is the stricter reading.

The orchestrator should assign a registry id (suggested: **HD-PRH2-8**) and put the question to the
human.

### 3.5 Per-asset thresholds (LF-12, HD-PRH2-3)

- A row with a threshold must carry an `asset_code` (CHECK). Minor units are per asset. A threshold
  never applies across assets.
- `threshold_minor_units` is `NUMERIC(38,0) >= 0`.
- No migration seeds a policy row. Fixtures use synthetic values, marked test-only.
- Real values are set later per tenant, jurisdiction and profile, under **LEGAL / COMPLIANCE REVIEW
  REQUIRED** (ADR 0098 §4).

### 3.6 Pinning

- At submission, the request stores `required_at_submission` and `contributing_policy_ids`.
- At execution, `required = MAX(required_at_submission, evaluation at execution)`:
  - a policy tightened in between requires the extra approval (LF K2 test);
  - a policy loosened in between never lowers the requirement below what was pinned.

## 4. The posting-shape catalogue (LF-9, S-13)

**PRH-2 allows exactly one shape.** It is enforced in the executor **and** by DB checks (§5 and the
§9 link trigger):

| Direction | Debit | Credit |
|---|---|---|
| `credit_player` | the tenant's `manual_adjustment` account (house-level, same asset) | the target player's `player_cash` account (the wallet's, same asset) |
| `debit_player` | the target player's `player_cash` | the tenant's `manual_adjustment` |

- Exactly two entries, one amount, one asset, one tenant (the request's).
- `transaction_type = 'manual_adjustment'`.
- The request's `account_type` column has CHECK `= 'player_cash'`.
- **Refused (not representable in the request):**
  - `player_withdrawal_hold`, `player_locked_*`, `player_bonus*` (all BONUS_SET, so no mirror
    legs are ever generated);
  - `psp_*`, `provider_payable`, `promo_liability`, `bonus_expense`, `jackpot_contribution`,
    `house_gaming`.
- Bonus corrections go through the bonus engine.
- Cross-asset value movement is never an adjustment. It is a `ConversionOperation` (CLAUDE.md).
- Extending the catalogue needs a `ledger-finance` ruling plus an amendment to this section.

## 5. The request: payload, immutability, checks and keys

### 5.1 Payload (LF-10; immutable after submission)

The payload is:
- `tenant_id`, `wallet_id`, `player_account_id` (derived by trigger from the wallet, never accepted
  from the client), `brand_id` (derived);
- `account_type` (= `player_cash`), `asset_code` (must equal the wallet's asset), `direction`,
  `amount`;
- `reason_code`, `causation_transaction_id NULL`, `note_hash` (the free-text note lives in the audit
  metadata; only its hash is pinned here).

Rules:
- A BEFORE UPDATE trigger refuses any change to a payload column (the 0063 immutability precedent).
- `payload_hash` is computed **by a DB function** over a canonical encoding of the payload (the
  0096 content-hash precedent). The Go code never supplies it.

### 5.2 Checks at submission (LF-12)

- `amount` is `NUMERIC(38,0)`, with CHECK `amount > 0 AND amount <= 9223372036854775807`. An approved
  request can therefore always become an `EntryInput.Amount` (int64, `ledger.go:259`).
- `asset_code` equals the wallet's asset and exists in the asset registry. Whether a suspended asset
  may be corrected is an open item (§14).
- `causation_transaction_id`, if present, is a same-tenant `ledger_transactions` row (composite FK
  on `ledger_transactions_id_tenant_key`, `0021:51`), and its type is not `deposit`. The request has
  **no column** that can reference a payment attempt or deposit intent (LF-2).
- The reason code is in the closed catalogue (§5.4), and the direction is allowed for that code.
- The policy is enabled (§3.2).
- The initiator holds an in-force `ledger_adjustment:initiate` grant for the tenant, and passes
  S-2 (iii) (§3.3) and S-12 (§6.3).

### 5.3 Keys (LF-14)

| Key | Value |
|---|---|
| `idempotency_key` | `'manual_adjustment:' || request_id`, so UNIQUE `(tenant_id, idempotency_key)` (`0021:43`) makes a second execution a DB-level no-op or conflict, never a second posting |
| `correlation_id` | the request id |
| `causation_id` | the compensated transaction, if any |
| `reason_code` | copied from the request to `ledger_transactions.reason_code` (`0051:72`) |
| `ProviderID` / `ProviderTxID` | nil (not a provider posting) |

### 5.4 The closed reason-code catalogue

- The catalogue is reference table `ledger_adjustment_reason_codes`: `code` PK,
  `allowed_directions`, `requires_causation`. It is written only by migration.
- **It has no deposit-allocation code** (LF-2).

Proposed content, **RECOMMENDATION** for `ledger-finance` to confirm or replace before 0112 is
written:

| Code | Directions | Causation |
|---|---|---|
| `operational_error_correction` | both | optional |
| `compensating_entry` | both | **required** |
| `goodwill_credit` | credit only | none |
| `external_instruction` | both | optional; the instruction reference goes in the audit metadata |

## 6. Approval and execution

### 6.1 States

`pending → executed | refused_insufficient_funds | rejected | cancelled | expired`, plus the
in-transaction-only `executing` (§6.5).

- **Terminal rows are immutable.** One `reject` approval ends the request.
- **Cancel** is by the initiator only.
- **TTL:** bounded and forced by trigger; a technical default, not a money value.

### 6.2 Approval counting (LF-14, S-4)

An approval row counts toward `required` at execution **only if all** of these hold:
- `decision = 'approve'`, and its `payload_hash` equals the request's recomputed hash (a client
  cannot approve content it did not see);
- the approver holds an in-force, unrevoked `ledger_adjustment:approve` grant for the tenant **at
  execution time** (a timestamp check then, not a sweeper);
- the approver's staff row is `active` with an eligible role at execution (ADR 0099 §7.4);
- the approver's `person_id` is non-NULL and distinct from the initiator's and from every other
  counted approver's (§6.3);
- the approver is not the beneficiary Person (§6.3).

The initiator's grant, status and role are re-checked the same way. If they fail, the request cannot
execute.

### 6.3 The independence floors (non-configurable)

- **Distinct-Person floor (LF-11).** Every counted approval needs a non-NULL `person_id`, distinct
  from the initiator's and from each other's. There is **no `distinct_principal` option**. Two
  principals of one Person are one actor. The floor is enforced by trigger on approval insert and
  again at execution, and **no policy column can relax it**. ADR 0099 §9 applies: under unverified
  identity this is defence in depth, and the structural control is the platform co-approved grant.
- **Beneficiary exclusion (S-12, the 0029 precedent).** Neither the initiator nor any approver may
  resolve to the Person who owns `player_account_id`.
  - It is enforced by the BEFORE INSERT triggers `ledger_adjustment_requests_beneficiary_guard` and
    `ledger_adjustment_approvals_beneficiary_guard`, and re-checked at execution.
  - Player Persons are NOT NULL (`0010:15`), so the comparison is total.
  - Unlike 0029, **an unlinked staff Person is refused, not skipped** (§6.3, first bullet).

### 6.4 No negative balance (LF-13 ruling)

- A `debit_player` adjustment may not drive the player's `player_cash` negative.
- In the execution transaction, the executor builds the exact `TransactionInput` it will post and
  calls `ledger.LockProjectionsForPosting(ctx, tx, in)` with **the same input** (R3; never a subset).
  It then reads the player-cash balance from the result.
- If the balance is `< amount`, the request goes to `refused_insufficient_funds` with **nothing
  posted**, and the transaction commits (the approval and the refusal are both recorded). The request
  is terminal; a new request is needed.
- Credits need no sufficiency check. The house-level `manual_adjustment` account is a contra
  account and may carry any sign.

### 6.5 Execution in the final approval's transaction (LF-13, LF-10)

There is **no approved-but-unexecuted window**. The transaction that inserts the approval that makes
`counted >= required` also executes, in one transaction:

1. Lock the request row `FOR UPDATE` (class L1; §7). Refuse unless it is `pending` and unexpired.
2. Insert the approval (triggers: actor from the session, the §6.2/§6.3 checks, the payload hash).
3. Evaluate `required` (§3.6) and `counted` (§6.2) with one SQL function. If `counted < required`,
   commit here: the request stays `pending`.
4. Set `state = 'executing'` and `executed_txid = txid_current()`.
5. Call `LockProjectionsForPosting(in)` (L3) and run the §6.4 check. If funds are insufficient, set
   `refused_insufficient_funds`, audit, and commit.
6. `ledger.Post(in)` (L4).
7. Set `state = 'executed'` and `ledger_transaction_id`.
8. Audit, then commit.

Rules:
- **"Same transaction" is `executed_txid` / `decided_txid = txid_current()`, never `xmin`.**
  `ledger.Post` inserts under a savepoint, so `xmin` would be a subtransaction xid (LF-10).
- **The link trigger (LF-10).** On `→ executed`, `ledger_transaction_id` must reference a
  same-tenant row (composite FK `(ledger_transaction_id, tenant_id)` →
  `ledger_transactions_id_tenant_key`) with:
  - `transaction_type = 'manual_adjustment'`, the §5.3 idempotency key, correlation and reason code;
  - **exactly the two §4 entries**, equal to the payload (accounts, directions, amount, asset).

  The column is UNIQUE, so one ledger transaction backs at most one request.
- **A DEFERRABLE INITIALLY DEFERRED constraint trigger** refuses to commit any request in
  `executing`. A crash between steps 4 and 7 can never leave a half-executed request.
- **Concurrency:** two concurrent final approvals serialize on the step-1 lock. The second sees
  `executed` and stops. The §5.3 idempotency key is the DB backstop.
- If the requirement is already met at submission (possible only under §3.4 option (a)), the
  submission transaction itself runs steps 4–8.

### 6.6 Platform principals (HD-PRH2-6)

- A platform principal may initiate or approve **only** through an ADR 0099 §6 acting session for
  that tenant, holding the specific grant.
- The plain platform session has **no** policy on any 0112 request or approval table, and no ledger
  access. It is refused.
- The acting-session ledger post passes the ADR 0099 §6.5 fence because step 4 sets `executed_txid`
  before step 6.

## 7. ADR 0082 amendment note (LF-13) — proposed Amendment A8

Text for the orchestrator to append to ADR 0082 (plan Rule 2: the orchestrator writes amendment
sections; this ADR does not edit 0082):

> **Amendment A8 — PRH-2 K2/K3 (ADRs 0100, 0101).** Class L1 gains, appended after
> `payment_attempts`:
> - `ledger_adjustment_requests`;
> - `payment_manual_resolutions`.
>
> Rules:
> - A `payment_manual_resolutions` row is locked only after its attempt, which is locked after the
>   attempt's parent (`withdrawal_requests` or `deposit_intents`), per A7's parent-before-attempt
>   rule.
> - A `ledger_adjustment_requests` row is the only L1 row a manual-adjustment execution takes, and it
>   is taken before `LockProjectionsForPosting` (L3) and `Post` (L4).
> - The approval-row inserts take no row lock beyond their unique-index waits, which are class L4.
>
> There is no new class and no new exception.

## 8. Audit

Every submission, approval, rejection, cancellation, expiry, refusal, execution, policy change and
profile assignment writes `audit_log` in the same transaction. Each record carries:
- actor, actor scope, and acting tenant where relevant;
- request id, wallet and player (as ids only), asset, direction, amount and reason code;
- before and after state;
- `payload_hash` and contributing policy ids;
- the ledger transaction id on execution;
- IP, user agent and request id.

Other rules:
- Acting-session rows follow ADR 0099 §11.
- Per HD-PRH2-5, the tenant presentation shows the identifiable actor and the approval chain.
- The free-text note is in the audit metadata. No PII beyond ids.
- The request and approval tables are themselves an append-only record, so the audit log is not
  the only evidence.

## 9. Migration 0112 (K2) — tables and RLS (QA F4)

Every table: **`ENABLE` and `FORCE ROW LEVEL SECURITY`**; no `FOR ALL` policy; `BEFORE TRUNCATE`
deny; DELETE refused; no `SECURITY DEFINER`; SQLSTATE class **`MA`**.

Family definitions:
- **tenant:** `tenant_id = app.tenant_id`, principal set, player, platform and acting GUCs unset
  (the 0106 lesson).
- **platform:** the validated platform GUC; `app.tenant_id` and the acting GUCs unset.
- **acting:** ADR 0099 §6.3.
- **reference:** readable by any non-player session; no write policy; no app-role write grant.

| Table | Content | RLS families |
|---|---|---|
| `financial_control_classifications` | §2 | reference |
| `ledger_adjustment_reason_codes` | §5.4 | reference |
| `financial_approval_policies` | §3. Append-only. CHECKs: level ↔ scope columns; a threshold ⇒ an `asset_code` (LF-12); `above >= base`; `base >= 1` for the mandatory class (§3.4 interim, by trigger). Rows are inserted only by the change-approval trigger. | **SELECT:** rows with `tenant_id IS NULL` (platform, jurisdiction and profile levels) are readable by tenant, platform and acting sessions; tenant and brand rows are readable by tenant (own), acting (X) and platform. **INSERT:** platform (any level); tenant (own tenant and brand rows, tighten-only by trigger). |
| `tenant_financial_policy_profiles` | tenant → profile, effective-dated, append-only | SELECT: tenant (own), acting (X), platform. INSERT: platform only |
| `financial_approval_policy_changes`, `financial_approval_policy_change_approvals` | §3.3. Content hash, `decided_txid`, distinct Persons | tenant (own tenant and brand changes), platform (all). **No acting.** |
| `ledger_adjustment_requests` | §5–§6. Immutable payload, `payload_hash`, pinned policy, `idempotency_key` UNIQUE `(tenant_id, idempotency_key)`, `executed_txid`, `ledger_transaction_id` UNIQUE with the composite FK. Beneficiary guard; deferred `executing` check; link trigger. | tenant; acting. **No platform family** (HD-PRH2-6). |
| `ledger_adjustment_approvals` | `payload_hash`, `decided_txid`, derived actor, scope and Person. Immutable. Distinct-Person and beneficiary guards. | tenant; acting. **No platform family.** |

**Acting-family policies on existing tables**, added by 0112 per ADR 0099 §6.4:
- `staff_users` (SELECT, tenant X);
- `player_accounts` (SELECT, X);
- `wallets` (SELECT);
- `ledger_accounts` (SELECT, INSERT);
- `ledger_transactions` and `ledger_entries` (SELECT, INSERT);
- `wallet_balance_projection` (SELECT, INSERT, UPDATE);
- `audit_log` (INSERT, `tenant_id = X`).

The ADR 0099 §6.5 fence trigger on `ledger_transactions` and `ledger_entries` is added with its
`manual_adjustment` branch. **K2 enumerates from the code every table and trigger that `ledger.Post`
and `LockProjectionsForPosting` touch before writing 0112.** A missing table fails closed and is
caught by test B-3.

**Down:** refuse while any row exists in any 0112 table. Otherwise drop the policies, triggers and
tables in reverse order. The existing ledger policies are left byte-identical.

## 10. Tests and mutants (K2 DoD; LF K2 list and QA W2, incorporated by reference)

Notes:
- T-1 clock for expiry and validity.
- No new wall-clock assertions.
- Results are reported PASS / FAIL / FLAKE / NOT RUN / BLOCKED. Local runs are never labelled CI.

| ID | Class | Test |
|---|---|---|
| B-1 | R | Credit and debit, each executed in the final approval's tx; `SUM(D) = SUM(C)`; the projection equals the recomputed value |
| B-2 | PROP | Random sequences of adjustments (with some refused): `SUM(D) = SUM(C)` and projection = recomputed hold throughout (QA W2) |
| B-3 | RLS | The whole flow under an acting session with grants, and under a tenant session. The plain platform session is refused. An acting session for Y on X's request is refused. |
| B-4 | ADV | The payload is changed after submission (refused by trigger); an approval carries a stale or foreign `payload_hash` (refused) |
| B-5 | AZ | Self-approval; the same Person under two principals; an unlinked Person (initiator or approver); **an initiator or approver who is the beneficiary Person** (S-12). All refused. |
| B-6 | AZ | S-4: a suspended approver with a live token is refused; an approver demoted after approving is not counted; an approver whose grant is revoked or expires between approval and execution is not counted |
| B-7 | CON | Revoke racing execution; two concurrent final approvals give exactly one ledger transaction; execute twice gives one ledger transaction (the idempotency key) |
| B-8 | R | A policy tightened between submission and execution requires the extra approval; a policy loosened in between does not lower the pinned requirement |
| B-9 | R | No in-force platform policy: disabled. A tenant row without a platform baseline: still disabled. An unresolvable jurisdiction when a jurisdiction row exists: refused. |
| B-10 | ADV | A tenant-session loosening row (base lower, threshold higher, or above lower) is refused by trigger. A platform loosening with one principal or one Person is refused. The policy author initiating under its own row is refused (S-2 iii). |
| B-11 | R | A debit beyond balance ends `refused_insufficient_funds` with nothing posted and the tx committed. A debit equal to the balance posts. |
| B-12 | ADV | A shape other than `player_cash`/`manual_adjustment` (BONUS_SET, hold, locked, PSP, provider, promo): not representable. A forged `ledger_transaction_id` link (wrong tenant, wrong type, entries ≠ payload, reused): refused. |
| B-13 | R | A reason code outside the catalogue; a direction not allowed for the code; `compensating_entry` without causation; causation pointing at a `deposit` or another tenant's transaction. All refused. |
| B-14 | R | `above_threshold` without an asset is refused by CHECK. An amount of `2^63` is refused at submission. |
| B-15 | FL/RB | A crash or error between steps 4 and 7: nothing committed. The deferred trigger refuses a committed `executing`. |
| B-16 | ADV | Under an acting session: a `manual_adjustment` posting with no executing request is refused by the fence; a `casino_bet` posting is refused |
| B-17 | AU | One audit row per transition. The tenant projection shows the identifiable actor and never IP or user agent. |
| B-18 | R | Reconciliation: the hourly drift check shows zero after mixed adjustments. `manual_adjustment` postings appear in reconciliation classified correctly. |
| B-19 | MIG | 0112 up/down/up; down refuses while rows exist; the existing ledger policies are unchanged after down |
| B-20 | R | Both classification rows are pinned `mandatory_four_eyes` by CHECK; `base_required_approvals = 0` for the mandatory class is refused (§3.4 interim) |

**Mutants (each must be killed):**

| Mutant | Killed by |
|---|---|
| drop the payload-hash comparison | B-4 |
| drop the distinct-Person check | B-5 |
| drop the in-tx grant read at execution | B-6 |
| drop the executed ↔ transaction match | B-12 |
| drop the sufficiency check | B-11 |
| drop the beneficiary guard | B-5 |
| use `xmin` instead of `executed_txid` | B-7/B-16 with a savepoint fixture |
| evaluate policy as "most specific wins" instead of MAX | B-10 |
| drop the platform-baseline requirement | B-9 |
| drop the deferred `executing` check | B-15 |

## 11. Invariants preserved

- **INV-DEP-1:** untouched. There is no reference to attempts or intents, no deposit-allocation
  reason code, causation to a `deposit` transaction is refused, and ADR 0101 §3 refuses any link to a
  deposit resolution. A goodwill credit could still be used to pay out a captured-but-unposted
  deposit by hand. That residual is a **detective** control, not a preventive one:
  `pay_captured_unposted` keeps reporting (ADR 0101 §4), and the independent approver sees the
  request. `ledger-finance` rules on whether that is acceptable (§14).
- **Append-only double-entry:** the only write is `ledger.Post`. Corrections are new compensating
  entries (`compensating_entry` plus causation). No edit or delete. `SUM(D) = SUM(C)` (B-1/B-2).
- **DB idempotency:** UNIQUE `(tenant_id, idempotency_key)` on `ledger_transactions`; UNIQUE
  `ledger_transaction_id` on requests; the request row lock.
- **RLS:** FORCE RLS everywhere. There is no platform family on tenant money rows. The acting family
  is grant-gated (ADR 0099).
- **No direct balance mutation:** balances change only through the ledger trigger. The sufficiency
  read happens under the L3 lock in the same transaction.
- **HD-LEDGER-UNALLOC-1 "A now, B later":** unchanged. There is no suspense or allocation posting;
  LEDGER-SUSPENSE-B-1 stays deferred.

Introduced:
- **INV-ADJ-1:** an adjustment posts only the §4 shape, exactly the approved payload, in the final
  approval's transaction.
- **INV-ADJ-2:** required approvals are the MAX over all applicable in-force rows, never below the
  pinned value, and always `>= 1` for the mandatory class (interim, §3.4).
- **INV-ADJ-3:** a counted approval has an in-force grant at execution, a distinct non-NULL Person,
  and is not the beneficiary.
- **INV-ADJ-4:** a debit adjustment never makes `player_cash` negative.

## 12. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| A `never` mode, or a policy switch that turns four-eyes off | HD-PRH2-1 |
| "Most specific row wins" | A tenant row could loosen a platform floor (S-2) |
| Tenant authoring with tenant-local four-eyes (HD-PRH2-7 (b)) | Not chosen, and exposed to sock puppets |
| An open posting shape ("any balanced `manual_adjustment`") | The BONUS_SET mirror generator and HR-17; refused by LF-9/S-13 |
| Approve now, execute later by a sweeper or a second call | An approved-but-unexecuted window; LF-13 |
| Linking execution by `xmin` | A subtransaction xid under `ledger.Post`'s savepoint (LF-10) |
| A generic sufficiency check inside `ledger.Post` | A `ledger` edit outside K2's Touches; the sanctioned pattern is `LockProjectionsForPosting` |
| A `distinct_principal` independence option | LF-11 |
| A DB-wide rule that every `manual_adjustment` transaction must link a request, for all sessions | Correct in spirit, but 24 test files post `TxManualAdjustment` as a fixture today. Recorded as an open item. For now, a static test pins that `internal/adjustment` is the only non-test caller. |

## 13. Relation to CLAUDE.md

- "Manual balance adjustments require a reason code and four-eyes approval above a configurable
  threshold": satisfied. There is a reason code from a closed catalogue. Four-eyes applies at every
  amount under the interim (§3.4), and the threshold is configuration.
- "Every mutating administrative/financial action writes an audit record": §8.
- "Four-eyes approval above a configurable threshold" with no invented value: HD-PRH2-3 and §3.5.

## 14. Open items

1. **HUMAN DECISION REQUIRED: HD-PRH2-8 (proposed id), below-threshold semantics** (§3.4). The
   interim is (b).
2. **The reason-code catalogue content** (§5.4) is a RECOMMENDATION. `ledger-finance` confirms it.
3. **The goodwill-credit residual against unposted captures** (§11, INV-DEP-1 bullet).
   `ledger-finance` rules whether a preventive control is needed, for example refusing a credit
   while the player has an open `pay_captured_unposted` row.
4. **Suspended-asset corrections** (§5.2). `ledger-finance` rules.
5. **Extending S-2 (iii) to approvers** (§3.3). `security` rules.
6. **A DB-wide `manual_adjustment` ⇔ request rule** (§12, last row). `ledger-finance` and `qa`
   decide whether to migrate the test fixtures and add it.
7. **Player-jurisdiction versus licence-jurisdiction** as the policy key (§3.2). This carries
   **LEGAL / COMPLIANCE REVIEW REQUIRED**. The licence jurisdiction is used until someone rules
   otherwise.
8. **Non-active tenants:** whether adjustments are allowed for a suspended tenant (compare the H
   ruling that in-flight money must resolve). The default is no restriction beyond RLS, and
   `security` rules.
9. **ADR 0082 Amendment A8** (§7) is written into ADR 0082 by the orchestrator.

## 15. Handover / DoD

K2 is not done until it updates:
- `docs/architecture/ledger-accounting-model.md` (the §4 posting shape and the reason-code
  catalogue);
- `docs/architecture/06-wallet-ledger-architecture.md` and `financial-transaction-flows.md` (a new
  flow, "manual adjustment");
- `docs/architecture/reconciliation-model.md` (how `manual_adjustment` postings are classified);
- `docs/security/security-architecture.md` (S-2 / S-12 / S-13 controls);
- `backoffice/src/auth/permissions.ts`;
- `docs/runbooks/operational-runbooks.md` (new entry "Manual adjustment": submitting, approving,
  insufficient-funds refusal, the policy-change procedure, and the fact that no threshold exists
  until the platform authors one);
- `deploy/init-app-role.sql` (append-only grants);
- the production configuration checklist, if any env var is added (none expected).

The orchestrator updates:
- ADR 0082 Amendment A8 (§7);
- registry LEDGER-MANUAL-ADJ-4EYES-1, and the new HD id from §3.4;
- the HANDOVER decisions index;
- the review records under `docs/plans/prh2-hardening-round/`.
