# Stage 4H-B1 Wave 3 — Bonus Engine Completion, Integration Hardening & Final Financial Gate

**Status: READY** (with named, disclosed, non-blocking exceptions — see §20/§25).
**Branch:** `claude/focused-wright-jw88w9`. **Final commit:** `cd6ee62`.

This report is the mandated Final Gate Report for Stage 4H-B1 Wave 3, per the
governing human directive "STAGE 4H-B1 — WAVE 3 — BONUS ENGINE COMPLETION,
INTEGRATION HARDENING & FINAL FINANCIAL GATE". It supersedes nothing in
`wave-2-report.md`; it records what changed since that report's READY verdict.

---

## 1. Executive verdict

**READY** to consider Wave 3 concluded, subject to:
- three explicitly disclosed, fail-closed, non-blocking gaps carried forward
  (the platform-wide jurisdiction-resolver gap; 3 of 7 `ChangeOperation` types
  still unwired; the bulk-job HTTP-execute completeness gap, F3); and
- one newly-raised item for human decision (§21).

No P0 is open. No unresolved P1 affects Bonus correctness or security today.
Every defect found during this Wave's 11 phases was fixed and independently
re-verified by a phase other than the one that introduced or found it, or
was recorded as a named, gated, non-live risk with an explicit owner.

## 2. Mandatory first action — reconnaissance

Before any code was written, `architect` produced
`docs/governance/wave-3-reconnaissance.md`: a from-code (not from-docs)
reconstruction of the Bonus state machine and accounting flows as Wave 2 had
actually left them, an ~32-item gap list (9 IMPLEMENTED / 19 PARTIAL / 4 NOT),
and a recommended Phase A-E dependency map. All subsequent phases treated this
as the scoping baseline.

## 3. Phase sequence actually run

1. **ledger-finance** — deposit/wagering-contribution/cashback event-consumption
   contracts, `ledger-accounting-model.md` §7.18 (`b617f75`).
2. **backend** — watermark schema (migrations 0068/0069), `bonus_grants.expires_at`
   (migration 0070) (`9d857dc`).
3. **bonus-engine** — implementation of items A-G (`94257ec` + 9 commits ending
   `f92eb39`). Interrupted once by a container restart mid-flight; recovered
   without loss (see §24).
4. **risk** — found and fixed 2 live fail-opens (`6166e1d`).
5. **identity-compliance** — RG/KYC review, KYC-tier ruling, multi-account
   detector (`1428da7`).
6. **security** — the mandated SEC-W15-02/CRM-decomposition re-test; found and
   fixed 5 defects across two dispatches (interrupted once by a second
   container restart, recovered without loss — see §24) (`ab8ee70` through
   `65d83d1`).
7. **casino** — composition/lock-ordering review of `postBet`'s new wiring;
   clean, one hardening test added (`fb829e1`).
8. **sportsbook** — boundary-only review; clean, no code changes.
9. **qa** — full validation floor, coverage-gap audit, migration round-trip,
   2 more defects found and fixed (`da33c5e` through `8c6ab7a`).
10. **architect** — final cross-domain composition certification; found and
    fixed 2 more defects, disposed of 8 routed items, recorded 3 new named
    findings (`5af05d2` through `c48a6ab`).
11. **ledger-finance** — final independent financial certification; found and
    fixed 1 more defect (a real clock-source bug), specified 2 posting shapes,
    raised 1 new escalation (`8c09e91` through `cd6ee62`).

Plus one out-of-band dispatch: **product-owner-proxy** confirmed architect's
D3 commercial-default recommendation (`35dd9a7`).

**Total defects found and fixed this Wave: 12**, across risk (2), security
(5), qa (2), architect (2), ledger-finance (1) — every one proven by a
regression test shown to fail pre-fix and pass post-fix, per this project's
established discipline. Zero were self-certified; every fix was reviewed
independently by at least the next phase in the sequence.

## 4. What was implemented (vs. the reconnaissance's gap list)

- **Deposit/reload event-consumption sweep** — durable, tenant-scoped,
  `(posted_at, id)`-ordered, watermarked. `IMPLEMENTED` (mechanism);
  `PROVIDER/PLATFORM DEPENDENT` for actual issuance, gated on the pre-existing
  platform-wide jurisdiction-resolver gap (denies fail-closed at
  AssetAuthorization until that gap closes elsewhere — not a Wave 3 defect).
- **Cash-funded wagering-contribution event consumption** — `IMPLEMENTED`.
  `postBet` now records qualifying stake toward a Grant's wagering target in
  the same transaction as the bet's own `player_cash` posting. Bonus-funded/
  locked-stake wagering (using locked bonus balance as an actual casino
  stake) remains explicitly **NOT IMPLEMENTED** — a disclosed orchestrator
  scope decision, gated on the same undecided jurisdiction/settlement-window
  dependency that already blocks casino's own settlement-timeout sweep.
- **Cashback scheduler** — `IMPLEMENTED`. Same jurisdiction-gap caveat as
  deposit sweep for actual issuance. Opt-in enrollment is out of scope
  (non-opt-in cashback only); disclosed as a named simplification.
- **Expiry sweep** — `IMPLEMENTED`. Terminates via the existing `TerminateGrant`
  machinery; no parallel path.
- **Four-eyes application-level wiring** — `PARTIALLY IMPLEMENTED`: 4 of 7
  `ChangeOperation` types wired (`campaign_activate`, `offer_publish`,
  `manual_grant_issue`, `bulk_job_execute`). The remaining 3
  (`bonus_adjustment_write`, `grant_forced_conversion`, `grant_cancel_completed`)
  are **NOT IMPLEMENTED**, correctly blocked on posting-shape/design decisions
  — now specified by ledger-finance (§7.19, DESIGN ONLY) and architect
  (ADR 0040 RC-1/RC-2/RC-3) for whoever implements them next.
- **HTTP/API admin surfaces** — `IMPLEMENTED`. 11 new routes: four-eyes
  filing/approval, EOI minting, campaign activate, offer create/version/
  publish, manual-grant issue+activate, bulk-job create+execute. API only —
  no Back Office UI was built, per the directive's explicit constraint.
  One completeness gap: bulk-job HTTP execute always fails (F3, below),
  confirmed fail-closed, not yet fixed (bonus-engine's to close).
- **Multi-account abuse detector** — `PARTIALLY IMPLEMENTED` by design: a
  Bonus-domain-only `person_id` cross-account signal for first-deposit-only
  eligibility, audit-only, never denies. Device/fingerprint correlation
  remains explicitly out of scope (no such signal source exists on this
  platform yet).
- **KYC-tier taxonomy** — ruled **NOT NEEDED FOR THIS WAVE** (nothing shipped
  reads or is blocked by a KYC-tier concept); the underlying gap
  (`kyc_rg_level_required`/`player_accounts.kyc_tier` both dormant) is real
  and is now scheduled as a future cross-domain dispatch
  (identity-compliance → architect → bonus-engine), recorded in
  `docs/architecture/13-dependency-map-and-risk-register.md`.

## 5. State machine

Unchanged from Wave 2's Grant lifecycle (`issued → activated → in_progress →
completed/cancelled/expired/reversed`, plus `pending_settlement` for the G-2
AOE mechanism). Wave 3 adds exactly one new field-driven transition:
`bonus_grants.expires_at` (migration 0070, write-once) now actually gets
populated at activation and is consumed by the new expiry sweep to drive
`activated`/`in_progress → cancelled` via the existing `TerminateGrant`
path — no new status value, no new transition table.

## 6. Accounting flows / posting shapes

**No new ledger posting shape was introduced this Wave.** Confirmed
independently by ledger-finance's Phase 11 certification against §7.18.5's
own "no new posting shape required" statement: the deposit sweep, cashback
scheduler, and expiry sweep all post through the pre-existing `ActivateGrant`
→ `ledger.Post` / `TerminateGrant` paths; the cash-funded wagering-contribution
mechanic posts nothing at all (it is a progress-tracking measure, not a value
movement). Two future posting shapes were specified (not implemented) for
`bonus_adjustment_write` and `grant_cancel_completed` — see
`ledger-accounting-model.md` §7.19.

## 7. Conversion flows

Unchanged. `ConvertGrant` was not modified this Wave and — verified
independently by both architect and ledger-finance in Phase 10/11 — has zero
non-test callers today, which is exactly why the newly-identified latent
lock-order risk (`DR-4HB1W3-ARCH-01`, §17) is gated rather than live.

## 8. Wagering/progress handling

Cash-funded wagering-contribution recording (`RecordCashFundedWageringContribution`)
reads `staked_bonus_amount` from the bet's own posted `player_cash` debit,
never from a caller's claim — the same "read from the ledger" invariant the
bonus-funded case already used, generalized to a different account. Two real
fail-opens found and fixed by risk (cross-asset attribution; nil-target read
as satisfied). Multi-Grant-per-player ambiguity is a genuine fail-closed,
audited no-op — verified independently by risk, identity-compliance, and
casino. Idempotency hardened via `ON CONFLICT DO NOTHING` +
`CreateWageringProgressIdempotent`, verified under real concurrent load by
casino's Phase 7 stress test.

## 9. Cashback handling

Independent-rounding/no-residue rule (DS-1/DS-2: round-half-up, ties away
from zero, round once at the final boundary) re-verified end-to-end against
a real posting by ledger-finance's Phase 11 certification test (a tie case:
333 minor units @ 5000bp → 166.5 → posted 167, exactly as specified). Window
settlement uses the database's own clock (fixed this Wave — see
`DR-4HB1W3-LF-01`, §16) rather than the API host's clock, closing a silent
platform-favoring underpayment risk under clock skew.

## 10. Bulk-operation lineage

`bulk_job_execute`'s four-eyes payload now binds offer version, asset,
per-recipient amount, and a stable recipient-set hash (security's Phase 6
fix) — closing a payload-substitution/pagination-laundering vector where a
modest approved bulk grant could previously execute an arbitrarily larger
one. The HTTP path to actually execute an HTTP-created bulk job is
confirmed **fail-closed but not yet functional** (F3): `newCreateBulkGrantJobHandler`
never sets `parent_operation_id`, so execution always 500s with zero partial
state (verified by an explicit test) — a completeness gap for bonus-engine,
not a security defect.

## 11. Actor/subject controls (SEP-1)

The directive's explicit mandate — "re-test the previously closed SEC-W15-02
/ CRM-decomposition class of vectors through every Bonus surface" — was
carried out as a full 4-vector-shape × 5-surface matrix (20 cells), all
passing at final HEAD. Two genuine defects were found and fixed in this
pass: (1) `ConsumeRootBudget` never enforced subject-set containment for
`single_subject` scope, allowing a root minted/approved for player A to
authorize a grant to player B; (2) the EOI-minting endpoint allowed an
unbounded-budget root on a single actor's authority. A further structural
version of fix (2) was applied by architect in Phase 10, moving the bound
enforcement from the HTTP handler into `bonus.MintRootOperation` itself so
no future non-HTTP caller can reopen it.

## 12. Risk / RG / KYC / AssetAuthorization integration

All new value-movement paths run the full T.1 gate chain
(AssetAuthorization → RG → Risk) at the moment of actual value movement, not
just at request-filing time — verified independently by identity-compliance
(Phase 5) and re-confirmed by security (Phase 6) for the four-eyes-approved
paths specifically (an approval unlocks the four-eyes control; it never
substitutes for the gate). Self-excluded/Risk-denied players cannot receive
or convert prohibited value through any new surface, direct or indirect.
KYC is not itself a Bonus gate step (confirmed unchanged from Wave 2 — Bonus's
gate chain is AssetAuthorization/RG/Risk only); no new bypass of the
platform's own (separately, pre-existingly incomplete) KYC-at-withdrawal
enforcement was introduced.

## 13. Casino integration

`postBet`'s new wiring is strictly downstream of the bet's own RG/Risk
checks and its own `casino_bet` ledger posting; a failure in the new code
rolls back the whole transaction (HR-10's deliberate same-transaction
design) rather than corrupting or silently swallowing the bet's own
response. Idempotent under real concurrent redelivery (proven by a new
10-way concurrency stress test). Does not touch bonus-funded/locked-stake
staking anywhere (confirmed by grep + code read). Does not interact with or
destabilize the pre-existing G-2 held-disposition seams
(`postWin`/`postRollback`'s `ResolveTerminalGrantCredit`/
`RecheckGrantExposure`) — separate call trees, no shared lock, no shared
attribution-table conflict.

## 14. Provider-native coexistence

Unchanged from Wave 2 — no provider-native free-round/bonus mechanism was
touched or affected by any Wave 3 work.

## 15. Idempotency

Re-verified across every new write path: deposit sweep's grant issuance
(existing `bonus_grants` uniqueness), cashback scheduler's (`trigger_reference`
uniqueness), wagering-progress `ON CONFLICT DO NOTHING`, the four-eyes
single-consume transition (genuinely DB-enforced via `SELECT...FOR UPDATE`
then a guarded `UPDATE`), EOI minting's `(tenant_id, idempotency_key)`
constraint. One narrow, non-live finding recorded (`LF-W3-01`): the
uniqueness constraint backing "never twice for the same trigger event" is
scoped per-Offer-version, not fully unconditional — routed to bonus-engine,
no live exposure today (no RLS `DELETE` path exists that could re-trigger it).

## 16. Concurrency

Every new batch/scheduler mechanism was tested under genuine concurrent
execution (not just sequential redelivery) — deposit sweep, expiry sweep,
and the cross-scheduler interaction between deposit sweep and cashback
scheduler (risk's F1 concern: different advisory-lock namespaces could in
principle deadlock; empirically closed by qa — both use non-blocking
`pg_try_advisory_xact_lock`, which by definition cannot participate in a
wait-for cycle). One genuine defect found and fixed by ledger-finance's
final certification: `DR-4HB1W3-LF-01` — the cashback scheduler and expiry
sweep compared windows against the API host's `time.Now()` while doc
comments falsely claimed a `clock_timestamp()` read; fixed to read the
database's own clock inside each tenant's transaction, closing a silent,
platform-favoring underpayment/early-expiry risk under host/DB clock skew.

## 17. RLS / security

No new tables were added this Wave (all schema landed in Phase 2). Every
new sweep/scheduler/HTTP handler was confirmed to use the established
`WithTenant`-scoped connection pattern exclusively — no `WithPlatformAdmin`
or RLS-bypass connection anywhere in the new code. Client-supplied
`tenant_id` is never trusted (derived only from the verified JWT). One
latent (not live) architectural finding was recorded, not fixed:
`DR-4HB1W3-ARCH-01` — a lock-order inversion between `postBet`'s new
`player_cash`-then-`Grant` acquisition order and every existing Bonus
path's `Grant`-then-`player_cash` order. Verified as a real AB-BA cycle
shape (not the non-blocking try-lock case F1 covers) but currently inert
because `ConvertGrant` and the held-disposition resolution paths have no
way to become concurrently reachable with a bet today (`ConvertGrant` has
zero non-test callers; held-disposition resolution requires a
`player_locked_bonus` debit that nothing in production code ever posts).
Recorded as a binding architectural constraint (HR-21 extended / EOI-20 /
ledger-finance's own HR-26) gating any future work that would make these
paths concurrently reachable.

## 18. Audit

Every new mutating operation writes an audit record. A genuine gap was
found and closed by qa (Phase 9): no test anywhere had actually read
`audit_log` back to confirm a new handler's `audit.Record` call fires — new
coverage added for 8 named audit event types. Two audit events remain
disclosed as untestable today rather than faked (`bonus_grant.manual_issue_activated`
needs a full T.1 HTTP fixture that doesn't exist for any domain yet;
`bulk_grant_job.executed` is structurally unreachable per F3). No sensitive
KYC/document information appears in any new audit metadata (confirmed by
review — Bonus's gate chain doesn't read KYC status at all, so there is
nothing sensitive to leak here).

## 19. Reconciliation

No new production `ledger.Post` call site was added; every new
posting-adjacent path (deposit sweep, cashback scheduler) posts through the
existing `ActivateGrant` → `ledger.Post` route, so the existing
account-agnostic `reconciliation.RunLedgerVsProjection` job automatically
covers them — verified by ledger-finance's Phase 11 certification test,
which asserts `StatusClean` with zero mismatches after real postings and
after replay, not merely structurally.

## 20. Remaining P0 / P1 / P2 / P3

- **P0: none open.**
- **P1: none open that affects Bonus correctness or security today.**
- **P2 (named, disclosed, gated, non-blocking)**:
  - `DR-4HB1W3-ARCH-01` — latent lock-order inversion (§17), gated on
    `ConvertGrant`/held-disposition resolution becoming concurrently
    reachable with `postBet`. Owner: whoever builds bonus-funded staking or
    wires a conversion route next.
  - `DEP-EOI-8`/ARCH-02 — `bonus_campaign_activation`'s standing-authorization
    EOI type has no mint point while its effecting writes (the new sweeps)
    are now live; inert today only because sweep issuance denies at the
    jurisdiction gate. Owner: whoever closes the jurisdiction gap, or
    security co-signing an alternative bound first.
  - `LF-W3-01` — per-Offer-version idempotency scoping on deposit/cashback
    triggers, no live exposure. Owner: bonus-engine.
  - F3 — bulk-job HTTP execute always fails closed but is non-functional.
    Owner: bonus-engine.
  - 3 of 7 `ChangeOperation` types unwired, posting shapes now specified.
    Owner: bonus-engine, per RC-1/RC-2/RC-3.
- **P3 (cosmetic/low, disclosed)**: `LF-W3-03` (unreachable-today fail-open
  asymmetry in cashback net-loss classification), `LF-W3-04` (inherent,
  bounded over-credit on a late-arriving rollback after window settlement),
  thin EOI-mint audit metadata, `ResolveContributionWeightBP`'s clamp-plus-
  audit-signal design (ruled, not a defect).

## 21. Human Decision Register — explicitly unchanged, plus one new item raised

**Unchanged**: G-2, `OpenBetSelfExclusionPolicy` default, mixed/bonus-funded
sportsbook cashout policy, and FD-1 were not selected, narrowed, or defaulted
by any mechanism this Wave. Confirmed independently by sportsbook's Phase 8
boundary review (zero references to FD-1/`OpenBetSelfExclusionPolicy`
anywhere in the Wave 3 diff) and by every phase's own compliance with this
constraint.

**New item raised, for human decision** (from ledger-finance's Phase 11
certification, not resolved by any specialist): **cancelling an
already-`converted` Grant is deliberately left unspecified.** Once a Grant
converts, its value is in `player_cash` — fungible, possibly already staked
or withdrawn. A future `grant_cancel_completed`-style operation that admits
`converted` Grants would need to claw back real player cash, which can drive
a balance negative — i.e., it would create a **receivable from a customer**.
Whether this platform ever creates player receivables, under which
jurisdictions' consumer-protection rules, and what recourse exists when a
balance is insufficient, is a legal/commercial question, not an engineering
one. No specialist attempted to answer it; `grant_cancel_completed`'s
current specification (§7.19) is deliberately scoped to admit only
`completed`-status Grants (pre-conversion), leaving the `converted` case
open pending this decision.

## 22. Carried dependencies

- **LF-10** — re-confirmed by both architect and ledger-finance in Phases
  10/11 as untouched and orthogonal to Wave 3 (`bonus_settlement.go`'s
  `postRollbackHeldWin` unmodified, still fails closed). Remains open,
  correctly routed to its own future dedicated dispatch.
- **`agentnetwork`/SEP-1 tenant-edge item** — re-confirmed by Wave 3
  reconnaissance as NOT a blocker (Bonus uses only `single_subject`/
  `pinned_set` SEP-1 resolver shapes; the `ancestor_closure` shape is
  Affiliate-only and `agentnetwork` doesn't exist in the repo).
- **Platform-wide jurisdiction-resolver gap** — pre-existing, shared with
  casino, denies all three new sweeps' actual grant issuance fail-closed.
  Not a Wave 3 defect; not fixed by Wave 3 (correctly out of scope).
- **KYC-tier taxonomy gap** — scheduled as a future cross-domain dispatch
  (§4 above).
- **Multi-account abuse detector** — Bonus-domain signal implemented;
  cross-domain device/fingerprint correlation remains a documented future
  item with no signal source yet on this platform.

## 23. Tests and results

Full validation floor run repeatedly across all 11 phases, final
confirmation by qa (Phase 9) and re-confirmed by ledger-finance (Phase 11):
`go build ./...`, `go vet ./...` (including `-tags=integration`),
`gofmt -l .` (empty every time), full unit suite, full integration suite
(`-tags=integration`, real Postgres 16), full integration suite `-race`,
migration up/down round-trip for migrations 0068-0070 (explicitly
re-verified this Wave — clean, no data loss), RLS adversarial tests,
financial-invariant tests (including a purpose-built test proving `SUM(DEBITS)
== SUM(CREDITS)` against a real, non-trivial successful posting rather than
relying on "zero postings occur" as a trivial proof), idempotency tests
(retry, concurrent delivery, payload-mismatch, redelivery), concurrency
tests (including genuine multi-goroutine overlap, not just sequential
redelivery), API/authorization/audit tests for every new endpoint. Zero
skipped financial/security tests without explicit written justification
(qa's Phase 9 skip/TODO audit found zero `t.Skip` calls and two weak
assertions, both strengthened). All 26 repo packages green at final HEAD.

## 24. Container-restart resilience (novel this Wave)

This Wave was interrupted by a container restart twice — once mid-Phase-3
(bonus-engine), once mid-Phase-6 (security's first attempt). Both times,
the interrupted dispatch's own final report was lost, but its code survived
uncommitted on disk. In both cases the orchestrator independently
investigated the surviving diff before trusting it (read the full diff,
confirmed internal consistency, ran the complete validation floor
including `-race` and the full repo suite from a cold/recovered Postgres
cluster), then committed it as a disclosed "(partial, recovered)" checkpoint
before re-dispatching a fresh phase to complete the remaining scope with
explicit incremental-commit-and-push instructions. No work was lost or
silently discarded; nothing was trusted without independent re-verification.

## 25. Explicit statement of what was NOT implemented

- Bonus-funded/locked-stake casino wagering (`postBet`'s locked-bonus-as-stake
  leg) — disclosed orchestrator scope decision, gated on an undecided
  jurisdiction/settlement-window dependency.
- 3 of 7 `ChangeOperation` four-eyes wirings (`bonus_adjustment_write`,
  `grant_forced_conversion`, `grant_cancel_completed`) — posting shapes now
  specified, application wiring not built.
- Functional bulk-job HTTP execution end-to-end (F3) — fails closed, not
  functional.
- Cross-domain device/fingerprint multi-account correlation.
- Any resolution of the KYC-tier taxonomy gap beyond scheduling it.
- CRM, Affiliate, Gamification, Reward-Orchestrator, real/in-house
  sportsbook, Retail, Back Office/Partner Console/B2C frontend UI, any real
  external provider — all correctly out of scope and untouched, confirmed
  by sportsbook's own explicit boundary review.
- Any Human Decision Register item — all four remain exactly as they were,
  plus the one new item raised in §21.

## 26. Git state

Branch `claude/focused-wright-jw88w9`. Working tree clean. All commits
pushed to `origin/claude/focused-wright-jw88w9`. Final commit before this
report: `cd6ee62`. Full commit sequence this Wave: `61601ad` (reconnaissance)
→ `b617f75` (Phase 1) → `9d857dc` (Phase 2) → `94257ec`...`f92eb39` (Phase 3,
10 commits incl. 1 recovered checkpoint) → `6166e1d` (Phase 4) → `1428da7`
(Phase 5) → `ab8ee70`...`65d83d1` (Phase 6, 6 commits incl. 1 recovered
checkpoint) → `fb829e1` (Phase 7) → (Phase 8, no commit) → `da33c5e`...`8c6ab7a`
(Phase 9, 4 commits) → `5af05d2`...`c48a6ab` (Phase 10, 4 commits) → `35dd9a7`
(product-owner-proxy) → `8c09e91`...`cd6ee62` (Phase 11, 5 commits).

## 27. Overall verdict

**READY.** Wave 3's authorized scope — Bonus Engine completion, integration
hardening, and final financial gate — is complete to the standard this
project has held throughout: every phase's work was independently reviewed
by the next, every genuine defect found was fixed and proven with a
regression test, no control was weakened to manufacture a pass, no Human
Decision Register item was touched, and every remaining gap is named,
owned, and either fail-closed-and-inert or explicitly scheduled. This
verdict certifies Wave 3's own scope; it is not a launch authorization, not
a claim that the complete Bonus/Gamification ecosystem is production-ready
(CRM/Affiliate/Gamification remain unbuilt by design), and not a resolution
of any pending Human Decision Register item.

## 28. Per this Wave's own governing instruction

**Do not proceed to Wave 4 or any other domain after this report. STOP and
await explicit human authorization.**
