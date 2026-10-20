# Residence determination maker-checker (lic10)

- **Scope:** migration 0181 part A, `internal/residenceapproval`, `internal/kyc` (`market_evidence.go`), audit gap G8, owner question Q21 (triage L-15).
- **Status:** IMPLEMENTED against the local database; the integration tests are for the owner to run (the authoring session could not reach the database harness). Software capability only. No policy value is shipped.
- **Not a legal decision.** Whether a second person must approve a residence determination is the operator's configuration (a compliance decision). Never set a mode because this page suggests one.

## 1. The behaviour, and the default

A reviewer's residence determination is the evidence that admits a player to deposits, casino play and sportsbook wagering. A tenant's mode decides whether a second person must approve it:

| Mode | A determination is market evidence |
|---|---|
| `none` (the DEFAULT of every tenant; no policy row = `none`) | at once, exactly as before migration 0181 |
| `into_open_market` | at once if the brand does not serve the country at the time of the determination; otherwise only after a second, distinct Person approves |
| `all` | only after a second, distinct Person approves |

**Behaviour change on deploy: none.** An existing tenant has no policy row, so nothing changes until a `compliance` officer configures a mode.

A determination that needs approval and has none is PENDING and is NOT market evidence (the evidence read excludes it, so the player fails closed at the market gate with the existing "no verified residence" outcome). If the same player has an older, still valid, approved determination on another verification, that one stays effective until the new one is approved.

The mode in force is **snapshotted on each determination when it is made**. Tightening the mode applies to FUTURE determinations; determinations already made stay as they were. If the operator wants existing players re-checked, that is a separate, deliberate action (not built).

`into_open_market` relies on the application's open-market evaluation at the time of the determination, passed to the database as the transaction-local setting `app.residence_into_open_market`. With the mode in force and the setting missing the database REFUSES the determination (fail closed). A determination into a market that opens LATER is not re-approved; choose `all` if that matters.

## 2. Configuring the mode (no code release)

Routes (tenant-scoped staff only; a platform token is refused; there is no platform_acting path):

| Route | Who | Step-up |
|---|---|---|
| `GET /v1/admin/tenants/{tenantID}/residence-approval-policy` | `compliance`, `tenant_admin` of the tenant | none |
| `POST /v1/admin/tenants/{tenantID}/residence-approval-policy` `{mode, reason_code}` | `compliance` of the tenant | none |
| `POST /v1/admin/tenants/{tenantID}/residence-approval-policy/{policyID}/approvals` `{reason_code}` | `compliance` of the tenant | critical |

- A change to a STRICTER mode (`none` to `into_open_market` to `all`) is active at once (200, one actor): it only tightens.
- A change to a LESS strict mode is filed PENDING (202) and takes effect only after two approvals by two distinct Persons, neither the requester's. The "stricter needs 0 approvals" rule is a table CHECK, not application logic.
- The database compares the requested mode with the mode in force; the caller never states the previous mode.
- A pending request is only valid against the mode it was filed against: if the mode changed since, approving it is 409 and it must be refiled.
- Every write needs a signed actor proof (ADR 0110) and a role/Person check in SQL; the runtime role has no write privilege on any of the tables.
- The approver accounts must be linked to a Person (staff without a Person get 403 "not linked to a Person").

## 3. Approving a determination

- `GET /v1/admin/kyc/residence-determinations/pending` (`verification:read`, tenant-scoped): ids and times only, never the country.
- `POST /v1/admin/kyc/verifications/{id}/residence-approval` `{determined_at, reason_code}` (`verification:review` = `compliance`, tenant-scoped, step-up critical).
  - `determined_at` is the exact time listed for the determination; a newer determination replaces it and makes the approval 404.
  - The approver must be a different staff account AND a different Person than the determiner (403 four-eyes). Two compliance officers are therefore needed in the tenant for a mode other than `none`.
  - The country itself is read with the existing residence-read permission; the approval is not a way to read it.

## 4. Failure modes and checks

| Symptom | Meaning | Action |
|---|---|---|
| Players of a tenant cannot deposit/play after the mode was tightened, "no verified residence" | their new determinations are pending | list pending, approve (second compliance officer) |
| Review fails with a database error mentioning the open-market evaluation | mode `into_open_market` and the market could not be evaluated | retry; check the operating-market configuration |
| 403 "not linked to a Person" | a staff account with no Person | link the account to a Person (staff lifecycle runbook) |
| 409 on approval of a policy | not pending, already active, or the mode in force changed | refile |

Audit: `residence_policy.changed`, `residence_policy.approved`, `kyc.residence_determination_approved` (ids and codes only, never the country), plus the existing `kyc.verified_residence_determined` and `market.residence_into_open_market` rows and the `kyc.residence_into_open_market` alert (unchanged).

## 5. Residuals

- Launch readiness has no INFO item for "mode is `none`" (adding one changes `launchgov.SnapshotVersion`, which this work must not touch). Proposed text is in `docs/plans/lic10-record.md`.
- No platform-level floor: a tenant's compliance can loosen its own mode with two approvals. A platform minimum is NOT IMPLEMENTED.
- Per-jurisdiction override of the mode is NOT IMPLEMENTED (the mode is per tenant).
- Back Office screens are NOT IMPLEMENTED (API only).
