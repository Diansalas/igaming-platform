# 14 — MVP Scope, Future B2B Scope, Build Sequence, and Effort Assessment

Status: Stage 0 draft. Source: Blueprint §9, §10.

## B2C MVP scope (own brand, real money, Stage 0–4 target)

In scope: identity + tenancy foundation, wallet/ledger, one game
aggregator, one PSP (plus crypto rail if Q1 resolves crypto-first), KYC/
AML/RG compliance subsystem, bonus engine core (Campaign/Offer/Grant/
Progress with a small initial rule set), reporting pipeline sufficient for
daily GGR/NGR and compliance exports, back office sufficient for our own
team to operate the brand, basic audit/RBAC.

Out of scope for MVP (deferred, tracked below): sportsbook, partner
console (no partners yet), second brand/tenant, GLI-19 certification,
ISO 27001, in-house affiliate platform, advanced CRM/journey builder.

## Future B2B platform scope (Stage 5–7+)

Sportsbook (proves the shared-wallet model works for a second product
type), partner console, multi-tenancy hardening beyond RLS (schema/
database/cluster-per-tenant as demanded), GLI-19 certification, second
brand onboarding as a real dry run of the "form + DNS record" goal,
ISO 27001 posture (year-two per Blueprint §8).

## Features deliberately deferred

- In-house affiliate tracking (buy first; Blueprint §1 notes many
  platforms bring it in-house in year two once volume justifies it).
- Sportsbook feed-and-API integration (start with widget/iframe only).
- Database-per-tenant / cluster-per-tenant isolation (build the path, not
  the destination, until a partner/jurisdiction demands it).
- CRM journey-builder depth beyond what's needed to trigger bonus grants.
- ISO 27001 formal certification work.
- **Gamification Engine in full** (points/XP/levels/achievements/badges/
  missions/challenges/tournaments/leaderboards/streaks), **the Reward
  Marketplace**, and **the standalone Reward Orchestrator** —
  architecture frozen speculatively in Stage 4H-A ahead of any product or
  business requirement. Added by a Wave-2 `product-owner-proxy` scope
  review of Stage 4H-A's architecture-freeze deliverables: the Blueprint
  (all 20 pages, reviewed in full for this finding) never mentions
  gamification, points, XP, levels, achievements, badges, missions,
  tournaments, leaderboards, streaks, a reward marketplace, raffles, or
  mini-games — its §4.5 "Bonus and promotion engine" is exactly
  Campaign→Offer→Grant→Progress with five config axes, precisely what
  this document's MVP scope above already lists. This domain has no
  anchor in the Blueprint and no anchor anywhere in this roadmap prior to
  Stage 4H-A. Revisit when a concrete retention/engagement product
  requirement or B2B partner need is identified, not before — and when it
  is, note that `18-tournament-architecture.md`'s settlement/prize-
  arithmetic/anti-collusion depth is the part of the frozen design most
  likely to need re-scoping at that time (it carries more design rigor
  than the Bonus Engine's own core lifecycle received, for a feature with
  zero scheduled build, and will need re-review against whatever RG/
  Risk/identity-resolution shape exists by then).
- **Extensible Asset/Currency Registry admin API + FX/Conversion
  architecture** (Stage 4H-B0-R3 confirmed product requirement, analysis
  only — `docs/architecture/27-stage-4h-b0-scope-and-implementation-
  plan.md` §26). The underlying schema (`assets`, migrations 0003/0006)
  is already an open, extensible registry with no closed enum and no
  hardcoded decimal count anywhere in the codebase; what's missing is
  the operational surface — an admin API, an authorization/RBAC model
  for who may register/activate an asset, mandatory audit logging on
  that mutation, additional per-asset eligibility columns (wallet/
  deposit/withdrawal/settlement), an FX Rate Provider interface, and a
  Conversion Service sitting between it and the ledger's existing
  `ConversionOperation` (ADR 0021). A dedicated future stage should
  define these in a new ADR (recommended: ADR 0037) before any of it is
  built. Not started; no impact on Bonus Stage 4H-B1 or Retail beyond
  the pre-existing, independently-tracked conversion-clearing-account
  open decision (ADR 0021, `ledger-accounting-model.md` §2).
- **The `ExternalRewardProvider`/External Reward Provider contract**
  (`docs/decisions/0033-provider-interoperability-and-external-bonus-
  engines.md`, `docs/architecture/23-external-reward-provider-contract.md`)
  — grounded in a real hybrid-licensing/sportsbook-interop concern
  (Blueprint §4.4's widget/iframe framing), so it clears the
  future-B2B-architecture bar in principle, but sequenced ahead of need:
  Sportsbook is P3 in this document's own build order below, no
  commercial sportsbook relationship exists yet, and
  `09-sportsbook-architecture.md` does not exist yet either. Defer further
  work on it until sportsbook architecture actually starts.

**Bonus Engine implementation scope note** (added by the same review):
when Stage 4H (Bonus Engine implementation) is authorized, scope it to
`10-bonus-engine-architecture.md`'s core lifecycle for the bonus types
this document's MVP scope actually needs (deposit, reload, cashback,
generic wagering bonus, coupon) — treat that document §2's tournament/
mission/loyalty-reward type-matrix rows as blocked on the deferred
Gamification Engine and out of the first implementation slice, not as
implied-included; and do not build the Reward Orchestrator as a
standalone domain in that same stage unless Gamification is authorized
alongside it — confirm at that time whether Bonus Engine can fulfill
directly through `wallet`/`ledger` and Casino's free-round interface
(once built) without the extra orchestration layer, since building a
three-domain-ready orchestration layer ahead of a second concrete
reward-producing domain is exactly the "generality for a hypothetical
future need" `CLAUDE.md`'s scope test exists to catch.

Recorded here rather than silently dropped, per `CLAUDE.md`'s scope-
expansion test — revisit each when the corresponding trigger condition
(B2B partner signed, volume threshold, regulator demand) occurs.

## Recommended implementation order

Matches `MASTER-BUILD-PROMPT.md` stages, which follow the Blueprint's
dependency-driven sequencing (§9): wallet/ledger gates everything;
compliance gates real money; multi-tenancy hardening pays off only once
there's a second tenant to prove it against.

```
P0  Licensing & vendor contracts, hosting, architecture decisions
P1  Core spine — identity, wallet, ledger, 1 aggregator, 1 PSP
P2  Compliance — KYC, AML, RG — plus bonus engine, reporting
P3  Sportsbook widget on the shared wallet
P4  Multi-tenancy hardening, partner console, GLI-19, second brand
```

Blueprint's estimate: ~12 months to the own brand taking real bets,
~22 months to a partner brand running on the platform, with overlap
because compliance/vendor lead times are not engineering time.

## Realistic engineering-effort assessment (Blueprint §9, indicative)

Team: 4–6 backend engineers, 2 frontend, 1 SRE, 1 QA (payments/gaming
background), 1 product owner, plus a compliance officer and a dedicated
provider/PSP relationship role (not optional — contract negotiation is a
full-time job founders routinely underestimate). Fully loaded cost
indicatively €1.2–2m/year (Malta reference point) plus vendor revenue
shares (aggregators 8–15% of casino GGR, sportsbook providers 8–12%) and
PSP costs at Anjouan rates. A smaller team stretches the timeline roughly
in proportion; vendor contracting, licensing, and certification do not
compress at any team size because they run on other parties' calendars.

**These figures are Blueprint-cited and explicitly marked there as
indicative — verify against current quotes before committing to a plan.**

## What Claude can reasonably automate

- Service/API scaffolding, schema design and migrations, business logic
  implementation against agreed interfaces.
- The full financial test matrix, integration adapters against sandboxes,
  reconciliation jobs.
- Documentation, ADRs, CI/observability wiring, back office/frontend UI
  implementation.
- Security- and correctness-focused code review, threat modeling at the
  design level.

## What still requires human/vendor/legal involvement

- Gambling licensing itself, jurisdiction/market selection with legal
  weight, provider and PSP contract negotiation, KYC/AML vendor
  contracting, hosting AUP confirmation in writing, GLI-19/ISO 27001
  actual certification and audits, penetration testing by a qualified
  external party, production incident accountability, and all six open
  decisions in `docs/decisions/0005-open-business-decisions.md`.
- Commercial pricing, equity/partner negotiations, and production launch
  authorization.

Software engineering feasibility and commercial/regulatory/operational
feasibility are tracked separately — this document does not claim the
former substitutes for the latter.
