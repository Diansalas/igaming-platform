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
