# Active Stage

## Stage 3A — Financial Architecture Freeze — Complete

Status: **Complete, pending human approval to authorize Stage 3B
implementation.** Stage 2 (Identity + Tenancy + Security) and the
pre-Stage-3 security hardening pass were both completed and approved by
the human. The human then directed Stage 3A: freeze the complete wallet/
ledger/payments/withdrawal/crypto-custody/reconciliation architecture and
put it through independent specialist review *before* any implementation
begins. This is explicitly NOT Stage 3B — no wallet, ledger, payment,
PSP, casino-wallet-callback, sportsbook, bonus-posting, or crypto
integration code, schema, or migration exists.

### Objectives (as instructed at the Stage 3A gate)

1. Re-read the Blueprint's full financial model and the existing Stage
   0-2 architecture/decisions, and design (not implement) the complete
   financial domain model, ledger/accounting model, canonical transaction
   flows, payment orchestration, withdrawal state machine, crypto custody
   boundary, and reconciliation model.
2. Preserve the Blueprint's terminology and intent; mark anything not
   literally specified as an architectural decision, and anything the
   Blueprint is silent on as an explicit open decision — never invent a
   business/policy resolution.
3. Design multi-wallet/multi-asset accounting, database-enforced
   idempotency and concurrency control, and balance-projection/
   reconciliation architecture consistent with CLAUDE.md's financial
   invariants.
4. Run independent specialist review (financial-domain, ledger/
   accounting, payments, crypto/custody, security, multi-tenancy,
   backend, QA, architecture/code review) and fix documentation/design
   defects directly; escalate anything requiring a business decision as
   an explicit open decision.
5. Produce a formal list of Mandatory Financial Invariants Stage 3B must
   enforce.
6. Change no application code — documentation and architecture only.

### Completed work

See `docs/progress.md`'s "Stage 3A — Financial Architecture Freeze"
section for the full itemized inventory. Summary:

- **Seven new architecture documents** (`docs/architecture/`):
  `financial-domain-model.md`, `ledger-accounting-model.md`,
  `financial-transaction-flows.md` (20 canonical flows),
  `payment-orchestration.md`, `withdrawal-state-machine.md`,
  `crypto-custody-boundary.md`, `reconciliation-model.md`.
- **Three new ADRs** (`docs/decisions/`): `0019` (authoritative ledger/
  balance-projection architecture, including the concrete RLS/tenancy
  shape and actor-authorization matrix), `0020` (financial idempotency
  and concurrency control), `0021` (multi-asset accounting).
- **One 13-line addendum** to `docs/architecture/03-database-architecture.md`
  pointing to the documents above where Stage 3A supersedes an earlier
  Stage 0 sketch (signed amounts, global idempotency keys). No other
  existing file was changed.
- **Independent specialist review** by `ledger-finance`, `payments`,
  `security`, `architect`, `backend`, `qa`, and `code-reviewer`, each
  reviewing the full document set and authorized to fix non-business
  defects directly. This pass found and fixed several genuine accounting
  defects (unbalanced/inverted entries in multiple flows), a real
  multi-tenancy defect (platform-global idempotency keys on tenant-
  partitioned RLS-protected tables), a self-inflicted RLS/P1-drift risk
  that reproduced ADR 0016's own discovered gotcha before it could ever
  ship, an unimplementable Postgres idempotency pattern (missing
  `SAVEPOINT`), and several withdrawal four-eyes bypass paths — full
  itemized list in `docs/progress.md`.

### Verification performed

Documentation-only stage — no build/test/migration verification applies.
`git status`/`git diff --stat` confirmed by the independent
`code-reviewer` pass: exactly ten new documentation files plus the one
13-line addendum above changed; no `.go` file, migration, or config file
touched. Cross-reference integrity (every `§`-reference across all ten
new documents resolves) was checked and confirmed.

### Pending (to close out this stage)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Stage 3A Completion Report delivered to the human, ending with the
  required closing statement. No Stage 3B work begins until explicitly
  authorized.

### Blockers

None technical — this is a documentation-only stage and it is complete.
Three of the open decisions below block implementing *specific* Stage 3B
flows (not the stage as a whole): the `promo_liability` accounting
framing (blocks bonus grant/conversion/forfeiture), the missing bank-
treasury ledger account for PSP settlement batching (blocks that flow
balancing inside the ledger), and the missing crypto-custodian ledger
account (blocks crypto deposit/withdrawal postings). Core ledger, wallet
model, withdrawal-workflow shell, PSP-orchestrator shell, and
reconciliation-job implementation are not blocked by any of the three and
can proceed once Stage 3B is authorized.

### Decisions/input still useful from the human before Stage 3B

1. Approve Stage 3A and authorize Stage 3B implementation.
2. Resolve, or explicitly accept a stated default for, the three blocking
   open decisions above (`docs/progress.md` lists all open decisions with
   pointers to where each is discussed).
3. The already-open, non-blocking business/compliance tracks carried
   forward from Stage 0-2 and the hardening pass remain open
   (`docs/decisions/0005`; ADRs 0017/0018's open items; `brands`' public-
   read RLS breadth).

## Stage 3A addendum — Payment Provider Agnosticism and Capability Model

The business owner added a core commercial requirement mid-gate (before
Stage 3B was authorized): the platform must integrate multiple
replaceable fiat and crypto payment providers, with adding a provider
never requiring a core financial-system rewrite. Addressed as a Stage 3A
addendum (still documentation-only): new `docs/decisions/0022-payment-
provider-agnosticism-and-capability-model.md`, plus updates to
`payment-orchestration.md`, `crypto-custody-boundary.md`,
`ledger-accounting-model.md`, and `financial-domain-model.md`. Full
inventory in `docs/progress.md`'s "Stage 3A addendum" section.

Independent specialist review (`payments`, `architect`, `security`,
`code-reviewer`) of this addendum is complete. It found and fixed a
blocking defect two reviewers caught independently (a `provider_kind`
value that would have let a crypto custodian enter the payment-routing
candidate pool — the exact custody-boundary collapse the addendum exists
to prevent) plus two further `security`-found privilege-boundary gaps
(inbound key material arriving in an ordinary payment-adapter API
response; a shared-SDK path that would have given a payment adapter
transitive custody scope). Full itemized findings:
`docs/progress.md`'s "Stage 3A addendum" section. No wallet/ledger/
orchestrator/adapter code exists; no real provider is integrated; no
production credentials requested or stored; no specific vendor selected.
