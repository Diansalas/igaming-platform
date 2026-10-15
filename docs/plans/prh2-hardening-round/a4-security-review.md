# A4 security review: MANUAL-ADJ-LINK-1 (migration 0153)

- **Reviewer:** `security` (independent live review). **Date:** 2026-10-15.
- **Subject:** commit `4c77ad5` on `worktree-agent-aae3ab5308d231dfa` (base `73b2836`): migration 0153 up/down
  (`ledger_transactions_manual_adjustment_link` MA050, `ledger_entries_manual_adjustment_link` MA051),
  `internal/testsupport/adjlinkfix`, `internal/testsupport/adjlinkpost`, the `TestNoTriggerDisableOutsideTests`
  allowlist entry, the new static/integration tests, ADR 0100 §21, `docs/plans/a4-adjustment-fence-record.md`.
- **Method:** I did not rely on the record. I rebuilt a PRIVATE database (`sr4_19264`, PostgreSQL 16.13) from the
  worktree's migrations, ran the fence tests to seed data, then attacked it directly with raw SQL as
  `igaming_runtime` and as the non-superuser table owner `igaming`. I also used two concurrent sessions and
  re-killed three of the claimed mutants. The DB was dropped afterwards. No role, password or shared
  infrastructure was touched. All evidence is LOCAL, not CI.

## Verdict

**ACCEPT WITH CONDITIONS.** I found no finding that blocks merge. The fence did what ADR 0100 §21 claims in
every attack I ran. All findings are Low or Informational. Conditions before the registry row is closed:

- **C-1:** record F3 (the fixture-strategy deviation) as an explicit, accepted deviation.
- **C-2:** correct the session_replication_role statement (F2).
- **C-3:** register F1 (house-account borrowing) as a tracked follow-up under ADR 0110 T5. Don't leave it as prose only.

Hosted CI and the independent `ledger-finance` / `code-reviewer` reviews are still pending and are not covered
by this review.

**Launch flag:** this change does not add a launch blocker. It removes one: the K2-2 / LF-ruling-4 gap is
closed for every non-DDL session. ADR 0110 T5 remains OPEN and launch-relevant, and F1 is part of T5.

## Live probe results (private DB)

The runtime role used forged GUCs throughout. "Tenant-principal shape" means `app.tenant_id` plus an arbitrary
`app.principal_id`, which the runtime role can always set.

| # | Attack | Session | Result |
|---|---|---|---|
| R0 | `SET session_replication_role = replica` | runtime | 42501 permission denied |
| R0 | `ALTER TABLE ledger_transactions DISABLE TRIGGER ...` | runtime | 42501 must be owner |
| R0 | `CREATE SCHEMA` / `CREATE TEMP TABLE` shadow / `CREATE FUNCTION` in public / `SET ROLE igaming` | runtime | 42501 on each (no CREATE, no TEMP, not a member of the owner role) |
| R1 | Unlinked manual_adjustment, no GUCs | runtime | 42501 RLS (never reaches the fence) |
| R2 | System-tenant shape, key of a pending request | runtime | MA050 (request not visible) |
| R3 | Tenant-principal shape, pending (visible) request | runtime | MA050 "not executing in this transaction" |
| R3b | Random key / correlation | runtime | MA050 |
| R4 | `UPDATE ... SET state='executing'` (with and without `executed_txid`) | runtime | CG001 (forged principal is not active staff) |
| F-proof | Real approver staff id as `app.principal_id` with a forged `app.actor_proof`, then with an empty one; then `-> executing`; then the posting | runtime | AP001 malformed, AP001 no proof, MA030, MA050 |
| R4b | `DELETE` of a request | runtime | 42501 (no DELETE privilege) |
| R5 | Another tenant's request id, posted in own tenant / in that tenant | runtime | MA050 / 42501 RLS |
| R6 | Executed request: same key / variant key | runtime | 23505 unique / MA050 key mismatch |
| R7 | Balanced pair appended to a COMMITTED manual_adjustment (same amount, and 999) | runtime | MA051 "no request executing in this transaction" |
| R8 | `INSERT ... ON CONFLICT DO NOTHING` (new row) / `ON CONFLICT DO UPDATE` | runtime | MA050 / append-only UPDATE refusal |
| R9 | `UPDATE transaction_type` (reclassify), `DELETE` entries, `TRUNCATE` | runtime | append-only P0001 / P0001 / 42501 |
| R10 | `COPY ledger_transactions FROM STDIN` | runtime | 0A000 (COPY FROM is unsupported under RLS) |
| R11 | `SET LOCAL search_path = pg_temp, public` then post | runtime | MA050 (function search_path is pinned) |
| R12 | `'Manual_Adjustment'` type variant | runtime | 23514 CHECK |
| R13 | `SET CONSTRAINTS <fence> DEFERRED` / `ALL DEFERRED` then post | runtime | 42809 not deferrable / MA050 at statement end |
| O0 | `SET session_replication_role` | owner | 42501 (see F2) |
| O1 | Unlinked manual_adjustment, tenant-principal shape, no trigger DDL | owner | MA050 |
| O2 | Owner residual: 0113 request triggers disabled, request forced to `executing` in this txid, 1 of 2 approvals | owner | MA050 "not approved (counted 1, required 2)": the fence's own (d) re-check holds without the 0113 guard |
| O3 | Same, but `executed_txid` = another txid | owner | MA050 "not executing in this transaction" |
| O4 | Same, payload (amount) tampered, hash kept | owner | MA050 "payload_hash does not match" |
| O5 | Wrong reason_code; provider ids set | owner | MA050 keys mismatch (each) |
| O6 | Entry fence alone (tx planted with the tx fence off): wrong amount; wrong player direction; correct two legs; extra balanced pair | owner | MA051; MA051; OK; MA051 "already holds a credit leg" |
| AF | adjlinkfix model: session A (owner) disables both 0153 triggers and holds the transaction open; session B (runtime) meanwhile | two sessions | B sees `tgenabled = 'O'` for both triggers, reads `ledger_transactions` normally, and its INSERT blocks (55P03 under a 3 s lock_timeout). After A rolls back, both triggers are `'O'` again |
| DN | Down, down again, up, in one rolled-back transaction | owner | After down: 0 triggers, 0 functions, 0 constraint rows. Second down is idempotent. Re-up restores 2 triggers and 2 constraints |
| T5 | See F1 | runtime | committed |

**Mutant re-kill (independent).** I applied each mutant to the private schema as the owner and ran the
behavioural tests:
- T11 (`enabled` arm off): killed by "policy disabled after submission".
- T13 (count arm off): killed by "executing without counted approvals".
- E19 (duplicate-leg arm off): killed by "duplicate player leg" and "duplicate leg in ONE statement".

I re-applied 0153 after each mutant. I did not re-run the other 41 mutants.

**Suites re-run on the private DB:** `-tags integration -count=1 -p 1` for `internal/adjustment`,
`internal/ledger`, `internal/tenant` and `internal/idempotency`: all PASS. `go build ./...` without tags: OK.
`adjlinkfix` is excluded by build constraints, and nothing under `./cmd/...` depends on any `testsupport`
package.

## ADR 0100 §21 claims checked

| Claim | Status |
|---|---|
| No GUC/role/environment condition, no SECURITY DEFINER, search_path pinned | VERIFIED (`proconfig = search_path=pg_catalog, public, pg_temp`, `prosecdef = f`). The unpinned 0113 helpers it calls inherit the pinned path. pg_temp is last |
| Refuses runtime and owner in every non-acting shape, forged GUCs, other tenant, executed request, wrong shape | VERIFIED (table above) |
| (b)/(c)/(d)/(e) re-checked by the fence itself, not trusted from the 0113 state flag | VERIFIED live (O2 to O5) with the 0113 request triggers disabled |
| Nothing can be appended to a committed adjustment | VERIFIED (R7) |
| NOT DEFERRABLE | VERIFIED (R13) |
| AFTER keeps the ADR 0020 replay contract | VERIFIED indirectly: R6 gives 23505 before the fence. The replay tests pass |
| adjlinkfix's in-transaction disable is invisible to other sessions and blocks their writers | VERIFIED (AF) |
| adjlinkfix is unreachable from production code | VERIFIED (build tag. A non-tagged importer cannot compile. Production Dockerfile builds without tags). The real control is the DB privilege: runtime is not the owner (R0) |
| Down restores the 0151 schema, touches no row, is idempotent | VERIFIED for the objects (DN). The byte-for-byte schema diff is the author's test (passes) |
| `session_replication_role` is an owner residual (§21.5 item 1) | INACCURATE (F2) |
| "No owner exemption added" (§21.3) | TRUE for the production schema. The fixture strategy differs from the agreed one (F3) |

## Findings

### F1: Low (within ADR 0110 T5, OPEN). Other transaction types can use the tenant's `manual_adjustment` house account

**Scenario (reproduced, committed on the private DB).** A runtime session with only `app.tenant_id` (the
system-tenant shape, no proof, no request) inserts a `bonus_forfeiture` transaction. Its entries debit the
tenant's `manual_adjustment` house account by 777,777 and credit a player's `player_cash` by 777,777. It commits.
The player's projected cash goes from 40 to 777,817. The §12 detector (`ledger_unlinked_manual_adjustment`)
reports nothing because it filters on `transaction_type`. The house account now carries a debit that no governed
request explains.

**Assessment.** This adds no new capability. Under T5 the same session can mint with a `deposit` against any
house account. 0153 is scoped to `manual_adjustment` transactions, and §21.5 item 3 discloses this. The specific
harm is misattribution. The house account that operators and auditors read as "governed adjustments only" can
hold ungoverned flows that no control flags. Low for this change.

**Fix (follow-up, not in A4):**
1. In `ledger_manual_adjustment_entry_check()`, when the parent is not a `manual_adjustment`, refuse an entry
   whose account is `account_type = 'manual_adjustment'` (new SQLSTATE, e.g. MA052). The cost is one more PK
   lookup per entry. `ledger-finance` must first confirm that no legitimate type posts to that account. I found
   none in migrations 0113, 0115, 0124 or 0125.
2. Or, at minimum, add a detector that checks the house `manual_adjustment` account's net movement equals the
   net of executed requests.

Register this under T5.

### F2: Low (documentation accuracy). session_replication_role is not an owner residual on the deployed version

ADR 0100 §21.5 item 1 lists `session_replication_role` next to owner DDL. On PostgreSQL 16 the non-superuser
owner `igaming` gets 42501, and so does the runtime role. Only a superuser, or a role explicitly granted SET on
the parameter (PG15+, via `pg_parameter_acl`, which is empty here), can set it. Both 0153 triggers are
`tgenabled = 'O'`, so a superuser with `replica` would skip them. That is the superuser residual, not the owner
residual.

**Fix:** reword item 1. Optionally add a catalog test asserting `pg_parameter_acl` grants no
`session_replication_role` SET to any application role.

### F3: Low (process / disclosure). Fixture strategy deviates from the agreed rule and is not recorded as a deviation

ADR 0100 §12 and the MANUAL-ADJ-LINK-1 registry row say the 19 fixture files "move to request-backed helpers".
A4 instead moves 15 caller files to an owner-only `DISABLE TRIGGER` exemption (`adjlinkfix`). The §12 amendment
note discloses only the BEFORE-to-AFTER change. Security's position: the exemption is acceptable. It is not a
production-schema bypass, it is invisible to other sessions and blocks their writers (verified), it is
integration-only, and it is inventoried by a static test.

The cost is that those fixtures never exercise the fence. The fence's own tests cover it, so I accept that. A
request-backed helper would have been the stronger design.

**Fix (C-1):** record the deviation explicitly in ADR 0100 §21.4 / §12 and in the proposed registry text. Get
`ledger-finance` (owner of ruling 4) and the orchestrator to accept it.

### F4: Informational. The trigger-disable static guard is lexical

`TestNoTriggerDisableOutsideTests` matches `DISABLE\s+TRIGGER|session_replication_role`. `adjlinkfix` builds its
DDL by concatenation (`ALTER TABLE ... `+action+` TRIGGER `), so the regex matches only its comments. Any
production file using the same pattern would pass the guard.

The allowlist's build-tag check is `HasPrefix("//go:build integration")`. That also accepts
`//go:build integration || x`. This is pre-existing, and the new adjlinkfix inventory test uses the stricter
`"//go:build integration\n"`.

The real controls hold: the runtime role is not the owner, and production builds use no tags.

**Fix (optional):** extend the guard to flag `ALTER TABLE` and `" TRIGGER "` string fragments in non-test,
non-migration Go. Use the strict build-line check in the allowlist branch too.

### F5: Informational (pre-existing T5, out of 0153 scope). Balanced entries can be appended to a committed non-adjustment transaction

Verified with a rolled-back probe: a runtime session appended a debit/credit pair to an already committed
`bonus_forfeiture` transaction, and the deferred balance check passed. 0153 seals only `manual_adjustment`
transactions.

**Recommendation:** `ledger-finance` should consider a generic seal under T5: entries only in the database
transaction that inserted the parent.

### F6: Informational. The entry fence now runs on every ledger entry and fails closed on an invisible parent

Cost: one plpgsql call plus one PK lookup per entry. The new fail-closed behaviour is that any future session
shape that may INSERT entries but not SELECT the parent transaction will get MA051, even for non-adjustment
types. Every current shape passed (author's suites, and my re-run of adjustment, ledger, tenant and idempotency).

Note this for future acting or service session shapes, and for load testing.

### F7: Informational. Record nit

In `docs/plans/a4-adjustment-fence-record.md`, the proposed registry text says "Mutation pass: see
docs/plans/a4-adjustment-fence-record.md", which points at itself. It should point to ADR 0100 §21.6.

## Tests security requires to stay (authorization / tenant isolation)

These are already present in `link_fence_0153_integration_test.go`. Keep them as gates:
- An unlinked posting is refused for runtime AND owner in every non-acting shape.
- Forged tenant or acting GUCs without an approved request are refused.
- A foreign tenant's request id is refused.
- An executed request cannot post again.
- An append to a committed adjustment is refused.
- The fence's own (b)/(c)/(d)/(e) checks hold with the 0113 guard disabled.

If F1's fix lands, add: "a non-manual_adjustment posting that touches the house `manual_adjustment` account is
refused, for runtime and owner".

## Scope of this review

**In scope:** what is listed above, at code and live-DB level on PostgreSQL 16.13, local.

**Not in scope:** hosted CI; the 41 mutants I did not re-run; the payments, casino, sportsbook, risk, wallet,
launchgov and reconciliation suites (the author reports them PASS, and I did not re-run them); performance under
load; the T5, T3 and T4 classes beyond F1 and F5; any production role provisioning other than what
`priv_db.sh` mirrors.

Passing this review does not make the subsystem "secure". It means no blocking finding was found in the stated
scope.
