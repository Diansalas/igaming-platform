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
