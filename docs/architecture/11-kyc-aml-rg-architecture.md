# 11 — KYC, AML and Responsible Gaming Architecture Proposal

Status: Stage 0 proposal. Source: Blueprint §4.7. Treated as a single
compliance subsystem behind vendor-agnostic interfaces, because partners
will arrive with an existing SumSub/Veriff/Jumio contract and expect
accommodation rather than a forced switch.

## KYC

Tiered, keyed to lifecycle events: registration, cumulative deposit
thresholds, first withdrawal, enhanced due diligence above configurable
limits. Vendor interface is document verification + liveness, provided by
the vendor; tier logic, thresholds, and enforcement are ours.

## AML

Screening against sanctions and PEP lists at registration **and on a
recurring schedule** (not once). Transaction monitoring with configurable
rules, a case management queue, and suspicious-activity report (SAR)
export.

## Responsible gaming

Deposit, loss, wager, and session limits — a **decrease** takes effect
immediately; an **increase** only after a cooling-off period. Reality
checks, time-outs, self-exclusion at both brand and platform level
(platform level requires the cross-brand `person` cluster from
`05-identity-architecture.md`). A permanent self-exclusion flag survives
account closure and re-registration.

## What makes this an architecture concern, not just a feature list

None of the above are "features" in the ordinary product sense — an audit
is largely an examination of whether they are **enforced in the platform**
and **logged immutably**. The logging is built together with the control,
not added after, per `CLAUDE.md`.

## Software capability vs. legal approval

Implementing this subsystem is a software-engineering deliverable. It does
not itself constitute regulatory approval, certification, or licensing —
those remain separate, human/legal/vendor processes (Blueprint §10 sources
page; `CLAUDE.md` compliance section).

## Ownership and stage mapping

Owned by `identity-compliance`; `security` reviews token/session handling
tied into KYC gating; `ledger-finance` reviews withdrawal-gating
interactions. Stage 4 in the build sequence, with vendor integration work
starting earlier than the engineering timeline suggests it should (KYC
vendor contracts have long lead times that are not engineering time —
Blueprint §9).

## Implementation status (Stage 4D-RG)

This document remains a Stage 0 proposal for KYC and AML in full - neither
is implemented. **Self-exclusion**, specifically the "at both brand and
platform level" requirement this document already anticipated above, is
now `IMPLEMENTED` as a foundation: see `docs/decisions/0026-responsible-
gaming-player-status-enforcement-foundation.md` for the full design (the
`player_restrictions` table, the cross-brand/cross-tenant Person-based
enforcement, the concurrency guarantees, and the RLS model) and
`internal/rg` for the code. Concretely:

- **Self-exclusion enforcement mechanism**: `IMPLEMENTED`. Platform-wide
  by default for player self-service; tenant/brand-scoped for staff-
  initiated restrictions. Indefinite or time-bound; append-only (no early
  termination endpoint - ADR 0026 §2's own recorded open decision on why).
- **Self-exclusion cross-brand/cross-tenant PROTECTION (evading
  self-exclusion by re-registering)**: `PROVIDER DEPENDENT` /
  `NOT IMPLEMENTED` in practice, despite the mechanism above being
  correct - added after this stage's own specialist review (independently
  found by three reviewers). `internal/identity.RegisterPlayer` mints a
  brand-new, unlinked `Person` on every registration; nothing in this
  codebase resolves or deduplicates a Person across registrations. A real
  player who self-excludes and registers a new account today is NOT
  blocked - the platform-wide mechanism has no way to recognize them as
  the same person. This is exactly the "cross-brand `person` cluster"
  precondition this document's own Context/§`Responsible gaming` section
  already named above; it remains unbuilt. See ADR 0026 §9/§16/"Carried-
  forward limitations" for full detail and the tracked open decision.
- **Deposit/loss/wager/session limits, reality checks, time-outs/cooling-
  off**: `NOT IMPLEMENTED` - documented extension points only (ADR 0026
  §15). None of these share self-exclusion's simple binary-restriction
  shape closely enough to retrofit onto `player_restrictions` without a
  concrete design of their own.
- **KYC tiering, AML screening/monitoring/SAR export**: `NOT IMPLEMENTED`.
  `EvaluateEligibility` (the new authoritative "may this player gamble
  right now" boundary casino launch/bet now consult) is deliberately
  shaped so a future KYC/AML check slots in as one more step in its
  existing sequence without changing any of its callers (ADR 0026 §14).
- **Enforcement point**: the platform-side gap this document's "What makes
  this an architecture concern" section warned about (controls that exist
  as a feature but are not actually enforced/logged) is now closed for
  self-exclusion specifically - casino game launch and casino bet are both
  gated, and every denial is audited (ADR 0026 §11).

## Implementation status (Stage 4F) — KYC provider abstraction and document management foundation

Stage 4F (`docs/decisions/0028`/`0029`) builds the platform-owned
verification/document-management SUBSYSTEM this document's "KYC
tiering, AML screening/monitoring/SAR export" bullet above still
correctly marks `NOT IMPLEMENTED` for any real vendor - it does not
contradict that bullet, it builds the boundary a real vendor slots into:

- **Platform-owned verification state model**: `IMPLEMENTED` as a
  foundation. `kyc_verifications`/`kyc_documents` (migration 0040) - a
  6-state verification state machine, versioned/immutable document
  evidence, entirely separate from `PlayerAccountStatus` and from RG's
  `player_restrictions`. See ADR 0028 for the full state model and which
  facts are platform-wide vs. tenant-specific.
- **KYC provider abstraction**: `IMPLEMENTED` as a foundation,
  `MockKYCProvider` only. `internal/kyc.KYCProvider` mirrors
  `CasinoProvider`/`PaymentProvider`'s exact shape - a real vendor
  (Onfido or otherwise) slots in as an adapter without changing
  `Person`/`PlayerAccount`/`kyc.Verification`/`kyc.Document`/`internal/
  rg`/`internal/wallet`/`internal/casino` (ADR 0028 §6). No vendor is
  selected or integrated this stage (directive §1's explicit non-goal).
- **Document management**: `IMPLEMENTED` as a foundation. Upload
  validation (size/content-sniffing/extension-consistency/filename
  sanitization), a mock malware-scanning boundary with a documented
  fail-closed contract, and a `DocumentStorageProvider` abstraction
  (`MockDocumentStorageProvider` only - in-memory, dev/test-only). See
  ADR 0029 for the full security/access-control model and the explicit
  list of future production requirements (encryption at rest, real
  malware scanning, signed access URLs, retention/legal-hold) not built
  this stage.
- **Relationship to self-exclusion cross-brand protection (above)**:
  UNCHANGED - this stage does NOT wire approved KYC evidence into
  `internal/identityresolution.PersonResolver`. That connection remains
  an explicit OPEN DECISION (ADR 0028 §7); a real KYC vendor integration
  is still the prerequisite for closing the cross-brand evasion gap this
  document and ADR 0026/0027 both already documented.
- **Email verification / password reset**: `IMPLEMENTED`. A separate
  authentication concern from KYC identity verification - see ADR 0030.
  Never mixed with KYC status, RG restriction, or account status (this
  document's own "do not mix these into one boolean" precedent, applied
  identically to email verification).

## Implementation status (Stage 4H-B0) — Retail KYC/AML/RG impact (architecture only, `NOT IMPLEMENTED`)

Issued as part of "STAGE 4H-B0 — BONUS + GAMIFICATION + RETAIL
ARCHITECTURE/SCOPE FREEZE" (design-only; no code, no migration). Covers
the confirmed business requirement that this platform support retail
operations (physical agent network, cashiers, in-person registration) as
another surface of the same platform, sharing identity/KYC/RG with
online — the KYC/AML/RG impact specifically. The hierarchy data model
(`docs/architecture/26-retail-operations-architecture.md`) and hierarchy
RBAC (`docs/decisions/0036-retail-hierarchy-rbac-and-audit.md`) are
`architect`'s/`security`'s parallel work, referenced here only through the
same explicitly stated assumptions recorded in
`05-identity-architecture.md`'s own Stage 4H-B0 section — not re-derived
independently, to avoid two divergent guesses about the same
not-yet-written design.

This section extends `docs/decisions/0026`/`0027`/`0028`/`0034` to a new
surface. None of those four ADRs, nor `internal/rg`/`internal/kyc`
themselves, are redesigned or modified by anything below — every
conclusion here is either "the existing mechanism already covers this
with zero new code" or an explicit `OPEN DECISION`/`RECOMMENDATION` for
future work, following ADR 0034's own precedent for how a new domain
integrates with RG/KYC without becoming a second implementation of either.

### 1. KYC/AML at retail must not be assumed weaker than online — jurisdiction-configurable, never hardcoded

A regulator in some jurisdictions treats face-to-face presence at a
licensed cashier as already satisfying part of identity verification (a
liveness/physical-document-inspection step a purely online flow would
otherwise need a vendor for). **This platform must not assume that by
default.** This mirrors a principle this platform already applies
everywhere else in this exact subsystem: `docs/decisions/0031`'s
`JurisdictionCode`/`LicensingMode` contract and this document's own
"Software capability vs. legal approval" section already establish that a
jurisdiction-specific rule is data, resolved server-side per jurisdiction,
never an `if jurisdiction == "X"` branch and never a hardcoded platform-
wide assumption. Applied here:

- **`ARCHITECTURAL DECISION`**: whether in-person presence at a retail
  cashier satisfies any part of a KYC tier's evidence requirement is a
  per-jurisdiction configuration value, read the same way every other
  jurisdiction-sensitive rule on this platform is read (server-side,
  keyed on `JurisdictionCode`, never inferred from the registration
  channel alone) — the registration channel (`registration_channel`,
  `05-identity-architecture.md` §1) is an INPUT to that jurisdiction rule,
  never itself the rule.
- **`ARCHITECTURAL DECISION`**: a cashier's in-person identity check can
  be modeled as a `KYCProvider` implementation without inventing a new
  interface or state machine — `internal/kyc.KYCProvider`'s existing
  shape (ADR 0028 §4) already generalizes to "a verification process,
  vendor or otherwise, that produces a normalized `ProviderResult`." A
  manual, cashier-witnessed verification is exactly such a process: it
  creates a `kyc_verifications` row through the SAME state machine
  (`unverified → pending → review_required → approved/rejected/expired`,
  ADR 0028 §2), distinguished only by its `provider_id`/method (e.g. a
  `"retail_manual"` adapter) — never a parallel table, never a shortcut
  that writes `approved` directly without going through review. This
  keeps the vendor-agnostic interface genuinely vendor-agnostic, including
  for a "vendor" that is actually a person behind a counter.
- **`OPEN DECISION` (compliance/legal sign-off required, not resolved
  here):** which specific jurisdictions permit or require treating retail
  in-person presence as sufficient for which specific KYC tier/evidence
  item, and what specific evidence a cashier must capture (document type,
  photo, signature) to make that in-person check auditable. Building a
  default assumption ("retail always satisfies tier 1") without this
  input would be exactly the kind of enforcement-weakening this
  specialist's Authority forbids doing unilaterally.
- **`OPEN DECISION` (compliance/legal sign-off required, not resolved
  here): cash-specific AML thresholds.** Many jurisdictions impose AML
  reporting obligations keyed specifically to CASH transaction amounts
  (e.g. currency-transaction-report-style thresholds) that do not exist,
  or exist at different thresholds, for card/bank/crypto rails. This
  document's own AML section already states transaction monitoring is
  `NOT IMPLEMENTED` platform-wide (no rule engine exists yet for AML
  specifically, as distinct from the RG-focused `internal/risk` engine
  ADR 0031 built for limits) — retail does not change that status, but it
  does add a genuinely new input dimension (payment channel/method, not
  merely amount) that a future AML rule engine will need to be able to key
  on. Recorded here as a requirement on that future engine's design, not
  invented or resolved now: which jurisdictions impose a cash-specific
  threshold, at what amount, and whether it applies per-transaction or
  cumulatively, is a legal question requiring compliance/legal sign-off
  before any such rule is built.

### 2. Retail registration and the tiered KYC trigger model — extension, not a new tier

This document's own "KYC" section already states the tiered model is
"keyed to lifecycle events: registration, cumulative deposit thresholds,
first withdrawal, enhanced due diligence above configurable limits."
Retail registration is the SAME lifecycle event ("registration") through
a different channel — it does not need a new trigger, a new tier
taxonomy, or a new enforcement mechanism. What retail changes is a
configuration input to that existing model, not the model's shape:

- The **initial KYC tier/verification requirement a retail-originated
  account defaults to** may legitimately differ from an online
  self-registration's default (e.g. a jurisdiction permitting a lower
  initial evidence bar because a cashier already inspected a physical ID)
  — this is the SAME jurisdiction-configuration question as §1 above, read
  using `registration_channel` as one of its inputs, never a new
  `KYCTier` value invented for retail specifically. `PlayerAccount.
  KYCTier` (Stage 2's existing, still-unenforced hook — ADR 0026 §14, ADR
  0034 §6) remains the single tier field; nothing about retail requires a
  second one.
- **This does not require re-opening ADR 0026/0028/0034.** ADR 0034 §6
  already established the pattern this reuses exactly: a bonus template
  reads a minimum KYC tier as configuration, never invents a new field on
  `kyc_verifications` for it. The same pattern applies to a retail-
  registration KYC-tier default: it is Bonus/registration-flow-owned (or
  a shared jurisdiction-config lookup) configuration, read by whichever
  code assigns the initial tier at account creation — not a change to
  `internal/kyc`'s own state machine or `internal/identity`'s own
  registration functions beyond reading one more configuration value.
- **`OPEN DECISION`**: the concrete mapping (which jurisdiction defaults
  retail registration to which tier, and whether any jurisdiction requires
  a HIGHER bar at retail specifically — e.g. because cash handling itself
  is a higher-AML-risk channel, independent of the identity-verification
  question in §1) is compliance/legal policy, not resolved here.

### 3. RG/self-exclusion must not be bypassable through retail — the binding mechanism, stated explicitly

This is the hard requirement (directive's own "no retail-specific
reimplementation, ever"), and it is treated with the same rigor as this
platform's own RG concurrency findings (ADR 0026 §8, the
`clock_timestamp()` fix in `docs/active-stage.md`'s Stage 4G-FINAL Part F)
because a bypass here is a compliance failure, not a bug in the ordinary
sense.

**Load-bearing premise, stated explicitly rather than assumed (Wave-2
review correction, F12, P2)**: everything in this section assumes every
retail player transacts through an identified `PlayerAccount` — the same
premise `docs/decisions/0031-risk-and-limits-engine.md` §22 and
`docs/decisions/0035-retail-agent-network-accounting.md` §11.4 each
independently escalate as a genuinely open human/business/legal decision
("if anonymous retail play is in scope, requirement #16 is not
satisfiable as written," per ADR 0031 §22 — no amount of Risk
configuration can compensate, because `matches()` has nothing to match
on for a transaction with no player). An earlier draft of this section
made this assumption silently. If the human answer is that anonymous or
bearer-instrument retail play is required in some jurisdiction, this
entire section's binding mechanism (and ADR 0036 §8's) does not apply to
that case and needs its own design — not a default, not resolved here.

**`ARCHITECTURAL DECISION`, binding on any future retail implementation
stage, for the identified-player case above:** any retail action that is
gambling-enabling or value-crediting/
debiting — registration completion where the resulting account can
immediately transact, a cash deposit crediting a wallet, a cash withdrawal
payout debiting a wallet, a bet placed at a retail terminal if retail bet
placement is ever in scope — **MUST call the exact same
`internal/rg.EvaluateEligibility` function** every other entry point on
this platform already calls (`internal/casino`'s `LaunchGame`/`postBet`,
and per ADR 0034 §1, the future Bonus/Gamification domain), with the same
signature (`TenantID, BrandID, PlayerAccountID, WalletID`), resolved
server-side, inside the SAME kind of transactional boundary. This is not
a recommendation to "use a similar check" — it is a requirement to call
the identical function, for the identical reason ADR 0034 §1 already
states for its own new caller: "no new indirection layer is required or
introduced... the signature already generalizes to any caller that can
resolve a `(TenantID, BrandID, PlayerAccountID)`." A retail-specific
reimplementation of any part of this logic (a local restriction check, a
cached eligibility flag interpreted locally, a retail-specific status
enum) is exactly the "duplicate RG checks throughout handlers" ADR 0026 §5
already forbids, extended to a new surface — and would reopen every
concurrency/race guarantee ADR 0026 §8 spent its own review cycle closing,
for no reason, on a surface with cash (a harder-to-reverse settlement
instrument than a card/bank reversal).

**Concretely, this means:** a cashier terminal is a CLIENT of the
`platform-api` backend (per `05-identity-architecture.md`'s stated
assumption 2) — never a standalone system with its own database
connection, its own copy of `player_restrictions` data, or its own
re-derivation of "is this player excluded." The retail hierarchy's own
authentication/session model (security's parallel work) determines HOW a
cashier's request reaches the backend; it does not change WHAT gets
called once it arrives — `EvaluateEligibility`, unchanged, exactly as
today.

**Cross-brand self-exclusion reach at retail is the SAME `Person`-keyed
mechanism, with the SAME honest limitation already on record.** ADR 0026's
platform-wide `player_restrictions` table and ADR 0027's Person-resolution
boundary are what make "excluded at Brand A, blocked at Brand B" possible
at all — a retail location is just another place a `PlayerAccount` gets
resolved to a `PersonID` and checked. Retail does not need this restated
as a new mechanism; it inherits it. It also inherits ADR 0026/0027's own
already-disclosed honest limitation verbatim: cross-brand/cross-tenant
protection depends on a real Person-resolution capability (a real KYC
vendor supplying `VerifiedAttributes`) that does not exist yet (ADR 0027
§8, ADR 0028 §7, both still open) — a retail-originated account is
exactly as exposed to the "re-register and evade" gap as an online one,
no better and no worse, until that dependency is closed.

### 4. POS connectivity — the latency/offline assumption this creates, stated rather than silently assumed

Because `EvaluateEligibility` runs inside a live database transaction on
the platform backend (§3), a cashier terminal that cannot reach
`platform-api` cannot obtain an eligibility decision, full stop — there is
no local fallback that would still be "the same function."

**`RECOMMENDATION`, not built, and binding as a floor per this
specialist's Authority ("cannot weaken an RG or KYC enforcement rule to
ease a product flow"):** the default posture for connectivity loss must
be fail-closed — an offline terminal must treat "no decision obtained" as
a denial, never as a default allow, for exactly the same reason a launch
denial must mint zero session (ADR 0026 §6): the absence of an "allow" is
not itself an "allow." A cash transaction is also harder to unwind than a
card/bank reversal once physically handed over, which argues for
fail-closed here at least as strongly as it does on the fully-online path.

**`OPEN DECISION` (business/operations sign-off required, not resolved
here):** whether any bounded, explicitly-approved exception to strict
fail-closed is commercially necessary. **Wave-2 review correction (F13,
P2)**: an earlier draft of this bullet floated one candidate shape for
such an exception — a short-lived, narrowly-scoped, pre-fetched
"provisionally clear" token issued while online and consulted (never
authoritatively decided) offline — without noting that `docs/
architecture/26-retail-operations-architecture.md` §2.5, ADR 0031 §19/§24,
and `docs/decisions/0035-retail-agent-network-accounting.md` §8.4 each
independently pre-reject precisely that mechanism shape ("a terminal-local
cache is just a cache that happens to be in a shop"; "**not** a cached
limit, **not** a locally-replicated rule set"; "not a quiet relaxation of
invariant #15 inside a retail code path") — a token consulted offline to
permit a transaction *is* authoritative offline, which is exactly what
those three documents rule out. The human decision is therefore between
"no offline capability" and "a genuinely new, explicitly-designed
exception with a hard per-terminal exposure cap and a recorded residual-
risk acceptance" — not between two engineering options, one of which is
already vetoed elsewhere. This is a genuine availability-vs-compliance
trade-off this specialist cannot resolve unilaterally: CLAUDE.md is
explicit that
"enforcement is not optional and any exception requires an explicit,
recorded decision, not a quiet code change." If the business decides some
bounded offline tolerance is commercially necessary, that decision must be
made explicitly, by the human/business owner, with the exact bound and
reconciliation mechanism recorded — never defaulted into existence by a
POS vendor's or hierarchy design's own convenience. Flagged as a **P0/P1
RG-bypass risk if resolved by silent assumption** rather than explicit
decision, per this stage's own instruction to treat this with the same
rigor as ADR 0026 §8's concurrency findings.

**A player self-excluding while physically at a retail counter — scope
clarification, `ARCHITECTURAL DECISION`.** ADR 0026 §3/§4 already draws a
sharp line: player self-service self-exclusion is always platform-wide;
staff-initiated restrictions are tenant/brand-scoped only, and there is no
platform-wide staff-initiated path (ADR 0026 §4's own recorded reasoning —
no platform-wide player-lookup capability exists for one to be built on).
Retail must preserve this line, not blur it: if a player at a retail
counter self-excludes using THEIR OWN authenticated identity (a PIN, ID
check, or credential that authenticates the request as the player's own,
regardless of the cashier merely operating the terminal on their behalf),
that is the EXISTING player-self-service path and must always land
platform-wide, exactly as it does online — a cashier keying in a request
the player authenticated as their own is a UI channel, not a different
actor. If instead a CASHIER unilaterally flags a player as needing
exclusion (the player did not request it — e.g. observed problem-gambling
behavior), that is squarely ADR 0026's existing staff-initiated path:
tenant/brand-scoped only, requiring `PermRGRestrictionWrite`
(`RoleCompliance` only, per ADR 0026 §12), same reason code requirement,
same restriction. **No third scope category is invented for retail** —
every retail self-exclusion request must be classified into one of these
two existing paths, never a new hybrid.

### 5. Cross-brand/cross-network self-exclusion for retail specifically — `OPEN DECISION`

**Wave-2 review correction (F11, P2)**: an earlier draft of this section
framed the question as whether a hierarchy spans multiple *tenants*.
`docs/architecture/26-retail-operations-architecture.md` H5/§5.1 has
since resolved that a network never spans tenants (a cross-tenant parent
edge is a constraint violation) — but a tenant may run **several
networks** (e.g. one per licence/jurisdiction/brand, doc 26 §5.1 entity
4). The genuine regulatory question survives with the corrected topology:
if a retail agent network spans more than one **network/brand within a
single tenant/licence** (see `05-identity-architecture.md` §3), a genuine
regulatory question arises that this specialist flags rather than
resolves, because it is a compliance/regulatory policy question, not an
engineering one:

- **Player self-service self-exclusion** already defaults platform-wide
  (ADR 0026 §3) regardless of tenant/brand/channel — this needs no change
  for retail; a player self-excluding at a retail counter is already
  protected across every tenant/brand this platform operates, exactly as
  online.
- **Staff/cashier-initiated restrictions**, however, are tenant/
  brand-scoped only today (ADR 0026 §4), by design, because no
  platform-wide staff player-lookup capability exists for a broader one to
  be built on safely. If a jurisdiction requires (or a licence/operator's
  own policy demands) that a CASHIER-OBSERVED restriction reach every
  retail location across a multi-tenant hierarchy under one licence — not
  just the tenant the cashier happens to work for — that requires exactly
  the capability ADR 0026 §4 already flagged as a hypothetical future
  item, now potentially forced into existence by a concrete retail
  requirement rather than remaining hypothetical. **`OPEN DECISION`,
  requiring both a regulatory answer (is licence-wide/operator-wide retail
  staff-initiated exclusion required or merely permitted in the
  jurisdictions this platform will operate retail in) and, once answered,
  a genuine architecture item (a platform-wide, or licence-scoped,
  staff-initiated restriction capability with its own player-lookup and
  RBAC design) that neither this document nor ADR 0026 builds today.**
  Not resolved here; not to be silently defaulted to either "tenant-scoped
  is obviously fine" or "must be licence-wide" without that input.

## Consequences of Stage 4H-B0 for already-approved architecture (Stages 0-4G)

Explicitly checked, per this stage's own instruction, rather than
silently redesigned:

- `internal/rg.EvaluateEligibility`'s signature, `internal/kyc`'s
  provider abstraction and state machine, and `internal/identityresolution`'s
  registration-flow dispatch are **unchanged** by this stage — every
  conclusion above is either "the existing mechanism already covers this
  with zero new code" (§2, §3, most of §5's first bullet) or a
  configuration/schema EXTENSION following an already-established additive
  pattern (`registration_channel`, a retail `KYCProvider` adapter,
  jurisdiction-keyed tier defaults) rather than a redesign of any approved
  Stage 0-4G decision.
- Nothing above weakens an existing RG or KYC enforcement rule to ease the
  retail product flow — every genuine risk of that happening (fail-open
  connectivity fallback, a hardcoded "retail already verified" assumption,
  a retail-specific eligibility reimplementation) is flagged as an
  `OPEN DECISION`/`RECOMMENDATION` requiring explicit human/compliance
  sign-off, never resolved unilaterally here, per this specialist's own
  Authority.
- The one item that could eventually force a genuine architectural
  addition to already-approved work is §5: a licence-wide/operator-wide
  staff-initiated restriction capability, if regulatorily required, is new
  work on top of ADR 0026 (a new capability, not a change to what ADR 0026
  already ships) — flagged, not built, not silently assumed unnecessary.
