# 32 — Affiliate and Acquisition Engine Architecture

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No Go code, no
schema, no migration, no route, no tracking pixel, no commission
calculation, no settlement is authorized by this document. Produced in
Stage 4H-B1, Wave 1.5 (Architecture Reconciliation Gate), directive §F.
Authored by `architect` under the roster adaptation recorded in
`docs/governance/task-registry.md` (no `affiliate` specialist exists;
same precedent as Gamification doc 17, Retail doc 26 and ADR 0037), with
`code-reviewer` as the independent architectural reviewer.

Numbering: next free after `31`. This document takes **32**.

## 0. Scope anchor — stated honestly

- The Blueprint **does** require affiliate capability:
  `01-requirements-inventory.md` lists "Affiliate attribution capture and
  FTD/deposit/NGR postbacks" as a functional requirement, and
  `13-dependency-map-and-risk-register.md` names third-party affiliate
  platforms (Income Access, MyAffiliates) as the assumed vendor class.
- The Blueprint's own bias, recorded in `02-domain-and-service-boundaries.md`
  and `14-mvp-scope-and-roadmap.md`, is **third-party first; in-house
  rebuild is a year-two consideration** (Blueprint §1 table).
- The Wave 1.5 **human directive** elevates Affiliate/Acquisition to a
  first-class architectural domain and requires this document.

As with CRM (doc 31 §0), these are not in conflict and this document does
not overturn the buy-first posture. It defines the boundary, the data
ownership, the attribution-evidence rules and the financial contract —
which is exactly what a bought affiliate platform must plug into rather
than replace. **Buy-vs-build remains an OPEN DECISION for the human**
(§12, OI-AFF-1); this architecture is compatible with both, and §6's
financial contract is the part that must exist either way, because a
vendor computing commission is not a vendor authorized to move our money.

### 0.1 Authority boundaries observed

Does not override `ledger-finance` on financial invariants (and
explicitly declines to design the commission posting shape — §7),
`security` on security requirements, `identity-compliance` on identity/
privacy/consent, `risk` on Risk, `bonus-engine` on bonus mechanics, or
`data-analytics` on reporting. Selects no Human Decision Register item
(G-2, `OpenBetSelfExclusionPolicy`, cashout policy, FD-1).

---

## 1. The boundary

> **Affiliate decides WHO INTRODUCED WHOM, and WHAT COMMISSION IS OWED
> under which versioned rule. It never moves money, never touches a
> player's balance, never gates play, and never grants a bonus.**

| Affiliate owns | Affiliate never owns |
|---|---|
| Affiliate/partner records and their hierarchy position | The hierarchy primitive itself (§3 — reused, not rebuilt) |
| Affiliate users and their scoped access | A new identity model, principal type or actor type |
| Tracking links, promo codes (as *attribution tokens*, §5.4), clicks | Bonus value, coupons that grant value, or any Offer's terms |
| The immutable attribution evidence chain and the attribution *decision* | Player identity, KYC state, RG state, jurisdiction |
| Commission **rules** (versioned configuration) and commission **accrual** (a claim) | Commission **settlement** — the money movement (`ledger-finance`, §7) |
| Affiliate-facing reporting scoped to its own subtree | The reporting/BI pipeline (`data-analytics`, doc 12) or any monetary figure computed outside the ledger |
| Sub-affiliate override rules | The wallet, the ledger, bonus accounting, Risk, RG |

### 1.1 The invariant that makes all of this safe

**AFF-1 — Affiliate is a read-and-record domain on the player side.** It
records facts *about* acquisition (click, attribution) and *claims*
against revenue (accrual). It writes nothing into any player-owned table,
posts nothing to the ledger, and cannot cause a player-facing effect
except indirectly, through a domain that decides for itself (Segmentation
→ CRM → Bonus, §8).

This is ADR 0035's retail precedent applied one domain over: Retail "emits
the *reason* for a movement and the *authorization* for it;
`ledger-finance` owns every account, posting and monetary invariant."
Affiliate emits the reason (an approved accrual) and the authorization (an
approval record); `ledger-finance` owns the posting.

---

## 2. Domain model

Every object carries doc 10 **W1**'s common object contract (UUID
identity; `tenant_id` from authenticated server context only; explicit
status enum; `clock_timestamp()`-sourced immutable `created_at`;
`actor_type`/`actor_id`; audit correlation; DB-enforced idempotency where
externally triggerable; append-only history).

```
  Partner structure
    Affiliate(node ref) · AffiliateUser · AffiliateAgreement(+Version)
  Acquisition
    TrackingLink · PromoCode · Click · ReferralSignal
  Attribution
    AttributionModel(+Version) · AttributionCandidate · PlayerAttribution
  Commercials
    CommissionRule(+Version) · CommissionPeriod · CommissionAccrual ·
    CommissionAdjustment · CommissionApproval · CommissionPayable
  Surfaces
    AffiliateReport (read model)
```

---

## 3. The hierarchy — reuse `internal/agentnetwork`, do not rebuild

**Decision, with the justification the directive asks for.**

`26-retail-operations-architecture.md` §7.1 already made the naming
decision for exactly this case, in its own words:

> "the hierarchy is not inherently a retail concept. A B2B sub-operator
> tree, **an affiliate/sub-affiliate structure**, and a white-label
> reseller chain are all the same graph with different node types. Naming
> it `retail` would guarantee that the second consumer either duplicates
> it or imports a package whose name lies."

`docs/governance/ownership.md` records the same reasoning verbatim on the
`internal/agentnetwork` row. **Affiliate is that predicted second
consumer.** Building `affiliate_nodes` + its own closure table would
falsify the prediction the platform already committed to in writing.

**Therefore:**

- The affiliate/sub-affiliate tree is **node instances in
  `internal/agentnetwork`**, with node *types* (`affiliate`,
  `sub_affiliate`, `network`, …) as **configuration rows**, never enum
  values, Go types, constraints or permissions — doc 26's "the hierarchy
  is configuration, never a code path" rule, unchanged. A different
  tenant's affiliate network is a different set of rows with zero code
  change.
- Node type/relation/capability **definitions** are dual-scope
  (`tenant_id IS NULL` = platform-wide template), mirroring `risk_rules`
  (ADR 0031 §3); every node **instance** is `tenant_id NOT NULL`.
- **`internal/affiliate`** is the operational surface — tracking,
  attribution, commission rules/accrual/approval, affiliate reporting —
  composing `agentnetwork` + `identity` + `segment` + ledger-derived
  reads. The split mirrors doc 26's `agentnetwork`/`retail` split exactly
  and for the same reason.
- Subtree scoping (an affiliate sees only its own subtree) uses the
  **same** node-subtree RLS dimension doc 26 §8 already flags as
  P1 `architect` + `security` work (`app.hierarchy_node_id` +
  `WithNodeScope`, including the permissive-policy OR-widening split
  ADR 0019 documents for `app.player_account_id`). Affiliate does not
  invent a second scoping mechanism.

### 3.1 The honest cost of this decision

`internal/agentnetwork` **does not exist.** Retail is architecture-only
and unbuilt. So reuse creates a real build-order dependency:

- Either the hierarchy primitive is built by whichever of {Retail,
  Affiliate} is authorized first, and the second consumes it;
- or Affiliate's first slice is restricted to a **flat** affiliate list
  (no sub-affiliates), which needs no closure table, with the hierarchy
  arriving when the primitive does.

The second option is the lower-risk sequencing and is this document's
`RECOMMENDATION` if Affiliate is ever authorized before Retail —
sub-affiliate commission (§6.4) is the only capability it defers, and
deferring it costs a migration, not a redesign. **Neither is authorized
here**; recorded for the human's stage sequencing in doc 33 §4 and as
OI-AFF-2.

### 3.2 Affiliate users — no new principal type

An affiliate user authenticates as an `identity.StaffUser` with an
affiliate-specific role, scoped to its node subtree — **not** a new
`auth.PrincipalType` and **not** a new `audit.ActorType`. This mirrors
doc 26's identical decision for cashiers ("a cashier is an
`identity.StaffUser` with a retail role — **not** a new actor or
principal type, so `audit.ActorType` and `auth.PrincipalType` are
unchanged").

**But the security question is materially different and I flag it rather
than wave it through**: a cashier is *our* staff; an affiliate is an
*external commercial counterparty* authenticating into the platform's
staff principal space. That deserves `security`'s explicit judgment on
whether subtree-scoped RLS plus a narrow role set is sufficient isolation,
or whether a distinct principal type is warranted. Routed as
**DEP-AFF-1**; `architect` proposes the reuse, `security` decides.

An affiliate is never a `Person`/`PlayerAccount`, and a player is never a
node (doc 26 §1.6's rule) — a player attaches to a node by reference,
through `PlayerAttribution` (§5.3).

---

## 4. Agreements and commission rules — versioned configuration

- **`AffiliateAgreement`** (+ immutable `AffiliateAgreementVersion`): the
  commercial relationship — the affiliate node, effective window, the
  currency/asset of settlement, the attribution model reference, the
  commission rule set reference, negative-carryover policy, minimum
  payout threshold, and the agreement's status.
- **`CommissionRule`** (+ immutable `CommissionRuleVersion`): the
  calculation configuration. Supported model classes (each a
  configuration shape, never a code branch per affiliate):
  `cpa` (per qualifying acquisition), `revenue_share` (a % of a canonical
  revenue measure), `hybrid`, `tiered` (bands over a measured quantity),
  `sub_affiliate_override` (§6.4).
- **Immutable once referenced.** Once any accrual references a rule
  version, that version is frozen forever — doc 10 §1.1's Offer-version
  rule and W2.1's Campaign-version rule, applied to commercial terms. A
  renegotiation is a new version with an effective date; it never
  retroactively rewrites an accrual already computed.
- **Scope**: rules are `tenant_id NOT NULL`, optionally narrowed by
  brand, product, jurisdiction and asset. Nothing affiliate-specific is a
  code path (`CLAUDE.md`).

---

## 5. The attribution lifecycle and its immutable evidence

This is the section the directive weights most heavily, because
attribution is where money later gets decided and where a dispute is
later litigated.

### 5.1 Stage 1 — Click (or equivalent inbound signal)

A `Click` row is **append-only and immutable**, written before any
identity exists:

| Field | Rule |
|---|---|
| `id`, `tenant_id`, `brand_id` | tenant/brand resolved **server-side** from the tracking link's own registration, never from a query parameter (`CLAUDE.md`: never trust a client-supplied tenant id) |
| `tracking_link_id` / `promo_code_id` | resolved from a registered link/code; an unregistered value is recorded as `unattributed` with the raw token, never silently attributed to anyone |
| `affiliate_node_id`, `agreement_version_id` | resolved server-side at click time and **frozen on the row** |
| `tracking_token` | **server-minted and integrity-protected** (an HMAC/signed value over `(tenant_id, link_id, issued_at, nonce)` with a server-held key). The client is given an opaque token; it never supplies affiliate identity directly at conversion time |
| `occurred_at` | `clock_timestamp()`, server clock only |
| `landing_context` | destination path/campaign parameters, size-bounded, no free-form injection into later queries |
| `jurisdiction_hint`, `device_class` | coarse, non-identifying |
| `ip_address`, `user_agent` | **PII** — see §5.6. Retained under doc 16's (unresolved) retention regime, minimized/truncated where the attribution model does not require full fidelity |
| `consent_state` | the tracking/cookie consent state at click time, by reference (§5.6) |

No `UPDATE`, no `DELETE` — enforced by trigger, mirroring `audit_log`'s
`audit_log_deny_mutation` (ADR 0013), not by application discipline.

### 5.2 Stage 2 — Attribution candidate

At registration (and at any later qualifying event the model uses), the
platform resolves **candidates**: the set of clicks/codes that the
attribution model considers, each with its evidence reference. An
`AttributionCandidate` row is append-only and records *what was
considered*, not merely what won — a model that only stores the winner
cannot answer "why not the other affiliate", which is the exact question
a commercial dispute asks.

Binding: the affiliate is resolved from the **server-side token**, not
from a client-supplied affiliate id. A client may present the opaque
`tracking_token` it was given; the platform verifies the signature and
looks up the click. A forged or expired token yields `unattributed`,
never a fallback attribution.

### 5.3 Stage 3 — `PlayerAttribution`, the frozen decision

The attribution decision is computed **at the qualifying event** using
the evidence as-of that instant, and the **result is snapshotted** —
structurally the same discipline as doc 30 §SEG-1 (resolve at decision
time, snapshot the result) and doc 10 T.2's eligibility snapshot:

| Field | Note |
|---|---|
| `id`, `tenant_id`, `brand_id`, `player_account_id` | |
| `affiliate_node_id` | the winner |
| `attribution_model_id` + `attribution_model_version_id` | **by reference** (immutable version) — doc 30 §7.1's EDR-R1 |
| `evidence_refs[]` | the candidate/click ids considered |
| `decided_at`, `decided_by` (`actor_type=system`), `reason_code` | |
| `qualifying_event_ref` | the canonical event/ledger transaction that triggered the decision |
| `status` | `active` \| `superseded` |

**Immutable.** A re-attribution is a **new row superseding the old**,
carrying `supersedes_attribution_id`, a mandatory reason code, the
authorizing actor, and — above a configurable materiality threshold —
four-eyes approval (ADR 0024's withdrawal-approval precedent,
`CLAUDE.md`'s manual-adjustment rule). The original is never edited or
deleted. This is the compensating-entry discipline the ledger uses,
applied to a commercial fact that determines money.

### 5.4 Promo codes are attribution tokens, not a second coupon system

`bonus-engine` owns coupons (doc 29 BC-06; doc 10 W4 "Coded bonuses").
An affiliate promo code must not become a parallel value-granting
mechanism.

**Decision:** an affiliate `PromoCode` is an **attribution token** that
resolves to `(affiliate_node_id, agreement_version)` and *may* carry an
optional **reference** to a Bonus Offer. Entering it records attribution;
**any bonus value it implies is granted only by the Bonus Engine, through
Bonus's own coupon/redemption path, under Bonus's own full gate**
(`AssetAuthorization` → RG → Risk, doc 10 §T.1). Affiliate supplies no
amount, no terms, no eligibility override.

Code-space collisions between affiliate promo codes and bonus coupon
codes are prevented by a single namespace check at authoring time, owned
by `bonus-engine` (the domain that already owns code redemption). Filed
as **DEP-AFF-3**. Doc 29 §5.1's enumeration/rate-limit discipline for the
coupon endpoint (`security`-owned, OI-9) applies to any affiliate code
entry surface unchanged.

### 5.5 Attribution model — versioned configuration, never a code path

Model classes: `last_click`, `first_click`, `last_click_within_window`,
`multi_touch(weights)`, `code_overrides_click`, `manual`. Each with a
configured attribution window, a tie-break rule, and a declared
precedence between a code and a click.

- The model is **tenant/brand-scoped configuration**, versioned, with the
  applied version recorded on every decision (§5.3).
- The model is **deterministic**: same evidence + same model version =
  same decision. No randomness, no wall-clock-dependent tie-break; ties
  break on a declared, recorded rule (e.g. earliest `occurred_at`, then
  lowest click id).
- **Replay** is possible for any past decision because the evidence
  (§5.1/§5.2) is immutable and the model version is immutable. Where a
  model input is *not* immutable, the decision snapshot (§5.3) is the
  authoritative reconstruction — the same honest limit doc 30 §6 records
  for dynamic segments.

### 5.6 Tamper-resistance, privacy, and what is deliberately not built

**Tamper-resistance mechanisms, in order of load-bearingness:**

1. Server-minted, integrity-protected `tracking_token` — a client cannot
   assert an affiliate identity (§5.1).
2. Server-side tenant/brand/affiliate resolution — never a query
   parameter (`CLAUDE.md`).
3. Append-only clicks/candidates/attributions with DB-level mutation
   denial (ADR 0013's trigger pattern).
4. Supersede-never-edit for re-attribution, with reason code, actor and
   four-eyes above a threshold (§5.3).
5. Every mutation of a link, code, agreement, rule, accrual or attribution
   writes an `audit.Record` in the same transaction (`CLAUDE.md`;
   ADR 0013).
6. Idempotency on click and conversion ingestion, DB-enforced, never
   check-then-insert.

**Deliberately NOT built** (`CLAUDE.md`'s no-scope-expansion rule): a
cryptographic hash chain or Merkle log over click records. Append-only
tables plus the immutable `audit_log` are the platform's established
tamper-evidence mechanism and are consistent with how far this project
has gone for *financial* records; a bespoke chain for a pre-identity
marketing record would be a heavier control than the ledger itself
carries. Recorded as a conscious decision, not an omission.

**Privacy (routed to `identity-compliance`/`security`, DEP-AFF-2):**
click tracking collects IP and user agent before any account exists, and
it is the first place in this platform where a *tracking-consent*
question (cookies/identifiers) arises. Doc 16 has no model for it and
doc 31 §8.1 already establishes that consent is `identity-compliance`'s
to own. Affiliate must not invent a second consent concept; it records a
`consent_state` **reference** and fails closed where the model requires
consent it does not have. Retention of click PII inherits doc 16's
unresolved retention-period question (a legal decision, not an
engineering one).

---

## 6. Commission — calculation as a claim, never as money

### 6.1 The revenue measure is not Affiliate's to define

**AFF-3, binding:** a revenue-share commission is computed from a
**canonical, ledger-derived revenue measure** (NGR/GGR as defined by
`ledger-finance` and produced through `data-analytics`'s ledger-derived
models, doc 12). Affiliate **never** computes its own revenue figure from
its own tables, and no `affiliate_*` table maintains a running revenue or
liability counter.

This is doc 29 §5.2's rule for campaign reporting ("every figure is
derived from `ledger_entries`, never from a Bonus-owned balance column")
and doc 26's "Retail owns no money", applied here. The failure being
prevented is concrete and common in this industry: an affiliate platform
whose NGR disagrees with the operator's ledger, with no way to say which
is right.

What "NGR" means for commission (which deductions: bonus cost, provider
fees, PSP fees, chargebacks, tax) is a **financial and commercial
definition**, owned by `ledger-finance` (definition) and the human
(commercial policy) — not invented here. Routed as **DEP-AFF-4**.

### 6.2 `CommissionAccrual` — a claim, with its inputs

Per `(affiliate_node, CommissionPeriod, rule_version, asset)`:
append-only, immutable once written:

| Field | Rule |
|---|---|
| `commission_rule_version_id`, `agreement_version_id`, `attribution_model_version_id` | EDR-R1 references (immutable) |
| `input_refs[]` + input values with their `as_of` | the ledger-derived measures used, by reference **and** by value (EDR-R1: copy the mutable with its as-of), so the accrual is reconstructable even if a report definition later changes |
| `asset_code`, `amount` | **integer minor units** against the Asset Registry's exponent, `NUMERIC(38,0)`-compatible decimal representation over any wire (ADR 0021; doc 21's exponent-18 correction — never `int64`). **Never floating point** |
| `rounding_rule_id` | the applied `rounding_rules` row (ADR 0021 DS-1/DS-2: round once, at the final monetary boundary, via the one shared function, never an implicit cast) |
| `computed_at`, `computed_by` | `actor_type = system` |
| `status` | `draft` → `pending_approval` → `approved` \| `rejected` → `settled` \| `carried_forward` |

A recomputation never edits an accrual; it writes a
`CommissionAdjustment` (a compensating record, `CLAUDE.md`'s corrections
rule applied one domain up from the ledger).

### 6.3 Negative commission, clawback, and carryover — flagged, not invented

Voided deposits, chargebacks, bonus abuse and player-fraud reversals can
make a period's commission negative. Three candidate policies exist
(carry the negative forward within the affiliate's own future periods;
claw back already-settled money; absorb it), and choosing between them is
**a commercial decision with a financial posting consequence** — not an
engineering judgment call.

- `architect` does **not** select one.
- The *mechanism* this architecture supports without prejudging the
  policy: negative `CommissionAdjustment` records plus an
  agreement-version-level `negative_carryover_policy` field.
- Clawback of already-settled money is a `ledger-finance` posting
  question and a human commercial question. Routed as **OI-AFF-3** and
  **DEP-AFF-4**.

### 6.4 Sub-affiliate override

A sub-affiliate override is a commission rule whose measured quantity is
a *descendant node's* accrual, resolved through the hierarchy closure
(§3). Two guards:

- **Depth and total-payout bounds are configuration**, validated at rule
  authoring time, so a deep or mis-built tree cannot produce an unbounded
  or >100% total payout. (The check is Affiliate's own authoring
  validation, not a Risk rule — it caps a *commercial term*, not a
  player-facing value movement; the doc 31 §8.4 line applies in the same
  shape.)
- **No cycles** — guaranteed by `agentnetwork`'s own acyclicity
  invariant, not re-implemented here.

### 6.5 Approval

`CommissionApproval`: staff review with a mandatory reason code, and
**four-eyes above a configurable threshold** — the identical control
`CLAUDE.md` mandates for manual balance adjustments and ADR 0024
established for withdrawals. The approver must be a distinct, resolved,
active person from the requester (the Stage 4H-B0-R6 P1: the four-eyes
self-approval check was inert because no code path could set `person_id`
on the approving account — any affiliate four-eyes control must not
repeat that, and must be verified by a literal self-approval-rejection
test, doc 29 BC-18's precedent).

---

## 7. Commission settlement — the contract handed to `ledger-finance`

**`architect` does not design the posting shape.** This mirrors exactly
how ADR 0035 handled retail: `ledger-finance` owned `agent_float`
accounting while the hierarchy/operational architecture was
`architect`'s, and doc 26 records the rule that if the two ever disagree
on monetary treatment, **ADR 0035 wins.** The same precedence applies
here: if this document and `ledger-finance`'s eventual commission ADR
disagree on anything monetary, **`ledger-finance` wins.**

What `architect` specifies is the **trigger and the data contract**:

```
CommissionSettlementInstruction            ← Affiliate hands this to Ledger
  instruction_id            (idempotency key; DB-unique, never check-then-insert)
  tenant_id                 (server-resolved)
  affiliate_node_id
  period_id                 (CommissionPeriod)
  asset_code                (Asset Registry code)
  amount                    (integer minor units, decimal-string wire form)
  rounding_rule_id          (ADR 0021)
  accrual_refs[]            (the approved accruals being settled)
  adjustment_refs[]         (negative/compensating records applied)
  approval_refs[]           (CommissionApproval rows, incl. four-eyes)
  commission_rule_version_id, agreement_version_id
  reason_code
  requested_by              (actor_type/actor_id)
  requested_at              (clock_timestamp())
```

And the **five rules that bind whatever posting shape `ledger-finance`
chooses**, each inherited rather than invented:

1. **Affiliate never calls `ledger.Post` itself**, and `internal/affiliate`
   never imports `internal/ledger`/`internal/wallet` (doc 02's "Only
   `wallet` may write ledger tables"; doc 26's identical rule for
   retail).
2. **Never a player account.** A commission settlement moves value
   between platform/affiliate-side accounts. It must never debit or
   credit `player_cash`, `player_bonus`, `player_locked_cash` or
   `player_locked_bonus`, and must never appear in a player's balance
   projection. (An affiliate who is *also* a player is two distinct
   entities; the platform must not net them.)
3. **Double-entry, append-only, idempotent** by DB constraint on
   `instruction_id` (or its composition into the existing
   `(provider_id, provider_tx_id)` / `(tenant_id, idempotency_key)`
   routing, ADR 0038 §14/§14.6 — `ledger-finance`'s call).
4. **Compensating entries only** for corrections; never an edit of a
   settled posting.
5. **Whether an approved-but-unsettled accrual is a recognized liability
   on the balance sheet** is `ledger-finance`'s determination (the same
   class of question as ADR 0032 §2's open `bonus_expense` presentation
   decision), not this document's.

Routed as **DEP-AFF-4** (§12). Payout *rails* (how money actually leaves
— bank transfer, PSP payout, crypto) are `payments`', not Affiliate's,
and are out of scope here.

---

## 8. The full chain: Affiliate → Acquisition → Segment → CRM Journey → Bonus Offer → Bonus Grant

The directive asks exactly how this works **without coupling Affiliate to
financial posting.** Step by step, with what crosses each boundary:

| # | Step | What crosses | What does NOT cross |
|---|---|---|---|
| 1 | **Affiliate → Acquisition** | A click is recorded (§5.1); on registration/first qualifying event, `PlayerAttribution` is written (§5.3) — a *fact about origin*, tenant-scoped, server-resolved | No player balance, no bonus, no eligibility effect. Attribution does not authorize anything |
| 2 | **Acquisition → Segmentation** | Segmentation reads the attribution projection as criterion **C-21** (doc 30 §4) — "acquired via affiliate node subtree X", "acquired in the last 30 days" — through Affiliate's read interface, never its tables | Segmentation does not compute attribution, does not re-run the model, and does not store a copy. C-21 is `BLOCKED` until this domain exists |
| 3 | **Segmentation → CRM** | An `Audience` include/exclude term resolves via `segment.Resolve`, returning an `Evaluation` (result + `segment_version_id` + `criteria_hash` + `as_of`), which CRM records (doc 31 §5) | A segment result is not an authorization (doc 30 §8.1). CRM builds no criteria of its own |
| 4 | **CRM Journey → Bonus Offer** | `bonus.RequestOfferGrant` (doc 31 §7.2): an `offer_id` + `offer_version_id` **reference**, the player, an idempotency key, the trigger reference, and the audience evidence | **No amount, no wagering terms, no eligibility override.** CRM cannot grant, and cannot retry with altered parameters to get a different answer |
| 5 | **Bonus Offer → Bonus Grant** | Bonus runs its unmodified gate — `AssetAuthorization.CheckEligibility(operation = wagering)` → `rg.EvaluateEligibility` → `risk.Evaluate`, in the same transaction as the effect, all fail-closed (doc 10 §T.1; doc 29 §4.2, §8 BI-7) — then issues, or denies | Affiliate has **no** presence in this step. No affiliate identity, node id, code or commission term appears in the gate, the Grant, or the posting |
| 6 | **Grant → Ledger** | `ledger.Post` under ADR 0032's posting map, with the eligibility decision record (doc 30 §7) written in the same transaction | Nothing affiliate-owned. A Grant is not a commission event |
| 7 | **Ledger → revenue measure → Commission** (the *parallel*, decoupled path) | Commission accrual (§6.2) reads a canonical **ledger-derived** revenue measure for the period; the player's attribution (§5.3) determines *which affiliate* the revenue is attributed to | The commission path is **triggered by period close and ledger-derived revenue, never by a Grant, a CRM send, or a journey step.** Affiliate never observes an individual Grant's terms |
| 8 | **Commission → settlement** | §7's `CommissionSettlementInstruction`, handed to `ledger-finance` | Affiliate never posts, never touches a player account, never computes the posting shape |

**The decoupling, stated as an invariant — AFF-2:** the acquisition chain
(steps 1–6) and the commission chain (steps 7–8) share exactly one thing:
the immutable `PlayerAttribution` row. Neither chain calls the other.
Removing the entire commission subsystem would not change a single
player-facing outcome in steps 1–6; removing CRM would not change a
single commission figure.

---

## 9. Integration with the other domains

| Domain | Affiliate's relationship | Never |
|---|---|---|
| **CRM** (doc 31) | Attribution feeds audiences (via Segmentation); CRM's `crm.journey.entered`/`crm.communication.sent` events may be recorded as engagement touches for multi-touch models | Affiliate never triggers a CRM send, never reads a player's messages, never reads CRM's profile projection for commercial terms |
| **Segmentation** (doc 30) | Provides C-21 as a read | Affiliate does not define segments |
| **Bonus** (doc 10) | Promo code may *reference* an Offer (§5.4); bonus **cost** is an input to a revenue measure `ledger-finance` defines | Affiliate never grants, configures, or overrides a bonus; never sees Offer terms |
| **Gamification** (interface-only, doc 17) | If Gamification ever exists, its events are ordinary engagement touches for multi-touch attribution, consumed through the canonical taxonomy | No `affiliate_*` table carries a point, level, badge, streak, mission or tournament column (doc 29 §6.6's rule) |
| **Reporting** (doc 12) | Affiliate report *definitions* are Affiliate's; the pipeline is `data-analytics`'s; every monetary figure is ledger-derived with `ledger-finance` review | Affiliate builds no CDC/warehouse/BI component and publishes no monetary figure it computed itself |
| **Ledger** (ADR 0019/0032/0035) | §7's instruction contract | No posting, no import, no player account, no balance |
| **Identity** (ADR 0027) | Players are ordinary `Person`+`PlayerAccount`; affiliate users are `StaffUser`s with affiliate roles (§3.2) | No second identity model, no player creation path of its own |
| **Risk** (ADR 0031) | Affiliate-fraud *player-side* controls (self-referral, bonus abuse) are Risk rules and `bonus-engine`'s abuse detector — not affiliate-local heuristics | Affiliate has no limit, cap, counter, velocity or player-risk concept of its own (doc 30 §8.3's third-occurrence rule) |
| **RG / KYC** | Consumed only as context in reporting-safe, aggregate form | Affiliate never gates play, never sees RG/KYC detail, never influences either decision |
| **Payments** | Settlement payout rails | Affiliate does not orchestrate payouts |

### 9.1 Self-referral and affiliate fraud — named, routed, not solved here

A player who is also an affiliate self-referring for CPA is the canonical
affiliate-fraud vector, and it sits precisely at the intersection of
`risk` (player-side limits), `bonus-engine` (abuse detection, doc 10
§1.4, and its device/payment-fingerprint-linking detector) and
`identity-compliance` (person resolution, ADR 0027). **It is not an
affiliate-local heuristic**, and Affiliate must not grow one (that would
be the fourth occurrence of the "second risk engine" failure doc 02
already records twice and doc 30 §8.3 a third time).

What Affiliate contributes: the immutable evidence (§5) those domains
evaluate, plus the ability to mark an attribution `superseded` with a
reason code after their determination. Routed as **OI-AFF-4**.

---

## 10. Affiliate-facing surface and privacy

- **Subtree scoping is structural**, via §3's node-subtree RLS dimension
  plus server-side authorization — never a query filter the application
  remembers to apply (`CLAUDE.md`: isolation is enforced by RLS, not by
  discipline in application code).
- **No player PII.** An affiliate report exposes aggregates and, where a
  per-player line is commercially necessary (FTD confirmation), a
  **pseudonymous player reference** — never email, name, date of birth,
  document data, IP, payment instrument, or exact balance. This is
  doc 16's minimization posture applied to an *external* reader, which is
  the strictest case in the platform.
- **Postbacks** (the Blueprint's "FTD/deposit/NGR postbacks") are an
  outbound integration behind an adapter, with per-affiliate credentials,
  DB-enforced idempotency, retry/state machine, and the same minimization
  rule. A postback carries a pseudonymous reference and an event class —
  never PII, never a raw ledger row.
- **No affiliate-facing write path** touches any player-owned table.

---

## 11. Invariants (for `qa` and `code-reviewer`; mechanically checkable)

| ID | Invariant | Check |
|---|---|---|
| **AI-1** | `internal/affiliate` never imports `internal/ledger` or `internal/wallet` | Import inspection |
| **AI-2** | No `affiliate_*` table holds a player balance, and no settlement instruction names a player ledger account type | Schema inspection + a test asserting rejection |
| **AI-3** | Clicks, attribution candidates, `PlayerAttribution` and accruals are append-only (no `UPDATE`/`DELETE`), enforced by trigger | Trigger/constraint inspection (ADR 0013's pattern) |
| **AI-4** | Affiliate identity at conversion is resolved from a server-minted, signature-verified token; a forged/expired token yields `unattributed`, never a fallback attribution | Adversarial test: forged token, replayed token, tampered payload |
| **AI-5** | Attribution is deterministic: same evidence + same model version = same decision, including tie-breaks | Property test |
| **AI-6** | Re-attribution supersedes, never edits; requires reason code and four-eyes above threshold, with a genuine self-approval rejection (the Stage 4H-B0-R6 P1 shape) | Test incl. literal self-approval attempt |
| **AI-7** | No commission figure is computed from an affiliate-owned counter; every revenue input traces to a ledger-derived measure | Code review + reconciliation test (accrual inputs vs. ledger) |
| **AI-8** | No floating-point money anywhere; amounts are integer minor units against the registry exponent, rounded once via the shared `rounding_rules` function | Type inspection + multi-exponent test (0/2/6/8/18) |
| **AI-9** | Every `affiliate_*` table has `FORCE ROW LEVEL SECURITY`, node-subtree scoping, and the `app.player_account_id IS NULL` conjunct on staff-scope policies | Schema inspection + cross-tenant/cross-subtree RLS tests |
| **AI-10** | No affiliate-facing response contains player PII | Response-shape review + test |
| **AI-11** | `internal/affiliate` contains no limit/cap/counter/velocity/risk-score concept | Code review |
| **AI-12** | No affiliate node type, relation or capability appears as a Go enum, table constraint or permission string — all are configuration rows | grep (doc 26's rule) |
| **AI-13** | Affiliate builds no second hierarchy/closure table; the tree is `agentnetwork`'s | Schema inspection |
| **AI-14** | Every mutation of a link, code, agreement, rule, accrual, approval or attribution writes an `audit.Record` in the same transaction | Count-matching test |

---

## 12. Open items and routed dependencies

| ID | Item | Owner | Blocking? |
|---|---|---|---|
| **DEP-AFF-1** | Affiliate users as subtree-scoped `StaffUser`s vs. a distinct principal type — an *external* counterparty in the staff principal space (§3.2). `architect` proposes reuse; `security` decides | security | Yes, before implementation |
| **DEP-AFF-2** | Tracking/cookie consent and click-PII retention (§5.6). No consent model exists (doc 31 §8.1 / DEP-CRM-1); Affiliate must not invent a second one | identity-compliance (+ human on retention) | Yes, before any click is recorded |
| **DEP-AFF-3** | Promo-code namespace collision with Bonus coupons; single authoring-time check owned by `bonus-engine` (§5.4) | bonus-engine | Yes, for codes |
| **DEP-AFF-4** | **Commission settlement posting shape, the canonical revenue (NGR/GGR) definition for commission, liability recognition for approved-unsettled accruals, and clawback treatment** — all `ledger-finance`'s, per the ADR 0035 precedent (§6.1, §6.3, §7) | ledger-finance (+ human on commercial policy) | **Yes** — no settlement without it |
| **DEP-AFF-5** | Node-subtree RLS dimension (`app.hierarchy_node_id` + `WithNodeScope`) is already flagged P1 in doc 26 §8 for `architect` + `security`; Affiliate is its second consumer | architect + security | Yes, for subtree scoping |
| **OI-AFF-1** | **Buy vs. build** the affiliate platform (Blueprint §1: third-party first). This architecture is compatible with both; §7's financial contract is required either way | Orchestrator → human | No |
| **OI-AFF-2** | Build-order: `internal/agentnetwork` does not exist (§3.1). Either it is built first, or Affiliate's first slice is flat (no sub-affiliates) | Orchestrator → human | No |
| **OI-AFF-3** | Negative-commission policy: carry forward / claw back / absorb. Commercial decision with a financial consequence; deliberately not selected (§6.3) | Orchestrator → human | No |
| **OI-AFF-4** | Self-referral/affiliate fraud belongs to `risk` + `bonus-engine` + `identity-compliance`, not to an affiliate-local heuristic (§9.1) | risk + bonus-engine + identity-compliance | No |
| **OI-AFF-5** | doc 22 amendments for affiliate events — see §12.1 | Master Orchestrator (doc 22's owner) | Yes, before any producer |

### 12.1 Required doc 22 amendments — routed, not applied

Same pattern as doc 31 §14.1: recorded here, applied by doc 22's owner.

**Consumed — no change required.** `identity.person.registered`,
`payments.deposit.settled` (FTD and ongoing), `payments.withdrawal.settled`,
`casino.*`/`sportsbook.*` (revenue-relevant activity), `bonus.grant.*`
(bonus cost as a revenue-measure input), `crm.journey.entered`/
`crm.communication.sent` (engagement touches for multi-touch models).

**Produced — two genuinely new affiliate-owned types**, each with a named
consumer:

| Proposed type | Fires when | Why needed |
|---|---|---|
| `affiliate.player.attributed` | A `PlayerAttribution` becomes `active` (including a supersede, which carries `reverses_ref` to the superseded attribution — doc 22's field, used exactly as specified) | Consumed by Segmentation (C-21 invalidation), CRM (acquisition-source journeys) and reporting. Without it, every consumer must poll Affiliate's tables, which no domain may read |
| `affiliate.commission.approved` | A `CommissionApproval` completes (including four-eyes where required) | The trigger boundary between the commission domain and `ledger-finance`'s settlement path (§7). Carries the `instruction_id`, never the posting |

**Deliberately NOT proposed**: `affiliate.click.recorded` (very high
volume, pre-identity, PII-bearing, no cross-domain consumer — it belongs
in Affiliate's own store and, if ever needed, `data-analytics`'s
pipeline; doc 22 already excludes clickstream for exactly this reason);
`affiliate.commission.calculated` (an intermediate computation, not a
business fact — `approved` is the fact that matters);
`affiliate.commission.settled` (that is a **ledger** fact, and publishing
an affiliate-sourced duplicate of it would violate doc 22's
one-producer-per-fact rule).

---

## 13. What Affiliate does NOT build — restated

No wallet, no ledger, no balance, no posting, no bonus grant or coupon
value, no eligibility gate, no Risk/RG/KYC model, no player risk score,
no second identity model, no second hierarchy/closure table, no consent
store, no segmentation engine, no CRM journey engine, no BI pipeline, no
cryptographic chain ledger, no payout rail — and no implementation of
anything in this document. `internal/affiliate` does not exist and is not
authorized to exist by this document.

## 14. Cross-references

- Hierarchy primitive and its naming rationale: `26-retail-operations-architecture.md` §7.1, §8; `docs/governance/ownership.md`
- Retail financial-ownership precedent (the model for §7): `docs/decisions/0035-retail-agent-network-accounting.md`
- Retail RBAC/RLS/audit precedent: `docs/decisions/0036-…`
- Segmentation (C-21, EDR-R1): `30-segmentation-engine-architecture.md`
- CRM (steps 3–4 of §8): `31-crm-engine-architecture.md`
- Flow diagrams and build order: `33-cross-domain-commercial-flow-map.md`
- Bonus gate and coupon ownership: `10-bonus-engine-architecture.md` §T.1, W4; `29-bonus-implementation-contract.md` §5.1
- Rounding/precision: `docs/decisions/0021-multi-asset-accounting.md`
- Idempotency routing: `docs/decisions/0038-…` §14, §14.6
- Four-eyes precedent: `docs/decisions/0024-…`
- Event taxonomy: `22-canonical-activity-event-taxonomy.md`
- Reporting ownership: `12-audit-reporting-architecture.md`
- Privacy: `16-privacy.md`
- Requirements source: `01-requirements-inventory.md`; `13-dependency-map-and-risk-register.md`
