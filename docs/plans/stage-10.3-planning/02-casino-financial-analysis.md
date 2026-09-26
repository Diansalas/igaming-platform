# Stage 10.3 planning — 02 Casino financial readiness (ledger-finance)

- **Type:** design analysis only. No code, migration, test, commit, `deploy/`, AWS or terraform action.
- **Author:** `ledger-finance` specialist. Casino-domain behaviour was established by reading `internal/casino`, not by the `casino` specialist; `casino` co-owns every item below and must confirm the domain parts.
- **Repository state read:** HEAD `957a3e8`. Line references are to that commit.
- **Evidence standard:** every claim about current behaviour comes from reading the code at the cited lines. **No test was executed for this analysis.** One finding (§3 G-1, multi-bet cash rounds) is from code reading only and must be pinned by a characterization test before the fix is written.
- **Migration numbers:** written as `M-CAS-n` because sibling 10.3 analyses may claim numbers too. The orchestrator assigns the real numbers (next free number at HEAD is `0094`).
- **Nothing here is a decision.** Where a human decision is actually needed, it is named. Everything else is an ordinary, reversible engineering call inside `ledger-finance`/`casino` authority.

Sources read: `CLAUDE.md`; `docs/plans/stage-10.2-planning/10-review-ledger-finance.md` §5; `docs/governance/task-registry.md:3828` (CAS-CAP-ROLLBACK-1); ADR 0019 (Stage 10.2 amendment), ADR 0022 §3 (as amended, "status governs routing only"), ADR 0025 (§4, §6, Stage 10.2 amendment), ADR 0082 §2.1 (lock classes), ADR 0088 §8 (sportsbook reconciliation), `docs/architecture/reconciliation-model.md` §1, §2.3, §5; `docs/architecture/08-casino-integration-architecture.md` §6, §7, §16.4a, §16.5a; `internal/casino/{orchestrator.go,capability.go,bonus_settlement.go,types.go,mock.go}`; `internal/httpserver/{casino_handlers.go,casino_admin_handlers.go}`; `internal/reconciliation/*`; `internal/sportsbook/mock.go`; migrations `0021`, `0027`, `0031`, `0035`, `0080`, `0082`, `0091`, `0092`; `docs/api/openapi/platform-api.yaml:2692`.

---

## 0. Current behaviour, verified at `957a3e8`

| # | Fact | Where |
|---|---|---|
| F1 | After signature verification, `ReceiveCallback` loads the **tenant-wide** capability (`brandID = uuid.Nil`) and returns `ErrProviderUnavailable` (503) if it is missing or `status != active`. This happens before any dispatch, for **every** event type. | `orchestrator.go:662-668` |
| F2 | A per-event flag gate follows: bet needs `supports_bet`, win needs `supports_win`, rollback needs `supports_rollback`. Otherwise 503. | `orchestrator.go:670-686` |
| F3 | So a verified win for a posted bet, a verified rollback of a posted bet, and a verified rollback of an **unseen** original all return 503 and write nothing while the capability is disabled. The last case writes **no tombstone**. | F1+F2; `postRollback` `:1357`; `postRollbackTombstone` `:1560` |
| F4 | `LoadCapability` resolves most-specific-row-wins by `ORDER BY (brand_id IS NULL) ASC LIMIT 1`. With `brandID = uuid.Nil`, no brand row can ever match, so a tenant that has only brand rows gets 503 on every callback. `LaunchGame` resolves with the real brand (`:284`), so launch and callback disagree. | `capability.go:18-37`; `orchestrator.go:284, 662` |
| F5 | `casino_provider_capabilities.supports_*` all default `false`. No CHECK ties `supports_bet` to `supports_win`/`supports_rollback`. `validateNarrowing` only stops tenant config from widening what the adapter declares, and the adapter itself may declare bet without rollback (`internal/providers/casino_adapter_composition_demo_test.go:76-78` does exactly that). | `0035…up.sql:103-120`; `capability.go:176-205` |
| F6 | The admin API writes only tenant-wide rows (`brandID = nil`). Its audit metadata records `provider_id`, `status` and `priority`, but not the before/after `supports_*` flags. | `casino_admin_handlers.go:260-273` |
| F7 | Tombstone: `ledger.TxTombstone`, idempotency key `tombstone:<provider>:<original_provider_tx_id>`, `provider_tx_id = original`, **random** `CorrelationID` (`uuid.New()`). Payments uses the same key format (`internal/payments/orchestrator.go:1295`). | `orchestrator.go:1560-1574` |
| F8 | A late original arriving after its tombstone is rejected only by the `(tenant_id, provider_id, provider_tx_id)` unique index (`0021…up.sql:44-46`). It surfaces as an untyped error, so the HTTP response is **500**, which invites endless provider retries. It also runs RG/Risk evaluation (with audit side effects), takes L0.2 and runs `BindProviderRound` before failing. The financial outcome is correct (nothing posts); the error class is not. This is pinned but deliberately left unchanged by `failure_mode_matrix_integration_test.go:679-700`. | `postBet` `:864-1200`; `ledger.go:300-303` |
| F9 | `resolveWinOrigin` → `classifyOriginRows` returns `ErrAmbiguousMultiOriginRound` whenever more than one un-reversed `casino_bet` shares a correlation id. That covers the **plain cash** path too (`directRows`), not only locked/bonus origins. The HTTP handler does not map this error, so the result is **500**. `postWinDirectCash` never uses `BetTransactionID`, so for cash the ambiguity has no financial basis. | `bonus_settlement.go:121-141, 208-219`; `casino_handlers.go:343-430` |
| F10 | Casino has no loss or round-close callback: a losing round is a bet that is never followed by anything (08 §16.5a, LF-4). `validateCallbackEvent` rejects `amount <= 0`, so a zero-win round-close cannot be sent either. | `orchestrator.go` `validateCallbackEvent`; 08 §16.5a |
| F11 | `internal/reconciliation` has two streams, `ledger_vs_projection` and `sportsbook_settlement`, plus a leaf `statement` package and an hourly sweep (`RunSweep`). There is no casino stream. `ResolveMismatch` exists but has **no** HTTP caller, and no four-eyes manual-adjustment API exists anywhere (`grep manual_adjustment` finds only ledger validation). | `reconciliation.go:236-257`; `scheduler.go` |
| F12 | Verified-but-rejected casino callbacks leave no durable record. The posting transaction rolls back and only a log line remains. | `casino_handlers.go:343-425` |

---

## 1. Item 1 — CAS-CAP-ROLLBACK-1: a hard casino capability contract

### 1.1 Why it exists

- **Source:** `task-registry.md:3828`, scope (a)–(f); `10-review-ledger-finance.md` §5 and condition 2; ADR 0025 Stage 10.2 amendment ("Capability check"); ADR 0022 §3 as amended ("`ProviderCapability.status` governs routing only. Callback acceptance is revoked by revoking the tenant's credential, so in-flight funds are not stranded"); CLAUDE.md ("A rollback for a transaction never seen writes a tombstone so a late-arriving original is rejected").
- **The defect:** the capability is a kill switch on **settlement** as well as on exposure. Disabling it strands the stake of a posted bet (rollback 503), withholds winnings (win 503), and breaks the late-arrival guarantee (no tombstone). Nothing is written, so debits still equal credits. The damage is economic and invisible to the ledger.

### 1.2 Blocks real-provider integration?

**Yes.** It is a registered hard pre-condition for wiring any real casino resolver or going live with a real aggregator (`task-registry.md:3828`, `stage-10.2-completion-report.md` §11). Items 1.3–1.6 below discharge registry scope (a), (b), (c), (d) and (f). Scope (e) is Item 2.

### 1.3 The contract

**Principle (ledger-finance ruling, already given):** capability and status gate **new exposure** only. **Settlement of existing exposure is never blocked** by capability, status, flag, or a missing row. **A verified rollback of an unseen original always writes its tombstone.**

The guarantee applies to **verified** callbacks. Anything rejected before verification (unknown tenant, unregistered adapter, nil resolver, revoked credential, bad signature) stays a uniform 401 with zero writes (strict I1, ADR 0022 §3 point 9). Security takes priority over this guarantee. The operational consequences are in §1.10.

**Capability states** (resolved for the bet's own brand, see §1.4):

- `S-none`: no row for the tenant/brand/provider.
- `S-off`: row present, `status = disabled`.
- `S-nobet`: `status = active`, `supports_bet = false`.
- `S-on`: `status = active`, `supports_bet = true` (implies `supports_win` and `supports_rollback` by the new CHECK, §1.5).
- Additionally, for bets: the session's `asset_code` must be in `supported_assets` (the capability may have narrowed since launch).

**Rules per event type × capability state:**

| Event | S-none | S-off | S-nobet | S-on | Response on rejection |
|---|---|---|---|---|---|
| E1 bet, new `provider_tx_id` | reject, no write | reject | reject | normal Flow 5 (RG, Risk, balance, bind, post) | `ErrProviderUnavailable` → 503 (unchanged status code; see note A) |
| E2 bet replay of a **posted** `provider_tx_id` | original result | original result | original result | original result | F-7 comparison still applies: 409 on payload mismatch |
| E3 bet whose `provider_tx_id` is **tombstoned** | reject | reject | reject | reject | new `ErrOriginalTombstoned`; no RG/Risk/bind; see note B |
| E4 win on a round with an un-reversed posted bet | **post** | **post** | **post** | **post** | existing guards unchanged (`ErrBetNotFound`, LF-18, G-2 holds) |
| E5 win on a round with no bet / only reversed bets | `ErrBetNotFound` 400 + integrity alert | same | same | same | unchanged |
| E6 win replay | idempotent | idempotent | idempotent | idempotent | F-7 409 on mismatch |
| E7 rollback of a posted bet or win | **post reversal** | **post reversal** | **post reversal** | **post reversal** | unchanged (`ErrAlreadyRolledBack` 409 for a second distinct reference) |
| E8 rollback of an **unseen** original | **tombstone** | **tombstone** | **tombstone** | **tombstone** | 200 `tombstoned: true` |
| E9 rollback replay, or a second reference to a tombstoned original | idempotent tombstone result | same | same | same | unchanged |
| E10 win whose `provider_tx_id` is tombstoned (a rollback of the win arrived first) | reject | reject | reject | reject | `ErrOriginalTombstoned` → 409 |

- **Note A (bet rejection shape):** financially, 503 and a definitive `declined` are equivalent, since neither posts. `casino` owns the choice. Keeping 503 means no API change for bets. A provider that retries after re-enable is safe because of E2/E3. A provider that cancels sends a rollback, which hits E8 (tombstone).
- **Note B (E3 response):** the ledger-finance requirements are: no posting, deterministic, not retryable, and durably recorded. Recommended: `ReceiveCallbackResult{Outcome: declined, DeclineReason: "original_rolled_back"}` with a `casino_bet.rejected_tombstoned` audit row in the same (committing) transaction. "Declined" is literally true here (the stake was not taken), and the audit row commits. If `casino` prefers 409, the durable record must come from Item 2's rejection record instead.
- **What `supports_win`/`supports_rollback` now mean:** they are no longer runtime gates. They become configuration assertions ("this provider relationship can settle"), required for `supports_bet` by the CHECK. Keeping the columns avoids a destructive schema change.
- **Removing the tenant-wide pre-dispatch check (F1)** also removes F4 for win and rollback, because those events no longer read the capability at all.

### 1.4 Implementation approach (`casino` code, `ledger-finance` sign-off)

1. **`ReceiveCallback`** (`:655-686`): delete the pre-dispatch `LoadCapability`/status check and the win/rollback flag checks. Dispatch directly after verification. Verification order (a)–(c) and I1 are untouched.
2. **`postBet`**, in this order:
   1. L0.1 delivery lock (`:914`).
   2. Idempotency short-circuit (`:938`), i.e. E2.
   3. **New:** tombstone check (E3): `SELECT 1 FROM ledger_transactions WHERE tenant_id=$1 AND provider_id=$2 AND provider_tx_id=$3 AND transaction_type='tombstone'`. A plain SELECT is enough because the L0.1 lock plus the unique index already serialize it.
   4. Session resolution and validation (unchanged).
   5. **New:** capability gate `LoadCapability(ctx, tx, tenantID, session.BrandID, providerID)`, requiring `found && status=active && supports_bet && asset ∈ supported_assets`. This fixes F4 by resolving the capability the same way `LaunchGame` does. It is a plain SELECT (no lock class), positioned **before** L0.2 (`:989`), so a rejected bet never takes the player lock. The ADR 0082 order is unchanged.
   6. The remainder is unchanged.
3. **`postWin`:** add the E10 check (tombstone on the win's own `provider_tx_id`) before the L2 round lock. Everything else is unchanged.
4. **`postRollback`:** before the `FOR UPDATE` lookup, take the L0.1 advisory lock keyed on the **original** reference (`casino_bet_delivery:<tenant>:<provider>:<original_provider_tx_id>`, the same key `postBet` uses). A concurrent late original and its rollback then serialize deterministically: either the bet posts first and the rollback reverses it, or the tombstone is written first and the bet is rejected (E3). Today the loser of that race gets an untyped unique-violation error and a 500, which only resolves on provider retry. L0.1 is taken before L2/L0.3, so class order is preserved. **This needs an ADR 0082 amendment:** L0.1 is now also taken by `postRollback`, still exactly once per transaction.
5. **Tombstone correlation (F7):** use `roundCorrelationID(tenant, provider, event.RoundID)` when `RoundID` is present, and a deterministic v5 UUID of the tombstone key otherwise. This is replay-safe because tombstones are exempt from correlation comparison (`replay.go:84-115`). Existing rows are untouched. Purpose: Item 2 can join tombstones to rounds.
6. **`WriteCapability`:** reject `cfg.SupportsBet && !(cfg.SupportsWin && cfg.SupportsRollback)` with new `ErrCapabilitySettlementIncomplete` (admin 400). Add a conformance-suite case: an adapter declaring `SupportsBet` must declare `SupportsWin` and `SupportsRollback`. Fix the demo fixture at `casino_adapter_composition_demo_test.go:76-78`.
7. **Launch coherence (small):** a `mode=real` launch should also require `supports_bet`. Otherwise a player can launch a game in which every bet 503s. This is a `casino` call; it has no ledger effect.
8. **Audit (F6):** the capability write audit records before/after of every `supports_*`, `status` and `supported_assets`. CLAUDE.md requires before/after state, and this row now controls new exposure.

### 1.5 DB impact and migrations

**M-CAS-1 (up):** capability settlement invariant.
```sql
DO $$
BEGIN
    ALTER TABLE casino_provider_capabilities
        ADD CONSTRAINT casino_provider_capabilities_bet_requires_settlement
        CHECK (NOT supports_bet OR (supports_win AND supports_rollback));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'M-CAS-1: a casino_provider_capabilities row has supports_bet = true without supports_win AND supports_rollback. Remediate through the audited capability admin API (narrow supports_bet, or enable win+rollback where the adapter declares them), then re-run.';
END $$;
```
- **Pre-flight:** the validating `ADD CONSTRAINT` **is** the pre-flight. It scans every row regardless of RLS. `casino_provider_capabilities` carries `FORCE ROW LEVEL SECURITY`, so a `SELECT count(*)` pre-check would see zero rows as a non-bypass role and let a violating database through. This is the technique used by migrations 0091 (down) and 0092.
- **No automatic data fix.** Flipping `supports_bet` inside a migration would be an unaudited configuration change with no actor. Refusal is correct. Expected violators: none in a fresh database. Dev databases may carry test rows. Staging is being rebuilt from scratch, and the refresh re-runs the chain.
- **Lock:** `ACCESS EXCLUSIVE` briefly; the table is tiny.
- **Down:** `ALTER TABLE casino_provider_capabilities DROP CONSTRAINT IF EXISTS casino_provider_capabilities_bet_requires_settlement;`. This is always safe, because it only relaxes the constraint.
- **No ledger schema change** for Item 1. The E3/E10 checks and the tombstone correlation are code only.

### 1.6 API/OpenAPI impact

`docs/api/openapi/platform-api.yaml:2692` (casino webhook description and responses) changes as follows, and `internal/httpserver/openapi_casinowebhook_contract_test.go` follows:
- 503 cause (2) becomes "a **new bet** while the tenant/brand capability is not active, does not support bet, or no longer supports the session asset". It is never returned for win or rollback.
- E3: 200 `declined` / `decline_reason=original_rolled_back` (or 409 if `casino` chooses so).
- E10: 409 (generic body).
- A late original no longer produces a 500.

Capability admin write: a new 400 for bet-without-settlement. There are no new endpoints.

### 1.7 Security impact (needs `security` review)

- **I1 is preserved.** The only capability read stays post-verification. An unverified caller still never observes capability state.
- **Loss of a control:** `status = disabled` no longer stops **settlement** callbacks. A compromised provider key could, while the capability is disabled, post forged wins against existing rounds. Such wins are payable only to the round's own bettor wallet (derived from the ledger, `postWin`), but **the amount is not bounded**. The emergency stop becomes **credential revocation** (ADR 0022 §3 as amended). A real casino resolver with revocation is `NOT IMPLEMENTED`. Until it exists, the only stop is the ADR 0085 resolver gate, which applies to the whole deployment and currently leaves MOCK as the only resolver.
- `security` should confirm the runbook: "disable capability = stop new bets; revoke credential = stop all callbacks, strands exposure, triggers mandatory reconciliation".
- **Optional, and not recommended by ledger-finance:** a separate, four-eyes, audited "settlement freeze". If `security` wants it, it must carry a mandatory reconciliation follow-up, because it re-creates the stranding problem.
- The new audit before/after (step 8) closes a CLAUDE.md audit gap.

### 1.8 Financial impact and invariants

- **Debits = credits:** unchanged. Every posting still goes through `ledger.Post` with its deferred balance trigger. The new paths add **no** posting shapes: E4/E7 already exist, and E8 is the existing zero-entry tombstone.
- **Idempotency:** unchanged keys. E2 now returns the original result even while disabled. That is correct idempotency; previously a replay got 503.
- **No balance UPDATE:** no new write paths. Projections change only via the ledger trigger.
- **Compensating entries only:** a rollback is still a new transaction with `reverses_transaction_id`.
- **Tombstone guarantee restored** for every verified rollback (E8).
- **Economic effect:** stranded stakes and withheld wins can no longer be caused by configuration.

### 1.9 RLS impact

None. `LoadCapability` still runs under `WithTenant` with no player setting, which satisfies the table's `tenant_isolation` policy and its player-exclusion guard. CHECK validation is not subject to RLS (that is intended). Integration suites must run as the NOBYPASSRLS runtime role (the Stage 10.2 condition 3 precedent).

### 1.10 Failure modes

| Failure | Behaviour after fix |
|---|---|
| Capability disabled mid-round | New bets are rejected. The win and rollback of already-posted bets settle. Unseen-original rollbacks tombstone. |
| Brand-only capability rows | Bets resolve the brand row. Win and rollback do not read the capability. F4 is closed. |
| Adapter removed from the registry, credential revoked, or tenant suspended | Pre-verification 401 for **all** events, so exposure is stranded. This is **by design** (security > settlement). Runbook: never deregister an adapter or revoke a credential with open exposure unless it is an incident; Item 2 detects the result. Tenant-suspension settlement semantics are an `architect` question (§5). |
| Late original racing its rollback | Serialized by L0.1 on the original reference, so the outcome is deterministic. |
| Late original after the tombstone | E3/E10 give a named rejection with no RG/Risk side effects. |
| Migration applied to a database with a violating row | Refuses with a clear message; nothing changes. |
| Concurrent `WriteCapability` disabling while a bet is in flight | The bet reads the capability once without a lock. The last committed state before its read wins. This is acceptable: a bet that read `active` is a legitimate exposure that settles normally afterwards. |

### 1.11 Test requirements (CLAUDE.md financial list; integration tests as the NOBYPASSRLS role)

- **Normal:** bet/win/rollback with an active capability, unchanged. Win and rollback while S-none/S-off/S-nobet post correctly. The balance returns to its expected value, and debits = credits per asset.
- **Duplicates / idempotency:** a bet replay while disabled returns the original id (E2). A win or rollback replay while disabled is idempotent. A rollback replay against a tombstone returns the same tombstone.
- **Concurrency:** (i) a late original and its rollback, concurrently, 50 iterations: exactly one of {bet+reversal, tombstone+rejected bet}, never both, never a 500. (ii) Two distinct rollbacks of an unseen original: one tombstone. (iii) A capability disabled concurrently with a bet: no partial state.
- **Retries:** a bet 503 while disabled, then re-enable, then retry: posts once. A bet 503, then a provider rollback: tombstone, then a retried bet is rejected (E3).
- **Partial failure:** failure injected after the tombstone insert and before the audit: the whole transaction rolls back and the redelivery produces exactly one tombstone.
- **Rollback:** with a prior original (bet and win, disabled and enabled), and without one (tombstone in all four states).
- **Settlement:** a win on a disabled capability pays the bettor's wallet; LF-18 and G-2 guards are still enforced.
- **Reconciliation:** the `ledger_vs_projection` sweep is clean after every scenario. Item 2 streams are clean too.
- **Provider callbacks:** HTTP-level tests with the MOCK credential for each row of the §1.3 table. **Flip** `TestCasinoWebhook_CAS_CAP_ROLLBACK_1_DisabledCapabilityBlocksRollbackOfAlreadyPostedBet` (`casino_webhook_tenant_binding_test.go:469`) from a characterization to a requirement. **Rewrite** `TestReceiveCallback_DisabledCapabilityBlocksCallback` (`orchestrator_integration_test.go:1369`) to assert bets only.
- **Authorization:** cross-tenant rollback is still 401 with zero tombstones in both tenants (existing E4 test). A brand-B capability does not authorize brand-A bets.
- **Auditability:** the E3 audit row exists. Capability writes record before/after.
- **Migration:** up refuses on a seeded violating row (scratch DB, owner role) and applies on a clean DB. Down drops the constraint. Up/down/up round trip.
- **Conformance:** an adapter declaring bet without rollback fails the suite.

### 1.12 Rollback strategy

The code change is a revert of one PR. Revert restores the old (stricter, stranding) behaviour, and no data depends on the new code: tombstones written under the new code are ordinary tombstones that the old code also honours. A deterministic tombstone correlation is compatible with the old code. M-CAS-1 down drops the CHECK. The order is: revert the code first, then down-migrate if needed; either order is safe.

### 1.13 Ownership

`casino` (implementation), `ledger-finance` (the financial contract, E3/E8/E10, lock amendment, final sign-off), `security` (loss of the settlement kill switch, runbook), `qa` (test gate), `architect` (ADR 0025 and ADR 0082 amendments, reconciling with ADR 0022 §3). `code-reviewer` independent review.

### 1.14 Human decision required?

**No.** The ledger-finance ruling is recorded (`10-review-ledger-finance.md` §5). Everything else is reversible engineering inside an approved follow-up. Only the **stage authorization** itself is a human act.

---

## 2. Item 2 — Casino reconciliation

### 2.1 Why it exists

- **Source:** `task-registry.md:3828` scope (e) (part of the hard pre-condition); `reconciliation-model.md` §2.3 (`BLUEPRINT`: key `(provider_id, provider_tx_id)`, expected state "house_gaming net movement for that provider/period matches the provider's reported GGR", any non-zero difference is a mismatch); CLAUDE.md ("reconciliation-capable", "Any non-zero drift is a P1 incident", "daily reconciliation against the ledger" for every integration); ADR 0004.
- **Today:** a provider/platform divergence, a missing settlement, or a rejected-then-forgotten callback is invisible (F11, F12).

### 2.2 Blocks real-provider integration?

- **Internal consistency stream + statement interface + MOCK source:** **yes**. They are part of the registered hard pre-condition for wiring a real resolver.
- **Statement matching against a real provider's statement:** `PROVIDER DEPENDENT`. It blocks **real-money go-live** with that provider, **not** sandbox integration. It cannot be built until a contracted provider's statement format exists.
- **A human/four-eyes compensation mechanism (§2.9):** it blocks real-money go-live, not integration. It is not casino-specific.

### 2.3 The casino reconciliation model (text)

Two streams on the existing `reconciliation_runs`/`reconciliation_mismatches` tables (no new run/mismatch tables), following the `sportsbook_settlement` pattern exactly:

1. **`casino_consistency`**: platform-internal, zero tolerance, hourly, no counterparty. It proves that the platform's own casino records agree with each other and with the ledger, and it turns durable evidence of a provider-asserted event that the ledger lacks into a finding.
2. **`casino_statement`**: counterparty match. It matches the ledger against a provider statement obtained through a provider-neutral `statement.CasinoStatementSource`. Today the only source is `casino.MockStatementSource` (**MOCK**, label contains "MOCK", tautological by construction exactly like `sportsbook.MockSettlementStatementSource`). It proves the matching path; tests inject divergent statements to prove detection.

Both streams:
- run inside `db.Pool.WithTenant(tenant)`;
- take a stream-specific `pg_try_advisory_xact_lock(hashtextextended('reconciliation:<stream>:' || tenant, 0))`, so two sweeps of the same tenant and stream serialize and the loser records `skipped`;
- **only ever insert** one immutable `reconciliation_runs` row (0082 deny trigger) plus zero or more `reconciliation_mismatches` rows (evidence columns immutable per 0082);
- never write ledger, projection, round, session or capability data;
- record every attempt, including a lock-skipped or failed one, to `audit_log`;
- log a mismatch at `Error` level ("MISMATCH FOUND", P1) with the statement-source label;
- are started by `RunSweep` after `sportsbook_settlement`, each in its own transaction, so one stream's failure never discards another's evidence;
- skip their checks for a tenant with no casino footprint (a cost guard, not a tolerance, the `sbHasFootprint` precedent);
- sum amounts as text into `big.Int`, never `int64` (ADR 0088 §3.6).

**Severity rule.** A mismatch row is a P1 by definition, so the stream emits a mismatch only for conditions that are **always** wrong. A condition that is often legitimate (an ageing cash round) is a **metric** in the run's audit metadata, not a mismatch. That keeps the zero-tolerance rule honest and avoids permanent P1 noise.

### 2.4 `casino_consistency` checks

All casino tombstones are identified by `transaction_type = 'tombstone' AND provider_id IN (casino provider ids of this tenant)`, derived from `casino_provider_capabilities ∪ casino_provider_rounds ∪ casino_* ledger rows`. The key format alone cannot distinguish casino from payments (F7; see §3 G-4).

| Check | Invariant (must always hold) | Mismatch kind |
|---|---|---|
| C1 round binding | Every `casino_bet` has a `casino_provider_rounds` row with the same `(tenant, provider, correlation_id)`, and every round row has ≥ 1 `casino_bet` under its correlation id. The player owning the bet's debited wallet equals the round's `player_account_id`. `postBet` writes both in one transaction, so any gap means a bypassing writer. | `cas_round_binding_mismatch` |
| C2 posting shape | `casino_bet`: player-side debits on one wallet and asset, the credit on `house_gaming` (plus Rule B2 legs where bonus-set). `casino_win`: the credited wallet equals the wallet of the round's bet(s). Every casino transaction has non-null `provider_id`/`provider_tx_id`. | `cas_posting_shape_mismatch` |
| C3 win orphan / order | Every `casino_win` correlation has a `casino_bet`. No `casino_win` was posted after the round's only bet(s) were reversed. | `cas_orphan_win` |
| C4 rollback linkage | Every `casino_rollback` reverses a `casino_bet`/`casino_win` with the same `provider_id`. Its entries are the exact inverse multiset of the original's caller legs. At most one `casino_rollback` per original. | `cas_rollback_linkage_mismatch` |
| C5 tombstone backstop | No non-tombstone ledger transaction shares `(tenant, provider_id, provider_tx_id)` with a casino tombstone. This is structurally impossible under the unique index, so it is a backstop against an index drop or a bypass. | `cas_tombstone_conflict` |
| C6 unposted provider-asserted event | For every row of the rejection record (§2.6) with reason in {`bet_not_found`, `ambiguous_round`, `original_tombstoned`, `lock_already_released`, `internal_error`} and **no** later successful ledger posting under the same `(provider_id, provider_tx_id)`: a provider asserted a financial event that the ledger does not hold. **Evidence-type:** recorded **once per key** (deduplicated against existing mismatch rows of this stream and kind, any status), so it is not re-raised hourly. | `cas_unposted_provider_event` |
| C7 tombstone later matched by an original | Recorded when the rejection record shows an `original_tombstoned` rejection for a tombstone's reference. On the platform side the net is zero (correct). It becomes a real divergence only if the provider still counts the original, which only the statement can tell. Evidence-type, recorded once per tombstone. The statement stream can confirm it; a human resolves it. | `cas_tombstone_late_original` |

**Metrics, not mismatches** (in `audit_log` metadata of each run):
- `unresolved_cash_rounds_older_than_window`, i.e. cash bets with no win and no rollback older than W. Loss-by-silence (F10) makes this normal, so it is **not** a mismatch.
- `tombstones_total`, `rejections_open`.

**Locked-origin ageing** (a bonus-funded casino bet whose stake sits in `player_locked_*` past W) would be a real stranded-exposure mismatch (`cas_locked_unresolved`). It stays **dormant**: bonus-funded casino bets are not implemented (`postBet` is cash-only), and W is an unmade per-jurisdiction human decision (08 §16.5a). Absent configuration, no mismatch is raised; that is the established "absent config" rule. The check activates in the same change that ships bonus-funded casino bets.

### 2.5 `casino_statement` stream

**Interface** (in the dependency-free leaf `internal/reconciliation/statement`, so `reconciliation` never imports `casino`; the same import-cycle reason as `statement.go:1-8`):
```go
type CasinoStatementLine struct {
    ProviderID           string
    ProviderTxID         string
    Kind                 string  // "bet" | "win" | "rollback"
    OriginalProviderTxID string  // rollback only
    RoundID              string
    AssetCode            string
    Amount               int64   // minor units; summed as big.Int
}
type CasinoStatementTotal struct { ProviderID, AssetCode string; GGR *big.Int } // optional per-period aggregate
type CasinoStatementSource interface {
    Label() string // must contain "MOCK" for a mock
    Statement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time) ([]CasinoStatementLine, []CasinoStatementTotal, error) // read-only
}
```
- **Key match:** by `(provider_id, provider_tx_id)`: present on both sides, same kind, amount, asset and round, and for a rollback the same original. The ledger view comes from `casino_bet`/`casino_win`/`casino_rollback` rows. The amount is the player-side leg sum for bets and wins and the original's amount for rollbacks. Tombstones are **not** statement lines; a provider lists its rollback, and the ledger side shows "rollback of unseen original" as a tombstone. That pairing is a match, not a mismatch.
- **Totals match:** per `(provider, asset, period)`, the net `house_gaming` movement over casino transaction types with that `provider_id` equals the provider's GGR. `house_gaming` is tenant-level per asset, not per provider, so the filter goes through `ledger_transactions.provider_id`.
- **Period semantics:** the MOCK source is all-time, like sportsbook. For a real source, a key on one side only is a mismatch only if it is also absent from the adjacent period's statement. That is a **timing** window for provider cut-offs, never an **amount** tolerance (the precedent is `reconciliation-model.md` §2.8, the crypto confirmation window). The exact cut-off is `PROVIDER DEPENDENT`.
- **MOCK source:** `casino.MockStatementSource` renders lines from the casino ledger rows themselves. It is tautological on uncorrupted data and labelled MOCK in code, logs, run audit and mismatch `actual_value` (the `sb_mock_statement_mismatch` precedent). A `nil` source fails the run closed.
- **Mismatch kind:** `cas_mock_statement_mismatch` for now. A real kind is added with the first real source.
- **Real statement ingestion** (provider API or file, stored as `casino_provider_statements`/`_lines`, FORCE RLS, append-only, unique `(tenant, provider, statement_ref)`, idempotent ingest, secrets via the secret store): `NOT IMPLEMENTED` and not designed further here. It depends on a contracted format and on the secret-store ADR. `reconciliation-model.md` §2.2 already requires that any **manual** statement upload be four-eyes, reason-coded and audited. That stays an `OPEN DECISION` and must not be added silently.

### 2.6 Rejection record (the durable input for C6/C7; addresses F12)

This is **recommended, not optional, if C6/C7 are wanted**. `product-owner-proxy` should confirm scope. Without it, the internal stream cannot see missed events at all, and only a real statement could.

- **Table** `casino_callback_rejections`: `id`, `tenant_id NOT NULL`, `provider_id`, `event_type CHECK IN ('bet','win','rollback')`, `provider_tx_id NOT NULL`, `original_provider_tx_id`, `round_id`, `asset_code`, `amount BIGINT`, `reason_class CHECK IN (...)`, `first_seen_at`. `UNIQUE (tenant_id, provider_id, provider_tx_id, reason_class)`; inserts use `ON CONFLICT DO NOTHING` (idempotent, bounded).
- **Written** by the webhook handler in a **fresh** `WithTenant` transaction, after the posting transaction rolled back. That is the same pattern as the reconciliation sweep's `sweep_run_failed` audit. It is written **only** for a verified callback whose error is a financial rejection class. It is **never** written for an `AuthError`, so I1 is preserved and an unverified caller cannot create rows. A failure to write it is logged and does not change the provider response.
- It stores no raw body and no signature.

### 2.7 Implementation approach

1. **M-CAS-2:** widen `reconciliation_mismatches_mismatch_kind_check` with the `cas_*` kinds, and create `casino_callback_rejections`. This is the 0091 step-5 technique.
2. `internal/reconciliation/casino_consistency.go`: `RunCasinoConsistency`, `TryRunCasinoConsistencyForTenant`.
3. `statement.CasinoStatementSource` and `internal/reconciliation/casino_statement.go`: `RunCasinoStatement` and its `TryRun…`.
4. `casino.MockStatementSource` in `internal/casino/mock.go`.
5. The handler writes the rejection record (`casino_handlers.go`).
6. `RunSweep`/`RunSchedulerLoop` take the casino source; `cmd/platform-api` injects the MOCK source (it is in-house; the label says MOCK).
7. Update `reconciliation-model.md` status and §2.3; update the registry.

### 2.8 DB impact and migrations

**M-CAS-2 (up):**
- Drop and recreate `reconciliation_mismatches_mismatch_kind_check`, adding `cas_round_binding_mismatch`, `cas_posting_shape_mismatch`, `cas_orphan_win`, `cas_rollback_linkage_mismatch`, `cas_tombstone_conflict`, `cas_unposted_provider_event`, `cas_tombstone_late_original`, `cas_mock_statement_mismatch`. This is purely additive.
- `CREATE TABLE casino_callback_rejections (...)`: `ENABLE` + `FORCE ROW LEVEL SECURITY`; a `tenant_isolation` policy with the player-exclusion guard (the 0035/0028 staff-only shape); the `ledger_deny_mutation` triggers for UPDATE/DELETE and TRUNCATE (append-only evidence); a guarded `REVOKE UPDATE, DELETE, TRUNCATE … FROM igaming_runtime` (the 0091 step 6 pattern); `(tenant_id, provider_id, provider_tx_id)` index.
- **Pre-flight:** none is needed for the additive CHECK widening. Existing rows hold only old kinds, and validation runs anyway.

**M-CAS-2 (down), refusal-based**, as in 0091 down:
1. Recreate the narrower kind CHECK inside `DO … EXCEPTION WHEN check_violation THEN RAISE 'casino reconciliation evidence exists; roll forward'`. Constraint validation, not `count(*)`, because of FORCE RLS.
2. `ALTER TABLE casino_callback_rejections ADD CONSTRAINT tmp_empty CHECK (false)` inside the same kind of guard, so the down refuses when evidence exists; then drop the table.

After the first casino mismatch or rejection row exists, down is refused: **roll forward**.

No change to `reconciliation_runs`; the `stream` column is free TEXT.

### 2.9 Drift handling and compensation

- **Detection only; never auto-correct.** No stream writes to the ledger. "The projection is rebuilt from the ledger, never the reverse" (`reconciliation-model.md` §2.1). A statement divergence is never "fixed" by posting to match the provider.
- **P1:** every mismatch row is logged at `Error` level and audited. Paging and alert routing belong to the observability pipeline; this plan does not add them.
- **Compensation order of preference** (every step human-initiated):
  1. **Provider redelivery** through the normal idempotent callback path, using the provider's own `provider_tx_id`. After Item 1 this is never blocked, so a missing win or rollback is best fixed by the provider re-sending it.
  2. **A provider-issued rollback** for something the platform holds but the provider does not.
  3. **A compensating `manual_adjustment`** (reason code mandatory by the 0021 CHECK), **four-eyes above a configurable threshold** (CLAUDE.md), audited with before/after, and linked through `ResolveMismatch(…, correctionLedgerTransactionID)`.
- **Gap (F11):** step 3 has **no implemented mechanism**. There is no manual-adjustment API, no four-eyes approval flow, and no HTTP route for `ResolveMismatch`. This is platform-wide, not casino-specific. Proposed registry item: **LEDGER-MANUAL-ADJ-4EYES-1** (`ledger-finance` + `security` + `backend`). It blocks real-money go-live, not provider integration, and it is not in this item's scope.
- **Provider-payable/GGR disputes** (`reconciliation-model.md` §2.5) are resolved commercially, outside the ledger. The ledger is touched only after a confirmed resolution, and then only by compensating entries.

### 2.10 API/OpenAPI impact

None required. There are no new public or admin routes; reconciliation results are read through the database/reporting layer, as for the existing streams. A staff mismatch view or resolution API would belong to LEDGER-MANUAL-ADJ-4EYES-1, with its own OpenAPI entries and four-eyes permissions. The webhook response codes are unchanged by the rejection record.

### 2.11 Security impact

- The streams read tenant data only inside `WithTenant`. The one cross-tenant read is the existing `allTenantIDs` listing.
- Mismatch `expected_value`/`actual_value` may contain provider references and amounts. They are tenant-scoped and staff-only; they must never contain PAN, tokens or raw bodies.
- Rejection record: verified-only writes (I1). A key holder could flood it with distinct references, but rows are bounded per `(tx, reason)` and small. Webhook rate limiting is the control. A compromised key is already an incident.
- A real statement source will need per-tenant provider credentials from the secret store (blocked on the secret-store ADR). The MOCK source uses none.
- `security` review is needed for the rejection-record write path and for the P1 signal content (no secrets in logs).

### 2.12 Financial impact and invariants

The streams are read-only with respect to money: **no posting, no projection write, no balance UPDATE**. Debits = credits is independently re-checked per casino transaction by C2/C4, as a backstop to the deferred trigger. Idempotency: run rows are append-only, and a re-run adds a new run without altering earlier evidence. Evidence-type findings are deduplicated per key, and state-type findings are re-detected each run (the existing semantics, ADR 0023 §4). Corrections stay compensating entries only (§2.9).

### 2.13 RLS impact

- `reconciliation_runs`/`_mismatches`: existing tenant-only FORCE RLS; no dual scope (0027 comment).
- The streams read `ledger_transactions`, `ledger_entries`, `ledger_accounts`, `casino_provider_rounds` (tenant staff policy; no player setting in the sweep), `casino_provider_capabilities` and `casino_callback_rejections`, all under the tenant setting.
- Tests must run as NOBYPASSRLS and include a two-tenant case: tenant A's casino drift produces zero rows in tenant B, and tenant B's sweep does not see A's rejections.

### 2.14 Failure modes

| Failure | Handling |
|---|---|
| A stream errors mid-run | Its transaction rolls back (no partial run). `sweep_run_failed` is audited in a fresh transaction. Other streams and tenants are unaffected. |
| Concurrent sweeps | Advisory xact lock; the loser is `skipped` and audited. |
| Statement source is nil or erroring | The run fails closed (never "clean"). |
| MOCK source always clean | This is expected and disclosed. Detection is proven by injected divergence tests. |
| Persistent drift | Evidence-type findings are deduplicated. State-type findings re-raise hourly, which is intentional until resolved (known limitation, ADR 0023 §4). |
| Rejection-record write fails | Logged. The provider response is unchanged. C6 then depends on the statement stream. This is disclosed. |
| Loss-by-silence cash rounds | A metric only, never a P1. |
| Clock and period cut-offs (real source) | A timing window only, no amount tolerance. |

### 2.15 Test requirements

- **Normal:** a clean seeded world (bets, wins, rollbacks, tombstones, multi-tenant) gives a clean run for both streams.
- **Detection:** one test per mismatch kind, injected on a scratch database as owner with triggers bypassed where needed (the ADR 0088 §8.4 precedent). Each asserts exactly one row of the right kind and key.
- **Duplicates / idempotency:** a re-run adds a new clean run. Evidence-type findings are not duplicated across runs. A duplicate statement line is a mismatch (the sportsbook dedup-test precedent).
- **Concurrency:** two concurrent sweeps of the same tenant and stream give one run and one `skipped`. A sweep concurrent with live bet/win/rollback traffic produces no false mismatch, because each check reads a consistent snapshot. A REPEATABLE READ or single-statement design is required: C1/C3 must not see a bet without its round mid-transaction. Ledger and round rows commit atomically, so a single snapshot suffices.
- **Retries / partial failure:** a statement source error mid-run leaves zero rows and an audited failure.
- **Rollback / tombstone:** C5 (backstop), C7 (late original after tombstone), and a statement-listed rollback matching a platform tombstone (no mismatch).
- **Settlement:** a missing win in the ledger versus the statement gives a mismatch. After provider redelivery the next run is clean, and the old mismatch stays as evidence until resolved.
- **Reconciliation:** `ledger_vs_projection` is unaffected.
- **Provider callbacks:** a verified rejection writes one rejection row; an unverified one writes zero (I1); redelivery of the same rejection adds none.
- **Authorization / RLS:** two-tenant isolation; the runtime role cannot UPDATE or DELETE rejections or runs.
- **Auditability:** every attempt is audited with the MOCK label.
- **Migration:** up; down refuses with evidence present; down on a clean DB; round trip.

### 2.16 Rollback strategy

- The stream code can be removed from `RunSweep` by a revert. Reconciliation has no effect on money, so a revert is always safe.
- M-CAS-2 down works only before evidence exists; after that, roll forward (for example, stop invoking the stream but keep the tables).
- The rejection-record write can be reverted independently. Existing rows stay as evidence.

### 2.17 Ownership

`ledger-finance` (stream design, invariants, sign-off), `casino` (rejection classes, MOCK source, handler write), `backend` (sweep wiring), `security` (rejection write path, logs), `qa` (detection matrix), `architect` (the `reconciliation-model.md` update, the M-CAS-2 review). `product-owner-proxy` confirms the rejection record is in scope.

### 2.18 Human decision required?

- For the streams as designed: **no**.
- **Deferred, and genuinely human:**
  - (i) the per-jurisdiction settlement window W. It is needed only when bonus-funded casino bets ship, and is already recorded as open in 08 §16.5a.
  - (ii) the four-eyes **threshold value** for manual adjustments. It is needed for LEDGER-MANUAL-ADJ-4EYES-1, not for detection.
  - (iii) whether operators may **upload** statements manually (`reconciliation-model.md` §2.2 `OPEN DECISION`).
  - (iv) the statement cadence and cut-off per real provider. That is contractual (`PROVIDER DEPENDENT`), not a decision to make now.

---

## 3. Item 3 — Other casino financial-readiness gaps (only the ones actually found)

### G-1 Multi-bet **cash** rounds withhold wins (F9) — **must fix; blocks go-live with a real aggregator**

- **Why:** `classifyOriginRows` treats two or more un-reversed cash bets under one correlation id as `ErrAmbiguousMultiOriginRound` (`bonus_settlement.go:130-132`). The handler does not map this error, so the result is 500. Multi-debit rounds are common in real aggregators: side bets, multi-hand table games, feature buys, re-bets. `TestPostBet_BindsProviderRound_IdempotentAcrossTwoBetsOnSameRound` shows the platform accepts the second bet, so any win on such a round is withheld indefinitely. The economic effect is the same as CAS-CAP-ROLLBACK-1 (b). LF-8 (08 §16.4a) scoped the restriction to **bonus-funded** wagering; its application to cash is unjustified, because `postWinDirectCash` never uses `BetTransactionID`.
- **Evidence:** code reading only. **First write a characterization test** (two cash bets, one win, currently 500 / `ErrAmbiguousMultiOriginRound`).
- **Fix:** in the direct branch of `resolveWinOrigin`, when every row is `player_cash` on one wallet and one asset, resolve to that wallet. Keep `ErrCorrelationWalletCollision` (different wallets), `ErrMixedFundingUnsupported`, and every locked/bonus outcome exactly as they are. Map `ErrAmbiguousMultiOriginRound`, `ErrCorrelationWalletCollision`, `ErrLockAlreadyReleased`, `ErrMixedFundingUnsupported` and `ErrBonusBetNotLocked` to a 409 integrity alert instead of 500. They are not platform failures, and they feed the rejection record.
- **DB:** none. **API:** the 500 becomes a 409 for those classes; the OpenAPI text is updated.
- **Financial:** a win credits `player_cash` of the only wallet involved; the posting shape is unchanged; debits = credits. Rollback of one bet in a multi-bet round stays per-original, and later wins still resolve to the same wallet.
- **Tests:** multi-bet cash win (normal, replay, concurrent wins on one round (the L2 round lock already serializes them), a rollback of one bet then a win, two wallets under one round → 409). **Owner:** `casino` + `ledger-finance`. **Human decision:** none.

### G-2 Late original after tombstone is a 500 (F8) — **folded into Item 1** (E3/E10, L0.1 on rollback). It does not block on its own, but it must ship with Item 1.

### G-3 One reversal per casino original is not enforced by the database

- `postRollback` relies on the L2 `FOR UPDATE` (tested). There is no backstop like `0092`'s `ledger_transactions_one_deposit_reversal`.
- **Recommendation:** fold into the already-deferred **LEDGER-REV-UNIQ** (0092 §U), as a partial unique index `(tenant_id, reverses_transaction_id) WHERE transaction_type = 'casino_rollback'`. The index build itself is the pre-flight (0092 technique; dev databases may hold pre-lock-fix duplicates, and the build refuses on them); the down path drops the index.
- **Not a blocker.** C4 detects violations in the meantime. **Owner:** `ledger-finance`.

### G-4 Provider-id namespace is shared across domains

- Casino and payments tombstones use the identical key `tombstone:<provider>:<ref>`, and all domains share the `(tenant, provider_id, provider_tx_id)` index. If a casino provider id equals a payments provider id in one tenant, references can collide: a false "already taken", or a wrong tombstone match.
- **Recommendation:** an `architect` rule that provider ids are disjoint across domains, enforced at adapter registration. Optionally, a `casino_tombstone:` key prefix for new tombstones. That is safe because `postRollback` finds existing tombstones by provider reference before `Post`.
- **Not a blocker** if the registration rule lands before the first real adapter. **Human decision:** none.

### G-5 Capability audit lacks before/after (F6) — folded into Item 1 step 8.

### G-6 Free rounds, jackpots, bonus-funded casino stakes — **not required now; no blocker invented**

- Bonus-funded casino bets are not implemented (cash-only `postBet`; 08 §16.5a/§16.10 say "NOT SAFE to implement" until W and the lock-release sweep exist).
- Free rounds and jackpot contribution splits are `OPEN DECISION`s in ADR 0025 §6 and are owned by `bonus-engine`.
- **One conformance rule is needed now:** a real adapter must not map a free-round or jackpot payout with no platform bet to `CallbackEventWin`. It would hit `ErrBetNotFound`, and the provider would believe the player was paid. Until a design exists, such payouts must be excluded by the adapter/contract, and the conformance suite asserts the adapter declares no such mapping. This becomes a real blocker **only if** the first aggregator contract includes free rounds or jackpots; then it is `PROVIDER DEPENDENT` and needs a `bonus-engine` + `ledger-finance` design.

### G-7 Stranded exposure on adapter deregistration or credential revocation

This is the operational side of §1.10. Proposal: a pre-launch checklist entry (next to MOCK-ADAPTER-PROD-1) that says never to deregister an adapter or revoke a casino credential while C-stream metrics show unresolved rounds, except as an incident with a mandatory reconciliation. No code is required.

---

## 4. Waves and dependencies

| Wave | Content | Depends on | Blocks real-provider wiring? |
|---|---|---|---|
| **A: capability contract** | Item 1 (§1.4 steps 1–8), M-CAS-1, G-1, G-2 (inside Item 1), ADR 0025 + ADR 0082 (L0.1 in `postRollback`) amendments, OpenAPI, test flips | none (independent of `deploy/` and of the staging teardown) | **Yes** |
| **B: internal reconciliation** | M-CAS-2, rejection record (§2.6), `casino_consistency` C1–C7, sweep wiring | A: deterministic tombstone correlation, named rejection classes (E3/E10, G-1 mappings) | **Yes** (registry scope (e)) |
| **C: statement stream** | `statement.CasinoStatementSource`, `casino.MockStatementSource`, `casino_statement` key and totals match, `reconciliation-model.md` update | B (shared M-CAS-2, sweep plumbing) | Yes for interface + MOCK; a **real** source is `PROVIDER DEPENDENT` and blocks real-money go-live only |
| **D: hardening, may slip** | G-3 (via LEDGER-REV-UNIQ), G-4 registration rule, G-7 checklist, launch-requires-bet | A | No |
| **External dependency** | LEDGER-MANUAL-ADJ-4EYES-1 (compensation mechanism, mismatch resolution API) | none (platform-wide) | No; blocks **real-money go-live** |

Waves B and C can be developed in parallel once M-CAS-2 is agreed. Merge order: A → B → C. Each wave closes with its own `ledger-finance` sign-off. `security` review is needed for A (the kill-switch change) and B (the rejection write path).

## 5. Open questions for other specialists (not human decisions)

- `casino`: E1/E3 response shape (503 versus `declined`; §1.3 notes A and B).
- `architect`: settlement semantics for a **suspended tenant**. Today the preamble rejects every callback, which strands exposure. Should a suspended tenant's verified settlement callbacks still be accepted? This is recorded as a question and not pre-decided. It may need a human answer if it carries licensing weight.
- `security`: whether an explicit, four-eyes "settlement freeze" is wanted despite §1.7.
- `product-owner-proxy`: confirm the rejection record (§2.6) is in scope for 10.3.

## 6. Status labels for this plan's deliverables (after implementation, if authorized)

- Item 1: `IMPLEMENTED — MOCK provider only`. A real resolver remains `NOT IMPLEMENTED`.
- Item 2 internal stream: `IMPLEMENTED`. Statement stream: `MOCK`. Real statement matching: `PROVIDER DEPENDENT`.
- G-1: `IMPLEMENTED`. G-3, G-4: `NOT IMPLEMENTED` (deferred). G-6: `NOT IMPLEMENTED` (conformance rule only). Compensation: `NOT IMPLEMENTED` (LEDGER-MANUAL-ADJ-4EYES-1).
