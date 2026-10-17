# Closure security RE-review record (independent reviewer, `security`)

Worktree `.claude/worktrees/secrev2`, branch `secrev2` from `claude/focused-wright-jw88w9` @ `d64d478`. Range reviewed:
`2daf352..d64d478`. That covers:
- tomb: F-1 / F-3 / F-4 and migration 0175;
- c1: migration 0173 and `deposit_sweep.go`;
- c3: migration 0174 and `negative_balance.go`;
- the `ServerCursor.Fetch -> NextBatch` rename.

Date 2026-10-17. I did not write any of this code and I fixed nothing. This file is the only change on the branch.

Scope and limits:
- This is a code- and design-level review against MOCK providers and synthetic data.
- It is not a pentest, a certification audit, or a regulatory or legal approval.
- Passing this review does not make the subsystem "secure". Section 6 lists what was not in scope.

Attacker models:
- (A) a SQL-only actor holding the stolen `igaming_runtime` credential;
- (B) a staff insider;
- (C) an external webhook sender or a player.

## 1. Verdict

**APPROVE WITH CONDITIONS** for the provider-independent freeze.

**F-1 (forged deposit tombstone) is CLOSED for its two original harms against attacker A, on every reference of a provider
the tenant holds a payment row for:**
- **MA020 cannot be cleared.** `payment_ref_cleared` counts only rows that carry a verifier-consumed nonce. The nonce column
  cannot be set by the caller (the guard overwrites it) and cannot be updated later (`ledger_transactions_immutable`).
- **A tombstone cannot be forged on such a reference.** Every one is refused AP001-AP005 / DT001 without the key.

What remains is availability (denial-of-service) residuals for attacker A, plus two backstop-precision issues:
- none of them lets attacker A clear MA020;
- none of them lets attacker C cause a money loss;
- none of them requires a code change before the freeze.

The conditions in section 4 are about the accuracy of the registry and residual text.

No finding here should block launch on its own. The general residual "the runtime credential can write ledger rows (any type,
any reference, zero entries)" was confirmed again (S-1b) and must stay in the launch-risk list. It is the root of S-1 and
of tomb residual 2.

## 2. What was verified (and how)

Private DB `secrevtwo165940p13150x01`, built by `priv_db.sh` to head 0175 and dropped at the end. Tests ran with
`-tags integration -count=1 -p 1`.

- Scratch probes ran as the REAL `igaming_runtime` role (`f1RuntimePool`: `rolsuper=f`, `rolbypassrls=f`). They were in
  `internal/payments/zz_secrev2_probe_integration_test.go` and `internal/bonus/zz_secrev2_probe_integration_test.go`. Both
  files were deleted and neither was committed.
- Existing tests, all ok:
  - `./internal/payments/ -run 'TestF1Tomb|TestMigration0175'`: 7 tests PASS (all F1Tomb tests, catalog, up/down/up and DT095);
  - `./internal/bonus/ -run 'FirstDeposit|DepositSweep'`;
  - `./internal/reconciliation/ -run 'C3|NegativeBalance|Migration0174|MA020_Parity|R9'`;
  - `./internal/casino/ -run 'Rollback|Tomb'`;
  - `go test ./internal/txscope/ ./internal/actorproof/...` (static pins, including `TestStatic_DepositTombstoneSignedOnlyHere`,
    and the INV-IO-1 analyzer).
- Catalog checks:
  - `actor_proof_require` is owned by `igaming`, is SECURITY DEFINER, has `search_path` pinned, and has ACL
    `{igaming=X, igaming_runtime=X}` (unchanged by CREATE OR REPLACE);
  - the guard and `payment_ref_cleared` are not SECURITY DEFINER and have `search_path` pinned;
  - `ledger_transactions_zz_deposit_tombstone_proof_guard` is the last BEFORE INSERT row trigger by name;
  - the runtime has no TEMP privilege. `deploy/init-app-role.sql:86-87` revokes it in deployment too.

### 2.1 0175 / F-1: results as attacker A (runtime role)

| Probe | Result |
|---|---|
| Raw deposit tombstone on a captured-unposted reference: deposit key, casino key, caller nonce, platform-admin GUC | AP001 (also covered by `TestF1Tomb_ForgedDepositTombstone_RuntimeRefused`) |
| Forged token (no key) with a runtime-planted, applied `deposit_reversal` receipt as target | AP001 / AP002. The receipt row alone is worth nothing without the MAC. |
| `UPDATE ledger_transactions SET actor_proof_nonce = ...` / `DELETE` of a signed tombstone | refused (`... is append-only`) |
| Use the new system scope for another operation, or the dispatch actor for this one | AP004 (binding matrix; Go mirror `systemClaimAdmitted`) |
| Replay or reuse of a proof for another row / tenant / receipt | AP005 / AP004 / DT001 (binding matrix) |
| Down migration as runtime | not possible (owner-only DDL). As owner it refuses DT095 while signed tombstones exist. |
| Provider-less tombstone on the legitimate key `tombstone:<psp>:<ref>` | **admitted** (S-1) |
| Tombstone naming a non-payment provider on the legitimate key `tombstone:<psp>:<ref>` | **admitted** (S-1) |
| Non-tombstone row (`deposit`, `deposit_reversal`, `casino_bet`, `casino_rollback`, `sportsbook_bet`) with ZERO entries on `(psp, ref)` | **admitted** (S-1b; `withdrawal_completed` refused WD043) |
| Planted `payment_provider_events` row naming a casino provider id, then a casino rollback tombstone | **AP001 from then on**. The event cannot be deleted. (S-2) |
| `payment_attempts` visible to the runtime: plain tenant context vs `app.platform_admin_principal_id` set | 1 vs **0** (S-3) |

### 2.2 Verifier change

- The `system` block now admits exactly two (operation, actor) pairs. Each operation is refused for any other scope or actor
  (AP004), and the `ELSIF p_tenant IS NULL` branch still requires a tenant.
- The LIKE patterns escape `_`.
- Every other trigger passes a fixed, non-system operation, so no existing caller can reach the new pair.
- The rest of the body is byte-identical to 0171 (diffed against the down's restored body).
- The proof can only be consumed. Calling `actor_proof_require` directly with a stolen proof burns it, nothing else.

### 2.3 Trigger hardening

- The caller's nonce is overwritten before any decision.
- The nonce is read from the same GUC in the same function, right after the verifier consumed it. No user code runs in
  between, and every other BEFORE trigger has already run.
- The `split_part(...,7)` target is MAC-covered and also inside the digest. The receipt check (DT001) runs after
  verification.
- `search_path` is pinned and references are schema-qualified.
- The trigger takes no lock.

### 2.4 Attacker C: can a PSP callback tombstone an already-credited deposit (money loss)?

**No**, with the following reasoning:
- The only producer of the signed tombstone is `applyReversalReceiptEvidence` (`internal/payments/receipt.go:1468`), reached
  only from the adapter-verified callback (`orchestrator.go:1153` -> `receiveCallbackViaReceiptPath` -> `ApplyReceiptEvidence`).
- The tombstone-vs-debit branch is decided on a fresh read taken under the `deposit_intents` row lock.
- A credited deposit has `LedgerTransactionID` set, so it takes the compensating `deposit_reversal` posting branch.
- In the unresolved (no attempt, no lock) branch, the deposit posting (`orchestrator.go:996`, `provider_tx_id = ref`) and the
  tombstone (`provider_tx_id = ref`) collide on `idx_ledger_transactions_tenant_provider_tx`. Whichever commits first wins:
  - a deposit first: the tombstone fails ErrProviderReferenceTaken, the PSP retries, the attempt now resolves, and the debit
    branch runs;
  - a tombstone first: the late success is rejected (`receipt.go:1722`).
- The receipt binding (target = the stored applied `deposit_reversal` receipt of the same tenant / provider / original
  reference, DT001) is sufficient for its purpose: it pins the proof to the verified evidence of the same transaction.
- Its strength against C is exactly the adapter's webhook signature verification. That is **PROVIDER DEPENDENT** (the
  adapters are MOCK).

### 2.5 Attacker B (staff)

- No back-office route reaches `postDepositReversalTombstone` or the signer.
- Staff hold no `system` scope.
- The integration-only fixture `PlantVerifiedDepositReversalTombstone` carries `//go:build integration` on line 1 (pinned).

### 2.6 0174 / C3 detector

- It is read-only: one SELECT, tenant bound as `$1`. The only string substitution is a compile-time constant chosen by
  `to_regclass`.
- It fails closed on a blind read: the session is not exactly the tenant scope, or no entries are visible while projections
  show postings.
- Alerts use the existing P1 Kind with ONE stable discriminator per (tenant, finding kind), so occurrences grow and alerts do
  not flood.
- Mismatch rows per run grow with the real number of negative accounts and dispatched requests. Only real ledger state
  (chargebacks) can drive them, so neither attacker C nor a player can flood them.
- `MismatchSubject` maps the new kind to `ledger_account` only.

### 2.7 0173

- A caller-supplied `first_deposit_only` is overwritten, the flag is immutable, and terminal status does not free the slot.
- The app rule is stricter than the index (it counts any grant of the offer).
- **The backstop can be bypassed by a direct SQL writer** (S-4).

### 2.8 NextBatch rename

- The INV-IO-1(c) analyzer (`internal/txscope/no_provider_call_in_tx_closure_static_test.go:94`) is name-based and still flags
  `Fetch`. The rename removes a false positive: a DB cursor FETCH in the same transaction is not provider I/O.
- No analyzer list, exemption or assertion changed. The analyzer test passes.

## 3. Findings (severity-ranked)

### S-1 LOW-MEDIUM (A; availability with a bounded money tail) - the tombstone KEY namespace is not reserved

Locations:
- `migrations/0175_deposit_reversal_tombstone_actor_proof.up.sql:236`: early exit when `provider_id IS NULL`;
- `migrations/0175_deposit_reversal_tombstone_actor_proof.up.sql:252-257`: the non-payment exemption looks at the row's
  `provider_id`, not at its key;
- `internal/payments/orchestrator.go:1344`: the legitimate key `tombstone:<psp>:<ref>`.

The attack:
- The runtime inserts a tombstone with NO provider, or with a provider that has no payment rows, under the idempotency key
  `tombstone:<psp>:<deposit ref>`. Verified: admitted.
- The genuine signed reversal tombstone for that reference then fails `ErrIdempotencyPayloadMismatch` (verified).
- The webhook answers a generic 500 (`internal/httpserver/deposit_handlers.go:655`).
- The whole transaction, receipt included, rolls back. So there is no durable trace, no alert, and an endless PSP retry loop.

This is NOT tomb residual 2. That one occupies `(psp, ref)` and so blocks the late success. Here `(psp, ref)` stays free:
- if the late original success arrives, it is CREDITED;
- the reversal's next retry then resolves the attempt and posts the compensating debit, so it is money-safe while the PSP keeps
  retrying;
- if the PSP stops retrying first, the reversal is lost and the credit stands. That money tail is bounded by the PSP's retry
  policy and is visible only to statement reconciliation.

S-1b: the same probe confirmed that non-tombstone rows with ZERO entries are admitted by SQL on any `(psp, ref)`. The
"only a tombstone may have no entries" rule exists only in Go (`internal/ledger/lockorder.go:372`). This is the root of tomb
residual 2.

Fix (needs a migration; not done):
- in the guard, refuse any `tombstone` whose `idempotency_key` starts with `tombstone:` unless it equals
  `'tombstone:' || provider_id || ':' || provider_tx_id`. Casino uses the same shape (`internal/casino/orchestrator.go:2861`),
  so this does not affect it. Sportsbook tombstones use the settlement key (`internal/sportsbook/settlement.go:202`) with no provider, not the `tombstone:` prefix, so they are not affected either;
- a deferred constraint "a non-tombstone transaction has at least two entries at commit" (`ledger-finance`).

Detection today: none in SQL. Recommended detective: unsigned tombstones whose key names a payment provider.

### S-2 LOW (A, or a configuration collision; fail-closed DoS that cannot be undone without the owner) - casino rollback tombstones

Locations: `migrations/0175_...up.sql:252-257`. Disclosed as tomb residual 4. I verified it, and it is worse than "loud":
- The runtime can insert a `payment_provider_events` row naming a casino provider id (insert verified).
- From then on, every unseen-original casino rollback tombstone of that provider in the tenant requires a deposit proof and
  fails AP001 (verified).
- `payment_provider_events` is append-only and DELETE is refused (verified). Recovery therefore needs owner DDL or a
  migration.
- The same happens with no attacker at all if a tenant configures a casino provider id equal to a payment provider id.
  `provider_capabilities` and `casino_provider_capabilities` share no uniqueness rule.

Player impact:
- the rollback-for-unseen-bet is not recorded;
- a late bet would then be accepted and debited;
- the rollback retry later reverses it, provided the game provider keeps retrying. If it stops, the stake is lost. This is
  bounded by the provider's retry policy.

Fix (follow-up):
- a configuration-time refusal of overlapping casino/payment provider ids per tenant (no migration needed);
- a runbook entry for the owner-level remedy;
- an alert on AP001 from the casino rollback path.

### S-3 LOW / INFO (A + B or key) - the payout exemption depends on RLS visibility

Location: `migrations/0175_...up.sql:240-251`.

- The "no DEPOSIT attempt holds this reference" clause runs under the caller's RLS.
- With `app.platform_admin_principal_id` set, `payment_attempts` is invisible (verified 1 -> 0), while `withdrawal_requests`
  and the `ledger_transactions` INSERT stay allowed. The clause is then vacuously true.

Exploitation still requires all of the following, so it is not practical for A alone:
- WD027 (`migrations/0165:249`): two proof-bound reverse approvals on the correlated request;
- the request's own set-once `provider_reference` equal to the deposit reference.

The resulting row carries no nonce, so it cannot clear MA020. Its only effect is to block a late success, which S-1b achieves
more cheaply anyway.

The tomb record's claim (section 5, "cannot ... under player or platform-admin GUCs") is verified for the deposit / casino
cases. It is not structurally true of the payout exemption.

Fix (follow-up): pin the same plain tenant GUC context in the payout exemption as in the non-payment exemption, or fail
closed outside it.

### S-4 LOW (A) - the 0173 backstop is bypassable by an offer / version mismatch

Location: `migrations/0173_bonus_first_deposit_single_grant_backstop.up.sql:44-52`.

- `bonus_grants` has independent FKs `(offer_id, tenant_id)` and `(offer_version_id, tenant_id)`, but none on
  `(offer_version_id, offer_id)`.
- Verified (owner pool, RLS forced): a second and a third grant for (tenant, FDO offer X, player) were admitted when
  `offer_version_id` named a version of a non-FDO offer. The flag then derives `false`.
- The application path always pairs them consistently. So the backstop holds against app-path defects and races (its stated
  purpose, and attacker C is closed), but not against attacker A.
- Attacker A gains nothing new from this: they can already insert unbounded grants on any non-FDO offer.
- Hygiene: both 0173 functions lack `SET search_path`. 0175 pins it everywhere. pg_temp shadowing is not exploitable here
  because TEMP is revoked.

Fix (follow-up, migration): validate that the version belongs to `offer_id` in the derive trigger (or add a composite FK),
and pin `search_path`.

### INFO

- I-1: 0175's guard runs `EXISTS (payment_attempts WHERE tenant_id AND provider_id)` for every non-payment tombstone. The only
  `provider_id` index on `payment_attempts` is partial (`provider_reference IS NOT NULL`), so this is a scan of the tenant's
  attempts per casino unseen-original rollback. Performance only; a candidate index for `ledger-finance`.
- I-2: the 0175 up adds a CHECK constraint (a validating scan under ACCESS EXCLUSIVE, with a 5 s `lock_timeout`) on
  `ledger_transactions`. That is fine pre-launch, but needs a NOT VALID + VALIDATE pattern for any post-launch equivalent.
- I-3: the C3 query aggregates every player-account entry of a tenant every hour (no watermark). Performance and ledger-finance
  matter only; no security effect.
- I-4: the `payment_ref_cleared` and guard functions have the default PUBLIC EXECUTE. Harmless: the guard is a trigger
  function, and `payment_ref_cleared` is not SECURITY DEFINER and is subject to RLS.

## 4. Conditions (orchestrator, registry text only)

1. F-1 row: "IMPLEMENTED. MA020 clearing is proof-bound. Tombstone forgery on payment references is closed against the
   runtime credential. OPEN availability residuals: S-1 (key squat), S-1b / tomb residual 2 (reference occupation by
   zero-entry rows), S-2 (casino rollback DoS, no owner-free recovery), S-3 (payout exemption context)." Do not register
   F-1 as closing every runtime-credential tombstone effect.
2. C1 row: "DB backstop holds against application-path defects and races. It does not hold against the runtime credential
   (S-4)."
3. Track S-1, S-2, S-3 and S-4 as LOW follow-ups owned by `security` (S-1, S-3), `ledger-finance` (S-1b, S-4) and casino /
   config (S-2). None blocks the provider-independent freeze. S-1 and S-1b should be closed, or explicitly accepted by the
   owner, before real-money launch.
4. Keep "the runtime credential can write ledger rows (any type, zero entries)" in the launch-risk list. It is the general
   residual behind S-1b.

## 5. Precise residual list after this range

| # | Residual | Model | Impact | Detection |
|---|---|---|---|---|
| R-a | S-1 key squat on `tombstone:<psp>:<ref>` | A | the genuine reversal 500s with no durable trace; money loss only if the PSP gives up before the late success | none (recommend a detective) |
| R-b | S-1b zero-entry non-tombstone rows on `(psp, ref)` (tomb residual 2) | A | genuine deposit / reversal posting blocked | none in SQL |
| R-c | tomb residual 1: pre-empting a never-used provider's future references | A | late success rejected (no MA020 effect) | R9 / C5 tombstone checks |
| R-d | S-2 casino rollback DoS via a planted receipt or an id collision | A / config | unseen-original rollbacks fail AP001 until owner DDL | AP001 in logs only |
| R-e | S-3 payout exemption RLS-context sensitivity | A + B | late success blocked (no MA020 effect) | WD027 trail |
| R-f | S-4 0173 offer / version mismatch | A | extra FDO grants (no gain over non-FDO grants) | none |
| R-g | T6: the holder of the signing key can sign deposit tombstones | process compromise | MA020 cleared; late success rejected | key-access audit |
| R-h | Proof capture within its lifetime (pgaudit `log_parameter` must stay off) | log reader | the same row the app writes | nonce table |
| R-i | Receipt binding strength vs C = adapter webhook verification | C | PROVIDER DEPENDENT | adapter |

## 6. Not in scope / not run

- Full package suites (only the targeted runs above).
- The NA / U items from the previous range.
- Real PSP or casino adapters.
- AWS deploy grants. I read `deploy/init-app-role.sql` for TEMP only.
- No load or performance measurement of I-1 / I-3.

## 7. Labels

- This review: IMPLEMENTED (record only).
- S-1 to S-4 fixes: NOT IMPLEMENTED (recommendations; S-1, S-3 and S-4 need a migration, which I may not add).
- Receipt binding against attacker C: PROVIDER DEPENDENT.
