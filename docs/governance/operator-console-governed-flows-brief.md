# Decision brief - operator console for the governed payout flows (2026-10-10)

Status: **decision material only; nothing is built.** Source: a read-only product-surface review on 2026-10-10 (repository at
`c01c859`). The request was: "Fix only genuine, approved product/contract gaps. Do not create unnecessary UI work."

## Findings (verified in the code by the review; paths approximate)

1. The approved governed flows exist only as API routes plus Go tests:
   - M4/M2/M1 payment force-resolutions: `/v1/admin/tenants/{tid}/payment-force-resolutions` (6 routes);
   - HSEC hold release: `/v1/admin/tenants/{tid}/withdrawal-hold-resolutions` (6 routes);
   - payment kill switches (7 routes) and staff payout-instrument list/suspend.
   The Back Office has no screen or API client for any of them.
2. The Back Office never shows a payout attempt state: no `disputed`, no park reason (`destination_mismatch`,
   `destination_integrity_failure`, `provider_reference_conflict`, `amount_asset_mismatch`). No admin read route exists for
   payout attempts, parks or reconciliation findings (`pay_captured_unposted`, `pay_declared_*`), so no UI could show them yet.
3. OpenAPI lacks the 12 M4/HSEC routes and the core withdrawal lifecycle routes. This is a documentation gap and is being
   closed under the provider-independent, approved work of this cycle (branch `gov-r36-openapi`).
4. B2C shows no neutral "under review" wording for a parked payout (the player sees the withdrawal state only); nothing
   sensitive leaks.

## Why the UI is NOT built now (recommendation: defer)

- Every governed flow is `MOCK`-only: non-MOCK M4 is BLOCKED (D-7, T10, S-3, provider mapping) and no production HSEC policy
  row exists (Q-HSEC-1/2/3 open). There are no real operators and no real parks to act on.
- A console for four-eyes money-resolution flows is a security-sensitive surface (platform_acting scope, ADR 0110 signed
  actor proofs, evidence line selection, approvals by distinct Persons). It needs a design (roles, what an approver must see
  before approving, blind entry vs displayed reference: the OPEN `evidence_ref_hash` question) and a security review; building
  it before those decisions would bake in assumptions.
- It also needs a NEW read surface (attempt/park/finding views). That is new API and a new data-exposure review, not a UI-only
  change.

## What WOULD be provider-independent and cheap once the owner wants it

- A read-only admin view of the payout attempt state and park reason on the withdrawal detail (needs one new tenant-scoped
  read route + security review of what is exposed).
- A read-only reconciliation findings list.
- The M4/HSEC request/approve screens (after the `evidence_ref_hash` and approver-display decisions).

## Decision required (owner, with security)

- D-OPS-1: is an operator console for the governed flows wanted before any real provider, or can operators use the API
  (scripts/tooling) for MOCK and early sandbox? (Recommended: API-only until a sandbox provider exists; then build the read-only
  views first.)
- D-OPS-2: which roles may see park reasons and attempt state (finance, compliance, platform_admin).
