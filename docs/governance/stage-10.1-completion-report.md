# Stage 10.1 Completion Report: PAY-REV-1, SB-T1-XMIN, PAY-WH-TENANT-1

- **Date:** 2026-09-26.
- **Authority:** ADR 0090, ACCEPTED on the human's approval of planning-gate commit `8561ac2`, with PAY-WH-TENANT-1 added by the human.
- **Branch:** `claude/focused-wright-jw88w9`.
- **Status:** all three workstreams **IMPLEMENTED**. PAY-WH-TENANT-1 is implemented with a **MOCK credential resolver only**.
- **Stopped** at the staging-deployment gate. No AWS action was taken.

Labels follow CLAUDE.md. The payments provider in this repository is a **MOCK**; no real PSP is integrated.

## Gates

| Gate | Result |
|---|---|
| G1 — three workstreams complete | PASSED |
| G2 — required tests, including concurrency and negative security tests | PASSED |
| G3 — full Go/CI suite | PASSED: CI #268–#279 green; the local fresh-database replay passed 3/3 integration runs plus reversibility |
| G4 — security, RLS and financial reviews | PASSED: every P2 closed and re-verified; residual P3s recorded (§10) |
| G5 — OpenAPI, documentation and ADR records | PASSED |
| G6 — no prohibited scope expansion | PASSED: KYC and casino webhook code untouched; no AI code; no AWS action |
| G7 / G8 — clean tree, pushed | PASSED: see §13–§14 |

## 1. PAY-REV-1 — IMPLEMENTED

**Defect:** two concurrent reversals of one deposit, with different references, both posted. This was reproduced before the fix: two `deposit_reversal` rows were posted and `player_cash` reached −1,000 (`evidence/pay-rev-1-defect-reproduction-prefix.txt`).

**Fix** (`e9e0ad8`, with later fixes in `3f67ac5`, `009c6d0` and `2704bdd`):
1. **Lock and re-check.** An ADR 0082 L2 `SELECT … FOR UPDATE` is taken on the original deposit's `ledger_transactions` row, with a tenant predicate. A missing row or a wrong type gives `ErrDepositReversalIntegrity`, never a tombstone. The already-reversed check is then re-run after the lock (READ COMMITTED).
2. **Migration 0092.** A tenant-leading partial unique index allows one `deposit_reversal` per original. It is created inside an RLS-proof `unique_violation` refusal: the index build itself detects any duplicates, with no `SELECT` pre-check.
3. **Ledger classification.** `db.IdempotentInsert` now reports which constraint was violated. `ledger.Post` looks up the idempotency key first, so a legitimate retry replays whichever index fires. It returns `ErrReversalAlreadyExists` only for a genuinely distinct second reversal.
4. **Rejection handling.**
   - The response is HTTP **409** with the generic body "callback rejected" (previously 500).
   - An alert line is written with only `provider_id`, `tenant_id` and `request_id`.
   - A `deposit.reversal_rejected` audit record is **committed in a separate transaction**, so it survives the rollback. It targets the deposit intent and names the original transaction, the rejected reference and the existing reversal (`DepositAlreadyReversedError`).

## 2. SB-T1-XMIN — IMPLEMENTED

**Defect:** trigger T-1 rejected a legitimate composed void whose rollback row had been written inside a savepoint. This was reproduced before the fix (`evidence/sb-t1-xmin-prefix-failure.txt`).

**Fix** (`e9e0ad8` and `2cb3600`): migration **0093** replaces only the function body. It adds a fail-closed `pg_xact_status` check; NULL and any error are rejected. The down migration restores 0091's body byte for byte.

**Deviation from the approved design (ruling R-2), ratified by `architect` and `ledger-finance`:** the plan anchored the epoch to `pg_snapshot_xmax`. That was shown to mis-place the transaction's own savepoint xids, which would have reproduced the bug. The epoch is anchored to `pg_current_xact_id()` instead.

The status check is proven load-bearing: replacing it with "accept" fails only the F4 test.

Residual cases, both documented in ADR 0088 §3.3:
- A transaction that straddles an epoch boundary is rejected, which fails closed. This is deferred as SB-T1-XMIN-STRADDLE (P3).
- A rollback row at least 2^32 xids old whose low bits collide with an in-progress xid can be accepted. This affects provenance only, has no ledger effect, and is the same class of issue 0091 had.

## 3. PAY-WH-TENANT-1 — IMPLEMENTED (MOCK resolver only)

**Defect:** a callback signed for tenant A was accepted at tenant B's URL and wrote a tombstone in tenant B. This was reproduced before the fix (`evidence/pay-wh-tenant-1-cross-tenant-prefix.txt`).

**Fix** (`250828b`, tests in `505a311` and `516bc0b`, later fixes in `3f67ac5`):
- **Credential selection.** The route's tenant only *selects* the single per-(tenant, provider) credential. The HMAC covers a v1 prefix, the route tenant, the provider, the key id and the raw body (`X-Payments-Signature`, `X-Payments-Key-Id`).
- **Order of checks.** Provider id, body size and header format are validated before the tenant lookup. The signature is verified before any parse, ledger/intent/wallet read, lock, write, tombstone or audit row. This is invariant I1, proven by a recording-transaction test.
- **Uniform rejection.** Every failure before verification returns the same **401** "callback rejected", and an allow-listed log line is written.
- **Status codes after verification:** a malformed body or provider mismatch returns **400**; an unknown intent returns **404**; an already-reversed deposit returns **409**.
- **Replay and idempotency** are unchanged.
- **Simulate route:** it signs only for the JWT's tenant.
- **Disabled provider capabilities** still accept callbacks. The flag controls routing only.
- **Credential resolver.** `WebhookCredentialResolver` has a MOCK per-tenant HMAC derivation from a per-process random master key; no secret is in the repository.
- **Not implemented:** the real resolver and secret store. **This remains launch-blocking for any real PSP.**
- **Records:** the ADR 0022 §3 amendment (callback contract points 1–7) and the ADR 0019 wording change (with `ledger-finance` concurrence).

## 4. Financial invariants

`ledger-finance` gave an unconditional sign-off (`stage-10.1-ledger-finance-signoff.md`, including its re-verification and the Orchestrator's note on the test fix).

| Required proof | Evidence |
|---|---|
| A. The same deposit cannot be reversed twice | L2 lock plus re-check, backed by the 0092 index; sequential, backstop and index-order tests |
| B. Concurrent reversals cannot both post | Test #1 (waits at the S2 `FOR UPDATE` with no projection locks held); defect-repro test (failed before the fix, passes after); `-race -count=10` |
| C. A legitimate retry is idempotent | F-7 replay suites plus the index-order test, which fails on the pre-fix `Post` |
| D. A different payload under the same key is rejected | `ErrIdempotencyPayloadMismatch` tests |
| E. A cross-tenant webhook has no financial effect | T2/T3/T14 six-point checklist; HTTP cross-tenant replay test; I1 recording test |
| F. A rejected reversal leaves no partial effect | Everything rolls back in one transaction; balances and counts unchanged |
| G. The rejection audit survives | Separate committed transaction, read back from a fresh transaction, all fields asserted |

**Also verified:**
- the ledger stays append-only, with no balance `UPDATE`;
- double-entry is unchanged and there is no floating point;
- the lock order is deterministic: L2 before L3/L4, and nothing is locked before signature verification;
- the `IdempotentInsert` change preserves behaviour at all five call sites.

## 5. Security and RLS

The security review (`stage-10.1-security-review.md`, including its re-verification) found no P0 or P1. **P2-1** (the 401 oracle), **P2-2** (audit content) and **P2-3** (missing tests) are all CLOSED.

**Verdicts:**
- SB-T1-XMIN: cleared.
- PAY-REV-1: cleared. The alert-field test landed in `2704bdd`.
- PAY-WH-TENANT-1: cleared for IMPLEMENTED, for the MOCK adapter only.

**RLS:**
- Tenant-scoped connections are used throughout.
- The migration refusals do not depend on RLS.
- No policy changes were made.
- The only read before verification is the tenant-scoped, read-only capability `EXISTS`.

**Accepted P3:** the key-material scan now runs after signature verification, as ADR 0022 point 7 requires, so an unsigned body is logged as `signature_invalid` rather than `key_material`.

## 6. OpenAPI

`docs/api/openapi/platform-api.yaml` now documents `POST /v1/webhooks/payments/{tenantSlug}/{providerID}`: headers, the mock body schema, and responses 200, 400 (after verification only), 401 (uniform), 404, 409, 500 and 503.

`TestOpenAPI_PaymentsWebhook_ContractMatchesHandler` is a structural check only. There is no YAML library in `go.sum` and no new dependency was added; this limitation is recorded in the testing strategy.

## 7. Tests

The binding plans are papers 06 and 16. Tests added:
- **PAY-REV-1:** tests #1–#12 plus the defect-repro, index-order, alert-field and denial-audit-content tests.
- **SB-T1-XMIN:** tests #13–#19 plus the F4 status-check test and the guard-pinned-to-trigger test.
- **PAY-WH-TENANT-1:**
  - tenant-binding tests T1–T15, plus T8a/b/c (replay, duplicate, concurrent duplicate at orchestrator and HTTP level);
  - T11a (no write, lock, batch or copy before verification);
  - T12 (log allow-list for every auth-failure reason, including body too large, with zero audit rows);
  - T13 (simulate route uses only the JWT's tenant);
  - post-verification 400 tests;
  - an ADR 0022 §6 conformance case;
  - redaction tests (`GoString` and `LogValue`);
  - about 80 existing call sites migrated, each changed status re-verified individually.

**Pre-fix evidence:** three files in `docs/plans/stage-10.1-planning/evidence/`.

**Non-vacuity:** the code reviewer reverted or weakened each fix in a scratch copy and confirmed the corresponding test fails. The index-order test was corrected after review because it originally passed on the old code.

## 8. CI

| Run | Commit | Contents | Result |
|---|---|---|---|
| #268 | `e9e0ad8` | PAY-REV-1 and SB-T1-XMIN | green |
| #273–#279 | `909d75b` → `2ec3026` | review fixes, test fixes, records | all green |
| #280 | `516bc0b` | P3 tests | see hand-off message |
| — | the commit carrying this report | report | see hand-off message |

**Local replay** (`scratchpad/ci-local.sh`, fresh database, at `009c6d0`): gofmt, vet and lint clean (0 issues); migrate up to 93 and verify clean; unit tests with the race detector; integration tests 3/3 with 32/32 packages each; reversibility 93→90→93 clean.

## 9. Specialist reviews

All reviews, papers and rulings are recorded in the repository.

**Planning gate:** papers 01–10.

**PAY-WH-TENANT-1 design:** paper 11 with Orchestrator rulings 1–12; reviews 13–16; KYC verification 12.

**Final implementation reviews:**

| Reviewer | Verdict | Where recorded |
|---|---|---|
| Security | Approved with changes, then re-verified: all P2 closed | `stage-10.1-security-review.md` |
| Ledger-finance | Conditional, then unconditional after the P2-A test fix | `stage-10.1-ledger-finance-signoff.md` |
| Architecture | XMIN deviation accepted; PW-1…4 and records R-1…R-8 applied | `stage-10.1-architecture-review.md` |
| Code review | F1–F5 and N1 addressed, then re-verified | `stage-10.1-code-review.md` |

**Disagreements and Orchestrator rulings:**
- **R-2 epoch anchor.** The plan (security, architect, ledger-finance) specified `pg_snapshot_xmax`; the implementer produced empirical counter-evidence. Ruling: the `pg_current_xact_id()` anchor stands, ratified by both owners.
- **Key-material scan order.** Ruling C5 said to scan before verification; point 7 says never to parse before verification. Ruling: point 7 wins, and security accepted it (P3-7).
- **Resolver shape.** Ruling C2 asked for a single resolver; the code uses `MultiWebhookCredentialResolver`. Ruling: accepted as a single injected interface. The composite is recorded as mock/test wiring only.

## 10. Deferred items

- **SB-T1-XMIN-STRADDLE (P3):** handle a transaction that straddles an epoch boundary (epoch+1).
- **0092 refusal message:** Postgres `DETAIL` is not included. The migration is checksum-immutable, so this is recorded in ADR 0020.
- **PAYWH-BRAND-1:** the webhook capability check has no brand scoping.
- **PAYWH-RL-1:** webhook rate limiting.
- **PAYWH-TS-1:** signed-timestamp replay window.
- **Real `WebhookCredentialResolver` and secret store:** future work, requiring human authorization; ADR 0009 pre-production confirmations apply.
- **LEDGER-REV-UNIQ** (cross-type reversal uniqueness) and **REV-UNIQ-CASINO**.
- **Single-column `reverses_transaction_id` foreign key**, which is not tenant-scoped.
- **P3 polish:**
  - `main.go` still wires the composite resolver;
  - one error branch bypasses the shared mapper;
  - the C4 conformance case skips for non-mock adapters until the first real adapter;
  - other items listed in the code-review re-verification.
- **Carried from Stage 10:** OI-5 named debt, the L0.6 residual, money width, reconciliation scale, the ALB security-group description, and integration-tagged lint.

## 11. Remaining human decisions

1. **KYC-WH-1 (High, pre-existing, affects running staging `9190d5d`):** the KYC webhook secret is a literal constant committed to the repository, and the mock KYC route has no environment gate. A player can forge an "approved" KYC callback. No payment path consults KYC status today, so the harm is currently a false compliance record, but it is launch-blocking. **Scope ruling needed.**
2. **CAS-WH-TENANT-1 (Medium):** the casino webhook has the same tenant-binding flaw. Scope ruling needed.
3. **Staging deployment of Stage 10.1:** needs authorization; plan in §15.
4. **Still OPEN, not reopened:**
   - ADR 0009 pre-production confirmations
   - HDR-J-6, HDR-J-7, HDR-J-8, HDR-J-9
   - HDR-M-1, HDR-M-2
   - HDR-SB-1
   - OB-1
   - vendor, legal and retail decisions
   - ADR 0089 §8 future decisions (model/provider selection, automated-targeting restrictions, vulnerable-player status)

## 12. AI architecture status

ADR 0089 (future AI-agent boundary) remains a binding future requirement and is **NOT IMPLEMENTED**. Stage 10.1 added no AI code, dependency, model provider or infrastructure. Its registry entry is AI-ARCH-FUTURE.

## 13. Final commit

The commit that carries this report sits on top of `516bc0b`. Its SHA is given in the hand-off message and can be verified with `git log origin/claude/focused-wright-jw88w9 -1`.

## 14. Push verification

Local `HEAD` and `origin/claude/focused-wright-jw88w9` are compared after the final push and reported in the hand-off message. The working tree is clean.

## 15. AWS/staging confirmation and staging deployment plan

**AWS and staging were not touched.**
- No `deploy.sh`, Terraform, ECS, RDS, IAM, CloudFront or network action was taken.
- Staging is still running `9190d5d`, at migration 0090.
- All development and testing used the local synthetic PostgreSQL.

**Proposed staging deployment of Stage 10.1.** This needs separate human authorization, and the human executes it from the approved network.

1. **Pre-conditions.**
   - The Stage 10.1 head is green in CI.
   - The human has ruled on KYC-WH-1. Redeploying ships the same KYC flaw unless it is contained first; staging is IP-allowlisted.
   - The approved deployer credential and allowlisted CIDR are available.
2. **Lifecycle constraint.** Per `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md` §3, `deploy.sh up` refuses to deploy a different commit onto a running environment ("one commit per environment lifetime"). Moving staging from `9190d5d` to the Stage 10.1 head therefore requires `deploy.sh down` then `deploy.sh up`.
   - This recreates the RDS database. All staging data is synthetic.
   - Because the database is fresh, migration 0092 cannot meet a pre-existing duplicate reversal.
3. **Run.** `deploy/aws/scripts/deploy.sh down`, then verify teardown (runbook §6). Then `deploy/aws/scripts/deploy.sh up` at the Stage 10.1 head. This covers phases 1–5, including `role-init` then `migrate`, which applies 0091–0093 and the runtime-role REVOKEs.
4. **Acceptance**, runbook §4 plus:
   - `/readyz` returns 200 from the allowlisted network and 403 from outside it;
   - the deposit, reversal and 409 flow works via the simulate route;
   - a payments webhook signed for a different tenant returns 401;
   - the sportsbook settle/void/rollback simulation works;
   - the Back Office bet lifecycle view works;
   - CloudWatch logs contain no key material or references.
5. **If staging must keep its current data:** there is no supported in-place path. Keep `9190d5d` running and defer the redeploy.

## 16. Next stage recommendation

**Stage 10.2 — webhook trust hardening.** This needs a planning gate and human authorization; nothing has been started.
1. **KYC-WH-1 (High):** remove the committed secret, gate the mock KYC provider and route behind test-support, stop exposing `provider_reference` to players, and bind the KYC webhook to the tenant under the ADR 0022 §3 contract.
2. **CAS-WH-TENANT-1 (Medium):** tenant-bind the casino webhook under the same contract.
3. Optionally, **PAYWH-TS-1** (signed-timestamp replay window) and **PAYWH-RL-1** (rate limiting).

The Stage 10.1 staging redeploy (§15) can be authorized independently of Stage 10.2.
