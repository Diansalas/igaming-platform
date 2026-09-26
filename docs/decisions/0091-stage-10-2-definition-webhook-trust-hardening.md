# ADR 0091 — Stage 10.2 Definition: Webhook Trust Hardening

- **Status:** ACCEPTED — authorized by the human on 2026-09-26 after
  accepting the Stage 10.1 completion report (final commit
  `7ea084025be058e1bff5e8cfd238a4f6bb8f6041`).
- **Decision type:** stage definition (precedent: ADR 0087, ADR 0090).
- **Owner:** Master Orchestrator.

> Numbering note: this is ADR 0091 in `docs/decisions/`. Migration
> `0091_sportsbook_settlement` in `migrations/` is an unrelated file.

## Context

Stage 10.1 closed PAY-WH-TENANT-1 for the payments webhook (mock
credential resolver) and recorded two pre-existing webhook weaknesses:
**KYC-WH-1** (High: committed KYC webhook secret, no environment gate on
the mock KYC provider/route, `provider_reference` exposed to players, so a
player can forge an "approved" callback) and **CAS-WH-TENANT-1** (Medium:
casino webhook tenant taken from the URL, per-process key, tenant not in
the signed content). It also recorded **CI-FLAKE-281** (one unidentified
intermittent integration failure).

## Decision

Stage 10.2 = webhook trust hardening, three workstreams:

1. **KYC-WH-1 (mandatory):** remove the committed secret (no replacement
   secret in source, tests, fixtures, docs or new history); environment-
   gate the mock KYC provider/route (absent in production, fail closed when
   test support is disabled, staff/test authorization where applicable,
   never a player self-approval path); never trust a player-supplied
   provider reference; cryptographic tenant binding; verify before parsing
   trusted claims, loading tenant state, changing KYC state or writing
   success audit records; cross-tenant attempts fail closed; the listed
   tests; KYC API/OpenAPI review.
2. **CAS-WH-TENANT-1 (mandatory):** the same principles for the casino
   webhook — cryptographic tenant binding, URL tenant never authoritative
   alone, verify before trusted processing, cross-tenant rejection,
   replay/idempotency consistent with casino architecture, no financial
   effect on rejection, RLS, audit, production-safe gating of mock
   behaviour, regression tests. Reuse or extract a provider-neutral webhook
   verification abstraction rather than a second subtly different
   implementation (the ADR 0022 §3 callback contract of Stage 10.1).
3. **CI-FLAKE-281:** investigate without blocking 1–2; fix if reproducible;
   otherwise document evidence, attempts, likely cause, why it does not
   block, and the diagnostic improvement.

**Not automatically in scope:** PAYWH-BRAND-1, PAYWH-RL-1, PAYWH-TS-1 —
`security`/`architect` decide whether any is required to close the
High/Medium blockers; if a genuinely necessary new requirement appears,
stop at a human gate rather than expand scope. No real PSP/KYC/casino
provider or real credential system (the real credential resolver remains
future provider-integration work). No AI (ADR 0089). No AWS action; the
future staging refresh uses `deploy.sh down` then `deploy.sh up` only on
explicit human authorization.

**Gates:** G1 KYC complete; G2 casino complete; G3 security/RLS review;
G4 full regression; G5 CI evidence + CI-FLAKE-281 disposition; G6
OpenAPI/docs; G7 no secrets committed; G8 pushed; G9 clean tree; G10
deployment plan. Stop before AWS deployment.

## Consequences

Records: planning/design papers under `docs/plans/stage-10.2-planning/`,
task registry "Stage 10.2", completion report
`docs/governance/stage-10.2-completion-report.md`.
