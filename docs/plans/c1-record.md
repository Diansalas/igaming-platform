# C1 record: BONUS-FIRST-DEPOSIT-DOUBLE-GRANT-1 (ledger-finance review condition C1)

Branch `c1-first-deposit` (from `claude/focused-wright-jw88w9`). Label: IMPLEMENTED (app rule + DB backstop 0173).

Verification (private DB): internal/bonus ok (38s), internal/db ok (50s), internal/casino + internal/httpserver `-run Bonus|Grant|Wager|Free` ok; gofmt clean, go vet plain and -tags integration clean.

## Finding
After NA-4's bounded late-commit re-scan, a late-committing EARLIER deposit also passed `isFirstDeposit` (it is the
earliest by `(posted_at, id)` once visible) and received a second FirstDepositOnly Grant for the same (tenant, player,
offer), although a Grant for the later deposit was already issued. Probe: two grants (both cancelled today by the
jurisdiction gate); would double-credit once activation is possible.

## Decisions
1. Application rule (`matchesDepositEligibility`, `internal/bonus/deposit_sweep.go`): for a FirstDepositOnly offer,
   not-eligible if ANY `bonus_grants` row exists for (tenant, player, offer), any offer version, any status
   (cancelled/forfeited/expired/reversed included), except a row whose `trigger_reference` is THIS deposit (idempotent
   replay, left to the existing ErrAlreadyGranted path). Function signature unchanged (uses `ov.OfferID`).
2. Semantics, no retroactive swap: the first deposit to be GRANTED holds the one FirstDepositOnly slot for
   (tenant, player, offer). A deposit that commits late with an earlier `posted_at` never replaces it and gets no
   Grant for that offer. Rationale: the issued Grant, its ledger postings and Progress trail are attributed to the deposit
   it was issued for; swapping would require reversing financial records. A cancelled Grant still consumes the slot
   (operator may intervene manually; the bonus is not auto-reoffered). Consequence: where a late-visible deposit was the
   true chronological first, the player received the bonus against their second-visible deposit; accepted and documented.
   Race-freedom for the sweep: it runs under the per-tenant advisory lock; the backstop covers everything else.
3. DB backstop, migration 0173 (`0173_bonus_first_deposit_single_grant_backstop`): a partial unique index needs a
   column on `bonus_grants`, so: `first_deposit_only BOOLEAN NOT NULL DEFAULT false`, DERIVED by a BEFORE INSERT trigger
   from the Grant's own offer version (a caller-supplied value is overwritten, never trusted), immutable thereafter
   (BEFORE UPDATE trigger), plus `UNIQUE INDEX bonus_grants_one_first_deposit_per_player_offer (tenant_id, offer_id,
   player_account_id) WHERE first_deposit_only`. Runtime inserts (`CreateGrant`) are unchanged. Status is irrelevant to
   the index. The index is narrower than the app rule on purpose: it only forbids two grants that are both
   FirstDepositOnly (a non-FDO v1 grant followed by an FDO v2 grant is blocked by the app rule, not by the index).
   Up pre-check: refuses with SQLSTATE `BG173` if any (tenant, offer, player) already holds more than one
   FirstDepositOnly grant (never deletes/rewrites financial rows); backfills existing FDO grants. RLS on `bonus_grants` and
   `bonus_offer_versions` is lifted for the owner only inside the migration transaction (0171 idiom) and restored.
   Down: drops the index, both triggers and functions, the column; no data loss (derived).
4. Migration-head pins: none exist in Go tests (grep for head literals found none; `reservedUnusedVersions` untouched,
   0173 contiguous after 0172). Docs that quote "migration head" are orchestrator-owned.

## Tests (written first; failing on pre-fix code)
Failing run before the fix: `TestRunDepositSweepForTenant_LateEarlierDepositDoesNotGetSecondFirstDepositOnlyGrant`:
"late earlier deposit A received a second FirstDepositOnly grant (grants=1, want 0)".
- `deposit_sweep_watermark_integration_test.go`: late earlier deposit (exactly one FDO grant, three re-sweeps = retry);
  same scenario non-FDO offer (both deposits still granted, 2 grants); six overlapping sweeps (only 23505 allowed as a
  loser error, exactly one grant after a retry).
- `first_deposit_backstop_integration_test.go`: direct SQL second insert refused with 23505 on the index name, also with
  a forged `first_deposit_only=false`, also after the first grant is cancelled; flag immutable; other player and
  non-FDO offer unconstrained; 8 concurrent inserts, exactly one wins; migration refuses BG173 on duplicate data and
  changes nothing; clean up (backfill) / down (0172 shape restored) / up (identical) round trip on scratch DBs.
No existing assertion was changed or weakened.

## Residuals
- A cancelled/forfeited FDO grant permanently consumes the slot (no auto re-offer); product may want an explicit
  operator "reset first-deposit eligibility" action. Not built (scope).
- Where a deposit was invisible (open transaction longer than the 15-minute lookback or the race itself), the granted
  deposit is the first VISIBLE one, not the chronologically first.
- Concurrent sweeps WITHOUT the advisory lock can still make the loser fail with 23505 (safe: transaction rolls back;
  production holds the lock).
