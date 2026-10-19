# Licensing slice 1: licence lifecycle and expiry (record)

- **Branch:** `lic1-licence-lifecycle`, from `claude/licensing-jurisdiction-completion` at `812faf6`. Worktree `.claude/worktrees/lic1`.
- **Closes (provider-independent, interim safe behaviour):** audit gaps G-4, G-5, G-6, G-8 (rule only), G-12; triage L-22 (writers and
  alert; the owner answers are still pending, see section 6).
- **Migration:** `0176_licence_lifecycle_and_expiry_alerts` only (up and down). `0177` belongs to slice 2.
- Software capability only. Nothing here says any licence is legally sufficient or any jurisdiction approved.

## 1. Deliverables and labels

| Deliverable | Label | Notes |
|---|---|---|
| `SetLicenceStatus`, `SetLicenceValidityDates`, `SetLicenceReviewDue`, `SetLicenceEvidenceReference` (`internal/jurisdiction/licence_lifecycle.go`) | IMPLEMENTED | platform-scope assert first; reason code required; row lock; one audit row (before/after, reason code, change kind) in the same transaction |
| ChangeKind classification (widening / narrowing / neutral) | IMPLEMENTED | returned by every write, recorded in the audit row; unit-tested for every branch |
| HTTP routes + OpenAPI + golden route table + step-up class | IMPLEMENTED | 4 PUT (critical) + 1 GET (none); `docs/api/openapi/platform-api.yaml` |
| Migration 0176: `review_due_at`, `evidence_reference`, `updated_at`; `licence_versions` (append-only, RLS platform-admin read/insert only, deny UPDATE/DELETE/TRUNCATE, trigger on every licence change); baseline version for existing licences | IMPLEMENTED | down refuses (`LV099`) while any licence has more than one version (change history would be lost) or licence alerts exist |
| Maker-checker for widening changes | NOT IMPLEMENTED (slice 2) | residual G-1/G-2, section 4 |
| Expiry/review sweep (hourly, read-only) + alert kinds `licence.expired` (P1) / `licence.expiring` (P2) | IMPLEMENTED | `internal/jurisdiction/licence_sweep.go`, wired in `cmd/platform-api/main.go`; no auto-suspension |
| Alert delivery to a person | NOT IMPLEMENTED (ALERT-DELIVERY-1 open) | rows only |
| G-8 rule: recorded `in_force` must not contradict the registry | IMPLEMENTED | new hard code `H-2:licensing_status_contradicts_licence_registry` |
| G-12: licence read error refuses | IMPLEMENTED (behaviour change, section 3) | |
| G-6 tests (resolver, market gate, HTTP, activation acknowledgement) | IMPLEMENTED | section 5 |
| Back Office page for licences (G-10) | NOT IMPLEMENTED | out of this slice (API-only) |
| Runbook `docs/runbooks/licence-lifecycle.md`, alert runbook in `observability-and-alerting.md` | IMPLEMENTED | |

## 2. Design decisions (local, reversible)

- **Status vocabulary unchanged** (`active`, `suspended`, `expired`). A regulator revocation is `expired` plus reason code and evidence
  reference. A distinct `revoked` is a human question (Q6).
- **Single actor plus `stepUpCritical` for every lifecycle write**, narrowing and widening alike. Maker-checker was not cheap to reuse:
  the ADR 0110 actor-proof operations are bound in the migration 0120 verifier and the launchgov four-eyes is a request/approval state
  machine; both need new database-bound operations, which is the slice 2 approval primitive. So the "two distinct Persons for widening" interim
  was NOT applied here. **Residual G-1/G-2 is owned by slice 2.** Hooks left for it: `ChangeKind` on every result and in the audit row.
- **Classification** (conservative: anything that makes the licence easier to rely on is widening): status toward `active`; later or cleared
  `expires_at`; earlier or cleared `issued_at`; postponed or cleared `review_due_at`. A mixed change is widening. An unknown stored status
  classifies as widening (stricter control). No-op changes are refused (400), so history has no empty rows.
- **Dates** are UTC calendar dates; `expires_at` must be after `issued_at` (Go check, not a DB CHECK, so existing raw-SQL fixtures cannot break).
  The validity boundary is `EvaluateLicenceValidity`'s, unchanged (half-open; not valid on `expires_at`).
- **evidence_reference**: opaque, 1..200 chars, trimmed, no control characters, no "@" (DB CHECK for length and trim). Nullable; never defaulted or
  generated. The HTTP JSON key is `licence_evidence_reference` and not `evidence_reference` because `TestEvidenceReferenceText_NotExposedByAPIOrAuditMetadata`
  (ADR 0114 S0) reserves that literal in `internal/httpserver` for the payments M4 reference; the guard was not weakened. The DB column and the service use
  `evidence_reference`. The audit row (`licenceState`) carries the value in `evidence_reference`; this is metadata in a platform-scope audit row
  (`internal/jurisdiction`, outside the guarded packages).
- **History** is written by trigger from the licence row, so even a raw SQL change by a platform-admin session is versioned. `changed_by` is the
  platform-admin principal GUC of the writing session and is redacted through `staffViewer` in the API (ADR 0114).
- **Alert design** copies `reconciliation.sweep_failed` (migration 0156): platform scope, subjectless (a licence can be shared by many tenants),
  raised only under the `alert_dispatcher` service identity, `alert_kinds_check1` widened by exactly the two kinds, the dispatcher INSERT policies widened by
  exactly the two kinds, a RESTRICTIVE dispatcher-only policy so a platform admin, tenant or scopeless session cannot raise them. `alerts_guard()` was NOT
  replaced (no kind-specific payload trigger): the allowlist of keys plus Go-side flat-scalar, size and charset validation apply, and the only raise path is
  `alerting.RaiseLicenceSweep`. Attributes: ids, counts and dates only; id lists are capped at 15 and `truncated` says so; exact counts are separate.
- **Sweep reads per tenant** with `WithTenantReadOnly` (the `licences_read` policy admits exactly the tenant's own licence), so no new RLS read path or service
  identity was needed. It aggregates per licence. A licence bound to no tenant affects nobody and raises nothing. The sweep does not read `tenants.status` /
  `brands.status` (the reviewed status-reader set is closed, `TestStatusReaderInventory_*`), so closed tenants can appear in the list; residual.
  A failure to list tenants is logged only (it is not `reconciliation.sweep_failed`: that kind's phase vocabulary is closed and belongs to the reconciliation sweep).
- **`expired` classification** in the sweep: status `expired`, or `expires_at` reached, whatever the status (a suspended licence past its expiry is still expired).
  `suspended` alone raises nothing: a person recorded it.
- **Warning window** 30 days: constant `jurisdiction.LicenceExpiryWarningWindowDays`, an interim engineering default (Q4). An overdue `review_due_at` counts as expiring.

## 3. Behaviour changes to existing code (stated explicitly)

1. **G-12 (`launchgov.licenceCheck`)**: a licence read error used to return an acknowledgeable `not_evaluable` / `read_error` check. It now returns an error and the whole
   evaluation refuses. Why: ADR 0112 section 8.5 says an unreadable licence refuses; before this change that was true in practice only because H-2 read the same row earlier.
   `configChecks` now returns `([]Check, error)`. An INVALID licence (suspended, expired, not yet issued, unbound, not found) is unchanged: an acknowledgeable `fail`.
2. **G-8 (`launchgov.licensingFailures`)**: new hard failure `H-2:licensing_status_contradicts_licence_registry` when the request records `in_force`, a licence is bound and found, and
   `EvaluateLicenceValidity != valid`. Recording `suspended`, `expired`, `applied_pending` or `conditional` for an invalid licence, and acknowledging R-LIC-VALID, still work.
   A validity evaluation error returns an error (refuse). No existing assertion changed.
3. `ListLicences` / `Licence` / `CreateLicence` now return the lifecycle fields; the audit state of `licence_created` gains `issued_at`, `expires_at`, `review_due_at`,
   `evidence_reference` (empty on create). The OpenAPI `Licence` schema gained the fields (the JSON key is `licence_evidence_reference`).
4. **Down-migration rule, and why it is not "any version row":** the existing `TestMigration0076_DownMigrationCleanThenFailsOnDirtyDatabase` and `TestQAAdversarial_PartialRollbackLeavesRegistryRLSDisabledButReapplyRestoresIt` seed a licence and then expect the down chain to roll back migrations newest-first until 0076's own guard. A rule that refused on ANY version row would have made 0176's down fail first and broken that premise. The 0176 down therefore refuses when a licence has MORE THAN ONE version (real change history would be lost) and tolerates the single creation/baseline snapshot, which is the licence row itself. Neither existing test was edited.
5. Test pins updated for the new routes only: `staff_route_static_test.go` (228 routes, 139 mutating), `staff_stepup_integration_test.go` (critical 68, read 89),
   `alert_test.go` subjectless allowlist (+2 kinds, an addition). No assertion was weakened.

## 4. Residuals

- **G-1/G-2 (slice 2):** reinstating a licence or extending `expires_at` is single actor plus step-up. Compensating control today: the audit row carries `change_kind`, and the
  runbook tells the operator to confirm every widening change out of band.
- **Alert delivery:** nobody is paged (ALERT-DELIVERY-1). The sweep alert is a durable row.
- **Sweep scope:** closed tenants/brands can be listed as affected; the list in an open alert is the one at first raise (later occurrences do not update attributes);
  an unbound licence is not alerted; a tenant-listing failure is logged only.
- **Auto-suspension** is not built (human decision). The runtime market gate stays the enforcement; withdrawals are not market-gated.
- **`issued_at` mandatory / evidence required before activation:** not enforced (human decisions Q2, Q8).
- **No Back Office page** (G-10); the lifecycle is API-only.
- `alerts_guard()` has no kind-specific payload trigger for the two kinds (see section 2).
- Security review: this slice touches admin permissions and RLS/alert policy surfaces; **`security` review is required before it is called complete** (not requested from here).

## 5. Tests (what each proves)

Unit (no tag): `internal/jurisdiction/licence_lifecycle_test.go` (classification, evidence validation, sweep classification incl. the `expires_at = today` boundary and the 30-day horizon,
payload bounded under 2048 bytes), `internal/launchgov/licence_check_test.go` (G-12: read error refuses; invalid licence stays a fail; boundary).

Integration (`-tags integration`):
- `internal/jurisdiction/licence_lifecycle_integration_test.go`: audit row (before/after/reason/change kind), version rows (gapless, `changed_by`), refused no-ops write nothing,
  missing reason refused, tenant-scoped and scopeless transactions refused, a tenant-scoped raw UPDATE changes nothing, a tenant cannot read `licence_versions`, history cannot be updated/deleted/truncated,
  a raw SQL change is still versioned, sweep raises one alert per licence with the affected ids, changes nothing, and (real alert write) dedups across passes.
- `internal/alerting/migration_0176_integration_test.go`: kinds seeded (p1/p2, platform, subjectless, not tenant-raisable); dispatcher raise works and dedups; platform admin, tenant and scopeless sessions cannot raise;
  Go refuses foreign attributes / wrong kind / nil licence; **down refuses while a licence has change history (more than one version)**, refuses while licence alerts exist, round-trips when no history exists and when a licence has only its single baseline/creation snapshot; baseline version for pre-existing licences.
- `internal/operatingmarket/licence_lifecycle_resolve_integration_test.go`: `suspended`, `expired` status, `expires_at = today`, `expires_at = yesterday` each give `not_permitted_by_licence` with the right ceiling reason, and
  restoring through the lifecycle re-opens it; `expires_at = tomorrow` still permits; `licence_id` NULL (new tenant and unbinding) gives `licensing_unknown`.
- `internal/httpserver/licence_lifecycle_market_gate_integration_test.go`: the real market gate, all four causes, registration / deposit / casino launch / sportsbook wager each 403 with one identical refusal, nothing created, refusal audit rows, restore re-opens.
- `internal/httpserver/licence_lifecycle_http_integration_test.go`: platform-admin round trip (change kinds, versions, list fields); validation refusals (no reason, `revoked`, unknown field such as `tenant_id`, no-op, bad dates, contact-like evidence, bad/unknown id) write nothing;
  tenant admin 403 and anonymous 401 on every route and the licence is unchanged; stale second factor refused with `step_up_required` (300 s) on every write and the licence unchanged, fresh session passes. The generic golden-table step-up test also covers the new routes.
- `internal/launchgov/licence_validity_integration_test.go`: for suspended, expired status, expires today and not-yet-issued: `in_force` refused by H-2 (nothing stored); an honest status is accepted; R-LIC-VALID is a recorded `fail` with the exact detail; not acknowledging is refused; with acknowledgement the activation succeeds (brand `active`).

Run results: see section 8.

## 6. Human decisions pending (exact one-question requests; interim behaviour in brackets)

1. Boundary: "Is a licence valid on its own `expires_at` date and on its own `issued_at` date in the Anjouan licence terms (and each later jurisdiction)?" [not valid on `expires_at`, valid on `issued_at`]
2. Evidence: "Which evidence reference must be recorded before a licence is bound to a tenant or a brand is activated?" [none required]
3. Auto-suspend: "When a licence expires, should the platform suspend dependent brands automatically, or only alert?" [alert only]
4. Window: "How many days before `expires_at` / `review_due_at` should the warning fire?" [30]
5. Recipients: "Who receives `licence.expired` and `licence.expiring`?" [no route; durable rows only]
6. Revocation: "Is a distinct `revoked` licence status needed, separate from `expired`?" [no; revocation = `expired` + reason code]
7. Hard block: "Should activation under an invalid licence be refused instead of acknowledgeable?" [acknowledgeable; contradiction with `in_force` refused]
8. Issue date: "Must `issued_at` be mandatory?" [no; NULL = none asserted]
9. Widening: "Is a second approver required to reinstate a licence or extend its expiry?" [slice 2 builds the primitive; single actor + step-up until then]

## 7. Proposed text for the orchestrator (not applied; governance files are not mine to edit)

- **Registry:** `MKT-LICSTATUS-1` and `MKT-EXPIRY-1` writer part: IMPLEMENTED by migration 0176 + `licence_lifecycle.go` (single actor, step-up critical; maker-checker NOT IMPLEMENTED, slice 2); boundary confirmation still OPEN (Q1).
  New items: `LIC1-SWEEP-1` (sweep lists closed tenants, attributes frozen at first raise, tenant-listing failure log-only), `LIC1-GUARD-1` (no kind-specific `alerts_guard()` payload trigger for `licence.*`).
- **ADR 0112 section 8.5:** state that `licenceCheck` read errors refuse as of this slice. Section 5.2: add H-2 `licensing_status_contradicts_licence_registry`.
- **Migration pin docs** (if the orchestrator keeps a list): 0176.

## 8. Verification evidence

(Filled in at commit time; see the final report.)

## 9. What the orchestrator must do to merge

- Merge branch `lic1-licence-lifecycle` into `claude/licensing-jurisdiction-completion`. Expected textual conflicts with slice 2 only in
  `internal/httpserver/staff_route_static_test.go` (route and mutating pin: lic1 is 228 / 139 from 223 / 135), `internal/httpserver/staff_stepup_integration_test.go` (critical / read pins),
  `internal/httpserver/staff_route_golden_test.go`, `internal/httpserver/jurisdiction_admin_routes.go`, `docs/api/openapi/platform-api.yaml` and `cmd/platform-api/main.go`; resolve by summing the counts.
- Do not renumber 0176. After merging, rebuild the test database from the merged tree and run `go test ./internal/db` and the integration suites listed in section 5.
- Request `security` review (admin permissions, RLS, alert policies) and `code-reviewer`.
