> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# SB-T1-XMIN planning analysis

Scope: PLANNING ONLY. No repo files touched (verified with a standalone
`xmin_scratch` DB, created and dropped in this session; CI/synthetic DBs
untouched). All empirical results below ran on the local PostgreSQL 16.13
cluster (`sudo -u postgres psql`), each experiment inside a transaction
that ended in `ROLLBACK` unless stated otherwise.

## 1. Root cause — confirmed empirically

`migrations/0091_sportsbook_settlement.up.sql:244-255` (T-1, the
composed-void `causation_record_id` branch):

```sql
SELECT xmin::text INTO cause_xmin FROM sportsbook_bet_settlements WHERE id = NEW.causation_record_id;
IF cause.id IS NULL OR cause.bet_id <> NEW.bet_id OR cause.event_kind <> 'rollback'
    OR cause_xmin <> (pg_current_xact_id()::text::bigint % 4294967296)::text THEN
    RAISE EXCEPTION 'sportsbook_bet_settlements: a void may cite only a rollback of this bet inserted by the same transaction';
END IF;
```

`pg_current_xact_id()` (`internal/... ` n/a — Postgres builtin) always
returns the **top-level** transaction's xid8 (docs confirm this; verified
below). A row's `xmin` is the **32-bit xid of the (sub)transaction that
inserted it** — for a row inserted directly under `SAVEPOINT`/`RELEASE
SAVEPOINT`, that is the **subtransaction's own xid**, distinct from and
numerically greater than the top-level xid, and it is never rewritten by
`RELEASE SAVEPOINT`. So the equality check is comparing "this row's
(sub)xact id" to "the top-level xact id" — sound only when the row was
inserted as a plain, non-nested statement on the top-level transaction.

### Empirical verification (session on `xmin_scratch`, PG 16.13, schema `probe(id serial, note text)`)

One transaction, `pg_current_xact_id()::text::bigint` = `2132006` throughout:

| Insert | `xmin` (bigint) | equals top xid? |
|---|---|---|
| plain (no savepoint) | 2132006 | **yes** |
| under `SAVEPOINT sp1; ... RELEASE SAVEPOINT sp1` | 2132007 | no |
| nested outer (`SAVEPOINT sp_outer`) | 2132008 | no |
| nested inner (`SAVEPOINT sp_inner` inside `sp_outer`) | 2132009 | no |
| under `SAVEPOINT sp_rb`, then `ROLLBACK TO SAVEPOINT sp_rb` | — | row does not exist (0 rows on re-select in the same transaction) |

- Every savepoint/nested-savepoint case consumes a **new, distinct**
  subtransaction xid, strictly increasing, none equal to the top-level
  xid captured at transaction start (`RELEASE SAVEPOINT` does not
  retroactively relabel the row's stored `xmin`).
- `pg_xact_status(xid8)` for each of the four **surviving** rows, computed
  by taking the current top-level xid8's epoch and substituting each row's
  32-bit `xmin` into the low bits (`epoch_component + xmin::bigint`,
  since a same-epoch, no-wraparound single transaction was used):
  all four — the plain insert **and all three savepoint/nested-savepoint
  inserts** — report `in progress`. This is from **inside the same
  top-level transaction**, i.e. `pg_xact_status` classifies every xid in
  the current transaction's own tree (top-level and every subtransaction,
  however nested) as `in progress` under the current snapshot, which is
  exactly the ADR's SB-T1-XMIN replacement claim. Verified.
- `ROLLBACK TO SAVEPOINT` truly removes the row: after
  `ROLLBACK TO SAVEPOINT sp_rb`, re-`SELECT`ing inside the same
  transaction returns 0 rows for that insert. So a `causation_record_id`
  FK pointing at such a row cannot exist post-rollback-to-savepoint in the
  first place — the FK, not T-1, is what would reject that shape (row
  gone ⇒ `NOT FOUND` in T-1's own `SELECT ... INTO cause`, same "not
  found" branch as any dangling reference, not the xmin branch at all).
- Cross-transaction control (a row committed in an **earlier**,
  already-committed transaction, checked from a fresh later transaction):
  `pg_xact_status(...)` on its epoch-qualified xid returns `committed`,
  confirming the intended discriminator ("in progress in *my* tree" vs.
  "already committed/visible from an earlier transaction") is exactly
  what distinguishes a genuine composed void from a rollback-then-void.

### Epoch-qualified xid8 construction and wraparound edge

`xmin` is a 32-bit `xid`; `pg_xact_status` requires a 64-bit `xid8`
(epoch<<32 | xid). Because a single logical transaction cannot itself
span an xid-epoch wraparound (wraparound is on the order of 2^31
transactions), the current top-level transaction's own epoch — obtainable
by masking the low 32 bits off `pg_current_xact_id()::text::bigint` — is
the correct epoch for every subxid the same top-level transaction has
opened, **except** at the exact instant a row's raw 32-bit `xmin` value is
numerically *greater than* the top-level xid's low 32 bits (i.e. the
'wraparound just occurred *within* this transaction's own subxid
sequence' edge, which cannot happen for a single non-huge transaction —
subxids only increase). The documented edge that matters in general (and
that ADR 0088's implementation note already flags) is: if the row's raw
xmin is *numerically greater than* `pg_current_xact_id()`'s low 32 bits,
use `epoch − 1`, because the low-32 counter itself wrapped between the
row's insertion and the read — this only matters for rows from **older**
transactions (the cross-transaction check), never for a row inserted by a
still-open subtransaction of the current top-level transaction (subxids
are always issued after, hence numerically ≥, the top-level xid, until an
actual epoch wraparound occurs, which is an out-of-scope operational
event independent of this trigger). This matches the ADR's phrasing
exactly and is confirmed consistent by the experiment above (all subxid
values were monotonically increasing integers above the top xid, in the
same epoch).

### Alternatives evaluated

**(a) `pg_xact_status(<epoch-qualified xmin>::xid8) = 'in progress'`.**
Empirically correct (above): classifies every xid belonging to the
current transaction's own tree — top-level or any subtransaction, however
nested, released or not — as `'in progress'`, and any earlier-committed
transaction's xid as `'committed'`. Because Postgres MVCC visibility
already guarantees a row from a *concurrent, different* transaction is
either invisible (not yet committed) or would show as `'committed'`/
`'aborted'` once visible, a **visible** row reporting `'in progress'` can
only belong to the checking transaction's own tree. This is the
correct, general fix and is what ADR 0088 §3.3 already names as
`SB-T1-XMIN`'s target replacement.

**(b) Compare against `pg_current_xact_id()` AND all subxact ids.**
No public API enumerates a transaction's own subtransaction ids from SQL
(subxids are only visible via internal snapshot machinery, which is
exactly what `pg_xact_status` already exposes). This alternative reduces
to (a) with extra, unavailable bookkeeping — rejected as strictly worse
than (a).

**(c) A transaction-local marker via `set_config('sportsbook.composed_void_rollback', <row id>, true)`.**
Technically works (GUC is transaction-scoped with `is_local = true`), but
it is **caller-settable**: any role connected to the database — including
a compromised or buggy application code path, or anyone with a raw SQL
console under RLS — can set an arbitrary custom GUC before an INSERT,
with **no DB-level provenance check** tying the GUC value to any actual
rollback-row-insertion event. `xmin` (and `pg_xact_status`) are
**database-controlled facts** about a row's own insertion transaction;
a GUC is an **assertion by the client**, exactly the class of "trust the
caller" mechanism CLAUDE.md's authorization rules reject ("Authorization
is enforced server-side only... never inferred from the UI or trusted
from the client" — the same principle applies to a trigger trusting a
session variable the same session set). It would also require every
future caller (batch driver, provider-webhook driver, test harness) to
remember to set and clear it correctly, silently reintroducing exactly
the kind of unenforced precondition B-1/finding-1 already flagged as
fragile. **Rejected**: weaker security/provenance than (a), and it
doesn't reduce implementation cost enough to justify that weakening.

**(d) Structural rule: void's ledger `causation_id` = rollback's ledger
transaction id (both immutable, both in `ledger_transactions`).**
This says "the void's ledger transaction is causally linked to that
specific rollback's ledger transaction" — true for a composed void
today (`lockAndPost`'s `chainCausation` sets the void's `CausationID` to
the rollback's freshly minted transaction id, `settlement.go:939-941`),
but it is **necessary, not sufficient**, for "composed in the same DB
transaction". Nothing in the ledger schema prevents a hypothetical future
caller from posting a rollback in transaction A, committing, and later
posting a void in transaction B with `CausationID` manually set to A's
rollback transaction id — that is *causally linked* (by convention) but
is exactly the rollback-then-void shape the ADR's §2.1 causation table
distinguishes from a composed void and treats differently (rollback-then-
void's void carries `causation_record_id = NULL` at the history-row
level, by design). Rule (d) cannot detect "same DB transaction" from
`ledger_transactions` alone, because `ledger_transactions` carries no
transaction-boundary marker (no `xact_id` column, and adding one would be
new schema, not a T-1 body-only fix) — it would only re-describe the
causation link that the Go code already sets, without adding any DB-level
guarantee beyond "some code chose to link these." It is **not sufficient**
for the ADR's intent and is **rejected** as the primary rule; it remains
true and valuable as a **secondary** sanity check.

### Recommendation

**Adopt (a):** replace the xmin equality with
`pg_xact_status(<epoch-qualified xid8 of NEW.causation_record_id's xmin>) = 'in progress'`,
keeping every other T-1 condition (bet match, `event_kind = 'rollback'`)
unchanged, and **additionally keep** the existing `cause.bet_id <>
NEW.bet_id OR cause.event_kind <> 'rollback'` checks as-is (rule (d)'s
ledger-causation-id linkage is not proposed as a replacement, only left
as the already-existing Go-level convention — no change needed there).
Rationale: it is the only alternative that (i) is DB-provenance-based,
not caller-assertable, (ii) is verified correct for every case the ADR
needs to distinguish (same-transaction plain insert, same-transaction
savepoint/nested-savepoint insert, and any earlier-committed
transaction's rollback), and (iii) requires no new schema, no new trust
boundary, and no change to the caller contract. This does **not** weaken
the trigger: it strictly **widens** what a legitimate same-transaction
composed void can look like (accepting the savepoint case it currently
rejects) while preserving the reject branch for a genuinely
different/earlier transaction's rollback (still verified above:
`pg_xact_status` on a committed row returns `'committed'`, never `'in
progress'`).

## 2. Affected transaction paths

- The **only** current writer of `sportsbook_bet_settlements` is
  `insertSettlementRecord` (`internal/sportsbook/settlement.go:1023`),
  called exclusively from `writeTransition` (`settlement.go:976`), called
  exclusively from `voidBet` (`:762`) and the settle/rollback counterparts
  in the same file, all of which run inside the single top-level `pgx.Tx`
  that `db.Pool.WithTenant` (`internal/db/tenant_rls.go:30`) opens and
  hands down through `SimulateSettlementEvent` (`settlement.go:308`).
  `insertSettlementRecord` issues one plain `INSERT ... RETURNING id`
  (`:1030-1038`) directly on that `tx` — no `tx.Begin(ctx)` (which is how
  pgx issues a `SAVEPOINT`) is ever called around it. The precondition is
  already documented as a doc comment directly above the function
  (`:1013-1022`), matching ADR 0088 §3.3's implementation note verbatim.
- `ledger.Post` (`internal/ledger/ledger.go:320`) uses
  `db.IdempotentInsert` (`internal/db/idempotency.go:34`) — which opens
  exactly one `SAVEPOINT` via `tx.Begin` — **only** around the
  `ledger_transactions` row insert (`ledger.go:350-360`, inside the
  `db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error { ... })`
  closure). It never wraps `ledger_entries` inserts (those run on the
  outer `tx` after the savepoint commits/rolls back, confirmed by reading
  `ledger.go` around the `IdempotentInsert` call) and it is never called
  from `insertSettlementRecord` or `writeTransition`. So today, no
  `sportsbook_bet_settlements` row is ever inserted inside a `SAVEPOINT`,
  directly or transitively via `ledger.Post`'s own savepoint use — the
  two savepoint-using code paths in the whole financial write path
  (`db.IdempotentInsert` for `ledger_transactions`, and nothing else) do
  not overlap with the history-row insert at all.
- **Future risk, confirmed real, not hypothetical:** ADR 0088 §5.1 itself
  anticipates "any future batch driver must lock them in one `ORDER BY id
  FOR UPDATE`" and the code review (finding 1) and the ADR's own
  `SB-T1-XMIN` deferred item both name (i) a batch driver settling many
  bets per outer transaction, one `SAVEPOINT` per bet, and (ii) a
  provider-webhook driver that wraps the whole operation in its own
  nested transaction, as the two concrete future callers that would hit
  this. Both are out of Stage-10 W1 scope (§1.2: "Real provider
  settlement, webhook... " are explicit non-goals) but are named,
  expected next steps once a real provider or a batch settlement path is
  built — i.e. this is a genuine, tracked forward risk (`SB-T1-XMIN`),
  not a speculative one.

## 3. Migration implications

- Task-registry (`docs/governance/task-registry.md:3794-3795`) and the
  Stage 10 completion report (`docs/governance/stage-10-completion-
  report.md:261-263`) already scope **both** `PAY-REV-1` and
  `SB-T1-XMIN` into the same recommended next stage ("Stage 10.1 —
  PAY-REV-1 remediation plus Stage 10 residual hardening"), each needing
  its own migration. Repo state today: migrations end at `0091` (no
  `0092`/`0093` exist yet) — confirmed via `ls migrations/`.
- Per CLAUDE.md ("never edit 0091" — corrections are compensating,
  historical migrations are immutable once applied/checksummed) and per
  ADR 0088 §12's own down-migration design (a single-transaction refusal
  gate specific to 0091's exact schema), **0091 must not be edited**. The
  fix is a new migration whose **up** file does exactly one thing:
  `CREATE OR REPLACE FUNCTION sportsbook_bet_settlements_validate() ...`
  with the causation branch rewritten to use `pg_xact_status`, leaving
  every other line of the function (and T-2, the table DDL, RLS, indexes,
  REVOKEs) byte-identical to 0091's current body. This is a pure function
  body swap — no `DROP`/`CREATE TABLE`, no data migration, no column
  change.
- **Numbering/coordination with PAY-REV-1:** both land in the same
  planned stage and both take "the next free migration number." Since
  neither has a hard dependency on the other (PAY-REV-1 touches
  `internal/payments`/a deposit-reversal partial unique index; SB-T1-XMIN
  touches only T-1's function body in `sportsbook_bet_settlements_validate`),
  ordering is a coordination choice, not a correctness one. Recommend:
  **PAY-REV-1 = 0092, SB-T1-XMIN = 0093** — PAY-REV-1 is the P1
  (open financial-correctness gap, negative-balance risk on concurrent
  reversals) versus SB-T1-XMIN's P2 (fail-closed availability defect,
  not reachable today); sequencing the higher-severity, currently-active
  risk first is consistent with how both are prioritized in the
  completion report. If `payments`/`ledger-finance` land their migration
  first under a different number, `SB-T1-XMIN` simply takes whatever
  number is free at merge time — the two are independent and commute.
- **Down migration (0093 down, assuming that numbering):** restores
  0091's **exact original function body** (the xmin-equality version),
  verbatim, so that reverting 0093 alone returns the database to exactly
  0091's validated state — never a partial/rewritten version of the old
  body. This is checked mechanically: the down file's `CREATE OR REPLACE
  FUNCTION` body must diff-empty against 0091's `up.sql:145-262` function
  text (tooling: a repo test or CI step diffing the two SQL blobs, similar
  in spirit to the existing migration-checksum guard, would be a
  reasonable `qa`-owned addition but is not strictly required — a manual
  reviewer diff is sufficient given this is a single, small function).
- **No data change.** The migration touches only the trigger function
  definition (`pg_proc`), not `sportsbook_bet_settlements` or any other
  table's rows/columns/constraints. No backfill, no lock beyond the
  implicit lock `CREATE OR REPLACE FUNCTION` takes on the function object
  itself (not on the table), so it is safe to run against a live table
  with existing history rows.
- **Checksum rules:** 0091's own up/down files are never touched (their
  checksums, if the migration runner enforces them, stay exactly as
  applied). The new migration gets its own fresh checksum on first apply.

## 4. Rollback implications

- **Code revert with migration kept:** since the fix is confined to a
  trigger function body and the Go code's behavior on the "reject"
  side doesn't change (the reject error message and `ErrSettlementIntegrity`
  mapping in `lockAndPost` are untouched — T-1 is a DB-only check, the Go
  layer never inspects `xmin`), a plain Go code revert with the new
  migration still applied is safe: T-1 becomes strictly more permissive
  (accepts the savepoint case) with no Go-side behavior depending on the
  old rejection. This is a pure widening at the DB layer, so "code revert,
  migration kept" cannot reintroduce a regression the Go code relies on.
- **Down-migration safety:** confirmed — the new migration's down file
  only swaps `sportsbook_bet_settlements_validate`'s body back to 0091's
  original text via `CREATE OR REPLACE FUNCTION`. It performs no `DROP
  TABLE`, `ALTER TABLE`, or data mutation, so it is safe and fully
  reversible **even with settlement evidence already posted** — unlike
  0091's own down migration (§12), which must refuse once any settlement/
  void/rollback/tombstone row exists (because reverting 0091 would delete
  the whole table/columns those rows depend on). The SB-T1-XMIN migration
  carries none of 0091's irreversibility: reverting it merely restores a
  stricter (but still correct-for-the-precondition-holding-today) trigger
  body, which is safe regardless of how much settlement history already
  exists, since existing rows were all inserted under the plain-INSERT
  precondition and therefore already satisfy the old xmin-equality rule
  trivially (they'd re-validate correctly under either version — the old
  rule is a special case the new rule still accepts, so no existing row
  becomes newly invalid on rollback).

## 5. Regression tests

- **Flip `TestDBConstraints_T1_ComposedVoidCausation_SavepointRollbackIsRejected`
  to Accepted.** The test at `internal/sportsbook/settlement_db_constraints_integration_test.go:607-646`
  currently asserts `err != nil` with the same-transaction message
  (`:639-645`). Under the `pg_xact_status` fix this case must now
  **succeed** (`err == nil`), since the rollback row was inserted under a
  released savepoint in the *same* top-level transaction as the void. The
  test needs a new name (e.g.
  `TestDBConstraints_T1_ComposedVoidCausation_SavepointRollbackIsAccepted`)
  and updated doc comment: remove the "deliberately pinned as REJECTED...
  not fixed here" framing (`:592-606`) and replace with "SB-T1-XMIN
  resolved: pg_xact_status classifies a released-savepoint row as
  belonging to the current transaction tree; this is now the desired,
  legitimate composed-void shape a savepoint-taking driver produces."
- **Keep `TestDBConstraints_T1_ComposedVoidCausation_RejectsEarlierTransactionRollback`
  rejected** (`:522-544`) — unaffected by the fix: that rollback row is
  `committed` (not `in progress`) from the later transaction's snapshot,
  so `pg_xact_status` still returns `'committed'` and the branch still
  raises. No change needed beyond re-confirming (already empirically
  verified above with the cross-transaction control case).
- **Keep `TestDBConstraints_T1_ComposedVoidCausation_AcceptsSameTransaction`
  passing as-is** (`:552-580`) — the plain-insert-in-top-level-tx case is
  the trivial "in progress, same transaction" case for either
  implementation; no behavior change.
- **Add: nested-savepoint composed void.** New test mirroring
  `_SavepointRollbackIsRejected`'s structure but nesting an inner
  savepoint around the rollback insert (`SAVEPOINT sp_outer; SAVEPOINT
  sp_inner; INSERT rollback; RELEASE sp_inner; RELEASE sp_outer;`), then
  the void insert on the outer transaction, asserting **acceptance**
  under the fix — pins the "however nested" part of the `pg_xact_status`
  claim (empirically confirmed above: nested-outer and nested-inner rows
  both report `in progress`).
- **Add: rolled-back-savepoint rollback row.** A test that opens a
  savepoint, inserts the rollback row, then issues `ROLLBACK TO SAVEPOINT`
  (not `RELEASE`) before attempting the void insert referencing that now-
  gone row's id. Confirmed above: the row genuinely ceases to exist after
  `ROLLBACK TO SAVEPOINT` (0 rows on re-select in the same transaction),
  so the void insert's FK (`causation_record_id REFERENCES
  sportsbook_bet_settlements(id)`) fails with a foreign-key violation —
  **before** T-1 even runs (FK enforcement and BEFORE ROW triggers both
  fire pre-insert on the referencing table, but the FK on
  `causation_record_id` is checked as a constraint trigger; either way
  T-1's own `SELECT ... INTO cause` would simply find nothing and hit the
  pre-existing `cause.id IS NULL` branch, same message). Assert the
  error is the FK-violation shape (or T-1's `cause.id IS NULL` message,
  whichever fires first in practice — confirm empirically at
  implementation time) rather than the xmin-specific message, so this
  test does not silently start asserting the wrong branch.
- **Add: service-level test driving `SimulateSettlementEvent`'s composed
  void inside an outer savepoint via a test-only wrapper.** Feasible: add
  a test helper that calls `pool.WithTenant(ctx, tenantID, func(ctx, tx)
  error { spTx, _ := tx.Begin(ctx); defer spTx.Rollback(ctx); err :=
  sportsbook.SimulateSettlementEvent(ctx, spTx, voidAfterSettlementEvent);
  ... ; return spTx.Commit(ctx) })` — i.e. wrap the *whole*
  `SimulateSettlementEvent` call (not just the history insert) in one
  `tx.Begin`-issued savepoint, then commit the savepoint before the outer
  transaction commits. Before the fix this must reproduce the failure
  end-to-end (not just at the raw-SQL level); after the fix it must
  succeed end-to-end, proving the DB fix actually unblocks a real,
  savepoint-wrapping caller shape, not just the raw-SQL probe. This
  directly retires the "not reachable in practice" caveat for any driver
  that does eventually wrap the call this way, and is a reasonable
  low-cost addition since `SimulateSettlementEvent` already accepts a
  `pgx.Tx` parameter (no product code change needed to write this test —
  purely additive).
- **SQL branch checklist** (`docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md:267`):
  the row "causation (composed void, same-transaction xmin rule)" must be
  rewritten to reflect the new `pg_xact_status` mechanism and updated test
  names: replace `_SavepointRollbackIsRejected` with
  `_SavepointRollbackIsAccepted` (documents the SB-T1-XMIN fix, not the
  precondition) and add the new nested-savepoint and rolled-back-savepoint
  rows as separate pinned branches. The task-registry row
  (`docs/governance/task-registry.md:3795`, `SB-T1-XMIN | ... | Deferred`)
  moves from "Deferred" to "Done" with the new migration/commit ids once
  implemented, and `docs/decisions/0088-...md`'s 2026-09-25 implementation
  note (§3.3, lines ~304-332) gains a follow-up note (not an edit of the
  existing note — the existing note correctly describes the state as of
  that date) recording that `SB-T1-XMIN` was resolved by migration
  0093 (or whatever number lands), replacing the xmin check with
  `pg_xact_status`.

## 6. Human decision required

**None, strictly.** This is ordinary, reversible engineering work inside
already-approved architecture:
- The fix does not touch `player_locked_*` semantics (no `ledger-finance`
  sign-off gate beyond routine review of the migration, which ADR 0088
  already assigns as `ledger-finance`'s drafting ownership of this exact
  trigger).
- It does not change ledger postings, transaction types, idempotency
  keys, or any externally observable settlement behavior on the
  currently-reachable path (the precondition holds today, so the fix is
  invisible to all existing callers).
- It was already anticipated and explicitly deferred, not newly
  discovered, by ADR 0088 itself (`SB-T1-XMIN`) and by the independent
  code review (finding 1) and is already tracked in
  `docs/governance/task-registry.md` as an owned, scoped item awaiting
  only implementation, not a decision.
- The only genuinely open item requiring a **human** call is the
  pre-existing, unrelated **PAY-REV-1** authorization to open "Stage
  10.1" as a stage at all (`docs/governance/stage-10-completion-
  report.md` item 15 / `docs/active-stage.md:1500`) — that gate is
  already flagged elsewhere in the project's own governance trail and is
  not specific to SB-T1-XMIN. If the orchestrator has already obtained
  that stage authorization, SB-T1-XMIN can be implemented within it
  without a further separate human decision. If not, implementation of
  SB-T1-XMIN should wait for that same stage-open authorization (bundling
  it with PAY-REV-1's migration as task-registry already plans), rather
  than being fast-tracked alone outside an authorized stage.

## Appendix — commands run (for auditability of the empirical claims above)

```
sudo -u postgres pg_ctlcluster 16 main start
sudo -u postgres psql -c "CREATE DATABASE xmin_scratch OWNER igaming;"
sudo -u postgres psql -d xmin_scratch -c "CREATE TABLE probe (id serial primary key, note text);"
# ... BEGIN; plain/savepoint/nested/rollback-to-savepoint inserts; pg_xact_status checks; ROLLBACK; ...
# ... committed-earlier row + pg_xact_status = 'committed' control ...
sudo -u postgres psql -c "DROP DATABASE xmin_scratch;"
```

No repository file, migration, or CI/production/synthetic-shared database
was modified. `xmin_scratch` was created and dropped entirely within this
session on the local dev Postgres cluster.
