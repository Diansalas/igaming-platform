# ADR 0090 — Stage 10.1 Definition: PAY-REV-1 and SB-T1-XMIN Remediation

- **Status:** **PROPOSED** — becomes ACCEPTED only on explicit human approval of
  `docs/plans/stage-10.1-planning-gate-proposal.md` (planning gate G0).
- **Decision type:** stage definition + short implementation contract
  (precedent: ADR 0087; the stage is small enough not to need a separate
  contract ADR — architect ruling).
- **Owner:** Master Orchestrator. Implementers: `payments` (PAY-REV-1),
  `ledger-finance` (ledger `Post` constraint routing, migration 0092 review),
  `sportsbook` (SB-T1-XMIN). Reviewers: `security`, `code-reviewer`, `qa`,
  `architect`.

> Numbering note: this is ADR 0090 in `docs/decisions/`. Migration
> `0090_sb_jurisdiction_*` in `migrations/` is an unrelated file.

## Context

Stage 10 (ADR 0087/0088) completed at `20c72e4` and was accepted by the
human. Its completion report carried two items: **PAY-REV-1** (P1,
pre-existing since Stage 3B: concurrent payment deposit reversals of one
deposit under different references both post) and **SB-T1-XMIN** (deferred:
migration 0091's T-1 composed-void causation check rejects a rollback row
inserted inside a savepoint). The human authorized a planning gate only.

## Decision (effective on approval)

Stage 10.1 implements exactly the scope of the planning report §T:

1. **PAY-REV-1.** In `receiveDepositReversalCallback`: an ADR 0082 **L2**
   `SELECT … FOR UPDATE` on the original deposit's `ledger_transactions` row
   (tenant predicate; a missing row or wrong type fails closed, never a
   tombstone), then the already-reversed check re-run after the lock, then
   L3/L4 as today (sequence S0–S7, report §E). Migration **0092**: partial
   unique index `(tenant_id, reverses_transaction_id) WHERE
   transaction_type = 'deposit_reversal'` (INV-PAY-REV-1), created inside a
   `DO … EXCEPTION WHEN unique_violation` refusal (RLS-proof; no `SELECT`
   pre-check, no `CONCURRENTLY`). `ledger.Post` routes a violation of that
   index by constraint name to a new `ErrReversalAlreadyExists`.
   `ErrDepositAlreadyReversed` maps to HTTP 409 with a generic body, an
   allow-listed alert, and a `deposit.reversal_rejected` audit committed in
   a separate tenant-scoped transaction.
2. **SB-T1-XMIN.** Migration **0093**: body-only `CREATE OR REPLACE` of
   `sportsbook_bet_settlements_validate()` (SECURITY INVOKER kept; every
   other branch byte-identical to 0091) replacing the xmin-equality check
   with a fail-closed `pg_xact_status` check (xid8 reconstructed relative to
   `pg_snapshot_xmax(pg_current_snapshot())`; accept only if ≥
   `pg_current_xact_id()` and status is `'in progress'`; NULL/error ⇒
   reject). Down restores 0091's body verbatim.
3. Tests, records and gates per report §J, §T.4, §V, §W.

## Out of scope

Report §U, notably **PAY-WH-TENANT-1** (payments webhook tenant not bound
to the verifying credential; launch-blocking for a real PSP; pending the
human ruling in report §R.2), LEDGER-REV-UNIQ, REV-UNIQ-CASINO, any AI
implementation (ADR 0089), any AWS/staging change, OB-1.

## Consequences

- ADR 0082 gains Amendment A5 (inventory only: the payments row gains an L2
  lock; classes and rules unchanged).
- ADR 0020 gains an amendment: semantic-duplicate callbacks (distinct
  references for one financial event) are not resolved by the idempotency
  key; any future `withdrawal_reversed`/`bonus_reversal` poster must add an
  L2 lock and a type-scoped unique index.
- ADR 0088 §3.3 gains a follow-up note closing SB-T1-XMIN.
- Human decisions that remain OPEN are untouched (report §Q).
