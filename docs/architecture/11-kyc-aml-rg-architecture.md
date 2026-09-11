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
