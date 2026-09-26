# Stage 10.3 planning gate — QA review and binding test plan

- **Type:** planning review only. No code, migration, `deploy/`, AWS or Terraform action. Nothing
  committed beyond this file.
- **Reviewer:** `qa`.
- **Reviewed:** `docs/plans/stage-10.3-planning-gate-proposal.md` (all sections; §6, §15, §20 read in
  full), `00-roadmap-reconciliation.md`, `01-provider-trust-analysis.md`, `02-casino-financial-
  analysis.md`, `03-kyc-reason-bound-analysis.md`, `docs/testing/testing-strategy.md` (baseline
  requirement, CLAUDE.md financial list restatement, Stage 10.1/10.2 "Results" and rules sections),
  `CLAUDE.md`, `.github/workflows/ci.yml`, `docs/plans/stage-10.2-planning/02-ci-flake-281-
  investigation.md`.
- **Repository state:** branch `claude/focused-wright-jw88w9`, HEAD `ff30d87`.

This document is the binding test plan for Stage 10.3, per wave. It also records where the proposal's
own §15 falls short of `CLAUDE.md`'s financial test list, what CI changes are needed, and the QA
disposition on CI-FLAKE-281.

---

## 1. Overall assessment

The proposal is unusually well-evidenced for a planning document: every item cites the exact line
where current behaviour was verified, every migration names its pre-flight and down path, and the
specialist papers (00–03) each carry their own detailed test sections (§1.11, §2.15 in the casino
paper; the Tests section in the KYC paper). It correctly inherits the binding rules `qa` set in Stage
10.1/10.2 (regression-must-fail-first, mutation-kill-on-the-assertion, statement-capture must be
structural, no-effect checks from a fresh transaction on both tenants) and does not attempt to weaken
any of them.

Two structural gaps keep this from a clean APPROVE:

1. **§15 is a summary, not a floor.** It is a compressed matrix; the actual CLAUDE.md 12-item
   financial list is only demonstrably complete by reading the casino paper's §1.11/§2.15 and the KYC
   paper's Tests section, not §15 itself. One list-item — **idempotency under concurrency** (item 10,
   distinct from ordinary duplicate-replay and from ordinary concurrency) — is missing as a named case
   from every wave that posts money (W1c, and by inheritance G-1). This is called out below wave by
   wave and is condition 1.
2. **CI-FLAKE-281's likely cause is not addressed, and 10.3 makes it more likely to recur**, because
   the new admin/casino/reconciliation integration tests add load to the same CI job that is already
   running the Argon2-heavy Stage 9 `internal/httpserver` tests at a measured 74% of their fixed 30s
   ceiling under artificial stress (§6 below).

Neither gap requires re-scoping the proposal; both are closeable inside the wave structure already
proposed. See §8 for the verdict and numbered conditions.

---

## 2. §15 versus the CLAUDE.md financial test list — completeness check

CLAUDE.md's list: (1) normal, (2) duplicates/idempotency, (3) concurrency, (4) retries, (5) partial
failure, (6) rollback (tombstone + compensating entry), (7) settlement, (8) reconciliation, (9)
provider callbacks, (10) idempotency **under concurrency**, (11) authorization, (12) auditability.

| Wave | Touches a posting/settlement path? | Where the full list actually lives | Gaps found |
|---|---|---|---|
| W1a (scheme) | No (auth gate only, no posting) | N/A — full matrix not required; conformance SC1–SC13 is the correct floor for this wave | None for the financial list; see §4.1 for what is required instead |
| W1b (synthetic guard) | No | N/A | None |
| W1c (capability contract + G-1) | **Yes** | `02-casino-financial-analysis.md` §1.11, §3 (G-1 tests) | **Item 10 missing**: every listed concurrency case is a *distinct*-reference race (late original vs. its rollback, two rollbacks of one unseen original, disable-vs-bet). None is "the identical callback (same `provider_tx_id`) delivered twice concurrently" for bet, win, **or** rollback. Ordinary sequential replay (item 2) is covered; the concurrent case is not. Same gap for G-1's win path (concurrent identical win callback). |
| W1d (KYC reason bound) | No (paper states "Financial impact: None", and this is correct — `kyc_verifications` does not gate a financial write in this codebase today) | Full matrix not required. Baseline (unit + integration + authorization + tenant isolation, per `testing-strategy.md` §"Baseline requirement") applies instead | **Authorization/tenant-isolation cases not named.** The paper's Tests section has unit, mock-provider, API-contract and conformance cases, but no case asserting (a) the player-facing response never carries the raw `reason` string under any adapter/role, and (b) tenant B cannot read tenant A's `reason_code`/`reason` (RLS is inherited from the existing table, but the new column needs its own assertion, not an assumption). |
| W2a (credential resolver) | Indirectly (gates every financial callback) | `01-provider-trust-analysis.md` §2.3/§2.4 | Concurrency (cache/revoke races) is present. Fail-closed matrix (§2.2) doubles as the retry/partial-failure coverage. No gap found. |
| W2b (casino_consistency) | Read-only w.r.t. money | `02-casino-financial-analysis.md` §2.15 | Read-only stream; "settlement"/"rollback" are represented as detection cases, which is the correct adaptation for a reconciliation stream. No gap found. |
| W3a (casino_statement, MOCK) | Read-only w.r.t. money | `02-casino-financial-analysis.md` §2.15 | Same as W2b. No gap. |
| W3b (awssm backend) | No (secret retrieval only) | `01-provider-trust-analysis.md` §2.3 | Full matrix not required. No gap found for its own scope. |

**Binding requirement (condition 1, §8):** W1c's test suite must add, using the existing MOCK
resolver:

- `TestPostBet_G1_ConcurrentIdenticalBetCallbacks_PostsExactlyOnce`
- `TestPostWin_ConcurrentIdenticalWinCallbacks_PostsExactlyOnce`
- `TestPostRollback_ConcurrentIdenticalRollbackCallbacks_TombstonesExactlyOnce`

Each fires the same signed payload (same `provider_tx_id`) from N concurrent goroutines against the
webhook HTTP handler (not the internal function directly — item 9, "the actual inbound path"), and
asserts exactly one ledger transaction, exactly one audit row, and that every non-winning goroutine
received the *original* result (200 with the same effect), never a 500 or a second posting. This is
distinct from the already-planned "a late original and its rollback, concurrently" case (§1.11 (i)),
which races two *different* references, not the same one.

---

## 3. Pre-fix evidence requirement (Stage 10.1 rule, re-applied)

Per the rule this stage inherits ("a regression test must be shown to fail against the pre-fix code"),
the following defects named in the papers must each ship with a red-before-green artifact, committed to
`docs/plans/stage-10.3-planning/evidence/` (the proposal does not yet name this directory; naming it is
part of condition 8):

| Defect | Pre-fix evidence required |
|---|---|
| **CAS-CAP-ROLLBACK-1** (F1–F3): disabled capability 503s a verified win/rollback, writes no tombstone | `TestCasinoWebhook_CAS_CAP_ROLLBACK_1_DisabledCapabilityBlocksRollbackOfAlreadyPostedBet` and the equivalent win case must be run and recorded as **failing** (503, no posting, no tombstone) against the pre-W1c code before the fix lands, then flipped to assert the new §1.3 table's row (post succeeds). The proposal already names this test (`orchestrator.go` §1.11) as a flip target; the red run itself must be captured as evidence, not just asserted to exist. |
| **G-1** (F9): two cash bets, one win → 500 `ErrAmbiguousMultiOriginRound` | `TestPostWin_G1_MultiCashBetRound_CharacterizesAmbiguousOriginAs500` must be written and run **first**, showing the current 500, per the paper's own instruction ("First write a characterization test"). Only then does the fix land and the test is renamed/re-asserted to the 409-mapped or successful-win outcome. |
| **F8** (late original after tombstone is an untyped 500) | `TestPostBet_E3_LateOriginalAfterTombstone_CharacterizesAs500` before, then reasserts the named rejection (E3) after. |
| **WH-VENDOR-SCHEME-1 skip→fail conversions** (payments 85/114/221, casino 164/187/270/292) | Each converted case must be run once against the **pre-conversion** conformance fixture hook to confirm it currently `t.Skip`s (i.e., would silently pass a non-mock adapter that fails ADR 0022 §6), then confirmed `t.Fatal`s once a deliberately broken fixture is supplied post-conversion. This is the self-test obligation (§4.1) applied to the conversion itself, not only to the final suite. |
| **KYC unbounded reason** | A test against the current `MockKYCProvider.HandleCallback` showing an oversized (>512 byte) or control-character-laden `reason` reaching `kyc_verifications.reason`/`audit_log.metadata` unmodified, before `NormalizeReason` exists. |

---

## 4. Per-wave binding test plan

Naming convention used throughout: `Test<Subject>_<ID-or-CaseLetter>_<WhatItProves>`, following the
codebase's existing precedent (`TestCasinoWebhook_CAS_CAP_ROLLBACK_1_...`,
`TestMigration0048_PreflightGuardFiresOnBarePlayerLocked`). Every new guard/scheme must have a
demonstrated mutation-kill (Stage 10.2 rule: "must fail on the test's own assertion, not an incidental
error") and, where the wave introduces a scheme/conformance suite, a **self-test** proving deliberately
broken reference implementations go red (ruling J1, already invoked by the proposal for W1a — extended
here to W1c and W1d's conformance additions, which the proposal does not explicitly say need a
self-test of their own).

### W1a — WH-VENDOR-SCHEME-1

- **Package:** `internal/webhookauth`, new `internal/webhookauth/webhookauthtest`.
- **Required cases:** SC1–SC13 exactly as specified in `01-provider-trust-analysis.md` §1.2, each named
  `TestSchemeConformance_SC<n>_<Assertion>` in the harness, run against (a) the MOCK scheme adapted to
  `VerificationScheme`, and (b) a test-only timestamped reference scheme (for SC7, since MOCK is
  exempt).
- **Conformance self-test (mandatory, per J1):** `TestSchemeConformanceSelfTest_IgnoresTenant_GoesRed`,
  `..._IgnoresTimestamp_GoesRed`, `..._PrefixCompareAcceptsPrefix_GoesRed` — three deliberately broken
  reference schemes, one per named failure mode in the proposal's §1.2 "Rules for the suite itself".
  Each must be shown to fail the suite; a self-test that itself always passes is not acceptable
  evidence.
- **Mutation-kill:** removing the orchestrator-enforced `scheme.Verify` call (making `HandleCallback`
  solely responsible again) must turn at least one test red — a dedicated
  `TestOrchestratorVerify_<Domain>_EnforcedEvenIfAdapterSkipsIt` per domain, not inferred from SC1–SC13
  alone (those exercise the scheme, not the orchestrator's call site).
- **Skip→fail conversions:** `TestPaymentsConformance_TenantBindingFixtureHook_FailsForNonMockAdapter`
  and the casino equivalents at the four cited line numbers — each renamed off `t.Skip` and proven per
  §3 above.
- **No-effect:** N/A directly (this wave changes the auth gate, not posting logic); the existing
  no-effect suite for a failed-verify path (`internal/testsupport/noeffect`) must be re-run unedited and
  stay green, since I1 is explicitly claimed unchanged.
- **API contract:** update `TestOpenAPI_PaymentsWebhook_ContractMatchesHandler` and the casino/KYC
  equivalents for the header-description change; these stay structural per the existing accepted
  limitation (`testing-strategy.md` "Stage 10.1 — known limitation").
- **Local only.** No STAGING REQUIRED items in this wave.

### W1b — MOCK-ADAPTER-PROD-1

- **Package:** new `internal/providerkind`.
- **Required cases:**
  - `TestSyntheticGuard_Matrix` — table over `{production, staging, development} × {synthetic,
    non-synthetic}`, one subtest per cell.
  - `TestSyntheticGuard_ASTCompletenessScan` — scans `internal/**` for `type Mock\w+` implementing a
    provider/scanner/storage/resolver/statement-source interface and requires `SyntheticComponent()`.
  - `TestSyntheticGuard_ASTCompletenessScan_GoesRedIfMarkerRemoved` — the required negative control:
    temporarily (in a subtest, via a build-tag-gated fixture type, not by editing production code)
    remove the marker from a fixture mock and assert the scan fails. This is the wave's own self-test
    obligation and is not explicitly named in the proposal's item-2 description ("a mock with the
    marker deleted must go red") — it must exist as a real, runnable test, not only as a described
    property.
  - `TestSyntheticGuard_RegistrationCompletenessScan` — a deliberately unregistered mock added to the
    test fixture must fail.
  - `TestSyntheticGuard_RunsBeforeDBConnect_Subprocess` — spawns the binary with `APP_ENV=production`
    and an unreachable `DATABASE_URL`; asserts the guard's named error, not a DB-connection error.
- **CI tag:** the subprocess test spawns a real process and manipulates environment/DB-reachability —
  tag it `//go:build integration`, consistent with this codebase's existing convention of keeping
  slow/process-spawning tests out of the plain `go test -race ./...` unit step (see condition 7).
- **Mutation-kill:** deleting the `syntheticGuard` call from `run()` must turn the subprocess test red.
- **Local only.**

### W1c — CAS-CAP-ROLLBACK-1 + G-1

- **Package:** `internal/casino`, `internal/httpserver` (webhook path), migration `0094`.
- **Required cases (per `02-casino-financial-analysis.md` §1.11, plus condition 1 additions):**
  - Full E1–E10 table from §1.3, one test per row per capability state (`S-none`/`S-off`/`S-nobet`/
    `S-on`), named `TestCasinoWebhook_E<n>_<CapabilityState>`.
  - **Idempotency under concurrency (new, condition 1):** the three concurrent-identical-callback tests
    named in §2 above.
  - **Concurrency (existing, from the paper):** late-original-vs-rollback race (asserting the L0.1
    serialization point, not only the outcome — per the Stage 10.1 rule that concurrency tests must
    assert *where* the waiter blocks), two-distinct-rollbacks-of-one-unseen-original, capability
    disabled concurrently with an in-flight bet.
  - **G-1:** `TestPostWin_G1_MultiCashBetRound_Wins` (normal), `..._Replay`, `..._ConcurrentWins` (round
    lock serializes), `..._RollbackOneThenWin`, `..._TwoWalletsUnderOneRound_Returns409`.
  - **Conformance:** `TestCasinoConformance_BetWithoutRollback_FailsSuite` (an adapter declaring
    `SupportsBet` without `SupportsWin`/`SupportsRollback` must fail the suite) — this is the wave's own
    conformance addition and needs the same self-test treatment as W1a: a deliberately compliant
    reference adapter must still pass, proving the check isn't vacuously red.
  - **Authorization:** cross-tenant rollback stays 401 with zero tombstones in both tenants (existing
    E4-shaped test, re-run unedited); a brand-B capability must not authorize brand-A bets.
  - **Auditability:** E3 rejection audit row exists; capability-write audit records before/after of
    every `supports_*`/`status`/`supported_assets` (closing F6).
- **No-effect checklist** (the Stage 10.1 six-point list, applied to every rejected-callback path: E3,
  E10, and pre-verification 401s): (1) no new ledger rows, (2) no new/changed `casino_provider_rounds`
  rows, (3) no tombstones beyond the one expected, (4) no audit rows beyond the one expected rejection
  record, (5) debit/credit totals unchanged, (6) projections unchanged — via
  `internal/testsupport/noeffect.AssertNoCasinoEffect` on **both** tenants from a fresh transaction.
- **Migration `0094` tests:** up on a clean DB; up **refuses** on a scratch DB (owner role, FORCE RLS
  bypassed by ownership) seeded with a violating row (`supports_bet=true, supports_win=false`), with the
  exact `RAISE EXCEPTION` message asserted, not just a nonzero exit; down (`DROP CONSTRAINT IF EXISTS`)
  is idempotent and always succeeds; up→down→up round trip on a fresh DB. Named
  `TestMigration0094_PreflightRefusesViolatingCapabilityRow`,
  `TestMigration0094_UpDownUpRoundTrip`. The chain-tip pin test (the codebase's equivalent of
  `TestWave3Phase2Migrations_FullChainUpDownUpRoundTrip`) must be extended to include 0094 as the new
  tip.
- **Locally:** everything above. **No STAGING REQUIRED items** in this wave.

### W1d — KYC-REASON-BOUND-1

- **Package:** `internal/kyc`, migration `0095`.
- **Required cases:**
  - Unit (`reason_normalize_test.go`): truncation boundary at 511/512/513 bytes with multi-byte UTF-8
    (never split a rune), control-character stripping (NUL, `\n`, `\r`, ANSI escapes, other C0/C1),
    truncation-marker/flag set on truncation, clean short string passes through unchanged (no
    false-positive mutation on the identity case).
  - Integration: `MockKYCProvider.HandleCallback` with an oversized/control-character body normalizes
    before it reaches `ProviderResult`, the DB row, **and** `audit_log.metadata` — assert the *stored*
    value, not just the in-memory struct (per the paper's own Tests section).
  - **Authorization/tenant isolation (new, condition 2):**
    `TestKYCHandlers_PlayerResponse_NeverContainsRawReason` — for every adapter outcome fixture, the
    player-facing JSON body must not contain the raw `reason` string under any field name.
    `TestKYCVerifications_TenantIsolation_ReasonAndReasonCode` — tenant B cannot read tenant A's
    `reason`/`reason_code` through any staff route, run as the NOBYPASSRLS role.
  - **Conformance:** mandatory (fail-not-skip) case that a real adapter's `HandleCallback` returns a
    bounded, control-character-free `reason` and a valid `reason_code` enum member, per the paper's own
    Tests section. This new conformance case also needs its own self-test: a deliberately
    non-conforming fixture adapter (returns an oversized reason) must fail it.
  - **API contract:** structural test asserting `PlayerVerification` never carries a `reason` field (or,
    if HD-10.3-3 keeps it present-but-empty, that it is never populated) — matches the existing
    `provider_reference` removal precedent.
- **Migration `0095` tests:** pre-flight normalizes existing over-length/control-char rows (synthetic
  data only, stated in the migration's own comment) before the CHECK is added — a scratch-DB test
  seeding an over-length row and asserting the migration succeeds and the row is truncated/cleaned, not
  refused (this migration's pre-flight fixes data rather than refusing, unlike 0094/0097 — the test must
  confirm that distinction deliberately, not by omission). Down drops both CHECKs and the column
  cleanly, round-trip tested.
- **Locally:** everything above. **No STAGING REQUIRED items.**

### W2a — Credential resolver, secret store, outbound credentials, O4

- **Package:** `internal/webhookauth` (resolver, `secretstore` interface), new `internal/secretstore`
  (memory/devfile backends locally; `awssm` is W3b), migration `0096`.
- **Required cases:**
  - Resolver: exactly-one-row, overlap (`active` + `verify_only` within `not_after`), expiry, immediate
    revocation, fail-closed for every row of the §2.2 fail-closed matrix (`no_resolver`,
    `credential_unavailable`, `credential_store_unavailable`, `credential_integrity`,
    `disallowed_backend`).
  - **Concurrency:** cache/revoke race — a request resolving mid-revocation must either see the
    pre-revocation credential (and its verify succeeds, consistent with "last committed state before
    read wins") or the post-revocation state (401), never a torn read; `singleflight` under concurrent
    identical-ref lookups (exactly one store call for N concurrent callers).
  - **Point-9 amendment statement capture:** extend the existing K7/C7-style statement-capture tests to
    assert the **new** allowed pre-verification statement shape (the resolver's tenant-predicated
    `SELECT` on `provider_credential_handles`) is the *only* addition — any other pre-verification
    statement must still fail the capture test. Named
    `TestPointNineCapture_<Domain>_AllowsExactlyOneHandleRead`.
  - **Mutation-kill (per §2.4 impact matrix, explicitly required by the paper):** remove the tenant
    predicate from the lookup query → red; remove the fingerprint check → red; remove the `not_after`
    predicate → red; give the table a `WithoutTenant`-readable policy → red. Four distinct tests, one
    per guard.
  - **RLS:** tenant A cannot read/write tenant B's handles; `WithoutTenant` sees zero rows; the
    forward-only status trigger rejects a backward transition (`revoked → active`) and rejects
    `DELETE`.
  - **Admin API:** create/rotate/revoke round trip; audit rows carry handle+fingerprint only, never
    secret material; permission-denied case for the wrong role; a fingerprint mismatch on registration
    is rejected before the row commits.
  - **O1 (outbound credential):** the HTTP client's per-call `Authenticator` is exercised with two
    different tenants' outbound credentials in the same process and asserted not to cross-contaminate
    (a static-header regression here would be silent and severe — this is the direct test of "a shared
    adapter therefore cannot use per-tenant credentials" being fixed).
  - **O4:** KYC provider selection reads tenant/jurisdiction configuration, not a hard-coded string;
    test with two tenants configured for different (mock) providers.
- **No-effect:** a resolver failure (any row of the fail-closed matrix) must produce zero DB writes
  beyond the read itself — reuse `AssertNoEffect`/`AssertNoCasinoEffect`.
- **Migration `0096` tests:** additive, no pre-flight needed (new table) — still requires up/down
  (refuses if rows exist, per the proposal's own §6) and a round trip on a fresh DB.
- **Local versus staging:** exactly as `01-provider-trust-analysis.md` §2.3 splits it — everything above
  is local against `memory://`/`devfile://` and an SDK-interface fake is **not** part of W2a (that is
  W3b). **STAGING REQUIRED**: IAM policy, network path to Secrets Manager, rotate/outage drills — none
  of these can be satisfied by W2a's own test suite, and W2a's tests must not claim to satisfy them.

### W2b — Casino reconciliation, internal stream

- **Package:** `internal/reconciliation`, migration `0097`.
- **Required cases (per `02-casino-financial-analysis.md` §2.15):**
  - One detection test per mismatch kind C1–C7, named `TestCasinoConsistency_C<n>_<Kind>`, each
    asserting exactly one mismatch row of the right kind and key, injected on a scratch DB as owner with
    triggers bypassed where needed.
  - A clean seeded world (bets, wins, rollbacks, tombstones, multi-tenant) gives a clean run for both
    streams — the explicit "no false positive" case.
  - Deduplication: evidence-type findings (C6, C7) are not re-raised across runs; a re-run after a
    persistent state-type condition still detects it (documented as intentional per ADR 0023 §4).
  - **Concurrency:** two concurrent sweeps of the same tenant/stream → one run, one `skipped`
    (advisory-lock proof); a sweep concurrent with live bet/win/rollback traffic produces zero false
    mismatches (snapshot consistency).
  - **Rejection record:** a verified rejection writes exactly one row (idempotent on redelivery via
    `ON CONFLICT DO NOTHING`); an `AuthError`-rejected (unverified) callback writes **zero** rows — this
    is the I1-preservation proof for this wave and must be its own named test, not inferred from the C6
    test alone.
  - **RLS / authorization:** two-tenant isolation on both `reconciliation_mismatches` and
    `casino_callback_rejections`; the runtime role cannot `UPDATE`/`DELETE` either (append-only proof,
    mutation-kill: temporarily grant `UPDATE` in a scratch fixture and confirm the *deny trigger* — not
    just the `REVOKE` — is what stops it, per the "binding control is the trigger, REVOKE is
    defense-in-depth" framing already used for `sportsbook_bet_settlements`).
- **Migration `0097` tests:** additive CHECK-widening needs no pre-flight (stated correctly in the
  paper); the down path is refusal-based once evidence exists — a scratch-DB test that inserts one
  `casino_callback_rejections` row and asserts down **refuses** with the "roll forward" message, plus a
  separate clean-DB test that down **succeeds** with no evidence. Round trip on the clean-DB case only.
- **Local only.**

### W3a — Casino statement stream (MOCK source)

- **Package:** `internal/reconciliation`, `internal/reconciliation/statement`, `internal/casino`
  (`MockStatementSource`).
- **Required cases:**
  - Key match: bet/win/rollback lines matched by `(provider_id, provider_tx_id)`, including the
    rollback-vs-tombstone pairing being treated as a match, not a mismatch (explicit test, since this is
    the one case in the model where a real mismatch pattern is *not* raised).
  - Totals match: per `(provider, asset, period)`, net `house_gaming` movement equals the MOCK source's
    stated GGR.
  - Injected divergence (amount, missing line, extra line, wrong kind) each produces exactly one
    `cas_mock_statement_mismatch` row — the label must contain "MOCK" in the run audit, the mismatch's
    `actual_value`, and the source's own `Label()`, per the `sb_mock_statement_mismatch` precedent this
    wave explicitly follows.
  - `nil` source fails the run closed (not "clean") — this is a required negative case, not optional,
    since a silently-clean run on a broken wiring is the worst failure mode for a reconciliation stream.
  - Duplicate statement line → mismatch (the sportsbook dedup-test precedent, explicitly named in the
    paper — must be a real test, not asserted by analogy).
- **Conformance self-test angle:** because the MOCK source is tautological by construction, its "clean"
  result proves nothing about detection on its own — the injected-divergence tests above **are** this
  wave's self-test equivalent, and at least one must be shown to fail if the matching logic is stubbed
  to always return "match" (mutation-kill on the matcher itself).
- **Local only** for the interface + MOCK source. A real statement source is explicitly `PROVIDER
  DEPENDENT` and out of scope for any test QA can write now.

### W3b — Secret-store AWS backend (code only)

- **Package:** new `internal/secretstore/awssm` (or equivalent), tested against an SDK-interface fake.
- **Required cases:**
  - Pinned `VersionId` is passed on every `GetSecretValue` call; a ref without a pinned version is
    rejected before any call is made.
  - JSON-key extraction from the secret payload; a missing/wrong key produces a typed error, never a
    panic.
  - Timeout: the fake simulates a hang past the 2s context budget → the call returns
    `credential_store_unavailable`-shaped error, not a hang.
  - Error classification: distinguishing "not found" from "access denied" from "transient" against the
    fake's error taxonomy, each mapped to the correct fail-closed reason.
  - **A CI-enforceable guarantee that this backend's tests never make a real network call.** Add
    `TestAWSSMBackend_NeverDialsRealAWS` (or an equivalent build-time/runtime guard, mirroring the
    `infrastructure` CI job's own "Uses NO AWS credentials" framing) so a future edit that
    accidentally wires a real client into a test cannot pass silently in an environment that happens to
    have `AWS_*` env vars set.
- **Local versus staging:** the SDK-interface fake tests above are local and sufficient for `W3b: code`.
  **STAGING REQUIRED** (not testable by QA locally, and must not be claimed as covered by W3b's own
  suite): real `GetSecretValue` latency/throttling at 2 replicas, cache warm-up/cold-start behaviour, an
  end-to-end rotation drill and a store-outage drill with a synthetic secret, the CloudWatch alarms
  firing on `credential_store_unavailable`/`credential_integrity`, confirming no secret value appears in
  container logs/ECS task metadata/Terraform state.

---

## 5. Migration test summary (0094–0097)

| # | Wave | Up (clean DB) | Pre-flight refusal test | Down | Round trip | Chain-tip pin |
|---|---|---|---|---|---|---|
| 0094 | W1c | required | required (seeded violating row, exact message asserted) | idempotent `DROP CONSTRAINT IF EXISTS` | required | extend to 0094 |
| 0095 | W1d | required | pre-flight **fixes** data (not a refusal) — test must confirm normalization, not refusal | drop CHECKs + column, no data transform needed | required | extend to 0095 |
| 0096 | W2a | required (new table) | n/a (additive) | refuses if rows exist | required | extend to 0096 |
| 0097 | W2b | required | n/a for the CHECK widening; refusal path is on **down**, not up | refuses once evidence exists (scratch-DB test with one seeded row); succeeds on a clean DB | required (clean-DB case only) | extend to 0097 |

All four must run under the existing "Migration reversibility (down then up)" CI step's pattern (fresh
database via the CI-only test-admin role), and each needs its own scratch-DB pre-flight/refusal test
under `internal/testsupport/scratchdb`, per the Stage 10 W0 convention.

---

## 6. CI-FLAKE-281 disposition

**Should its likely cause be addressed in 10.3? Yes — as a condition, not a blocker to starting
planning.**

The investigation's own classification is "most likely: the Stage 9 `internal/httpserver` Argon2-heavy
concurrent-auth tests' fixed 30-second ceiling (`stage9AwaitAll`), under CI-runner CPU contention," with
a directly measured 74% margin consumption under this session's own artificial stress — no code defect,
a real timing budget problem. Stage 10.3 makes this worse in two concrete ways:

1. **W2a's admin credential API** adds new HTTP-level integration tests. If they land in
   `internal/httpserver` (the natural home for an HTTP handler test, and already the single largest
   package in the suite at 225–274s), they add more CPU/DB work to the same test binary that already
   contains the tight-margin Stage 9 tests, run under the same `GOMAXPROCS`-bounded scheduler.
2. **W1c's expanded casino webhook table (E1–E10 × 4 capability states) and W2b's C1–C7 detection
   tests** add real PostgreSQL work to the same overall `go test -race -tags=integration ./...`
   invocation, increasing total contention for CPU and DB connections across the whole job — the exact
   mechanism the investigation names ("31 other packages are also executing" at the same time).

Recommendation (condition 6): before or alongside W1c/W2a landing, `devops`+`qa` should either (a)
raise `stage9AwaitAll`'s fixed timeout with a documented margin re-measurement, or (b) scale it to
`configuredMaxConns`/observed CPU count, per the investigation's own §7 remedy — and in the meantime,
new W2a admin-API tests should be placed in a way that does not add to `internal/httpserver`'s own
heaviest existing test files (a separate `_test.go` file is sufficient; a separate package is not
required). This is not a new investigation; it is executing the remedy the existing investigation
already named as the correct next step "if it recurs" — 10.3 is exactly the kind of change likely to
make it recur sooner.

---

## 7. CI changes needed

1. **New required-pass test names** for the "Assert integration evidence (Stage 10 W0 gate)" step in
   `.github/workflows/ci.yml`, mirroring the existing `TestMigration0048_...`,
   `TestMigrateUp_RecordsCorrectChecksum`, `TestWave3Phase2Migrations_FullChainUpDownUpRoundTrip`,
   `TestMigration0077_...` list: add the 0094 pre-flight-refusal test, the 0095 normalization test, and
   the 0097 down-refusal test named in §5. A green step does not otherwise prove these actually ran and
   passed, exactly the reasoning already recorded in that CI step's own comment.
2. **Tag the synthetic-guard ordering test (W1b) `//go:build integration`** — it spawns a subprocess and
   depends on DB-unreachability semantics, which is the existing dividing line this codebase already
   uses between the plain unit step and the `-tags=integration` step.
3. **A go.sum/dependency-drift guard**, or at minimum a documented manual check, before W3b lands. This
   is the platform's first runtime AWS SDK dependency (`aws-sdk-go-v2/service/secretsmanager`); nothing
   in the current workflow verifies `go.sum` isn't silently stale relative to `go.mod` (no `go mod
   verify`/`go mod tidy --check` step exists today). Given `security` must already review this
   dependency (per the provider-trust paper §2.2), a CI-enforced drift check closes the loop rather than
   relying on manual review alone.
4. No new integration **package** wiring is otherwise needed: `internal/webhookauth/webhookauthtest`,
   `internal/providerkind`, `internal/secretstore` (and its `awssm` subpackage) are all picked up
   automatically by the existing `go test -race ./...` and `go test -race -tags=integration ./...`
   invocations, which already recurse over `./...`.
5. No change is needed to the fixed `-steps=4 down` in the "Migration reversibility" step: it always
   exercises the most recent four migrations on a fresh database with no evidence, which correctly
   covers 0094–0097 once all four have landed, and correctly covers a subset during intermediate waves.

---

## 8. Verdict

**APPROVE WITH CONDITIONS.**

The proposal's scope, dependency ordering, migration design, and per-item test intentions (as recorded
in the three specialist papers) are sound, evidence-based, and consistent with the binding rules `qa`
already set in Stage 10.1/10.2. The gaps found are additions to an already-strong plan, not
re-scoping — none of them changes wave boundaries, migration numbering, or the human-decision register.

**Conditions (binding; each must be satisfied before the corresponding wave is marked `IMPLEMENTED`):**

1. Add the three concurrent-identical-callback ("idempotency under concurrency") tests to W1c/G-1, named
   in §2, closing the one CLAUDE.md financial-list item not explicitly covered by the existing test
   lists.
2. Add the player-facing-response and tenant-isolation tests for the new KYC `reason`/`reason_code`
   columns to W1d, named in §4 (W1d), per the "Baseline requirement" (authorization + tenant isolation)
   that applies to this change even though the full financial matrix does not.
3. Fold this document's per-wave test plan into the implementation gate's own §15 (or reference it
   directly by section), so a reviewer can confirm CLAUDE.md financial-list completeness from the gate
   document itself rather than needing to cross-reference three specialist papers plus this review.
4. Wire the new migration test names (0094–0097, §5/§7) into `.github/workflows/ci.yml`'s "Assert
   integration evidence" gate before each corresponding wave's implementation is marked complete.
5. Add a go.sum/dependency-drift check to CI before or with W3b (§7 item 3), given this is the
   platform's first runtime AWS SDK dependency.
6. Address CI-FLAKE-281's likely cause (§6) before or alongside W1c/W2a: raise or scale
   `stage9AwaitAll`'s fixed 30s ceiling, and avoid adding new W2a admin-API HTTP tests to
   `internal/httpserver`'s existing heaviest test files. This is a condition on wave execution hygiene,
   not on the planning-gate authorization itself.
7. Tag the W1b synthetic-guard subprocess/ordering test `//go:build integration` (§7 item 2).
8. Name the evidence directory (`docs/plans/stage-10.3-planning/evidence/`) explicitly in the
   implementation gate, and ensure every pre-fix characterization test named in §3 above is committed as
   a recorded red-then-green artifact, exactly as Stage 10.1/10.2 did.

None of these conditions block **HD-10.3-1** (scope authorization) from being granted; they are
conditions on the waves' own completion, to be verified by `qa` at each wave's close per the existing
"reviews test coverage on every non-trivial change before it's marked `IMPLEMENTED`" mandate.
