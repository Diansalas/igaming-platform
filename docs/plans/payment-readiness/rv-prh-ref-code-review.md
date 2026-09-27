# PRH-REF (PROVIDER-REF-BOUND-1): independent code review of commit 43f5071

Reviewer: `code-reviewer`, 2026-09-27. Scope: `git show 43f5071` on branch
`claude/focused-wright-jw88w9` (HEAD 28baca0), read against CLAUDE.md, the design note
`prh-ref-provider-reference-bound.md` and the security review in its §10.

## Verdict: READY WITH FOLLOW-UPS

I found no correctness bug in the commit's own scope. The Go rule and the 0099 CHECKs agree. Every inbound
verified provider reference is bounded before any domain statement runs. The migration is fail-closed and
never rewrites data. The findings below are follow-ups; none blocks closing PROVIDER-REF-BOUND-1. The
security domain concern (withdrawal double-payout through an outbound reference) is `security`'s
condition C1, and I concur with it. I do not re-adjudicate it here.

## What I verified

- **`internal/providerref`.**
  - Length is `len()`, so it counts bytes. It is checked first, before any scan.
  - Then `utf8.ValidString`, then a rune loop with `r <= 0x1F || 0x7F <= r <= 0x9F`. That is C0 including
    NUL, DEL, and C1 by code point, so C1 in its 2-byte UTF-8 form (`\xC2\x80`..`\xC2\x9F`) is caught.
    A raw `\x80`..`\x9F` byte is invalid UTF-8 and is rejected one step earlier.
  - Minimum 1 is enforced by `Validate`. `ValidateOptional` treats "" as absent.
  - The error type holds no value. `Error()` and `LogAttrs()` carry the field, reason, length and a
    12-hex SHA-256 prefix only.
  - The unit tests cover: 255 and 256 bytes; 128 two-byte runes; a 3-byte rune straddling and fitting
    the boundary; five invalid-UTF-8 shapes; NUL, TAB, CR, LF, ESC, US, DEL, U+0080 and U+009F; U+00A0
    and multibyte text accepted; first-failure-wins; and no leak.
- **Go/SQL parity.** `octet_length(col) BETWEEN 1 AND 255 AND col !~ '[\x01-\x1F\x7F-\x9F]'` matches.
  - In a UTF8 database, a PostgreSQL ARE `\xhh` is a code point, so `\x7F-\x9F` equals Go's DEL+C1 range.
  - NUL cannot occur in `text`, so starting the SQL range at `\x01` is not a gap.
  - NULL passes on nullable columns. Invalid UTF-8 cannot reach the database.
  - Every writer that can see "" for a nullable column uses `NULLIF`, so no legitimate write turns ""
    into a CHECK violation: `RecordCallbackRejection` for round_id and asset_code, `BindProviderRound`
    for provider_session_id, and the KYC insert for provider_reference. The deposit decline path passes
    nil for an empty reference, not "".
- **Call sites.** I traced every path from provider input to a write.
  - **Casino webhook and play simulation.** Both reach `ReceiveVerifiedCallback`, where
    `validateCallbackReferences` runs after `HandleCallback` and before dispatch. The event's string
    fields are all covered. `DeclineReason` from a casino event is never persisted.
  - **Payments webhook and deposit simulation.** Both reach the single `ValidateAll`. Its only caller
    paths are `receiveDepositCallback` and `receiveDepositReversalCallback`.
  - **Handlers.** `ErrProviderReferenceInvalid` is not a `rejectionClassFor` class, so no rejection row
    is attempted. No earlier `errors.Is` branch in either handler swallows the error.
  - **Other writers.** `UpsertGame` covers provider_game_id; its provider_id is operator config, and the
    CHECK covers it. `SyncCatalogue` validates the whole tree before the first upsert. Settlement covers
    the claimed asset code. `sportsbook_bets.provider_bet_reference` has no writer.
  - **Remaining uncovered paths.** The only unvalidated paths to a bounded column are outbound adapter
    responses: `Deposit`, `Withdraw`, `QueryStatus` and KYC `CreateVerification`. These are §9.1/§9.2
    residuals plus security C1.
- **Migration 0099.**
  - There are 27 constraints across 20 tenant-table columns and 7 platform-table columns, and the
    pre-flight lists match the ALTERs one-to-one.
  - No migration seeds rows into these columns, so there is no legitimate existing data that fails.
  - The down migration drops all 27.
  - The pre-flight predicate is the exact negation of the CHECK. It counts "" as a violation, which is
    correct because the CHECK also rejects "".
  - Every tenant table's staff policy is satisfied by `app.tenant_id` plus an empty
    `app.player_account_id`. That covers ledger_transactions (0028), deposit_intents,
    withdrawal_requests, kyc_verifications, casino_launch_sessions, casino_provider_rounds,
    casino_callback_rejections and sportsbook_bets.
- **Pins and CI.**
  - bonus: 32 steps. operatingmarket 0076 and QA: 24. jurisdiction 0077: 23.
  - Tests that use a derived or staged directory need no change: 0075, 0092, 0098, 0048, providercred
    and sportsbook 0091.
  - The CI evidence list gains both 0099 migration tests.
- **Local runs.** I ran unit tests only, because the shared DB is busy. These pass:
  `./internal/providerref/`; the casino `ValidateCallbackReferences` test; the sportsbook
  `ValidateCatalogueReferences|ValidateSettlementEvent` tests; and the httpserver `MapReceiveCallbackError`
  test. `go vet -tags=integration` is clean on every touched package. I did not re-run the integration,
  migration or mutation evidence.
- **Mutation file.** It names parent commit 1560ad0, which matches `43f5071^`. There are 28 mutants,
  each with a named killing test that exists in the diff. The harness is not committed, so the evidence
  cannot be reproduced mechanically (see F6).

## Findings (most severe first)

### F1 (Medium, follow-up for ledger-finance): the latent `idempotency.Assign` external mode would violate the new ledger CHECK
`internal/idempotency/routing.go` `Assign(ModeExternalProvider, ...)` sets `ProviderTxID = composed`, where
`composed = "<len>:" + reference [+ "#" + discriminator]` (`compose.go`).

Failure scenario: a provider reference of 250 to 255 bytes, which is valid under providerref, composes to
255 to 263+ bytes. `ledger_transactions_provider_tx_id_ref_bound` then rejects the posting with 23514,
which surfaces as a generic 500. This is the exact failure class R-2 was closing.

There is no production importer today (`grep` finds no non-test import of `internal/idempotency`), so
this is latent, not live. It needs one of two things:
- the ADR 0038 §14 external-mode wiring validates the **composed** key, not the raw reference; or
- the bound on references that are composed is reduced by the composition overhead.

Record this against ADR 0095 or PRH-I1 so it is not rediscovered in production.

### F2 (Medium, concurrence with security C1, not re-adjudicated): 0099 adds a deterministic trigger for the withdrawal post-payout rollback
`withdrawal_handlers.go:838-875` calls `provider.Withdraw` inside the transaction. It then calls
`MarkSubmitted` to write `withdrawal_requests.provider_reference`, and `withdrawal.Complete` to write
`ledger_transactions.provider_tx_id`.

Before 0099, only a reference over about 2.7 KB failed here. Now a reference of 256 bytes or more, or one
containing a control character (a trailing `\n` from a sloppy adapter is enough), rolls the transaction
back after the money has moved.

Security §10 C1 already covers this, so I defer to `security` and `ledger-finance`. §9.1 still says
"fail closed", which is inaccurate for this path: the failure happens after an external side effect.

### F3 (Low): the DB test does not pin the upper edge of the SQL control-character range or the minimum length
`TestMigration0099_UpEnforcesBoundDownDropsItRoundTrip` checks the following at the DB level: 256 bytes,
128 two-byte runes, U+0085, and a 255-byte accept. The pre-flight test adds `\x01`.

Nothing asserts that U+00A0 is **accepted**, that DEL is rejected, or that "" is rejected by the CHECK.
A SQL mutant such as `[\x01-\x1F\x7F-\xA0]` or `[\x01-\x1F\x80-\x9F]` would survive every test. The Go
side cannot catch it either, because the two sides are independent literals. As a result, Go/SQL parity
is proven by reading the code, not by a test. Adding those three inserts would pin it.

### F4 (Low, adjacent; surface to security): the payments verified-callback `decline_reason` is unbounded and persisted
This is outside R-2's reference definition, and it has no index, so it cannot cause the btree failure.

Failure scenario: a verified sender puts up to about 1 MiB (the body cap) into `decline_reason`.
`receiveDepositCallback` calls `handleDecline`, which records it in `audit_log.metadata` twice:
`deposit.attempt_declined` and `deposit.declined`. The audit store is append-only and permanent. This is
the same amplification class the commit closes for references, and KYC already bounds its reason
(security S-5). The casino path does not persist `event.DeclineReason`.

### F5 (Low): OpenAPI `maxLength: 255` counts characters, not bytes
JSON Schema `maxLength` counts code points. A schema-validating client or gateway will therefore accept
255 multibyte characters (up to 1020 bytes) that the server rejects with 400. The descriptions do say
"BYTES", so this is not a silent contradiction. Two ways to make the machine-readable bound
conservative:
- state it as `maxLength: 63` (4 bytes × 63 ≤ 255) plus the byte rule; or
- keep 255 and add an `x-max-bytes: 255` extension.

### F6 (Low, evidence hygiene)
- The mutation harness is "a scratchpad script (not committed)". I can check the 28 mutant descriptions
  against the test names, but I cannot re-run them. Committing the harness, or the exact
  `sed`/replacement pairs, would make the evidence reproducible.
- **Comment drift.**
  - The 0099 header says "tenants has no RLS". Since 0077 it has FORCE RLS. The loop works only because
    `tenants_read` is `USING (true)`. If that policy is ever narrowed, the tenant loop silently iterates
    zero tenants. The migration then still fails closed through ADD CONSTRAINT (23514), but it loses the
    per-column report.
  - `wave3_phase2_migrations_integration_test.go` still says "thirty-one most recently applied".
  - In `jurisdiction/migration_0077_integration_test.go`, the new `const migration0099Version` was
    inserted between `TestMigration0077_EnablesAndForces...`'s doc comment and the function. That
    detaches the test's doc comment and attaches it to the const.

## Not findings (checked and correct)
- **Not truncating is correct.** Truncating would create idempotency-prefix collisions.
- **The length check runs first.** Oversize input is never scanned.
- **The rejection is a 400 after verification.** It is not a pre-auth oracle, and the redeem rolls back
  with the transaction.
- **No NOT VALID split.** This is justified by the runner's one-transaction-per-file model.
- **The casino rollback record is safe after 0099.** It writes `original_provider_tx_id` without
  `NULLIF`, but the empty-original case returns `ErrInvalidInput` before `wrapRejection` can classify it,
  so a "" original never reaches the INSERT.
