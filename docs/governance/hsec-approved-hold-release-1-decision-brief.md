# Decision brief: HSEC-APPROVED-HOLD-RELEASE-1 (approved withdrawals on a suspended or closed tenant or brand)

Status: DOCUMENT ONLY. This brief decides nothing, implements nothing and invents no option. It changes no code, no tests,
no migration and no registry. Baseline `86a5439` (branch `gate-r14-c`). Every statement is sourced from the record or from
the code at that baseline; every gap is marked "not established by the record".

Owner register row: `docs/governance/human-decision-register.md:51` (HSEC-APPROVED-HOLD-RELEASE-1, "AWAITING OWNER (with
HD-CTF-6)", owner + security; "None decided; funds frozen (not lost) until reactivation or ADR 0107 CT-PRE").
Registry origin: `docs/governance/task-registry.md:4216` (H-SEC-5-11-IMPLEMENTED-2026-10-08, "OWNER DECISIONS RECORDED, NOT DECIDED" (a)).

Sources: ADR 0095 section 43 (43.1 "Not touched" and its 2026-10-08 correction; 43.2); ADR 0107 (PROPOSED, DESIGN ONLY) sections
1, 2, 4.1, 5.2, 7.2, 12, 13, 14; HD-CTF-6 (ADR 0107 section 12); H-W1 (registry `task-registry.md:4173`, `:4182`; register
line 19); CLAUDE.md; `internal/withdrawal/withdrawal.go`; `internal/payments/payout.go`; `internal/tenant/status.go`,
`internal/tenant/payment_initiation_gate.go`; `internal/httpserver/withdrawal_handlers.go`, `financial_routes.go`;
`internal/wallet/wallet.go`; `docs/architecture/withdrawal-state-machine.md`, `withdrawal-policy-configuration.md`; ADR 0096.

Naming note (so the owner is not misled): the task framing groups "HD-CTF-10/H-W1" as the never-automatic rule. In the record the
never-automatic rule is the owner decision **H-W1** (2026-10-05). **HD-CTF-10** is a different item (credential treatment when
observing a closed tenant; register line 20 shows it DECIDED). Both are cited below for what they actually say.

## 1. The facts, in one paragraph

A withdrawal hold is posted at `requested` (`player_cash` to `player_withdrawal_hold`). The hold is released by exactly one
terminal posting. At the baseline, `Reject` accepts only `pending_review`, `Cancel` only `requested`, and the only code paths
that move an `approved` request are the staff payout claim (`ClaimForDispatch`: to `submitted`, or to `rejected` by KYC deny)
and both are refused for a non-active tenant or brand by the H-SEC-11 gate. Therefore an `approved` request on a non-active
tenant or brand has no release path: its hold stays and the funds are frozen until the tenant and brand are `active` again, or
until the (undesigned-in-code, DESIGN ONLY) ADR 0107 CT-PRE path exists for a CLOSED tenant. ADR 0095 section 43.1 records this as an
owner decision item together with HD-CTF-6; it is NOT decided.

## 2. Withdrawal lifecycle in code (what moves what)

States (`withdrawal.go:38-48`): `requested`, `pending_review`, `approved`, `rejected`, `submitted`, `completed`, `failed`,
`cancelled`, `reversed`.

| Function | From state (code) | To | Ledger effect | Actor | Gated by tenant/brand status? |
|---|---|---|---|---|---|
| `RequestWithdrawal` (`:383`) | none | `requested` | hold posted | player (HTTP) | YES (first statement; H-SEC-11) |
| `MoveToPendingReview` (`:591`) | `requested` | `pending_review` | none | system | no |
| `Approve` (`:709`) | `pending_review` only (`:728`) | `approved` when ready (`:836-844`) | none (hold stays) | staff / automated | NO ("Not touched", ADR 0095 43.1) |
| `Reject` (`:893`) | `pending_review` only (`:911`) | `rejected` | `:rejected` reversal, hold to `player_cash` | staff (human only) | NO |
| `Cancel` (`:1651`) | `requested` only (`:1656`) | `cancelled` | `:cancelled` reversal, hold to `player_cash` | player | NO |
| `LockApprovedForSubmission` (`:1026`) | `approved` | (lock only) | none | via `ClaimForDispatch` | n/a |
| `DenyForCompliance` (`:1081`) | `approved` only | `rejected` | `:kyc_denied` reversal, hold to `player_cash` | system, inside the T1p claim tx | Only reachable AFTER the H-SEC-11 gate (see section 6) |
| `MarkSubmittedPending` (`:1316`), `MarkSubmitted` (`:1410`) | `approved` | `submitted` | none | `ClaimForDispatch` (T1p); `MarkSubmitted` has no non-test caller | `ClaimForDispatch` YES |
| `Complete` / `Fail` | `submitted` only | `completed` / `failed` | settle / release | provider evidence, staff M2 (ADR 0101) | not applicable to `approved` |

Ledger accounts: hold is `player_withdrawal_hold` and cash is `player_cash`, per wallet and asset (`withdrawal.go` Reject/Cancel/Deny
all resolve `AccountPlayerWithdrawalHold` and `AccountPlayerCash` on `wr.WalletID`).

Consequence worth stating plainly (from the "Not touched" list, ADR 0095 43.1): `Approve` is NOT gated by tenant or brand
status. A `pending_review` request on a suspended tenant can still be approved and then becomes `approved` with no way forward.
Whether that is intended is not established by the record (the record says only that approve/reject/resolve/cancel were
deliberately left untouched by the H-SEC rulings, "do not broaden").

## 3. The comparison table (items a to d)

"Active" means `tenants.status = 'active'` AND `brands.status = 'active'` (both vocabularies are `active|suspended|closed`;
there is no separate "inactive" status, ADR 0095 43.1).

| | (a) Active tenant + brand | (b) Tenant or brand SUSPENDED | (c) Tenant or brand CLOSED |
|---|---|---|---|
| Approved payout: can staff dispatch? | Yes: `POST /v1/admin/withdrawals/{id}/submit` runs `ClaimForDispatch`: lock, gate passes, KYC gate, route, `approved` to `submitted` + attempt. | NO. 409 `TENANT_OR_BRAND_NOT_ACTIVE`; request left `approved`; whole tx rolled back; no attempt, no KYC decision, no audit row (`payout.go:299-309`; handler `withdrawal_handlers.go:925-931`). | NO, same refusal (the gate treats anything not `active` the same). For a closed TENANT, ADR 0107 section 6.4 additionally plans DB triggers refusing `-> submitted` (not implemented). |
| Approved payout: can the sweeper dispatch it? | An `approved` request has no attempt (attempt is inserted only at `submitting`, ADR 0107 section 2 CT-PRE row). The sweeper acts on attempts, so it never touches `approved`. | Same: nothing for the sweeper to act on. | Same. |
| Can the hold be released by existing code? | Yes, by dispatching then settling or failing, or by KYC deny inside T1p. Not by `Reject`/`Cancel` once `approved`. | NO. `Reject` needs `pending_review`, `Cancel` needs `requested`. Only KYC deny, which sits behind the gate, and is unreachable. | NO, same. ADR 0107 CT-PRE `release_hold_to_player_cash` would apply to a closed TENANT only (see section 7); it is DESIGN ONLY and "every outcome disabled by configuration" until HD-CTF-1 is answered. |
| Recovery | n/a | Reactivation of tenant AND brand (section 6). | Reactivation, or ADR 0107 CT-PRE (not implemented). Whether a closed tenant may ever be reopened is HD-CTF-9, OPEN. |
| (d) What is held | Amount `wr.amount` of `wr.asset_code`, as a credit balance on `player_withdrawal_hold` for the player's wallet. Cash is not spendable (`player_cash` was debited at `requested`). | Unchanged from (a). The hold is neither moved nor released by suspension. | Unchanged. |
| (d) Who can see it (staff) | Tenant staff with `PermWithdrawalReview` via `GET /v1/admin/withdrawals/history` and `/{id}` (`financial_routes.go:101-104`, tenant scope). | The same routes are not gated by tenant status (the record lists read routes as untouched, 43.1). | Closed-tenant staff access "is still not refused by auth" (ADR 0107 header pointer 2026-10-05). Whether closed-tenant staff SHOULD see it is HD-CTF-2, OPEN (default in ADR 0107: none). Platform view: reconciliation observation of non-active tenants, evidence only (H-W1, ADR 0095 section 40.4); ADR 0107 section 3 plans a platform_acting queue (not implemented). |
| (d) What the player sees | `GET /v1/me/withdrawals` returns `state`, `amount`, `asset_code`, `hold_ledger_transaction_id` (`withdrawalRequestResponse`, `withdrawal_handlers.go:32-39`): state `approved`. Wallet summary (`wallet.GetSummary`, `wallet.go:150-178`) reports `HeldForWithdrawal` separately from `CashBalance`. | Same: state `approved`, funds shown as held. No message explaining the freeze exists in the code I read. | Same. After closure the player's access, notification and path to the money is HD-CTF-3 / HD-CTF-8, OPEN. Whether a player session on a closed tenant can read these routes: not established by the record. |

(b) versus (c) in code: the gate does not distinguish suspended from closed (both refuse), and ADR 0107 treats only `closed`
(it requires `tenants.status = 'closed'` at submission and at execution; section 5.1 columns `tenant_status_at_*`, and section 13 ADV
test "Release for an active or suspended tenant" is a REFUSAL case). So for a SUSPENDED tenant there is no designed
release path anywhere in the record, and for a non-active BRAND on an otherwise active tenant there is none either (ADR 0107
section 4.2: "There are no tenant or brand rows"; it is keyed to tenant closure). Not established by the record: any
brand-level release.

## 4. What is NOT frozen (so the owner sees the whole picture)

- Before `approved`: a `requested` hold can be cancelled by the player and a `pending_review` hold can be rejected by staff, on a
  non-active tenant or brand, because those functions carry no status gate (ADR 0095 43.1 "Not touched").
- New withdrawal requests (and so new holds) on a non-active tenant or brand are refused (H-SEC-11, 409).
- Submitted payouts (`submitted`) are a different class (ADR 0107 CT-NEVER-SENT / CT-INFLIGHT / CT-M2 / CT-BLOCKED) and are not the subject of this brief.

## 5. Four-eyes today, and what a release/cancel path would additionally require (item e)

### 5.1 Today (code)

| Step | Requirement today | Source |
|---|---|---|
| Approve below the threshold | One approval (a human staff Person, or an automated service identity). | `withdrawal.go:757`, `:802-808` (`ready := !requiresMultipleApprovals`) |
| Approve at or above the threshold | `wr.Amount >= policy.ThresholdAmount` means `RequiredApprovals` DISTINCT human Persons (count collapses two logins of one Person via `COALESCE(su.person_id, ...)`); `RequireStepUp` returns `ErrStepUpRequired` (the MFA mechanism itself is NOT IMPLEMENTED). | `withdrawal.go:757-815`; `withdrawal-policy-configuration.md` section 7 |
| Approver constraints (DB) | Person-linked, active staff, not the withdrawing player's Person (self-approval); trigger `withdrawal_approvals_enforce_governance`. | migrations 0033, 0034; `ErrSelfApproval` |
| Threshold value | Per tenant/brand/asset policy row, fail closed default; the real production values are not decided (HD-PRH2-3; `withdrawal-policy-configuration.md` section "not decided"). Below-threshold four-eyes semantics: HD-PRH2-8, OPEN, non-blocking. | policy doc `:133-137`; register |
| Reject | One human decision, always human, recorded in `withdrawal_approvals` with threshold and amount in force; NOT threshold-gated and NOT four-eyes. | `withdrawal.go:893-1015` |
| Cancel | Player only, `requested` only; no staff four-eyes. | `withdrawal.go:1651`; handler `:1279-1335` |
| Staff submit (payout dispatch) | NO four-eyes and NO `withdrawal_approvals` row. One staff principal with `PermWithdrawalSubmit` that passes the Person-linkage/active eligibility check (the Go check is stated as "the ONLY enforcement point for this transition", handler `:875-883`). The four-eyes protection for a payout is entirely upstream, at `Approve`. | `withdrawal_handlers.go:866-903` |

### 5.2 What a release/cancel path would additionally require, per the record

The only designed release of an `approved` hold is ADR 0107 CT-PRE (`release_hold_to_player_cash`, posting shape
`withdrawal_rejected_hold_to_cash`, section 4.1; function `withdrawal.ReleaseForGovernedResolution`, section 7.2). If it were built
and enabled, the record requires all of the following (none is implemented; the ADR is PROPOSED, DESIGN ONLY, and the mechanism
stays "every outcome disabled by configuration" until HD-CTF-1 is answered):

- Classification `closed_tenant_hold_resolution` = `mandatory_four_eyes` (section 5.2, security Q-CT-SEC-2 confirmed). It is
  not the same as the threshold-based four-eyes above: it applies regardless of amount. "No threshold is seeded" (HD-PRH2-3).
- Request and approve only by platform_acting principals holding the new capability pair `closed_tenant_hold_resolution:request` / `:approve`
  (explicit, in-force grant with NOT NULL `valid_until`; do NOT reuse `payment_force_resolve`; no `tenant_admin` permission).
- Distinct non-NULL Persons for requester and approver (LF-11, non-configurable); requester and approver Persons different from the
  withdrawing player's Person (S-12); author-of-rule and author-of-policy separation (CT-R5); interim "at least one independent
  approver" (HD-PRH2-8 interim); `GREATEST(1, ...)` over a policy with no row meaning disabled.
- A permitting rule row, effective-dated, platform-only four-eyes rule changes, no backdating; "No row => false. Nothing is seeded"
  (jurisdiction/licence/platform levels; HD-CTF-1(a),(b) OPEN).
- A reason code from a closed catalogue and `evidence_ref_hash` (HD-CTF-7 OPEN).
- A distinct ledger key `<wr.id>:closed_tenant_released`, DB triggers in all sessions, acting-fence shapes, an audit record on every step,
  and exactly-once guarantees (sections 7.2, 7.3, 7.5).
- Preconditions asserted at execution: tenant is `closed`; no `payment_attempts` row exists for the withdrawal (LF CT-5); the withdrawal is
  still one of `requested|pending_review|approved`.

What the record does NOT establish for any other release/cancel path (suspended tenant, non-active brand, or a tenant-level
non-ADR-0107 route): the approval count, the requester/approver roles, the threshold semantics, the reason codes, the ledger key
and the audit shape. Not established by the record.

## 6. Is reactivation the ONLY current recovery path? (item f; verified in code)

Verified by reading every `UPDATE withdrawal_requests` site (`withdrawal.go:541, 601, 837, 975, 1142, 1330, 1379, 1424, 1512, 1608, 1693`)
and every non-test caller of the withdrawal transition functions:

| Candidate route out of `approved` | Reachable on a non-active tenant/brand? |
|---|---|
| `Reject` | No: requires `pending_review` (`:911`). |
| `Cancel` | No: requires `requested` (`:1656`). |
| `Approve` | No: requires `pending_review`. |
| `MarkSubmittedPending` / `MarkSubmitted` | Only via `ClaimForDispatch` (refused by the gate); `MarkSubmitted` has no non-test caller. |
| `DenyForCompliance` (`approved` to `rejected`, releases the hold) | Only via `ClaimForDispatch`, and the H-SEC-11 gate runs BEFORE the KYC gate (`payout.go:307`, "so a refusal never commits a KYC deny/hold release"). So it is also unreachable while non-active. |
| `Complete` / `Fail` / staff `resolve` / ADR 0101 M2 | Require `submitted` (`LockSubmittedForResolution`, `Complete`, `Fail`). Not applicable. |
| Payments sweeper | Acts on `payment_attempts`; an `approved` request has none. No sweeper code in `internal/payments` reads `withdrawal_requests.state = 'approved'` (grep). |
| Direct database edit | Not a code route; outside the record. |

So within the code, the only way an `approved` request moves is to make the tenant AND the brand `active` again and let staff
re-run submit (which may then dispatch, or KYC-deny and release). Two qualifiers that the owner should see:

1. Tenant reactivation exists as a function only: `tenant.ChangeStatus` (`status.go:128`) allows any of `active|suspended|closed` in
   any direction (so `closed` to `active` is not blocked in code), but it "performs NO authorization check", and "No route calls it
   today" (TENANT-STATUS-AUTHZ-1, OPEN, register line 45). Whether a closed tenant may ever be reopened: HD-CTF-9, OPEN.
2. Brand status: I found no non-test Go code that updates `brands.status` (grep for `UPDATE brands` returned nothing). The brand
   reactivation route is therefore not established by the record. This matters because the gate needs the brand `active` too.

After reactivation, a deposit/withdrawal state change never happens automatically: the request is still `approved` and staff must
submit it again (the gate refusal "is non-retryable-until-reactivation", `payment_initiation_gate.go` comment).

## 7. Open question (item g): should a controlled release/cancel path exist?

Stated as the question only: should an `approved` withdrawal on a suspended or closed tenant or brand have a controlled path out
that releases the hold to `player_cash` (or otherwise resolves it), or should it stay frozen until reactivation / ADR 0107?

Considerations from the record only, on each side.

**For a controlled path (the record's considerations):**
- The ADR 0095 section 43.1 correction records the freeze as a gap: "an `approved` request has NO release path; its hold stays and the funds are
  frozen until reactivation (or ADR 0107 CT-PRE)".
- The existing code already treats an unreleased hold as a defect when it can: `DenyForCompliance`'s comment says "leaving the hold in place would
  strand the player's funds with no release path", and the stranded-hold resolve path exists for `submitted` ("could remain financially
  stranded ... with no observable recovery path", `withdrawal.go:1198-1208`).
- ADR 0107 header and ADR 0095 section 43.2: the tenant-closure flow "must not launch" until its prerequisites close; this item gates "Any
  tenant-suspension/closure flow" (register line 51). Without a path, a suspension or closure leaves approved holds with no designed exit
  for a suspended tenant or any brand, because ADR 0107 covers a closed tenant only.
- ADR 0107 R-CT-1 / HD-CTF-3: funds sitting in a closed tenant is itself recognised as a residual needing an answer.
- ADR 0107 CT-PRE has no attempt (nothing sent), which is why a CT-PRE release is the lower-risk class in the ADR's own terms (section 2, LF CT-5).

**Against / for keeping the freeze (the record's considerations):**
- Funds are frozen, "not lost" (register line 51); the hold is an ordinary balance on `player_withdrawal_hold` and reconciliation observes
  non-active tenants (H-W1, ADR 0095 section 40.4), so the freeze is visible, not silent.
- CLAUDE.md and the owner decision H-W1 (2026-10-05) say never auto dispatch/release/cancel/settle closed-tenant funds; a release path
  would therefore have to be a controlled, staff, four-eyes path, which is exactly the ADR 0107 mechanism: DESIGN ONLY, with HD-CTF-1..9 OPEN.
  Building a release for suspended/brand cases would create a second path the record has not designed or reviewed.
- Which outcomes are legally permitted on closure (release to cash, retain, dispatch, third-party transfer) is a legal/compliance decision
  (HD-CTF-1, HD-CTF-5); the architecture cannot determine it (ADR 0107 requirement R6).
- The ruling that created the gate said "do not broaden" (ADR 0095 43.1, LF N2); a release path is outside the H-SEC-5/11 scope.
- ADR 0107 requires `security` re-review, `ledger-finance` confirmation, `product-owner-proxy` concurrence and a human-authorized workstream
  before any code (header).
- The freeze ends by itself on reactivation; for a suspension (a temporary status by name) that may be the intended behaviour. The record does not say
  whether suspensions are expected to be temporary. Not established by the record.

Not established by the record: how long a suspension typically lasts; how many approved holds a suspension would strand; any player
communication duty during a freeze (HD-CTF-8 OPEN); whether a suspended tenant's player may withdraw to the same wallet later.

## 8. What must NEVER happen automatically (item h)

| Rule | Source |
|---|---|
| Closed/suspended-tenant funds are never auto-dispatched, auto-released, auto-cancelled or auto-settled; the staff path (platform_acting, four-eyes) is the only route and is unchanged. | H-SEC owner decision H-W1 DECIDED 2026-10-05 (`task-registry.md:4173`, `:4182`; register line 19) |
| No automatic payout dispatch for a closed tenant; no automatic cancel or release without a controlled resolution; flow = authorized financial-staff queue, four-eyes, deterministic ledger operation, full audit. | ADR 0107 requirements R1, R2, R3 (section 1) |
| Reactivation, suspension and reopening must not silently resume or release anything: reopening "voids nothing automatically, but every pending resolution then refuses at execution". | ADR 0107 section 8 (Reopening) |
| No KYC outcome after possible dispatch triggers `Fail`, a hold reversal or any automated release. | ADR 0095 section 5.2.1 |
| "never automatically resubmit on timeout/ambiguity". | `withdrawal-state-machine.md` (cited in `withdrawal.go:1198-1210`) |
| Never auto-pay a withdrawal above the tenant-configured threshold. | Payments agent limitation; `withdrawal.Approve` four-eyes |
| Never edit a balance; corrections are compensating ledger entries through `ledger.Post`. | CLAUDE.md "Financial / ledger rules" |
| No unrestricted bypass, no direct balance mutation, actor/subject separation. | ADR 0107 R5 |
| "No new money moves" is the only default for a closed tenant; what happens to player funds on closure is a business/compliance decision the code does not make. | ADR 0095 section 37.5 (LF F3), `0095:7210-7214` |

Observation (HD-CTF-10 as it actually is): observation of a closed tenant is evidence only; the credential treatment for that
observation is a separate, DECIDED item (register line 20).

## 9. Decisions required from the owner (with security)

Numbered questions. Each needs an answer from the owner (with `security` where marked).

1. Is a controlled release/cancel path for an `approved` withdrawal on a non-active (suspended or closed) tenant or brand WANTED at all,
   or is the freeze until reactivation (and, for a closed tenant, until ADR 0107 CT-PRE) the accepted behaviour for now?
2. If wanted: for which non-active cases? Choose from the cases the record distinguishes: closed tenant only (the ADR 0107 CT-PRE scope);
   also suspended tenant; also a non-active brand on an active tenant. (The record designs only the first.)
3. If wanted: is the approval requirement the ADR 0107 shape (platform_acting, mandatory four-eyes regardless of amount, distinct Persons,
   S-12), or something else? Not established by the record for suspended tenants or brands. (owner + security)
4. Is the answer to HD-CTF-6 (may a tenant be closed while hold-bearing withdrawals exist; the architect's interim recommendation is to refuse)
   decided together with this item, as the registry says? If yes, state it.
5. Should `Approve` on a non-active tenant or brand remain ungated (today it can add to the stuck `approved` pile), or is that a separate
   ruling? (owner + security; the H-SEC rulings did not cover it.)
6. Is a refused staff submit required to leave an audit row (ADR 0095 43.2 follow-up (a))? Listed because it determines how a frozen
   approved item is evidenced; may be answered separately.

## 10. What is NOT being decided here

- No release/cancel path is designed, approved, scheduled or implemented by this brief; ADR 0107 stays PROPOSED / DESIGN ONLY.
- HD-CTF-1 to HD-CTF-9 (permitted outcomes, closed-tenant staff visibility, player access after release, balances not under a withdrawal,
  other outcomes, closure while holds exist, reason codes, alerts, reopening) are not answered here; HD-CTF-6 is referenced only because
  the register links the two.
- No change to the H-SEC-5/11 gate, to `Reject`/`Cancel` preconditions, to the sweeper, or to the brand-sweeper gate (that is H(8), a
  separate brief: `h8-brand-sweeper-gate-decision-brief.md`).
- No legal or regulatory conclusion (software capability is not licence or legal approval).
- No claim of real-PSP readiness; all of this is `IMPLEMENTED` against `MOCK` where implemented, and ADR 0107 is `NOT IMPLEMENTED`.
- Tenant status-change authorization (TENANT-STATUS-AUTHZ-1) and the brand-status change route are not decided here.

## 11. Facts not establishable from the record

- Whether any production-style route can reactivate a tenant or a brand today (no route calls `ChangeStatus`; no brand status writer found).
- What a player on a closed tenant can read or do (auth does not refuse closed-tenant staff sessions per ADR 0107; the player session case is not stated).
- Expected duration of suspensions; the count or value of `approved` holds at risk; any player-facing message during a freeze.
- Whether leaving `Approve`/`Reject`/`Cancel` ungated on non-active tenants was a considered position beyond the "do not broaden" instruction.
- The production four-eyes threshold values (HD-PRH2-3) and below-threshold semantics (HD-PRH2-8).
