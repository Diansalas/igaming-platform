> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# Review: ADR 0089 (future AI agent boundary) — identity-compliance viewpoint

## Verdict
NO BLOCKING ISSUE. ADR 0089 correctly keeps agents non-authoritative for
RG/KYC/AML/jurisdiction/self-exclusion (§2.2, §5.1), correctly reuses
`rg.EvaluateEligibility` (ADR 0034) as sole gate, correctly labels
everything NOT IMPLEMENTED, and makes no regulatory-approval claim. Four
gaps should be closed before any future implementation; none require
editing the ADR now (I did not edit it). Two items should be logged as
FUTURE human/compliance decisions in addition to the one already in §8.3.

## Findings

### F1 — Self-exclusion-only wording under-scopes RG (moderate)
§5.1 says: "agents **must not be designed to target self-excluded
players**, and read tools must not expose self-exclusion status as a
targeting input." This only names self-exclusion, but RG scope (this
specialist's authority) also includes cooling-off periods after a limit
increase, active time-outs, and deposit/loss/wager/session-limit
restrictions — all of which must equally never be targeting inputs and
must equally exclude a player from a proposed campaign population.
Proposed text change (for the future editor, not applied):
> Replace "agents must not be designed to target self-excluded players,
> and read tools must not expose self-exclusion status as a targeting
> input" with "agents must not be designed to target self-excluded,
> timed-out, or cooling-off players, and read tools must not expose
> self-exclusion, time-out, cooling-off, or RG-limit-restriction status
> as a targeting input or population filter."

### F2 — "Vulnerable player" / inducement not addressed (note, not a gap in ADR's own scope)
Neither ADR 0089 nor ADR 0034 currently define a "vulnerable player"
status or an inducement rule (this is legitimate — no such status exists
in `internal/rg` today, per Blueprint §4.7 and CLAUDE.md's "don't assume
a requirement exists unless Blueprint/decision supports it"). ADR 0089
does not overclaim here, which is correct. Record as a FUTURE decision
rather than an ADR defect (see below).

### F3 — Segmentation READ can leak RG status indirectly (minor)
§4.1 lists "READ player segmentation" (doc 30) as a future capability,
and §5.1 states RG-restricted players are "removed from any population
the agent sees, by the canonical service." Good, but this guarantee is
stated only in the RG paragraph, not repeated against the segmentation
READ row in §4.1's table, and the ADR does not say that population
*counts* (e.g., "12,450 eligible players") must also exclude
RG-restricted players rather than merely being filtered at the
targeting step. If a count includes RG-restricted players before
filtering, an agent could infer self-exclusion prevalence within a
segment.
Proposed text addition to §4.1's "READ player segmentation" row:
> Append: "Any population size or count an agent reads is post-RG-filter;
> pre-filter counts that would let self-exclusion/RG-restriction rates be
> inferred are never exposed to an agent."

### F4 — §6 data-minimisation list omits KYC/AML-derived special-category signals
§6 "Data minimisation / PII" lists name, email, document data, IP,
payment details, and correctly excludes PAN/KYC document content. It
does not mention AML case status, sanctions/PEP match flags, or RG-
derived behavioural inferences (deposit patterns, loss chasing signals)
— these are exactly the special-category-adjacent, highest-sensitivity
compliance signals this specialist owns, and are a stronger leakage risk
than generic PII if ever exposed to a model/report.
Proposed text change to §6:
> After "Player-level PII (name, email, document data, IP, payment
> details) is excluded unless a specific tool is justified,
> `security`-reviewed and audited per read." add: "AML case data
> (sanctions/PEP match status, SAR-related flags, case notes) and RG
> behavioural signals (self-exclusion/time-out/limit history, deposit or
> loss-chasing indicators) are excluded by default under the same rule,
> and `identity-compliance` review is required in addition to `security`
> before any such tool is authorized."

## FUTURE human/compliance decisions to record (not decided now)
1. Whether jurisdictions (Anjouan; future BYOL tenants) restrict or
   require disclosure for automated/AI-driven marketing targeting or
   profiling — already correctly flagged in ADR 0089 §8.3 item 3. No
   change needed there.
2. NEW — whether the platform should define a "vulnerable player"
   status/flag (distinct from self-exclusion/cooling-off) and a rule
   banning inducement/targeting of such players, and if so, which
   jurisdiction(s) require it. This is a legal-interpretation question
   (CLAUDE.md "When to stop and ask") and should be raised at the next
   planning gate that touches CRM/segmentation (doc 30/31), not decided
   here or inferred from silence.
3. NEW — confirm that "READ player segmentation" and "READ reward
   history" tool designs, when built, are reviewed by
   `identity-compliance` (not only `security`) before granting any agent
   access, since both surfaces sit directly on top of RG/KYC-adjacent
   data even though nominally "bonus/CRM" data.

## No-approval-claim check
Confirmed: ADR 0089 makes no statement implying legal/regulatory
approval or certification. All capabilities are labeled
`NOT IMPLEMENTED`; §8.3 and the new item above correctly push legal
interpretation to a human/compliance decision rather than assuming it.
