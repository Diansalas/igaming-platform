# ADR 0118 - Player brand binding is immutable (BRAND-REHOME-HOLD-1)

- **Status:** ACCEPTED pending implementation (`architect`, 2026-10-13). DESIGN ONLY: no SQL, Go or migration is part
  of this document. Every item below is `NOT IMPLEMENTED` until migration `0136` and its tests merge with the reviews
  named in section 9. There is no blocking owner question (section 11 has one non-blocking confirmation).
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
brand-pinned child (for example a governed or raw `manual_adjustment`, or a hold planted by a forged leg - ADR 0113
residual 9.2/9.5). That is a real population, not a theoretical one.

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
- **T2 the motivating scenario:** player with a withdrawal hold and no brand-pinned child row (hold built through the
  0132 legal path helper, then the request driven terminal so only the ledger remains, or the ADR 0113 slice-2
  stranded-hold fixture); B2 closed after `LA025` evaluated; attempt T1 into B2: refused; `LA025` arithmetic for B1
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
