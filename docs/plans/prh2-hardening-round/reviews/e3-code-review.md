# Code review — PRH-2 E3 SB-CATALOGUE-IO-1 (2026-09-28)

Reviewer: `code-reviewer`. Commit reviewed: `33213c7` (branch `sb-catalogue-io-1`, base `cabca27`). Nothing in the repo was changed; tests ran on an exported scratch copy against a private DB, which has been dropped.

**Verdict: READY WITH CONDITIONS.** The change is correct:
- No provider call is made while a connection or transaction is held.
- Startup still fails closed (confirmed by reading; no test runs `run()`).
- Validation is unchanged.
- Nothing still uses the old signature.
- Build, vet, gofmt and the pinned lint are clean (0 issues). `-race -tags integration` passes for `internal/sportsbook` and `cmd/platform-api`.
- The author's two mutants are killed.

Of the reviewer's 8 extra mutants, 4 survive.

| # | Severity | Finding | Recommendation |
|---|---|---|---|
| F1 | Medium (condition) | Nothing protects the production call site in `main.go`. The new tests exercise a test-file copy of the sequence, and the IO-1C static guard scans only `internal/{casino,kyc,payments}`. Mutant **X5b** (fetch moved back inside the `WithPlatformService` closure, but called with the outer, unmarked ctx) survives, and would reintroduce exactly the bug SB-CATALOGUE-IO-1 fixes. | Add `internal/sportsbook` and `cmd/platform-api` to the IO-1C `dirs`, and `FetchCatalogue` to `ioc1FlaggedMethods`. A scratch probe on today's tree finds 0 violations and flags X5b at `main.go:261`. |
| F2 | Low (condition) | `FetchCatalogue`'s own validation is untested: mutant **X2** (validation removed from the fetch) survives, because `SyncCatalogue` re-checks inside the transaction. | A pure unit test: an over-bound catalogue gives `ErrProviderReferenceInvalid`, and no transaction is opened. |
| F3 | Low (condition) | `SyncCatalogue`'s re-validation is untested: mutant **X3** survives. | An integration test calling `SyncCatalogue` directly with an over-bound result: `ErrProviderReferenceInvalid` and 0 rows. |
| F4 | Low | Startup fail-closed is enforced only by the code as written: mutant **X6b** (fetch error ignored) survives. This predates E3. | Not blocking; revisit if a startup-test harness ever exists. |
| F5 | Low | `ErrCatalogueFetchRefused` wraps both the tx-held refusal (a programming bug) and ordinary provider errors (an outage), unlike casino's `ErrProviderCallRefused`, which means tx-held only. | Keep the sentinel for tx-held only, and wrap provider errors with plain context. Correct the "identical" wording. |
| F6 | Info (forward condition) | The fetch has no deadline (it uses the root signal ctx). | For the first real sportsbook adapter: a bounded timeout. |
| F7 | Info | ADR 0095 §33 omits the AST allow-list change; "ONLY sanctioned caller" is a convention rather than enforced (F1); and the no-transaction claim is untested (F2). The registry and HANDOVER rows need updating at merge. | Add one line to §33; update the rows at merge. |

**Architecture/security opinion:** consistent with ADR 0094 INV-POOL and ADR 0095 D1, using the same three-layer pattern as payments, casino and KYC (API shape, runtime refusal, separate write transaction). No new risk of significance: this path handles no money, tenant data or secrets. Close F1 and F6 before the first real sportsbook adapter.
