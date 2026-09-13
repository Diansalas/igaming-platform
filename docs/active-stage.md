# Active Stage

## Stage 3B — Core Financial Infrastructure Implementation — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Stage 3A (Financial Architecture Freeze) and its Payment Provider
Agnosticism addendum were approved by the human. The human then issued
the Stage 3B Final Approval directive with a controlled scope (25 approved
implementation items; bonus/crypto financial posting, PSP batch
settlement, and any flow requiring an unresolved accounting counter-
account explicitly blocked). This stage implements that approved scope:
the wallet, ledger, payment-provider orchestration, and withdrawal
four-eyes approval subsystems, behind a mock PSP only.

### Objectives (as instructed at the Stage 3B gate)

1. Implement wallets, the append-only double-entry ledger, and the
   balance-projection model exactly as frozen in Stage 3A's architecture
   documents and ADRs 0019-0021.
2. Implement database-enforced financial idempotency and concurrency
   control (ADR 0020) with real-PostgreSQL adversarial tests, not
   assumed correctness.
3. Implement the PaymentProvider abstraction, PaymentOrchestrator,
   provider capability/routing model, and a conformance-tested mock PSP
   adapter (ADR 0022) - preserving strict PaymentProvider/
   CryptoCustodyProvider separation, since no crypto custody code exists
   yet.
4. Implement the withdrawal state machine and four-eyes approval
   infrastructure (`withdrawal-state-machine.md`).
5. Implement a reconciliation framework for the streams implementable
   this stage (ledger vs. balance projection, at minimum).
6. Run a mandatory 26-item adversarial financial test list against real
   PostgreSQL 16, and an independent specialist review pass (`ledger-
   finance`, `payments`, `security`, `architect`, `qa`, `code-reviewer`),
   fixing all P0/P1 findings before completion.
7. Leave every blocked feature (bonus posting, crypto financial posting,
   PSP batch settlement) untouched, and record every genuine scope
   boundary honestly rather than inventing an accounting treatment to
   make code compile.

### Completed work

See `docs/progress.md`'s "Stage 3B — Core Financial Infrastructure
Implementation" section for the full itemized inventory, including every
specialist-review finding and its resolution. Summary:

- **Nine new migrations** (`migrations/0019`-`0027`): wallets, ledger
  accounts/transactions/entries, wallet balance projection, provider
  capabilities (+ amount limits), deposit intents, withdrawal requests +
  approvals, reconciliation runs + mismatches. **One security-hardening
  migration** (`0028`) closing an RLS gap found during specialist review
  (see below).
- **Five new Go packages**: `internal/ledger` (posting engine, account
  model, balance projection/rebuild), `internal/wallet` (multi-asset
  wallet + summary), `internal/payments` (PaymentProvider interface,
  Orchestrator, mock adapter, capability model), `internal/withdrawal`
  (state machine, four-eyes approval), `internal/reconciliation`
  (ledger-vs-projection stream).
- **HTTP layer**: wallet/deposit/withdrawal player self-service routes,
  a staff four-eyes review queue and approve/reject/submit routes, an
  unauthenticated-by-design provider webhook route (tenant resolved from
  the URL, payload authenticated by the adapter's own signature
  verification), and a provider-capability admin route.
- **Comprehensive test suite**: the full mandatory 26-item adversarial
  list (double spend, duplicate callbacks, concurrent idempotency, same-
  key-different-payload, cross-tenant access, direct ledger UPDATE/DELETE
  rejection, compensation, withdrawal self-approval/amount-mutation/
  duplicate-approval/approval-substitution/threshold-manipulation,
  projection rebuild, provider timeout/ambiguous-outcome/retry/swap,
  asset precision, insufficient funds, unbalanced transactions, provider
  capability mismatch, custody-boundary checks) verified against real
  PostgreSQL 16, plus a race-detector run on every concurrency-heavy
  package.
- **Independent specialist review** (`ledger-finance`, `payments`,
  `security`, `architect`, `qa`, `code-reviewer`) found one **P0** (the
  webhook route's only registered adapter performed no signature
  verification - unauthenticated fund creation was possible) and several
  **P1**s (uncapped/unbounded deposit-reversal amount; an RLS policy gap
  letting a player-scoped connection write to six staff-only tables; a
  missing terminal-state guard letting a late callback corrupt an already
  -succeeded deposit; a withdrawal-submission race that could call a
  payment provider twice; a ledger idempotency key not namespaced by
  provider). **All were fixed and each has a dedicated adversarial test
  proving the fix**, not just the review's word for it. Full itemized
  findings and fixes: `docs/progress.md`.

### Verification performed

`go build ./...`, `go vet ./...`, and `gofmt -l .` clean. Full test suite
(`go test -tags=integration ./...`) passes with real PostgreSQL 16,
including the new adversarial suite. `go test -race` clean on
`internal/ledger`, `internal/wallet`, `internal/withdrawal`. Migration
round-trip (`up` → `down` → `up`) verified for all nine new migrations
plus the security-hardening migration. `git status`/`git diff --stat`
confirmed no production credentials, no unrelated scope creep, and every
changed file traces to an approved Stage 3B item or a specialist-review
fix.

### Pending (to close out this stage)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Stage 3B Completion Report delivered to the human, ending with the
  required closing statement. No further-stage work begins until
  explicitly authorized.

### Blockers / genuine scope boundaries (not defects)

None block Stage 3B's own approved scope, which is complete. The
following are honestly labeled boundaries for future stages, not silent
gaps:

- **Withdrawal PSP submission is narrower than the deposit orchestrator.**
  A staff-triggered `POST /v1/admin/withdrawals/{id}/submit` routes
  through the same PaymentOrchestrator and drives the withdrawal state
  machine's existing, tested `MarkSubmitted`/`Complete`/`Fail`
  transitions, but implements no cascade-on-decline and no automated
  ambiguous-outcome resolution for withdrawals (unlike deposits), and
  `internal/payments`'s `CallbackEventType` covers only deposit/deposit-
  reversal events - there is no withdrawal callback path yet. With only
  the mock adapter registered (whose `Withdraw` never returns
  `OutcomeSucceeded` for a non-magic amount), a submitted withdrawal
  reaches `completed` only via a magic decline amount or a direct package
  -level call; in the deployed system it safely stays at `submitted`
  (money held, no incorrect ledger effect) rather than reaching
  `completed`. Real PSP withdrawal settlement requires this gap closed.
- **Reconciliation implements one of the Blueprint's streams.** Only
  ledger-vs-balance-projection is built and tested; wallet-vs-PSP,
  wallet-vs-casino/sportsbook, provider-payable, and PSP-clearing/reserve
  streams are architecture-only (`reconciliation-model.md`), since no
  real PSP/casino/sportsbook relationship exists yet to reconcile
  against. No scheduler/cron wires `RunLedgerVsProjection` to run hourly
  as CLAUDE.md targets - it is implemented and tested but not yet
  invoked outside tests.
- **Withdrawal self-approval bypass (`withdrawal-state-machine.md` §5
  bypass #2) is not enforced at the HTTP layer.** `staff_users` has no
  `person_id` linking it to `persons`, so there is no data to check a
  "the approving staff member is also the withdrawing player" self-
  approval bypass against; the HTTP handler passes `beneficiaryCheck =
  nil`. The distinct-approver-principal-id check (bypass #1) IS enforced.
  Closing this requires a Stage 2 identity-model schema change
  (`staff_users.person_id`), out of Stage 3B's scope.
- **`provider_capability_amount_limits` has no `tenant_id` column of its
  own** and its RLS policy is a subquery into `provider_capabilities`
  (ADR 0019 otherwise forbids this shape). Not currently exploitable
  (the subquery inherits `provider_capabilities`' own tenant check), but
  structurally fragile; closing it requires a column addition and
  backfill.
- Bonus financial posting, crypto deposit/withdrawal financial posting,
  crypto-custodian ledger settlement, and any PSP batch-settlement flow
  remain **NOT IMPLEMENTED**, per the Stage 3B directive's explicit block
  list. No bonus/crypto/bank-treasury accounting was invented to make
  code compile.

### Decisions/input still useful from the human before the next stage

1. Approve Stage 3B and authorize the next stage (per CLAUDE.md's stage
   gate, casino/sportsbook/bonus/B2C frontend/partner console/production
   deployment/real PSP/real crypto integrations do not begin
   automatically).
2. Decide whether closing the withdrawal-self-approval gap
   (`staff_users.person_id`) should be prioritized before any B2C launch
   that has staff who are also players at the same brand.
3. The already-open, non-blocking business/compliance tracks carried
   forward from Stage 0-3A remain open (`docs/decisions/0005`; ADRs
   0017/0018's open items; `brands`' public-read RLS breadth; the
   promo_liability/bank-treasury/crypto-custodian accounting decisions
   that still block bonus and crypto financial posting specifically).
