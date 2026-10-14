# ADR 0119 S5 + S6 (staff lifecycle): independent code + architecture review

- Reviewer: `code-reviewer` (independent of the implementer). Date: 2026-10-14.
- Branch `worktree-agent-a031d51e36f08b4f0`, head `131f0d8`, base `ff23a5c` (24 commits, 153 files, +9308 / -737).
- Scope: migration 0139 (up / down), the 9.5(b) policy pair, `internal/stafflifecycle`, the HTTP layer and route freeze,
  OpenAPI parity, removal of the legacy create route, every row of ADR 0119 s28.4, deviations S56-D1..D14,
  `cmd/seed-admin`, CLAUDE.md rule compliance. Financial correctness of the ADR 0082 A9 HARD GATE (submit / K1
  positions) is `ledger-finance`'s veto and security of the invite / break-glass design is `security`'s; concerns in
  those domains are surfaced below, not adjudicated.
- This review is not an approval of anything outside its own scope. Nothing here is a regulatory claim.

## Verdict

**NOT READY TO MARK COMPLETE - rework required on F1; F2-F4 should be resolved or explicitly accepted by the named
owner before merge.** The core of the slice is solid: the migration shape, triggers, the 9.5(b) pair, tenant
isolation, the error mapping, the audit / event split, the route freeze and the OpenAPI parity all hold up, and every
targeted test I ran passes. F1 is a confirmed, reproduced bypass of the ADR 0119 s11 break-glass control.

## Evidence I ran (local, private database only; NOT GitHub CI)

Private database `priv_cr_s56` built with the scratchpad `priv_db.sh` (sanctioned local credentials; no role, password
or privilege change; dropped after the review).

| Command | Result |
|---|---|
| `gofmt -l internal cmd deploy` | PASS (no output) |
| `go vet ./...`, `go vet -tags integration ./...` | PASS |
| `go test ./... -count=1` | PASS (no FAIL; only "no test files" lines) |
| `-tags integration -p 1`: `./internal/stafflifecycle/ ./cmd/seed-admin/ ./internal/capability/...` | ok (stafflifecycle re-run `-v`: 14 tests PASS, none skipped) |
| same, payments `-run 'TestSubmitReassertion_|TestStatic_SubmitReassertionPins|TestHSEC_HoldRelease_ActorProofTriggerOnGovernedTables_Last'` | ok |
| same, httpserver `-run 'TestLifecycle_|TestOpenAPI_StaffLifecycle|TestK1Reassertion_|TestSubmitWithdrawalHandler_SubmitterSuspendedInFlight|TestStaffRoutes_|TestStaffPersonLink_Remediates|TestStaffLiveness'` | ok |
| same, `./internal/providercred/ ./internal/db/ ./internal/staffmfa/...` (full packages) | ok |
| F1 reproduction: built `cmd/seed-admin`, ran the DEFAULT mode (`-email=... -create-person`, no `-break-glass`) against the private database while 5 active, enrolled platform admins existed | exit 0; an `active` `platform_admin` was created and a bootstrap token printed; 0 `staff_lifecycle_events` rows, audit `staff.platform_admin_bootstrapped` without `operator`, 0 `staff.platform_break_glass_used` alerts |

Not run by me: the full httpserver / payments packages, golangci-lint, the mutation pass, `migrate up/down/up`
round-trip (the implementer's s28.5 evidence covers those; I read the down migration instead).

## Findings (most severe first)

### F1 - HIGH (confirmed, reproduced): `cmd/seed-admin` default mode bypasses every s11 break-glass control

`cmd/seed-admin/main.go:184-248`. The new `-break-glass` mode refuses while a usable platform admin exists, requires
`-operator` / `-reason`, writes `owner_provisioned` and raises the P1 `staff.platform_break_glass_used`. The
pre-existing default (bootstrap) mode is unchanged apart from the Person requirement: it has no "no platform admin
exists" guard, no operator / reason (`op.validate(*issueBootstrap || *breakGlass)` is false), no lifecycle event and no
alert.

Failure scenario: an operator who can run the deployment job runs `seed-admin -email=x@corp -create-person` (omitting
`-break-glass`) on a live platform with N usable platform admins. A new active `platform_admin` plus a bootstrap token
is created; nobody is paged; the audit row names no operator or reason. ADR 0119 s11 step 1 ("refuses unless
`-break-glass` and `-reason` are given") and steps 2, 5, 6 are therefore advisory, and the s10.4 claim "an HTTP actor
cannot grant platform admin without a second Person" has a silent side door whose only detective control (the P1) is
skipped. `TestSeedAdmin_BreakGlass_Subprocess` never runs the default mode while a usable admin exists, so the gap is
untested.

Required: the default create mode must refuse once any `platform_admin` row exists (true first-bootstrap only), or be
folded into `-break-glass` semantics (operator / reason, event, P1); plus a subprocess test of the refusal.
Route to `security` (s11 is its control).

### F2 - MEDIUM: the documented "offboard + re-invite" recovery cannot work for the same mailbox

`UNIQUE NULLS NOT DISTINCT (tenant_id, email)` (migration 0011) + `SF011` (email immutable) + offboarded rows keep
their email (s10.1). `Invite` -> 23505 -> `409 conflict`; `CreateRequest(platform_invite)` -> the
`EXISTS (... tenant_id IS NULL AND email = $1)` pre-check -> `ErrConflict` (it counts offboarded rows too).

Failure scenario: a tenant finance user loses password and device (ADR s6.7 "lost both: offboard and re-invite"; S56-R4
"lost password = offboard + re-invite" because password reset is NOT IMPLEMENTED). The admin offboards them and
re-invites `jane@operator.com`: 409. The person's corporate identity is permanently burned; the only path is a
different email address. The residual S56-R4 states a workaround that does not exist. No test covers re-invite after
offboard. Needs an `architect` decision (e.g. uniqueness only over non-offboarded rows, or record the limitation
honestly in S56-R4 and s6.7).

### F3 - MEDIUM: test coverage of a load-bearing control was removed, contradicting S56-R2

`internal/providercred/lifecycle_integration_test.go:335-340, 462-467`: the `"unlinked"` arms of
`TestPCRequest_RequesterEligibility` and `TestPCApproval_ApproverEligibility` were deleted as "structurally
unreachable" (also the comment on `TestPCFixture_UnlinkedPlatformAdminRefusedBySF014`). But S56-R2 itself says legacy
unlinked approve-capable rows are NOT backfilled or blocked by 0139 and that "the downstream 'no Person' refusals ...
remain load-bearing and are still tested with legacy-shaped rows". For provider credentials that is no longer true:
the approver-unlinked refusal is now untested anywhere, and the requester-unlinked refusal survives only as the scratch
database trigger test (`TestPCApproval_RequesterUnlinkedRefused`), not through the Go eligibility path.

Failure scenario: a regression that drops the Person check from the provider-credential approver eligibility passes
the suite while a pre-0139 unlinked platform admin still exists in an environment. Fix: restore both arms with a
legacy-shaped row built through the owner pool (`staffsv.InsertLegacyUnlinkedStaff`, as the other packages now do).
This is the one s28.4 row I classify as a weakened test rather than a legitimate adaptation (see the s28.4 table below).

### F4 - MEDIUM: destructive fixtures are only safe under `-p 1`, but CI runs packages in parallel

`internal/stafflifecycle/lifecycle_db_integration_test.go:303-316` (`neutralisePlatformAdmins`) irreversibly revokes
the factor of EVERY platform admin in the database except the two it keeps. `.github/workflows/ci.yml:267` runs
`go test -race -tags=integration ./...` with default package parallelism against one database; all of the
implementer's s28.5 evidence is `-p 1` or per-package.

Failure scenario: while `TestLastAdmin_Platform_ConcurrentMutualSuspension` runs, a payments / adjustment / db test
has just planted its "spare enrolled platform admin" (`staffsv.PlantEnrolledPlatformAdmin`, the s28.4 SF020
adaptations) and is about to suspend a platform admin: the spare's factor is revoked in between, `SF020` fires, the
test fails nondeterministically; an httpserver fixture that logs a platform admin in via TOTP can likewise lose its
factor mid-test. S56-R8 records the destructiveness but not this consequence. `InsertLegacyUnlinkedStaff`'s
`ALTER TABLE staff_users DISABLE TRIGGER` also takes ACCESS EXCLUSIVE on `staff_users` until commit, stalling every
other package for that window. Fix: run the last-admin tests on a scratch database (the pattern exists), or confirm
with `qa` that CI integration runs `-p 1`. Route to `qa`.

### F5 - LOW: platform four-eyes approve drops the email delivery outcome

`staff_lifecycle_routes.go:712` `out, _, err = c.svc.Approve(...)`. For `platform_invite` and `platform_mfa_reset` the
approver never learns that the email failed (`Delivery` is only logged). The contract (s"on `failed` use the resend
route") has no resend route for an MFA reset, so a silently undelivered platform reset leaves the target without a
factor until someone notices and files a new four-eyes request. Also `deliver` uses the request context after the
commit: a client disconnect right after commit turns delivery into `failed`. Return `email_delivery` on approve (schema
change, OpenAPI + DTO test) or document it.

### F6 - LOW: the "9.5(b) pair used only by the lifecycle" claim is true today but not pinned

`internal/db/actor_staff_test.go:16-49` pins only the setter definition, by matching the literal
`set_config('app.actor_staff_id'`. A future production caller of `db.SetActorStaffIDForCurrentTx` outside
`internal/stafflifecycle` (making the actor's own platform row lockable in some other tenant-scoped flow, and flipping
SF021 to "platform actor"), or a `SET LOCAL app.actor_staff_id`, passes every test. ADR s28.1 and the ADR 0082 A9
text both assert the "lifecycle only" scope. Add a caller pin (non-test callers: `internal/stafflifecycle/service.go`
only) and widen the regex. Route to `architect`.

### F7 - LOW: K1 re-assertion silently skips when no `ProofActor` is in the context, in production builds too

`internal/capability/proof.go` `reassertActor`: `if !ok { return nil }`. Safe today only because `attachProof` then
cannot sign and the database refuses with AP001. The submit path uses an explicit build-tag split for the same
situation (`submit_reassert_prod.go` refuses in production). Use the same split so the HARD GATE does not depend on an
incidental second check. Surface to `ledger-finance` (A9).

### F8 - LOW: T2 retry is skipped when the lock timeout fires inside the status UPDATE

`actions.go:214-224` `updateStatus` runs `mapDBError`, which turns `55P03` into `ErrBusy` and drops the `PgError`;
`phaseTwo` (`actions.go:339-343`) then cannot recognise it as `errLockTimeout`, so no retry. A timeout on the
`SF020` advisory lock (two platform suspensions racing) answers `lifecycle_busy` on the first attempt. Outcome is still
fail-safe; only the specified single retry is lost. Return the raw error from `updateStatus` or test
`errors.Is(err, ErrBusy)` in `phaseTwo`.

### F9 - LOW: byte-wise truncation of request metadata can produce invalid UTF-8

`service.go:328-333` `truncate` cuts `user_agent` / `ip` / `request_id` at a byte offset. A User-Agent longer than 512
bytes with a multi-byte character across the boundary (or any non-UTF-8 header byte) makes the event INSERT fail with
22021 and the whole lifecycle action answer 503 for that caller. Self-inflicted only; truncate on a rune boundary and
sanitise.

### F10 - LOW: unclassified errors, including actor-proof refusals, answer 503 "temporarily unavailable"

`staff_lifecycle_routes.go:215-217`. AP001-AP005 (expired / replayed proof), 23514, 42501 all surface as an outage and
an ERROR log. Other governed flows map proof refusals to a closed refusal code. Map `AP00x` to a refusal.

### F11 - LOW: test name and ADR wording over-claim

- `TestRequestDigest_MatchesTheDatabaseCanonicalForm` (`model_test.go:81-89`) compares nothing with the database or a
  pinned vector; parity is only exercised implicitly by the HTTP four-eyes tests (ASCII notes). The encodings do match
  (`octet_length` vs `len`), so this is a coverage-claim issue: add a pinned vector including a non-ASCII note.
- ADR s28.2 "Liveness and refresh now compare `staff_version`": refresh re-reads and re-mints at the current `sv` (by
  design, s17.5 "a refresh issues the new role"); it does not compare.

### F12 - LOW: database does not force `target_staff_id IS NULL` on a `platform_invite` INSERT

`staff_lifecycle_requests_shape` admits an invite with a target; Go refuses it, and execution would then hit SF031
(target set only when NULL), so the effect is a request that can never execute. Informational; tighten the CHECK.

### F13 - LOW (efficiency): directory `last_login_at` is a correlated `max()` over `audit_log` per row

`reads.go:22-27`: up to 501 rows, each scanning every audit row of that actor (index `(actor_type, actor_id)` only,
`action` filtered after). Grows with audit volume. Consider an index on `(actor_id, action, created_at)` or a bounded
lateral; not a blocker.

## Questions for domain owners (not findings)

- `ledger-finance`: in `ClaimForDispatch` the KYC-deny (`DenyForCompliance`), KYC-unavailable and destination-refusal
  branches commit before the new `reassertSubmitter` position, so a submitter suspended in flight can still commit a
  compliance denial. Arguably protective; your call whether A9 covers it.
- `security`: S56-D6 / SF021 trust `app.actor_staff_id`, which any runtime SQL can set (R1, recorded). F6 matters more
  because of this.

## Checklist results

| Item | Result |
|---|---|
| Migration 0139 up shape | OK. SF095 pre-flight is precise (status CHECK text, no lifecycle columns, exact 8 RESTRICTIVE fences, no self pair, 0135 `actor_proof_require` body, no Kind, no events table). Triggers fire in name order; all plpgsql, `search_path` pinned, none SECURITY DEFINER; `sf_last_admin` VOLATILE with advisory lock + re-count; `staff_version` only by SF012; column-level UPDATE grant; no DELETE / TRUNCATE on the new tables; `zz_actor_proof_guard` last; SF033 deferred. `actor_proof_require` replacement is the 0135 body plus a closed `staff_lifecycle:*` set (scope platform only). |
| Migration 0139 down | OK. SF094 refuses before any change if invited / offboarded rows, any events / requests / approvals or a break-glass alert exist; the FORCE-RLS lift is inside the refused transaction; restores the 0135 verifier body, the table-level UPDATE grant, the Kind CHECK and the column state. Not re-runnable by itself (no IF EXISTS), which is correct under the runner. |
| Reserved number / ordering | OK. 0139 lands after 0140-0142 / 0151 exist; `MigrateUp` applies every missing version in ascending order and `MigrateDown` rolls back by `applied_at`; 0140-0142 / 0151 touch neither `actor_proof_require` nor `staff_users` (verified by grep). |
| 9.5(b) pair used only by the lifecycle | True today: the single non-test caller is `stafflifecycle/service.go:91` (platform actor on a tenant target). Unpinned: F6. `WITH CHECK (false)` cannot admit an UPDATE (`TestActorSelfPolicyPair_VisibleLockableNeverUpdatable`). |
| Error mapping | OK: one closed code per domain error; SF010 / SF031 -> 409 transition, SF013 -> 403, SF014 -> 422, SF020 / SF021 -> 409, SF032 -> 403, 55P03 -> 409 busy, 23505 -> 409, person FK -> 400, re-assertion -> 401 `session_revoked` + metric. Gaps: F8, F10. |
| Audit / event split | OK: every staff change writes the event and the audit row in one transaction; T1 audit-only (by design, the event is T2's); person-link event-only (the route writes its audit); request / reject / cancel audit-only (no staff change); approve audited by the executed change with the request id. D2 tenant-owned rows are redacted for tenant viewers by both `queryAuditLog` (membership, fail closed) and the events endpoint (verified by `TestLifecycle_TenantIsolation`). |
| Route freeze | OK: 24 `staffRoute` registrations with golden rows (11 tenant, 7 platform-staff, 6 four-eyes); counts 180 -> 203 routes, 110 -> 125 mutating are arithmetically consistent (+24 / +16, -1 / -1); accept is outside `staffRoute` by design (not a staff path for the scanner, credential rate bucket, token-authenticated). |
| OpenAPI parity | OK: mounted == documented in both directions for the 25 lifecycle routes (+ platform person-link); every DTO == schema; no secret-shaped response field; accept body `x-sensitive`. |
| Legacy create route removed | OK: route, handler and OpenAPI operation gone; pinned absent in both directions; `admin_routes_test.go`'s allow-list coverage moved to `TestGrantable_RoleAuthorityMatrix` (equivalent matrix). |
| `cmd/seed-admin` | Person requirement OK; `-break-glass` mode OK in itself; default mode bypass F1. |
| CLAUDE.md: tenant isolation | OK: tenant from the verified token (`canActOnTenant`, then RLS); cross-tenant 404; platform rows invisible to tenant scopes; `Actor` built only from token claims. |
| CLAUDE.md: audit | OK except F1 (default seed-admin path records no operator / reason). |
| CLAUDE.md: no fake completion | OK: labels honest (email MOCK / PROVIDER DEPENDENT, password reset / resolve re-assertion NOT IMPLEMENTED, local-only evidence, NOT MERGED). One inaccurate residual (S56-R4 workaround, F2). |
| CLAUDE.md: secrets | OK: tokens only in the email body; never logged; `RevealForDelivery` allow-listed. |
| Money / ledger | No money arithmetic in this slice. A9 positions are `ledger-finance`'s veto. |
| Constant-time checklist | Not applicable (no webhook scheme package touched). |

## ADR 0119 s28.4 changed assertions: row-by-row

| Row | Classification |
|---|---|
| golden / static route counts | Legitimate (arithmetic verified). |
| `openapi_governed_routes_contract_test` 25 -> 39 | Legitimate (new governed operations). |
| `staff_identity_static_test` DTO allow-list | Legitimate (legacy DTOs removed, lifecycle DTOs added). |
| `staff_mfa_static_test` reveal allow-list + metadata scan | Legitimate; the scan was widened, not narrowed. |
| `admin_routes_test.go` deleted | Legitimate; coverage moved to `TestGrantable_RoleAuthorityMatrix`. |
| stage3d / risk_flow create via invitation | Legitimate; same assertions. |
| step-up withdrawal tests pass the real `sv` | Legitimate. |
| `actor_staff_test` single setter + `sv` cases | Legitimate, but the pin is narrower than the claim (F6). |
| `actor_staff_integration_test` exact pair | Legitimate (stronger). |
| A-18 / KYC fence counts 52 -> 56 | Legitimate. |
| dangling `person_id` on forged platform_admin probes | Legitimate: SF014 is BEFORE, RLS WITH CHECK runs before the FK, so the probe still reaches the 42501 under test. |
| `capability_grant_integration_test` display_name probe; platform-actor suspension fixture | Legitimate: same RLS refusal; fixture forges the GUC deliberately (test-only). |
| spare-admin plants before suspending a platform admin (r12, k2, audit, adjustment, payments S4b) | Legitimate (SF020); flake risk under parallel CI (F4). |
| `identity_integration_test` dangling person | Legitimate (CHECK 23514 still the refusal). |
| A-14b allow-list | Legitimate. |
| SF class / function pins / rls_grants flips | Legitimate (stronger: VOLATILE + single-function pins). |
| status readers / no-trigger-disable exception | Legitimate (integration-tag only, pinned). |
| launch governance / launchgov / assetregistry / casino legacy rows | Legitimate (adds an SF014 assertion, keeps LA012 / LA001 on legacy-shaped rows). |
| `b4_b5` / `k3_gov` / `withdrawal/adversarial` finance -> support for the unlinked actor | Legitimate: the guards under test (K2 MA002, K3 MR032, `withdrawal_approvals_enforce_governance`) check the Person, not the role (verified in 0034 / 0113 / 0115); K3 asserts the exact MR032; the new SF014 assertion on an unlinked finance row is an addition. Pre-existing looseness kept as it was: K2 accepts MA001 / MA002 / MA032 and the withdrawal tests accept any error. |
| `providercred/lifecycle_integration_test` unlinked arms removed | **Weakened (F3).** |
| `providercred/scratch` SF014 disabled for one statement; 0096 scratch gets `staff_version` | Legitimate. |
| `providercred/rls` policy allow-map | Legitimate. |
| `providercred/fixture` `suspended` -> `invited` | Acceptable (predicate is `status = 'active'`); loses the literal "suspended platform admin" case. |
| 0105 / 0106 / assetregistry / casino non-active row `invited` | Acceptable for the same reason. |
| adjustment scratch schema 113 -> 151, temp shadow `staff_version = 1` | Legitimate (the executor reads the column). |
| actorproof backstop / signer allow-list | Legitimate (security review of the signer change is mandatory per s10.4). |
| alerting subjectless set | Legitimate. |
| seed-admin `seedTenantStaff` links a Person | Legitimate. |
| HSEC governed tables 13 -> 15 | Legitimate. |
| liveness / MFA flow / step-up `sv` and counts | Legitimate (version now real; mismatch case is `sv + 1`). |
| `identity_flow` legacy unlinked helper | Legitimate. |
| asset registry / stage3d person-link re-login | Legitimate and stronger (pre-link token now asserted refused). |

## Deviations S56-D1..D14

| # | Assessment |
|---|---|
| D1 `last_login_at` from audit | Acceptable; efficiency note F13. |
| D2 tenant-owned audit / event for a platform actor | Acceptable: both tenant read paths redact the platform actor (verified). Differs from s10.6's `SubjectTenantID` wording; `architect` to record. |
| D3 invite / resend carry no reason | Acceptable (s10.1 lists reasons for removals / restorations). |
| D4 platform resend / revoke routes | Acceptable (protective / non-escalating; the email is immutable). |
| D5 column grant includes `person_id`, `display_name`, `updated_at` | Acceptable: these are the only other runtime UPDATEs of `staff_users` (grep verified); `staff_version` excluded. |
| D6 SF021 treats a platform `app.actor_staff_id` as platform-scoped | Acceptable as an accident guard; GUC forgeable (R1); see F6. |
| D7 SF013 exempts invited -> active | Acceptable. |
| D8 S3 paths write no `mfa_enrolled` events | Acceptable, recorded. |
| D9 empty link base | Acceptable (deployment config). |
| D10 `refused_at_execution` / `expired` never written | Acceptable; requests read as `expired` by evaluation. |
| D11 integration-only empty-role skip | Acceptable; production refuses (pinned). Contrast F7. |
| D12 K1 approve / revoke only | Acceptable (protective reject / cancel). |
| D13 trailer attribution | Acceptable; this review commit also uses the session's attribution rule. |
| D14 reject / cancel class none | Acceptable (protective; the DB still requires a proof and SF032 / requester-only cancel). |
