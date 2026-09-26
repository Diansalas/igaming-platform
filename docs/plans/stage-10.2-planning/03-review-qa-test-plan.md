> Stage 10.2 design review — specialist working paper (verbatim, recorded 2026-09-26 against 69e80c1). Where it differs from the Orchestrator rulings in `01-webhook-trust-design.md` §J, the rulings govern.

# Stage 10.2 QA Test Plan Review — Binding (per ADR 0091 + design §H)

Verdict on design doc §H: coverage is adequate and binding as written. Below
maps every required case to exact names/packages, adds the protocol gaps
the design leaves implicit, and flags two risks to close before code review.

## Pre-fix evidence protocol (run once, at `d76bdd3`, before any fix)
- Branch/worktree pinned to `d76bdd3`. Never rebase these test files onto post-fix code.
- Files: `internal/kyc/prefix_evidence_test.go` (E1–E3), `internal/casino/prefix_evidence_test.go` (E4).
  Build tag `//go:build prefix_evidence` so they never run in normal CI post-fix.
- Output: raw `go test -run ... -v` stdout redirected to
  `docs/plans/stage-10.2-planning/evidence/E1-kyc-self-approval.txt`,
  `.../E2-kyc-secret-is-const.txt`, `.../E3-kyc-ungated.txt`,
  `.../E4-casino-cross-tenant-tombstone.txt`. Each file prefixed with commit SHA,
  date, command line. **grep each output file for the literal secret bytes before
  committing it — CI step, not manual.**
- E2 (`TestKYCWH1_PreFix_SecretIsCompileTimeConstant`): uses `go/ast` to assert the
  argument to `kyc.NewMockKYCProvider` at `cmd/platform-api/main.go` is a
  `*ast.BasicLit` (STRING kind) — asserts **kind only**. Test body and its `-v`
  output must never call `t.Log`/`fmt.Print` on the literal's `.Value`; assert
  `lit.Kind == token.STRING` and stop. Evidence-capture step re-runs with
  `-v 2>&1 | grep -c` a placeholder marker, not the value, to prove non-print.
- After capture: E1/E3/E4 are retired (deleted) once their fixed-behavior
  counterparts (K1, K11-TS-off, C1) exist and pass. E2 is **inverted**, not
  deleted, into `TestG7_NoConstStringFeedsWebhookCredential` (kept permanently
  as a G7 guard) asserting `NewMockKYCProvider` takes 0 arguments.

## Package layout
- `internal/webhookauth/*_test.go` — P2 (extraction unit tests).
- `internal/kyc/*_test.go`, `internal/kyc/mock_test.go` — K-series unit/mock tests.
- `cmd/platform-api/kyc_webhook_integration_test.go` (new) — K1–K12 integration.
- `cmd/platform-api/kyc_flow_integration_test.go` (existing, migrated) — K15.
- `internal/casino/*_test.go`, `casino/conformance_test.go` — C-series unit + C12.
- `cmd/platform-api/casino_webhook_integration_test.go` (new) — C1–C11.
- `cmd/platform-api/casino_flow_integration_test.go` (existing, migrated) — C13.
- `cmd/platform-api/openapi_kycwebhook_contract_test.go`, `openapi_casinowebhook_contract_test.go` — K14, C14.
- `cmd/platform-api/payments_webhook_test.go` (unchanged) + `openapi_paymentswebhook_contract_test.go` — P1.

## ADR 0091 required-case → test-name map
| ADR 0091 requirement | Test(s) |
|---|---|
| Valid tenant webhook | K2 (`TestKYCWebhook_ValidSameTenant_Approves`), C2 (`TestCasinoWebhook_ValidSameTenant_BetWinRollback`) |
| Wrong tenant | K3/K4 (`TestKYCWebhook_CrossTenant_Rejected`, `_EqualSecretResolver_Rejected`), C3/C4 (analogous) |
| Invalid signature | K5/K6, C5/C6 tamper matrices |
| Modified payload | K5 field-tamper subcases, C5 field-tamper subcases |
| Replay | K8 (`TestKYCWebhook_Replay_NoOp`), C8 (`TestCasinoWebhook_DuplicateProviderTxID_Idempotent`) |
| Player self-approval | K1 (`TestKYCWebhook_PlayerComputedSignature_Rejected`, direct descendant of E1) |
| Provider-reference tampering | K10 (`TestKYCPlayerResponse_NoProviderReference`) + K1 covers the forge path itself |
| Production environment | K11 `mockProviderWiring` prod-matrix subtests; C10 |
| Test-support disabled | K11 TS-off subtests (KYC 404, casino nil-resolver 401); C10 |
| Tenant isolation | K3/K4/K7 (statement-capture proof), C3/C4/C7 |
| RLS | K7/C7 recording-transaction proof (I1); K3 "no read of B's row" assertion |
| Audit | K2/K8/K9-error audit-row assertions; no-audit-row assertions in every rejection test via the six-point checklist |
| No financial/account-state effect on rejection | Six-point checklist (below) run inside K1,K3-K7,K9(400 case),K11,C1,C3-C7,C9,C10 |
| Casino list (C1–C14) | All present per design §H, see package layout above |

## Six-point no-effect checklist (ledger-adapted per testing-strategy.md, KYC-adapted)
Run in a fresh transaction after every rejection-path test:
1. no new/changed `kyc_verifications` rows (KYC) / no new `ledger_transactions` (casino);
2. no `ledger_entries` or casino projection changes;
3. no tombstones (either tenant);
4. no `audit_log` rows;
5. SUM(debits)==SUM(credits) unchanged;
6. projections unchanged.
Implement as one shared helper `assertNoEffect(t, tx, before, after Snapshot)` in
`internal/testsupport/noeffect` — do not duplicate six ad hoc assertions per test.

## I1 pre-verification proof (recording transaction)
`internal/testsupport/recordingtx` wraps the DB handle used inside
`webhookPreamble`→`Verify` and panics/fails the test if any statement,
savepoint, batch, or COPY is issued before `Scheme.Verify` returns success.
Wired into K7 and C7 as the primary mechanism, not a side assertion — a
mutation that moves `WithTenant`'s tenant-scoped read before `Verify` must
fail K7/C7, not just look wrong on review.

## Mutation-killability
Every K/C test whose purpose is "verify happens before X" or "signature
required" must be shown to go red when the corresponding guard is removed
(mutate: drop `Scheme.Verify` call, swap `tenant_id` predicate for none,
remove monotonic rank check). Record kill-matrix in
`docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md` follow-up
entry before sign-off, per Stage 10 W0 rule.

## Payments regression guarantee (extraction must not touch payments)
- P1: full existing payments webhook suite + the 17-test plan (paper 16) +
  `openapi_paymentswebhook_contract_test.go` run **unmodified except import
  paths**, green, on the extraction commit — verified by `git diff --stat`
  showing zero non-import hunks in payments test files.
- P2: new `internal/webhookauth` unit tests (framing, `ParseHeaders` per
  scheme, cross-scheme rejection under equal key, `MockResolver` foreign
  provider/key rejection, redaction) — these are new, not migrated.
- No test may assert on error text (`errors.Is/As` only) — grep-checked in CI.

## Existing tests to migrate (call-site counts, from design §A/C1/C2)
- KYC: 4 literal call sites drop the secret param — `internal/kyc/mock_test.go:59,74,83,96`,
  plus `kyc_integration_test.go:92`, `kyc_flow_integration_test.go:39` (6 total).
  `kyc_flow_integration_test.go:706-760,815-835` rewritten to
  `CallbackPayload(tenant.ID,…)` (K15).
- Casino: ~91 `casino.NewOrchestrator` call sites (resolver param added) +
  ~140 `CallbackPayload` sites + 3 play-handler sites (C2 design). Status-code
  assertions at `casino_flow_integration_test.go:291,315,338,387-395`
  (400/404→401) — 4 sites (C13).
- **Re-verification rule (K15/C13):** each changed assertion is re-run
  individually (`go test -run '^TestName$/^SubtestName$' -v`) and its diff
  reviewed line-by-line — mechanical `sed` replacement across 91+140+4+6
  sites is not itself proof; a sampled minimum of 10% of casino sites plus
  100% of the 4 status-code sites get individual manual re-verification
  logged in the PR description.

## Sign-off gate (qa authority)
Not `IMPLEMENTED` until: all E1–E4 evidence captured pre-fix without printing
the secret; K1–K15, C1–C14, P1–P3 green; six-point checklist wired into every
rejection test; I1 recording-tx proof passes; mutation-kill matrix recorded;
migrated call sites individually re-verified per above. Any gap reported to
orchestrator as `PARTIALLY IMPLEMENTED`, not silently narrowed.
