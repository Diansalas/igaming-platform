> Gate 10.3-W1 — `code-reviewer` working paper (recorded 2026-09-26 against `4a2a978..ee2192f`; condensed from the reviewer's report). Dispositions: `05-gate-log.md`, gate 10.3-W1.

# Gate 10.3-W1 independent code review

**Verdict: NOT READY — changes required, no rewrite.** No confirmed correctness bug in the money path or tenant
isolation; the merged control flow is right (casino: verify → parse → dispatch; capability gate only in `postBet`
against `session.BrandID`; payments keeps `ProviderAcceptsWebhook` as its only pre-verification statement; KYC none;
tenant binding holds; G-1 `rows[0]` safe; one L0.1 per transaction, always first; every mock with `WebhookScheme()`
also carries `SyntheticComponent()`; webhookauthtest proportionate to the mandate; panic on bad declaration acceptable).
Ran `go vet` (±integration) and unit tests only.

| # | Sev | Finding | Fix |
|---|---|---|---|
| 1 | HIGH until committed | `TestMigration0094_UpDownUpRoundTrip` fails at committed HEAD once 0095 is the tip | commit the pin fix (`migration0094DirThroughSelf`) — **done in `5d2c997`, full integration green** |
| 2 | MEDIUM | W1b claim false: mock webhook credential resolvers (`main.go:183,219,332`) built outside `providerBundle`, never registered; gated by `TestSupportRoutesEnabled()` (reads `Environment`, not `GuardEnvironment()`); completeness test checks bundle vs registrations only, and its negative control re-implements the reflection loop | build resolvers in the bundle and register them; one shared completeness helper used by both tests |
| 3 | MEDIUM | W1c guards untested: `postBet` S-nobet branch, `supported_assets` narrowing, `LaunchGame` real-mode `!SupportsBet` | tests for each |
| 4 | MEDIUM | W1d normalization relies on each adapter; platform write sites (`verification_service.go:112`, `provider.go:398,424,438`) store adapter `Reason` as given; audit metadata has no DB backstop; truncation flag discarded | apply idempotent `NormalizeReason` at platform write sites; record truncation flag (identity-compliance condition 1) |
| 5 | MEDIUM | ADR 0022 overclaims a `CallbackFixture` hook (line 629); non-mock adapters `t.Fatalf` in casino/payments conformance, so no real adapter can ever pass without editing the suite | add an optional fixture interface or amend the ADR to NOT IMPLEMENTED |
| 6 | LOW–MED | a production-eligible adapter can register the platform MOCK scheme | mark `mockVerificationScheme` synthetic; register each adapter's `WebhookScheme()` in `buildRegistrations` |
| 7 | LOW–MED | ADR claims not matching code: 0085:180 (AST scan uses a name heuristic; doc says exported-or-unexported but skips unexported); 0025 item 8 (no free-round/jackpot conformance case); 0022:621 (registry run iterates the static manifest); 0022:624 vs lint on `crypto/subtle`; 0082:1485 (late bet is a 200 decline, not `ErrOriginalTombstoned`); 0082 A6 (50 iterations + waiter assertion vs 20, outcomes only) | correct text or code |
| 8 | LOW | `make run` no longer starts: missing APP_ENV is treated as production and refused | `APP_ENV ?= development` in the target, or document |
| 9 | LOW | dead code: `casino/payments.ParseWebhookAuthHeaders`, package-level `payments.WebhookScheme()`, `casino.WebhookScheme()` (test-only), `Scheme.CheckPreamble` (test-only), `Orchestrator.now` never set, `txs` map in `classifyDirectOriginRows` | delete |
| 10 | LOW | a manifest-listed `KeyImplicit` scheme registers then 401s every callback (resolution NOT IMPLEMENTED until W2a) | refuse `KeyImplicit` at registration until W2a |
| 11 | LOW | OpenAPI casino webhook description says "503 has exactly ONE cause" then lists two | fix text |
| 12 | LOW | weak assertions: KYC tenant-isolation test skips the zero-row check silently if the body is not a JSON array and never asserts status; KYC reason self-test uses a raw string, not a broken fixture adapter; G-1 "409" test asserts only 400 | strengthen |
| 13 | INFO | tombstone correlation fallback `NewSHA1(idempotencyKey)` is not tenant-qualified | include tenant id |
| 14 | INFO | status labels stale (active-stage "nothing implemented", registry "W1a pending", amendments `NOT IMPLEMENTED`) | update at gate close; keep real vendors PROVIDER DEPENDENT, KeyImplicit NOT IMPLEMENTED; disclose that the all-mock bundle means a production binary refuses to start |
