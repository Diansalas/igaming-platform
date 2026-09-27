# PRH-REF — PROVIDER-REF-BOUND-1: platform provider-reference bound

Owner: `ledger-finance`. Reviewers: `casino`, `payments`, `security`.
Source finding: security review `docs/plans/stage-10.3-planning/10-gate-w2w3-review-security.md` R-2.
Registry: `PROVIDER-REF-BOUND-1` / `PRH-REF` in `docs/governance/task-registry.md`.
Migration: 0099 (allocated by the orchestrator).

Status: **IMPLEMENTED** for code and tests. Two items are still open:
- `security` agreement on the value and charset rule (§8) is **PENDING**.
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

**Mutation evidence:** `docs/plans/payment-readiness/evidence/prh-ref-mutation-kill.txt`.

## 8. Security agreement (value and charset)

The proposal to `security`:
- 255 bytes, octet-based;
- valid UTF-8, with no C0, DEL or C1 control characters;
- never truncated;
- 400 non-retryable after verification;
- log length plus a 12-hex SHA-256 prefix only.

**Status: PENDING.** `ledger-finance` cannot record another specialist's agreement on that specialist's
behalf. The orchestrator must obtain `security`'s ruling and record it here (verbatim or by reference to a
review file) before PROVIDER-REF-BOUND-1 is closed.

## 9. Residuals (not built here, recorded rather than silently skipped)

1. **Payments outbound responses (for PRH-I1 / ADR 0095).** `DepositResult.ProviderReference` and
   `WithdrawResult.ProviderReference` come back from the adapter and are written to `deposit_intents` and
   `withdrawal_requests` without app-level validation. The 0099 CHECK makes an over-bound value fail closed,
   but as a generic 500. The redesign must call `providerref.Validate` at the adapter-response boundary and
   treat a violation as a deterministic provider-protocol failure. The same applies to `QueryStatus` inputs.
   This call was deliberately not added to the current orchestrator, per the coordination instruction.
2. **KYC.** KYC has the DB CHECK only. The KYC callback just looks the reference up, and an oversize lookup
   finds nothing, which is deterministic. The KYC outbound `CreateVerification` reference has the same
   residual as item 1. Owner: `identity-compliance`.
3. **Casino evidence for an over-bound reference is log-only**, by design (§3).
4. **`sportsbook_bets.provider_bet_reference`** has no writer yet. It is bounded by CHECK only. The future
   bet-placement adapter must validate at its boundary.
5. **GitHub CI evidence** is unavailable while CI-BILLING-1 is open. The evidence here is local runs only.
