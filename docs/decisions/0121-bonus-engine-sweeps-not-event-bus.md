# ADR 0121 - Bonus engine uses ledger-tailing sweeps, not event-bus subscriptions (recorded deviation)

- **Status:** ACCEPTED (2026-10-15). Record of a **deliberate deviation** from the event-bus subscription model that the
  Blueprint describes for the bonus engine. It documents what is built; it changes no code and no schema.
- **Decision type:** amendment / ratification record. It closes `OI-11` of `docs/architecture/29-bonus-implementation-contract.md`
  ("ratify section 2.2's transport decision ... as an ADR"), which had stayed open since the bonus waves. It also closes the
  "bus-vs-sweep deviation unrecorded" item of `docs/plans/platform-completion-audit-2026-10-14.md` (section 2, Bonus row; item A2).
- **Owner:** `architect` (decision record), with `ledger-finance` as owner of the sweep-correctness claims in section 4.
- **Not a Blueprint change.** `CLAUDE.md` forbids silently contradicting the Blueprint. This ADR is the explicit, recorded
  contradiction: the Blueprint model is not what is built, and the reasons and limits are below. The owner may overrule it.
- **Source limitation (recorded).** The Blueprint PDF could not be text-extracted in this environment (no `pdftotext` or
  `pypdf`). The statement that Blueprint section 4.5 prescribes an event-bus subscription model for the bonus engine is taken
  from the repository's own references to it: `internal/eventbus/eventbus.go` ("Blueprint section 4.5's per-player ordering"),
  ADR 0003 (event bus = Kafka, or NATS JetStream) and the task brief for this record. `docs/architecture/01-requirements-inventory.md`
  lists the bonus capability (campaign creation, eligibility, grants, wagering progress, payout/forfeiture) but does **not** itself
  describe a subscription model. The wording of Blueprint 4.5 should be re-checked against the PDF by a human before this ADR
  is cited as a Blueprint finding.

## 1. The deviation

| Aspect | Blueprint model (as referenced above) | What is built |
|---|---|---|
| Trigger delivery | Bonus engine subscribes to domain events on an event bus (per-player ordering) | Bonus engine **polls durable database facts** on timers |
| Transport | Broker (Kafka / NATS JetStream per ADR 0003) behind `eventbus.Publisher` / `Subscriber` | No broker, no bus; `internal/eventbus` is an in-memory STUB with **no importer** |
| Monetary triggers | Event per deposit / bet / win | Read from the ledger (`ledger_transactions` joined to `deposit_intents`) by a watermarked sweep |
| Time triggers | Scheduled event | Cashback settlement window and grant expiry swept hourly |

## 2. What is built (verified in code 2026-10-15)

- `internal/bonus/deposit_sweep.go`: `RunDepositSweepForTenant` reads `ledger_transactions` (`transaction_type = 'deposit'`)
  joined to `deposit_intents` in `listUnprocessedDeposits`, ordered by `(posted_at, id)` after a per-tenant watermark kept in
  `bonus_ledger_sweep_watermarks` (migration 0068, consumer name `bonus_deposit_sweep`), at most `DepositSweepBatchSize = 500`
  deposits per tick, and calls `IssueAndActivateDepositBonus`. It deliberately imports no `internal/payments`.
- `internal/bonus/cashback_scheduler.go`: `RunCashbackSchedulerForTenant` computes the net loss of elapsed windows from
  `ledger_transactions` / `ledger_entries` and issues cashback grants with the deterministic `trigger_reference`
  `cashback:<campaign_id>:<player_account_id>:<window_start>:<window_end>`. Its watermark table (migration 0069) is documented in
  the file as a performance optimisation only, not a correctness dependency.
- `internal/bonus/expiry_sweep.go`: `RunExpirySweepForTenant` terminates open grants past `expires_at` through the existing
  `TerminateGrant` path, using the database clock.
- `internal/bonus/schedulers.go` wraps the three jobs: per-tenant `pool.WithTenant` transaction, `pg_try_advisory_xact_lock`
  keyed with `hashtextextended`, `RunDepositSweepSchedulerLoop` / `RunCashbackSchedulerLoop` / `RunExpirySweepSchedulerLoop`.
- `cmd/platform-api/main.go` (around lines 571-584) starts the three loops with `cfg.BonusDepositSweepInterval`,
  `cfg.BonusCashbackSweepInterval` and `cfg.BonusExpirySweepInterval` (defaults in `internal/config/config.go`: 5 minutes, 1 hour,
  1 hour; each overridable in seconds by environment), and waits for them on shutdown.
- Wagering progress is **not** swept: the bonus mirror is generated inside `ledger.Post` (`internal/ledger/bonus_mirror.go`), in
  the same transaction as the posting. HR-9 (the fail-closed `BONUS_SET` posting guard) was removed and is verified absent.
- `internal/eventbus/eventbus.go` carries `STATUS: STUB`; `grep -rn "internal/eventbus" --include=*.go` finds no importer
  anywhere in the repository. Label: `STUB`, unused.

The design decision itself is older than this record: `docs/architecture/29-bonus-implementation-contract.md` section 2.2
(`ARCHITECTURAL DECISION`, Wave 1) and `docs/architecture/ledger-accounting-model.md` section 7.18.1 ("there is no event bus");
section 7.18.1 binds Wave 3 to it. This ADR ratifies it and states the consequences.

## 3. Why this is acceptable for technical readiness

1. **No dual write.** A sweep derives the trigger from a fact that is already committed (the ledger row). An event-bus
   publish after commit can be lost between the commit and the publish; a publish before commit can announce a fact that
   rolls back. The ledger-tailing read has neither failure mode. (An outbox would fix this too, but it is a larger build; see 6.)
2. **Idempotent by business key, not by event id.** A grant is unique on
   `(tenant_id, campaign_id, offer_version_id, player_account_id, trigger_reference)`; a re-scan of a processed deposit
   resolves to `ErrAlreadyGranted`. A lost or reset watermark causes redundant work, never a double payout (stated in
   `cashback_scheduler.go` and `deposit_sweep.go`).
3. **Transactional.** Each tick processes a tenant inside one tenant-scoped transaction under an advisory lock that releases at
   commit or rollback, so a crashed process cannot leave a tenant permanently un-swept and two replicas do not double-process.
4. **No new infrastructure.** No broker to operate, secure or reconcile, consistent with ADR 0003's Stage 1 stance and
   `CLAUDE.md` ("no uncontrolled scope expansion").
5. **Money correctness does not depend on it.** The bonus ledger mirror and wagering contribution are in the ledger
   transaction; the sweeps only decide *when a grant is issued or terminated*. A late sweep is a player-experience lag, not
   ledger drift (the rationale recorded in `internal/config/config.go` for the 5-minute default).
6. **Consistent with existing platform patterns.** Reconciliation, the RG enumeration sweep and the payments sweeper use the
   same scheduler shape.

## 4. Consequences and limits (stated honestly)

- **Latency equals the sweep interval.** Deposit bonuses appear up to 5 minutes (default) after the deposit settles; cashback and
  expiry are hourly. There is no real-time reaction to an event.
- **No fan-out.** A second consumer of "deposit settled" must either add its own sweep or wait for the outbox (section 6). No other
  subsystem can subscribe.
- **Per-player ordering is not a bus property here.** Ordering comes from `(posted_at, id)` plus per-grant uniqueness.
- **Open risk, verified in code and not yet closed: watermark skew in the deposit sweep.** `ledger_transactions.posted_at`
  defaults to `now()` (transaction start time, migration 0021), and `listUnprocessedDeposits` selects strictly after the
  watermark `(posted_at, id)` which is advanced to the last processed deposit with **no trailing safety margin**. Migration 0068
  documents the commit-order versus timestamp-order skew and leaves the margin to the scan logic; the scan logic does not
  apply one. A deposit transaction that started earlier but committed after a later-started deposit had already been swept
  could therefore be skipped permanently by this sweep (the player would miss a deposit bonus; there is no ledger effect).
  This was **read, not reproduced by a test**. Proposed registry row `BONUS-SWEEP-WATERMARK-SKEW-1` (class B: bonus fairness,
  not a money-drift defect); the fix is a trailing margin behind `clock_timestamp()` or a periodic bounded re-scan, both of which
  fit the existing columns without a migration.
- **Batch bound.** 500 deposits per tenant per tick; a larger backlog drains over later ticks.
- **Only active tenants are swept** (`tenants.status = 'active'`).
- **Providers:** nothing here depends on a provider; the sweeps are `IMPLEMENTED`. Bonus UI and abuse controls are separate
  open items (audit section 3, class B).

## 5. Labels

| Item | Label |
|---|---|
| Deposit / cashback / expiry sweeps and scheduler wiring | `IMPLEMENTED` (local DB evidence; hosted CI run #828 green) |
| Event-bus subscription model for the bonus engine (Blueprint) | `NOT IMPLEMENTED` (deliberate deviation, this ADR) |
| `internal/eventbus` | `STUB`, unused by any package |
| Message broker (Kafka / NATS) | `NOT IMPLEMENTED`, not authorised |
| Deposit-sweep watermark margin | `NOT IMPLEMENTED` (open risk above) |

## 6. Migration path to the outbox (ADR 0117)

ADR 0117 Part A (accepted, not implemented) designs a transactional outbox: `platform_events` written in the business
transaction, per-subscription `event_deliveries` fan-out at insert, state-based claiming (no high-water-mark cursor, precisely
because of the skew above), `event_relay` claim-only identity, consumers that re-read the fact row. Its slice **A5** ("later,
bonus owner: evaluate moving the migration 0068 sweep onto this runtime") is the planned migration:

1. Build ADR 0117 A1-A3 first (outbox, `wallet.ledger_posted` producer, MOCK sinks). Not authorised yet.
2. Add a `bonus_deposit` ordered subscription on `wallet.ledger_posted` (partition `wallet_id`). The consumer re-reads the
   ledger/deposit row (events are hints) and calls the same `IssueAndActivateDepositBonus`, so the business-key idempotency is
   unchanged.
3. Run sweep and consumer side by side; the unique grant key makes the overlap harmless. Compare grant sets, then retire the
   deposit sweep. Cashback and expiry are time-driven and may stay as schedulers.
4. Retire `internal/eventbus` or reduce it to the test double ADR 0117 A.2 already calls it.

Trigger to start: a second in-process consumer of deposit/settlement facts, a latency requirement below the sweep interval,
or the owner deciding that the Blueprint model is required. Until then no work is scheduled, and none is needed for technical
readiness.

## 7. Verification performed for this record

Read in code on 2026-10-15 at the branch tip: `internal/bonus/deposit_sweep.go`, `cashback_scheduler.go`, `expiry_sweep.go`,
`schedulers.go`; `cmd/platform-api/main.go` lines 571-584; `internal/config/config.go` defaults; `internal/eventbus/eventbus.go`;
`grep` for importers of `internal/eventbus` (none); `migrations/0021` (`posted_at DEFAULT now()`) and `0068` (ordering caveat);
ADR 0117 Part A; `ledger-accounting-model.md` 7.18.1; `29-bonus-implementation-contract.md` 2.2 and OI-11. No test was run for
this documentation change.
