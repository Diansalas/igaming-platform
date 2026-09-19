# 0042 — Human Decision Response Pack

**Status: HUMAN DECISIONS RECORDED.** This document was originally
published as a blank questionnaire; every `HUMAN ANSWER:` field below has
now been completed with the decision the human owner provided. Every item
still reproduces the exact original question wording already on record in
this platform's architecture/governance documents — nothing here
paraphrases a question into a different question, and nothing here
invents an option that is not already named in the source material.
Where a source document did not name discrete options (this is true for
several items), the document still says so explicitly rather than
inventing labels; the human's answer for those items is recorded as free
text rather than as a selection among invented options.

**How this document is structured.** Each item preserves its original
question, plain-English explanation, documented options, and documented
technical consequences exactly as first published. Immediately below
that, `HUMAN ANSWER:` records the decision as given, `NOTES / CONDITIONS:`
records any qualification or scope limitation attached to that answer,
and `LEGAL / COMPLIANCE REVIEW REQUIRED:` records whether the
architecture record itself flags a legal/compliance dimension for that
item (this flag was pre-marked before the human's answer was received and
is unchanged by it, except where an answer's own conditions state that a
review is still pending). **Recording an answer here is not, by itself,
an implementation instruction** — see the closing note.

**Sources read in full to build this document:**
`docs/decisions/0041-jurisdiction-human-decision-brief.md`,
`docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md`,
`docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`,
`docs/governance/stage-4i-report.md`, `docs/governance/stage-4i-canonical-model.md`,
`docs/governance/task-registry.md` (the Wave 3 Phase 11 escalation),
`docs/architecture/ledger-accounting-model.md` §7.19 (posting-shape
specifications for `bonus_adjustment_write`/`grant_cancel_completed`, and
§7.19.4's escalation of the `converted`-Grant cancellation question),
`docs/architecture/10-bonus-engine-architecture.md` §T.7.

**No implementation of any kind follows from this document alone.** No
code, migration, API, or schema is authorized, created, or modified by
completing this questionnaire. A decision recorded here still requires its
own follow-on implementation dispatch, per this project's standard
process, once given.

---

# Part 1 — Stage 4I jurisdiction decisions (HDR-J-1 through HDR-J-6)

## HDR-J-1 — Fallback to tenant/brand jurisdiction

**Exact registered question** (`0041-human-decision-register-stage-4i-jurisdiction.md`):

> "May a missing/unresolved player jurisdiction ever fall back to the
> tenant's or brand's own jurisdiction? When the platform cannot
> determine a player's own jurisdiction for a given operation, may it
> ever substitute the tenant's (or brand's) own licensing jurisdiction as
> a stand-in, rather than treating the operation as having no determined
> jurisdiction at all (which, per the platform-wide invariant, means the
> operation fails closed wherever a jurisdiction-dependent policy is in
> force)?"

**In plain English:** When we don't know a specific player's own
jurisdiction, are we ever allowed to just use our own tenant's (or
brand's) licensing jurisdiction as a stand-in for that player, or must
every jurisdiction-dependent action simply refuse to proceed for that
player until their own jurisdiction is actually known?

**Documented options (exact wording):**

- **"No fallback, ever."** An unresolved player-scoped jurisdiction stays
  `unresolved(no_signal)` permanently for that operation, regardless of
  how confidently the tenant's own jurisdiction is known. This is the
  current, structurally-enforced state.
- **"A bounded fallback is permitted."** For some operation classes,
  under some conditions, an unresolved player jurisdiction may resolve to
  the tenant's (or brand's) own licensing jurisdiction instead of
  remaining unresolved. This would require building a new resolver basis
  producer where today none exists, and deciding, for each operation
  class, whether that basis is acceptable at all.

**Technical consequences of each option:**

- *No fallback, ever*: no new code is built. The reserved-but-unproducible
  basis value and the database constraint preventing its misuse remain
  exactly as they are today.
- *A bounded fallback is permitted*: requires building a new basis
  producer in the jurisdiction resolver, and — per a binding constraint
  already recorded — the fallback value must never be treated by any
  downstream system as indistinguishable from a genuinely resolved player
  jurisdiction (it must be a distinct, recorded, and separately-refusable
  value). It also requires deciding how a fallback-derived record is
  later distinguished, on inspection, from a genuinely resolved player
  fact, since an indistinguishable record would look authoritative to a
  regulator later even though it was never actually established.

**HUMAN ANSWER:**
```
No fallback, ever.
```

**NOTES / CONDITIONS:**
```
An unresolved player jurisdiction must remain unresolved and
jurisdiction-dependent operations must fail closed. The tenant/brand
licensing jurisdiction must never be substituted for the player's
jurisdiction merely because the player's jurisdiction is unknown.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Yes — this determines whether a
compliance-relevant decision may ever be made on a substituted rather
than a real player fact.

---

## HDR-J-2 — Source precedence when signals disagree

**Exact registered question:**

> "Which jurisdiction-relevant signal is legally authoritative when more
> than one exists? For a given class of operation (a casino bet, a KYC
> determination, a regulatory report, a Bonus grant, and so on), when
> more than one jurisdiction-relevant signal exists for the same player —
> a self-declared residence, a KYC-reviewer-verified residence, a
> real-time location signal — which one governs? And does the answer
> differ by operation class?"

**In plain English:** If we ever hold more than one fact about a
player's jurisdiction at the same time (for example, what they told us
themselves versus what KYC verified versus a real-time location signal)
and those facts disagree, which one do we trust — and should the answer
be different depending on what we're using it for (placing a bet versus
a KYC check versus a regulatory report)?

**Documented options:** The source material does **not** name discrete
options for this item — it is a configuration-content question ("which
signal wins, for which operation class"), not a binary or enumerated
choice. No specialist phase proposed specific precedence content; this
document does not invent one.

What **is** already settled, and is **not** part of this decision: the
*mechanism* by which any answer would be applied (a precedence
configuration keyed on the combination of the tenant's own licensing
jurisdiction and the type of operation) is already designed and does not
need your input. Only the *content* — which signal actually wins, for
which operation type — is open.

**Technical consequences:** Whatever content is chosen, it must be
versioned and logged on every change, and changing it should require the
same dual-staff-approval discipline this platform already uses for its
other approval-gated policy configurations, so a decision made under an
earlier rule remains reconstructable later.

**HUMAN ANSWER:**
```
Jurisdiction signal precedence must be operation-specific.

For identity/KYC and residence-based regulatory determinations, verified
residence is authoritative once established. Declared residence is a
lower-trust signal and must not override verified residence.

For real-time market-access/geolocation controls, the current
physical-location signal may impose an additional restriction and must
not be treated as a substitute for verified residence.

Where multiple applicable signals impose different restrictions, the
more restrictive applicable result should prevail for the operation.

Historical/regulatory reporting must preserve the jurisdiction
determination and evidence applicable at the time of the event.
```

**NOTES / CONDITIONS:**
```
The precedence configuration must be versioned, auditable,
tenant/jurisdiction aware, and subject to legal/compliance validation for
each operating jurisdiction.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Yes — this is a judgment about
which evidence a jurisdiction or regulator would treat as authoritative,
which is a legal/compliance question, not an engineering one.

---

## HDR-J-3 — Player residence / location / nationality collection

**Exact registered question:**

> "Should the platform collect a player residence/location/nationality
> attribute at all, and under what lawful basis and retention rule? Does
> the platform collect any jurisdiction-relevant fact about a player at
> all — a self-declared residence, a KYC-reviewer-verified residence, a
> real-time location signal, or nationality — and if so, on what lawful
> basis, with what retention rule, and with what access controls?"

**In plain English:** Right now, the platform holds **no** fact of any
kind about where a player actually is, where they live, or what
nationality they hold. This is the single decision that everything else
in this document's Part 1 depends on in practice: until something is
answered here, every other jurisdiction mechanism has nothing to work
with. This decision is split below into its component parts, exactly as
requested, because "residence," "location," "nationality," "lawful
basis," "persistence," "audit," and "KYC sourcing" are genuinely
different questions that the architecture record deliberately does not
collapse into one.

**Overall documented options:** Not a single yes/no. It is a compound
question with the eight independent components below. A "no" to all
eight is the current, structurally-enforced default and requires no
engineering change.

### 3a. Current physical location

**What this asks:** Should the platform ever use a real-time signal of
where a player physically is right now (e.g., an IP-derived or
vendor-supplied geolocation signal) as an input to a jurisdiction
decision?

**Documented technical position:** Location is architecturally treated as
a point-in-time signal, never as a stored identity attribute — if ever
authorized, it would appear only as a reference to the evidence that
produced it, never as a column on a player's own record. Authorizing this
also requires resolving a separate, still-open question about which
vendor/provider interface would supply such a signal, and introduces a
new operational dependency: a location vendor would become a hard
dependency of the casino launch path (i.e., if that vendor is down,
launches referencing location could be affected) — a materially different
availability and security posture from what exists today, which this
document flags but does not resolve.

**HUMAN ANSWER:**
```
Yes, the platform may use a real-time physical-location signal for
jurisdiction-sensitive market-access and access-control decisions. It
must not be treated as a permanent residence attribute or as proof of
residence.
```

**NOTES / CONDITIONS:**
```
The signal is point-in-time and must not be persisted as a player
residence. The location provider and availability/failure behavior
require separate security, privacy and compliance review.
```

### 3b. Declared residence

**What this asks:** Should the platform collect a player's own
self-reported (unverified) statement of where they reside?

**Documented technical position:** If authorized, this would live on the
player's brand-specific account record, never on the shared
cross-brand person record. It is not verified by anyone when first
captured, and the architecture record is explicit that a declared,
unverified residence must never on its own be treated as sufficient for
an enforcement-grade decision (a hard limit, an asset-eligibility gate, a
withdrawal policy) unless HDR-J-2 says otherwise for that specific type
of decision.

**HUMAN ANSWER:**
```
Yes. The platform may collect and store the player's self-declared
residence on the brand-specific player account.
```

**NOTES / CONDITIONS:**
```
Declared residence is unverified and must not by itself be treated as
sufficient for enforcement-grade jurisdiction decisions unless an
explicitly approved operation-specific policy permits it.
```

### 3c. Verified residence

**What this asks:** Should the platform collect a residence fact that has
been specifically confirmed by a KYC reviewer (as distinct from what the
player merely declared)?

**Documented technical position:** If authorized, this would live
alongside the platform's existing KYC verification records, set only by
an explicit reviewer determination — never auto-derived from any KYC
document field. This is treated as a stronger signal than a declared
residence, but even a verified residence's authority for a given decision
type is governed separately by HDR-J-2, not automatically supreme in
every case.

**HUMAN ANSWER:**
```
Yes. The platform may record a KYC-reviewer-verified residence.
```

**NOTES / CONDITIONS:**
```
Verified residence must be explicitly established through the approved
KYC process and must not be automatically inferred from a document's
issuing country. Its authority for individual operation types remains
governed by HDR-J-2.
```

### 3d. Nationality

**What this asks:** Should the platform collect a player's nationality?

**Documented technical position:** This is deliberately gated more
tightly than residence: a "yes" to residence does **not** automatically
extend to nationality. Nationality would require **both** a "yes" here
**and** a separately demonstrated operational need for it — it is not to
be collected merely because residence collection was authorized.

**HUMAN ANSWER:**
```
No, nationality should not be collected as a general platform attribute.
```

**NOTES / CONDITIONS:**
```
Nationality may only be introduced later where a specific, documented
regulatory or operational requirement demonstrates a legitimate need for
it, with separate legal/compliance approval defining the permitted use.
```

### 3e. Lawful basis / permitted use

**What this asks:** If any of the above is authorized, under what lawful
basis is it collected, and for what specific purposes may the resulting
fact be used?

**Documented technical position:** No specialist phase claims authority
to answer this — it is explicitly a legal/compliance judgment. The
platform's existing privacy documentation would need a corresponding
update recording whichever field(s) are authorized, their sensitivity
classification, and the lawful basis relied upon.

**HUMAN ANSWER:**
```
Collection and use of jurisdiction-relevant player data must be
supported by a documented lawful basis applicable to the relevant
jurisdiction and purpose. The final lawful basis, privacy notice,
purpose limitation and permitted uses must be validated by qualified
legal/privacy counsel before production use.
```

**NOTES / CONDITIONS:**
```
No jurisdiction-relevant player attribute may be collected or used
merely because it is technically available.
```

### 3f. Persistence

**What this asks:** If collected, where should the resulting fact live,
and for how long should it be retained?

**Documented technical position:** Table placement is already decided as
a matter of data-model design (residence-type facts on the player's own
brand-account record, never on the shared cross-brand person record) —
this sub-question is about retention period and is not otherwise a free
design choice. Whatever is collected must be projected out of any
existing database query that does not specifically need it (never
returned as a side effect of an unrelated lookup), and reading it would
require its own specific permission, separate from ordinary
player-record access.

**HUMAN ANSWER:**
```
Retention must be defined by data type, purpose and applicable
jurisdiction rather than by one universal retention period. The platform
must retain each jurisdiction-relevant fact only for as long as required
for its documented purpose and applicable legal/regulatory obligations,
with deletion/retention controls enforced accordingly.
```

**NOTES / CONDITIONS:**
```
Final retention periods require legal/privacy/compliance validation for
each relevant jurisdiction and data category.
```

### 3g. Audit requirements

**What this asks:** What must be logged whenever this fact is
collected, used, or corrected?

**Documented technical position:** The platform's jurisdiction-decision
audit trail is already built to record that a residence/location fact was
consulted (and which one), and to record the resulting jurisdiction
decision itself — but it is built to **never** record the underlying
raw value (the actual declared country, the actual verified country, the
actual coordinates) inside that audit trail. This sub-question is
whether any additional audit requirement beyond that existing design is
needed (for example, a specific staff-correction workflow with its own
audit record), not whether auditing happens at all.

**HUMAN ANSWER:**
```
The existing audit model is sufficient as the baseline: record when a
jurisdiction-relevant fact was collected, changed, consulted, or used
for a jurisdiction decision, including the source/type and resulting
decision, but never place the underlying raw residence, nationality,
location or coordinate value into the general audit trail.
```

**NOTES / CONDITIONS:**
```
Staff corrections and administrative changes must have actor, subject,
reason and timestamp auditability. Legal/compliance may impose
additional requirements by jurisdiction.
```

### 3h. KYC source requirements

**What this asks:** What role, if any, should existing KYC evidence
(specifically, the issuing country recorded on a KYC document) play in
establishing or corroborating a residence/nationality fact?

**Documented technical position:** The current, binding rule is that a
KYC document's issuing-country field may **never** be read and assigned
directly to a residence, nationality, or jurisdiction field — it may, at
most, serve as a corroborating signal ("does this independently-captured
residence claim agree or disagree with this document's issuing
country"), and even that corroboration has nothing to corroborate until
3b or 3c above is authorized. This sub-question is whether that
corroboration role should ever be activated, and if so, exactly which KYC
document types should be allowed to serve as corroborating evidence.

**HUMAN ANSWER:**
```
KYC document issuing country may be used only as corroborating evidence
for an independently established residence or nationality fact. It must
never automatically populate or override residence, nationality or
jurisdiction.
```

**NOTES / CONDITIONS:**
```
The permitted document types and evidentiary weight must be defined
through the KYC/compliance policy and validated for the applicable
jurisdictions.
```

**NOTES / CONDITIONS (applies to all of HDR-J-3):**
```
See each sub-item's own NOTES / CONDITIONS above for item-specific
qualifications. No jurisdiction-relevant player attribute is authorized
for collection or use beyond what 3a-3h explicitly state above.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Yes, for every sub-item except
3f/3g's purely technical retention-period/audit-format details, which
still require legal input for the *retention period value itself* even
though the mechanism is already designed. This is the item the
architecture record itself identifies as carrying the most privacy and
legal weight of the six HDR-J items.

---

## HDR-J-4 — Record-of-authority for in-flight obligations across a jurisdiction change

**Exact registered question (narrowed form, as currently on record):**

> "When an obligation is already in flight and a player's jurisdiction
> changes, which jurisdiction's determination is authoritative for
> regulator-facing record-keeping? If a player's resolved jurisdiction
> changes while an obligation is already open — an in-progress bet, an
> active wagering requirement — which jurisdiction's determination is
> authoritative for a regulator-facing record-keeping purpose: which
> jurisdiction's SAR (suspicious-activity report) obligation attaches, or
> which jurisdiction's data-residency rule governs where the record is
> retained?"

**Important note on scope, already resolved and NOT part of this
decision:** This item originally had two halves. The first half — what
the platform actually *does* going forward once two different
jurisdictions could apply to the same in-progress operation — has
already been technically resolved by engineering and does **not** require
your input: the platform will apply whichever of the two jurisdictions'
rules is more restrictive, and will keep a record showing both were
considered. What remains open, and is the actual subject of this
decision, is narrower: **which jurisdiction's record is treated as
authoritative for reporting to a regulator** when the two disagree.

**In plain English:** If a player's jurisdiction is later found to have
changed partway through something already underway (an open bet, an
active bonus wagering requirement), and that matters for a report we
might have to file with a regulator or for a data-residency rule about
where records must be kept — which jurisdiction's version of events do we
treat as the official one for that filing/retention purpose?

**Documented options:** No named options exist in the source material.
This is an open content question (which jurisdiction's record governs),
not a choice among labeled alternatives.

**Technical consequences:** None currently pending. The platform already
keeps a record of both jurisdictions whenever they differ for the same
operation, regardless of how this is answered — so an answer here would
be a rule for interpreting an already-complete record, not a
prerequisite for the record existing.

**Note on a related but distinct decision:** This is **not** the same
question as this platform's existing `OpenBetSelfExclusionPolicy`
decision (below), which asks what happens to an open bet when a *player
self-excludes* — a different triggering event and a different question.
Answering one does not answer the other.

**HUMAN ANSWER:**
```
For regulator-facing historical record-keeping, the authoritative
jurisdiction should be the jurisdiction determination applicable at the
time the relevant obligation or reportable event arose. Where the
jurisdiction subsequently changes, the later jurisdiction must also
remain recorded as contextual evidence. The system must not rewrite
historical jurisdiction determinations.
```

**NOTES / CONDITIONS:**
```
Where applicable law requires reporting to another jurisdiction as well,
both records must remain available. Legal/compliance review is required
for jurisdiction-specific SAR and data-residency obligations.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Yes — this determines which
regulator a report may be owed to.

---

## HDR-J-5 — BYOL tenant jurisdiction authority

**Exact registered question:**

> "For a BYOL tenant, whose jurisdiction determination governs: the
> platform's, or the tenant's own? For a tenant operating under its own
> licence in its own jurisdiction (rather than under the platform's
> Anjouan licence) — the hybrid licensing model already established as
> the platform's future direction — whose jurisdiction determination
> governs a given operation: the platform's own resolution, or the
> tenant's own asserted determination?"

**Status, as currently recorded: not urgent.** No bring-your-own-licence
(BYOL) tenant exists on the platform today. This item is recorded so it
is not lost by the time one is being onboarded — not because it blocks
anything today. You are not required to answer this now.

**In plain English:** Once (if ever) a tenant operates under its own
gambling licence rather than under this platform's own licence, whose
statement of jurisdiction do we trust for that tenant's operations — the
platform's own determination, or whatever that tenant itself asserts?

**Documented options (as framed in the question itself, not given
separate labels in the source):**

- The platform's own resolution governs.
- The tenant's own asserted determination governs.

**Technical consequences of each option:** Either way, the platform's own
existing ceiling on how much value can move under its own licence would
continue to apply regardless of this answer — a tenant's own assertion,
if ever given weight, could never be used to exceed a limit the platform
itself has set for its own licence. If the tenant's own assertion is ever
given weight, it must be recorded as a distinct, clearly-labeled kind of
fact — never merged with, or made to look identical to, a platform-resolved
one.

**HUMAN ANSWER:**
```
For player-level jurisdiction determination, the platform's own
jurisdiction resolution is authoritative.

The tenant's licensed jurisdiction is authoritative only for the
tenant's own licensing context, subject to platform validation and the
platform's own licensing/security ceilings. It must not automatically
become the player's jurisdiction.
```

**NOTES / CONDITIONS:**
```
A tenant assertion must remain explicitly identified as tenant-provided
/licence-context data and must never be merged with or represented as
an independently platform-resolved player jurisdiction. Final BYOL
policy requires legal/compliance validation before the first BYOL tenant
is onboarded.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Not urgent today (no BYOL tenant
exists); will require review before this platform's first BYOL tenant is
onboarded.

---

## HDR-J-6 — Permitted markets for the first B2C brand

**Exact registered question:**

> "Which specific markets/jurisdictions may the platform's first
> (Anjouan-licensed) B2C brand actually serve? Which countries or
> regulatory jurisdictions is the platform's first, Anjouan-licensed B2C
> brand actually permitted to serve — the concrete, enumerable list a
> resolved jurisdiction should be checked against before an operation is
> allowed to proceed?"

**Note on why this is being asked now:** This is not a new requirement.
This platform's own governing rules already require this exact question —
"jurisdiction selection beyond what's already fixed (Anjouan)" — to be
put to a human before being decided, and it has simply never yet been
formally asked. A database field already exists to hold the answer; it
is currently empty.

**In plain English:** Which specific countries/markets is this platform's
first brand actually allowed to accept players from or operate in?

**Documented options:** No named options — this is an enumerable list to
be supplied (which countries/markets), not a choice among labeled
alternatives.

**Technical consequences:** Whatever list is supplied, checking a
player's resolved jurisdiction against it would be built as its own
distinct check, clearly separated in any resulting record from an
ordinary betting-limit denial or a game-availability denial — so that if
a player is ever refused specifically because their market isn't on this
list, that reason is never confused in any log or report with an
unrelated denial reason.

**HUMAN ANSWER (list the permitted markets/jurisdictions, or state the
policy for determining them):**
```
Not yet determined. The first B2C permitted-market list must be
established through a jurisdiction-by-jurisdiction legal/compliance
review before launch. No country is permitted by default merely because
it is in Europe or LATAM.
```

**NOTES / CONDITIONS:**
```
Once approved, the permitted-market list must be explicit and
enumerable. Any jurisdiction not on the approved list must fail closed.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Yes — this is the platform's core
licensing-scope question and is already flagged by this platform's own
rules as requiring explicit human authorization. **Status: not yet
determined — this remains a launch-governance dependency until the
jurisdiction-by-jurisdiction review is complete.**

---

# Part 2 — Pre-existing, previously-registered decisions

## G-2 — Terminal-Grant settlement-credit resolution

**Exact registered question** (`0039-human-decision-register-stage-4h-b0-r7.md`,
Decision 2):

> "When a settlement or void credit arrives against a bonus Grant that
> has already gone terminal (expired, cancelled, or forfeited) — because
> a portion of that Grant's value remained locked inside an open
> sportsbook bet whose settlement or void timing didn't line up with the
> Grant's own lifecycle — what should the platform do with that credit?"

**In plain English:** Sometimes a bonus's own lifecycle can end (it
expires, gets cancelled, or is forfeited) while some of its value is
still tied up in a bet that hasn't finished yet. When that bet finally
settles and produces a win, what should happen to that late-arriving
money, given the bonus it was tied to no longer technically exists?

**Documented options (exact wording):**

- **(a) Re-forfeit the credit immediately** on arrival against the
  terminal Grant.
- **(b) Route it to `player_cash`** as a mechanical settlement of an
  already-earned entitlement.
- **(c) Hold it for manual review** in a staff queue pending a per-Grant
  human decision.

**Technical consequences of each option (increasing cost, (a) < (b) <
(c)):**

- *(a) Re-forfeit*: reuses the platform's existing forfeiture posting
  mechanism unchanged; the money is retained by the house as promotional
  liability rather than given to the player. This is the cheapest option
  to build.
- *(b) Route to cash*: requires the platform's settlement-posting logic
  (which today lives outside the Bonus system, in the casino/sportsbook
  settlement path) to check a Grant's status *before* deciding where a
  credit goes — a new cross-system check that does not exist today, in
  addition to the shared underlying work both (a) and (b) require.
- *(c) Hold for manual review*: the most expensive option — the
  underlying manual-review-and-approval workflow needed to actually act
  on a held item does not yet exist and would need to be designed and
  built as part of choosing this option.

**HUMAN ANSWER:**
```
All three documented late-settlement treatments must be supported as
configurable policy options:

(a) Re-forfeit immediately.

(b) Route to player_cash as mechanical settlement of an already-earned
    entitlement.

(c) Hold for manual review.

The selected treatment must be configurable at brand level before the
brand is activated, so different brands may use different approved
policies.

The initial/default policy should be (b) route to player_cash unless
explicitly configured otherwise.
```

**NOTES / CONDITIONS:**
```
The policy must be explicit, auditable and versioned. Changes to the
selected policy must be subject to the applicable staff
authorization/four-eyes controls and must not retroactively change the
treatment of already-created settlement events.

The three options must remain distinct and must not be silently
substituted for one another.

IMPORTANT: this is a decision/configuration requirement only at this
stage. It is not implemented now — per (b)'s own documented technical
consequence above, routing to cash requires new cross-system plumbing
that does not exist today, and (c)'s manual-review workflow does not yet
exist either. Building brand-level configurability across all three
options, including (c)'s workflow, is deferred to a future
implementation dispatch.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Recommended — this has a
consumer-protection dimension (whether the platform retains value a
player could argue they legitimately earned) that could interact with
fair-treatment expectations in a given jurisdiction, though the source
record does not assert a specific legal requirement either way.

---

## OpenBetSelfExclusionPolicy — platform-wide default

**Exact registered question** (`0039`, Decision 1):

> "When a jurisdiction, tenant, or brand has configured no explicit value
> for what happens to a self-excluding player's currently-open
> sportsbook bets, what should the platform apply as the fallback
> default: `SETTLE_NORMALLY` or `VOID_ON_SELF_EXCLUSION`?"

**In plain English:** If a player self-excludes while they still have an
open sportsbook bet, and nobody has specifically configured what should
happen in that exact jurisdiction/tenant/brand combination, what's the
platform-wide fallback: let the bet play out normally, or cancel it and
give the stake back immediately?

**Documented options (exact names):**

- **`SETTLE_NORMALLY`** — the bet proceeds to its natural settlement
  (win/loss/push/partial-settlement) exactly as if self-exclusion had not
  occurred.
- **`VOID_ON_SELF_EXCLUSION`** — the bet is voided immediately when
  self-exclusion becomes effective; the full stake is returned to the
  player.

**Note:** this is a fallback-only decision — any specific jurisdiction
can still later be configured with its own explicit answer that overrides
this platform-wide default for that jurisdiction, and a jurisdiction's own
configured value is always a floor a tenant/brand may tighten but never
loosen.

**Technical consequences of each option:** Both options are already
fully built and ready to use as the default — selecting one over the
other is a configuration change, not new engineering work, for either
option. Separately (and unaffected by which default is chosen), two
security-flagged gaps and one sportsbook-flagged gap in the surrounding
mechanism remain open engineering work regardless of this decision (the
policy's effective-date handling, evidence that the platform checked
*every* open bet and not just some of them, and how this interacts with
a bet that has multiple legs mid-settlement).

**HUMAN ANSWER:**
```
VOID_ON_SELF_EXCLUSION.
```

**NOTES / CONDITIONS:**
```
This is the platform-wide fallback only. Jurisdiction-specific policy
may override it where required, and tenant/brand configuration may
tighten but not loosen the applicable jurisdictional floor.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Yes — this is a
responsible-gaming control with real regulatory weight, and the
architecture record explicitly does not assume any specific
jurisdiction's legal position on this question.

---

## Mixed/bonus-funded sportsbook cashout policy (proceeds split)

**Exact registered question** (`0039`, Decision 3, sub-question 3a):

> "How should a bonus-funded bet's cashout proceeds be handled: split
> proportionally between cash and bonus wallets, paid entirely to cash,
> or should such bets simply not be offered a cashout at all?"

**Scope note, exactly as corrected on record:** This applies to
bonus-funded sportsbook bets generally — both a bet funded entirely by
bonus money and a bet funded by a mix of cash and bonus money — not only
to mixed-funding bets as originally scoped.

**In plain English:** If a player has money on a bet that's partly or
entirely funded by a bonus, and sportsbook betting ever gains an early
"cash out" feature, what happens to the cashed-out money — does the
bonus-origin portion stay restricted, does it all just become regular
cash, or do we simply not let bonus-funded bets use cashout at all?

**Documented options (exact names):**

- **Proportional split** — cashout proceeds are split between cash and
  bonus wallets in the same ratio as the original cash/bonus funding mix.
- **All-to-cash** — cashout proceeds are paid entirely to the cash
  wallet regardless of the original funding mix.
- **Not cashout-eligible** — bonus-funded bets are simply excluded from
  the cashout feature; the split question never arises.

**Technical consequences of each option:**

- *Proportional split*: requires building new posting logic that does
  not exist today, plus an anti-structuring safeguard already flagged as
  necessary (to stop a bet being deliberately structured so its bonus
  share rounds down to nothing, effectively laundering bonus money into
  cash).
- *All-to-cash*: also requires new posting logic; separately flagged as a
  potential wagering-requirement bypass, since it lets bonus-origin value
  become withdrawable cash via a buyout.
- *Not cashout-eligible*: requires only a simple eligibility check
  upstream — no new posting logic of any kind.

**IMPORTANT — this must be answered together with FD-1, below, not
separately.** The source record is explicit that answering this question
alone does not determine FD-1's answer, and an incomplete cashout policy
(this question answered, FD-1 left open) is not a usable policy.

**HUMAN ANSWER:**
```
Not cashout-eligible.
```

**NOTES / CONDITIONS:**
```
Bonus-funded sportsbook bets, including bets funded entirely or
partially with bonus value, are excluded from cashout unless a future
explicitly approved policy changes this. This avoids converting
bonus-origin value into cash through the cashout mechanism.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Recommended — potential
bonus-abuse and consumer-protection dimensions are flagged, and there may
be a jurisdiction-specific rule about whether bonus funds may be cashed
out at all.

---

## FD-1 — Cashout wagering-progress treatment

**Exact registered question** (`0039`, Decision 3, sub-question 3b):

> "Independent of how proceeds are split: does cashing out a bonus-funded
> stake preserve the wagering progress already accrued against that
> stake ('risk-preserving'), nullify it ('nullifying,' netting it away),
> or scale it proportionally to the cashout price?"

**In plain English:** Separately from what happens to the money itself
when a bonus-funded bet is cashed out early — what happens to the bonus
wagering-requirement progress that had already built up on that bet by
the time it's cashed out? Does the player keep all of that progress, lose
all of it, or keep a proportional share based on how much of the original
stake they cashed out for?

**Documented options (exact names):**

- **Risk-preserving** — cashing out leaves accrued wagering progress
  standing, unchanged.
- **Nullifying** — cashing out nets the accrued wagering progress away,
  as if the stake had been voided.
- **Proportional-to-price** — accrued wagering progress is scaled by the
  cashout price relative to the original stake.

**Note on a recorded engineering lean — reported, not a recommendation:**
`sportsbook` recorded a **non-binding engineering lean toward
"Nullifying"** in the architecture record. This document reports that a
lean was offered, exactly as the register itself does, without adopting
it as a recommendation. No option is favored by this document.

**Technical consequences of each option:** All three are technically
buildable with no ledger-balance risk; the source record states the
choice "turns on bonus-abuse policy and consumer-protection disclosure,"
not on any technical constraint. Separately, the record flags that if
cashout is ever risk-preserving, a specific abuse pattern becomes
possible: a player could lock a bonus-funded stake, let wagering progress
accrue while the bet is open, then cash out almost immediately at a
price close to the original stake — keeping essentially all of the
accrued wagering credit for a small, deliberately-timed cost. This is
flagged as potentially a more serious and more deterministic exposure
than the proceeds-split question above, because it depends on the
player's own chosen timing rather than on how a bet happens to settle.
"Nullifying" avoids that specific pattern but takes wagering credit away
from a player who cashes out early for a genuine, unrelated reason.
"Proportional-to-price" is recorded as avoiding both failure modes at the
cost of a new ratio calculation.

**IMPORTANT — must be answered together with the item above, not
separately.**

**HUMAN ANSWER:**
```
Nullifying.

This policy is currently inactive because bonus-funded sportsbook bets
are not cashout-eligible.
```

**NOTES / CONDITIONS:**
```
If bonus-funded sportsbook cashout is enabled in the future, cashout
must nullify the wagering progress attributable to the cashed-out
bonus-funded stake unless a new, explicitly approved decision replaces
this policy before implementation.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Recommended — same
bonus-abuse/consumer-protection dimension as the item above.

---

## Converted Grant cancellation → customer receivable

**Exact question as escalated** (`docs/governance/task-registry.md`, Wave
3 Phase 11; `docs/architecture/ledger-accounting-model.md` §7.19.4):

> "`grant_cancel_completed`'s posting shape is specified for a
> `completed` Grant. For a **`converted`** Grant it is deliberately not
> specified: the value is in `player_cash`, fungible, possibly already
> staked or withdrawn, so clawing it back debits a real player balance
> and can create a **receivable from a customer**. Whether this platform
> ever creates player receivables, under which jurisdictions'
> consumer-protection rules, and what recourse exists when the balance
> is insufficient, carries legal and insurance weight that `ledger-finance`
> does not decide alone — and inventing a posting shape would decide it
> by implication."

**In plain English:** Right now, cancelling a bonus Grant after its value
has already been converted into ordinary cash is deliberately left
unbuilt. If we ever wanted to let staff "undo" a bonus after the player
has already turned it into real cash — cash that might already be spent,
staked, or withdrawn — clawing that value back would mean taking money
from a player's balance that may not be there anymore, potentially
leaving the player owing the platform money (a receivable). Nobody has
decided whether the platform is ever willing to do that, under what
circumstances, or what happens if the player's balance can't cover it.

**Documented options:** No named options exist in the source material —
this question was explicitly escalated as open rather than framed as a
choice between labeled alternatives. The specialist who raised it
declined to invent option names, on the basis that doing so would itself
be making the decision by implication. Illustrative possibilities that
would need to be named and evaluated if you choose to answer this
(not proposed as documented options, since none exist on record):
never permit cancellation of a converted Grant at all; permit it only
under specific, pre-defined conditions with recourse rules spelled out in
advance; or require case-by-case staff/legal review for every instance.

**Technical consequences:** None currently pending — no posting shape,
migration, or code exists for this case, and none is authorized by this
document. Cancellation of a Grant that has already gone through the
normal `completed` (not-yet-converted) lifecycle stage is separately
already specified and unaffected by this question.

**HUMAN ANSWER:**
```
Do not permit automatic cancellation or clawback of a converted Grant in
the initial platform. A converted Grant must not automatically create a
customer receivable.
```

**NOTES / CONDITIONS:**
```
If a converted Grant must ever be reversed because of fraud, material
error, regulatory requirement, or another exceptional circumstance, this
must be handled through a separately approved finance/legal process with
explicit rules for insufficient player balances, customer recourse,
approvals, accounting treatment and applicable jurisdiction.

No automatic negative player balance or receivable should be created
until that policy is separately approved.
```

**LEGAL / COMPLIANCE REVIEW REQUIRED:** Yes — the source record
identifies this as carrying legal and insurance weight (creating a
customer receivable, consumer-protection rules by jurisdiction, and
recourse when a balance is insufficient) that a technical specialist
explicitly declined to decide alone.

---

# Validation — every unresolved Human Decision Register item is represented

| # | Item | Represented above |
|---|---|---|
| 1 | HDR-J-1 | ✅ Part 1 |
| 2 | HDR-J-2 | ✅ Part 1 |
| 3 | HDR-J-3 (split into 8 sub-items: location, declared residence, verified residence, nationality, lawful basis, persistence, audit, KYC sourcing) | ✅ Part 1 |
| 4 | HDR-J-4 | ✅ Part 1 |
| 5 | HDR-J-5 | ✅ Part 1 |
| 6 | HDR-J-6 | ✅ Part 1 |
| 7 | G-2 (Terminal-Grant settlement-credit resolution) | ✅ Part 2 |
| 8 | `OpenBetSelfExclusionPolicy` | ✅ Part 2 |
| 9 | Mixed/bonus-funded sportsbook cashout (proceeds split, 3a) | ✅ Part 2 |
| 10 | FD-1 (cashout wagering-progress treatment, 3b) | ✅ Part 2 |
| 11 | Converted Grant cancellation → customer receivable | ✅ Part 2 |

All eleven previously-unresolved items across both the Stage 4I register
(`0041-human-decision-register-stage-4i-jurisdiction.md`) and the
pre-existing register (`0039-human-decision-register-stage-4h-b0-r7.md`),
plus the Wave 3 Phase 11 escalation, are represented above. No item was
omitted, and no additional, undocumented decision was introduced.

---

## Closing note

Every `HUMAN ANSWER:` field above now records the decision the human
owner provided, verbatim. This document itself did not select, recommend,
or default any answer — every answer above was supplied by the human, not
inferred, completed, or resolved by this document. No code, migration,
API, schema, configuration, or policy has been created or modified as
part of recording these answers; this remains a governance/decision-record
update only. Recording an answer here does not, by itself, authorize
implementation — a follow-on implementation dispatch, scoped precisely to
these recorded answers and to nothing beyond them, would still be
required before any of this is built, and several answers above (HDR-J-2's
precedence content, HDR-J-3's several sub-items, HDR-J-6's permitted-market
list, G-2's brand-level configurability, and the remaining
legal/compliance validations noted throughout) explicitly still require
further legal/compliance review or further specification before that
implementation dispatch could be scoped.
