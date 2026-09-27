# RV-PRH-I5 — Security review of payment statement reconciliation (ADR 0095 §12)

- **Reviewer:** `security`.
- **Subject:** PRH-I5, commit `16c69b7` (on `claude/focused-wright-jw88w9`, HEAD `f6ded0a`), checked
  against ADR 0095 §12.1–§12.7, §13.3, S95-C1/C11, INV-IO-1/12/14, and ADR 0082 A7.
- **Verdict:** **APPROVE (no blocking finding), with two conditions (C1, C2) on the first non-MOCK
  statement source.** The conditions do not block PRH-I5 as delivered, because the only source is
  the MOCK and production refuses it. They **do block** registering any real PSP statement source,
  and therefore real-money launch with statement reconciliation. The label stays
  **IMPLEMENTED against a `MOCK` source; real PSP statement matching `PROVIDER DEPENDENT`**.
- **In scope:**
  - `internal/reconciliation/payment_statement.go`, the `scheduler.go` delta and
    `internal/reconciliation/statement/statement.go`.
  - `internal/payments/{mock_statement_source.go,gate.go,mock.go,contract.go}` deltas.
  - Migration 0102 (up and down), `deploy/init-app-role.sql` and the `cmd/platform-api` wiring.
- **Not in scope:** the correctness of the matcher's classification (owned by `ledger-finance` and
  `qa`), the ADR text itself, and the kill switch (migration 0103). I ran no penetration test or
  load test, and did not test the 1M-line cap at full scale.
- **Environment:** the private database `igaming_prh_i5` (migrated to 0102), used as the owner role
  `igaming` and the runtime role `igaming_runtime`. Targeted suites pass:
  - `go test -tags integration -run 'TestPaymentStatement|TestMigration0102' ./internal/reconciliation/`
  - unit tests for `./cmd/platform-api/`, `./internal/payments/` and `./internal/providerkind/`

## 1. Verified controls

| Control | Evidence |
|---|---|
| Fetch with no tx held (INV-IO-1) | `FetchPaymentStatement` returns `ErrPaymentFetchUnderTx` under `txscope.Held`, and `MockStatementSource.Fetch` refuses independently. `ReconcilePaymentStatementForTenant` calls fetch on the bare sweep ctx, before any `WithTenant`. Mutation M2 (below) was killed. |
| Fetch goes through the provider-call gate | `callProvider(..., ReadOnly: true, Domain: "payments")`, so the credential is resolved and its binding checked as for QueryStatus. Adapter errors reach the audit only through `redactedReason`. |
| Server-side tenant/provider only | `PaymentFetchRequest.TenantID` comes from the sweep's tenant list. `ProviderID` comes from the source itself, never from a statement. The MOCK filters records by `cc.TenantID` from the gate, and untagged (gate-less) records appear on no statement. |
| Cross-provider line refused (INV-IO-14 / S95-C1) | `validatePaymentLine` rejects the whole import if any line's provider differs from the source's (M1 killed). Matching is also provider-bound: attempts and ledger rows are loaded `WHERE tenant_id=$1 AND provider_id=$2`. |
| S95-C11 lengths and caps | Go side: `providerref.Validate` for provider references (255 B, no control characters), merchant ≤64, asset ≤16, source label ≤256, line cap checked before anything is stored. DB side: the matching CHECKs. Probed as runtime: 256-byte reference rejected, `E'a\nb'` reference rejected, `line_count=1000001` rejected. |
| `line_count` bound to stored rows | Probed as runtime. An over-count append (a second line into a one-line import) is refused by the statement trigger. A short import (declares 2, stores 0) is refused at COMMIT by the deferred constraint trigger. |
| Append-only store | Probed as `igaming_runtime`: UPDATE, DELETE and TRUNCATE all give `permission denied`. Probed as owner `igaming`: UPDATE gives `append-only: UPDATE is not permitted` and TRUNCATE gives `append-only: TRUNCATE is not permitted` (the triggers bind the owner). |
| Runtime grants | `information_schema.role_table_grants`: `igaming_runtime` has SELECT and INSERT only, on both tables. `init-app-role.sql` re-asserts this after its blanket backfill GRANT, and `TestInitAppRole_RerunKeepsPaymentStatementGrants` covers that. |
| FORCE RLS / cross-tenant | Both tables have `relrowsecurity` and `relforcerowsecurity` set. As runtime with tenant A's rows present: tenant B sees 0 imports and 0 lines. No tenant set: 0 rows. Player scope (`app.player_account_id` set): 0 rows. Inserting a row for tenant B under tenant A's context violates the RLS policy. The lines' composite FK `(import_id, tenant_id)` prevents attaching lines to another tenant's import. |
| MOCK label CHECK | Inserting `is_mock=true` with the label `synthetic` violates `payment_statement_imports_check`. On the Go side, a Synthetic source whose label lacks "MOCK" is refused before any I/O (`sneakyLabel` test). |
| MOCK refused in production | `payments/statement_source` is registered, and `RefuseSyntheticInProduction(cfg.GuardEnvironment())` names it (`registrations_test.go`). `GuardEnvironment` fails closed to `production` when the environment is not explicit. |
| Never writes ledger or balances (INV-IO-12) | The only DML in the stream is INSERT into the two statement tables (ingest tx) and `persistRun` plus `audit.Record` (match tx). No UPDATE or DELETE anywhere. `TestPaymentStatement_NoFinancialEffect` covers this. |
| Advisory lock vs ADR 0082 A7 | The match tx takes one non-blocking `pg_try_advisory_xact_lock('reconciliation:payment_statement:<tenant>')` and no L1–L4 row locks. A try-lock never waits, so it cannot join a wait cycle. This is consistent with ADR 0082 §4.8 (reconciliation stays a lock-free read) and adds no step to A7's R0/L1 order. Fetch and ingest take no advisory lock. Concurrent identical ingests converge through `ON CONFLICT DO NOTHING` followed by a re-SELECT. |
| Down migration refusal | Run as owner (in a tx, then rolled back) with one import stored: it raised `payment statement imports exist ... roll forward`. The guard uses constraint validation, which RLS cannot blind; the owner had no tenant set and would have seen `count(*)=0`. Lines cannot exist without an import (FK), so the import guard covers both tables. The pay_* mismatch guard is the second check. |
| Audit contents | Run audit: stream, status, counts, import_id, provider_id, label, is_mock, coverage, line count. Failure audit adds phase, severity and the error string. No credential, no secret, no player PII. `providerref.Error` renders field, reason, length and a sha256 prefix, never the value. Mismatch details carry references, amounts, assets and attempt/tx ids only. |
| `gate.go` one-line change | `fn(withCallContext(ctx, cc), cc)` puts the same `CallContext` the adapter already receives as an argument onto `ctx`, under an unexported key type. `OutboundCredential` holds only a handle id, key id and fingerprint (no secret field), and `CallContext`/`OutboundCredential` implement redacting `String`/`GoString`/`MarshalJSON`, so even `valueCtx.String()` renders the redacted form. The value does not outlive the call. **No new credential exposure.** Nothing routes on it; only the MockProvider reads it, to tag records. |

## 2. Mutation spot-check (2/2 killed; reverted, `git status` clean)

- **M1:** `validatePaymentLine` cross-provider check replaced with `if false && ...`. Killed by
  `TestPaymentStatement_CrossProviderLineNeverMatches`, which failed with "expected a cross-provider
  line to refuse the import, got <nil>".
- **M2:** `FetchPaymentStatement` `txscope.Held` guard replaced with `if false && ...`. Killed by
  `TestPaymentStatement_FetchRunsWithNoTransactionHeld`, which failed with "stream fetch under tx:
  expected ErrPaymentFetchUnderTx, got <nil>".

## 3. Findings

**F1 — Low (condition C1, blocks the first real source): the body-size cap is declared but not
enforced anywhere.** `statement.MaxPaymentStatementBodyBytes` (256 MiB) is a constant with no reader.
The only size control that actually runs is the line-count check, and it runs *after* `Fetch` has
returned a fully built `[]PaymentStatementLine` in memory. A real source that decodes an HTTP body
without a streaming limit can be pushed out of memory by a hostile or broken PSP response: an
unbounded body, or 10M lines. That takes down the platform-api process that also serves traffic,
because the reconciliation loop runs in-process. This does not apply to the MOCK, which has no wire
body.

**C1:** the first real `PaymentStatementSource` must:
- read through `io.LimitReader(body, MaxPaymentStatementBodyBytes+1)` and fail when it hits the limit;
- stop decoding at `MaxPaymentStatementLines+1`;
- ship a test for each of the two limits.

`security` verifies this in code when the source is proposed.

**F2 — Low (condition C2, blocks the first real source): `merchant_reference` and `asset_code`
accept control characters and are not registry-checked.** The Go and DB checks are length plus
UTF-8 only. Two failure scenarios:

- A PSP-echoed `merchant_reference` containing `\n`, ANSI escapes or CSV formula prefixes is
  copied verbatim into `reconciliation_mismatches` detail strings. Those strings are rendered in the
  back office and logs, which is a log/CSV-injection surface.
- A NUL byte passes Go validation but fails the Postgres text insert. That fails the ingest phase
  (fail-closed, audited as P1), so a provider can force a recurring P1 for every run.

**C2:** before a real source, apply the `providerref` no-control-character rule to
`merchant_reference`, add the matching DB CHECK in a forward migration, and validate `asset_code`
against the `Asset` registry, or at least `^[A-Z0-9]{1,16}$`.

**F3 — Info: `is_mock` is set by the application, not the database.** The MOCK label CHECK only fires
when `is_mock=true`. A runtime-role writer (a bug, not the stream) could store synthetic data with
`is_mock=false`. The authoritative control is the Go `Synthetic` marker plus the production guard.
The CHECK is defence in depth and does what it claims. No action needed.

**F4 — Info: the stream embeds `source.Fetch` errors in the audit verbatim.** Today the only source
routes through the gate (`redactedReason`), so this is safe. The interface does not force that,
though: a real source that bypassed the gate could put a raw response body or header in
`reconciliation.sweep_run_failed` metadata. This is already required by ADR 0095 §12.1 ("through the
§3.2 gate"). It is restated here as a review point for the real source, not a new condition.

**F5 — Info (disclosed by the author, restated): the MOCK is single-process, and its records are
in-memory and reset on restart.** Coverage starts at MockProvider construction. There is no security
impact, because it is refused in production.

## 4. Tests every real-source change must keep or add (authorization and tenant isolation)

- A tenant-B context reading tenant-A imports and lines returns 0 rows, never data. This already
  exists (`TestPaymentStatement_RLS_CrossTenant`) and must stay.
- A fetch for tenant A must resolve tenant A's outbound credential. A binding mismatch (a credential
  for B, or for another provider) must refuse the fetch through the gate and store nothing.
- A statement line naming another provider refuses the whole import. This exists and must stay.
- An oversized body and an over-cap line count each refuse the import with nothing stored (C1).

## 5. Launch relevance

C1 and C2 **block registering any non-MOCK payment statement source**, and so block real-money
launch with PSP statement reconciliation. They do not block PRH-I5 being marked done at its current
MOCK label. This review makes no claim of regulatory or PCI assurance.
