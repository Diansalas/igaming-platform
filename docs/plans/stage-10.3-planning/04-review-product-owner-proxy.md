# Stage 10.3 planning — 04 Review (product-owner-proxy)

- **Type:** planning-gate review only. No code, no migration, no `deploy/`, no commit.
- **Reviewed:** `stage-10.3-planning-gate-proposal.md`, `00-roadmap-reconciliation.md`,
  `01-provider-trust-analysis.md`, `02-casino-financial-analysis.md`,
  `03-kyc-reason-bound-analysis.md`, all at HEAD `957a3e8`/`ff30d87`.
- **Test applied to every item:** is it required by (a) the Blueprint, (b) the current stage's
  B2C-MVP path, (c) the future B2B architecture, (d) security/compliance, or (e) to avoid material
  technical debt (CLAUDE.md "No uncontrolled scope expansion")? Anything that fails all five is
  flagged for deferral, not silently accepted.

## 1. Framing

Stage 10.3 is not itself a B2C feature. It is the precondition set for connecting *any* real vendor
behind the interfaces the B2C brand already depends on (payments, KYC, casino). The relevant question
is not "does this ship a B2C screen" but "does the platform stay stuck on MOCK providers, or on a
credential/architecture shortcut that has to be redone for the first B2B tenant, without this work."
Measured that way, most of the proposal is not overengineering — it is closing gaps the platform
already committed to (ADR 0022 §2.2/§3, CLAUDE.md financial/security rules) and that the Stage 10.2
review already flagged and deferred to "the first real adapter." This review does not re-litigate
those prior rulings; it checks whether *this* synthesis re-opens them into something bigger than
necessary.

## 2. Item-by-item verdicts

| Item | Verdict | Basis |
|---|---|---|
| WH-VENDOR-SCHEME-1 | **Build now** | Already registered as a hard pre-condition (ADR 0022 §3 Stage 10.2 amendment). Without it, no real adapter's headers ever reach its own verifier. Blueprint/current-stage path: blocks the first real adapter in all three domains. Not new scope — it is the previously-deferred item now being executed. |
| Credential resolver + secret store (handle table, resolver, cache, backends) | **Build now** | Same status: registered NOT IMPLEMENTED blocker since Stage 10.1/10.2. The machinery (rotation overlap, fingerprint pinning, fail-closed) is not gold-plating — it directly implements already-accepted ADR 0022 §2.2, and per-tenant credential isolation is a **future-B2B-architecture** requirement, not an MVP nicety: a shared static env-key shortcut (today's state) would have to be ripped out at the first B2B tenant. Building it once, correctly, avoids that debt. |
| MOCK-ADAPTER-PROD-1 | **Build now** | Pre-launch gate already registered. Cost is low (a marker interface, a startup guard, an AST completeness test). This is exactly the kind of "avoid material technical debt / security" item CLAUDE.md protects even under MVP pressure — a synthetic adapter reachable in production is a real-money and compliance risk, not a cosmetic one. |
| CAS-CAP-ROLLBACK-1 | **Build now** | Already registered as a hard pre-condition for wiring any real casino resolver, and it is a live financial-correctness defect (stranded stakes, withheld wins, missing tombstones) under CLAUDE.md's ledger rules. This is not new scope; it is closing a known defect. |
| G-1 (multi-bet cash rounds → 500 withholding wins) | **Build now, folded into CAS-CAP-ROLLBACK-1's wave** | Real financial-correctness bug (indefinite withheld winnings for a normal real-aggregator pattern: side bets, multi-hand tables). Cheap to fix, same wave, same reviewers. Avoiding it would be shipping a known defect into casino go-live — not acceptable to defer on MVP-speed grounds. |
| KYC-REASON-BOUND-1 | **Build now** | Already registered, already scoped narrowly (length/charset bound at ingestion, player-facing closed `reason_code` vs. staff-only bounded raw text). This is a compliance/security requirement (unbounded vendor free text reaching a player's browser and the audit trail is a real injection/disclosure surface) — non-negotiable under CLAUDE.md regardless of MVP pressure, so it cannot be deferred even if it were purely additive scope. Player-facing wording is correctly left to a human/compliance decision, not invented here. |
| PAYWH-TS-1 | **Recommended-closed disposition: approve, with a condition** | Superseding it with WH-VENDOR-SCHEME-1's mandatory SC7 timestamp window is sound engineering, not a silent drop — the residual (replayed KYC `error` audit growth) is disclosed and bounded. **Condition:** this disposition must be recorded as an explicit registry/ADR update when Stage 10.3 is authorized, not just left as prose in a planning paper, so it doesn't quietly disappear from `task-registry.md`. |
| PAYWH-BRAND-1 | **Correctly deferred** | No tenant today has per-brand merchant accounts at one provider. Building brand-scoped credential checks now would be speculative — there is no current trigger. The proposal records the exact trigger condition and the (small, additive) fix shape, so it is deferred, not dropped. This is the right call under the five-question test: none of the five apply *yet*. |
| PAYWH-RL-1 | **Correctly deferred to pre-launch** | Not required for sandbox integration; required before production traffic. Deferring it to a pre-launch wave (not 10.3) is proportionate — building edge/app rate limiting now, before any real adapter or even a staging environment exists, would be premature infrastructure. |
| Casino reconciliation — `casino_consistency` (internal stream) | **Build now** | Explicitly requested by the human ("casino reconciliation"). Directly required by CLAUDE.md ("reconciliation-capable," "any non-zero drift is a P1 incident") and is the only way today's silent failure modes (F11/F12: a rejected callback disappears, a provider-asserted event the ledger never saw is invisible) become visible. Not speculative — it detects real, already-possible defects. |
| **Casino rejection record (02 §2.6)** | **In scope for 10.3 — build now** | Directly answering the orchestrator's specific question: this is not a stand-alone feature, it is the durable evidence C6/C7 need to detect a provider-asserted event the ledger never posted. Without it, the "casino reconciliation" the human explicitly asked for cannot see that failure class at all — only a real statement could, and that's `PROVIDER DEPENDENT` and months away. It is minimal (append-only, no ledger effect, verified-callbacks-only per I1, bounded by a unique key), and it satisfies CLAUDE.md's audit and reconciliation requirements directly. Deferring it would leave "casino reconciliation" half-built while claiming the label — that is closer to fake completion than to appropriate scope discipline. |
| **`casino_statement` stream, MOCK source only (02 §2.5)** | **Approve, but this is the most deferrable item in the set** | Two things are true at once: (1) it follows the exact precedent already shipped for sportsbook (`MockSettlementStatementSource`), so building it keeps casino and sportsbook reconciliation architecturally consistent — a legitimate "avoid technical debt / consistency" argument, and it costs no new tables (it reuses `reconciliation_runs`/`_mismatches` plus one enum widening); (2) by the paper's own admission the MOCK source is "tautological by construction" and provides **zero** real detection value until a real casino statement format exists (`PROVIDER DEPENDENT`, not designed here). It buys proof of the matching *plumbing*, nothing more. **Verdict:** acceptable to build given the low incremental cost and the consistency argument, but it is not required by any of the five tests as urgently as the rest of the wave — if wave capacity or schedule pressure appears during implementation, this (W3a) is the first item to cut without weakening the B2C path, since real statement matching cannot exist without a vendor anyway. It should not be allowed to grow beyond "interface + MOCK renderer + key/total match," and any temptation to build a "more realistic" fake statement format should be resisted — there is no vendor to be realistic about yet. |
| Admin handle API (create/rotate/revoke provider credential handles) | **Needed now — build it** | This is not an optional nicety bolted onto the credential resolver; it is the *only* way to populate `provider_credential_handles` through an audited path once the resolver is real. Without it, testing rotation/revocation would mean hand-inserting rows, which bypasses the audit trail CLAUDE.md requires for every mutating administrative action (actor, tenant, before/after, reason code). Building the resolver without its write path would be incomplete, not minimal. |
| **Four-eyes approval on credential changes** | **Correctly NOT built now — confirm this disposition** | The proposal itself already defers this to W4 (pre-production-launch), calling it "`security`'s call and a recommended condition for production," not a CLAUDE.md hard requirement (CLAUDE.md's four-eyes mandate is scoped to *manual balance adjustments*, not credential handle writes). This is good scope discipline already present in the plan — this review's role is to confirm it stays deferred and does not creep into W2a under "while we're in there" pressure. No real credential exists yet to protect; four-eyes on handle changes can wait for the security specialist's pre-launch ruling. |
| PROV-OUTBOUND-CRED-1 (new) | **Justified addition, not scope creep** | A static process-wide env-key for outbound calls is a shortcut that directly contradicts already-accepted ADR 0022 ("independent credentials per tenant") and would be exactly the kind of debt a B2B tenant forces a rewrite over. The architect correctly caught this as a gap in doing WH-VENDOR-SCHEME-1/credential-resolver work honestly, not as a nice-to-have. Small, mechanical, in scope. |
| O4 (remove hard-coded `Provider("mock")` in `kyc_handlers.go`) | **Justified, trivial** | A real per-tenant KYC vendor cannot be selected while this hard-code exists. Blocks the KYC path. One-line-class fix, correctly scoped. |
| awssm secret-store backend (code only, tested against an SDK-interface fake) | **Build now, but only the code** | Consistent with the already-accepted platform store (ADR 0084/0086) — this is not introducing a new dependency decision, just implementing against one already made. Correctly scoped to code + local fake; the IAM/KMS/`deploy/` apply and any live AWS exercise are correctly held for separate human authorization (HD-10.3-2) and staging, not smuggled into 10.3's local-only work. |
| G-3, G-4, G-7 (casino DB backstop, cross-domain provider-id namespace, stranded-exposure checklist) | **Correctly deferred** | None of these block a first real adapter; each is disclosed with a registry-worthy home (LEDGER-REV-UNIQ, an architect registration rule, a checklist entry). Right call — building them now would be solving a problem with no adapter yet to trigger it. |
| Free rounds / jackpots / bonus-funded casino stakes (G-6) | **Correctly not built** | Not implemented, not required; only a conformance rule (adapter must not map such payouts to `CallbackEventWin`) is proposed, which is cheap insurance against a real defect (provider believes it paid, ledger has no record) rather than building the feature. Proportionate. |
| LEDGER-MANUAL-ADJ-4EYES-1 (compensation/four-eyes mismatch-resolution API) | **Correctly out of 10.3, flagged as its own future stage** | Platform-wide, not casino-specific, blocks real-money go-live rather than integration. Naming it as a real registry item (rather than quietly dropping it) is the correct discipline — it is exactly the "record as a future consideration" behavior CLAUDE.md asks for. |
| Hosted-KYC session token (J12), sanctions/PEP interface | **Correctly registered, not built** | No vendor requires them yet; both are noted with a clear trigger. Right call. |

## 3. Specific rulings requested

1. **Is the casino rejection record (02 §2.6) in scope for 10.3?** Yes. It is not a separate feature —
   it is the load-bearing evidence table for the "casino reconciliation" the human explicitly listed
   as in-scope. Without it, the reconciliation stream cannot see the "provider asserted a financial
   event the ledger never posted" failure class at all (C6/C7), which is precisely the class the
   capability-contract fix (CAS-CAP-ROLLBACK-1) makes newly relevant (E3/E10 named rejections now
   exist and need somewhere durable to land). Cutting it would leave "casino reconciliation" as a
   label without the evidence it needs to do its job.

2. **Does the `casino_statement` stream with only a MOCK source earn its place now?** Marginally yes,
   for consistency with the already-shipped sportsbook pattern and because it costs no new tables —
   but it is the single most deferrable item in the whole proposal. It proves plumbing, not detection.
   If schedule pressure hits during implementation, cut W3a before cutting anything else in this list;
   the real statement source is correctly labeled `PROVIDER DEPENDENT` regardless of whether the MOCK
   wrapper ships now or later.

3. **Is the admin handle API plus four-eyes on credentials needed now?** The admin API: yes, needed
   now — it is the only audited write path for the table the resolver depends on, not an optional
   extra. Four-eyes: no, and the proposal already agrees — it is correctly deferred to W4
   (pre-production-launch) as a `security` decision, not built in W2a. This review's ruling is to hold
   that line, not to add four-eyes now.

4. **Is the wave split proportionate?** Yes. W0 (ADR paper) → W1a–d (four disjoint, mostly parallel
   fixes) → W2a/b (credential resolver + outbound creds + O4; internal casino reconciliation) →
   W3a/b (statement MOCK stream; awssm code) → staging drills (deferred, human-authorized) matches
   real dependency order (each wave's own gate, no wave merges on red) and mirrors the granularity
   already used successfully in Stages 10/10.1/10.2. It is not fragmented for its own sake, and it is
   not one undifferentiated blob that would make partial authorization impossible.

## 4. Nothing on the human's list was silently dropped

Checked against the human's explicit list: WH-VENDOR-SCHEME-1, real signing-key/credential
resolution, MOCK-ADAPTER-PROD-1, CAS-CAP-ROLLBACK-1, casino reconciliation, KYC-REASON-BOUND-1,
PAYWH-BRAND-1, PAYWH-RL-1, PAYWH-TS-1, and "any other provider-readiness blockers found." Every one
of these appears in §2 of the gate proposal with an explicit disposition (build now / deferred with a
recorded trigger / recommended-closed with a rationale). None is missing, and none is deferred without
a named home (registry row, ADR, or explicit trigger condition). The one process condition this review
adds: PAYWH-TS-1's "recommended closed" status must be written into `task-registry.md`/an ADR at
authorization time, not left only in this planning folder, so it survives past this synthesis document.

## 5. Overengineering watch-list (not blocking, but flag for implementation discipline)

- **`casino_statement` (W3a):** do not let the MOCK source grow beyond interface + renderer + key/total
  match. There is no vendor to be realistic about yet (see §2/§3).
- **Credential resolver machinery** (rotation overlap, fingerprint pinning, cache with singleflight):
  commensurate with handling financial provider secrets and already-accepted ADR 0022 obligations —
  not flagged as excessive, but implementers should resist adding anything beyond what §2.1–2.2 of
  `01-provider-trust-analysis.md` actually specifies (e.g., no per-tenant KMS keys, no partner-console
  self-service secret writing — both are already correctly named as deferred in that paper).
- **Four-eyes on credentials:** do not let it creep into W2a "while we're in there." It is correctly a
  W4/`security` decision.

## 6. Verdict

**APPROVE WITH CONDITIONS.**

1. Record PAYWH-TS-1's "recommended closed / superseded by SC7" disposition in `task-registry.md` and/or
   an ADR at the moment Stage 10.3 is authorized — not only in this planning folder.
2. Treat W3a (`casino_statement` MOCK stream) as the lowest-priority, first-to-cut item if wave capacity
   is constrained during implementation; it is approved to build, not required to.
3. Hold the line already drawn in the proposal itself: four-eyes on provider credential changes stays
   out of Stage 10.3 (W4/`security`, pre-production-launch), and the admin handle API's scope stays at
   create/rotate/revoke plus audit — no self-service secret-material writing, no per-tenant IAM/KMS
   tightening, both already correctly named as deferred.
4. No other cuts requested. The remainder of the proposal (WH-VENDOR-SCHEME-1, credential resolver,
   MOCK-ADAPTER-PROD-1, CAS-CAP-ROLLBACK-1 + G-1, casino rejection record, casino_consistency,
   KYC-REASON-BOUND-1, PROV-OUTBOUND-CRED-1, O4, awssm code) is justified against the Blueprint /
   current-stage / future-B2B / security-compliance / technical-debt test and should proceed as scoped,
   pending `security` and `qa` sign-off per §17 of the gate proposal and the human's HD-10.3-1..4
   rulings.
