# 35 — Product Surfaces Roadmap: Stage Skeleton and Dependency Graph

Status: **DESIGN/ARCHITECTURE ONLY — `NOT IMPLEMENTED`.** No UI code, no
route, no screen is authorized by this document. Produced in Stage
4H-B1, Wave 1.5 Fix Round 2 ("Product Surface Roadmap Gate"), authored by
`architect`, in parallel with two companion documents this document does
**not** restate:

- `36-backoffice-and-partner-console-architecture.md` (`backoffice`) —
  Operator Back Office and Partner Console detail.
- `37-b2c-brand-frontend-architecture.md` (`frontend`) — B2C Brand
  Frontend detail.

Both companion documents were authored concurrently without this
document's stage IDs available to them, per the authorizing directive;
they use placeholders (`[Stage: Back Office MVP — ID TBD]`, `[Stage:
Partner Console MVP — ID TBD]`, `[Stage: B2C Brand Frontend MVP — ID
TBD]`). §3 below assigns the real IDs and §8 states plainly that
reconciling those placeholder references in docs 36/37 to the IDs below
is a required, mechanical follow-up step — not done by this document,
since it does not own those files.

This document owns the **overall skeleton**: the stage-numbering
decision, the five stages' dependencies and one-line scopes, the
cross-domain dependency graph, and an honest statement of which backend
domains are and are not implementation-complete today. It does not
design any UI and does not restate §5–§9 of doc 36 or §2–§4 of doc 37.

---

## 0. The gap this closes

`MASTER-BUILD-PROMPT.md`'s Stage 6 reads, in full: *"B2C frontend + back
office + partner console, reporting/BI pipeline (CDC → ClickHouse)."*
One line, four surfaces, no information architecture, no MVP boundary
per surface, no stated backend dependency, and — after Stage 4H's very
deep sub-stage history — no acknowledgment that Stage 6 as originally
scoped is now years of elapsed project history away from the stage
numbers currently in play. A human directive flagged this as a real gap:
the Blueprint defines three product surfaces (B2C Brand Frontend,
Operator Back Office, Partner Console) plus a Retail/POS surface this
project separately investigated (doc 26), and the roadmap has never named
an explicit implementation stage for any of them. This document is that
naming, plus the dependency graph the directive asked for.

**This document does not authorize any of the five stages it names.**
Authorization is a human decision at the appropriate gate, per
`CLAUDE.md`'s stage-gate rule, exactly like every other stage in this
project's history.

---

## 1. Stage-numbering convention found, and why this document follows it rather than inventing one

Inspected: `MASTER-BUILD-PROMPT.md`'s Stage 0–7 scheme, and
`docs/progress.md`'s actual stage history (`## Stage <N>` headers).
Actual practice, verified against the headers that exist:

- The original scheme is `Stage 0` … `Stage 7`, sequential, one topic
  each (`MASTER-BUILD-PROMPT.md`).
- In practice, **Stage 4 alone has been extended far beyond that
  original one-line scope**, through a lettered/dashed sub-stage
  convention invented incrementally as real work demanded it, always
  keeping the parent stage number: `4A`, `4D-RG`, `4E`, `4F`, `4G`,
  `4G-FINAL`, `4G-FINAL-FINANCE-GATE`, `4H-A`, `4H-B0`, `4H-B0-R1`
  through `4H-B0-R7`, `4H-B1` (itself further subdivided into `Wave 1`,
  `Wave 1.5`, `Wave 1.5 Fix Wave`, `Wave 1.5 Fix Wave, Phase 2`, and now
  this dispatch, `Wave 1.5 Fix Round 2`). No stage in this history has
  ever been given an arbitrary new top-level number (there is no
  "Stage 9"); every extension stayed inside its logical parent stage's
  number.
- **Stage 5 (Sportsbook) and Stage 6 (the three UI surfaces +
  reporting/BI) have never been started or sub-numbered.** They remain
  exactly as `MASTER-BUILD-PROMPT.md` defined them: reserved numbers,
  zero elaboration.

**Conclusion applied here**: Product Surfaces work is not a Bonus Engine
concern and does not belong inside the `4H` family (which the convention
above reserves for Bonus/Gamification/Retail-scope work specifically).
It **is** exactly what `MASTER-BUILD-PROMPT.md`'s Stage 6 already names.
Following the established convention (extend the existing reserved
number with letters, as Stage 4 did) rather than inventing a new
top-level number: **this document sub-stages Stage 6**, exactly as Stage
4 was sub-staged, the first time Stage 6 has needed real elaboration.

---

## 2. Whether the Surface Architecture Gate needs its own stage ID

**Decision: no new stage ID.** This dispatch (`architect` producing this
document; `backoffice` producing doc 36; `frontend` producing doc 37) is
itself the gate the directive asked about — but it is a design-only
activity dispatched inside an already-named unit of work (Stage 4H-B1,
Wave 1.5 Fix Round 2), the same way Wave 1.5's own "Architecture
Reconciliation Gate" was handled: as a named activity inside an existing
stage, not a new stage number. Two reasons this is the right call, not
just administrative convenience:

1. **A new "Stage 6-GATE" would be a stage with no implementation
   surface of its own** — it produces documents, not code, and this
   project's convention (per §1) reserves stage IDs for units that get
   their own gate-report/authorization cycle with actual build content.
   A pure documentation exercise inside an already-open stage does not
   need one, and inventing one here would be exactly the kind of
   process-for-its-own-sake `product-owner-proxy` exists to catch.
2. **It would create exactly the numbering-collision risk this project
   has already hit twice** (OI-EOI-1 in doc 34: the Stage 4H-B0-R6 ADR
   `0049` collision, and a second near-collision in this same gate's
   numbering discussions). Folding this gate into the dispatch that
   already names it avoids a third occurrence.

If a future reader needs to cite this gate precisely: **"Stage 4H-B1,
Wave 1.5 Fix Round 2 — Product Surface Roadmap Gate"** is its full name,
and this document plus docs 36/37 are its output.

---

## 3. The five stages — IDs, dependencies, one-line scope

| ID | Name | One-line scope | Blocking dependency | Detail owned by |
|---|---|---|---|---|
| *(folded into current dispatch — see §2)* | Surface Architecture Gate | Define the roadmap skeleton (this document) and the two surfaces' detailed target architecture (docs 36/37) before any UI implementation starts | None — documentation only | architect (this doc), backoffice (doc 36), frontend (doc 37) |
| **Stage 6A** | Back Office MVP implementation | Operator-facing control surface: players, wallets/transactions (read), KYC/AML review, RG restrictions, risk rule config + denial view, casino catalogue admin, payments/provider config, withdrawal review/approve queue, asset registry admin, tenant/brand provisioning, staff RBAC (create/list), audit search — as **queues built around real staff jobs**, not a table browser (doc 36 §2) | **None blocking.** Every in-scope screen is backed by an `IMPLEMENTED` domain today (doc 36 §1, §6.1) | backoffice (doc 36 §5–§6) |
| **Stage 6B** | Partner Console MVP implementation | External B2B partner/tenant-admin surface: read-only own-tenant brand/provider config, minimal aggregate-only compliance posture view | **Yes — a partner-scoped RBAC role/principal does not exist yet** (`internal/auth`'s `rolePermissions` explicitly defers this; doc 36 §4.1, §8.2 item 1). No screen can ship safely before it | backoffice (doc 36 §7–§8) |
| **Stage 6C** | B2C Brand Frontend MVP implementation | Player-facing site for our own B2C brand (and, later, any B2C-shaped tenant): registration/login, lobby + mock-provider casino, wallet/cashier, RG self-service (self-exclusion + status), account, KYC | **None blocking.** Every in-scope feature area is backed by an `IMPLEMENTED` domain today (doc 37 §2.1, §3.1). Two small, non-blocking dependency requests noted (doc 37 §3.2.1) | frontend (doc 37 §2–§3) |
| **Stage 6D** | Retail/POS operational surface | Land-based/agent-network distribution: agent hierarchy, terminal/POS transaction flow, cash settlement, commission accounting | **Not a Blueprint requirement — see §7.** Depends on `internal/retail`/`internal/agentnetwork`, neither of which exists at all (design-only, doc 26); also depends on Wallet/Ledger (already implemented) | architect (doc 26; a future dedicated stage would own the detail) |

**Recommended relative sequencing** (stated for the human authorization
decision this document does not make): **6A before 6B** (doc 36 §8.1,
§9 — 6B cannot ship a single screen before 6B's blocking RBAC dependency
resolves, while 6A has none). **6D last, and only if a real business
need materializes** (§7). **6C's position relative to 6A/6B is a
business-priority call, not a technical dependency** — see §5's
discussion of the directive's literal graph vs. commercial priority.

---

## 4. Domain dependency list — which backend domains gate which surface

Per the directive's list (Identity, Wallet/Ledger, Risk, RG, KYC,
Payments, Casino, Sportsbook, Bonus, Segmentation, CRM, Gamification,
Affiliate, Reporting, CMS), cross-checked against `internal/` at HEAD
(the same verification doc 36 §1 and doc 37 §2 already did — not
re-derived independently here, reused so all three documents agree on
one status table rather than three slightly different ones):

| Domain | Status | Gates |
|---|---|---|
| Identity (Person/PlayerAccount/StaffUser/Tenant/Brand) + Auth/RBAC | `IMPLEMENTED` (foundation-level; partner-scoped role deferred — the single blocker in this table) | Foundational to all three surfaces; the partner-scoped RBAC gap specifically blocks **6B only** |
| Wallet / Ledger | `IMPLEMENTED` | Foundational to 6A and 6C (balance/transaction views, deposit/withdrawal) |
| Withdrawal (state machine, four-eyes) | `IMPLEMENTED` | 6A (review queue), 6C (player-facing request/cancel) |
| Payments (deposits) | `IMPLEMENTED` | 6A (provider config), 6C (deposit initiate) |
| Casino (mock provider, catalogue) | `IMPLEMENTED` (mock; real aggregator `PROVIDER DEPENDENT`) | 6A (catalogue admin), 6C (lobby/launch) — both labeled `MOCK` per `CLAUDE.md`'s completion-label rule, not silently presented as production-ready |
| KYC | `IMPLEMENTED` (foundation; vendor `PROVIDER DEPENDENT`) | 6A (review queue), 6C (verification/document upload) |
| RG (Responsible Gaming) | `IMPLEMENTED` — **but only the self-exclusion + status subset**; deposit/loss/wagering/session limits and reality checks are documented extension points (ADR 0026 §15), `NOT IMPLEMENTED` | 6A (restriction management, implemented subset), 6C (self-service, implemented subset only — the limits/reality-check gap is a compliance-relevant exclusion, stated honestly in doc 37 §3.4, not silently dropped) |
| Risk (rule config + inline `Evaluate`) | `IMPLEMENTED` as configuration + enforcement; **no persisted case/alert/decision-log table** | 6A gets a rule-config screen + an audit-derived recent-denial view, explicitly **not** a case-management queue (doc 36 §5.1 finding) — a real gap for anyone expecting 6A to be a risk analyst's primary tool |
| Asset Registry (+ dual-control four-eyes) | `IMPLEMENTED` | 6A (admin), 6C (currency/asset display) |
| Reconciliation | `IMPLEMENTED` as a scheduled job + data model; **no admin read API** | 6A — a small, non-blocking dependency request (doc 36 §6.3) |
| Audit | `IMPLEMENTED` | 6A (search) |
| **Sportsbook** | **`NOT IMPLEMENTED`** — Stage 5 not started, no `internal/sportsbook` package | Gates: no sportsbook UI in **any** surface (6A, 6B, or 6C) until Stage 5 lands |
| **Bonus Engine** | **`NOT IMPLEMENTED`** — architecture frozen only (docs 10, 27–29, 34), Stage 4H-B1 core not authorized (this project's own current Phase 2 verdict: **NOT READY**) | Gates: no bonus opt-in/wagering-progress UI in 6C, no bonus campaign/grant admin in 6A, beyond what already exists as raw ledger-account views (`player_locked_cash`/`player_locked_bonus`) |
| **Segmentation** | **`NOT IMPLEMENTED`** — architecture frozen only (doc 30) | Gates: no audience/segment builder UI in 6A/6B |
| **CRM** | **`NOT IMPLEMENTED`** — architecture frozen only (doc 31) | Gates: no campaign/journey UI in 6A/6B, no in-app communications/preference-center UI in 6C |
| **Gamification** | **`NOT IMPLEMENTED`** — architecture frozen only (docs 17–21); also `product-owner-proxy`-deferred pending a real business/B2B need (doc 14) | Gates: excluded entirely from every surface's MVP, indefinitely, until the doc 14 deferral is revisited |
| **Affiliate** | **`NOT IMPLEMENTED`** — architecture frozen only (doc 32) | Gates: no affiliate management UI in 6A/6B |
| **Reporting / BI (CDC → ClickHouse)** | **`NOT IMPLEMENTED`** — no pipeline exists at all; itself part of the original Stage 6 one-liner and not yet given its own sub-stage (§8 open item) | Gates: 6A's dashboard is a "minimal operational-status panel," not real BI (doc 36 §6.1); 6B's "aggregate-only compliance posture view" reads directly from source domains, not a BI layer, and is itself gated on a `security`/`identity-compliance` sign-off on which aggregates are safe to expose (doc 36 §7.3) |
| **CMS** | **`NOT IMPLEMENTED`** — no Blueprint-anchored requirement found beyond brand/theme config, which is tenant-config's, not a separate CMS domain (doc 36 §1) | Gates nothing MVP-blocking; a real content-managed marketing/promo surface is out of scope until a concrete requirement exists (mirrors doc 14's "no fabricated content" reasoning, doc 37 §2's Promotions row) |

**Honest summary, stated plainly per the directive**: of fifteen listed
domains, **six are not implemented at all** (Sportsbook, Bonus,
Segmentation, CRM, Gamification, Affiliate), a seventh has no real
pipeline (Reporting/BI), and an eighth has no anchored requirement (CMS).
**Stage 6A and 6C are, today, gated by none of the eight** — their MVP
scopes were deliberately drawn to the domains that already exist,
per doc 36 §1 and doc 37 §2.1. **Stage 6B is gated by one thing that
does exist as a concept but not as code**: the partner-scoped RBAC role.
**Stage 6D is gated by two domains that do not exist even as a design
beyond doc 26's freeze**, and, more fundamentally, by having no Blueprint
anchor at all (§7).

---

## 5. The dependency graph the directive asked for

```
Core Platform → APIs → Back Office → Partner Console → B2C Brand Frontend → Retail/POS
```

Reproduced exactly as specified. Read as an **exposure/trust-boundary
ordering** — each stage sits further from internal staff and closer to
the public than the one before it (internal operator tooling, then a
still-gated external-partner surface, then the fully public real-money
player surface, then a physically separate operational channel) — **not**
as a statement that each stage's *backend* strictly requires the
previous UI stage to exist first. It does not, and §4's table shows why:
6A and 6C both sit directly on "Core Platform → APIs" with no UI-to-UI
dependency between them.

**Flagged honestly, not silently resolved** (per `CLAUDE.md`'s rule
against silently contradicting an existing record): this graph's literal
ordering — Back Office, then Partner Console, then B2C — is a
defensible *exposure-ordering* read, but it is **not** the same thing as
*commercial launch priority*, and should not be read as overriding
`MASTER-BUILD-PROMPT.md`'s and doc 14's existing, human-approved
priority: **B2C is the P1 commercial objective** ("Launch our own B2C
casino brand... first tenant"), while Partner Console is explicitly
**out of MVP scope** in doc 14 ("no partners yet"). Both orderings are
legitimate for different questions:

| Question | Graph to use |
|---|---|
| "Which surface can be safely built without a new RBAC primitive, purely on what's implemented?" | The directive's graph: 6A first (no blocker), 6B needs the new role, 6C independently has no blocker either |
| "Which surface should the business actually launch first, for revenue?" | Doc 14's existing priority: **B2C (6C)**, unchanged by this document |

This document does not resolve which ordering governs actual scheduling
— that is a human sequencing decision, informed by both readings, not
an architecture question this document can answer for them.

### 5.1 Expanded technical dependency chain (Core Platform → APIs, made concrete)

```
Identity/Auth ─┐
Wallet/Ledger ─┤
Payments      ─┼──▶  internal/httpserver (existing API layer)  ──▶  6A (Back Office)  ──▶  6B (Partner Console, blocked on partner RBAC)
Withdrawal    ─┤                                                └──▶  6C (B2C Brand Frontend)
Casino (mock) ─┤
KYC           ─┤
RG (subset)   ─┤
Risk (config) ─┤
Asset Registry┘

Sportsbook, Bonus, Segmentation, CRM, Gamification, Affiliate, Reporting/BI, CMS:
  NOT on the critical path above — each is an independently gated future
  extension to 6A/6B/6C's scope, not a precondition for the MVP slice
  defined in §3, and each surface's screen inventory (doc 36 §5, doc 37
  §2) explicitly excludes what these domains would add until they exist.

Retail/POS (6D): a separate branch entirely — internal/retail,
  internal/agentnetwork (neither exists) sitting alongside, not atop,
  the chain above; only Wallet/Ledger is shared.
```

---

## 6. When does actual Back Office frontend implementation occur — stated plainly

The directive requires an explicit answer, not a deferral. Here it is:

**Stage 6A (Back Office MVP) has no unbuilt-domain blocker today.**
Every screen in its scope (doc 36 §6.1) is backed by an `IMPLEMENTED`
domain, verified against `internal/` at HEAD by `backoffice` independently
of this document. Nothing about Bonus, Segmentation, CRM, Gamification,
Affiliate, Sportsbook, Reporting/BI, or CMS being unbuilt prevents Stage
6A from starting, because none of those domains is in Stage 6A's scope.

**My best-reasoned recommendation, given that**: Stage 6A could be
authorized to start **immediately, in parallel with the current Stage
4H-B1 Bonus Engine work**, if the human wants Back Office tooling sooner.
But I recommend, if there is a choice to be made about *sequencing*
rather than *technical readiness*, that Stage 6A start **after Stage
4H-B1 reaches a resolution** (either an implementation is authorized and
lands, or the human defers Bonus Engine work entirely) — not because 6A
needs anything Bonus produces, but because several of 6A's own screens
(Wallets, Transactions, the withdrawal queue's ledger-derived state)
render ledger/wallet shapes that Stage 4H-B1's own work is actively
changing under it this same stage: the `player_locked_cash`/
`player_locked_bonus` account split (migration `0048`, landed), a
possible future `bonus_expense` account, and `HeldDispositionRecord` (an
LF-2 fix this round introduces, doc 34 §3.1). Building 6A's read views
against a ledger surface that is still being actively re-shaped risks
avoidable rework for a UI that itself is not blocked by any of it.
**If the human's priority is Back Office regardless of that timing risk,
nothing technical prevents starting Stage 6A today** — this is a
sequencing recommendation, not a dependency.

Restated as the directive asked, in the requested form: **Back Office
MVP implementation (Stage 6A) occurs once authorized — technically
ready now; recommended after Stage 4H-B1 (Bonus Engine) reaches a
resolution, to avoid building against a ledger surface still being
actively changed by that stage's own work.**

---

## 7. Retail/POS — an honest, non-Blueprint-anchored stage

Per `CLAUDE.md`'s "do not assume a requirement exists unless the
Blueprint supports it" rule, and doc 26's own already-recorded finding
(verified by full-text search of all 20 Blueprint pages): **the
Blueprint contains no retail, land-based, agent-network, POS, terminal,
shop, kiosk, or cash-counter requirement of any kind.** Retail/POS is a
`RECOMMENDATION`-only stage (doc 26's own labeling), frozen at the
architecture level during Stage 4H-B0's scope investigation, with **zero
implementation** and two entirely new, unbuilt domains
(`internal/retail`, `internal/agentnetwork`) as prerequisites beyond the
already-implemented Wallet/Ledger it would reuse.

Stated plainly, mirroring how doc 14 already treats Gamification: **Stage
6D should not be scheduled ahead of, or given resourcing priority over,
Stage 6A/6B/6C** — those three are Blueprint-anchored surfaces the
Blueprint's own system map names explicitly; Stage 6D is not. Revisit
Stage 6D only when a concrete land-based/agent-distribution business
requirement is identified (a real B2B partner or market need), the same
trigger condition doc 14 already applies to Gamification. Numbering it
`6D` here reserves a slot in the family without implying priority.

---

## 8. Open items and required follow-up

| ID | Item | Owner | Blocking? |
|---|---|---|---|
| **OI-PSR-1** | Docs 36 and 37's placeholder stage references (`[Stage: Back Office MVP — ID TBD]`, `[Stage: Partner Console MVP — ID TBD]`, `[Stage: B2C Brand Frontend MVP — ID TBD]`) must be mechanically replaced with `Stage 6A`, `Stage 6B`, `Stage 6C` respectively. Not done by this document, since it does not own those files | Orchestrator (or backoffice/frontend on next touch) | No — cosmetic, does not block authorization of any stage |
| **OI-PSR-2** | Reporting/BI (CDC → ClickHouse) was part of the original Stage 6 one-liner but is not one of the five items this directive asked to be staged. It remains un-sub-staged. A future `Stage 6E` (or folding it into 6A/6B's own dependency lists more concretely) is a real open question, not resolved here — flagged rather than silently numbered | architect (future) | No |
| **OI-PSR-3** | The Back Office vs. B2C sequencing question (§5, §6) is a human priority call this document deliberately does not make. Both readings of the dependency graph are presented; neither is authoritative over the other | Human | No — informs, does not block, either stage's eventual authorization |
| **OI-PSR-4** | Stage 6B's blocking RBAC dependency (doc 36 §4.1, §8.2) needs a `security` + `identity-compliance` decision (new `StaffRole` vs. distinct principal type) before Stage 6B can be authorized at all | security + identity-compliance | Yes, before Stage 6B |
| **OI-PSR-5** | This document's own stage IDs (`6A`–`6D`) are a recommendation, not yet ratified anywhere outside this document. Recording them in `docs/active-stage.md`/`docs/progress.md` at the next stage transition, and updating `MASTER-BUILD-PROMPT.md`'s Stage 6 one-liner to point here, are both Orchestrator follow-ups, not actions this document takes itself | Orchestrator | No |

---

## 9. Cross-references

- The gap this closes: the human directive naming no explicit
  implementation stage for any of the three Blueprint product surfaces;
  `MASTER-BUILD-PROMPT.md`'s Stage 6 one-liner.
- Companion documents (concurrent, cross-reference this one back):
  `36-backoffice-and-partner-console-architecture.md`,
  `37-b2c-brand-frontend-architecture.md`.
- Stage-numbering precedent this document follows: `docs/progress.md`'s
  `## Stage 4*` header history; `MASTER-BUILD-PROMPT.md`'s Stage 0–7
  scheme.
- Retail/POS's honest non-Blueprint status: `26-retail-operations-
  architecture.md` §0.1.
- Gamification's identical deferral pattern, applied here to Retail/POS
  by analogy: `14-mvp-scope-and-roadmap.md` ("Features deliberately
  deferred").
- Domain boundary verification source: `02-domain-and-service-
  boundaries.md`, `13-dependency-map-and-risk-register.md`.
- Bonus Engine's current, unresolved state (informing §6's sequencing
  recommendation): `docs/active-stage.md`, `docs/governance/wave-1.5-
  fixwave-phase2-report.md`.
