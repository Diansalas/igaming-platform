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

## 10. Security review (independent, `security`, 2026-10-16)

Scope: the branch diff against its merge-base with `origin/claude/focused-wright-jw88w9` (`12edec0`, 4 commits): migration 0165 up/down,
`internal/actorproof` (operation, signer scope rule, step-up registration), `internal/withdrawal` (attacher, Go quorum, replay check),
`internal/payments` (issuer), `internal/reconciliation` check D, the HTTP handler (read, unchanged), the runbook note and the ADR 0110
amendment text in section 9. Read against ADR 0110, ADR 0113, migration 0124 (precedent), 0162, 0034, 0026 and the current
`actor_proof_require` (0139 body). Code- and design-level review on private scratch databases only (PostgreSQL 16.13); not a
penetration test. Not in scope: ADR 0113 s9.3 (`approve` / `reject`), T5 / E10, the other T8 flows, production key provisioning.

**Verdict: APPROVE.** No HIGH or MEDIUM finding. SR5-2 / A5 R1 is closed for the `reverse` decision rows against the
`igaming_runtime` threat model of ADR 0110 (THREAT-MODEL-ARBITRARY-SQL-1); the ADR 0110 s9 residuals (T3, T6, T7, owner class, wire
window) apply unchanged and are NOT closed by this change. SRT8-1..3 are documentation accuracy items, non-blocking.

### 10.1 Verified by execution (scratch databases `t8s20261016174135p19928x01`, `t8s20261016174150p20304x02`, both dropped)

- Targeted suites green: `TestT8DB_*` (6), `TestMigration0165_*`, `TestT8D_*`, `TestT8_*` (payments, 2), every `TestA5_PayoutReversal_*`
  (14), `TestA5DB_*`, `TestA5D_*`, `TestA5HTTP_*`, `TestPayoutReversalRoute_*`, `TestActorProof_PayoutReversal_*`,
  `TestStepUpOperations_*`, both governed-table catalog pins (16 tables carry `zz_actor_proof_guard`, confirmed in the catalog);
  `go vet -tags integration` clean on the touched packages.
- Additional probes (temporary test file, NOT committed; as `igaming_runtime`, principal-scoped tenant session unless noted):

| Probe | Result |
|---|---|
| `decision` = `Reverse` / `REVERSE` / ` reverse` / `reverse ` / `reverse\n` (no proof) | 23514 (0162 decision CHECK; the guard passes them, the CHECK refuses, so no non-canonical `reverse` row can exist); NUL byte 22021 |
| `reason_code` with a trailing newline, valid proof | 23514 (guard shape check; ARE `$` is end-of-string) |
| `COPY` a `reverse` row, with and without a planted nonce | 0A000 (COPY FROM refused under RLS; the BEFORE ROW guard would fire on COPY in any case) |
| multi-row INSERT, one proof, two approvers | AP004 (second row's approver is not the proven actor) |
| `INSERT ... ON CONFLICT (one reverse per approver) DO UPDATE`, no proof / fresh valid proof | AP001 / P0001 append-only (the BEFORE INSERT guard runs before conflict resolution; the UPDATE arm hits `withdrawal_approvals_immutable`) |
| UPDATE a proven row's approver; DELETE it; UPDATE a legacy (pre-0165 shape) row to add a nonce | P0001 append-only (all three) |
| tenant + principal + `app.platform_admin_principal_id` (mixed shape), valid proof | CG001 |
| player GUC set, valid proof | CG001 |
| acting GUCs set, valid tenant proof | CG020 |
| token with 13 fields; token with an upper-case hex MAC | AP001 / AP001 |
| session of ANOTHER tenant naming this tenant's staff and row | refused by the 0034 governance trigger (staff row invisible under RLS) before the guard |
| `SET session_replication_role = replica` | permission denied |
| `ALTER TABLE ... DISABLE TRIGGER zz_actor_proof_guard`, `DROP TRIGGER`, `CREATE TRIGGER`, `TRUNCATE` | must be owner / permission denied (runtime has no TRIGGER / TRUNCATE privilege; the 0026 no-truncate trigger also applies) |
| read `actor_proof_keys`; read / write `actor_proof_nonces`; `CREATE FUNCTION` in `public`; `CREATE TEMP TABLE` | permission denied (no shadowing of the guard's unqualified `financial_actor_session` / `actor_proof_require` calls is possible; the guard's `search_path` is pinned in any case) |
| after all refusals, one genuine proof per approver | admitted; stored nonce = the consumed token's field 11; `approve` rows of the fixture carry NULL (pass-through) |

- SECURITY DEFINER functions in `public` (27) were scanned for writes to `withdrawal_approvals`, `DISABLE TRIGGER`,
  `session_replication_role`, `app.actor_proof` or `app.principal_id`: only `actor_proof_require` (the verifier) and `aml_readiness`
  (sets and restores `app.principal_id` around a read; no approvals write) match. No definer path inserts a `reverse` row.

### 10.2 Verified by reading

- Guard: last BEFORE ROW trigger (`zz_` after `withdrawal_approvals_enforce_governance`, which modifies no bound column); NOT security
  definer; `search_path` pinned; `actor_proof_require(actor, 'tenant', session tenant, 'withdrawal_payout_reversal:decide',
  request id, substr(reason_code, 17))`; approver must equal `financial_actor_session().actor`; session scope `tenant` and session tenant =
  row tenant; automated `reverse` refused (WD028); nonce on non-`reverse` refused (WD028) and also by the CHECK; nonce taken from the same
  GUC the verifier just consumed in the same trigger invocation (no interleaving possible). Rolled-back proof is reusable within its 60 s
  window by design (ADR 0110 s5), tested.
- Counting: the WD026 guard, the WD027 tombstone guard, check D's WD026 twin, the Go quorum and BOTH arms of `isExecutedReversalReplay`
  filter `actor_proof_nonce IS NOT NULL`. No other reader of `decision = 'reverse'` exists in Go or SQL (grep). The HTTP handler
  derives only from `DecidePayoutReversal`'s result, so it cannot report a quorum the counts deny.
- Pre-0165 rows: NULL nonce, counted nowhere, still occupy the approver's one-`reverse`-per-request slot (R-T8-1). Acceptable: it can
  only reduce who may vote (fail-closed), never add a vote; no production data exists, and a post-0165 attacker cannot occupy a slot
  without a proof.
- Go: `OpPayoutReversalDecide` constant; signer refuses any other `withdrawal_payout_reversal:` operation and any scope but `tenant`;
  step-up registry `critical`, equal to the route's golden class (unit test); attacher REQUIRED (nil = `ErrInvalidInput`) and called
  after every typed Go refusal, immediately before the INSERT; `Issuer.Attach` errors (`ErrNoIssuer`, `ErrStepUpMissing`, signing) fall
  into the handler's default branch: 500, logged without the token, transaction rolled back, nothing recorded. `internal/withdrawal`
  does not import `internal/actorproof` (signer allow-list unchanged). The decision runs in `NewPrincipalRunner(pool, tenant,
  ApproverID)`, ApproverID is the verified token subject. Audit rows: unchanged fields (actor, IP, UA, request id, outcome, counts,
  payload digest); no token, key or nonce is written.
- HTTP contract unchanged (body, responses, step-up class, route counts); no back-office screen exists.
- Down: refuses WD096 while any proof-bound row exists (per-tenant count under FORCE RLS); otherwise restores the two 0162 bodies
  (verified textually identical to 0162, and by the whole-schema snapshot test) and drops trigger, function, index, constraint, column.
  A down to 0164 re-opens SR5-2 by definition; the refusal guarantees it never silently re-counts proven rows. Honest.
- ADR 0113 s9.3: the guard returns `approve` / `reject` rows unchanged (nonce must be NULL); WD023 untouched. T5 / E10: unchanged and
  correctly labelled NOT CLOSED (the tombstone branch's hold release is the ordinary E10 posting).

### 10.3 Findings

- **SRT8-1 (LOW, documentation).** The 0165 up header cites a whole-schema snapshot test
  `internal/withdrawal/t8_migration_0165_integration_test.go`, which does not exist; the test is
  `TestMigration0165_LegacyRowsDoNotCount_UpDownUp_DownRefusal` in `internal/withdrawal/t8_proof_bound_reversal_db_integration_test.go`.
  Fix the comment before merge (unmerged migration, comment-only change). Non-blocking.
- **SRT8-2 (INFO, claim accuracy; owner class).** Section 7 states that check D's WD026 twin "now detects a reversal whose decisions
  are unproven". That holds for NULL-nonce rows only. An actor holding the table OWNER (ADR 0110 s9.4) can disable
  `zz_actor_proof_guard` and plant `reverse` rows with any shape-valid, unique nonce; the CHECK and UNIQUE do not bind the nonce to a
  consumed proof, so WD026, WD027 and check D all count them. No regression (before 0165 the owner could plant counting rows too) and
  the owner class is an accepted ADR 0110 residual, but the record / ADR must not imply detection of an owner forgery. Optional
  future hardening (not required): check D could join `actor_proof_nonces` and flag a counted nonce that is absent or not bound to
  (approver, tenant, operation, request id, digest) while the nonce is inside the retention window.
- **SRT8-3 (INFO, ADR text in section 9).** (a) "(sixteen governed tables)" should say that on `withdrawal_approvals` only `reverse`
  rows are bound, e.g. "(sixteen tables carry `zz_actor_proof_guard`; on `withdrawal_approvals` only `decision = 'reverse'` rows are
  proof-bound)"; (b) the list of counters should include the Go replay check (`isExecutedReversalReplay`); (c) add to the T8 row that
  the owner-class residual (SRT8-2) applies to the nonce marker. The rest of the section 9 text (operation, scope, target, payload,
  ordering, WD codes, T5 note, T8 state PARTIALLY MITIGATED with `approve` / `reject` still open) is accurate against the code.

### 10.4 Launch flag

Nothing in this change blocks launch on its own. The open items that still bear on real-money launch are unchanged by it and remain
owner decisions: ADR 0110 T8 for the other flows (including withdrawal `approve` / `reject`, ADR 0113 s9.3), T5 (including the A5
tombstone branch's E10 hold release), and the ADR 0110 s9 inherent residuals (T6 application-host compromise holds the signing key).

## 11. Ledger-finance review (independent, `ledger-finance`, 2026-10-16)

Scope: the branch diff against its merge-base with `origin/claude/focused-wright-jw88w9` (`12edec0`), read against ADR 0113, ADR 0082,
migrations 0162 and 0165 (up and down), the 0139 `actor_proof_require` body and `docs/plans/a5-payout-reversal-record.md`. Separate from
the security review in section 10; no overlap in findings numbering (LFT8-n).

**Verdict: APPROVE.** No HIGH or MEDIUM finding. The money path is unchanged; the only semantic change is that a `reverse` decision
needs a consumed proof to exist and only proof-bound decisions count (WD026, WD027, check D twin, Go quorum, Go replay).

### 11.1 Verified by execution (private scratch database `t8l20261016174112p19219x01`, dropped after the run)

- `-run 'T8|A5|TestADR0113|TestMigration0165|TestMigration0162|TestActorProof|TestStepUpOperations|TestPayoutReversalRoute'` over
  `internal/withdrawal`, `internal/reconciliation`, `internal/actorproof/...`, `internal/adjustment`, `internal/httpserver`: 5/5 ok,
  70 top-level / 130 total PASS, 0 FAIL, 0 SKIP.
- `-run 'T8|A5|TestADR0113|TestHSEC_HoldRelease_ActorProofTriggerOnGovernedTables_Last'` over `internal/payments` (per-test scratch
  databases): ok, 22 top-level / 47 total PASS, 0 FAIL, 0 SKIP. Includes the A5 normal, duplicates/replay, 8 Persons x 3 rounds
  concurrency (exactly one posting), partial failure, tombstone branch, grant revoked, suspended voter, in-flight attempt and floor-of-two
  tests, now running proof-bound end to end, and the two T8 payments tests (balanced ledger, projection = rebuild).
- Catalog (scratch database at 0165): BEFORE ROW triggers on `withdrawal_approvals` are `withdrawal_approvals_enforce_governance`,
  `zz_actor_proof_guard` (INSERT) and `withdrawal_approvals_immutable` (UPDATE / DELETE, unconditional `ledger_deny_mutation`), so the
  nonce marker cannot be added to a legacy row after the fact (security 10.1 probed the same as `igaming_runtime`).
- Function bodies (text comparison of the migration files): the 0165 up bodies of `withdrawal_requests_state_transition_guard` and
  `ledger_transactions_payout_tombstone_guard` differ from 0162 by exactly the one marked line each (plus `CREATE` -> `CREATE OR
  REPLACE` on the tombstone guard header); the 0165 down bodies are byte-identical to 0162 (8264 and 1374 bytes). The snapshot test
  hashes `prosrc`, `proconfig`, `prosecdef` and ACLs, so the restoration is also asserted in the catalog.

### 11.2 Money path (verified by reading)

- No new ledger write path. `ReverseCompletedPayout` (E11 posting `withdrawal_reversed` Dr psp_clearing / Cr player_cash, key
  `<id>:reversed`), the provider UNIQUE on the return reference, one reversal per completion (WD035 / WD044, E11 only out of
  `completed`), the tombstone branch (tombstone + E10 `withdrawal.Fail` hold release), check A and check D's other arms are untouched
  by the diff. The proof nonce is not an idempotency key of any posting; the business idempotency is unchanged.
- Quorum: the Go count, `isExecutedReversalReplay` (both arms), WD026, WD027 and the check D twin carry the same
  `actor_proof_nonce IS NOT NULL` filter; nothing else reads `decision = 'reverse'`. The Go count and WD026 can therefore not disagree
  about a proof-bound quorum, so a Go "Ready" is never followed by a database WD026 on the same rows.
- Lock order (ADR 0082): the new acquisitions are (a) the `actor_proof_nonces` insert (`ON CONFLICT (nonce) DO NOTHING`, random nonce),
  (b) the partial UNIQUE index entry on `withdrawal_approvals.actor_proof_nonce`, (c) plain reads of `staff_users` and
  `actor_proof_keys` (no row locks). On the Go path they are taken after `LockForPayoutEvidence` / `lockRequestForUpdate` (and the
  attempt locks on the submitted branch) and before any ledger / projection lock, the same position as the K3 and hold-release proofs.
  The only possible wait on (a) or (b) is between two consumers of the SAME token, which is bound to one request id: on the Go path
  both already hold that request's row lock, so they serialise there first. No new cycle with request, attempt, staff or ledger locks.
  The down migration takes `SHARE ROW EXCLUSIVE` on `withdrawal_approvals` only (migration-time, 0162 pattern).

### 11.3 Concurrency and retries (reading + the executed tests above)

- Two approvers voting simultaneously: both take the request row lock first and serialise; each consumes its own proof; the second
  sees the first's committed row and executes (8 x 3 concurrency test: exactly one posting).
- Retry by the approver whose vote completed the quorum: the request is `reversed` (or `failed` on the tombstone branch), so
  `RecordPayoutReversalDecision` returns through `isExecutedReversalReplay` BEFORE the attacher: no proof is signed or consumed, nothing
  is recorded or posted (200 replay).
- Retry by an approver whose vote is on file but the quorum is not reached: a fresh proof is consumed, the INSERT hits the
  (request, approver, decision) UNIQUE -> `ErrDuplicateApproval` (409); the transaction rolls back, so the nonce is not burnt.
- Vote while `submitted` (tombstone branch): unchanged plus the proof; WD027 counts proof-bound grant holders only (T8 payments test).
  Vote after completion: the normal branch. Vote by a third Person after execution: `ErrStateConflict`.
- Reversal racing completion (E9): both take the request row lock; unchanged from A5 / ADR 0113 (concurrent edge race tests pass).
- Rolled-back transaction: the `actor_proof_nonces` row and the decision row roll back together; a retry with the same proof inside
  its 60 s window succeeds (tested in `TestT8DB_ProofDefects_Refused`); no counted row survives a rollback.
- `alerting.InTx` re-runs the closure on a retry: each run signs a fresh proof (the attacher is invoked inside the closure).

### 11.4 Legacy (pre-0165) rows

- R-T8-1 (a legacy `reverse` row occupies its approver's one-per-request slot, counts nowhere): fail-closed; it can only remove a
  voter, never add a vote; other Persons can complete the request. Harmless.
- R-T8-2 (a request reversed before 0165 shows as check D WD026, P1): NOT a false-P1 risk in any real environment. No production
  data exists, and the AWS staging run has not been executed (`docs/progress.md` status line), so no shared environment holds
  pre-0165 reversals. Only local development databases that executed A5 reversals between 0162 and 0164 would show it.
- Recommendation: **no backfill and no exemption.** A backfill would have to invent a nonce for an unproven decision (a forged proof
  marker, and it requires disabling `withdrawal_approvals_immutable`); an exemption list in check D or WD026 would be a weakening of
  the detective twin. Instead, condition for the staging deployment (LFT8-2 below): the named staging commit must include 0165, and
  any local development database with pre-0165 reversals is rebuilt rather than patched.

### 11.5 Findings

- **LFT8-1 (LOW, detective accuracy on old schemas).** Check D reads the nonce through `to_jsonb(a) ->> 'actor_proof_nonce'`. On a
  schema pinned below 0162 no request can be `reversed`, so the arm is inert; on a schema pinned at 0162-0164 every `reversed` request
  would be reported WD026 (the key is absent, read as NULL). The statement is robust (it parses and runs at every version, same
  technique as the 0162 `reversal_ledger_transaction_id` read); no current test runs check D at 0162-0164 with a reversal, and the
  application always runs against the head schema. Accept; if a future migration test pins 0162-0164 and runs check D on a reversal,
  it must expect WD026. Non-blocking.
- **LFT8-2 (LOW, operational condition).** Pre-0165 reversals become standing check D WD026 P1 findings (R-T8-2). Condition: the AWS
  staging deployment commit includes migration 0165 (deploy from a commit at or after the T8 merge), so no shared environment ever holds
  legacy reversals. Add one line to the staging deploy plan / runbook when T8 is merged (orchestrator). Non-blocking for the merge.
- **LFT8-3 (INFO).** The decision INSERT now has two UNIQUE indexes that can raise 23505 (one-`reverse`-per-approver and the nonce
  index); `db.IsUniqueViolation` maps both to `ErrDuplicateApproval` (409). The nonce-index arm is unreachable in practice (a replayed
  token is AP005 at `actor_proof_nonces` first, and a pruned nonce is past expiry, AP003) and would in any case be a refusal with nothing
  recorded. Optional: map by constraint name. No change required.
- **LFT8-4 (INFO, documentation).** ADR 0082 s1.7 (`internal/withdrawal` inventory) does not mention that a reversal decision now
  inserts into `actor_proof_nonces` after the request row lock and before any ledger lock (11.2). Doc-only addition for the orchestrator.
- **LFT8-5 (INFO, concurrence).** Concur with SRT8-1 (stale test-file name in the 0165 up header; comment-only fix) and SRT8-2 (the
  nonce marker is not cross-checked against `actor_proof_nonces`; owner-class only, no regression). From the ledger side, the
  optional SRT8-2 hardening of check D would be read-only and is acceptable if pursued later; it is not required for this merge.

No changed assertion is a weakening: the catalog pins go 15 -> 16 tables and the step-up registry 21 -> 22 (strictly more coverage);
the A5 fixtures (`a5Vote`, `a5DReverse`, the WD027 one-grant-holder fixture) now write proof-bound rows because unproven rows no longer
count, and every original assertion (codes, "(found n)" messages, posting counts, balances) is unchanged.
