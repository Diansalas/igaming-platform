# F4 residual round 1: signed dispatch-gate decision (SF4-1 / R1), F4-R2-NEGBAL-SQL, SF4-5

Status: implementation record on branch `f4r1-signed-dispatch-decision` (from 76187cc, migration head 0170 -> 0171). Proposed
registry text only: this file does not edit the task registry, HANDOVER, progress, active-stage, the decision register or any
ADR. All providers are MOCK. Software capability only; no regulatory approval is claimed. Needs `security` and
`ledger-finance` review before merge (probe lists in section 9).

## 1. Proposed registry text

| Item | Label | Note |
|---|---|---|
| SF4-1 / R1: runtime credential forges an ALLOW row in `withdrawal_gate_decisions` | `IMPLEMENTED` (closed against the owner-decided SQL-only attacker; ADR 0110 T6 residual) | The allow is a signed, nonce-consumed system actor proof verified in SQL by the existing SECURITY DEFINER verifier. WD053 counts only proof-bound allows. |
| F4-R2-NEGBAL-SQL (negative balance in the SQL backstop) | `NOT IMPLEMENTED` in SQL (decision: accepted, subsumed by R1 for the forged-row attacker; tracked follow-up unchanged) | The SQL clause was built and proven, then withdrawn (section 5). |
| SF4-5 (fail-closed gate is silent) | `IMPLEMENTED` (raise); notification `NOT IMPLEMENTED` | New alert Kind `withdrawal.payout_gate_unavailable` (p2 engineering default, detached). ALERT-DELIVERY-1 still open. |
| SF4-3 (T2/T12 escalation not sticky) | unchanged documented behaviour | Runbook and R5 text already state it; no code change. |
| Migration 0171 | `IMPLEMENTED` | up/down/up byte-identical snapshot, WD095 down refusal. |

## 2. Exact threat model of R1 (before the fix)

### 2.1 Objects, grants, policy

- Table `withdrawal_gate_decisions` (migration 0169): owner `igaming`, FORCE RLS, policy `tenant_staff_scope` FOR ALL
  (`tenant_id = app.tenant_id` AND no `app.player_account_id`), `igaming_runtime` holds exactly `SELECT, INSERT`
  (`deploy/init-app-role.sql` mirrors it). Append-only (WD055 owner included). The BEFORE INSERT trigger sets `id`, `decided_at`,
  `decided_xact` and refuses (WD054) a row whose request is not visible in the tenant, a `dispatch` row for a request that is not
  `approved`, a `redispatch` row for a request that is not `submitted` or whose attempt is not a payout attempt of the request.
- Guards that read it: `withdrawal_requests_zz_dispatch_gate_guard` (E7 `approved -> submitted`),
  `payment_attempts_zz_payout_redispatch_gate_guard` (payout `created|ambiguous -> submitting`, T2/T12),
  `payment_attempts_zz_payout_dispatch_insert_guard` (payout attempt born `submitting`, T1p). All call
  `withdrawal_dispatch_decision_ok` (WD053: an allowing row recorded in THIS transaction and no denying one) and
  `withdrawal_dispatch_rg_violation` (WD052: account / wallet status).
- The runtime principal: any session of the `igaming_runtime` role. `app.tenant_id` is a transaction-local GUC the role sets
  itself (`set_config`), so the RLS policy admits an INSERT for any tenant the attacker names.

### 2.2 Exploit path (SQL-only actor with the stolen `igaming_runtime` credential, before 0171)

One transaction: `set_config('app.tenant_id', <victim>, true)`; `INSERT INTO withdrawal_gate_decisions (tenant_id,
withdrawal_request_id, stage, allowed) VALUES (..., 'dispatch', true)` (the request must already be `approved`);
`UPDATE withdrawal_requests SET state='submitted', provider_id=...` (E7); `INSERT INTO payment_attempts (... payout,
'submitting' ...)` plus the destination snapshot the deferred PI055 check wants. The same pattern with `stage='redispatch'`
drives a payout attempt `created|ambiguous -> submitting` (T2/T12). This was confirmed by the security probe of the F4 record
(section 14: "forged allow row, then E7: ADMITTED").

### 2.3 What it buys, financially and for compliance

Bypassed: the dispatch-time WD-RG-1 evaluation as EVIDENCE. The property "every submitted payout carries a decision of the
code that evaluated the RG half (account / wallet status, negative balance WD050) and the risk rules (WD051, deny / review)" is
destroyed: a request can be moved to `submitted` and an attempt to `submitting` although the risk outcome was deny or review or
the wallet is negative. The WD052 status re-check stays in force (it is read in SQL, not recorded). The decision table stops
being trustworthy compliance history: an auditor would read an ALLOW that no code path took.

NOT bypassed (all verified by reading the guards, not assumed): WD052 (account / wallet status, re-read in SQL at the
transition); every other E7 / attempt guard (state machine WD020-WD025, column discipline, provider_id, the B13-B destination
snapshot, the kill-switch predicate); amount, destination instrument and asset (immutable columns, binding guard 0126); the
ledger (the hold was posted at request time, balanced, append-only; E7 posts nothing); the four-eyes APPROVAL that put the
request in `approved` (a separate record, see the caveat below).

Does the forgery by itself make the platform pay? To the extent I could verify, NO. The only provider `Withdraw` call sites are
`DispatchWithdraw` from the HTTP T1p claim (after the app's own claim transaction and gate evaluation) and
`dispatchPayoutAttempt` after the app's own T2/T12 claim (pinned by `b13b_static_test.go`: exactly 2 production callers). A
`submitting` attempt a SQL actor created is only polled (`resolvePayoutViaQueryStatus`); the sweeper's resend of an `ambiguous`
attempt goes through T12, which re-runs the gate in application code. So the SQL-only forgery fabricates dispatch STATE and
evidence; moving money additionally needs the application to call the provider, which it does only inside its own gated
claims. I do not claim this is an exhaustive proof (a future code path that calls `DispatchWithdraw` after a claim made
elsewhere would combine with the forgery; the call-site pin is the guard against that).

Caveat that bounds the value of ANY fix to R1: the request has to be `approved` first. The `approve` / `reject` rows of
`withdrawal_approvals` are NOT proof-bound (ADR 0113 s9.3 stays open; only `reverse` decisions are, ADR 0110 T8 amendment), so
a SQL-only actor can also forge the approval chain (the fixtures do exactly that with an automated approve record). The signed
decision closes the dispatch link; the approval link is a separate, already-recorded residual (T8 list). The honest summary for
the owner: with 0171 a stolen runtime credential can no longer forge the dispatch attestation; it can still forge approvals
until ADR 0113 s9.3 is built; and it still cannot call the provider.

### 2.4 Is a scheduled re-check only detective?

Yes. A job that re-evaluates every allow row against the request figures at `decided_at` can only ALERT after the transition
committed (the transition is not undone: money may already have left in the real world). It is also unable to re-run the risk
engine as of `decided_at` (cumulative rules are time dependent) without a historical snapshot the platform does not keep. It
remains a reasonable defence-in-depth option for launch; it is not needed to close R1 and is not built.

## 3. Decision

The security review (section 14 of the F4 record, SF4-1) argued that a signature minted inside the runtime's credential
domain does not raise the bar. That conflates two things: the owner-decided threat model (THREAT-MODEL-ARBITRARY-SQL-1 = YES,
ADR 0110) is a SQL-only actor holding the stolen `igaming_runtime` DATABASE credential. The ADR 0110 signing key is NOT in
that credential: it is application configuration (`ACTOR_PROOF_KEYS`) and, in the database, lives in `actorproof_private`
(owner-only since 0166, unreadable to runtime and backup roles). A SQL-only attacker cannot sign. An attacker inside the
application process holds the key and is out of scope by the same ADR (T6). So the preferred direction is sound and it reuses
the existing mechanism with no new service boundary and no new tenant-isolation mechanism: IMPLEMENTED as below.

### 3.1 Design (migration 0171)

- New proof SCOPE `system`: provable ONLY for the single operation `withdrawal_dispatch_gate:allow`, only with a tenant, only
  for the fixed system actor `00000000-0000-0000-0000-00000f4d1500`; that operation is provable for no other scope. The model
  had no system scope (it knows `tenant`, `platform_acting`, `platform`), so this is a closed addition to
  `actor_proof_require` (0166's body verbatim plus one marked block and the scope list; `CREATE OR REPLACE` keeps owner,
  SECURITY DEFINER, pinned `search_path`, EXECUTE grants). Error codes are the existing AP001..AP005 (AP004 for the closed
  operation/scope/actor binding).
- A system scope (not the staff submitter) is deliberate: the allow attests that the PLATFORM evaluated the gate, it is not a
  human authorisation. T1p still records the staff submitter in the audit row; T2/T12 have no staff actor at all.
- `withdrawal_gate_decisions_zz_actor_proof_guard` (BEFORE INSERT, the LAST BEFORE ROW trigger of the table, catalog pinned):
  for an `allowed` row it calls `actor_proof_require(system actor, 'system', tenant, 'withdrawal_dispatch_gate:allow',
  <request id>, digest)` with `digest = SHA-256(k2_canonical(tenant, request, attempt | ~, stage, risk_outcome | ~,
  restriction_in_force | ~, account, wallet, asset, amount))`, recomputed from the INSERTED row and the immutable request
  columns. The proof therefore binds tenant, request, attempt (re-dispatch), stage, the decision outcome AND the figures the
  gate evaluated. Freshness (lifetime <= 60 s, 5 s skew), replay (nonce UNIQUE, consumed in the SAME transaction, rolled back
  with it) and key rules are the verifier's.
- The consumed nonce is stored in `withdrawal_gate_decisions.actor_proof_nonce`, written only by that trigger (a caller value
  is overwritten with NULL first), UNIQUE when present. `withdrawal_dispatch_decision_ok` (WD053) now counts only allowed rows
  that carry a nonce. A CHECK `NOT allowed OR actor_proof_nonce IS NOT NULL` is added `NOT VALID` (0169-era history is
  append-only and not rewritten; every new row is checked).
- Denying rows are not proof-bound: a denial only restricts, a forged denial gains nothing (a denying row of the transaction
  already vetoes WD053). So the denial path, the park / escalate behaviour and every denial test are unchanged.
- Go: `internal/withdrawal/dispatchproof.go` (the single signer in that package) is called from
  `RecordDispatchGateDecision` for `Allowed` decisions: `actorproof.Default().Attach` with the system claims and the digest.
  No issuer / unsignable claims -> `ErrDispatchProofUnavailable` (wraps `ErrGateUnavailable`): the claim fails closed and is
  retryable, nothing recorded, nothing sent (T1p: `ErrPayoutWithdrawalGateUnavailable`, 503).
- ADR 0110 conventions: proof-bound table list gains `withdrawal_gate_decisions` (INSERT with `allowed`); signer-owner static
  pin `TestStatic_OnlyFourEyesPackagesSign` extended FILE-granularly (`internal/withdrawal/dispatchproof.go` only, not the
  package), plus `TestStatic_WithdrawalSignsOnlyInDispatchProof` (exactly one `Attach`, no `Sign`, system claims only, no other
  file of the package imports the issuer) and non-vacuity plants. `RecordDispatchGateDecision` itself stays pinned to its four
  production call sites (SF4-2, `dispatchgate_callers_static_test.go`).

### 3.2 Proposed ADR 0110 amendment text (the orchestrator applies it)

> Amendment (F4 residual round 1, migration 0171): the WD-RG-1 payout dispatch gate ALLOW (`withdrawal_gate_decisions`, INSERT
> with `allowed = true`) is proof-bound (seventeen governed tables). Operation `withdrawal_dispatch_gate:allow`, new scope
> `system` (provable only for this operation, with a tenant, for the fixed system actor `...f4d1500`), target = the withdrawal
> request id, payload = SHA-256(k2_canonical(tenant, request, attempt, stage, risk outcome, restriction in force, account,
> wallet, asset, amount)) recomputed by the trigger. The consumed nonce is stored in `actor_proof_nonce`; WD053 counts only such
> rows. The issuer is `internal/withdrawal/dispatchproof.go` (file-granular signer pin). Denials are not proof-bound. T5 note:
> binding the dispatch allow does not bind the approval rows (ADR 0113 s9.3, still open). T6 unchanged.

### 3.3 Residuals after 0171 (exact)

1. T6: whoever holds the application signing key (code execution in the platform-api process, or the secret store) can sign an
   allow. Out of scope by ADR 0110 and by the owner decision.
2. The proof attests that the application EVALUATED the gate and recorded its outcome; it does not prove the evaluation was
   correct. A weak input (SF4-6: account / wallet status changes, risk-rule creation and removal) is a governance question
   unchanged by this item.
3. A proof captured on the wire inside its 30 s life can be used once, for exactly the row it names (ADR 0110 s9.5).
4. Approval records are still unproven (ADR 0113 s9.3).
5. The key must be configured wherever dispatch runs: platform-api already refuses to start in production without
   `ACTOR_PROOF_KEYS` and an ACTIVE kid in the database (`actorproof.VerifyConfiguredInProduction`), so there is no new
   deployment configuration. Key rotation (runbook section 15) keeps working: a proof is minted per write.

### 3.4 Monitoring / detection

AP001..AP005 on `withdrawal_gate_decisions` in the database log are an attack signal or a rotation gap (runbook updated). The
new alert covers the availability side (signer missing). The hourly drift job is unaffected.

## 4. Changed and new tests

Changed assertions (intentional behaviour change, none weakened):

- `internal/withdrawal/f4_dispatch_gate_integration_test.go`:
  - `TestF4_DB_DecisionRows_AppendOnly_Shape_AndTriggerOwnedColumns`: the trigger-owned-columns probe inserts a DENYING row
    (an ALLOWED raw row is now AP001 without a proof); the shape/CHECK probes (CHECKs run after BEFORE triggers) insert their
    allowed rows with a valid signed proof so the probed refusal is still the CHECK's (23514) / the trigger's (WD054).
  - `TestF4_DB_Catalog`: adds the pin that the proof guard is the last BEFORE INSERT row trigger of
    `withdrawal_gate_decisions` and that its function is not SECURITY DEFINER and pins `search_path`.
- `internal/withdrawal/adr0113_state_machine_integration_test.go` `TestADR0113_HeaderFirstAttacks`: the "forged decision" arm
  records the decision through the real signer (a raw allow is AP001 now); the asserted result is unchanged (WD031 at COMMIT).
- `internal/actorproof/actorproof_test.go`: `TestStatic_ScanIsNotVacuous` plants 8 offenders instead of 6 (a second file of
  `internal/withdrawal` importing the issuer, a second reader of the new hook); new pins described in 3.1.
- `internal/testsupport/wdpath` imports `prooftest` (blank, installs the key-provisioning hook; already a pinned fixture
  package).
- Fixtures: every fixture ALLOW already went through `RecordDispatchGateDecision`, so none needed a per-site change: the
  integration-only hook `actorproof.TestEnsureKeyHook` (set by `prooftest/dispatchhook.go`, read only in `dispatchproof.go`,
  pinned) provisions the process key into the database of the transaction once per database name.

New tests (`internal/withdrawal/f4r1_signed_decision_integration_test.go`, runtime role):

- forged allow, no proof -> AP001 (including with E7 in the same transaction; request stays `approved`; no row); caller-chosen
  nonce column -> AP001; wrong key -> AP002; digest of another outcome -> AP004; unsigned denial admitted with a NULL nonce.
- valid proof accepted, nonce recorded equals the proof's nonce, E7 passes.
- binding matrix, all refused with the right code and no row recorded: another request (target), another tenant, another
  request's digest, redispatch-stage digest, attempt-bound digest, different amount (AP004); another operation, staff scope,
  another actor (AP004); expired and over-long lifetime (AP003); system scope used for another operation (AP004).
- replay: a rolled-back use does not burn the nonce (retry succeeds); a committed one is AP005; 4 concurrent uses of one proof:
  exactly one wins, three AP005.
- real writer signs and passes E7; no issuer -> `ErrDispatchProofUnavailable` (wraps `ErrGateUnavailable`).
- `TestMigration0171_UpDownUp_SnapshotAndDownRefusal`: up, down (byte-identical snapshot), up again equal, WD095 down refusal
  with a proof-bound row, refused down changes nothing.
- `internal/payments/f4_payout_gate_integration_test.go`: T1p unavailable raises one open `withdrawal.payout_gate_unavailable`
  alert per request (two refusals = two occurrences), no issuer fails closed with nothing claimed and goes through on retry;
  T2 raises the alert only at the 3rd consecutive fail-closed tick and sends nothing.
- `internal/actorproof`: static pins (section 3.1), existing proof suites unchanged.
- normal flows (T1p, T2, T12, E7 fixtures) are the existing F4 suites run unchanged (section 6).

## 5. F4-R2-NEGBAL-SQL: built, proven, withdrawn

I implemented the clause (`withdrawal_dispatch_rg_violation` returns `wallet_balance_negative` when the
`wallet_balance_projection` of `player_cash` / `player_locked_cash` has `credit_total < debit_total`, the exact read of
`rg.walletBalanceNegative`, owner decision 13) and a test proving WD052 at E7 after a chargeback. It is feasible without any
provider. Running the payments subset then showed, as the F4 record predicted (R2), that it refuses every `wdpath.Plant`
fixture (unfunded holds drive `player_cash` negative by design; the reconciliation, launch-closure and payments fixtures assert
balances equal to minus the hold): `TestDepRefConflict_ReferenceBoundToAPayoutAttempt_Parks`,
`TestKillSwitchPredicate_T1p_Payout_InsertSubmittingAttempt`, `TestMigration0101_UpdateGuard_TransitionWhitelist`,
`TestB3_PayoutMismatchPark_StaysReferenceLess` failed in that subset alone, and 14 reconciliation fixtures depend on the same
shape. A weaker variant that tolerates the fixtures (netting the open holds) would miss precisely the chargeback case the
rule exists for, so I did not ship it. Making the real rule bind means funding `Plant`, which changes the ledger content those
suites assert and belongs to the payments-recon workstream editing the same files.

Decision: WITHDRAWN from 0171 (the migration carries a comment saying so), recorded as ACCEPTED and subsumed for the attackers
the freeze considers:
- against a forgotten code path: WD053 refuses a transition without a recorded decision and the application gate (which
  applies the negative-balance refusal at T1p / T2 / T12) is what produces the decision;
- against the forged-allow attacker (R1): the allow can no longer be forged, so the application check cannot be bypassed by a
  fake row;
- remaining window: a chargeback / negative event committed between the gate's read and the transition inside one claim
  transaction (as in R2).
The follow-up `F4-R2-NEGBAL-SQL` stays tracked (owner `payments-recon`, reviewed by `ledger-finance`): add the clause (below) to
`withdrawal_dispatch_rg_violation` once `Plant`-based fixtures are funded.

```sql
-- after the wallet-status checks in withdrawal_dispatch_rg_violation:
IF EXISTS (SELECT 1 FROM public.wallet_balance_projection p
            WHERE p.tenant_id = p_tenant AND p.wallet_id = p_wallet
              AND p.account_type IN ('player_cash', 'player_locked_cash') AND p.credit_total < p.debit_total) THEN
    RETURN 'wallet_balance_negative';
END IF;
```

## 6. SF4-5 and SF4-3

- SF4-5 `IMPLEMENTED`: Kind `withdrawal.payout_gate_unavailable` (migration 0171 seed, `internal/alerting/kind.go`): p2
  (engineering default), `detached`, scope platform, subject required, attributes `stage` and `cause`
  (`evaluation_failed` | `proof_unavailable`; closed codes). Raised (a) at T1p on every fail-closed refusal that is an
  evaluation or signing failure (not on the benign "request changed during the claim, retry" case), discriminator
  `withdrawal:<id>:gate_unavailable`; (b) at T2 / T12 after 3 CONSECUTIVE fail-closed ticks of the same attempt
  (`gateUnavailableAlertAfter`, process-local counter, reset by any tick that decides), discriminator
  `payout_attempt:<id>:gate_unavailable`. The alerting store keeps one open alert per (subject, kind, discriminator) and only
  adds occurrences, which is the throttle. The counter is process local by design (a restart restarts the count; it is only a
  noise filter). Delivery to a person is ALERT-DELIVERY-1 (open).
- SF4-3: no code change; the runbook already states the escalation is not a human-release latch.

## 7. Files

Migration `migrations/0171_withdrawal_dispatch_gate_signed_decision.{up,down}.sql`; `internal/actorproof/actorproof.go`
(scope, operation, system actor, hook declaration), `internal/actorproof/prooftest/dispatchhook.go`,
`internal/withdrawal/dispatchproof.go`, `internal/withdrawal/dispatchgate.go`, `internal/alerting/kind.go`,
`internal/payments/payout.go`, `payout_sweep.go`, `payout_withdrawal_gate.go`, `internal/testsupport/wdpath/wdpath.go`, tests
named above, runbooks `docs/runbooks/withdrawal-dispatch-gate.md` and `docs/runbooks/observability-and-alerting.md`.
`deploy/init-app-role.sql`, `deploy/aws/sql/init-runtime-role.rds.sql`, `deploy/backup/*`: NO change needed (no new table,
grant, schema or SECURITY DEFINER function; the verifier is replaced in place with unchanged owner and ACL; the backup baseline
is derived and keys only on migration checksums/head). No production-checklist config change (no new setting: the signing key
is the existing `ACTOR_PROOF_KEYS`).

## 8. Merge notes for the orchestrator

- Migration head is 0171; `reservedUnusedVersions` unchanged (143..150, 159, 161). Any other agent's test that records a
  dispatch decision with a RAW INSERT of `allowed = true` now gets AP001: use `withdrawal.RecordDispatchGateDecision` (it signs)
  and import `prooftest` (directly or through `wdpath`) in integration tests.
- Apply the ADR 0110 amendment text of section 3.2, the registry rows of section 1 and the SF4-1 / R1 launch-risk-list edit
  (R1 narrowed to T6 plus input governance; add: approvals still unproven, ADR 0113 s9.3).

## 9. Review probes

Security: (1) the new verifier block: can `scope = 'system'` be reached by any other operation or actor? (2) can a caller
choose `actor_proof_nonce`? (trigger overwrites; denial rows forced NULL); (3) is `withdrawal_dispatch_decision_ok` the only
reader of allows? (4) TestEnsureKeyHook cannot sign or alter claims and is nil in production (pinned); (5) the system actor id
is public: it is not a credential, the MAC is.
Ledger-finance: nothing financial changed; no lock added (the trigger reads the request row, takes none); T1p / T2 / T12 order
unchanged; rollback semantics (the nonce rolls back with the claim).

## 10. Commands and counts

Private scratch database only (`priv_db.sh` / `priv_test.sh`, rebuilt at 0171), `-tags integration -count=1 -p 1`, synthetic data,
MOCK providers. `gofmt -l` clean; `go vet` clean on the touched packages (plain and `-tags integration`).

| Command | Result |
|---|---|
| `go test -count=1 ./...` (unit) | no failures (all packages ok) |
| `./internal/withdrawal/ ./internal/actorproof/...` | ok (withdrawal incl. the 11 new `TestF4R1_*` / `TestMigration0171_*` and the adapted F4 / ADR0113 tests) |
| `./internal/alerting/... ./internal/db/` | ok (Kind parity with the database included) |
| `-run 'F4\|Claim\|Payout\|Sweeper\|Dispatch\|Migration0169\|Migration0101\|KillSwitch\|DepRef\|B3_\|HSEC\|B13B\|B11\|A5\|T8\|RVLF' ./internal/payments/` | ok (1758 s) |
| `./internal/reconciliation/ ./internal/launchgov/ ./internal/risk/ ./internal/tenant/ ./internal/payoutinstrument/... ./internal/rg/ ./internal/withdrawalgate/... ./cmd/... ./deploy/backup/...` | all ok |
| `-run 'F4\|Withdraw\|Submit\|Payout\|A5\|IWire\|Golden\|OpenAPI' ./internal/httpserver/` | ok |
| `-race -run 'F4R1\|TestF4_DB' ./internal/withdrawal/` ; `-race -run F4 ./internal/payments/ ./internal/httpserver/` | ok, no race |

NOT run by me (the orchestrator runs it): the full payments suite. Payments therefore stays `PARTIALLY VERIFIED` beyond the
subset above. A first payments subset run with the (later withdrawn) SQL negative-balance clause failed 4 fixtures, which is
the evidence for section 5; the final run has none.

## 11. Labels

- Signed dispatch-gate ALLOW (R1): `IMPLEMENTED`, verified as in section 10. Residual: T6, approvals unproven (ADR 0113 s9.3).
- F4-R2-NEGBAL-SQL: `NOT IMPLEMENTED` in SQL (accepted, tracked); the application gate still enforces the rule.
- SF4-5 alert: `IMPLEMENTED` (raise); delivery `NOT IMPLEMENTED` (ALERT-DELIVERY-1).
- SF4-3: documented behaviour, unchanged.
- Not claimed: regulatory approval of any withdrawal policy; `security` and `ledger-finance` sign-off (required, CLAUDE.md).
