# A10b record: operator consoles S3, S5, S7, S8 (ADR 0114)

Branch `a10b-operator-consoles`, base `1983863` (Record A4 merged, CI green (run 833)). Backoffice only: no Go, no migration,
no OpenAPI change, no route added. UI against the existing routes. Server-side authorization remains the authority; every
UI gate below is a hint, every refusal is rendered through a closed code map (the server `message` is never printed; the K3
and HSEC closed tokens in `message` are matched against an allow-list and replaced by UI-owned text).

## Delivered (labels per CLAUDE.md)

| Slice | Screen / route | Label |
|---|---|---|
| S3 | `/parked-payments`: queue over `GET .../payment-attempts` (filters state, operation, park reason, withdrawal id; keyset cursor next/previous; age column), attempt detail (single read, derived timeline, server `admissible_kinds` hint, links to withdrawal and to the force-resolution screen) | PARTIALLY IMPLEMENTED against MOCK (see gaps G1-G4) |
| S5 | `/reconciliation`: runs + findings for payment_statement, ledger_vs_projection, sportsbook_settlement (R2) and casino_consistency / casino_statement (existing casino routes); run detail, finding detail, severity/age, subject links; read only | PARTIALLY IMPLEMENTED (G5) |
| S7 | `/capability-grants`: K1 requests and grants lists, request form (staff directory picker or typed id, capability, expiry, reason), approve / reject (platform_admin), cancel (requester), revoke | PARTIALLY IMPLEMENTED (G6: no payload hash exists on K1) |
| S8 | `/force-resolutions`: K3 M1/M2 list, detail, propose, approve, reject, cancel; `/hold-resolutions`: HSEC request, approve, reject, cancel (platform_admin only) | PARTIALLY IMPLEMENTED against MOCK (G7-G9; M4 is S11 and is not offered) |

Common: permission-gated nav and routes (`RequireNavPermission`), tenant picker for platform principals only (UUID from the
URL, never used otherwise), loading / empty / error states, MOCK banner on every payments screen, no tokens or secrets in
URLs, server strings rendered as React text only, labelled controls, dialog roles. Money is formatted with BigInt
(`lib/minorUnits.ts`), never floating point.

How the four-eyes rules are handled: approve / reject dialogs open on a SNAPSHOT of the resolution and send exactly that
snapshot's `payload_hash` (verified by test, including a mutant that sends the refreshed hash; it is killed). The operator
confirms by typing the first 8 characters of the hash. Own-request approve is disabled as a hint; the server (distinct
Person, grant, proof, recount) decides, and a refusal is shown. Step-up: handled by the existing `apiFetch` interceptor
(`financial` for K3/HSEC request + approve, `critical` for K1 request/approve/revoke per the golden table); tested end to end
with the real `StepUpProvider` (the re-send carries the same body and the same Idempotency-Key; cancelling the prompt sends
nothing and says so). No hashing or signing exists in the UI (static pin).

## Gaps found (evidence), none worked around with invented behaviour

| # | Gap | Evidence | Effect on the UI |
|---|---|---|---|
| G1 | ADR 0114 S3 names provider and age filters; R1 has neither | OpenAPI `/payment-attempts` parameters: operation, state, terminal_reason, withdrawal_request_id, limit, cursor only | Not offered (a client filter over one page would hide rows). Provider and Age are columns. Needs a backend change if the owner wants the filters |
| G2 | No attempt event history route | OpenAPI has only list + single read | "Timeline" is derived from created / updated / resolved timestamps and the attempt's force-resolutions; the screen says provider interactions between are not shown |
| G3 | S3 items not delivered: payout-instrument view/suspend, withdrawal-detail attempt panel (R1b `payout_attempt`) | outside the requested A10b scope | NOT IMPLEMENTED here |
| G4 | `open_resolution_id` is always null for a platform caller | OpenAPI PaymentAttempt description; ADR 0114 section 12 item 4 | Detail cross-checks the K3 list for a pending resolution of the attempt |
| G5 | S5: no single-run / single-mismatch route, no `run_id` filter on mismatches, no severity on R2 shapes, no withdrawal-hold check D vocabulary (ADR 0113 4.4 kinds `withdrawal_hold_open_mismatch`, `withdrawal_request_ledger_link_invalid` are not in `StaffReconciliationKind`; they would surface only as `other`), no mutation route | OpenAPI `/reconciliation/{runs,mismatches}` and `StaffReconciliationKind`; `internal/reconciliation/alerts.go:42` | Detail comes from the list row; the finding table marks which loaded rows belong to the selected run; severity: casino "P1" (documented), staff "Not provided by the API"; check D has no representable stream; no acknowledge/resolve control (ADR 2.3: not a flow). PA gets no casino stream (tenant-scope route) |
| G6 | K1 has no `payload_hash`; list routes are unpaged and unfiltered; the staff directory never lists platform staff | OpenAPI CapabilityGrant* schemas and `/staff` description | The task's "proof-bound payload hash" cannot be shown for K1 (it exists for K2/K3/HSEC). The approve dialog shows the frozen request content and states K1 has none. A platform principal uses a typed staff id (a platform session may not request a grant for tenant staff: 409, mapped) |
| G7 | K3 / HSEC DTOs carry no asset exponent | OpenAPI PaymentForceResolution, WithdrawalHoldResolution | Amounts render as "N minor units of EUR (exponent unknown)" in those tables; the attempt views (which have `exponent`) format fully |
| G8 | `evidence_ref_hash` semantics are an OPEN owner decision (ADR 0114 9.4, ADR 0111 25.7); blind entry is not implemented | ADR text | The operator supplies a 64-hex digest computed OUTSIDE the console (runbook: hex SHA-256 of the external reference); the console never sees, hashes or sends the reference text. M4 kinds are listed as a hint only; no M4 action (S11) |
| G9 | HSEC has no candidate-list route (R3) | OpenAPI has only the four resolution routes | The withdrawal id is typed; the screen says so |
| G10 | The K1 / K3 / HSEC routes read no `Idempotency-Key` | grep of `capability_routes.go`, `payment_force_resolution_routes.go`, `withdrawal_hold_resolution_routes.go`; `launch_authorisation_routes.go:27` | The key is generated per dialog and re-sent identically (step-up re-send), but the server ignores it. Real double-submit protection is a synchronous once-guard plus the server's state refusals (409). Not claimed as server idempotency |

No small additive backend change was made or needed to deliver the slices.

## Changed existing assertions (complete list)

- `backoffice/src/layout/nav.test.ts`: the exact nav lists for finance, compliance and platform_admin gained the new
  entries (finance: Parked Payments, Force Resolutions, Reconciliation; compliance: those plus Capability Grants; platform
  admin: those plus Hold Release). Nothing relaxed; a new test pins which roles get which new entry.
- `backoffice/src/auth/permissions.test.ts`: additions only.
- No Go test, golden table, pin or OpenAPI file was touched.

## Files

API modules (each with `<module>.test.ts` pinning every request against the spec): `api/paymentAttempts.ts`,
`api/reconciliation.ts`, `api/capabilityGrants.ts`, `api/forceResolutions.ts`, `api/holdResolutions.ts`,
`api/idempotency.ts`. Test helpers: `test/openapiLite.ts` (+ its own negative-control test), `test/contractCapture.ts`,
`test/consoleFixtures.ts`. `openapiContract.ts` from sibling A10a did not exist in this base; `openapiLite.ts` is
independent and dependency-free (no YAML parser is installed), the orchestrator reconciles at merge. Screens under
`features/{consoles,payments,reconciliation,capability,resolutions}`. Wiring: `auth/permissions.ts` (flags
`reconciliation`, `capabilityGrants*`, `reconciliationStreams`), `layout/nav.ts`, `app/AppRoutes.tsx`.

## Proposed registry text (orchestrator to place; this record edits no registry)

`A10B-OPERATOR-CONSOLES-S3-S5-S7-S8`: ADR 0114 S3 (parked payments queue + attempt detail), S5 (reconciliation runs and
findings, 5 streams, read only), S7 (K1 capability grants console), S8 (K3 M1/M2 and HSEC consoles) `PARTIALLY IMPLEMENTED`
against the existing routes and MOCK providers; backoffice only; vitest + build green; security review pending. Not delivered:
S3 provider/age filters (no API), S3 payout instruments and R1b panel, S5 check D stream (no API vocabulary), K1 payload hash
(none exists), M4 (S11), blind entry (OPEN owner decision), HSEC candidate list (R3 absent). No legal or regulatory claim.

## Security probes (for the follow-up review)

1. Type `/hold-resolutions`, `/force-resolutions`, `/reconciliation`, `/capability-grants`, `/parked-payments` as support / risk /
   promotions / bonus_operations / tenant_admin / finance (per route): expect NotAuthorized and zero network calls; with the UI
   gate patched out, the server must still answer 403 (the screens render it).
2. As tenant staff, hand-edit `?tenant=` to another tenant: the hook ignores it for tenant principals; confirm the path always
   carries the token tenant and the server 404s a foreign tenant if forced.
3. Platform picker: `?tenant=` with a non-UUID, path traversal, very long string: never reaches a path.
4. Approve with a stale `payload_hash` (change the resolution between list and dialog): the dialog sends the frozen hash and
   blocks on a changed live hash; the server must refuse a hash mismatch.
5. Self-approval via a hand-crafted request as the requester; approval without a K1 grant; approval as a revoked grantee: server
   refusals appear through the token map.
6. K1: tenant_admin approve attempt (R-7): 403 or 409 shown; PA approve of own request; request for a tenant grantee from a
   platform session (409 mapped).
7. Hostile strings in every enum / id / reason field render as text (tested for attempts, grants, reconciliation); check
   `evidence_reference` text never appears in a K3 response, DOM or audit (H2 server side).
8. Double-submit under latency (dblclick tested), step-up replay: the re-send reuses body and Idempotency-Key; confirm the
   server's state refusals make a replay harmless since it ignores the key (G10).
9. Redaction: tenant viewer of platform-requested K1 / K3 rows sees scope only (null ids); no email appears from the staff
   directory (the typed entry omits it).
10. Casino mismatch free text (`reconciliation_key`, expected/actual) is rendered verbatim as text; confirm the server-side
    content of those fields is acceptable to show to tenant staff (pre-existing exposure).
11. Reason / note fields: PII warning shown; confirm no free text is echoed into URLs or storage (static pins).

## Residuals

Pagination of reconciliation / K3 / K1 lists follows the API (offset for R2 and casino; none for K1 / K3 lists, K3 capped
at 100). The K3 / HSEC amounts lack an exponent (G7). The MOCK banner is hardcoded (no server field). `Idempotency-Key` is
client-only (G10). Accessibility is basics only (labels, roles, captions, keyboard dialogs via the shared Modal); no audit
with assistive technology was done. Approver-side display of the statement evidence line (S11) is not built.
