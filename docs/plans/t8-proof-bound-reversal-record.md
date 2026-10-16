# T8 record: payout-reversal decisions bound to the signed actor proof (ADR 0110 T8 amendment, migration 0165)

Author: `ledger-finance` (implementation). Branch `t8-proof-bound-reversal`, base `12edec0`
(`origin/claude/focused-wright-jw88w9`). Owner decision 2026-10-16 (decision register row "2026-10-16 owner decisions", item 2:
"ADR 0110 T8 amendment - payout-reversal approvals bound to the signed actor proof before any real PSP is activated").
Closes A5 R1 / SR5-2 / LF-A5-2 as proposed in `docs/plans/a5-payout-reversal-record.md` s11.3 with the two s12.3 additions.
Providers stay MOCK. Evidence is local only (private scratch databases on the local PostgreSQL; never CI, never staging). Reviews by
`security` and `ledger-finance` (independent), `code-reviewer` and `qa` are still required before merge. ADRs, task registry,
HANDOVER, progress, active-stage and the decision register are NOT edited (the orchestrator is the single writer); the ADR 0110
amendment text is in section 9, ready to paste.

## 1. Deliverables and labels

| # | Deliverable | Label |
|---|---|---|
| 1 | `reverse` rows of `withdrawal_approvals` insertable only with a valid signed actor proof (`withdrawal_payout_reversal:decide`, scope `tenant`, target = withdrawal request id, payload = the existing payout digest), enforced in the DATABASE by `zz_actor_proof_guard` (LAST BEFORE ROW trigger, catalog-pinned) | **IMPLEMENTED** (local database) |
| 1a | Same binding, freshness, replay and key-id rules as every proof-bound operation (the unchanged `actor_proof_require` verifier, AP001-AP005) | IMPLEMENTED |
| 1b | The forging runtime session (SR5-2 probe, as `igaming_runtime` in a tenant session) is refused | IMPLEMENTED (tested) |
| 2 | WD026 (E11), WD027 (tombstone branch), check D's WD026 twin and the Go quorum count ONLY proof-bound decisions; unproven (pre-0165) rows count nowhere | IMPLEMENTED |
| 2a | Migration safe for existing data (nothing edited; legacy rows keep a NULL nonce); down honest and refusing (`WD096`) while proof-bound decisions exist | IMPLEMENTED (tested on a scratch database) |
| 3 | Go entry point signs exactly like the K3 (M2) and hold-release services: `actorproof.Issuer.Attach` in `internal/payments` for the verified decider, step-up backstop (critical), signing failures returned (500), never skipped | IMPLEMENTED |
| 3a | HTTP route `POST /v1/admin/withdrawals/{id}/payout-reversal` | contract UNCHANGED (body, responses, step-up class critical, route counts); the handler needed no change: the decider is already the token subject and the proof is issued server-side inside `DecidePayoutReversal` |
| 3b | Back-office screen | **NOT IMPLEMENTED** (no screen calls this route; `backoffice/` has no reference to it); nothing to update |
| 4 | ADR 0110 T5 / ADR 0113 E10 (the tombstone branch's hold release) | **NOT CLOSED** by this change (section 7) |
| 4a | ADR 0113 s9.3 (`approve` / `reject` records, WD023) | **NOT IMPLEMENTED** here, deliberately out of scope (section 2.4) |
| 5 | Production key provisioning for this operation | PROVIDER DEPENDENT on the operator (no new key: the existing `ACTOR_PROOF_*` key signs it; the startup gate `VerifyConfiguredInProduction` already refuses to start without an active kid) |

## 2. Design

### 2.1 Database (`migrations/0165_payout_reversal_actor_proof.{up,down}.sql`)

Next free number (0159 and 0161 stay reserved and unused; 0164 was the newest). If another branch of the provider phase also takes
0165, the later merge renumbers (the 0163 down test steps through later migrations dynamically, so it does not pin the number).

- `withdrawal_approvals.actor_proof_nonce TEXT NULL`, CHECK `withdrawal_approvals_actor_proof_nonce_shape`
  (`NULL` or (`decision = 'reverse'` and `^[A-Za-z0-9_-]{16,64}$`)), partial UNIQUE `withdrawal_approvals_actor_proof_nonce_key`.
  The column is the durable "this decision was proven" marker. It is written ONLY by the guard; it does not depend on
  `actor_proof_nonces` (that table is prunable, ADR 0110 s6 step 6).
- `actor_proof_withdrawal_approvals_guard()` (plpgsql, NOT security definer, `search_path` pinned) on trigger
  `zz_actor_proof_guard BEFORE INSERT ON withdrawal_approvals FOR EACH ROW`. `zz_` sorts after
  `withdrawal_approvals_enforce_governance` (the only other BEFORE ROW INSERT trigger), so every existing refusal keeps its own code
  and the proof is the last gate (ADR 0110 s3 ordering rule). For `decision = 'reverse'`:
  1. reason shape `^payout_reversal:[0-9a-f]{64}$` else `23514` (same code as the 0162 CHECK, raised by the guard itself, so the
     verifier is never called on a malformed digest);
  2. `is_automated_approval` refused (`WD028`): a reversal is a proven human decision;
  3. `financial_actor_session()` (CG001 when unresolvable); scope must be `tenant` and its tenant the row's tenant (AP004);
  4. `approver_principal_id` must equal the proven actor (AP004);
  5. `actor_proof_require(actor, 'tenant', tenant, 'withdrawal_payout_reversal:decide', withdrawal_request_id::text,
     substr(reason_code, 17))` (unchanged verifier: AP001 missing/malformed, AP002 kid/MAC, AP003 window, AP004 binding,
     AP005 replay; the nonce row is consumed in the same transaction);
  6. `NEW.actor_proof_nonce :=` field 11 of the verified GUC (a caller-supplied value is overwritten).
  For any other decision: a non-NULL nonce is refused (`WD028`); otherwise unchanged (no proof, no cost).
- `withdrawal_requests_state_transition_guard()` and `ledger_transactions_payout_tombstone_guard()`: the 0162 bodies plus ONE marked
  line each, `AND a.actor_proof_nonce IS NOT NULL` in the WD026 / WD027 Person count. Messages and codes unchanged.
- `actor_proof_require` is NOT replaced: the trigger pins scope `tenant` and the target/payload, so a proof for any other scope,
  tenant, operation, target or payload fails the existing binding check (AP004). The Go signer mirrors the closed family
  (`withdrawal_payout_reversal:decide`, scope `tenant` only).
- Down: refuses `WD096` while any `actor_proof_nonce IS NOT NULL` row exists (per-tenant count under FORCE RLS, 0162 pattern), else
  restores the two 0162 bodies verbatim and drops the trigger, function, index, constraint and column (whole-schema snapshot test).

### 2.2 Go

- `internal/actorproof`: `OpPayoutReversalDecide = "withdrawal_payout_reversal:decide"`; `Claims.validate` refuses any other
  operation of the `withdrawal_payout_reversal:` family and any scope but `tenant`; `StepUpOperations` gains the operation as
  `critical` (the route's golden class; a unit test pins route class == backstop class).
- `internal/withdrawal.RecordPayoutReversalDecision` gains a REQUIRED `ReversalProofAttacher` parameter (nil = `ErrInvalidInput`),
  called after every Go check and immediately before the INSERT (the K3 pattern: typed refusals first, proof last). The Go quorum
  count and `isExecutedReversalReplay` count only `actor_proof_nonce IS NOT NULL` rows. `internal/withdrawal` still never imports
  `internal/actorproof` (the static signer allow-list is unchanged).
- `internal/payments.DecidePayoutReversal` supplies the attacher (`attachReversalProof`): `Issuer.Attach` with the decider
  (`PayoutReversalInput.ApproverID`, the verified token subject, also the session's `app.principal_id`), scope `tenant`, the
  session tenant, target = request id, payload = `PayoutReversal.Digest`. `PayoutReversalInput.Proofs` (nil = process default)
  mirrors `ResolutionCall.Proofs`. `Attach` applies the ADR 0119 signer backstop: an HTTP staff request without a fresh step-up
  for the decider is a 500 (`ErrStepUpMissing`), never a silent signature.
- `internal/reconciliation` check D: the WD026 twin counts proof-bound decisions only, read through
  `to_jsonb(a) ->> 'actor_proof_nonce'` so the one statement still runs on schemas pinned below 0165 (migration tests).

### 2.3 HTTP and back office

The route already took the decider from the token, required tenant scope, `withdrawal:approve` and step-up critical. The proof is
issued server-side in `DecidePayoutReversal` exactly as K2/K3/hold-release issue theirs in their domain services (the K1 routes
use `capability.WithProofActor`; that pattern is not needed here because the principal is passed explicitly). A signing failure
falls into the handler's existing default branch (500, logged, nothing recorded); a database AP-class refusal likewise. No body
field, response field, status, step-up class or route count changed, so the route freeze counts, the golden step-up table and the
OpenAPI document are unchanged and their pins pass as they are (re-run, section 5). No back-office screen exists for this route.

### 2.4 ADR 0113 s9.3 (`approve` / `reject`): out of scope, not the same code path

`approve` / `reject` rows are written by `withdrawal.Approve` / `Reject` and by AUTOMATED approvals whose principal is not a staff
row (0034 admits them); WD023 is a workflow-record check, not a four-eyes count. Binding them needs a decision on how automated
approvals are proven (there is no human actor to sign), a digest of the review decision, and changes to every approve/reject writer
and their tests. That is not "trivially the same code path", so the guard passes `approve` / `reject` rows through unchanged (and
refuses a nonce on them). ADR 0110 T8 therefore stays OPEN for withdrawal approve/reject and for the other flows it lists.

## 3. Changed assertions (none weakened)

| Test | Before | After | Why |
|---|---|---|---|
| `internal/payments/hsec_hold_resolution_db_integration_test.go` `TestHSEC_HoldRelease_ActorProofTriggerOnGovernedTables_Last` | 15 governed tables, `zz_actor_proof_guard` on 15 | 16 (+ `withdrawal_approvals`) | migration 0165 (stronger) |
| `internal/adjustment/actor_proof_k1_integration_test.go` `TestActorProof_ProofTriggerIsLastBeforeRowTriggerOnEveryGovernedTable` | 13 tables listed | + `withdrawal_approvals` | migration 0165 (stronger) |
| `internal/actorproof/stepup_test.go` `TestStepUpOperations_RegistryPinned` | 21 operations | 22 (+ `withdrawal_payout_reversal:decide` critical) | the signer backstop covers the new operation |
| `internal/payments/a5_payout_reversal_integration_test.go` `TestA5_PayoutReversal_WD027_OneGrantHolderIsNotAQuorum` | fixture: the non-holder's raw `reverse` row written in f1's principal scope without a proof | fixture: written as the non-holder (principal + proof), then back to f1's scope; assertion UNCHANGED (WD027, "(found 1)") | an unproven row is now refused (AP004), so the fixture must prove it to keep testing WD027's grant rule |
| `internal/withdrawal/a5_payout_reversal_db_integration_test.go` `a5Vote` (fixture used by the A5 DB tests and the 0162 migration test) | raw unproven insert | proof-bound raw insert (principal + `prooftest` proof, plain tenant session restored); every assertion UNCHANGED | WD026 now counts proof-bound rows only |
| same file, `TestA5DB_RecordDecision_CountsDistinctPersonsPerPayload` | `RecordPayoutReversalDecision(..., stepUp)` | `+ t8Attacher(staff)`; assertions unchanged | new required parameter |
| `internal/reconciliation/a5_check_d_reversal_integration_test.go` `a5DReverse` (fixture) | decisions without a proof | each decision proof-bound (`prooftest`); assertions unchanged | WD026 / check D count proof-bound rows only |
| `internal/httpserver/a5_payout_reversal_http_integration_test.go` `TestA5HTTP_PayoutReversal_AuthorizationAndQuorum` | - | + `prooftest.Install`; + assertion: both decisions carry a nonce bound in `actor_proof_nonces` to their authenticated decider, scope tenant, operation, request and digest | added assertion |

No OpenAPI, route-freeze or step-up golden pin changed (the route contract did not change).

## 4. Tests

New (all `//go:build integration` unless noted):

- `internal/withdrawal/t8_proof_bound_reversal_db_integration_test.go`
  - `TestT8DB_SR52Probe_ForgedReverseRowsRefused_AsRuntimeTenantSession`: the exact SR5-2 probe as `igaming_runtime` in a tenant
    session (two well-shaped `reverse` rows naming two real active linked staff + the posting + E11 in one transaction): CG001 (no
    principal), AP001 (impersonated principal, no proof), AP002 (proof under a wrong key); nothing committed (state `completed`, 0
    rows). Non-vacuity: the same runtime transaction with two genuine proofs executes E11. The runtime role cannot
    `DISABLE TRIGGER zz_actor_proof_guard` (42501).
  - `TestT8DB_ProofDefects_Refused` (runtime, principal-scoped session): missing / malformed (AP001), unknown kid / bad MAC (AP002),
    expired / future / lifetime > 60 s (AP003), wrong operation / target / payload / actor / scope / tenant (AP004), approver column
    != proven actor (AP004), a row of another tenant (AP004, before RLS), replay (AP005), automated reverse (WD028), nonce on a
    `reject` row (WD028); a write rolled back after consuming its proof does not burn the nonce (retry succeeds); both admitted rows
    are bound in `actor_proof_nonces`.
  - `TestT8DB_CallerSuppliedNonceIgnored`: a planted nonce without a proof is AP001; with a proof the stored nonce is the consumed one.
  - `TestT8DB_UnprovenDecisionsDoNotCount_WD026AndGo`: two unproven (WD026 found 0), one proven + one unproven (found 1), two proven
    + one unproven executes; Go count ignores unproven rows (Counted 1, then 2 = Ready); nil attacher `ErrInvalidInput`; an attacher
    that attaches nothing -> AP001.
  - `TestT8DB_ConcurrentReplayOfOneProof_ExactlyOneRow`: 4 rounds, one token in two concurrent sessions: exactly one row, the other
    AP005.
  - `TestT8DB_ProofGuardIsLastBeforeRowTrigger_Catalog`: last BEFORE ROW trigger, `BEFORE INSERT`, pinned `search_path`, not
    security definer.
  - `TestMigration0165_LegacyRowsDoNotCount_UpDownUp_DownRefusal` (scratch database): legacy rows planted at 0164 keep NULL nonces
    after the up and E11 on them is WD026 (found 0); a reversal executed at 0164 on legacy votes is NOT replayable after 0165 (a
    legacy voter's retry is `ErrStateConflict`); up -> down -> up whole-schema snapshots; down refused WD096 with a proof-bound row,
    changing nothing.
- `internal/payments/t8_proof_bound_reversal_integration_test.go`
  - `TestT8_DecidePayoutReversal_SignsForTheDecider_AndFailuresRecordNothing`: no issuer (`ErrNoIssuer`), a key the database does
    not hold (AP002), an HTTP-marked context without a fresh step-up (`ErrStepUpMissing`): each records nothing; then two decisions
    execute and both are bound to their decider's proof; balanced, projection = rebuild.
  - `TestT8_WD027_UnprovenGrantHolderDecisionsDoNotCount`: two grant holders' unproven decisions -> the payout tombstone is WD027
    (found 0); the Go quorum ignores them; a legacy voter cannot vote again (`ErrDuplicateApproval`); two other proof-bound grant
    holders complete the tombstone branch; invariants hold.
- `internal/reconciliation/t8_check_d_proof_bound_integration_test.go` `TestT8D_ReversedOnUnprovenDecisions_IsWD026`: E11 on two
  unproven decisions is refused by the guard (WD026); the owner plant past the guard is a check D WD026 finding; the proof-bound
  reversal in the same sweep is clean.
- `internal/actorproof/payout_reversal_scope_test.go` (unit) `TestActorProof_PayoutReversal_ScopeRestriction`.
- `internal/httpserver/payout_reversal_proof_stepup_test.go` (unit) `TestPayoutReversalRoute_StepUpClassMatchesSignerBackstop`.

Existing A5 tests that now run proof-bound end to end without change: every `TestA5_PayoutReversal_*` (normal, duplicates/replay,
8 Persons x 3 rounds concurrency, partial failure, authorization, step-up policy, floor of two, tombstone branch, amount dispute then
return, return reference held, grant revoked, suspended voter, in-flight attempt), `TestA5DB_*`, `TestA5D_*`, `TestA5HTTP_*`.

## 5. Commands and counts (local private databases; never CI)

Harness: `PRIV_DB=<db> PRIV_SRC=<worktree> bash priv_db.sh` (fresh database, all migrations through 0165), then
`PRIV_DB=<db> PRIV_SRC=<worktree> bash priv_test.sh <go test args>`. Databases: `t820261016151746p21529x01` (batches),
`t820261016154638p25035x02` (payments), `t820261016154747p26546x03` (mutation pass, race run, final withdrawal run); all dropped
after the run. `internal/payments` integration tests run on per-test scratch databases migrated from the source tree.

```
gofmt -l . ; go vet ./... ; go vet -tags integration ./...
golangci-lint run ./...                                    # 2.9.0
golangci-lint run --build-tags integration ./...
go test -count=1 ./...
bash priv_test.sh -tags integration -count=1 -p 1 -timeout 4h -json ./internal/payments/
bash priv_test.sh -tags integration -count=1 -p 1 -timeout 4h -json ./internal/reconciliation/... ./internal/actorproof/... \
  ./internal/db/... ./internal/adjustment/... ./internal/tenant/... ./internal/ledger/... ./internal/stafflifecycle/... \
  ./internal/launchgov/... ./internal/alerting/... ./internal/httpserver/...
bash priv_test.sh -tags integration -count=1 -p 1 -json ./internal/withdrawal/...
bash priv_test.sh -tags integration -race -count=1 -p 1 -json \
  -run 'TestT8|TestA5|TestMigration0165|TestPayoutReversalRoute|TestActorProof_PayoutReversal|TestStepUpOperations' \
  ./internal/withdrawal/ ./internal/reconciliation/ ./internal/payments/ ./internal/httpserver/ ./internal/actorproof/
```

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...`, `go vet -tags integration ./...` | clean |
| `golangci-lint run ./...` | 0 issues |
| `golangci-lint run --build-tags integration ./...` | 682 issues, the same pre-existing backlog count as the A5 record (errcheck 675, staticcheck 4, unused 3, older test files); **0 in any file this change added or touched** |
| `go test -count=1 ./...` (unit) | 69 packages ok, 0 FAIL, 8 without tests |
| `internal/payments` (integration, full package) | ok (6541 s); 2247 PASS (1139 top-level), 0 FAIL, 0 SKIP |
| `internal/withdrawal/...` (integration, full, final code) | ok (115 s); 195 PASS (109 top-level), 0 FAIL, 0 SKIP |
| Batch: reconciliation (+ statement), actorproof, db, adjustment, tenant, ledger, stafflifecycle, launchgov, alerting (+ alertingtest, channels), httpserver | 12 packages ok (`prooftest`: no tests), httpserver FAIL on the first run: 3077 PASS (2101 top-level), **4 FAIL**, 2 SKIP. The 4 failures are `TestResolutionIsolation_{OneTenantStoreOutage, MultipleTenantsOutage, ConnectionExhaustion, FinancialDuringOutage}` (tenant-resolution pool timing: "a pooled transaction stayed open 673 ms (> 400 ms)"), measured while the 109-minute payments suite and a race run loaded the same machine; they touch no code of this change. Re-run alone after the load ended, `-count=2`: 16/16 PASS. The 2 SKIPs are pre-existing environment skips (`TestVerifyEnvironmentAnchor_RealDatabase_FourStates`, `TestConformance_LogSink/classifies_every_failure`) |
| httpserver pins re-run (`TestA5HTTP|TestStaffRoute|TestStepUp_|TestPayoutReversalRoute|OpenAPI|TestAdmissibleKinds`) | 78 PASS, 0 FAIL (route freeze, golden step-up table, OpenAPI unchanged) |
| Race (`-race`, 5 packages, the T8 + A5 sets) | 5/5 ok; 76 PASS (41 top-level), 0 FAIL, 0 SKIP, 0 DATA RACE |
| Migration 0165 | up / down / up whole-schema snapshots equal; down refused WD096 with a proof-bound row; legacy rows untouched (test) |

A first batch run was lost to an environment fault, not a test result: the shared Go build cache was cleaned by another process
mid-run (`could not import ... open /root/.cache/go-build/...: no such file or directory`, build failures in 12 packages before any
test ran); the batch was re-run in full (the table above).

## 6. Mutation pass (kill or justify)

Driver: `mut.py` in the session scratchpad (not committed). SQL mutants replace ONE live function body on a private mutation database
(`pg_get_functiondef` + one text substitution) or run one statement pair, run the targeted tests, then restore and assert the body is
back; FILE mutants edit ONE spot of a copy of the source tree (Go, or a migration file for the scratch-database tests), run the
targeted tests, restore. KILLED = the targeted tests fail with the mutant in place.

Result: **27 mutants run; 25 KILLED, 2 SURVIVED (both justified, and killed together as G11)**. Two mutants survived the first
round (S11, G09); S11 was killed by an added case (a row of another tenant), G09 is justified (with G10) and the pair is killed (G11)
by an added case (a legacy voter's retry on a pre-0165 reversal).

| # | Mutant | Targeted tests | Result |
|---|---|---|---|
| S01 | guard: `actor_proof_require` call removed | `TestT8DB_SR52Probe`, `TestT8DB_ProofDefects` | KILLED |
| S02 | guard: approver = proven actor check removed | `TestT8DB_ProofDefects` | KILLED |
| S03 | guard: payload not bound (uses the proof's own payload field) | `TestT8DB_ProofDefects` | KILLED |
| S04 | guard: target not bound (the proof's own target field) | `TestT8DB_ProofDefects` | KILLED |
| S05 | guard: operation not bound (the proof's own operation field) | `TestT8DB_ProofDefects` | KILLED |
| S06 | guard: nonce not recorded | T8 + A5 DB tests | KILLED |
| S07 | guard: a caller-supplied nonce is kept | `TestT8DB_CallerSuppliedNonceIgnored` | KILLED |
| S08 | guard: nonce on a non-`reverse` row admitted | `TestT8DB_ProofDefects` | KILLED |
| S09 | guard: automated `reverse` admitted | `TestT8DB_ProofDefects` | KILLED |
| S10 | guard: reason-shape check removed | `TestA5DB_ReverseDecisionRow_Guards` | KILLED |
| S11 | guard: tenant-session / tenant check removed | T8 + A5 DB tests | KILLED (survived first; killed by the added another-tenant case) |
| S12 | trigger no longer last (renamed `aa_`) | `TestT8DB_ProofGuardIsLastBeforeRowTrigger_Catalog` | KILLED |
| S13 | trigger disabled | `TestT8DB_SR52Probe` | KILLED |
| S14 | WD026: proof filter removed | `TestT8DB_UnprovenDecisionsDoNotCount_WD026AndGo` | KILLED |
| S15 | WD027: proof filter removed (migration source, scratch databases) | `TestT8_WD027_UnprovenGrantHolderDecisionsDoNotCount` | KILLED |
| S16 | down: WD096 refusal removed | `TestMigration0165_*` | KILLED |
| G01 | Go quorum count: proof filter removed | `TestT8DB_UnprovenDecisionsDoNotCount_WD026AndGo` | KILLED |
| G02 | Go: attacher not called | same + `TestA5DB_RecordDecision_*` | KILLED |
| G03 | Go: nil attacher admitted | `TestT8DB_UnprovenDecisionsDoNotCount_WD026AndGo` | KILLED |
| G04 | payments: proof target = tenant id | `TestT8_DecidePayoutReversal_*` | KILLED |
| G05 | payments: attacher ignores `PayoutReversalInput.Proofs` | `TestT8_DecidePayoutReversal_*` (unknown-key case) | KILLED |
| G06 | check D: proof filter removed | `TestT8D_*`, `TestA5D_*` | KILLED |
| G07 | signer: `withdrawal_payout_reversal:` scope restriction removed | `TestActorProof_PayoutReversal_ScopeRestriction` (unit) | KILLED |
| G08 | signer: decide not step-up classified | `TestT8_DecidePayoutReversal_*` (backstop case) | KILLED |
| G09 | Go replay: quorum-count proof filter removed | T8 + A5 DB + 0165 migration tests | SURVIVED, **justified** |
| G10 | Go replay: own-vote proof filter removed | same | SURVIVED, **justified** |
| G11 | Go replay: BOTH proof filters removed | `TestMigration0165_*` (legacy voter's retry) | KILLED |

Justified:
- G09 / G10: the two proof filters of `isExecutedReversalReplay` are mutually redundant. After 0165 every executed reversal has a
  proof-bound quorum (WD026), and a vote can no longer be added once the request is terminal, so a replay candidate is either all
  proof-bound (post-0165) or all legacy (pre-0165): either filter alone decides it. Removing both is killed (G11). A replay records
  and posts nothing in any case (only the 200 / 409 label differs).
- The scope arm of the guard's session check (S11 is killed through its tenant arm): a `platform_acting` session would need a proof
  signed for scope `tenant` and a platform principal; the verifier binding and the Go signer (scope `tenant` only, for the tenant-scoped
  token subject behind `RequireTenantScope`) never produce one. It is kept as a restatement (defence in depth).

## 7. Residuals

- **T5 / E10 NOT closed (security s12.3 addition 2).** WD027 binds the payout TOMBSTONE to proof-bound decisions of grant holders,
  but the tombstone branch's hold release is the ordinary E10 `withdrawal.Fail` posting, which a runtime session can still reach
  through the ordinary fail path without any decision (ADR 0110 T5: non-governed posting paths). Bounded by the ledger invariants
  and check D; not by a proof.
- **ADR 0110 T8 stays OPEN** for every other flow it lists (withdrawal `approve` / `reject` = ADR 0113 s9.3 / WD023, payment
  kill-switch release, provider credential handles, KYC enforcement policy, asset registry, bonus change governance, casino
  catalogue). Only the `reverse` decisions are closed.
- **R-T8-1 (LOW, dev data only).** A `reverse` row recorded before 0165 keeps a NULL nonce, counts nowhere and still occupies its
  approver's one-`reverse`-per-request slot (0162 UNIQUE): that approver gets `ErrDuplicateApproval` (409) on that request; other
  Persons complete it. There is no production data.
- **R-T8-2 (LOW, dev data only).** A request reversed before 0165 (legacy quorum) is reported by check D as `WD026` after 0165
  (its decisions are unproven) and a legacy voter's retry is a 409, not `already_executed`. Intended (unproven = not counted); on
  development or staging databases that ran A5 before 0165 this is expected, not an incident.
- **ADR 0110 s9 residuals apply unchanged:** application-host compromise (T6), identity-store integrity (T3: a minted or taken-over
  staff login obtains genuine proofs), symmetric key readable by the owner (T7), the 30 s wire window of an unused proof, the
  owner `DISABLE TRIGGER` class (A5 R2; check D's WD026 twin now detects a reversal whose decisions are unproven).
- **Unchanged A5 items:** SR5R-1 (a pre-existing tombstone on the payout reference skips WD027 and the late-success cell), R3, R6,
  R8 (the posting = voted payload is Go-level), R9, vote expiry not modelled.
- **Mutation survivors (justified, section 6):** the scope arm of the guard's tenant-session check (defence in depth behind the
  verifier binding and the Go signer, which only signs scope `tenant` for a tenant-scoped token subject); the two proof filters of
  `isExecutedReversalReplay` are mutually redundant (each alone survives, both removed is killed); a replay never records or posts.

## 8. Review probe lists

### 8.1 Security

1. SR5-2 re-run: as `igaming_runtime` in a tenant session, two `reverse` rows + posting + E11 -> refused at the first row (CG001 /
   AP001 / AP002), nothing committed; with `app.principal_id` set to a real staff id -> AP001.
2. Guard ordering: `zz_actor_proof_guard` sorts after `withdrawal_approvals_enforce_governance`; no column the guard binds
   (`withdrawal_request_id`, `reason_code`, `approver_principal_id`, `tenant_id`, `decision`) is modified by the earlier trigger.
3. Binding completeness: actor (= approver column = session principal), scope pinned `tenant`, tenant (= row tenant), operation,
   target (= request id), payload (= reason digest), iat/exp, kid, nonce. Confirm no way to get a proof-bound row whose digest
   differs from its `reason_code`.
4. The nonce column: written only by the guard (caller value overwritten on `reverse`, refused elsewhere), shape CHECK, UNIQUE;
   counting relies on it, not on the prunable `actor_proof_nonces`.
5. Signer: `withdrawal_payout_reversal:decide` is signable only for scope `tenant`; step-up critical backstop; `internal/withdrawal`
   does not sign (the static signer allow-list is unchanged); signing failures are 500, never 403, never skipped.
6. Down migration refusal WD096; legacy rows behaviour (R-T8-1/2).
7. T5/E10 explicitly not closed; T8 remains open for `approve` / `reject` and the other listed flows.

### 8.2 Ledger-finance (independent)

1. No financial semantic changed: the posting shapes, keys, UNIQUEs, state machine, check A, WD035/WD044 are untouched; the proof is
   an extra precondition on the decision row, and the only counting change is the proof filter (DB WD026/WD027, check D twin, Go).
2. Idempotency (ADR 0110 s5): fresh proof per attempt; the nonce rolls back with a failed transaction (tested); the business
   idempotency (`<id>:reversed`, provider UNIQUE, one reversal per completion, replay) is unchanged; a replay records nothing.
3. Concurrency: 8 Persons x 3 rounds still post exactly once; one captured proof in two sessions admits one row (AP005).
4. Positive paths assert one posting, balanced ledger, projection = rebuild (normal, concurrency, T8 signing test).
5. Legacy data: unproven rows do not count anywhere; down refuses while proven rows exist.

## 9. READY-TO-PASTE ADR 0110 amendment (the orchestrator is the single writer)

Insert as a new pointer bullet at the top of `docs/decisions/0110-signed-actor-proof.md`:

> - **Implemented (2026-10-16, migration 0165, ADR 0110 T8 amendment, owner decision 2026-10-16 item 2):** the `reverse` decisions of
>   `withdrawal_approvals` (A5 governed payout reversal) are proof-bound (sixteen governed tables). Operation
>   `withdrawal_payout_reversal:decide`, scope `tenant` only, target = the withdrawal request id, payload hash = the payout digest the
>   decision's `reason_code` carries (`payout_reversal:<sha256>`, `withdrawal.PayoutReversal.Digest`). `zz_actor_proof_guard` (BEFORE
>   INSERT, last BEFORE ROW trigger, catalog-pinned) also requires the approver column to equal the proven actor and the session tenant
>   to equal the row tenant, refuses automated `reverse` rows and records the consumed nonce in `withdrawal_approvals.actor_proof_nonce`;
>   WD026 (E11), WD027 (tombstone branch), reconciliation check D's WD026 twin and the Go quorum count only rows with that nonce.
>   `approve` / `reject` rows are unchanged (ADR 0113 s9.3 stays open). The issuer is `internal/payments.DecidePayoutReversal`
>   (step-up critical backstop). Record: `docs/plans/t8-proof-bound-reversal-record.md`.

Append to the section 3 table:

> | `withdrawal_approvals` | INSERT with `decision = 'reverse'` | `withdrawal_payout_reversal:decide` | the withdrawal request id | the payout digest (`reason_code` without the `payout_reversal:` prefix); scope `tenant` only; `approver_principal_id` must be the proven actor; the consumed nonce is stored in `actor_proof_nonce` and only such rows count (WD026 / WD027 / check D) (migration 0165) |

Replace the T8 row of the section 10a threat register with:

> | T8 | Other dual-control governance flows OUTSIDE the proof-bound tables still take the actor identity from GUCs or app-supplied values with NO proof: payment kill-switch release (0105), provider credential handles (0096), KYC enforcement policy (0103), asset-registry dual control (0047), withdrawal `approve` / `reject` records and policies (0026/0034; ADR 0113 s9.3), bonus change governance (0063), casino catalogue dual control (0086/0089). A runtime session impersonating two admins can still approve in those flows. **Withdrawal payout-reversal (`reverse`) decisions: CLOSED by migration 0165** (owner decision 2026-10-16) | PARTIALLY MITIGATED: `reverse` closed; the rest NOT mitigated, DEFERRED-REQUIRES-OWNER-AUTHORIZATION |

Add to the T5 row's State column:

> ... NOT by an actor proof. Note (migration 0165): binding the payout-reversal decisions does NOT close T5 for the A5 tombstone
> branch: its hold release is the ordinary E10 `withdrawal_failed` posting, reachable by a runtime session through the ordinary fail
> path without any decision; WD027 binds only the payout tombstone row.
