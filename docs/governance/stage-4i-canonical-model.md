# Stage 4I — Canonical Jurisdiction Resolution Model

**Status of this document: CANONICAL AND BINDING for Stage 4I.** This is
`architect`'s Phase 5 synthesis and it supersedes, for implementation
purposes, every proposal in the four prior Stage 4I phase documents. Where
this document and a prior phase document differ, **this document governs**;
where they agree, this document states the answer once and cross-references
the prior document for the reasoning rather than restating it.

Implementation status label, per CLAUDE.md's no-fake-completion rule:
**`PARTIALLY IMPLEMENTED`** — updated by `architect` at Stage 4I's closing
certification phase. **§13 is the current status record**; read it before
relying on any implementation claim in this document. As originally
written (Phase 5) this document was a specification with nothing
implementing it; the resolver, the `jurisdiction_resolutions` record and
the registry write surface now exist, but **no player-side jurisdiction
signal does** (HDR-J-3), so every player-scoped resolution returns
`unresolved(no_signal)` and §11.3's honest-outcome statement stands
unchanged. This document remains a specification; §13 records how much of
it is real.

**Chain position.** `architect` reconnaissance (Phase 1, `2bab29f`) →
`identity-compliance` (Phase 2, `c91fe40`) → `risk` (Phase 3, `61c020e`) →
`security` (Phase 4, `89af4c0`) → **`architect` synthesis (Phase 5, this
document)** → `backend` (schema + resolver) → `casino` → `bonus-engine` →
`payments` → `sportsbook` → `qa` → independent security/compliance final.

**Source documents, cited throughout by short name:**

| Short name | File |
|---|---|
| **RECON** | `docs/governance/stage-4i-reconnaissance.md` |
| **IC** | `docs/governance/stage-4i-identity-compliance-model.md` |
| **RISK** | `docs/governance/stage-4i-risk-model.md` |
| **SEC** | `docs/governance/stage-4i-security-model.md` |

**Verification method.** Every code and schema claim this document *relies
on* was re-checked directly against the repository at HEAD `89af4c0`,
not inherited from a prior phase's summary. The re-checks that changed or
sharpened a prior phase's conclusion are called out in place (§2.4, §6.2,
§9.1).

**HDR discipline.** No Human Decision Register item is decided here. §10
consolidates the six candidates and recommends which the orchestrator
should formally open — a recommendation about *routing*, never about
content.

---

## 0. The six adjudications, stated first

The orchestrator dispatched this phase with six named tensions. Answers,
in one line each; full reasoning follows in the cited section.

| # | Tension | **Adjudication** | §|
|---|---|---|---|
| 1 | HDR-J-4 mechanism: IC's "stricter rule set" vs. RISK's "merge outcomes" | **RISK's mechanism governs.** IC's *posture* (tighten, never weaken) is retained and is correct; IC's *mechanism* is withdrawn because rule sets are not totally ordered and merging them manufactures `ErrConflictingRules`. The canonical mechanism is **Most-Restrictive-Outcome Composition (MROC)**. SEC's "the record must show both" is folded in as a recording requirement on MROC | §7 |
| 2 | Doc 34 §5.3 placement of the read-only-resolver constraint | **SEC's placement governs: a PRECONDITION, not "rule 0."** The substance (RISK H-2) is mandatory and unchanged. Recorded in `docs/architecture/34-economic-operation-identity.md` §5.3 by this phase | §6.4 |
| 3 | Resolved-value shape (Q-1/Q-2) | **Finalized.** One opaque `jurisdictions.code` string for matching; a non-forgeable `Resolution` value carrying both `code` and `id` for the operation; a persisted `jurisdiction_resolutions` record for reporting/audit. Three-valued `outcome` + a diagnostic `reason` enum reconciles RISK's and SEC's vocabularies | §3, §4 |
| 4 | Casino blocklist remediation (K-3) | **Synthesized into one spec — with a correction neither RISK nor SEC made.** RISK §5.2's "remove the nil guard" is *too broad*: applied literally it denies 100% of casino launches in Stage 4I. The correct contract arms the control **per game with a non-empty blocklist**, which is RISK's own §1.4 invariant applied to K-3 | §9 |
| 5 | C-4 removal mechanism (JV-1/JV-2/JV-3) | **Settled. Confirmed binding, carried forward unchanged**, including SEC's per-handler sequencing rule. Precision (4 structs / 5 surfaces) independently re-verified | §8 |
| 6 | Caching (CA-1) | **Settled. Confirmed binding: no jurisdiction cache in Stage 4I.** SEC §S-7.2's conditions become the binding terms of any future reversal, and reversal requires an ADR plus `security` review | §6.5 |

Three additional items the dispatch asked to be closed:

- **SEC-4I-F2** (manual-grant jurisdiction metadata gap) — SEC's interim
  control is **confirmed correct**, with one tightening: `staff_supplied`
  must be a value in the *same* `basis` enum the resolver uses, and must be
  **structurally unproducible by the resolver**. §8.4.
- **SEC-4I-F1** (four-eyes threshold bypass) — **tracked separately**, not
  Stage 4I scope, not re-analyzed here. Confirmed still unfixed at
  `89af4c0`. §11.2.
- **SEC-4I-F3** (`casino_game.upserted` audit gap) — **confirmed as a hard
  prerequisite** of casino's K-3 remediation, and the K-3 contract in §9
  makes it *more* clearly so, not less. §9.5.

---

## 1. The canonical data model — seven concepts, and exactly where each lives

RECON §2 established that the seven concept distinctions the directive
requires are mostly absent from the schema. This section states, finally,
where each one lives or does not, and what gates its existence.

| # | Concept | Canonical home | Status at `89af4c0` | Gated on |
|---|---|---|---|---|
| 1 | **Player location** — where the player physically is at operation time | **Nowhere persistent, by ruling.** It is a point-in-time signal, never an identity attribute (IC §1). It may appear only as a `basis = geo_signal` with an `evidence_ref` on a `jurisdiction_resolutions` row — never as a column on `persons`, `player_accounts`, or any other entity | **DOES NOT EXIST** | HDR-J-3 (collection), RECON Q-7 (provider interface) |
| 2 | **Player residence** — declared or KYC-verified home jurisdiction | Two distinct facts, both **tenant-scoped**, never on `persons` (IC §1, Arguments 1–4, adopted in full): `player_accounts.declared_residence_country` (self-reported, **unverified**) and `kyc_verifications.verified_residence_country` (set only by an explicit reviewer determination) | **DOES NOT EXIST** | **HDR-J-3** — hard block |
| 3 | **Nationality** | **Not collected.** If ever collected it is `persons`-shaped in principle, and inherits IC §1's access-control widening in its sharpest form | **DOES NOT EXIST**, and is **not to be added in Stage 4I** | HDR-J-3 **and** a demonstrated operation that needs it |
| 4 | **Tenant licensing jurisdiction** | `licences.jurisdiction_id`, reached via `tenants.licence_id`. This is the **only jurisdiction fact the platform can establish today with no human decision and no new PII** | **EXISTS as unread configuration.** Zero production readers (RECON §1.1 items 2–3) | **Nothing.** Buildable now (§11.1) |
| 5 | **Brand operating jurisdiction** | **RULING BI-4I-1: a brand has no independent operating jurisdiction.** Brand is a *narrowing* dimension over the tenant's jurisdiction set, never an independent source of one. See §1.1 | Resolves RECON **C-2** with no schema change | Nothing |
| 6 | **Transaction / operation jurisdiction** | The `jurisdiction_resolutions` row (§5) is the canonical artefact. The existing snapshot columns (`casino_launch_sessions.jurisdiction_code`, `bonus_grants.jurisdiction_code`) are retained and gain a `jurisdiction_resolution_id` FK (SEC AR-2) | **Snapshot slots exist, permanently NULL.** Resolution record **DOES NOT EXIST** | Nothing for the mechanism; §11.2 for what can actually populate it |
| 7 | **Product jurisdiction** | `asset_authorizations.product` (migration 0045, most-specific-match) is the live axis. `licences.permitted_products` is the licence-side statement and is unread | **PARTIALLY EXISTS.** ADR 0037 open question 7 is **stale** and is an owed `architect` correction (§12.3) | Nothing |

### 1.1 RULING BI-4I-1 — brand narrows, never sources

RECON **C-2** recorded a genuine inconsistency: `tenant_jurisdiction_configs`
is keyed `(tenant_id, jurisdiction_id, effective_from)` with **no brand
dimension**, while `asset_authorizations`, `risk_rules` and
`open_bet_self_exclusion_policies` all carry brand *alongside* jurisdiction
as if they were orthogonal.

**Ruling: they are not orthogonal, and the configuration layer is right.**
A brand does not have an operating jurisdiction of its own. What a brand
has is a possibly-narrower subset of what its tenant is licensed to do.
Concretely:

- The **set of jurisdictions in play** for an operation is a tenant-level
  fact, derived from the tenant's licence and its
  `tenant_jurisdiction_configs` rows.
- A **brand-scoped enforcement row** (`asset_authorizations` layer 5,
  `risk_rules.brand_id`, an `open_bet_self_exclusion_policies` tightening)
  narrows what is permitted *within* that set. It can never introduce a
  jurisdiction the tenant is not configured for, and it can never widen.
- Consequently a resolution produced for brand A is **refused**, never
  silently widened, when presented to a brand-B operation — which is
  exactly SEC §S-3.3 case 2's required behaviour, now with a stated reason
  rather than a convention.

Two consequences `backend` must implement: `jurisdiction_resolutions.brand_id`
is **nullable** (a tenant-level or system-level resolution legitimately has
no brand), and where it is non-NULL the consuming gate re-asserts it
(§4.4 Layer 2). No schema change to `tenant_jurisdiction_configs` is
authorized or needed.

This ruling is reversible: if a future licensing arrangement genuinely
gives a brand its own regulator, that is a new ADR and a brand dimension on
the configuration table — not a silent reinterpretation of the enforcement
rows.

### 1.2 RULING BI-4I-2 — `kyc_documents.issuing_country` is evidence, never a source

IC §2 is **adopted in full and without amendment**. Restated as the binding
rule so no implementer has to reconstruct it:

> No code path may read `kyc_documents.issuing_country` and assign it, or
> any value derived from it, to a residence field, a nationality field, a
> `jurisdiction_code`, or a resolution's `selected_basis`. Its only
> permitted participation is as `basis = kyc_corroboration` with status
> `agreed` / `disagreed` / `unavailable` against an
> independently-captured residence fact — and **its value is never
> recorded** (§5.3 item 1).

Until concept 2 exists (HDR-J-3), `kyc_corroboration` has nothing to
corroborate and the resolver must not consult it at all. RECON's Q-9
answer "none, and that is a legitimate and probably safer answer" is
therefore the **operative** answer for Stage 4I.

### 1.3 RULING BI-4I-3 — `tenant_jurisdiction_configs` keeps four of its five roles

Closing RECON **Q-12** / **C-7**. `tenant_jurisdiction_configs` remains the
per-tenant carrier of the KYC / AML / RG / reporting ruleset references,
the geo-block list, and the allowed payment methods. Its
`allowed_currencies` column is **superseded** by `asset_authorizations` for
the asset-availability question (migration 0045's own header already says
so) and must not be read for that purpose. It is not dropped in Stage 4I —
dropping a column on a table with RLS and effective-dating is its own
change with its own review, and nothing reads it today either way.

`docs/architecture/15-jurisdiction-and-licensing-model.md` currently
describes an enforcement model none of this reflects. Correcting doc 15 is
an owed `architect` action, listed in §12.3, deliberately **not** performed
in this phase (it depends on §11's buildable/blocked split landing first,
and doing it now would document a resolver that does not exist).

---

## 2. The resolution interface — the contract every domain implements against

### 2.1 What the resolver is, and is not

**The resolver answers exactly one question:** *for this operation, of this
class, by this subject, under this tenant and brand — which jurisdiction's
rules govern, and on what basis?*

It does **not**: decide whether the tenant may lawfully serve that
jurisdiction (that is a separate authorization check, §9.6 and RISK §7's
HDR-J-6 boundary); decide whether a limit is breached (Risk); decide
whether an asset is authorized (AssetAuthorization); or decide whether the
player may gamble (RG).

**Ownership (closing RECON Q-15, which `docs/governance/ownership.md` has
no row for).** The resolver is **platform core**. Its *interface contract*
is `architect`-owned (this document). Its *implementation* is
`backend`-owned. Its *source-precedence ruleset content* is
`identity-compliance`-owned, mirroring doc 15's existing schema/content
split. It is **not** owned by `internal/risk` (RISK §4.3, adopted), and it
is not owned by any consuming domain. `docs/governance/ownership.md` needs
a jurisdiction row recording this — owed action, §12.3.

### 2.2 The outcome states — reconciling three vocabularies

The dispatch named five states (successful / uncertain / unavailable /
conflicting / unsupported); SEC §S-2.2 specified three persisted ones
(`resolved` / `unresolved` / `refused`); RISK reasons in terms of
"empty vs. present." These are reconciled on **two axes**, and the
distinction between the axes is load-bearing:

**Axis 1 — `outcome`, three values. This is the contract axis. Every gate
branches on this and only this.**

| `outcome` | Meaning | Carries a code? | Gate behaviour |
|---|---|---|---|
| `resolved` | The inputs determine exactly one jurisdiction, at or above the confidence the operation class requires | **Yes**, FK to `jurisdictions (code)` | Proceed to the gate's own logic |
| `unresolved` | The inputs do **not** determine a jurisdiction — absent, insufficient, or irreconcilable | **No**, NULL | **Never ALLOW.** Per-consumer contract in §6 |
| `refused` | The resolver declined to answer — it was not asked a question it can answer, or a dependency was unavailable | **No**, NULL | **Never ALLOW.** Per-consumer contract in §6 |

**Axis 2 — `reason`, a diagnostic enum. Recorded always; branched on by no
gate, ever.** The dispatch's five-state vocabulary maps onto it exactly:

| Dispatch term | `outcome` | `reason` |
|---|---|---|
| successful | `resolved` | `determined` |
| uncertain | `unresolved` | `insufficient_confidence` — a basis was available but ranks below the operation class's requirement (IC §4's "an unverified declaration is not dispositive for enforcement-grade decisions", enforced structurally per RISK §3.1) |
| *(absent)* | `unresolved` | `no_signal` — no basis of any kind was available. **This is Stage 4I's universal answer for player-scoped operations** (§11.3) |
| conflicting | `unresolved` | `irreconcilable_bases` — two or more bases disagree and the precedence configuration does not determine a winner. Never an arbitrary pick, never "prefer the permissive", never "prefer the most recent" (SEC §S-3.3 case 8) |
| unavailable | `refused` | `dependency_unavailable` |
| unsupported | `refused` | `unsupported_operation_class` |
| — | `refused` | `scope_mismatch` — the tenant/brand/player binding failed its cross-check (§4.4 Layer 2) |
| — | `refused` | `registry_unknown_code` — a code was produced that is not in `jurisdictions`. See §2.5 |

**Why two axes and not one flat enum.** A gate that branches on eight
values will eventually branch on seven of them correctly and one of them
wrongly, and the wrong one will be an ALLOW. A gate that branches on three
values, only one of which can proceed, cannot make that mistake. The
diagnostic detail is for humans, incident response, and regulators — it is
recorded in full and is never a control input. This is the same discipline
ADR 0031 §34 applied to keeping `Outcome` at four values, and the same
reason RISK §2.3 refused a fifth `RuleStatus`.

**`unresolved` vs. `refused` is not cosmetic** (SEC §S-2.2, confirmed):
they have different remediations. `unresolved` means "get better inputs";
`refused` means "fix the caller or the dependency." Collapsing them makes
an outage indistinguishable from a data gap.

### 2.3 The `Resolution` value — non-forgeable by construction

```go
// Package jurisdiction. ILLUSTRATIVE SIGNATURE — the exact field/method
// names are `backend`'s; the PROPERTIES below are binding.
//
// Resolution is produced ONLY by Resolve. Its fields are unexported, so a
// struct literal of this type does not compile outside this package
// (SEC §S-1.3 Layer 1). There is deliberately no exported constructor,
// no setter, and no function anywhere in this package that accepts a
// jurisdiction code from a caller and returns a Resolution.
type Resolution struct {
    recordID       uuid.UUID // FK into jurisdiction_resolutions (SEC AR-2)
    outcome        Outcome   // resolved | unresolved | refused  (§2.2)
    reason         Reason
    code           string    // jurisdictions.code; "" unless outcome == Resolved
    id             uuid.UUID // jurisdictions.id;  Nil unless outcome == Resolved
    asOf           time.Time
    policyVersion  string
    // Binding scope — re-asserted by every consuming gate (§4.4 Layer 2).
    tenantID       uuid.UUID
    brandID        *uuid.UUID
    playerAcctID   *uuid.UUID
    operationClass OperationClass
}

func (r Resolution) Outcome() Outcome
func (r Resolution) Code() string     // panics or errors unless Resolved
func (r Resolution) ID() uuid.UUID    // panics or errors unless Resolved
func (r Resolution) RecordID() uuid.UUID
func (r Resolution) AsOf() time.Time

// Resolve takes a READ-ONLY database handle (§6.4), never a pgx.Tx.
func Resolve(ctx context.Context, q ReadOnlyQuerier, p Params) (Resolution, error)
```

**Binding properties** (each is mechanically checkable; `qa` asserts each):

1. **`Resolution{...}` does not compile outside the resolver package.**
   SEC §S-6 case D-1 is a compile-fail test, not a runtime assertion.
2. **`Code()` and `ID()` are unreachable for a non-`resolved` outcome.**
   There is no path by which a caller obtains `""` or `uuid.Nil` from a
   `Resolution` and proceeds as though it had a value. This is the single
   most important property: it is what structurally prevents RECON C-3's
   "absent looks like unscoped" family of defects from recurring.
3. **Both `code` and `id` come from one resolution.** No domain performs
   its own code→id translation (RISK §3.3's ask, adopted).
   `bonus.resolveJurisdictionID` (`internal/bonus/eligibility.go:139-152`)
   is **deleted**, not wrapped — see §2.5.
4. **The value carries its own scope binding** and every consumer
   re-asserts it (§4.4 Layer 2).
5. **`Resolve` cannot write.** Its `q` parameter exposes only `Query` /
   `QueryRow` (§6.4).

### 2.4 Q-2 finally answered: the code is canonical, the id is carried

`jurisdictions.code` is the **canonical carrier**. Reasons, all
code-verified at `89af4c0`:

- `risk_rules.jurisdiction_code` is `TEXT` with an FK to
  `jurisdictions (code)`, is inside migration 0041's immutability trigger's
  core-field set, and is compared by string equality at
  `internal/risk/evaluator.go:174`. Migrating it is a migration on an
  append-only table plus a rewrite of every authored rule, for nothing
  (RISK §3.3).
- `casino_launch_sessions.jurisdiction_code`,
  `bonus_grants.jurisdiction_code`,
  `open_bet_self_exclusion_policies.jurisdiction_code` and
  `withdrawal_policies.jurisdiction_code` are all code-keyed.
- `asset_authorizations.jurisdiction_id` is the **only** id-keyed consumer.

So: the `id` is an internal representation detail of one consumer, carried
alongside the code on the resolution so that consumer never has to look it
up. It is not the canonical identity of a jurisdiction.

**Independently re-verified sharpening.** RECON §1.2 stated that the
jurisdiction code space is not the ISO-3166 country space, and this is
confirmed by migration 0002's own comment
(`code TEXT NOT NULL UNIQUE, -- e.g. 'KM-ANJ', 'MT', 'CO'`). `KM-ANJ` is a
sub-national regulator code; `MT` and `CO` happen to collide with ISO
alpha-2 values. **That partial collision is a trap, not a convenience**: it
means a country→jurisdiction mapping bug produces correct-looking results
for most inputs and silently wrong ones for the platform's own first
licensing jurisdiction. Any country→jurisdiction mapping is therefore an
explicit, stored, audited mapping — never a string equality, never a
prefix match, never an implicit cast. This binds `payments` (§6.5) and
binds any future use of IC §5's ISO-3166 `declared_residence_country`.

### 2.5 The unresolved/unknown collapse is closed structurally

RISK §3.3 flagged a **live latent fail-open**, re-verified at
`internal/bonus/eligibility.go:139-152` at `89af4c0`:
`resolveJurisdictionID` returns `uuid.Nil` for an **empty** code *and* for
an **unknown** code. For AssetAuthorization both deny (fine). For Risk they
differ catastrophically: an empty code is caught by `missingScopeContext`'s
gate, while an unknown-but-non-empty code matches **zero rules** and
resolves to **ALLOW**.

**Ruling: the collapse is closed structurally, in three places at once, so
that no single regression reopens it.**

1. A code that is not in `jurisdictions` can never be produced by the
   resolver: `Resolve` returns `refused(registry_unknown_code)`, not a
   `Resolution` carrying an unregistered code.
2. A code that is not in `jurisdictions` can never be *stored*: every
   persisted jurisdiction value carries an FK to `jurisdictions (code)` or
   `(id)` (SEC §S-1.3 Layer 3, already the precedent at migration 0042).
3. `bonus.resolveJurisdictionID` is **deleted** when the resolver lands.
   Bonus obtains both representations from one `Resolution`. A wrapper
   preserving the old signature would preserve the defect.

`qa` case D-4 (SEC §S-6) remains mandatory as the regression guard.

---

## 3. The source hierarchy

### 3.1 The canonical `basis` enum — closed, with reserved values

Every basis is one of the following. The set is closed; adding a value is
an ADR, not an implementation detail.

| `basis` | Meaning | Producible by `Resolve` in Stage 4I? |
|---|---|---|
| `player_verified_residence` | `kyc_verifications.verified_residence_country`, set by an explicit reviewer determination (IC §5) | **No** — blocked on HDR-J-3 |
| `player_declared_residence` | `player_accounts.declared_residence_country`, self-reported, unverified (IC §5) | **No** — blocked on HDR-J-3 |
| `kyc_corroboration` | Corroborates or disagrees with another basis. **Never a `selected_basis`** (§1.2) | **No** — nothing to corroborate |
| `geo_signal` | A point-in-time location signal (IP-derived or provider-supplied) | **No** — no producer exists; RECON Q-7 |
| `retail_node` | A registered shop address (doc 26 §1.4). Reserved so the shape does not have to change when retail arrives (RECON Q-17) | **No** — retail not implemented |
| `tenant_licence` | The tenant's own licensing jurisdiction, via `tenants.licence_id → licences.jurisdiction_id` | **Yes — but only for tenant-subject operations.** See §3.2 |
| `tenant_asserted` | A BYOL tenant's own determination. Reserved so HDR-J-5 does not require a redesign (IC §3, SEC §S-9) | **No** — blocked on HDR-J-5 |
| `platform_fallback` | A fallback from tenant/brand jurisdiction to a player's | **No — and structurally unproducible until HDR-J-1 is answered.** §3.3 |
| `staff_supplied` | A jurisdiction a staff member typed into a request body | **No, and never.** §8.4 |

**Binding on `backend`: `Resolve` must be structurally incapable of
emitting `staff_supplied` or `platform_fallback`.** These two exist in the
enum only so that (a) the SEC-4I-F2 interim audit records are
distinguishable from post-resolver records forever, and (b) a future
HDR-J-1 "yes" answer adds a producer rather than a schema migration. A
basis value that the resolver cannot produce and that no code path can
assign is an inert label, which is exactly what is wanted.

### 3.2 `tenant_licence` — producible, and precisely bounded

This is the one basis Stage 4I can genuinely produce, and it is also the
most dangerous one, because using it for the wrong subject **is** HDR-J-1's
forbidden fallback wearing a different name.

**Binding boundary:**

> `basis = tenant_licence` may be selected **only** when the operation's
> subject is the tenant or the brand itself — e.g. resolving which
> jurisdiction's configuration governs a catalogue-availability question
> asked about a tenant, or which precedence configuration applies (§3.4).
> It may **never** be selected for an operation whose subject is a
> **player**. A player-scoped operation with no player-side basis available
> resolves `unresolved(no_signal)` — never `resolved` on the tenant's
> licence.

The distinction is recorded on the resolution row itself: a resolution with
a non-NULL `player_account_id` may not carry `selected_basis =
tenant_licence`. **This must be a database `CHECK` constraint, not an
application convention** — it is the single rule whose violation would
silently answer HDR-J-1 in the affirmative, and CLAUDE.md's multi-tenancy
section already establishes that this class of rule belongs in the database
rather than in discipline.

### 3.3 Pre-KYC declaration vs. post-KYC evidence — and where the confidence threshold lives

IC §4's framing is **adopted**, and RISK §3.1 supplies the mechanism that
gives it teeth. Combined, the canonical rule:

1. A **declared** residence is a real signal and is not worthless. It is
   sufficient for low-stakes, reversible, non-enforcement uses.
2. A declared residence is **never on its own dispositive for an
   enforcement-grade decision** — a jurisdiction-scoped `HARD_LIMIT`, an
   `asset_authorizations` layer-6 gate, a withdrawal jurisdiction policy —
   unless HDR-J-2 says otherwise for that operation class specifically.
3. **The confidence threshold lives in the resolver, keyed on operation
   class. It does NOT live on rules.** RISK §3.1's four reasons are adopted
   in full; the disqualifying one is the first: a per-rule
   `min_jurisdiction_source` predicate would make `matches()` return false
   for a present-but-insufficient value, silently excluding the rule —
   which `missingScopeContext` structurally cannot catch, because it tests
   emptiness. That is the exact fail-open class the gate exists to prevent,
   reintroduced outside the reach of the mechanism built to catch it.
4. Therefore: an enforcement-grade operation with only a declared residence
   available receives `unresolved(insufficient_confidence)` — **empty, not
   "resolved, low confidence."** Risk's existing string parameter and
   existing conditional gate then do the rest, **with zero new surface in
   `internal/risk`.**
5. KYC evidence does **not** blanket-outrank a declaration. Its authority is
   operation-class-dependent (HDR-J-2) and document-type-dependent (IC §2).
   A verified residence outranks a declared one for KYC/AML/reporting-class
   operations; whether it outranks a real-time location signal for *play*
   is precisely HDR-J-2 and is not answered here.

### 3.4 Precedence configuration — keyed on the licence side (closing RISK §4.2's circularity)

IC §3 observed that source precedence is likely per-jurisdiction
configuration. RISK §4.2 found the bootstrap circularity in that: selecting
a per-jurisdiction precedence rule requires knowing the jurisdiction, which
is the output of applying the rule.

**RISK §4.2's correction is adopted as canonical.** The precedence
configuration is keyed on

> **`(tenant licensing jurisdiction, operation_class)`**

— both knowable before any player-side resolution runs.
`tenants.licensing_model` is already server-resolved on every operation
today (`resolveLicensingMode`, RECON P-6), and `licences.jurisdiction_id`
becomes readable as part of §11.1's buildable work. This preserves IC's
insight that precedence is configuration rather than a global constant,
while making it computable, and it composes with
`risk_rules.licensing_mode`'s existing rationale (ADR 0031 §10): the
platform already treats the licensing side as the knowable coarse anchor
and the player side as the fine, currently-absent one.

The **table shape** is buildable now. Its **rows** are blocked on HDR-J-2.
A resolver with no precedence rows resolves `unresolved`, which is correct
and is the Stage 4I steady state (§11.3).

**Amendment (Stage 4I Phase D, `docs/decisions/0043-jurisdiction-
evaluation-policy-configuration.md`).** `jurisdiction_precedence_configs`'
role is widened, not replaced: it is now "the effective-dated
evaluation-policy configuration for a licensing jurisdiction and operation
class, of which the source-precedence ordering described above is one
(currently unset) component." Phase D adds the PC-GAP-1/PC-GAP-2 location-
policy knobs (`status`, `location_requirement`,
`max_location_signal_age_seconds`, `precedence_status`, and their
accompanying provenance columns) to this SAME table, at the SAME key —
closing PC-GAP-4. See §14.6 and §15 below for the full account; this
paragraph exists so a reader of §3.4 alone is not left believing the table
remains shape-only for precedence content alone.

### 3.5 RISK §4.1's invariant, adopted verbatim

> **One operation resolves exactly one jurisdiction, and every gate in that
> operation's chain consumes that same value. No gate re-resolves,
> overrides, narrows, or substitutes it.**

This is binding platform-wide. It is what makes a denial interpretable, it
is what `bonus.GateCheckpoint`'s two-representation `GateParams` already
assumes, and it is what §7's MROC is carefully constructed **not** to
violate.

---

## 4. The four consumer contracts, finalized

### 4.0 The canonical absent-value invariant (closing RECON Q-3)

RISK §1.4's proposed wording is **adopted as the platform-wide rule**, in
preference to RECON Q-3's own framing of "adopt absent ⇒ deny uniformly":

> **An absent jurisdiction value must never cause a jurisdiction-dependent
> policy, gate, restriction or authorization to be evaluated as if it did
> not exist. Where a consumer cannot determine whether any such policy
> applies without a jurisdiction, it denies. Where a consumer can determine
> that no such policy is in force, it may proceed — and must deny the
> moment one is.**

RISK's §1.4 correction to RECON **C-3** is accepted: there are **two
correct specialisations of one invariant, plus two genuine defects**, not
four incompatible contracts. Adopting AssetAuthorization's *specialisation*
platform-wide would over-apply it to domains with a different structure;
adopting the *invariant* is what unifies them.

This is also what §9 uses to correct RISK's own K-3 recommendation.

### 4.1 AssetAuthorization (K-1) — UNCHANGED

`assetregistry.CheckEligibility` layer 6 keeps **unconditional
fail-closed**: `uuid.Nil` is an immediate denial at
`ReasonJurisdictionContextMissing`, never "unscoped." Layer 6 is
*structurally* jurisdiction-keyed (`asset_authorizations` is keyed
`(tenant, jurisdiction[, product])`), so with no jurisdiction there is no
lookup to perform — the question is unanswerable, not answerable-as-yes.

Only change: the caller passes `resolution.ID()`, reachable only for a
`resolved` outcome, instead of a separately-translated uuid (§2.5).
`assertTenantScope` (`internal/assetregistry/authorization.go:199-212`)
is unchanged and remains the model the resolver copies (§4.4 Layer 2).

**Consequence, stated plainly: Bonus stays blocked in Stage 4I.** §11.3.

### 4.2 Risk (K-2) — conditional fail-closed RETAINED, per R-1

RISK **R-1** is accepted as `risk`'s ruling in `risk`'s own domain and is
binding: `risk.Evaluate` does **not** become unconditionally fail-closed.
The existing contract — fail closed iff at least one currently-effective
rule visible to this request scopes `jurisdiction_code` for this operation
— is the correct specialisation of §4.0's invariant for a domain where
jurisdiction-dependence is a per-rule configuration fact.

`RiskRequest.JurisdictionCode string` is unchanged. The `Resolution` is
**not** threaded into `RiskRequest` (RISK §3.2: a field that matching
ignores is an invitation). The enforcement point carries the resolution's
provenance into the audit record alongside the `RiskDecision`.

**The authoring-time precondition (R-2b), specified.** RISK ruled that
`risk.CreateRule` (`internal/risk/policy_service.go:145`) must reject
creating a rule that scopes `jurisdiction_code` for an operation whose
`(tenant, operation)` pair is not recorded as jurisdiction-resolution-
active. `architect` supplies the piece RISK correctly declined to define,
because it is not Risk's to own:

- **The fact is `jurisdiction_resolution_active (tenant_id,
  operation_class, active, effective_from, ...)`** — a tenant-scoped
  configuration table owned by the resolver, not by `internal/risk`.
  Tenant-scoped, `ENABLE` + `FORCE` RLS, composite FKs, per-command
  policies, audited on change (SEC §S-2.4 class 3).
- **Risk consumes it read-only through one narrow accessor** exposed by the
  resolver package. Risk defines no enablement table, no flag column and no
  `StaffRole` (RISK §2.4, respected).
- **Write permission** follows `PermAssetAuthorizationWrite`'s precedent:
  tenant-scoped, granted to `RoleTenantAdmin`, never `RolePlatformAdmin`
  (SEC §S-1.7).
- **It is buildable now** and is not blocked on any HDR item.
- Until it exists, the fallback is RISK §2.4a's fixed activation order plus
  RISK §2.2's enumeration as a hard `qa`-asserted gate. That is weaker but
  acceptable, and is **not** a blocker on the resolver.

RISK **R-2** (no shadow / observe / dry-run `RuleStatus`) is likewise
accepted and binding, including the explicit trap that `RuleRiskSignal` is
not a usable shadow mode because `internal/casino`'s `classifyRiskOutcome`
collapses REVIEW into a block.

### 4.3 Casino per-game blocklist (K-3) — remediation spec in §9

Broken out into its own section because it is the one place where this
synthesis materially corrects a prior phase's recommendation.

### 4.4 The three-layer non-forgeability mechanism (SEC §S-1.3), adopted

Binding on every jurisdiction-consuming enforcement path. All three layers
are required; one or two is not compliance.

- **Layer 1 — no parse path.** The field is absent from every
  enforcement-facing request struct (`decodeJSON`'s
  `DisallowUnknownFields`, re-verified at
  `internal/httpserver/json.go:14`, then makes a submitted value a **400**
  for free). The resolution type is non-forgeable (§2.3). Resolution inputs
  are exactly: tenant from `tenant.FromContext`; the player account from
  the authenticated subject or a tenant-scope-validated path parameter; the
  brand **derived from the player account row** (the existing pattern,
  `internal/httpserver/casino_handlers.go:183`); and the operation class,
  a **compile-time constant at each call site, never a string from the
  wire**.
- **Layer 2 — context cross-check.** The resolver asserts the transaction's
  `app.tenant_id` GUC against the tenant it resolves for, by the same query
  `assertTenantScope` uses. Every consuming gate re-asserts the
  resolution's `tenantID` / `brandID` / `playerAcctID` binding against its
  own authenticated context before use. A resolution for player X is
  **structurally unusable** in an operation for player Y.
- **Layer 3 — database.** FK to `jurisdictions` on every persisted value;
  write-once immutability triggers on operation snapshots (migration 0042's
  pattern); and the migration-0033 precedent —
  `CHECK (jurisdiction_code IS NULL)` plus the handler comment at
  `internal/httpserver/withdrawal_policy_handlers.go:39-47` — for any
  surface not yet resolution-active. SEC is right that
  `withdrawal_policy_handlers.go` is the **reference implementation** of
  this contract already in the repository; `backend` should cite it, not
  the Bonus handlers.

### 4.5 Payments (K-8) — explicitly NOT resolved in this phase

`payments` has not had its design phase. This document **does not** specify
the payments contract and no implementer should infer one. What is recorded
for that phase:

- RECON **C-3(d)** is **two defects, not one** (RISK §1.4): an inverted
  absent-value semantic (`SupportedCountries` empty means *permissive*,
  `internal/payments/types.go:188-191`), **and** a code-space confusion
  (ISO-3166 country vs. `jurisdictions.code`).
- Routing dimension 2 is still `TODO(jurisdiction)`
  (`internal/payments/orchestrator.go:83-103`), re-confirmed at `89af4c0`.
- Any country→jurisdiction mapping is an **explicit, stored, audited
  mapping** (§2.4), is an **authorization surface**, and requires
  `security` review when proposed (SEC §S-10).
- `withdrawal_policies`' `CHECK (jurisdiction_code IS NULL)` (migration
  0033) is **not lifted in Stage 4I**. Lifting it is a deliberate, reviewed
  act in the phase that has a real jurisdiction to write, not a side effect
  of the resolver existing. RECON Q-16 is thereby answered for Stage 4I:
  **explicitly deferred, not silently deferred.**

---

## 5. The audit and provenance model

SEC **AR-1** and **AR-2** are adopted as binding, unchanged in substance.

### 5.1 Two artefacts, deliberately separate

- **`jurisdiction_resolutions`** — its own append-only, tenant-scoped
  table. One row per resolution attempt, **including failures**
  (`unresolved` and `refused` are recorded outcomes, never absent rows).
- **`audit_log`** — receives an entry for exactly four event classes
  (SEC §S-2.4): `jurisdiction_registry.*` (writes to `jurisdictions` /
  `licences` / `tenant_jurisdiction_configs`);
  `jurisdiction_fact.corrected` (a staff correction to a player's
  determining fact); `jurisdiction_resolution_active.changed` (RISK
  §2.4b's toggle); and `jurisdiction.resolver_unavailable`, **aggregated**.

Per-request denials caused by an unresolved jurisdiction are **not**
individually written to `audit_log`. They are recorded by the existing
per-domain denial mechanisms plus the `jurisdiction_resolutions` row.

**SEC §S-2.5's amplification guard is binding**:
`jurisdiction.resolver_unavailable` is emitted at most once per
`(tenant, operation_class, time_bucket)` with a count. A per-request entry
would let one degraded dependency, or one attacker, write unbounded
immutable rows into the platform's most retention-sensitive table —
`audit_log` is append-only by trigger (`audit_log_deny_mutation`, migration
0014) and cannot be pruned by the application even deliberately.

### 5.2 `jurisdiction_resolutions` — the finalized column set

SEC §S-2.2's field list is adopted with four `architect` additions, marked
**[new]**.

| Column | Type / constraint | Notes |
|---|---|---|
| `id` | `UUID PK` | AR-2's FK target |
| `tenant_id` | `UUID NOT NULL` | RLS key |
| `brand_id` | `UUID NULL`, composite FK `(brand_id, tenant_id) → brands (id, tenant_id)` | NULL is legitimate (§1.1) |
| `player_account_id` | `UUID NULL`, composite FK `(player_account_id, tenant_id)` | NULL for tenant-subject and config-time resolutions |
| `operation_class` | enum, `NOT NULL` | Compile-time constant at the call site, never from the wire |
| `requested_by_actor_type` / `requested_by_actor_id` | `staff` \| `player` \| `service` \| `system`, matching `audit_log`'s existing CHECK vocabulary (migration 0014) | For `system`, actor id NULL and the **job name** recorded (`deposit_sweep`, `cashback_scheduler`) |
| `outcome` | `resolved` \| `unresolved` \| `refused`, `NOT NULL` | §2.2 axis 1 |
| **`reason`** **[new]** | enum, `NOT NULL` | §2.2 axis 2. SEC's list did not carry the diagnostic axis; without it `unresolved` is uninvestigable |
| `jurisdiction_code` | `TEXT NULL`, FK → `jurisdictions (code)` | `CHECK (jurisdiction_code IS NOT NULL) = (outcome = 'resolved')` |
| `selected_basis` | enum `NULL` | §3.1. `CHECK (player_account_id IS NULL OR selected_basis <> 'tenant_licence')` — §3.2 |
| `considered_bases` | enum values **with per-basis status only** (`selected` / `rejected_lower_precedence` / `unavailable` / `disagreed`) | **Never the values those bases held** (§5.3). Projected out for a caller lacking `verification:read` |
| `confidence_class` | enum `NULL` | The bucket the resolver applied for this operation class. **Never a raw vendor score** |
| `resolver_policy_version` | `TEXT NOT NULL` | Without it a decision made under an older precedence rule is not reproducible |
| `registry_version` / `config_effective_from` | | Which `tenant_jurisdiction_configs` / `licences` row was in force |
| `as_of` | `TIMESTAMPTZ NOT NULL` | Load-bearing for §6.4's staleness bound |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT now()` | Append-only insert time |

Three further **[new]** `architect` requirements on the table:

1. **No `UNIQUE` constraint keyed on an operation.** More than one
   resolution may legitimately exist for one operation — today because a
   retry produces a second attempt, and in future because §7's MROC
   produces one per candidate jurisdiction. A uniqueness constraint added
   now for tidiness would foreclose §7 and require a migration on an
   append-only table to undo. This is the **only** MROC-related schema
   requirement in Stage 4I (§7.4).
2. **No composition column in Stage 4I.** `composition_group_id` and any
   equivalent is deferred until HDR-J-4's record-of-authority half is
   answered; the non-foreclosure requirement above is sufficient.
3. **`operation_class` is seeded with only the classes that have a Stage 4I
   consumer**: `play`, `catalogue_availability`, `bonus_issuance`,
   `bonus_conversion`. `deposit`, `withdrawal`, `kyc_aml_determination`
   and `reporting` are added by the phase that builds their consumer, not
   speculatively now (CLAUDE.md's no-uncontrolled-scope rule).

### 5.3 What must NEVER be recorded

SEC §S-2.3's governing principle is adopted verbatim and is the rule from
which the list below is derivable:

> **A resolution record persists the DECISION and REFERENCES to its
> evidence. It never persists the evidence VALUES.** Reconstruction is
> performed by re-reading the referenced evidence under that evidence's own
> access control and retention rule — never by reading a copy made into a
> store with different access control and a longer retention.

**An implementation that records any of the following is a blocking
finding.** This extends `docs/security/security-architecture.md` §B1.4's
existing never-log list, which continues to apply in full.

1. `kyc_documents.issuing_country`'s **value** — never, including when it
   was a corroborating input. Record `basis = kyc_corroboration`,
   `evidence_ref = kyc_documents.id`, the `document_type` (needed, because
   §1.2 requires types to be treated differently), and the agreement flag.
2. The player's **declared or verified residence country value**.
3. **Nationality**, in any form.
4. Any **raw IP, geo-coordinate, city, ISP or geolocation-vendor payload**
   as a jurisdiction-evidence field. Record `basis = geo_signal` and
   `evidence_ref = sessions.id` or the request id, and nothing more.
5. **`persons.person_key_hash` or any cross-brand identity correlator.**
   `jurisdiction_resolutions` is tenant-readable; `persons` is deliberately
   platform-scoped (ADR 0015). Putting the correlator there hands every
   tenant a join key for correlating the same human across other operators'
   brands — a cross-tenant privacy leak with no attacker required, and the
   most likely accidental version of the mistake because a future
   implementer will want it for reporting.
6. Full name, date of birth, address, phone, email, document number,
   document image or document URL.
7. **Vendor raw responses**, verbatim. Store the mapped enum plus the
   vendor's opaque reference id.
8. Provider or vendor **credentials, HMAC secrets or API keys**.

The one thing that *is* recorded and approximately discloses residence is
the resolved `jurisdiction_code` itself. That is unavoidable and correct:
the jurisdiction **is** the decision, RECON C-5 is the finding that not
recording it is a defect, and a decision that cannot be reconstructed
cannot be defended to a regulator.

---

## 6. RLS, isolation, lock ordering, and caching

### 6.1 Universal schema rules (SEC §S-3.1, adopted)

Binding on `jurisdiction_resolutions` and on `jurisdiction_resolution_active`:

- `tenant_id UUID NOT NULL`; `ENABLE ROW LEVEL SECURITY` **and** `FORCE ROW
  LEVEL SECURITY` (FORCE is the load-bearing half — the application role
  owns these tables).
- No `BYPASSRLS` assumption anywhere. Every read/write goes through
  `db.Pool.WithTenant` / `WithPlayerScope` / `WithPrincipalScope` /
  `WithoutTenant`. **A resolver query on a bare pool connection reads zero
  rows under FORCE RLS**, which an unwary implementation reports as
  "no residence on file" — a silent-wrong-answer bug that becomes a
  fail-closed denial with a misleading cause. It must **error**, not return
  `unresolved` (`qa` cases B-2 / G-3).
- **Composite foreign keys, never plain ones** (migration 0043's
  precedent).
- Per-command policies. **No `FOR ALL` policy. No DELETE policy. No UPDATE
  policy on `jurisdiction_resolutions`.** Append-only enforced by a
  `BEFORE UPDATE OR DELETE` trigger (`ledger_deny_mutation()`'s pattern),
  not only by the absence of a policy — a trigger is not bypassed by table
  ownership, which is precisely why `audit_log_immutable` exists. Plus a
  `BEFORE TRUNCATE ... FOR EACH STATEMENT` deny trigger.
- **No player-read policy on `jurisdiction_resolutions`.** A player who can
  read it learns which signal the platform trusted and which it ignored —
  directly attack-useful (§6.3). Anything player-facing is a **curated
  server-side projection**, never a passthrough.
- **Staff read is its own permission**, not implied by `audit:read`,
  `bonus:read` or `player:read`; `considered_bases` is projected out for a
  caller lacking `verification:read`.
- **`jurisdictions` stays platform-scoped with read-open RLS** (SEC
  §S-3.2). **CORRECTED (Stage 4I Phase E-SECURITY, migration 0077, ADR
  0046): `jurisdictions` DOES carry row-level security as of migration
  0077** — `ENABLE`+`FORCE ROW LEVEL SECURITY` with a `jurisdictions_read`
  policy that is `USING (true)` (read-open, unchanged in effect from the
  "no RLS" claim this sentence originally made), plus write policies
  restricting INSERT/UPDATE to a genuinely platform-admin-scoped
  transaction (there was previously no database-level backstop on writes
  at all, closed by the same migration). It is a platform fact and an FK
  target every tenant-scoped transaction must read, which is exactly why
  the READ side stays open — this sentence's original point about reads
  is unchanged; only "no RLS" (which was never true of the write side,
  and is no longer true of the table at all) is corrected. Only the
  **write** side changes: one new **platform-only**
  permission on `RolePlatformAdmin`, following `PermCasinoCatalogueManage`'s
  exact precedent (`internal/auth/permission.go:76-82`). Per-tenant
  jurisdiction configuration writes follow `PermAssetAuthorizationWrite`'s
  precedent instead (tenant-scoped, `RoleTenantAdmin`). **No other
  permission and no new role is authorized** *(amended by Stage 4I Phase D,
  `docs/decisions/0043-jurisdiction-evaluation-policy-configuration.md`
  Decision 5, to: "no other permission without a recorded architect
  ruling naming it." Phase D names one such exception,
  `PermJurisdictionEvaluationPolicyWrite` — platform-only, granted only to
  `RolePlatformAdmin`, gating authoring a `jurisdiction_precedence_configs`
  evaluation-policy version. No new role is created. This amendment also
  retroactively documents that Phase B had already, correctly, added two
  more named exceptions — `PermPlayerResidenceRead` and
  `PermJurisdictionEvidenceCollectionActivate` — without this sentence
  having been updated to say so at the time. Stage 4I Phase E
  (`docs/decisions/0045-operating-market-and-country-policy-foundation.md`
  §6.1) names four further exceptions, for the structurally separate
  operating-market/country-policy domain (`internal/operatingmarket`, not
  `internal/jurisdiction`): `PermOperatingMarketCeilingManage`
  (platform-only, `RolePlatformAdmin` — authoring a licence's country
  ceiling), `PermOperatingMarketTenantPolicyWrite` (tenant-scoped,
  `RoleCompliance` only — deciding whether a tenant operates in a country
  at all), `PermOperatingMarketBrandPolicyWrite` (tenant-scoped,
  `RoleTenantAdmin` — narrowing within the tenant footprint Compliance
  already approved), and `PermOperatingMarketPolicyRead` (read/diagnostic
  surface, granted to `RolePlatformAdmin` for the ceiling only,
  `RoleCompliance` and `RoleTenantAdmin` for their own tenant's footprint).
  No new role is created. As with Phase D's own additions, no HTTP route
  exists in this phase — the permissions are declared and role-scoped so
  the phase that adds routes adds a handler, not a permission model.)*

### 6.2 The nine-scenario contract — binding on `qa`

SEC §S-3.3's table is adopted as the **binding test contract** for `qa`'s
later phase. Restated compactly; SEC §S-3.3 carries the full reasoning and
the enforcement-point citations, and SEC §S-6's seven adversarial
categories (~30 cases) remain mandatory in full.

| # | Scenario | Required behaviour | Must NOT happen |
|---|---|---|---|
| 1 | **Cross-tenant** — tenant B's valid staff token requests tenant A's resolution | RLS returns zero rows; handler surfaces **404**. A tenant-A resolution is also unusable as an input to any tenant-B operation even if an id were guessed | 200 with data; an error message distinguishing "exists but forbidden" from "does not exist"; a resolution crossing a tenant boundary in-process |
| 2 | **Cross-brand** — a brand-B-scoped token consumes a brand-A resolution, same tenant | Composite FK makes cross-tenant brand attachment impossible; within a tenant, a brand-mismatched resolution is **refused, not silently widened** (§1.1) | A brand-A resolution accepted for a brand-B operation because the tenant matched |
| 3 | **Player-scope read** — `WithPlayerScope` reads `jurisdiction_resolutions` | **Zero rows.** No player-read policy | A "my account" endpoint passing the row through instead of projecting |
| 4 | **Forged jurisdiction payload** | **400** from `DisallowUnknownFields`; the server-resolved value is used regardless | The supplied value reaching `GateParams`, `RiskRequest.JurisdictionCode`, `CheckEligibility`'s jurisdiction argument, or any snapshot |
| 5 | **Forged tenant payload** | Tenant from `tenant.FromContext` only; the resolver performs `assertTenantScope`'s identical assertion | A resolution produced for a tenant the caller never proved it was |
| 6 | **Forged brand payload** | Brand derived server-side from the player account row; a body brand on an enforcement path is refused | A body brand narrowing or widening a layer-5 answer or a brand-scoped rule match |
| 7 | **Stale context** | Every resolution carries `as_of` and `resolver_policy_version`; the enforcement point rejects a resolution older than the operation class's configured maximum age and **fails closed**. A *frozen* per-round snapshot and a *stale* resolution are different things and are distinguishable in the record | An unbounded-age resolution reused indefinitely. *(Which jurisdiction **governs** is HDR-J-4 and is not asserted by this test.)* |
| 8 | **Conflicting source** | Deterministic precedence keyed per §3.4; the disagreement is recorded (`status = disagreed`, values omitted); where precedence does not determine an answer the outcome is **`unresolved(irreconcilable_bases)`** | A gate re-resolving or overriding; a silent "prefer the permissive"; a silent "prefer the most recent" |
| 9 | **Unavailable resolver** | `refused(dependency_unavailable)`; the operation **fails closed** with a distinguishable *internal* reason code; `audit_log` receives an **aggregated** entry; the **player-facing** response does not distinguish it from "blocked" (§6.3) | Falling back to a previously-known-good answer (the prohibition `internal/assetregistry/authorization.go:30-42` already states); falling back to a tenant/brand default (**that is HDR-J-1 and is not an engineering decision**); one `audit_log` row per failed request |

### 6.3 The oracle rule — binding platform-wide, not only on casino

SEC §S-5.3's ruling is adopted and **generalized beyond casino**, because
the reasoning is not casino-specific:

> **Internal distinctness between `jurisdiction_unresolved` and
> `jurisdiction_blocked` is preserved in full** — in sentinels, denial
> records, `jurisdiction_resolutions.reason`, and operator-facing logs.
> **The player-facing response MUST NOT distinguish them**: one status
> code, one message, one body shape, on every player-facing surface.

The distinction that carries attack value is precisely the one that tells
an attacker whether their manipulation (a VPN, a proxy, a changed declared
residence, a timing trick) *registered*. Distinguishing
"jurisdiction-related denial" from "game not available" **is** permitted —
the blocklist is not a secret, the enumeration value is low, and there is
genuine consumer-transparency value in it. This is a graded ruling, not
blanket paranoia.

`qa` asserts this on the **serialised response**, not on the sentinel, and
also coarsely on **timing** (SEC cases E-1 / E-2).

### 6.4 H-2 — the read-only constraint, and its documentation placement (TENSION 2 RESOLVED)

**Substance — binding, from RISK §6.3 as elevated by SEC §S-4:**

> **The jurisdiction resolver is READ-ONLY on the evaluation path.** It
> takes no `FOR UPDATE`, acquires no advisory lock, and performs no write
> of any kind while any gate in the chain is running. Any persistence of a
> resolution happens either entirely outside the guarded transaction, or
> strictly **after** the whole gate chain completes and immediately before
> or with the effecting write.

SEC's elevation is accepted: this is not only a lock-ordering correctness
hazard, it is a **remotely triggerable availability attack**. Both
candidate lock holders are player-keyed; "two concurrent operations on the
same player" is two browser tabs; and under
`internal/risk/evaluator.go:363-377`'s fail-closed contract the resulting
`40P01` is a **DENY of a real bet**. A zero-cost, client-driven action that
produces deterministic denials on the money path is a denial-of-service
primitive.

**Therefore the constraint is enforced structurally, not by comment**
(SEC §S-4.2 — and SEC's argument is correct: a doc comment is worth exactly
what `LaunchGameParams.JurisdictionCode`'s "MUST be resolved server-side…
NEVER from client-supplied input" was worth, which is nothing, since its
one production caller never sets the field at all):

1. **`backend`:** `Resolve` does not accept a `pgx.Tx`. It accepts a narrow
   read-only interface exposing only `Query` and `QueryRow`. A write inside
   the resolver then **does not compile** — the only compile-time guarantee
   of the three.
2. **`qa`:** an integration test resolves inside `BEGIN ... READ ONLY` and
   asserts success.
3. **`qa`:** an integration test queries `pg_locks` after a resolution and
   asserts no advisory lock and no row-level lock attributable to it —
   catching a `SELECT ... FOR SHARE` that mechanisms 1 and 2 both permit.

**Preferred position:** resolution runs **before the guarded transaction
begins**, not merely before the first gate within it (SEC §S-4.3). The
TOCTOU window this creates is accepted on two conditions, both binding:
(a) the **resolution id is persisted inside the guarded transaction**
(AR-2), so the window is never invisible; and (b) **`as_of` staleness is
bounded per operation class and exceeding it fails closed** — without which
"resolve outside the transaction" silently becomes "resolve once at login
and reuse forever."

RISK **H-1** (eager, exactly-once, strictly before the first rule is
examined) and **H-3** (no network I/O of any kind between Risk's
`pg_advisory_xact_lock` and commit) are adopted unchanged.

**Documentation placement — ADJUDICATED IN SECURITY'S FAVOUR.** RISK §6.3
proposed adding a **"rule 0"** to `docs/architecture/34-economic-operation-
identity.md` §5.3's canonical lock order. SEC §S-4.5 recommended recording
it as a **precondition** instead. SEC's placement governs, for SEC's own
reason, which is correct: §5.3's rules 1–4 are an **in-transaction lock
order**, and the entire content of H-2 is that jurisdiction resolution is
**not in that order at all** — it holds no lock and ideally is not even in
the transaction. "Rule 0" invites a future author to ask what could
legitimately precede it; "this never participates" cannot be reordered,
which is a strictly stronger statement.

`architect` has recorded this in `docs/architecture/34-economic-operation-
identity.md` §5.3 in this phase, as a precondition paragraph above the
numbered list. With it in place the total order RISK wanted holds —
jurisdiction (no lock) → Risk advisory lock → EOI row lock — and AB-BA
stays structurally unreachable, which is the property §5.3 was written to
guarantee.

Confirmed, independently: casino's existing `CreateLaunchSession` write
(`internal/casino/launch.go:124`) is already compliant — it is a
post-gate-chain write. **Nothing in `internal/casino` changes for H-2.**
The hazard is created only by how the resolver is built.

### 6.5 Caching — CA-1 CONFIRMED BINDING (TENSION 6 RESOLVED)

SEC **CA-1** is confirmed: **`backend` does not build a jurisdiction
resolution cache in Stage 4I.** This is a decision for this stage, not a
deferral, and all four of SEC's grounds are re-confirmed as factual about
the repository at `89af4c0`: there is no cache infrastructure to use; a
cache would create H-3's hazard where none exists; it would undo §9.4's
availability property (resolver availability would become database
availability **and** cache availability, on the casino launch path); and no
measured performance requirement for one has been stated anywhere in this
stage's chain.

**Terms of any future reversal, binding now so a future implementer
inherits them rather than rediscovering them:** SEC §S-7.2 in full — read
only before the guarded transaction; never cache a negative outcome
(negative caching on a fail-closed control is a DoS multiplier); never
cache a basis whose invalidating write the platform cannot observe (which
excludes geo-derived bases by construction); never cache evidence; never
key without the tenant (a cache is the one place on this platform where RLS
does not protect you); write-driven invalidation with TTL as a backstop
only; **a cache hit may never authorise an operation that a fresh
resolution would refuse**; and historical reads come from the persisted
record via AR-2's FK, never from the cache and never from a recomputation.

**`architect` adds one procedural term:** reversing CA-1 requires an ADR
under `docs/decisions/` and a `security` review before implementation. It
is not a `backend` implementation choice.

---

## 7. HDR-J-4's enforcement mechanism — TENSION 1 ADJUDICATED

### 7.1 The disagreement, precisely

IC §3 proposed, as a safe engineering default for the enforcement half of
HDR-J-4: when an operation's jurisdiction changes between its stages,
**"apply the stricter of the two applicable rule sets."** RISK §7 endorsed
the **posture** (tighten, never weaken) and **dissented on the mechanism**,
proposing instead: **merge OUTCOMES, never rule sets** — evaluate twice and
take the most restrictive `Outcome` by the evaluator's own
`DENY > REVIEW > ALLOW` precedence. SEC §S-9 took no position, observing
that RISK's objection "appears correct on the code," and required only that
**the record must show both** when they differ.

### 7.2 Ruling — RISK's mechanism governs; IC's posture is retained

**IC's posture is adopted. IC's mechanism is withdrawn.** RISK's three
objections are each independently sufficient, and the second is decisive:

1. **Rule sets are not totally ordered.** Jurisdiction A may carry a lower
   `max_amount` on `casino_bet` and no cumulative cap; B the reverse. There
   is no "stricter" rule set — only a stricter *outcome for this specific
   request*. IC's mechanism presupposes an ordering that does not exist,
   so it is not merely risky, it is **not computable**.
2. **Merging rule sets manufactures a fail-closed outage.** Two
   equally-specific `RuleConfigurableLimit` rules for the same
   `(limit_kind, time_window)`, one per jurisdiction, tie on
   `specificity()` and trip `ErrConflictingRules`
   (`internal/risk/evaluator.go:26`, `:488`) — because `specificity()`
   cannot break a tie between two rules narrowing the **same** dimension to
   **different** values. IC's mechanism would therefore convert a
   consumer-protective tightening into a **denial of the whole operation**.
   A mechanism whose failure mode is "the protective case breaks" is worse
   than the problem it solves.
3. **It contradicts a recorded decision.** ADR 0031 §9 states: "A
   `RiskRequest` carries exactly one `JurisdictionCode` value, never a
   set." `architect` is the only role that may change a recorded decision,
   and declines to: a mechanism preserving the invariant exists, so there
   is no case for weakening it.

This is a mechanism adjudication, not a posture reversal. IC's underlying
compliance judgment — *tighten, never weaken; and the fact that two
jurisdictions were in play must be visible* — is **correct and is
preserved in full** by the mechanism below. IC is not overruled on
anything inside IC's own authority.

### 7.3 The canonical mechanism — Most-Restrictive-Outcome Composition (MROC)

> When an operation has **two candidate jurisdictions** — a frozen one and
> a current one, or any other pair — each **jurisdiction-dependent gate**
> in that operation's chain is evaluated **once per candidate**, and the
> operation takes the **most restrictive outcome** by **that gate's own
> declared total order** on its own outcome type. **Configuration is never
> merged.**

Three binding elaborations, generalizing RISK's Risk-specific proposal to
every gate, which is `architect`'s part of the work:

1. **Every jurisdiction-dependent gate declares a total order on its own
   outcome type, with "most restrictive" at the top.** Risk already has
   one: `DENY > REVIEW > ALLOW` (`internal/risk/evaluator.go:518-529`).
   AssetAuthorization's is `ineligible > eligible`. The casino blocklist's
   is `blocked > not blocked`. **A gate that cannot declare a total order
   on its outcomes must deny when two candidates are in play** — no
   exceptions, no ad-hoc tie-breaks.
2. **§3.5's one-operation-one-jurisdiction invariant is not violated**,
   because each *evaluation* still carries exactly one code. MROC composes
   *evaluations*; it never constructs a request carrying a set.
3. **The composition is recorded.** One `jurisdiction_resolutions` row per
   candidate (which is why §5.2 forbids an operation-keyed `UNIQUE`
   constraint), and the gate's own decision record names which candidate
   produced the restrictive outcome. This discharges SEC's "the record must
   show both when they differ" requirement.

**Two disclosed costs, carried forward from RISK §7 for whoever eventually
builds this:** the cumulative-usage ledger query
(`internal/risk/cumulative.go:262-312`) runs twice; and both Risk
evaluations hash to the **same** advisory-lock key, because the key omits
jurisdiction (`evaluator.go:337-340`), so the second acquisition is a no-op
on an already-held transaction-scoped lock — correct, but it needs a
`qa`-owned concurrency test if MROC is ever built.

**Stage 4I Phase C addendum (architect, fix-round action item) — the
canonical severity vocabulary, now that code exists to anchor it.**
`internal/jurisdiction.RestrictionOutcome` (`restriction.go`) is the first
MROC-shaped composition this codebase has built, and it fixes a concrete
three-level vocabulary: `blocked` (3) > `restricted` (2) > `allowed` (1),
via `RestrictionOutcome.Severity()`. Mapping each existing gate's own total
order onto it, per this section's own examples: Risk's `DENY > REVIEW >
ALLOW` maps `DENY→blocked`, `REVIEW→restricted`, `ALLOW→allowed`;
AssetAuthorization's `ineligible > eligible` maps `ineligible→blocked`,
`eligible→allowed` (its two-valued order has no `restricted` rung); the
casino blocklist's `blocked > not blocked` maps identically
(`blocked→blocked`, `not blocked→allowed`). **This is a naming/severity
convention for composing gate outcomes, not a new outcome axis** — it does
not replace or widen §2.2's binding three-valued `Outcome` enum
(`Resolved`/`Unresolved`/`Refused`), which remains the only axis any gate
branches on. A future gate whose own total order needs a fourth
distinguishable severity level (i.e. does not collapse cleanly onto
`blocked`/`restricted`/`allowed`) needs an ADR extending
`RestrictionOutcome`, not an ad-hoc local ordinal — extending this enum is
a policy decision (which of two composed restrictions the platform-wide
vocabulary calls "more severe"), the same governance bar §3.1 sets for the
`Basis` enum.

### 7.4 What Stage 4I actually does about MROC: the composition primitive exists; nothing calls it yet

**Corrected, Stage 4I Phase C (architect, fix-round action item).** This
section previously stated "MROC is a forward-binding specification. It is
NOT built in Stage 4I." That is no longer accurate and is withdrawn:
`internal/jurisdiction.ComposeRestrictions` (`restriction.go`) implements
exactly the composition rule §7.3 specifies — it takes every already-computed
`AppliedRestriction`, returns the outcome at the maximum severity, and
returns **every** contributor tied at that severity rather than picking
one, per §7.3 item 2's no-arbitrary-tie-break requirement. It is a pure
function: no I/O, no database handle, no jurisdiction resolution of its own
— it composes restrictions a caller has already produced.

**What is still true, and is the actual reason this remains
non-production:** `ComposeRestrictions` has **zero production call sites**.
No two-candidate case can arise yet — `DeterminePlayerJurisdiction`
(the Phase C precedence engine, §14 below) can itself emit at most one
additional-restriction candidate per call (the location dimension), and no
domain (`casino`, `bonus-engine`, `risk`, `payments`, `sportsbook`) calls
either function in its own evaluation path. `internal/jurisdiction/
resolver.go` — the only resolver any of those domains actually call today —
has zero diff throughout Phase C.

The **only** Stage 4I obligation remains the non-foreclosure requirement
already stated in §5.2: no `UNIQUE` constraint keyed on an operation, and no
composition column added speculatively. Building the composition
*primitive* was in scope for Phase C (it is a rule-engine building block,
not a production wiring decision); wiring it into any domain's live
evaluation path would be exactly the scope expansion CLAUDE.md forbids —
and would also pre-empt the half of HDR-J-4 that is genuinely human (which
jurisdiction's *record* is authoritative for a regulator-facing question,
per this section's own closing paragraph, unchanged).

**What remains genuinely human in HDR-J-4** (unchanged from IC §3's split,
which is correct): which jurisdiction's *record* is authoritative for a
regulator-facing question — which jurisdiction's SAR obligation attaches,
which data-residency rule governs the retained record. MROC answers what
the platform *enforces*. It does not, and must not be read to, answer what
the platform *reports*. §10, item J-4.

### 7.5 C-6 resolved: temporal grain (closing RECON Q-4)

RECON **C-6** found casino and Bonus disagree on the temporal grain of an
operation's jurisdiction, with no platform rule reconciling them.

**Ruling: both are correct, and the missing piece was never the grain — it
was the absence of a record.**

- **Resolution is per operation.** A checkpoint that carries its own
  regulatory moment (issue, activate, convert, resolve-held-disposition) is
  its own operation and resolves its own jurisdiction. Bonus's
  per-checkpoint behaviour stands.
- **An operation may FREEZE its resolution for a bounded composite
  lifecycle** — a casino round is the existing example (ADR 0031 §9,
  enforced by migration 0042's immutability trigger). Freezing is
  legitimate, is **explicit**, and is **recorded** as frozen so that
  "frozen by design" and "stale" are distinguishable in the record
  (`qa` case C-4).
- The real defect C-6 named is that a Grant issued under one jurisdiction
  and converted under another had **nothing detecting it**. AR-2 fixes
  exactly that: each checkpoint persists its own resolution reference, so
  the divergence becomes **detectable**. What the platform should *do*
  about a detected divergence is HDR-J-4's human half plus §7.3's
  mechanism — not a temporal-grain rule.

---

## 8. C-4 removal — TENSION 5 CONFIRMED SETTLED

SEC's ruling reads as clean and non-contradicted, and this synthesis
carries it forward **binding and unamended**.

### 8.1 JV-1 / JV-2 / JV-3, confirmed

- **JV-1.** Every jurisdiction value is exactly one of: **(a)** a scope
  declaration on a configuration row (staff-authored by design,
  legitimately in a request body, controlled by RBAC + FK + audit + R-2b's
  precondition); or **(b)** a resolved fact about an operation
  (**server-resolved only**). **No request body, header, query parameter or
  path segment — staff- or player-supplied — may ever carry a class (b)
  value.**
- **JV-2.** The Bonus admin `jurisdiction_code` fields are **removed**, not
  kept-and-cross-validated. SEC's five arguments are accepted; the
  decisive one is factual and free: `decodeJSON` already calls
  `DisallowUnknownFields` (re-verified at
  `internal/httpserver/json.go:14`), so deleting the field turns a
  submitted `jurisdiction_code` into a **400**, not a silent ignore.
  Removal is strictly better than cross-validation on the very axis
  cross-validation was proposed to win, at zero implementation cost.
  Cross-validation would additionally manufacture a third state
  (resolver-unavailable-but-field-present) whose "just trust what the
  operator typed" resolution is a one-line change under incident pressure
  with no security tripwire.
- **JV-3.** `createRuleRequest.JurisdictionCode`
  (`internal/httpserver/risk_handlers.go:127`), `AuthorizeScope`'s
  jurisdiction, and `casino_games.jurisdiction_blocklist` on the catalogue
  upsert are **class (a) and stay.** They acquire their own controls
  (R-2b's precondition; §9.5's before/after audit).

### 8.2 Precision, independently re-verified

SEC's correction to RECON C-4 — **four structs across five handler
surfaces**, not five structs — is **confirmed by `architect`'s own grep at
`89af4c0`**:

| # | Struct | Field | Surfaces |
|---|---|---|---|
| 1 | `issueManualGrantRequest` | `internal/httpserver/bonus_handlers.go:399` | `newIssueManualGrantHandler` **and** `newIssueManualGrantRequestHandler` (`bonus_domain_ops_handlers.go:402`) — **two endpoints, one struct** |
| 2 | `resolveHeldDispositionRequest` | `bonus_handlers.go:633` | `newResolveHeldDispositionHandler` |
| 3 | `activateManualGrantRequest` | `bonus_domain_ops_handlers.go:497` | `newActivateManualGrantHandler` |
| 4 | `executeBulkGrantJobRequest` | `bonus_domain_ops_handlers.go:719` | `newExecuteBulkGrantJobHandler` |

`bonus_domain_ops_handlers.go:546` and `:806` are **call sites**, not struct
fields. The distinction matters for execution: removing the field from
struct 1 changes **two endpoints at once**, and a plan written against
"five structs, five endpoints" mis-sequences. `qa` still writes **five**
A-1 cases, one per routed surface, because routing is what a regression
breaks.

### 8.3 Sequencing — binding

**JV-2's removal and the resolver call site land in the same change, per
handler. Never before.** SEC §S-1.4's reasoning is confirmed: the
staff-supplied field is today the **only** way a non-empty jurisdiction
reaches a gate in production (RECON P-3), so removing it first converts
"manual grant issuance works when staff supply a code" into "manual grant
issuance always denies." That is fail-closed and therefore not a security
regression, but it is an availability regression on a staff remediation
path and must be a deliberate, sequenced choice.

**`architect`'s honest addition, which changes how `bonus-engine` should
read this:** in Stage 4I the resolver will return `unresolved(no_signal)`
for every player-scoped operation (§11.3). So for Bonus, JV-2-plus-resolver
lands the *same functional outcome* as JV-2 alone — manual grant issuance
denies either way. The difference is that after JV-2-plus-resolver the
denial is **recorded, explained, and non-forgeable**, instead of being an
undocumented consequence of a deleted field. `bonus-engine` should
therefore sequence JV-2 **deliberately and visibly**, with the availability
consequence stated to the orchestrator in advance — not discover it.

### 8.4 SEC-4I-F2 — the interim control, confirmed with one tightening

SEC's interim answer is **confirmed correct**: until JV-2 lands for a given
handler, that handler's audit metadata records the staff-supplied
jurisdiction **explicitly labelled as staff-supplied** —
`"jurisdiction": {"code": "MT", "basis": "staff_supplied"}` — on
`bonus_grant.manual_issue`, the manual-activate, the held-disposition
resolve, and the bulk-execute paths.

The `basis` label is not cosmetic: it is what lets a later reader
distinguish records written before JV-2 from records written after, without
which the post-resolver audit trail is contaminated by values of unknown
provenance.

**`architect`'s tightening, binding:**

1. `staff_supplied` is a value in **the same `basis` enum the resolver
   uses** (§3.1) — not a free string invented at the audit site. One
   vocabulary, one place it is defined.
2. **`Resolve` must be structurally incapable of producing it** (§3.1).
   An interim label that the resolver could later emit would defeat its own
   purpose.
3. This is **not** a licence to keep the field. It is a control on the
   window before JV-2, and it ends when JV-2 lands for that handler.

This is the same one-line improvement RISK §3.2 recommends for
`evaluateAndAuditRisk`'s denial metadata, and both are correct
independently of Stage 4I's outcome.

---

## 9. Casino blocklist remediation (K-3) — TENSION 4, with a correction

### 9.1 The correction neither prior phase made

RISK §5.2 item 1 requires removing the `params.JurisdictionCode != nil &&`
guard at `internal/casino/orchestrator.go:138`. SEC §S-5.1 confirmed it as
written. **Applied literally, in Stage 4I, that change denies 100% of
casino launches** — because the resolver will return
`unresolved(no_signal)` for every player (§11.3), and a literal fail-closed
blocklist check denies on every unresolved jurisdiction.

SEC §S-5.2 came close: it noted that after the fix "every casino launch now
has a hard dependency on jurisdiction resolution succeeding," and argued
this is acceptable because resolver availability equals database
availability. That argument is sound **about availability** and is adopted
(§9.4) — but it assumes resolution can *succeed*. In Stage 4I it cannot,
for any player, because its only inputs are blocked on HDR-J-3.

**The fix is RISK's own §1.4 invariant, which RISK did not apply to its own
§5.2 recommendation.** §4.0's canonical wording says: *"Where a consumer can
determine that no such policy is in force, it may proceed — and must deny
the moment one is."* For the blocklist, the jurisdiction-dependent policy
is **the game's own blocklist array**, and whether one is in force is
determined **without needing a jurisdiction at all**: it is in force iff the
array is non-empty. Re-verified at migration
`0035_create_casino_integration_foundation.up.sql:50` —
`jurisdiction_blocklist TEXT[] NOT NULL DEFAULT '{}'`, so the array is
never NULL and emptiness is a clean, total test.

### 9.2 The canonical K-3 contract

```go
// CANONICAL CONTRACT — sketch for `casino` to implement properly, not
// final code. `casino` owns the implementation and the naming.
//
// The blocklist is a jurisdiction-DEPENDENT control. Per the canonical
// absent-value invariant (canonical-model §4.0), it fails closed when a
// policy is in force and it cannot be evaluated — and only then.
if len(game.JurisdictionBlocklist) > 0 {
    // A policy IS in force for this game. Jurisdiction is now required.
    if resolution.Outcome() != jurisdiction.Resolved {
        return denied(DenialCodeJurisdictionUnresolved) // internal code
    }
    if containsString(game.JurisdictionBlocklist, resolution.Code()) {
        return denied(DenialCodeJurisdictionBlocked)    // internal code
    }
}
// Empty blocklist: no jurisdiction-dependent policy is in force for this
// game, so the check is genuinely not applicable — NOT skipped-because-
// we-lack-an-input. These are different things and only the second is a
// fail-open.
```

**Why this is not a weakening of RISK's or SEC's requirement.** The
fail-open RISK and SEC correctly identified is: *a jurisdiction-dependent
control is silently not run because an input is missing.* That is fully
closed here — whenever a game carries a blocklist, an unresolved
jurisdiction **denies**. What is not done is deny for games that carry no
blocklist at all, where there is no control to run and never was.

**And it gives casino the same rollout-safety property RISK §2.1/§2.2 gives
Risk.** The blast radius becomes statically enumerable before activation,
with one query:

```sql
-- Pre-activation enumeration for K-3. `qa` asserts this, per RISK §2.4a's
-- precedent for risk_rules — an assertion, not a checklist item.
SELECT id, provider_id, provider_game_id, status, jurisdiction_blocklist
FROM   casino_games
WHERE  cardinality(jurisdiction_blocklist) > 0;
```

If that returns zero rows, fail-closing K-3 changes **no** launch outcome
for **any** tenant. RECON §5's warning that closing the producer gap would
"convert a fail-open control into a live one" is thereby made *measurable*
rather than feared.

### 9.3 The full remediation spec — six items

Synthesizing RISK §5.2's four and SEC §S-5.3/§S-5.4/§S-5.5's additions into
one list for `casino`'s phase.

| # | Requirement | Source | Binding? |
|---|---|---|---|
| **K3-1** | Replace the `params.JurisdictionCode != nil &&` guard with §9.2's contract: armed per game with a non-empty blocklist, fail-closed within it | RISK §5.2(1), **corrected** by §9.1 | Binding |
| **K3-2** | A **distinguishable internal** outcome for "could not determine jurisdiction," never reusing `ErrJurisdictionBlocked`. A player refused because the platform could not determine their jurisdiction has not been "blocked in their jurisdiction" — consistent with casino's own stated discipline at `orchestrator.go:109-112` | RISK §5.2(1), SEC §S-5.1(2) | Binding |
| **K3-3** | **One resolution, one variable, three consumers.** Resolve once above line 138 and feed the blocklist check, the `RiskRequest`, and `CreateLaunchSession` from that single value. Today line 138 dereferences `params.JurisdictionCode` while lines 166-169 derive a separate local — same value, two dereferences, only one persisted | RISK §5.2(2); required independently by §3.5 | Binding |
| **K3-4** | **Demo mode decided explicitly in code, with its reason in a comment** — never inherited from whether a line sits above or below the `params.Mode == ModeReal` branch at `:179`. `architect`'s ruling on the default: §9.6 | RISK §5.2(3), SEC §S-5.5 | Binding |
| **K3-5** | **Return-shape consistency: use `LaunchGameResult{Denied: true, DenialCode: …}`** for both jurisdiction outcomes, matching the RG and Risk gates at `orchestrator.go:155-157` / `:201-204`, rather than the `error` shape `ErrJurisdictionBlocked` uses today. RISK had no preference; `architect` chooses, because routing both through one shape is what makes §6.3's byte-identical player-facing response easy to guarantee in one place instead of two. The existing `ErrJurisdictionBlocked` HTTP mapping at `casino_handlers.go:204-206` collapses into that one place | RISK §5.2(4); `architect` decides | Binding |
| **K3-6** | **Collapse `jurisdiction_unresolved` and `jurisdiction_blocked` into one player-facing response** — same status, same message, same body. Internal distinctness preserved in full (K3-2) | SEC §S-5.3, generalized at §6.3 | Binding |

Plus one item that is correct today, independent of the resolver, and
should land with this work: **add `jurisdiction_code` and `licensing_mode`
to `evaluateAndAuditRisk`'s denial metadata**
(`internal/casino/orchestrator.go:383-392`). Today a denial by a
jurisdiction-scoped `HARD_LIMIT` produces an audit record from which the
jurisdiction cannot be recovered (RISK §3.2).

### 9.4 The availability property — binding on `backend`

SEC §S-5.2's requirement is adopted: **the Stage 4I resolver has no
external network dependency.** Its inputs are Postgres rows only
(`player_accounts`, `kyc_verifications`, `tenant_jurisdiction_configs`,
`jurisdictions`, `licences`, `tenants.licensing_model`). With that
property, resolver availability **equals** database availability, and
fail-closing K-3 adds no new failure domain: if the database is down, no
launch was going to succeed anyway. That property holds trivially today and
must be held **on purpose**, not by accident. CA-1 (§6.5) is what keeps it
true.

**Forward constraint, flagged and not decided:** the day a geolocation or
KYC vendor becomes an input to resolution (RECON Q-7), that vendor becomes
a **hard dependency of the casino launch path** — a third party can then
stop the lobby. That is a materially different operational and security
posture requiring its own decision, its own timeout/degradation design, and
its own security review. It must not arrive as an implementation detail of
a provider adapter. Recorded in §12.2 as a named future consideration.

### 9.5 SEC-4I-F3 is a HARD PREREQUISITE — confirmed and reinforced

**Confirmed: `casino_game.upserted`'s audit gap blocks fail-closing K-3.**
`internal/httpserver/casino_admin_handlers.go:92-97` writes
`Metadata: {"provider_id", "provider_game_id", "status"}` with **no
before/after state and specifically no `jurisdiction_blocklist`**, even
though `req.JurisdictionBlocklist` is written on line 87.

§9.2's contract makes this prerequisite **stronger**, not weaker. Under
§9.2 the blocklist write is *precisely the act that arms the control* for
that game: a platform admin adding one code to one game's array transitions
that game from "no jurisdiction policy in force, launches proceed" to
"jurisdiction required, unresolved denies" — which in Stage 4I means
**every** launch of that game denies, for every tenant. A control with that
blast radius that leaves no diff is not operable, and the remediation path
(restore the previous array) requires a value the platform did not keep.

**Binding: SEC-4I-F3 is fixed before K-3 goes live.** `casino_game.upserted`
records before/after for `jurisdiction_blocklist` and for the other
enforcement-relevant catalogue fields (`status`, `supported_assets`,
`demo_supported`). CLAUDE.md's audit rule already requires before/after for
a mutating administrative action; the current entry does not satisfy it.

SEC's *non-binding* recommendation — that a blocklist addition is a
denial-widening change and a reasonable candidate for the existing
`bonus_change_requests` / `asset_change_requests` dual-control pattern —
is carried forward as a **named future consideration** (§12.2), not a
Stage 4I requirement. `architect` agrees with SEC's own reasoning for not
requiring it: the platform already accepts single-actor platform-admin
catalogue writes, and CLAUDE.md's no-uncontrolled-scope rule applies to
security additions too.

### 9.6 Demo mode — `architect`'s ruling on the default

RISK §5.2(3) required `casino` to decide this rather than inherit it. SEC
§S-5.5 leaned toward applying the blocklist to demo, reasoning that
offering and advertising are regulated activities in several real regimes
independently of whether money moves.

**`architect`'s ruling, which deliberately rules on the *default and the
burden of proof* rather than on the regulatory question:**

> **Demo launches are jurisdiction-bearing by default.** The blocklist is a
> **catalogue-availability** fact — *may this title be offered in this
> market* — and the platform's answer to "do you offer this game in market
> X" must not depend on which endpoint is asked. Any exemption of the demo
> surface requires an **explicit recorded decision with a stated reason**,
> not silence and not line ordering.

`architect` deliberately does **not** assert that demo play *is* a
regulated offering in any given jurisdiction — that is a legal
interpretation, it varies by jurisdiction, and it belongs to the
permitted-markets family of questions (HDR-J-6). What is ruled is the
engineering default in the absence of that answer, and the fail-closed
default is the conservative one.

Under §9.2's contract this ruling is also **cheap**: it only bites for
games that carry a non-empty blocklist, which §9.2's enumeration makes
measurable before activation. SEC's concern that applying to demo "doubles
the availability blast radius" is therefore bounded by the same query.

`casino` retains the right to propose an amendment with reasons; that is an
`architect`-reviewable change recorded in `docs/decisions/`, not a
casino-local choice.

---

## 10. Human Decision Register — consolidated recommendation

`architect` **does not decide any of these.** What follows is a
recommendation to the orchestrator about **which to formally open and route
to the human**, with one-line reasons, consolidating IC §3, RISK §7 and
SEC §S-9.

### 10.1 Recommendation

| Item | Recommendation | One-line reason |
|---|---|---|
| **HDR-J-1** — may a missing player jurisdiction ever fall back to the tenant's/brand's? | **OPEN NOW** | It is the difference between the Bonus deposit/cashback sweeps staying fail-closed forever and issuing under an assumed jurisdiction — a compliance event if wrong, and the one item with a live, business-visible blocked path behind it |
| **HDR-J-2** — which player-side signal is legally authoritative for which operation class? | **OPEN NOW** | It is the *content* of the precedence configuration, without which the resolver has rules-engine shape and no rules; the *keying mechanism* (§3.4) is settled engineering and is buildable meanwhile |
| **HDR-J-3** — is a player residence/location/nationality field a privacy decision needing its own lawful basis and retention rule? | **OPEN NOW — highest leverage of the six** | It gates whether the resolver has **any** player-side input at all; until it is answered every player-scoped resolution is `unresolved` and Bonus stays blocked (§11.3) |
| **HDR-J-4** — what happens to obligations already in flight when a player's jurisdiction changes? | **OPEN NOW, NARROWED to the record-of-authority half** | The enforcement half is adjudicated here (§7, MROC) and needs no human input; what remains genuinely human is which jurisdiction's *record* is authoritative for a regulator-facing obligation |
| **HDR-J-5** — for a BYOL (`own_licence`) tenant, whose determination governs? | **REGISTER NOW, but mark NOT-YET-URGENT — do not route for an answer** | No BYOL tenant exists, and `tenant_asserted` being a reserved-but-unproducible `basis` (§3.1) plus `licensing_mode`-scoped platform ceilings (RISK §7) keep **both** answers open at zero cost — so this can be answered when a BYOL tenant is first contemplated, without foreclosure |
| **HDR-J-6** — which markets is the first B2C (Anjouan-licensed) brand permitted to serve? | **OPEN NOW** | `licences.permitted_markets` is empty, and a resolver that returns a jurisdiction is worthless without a permitted-market list to validate it against; CLAUDE.md already classifies this as a stop-and-ask |

**Net recommendation: formally open five (J-1, J-2, J-3, J-4-narrowed,
J-6); register J-5 with its non-foreclosure note and do not spend human
attention on it yet.**

### 10.2 Constraints on any answer, collected in one place

These were each recorded by a prior phase and are restated here so that
whoever drafts the register carries them as constraints, not as decisions:

- **On any J-1 "yes":** a fallback must be a **distinct, recorded `basis`**
  (`platform_fallback`, §3.1), must never be injected into
  `RiskRequest.JurisdictionCode` as if it were a resolved player
  jurisdiction (RISK §7 — `Rule.matches` compares a bare string and cannot
  tell the difference), and the consuming gate must be able to refuse it
  per operation class. SEC §S-9 adds the persistence half: a fallback
  indistinguishable from a resolved value, once written into an
  immutability-trigger-protected snapshot, becomes a **permanent,
  unfalsifiable record of a fact that was never established** — worse than
  no record, because it looks authoritative to a regulator.
- **On any J-2 answer:** it is configuration that changes enforcement, so
  it needs a version (`resolver_policy_version`), an audit entry on change,
  and a four-eyes posture at least as strong as `bonus_approval_policies`'.
  It must be keyed per §3.4, which is the only shape that does not smuggle
  in a global default while appearing configuration-driven.
- **On any J-3 "yes":** the attribute is **projected out of every existing
  read that does not need it** (explicit `SELECT` lists, never `SELECT *`)
  and reading it requires its own permission rather than riding on
  `player:read` (SEC §S-9). `docs/architecture/16-privacy.md` needs a
  corresponding update recording the field, its sensitivity classification
  and its access controls (IC §1). Both are implementation costs of a
  "yes" and the orchestrator should surface them **before** the decision,
  not after.
- **On any J-4 answer:** the record must show **both** jurisdictions when
  they differ (SEC §S-9) — discharged by §7.3's per-candidate resolution
  rows.
- **On any J-5 "yes":** a tenant-supplied determination is a distinct
  `basis` (`tenant_asserted`), never merged into a platform-resolved one,
  and platform-licence ceilings continue to be scoped by `licensing_mode`
  — a value resolved server-side from `tenants.licensing_model` that a
  tenant cannot influence (RISK §7, SEC §S-9, and already the documented
  discipline at `internal/risk/types.go:176-185`).
- **On any J-6 answer:** validating a resolved jurisdiction against
  `licences.permitted_markets` is an **authorization** check, is not a Risk
  rule, and must be distinguishable in its reason code from a limit breach
  and from a blocklist hit (RISK §7, SEC §S-9). A licence-scope violation
  surfacing as a generic denial is an incident nobody can triage.

### 10.3 Explicitly untouched

Per RECON §7 and every subsequent phase: G-2, the
`OpenBetSelfExclusionPolicy` default, the mixed/bonus-funded cashout
policy, FD-1, and the Grant-cancellation-after-conversion question are
**not** selected, narrowed, defaulted, or referenced as settled by this
document.

---

## 11. Migration and schema implications — buildable NOW vs. BLOCKED

This is `backend`'s work list. The split is the point of the section: a
great deal of Stage 4I is genuinely buildable with **no** human decision,
and a small, precisely-named part is not.

### 11.1 Buildable NOW — blocked by no HDR item

| # | Item | Owner | Notes |
|---|---|---|---|
| **B-1** | **`jurisdictions` registry write surface** + one new **platform-only** permission on `RolePlatformAdmin` | `backend` (role wiring subject to `security` review before landing) | RECON P-11 / §5: registry-writability is a **hard prerequisite** of any resolver — today a resolver could not return a value that satisfies any FK. The *mechanism* is independent of HDR-J-6's *content* |
| **B-2** | **`jurisdiction_resolutions` table** with the §5.2 column set, §6.1's RLS/trigger contract, composite FKs, and the two CHECK constraints (§3.2, §5.2) | `backend` | The single highest-value artefact of Stage 4I: it converts four silent absent-value behaviours into one explicit, recorded, queryable one |
| **B-3** | **The resolver package skeleton** — non-forgeable `Resolution` (§2.3), read-only `ReadOnlyQuerier` (§6.4), the 3-outcome + reason contract (§2.2), the closed `basis` enum with `staff_supplied` / `platform_fallback` structurally unproducible (§3.1), `assertTenantScope`-equivalent cross-check (§4.4 Layer 2) | `backend` | Resolves `unresolved(no_signal)` for every player-scoped operation in Stage 4I. That is the correct, honest behaviour — see §11.3 |
| **B-4** | **`tenant_licence` basis** — read `tenants.licence_id → licences.jurisdiction_id`, bounded to tenant-subject operations by a DB `CHECK` (§3.2) | `backend` | The first production read of `licences` / `tenants.licence_id` in the platform's history (RECON §10) |
| **B-5** | **Precedence-configuration table shape**, keyed `(tenant licensing jurisdiction, operation_class)` per §3.4 | `backend` | The **shape** is buildable; the **rows** are blocked on HDR-J-2 |
| **B-6** | **`jurisdiction_resolution_active (tenant_id, operation_class, …)`**, tenant-scoped, audited on change, with a narrow read-only accessor for Risk | `backend` (resolver-owned), consumed by `risk` | Supplies the fact RISK §2.4b's `CreateRule` precondition reads. Risk defines no table, flag or role for it |
| **B-7** | **R-2b's authoring-time precondition** in `risk.CreateRule` | `risk`, after `security` review | Moves the failure from evaluation time (denying real players mid-round, RISK §1.6) to authoring time (a 400 to a staff member). Structurally eliminates RISK §1.6's stuck-round hazard |
| **B-8** | **SEC-4I-F2 interim audit metadata** on the four Bonus paths, `basis: staff_supplied` per §8.4 | `bonus-engine` | Correct independently of Stage 4I's outcome |
| **B-9** | **SEC-4I-F3 fix** — before/after on `casino_game.upserted` | `casino` | **Hard prerequisite** of K-3 (§9.5) |
| **B-10** | **`evaluateAndAuditRisk` denial metadata** gains `jurisdiction_code` + `licensing_mode` | `casino` | One line, zero risk, correct today (RISK §3.2) |
| **B-11** | **Delete `bonus.resolveJurisdictionID`**; Bonus takes both representations from one `Resolution` | `bonus-engine` | Closes RISK §3.3's live latent fail-open (§2.5). Lands with B-3 |
| **B-12** | **K-3 remediation** per §9.3, after B-9 | `casino` | §9.2's contract makes its blast radius statically enumerable before activation |
| **B-13** | **`qa`'s SEC §S-6 suite** — all seven categories; plus §9.2's and RISK §2.2's enumeration assertions; plus §6.4's `READ ONLY` and `pg_locks` tests | `qa` | Mandatory for the stage gate. The subset depending on player-side signals is written against the `unresolved` path and re-run when HDR-J-3 resolves |

### 11.2 BLOCKED — each on a specific, named HDR item

| Item | Blocked on | Precisely what is blocked |
|---|---|---|
| `player_accounts.declared_residence_country` + `captured_at`; `kyc_verifications.verified_residence_country` + `source_document_id` | **HDR-J-3** | The **migration itself**. Not the schema placement (IC §1's `player_accounts`-not-`persons` answer is pre-decided and stands), and not the column shapes (IC §5's sketch). Only the act of collecting |
| Any nationality field | **HDR-J-3** + a demonstrated operation need | Both conditions, not either |
| `geo_signal` as a producible basis | **HDR-J-3** + RECON Q-7 + §9.4's forward constraint | A geolocation vendor on the launch path is a separate decision |
| Precedence-configuration **rows** | **HDR-J-2** | The table shape (B-5) is not blocked |
| `platform_fallback` as a producible basis; unblocking the Bonus sweeps | **HDR-J-1** | The enum value exists and is inert (§3.1) |
| Permitted-market **validation content**; lifting `withdrawal_policies`' `CHECK (jurisdiction_code IS NULL)` | **HDR-J-6** (content) and payments'/withdrawal's own phases (mechanism) | Deferred **explicitly**, closing RECON Q-16 |
| `tenant_asserted` as a producible basis | **HDR-J-5** | Reserved; zero-cost non-foreclosure (§3.1) |
| MROC composition machinery | **HDR-J-4**'s record-of-authority half | Only §5.2's non-foreclosure requirement applies now (§7.4) |
| `registration_channel` on `player_accounts` | **Nothing — but NOT authorized for Stage 4I** | IC §5 correctly notes it is not PII and not HDR-gated. But it has **no consumer**, and CLAUDE.md's no-uncontrolled-scope rule applies: it is recorded as a deferred future consideration (§12.2), not built |

### 11.3 The honest Stage 4I outcome — stated plainly, per CLAUDE.md

**With HDR-J-3 unanswered, the resolver has no player-side input. Every
player-scoped resolution in Stage 4I therefore returns
`unresolved(no_signal)`.**

Consequences, stated so no one discovers them:

- **Bonus remains blocked.** `CheckEligibility` layer 6 still denies
  unconditionally (§4.1), and it is right to. Stage 4I does **not** unblock
  the Bonus deposit sweep, the cashback scheduler, or manual grant
  issuance. Anyone reading "jurisdiction resolution foundation" as
  "Bonus unblocked" is reading it wrong.
- **Risk's behaviour does not change at all.** RISK §2.1's invariance proof
  is the reason: `Rule.matches` reads `req.JurisdictionCode` only inside
  `if r.JurisdictionCode != ""`, and `missingScopeContext` fires only when
  a rule scopes jurisdiction and the request does not. A resolver that
  always returns unresolved supplies the same empty string Risk already
  receives. **Strictly non-regressive, for every tenant, for every
  operation.**
- **Casino's behaviour does not change** for any game with an empty
  blocklist — which, per §9.2's enumeration, is expected to be all of them.
- **Payments and withdrawal are untouched**; their dimensions stay off.

**So what does Stage 4I actually deliver?** Precisely this: the platform
stops having *four mutually incompatible, mostly-silent* answers to
"jurisdiction is absent" and starts having **one explicit, recorded,
non-forgeable, auditable, fail-closed** one — with the registry writable,
the record queryable, the interface fixed, the RLS contract enforced, the
staff-supplied side channel removed, and the fail-open (K-3) closed. That
is a real and substantial foundation. It is **not** a jurisdiction
capability, and this document does not claim one. Per CLAUDE.md: software
capability and legal/regulatory approval are different things, and so are
*infrastructure for a determination* and *the determination itself*.

---

## 12. Residual items

### 12.1 Findings status

| Id | Status |
|---|---|
| **SEC-4I-F1** (staff-supplied `required_approvals` on held-disposition resolve, HIGH) | **Tracked separately, NOT Stage 4I scope, NOT re-analyzed here.** Confirmed still unfixed at `89af4c0` — a concurrent dispatch is handling it on this branch and `architect` deliberately did not touch those files. It does not block Stage 4I; `security` records it as launch-blocking if unresolved, and it must not be closed by Stage 4I's completion report |
| **SEC-4I-F2** (manual-grant jurisdiction metadata, MEDIUM) | Interim control **confirmed and tightened** (§8.4). Owner `bonus-engine`, item B-8 |
| **SEC-4I-F3** (`casino_game.upserted` audit gap, MEDIUM) | **Confirmed as a hard prerequisite of K-3** and reinforced by §9.2's contract (§9.5). Owner `casino`, item B-9 |

### 12.2 Named future considerations — recorded, not built

Per CLAUDE.md's no-uncontrolled-scope rule, each is recorded here rather
than built, and none is a Stage 4I requirement:

1. **A vendor input to resolution makes a third party a hard dependency of
   the casino launch path** (§9.4). Needs its own decision, timeout /
   degradation design, and security review. Must not arrive as an
   implementation detail of a provider adapter.
2. **Dual control on a blocklist addition** (§9.5) — a denial-widening
   platform-wide write. SEC explicitly declined to require it; `architect`
   agrees.
3. **`registration_channel` on `player_accounts`** (§11.2) — not PII, not
   HDR-gated, genuinely a named gap in docs 05/11, and with no consumer.
4. **A bounded Risk observability signal** — emit an observation only when
   a jurisdiction-scoped rule matched and did not breach (RISK §2.5).
   Deliberately not built; auditing ALLOW decisions would write an audit
   row on the bet hot path for every bet.
5. **A player-facing curated jurisdiction projection** (§6.1) — at most
   "your account is registered under jurisdiction X," never a passthrough
   of a resolution record.

### 12.3 Owed `architect` document corrections

Named, with their exact content, and deliberately **not** performed in this
phase except where noted:

1. **`docs/architecture/34-economic-operation-identity.md` §5.3** — the
   H-2 precondition. **DONE in this phase** (§6.4).
2. **`docs/governance/ownership.md`** — add a jurisdiction row recording
   §2.1's ownership split (interface `architect`, implementation
   `backend`, precedence content `identity-compliance`). Owed; it should
   land with `backend`'s implementation so it describes something that
   exists.
3. **`docs/architecture/15-jurisdiction-and-licensing-model.md`** —
   RECON **C-7**: doc 15 names five enforcement points for
   `TenantJurisdictionConfig`, zero of which are wired, and its
   `allowed_currencies` role is superseded by `asset_authorizations`
   (§1.3). Owed; deliberately deferred until §11.1's work lands, because
   correcting it now would document a resolver that does not exist.
4. **`docs/decisions/0037-asset-currency-registry-and-fx-conversion-
   architecture.md` open question 7** — stale: it flags the absence of a
   product axis on layer 6, but migration 0045 shipped a `product` column
   with most-specific-match semantics and `CheckEligibility` passes
   `scope.Product` through (RECON C-7, §1 concept 7). Owed as a staleness
   marker.
5. **`docs/security/security-architecture.md`** — fold SEC §S-2, §S-3 and
   §S-6 in as a numbered section (SEC's own routing request). Owed; it
   should land with or after `backend`'s implementation so the section
   describes enforced controls rather than intended ones.

### 12.4 Cross-references, for the avoidance of doubt

Where this document is silent on a detail, the prior phase document
governs: **IC** for identity-side placement and KYC evidence semantics;
**RISK** for `internal/risk`'s own contracts (R-1, R-2) and the H-1/H-3
hazards; **SEC** for the full RLS contract, the ~30-case adversarial test
specification, and the caching reversal conditions; **RECON** for the
from-code inventory. Where this document **is** explicit, it governs.

---

**Labelled status AS THIS DOCUMENT WAS WRITTEN (Phase 5, `22ac91a`), per
CLAUDE.md's no-fake-completion rule:**
**`NOT IMPLEMENTED`.** No resolver, no `jurisdiction_resolutions` table, no
registry write surface, no player-side jurisdiction signal, and no
jurisdiction value populating any of the platform's seventeen
jurisdiction-bearing schema elements existed at that commit. The
`jurisdiction_code` fields at `internal/httpserver/bonus_handlers.go:399`
/ `:633` and `bonus_domain_ops_handlers.go:497` / `:719` were unchanged and
still client-supplied; `internal/casino/orchestrator.go:138`'s blocklist
check was unchanged and still fail-open; `bonus.resolveJurisdictionID` still
collapsed empty and unknown codes; and SEC-4I-F1, SEC-4I-F2 and SEC-4I-F3
were all still open. This document was the specification those changes are
implemented against, and nothing more.

**For the status after implementation, see §13 below — that section, not
this paragraph, is the current record.**

---

## 13. Final cross-domain certification (`architect`, Stage 4I closing phase)

**Status of this section: `architect`'s architectural sign-off on Stage 4I
as a whole, written after reading the entire stage's diff and every phase
document as one integrated artefact.** It is the counterpart to
`docs/governance/wave-3-report.md` §9's closing role for Stage 4H-B1 Wave 3.
It does **not** close the stage — the final independent security/compliance
review and the orchestrator's Stage 4I Final Gate Report follow it — and it
decides no Human Decision Register item.

### 13.1 Labelled status after implementation

**`PARTIALLY IMPLEMENTED`**, and deliberately so. Specifically:

| Artefact | Status |
|---|---|
| `internal/jurisdiction` resolver, non-forgeable `Resolution`, 3-outcome + reason contract, closed `basis` enum | **IMPLEMENTED** |
| `jurisdiction_resolutions` table, RLS + append-only + TRUNCATE-deny + both CHECK constraints (migration `0071`) | **IMPLEMENTED** |
| `jurisdiction_resolution_active`, per-command policies, no DELETE (`0071` + `0072`) | **IMPLEMENTED** |
| `jurisdiction_precedence_configs` **shape** (`0071`) | **IMPLEMENTED** (shape); **rows BLOCKED on HDR-J-2** |
| `jurisdictions` / `licences` registry write surface + platform-only permission | **IMPLEMENTED** |
| `tenant_licence` basis, bounded to tenant-subject operations by DB `CHECK` **and** by a resolver-side `app.tenant_id` assertion (§13.3) | **IMPLEMENTED** |
| K-3 casino blocklist remediation (K3-1…K3-6 + the `evaluateAndAuditRisk` metadata item) | **IMPLEMENTED** |
| JV-2 field removal (4 structs / 5 surfaces) + B-11 (`resolveJurisdictionID` deleted, not wrapped) | **IMPLEMENTED** |
| SEC-4I-F1 / F2 / F3 / F4 / F5 / F6 / F7 | **CLOSED** |
| Any player-side jurisdiction signal | **NOT IMPLEMENTED — BLOCKED on HDR-J-3** |
| `Persist` wired into any production path; AR-2's `jurisdiction_resolution_id` FK | **NOT IMPLEMENTED** — §13.6, `DR-4I-BE-01` / `DR-4I-BE-02` |
| B-7 (R-2b's `risk.CreateRule` precondition) | **NOT IMPLEMENTED** — §13.6, `DR-4I-RISK-01` |
| §6.4 enforcement mechanisms 2 and 3 (`READ ONLY` test, `pg_locks` test) | **NOT IMPLEMENTED** — §13.6, `DR-4I-QA-01` |
| MROC composition machinery | **NOT BUILT, deliberately** (§7.4) — non-foreclosure requirements verified satisfied |

§11.3's honest outcome statement holds **unchanged and unqualified**: every
player-scoped resolution returns `unresolved(no_signal)`; Bonus is not
unblocked; Risk's behaviour is unchanged; casino's behaviour is unchanged
for every game with an empty blocklist. Stage 4I delivered the *foundation*
for a determination, not the determination.

### 13.2 Implementation fidelity — verified, not assumed

Each of the six Phase 5 adjudications was checked against the code that
followed, for architectural faithfulness rather than merely for a passing
build. **Five landed faithfully; one needed a correction (§13.3).**

1. **J-4 mechanism (MROC).** Correctly *not* built. The only Stage 4I
   obligation — no operation-keyed `UNIQUE` constraint, no speculative
   composition column — is satisfied by `0071`, and `0071`'s own comment
   states the reason, so a future tidier cannot add one innocently.
2. **Doc 34 §5.3 placement.** Landed in Phase 5 as a precondition. Casino's
   K-3 preserved the property: `Resolve` holds no lock, and the total order
   jurisdiction (no lock) → Risk advisory lock → EOI row lock is intact.
3. **Resolved-value shape.** Faithful. `Resolution` has unexported fields,
   no exported constructor, and `Code()`/`ID()` are genuinely unreachable
   for a non-`resolved` outcome. §2.5's three-place structural closure of
   the unresolved/unknown collapse is all three places: `refused(
   registry_unknown_code)` is reachable in the type, the FK to
   `jurisdictions (code)` plus `CHECK ((jurisdiction_code IS NOT NULL) =
   (outcome = 'resolved'))` closes persistence, and
   `bonus.resolveJurisdictionID` is **deleted**, not wrapped, exactly as
   instructed.
4. **K-3 spec.** Faithful to §9.2's *corrected* contract, including the
   part prior phases got wrong: armed per game with a non-empty blocklist,
   fail-closed within it. K3-2's distinguishable internal code, K3-3's one-
   resolution-three-consumers, K3-4's explicit demo ruling, K3-5's return
   shape and K3-6's byte-identical player-facing collapse are each present
   and each separately tested. §9.5's hard prerequisite (SEC-4I-F3) landed
   **before** K-3, in the required order.
5. **C-4 removal.** Faithful, including §8.3's sequencing rule (removal and
   resolver call site in the same change, per handler) and §8.2's "four
   structs, five surfaces" precision.
6. **Caching (CA-1).** Confirmed: no cache was built, and §9.4's
   availability property holds — the resolver's inputs are Postgres rows
   only, with no external network dependency.

### 13.3 The one finding this phase had to correct — `DR-4I-ARCH-01`, FIXED

**§4.4 Layer 2's resolver-side half was never implemented**, and it was
load-bearing rather than decorative. Every table `resolveTenantLicence`
reads — `tenants`, `licences`, `jurisdictions` — carried **no row-level
security at all** at the time this was written (**CORRECTED, Stage 4I
Phase E-SECURITY, migration 0077, ADR 0046: this is no longer true — all
three tables now carry RLS, though `tenants`/`jurisdictions` remain
deliberately read-open, so the READ-side isolation problem this paragraph
describes is unchanged in practice; see `internal/operatingmarket/
resolve.go`'s own updated comment for the current, accurate division of
labor between write-side RLS and this resolver's own `assertTenantScope`
**). So `Resolve` returned a fully `Resolved` Resolution
carrying tenant A's jurisdiction to a transaction that had only ever proven
tenant B, on the strength of a caller-supplied `Params.TenantID`. Confirmed
live by mutation test, not by inspection.

`Resolution.AssertScope` — which casino and bonus both correctly call —
cannot substitute for it: it compares the resolution's binding against the
caller's own arguments, which are the same arguments `Resolve` was given.
Both consuming domains' comments honestly say so. Only a comparison against
the **connection's proven scope** is non-tautological.

**No single earlier phase would have caught this**, which is precisely why
the stage has this closing pass: `security`'s control model predates the
code; `backend` implemented B-3 without it; `casino` and `bonus-engine`
each added the other half; and `qa`'s concurrency test observed the
underlying no-RLS fact but only as a lock-ordering safety property.

**Fixed this phase** (`assertTenantScope` in `internal/jurisdiction`, five
new tests including a real-database cross-tenant case with an
anti-inertness control). **Two corrections to this document follow from
it and are binding:**

- **§6.1's stated safety net — "a resolver query on a bare pool connection
  reads zero rows under FORCE RLS" — is FALSE for the `tenant_licence`
  path**, because those three tables had no RLS at the time this was
  written. **CORRECTED (ADR 0046): as of migration 0077 the three tables
  DO carry RLS, but the safety net is STILL false for this path** —
  `tenants`/`jurisdictions` are deliberately read-open (`USING (true)`),
  so a bare pool connection reads real rows, not zero. The required
  behaviour ("it must error, not return `unresolved`") is now real, but it
  is real because of an explicit assertion, not because of RLS. Any
  future basis that reads a read-open-by-design table inherits this
  obligation explicitly.
- **§4.4 Layer 2 is two independent checks, and a consumer implementing
  only `AssertScope` has implemented neither half usefully.** The resolver
  asserts against the connection; the consumer asserts against its own
  authenticated context. Where a consumer derives both from the same
  values, its half is defence-in-depth against future refactors — valuable,
  but not isolation.

### 13.4 The Bonus blast radius — CONFIRMED CORRECT AND INTENDED, with a precision

`bonus-engine` disclosed that deleting `resolveJurisdictionID`'s fail-open
(per §2.5's explicit "deleted, not wrapped" instruction) had a larger effect
than the dispatch anticipated, because `AssetAuthorization.CheckEligibility`
denies **unconditionally** on an unresolved jurisdiction — unlike casino's
blocklist, which is armed only per game — so essentially all Bonus grant
activation, conversion and route-to-cash now deny at the gate.

**`architect`'s ruling: this is the CORRECT, INTENDED and pre-stated
consequence of this document's own model. It is not an over-broad side
effect and it must NOT be narrowed.** Three independent grounds:

1. **§4.1 rules it explicitly and by design.** AssetAuthorization layer 6
   keeps unconditional fail-closed, with the reason given: layer 6 is
   *structurally* jurisdiction-keyed, so with no jurisdiction there is no
   lookup to perform — the question is unanswerable, not answerable-as-yes.
   §4.1 then states the consequence verbatim: *"Bonus stays blocked in
   Stage 4I."*
2. **§11.3 states it again, pre-emptively, as the headline consequence**:
   "Stage 4I does not unblock the Bonus deposit sweep, the cashback
   scheduler, or manual grant issuance. Anyone reading 'jurisdiction
   resolution foundation' as 'Bonus unblocked' is reading it wrong."
3. **Narrowing it would be indefensible.** The only mechanism that could
   narrow it is falling back to the tenant's jurisdiction for a player —
   which **is** HDR-J-1, an unanswered human decision, explicitly
   forbidden to the resolver (§3.1, §3.2) and enforced by a database
   `CHECK`. An engineering narrowing would silently answer a human
   decision in the affirmative.

**One precision worth recording, because "blast radius" overstates the
behavioural delta.** The set of code paths that now deny is indeed broad,
but the set whose *production behaviour changed* is exactly what §8.3
predicted:

- The **five admin surfaces** genuinely changed: a staff member could
  previously type a `jurisdiction_code` into a request body and pass the
  gate; now they cannot. That is JV-2's entire point and §8.3 required it
  to be sequenced deliberately and visibly, which it was.
- The **deposit sweep and cashback scheduler did not change**: both already
  passed `""`, which the deleted helper already collapsed to `uuid.Nil`,
  which layer 6 already denied. `bonus-engine`'s own commit message says
  so.
- **`ConvertGrant` has no production caller at all** outside the package —
  no HTTP handler, no scheduler — so its denial is presently moot.

So: the *state* is "essentially all Bonus checkpoints deny," the *change*
is "the staff-supplied side channel is gone," and both were specified in
advance. §11.3 needs no amendment.

### 13.5 The two test seams — CERTIFIED, with a convention and an expiry

SEC-4I-F6 (`security`) and SEC-4I-F7 (`qa`) each closed a hand-copy of
production logic that existed because Stage 4I made a fail-closed gate deny
unconditionally, putting the post-gate logic out of reach of the tests that
must exercise it. `architect` reviewed both as production-code patterns,
not merely as test fixes.

**Both are certified sound, minimal, and non-load-bearing in production:**

- **SEC-4I-F6 (`manualGrantApprovalPostGateHook`) is not a seam at all** —
  it is an ordinary extraction of a closure into a named function that both
  the production caller and the test call. No production signature changed,
  no injection point exists, nothing is parameterized. This needs no
  special sanction; it is just DRY.
- **SEC-4I-F7 (`activateGrantFunc`) is a genuine seam, and is correctly
  built.** The type is **unexported**; the exported entry points keep their
  exact signatures and **always** bind `activateGrantProd`; the only
  substituting implementation lives in a `_test.go` file and is therefore
  never compiled into the production binary. A production bypass would
  require someone writing a new bypass in production code, which this
  pattern makes no easier than it was before.

**Ruling — this is a sanctioned platform convention, with a named shape and
a named expiry.** It is recorded here rather than left as three ad-hoc
precedents, because Stage 4I created a repo-wide condition that will keep
producing the need until HDR-J-3 is answered.

> **Convention (`architect`, binding):** where a fail-closed gate denies
> unconditionally for reasons outside the code under test, the test reaches
> the post-gate logic by **injecting an unexported function-typed seam into
> a shared, unexported implementation that the production entry point also
> calls** — never by hand-copying the production body. The seam type is
> unexported; the exported entry point's signature is unchanged; production
> always binds the real implementation; the substituting implementation
> lives only in `_test.go`. A hand-copy is a defect, not an alternative:
> all three instances this stage found were hiding a real control or a
> recorded financial invariant behind a copy nothing bound to the original.

> **Expiry (binding, so this does not calcify):** these seams exist because
> a *temporary* condition makes the real gate chain unreachable. **When
> HDR-J-1/HDR-J-3 are answered and player-scoped activation can resolve,
> every use of `activateGrantForceAdapter` must be re-examined and
> re-pointed at the real gate chain wherever the test's own subject does
> not require the bypass.** They are not a permanent licence to test around
> T.1. Tracked as `DR-4I-BONUS-02`.

`security`'s deliberate HTTP-test tripwire (asserting *which* 403 is
returned, so the test fails once the four-eyes branch becomes reachable) is
the right mechanism for that expiry and should be imitated, not removed.

### 13.6 Carried debt — named, owned, and triggered

None of the following blocks Stage 4I's architectural certification. Each is
named so it cannot be lost, following this session's `DR-*` convention.

| Id | Item | Owner | Trigger / note |
|---|---|---|---|
| **`DR-4I-ARCH-01`** | Resolver-side §4.4 Layer 2 assertion | `architect` | **CLOSED this phase** (§13.3) |
| **`DR-4I-BONUS-01`** | Last hand-copied force helpers (deposit bonus / cashback) | `architect` | **CLOSED this phase** (§13.5 convention applied) |
| **`DR-4I-BE-01`** | **`jurisdiction.Persist` has ZERO production call sites.** `jurisdiction_resolutions` — §11.1's "single highest-value artefact of Stage 4I" — is never written outside tests. §5.1's "one row per resolution attempt, including failures" is therefore specification, not behaviour | `backend` + each consuming domain | **Deliberately carried, with reasons.** In Stage 4I every player-scoped resolution is `unresolved(no_signal)`, so a per-launch row would be an unbounded, player-triggerable stream of identical rows into an append-only table with no application delete path and no pruning — the same amplification hazard §5.2 forbids for `audit_log`, for less evidentiary value. **Wire it when a resolution can carry a real basis** (HDR-J-3), or earlier if a consumer needs AR-2's reference. **A future phase must not read §5.1 and assume the table is populated.** |
| **`DR-4I-BE-02`** | **AR-2's `jurisdiction_resolution_id` FK** on `casino_launch_sessions` / `bonus_grants` is not built (§1 concept 6) | `casino`, `bonus-engine` | Lands with `DR-4I-BE-01`; meaningless before it. Note the §6.4 condition it discharges is presently moot: casino resolves *inside* the guarded transaction, so the TOCTOU window the FK was to make visible does not currently exist |
| **`DR-4I-RISK-01`** | **B-7 / R-2b not implemented.** `jurisdiction.IsActive` exists, is tested and has **zero production callers**; `risk.CreateRule` still accepts a jurisdiction-scoped rule for a `(tenant, operation)` pair not recorded as resolution-active | `risk`, after `security` review | `risk` had no implementation phase in this stage's sequence. Until B-7 lands, RISK §2.4a's fixed activation order plus §2.2's enumeration remain the fallback — weaker but, per §4.2, acceptable and not a resolver blocker |
| **`DR-4I-QA-01`** | **§6.4's enforcement mechanisms 2 and 3 are absent**: no test resolves inside `BEGIN … READ ONLY`, and no test queries `pg_locks` after a resolution. Mechanism 1 (compile-time, via `ReadOnlyQuerier`) is the only one in force | `qa` | Mechanism 1 is the strongest of the three and genuinely holds, so H-2 is not unguarded. But mechanism 3 exists specifically to catch a `SELECT … FOR SHARE` that mechanisms 1 and 2 both permit, and nothing catches that today |
| **`DR-4I-QA-02`** | `DepositBonusParams.MaxQualifying`'s clamp branch is **untested** — removing the clamp fails no test in the repository. A monetary-boundary branch with no coverage | `qa`, with `ledger-finance` input | Pre-existing, found while mutation-testing `DR-4I-BONUS-01`; not introduced by Stage 4I |
| **`DR-4I-SEC-01`** | **§12.3 item 5 re-deferred**: folding SEC §S-2 / §S-3 / §S-6 into `docs/security/security-architecture.md` as a numbered section | `security` | **Explicitly re-deferred, not dropped.** Routed to the Stage 4I final independent security/compliance review, which owns that document and is reading this whole stage anyway. `architect` deliberately does not pre-empt it |
| **`DR-4I-BONUS-02`** | Test-seam expiry (§13.5) | `bonus-engine`, `qa` | Triggered by HDR-J-1/HDR-J-3 being answered |

§12.3's other three owed corrections are **DONE**: item 1 (doc 34 §5.3) in
Phase 5; item 2 (`ownership.md`'s jurisdiction row) by `backend`; items 3
(doc 15 / C-7) and 4 (ADR 0037 open question 7) by `architect` in this
phase.

### 13.7 Cross-document tensions — all closed

The six Phase 5 tensions remain closed and were re-verified against the
code (§13.2). **One new cross-document tension arose after Phase 5 and is
adjudicated here:**

**`payments`' C-3(d) determination — ACCEPTED.** §4.5 forwarded RISK §1.4's
characterization that C-3(d) is "two defects, not one." `payments`'
determination is that only one survives. **`architect` accepts it**, and
amends §4.5 accordingly: `AdapterCapability.SupportedCountries`' empty-means-
permissive default is **not** a jurisdiction-gating defect but a correctly-
designed, ADR-0022-original, currently-inert payment-rail *capability*
default; the **code-space confusion** (ISO-3166 country vs.
`jurisdictions.code`) is the real and surviving hazard, and remains binding
per §2.4 on anyone building routing dimension 2.

The reasoning is sound and was checked independently: `SupportedCountries`
has exactly one consumer — a narrowing invariant in `WriteCapability` — and
`RouteProvider` never reads it, so there is no gate whose absent-value
behaviour could fail open. §4.0's invariant does not even reach it, because
it is not a jurisdiction-dependent policy. §4.5's own remaining
requirements are untouched and confirmed honoured: routing dimension 2 is
still `TODO(jurisdiction)`, and `withdrawal_policies`' `CHECK
(jurisdiction_code IS NULL)` was not lifted.

### 13.8 Verdict

**Stage 4I is ARCHITECTURALLY SOUND and CERTIFIED by `architect`**, subject
to the final independent security/compliance review that follows this
phase. The stage delivered what §11.3 said it would and claimed nothing it
did not: the platform has stopped having four mutually incompatible,
mostly-silent answers to "jurisdiction is absent" and now has one explicit,
recorded, non-forgeable, auditable, fail-closed answer — with the registry
writable, the interface fixed, the RLS contract enforced, the staff-supplied
side channel removed, the K-3 fail-open closed, and one genuine
cross-tenant defect found and fixed in this closing pass.

It is **not** a jurisdiction capability. Per CLAUDE.md: infrastructure for a
determination and the determination itself are different things, and so are
software capability and regulatory approval. Real jurisdiction resolution
remains blocked on **HDR-J-3** above all, and no engineering work in any
later stage may route around it.

---

## 14. Stage 4I Phase C — player-jurisdiction precedence and resolution rules foundation

**Status of this section: implementation record, added after Phase C's
independent review and fix round.** Per HDR-J-2/HDR-J-3a (`docs/decisions/
0042-human-decision-response.md`), this phase built the deterministic
TECHNICAL FOUNDATION for resolving a player's own jurisdiction from
evidence — the rule engine `internal/jurisdiction.DeterminePlayerJurisdiction`
(`precedence.go`) and its supporting types (`purpose.go`, `evidence.go`,
`player_result.go`, `restriction.go`). It does **not** activate production
enforcement: `internal/jurisdiction/resolver.go` — the only resolver any
consuming domain (`casino`, `bonus`, `risk`) actually calls — carries **zero
diff** throughout this phase, verified by `git diff --stat` after
implementation, after all four independent reviews, and after the fix
round. There are zero production call sites of
`DeterminePlayerJurisdiction` or `ComposeRestrictions`.

Labelled status per CLAUDE.md's no-fake-completion rule: **`PARTIALLY
IMPLEMENTED`** (rule engine + tests: `IMPLEMENTED`; production wiring:
`NOT IMPLEMENTED`, deliberately, per this phase's own scope).

### 14.1 The canonical resolution contract — `PlayerJurisdictionResult`

`PlayerJurisdictionResult` (`player_result.go`) is this phase's answer to
§2.2's outcome-vocabulary requirement applied at player-evidence
granularity, and it deliberately does **not** collapse every non-resolution
into a generic "unknown." Non-forgeable by construction (unexported fields,
identical rationale to §2.3's `Resolution`), it distinguishes:

- **resolved** — `Outcome() == Resolved`; `Candidates()`/`PrimaryCandidate()`
  become reachable (both return `ErrNotResolved` otherwise, mirroring
  `Resolution.Code()`/`ID()`'s own non-forgeability property).
- **unresolved, no signal** (`ReasonNoSignal`) — no evidence at all was
  supplied.
- **unresolved, no applicable evidence** (`ReasonNoApplicableEvidence`) —
  evidence existed but none of it was a permissible determination for this
  `Purpose` (e.g. a location signal alone, for `PurposeIdentityDetermination`,
  which never treats location as a residence signal per HDR-J-3a).
- **unresolved, insufficient confidence** (`ReasonInsufficientConfidence`) —
  only declared residence was present; declared residence alone is
  insufficient for either live purpose (HDR-J-3b).
- **unresolved, evidence invalid** (`ReasonEvidenceInvalid`) — the
  highest-precedence applicable evidence carries a structurally invalid
  value (a non-ISO-3166 code); it is never silently demoted to a
  lower-precedence basis.
- **unresolved, location signal unusable** (`ReasonLocationSignalUnusable`)
  — the location dimension, when policy makes it required, was missing,
  stale, inconclusive, unavailable, or provider-errored.
- **conflicting evidence** — recorded via `HasDisagreement()` plus a
  `StatusDisagreed` `ConsideredEvidence` entry, never as a distinct
  `Outcome`/`Reason` value — disagreement is a recorded fact about the
  *evidence*, not a resolution failure, because verified residence still
  authoritatively resolves the operation over a disagreeing declared value
  (HDR-J-3b/c).

`ConsideredEvidence` records every basis considered and what happened to
it, with a closed `ConsideredBasisStatus` vocabulary distinguishing a
basis that lost to a higher-precedence one (`StatusRejectedLowerPrecedence`),
one this purpose's own rules say is not authoritative here
(`StatusInapplicable` — a success state, not a failure), one that was
present but structurally unusable (`StatusInvalid`), and one that could not
be evaluated at all (`StatusUnavailable`). Per canonical-model §5.3's
governing rule — "persist the DECISION and REFERENCES to evidence, never
the evidence VALUES" — `ConsideredEvidence` has **no code field at all**: a
resolved candidate's code is the decision and is exposed via `Code()`;
anything merely considered is not the decision and cannot carry one, by
type shape rather than by convention.

### 14.2 Operation/purpose taxonomy — deliberately separate from `OperationClass`

`Purpose` (`purpose.go`) is a new, real, typed enum —
`PurposeIdentityDetermination`, `PurposeMarketAccessControl`,
`PurposeHistoricalReporting` — distinguishing identity/KYC determination
from access/market-control from historical regulator-facing reporting, per
the directive's taxonomy requirement.

It is deliberately **not** merged with the pre-existing `OperationClass`
enum (`OperationPlay`/`OperationCatalogueAvailability`/
`OperationBonusIssuance`/`OperationBonusConversion`, §4.4 Layer 1). No
mapping function between the two exists anywhere in this codebase
(**PC-GAP-3**, §14.6) — which `OperationClass` values require which
`Purpose` is a legal/policy content decision (e.g. does `play` require
market-access control, identity determination, or both?), not an
engineering one, and inventing that mapping here would be exactly the kind
of legal-content guess CLAUDE.md and this phase's own directive forbid.

### 14.3 Evidence precedence — verified residence authoritative, declared insufficient alone, location never a residence substitute

`evaluateResidenceDimension` (`precedence.go`) implements the shared rule
both `Purpose`s use for the residence dimension: a structurally valid
verified residence (`BasisPlayerVerifiedResidence`, `ConfidenceVerified`)
is authoritative and is never demoted by a disagreeing or invalid declared
value sitting alongside it; a structurally invalid verified residence is
**never** silently demoted to declared residence (`ReasonEvidenceInvalid`,
not a declared-residence fallback); declared residence alone
(`BasisPlayerDeclaredResidence`, `ConfidenceDeclared`) is insufficient for
either live `Purpose` (`ReasonInsufficientConfidence`) — it is recorded,
never selected; a location signal (`BasisGeoSignal`) is **never** a
residence signal for `PurposeIdentityDetermination` — recorded as
`StatusInapplicable`, never built into a residence candidate, regardless of
how fresh or valid it is (HDR-J-3a, the specific property the directive
names first). For `PurposeMarketAccessControl`, a resolved residence
dimension may additionally carry a location-derived
`RoleAdditionalRestriction` candidate (never a `RolePrimaryDetermination`
one) — the mechanism §14.5 below describes.

### 14.4 Unresolved-state semantics and the fail-closed invariant

**No player jurisdiction may ever become a tenant jurisdiction via any
default, fallback, or error-handling path — verified structurally, not by
convention.** `TestDetermine_NeverEmitsATenantOrFallbackBasisOnAnyInput`
(`precedence_invariants_test.go`) exhaustively cross-products every
verified/declared/location/purpose/policy combination this engine accepts
and asserts no result ever carries `BasisTenantLicence`,
`BasisTenantAsserted`, or `BasisPlatformFallback` — the three tenant/
fallback-shaped values in the closed `Basis` enum (§3.1). `EvidenceSet`
itself (`evidence.go`) has **exactly three fields**
(`VerifiedResidence`/`DeclaredResidence`/`LocationSignal`), enforced by a
reflection-based tripwire test — there is no tenant, brand, or licence
input this engine could read even if it wanted to.

Two policy-gated evaluation knobs — `LocationRequirement` and
`EvaluationPolicy.MaxLocationSignalAge` — have zero values that
**deliberately fail closed rather than default permissively**:
`LocationRequirementUnset` and a `nil` `MaxLocationSignalAge` both return
`ErrPolicyUnset`, a Go error, not a silent `Unresolved` or `Resolved`
result — an unmade human/legal decision (which operations need a fresh
location check, and how strictly) must surface as a caller-visible error,
never a guessed default (PC-GAP-1/PC-GAP-2, §14.6). Every string-backed
enum this engine accepts from a caller (`LocationRequirement`,
`LocationSignalState`) is validated by an exhaustive switch with a
rejecting or normalizing default — never a permissive fallthrough. `AsOf`
is mandatory and a zero value is rejected outright (`ErrInvalidInput`),
because a zero `AsOf` would silently disable the location-freshness gate
(every signal would compute as infinitely fresh).

### 14.5 More-restrictive-outcome semantics — the composition primitive, not a policy decision

See §7.3/§7.4 above (both amended in this phase) for the canonical
severity vocabulary (`blocked > restricted > allowed`) and the honest
statement of what is and is not built: `ComposeRestrictions`
(`restriction.go`) is a real, tested, pure composition function — deriving
the winning outcome from an explicit severity-to-outcome mapping (never a
last-wins loop assignment, which would silently contradict its own
documented "no tie-break policy" guarantee) and returning **every**
contributor tied at the winning severity, never an arbitrary pick — but it
has zero production callers, because no domain yet produces the
second-candidate case it exists to compose.

### 14.6 Deferred legal/policy decisions — the PC-GAP items

Each of the following is an explicit, unset-by-default seam, never a
smuggled default. None is decided here; each names its owner, why it is
deferred, what depends on it, which future phase must close it, and its
security/regulatory impact if left open.

| ID | What is deferred | Owner | Why deferred | Depends on | Closes in | Security/regulatory impact if left open | Status after Phase D |
|---|---|---|---|---|---|---|---|
| **PC-GAP-1** | Whether a market-access operation may proceed when its location signal is unusable (missing/stale/inconclusive/unavailable/provider-errored), and under what conditions | `architect` + `identity-compliance`, with legal input | This is a legal/policy threshold (which operations require a fresh location check to proceed at all), not an engineering default. `LocationRequirementUnset` (the zero value) fails closed with `ErrPolicyUnset` rather than guessing `LocationRequired` or `LocationAdvisory` | HDR-J-3 (real player-side evidence collection); a real geolocation vendor | The phase that first wires `DeterminePlayerJurisdiction` into a production evaluation path (must supply this policy value explicitly, per operation class, before that wiring compiles into a reachable call) | **None while unset** — the seam is inert (zero production callers); a caller-supplied guess here, instead of an explicit human decision, would risk either wrongly blocking lawful play (over-strict) or wrongly permitting play from a restricted jurisdiction (under-strict, a licensing/regulatory exposure) | **Mechanism CLOSED, content BLOCKED on HDR-J-8.** `jurisdiction_precedence_configs.location_requirement` (unset/required/advisory), `ResolveEvaluationPolicy`, and the four-state fail-closed distinction (key-level unset / field-level unset / not-active / active) are built (Stage 4I Phase D, §15). Zero rows exist. |
| **PC-GAP-2** | The maximum age before a physical-location signal is considered stale (`EvaluationPolicy.MaxLocationSignalAge`) | `architect` + `identity-compliance`, with legal input | Same class of decision as PC-GAP-1 — a `nil` value fails closed with `ErrPolicyUnset` rather than guessing "no limit" (which would defeat the freshness gate entirely) or an arbitrary duration (which would be a policy value invented by engineering) | PC-GAP-1 (the requirement decision this bounds); a real geolocation vendor's actual latency/refresh characteristics | Same production-wiring phase as PC-GAP-1 | **None while unset** — same reasoning as PC-GAP-1; note also SEC-4I-C-06 (a zero, non-nil duration is a distinct footgun — see `EvaluationPolicy.MaxLocationSignalAge`'s own doc comment, `precedence.go`) | **Mechanism CLOSED, content BLOCKED on HDR-J-9.** `jurisdiction_precedence_configs.max_location_signal_age_seconds` (whole positive seconds, NULL = unset) is built, with the database and the write API both rejecting zero/negative/sub-second values (closing SEC-4I-C-06 for every config-sourced policy). No upper bound is imposed (Stage 4I Phase D, §15; residual risk named for `security`). Zero rows exist. |
| **PC-GAP-3** | The mapping from each `OperationClass` (`play`/`catalogue_availability`/`bonus_issuance`/`bonus_conversion`) to the `Purpose` it requires (identity determination, market-access control, both, or neither) | `identity-compliance`, with legal input | This is precisely the legal-content decision §14.2 explains this phase deliberately declined to invent — no code anywhere maps `OperationClass` to `Purpose` | HDR-J-3 (the same player-side evidence dependency as PC-GAP-1); the licensing/regulatory basis for which operations actually require which determination | The phase that first wires either `Purpose` into a per-`OperationClass` production check | **None while unset** — `DeterminePlayerJurisdiction` has no production callers; a guessed mapping would risk requiring identity determination where only market-access control is legally needed (unnecessary KYC friction) or the reverse (a compliance gap) | **Seam CLOSED, mapping BLOCKED on HDR-J-7.** `RequiredPurposes` (`internal/jurisdiction/operation_purpose.go`) is the single canonical owner of this mapping; all four classes return `ErrPurposeMappingUndetermined` today (Stage 4I Phase D, §15). No other code may branch on an `OperationClass` to select a `Purpose`. |
| **PC-GAP-4** | Tenant/jurisdiction-aware precedence keying — canonical-model §3.4 ("Precedence configuration — keyed on the licence side") is not yet reflected in any Phase C type. `DeterminePlayerJurisdiction` takes a single global `EvaluationPolicy`, not one keyed per tenant/licence | `architect` | Documentation-only gap, flagged by `architect`'s Phase C review: §3.4 already establishes that precedence configuration must key on the licence side once real content exists, but no `jurisdiction_precedence_configs` row shape or per-tenant policy lookup exists yet to wire this engine against — building that lookup now, with no real precedence content to populate it (HDR-J-2/HDR-J-3 both still open), would be exactly the scope expansion CLAUDE.md forbids | HDR-J-2 (precedence-configuration content); HDR-J-3 (player evidence collection, live) | The phase that gives `jurisdiction_precedence_configs` real write/read content and wires `EvaluationPolicy` construction to a per-tenant/licence lookup rather than a caller-constructed literal | **None while unset** — `EvaluationPolicy` today is always caller-constructed per call, never read from tenant configuration, so there is no cross-tenant leakage surface; the gap is purely that the eventual per-tenant policy source does not exist yet | **Lookup/config mechanism CLOSED** (schema + Go read/write API delivered: `ResolveEvaluationPolicy` — tenant-keyed input, licensing-jurisdiction-keyed storage, ADR 0043 Decision 2 — and `CreateEvaluationPolicyVersion`/`ListEvaluationPolicyVersions`, Stage 4I Phase D, §15); **production wiring NOT IMPLEMENTED** (zero callers of `ResolveEvaluationPolicy` outside this package's own tests); content still blocked on HDR-J-2. |

### 14.7 Independent review and fix round

Four independent specialist reviews (`architect` fidelity review,
`security`, `identity-compliance` for compliance/privacy, `qa`), each
without seeing the others' findings. Convergent findings (the same defect
independently found by two or more reviewers) carried the highest
confidence and were fixed first. Full findings/disposition table:
`docs/governance/task-registry.md`, "Stage 4I Phase C" section.

**Two defects were independently found by three of the four reviewers**
(architect, security, and qa each found both by different methods — direct
code reading, adversarial mutation/compile probes, and adversarial
format-verb probes respectively):

1. **Slice-aliasing non-forgeability break** (architect P1-3 / security
   SEC-4I-C-01): `Candidates()`, `ConsideredEvidence()`, and
   `ComposedRestriction.Contributors()` all returned their internal backing
   array directly. A caller could reorder `Candidates()` in place and have
   `PrimaryCandidate()` then report an additional-restriction candidate
   (e.g. a geo signal) as the primary determination — defeating exactly
   HDR-J-3a's "location never substitutes for verified residence"
   guarantee. **Fixed**: all three accessors now return `slices.Clone` of
   their internal slice.
2. **`%#v` redaction bypass** (independently found by all three of
   architect, security, and qa): `fmt`'s `%#v` verb bypasses `Stringer`
   entirely and dumps unexported struct field values in full, including
   the country code every `String()` method on these types was written to
   redact. **Fixed**: `GoString() string` (implementing `fmt.GoStringer`)
   added to `PlayerJurisdictionCode`, `Candidate`, `PlayerJurisdictionResult`,
   `ComposedRestriction`, and `AppliedRestriction`, each returning the same
   redacted shape as the type's own `String()`.

Every other P1/P2 finding (fail-open `LocationRequirement` validation, a
zero `AsOf` silently disabling the freshness gate, a dropped
`ConsideredEvidence` entry on the required-and-unusable path, and
`ReasonNoApplicableEvidence` never being emitted by the identity-purpose
path despite the market-access path already emitting it for the
symmetric case) was fixed in the same round, each with a new regression
test. All fixes and their tests are enumerated in
`docs/governance/task-registry.md`.

### 14.8 Event-time and tenant/player separation — carried, not newly built

Historical-reporting event-time semantics (a past event's jurisdiction
context must never be silently rewritten by later evidence changes) are
enforced the same way §7.5 already established for casino rounds:
`DeterminePlayerJurisdiction` unconditionally refuses
`PurposeHistoricalReporting` with `ErrHistoricalPurposeNotComputable` — a
historical-reporting jurisdiction must be read from the event-time record,
never recomputed from current evidence, and this phase adds no mechanism
that could recompute one. Player-jurisdiction / tenant-licensing-jurisdiction
/ brand-context separation is the same structural separation this whole
stage already established (§1, §3.2, §3.3): `EvidenceSet`'s three fields
carry only player-evidence-shaped values, and §14.4's exhaustive test
confirms no tenant/brand/licence-shaped `Basis` can ever result from any
input this engine accepts.

### 14.9 Verdict

**Stage 4I Phase C is ARCHITECTURALLY SOUND and CERTIFIED**, with all
P0/P1 findings from the independent review fixed and re-validated (full
build/vet/gofmt/race/integration suite green — see the Phase C completion
report). It delivered exactly what the directive asked: a deterministic
precedence rule engine and canonical resolution/evidence/purpose types,
zero production wiring, zero diff to `resolver.go`, and every genuinely
undecided legal/policy question named as an explicit, fail-closed,
unset-by-default seam (PC-GAP-1 through 4) rather than guessed. **All
activation switches remain OFF.** No production permitted-market list, no
country allow/deny content, no real geolocation vendor, and no production
jurisdiction enforcement exist as a result of this phase. Real
player-jurisdiction resolution remains blocked on **HDR-J-3** above all,
unchanged from §13.8's closing statement.

---

## 15. Stage 4I Phase D — jurisdiction policy configuration & operational semantics

Full ruling: the Stage 4I Phase D architect design ruling; full reasoning
and decisions: `docs/decisions/0043-jurisdiction-evaluation-policy-
configuration.md`. This section records the summary a reader of the
canonical model needs without re-reading either document in full.

### 15.1 What this phase closed

- **PC-GAP-4 (precedence-configuration keying), lookup/config mechanism
  CLOSED; production wiring NOT IMPLEMENTED (zero callers outside this
  package's own tests).**
  `jurisdiction_precedence_configs` (migration 0071) is widened, not
  replaced, by migration `0075_jurisdiction_evaluation_policy_config`: new
  columns `status`, `location_requirement`,
  `max_location_signal_age_seconds`, `precedence_status`,
  `precedence_policy_version`, `legal_review_reference`, `reason_code`;
  new append-only/provenance triggers; new RLS (added, hardening
  direction only — permissive read, platform-admin-only write, no DELETE,
  no FOR ALL). `internal/jurisdiction/evaluation_policy.go` adds
  `ResolveEvaluationPolicy` (tenant-keyed input, licensing-jurisdiction-
  keyed storage — §3.4's reconciliation, restated in this section's own
  amendment note above) and `ListEvaluationPolicyVersions`.
  `internal/jurisdiction/evaluation_policy_admin.go` adds
  `CreateEvaluationPolicyVersion` (authors/supersedes/withdraws a
  version; refuses to author an `active` one — `ErrActivationNotAuthorized`
  — since activation requires HDR-J-8/HDR-J-9 content plus its own
  permission and dual-control ruling, none of which exist yet).
- **PC-GAP-3 (`OperationClass` → `Purpose` mapping), seam CLOSED, mapping
  content BLOCKED on the new HDR-J-7.**
  `internal/jurisdiction/operation_purpose.go` adds `RequiredPurposes`,
  the single canonical owner of this mapping. It contains zero mapping
  content: all four operation classes (`play`,
  `catalogue_availability`, `bonus_issuance`, `bonus_conversion`) return
  `ErrPurposeMappingUndetermined`. §14.2's own consumer-behaviour argument
  ("every existing consumer makes an availability/restriction decision, so
  `Purpose` must be `PurposeMarketAccessControl` for all four") is
  explicitly rejected as invalid by the Phase D ruling: `Purpose` selects
  which evidence hierarchy governs a determination, not what kind of
  decision a consumer happens to make, and the two evidence hierarchies
  differ in whether a real-time geolocation signal may ever restrict an
  operation at all (HDR-J-3a). That is licensing content, not a fact
  recoverable from consumer code.
- **PC-GAP-1/PC-GAP-2 (location-requirement threshold and staleness
  bound), mechanism CLOSED, content BLOCKED on the new HDR-J-8/HDR-J-9.**
  The four-state fail-closed distinction — location-required,
  location-advisory, field-level policy-unset (a row exists, the field is
  explicitly `unset`), key-level policy-not-configured (no row at all),
  and policy-not-active (a row exists and is in force but is `draft` or
  `withdrawn`) — is real and tested, and every non-value state (key-level
  unset, not-active, mismatched-version, unknown-jurisdiction) fails
  closed as a distinguishable error. **Correction:** a nil error from
  `ResolveEvaluationPolicy` does NOT mean the policy's fields are all
  decided — a field-level `unset` value (`LocationRequirementUnset` with a
  nil `MaxLocationSignalAge`, which is exactly the zero `EvaluationPolicy`)
  is returned WITH a nil error BY DESIGN, so it is distinguishable from the
  key-level `ErrPolicyNotConfigured`, and fails closed only downstream, at
  `DeterminePlayerJurisdiction`, via `ErrPolicyUnset`. No caller may treat
  a nil error from `ResolveEvaluationPolicy` as license to skip feeding the
  result through `DeterminePlayerJurisdiction`. No permanent global
  default exists anywhere in this mechanism — no package-level default
  `EvaluationPolicy`, no env var, no config-file fallback.

### 15.2 What this phase deliberately did NOT do

- **No policy content for any real jurisdiction.** Zero rows are inserted
  by migration 0075. No seed, no fixture outside tests, no example
  jurisdiction.
- **No HTTP route, no OpenAPI change, no console/back-office surface.**
  A `draft` MAY carry real location/staleness content authored ahead of
  legal review (`CreateEvaluationPolicyVersion` accepts it; only
  `withdrawn` is forced content-free) — but no HTTP authoring route exists
  in this phase because that content can never be activated
  (`ErrActivationNotAuthorized` refuses it at the sanctioned Go write
  path) until HDR-J-8/HDR-J-9 are answered and a future phase adds the
  activation permission and dual-control mechanism; shipping an authoring
  endpoint for content that cannot yet be approved is a needless surface.
- **No activation writer, and no database-level guard against one either
  — an application-layer control, not a structural one.** Writing
  `status = 'active'` is refused by `CreateEvaluationPolicyVersion`
  (`ErrActivationNotAuthorized`), but migration 0075's own CHECK
  constraint and RLS `INSERT` policy both admit an `active` row from any
  platform-admin-scoped writer — verified directly by `code-reviewer`'s
  Phase D review. The database representation exists so a future phase
  adds a writer, not a migration; that future phase must not assume the
  schema itself blocks activation.
- **No change to `resolver.go`, `precedence.go`, or `types.go` (zero diff,
  verified by `git diff --stat`).** `purpose.go` carries a doc-comment-only
  diff (one paragraph, replacing the sentence that predated
  `RequiredPurposes`'s existence).
- **No coupling to `jurisdiction_resolution_active` or
  `jurisdiction_evidence_collection_active`.** `ResolveEvaluationPolicy`
  reads neither. The future wiring phase must require all three switches
  independently (§3.4's amendment note above; ADR 0043 Decision 6).
- **No widening of `jurisdiction_resolutions.reason`'s CHECK** (Correction
  2 in the Phase D ruling) — nothing persists a `PlayerJurisdictionResult`
  yet, so widening it now would be shape without a writer. Named as a
  prerequisite of the future phase that first calls `Persist` on a
  player-jurisdiction determination.

### 15.3 New human decision register items

`docs/decisions/0044-human-decision-register-stage-4i-phase-d.md` registers
three new, open items — **HDR-J-7** (which of the four operation classes
require identity determination, market-access control, both, or neither),
**HDR-J-8** (location-requirement threshold, per licensing jurisdiction and
operation class), and **HDR-J-9** (location-staleness bound, per licensing
jurisdiction and operation class). None of the six items in
`docs/decisions/0041-human-decision-register-stage-4i-jurisdiction.md` /
`docs/decisions/0042-human-decision-response.md` is restated, altered, or
reopened by this phase.

### 15.4 Status and independent review record

Per CLAUDE.md's no-fake-completion rule, this section states Phase D's
factual labels directly rather than a self-declared verdict written before
independent review ran (an earlier revision of this section stated
"CERTIFIED... with full build/vet/gofmt/race/integration coverage" — that
was the implementer's own claim, made before any of the five reviews below
ran, and some of its coverage claims were not yet accurate at the time it
was written, e.g. the concurrency test's own flakiness and
`ListEvaluationPolicyVersions`' missing coverage, both closed by the fix
round recorded below).

**Labels:**
- Config schema + Go read/write API + mapping seam: `IMPLEMENTED`.
- Any actual jurisdiction policy content (location requirement, staleness
  bound, operation-class → Purpose mapping): `BLOCKED` on
  HDR-J-7/HDR-J-8/HDR-J-9.
- Production wiring of any of this into an enforcement path:
  `NOT IMPLEMENTED`, deliberately.

**Independent review record** (five reviews ran against the initial Phase D
implementation; a fix round closed most of what they raised; a focused
security re-verification of that fix round then found one of the fixes
itself still incomplete; a second, targeted fix replaced the
scheduling-dependent test with a genuinely deterministic one, independently
re-run by the orchestrator directly against a live database — 30 repeat
runs with no failure, plus the whole-repo integration suite green):
- `architect` — original review BLOCKED on one P1 (a concurrency
  integration test that did not reliably force the race it claimed to
  test — found independently by `architect`, `security`, and `qa`) plus
  four P2s. The fix round closed the four P2s. A first attempted fix for
  the P1 (a `sync.WaitGroup` barrier) was found by `security`'s
  re-verification to still fail intermittently under CPU contention,
  because the barrier synchronized only transaction start, not the actual
  write race. **Now CLOSED**: the test was replaced with (a) a loosened
  invariant-only regression test
  (`TestCreateEvaluationPolicyVersion_ConcurrentCreatesNeverCorruptState`)
  asserting only the properties that hold under every legitimate
  scheduling, and (b) a genuinely deterministic test
  (`TestCreateEvaluationPolicyVersion_DeterministicConflictViaUncommittedCompetingRow`)
  that forces the race via real PostgreSQL unique-index locking semantics
  (an uncommitted competing row blocks the real call's INSERT; a
  `pg_stat_activity` poll — not a sleep — confirms the block before
  release), removing the timing assumption entirely rather than narrowing
  it. Independently re-run by the orchestrator: 30 consecutive passes
  (`-race`, `-count=15` × 2 tests) with no failure, plus a clean whole-repo
  `go test -tags=integration ./...` run.
- `security` — CERTIFIED WITH NAMED EXCEPTIONS on its original review (no
  P0/P1). Its follow-up re-verification of the first fix round confirmed
  five of six fixes solid (the append-only-trigger hardening, the
  down-migration guard, `ListEvaluationPolicyVersions`' scope assertion,
  the 23514 error mapping, and the corrected doc claims) and reproduced
  the then-still-open concurrency-test defect, explicitly not blocking on
  security grounds (no data-integrity or authorization impact — a real
  race between two concurrent authors can only ever produce a correct
  supersession or a correct `ErrConcurrentPolicyWrite`, never corruption)
  but naming the defect's persistence as a no-fake-completion issue for
  `architect` to adjudicate. The deterministic replacement test above
  uses exactly the locking-based technique `security`'s own report
  proposed.
- `identity-compliance` — NO VIOLATIONS FOUND.
- `qa` — original review NOT READY, citing the same concurrency-test
  defect independently (the third of three independent reviewers to find
  it by different methods). Its other P2/P3 findings were closed by the
  fix round; the concurrency-test item is closed by the deterministic
  replacement test above.
- `risk` — NO INTEGRATION CONCERNS.

A pre-existing, unrelated test outside this phase's own files
(`internal/bonus/wave3_phase2_migrations_integration_test.go`'s
`TestWave3Phase2Migrations_FullChainUpDownUpRoundTrip`) hardcodes the
migration chain's most-recently-applied window by count; migration 0075
landing on the chain's tip required extending that window by one
(mirroring the identical, already-established pattern each of migrations
0071/0072/0073/0074 required in turn) — updated and re-verified passing,
per this project's standing convention for that test.

No jurisdiction policy content exists for any real jurisdiction as a
result of this phase, and none is claimed. Real player-jurisdiction
resolution and any per-operation-class enforcement remain blocked on
**HDR-J-3** (collection at all) jointly with the three new items this
phase registers (**HDR-J-7/HDR-J-8/HDR-J-9**), unchanged in direction from
§13.8's and §14.9's closing statements.
