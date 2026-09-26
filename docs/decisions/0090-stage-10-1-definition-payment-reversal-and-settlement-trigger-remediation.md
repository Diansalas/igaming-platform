# ADR 0090 — Stage 10.1 Definition: PAY-REV-1 and SB-T1-XMIN Remediation

- **Status:** **ACCEPTED** — approved by the human on 2026-09-26 against
  planning-gate commit `8561ac2d531995625fc9450c628eaa1a1c2347dd`, **with one
  scope change: PAY-WH-TENANT-1 is ADDED to Stage 10.1** (see "Human
  approval and scope amendment" below).
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

## Human approval and scope amendment (2026-09-26)

The human approved Stage 10.1 exactly as defined in the planning report and
accepted this ADR, and **added PAY-WH-TENANT-1** (planning report §K/§R.2;
security finding S-6) as a third, independent workstream. Stage 10.1 is
therefore:

1. PAY-REV-1 (as above);
2. SB-T1-XMIN (as above);
3. **PAY-WH-TENANT-1** — the payments webhook must cryptographically bind
   the callback to its tenant: the tenant must not be selectable solely
   from the untrusted URL path; provider and tenant identity are resolved
   consistently from the verified credential; the signed payload covers the
   tenant binding; cross-tenant callbacks fail closed without leaking
   tenant/provider information; replay protection, idempotency and RLS stay
   intact; the mock provider signs under the same rule (it remains a MOCK,
   not a real PSP integration); the payments webhook contract is added to
   OpenAPI (API-DOC-PAYWH). Its design is reviewed by `payments`,
   `security`, `backend`, `architect`, database/RLS and `qa` before
   implementation and recorded in
   `docs/plans/stage-10.1-planning/11-pay-wh-tenant-1-design.md`.

Acceptance gates G1–G8 and the stop point before any AWS/staging deployment
are as stated in the human's approval.

## Out of scope

Report §U except PAY-WH-TENANT-1 and API-DOC-PAYWH (now in scope): LEDGER-REV-UNIQ, REV-UNIQ-CASINO, any AI
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

## Implementation record (2026-09-26)

Stage 10.1 was implemented and reviewed; details and evidence are in
`docs/governance/stage-10.1-completion-report.md`. Deviations from the
accepted text above, each reviewed and ratified:

1. **SB-T1-XMIN epoch anchor.** Decision item 2 specifies reconstructing
   the xid8 relative to `pg_snapshot_xmax(pg_current_snapshot())`. That
   construction was shown empirically to mis-place the current
   transaction's own open savepoint xids (they exceed the snapshot xmax,
   which counts only completed transactions) and so to reproduce the very
   rejection being fixed. Migration 0093 anchors the epoch to
   `pg_current_xact_id()` instead, keeping every fail-closed guard.
   Ratified by `architect` (`docs/governance/stage-10.1-architecture-review.md`
   §1) and `ledger-finance` (`docs/governance/stage-10.1-ledger-finance-signoff.md`,
   which withdrew its own planning construction). Residuals, both
   documented in ADR 0088 §3.3: an epoch-straddling transaction is
   rejected (fail closed; SB-T1-XMIN-STRADDLE, deferred P3); an ancient-row
   alias (≥ 2^32 xids, low-bit collision with an in-progress xid) can be
   accepted — provenance only, no ledger effect, same class as 0091.
2. **PAY-REV-1 classification.** `ledger.Post` looks up the idempotency key
   before classifying a violation of the 0092 index, so a legitimate retry
   replays regardless of index order (ledger-finance P2-A).
3. **PAY-REV-1 denial error.** `ErrDepositAlreadyReversed` is returned as
   the typed `DepositAlreadyReversedError` so the denial audit can name the
   deposit intent, original transaction, rejected reference and existing
   reversal (security P2-2 / ledger-finance P2-B).
4. **PAY-WH-TENANT-1 key-material scan** runs after signature verification
   (adapters never parse an unverified body — ADR 0022 §3 amendment point
   7); accepted by `security` (re-verification P3-7).
5. **Resolver wiring.** Production wiring uses `MultiWebhookCredentialResolver`
   composing the single MOCK resolver; ADR 0022 records it as mock/test
   wiring only, not a template for the real resolver.

