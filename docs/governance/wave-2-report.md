# Stage 4H-B1, Wave 2 — Bonus Engine Implementation — Final Report

**Status: READY**, subject to the explicitly-open items in §21.

This report is the Master Orchestrator's synthesis of the human-authorized
"STAGE 4H-B1 — WAVE 2 — BONUS ENGINE IMPLEMENTATION" directive: the first
real-code implementation stage for the Bonus Engine, following four
prior design-only stages (Wave 1, Wave 1.5, Wave 1.5 Fix Wave, Wave 1.5
Fix Round 2) that produced a fully frozen, independently-certified
architecture. Structured per the directive's own 11-phase specialist
sequence, plus two dependency-request fix dispatches surfaced along the
way and closed before final sign-off.

## 1. Implementation summary

A real, tested `internal/bonus` package now exists, with live integration
into `internal/ledger`, `internal/risk`, and `internal/casino`. Built:
Campaign/Offer/Grant domain model and lifecycle state machine; the `AOE`
(Attributable Open Exposure) / `NewStakeEligibility` G-2-avoidance
mechanism; `GrantLedgerAttribution`; the G-2 mechanism's mechanical half
(`ResolveTerminalGrantCredit`/`RecheckGrantExposure`, now called for real
by casino's `postWin`/`postRollback`); the five authorized bonus types
(Deposit, Reload, Cashback, Generic Wagering, Coupon); Bonus Conversion
(now functional end to end, since Phase 4 landed the `bonus_conversion`
Risk Operation); `EconomicOperationIdentity` enforcement at every
grant-causing surface; `SEP-1` actor≠subject enforcement at all four
`REQ-SEP-BONUS` points; four-eyes governance infrastructure; the Bonus
Suggestion lifecycle; static/pinned targeting and bulk grant jobs; and a
player/staff HTTP surface. Segmentation (dynamic), CRM, Affiliate,
Gamification, real sportsbook, and all real external providers were
correctly not implemented, per the directive's explicit scope boundary.

Across the 11 phases and two follow-on fix dispatches, **seven genuine
defects were found and fixed** during implementation and review (not
merely designed against): an RG/Risk/AssetAuthorization gate bypass on
`ACTION_ROUTE_TO_CASH` (Phase 5); a posting-shape bug that would have
made that same action's ledger posting always fail (Phase 5, found in
passing); a missing `SEP-1` Step-0 self-proof (Phase 6); an
error-masking bug in bulk-worker concurrency handling (Phase 6); an
AOE-attribution gap and a redelivery-idempotency bug in casino's new
integration (Phase 7); an EOI/Risk lock-ordering reversal (`DR-4HB1W2-01`,
dormant but a live future deadlock risk); and a structural no-op in the
single-grant surface's EOI recipient-ceiling enforcement that reopened
the SEC-W15-02 decomposition vector through a different door
(`DR-4HB1W2-02`). All seven are closed, each with a regression test
proven to fail against the pre-fix code and pass against the fix.

## 2. Exact files changed (by phase; full diffs in the cited commits)

- **Phase 1** (`d145ba1`): `internal/ledger/bonus_mirror.go` (new),
  `internal/ledger/ledger.go`, `internal/money/money.go` (new),
  migrations `0050`-`0052`, `ledger-accounting-model.md`.
- **Phase 2** (`e397985`): migrations `0053`-`0060`, `internal/bonus/*`
  (schema/repository skeleton), `internal/economicop/*` (skeleton),
  `internal/auth/permission.go`, `internal/identity/staff_user.go`.
- **Phase 3** (`089c6a8`): migrations `0061`-`0064`, `internal/bonus/*`
  (lifecycle, AOE, attribution, held-disposition ops, types, targeting,
  suggestion lifecycle, change governance, wagering), `internal/httpserver/bonus_{handlers,routes}.go`,
  `internal/httpserver/admin_routes.go`.
- **Phase 4** (`8144275`): migration `0065`, `internal/risk/{types,cumulative}.go`,
  `internal/httpserver/risk_handlers.go`, `docs/api/openapi/platform-api.yaml`,
  `docs/decisions/0031-*.md`, `internal/bonus/{conversion,eligibility}.go`.
- **Phase 5** (`4df40b9`): `internal/bonus/held_disposition_ops.go`,
  `internal/httpserver/bonus_handlers.go`.
- **Phase 6** (`ac7bad2`): migration `0066`, `internal/bonus/targeting.go`,
  `internal/httpserver/admin_routes_test.go` (new).
- **Phase 7** (`a79db4a`): `internal/casino/{orchestrator,types}.go`,
  `internal/casino/bonus_settlement.go` (new).
- **Phase 8**: read-only, no files changed.
- **Phase 9** (`be2eed6`): `docs/testing/testing-strategy.md`,
  `internal/bonus/lifecycle_integration_test.go`.
- **Phase 10** (`90b0ae7`): `docs/architecture/{08-casino-integration-architecture,10-bonus-engine-architecture,ledger-accounting-model}.md`,
  `docs/governance/{ownership,task-registry}.md`.
- **`DR-4HB1W2-01`** (`28e34db`): `internal/bonus/{lifecycle,targeting}.go`.
- **`DR-4HB1W2-02`** (`af4f9cd`): `internal/economicop/enforce.go`,
  `internal/bonus/lifecycle_integration_test.go`.
- **Phase 11** (`3526d87`): migration `0067`, `internal/bonus/lifecycle.go`,
  `ledger-accounting-model.md`.

## 3. Migrations (all additive, all round-tripped up/down/up against real Postgres)

`0050` bonus_expense account · `0051` bonus transaction types ·
`0052` player_bonus_held account · `0053` economic_operations ·
`0054` bonus_campaigns(+versions) · `0055` bonus_offers(+versions) ·
`0056` bonus_suggestions(+review events) · `0057` bonus_grants ·
`0058` bonus_wagering_progress · `0059` bonus_held_dispositions ·
`0060` bulk_grant_jobs(+items) · `0061` grant_ledger_attributions ·
`0062` bonus_grant_progress · `0063` bonus_change_governance ·
`0064` bonus staff roles + EOI item column · `0065` risk bonus_conversion
operation · `0066` bonus SEP-1 step-0 self-proof · `0067` bonus_grants
granted_amount.

## 4. API / OpenAPI changes

`internal/httpserver/bonus_handlers.go`/`bonus_routes.go`: player
surface (list/view own grants and progress, redeem coupon), staff
surface (create campaign, issue manual grant, claim/decide suggestion,
resolve held disposition). `docs/api/openapi/platform-api.yaml` updated
for the new `bonus_conversion` Risk operation enum value. **Disclosed
incompleteness** (Phase 6's finding, restated in §21): no HTTP endpoint
exists yet to file/approve a `bonus_change_requests` row or to mint an
`EconomicOperationIdentity` root, so the manual-grant/bulk-job four-eyes
path is fully built and tested at the Go/DB level but not reachable via
HTTP without direct database access. This is feature-incompleteness, not
a security defect — no insecure path is reachable.

## 5. Bonus state machine

`internal/bonus/lifecycle.go`: `issued → activated → (pending_settlement) → (converted | expired | cancelled | forfeited)`, with explicit compare-and-swap
transition guards (illegal transitions rejected, never overwritten),
full Progress-trail append on every transition, `audit.Record` on every
mutating write, `(tenant_id, grant_id)` advisory-lock discipline per
HR-25's pinned order. `NewStakeEligibility`/`AOE`'s three-component
model (`LockedExposure`, `InFlightExposure`, `HeldDisposition`)
implemented exactly per the frozen §N1.3 design — `HeldDisposition` is a
live `player_bonus_held` balance read, never a row-existence check.

## 6. Accounting model

`player_bonus_held` (new, disjoint account, closing LF-19/LF-20 from
prior rounds) alongside `bonus_expense`/`promo_liability`/`player_bonus`/
`player_locked_bonus`. The Rule B2 mirror generator
(`internal/ledger/bonus_mirror.go`) fires unconditionally on any
transaction touching a `BONUS_SET` account (`player_bonus`,
`player_locked_bonus`, `player_bonus_held`), with no caller-selectable
mirror leg (HR-17) and no `transaction_type` switch. `RoundToMinorUnits`
(`internal/money`) implements DS-1/DS-2 (round-half-up, ties away from
zero, round once at the final boundary), `*big.Int`-based throughout —
no floating-point money anywhere in the new code.

## 7. Transaction flows — every real posting shape, traced and certified balanced (Phase 11)

Grant activation (1 caller leg → 2 total, mirror only) · ordinary
forfeiture (1 → 2) · bonus_conversion (2 → 4, mirror + recognition) ·
casino hold-capture (4 → 6) · held-win rollback (2 legs, straight
reversal to `house_gaming`, never restores `player_locked_bonus`) ·
`ACTION_REFORFEIT` (1 → 2, residual exactly zero) ·
`ACTION_ROUTE_TO_CASH` (2 → 4, structurally identical to
bonus_conversion's own frozen shape — **final ruling: correct**, closing
the question Fix Round 2 deferred back to ledger-finance). No
hand-assembled mirror leg found anywhere; every shape verified by
tracing the actual algorithm against actual caller-supplied entries, not
by re-deriving from design docs.

## 8. Risk / RG / KYC integration

The T.1 three-way gate (AssetAuthorization → RG → Risk, live, same
transaction, unmodified order) runs at every value-creating or
eligibility-determining checkpoint, confirmed by three independent
reviewers (Phases 4, 5, 6) reading the actual call sequences, not
trusting the design. `bonus_conversion`'s Risk Operation was landed for
real (Phase 4, six-artifact ADR 0031 §16 change) — conversion now works
end to end, proven by a real test. A DENY/REVIEW/error at any checkpoint
always blocks; `ConvertGrant` never forfeits on a Risk error, only
blocks. RG integration confirmed clean with no parallel self-exclusion
mechanism (Phase 5). **KYC integration has a disclosed, unbuilt gap**:
`bonus_offer_versions.kyc_rg_level_required` is stored but never
enforced, because `internal/kyc` has no "level/tier" taxonomy at all —
this traces to a missing upstream identity-compliance concept the
design presupposed, not a Bonus omission, and is correctly named rather
than invented unilaterally (§21).

## 9. AssetAuthorization integration

Runs as part of the T.1 gate at every checkpoint; no bonus code
hardcodes a currency list. `player_bonus_held`/`bonus_expense` are
`NUMERIC(38,0)` per-asset like every other ledger account.

## 10. Targeting / segmentation model

Single-player, explicit list, promo/voucher codes, manual grants, and
bulk grants (via `bulk_grant_jobs`) are implemented. **Dynamic
Segmentation was deliberately not implemented** — `internal/segment`
does not exist, and its design (doc 30) still carries known-unfixed
correctness bugs (Kleene-logic polarity gap, unpinned `member_of`) from
before this Wave. Only the static/pinned case (a caller-supplied,
already-resolved player-ID list) is built; a caller requesting dynamic
segment resolution gets a clear, disclosed failure, not a workaround.

## 11. Bulk economic-operation model

`EconomicOperationIdentity` (`internal/economicop`) is implemented as a
domain-agnostic, additive-by-extension mechanism (confirmed by Phase 8's
sportsbook-boundary review reading the actual code): a closed
`operation_type` enum, root-subtree-scoped budget consumption (not
per-page), a monotone `COUNT(DISTINCT)` recipient ceiling, and the
canonical lock order (non-locking entry check → gate chain including
Risk's advisory lock → locking EOI consume, exactly once, immediately
before the effecting write) — **now correctly implemented on both
EOI-gated surfaces after `DR-4HB1W2-01`'s fix**. Bulk-decomposition
adversarial testing (sequential and genuinely concurrent) confirms the
ceiling holds under real multi-worker races.

## 12. Actor/subject enforcement

`SEP-1` (actor≠subject/beneficiary, unconditional, DB-trigger-enforced)
is adopted at all four `REQ-SEP-BONUS` points, including the
newly-T.1-gated `ACTION_ROUTE_TO_CASH`. Migration `0066` closed a real
gap (missing Step-0 tenant-scope self-proof on the four-eyes governance
trigger). `SEP-1`'s own core case (a staff member who genuinely is the
Grant's beneficiary) had never actually been exercised by any test
before Phase 6 added one — now covered, plus an anti-inertness check
proving an unrelated approver still succeeds.

## 13. Provider-native bonus coexistence

Confirmed by Phase 7's own test (`TestPostWin_PlainCashWin_NeverTouchesBonusMachinery`)
and by construction: the Grant-lookup path is only reachable when the
origin resolves to `player_locked_bonus`, so a cash-settled provider-native
promo structurally cannot reach any Bonus call. `event.PlayerAccountID`
is never read in the destination-resolution path — the provider payload
cannot select a beneficiary.

## 14. Reconciliation design

`GrantLedgerAttribution` derives every Bonus balance from a live join of
`grant_ledger_attributions` → `ledger_entries` → `ledger_accounts` —
zero maintained counters, confirmed by Phase 11 reading the actual SQL.
The generic ledger-vs-projection drift sweep (`internal/reconciliation`)
already covers every new Bonus account automatically. The
bonus-specific B1(extended) invariant is proven correct by a real test
helper but **not yet wired into the production scheduler** — disclosed,
non-blocking (§21).

## 15. Audit model

Every mutating administrative/financial action in the wired paths writes
an `audit.Record` (actor, tenant, entity, outcome), spot-checked by
Phase 6 across issuance, activation, held-disposition resolution, and
suggestion lifecycle transitions.

## 16. RLS / security model

All 16 new tables carry both `relrowsecurity` and `relforcerowsecurity`,
verified with direct SQL (not just code reading) by Phase 6, including
adversarial forged-tenant-id `INSERT`/`SELECT`/`UPDATE` attempts. The app
role has neither `rolsuper` nor `rolbypassrls`. The staff-creation
allowlist gap (Phase 2's own finding) was closed with the same
platform-admin-only restriction pattern used for `finance`/`risk_manager`.

## 17. Concurrency / idempotency evidence

Every financial write in `internal/bonus` is DB-enforced idempotent (real
unique constraints, never check-then-insert): Grant issuance
(`UNIQUE(tenant_id, campaign_id, offer_version_id, player_account_id, trigger_reference)`),
held-disposition creation (`settlement_ledger_transaction_id`),
resolution/conversion (`ledger.Post`'s own idempotency key). Real
PostgreSQL concurrency tests cover: concurrent activation, concurrent
bulk-worker recipient-ceiling races, concurrent expiry-vs-cancellation,
concurrent reversal, concurrent single-manual-grant ceiling races, and
casino's held-win-rollback compare-and-swap under real races (both
lock-acquisition orderings). Full `-race -tags=integration` suite run to
completion multiple times across the Wave, zero flakes, zero
regressions, confirmed independently by at least three separate
dispatches plus the Orchestrator's own manual run.

## 18. Specialist reports

Preserved in each phase's commit message and in this session's own
record, per this project's established convention (individual
SubagentHandback transcripts are not separately committed as files).

## 19. P0/P1/P2/P3 matrix

**P0/P1 found and CLOSED this Wave**: `ACTION_ROUTE_TO_CASH` RG/Risk/AssetAuthorization
bypass (Phase 5) · its accompanying posting-shape bug (Phase 5) ·
`SEP-1` Step-0 gap on migration `0063` (Phase 6) · bulk-worker
error-masking bug (Phase 6) · AOE-attribution gap in casino's rollback
posting (Phase 7) · redelivery-idempotency bug in held-win rollback
(Phase 7) · EOI/Risk lock-ordering reversal (`DR-4HB1W2-01`) · single-grant
EOI recipient-ceiling no-op / SEC-W15-02 reopened through the single-grant
door (`DR-4HB1W2-02`) · missing `bonus_grants.granted_amount` column
(`DR-4HB1W2-03`, Phase 11).

**P2, documented risk, not fixed (owning domain's own call)**: a 3+-way
deadlock on the EOI root row under true multi-worker concurrency (Phase
6 — availability, not correctness; today's only real executor is
sequential and never reaches this shape).

**P2/P3, genuinely open, named and routed (not silently dropped)**: LF-10's
general case (rollback of an already-*resolved* financial event —
correctly fails closed, still ledger-finance's) · KYC-tier taxonomy gap
(architect/identity-compliance) · multi-account/Person-level abuse
detector, schema-only (bonus-engine/risk) · B1(extended) reconciliation
scheduler wiring (non-blocking) · missing HTTP surface for four-eyes
filing/approval and campaign-activate/offer-publish/bulk-job-execute
(backend/bonus-engine, feature-incompleteness) · `IssueAndActivateCashback`
has zero live callers anywhere in the repo (backend/bonus-engine) ·
`HELD-ROLLBACK-CAS-RACE-1`/`CASINO-LF18-QUERY-RACE-1`/`EOI-BUDGET-RACE-1`'s
missing negative-control/multi-page proofs (qa, test-coverage
completeness, not a code defect) · two sportsbook-forward P3s (the
two-quantity seam shape, casino-specific transaction-type constants in
`aoe.go`/`wagering.go` — both disclosed, both fail in the safe direction,
both purely additive extension points).

## 20. Human Decision Register — confirmed untouched

Every one of the 13 dispatches this Wave (11 phases + 2 fix dispatches)
explicitly confirmed no selection, narrowing, or default of G-2,
`OpenBetSelfExclusionPolicy`, the mixed/bonus-funded sportsbook cashout
policy, or FD-1. `ConvertGrant`'s and `ResolveHeldDispositionAction`'s
own fail-closed behavior on a missing/denying dependency is the
mechanism this project has used throughout to guarantee this — a
refusal, never an inferred default.

## 21. Remaining known limitations (explicit, not swept under the READY verdict)

- **LF-10's general case** (rollback of an already-resolved, non-held
  financial event) remains open — ledger-finance's, fails closed safely
  today, needs a dedicated future design dispatch.
- **No KYC-tier/level taxonomy exists** for Bonus's `kyc_rg_level_required`
  field to reference — a cross-cutting architecture decision, not built
  this Wave.
- **No multi-account/Person-level abuse detector exists** — schema
  columns are present (`velocity_cap_refs`, `device_fingerprint_linking_config`),
  no detection code. One Person with multiple PlayerAccounts could claim
  a first-deposit-only Offer once per account today, undetected.
- **`postBet`'s bonus-funded locking side and the settlement-timeout
  sweep are not implemented** — correctly deferred, the sweep is
  explicitly blocked on a still-unmade jurisdiction/ratification
  decision per doc 08's own stated posture.
- **No HTTP surface exists yet** for filing/approving four-eyes change
  requests, minting an EOI root, or campaign-activate/offer-publish/
  bulk-job-execute — the underlying mechanisms are built and tested;
  only the admin UI/API surface for them is not.
- **`IssueAndActivateCashback` has no live caller** anywhere in the
  repository — correctly gated by construction, but entirely unwired.
- **The B1(extended) reconciliation invariant is proven correct but not
  scheduled** in production.

None of these are silent gaps — every one was found, named, and
attributed to a specific owning domain by the specialist who found it,
consistent with this project's standing no-fake-completion discipline.

## 22-25. Git state

Branch `claude/focused-wright-jw88w9`, HEAD `3526d87`, working tree
clean, fully pushed. Full test results: see §17 and each phase's
individual commit message; the final, complete validation floor
(`gofmt -l .`, `go build ./...`, `go vet ./...`, `go test -count=1 ./...`,
`go test -tags=integration -count=1 ./...`, `go test -race -tags=integration -count=1 ./...`)
was run to full completion in Phase 11 with zero failures across all 27
tested packages, and independently re-confirmed by the Orchestrator's
own manual run after `DR-4HB1W2-01`'s fix.

## Final verdict

# **READY**

Every P0/P1 found during this Wave's own implementation and review was
closed with a proven regression test before this report was written. No
control was weakened to manufacture this verdict — several fixes made
the design measurably stricter than the frozen architecture required
(the `SEP-1` Step-0 self-proof, the EOI recipient-ceiling fix, the
lock-ordering fix). The remaining open items (§21) are genuine,
disclosed, non-blocking limitations — mostly forward-looking gaps in
domains explicitly out of this Wave's authorized scope (Segmentation,
sportsbook, KYC-tier taxonomy) or feature-completeness gaps (missing
HTTP admin surfaces) rather than correctness or security defects in what
was built.

## Explicit stop

Per the authorizing directive: **this concludes Stage 4H-B1 Wave 2.
Wave 3, CRM implementation, Affiliate implementation, Gamification
implementation, sportsbook implementation, Retail/POS implementation,
Back Office frontend implementation, Partner Console frontend
implementation, and B2C frontend implementation all remain
unauthorized.** No further work proceeds without a new, explicit human
directive.
