# PRH-REF — PROVIDER-REF-BOUND-1: platform provider-reference bound

Owner: `ledger-finance`. Reviewers: `casino`, `payments`, `security`.
Source finding: security review `docs/plans/stage-10.3-planning/10-gate-w2w3-review-security.md` R-2.
Registry: `PROVIDER-REF-BOUND-1` / `PRH-REF` in `docs/governance/task-registry.md`.
Migration: 0099 (allocated by the orchestrator).

Status: **IMPLEMENTED** for code and tests. Two items are still open:
- `security` agreement on the value and charset rule (§8): **AGREED** (2026-09-27); security review verdict in §10: **APPROVE WITH CONDITIONS**.
- The gate review is **PENDING**.

## 1. The problem

Provider references had no length bound anywhere. A verified sender could put several KB (up to the
1 MiB body cap) into `provider_tx_id`. That value exceeds PostgreSQL's btree tuple limit (about 2704 bytes
on 8 KB pages) for every UNIQUE or idempotency index that contains it, so the INSERT failed:

- On the ledger side, nothing was posted, and the provider saw a generic, retryable 500. It would retry
  indefinitely.
- On the rejection side, the evidence row was lost.

## 2. The rule (one platform-wide bound)

`internal/providerref` is a dependency-free leaf package, so the ADR 0095 payments redesign can call it
without importing a domain package. `MaxBytes = 255`.

A provider reference must meet all of the following:
- **1..255 bytes**. This is the octet length, not a character count. A multibyte value gets fewer characters.
- **Valid UTF-8.**
- **No control characters:** U+0000–U+001F, U+007F, or U+0080–U+009F. This is the Unicode Cc category,
  spelled out so the rule never depends on a locale or a Unicode table version. Migration 0099 uses the
  identical regex, `[\x01-\x1F\x7F-\x9F]`. NUL cannot exist in a PostgreSQL text value.
- **Never truncated.** Two long references with a shared prefix would collide on the idempotency key, and
  one provider event would be silently treated as a replay of another.

### Why 255 bytes

- **Index headroom.** The widest composite key is casino_callback_rejections'
  `UNIQUE (tenant_id, provider_id, event_type, provider_tx_id, reason_class)`. Its size is
  16 + 255 + ~9 + 255 + ~32 bytes plus per-column headers, which is under 600 bytes: less than a quarter of
  the btree limit.
- **The ledger idempotency key** `(tenant_id, idempotency_key)` embeds a casino reference in the form
  `tombstone:<provider_id>:<ref>`. That is at most 521 bytes.
- **provider_id is bounded too.** It is bounded by the same rule in every table whose idempotency index
  includes it, so this proof holds at the database level. The webhook route's own charset already limits
  provider_id to 63 bytes.
- **Real-world identifiers fit easily.** The platform's own and common opaque identifier formats are well
  inside the bound: a UUID is 36 bytes, a ULID 26, a 64-hex digest 64. No vendor-specific length is assumed.
  If a contracted vendor documents a longer id, raising the bound requires all of the following:
  - a new migration replacing the 0099 CHECKs;
  - a new `MaxBytes` value;
  - an OpenAPI update;
  - `ledger-finance` and `security` sign-off.

  The real-provider planning gate question 11 ("Maximum length?") is where this gets checked.

## 3. Where the rule is validated

Each domain validates at its own boundary: after webhook verification and adapter parsing, and before any
domain statement (lock, read, write, tombstone, audit).

| Domain | Where | Fields | Rejection |
|---|---|---|---|
| Casino webhook | `casino.Orchestrator.ReceiveVerifiedCallback`, right after `HandleCallback` | provider_tx_id (required), original_provider_tx_id, round_id, provider_game_id, asset_code | `casino.ErrProviderReferenceInvalid`, returned as **400 `validation_error` "callback rejected"**. This is the same class as a malformed verified body. It is not a `casino_callback_rejections` class: the value cannot be stored in that table's bounded columns, so the evidence is **log-only** (§4). No new reason_class was added. |
| Casino play simulation (test-support) | rollback request decode, plus the error mapper | original_provider_tx_id | 400 with a fixed message |
| Casino catalogue | `casino.UpsertGame` | provider_game_id | `ErrInvalidInput`, returned as the existing 400 |
| Payments webhook | `payments.Orchestrator.ReceiveVerifiedCallback`, right after `HandleCallback`. This is **one helper call**, kept small for the ADR 0095 redesign. | provider_reference (required), original_provider_reference, asset_code | `payments.ErrProviderReferenceInvalid`. `mapReceiveCallbackError` returns **400 `validation_error` "callback rejected"**; the simulate route returns 400 "invalid provider reference". |
| Sportsbook catalogue | `sportsbook.SyncCatalogue`. The whole tree is validated before the first upsert. | every external_ref (sport, competition, event, market, selection) | `sportsbook.ErrProviderReferenceInvalid`; nothing is written |
| Sportsbook settlement (test-support) | `validateSettlementEvent` | claim asset_code | `ErrInvalidInput` + `ErrProviderReferenceInvalid`, returned as the existing `VALIDATION_FAILED` 400 |

Every one of these rejections is a 4xx, so a provider does not retry it. Every one is deterministic: a
redelivery gets the same answer and nothing is written. The integration tests assert both properties.

## 4. Logging: no amplification

The webhook handlers emit exactly one allow-listed WARN line:
- `casino_webhook_provider_reference_rejected`
- `payment_webhook_provider_reference_rejected`

The line carries `provider_id`, `tenant_id`, `request_id`, `ref_field`, `ref_reason`, `ref_len` and
`ref_sha256_prefix` (the first 12 hex characters of SHA-256). It never carries the value or the error text.

`providerref.Error` does not hold the value at all, so neither `Error()` nor `LogAttrs()` can leak it.

## 5. Migration 0099

Migration 0099 adds 27 `*_ref_bound` CHECK constraints, each of the form
`octet_length(col) BETWEEN 1 AND 255 AND col !~ '[\x01-\x1F\x7F-\x9F]'`. NULL stays allowed wherever the
column is already nullable.

**Ledger and payments:**
- ledger_transactions: provider_id, provider_tx_id
- deposit_intents: provider_id, provider_reference
- withdrawal_requests: provider_id, provider_reference

**KYC:**
- kyc_verifications: provider_id, provider_reference

**Casino:**
- casino_games: provider_id, provider_game_id
- casino_launch_sessions: provider_id, provider_game_id
- casino_provider_rounds: provider_id, provider_round_id, provider_session_id
- casino_callback_rejections: provider_id, provider_tx_id, original_provider_tx_id, round_id, asset_code

**Sportsbook:**
- sportsbook_bets: provider_id, provider_bet_reference
- sb_sports, sb_competitions, sb_events, sb_markets, sb_selections: external_ref

### Pre-flight

The pre-flight counts violating rows per column. If any exist, it fails with a message such as
`migration 0099 pre-flight: N existing row(s) violate ...: casino_callback_rejections.provider_tx_id=2 sb_sports.external_ref=1`.
It never truncates or deletes a row.

The tenant tables carry FORCE RLS, which would make a naive `count(*)` return zero. To avoid that, the
pre-flight follows migration 0095's pattern: it loops over `tenants` and sets `app.tenant_id` for each
tenant. That satisfies each table's staff-scope policy; FORCE is never toggled. The `ADD CONSTRAINT`
validation is the second line of defence, because constraint validation cannot be filtered by RLS.

### No NOT VALID + VALIDATE split

The runner (`internal/db/migrate.go`) runs each file in one transaction, so the ACCESS EXCLUSIVE lock is
held until commit either way. If a production-sized table ever needs the split, it must be two migrations.
That is a deployment decision for real-provider go-live.

### Comment correction after merge (code review F6)

The 0099 header comment originally claimed "tenants has no RLS". That was wrong: `tenants` carries
FORCE RLS since 0077, and the loop lists every tenant only because `tenants_read` is `USING (true)`.
The comment now says so, including the consequence if that policy is ever narrowed: no per-column
report, but the migration still fails closed through ADD CONSTRAINT (23514).

This is a comment-only edit. It does change the file's SHA-256, so `migrate verify` reports a checksum
mismatch on any database that already applied the earlier 0099 bytes. Only local/dev databases can be
affected (CI builds fresh databases, staging is off). Rebuild those databases, or re-run
`migrate up` on a fresh one.

### Down migration

The down migration drops all 27 constraints. It is unconditionally reversible, because dropping a CHECK
loses no data.

### Chain-tip pins (0098 → 0099)

- `internal/bonus/wave3_phase2_migrations_integration_test.go`: gains `migration0099Version`; rollback depth 31 → 32.
- `internal/operatingmarket/migration_0076_integration_test.go` and
  `internal/operatingmarket/qa_migration_rls_survives_failed_rollback_test.go`: gain `migration0099Version`;
  depth 23 → 24.
- `internal/jurisdiction/migration_0077_integration_test.go`: gains `migration0099Version`; depth 22 → 23.
- No change was needed in these, which already use the derived hold-back patterns:
  - `internal/jurisdiction/migration_0075_integration_test.go` stages ≤ 0098.
  - `internal/ledger/migration_0092_integration_test.go` holds back everything > 0092.
  - `internal/reconciliation/migration_0098_integration_test.go` stages ≤ 0098.

## 6. API (OpenAPI)

- `maxLength: 255` (plus `minLength: 1` where required) is documented in these places:
  - on the provider references in `CasinoMockCallback` and `PaymentsMockCallback`;
  - on the play rollback request;
  - on these response fields: `CasinoCallbackRejection`, `Round`, `AdminRound.provider_round_id`,
    `AdminBet.provider_bet_reference` and `CasinoPlayResult.provider_tx_id`.
- The casino and payments webhook `400` descriptions name the bound and say "do not retry".

## 7. Tests and evidence

**Unit tests:**
- `internal/providerref/providerref_test.go` covers:
  - exact max, max+1, a multibyte value at the boundary (a 128-rune/256-byte value, and a 3-byte rune
    straddling the boundary);
  - invalid UTF-8 (lone continuation, truncated, overlong, surrogate, 0xFF), and C0/DEL/C1 control characters;
  - empty required vs optional, and first-failure-wins;
  - no value in `Error()`/`LogAttrs()`, with bounded output.
- `internal/casino/provider_reference_bound_test.go` (`validateCallbackReferences`).
- `internal/sportsbook/provider_reference_bound_test.go` covers every catalogue level and the settlement asset code.
- `internal/httpserver/payment_callback_errors_test.go` covers the mapping.

**Integration tests, per domain:**
- **Casino:** `internal/casino/provider_reference_bound_integration_test.go`.
  - Oversize bet, win or rollback references (own, original, round, game, asset; ASCII, multibyte and
    control character) are a deterministic rejection on two deliveries.
  - Nothing is written: ledger, rounds, rejections and audit counts and the balance are unchanged, and the
    ledger stays balanced.
  - Exact-max bet, win and rollback references post once, are replays on redelivery, and are stored verbatim.
  - A tombstone for an exact-max never-seen original is followed by an E3 decline and a rejection row.
- **Payments:** `internal/payments/provider_reference_bound_integration_test.go`.
  - Oversize deposit or reversal references (own, original, asset; multibyte; C1) are rejected, and the
    ledger/entries/audit counts and intent status are unchanged.
  - An exact-max reversal reference posts once, resolves to the same ledger transaction on redelivery, and
    is stored verbatim; the ledger stays balanced.
- **Sportsbook:** `internal/sportsbook/provider_reference_bound_integration_test.go`.
  - An oversize selection ref causes zero catalogue rows to be written.
  - An exact-max ref is stored verbatim, and a re-sync is idempotent.
  - The settlement field matrix gains the asset_code cases.
- **HTTP and logs:** `internal/httpserver/provider_reference_bound_webhook_integration_test.go`.
  - Casino and payments webhooks return 400 with one allow-listed line carrying `ref_len` and the hash prefix.
  - No captured log line, from any logger call in the request, contains the 4 KiB value.
  - The play rollback returns 400.
- **Migration:** `internal/casino/migration_0099_integration_test.go`, on scratch databases.
  - After up: 27 constraints; 256 bytes, 128 two-byte runes and C1 are refused with 23514 on the named
    constraint; 255 bytes is accepted; the ledger column is enforced.
  - Down removes all 27; after down an over-bound insert is possible again, and re-up then fails its pre-flight.
  - The pre-flight test covers one FORCE-RLS tenant table and one platform table. It checks the loud
    per-column counts, and that no constraint was added, 0099 was not recorded, and the rows are untouched.

**Mutation evidence:** `docs/plans/payment-readiness/evidence/prh-ref-mutation-kill.txt` (33/33 killed; re-runnable with the committed harness `docs/plans/payment-readiness/evidence/prh-ref-mutate.py`).

## 8. Security agreement (value and charset)

The proposal to `security`:
- 255 bytes, octet-based;
- valid UTF-8, with no C0, DEL or C1 control characters;
- never truncated;
- 400 non-retryable after verification;
- log length plus a 12-hex SHA-256 prefix only.

**Status: AGREED by `security` (2026-09-27).** Ruling:

1. **Value.** 255 bytes, counted as octets (`len()` in Go, `octet_length()` in SQL), minimum 1 for a
   present value. This bound is agreed. It keeps every composite UNIQUE/idempotency key under about a
   quarter of the btree tuple limit, including the 521-byte `tombstone:<provider_id>:<ref>` key. It also
   stays well above every opaque identifier format the platform issues or expects. Raising it follows the
   §2 procedure, and `security` sign-off is part of that procedure.
2. **Charset.** Valid UTF-8, and no U+0000–U+001F, U+007F or U+0080–U+009F. This is agreed as the minimum
   rule. The rule rejects CR/LF/TAB and the C1 controls, which closes log-line and CSV/report injection
   through a reference. It deliberately does **not** reject Unicode format characters (for example
   U+200B–U+200F, U+202A–U+202E, U+2066–U+2069) or confusables. That is acceptable, for two reasons:
   references are opaque, compared byte-exactly and never normalised, and they are never used for display
   authorisation. Any **display** surface (back office, partner console, exported reports) must
   render or escape them as untrusted text. That is an output-encoding obligation, not a change to this
   rule.
3. **Never truncated.** Agreed and mandatory. Truncation would turn two distinct provider events that
   share a prefix into an idempotency collision, so one event would be silently swallowed as a replay.
4. **Rejection.** A deterministic, non-retryable 400, raised only **after** webhook verification. It is
   agreed that nothing is written, including no evidence row. The request is authenticated, so this is
   not a pre-authentication oracle. The uniform-401 contract for unauthenticated failures is unaffected.
5. **Logging.** Agreed: log only field, reason, byte length and the first 12 hex characters of SHA-256.
   48 bits is enough to correlate redeliveries. It is not a confidentiality control, and it does not need
   to be one: references are provider identifiers, not secrets. It must never be described as one.
   Neither the value nor the error text may be logged. `providerref.Error` does not hold the value, so this
   holds by construction.

## 9. Residuals (not built here, recorded rather than silently skipped)

1. **Payments outbound responses (for PRH-I1 / ADR 0095; security C1, code review F2).**
   `DepositResult.ProviderReference` and `WithdrawResult.ProviderReference` come back from the adapter
   without app-level validation. They are written to `deposit_intents`, `withdrawal_requests`
   (`MarkSubmitted`) and `ledger_transactions.provider_tx_id` (`withdrawal.Complete`).

   An over-bound value makes the 0099 CHECK fail, with a generic 500. That is **not** "fail closed" on
   every path.
   - **Deposit initiation:** nothing has moved yet, so the transaction rollback is safe.
   - **Withdrawal payout:** `withdrawal_handlers.go` calls `provider.Withdraw` **inside** the domain
     transaction. The CHECK then fails **after** the PSP has executed the payout. The rollback leaves the
     request unsubmitted while the money has left. A staff retry calls `Withdraw` again, which is a
     **double-payout** path whenever the PSP does not deduplicate on `MerchantReference`.

   The root cause (provider I/O inside the transaction) predates 0099 and is ADR 0095's subject. 0099
   adds a new deterministic trigger for it, where before only a reference over about 2.7 KB or one with
   NUL triggered it. The ADR 0095 redesign must:
   - call `providerref.Validate` on every adapter-response reference: `Deposit`, `Withdraw`,
     `QueryStatus` inputs and KYC `CreateVerification`;
   - treat a violation as a durable, non-retryable provider-protocol failure that parks the operation
     for manual reconciliation, never rolling back to a state from which the payout can be resubmitted.

   This blocks real-PSP/KYC go-live and the PRH-I1 close-out (security C1); it does not block closing
   PROVIDER-REF-BOUND-1. The call was deliberately not added to the current orchestrator, per the
   coordination instruction.
2. **KYC.** KYC has the DB CHECK only. The KYC callback just looks the reference up, and an oversize lookup
   finds nothing, which is deterministic. The KYC outbound `CreateVerification` reference has the same
   residual as item 1. Owner: `identity-compliance`.
3. **Casino evidence for an over-bound reference is log-only**, by design (§3).
4. **`sportsbook_bets.provider_bet_reference`** has no writer yet. It is bounded by CHECK only. The future
   bet-placement adapter must validate at its boundary.
5. **GitHub CI evidence** is unavailable while CI-BILLING-1 is open. The evidence here is local runs only.

## 10. Security review of commit 43f5071: verdict APPROVE WITH CONDITIONS

Reviewer: `security`, 2026-09-27. Reviewed `git show 43f5071` at HEAD 28baca0.

### What was verified

- **Placement.** In both `casino.Orchestrator.ReceiveVerifiedCallback` and
  `payments.Orchestrator.ReceiveVerifiedCallback`, the only statements before the bound are the ADR 0094
  Redeem/Recheck of the verified handle and the adapter's `HandleCallback`. The bound runs before
  dispatch, which means before any lock, read, write, tombstone or audit in the domain. On a rejection,
  `WithTenant` rolls the transaction back, so the Redeem is not committed either.
  `recordCasinoCallbackRejection` is a no-op, because the error is not a `*casino.CallbackRejectedError`.
  In the payments handler, no earlier `errors.Is` branch matches the new sentinel, so the error cannot
  fall through to the generic 500.
- **Responses.** Both webhooks return a fixed-body 400 `callback rejected`. The simulate and play routes
  also use fixed messages. The value is never echoed.
- **Logs.** `logProviderReferenceRejected` emits allow-listed attributes only and never `err`. The
  `Error()` text of a wrapped error names the field, reason, length and hash, never the value. The length
  check runs first, so an oversize value is never scanned by the UTF-8 or control-character checks.
- **Migration 0099.**
  - The CHECK predicate matches the Go rule.
  - The pre-flight sets `app.tenant_id` for each tenant and never touches FORCE RLS or `row_security`.
  - The pre-flight only counts, and it raises an exception on any violation. It contains no UPDATE and no
    DELETE.
  - The RLS-independent `ADD CONSTRAINT` validation backs the pre-flight up. If a table's policy ever hid
    rows from the pre-flight, the migration still fails closed (with 23514); it never proceeds.
  - The down migration only drops constraints.
- **Mutation spot-checks.** Both were novel (not in the M1–M28 list), reverted, and left `git diff` clean.
  1. **Bound counted in runes instead of bytes** (`len(value)` became `utf8.RuneCountInString(value)`):
     **KILLED** by `TestValidate_MultibyteAtBoundary`.
  2. **Casino `asset_code` dropped from `validateCallbackReferences`:** **KILLED** by
     `TestValidateCallbackReferences`.

  The baseline unit tests for providerref, casino, payments and httpserver pass. I had no credentials for
  the local PostgreSQL, so I did not re-run the integration and migration tests. For those I relied on the
  implementer's local evidence (`evidence/prh-ref-mutation-kill.txt`).

### Conditions

**C1: blocks real-PSP/KYC go-live and the PRH-I1 / ADR 0095 close-out. It does not block closing
PROVIDER-REF-BOUND-1.**

The §9.1 residual is broader than the note states. Consider `withdrawal_handlers.go`:
1. It calls `provider.Withdraw` **inside** the domain transaction.
2. It then writes `result.ProviderReference` to `withdrawal_requests` (`MarkSubmitted`) and to
   `ledger_transactions.provider_tx_id` (`withdrawal.Complete`).

If a real PSP returns an over-bound reference after executing the payout, the 0099 CHECK fails. The
transaction rolls back and the request stays unsubmitted, although the money has left. A staff retry then
calls `Withdraw` again, which is a **double-payout** path whenever the PSP does not deduplicate on
`MerchantReference`.

The root cause is pre-existing: provider I/O inside the transaction, which is already ADR 0095's subject.
Migration 0099 adds a new deterministic trigger for it. The ADR 0095 redesign must do both of the
following:
- call `providerref.Validate` on every adapter-response reference: `Deposit`, `Withdraw`, `QueryStatus`
  inputs, and KYC `CreateVerification`;
- treat a violation as a durable, non-retryable provider-protocol failure that parks the operation for
  manual reconciliation. It must never roll back to a state from which the payout can be resubmitted.

§9.1 should also name `ledger_transactions.provider_tx_id` (through `withdrawal.Complete`) alongside
`deposit_intents` and `withdrawal_requests`.

**C2: before go-live.** For an over-bound casino callback, the only evidence is the log line (§9.3). The
following must be true:
- the retention of `casino_webhook_provider_reference_rejected` and
  `payment_webhook_provider_reference_rejected` meets the evidence-retention requirement for callback
  rejections;
- both events are alertable by rate per provider. A verified sender repeatedly hitting the bound is a
  provider-integration fault or a compromised credential.

**C3: before go-live.** Re-run the 0099 integration, migration and mutation evidence on GitHub CI once
CI-BILLING-1 is closed (§9.5).

### Out of scope for this review

- The OpenAPI changes.
- The sportsbook catalogue and settlement paths, beyond confirming that they call the same package.
- KYC callback behaviour, beyond the §9.2 reasoning.
- Production lock and duration impact of the non-split `ADD CONSTRAINT`, which is a deployment decision.
- Display-side escaping of references (§8 point 2).

This review is a code-level and design-level review of one commit. It is not a penetration test, and it
does not certify the payment paths as secure.

