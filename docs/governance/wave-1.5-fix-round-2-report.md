# Stage 4H-B1, Wave 1.5 — Fix Round 2 + Product Surfaces Roadmap Gate — Final Report

**Status: READY for Wave 2 authorization, subject to the residuals in §12.**

This report is the Master Orchestrator's synthesis of "STAGE 4H-B1 — WAVE 1.5
FIX ROUND 2 — FINAL FINANCIAL/SECURITY CLOSURE + PRODUCT SURFACE ROADMAP
GATE." It covers: eight Phase-1 authorship dispatches, ten Phase-2
independent re-verification dispatches, and six further closing-pass
dispatches (fixing what Phase 2 found), for a total of 24 specialist
dispatches this round. No production code, migration, or route was written
at any point — every artifact discussed below is `docs/architecture/*`,
`docs/security/*`, or `docs/testing/*` design text, exactly as required.

## 0. Git state

Branch `claude/focused-wright-jw88w9`, HEAD `61a203d`, working tree clean,
fully pushed. Commits this round, in order:

`b119c6f` (frontend, B2C roadmap) · `ad5313d` (backoffice, Back
Office/Partner Console roadmap) · `7fe8143` (security, Round 2 fixes) ·
`d59d748` (architect, EOI redesign + Product Surfaces skeleton) ·
`48a3a2a` (ledger-finance, G-2 holding-representation decision) ·
`a1e8208` (casino, LF-18 fix) · `0c6ff60` (bonus-engine, adopts
ledger/casino contract + SEP-1 + EOI) · `3c1c976` (Phase 2 fixes:
identity-compliance/code-reviewer/architect/security/qa) · `081b5c5`
(architect, NEW-7 + four-eyes ratification) · `6745e10` (security, SEP-1
fail-closed fix, part 2 + REQ-SEP-STAFF-1) · `985d8cc` (ledger-finance,
ACTION_REFORFEIT resolution) · `04e200f` (casino, hold-capture timing +
`RecheckGrantExposure` seam) · `d3a7c39` (bonus-engine, seam adoption +
citation fixes) · `61a203d` (architect, final composition certification +
drift cleanup).

No `internal/bonus`, `internal/crm`, `internal/affiliate`,
`internal/gamification`, or `internal/economicop` package exists. No
migration beyond `0049` exists (migration `0050` and `0055` are named,
disclosed, **not-yet-created** design references, correctly labeled as
such everywhere they appear). No UI code, framework file, or `package.json`
was created for Back Office, Partner Console, or B2C frontend.

No Human Decision Register item (G-2, `OpenBetSelfExclusionPolicy`,
sportsbook mixed-funded cashout policy, FD-1) was selected, narrowed, or
defaulted at any point this round. This was independently re-confirmed by
every one of the 24 dispatches, and specifically audited by
`product-owner-proxy`.

---

## 1. Original P0 → resolution matrix

| Finding | Round 1 claim | This round | Final status |
|---|---|---|---|
| **LF-2** | Closed (mechanism), no buildable shape | `ledger-finance` decided the `player_bonus_held` disjoint account + `bonus_held_dispositions` schema (48a3a2a); `casino` and `bonus-engine` adopted it exactly (a1e8208, 0c6ff60), independently certified field-for-field by `ledger-finance`'s own re-verification; the `ACTION_REFORFEIT` posting-sequence contradiction this surfaced was resolved (985d8cc) and the hold-capture-timing question resolved (04e200f); `architect` certifies the full three-document composition consistent (61a203d) after fixing residual text drift | **CLOSED**, design-complete and internally consistent |
| **SEC-W15-01** | Closed, but resolver non-reflexive (fail-open + deadlock) | `code-reviewer` closed the reflexive-ancestor-closure bug directly in doc 32 (3c1c976, disclosed substitution since `architect` did not touch the owning document); `security` independently certified the fix content correct | **CLOSED** |
| **SEC-W15-02** | Closed ("adopting N2.4"), actually fully open on Bonus's enforcement side | `architect` redesigned `EconomicOperationIdentity`'s budget mechanism to genuinely bound the decomposition attack (root-subtree-scoped consumption, monotone recipient ceiling, canonical lock ordering — d59d748); `bonus-engine` implemented the required entry-check/lock-ordering adoption at every grant-causing surface, explicitly retiring the exact unguarded sentence from Round 1 (0c6ff60); independently re-verified as CLOSED by `security`, `code-reviewer`, and `architect` | **CLOSED** |
| **SEC-W15-03** | Partially closed (2 of 5 enforcement points) | `bonus-engine` adopted REQ-SEP-BONUS-1/2/3/4 at all remaining points, with the new `bonus_held_disposition:resolve` permission kept structurally separate (0c6ff60); independently certified correct by `security` | **CLOSED** |

---

## 2. New P0-severity findings (surfaced by Phase 2 of the prior gate) → resolution matrix

| Finding | This round | Final status |
|---|---|---|
| **LF-18** (casino lock-release, reachable ledger-invariant violation) | `casino` rewrote the destination-resolution query to the net-outstanding (variant-2) shape and added a sixth outcome branch; `ledger-finance` independently re-walked the exact original exploit scenario against the fix and confirmed it aborts correctly | **CLOSED** |
| **REQ-SEP-BONUS-4** (HeldDispositionRecord resolution was an ungated self-dealing surface) | `bonus-engine` adopted it as a dedicated `SEP-1` enforcement point with its own permission, four-eyes at the ratified threshold (see §7); `security` independently certified adoption correct | **CLOSED** |
| **Held-win-rollback state-transition gap** | `ledger-finance` closed the still-held case with a guarded compare-and-swap on `bonus_held_dispositions`; `casino` implemented it in `postRollback`; independently proven safe under three interleavings by `ledger-finance`'s re-verification | **CLOSED** (the narrower "rollback of an already-resolved disposition" sub-case remains open — see §12, LF-10) |
| **RK-W15P2-1** (`SEP-1` fails open on partial RLS read) | `security` added the cardinality assertion + tenant-scope self-proof (first pass); `risk`'s independent re-verification found the fix's `CASE` had no fail-closed default and silently no-op'd for the affiliate ancestor-closure resolver shape; `security` fixed this in a second pass (explicit PL/pgSQL `IF/ELSIF/ELSE RAISE`, a new `ancestor_closure` resolver shape with structural totality via `SECURITY DEFINER` + a subtree-scope self-proof) | **CLOSED**, with one routed, unconfirmed dependency (see §12) |

---

## 3. Consolidated P1 closure matrix (this round's fixes; Wave 1.5's original ~20 P1s remain tracked in `task-registry.md`)

- **NEW-2** (doc 34's budget projection didn't bound the decomposition attack) — CLOSED; re-derived by `code-reviewer` against the corrected root-subtree-scoped SQL and found genuinely sound, not merely asserted.
- **NEW-6** (affiliate non-reflexive resolver) — CLOSED (§1).
- **NEW-7** (doc 34 single-shot vs. standing-authorization approval-consumption ambiguity, sharpened by the NEW-2 fix) — CLOSED; `architect` split the `operation_type` mint list into declared single-consumption vs. standing-authorization shapes with distinct state-machine semantics (081b5c5).
- **`ACTION_REFORFEIT` posting-sequence contradiction** (architect's Phase-2 finding: relocated, not resolved, between ledger-finance/casino and bonus-engine) — CLOSED (985d8cc, 04e200f, d3a7c39, verified 61a203d).
- **Missing Grant-status-finalization seam** (casino's Phase-2 finding: no named mechanism for non-WIN closing events) — CLOSED via the new `bonusengine.RecheckGrantExposure` seam (04e200f, d3a7c39), signature/semantics verified matching by `architect`.
- **Four-eyes threshold for `bonus_held_disposition:resolve`** (genuine disagreement between `risk` and `security`/`architect`) — RESOLVED: tenant-configurable, above CLAUDE.md's threshold, default 0 — ratified by `architect` in doc 34 §3.1 with `risk`'s dissenting reasoning explicitly recorded (081b5c5), not silently overridden.
- **REQ-SEP-STAFF-1** (identity-compliance's non-sign-off: platform-scoped remediation gap, pointless `platform_admin` gating) — CLOSED; `security` also found and fixed a genuine bug this surfaced (a bare `<>` comparison in `SEP-1` step 4 would have silently passed `platform_admin` actors instead of refusing them — fixed to `IS DISTINCT FROM`) (6745e10).
- **RK-W15P2-2/3/4/5/6/7/8** (recipient-ceiling netting, lock-ordering ambiguity, undeclared-consumption self-defense, cashback value-unknown-at-execution, non-NULL-actor constraint, denial-audit mechanism, missing EOI mint-list entry) — all independently re-verified **CLOSED** by `risk` against `architect`'s and `security`'s Round 2 fixes, each checked against the actual cited source pattern (not merely a plausible-looking analogy).
- **DEP-EOI-4** (no derivation from Risk's `Operation` enum) — CLOSED, confirmed by `risk` with three binding conditions (no derivation ever, vocabulary-disjointness test, negative-control concurrency test) all present in doc 34.
- **Casino's full state-machine proof** (Bet→Hold→Win→Rollback→late Win→duplicate Win→duplicate Rollback→concurrent Win/Rollback) — delivered in `08-casino-integration-architecture.md` §16.18/§16.19, verified balanced at every step by `ledger-finance`'s independent re-derivation of two scenarios and confirmed compositionally sound by `architect`'s final certification.

---

## 4. G-2 technical boundary — preserved, not selected

Every dispatch this round reconfirms: the `HeldDispositionRecord`/`bonus_held_dispositions` mechanism captures a value-creating credit into a policy-neutral, ledger-balanced, tenant-and-asset-scoped, idempotent, replay-resistant holding account (`player_bonus_held`) and leaves its final disposition (`ACTION_REFORFEIT` / `ACTION_ROUTE_TO_CASH` / `ACTION_HOLD_FOR_REVIEW`) entirely to a future human-supplied G-2 answer. No mechanism fixed this round — including the newly-named `RecheckGrantExposure` seam, the corrected `ACTION_REFORFEIT` posting shape, or the unconditional hold-capture timing — selects, narrows, or defaults which G-2 answer applies. `OpenBetSelfExclusionPolicy`, the sportsbook mixed-funded cashout policy, and FD-1 remain equally untouched.

---

## 5. Actor/subject authorization model (`SEP-1`)

Final state: one reusable actor≠subject/beneficiary invariant, enforced via a `BEFORE INSERT/UPDATE` trigger family, now with: a step-0 tenant-scope self-proof (ported from `internal/risk`'s `verifyConnectionScope`), an explicit PL/pgSQL fail-closed cardinality assertion covering three declared resolver shapes (`single_subject`, `pinned_set`, `ancestor_closure`), a real DB `CHECK` constraint (not a pre-deploy query) for the non-NULL-actor precondition with a corrected `platform_admin` exemption, and a separately-transacted denial-audit mechanism modeled on ADR 0031 §39's precedent. Adopted at all five Bonus enforcement points (REQ-SEP-BONUS-1/2/3/4), CRM's `offer_request` targeting, and Affiliate's approval points (with the reflexive-ancestor-closure correction). Full detail: `docs/security/security-architecture.md` §W15.1.

---

## 6. `EconomicOperationIdentity` — corrected design

Final state: root-subtree-scoped budget consumption (via `root_operation_id`, not direct-parent-scoped), a monotone `COUNT(DISTINCT subject_ref)` recipient ceiling that compensation/clawback rows never decrease, a canonical lock-ordering rule (non-locking entry check → unmodified Risk gate chain, including Risk's own advisory lock → locking consume on the root row, exactly once, strictly after the gate chain), a declared-consumption-row-shape requirement modeled on `internal/risk`'s `ErrUnrecognizedCumulativeLeg` pattern, conservative-maximum consumption for value-unknown-at-execution operations (cashback), an explicit single-consumption-vs-standing-authorization split for approval-state semantics, and a `bonus_held_disposition_resolution` mint-list entry with its four-eyes threshold formally ratified (dissent recorded). Full detail: `docs/architecture/34-economic-operation-identity.md`.

---

## 7. Segmentation, CRM, Affiliate architecture — status

- **Segmentation (doc 30)**: untouched this round — correctly out of scope (Fix Round 2 was scoped to the financial/security P0s and Product Surfaces, not Segmentation). NEW-1 (`InclusionSafety`'s `Not` rule not polarity-aware), NEW-3 (unpinned `member_of`), and NEW-4 (`Resolve` two/three-value contradiction) remain open, correctly still tracked, not silently dropped.
- **CRM (doc 31)**: untouched this round; no CRM implementation was authorized or attempted. Its existing interface to Bonus (via the corrected `EconomicOperationIdentity`) was independently re-verified consistent by `security`'s certification.
- **Affiliate (doc 32)**: fixed this round (reflexive-ancestor-closure, §1/§3 above). `security`'s new `ancestor_closure` `SEP-1` resolver shape depends on an `agentnetwork` parent/child tenant-boundary invariant not yet confirmed by `architect` — see §12.

---

## 8. Bonus targeting/catalogue/segmentation-consumption re-verification

`bonus-engine`'s Round 2 dispatch (item D) confirmed the existing catalogue (deposit/reload/cashback/coupon/manual/promo-code/bulk/etc.), targeting modes (single/list/segment/multi-segment/bulk/CRM-driven/etc.), and the Bonus Suggestion lifecycle (Generated→Review→Edit/Approve/Reject→Activate, never itself a Grant) are unaffected in mechanism by this round's G-2/`SEP-1`/EOI fixes — targeting is now additionally EOI-gated per §N2.4a, with no other change to catalogue mechanics.

---

## 9. Product Surfaces architecture — roadmap gate deliverables

Per the directive's explicit "do not build the UI now, create a formal roadmap" instruction:

- **`docs/architecture/35-product-surfaces-roadmap.md`** (`architect`): follows the project's existing stage-numbering convention (Stage 4 extended incrementally) by sub-staging the already-reserved **Stage 6** (per `MASTER-BUILD-PROMPT.md`'s own "B2C frontend + back office + partner console + reporting/BI" reservation). Defines **Stage 6A** (Back Office MVP), **Stage 6B** (Partner Console MVP, blocked on a not-yet-built partner-scoped RBAC role), **Stage 6C** (B2C Brand Frontend MVP), **Stage 6D** (Retail/POS, explicitly `RECOMMENDATION`-only, deferred last). The Surface Architecture Gate itself is folded into this current dispatch, with reasoning given. Full domain-dependency table (15 domains, each marked IMPLEMENTED/NOT IMPLEMENTED against the real `internal/` tree) and the directive's literal `Core Platform → APIs → Back Office → Partner Console → B2C → Retail/POS` dependency graph, with an honest note that this ordering reflects exposure/trust-boundary sequencing, not commercial launch priority (doc 14's existing, human-approved priority still has B2C first commercially).
- **`docs/architecture/36-backoffice-and-partner-console-architecture.md`** (`backoffice`): Back Office designed as an operational control surface (queue/worklist-first, not generic CRUD), full 24-section information architecture with each section tagged by real backend status and MVP treatment; MVP scoped to what's buildable against today's implemented domains only. Partner Console defined as a genuinely separate surface, with its first blocking dependency named explicitly (no partner-scoped RBAC role exists yet — `tenant_admin` is unsafe to reuse for an external partner).
- **`docs/architecture/37-b2c-brand-frontend-architecture.md`** (`frontend`): full feature-area breakdown (implemented-backend vs. design-only vs. not-started, honestly mapped against real code), MVP scoped accordingly, SSR-with-partial-hydration technology recommendation with stated rationale (brand/theme/locale/currency must resolve server-side per-request), explicit RG/compliance UI requirements (self-exclusion reachable at a fixed unhidden location, never behind a retention interstitial; no engagement mechanic may share visual priority with an RG control).
- **Explicit answer to "when does Back Office implementation actually start"**: Stage 6A has no unbuilt-domain blocker today; `architect`'s reasoned recommendation is to start it after Stage 4H-B1 (Bonus Engine) reaches resolution, purely to avoid rework on ledger/wallet-shape read views this stage is still actively reshaping — not a hard technical dependency, and the human may authorize starting it in parallel if preferred.
- Non-negotiables restated in all three documents and confirmed by `product-owner-proxy`'s scope audit: the UI never owns financial/business rules; permissions enforced server-side only; no brand-specific forked backend logic.

No UI code, wireframe-as-code, or schema for any deferred feature was written — confirmed clean by `product-owner-proxy`'s explicit scope-creep check.

---

## 10. Specialist reports

All 24 dispatches' full findings are preserved in the committed documents cited throughout this report (`docs/architecture/*`, `docs/security/security-architecture.md`, `docs/testing/testing-strategy.md`) and in this session's own record. Per this project's established convention, individual SubagentHandback transcripts are not separately committed as files.

---

## 11. Test evidence

`docs/testing/testing-strategy.md`'s new Phase 2 Round 2 section (`qa`, commit `3c1c976`) finalizes `G2-HOLD-1/2/3` against the actual `player_bonus_held` design, adds a real PostgreSQL concurrency test for LF-18's exact exploit (`CASINO-LF18-QUERY-RACE-1`, buildable today; `CASINO-LF18-FULL-1`, blocked on Round 2 code), a real RLS adversarial test for `SEP-1`'s cardinality mechanism (`SEP-1-CARDINALITY-RLS-1`, grounded in actual `player_accounts`/`persons` RLS shapes), and the EOI root-subtree-budget race test with the required negative control (`EOI-BUDGET-RACE-1`). All correctly labeled `SPECIFIED, READY TO IMPLEMENT` or `BLOCKED` — nothing claimed as passing, since no code exists to run any of it against.

---

## 12. Remaining P1/P2/P3 findings (explicit, not swept under the READY verdict)

None of these block the READY verdict below — each is either explicitly out of this round's authorized scope, a routed dependency awaiting one further confirmation (not a known defect), or non-blocking.

- **P1 — LF-10** (rollback of an already-*resolved* — not still-held — `bonus_held_dispositions` row). Explicitly open, consistently described as open and routed to `ledger-finance` in all three of `casino`'s, `ledger-finance`'s, and `bonus-engine`'s documents. Not fund-unsafe as currently scoped (no path posts against it without a decision); required before sportsbook's own G-2 generalization, per `sportsbook`'s Phase 2 finding that its market-correction scenario is the same gap.
- **P1 — `SEP-1`'s `ancestor_closure` resolver's dependency on an `agentnetwork` tenant-boundary invariant.** `security` built the resolver assuming `agentnetwork` parent/child edges never cross a tenant boundary but explicitly declined to claim `REQ-SEP-AFF-1` complete pending `architect`'s confirmation of this as an edge-level (not just node-level) invariant. Routed, not yet confirmed — recommended as the first item of any follow-up dispatch.
- **P1/P2 — Segmentation's NEW-1/NEW-3/NEW-4** (Kleene polarity-awareness, unpinned `member_of`, `Resolve` value-contradiction). Untouched this round by design (out of scope), correctly still open.
- **P2 — Sportsbook's partial-settlement/cashout posting-shape gap.** The two-amount (`payoutAmount`/`releasedLockAmount`) seam doesn't map onto sportsbook's three-quantity (stake released / payout / house margin) netted-leg shape. No sportsbook bonus-funded wagering exists or is authorized; this is a named requirement for whenever that future design work is authorized, not a current defect.
- **P3 — `creditKind`'s type/enum is never defined** in either casino's or bonus-engine's document, though the name is used identically in both (`casino`'s own Phase 2 finding, self-disclosed as unresolved).
- **P3 — EOI's root-row lock is a real throughput ceiling** for a very large (100k-item) campaign, acknowledged but not quantified (`code-reviewer`'s residual observation) — a performance note for implementation time, not a correctness defect.

---

## 13. Human Decision Register confirmation

Re-confirmed, independently, by every one of the 24 dispatches and specifically audited by `product-owner-proxy`: no item on the Human Decision Register (G-2, `OpenBetSelfExclusionPolicy`, mixed/bonus-funded sportsbook cashout policy, FD-1) was selected, narrowed, defaulted, or resolved by any default/timeout/expiry/terminal-state/fallback/cancellation/accounting mechanism this round.

---

## 14. Wave 2 readiness decision

# **READY**, subject to the two routed P1s in §12 (LF-10, the `agentnetwork` tenant-edge confirmation) being resolved before implementation of the specific code paths they gate (bonus-funded-sportsbook and Affiliate `SEP-1` adoption, respectively) — neither blocks the rest of the Bonus Engine's design from being implementation-ready.

Evidence: all four original P0s and all four new P0-severity findings from the prior gate are independently certified closed by reviewers who did not author the fixes they certified. The one genuine composition failure discovered mid-round (the `ACTION_REFORFEIT` posting-sequence contradiction, plus the fix-chain drift it left behind) was traced to root cause, fixed, and re-certified by `architect` with the same rigor as the original finding. Every disagreement between independent reviewers this round (the four-eyes threshold question) was resolved with the dissenting position recorded, not silently overridden. No control was weakened to manufacture this verdict — several fixes (the `SEP-1` cardinality assertion, the `RecheckGrantExposure` seam, the reflexive-ancestor-closure) made the design measurably stricter than before.

---

## 15. Explicit stop

Per the authorizing directive: **this concludes Fix Round 2. Wave 2, bonus-funded wagering implementation, CRM implementation, Affiliate implementation, Gamification implementation, Back Office frontend implementation, Partner Console frontend implementation, and B2C frontend implementation all remain unauthorized.** No further work proceeds without a new, explicit human directive.
