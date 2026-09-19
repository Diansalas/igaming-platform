# 0041 — Human Decision Register: Stage 4I Jurisdiction Resolution Items (HDR-J-1 through HDR-J-6)

Status: **Record only. No decision is selected in this document.** Owner:
`product-owner-proxy`, Stage 4I ("Platform-wide Jurisdiction Resolution
Foundation"). This document is a **companion to
`docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`**, not a
replacement or a duplicate of it: `0039` organizes the three still-unmade
financial/RG decisions from Stage 4H-B0-R7 (`OpenBetSelfExclusionPolicy`
default, Terminal-Grant settlement-credit resolution, bonus-funded
sportsbook cashout policy/FD-1); this document adds the six jurisdiction-
related items (`HDR-J-1` through `HDR-J-6`) that `architect`'s Stage 4I
canonical synthesis identified as candidates for the register. Both
documents together form the platform's Human Decision Register. Neither
selects an answer to anything in the other.

This document follows `0039`'s exact discipline, restated here rather than
assumed: it does not recommend an answer to any of the six items — per
this workstream's explicit mandate, recommending would cross into
`identity-compliance`'s, `risk`'s, or `security`'s domain expertise, or
into a legal/compliance/licensing judgment this role has no authority to
make. Where `architect`'s Stage 4I synthesis offered a **routing**
recommendation (which items to formally open now versus register for
later), that routing recommendation is reported here, attributed, exactly
as `0039` reported `sportsbook`'s non-binding engineering lean on FD-1 —
recorded as having been offered, not adopted as this document's own
position, and never a recommendation about the *content* of any answer.

Each decision below already exists in the architecture record; this
document's only original content is the organization, the plain-language
question framing, and the "what is actually blocked" analysis in each
decision's final subsection — the same original-content boundary `0039`
draws for itself. Options are named exactly as the source documents name
them; where a source document did not name discrete options (several of
these are configuration-content or lawful-basis questions rather than a
choice between two named values), this document says so rather than
inventing option labels.

**Source documents, cited throughout:**

- **CANON** — `docs/governance/stage-4i-canonical-model.md` (`architect`'s
  Phase 5 synthesis; canonical and binding for Stage 4I implementation
  purposes, per its own status line).
- **RECON** — `docs/governance/stage-4i-reconnaissance.md`
- **IC** — `docs/governance/stage-4i-identity-compliance-model.md`
- **RISK** — `docs/governance/stage-4i-risk-model.md`
- **SEC** — `docs/governance/stage-4i-security-model.md`

**Implementation status, per CLAUDE.md's no-fake-completion rule.**

*As originally written (commit `a7c6279`):* `NOT IMPLEMENTED` — no
resolver, no `jurisdiction_resolutions` table, no registry write surface,
and no player-side jurisdiction signal existed at that commit.

*Corrected by `architect` at Stage 4I's final cross-domain certification,
for factual accuracy only — no recommendation, routing, framing or
decision in this document is altered:* the first three of those four now
**exist**. `internal/jurisdiction` (the resolver), migration `0071`'s
`jurisdiction_resolutions` / `jurisdiction_resolution_active` /
`jurisdiction_precedence_configs` tables, and the `jurisdictions` /
`licences` registry write surface all landed during Stage 4I. The overall
label is therefore **`PARTIALLY IMPLEMENTED`**, not `NOT IMPLEMENTED`.

**The fourth — a player-side jurisdiction signal — does not exist, and
that is the one that matters for every "what is actually blocked"
analysis below.** Each of those analyses was re-verified against the final
Stage 4I code state and **none required correction**: the resolver
genuinely returns `unresolved(no_signal)` for every player-scoped
operation; `AssetAuthorization` layer 6 genuinely denies unconditionally
on it; the precedence table genuinely exists with zero rows; the
casino per-game blocklist is genuinely armed-per-game and fail-closed
within it; and the MROC non-foreclosure requirements are genuinely
satisfied by `0071`'s schema. Where an analysis below says a mechanism is
"already specified and buildable," read it now as **built**; where it says
a fact or a row is missing, it is still missing.

Recording these six items did not, and does not, change that status in
either direction.

---

## HDR-J-1 — May a missing/unresolved player jurisdiction ever fall back to the tenant's or brand's own jurisdiction?

### The question

When the platform cannot determine a player's own jurisdiction for a
given operation, may it ever substitute the tenant's (or brand's) own
licensing jurisdiction as a stand-in, rather than treating the operation
as having no determined jurisdiction at all (which, per the platform-wide
invariant, means the operation fails closed wherever a jurisdiction-
dependent policy is in force)?

### The two directions

- **No fallback, ever.** An unresolved player-scoped jurisdiction stays
  `unresolved(no_signal)` permanently for that operation, regardless of
  how confidently the tenant's own jurisdiction is known. This is the
  **current, structurally-enforced state**: the resolver's `basis` enum
  reserves a value for a fallback (`platform_fallback`) precisely so that
  a future "yes" answer does not require a schema migration, but the
  resolver is **structurally incapable of producing it** today, and stays
  that way unless this decision says otherwise (CANON §3.1).
- **A bounded fallback is permitted.** For some operation classes, under
  some conditions, an unresolved player jurisdiction may resolve to the
  tenant's (or brand's) own licensing jurisdiction instead of remaining
  unresolved. This would require building a new resolver basis producer
  (`platform_fallback`) where today none exists, and deciding, for each
  operation class, whether that basis is acceptable at all (CANON §3.1
  already forecloses one specific misuse of this idea: `tenant_licence` —
  the basis the platform *can* already produce — may never be selected for
  a player-scoped operation, because doing so **is** this decision's
  forbidden fallback wearing a different name; that boundary is enforced
  by a database `CHECK` constraint, not application discipline, CANON
  §3.2).

### Constraints that apply regardless of which direction is chosen

These are not options — they are conditions every prior specialist phase
recorded as binding on *any* answer that permits a fallback at all, cited
precisely so a decision-maker sees the implementation cost of "yes" before
deciding, not after:

- **`risk`'s binding constraint:** a fallback value must never be injected
  into `RiskRequest.JurisdictionCode` as a bare, indistinguishable string.
  `Rule.matches` compares that field by string equality and has no way to
  tell a resolved player jurisdiction from a substituted tenant one — so
  an indistinguishable fallback would silently apply every
  jurisdiction-scoped Risk rule as if the player's own jurisdiction were
  known, when it is not (RISK §7, adopted at CANON §10.2). Any fallback
  must be a **distinct, recorded `basis`** value, and the consuming gate
  must be able to refuse it per operation class rather than treat it as
  equivalent to a resolved player fact.
- **`security`'s binding constraint:** a fallback value, once written into
  a resolution record that later feeds an immutability-trigger-protected
  snapshot (the pattern this platform already uses for casino launch
  sessions, bonus grants, and similar operation-level records), becomes a
  **permanent, unfalsifiable claim** that a jurisdiction fact was
  established when it was not (SEC §S-9, adopted at CANON §10.2). This is
  worse than no record, because it looks authoritative to a regulator
  reading it later. Any fallback answer therefore also requires deciding
  how that record is later distinguished, on inspection, from a genuinely
  resolved player fact.

### What is actually blocked while this is unanswered

**The Bonus deposit sweep and the cashback scheduler cannot issue real
grants.** `AssetAuthorization`'s eligibility check denies unconditionally
when the resolver returns anything other than `resolved` (CANON §4.1), and
every player-scoped resolution in Stage 4I returns `unresolved(no_signal)`
regardless of this decision (CANON §11.3) — because the deeper blocker is
`HDR-J-3` (below), not this one. A "yes" answer to this decision, on its
own, does not unblock Bonus issuance either: it would only do so once (a)
`HDR-J-3` supplies a player-side signal to be absent in the first place,
or (b) the fallback basis is built as a genuinely new producer and wired
into the specific operation classes this decision authorizes it for. This
decision and `HDR-J-3` are therefore both real blockers on the same
outcome, from different directions, and answering one does not answer the
other.

Nothing on the engineering side is blocked by this decision remaining
open: the reserved-but-unproducible `basis` value (CANON §3.1) and the
`CHECK` constraint preventing its misuse (CANON §3.2) are both already
specified and buildable regardless of which way this is eventually
answered.

---

## HDR-J-2 — Which jurisdiction-relevant signal is legally authoritative when more than one exists?

### The question

For a given class of operation (a casino bet, a KYC determination, a
regulatory report, a Bonus grant, and so on), when more than one
jurisdiction-relevant signal exists for the same player — a self-declared
residence, a KYC-reviewer-verified residence, a real-time location
signal — which one governs? And does the answer differ by operation
class (a real-time location signal might reasonably govern *play*, while
a verified residence governs *KYC/AML/reporting*, or it might not — this
document does not assert either way)?

### What is settled already, and what is not

`architect`'s synthesis draws a firm line here that this document repeats
because it changes what is actually being asked: the **mechanism** for
this decision is already resolved and is buildable with no human input.
The precedence configuration is keyed on `(tenant licensing jurisdiction,
operation_class)` — both facts knowable before any player-side signal is
consulted, closing a circularity `risk`'s own review identified in an
earlier proposal (CANON §3.4). The table shape this precedence lives in is
buildable now (CANON §11.1, item B-5).

**What is not settled, and is squarely this decision, is the CONTENT**:
which signal wins, for which operation class. No specialist phase
proposed specific precedence content — this is a legal/compliance
judgment about which evidence a jurisdiction, regulator, or licence
condition treats as authoritative for which kind of decision, and none of
the four Stage 4I specialist phases claimed authority to answer it. A
related, already-established rule narrows the shape of any answer without
supplying its content: a self-declared residence is a real signal, not
worthless, but is never on its own sufficient for an enforcement-grade
decision (a jurisdiction-scoped hard limit, an asset-eligibility gate, a
withdrawal jurisdiction policy) **unless this decision says otherwise for
that operation class specifically** (CANON §3.3).

### Consequences of any answer

Whatever the content, the constraints already recorded are: it must be
versioned (`resolver_policy_version`) and audited on change, so a decision
made under an earlier precedence rule remains reproducible; it needs a
four-eyes posture at least as strong as the platform's existing
`bonus_approval_policies` pattern; and it must be expressed in the
`(tenant licensing jurisdiction, operation_class)` shape above — a
different shape would risk smuggling in a global default while appearing
configuration-driven (CANON §10.2).

### What is actually blocked while this is unanswered

The precedence table exists with **no rows**. A resolver with no
precedence rows resolves `unresolved` for any operation that would
otherwise need to adjudicate between competing signals — which is
correct, and is the deliberate Stage 4I steady state (CANON §3.4, §11.3).
Nothing about the table's *shape*, the Risk-authoring-time precondition
that reads it, or any other buildable Stage 4I item is blocked by this
remaining open (CANON §11.1/§11.2 draw this line explicitly: "the shape
… is not blocked … the rows are").

---

## HDR-J-3 — Should the platform collect a player residence/location/nationality attribute at all, and under what lawful basis and retention rule?

### The question

Does the platform collect any jurisdiction-relevant fact about a player at
all — a self-declared residence, a KYC-reviewer-verified residence, a
real-time location signal, or nationality — and if so, on what lawful
basis, with what retention rule, and with what access controls?

### Why this is presented as the highest-leverage item of the six

This framing is `architect`'s own assessment, reported here rather than
adopted as this document's independent judgment, because it is verifiable
directly against the other five items: **every one of the other five items
either depends on this one or is materially smaller in consequence.**
Until this is answered, the resolver has no player-side input of any
kind, and every player-scoped jurisdiction resolution in the platform
returns `unresolved(no_signal)` — not because the resolver is unbuilt
(most of it is buildable now, CANON §11.1), but because there is nothing
for it to read (CANON §11.2, §11.3). Concretely: `player_accounts.
declared_residence_country`, `kyc_verifications.verified_residence_
country`, and any nationality field do not exist and are not migrated
until this is answered (CANON §11.2) — this is a migration blocked on a
lawful-basis decision, not a design gap.

**Stated plainly, per `architect`'s own framing: this, not a resolver
implementation gap, is the actual root blocker for real Bonus issuance.**
`AssetAuthorization`'s layer-6 eligibility check is correctly,
unconditionally fail-closed against an unresolved jurisdiction (CANON
§4.1) — building more resolver machinery does not change its answer while
there is no player-side fact to feed it.

### The cost of a "yes" answer, stated before the decision, not after

A "yes" is not a small addition. The specialist chain recorded four
concrete costs that a decision-maker should see up front:

1. **A new PII category.** Residence (declared and/or verified) is
   proposed to live on `player_accounts` — never on `persons`, which is
   deliberately platform-scoped and shared across a person's brands
   (`identity-compliance`'s placement ruling, adopted in full at CANON
   §1.2) — but it is a new sensitive attribute regardless of table
   placement.
2. **Its own column-level access-control discipline.** The attribute must
   be **projected out of every existing read that does not need it** —
   explicit `SELECT` lists, never `SELECT *` — so that reading it is a
   deliberate act, not an incidental side effect of an unrelated query
   (CANON §10.2).
3. **Its own permission, separate from ordinary player-read.** Reading
   this attribute requires a permission of its own rather than riding on
   `player:read` (CANON §10.2, echoing `security`'s Stage 4I review).
4. **A privacy documentation update.** `docs/architecture/16-privacy.md`
   needs a corresponding update recording the field, its sensitivity
   classification, and its access controls (CANON §10.2, citing IC §1).

A "yes" to residence does **not** automatically extend to nationality:
nationality is a separately-gated question requiring both this decision
**and** a demonstrated operation that actually needs it (CANON §11.2) — a
narrower, two-part test recorded so nationality is not collected merely
because residence was authorized.

A "yes" also does not authorize a real-time location signal as an input:
that is gated on this decision **plus** a still-open provider-interface
question (RECON Q-7) and carries its own separate operational
consequence — a location vendor becomes a hard dependency of the casino
launch path, a materially different availability and security posture
requiring its own decision and its own `security` review (CANON §9.4,
§12.2) — recorded as a named future consideration, not decided here.

### What is actually blocked while this is unanswered

Everything player-scoped: the Bonus deposit sweep and cashback scheduler
(directly, via `AssetAuthorization`'s fail-closed layer 6), any future
jurisdiction-scoped `HARD_LIMIT` Risk rule as applied to a specific
player, the casino per-game blocklist for any game whose blocklist array
is ever populated (currently none are, so this has no live effect yet,
but the mechanism denies the moment one is populated, CANON §9.2), and any
KYC/AML/reporting-class determination that would consult a residence fact.
None of this is a missing engineering capability — the resolver, the
registry, and the audit/record model are all buildable and, per CANON
§11.1, largely already specified. What is missing is the fact itself.

---

## HDR-J-4 (narrowed) — When an obligation is already in flight and a player's jurisdiction changes, which jurisdiction's determination is authoritative for regulator-facing record-keeping?

### The question, narrowed exactly as `architect`'s synthesis narrows it

If a player's resolved jurisdiction changes while an obligation is already
open — an in-progress bet, an active wagering requirement — which
jurisdiction's determination is authoritative for a regulator-facing
record-keeping purpose: which jurisdiction's SAR (suspicious-activity
report) obligation attaches, or which jurisdiction's data-residency rule
governs where the record is retained?

**This is the only half of the original HDR-J-4 tension that remains
open.** The other half — what the platform actually *enforces* going
forward once two candidate jurisdictions are in play for the same
operation — has already been technically adjudicated and needs no human
input: `architect`'s synthesis resolved a genuine disagreement between
`identity-compliance`'s "apply the stricter rule set" proposal and
`risk`'s "merge outcomes, not rule sets" objection in `risk`'s favor,
adopting **Most-Restrictive-Outcome Composition (MROC)** — each
jurisdiction-dependent gate is evaluated once per candidate jurisdiction
and the operation takes the most restrictive outcome by that gate's own
declared order, with `identity-compliance`'s underlying compliance
posture (tighten, never weaken) fully preserved (CANON §7). MROC answers
what the platform *enforces*; it does not, and must not be read to,
answer what the platform *reports* — that reporting question is what
remains genuinely human (CANON §7.4).

### Not to be confused with Decision 1 in doc `0039`

This is adjacent to, but distinct from, `0039`'s Decision 1 (the
`OpenBetSelfExclusionPolicy` platform-wide default). Decision 1 asks what
happens to an open sportsbook bet when a *player becomes self-excluded*.
This item asks which jurisdiction's *record* governs a regulator-facing
obligation when a player's *resolved jurisdiction itself changes* while an
obligation is open — a different triggering event (a jurisdiction change,
not a self-exclusion event) and a different question (record-of-authority
for reporting, not settlement/void treatment of the bet). A human
answering one does not answer the other, and this document does not merge
them.

### Consequences of any answer

Whichever jurisdiction's record is designated authoritative, the
recording mechanism is already in place regardless of the answer: when two
candidate jurisdictions produce different outcomes, the record shows
**both** — one `jurisdiction_resolutions` row per candidate, with the
gate's own decision record naming which candidate produced the governing
outcome (CANON §7.3, discharging `security`'s "the record must show both"
requirement). This means the answer to this decision, once given, is a
rule for interpreting an already-complete record, not a prerequisite for
the record existing.

### What is actually blocked while this is unanswered

**Nothing today.** MROC's composition machinery is not built in Stage 4I,
and cannot be exercised yet: no two-candidate case can arise, because
there is no resolver output for a player at all (`HDR-J-3` is
unanswered), so there is no second candidate to compose with (CANON §7.4).
The only Stage 4I obligation tied to this item is a non-foreclosure one —
no `UNIQUE` constraint keyed on an operation in the resolution table, and
no speculative composition column — both already satisfied by the current
schema design (CANON §5.2, §7.4). This item is recorded now so it is not
lost, not because anything depends on it today.

---

## HDR-J-5 — For a BYOL tenant, whose jurisdiction determination governs: the platform's, or the tenant's own?

### Status: registered for completeness, not routed as urgent

**No BYOL (bring-your-own-licence) tenant exists on this platform today.**
This item is recorded here so it is not lost by the time one is
contemplated — not because it blocks any current work. Stated plainly so
no one feels pressured to answer it now: this is the lowest-urgency item
of the six, and the specialist chain's own assessment is that it can
safely wait.

### The question, for when it becomes relevant

For a tenant operating under its own licence in its own jurisdiction
(rather than under the platform's Anjouan licence) — the hybrid licensing
model `docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md`
already establishes as the platform's future direction — whose
jurisdiction determination governs a given operation: the platform's own
resolution, or the tenant's own asserted determination?

### Why this can genuinely wait, stated plainly

Both possible answers remain open at zero present cost. A resolver
`basis` value for a tenant's own assertion (`tenant_asserted`) is reserved
in the closed `basis` enum specifically so that answering this decision
later does not require a schema redesign — but the resolver is
structurally incapable of producing it today (CANON §3.1). Independently
of whichever way this is eventually answered, platform-licence ceilings
already remain scoped by `licensing_mode` — a value resolved server-side
from `tenants.licensing_model`, which no tenant can influence — so a
future BYOL tenant's own assertion, if authorized, would never be able to
raise itself above a platform-set ceiling (RISK §7, SEC §S-9, adopted at
CANON §10.2).

### Constraint that would apply to any future "yes"

If this is ever answered such that a BYOL tenant's own determination is
given some weight, that determination must be recorded as its own
distinct `basis` (`tenant_asserted`) and never merged into, or made
indistinguishable from, a platform-resolved value (CANON §10.2) — the same
non-negotiable non-forgeability discipline the platform applies to every
other jurisdiction-bearing value.

### What is actually blocked while this is unanswered

Nothing. There is no BYOL tenant to be affected, and the reserved,
currently-unproducible `basis` value carries zero present cost either way
(CANON §11.2).

---

## HDR-J-6 — Which specific markets/jurisdictions may the platform's first (Anjouan-licensed) B2C brand actually serve?

### The question

Which countries or regulatory jurisdictions is the platform's first,
Anjouan-licensed B2C brand actually permitted to serve — the concrete,
enumerable list a resolved jurisdiction should be checked against before
an operation is allowed to proceed?

### This is not a new question — it is an existing one that has never been formally captured with its technical hook

`CLAUDE.md`'s own "When to stop and ask" section already lists "jurisdiction
selection beyond what's already fixed (Anjouan)" as something requiring
explicit human authorization, not an engineering call. This item does not
introduce a new requirement — it formally opens, in the register, a
decision CLAUDE.md already flags as needing to be asked, now that Stage 4I
work has surfaced its concrete technical hook: `licences.
permitted_markets`, a column that already exists in the schema but is
currently **empty and unread by any code path** (CANON §10.1). A resolver
that successfully returns a jurisdiction is not, on its own, worth
anything without a permitted-market list to validate that jurisdiction
against.

### Consequences of any answer

Whatever the permitted-market list ends up being, validating a resolved
jurisdiction against it is an **authorization** check, not a Risk rule —
and it must be distinguishable in its own reason code from an ordinary
Risk limit breach and from a casino blocklist hit (CANON §10.2). This
matters operationally: a licence-scope violation surfacing as a generic
denial is, in the specialist chain's own words, "an incident nobody can
triage" — so however the list is populated, the mechanism that checks it
against it needs its own distinguishable outcome from day one.

### What is actually blocked while this is unanswered

The permitted-market **validation content** stays unbuilt — this is an
explicit, recorded deferral (closing an open question, RECON Q-16, at
CANON §4.5), not a silent one. This is a narrower, catalogue/availability-
side gap than `HDR-J-3`: it does not by itself block Bonus issuance or any
of the mechanisms `HDR-J-3` gates, but it does mean that even once a
player-side jurisdiction signal exists, the platform has no enumerated
answer to "is this market one we may actually serve" to check that signal
against.

---

## Summary table

| # | Item | Question (one line) | Blocking today? | Routing `architect`'s Stage 4I synthesis recommended (reported, not adopted by this document) |
|---|---|---|---|---|
| J-1 | Fallback to tenant/brand jurisdiction | May an unresolved player jurisdiction ever fall back to the tenant's/brand's own? | No engineering blocker; jointly gates real Bonus issuance together with J-3 | Open now |
| J-2 | Source precedence content | Which signal governs, per operation class, when more than one exists? | Precedence table has zero rows; mechanism/shape is unblocked | Open now |
| J-3 | Collect residence/location/nationality at all? | Should the platform collect this attribute, under what lawful basis/retention? | **Root blocker** — every player-scoped resolution returns unresolved until this is answered | Open now — highest leverage of the six |
| J-4 (narrowed) | Record-of-authority for in-flight obligations | Which jurisdiction's record governs SAR/data-residency when jurisdiction changes mid-obligation? | Nothing today — no two-candidate case can arise until J-3 is answered | Open now, narrowed to the record-of-authority half |
| J-5 | BYOL tenant jurisdiction | Platform's or tenant's own determination governs, for a BYOL tenant? | Nothing — no BYOL tenant exists | Register now; do not route for an answer |
| J-6 | Permitted markets for the first B2C brand | Which markets may the Anjouan-licensed brand actually serve? | Permitted-market validation content stays unbuilt; explicit deferral | Open now |

## Cross-references

- `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md` — the
  companion register entry for the three Stage 4H-B0-R7 decisions
  (`OpenBetSelfExclusionPolicy` default, Terminal-Grant settlement-credit
  resolution, bonus-funded sportsbook cashout policy plus FD-1). Not
  duplicated here; see that document directly. `HDR-J-4`'s narrowing above
  is deliberately distinguished from that document's Decision 1.
- `docs/governance/stage-4i-canonical-model.md` — §0 (the six
  adjudications), §3.1/§3.2 (the closed `basis` enum and the
  `tenant_licence` boundary), §3.4 (precedence-configuration keying), §3.3
  (the declared-vs-verified confidence rule), §4.1 (`AssetAuthorization`
  unconditional fail-closed), §7/§7.3/§7.4 (MROC and its non-foreclosure
  requirement), §10 (the consolidated Human Decision Register
  recommendation and its per-item constraints), §11.1/§11.2/§11.3 (the
  buildable-now vs. blocked-on-HDR split and the honest Stage 4I outcome),
  §12.2 (named future considerations, including the geolocation-vendor
  hard-dependency consequence referenced under `HDR-J-3`).
- `docs/governance/stage-4i-identity-compliance-model.md` (IC),
  `docs/governance/stage-4i-risk-model.md` (RISK), and
  `docs/governance/stage-4i-security-model.md` (SEC) — the three phase
  documents whose findings `architect`'s canonical synthesis consolidates
  and this document, in turn, reports without adopting or amending.
- `docs/decisions/0006-hybrid-licensing-and-jurisdiction-model.md` — the
  hybrid licensing model `HDR-J-5` is a future instance of.
- `CLAUDE.md`, "When to stop and ask" — the existing, pre-Stage-4I
  authority for treating jurisdiction selection beyond Anjouan (`HDR-J-6`)
  as requiring explicit human authorization.

## Ownership and scope note

Owned by `product-owner-proxy`. This document selects no answer to any of
the six items above, and none should be inferred from item ordering —
items are numbered `HDR-J-1` through `HDR-J-6` in the order the Stage 4I
dispatch originally named the six tensions, not in a ranked or recommended
order, except where `architect`'s own synthesis explicitly assessed
relative leverage (`HDR-J-3`, reported as such above) or relative urgency
(`HDR-J-5`, reported as such above) — both are reported as `architect`'s
findings, not adopted as this document's independent judgment. Where
`architect`'s synthesis offered a routing recommendation for which items
to formally open now, that recommendation is recorded in the summary table
above as having been offered, exactly as `0039` records `sportsbook`'s
non-binding lean on FD-1 without adopting it. Once a human answers any of
these six items, the recording of that answer belongs in
`docs/governance/stage-4i-canonical-model.md` or the ADR that formally
supersedes it for the relevant section — not in this register, which
exists to organize the questions, not to hold their answers.
