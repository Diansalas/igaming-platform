# TOMB record: signed deposit reversal tombstone (closure security review F-1), plus F-3 and F-4

Status: implementation record on branch `tomb-deposit-tombstone-proof` (from `claude/focused-wright-jw88w9` @ `6f8a522`,
migration head 0172 -> 0175). This file proposes registry text only. It does not edit the task registry, HANDOVER, progress,
active-stage, the decision register or any ADR. All providers are MOCK and all data is synthetic. Software capability only:
no regulatory approval is claimed. `ledger-finance`, `code-reviewer` and `qa` review is required before merge (money-path
trigger on `ledger_transactions`). I wrote this as the `security` specialist, so it is not an independent security sign-off
of my own change. An independent re-review is recommended (probe list in section 9).

Source finding: `.claude/worktrees/secrev/docs/plans/closure-security-review-record.md`.
- F-1 MEDIUM: forged deposit tombstone.
- F-3 LOW: the dispatch-gate signer pin covered one package.
- F-4 LOW: the T2/T12 alert counter was wiped at 4096 entries.

## 1. Proposed registry text

| Item | Label | Note |
|---|---|---|
| F-1 / R-6: the runtime credential forges a DEPOSIT tombstone (clears MA020; rejects a genuine late deposit) | `IMPLEMENTED` (closed against the SQL-only attacker for every reference the tenant holds a payment row for; residuals in section 5) | Migration 0175. A deposit reversal tombstone is a signed, nonce-consumed SYSTEM actor proof (`deposit_reversal_tombstone:write`). It is bound to tenant, provider, reference, ledger key, correlation and the stored reversal receipt, and verified in SQL by the existing SECURITY DEFINER verifier. `payment_ref_cleared` (MA020) counts only proof-bound tombstones. |
| SRF2R-3 / 6.10 wording (security review condition 1) | superseded | "Statement-line vector CLOSED (0172); ledger-tombstone vector CLOSED (0175) for payment references; residuals in tomb-record section 5." |
| F-3: dispatch-gate claim and decision INSERT not pinned outside `internal/withdrawal` | `IMPLEMENTED` | Code-line pins cover the whole repository (operation, actor, scope, operation literal, actor uuid, `INSERT INTO withdrawal_gate_decisions`). `SystemActorDispatchGate` is now a function over an unexported value. |
| F-4: T2/T12 gate-unavailable alert suppressed above 4096 failing attempts | `IMPLEMENTED` | Bounded counter. At the bound it evicts only stale entries, and if it is still full it reports the untracked failing attempt at the threshold (alert). A unit test covers 5096 stuck attempts. |
| Migration 0175 | `IMPLEMENTED` | up / down / up byte-identical snapshot. DT095 refuses the down while proof-bound tombstones exist. |

## 2. Threat model before the fix (verified, not assumed)

The attacker is a SQL-only actor holding the stolen `igaming_runtime` credential. That role has INSERT on
`ledger_transactions` under RLS `tenant_isolation`, and it sets `app.tenant_id` itself.

The attack:

```sql
INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
VALUES (<t>, 'tombstone', <any key>, <psp>, <deposit ref>, <random uuid>)
```

The only guard was WD027 (0162/0165, AFTER INSERT), and it fires only when `correlation_id` is a withdrawal request. The insert
was therefore admitted, with two consequences:
- `payment_ref_cleared` returned true, which removed the MA020 barrier.
- The `(tenant, provider_id, provider_tx_id)` unique index and `tombstoneExists` reject the genuine late success.

Classifying the row by its shape cannot fix this:
- Casino rollback tombstones use the SAME key shape (`tombstone:<provider>:<ref>`).
- The deposit tombstone's correlation id is random.
- The payout guard does not bind the reference. A genuinely reversed withdrawal request could therefore serve as a correlation
  "licence" for any reference.

## 3. Design (migration 0175, ADR 0110 reused as F4 / 0171 did)

### 3.1 Verifier (`actor_proof_require`)

- `CREATE OR REPLACE` with 0171's body and ONE changed marked block. The owner, SECURITY DEFINER, the pinned
  `search_path` and the EXECUTE grants are kept.
- The `system` scope now admits exactly two (operation, actor) pairs:
  - `withdrawal_dispatch_gate:allow` with actor `...00000f4d1500` (unchanged);
  - `deposit_reversal_tombstone:write` with actor `00000000-0000-0000-0000-0000000de175` (new, distinct).
- Each operation is provable for no other scope and no other actor, and a tenant is still required.
- No other branch changed. The down restores the 0171 body (md5 in the snapshot).

### 3.2 Nonce column

`ledger_transactions.actor_proof_nonce TEXT NULL`:
- shape CHECK, and allowed only on `tombstone` rows;
- UNIQUE partial index;
- written ONLY by the guard, which overwrites any value the caller supplies with NULL before deciding.

### 3.3 Guard `ledger_transactions_zz_deposit_tombstone_proof_guard`

It is BEFORE INSERT FOR EACH ROW and the LAST BEFORE ROW trigger (catalog-pinned). It is not SECURITY DEFINER and pins
`search_path`. It takes no lock. It applies to every tombstone that names a provider, unless the row matches one of two
exemptions:

- **Payout exemption** (WD027 then governs the row, unchanged). All three must hold:
  - the correlation is a withdrawal request of the tenant;
  - (provider, reference) is that request's own payout reference, on the request or on its payout attempt;
  - no DEPOSIT attempt of the tenant holds that reference.
- **Non-payment exemption** (a casino rollback). Both must hold:
  - the session is the plain tenant context of the row: `app.tenant_id` = row tenant, with no player GUC and no
    platform-admin GUC. That is exactly where the `tenant_staff_scope` RLS of the payment tables shows every tenant row;
  - the provider has no payment attempt and no payment receipt in the tenant. Both tables are append-only, with no DELETE
    or TRUNCATE for any role.
  - Any other context fails closed, so an attacker cannot hide the payment rows behind GUCs.

For every other row, the guard runs:

```
actor_proof_require(...de175, 'system', tenant, 'deposit_reversal_tombstone:write', <target>,
  SHA-256(k2_canonical(tenant, provider_id, provider_tx_id, idempotency_key, correlation_id, <target>)))
```

- `<target>` is the proof's own MAC-covered target, which is the reversal receipt id.
- After verification, the target must name an `applied` `deposit_reversal` receipt (`payment_provider_events`) of the same
  tenant, provider and `original_provider_reference` (DT001).
- The guard then records the consumed nonce.

### 3.4 MA020

`payment_ref_cleared` = the 0172 body plus `AND t.actor_proof_nonce IS NOT NULL`. The MA020 closure therefore does not depend
on the classification: a tombstone that took either exemption can never clear MA020.

### 3.5 Go

- `internal/payments/deposit_tombstone_proof.go` is the ONLY signer of the new operation. It does one `Attach`, with the
  system claims and the receipt as target.
- It is called only by `postDepositReversalTombstone`, which now takes the receipt id. Its single production caller is the
  tombstone branch of `applyReversalReceiptEvidence`, right after the receipt (R0) is stored in the same transaction.
- The idempotency key and correlation are computed once and then both signed and posted. Posting semantics are unchanged:
  same row, same key, same random correlation, same audit `deposit.reversal_tombstoned`, same receipt resolution.

### 3.6 Fail closed (no issuer or key)

- The signer returns `ErrDepositTombstoneProofUnavailable`, and the whole callback transaction rolls back, receipt
  included.
- The public webhook (`deposit_handlers.go`) maps this error to 503 + `Retry-After: 1`. The log line is fixed:
  `payment_webhook_deposit_tombstone_proof_unavailable` (no err text).
- The PSP redelivers. Nothing is skipped silently, and the redelivery succeeds once the key is back (tested).
- The simulate route keeps its generic 500 (MOCK only).
- Production startup already refuses to run without `ACTOR_PROOF_KEYS` and an active kid
  (`VerifyConfiguredInProduction`), so no new configuration is needed.

### 3.7 Concurrency and idempotency

- `ledger.Post` inserts inside a savepoint (`IdempotentInsert`). When a second reversal event for the same original hits the
  existing key, its nonce consumption is rolled back with the savepoint, and `AlreadyPosted` behaves exactly as before.
- Duplicate deliveries stop at the receipt dedup, before any signing.
- Four concurrent identical deliveries produce exactly one applied delivery, one tombstone and one nonce (tested).

### 3.8 Signer pin (ADR 0110 convention, file-granular, non-vacuous)

`internal/payments` was already a package-level signer owner, so the new pins are code-line pins over the whole repository:
- `OpDepositReversalTombstone`, `SystemActorDepositReversal`, the literal `deposit_reversal_tombstone:` and the uuid
  `0000000de175` may appear only in `actorproof.go` and `deposit_tombstone_proof.go`;
- `attachDepositTombstoneProof(` only in that file and `orchestrator.go`;
- `postDepositReversalTombstone(` only in `orchestrator.go`, `receipt.go` and the integration-only fixture;
- `ScopeSystem` only in `actorproof.go`, `dispatchproof.go` and `deposit_tombstone_proof.go`;
- the key-provisioning hook may be read in `deposit_tombstone_proof.go` (count 1).

`TestStatic_DepositTombstoneSignedOnlyHere` checks:
- exactly one `Attach` and no `Sign`;
- the claims include `Target: receiptID.String()`;
- the fixture file has the integration build tag on line 1 and contains no issuer code;
- the Go mirror of the verifier matrix.

`TestStatic_ScanIsNotVacuous` now plants 13 offenders (was 8) and three legitimate shapes.

### 3.9 Proposed ADR 0110 amendment text (the orchestrator applies it)

> Amendment (closure security review F-1, migration 0175): a DEPOSIT reversal tombstone (`ledger_transactions`, INSERT of a
> `tombstone` naming a payment provider that is not the A5 payout tombstone) is proof-bound. The operation is
> `deposit_reversal_tombstone:write` under scope `system`, provable only for this operation's own fixed system actor
> `...0000000de175` with a tenant. The target is the stored `deposit_reversal` receipt id. The payload is
> SHA-256(k2_canonical(tenant, provider, original reference, idempotency key, correlation, receipt)), recomputed by the trigger,
> and the receipt must be an applied deposit_reversal of the same tenant, provider and reference (DT001). The consumed nonce
> is stored in `ledger_transactions.actor_proof_nonce`, and `payment_ref_cleared` (MA020) counts only such rows. The issuer
> is `internal/payments/deposit_tombstone_proof.go` (file-granular pin). The system scope now covers two closed (operation,
> actor) pairs. T6 is unchanged.

## 4. F-3 and F-4

**F-3.**
- `actorproof.SystemActorDispatchGate` is now `func SystemActorDispatchGate() uuid.UUID` over an unexported value, so no package
  can reassign it.
- New code-line pins over every non-test Go file:
  - `OpDispatchGateAllow` (actorproof.go 3, dispatchproof.go 1);
  - `SystemActorDispatchGate` (1 / 1);
  - the literal `withdrawal_dispatch_gate:` (actorproof.go only);
  - the uuid `00000f4d1500` (actorproof.go only);
  - `INSERT INTO withdrawal_gate_decisions` (`internal/withdrawal/dispatchgate.go` only).
- Non-vacuity plants:
  - a second claim builder in `internal/httpserver`;
  - a second INSERT in `internal/withdrawal`;
  - a raw operation literal in `internal/risk`.
- Residual: static pins bind code shape, not intent. A reviewed code change can always move the pins.

**F-4.**
- `gateFailCounts` is now a bounded `gateFailCounter`:
  - a deciding tick still resets an attempt's count;
  - at the bound (4096) it first evicts entries not touched for 1 h;
  - if it is still full, the untracked failing attempt is reported AT the threshold, so the alert is raised.
- So in a mass outage every stuck attempt alerts within at most 3 sweeps, memory stays bounded, and no tracked count is wiped.
- Unit tests:
  - `TestGateFailCounter_MassOutageAboveBoundStillAlerts` (5096 attempts, round-robin, every one reaches the threshold);
  - `..._ThresholdAndReset`;
  - `..._StaleEntriesAreEvicted`.
- They ran with `-race`.
- The existing `TestF4R1_Sweeper_T2_GateUnavailable_AlertsAfterRepeatedFailures` is unchanged.

## 5. What the SQL-only attacker can and cannot do after the fix

CANNOT (verified by tests as `igaming_runtime`, section 7):
- Write a tombstone on a reference of any provider that has a payment attempt or receipt in the tenant. This holds in any key
  shape, with a self-chosen nonce, and under player or platform-admin GUCs. The result is AP001 without a proof, and AP002,
  AP003, AP004, AP005 or DT001 with a forged, expired, replayed, wrongly bound or wrongly targeted one.
- This covers every MA020 target and every deposit attempt's reference. So the attacker can neither remove the MA020 barrier
  nor make the genuine late success of such a deposit be rejected by a tombstone.
- Make `payment_ref_cleared` true by any ledger write. Only a row carrying a verifier-consumed nonce counts.
- Use the payout exemption against a deposit reference, because a deposit attempt holding the reference disables it.
- Use the new system scope for another operation, or the dispatch actor for this operation (AP004), in either direction.
- Replay a proof (AP005), or reuse it for another row, tenant, provider, reference, key, correlation or receipt (AP004 /
  DT001).
- Run the down migration (owner only). The down also refuses with DT095 while proof-bound tombstones exist.

CAN still (accepted or out of scope, each with detection):
1. **Pre-empt a provider the tenant has never used for payments.** In the plain tenant context, a tombstone on a provider with
   no payment attempt or receipt in the tenant is classified as a casino rollback and admitted unsigned. If the attacker
   predicts a future PSP-assigned reference of a provider before that tenant's first payment row on it, the later genuine
   deposit success on that exact reference would be rejected.
   - It can never clear MA020 (no nonce).
   - Detection: the reconciliation tombstone checks (`casino_consistency` C5 / late-original, R9), and a tombstone with
     `actor_proof_nonce IS NULL` on a provider that later appears in `payment_attempts` is a simple detective query (recommended,
     not built).
2. **Occupy (provider, reference) with a NON-tombstone ledger row** (another `transaction_type`). This also blocks the genuine
   posting through the unique index. It is the general "runtime role writes ledger rows" residual, not a tombstone property.
   It is not analysed or closed here, and it is flagged for `ledger-finance`.
3. **Forge casino rollback tombstones** (casino harm: a late bet is rejected). Unchanged and out of F-1 scope.
4. **Availability.** A forged `payment_provider_events` row naming a casino provider id makes that tenant's genuine casino
   rollback tombstones on that provider require a deposit proof, so they fail closed (AP001, loud). The same happens through
   configuration if a casino provider id ever equals a payment provider id in one tenant. This is a fail-closed DoS, not an
   integrity break.
5. **T6.** Whoever holds the application signing key (code execution in platform-api, or the secret store) can sign.
6. **Proof capture.** A proof captured within its 30 s life can be used once, for exactly the row it names, which the app
   writes anyway. pgaudit `log_parameter` must stay off for the runtime role (ADR 0110 s9.5).
7. **History.** Tombstones written before 0175 carry no nonce and no longer clear MA020 in SQL (fail-closed direction). There is
   no production history (pre-launch).

## 6. Behaviour changes and changed tests (none weakened)

Intentional behaviour changes:
- (a) A raw, unsigned deposit-shaped tombstone on a payment provider is refused (AP001).
- (b) `payment_ref_cleared` counts only proof-bound tombstones.
- (c) The webhook answers 503 + Retry-After when the tombstone cannot be signed (before this change the tombstone was simply
  written).

Fixture changes. Every fixture that posted a raw tombstone on a payment provider now calls
`payments.PlantVerifiedDepositReversalTombstone` (integration build only):
- Each call stores a synthetic applied and resolved `deposit_reversal` receipt and posts through the production signer.
- The ledger key becomes `tombstone:<provider>:<ref>` where a fixture used another key. No assertion read the key.
- Every assertion is unchanged. Sites:
  - `internal/adjustment/b23_b25_integration_test.go` (`tombstoneFor`, used by B23 and every `TestMA020_K2_*`);
  - `internal/adjustment/b11_b17_integration_test.go` (`postDepositFamily`);
  - `internal/reconciliation/ma020_parity_integration_test.go` (`ma020TombstoneFor`, every `TestMA020_Parity_*`);
  - `internal/reconciliation/prh2_r9_bound_clear_integration_test.go` (`bcTombstone`);
  - `internal/httpserver/a5_payout_reversal_http_integration_test.go` (the "held" reference);
  - `internal/payments/a5_payout_reversal_integration_test.go` (`ReturnReferenceAlreadyHeld`, the ledger-only source);
  - `internal/payments/{f3_payout_tombstone,b11_payout_unbound_hold,ma020_b3_e2e,m4_resolve1_world}_integration_test.go`
  (raw tombstone fixtures on payment providers; for F3 / B11 the forged "foreign" tombstone is now the signed one, which
  also shows SR5R-1's precondition is no longer reachable by SQL forgery for payment references);
- `internal/payments/{iwire_tombstone,escalation_b6_b7,poll_amount}_integration_test.go` (they called
    `postDepositReversalTombstone` directly; the signature now takes the receipt).
- Raw tombstone fixtures on providers with no payment row yet (several payout B12/R8/RefBind/F3 fixtures seed the tombstone
  BEFORE the first attempt) still pass unsigned. That is residual 1 in action, and it is disclosed.

Static test changes:
- `internal/actorproof/actorproof_test.go`:
  - `TestStatic_WithdrawalSignsOnlyInDispatchProof` looks for `SystemActorDispatchGate()` (call form);
  - `TestStatic_ScanIsNotVacuous` has 13 plants (was 8);
  - new pins and the new test, as in sections 3.8 and 4.
- `internal/withdrawal/{dispatchproof.go, f4r1_signed_decision_integration_test.go}`: `SystemActorDispatchGate` becomes
  `SystemActorDispatchGate()`. No assertion changed.

New tests:
- `internal/payments/f1_deposit_tombstone_proof_integration_test.go`:
  - `TestF1Tomb_ForgedDepositTombstone_RuntimeRefused`;
  - `TestF1Tomb_BindingMatrix` (15 refusal cases, rolled-back use not burnt, valid proof, nonce recorded, AP005 replay, the
    other tenant not cleared, deposit actor cannot prove a dispatch allow);
  - `TestF1Tomb_ConcurrentUseOfOneProof`;
  - `TestF1Tomb_RealReversalCallback_PostsSignedTombstone_Idempotent` (normal path, duplicate delivery, a second reversal
    event, unchanged tombstone semantics, 4 concurrent deliveries);
  - `TestF1Tomb_NoIssuer_FailsClosed_ThenRetrySucceeds`;
  - `TestF1Tomb_DB_Catalog`.
- `internal/payments/f1_migration_0175_integration_test.go`: `TestMigration0175_UpDownUp_SnapshotAndDownRefusal`.
- `internal/payments/payout_gate_failcounter_test.go` (F-4, unit).
- `internal/actorproof`: `TestStatic_DepositTombstoneSignedOnlyHere`.

## 7. Commands and counts

Private scratch database `tomb154956p1990x01` only (`priv_db.sh` at head 0172, then `cmd/migrate up` 0173/0174 placeholders
+ 0175; `priv_test.sh`), `-tags integration -count=1 -p 1`, MOCK providers, synthetic data. Dropped at the end.
`gofmt -l` clean; `go vet` clean on actorproof, payments, withdrawal, httpserver, adjustment, reconciliation (plain and
`-tags integration`).

| Command | Result (PASS lines incl. subtests) |
|---|---|
| `go test -count=1` (unit) payments, httpserver, actorproof/..., withdrawal, db, ledger, adjustment, reconciliation | all ok |
| `-run 'Receipt\|Callback\|MA020\|Reversal\|Tombstone\|Tomb\|B3_\|IWire\|F1\|Migration0175\|A5\|F3\|RVLF\|DepRef\|B7\|B6\|PollAmount\|Poll\|A7\|InvDep\|Webhook\|D1\|F4\|T8\|M4\|B11\|B12\|RefBind\|R8\|GateFail' -timeout 110m ./internal/payments/` | ok, 1279 s, 484 PASS |
| `./internal/withdrawal/` (full) | ok, 213 PASS |
| `./internal/ledger/` (full) | ok, 113 PASS |
| `./internal/actorproof/...` (full) | ok, 23 PASS |
| `./internal/db/` (full) | ok, 312 PASS |
| `./internal/adjustment/` (full) | ok, 215 PASS |
| `./internal/reconciliation/` (full) | ok, 563 PASS |
| `-run 'Rollback\|Tomb\|Failure\|Subject\|TenantNotActive\|Rejection\|Stake' ./internal/casino/` | ok, 81 PASS |
| `-run 'Tomb\|Rollback\|Settlement' ./internal/sportsbook/` | ok, 115 PASS |
| `-run 'Webhook\|Deposit\|Payment\|Callback\|Reversal\|A5\|Casino.*Rollback\|CapRollback\|Tomb\|OpenAPI\|Golden' ./internal/httpserver/` | ok, 296 PASS |
| `-race` `-run 'TestF1Tomb\|TestGateFailCounter\|TestMigration0175\|TestF3_SR5R1\|...\|TestMA020' ./internal/payments/` | ok, no race |

A first payments run hit the default 10 min timeout and showed 6 fixture failures (A5 ReturnReferenceAlreadyHeld, B11
TombstoneNeverClears, F3 SR5R1 x2, F3 R9, MA020_B3): all raw tombstone fixtures on payment providers, converted as listed
in section 6; the rerun above is green. NOT run by me: the full payments suite outside the subset, other packages'
full suites (orchestrator verification run).

## 8. Files and merge notes

New:
- `migrations/0175_deposit_reversal_tombstone_actor_proof.{up,down}.sql`;
- `internal/payments/deposit_tombstone_proof.go`;
- `internal/payments/testsupport_tombstone_integration.go` (integration build tag);
- the tests named in section 6.

Changed:
- `internal/actorproof/actorproof.go`;
- `internal/payments/orchestrator.go`, `receipt.go`, `payout_withdrawal_gate.go`;
- `internal/httpserver/deposit_handlers.go`;
- `internal/withdrawal/dispatchproof.go`;
- the fixtures in section 6.

NO change needed:
- `deploy/init-app-role.sql`, `deploy/aws/sql/init-runtime-role.rds.sql`, `deploy/backup/*`, `deploy/drill/*`.
- Reasons: no new table, grant, schema or SECURITY DEFINER function. The verifier is replaced in place with the same owner and
  ACL. The new column inherits the table's existing grants. The drill seeds write deposits only, never tombstones.
- No new configuration.

Merge notes:
- Merge AFTER 0173 (bonus) and 0174 (reconciliation). This branch does NOT contain them. For local runs I created uncommitted
  placeholder files `0173_placeholder_local_only` and `0174_placeholder_local_only` (`SELECT 1;`), because `migrate` refuses
  version gaps. They are not committed. After merging the real 0173/0174, head is 0175 with no gap. `reservedUnusedVersions` is
  unchanged.
- 0175 replaces `actor_proof_require`. If any other branch also replaces it, the merge must combine the bodies. I checked that
  c1/0173 and c3/0174 do not touch it.
- Any other agent's test that posts a raw `tombstone` on a provider with payment rows now gets AP001. Use
  `payments.PlantVerifiedDepositReversalTombstone` (integration build), and make the test binary import `prooftest` (directly
  or through `wdpath`).
- Apply the registry rows (section 1), the ADR 0110 amendment (3.9), the reworded SRF2R-3 / 6.10 condition, and the residuals
  of section 5 in the launch-risk list, replacing R-6 in the security review's table.

## 9. Review probes (for an independent `security` / `ledger-finance` pass)

1. Can any session context make the non-payment exemption see fewer payment rows than exist? I claim no: the exemption
   requires exactly the GUC state in which `tenant_staff_scope` shows the whole tenant. Acting policies only ever ADD
   visibility.
2. Can the payout exemption be satisfied for a deposit reference? It requires the reference to be the request's own payout
   reference AND no deposit attempt holding it. Its rows never carry a nonce, so they never clear MA020.
3. Is the `split_part(..., 7)` target read sound? It is MAC-covered, and the digest also binds it. The receipt check runs
   after verification.
4. Does the savepoint in `IdempotentInsert` always roll back the nonce on a key conflict? (`TestF1Tomb_Real...` covers a second
   reversal event.)
5. Ledger-finance: there is no new lock, the posting order is unchanged, and the extra index on `ledger_transactions` is partial
   (only proof-bound rows).

## 10. Labels

- F-1 signed deposit tombstone + MA020 proof-bound clearing: `IMPLEMENTED` (residuals in section 5).
- F-3 pins + unexported actor: `IMPLEMENTED`.
- F-4 bounded counter: `IMPLEMENTED`.
- Detective "unsigned tombstone on a payment provider" query: `NOT IMPLEMENTED` (recommended).
- Non-tombstone occupation of a deposit reference by the runtime role: `NOT IMPLEMENTED` (out of scope, flagged).
- Not claimed: an independent security sign-off of this change, `ledger-finance` sign-off, or any regulatory approval.
