# Runbook: migration 0101 (`payment_attempts`) pre-flight remediation

Owner: `payments` + `ledger-finance`. Source: ledger-finance review
`docs/plans/payment-readiness/rv-prh-i1-payout-ledger.md`, "Ruling on
migration 0101's `succeeded_missing_link` pre-flight". This runbook is
required reading before running `migrate up` against ANY database (staging,
demo, or a from-scratch environment seeded with imported/restored data)
that has never had migration 0101 applied.

## 1. What the pre-flight checks, and why it refuses rather than skips

Migration 0101 adds `payment_attempts`/`payment_provider_events` and
backfills historical `deposit_intents` rows into synthetic `legacy_backfill`
attempts. Its pre-flight step scans for `deposit_intents` rows with
`status = 'succeeded'` that have **no ledger link it can backfill from**
(`succeeded_missing_link`). If it finds any, it raises and aborts with
`Nothing was changed` — no partial migration, no rows touched, no
constraint added.

**This refusal is correct and must never be weakened, bypassed with a
flag, or silently skipped.** A `succeeded` deposit intent with no provider
reference and no ledger transaction link means one of:

- the player was told "deposited" and no ledger credit exists (a real,
  historical missing-credit incident), or
- the ledger credit exists but the link between it and the intent was lost
  (a real data-integrity defect), or
- the row is test/fixture data written by raw SQL (`status='succeeded'`
  set directly, bypassing `postDepositSuccess`, the only application code
  path that can legitimately produce this shape — see §2), or
- the row came from a data import/restore that didn't go through the
  platform's own posting path.

None of these can be resolved by loosening the CHECK, truncating the
history, or "just letting the migration through" — every one of those
options either fabricates a `succeeded` attempt row with no evidence, or
silently drops a financial fact reconciliation (and, for a genuine
production incident, the player) depends on. **There is no bypass flag by
design.**

## 2. Is this reachable from real application traffic?

**No, not through the application.** Since migration 0025, the *only*
writer of `deposit_intents.status = 'succeeded'` is
`(*Orchestrator).postDepositSuccess` (`internal/payments/orchestrator.go`),
which sets `provider_id`, `provider_reference`, `status`, and
`ledger_transaction_id` together, in the *same* transaction as
`ledger.Post`. `setIntentAttempt` (the pending-transition path) never
writes `succeeded`. So a row the pre-flight rejects can only come from:

1. **Test fixtures** — e.g. `payrev1_tenant_isolation_integration_test.go`
   sets `status='succeeded'` directly via raw SQL, bypassing the posting
   path entirely, specifically to exercise other invariants. This is the
   overwhelming majority case for any database integration tests have run
   against, including the shared `TEST_DATABASE_URL` database in this
   repository's own CI/dev setup.
2. **A manual DBA "fix"** applied during a past incident, done via raw SQL
   instead of the platform's own compensating-entry mechanism.
3. **A data import/restore** from outside the platform.
4. **An undiscovered historical application bug** (not currently known to
   exist, and not reproducible through today's code path).

Every one of these is either non-production synthetic data (case 1, the
common case pre-launch) or an existing financial-integrity incident (cases
2–4) that predates this migration and that 0101 has simply made visible.
No production environment exists yet for this platform (CLAUDE.md:
"Development uses local/dev environments, mock and sandbox providers, and
synthetic data only") — so case 1 is expected to be the *only* case
encountered before go-live, and cases 2–4 apply only if this platform is
ever pointed at externally-sourced data.

## 3. Procedure

Run this **before** `migrate up` on any target that has not yet run 0101,
whenever the pre-flight is expected to (or does) refuse.

### (a) List the offending rows

The pre-flight itself reports up to 20 ids per tenant when it refuses; for
the full list, run this against the target database as a role that can see
every tenant (a superuser/owner role, or by iterating `SET
app.current_tenant_id` per tenant under RLS):

```sql
SELECT di.tenant_id, di.id AS deposit_intent_id, di.provider_id, di.provider_reference,
       di.amount, di.asset_code, di.created_at
FROM deposit_intents di
WHERE di.status = 'succeeded'
  AND NOT EXISTS (
    SELECT 1 FROM payment_attempts pa
    WHERE pa.deposit_intent_id = di.id AND pa.state = 'succeeded'
  )
ORDER BY di.tenant_id, di.created_at;
```

(This mirrors the pre-flight's own query — see migration
`0101_payment_attempts.up.sql`'s pre-flight step for the authoritative
predicate if it has since been revised; that file is applied and
checksum-verified, and must never be edited to make this runbook's copy
"more correct" instead.)

### (b) For each row: look for a matching, unlinked ledger posting

For each `(tenant_id, deposit_intent_id)`:

```sql
SELECT lt.id, lt.provider_id, lt.provider_tx_id, lt.correlation_id, lt.created_at,
       le.ledger_account_id, le.direction, le.amount
FROM ledger_transactions lt
JOIN ledger_entries le ON le.ledger_transaction_id = lt.id
WHERE lt.transaction_type = 'deposit'
  AND (lt.correlation_id = :deposit_intent_id
       OR (lt.provider_id = :provider_id AND lt.provider_tx_id = :provider_reference));
```

- **If exactly one matching transaction exists, and its amount/asset agree
  with the intent's own `amount`/`asset_code`:** this is a lost-link case,
  not a missing-credit case. Proceed to (b-i).
- **If no matching transaction exists:** this is a real reconciliation gap.
  Proceed to (c).
- **If more than one plausible match exists, or the amounts/assets
  disagree:** stop and treat as (c) — do not guess.

**(b-i) Reviewed, audited, four-eyes link-attach.** Write a script (not an
ad hoc interactive `UPDATE`) that, for each confirmed match, sets:

```sql
UPDATE deposit_intents
SET ledger_transaction_id = :ledger_transaction_id  -- if this column exists on your checkout
WHERE id = :deposit_intent_id AND ledger_transaction_id IS NULL;
```

(Migration 0082's guard permits NULL → value on an immutable-once-set
column; it does **not** permit overwriting an existing value — if the
column is already set, this is not a lost-link case, investigate why the
pre-flight still flagged it.) This script:

- must be reviewed by a second person (four-eyes) before it runs against
  any non-scratch database, per CLAUDE.md's manual-financial-action rule;
- must write an `audit_log` entry per row (actor, tenant, entity,
  before/after, reason code `"migration_0101_remediation_link_attach"`);
- must be run and verified against a copy/snapshot first, never directly
  against the only copy of the data.

### (c) No matching posting exists: treat as a P1 reconciliation incident

Do **not** fabricate a posting and do **not** silently reclassify the row.
Confirm the true outcome with the PSP (or the equivalent source of truth
for that provider), then either:

- post the missing credit through the **normal** deposit path (a fresh,
  dated, audited compensating entry — never a backdated raw insert), or
- move the intent to a non-`succeeded` status with its own evidence trail
  (e.g. `disputed`), recording why.

This is the same class of incident CLAUDE.md's ledger rules already
require a P1 response to ("any non-zero drift is a P1 incident") — it is
not specific to this migration, this migration has simply surfaced it.

### (d) Test and scratch databases: drop and recreate, never hand-patch

For any database whose sole purpose is running this repository's test
suite (the shared `TEST_DATABASE_URL` database, any `scratchdb`-created
database, any developer's local dev database seeded only with synthetic
fixtures): **do not** run (a)–(c) against it. Drop it and let the normal
migration/fixture setup recreate it from empty. Hand-patching a test
database's `succeeded_missing_link` rows to satisfy the pre-flight risks
masking a genuine bug in a fixture or in application code, and provides no
audit value once the database is disposable anyway.

As of this fix round, the repository's own shared CI/dev database
(`igaming_platform_ci_local`, `TEST_DATABASE_URL`) has ~198 such rows
accumulated from historical test runs across ~180 tenants and cannot have
migration 0101 applied in place. It should be rebuilt from empty (or a
freshly-migrated scratch/parallel database used instead, exactly as
`internal/payments/deposit_v2_integration_test.go`'s
`depositV2ScratchPool` and this fix round's own test files already do),
not repaired.

## 4. A related, permanent consequence: 0101 is no longer re-appliable (down, then up) once payout dispatch has run

After PRH-I1's payout dispatch cutover, `withdrawal.MarkSubmittedPending`
legitimately creates `submitted` withdrawal requests with a NULL
`provider_reference` (P95-C2, `docs/architecture/withdrawal-state-
machine.md` §2) — that is exactly the shape migration 0101's pre-flight
rejects for a *deposit* intent, and the underlying reasoning (a
`succeeded`/dispatched money-movement row with no way to look it up at the
provider) is the same on the payout side once dispatch has actually run.

**Consequence:** on any database where the new payout dispatch code has
run for real (not merely migrated), migration 0101 cannot be safely
down-migrated and re-applied — a `down` that removes `payment_attempts`
followed by an `up` that re-runs the pre-flight would encounter this
legitimate, expected shape and could misclassify live in-flight payouts.

This is accepted as-is: down-migrating a payments table in any environment
with live/real traffic is already forbidden by this platform's own
migration-safety rules (expand → migrate → contract, `docs/architecture/
38-deployment-architecture.md`) — 0101 is not a special case, this section
exists only to make the interaction explicit so nobody attempts a down/up
cycle on such a database expecting it to behave like a fresh one.

## 5. What this runbook does not do

- It does not modify `migrations/0101_payment_attempts.up.sql` or
  `.down.sql` — both are applied in places already and must remain
  byte-identical (migration checksum verification would otherwise report
  drift).
- It does not add a bypass flag to the pre-flight. There isn't one, by
  design (§1).
- It is not a substitute for `ledger-finance` sign-off on any (b-i)/(c)
  remediation script before it runs against real data.
