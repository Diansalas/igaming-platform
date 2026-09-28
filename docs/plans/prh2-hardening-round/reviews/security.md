# Security review — PRH-2 plan (2026-09-28)

Reviewer: `security`, read-only. Plan at `a3547f5`; claims checked against code with grep/read. The ADR 0098 decisions are treated as settled.

**Verdict: APPROVE WITH CONDITIONS.**
- **Can proceed** (subject to §3): W0 and the provider-independent lanes A, C, D, E2, F, G1, H, I-core and J, then B after A.
- **Blocked:** K1, K2 and K3, until the High findings S-1 and S-2 are resolved in ADRs 0099/0100 and HD-PRH2-1, -2, -6 and the new -7 are answered.
- This reviews the plan, not the implementation; per-workstream security review is still required.

## 1. Verified claims (at `a3547f5`)

- **A**
  - 0042:58-60 refuses every change out of `consumed`, `expired` and `revoked`; 0042 holds the latest trigger body.
  - `launch.go:377-383` revokes only `active` sessions.
  - Plan §4 row 0108 and §5-A carry security's FH-7 spec verbatim: trigger text, `FOR UPDATE`, `IN ('active','consumed')`, `prior_status`, the test matrix, all four mutants.
- **B:** `ResolveLaunchToken` (`launch.go:279`) has no non-test caller.
- **C:** `drive.go:250-273` does not validate the reference; payout does (`payout.go:392-395`).
- **D:** a poll success posts without comparing (`sweeper.go:~497-513`); the only check is in the already-succeeded branch (`:471`).
- **G1**
  - `audit_log` has a single `FOR ALL` dual-scope policy (0014:32-40).
  - Append-only is enforced by the UPDATE/DELETE trigger (0014:43-53) and the TRUNCATE trigger (0016:14-16).
  - `auditTenantID()` returns `uuid.Nil` for platform sessions (`payments_kill_switch_handlers.go:245-250`).
  - A permissive `FOR SELECT` policy only ORs into reads; write-side RLS is unchanged.
  - The existing tenant-context audit readers filter `tenant_id = $1` explicitly (`admin_routes.go:1167`, `kyc/provider.go:672`, `noeffect.go:81,197`).
- **K**
  - The role→permission map is static (`permission.go:587+`), and `RequirePermission` reads the role from the JWT (`:899-914`).
  - Access tokens are not revocable by jti (`jwt.go:98-102`) and live 15 minutes (`config.go:448`); role and status are re-checked only on refresh (`auth_routes.go:346-349`).
  - `tenant_admin` holds `PermStaffManage` but no withdrawal authority (`permission.go:651-674`).

## 2. Findings

| ID | Sev | Plan § | Evidence | Required change |
|---|---|---|---|---|
| **S-1** | **High** | K1, §7 HD-PRH2-2, §9 | **Distinct-Person does not stop sock puppets today.** A tenant caller may create `tenant_admin` and `support` staff; only `finance`, `risk_manager`, `promotions_manager` and `bonus_operations` are restricted to platform callers (`admin_routes.go:491,505-535`). The creator chooses the password and `person_id` (`:492-499,550`), or links one later (`:656`, `identity/staff_user.go:162`). Persons are platform-wide and unverified (0009:13-19), and a new one is minted per NO_MATCH player registration (`player_account.go:108-113`), so one human can obtain any number of "distinct" Persons. 0029:12-17 calls staff person-linking optional. A tenant admin holding `capability_grant:manage` can therefore mint two accounts linked to self-registered Persons and grant them initiate and approve; a second minted tenant admin also satisfies option (b). The plan's §9 mitigation overstates this. | 1. ADR 0099 states plainly that under the unverified identity model, distinct-Person or distinct-principal checks do not prove two humans. 2. Do not implement K1 on HD-PRH2-2 (a), or on (b) with tenant-local approval only, unless grantees are restricted to platform-minted accounts (the Stage 3D `finance` precedent) or platform co-approval is required (c). 3. Re-frame HD-PRH2-2 (§4). 4. Keep the distinct-Person trigger as defence in depth; if a person link is a precondition, define what makes it trustworthy (`persons.status='verified'` means something only once KYC is real). |
| **S-2** | **High** | K2 policy model | **Who may author `financial_approval_policies` is unspecified, and only a jurisdiction row is a floor.** A tenant principal able to insert a tenant row could loosen approvals with no second person: `above_threshold` with a huge threshold, or `required_approvals=1` with `distinct_principal`, which S-1 makes meaningless. `legal_review_reference` is free text. Precedent: `bonus_approval_policies` (tenant-staff insert, 0063:79-84; insert-only, :56). | ADR 0100 specifies: (i) the permission and principal scope allowed to insert policy rows at each level; (ii) that every less-specific row, platform default included, is a floor, enforced in the DB resolver or trigger, not only in Go; (iii) that a policy change is itself four-eyes and audited, and cannot be made by the same Person who initiates under it; (iv) that a loosening row fails closed at DB level. Human input: new HD-PRH2-7. |
| S-3 | Medium | K1/K2/K3, HD-PRH2-6 | No session shape lets a platform principal act inside one tenant's rows. Ledger rows are tenant-RLS; `WithPlatformAdmin` never sets `app.tenant_id` (`db/tenant_rls.go:112-133`); 0105's resolver raises on mixed platform+tenant sessions (0105:27-50). Platform grants reaching tenant ledgers (HD-PRH2-6 a/c) need a new "platform principal acting in tenant X" RLS family, the same kind of power G1 rejected for audit. | ADR 0099/0100 designs that family explicitly (validated platform GUC plus target tenant, trigger-validated, audited with both, only on the named K tables and the ledger post), or limits platform scope to platform-owned objects until HD-PRH2-6 is answered. Threat model required before K1 code. |
| S-4 | Medium | K1/K2 | Grant revocation takes effect immediately (read in-tx), but the static permission and role come from a JWT up to 15 minutes stale, and staff `status` is re-checked only at refresh. `granted_by`, `initiated_by` and `approved_by` are not bound to the session identity at DB level. In-tx precedents: `withdrawal_handlers.go:537-552` (ADR 0024 §1), 0105:27-50. | In every K mutation tx: derive the actor from the DB session GUC and require it in the triggers; re-read the actor's `staff_users` row (active, tenant, eligible role); at execution re-check the initiator's and every counted approver's grant and status (a revoked or suspended approver does not count). AZ tests: a suspended actor with an unexpired token is refused; an actor demoted mid-token is refused. |
| S-5 | Medium | B | `ResolveLaunchToken` consumes on the token hash alone, with no provider/mode/asset predicate (`launch.go:290-325`), and its lazy expire writes `expired` (`:311`). Checking the binding afterwards either burns the session (DoS) or commits a consume that should have been refused. Replay semantics are undefined against A's `consumed → revoked` or a reused request id. | ADR 0103, one transaction: (1) authenticate with webhookauth outside the tx, taking the tenant from the credential or route, never the body; (2) look up by token hash `FOR UPDATE`; (3) check provider, tenant, mode, asset and expiry, refusing with a uniform error (no existence oracle); (4) re-check kill-switch, capability and RG; (5) CAS `active → consumed` with the provider binding in the predicate; (6) write an idempotency record keyed `(tenant, provider, request_id)` and bound to `token_hash`; (7) audit, then commit. A refusal never consumes (state whether a gate denial leaves the session `active` or `revoked`). A replay returns the stored response only if the token hash matches and the session is still `consumed`. The raw token is never logged or audited. The player ref is opaque and provider-scoped (no PII). ADV tests for each. Do not reuse `ResolveLaunchToken` as-is. |
| S-6 | Medium | D | The tombstone lookup and `postDepositSuccessOrDispute` receive the echoed `res.ProviderReference` (`sweeper.go:~497,513`). The plan compares the echo only "if non-empty", so an empty echo skips the tombstone check and posts `""`. | After comparing, use `*attempt.ProviderReference` (the bound value) for the tombstone lookup and the posting. ADV: empty echo; a live attempt with a tombstoned bound reference. MUT: pass the echo to the posting. |
| S-7 | Medium | I | A global `dedup key UNIQUE` under RLS lets tenant A's upsert collide with tenant B's invisible row, erroring inside the caller's financial tx and leaking the key's existence. Whether a tenant ack can hide a tenant-scoped integrity P1 from the platform is undefined. "Redacted following existing rules" is not a closed contract. The dispatcher's DB scope is unspecified. | 1. Unique `(tenant_id, dedup_key)` (NULLS NOT DISTINCT or partial indexes), with the tenant in any derived key. 2. Ledger and payment integrity P1s are platform-owned (`tenant_id NULL` plus a subject tenant, the G1 pattern), tenant-visible read-only; a tenant ack never suppresses platform view or delivery. 3. Per-Kind attribute allowlist; provider refs only as `providerref.Fingerprint`; no player email, name or IP, no credential or token material, no raw error strings; `last_error_class` is an enum. 4. The dispatcher uses the validated platform-service GUC (0106), not `WithoutTenant`, with read on alerts and insert on deliveries only. 5. ADR 0102 decides whether a `Raise` failure aborts the business tx (recommended: yes for integrity P1s, fail closed) and tests it. |
| S-8 | Low-Med | H | `pg_try_advisory_xact_lock` lasts one tx. The provider call runs outside any tx (`sweeper.go:354`), so the lock cannot be a lease across phases A→C, and holding a tx open would break I7. | State that the claim token is the correctness guard and the advisory lock only de-duplicates phase A. Never hold it across a provider call; a lease needs a session lock on a dedicated non-tx connection. Key via `hashtext()` or the two-key form. Test that no tx is open during `QueryStatus`/`Deposit`. State whether suspended tenants are still swept (in-flight money must still resolve). |
| S-9 | Low | C | Pending with an empty reference reaches `MarkAccepted`/`setIntentAttempt`, where the 0099 CHECK (`octet_length 1..255`, 0099:136-137) raises an untyped error instead of parking. "Another tenant's reference" is unobservable under RLS (0101:131 index is per tenant). | Treat an empty or invalid reference on Pending like Succeeded (park) and add an ADV test. Re-word the cross-tenant expectation: tenant B sees an unbound reference and posts nothing; there is no cross-tenant read. |
| S-10 | Low | G1 | Planned policy `tenant_id IS NULL AND subject_tenant_id = app.tenant_id`. | `CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL)` plus an FK to tenants; the `NULLIF(current_setting('app.tenant_id', true),'')::uuid` form; exclude player-scope sessions; a BEFORE INSERT trigger requiring staff `actor_id` to equal the validated `app.platform_admin_principal_id` when `subject_tenant_id` is set; the target from the route-validated target (`canActOnTenant`), never the body; the tenant read projection never exposes platform staff IP, user agent or free-form metadata; test that tenant audit readers still filter explicitly. |
| S-11 | Low | K1 lifecycle | Undefined: pending requests and approvals when a grant is revoked; grants issued by a grantor later demoted or suspended; `valid_until` expiring mid-request. | ADR 0099 defines these. Recommended: the S-4 execution-time re-check voids stale approvals; grants from a demoted grantor remain but are surfaced for re-attestation. CON tests: revoke racing execute; expiry between approve and execute. |
| S-12 | Low | K2/K3 | The 0029 precedent refuses a withdrawal approver who is the player's own Person; the plan separates only initiator from approver. | DB trigger: neither the initiator nor any approver may resolve to the beneficiary player's Person (the adjustment target, or the M1/M2 attempt owner). Subject to S-1's caveat. |
| S-13 | Low | K2 | "`ledger.Post` a balanced `manual_adjustment`" does not constrain the accounts. | Exactly one player wallet account of the request's tenant plus that tenant's `manual_adjustment` account (`ledger.go:76`), same asset, enforced in the executor and by a DB check; never house, PSP, provider or bonus-set accounts (HR-9). |
| S-14 | Info | A | Carried correctly. | — |

## 3. Conditions before implementation

- **W0:**
  - ADR 0099 incorporates S-1, S-3, S-4 and S-11.
  - ADR 0100 incorporates S-2, S-12 and S-13.
  - ADR 0102 incorporates S-7.
  - ADR 0103 incorporates S-5.
  - ADR 0104 incorporates S-10.
  - Security reviews each ADR before its code starts.
- **A:** as specified.
- **B:** after A merges, with ADR 0103 (S-5 order) accepted and no as-is reuse of `ResolveLaunchToken`.
- **C:** S-9 added.
- **D:** S-6 added.
- **G1:** S-10 items 1–6. HD-PRH2-5 needs an answer only before the read API ships actor identity; until then the pseudonymous form (fail closed on disclosure).
- **H:** S-8 wording in the plan and ADR 0095 §7.3, plus the no-tx-during-provider-call test.
- **I-core:** S-7 items 1, 3 and 4 in 0110; item 2 before I-wire.
- **K1:** blocked until HD-PRH2-2 (re-framed) is answered; HD-PRH2-6 is answered or platform scope is limited to platform objects; ADR 0099 is accepted with S-1, S-3, S-4 and S-11; and product-owner-proxy confirms the four-capability closed enum.
- **K2:** blocked until K1 merges; HD-PRH2-1 and HD-PRH2-7 are answered; and ADR 0100 is accepted with S-2, S-12 and S-13.
- **K3:** blocked until K2 merges; ADR 0101 is accepted; ledger-finance rules on whether an M1 credit is allowed at all; and M2 applies the S-12 beneficiary check on top of the withdrawal state machine's guards.
- **Launch flags (unchanged):** PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking until HD-PRH2-4 routes P1s to a human; CAS-REVOKE-CONSUMED-1 until A merges.

## 4. Corrections to the human-decision list

- **HD-PRH2-2 (re-frame):**
  - Add the fact that a `tenant_admin` can create `tenant_admin` and `support` accounts, choosing their password and `person_id`, and Persons are unverified self-registrations. So the current identity model cannot distinguish two approvers who are one human.
  - Option (b) with a tenant-local grant approver does not close this.
  - Add option **(d):** tenant admins grant, but money-moving capabilities can only be granted to staff accounts a platform caller created (extending the Stage 3D `finance` precedent). The cost is platform involvement in onboarding each B2B approver.
  - Only (c) or (d) structurally prevent unilateral money movement by one tenant admin until verified staff identity exists.
- **HD-PRH2-1:**
  - Add the combined consequence: `never` (a) plus HD-PRH2-2 (a) or (b) lets one human move money alone.
  - (a) conflicts with CLAUDE.md's permanent rule "four-eyes approval above a configurable threshold"; changing that needs the human to amend CLAUDE.md, not only an ADR note.
- **New HD-PRH2-7:**
  - The question: who may author or loosen `financial_approval_policies` at tenant and brand level, and does loosening need platform approval?
  - Security's recommended engineering default until decided: platform-only authoring; tenants may only tighten.
- **HD-PRH2-6:**
  - (a) and (c) require a new platform-acting-in-tenant RLS family (S-3), which is new privileged power.
  - (b) needs a reliable per-tenant licensing-mode attribute, which security did not verify exists.
- **HD-PRH2-5:** accurate. Platform staff IP, user agent and metadata are never disclosed under any option; that is an engineering rule.
- **HD-PRH2-3 and HD-PRH2-4:** accurate.

## 5. Out of scope

- The list is acceptable.
- CAS-REVOKE-BET-RACE-1 and PAY-SEC-LAUNCH-1 stay registered as launch-blocking and must be listed as open in the W5 report.
- Local runs are never labelled CI.

## 6. Not covered

- No tests, migrations or DB probes were run.
- E1, E2, F and J were checked for consistency only.
- The "19 call sites across 9 files" count and the E3 sweep list were not verified.
- Whether a staff suspension endpoint exists was not verified; S-4 holds either way.
- This is a design-level review, not a pen-test.

## Addendum — ruling on plan revision 2 (`d2cf040`), 2026-09-28

**1. S-7 item 5 vs LF-7: the split is ACCEPTED, with conditions.**
- Security withdraws abort-on-failure for financial evidence and posting transactions: there, the dispute or receipt record is itself the fail-closed outcome.
- ADR 0102 may use abort-on-failure for non-financial integrity alerts.

Conditions, for ADR 0102 and the I tests:
- **(a) Narrow swallow.** Only `Raise`'s own error inside its savepoint is swallowed, and the savepoint is rolled back. An already-aborted outer transaction (25P02), a serialization failure or a deadlock propagates; it is never swallowed.
- **(b) Post-commit retry.** After the business transaction commits, a swallowed in-transaction `Raise` gets a best-effort detached `Raise` in a fresh transaction. A log plus a metric alone would lose the durable P1 in exactly the case that needs it.
- **(c) Error log and metric.** Log at Error and increment `alert_raise_failures_total{kind}` (bounded labels, no tenant label). Neither is a prerequisite for the money path.
- **(d) Reconciliation backstop.** The standing reconciliation checks (`pay_captured_unposted`, `pay_duplicate`) must surface every condition whose P1 might be swallowed. ADR 0102 lists the backing check per alert Kind; for any Kind without a backstop, (b) becomes mandatory, not best-effort.
- **(e) Tests.**
  - An injected `Raise` failure inside T10/T13d still commits the dispute or receipt, returns the uniform 200, and fires the post-commit detached `Raise`.
  - An already-aborted outer transaction is not masked.
  - Mutants: no savepoint; swallowing the outer-transaction error.

**2. H sweeping regardless of tenant status: ACCEPTED, with amendments.**
- **(a) Resolution only for non-active tenants.** Allowed: `QueryStatus`, evidence application, dispute, and T17 re-drive of an already-sent attempt. Never a new money-moving call: no `created`-attempt dispatch, no cascade child, no new payout `Withdraw`. A non-active tenant is treated like an engaged kill switch for new dispatch.
- **(b) Per-tenant safeguards.**
  - Tenant status is read in-transaction for each tenant.
  - The kill switch and the synthetic-adapter tripwire still apply.
  - Credentials come only from the per-tenant resolver, never from another tenant or a platform credential.
  - Every resolution action is audited as for active tenants.
- **(c) Tests.**
  - A suspended tenant's pending attempt resolves by poll.
  - A suspended tenant's `created` attempt is not dispatched.
  - Tenant isolation: one tenant's suspension neither affects nor exposes another.
