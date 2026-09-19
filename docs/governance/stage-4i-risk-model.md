# Stage 4I — Risk Phase 3 Proposal: the Risk-domain pieces of the jurisdiction model

**Status of this document: RULINGS on `internal/risk`'s own contracts, and
PROPOSALS/RECOMMENDATIONS everywhere else.** This is `risk`'s contribution
to Stage 4I's chain (`architect` reconnaissance → `identity-compliance` →
**`risk`** → `security` → `backend` → `casino` → `bonus-engine` →
`payments` → `sportsbook` → `qa` → `architect` final → independent
security/compliance final), per the directive.

Two of the six items below (§1, §2) are **rulings inside this
specialist's own Authority** — they concern `internal/risk`'s own
fail-closed contract and its own rule-authoring/activation discipline,
which `docs/governance/ownership.md` line 20 assigns to this specialist,
and which this task explicitly designates as this domain's call rather
than an escalation. The remaining four (§3, §4, §5, §6) are this domain's
**requirements and flagged hazards for other phases** — `architect` still
owns the final cross-domain resolution shape (Q-1/Q-2/Q-3), `casino` owns
its own blocklist, `backend` owns the resolver implementation, and
`security` owns the RBAC/RLS review of anything touching `risk_rules`.

**No code, migration, or schema was modified to produce this document.**
Nothing here is implemented. Where a ruling implies a future code or
schema change (§2's authoring-time precondition), it is named as a
proposal requiring `security` review before implementation, consistent
with this specialist's stated Authority ("cannot unilaterally add a new
StaffRole, RLS policy shape, or ledger schema change").

This document **cross-references rather than restates**
`docs/governance/stage-4i-reconnaissance.md` (hereafter "the
reconnaissance"; every bare `§N`, `C-N`, `K-N`, `P-N`, `Q-N` and `HDR-J-N`
reference below is to that document) and
`docs/governance/stage-4i-identity-compliance-model.md` (hereafter
"IC"; references to it are written `IC §N`). Read alongside
`docs/decisions/0031-risk-and-limits-engine.md` (ADR 0031) §9, §10, §34,
§42(a) and §42(b), and `docs/architecture/34-economic-operation-identity.md`
§5.3.

**HDR discipline.** None of HDR-J-1 … HDR-J-6 is decided here. §7 states
this domain's *position on* or *constraint upon any answer to* four of
them, exactly as IC §3 did, and routes the decisions themselves to the
orchestrator.

---

## 1. RULING R-1 — Risk's conditional fail-closed contract is CORRECT for Risk and is retained

**Ruling: `risk.Evaluate` does NOT become unconditionally fail-closed on a
missing jurisdiction. The existing contract — fail closed if and only if
at least one currently-effective rule visible to this request scopes
`jurisdiction_code` for this operation — is the correct semantic for Risk
and is retained unchanged.**

The reconnaissance's C-3 characterises this as one of four *mutually
incompatible* absent-value contracts. §1.4 of this document argues that
framing is imprecise and proposes a correction that changes what the rest
of the chain has to reconcile.

### 1.1 Why Risk is structurally different from AssetAuthorization

`assetregistry.CheckEligibility`'s layer 6
(`internal/assetregistry/authorization.go:163-172`) is **structurally**
jurisdiction-keyed: `asset_authorizations` rows are keyed
`(tenant, jurisdiction[, product])` (migration 0045). With `uuid.Nil` there
is no lookup to perform at all — the question "is this asset authorized
here?" is unanswerable, not answerable-as-yes. Unconditional deny is
therefore the only coherent behaviour, and ADR 0037 §C.2 / security
finding S-6a are right that it must never be read as "this check is not
scoped to a jurisdiction."

`risk.Evaluate`'s question is **not** structurally jurisdiction-keyed.
Jurisdiction is one of the ten optional scope dimensions on a
`risk_rules` row (`internal/risk/types.go:170-235`), ranked as a single
bit in `specificity()` (`types.go:296-297`, `1 << 1`). A
`max_amount` `HARD_LIMIT` on `casino_bet` scoped to `(tenant, asset_code)`
is **completely and correctly evaluable with no jurisdiction context
whatsoever**. Forcing it to refuse would not make that evaluation safer —
it would make a *complete, correct* evaluation refuse to run. That is not
fail-closed; it is fail-broken, and it denies operations for which no
policy gap exists.

### 1.2 The invariant the current contract actually guarantees

`missingScopeContext` (`evaluator.go:221-239`), invoked from Evaluate's
pre-matching loop (`evaluator.go:414-421`), guarantees exactly this:

> **No currently-effective rule may ever be silently skipped because the
> request omitted the dimension that rule narrows.**

That is the right invariant, and the code is deliberately conservative in
the right direction: the gate is **operation-wide**, not
"only if the rule would otherwise have matched on every other dimension."
Its own doc comment states why — "determining that requires the very
matches() logic this gate exists to backstop" — and `matches()` carries
the matching NOTE at `evaluator.go:159-163` recording that it is
intentionally *not* the fail-closed boundary. An unconditional gate would
not strengthen this invariant by one case; it would only add denials in
the cases where the invariant already holds vacuously.

### 1.3 The availability argument, stated concretely rather than abstractly

Both production callers today — `internal/casino`'s `LaunchGame`
(`orchestrator.go:190-194`) and `postBet` (`orchestrator.go:728`) — pass
`JurisdictionCode: ""`, because §1.3 of the reconnaissance confirms no
production path ever populates it. An unconditional gate would therefore
**deny 100% of real-money casino launches and bets from the moment it
shipped until the resolver shipped and was populated for every player of
every tenant** — while protecting against a class of evasion
(`a jurisdiction-scoped rule silently skipped`) that the conditional gate
**already closes completely**. A strictly stricter rule that adds zero
coverage and takes the money path down is not a security improvement;
this specialist's own Authority is "can block a change… if a risk
evaluation failure could resolve to ALLOW," which the current contract
already satisfies.

### 1.4 The correction to C-3: there are TWO contracts and TWO defects, not four incompatible contracts

This is the substantive finding this ruling contributes, and it narrows
what Q-3 has to reconcile:

| C-3 item | Behaviour | This domain's reading |
|---|---|---|
| (a) AssetAuthorization: absent ⇒ unconditional deny | Every layer-6 evaluation is jurisdiction-dependent by construction | **Contract A**, correctly specialised |
| (b) Risk: absent ⇒ deny iff a jurisdiction-scoped rule is effective for the operation | Only *some* evaluations are jurisdiction-dependent, and which ones is *configuration* | **Contract A**, correctly specialised |
| (c) Casino per-game blocklist: absent ⇒ skip (fail-open) | A jurisdiction-dependent control is silently not run | **A defect**, not a contract — see §5 |
| (d) Payments `SupportedCountries`: empty ⇒ permissive | Inverted semantics on a different code space (ISO country, §1.2 point 3) | **A defect and a code-space confusion** — `payments`' phase |

(a) and (b) are **the same rule**: *an absent jurisdiction may never cause
a jurisdiction-dependent policy to be silently skipped.* Where every
evaluation is jurisdiction-dependent, that reduces to unconditional deny.
Where jurisdiction-dependence is a per-rule configuration fact, it reduces
to the conditional gate. **Adopting "absent ⇒ deny" platform-wide as a
literal, uniform rule (Q-3's framing) would not unify two contracts — it
would over-apply Contract A's AssetAuthorization specialisation to a
domain with a different structure.** The platform-wide contract worth
adopting is the *invariant*, not the *specialisation*.

Proposed canonical wording for Q-3, offered to `architect`:

> **An absent jurisdiction value must never cause a jurisdiction-dependent
> policy, gate, restriction or authorization to be evaluated as if it did
> not exist. Where a consumer cannot determine whether any such policy
> applies without a jurisdiction, it denies. Where a consumer can
> determine that no such policy is in force, it may proceed — and must
> deny the moment one is.**

Contract (c) violates this. Contract (d) violates this. (a) and (b)
satisfy it.

### 1.5 Three things this ruling deliberately does NOT claim

1. **It is not a per-tenant opt-out.** The task's framing — "a tenant with
   zero jurisdiction rules may have legitimately decided jurisdiction is
   irrelevant" — is not accurate against the code, and this ruling does
   not rest on it. `listEffectiveRules` (`policy_service.go:88-98`) reads
   under migration 0041's `tenant_and_platform_read` RLS policy, which
   returns the caller's tenant's rules **plus every platform-wide rule**.
   A single platform-wide jurisdiction-scoped rule therefore makes
   jurisdiction mandatory for **every** tenant on that operation. That is
   deliberate and correct, and it is the identical blast radius
   `ErrMissingLicensingMode`'s doc comment (`evaluator.go:105-113`)
   already considered and accepted: "a platform-wide rule is deliberately
   binding on every tenant." The gate is scoped to *effective policy
   visible to this request*, never to a tenant's preference.
2. **It is not a per-operation hard-coding.** Evaluate must not gain a
   table of "operations that always require jurisdiction." Whether an
   operation is jurisdiction-dependent **is** the configuration — a rule
   exists for it or it does not. Hard-coding it would be inventing
   per-domain risk logic inside the engine, which this specialist's own
   Boundaries forbid.
3. **It guarantees nothing about policy coverage.** The gate protects
   against a *configured* rule being skipped. It does not and cannot
   protect against a rule that *should* exist and does not. Jurisdiction
   ruleset content is `identity-compliance`'s and the licence holder's,
   per `docs/architecture/15-jurisdiction-and-licensing-model.md`; no
   reader of this ruling should take it as "Risk guarantees jurisdiction
   correctness."

### 1.6 The one genuine weakness of the conditional shape, named honestly

It is **temporal**, not logical. The set of effective jurisdiction-scoped
rules can change *between two operations of the same round*:

- T0: launch, no jurisdiction-scoped `casino_launch` rule ⇒ ALLOW,
  `casino_launch_sessions.jurisdiction_code` stored NULL
  (`internal/casino/launch.go:124`'s `NULLIF($11,'')`), and that column is
  **write-once** (migration 0042's immutability trigger).
- T1: staff author a jurisdiction-scoped `casino_bet` rule.
- T2: a bet in that same round ⇒ `postBet` reads the frozen NULL ⇒
  `ErrMissingJurisdiction` ⇒ the round becomes un-completable.

Every outcome here is fail-closed (correct), but the round is stuck and
the session cannot be repaired, because the column is immutable by
design. This is not an argument for an unconditional gate — an
unconditional gate makes it strictly worse, stopping every round rather
than the ones affected by a newly-authored rule. It is an argument for
§2's authoring-time precondition, which removes the hazard at its source.

**Ruling R-1 is therefore: retained, unchanged, with the correction to
C-3 at §1.4 offered to `architect` as the platform-wide wording, and the
temporal hazard at §1.6 closed by R-2 rather than by widening the gate.**

---

## 2. RULING R-2 — activation safety: no shadow mode; an authoring-time precondition and a fixed activation order instead

The reconnaissance's §5 and Q-14 warn that closing the producer gap will
"convert three currently-dormant surfaces into live ones simultaneously."
For Risk specifically, that warning needs refining before it can be acted
on, because Risk's dormancy has a different shape from casino's.

### 2.1 Risk's blast radius is provably bounded, and the proof is two lines of the evaluator

**Finding: for every rule authored today, `risk.Evaluate`'s outcome is
invariant under the transition `JurisdictionCode: "" → "XX"`.** Proof,
directly from code:

- `Rule.matches` reads `req.JurisdictionCode` **only** inside
  `if r.JurisdictionCode != ""` (`evaluator.go:174-176`). A
  jurisdiction-*unscoped* rule's match result cannot change.
- `missingScopeContext`'s jurisdiction branch fires **only** when
  `r.JurisdictionCode != "" && req.JurisdictionCode == ""`
  (`evaluator.go:223`). Supplying a value can only *remove*
  `ErrMissingJurisdiction`, never introduce it.

So the resolver going live is, for Risk, **strictly non-regressive**
against the existing rule population. Newly-*matching* behaviour can only
arise from rules that scope `jurisdiction_code`.

### 2.2 And that population is statically enumerable — and is almost certainly empty today

Unlike casino's `casino_games.jurisdiction_blocklist` (which is
dormant-and-**harmless** today because K-3 fails open, so rows can
accumulate at zero cost and all fire at once), a jurisdiction-scoped
*Risk* rule is dormant-and-**catastrophic** today: per ADR 0031 §42(a),
authoring one denies 100% of that operation for every tenant immediately,
because no caller can supply a jurisdiction. ADR 0031 §42(a) records this
as a P1 `TRAP` with an explicit standing constraint against authoring
such rules, and §42(b) extends it to `provider_id`/`payment_method`/
`product`/`game_id`.

Consequence: **Risk cannot accumulate a hidden backlog of
dormant-but-live-on-activation rules the way casino can.** Any such rule
would already be causing a visible outage. The blast-radius question for
Risk is therefore answerable *statically, before activation, with one
query* — not observationally:

```sql
-- Pre-activation enumeration. No shadow mode required to answer
-- "what starts matching when the resolver goes live?"
SELECT id, tenant_id, brand_id, operation, jurisdiction_code, licensing_mode,
       rule_kind, action, limit_kind, time_window, effective_from, effective_until
FROM   risk_rules
WHERE  jurisdiction_code IS NOT NULL
  AND  status = 'active'
  AND  (effective_until IS NULL OR effective_until > now());
```

### 2.3 Ruling: do NOT add a shadow/audit-only rule status

**Ruling: `internal/risk` does not gain a `shadow`/`observe`/`dry_run`
`RuleStatus`, and does not gain a per-rule "evaluate but never enforce"
flag.** Four reasons, in descending weight:

1. **It cannot be made coherent with `missingScopeContext`.** A shadow
   rule is either "configured and effective for this operation" for the
   purposes of the operation-wide gate, or it is not. If **yes**, a shadow
   jurisdiction-scoped rule raises `ErrMissingJurisdiction` for every
   request lacking jurisdiction — producing precisely the denials shadow
   mode exists to avoid. If **no**, then during the observation window the
   engine is *not* fail-closed for that dimension, which is a deliberately
   introduced, temporary fail-open in the one gate whose entire purpose is
   to prevent a silent skip. **Neither answer is acceptable**, and there
   is no third.
2. **It contradicts the engine's own stated philosophy.** `types.go:24-29`
   records the rule this package already lives by: "a rule this package
   cannot evaluate must never be configurable at all, which is stronger
   than defensively skipping it at evaluation time." A status meaning
   "configured but deliberately not enforced" is the exact inverse.
3. **It multiplies the outcome space that ADR 0031 §34 worked to keep at
   four.** ALLOW / REVIEW / DENY / error are four semantically distinct
   outcomes, asserted by `Outcome.IsKnown()` (`types.go:367-374`) and
   re-checked against Evaluate's own output (`evaluator.go:533-535`). A
   fifth, "would-have-denied," has no defined handling at any enforcement
   point and would inevitably be collapsed into ALLOW by a caller.
4. **It costs a migration on an append-only, immutability-triggered
   table.** `risk_rules.status` is `CHECK (status IN ('active',
   'disabled'))` (migration 0041:94) and the immutability trigger
   (0041:241) permits only `status`/`description`/`effective_until` to
   change. Widening that CHECK is a schema change to a table whose RLS and
   permissions are `security`-reviewed — real cost, for a mechanism §2.1
   and §2.2 show is not needed.

**Explicit trap, named so a later reader does not reach for it:**
`RuleRiskSignal` is **not** a usable shadow mode either, despite its
"contributes to REVIEW but never DENY by itself" contract
(`types.go:87-89`). Today's only enforcement point collapses REVIEW into a
block — `internal/casino`'s `classifyRiskOutcome`, documented at
`orchestrator.go:174-178` ("A REVIEW outcome is treated identically to
DENY at this integration point"). A `risk_signal` rule authored "to
observe" would block real launches and bets.

### 2.4 What replaces it: a fixed activation order plus an authoring-time precondition

**(R-2a) Activation order — the resolver ships FIRST, rules second.**
The dangerous order is "author jurisdiction rules, then turn on the
resolver." The safe order, which §2.1's invariance proof makes free:

1. The resolver ships and begins returning real values, with **zero**
   jurisdiction-scoped Risk rules authored. Per §2.1 this changes no Risk
   outcome at all, for any tenant, for any operation — a genuinely
   no-op-for-Risk rollout, which is the strongest possible
   rollout-safety property and is better than any shadow mode could give.
2. The `§2.2` enumeration is run as an explicit gate step and must return
   zero rows. `qa` should own this as an assertion, not a checklist item.
3. Only then may a jurisdiction-scoped rule be authored — and each one is
   authored per `(tenant, operation)`, deliberately, with the tenant's
   own resolution coverage already demonstrated.

**(R-2b) Authoring-time precondition — the durable control.** ADR 0031
§42(a)'s constraint ("no jurisdiction-scoped `bonus_grant` rule may be
authored until the Bonus Engine can supply a real `JurisdictionCode` on
every one of its call sites") is today **prose in an ADR**. It should
become a machine-enforced precondition in
`risk.CreateRule` (`policy_service.go:145`): *reject the creation of a
rule scoping `jurisdiction_code` for an operation whose
`(tenant, operation)` pair is not recorded as jurisdiction-resolution-
active.*

This is the correct place for the fail-closed, for exactly the reason
`types.go:24-29` gives: it moves the failure from **evaluation time**
(where it denies real players mid-round, per §1.6) to **authoring time**
(where it returns a 400 to a `risk_manager` staff member with a message
explaining what is missing). It also structurally eliminates §1.6's
stuck-round hazard, because a jurisdiction-scoped rule can no longer come
into existence for an operation whose callers cannot supply the value.

**Scope and ownership of the precondition, stated precisely so this stays
inside this specialist's authority:**

- The **precondition check** lives in `internal/risk` and is this
  specialist's to implement when authorized.
- The **fact it reads** — "is jurisdiction resolution active for this
  `(tenant, operation)`" — is **not** Risk's to define, own, or store.
  It is a property of the resolver, and belongs with whoever owns the
  resolver (Q-15, currently unassigned; `docs/governance/ownership.md`
  has no jurisdiction row at all). Risk consumes it read-only, through
  whatever narrow accessor the resolver's owner exposes. **Risk must not
  define a new enablement table, flag column, or StaffRole for it** —
  that would be this specialist adding platform configuration outside its
  own domain, and it needs `architect` + `security` per
  `docs/governance/change-control.md`.
- If the chain decides no such per-`(tenant, operation)` fact will exist,
  the fallback is R-2a's ordering plus the §2.2 enumeration as a hard
  gate, and ADR 0031 §42(a)'s constraint stays prose. That is weaker but
  acceptable; it is not a blocker on the resolver.

### 2.5 Known, accepted observability gap — disclosed, not papered over

There is today **no artifact recording that a rule matched and did not
breach.** `RiskDecision.MatchedRules` (`types.go:380-397`) carries exactly
that information, including `Breached: false` entries, and it is
**discarded**: `internal/casino`'s `evaluateAndAuditRisk` returns early on
ALLOW without writing an audit record (`orchestrator.go:375-376`), and no
caller reads `MatchedRules` at all.

So the question "how often would this jurisdiction-scoped rule have
fired?" is **unanswerable from persisted data** today. This ruling
**accepts that gap rather than closing it**, deliberately:

- Closing it by auditing ALLOW decisions would write an audit row on the
  **bet hot path for every bet** — an audit-volume and throughput
  regression on the single path CLAUDE.md's financial rules are most
  protective of, to observe a rule population §2.2 shows is enumerable
  statically and is currently empty.
- The narrow future option, if a real need appears: emit an observation
  **only when a jurisdiction-scoped rule matched and did not breach** —
  bounded by construction, since such rules are rare and enumerable. It is
  named here as a deferred option (a `docs/decisions/` future
  consideration), not proposed for this stage.

`qa` should note that the absence of this artifact means the
concurrency/precedence test coverage for jurisdiction-scoped rules must be
built on `MatchedRules` returned from `Evaluate` directly (as
`internal/risk/risk_integration_test.go` already does), not on audit
records.

---

## 3. The resolved-value shape, from Risk's consumption perspective (Q-1, Q-2)

**Requirement, stated as two separate answers for two separate
questions.**

### 3.1 For MATCHING: Risk requires a single opaque `jurisdictions.code` string, and must NOT learn the source or confidence

**Risk's rule-matching logic must not gain a confidence/source
dimension.** `RiskRequest.JurisdictionCode string` (`types.go:320`) stays
exactly as it is. Four reasons:

1. **It would create a fail-open shape the existing gate structurally
   cannot catch.** Suppose a rule could declare `min_jurisdiction_source =
   "verified"`. `matches()` would then return `false` for a request whose
   jurisdiction came from an unverified declaration — silently excluding
   the rule. `missingScopeContext` cannot backstop this: it tests
   **emptiness** (`req.JurisdictionCode == ""`), and here the request
   carries a *present but insufficient* value. This is precisely the
   "empty request-side dimension silently excludes the rule" fail-open
   that `evaluator.go:159-163` and `evaluator.go:78-89` exist to prevent,
   reintroduced one level up and **outside the reach of the mechanism
   built to catch it**. That alone is disqualifying.
2. **It makes every rule author re-decide a compliance question.**
   "Is this evidence good enough to enforce on?" is a compliance
   determination (IC §4) and, per HDR-J-2, partly a legal one. Encoding it
   as a per-rule predicate distributes that judgment across every rule a
   `risk_manager` ever writes, with no single place it is right or wrong.
3. **It has no principled place in `specificity()`.** Adding an 11th bit
   to `types.go:270-303` requires answering: is "jurisdiction X, verified
   sources only" more or less specific than "jurisdiction X, any source"?
   There is no defensible answer — they narrow *different kinds* of thing.
   Two rules differing only on that axis would score identically and trip
   `ErrConflictingRules` (`evaluator.go:26`, `:488`), converting a
   reasonable pair of policies into a fail-closed outage for that
   operation.
4. **It duplicates a separation this package already makes deliberately.**
   `internal/risk/denomination.go:57-62` states the pattern verbatim for
   assets: "Risk asks [the registry] only 'what denomination is this
   request in'; whether the asset may be used for the operation at all is
   `assetregistry.CheckEligibility`'s call, made at the enforcement point,
   and `internal/risk` neither duplicates nor substitutes for it."
   Jurisdiction is the identical shape: **Risk asks only "which
   jurisdiction is this request in"; whether the evidence establishing
   that is good enough to enforce on is the resolver's/compliance's call,
   made before Risk.**

**Where the confidence threshold belongs instead:** in the resolver, per
**operation class** — which is the axis HDR-J-2 is actually about. The
resolver is asked to resolve for an operation class and either returns a
code good enough for that class, or returns unresolved. Risk then receives
its existing string and its existing fail-closed gate (R-1) does the rest,
**with zero new surface in `internal/risk`**.

This composes exactly with IC §4's position, and gives it teeth: IC states
an unverified declaration "must not, on its own, be treated as dispositive
for an enforcement-grade decision (a jurisdiction-scoped `HARD_LIMIT`…)."
Under this design that is enforced *structurally* — an enforcement-grade
Risk evaluation that only has an unverified declaration available receives
**unresolved**, i.e. empty, and R-1's gate denies if any
jurisdiction-scoped rule is in force. Under a per-rule `min_source`
design it would be enforced only by rule authors remembering to set a
field. **Risk endorses IC §4 and asks that it be implemented in the
resolver, not in `risk_rules`.**

### 3.2 For REPORTING/AUDIT: Risk needs the provenance, and therefore the resolver SHOULD return a record, not a bare code

**Q-1 answer from this domain: a resolution record, with Risk consuming
only `.Code` for matching and carrying the rest to audit.**

C-5's finding — the jurisdiction actually used for a decision is never
recorded — bites Risk directly and is not hypothetical. Today
`evaluateAndAuditRisk`'s denial metadata (`orchestrator.go:383-392`)
records `reason_code`, `outcome`, `operation`, `brand_id`, `provider_id`,
`asset_code`, optionally `game_id` and `amount` — and **not
`jurisdiction_code`, and not `licensing_mode`**. So even today, for the
one production path where a jurisdiction could in principle be supplied, a
denial by a jurisdiction-scoped `HARD_LIMIT` produces an audit record from
which the jurisdiction cannot be recovered. A regulator-facing
reconstruction of "under which jurisdiction's rules, established on what
basis, was this bet denied?" is impossible.

Risk's requirement, therefore:

- The resolver returns a record carrying at minimum `Code`, `Basis`/
  source, and `AsOf` (the reconnaissance's §9 illustrative shape is
  adequate; this domain takes no position on the exact field names, which
  are `architect`'s).
- `risk.Evaluate` continues to take **only the code** — the record is
  **not** threaded into `RiskRequest`, because §3.1 forbids it
  influencing matching, and a field on `RiskRequest` that matching ignores
  would be an invitation for a future author to use it.
- The **enforcement point** (casino's `evaluateAndAuditRisk`, Bonus's
  `GateCheckpoint`) carries the record's provenance into the audit record
  alongside the `RiskDecision` it already has. That is a caller-side
  change owned by `casino`/`bonus-engine`, recommended here.
- Separately and independently of Stage 4I's resolver work, **this
  specialist recommends adding `jurisdiction_code` and `licensing_mode` to
  `evaluateAndAuditRisk`'s denial metadata** — a one-line,
  zero-risk, already-correct-today improvement that makes a
  jurisdiction-scoped denial explainable. Flagged for `casino`'s phase.

### 3.3 Q-2 (uuid vs. code) — Risk's requirement is the code, and it is load-bearing

Risk needs `jurisdictions.code` (a string), not `jurisdictions.id`.
`risk_rules.jurisdiction_code` is a `TEXT` column with an FK to
`jurisdictions (code)` (migration 0042), it is part of the immutability
trigger's core-field set, and it is compared by string equality at
`evaluator.go:174`. Switching Risk to the uuid would be a migration on an
append-only table plus a rewrite of every authored rule, for no benefit.

**Risk's ask of `architect`: whichever carrier is canonical, the record
must expose BOTH** (as the §9 sketch does), so that `assetregistry`
(needs `id`) and Risk/casino/RG/withdrawal/bonus (need `code`) both read
from one resolution rather than translating independently. The only
translator that exists today is package-private inside `internal/bonus`
(`resolveJurisdictionID`, `eligibility.go:139-152`), and it returns
`uuid.Nil` for **both** an empty code and an *unknown* code — collapsing
"not resolved" and "resolved to a jurisdiction not in the registry" into
one value. For Risk that distinction matters: an unknown code would match
zero rules and resolve to ALLOW, while an empty one is caught by R-1's
gate. **Any shared translator must keep those two cases distinct and both
non-ALLOW.** Flagged for `architect`/`backend`; it is a live latent
fail-open in the existing helper, not a new concern introduced by this
stage.

---

## 4. HDR-J-2's domain note — CONFIRMED, with two sharpenings

IC §3 proposed, explicitly as an observation and not a decision, that the
source precedence "is itself likely to be per-jurisdiction configuration,
not one global platform rule."

**Risk's position: confirmed. Risk requires no domain-specific
jurisdiction and must never have one.** Plus two sharpenings, the second
of which is a problem neither prior document names.

### 4.1 Sharpening 1 — the invariant is stronger than "no domain override": one operation, one jurisdiction, every gate

The axis that legitimately varies is **operation class**, not **consumer
domain**. Risk must consume whatever the platform's single canonical
resolution says for the operation being gated. The invariant this domain
asks `architect` to record:

> **One operation resolves exactly one jurisdiction, and every gate in
> that operation's chain consumes that same value. No gate re-resolves,
> overrides, narrows, or substitutes it.**

Code grounding, three independent reasons:

1. **The gates are composed in series in one transaction.** Doc 34 §5.3
   rule 2 fixes the chain as `AssetAuthorization → RG → Risk`, "unchanged
   and unreordered." Two gates in one chain resolving *different*
   jurisdictions for *one* operation is not pluggability — it is C-3's
   four-absent-value-contracts problem reproduced in the *present* value,
   where it would be far harder to detect.
2. **The one existing composed caller already assumes it.**
   `bonus.GateCheckpoint`'s `GateParams` carries `JurisdictionID
   uuid.UUID` and `JurisdictionCode string` (`eligibility.go:53-54`) as
   **two representations of one intended value**, derived from each other
   by `resolveJurisdictionID`. If Risk and AssetAuthorization could
   legitimately differ, that struct is already wrong.
3. **A denial would become uninterpretable.** A `GateOutcome` reporting
   "denied on jurisdiction" would mean two different jurisdictions inside
   one decision, and the C-5 audit record — which §3.2 argues must record
   *the* jurisdiction — would have no single value to record.

### 4.2 Sharpening 2 — a bootstrap circularity in "per-jurisdiction precedence" that must be resolved before it is adopted

**New finding, not named in the reconnaissance or in IC.** If the
precedence among location/residence/nationality is configured
*per jurisdiction*, then selecting the precedence rule requires knowing
the jurisdiction — which is the output of applying the precedence rule.
That is circular, and a naive implementation resolves it by picking some
signal arbitrarily to bootstrap, which silently reintroduces a global
default while appearing to be configuration-driven.

Risk's proposed resolution, offered to `architect` (this is engineering
shape, not the HDR-J-2 legal question):

> **The precedence configuration must be keyed on something knowable
> *before* player-side resolution runs.** The natural candidate already
> exists and is already server-resolved on every operation: the
> **tenant/licence side** — `tenants.licensing_model` (read today by
> `resolveLicensingMode` in both `internal/casino` and
> `internal/bonus`, the reconnaissance's P-6, "the only
> jurisdiction-adjacent value that is genuinely server-resolved on every
> operation today") and, once readable, `licences.jurisdiction_id`. Key
> the precedence config on `(tenant licensing jurisdiction, operation
> class)`, never on the resolved player jurisdiction.

This preserves IC's insight (precedence is configuration, not a global
constant) while making it computable, and it uses the one jurisdiction
fact the platform can already establish without a resolver. It also
composes with `risk_rules.licensing_mode`'s existing rationale (ADR 0031
§10): the platform already accepts that the licensing side is the
knowable, coarse anchor and the player side is the fine, currently-absent
one.

### 4.3 Also: the resolver must not be owned by `internal/risk` (Q-15)

Stated because Q-15 is open and `docs/governance/ownership.md` has no
jurisdiction row. **Risk is a consumer of jurisdiction, not a producer,
and must stay one.** Absorbing resolution into `internal/risk` would make
this package both the producer and the consumer of its own scope
dimension, remove the independent check that R-1's gate provides, and
violate this specialist's own Boundaries in the same way §3.1(4) already
rejects. This domain takes no position on *who* should own it.

---

## 5. Recommendation to `casino`'s phase — the per-game jurisdiction blocklist (K-3 / C-3(c))

**Not implemented here. `internal/casino` was read only.** This section is
a recommendation for `casino`'s later phase to accept, reject or amend.

**Recommendation, in one sentence: keep the blocklist as a separate
mechanism owned by casino, but fix its absent-value contract, its
resolution source, and its position — do NOT convert it into
`risk_rules`.**

### 5.1 Why it should NOT be absorbed into the Risk engine

This is the part where this specialist's own "one engine, never a
per-domain limit engine" principle could be misread as "absorb every
gate." It should not be:

- **It is not a limit.** It is a **catalogue availability** fact — *may
  this game be offered in this jurisdiction* — the same class of fact as
  `casino_games.supported_assets` and `IsGameAvailable`'s tenant/brand
  availability check. Risk's engine governs limits (`min_amount`,
  `max_amount`, `cumulative_amount` — `types.go:32-36`). Absorbing a
  catalogue fact would be the inverse boundary violation.
- **The rule-row cardinality would be pathological.** Expressing it as
  Risk rules needs one `casino_launch` rule per `(game × blocked
  jurisdiction)` on an **append-only, immutability-triggered** table where
  rules are disabled and never deleted (migration 0041:208-211, :241).
  Delisting a game in one market becomes N irreversible rows.
- **It would poison the operation-wide gate.** `missingScopeContext` is
  deliberately operation-wide. A single `game_id`-scoped `casino_launch`
  rule forces **every** `casino_launch` request to supply a game id —
  precisely ADR 0031 §42(b)'s documented P1 trap. A `TEXT[]` column on the
  game row is the right shape for this fact.

### 5.2 What should change (four items for `casino`)

1. **Fail closed on an unresolved jurisdiction.** `orchestrator.go:138`'s
   `params.JurisdictionCode != nil &&` guard must go. Per §1.4's proposed
   canonical wording, this is a jurisdiction-*dependent* control that
   currently is not run when jurisdiction is absent — the defect, not a
   contract. Recommend a **distinguishable new sentinel** (e.g.
   `ErrJurisdictionContextMissing`), never reusing
   `ErrJurisdictionBlocked`, consistent with casino's own stated
   discipline at `orchestrator.go:109-112` ("'game doesn't exist' vs. 'not
   enabled here' vs. 'provider unavailable' vs. 'blocked in this
   jurisdiction' are never collapsed into one generic not-found"). A
   player refused because the platform could not determine their
   jurisdiction has not been "blocked in their jurisdiction."
2. **One resolution, one variable, three consumers.** Today the blocklist
   dereferences `params.JurisdictionCode` at line 138, while the local
   `jurisdictionCode` used for Risk and for the session snapshot is
   derived separately at lines 166-169. Same value today, two
   dereferences, and only one of them is persisted. Resolve once, above
   line 138, and feed the blocklist check, the `RiskRequest`, and
   `CreateLaunchSession` from that single variable — this is also what
   §4.1's invariant requires.
3. **Blast-radius item nobody has named: demo mode.** The Risk gate is
   real-money-only (`orchestrator.go:179`, `params.Mode == ModeReal`). The
   blocklist check at line 138 is **not** mode-gated. Making it
   fail-closed therefore starts denying **demo launches** too, the moment
   it ships — a surface neither the reconnaissance's §5 nor Q-14 mentions,
   and a plausible source of a "why did the whole demo lobby break"
   incident. `casino` must decide deliberately whether demo launches are
   jurisdiction-bearing (this is the reconnaissance's Q-6, "which
   operations are jurisdiction-bearing at all", instantiated concretely)
   and record the answer either way.
4. **Return shape consistency.** The blocklist returns a Go `error`
   (`ErrJurisdictionBlocked`), while the RG and Risk gates return a
   `LaunchGameResult{Denied: true, DenialCode: …}`
   (`orchestrator.go:155-157`, `:201-204`). A fail-closed
   jurisdiction-missing outcome should pick one shape deliberately;
   this specialist has no preference, only that it be chosen rather than
   inherited.

### 5.3 What should stay the same

The blocklist's **position** before RG and Risk is correct and should not
change. Cheap catalogue/availability facts first, then RG, then Risk
(which is the only gate that takes a lock — see §6) is the right order
both for cost and for lock-hold time, and doc 34 §5.3 rule 2 pins the
`AssetAuthorization → RG → Risk` portion as unreorderable. What the
blocklist must acquire is the **T.1 discipline** — ordered, each gate
distinguishable, each fail-closed, none skippable by an absent input,
every denial audited — not a new position or a new owner.

---

## 6. Lock-ordering and caching hazards for `backend`'s implementation phase

This platform has taken real lock-ordering defects from exactly this class
of interaction (`DR-4HB1W2-01`, the EOI/Risk reversal; and the
`DR-4HB1W3-ARCH-01`-era work that produced doc 34 §5.3's canonical rule).
The question was whether introducing jurisdiction resolution/caching
creates a new one. **It does — three distinct hazard shapes, of which the
second is the serious one.** All three are flagged for `backend` to design
around; none is a defect in code today.

### 6.1 The lock Risk actually takes, stated precisely

`internal/risk/evaluator.go:337-340`, inside `Rule.breach()`, for
`LimitCumulativeAmount` rules only:

```go
lockKey := fmt.Sprintf("%s:%s:%s:%s:%s", req.TenantID, req.PlayerAccountID, req.Operation, r.LimitKind, req.AssetCode)
tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('risk_cumulative'), hashtext($1))`, lockKey)
```

Transaction-scoped, released only at the **caller's** commit/rollback,
and — note for all three hazards — **the key does not include
jurisdiction**. Doc 34 §5.3 rule 4 makes the canonical ordering
"Risk's advisory lock is always acquired BEFORE the EOI row lock, never
after," and explicitly grounds the AB-BA-unreachability argument on that
being true on **every** path.

### 6.2 Hazard H-1 — lazy in-transaction resolution interleaved with the advisory lock

If any jurisdiction read ever happens **inside** `risk.Evaluate` (it must
not — §4.3 — but the hazard generalises to any gate that resolves lazily),
and that read takes any lock or row-level share, its order relative to
`pg_advisory_xact_lock` becomes **rule-set-dependent and therefore
non-deterministic across tenants**: `listEffectiveRules` orders by
`id` (`policy_service.go:93`), and `id` is a random v4 UUID, so whether a
cumulative rule or a jurisdiction-scoped rule is examined first varies by
rule population. Two tenants' rule sets could acquire the same two locks
in opposite orders — a textbook AB-BA.

Note that the existing `assetExponents` resolver avoids this **by
accident, not by contract**: `exponents.forRequest` is called at
`evaluator.go:297`, above the `switch` and therefore always before the
lock at line 338. Nothing states that as a requirement.

**Requirement for `backend`:** any in-transaction resolution that Risk's
evaluation path depends on must be performed **eagerly, exactly once,
strictly before the first rule is examined** — i.e. above
`evaluator.go:414`'s pre-matching loop — never lazily from inside
`breach()`. The `assetExponents` memoisation pattern
(`denomination.go:67-93`) is the right shape; its call *position* should
be made explicit rather than incidental.

### 6.3 Hazard H-2 — a resolve-and-persist resolver, which IS a live AB-BA shape (the serious one)

**If the resolver writes anything — a cache fill, a snapshot row, a
"last resolved jurisdiction" upsert on a player-keyed table — inside the
guarded transaction, there is a genuine AB-BA between that row lock and
Risk's advisory lock today**, because the platform already contains both
orders:

- **Order 1 (write-after):** `internal/casino`'s `LaunchGame` resolves
  jurisdiction at `orchestrator.go:166-169`, runs the gate chain
  (including Risk's advisory lock at `evaluator.go:338`), and *then*
  persists the jurisdiction via `CreateLaunchSession`
  (`launch.go:124`). Risk lock → jurisdiction row write.
- **Order 2 (write-before):** any resolver that upserts its resolution as
  part of resolving — the natural implementation of "resolve and cache,
  with explicit invalidation" — writes the jurisdiction row *before* the
  gate chain runs. Jurisdiction row write → Risk lock.

Both locks are **player-keyed** (Risk's key includes
`req.PlayerAccountID`; a jurisdiction cache row would be keyed by player
or player+tenant), so two concurrent operations on the **same player**
— exactly the case the cumulative-limit lock exists to serialise, and
exactly what `internal/risk/cumulative_race_integration_test.go` already
exercises — can deadlock on a real `40P01`. Under the fail-closed
contract at `evaluator.go:363-377`, a `40P01` is an Evaluate error and
therefore a **DENY of a real player's bet**.

**Requirement for `backend`, stated as a rule:**

> **The jurisdiction resolver must be READ-ONLY on the evaluation path.**
> It takes no `FOR UPDATE`, no advisory lock, and performs no write of
> any kind while any gate in the chain is running. Any persistence of a
> resolution — cache fill, snapshot row, audit of the resolution itself —
> happens either (a) entirely outside the guarded transaction, or (b)
> strictly **after** the whole gate chain completes and immediately before
> or with the effecting write.

This is deliberately the same position doc 34 §5.3 rule 3 already assigns
to the EOI consume, and it extends §5.3's numbered list with what is
effectively a **rule 0**:

> 0. **Jurisdiction resolution happens once, read-only, with no locks,
>    strictly before the gate chain begins.**
> 1. EOI entry check — non-locking. *(unchanged)*
> 2. Gate chain: `AssetAuthorization → RG → Risk`, including Risk's
>    `pg_advisory_xact_lock`. *(unchanged)*
> 3. Locking EOI consume on the root row, immediately before the effecting
>    write. *(unchanged)*
> 4. Risk's advisory lock always before the EOI row lock. *(unchanged)*

With rule 0 in place the ordering is total — jurisdiction (no lock) →
Risk advisory lock → EOI row lock — and AB-BA stays structurally
unreachable, which is the property §5.3 was written to guarantee.
**Recommending that doc 34 §5.3 gain this rule 0 is `architect`'s call,
not this specialist's; it is flagged, not made.**

Note that casino's existing Order-1 write (`CreateLaunchSession`) is
already compliant with rule 0 — it is a post-gate-chain write. Nothing in
`internal/casino` needs to change for H-2; the hazard is created only by
how the *resolver* is built.

### 6.4 Hazard H-3 — no network I/O between the advisory lock and commit

If a jurisdiction cache is Redis-backed, a cache read is **network I/O**.
Placed anywhere after `evaluator.go:338`, a slow or timing-out Redis read
executes while holding a transaction-scoped advisory lock on
`(tenant, player, operation, limit_kind, asset)`, serialising every other
operation for that player behind an external service's latency —
converting a cache-latency blip into cascading fail-closed **denials**
under load. This is the identical failure mode doc 34 §5.3 names for the
EOI lock ("turning an EOI throughput concern into Risk fail-closed denials
under load").

**Requirement for `backend`:** **no network I/O of any kind may occur
between the acquisition of Risk's `pg_advisory_xact_lock` and the
transaction's commit.** Rule 0 already satisfies this for jurisdiction
specifically; it is stated separately because it is the more general
property, it is cheaply testable, and it is the one a future author is
most likely to violate for an unrelated reason.

### 6.5 Restating this domain's own cache boundary, since the directive permits caching

The directive allows caching and requires that the cache never become
authoritative and that invalidation be explicit. This specialist's own
Boundaries already state the stricter local rule, and it is unchanged and
non-negotiable here:

> **Redis or any cache is never the authoritative source for a
> cumulative/exposure check. PostgreSQL, inside the same transaction as
> the guarded operation, is the only authoritative correctness boundary.**

Concretely: `cumulativeUsage` (`cumulative.go:262-312`) reads
`ledger_entries`/`ledger_transactions`/`ledger_accounts` **through the
same `pgx.Tx` the caller will post its own effect in**, and
`assetExponents.forRequest` reads the asset registry the same way, with
its doc comment explicitly recording "so no exponent is ever cached across
transactions" (`denomination.go:79-81`). **A cached jurisdiction is
acceptable in a way a cached exposure total is not** — it is a scope
selector, not a monetary quantity, and R-1's gate plus §3.1's
resolver-side confidence threshold bound the damage a stale one can do —
**but only under rule 0**, i.e. read outside or before the gate chain,
never as a substitute for anything Risk reads in-transaction, and never
holding the advisory lock. `ledger-finance` should be asked to confirm
this reading if any cached value ever influences a cumulative evaluation;
this specialist's position is that none ever should.

---

## 7. Positions on Human Decision Register candidates — POSITIONS ONLY, NONE DECIDED

Per this task's constraints and this specialist's Authority. Four of the
six are genuinely Risk-relevant; the orchestrator retains the call on
which are formally opened.

**HDR-J-1 (fallback to tenant/brand jurisdiction) — agree it is genuinely
human; one binding constraint on any answer.** A fallback value, if ever
authorized, **must never be injected into `RiskRequest.JurisdictionCode`
as if it were a resolved player jurisdiction.** `Rule.matches`
(`evaluator.go:174`) compares a bare string and cannot distinguish a
fallback from a real resolution; a jurisdiction-scoped `HARD_LIMIT` would
then be applied — or, worse, *evaded* — on a fabricated basis, with no
artifact recording that it was fabricated (C-5). If J-1 is answered "yes,
under conditions X," the fallback must be a distinct `Basis` on the
resolution record, and whether that basis is sufficient for an
enforcement-grade decision must be settled **in the resolver, per
operation class** (§3.1), before Risk ever sees a value. Risk endorses
IC §3's interim posture (no fallback, ever, until answered) as safe for
this domain: per §2.1 it is strictly non-regressive.

**HDR-J-2 (which signal governs which operation class) — agree it is
genuinely human; confirm IC's per-jurisdiction-configuration note, with
§4.1's one-operation-one-jurisdiction invariant and §4.2's
bootstrap-circularity correction.** See §4 in full.

**HDR-J-3 (privacy/lawful basis for a player geographic attribute) — no
Risk position on the decision; one clarification for the record.**
`internal/risk` consumes **no PII and must never receive any**. It takes a
`jurisdictions.code` string, never a residence value, a country, an
address, or a document field. Whatever HDR-J-3 decides, Risk does not
widen the privacy surface and should not be cited as a consumer requiring
the collection. This also means Risk has no stake in IC §1's
`player_accounts`-vs-`persons` placement question and takes no position on
it.

**HDR-J-4 (obligations in flight when jurisdiction changes) — agree with
IC's POSTURE, dissent on its MECHANISM as applied to Risk.** IC §3
proposes "apply the **stricter** of the two applicable rule sets" as a
safe engineering default. The posture (tighten, never weaken) is right and
Risk endorses it. **The mechanism is not computable over Risk's rule
model**, for three code-grounded reasons:

1. **Two jurisdictions' rule sets are not totally ordered.** Jurisdiction
   A may carry a lower `max_amount` on `casino_bet` and no
   `cumulative_amount` cap; B the reverse. There is no "stricter" rule
   set, only a stricter *outcome for this specific request*.
2. **Merging rule sets would manufacture `ErrConflictingRules`.** Two
   equally-specific `RuleConfigurableLimit` rules for the same
   `(limit_kind, time_window)` — one per jurisdiction — tie on
   `specificity()` and trip `evaluator.go:488`, turning a
   consumer-protective tightening into a **fail-closed outage** for that
   operation. `specificity()` cannot break the tie, because the two rules
   narrow the *same* dimension to *different* values.
3. **It contradicts ADR 0031 §9's explicit invariant:** "A `RiskRequest`
   carries exactly one `JurisdictionCode` value, never a set."

**Risk's proposed mechanism for HDR-J-4's enforcement half, if it is
adopted: merge OUTCOMES, never rule sets.** Evaluate twice — once under
the frozen/original jurisdiction, once under the current — and take the
most restrictive `Outcome` by the precedence the evaluator already fixes
(`DENY > REVIEW > ALLOW`, `evaluator.go:518-529`). `Outcome` **is** totally
ordered; rule sets are not. This preserves the one-code-per-request
invariant, needs no new matching logic, and each call's `MatchedRules`
remains separately explainable. Two disclosed costs: the cumulative-usage
ledger query (`cumulative.go:262-312`) runs twice, and both calls hash to
the **same** advisory-lock key (the key omits jurisdiction, §6.1), so the
second acquisition is a no-op on an already-held `xact` lock — correct,
but worth a `qa`-owned concurrency test if this is ever built. **Nothing
here is proposed for implementation**; it exists so that whichever way
HDR-J-4 is answered, its enforcement half has a mechanism that Risk can
actually execute.

**HDR-J-5 (BYOL — whose determination governs) — agree it is genuinely
human; Risk is directly implicated and has an interim constraint.**
`risk_rules.licensing_mode` (migration 0042) exists *precisely* to stop a
platform-licence `HARD_LIMIT` from binding a BYOL tenant operating under
another regulator (ADR 0031 §10; `types.go:176-185`). If J-5 resolves as
"tenant-supplied or tenant-overridable," then a tenant-influenced
jurisdiction value plus tenant-authored rules means a BYOL tenant could
select a jurisdiction that **dodges a platform-wide jurisdiction-scoped
`HARD_LIMIT`** — the exact evasion `casino.LaunchGameParams`' own doc
comment names, transposed from the player to the tenant. **Interim
constraint this specialist adopts for its own domain regardless of how J-5
resolves:** a platform-wide rule expressing the *platform's own licence's*
legal ceiling should be scoped by `licensing_mode =
'under_platform_licence'` — a value resolved server-side from
`tenants.licensing_model`, which a tenant cannot influence — and **not**
by `jurisdiction_code` alone, until J-5 is settled. This is already the
documented discipline at `types.go:176-185`; it is restated here because
Stage 4I is when it starts to matter.

**HDR-J-6 (permitted markets) — no Risk position on the decision; one
boundary.** Validating a resolved jurisdiction against
`licences.permitted_markets` (the reconnaissance's Q-11) is **not Risk's
job and must not become a `risk_rule`.** Risk rules *match* a
jurisdiction; they do not decide whether the tenant may lawfully serve it.
This is the same separation `denomination.go:57-62` already draws for
assets (registry read ≠ authorization decision), and collapsing it would
make a licence-scope violation indistinguishable from a limit breach in
`RiskDecision.Code`.

---

## 8. Summary, and items routed onward

**Rulings (this specialist's own domain, effective as proposals for the
chain to review; no code changed):**

- **R-1 (§1)** — `risk.Evaluate` keeps its conditional fail-closed
  jurisdiction contract. It is not a weaker sibling of
  AssetAuthorization's unconditional deny; it is the **same invariant**
  correctly specialised to a configurable rule set. **C-3 is two
  contracts and two defects, not four incompatible contracts** (§1.4), and
  §1.4 offers proposed canonical wording for Q-3.
- **R-2 (§2)** — **no shadow/audit-only mode in the Risk engine**; it
  cannot be made coherent with `missingScopeContext` and is not needed,
  because Risk's blast radius is **provably bounded** (§2.1: outcomes are
  invariant under `"" → "XX"` for every jurisdiction-unscoped rule) and
  **statically enumerable** (§2.2). Replaced by a fixed activation order
  (resolver first, rules second) and an **authoring-time precondition** in
  `CreateRule` (§2.4b). The `RuleRiskSignal`-as-shadow trap and the
  `MatchedRules`-discarded-on-ALLOW observability gap are both disclosed
  (§2.3, §2.5).

**Requirements and recommendations routed to later phases:**

| To | Item | Where |
|---|---|---|
| `architect` | Q-3 canonical absent-value wording — adopt the *invariant*, not AssetAuthorization's specialisation | §1.4 |
| `architect` | Q-1: resolver returns a **record**; Risk consumes `.Code` only for matching, provenance goes to audit | §3.1, §3.2 |
| `architect` | Q-2: record must expose both `id` and `code`; a shared translator must keep "unresolved" and "unknown code" distinct and both non-ALLOW | §3.3 |
| `architect` | The one-operation-one-jurisdiction invariant; and the **bootstrap circularity** in per-jurisdiction precedence — key it on the tenant/licence side | §4.1, §4.2 |
| `architect` | Whether doc 34 §5.3 gains a **rule 0** (jurisdiction resolved once, read-only, no locks, before the gate chain) | §6.3 |
| `architect` / Q-15 | The resolver must **not** be owned by `internal/risk` | §4.3 |
| `security` | Review of §2.4b's `CreateRule` precondition before implementation; and of the `risk_manager` write path for jurisdiction-scoped rules | §2.4 |
| `backend` | **H-1** eager-not-lazy resolution; **H-2** resolver must be read-only on the evaluation path (live AB-BA shape); **H-3** no network I/O between Risk's advisory lock and commit | §6.2, §6.3, §6.4 |
| `casino` | Blocklist: keep the mechanism, fix the contract — fail closed with a **distinguishable** sentinel, single resolution source, **decide demo-mode explicitly**, consistent return shape | §5.2 |
| `casino` | Add `jurisdiction_code`/`licensing_mode` to `evaluateAndAuditRisk`'s denial metadata — correct and useful today, independent of the resolver | §3.2 |
| `bonus-engine` | `resolveJurisdictionID`'s collapse of empty and unknown codes to `uuid.Nil` is a latent fail-open for any Risk-adjacent use | §3.3 |
| `payments` | C-3(d): empty-means-permissive on a **different code space** (ISO country, not `jurisdictions.code`) — two defects, not one | §1.4 |
| `qa` | Assert §2.2's enumeration returns zero rows at activation; jurisdiction precedence/concurrency coverage must be built on `MatchedRules` from `Evaluate`, not audit records; and see §7 HDR-J-4's double-evaluation advisory-lock note if that mechanism is ever built | §2.4a, §2.5, §7 |
| Orchestrator | HDR positions on J-1, J-2, J-4, J-5 (and boundaries on J-3, J-6). **None decided here** | §7 |

**Labelled status of this deliverable, per CLAUDE.md's "No fake
completion":** `NOT IMPLEMENTED` — this is a design-phase document.
`internal/risk` is unchanged at this commit; `risk.Evaluate`'s
jurisdiction behaviour today remains exactly as ADR 0031 §34/§42(a)
describe it, and no resolver exists.
