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

- **Self-exclusion**: `IMPLEMENTED`. Platform-wide by default for player
  self-service; tenant/brand-scoped for staff-initiated restrictions.
  Indefinite or time-bound; append-only (no early termination endpoint -
  ADR 0026 §2's own recorded open decision on why).
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
