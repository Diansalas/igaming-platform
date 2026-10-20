# lic15: real-stack e2e for operating bases (record)

Branch `lic15-e2e-basis` (from integ-b). Test code only (`backoffice/e2e/`), plus one server setting in `run.sh`
(`STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS=60`, a documented lowest bound). No application, Go, migration or OpenAPI change.
`06-operating-basis.spec.ts` no longer uses `test.fixme`.

## Covered (all against the real API and database)
(a) no_licence_documented set through the nav, tenant picker and dialog with every field explicit; "Pending approval"; before
approval no `operating_bases` row exists and `GET /v1/admin/operating-bases?current=true` totals 0; requester's Approve disabled and
server 409 on the requester's token; two distinct Persons approve; row `active` with `change_request_id` = the request; detail shows
the decision record with two approvers. (b) platform_licence set with the synthetic licence, same flow. (c) withdraw: single actor,
applied at once, version history 2. (d) review: flag for re-review with an earlier date is narrowing (applied at once, DB
`needs_rereview` true, UI "Flagged for re-review"); extend + clear flag is widening (pending, row unchanged, then B and C approve
via API, row updated, 3 versions). (e) tenant staff: nav hidden, UI denial, 403 on `/v1/admin/operating-bases`, tenant filing of a
basis refused (4xx) with no row. (f) empty review date keeps Submit disabled; an unknown operation code passes the client and is
refused by the server, the refusal is rendered, nothing stored. (g) a PAST review date is accepted by the system as a narrowing
review and the UI shows "Review overdue: no longer permitting". (h) step-up prompt forced by ageing the session past the 60 s
critical age; the prompt appears and the filing completes.

## Not covered
The resolver itself (no HTTP endpoint; evidence is the absence of an approved row), partner_licence, brand/product-scoped bases,
restricted products, 72 h expiry and 24 h stale approvals, withdrawal-restriction-policy and KYC enforcement-policy screens (do not
exist), operating-policy/ceiling kinds.

## Notes
* Scopes are fixed per test (FI/SE x operations), so a fresh database is required per run (already required by the first-admin seed).
* A full run takes about 12 minutes (TOTP step waits and the 70 s step-up ageing).
