# ADR 0088 — Sportsbook Settlement Implementation Contract (Stage 10 W1)

- **Status:** PROPOSED — pending `architect` / `sportsbook` / `security` /
  `risk` / `qa` review; **no code before acceptance** (ADR 0087; proposal
  §8 H W1 item 1).
- **Authors:** `ledger-finance` (drafting owner of §2, §4, §5, §8, §11,
  §12); `sportsbook` co-author for §3 and §9 on review.
- **Decision type:** implementation contract. It narrows already-accepted
  architecture (ADR 0038 §5/§8.1/§10–§14.6, ADR 0082, ADR 0083) to exact,
  mechanical rules for the approved Stage 10 W1 scope. It creates no new
  product scope and reopens no human decision (ADR 0042, ADR 0087).
- **Inputs:** `docs/plans/stage-10-planning-gate-proposal.md` §8 (W1 items
  0–10, §I, §K–§T), §9 (rulings R-1, R-2); ADR 0087.

> Numbering: "ADR 0088" is this decision record. Migration
> `0088_sportsbook_exposure_limits` is unrelated. The W1 migration proposed
> here takes the next free migration number, **0091** at the time of
> writing (`migrations/` ends at `0090_*`).

Labels (`CLAUDE.md`): everything here is **NOT IMPLEMENTED** until W1
lands; once built the settlement driver is a **MOCK**, never a real
provider integration.

## 0. Context (verified against HEAD `05e1990`)

File:line references throughout cite HEAD at drafting (`05e1990`) and
must be re-verified at implementation; a drifted line number never
overrides the named symbol it points at.

- `PlaceBet` (`internal/sportsbook/orchestrator.go`) posts `sportsbook_bet`
  (Dr `player_cash` S · Cr `player_locked_cash` S) in in-house mode:
  provider fields NULL, key `sportsbook_bet:<player_account_id>:<client
  key>` (:379), `correlation_id` = server-minted bet id (:256), bet row
  inserted **after** `ledger.Post` (:428 → :433; ADR 0082 E-3).
- `sportsbook_bets.status` (migration 0078) is deliberately mutable; only
  `open` is written; stake/odds/`potential_return`/references/
  `jurisdiction_code` are frozen (migrations 0082, 0087). Exposure counts
  `status = 'open'` only (`exposure.go`:190–199).
- `ReversalTypes = nil` for `OperationSportsbookBet`
  (`internal/risk/cumulative.go:237`); `ledger.Post` replay compares only
  `transaction_type` (`ledger.go:316–326`, **F-7**); the type CHECK admits
  17 values and no settlement type. A placed stake has no release path.

## 1. Scope and non-goals

### 1.1 In scope (exactly proposal §8 H W1)

Cash-funded **singles**, **in-house mock mode only** (ADR 0038 §14.6):
settle won, settle lost, void before settlement, void after settlement,
rollback (never-seen and seen), re-settlement, rollback-then-void; with
idempotency, payload integrity, audit, authorization, RLS, concurrency
safety, reconciliation and deterministic tests.

### 1.2 Non-goals (binding; `product-owner-proxy` scope guard)

Real provider settlement, **webhook**, provider-mode idempotency and
provider references (§I; R-1); cashout, partial settlement, accumulators,
live odds (§I); bonus/mixed-funded settlement (ADR 0038 §9; HR-9);
operator manual settlement; the self-exclusion auto-void consumer (R-2;
ADR 0042 default not re-asked); liability reporting, arming limits,
rung 2; `NUMERIC(38,0)` widening (debt, §3.6); OB-1 resolution. The
test-support route (§9) is **test tooling**, never described as operator
settlement or a provider integration.

## 2. Ledger postings

### 2.1 New transaction types

Migration 0091 widens `ledger_transactions_transaction_type_check` from 17
to 20 values, adding exactly `sportsbook_settlement`, `sportsbook_void`,
`sportsbook_rollback` (same `DO $$ … EXCEPTION WHEN check_violation`
pattern as migration 0078). Go constants in `internal/ledger/ledger.go`:
`TxSportsbookSettlement`, `TxSportsbookVoid`, `TxSportsbookRollback`.
No new account type (ADR 0038 §1; proposal §L "no change to
`player_locked_*`").

Every W1 posting: `ProviderID = nil`, `ProviderTxID = nil`,
`ReasonCode = nil` (the migration-0021 CHECK forbids a reason code outside
`manual_adjustment`; the void reason lives in the history row, §3.2),
`CorrelationID = bet.id`, `BonusCost = nil`.

`CausationID`, exhaustively:

| Posting | `CausationID` |
|---|---|
| Re-settlement (generation g > 1) | ledger transaction id of the rollback **or** tombstone of generation g − 1 |
| The `sportsbook_void` of a void-after-settlement | ledger transaction id of the rollback posted just before it in the same DB transaction |
| Every other W1 posting (first settlement, standalone rollback, void-before, rollback-then-void's void, tombstone) | `nil` |

The void's `CausationID` is not an entry field, so setting it after the
rollback's `Post` returns does not change the pre-locked entry set
(§5.2). The history row's `causation_record_id` (§3.2) mirrors these
rules at row level.

### 2.2 Accounts and stake — derived from the bet's own ledger postings

Never from the request, never re-resolved from wallet/asset:

1. Load the placement transaction `B = ledger_transactions[bet.ledger_transaction_id]`.
   Require `B.transaction_type = 'sportsbook_bet'`, `B.correlation_id =
   bet.id`, exactly two entries: Dr on an account of type `player_cash`,
   Cr on an account of type `player_locked_cash`, equal amounts, asset =
   `bet.asset_code`. Otherwise → integrity failure (§4.7).
2. `CASH` = the debited account id; `LOCKED` = the credited account id;
   `S_placed` = the entry amount. Require `S_placed = bet.stake_amount`.
3. `HOUSE` = `ledger.GetOrCreateAccounts(tenant, {WalletID: nil,
   AccountType: house_gaming, AssetCode: bet.asset_code})` (before L3; ADR
   0082 §2.1 "ledger_accounts creation happens before L3").
4. `NetLocked(bet)` = Σ over entries of transactions with `correlation_id
   = bet.id` and `transaction_type IN ('sportsbook_bet',
   'sportsbook_settlement','sportsbook_void','sportsbook_rollback')` on
   `LOCKED` of (credit − debit). Also `NetLockedBonus(bet)` = same over any
   `player_locked_bonus` account; must be 0.
5. Pre-posting assertion, evaluated under the L1 bet lock: bet `open` ⇒
   `NetLocked = S_placed`; bet `settled_*`/`void` ⇒ `NetLocked = 0`. A
   violation is an integrity failure; nothing posts.

`S` below always denotes `S_placed` after these checks pass. Unlocked
reads are sound here because every writer of transactions with
`correlation_id = bet.id` after placement holds the bet's L1 row lock
(INV-SB-SETTLE-2).

### 2.3 Exact entries

`P` = payout for a won single = `bet.potential_return` (§2.4). All amounts
are strictly positive `int64` minor units; every row balances per asset.

| Event | `transaction_type` | `reverses_transaction_id` | Entries (in this insertion order, ADR 0082 R7) |
|---|---|---|---|
| Settle **lost** | `sportsbook_settlement` | NULL | Dr `LOCKED` S · Cr `HOUSE` S |
| Settle **won** | `sportsbook_settlement` | NULL | (1) Dr `LOCKED` S · Cr `HOUSE` S; (2) Dr `HOUSE` P · Cr `CASH` P — two balanced pairs, full payout not winnings (Flow 9 trap) |
| **Void before settlement** | `sportsbook_void` | NULL | Dr `LOCKED` S · Cr `CASH` S |
| **Rollback** of settlement `X` | `sportsbook_rollback` | `X.ledger_transaction_id` | Exact inverse of `X`'s stored entries, loaded from `ledger_entries` (same account ids, direction flipped, same amounts, same order) — the `casino.postRollback` inversion pattern (`internal/casino/orchestrator.go` ~:1260) |
| **Void after settlement** | two postings, **one DB transaction** | — | (a) `sportsbook_rollback` of the current settlement exactly as above, then (b) a before-settlement-shape `sportsbook_void` Dr `LOCKED` S · Cr `CASH` S |
| **Re-settlement** after rollback/tombstone | `sportsbook_settlement` | NULL | As settle won/lost; new generation (§4.2) |
| **Rollback-then-void** (standalone rollback earlier, void later) | `sportsbook_void` | NULL | Before-settlement shape (the bet is `open` again) |
| **Rollback of never-seen settlement** | `tombstone` (existing type) | NULL | **No entries** (ledger-accounting-model §1.4) |

Void-after-settlement is two postings (amends ADR 0038 §8.1/Flow 10,
§13): `reverses_transaction_id` is one column (migration 0021), and a
settlement's exact inverse returns the stake to `player_locked_cash`, not
`player_cash` — Flow 10's current text is wrong without the trailing void.

End states (net credit − debit over the bet's `correlation_id`, incl.
placement; the per-bet reconciliation targets, §8.2):

| Final state | `CASH` net | `LOCKED` net | `HOUSE` net |
|---|---|---|---|
| open (incl. after rollback) | −S | +S | 0 |
| settled_lost | −S | 0 | +S |
| settled_won | P − S | 0 | S − P |
| void (either path) | 0 | 0 | 0 |

### 2.4 Payout: derived, never an input to posting

**Decision:** the posted payout is **always derived** from stored state:
won ⇒ `P = bet.potential_return` (frozen at placement by
`computePotentialReturn`, ADR 0021 rounding, immutable by trigger); lost ⇒
no payout pair. The route (§9) carries `payout_amount` and `asset_code`
only as a **claim** — the simulated provider's statement — which is
validated against the derived values and **never** flows into a
`TransactionInput`.

Why: in-house mode has no provider to state `S+W` (ADR 0038 §5's
posture has no source); no posted money in the request is the strongest
form of ADR 0048 mitigation 3; the claim keeps the provider-shaped
validation path and makes §R's payout-mismatch tests drivable through the
real route.

Validation (all failures: rejected, integrity alert, audit, nothing
posted — §4.4):

| Rule | Check |
|---|---|
| V-1 asset | claim `asset_code` = `bet.asset_code` |
| V-2 lost | claim `payout_amount` = 0 |
| V-3 won, positive | `bet.potential_return` > 0 (the column CHECK allows 0; a zero-payout win cannot post a `amount > 0` entry — reject) |
| V-4 won, anti-minting | claim `payout_amount` = `bet.potential_return` (stored-value comparison; no platform odds math) |
| V-5 range | claim parsed as a JSON integer into `int64`; non-integer, negative, or > `math.MaxInt64` rejected at decode (explicit overflow test) |

V-3/V-4 are also enforced by the history-table trigger (§3.3), so a code
defect cannot mint.

### 2.5 OB-1 (remains OPEN)

Rolling back a won settlement debits `CASH` by P. If the player has spent
or withdrawn it, `player_cash` goes negative. That posting is correct and
**must post** (ledger-accounting-model §6.4.5 case K property 4,
~l.2640–2658): no balance check, no RG/eligibility gate on rollback or
void (casino `postRollback` precedent). The receivable's business
treatment is **OB-1, OPEN**, not resolved here; ADR 0087 records it. A
test asserts the negative `player_cash` posting succeeds and that
`LOCKED` never goes negative.

## 3. Bet state machine and schema

### 3.1 Transition graph (DB-enforced)

```
open --settle(g,won)--> settled_won --rollback(g)--> open   (exposure counts it again)
open --settle(g,lost)-> settled_lost --rollback(g)-> open
open --void--> void                                          (terminal)
settled_* --[rollback(g) + void, one DB txn]--> open --> void (two UPDATEs)
```

Allowed `sportsbook_bets.status` updates: `open→settled_won`,
`open→settled_lost`, `open→void`, `settled_won→open`, `settled_lost→open`.
Everything else is rejected, including any change from `void`
(terminal) and a direct `settled_*→void` (void-after-settlement performs
two UPDATEs). Settlement after void is rejected; a second settlement while
settled is rejected (rollback required).

### 3.2 `sportsbook_bet_settlements` (append-only history; migration 0091)

| Column | Type | Rule |
|---|---|---|
| `id` | `UUID PRIMARY KEY DEFAULT gen_random_uuid()` | |
| `tenant_id` | `UUID NOT NULL` | |
| `bet_id` | `UUID NOT NULL` | `FOREIGN KEY (bet_id, tenant_id) REFERENCES sportsbook_bets (id, tenant_id)` (migration adds `UNIQUE (id, tenant_id)` on `sportsbook_bets`) |
| `event_kind` | `TEXT NOT NULL` | `CHECK IN ('settlement','rollback','void','tombstone')` |
| `generation` | `INTEGER` | `CHECK (generation >= 1)`; NOT NULL for settlement/rollback/tombstone; NULL for void |
| `outcome` | `TEXT` | `CHECK IN ('won','lost')`; NOT NULL iff settlement |
| `payout_amount` | `BIGINT` | NOT NULL iff settlement; `CHECK (payout_amount >= 0)` |
| `asset_code` | `TEXT NOT NULL REFERENCES assets (code)` | |
| `void_reason` | `TEXT` | NOT NULL iff void; `CHECK IN ('market_cancelled','push','data_error')` (ADR 0034 §14.7 sub-reason pattern; `player_self_exclusion` is added only by the future consumer's own migration) |
| `reverses_settlement_id` | `UUID REFERENCES sportsbook_bet_settlements (id)` | NOT NULL iff rollback |
| `causation_record_id` | `UUID REFERENCES sportsbook_bet_settlements (id)` | re-settlement ⇒ the rollback/tombstone row of `generation − 1`; composed void ⇒ its rollback row; else NULL |
| `ledger_transaction_id` | `UUID NOT NULL` | `FOREIGN KEY (ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id)`; `UNIQUE` |
| `actor_staff_account_id` | `UUID` | the driving staff principal (§9); nullable for a future provider driver |
| `request_id` | `TEXT` | HTTP request id for traceability |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | |

Shape CHECKs per `event_kind` (the NOT NULL-iff rules above as one CHECK
each). Uniqueness (DB-enforced, proposal §L):

- `UNIQUE (bet_id, generation) WHERE event_kind IN ('settlement','tombstone')`
  — one settlement slot per generation; a tombstone occupies it.
- `UNIQUE (reverses_settlement_id) WHERE event_kind = 'rollback'` — at most
  one rollback per settlement.
- `UNIQUE (bet_id) WHERE event_kind = 'void'` — void is single-occurrence.
- `UNIQUE (ledger_transaction_id)`.

Security: `ENABLE` + `FORCE ROW LEVEL SECURITY`. Three policies (not
one `FOR ALL`, since the table is append-only): `tenant_staff_select FOR
SELECT USING (tenant match AND app.player_account_id empty)`;
`tenant_staff_insert FOR INSERT WITH CHECK (tenant match AND
app.player_account_id empty)`; `player_self_scope FOR SELECT` joined
through the bet's `player_account_id` (via `EXISTS` on `sportsbook_bets`,
itself RLS-scoped). No UPDATE/DELETE policy exists. A test asserts an
INSERT under a player-scoped connection is rejected. `BEFORE UPDATE OR DELETE … FOR EACH
ROW` and `BEFORE TRUNCATE … FOR EACH STATEMENT` triggers executing
`ledger_deny_mutation()` (migration 0021). Indexes: `(tenant_id, bet_id)`,
`(tenant_id, created_at)`. Runtime-role privileges: §3.5.

### 3.3 Triggers (migration 0091)

**T-1 `sportsbook_bet_settlements_validate` (BEFORE INSERT, row).** Under
the tenant's RLS, reading the parent bet and its existing history rows:

- `NEW.tenant_id`/`asset_code` equal the bet's.
- `settlement`/`tombstone`: no `void` row exists; no un-reversed
  settlement exists; `NEW.generation = COALESCE(max(generation) over
  settlement/tombstone rows, 0) + 1`.
- `settlement` & `won`: `payout_amount = bet.potential_return AND > 0`;
  `lost`: `payout_amount = 0` (DB-level anti-minting, §2.4).
- `rollback`: target row is this bet's, `event_kind = 'settlement'`, same
  `generation`, and is the latest settlement.
- `void`: no `void` row exists; no un-reversed settlement exists (a
  composed void is inserted after its rollback row).
- `ledger_transaction_id`'s `transaction_type` matches the kind
  (`settlement→sportsbook_settlement`, `rollback→sportsbook_rollback`,
  `void→sportsbook_void`, `tombstone→tombstone`) and its
  `correlation_id = bet_id`.
- `causation_record_id` follows §2.1's rule: NOT NULL (the generation
  g − 1 rollback/tombstone row) for a settlement with g > 1, NOT NULL (a
  rollback row of this bet) for a void inserted while that rollback is the
  bet's latest event, else NULL.

**T-2 `sportsbook_bets_status_transition` (BEFORE INSERT OR UPDATE OF
status, row).** INSERT requires `status = 'open'`. UPDATE requires
`(OLD.status, NEW.status)` ∈ §3.1's allowed set **and** `NEW.status =`
the status derived from history: any `void` row ⇒ `void`; else the latest
un-reversed settlement ⇒ `settled_<outcome>`; else `open`. The history row
is therefore always inserted **before** the status UPDATE; `status` is a
derived cache, never an independent fact.

**Both triggers are `SECURITY INVOKER`** (they run under the caller's RLS,
never as the owner) and **`RAISE` if the parent bet row is not visible**
— no tenant context, or a player-scoped connection — rather than passing
on an empty read. A missing or empty read is never interpreted as "no
history, therefore valid".

**Soundness of T-1's read-then-insert.** T-1 reads existing history and
then permits the insert — a check-then-insert that is sound only because
INV-LOCK-E4 (§5.3) serializes every writer of a given bet on its L1 row
lock, so no concurrent insert for the same bet can be in flight. The
partial unique indexes (§3.2) are the backstop if that invariant is ever
broken: they convert a would-be double settlement/rollback/void into a
constraint violation rather than a second row.

`sportsbook_bets_enforce_immutable_fields` is unchanged; a regression test
proves stake/odds/`potential_return`/references stay frozen through every
transition.

### 3.4 Read surfaces (W1 item 9, for `frontend`/`backoffice`)

`GET /v1/me/sportsbook/bets` and `GET /v1/admin/sportsbook/bets` add
read-only `status`, `outcome`, `payout_amount`, `settled_at`; admin also
lifecycle `ledger_transaction_id`s and `correlation_id`. No UI controls.
The player endpoint **must not** expose `actor_staff_account_id` or
`request_id` (a test asserts both are absent from the player response).
Implementation must locate any exact-JSON or snapshot tests of either
bet-history response and update them in the same change.

### 3.5 Runtime-role privileges

The deny triggers are the **binding** control (they bind the owner too).
Proposal §L also asks for runtime-role INSERT/SELECT only, but
`deploy/init-app-role.sql:85–89` default privileges grant UPDATE/DELETE
on every new table. Migration 0091 therefore runs, guarded by role
existence (`DO $$ IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname =
'igaming_runtime') …`), `REVOKE UPDATE, DELETE, TRUNCATE ON
sportsbook_bet_settlements FROM igaming_runtime` — new precedent (no
migration references the runtime role today). Because a re-run of the
init script's backfill GRANT (:85) would re-grant, **`deploy/init-app-
role.sql` also gains a table-existence-guarded `REVOKE UPDATE, DELETE,
TRUNCATE ON sportsbook_bet_settlements FROM igaming_runtime` placed after
its grants**, mirroring its guarded `schema_migrations` narrowing
(:99–107); the same statement is added to
`docs/security/runtime-role-separation.md` §6, and the CI runtime-role
setup step notes it. The script change is W1 implementation work owned by
`devops`. `runtime_role_separation_test.go` gains a probe asserting
UPDATE/DELETE/TRUNCATE fail as the runtime role; a **separate** test
(§14) proves the deny triggers alone reject all three for a role that
still holds the privileges (the owner). **Closed** (review S2): the deny
triggers are the binding control; the REVOKEs are defence in depth.

### 3.6 Money width (recorded debt)

`payout_amount` is `BIGINT` to match `sportsbook_bets.stake_amount`/
`potential_return` and Go `int64` `EntryInput.Amount`
(`ledger_entries.amount` is already `NUMERIC(38,0)`). **Debt:** a
crypto-precision (exponent 18) sportsbook requires widening
`sportsbook_bets` money columns, this table, and the `int64` posting path
to `NUMERIC(38,0)`/`big.Int` per `CLAUDE.md`; not in Stage 10 (§I). All
new aggregate reads (reconciliation, §8) scan `NUMERIC` into `big.Int`,
never `int64` sums (the `internal/risk.numericToBigInt` discipline).

## 4. Idempotency and payload integrity (in-house mode)

### 4.1 Single authority

ADR 0038 §14.6 in-house routing for the whole lifecycle: `provider_id` and
`provider_tx_id` NULL; the ledger's unconditional `UNIQUE (tenant_id,
idempotency_key)` (`ledger_transactions_tenant_idempotency_key_key`) is
the **only** idempotency authority. The history table's partial unique
indexes (§3.2) enforce **state** (one settlement per generation, one
rollback per settlement, one void per bet) — they are not a parallel key
table and are never consulted instead of the ledger key.

### 4.2 Key formats (server-composed; fixed prefixes)

| Posting | `idempotency_key` |
|---|---|
| Settlement, generation g | `sportsbook_settlement:<bet_id>#<g>` |
| Tombstone for never-seen settlement g | `sportsbook_settlement:<bet_id>#<g>` (**same key** — it occupies the slot, casino `postRollbackTombstone` pattern) |
| Rollback of settlement g | `sportsbook_rollback:<bet_id>#<g>` |
| Void (before or after settlement) | `sportsbook_void:<bet_id>` |

`<bet_id>`: canonical lowercase UUID; `<g>`: unpadded base-10. `#<g>` is
ADR 0038 §14.1/§14.6's `{reference}#{occurrence_ordinal}` composition,
`generation` being the ordinal. Tenant is implicit in the constraint and
`bet_id` fixes the player.

**Generation is caller-supplied and server-validated, never counted.** ADR
0038 §14.1 forbids "count existing rows and add one": a delayed duplicate
of `settle(1)` arriving after `rollback(1)` would be counted as generation
2 and post a real second settlement. The request carries `generation`
(the simulated provider's per-occurrence signal, §9); the server validates
it under L1 (§4.3). "Server-derived key" thus means: composed by the
server from a server-resolved bet id and a validated generation — never
a client-chosen string.

**Namespace reservation.** The three prefixes are reserved; W1 item 0
(§11) confirms no existing caller can produce a key in them from client
input (a pre-occupied key would be a settlement denial-of-service).

### 4.3 Decision table (evaluated under the L1 bet lock, before any L3 lock)

Let `G` = max `generation` over the bet's `settlement`/`tombstone` rows (0
if none); "current settlement" = the latest un-reversed settlement row.

**settle(g, outcome, claim)**

| Condition | Result |
|---|---|
| a `settlement` row for g exists | payload compare (§4.4): equal ⇒ **replayed** (no posting; return original); differ ⇒ reject `SETTLEMENT_PAYLOAD_MISMATCH` + alert |
| a `tombstone` row for g exists | reject `SETTLEMENT_TOMBSTONED` (typed `ErrSettlementTombstoned`) + alert; bet unchanged |
| bet `void` | reject `BET_VOIDED` + alert |
| bet `settled_*` (g > G) | reject `BET_ALREADY_SETTLED` (rollback required) + alert |
| bet `open`, g = G + 1 | validate §2.4 V-1…V-5 ⇒ post settlement, insert row, status UPDATE |
| g ≠ G + 1 otherwise | reject `GENERATION_OUT_OF_SEQUENCE`; nothing posted |

**rollback(g)**

| Condition | Result |
|---|---|
| a `rollback` row reversing settlement g exists | **replayed** |
| a `tombstone` row for g exists | **replayed** (returns the tombstone) |
| current settlement has generation g | post rollback (§2.3), insert row, status → `open` |
| bet `open`, g = G + 1, no row for g | **tombstone** (§4.5) |
| bet `void` (and no row above matched) | reject `BET_VOIDED` |
| otherwise | reject `GENERATION_OUT_OF_SEQUENCE` |

**void(void_reason)**

| Condition | Result |
|---|---|
| a `void` row exists | compare `void_reason`: equal ⇒ **replayed**; differ ⇒ `SETTLEMENT_PAYLOAD_MISMATCH` + alert |
| bet `open` | post void-before (§2.3) |
| bet `settled_*` at generation g | post rollback of g (key `sportsbook_rollback:<bet>#g`) then void, one DB transaction; two history rows; two status UPDATEs |

A void request carries no `generation`. A later standalone `rollback(g)`
redelivery after a composed void resolves as **replayed** (same key).

### 4.4 Payload comparison, rejection and alerting

- Compared fields: settlement — `outcome`, claim `payout_amount`, claim
  `asset_code` against the stored row; void — `void_reason`; rollback —
  none beyond `(bet, g)`.
- Every rejection in §4.3 and every §2.4 validation failure is returned as
  a **result**, not a Go error, so its audit record commits with **no**
  ledger write (the `PlaceBet` "declined" pattern): audit action
  `sportsbook_bet.settlement_rejected`, `Outcome = failure`, metadata
  `{rejection_code, event_type, generation, bet_status}` (`rejection_code`
  rather than `reason_code`, which §10 reserves).
- Integrity alerts are `logger.Error` events named
  `sportsbook_settlement_integrity_alert_<reason>` (the
  `casino_play_integrity_alert_*` convention,
  `internal/httpserver/casino_play_handlers.go:226`), emitted for
  `SETTLEMENT_PAYLOAD_MISMATCH`, `SETTLEMENT_TOMBSTONED`, `BET_VOIDED`,
  `BET_ALREADY_SETTLED`, payout/asset validation failures, and every
  §4.7 failure. **Alert fields are limited to** `tenant_id`, `bet_id`,
  `reason`, `event_type`, `generation`, `bet_status`, the staff actor id
  and `request_id` — never the request body, headers, token, or player
  PII.

### 4.5 Tombstones (rollback before settlement)

`ledger.Post(TransactionInput{TransactionType: TxTombstone, IdempotencyKey:
"sportsbook_settlement:<bet>#<g>", CorrelationID: bet.id, Entries: nil})`
plus a history row `event_kind = 'tombstone'`, `generation = g`. Effects:
a late `settle(g)` is rejected twice over — by §4.3 (row exists) and, as a
backstop, by the ledger key (type mismatch ⇒ `ErrIdempotencyKeyReused`,
mapped to `ErrSettlementTombstoned`). The bet stays `open`, `NetLocked`
unchanged; `settle(g+1)` succeeds with `causation_record_id` = the
tombstone row. Audit action `sportsbook_bet.rollback_tombstoned`.
Tombstones are permitted only for `g = G + 1` on an `open` bet (bounds the
ADR 0048 "forged reference writes a permanent tombstone" DoS to one
generation of one bet in the caller's own tenant).

### 4.6 Settlement/rollback/void for an unknown bet

Resolved by `GetBetByID` in the caller's tenant-scoped transaction (RLS).
Not found (nonexistent or another tenant's) ⇒ `404`, identical body,
**no posting, no tombstone**; alert
`sportsbook_settlement_integrity_alert_bet_not_found`; rejection audit
with `TargetID` = requested id. No tombstone: in-house bet ids are minted
synchronously by `PlaceBet`, so an unknown id can never arrive late
(unlike a provider-attested reference). A bet failing §2.2's checks ⇒
`409 SETTLEMENT_INTEGRITY` + alert (ADR 0038 §5).

### 4.7 `ledger.Post` backstop

W1 never relies on `Post`'s replay path for correctness: every replay is
detected at §4.3 before L3. If `Post` nevertheless returns
`AlreadyPosted = true`, or `ErrIdempotencyKeyReused`, for a posting §4.3
classified as new, the operation returns a Go error
(`ErrSettlementIntegrity`) so the whole transaction rolls back (the
`PlaceBet` orphan-guard pattern, `orchestrator.go:451–472`); the handler
emits the integrity alert and writes the rejection audit in a **separate**
tenant-scoped transaction (the failed one cannot carry it). This is
independent of F-7's outcome (§11).

## 5. Lock order

### 5.1 Normative sequence (every W1 operation, one bet per DB transaction)

| Step | Action | Class |
|---|---|---|
| 1 | `SELECT … FROM sportsbook_bets WHERE id = $1 FOR UPDATE` (RLS-scoped) | **L1** |
| 2 | Read history rows, placement posting, `NetLocked` (§2.2); evaluate §4.3 and §2.4. Rejections return here (no L2/L3 taken) | — |
| 3 | Rollback/void-after-settlement only: `SELECT id FROM ledger_transactions WHERE id = <settlement tx> FOR UPDATE`; confirm no ledger transaction with `reverses_transaction_id` = it exists (ADR 0038 §10 double-reversal protection; redundant under L1, kept for any future non-L1 writer) | **L2** |
| 4 | `ledger.GetOrCreateAccounts` for `HOUSE`; build every `TransactionInput` of the operation | (pre-L3) |
| 5 | `ledger.LockProjectionsForPostings(ctx, tx, in₁[, in₂])` over the complete entry set of **all** postings of the operation | **L3** |
| 6 | `ledger.Post` for each input, in order (rollback before void) | **L4** |
| 7 | Insert history row(s); `UPDATE sportsbook_bets SET status` (after each row); `audit.Record` | E-4 (§5.3) |

No L0 lock is taken (no RG, Risk, or exposure evaluation — ADR 0038 §13:
settlement/void/rollback are not Risk checkpoints; §5.4 for L0.6). Settling
several bets in one transaction is not built; any future batch driver must
lock them in one `ORDER BY id FOR UPDATE` statement (INV-LOCK-E3's
ascending-id rule).

### 5.2 Multi-posting pre-lock (new `internal/ledger` API)

Void-after-settlement posts two transactions. Their projection sets differ
(lost: rollback {`LOCKED`,`HOUSE`}, void {`LOCKED`,`CASH`}). Locking per
`Post` would acquire `CASH` while holding `LOCKED`/`HOUSE` — a subset
pre-lock (R3 violation) that can deadlock against `PlaceBet` on the same
wallet. `LockProjectionsForPosting` accepts one input, so R1 cannot be met
with the current API.

**Decision:** add to `internal/ledger/lockorder.go`:

```go
// LockProjectionsForPostings is LockProjectionsForPosting over the union of
// several postings the caller will Post, in order, in this same transaction.
func LockProjectionsForPostings(ctx context.Context, tx pgx.Tx, ins ...TransactionInput) (LockedProjections, error)
```

It runs `prepareEntries` per input, unions account ids, applies
`canonicalAccountOrder`, calls `ensureAndLockProjectionsInOrder` once, and
rejects inputs with differing `TenantID`. R3 generalizes to: "the set of
inputs pre-locked is exactly the set subsequently posted, with identical
`Entries` and `BonusCost`". Each subsequent `Post` re-locks rows already
held (no-op). Single-posting W1 operations may call either function. R4
is preserved (still inside `internal/ledger`).

### 5.3 Named exception E-4 — post-L4 history insert and status UPDATE

History rows cannot be inserted before `Post`: `ledger_transaction_id` is
`NOT NULL` with an FK, `Post` mints the id internally, and the table is
append-only (no reserve-then-update). So step 7 follows L4, as `PlaceBet`'s
`insertBet` does (E-3 precedent).

**Safety argument.** The status UPDATE is **not HOT**: `status` is in the
predicate of `idx_sportsbook_bets_open_exposure` (migration 0088:124), so
the UPDATE writes new entries into every index on `sportsbook_bets`,
including `UNIQUE (tenant_id, player_account_id, idempotency_key)`, and
another transaction's unique check on that key may **wait on this
in-progress updater**. That wait cannot close a cycle, for one reason:
**step 7 begins only after step 5 already holds every L3 lock this
transaction will ever take**. From step 7 onward this transaction
acquires nothing a `PlaceBet` (or any L3-holding transaction) could hold:
the history unique-index entries and the FK `KEY SHARE` are keyed by this
bet (every other writer of this bet is queued behind the same L1 lock
before it reaches L3), the bet row is already locked by this transaction,
and `UNIQUE (ledger_transaction_id)` keys a row this transaction just
created. So anything waiting on step 7 waits on a transaction that will
finish without waiting on it.

> **INV-LOCK-E4:** every INSERT into `sportsbook_bet_settlements` and every
> UPDATE of `sportsbook_bets.status` happens in a transaction that (i)
> took the bet's `FOR UPDATE` row lock at L1, before any L2/L3 lock, and
> (ii) performs the INSERT/UPDATE only **after** all of its L3 locks are
> held (after `LockProjectionsForPostings`). Sole writers: the named W1
> settlement functions in `internal/sportsbook`, verified by the static
> test `sportsbook_settlement_sole_writer_test.go` (§14; owned by
> `code-reviewer` and `qa`).

**`sportsbook_bets` writers.** They are `insertBet` (E-3, from `PlaceBet`)
and the W1 settlement functions, which take L1 first (INV-LOCK-E4).
ADR 0082 A3's and ADR 0083 §7.3/§10's "exactly one writer" statements are
reworded to this (§5.5, §13). INV-LOCK-E3's forward condition ("a second
writer … must take its row lock at L1 before `LockProjectionsForPosting`")
is met. `PlaceBet` inserts only **new** rows and never locks an existing
bet row; E-3 itself is not closed by W1 (review A-OI-4, §15).

### 5.4 L0.6 — not taken (decision)

ADR 0082 Amendment A2 bound settlement/void/partial/cashout prospectively
to take L0.6. **Decision: no W1 path (settle, void, rollback) takes
L0.6**; A2's prospective binding is amended for those three only —
partial settlement and cashout remain bound by A2 (§5.5).

- **Settle and void** only move a bet out of `open`. A concurrent
  `PlaceBet` holding L0.6 reads the aggregate unlocked (READ COMMITTED);
  a settlement committing in between makes it over-count — the safe
  direction.
- **Standalone rollback** returns a bet to `open` and **increases**
  exposure. Taking L0.6 *would* help here: a rollback holding L0.6 would
  make a concurrent `PlaceBet` on the same event wait and then see the
  re-opened bet. (Rollback is still never rejected for exposure, ADR 0038
  §13, so the ceiling is a placement-time gate, not a book invariant.)
- **The reason L0.6 is not taken:** the L0.6 key is the **event id**, and
  `sportsbook_bets` has no `event_id` column (migration 0078 stores only
  `selection_id`). Deriving the key requires reading `sb_selections` →
  `sb_markets` before L1 (R8: L0 precedes L1), which contradicts
  INV-SB-SETTLE-6 and ADR 0047 §5(c)'s deferment (settlement never reads
  the catalogue). Without L0.6 the residual is bounded: bets admitted
  concurrently with an in-flight rollback may push an armed ceiling over
  by those bets — the same class as ADR 0082 review P3-3 (arming
  transient).
- **Binding condition:** this no-L0.6 decision **must be revisited before
  any exposure limit is armed** (any `sb_exposure_limits` row created in
  an environment — HDR-SB-1). Today zero rows exist. The residual is
  recorded in ADR 0083 §6.2.5 next to the existing arming under-count
  note (amendment text in §13).

### 5.5 ADR 0082 amendment text (Amendment A4, applied on acceptance)

> **Amendment A4 — Stage 10 W1 — sportsbook settlement, void and rollback
> (ADR 0088 §5).** (1) §1.5 row "settlement / void …" now reads: L1
> `sportsbook_bets` row `FOR UPDATE` → (rollback/void-after-settlement) L2
> settlement `ledger_transactions` `FOR UPDATE` → L3
> `LockProjectionsForPostings` over every posting → L4 `Post` (one or
> two) → E-4. (2) §2.2 R1/R3: an operation posting more than one
> transaction pre-locks the union once via `LockProjectionsForPostings`;
> the pre-locked inputs are exactly those posted. (3) §2.1 exception list
> becomes "E-1, E-2, E-3 and E-4". (4) New §5.1c **E-4** with
> INV-LOCK-E4 (text of ADR 0088 §5.3, including the "writes only after
> all L3 locks are held" clause). (5) Amendment A2's "Prospective
> binding" paragraph: settlement, void and rollback do **not** take L0.6,
> because `sportsbook_bets` carries no `event_id` and deriving the key
> would read the catalogue before L1 (ADR 0088 §5.4); this must be
> revisited before any exposure limit is armed (HDR-SB-1). Partial
> settlement and cashout remain bound by A2 until their own ADR. (6)
> Amendment A3's "`sportsbook_bets` has exactly one writer" and
> INV-LOCK-E3's text become: "writers are `insertBet` (E-3) and the W1
> settlement functions, which take L1 first (INV-LOCK-E4)". (7)
> §4.4/§5.3 "NOT IMPLEMENTED" updated to point here. (8) Also apply the
> pending P3-2 note ("A2 subsequently made — see below").

## 6. Risk and exposure

### 6.1 Cumulative spec (INV-SB-CUM-1) — same commit as migration 0091

```go
OperationSportsbookBet: {
    TransactionTypes:     []string{"sportsbook_bet"},
    ReversalTypes:        []string{"sportsbook_void"}, // exactly; never sportsbook_rollback (ADR 0038 §13)
    MeasuredAccountTypes: []string{"player_cash"},
    IgnoredAccountTypes:  []string{"player_locked_cash"},
    ConsumingDirection:   directionDebit,
},
```

`MeasuredAccountTypes`/`IgnoredAccountTypes` need no change: the void's
player-side legs are `player_cash` (measured) and `player_locked_cash`
(ignored); `house_gaming` carries no `player_account_id` and is excluded
by the query (`cumulative.go` `le.player_account_id = $2`). No
`ErrUnrecognizedCumulativeLeg` can arise. `sportsbook_settlement` and
`sportsbook_rollback` are in neither list and are invisible to the
measure (confirmed by `risk` review).

Same commit (INV-SB-CUM-1, and §12): `internal/risk/cumulative_sportsbook_test.go`
:42–43 is rewritten to assert `ReversalTypes` is exactly
`["sportsbook_void"]` and that `sportsbook_rollback` and
`sportsbook_settlement` are absent; the "deliberately EMPTY" comments at
`internal/risk/cumulative.go`:116–119 and :237 are updated.

Settle, void and rollback **deliberately do not take the Risk cumulative
advisory lock (L0.5)**: a void racing a `PlaceBet` can only lower usage,
and a void the concurrent evaluation misses makes it over-count — fail
closed.

### 6.2 Netting (per bet, stake S)

| Sequence | Consumed | Why |
|---|---|---|
| place | +S | Dr `player_cash` |
| place → settle (won or lost) | +S | settlement not netted (§13) |
| place → void-before | 0 | void Cr `player_cash` S nets −S |
| place → settle → void-after (rollback + void) | 0 | rollback invisible; void nets once |
| place → settle → rollback | +S | rollback never netted |
| place → settle → rollback → void | 0 | nets exactly once via the void |
| place → settle → rollback → re-settle | +S | |

**Limit semantics — named debt (Orchestrator ruling R-3; accepted by
`risk`).** Cumulative stake limits measure **net outflow by posting
time**, not gross stakes placed within the window: netting is by each
entry's `created_at`, so a void inside the window of a bet placed before
the window start contributes −S to that window, and a new stake S is then
admitted (the same property `casino_rollback` netting has today). The
shared netting query in `internal/risk/cumulative.go` is **not changed**.
A pinning integration test records the behaviour (bet before the window,
void inside it ⇒ window usage −S, new stake S admitted, §14). The
residual is recorded in ADR 0083 §6.1.2 (§13). It becomes **P1 for both
casino and sportsbook** if product/compliance rules that limits must be
gross-by-placement.

### 6.3 Exposure

Unchanged code: `status = 'open'` (`exposure.go`). Settle/void remove the
bet; rollback re-adds it; void is terminal. Tests: a bet blocked by an
armed limit is admitted after another bet settles/voids; a rolled-back bet
is counted again. INV-SB-EXP-1/EXP-2 unchanged (settlement never writes
exposure, never crosses tenants).

## 7. Invariants introduced

- **INV-SB-SETTLE-1** — posted amounts derive only from the placement
  posting and frozen `potential_return` (§2.2, §2.4).
- **INV-SB-SETTLE-2** — every post-placement transaction with
  `correlation_id = bet.id` is written under the bet's L1 lock.
- **INV-SB-SETTLE-3** — `sportsbook_bets.status` = history-derived status
  at every commit (T-2).
- **INV-SB-SETTLE-4** — history rows and new-type/sportsbook-tombstone
  ledger transactions correspond one-to-one (§8.3).
- **INV-SB-SETTLE-5** — the cumulative-spec requirement is INV-SB-CUM-1
  (ADR 0083 §10), as instantiated in §6.1; not restated here.
- **INV-SB-SETTLE-6** — settlement never reads or locks `sb_selections`/
  `sb_markets`/`sb_events` (ADR 0047 §5(c) unaffected).

## 8. Reconciliation — sportsbook stream

New stream `sportsbook_settlement` in `internal/reconciliation` (same
`reconciliation_runs`/`reconciliation_mismatches` tables; zero tolerance,
reconciliation-model §1), run per tenant by the existing sweep
(`scheduler.go` `RunSweep`) after `ledger_vs_projection`. Migration 0091
widens `reconciliation_mismatches.mismatch_kind` (migration 0031 CHECK)
with: `sb_locked_mismatch`, `sb_bet_net_mismatch`, `sb_orphan_ledger`,
`sb_orphan_history`, `sb_status_mismatch`, `sb_mock_statement_mismatch`.
All sums scanned as `NUMERIC` → `big.Int`.

### 8.1 (a) Locked balance vs open stakes — per wallet and asset

Σ over `player_locked_cash` entries **of sportsbook transaction types
only** (`sportsbook_bet`, `sportsbook_settlement`, `sportsbook_void`,
`sportsbook_rollback`) of (credit − debit) = Σ `stake_amount` of the
wallet's `open` bets in that asset. Scoping is mandatory: casino also
writes `player_locked_cash`. Also Σ over `player_locked_bonus` entries of
the same sportsbook types = 0 (never the wallet's whole
`player_locked_bonus` balance — casino bonus bets use it; OI-2).

### 8.2 (b) Per-bet netting (`correlation_id = bet.id`)

For each bet, the nets of `CASH`, `LOCKED`, `HOUSE` over its sportsbook
transactions must equal §2.3's end-state table for its status, with `P` =
the payout of its current un-reversed settlement row; a `settled_won` bet's
un-reversed ledger payout (the `HOUSE`→`CASH` pair) equals that row's
`payout_amount`.

### 8.3 (c) Two-way orphan check

- Every ledger transaction of type `sportsbook_settlement`/`_void`/
  `_rollback`, and every `tombstone` whose `idempotency_key` starts with
  `sportsbook_settlement:`, has exactly one history row referencing it,
  whose `bet_id = correlation_id` and whose kind matches.
- Every history row's `ledger_transaction_id` exists with the matching
  type and `correlation_id`.
- Causation is consistent with §2.1: a settlement row with g > 1 has a
  ledger `causation_id` equal to the ledger transaction of its
  `causation_record_id` (the g − 1 rollback/tombstone); a composed void's
  ledger `causation_id` equals its rollback row's ledger transaction; every
  other W1 transaction has `causation_id IS NULL`.
- `sportsbook_bets.status` = derived status (`sb_status_mismatch`).

### 8.4 (d) Provider-statement match — **MOCK**

A mock statement source in `internal/sportsbook/mock.go` renders (bet id,
generation, outcome, payout, asset, void flag) from the history table
itself — tautological by construction, no new storage — and the stream
matches it; a test feeds a divergent statement to prove detection.
Labelled **MOCK** in code, logs and records; real statement matching is
**PROVIDER DEPENDENT** (ADR 0038 §12). A negative test injects drift per
mismatch kind (scratch DB, owner, triggers bypassed) and asserts
detection.

## 9. Test-support staff route

### 9.1 Registration and gate

- `POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event`
- New `httpserver.Deps.SportsbookSettlementSimulationEnabled`, set in
  `cmd/platform-api/main.go` to `cfg.TestSupportRoutesEnabled()` (ADR 0085
  double opt-in: `Environment != "production" &&
  TestSupportEndpointsEnabled`). Registered in `registerSportsbookRoutes`
  **only** when that flag **and** `SportsbookEnabled` are true; otherwise
  the pattern is never added to the mux. `Config.TestSupportRoutesEnabled`'s
  doc comment ("three … routes") is updated to four.
- Chain: `auth.Middleware` → `auth.RequireTenantScope` → new
  `auth.RequireStaffPrincipal` →
  `auth.RequirePermission(auth.PermSportsbookSettlementSimulate)`.
- `RequireStaffPrincipal` admits **exactly** `PrincipalStaff`
  (`internal/auth/jwt.go`); `PrincipalPlayer` and `PrincipalService` are
  rejected with `403` in `RequirePlayerPrincipal`'s style
  (`internal/auth/middleware.go:85`); unit tests cover player, service and
  staff tokens. The handler parses `tc.Subject` as a UUID or fails closed.
  Recommended: inside the posting transaction, verify the staff user
  exists and is active in the tenant before any posting.
- New permission `PermSportsbookSettlementSimulate =
  "sportsbook_settlement:simulate"`. **Sole grantee: `RoleRiskManager`**
  (security review S1). `RoleFinance` is rejected: it holds withdrawal
  approval, and combining that with the ability to drive payouts breaks
  ADR 0024's separation of duties. Never granted to `RoleTenantAdmin`,
  `RoleFinance`, `RoleCompliance`, `RoleSupport`, `RolePlatformAdmin` or
  any bonus role. The permission-table test asserts **exactly one**
  grantee, by permission constant (§14). Not added to
  `backoffice/src/auth/permissions.ts` (no UI control exists). The
  permission is inert in production because the route does not exist
  there.
- OpenAPI (`docs/api/openapi/platform-api.yaml`): documented with
  `x-test-support: true` and the sentence "Absent unless test-support
  routes are enabled; never present when `APP_ENV=production`; simulates an
  in-house provider event; not an operator settlement feature."
- **Removal condition (ADR 0048 pattern).** Once a real sportsbook
  settlement provider is registered for any tenant, this route is removed
  or permanently disabled in the same change; it is not a pattern intended
  to survive the real-provider stage.

### 9.2 Request

```json
{
  "event_type": "settle | void | rollback",
  "generation": 1,
  "outcome": "won | lost",
  "payout_amount": 2500,
  "asset_code": "EUR",
  "void_reason": "market_cancelled | push | data_error"
}
```

| Field | settle | rollback | void |
|---|---|---|---|
| `generation` (int ≥ 1) | required | required | forbidden |
| `outcome` | required | forbidden | forbidden |
| `payout_amount` (claim, §2.4) | required | forbidden | forbidden |
| `asset_code` (claim) | required | forbidden | forbidden |
| `void_reason` | forbidden | forbidden | required |

Unknown fields rejected; body size limited by the existing decoder. The
request never carries tenant, player, wallet, account, stake, or any
amount that is posted. `{id}` is resolved inside
`WithTenant(tc.TenantID)` (RLS), satisfying ADR 0048 mitigation 2 (every
effect-bearing field is re-derived server-side from the bet row).

### 9.3 Response

`200` with `{bet_id, event_type, result: "applied"|"replayed"|
"tombstoned", bet_status, generation, ledger_transaction_ids: [...],
settlement_record_ids: [...]}`.

| HTTP | Code | When |
|---|---|---|
| 400 | `VALIDATION_FAILED` | shape/field-matrix violation, V-5 |
| 401/403 | existing | no token; player or platform-admin token; missing permission |
| 404 | `NOT_FOUND` | route absent (flag off / production), or bet not in tenant (§4.6) |
| 409 | `SETTLEMENT_PAYLOAD_MISMATCH`, `SETTLEMENT_TOMBSTONED`, `BET_VOIDED`, `BET_ALREADY_SETTLED`, `GENERATION_OUT_OF_SEQUENCE`, `SETTLEMENT_INTEGRITY` | §4.3–§4.7 |
| 422 | `PAYOUT_INVALID`, `ASSET_MISMATCH` | V-1…V-4 |

ADR 0048 mitigations 1–5 map to: the §9.1 gate; RLS-resolved bet and
L1-validated generation; no posted amount accepted (§2.4); §4.2 keys; the
staff-attributed audit (§10). R-1 and ADR 0087 bind: no webhook, never a
player capability, never a production bypass. Settlement and void are not
gated on the player's RG/KYC status (casino rollback precedent).

## 10. Audit and client IP

One `audit.Record` per state transition, in the posting transaction:

| Action | When | `TargetType`/`TargetID` |
|---|---|---|
| `sportsbook_bet.settled` | settlement (incl. re-settlement) | `sportsbook_bet` / bet id |
| `sportsbook_bet.rolled_back` | rollback (standalone or composed) | same |
| `sportsbook_bet.voided` | void (either path) | same |
| `sportsbook_bet.rollback_tombstoned` | §4.5 | same |
| `sportsbook_bet.settlement_rejected` | §4.3/§4.4/§4.6 rejections (`Outcome = failure`) | same, or requested id |

`ActorType = staff`, `ActorID` = staff account id, `TenantID` from the
authenticated context. Metadata: `before_status`, `after_status`,
`event_type`, `generation`, `outcome`, `payout_amount`, `asset_code`,
`void_reason`, `ledger_transaction_ids`, `settlement_record_ids`,
`replayed`, `driver: "test_support_simulation"`, `mode: "in_house_mock"`,
`remote_addr` (raw `RemoteAddr`). `reason_code` = `"test_support_simulation"`
for settle and rollback records; void records carry `void_reason`
instead. A replay writes an audit record with `replayed: true`
(traceability of retries) but posts nothing.

**Client IP decision (confirmed by `security`, S6):** `IPAddress =
trustedProxyClientIP(r, deps.TrustedProxyCount)`
(`internal/httpserver/ratelimit.go:179`), which equals `clientIP(r)` when
`TRUSTED_PROXY_COUNT = 0`; raw `RemoteAddr` goes in metadata; the raw
`X-Forwarded-For` header is **never** copied into the record. This
**neither affects nor partially closes** ADR 0086's audit-IP production
launch gate (other call sites still use `clientIP`; the duplicate-
`X-Forwarded-For`-lines defect remains). In staging the value is
best-effort.

## 11. F-7 — `ledger.Post` replay compares only the type (W1 item 0)

**Must complete and be recorded before W1 code merges.** Owner:
`ledger-finance`.

### 11.1 Audit method (21 call sites at HEAD)

`internal/withdrawal/withdrawal.go` (5), `internal/casino/orchestrator.go`
(3), `internal/casino/bonus_settlement.go` (5), `internal/bonus/
lifecycle.go` (2), `internal/bonus/held_disposition_ops.go` (1),
`internal/bonus/conversion.go` (1), `internal/sportsbook/orchestrator.go`
(1), `internal/payments/orchestrator.go` (3). Re-grep `ledger\.Post(` at
audit time; the list is not assumed complete, and the audit also covers
**indirect callers** (wrappers around `ledger.Post`, and any
interface-typed or function-value `Post`). The audit is recorded against
a named commit SHA. For each site record:

1. **Key derivation** — format, inputs, which are client/provider
   controlled, whether a server prefix reserves the namespace, and
   explicitly **whether a client or provider can produce a key beginning
   `sportsbook_settlement:`, `sportsbook_rollback:` or `sportsbook_void:`**
   (§4.2's reservation check).
2. **Payload determination** — can one key carry different `Entries`,
   `BonusCost`, `ReversesTransactionID` or `CorrelationID`?
3. **Caller-side comparison** — e.g. `PlaceBet`'s
   `findBetByIdempotencyKey`/`insertBet` checks, casino's
   `existingReversalProviderTxID` check.
4. **Reachability** from any external input (player, staff, provider
   callback, test-support route).
5. **Evidence** — a named integration test per classification.

### 11.2 Classification and decision rule

- **A — key determines payload** (every payload input is part of, or
  functionally determined by, the key). Not exposed.
- **B — caller compares** a complete payload before trusting
  `AlreadyPosted`. Not exposed.
- **C — exposed**: a different-payload replay is reachable and silently
  returns the original.

Rule: all sites A/B ⇒ record "no defect", **do not change `Post`
semantics**; correct the `ErrIdempotencyKeyReused` doc comment and ADR
0038 §11 wording (§13). Any C ⇒ report to the Orchestrator as its **own**
item with its own review (proposal W1 item 0) — not folded into W1. A
ledger-level fix (comparing the canonical final entry set, reversal link
and correlation on replay, returning a new typed error) is permitted only
with `ledger-finance` review, a replay-regression test per existing
caller proving legitimate retries still match (Rule B2 mirror legs are
state-derived via `BonusCost`), and an ADR 0020 amendment. Never a silent
semantic change. W1 does not depend on the outcome (§4.7).

## 12. Migration 0091, down-migration and rollback rules

Contents (one migration, one commit with §6.1's spec change —
INV-SB-CUM-1): transaction-type CHECK widening; `UNIQUE (id, tenant_id)`
on `sportsbook_bets`; `sportsbook_bet_settlements` with RLS, deny
triggers, T-1; T-2 on `sportsbook_bets`; `mismatch_kind` widening; the
§3.5 guarded REVOKE.

**Down migration refuses** (single transaction; checks run **before** any
drop). FORCE RLS applies to the owner, so a `count(*)` in a migration
sees zero tenant rows and would wrongly proceed; every check therefore
uses **constraint validation, which RLS cannot filter** (the migration
0078 down-script technique):

1. Re-add the 17-value `transaction_type` CHECK inside `DO … EXCEPTION
   WHEN check_violation THEN RAISE` (any new-type row ⇒ refuse).
2. `ALTER TABLE ledger_transactions ADD CONSTRAINT tmp_no_sb_tombstone
   CHECK (NOT (transaction_type = 'tombstone' AND idempotency_key LIKE
   'sportsbook\_settlement:%'))` — validation failure ⇒ refuse; else drop
   the temporary constraint.
3. `ALTER TABLE sportsbook_bet_settlements ADD CONSTRAINT tmp_empty CHECK
   (false)` — succeeds only on an empty table; else refuse.
4. `ALTER TABLE sportsbook_bets ADD CONSTRAINT tmp_all_open CHECK (status
   = 'open')` — else refuse; then drop it.
5. Only then drop T-2, T-1, the table, the unique constraint, and restore
   the `mismatch_kind` CHECK (which itself refuses if a new-kind mismatch
   row exists).

Refusal message: "migration 0091 (down): sportsbook settlement evidence
exists … irreversible once any settlement, void, rollback or sportsbook
tombstone has been posted; roll forward". After the first posting in any
environment, rollback is **forward-fix only**; ledger and history rows are
never deleted. A code revert with the migration kept must still render
non-open bets (read paths tolerate all four statuses; tested).

## 13. Record amendments (texts applied on acceptance, not by this draft)

| Record | Amendment |
|---|---|
| ADR 0038 header | Cash-funded singles settlement/void/rollback in in-house mode: IMPLEMENTED by Stage 10 (ADR 0088) once W1 lands; provider mode, bonus, cashout, partial unchanged |
| ADR 0038 §1, §5, §8.1, §13 | Replace `player_locked` with `player_locked_cash` (cash-funded) / `player_locked_bonus` (blocked), per migration 0048 |
| ADR 0038 §5 | Add: "In in-house mode (ADR 0088 §2.4) the payout is derived from the frozen `potential_return` and any stated payout is validated against it (anti-minting). The provider-mode posture above is unchanged." |
| ADR 0038 §8.1 | Void-after-settlement row → "`sportsbook_rollback` (exact inverse, `reverses_transaction_id` = settlement) + before-settlement-shape `sportsbook_void`, one DB transaction (ADR 0088 §2.3)"; delete the parenthetical about reversing the bet transaction; in-house void key `sportsbook_void:<bet_id>` |
| ADR 0038 §10 | In-house keys per ADR 0088 §4.2; tombstone occupies `sportsbook_settlement:<bet_id>#<g>`; re-settlement `causation_id` = the rollback/tombstone ledger transaction |
| ADR 0038 §11 | "Same-key-different-payload" paragraph: "As implemented, `ledger.Post` rejects only a differing `transaction_type` (F-7); sportsbook lifecycle payload comparison is performed in `internal/sportsbook` against `sportsbook_bet_settlements` under the bet lock (ADR 0088 §4.4). Ledger-level behaviour per ADR 0088 §11's outcome." |
| ADR 0038 §13 | `operationLedgerTransactionTypes`/`operationLedgerRollbackTypes` → `operationCumulativeSpecs[OperationSportsbookBet].TransactionTypes`/`.ReversalTypes` (`internal/risk/cumulative.go`) |
| ADR 0038 §15 | Status: the `player_locked` origin split is **IMPLEMENTED** by migration 0048 |
| `financial-transaction-flows.md` Flow 10 | "Accounts (void after settlement)" → the §8.1 composite above; Flow 9 idempotency line gains the in-house key |
| ADR 0019 actor matrix | New row: "**In-house sportsbook engine (mock mode)** — `sportsbook_bet` from an authenticated player session via `PlaceBet` (stake debit only). `sportsbook_settlement`, `sportsbook_void`, `sportsbook_rollback` and sportsbook `tombstone`: **non-production only**, originated solely by a tenant-scoped staff principal holding `sportsbook_settlement:simulate` through the test-support route (ADR 0088 §9); never a player, never platform-admin. In production no originator exists until a real-provider stage uses the verified-provider-callback row." |
| ADR 0083 §6.1.2 | Status note: "Superseded in part by ADR 0088 §6.1: `ReversalTypes = ["sportsbook_void"]` from migration 0091; INV-SB-CUM-1 satisfied in that commit." §6.2.2: "`'open'` is no longer the only status written." §6.2.5: rollback re-opening residual (ADR 0088 §5.4) |
| ADR 0034 §14.7 | Dependency note: the `VOID_ON_SELF_EXCLUSION` consumer, when authorized, reuses ADR 0088's void operation (both timing variants), adds `player_self_exclusion` to `void_reason` by its own migration, and needs its own ADR 0019 actor row (platform/compliance-initiated) and the ADR 0038 §8.1 "always provider/event-initiated" amendment; it never uses the test-support route |
| ADR 0047 §5(c) | Unaffected: settlement never reads or locks `sb_selections` (INV-SB-SETTLE-6) |
| ADR 0082 | Amendment A4 (§5.5) |
| `ledger-accounting-model.md` | OB-1 cross-reference: ADR 0088 §2.5 builds the correct posting; OB-1 remains OPEN |

## 14. Test obligations (for `qa`'s W1 plan; proposal §R is binding)

Additions specific to this contract, each a named test: delayed
`settle(1)` after `rollback(1)`+`settle(2)` ⇒ replayed; rollback
redelivery after a composed void ⇒ replayed; tombstone → late `settle(g)`
rejected → `settle(g+1)` succeeds; `GENERATION_OUT_OF_SEQUENCE` (gap,
stale); void-after-settlement won/lost end balances (§2.3); the lost
variant racing `PlaceBet` on one wallet (blocker test; must fail under
per-`Post` locking); T-1 rejects `payout_amount ≠ potential_return` even
as the owner; T-2 rejects disallowed transitions and history-inconsistent
status; deny triggers; each §12 refusal **with FORCE RLS active**; §4.7
backstop by fault injection; route absent (flag off; production config);
403 for player/platform-admin/permissionless staff; cross-tenant id ⇒ 404
and no rows; negative `player_cash` after won rollback posts (OB-1);
drift injection per mismatch kind; INV-SB-SETTLE-6 static check.

## 15. Open items for reviewers (not decided by this draft)

- **OI-1 (`architect`, `risk`)** — accept §5.4: rollback may raise
  exposure above an armed ceiling irrespective of locking; no L0.6.
- **OI-2 (`ledger-finance`, `qa`)** — proposal W1 item 7(a)'s
  "`player_locked_bonus` == 0" is read here as scoped to sportsbook
  transaction types (§8.1), since casino bonus bets hold
  `player_locked_bonus` legitimately.
- **OI-3 (`devops`, `security`)** — runtime-role INSERT/SELECT-only on the
  new table: the migration REVOKE is new precedent and is re-granted by
  `init-app-role.sql`'s backfill; decide whether the init script
  re-applies the REVOKE or the triggers alone are accepted as the control.
- **OI-4 (`architect`)** — confirm INV-LOCK-E3 is satisfied by the L1-first
  settlement path and that E-3 (in `PlaceBet`) may remain open.
- **OI-5 (`risk`)** — window-boundary netting under-count (§6.2),
  inherited from `casino_rollback`.
- **OI-6 (`security`)** — grantee of `sportsbook_settlement:simulate`
  (proposed `RoleFinance`); the client-IP choice (§10).

## Consequences

W1 becomes mechanical against §2–§12 once this ADR is ACCEPTED: three
transaction types, one ledger function (`LockProjectionsForPostings`),
one table, two triggers, one reconciliation stream, one test-support
route, one permission, one middleware. The settlement driver stays MOCK;
provider settlement and webhooks stay deferred with `security`'s recorded
requirements (proposal §9.1). OB-1 remains OPEN.
