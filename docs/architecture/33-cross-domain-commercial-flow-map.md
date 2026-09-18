# 33 — Canonical Cross-Domain Flow Map, Non-Duplication Register, and Dependency Graph

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** Nothing in this
document authorizes building, sequencing, or starting any domain. The
build-order graph in §4 exists so the human can make stage-sequencing
decisions with the dependencies visible; it **does not** authorize a
stage transition (only the human can, per `CLAUDE.md`'s stage-gate rule).
Produced in Stage 4H-B1, Wave 1.5, directive §G. Owned by `architect`.

Numbering: next free after `32`. This document takes **33**.

## 0. How to read this document

Three parts, each answering a different question:

- **§1/§2 — the two flow diagrams** the gate directive names. For every
  arrow: what crosses the boundary (a **call**, an **event**, or a
  **read**), in which transaction, and — equally binding — **what does
  not cross**.
- **§3 — the non-duplication register.** Per domain, explicitly: it does
  not duplicate wallet truth, ledger truth, Risk, RG, KYC, identity, or
  bonus accounting.
- **§4 — the dependency graph and build-order implications**, including
  the honest answers to "can X be built before Y."

Legend used throughout:

| Symbol | Meaning |
|---|---|
| `──call──▶` | A synchronous in-process call. Where it gates or causes a value movement, it happens **inside the caller's `pgx.Tx`**, in the same transaction as the effect |
| `··event··▶` | A canonical event (doc 22 envelope). **No transport exists today** — §4.1 |
| `──read──▶` | A read of another domain's data **through that domain's interface**, never its tables |
| `═══✗═══` | A boundary that must never be crossed |

---

## 1. Flow (a) — Activity → Segmentation → CRM → Bonus → Gamification → Reward Orchestrator → Casino/Sportsbook/Payments → Ledger

### 1.1 The directive's chain, drawn as it actually is

The directive names this as a linear chain. Two segments of it are **not
linear**, and drawing them as a chain would encode two boundary errors
this platform has already explicitly rejected. The corrected topology:

```
  ┌──────────────────────────────────────────────────────────────────┐
  │ PRODUCERS OF FACT                                                │
  │  identity · kyc · payments · casino · sportsbook · rg · risk ·   │
  │  bonus · gamification · crm · affiliate                          │
  └───────────────┬──────────────────────────────────────────────────┘
                  │ (1) ··event··▶  canonical Activity/Event taxonomy (doc 22)
                  ▼
        ┌───────────────────┐
        │  Activity/Event   │
        └─────────┬─────────┘
                  │ (2) ··event··▶ (trigger only)
                  ▼
        ┌───────────────────┐        (3) ──read──▶  owning domains
        │   Segmentation    │◀────────────────────  (ledger, kyc, rg,
        │  internal/segment │                        identity, casino,
        └─────────┬─────────┘                        affiliate, crm…)
                  │ (4) ──call──▶ segment.Resolve → Evaluation (evidence, not authority)
                  ▼
        ┌───────────────────┐
        │       CRM         │ (5) ··event··▶ journey triggers
        │  internal/crm     │ (6) send-gate chain: consent→pref→suppress→RG→freq→juris
        └─────────┬─────────┘
                  │ (7) ──call──▶ bonus.RequestOfferGrant(offer_id, offer_version_id, …)
                  ▼
        ┌───────────────────┐
        │   Bonus Engine    │ (8) GATE: AssetAuthorization → RG → Risk  (fail-closed)
        │  internal/bonus   │
        └─────────┬─────────┘
                  │ (9) ──call──▶ RewardFulfiller.Fulfil(RewardDecision)
                  │                                    ▲
        ┌───────────────────┐                          │ (9') ──call──▶
        │  Gamification     │──────────────────────────┘   (a SIBLING producer,
        │ (NOT IMPLEMENTED) │                               not downstream of Bonus)
        └───────────────────┘
                  ▼
        ┌───────────────────┐
        │ Reward Orchestr.  │ (10) UNCONDITIONAL RG re-check inside the fulfilment lock
        └───┬───────┬───────┘
            │       │ (11a) ──call──▶ casino / sportsbook  (free spins / free bets)
            │       │ (11b) ──call──▶ external reward provider (zero ledger entries
            │       │                  while destination = inside_provider)
            │ (11c) ──call──▶ ledger.Post   (cash_credit / bonus_credit)
            ▼
        ┌───────────────────┐
        │      Ledger       │ (12) double-entry, append-only, idempotent, RLS
        └─────────┬─────────┘
                  │ (13) ··event··▶ back to (1) — the loop closes
                  └──────────────────────────────────────────────▶
```

### 1.2 The two corrections to the linear reading

**Correction A — Gamification is a sibling of Bonus, not a stage after
it.** `02-domain-and-service-boundaries.md` states it directly
("Gamification is a sibling of the Bonus Engine, not a layer of it… A
gamification reward that happens to be bonus-shaped is fulfilled by the
Bonus Engine *via* the Reward Orchestrator, never by Gamification calling
the Bonus Engine directly"), and `29-bonus-implementation-contract.md`
§6.6 restates it as a B1 requirement. Bonus and Gamification both emit
`RewardDecision`s into the Orchestrator — a **fan-in**. Drawn as a chain,
step (9) would become "Bonus calls Gamification," which is the exact
coupling both documents forbid.

**Correction B — Payments is a producer, not a fulfilment target.**
Doc 21's fulfilment-mechanism table contains casino/sportsbook (free
spins/free bets), the ledger (cash/bonus credit), the points ledger, the
gamification state store, and the external reward provider. **Payments is
not a fulfilment mechanism.** Payments' role in this flow is at step (1),
producing `payments.deposit.settled` — the trigger for a deposit-match
bonus. A reward is never "fulfilled through payments"; a *withdrawal* of
already-converted cash later goes through payments as an ordinary
withdrawal, gated by its own policy, and is not part of this chain.

### 1.3 Arrow-by-arrow contract

| # | Arrow | Kind | What crosses | What does NOT cross |
|---|---|---|---|---|
| 1 | producer → Activity/Event | event | doc 22's envelope: type, source, tenant, brand, person/player **reference**, `occurred_at`/`recorded_at`, `is_real_money`, `funding_source`, `correlation_id`, `reverses_ref`, `operation_ref`, `asset_code`/`amount_minor_units`, `idempotency_key`, `schema_version`, versioned payload | **No provider payload, no raw DB row, no identity evidence, no PII, no KYC document, no credential.** An event never itself triggers a ledger posting (doc 22) |
| 2 | Activity/Event → Segmentation | event | Nothing today. Segmentation is **pull, not push**: it evaluates on demand at `as_of`. Events matter to segmentation only as cache-invalidation signals for consumers, and no cache exists (doc 30 §6) | No membership materialization, no streaming membership |
| 3 | Segmentation → owning domains | read | A bounded, side-effect-free read per leaf predicate, through the owning domain's interface, with one `as_of` for the whole evaluation | **No table reads**, no writes, no locks, no cross-player reads, no FX conversion (doc 30 §4.2, §5.2) |
| 4 | Segmentation → CRM / Bonus / Gamification | call | `segment.Resolve` → `Evaluation{result, reason_code, segment_version_id, criteria_hash, evaluated_as_of, evaluator_version}` | **Not an authorization.** `member` is worth nothing at a gate (doc 30 §8.1/SEG-3). A DENY is never overridden by membership (SEG-4) |
| 5 | Activity/Event → CRM | event | Journey triggers + conversion-goal anchors | No authoritative status. `rg.status.changed` triggers a re-check; it is never the RG answer (doc 22 consumer contract item 3) |
| 6 | CRM internal send gate | call | consent → channel preference → suppression → `rg.EvaluateEligibility` → frequency → jurisdiction → channel adapter; every denial recorded with a reason code | No send on absent consent (fail-closed). No cached RG answer. No vendor-side enforcement |
| 7 | CRM → Bonus | call | `OfferGrantRequest`: `offer_id` + `offer_version_id` **reference**, player, tenant/brand (server-resolved), idempotency key, trigger reference, audience evidence, optional experiment variant | **No amount, no %, no wagering multiplier, no max-cashout, no expiry, no forfeiture rule, no eligibility override, no retry-with-altered-parameters** (doc 31 §7.2) |
| 8 | Bonus internal gate | call | `assetregistry.CheckEligibility(operation = wagering)` → `rg.EvaluateEligibility` → `risk.Evaluate`, in that fixed order, in the same `pgx.Tx` as the effect, all fail-closed (doc 10 §T.1; doc 29 §4.2, §8 BI-7) | No segment result in the chain. No CRM influence. No affiliate influence. No cached decision |
| 9 / 9' | Bonus → Orchestrator; Gamification → Orchestrator | call | `RewardDecision` (doc 21): `decision_id`, scope, `source_domain`, `reward_type`, amount as a decimal string (never `int64`), asset, jurisdiction/licensing mode, fulfilment destination | The Orchestrator never decides **whether** a reward is earned. Bonus never calls Gamification and vice versa |
| 10 | Orchestrator internal | call | **Unconditional** `rg.EvaluateEligibility` re-check inside the `(tenant_id, decision_id)` fulfilment lock, immediately before crediting (doc 21's corrected P1 rule — no "is the delay non-trivial" judgment) | No `risk.Evaluate` re-run (exposure was fixed at decision time). No new time-window comparison written for the purpose |
| 11a | Orchestrator → casino/sportsbook | call | A free-round/free-bet fulfilment request | **Not built** — `CasinoProvider` has no free-round method; `internal/sportsbook` does not exist (doc 21; doc 29 §6.1) |
| 11b | Orchestrator → external reward provider | call | `RequestReward` | **Zero ledger entries** while the declared destination is `inside_provider` (ADR 0032 §6(c)) |
| 11c | Orchestrator → Ledger | call | `ledger.Post` with the split instruction it received | The Orchestrator **never invents accounting treatment** (doc 21) |
| 12 | Ledger internal | — | Double-entry, append-only, DB-enforced idempotency, `SUM(DEBITS)==SUM(CREDITS)`, balances as projections | No balance `UPDATE`, ever. No cache on the path (`CLAUDE.md`) |
| 13 | Ledger → Activity/Event | event | Monetary facts re-enter the taxonomy (`casino.*`, `payments.*`, `bonus.*`), closing the loop | The event is a *fact about* a movement that already happened; it never causes one |

---

## 2. Flow (b) — Affiliate → Attribution → Segmentation → CRM → Bonus → Gamification → Reporting → Commission settlement

### 2.1 The corrected topology — two chains, one shared fact

```
   ACQUISITION CHAIN (player-facing)
   ─────────────────────────────────
   (A1) inbound click / promo code
          │  server-minted signed tracking_token; tenant/brand server-resolved
          ▼
   ┌──────────────┐
   │  Affiliate   │ (A2) Click (append-only, immutable)
   │ internal/    │ (A3) AttributionCandidate set recorded (what was CONSIDERED)
   │  affiliate   │ (A4) PlayerAttribution decided at the qualifying event,
   └──────┬───────┘       model version pinned, result FROZEN
          │ (A5) ──read──▶ (criterion C-21 only)
          ▼
   ┌──────────────┐  (A6) Resolve → Evaluation
   │ Segmentation │──────────────────────────────▶┌──────────────┐
   └──────────────┘                               │     CRM      │
                                                  └──────┬───────┘
                                          (A7) ──call──▶ │
                                                         ▼
                                                  ┌──────────────┐
                                                  │    Bonus     │ full gate
                                                  └──────┬───────┘
                                                         │ (A8) RewardDecision
                       ┌──────────────┐                  ▼
                       │ Gamification │────────▶ ┌──────────────────┐
                       │(NOT IMPL.)   │ sibling  │ Reward Orchestr. │
                       └──────────────┘          └────────┬─────────┘
                                                          ▼
                                                  ┌──────────────┐
                                                  │    Ledger    │
                                                  └──────┬───────┘
                                                         │
   COMMISSION CHAIN (back-office, decoupled)             │ (B1) ledger-derived
   ──────────────────────────────────────────            │      revenue measure
                                                         ▼
                                        ┌─────────────────────────────┐
                                        │ data-analytics / Reporting  │ doc 12
                                        └──────────────┬──────────────┘
                                                       │ (B2) ──read──▶ canonical NGR/GGR
                                                       ▼
                                        ┌─────────────────────────────┐
                                        │ Affiliate: CommissionAccrual│ (a CLAIM)
                                        │  + Adjustment + Approval    │  four-eyes
                                        └──────────────┬──────────────┘
                                                       │ (B3) CommissionSettlementInstruction
                                                       ▼
                                        ┌─────────────────────────────┐
                                        │ ledger-finance: settlement  │ posting shape
                                        │  posting (DEP-AFF-4)        │ is THEIRS
                                        └─────────────────────────────┘

   The ONLY thing the two chains share is the immutable PlayerAttribution row.
   Neither chain calls the other.  ═══✗═══  (invariant AFF-2, doc 32 §8)
```

### 2.2 Arrow-by-arrow contract

| # | Arrow | Kind | What crosses | What does NOT cross |
|---|---|---|---|---|
| A1–A2 | inbound → Affiliate | call | A click record: server-resolved tenant/brand/affiliate, server-minted signed token, `clock_timestamp()`, landing context, consent-state reference | **No client-supplied affiliate id, tenant id or brand id.** A forged/expired token ⇒ `unattributed`, never a fallback (doc 32 AI-4) |
| A3–A4 | Affiliate internal | call | Candidates + the frozen `PlayerAttribution` (model version by reference, result by value) | No player balance touched, no bonus granted, no eligibility effect. Attribution authorizes nothing (doc 32 AFF-1) |
| A5 | Affiliate → Segmentation | read | Criterion C-21: "acquired via node subtree X", "acquired within N days" — through Affiliate's read interface | Segmentation never re-runs the attribution model, never copies attribution, never reads affiliate tables (doc 30 §4) |
| A6 | Segmentation → CRM | call | `Evaluation` + evidence | Not an authorization (SEG-3) |
| A7 | CRM → Bonus | call | `OfferGrantRequest` (§1.3 row 7) | **No affiliate identity, node id, code or commercial term reaches Bonus.** The gate never sees an affiliate |
| A8 | Bonus/Gamification → Orchestrator → Ledger | call | As flow (a), unchanged | Affiliate has zero presence in the fulfilment path |
| B1 | Ledger → Reporting | event/CDC | Ledger-derived revenue facts into `data-analytics`'s pipeline (doc 12) | Affiliate never reads `ledger_entries` directly and never maintains a revenue counter (doc 32 AFF-3) |
| B2 | Reporting → Affiliate | read | A canonical NGR/GGR measure, per period, whose **definition is `ledger-finance`'s** (DEP-AFF-4) | Affiliate never defines or computes the revenue measure |
| B3 | Affiliate → Ledger | call | `CommissionSettlementInstruction`: `instruction_id` (DB-unique), node, period, asset, integer-minor-unit amount, `rounding_rule_id`, accrual/adjustment/approval refs, reason code, requester | **Affiliate never calls `ledger.Post`, never imports `internal/ledger`/`internal/wallet`, never names a player ledger account, and never designs the posting shape** (doc 32 §7) |
| — | Affiliate → Gamification | (interface only) | Gamification events, if they ever exist, are engagement touches for multi-touch attribution | No `affiliate_*` table carries a point, level, badge, streak, mission or tournament column |
| — | Affiliate → affiliate-facing reporting | read | Subtree-scoped aggregates; pseudonymous per-player references where commercially required | **No player PII** — no email, name, DOB, document data, IP, instrument, or exact balance (doc 32 §10) |

---

## 3. Non-duplication register

The directive requires this stated **explicitly, per domain**. Each row
below is a commitment that the named domain does not hold a second copy
of, or a second authority over, the fact in the column. `✗` = must never
own/duplicate/decide. `read` = consumes through the owning domain's
interface. `own` = is the authority.

| Domain | Wallet balance | Ledger truth | Risk | RG | KYC | Identity | Bonus accounting |
|---|---|---|---|---|---|---|---|
| **`internal/ledger` / `internal/wallet`** | **own** | **own** | ✗ (never decides *whether* to post) | ✗ | ✗ | ✗ | **own** (posting map, ADR 0032) |
| **`internal/identity` / `identityresolution`** | ✗ | ✗ | ✗ | ✗ | ✗ (KYC is `internal/kyc`) | **own** | ✗ |
| **`internal/kyc`** | ✗ | ✗ | ✗ | ✗ (never blocks play directly) | **own** | read | ✗ |
| **`internal/rg`** | ✗ | ✗ | ✗ (no rule/threshold concept) | **own** | read | read | ✗ |
| **`internal/risk`** | ✗ | ✗ | **own** | ✗ (no player-status concept) | read (as request fields) | read | ✗ |
| **`internal/assetregistry`** | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ (owns asset authorization only) |
| **`internal/casino`** | ✗ | ✗ (calls `ledger.Post`) | read (calls `Evaluate`) | read (calls `EvaluateEligibility`) | ✗ | read | ✗ (but owns the funding-source-aware destination resolution in `postWin` — doc 29 §4.3) |
| **`internal/payments` / `withdrawal`** | ✗ | ✗ (calls `ledger.Post`) | read | read | read | read | ✗ |
| **`internal/sportsbook`** (unbuilt) | ✗ | ✗ | read | read | ✗ | read | ✗ |
| **`internal/bonus`** (unbuilt) | ✗ — no bonus table stores a balance; every bonus balance is derived from `ledger_entries` (doc 29 BI-3) | ✗ — calls `ledger.Post`; posting map is ADR 0032's | read | read | read | read | **own** (lifecycle/terms); accounting treatment is `ledger-finance`'s |
| **`internal/gamification`** (unbuilt) | ✗ | ✗ — never imports ledger/wallet | ✗ — no limit/threshold/cap/counter/velocity | ✗ — no self-exclusion concept | ✗ | read | ✗ — never grants a bonus |
| **`internal/segment`** (unbuilt) | ✗ | ✗ | ✗ — **no risk score/tier/classification; reads `internal/risk`, never computes** (doc 30 §8.3) | ✗ — may suppress, never constitute (doc 30 §8.4) | ✗ — reads status only | ✗ — reads references only | ✗ |
| **`internal/crm`** (unbuilt) | ✗ — profile balance is a decorative projection with `as_of` (doc 31 §4) | ✗ — never imports ledger/wallet; no `crm_*` monetary counter | ✗ — caps messages, never value (doc 31 §8.4) | ✗ — enforces by calling `EvaluateEligibility`, never caches | ✗ — reads status with `as_of` | ✗ — read projection only, never written back | ✗ — references `offer_id`+`offer_version_id`; supplies no terms |
| **`internal/affiliate`** (unbuilt) | ✗ — never touches a player balance | ✗ — never posts; hands an instruction to `ledger-finance` | ✗ — no limit/counter/player-risk concept; self-referral fraud is `risk`+`bonus-engine`+`identity-compliance`'s | ✗ | ✗ | ✗ — affiliates are `StaffUser`s with affiliate roles (pending `security`, DEP-AFF-1); players are ordinary `Person`+`PlayerAccount` | ✗ — a promo code is an attribution token; bonus value is granted only by Bonus |
| **`internal/agentnetwork`** (unbuilt) | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ — pure hierarchy primitive |
| **Reward Orchestrator** (unbuilt) | ✗ | ✗ — supplies the split instruction it received; never invents treatment | ✗ — does not re-run Risk | **re-checks, unconditionally** (doc 21) — but never owns | ✗ | ✗ | ✗ |
| **`data-analytics` / Reporting** | ✗ | read-only (CDC), never authoritative; never run against the transactional ledger for reports | ✗ | ✗ | ✗ | ✗ | ✗ |

### 3.1 The three cross-cutting statements behind that table

1. **One wallet, one ledger.** No domain above holds a balance, an
   accrual, a float, a points balance, a commission counter or a
   liability figure outside the ledger(s) the `ledger-finance` specialist
   owns. Every monetary figure any of these domains *displays* is derived
   at read time.
2. **One gate chain.** `AssetAuthorization → RG → Risk`, in that fixed
   order, inside the transaction that produces the effect, fail-closed on
   any error. No domain adds a fourth gate, reorders the three, caches an
   answer, or introduces a bypass. Segment membership, CRM audience
   membership and affiliate attribution are **inputs and evidence**, never
   authorizations.
3. **One identity.** No domain creates a second player, person, staff or
   principal model. New actors attach to the existing ones by reference
   (doc 26's rule, now applied to affiliates as well).

---

## 4. Dependency graph and build-order implications

**This section authorizes nothing.** It makes the dependencies visible so
that the human's stage sequencing is an informed decision rather than a
discovered one.

### 4.1 The graph

```
                       ┌──────────────────────────────────────┐
  BUILT TODAY          │ identity · auth · audit · tenant ·    │
  (Stage 1–4G)         │ ledger · wallet · payments ·          │
                       │ withdrawal · casino · kyc · rg ·      │
                       │ risk · assetregistry · idempotency    │
                       └───────────────┬──────────────────────┘
                                       │
        ┌──────────────────────────────┼───────────────────────────┐
        │                              │                           │
        ▼                              ▼                           ▼
 ┌─────────────┐              ┌────────────────┐         ┌──────────────────┐
 │ casino      │              │ EVENT TRANSPORT│         │ agentnetwork     │
 │ postWin     │              │ (outbox OR     │         │ (hierarchy       │
 │ destination │              │  broker)       │         │  primitive)      │
 │ (doc 29     │              │ OI-CRM-1       │         │ unbuilt          │
 │  §4.3, OI-5)│              │ doc 22 open#1  │         └────────┬─────────┘
 └──────┬──────┘              └───────┬────────┘                  │
        │                             │                           │
        │   ┌─────────────────────────┴───────────┐               │
        ▼   ▼                                     │               ▼
 ┌────────────────┐        ┌──────────────┐       │      ┌──────────────────┐
 │ BONUS ENGINE   │◀───────│ SEGMENTATION │       │      │ RETAIL  │AFFILIATE│
 │ internal/bonus │ consumes│internal/     │       │      │         │(subtree)│
 │ (Stage 4H-B1)  │        │  segment     │       │      └─────────┴────┬────┘
 └────────┬───────┘        └──────┬───────┘       │                     │
          │                       │               │                     │
          │                       ▼               ▼                     │
          │                ┌──────────────────────────┐                 │
          │                │          CRM             │◀────────────────┘
          │                │      internal/crm        │  (attribution → C-21)
          │                │  needs: segment + event  │
          │                │  transport + CONSENT     │
          │                └───────────┬──────────────┘
          │                            │
          │  ┌─────────────────────────┘
          ▼  ▼
   ┌──────────────────┐        ┌──────────────────┐
   │ REWARD ORCHESTR. │◀───────│  GAMIFICATION    │
   │ (needs a SECOND  │        │  (independent of │
   │  producer to be  │        │   CRM/Affiliate/ │
   │  worth building) │        │   Segmentation)  │
   └──────────────────┘        └──────────────────┘

   CONSENT MODEL (identity-compliance, DEP-CRM-1) ──▶ blocks every CRM send
   COMMISSION POSTING (ledger-finance, DEP-AFF-4) ──▶ blocks affiliate settlement
```

### 4.2 The honest answers

| Question | Answer | Why |
|---|---|---|
| Can CRM's journey engine be built before Segmentation exists? | **No.** | CRM builds no criteria (doc 31 CI-8). Every audience term is a `segment.Resolve` call. Without segmentation, CRM would grow a criteria evaluator, which is precisely the duplication this gate exists to prevent |
| Can CRM be built before an event transport exists? | **No, not the trigger/journey half.** | Doc 31 §6.2: CRM's triggers include absences and non-monetary states that no ledger-derived stream can express. A best-effort in-process call silently drops triggers. The static half (campaign/audience/preference authoring) is buildable; the journey engine is not |
| Can CRM send anything before a consent model exists? | **No.** | DEP-CRM-1. No consent record ⇒ fail-closed deny. `identity-compliance` must own it first |
| Can Segmentation be built before Bonus? | **Yes, and it should precede it** where Bonus's Offer eligibility axis pins a `segment_version_id` | Doc 10 W2.2/W7 already write the Offer axis as a segment reference. The minimal slice (static membership + the few predicates the five in-slice bonus types need) is small |
| Can Segmentation deliver its full criteria list today? | **No.** | Doc 30 §4: C-01 (VIP) blocked on Gamification, C-03/C-05/C-06 (lifecycle) on CRM, C-08 on Sportsbook, C-18 on Bonus, C-19 **has no authoritative artifact at all**, C-21 on Affiliate, C-14 blocked by the standing `TODO(jurisdiction)` gap |
| Can Affiliate be built before `agentnetwork`? | **Only flat (no sub-affiliates).** | Doc 32 §3.1. A flat first slice needs no closure table; sub-affiliate override (§6.4) defers. The alternative — a second tree — is the duplication doc 26 predicted and forbade |
| Can Affiliate settle commission before `ledger-finance` designs the posting? | **No.** | DEP-AFF-4, the ADR 0035 precedent. Accrual and approval are buildable; settlement is not |
| Can Bonus be built before CRM/Affiliate/Segmentation-at-full-capability? | **Yes** — and Stage 4H-B1's slice is already scoped that way | Doc 29 §3.2's minimal segmentation, plus doc 10's five in-slice types, none of which requires CRM or Affiliate |
| Is the Reward Orchestrator worth building for Bonus alone? | **No.** | Doc 10 B0 §4/doc 29 §6.3: one fulfilment mechanism, one producer. B1 leaves the `RewardFulfiller` **interface** (not a stub) so a second producer does not force a rewrite |
| Can Gamification be built independently of all three new domains? | **Yes** — it has no dependency on CRM, Affiliate or (beyond `segment_ref`) Segmentation | Docs 17/18/19/20 |
| Does any of this unblock the open human decisions? | **No.** | G-2, `OpenBetSelfExclusionPolicy`, cashout policy and FD-1 are untouched by this gate (ADR 0039) |

### 4.3 Critical-path observation for the human

Two items are **upstream of a great deal** and are not owned by any of
the three new domains:

1. **Event transport** (outbox vs. broker) — gates CRM journeys, and is
   the honest reason doc 22 has stayed "design only" since Stage 4H-A.
2. **A consent model** — gates every CRM communication, and is
   `identity-compliance`'s, not CRM's.

And one is upstream of Stage 4H-B1 itself and already escalated:
`casino`'s `postWin` funding-source-aware destination resolution
(doc 29 §4.3/OI-5) — without it, a bonus-funded casino win credits
withdrawable cash with no wagering requirement. It appears in §4.1's
graph because it is on the Bonus path, not because this gate resolves it.

---

## 5. Ownership map — as updated

`docs/governance/ownership.md` now carries rows for `internal/segment`
(pre-existing, ratified at Wave 1 and confirmed consistent with doc 30),
`internal/crm`, and `internal/affiliate`. Summary, with the reasoning
recorded where a choice was made:

| Path | Architecture owner | Implementation owner | Note |
|---|---|---|---|
| `internal/segment` | `architect` (interface/contract/schema) | `bonus-engine` (first consumer's call sites) | Unchanged from Wave 1; doc 30 confirms it holds against the fuller requirement list |
| `internal/crm` | `architect` (doc 31) | **OPEN DECISION** — `backend` while architecture-only; a dedicated `crm` specialist if implementation is authorized | Same shape as the Retail/Gamification precedent. Flat package; no Retail-style split (doc 31 §12.3) |
| `internal/affiliate` | `architect` (doc 32) | **OPEN DECISION** — same two options | Operational surface only |
| `internal/agentnetwork` | `architect` | **OPEN DECISION** (pre-existing row) | **Now has a second named consumer** (Affiliate), which is the outcome doc 26 §7.1 predicted; the row's rationale is confirmed rather than revised |
| Commission accounting | `ledger-finance` | `ledger-finance` | Mirrors ADR 0035's retail-accounting split (DEP-AFF-4) |
| Consent model | `identity-compliance` | `identity-compliance` | DEP-CRM-1 — does not exist today |
| CRM/affiliate reporting dimensions | `data-analytics` | `data-analytics` | DEP-CRM-2; mirrors doc 26's retail reporting extension |

## 6. Cross-references

- Segmentation: `30-segmentation-engine-architecture.md`
- CRM: `31-crm-engine-architecture.md`
- Affiliate/Acquisition: `32-affiliate-and-acquisition-architecture.md`
- Bonus implementation contract (master map, transport finding, invariants): `29-bonus-implementation-contract.md`
- Bonus lifecycle and terminal-grant contract: `10-bonus-engine-architecture.md`
- Domain boundary registry: `02-domain-and-service-boundaries.md`
- Reward Orchestrator: `21-reward-orchestration-architecture.md`
- Event taxonomy: `22-canonical-activity-event-taxonomy.md`
- Retail/hierarchy precedent: `26-retail-operations-architecture.md`; `docs/decisions/0035-…`, `0036-…`
- Reporting/BI: `12-audit-reporting-architecture.md`
- Unmade human decisions: `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`
- Dependency/risk register: `13-dependency-map-and-risk-register.md`
