# Jurisdiction Human Decision Brief — HDR-J-1 through HDR-J-6

**Purpose of this document.** This is a decision brief, not a new register
entry and not a decision. It exists to put the six jurisdiction-related
Human Decision Register items — already opened, in exact wording, in
`docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`
("the register") — in front of the human owner with the technical
consequences of each option made explicit, so the decision can be made
with full information. **No decision is selected, recommended, or
defaulted anywhere in this document.** Where the canonical architecture
document (`docs/governance/stage-4i-canonical-model.md`, "CANON") or the
register itself already assessed relative leverage or urgency, that
assessment is reported as *their* finding, attributed, exactly as the
register itself reports it — never adopted here as an independent
recommendation.

**Status of the underlying platform, unchanged by this document.**
`PARTIALLY IMPLEMENTED` (per `docs/governance/stage-4i-report.md`). A real,
tested, independently-certified jurisdiction resolution foundation exists
(`internal/jurisdiction`, migrations `0071`-`0073`, a registry admin
surface, an append-only resolution-record table). It does not yet resolve
any player's actual jurisdiction. Every player-scoped resolution today
returns `unresolved(no_signal)`. This document changes nothing about that
state; it exists only to make the six decisions gating it legible.

**Sources used, cited by tag throughout:** REGISTER
(`docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`),
CANON (`docs/governance/stage-4i-canonical-model.md`), RECON
(`docs/governance/stage-4i-reconnaissance.md`), IC
(`docs/governance/stage-4i-identity-compliance-model.md`), RISK
(`docs/governance/stage-4i-risk-model.md`), SEC
(`docs/governance/stage-4i-security-model.md`), REPORT
(`docs/governance/stage-4i-report.md`). No content below is invented; every
factual claim traces to one of these six documents or to the code/schema
they cite.

---

## How to read this brief

Each decision is presented as: exact question, why it exists, current
platform behavior, what remains blocked, affected domains, the named
option(s), consequences by category, required implementation changes,
dependencies on other items, what would remain unresolved either way, and
whether answering it unblocks anything. Options are named exactly as the
source documents name them. Where a source document frames a decision as
open-ended configuration content rather than a two-option choice (this is
true of J-2, J-3, and J-6), this brief says so rather than inventing an
"Option A / Option B" structure that does not exist in the source
material.

---

## Decision HDR-J-1

### 1. Exact decision question

> **May a missing/unresolved player jurisdiction ever fall back to the
> tenant's or brand's own jurisdiction?**

Full framing (REGISTER): "When the platform cannot determine a player's
own jurisdiction for a given operation, may it ever substitute the
tenant's (or brand's) own licensing jurisdiction as a stand-in, rather
than treating the operation as having no determined jurisdiction at all
(which, per the platform-wide invariant, means the operation fails closed
wherever a jurisdiction-dependent policy is in force)?"

### 2. Why the decision exists

The resolver's `basis` enum reserves a value, `platform_fallback`, for
exactly this outcome, precisely so that answering "yes" later does not
require a schema migration — but the resolver is structurally incapable of
producing it today (CANON §3.1). The decision exists because substituting
a known fact (the tenant's jurisdiction) for an unknown one (the player's)
is a policy choice about acceptable risk, not an engineering default —
and because the platform's own basis the resolver *can* already produce
(`tenant_licence`) is explicitly forbidden from being used this way except
by a database `CHECK` constraint, closing off the most likely accidental
route to the same outcome (CANON §3.2).

### 3. Current platform behavior

An unresolved player-scoped jurisdiction stays `unresolved(no_signal)`
permanently for that operation. This is enforced structurally, not by
convention: `basis = tenant_licence` may never be selected for an
operation whose subject is a player — a resolution with a non-NULL
`player_account_id` cannot carry `selected_basis = 'tenant_licence'`, a
rule enforced by a database `CHECK` constraint (CANON §3.2). No code path
anywhere substitutes a tenant or brand value for a missing player value.

### 4. What remains blocked

**The Bonus deposit sweep and the cashback scheduler cannot issue real
grants.** `AssetAuthorization`'s eligibility check denies unconditionally
when the resolver returns anything other than `resolved` (CANON §4.1),
and every player-scoped resolution today returns `unresolved(no_signal)`
regardless of this decision — because the deeper blocker is HDR-J-3, not
this one (REGISTER). Answering "yes" to this decision alone does not
unblock Bonus issuance: it would only do so once (a) HDR-J-3 supplies a
player-side signal to be absent in the first place, or (b) the fallback
basis is built as a genuinely new producer and wired into specific
operation classes this decision would need to authorize it for
(REGISTER).

### 5. Domains affected

Bonus (deposit sweep, cashback scheduler — the direct blocked path), Risk
(the consuming gate that would receive a fallback value), AssetAuthorization
(the gate whose unconditional deny this decision would need to route
around for specific operation classes), Audit (the record must remain
distinguishable per §10 below), and the jurisdiction resolver itself (the
`platform_fallback` basis producer this decision would need to build).

### 6. Option A

**No fallback, ever.** An unresolved player-scoped jurisdiction stays
`unresolved(no_signal)` permanently for that operation, regardless of how
confidently the tenant's own jurisdiction is known. This is the current,
structurally-enforced state (REGISTER).

### 7. Option B

**A bounded fallback is permitted.** For some operation classes, under
some conditions, an unresolved player jurisdiction may resolve to the
tenant's (or brand's) own licensing jurisdiction instead of remaining
unresolved. This would require building a new resolver basis producer
(`platform_fallback`) where today none exists, and deciding, for each
operation class, whether that basis is acceptable at all (REGISTER).

### 8. Other valid options already documented

None. The register presents this as a binary (fallback permitted / not
permitted), with the scope of "when permitted" (which operation classes)
left as a further, un-named sub-question inside Option B rather than a
separate named option.

### 9. Technical consequences of each option

- **Option A**: No new resolver basis producer is built. No change to any
  consuming gate. The reserved-but-unproducible `basis` value and the
  `CHECK` constraint preventing its misuse remain exactly as they are —
  both already specified and buildable regardless of which way this is
  eventually answered (REGISTER).
- **Option B**: Requires building `platform_fallback` as a genuine new
  basis producer in `internal/jurisdiction`, and — per RISK's binding
  constraint — the fallback value must never be injected into
  `RiskRequest.JurisdictionCode` as a bare, indistinguishable string,
  because `Rule.matches` compares that field by string equality and has
  no way to tell a resolved player jurisdiction from a substituted tenant
  one (RISK §7, CANON §10.2). The consuming gate must be able to refuse
  the fallback per operation class rather than treat it as equivalent to
  a resolved player fact.

### 10. Security consequences

**Option B only** (security's binding constraint, SEC §S-9, CANON §10.2):
a fallback value, once written into a resolution record that later feeds
an immutability-trigger-protected snapshot (the pattern this platform
already uses for casino launch sessions, bonus grants, and similar
operation-level records), becomes a **permanent, unfalsifiable claim**
that a jurisdiction fact was established when it was not. This is worse
than no record, because it looks authoritative to a regulator reading it
later. Any Option-B answer therefore also requires deciding how that
record is later distinguished, on inspection, from a genuinely resolved
player fact.

### 11. Privacy/data consequences

None directly — this decision does not itself introduce a new player data
attribute (that is HDR-J-3). It does determine whether the *absence* of
such an attribute can be papered over with a substituted value.

### 12. Financial consequences

Option B, if built and misapplied, could allow a jurisdiction-scoped
`HARD_LIMIT` or an asset-eligibility gate to apply (or fail to apply)
based on a substituted rather than a real player fact — a compliance
event if the substitution is wrong for that player, not merely a
technical one (REGISTER's framing: "a compliance event if wrong").

### 13. Regulatory/compliance implications

This is a compliance risk-acceptance question: whether the platform may
treat "we don't know the player's jurisdiction, but we know the tenant's"
as an acceptable basis for jurisdiction-gated decisions, for which
operation classes, and under what recorded justification. No specialist
phase in this stage claimed authority to answer this; it was explicitly
routed to the human (REGISTER).

### 14. Required implementation changes if answered "yes" (Option B)

Build the `platform_fallback` basis producer in the resolver; decide and
implement per-operation-class authorization for its use; ensure the
consuming gate (Risk, and any other jurisdiction-scoped gate) can
distinguish and, where required, refuse a fallback value; extend the
audit/record model so a fallback-derived resolution is distinguishable
from a genuinely resolved one on inspection (see §10 above).

### 15. Dependencies on other HDR decisions

**Independently decidable.** This decision does not require any other
HDR item to be answered first. However, its *practical effect* is
entangled with HDR-J-3: today, with no player-side signal existing at
all, a "yes" answer to this decision could in principle apply to every
player-scoped operation (since every one is currently unresolved) — so
answering "yes" here without also constraining scope carefully would have
an immediately broad effect, not a narrow one. Conversely, once HDR-J-3
supplies a genuine player-side signal, this decision's practical scope
narrows to only the residual cases where that signal itself is unavailable
or insufficient. REGISTER states plainly: "This decision and HDR-J-3 are
therefore both real blockers on the same outcome, from different
directions, and answering one does not answer the other."

### 16. What would remain unresolved either way

Whichever way this is answered, HDR-J-3 (whether the platform collects any
player-side jurisdiction signal at all) remains a fully separate,
unanswered question. Answering this decision alone does not supply a
player-side signal; it only decides what happens in its absence.

### 17. Would choosing this decision unblock engineering?

**Partially, and only in combination with other work.** A "yes" answer
does not, by itself, unblock Bonus issuance (see §4). It authorizes
engineering to *build* the fallback producer and its per-operation-class
wiring — work that is not blocked from being scoped and specified today,
but whose actual construction has not begun and is not authorized absent
this decision. A "no" answer requires no new engineering at all; the
current state already implements "no fallback, ever."

---

## Decision HDR-J-2

### 1. Exact decision question

> **Which jurisdiction-relevant signal is legally authoritative when more
> than one exists?**

Full framing (REGISTER): "For a given class of operation (a casino bet, a
KYC determination, a regulatory report, a Bonus grant, and so on), when
more than one jurisdiction-relevant signal exists for the same player — a
self-declared residence, a KYC-reviewer-verified residence, a real-time
location signal — which one governs? And does the answer differ by
operation class?"

### 2. Why the decision exists

Different jurisdiction-relevant facts about the same player can
legitimately disagree (a self-declared residence may differ from a
KYC-verified one, or from a real-time location). Something must decide
which one a given operation trusts, and that "something" is a legal or
compliance judgment about evidentiary weight — not an engineering
convention. No specialist phase in this stage claimed authority to supply
this content (CANON §10.1: "a legal/compliance judgment about which
evidence a jurisdiction, regulator, or licence condition treats as
authoritative for which kind of decision, and none of the four Stage 4I
specialist phases claimed authority to answer it").

### 3. Current platform behavior

A precedence-configuration table exists in schema (`jurisdiction_precedence_configs`,
per REGISTER's corrected implementation-status note) with **zero rows**. A
resolver with no precedence rows resolves `unresolved` for any operation
that would otherwise need to adjudicate between competing signals — the
correct, deliberate Stage 4I steady state (CANON §3.4, §11.3).

### 4. What remains blocked

The precedence table's rows. Nothing about the table's shape, the
Risk-authoring-time precondition that reads related state, or any other
buildable Stage 4I item is blocked by this remaining open (CANON
§11.1/§11.2).

### 5. Domains affected

Risk (jurisdiction-scoped rule matching), AssetAuthorization, KYC/AML
(a "KYC determination" is named explicitly as an operation class this
decision would govern), Reporting (a "regulatory report" is named
explicitly), Bonus (a "Bonus grant" is named explicitly), and the
jurisdiction resolver's own precedence-configuration mechanism.

### 6-8. Options

This is **not** framed by the source documents as a two-option choice. It
is a configuration-content question: for each operation class, which
signal (or ranked order of signals) governs. No specialist phase proposed
specific precedence content, and this document does not invent one. What
**is** already settled, and is not part of this decision, is the
**mechanism**: precedence configuration is keyed on `(tenant licensing
jurisdiction, operation_class)` — both facts knowable before any
player-side signal is consulted, closing a bootstrap-circularity problem
Risk's own review identified in an earlier keying proposal (CANON §3.4).
The mechanism's keying shape is not open for reconsideration as part of
this decision; only the content that populates it is.

A related, already-established rule narrows the shape of any answer
without supplying its content: a self-declared residence is a real
signal, not worthless, but is never on its own sufficient for an
enforcement-grade decision (a jurisdiction-scoped hard limit, an
`asset_authorizations` layer-6 gate, a withdrawal jurisdiction policy)
**unless this decision says otherwise for that operation class
specifically** (CANON §3.3).

### 9. Technical consequences

The precedence table's schema shape does not change regardless of
content. Populating it with real rows is what activates jurisdiction-scoped
Risk rules that are currently dormant for lack of a resolvable
jurisdiction (subject also to HDR-J-3 supplying an actual signal to
prioritize among).

### 10. Security consequences

None distinct from the general resolver security model — the precedence
content itself does not introduce a new attack surface, since the
resolver's non-forgeability and audit properties (CANON §2.3, §5) apply
regardless of what the precedence table says.

### 11. Privacy/data consequences

None directly from this decision alone; it governs how existing/future
signals are weighted against each other, not whether new signals are
collected (that is HDR-J-3).

### 12. Financial consequences

Precedence content directly determines whether and when a
jurisdiction-scoped `HARD_LIMIT` or asset-eligibility gate actually fires
for a given player — i.e., it is the content that turns a currently-inert
control into a live one, once a signal exists to evaluate it against.

### 13. Regulatory/compliance implications

This is squarely a compliance/legal-evidentiary-weight question: which
signal a jurisdiction, regulator, or licence condition would treat as
authoritative for which class of decision.

### 14. Required implementation changes

Populate the precedence-configuration table with real rows, per operation
class. Per the constraints already recorded (CANON §10.2): the content
must be versioned (`resolver_policy_version`) and audited on change; it
needs a four-eyes posture at least as strong as the platform's existing
`bonus_approval_policies` pattern; and it must be expressed in the
`(tenant licensing jurisdiction, operation_class)` shape — a different
shape would risk smuggling in a global default while appearing
configuration-driven.

### 15. Dependencies on other HDR decisions

**Independently decidable** — the precedence keying mechanism does not
require HDR-J-3 to be answered first, and content can in principle be
authored in advance of any signal existing to apply it to. **Practical
effect depends on HDR-J-3** (and, to a lesser extent, HDR-J-1): precedence
content only has something to adjudicate once more than one producible
basis exists for the same player and operation. Today only `tenant_licence`
is producible, and it is forbidden for player-scoped operations (HDR-J-1's
subject matter) — so, absent HDR-J-3 supplying at least one player-side
basis, this decision's content remains inert even once authored.

### 16. What would remain unresolved either way

HDR-J-3 (whether any player-side signal exists to prioritize among) and
HDR-J-1 (whether a non-player-side fallback basis exists as a competing
candidate) both remain open regardless of how this decision is answered.

### 17. Would choosing this decision unblock engineering?

The table shape is already buildable and is not blocked by this decision
remaining open. Authoring real content does not, by itself, unblock any
currently-blocked engineering item — its effect is latent until a
player-side or fallback basis exists to apply it to.

---

## Decision HDR-J-3

### 1. Exact decision question

> **Should the platform collect a player residence/location/nationality
> attribute at all, and under what lawful basis and retention rule?**

Full framing (REGISTER): "Does the platform collect any jurisdiction-relevant
fact about a player at all — a self-declared residence, a KYC-reviewer-verified
residence, a real-time location signal, or nationality — and if so, on
what lawful basis, with what retention rule, and with what access
controls?"

### 2. Why the decision exists

Collecting any of these facts is a new category of player PII with its own
lawful-basis and retention obligations. No specialist phase in this stage
claimed authority to make that lawful-basis determination — it is
explicitly a legal/compliance judgment, and CLAUDE.md already forbids
engineering from inventing lawful basis/retention rules even for
existing, lighter categories of PII (IC's own framing, REGISTER/CANON
§10.2).

### 3. Current platform behavior

**No player location, residence, or nationality attribute exists in any
form anywhere in the codebase** (REPORT §3, RECON, CANON §1). The
resolver has no player-side input of any kind. Every player-scoped
jurisdiction resolution returns `unresolved(no_signal)` — not because the
resolver is unbuilt (most of the surrounding machinery is buildable and
built, CANON §11.1), but because there is nothing for it to read (CANON
§11.2, §11.3).

### 4. What remains blocked

Everything player-scoped: the Bonus deposit sweep and cashback scheduler
(directly, via `AssetAuthorization`'s fail-closed layer 6); any future
jurisdiction-scoped `HARD_LIMIT` Risk rule as applied to a specific
player; the casino per-game blocklist for any game whose blocklist array
is ever populated (currently none are, so this has no live effect yet,
but the mechanism denies the moment one is populated, CANON §9.2); and any
KYC/AML/reporting-class determination that would consult a residence
fact. None of this is a missing engineering capability — the resolver,
the registry, and the audit/record model are all buildable and largely
already built (CANON §11.1). What is missing is the fact itself.

Concretely, the following migrations are themselves blocked on this
decision, not merely the data they would collect: `player_accounts.
declared_residence_country` + `captured_at`; `kyc_verifications.
verified_residence_country` + `source_document_id`; any nationality field
(CANON §11.2).

### 5. Domains affected

Identity (`player_accounts` is the proposed home for a declared residence
attribute — never `persons`, which is deliberately platform-scoped and
shared across a person's brands, IC's placement ruling adopted in full at
CANON §1.2), KYC (`kyc_verifications.verified_residence_country`, set only
by an explicit reviewer determination, is the proposed home for a verified
residence attribute), the jurisdiction resolver (gains its first
player-side basis), AssetAuthorization (its layer-6 gate would begin
receiving a real value instead of unconditional absence for
player-scoped operations), Risk (jurisdiction-scoped rules gain a real
input), Casino (the per-game blocklist becomes exercisable), Bonus (the
deposit sweep, cashback scheduler, and manual grant issuance gain a path
to succeed), Audit (a new category of evidence reference, though never the
raw value, per §5.3's "must never be recorded" list), and — if a real-time
location signal is ever authorized as part of this decision — a future
geolocation vendor becomes a hard dependency of the casino launch path
(CANON §9.4, a named, separately-flagged consequence, not decided here).

### 6-8. Options

This is **not** a two-option choice in the source material. It is a
compound lawful-basis/collection-scope question with several independent
dimensions the register and canonical model separate explicitly:

- **Whether to collect a declared (self-reported, unverified) residence**
  at all.
- **Whether to collect a verified (KYC-reviewer-confirmed) residence** at
  all — a related but analytically separate question, since the two live
  on different tables with different access-control weight (`player_accounts`
  vs. `kyc_verifications`).
- **Whether to collect nationality** — explicitly gated on **both** a "yes"
  to residence collection **and** a demonstrated operational need; a "yes"
  to residence does **not** automatically extend to nationality (CANON
  §1, "a narrower, two-part test recorded so nationality is not collected
  merely because residence was authorized").
- **Whether to authorize a real-time location signal** as an input — gated
  on this decision **plus** a still-open provider-interface question
  (RECON Q-7), and carrying its own separate operational consequence (a
  location vendor becomes a hard dependency of the casino launch path,
  CANON §9.4, §12.2) — recorded as a named future consideration, not
  decided here.

A "no" to all of the above is the current, structurally-enforced default
and requires no engineering change.

### 9. Technical consequences

A "yes" to declared/verified residence enables the resolver's
`player_declared_residence` and/or `player_verified_residence` basis
values, both already reserved in the closed `basis` enum but structurally
unproducible today (CANON §3.1). Building the producer is not itself
blocked by anything other than this decision. A "yes" to a real-time
location signal additionally requires resolving RECON Q-7 (a provider
interface question) and introduces a new failure domain: today the
resolver has "no external network dependency," so "resolver availability
equals database availability" (CANON §9.4, SEC §S-5.2) — a location
vendor breaks that property, meaning a third party could then affect
whether the casino lobby is reachable at all. This consequence is
explicitly flagged and explicitly not decided by this brief.

### 10. Security consequences

A "yes" answer of any kind requires (CANON §10.2, echoing SEC's own Stage
4I review): the attribute is **projected out of every existing read that
does not need it** — explicit `SELECT` lists, never `SELECT *` — so
reading it is a deliberate act, never an incidental side effect of an
unrelated query; and it requires **its own permission**, separate from
ordinary player-read (`player:read`), so that reading this specific
attribute is itself an auditable, narrowly-granted act.

### 11. Privacy/data consequences

A new PII category is created regardless of table placement (CANON §10.2).
Four concrete costs are recorded up front, per the specialist chain's own
framing:

1. A new sensitive attribute, proposed to live on `player_accounts`
   (tenant-scoped), never on `persons` (platform-scoped, shared across a
   person's brands) — IC's placement ruling, adopted in full.
2. Its own column-level access-control discipline (explicit projection,
   never `SELECT *`).
3. Its own permission, separate from ordinary player-read.
4. A required update to `docs/architecture/16-privacy.md`, recording the
   field, its sensitivity classification, and its access controls.

Separately, the audit/provenance model already governs what may **never**
be recorded once a residence fact exists (CANON §5.3, binding regardless
of how this decision is answered, since it also governs the currently-inert
`kyc_corroboration` mechanism): the resolved `jurisdiction_code` itself is
recorded (this is the decision, and not recording it was already
identified as a defect), but the underlying residence/nationality/location
**value** is never recorded in the resolution audit trail — only a
reference to where the evidence actually lives, under that evidence's own
access control and retention rule.

### 12. Financial consequences

This is the item that, if answered "yes" with an actual producible signal,
would allow the Bonus deposit sweep and cashback scheduler to begin
issuing real grants for the first time (currently blocked unconditionally)
— a direct, first-order financial consequence, since it activates real
money-adjacent automation that is currently structurally inert.

### 13. Regulatory/compliance implications

This is the platform's central "what jurisdiction-relevant fact may we
hold about a player, and on what lawful basis" question — CLAUDE.md
already treats lawful-basis/retention decisions for existing PII as
outside engineering's authority; a new, more sensitive category cannot
receive less scrutiny (IC's own framing, adopted). A "yes" answer commits
the platform to a data-protection posture (lawful basis, retention period,
data-subject rights handling) that has legal consequences independent of
the engineering that follows from it.

### 14. Required implementation changes if answered "yes"

Migrations for whichever attribute(s) are authorized
(`player_accounts.declared_residence_country`/`captured_at`,
`kyc_verifications.verified_residence_country`/`source_document_id`, and/or
a nationality field); the corresponding basis producer(s) in
`internal/jurisdiction`; column-level projection discipline on every
existing read touching the affected table; a new, narrowly-scoped read
permission; an update to `docs/architecture/16-privacy.md`. If a
real-time location signal is authorized: a provider-interface design
(RECON Q-7), a `geo_signal` basis producer, and a separate decision plus
its own security review on the resulting hard dependency on a vendor
(explicitly named as a separate future item, not covered by this
decision alone).

### 15. Dependencies on other HDR decisions

**Independently decidable — this is the item every other item's practical
effect depends on, not the reverse.** Answering HDR-J-1, J-2, J-4, or J-6
does not require this decision to be answered first, and this decision
does not require any of them to be answered first either. But the
canonical model's own assessment, reported here rather than adopted, is
that "every one of the other five items either depends on this one or is
materially smaller in consequence" (CANON §10.1) — meaning a "yes" here,
paired with an actual producible signal, is what gives HDR-J-1's fallback
question, HDR-J-2's precedence content, and HDR-J-4's record-of-authority
question something real to operate on, and gives HDR-J-6's permitted-market
check a resolved value to validate against.

### 16. What would remain unresolved either way

Even a "yes" answer here does not by itself resolve HDR-J-1 (whether a
fallback is separately permitted for cases this attribute still can't
resolve), HDR-J-2 (which signal wins when this attribute conflicts with
another), HDR-J-4 (which jurisdiction's record governs when this
attribute's resolved value changes mid-obligation), or HDR-J-6 (which
markets a resolved jurisdiction is actually permitted to serve).

### 17. Would choosing this decision unblock engineering?

**Yes, materially, if answered "yes" with an actual authorized attribute.**
This is the one decision among the six whose "yes" answer directly
authorizes new migrations and a new resolver basis producer that are
otherwise fully specified and ready to build (CANON §11.2: "Not the
schema placement... and not the column shapes... Only the act of
collecting"). A "no" answer requires no new engineering and leaves the
current, fully fail-closed state exactly as it is.

---

## Decision HDR-J-4 (narrowed)

### 1. Exact decision question

> **When an obligation is already in flight and a player's jurisdiction
> changes, which jurisdiction's determination is authoritative for
> regulator-facing record-keeping?**

Full framing (REGISTER): "If a player's resolved jurisdiction changes
while an obligation is already open — an in-progress bet, an active
wagering requirement — which jurisdiction's determination is authoritative
for a regulator-facing record-keeping purpose: which jurisdiction's SAR
(suspicious-activity report) obligation attaches, or which jurisdiction's
data-residency rule governs where the record is retained?"

**This is a narrowed version of the item as originally identified.** The
original tension had two halves. The other half — what the platform
actually *enforces* going forward once two candidate jurisdictions are in
play for the same operation — has already been technically adjudicated
by `architect`'s synthesis and needs no human input: **Most-Restrictive-Outcome
Composition (MROC)** — each jurisdiction-dependent gate is evaluated once
per candidate jurisdiction and the operation takes the most restrictive
outcome by that gate's own declared order, with the underlying compliance
posture (tighten, never weaken) fully preserved (CANON §7). MROC answers
what the platform *enforces*; it does not, and must not be read to,
answer what the platform *reports* — that reporting question is what
remains genuinely human, and is this decision's actual scope (CANON §7.4).

### 2. Why the decision exists

A regulator-facing obligation (a SAR filing, a data-residency rule) needs
exactly one authoritative jurisdiction of record when two candidates
disagree, and choosing which one is a regulatory-record-keeping judgment,
not an enforcement-engineering one.

### 3. Current platform behavior

**Nothing today exercises this.** MROC's composition machinery is not
built in Stage 4I, and cannot be exercised yet: no two-candidate case can
arise, because there is no resolver output for a player at all
(HDR-J-3 is unanswered), so there is no second candidate to compose with
(CANON §7.4).

### 4. What remains blocked

Nothing today. The only Stage 4I obligation tied to this item is a
non-foreclosure one — no `UNIQUE` constraint keyed on an operation in the
resolution table, and no speculative composition column — both already
satisfied by the current schema design (CANON §5.2, §7.4). This item is
recorded now so it is not lost, not because anything depends on it today.

### 5. Domains affected

Reporting (SAR-style regulatory filings), Audit/record-keeping (data
residency), and — for the already-adjudicated enforcement half, not part
of this decision — every jurisdiction-dependent gate (AssetAuthorization,
Risk, Casino, Bonus) via MROC's composition mechanism.

### 6-8. Options

Not framed as named options in the source material. The decision is:
which jurisdiction's record is designated authoritative for
regulator-facing purposes when two candidates exist and differ — a
content question, not a binary choice, and no specialist phase proposed
specific candidate answers.

### 9. Technical consequences

Whichever jurisdiction's record is designated authoritative, the
recording mechanism is already in place regardless of the answer: when
two candidate jurisdictions produce different outcomes, the record shows
**both** — one `jurisdiction_resolutions` row per candidate, with the
gate's own decision record naming which candidate produced the governing
outcome (CANON §7.3). This means the answer to this decision, once given,
is a rule for interpreting an already-complete record, not a prerequisite
for the record existing.

### 10. Security consequences

None beyond what the already-built recording mechanism already provides
(both candidates are always recorded, regardless of this decision).

### 11. Privacy/data consequences

None directly; this concerns which existing resolved value is treated as
authoritative for reporting, not the collection of a new attribute.

### 12. Financial/regulatory consequences

Determines which jurisdiction's SAR obligation attaches and which
jurisdiction's data-residency rule governs record retention when a
player's jurisdiction changes mid-obligation — a genuine regulatory
consequence, since it could determine to which regulator a report is
owed.

### 13. Required implementation changes

None currently pending — the recording mechanism (per-candidate resolution
rows) is already specified and non-foreclosed by the current schema. An
answer would be encoded as an interpretation rule applied to an
already-complete record, not a new data-capture requirement.

### 14. Dependencies on other HDR decisions

**Cannot be practically exercised until HDR-J-3 is answered** — the
enforcement half needs at least one player-side signal to exist so that
two candidates could ever be in play; the same is true for this,
narrower, reporting half. It does not, however, require HDR-J-1, J-2,
J-5, or J-6 to be answered.

### 15. Not to be confused with

`docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`'s Decision
1 (the `OpenBetSelfExclusionPolicy` platform-wide default). That decision
asks what happens to an open sportsbook bet when a *player becomes
self-excluded*. This item asks which jurisdiction's *record* governs a
regulator-facing obligation when a player's *resolved jurisdiction itself
changes* while an obligation is open — a different triggering event and a
different question. A human answering one does not answer the other, and
neither document merges them (REGISTER).

### 16. What would remain unresolved either way

HDR-J-3 remains the practical precondition for this decision ever
mattering. Regardless of how this is answered, MROC's enforcement
mechanism (already adjudicated, not part of this decision) governs what
the platform does going forward; this decision only governs how the
resulting record is interpreted for reporting purposes.

### 17. Would choosing this decision unblock engineering?

No engineering is currently blocked by this remaining open. It is
recorded for completeness so it is not lost by the time it becomes
practically relevant.

---

## Decision HDR-J-5

### 1. Exact decision question

> **For a BYOL tenant, whose jurisdiction determination governs: the
> platform's, or the tenant's own?**

Full framing (REGISTER): "For a tenant operating under its own licence in
its own jurisdiction (rather than under the platform's Anjouan licence) —
the hybrid licensing model `docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md`
already establishes as the platform's future direction — whose
jurisdiction determination governs a given operation: the platform's own
resolution, or the tenant's own asserted determination?"

### 2. Status: registered for completeness, not routed as urgent

**No BYOL (bring-your-own-licence) tenant exists on this platform today.**
This item is recorded so it is not lost by the time one is contemplated —
not because it blocks any current work. The specialist chain's own
assessment, reported here rather than adopted: this is the lowest-urgency
item of the six, and it can safely wait (REGISTER).

### 3. Why the decision exists

The hybrid licensing model (ADR 0006) already establishes that some future
tenants will operate under their own licence rather than the platform's.
When that happens, a genuine question arises about whose jurisdiction
determination is authoritative for that tenant's operations.

### 4. Current platform behavior

Both possible answers remain open at zero present cost. A resolver
`basis` value for a tenant's own assertion (`tenant_asserted`) is reserved
in the closed `basis` enum specifically so that answering this decision
later does not require a schema redesign — but the resolver is
structurally incapable of producing it today (CANON §3.1).

### 5. What remains blocked

Nothing. There is no BYOL tenant to be affected, and the reserved,
currently-unproducible `basis` value carries zero present cost either way
(CANON §11.2).

### 6. Domains affected

None today (no live BYOL tenant). When relevant: Risk (platform-licence
ceilings, which already remain scoped by `licensing_mode` regardless of
this decision's answer — see §9 below), the jurisdiction resolver (a new
basis producer), and Audit.

### 7. Option A

The platform's own resolution governs, even for a BYOL tenant.

### 8. Option B

The tenant's own asserted determination governs (subject to the
constraint in §9 below).

### 9. Consequences of either option

Independently of whichever way this is eventually answered,
platform-licence ceilings already remain scoped by `licensing_mode` — a
value resolved server-side from `tenants.licensing_model`, which no
tenant can influence — so a future BYOL tenant's own assertion, if
authorized, would never be able to raise itself above a platform-set
ceiling (RISK §7, SEC §S-9, CANON §10.2). If this is ever answered such
that a BYOL tenant's own determination is given some weight, that
determination must be recorded as its own distinct `basis`
(`tenant_asserted`) and never merged into, or made indistinguishable
from, a platform-resolved value (CANON §10.2) — the same non-negotiable
non-forgeability discipline the platform applies to every other
jurisdiction-bearing value.

### 10-13. Security/privacy/financial/regulatory consequences

Not yet applicable — no BYOL tenant exists to be affected. When one is
contemplated, the security consequence is fully specified already (§9's
distinct-basis, non-forgeable-recording requirement); no new privacy
consequence is implied (this concerns tenant-level, not player-level,
determination); the financial/regulatory consequence would depend on the
specific BYOL tenant's own licensing terms, not decidable in the
abstract.

### 14. Required implementation changes

None currently authorized or needed. If answered: build the
`tenant_asserted` basis producer (already reserved in the enum) and wire
it per whichever operation classes the answer authorizes.

### 15. Dependencies on other HDR decisions

**Fully independent.** Does not depend on, and is not depended on by, any
of the other five items.

### 16. What would remain unresolved either way

Nothing tied to this decision specifically; it is self-contained.

### 17. Would choosing this decision unblock engineering?

No — there is no blocked engineering item tied to this decision today.

---

## Decision HDR-J-6

### 1. Exact decision question

> **Which specific markets/jurisdictions may the platform's first
> (Anjouan-licensed) B2C brand actually serve?**

Full framing (REGISTER): "Which countries or regulatory jurisdictions is
the platform's first, Anjouan-licensed B2C brand actually permitted to
serve — the concrete, enumerable list a resolved jurisdiction should be
checked against before an operation is allowed to proceed?"

### 2. Why the decision exists

`CLAUDE.md`'s own "When to stop and ask" section already lists
"jurisdiction selection beyond what's already fixed (Anjouan)" as
something requiring explicit human authorization, not an engineering
call. This item does not introduce a new requirement — it formally opens,
in the register, a decision CLAUDE.md already flags as needing to be
asked, now that Stage 4I work has surfaced its concrete technical hook.

### 3. Current platform behavior

`licences.permitted_markets` is a column that already exists in the
schema but is currently **empty and unread by any code path** (CANON
§10.1). A resolver that successfully returns a jurisdiction is not, on
its own, worth anything without a permitted-market list to validate that
jurisdiction against.

### 4. What remains blocked

The permitted-market **validation content** stays unbuilt — this is an
explicit, recorded deferral (closing an open question, RECON Q-16, at
CANON §4.5), not a silent one. This is a narrower, catalogue/availability-side
gap than HDR-J-3: it does not by itself block Bonus issuance or any of
the mechanisms HDR-J-3 gates, but it does mean that even once a
player-side jurisdiction signal exists, the platform has no enumerated
answer to "is this market one we may actually serve" to check that signal
against.

### 5. Domains affected

Casino (catalogue availability), Payments (routing dimension 2, currently
an explicit `TODO(jurisdiction)`, distinct from this decision but related),
Withdrawal (the `withdrawal_policies.jurisdiction_code` `CHECK (...IS NULL)`
constraint, migration 0033, explicitly not lifted in Stage 4I regardless
of this decision — a separate, deliberate deferral), Reporting, and the
jurisdiction resolver/registry (`licences.permitted_markets`).

### 6-8. Options

Not framed as named options — this is an enumerable-content question
(which specific markets/countries), not a binary choice.

### 9. Technical consequences

Whatever the permitted-market list ends up being, validating a resolved
jurisdiction against it is an **authorization** check, not a Risk rule —
and it must be distinguishable in its own reason code from an ordinary
Risk limit breach and from a casino blocklist hit (CANON §10.2). This
matters operationally: a licence-scope violation surfacing as a generic
denial is, in the specialist chain's own words, "an incident nobody can
triage" — so however the list is populated, the mechanism that checks
against it needs its own distinguishable outcome from day one.

### 10. Security consequences

None beyond the distinguishability requirement in §9 — this is an
authorization-surface design question flagged for a `security` review
when proposed (SEC §S-10, CANON §4.5), not decided by this brief.

### 11. Privacy/data consequences

None directly.

### 12. Financial consequences

Determines which markets the platform's revenue-generating operations
(deposits, bets, bonus issuance) may lawfully be offered in — a
first-order commercial and compliance consequence, though the content
itself is a licensing/legal question outside this document's scope.

### 13. Regulatory/compliance implications

This is the core licensing-scope question CLAUDE.md already flags as
requiring explicit human authorization: which markets the Anjouan licence
(or any future licence) actually permits the platform to serve.

### 14. Required implementation changes

Populate `licences.permitted_markets` with the authorized content; build
the validation mechanism (an authorization check with its own
distinguishable reason code, per §9); wire it into the catalogue/availability
and routing paths that would consult it (a later phase's implementation
work, not built in Stage 4I).

### 15. Dependencies on other HDR decisions

**Independently decidable** in terms of content (the list of permitted
markets can be authored regardless of any other item's answer). Its
**practical enforcement value** depends on there being a resolved
jurisdiction to check against — which depends on HDR-J-3 (or HDR-J-1's
fallback) supplying one. It does not depend on HDR-J-2, J-4, or J-5.

### 16. What would remain unresolved either way

HDR-J-3 (whether any player-side jurisdiction signal exists to check
against the permitted-market list) remains open regardless of how this is
answered. Payments' routing dimension 2 and the `withdrawal_policies`
CHECK constraint remain separately, explicitly deferred regardless of
this decision.

### 17. Would choosing this decision unblock engineering?

Answering this decision authorizes building the permitted-market
validation mechanism, which is not currently authorized. It does not, by
itself, unblock Bonus issuance or any HDR-J-3-gated mechanism, since those
depend on a resolved jurisdiction existing in the first place.

---

## Dependency graph — determined from the repository, not assumed

The directive's suggested linear chain (J-1 → J-2 → J-3 → J-4 → J-5 → J-6)
**does not match the actual dependency structure recorded in the source
documents.** The real structure, derived from the citations above:

```
                    ┌─────────────────────────────────────────┐
                    │              HDR-J-3                     │
                    │  (collect residence/location/nationality?)│
                    │      THE PRACTICAL ROOT DEPENDENCY        │
                    └───────────────────┬───────────────────────┘
                                         │
        ┌────────────────┬──────────────┼──────────────┬────────────────┐
        │                │              │               │                │
        ▼                ▼              ▼               ▼                │
   HDR-J-1           HDR-J-2        HDR-J-4         HDR-J-6               │
  (fallback to      (precedence    (record-of-      (permitted            │
   tenant/brand)      content)     authority for      markets)            │
        │                │         in-flight                              │
        │                │         obligations)                           │
        └────────┬───────┴──────────────┬─────────────┘                   │
                  │  all four are        │                                │
                  │  DECIDABLE NOW,      │                                │
                  │  but INERT until     │                                │
                  │  J-3 supplies a      │                                │
                  │  signal (or J-1's    │                                │
                  │  fallback supplies   │                                │
                  │  one independently)  │                                │
                  ▼                      ▼                                │
                                                                            │
   HDR-J-5 (BYOL tenant jurisdiction) ─────────────────────────────────────┘
   FULLY ISOLATED — no dependency on or from any other item.
```

**Precise dependency statements, each traced to source:**

- **All six items are independently decidable** — none requires another
  item's answer as a precondition for being formally answered by the
  human. This is stated or directly implied for every item above (see
  each item's §15).
- **HDR-J-3 is not a formal prerequisite of the other five, but it is the
  practical one.** REGISTER and CANON both state that HDR-J-1, HDR-J-2,
  and HDR-J-4 remain **inert** in practice — their content has nothing to
  operate on — until HDR-J-3 supplies at least one producible player-side
  basis. HDR-J-6's enforcement mechanism is similarly inert without a
  resolved jurisdiction to validate, though its *content* (which markets)
  can be authored independently.
- **HDR-J-1 is a partial exception to HDR-J-3's practical gating.** A
  "yes" answer to HDR-J-1, if implemented, would produce a resolved value
  (the tenant's own jurisdiction, substituted) for player-scoped
  operations **without** HDR-J-3 being answered at all — since HDR-J-1's
  fallback is defined to apply precisely when no player-side signal
  exists. This means HDR-J-1 could, in principle, be the item that first
  makes HDR-J-4 and HDR-J-6 practically relevant, ahead of HDR-J-3 — a
  path the source documents acknowledge only implicitly (REGISTER's own
  text notes HDR-J-1 and HDR-J-3 "are both real blockers on the same
  outcome, from different directions"), and this brief states the
  consequence explicitly because the directive requires the actual graph,
  not the assumed one.
- **HDR-J-2's content is inert without at least two producible bases to
  adjudicate between.** Since HDR-J-1 (a fallback basis) and HDR-J-3
  (player-side bases) are the only two mechanisms that could ever produce
  a second candidate basis, HDR-J-2's practical relevance depends on at
  least one of those two being answered "yes" and implemented — not on
  either being answered in any particular order relative to HDR-J-2
  itself.
- **HDR-J-5 has zero dependency in either direction.** No BYOL tenant
  exists, and both possible answers remain open at zero present cost
  regardless of any other item's state (REGISTER, CANON §11.2).
- **HDR-J-4's enforcement half is not part of this decision at all** — it
  was already resolved technically (MROC, CANON §7) and requires no human
  input regardless of how any of the six items are answered. Only its
  narrower reporting/record-of-authority half is a live item, and it
  remains unexercisable until a second candidate jurisdiction can exist,
  which traces back to HDR-J-3 (or HDR-J-1) exactly as described above.

---

## Impact on platform domains

| Domain | Current state | Impact of J-1 | Impact of J-2 | Impact of J-3 | Impact of J-4 | Impact of J-5 | Impact of J-6 |
|---|---|---|---|---|---|---|---|
| **Identity** | No residence/location/nationality field exists (`persons`, `player_accounts` unchanged) | None directly | None directly | **Direct** — proposed new field(s) on `player_accounts`, never `persons` (IC ruling) | None directly | None (no BYOL tenant) | None directly |
| **KYC** | `kyc_documents.issuing_country` exists but is read by no jurisdiction code path; `kyc_verifications` has no residence field | None directly | Names KYC as an operation class the precedence content would govern | **Direct** — proposed `kyc_verifications.verified_residence_country`, set only by reviewer determination | None directly | None | None directly |
| **Jurisdiction Resolver** (`internal/jurisdiction`) | Built; one producible basis (`tenant_licence`, tenant-subject only); every player-scoped resolution returns `unresolved(no_signal)` | Would require a new `platform_fallback` basis producer | Precedence-config rows would activate; table shape already exists | Would gain its first player-side basis producer(s) | MROC's non-foreclosure requirements already satisfied; composition machinery unbuilt | Would require a new `tenant_asserted` basis producer (reserved, unbuilt) | No new resolver work; registry (`licences.permitted_markets`) is a separate, existing table |
| **AssetAuthorization** | Layer 6 denies unconditionally on unresolved/refused; unchanged this stage | Could receive a fallback value for specific operation classes, if authorized | No direct change (AssetAuthorization does not consult precedence content directly) | Would begin receiving real resolved values for player-scoped operations | None | None (no BYOL tenant) | Would need to compose with a separate permitted-market check (not itself an AssetAuthorization change) |
| **Risk** | Conditional fail-closed retained (confirmed correct, unchanged); zero jurisdiction-scoped rules exist today, so no rule currently fires | Fallback value must never be injected as an indistinguishable string (binding constraint) | Precedence content would activate any future jurisdiction-scoped rule for the operation classes it covers | Real player-side values would begin reaching Risk's jurisdiction-scoped rule matching, once any exist | None | Platform-licence ceilings remain `licensing_mode`-scoped regardless (already true) | None directly |
| **Responsible Gaming** | Not touched by Stage 4I (`internal/rg` untouched, confirmed by dedicated boundary review) | None | None | None | None | None | None |
| **Casino** | Per-game blocklist remediated (K-3); armed only for games with a non-empty blocklist; currently no games have one, so no live effect | Could produce a resolvable value the blocklist would check, for armed games | None directly | Would supply a real value for the blocklist check on any armed game | None | None (no BYOL tenant) | Permitted-market validation, once built, is a separate authorization check alongside the blocklist |
| **Bonus** | Five admin surfaces converted to server-resolved jurisdiction (JV-2); all deny at the gate today since resolution is always `unresolved` | Could unblock specific operation classes if a fallback basis is authorized for them | Precedence content would matter once Bonus-relevant signals exist | **Direct — the primary blocked-engineering path**: deposit sweep, cashback scheduler, and manual grant issuance all depend on this | None directly (would matter to Bonus grant record-keeping if MROC composition is ever exercised for a Bonus operation) | None (no BYOL tenant) | None directly |
| **Payments** | Not yet designed for jurisdiction (`payments` "has not had its design phase," per CANON §4.5); `SupportedCountries`' permissive-empty semantic reviewed and determined to be a legitimate, distinct provider-capability concept, not a jurisdiction-gating defect | None directly | None directly | Would eventually supply the resolved value payments' still-undesigned routing dimension 2 (`TODO(jurisdiction)`) could consume | None | None (no BYOL tenant) | Directly relevant — routing dimension 2 and any country→jurisdiction mapping is named as its own future authorization surface requiring `security` review |
| **Sportsbook** | Does not exist as implemented code; boundary confirmed untouched by Stage 4I | None | None | None | None | None | None |
| **Retail** | Not implemented; `retail_node` basis reserved in the closed enum for when retail arrives | None | None | Retail's own future basis producer (`retail_node`) is a separate, already-reserved gap, not part of this decision | None | None | None |
| **Reporting** | `reporting` is not yet a seeded `operation_class` value (only `play`, `catalogue_availability`, `bonus_issuance`, `bonus_conversion` are seeded; others are added by the phase that builds their consumer) | None directly | Names "a regulatory report" as an operation class the precedence content would govern | Would supply the underlying resolved value any future reporting consumer would use | **Direct** — the narrowed decision itself is a reporting/record-keeping question | None | None directly |
| **Audit** | `jurisdiction_resolutions` records every resolution attempt including failures; a strict "never record raw evidence values" rule is enforced regardless of any of these decisions | A fallback-derived record must remain distinguishable from a genuinely resolved one (binding constraint) | Precedence-content changes require versioning and audit entries on change (already-specified requirement) | New evidence references (never values) would begin appearing per the existing "must never be recorded" rules | Per-candidate resolution rows already record both jurisdictions when they differ — the mechanism predates the decision | None | Permitted-market violations would need their own distinguishable audit/reason code (already-specified requirement) |
| **Back Office** | No UI exists for any of this (API surfaces only, per Stage 4I's own explicit constraint); a future "player-facing curated jurisdiction projection" is named as a future consideration, not built | None directly | None directly | Would be a prerequisite for any future back-office jurisdiction display | None directly | None | None directly |

---

## Current safe state — explicitly confirmed, true regardless of when these decisions are made

The following are true today and remain true for as long as HDR-J-1
through HDR-J-6 remain unanswered, per the independent final security
certification (REPORT §16, SEC final phase, "CERTIFIED WITH NAMED
EXCEPTIONS"):

- **Jurisdiction resolution remains unresolved** for every player-scoped
  operation on the platform today (`unresolved(no_signal)`), because the
  resolver's only producible basis (`tenant_licence`) cannot legally apply
  to a player-scoped operation and no player-side basis exists.
- **Every jurisdiction-dependent financial or game operation fails
  closed** where a jurisdiction-dependent policy is in force:
  `AssetAuthorization` denies unconditionally; the casino per-game
  blocklist denies within any game that carries a non-empty blocklist;
  Bonus's five admin surfaces deny at the gate. Where no such policy is in
  force (an empty-blocklist casino game, a tenant with zero
  jurisdiction-scoped Risk rules), behavior is unchanged and non-regressive
  — confirmed by a specific invariance proof (CANON §11.3, RISK §2.1).
- **No client-supplied jurisdiction value is trusted anywhere.** Every
  jurisdiction-consuming HTTP surface either never accepted a jurisdiction
  field, or had it removed this stage (JV-1/JV-2), following the same
  server-side-only discipline this platform already applies to `tenant_id`.
- **No permissive fallback has been introduced.** The resolver's
  `basis` enum reserves values for a future fallback (`platform_fallback`)
  and a future tenant-assertion path (`tenant_asserted`), but both are
  structurally unproducible by the resolver today — verified, not merely
  claimed, by the independent final certification.
- **No Human Decision has been silently selected.** All six items above
  remain genuinely open. No pre-existing Human Decision Register item
  (G-2, the `OpenBetSelfExclusionPolicy` default, the mixed/bonus-funded
  sportsbook cashout policy, FD-1, or Wave 3's Grant-cancellation-after-conversion
  item) was touched, narrowed, or defaulted by Stage 4I's work — confirmed
  independently by a dedicated sportsbook boundary review that found zero
  commits touching `internal/rg` and zero references to any of those
  items beyond explicit disclaimers of overlap.

---

## Completion report

1. **Six decisions, exact wording preserved from the register:**
   - HDR-J-1: "May a missing/unresolved player jurisdiction ever fall
     back to the tenant's or brand's own jurisdiction?"
   - HDR-J-2: "Which jurisdiction-relevant signal is legally authoritative
     when more than one exists?"
   - HDR-J-3: "Should the platform collect a player
     residence/location/nationality attribute at all, and under what
     lawful basis and retention rule?"
   - HDR-J-4 (narrowed): "When an obligation is already in flight and a
     player's jurisdiction changes, which jurisdiction's determination is
     authoritative for regulator-facing record-keeping?"
   - HDR-J-5: "For a BYOL tenant, whose jurisdiction determination
     governs: the platform's, or the tenant's own?"
   - HDR-J-6: "Which specific markets/jurisdictions may the platform's
     first (Anjouan-licensed) B2C brand actually serve?"

2. **Dependencies**: all six are formally independently decidable. HDR-J-3
   is the practical root — HDR-J-1, HDR-J-2, HDR-J-4, and HDR-J-6 are each
   decidable now but remain inert in effect until HDR-J-3 (or, for J-4/J-6,
   possibly HDR-J-1's fallback) supplies a resolvable player-side value.
   HDR-J-5 is fully isolated from the other five in both directions.

3. **Highest-leverage decision**: HDR-J-3, per `architect`'s own
   assessment (reported, not adopted as this brief's independent
   judgment) — every other item's practical effect is gated on it or is
   materially smaller in consequence.

4. **Domains affected**: mapped in full above. Bonus and the jurisdiction
   resolver itself are the most directly affected by HDR-J-3 specifically;
   Payments, Reporting, and Retail are affected mainly by name-only
   forward references (their own consumption of jurisdiction is not yet
   designed or built); Responsible Gaming and Sportsbook are unaffected.

5. **Engineering consequences**: none of the six decisions, in any
   direction, requires new production code, migrations, or schema changes
   to be written before the decision is made — every reserved value,
   table shape, and constraint the eventual implementation would need is
   already specified and, where independent of the decision's content,
   already built. Only HDR-J-3 (and, secondarily, HDR-J-1) directly
   authorizes new migrations once answered.

6. **Current blockers**: real Bonus grant issuance (deposit sweep,
   cashback scheduler, manual grant issuance) remains blocked, jointly, on
   HDR-J-1 and HDR-J-3. The casino per-game blocklist mechanism is armed
   and fail-closed but has no games currently configured with a
   non-empty blocklist, so it has no live blocking effect today. Payments'
   and withdrawal's own jurisdiction dimensions remain separately,
   explicitly deferred regardless of these six decisions.

7. **What can continue safely without resolving them**: all of the
   already-buildable, HDR-independent engineering named in
   `docs/governance/stage-4i-canonical-model.md` §11.1 (already built this
   stage) remains sound and requires no revisiting. Any future work in
   Risk (`R-2b`'s authoring-time precondition), Casino, Payments'
   jurisdiction design phase, or QA's adversarial suite extensions can
   proceed without any of these six decisions being answered first, per
   each domain's own already-recorded scope.

8. **Confirmation that no decision was selected**: this document selects,
   recommends, or defaults no answer to any of the six items. Every
   "consequence" stated above is a reported consequence of an option
   already named in the source documents, not a new option or a ranking
   invented by this document. Where the source material itself reported
   a relative-leverage or relative-urgency assessment (HDR-J-3, HDR-J-5),
   that assessment is attributed to its origin (`architect`'s synthesis)
   and not adopted as this document's own judgment.

9. **Git status**: this document is new (`docs/decisions/0041-jurisdiction-human-decision-brief.md`).
   No other file was modified. No code, migration, API, schema, provider
   integration, configuration, default, or policy was changed or created.
