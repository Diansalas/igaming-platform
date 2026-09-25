> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# Security review — Stage 10.1 plan: PAY-REV-1 + SB-T1-XMIN (planning gate)

Reviewer: security. Code verified at HEAD 6e25d24 (no diff vs 56f5135 in
internal/payments, deposit_handlers.go, migrations). Planning-only; no repo edits.

## Verdict
**APPROVE WITH REQUIRED CHANGES.** Both designs are sound: the L2 `FOR UPDATE` +
post-lock re-check for PAY-REV-1, and `pg_xact_status` for SB-T1-XMIN. Four plan
changes are required before implementation starts (S-1..S-4). None of them needs
a human decision. No finding blocks launch *from this plan*. S-6 is a
pre-existing issue that does block launch for any real PSP; it is recorded here
and is not part of 10.1.

## Findings — PAY-REV-1

**S-1 (Medium, REQUIRED) — Unique index must be tenant-scoped.** The plan proposes
`UNIQUE (reverses_transaction_id) WHERE transaction_type='deposit_reversal'`,
which is global across tenants. The repo's own rule in 0021 says a
platform-global unique key on an RLS table is "a cross-tenant collision and
existence-oracle risk". It also matters here because
`reverses_transaction_id REFERENCES ledger_transactions(id)` is a
single-column FK, and FK checks ignore RLS. A buggy future writer in tenant B
that names tenant A's tx id would pass the FK. It would then hit a
unique_violation whose DETAIL echoes the key, which confirms that A's reversal
exists.
Required: `CREATE UNIQUE INDEX ... ON ledger_transactions (tenant_id,
reverses_transaction_id) WHERE transaction_type='deposit_reversal' AND
reverses_transaction_id IS NOT NULL`. Apply the same shape to the optional
casino_rollback sibling. Record the non-tenant-composite
`reverses_transaction_id` FK as a deferred hardening item. Do not fix it in 10.1.

**S-2 (Medium, REQUIRED) — Migration pre-check mechanism in §5.2 is wrong for this repo.**
Migrations run as `igaming`, which is NOSUPERUSER/NOBYPASSRLS (deploy/init-app-role.sql;
0030 header). Under that role, option (a) BYPASSRLS does not exist.
`SET row_security = off` raises an error on a FORCE-RLS table. Option (b), a
per-tenant loop, depends on enumerating tenants from another RLS-scoped source
and misses rows whose tenant has been deleted or suspended.
Required: use the repo's RLS-blind precedent from the 0091 down migration
(constraint/index validation). Wrap the `CREATE UNIQUE INDEX` in
`DO $$ BEGIN ... EXCEPTION WHEN unique_violation THEN RAISE EXCEPTION
'migration 0092: duplicate deposit_reversal ... (PAY-REV-1); resolve via
reviewed compensating entry'; END $$`. The index build ignores RLS, so it is the
check. A `count(*)` pre-scan must never be the gate. The refusal text may carry
ids/counts but no amounts, player ids or references. Test 7 must seed the
duplicate across **two tenants** and run as the real NOBYPASSRLS migration
role. A single-tenant seed would not prove that the check is RLS-blind.

**S-3 (Medium, REQUIRED) — Denied-reversal audit as planned would be silently lost.**
§6.1 adds `audit.Record(OutcomeDenied)` inside `receiveDepositReversalCallback`
and then returns `ErrDepositAlreadyReversed`. `db.Pool.WithTenant`
(internal/db/tenant_rls.go:45-46) rolls back on any error, which would discard
the audit row. A test that checks the audit inside the same tx would pass
anyway, so the bug would go unnoticed.
Required: the handler writes the denial audit in a **separate**
`deps.DB.WithTenant(t.ID)` tx after the rollback. Use ActorSystem, the tenant
from the path, action `deposit.reversal_rejected_already_reversed`, target =
the original deposit_intent, and metadata {provider_id,
rejected reversal_provider_reference}. Leave out the winner's reference. The
alternative is a non-error result that commits. Test 9 must check the audit
row **after commit, from a fresh tx**. If that separate audit write fails, the
handler logs an error and still returns 409; the financial outcome does not
change.

**S-4 (Low, REQUIRED) — Lock query details.** In the L2 lock:
(a) add an explicit `tenant_id = $tenant` predicate (defence in depth,
matching casino.postRollback); RLS on `FOR UPDATE` uses the FOR ALL policy,
which is correct but should not be the only guard.
(b) `ErrNoRows` on the lock means the deposit_intent points at an invisible or
missing tx. This must fail closed with an integrity error. It must **never**
fall into the tombstone branch.
(c) The lock must stay after `HandleCallback` signature verification, which it
does today. Unauthenticated input must never take a row lock.
(d) Note: `FOR UPDATE` needs UPDATE privilege. If UPDATE is ever REVOKEd on
`ledger_transactions` (as was done for sportsbook_bet_settlements), both this
lock and the casino lock fail closed. Record this in the ADR note.

**S-5 (Low, ACCEPT plan) — HTTP mapping and client leakage.** 500 → 409 for
`ErrDepositAlreadyReversed` is correct. The body must be the generic
`"callback rejected"` (same as payload mismatch). Do not use a descriptive
message. The alert log line carries provider_id, tenant_id and request_id
only. Do not log `err`, references or amounts; the reference goes in the audit
row. Only a signature-verified caller can see 404 vs 409, so no new
unauthenticated oracle appears. Pre-existing, noted only:
`ErrCallbackProviderMismatch` falls through to a 500 and logs `err`, which
contains both amounts (orchestrator.go:965-970). Clients get nothing, but
amounts end up in ops logs. Recommend mapping it to 409 and dropping the
amounts from the log (optional, in scope).

**S-6 (High, PRE-EXISTING, NOT 10.1 scope — LAUNCH-BLOCKING for any real PSP).**
Tenant comes from the **URL slug**, not from the verifying credential. The
mock uses one global HMAC secret, and the signed fields
(mock.go:207-213) do not include tenant. A callback validly signed for tenant A
therefore verifies when POSTed to tenant B's slug. For a reversal whose
original is not visible under B, this writes a **tombstone in tenant B**
(orchestrator.go:937), which is a cross-tenant replay with write effect. The
impact is negligible today (mock only, UUID references). This is ADR 0022 §3's
open decision, and the ReceiveCallback doc comment already concedes it.
Required before any real adapter: per-tenant (or per-merchant-account)
verification keys resolved from the path tenant, with the tenant bound into the
verified material. PAY-REV-1 must not claim "tenant from verified credential".
The completion report should state "tenant from path slug; credential not yet
tenant-bound".

**S-7 (Info, ACCEPT) — One-reversal-slot DoS.** To pre-occupy the slot, a caller
needs a validly signed reversal that names the original, which is the PSP
asserting that the deposit is reversed. Given a signed PSP, that is a true
fact, so the risk is acceptable. The fix also turns what used to be a second
debit into a rejection, so it reduces the attack surface. The residual risk is
key compromise, which is covered by the payload-mismatch alert and S-6.
Tombstone pre-occupation of a *not yet seen* original is by-design behaviour
from before this change (Flow 2), and S-6 amplifies it.

**Confirmed OK:** the tenant-scoped `tx` wraps the whole reversal path. The
original is resolved only through RLS-scoped `deposit_intents`. Locking one row
per original cannot deadlock (§3.4 checked). Rolling back the code while
keeping the index is safe. The deny-mutation trigger does not fire on
`FOR UPDATE`.

## Findings — SB-T1-XMIN

**X-1 (ACCEPT) — pg_xact_status keeps the provenance guarantee.** A row's `xmin` is
written by the server. The client cannot set it and cannot rewrite it here:
UPDATE/DELETE/TRUNCATE are blocked for every role, the owner included
(`sportsbook_bet_settlements_immutable`, 0091:126-132). `FOR UPDATE`/`SHARE`
changes xmax, not xmin. The check reads through the trigger's MVCC snapshot.
Rows from a concurrent uncommitted (or prepared) transaction are invisible, so
they hit `cause.id IS NULL`. Rows from a committed transaction report
`committed`. So `'in progress'` on a visible row means the checking
transaction's own tree, and nothing else. The change only widens acceptance to
released savepoints of the same top-level transaction. That is the intended
composed-void semantics, not a new trust path. The rejection of the GUC-marker
alternative (c) is correct: a client can assert that marker, and nothing in the
DB ties it to how the row came to exist.

**X-2 (Medium, REQUIRED) — Fail closed on NULL and epoch edge cases.**
`pg_xact_status` returns NULL when the xid is older than the retained clog, and
it errors on a "future" xid. Write the predicate as
`pg_xact_status(x) IS DISTINCT FROM 'in progress'` → RAISE. Never use
`<> 'in progress'`, because NULL would make the IF false and let the insert
through. Construct the epoch as the current xid8's epoch, minus 1 when
`xmin > low32(current)`. Wrap it in `BEGIN ... EXCEPTION WHEN OTHERS THEN RAISE
<same T-1 message>` so an arithmetic mistake cannot turn into acceptance. Add a
unit test that uses a synthetic too-old or future xid8 (or a mocked helper) to
check that NULL and error both reject.

**X-3 (Low, REQUIRED) — Function hygiene in the new migration.** Keep
SECURITY INVOKER; do not add SECURITY DEFINER. Leave T-1's other branches
byte-identical, and apply the 0091 diff check to both the up body (except the
causation branch) and the down body. Do not add `SET search_path` unless 0091
has it, so the two versions stay equivalent. `pg_xact_status` is EXECUTE-to-PUBLIC
by default, so no grant is needed; confirm no migration REVOKEs it. The test for
a savepoint that was rolled back must assert rejection by FK or `cause.id IS
NULL`, and must never assert acceptance.

## Required plan changes (summary)
1. S-1: make the index `(tenant_id, reverses_transaction_id)`, partial on deposit_reversal (and on casino_rollback if taken).
2. S-2: replace the §5.2 options with a DO-block-wrapped CREATE UNIQUE INDEX that catches unique_violation, and test it cross-tenant as the NOBYPASSRLS migration role.
3. S-3: write the denial audit in a separate committed tx, and test it from a fresh tx.
4. S-4: add the tenant predicate to the lock; make ErrNoRows fail closed, not tombstone; take the lock after signature verification.
5. S-5: 409 with a generic body; no references, amounts or err text in the alert log.
6. X-2 / X-3: `IS DISTINCT FROM 'in progress'`, exception-to-reject, epoch-1 edge, SECURITY INVOKER, NULL/error tests.
7. Record S-6 as a pre-existing launch blocker for real PSPs in the stage report and risk list. Do not fix it in 10.1.

## Scope of this review
Covered: plan text, orchestrator.go reversal and callback paths, deposit_handlers.go
webhook mapping, the mock signature scheme, WithTenant semantics, and 0021 RLS/index/FK.
Also 0091 T-1, the immutability triggers and down-migration technique, and runtime/migration role attributes.
Not covered: code for the fix (none exists yet), real PSP adapters, rate-limiting or
volumetric webhook DoS, the simulation route beyond confirming it cannot emit a
reversal, and running pg_xact_status NULL/epoch behaviour myself (relied on PG
docs and the plan's PG 16.13 probe). A post-implementation security review of the
diff is mandatory before either item is marked IMPLEMENTED.
