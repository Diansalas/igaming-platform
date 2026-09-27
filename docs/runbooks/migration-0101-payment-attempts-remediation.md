# Runbook: migration 0101 (`payment_attempts`) pre-flight remediation

Owner: `payments` + `ledger-finance`. Source: ledger-finance review
`docs/plans/payment-readiness/rv-prh-i1-payout-ledger.md`, "Ruling on
migration 0101's `succeeded_missing_link` pre-flight", and its re-review
("Runbook check", 5 corrections applied below). This runbook is required
reading before running `migrate up` against ANY database (staging, demo, or
a from-scratch environment seeded with imported/restored data) that has
never had migration 0101 applied.

## 1. What the pre-flight checks, and why it refuses rather than skips

Migration 0101 adds `payment_attempts`/`payment_provider_events` and
backfills historical `deposit_intents`/`withdrawal_requests` rows into
synthetic `legacy_backfill` attempts. Before writing anything, its backfill
block runs **three** pre-flight checks, once per tenant (see
`migrations/0101_payment_attempts.up.sql`'s `DO $$ ... $$` block, "Pass 1:
pre-flight" - that file is applied and checksum-verified and is the sole
authoritative source; this runbook only ever mirrors its predicates, never
the reverse):

1. **`pending_ambiguous_no_provider`** — a non-terminal (`pending` or
   `ambiguous`) `deposit_intents` row with `provider_reference IS NULL AND
   provider_id IS NULL`. Without either, the backfill cannot classify
   `ever_possibly_sent`.
2. **`succeeded_missing_link`** — a `deposit_intents` row with `status =
   'succeeded'` where **either** `ledger_transaction_id IS NULL` **or**
   `provider_reference IS NULL` (this is an OR, not an AND — a row missing
   only one of the two is just as unbackfillable as one missing both).
3. **`submitted_no_reference`** — a `withdrawal_requests` row with `state =
   'submitted'` and `provider_reference IS NULL`.

If any tenant has ANY row matching ANY of the three, the whole migration
raises and aborts with `Nothing was changed` — no partial migration, no
rows touched, no constraint added, across every tenant, not just the
offending one.

**This refusal is correct and must never be weakened, bypassed with a
flag, or silently skipped.** Each of the three shapes above means one of:

- the player/withdrawal was told "succeeded"/"submitted" and the platform
  cannot reconstruct enough to safely backfill (a real, historical
  financial-integrity gap), or
- the row is test/fixture data written by raw SQL, bypassing the
  application's own posting/transition path (see §2), or
- the row came from a data import/restore that didn't go through the
  platform's own posting path.

None of these can be resolved by loosening a CHECK, truncating history, or
"just letting the migration through" — every one of those options either
fabricates a row with no evidence, or silently drops a financial fact
reconciliation (and, for a genuine production incident, the player)
depends on. **There is no bypass flag by design.**

## 2. Is this reachable from real application traffic?

**No, not through the application, for pre-flight 2 (`succeeded_missing_link`).**
Since migration 0025, the *only* writer of `deposit_intents.status =
'succeeded'` is `(*Orchestrator).postDepositSuccess`
(`internal/payments/orchestrator.go`), which sets `provider_id`,
`provider_reference`, `status`, and `ledger_transaction_id` together, in
the *same* transaction as `ledger.Post`. `setIntentAttempt` (the
pending-transition path) never writes `succeeded`.

Pre-flight 1 (`pending_ambiguous_no_provider`) and pre-flight 3
(`submitted_no_reference`) are similarly unreachable through today's
application code: every path that moves a deposit intent to
`pending`/`ambiguous` sets a `provider_id` (routing always resolves one
before the first attempt), and every path that moves a withdrawal to
`submitted` (`withdrawal.MarkSubmitted`, the pre-cutover single-call path)
requires a non-empty `providerTxID` already in hand as a precondition.

So a row any of the three pre-flights rejects can only come from:

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

**A separate, PERMANENT consequence (not a pre-flight defect):** after
PRH-I1's payout dispatch cutover, `withdrawal.MarkSubmittedPending`
*legitimately* creates `submitted` withdrawal requests with a NULL
`provider_reference` (P95-C2). That is EXACTLY pre-flight 3's shape. See
§4 — this means 0101 can no longer be safely down-migrated and re-applied
on any database where the new payout dispatch code has actually run, even
though pre-flight 3 itself is correct and must not be weakened.

## 3. Procedure

Run this **before** `migrate up` on any target that has not yet run 0101,
whenever any of the three pre-flights is expected to (or does) refuse. RLS
is FORCE on every table below — every query in this section MUST run with
`app.tenant_id` set to the tenant being inspected (and `app.player_account_id`
cleared), exactly as migration 0101's own pre-flight does via
`PERFORM set_config('app.tenant_id', tenant_rec.id::text, true)`. **The
setting is `app.tenant_id`, never `app.current_tenant_id`** — using the
wrong setting name is exactly migration 0048's own lesson (restated in
0101's comments): FORCE RLS silently returns zero rows for an unrecognized
setting, so a operator would see a false "nothing to fix" and proceed
straight into the pre-flight's abort.

`payment_attempts` **does not exist** before 0101 has successfully applied
— none of the queries below may reference it. All of them run against
`deposit_intents` / `withdrawal_requests` / `ledger_transactions` /
`ledger_entries` only, which all pre-date 0101.

### (a) List the offending rows, per pre-flight

Run each of these against the target database, iterating every tenant
(`SELECT id FROM tenants`) and setting `app.tenant_id` before each — or, as
a superuser/owner role with BYPASSRLS for a one-off listing only (never for
step (b-i)'s write), drop the per-tenant loop. These mirror migration
0101's own pre-flight predicates **exactly**, including pre-flight 2's OR
(not AND):

```sql
-- Pre-flight 1: pending_ambiguous_no_provider
SELECT id, status, provider_id, provider_reference, amount, asset_code, created_at
FROM deposit_intents
WHERE status IN ('pending', 'ambiguous')
  AND provider_reference IS NULL
  AND provider_id IS NULL
ORDER BY created_at;

-- Pre-flight 2: succeeded_missing_link (OR, not AND - a row missing EITHER
-- the ledger link OR the provider reference is unbackfillable)
SELECT id, provider_id, provider_reference, ledger_transaction_id, amount, asset_code, created_at
FROM deposit_intents
WHERE status = 'succeeded'
  AND (ledger_transaction_id IS NULL OR provider_reference IS NULL)
ORDER BY created_at;

-- Pre-flight 3: submitted_no_reference (a WITHDRAWAL shape, not a deposit
-- intent shape)
SELECT id, provider_id, amount, asset_code, requested_at
FROM withdrawal_requests
WHERE state = 'submitted' AND provider_reference IS NULL
ORDER BY requested_at;
```

If migration `0101_payment_attempts.up.sql`'s pre-flight predicates have
since been revised, that file is the authoritative source — update these
queries to match it, never the other way around.

### (b) Pre-flight 2 (`succeeded_missing_link`): look for a matching, unlinked ledger posting

This pre-flight fires on a row missing its ledger link, its provider
reference, or both — handle each case:

**(b-i) `ledger_transaction_id IS NULL` (whether or not `provider_reference`
is also NULL):** look for a matching posting:

```sql
SELECT lt.id, lt.provider_id, lt.provider_tx_id, lt.correlation_id, lt.created_at,
       le.ledger_account_id, le.direction, le.amount
FROM ledger_transactions lt
JOIN ledger_entries le ON le.ledger_transaction_id = lt.id
WHERE lt.transaction_type = 'deposit'
  AND (lt.correlation_id = :deposit_intent_id
       OR (:provider_id IS NOT NULL AND :provider_reference IS NOT NULL
           AND lt.provider_id = :provider_id AND lt.provider_tx_id = :provider_reference));
```

- **Exactly one matching transaction, amount/asset agree with the intent's
  own `amount`/`asset_code`:** a lost-link case, not a missing-credit case.
  Proceed to the four-eyes script below, setting `ledger_transaction_id`
  (and `provider_reference`, from `lt.provider_tx_id`, if that was ALSO
  NULL — see b-ii).
- **No matching transaction exists:** a real reconciliation gap. Proceed to
  (c).
- **More than one plausible match, or amounts/assets disagree:** stop and
  treat as (c) — do not guess.

**(b-ii) `provider_reference IS NULL` but `ledger_transaction_id` IS
already set (the ledger link exists; only the reference is missing):**
attach the reference from the matched transaction's own `provider_tx_id` —
this is the case the earlier version of this runbook did not cover at all
and sent to "investigate" with no procedure:

```sql
UPDATE deposit_intents
SET provider_reference = :provider_tx_id  -- from ledger_transactions.provider_tx_id, matched via ledger_transaction_id
WHERE id = :deposit_intent_id AND provider_reference IS NULL;
```

**Four-eyes link-attach script (covers b-i and b-ii together).** Write a
script (not an ad hoc interactive `UPDATE`) that, for each confirmed match,
sets:

```sql
UPDATE deposit_intents
SET ledger_transaction_id = COALESCE(ledger_transaction_id, :ledger_transaction_id),
    provider_reference    = COALESCE(provider_reference, :provider_tx_id)
WHERE id = :deposit_intent_id;
```

`deposit_intents.ledger_transaction_id` has existed since migration 0025;
migration 0082's guard permits NULL → value on both of these
immutable-once-set columns and does **not** permit overwriting an existing
value — if a column is already set to something OTHER than the matched
value, this is not a lost-link case, stop and investigate why the
pre-flight still flagged it. This script:

- must be reviewed by a second person (four-eyes) before it runs against
  any non-scratch database, per CLAUDE.md's manual-financial-action rule;
- must write an `audit_log` entry per row (actor, tenant, entity,
  before/after, reason code `"migration_0101_remediation_link_attach"`);
- must be run and verified against a copy/snapshot first, never directly
  against the only copy of the data.

### (c) No matching posting exists (pre-flight 2), or pre-flight 1/3 with no PSP-confirmed outcome: treat as a P1 reconciliation incident

Do **not** fabricate a posting and do **not** silently reclassify the row.
Confirm the true outcome with the PSP/custodian (or the equivalent source
of truth for that provider) for every affected row, then:

- **Pre-flight 2, no matching posting:** either post the missing credit
  through the **normal** deposit path (a fresh, dated, audited compensating
  entry — never a backdated raw insert), or move the intent to
  `deposit_intents.status = 'failed'`. **Never `disputed`** —
  `deposit_intents.status`'s CHECK constraint allows only `pending |
  succeeded | declined | ambiguous | failed`; `disputed` is a
  `payment_attempts.state` value (a table that does not exist yet, on the
  very database this pre-flight is blocking), never a valid
  `deposit_intents` status. Record the PSP confirmation and the reason in
  the same audited script as (b).
- **Pre-flight 1 (`pending_ambiguous_no_provider`):** confirm with the PSP
  whether the deposit was ever actually routed. If yes, backfill
  `provider_id`/`provider_reference` from the PSP's own record (four-eyes,
  audited, same rules as (b)). If genuinely never routed, move the intent
  to `status = 'failed'`.
- **Pre-flight 3 (`submitted_no_reference`, a WITHDRAWAL row):** confirm
  with the PSP/custodian whether the payout instruction was ever actually
  sent. If yes, backfill `withdrawal_requests.provider_reference` from the
  PSP's own record. If genuinely never sent, this row should transition
  through the normal withdrawal state machine (`withdrawal.Fail`, or the
  payout-dispatch NotSent path) rather than being hand-patched to satisfy
  the pre-flight.

This is the same class of incident CLAUDE.md's ledger rules already
require a P1 response to ("any non-zero drift is a P1 incident") — it is
not specific to this migration, this migration has simply surfaced it.

### (d) Test and scratch databases: drop and recreate, never hand-patch

For any database whose sole purpose is running this repository's test
suite (the shared `TEST_DATABASE_URL` database, any `scratchdb`-created
database, any developer's local dev database seeded only with synthetic
fixtures): **do not** run (a)–(c) against it. Drop it and let the normal
migration/fixture setup recreate it from empty. Hand-patching a test
database's offending rows to satisfy any of the three pre-flights risks
masking a genuine bug in a fixture or in application code, and provides no
audit value once the database is disposable anyway.

As of this fix round, the repository's own shared CI/dev database
(`igaming_platform_ci_local`, `TEST_DATABASE_URL`) has ~198 such rows
accumulated from historical test runs across ~180 tenants (pre-flight 2)
and cannot have migration 0101 applied in place. It should be rebuilt from
empty (or a freshly-migrated scratch/parallel database used instead,
exactly as `internal/payments/deposit_v2_integration_test.go`'s
`depositV2ScratchPool` and this fix round's own test files already do),
not repaired.

## 4. A related, permanent consequence: 0101 is no longer re-appliable (down, then up) once payout dispatch has run

After PRH-I1's payout dispatch cutover, `withdrawal.MarkSubmittedPending`
legitimately creates `submitted` withdrawal requests with a NULL
`provider_reference` (P95-C2, `docs/architecture/withdrawal-state-
machine.md` §2) — that is **exactly** pre-flight 3's own shape
(`submitted_no_reference`), and the underlying reasoning (a
`submitted`/dispatched money-movement row with no way to look it up at the
provider) is the same one pre-flight 3 exists to catch on a fresh
migration.

**Consequence:** on any database where the new payout dispatch code has
run for real (not merely migrated), migration 0101 cannot be safely
down-migrated and re-applied — a `down` that removes `payment_attempts`
followed by an `up` that re-runs the pre-flight would encounter this
legitimate, expected shape and abort, or (worse, if the pre-flight were
ever weakened to let it through) misclassify live in-flight payouts.

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
- It does not add a bypass flag to any of the three pre-flights. There
  isn't one, by design (§1).
- It is not a substitute for `ledger-finance` sign-off on any (b)/(c)
  remediation script before it runs against real data.
