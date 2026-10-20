# Runbook: per-tenant withdrawal restriction policy (lic11, migration 0182)

Status: IMPLEMENTED against MOCK providers and a local database (integration tests written; run by the orchestrator). Not a
legal or regulatory authorisation: a configured policy is a decision recorded by named staff. Detail: `docs/plans/lic11-withdrawal-restriction-record.md`;
code-derived behaviour matrix: `docs/plans/lic11-withdrawal-restriction-analysis.md`.

## 1. What it governs (and what it does not)

| Cause | Governed by this policy? | Behaviour with NO policy row (interim engineering default) |
|---|---|---|
| `licence_suspended`, `licence_expired` (status expired or expiry date passed), `licence_not_yet_issued`, `licence_missing` (no licence bound / row unreadable) | YES (configurable) | allow at every stage: request, payout claim and re-dispatch proceed (exactly the behaviour before 0182). Pending owner decision Q12. |
| `tenant_suspended`, `tenant_closed`, `brand_suspended`, `brand_closed` | NO: owner decision ADR 0095 section 44 decisions 13-18 | request refused; payout claim refused (request stays `approved`, hold frozen, four-eyes governed release only); tenant: re-dispatch deferred until reactivation |
| `market_closed` (player's country closed for the tenant) | NO: not evaluable (no per-withdrawal market determination, Q18 / Q19) | not checked on any withdrawal path |

HARD restrictions are never affected by any policy row: account / wallet status (WD050 / WD052), KYC and AML / sanctions
holds, risk DENY / REVIEW (WD051), payout kill switch, provider capability, payout instrument. A policy can only ADD a
refusal; `allow` never skips another gate.

## 2. Actions per stage

| Stage | allow | hold_for_review | block |
|---|---|---|---|
| request (new withdrawal) | proceed | request ACCEPTED (hold placed), audit `withdrawal.held_for_restriction_review` | refused: player sees the generic "not available" (403), denial audit `withdrawal.denied_by_restriction_policy` (WD056), no row, no hold, key not consumed |
| dispatch (staff submit, T1p) | proceed (WD-RG-1, KYC, routing, destination as before) | PARK: request stays `approved` with its hold; denying `withdrawal_gate_decisions` row WD056; alert `withdrawal.payout_gate_denied` (`gate_code` WD056); staff gets 409 "held under the tenant's withdrawal restriction policy" | same as hold_for_review (the action is recorded) |
| redispatch (sweeper T2 / T12) | proceed | ESCALATE the attempt (T16: no resend, no release), WD056 row, system audit, alert | same |
| settlement (callbacks, poll, completion, decline, reversal) | never gated: the platform records the provider's truth | | |

"Review" means a human uses the EXISTING exits: reject the parked request (releases the hold back to the player's cash),
wait for the licence to be reinstated (the next submit / tick re-evaluates), or loosen the policy through the governed change
below. There is no per-withdrawal override (none exists, none is invented). Note (SF4-3, unchanged): a T2 / T12 escalation is
not a latch: once the cause clears or the policy loosens, the next sweeper tick re-runs the gate and may resend.

## 3. Reading the policy (operators, tenants)

- `GET /v1/admin/tenants/{tenantID}/withdrawal-restriction-policy` (platform_admin any tenant; tenant_admin / compliance their
  own): every cause, per stage action and `source` (`configured` | `default_unconfigured`), `set_version` (0 = nothing
  configured) and, for each unconfigured cause, the UNCONFIGURED notice. Treat any `default_unconfigured` licence cause on a
  tenant going live as an OPEN ITEM until the owner answers Q12.
- `GET /v1/admin/tenants/{tenantID}/withdrawal-restriction-policy/versions`: append-only history.
- Every withdrawal decision records its provenance: `withdrawal.requested` audit metadata (`restriction_policy_source`,
  `restriction_policy_set_version`, `restriction_cause`, `restriction_licence_state`) and the
  `withdrawal_gate_decisions.restriction_*` columns at dispatch / redispatch. To replay a decision, read the versions up to
  the recorded `set_version`.

## 4. Changing the policy (platform_admin only)

1. `POST /v1/admin/withdrawal-restriction-policy-changes` `{tenant_id, cause, action, reason_code, note?}` (step-up: critical).
2. The database classifies it. TIGHTENING or equal (allow -> hold_for_review -> block, at every stage): single actor, executes
   in the same call (`executed: true`). LOOSENING (any stage less strict, e.g. block -> allow, block -> hold_for_review):
   `required_approvals: 2`.
3. Two OTHER platform_admins, distinct Persons, neither the requester's Person: `POST .../{changeID}/approve`
   `{reason_code}` (step-up: critical). The second approval executes it. Approvals older than 24 h do not count; a change
   expires after 72 h. `reject` (any eligible approver) and `cancel` (requester) are protective (no step-up).
4. Execution takes the tenant status gate key EXCLUSIVE: in-flight withdrawal requests / payout claims of that tenant finish
   first (or wait a moment). Expect a short wait, not an error; `..._RETRYABLE_LOCK_TIMEOUT` means retry.

Refusals: `..._NOT_CONFIGURABLE` (tenant / brand / market cause), `..._DUPLICATE_PENDING` (an open change for the same tenant and
cause), `..._SELF_APPROVAL`, `..._ALREADY_DECIDED`, `..._APPROVALS_STALE`, `..._PROOF_REFUSED` (actor-proof key problem, see
`operational-runbooks.md` section 15), `..._GUARD_REFUSED` (a WR0xx guard: treat as a defect or an attack), `..._UNAVAILABLE`.

## 5. Symptoms

| Symptom | Meaning | Action |
|---|---|---|
| Player withdrawals refused (403) and `withdrawal.denied_by_restriction_policy` audit rows | a configured `block` and the licence cause in force | Confirm the licence state (licence registry) and the policy version; this is the configured behaviour. |
| Alert `withdrawal.payout_gate_denied` with `gate_code` WD056 | a payout parked / escalated by a configured hold / block | Review: reject (funds back to the player's cash), wait for reinstatement, or file a loosening change. |
| Withdrawals or payouts fail closed with 503 / "gate unavailable" right after deploy | the restriction evaluation could not read the tenant / licence / policy (a mis-scoped session or a broken database) | Check logs for `restriction policy:`; nothing was written or sent. |
| SQLSTATE WR040 in logs | a runtime write to `withdrawal_restriction_policies` outside an executed change | A defect or an attack; nothing was written. Preserve logs. |
| SQLSTATE WR002 | a write for a non-configurable cause | Refused by design (owner decision 13-18 / not evaluable). |

## 6. Rollback

`migrations/0182...down.sql` is refused (WR095) once any policy row, change request or provenance-bearing gate decision
exists (governance and compliance history). Dev / staging only.
