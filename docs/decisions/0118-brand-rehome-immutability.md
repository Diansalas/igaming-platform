# ADR 0118 - Player brand binding is immutable (BRAND-REHOME-HOLD-1)

- **Status:** ACCEPTED (`architect`, 2026-10-13). **IMPLEMENTED on the implementation branch (2026-10-13,
  `ledger-finance`), NOT MERGED; pending the section 9 reviews** (security LIVE re-run blocking). Implementation record:
  section 12. Evidence is MOCK/local only (no GitHub CI, CI-BILLING-1). The original design text below is unchanged
  except where section 12 records a deviation.
- **Closes (on implementation):** task-registry row BRAND-REHOME-HOLD-1; ADR 0113 section 9 item 10;
  `docs/plans/platform-reconciliation-2026-10-13.md` class-A item 2/3.
- **Decision type:** cross-domain integrity control (`identity`, `wallet`, `ledger`, `launchgov`, `kyc`). Author:
  `architect`, on orchestrator instruction, for the identity domain (the reconciliation plan names identity as owner
  with architect co-sign; the identity-domain sign-off is taken at implementation review, section 9).
- **Builds on, without reopening:** ADR 0012 (Brand distinct from Tenant; PlayerAccount bound to one
  `(tenant_id, brand_id)`), ADR 0007 / `docs/architecture/financial-domain-model.md` "Wallet: scoped to
  PlayerAccount, not to Person", ADR 0110 section 9.4 (owner role outside the threat model), ADR 0112 (brand gate key,
  `GP011`, `LA` family), ADR 0113 (`LA025`, check D, pre-flight pattern), migration 0082 (ARCH-DB-3 composite brand
  pinning).

## 1. Problem (security-confirmed, executed live)

`igaming_runtime` can re-home a player into another brand of the same tenant with one statement:

```sql
WITH pa AS (UPDATE player_accounts SET brand_id = $closed_brand WHERE id = $player RETURNING id)
UPDATE wallets SET brand_id = $closed_brand WHERE player_account_id = $player;
```

The composite FK `wallets_player_account_id_tenant_id_brand_id_fkey` (`(player_account_id, tenant_id, brand_id)
REFERENCES player_accounts (id, tenant_id, brand_id)`, migration 0019) is `NO ACTION`, not deferrable, so it is
checked at the end of the statement, when both sides already agree. RLS does not stop it: both tables' write policies
check `tenant_id` only, and the tenant does not change.

Consequences:

1. A wallet carrying a `player_withdrawal_hold` balance can be moved into a brand already `closed` after `LA025`
   passed (0133 joins `wallets.brand_id` at closure time only), or moved out of an active brand so that brand's `LA025`
   count is zero and its closure passes.
2. Ordinary `player_cash` can be stranded in a closed brand (no gameplay, no deposit, no withdrawal path).
3. Every per-brand control that resolves "the player's brand" at use time is undermined: `GP011` (0129, resolves
   `ledger_entries.wallet_id -> wallets.brand_id` at COMMIT; a player of a suspended brand re-homed to an active brand
   can wager), brand-level financial approval policy (`financial_policy_required_approvals`, 0113/0124: a re-home can
   pick a brand with a lower four-eyes threshold), brand-scoped RG configuration (0135 `rg_limit_configs`), brand
   jurisdiction resolution, and `UNIQUE (brand_id, email)` identity.
4. Snapshot columns copied from the wallet at insert become stale and silently disagree with it
   (`ledger_adjustment_requests.brand_id`, 0113 lines 977-983).

What partially limits it today (verified, section 3): every child table holding a composite FK to
`(id, tenant_id, brand_id)` already pins its own `brand_id`, so a player who has ANY such child row (a withdrawal
request, a deposit intent, a bet, a launch session, a bonus grant, a KYC document, ...) cannot be re-homed: the child
FK fails at end of statement. The attack therefore works on players whose only brand-bound rows are
`player_accounts`, `wallets` and `kyc_verifications`, and whose ledger history comes from postings that leave no
brand-pinned child: ordinary cash from a `manual_adjustment` (ADR 0113 residual 9.5) or other postings without a
pinned child row, and a hold balance that no `withdrawal_requests` row references (`withdrawal_requests` rows are
never deleted, 0131, so a player with any request, even terminal, is NOT re-homable today: `23503`). The exact
fixture security executed is in their slices 2-3 review record; this ADR did not re-execute it. The ordinary-cash
population alone makes this a real hole, not a theoretical one.

### 1.1 Same-class finding found while verifying (NOT executed; analytically derived)

`wallets` and `player_accounts` have NO update trigger at all (verified: `grep "ON wallets"` / `"ON player_accounts"`
over `migrations/*.up.sql` returns only indexes and policies). Therefore, by the same reasoning:

- `wallets.player_account_id` can be re-pointed to another player of the same tenant and brand (FK satisfied), which
  moves that wallet's ledger accounts and balance to another person without any ledger posting. The denormalised
  `ledger_accounts.player_account_id` (immutable since 0082) would then disagree with its wallet, and
  `player_self_scope` on `ledger_accounts` reads that copy.
- `wallets.asset_code` can be changed while its ledger accounts keep their (immutable) `asset_code`.
- `kyc_verifications` has no update trigger (only delete/truncate, 0040), so its `player_account_id` and `brand_id`
  can be re-pointed together to another player's account.

These have not been executed (this ADR made no database mutation). They are the same control in the same migration,
so they are folded in (section 2) rather than tracked separately. Security must confirm them live (section 9).

## 2. Decision

**D1. Re-home is not a supported operation.** A `PlayerAccount` IS a person's relationship with one brand (migration
0010 header, ADR 0012, `docs/architecture/05-identity-architecture.md`); a wallet belongs to exactly one
PlayerAccount (financial-domain-model "Wallet: scoped to PlayerAccount"). A person who plays at a second brand has a
second PlayerAccount with its own wallets. Moving a relationship between brands has no definition in this model.
**A future governed brand transfer (for example brand consolidation or an operator acquisition) needs its own ADR**,
and would be modelled as closing one account and opening another with explicit, auditable ledger transfers under
ledger-finance ownership, never as an UPDATE of `brand_id`. No exception path is created by this ADR.

**D2. Identity columns are immutable in the database.** Migration `0136` adds one `BEFORE UPDATE ... FOR EACH ROW`
trigger per table below, raising a distinct SQLSTATE in a new class `BR` when any listed column changes
(`IS DISTINCT FROM`). The trigger has a `WHEN (...)` clause listing the same columns, so ordinary updates (status,
password, residence, KYC status) never call the function.

| Table | Pinned columns | SQLSTATE | Why this table |
|---|---|---|---|
| `player_accounts` | `id`, `tenant_id`, `brand_id`, `created_at` | `BR001` | the parent of every composite brand FK |
| `wallets` | `id`, `tenant_id`, `brand_id`, `player_account_id`, `asset_code`, `created_at` | `BR002` | the finding; 1.1 |
| `kyc_verifications` | `tenant_id`, `brand_id`, `player_account_id` | `BR003` | the only composite-FK child with no pin (1.1) |

Not pinned here, deliberately:

- `player_accounts.person_id`: it is the self-exclusion enforcement key, so its mutability matters, but a future
  post-KYC person re-link is foreseen by `05-identity-architecture.md`. Tracked as a new follow-up
  **PERSON-RELINK-PIN-1** (identity owner decision), not decided here.
- `player_accounts.email`, `kyc_verifications.person_id`: no evidence either way; outside this finding.

**D3. Codes (class `BR`, "brand binding").** Collision check: no `BR` code exists anywhere in the repository
(`grep -rnoE "'BR[0-9A-Z]{3}'|BR0[0-9]{2}"` over `*.sql`, `*.go`, `*.md` is empty); the class is not a PostgreSQL
standard class. Existing families checked: `AP AR CG GP HR LA MA MR PC PI RL SP WD`.

| Code | Meaning |
|---|---|
| `BR001` | `player_accounts` identity column change refused |
| `BR002` | `wallets` identity column change refused |
| `BR003` | `kyc_verifications` identity column change refused |
| `BR098` | `0136` up pre-flight refused: evidence of an earlier re-home or re-point (section 5) |

No `BR` code is mapped to an HTTP code. No legitimate path can raise one, so a `BR` raise is a security signal
(section 8).

**D4. Rejected alternative: "refuse a move into a non-`active` brand under the brand gate key"** (ADR 0113 s9.10's
second option). Rejected because it (a) still allows moving a hold OUT of a brand during its closure window, (b) still
lets a player shop for a weaker brand's approval threshold, RG config or jurisdiction, (c) still makes snapshot
columns stale, (d) needs gate-key locking on an operation that has no legitimate caller, and (e) would invent a
supported re-home flow that no Blueprint text or ADR requires.

**D5. Rejected as the primary control: column-level `GRANT UPDATE (...)`.** It would bind only `igaming_runtime`,
needs every writable column enumerated in both `migrations/` and `deploy/init-app-role.sql`, and breaks silently
when a column is added. The trigger binds every role while enabled (owner included) and follows the established
repo pattern (`*_enforce_immutable_fields`, 0026/0057/0080/0082/0123). It may be added later as defence in depth.

## 3. Evidence: no code path legitimately changes these columns

Searches run on `5bf52c3` (results quoted, nothing assumed):

- `UPDATE (public.)?(player_accounts|wallets)` across `*.go`, `*.sql`, `*.sh`, `*.py` outside `migrations/`:
  production writers are `internal/identity/player_account.go:299` (status), `:314` (password_hash), `:332`
  (verified_at), `:361` (status CAS), `:512` (declared_residence CTE). Test writers:
  `internal/identity/declared_residence_integration_test.go:372` (residence), `internal/rg/rg_integration_test.go:167`
  and `internal/casino/rg_enforcement_integration_test.go:224` (`wallets.status`),
  `internal/payments/rg_enforcement_integration_test.go:166` (`player_accounts.status`),
  `internal/launchgov/status_writer_static_test.go:173` (a string fixture, not executed SQL). None sets a pinned
  column.
- Multiline `UPDATE (player_accounts|wallets) ... brand_id` across the whole tree: no match.
- `SET ... brand_id =` anywhere: only `internal/payoutinstrument/rls_grants_integration_test.go:305`, which is an
  adversarial test on `payout_instruments` expecting `PI011` (unaffected).
- `ON CONFLICT ... DO UPDATE` touching these tables: none (`internal/wallet/wallet.go:53-55` is `DO NOTHING`).
- No migration UPDATEs `player_accounts` or `wallets`.
- `kyc_verifications` writers: `internal/kyc/verification_service.go:196`, `:481`, `internal/kyc/provider.go:539`
  (status / reason / provider_reference) and `internal/kyc/verified_residence_integration_test.go:367-599`
  (verified_residence_* columns). None sets a pinned column.
- `internal/identityresolution` performs no UPDATE (`register.go`, `types.go`, `mock.go`).
- Blueprint: the PDF could not be rendered in this environment (no `pdftoppm`/PDF library), so it was not re-read in
  this session. The repo's own Blueprint reading (`financial-domain-model.md` "Wallet: scoped to PlayerAccount")
  records that the Blueprint "has no concept of one tenant operating several brands", so it cannot specify a
  cross-brand re-home. No ADR in `docs/decisions/` describes one (`grep` for brand transfer / move player / player
  migration / account merge: no hit). Reviewers may re-confirm against the PDF; the decision does not depend on it.

**Fixtures or tests that currently update a pinned column: NONE.** No existing assertion changes. All test changes
are additions (section 6).

### 3.1 Composite-FK tables (enumeration)

Every table with `FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id,
brand_id)`, and how its own `brand_id` is pinned today:

| Table | FK origin | Own `brand_id` pin today |
|---|---|---|
| `wallets` | 0019 | **none (fixed here, BR002)** |
| `bonus_grants` | 0057 | `bonus_grants_enforce_immutable_fields` (latest 0070) |
| `sportsbook_bets` | 0078 | `sportsbook_bets_enforce_immutable_fields` (latest 0087) |
| `casino_launch_sessions` | 0079 | `casino_launch_sessions_enforce_immutable_fields` (latest 0108) |
| `casino_provider_rounds` | 0080 | `casino_provider_rounds_enforce_immutable_fields` (0080) |
| `withdrawal_requests` | 0082 | `withdrawal_requests_enforce_immutable_fields` (latest 0123) |
| `deposit_intents` | 0082 | `deposit_intents_enforce_immutable_fields` (0082) |
| `bonus_held_dispositions` | 0082 | `bonus_held_dispositions_enforce_immutable_fields` (0059) |
| `bonus_grant_progress` | 0082 | append-only (`bonus_grant_progress_deny_update`, 0062) |
| `kyc_verifications` | 0082 | **none (fixed here, BR003)** |
| `kyc_documents` | 0082 | `kyc_documents_enforce_immutability` (0040) |
| `jurisdiction_resolutions` | 0082 (nullable composite, MATCH SIMPLE) | append-only (0071) |
| `payout_instruments` | 0123 | `payout_instruments_before_update`, `PI011` |

Tables carrying `player_account_id` + `brand_id` WITHOUT the composite FK: `ledger_adjustment_requests` and
`withdrawal_hold_resolutions` (brand snapshotted by their insert triggers from the wallet / withdrawal; immutable
after submission by their own guards), `kyc_enforcement_decisions` and `player_rg_limit_changes` (append-only),
`bulk_grant_jobs` (job scope). `player_restrictions` and `risk_rules` carry `brand_id` as an administrative scope, not
a player binding (0082 lines 383-406); out of scope.

Once `player_accounts.brand_id` is immutable, every child's `brand_id` is also pinned transitively by its composite
FK, unless the child's `player_account_id` is re-pointed too; that is why `kyc_verifications` (the one child with no
pin) is included.

## 4. Migration 0136 (specification for the implementer)

- **Number:** `0136`. Verified: head is `0135_rg_limits_schema_and_index` and no ref in this repository carries a
  `migrations/0136*` or `0137*` file (`git log --all -- 'migrations/0136*'` empty). The orchestrator allocates; re-check
  at branch time.
- **Name:** `0136_player_brand_binding_immutable.{up,down}.sql`.
- **Up, in order, one transaction:**
  1. `SET LOCAL lock_timeout = '5s'` (0133 precedent). `CREATE TRIGGER` takes `SHARE ROW EXCLUSIVE` on each table,
     briefly.
  2. Pre-flight (section 5), refusing with `BR098`. Never repairs a row.
  3. Three plain `plpgsql` functions (`player_accounts_enforce_identity_immutable`,
     `wallets_enforce_identity_immutable`, `kyc_verifications_enforce_identity_immutable`): NOT `SECURITY DEFINER`,
     schema-qualified references, message names the table and the changed column set, no row values in the message.
  4. Triggers `player_accounts_identity_immutable`, `wallets_identity_immutable`,
     `kyc_verifications_identity_immutable`: `BEFORE UPDATE ... FOR EACH ROW WHEN (<columns> IS DISTINCT FROM ...)`.
     No other BEFORE UPDATE trigger exists on these three tables today, so firing order is not a concern; if one is
     added later, name ordering must keep this guard first (record in that migration).
  5. `COMMENT ON TRIGGER` citing this ADR.
- **No change to any existing function, policy, constraint or grant.** In particular 0128-0135 function bodies are
  not replaced.
- **Down:** drops the three triggers and three functions, restoring the 0135 schema exactly (whole-schema snapshot
  test). No data pre-flight is needed: removing a transition guard cannot make any existing row invalid, and the down
  touches no row. The down is a development tool; removing this control in any persistent environment requires a new
  forward migration and an ADR amending this one.

## 5. Up pre-flight (`BR098`)

Immutability constrains transitions, so no current row can "violate" it. The pre-flight instead refuses when the
database already shows evidence that a re-home or re-point happened, so the guard does not freeze an unexplained
state silently. Pattern: 0124 / 0132 class 9 (owner count with `FORCE RLS` lifted and restored inside the migration
transaction, so the counts are not RLS-blind). Classes:

1. `ledger_accounts` with `wallet_id IS NOT NULL` whose `player_account_id` differs from `wallets.player_account_id`
   (wallet re-pointed, 1.1).
2. `ledger_accounts` with `wallet_id IS NOT NULL` whose `asset_code` differs from `wallets.asset_code`.
3. `ledger_adjustment_requests` whose `brand_id` or `player_account_id` differs from its wallet's (stale snapshot).
4. (Informational, does NOT refuse) count of non-zero `player_withdrawal_hold` / `player_cash` balances, rebuilt from
   `ledger_entries`, on wallets of a `closed` brand. Reported through `RAISE NOTICE` with counts only. It does not
   refuse because it may be a legitimately stranded balance (ADR 0107 closed-tenant funds) and because refusing would
   block the fix itself; the runbook (section 8) makes the operator read it.

Refusal message carries counts per class only (no ids, no amounts). Honest limit: a past re-home of a player who has
no snapshot row leaves no trace, so a clean pre-flight does not prove no re-home ever happened. All current data is
local/MOCK synthetic, so in practice the pre-flight protects persistent dev/staging databases.

## 6. Test plan (all additions; no existing assertion changes)

Integration tests run as `igaming_runtime` unless stated. Each asserts the exact SQLSTATE and that no row changed.

- **T1 adversarial CTE (the C2 shape, both orders):** `WITH pa AS (UPDATE player_accounts SET brand_id=B2 ...) UPDATE
  wallets SET brand_id=B2 ...` and the reverse nesting. Expect class `BR` (`BR001` or `BR002`, whichever row fires
  first; assert the class and assert both tables unchanged).
- **T2 the motivating scenario:** player with a non-zero `player_withdrawal_hold` balance and NO brand-pinned child
  row. Note: any `withdrawal_requests` row, even terminal, already makes the re-home fail with `23503` (its brand is
  pinned and FK-bound), so the fixture must be the stranded-hold shape security used (the ADR 0113 slice-2 stranded
  hold, owner tamper-plant pattern with `wdpath.OnPersistentDatabase` CI rule); a second case with a terminal request
  asserts `23503` before 0136 and class `BR` after. B2 closed after `LA025` evaluated; attempt T1 into B2: refused; `LA025` arithmetic for B1
  unchanged; closing B1 still refused with `LA025`.
- **T3 single-table updates:** each pinned column of each table, alone. `player_accounts.brand_id` alone fails
  on its own trigger (`BR001`) before the FK would.
- **T4 1.1 shapes:** `wallets.player_account_id` re-pointed to a same-brand player -> `BR002`; `wallets.asset_code`
  -> `BR002`; `kyc_verifications` re-pointed (player + brand together) -> `BR003`.
- **T5 no-op and legitimate updates pass:** `SET brand_id = brand_id`; `player_accounts` status / password_hash /
  verified_at / declared_residence (existing service functions); `wallets.status` freeze (existing RG tests);
  `kyc_verifications` status / reason / provider_reference / verified_residence_*.
- **T6 inserts unaffected:** registration through `identityresolution.Register`; `wallet.GetOrCreate` including the
  concurrent first-use race; KYC verification create. Full existing integration suite green, unchanged.
- **T7 owner binding:** as the table owner with the trigger enabled, the T1 statement is refused (the trigger binds
  the owner). `DISABLE TRIGGER` bypass is a residual (section 7), asserted only in the catalog test below.
- **T8 catalog:** exactly three triggers, `tgenabled = 'O'`, BEFORE / ROW / UPDATE, on exactly these tables, functions
  not `SECURITY DEFINER`; the `WHEN` clause lists exactly the pinned columns of section 2 (parse
  `pg_get_triggerdef`).
- **T9 pre-flight:** on a scratch database at 0135, as owner, plant each class 1-3 then apply 0136 up -> `BR098`, no
  trigger created, planted rows unchanged. Class 4 planted -> up succeeds with the NOTICE.
- **T10 down/up cycle:** up, down, up; whole-schema snapshot after down equals 0135's.
- **T11 static guard (cheap, recommended):** a Go static test fails if any non-test `.go` file contains an SQL
  `UPDATE` of `player_accounts`, `wallets` or `kyc_verifications` whose SET list names a pinned column (mirrors
  `internal/launchgov/status_writer_static_test.go`), with a negative control.

**Mutation plan (each must be killed by a named test):** M1 drop `brand_id` from the `wallets` WHEN clause (T1/T3);
M2 drop it from the function body only (T3, since WHEN still passes the row in); M3 drop the `player_accounts`
trigger (T3; T1 still fails on `BR002`, so T3's single-table case is the killer); M4 `FOR EACH STATEMENT` (T3);
M5 `AFTER` instead of `BEFORE` (T8; behaviourally equivalent for refusal, so the catalog test is the killer and the
equivalence is recorded); M6 remove `player_account_id` from the wallets pin (T4); M7 remove `kyc_verifications`
trigger (T4); M8 delete pre-flight class 1 (T9); M9 down leaves a function behind (T10); M10 `<>` instead of
`IS DISTINCT FROM` (equivalent on these NOT NULL columns; recorded as an equivalent mutant, not a gap).

## 7. Threat-model note and residuals

1. **Owner credential bypass (ADR 0110 s9.4).** The migration/owner role can `DISABLE TRIGGER` or drop it. This is
   outside the threat model, as for every other guard. Detection after an owner bypass is partial: reconciliation D
   (0134) still sees a hold per tenant, but D is per tenant, not per brand, so it does not see WHERE a hold sits.
   A per-brand check ("non-zero player balance on a closed brand") is a cheap read-only addition to D; recorded as
   follow-up **BRAND-CLOSED-BALANCE-CHECK-1** for ledger-finance, not decided here.
2. **Value movement by ledger posting is not this control.** A forged posting moving value from a wallet in brand A
   into an existing wallet in a closed brand B is a posting-path question (ADR 0113 residual 9.2 / 9.5, `GP011` covers
   stakes only). This ADR closes the re-home path only.
3. **`person_id` stays mutable** (PERSON-RELINK-PIN-1, section 2).
4. **Detection limit of the pre-flight** (section 5).

## 8. Rollout and runbook notes

- No Go change is required to ship: no production code writes a pinned column (section 3). The migration can ship
  alone.
- Before applying on a persistent database: take the migration lock window as for 0133; read the class 4 NOTICE.
  Any non-zero class 4 count is investigated as a potential P1 (stranded or moved funds) with ledger-finance before
  any closure is treated as clean.
- On `BR098`: STOP. Do not repair rows from SQL. Open a P1 incident (it is evidence of past re-home / re-point),
  escalate to ledger-finance and security through the orchestrator. On local/dev databases the remedy is to rebuild
  from migrations (as WD098).
- Observability (follow-up, NOT built): log SQLSTATE class `BR` at the database-error mapping layer as a security
  event; it should never occur in normal operation.
- Documentation on merge: ADR 0113 s9 item 10 -> "closed by ADR 0118 / migration 0136"; task-registry row
  BRAND-REHOME-HOLD-1 -> closed with evidence; `financial-domain-model.md` "Wallet identity" notes the DB-enforced
  pin; new registry rows PERSON-RELINK-PIN-1 and BRAND-CLOSED-BALANCE-CHECK-1; HANDOVER.

## 9. Review requirements

| Reviewer | Scope | Gate |
|---|---|---|
| `security` | LIVE re-run of the exact C2 CTE and both 1.1 shapes before and after 0136; T1/T4/T7 evidence; confirm no BR code leaks row data | blocking |
| `ledger-finance` | T2 against `LA025` / check D; pre-flight classes 1-4; whether class 4 should refuse; BRAND-CLOSED-BALANCE-CHECK-1 | blocking |
| identity owner | D1 (re-home unsupported), PERSON-RELINK-PIN-1 left open | blocking for D1 |
| `code-reviewer` | migration, catalog and down snapshot tests, mutation evidence | blocking |
| `qa` | test plan coverage and mutation results | blocking |

Labels: everything here is `NOT IMPLEMENTED`; evidence will be MOCK/local only.

## 10. Interaction with migrations 0128-0135

- **0128/0129/0130** (`launch_subject_status_guard` on `tenants`/`brands`; brand gate key): different tables, no
  shared function. 0129's `GP011` comment assumes `wallets.brand_id` is "the FK-pinned brand of the player"; 0136
  makes that true over time, not only at insert.
- **0131** (withdrawal subject-active guard): reads status via the withdrawal row; unaffected.
- **0132** (withdrawal state machine, hold invariant, WD043): no trigger on `wallets`/`player_accounts`; unaffected.
  0136's pre-flight reuses its owner-count pattern.
- **0133** (`LA025` joins `wallets.brand_id`): its per-brand count becomes stable; the move-out-of-count bypass closes.
- **0134** (check D): unaffected; remains the detection backstop for owner bypass of holds.
- **0135** (RG; `rg_limit_configs` brand scope; environment anchor): brand-scoped RG resolution can no longer be
  shopped by re-home; no shared object.
- 0136 replaces no function from 0128-0135 and adds no `LA`/`GP`/`WD`/`RL` code.

## 11. Owner question (non-blocking)

**Q-BR-1 (owner, commercial):** Is there any planned need to move existing players between brands of a tenant (brand
consolidation, rebrand that merges skins, operator acquisition)? **Recommended answer: not now.** A brand rename or
re-theme is a `brands` configuration change and is unaffected. Moving players between brands would be a new governed
transfer flow with its own ADR (account close/open plus explicit ledger transfers, ledger-finance owned). This ADR is
reversible by a later forward migration, so the question does not block implementation.

## 12. Implementation record (2026-10-13, `ledger-finance`; NOT MERGED, pending section 9 reviews)

Branch base `2ee2041`. Labels: migration and tests `IMPLEMENTED`; evidence MOCK/local only (private databases on the
local CI Postgres; no GitHub CI, CI-BILLING-1). Nothing here is a review sign-off.

**Files.**

| File | What |
|---|---|
| `migrations/0136_player_brand_binding_immutable.up.sql` | s4 exactly: `SET LOCAL lock_timeout = '5s'`; pre-flight `BR098` (classes 1-3) + class 4 NOTICE; three plain plpgsql functions (search_path pinned, not SECURITY DEFINER, schema-qualified, message = table + changed column names only); three BEFORE UPDATE ROW triggers with WHEN; `COMMENT ON TRIGGER` |
| `migrations/0136_player_brand_binding_immutable.down.sql` | drops the three triggers and functions; restores the 0135 schema exactly (T10) |
| `internal/launchgov/brand_binding_integration_test.go` | T1, T2 (both cases), T3, T4, T5, T7 (runtime role; owner for T7) |
| `internal/tenant/migration_0136_integration_test.go` | 0135 hole vs 0136 refusal for the CTE (both orders) and the three s1.1 shapes; T8 catalog; T9 pre-flight; T10 up/down/up |
| `internal/identity/brand_binding_static_test.go` | T11 static writer guard + detector negative control + non-vacuity coverage |
| `internal/tenant/status_readers_static_test.go`, `docs/security/launch-status-readers.md` | allow-list entry for the class 4 literal `AND b.status = 'closed'` (see deviation 1) |
| `docs/plans/prh2-hardening-round/adr0118-mutation-kill.txt`, `adr0118-mutation-driver.py` | mutation evidence |

**Deviations from the design text (each reversible, none changes the decision).**

1. s6 "no existing assertion changes": ONE existing static pin gained an allow-list entry.
   `TestStatusLiteralPin_NoSuspendedOrClosedComparisonOutsideAllowList` (internal/tenant) refuses any
   tenant/brand status literal comparison outside its reviewed list; the class 4 count compares `b.status = 'closed'`.
   Added: `migrations/0136_player_brand_binding_immutable.up.sql:AND b.status = 'closed'` (informational NOTICE only,
   refuses nothing; `pending_launch` is not closed and holds no player funds). No assertion was removed or weakened.
   No migration-head pin exists in the repository (grep for `0135`/`135` in tests: every 0135 test scopes its
   migration directory to `<= 135`), so none changed.
2. Pre-flight additions beyond s5's text: (a) `LOCK TABLE player_accounts, wallets, kyc_verifications IN SHARE ROW
   EXCLUSIVE MODE` first (the lock `CREATE TRIGGER` takes anyway), so no re-point can land between the scan and the
   trigger creation; (b) FORCE RLS is lifted on the five tables the counts read (`wallets`, `ledger_accounts`,
   `ledger_adjustment_requests`, `ledger_entries`, `brands`) inside one `DO` block and restored before the refusal,
   as 0132 class 9b. Operational consequence: `ALTER TABLE ... NO FORCE` takes ACCESS EXCLUSIVE on those tables
   (including `ledger_entries`) until the migration commits; the 5 s `lock_timeout` makes a busy table fail the
   migration fast instead of queueing writers. Same profile as 0132.
3. T2 second case ("23503 before 0136"): executed on the shared test database by disabling the two 0136 triggers in
   ONE rolled-back owner transaction (for these tables that is exactly the 0135 schema: 0136 adds only these
   triggers), then asserting the triggers are enabled again. The scratch-database 0135 evidence (no trigger at all)
   covers the no-child shapes (CTE both orders, wallet re-point, asset change, KYC re-point).
4. T5 `declared_residence`: asserted with the exact SET list of `identity.SetPlayerAccountDeclaredResidence` (the
   function needs an active evidence-collection configuration; its own suite runs it end to end and stays green).
   status / password_hash / verified_at / status CAS / reinstate go through the real service functions.
5. T6 inserts: no new test; the existing suites (`identityresolution` concurrent registrations,
   `wallet` `TestGetOrCreate_ConcurrentFirstUseCreatesExactlyOneWallet`, `kyc`) run green, unchanged, at head 0136.
6. M5 (AFTER instead of BEFORE) is NOT behaviourally equivalent as s6 predicted: an AFTER row trigger fires after the
   RLS WITH CHECK, so `wallets SET tenant_id` returns `42501` instead of `BR002` (T3 kills it); the catalog (T8)
   kills it too. M10 (`<>`) is behaviourally equivalent on these NOT NULL columns and is killed statically by T8
   (the WHEN clause and the body must be `IS DISTINCT FROM`).
7. Test placement: behaviour tests in `internal/launchgov` (its world fixtures: governed closures, stranded holds,
   LA025 counts), migration tests in `internal/tenant` (scratch helpers), static guard in `internal/identity`.

**Extra live probes (private DB at 0136, `igaming_runtime`, rolled back):** `MERGE INTO wallets ... UPDATE SET
brand_id` -> `BR002`; `INSERT INTO wallets ... ON CONFLICT (id) DO UPDATE SET brand_id` -> `BR002`; `UPDATE
player_accounts ... FROM (...)` re-home -> `BR001`; `SET session_replication_role = replica` -> `42501`; `ALTER TABLE
wallets DISABLE TRIGGER` -> `42501` (must be owner).

**Verification (exact commands and results):** see the orchestrator hand-off report and
`docs/plans/prh2-hardening-round/adr0118-mutation-kill.txt`. Summary: integration suites of `internal/tenant`,
`internal/identity`, `internal/identityresolution`, `internal/wallet`, `internal/ledger`, `internal/launchgov`,
`internal/withdrawal`, `internal/reconciliation`, `internal/db`, `cmd/migrate`, `internal/rg`, `internal/kyc` PASS at
head 0136; repo-wide unit tests PASS; gofmt clean; go vet (default and `-tags integration`) clean; golangci-lint
(`--build-tags integration --new-from-rev=2ee2041`) 0 issues. Mutation plan M1-M10: **10 of 10 killed** (final pass;
M4 does not apply; M5 and M10 notes in deviation 6); T11 manually mutated and killed. Evidence file above.

**Residuals (unchanged from s7, plus):** class 3 compares brand / player only (not `asset_code`; an asset re-point of
a wallet with no ledger account but an adjustment request would not be reported); class 4 counts closed BRANDS only
(a closed tenant with active brands is not reported); a past re-home of a player with no snapshot row leaves no trace
(s5). HANDOVER and the registry closure are "on merge" items (s8) and are NOT done here beyond the registry status.
