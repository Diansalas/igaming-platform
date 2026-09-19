# Stage 4I — Identity/Compliance Phase 2 Proposal: the identity-side pieces of the jurisdiction model

**Status of this document: PROPOSAL, not a ruling.** This is `identity-
compliance`'s contribution to Stage 4I's chain
(`identity-compliance` → `risk` → `security` → `backend` → `casino` →
`bonus-engine` → `payments` → `sportsbook` → `qa` → `architect` final →
independent security/compliance final), per the directive. It covers only
the parts of the canonical jurisdiction-resolution model that are
genuinely this specialist's domain: where a player's jurisdiction-relevant
identity fact lives, what role (if any) KYC evidence plays as an input,
which of reconnaissance's six candidate Human Decision Register items are
genuinely human decisions versus safe fail-closed engineering defaults,
the identity-side shape of the source hierarchy, and a registration-time
capture sketch. It does **not** propose the cross-domain resolution
interface itself (`architect`'s job, later in the chain, after `risk` and
`security` have also weighed in), does **not** decide any HDR item, does
**not** implement code or a migration, and does **not** invent a KYC-tier
taxonomy or select a real vendor.

This document cross-references
`docs/governance/stage-4i-reconnaissance.md` (architect's Phase 1
reconnaissance, hereafter "the reconnaissance") throughout rather than
restating its findings. Every "§N" reference below without a document name
refers to the reconnaissance. This document answers reconnaissance's
**Q-8** (item 1 below) and **Q-9** (item 2 below) from an identity-
compliance perspective, and responds to **C-4** (item 6) and to all six
**HDR-J** candidates (item 3), as the directive requires.

Read together with `docs/architecture/05-identity-architecture.md` (the
Person/PlayerAccount model and its full implementation history through
Stage 4H-B1), `docs/architecture/16-privacy.md` (the deliberate PII
boundary), `docs/architecture/11-kyc-aml-rg-architecture.md` (the KYC/AML/
RG subsystem this specialist owns), and `docs/decisions/0027-person-
resolution-and-cross-brand-identity-foundation.md` / `0028-kyc-provider-
abstraction.md`.

---

## 1. Where a player's residence/location/nationality fact lives — `persons` vs. `player_accounts`

**Proposal: if and when a residence fact is captured at all (gated on
HDR-J-3, see §3), it lives on `player_accounts` (tenant-scoped,
RLS-protected), never on `persons` (platform-scoped, RLS-restricted to
the platform scope for read/write).** Location is not persisted as an
identity attribute at all (neither table). Nationality, if ever
collected, is the one candidate that leans `persons`-shaped in principle,
but this document recommends **not** adding it in this phase — see the
nationality discussion below.

### The three concepts are not interchangeable for this question

- **Player location** (concept 1) is not a persistent fact about a person
  or an account at all — it is a signal resolved fresh at operation time
  (geolocation of an IP, or a future provider-supplied value). It has no
  natural home on either `persons` or `player_accounts`; forcing it onto
  either table would misrepresent a point-in-time signal as a durable
  identity attribute. To the extent it needs to be *recorded* (for
  audit — reconnaissance's C-5 finding that the jurisdiction actually
  used is never recorded applies here too), the right place is a
  per-operation snapshot, the same shape `casino_launch_sessions.
  jurisdiction_code` already exists for (currently always NULL, migration
  0042) — not a mutable "current location" column on identity. This is
  the resolver's/consumer's concern (§4/§5 below), not an identity-schema
  question.
- **Player residence** (concept 2) is the genuinely contested one, and is
  the actual subject of this section.
- **Nationality** (concept 3) is discussed separately at the end of this
  section because it has a different natural shape than residence.

### Why residence should attach to `player_accounts`, not `persons`

**Argument 1 — the enforcement points that need it are already tenant/
brand-scoped, and reading `persons` from inside them would widen the
access-control footprint, not narrow it.** Every current and prospective
jurisdiction consumer (K-1 AssetAuthorization, K-2 Risk, K-3 casino
blocklist, K-5 Bonus gate, K-6 withdrawal policy, K-8 payments routing)
runs inside an already tenant-scoped database transaction, with
`app.tenant_id` set. `player_accounts` RLS (migration 0010) is scoped
exactly to that same connection-level setting — reading a residence
field from `player_accounts` inside any of these consumers requires no
privilege escalation beyond what already gates every other read those
consumers already do. `persons`, by contrast, has **RLS that restricts
`SELECT`/`UPDATE`/`DELETE` to the platform scope (`WithoutTenant`)**
(`docs/decisions/0015-persons-platform-scope-access-control.md`,
confirmed at `05-identity-architecture.md`'s "Implementation status
(Stage 2)" section) — an ordinary tenant-scoped transaction cannot read a
`persons` row's content today at all. Putting a jurisdiction-relevant
fact on `persons` would mean every one of these six-plus consumers needs
a platform-scope escalation just to resolve jurisdiction for an ordinary
bet or deposit — a strictly larger, more dangerous access-control surface
than today's design, for a fact that is consumed almost exclusively
inside tenant-scoped operations. This is a concrete, code-grounded
reason, not a preference.

**Argument 2 — KYC/AML obligations, and therefore the residence fact they
key off, are inherently a (tenant × player) relationship, not a platform-
wide fact about a human — and the codebase has already made exactly this
call once, for evidence that is structurally the same shape.** Migration
0040's own header states, explicitly and deliberately: `kyc_verifications`
and `kyc_documents` are **tenant-owned**, "unlike Stage 4D-RG's
`player_restrictions`, this stage does not attempt cross-tenant/cross-
brand REUSE of a verification (e.g. 'verified at Brand A therefore
automatically verified at Brand B')." `person_id` is carried on both
tables "only as a denormalized anchor for a future platform-scoped read,
**never as an enforcement or RLS key today**." Residence is evidence of
exactly the same character as a KYC verification (indeed, per §2 below,
verified residence *is* a fact a KYC verification would establish) — the
platform has already decided, for the closely analogous case, that this
class of fact does not automatically generalize across a person's other
brand accounts. There is no principled reason to treat residence
differently from the KYC evidence it derives from.

**Argument 3 — a real person can accurately hold different declared
residences at different brand accounts, and that is not a data-integrity
problem to be resolved, it is the correct model.** A person who registers
at Brand A while resident in Malta and, eighteen months later, registers
at Brand B while resident in Colombia has given two *different, both
individually accurate* declarations. `persons` is a single row per real
human; if residence lived there, the platform would need a rule for which
of two contradictory-in-time values is "the" residence, and every
tenant's compliance obligation toward its own account would depend on a
fact partly written by a different tenant's registration flow. That is a
worse design than what the tenant-scoped alternative gives for free: each
`player_account`'s own declared/verified residence governs that tenant's
own regulatory relationship with that specific account, which is what
ADR 0006's hybrid-licensing model already establishes as the operative
unit (a *tenant's* licence and jurisdiction configuration, not a global
platform-wide one).

**Argument 4 — contrast with why self-exclusion legitimately IS
`persons`-level, so this proposal doesn't quietly contradict that
precedent.** Doc 05's "Model" section gives the actual reason
self-exclusion/fraud/AML case history attach to `Person`: "otherwise
platform-level self-exclusion and multi-accounting detection... are
impossible" — the entire *point* of that mechanism is that it must be
reachable regardless of which brand a person tries next. Jurisdiction
resolution for an in-flight operation has the opposite shape: the
question a jurisdiction consumer asks is never "does this real human
carry a platform-wide flag," it is "what does *this* tenant's licensed
regime say about *this* account, right now." Residence genuinely needing
cross-brand reach would only arise if a future HDR (see HDR-J-1's
adjacent territory) established that a *platform-wide* self-exclusion-
style residence restriction is required — that is not what "resolve the
applicable jurisdiction for a bet" needs, and conflating the two would
reintroduce exactly the widened-access-control problem in Argument 1.

### Nationality — a different shape, not recommended for this phase

Nationality (unlike residence) genuinely is a fixed attribute of the
human that does not vary by brand — a person's citizenship doesn't
change because they registered at a second brand. If it is ever
collected, it is the one candidate that would make more conceptual sense
attached to `Person` than to `PlayerAccount`. This document does **not**
recommend adding it now, for three reasons: (a) reconnaissance's own
directive framing already notes "real regimes differ... nationality may
govern neither or both" (HDR-J-2) — there is no confirmed operation that
needs it yet; (b) it would be the first PII field ever added to
`persons`, which today (migration 0009) deliberately carries none beyond
a hash hook, and doing so inherits Argument 1's access-control widening
in its sharpest form (nationality would need platform-scope reads from
*every* jurisdiction consumer, permanently, for a fact fewer consumers
plausibly need than residence); (c) it is squarely inside HDR-J-3's gate
(§3) regardless of which table it would land on. Recommendation: leave
nationality uncollected until a concrete operation is shown to require it
and HDR-J-3 is resolved for it specifically.

### This is a revision to doc 16's boundary, stated as one, not a silent crossing of it

Doc 16 states `player_accounts` holds "only what registration and
authentication require... No name, date of birth, address, phone number,
or government ID is collected — that belongs to the Stage 4 KYC
subsystem." A declared-residence field on `player_accounts` (§5) is new
PII on that table, genuinely in tension with that sentence as originally
written. This is not proposed as a quiet exception: doc 16 itself
anticipates this exact moment ("KYC document handling and its privacy
implications are Stage 4... will need this document revisited when that
data starts flowing"). If HDR-J-3 authorizes collecting residence at all,
doc 16 needs a corresponding update recording the new field, its
sensitivity classification, and its access controls — not silence. That
update is not made here; it is named as required follow-up gated on
HDR-J-3, consistent with this specialist's Authority (cannot make a quiet
change to an existing privacy boundary).

---

## 2. KYC as an evidence source, not an authority — the precise role of `kyc_documents.issuing_country`

The directive is explicit: "KYC may provide evidence, but do not invent a
KYC-tier taxonomy." Reconnaissance's **C-8** already states what
`issuing_country` is not (not nationality, not residence, not location).
This section defines what role it *can* legitimately play.

### What `issuing_country` actually is, precisely

Per migration 0040: a nullable `TEXT` column on `kyc_documents`, one row
per uploaded document, versioned (a reupload is a new row, `version+1`,
never an overwrite — immutable once inserted except for the
review-outcome fields), tenant-owned (no cross-brand/cross-tenant reuse,
per §1's Argument 2), applicable across all six `document_type` values
(`passport`, `national_id`, `drivers_license`, `proof_of_address`,
`selfie`, `other`) with no per-type semantics distinguished in the
schema. It records **the country whose authority issued that specific
document** — nothing more, nothing less.

### Why it cannot be read directly as residence, nationality, or location

- A **passport's** issuing country is usually, but not always,
  nationality — dual nationals, and some non-national travel-document
  holders, break the equivalence. It says nothing about current
  residence or location.
- A **national ID's** issuing country is usually residence-adjacent at
  time of issuance, but an ID can remain valid, and be the one on file,
  long after a person has relocated.
- A **driver's license's** issuing country often correlates with
  residence at time of issuance (many jurisdictions require local
  residence to obtain one) but is not itself a residence attestation,
  and licenses are frequently retained after a move.
- A **proof-of-address document's** issuing country is the closest
  correlate to residence of the six types — but the document type alone
  (an unpaid utility bill, a bank statement) does not distinguish "this
  document was reviewed and its address specifically validated as the
  player's current residence" from "this document happened to satisfy an
  identity-evidence checklist item." The schema does not record the
  latter as a distinct fact today.
- Two documents on the same verification can legitimately show different
  issuing countries (an expat's foreign-issued passport alongside a
  locally-issued proof of address is normal, not an anomaly) — the
  schema does not, and should not, attempt to reconcile these into one
  answer. That reconciliation, if ever needed, is exactly the kind of
  judgment call a real jurisdiction resolver would make, not something
  `kyc_documents` should pre-decide by picking "the" country.

### The role it should play — evidentiary input, never a resolver's direct source

**Proposal:** `issuing_country` participates in jurisdiction resolution
only as one low-confidence, document-type-qualified corroborating
signal, never as a directly-read jurisdiction value, and never before a
verification reaches `approved` status. Concretely:

1. **Never auto-populate a residence, nationality, or jurisdiction value
   from `issuing_country`.** No code path should read
   `kyc_documents.issuing_country` and write it into any field this
   document proposes in §5, silently or otherwise. Doing so would be
   exactly the conflation the directive forbids, laundered through an
   extra assignment statement instead of a comment.
2. **It may corroborate a genuinely-captured residence fact, never
   substitute for one.** If a declared-residence field exists (§5) and a
   later KYC verification's `proof_of_address` document shows a matching
   `issuing_country`, that consistency is useful *evidence supporting*
   the declared value's promotion to "verified" — it is not itself the
   verification. If a KYC reviewer's approval and the corroborating
   document disagree with the declared value, that disagreement is a
   fact for the reviewer to resolve and record (see the new
   `kyc_verifications`-side field sketched in §5), not something the
   resolver should silently average or prefer.
3. **It must never be treated as evidence for an operation-class it
   doesn't speak to.** A `passport`'s `issuing_country` is not
   acceptable evidence for a *residence* determination on its own
   (nationality ≠ residence, per above); a `drivers_license`'s
   `issuing_country` is weaker evidence for *current* residence than a
   recent `proof_of_address`'s. A resolver that treats all six document
   types' `issuing_country` values identically would silently reintroduce
   the exact conflation C-8 names.
4. **This is a "probably none" question, and that is a legitimate
   answer.** Reconnaissance's Q-9 already frames "does `issuing_country`
   participate at all" as answerable with "none... a legitimate and
   probably safer answer." This document's position: it is safe for
   `issuing_country` to participate *only* as bullet 2's narrow
   corroboration signal on `proof_of_address` specifically, once a
   genuinely-captured residence fact exists to corroborate — and it is
   equally safe, and simpler, for the resolver to ignore it entirely
   until that captured fact exists. It should never be the *first*
   residence-shaped fact a resolver reaches for, which is precisely the
   trap C-8 names.

### Explicitly not a KYC-tier taxonomy

None of the above proposes a new `KYCTier` value, a new state on
`kyc_verifications.status`, or a residence-specific verification
sub-type. `kyc_verifications.status`'s six states remain exactly a
document-verification state machine (ADR 0028 §2); a verification
reaching `approved` continues to mean "the identity evidence was
validated," not "a residence determination was made." If a residence
fact is captured (§5), it is captured and reviewed as its own field
change on a verification row a reviewer already touches — not a new
tier, not a new status, not a parallel taxonomy.

---

## 3. The six candidate Human Decision Register items — identity-compliance's classification

Per the directive and this specialist's Authority, none of these is
decided here. What follows is this specialist's domain opinion on
whether each is genuinely a human decision or a safe, fail-closed
engineering default — the orchestrator makes the final call on which get
formally opened.

**HDR-J-1 — May a missing player jurisdiction ever fall back to the
tenant's/brand's jurisdiction?**
**Genuinely human.** Agree fully with the reconnaissance's framing: this
determines whether Bonus's deposit/cashback sweeps (P-4, currently
honestly fail-closed forever, per `deposit_sweep.go:435-449`'s own
"NAMED, DISCLOSED LIMITATION" comment) may ever issue under an *assumed*
jurisdiction — that is a compliance event if wrong, not an engineering
convenience. **Not defaulted by identity-compliance.** One clarifying
note, not a partial answer: the *interim* engineering posture (no
fallback, ever, until this is answered) requires no decision to
implement safely, because it is a strict subset of every possible future
answer to HDR-J-1 — a future "yes, under conditions X" answer only ever
*adds* a fallback path, it never has to *retract* one. Building the
resolver today with zero fallback is therefore safe to do unilaterally;
building *any* fallback, however narrow, is not.

**HDR-J-2 — Which player-side signal is legally authoritative for which
operation class?**
**Genuinely human** — agree fully, this is jurisdiction-specific legal
interpretation (physical presence vs. residence vs. nationality
governing play vs. KYC/AML/reporting differently in different real
regimes) that no engineering default can substitute for. One domain-
informed observation, not a decision: this precedence is itself likely
to be **per-jurisdiction configuration**, not one global platform rule,
consistent with doc 15's own foundational principle that jurisdiction is
"a first-class, pluggable concept" — i.e., whatever `architect`/`risk`
eventually design for the source hierarchy should expect the precedence
order itself to vary by jurisdiction, not assume a single platform-wide
ranking exists to be picked once. This is a design-readiness note for
the chain, not a resolution of the question.

**HDR-J-3 — Is a residence/location/nationality field a privacy decision
requiring its own lawful basis and retention rule?**
**Genuinely human — and this is the most identity-compliance-relevant of
the six, so the reasoning is stated in full.** Yes, unambiguously, for
two independent reasons, either of which is sufficient on its own:

1. **It is new PII collection, and CLAUDE.md already forbids engineering
   from inventing the two things that make new PII collection lawful:
   a documented lawful basis and a retention period.** Doc 16's own
   "Retention" section states this precisely for the PII the platform
   *already* collects ("CLAUDE.md explicitly forbids inventing a legal
   retention period... retention periods... are jurisdiction- and
   regulator-dependent"). Residence/nationality would be a *new*
   category of PII, more sensitive than what doc 16 currently inventories
   (email, password hash, IP/user-agent, session metadata) because it is
   directly usable to infer legal status, immigration status, or
   political geography in a way none of today's fields are. If retention
   for the PII the platform already holds is correctly deferred to a
   human/legal decision, a *new, more sensitive* category cannot be
   engineering-defaulted into existence with less scrutiny than the
   existing, lighter-weight fields already received.
2. **It crosses a boundary doc 16 drew deliberately, not accidentally**
   ("privacy-conscious modeling, no unnecessary PII... belongs to the
   Stage 4 KYC/AML subsystem, behind its own vendor-agnostic interface").
   Crossing a deliberate boundary requires the same kind of explicit,
   recorded decision CLAUDE.md's Authority section already requires for
   weakening an RG/KYC rule — the analogous discipline for *expanding*
   what personal data the platform holds, given this specialist's
   Authority is explicitly scoped to compliance judgment calls, not
   unilateral data-collection expansion.

What this specialist *can* and does opine on, per this task's own
instruction, without deciding HDR-J-3 itself: **if and when HDR-J-3 is
answered in favor of collecting a residence fact, §1's schema-placement
answer (`player_accounts`, not `persons`) is this specialist's considered
engineering/architecture recommendation for where it should live** — that
placement question is separable from, and does not have to wait on,
resolving the underlying lawful-basis/retention question, but the
*collection itself* does have to wait.

**HDR-J-4 — What happens to obligations already in flight when a
player's jurisdiction changes?**
**Split answer.** The *enforcement posture* has a safe engineering
default; the *regulatory-record-of-authority* question does not.
- **Safe default (no human decision required to build):** when an
  operation's jurisdiction changes between its stages (C-6's exact
  concern — a Grant's issuance vs. its conversion, a launch vs. a later
  bet in the same round), apply the **stricter** of the two applicable
  rule sets, and record that both were considered (mirroring the
  reconnaissance's §9 illustrative `Considered` field). This is safe to
  build unilaterally because it can never result in *under*-protection —
  it is a tightening, not a weakening, of enforcement, which this
  specialist's Authority already permits without a recorded exception
  (the Authority section only constrains *weakening*). Casino's existing
  per-round freeze (ADR 0031 §9, an immutability trigger on
  `casino_launch_sessions.jurisdiction_code`) already establishes the
  precedent that *a* fixed-point rule is acceptable engineering
  discipline for one temporal grain; "apply the stricter when frozen and
  current disagree" extends that discipline to the disagreement case
  without inventing new policy.
- **Genuinely human:** which jurisdiction's *record* is authoritative for
  a regulator-facing question (e.g., which jurisdiction's SAR filing
  obligation attaches, which jurisdiction's data-residency rule governs
  the retained record) is a legal interpretation question the "apply the
  stricter enforcement rule" default does not answer and should not be
  read as answering by implication.

**HDR-J-5 — For a BYOL (`own_licence`) tenant, whose determination
governs?**
**Genuinely human** — agree fully, commercial + legal, and no BYOL
tenant exists yet so there is no urgency pressure to default it. One
design-readiness note for whichever answer is eventually given: the
resolver's output record (reconnaissance §9's illustrative `Basis` field)
should be able to express "platform-resolved" and "tenant-supplied/
tenant-overridden" as genuinely distinct provenance values from day one,
so that whichever way HDR-J-5 resolves, the resulting design does not
need a second provenance field bolted on later. This is not a decision
about which value should be *trusted more* — only that the shape should
be able to name both without redesign.

**HDR-J-6 — Which markets is the first B2C (Anjouan-licensed) brand
permitted to serve?**
**Genuinely human** — agree fully. `CLAUDE.md` already names "jurisdiction
selection beyond Anjouan" as a stop-and-ask item explicitly, and this is
precisely the data that decision produces (`licences.permitted_markets`,
currently empty and unread per reconnaissance's inventory item 2).
Nothing to add from identity-compliance beyond confirming the
reconnaissance's framing is correct and this is not a KYC/AML/RG
judgment call at all — it is a licence-scope fact, squarely a business/
legal decision.

**Summary table**

| HDR | Classification | Safe interim engineering default? |
|---|---|---|
| J-1 | Genuinely human | Yes — no fallback, ever, until answered |
| J-2 | Genuinely human | No safe default; precedence likely per-jurisdiction config once set |
| J-3 | Genuinely human | No — do not collect residence/nationality until answered; schema placement (§1) is pre-specified for when it is |
| J-4 | Split | Yes for enforcement (apply stricter); no for regulatory record-of-authority |
| J-5 | Genuinely human | No default; provenance shape should accommodate either answer |
| J-6 | Genuinely human | No default; already a named `CLAUDE.md` stop-and-ask item |

---

## 4. Source hierarchy — the identity-side inputs, and when KYC evidence outranks a declared value

The directive requires a deterministic source hierarchy (geolocation,
KYC evidence, operator/brand configuration, licensed-market
configuration, other signals) and forbids assuming any source is
authoritative without documenting why. This section addresses only the
identity-side portion: declared residence vs. verified (KYC-evidenced)
residence vs. absence of both.

**Before KYC completes: only a self-reported, unverified signal can
exist, and it must not be treated as equivalent to a verified one.**
Today the platform has *zero* player-side jurisdiction signals of any
kind (reconnaissance's headline finding). If §5's declared-residence
field is ever built, it is, by construction, unverified — a value the
player typed at registration, never independently checked. This mirrors
a pattern the codebase already applies elsewhere: `kyc_verifications.
status` defaults to `'unverified'`, and nothing in `internal/kyc`, `internal/rg`,
or `internal/identityresolution` treats an unverified state as
equivalent to a verified one for any enforcement purpose. The same
discipline should apply to a declared residence value: it may be
sufficient for *low-stakes, reversible* uses (e.g., a provisional
catalogue filter that a later verification could tighten or relax), but
must **not**, on its own, be treated as dispositive for an enforcement-
grade decision (a jurisdiction-scoped `HARD_LIMIT`, an
`asset_authorizations` jurisdiction gate, a withdrawal jurisdiction
policy) unless and until a human decision (HDR-J-2, and possibly HDR-J-1
for the absent case) says an unverified declaration is sufficient for
that operation class specifically. Absent that, an operation requiring
enforcement-grade jurisdiction certainty with only an unverified
declaration available should resolve as **unresolved**, not as "resolved,
low confidence" silently treated as good enough — this is the fail-closed
posture the directive requires, applied to identity's own input, not
just to the resolver's overall absent-value contract.

**Once KYC evidence exists, it does not blanket-outrank declared
residence — its authority is operation-class-dependent, per HDR-J-2, and
document-type-dependent, per §2.** A KYC-reviewed, explicitly-captured
verified-residence fact (§5's proposed field on `kyc_verifications`,
distinct from `issuing_country`) is higher-confidence evidence than an
unverified declaration for **KYC/AML/reporting-class operations**
(cumulative deposit threshold checks, EDD, SAR-relevant determinations) —
these are precisely the operations whose regulatory logic is about the
account holder's verified relationship with the tenant's regulator, which
is exactly what a KYC review evidences. It is **not** self-evidently more
authoritative for **play-gating at the moment of the operation**, because
KYC verifies who someone is and where they live, not where they are
sitting right now (concept 1, player location) — a verified UK resident
physically traveling elsewhere is a case where a real-time location
signal, if one ever exists, and residence genuinely diverge, and HDR-J-2
is exactly the question of which one governs *play* in that case. This
document does not resolve that; it states precisely why KYC evidence's
authority cannot be assumed uniform across operation classes, which is
the directive's own explicit instruction.

**Absent every signal (no location, no declared residence yet — e.g.
pre-registration-field-rollout — no KYC): the resolver must report
"unresolved," never guess.** This is not a new position; it restates
reconnaissance's own Q-1 (`Resolve` returning `(Resolution{}, ErrUnresolved)`
rather than a zero value) as it applies specifically to the identity
input side: identity-compliance will never supply a resolver with a
manufactured "best guess" jurisdiction assembled from a weak KYC document
field or a stale declaration past its useful confidence window. The
alternative — quietly quality-degrading a resolved value's confidence
without saying so — is exactly the invisible-conflation risk
reconnaissance's §2 closing paragraph names as the single most
consequential structural property of the current gap.

---

## 5. Registration-time capture — illustrative sketch, not decided

**Determination: yes, a genuinely new registration-time field is needed**
if the platform is ever to have any player-side jurisdiction signal
before KYC completes — today there is none (confirmed: no country,
residence, or address column anywhere on `persons` or `player_accounts`,
per reconnaissance §1.1's closing paragraph). This is gated on HDR-J-3
per §3; the sketch below exists to make the shape concrete for the rest
of the chain, exactly matching the reconnaissance's own "illustrative,
not decided" convention (§9).

```sql
-- ILLUSTRATIVE ONLY — NOT DECIDED, NOT PROPOSED FOR IMMEDIATE
-- IMPLEMENTATION. Gated on HDR-J-3 (§3 above). Shown only to make items
-- 1/2/5 of this document concrete for the rest of the Stage 4I chain.

-- registration_channel: NOT gated on HDR-J-3 (not PII, not a jurisdiction
-- fact, an INPUT to a jurisdiction rule per doc 11 §1) — already named
-- as a genuine gap by docs 05/11 and by reconnaissance §1.1's closing
-- paragraph.
ALTER TABLE player_accounts
    ADD COLUMN registration_channel TEXT NOT NULL DEFAULT 'online'
        CHECK (registration_channel IN ('online', 'retail'));

-- declared_residence_country: GATED ON HDR-J-3. Self-reported at
-- registration, UNVERIFIED (see §4). ISO-3166-1 alpha-2, deliberately a
-- COUNTRY, not a jurisdictions.code value — reconnaissance §1.2 point 3
-- already establishes these are different code spaces ('KM-ANJ' is not
-- ISO-3166) with no existing mapping table; a resolver, not this column,
-- would own country -> jurisdictions.code translation.
ALTER TABLE player_accounts
    ADD COLUMN declared_residence_country TEXT;
ALTER TABLE player_accounts
    ADD COLUMN declared_residence_captured_at TIMESTAMPTZ;

-- verified_residence_country: GATED ON HDR-J-3. Set ONLY by an explicit
-- reviewer determination during KYC review, never auto-derived from
-- kyc_documents.issuing_country (see §2's bullet 1 — no code path may
-- silently assign one from the other). Lives on kyc_verifications
-- (tenant-owned, per §1 Argument 2), not on kyc_documents (which stays
-- exactly what it is: per-document evidence metadata) and not on
-- persons (per §1 Argument 1).
ALTER TABLE kyc_verifications
    ADD COLUMN verified_residence_country TEXT;
ALTER TABLE kyc_verifications
    ADD COLUMN verified_residence_source_document_id UUID
        REFERENCES kyc_documents (id); -- which document corroborated it, for audit
```

Notes on this sketch, all deliberate:

- **No new KYC tier, state, or table.** `verified_residence_country`
  is one more field a reviewer sets when moving a `kyc_verifications`
  row toward `approved` — it does not touch the six-state machine
  (ADR 0028 §2) and is not a new taxonomy.
- **`declared_residence_country` lives on `player_accounts`, not
  `persons`**, consistent with §1's proposal — and consistent with §1's
  Argument 3, two different `player_accounts` rows for the same `Person`
  may legitimately carry two different values here; this is not treated
  as an anomaly to reconcile.
- **This is new PII** on a table (`player_accounts`) doc 16 explicitly
  says holds no address data today. It requires the doc 16 update named
  in §1's closing subsection, and requires HDR-J-3's lawful-basis/
  retention answer before any migration resembling this is actually
  written.
- **This does not resolve Q-2** (`uuid` vs. `code` as the canonical
  resolver carrier) — both new columns store an ISO-3166 country, which
  is a third code space distinct from both `jurisdictions.id` and
  `jurisdictions.code` (reconnaissance §1.2). Mapping country → jurisdiction
  is squarely part of the cross-domain resolver `architect` will design
  later in this chain, not something this sketch pre-decides.

---

## 6. Position on C-4 — should jurisdiction ever be client/staff-suppliable?

**Position: no. Jurisdiction should never be a bare, client/staff-
suppliable value on any enforcement-facing request — it must always be
server-resolved, with the same rigor `tenant_id` already gets.**
`security` will independently rule on the security-control mechanics
(this is explicitly routed to them, not decided here); this is this
specialist's identity-authority position, stated for the record because
C-4 touches player-supplied/staff-supplied compliance data squarely
inside this domain's Authority ("cannot weaken an RG or KYC enforcement
rule to ease a product flow").

**Reasoning:**

1. **The contract this platform already states for itself already says
   this.** `AssetAuthorization.CheckEligibility`'s own doc comment
   (`authorization.go:43-45`, quoted in reconnaissance C-4) already
   requires "tenant, brand and jurisdiction MUST be resolved server-side
   from authenticated context by the caller." C-4 is not a case for a new
   rule — it is a case where an *existing, already-correct* rule has no
   enforcement backstop on one class of caller (Bonus's staff admin
   surfaces). The fix is closing the gap between the stated contract and
   the code, not weighing whether the contract is right.
2. **The risk is not limited to a malicious actor picking a favorable
   jurisdiction — an honest mistake has the identical compliance
   consequence.** `casino.LaunchGameParams`'s own doc comment (quoted in
   C-4) frames the risk as a player evading a `HARD_LIMIT`. For Bonus's
   staff surfaces the more likely failure mode is different but equally
   serious: a staff member fat-fingering, or reusing a stale value for, a
   `jurisdiction_code` on a manual grant issuance or a held-disposition
   resolution applies the *wrong regulator's ruleset* to a real
   transaction — a compliance event regardless of intent. CLAUDE.md's
   audit requirement ("actor, tenant, entity, before/after state...
   reason code") exists precisely so this class of event is
   reconstructable; a client-suppliable jurisdiction with no server-side
   validation against what the tenant is actually licensed to serve
   (reconnaissance notes no cross-check against `licences.
   permitted_markets` exists anywhere) undermines that reconstruction at
   the source.
3. **A legitimate need for staff input does not require a bare per-call
   override, and conflating the two is the actual design mistake to
   avoid.** There is a real, legitimate need for a staff member to
   *correct* a jurisdiction-relevant fact during a support interaction
   (e.g., a player's declared residence was wrong). The correct channel
   for that is updating the underlying **identity fact** (§5's
   `declared_residence_country`, with an audit trail per CLAUDE.md's
   mutation rules, ideally with the same reason-code/four-eyes discipline
   CLAUDE.md already requires for manual balance adjustments above a
   threshold) — which then flows through the *same* resolver every other
   caller trusts. A bare `jurisdiction_code` field on a single grant-issue
   or disposition-resolve request body is the opposite of that: an
   unaudited, per-call side channel that bypasses resolution entirely for
   just that one operation, leaving no durable fact for the *next*
   operation on the same account to be consistent with. That inconsistency
   is exactly reconnaissance's **C-6** (Casino vs. Bonus disagreeing on
   temporal grain) waiting to happen a second, staff-introduced way.
4. **This generalizes past Bonus.** Nothing about this reasoning is
   Bonus-specific — any future admin surface (casino catalogue
   overrides, a future withdrawal-policy admin UI, a future retail
   registration override) that accepts a request-body jurisdiction value
   inherits the identical risk. The position stated here is intended as
   a platform-wide one: **no request body, staff or player, ever carries
   an authoritative jurisdiction value** — every jurisdiction used in an
   enforcement decision is either resolved server-side from stored,
   versioned, audited facts, or the operation is refused/held pending
   resolution.

This position does not itself decide the security-control mechanics
(validation rules, four-eyes thresholds, migration path for the existing
five Bonus request types) — that remains `security`'s ruling per
reconnaissance's Q-13, requested next-but-one in this chain.

---

## Summary of positions for the orchestrator and the next phases

- **Item 1 (Q-8):** residence lives on `player_accounts`
  (tenant-scoped), never on `persons` — gated on HDR-J-3. Location is
  never persisted as an identity attribute. Nationality is not
  recommended for collection this phase.
- **Item 2 (Q-9):** `kyc_documents.issuing_country` participates in
  jurisdiction resolution only as a narrow, document-type-qualified
  corroborating signal for a genuinely-captured residence fact — never
  as a direct source, never before `approved`, and it is a legitimate,
  probably-safer answer for it to participate in none at all until a
  captured residence fact exists to corroborate.
- **Item 3 (HDR-J-1..6):** five of six are genuinely human decisions
  with no safe engineering default (J-1, J-2, J-3, J-5, J-6); one (J-4)
  splits into a safe engineering default for enforcement posture
  (apply the stricter rule) and a genuinely human question for
  regulatory record-of-authority.
- **Item 4:** an unverified declared residence must never be treated as
  equivalent to verified evidence for enforcement-grade decisions;
  KYC-derived evidence's authority is operation-class- and document-
  type-dependent, not a blanket override of a declared value; absence of
  every signal must resolve to "unresolved," never a guess.
- **Item 5:** a new `registration_channel` column (not PII, not gated)
  and, gated on HDR-J-3, new `declared_residence_country` (on
  `player_accounts`) and `verified_residence_country` (on
  `kyc_verifications`) columns — illustrative shapes only.
- **Item 6 (C-4):** jurisdiction should never be client/staff-suppliable
  on any enforcement-facing request; corrections belong in the
  underlying identity fact, audited, not as a bare per-call override.
  `security` rules on the control mechanics next-but-one in this chain.
