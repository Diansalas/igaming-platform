# lic11 record: governed per-tenant withdrawal restriction policy (migration 0182)

> **lic17 (migration 0184) supersedes parts of this record:** every licence relied on is evaluated (F-1); an unreadable licence is
> `unknown`, not `licence_missing` (F-4 / M-2); the first configured `allow` is four-eyes (F-2); the policy functions are session
> bound (M-3); `hold_for_review` is a park with no per-withdrawal release, and the earlier "reject the parked request" exit was
> wrong (M-1). See `docs/plans/lic17-record.md`.

- **Author:** `ledger-finance` specialist. **Worktree:** `.claude/worktrees/lic11`, branch `lic11-withdrawal-restriction-policy`
  from `claude/licensing-jurisdiction-completion` at `ea3c379`. **Migration:** 0182 (assigned).
- **STEP 1 analysis (written before any code):** `docs/plans/lic11-withdrawal-restriction-analysis.md`.
- **Runbook:** `docs/runbooks/withdrawal-restriction-policy.md`.
- **Not a legal opinion.** No policy value, no legal value, no real money, no real provider. Ships NO policy row.
- **Database not run by this agent:** the private-database harness was refused by the permission system. Every integration
  test is written and compiles (`go vet -tags integration ./...` clean); the orchestrator runs them (section 8).

## 1. Labels

| Deliverable | Label |
|---|---|
| STEP 1 code-derived cause x stage matrix | IMPLEMENTED (document) |
| Licence-state causes evaluated at request, T1p dispatch, T2/T12 re-dispatch; actions allow / hold_for_review / block | IMPLEMENTED (unit + static tests run green; integration tests written, NOT RUN by this agent) |
| Default (no row) = behaviour before 0182, provenance `default_unconfigured` recorded on every request audit row and every dispatch / re-dispatch gate decision row | IMPLEMENTED (same caveat) |
| Governed changes (single actor tightening, two distinct approvers loosening, ADR 0110 proofs, append-only versions, FORCE RLS, audit with before / after, IP, reason) | IMPLEMENTED (same caveat) |
| Operator read API (Go service + HTTP routes + OpenAPI) with the UNCONFIGURED notice | IMPLEMENTED (HTTP: unit / contract tests only; no HTTP integration test) |
| Tenant / brand suspension or closure configurable | NOT IMPLEMENTED by design: governed by approved owner decisions (ADR 0095 s44 d13-18); amendment proposed (section 6, D-2) |
| Market / country closed for the tenant as a cause | NOT IMPLEMENTED: not evaluable on a withdrawal (no per-withdrawal market determination; Q18 / Q19) |
| Launch readiness informational note for an unconfigured policy | NOT IMPLEMENTED (launchgov SnapshotVersion is out of bounds for this slice; proposed text in section 9, R-5) |
| SQL backstop of the licence-cause policy | NOT IMPLEMENTED (single licence-validity implementation INV-M-5; residual R-1) |

## 2. What STEP 1 found (the owner's premise, checked)

The statement "withdrawals continue under suspension" (Q12 option (a) "as built") is **true for the licence and false for
the tenant / brand**:

- Licence suspended / expired / not yet issued / missing: no withdrawal path read the licence at all; withdrawals and payouts
  continued by omission, with no policy, no per-tenant control and no audit of the licence state. This was the hard-coded
  universal rule the owner asked to stop. **Now policy-driven** (default unchanged, labelled interim, visible).
- Tenant / brand suspended or closed: new requests and normal payout submission STOP (H-SEC-11 + 0131 WD010/WD011 + 0124
  HR050 + resolution-only deferral), by **approved owner decisions** ADR 0095 section 44 decisions 13-18 (2026-10-08). Funds
  stay held; the only exit is the four-eyes governed hold release back to the player's cash (ADR 0111 s6). Q12's "(a) as
  built" misdescribes this; ledger-finance condition C2 had already flagged it. **Not re-opened by configuration** (D-2).
- Player-specific restrictions, AML / sanctions holds, KYC, risk, kill switch, provider capability and instrument validity were
  already distinct, hard, and are unchanged (matrix in the analysis, sections 2 (e)-(h)).

## 3. Design (what was built)

### 3.1 Database (migration 0182)

- `withdrawal_restriction_causes`: closed registry (9 causes). Configurable: the four licence causes, interim default
  `allow / allow / allow` (request / dispatch / redispatch) = behaviour before 0182. Not configurable: tenant / brand status
  (governing reference ADR 0095 s44 d13-18), market closed (Q19, not evaluable). Read by platform and tenant sessions; never
  player / acting sessions.
- `withdrawal_restriction_policies`: append-only versions; per-tenant `set_version` assigned by the guard under the tenant
  status gate key taken EXCLUSIVE; FORCE RLS (tenant reads its own; platform reads all; only a platform session inserts); a
  non-owner insert needs an EXECUTED change of the same transaction with the same content (WR040); a non-configurable cause is
  WR002; `jurisdiction_id` exists for forward compatibility and is CHECK-refused (no per-withdrawal jurisdiction).
- `withdrawal_restriction_policy_changes` / `_approvals`: the maker-checker (section 3.3).
- `withdrawal_restriction_effective(tenant, cause, stage)`: the single resolution (configured row, else default) with source,
  policy id and set version; `withdrawal_restriction_set_version(tenant)`; `withdrawal_restriction_is_widening(...)`.
- `withdrawal_gate_decisions`: gate code WD056 admitted; four provenance columns (`restriction_policy_source`,
  `restriction_policy_set_version`, `restriction_cause`, `restriction_action`) with shape CHECKs; a WD056 row must be a
  CONFIGURED hold / block of the cause in `reason_code`. 0169 WD052 / WD053 and the 0171 signed allow are UNCHANGED.
- `actor_proof_require`: the 0177 body plus one marked block (operation family `withdrawal_restriction_policy:*`, platform
  scope only) and one allow-list term (static test pins this byte-for-byte; the down restores the 0177 body).
- Grants (runtime: SELECT/INSERT, UPDATE on the eight state columns of changes) mirrored in `deploy/init-app-role.sql`.
- No existing guard is replaced: 0131, 0124 HR050, 0169, 0171 bodies are untouched; `licensing_change_kinds`,
  `licensing_change_classify`, `licensing_change_row_matches`, operating-market and launchgov objects are untouched.

### 3.2 Money paths (Go)

- `internal/withdrawal/restriction.go`: `EvaluateRestriction` (plain reads: `jurisdiction.ResolveLicensingChain` +
  `jurisdiction.EvaluateLicenceValidity`, the single licence-validity implementation; then the SQL resolution). Any read error is
  `ErrGateUnavailable` (fail closed, retryable). A default never refuses.
- Request (`RequestWithdrawal`): after the replay lookup and H-SEC-11, before the WD-RG-1 gate. Block: WD056
  `*GateDeniedError` with the denial audit (the handler already commits gate denials); hold_for_review / allow: accepted. The
  provenance is merged into the existing `withdrawal.requested` audit metadata; a CONFIGURED cause also writes
  `withdrawal.restriction_cause_in_force` or `withdrawal.held_for_restriction_review`. The default path writes no extra row.
- Dispatch / re-dispatch: `EvaluateDispatchGate` evaluates the policy first (a refusal returns WD056 without consulting the
  gate), then the WD-RG-1 gate, and attaches the provenance (`withdrawalgate.Decision.Restriction`, set only by
  internal/withdrawal, overwritten after the gate); `RecordDispatchGateDecision` writes it. The existing pinned call sites
  park (T1p) or escalate (T2/T12) a WD056 exactly like WD050/WD051. Audit metadata carries the provenance.
- HTTP: a WD056 at T1p answers 409 with a policy-specific message; the request stage answers the generic 403.

### 3.3 Governance (`internal/withdrawalpolicy`)

- Request / Approve / Reject / Cancel / Effective / History / ListChanges / GetChange. Platform_admin only for writes; tenant
  admin / compliance read their own tenant (`auth.PermWithdrawalRestrictionPolicyChange` / `...Read`).
- Widening (any stage less strict) needs 2 approvals by distinct Persons, neither the requester's Person nor account, at
  most 24 h old, executed by the final approval; narrowing or equal is single actor with a reason code executed in the request's
  own transaction; the database re-classifies at execution under the tenant key (WR024). Changes expire after 72 h.
- Each write takes the tenant status gate key EXCLUSIVE first (ADR 0082 R8: advisory before the change-row lock).
- HTTP routes (8), OpenAPI, API codes `WITHDRAWAL_RESTRICTION_POLICY_*`, step-up critical on request / approve (the signer
  backstop registry now pins request / approve / execute).

### 3.4 Lock order (ADR 0082)

The gate reads are plain reads under locks the paths already hold: request and T1p take the tenant status key SHARED in
H-SEC-11 before L0.4; the policy read follows it. A policy write takes the same key EXCLUSIVE before any row lock, then the
change row, then inserts the policy row (FK KEY SHARE on `tenants`, compatible with the NO KEY UPDATE a status writer holds).
No path holds a change row and then waits on the status key, so no cycle. T2/T12 take no status key (unchanged): one tick
may still use the previous policy version; the recorded `set_version` shows which. Pinned by
`TestLic11_LockOrder_PolicyChangeVsRequest` (both orders, lockorder harness, no 40P01).

## 4. Decisions made here (interim, reversible)

| # | Decision | Why |
|---|---|---|
| D-1 | Default for every licence cause = allow at every stage, labelled INTERIM ENGINEERING DEFAULT, source `default_unconfigured` recorded and shown | Owner: "no policy row = current behaviour exactly"; "do not invent the missing policy". |
| D-2 | Tenant / brand suspension and closure are NOT configurable; listed as owner-decided | ADR 0095 s44 d13-18 are approved owner decisions; a two-admin configuration must not override them. Amendment proposed (section 6). |
| D-3 | Governance mirrors the 0177 licensing maker-checker on dedicated tables, reusing `licensing_change_actor()`, the ADR 0110 verifier and the k2 hashing | The licensing primitive is the existing fit, but its registry / classifier are being changed in parallel (0180 / 0181) and may not be touched here. Folding this into a `licensing_change` kind later is mechanical (R-4). |
| D-4 | `hold_for_review` = accept at request, park at T1p, escalate at T2/T12; review uses existing exits; no per-withdrawal override | No approved per-withdrawal clearance policy exists; inventing one would be an override path (owner: "without bypassing applicable restrictions"). |
| D-5 | Widening = less strict at ANY stage; pinning the same action is single actor | Same asymmetry as licensing / launch: tightening is fail-closed. |
| D-6 | The policy is per tenant (not per brand / jurisdiction): one licence per tenant (Q15 (a)); jurisdiction-scoped rows CHECK-refused | Mirrors `withdrawal_policies` (no per-withdrawal jurisdiction). |
| D-7 | Market-closed is listed but not evaluable / not configurable | No per-withdrawal market determination; a configured block that could never fire would be a false control. |
| D-8 | The licence read failing is fail closed (ErrGateUnavailable); an invisible tenant row is an error, an unreadable licence row is `licence_missing` | Same semantics as the operating-market resolver; a mis-scoped session never becomes "no licence". |

## 5. Tests

Unit / static (run, green): `internal/withdrawal` (cause mapping, combine, WD056 shape, placement pins),
`internal/withdrawalpolicy` (widening matrix, actor matrix, approval counting, error classification, migration 0182 static pins
incl. the verifier byte-diff and "ships no policy row", only this package writes the policy tables), `internal/actorproof`
(scope matrix, signer allow-list, step-up registry), `internal/auth` (permission matrix), `internal/apierror`,
`internal/httpserver` (OpenAPI contract both ways, DTO / schema, error mapping, golden step-up table), `internal/db`
(A-18 acting fence, migration pins). Full `go test ./...` green; `go vet ./...` and `go vet -tags integration ./...` clean.

Integration (written, NOT RUN here):

- `internal/withdrawal/lic11_restriction_integration_test.go`: default parity for every licence state (no extra row, provenance
  recorded); configured cause x action matrix at the request stage (block writes nothing, key not consumed, loosening lets the
  same key succeed); policy irrelevant when the cause is not in force; hard restrictions NOT overridable by a configured allow
  (account suspended WD050, risk WD051, KYC rejected, AML hold, unknown instrument, tenant / brand suspended H-SEC-11); dispatch
  evaluation + decision row (WD056 skips the gate, WD053 still vetoes E7; default / configured provenance on the allow row);
  WD056 / provenance CHECKs; tenant isolation, RLS write scope, WR002, WR040; append-only WR055; lock order both ways.
- `internal/payments/lic11_restriction_integration_test.go`: T1p default parity with provenance; T1p configured hold / block
  parks (approved, hold kept, no attempt, WD056 row, staff audit, alert, KYC not run); policy changed mid-flight (decision rows
  record the version used; replayed claim refused, one attempt); hard restrictions at T1p (WD050, kill switch); T2 configured
  hold / block escalates (no resend, no release), default proceeds with provenance; settlement NOT gated under a configured
  block. Every test asserts SUM(debits) == SUM(credits) and projection == rebuild where money moves.
- `internal/withdrawalpolicy/governance_integration_test.go` (runtime role asserted): tightening single actor with audit
  before / after, IP, reason; loosening needs two distinct approvers (self-approval refused, same-Person twin refused, one
  approver cannot loosen, raw `approved` with one approval WR020); write scope (tenant refused in Go and by RLS, WR002, AP001
  without a proof, WR040); rejected change writes nothing; read API (unconfigured visible, tenant-scoped, change requests
  platform-only); concurrent changes serialise with gap-free versions.

Existing tests: none edited except additive pins (section 7). The default path writes no extra row; existing withdrawal
audit rows gain metadata keys only (checked: no existing assertion compares the whole metadata map or matches the new keys).

## 6. Proposed decision-register text (for the orchestrator to place; not placed here)

**Q12 (restated).** "While a tenant's licence is suspended, expired, not yet issued or missing, what happens to withdrawals and
payouts of existing balances? The mechanism exists (migration 0182): per tenant and per licence cause, allow | hold_for_review
| block at the request, payout-claim and re-dispatch stages, changed only through the governed maker-checker (two approvers to
loosen). DECISION REQUESTED: approve the per-tenant policy VALUES (per jurisdiction / licence, with counsel). INTERIM
(engineering default, not an approved policy): allow at every stage, exactly the behaviour before 0182, shown as
`default_unconfigured` to operators and recorded on every decision. Recommendation: none from engineering on the value
(blocking or holding player funds is a legal call); record for each tenant going live which value counsel approved."

**C2 / D-2 (tenant / brand suspension, new).** "ADR 0095 s44 decisions 13-18 stop new withdrawal requests and normal payout
submission while a tenant or brand is suspended or closed; funds stay held and leave only through the four-eyes governed release
back to the player's cash (still on the platform). Q12's '(a) as built' does not describe this. Options: (a) keep decisions
13-18 unchanged (current); (b) amend: allow payouts of existing balances during a SUSPENSION after a per-tenant governed policy
(the 0182 mechanism would extend to tenant_suspended / brand_suspended, needing changes to 0131 WD010/WD011, 0124 HR050, the
resolution-only deferral and H-SEC-11 at T1p, plus a security review); (c) amend for CLOSURE only, tied to HD-CTF-4
(closed-tenant balances). Engineering keeps (a) until the owner decides."

**Q19 (market closed, restated).** "Not evaluable on a withdrawal until a per-withdrawal market determination exists (Q18 /
L-4). Listed in the cause registry as not configurable."

## 7. Pins and shared artefacts changed (all additive)

- `internal/httpserver/staff_route_golden_test.go`: +8 routes; `staff_route_static_test.go` count pin 239/143 -> 247/147.
- `internal/actorproof/stepup.go` + `stepup_test.go`: +3 operations (request / approve / execute critical; reject / cancel
  listed as protective).
- `internal/actorproof/actorproof_test.go`: `internal/withdrawalpolicy` added to the signer allow-list (security review
  mandatory, like `internal/licensingchange`).
- `internal/apierror`: +11 `WITHDRAWAL_RESTRICTION_POLICY_*` codes; OpenAPI Error enum, 8 paths, schemas, responses.
- `withdrawalgate.Decision` gains an optional `Restriction` field (no existing literal affected).
- `deploy/init-app-role.sql`: one grant block (narrowing only; no role / password change).

## 8. For the orchestrator: commands

```
export PRIV_DB=lic11$(date -u +%H%M%S)p$$x01 PRIV_SRC=/home/user/igaming-platform/.claude/worktrees/lic11
bash <scratchpad>/priv_db.sh
bash <scratchpad>/priv_test.sh -tags integration -count=1 -p 1 ./internal/withdrawal/ -run 'Lic11|WDRG1|F4|B13B|KYC|AMLHold|LockOrder|A13|Withdrawal'
bash <scratchpad>/priv_test.sh -tags integration -count=1 -p 1 ./internal/payments/ -run 'Lic11|F4|HSEC|Payout|Claim|Sweeper|AMLHold'
bash <scratchpad>/priv_test.sh -tags integration -count=1 -p 1 ./internal/withdrawalpolicy/ ./internal/licensingchange/ ./internal/actorproof/... ./internal/launchgov/ ./internal/tenant/
bash <scratchpad>/priv_test.sh -tags integration -count=1 -p 1 ./internal/httpserver/ -run 'Withdrawal|Licensing|Staff|OpenAPI'
go test ./... && go vet ./... && go vet -tags integration ./...
```

The full integration suites of `internal/withdrawal`, `internal/payments` and `internal/httpserver` should be run once (the
request path and every dispatch now read the licence chain and the policy; the default must leave them green).

## 9. Residuals

- **R-1** The licence-cause policy has no SQL backstop (INV-M-5 forbids a second licence-validity implementation); a code path
  that bypassed `EvaluateRestriction` would not be refused by the database. The F4 signed allow still proves the dispatch code
  ran (which includes the policy evaluation). Same class as the risk half of WD-RG-1.
- **R-2** T2/T12 take no tenant status key: a policy change can race one sweeper tick (the recorded set version shows which
  version decided). Escalation is not a latch (SF4-3, unchanged).
- **R-3** The provenance columns of `withdrawal_gate_decisions` are not part of the 0171 proof digest (an allow row can only be
  written by the one pinned signer, so they are as trustworthy as that code; a forged DENIAL row can carry any provenance but
  only restricts).
- **R-4** Governance is a mirror of the 0177 primitive, not a `licensing_change` kind (D-3). Folding it in after 0180 / 0181 is
  a migration that adds the kind, a classify / row_matches branch and points the policy guard at `licensing_change_requests`.
- **R-5** No launch-readiness item for an unconfigured policy (launchgov SnapshotVersion is out of bounds). Proposed
  informational check text: "R-WITHDRAWAL-RESTRICTION-POLICY (informational): the tenant has no configured withdrawal
  restriction policy for <causes>; the interim engineering default (withdrawals and payouts proceed whatever the licence
  state) is in force pending owner decision Q12." The data is available from `withdrawalpolicy.Service.Effective`.
- **R-6** MERGE: 0182 replaces `actor_proof_require`; if 0180 / 0181 also replace it, the last-landing migration must carry all
  blocks (static test `TestMigration0182_Static` compares 0182's body with 0177's and must then be re-pointed at the previous
  body). The worktree has a 0180-0181 gap until merge (migrate verify reports it; migrate up is unaffected).
- **R-7** MERGE: the licence chain is read through `jurisdiction.ResolveLicensingChain` (`tenants.licence_id`). If 0180
  (operating bases) changes how a tenant's licence / basis is bound, `EvaluateRestriction` must follow the new resolver (one
  call site, `restriction.go`).
- **R-8** No HTTP integration test for the 8 routes (service-level integration tests cover the rules; contract tests cover the
  wire). An HTTP world test is a follow-up.
- **R-9** `hold_for_review` has no dedicated alert at the request stage (the payout parks with the existing
  `withdrawal.payout_gate_denied` alert at T1p); a review queue / per-withdrawal clearance is a policy question (D-4).
- **R-10** Under a tenant / brand suspension the restriction evaluation never runs at request / T1p (H-SEC-11 refuses first), so
  those refusals carry no restriction provenance (unchanged behaviour, no row is written).

## 10. Proposed ADR text (for the orchestrator; ADR number to be allocated)

> **ADR XXXX: Per-tenant withdrawal restriction policy (lic11).** Status: PROPOSED. Context: before migration 0182 a
> withdrawal never read the tenant's licence, so withdrawals and payouts continued under any licence state by omission, with no
> policy and no record; tenant / brand suspension is governed by ADR 0095 s44 decisions 13-18. Decision: (1) a closed cause
> registry separates licence-state causes (configurable), subject-status causes (owner-decided, not configurable) and market
> causes (not evaluable); (2) for a configurable cause, a per-tenant append-only versioned policy chooses allow | hold_for_review
> | block, evaluated at the request (after H-SEC-11, before WD-RG-1), the payout claim (T1p) and re-dispatch (T2/T12), never at
> settlement; a refusal at a dispatch point is gate code WD056 through the existing F4 park / escalate paths; (3) with no row the
> interim engineering default (allow) applies and its provenance `default_unconfigured` is recorded on every decision and shown
> to operators; (4) changes are maker-checker: tightening single actor with a reason code, loosening two distinct approvers
> (Persons) other than the requester, ADR 0110 proofs, 24 h approval freshness, 72 h expiry, re-classification at execution
> under the tenant status gate key; (5) hard restrictions (account / wallet status, KYC, AML / sanctions, risk, kill switch,
> provider, instrument, tenant / brand status) are never affected by a policy row. Consequences: the owner can approve per-tenant
> values without code; the default is unchanged until then; no SQL backstop for the licence cause (INV-M-5); folding into the
> licensing change primitive is a later mechanical step.
