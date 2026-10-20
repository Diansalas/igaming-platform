# Runbook: per-tenant withdrawal restriction policy (lic11 migration 0182, lic17 migration 0184)

Status: IMPLEMENTED against MOCK providers and a local database (integration tests written and run). Not a
legal or regulatory authorisation: a configured policy is a decision recorded by named staff. Detail: `docs/plans/lic11-withdrawal-restriction-record.md`,
`docs/plans/lic17-record.md` (fixes after the independent security and ledger-finance reviews);
code-derived behaviour matrix: `docs/plans/lic11-withdrawal-restriction-analysis.md`.

## 1. What it governs (and what it does not)

| Cause | Governed by this policy? | Behaviour with NO policy row (interim engineering default) |
|---|---|---|
| `licence_suspended`, `licence_expired` (status expired or expiry date passed), `licence_not_yet_issued` (of ANY licence the tenant relies on, see 1a), `licence_missing` (the tenant has NO licence and NO approved operating basis) | YES (configurable) | allow at every stage: request, payout claim and re-dispatch proceed (exactly the behaviour before 0182). Pending owner decision Q12. |
| `tenant_suspended`, `tenant_closed`, `brand_suspended`, `brand_closed` | NO: owner decision ADR 0095 section 44 decisions 13-18 | request refused; payout claim refused (request stays `approved`, hold frozen, four-eyes governed release only); tenant: re-dispatch deferred until reactivation |
| `market_closed` (player's country closed for the tenant) | NO: not evaluable (no per-withdrawal market determination, Q18 / Q19) | not checked on any withdrawal path |

### 1a. Which licences are evaluated (lic17 F-1)

Every licence the tenant (and the withdrawal's brand) RELIES ON: `tenants.licence_id` plus the licence of every approved
(executed licensing change), active, in-force `operating_bases` row of the tenant, tenant-wide or of that brand
(`platform_licence` and `partner_licence` bases). Another brand's partner licence is not relied on. Each licence is evaluated
with the single `EvaluateLicenceValidity`; every cause in force is listed and the STRICTEST configured action decides (a
configured source wins a tie). A documented no-licence basis (`no_licence_documented`) contributes no licence cause and never
makes the tenant `licence_missing`; `licence_missing` remains only for a tenant with no licence and no approved basis.

### 1b. A licence that cannot be read: `unknown` (lic17 F-4 / M-2)

If a relied licence cannot be evaluated (the tenant row or the licence is not visible in the session, i.e. a LicensingInvalid
chain or LicenceNotFound; a read error; a validity value the code does not map), that is NOT `licence_missing`:
- tenant with NO policy row (set version 0): the decision is the interim default `allow`, recorded with source
  `default_unconfigured` and `restriction_licence_state = unknown` (no cause). This is the pre-0182 behaviour, which never read
  the licence; it is not a statement that the licence is valid.
- tenant WITH a policy row: FAIL CLOSED. The request / claim / tick fails as "gate unavailable" (503 / retried), nothing
  is written or sent. Fix the read (scope, data), never the policy.

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

**`hold_for_review` is a PARK, not a review queue (lic17 M-1).** Nothing in this policy releases a parked withdrawal. The
amount stays on the player's `player_withdrawal_hold` (not forfeited, not paid; the ledger is unchanged by a park). The ONLY
exits are:
1. the cause clears (licence reinstated / in force again): the next staff submit (T1p) or sweeper tick (T2 / T12) re-runs
   the gate and proceeds;
2. the policy is widened through the governed change of section 4 (two distinct approvers); it applies to the whole tenant
   and cause, not to one withdrawal;
3. for a NON-ACTIVE tenant or brand only: the governed four-eyes `release_hold_to_player` (ADR 0111, `launch-and-suspension.md`)
   returns an `approved` request's hold to the player's cash;
4. for an escalated, possibly SENT (T12, ambiguous) attempt: the payment force-resolution flow (`operational-runbooks.md`
   section 14).

There is NO release-to-player for an `approved` request of an ACTIVE tenant (a staff `reject` applies to `pending_review`
only; the lic11 text that said "reject the parked request" was wrong) and none for a never-sent escalated attempt (M3 has no
caller, `operational-runbooks.md` section 12 item 7). A per-withdrawal governed release is an OWNER decision (who may release,
on what evidence, whether to the player or to dispatch): residual R-M1 in `docs/plans/lic17-record.md`. Do not edit rows.
Note (SF4-3, unchanged): a T2 / T12 escalation is not a latch: once the cause clears or the policy loosens, the next sweeper
tick re-runs the gate and may resend.

## 3. Reading the policy (operators, tenants)

- `GET /v1/admin/tenants/{tenantID}/withdrawal-restriction-policy` (platform_admin any tenant; tenant_admin / compliance their
  own): every cause, per stage action and `source` (`configured` | `default_unconfigured`), `set_version` (0 = nothing
  configured) and, for each unconfigured cause, the UNCONFIGURED notice. Treat any `default_unconfigured` licence cause on a
  tenant going live as an OPEN ITEM until the owner answers Q12.
- The same read returns `action_semantics` (the plain meaning of each action, closed text) and `parked`: for a tenant
  caller, the count of open (`approved` / `submitted`) withdrawals whose latest dispatch gate decision is a WD056 refusal,
  `oldest_parked_since` (the earliest WD056 refusal among them) and `oldest_age_seconds`; for a platform caller
  `visible: false` (the decision rows are tenant-scoped) with a pointer to the alerts.
- `GET /v1/admin/tenants/{tenantID}/withdrawal-restriction-policy/versions`: append-only history.
- Every withdrawal decision records its provenance: `withdrawal.requested` audit metadata (`restriction_policy_source`,
  `restriction_policy_set_version`, `restriction_cause`, `restriction_licence_state`) and the
  `withdrawal_gate_decisions.restriction_*` columns at dispatch / redispatch. To replay a decision, read the versions up to
  the recorded `set_version`.

## 4. Changing the policy (platform_admin only)

1. `POST /v1/admin/withdrawal-restriction-policy-changes` `{tenant_id, cause, action, reason_code, note?}` (step-up: critical).
2. The database classifies it. TIGHTENING or equal (allow -> hold_for_review -> block, at every stage): single actor, executes
   in the same call (`executed: true`). LOOSENING (any stage less strict, e.g. block -> allow, block -> hold_for_review) AND,
   since 0184 (lic17 F-2), any change to `allow` while the cause is still on the interim default (`default_unconfigured`):
   `required_approvals: 2`. Re-pinning an already CONFIGURED `allow` stays single actor.
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
| Alert `withdrawal.payout_gate_denied` with `gate_code` WD056 | a payout parked / escalated by a configured hold / block (one open alert per withdrawal / attempt; `first_seen_at` = park start) | Confirm the cause. Exits: section 2 (cause clears, governed widening; release only for a non-active tenant / brand). There is no per-withdrawal release. |
| Parked too long: `parked.oldest_age_seconds` (tenant read) or the WD056 alert's `first_seen_at` older than N | funds of a player held with no exit in progress | ENGINEERING DEFAULT N = 72 h (not a legal or regulatory figure; the owner sets it). Escalate to the tenant's compliance owner and the business owner (R-M1). No automatic age alert exists yet (residual). |
| SQLSTATE WR003 in logs, withdrawals / payouts "gate unavailable" | lic17 M-3: the restriction functions were called from a mis-scoped session (player or acting session, tenant + platform principal together, another tenant's session, no scope). Refused so a configured block can never read as "nothing configured" | A defect in the calling code; nothing was written or sent. |
| Withdrawals or payouts fail closed with 503 / "gate unavailable" | the restriction evaluation could not read the tenant / a relied licence / the policy while the tenant HAS a policy row (section 1b), or a mis-scoped session (WR003) | Check logs for `restriction policy:`; nothing was written or sent. A tenant with no policy row does not fail here (recorded `unknown`). |
| `restriction_licence_state = unknown` in `withdrawal.requested` audit metadata | section 1b: a relied licence could not be read and the tenant has no policy row; the default allowed | Investigate the visibility of the tenant / licence rows. Configure a policy only after the read is fixed (with a row, the same read fails closed). |
| SQLSTATE WR040 in logs | a runtime write to `withdrawal_restriction_policies` outside an executed change | A defect or an attack; nothing was written. Preserve logs. |
| SQLSTATE WR002 | a write for a non-configurable cause | Refused by design (owner decision 13-18 / not evaluable). |

## 6. Rollback

`migrations/0184...down.sql` restores the 0182 function bodies byte for byte (no data, no guard). Rolling it back re-opens
F-2 (a single actor could pin the first `allow`) and M-3 (a mis-scoped session reads the default). Dev / staging only.

`migrations/0182...down.sql` is refused (WR095) once any policy row, change request or provenance-bearing gate decision
exists (governance and compliance history). Dev / staging only.
