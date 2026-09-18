# Stage 4H-B1, Wave 1.5 Fix Wave — Phase 2 Independent Re-Verification Report

**Status: NOT READY. Do not proceed to Wave 2.**

This report is the Master Orchestrator's synthesis of the Phase 2
independent re-verification round authorized by the human directive
"STAGE 4H-B1 — WAVE 1.5 FIX WAVE + INDEPENDENT RE-VERIFICATION." It
covers eleven parallel review dispatches against Phase 1's six fix
commits. No production code, migration, or route was written in Phase 1
or Phase 2 — every artifact discussed below is `docs/architecture/*`,
`docs/security/*`, or `docs/testing/*` design text.

Git state at the close of this round: branch `claude/focused-wright-jw88w9`,
HEAD `08d10e3`, working tree clean. Commits this round:
`3ce48f4` (identity-compliance), `3ed489f` (ledger-finance),
`a831693` (security), `bb2963c` (casino), `e077b6f` (bonus-engine),
`a1f6fd4` (architect) — Phase 1 fixes — and `08d10e3` (qa) — the sole
Phase 2 dispatch that produced a file change; the other ten Phase 2
dispatches (ledger-finance, security, architect, code-reviewer,
bonus-engine, casino, risk, sportsbook, product-owner-proxy,
identity-compliance) were review-only and produced no diff. No
migration exists beyond `0049`. No `internal/economicop`, no
`internal/crm`, no `internal/affiliate`, no `internal/gamification`
package exists in the repository.

No Human Decision Register item (G-2, `OpenBetSelfExclusionPolicy`,
sportsbook mixed-funded cashout policy, FD-1) was selected, narrowed,
or defaulted by any Phase 1 or Phase 2 dispatch. Every reviewer states
this explicitly; the Orchestrator has independently confirmed it by
reading each report's own reasoning, not merely its self-certification.

---

## 1. Verdict on the four original P0s

| Finding | Phase 1 claim | Phase 2 verdict | Evidence |
|---|---|---|---|
| **LF-2** (G-2 silently selected via auto-forfeit) | Closed | **Mechanism CLOSED; implementation NOT READY** | ledger-finance, security, architect, casino, code-reviewer, risk all independently agree the eligibility/disposition split (`NewStakeEligibility` vs. `AOE`/`HeldDisposition`) genuinely removes the auto-`ACTION_REFORFEIT` defect. But the *only* named holding-representation shape (branch 1, reusing `player_locked_bonus`) is independently vetoed as unsafe by ledger-finance (LF-19/LF-20) and architect (double-counts `AOE`, contradicts casino's mandatory lock-release), no shape is currently buildable, `ACTION_REFORFEIT`'s posting sequence is specified two contradictory ways across three sites (architect's P0 #1), and migration `0050`/the Rule B2 generator are hard preconditions currently blocked by HR-9. |
| **SEC-W15-01** (affiliate four-eyes collusion) | Closed | **NOT FULLY CLOSED** | security confirms `AFF-4E-1` + the corrected ancestor-chain `SEP-1` resolver closes the originally-described collusion. code-reviewer (NEW-6) independently found the resolver as literally worded (`B(O)` = strict ancestors, excluding the node itself) has a **fail-open branch** (an internal staff member with a declared beneficial interest in the paying node itself is not blocked — directly contradicting doc 32's own stated purpose for the attestation control) and a **deadlock branch** (a flat/root affiliate, the platform's own recommended first slice, can never have any commission approved). One-word fix (reflexive ancestor closure) identified but not yet applied. |
| **SEC-W15-02** (CRM bulk decomposition bypass) | Closed ("adopting N2.4") | **NOT CLOSED** | security, architect, and `bonus-engine` itself (reviewing Segmentation as first consumer) independently confirm, by grepping doc 10 for `parent_operation_id`/`EconomicOperationIdentity`/`REQ-BONUS-VOL-1`, that zero enforcement exists on the Bonus side. `architect`'s doc 31/doc 34 (the CRM-side half) is complete and correct, but the enforcing half was never built, and the CRM→Bonus interface at HEAD is architecturally weaker than pre-Wave-1.5 (the old `RequestOfferGrant` surface was withdrawn; nothing replaced its authorization check). Compounding this, code-reviewer (NEW-2) proved doc 34's own budget-projection mechanism — the thing that would close this once adopted — does not actually bound the decomposition attack as specified (scoped to the direct parent operation, not the whole lineage subtree; a 100-page split against a 5,000 ceiling passes cleanly under the literal rule). risk (RK-W15P2-2/RK-W15P2-4) separately found the `recipient_ceiling` budget is not monotone under compensating/clawback entries and has no self-defense against an undeclared consumption-row shape — two more ways the same ceiling fails open. |
| **SEC-W15-03** (actor≠subject self-dealing) | Partially closed | **PARTIALLY CLOSED, and its own mechanism has a new P0** | Adopted correctly at 2 of 5 named enforcement points (CRM `offer_request` targeting, Affiliate approvals). Zero adoption at Bonus (Grant issuance/activation, adjustment, forced conversion, `BulkGrantJob`, `BonusSuggestion` review) — the largest share of the platform's value-moving surface. Independently, `risk` (RK-W15P2-1) found `SEP-1`'s resolver **fails OPEN, not closed**, on a partial RLS read: a beneficiary set truncated by an RLS policy on a joined table (not merely emptied) passes every step of the documented algorithm and lets the self-deal through, directly contradicting `SEP-1-H1`'s stated belief that its only failure mode is an availability outage. This is a P0 in the control that was supposed to close the original P0. |

**Net: none of the four original P0s is closed to a state this Orchestrator would certify as implementation-ready.** LF-2's core mechanism is sound but has no buildable realization yet. The other three have concrete, independently-reproduced gaps or a newly-discovered defect in their own remediation.

---

## 2. New P0-severity findings surfaced this round

These were not part of the original four and must be tracked as first-class findings of equal severity, per the Fix Wave directive's own instruction that a re-verification round which produces new findings does not get to characterize the round as merely "closing the four."

1. **LF-18** (ledger-finance) — Casino's §16.4 lock-release step reads the wrong ledger-query variant (a bet-time-only credit-leg sum, not the currently-outstanding net-locked amount). On a concretely reachable path (a `casino_settlement_timeout` sweep followed by a late genuine win), this can drive `player_locked_bonus` negative and materialize restricted bonus value into `player_bonus` — value created from nothing, invisible to every ledger balance/reconciliation constraint including `SUM(DEBITS)==SUM(CREDITS)`. **Vetoed by ledger-finance.** This is arguably the single most severe finding of the round: a genuine, reachable ledger-invariant violation in a document that had already been through one review cycle.
2. **REQ-SEP-BONUS-4** (security) — The `HeldDispositionRecord` resolution mechanism — i.e., the fix for the original LF-2 — is itself an ungated self-dealing surface: threshold-gated four-eyes only, no `SEP-1` adoption, no dedicated permission. A staff member with hold-resolution authority can arrange or wait for a small bonus-funded win on their own account and resolve it to `release` alone, below any threshold, with a clean-looking audit trail. This is SEC-W15-03's exact pattern, reproduced in the control introduced to close a different P0 in the same round.
3. **Held-win rollback gap** (casino, independently corroborated by qa as test `C28`) — A provider rollback targeting a WIN that is currently parked in an open `HeldDispositionRecord` has no defined state transition anywhere in either document. The record stays `held` indefinitely; if staff later resolve it, they post real money against a credit that has already been reversed and no longer exists in the ledger — value creation or double-counting, and the round's own new reconciliation stream (`LF-12`) only catches this after the fact, not before the unsafe act.
4. **RK-W15P2-1** (risk) — Independent of REQ-SEP-BONUS-4 above, `risk`'s review of the `SEP-1` mechanism itself (as specified for the CRM/Affiliate adoption points that *are* built) finds the resolver fails open on a partial (not empty) RLS-filtered read of the beneficiary set — a defect in the shared mechanism everything else in this round depends on, not specific to any one adopting domain.

Three of these four (LF-18, the held-win rollback gap, RK-W15P2-1) are defects in *this round's own fixes*, not carryovers from Wave 1.5's original gate. That is the central finding of Phase 2: the fix wave closed some real gaps and opened others of comparable severity, several inside the closing mechanism itself.

---

## 3. Compositional incompatibility between bonus-engine's and casino's fixes

`architect`'s cross-domain review (not the author of either document) found the two Phase 1 fixes — written in parallel, each without sight of the other's final text — **do not compose**, in three independent, concrete ways rather than one naming mismatch:

- **`ACTION_REFORFEIT`'s posting sequence is specified two mutually exclusive ways across three live sites**: doc 10 §N1.4 step 5c (direct-to-`promo_liability`, "never via `player_bonus`, even transiently," with a regression test `G2-HOLD-1` written to fail the alternative) contradicts doc 10's own frozen §T.7 text and doc 08 §16.9 (both still describe "post normally, then compensate"). An implementer following either of the two unfixed sites builds the exact sequence LF-2 was raised against.
- **The holding-representation branch selection is circular**: doc 10 makes it conditional on a fact casino's fix does not decide; casino routes the decision to `ledger-finance`/`architect`; neither of the two candidate branches has a designed loss-side clearing mechanism (branch (a) has no timeout-sweep coverage as `bonus-engine` assumed — sportsbook's exhaustive settlement comes from its provider protocol, not from the locked-account shape, so adopting the lock shape does not confer it; branch (b) has no clearing mechanism at all).
- **The released-lock amount has no destination.** Casino flagged this explicitly twice across two rounds; `HeldDispositionRecord` carries one `amount` field with no way to represent both the win payout and the released stake as one occurrence.

`casino`'s own Phase 2 review (stress-testing against the live async callback model) independently reaches compatible conclusions from a different angle (finding #3 in its report: "these are directly contradictory factual claims about the same action, written in the same round by two specialists who did not see each other's final text").

**Architect's own verdict, quoted directly: "I would not certify this pair as architecturally consistent for a Wave 2 readiness call."** The Orchestrator adopts this verdict.

---

## 4. The four previously-flagged cross-dependencies — resolution status

1. **SEC-W15-02's actual closure, pending `bonus-engine`'s enforcement-side implementation** — **Confirmed still open**, and worse than "pending": the remediation mechanism it was pending on (doc 34's budget projection) is itself defective (§1 and §2 above).
2. **Pin-vs-live-resolution conflict (`CRM-BR-1` vs. doc 10 W5)** — **Substantively addressed but not confirmed by both required owners.** `architect`'s "pin is a ceiling, live resolution may only shrink it" proposal is independently re-derived and refined by `security` (three required adjustments: evaluate `SEP-1` at approval time against the pin only, never at execution against the shrunk set; make the tolerance check asymmetric — zero tolerance on widening, unlimited-but-evidenced on shrinkage; and for `resolution_mode = live`, the reconciliation is vacuous because no materialized pin exists — `security` requires either banning live mode for any campaign with an `offer_request` step, or adding a per-item `SEP-1` check at execution). `bonus-engine`'s own W5 side has still not confirmed the reconciliation from its own document.
3. **`ledger-finance`'s §7.7.1 finding (bonus_expense/Rule-B2-generator precondition) against `bonus-engine`'s actual §N1.4.1 design** — **Confirmed as a real, live gap**, restated this round as LF-19: branch 1's claim that "no new account type is needed" is true but incomplete, because the held credit still arrives from `house_gaming` (outside `BONUS_SET`), nets non-zero, and requires migration `0050` + the Rule B2 generator to exist — both currently blocked by HR-9.
4. **Casino/bonus-engine holding-mechanism composition** — **Confirmed NOT resolved**, and shown this round to be more broken than either side's own flag suggested (§3 above — three independent incompatibilities, not one).

---

## 5. Consolidated P1 register (new this round; does not restate Wave 1.5's original ~20 P1s, which remain tracked in `task-registry.md`)

Grouped by owning domain. Full detail lives in each specialist's committed architecture document section; this is a locator index, not a restatement.

**Ledger-finance** (`ledger-accounting-model.md` §7.7.1 continuation): LF-19 (holding-representation nullifiability under branch 1), LF-20 (`LockedExposure` double-counts under branch 1), LF-21 (three-way incompatible `ACTION_HOLD_FOR_REVIEW` descriptions), LF-22 (hold idempotency key is round-scoped, casino proved it must be occurrence-scoped), LF-23 (casino §16.7 still describes the forbidden post-then-reforfeit sequence), LF-24 (five unguarded `*big.Int`/`int64` boundary crossings in the new mechanics).

**Security** (`security-architecture.md` §W15): ancestor-chain inclusivity ambiguity (feeds code-reviewer's NEW-6), `principal_class` not yet adopted by `identity-compliance`'s owning doc 05, `CheckOfferEligibility` needs its own audit obligation (`CRM-BR-3`), `EDR-S1`–`S5` not yet carried into doc 30 §10, stale `RequestOfferGrant` references in doc 05.

**Architect** (docs 10/08 composition, doc 34): the three incompatibilities in §3 above; `HeldDispositionRecord` missing `tenant_id`/RLS/brand scope/approval references entirely (a CLAUDE.md multi-tenancy violation in a design that has not yet been implemented — caught before code, as intended); a genuinely unmodelled value-moving checkpoint at `ACTION_ROUTE_TO_CASH` (no live `AssetAuthorization → RG → Risk` re-evaluation, no EDR record).

**Code-reviewer** (docs 30/31/32/34): NEW-1 (`InclusionSafety`'s `Not` rule is not polarity-aware — the exact P1-2 laundering exploit is reachable again via double negation or `Not(member_of(...))`, with zero test coverage for this shape); NEW-2 (doc 34's budget projection, discussed above); NEW-3 (unpinned `member_of` lets a frozen `SegmentVersion`'s effective meaning drift after freeze, defeating `criteria_hash`'s reconstruction guarantee); NEW-4 (doc 30 §5.3 self-contradicts on whether `Resolve` returns two or three values, with one reading fail-open on the suppression path doc 31 depends on); NEW-6 (ancestor-chain reflexivity, above); NEW-7 (doc 34's approval-consumption model cannot support both single-shot and standing multi-execution authorizations as currently specified).

**Casino**: rollback-of-held-win gap (§2 above), multi-bet-round granularity mismatch between `HeldDispositionRecord`'s key and casino's own proven multi-bet-round reality, `L(G)`/`INV-TG` contradiction with bonus-engine's "leave the lock in place" branch.

**Risk**: RK-W15P2-1 (§2 above); RK-W15P2-2 (recipient-ceiling budget must be a monotone `COUNT(DISTINCT subject)`, not net with compensations, or clawback-then-regrant defeats the ceiling); RK-W15P2-3 (doc 34's serialization point is ambiguous in a way that can turn EOI lock contention into Risk fail-closed denials, and can deadlock against Risk's own advisory lock if acquisition order isn't fixed); RK-W15P2-4 (no self-defense against an undeclared consumption-row shape — the exact defect ADR 0031 §33 already fixed once in Risk's own package, reopened one level up); RK-W15P2-5 (cashback's value-at-execution-time-unknown problem, already named in ADR 0031 §42(c), reappears unaddressed in doc 34's value budget); RK-W15P2-6 (`SEP-1`'s non-NULL-actor precondition is enforced by a pre-deploy query, not a DB constraint); RK-W15P2-7 (`SEP-1`'s required denial audit record is unwritable as specified — a `BEFORE` trigger abort loses everything in the transaction, ADR 0031 §39 already solved this exact problem once); RK-W15P2-8 (the `HeldDispositionRecord` resolution surface is in neither `SEP-1`'s enforcement-point list nor doc 34's closed mint list); RK-W15P2-9 (`SEP-1` must test the pinned set, never the live-shrunk one — composes with cross-dependency #2 above).

**Sportsbook** (forward-looking, no sportsbook code/bonus-funded wagering exists yet): market-correction re-settlement has no bound and can in principle target an already-`converted` Grant; partial-settlement/cashout's netted-leg posting shape doesn't map onto the current binary technical/held split.

**Identity-compliance**: `REQ-PS-ID-1` unrouted in bonus-engine's own N1.10 and narrower than the actual gap (a `HeldDispositionRecord` can exist on an already-fully-terminal Grant with no open bet at all — no generalization of *open-bet* enumeration will ever surface it; a second, independent surface is needed); LF-12's aging check doesn't prioritize self-excluded persons.

**Product-owner-proxy**: no P0/P1 scope findings — confirmed clean on scope discipline across all six new/revised documents, with one forward watch-item (doc 34's dependency chain should not become "already decided" by virtue of being thorough).

---

## 6. Wave 2 readiness decision

# **NOT READY.**

Evidence: four new P0s discovered this round (§2), none of the four original P0s independently certified as closed (§1), a proven, reachable ledger-invariant violation in casino's own already-once-reviewed fix (LF-18), a genuine architectural incompatibility between the two largest fixes of the round confirmed by three independent reviewers from three different angles (§3), and a defect in the very control mechanism (`SEP-1`) that the round's security remediation depends on. This is not a "close a few residual P1s" gap — the fix wave's own outputs contain new, comparably severe defects, several of them inside the mechanisms built to close the original findings.

No human business decision was silently selected anywhere in this round; every gap above is a technical/architectural defect or an unimplemented enforcement point, not a disguised policy choice.

---

## 7. Required scope for the next round (Phase 3 — not yet authorized)

This is a recommendation for a future human-authorized dispatch, not an authorization to proceed. Per the standing instruction, the Orchestrator stops here.

Minimum closure list before any subsequent Wave 2 readiness re-assessment:

1. **LF-18** — fix casino's lock-release query to read the net-outstanding locked amount (variant-2 shape), add the sixth outcome branch. Owner: `casino`, reviewed by `ledger-finance`.
2. **One joint dispatch** (not two more parallel ones, per `ledger-finance`'s explicit recommendation) covering `bonus-engine` + `casino` + `ledger-finance` + `architect`, to settle: the holding-representation branch/account-type decision (A-2), the `ACTION_REFORFEIT` posting-sequence contradiction, the released-lock-amount destination, the pre-G-2 fail-closed behavior (reject-outright vs. capture-and-hold), and the rollback-of-held-win state transition.
3. **`SEP-1` hardening**: cardinality assertion + tenant-scope self-proof (RK-W15P2-1), DB-level NOT NULL constraint for the actor precondition (RK-W15P2-6), a separately-transacted denial-audit mechanism (RK-W15P2-7), reflexive ancestor-chain closure (code-reviewer NEW-6), and adoption at the three still-missing Bonus enforcement points (REQ-SEP-BONUS-1/2/3) plus the new REQ-SEP-BONUS-4 (`HeldDispositionRecord` resolution).
4. **EOI budget-projection fixes**: root-subtree-scoped consumption (not direct-parent-scoped, code-reviewer NEW-2), monotone recipient-ceiling counting (risk RK-W15P2-2), declared-consumption-row self-defense (risk RK-W15P2-4), value-unknown-at-execution handling for cashback-shaped grants (risk RK-W15P2-5), a single canonical lock-acquisition-order rule relative to the Risk gate chain (risk RK-W15P2-3), and a decision on whether `HeldDispositionRecord` resolution needs its own `operation_type` in the closed mint list (risk RK-W15P2-8, part B).
5. **Segmentation**: polarity-aware `InclusionSafety` (code-reviewer NEW-1), mandatory version-pinning on `member_of` (NEW-3), and the `Resolve` two/three-value self-contradiction (NEW-4).
6. **`HeldDispositionRecord`'s schema** needs `tenant_id`/RLS/brand scope/approval references before it can be considered a viable design at all (architect).

No CRM, Affiliate, Gamification, or bonus-funded-wagering implementation should begin until this list closes and a further independent re-verification confirms it. This report does not authorize that work — it is recorded here so the next human directive can scope it precisely rather than re-discovering it.

---

## 8. Deliverables checklist (per the Fix Wave directive's 22-item list)

1. This Fix Wave Report — ✅ this document.
2. P0→resolution matrix — ✅ §1 (original four) + §2 (new four).
3. P1 resolution matrix — ✅ §5 (index; full detail in owning documents).
4. Updated ownership map/dependency graph — pending; no ownership changes are required by this round's findings (all gaps route to already-assigned owners), so `ownership.md` is not edited this round. Recorded here as an explicit no-op, not an omission.
5. Updated Bonus/Segmentation/CRM/Affiliate/Casino-postWin architecture — **not updated this round**; the findings above are inputs to the next fix dispatch, not yet applied, per the directive's instruction that Phase 2 is review-only.
6. Updated G-2 boundary — unchanged; G-2 remains human-owned and unselected (confirmed §0).
7. `EconomicOperationIdentity` design — reviewed, found defective as specified (§1 SEC-W15-02 row, §5); not yet revised.
8. Updated ADRs/Human-Decision-Register/Agent-Task-Registry — task-registry.md updated in the same commit as this report (see below); no ADR changes required this round (no new architecture decision was ratified — only defects found).
9. Full specialist reports — held in each agent's SubagentHandback transcript; not separately committed as files (this project's established convention for Phase 2 review-only dispatches, matching the original Wave 1.5 gate).
10. Test evidence — `docs/testing/testing-strategy.md`'s new Phase 2 section (commit `08d10e3`), design/strategy only; no tests exist to run (`internal/bonus`, `internal/crm`, `internal/affiliate`, `internal/economicop` do not exist).
11. Exact remaining P0/P1/P2/P3 findings — §2, §5 (P0/P1); P2/P3 items are recorded in each owning specialist's document and are not exhaustively re-listed here given volume.
12. Exact Wave 2 readiness decision — §6: **NOT READY**.
13. Exact git commit SHA/branch/clean-dirty status/migration list — §0 (top of this document).

---

## 9. Explicit stop

Per the authorizing directive: **this fix wave is concluded. Wave 2, CRM implementation, Affiliate implementation, Gamification implementation, and bonus-funded-wagering implementation remain unauthorized.** No further work proceeds without a new, explicit human directive scoping the next round (recommended shape: §7 above).
