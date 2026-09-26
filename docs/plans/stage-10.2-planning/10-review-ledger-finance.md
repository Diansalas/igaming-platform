> Stage 10.2 final review — specialist working paper (verbatim, recorded 2026-09-26 against 11b2ad1). Orchestrator actions: conditions 1 and 2 applied in the same commit that records this paper (ADR 0019 concurrence set to GIVEN; CAS-CAP-ROLLBACK-1 widened and made a hard pre-condition). Condition 3 is satisfied by the Stage 10.2 completion report's integration evidence (local replay + GitHub CI; integration suites run as the NOBYPASSRLS runtime role).

# ledger-finance review: ADR 0019 Stage 10.2 amendment and casino callback change

## Verdict: CONCUR WITH CONDITIONS

I concur with the ADR 0019 Stage 10.2 amendment and with the financial side of the casino callback change. I found no ledger-invariant defect. Read-only; I did not run the `//go:build integration` suites (no DB available to me). `go test ./internal/casino/ ./internal/webhookauth/` passes.

## 1. The money path is unchanged: VERIFIED
- The only diff hunks in `internal/casino/orchestrator.go` are the import, the struct/constructor and `ReceiveCallback` (lines 19, 34, 549-690). `postBet` (:857), `postWin` (:1259), `postRollback` (:1350), `postRollbackTombstone` (:1553) and `verifyPostedBetMatchesEvent` have no diff.
- The diff does not touch `migrations/`, `internal/ledger/` or `internal/wallet/`; debits-equal-credits enforcement in `ledger.Post` and same-transaction projection writes are unchanged.
- Idempotency key: `migrations/0021_create_ledger_transactions.up.sql:44-46` partial unique index on `(tenant_id, provider_id, provider_tx_id)`. Posting inputs unchanged (bet `providerID+":"+ProviderTxID` :1089-1090; rollback :1506-1508; tombstone `tombstone:<provider>:<orig>` :1560-1561).
- F-7: `mapReplayPayloadMismatch` (:695-700) still wraps every post* result; handler maps `ErrProviderTxPayloadMismatch` to 409; `ErrAlreadyRolledBack` still 409.
- Tombstones: unseen-original path unchanged (:1368-1386, `FOR UPDATE` lookup then `postRollbackTombstone`); replayed-tombstone path unchanged (:1391-1402).
- Lock order (ADR 0082): unchanged. The one statement before post* is `LoadCapability` (:662), a plain `SELECT` without `FOR UPDATE` (`capability.go:19-28`), in the same position as before.

## 2. A rejected callback has no financial effect: VERIFIED
- Every pre-verification rejection returns `*webhookauth.AuthError` before any tenant-scoped statement: header parse (:610), adapter not registered (:618), nil resolver (:625), `Resolve` (:627), credential tenant/provider equality (:631), `HandleCallback` → `casinoScheme.Verify` (mock.go:417). Nothing touches `tx` until `LoadCapability` (:662). The handler runs inside `deps.DB.WithTenant` (casino_handlers.go:343); any error rolls back.
- A cross-tenant callback cannot verify: the resolver derives the key per (route tenant, provider) (`webhookauth/mock.go:52-63, 76-90`), and `Verify` rebuilds the signing input from `in.TenantID`/`in.ProviderID`, overwritten from the route at :603-604 (`webhookauth.go:283-299`). No tombstone, ledger transaction, entry or projection change in either tenant.
- Test evidence (not executed by me): `internal/httpserver/casino_prefix_e4_defect_test.go:29` (401, zero ledger_transactions incl. tombstones, zero audit_log in both tenants); `internal/casino/failure_mode_matrix_integration_test.go:1085` (cross-tenant rollback). E4 does not check projections directly — acceptable, since projections change only in the same transaction as a ledger entry.

## 3. The route provider_id is the verified value and feeds the key: VERIFIED
Same `providerID` flows through `in.ProviderID` (:604) → resolver key (:627; mock rejects any other provider, `webhookauth/mock.go:80`) → `cred.ProviderID` check (:631) → signing input (`webhookauth.go:291-294`) → post* arguments (:676-686) → idempotency and tombstone keys. No body field supplies provider_id (ADR 0019 refinement point 2).

## 4. The amendment text is accurate: VERIFIED
- "Casino conforms with a MOCK credential only": correct; mock resolver wired only with test support (`cmd/platform-api/wiring.go:109-114`), otherwise nil → 401.
- "KYC callbacks originate no LedgerTransaction": correct; nothing in `internal/kyc/` or `kyc_admin_handlers.go` references ledger/wallet tables.
- No KYC row in the actor matrix: agree with the category argument (the matrix governs posting origination; KYC blocking a withdrawal is enforcement).
- Domain prefixes separate credentials: correct (`webhookauth.go:248-260`, headers, mock labels); the "cryptographic" claim is rightly limited to the MOCK schemes.

## 5. CAS-CAP-ROLLBACK-1: analysis correct but too narrow; OK as a follow-up with a gate
Correct: a verified rollback with the capability disabled or `SupportsRollback=false` → `ErrProviderUnavailable` → 503 (:662-668, 682-684). Nothing is written, so the ledger stays balanced; the problem is economic (stake stays debited) and invisible to the ledger.

Missed by the registry entry:
- Win has the same problem (:677-679): winnings for a posted bet are withheld.
- Tombstone gap: an unseen-original rollback gets 503 and writes no tombstone. If provider retries run out while disabled, and a retried original bet arrives after re-enable, it posts unreversed — breaks CLAUDE.md's late-arrival guarantee for that window.
- Bad configuration can trigger it: `supports_rollback` defaults false with no CHECK tying it to `supports_bet` (`migrations/0035_create_casino_integration_foundation.up.sql:112`).
- Nothing would detect it: `internal/reconciliation/` has no casino provider reconciliation.

Acceptable outside 10.2 because the behaviour pre-exists (ADR 0025 review P1) and 10.2 does not change it; with no production resolver no casino money moves outside test support; no ledger invariant is violated.

Position for the follow-up: capability/status gates new exposure only (bets), never settlement of existing exposure (win or rollback for a posted bet). An unseen-original rollback always writes its tombstone, whatever the capability says.

## Conditions
1. Change PENDING to GIVEN in ADR 0019 (both places), pointing to this record.
2. Widen CAS-CAP-ROLLBACK-1 (win, tombstone gap, `supports_bet`/`supports_rollback` config gap, no casino reconciliation) and record it as a hard pre-condition for wiring any real casino resolver or going live with a real aggregator.
3. Stage 10.2 gate evidence must show the casino and httpserver integration suites passing against Postgres as the NOBYPASSRLS runtime role, including `replay_f7_integration_test.go`, `lockorder_integration_test.go`, `failure_mode_matrix_integration_test.go` and `casino_prefix_e4_defect_test.go`.

## Non-blocking observations
- The orchestrator trusts the adapter's `HandleCallback` to call `Verify` (mock.go:417). The mandatory tenant-binding conformance case for the first real adapter must be a hard failure, as the ADR says.
- A legacy `signature` body field is rejected after verification as `ErrCallbackSignatureInvalid` → 401 (mock.go:425); matches ADR 0025, no financial effect.
- Callbacks load the tenant-wide capability row (`brandID = uuid.Nil`, :662); a tenant with only brand-specific rows 503s every callback. Pre-existing; fold into CAS-CAP-ROLLBACK-1.
