# Active Stage

> **Current stage:** see ["## Current stage"](#current-stage-stage-103--authorized-2026-09-26-adr-0092--w0-in-progress) section (Stage 10.3 authorized, W0 in progress; staging OFF). Sections below are kept in their historical order.

## Stage 4H-B0-R7 — Final Financial/Bonus Implementation Gate — Complete

Status: **Complete. Workstream A (`player_locked` ledger-account origin
split) IMPLEMENTED, independently reviewed, fixed, and re-verified.
Workstreams B/C/D/E/F closed at the design-validation level (no code
authorized for them this stage). Three human decisions (G-2 Terminal-
Grant settlement-credit resolution, `OpenBetSelfExclusionPolicy` default,
mixed/bonus-funded cashout policy) remain unmade and are NOT selected by
any agent. Stage 4H-B1 (Bonus Engine) NOT started. No automatic
progression — explicit human authorization required.**

*Status note 2026-09-26: the three decisions above were answered by the human in `docs/decisions/0042-human-decision-response.md` Part 2 — G-2: all three treatments configurable per brand, default (b) route to `player_cash`; `OpenBetSelfExclusionPolicy` platform-wide fallback: `VOID_ON_SELF_EXCLUSION`; mixed/bonus-funded cashout: "Not cashout-eligible". Implementation remains outstanding: ADR 0042 records G-2 brand-level configurability and the (c) manual-review workflow as NOT IMPLEMENTED, and the self-exclusion default is not seeded with no auto-void consumer (`docs/plans/stage-10-planning-gate-proposal.md` §5).*

Purpose: close the implementation-blocking financial dependencies Stage
4H-B0-R6 discovered — `player_locked` phase 2, gate G-3 (bonus-funded
wagering-progress farming after a later void/rollback), the Terminal-
Grant and self-exclusion technical contracts, and a formal Human Decision
Register — while explicitly forbidding any Bonus Engine/Gamification/
Reward Orchestrator/real-provider code.

**Workstream A — `player_locked` phase 2 (migration 0048): IMPLEMENTED.**

Design/validation (pre-implementation, no code): `ledger-finance` re-
verified R6's claims against live HEAD and produced the exact migration
SQL, Go changes, and a ten-item test set (§6.5 of
`ledger-accounting-model.md`); independently cross-validated by
`architect`, `bonus-engine`, and `sportsbook` before any code was
written, closing two real cross-workstream inconsistencies neither
authoring specialist's own review caught (a G-2-dependent blind spot in
the wagering-progress netting measure, fixed via new **HR-14**; a
self-contradiction between two of `bonus-engine`'s own R7 deliverables on
whether forfeiture postings share a bet's `correlation_id`).

Implementation: `ledger-finance` split the ledger account type
`player_locked` into `player_locked_cash`/`player_locked_bonus`
(migration `0048`, invariant **L1** — locked-origin determinacy,
enforced across a 5-layer stack: CHECK constraint, Go type system,
migration pre-flight guard, `wallet.GetSummary`'s erroring default arm,
tests). Added the **HR-9** fail-closed posting guard
(`assertNoBonusSetEntries` in `internal/ledger`), rejecting any posting
against `player_bonus`/`player_locked_bonus` until `bonus_expense` and
the Rule B2 (extended) mirror generator both exist. `internal/wallet`'s
`GetSummary` gained `LockedCashBalance`/`LockedBonusBalance` alongside
the combined `LockedBalance`, with a new erroring `default` arm replacing
a prior silent-zero risk.

**A real defect was found and fixed during implementation, not before
it**: the migration's own pre-flight guard, as designed, was a bare
`SELECT count(*)` — silently inert, because `ledger_accounts` carries
`FORCE ROW LEVEL SECURITY` and a migration connection sets no
`app.tenant_id`, so the count always reads zero regardless of what the
table holds. `ledger-finance`'s own test caught this on first run. The
first fix attempt toggled `NO FORCE`/`FORCE ROW LEVEL SECURITY` around
the count — independent `security` review found this **blocking**
(finding **S-1**): the restore is transaction-local, so a standalone
`psql -v ON_ERROR_STOP=1 -f` run of the file (exactly what an operator
would do to read the guard's message during an incident) leaves
`ledger_accounts` with tenant isolation silently, permanently off.
Resolved by removing the RLS toggle entirely: the sole guard mechanism is
now `ADD CONSTRAINT ... EXCEPTION WHEN check_violation`, which is immune
to RLS by construction (constraint validation scans every row regardless
of role or policy). `security` independently re-confirmed this fix in a
dedicated follow-up pass: zero RLS statements remain in the migration,
the sole mechanism is genuinely RLS-immune, and the regression-guard test
would actually catch a reintroduction of the toggle.

**Independent review (two rounds, no self-review)**: `security`,
`code-reviewer`, and `qa` each reviewed the implementation independently.
`qa` cleared it with no blocking gaps (test coverage judged sufficient
for the narrow schema-reclassification-plus-guard scope; concurrency
testing correctly judged unnecessary for this guard's shape). `security`
found S-1 (blocking, above), plus three Low/informational items
(a mutable package-level guard list; unenforced player-ownership on the
new account types; a deploy-ordering note). `code-reviewer` independently
converged on the same RLS defect from a different angle (**F4**, a
factually-incorrect fallback-degradation claim in the migration comment)
and additionally found: **F1** — the completion-status note overclaimed
invariant L1 as fully addressed while five `ledger-finance`-owned
documents (`reconciliation-model.md`'s three sites,
`03-database-architecture.md`, `06-wallet-ledger-architecture.md`,
`financial-domain-model.md`, `financial-transaction-flows.md`) still
carried the pre-split `player_locked` enumeration; **F2** — the HR-9
rejection message hardcoded "both preconditions missing," which would go
factually stale in the exact window HR-9's reworded conjunctive-removal
condition creates (`bonus_expense` landing first, alone); **F3** — a
CI-reachable test-fixture race (two packages' test files creating the
same synthetic asset codes concurrently).

**Fix wave**: `ledger-finance` (same specialist, fixing its own code
after independent reviewers found the defects — not self-review) removed
the RLS toggle entirely; reworded the HR-9 message to a requirement list
rather than a "missing" assertion; made the BONUS_SET guard list
immutable by construction; gave the two test files distinct synthetic
asset codes; completed all of its own owed §6.3.4 item 6 document edits;
and — while re-running the full suite as part of its own verification —
found and fixed a **second, previously-undetected defect**: a flaky test
(`TestGetSummary_CombinesBothLockedOriginsInEitherRowOrder`) whose
row-order-forcing harness disabled index/bitmap scans but not
`synchronize_seqscans`, letting PostgreSQL's synchronized sequential
scans rotate the observed row order under concurrent load. Fixed with
`SET LOCAL synchronize_seqscans = off` plus a bounded, still-hard-failing
retry. Also recorded **HR-15** (new, not implemented): a `BEFORE UPDATE`
trigger on `ledger_accounts` guarding `account_type`/`wallet_id`/
`asset_code`/`tenant_id` immutability is a required gate — its own
migration, its own review — before any `transaction_type` posts to a
locked-origin account (security finding **S-2**: nothing currently
prevents an `UPDATE` from retroactively falsifying invariant L1's origin
attribution or bypassing HR-9). Not a migration-0048 blocker, since
nothing posts to these accounts yet.

**Final independent re-verification**: `security` re-confirmed S-1 fully
resolved with no new defect introduced. The Orchestrator independently
verified throughout (not merely trusting specialist claims): `gofmt`,
`go build ./...`, `go vet ./...`, `golangci-lint run` (0 issues) all
clean before and after the fix wave; the full `go test -tags=integration
./...` suite run repeatedly (5+ times across both rounds) with zero
failures and no flake recurrence after the `synchronize_seqscans` fix;
migration round-trip (`up`/`down`/`up`, dirty-database down-rejection)
confirmed against both a throwaway database and the shared dev database;
every file changed was read and its diff inspected directly, not taken
on the implementing specialist's word.

**Sportsbook/Bonus Engine posting call sites remain NOT IMPLEMENTED** (no
`internal/sportsbook` package exists; cases A/D/F/H/J/K stay as
architecture only) — deliberately, per this stage's explicit boundary.

**Workstreams B/C/D/E/F — design/validation only, no code authorized:**

- **Workstream B (wagering-progress integrity, gate G-3)**: CLOSED at the
  design level via **Model C** ("dual-measure derived progress" — P_net/
  P_firm, invariant **W1**/**W2**), independently validated by
  `sportsbook`, `bonus-engine`, and `architect` across two rounds
  (`architect`'s independent cross-check found two real inconsistencies —　
  Inconsistency A/B — that neither original author's own review caught;
  both fixed). **NOT IMPLEMENTED** — no Bonus Engine package exists, and
  building one is explicitly out of scope for this stage. Status:
  **BLOCKED on Stage 4H-B1 authorization**, not on any further design
  work.
- **Workstream C (Terminal-Grant technical contract, gate G-2)**: T.1-T.13
  fully modeled (value-creating/value-reducing asymmetry; full
  immutable-vs-live-evaluated table; all three candidate posting actions
  modeled at instruction level). Does **NOT** select the human decision.
  **DESIGN COMPLETE, NOT IMPLEMENTED** (no code path exists for any of
  the three candidate actions).
- **Workstream D (self-exclusion technical hardening)**: new ADR 0034
  §14.10-§14.13 (mid-partial-settlement void resolution, `correlation_id`-
  keyed nullification per §14.11, as-of re-confirmation, audit ordering).
  Does **NOT** select the `OpenBetSelfExclusionPolicy` default.
  **DESIGN COMPLETE, NOT IMPLEMENTED** beyond what R6 already shipped
  (the policy config/resolution infrastructure itself).
- **Workstream E (sportsbook financial contract conformance)**: read-only
  validation confirming ADR 0038's accounting design is internally
  consistent with the Workstream A/B changes; no sportsbook code written.
  **DONE** (as a review, not an implementation).
- **Workstream F (Human Decision Register)**: new
  `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`,
  formalizing the three still-unmade human decisions (G-2 settlement-
  credit resolution; `OpenBetSelfExclusionPolicy` default; mixed/bonus-
  funded cashout policy, widened to include forward dependency **FD-1** —
  cashout's wagering-progress treatment must be decided together with its
  proceeds-split policy) with exact options, financial/technical/
  regulatory consequences, and an honest engineering-vs-configuration
  impact assessment. Corrected twice by `architect`'s independent
  cross-checks. **DONE.**

**Disclosed, not fixed this stage (each attributed, none hidden):**
- `LF-0048-1` — a pre-existing (confirmed unrelated to this stage's
  changes) false-positive in `internal/reconciliation`'s ledger-vs-
  projection sweep for any ledger account with no entries yet, pinned by
  a characterization test. Owner: `ledger-finance`; needs its own change.
- `HR-15` (above) — a required future gate, explicitly not built.
- The ADR 0035 `ledger_accounts_owner_family` CHECK collision (§6.5.11):
  ADR 0035's proposed constraint would reject every locked-origin account
  creation in either landing order. Owed by whichever of {migration 0048,
  ADR 0035's amendment} lands second — 0048 has now landed first.
- Two Low, optional security follow-ups from the final S-1 re-check
  (L-1: a standalone non-transactional migration run could briefly leave
  no `account_type` CHECK constraint at all; L-2: the RLS-toggle
  regression-guard test is case-sensitive and skipped without a test
  database) — neither blocks, both cheap, left as backlog.
- **Pre-existing governance-doc gap, found during this stage's own
  close-out, not introduced by it**: `docs/governance/project-status.md`
  has no dedicated sections for Stage 4H-B0-R5 or Stage 4H-B0-R6 (it stops
  at Stage 4H-B0-R4); `docs/active-stage.md`, `docs/progress.md`, and
  `docs/governance/task-registry.md` all correctly carry the full R5/R6
  record. Not backfilled this stage — recording R5/R6 retroactively into
  `project-status.md` is not this stage's purpose and was not authorized —
  but disclosed here rather than silently perpetuated.

**B1 readiness: B1 NOT READY.** `player_locked` phase 2's cash-only
ledger capability is implementation-complete and reviewed, but Bonus
Engine, Gamification, and the Reward Orchestrator remain entirely
unbuilt, and three human decisions (G-2, the self-exclusion default,
cashout policy) remain unmade. "Architecture/design exists" is not
treated as sufficient, per this stage's own directive.

---

## Stage 4H-B0-R6 — Foundational Implementation Hardening — Complete

Status: **Complete. The first implementation stage since a long
architecture-only period. Six foundational workstreams implemented with
real production code and migrations, each independently reviewed, and a
fix wave closing every confirmed defect — including one live,
exploited P1 — with a final independent re-verification pass confirming
every fix holds. No implementation stage is authorized to begin next.
Stage 4H-B1 (Bonus Engine) NOT started.**

Six authorized workstreams, per the stage directive: **A** Asset
Registry + Authorization implementation, **B** financial idempotency
hardening, **C** `player_locked` accounting implementation, **D** Risk
fail-closed hardening, **E** RG self-exclusion technical hardening, **F**
Bonus dependency contract closure.

**Wave 1 (6 parallel implementation dispatches, distinct file
ownership, exclusive reserved migration-number ranges to prevent
collision):**
- `architect` — Workstream A: `internal/assetregistry` (new), migrations
  `0044`-`0045`. Fixed `assets.active`'s fail-open default; added a
  `platform_authorized` layer with an RLS+FORCE+trigger backstop (no
  second Postgres role exists to do this properly — disclosed limitation);
  immutable identity-field enforcement; a `(product, operation)`
  dimension on layers 4-7 (closing sportsbook's own Stage 4H-B0-R5
  finding); four-eyes (`asset_change_requests`/`asset_change_approvals`)
  mirroring the `withdrawal_approvals` precedent.
- `identity-compliance` — Workstream E: migration `0043`,
  `internal/rg/self_exclusion_*.go`. Policy config schema (jurisdiction-
  primary, tenant/brand tighten-only, enforced at both write time via a
  DB trigger and read time via `max(strictness)` aggregation), as-of
  temporal resolution, authoritative `clock_timestamp()` throughout, a
  stalled-enumeration-run detection primitive. Explicitly did NOT select
  the platform-wide default policy value.
- `risk` — Workstream D: `internal/risk/{cumulative,denomination}.go`,
  migration `0046`. Fixed the ADR 0031 §32 latent fail-open (leg-aware
  cumulative-usage query, self-defending against an undeclared leg);
  closed seven accidental-ALLOW paths in `evaluator.go`; added
  decimal-exponent awareness to `risk_rules` (validated at 0/2/6/8/18);
  resolved the ADR 0031/0038 Risk-checkpoint conflict on Risk's side
  (new §36, visible superseded-in-part markers).
- `ledger-finance` — Workstream C, phase 1 only (implementation ADR,
  explicitly not blind-implemented): `ledger-accounting-model.md` §6.4,
  the 12-case (A-L) accounting-flow reference. Deferred mixed cash+bonus
  funding for this pass (a fail-closed rejection at placement, HR-2 —
  the anti-structuring control it would need is bonus-engine's to design,
  not ledger-finance's to invent unilaterally) and cashout (no sportsbook
  code exists to offer one). No migration or code written this phase.
- `integrations` — Workstream B: new `internal/idempotency` package
  closing the two vulnerabilities security found in Stage 4H-B0-R5
  (unauthenticated fallback discriminator; unescaped collision-prone key
  composition), proven collision-free over 262k+ adversarial pairs. Zero
  production call sites — a shared primitive, not yet adopted by any
  domain.
- `bonus-engine` — Workstream F: `docs/architecture/10-*.md`'s new "Bonus
  Dependency Contract Freeze" — the exact contracts Bonus will depend on
  (Asset Registry, AssetAuthorization, Risk, RG, Wallet/Ledger, Activity/
  Event taxonomy, rounding_rules, conversion boundary, `player_locked`
  origin, external provider-native bonus coexistence), plus an explicit
  "Bonus must never build" list. Documentation only.

**Wave 2 (6 independent reviewers, none reviewing their own Wave 1
work):**
- `sportsbook` and `bonus-engine` each validated Workstream C's
  implementation ADR against the exact questions ledger-finance posed.
  `bonus-engine` confirmed a real, structurally-triggered gap while
  answering one of them: bonus-funded sportsbook wagering progress is
  never netted against a later void/rollback of the same lock — a
  player-reachable progress-farming vector, generalizing far beyond a
  Stage 4H-B0-R5 self-exclusion-specific finding — with a precise fix
  design specified (gate **G-3**). `sportsbook` also confirmed the
  terminal-Grant human-decision gap (gate **G-2**) blocks bonus-only
  funding directly, not only the deferred mixed-funding case.
- `product-owner-proxy` and `sportsbook` also each did a dedicated
  sportsbook-readiness/scope-check pass, finding the three foundational
  primitives (idempotency, asset authorization, risk) sufficient for a
  future sportsbook slice in both provider modes, and no scope creep in
  this stage's output.
- `architect` (independent of its own Workstream A) did a
  cross-workstream consistency pass: single exponent source of truth
  confirmed load-bearing (not just tidy — routing Risk's exponent lookup
  through `CheckEligibility` would have denied every live casino bet,
  since all seven seeded assets are `platform_authorized=false`); RLS/
  backstop idiom consistent between Workstreams A and E; no idempotency
  routing conflict; repo-wide build/vet/fmt independently reconfirmed
  clean. Filed two real documentation-level cross-references (a stale
  `CheckEligibility` signature citation in Workstream F; confirmation
  that Workstream C's §6.4 neither resolves nor contradicts the ADR
  0031/0038 conflict).
- `qa` independently re-ran and read the actual test bodies (not just
  names) across A/B/D/E — confirmed the bulk of it genuine, but found
  three real gaps: Workstream B's changed-asset replay test didn't
  actually vary the asset; Workstream D had no TOCTOU race test for the
  cumulative-limit advisory lock; Workstream E had no cross-tenant RLS
  test for either of its two new tables.
- `code-reviewer` found two real High-severity defects: **F1** the
  platform-wide layer-7 eligibility grant had no four-eyes representation
  at all despite ADR 0037 requiring one; **F2** `self_exclusion_enumeration_runs`'
  RLS policy was missing the `app.player_account_id IS NULL` conjunct
  every sibling table in the same migration correctly carried — a real
  cross-player compliance-data exposure. Plus F3 (a silent fail-open on
  a wrongly-scoped reconciliation call, the same shape Workstream D
  explicitly fixed elsewhere this stage) and six lower-severity findings.
- `security` independently reproduced a **live P1**: the four-eyes
  self-approval person-identity check was unconditionally inert, because
  no code path anywhere in the platform could ever set `person_id` on a
  `platform_admin` account — a single human running `seed-admin` twice
  could complete every dual-controlled operation (create/activate/
  platform-authorize) alone. Reproduced end-to-end against the live dev
  database. Also confirmed code-reviewer's F1/F2 independently, found a
  DELETE-based RLS-widening exploit on `asset_authorizations`, and
  confirmed Workstream D clean (IMPLEMENTED, no findings).

**Fix wave (4 dispatches, each to the specialist owning the affected
code, closing every confirmed defect):**
- `architect` hardened the four-eyes trigger to require a resolved,
  non-NULL `person_id` and active status on both sides (migration 0047,
  mirroring the withdrawal-governance precedent instead of an earlier,
  since-withdrawn version); added a fourth dual-controlled operation for
  the layer-7 eligibility grant (an `AFTER` trigger, after a failing
  test caught that a `BEFORE` trigger double-fires on `ON CONFLICT DO
  UPDATE`); split `asset_authorizations`' RLS to remove the DELETE-
  widening path; added TRUNCATE deny-triggers. Verified fail-before/
  pass-after against a literal reproduction of security's exploit.
- `identity-compliance` built the person-linking path the above fix
  depends on (new `cmd/seed-admin` flags; a new platform-scoped
  remediation route, explicitly checked against a `tenant_admin` caller
  who also holds the same permission) — confirmed the two fixes land
  together with an end-to-end test proving the dependency is satisfied,
  not assumed. Also closed the RLS conjunct gap, the reconciliation
  scope-verification gap, a jurisdiction-floor backdating exploit (with
  a real bug of its own caught and fixed: a naive check failed every
  legitimate write due to `clock_timestamp()` drift), and an RLS-policy
  alignment gap. Self-resolved a migration-number collision with
  architect's parallel dispatch by using `0049`.
- `integrations` trimmed genuinely dead code from `internal/idempotency`
  (four zero-value type aliases, an unimplemented interface, several
  unreferenced functions) per CLAUDE.md's no-uncontrolled-scope-expansion
  rule, and closed the changed-asset test gap.
- `risk` consolidated the exponent lookup through `internal/assetregistry.GetAsset`
  and added a genuine, mutation-tested TOCTOU race test proving the
  cumulative-limit advisory lock actually prevents overshoot (confirmed
  by temporarily removing the lock and watching the test fail 10/10
  runs, then restoring the file byte-identical).

**Final independent re-verification (2 dispatches):** `security`
re-reproduced the original P1 exploit against the fixed code and could
not reconstruct it by any route tried — **CONFIRMED CLOSED**; Workstream
A's four-eyes control and RLS backstop are now labeled IMPLEMENTED.
Found five new minor items, only one non-trivial (the stalled-run
detector has no scheduler wiring — labeled PARTIALLY IMPLEMENTED, not
launch-blocking). `qa` independently re-verified all three originally-
flagged test gaps are genuinely closed and confirmed the two new
four-eyes regression tests are real; full 588-test integration suite
green, zero skips/failures; migration round-trip clean (one pre-existing,
unrelated Stage 4E migration-0039 down-migration limitation confirmed
real but fresh-database-safe).

**Final labels** (CLAUDE.md's no-fake-completion rule): Workstream A
(Asset Registry) — **IMPLEMENTED** (layer 8 market-rate-availability
NOT IMPLEMENTED, concluded to be a runtime FX-provider check, not a
stored fact). Workstream B (idempotency) — **IMPLEMENTED** as a shared
primitive, zero production call sites. Workstream C (`player_locked`) —
phase 1 (ADR) **DONE**; phase 2 (migration `0048` + code) **NOT
STARTED**, gated on G-2 (human decision) and G-3 (fix design specified,
not built) for bonus-funded cases; cash-only cases unblocked. Workstream
D (Risk) — **IMPLEMENTED**, security sign-off granted, no findings.
Workstream E (RG self-exclusion) — **PARTIALLY IMPLEMENTED** (scheduler-
wiring gap, non-launch-blocking; default policy value remains an unmade
human/legal decision). Workstream F (Bonus dependency contract) —
**DONE**.

**No fake completion**: every residual finding — the two open gates
blocking bonus-funded sportsbook (G-2, G-3), the unwired stalled-run
scheduler, FX/Conversion Part B's untouched security findings, the
pre-existing migration-0039 limitation — is recorded, attributed, and
routed to its owning specialist, none hidden or downgraded.

---

## Stage 4H-B0-R5 — Implementation Readiness and Final P1 Closure — Complete

Status: **Complete. All five Stage 4H-B0-R4 P1s are ARCHITECTURALLY
RESOLVED. None is fully implementation-ready — each carries a catalogued
list of residual findings surfaced by this stage's own review process,
recorded below and in the cited documents, that must be closed before or
during implementation. No implementation stage is authorized. Documentation/
ADR-only stage — no code, no migrations, no provider integration.**

Purpose: close the five P1s Stage 4H-B0-R4 disclosed (FX rate-plausibility,
Asset Authorization RBAC surface, idempotency per-occurrence design, the
`player_locked` origin-split, `OpenBetSelfExclusionPolicy`) and reach a
genuine implementation-readiness verdict, not a documents-were-created
verdict, per this stage's own directive.

**Wave 1 — none needed.** All five P1s already had an authoring specialist
from Stage 4H-B0-R4; this stage began at review/closure.

**Wave 1 (closure-authorship, 4 specialists, distinct file ownership):**
- `architect` — closed P1-1 and P1-2 with new ADR 0037 §B.7 (rate-
  plausibility: category A universal vs. category B configurable checks,
  a 9th fail-closed condition for cross-provider disagreement) and new
  §C.5 (Asset Authorization administrative API surface: 9 canonical
  operations, four-eyes reasoning, immutable/mutable field split). Also
  added §2.5 to doc 09 (canonical sportsbook identity clarification).
- `ledger-finance` — closed P1-3 with new ADR 0038 §14 (idempotency
  contract: `occurrence_ordinal`, strictly increasing per
  `(tenant_id, correlation_id, transaction_type)`, composed into the
  existing `provider_tx_id` string, no schema change) and proposed the
  P1-4 resolution as new `ledger-accounting-model.md` §6.3 (Shape A: split
  `player_locked` into `player_locked_cash`/`player_locked_bonus` via
  additive `account_type` CHECK widening, explicitly PROPOSAL ONLY,
  requiring independent review before being treated as decided, per
  CLAUDE.md's rule against unilaterally redesigning the human-approved
  ledger schema — precedent: the Stage 4H-B0-R1 agent-float amendment).
  Cross-referenced from new ADR 0038 §15.
- `identity-compliance` — closed P1-5 with new ADR 0034 §14
  (`OpenBetSelfExclusionPolicy`: exactly 2 values, `SETTLE_NORMALLY` /
  `VOID_ON_SELF_EXCLUSION`, jurisdiction-primary tighten-only scope, new
  audit trigger point; explicitly did NOT select the platform-wide
  default value — left as a human/legal decision).
- `sportsbook` — added doc 09 §15 (external-first vs. in-house-first
  sequencing recommendation: external-first, 7/9 dimensions favor it —
  a business/engineering recommendation, not a permanent architectural
  constraint).

**Wave 2 (4 specialists, independent review of the P1-4 `player_locked`
proposal — none reviewed its own authored work):**
- `sportsbook`, `architect`, `bonus-engine` each independently reviewed
  Shape A against ledger-finance's own posed review questions. All three
  **approved Shape A** (the schema shape, the extended Invariant B1, and
  cases A-G) but each found a specific completeness gap: sportsbook found
  the worked cases proved the mixed-funded split at *lock* time but never
  worked through the *unlock*-side cases (void/settlement/partial/
  cashout); architect found a factually incorrect "already done once
  successfully" precedent claim (`bonus_expense` was never migrated) and
  a real silent-defect call site (`internal/wallet/wallet.go`'s
  `GetSummary` switch would zero `LockedBalance` for split accounts);
  bonus-engine found Rule B2 (not just Invariant B1) needed restating as
  a boundary-crossing rule, and — while investigating a posed question
  about forfeiture of a currently-locked bonus-funded stake — found a
  **real, structurally-triggered gap**: once a Grant goes terminal
  (expired/cancelled/forfeited) while a portion remains locked, a later
  settlement or self-exclusion-triggered void credit against that Grant
  has no defined state-machine transition. This is the *same* unresolved
  question ADR 0034 §2 already left open (completing an already-satisfied
  wagering requirement post-self-exclusion) reached by a second, concrete
  trigger path — not a new question. Recorded as an explicit **Human
  decision required** cross-reference (doc10 §5, ADR 0032 §5), not
  resolved by any specialist.
- `ledger-finance` (follow-up) independently closed a real database-level
  idempotency hole `bonus-engine`'s Wave 2 review found: the partial
  unique index `UNIQUE (tenant_id, provider_id, provider_tx_id) WHERE
  provider_id IS NOT NULL` never evaluates for in-house-mode postings
  (`provider_id` NULL per ADR 0033 §2), leaving in-house sportsbook
  postings with **no database-level idempotency enforcement at all** —
  new ADR 0038 §14.6: in-house-mode postings route through
  `UNIQUE (tenant_id, idempotency_key)` (unconditional) instead,
  `provider_id`/`provider_tx_id` stay NULL (never a reserved sentinel),
  justified against two existing precedents (ADR 0033 §2's own rule;
  `internal/audit`'s `ActorType`/`ActorID` discriminator-plus-empty-field
  pattern).

**Wave 2b (targeted gap closure, 3 dispatches, each routed to the
specialist owning the affected document):**
- `ledger-finance` closed all four gaps Wave 2 found: withdrew the false
  precedent claim; added the `wallet.go` `GetSummary` fix to the
  implementation checklist (§6.3.4); added the mixed-funded unlock-side
  worked cases (§6.3.3.2: C-void, C-loss, C-win, C-partial) plus the
  settlement-time split-recovery mechanism (§6.3.3.1); restated Rule B2
  as a boundary-crossing rule over `{player_bonus, player_locked_bonus}`
  with a worked numeric combined-transaction proof. In doing so, produced
  **new, explicitly-unreviewed content** requiring its own sign-off: a
  proposed C-win proportional payout-split rule, and an explicit
  **OPEN QUESTION** for C-cashout (two ledger-balanced candidates,
  the deciding factor is bonus-abuse/consumer-protection policy, not
  ledger mechanics).
- `sportsbook` added doc 09 §6.1 (in-house-mode ledger-posting
  idempotency routing statement, per the requirement ledger-finance
  flagged in §14.6).
- `bonus-engine` added the terminal-grant cross-reference note (doc10 §5,
  ADR 0032 §5) as an explicit **Human decision required** item with three
  named options (re-forfeit / route to `player_cash` / manual-review
  queue), none selected.

**Wave 3 (6 specialists — joint C-win/C-cashout decision input, plus the
directive's required independent challenge of all five P1s by reviewers
independent of every Wave 1/2 author):**
- `bonus-engine`: **APPROVE-WITH-CHANGES** on C-win — sound in shape, but
  found a real, concrete bonus-abuse/structuring vector (a cash-dominant/
  bonus-sliver mixed stake can be structured so `bonus_share` always
  rounds to zero, converting the bonus sliver to withdrawable cash on
  every win with no mirror or detection); requires an anti-structuring
  control before implementation. C-cashout input: recommends
  "not cashout-eligible" first, proportional-with-control as fallback,
  rejects all-to-cash outright. Confirmed the terminal-grant gap
  generalizes (asset deactivation mid-campaign hits the same root cause).
  Found `VOID_ON_SELF_EXCLUSION` doesn't net the lock-time
  wagering-progress debit against the reversal (full wagering credit for
  a stake that was never actually risked).
- `sportsbook`: found the C-win split-recovery computation must live
  inside `internal/ledger`'s settlement handler, not sportsbook itself
  (sportsbook has no non-drifting source for the original ratio at
  settlement time); found P1-2 has no casino-vs-sportsbook product
  dimension, in tension with the platform's per-product licensing model;
  found P1-5's `VOID_ON_SELF_EXCLUSION` isn't fully specified for a
  multi-leg bet caught mid-partial-settlement (ADR 0038 §8.1 gap).
- `product-owner-proxy`: recommended "not cashout-eligible" as the right
  MVP-scope answer for C-cashout (lowest-risk, trivially reversible
  later); ran a scope-check across this stage's entire output and found
  **no scope creep** — every section maps 1:1 onto one of the five named
  P1s, and the stage repeatedly declines to overreach (disclaiming
  authority to decide C-win, refusing to resolve C-cashout by
  extrapolation).
- `security` (independent, first look at all five P1s): found **9 P1-level
  gaps** — FX control-plane's own bounds (deviation/staleness/spread) have
  no RBAC tier, no dual control, no mandatory audit; single-provider
  plausibility check is circular; Asset Authorization layers 1-3 have no
  RLS backstop despite a "structurally cannot reach" claim (`assets`
  isn't tenant-scoped); `assets.active` defaults to `true` in the live
  schema, contradicting the fail-closed design; four-eyes is asserted
  with no enforcement mechanism; tenant/jurisdiction aren't required to
  be server-sourced; the idempotency `occurrence_ordinal` fallback
  derives from an unauthenticated transport-level signal (a real
  double-post vector), and its string-concatenation composition has no
  delimiter discipline (collision risk); the self-exclusion policy's
  "resolved fresh, never cached" language has no as-of timestamp anchor
  (a tampering window), and its per-bet audit records can't prove
  enumeration completeness. Found **no tenant-isolation defect** in P1-4
  (verified RLS/FORCE RLS directly against migrations).
- `qa` (independent, first look): found P1-1/P1-3/P1-4 testable-as-
  specified; found P1-2's layers 4/6 (tenant/jurisdiction) cannot be
  independently tested despite a claim of per-layer distinguishable
  reason codes (they resolve from one row); found P1-5's jurisdiction-
  floor enforcement point (write-time vs. read-time) is unspecified and
  its new self-exclusion-commit listener has zero named test cases.
- `risk` (independent, first look, verified against actual code not
  document prose): found **no gap** in P1-1/P1-4/P1-5 from Risk's angle;
  found a real interaction gap in P1-2 (asset-agnostic `risk_rules`
  thresholds have no decimal-exponent awareness — authorizing a new
  asset with a different exponent can silently turn an existing wildcard
  amount cap into an effectively-unlimited or always-denying rule); while
  investigating P1-3, **discovered a latent fail-open already live in
  `internal/risk`'s own code** (`Rule.breach()`'s cumulative-usage query
  never joins `ledger_accounts`, so it is blind to `account_type` — works
  by accident for `casino_bet` but would make a `sportsbook_bet`
  cumulative-amount rule always compute zero usage once wired, since both
  its legs are player-owned and net to zero). Not exploitable today (only
  `casino_bet` is mapped) but must be fixed before that mapping widens.
  Also flagged a real cross-document conflict: ADR 0031 §26/§31 call
  `sportsbook_settlement`/`sportsbook_cashout` "near-term, load-bearing"
  Risk Operations; ADR 0038 §13 states they are "not additional Risk
  checkpoints" — escalated for Orchestrator assignment, not resolved.

**Overall P1 closure verdict**: architecturally resolved, not fully
implementation-ready. See `docs/decisions/0037-*.md` §B.7/§C.5,
`docs/decisions/0038-*.md` §14/§14.6/§15, `docs/architecture/
ledger-accounting-model.md` §6.3, and `docs/decisions/0034-*.md` §14 for
full text; `docs/security/security-architecture.md` and
`docs/testing/testing-strategy.md` (both new Stage 4H-B0-R5 sections) and
ADR 0031 §32 for the independent-review findings. No finding was asserted
as blocking the *architecture* status of any P1; several are asserted as
blocking *implementation* and are catalogued as such. Full dependency-
ordered implementation contract and residual-findings-by-owner list: see
this stage's completion report (delivered to the human alongside this
update) and `docs/governance/project-status.md`'s Blocked-stages section.

**No fake completion**: this stage explicitly did not attempt to close
every finding Wave 3 surfaced — most require code or migrations, out of
scope for a documentation-only stage per this stage's own directive.
Every open finding is attributed to its discovering specialist and
routed to its owning specialist; none was downgraded or hidden.

---

## Stage 4H-B0-R4 — Asset/Currency Registry, FX/Conversion, and Dual-Mode Sportsbook Architecture Closure — Complete

Status: **Complete. Architecture READY FOR IMPLEMENTATION for the Asset/
Currency Registry, FX/Conversion, and Sportsbook (external + in-house)
domains. None authorized to begin. Bonus Stage 4H-B1's own gate and
Retail's Stage 4H-B2 gate are unaffected by this stage.**

Architecture-only stage — no code, no migrations, no provider
integration, no real vendor named. Closes doc 27 §26's deferred Asset/
Currency Registry requirement and formalizes a new mandatory
requirement: the platform must architecturally support both an external
sportsbook-provider integration and a strong in-house sportsbook engine,
provider-neutral, co-equal from day one.

**Two waves of specialist work, no self-review:**

**Wave 1 (5 specialists, parallel authorship, distinct file ownership):**
- `architect` — new `docs/decisions/0037-asset-currency-registry-and-fx-
  conversion-architecture.md`: an 8-layer Asset Authorization model
  (existence → activation → platform authorization → tenant → brand →
  jurisdiction → operation eligibility → market-rate availability, each
  independently gated, none implying the others); a 4-component FX
  architecture (Registry / FX Rate Provider interface / Conversion
  Service / Ledger's existing `ConversionOperation`, never coupled) with
  an 8-condition fail-closed rule; one canonical
  `AssetAuthorization.CheckEligibility` service every domain must
  consume, closing Stage 4H-B0-R3's flagged authorization-boundary P1.
- `sportsbook` — rewrote `docs/architecture/09-sportsbook-architecture.md`
  (48 → ~1050 lines): canonical domain model (Sport/Competition/Event/
  Market — split into MarketType template vs. Market instance/
  Participant/Selection/Price, with Exposure explicitly distinguished
  from Liability to prevent a "second wallet" mistake), provider-neutral
  `SportsbookProvider`/`DataFeedProvider` abstractions, a five-layer
  in-house engine (Sports Data → Business Logic → Ledger → Risk →
  Trading Operations), tenant/brand/jurisdiction mode-selection routing.
  The original Stage-0 "start with widget/iframe" recommendation is
  preserved as operational sequencing guidance, not an architectural
  constraint the new dual-mode requirement would otherwise contradict.
- `ledger-finance` — new `docs/decisions/0038-sportsbook-accounting-and-
  ledger-integration.md`: no new account types, 6 new transaction types
  (`sportsbook_bet`/`_settlement`/`_void`/`_partial_settlement`/
  `_cashout`/`_rollback`), an idempotency key design keyed on
  `(tenant_id, provider_id, provider_tx_id)` with a `correlation_id`
  tying a bet's full lifecycle together, aggregate open-bet liability as
  a derived read (never a maintained counter), market-correction
  reversal as a two-transaction compensating sequence (never a balance
  edit).
- `risk` — added ADR 0031 §25-31: confirmed `sportsbook_bet` already
  exists as a real `Operation`; proposed `sportsbook_settlement`/
  `sportsbook_cashout` as new gated checkpoints; determined market/
  selection-level trading-exposure management is a Sportsbook Engine
  trading concern, not gated by `risk.Evaluate` (mirroring the existing
  Bonus-campaign-budget-cap precedent), while per-player stake/payout/
  cashout limits remain ordinary Risk rules.
- `identity-compliance` — added ADR 0034 §9-13: RG evaluated fresh at
  placement and at cashout (a discretionary, time-displaced,
  value-crediting action), RG-exempt at settlement (no intervening
  player-contributed progress between acceptance and outcome, unlike a
  Bonus wagering requirement); flagged the open-bet self-exclusion
  question as a genuine human/compliance decision, not decided
  unilaterally.

**Wave 2 (4 specialists, independent review — none reviewed its own
authored work):**
- `architect` reviewed the other four documents, reconciling `risk`'s
  Operation proposals and `sportsbook`'s final domain model (both had
  been drafted in parallel without seeing each other's final output) —
  found and fixed two precision gaps in ADR 0033 (in-house
  `provider_id`/bonus-source degeneracy), found one real contradiction
  in ADR 0038 (below), flagged a terminology collision ("Cancellation"
  meaning two different things in doc 09 vs. ADR 0038 — routed to
  `ledger-finance`, fixed), and independently re-verified extensibility
  items 10-18.
- `ledger-finance` independently reviewed `architect`'s ADR 0037 (not
  its own ADR 0038) — found a missing `rounding_rule_id` field-mapping
  gap, an unstated tenant/wallet-context sourcing question, a missing
  rate-plausibility caveat, and a mischaracterized ADR 0031 §8 precedent
  ("already established and implemented" — actually itself an open,
  unbuilt gap by ADR 0031's own words).
- `security` independently reviewed all five documents — found the same
  rate-plausibility gap `ledger-finance` found (independent convergence)
  plus two fail-closed/error-contract gaps in the Asset Authorization
  boundary, confirmed provider-callback authentication is already
  correctly specified (citing the existing hardened casino-callback
  precedent), confirmed tenant isolation and audit coverage, and flagged
  one operational (non-blocking) gap: an RG denial after a widget/iframe
  provider already accepted a bet should trigger the adapter's own
  cancel call back to the provider.
- `qa` ran the full 20-item extensibility test — **all 20 answered YES**,
  each independently verified against actual document content, not
  documents' own summary claims. Designed (architecture-level only) an
  adversarial idempotency-collision test category and an authorization-
  widening defense-in-depth test category, both flagged as required
  future test-plan additions, not present defects.

**One real, substantive contradiction found and fixed**: ADR 0038 §13
originally proposed netting `sportsbook_rollback` against a player's
cumulative daily stake usage, alongside `sportsbook_void` — this
contradicted the same ADR's own §10, which is explicit that a rollback
corrects a wrongly-recorded settlement outcome without nullifying the
underlying staked bet (unlike void, which returns the full stake as if
the bet never happened). Netting both would have zeroed out, or in a
composite rollback-then-void case gone negative on, a player's genuinely
staked amount after any market-correction event. `ledger-finance`
corrected this directly once identified: `sportsbook_rollback` is never
netted against cumulative stake usage; the composite case is already
handled correctly by the void leg alone.

**Five smaller gaps closed** as narrow, explicitly-attributed additions
(all in `docs/decisions/0037-*.md` and `0038-*.md`; no redesign): two
fail-closed/error-contract statements for the Asset Authorization
boundary (absent config = deny; non-nil error = ineligible, both
mirroring existing platform conventions from ADR 0031/CLAUDE.md's
Redis-never-authoritative rule); a rate-plausibility caveat on the FX
fail-closed rule (structural checks don't catch a well-formed-but-wrong
rate from a compromised provider — named as a required implementation-
time control); a missing `rounding_rule_id` field-mapping between the
Conversion record and `ConversionOperation`; and the mischaracterized
ADR 0031 §8 precedent, corrected.

**Final gate — see `docs/governance/project-status.md`'s Stage 4H-B0-R4
section for the full A-L verdict.** Bottom line: all closed, no P0. Five
disclosed, non-blocking P1s recorded for implementation time: a
rate-plausibility check requirement; the Asset Authorization boundary's
concrete RBAC/API surface still to be designed; ADR 0038's idempotency
scheme's residual same-type/coincidental-payload collision risk needing
a per-occurrence distinguishing mechanism; the pre-existing
`player_locked` origin-split decision still blocking bonus-funded (not
cash-funded) sportsbook wagering; and the open-bet self-exclusion policy
as a genuine human/compliance decision.

**No production code, no migrations, no provider integration, no real
vendor was named or implemented this stage.** `go build ./...` remains
clean (docs-only diff).

### Decisions/input needed before the next stage

None block architecture closure. Before a real implementation stage:
(1) the open-bet self-exclusion policy decision (above); (2) the
`player_locked` origin-split decision for bonus-funded sportsbook
wagering (pre-existing, ADR 0032 §10); (3) selecting and contracting a
real sportsbook provider and/or committing engineering resources to the
in-house engine — neither is authorized by this stage.

---

## Stage 4H-B0-R3 — Bonus Rounding Decision Validation and Financial Gate Closure — Complete

Status: **Complete. Stage 4H-B1 is READY FOR HUMAN AUTHORIZATION after
completion of the `bonus_conversion` Risk dependency.**

The human proposed answers to the three ADR 0021 rounding questions
(DS-1/DS-2/DS-3) raised in Stage 4H-B0-R2's decision sheet. This stage's
job was to validate those proposals against the existing architecture —
never to select or silently change them — using six specialists in
parallel (`ledger-finance`, `bonus-engine`, `risk`, `architect`,
`security`, `qa`), then formally record the decision if safe and
re-check the overall financial gate.

**Proposed and recorded**: DS-1 = round-half-up (ties away from zero);
DS-2 = round once, at the final monetary boundary, full `NUMERIC`
precision until then, via an explicit shared function, never an implicit
database cast, no truncate-and-carry unless the architecture requires
it; DS-3 = one platform-wide rule by default, room for a future
per-asset/jurisdiction override if genuinely required.

**Validation verdict: VALID AND READY TO RECORD.** No specialist found a
contradiction, financial problem, precision problem, reconciliation
problem, or architecturally unsafe consequence requiring the human to
change the proposal. Specific findings folded into the recorded decision
as clarifications, not changes:

- **Zero-rounding-result edge case** (`ledger-finance`): a bonus that
  computes to exactly 0 minor units cannot post (`ledger_entries.amount
  > 0`). This is a Bonus Engine eligibility/config question (e.g. a
  minimum-deposit guard), not a rounding-rule question — flagged for
  Stage 4H-B1, does not block recording the decision.
- **Wagering-requirement vs. contribution-weighting distinction**
  (`bonus-engine`, resolving an ambiguity `ledger-finance` had flagged):
  the wagering-requirement target is a comparison threshold, never
  posted to the ledger, so DS-2's "final monetary boundary" language
  doesn't apply to it in the posting sense. Per-game contribution
  weighting **is** monetary — it determines the actual cash/bonus split
  posted for a wagering event — and DS-2's boundary for it is the
  split-instruction computation. Recorded explicitly in ADR 0021 so this
  is not left ambiguous for Stage 4H-B1.
- **Cashback residual — confirmed consequence, not silently assumed**:
  the existing architecture has no remainder-accumulation mechanism
  anywhere (`ledger-finance` searched all four relevant documents).
  Building one (truncate-and-carry) would be new, unauthorized
  architecture. What DS-1+DS-2 as recorded actually mean for cashback:
  each calculation rounds independently and immediately, no
  accumulation — recorded explicitly rather than left implicit, per
  `ledger-finance`'s own recommendation to surface this back to the
  human.
- **Exact multiply→cap-compare→round ordering** (`bonus-engine`): DS-2
  says "round once at the final boundary" — the only reading consistent
  with that language is rounding `min(exact_%_result, cap)` once, after
  both the percentage multiply and the cap comparison, not the raw
  percentage result before the cap decision. Recorded explicitly.
- **Coupon's Reward-axis shape** (`bonus-engine`): doc 10 does not pin
  down whether an in-slice Coupon is configured as a flat grant or a
  %-based Offer — flagged as an open Stage 4H-B1 template-authoring
  scoping question, unrelated to the rounding decision.
- **Exact algorithm specified** (`ledger-finance`): round half away from
  zero — `sign(x) × floor(|x| + 0.5)` — via one named, shared function,
  never a bare `ROUND()` call or an implicit `NUMERIC(38,0)` column-scale
  coercion (PostgreSQL's own implicit coercion happens to match
  round-half-up for positive values today — a coincidence, not a
  specification, and must never be relied upon). Negative-input contract
  specified for future FX/commission reuse even though no bonus call
  site produces a negative input today.
- **Storage location specified** (`security`): an immutable, append-only
  `rounding_rules` reference table (one composite version per Q1+Q2
  combination) plus the applied identifier denormalized onto
  `ledger_transactions` at post time — mirroring the existing
  denormalization rationale already used for `tenant_id`/`wallet_id`/
  `player_account_id`/`asset_code` on `LedgerEntry`.
- **Risk evaluates only the post-rounded amount** (`risk`, settled by
  construction, no ambiguity found or manufactured): `RiskRequest.
  Amount`'s `int64` type cannot hold a pre-rounding exact value, and the
  one live precedent (`postBet`) already passes Risk the identical
  integer that becomes the ledger posting. No Risk-side change is a
  prerequisite for recording this decision.
- **Multi-asset genericity confirmed** (`qa`): the algorithm is exponent-
  agnostic by construction (operates on minor-unit integers, never
  hardcodes a decimal count) — validated across 0-, 2-, 6-, 8-, and
  18-decimal assets, with two schema-legal-but-not-yet-populated
  exponents (0 and 18) needing synthetic test fixtures rather than live
  registry rows.

**Full recorded decision**: `docs/decisions/0021-multi-asset-
accounting.md`'s "Rounding and precision — RESOLVED" section — the
authoritative text, including the algorithm, storage specification, and
all clarifications above. `docs/architecture/28-bonus-financial-gate-
decision-sheet.md` is now marked RESOLVED and retained as the
historical record of the question as originally put to the human.

**`bonus_conversion` re-verified, unchanged**: `risk` re-checked all six
ADR 0031 §16 artifacts directly against current repository state (commit
`aa4a926`, confirmed docs-only) — still **NOT STARTED, zero of six
steps**. Per the directive's explicit instruction ("prefer keeping
implementation for Stage 4H-B1... do not silently implement it unless
this stage's final gate explicitly determines it is necessary"), `risk`
confirmed the rounding decision requires no Risk-side implementation now
— it was not implemented.

**Final B1 gate review**: **Stage 4H-B1 is READY FOR HUMAN
AUTHORIZATION after completion of `bonus_conversion`.** No P0, no P1
financial blocker, no unresolved accounting decision, no unresolved
precision decision, no unresolved rounding ambiguity remains —
confirmed by `architect`'s re-run of the same 12-area focused review
from Stage 4H-B0-R2.

**New confirmed product requirement — extensible Asset/Currency Registry
+ FX/Conversion architecture (analysis only, not implemented)**:
`architect` found the `assets` schema is already open/extensible
(no closed enum, no hardcoded decimal count anywhere in `internal/`) but
lacks the operational surface the requirement needs — an admin API, an
authorization model for who may register/activate an asset, mandatory
audit logging on that mutation, and additional per-asset eligibility
columns. Recommended a future ADR (0037) and a dedicated future
implementation stage, both recorded as deferred
(`docs/architecture/14-mvp-scope-and-roadmap.md`,
`docs/architecture/27-*.md` §26). The FX/Conversion boundary was
designed on paper only — Asset/Currency Registry, FX Rate Provider,
Conversion Service, and the Ledger's existing `ConversionOperation` kept
structurally separate, with the immutable audit fields any future
conversion must retain specified. Confirmed: **no impact on Bonus Stage
4H-B1; no new blocker for Retail** beyond the pre-existing conversion-
clearing-account open decision. Confirmed: **no expansion of the Bonus
MVP, no FX implementation, no new payment/custody provider, no retail
implementation** this stage. Two P1 risks flagged for the eventual
Registry design (undefined asset-creation authorization boundary;
fail-closed FX behavior must be written into the new ADR as a binding
rule now).

**No production code, no migrations, no implementation was authorized or
started this stage.** `go build ./...` remains clean (docs-only diff).

### Decisions/input needed before the next stage

None remain for the Bonus rounding decision — it is resolved. Stage
4H-B1 can be authorized once `bonus_conversion`'s six-step checklist
(ADR 0031 §16a) is completed as part of that stage's own work. The
Asset/Currency Registry + FX architecture requires no immediate human
decision — it is recorded as a deferred future stage with a
recommended future ADR number, not something blocking any currently
open stage.

---

## Stage 4H-B0-R2 — Bonus Financial Gate Clarification — Complete

Status: **Complete, pending an explicit human decision on the ADR 0021
rounding questions before Stage 4H-B1 can be authorized.**

A financial-gate clarification stage — no architecture redesign, no
production code, no migrations. Purpose: close the remaining financial-
design gate for Bonus implementation by preparing an exact human
decision package for ADR 0021 and independently re-verifying the
remaining Risk dependency and every other financial-gate area. Six
specialists (`ledger-finance`, `bonus-engine`, `risk`, `architect`,
`security`, `qa`) reviewed in parallel, each verifying directly against
current repository state at HEAD rather than trusting prior-stage prose.

**New document**: `docs/architecture/28-bonus-financial-gate-decision-
sheet.md` — the deliverable this stage exists to produce. Written for a
non-accountant business owner. Contains:

- **DS-1/DS-2/DS-3**: the three linked human decisions inside ADR 0021's
  rounding/precision `OPEN DECISION` (rounding direction; where and how
  rounding is applied; whether the rule is uniform platform-wide or may
  vary), each with plain-language options, financial consequences, and a
  numerical worked example (a 50% deposit-match landing exactly on a
  half-cent tie; a repeating 7.3% weekly cashback showing the
  truncate-and-carry mechanism concretely). No option is selected or
  recommended.
- **A precise bonus-type impact table**: the rounding decision affects
  the grant amount itself for Deposit bonus, Reload bonus, and Cashback;
  for the generic Wagering bonus and Coupon it affects only the derived
  wagering-requirement/contribution-tracking computation, not the flat
  grant/face amount those two types actually pay out — a materially more
  precise finding than a blanket "all five types are affected
  identically."
- **A six-step `bonus_conversion` engineering checklist**, formatted for
  the decision sheet but explicitly labeled informational, not a
  decision for the human — `risk` re-verified the dependency is still
  NOT STARTED (zero of six ADR 0031 §16 steps) against current
  repository state, with no code changed since Stage 4H-B0-R1.
- **Confirmation that no other blocker exists**: `architect` performed a
  focused 12-area review (ledger accounting, wallet, idempotency,
  concurrency, Risk, RG, audit, RLS, reconciliation, transaction/account
  types, multi-asset precision, bonus conversion) and found **no
  additional P0/P1 blocker** beyond the two already-known gates.

**Specialist findings of note:**

- `ledger-finance` confirmed no rounding option can break
  `SUM(DEBITS)==SUM(CREDITS)` — options A-E have no separable residue to
  drop at the bonus-grant/cashback posting site (the mirrored legs are
  always posted with the same already-rounded integer); option F
  introduces genuine new financial state (a remainder accumulator)
  needing the same rigor as the ledger itself, plus an unresolved
  reversal/forfeiture policy for an unreleased remainder. Flagged a
  concrete implementation trap: PostgreSQL's default numeric-to-integer
  cast silently implements round-half-up, so whichever option is chosen
  must be an explicit function in the platform's one shared rounding
  helper, never an implicit cast.
- `bonus-engine` independently confirmed (not merely trusted) that all
  five in-slice types reach `completed → converted` and therefore all
  five require the `bonus_conversion` Risk dependency, even though only
  three of the five have their *payout amount* affected by the rounding
  decision.
- `security` found no additional blocker; flagged that whichever
  rounding rule is eventually built must ship with an explicit manage
  permission and mandatory audit logging on any rule change, and found
  one minor non-blocking audit-table completeness gap (`reversed` bonus
  transitions have no explicit row in doc 10 §10's audit table, though
  the Progress-trail requirement already covers it).
- `qa` confirmed the core CLAUDE.md financial test matrix is already
  well-mapped to bonus scenarios, designed the property-based test
  approach needed to prove whichever rounding rule is chosen is
  deterministic and exactly recomputable, and flagged several
  non-blocking test-plan additions for engineering's backlog.

**No production code, no migrations, no implementation was authorized or
started this stage.** `go build ./...` remains clean (docs-only diff).

**Stage 4H-B1 remains BLOCKED and is NOT authorized.** This stage
produced the decision package; it did not make the rounding decision or
approve the next stage. The Master Orchestrator did not select an answer
to DS-1, DS-2, or DS-3, and no specialist was authorized to do so on the
user's behalf.

### Decisions/input needed before the next stage

The human reviews `docs/architecture/28-bonus-financial-gate-decision-
sheet.md` and answers DS-1, DS-2, and DS-3. Once answered and recorded in
ADR 0021, and once the `bonus_conversion` six-step checklist is completed
as part of Stage 4H-B1's own work, Stage 4H-B1 can be authorized.

---

## Stage 4H-B0-R1 — B0 Gate Corrections and Finalization — Complete

Status: **Complete, pending explicit human approval before Stage 4H-B1
(Bonus Engine) or Stage 4H-B2 (Retail Architecture Hardening).**

A correction/finalization stage responding to specific errors identified
in the Stage 4H-B0 completion report — **no architecture redesign, no
production code, no migrations, no implementation.** Governance record:
`docs/governance/task-registry.md`'s Stage 4H-B0-R1 rows;
`docs/governance/project-status.md`'s Stage 4H-B0-R1 section.

**Corrections made:**

1. **Bonus gate contradiction resolved.** Stage 4H-B0's report claimed
   Stage 4H-B1 was "independently authorizable" while its own text
   disclosed an unresolved ADR 0021 rounding dependency blocking 3 of the
   5 first-slice bonus types. **Corrected: Stage 4H-B1 is CONDITIONALLY
   READY, not independently ready**, blocked until (1) ADR 0021's
   rounding/precision decision is resolved by a human, (2) the
   `bonus_conversion` Risk dependency is completed and reviewed, (3) no
   other P0/P1 financial dependency remains. `risk` verified directly
   against repository state that `bonus_conversion` is **NOT STARTED —
   zero of ADR 0031 §16's six extension-process steps complete**,
   correcting an earlier draft's understatement ("one small dependency
   request"). `ledger-finance` enumerated ADR 0021's rounding decision as
   three separable questions (direction — 6 neutrally-presented options;
   rounding point/precision handling; uniformity/scope) without selecting
   one — see ADR 0021's new "Rounding and precision" section and doc 27
   §1.1/§24 #15.
2. **Retail P0 list reclassified.** The "8 P0 decisions block retail"
   framing conflated genuine human/business/legal decisions with
   engineering acceptance criteria. Split into doc 27 §23A (3 genuine
   human decisions: retail licensing/jurisdiction status; confirmation of
   the node-owned `agent_float` extension to ADR 0007; anonymous/bearer
   play policy) and §23B (mandatory engineering acceptance criteria that
   require no human input: fail-closed hierarchy RLS with no OR-NULL
   escape, no retail role holding `PermStaffManage`, RG-before-Risk
   ordering, closure-table write protection, server-side terminal
   credential resolution, POS idempotency namespace protection, offline
   fail-closed baseline, and others).
3. **Agent float vs. Player Wallet formalized.** ADR 0007's `Wallet`
   stays untouched and strictly player-owned; agent float is explicitly
   NOT a Player Wallet — a hierarchy-node-owned operational account in
   the SAME authoritative ledger, never a second ledger, never confused
   with a physical till (physical cash remains fulfillment/custody, not
   an authoritative balance). `ledger-finance` drafted the minimum
   additive schema change in ADR 0035 §1.3.1 (`ledger_accounts` gaining a
   nullable `hierarchy_node_id` + a `num_nonnulls(...) <= 1`
   mutual-exclusion CHECK + an owner-family CHECK), explicitly `NOT
   IMPLEMENTED`. Reading the actual migration
   (`migrations/0020_create_ledger_accounts.up.sql`) rather than trusting
   the prior draft, `ledger-finance` found and corrected a load-bearing
   error: the original claim that the amendment "changes no existing
   constraint" was **false** — the existing house-level unique index
   predicate (`wallet_id IS NULL`) would have silently collapsed every
   hierarchy node's `agent_float`/asset into one shared row per tenant,
   recreating exactly the P0 failure the amendment exists to prevent. The
   corrected proposal widens that predicate to `wallet_id IS NULL AND
   hierarchy_node_id IS NULL`. `architect` and `security` reviewed the
   corrected proposal this stage (see their added ADR 0035 subsections);
   the amendment remains `NOT IMPLEMENTED` and requires human approval
   before any migration is written, since it changes the practical shape
   of a table whose broader design traces to human-approved ADR 0007/0019.
4. **First Retail Product Baseline formalized** (doc 27 §1.3a):
   identified players only, no anonymous/bearer play, online connection
   required, no offline/store-and-forward, single retail currency
   initially, cash deposit/withdrawal, a fixed shallow hierarchy for the
   first contracted operator, no automated commissions, no
   agent-to-agent float transfer, no direct bank agent settlement, no
   proxy/assisted play — implementation-scope constraints for the first
   slice, not claims about what every jurisdiction permits.
5. **Generic hierarchy model reaffirmed**: Node Type/Structure/Capability
   remain separate concepts; Operator/Partner/SuperAgent/Agent remain
   seed/configuration data, never hardcoded schema roles; Player is not a
   hierarchy node; Cashier is a staff identity assigned to a node;
   Terminal is a service principal.
6. **Shared platform model restated** (doc 27 §9): Online and Retail are
   operating channels of one platform sharing Person, PlayerAccount,
   Identity Resolution, Wallet, Ledger, Risk, RG, KYC, Payments, Bonus,
   Audit, Reporting, Reconciliation — retail terminals/POS are API
   clients of the platform, never a second retail financial system.
7. **Reporting requirement preserved** (doc 27 §8): Back Office and
   future retail/agent frontends use the same reporting facts and API
   surface; visibility is controlled by authorized tenant + hierarchy
   subtree, never hard-coded by hierarchy level.
8. **Retail financial movement model preserved** (doc 27 §6): a retail
   cash deposit is a transfer of existing liability (`Dr agent_float(node)
   / Cr player_cash(wallet)`), never a PSP deposit; retail withdrawal
   goes through the existing withdrawal hold/state-machine architecture
   with a cash-at-cashier fulfillment channel; every retail financial
   operation retains double-entry, idempotency, authorization,
   RG-then-Risk ordering, audit, reconciliation, and concurrency
   protection without exception.
9. **Commissions kept as future architecture** (doc 27 §9a): no automated
   commission calculation, accrual, payout, or cascade mechanics in the
   first retail slice; commercial terms (rate, base, hierarchy cascade,
   overrides, settlement frequency, tax treatment) must be defined first.
10. **Stage dependency graph corrected into two independent, parallel
    paths** (doc 27 §22): Path A (Bonus financial gate → Stage 4H-B1) and
    Path B (Retail-Legal/Business gate → Stage 4H-B2 Retail Architecture
    Hardening → Stage 4H-B3 Retail First Implementation). Gamification
    and the Reward Orchestrator remain independently deferred with no
    scheduled next stage.
11. **Human Decision Register rewritten** (doc 27 §24): a clean 15-item
    list containing only genuine human/business/legal decisions —
    retail licensing/jurisdiction structure; whether hierarchy agents are
    independent legal entities or platform-operated; confirmation of the
    node-owned `agent_float` extension to ADR 0007; anonymous/bearer
    retail play policy; offline retail policy; proxy/assisted play
    policy; commission commercial terms; agent credit/post-pay policy;
    franchised vs. company-owned retail model; cash AML thresholds by
    jurisdiction; KYC evidence requirements for retail presence; terminal
    ownership/fleet model; whether hierarchy actors may author subordinate
    risk limits; confirmation of the first contracted retail
    market/operator; and the ADR 0021 rounding/precision decision. An
    earlier draft's "confirm which stage to authorize next" item was
    removed — it is not a business/legal decision, it is the ordinary
    end-of-stage authorization every stage ends with.

**No production code, no migrations, no implementation was authorized or
started this stage.** Full detail: this stage's completion report and
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`.

### Decisions/input needed before the next stage

See doc 27 §24's 15-item Human Decision Register. The two independently
authorizable next steps are: (1) resolve the Bonus financial gate (ADR
0021 rounding decision + `bonus_conversion` Risk dependency) then
authorize Stage 4H-B1; (2) resolve the 3 Retail-Legal/Business decisions
(§23A) then authorize Stage 4H-B2 (Retail Architecture Hardening).
Neither is authorized by this stage.

---

## Stage 4H-B0 — Bonus, Gamification & Retail Scope/Implementation Plan — Complete (corrected by Stage 4H-B0-R1 above)

Status: **Complete, pending explicit human approval before Stage 4H-B1
(or any retail implementation stage).**

Architecture/scope-freeze stage responding to a new confirmed business
requirement: retail iGaming operations (a configurable agent-hierarchy
network — Operator → Partner → Super Agent → Agent → Player/Cashier,
never hardcoded) as another surface of the same platform. **No
production code, no migrations, and no implementation were started.**
Full detail: `docs/architecture/27-stage-4h-b0-scope-and-implementation-
plan.md` (the master synthesis, covering the directive's 25 numbered
deliverables) and `docs/governance/project-status.md`'s Stage 4H-B0
section. Governance record: `docs/governance/task-registry.md`'s
Stage 4H-B0 rows (4HB0-01 through 4HB0-14).

**Ten Wave-1 specialist documents**, each specialist owning a distinct
file: architect (`26-retail-operations-architecture.md`, the core
hierarchy/retail architecture — adjacency list + closure-table
projection, no hardcoded level ladder), ledger-finance (ADR 0035, agent
float as a platform liability, three new account types, Invariants
R1-R3), security (ADR 0036, three-axis authorization, closure-table RLS
fail-closed by construction, RG/KYC non-bypass made structural),
identity-compliance (docs 05/11 addenda), payments (doc 07's "Retail
cash rail" section), risk (ADR 0031 §19-24), data-analytics (doc 12
addition), backend (doc 04 addition), qa (testing-strategy.md addition),
bonus-engine (doc 10's MVP implementation-scope plan — 5-type first
slice, Stage 4G §32 gate qualified-lifted).

**Wave-2 review found and fixed 1 P0 + 8 P1 genuine cross-document
contradictions** (code-reviewer, mirroring Stage 4H-A's own Wave-2
pattern at larger scale — 14 findings F1-F14 across 10 documents,
~8,000+ lines). Most severe: a transaction-phasing contradiction in ADR
0036 that would have silently broken every retail ledger posting or made
every counter operation fail closed permanently (F1); a fail-open
ancestor-suspension check defeated by the very RLS policy meant to
protect it (F8); a payments handler that never called RG at all and
inverted the fixed RG-then-Risk gate order (F9, safety-critical). All
fixed in-place with explicit "Wave-2 review correction" callouts. Full
list: doc 27 §22a.

**Wave-2 scope review (product-owner-proxy)** independently confirmed
retail has zero Blueprint content (like Gamification in Stage 4H-A —
human-directed business scope, correctly never presented as a Blueprint
requirement by any specialist), gave a concrete recommended MVP-vs-
deferred split, and found one scope-creep item (ADR 0035's commission
machinery downgraded from binding to documented-for-future-reference
pending unresolved commercial terms).

**Corrected in Stage 4H-B0-R1 (see that stage's section above)**: the
original "8 P0 decisions block retail" framing conflated genuine human
decisions with engineering acceptance criteria, and the original bonus
gate-check ("independently authorizable") contradicted this same
document's own disclosure of the ADR 0021 rounding dependency. Both are
corrected above — this section is retained for the historical record of
what Stage 4H-B0 itself concluded before that correction.

This stage's directive had no contradictory trailing line (unlike Stage
4H-A's) — it stated plainly "This stage must NOT automatically proceed
to implementation. Wait for explicit approval before Stage 4H-B1," and
this stage complies with that exactly: no implementation stage
(4H-B1/"Retail-Legal"/4H-B2/4H-B3) has been started.

---

## Stage 4H-A — Bonus, Gamification & Reward Orchestration Architecture Freeze — Complete

Status: **Complete, pending explicit human approval to authorize the next
stage (Stage 4H: Bonus Engine implementation).**

Explicitly an architecture + accounting + domain-contract freeze only,
per the stage's own directive: **no Bonus Engine, Gamification Engine,
Reward Orchestrator, sportsbook-bonus, CRM, notification-provider, or
external-reward-provider code was written.** Every deliverable this
stage is a design document, ADR, or governance-table update — zero
production code, zero migrations, zero tests, `go build ./...` untouched
and clean.

### What was frozen

- **Three distinct core domains** (never merged): Bonus Engine
  (`docs/architecture/10-bonus-engine-architecture.md`, rewritten),
  Gamification Engine (`17-gamification-engine-architecture.md`, new,
  plus `18-tournament-architecture.md`, `19-mission-architecture.md`,
  `20-reward-marketplace-architecture.md`), Reward Orchestrator
  (`21-reward-orchestration-architecture.md`, new — fulfillment
  mechanism only, never decides whether a reward is earned).
- **`ExternalRewardProvider` abstraction**
  (`23-external-reward-provider-contract.md`) for coexistence with a
  known future sportsbook provider's own native bonus engine —
  provider-neutral, not built.
- **Canonical Activity/Event taxonomy**
  (`22-canonical-activity-event-taxonomy.md`) extending the Stage-1
  `internal/eventbus.Event` stub, with the load-bearing `event_id` vs
  `idempotency_key` distinction made explicit after being found
  conflated in 3 draft documents.
- **Bonus accounting** (`docs/decisions/0032-bonus-accounting.md`,
  CRITICAL/authoritative, ledger-finance-owned): `promo_liability`,
  `bonus_expense`, Invariant B1, atomic 4-entry cash conversion, and the
  three funding-scenario ledger treatments (operator-funded,
  provider-funded, externally-fulfilled = zero ledger entries ever).
- **Points accounting** (`24-points-accounting-architecture.md`) with the
  `PointType` dual-scope-definition / tenant-scoped-balance correction.
- **Risk integration** (`docs/decisions/0031-risk-and-limits-engine.md`
  §14-§18): Bonus/Gamification consume `internal/risk.Evaluate`
  exclusively, no new limit engine.
- **RG integration** (`docs/decisions/0034-...rg-kyc-identity-
  integration.md`): RG remains sole authority; self-exclusion is
  prospective not retroactive; conversion-time denial leaves a Grant
  `completed`, never auto-forfeited.
- **Provider-neutral sportsbook interoperability**
  (`docs/decisions/0033-provider-interoperability-and-external-bonus-
  engines.md`): two future providers mapped onto one canonical contract,
  neither built.
- **API/RBAC contract** (`25-bonus-gamification-api-architecture.md`,
  design only): `bonus_config:read/manage`, `tournament:settle` split
  from `tournament_config:manage`, a recommended dedicated
  `RolePromotionsManager` role.

### Specialist review

Wave 1: bonus-engine, architect (Gamification/Tournament/Mission/
Marketplace), ledger-finance (ADR 0032 + doc 24), sportsbook (ADR 0033),
identity-compliance (ADR 0034), risk (ADR 0031 §14-§18), backend (doc
25) — 7 parallel drafts, plus 3 cross-domain connective documents
authored directly by the Orchestrator (doc 21, 22, 23).

Wave 2 (code-reviewer, security, qa, casino — parallel review of the
frozen set): found ~20 genuine P1-severity cross-document
contradictions (not stylistic — real conflicting decisions about the
same entity/mechanism). **All P1s fixed in-place**, each with an
explicit "specialist-review correction" callout: externally-fulfilled
bonus ledger treatment (3 conflicting documents), `event_id`-vs-
`idempotency_key` idempotency keying (3 documents), a
synchronous-vs-asynchronous marketplace-redemption transaction-boundary
conflict (docs 20 vs 24), a missing Reward Orchestrator reversal path,
the External Reward Provider callback contract missing 7 security rules
present in the casino-callback precedent it claimed to mirror
(including an integrity-alert rule for a callback not matching a
platform-created handle), tournament-settlement permission bundled with
prize-authoring permission, no permission proposed for bonus-campaign
authoring, anti-manipulation controls never wired into tournament
settlement as a required step, a contradictory instruction on reading
`internal/identityresolution` directly, and others (full list in this
stage's completion report delivered to the user). Lower-priority P2/P3
items (tournament entry/withdrawal re-entry cycling, achievement-unlock
reversal/void handling, demo-event exclusion enforced only as stated
policy rather than structurally) were recorded as open follow-up items
rather than fixed, per the stage's own architecture-freeze scope.

### Addendum: ledger-finance financial sign-off + product-owner-proxy scope review

Two further Wave-2 specialists completed after the first review round
above: `ledger-finance` (independent financial sign-off, required by
CLAUDE.md before any monetary architecture counts as reviewed) and
`product-owner-proxy` (scope discipline).

**ledger-finance: PASS WITH FINDINGS, sign-off granted once 7 P1s were
applied** — all 7 fixed in-place across ADR 0032, `financial-transaction-
flows.md` (Flows 5/6/7/9/11/20 gained the bonus-funded mirror legs an
implementer following the frozen documents literally would have missed,
breaking invariant B1 on the first bonus-funded bet; a new Flow 21 added
for externally-fulfilled = no posting), and docs 10/20/21/23 (a binding
lifecycle-event-to-posting map, a direct cash-reward treatment, an
explicit `manual_adjustment` account/mirror-leg rule, a tombstone on
Reward Orchestrator reversals of never-fulfilled decisions, a
fulfilment-destination declaration on the External Reward Provider
contract, and a corrected cross-tenant points-isolation rationale). Full
detail: `docs/governance/project-status.md`'s Stage 4H-A addendum.

**product-owner-proxy: no correctness findings, but a real scope-anchor
gap** — read the full Blueprint (20 pages) and confirmed it never
mentions gamification/points/XP/levels/achievements/badges/missions/
tournaments/leaderboards/streaks/marketplace/raffles/mini-games; the
entire Gamification Engine/Reward Marketplace/Reward Orchestrator domain
(5 architecture documents this stage) had no anchor in the Blueprint or
this project's prior MVP roadmap. Recorded as deferred scope in
`14-mvp-scope-and-roadmap.md`, with the Reward Orchestrator flagged as
premature abstraction and doc 18 (Tournaments) flagged as the most
disproportionately-designed sub-capability, both for if/when this domain
is ever authorized.

### Directive contradiction — flagged, not acted on

The stage's directive was a detailed, internally consistent 27-section
body explicit about being architecture-freeze-only ("YOU ARE NOT
AUTHORIZED TO IMPLEMENT THE BONUS ENGINE YET"), followed by a single
trailing line appended after the full body: "Approved — proceed with
Stage 4H: Bonus Engine." Per CLAUDE.md's stage-gate rule ("never begin
the next stage's implementation unprompted, even if it seems obviously
next"), the body was treated as authoritative and the trailing line was
not acted on. **Stage 4H (Bonus Engine implementation) is NOT
authorized.** This is re-flagged in the stage's completion report,
which explicitly asks for human confirmation before any Bonus Engine,
Gamification Engine, Reward Orchestrator, or sportsbook-bonus code is
written.

### Decisions/input needed before the next stage

1. Confirm which stage to authorize next: Stage 4H (Bonus Engine
   implementation) as the directive's trailing line suggested, or a
   different next stage — and confirm the architecture-freeze-only
   reading of this stage's directive was correct.
2. The lower-priority P2/P3 items recorded above (not fixed this stage)
   should be revisited once Bonus Engine/Gamification implementation is
   authorized.
3. All previously-open decisions from Stages 0-4G-FINAL remain open —
   see `docs/governance/project-status.md`'s consolidated list.

---

## Stage 4G-FINAL (+ FINANCE-GATE follow-up) — Architectural Hardening & Final Gate — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Explicitly not a business-functionality stage - the directive's objective
was to harden Stage 4G (project orchestration governance + the Risk &
Limits engine) so the platform core is genuinely extensible, governed,
and safe to build future domains on. No new domain, no new business
capability. A follow-up "Stage 4G-FINAL-FINANCE-GATE" closed the one gap
left open when this stage originally committed: the independent
Financial/Ledger specialist review (see the updated specialist-review
section below) - itself also final-gate-only, no business functionality.

### Part A — Governance made operational

- `docs/governance/agent-registry.md` - new "Absolute constraint on every
  specialist" (no silent cross-domain edits, no self-assigned scope, no
  self-reviewed work) and "How the Orchestrator assigns every task to an
  owner."
- `docs/governance/task-registry.md` - two new permanent, append-only,
  cross-stage tables: the **Dependency Request Log** and the
  **Integration Approval Log** - the concrete mechanism for "how
  dependency requests/integration approvals are recorded," not just
  prose. The Stage 4G task table is preserved unmodified alongside the
  new Stage 4G-FINAL one, per the registry's own "never delete history"
  rule.
- `docs/governance/ownership.md`/`integration-protocol.md`/
  `change-control.md` updated to reference the new logs and the new test-
  reporting standard.

### Part B — Risk Engine contract finalized

`docs/decisions/0031-risk-and-limits-engine.md` gained §9-§13:
jurisdiction-context contract, licensing-mode contract, explicit `REVIEW`
semantics (a distinct outcome from `DENY` in the domain model - only
today's enforcement points collapse them, as an enforcement-point choice),
the three-step extension model for any future `LimitKind`, and a table of
every future domain's Risk-integration obligation. `risk.Evaluate`'s
signature and every Stage 4G decision are otherwise unchanged.

### Part C — Jurisdiction context gap structurally closed (PARTIALLY IMPLEMENTED)

Stage 4G disclosed: jurisdiction-scoped rules were reachable only from
`LaunchGame`, never `postBet`. Migration `0042_jurisdiction_and_licensing_
context` adds `casino_launch_sessions.jurisdiction_code`, populated once
at launch and read back by every subsequent bet in that round. No
geolocation vendor invented. **Not claimed as fully `IMPLEMENTED`**: no
HTTP handler populates `LaunchGameParams.JurisdictionCode` yet (the
pre-existing `TODO(jurisdiction)` root cause - no per-player jurisdiction
resolver exists anywhere in this codebase), so a real production launch
persists no jurisdiction today; the wiring is proven correct only by
tests that populate it directly. Regression tests:
`TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession`,
`TestReceiveCallback_JurisdictionScopedRiskRuleDoesNotDenyADifferentJurisdiction`,
`TestReceiveCallback_SessionWithNoJurisdictionDoesNotMatchJurisdictionScopedRule`.

### Part D — Licensing-mode scoping added

New `LicensingMode` scope dimension on `Rule`/`RiskRequest`, mirroring
`tenants.licensing_model`'s existing two values (ADR 0006 - not a new
taxonomy). Resolved server-side by the caller (new `internal/identity.
GetTenantByID` + `internal/casino`'s `resolveLicensingMode`), never by
`risk.Evaluate` itself. Lets a platform-wide legal-ceiling `HARD_LIMIT`
avoid binding a future bring-your-own-licence tenant. No BYOL tenant
onboarded. Regression test:
`TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode`.

### Part F — Flake root-caused and fixed (two distinct bugs)

`TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion` (intermittent
since Stage 4D-RG/4E) was root-caused to a genuine mechanism, not
re-labeled: `postBet`'s idempotency short-circuit only reliably
serializes SEQUENTIAL redeliveries; two truly-concurrent deliveries of
the same bet could each start before the other committed and
independently re-evaluate live RG state, producing divergent outcomes for
the identical bet even though the ledger never posted more than once.
**Fixed**: a `pg_advisory_xact_lock` scoped to
`(tenant_id, provider_id, provider_tx_id)`, acquired before the
idempotency check.

Widening the shipped regression test to N=8 concurrent deliveries
(`internal/casino/adversarial_lock_stress_test.go`, QA-authored, not
requested by the directive) then surfaced a SECOND, deeper,
previously-undiscovered bug in `internal/rg.EvaluateEligibility`: it used
Postgres `now()` (frozen at transaction start) instead of
`clock_timestamp()` (re-evaluated per call), so a transaction queued
behind `rg.lockPerson`'s advisory lock could miss a self-exclusion that
had already committed. **Fixed** by switching to `clock_timestamp()` in
`internal/rg/rg.go`, with a new deterministic regression test
(`TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan`).

Both fixes verified via 30+ repeat full-iteration runs plus 9 additional
post-fix full-repo and targeted `-race -tags=integration` runs, all clean
(previously flaked within a single 15-iteration run, and the `internal/rg`
bug alone reproduced in ~50% of full-repo `-race -tags=integration` runs
before its fix).

### Parts E, G, H, I — documentation only

REVIEW semantics (E), the test-reporting standard (G,
`docs/testing/testing-strategy.md`), the LimitKind extension model (H,
ADR 0031 §12), and cross-domain boundary verification (I,
`docs/architecture/02-domain-and-service-boundaries.md`) - all
documentation, no code change beyond what Parts C/D/F already required.

### Specialist review: 11 of 11 areas now complete

Architecture, Risk, Casino, Responsible Gaming, Security/RBAC (x2),
PostgreSQL/RLS, API/HTTP, Adversarial Testing, Multi-tenancy,
Documentation/Governance all completed during the original Stage 4G-FINAL
run. **Financial/Ledger** did not complete in that run (its review agent
stalled and was stopped without findings; the Orchestrator's own
self-review at the time was explicitly recorded as not a substitute) but
was completed via the Stage 4G-FINAL-FINANCE-GATE follow-up: an
independent `ledger-finance` review of the `postBet` advisory lock and the
`internal/rg` `clock_timestamp()` fix, answering all 20 required questions
plus a 14-scenario adversarial-coverage matrix. **Verdict: PASS,
independent sign-off GRANTED.** No P0/P1 found; 6 P2s and 3 P3s recorded
as follow-up hardening/observability items (none blocking), none fixed
this stage per the finance-gate directive's own "no scope expansion"
instruction. Full findings, the 20 answers, and the coverage matrix are in
`docs/progress.md`'s Stage 4G-FINAL-FINANCE-GATE entry;
`task-registry.md`'s `IA-4GF-01`/`IA-4GF-03` rows carry the sign-off.

### Verification performed

See the Stage 4G-FINAL completion report's test matrix
(PASS/FAIL/FLAKE/NOT RUN/BLOCKED per suite, per the new reporting
standard).

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4G-FINAL and authorize the next stage. Explicitly not
   authorized by this stage: Bonus Engine, a real KYC provider, a real
   casino provider, sportsbook.
2. The open decisions carried forward from Stage 4G (REVIEW-provisional-
   proceed question, HARD_LIMIT-vs-CONFIGURABLE_LIMIT precedence in a
   real jurisdiction, no platform-wide-rule HTTP write path) remain open
   - see `docs/governance/project-status.md`'s consolidated list.
3. All other already-open, non-blocking items from Stages 0-4G remain
   open (see `docs/governance/project-status.md`).

## Prior stage (historical, superseded): Stage 4H-B1 Wave 1.5 Fix Round 2 — CONCLUDED

**Status: READY** for Wave 2 authorization, subject to two routed,
non-blocking P1s. Full detail:
`docs/governance/wave-1.5-fix-round-2-report.md` and
`docs/governance/task-registry.md`'s corresponding section.
`docs/progress.md`'s "Stage 4H-B1" entry has the running narrative.

All work remains design/documentation only (no code, no migration
beyond `0049`, no UI). 24 specialist dispatches across three phases
(authorship, independent re-verification, closing pass) closed all
four original Wave-1.5 P0s and all four Wave-1.5-Phase-2-discovered new
P0s, each independently certified by a reviewer who did not author the
fix. The composition failure between bonus-engine's and casino's
designs that blocked the prior round was root-caused, fixed, and
re-certified. This stage also delivered the Product Surfaces roadmap
gate (`docs/architecture/35-37`): Stage 6A (Back Office MVP), 6B
(Partner Console MVP), 6C (B2C Brand Frontend MVP), 6D (Retail/POS),
none authorized for implementation yet.

### Decisions/input needed from the human before any further B1 work

1. **Authorize (or not) Stage 4H-B1 Wave 2** — the Bonus Engine's
   design is now READY per independent re-verification. Two
   non-blocking P1s remain routed (LF-10; the `SEP-1` `agentnetwork`
   tenant-edge confirmation) and should be closed early in whatever
   round follows, but do not block starting Wave 2's own scope.
2. **Authorize (or not) Stage 6A** (Back Office MVP) — `architect`'s
   reasoned recommendation is to sequence it after Bonus Engine Wave 2
   to avoid rework on ledger/wallet-shape read views still being
   reshaped, but nothing technically blocks starting it in parallel if
   preferred.
3. CRM implementation, Affiliate implementation, Gamification
   implementation, bonus-funded-wagering implementation, and Partner
   Console/B2C frontend implementation remain **NOT authorized**.
4. The four Human Decision Register items (G-2, `OpenBetSelfExclusionPolicy`
   default, mixed/bonus-funded cashout policy, FD-1) remain unmade —
   none of this round's work required or selected one.

*Status note 2026-09-26: all four items were answered by the human in `docs/decisions/0042-human-decision-response.md` Part 2 — G-2: configurable per brand, default (b) route to `player_cash`; `OpenBetSelfExclusionPolicy` default: `VOID_ON_SELF_EXCLUSION`; mixed/bonus-funded cashout: "Not cashout-eligible"; FD-1: "Nullifying" (inactive while bonus-funded bets are not cashout-eligible). Implementation of G-2 brand configurability and of the self-exclusion default/auto-void consumer remains outstanding (ADR 0042 G-2 notes; `docs/plans/stage-10-planning-gate-proposal.md` §5).*

## Current stage: Stage 10.3 — AUTHORIZED 2026-09-26 (ADR 0092) — W0 in progress

Real Provider Trust & Casino Financial Readiness. The human authorized the full scope against
baseline `207c922`. The authorization is recorded in
`docs/plans/stage-10.3-planning-gate-proposal.md` §22 and supersedes conflicting text in that
proposal.

- **Stage definition:** `docs/decisions/0092-stage-10-3-definition-real-provider-trust-and-casino-financial-readiness.md`
  (scope, waves W0 → W1a–d → W2a/b → W3a/b, gates 10.3-W0..W3, status labels).
- **Credential model:** `docs/decisions/0093-provider-credential-model-and-secret-store.md`,
  binding for W2a and W3b.
- **Human rulings:**
  - HD-10.3-1: full scope approved.
  - HD-10.3-2: AWS IAM code **excluded**; W3b is backend code plus a local fake only.
  - HD-10.3-3: KYC players see **status only**.
  - HD-10.3-4: suspended-tenant casino settlement **unchanged**. The existing behaviour is
    documented in the ADR 0025 Stage 10.3 amendment.
- **Deferred:** PAYWH-TS-1, PAYWH-BRAND-1 and PAYWH-RL-1 remain deferred. Timestamp rules for real
  schemes land in W1a.
- **W0 (docs only), in progress:**
  - ADR 0092 and ADR 0093;
  - "Amendment (Stage 10.3, ADR 0092)" sections in ADR 0022 §3, ADR 0085 §1, ADR 0025,
    ADR 0082 (A6) and ADR 0028;
  - a pointer in `docs/architecture/08-casino-integration-architecture.md` §9a;
  - the task registry "Stage 10.3" section.
- **Nothing is implemented yet.** Every Stage 10.3 deliverable is `NOT IMPLEMENTED`, except
  KYC-REASON-BOUND-1, which is `PARTIALLY IMPLEMENTED`: the model exists, the bound does not.
- **Specialist inputs:**
  - the planning papers and reviews are in `docs/plans/stage-10.3-planning/`;
  - rulings R1–R15 are in the proposal's §19;
  - the binding per-wave test plan is in `04-review-qa.md` §4;
  - red-before-green evidence goes in `docs/plans/stage-10.3-planning/evidence/`.
- **Stop point:** the Stage 10.3 completion gate (after W3). The next stage needs explicit human
  authorization.
- Roadmap reconciliation: `docs/plans/stage-10.3-planning/00-roadmap-reconciliation.md`.
- **AWS staging: OFF.** Torn down 2026-09-26 by the governed `deploy.sh down`
  (`docs/governance/staging-teardown-2026-09-26.md`). Development continues locally; items
  needing AWS are marked STAGING REQUIRED and deferred to the single future governed staging
  deployment from the final approved commit.

## Prior stage: Stage 10.2 — Webhook trust hardening — COMPLETE (IMPLEMENTED for MOCK providers)

Completion report: `docs/governance/stage-10.2-completion-report.md` (final commit `957a3e8`).
Its staging deployment plan (§16) was superseded by the human's 2026-09-26 decision to tear
staging down and perform one governed deployment later from the final approved commit.

## Prior stage: Stage 10.1 — PAY-REV-1 + SB-T1-XMIN + PAY-WH-TENANT-1 — IMPLEMENTED; STOPPED at the staging-deployment gate

Approved by the human against `8561ac2` (ADR 0090 ACCEPTED) with PAY-WH-TENANT-1 added. All three workstreams implemented and reviewed (completion report: `docs/governance/stage-10.1-completion-report.md`). Staging deployment requires separate human authorization. Open human rulings: KYC-WH-1 scope.

*Status note 2026-09-26: the KYC-WH-1 scope ruling was resolved by `docs/decisions/0091-stage-10-2-definition-webhook-trust-hardening.md` (ACCEPTED) and implemented in Stage 10.2 — see "Current stage" above and `docs/governance/stage-10.2-completion-report.md`.*

### Planning gate record (below)

Authorized 2026-09-25 as a planning gate only (no implementation).
Report: `docs/plans/stage-10.1-planning-gate-proposal.md`; stage definition
ADR 0090 (PROPOSED); specialist record `docs/plans/stage-10.1-planning/`.
Future AI-agent boundary recorded as ADR 0089 (binding future requirement,
NOT IMPLEMENTED). New pre-existing finding PAY-WH-TENANT-1 (payments
webhook tenant not bound to the verifying credential) registered outside
scope pending a human ruling. No code, no implementation migration, no AWS
action. Stop point: gate G0 (human approval).

## Prior stage: Stage 10 — CI Evidence Restoration + Sportsbook Settlement Lifecycle — COMPLETE (awaiting authorization for the next stage)

Approved 2026-09-25 (ADR 0087). All seven gates passed; completion report:
`docs/governance/stage-10-completion-report.md`. **W0** restored the CI Go
gate (five consecutive green runs #239–#243). **W1** implemented
cash-funded single-bet sportsbook settlement in **in-house MOCK mode**
(ADR 0088; driven only by the non-production test-support staff route —
not a real provider integration): migration 0091, settlement service,
ledger multi-posting pre-lock, risk netting, reconciliation stream (MOCK
statement), route/permission/read surfaces. **F-7** remediated as its own
item (`36616f1`). Reviews: security (no P0/P1), code-review (B-1..B-3
closed), ledger-finance (approved). W1 code head `312db16`, CI #259 green.

Open/blocked: `b22d5c4` live IAM re-validation (external credential);
**PAY-REV-1** (P1, pre-existing payments deposit-reversal race, outside
scope, needs its own authorized stage); OB-1 and all prior human decisions
remain OPEN. Staging untouched. **Next stage NOT started** — recommended:
Stage 10.1 PAY-REV-1 remediation + residual hardening (report §15).

## Prior stage: Stage 10 Planning Gate — proposal approved by the human (PLANNING ONLY)

**Status: NOT AUTHORIZED — NOT STARTED.** Stage 9.4's AWS staging
deployment was executed by the human and the staging MVP acceptance
**PASSED (human-attested)** on deployed commit
`9190d5d01da076141a1f70d7e5897a573d3b18f4`. The Orchestrator
reconstructed project state from the repository and produced a
decision-ready proposal, reviewed by seven specialists:
`docs/plans/stage-10-planning-gate-proposal.md`.

**Proposed Stage 10:** W0 CI evidence restoration (hard gate: ≥5
consecutive green CI runs) then W1 sportsbook settlement lifecycle
(cash-funded singles, in-house mock mode: settle, void, correction).

**Findings recorded this gate:** F-1 CI Go gate never executed (broken
action reference); F-2 scratch-database tests need `CREATEDB`; F-3 19
lint issues; F-4 stale records; F-5 `b22d5c4` unreviewed (static review
clean, live checks owed); F-6 acceptance human-attested only; F-7
`ledger.Post` replay does not compare amounts (caller exposure
unaudited). Details in the proposal §2.4.

### Decisions/input needed from the human before any further work

1. Approve Stage 10 as proposed (or choose an alternative, proposal §G).
2. Approve the W0 test-admin mechanism (`igaming_test_admin`, CI and
   dev init only).
3. Name or supply the verification credential for the `b22d5c4` live
   checks, or accept that item as BLOCKED.
4. Staging disposition during Stage 10: keep running or tear down.
5. Acknowledge OB-1 remains an open pre-production decision.

No code, migration, Terraform, IAM or AWS change was made. Staging was
not redeployed or destroyed.

## Prior stage: Stage 9.4 — Staging Infrastructure Hardening + Cost Optimization — COMPLETE; deployed and accepted (human-executed, see Stage 10 Planning Gate above)

**Purpose.** After the human configured a READ-ONLY AWS credential
(`arn:aws:iam::765578795051:user/claude-staging-readonly`, account
765578795051 confirmed in writing as the authorized staging account,
eu-central-1 confirmed as the staging region), the first real read-only
`terraform plan` of the Stage 9.3 package surfaced blocking and costly
defects (PostgreSQL 16.4 not offered in eu-central-1, secrets in local
state, broken RDS alarms, HTTP-only public ALB, NAT/24x7 cost). The human
approved a consolidated hardening + cost pass (ADR 0086). Repository-side
work only: **no AWS resource was created, modified or deleted**; `terraform
apply`/`destroy` were never run; no credential was created or requested.

**Status: IMPLEMENTED (repository) / NOT DEPLOYED.** Terraform, scripts,
IAM policy documents, tests and runbooks are complete and verified offline
and with read-only AWS calls (plan: staging 74 to add, bootstrap 9 to
add). Deployment is **BLOCKED** on human authorization of a deployment
credential and the one-time bootstrap.

**Reviews** (independent, none self-approved; findings fixed and
re-verified by the finding reviewer): architect, security, FinOps,
backend, qa, code-reviewer — final verdicts in `docs/progress.md`.

**Remaining human inputs before deployment**: authorize and create the
deployment principal with `deploy/aws/iam/staging-{bootstrap,deployer-network,deployer-infra,deployer-edge-iam-state}-policy.json`;
run the bootstrap; supply `staging_access_cidrs` (own public IPv4 /32);
optionally `alarm_email` / `budget_alert_email`.

**Next step requires explicit authorization**: Stage 9.4 AWS deployment
(bootstrap + first `deploy.sh up`) per
`docs/runbooks/stage-9-4-staging-lifecycle-runbook.md`. Stage 10 is NOT
authorized.

## Stage 9.4 Part 1 — APP_ENV Fail-Closed Validation + Stateless Activation Seam — COMPLETE

**Purpose.** Authorized by the human as "STAGE 9.4 — STAGING DEPLOYMENT
READINESS + AWS STAGING DEPLOYMENT," opening with "Stage 9.3 is
approved." Part 1 closes the two issues Stage 9.3 deferred as needing a
dedicated config/architecture decision rather than a unilateral fix (see
"Remaining production blockers" in the Stage 9.3 section below). Part 2
(AWS account safety verification) and Parts 3-11 (actual AWS
provisioning) required authorized AWS credentials; the human explicitly
declined to supply or authorize any (same "produce operator instructions
only" answer as Stage 9.3's equivalent gate) — see
`docs/runbooks/stage-9-4-aws-account-verification.md` for the operator
verification runbook this stage produced in place of a real deployment.
**No AWS provisioning was attempted or performed this stage.** Full design
and rationale for both Part 1 fixes: ADR 0085.

**Fix 1 — APP_ENV two-layer fail-closed gate: IMPLEMENTED.**
`internal/config.Load()` now validates `Environment` against the closed
set `{"development", "staging", "production"}` whenever `APP_ENV` is
explicitly set to any value — including an explicit empty string, a gap
found independently by both the security and architecture reviews of this
exact change after the initial implementation, and closed via
`resolveAppEnv()` reading `os.LookupEnv` directly rather than folding
"unset" and "present-but-empty" into the same default. A second,
independent opt-in (`TestSupportEndpointsEnabled`/
`TEST_SUPPORT_ENDPOINTS_ENABLED`, default `false`) must ALSO be true
before any of the three non-production simulation flags
(`CasinoPlaySimulationEnabled`, `PaymentsMockSettlementEnabled`,
`AccountActivationTestSupportEnabled`) register, computed in one place
(`Config.TestSupportRoutesEnabled()`) — confirmed by grep, during
security review, to be the only call site. `Environment == "production"
&& TestSupportEndpointsEnabled == true` is a hard `Load()` startup
failure, not a silent correction.

**Fix 2 — stateless, multi-replica-safe activation seam: IMPLEMENTED.**
`GET /v1/me/email-verification/dev-token` (Stage 9.3's per-process,
in-memory token store) is removed entirely.
`POST /v1/me/email-verification/request`/`/resend` now, when
`AccountActivationTestSupportEnabled` is true, return the raw token
directly in that same request's `200` response body, at the exact point
the handler already holds it in memory — no new shared infrastructure
(no Redis, no new table) was introduced, per the directive's own
instruction to prefer the smallest correct fix. Flag-off/production
response shape is byte-for-byte unchanged (`204`, no body). Multi-replica
correctness is proven directly by
`TestAccountActivationDevToken_MultiReplica_RequestOnReplicaA_ConfirmOnReplicaB`,
which constructs two genuinely independent `httptest.NewServer` instances
sharing only the same `*db.Pool`, issues the request on one and confirms
on the other, and asserts both independently observe the account reach
`active`.

**A related, genuine multi-replica financial-correctness bug was found
and fixed in the same pass.** While reviewing Fix 2, the `architect`
review independently found the same bug *shape* on the actual financial
simulation path: `internal/payments.MockProvider.nextReference()` and
`internal/casino.MockCasinoProvider.nextReference()` each minted
references from a bare per-process `seq int` counter, so two replicas
could mint the same `provider_reference`/`provider_tx_id` for their own
first transaction, colliding on `deposit_intents`' uniqueness constraint
and aliasing the ledger idempotency key. Both now append a
`uuid.NewString()` suffix (no new dependency — already used elsewhere in
both packages); the sequence number remains as a log-readability hint
only. No test hardcoded the old exact reference format (confirmed by
grep before changing it). See ADR 0085's "Related fix" section.

**Reviews.** `security` and `architect` both independently reviewed the
diff: **APPROVED WITH MINOR NOTES** from both. Security found and itself
fixed one doc-only inaccuracy (a rate-limited-vs-success response
distinguishability claim, now corrected in the OpenAPI spec) and reconfirmed
`internal/auth/credential_token.go` genuinely untouched. Both
independently flagged the same residual gap (`APP_ENV=""` slipping past
the first implementation's validation) — closed directly, with a new
regression test (`TestLoad_ExplicitEmptyAppEnvRejected`), rather than
left as a further deferred item. Architect additionally flagged: this
change needed a new ADR (now ADR 0085, written this stage);
`docs/decisions/0048-casino-play-simulation-trust-boundary.md`'s mitigation
list still described the superseded single-condition gate (amended in
place, with an inline "Update (Stage 9.4)" note, same pattern that
document already used for its own Stage 8 update); and
`deploy/aws/modules/ecs/variables.tf`'s new variable's doc comment
described a scenario (a future production invocation of *this* module
setting both flags) that this module's own existing `app_environment`
validation block already structurally prevents — trimmed to describe the
actual defense-in-depth reasoning instead.

**Validation.** `go build ./...`, `go vet`, `gofmt -l` clean on every
changed file. Focused suites re-run against real Postgres after each
fix and again after the mock-provider fix:
`internal/config/...`, `internal/payments/...`, `internal/casino/...`,
`internal/httpserver/...` — all pass, including the new/rewritten
`internal/config/config_test.go` cases (`TestLoad_UnsetAppEnvStillDefaultsToDevelopment`,
`TestLoad_InvalidAppEnvRejected` (8 subtests), `TestLoad_ExplicitEmptyAppEnvRejected`,
`TestLoad_ValidAppEnvValuesAccepted`, `TestLoad_TestSupportEndpointsEnabledOverride`,
`TestLoad_InvalidTestSupportEndpointsEnabled`,
`TestLoad_ProductionWithTestSupportEndpointsEnabledFailsClosed`,
`TestLoad_ProductionWithTestSupportEndpointsDefaultSucceeds`,
`TestLoad_TestSupportRoutesEnabled_BothConditionsIndependentlyRequired`)
and `internal/httpserver/email_verification_dev_token_test.go`'s full
6-test rewrite (including the multi-replica test above).

**What this stage does not do.** No AWS resources were created, no
`terraform plan`/`apply` was run, no credential of any kind (ambient or
supplied) was used against a real cloud account. No B2B/Partner/Retail/
Stage 10 work. No HDR-SB-1/HDR-J-7 decision made or worked around. ADR
0009's open AUP/legal confirmation is untouched and still blocks any real
production deployment. The pre-existing, non-9.4-caused
`IssueCredentialToken` concurrent-request race (documented in ADR 0085's
"What this decision does not do") remains open, recorded as a separate,
narrower deferred item, not folded into this fix.

## Stage 9.3 — Staging Deployment + Real End-to-End Acceptance — COMPLETE (superseded by Stage 9.4 above)

**Purpose.** Authorized by the human as "STAGE 9.3 — STAGING DEPLOYMENT +
REAL END-TO-END ACCEPTANCE," opening with "Stage 9.2 is approved." Not a
production launch: the objective was to deploy the existing B2C and Back
Office MVPs into a real, isolated, non-production staging environment and
run genuine end-to-end acceptance against it — real Postgres, a real
running `platform-api`, real built frontends, real browser flows (headless
Chromium) and real HTTP calls, not a re-read of existing unit/integration
tests. Explicit constraints honored throughout: no redesign of completed
architecture, no B2B/Partner/Retail/Stage 10 work, no undocumented
external provider integration, no HDR-SB-1/HDR-J-7 decision made or
worked around, no production-readiness claim, no silent commitment to a
permanent hosting/domain/AUP decision.

**Hosting decision.** The human explicitly declined to let this session
use the ambient AWS credentials present in the container (unconfirmed
account/billing ownership) and instead directed: design a production-grade
AWS staging architecture, but do not provision anything until the account
is confirmed. Per the directive's own fallback for exactly this situation
("do not fabricate access... produce the smallest possible deployment
package and identify the exact one-time operator action required"), this
stage delivered a complete, independently-validated Terraform deployment
package and runbook, and validated the actual application by running it
directly in this sandbox (real Postgres, real Go binary, real built
frontends) rather than against a real cloud URL. See ADR 0084 for the
staging AWS architecture and its production-promotion path, and
`docs/runbooks/stage-9-3-staging-deployment-runbook.md` for the exact
steps and the one operator action this package is blocked on.

**Deployment package: DELIVERED, independently validated, NOT applied
anywhere real.** `deploy/docker/` (multi-stage Dockerfiles for
`platform-api`+`cmd/migrate` and a shared parameterized frontend image for
both SPAs; no Docker daemon exists in this sandbox, so builds were
reviewed line-by-line and hadolint-checked rather than executed — stated
honestly, not claimed as tested). `deploy/aws/` (10 Terraform modules —
network/security/database/ecr/secrets/iam/ecs/alb/dns/observability — plus
a `staging` environment root module, staging-sized defaults, a deploy
script, and the RDS role-init SQL mirroring this codebase's own
`igaming`/`igaming_runtime` split, corrected mid-stage to run the
`schema_migrations` write-revoke strictly AFTER migration, matching
`.github/workflows/ci.yml`'s own canonical sequence exactly). `terraform
fmt`/`validate` both run independently by the orchestrator (not merely
trusted from the implementing agent) against a real Terraform binary,
clean. No `terraform plan`/`apply`, no AWS credential of any kind used.
Ambiguity resolved honestly: HTTPS requires a supplied domain +
Route53 zone (both null by default → plain-HTTP ALB-DNS-name fallback,
never a fabricated domain decision).

**Local staging-equivalent validation: DELIVERED.** A fresh Postgres
database, all 90 migrations applied and `migrate verify`-clean, the exact
`igaming`/`igaming_runtime` role split (including the schema_migrations
write-revoke, applied and re-verified after a real ordering mistake was
caught mid-session), `platform-api` run with `APP_ENV=staging` (never
`production`) against the runtime role, and both frontends built and
served against it. A real, load-bearing gap was found and closed here:
no CORS middleware existed anywhere in this codebase, and the directive's
own required staging URL structure (three distinct subdomains) is
cross-origin by construction — closed with a small, config-driven,
allowlist-only `CORS_ALLOWED_ORIGINS` mechanism
(`internal/httpserver/cors.go`) that is explicitly documented as a
browser convenience, never an authorization boundary (every route's real
server-side authorization is unaffected by Origin).

**Two genuine testability gaps found by real acceptance testing, both
closed as small, reviewed, non-production-gated seams mirroring an
existing, already-reviewed precedent (`CasinoPlaySimulationEnabled`,
Stage 7) — never worked around, never silently patched:**

1. **No mock deposit could be completed via the real HTTP API alone** —
   `payments.MockProvider`'s signing secret is intentionally unrecoverable
   outside its own process. Closed by `POST /v1/me/deposits/{id}/
   simulate-callback`, gated behind `PaymentsMockSettlementEnabled`
   (`cfg.Environment != "production"`), which asks the tenant's own
   registered mock provider to mint a correctly-signed callback for the
   caller's own pending deposit and feeds it through the real
   `Orchestrator.ReceiveCallback` pipeline — never bypassing signature
   verification. Security-reviewed: **APPROVED WITH MINOR NOTES** (the
   reviewer independently re-verified every trust-boundary claim against
   the code, proved the request body is genuinely ignored via an
   adversarial test, fixed one real omission — audit records lacked
   IP/user-agent/request-id — and flagged, as a separate pre-existing
   Stage-7-origin finding, that both mock-simulation flags fail OPEN if
   `APP_ENV` is ever misconfigured in production; see "Remaining
   production blockers" below).
2. **No player registered through the real flow could ever reach `active`
   status** — the sole gate every deposit/bet/casino-launch action
   consults, reachable in the codebase only via email-verification
   confirmation, whose raw token `email.MockProvider.Send` deliberately
   never exposes (no real inbox to deliver to). This blocked the
   directive's own required critical end-to-end chain for every test
   player, not an edge case. Closed by `GET /v1/me/email-verification/
   dev-token`, gated behind `AccountActivationTestSupportEnabled`, which
   lets a player fetch (never bypass) their own pending token and feed it
   into the real, completely unmodified confirm endpoint. Security-
   reviewed: **APPROVED WITH MINOR NOTES** (independently re-verified
   identity derivation, cross-tenant isolation via a fresh adversarial
   probe, and non-weakening of the real token lifecycle; flagged a real
   but non-security multi-replica operational gap — the in-memory token
   store is per-process, so this seam silently fails on the staging
   Terraform's own default 2-replica topology — recommended fix
   documented, not applied this stage; also flagged as pre-existing, not
   blocking: the same `APP_ENV` fail-open pattern as item 1).

**One genuine, deterministic financial defect found by real end-to-end
use (not a contrived test) and fixed this stage.** The payments and
casino mock adapters were both registered under the literal `provider_id`
`"mock"` in `cmd/platform-api/main.go`, and both independently generate
transaction references via an identical low-entropy per-instance
sequential counter starting at 1. Since the ledger's idempotency key is
`(provider_id, provider_tx_id)`, this deterministically aliases the FIRST
transaction from each domain onto the same key
(`"mock:mock-1"`) — reproduced live: a funded deposit followed by the
first-ever casino rollback in the freshly-seeded environment failed with
`internal/ledger`'s own reused-idempotency-key guard correctly refusing
the collision (no ledger corruption resulted — the guard did its job, but
the operation itself was unusable). Fixed by re-registering the two
adapters under distinct ids (`mock-payments` / `mock-casino`) — the
natural shape any real deployment would have anyway, since no two real
vendors would ever share a provider identity. Verified by direct
regression: a fresh deposit, wager, win, and **rollback** (the exact
previously-failing operation) all now post correctly, each under its own
non-colliding idempotency key, with a full ledger/audit trail and correct
Back Office UI reflection. Full repo build, `go vet`, `gofmt`, and the
entire unit + integration suite re-run clean after the fix (no test file
depended on the literal string, since each constructs its own
orchestrator independently).

**Full acceptance results (real browser + real API, this stage's own
required checklist).** B2C: register/login/logout/re-login, view
account/wallet, mock deposit with confirmed balance update, sportsbook
catalogue browse and a real accepted bet, casino catalogue browse and a
full launch/wager/win/rollback cycle, transaction/bet/session history, and
understandable error messages — all PASS. Back Office: login (both tenant
staff and platform_admin), dashboard/tenant/brand/player navigation,
KYC/RG/bonus/withdrawal views populated with real seeded state, two real
approval workflows exercised (KYC review, and a genuine two-distinct-
approver withdrawal four-eyes), audit log, and cross-tenant isolation
(re-verified three times, including across three `platform-api` restarts)
— all PASS. **Critical end-to-end chain** (deposit → ledger → audit →
Back Office UI, and separately for sportsbook/casino/withdrawal) — PASS in
full, including non-trivial `SUM(debits) = SUM(credits)` verification and
real audit records for every action. A full, independent live security
acceptance pass (18 named items: anonymous access, player/tenant/staff
isolation, forged tenant/brand ids, invalid/expired/forged JWT, session
reuse, CORS, cookies, frontend-bypass, RLS, runtime DB role, secret/
source-map leakage, rate limiting) — **18/18 PASS**, no category-A
findings; two category-D notes recorded below.

**Remaining production blockers (not staging blockers).** (1) `APP_ENV`
fails OPEN for all three non-production simulation flags
(`CasinoPlaySimulationEnabled`, `PaymentsMockSettlementEnabled`,
`AccountActivationTestSupportEnabled`) — an unset or mistyped value in a
real production environment would silently register money-minting/
account-activation test routes. Pre-existing since Stage 7, surfaced and
formally flagged (not fixed) this stage per the reviewing specialist's own
recommendation, since closing it changes platform-wide config semantics
and needs an `architect`/`config` decision, not a unilateral one. (2) The
account-activation dev-token store is per-process and will silently 404
under the staging Terraform's own default 2-replica ALB topology if a
request and its token fetch land on different replicas — documented
interim mitigation (`desired_count=1` during acceptance runs) and a
stateless redesign recommendation, not yet applied. (3) ADR 0009's
hyperscale-cloud gambling-AUP confirmation remains open — unaffected by
this stage, still blocks any real production deployment regardless of
provider. (4) HDR-SB-1 and HDR-J-7 remain unresolved by design — this
stage neither decided nor worked around either.

**Update (Stage 9.4, see the "Current stage" section above and ADR
0085):** blockers (1) and (2) above are now CLOSED. (1) `Load()` now
validates `APP_ENV` against a closed set at startup and requires a second,
independent, explicit opt-in before any simulation flag registers — a
mis-set or unset `APP_ENV` can no longer silently enable anything. (2)
`GET /v1/me/email-verification/dev-token` (the per-process store this
item describes) has been removed; the token is now returned directly in
the same request that creates it, and multi-replica correctness (the
staging Terraform's default `desired_count=2`, no longer requiring the
`desired_count=1` interim mitigation described above) is proven by a
dedicated test. Blockers (3) and (4) remain open, unaffected by Stage 9.4,
exactly as this section originally described.

**No B2B/Partner/Retail/Stage 10 work was performed. No undocumented
external provider API was integrated or invented. No production
credential was requested, created, or used — the ambient AWS credentials
in this environment were explicitly identified and explicitly NOT used,
per the human's own instruction. No production-readiness claim is made.**

## Stage 9.2 — Sportsbook Risk + Jurisdiction Enforcement + Casino Governance — COMPLETE (superseded by Stage 9.3 above)

**Purpose.** Authorized by the human as "STAGE 9.2 — SPORTSBOOK RISK +
JURISDICTION ENFORCEMENT + CASINO GOVERNANCE," opening with "Stage 9.1 is
approved." A consolidated production-critical stage closing the three
items Stage 9.1 classified FIX BEFORE PRODUCTION: `ARCH-DB-2` Phase 2
(casino four-eyes governance for `casino_games.jurisdiction_blocklist`
removal), the missing sportsbook cumulative risk/exposure architecture,
and the missing sportsbook jurisdiction/operating-market enforcement
path. Explicit operating principle: fix meaningful risk now, do not
reopen completed architecture without a concrete defect, no mini-stages
for cosmetic issues. Full task table and review findings:
`docs/governance/task-registry.md`'s Stage 9.2 section.

**Workstream A — casino four-eyes governance: CLOSED.** New migration
`0086` (hardened to `0089` after review) implements exactly ADR 0081
§5.2's already-specified design: two new platform-scoped tables
(`casino_catalogue_change_requests`/`_approvals`), maker/checker
separation with self-approval denial, atomic consume-and-apply,
payload-matched request selection, and full audit records. Two real
bugs were found by independent review and fixed before this stage closed:
the self-approval check initially mirrored a weaker precedent that two
unlinked staff accounts (the actual default shape `seed-admin` produces)
could defeat entirely — upgraded to this codebase's own later, stronger
precedent; and the approval-consumption logic initially picked the
oldest pending request rather than the one whose payload actually
matched, which could deadlock two legitimate, independently-approved
compliance actions on the same game — fixed by adopting the exact
pattern this codebase already uses for the identical problem elsewhere.

**Workstream B — sportsbook cumulative risk: CLOSED (mechanism), unarmed
by design.** A full trace of the existing Risk & Limits engine found the
prior stage's characterization of this gap was itself stale in several
respects (corrected after verification, not assumed). One new
`risk_rules` map entry activates player-scoped rolling-window stake caps
for sportsbook bets, reusing the exact same tenant-configurable,
already-audited mechanism every other risk rule already uses — no new
threshold was invented. A separate, structurally distinct mechanism (a
new `sb_exposure_limits` table plus an event-scoped advisory lock) closes
cross-player, per-outcome book exposure, which was proven — with four
independently-verified structural reasons — to be inexpressible in the
generic risk-rules model and therefore correctly built as its own
sportsbook-domain control rather than forced into the wrong system. Both
mechanisms ship completely unarmed: zero rows configure either control
anywhere. **A new Human Decision Register item, `HDR-SB-1`, records
that sportsbook production go-live under a platform-carries-the-book
model must not proceed without this ceiling being authorized** — the
mechanism's existence required no such decision, only its eventual
arming does.

**Workstream C — sportsbook jurisdiction/market gating: PARTIALLY
CLOSED, correctly and explicitly not fully closed.** Player jurisdiction
resolution and sportsbook-catalogue-level restriction (a new
platform-scoped `sb_jurisdiction_restrictions` table, deny-only by
construction) are both implemented and independently verified, mirroring
casino's own existing jurisdiction-blocklist mechanism exactly, with
both the catalogue-visibility and bet-placement enforcement points
sharing one single implementation (a static regression test enforces
this can never drift into two divergent copies). The licence-ceiling/
operating-market rung of the canonical flow is **specified in complete,
implementation-ready detail but deliberately not shipped even as a stub**,
because it is genuinely blocked on the platform's pre-existing,
already-registered `HDR-J-7` decision — no player-scoped operating-
country determination exists anywhere in this codebase to feed it. This
is not a new gap this stage discovered; it is an existing, disclosed
blocker this stage could not and did not attempt to route around.

**Mandatory five-specialist review** (security, ledger-finance, an
architect self-verification pass against its own design's binding
invariants, qa, code-reviewer) found: one P1 security finding and one
independently-corroborated correctness bug (both described under
Workstream A above, both fixed); one genuine information-leak channel
(a rejection-category side-channel letting a client infer trading-book
capacity even though no raw number ever leaked — fixed by collapsing the
player-facing signal); one defense-in-depth gap in the new sportsbook
jurisdiction table mirroring a class of issue already fixed once for
casino in Stage 9.1 (fixed by applying the identical, already-established
pattern); and several test-integrity gaps, including two tests that
silently exercised nothing due to a context-handling bug (repaired and
re-verified to actually test what they claimed).

**Full independent validation, run by the Orchestrator against a fresh
`stage92_final` database** (not merely trusted from ten specialist
self-reports): all 90 migrations apply cleanly with `migrate verify`
reporting the whole chain clean; the full integration suite and the full
`-race` suite (32 packages) are green; the 12-probe runtime-role
adversarial suite is green; every new concurrency-sensitive test family
was repeated 3x under `-race` with identical results; the Stage 6
sportsbook and Stage 7 casino defining acceptance tests pass by exact
name; both frontends' tests, typechecks, and production builds are clean
and confirmed unchanged (this stage touched no frontend code); the
OpenAPI specification validates cleanly.

**Human/legal decisions this stage neither made nor attempted:** any
answer to `HDR-J-6/7/8/9` or `HDR-M-1/2`; the new `HDR-SB-1` (exposure
ceiling ownership); any production database cutover, hosting decision,
or credential action; any legal or regulatory determination.

**Status: technically COMPLETE. No B2B/partner/retail work started. No
undocumented external provider API was integrated — no real sportsbook,
casino, payment, or KYC provider contract was invented or assumed
anywhere in this stage's work. Per the directive's own final instruction,
this session STOPS here — Stage 10 is NOT authorized and will not begin
without explicit human authorization.**

## Stage 9.1 — Production Blocker Closure — COMPLETE (superseded by Stage 9.2 above)

**Purpose.** Authorized by the human as "STAGE 9.1 — PRODUCTION BLOCKER
CLOSURE," opening with "STAGE 9 IS APPROVED." A focused hardening stage
closing the concrete, within-engineering-boundary gaps Stage 9 identified
but did not fix: `ARCH-DB-2` (database backstop for 6 RLS-free
platform-wide catalogue tables), `LOCK-1` (canonical financial lock
ordering across ledger/casino/sportsbook/payments/withdrawal/bonus), the
two rate-limiter launch gates, `PLAT-MIGDRIFT-1` (migration content-drift
protection), the live shared-dev-DB drift instance it caused, a
provider-error-body safety P3, and 6 named minor technical debt items.
Full task table and review findings: `docs/governance/task-registry.md`'s
Stage 9.1 section.

**`ARCH-DB-2` closed.** New ADR 0081 ruled, after verifying the actual
schema, that none of the 6 tables (`casino_games` + 5 sportsbook
catalogue tables) has any tenant/brand column — they are genuinely
platform-wide catalogue data, so per-tenant RLS is the wrong model
entirely (not merely unbuilt). The correct backstop, built in migration
`0084`: `ENABLE`+`FORCE ROW LEVEL SECURITY` with open reads and writes
scoped to one of two platform-level identities — the existing
`WithPlatformAdmin` for `casino_games`, and a new, closed-vocabulary
`db.WithPlatformService` for the sportsbook catalogue's unattended
startup sync (explicitly not the human-admin identity, to avoid
laundering a machine process as a person). A fix-round migration (`0085`)
closed a related defense-in-depth gap a security review found: the
`casino_games` write policy checked only that a platform-admin GUC was
set, not that it resolved to a real staff member — now mirrors the
`assets` table's existing precedent.

**`LOCK-1` closed, and scope widened honestly.** New ADR 0082 traced
every money-touching lock site across 6 packages and found the original
finding understated the problem: three more genuine ABBA cycles existed
(`LOCK-1b` in payments, `LOCK-1c` in withdrawal, `LOCK-1d` a row-lock/
advisory-lock cycle between casino and bonus) that had never been
recorded. A new canonical lock order and a single new
`ledger.LockProjectionsForPosting` pre-lock step (implemented by
`ledger-finance`, who made an independent, documented judgment call
accepting the ADR's proposed zero-row-materialization design after
verifying it doesn't weaken any financial invariant) closes all four.
Every one of the 6 required deadlock-freedom tests was individually
proven to fail on pre-fix code (a real `40P01` deadlock) before being
shown to pass after — not merely written and trusted. One exception
(`E-1`, pre-existing) and one new exception (`E-2`) are named, bounded,
and grep-testable rather than silently left inconsistent — the directive
required this honesty over a clean "fully fixed" claim.

**Mandatory security + code-reviewer review of both changes** (CLAUDE.md:
security-sensitive work is not complete without it) found no P0/P1
security findings, one genuine correctness regression (a grant-lookup
error losing its `ErrNotFound` wrapping, causing 404→500 — fixed), and
strengthened two regression-guard tests that were meaningfully easier to
defeat than intended. All P1/P2 findings were closed in a fix round; P3s
are documented, not fixed, consistent with this project's "fix P0/P1
always, document P3/P4" convention.

**Other closures:** the rate limiter now has an explicit trusted-proxy
model (default: trust nothing, an untrusted `X-Forwarded-For` is never
read) and its per-minute limit is wired through real configuration;
`PLAT-MIGDRIFT-1` is closed with a migration content-hash column and a
new `migrate verify` CLI subcommand, run for real against CI and against
the exact shared dev database that had previously drifted (which was
also rebuilt from a clean chain); a provider-error-body safety net now
bounds and redacts captured response content without removing legitimate
diagnostics.

**Full independent validation, run by the Orchestrator against a fresh
`stage91_final` database** (not merely trusted from specialist
self-reports): all 85 migrations apply cleanly; `migrate verify` reports
the whole chain clean with no drift or gaps; the full integration suite
and the full `-race` suite (32 packages) are green; the runtime-role
adversarial suite and the new catalogue-RLS assertions are green; the
lock-ordering concurrency tests were repeated 3x under `-race` with
identical results; the Stage 6 sportsbook and Stage 7 casino defining
acceptance tests pass by exact name; both frontends' tests, typechecks,
and production builds are clean. This run itself caught and fixed one
real, missed occurrence of the project's own migration-count-fixture
maintenance pattern (documented in the task registry as a concrete
argument for never skipping this independent final gate).

**Status: technically COMPLETE. No B2B/partner/retail work started. No
undocumented external provider API integrated. No jurisdiction, legal, or
production-infrastructure decision made or attempted — those remain
explicitly out of this session's reach. Per the directive's own final
instruction, this session STOPS here — Stage 10 is NOT authorized.**

## Stage 9 — Production Readiness, Security, Resilience & Launch Hardening — COMPLETE (superseded by Stage 9.1 above)

**Purpose.** Directed by the platform owner as "STAGE 8 IS APPROVED...
Begin STAGE 9," a large, deliberately non-micro-staged production-
readiness pass across 28 named sections: database/role hardening,
migration safety, deployment, config/secrets, auth/session, RBAC,
tenant/brand security, financial-integrity adversarial concurrency,
withdrawal four-eyes, payment/provider readiness, RG/KYC, jurisdiction,
observability, health/readiness, backup/DR, retention/audit,
performance/load, API security, frontend readiness, CI/CD, operational
runbooks, compliance-evidence-only, and technical-debt disposition.
Explicitly out of scope: B2B/partner/retail, any undocumented external
provider integration, invented regulatory requirements, and any of the
unresolved jurisdiction human decisions (HDR-J-6/J-7/J-8/J-9). Full task
table and review findings: `docs/governance/task-registry.md`'s Stage 9
section.

**Critical production blocker closed at the in-repo/mechanical level:**
`PLAT-ROLESPLIT-1` — the platform previously ran all runtime traffic as
`igaming`, the table-owning migration role, which (regardless of RLS
correctness for ordinary DML) can always issue owner-only DDL
(`DISABLE ROW LEVEL SECURITY`, `DROP TABLE`, etc.) to remove that
protection entirely. A new non-owning `igaming_runtime` role now exists
with exactly `CONNECT`/`USAGE`/`SELECT`/`INSERT`/`UPDATE`/`DELETE` grants
(no `TRUNCATE`, no `schema_migrations` write access), a fail-closed
production-startup check (`internal/db.VerifyRuntimeRoleInProduction`)
refuses to start if the connecting role owns tables, and a 12-probe
adversarial test suite (session_replication_role, SET ROLE, ALTER TABLE,
DROP TABLE, TRUNCATE, ALTER/DROP POLICY, DISABLE RLS, OWNER TO, CREATE
ROLE, ALTER ROLE BYPASSRLS, SECURITY DEFINER) confirms every escalation
path is denied. **The actual production cutover to this role still
requires a human operator with real production database credentials —
this session cannot perform it.**

**Two genuine, previously-undetected financial defects found and fixed**
(both empirically reproduced by stashing the fix and re-running): a
double stake-release race allowing two concurrent win callbacks on one
locked round to each credit the player (driving `player_locked_cash`
negative while every individual posting still balanced — invisible to
the platform-wide debit/credit invariant); and an unhandled Postgres
deadlock between a win and rollback of the same round from a lock-order
inversion. Both fixed in `internal/casino/orchestrator.go`'s `postWin`.

**Other real gaps closed:** a responsible-gaming enforcement gap (a
self-excluded or suspended player could deposit indefinitely — RG was
only ever checked on gameplay, never on deposit) wired into
`InitiateDeposit`; a brand-pinning integrity gap on 6 tenant-owned tables
(`ARCH-DB-3`, same defect class Stage 6.1/7 already fixed elsewhere); a
sportsbook-bet/deposit-intent/reconciliation/login-attempt immutability
gap closed via new column-level triggers (migration 0082); a back-office
double-submit gap on the bonus four-eyes approval queue (the one
protected action that had been missed); an incidental-only (not
enforced-by-design) block on staff tokens reaching player self-service
endpoints, now enforced by a new `RequirePlayerPrincipal` middleware
across all 31 player-only routes; and unauthenticated-credential-route
rate limiting (register/login/refresh/password-reset/email-verification).

**Deferred, not silently dropped — both with full documented reasoning:**
`ARCH-DB-2` (6 RLS-free catalogue tables have application-level-only
write authorization, no DB backstop — a cross-domain casino+sportsbook+
internal/db+cmd fix, not attempted in a parallel window) and `LOCK-1` (a
real ABBA deadlock risk between `postBet` and `postWinDirectCash` on
wallet-projection rows; the architect's suggested in-`Post` sort fix was
rigorously proven not to close the cycle, since `postBet` locks its cash
account outside and before calling `ledger.Post` — needs a cross-cutting
lock-ordering discipline, an architect-level decision for a future
stage).

**Backup/disaster-recovery: confirmed, not assumed, near-empty.** No
automated backup mechanism, no tested restore, no replica/standby exist
anywhere in this codebase or its `deploy/` tooling. This is structurally
blocked on ADR 0009's still-open hyperscale-hosting-provider decision (no
cloud account has been provisioned; only local/dev environments exist),
not an oversight of this stage — documented honestly, with a concrete
minimum action plan, in `docs/runbooks/backup-and-disaster-recovery.md`.
Stated targets (RPO=0 for the ledger, RTO<15min) are **NOT MET**.

**Full independent validation, run by the Orchestrator against a fresh
`stage9_final` database** (not merely trusted from specialist self-
reports): all 82 migrations apply cleanly with a verified up→down→up
round-trip; `go build`/`go vet`/`gofmt` clean; the full integration suite
(32 packages) green; the full `-race` suite green; the 12-probe runtime-
role adversarial suite green; the Stage 6 sportsbook and Stage 7 casino
defining acceptance tests pass by exact name; both `b2c` and `backoffice`
frontends' test suites, typechecks, and production builds are clean
against the final merged state.

**Mid-stage infrastructure event:** 6 of 9 initially-dispatched specialist
agents failed to a weekly API rate-limit error. The Orchestrator diagnosed
actual repo damage (one build break, fixed directly — an unused import
left by an interrupted agent), stopped and explicitly asked the human
before proceeding rather than guessing, and resumed all 6 workstreams
(each instructed to inspect its own partial work first, not restart or
duplicate) once an empirical probe confirmed the human's plan upgrade had
resolved the block. No rework resulted; three resumed agents' interrupted
work was found already ~85-100% complete and correct.

**Human/legal decisions this stage neither made nor attempted:** the
production `igaming_runtime` cutover (needs real prod credentials); the
hyperscale hosting-provider selection and its gambling AUP confirmation
(ADR 0009, blocks real backup/DR); the data-retention-period decision
(`docs/architecture/16-privacy.md`); any of HDR-J-6/J-7/J-8/J-9; and any
licence-status/expiry/dual-licensing legal determination
(`MKT-LICSTATUS-1`, `MKT-EXPIRY-1`, `MKT-DUAL-1`).

**Status: technically COMPLETE. Full Stage 9 report delivered to the
human with an evidence-based final classification. No B2B/partner/retail
work started. No undocumented external provider API was integrated. Per
the directive's own final instruction, this session STOPS here — Stage
10 is NOT authorized and will not begin without explicit human
authorization.**

## Stage 8 — Provider Integration Readiness Without External Contracts — COMPLETE (superseded by Stage 9 above)

**Purpose.** Originally scoped as "Dummy Sportsbook/Dummy Casino API
integration"; the platform owner confirmed at Stage 8's start that no
documentation, base URL, or credentials for either API were actually
available (verified by this session's own reconnaissance — repo grep,
`.env.example`, `docker-compose.dev.yml`, `/etc/hosts`, filesystem, all
negative) and explicitly re-scoped the stage to **provider-integration
readiness without any external API call**: harden the existing
`CasinoProvider`/`sportsbook.Provider` boundary and build everything a
real adapter will need later, without guessing that adapter's actual
shape. Full design record: `docs/decisions/0080-provider-integration-
readiness-without-external-contracts.md`.

**What was built** (all additive, no redesign of completed domain
architecture):

- `casino_provider_rounds` (migration 0080) — provider-neutral, durable
  binding of a future real provider's own round id to the platform's
  session/player/brand/game/correlation id, resolving ADR 0048's (Stage
  7) documented residual round-visibility limitation. RLS mirrors
  `casino_launch_sessions` exactly; an atomic ownership-conflict guard
  (empirically proven race-free under concurrent binding attempts)
  rejects a different player/brand naming an already-bound round with an
  HTTP 409 (never a 500, never an identity/round-id echo); a same-player/
  same-brand continuation across two launch sessions succeeds; a
  `BEFORE UPDATE` immutability trigger matches this repo's established
  precedent for comparable binding tables.
- `sportsbook_bets.provider_id`/`provider_bet_reference` (migration
  0081) — nullable, symmetric-null-constrained, tenant-scoped uniqueness,
  always NULL this stage (bet placement remains the existing synchronous,
  same-process `PlaceBet` flow). `sportsbook.Provider` deliberately NOT
  extended with a bet-placement method — the real contract's sync/async
  shape is unknown.
- `internal/providers/httpclient` — a generic, provider-name-agnostic
  outbound HTTP client (explicit timeout, bounded idempotent-only retry,
  four-category error classification with a `Sent`/delivered
  distinction, OTel spans, no automatic redirect following after a
  security-review-confirmed credential-exfiltration finding was fixed)
  plus a reusable `httpclient/conformance` contract-test harness, clearly
  labeled as testing the generic client, never an external API.
- `internal/providers/config.go` — generic, fail-closed provider
  configuration loading (enabled/disabled, base URL, credential
  *reference* — never a hardcoded value, timeout, retries). No specific
  provider is wired into `cmd/platform-api/main.go`.
- `docs/integrations/dummy-casino.md` / `dummy-sportsbook.md` — explicit
  "PENDING — no documentation available" records, per CLAUDE.md's "no
  fake completion" rule.
- Minimal Back Office visibility additions (`provider_round_id` on the
  admin casino rounds list, `provider_id`/`provider_bet_reference` on the
  admin sportsbook bets list) — no new screen, no provider-management
  console.

**Review and fix round.** Three parallel specialist reviews (architect,
security, qa) independently examined the above; security and qa each
independently reproduced the same finding (the generic HTTP client
forwarded its configured credential to any host a provider redirected
to). Three P1s were fixed before this stage closed: the credential-
redirect leak (client now refuses to auto-follow any redirect); an
unmapped ownership-conflict error surfacing as a retryable 500 with no
integrity alert (now an explicit 409 + alert log at both the public
webhook and the play-simulation endpoints); and the round binding
committing even when a bet was declined by RG/Risk/insufficient funds
(binding moved to immediately before the ledger post). Several P2s were
also fixed while still cheap (a same-session-only ownership predicate
that would have rejected a legitimate free-spins round continuation; a
missing DB-level immutability trigger; a context-cancellation retry-loop
bug; a `provider_bet_ref`→`provider_bet_reference` rename to match this
project's own pre-existing canonical naming in doc 09, done while the
column was still unwritten by any code path). Full findings, verdicts,
and disposition are in the Stage 8 completion report delivered to the
user.

**Validation.** Full repo test suite (32 packages, 1273 tests) passes
against a freshly-migrated (81 migrations, clean up/down/up round-trip)
scratch database; race-detector clean on every touched package; the
Stage 6 sportsbook and Stage 7 casino defining acceptance tests re-run
unmodified and pass; both frontends (`b2c/`, `backoffice/`) build and
test clean.

No external network call was made at any point. No real commercial
provider, B2B, retail, full reconciliation/settlement platform,
jurisdiction human decision, country approval, or wallet/ledger/identity/
RG/risk redesign was performed. Stage 9 was NOT started; no automatic
progression.

## Prior stage: Stage 7 — B2C Casino Player Experience + Casino Vertical Slice — COMPLETE, awaiting human review

**Purpose.** Prove the existing casino/provider/payment/wallet/ledger
architecture (built and hardened Stage 4A onward) supports a real
player-facing casino flow end to end — register → login → deposit →
wallet → casino lobby → select game → launch → wager → win → rollback →
wallet → player history → Back Office visibility → audit — using the
existing mock casino provider, never a real one. Explicitly not the
complete casino product.

**Baseline verification.** Re-confirmed independently: branch
`claude/focused-wright-jw88w9` at commit
`cb8a0b3a3b3476f6580fd787502f804181286516` (Stage 6.1's own final commit),
working tree clean, remote synchronized, full 32-package
`-tags=integration` suite green against a fresh 78-migration database.

**Pre-stage security fix (directive §2, done before any player-facing
casino code was written).** `casino_launch_sessions` carried the identical
brand-pinning weakness Stage 6.1 found and fixed in `sportsbook_bets`: the
original `(brand_id, tenant_id) → brands` FK pinned brand only to "some
brand in this tenant," never specifically to the launching player's own
brand. Migration `0079` replaces it with a single composite
`(player_account_id, tenant_id, brand_id) → player_accounts` FK, reusing
the same `player_accounts_id_tenant_brand_key` constraint the sportsbook
fix already established as the platform pattern. Verified via a full
up→down→up round-trip on a scratch database and 6 new adversarial tests
(`internal/casino/launch_session_brand_pinning_test.go`): correct
player/tenant/brand succeeds; wrong brand in the same tenant, wrong
tenant, a forged player id, and a forged brand id are all rejected via FK
violation; a cross-tenant write attempt is rejected via RLS. Not reachable
via the application before this fix (the handler already derived
`BrandID` server-side), but a real database-level integrity gap per
CLAUDE.md's "RLS, not application-code discipline" rule.

**Backend — additive only, no orchestrator/schema redesign.** Two new
read-only endpoints (`GET /v1/me/casino/rounds`, `GET
/v1/admin/casino/rounds`, gated on a new `casino_transaction:read`
permission mirroring `sportsbook_bet:read`'s exact role grants) reconstruct
a casino "round" by anchoring on `casino_launch_sessions` and re-deriving
its ledger effects via the SAME `roundCorrelationID` derivation
`postBet`/`postWin`/`postRollback` already use — `ledger_transactions`
itself carries no player attribution or player-scope RLS policy at all
(migration 0028), so the player-facing history endpoint runs under
`WithTenant` with an explicit `player_account_id` predicate as its actual
authorization boundary, exactly like `newLaunchCasinoGameHandler`'s own
precedent, never `WithPlayerScope`. Three new player-authenticated
play-simulation endpoints (`POST /v1/me/casino/sessions/{id}/wager|win|
rollback`) stand in for a real hosted game client (none exists this
stage): each asks the tenant's registered mock provider to build a
correctly-signed callback payload and feeds it through the exact same
`Orchestrator.ReceiveCallback` pipeline the public webhook uses — see ADR
0048 for the full trust-boundary analysis this design required.

**Specialist review round — one P0, independently confirmed FOUR times.**
Architect, security, ledger-finance, and database/RLS reviews each
independently reproduced, live over HTTP, the same defect: the
play-simulation seam's mock signature is minted by the platform on the
authenticated player's own behalf, so it authenticates nothing about which
transaction a player names in a rollback request. `postRollback`'s own
lookup (`tenant_id, provider_id, provider_tx_id` only) is correct for a
real, independently-signed provider webhook and was never designed to
receive a player-supplied reference — without an additional check, a
player could reverse a DIFFERENT player's bet/win, reverse their own
transaction from an unrelated round, or write a permanent ledger tombstone
against a never-issued reference (a tenant-wide, unrecoverable
denial-of-service, since the ledger is append-only). Fixed in
`internal/httpserver/casino_play_handlers.go` via
`requireRollbackTargetOwnedByRound`/`casino.TransactionBelongsToRound`
(the named transaction must belong to the CALLING session's own round and
wallet), plus: `Deps.CasinoPlaySimulationEnabled` gates the three routes'
very registration to non-production only (a mock provider says yes to
everything, so "the provider type is restricted" is not itself a
deployment safety gate); a `ModeReal`/session-active/expiry check on
rollback (previously only on wager/win); an amount cap
(`maxCasinoPlaySimulationAmount`); a required, client-supplied
`idempotency_key` on wager/win with a deterministic derived
`provider_tx_id` (a retry after a lost response was, before this fix, a
genuinely new financial transaction — ledger-finance review's own
independent judgment call, rated P1, "not acceptable as a disclosed
mock-only limitation" given the platform already decided this question
one stage ago for sportsbook bet placement); and a player-attributed
`casino_play_simulation.*` audit record alongside the existing
system-attributed ones. Also fixed: `internal/casino/history.go`'s
multi-leg round summary previously overwrote rather than accumulated
repeated bet/win/rollback legs (ledger-finance finding — a round with two
wagers under-reported its true stake); pagination lacked a tiebreaker
(database/RLS finding); an RLS-rationale doc comment cited the wrong
migration. Five new adversarial regression tests reproduce and close each
exploit path directly: cross-player rollback, cross-round rollback (same
player, different session), forged-reference tombstone-poisoning, demo-
session rollback, and over-cap amount rejection — plus an idempotent-retry
test and a player-history cross-player-isolation test. Recorded as ADR
0048 (`docs/decisions/0048-casino-play-simulation-trust-boundary.md`),
including its own removal condition (delete this seam the moment a real
provider adapter exists).

**Frontend.** `b2c/src/features/casino/` (lobby, launch, a session screen
driving wager/win/rollback with live wallet balance, and a paginated round
history page) and `backoffice/src/features/casino/` (a minimum-visibility,
read-only round queue, permission-gated identically to the sportsbook
precedent — `tenant_admin`/`support`/`compliance`/`finance`, never
`platform_admin`) were built by dedicated frontend/backoffice specialists
against the finished backend contract and independently re-verified
(40 combined frontend tests passing, both apps build/typecheck cleanly).

**Acceptance tests.** A new defining Stage 7 acceptance test
(`TestStage7_B2CPlayerRegisterToBackOfficeVisibility_DefiningAcceptanceTest`)
drives the full real HTTP path — register → real deposit (payments mock
provider webhook) → wallet → casino lobby → launch → wager → win →
rollback → wallet → player history → Back Office visibility → audit trail
— with exact financial value assertions at every step (including a
reversed WIN correctly restoring the pre-win balance, not merely "some
change happened"). The Stage 6 sportsbook acceptance test was re-run
against the same final state and remains green — zero regression.

**Final validation.** Full `-tags=integration` suite (33+ Go packages,
fresh 79-migration database) green; both frontends' test suites green;
`gofmt`/`go vet` clean. Known, disclosed limitations (not defects): the
round read model is accurate only for simulation-originated rounds (a real
provider posting via the public webhook without the session-anchored
round-id convention would render blank in this view — ADR 0048's own
"known residual limitation" section); the pre-existing Stage 4A casino
catalogue/launch/webhook endpoints remain undocumented in the OpenAPI spec
(pre-existing debt, not introduced this stage); an admin round-listing
index and a `NOT VALID`+`VALIDATE CONSTRAINT` migration split are deferred
production-scale hardening, matching the sportsbook precedent's own
disclosed debt.

**Stage 8 was NOT started.** This report and its explicit stop instruction
stand; the next stage requires separate human authorization.

## Prior stage: Stage 6.1 — B2C/Sportsbook Hardening & Architectural Closure Gate — COMPLETE, awaiting human review

**Purpose.** A focused hardening/closure pass over the just-completed
Stage 6 diff — verify boundaries, fix real defects, document safe
deferrals, stop. Not a feature stage: no casino, no settlement/cashout,
no real sportsbook provider integration, no production country
approvals, no HDR-J-6/7/8/9 answers, no jurisdiction architecture
reopened.

**Baseline verification.** Re-confirmed independently rather than
trusting the Stage 6 completion report: branch/commit/push status
matched (`be6042d1189a365bd9026577422dea69df8bbec4`), working tree was
clean, a fresh 78-migration scratch database built cleanly, the full
31-package `-tags=integration` suite was green, and both frontends
(`backoffice/` 17 tests, `b2c/` 18 tests at baseline) built and passed.

**Five focused specialist reviews** (architect, security, ledger-finance,
a database/RLS-specific architect pass, qa) — no broad re-review of
frozen architecture, each scoped to this diff only.

**One P0-severity fix (found by security, independently reasoned through
by me before dispatch): the B2C bet slip and deposit form minted a FRESH
idempotency key on every submit ATTEMPT**, not once per composition.
Because the button-disable-while-pending only covers a double-click, a
genuine network/timeout error - where the original POST may have
actually committed and only the response was lost - meant the retry sent
a DIFFERENT key and placed a real second bet (or second deposit) with a
real second stake/amount debit. This defeated the entire purpose of the
idempotency-key mechanism for the exact failure mode it exists to cover.
**Fixed**: `BetSlipContext.setSelection` now mints the key once per
selection and holds it for the lifetime of that composition (regenerated
only on a new selection or `clear()`, never on retry);
`DepositPage` mints it once per form composition, regenerated only after
a successful submission. New regression tests
(`BetSlip.test.tsx`'s "reuses the exact same idempotency key on a retry
after a network error") lock this in. The `idempotencyKey.ts` helper's
own doc comment, which had codified the wrong contract, was corrected.

**Two P1/P2-equivalent financial-integrity fixes, both found
independently by the architect and ledger-finance reviews:**
1. Strengthened the Stage 6 ledger-idempotency-key fix further: the key
   is now `"sportsbook_bet:" + player_account_id + ":" + idempotency_key`
   (previously just `player_account_id + ":" + idempotency_key`) - an
   explicit transaction-type discriminator, not reliance on other
   domains' key formats happening to differ. New regression test
   (`TestPlaceBet_LedgerIdempotencyKeyIsNamespacedByTypeAndPlayer`) pins
   the exact stored format.
2. **Ledger-finance's own finding**: the Stage 6.1 key-format change
   itself introduced a latent rolling-deploy hazard - if an old-format
   node and a new-format node both handled the same (player,
   idempotency_key) bet concurrently, each would derive a DIFFERENT
   ledger key, each successfully post its OWN ledger transaction (a
   genuine double stake-lock), and only one would win the
   `sportsbook_bets` unique-constraint race - silently orphaning the
   loser's ledger posting with no bet row pointing at it. **Fixed**: a
   3-line cross-check after `insertBet`'s conflict-resolution path
   verifies the resolved bet's `ledger_transaction_id` matches what THIS
   call posted; a mismatch aborts the whole transaction (rolling back its
   own orphaned posting) rather than silently committing one.

**One real DB-level integrity gap, found by the database/RLS review and
fixed via an additive migration change (migration 0078 is still
unreleased, so edited in place rather than superseded):** the original
three composite FKs on `sportsbook_bets` pinned wallet→tenant and
wallet→player, but left `brand_id` pinned only to "some brand in this
tenant," not specifically the player's own brand - not reachable through
the application today (`newPlaceBetHandler` always derives `brand_id`
correctly via `identity.GetPlayerAccountByID`), but a real DB-level gap
per CLAUDE.md's RLS/integrity-by-database rule. **Fixed**: replaced the
separate `brand_id`→`brands` FK with a single composite
`(player_account_id, tenant_id, brand_id)` FK against `player_accounts`,
reusing the exact `UNIQUE (id, tenant_id, brand_id)` key migration 0019
already added for `wallets`' identical pinning (the same established
platform pattern migration 0057's `bonus_grants` also uses). Also added a
plain FK from `sportsbook_bets.asset_code` to `assets(code)` (a QA/DB
review P3, cheap and correct - `wallets` already has this FK,
`sportsbook_bets` didn't).

**QA-found test-coverage gaps, fixed:** the three sibling rejection
branches (event finished/cancelled, market not open, selection not
active) shared one `RejectionCategory` and only the first had a test - a
refactor that deleted either of the other two checks would have passed
the full suite silently. Added `TestPlaceBet_MarketNotOpenRejected` and
`TestPlaceBet_SelectionNotActiveRejected`. `brand_id` was a write-only
field with no HTTP surface and no test assertion anywhere - added to
`adminBetResponse` (Back Office visibility, per this stage's own §9 ask)
and asserted against the known correct brand in the existing cross-tenant
test. `decimal_exponent` was populated but never asserted against a
known-correct value (EUR=2) - added.

**Two cheap correctness/quality fixes, found by security review:**
unknown `asset_code` on `POST /v1/me/sportsbook/bets` returned a bare 500
instead of the established `db.IsForeignKeyViolation` → 400 "unknown
asset code" pattern casino already uses - fixed for consistency. A test
comment claiming `rg.EvaluateEligibility` takes "its own row lock on
player_accounts" was wrong - it is a `pg_advisory_xact_lock` keyed on
`person_id`, not a row lock - corrected (the test's own logic was
already valid regardless).

**Two factually-incorrect doc comments found and corrected during the
architectural trace** (not defects in behavior, defects in what the code
claimed about itself): `internal/sportsbook/orchestrator.go`'s own
comment claimed "no jurisdiction-blocklist-style check is applied to a
sportsbook bet this stage, exactly as none is applied by any other
production code path today" - false: `internal/casino`'s `LaunchGame`
DOES call `jurisdiction.Resolve` + `evaluateJurisdictionBlocklist` against
`casino_games.jurisdiction_blocklist`, a real, tested, already-shipped
mechanism (though every blocklist row in this codebase is `'{}'` today,
so it is currently inert - no HDR-J item has been answered). Corrected,
and the real boundary formally documented in a new
`docs/decisions/0047-sportsbook-catalogue-jurisdiction-boundary-and-
cumulative-risk-deferral.md`. `internal/risk/cumulative.go`'s comment
still said "no internal/sportsbook exists," stale since Stage 6 - also
corrected.

**New ADR 0047** formally documents, without answering any human
decision or inventing policy: the six-then-seven distinct concepts
(player jurisdiction determination / licence ceiling / tenant and brand
operating-country policy / per-request risk operation availability /
sportsbook catalogue blocklist availability / asset-product eligibility),
which of these sportsbook actually wires today (only per-request
`risk.Evaluate`), the casino-parity gap (casino's blocklist mechanism is
real and admin-configurable TODAY via `PUT /v1/admin/casino/games`, even
though every blocklist is currently empty - a sharper asymmetry than "both
are equally inert by construction"), and the cumulative-risk disposition
(the exact future measurement shape is already specified in ADR 0038 §13;
wiring it later is one additive map entry, not a design question).
**Disposition for both: SAFE DEFERMENT — required before a second
jurisdiction or the first B2B tenant, NOT required before Stage 7.**

**Four deferred-item dispositions** (formal, per this stage's own ask):
1. Cumulative sportsbook risk rule — **SAFE DEFERMENT** (ADR 0047 §4).
2. Catalogue tenant/jurisdiction gating — **SAFE DEFERMENT, required
   before second jurisdiction/B2B** (ADR 0047 §2-3).
3. Casino/withdrawal concurrency-test convention backfill (a Stage 6 QA
   finding, unchanged) — **SAFE DEFERMENT**, a backlog item, no financial-
   integrity impact (those tests already pass, just via an older
   technique).
4. B2C build-time brand model before true multi-brand — **SAFE
   DEFERMENT, required before a second B2C brand goes live** (confirmed
   by architect review: `brand_slug` only ever selects which brand a
   login/register call resolves to server-side; every subsequent call is
   scoped purely from the verified JWT, never a client-supplied brand
   value - build-time config is a presentation mechanism, not an
   authorization seam, today).

Three additional SAFE-DEFERMENT items recorded in ADR 0047 §5 (a
cumulative-rule write-time validation gap in `internal/risk` generally,
not sportsbook-specific; no explicit `PrincipalType==player` assertion on
`/v1/me/sportsbook/bets`, consistent with casino's identical shape; a
non-`FOR SHARE` catalogue read, inert until a live-odds feed exists).

**Full validation gate re-run after every fix**: `go build`/`vet`/`gofmt`
clean; fresh 78-migration scratch database; full 31-package
`-tags=integration` suite green; the sportsbook concurrency and
idempotency-collision tests re-run 3x fresh under `-race`; `backoffice/`
(17 tests) and `b2c/` (19 tests, +1 from the idempotency-retry regression
test) both build and pass clean.

No automatic progression. Per the stage-gate rule, Stage 7 is NOT
authorized and was not started. No casino, settlement, cashout, real
provider integration, or country-approval work was performed.

---

## Prior stage: Stage 6 — B2C Player/Brand MVP + First Sportsbook Vertical Slice — COMPLETE, awaiting human review

**Purpose.** Ship the first real B2C player product and the first
functioning sportsbook vertical slice: a genuine end-to-end chain from
player registration through wallet, sportsbook browse, bet slip, bet
placement, bet history, and Back Office visibility. Every domain this
stage depends on (identity/auth, wallet/ledger, RG, risk, KYC, audit,
brand/tenant config, Back Office) was reused, not duplicated — dependency
discovery confirmed zero pre-existing sportsbook code beyond an
architecture doc, so a minimum mock-provider-backed vertical slice was
built rather than a "complete world-class engine."

**Backend — new `internal/sportsbook` package + 5 endpoints.**
`types.go`/`catalogue.go`/`mock.go`/`bets.go`/`orchestrator.go`: Sport →
Competition → Event → Market → Selection catalogue (synced from a
`Provider` interface at server startup, not baked into the migration, so
a real provider is a drop-in with zero schema change), a same-process
`MockSportsbookProvider`, and `PlaceBet` — validation → RG eligibility →
risk evaluation → wallet balance lock → ledger post (`TxSportsbookBet`,
Dr `player_cash` / Cr `player_locked_cash`) → bet row insert, all in one
transaction, following `internal/casino`'s orchestrator pattern exactly
(simpler here: no external provider round-trip, so a single synchronous
request/response, never "pending"). Odds are an integer numerator/
denominator fraction (never a float); potential_return computed via
`internal/money`'s `*big.Rat`/`*big.Int`. Bet-placement rejections are a
well-formed 200 response (`accepted:false` + one of
`odds_changed|event_not_open|insufficient_funds|rg_denied|risk_denied`),
mirroring casino's webhook-decline precedent. New permission
`sportsbook_bet:read` granted to tenant_admin/compliance/support/finance
(never platform_admin — `db.Pool.WithTenant` hard-errors on the nil
tenant ID platform_admin always carries). Migration `0078` adds the
schema only; `TxSportsbookBet` added to the ledger's transaction-type
CHECK constraint (17th value, diffed against the prior 16 to confirm none
dropped). Concurrency proven with this codebase's own deterministic
technique (uncommitted competing row + `pg_stat_activity` poll for
`wait_event_type='Lock'`, never a bare `WaitGroup`/`time.Sleep`) — the
implementing agent correctly caught and disclosed that the orchestrator's
own brief had pointed at the wrong (non-compliant) precedent packages
and used the genuinely-compliant one instead.

**Back Office — sportsbook bet visibility page, built directly (small,
well-scoped, pattern already known from Stage 5).** New `sportsbook` nav
permission (granted to the same four roles as the backend's
`sportsbook_bet:read`), `SportsbookBetsPage.tsx` mirroring
`WithdrawalQueuePage.tsx`'s exact structure, route wiring. Backend gained
a matching `decimal_exponent` field on the admin bet response (same
per-distinct-asset-code lookup pattern as Stage 5's withdrawal admin
fix), so stakes/returns render as real decimals, not raw minor units.

**Frontend — new `b2c/` app, built from scratch by a dedicated frontend
agent.** Same architecture precedent as `backoffice/` (Vite + React +
TypeScript + React Router + TanStack Query + Tailwind, strict
api/components/features layering), correctly starting from the Stage 5
Back Office's ALREADY-FIXED patterns rather than reintroducing old bugs:
single-flight `refreshSessionOnce()` used on session bootstrap from day
one (never the raw refresh call), logout revokes the server-side session
best-effort before clearing local state. Brand-awareness via build-time
Vite env vars read from one `config/brand.ts` module (a full multi-brand
runtime theming engine was explicitly not required this stage). Ships
app shell/auth, sportsbook browse (sports→competitions→events, event
detail with markets/selections), a singles-only bet slip (stake entry,
a clearly-labeled non-authoritative estimate, and full three-way outcome
handling — 201 accepted always renders the server's own `bet.
potential_return`/odds fields, 200 not-accepted renders each of the 5
rejection categories distinctly with `odds_changed` live-refetching the
new price, and genuine HTTP/network errors rendered distinctly from a
rejection), paginated bet history, account page (wallet balances,
deposit). Explicitly deferred (per "no uncontrolled scope expansion", not
oversight): casino UI, B2B console, runtime multi-brand theme switching,
settlement/cashout UI, accumulator bets, live odds, withdrawal UI, RG
limit/self-exclusion UI. 18 vitest tests (13 original + 5 added in the
fix round below), `npm run build` clean — both independently re-verified
by me, not just trusted from the implementing agent's report.

**The defining acceptance test for Stage 6.** A single new Go integration
test, `internal/httpserver/stage6_b2c_sportsbook_acceptance_test.go`,
chains the full real HTTP API path a browser client would call: register
→ activate → a genuine deposit through the mock PSP with an actually
HMAC-signed webhook callback (not a shortcut) → wallet balance check →
catalogue browse → event/selection detail (bet slip data read from the
server's own response) → bet placement using the server-returned odds →
bet history check → wallet debit check → Back Office admin visibility
(tenant_admin sees player/selection/asset/stake/status/timestamp) →
audit-trail visibility (`sportsbook_bet.placed`, outcome `success`,
targeting the bet). Every assertion checks exact field values at each
hop (confirmed non-vacuous by the QA review below), not "a row exists."
Passed on first run against a freshly-migrated scratch database; the full
`internal/httpserver` suite (all pre-existing tests too) stayed green
after adding it.

**Independent review — architect, security, qa, per this stage's own
efficiency rule (no broad multi-agent review of frozen architecture).**

- **Architect: one P0, two P1s, two P2s — all fixed.** P0: the B2C
  frontend's `lib/odds.ts` computed decimal odds as `1 +
  numerator/denominator` and potential-return as `stake ×
  (denominator+numerator)/denominator`, but the server's `odds_numerator/
  odds_denominator` ratio IS the decimal odds value directly (already
  including the stake — `internal/sportsbook/orchestrator.go`'s
  `computePotentialReturn` is `stake × numerator/denominator`, no "+1").
  Every price and return shown in the B2C app — including the server's
  own authoritative placed-bet fields re-rendered through the same
  formatter — was overstated by the size of the stake itself (e.g. 2.50
  odds displayed as 3.50). **Fixed**: `formatDecimalOdds`/
  `estimatePotentialReturnMinorUnits` corrected to match the server
  exactly, the OpenAPI `Selection` schema's description clarified to
  state the ratio IS the decimal odds, a new `lib/odds.test.ts` (5 tests)
  added as a permanent regression guard, and the one existing `BetSlip`
  test whose fixture asserted the old (wrong) decimal value corrected.
  P1: the sportsbook bet idempotency key was scoped only by
  `(tenant_id, idempotency_key)` — unlike every other player-facing
  financial idempotency key on the platform (`deposit_intents`,
  `withdrawal_requests`, both `(tenant_id, player_account_id,
  idempotency_key)`) — so one player supplying a key another player had
  already used in the same tenant would silently receive the OTHER
  player's bet back as an `accepted:true` 201, with their own stake never
  charged (a cross-player financial-data disclosure, independently
  confirmed by the security review below). **Fixed**: the migration's
  unique constraint, the lookup, and the ledger's own idempotency-key
  namespace (previously the raw, player-chosen string, now prefixed with
  `player_account_id` so it can never collide across players in the same
  tenant's ledger) all rescoped to include `player_account_id`; a mismatch
  on a genuine key collision now surfaces a new `ErrBetIdempotencyKeyReused`
  (mirroring `withdrawal`/`payments`'s identical precedent) rather than
  silently returning the wrong bet; `insertBet` rewritten to use
  `db.IdempotentInsert` (the same DB-unique-constraint-is-the-real-
  enforcement pattern `withdrawal.RequestWithdrawal` uses) as the actual
  concurrent-race backstop, not just the pre-check; a new regression test,
  `TestPlaceBet_SameIdempotencyKeyDifferentPlayersNeverCollide`, proves
  two players sharing a key get two independent bets and neither
  balance is affected by the other's stake. Two P2s fixed: the OpenAPI
  spec's `EventSummary`/`EventDetail`/`MarketDetail`/`Selection` status
  enums didn't match the real Go enums (my own transcription error when
  first writing the spec) — corrected to `scheduled|live|finished|
  cancelled`, `open|suspended|closed`, and `active|suspended`
  respectively; the B2C `lib/money.ts`'s `toMinorUnits` silently guessed
  exponent 2 for an unrecognized asset code on the OUTBOUND (request)
  path — changed to refuse (return `null`, which every caller already
  handles) rather than risk submitting a silently-wrong amount. One P2
  deferred (recorded, not fixed — genuinely needs `risk`/`ledger-finance`
  design work, not a Stage 6 blocker): `internal/risk`'s cumulative-rule
  spec has no `OperationSportsbookBet` entry yet, so no cumulative
  stake/velocity cap constrains sportsbook bets beyond the per-request
  `risk.Evaluate` call this stage already wires. P4s recorded: the public
  catalogue routes carry no tenant/brand/jurisdiction gating (must be
  addressed before any second tenant or jurisdiction — doc 09 §5's own
  open decision); the B2C app's one-build-per-brand env-var model needs
  runtime host→brand resolution before true multi-brand B2C.
- **Security: one P1 (the same cross-player idempotency-key finding,
  independently discovered) — fixed as above.** Everything else verified
  sound: cross-player bet forgery is structurally impossible (player
  identity always resolved server-side from the authenticated session,
  never a request field); the cross-tenant Back Office visibility test is
  genuinely non-vacuous (two real bets placed in two tenants, asserted
  `total==1` and the visible row belongs to the right player); the public
  catalogue routes are safe (no PII, no tenant scoping, read-only,
  `WithoutTenant` leaves any RLS table failing closed); RG/risk denials
  and unrecognized risk outcomes fail closed (Go error → transaction
  rollback → 500, never an accepted bet); the audit trail is written
  in-transaction for every outcome (acceptance, RG denial, risk denial,
  insufficient funds); the orchestrator's own `DecimalExponent` addition
  to the admin response introduces no new data exposure. One P3 fixed: a
  stale test comment claiming "support is deliberately NOT" granted
  `sportsbook_bet:read" when the permission table actually does grant it
  to `RoleSupport` — corrected. Sessionstorage refresh-token tradeoff
  reconfirmed as disclosed, not a Stage 6 blocker.
- **QA: clean sign-off, no P0/P1.** Independently verified the defining
  acceptance test's assertions are genuinely non-vacuous at every hop;
  confirmed idempotency is DB-enforced (not check-then-insert) and
  concurrency uses the mandated deterministic technique with exact
  balance/row-count assertions; confirmed the `backoffice/nav.test.ts`
  change (finance's nav items growing from `['Withdrawals']` to
  `['Withdrawals','Sportsbook']`) is a legitimate reflection of a real,
  documented backend permission grant, not a weakened assertion. One P4
  recorded: `internal/casino`/`internal/withdrawal`'s own concurrency
  tests still use the older bare-`WaitGroup` technique instead of this
  stage's (and `internal/operatingmarket`/`internal/jurisdiction`'s)
  `pg_stat_activity` convention — a backlog item, not a Stage 6 blocker.

**Production wiring status:** none beyond this stage's own scope. No
casino UI, B2B partner console, or retail surface was started. Jurisdiction
(`HDR-J-6/7/8/9`/`HDR-M-1/2`) was correctly NOT reopened — no jurisdiction
check applies to a sportsbook bet this stage, exactly as none applies to
any other production code path today. Bonus-funded sportsbook stakes
remain platform-wide BLOCKED per `docs/decisions/0038` §9 (unchanged).
`PLAT-ROLESPLIT-1` remains the production deployment blocker, unaddressed
this stage per its own documented instruction.

No automatic progression. Per the stage-gate rule, Stage 7 is NOT
authorized and was not started.

---

## Prior stage: Stage 5 — Operator Back Office MVP — COMPLETE, awaiting human review

**Purpose.** Build the first genuinely usable Operator Back Office: a
staff-facing web application that is a CLIENT of the Platform API (never
authoritative for financial/authorization/KYC/RG/bonus/jurisdiction
decisions), plus the additive read/query API surface it needed. Stage 4I
and every other domain (KYC, RG, bonus, wallet/ledger, payments,
sportsbook, casino, operating-market) is FROZEN — none was reopened or
redesigned.

**Backend — additive query surface, four parallel implementers, zero file
conflicts.** New shared pagination helper (`internal/httpserver/
pagination.go`, `{items,limit,offset,total}`, reused by every new
endpoint). `backend`: `GET /v1/admin/tenants` (+detail), `GET
/v1/admin/tenants/{id}/brands` (+detail), paginated/searchable `GET
/v1/admin/players`, new `POST /v1/admin/players/{id}/reinstate`
(atomic-conditional `suspended→active` only — deliberately cannot clear
`self_excluded`/`identity_review_required`), new platform-scoped `GET
/v1/admin/platform/audit-log`, paginated/filterable `GET
/v1/admin/audit-log`. `identity-compliance`: tenant-wide `GET
/v1/admin/kyc/cases`; `GET /v1/admin/rg/restrictions` extended with a
tenant-wide mode when `player_account_id` is omitted — the implementing
agent found and fixed a real cross-tenant PII leak in its own adversarial
testing before shipping (a naive query surfaced every platform-wide
self-exclusion row to any tenant's staff; fixed with an `INNER JOIN
player_accounts`). `bonus-engine`: `GET /v1/admin/bonus/{campaigns,
change-requests,grants}` (read-only, reusing the existing `.../decide`
mutation). `backend` (second instance, disjoint files): `GET
/v1/admin/withdrawals/history` + `GET /v1/admin/withdrawals/{id}`
(pure reads, no state-transition side effect, registered at `/history`
because `/v1/admin/withdrawals` was already bound to the frozen
pending-only queue). My own combined-repo verification after all four
landed: `go build`/`go vet`/`gofmt -l` clean, full 30-package
`-tags=integration` suite green against a freshly built scratch database.

**Frontend — new `backoffice/` app, built from scratch (zero prior
frontend code existed anywhere in this repository).** Vite + React 18 +
TypeScript + React Router + TanStack Query + Tailwind, chosen so a future
visual redesign only touches `components/`/`layout/`/Tailwind tokens,
never `api/` or domain logic. Hard layering enforced: `api/` is the only
module allowed to call `fetch`; `components/` are domain-free primitives
(one generic `Table`/`Pagination` reused by every list page); `features/*`
holds per-domain logic; client-side permission/nav gating is explicitly
non-authoritative (labeled as such in code) — every mutation calls the
real server endpoint and renders its real response. Correctly models the
real backend constraint that `db.Pool.WithTenant` errors on a nil tenant
ID: platform_admin sees only Tenants + Platform Audit Log (no fake "view
as tenant" capability), tenant-scoped staff see Players/KYC/RG/Bonus/
Withdrawals/Audit scoped automatically to their own tenant. Pages: app
shell/login/nav; Tenants (list/detail/brands, read-only); Players
(list/detail composing KYC/RG/bonus sub-sections, Suspend/Reinstate with
required reason); KYC case queue + detail; RG tenant-wide queue +
per-player restrictions; Bonus campaigns/change-request approval
queue/grants; Withdrawals (priority workflow: full history queue,
detail, Approve/Reject wired to the existing frozen endpoints with
mandatory confirmation and reason capture); one reusable audit-log viewer
used for both the tenant and platform-scoped audit endpoints. `npm run
build`/`npm test` (17 tests, 6 files) both verified independently by me,
not just trusted from the implementing agent's report.

**Independent review — architect, security, qa, and a narrowly-scoped
ledger-finance review of the one page that moves money (the withdrawal
approve/reject flow), per this stage's own efficiency rule (no six-way
dispatch for routine work).** Architect: PASS, no P0/P1/P2 (two P3s on a
minor RG-response-shape inconsistency and theming-token completeness,
recorded not fixed). QA: full validation gate green (1492 Go tests, 17
frontend tests, `-race` clean on withdrawal/identity), test-coverage
judgment confirmed every new endpoint has real authorized/unauthorized/
cross-tenant tests. Ledger-finance: SIGN-OFF on the withdrawal UI as a
genuine pass-through with no client-side financial logic; one real P2
(amounts rendered in raw minor units with no exponent formatting at an
irreversible approval decision — **fixed**: new `decimal_exponent` field
on the two new withdrawal admin endpoints, a shared frontend money
formatter). Security found the review round's most consequential
results, two P2s **both fixed**: (1) the SPA's "Sign out" only cleared
local state and never revoked the session server-side, leaving a stolen
refresh token valid for the full 30-day TTL with no way for the user to
stop it — fixed, logout now calls the existing `POST /v1/auth/logout`
best-effort before clearing local state; (2) the RG cross-tenant leak's
own regression-test guard was itself vacuous — it seeded a
tenant-scoped restriction the pre-existing RLS policy already filtered
on its own, and tenant B had no player at all, so the test passed even
with the join-based fix's protection reverted (verified by mutation
testing: weakening the join to a `LEFT JOIN` made the OLD test still
pass, and makes the REWRITTEN test correctly fail) — fixed by rewriting
the test to seed a genuinely platform-wide self-exclusion via the
player's own self-exclusion endpoint, verify the fixture's `tenant_id IS
NULL` directly, and give tenant B its own real restriction so the
isolation assertion is non-vacuous. Security also found and I fixed a
related P3: the SPA's session-bootstrap-on-reload called the raw refresh
function directly instead of through the existing single-flight
`refreshSessionOnce()` dedupe, so React 18 StrictMode's deliberate
double-effect-invocation could present the same refresh token twice,
which the backend correctly treats as token reuse and revokes the whole
session chain — fixed by routing bootstrap through the same dedup path
`apiFetch`'s own 401 handling already uses.

**Deferred, not fixed this stage (recorded, not blocking):** refresh
token in `sessionStorage` (XSS-readable) combined with a 30-day TTL is a
documented MVP tradeoff — security flagged production hardening
(httpOnly/Secure/SameSite cookies, requires backend changes) as
**launch-blocking for the Back Office specifically**, tracked alongside
`PLAT-ROLESPLIT-1`, not resolved here; the 401-refresh-retry logic itself
has zero frontend test coverage despite being the most security-sensitive
module in the SPA; `tenants`/`brands` read isolation is handler-only with
no RLS backstop (correct as written, but should not be assumed inherited
by a future handler); RG tenant-wide queue rows carry no player
attribution (a real functional gap per the architect review — the queue
can't say whose restriction it is — deferred as a fast-follow, not fixed
this stage); `GET /v1/admin/rg/restrictions`'s two response shapes (bare
array vs. paginated envelope depending on a query param) is the one
inconsistency with the otherwise-uniform Stage 5 convention; `amount` is
a JSON number rather than the string convention `bonus.ts` already uses,
a latent precision hazard only once crypto-asset withdrawals (exponent
8/18) reach this endpoint.

**Production wiring status:** none. No B2C frontend, sportsbook, casino,
or partner console was started. No jurisdiction/operating-market content
or wiring was touched — `HDR-J-6/7/8/9`/`HDR-M-1/2` remain exactly as
Stage 4I left them. `PLAT-ROLESPLIT-1` remains the production deployment
blocker, documented in `docs/security/runtime-role-separation.md`, not
addressed this stage (development continues without it per that
document's own instruction).

No automatic progression. Per the stage-gate rule, Stage 6 (B2C Player/
Brand MVP) is NOT authorized and was not started.

---

## Prior stage: Stage 4I Exit Triage — Exit Register and Production Integration Readiness — COMPLETE, awaiting human review

**Purpose.** Per an explicit human directive changing execution strategy:
stop opening further jurisdiction/KYC/security architecture review stages
and instead perform a final Stage 4I exit/triage pass, closing out every
open item with a concrete disposition, and identify the next concrete
product/platform implementation stage. **Player-jurisdiction, licensing,
operating-market, KYC, bonus, sportsbook, wallet/ledger, payments, and RG
architecture are all explicitly FROZEN this stage** — none was reopened;
no concrete implementation dependency was found that required it.

**What this stage built.** Two new documents, zero Go code, zero
migrations:

1. `docs/governance/stage-4i-exit-register.md` — classifies every open
   Stage 4I item (`PLAT-ROLESPLIT-1`, `PLAT-TENANTREAD-1`,
   `MKT-LICSTATUS-1`, `MKT-AUDIT-1`, `MKT-DORMANT-1`, `MKT-DUAL-1`,
   `MKT-EXPIRY-1`, `MKT-PM-1`, `HDR-J-6/7/8/9`, `HDR-M-1/2`) as A
   (production blocker) / B (next-feature blocker) / C (human decision,
   not urgent) / D (deferred, safe) / E (technical debt), each with
   owner, concrete consequence, exact reopening trigger, and whether it
   blocks production or the next stage. Net result: **exactly one
   production blocker** (`PLAT-ROLESPLIT-1`), and **nothing blocks the
   recommended next stage**. Also includes a fail-closed "integration
   contract" table (informational only, zero wiring performed) stating
   which future call sites will need player-jurisdiction/licence-
   validity/operating-country-policy checks, and that every one of them
   is fail-closed on an unresolved result by construction already.

2. `docs/security/runtime-role-separation.md` — the precise,
   implementation-ready fix for `PLAT-ROLESPLIT-1`. Root cause: the
   application's Postgres role (`igaming`) owns every table it migrated,
   and PostgreSQL RLS never applies to a table's owner (not `CREATEROLE`
   — ownership is the load-bearing fact). Fix: a second, non-owning
   `igaming_runtime` role with only `SELECT/INSERT/UPDATE/DELETE`, used
   for runtime traffic, while `igaming` remains the migration-owner
   credential used only at deploy time. **Empirically verified this
   stage** against the local dev Postgres (not production): created a
   temporary role with exactly the proposed grants and confirmed it
   cannot disable RLS, disable triggers, `TRUNCATE`, `DROP`, `ALTER
   TABLE`, or `CREATE TABLE`, while ordinary CRUD still works — the exact
   attack chain the Phase E-SECURITY adversarial security reviewer used
   against the current single-role setup, now refused. **Classified
   `PRODUCTION BLOCKER — EXTERNAL INFRASTRUCTURE ACTION`** — closing it
   requires an operator with Postgres `CREATEROLE` access to run a
   four-statement `GRANT` script against each real environment, which
   this session cannot do (no production credential; CLAUDE.md's
   Environment Safety rule forbids requesting one).

**Independent review.** Security, architect, and qa reviewed both
documents (minimum-review set per this stage's own efficiency rule — no
six-way review dispatched for a documentation-only pass). The security
review's reproduction of the role-separation verification (plus 20
additional escalation probes, all denied) surfaced its most consequential
finding: this session's own local development database had drifted from
the committed migration files (a stale `tenants_read` policy, ten failing
`internal/operatingmarket` tests) — root-caused to this database having
migration 0077 applied before that file's later in-place amendments
landed (the `MKT-MIG76-1` hazard, recurring against the orchestrator's own
environment, not a defect in the committed code). Fixed by rebuilding the
database fresh; a first-time deployment is not exposed to this specific
drift, but the underlying tooling gap (`cmd/migrate` has no live-schema-
vs-file-content verification) is now tracked as new item
`PLAT-MIGDRIFT-1` (E, non-blocking). See `docs/governance/task-
registry.md`'s "Stage 4I Exit Triage" section for the full findings and
fixes from all three reviews.

**Recommended next stage (not authorized, not started): Operator
Back-Office MVP.** Requires **zero** unresolved HDR/policy decisions and
**zero** jurisdiction/licensing/operating-market wiring — that dependency
claim is confirmed. **Corrected per the independent architect review's
verification (an earlier draft of this entry overstated readiness):**
this is NOT simply "a UI over already-finished APIs." Withdrawal approval
is genuinely UI-ready as-is; every other named capability needs new
backend query/list surface first: there is no tenant-wide KYC pending-
case query (`kyc_admin_handlers.go` only looks up one already-known
player account), no tenant-wide RG restriction list (same shape), zero
`GET` routes anywhere under bonus admin (campaigns/offers/change-request
approval queue are write-only over HTTP today), zero `ListTenants`/brand-
list routes, a hardcoded `LIMIT 50` with no pagination/search on the
player list and no suspension-reversal endpoint, and the existing
platform-scoped audit reads are unreachable over HTTP (the one route
filters `WHERE tenant_id = $1`, which is exactly where migration 0077
now places `AssignTenantLicence`/`tenant.created`/operating-market audit
rows). No pagination convention exists anywhere in the API today. The
stage must therefore be scoped as **"back-office read/query API surface
+ UI,"** with roughly half its named capabilities needing new domain
query functions and endpoints (each with its own tests) before any
screen can consume them — not assumed to be UI-only. The repository has
no frontend or back-office code of any kind today (confirmed via `find`),
and this was already named as a candidate ("Stage 6A") in this file's own
prior-stage entries; the dependency-readiness conclusion (no policy
blocker, no frozen-architecture reopening needed) still holds even though
the effort estimate was corrected. See this stage's completion report for
the full, corrected dependency-chain rationale. **Not authorized to
start.**

---

## Prior stage: Stage 4I Phase E-SECURITY — Tenant/Licence/Jurisdiction Registry RLS Hardening — COMPLETE, awaiting human review

**Purpose.** Close `MKT-SCOPE-1`/`MKT-SCOPE-1(b)` (task registry, opened
during Phase E's own fix round): `tenants`, `licences`, and
`jurisdictions` had never had row-level security applied, in any
migration, since the platform's earliest schema. The architect
live-reproduced a full attack chain exploiting this: an ordinary
tenant-scoped connection repointing its own `tenants.licence_id` at
another tenant's BYOL licence and committing an `enabled` tenant-rung
operating-market policy for a country its own real licence never
permitted — plus two previously-unrecorded attacks (the composite FK
`tenants_licence_matches_model` defeated by changing `licensing_model`
and `licence_id` together in one UPDATE, since `expected_licensee` is a
`GENERATED` column recomputed from the new value in the same statement;
and an ordinary tenant-scoped `DELETE FROM tenants` cascading away a
DIFFERENT tenant's entire `operating_country_policies` set, reachable at
a strictly lower bar than ADR 0045 §18 finding F2 originally disclosed).
Full ruling: `docs/decisions/0046-tenant-licence-registry-rls.md`.

**What this phase built.** Migration `0077_tenant_licence_registry_rls`:
`ENABLE`+`FORCE ROW LEVEL SECURITY` on all three tables, asymmetric by
table — `tenants`/`jurisdictions` read-open (`USING (true)`, migration
0044's `assets` precedent: `identity.GetTenantBySlug`, three
`WithoutTenant` active-tenant sweeps in `internal/rg`/
`internal/reconciliation`/`internal/bonus`, and migration 0076's own
ceiling checks all read `tenants` from scopes with no tenant match to
offer), `licences` narrowed to platform-admin or the tenant whose own
`tenants.licence_id` names the row (ADR 0045 §4's BYOL precedent, applied
one level down); every write on all three restricted to a genuinely
platform-admin-scoped transaction, with `tenants` alone additionally
getting a platform-admin-only DELETE policy (the one legitimate DELETE on
that table, preserving both integration-test teardown and the
`ON DELETE CASCADE` migration 0076 §7.3 relies on). A new partial unique
index, `uq_tenants_exclusive_own_licence`, closes an independently
live-verified BYOL exclusivity gap (two tenants binding the same
`licensee='tenant'` licence) — keyed on the `GENERATED` `expected_licensee`
column, deliberately not constraining shared `licensee='platform'`
licences (ADR 0006).

Go changes: `internal/identity.CreateTenant` gained an
`assertPlatformScope` first statement (new sentinel
`ErrPlatformTransactionScope`, mirroring `internal/jurisdiction`'s
function of the same name byte-for-byte); `internal/jurisdiction.
AssignTenantLicence`'s contract moved from `db.Pool.WithTenant` to
`db.Pool.WithPlatformAdmin`, with its audit write moving from
tenant-scoped to platform-scoped as a direct consequence; `CreateJurisdiction`/
`CreateLicence`/`ListJurisdictions`/`ListLicences` each gained the
identical `assertPlatformScope` gate `SetJurisdictionCountryCode` already
had from Phase E's own fix round; `internal/httpserver.
newCreateTenantHandler` moved from `WithoutTenant` to `WithPlatformAdmin`,
gained a required `reason_code` field and a before/after audit shape
(closing the pre-existing `tenant.created` audit gap), and now surfaces a
`uuid.Parse(tc.Subject)` failure explicitly instead of silently
swallowing it. `internal/operatingmarket` received **zero executable
diff** — one comment-only correction in `resolve.go` explaining that
`assertTenantScope` remains load-bearing (read-side isolation on
`tenants` is still deliberately open) even though write-side forgery is
now closed. `internal/operatingmarket`'s own schema, triggers, RLS
policies, and resolution algorithm are unchanged.

**Task dispositions.** Fixed now: BYOL licence exclusivity (above).
Deferred, each recorded as its own task-registry item: `MKT-DUAL-1`
(dual control, unchanged — this phase only confirms the foundation is
*capable* of supporting it later), `PLAT-ROLESPLIT-1` (new — the
migration-owner/runtime-role split; genuinely blocked on infrastructure
this repository cannot provide, since provisioning a second Postgres role
requires `CREATEROLE`, which the application role lacks, verified live;
owner `security`; pre-production gate, not a gate on this phase),
`MKT-LICSTATUS-1` (new — no sanctioned write path for `licences.status`;
owner `architect`; gated on the first real `licence_country_ceilings`
row), `MKT-AUDIT-1` (new — `AssignTenantLicence`'s audit row is now
platform-scoped only, so the affected tenant cannot read its own
licence-assignment history back; owner `security`; not yet needed, no
consumer exists). Not touched: `EvaluateLicenceValidity` (already
correctly fail-closed — RLS-invisibility of a foreign licence now maps to
the same `LicenceNotFound` a genuinely-absent licence produces, proven by
a new regression test), `licence_country_ceilings`/
`operating_country_policies` schema/triggers/RLS (zero changes, confirmed
via `git diff`).

**Mechanical fixture migration.** Every existing integration test fixture
across the repository that seeded `tenants`/`licences`/`jurisdictions`
under a scopeless (`WithoutTenant`) connection was moved to
`WithPlatformAdmin` (~30 files across `internal/{rg,auth,risk,
reconciliation,audit,jurisdiction,assetregistry,wallet,ledger,economicop,
withdrawal,kyc,casino,bonus,payments,db,idempotency,operatingmarket,
identity,identityresolution,httpserver}`), with rows-affected checked
explicitly on every touched write (a denied RLS write is a silent
zero-row no-op, not an error — the exact failure mode the architect's own
reproduction found in `TestResolveOperatingCountryPolicy_
NotYetIssuedLicenceYieldsNotPermittedByLicence`'s `UPDATE licences SET
issued_at = ...` fixture, fixed here and applied as a discipline
project-wide). Several migration-mechanics tests that assumed migration
0076 was "the chain's tip" (rolling back a fixed step count and asserting
specific version numbers) were updated to account for migration 0077
landing on top of it, mirroring this workstream's own established
"account for every migration that lands after mine" precedent
(`internal/bonus/wave3_phase2_migrations_integration_test.go`'s own
`migration007XVersion` chain).

**New tests.** `internal/jurisdiction/migration_0077_integration_test.go`
(4 tests, mirroring the `TestMigration0076_*` pattern, including a
migration-safety proof that re-applying migration 0077 against data
violating `uq_tenants_exclusive_own_licence` fails cleanly and leaves no
partial schema behind) and `internal/jurisdiction/
registry_rls_integration_test.go` (18 tests covering every RLS posture
claim above, including the crux regression
`TestTenantsRLS_TenantScopedConnectionCannotUpdateOwnLicenceID` and the
exact composite-FK-defeating shape
`TestTenantsRLS_TenantScopedConnectionCannotDefeatCompositeFKByChangingLicensingModel`),
plus three new tests in `internal/operatingmarket/rls_integration_test.go`
reproducing the full end-to-end attack
(`TestOperatingCountryPolicy_TenantCannotEnableACountryByRepointingItsOwnLicence`)
and proving it is now refused. `internal/jurisdiction/
tenant_licence_admin_integration_test.go` and `internal/jurisdiction/
licence_validity_integration_test.go` were substantially updated for the
new platform-admin-scoped contract and a new
`TestEvaluateLicenceValidity_ForeignLicenceIsInvisibleAndFailsClosedNotOpen`
regression respectively.

**Validation.** Full gate run against a FRESH scratch database built via
`cmd/migrate up` from the current `migrations/` directory (never a reused
local database): `go build ./...`, `go vet ./...`,
`go vet -tags=integration ./...`, `gofmt -l .` all clean; RLS state
verified directly against `pg_class`/`pg_policies`/`pg_indexes` before
trusting any test result; focused packages
(`internal/jurisdiction` 154 tests, `internal/identity` 28,
`internal/operatingmarket` 80, `internal/httpserver` 173) all pass; 10
consecutive `-race` runs of the concurrency-relevant tests
(`TestAssignTenantLicence_ConcurrentAssignmentsSerializeCleanly`,
`TestOperatingCountryPolicy_ConcurrentCloseCannotBeRescuedByAnotherTransactionsSuccessor`,
`TestOperatingCountryPolicy_ConcurrentCreatesNeverCorruptState`) all pass;
the FULL whole-repo `go test -tags=integration ./... -count=1` passes
with zero failures. One additional fixture gap was found and fixed only
by actually running the suite (not by static review):
`internal/identityresolution/register_integration_test.go`'s own
`createTestTenant` helper called `identity.CreateTenant` under
`WithoutTenant` and was not caught by the initial `INSERT INTO tenants`
text search.

**SESSION-START NOTE — migration 0077 was amended in place twice** (once
to add deny-TRUNCATE triggers to all three tables, once to narrow
`tenants_read` to exclude player scope), following this workstream's own
established `MKT-MIG76-1` convention for an unreleased migration. Its
version number never changed. Before trusting any `internal/jurisdiction`
RLS test result, rebuild the target database or confirm directly against
`pg_trigger`/`pg_policies` rather than assuming — `TestMigration0077_
SchemaMatchesTheCurrentMigrationFile` and `TestMigration0077_
PerCommandPoliciesNoForAllAndNoLicenceDelete` (the latter now asserting
the exact 10-tuple policy whitelist) are the standing regression guards.
This note expires when migration 0077 is released.

**FIX ROUND (post-independent-review) — RESOLVED.** Six independent
reviews (architect fidelity, adversarial security, DB/RLS, compliance/
privacy, QA regression, code review) ran against the implementation
above. No P0/P1 findings, but strong convergence: the new
`uq_tenants_exclusive_own_licence` violation's unmapped SQLSTATE 23505
(surfacing as an opaque HTTP 500) was found independently by **four**
reviewers; `tenants_read`'s read posture being broader than its own
stated justification (open to player scope, unlike every sibling policy
in this family) was found independently by **three**; a test-fixture
cleanup (`DELETE FROM jurisdictions`) that had always been a silent
zero-row no-op, with a comment added during this workstream incorrectly
asserting it worked, was found independently by **two** (measured impact:
649 leaked rows after one whole-repo test run). All fixed:

- **`AssignTenantLicence`** now maps the new unique-constraint violation
  to `ErrInvalidInput` (400/409), mirroring the pre-existing 23503
  mapping; the test that had pinned the raw unmapped `*pgconn.PgError` as
  expected behaviour now asserts the proper mapping instead.
- **The dead `jurisdictions` DELETE fixture** was removed along with its
  incorrect comment; `jurisdictions`/`licences` rows are now documented
  as intentionally permanent test residue (neither table has a DELETE
  policy, by design).
- **`docs/security/security-architecture.md`** — never updated by the
  original implementation despite ADR 0046 claiming architecture docs
  were corrected — was found to still assert the exact opposite of
  reality ("`tenants`/`licences`/`jurisdictions` carry no RLS... they
  stay that way... the permission check is the entire control, there is
  no database backstop"). Corrected; flagged by the adversarial reviewer
  as "the finding I would hold the done label on," since an engineer
  trusting the stale doc could reintroduce the exact defect this phase
  closes.
- **Deny-TRUNCATE triggers added** to all three tables (migration 0077,
  amended in place) — `tenants`/`licences`/`jurisdictions` were the only
  tables in this subsystem without one; RLS does not govern `TRUNCATE` at
  all, so an ordinary tenant-scoped `TRUNCATE tenants CASCADE` was
  previously blocked only by an unrelated table's append-only trigger
  further down the cascade — an accident of the current FK graph, not a
  real control.
- **`tenants_read` narrowed** to exclude player scope (migration 0077,
  amended in place a second time), matching every sibling read policy in
  this family. Deliberately NOT narrowed further — full tenant-to-tenant
  enumeration of name/slug/licensing_model/status/licence_id remains
  open, tracked as new item `PLAT-TENANTREAD-1` (owner `architect`/
  `security`, revisit once real B2B tenant onboarding is planned) rather
  than fixed here, since it involves a real design tradeoff, not a bug.
  `jurisdictions_read` deliberately left fully open (public regulatory
  reference data, required by the HDR-J-5 player-jurisdiction path).
- **`jurisdiction.assertTenantScope`** gained the player-scope rejection
  `internal/operatingmarket`'s equivalent already had (unreachable today,
  since both production callers of `jurisdiction.Resolve` always pass a
  non-nil `PlayerAccountID`, but now fails with a diagnosable scope error
  instead of a misleading `dependency_unavailable` if that ever changes).
- **`TestAssignTenantLicence_ConcurrentAssignmentsSerializeCleanly`
  rewritten** — it had used a bare `sync.WaitGroup` with no real
  synchronization barrier, in direct violation of this codebase's own
  binding rule ("MANDATORY, non-negotiable... NEVER a `sync.WaitGroup`
  barrier... this exact mistake cost Stage 4I Phase D two fix rounds").
  It also could not reliably prove concurrent execution occurred, and its
  chain-ordering assertion (sorting by `audit_log.created_at`, i.e.
  transaction *start* time) was independently found to fail intermittently
  under CPU contention, since a later-started transaction can still win
  the lock race and commit first. Rewritten using the established
  deterministic uncommitted-competing-row + `pg_stat_activity`-poll
  technique, with the winner now determined by actual outcome rather than
  wall-clock inference. A new companion test covers the additional
  concurrency hazard the BYOL-exclusivity index itself introduces (two
  concurrent binds of the same licence to different tenants), which had
  zero prior coverage.
- **`resolver.go`/`resolve.go` comments corrected again** — the previous
  round's own corrections had swung too far the other way, describing a
  cross-tenant leak as preventable only by the Go-level assertion, when
  the new narrow `licences_read` policy independently blocks the same
  leak at the database layer. Both now accurately state the assertion
  remains necessary (for diagnosability) and is now backed by a second,
  independent database-level control.
- **New task-registry items**: `MKT-DORMANT-1` (a licence ceiling
  contraction-then-re-expansion silently resumes a dormant tenant-rung
  policy with no new authorization event — Phase E resolver semantics,
  not a Phase E-SECURITY defect; owner `architect`, not fixed here) and
  `PLAT-TENANTREAD-1` (above). `MKT-AUDIT-1`'s gate widened to also
  trigger on first B2B tenant onboarding, not only on a partner-console
  surface existing.
- Stray adversarial-review scratch artifacts (`cmd/zzsecprobe`, a
  compiled binary) removed from the working tree before commit; the
  QA-authored `qa_adversarial_registry_rls_integration_test.go` (4
  genuine, passing tests) kept, per this workstream's established
  practice of retaining review-round adversarial tests.
- Full validation gate re-run clean, independently, multiple times
  against freshly-built scratch databases (three consecutive whole-repo
  runs by the implementing agent; one further independent run by the
  orchestrator directly, not only trusted from agent self-report):
  `go build`/`go vet` (both tags)/`gofmt -l`, focused packages, 10+
  consecutive `-race` runs of every concurrency test including the two
  rewritten/new ones, and the whole-repo `go test -tags=integration
  ./... -count=1` gate — all green.

**One item explicitly escalated to the human, not resolved in this
phase**: the adversarial security review demonstrated that because the
application's runtime database role also owns every table (no migration-
owner/runtime-role separation — `PLAT-ROLESPLIT-1`, genuinely blocked on
missing `CREATEROLE`), an ordinary tenant-scoped connection that can issue
DDL can disable RLS entirely and, in the worst case demonstrated, wipe the
tenant registry, the ledger, and the audit log via a single `TRUNCATE ...
CASCADE` after disabling triggers repo-wide. This is pre-existing (not
introduced by this phase) and was already tracked, but the reviewer
explicitly flagged it as launch-blocking rather than a routine deferral —
**this session is relaying that recommendation to the human directly,
not downgrading it.** `PLAT-ROLESPLIT-1`'s gate: before first production
deployment against real tenant data, or before the first
`RoleTenantAdmin`/`RoleCompliance` credential grant to anyone outside
platform-operator staff, whichever comes first.

**No automatic progression.** Per the stage-gate rule, the next phase
requires its own separate human authorization.

## Prior stage: Stage 4I Phase E — Operating Market & Country Policy Foundation — COMPLETE (three fix rounds applied), awaiting human review

**SESSION-START INSTRUCTION — MKT-MIG76-1 (read this before running any
`internal/operatingmarket` test or trusting a prior test result in this
stage):** migration `0076` has been amended in place THREE TIMES
(AMENDMENT-1, then AMENDMENT-2/SEC-E-REV-1, then AMENDMENT-3/SEC-E-REV-2 -
see `docs/governance/task-registry.md`'s `MKT-MIG76-1` entry under "Stage
4I Phase E" for full detail). Its version number never changed, because it
remains untracked/unreleased. ANY database - the shared local dev database
included - where 0076 was applied before all three amendments landed
carries a STALE schema (missing some or all of: the write-time narrowing
step, the `ocp_inherit_rung_withdrawal_requires_authorization` CHECK
constraint, and the `ocp_inherit_rung_close_requires_successor` DEFERRED
constraint trigger) with NO version-number signal that anything is wrong.
Before trusting any `internal/operatingmarket` test result: rebuild the
target database via a full migrate-down-then-up cycle, or point
`TEST_DATABASE_URL` at a fresh database built via `cmd/migrate up` from
current `migrations/`, then confirm directly against `pg_constraint`/
`pg_trigger`/`pg_proc` rather than assuming.
`TestMigration0076_SchemaMatchesTheCurrentMigrationFile` is the standing
regression guard for this hazard (now checking markers for all three
amendments, after a fix-round finding that it had not been extended for
AMENDMENT-3). This instruction expires when migration 0076 is released
(committed/merged) - after release, no further in-place amendment of 0076
is permitted.

**FIX ROUND (post-independent-review) — RESOLVED.** Six independent
reviews (architect fidelity, security, DB/RLS, compliance/privacy, QA/
adversarial, code review) ran against the implementation below and found
one BLOCKING defect plus several confirmed bugs and P3 findings. All are
now resolved; full detail at `docs/decisions/0045-operating-market-and-
country-policy-foundation.md` §16 (§3.5-A AMENDMENT-1) and
`docs/governance/task-registry.md`'s `MKT-NARROW-1`/`MKT-SCOPE-1` entries
under the "Stage 4I Phase E" section.

- **THE BLOCKING FIX (MKT-NARROW-1):** the original operation-rung
  resolution algorithm (most-specific-candidate-only) let a more-specific
  `enabled` row unmask a broader, in-force, active `disabled` row once the
  narrower row's own disable was withdrawn - reachable using only
  narrowing writes. Architect-ruled amendment (Option C): migration 0076
  (amended in place, still the only migration file for this phase) gained
  a write-time narrowing-enforcement trigger step, and
  `internal/operatingmarket/resolve.go`'s STEP 4 was rewritten to query
  the FULL operation-rung candidate set (no `LIMIT`) and evaluate it with
  first-disabled-wins. `PolicyVersion` bumped `stage-4i-e.v1` ->
  `stage-4i-e.v2` (no backfill - zero rows existed in either table to
  backfill). A real placement bug in the trigger's own first draft (the
  new step was textually unreachable for tenant-wide, non-brand-specific
  operation writes, due to a pre-existing early `RETURN NEW` in the
  brand-check block) was caught by
  `TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused`
  actually failing against the first version of the fix, and corrected
  before this round closed.
- **Fix 1:** both write paths' audit metadata now correctly folds the
  ACTUAL closed `effective_to` into `prior_effective_to` (previously
  always JSON `null` past the first version, since the "before" state was
  read before the close `UPDATE` ran).
- **Fix 2:** `SetJurisdictionCountryCode` (the one Phase-E-added
  `internal/jurisdiction` function affected) now asserts platform scope as
  its first statement - `jurisdictions` carries no RLS, so this is the
  only control. Pre-existing platform-registry functions with the
  identical gap are tracked, not fixed, as `MKT-SCOPE-1` (owned by
  `security`).
- **Fix 3:** the migration-0076 "leaves existing rows null" test now
  genuinely exercises pre-existing rows (raw-SQL-inserted before 0076 runs
  on a scratch database), plus an independent static-source guard.
- Several P3 items fixed (RLS predicate-text assertions, a tenant-delete-
  cascade regression test, a licence-status-read error no longer silently
  swallowed, `Explanation.LicenceValidity` now the typed enum, a future-
  regression guard-file-list gap closed); two P3 items (`ErrNotFound`/
  `ErrLicenceNotDeterminable`) left declared-but-unused with an explicit
  reserved-for rationale rather than removed or force-wired.
- Full validation gate re-run clean: `go build`/`go vet` (both build tags)/
  `gofmt -l`/whole-repo `go test -tags=integration ./... -count=1`, plus
  10 consecutive `-race` runs of the concurrency and new regression tests
  and 10 consecutive full-package runs, all green.

**SECOND FIX ROUND (ADR 0045 §3.5-A AMENDMENT-2, finding SEC-E-REV-1) —
RESOLVED.** A second, independently-found defect in the same trigger's
write-time design: withdrawing an active `disabled` row at the BRAND or
OPERATION rung (where absence INHERITS from the rung above) is
functionally a widening act - it can flip a resolution to `permitted` -
but was being treated as unconditionally fail-closed-safe, with no
`authorization_reference` required and no audit distinguishability. Full
detail at `docs/decisions/0045-operating-market-and-country-policy-
foundation.md` §17 (§3.5-A AMENDMENT-2) and `docs/governance/task-
registry.md`'s `SEC-E-REV-1`/`MKT-MIG76-1` entries under "Stage 4I Phase
E".

- **THE FIX:** a new CHECK constraint,
  `ocp_inherit_rung_withdrawal_requires_authorization`, on
  `operating_country_policies` (migration 0076, amended in place a
  second time - still the only migration file for this phase; see
  `MKT-MIG76-1`), requiring a non-blank `authorization_reference` on any
  `status='withdrawn'` write at `scope_kind IN ('brand', 'operation')`.
  A CHECK, not a trigger step, because by the time the `BEFORE INSERT`
  trigger runs the writer has already closed the predecessor version in
  the same transaction (the trigger cannot see the row being withdrawn),
  and because a CHECK is race-free by construction under READ COMMITTED
  where the trigger's own lookup-based steps are not (SEC-E-REV-3). A
  mirroring Go-side guard was added to `policy_admin.go`
  (`CreateOperatingCountryPolicyVersion`) as the diagnosable half; the
  CHECK constraint is the authoritative half. The TENANT rung and
  `licence_country_ceilings` are deliberately EXCLUDED (absence there is
  terminal/fail-closed, never inherited, so a withdrawal there can only
  ever narrow) - fenced by two new tests proving the exclusion is
  correct, not an oversight.
- **The kill-switch asymmetry is untouched:** writing
  `state='disabled', status='active'` still needs no
  `authorization_reference`, at any scope, in any order, on either
  table - a non-regression test pins this explicitly.
- **Audit:** both write paths now compute and record two new metadata
  keys, `rung_block_transition` and `widening_capable`, from the prior
  version's state and the write's own parameters only (never by calling
  `resolve()` on the write path). Binding auditor rule recorded in code
  and in the ADR: filter on `widening_capable = true` to find every
  event that widened the operating footprint - filtering on
  `state = 'enabled'` is wrong and misses every inherit-rung widening
  withdrawal.
- **Seven pre-existing test call sites** that withdrew a brand/operation-
  scope row with no `AuthorizationReference` were updated to supply a
  non-blank one (the CHECK constraint was not weakened to accommodate
  them - per the architect's explicit instruction, narrowing this
  control was never authorized).
- Eleven new tests added (mandated names from the ruling), bringing the
  package to 63 top-level test functions. Full validation gate re-run
  clean against a FRESH scratch database built via `cmd/migrate up`
  (per `MKT-MIG76-1`, since the shared local dev database was found to
  carry a stale pre-amendment schema): `go build ./...`, `go vet ./...`
  and `go vet -tags=integration ./...`, `gofmt -l .`, the full package
  suite, 10 consecutive `-race` runs of the new/modified tests, and the
  whole-repo `go test -tags=integration ./... -count=1` gate - all
  green. `resolve.go` and
  `operating_country_policies_enforce_ceiling()`'s executable body both
  confirmed ZERO diff (comment-only); `PolicyVersion` confirmed still
  `"stage-4i-e.v2"`; no RLS/permission/role-grant/index/FK change
  anywhere; no new migration file.

**THIRD FIX ROUND (ADR 0045 §3.5-A AMENDMENT-3, finding SEC-E-REV-2 —
"the bare close") — RESOLVED, plus two smaller findings (F2, F4).** A
third, independently-found defect in the same rung model, this time in
the UPDATE path rather than the INSERT path: at the BRAND/OPERATION
rungs, simply closing an open `active`+`disabled` row's `effective_to`
WITHOUT writing any successor row removes the block exactly like a
withdrawal does — but every prior control (both CHECK constraints, the
ceiling trigger, the audit computation) is INSERT-shaped, so a bare
`UPDATE` from an ordinary tenant-scoped connection bypassed all of them
with zero authorization and zero audit trace. The architect gave a
structural argument (enumerating every column `resolve()`'s window
predicate reads, and every verb that can change each) for why this is
the last variant in this defect family reachable by ordinary DML, and
explicitly recommended against a fourth sweep of the same shape — a
recommendation this session followed. Full detail at
`docs/decisions/0045-operating-market-and-country-policy-
foundation.md` §18 and `docs/governance/task-registry.md`'s
`SEC-E-REV-2`/`MKT-MIG76-1` entries.

- **THE FIX:** a new `DEFERRABLE INITIALLY DEFERRED` CONSTRAINT TRIGGER,
  `ocp_inherit_rung_close_requires_successor` (migration 0076, amended in
  place a THIRD time), requiring that any close of an in-force
  `active`+`disabled` brand/operation-rung row be followed, in the SAME
  transaction, by an open successor at the same key. A constraint
  trigger, not a CHECK (AMENDMENT-2's mechanism), because the predicate
  is inherently cross-row (comparing the closed row against whatever, if
  anything, replaced it) — a CHECK cannot express this. The fix does not
  re-implement an authorization test: every legal successor shape is
  either non-widening (`active`+`disabled`, the block persists) or
  already gated by an existing CHECK (`active`+`enabled` by
  `ocp_enable_requires_authorization`; `withdrawn`+`disabled` by
  AMENDMENT-2's CHECK), so requiring a successor's mere existence is
  sufficient. `DEFERRABLE INITIALLY DEFERRED` is load-bearing, not
  decoration — the sanctioned writer's close-then-insert pattern needs
  the close to succeed at statement time and only be checked at commit.
  Composes with, and does not duplicate, AMENDMENT-2; the piggyback case
  (one authorized withdrawal plus one silent bare close in the same
  transaction) is closed by ordinary atomicity — the whole transaction
  rolls back, so no half-authorized audit trail can ever be committed.
- **No `resolve.go` diff, no `PolicyVersion` bump.** AMENDMENT-3
  constrains which row sets the write path can construct; it does not
  change what `resolve()` computes for a fixed row set, so the
  `PolicyVersion`-bump test (does this change what any stored row
  resolves to?) correctly says no — unlike AMENDMENT-1, which did bump
  it. `resolve.go` and both ceiling-trigger functions' executable bodies
  are confirmed byte-identical to their post-AMENDMENT-2 state.
- **F2 (disclosed, not fixed):** `DELETE` on `operating_country_policies`
  remains wholly uncontrolled for any role that bypasses RLS (no
  trigger, no CHECK, no audit) — the DELETE arm is deliberately omitted
  so `tenants ON DELETE CASCADE` keeps working. Documented as a
  disclosed residual in the migration comment, the ADR, and the rewritten
  INV-M-7, pointing to ADR 0026's already-recorded migration-owner/
  runtime-role separation as the genuine (out-of-scope-for-this-phase)
  fix.
- **F3 (out of scope, tracked not fixed):** `tenants`/`licences` carry NO
  RLS at all (pre-existing, confirmed live: an ordinary tenant-scoped
  connection can suspend/reactivate its own licence, extend its own
  `expires_at`, re-point `tenants.licence_id`, and even suspend a
  DIFFERENT tenant's licence — a cross-tenant write). Phase E newly makes
  the licence the ROOT of the operating-market ceiling, so this
  pre-existing gap is now a live path to widening an operating-market
  answer. Not fixed here — amended into the existing `MKT-SCOPE-1` item
  (not a new item) with a second, independently-labeled trigger
  condition `MKT-SCOPE-1(b)`. **This is a CLAUDE.md multi-tenancy-
  isolation concern the human reviewer should weigh directly, not just a
  registry line item.**
- **F4 (fixed, `PolicyVersion` bump — the ONLY reason for the v2→v3
  bump in this round):** `EvaluateLicenceValidity` never checked
  `licences.issued_at`, so a licence not yet issued was treated as
  valid — a genuine fail-open in the ceiling's own root predicate. Fixed
  with a half-open `[issued_at, expires_at)` interval check
  (`internal/jurisdiction/licence_validity.go`), status checked first (a
  suspended licence reads `suspended`, not `not_yet_issued`), NULL
  `issued_at` correctly falls through rather than fail-closing every
  existing row (no Go code writes this column today). `PolicyVersion`
  bumped `"stage-4i-e.v2"` → `"stage-4i-e.v3"` — attributable to F4
  alone, explicitly not to AMENDMENT-3, since this change (and only this
  change) alters what `resolve()` computes for an identical stored row
  set.
- **Independent re-verification, twice**, mirroring the discipline from
  the prior two fix rounds: an architect-fidelity/security dual pass
  confirmed the fix closes the reported attack (live-reproduced against
  a fresh database, including adversarial attempts via
  `SET CONSTRAINTS ALL IMMEDIATE` and trigger-disabling — both behaved
  exactly as the architect's ruling predicted, with the trigger-disabling
  residual explicitly disclosed rather than treated as newly discovered);
  a follow-up polish round then closed a further code-review/security P2
  (the stale-schema regression guard had not itself been extended to
  check for AMENDMENT-3's markers, and the task registry briefly implied
  it had) plus several smaller diagnosability/doc-accuracy items,
  including correcting ADR §18's own closing "residual set" claim after
  a reviewer live-reproduced a third, smaller residual it had
  understated (an ordinary-RLS transaction can still close a block and
  insert an authorized-shaped but *unverified* withdrawn successor —
  routed to `MKT-DUAL-1`'s existing scope, not a new fix).
- Package now stands at 78 top-level test functions in
  `internal/operatingmarket` plus 9 in `internal/jurisdiction`'s
  `licence_validity`/`country_code` test files. Full validation gate
  re-run clean multiple times against independently-built fresh scratch
  databases (schema markers for all three amendments confirmed directly
  against `pg_constraint`/`pg_trigger`/`pg_proc` before trusting any
  result, per `MKT-MIG76-1`'s own repeatedly-relearned lesson): `go
  build`/`go vet` (both tags)/`gofmt -l`, the full package suite, `-race`
  runs of every concurrency-sensitive and new regression test, and the
  whole-repo `go test -tags=integration ./... -count=1` gate — all
  green, independently re-verified by the orchestrator directly (not
  only trusted from agent self-reports) on a scratch database built and
  schema-checked from scratch.

**Status: IMPLEMENTED as a MECHANISM ONLY**, built exactly to the
architect design ruling recorded at `docs/decisions/0045-operating-
market-and-country-policy-foundation.md`. Full detail there, in
`docs/governance/task-registry.md`'s "Stage 4I Phase E" section (findings
and the one defect found/fixed during implementation), and in
`docs/governance/stage-4i-canonical-model.md` §6.1's amendment.

**What Phase E built:** a new package, `internal/operatingmarket`,
structurally separate from `internal/jurisdiction` (may import it for
exactly one shared function, `EvaluateLicenceValidity`; cannot import
`internal/identity`/`kyc`/`geolocation`/`rg`; mechanically enforced via
`go list -deps`). It answers "for a tenant/brand, an operation, optionally
a product, a country — is this platform permitted to operate, given the
licence's ceiling and every narrower policy beneath it?" — a
structurally different question from "which regulatory jurisdiction
governs this player", which remains exclusively `internal/jurisdiction`'s.
Three new tables (migration `0076`): `platform_operations` (a new,
extensible OPERATION vocabulary, deliberately disjoint from
`jurisdiction.OperationClass`/`asset_operation_eligibility.operation`/
`risk_rules.operation` — product and operation are two independent
dimensions, `platform_products` reused unchanged for the product axis);
`licence_country_ceilings` (the platform-wide, append-only ceiling a
licence places on permitted countries — now the SOLE authoritative
source, deprecating `licences.permitted_markets` in place); and
`operating_country_policies` (the tenant/brand/operation-scoped,
append-only narrowing beneath that ceiling). A five-step resolution
algorithm (`ResolveOperatingCountryPolicy`) produces an eleven-valued,
non-forgeable `Result` with no accessor for any blocking/source/licence
provenance (that diagnostic is a separate, staff-only
`ExplainOperatingCountryPolicy` call) and no accessor that could
substitute it for a `jurisdiction.Resolution`. One new file added to
`internal/jurisdiction` (`licence_validity.go`,
`EvaluateLicenceValidity` — the single technical implementation of
SEC-4I-F10's "is this licence reliable" predicate); every other file in
that package has **zero diff**. Four new permissions
(`internal/auth/permission.go`), no HTTP route, no OpenAPI change.

**The decisive scope-control property, unchanged from every prior Stage
4I phase:** `internal/jurisdiction/resolver.go`, `precedence.go`,
`types.go`, `evaluation_policy.go`, `evaluation_policy_admin.go`,
`resolution_active.go`, and `evidence_collection_active.go` have **zero
diff** (confirmed via `git diff --stat`). Zero production callers of any
new function exist anywhere in the codebase — no HTTP route, no
OpenAPI change, no country/market content (migration 0076 inserts only
the four seeded `platform_operations` vocabulary rows; zero rows in
`licence_country_ceilings`/`operating_country_policies`; zero non-NULL
`jurisdictions.country_code` values), no dual control (deliberately
fail-closed via the total absence of any production-reachable enable
path — task registry item `MKT-DUAL-1` names the required follow-up), no
real geolocation, no production jurisdiction enforcement.

**Testing:** originally 32 tests/test-groups (the architect ruling's own
coverage floor); three successive fix rounds each added a further set of
mandatory regression tests (named in each amendment's ruling) plus
several QA-authored adversarial tests written independently during
review, bringing the package to 78 top-level test functions in
`internal/operatingmarket` (several with multiple named sub-cases), plus
9 more in `internal/jurisdiction`'s `licence_validity`/`country_code`
test files, as of this stage's close. Coverage spans the registry/
country-code invariants, the licence ceiling and its four behavioural
cases (ON allows enable; OFF refuses enable; a subsequent OFF immediately
blocks a previously-enabled lower scope with no rewrite; a subsequent ON
does NOT auto-re-enable a previously-disabled lower scope), the full
resolution algorithm including two states unreachable via any sanctioned
write path (`configuration_conflict`, `policy_expired`, both constructed
with raw SQL against a temporarily-disabled trigger/CHECK and restored
afterward) and the §3.5-A AMENDMENT-1 operation-rung fix's own dedicated
regression tests, RLS per table (including the asymmetric no-platform-read
posture on `operating_country_policies` and the asymmetric no-DELETE-
trigger posture vs. `licence_country_ceilings`, and now predicate-text
assertions, not just policy names), the registration narrow-projection's
fail-closed contract, audit content (real JSON assertions, including the
fix-round's corrected `prior_effective_to`), the import-graph invariant,
and concurrency — using ONLY the mandatory deterministic
uncommitted-competing-row-plus-`pg_stat_activity`-poll technique (never a
`sync.WaitGroup` barrier or `time.Sleep`, the exact mistake that cost
Phase D two fix rounds). All pass under `-race`, independently re-run 10
consecutive times with no failure in the fix round's own validation gate,
alongside a clean whole-repo `go test -tags=integration ./...` run
covering every other package.

**A real defect found and fixed during implementation, disclosed rather
than silently patched:** the down-migration's own existence-guard
(refusing a rollback while either new policy table holds rows) was
initially ineffective — migrations run over a scopeless database
connection, and (unlike migration 0075's deliberately permissive
`jurisdiction_precedence_configs` read policy) both new tables' RLS is
narrower than a scopeless connection can satisfy, so the guard's own
`EXISTS` checks always saw zero rows regardless of real content. Fixed by
temporarily disabling RLS on both tables inside the SAME transaction as
the checks (self-contained: a real finding's `RAISE EXCEPTION` rolls back
the `ALTER TABLE` too). Caught by the down-migration test genuinely
failing against a real database, not by review — recorded in the task
registry's own "Stage 4I Phase E" section per this project's disclosure
discipline.

**Explicitly deferred, not performed this phase:** any HTTP route or
OpenAPI change; any resolver wiring into `casino`/`bonus`/`risk`/
`payments`/`sportsbook`/registration/withdrawal; any country/market
content; dual control on enabling a country (`MKT-DUAL-1`, a cross-cutting
decision requiring its own ADR, now explicitly bound to cover every
widening-capable write including AMENDMENT-2's withdrawal shape);
confirming the licence expiry AND issuance boundaries with compliance
(`MKT-EXPIRY-1`, widened by F4); removing `licences.permitted_markets`
(`MKT-PM-1`, blocked on HDR-J-6); giving `tenants`/`licences` RLS
(`MKT-SCOPE-1`, amended this round to add `tenants` and a second,
independent trigger condition `MKT-SCOPE-1(b)` specific to this phase's
own ceiling dependency — owned by `security`, out of this phase's
authorized diff); SEC-4I-F10's own full closure on the player-jurisdiction
path (re-scoped, not closed — `resolver.go` still has zero diff); any
answer to HDR-M-1/HDR-M-2/HDR-J-6/HDR-J-7/HDR-J-8/HDR-J-9.

**No automatic progression.** Per the directive's own mandatory stop
condition, any phase beyond this one, any of the above deferred items,
and any production jurisdiction/operating-market activation remain
unauthorized pending a separate human directive reviewing this Phase E
completion report.

---

## Prior stage: Stage 4I Phase D — Jurisdiction Policy Configuration & Operational Semantics — COMPLETE, awaiting human review

**Status: IMPLEMENTED, independently reviewed by five specialists
(`architect` fidelity, `security` double-hatting as DB/RLS specialist,
`identity-compliance` for compliance/privacy, `qa`, `risk` for cross-domain
integration), all P0/P1 findings fixed across two fix rounds (the second
triggered by a focused `security` re-verification that caught the first
fix's own residual defect), and independently re-verified.** Built the
configuration infrastructure for three of Phase C's four deferred PC-GAP
items — deliberately NOT the actual policy content, which remains a
legal/regulatory decision. Full detail: `docs/decisions/0043-jurisdiction-
evaluation-policy-configuration.md` (the design ADR), `docs/decisions/
0044-human-decision-register-stage-4i-phase-d.md` (three new open human-
decision items), `docs/governance/stage-4i-canonical-model.md` §15, and
`docs/governance/task-registry.md`'s "Stage 4I Phase D" section (full
findings/disposition ledger).

**What Phase D built:** `internal/jurisdiction.RequiredPurposes` (PC-GAP-3's
`OperationClass`→`Purpose` mapping seam — one canonical owner, zero mapping
content, all four operation classes fail closed with
`ErrPurposeMappingUndetermined`); `ResolveEvaluationPolicy`/
`CreateEvaluationPolicyVersion`/`ListEvaluationPolicyVersions` (PC-GAP-1/
PC-GAP-2/PC-GAP-4's config read/write API, backed by migration `0075`
widening the existing shape-only `jurisdiction_precedence_configs` table in
place — new `status`/`location_requirement`/`max_location_signal_age_seconds`/
`precedence_status`/`precedence_policy_version`/`legal_review_reference`/
`reason_code` columns, RLS added to this table for the first time,
forge-proof append-only triggers); one new permission
(`PermJurisdictionEvaluationPolicyWrite`, platform-admin-only, no HTTP
route in this phase). Three new human-decision items opened and registered
rather than guessed at: **HDR-J-7** (which `Purpose`(s) each
`OperationClass` requires — the architect explicitly rejected a tempting
but invalid engineering inference here, since the choice governs whether a
real-time geolocation signal may restrict an operation at all, a licensing
judgement, not a fact recoverable from existing code), **HDR-J-8**
(location-requirement threshold per licensing jurisdiction/operation
class), **HDR-J-9** (location-staleness bound, same key).

**The decisive scope-control property, unchanged from every prior Stage 4I
phase:** `internal/jurisdiction/resolver.go`, `precedence.go`, and
`types.go` have **zero diff**; `purpose.go` carries a doc-comment-only
diff. Zero production callers of any new function exist anywhere in the
codebase. No HTTP route, no OpenAPI change, no country/market content, no
real geolocation vendor, no production jurisdiction enforcement.

**Review chain (all independent, none self-certified), including a real
mid-course correction:** the orchestrator's own pre-implementation
reconnaissance concluded all four operation classes deterministically
require market-access control, reasoning from what each existing consumer
does. `architect`'s design ruling explicitly rejected this as an invalid
inference — `Purpose` selects which evidence hierarchy is legally
authoritative, not what kind of decision a consumer makes, and a per-game
blocklist keyed on *verified residence* is an equally coherent, sometimes
legally required, design. Guessing wrong in either direction carries real
harm with no safe default, hence HDR-J-7 rather than a coded answer.

Three independent reviewers (`architect`, `security`, `qa`) each
independently and empirically found the same P1: an integration test
proving the config write path's concurrency control did not reliably
force the race it claimed to test, failing ~30-50% of repeat runs. A first
fix (a synchronization barrier) closed the reproducible failure but was
then shown by a dedicated `security` re-verification pass to still fail
under CPU contention (9/200 runs) — the barrier synchronized transaction
start, not the actual write race. A second, targeted fix replaced
scheduling-dependent assertions entirely with a genuinely deterministic
test forcing the race via real PostgreSQL unique-index locking semantics
(a `pg_stat_activity` poll confirms the block, never a sleep) — verified
by the orchestrator directly: 30 consecutive passes under `-race`, plus a
clean whole-repo `go test -tags=integration ./...` run. Verdicts:
`architect` — CERTIFIED WITH NAMED EXCEPTIONS (BLOCKED on the P1 through
both review passes; closed by the deterministic replacement).
`security` — CERTIFIED WITH NAMED EXCEPTIONS on both its original review
and its fix-round re-verification (no P0/P1 on either pass). `identity-
compliance` — NO VIOLATIONS FOUND. `qa` — READY WITH NAMED GAPS (originally
NOT READY on the same P1, found independently). `risk` — NO INTEGRATION
CONCERNS. A subsequent `code-reviewer` pass over the final diff found one
P2 (this governance-doc gap, closed by this entry) and several accepted/
deferred P3-P4 cosmetic items, all recorded in the task registry.

**Documentation updated this phase:** new §15 in `docs/governance/
stage-4i-canonical-model.md` (the full Phase D record, including an honest
account of the concurrency-test defect's full lifecycle rather than a
premature self-certification); §3.4 and §6.1 amendment notes; §14.6's
PC-GAP table gained a "status after Phase D" note per item (PC-GAP-1/2
mechanism closed, content blocked on HDR-J-8/HDR-J-9; PC-GAP-3 seam
closed, mapping blocked on HDR-J-7; PC-GAP-4 lookup/config mechanism
closed, production wiring not implemented); new ADR 0043; new HDR register
0044.

**Explicitly deferred, not performed this phase:** HDR-J-7/HDR-J-8/HDR-J-9's
actual content; any production wiring of the new functions into `casino`,
`bonus`, `risk`, or `assetregistry`; an activation permission and
dual-control ruling for writing `status='active'`; the
`jurisdiction_resolutions.reason` CHECK widening for Phase C's three new
`Reason` values; the pre-existing PHASE-B-ARCH-1 `effective_from`/actor-
provenance defect on the two older activation tables (re-evaluated against
its own three trigger conditions this phase — none fired, remains
deferred); a `btree_gist` range-exclusion constraint for a narrow,
raw-SQL-only residual on overlapping policy windows; a maximum-staleness
ceiling (framed as a sub-question inside HDR-J-9); a shared-helper refactor
for duplicated row-scanning code between the list and admin read paths.

**No automatic progression.** Per the directive's own mandatory stop
condition, any phase beyond this one, HDR-J-7/HDR-J-8/HDR-J-9's content,
and any production jurisdiction activation remain unauthorized pending a
separate human directive reviewing this Phase D completion report.

---

## Prior stage: Stage 4I Phase C — Jurisdiction Precedence & Resolution Rules Foundation — COMPLETE, awaiting human review

**Status: IMPLEMENTED, independently reviewed by four specialists
(`architect` fidelity review, `security`, `identity-compliance` for
compliance/privacy, `qa`), all P0/P1 findings fixed, all P2-P4 findings
either fixed or explicitly disposed, and re-verified.** Built the
deterministic TECHNICAL FOUNDATION for resolving a player's own
jurisdiction from evidence — per HDR-J-1 through HDR-J-6 (including
HDR-J-3's 8 sub-items), `docs/decisions/0042-human-decision-response.md`
— explicitly NOT activating production enforcement. Full detail:
`docs/governance/stage-4i-canonical-model.md` §14 (the canonical contract)
and §7.3/§7.4 (amended this phase); `docs/governance/task-registry.md`'s
"Stage 4I Phase C" section (full findings/disposition ledger);
`docs/progress.md`'s matching narrative entry.

**What Phase C built:** `internal/jurisdiction.DeterminePlayerJurisdiction`
(`precedence.go`) — a pure function (no `context.Context`, no database
handle, never calls `time.Now()`) implementing the precedence rules for
two live purposes (`PurposeIdentityDetermination`,
`PurposeMarketAccessControl`) plus an explicit refusal for the third
(`PurposeHistoricalReporting` — a historical jurisdiction must be read
from the event-time record, never recomputed). Supporting types: the
`Purpose` taxonomy (`purpose.go`, deliberately separate from the
pre-existing `OperationClass` enum — no mapping between them exists,
PC-GAP-3); the `EvidenceSet`/`LocationSignalEvidence` evidence model
(`evidence.go`, exactly three fields, no tenant/brand/licence input
reachable); the non-forgeable `PlayerJurisdictionResult`/`Candidate`/
`ConsideredEvidence` result types (`player_result.go`) distinguishing
resolved / four distinct unresolved reasons / conflicting evidence
(`HasDisagreement()`) — never a generic "unknown"; and
`ComposeRestrictions` (`restriction.go`), the canonical-model §7.3
most-restrictive-outcome composition primitive, built for the first time
this phase but with zero production callers.

**The decisive scope-control property, unchanged from Phase A/B/
PHASE-B-ARCH-1:** `internal/jurisdiction/resolver.go` — the only resolver
any consuming domain (`casino`, `bonus`, `risk`) actually calls — has
**zero diff**, verified by `git diff --stat` after implementation, after
all four independent reviews, and after the fix round. There are zero
production call sites of `DeterminePlayerJurisdiction` or
`ComposeRestrictions` anywhere in the codebase.

**Review chain (all independent, none self-certified):** mandatory
pre-implementation impact-map analysis across every consuming package →
`architect` design ruling (full type/function signature specification,
including an explicit **CRITICAL STOP CONDITION** clause for any element
that would require inventing legal/policy content) → `backend`
implementation exactly per that ruling → four independent parallel
reviews → orchestrator triage and fix round → full validation re-run
(`go build`, `go vet ./...`, `go vet -tags=integration ./...`,
`gofmt -l .`, unit + race + whole-repo integration suite against a real
Postgres) → orchestrator integration. Verdicts: `architect` — CERTIFIED
WITH NAMED EXCEPTIONS (no blocking issues after the fix round;
`resolver.go` verified at literal zero diff throughout). `security` —
CERTIFIED WITH NAMED EXCEPTIONS (one P1 shared with architect/qa, closed
with a single fix; no unresolved P0/P1). `identity-compliance` — NO
VIOLATIONS FOUND (no nationality concept introduced; correct player/
tenant jurisdiction separation maintained; independently surfaced the
same redaction-bypass finding security and architect also found). `qa` —
READY WITH NAMED GAPS, all closed in the fix round.

**Two defects were independently found by three of the four reviewers**,
using different methods (direct code reading, adversarial mutation/
compile probes, and adversarial format-verb probes): (1) a slice-aliasing
non-forgeability break — `Candidates()`/`ConsideredEvidence()`/
`Contributors()` all returned their internal backing array directly,
letting a caller reorder a resolved result's candidates in place and have
`PrimaryCandidate()` report a location signal as the primary
determination, defeating HDR-J-3a's core guarantee — fixed via
`slices.Clone` on all three accessors; (2) `fmt`'s `%#v` verb bypassing
every `String()` method's redaction and dumping the country code via
unexported struct fields — fixed with `GoString()` (`fmt.GoStringer`)
added to all five affected types. Three further P1/P2 architect findings
(a fail-open `LocationRequirement` validation path, a zero `AsOf` silently
disabling the location-freshness gate, and a dropped audit-evidence entry
on the path that actually denies a player) were fixed in the same round,
plus five lower-severity security findings (a future-dated signal reading
as "always fresh"; an unrecognized location state echoed verbatim into a
diagnostic; a structurally invalid declared residence falsely recording as
"disagreeing" with a valid verified one; a zero-duration policy footgun,
doc-only; and the identity-purpose path never emitting a reason the
market-access path already emitted for the symmetric case). Full
findings ledger with disposition for every item, including the four
deferred PC-GAP legal/policy seams (PC-GAP-1 through 4):
`docs/governance/task-registry.md`'s "Stage 4I Phase C" section.

**Documentation updated this phase:** new §14 added to
`docs/governance/stage-4i-canonical-model.md` (the full canonical
resolution contract, operation taxonomy, evidence precedence, unresolved/
fail-closed semantics, more-restrictive semantics, event-time semantics,
tenant/player separation, and the PC-GAP register); §7.3 amended (the
`blocked > restricted > allowed` severity vocabulary, now anchored in
real code); §7.4 corrected (previously claimed MROC "is NOT built in
Stage 4I" — withdrawn; the composition primitive is now built, with the
honest caveat that it has zero production callers).

**Explicitly deferred, not performed this phase (per the directive's own
explicit scope boundary):** production market-list population, country
allow/deny content, production jurisdiction enforcement of any kind, a
real geolocation vendor, nationality (no concept exists anywhere in this
engine), retention/erasure implementation, legal-basis determination, the
staff-correction endpoint (no minimal seam was found strictly required),
G-2, sportsbook cashout, converted-Grant clawback, BYOL, and any payment/
casino/risk behaviour change beyond the resolver seams already buildable
now. **All activation switches remain OFF.**

**No automatic progression.** Per the directive's own mandatory stop
condition, Phase D and any production jurisdiction activation remain
unauthorized pending a separate human directive reviewing this Phase C
completion report.

---

## Prior stage: Stage 4I PHASE-B-ARCH-1 — Activation-Gate Enforcement Asymmetry Hardening Gate — COMPLETE, awaiting human review

**Status: IMPLEMENTED, independently reviewed by `security` (CERTIFIED
WITH NAMED EXCEPTIONS) and `qa` (READY WITH NAMED GAPS), all findings
either fixed or explicitly disposed, and re-verified.** Closes the one
P2 Phase B recorded as a hard prerequisite before any Phase C-dependent
work: the declared-residence write path's activation gate
(`jurisdiction_evidence_collection_active`) was enforced only in the
`PUT /v1/me/residence` HTTP handler, while the sibling KYC path enforced
it structurally inside `kyc.ReviewVerification` itself. Full detail:
this session's PHASE-B-ARCH-1 completion report; see
`docs/governance/task-registry.md`'s "Stage 4I PHASE-B-ARCH-1" section
and `docs/progress.md`'s matching narrative entry.

**What this gate built:** moved the activation check INSIDE
`identity.SetPlayerAccountDeclaredResidence` itself (in the same
transaction as the write, mirroring `kyc.ReviewVerification`'s own gate
exactly), added a connection-scope assertion (closing a verified hazard:
a player-scoped caller would otherwise see a misleading "collection is
off" 403 while collection is actually on, since the gate table's RLS
excludes player scope while `player_accounts`' own RLS does not), and
removed the HTTP handler's now-redundant duplicate check entirely rather
than keeping it as defence-in-depth. A `BEFORE INSERT OR UPDATE` trigger
— the remedy both prior Phase B reviews had floated as preferred — was
considered by the architect and **rejected** (it would require either
misfiring on legitimate player-scoped writes or a `SECURITY DEFINER`
RLS-bypassing function — a net security regression).

**Independent review found and the fix round closed five findings**, all
P3/P4 (no P0/P1/P2 — the target P2 is genuinely closed, confirmed by
`security`'s own adversarial bypass attempts across 10 distinct classes):
missing in-function ISO-3166 validation (fixed, mirroring KYC); a real,
previously-untested cross-tenant write-path gap (fixed, new regression
test); a documentation overclaim (corrected); the trigger-rejection
rationale's player-scope argument, undercut by this very fix (corrected
in place, the overall trigger decision itself not reopened); an
unclassifiable mis-scoped-transaction error (fixed with a dedicated
sentinel). One gap accepted and documented, not fixed: a bounded TOCTOU
window on the unlocked gate-check read, identical to a pre-existing
characteristic already on the KYC path, not introduced by this pass — a
fix belongs to a future `architect`-owned cross-path design change.

**Deferred, per the architect's own explicit ruling (not silently
dropped):** Phase B's separate `effective_from`/actor-provenance
staleness finding — verbatim disposition (owner, affected tables,
rationale) recorded in `docs/governance/task-registry.md`.

**Validation:** build/vet/gofmt clean; full integration suite for
`internal/identity` (29 tests), `internal/httpserver`, `internal/kyc`,
`internal/jurisdiction`, `internal/validation` green; race-clean;
broader regression (`casino`/`bonus`/`risk`/`ledger`/`payments`/
`withdrawal`) green; migration-chain round-trip re-confirmed (no new
migration this pass). `git diff --name-only` touched exactly 6 files —
no migration, no new HTTP endpoint, no OpenAPI change,
`internal/kyc`'s own gate at literal zero diff.

**No automatic progression.** Phase C, HDR-J-2 precedence configuration,
permitted-market population, production jurisdiction enforcement, a real
geolocation provider, nationality, G-2, sportsbook cashout, converted-
Grant clawback, and BYOL remain unauthorized pending a separate human
directive.

---

## Prior stage: Stage 4I Phase B — Player Jurisdiction Evidence Foundation — COMPLETE, awaiting human review

**Status: IMPLEMENTED, independently reviewed, all P1 findings fixed, and
re-verified.** Builds the technical evidence *foundation* for
player-level jurisdiction determination (HDR-J-3a/b/c/e/f/g/h,
`docs/decisions/0042-human-decision-response.md`), with an explicit
activation boundary so building the capability never itself activates a
regulatory decision. Full detail: this session's Phase B completion
report (delivered to the human directly); see
`docs/governance/task-registry.md`'s "Stage 4I Phase B" section and
`docs/progress.md`'s matching narrative entry for the technical trace.
Note on numbering: this is the human directive's own "Phase B" — see the
phase-lettering correction note added to
`docs/plans/stage-4i-jurisdiction-implementation-plan.md` §14, since that
document's own internal A–I lettering uses a different scheme (this work
is roughly that document's Phase E plus a narrowed slice of Phase F).

**What Phase B built:** three evidence subsystems. (1) Declared
residence: `player_accounts.declared_residence_{country,captured_at}`,
`internal/identity.SetPlayerAccountDeclaredResidence`/
`GetDeclaredResidence`, `GET`/`PUT /v1/me/residence` (player self-service
only, no staff write surface this phase). (2) KYC-verified residence:
`kyc_verifications.verified_residence_{country,source,set_by,set_at}`,
an optional `verified_residence_country` field added to the *existing*
`POST /v1/admin/kyc/verifications/{id}/review` (no new endpoint, no new
permission — reuses `verification:review`), `internal/kyc.GetVerifiedResidence`.
(3) A physical-location signal abstraction: `internal/geolocation`
(`LocationProvider` interface + `MockLocationProvider` only — no vendor,
no HTTP route, no resolver wiring). All three are gated by a new,
per-tenant, per-evidence-type activation switch
(`jurisdiction_evidence_collection_active`, migration `0074`,
`internal/jurisdiction/evidence_collection_active.go`,
`GET`/`PUT /v1/admin/jurisdiction-evidence-collection[/{evidenceType}]`,
`RoleCompliance`-only `PermJurisdictionEvidenceCollectionActivate`) —
absent means OFF, fail-closed, checked inside the same transaction as
every write.

**The decisive scope-control property:** `internal/jurisdiction/resolver.go`
has **zero diff**, verified independently by all three reviewers. Evidence
is technically collectible (subject to the activation switch) but is not
consumed by any jurisdiction decision this phase — two standalone,
independently-tested read accessors (`identity.GetDeclaredResidence`,
`kyc.GetVerifiedResidence`) exist with zero non-test callers, built for a
future phase to wire in. No player/tenant-subject behavior changes.

**Review chain (all independent, none self-certified):** `architect`
design ruling (12 numbered decisions) → parallel `backend`/`integrations`/
`identity-compliance` implementation → parallel independent
`security`/`architect`/`qa` review → orchestrator fix round → orchestrator
integration and independent re-verification (build/vet/fmt/unit/
integration-against-real-Postgres/race, run directly). Verdicts:
`security` — CERTIFIED WITH NAMED EXCEPTIONS (one P1, fixed: a reviewer
free-text field was leaking into an audit entry that specifically existed
to never carry the residence value — found independently by all three
reviewers, fixed by removing the field from that entry's metadata and
adding a regression test that deliberately tries to leak it). `architect`
— CERTIFIED WITH NAMED EXCEPTIONS (all twelve rulings conformed,
`resolver.go` and the `Basis` enum verified at literal zero diff). `qa` —
READY WITH NAMED GAPS, all three P1 test-coverage gaps closed in the fix
round (CHECK/FK constraint tests added; a cross-tenant isolation gap
closed at the root by removing an unnecessary caller-supplied `tenantID`
parameter rather than just adding the missing test). One P2 — an
enforcement-asymmetry between the two write paths' activation gates
(the KYC gate is structurally unreachable-around; the declared-residence
gate is enforced only in the HTTP handler) — independently found by both
`security` and `architect`, **not fixed this phase**: both converge on a
database-trigger remedy, but it is a cross-table `architect`+`security`
joint design decision, now recorded as a **hard prerequisite gate** on
the still-deferred staff-correction-of-declared-residence endpoint
(`PHASE-B-ARCH-1`). Full findings ledger with disposition for every item:
`docs/governance/task-registry.md`'s "Stage 4I Phase B" section.

**Documentation updated this phase:** `docs/api/openapi/platform-api.yaml`
(the Phase A backfill this phase's own architect ruling reversed the
prior deferral recommendation on, plus this phase's own new surface);
`docs/architecture/16-privacy.md` (corrected the stale Stage-2-era
data-minimization claim; new sensitive-fields rows; an explicit lawful-
basis/activation-boundary section); `docs/security/security-architecture.md`
(new §J4I.12); `docs/governance/ownership.md` (the first genuine
cross-domain ownership overlap between the jurisdiction and identity-
compliance domains, split by column not by table).

**Explicitly deferred, not performed this phase** (see the plan's own
Phase C+ and the task-registry findings ledger for the full list):
HDR-J-2's full precedence policy, permitted-market population,
nationality, G-2, sportsbook cashout, converted-Grant clawback, BYOL
onboarding, any resolver wiring of the two new read accessors, staff
correction of declared residence (now also gated by the enforcement-
asymmetry disposition above), a real physical-location vendor, the
retention/erasure job (HDR-J-3f), a staff-facing read surface for
`player_residence:read`. No new Human Decision Register item was
required.

**No automatic progression.** Per the authorizing directive, Phase C and
beyond, HDR-J-2 precedence configuration, permitted-market population,
production jurisdiction enforcement, G-2, sportsbook cashout, and
converted-Grant clawback remain unauthorized pending a separate human
directive reviewing this Phase B completion report.

---

## Prior stage: Stage 4I Phase A — Tenant Licence Jurisdiction Basis — COMPLETE, awaiting human review

**Status: IMPLEMENTED, independently reviewed, fixed, and re-verified.**
Closes the single highest-leverage gap the Stage 4I Implementation Plan's
current-state assessment identified: `tenants.licence_id` — the one
column the jurisdiction resolver's `tenant_licence` basis reads — had
zero application write path anywhere in the repository. Full detail:
this session's Phase A completion report (delivered to the human
directly; see `docs/governance/task-registry.md`'s new "Stage 4I Phase
A" section and the `docs/plans/stage-4i-jurisdiction-implementation-plan.md`
Phase A entry for the technical trace).

Between this stage and Stage 4I's own closure (below), two governance-only
gates ran first, per their own directives: (1) `docs/decisions/0042-
human-decision-response.md` was completed with the human's verbatim
decisions for all 11 previously-open items (HDR-J-1 through HDR-J-6 incl.
HDR-J-3's 8 sub-items, G-2, `OpenBetSelfExclusionPolicy`, the mixed/
bonus-funded sportsbook cashout policy plus FD-1, and the converted-
Grant-cancellation/receivable question) — no code changed; and (2)
`docs/plans/stage-4i-jurisdiction-implementation-plan.md` traced every
decision into concrete technical consequences across a 9-phase
implementation sequence (Phases A-I) — planning only, no code changed,
approved by the human for Phase A specifically.

**What Phase A built:** `internal/jurisdiction.AssignTenantLicence`
(new file `tenant_licence_admin.go`), the platform-admin-only endpoint
`PUT /v1/admin/tenants/{tenantID}/licence`, and a new, narrowly-scoped
permission `PermTenantLicenceAssign` (distinct from both `PermTenantWrite`
and `PermJurisdictionRegistryManage`). The licensee/`licensing_model`
match invariant is enforced entirely by migration 0007's pre-existing
composite FK, never re-implemented in application code. Generic across
both the platform-licensed and BYOL (`own_licence`) shapes of the hybrid
licensing model from day one, with a dedicated passing test for each.

**Review chain (all independent, none self-certified):** `architect`
design ruling → `backend` implementation → parallel independent
`security`/`architect`/`qa` review → `backend` fix round addressing every
finding → orchestrator integration. Verdicts: `security` — CERTIFIED WITH
NAMED EXCEPTIONS (no P0/P1); `architect` — ARCHITECTURALLY CERTIFIED,
with named exceptions (no blocking issues); `qa` — READY WITH NAMED GAPS,
both P1s closed in the fix round. Full findings ledger, with disposition
for every item: `docs/governance/task-registry.md`'s "Stage 4I Phase A"
section.

**Honest scope statement, unchanged from Stage 4I's own steady state:**
Phase A adds a write path; it does not add a consumer. Both production
`Resolve` call sites (`internal/casino`, `internal/bonus`) structurally
always pass a non-nil `PlayerAccountID`, so every player-scoped
resolution still returns `unresolved(no_signal)`, exactly as before Phase
A. Bonus issuance remains blocked; casino's per-game blocklist behaviour
is unchanged. The `tenant_licence` basis is now technically producible
for the first time in the platform's history, but remains unobserved in
production until a tenant/brand-subject consumer exists (a later phase)
**and** an operational data-entry step assigns the platform's real
licence to the real production tenant (explicitly not performed by this
phase — a human confirmation, not an engineering task).

**Explicitly deferred, not performed this phase** (see the plan's own
Phase B-I and the task-registry findings ledger for the full list):
player physical-location collection, declared/verified residence
collection, KYC verified-residence workflow, nationality, jurisdiction
precedence configuration content, permitted-market population, G-2
bonus brand policy implementation, sportsbook cashout, converted-Grant
clawback, BYOL onboarding, and any resolver-side (evaluation-time)
licence-status/expiry check (SEC-4I-F10 remains open, unchanged by this
phase's bind-time-only check). No new Human Decision was required or
invented.

**No automatic progression.** Per the authorizing directive, the next
phase (B through I) requires its own separate human authorization.

---

## Prior stage: Stage 4I — Platform-Wide Jurisdiction Resolution Foundation — CONCLUDED, awaiting human authorization for the next stage

**Status: PARTIALLY IMPLEMENTED** (per CLAUDE.md's no-fake-completion
discipline — deliberately, not as a shortfall). Full detail:
`docs/governance/stage-4i-report.md`. `docs/progress.md`'s "Stage 4I" entry
has the running narrative.

The human authorized closing the platform-wide jurisdiction-resolver gap
Wave 3 identified as a carried dependency. Thirteen specialist phases built
a canonical, provider-neutral, fail-closed jurisdiction resolution
foundation (`internal/jurisdiction`, migrations `0071`-`0073`, a registry
admin surface, an append-only resolution-record table) consumed correctly
by AssetAuthorization (unchanged), Risk (reviewed and confirmed correct,
unchanged), casino's per-game blocklist (fail-open defect fully
remediated), and five Bonus admin surfaces (client-suppliable jurisdiction
removed, server-resolved instead). Twelve real defects were found and
fixed across the review chain, every one proven with a regression test.
Final independent security/compliance verdict: CERTIFIED WITH NAMED
EXCEPTIONS (none blocking).

**The foundation does not yet resolve any player's actual jurisdiction** —
the one producible basis (`tenant_licence`) has no application write path,
so every player-scoped resolution returns `unresolved(no_signal)` by
design. This means Bonus deposit/cashback sweep issuance remains blocked,
and no jurisdiction-based regulatory enforcement capability can be claimed,
until the human decisions below are made.

### Decisions/input needed from the human before any further work

1. **Six new candidate Human Decision Register items were opened this
   stage** (`docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`,
   HDR-J-1 through HDR-J-6). **HDR-J-3 is the single highest-leverage
   item** — whether the platform may collect a player residence/location/
   nationality attribute at all, and under what lawful basis/retention
   rule. Until it (and HDR-J-1) is answered, the resolver structurally
   cannot resolve any player's jurisdiction, and Bonus issuance stays
   blocked. HDR-J-5 (BYOL) is registered but explicitly not urgent — no
   BYOL tenant exists yet.
2. **Authorize (or not) the next stage** — options include: answering one
   or more of the six new HDR items to unblock the resolver's actual
   capability; closing the named, non-blocking carried-debt items (see
   report §22/§24); resuming Bonus Engine work now that its remaining
   `ChangeOperation` posting shapes are specified; or Stage 6A (Back
   Office MVP).
3. Wave 4, CRM, Affiliate, Gamification, sportsbook, Retail/POS, and Back
   Office/Partner Console/B2C frontend implementation remain **NOT
   authorized**.
4. All four pre-existing Human Decision Register items (G-2,
   `OpenBetSelfExclusionPolicy` default, mixed/bonus-funded cashout policy,
   FD-1) plus Wave 3's Grant-cancellation-after-conversion item remain
   unmade — none of this stage's work required, selected, or narrowed one.

---

## Prior stage: Stage 4H-B1 Wave 3 — Bonus Engine Completion, Integration Hardening & Final Financial Gate — CONCLUDED

**Status: READY**, subject to explicitly-open, non-blocking items and one
newly-raised Human Decision Register-adjacent question. Full detail:
`docs/governance/wave-3-report.md`. `docs/progress.md`'s "Stage 4H-B1 Wave
3" entry has the running narrative.

The human authorized completion of the Bonus Engine's remaining Wave 2
scope plus integration hardening: deposit/reload/cashback/expiry event
consumption (real sweep jobs, not just the mechanism), four-eyes
application wiring for 4 of 7 `ChangeOperation` types, new HTTP/API admin
surfaces (API only, no Back Office UI), and a mandated re-test of the
SEC-W15-02/CRM-decomposition vector class through every Bonus surface.
11 specialist phases (reconnaissance → ledger-finance → backend →
bonus-engine → risk → identity-compliance → security → casino →
sportsbook → qa → architect → ledger-finance-final) plus one
product-owner-proxy dispatch. Twelve real defects found across the review
chain, all closed with proven regression tests, none self-certified. The
branch was interrupted by a container restart twice mid-Wave; both times
the surviving code was independently re-verified before being trusted and
committed — no work was lost or discarded uninvestigated.

### Decisions/input needed from the human before any further work

1. **New item raised this Wave**: whether this platform may ever create a
   receivable from a customer (clawing back real cash from an
   already-`converted` Grant's cancellation) — a legal/commercial question
   ledger-finance explicitly declined to answer unilaterally. Blocks only
   `grant_cancel_completed`'s extension to `converted`-status Grants; does
   not block anything already shipped.
2. **Authorize (or not) the next stage** — options include: closing the
   remaining named, non-blocking items (the 3 unwired `ChangeOperation`
   types now that ledger-finance has specified their posting shapes; the
   bulk-job HTTP-execute completeness gap, F3; the platform-wide
   jurisdiction-resolver gap that currently denies all three sweeps'
   actual issuance); Stage 6A (Back Office MVP); or a further Bonus Engine
   wave.
3. Wave 4, CRM, Affiliate, Gamification, sportsbook, Retail/POS, and
   Back Office/Partner Console/B2C frontend implementation remain **NOT
   authorized**.
4. The four pre-existing Human Decision Register items (G-2,
   `OpenBetSelfExclusionPolicy` default, mixed/bonus-funded cashout
   policy, FD-1) remain unmade — none of this Wave's work required,
   selected, or narrowed one.
