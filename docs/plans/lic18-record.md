# lic18 record: AML gate hardening (ledger-finance H-1) and the lic10 security addendum

Branch `lic18-aml-gate-fixes` from integ-b `5503334` (lic9 + lic10 + lic11 + lic14, migration head 0183). Migration **0181 was edited in place** (number kept, down kept consistent; lic17 uses 0184; no 0185). Software capability only: no regulatory approval is claimed and no real screening vendor exists. The only provider is the MOCK. Grace, re-screen and alert thresholds stay configuration; the platform ships none.

Tip commit and the exact commands/results are in the final report; section 8 lists what was run. Nothing is claimed that was not seen.

## 1. Findings addressed

| Finding | Change | Label |
|---|---|---|
| H-1 (a) gate with no worker | A withdrawal gate cannot be authored unless a screening worker has reported a heartbeat within 10 minutes (`screening_worker_heartbeat`, table `screening_worker_heartbeats`, runtime has no table privilege). The production binary does not start the worker (`cmd/platform-api/main.go`), so the gate cannot be enabled in production. | IMPLEMENTED (MOCK worker only) |
| H-1 (b) scheduled event | CHECK `NOT gate_withdrawal OR 'scheduled' = ANY(events)`; the service and the HTTP route refuse earlier. | IMPLEMENTED |
| H-1 (c) officer path over HTTP | 4 routes under `/v1/admin/tenants/{tenantID}/screening/` (not `/aml/`: the A7 pins count exactly the eleven `/aml/` routes): GET/POST `trigger-policy`, GET `persons/{personID}/requests`, POST `persons/{personID}/requests/{requestID}/resolution`. Writes are step-up `critical` behind `aml:hold:manage`; reads are step-up none behind `aml:case:read`. The database re-checks the officer grant, quorum and the signed actor proof (`aml_authorize`). Golden table (+4), staff route pins (263 routes / 156 mutating), audit allow-list entries, OpenAPI and a route test file were updated. Back Office pages are out of scope. | IMPLEMENTED (routes), NOT IMPLEMENTED (Back Office) |
| H-1 (d) / L3 versions | `effective_from` may not be more than 2 minutes in the past or 30 days ahead; the gate baseline (grace and clears) is the FIRST gating version of the jurisdiction, so a new version neither re-opens the grace nor discards a valid clear. | IMPLEMENTED |
| H-1 (e) park at T1p | `HoldVerdict.ScreeningOnly` (held by the screening gate, probe held, no officer hold row). `RetryableOnHoldEvalFailure` turns it into the existing retryable `unavailable` outcome (code `kyc_unavailable:screening`): the request stays approved, the funds hold stays, no attempt. An officer-placed hold still rejects through `DenyForCompliance`. | IMPLEMENTED |
| H-1 (f) trapped alert | Policy field `gate_alert_after_seconds` (60 s to 30 d, required with the gate); alert kind `aml.screening_gate_trapped` (p1, subject tenant, attribute `persons` = a count); `aml_screening_gate_overdue` and a sweep in the worker pass. | IMPLEMENTED (alert delivery is ALERT-DELIVERY-1 OPEN: nobody is paged unless a route exists) |
| S1 binding | `aml_screening_record` binds an idempotency key that belongs to a screening request to that request's person and trigger; an `error` can never resolve a `needs_officer` request; a sanctions-class (confirmed, or sanctions in the match lists) request can only be resolved by recording a sanctions match (hold, lifted only by four-eyes), never by a clear; the officer quorum is required for any resolution of a `needs_officer` request. The gate counts a clear only if it is newer than the latest match (database clock `recorded_at`/`completed_at`). | IMPLEMENTED |
| S2 provider | A worker `finish` with a clear needs a provider reference and a provider that holds a fresh heartbeat. Pre-real-provider requirements are in section 6. | PARTIALLY IMPLEMENTED (MOCK) |
| S3 residence | AFTER UPDATE guard on `kyc_verifications`: residence columns change only as a NEW, later determination (`set_at` advances); the check row now carries the country and the evidence read excludes a check whose country differs from the verification's. The lic10 record claim "mode `all` has no application dependence" is corrected (see section 7). | IMPLEMENTED |
| S4 KYC policy four-eyes | Person-distinct check (activator vs author, and for supersede both authors) before the transition; refusal is 403 and writes nothing. Signed proof NOT built (section 5). | PARTIALLY IMPLEMENTED |
| L1 | Unresolved `needs_officer` is checked BEFORE any policy lookup, so a new version with the gate off, a licence rebind or removal cannot release a match. | IMPLEMENTED |
| L2 | The gate loops over every jurisdiction whose latest effective trigger policy gates; it no longer depends on the tenant's licence being readable. Jurisdictions for authoring resolve through the licence and the approved, open operating bases (`aml_screening_jurisdictions`). | IMPLEMENTED |
| L4 | `residence_determination_approve` requires the compliance role and excludes the subject Person. | IMPLEMENTED |
| L6 | KYC enforcement policy audit rows carry an `after` object (trigger, status, threshold, asset, tier, play operation, successor, legal reference present). | IMPLEMENTED |
| L7 | Default claim batch 4; `ValidateForLoop` refuses a lease shorter than batch x (3 x phase + call) + 5 s; a `failed_terminal` scheduled request no longer blocks the next bucket's re-screen. | IMPLEMENTED |
| Info | A pending residence-policy loosening expires after 72 hours (must be refiled). | IMPLEMENTED (static pin only, see section 8) |
| L5 | Accepted by the review; no change. | n/a |

## 2. Design notes

- **Parking is a deliberate behaviour change.** Before, any hold at the payout claim rejected the withdrawal and released the funds to cash. A hold that exists only because a required screening is outstanding is a waiting state, not a finding: it now parks, and the trapped alert makes a long wait visible. A real officer hold and a KYC failure still reject.
- **The 10-minute heartbeat bound is an engineering value**, not a compliance value. The heartbeat is proof-less and database-wide (the worker is not an officer); a holder of the runtime credential could forge one (recorded residual, section 7).
- **Officer results are timed by the database clock** (`recorded_at`), never by the officer's `screened_at`.
- **Down migration**: restores the 0155 `aml_screening_record`, drops the new functions, table and guard, deletes both alert kinds, and refuses while an alert of either kind exists.

## 3. Operator runbook (MOCK environments only)

**Never enable a withdrawal gate in production.** No real screening provider exists; the production binary does not start the worker; a gate there would hold every new withdrawal with nothing able to clear it. The heartbeat requirement refuses it in an environment with no worker, but a refusal is a safeguard, not permission.

How to enable (dev/staging with the MOCK worker running): author a screening policy for the jurisdiction (existing route family), then POST `.../screening/trigger-policy` with `events` including `scheduled`, `gate_withdrawal: true`, `gate_alert_after_seconds`, `effective_from` inside the window, a legal review reference and a reason code. Needs a platform_acting AML officer, the officer quorum and a signed critical-step-up proof.

How to clear a stuck person: GET `.../screening/persons/{personID}/requests`; for a request in `needs_officer` or `failed_terminal` with `resolved=false`, POST `.../requests/{requestID}/resolution` with `outcome` `clear` (not allowed for a sanctions-class match), or `potential_match` / `confirmed` with `match_lists` to record a match and place the hold. An `error` outcome is refused. Replays are idempotent.

How to disable: POST a new trigger-policy version with `gate_withdrawal: false` and empty `events` (inside the effective_from window). Persons with an UNRESOLVED system-found match stay held (by design); resolve them through the officer route. Do not edit tables.

How to read the trapped alert: `aml.screening_gate_trapped` carries a count only. Find persons with the requests route; resolve or fix the provider. A dead worker cannot raise it.

Known limitation: a sanctions FALSE POSITIVE cannot be cleared by a single officer, and the scheduled re-screen flags the person again. The only lever is a policy version with the gate off (unresolved matches still hold). A legal/product decision on false-positive handling is needed before any real provider.

## 4. Pins changed (declared)

- `TestScreeningWorker_Static_Boundaries`: the phase C check looked for the FIRST `alerting.InTx(`; the heartbeat and trapped sweep now also use it, so it checks for an `InTx` AFTER the provider call. Same intent.
- `TestSetTriggerPolicyInput_Validate`: the valid base input now carries the scheduled event and an alert threshold (new rules); new refusal cases added.
- Golden route table +4 rows; `staffRouteGolden` pin 259/154 to 263/156; mutation audit allow-list +2 entries.
- `TestMigration0181_SeedRoundTripAndDownRefusal`: 5 tables to 6, new down-refusal message, `gate_trapped` seed assertions; one new test for the second kind.
- `k2OwnerOnlyProofTables` (internal/db K2-G1 dynamic probe) gained the three residence tables and `screening_worker_heartbeats`: they are RLS-enabled and not FORCEd (owner-written through SECURITY DEFINER functions), so the owner-run probe sees their rows; the runtime role cannot (heartbeat and two residence tables: no privilege; checks: tenant-RLS SELECT only). The lic10 slice never ran `./internal/db`.
- Catalog test lists: `aml_screening_gate_overdue` is an API function and proof-less (session-tenant bound).
- Existing aml gate fixtures (`setTrigger`) now supply the scheduled event, an alert threshold and a worker heartbeat; `TestGate_ProviderUnavailable_FailsClosedForWithdrawal` now accepts the extra scheduled request (all requests must be `failed_terminal`).
- `TestQueue_ClaimTokenCAS...`: the clear now carries a provider reference and a heartbeat.
- `TestKYCEnforcementPolicyAPI_DraftWithdraw...` (a lic10 test never run before): it asserted that the author could withdraw their own draft; migration 0100 requires a different principal for every withdrawal. The test now asserts 403 for the author and 200 for another admin.
- lic10 docs and the 0181 header comment: the "`all` has no application dependence" claim corrected.

## 5. S4 conflict report and proposal (STOP condition honored)

The brief: KYC policy four-eyes with a signed proof and Person-distinct. Person-distinct is built (section 1). The signed proof is **not** built: platform-scoped KYC policy writes have no matching verifier operation. `actor_proof_require` scopes platform operations to specific operation prefixes and I was told not to edit it. Proposal for the owner/architect: add a platform-scope operation family `kyc_enforcement_policy:{create,activate,withdraw,supersede}` (critical class) to the proof verifier registry and `actorproof.StepUpOperations`, then have the four handlers attach a proof bound to the policy id and reason code, mirroring the residence policy approvals. Until then the controls are: step-up critical on the route, DB principal-distinct four-eyes (migration 0100), Person-distinct (this slice, only where both staff accounts have a Person link; `staff_users.person_id` is optional), and the audit trail with request evidence.

## 6. Hard prerequisites before ANY real screening provider (PROVIDER DEPENDENT)

1. An authenticated worker identity (system-actor attestation, ADR 0116 F1) for heartbeat, claim and finish; today these are proof-less runtime calls.
2. A per-tenant provider id allow-list in configuration, checked in `finish` and `heartbeat` (today: freshness of a self-reported heartbeat only).
3. Vendor result authenticity (signed callback or verified fetch) and a provider reference that is verified, not merely present.
4. A real per-tenant subject key resolver (today `MockSubjectKeys`) and a data-protection review of what the vendor sees.
5. A legal decision on false-positive handling for sanctions (section 3) and on grace/re-screen values.
6. A configured alert route (ALERT-DELIVERY-1) so `aml.screening_gate_trapped` pages someone.
7. Removal of the production guard in `cmd/platform-api/main.go` only after 1 to 6, as a recorded decision.

## 7. Residuals

- Heartbeat/claim/finish are proof-less; a holder of the runtime credential with a tenant session could forge a heartbeat or finish a claimed request (prerequisite 1).
- A dead worker cannot raise the trapped alert.
- Residence: a tenant session cannot prove `verified_residence_set_by` is the reviewer. Mode `all` still depends on the reviewer path being the only writer and on market consumers reading only `ReadMarketResidenceEvidence` (`GetVerifiedResidence` is unfiltered and has no production caller).
- The `init-app-role.sql` changes (new signatures, heartbeat table revoke) were not executed (the file alters roles; not run in this environment).
- The 72-hour expiry of a pending residence-policy loosening is pinned statically only; the guard triggers do not allow staging a backdated request without altering shared database objects.
- `staff_users.person_id` is optional; Person-distinct cannot compare when a link is absent.
- Parking removes the "funds return to cash" outcome for screening-only holds: a person can wait for as long as the screening stays unresolved; visibility depends on the alert route.

## 8. What was run

Commands (worktree `lic18`, private scratch database rebuilt from the final migration set with `priv_db.sh`, tests via `priv_test.sh -tags integration -count=1 -p 1`):

- `gofmt -l cmd internal` (clean), `go vet ./...` and `go vet -tags integration ./internal/... ./cmd/...` (clean), `go test ./...` (all pass).
- Integration, full package, PASS: `./internal/aml`, `./internal/kyc`, `./internal/alerting`, `./internal/db`, `./internal/launchgov`, `./internal/marketgate`.
- `./internal/httpserver` full run: one failure, `TestStepUp_GoldenTable_EveryClassifiedRoute` (count pins; fixed as declared in section 4); that test plus the KYC policy, screening and residence policy API tests re-run and pass. The full package was not re-run after the pin fix (a full run takes about 21 minutes).
- `./internal/payments`: the full run panics in `TestLic11_T1p_ConfiguredHoldOrBlock_Parks` (provider id `l11-park-hold_for_review` fails the webhook provider-id charset), a lic11 test fixture defect untouched by this slice; the panic aborts the package, so the rest of the package was not completed. The AML hold tests (`-run AMLHold`, including the new screening-only parking test) pass.
- `./internal/withdrawal`: 5 failures, all `withdrawal_restriction_set_version(unknown) does not exist` in migration up/down tests that build a scratch database at an older version (0132, 0162, 0165, 0169, 0171) while the code calls lic11's 0182 function. Not touched by this slice; NOT verified against the base commit.

Mutation checks performed against the live scratch database (each killed by the named test unless stated): L4 role check removed (`L4_ApprovalNeedsTheComplianceRole`), key binding removed (`KeyBinding_AndErrorNeverResolves`; the first draft of this test SURVIVED the mutation because the sanctions rule refused for another reason, and was rewritten around a PEP match), `error` check removed, sanctions clear guard removed, gate baseline min to max (`NewVersionDoesNotResetGraceOrClears`), unresolved-match block removed (`UnresolvedMatchHoldsEvenAfterTheGateIsSwitchedOff`), worker-availability check removed (`TriggerPolicy_Authoring`), provider reference check removed (`FinishRefuses...`), residence guard (the first-IF-only mutant is equivalent because the second IF raises the same code; both removed is killed by `S3_...`), `ScreeningOnly` branch removed (`PayoutClaim_ScreeningOnlyHold_ParksNotRejects`), Person-distinct check removed (`PersonDistinctFourEyes_AndAuditAfterState`).
