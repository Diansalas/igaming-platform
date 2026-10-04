# 0105 — Human Decision Response: closed-tenant player funds (HD-PRH2-9), E1 KYC alert kind (HD-PRH2-10), E1 KYC worker identity (HD-PRH2-11), K3 scope

**Status: HUMAN DECISIONS RECORDED (2026-10-04).** Source: the human owner's instruction "MASTER
ORCHESTRATOR — AUTHORIZE E1, K3 AND FINAL PRH-2 PATH", sections 1-4. This record reproduces the decisions
faithfully. It adds no threshold, role name, jurisdiction rule or legal outcome the human did not give.
Recording a decision is not an implementation: the design is produced and reviewed (security,
ledger-finance, architect) before code, per `docs/plans/prh2-hardening-round/plan.md`.

## 1. HD-PRH2-9 (registry id HD-TENANT-CLOSURE-PLAYER-FUNDS-1) — player funds held by an already-closed tenant

**Question (from PRH-2 H ledger-finance F3):** a closed tenant's never-sent `created` payout keeps the
player's withdrawal hold forever (sweeper T2 re-claim is withheld for non-active tenants; M3 staff
RejectCreated has no non-test caller; ADR 0101 force-resolution is not implemented). What happens to the
player's funds?

**HUMAN ANSWER (decided):** a **controlled STAFF RESOLUTION PATH**.
- Do NOT automatically dispatch a payout for a closed tenant.
- Do NOT automatically cancel/release the funds without a controlled resolution.
- Flow: closed tenant -> outstanding player payout/hold -> authorized financial-staff resolution queue ->
  required authorization / four-eyes according to the applicable financial policy -> permitted
  resolution -> deterministic ledger operation -> complete audit trail.
- The resolution mechanism must be **configurable and jurisdiction-aware**. No single universal legal
  outcome is hard-coded. The platform provides the mechanism for the applicable operator/jurisdiction to
  determine the permitted resolution.
- It must preserve: tenant isolation, financial invariants, idempotency, actor/subject separation,
  auditability, required four-eyes controls, and no direct balance mutation. No unrestricted bypass.
- If a specific resolution outcome or threshold is legally required but cannot be determined from the
  existing architecture, it is surfaced as a **separate human decision**, not invented.

**Handling:** design owned by the K3 design phase (ADR 0101 refresh; ledger-finance + security +
architect). Whether it fits K3's M1/M2 force-resolution scope or is a separate registered workstream
(PAY-CLOSED-TENANT-FUNDS-RESOLUTION-1) is decided in that design under the no-scope-expansion rule; it is
never silently dropped and never decided by a code default. Until implemented, the H residual stands (the
held funds have no automated release) and is a launch blocker for any tenant-closure flow.

## 2. HD-PRH2-10 — E1 KYC alert kind

**HUMAN ANSWER (decided):** create a **dedicated KYC alert kind** (do not reduce the KYC alert to an
untyped generic log-only follow-up). Migration 0114 is used if required. The kind integrates with the
existing alert architecture (ADR 0102) and supports severity, tenant/platform scope, durable state,
auditability, routing, and future configurable recipient handling. No real recipients or on-call personnel
are invented; operational routing stays configurable. ALERT-DELIVERY-1 stays OPEN.

## 3. HD-PRH2-11 — E1 KYC worker identity

**HUMAN ANSWER (decided):** create a **dedicated non-human platform-service identity for the KYC worker**
using the existing identity/RBAC architecture. It must be least privilege, explicitly scoped to the KYC
worker, non-human, auditable, unable to perform unrelated financial/admin operations, tenant-safe, and
documented through an ADR. Migration 0114 is used if required. No broad generic service identity is
reused where a dedicated identity is required by the approved design. No credentials or secrets for real
external providers are created.

## 4. K3 (migration 0115) scope instructions

- Proceed with K3 using migration 0115.
- Before implementation, refresh ADR 0101 (stale migration numbers, post-D2/F-pay line references) and add
  `internal/payments/poll_evidence.go` to the authoritative file list.
- PAY-RECON-PARKED-CAPTURE-STANDING-1 and PAY-RECON-POLL-REF-CLEAR-1: **fold into K3 only if, after
  review, they are directly within K3's reconciliation scope.** If either is genuinely outside K3's
  technical scope it is NOT forced in for convenience: it stays separately registered with owner and
  dependency documented and remains a launch prerequisite where applicable. Neither is silently closed.
  PAY-RECON-PARKED-CAPTURE-STANDING-1 remains a HARD PREREQUISITE before any real PSP integration.
- E1 and K3 run in parallel only if their files/migrations do not conflict; separate ownership,
  migrations and review chains; otherwise sequenced with explicit ownership. Completed D1/D2/F-pay/H/I-wire
  code is modified only for a directly related defect.
