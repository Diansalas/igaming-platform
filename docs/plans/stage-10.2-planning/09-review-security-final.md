# Stage 10.2 — Final post-implementation security review (`security`)

Scope: diff `69e80c1..HEAD` on `claude/focused-wright-jw88w9` (HEAD `33492d2`; the review was run against
`11b2ad1`, and `33492d2` changes only docs). Covers KYC-WH-1, CAS-WH-TENANT-1, the `internal/webhookauth`
extraction and PAYWH-GATE-1. **Production code is treated as final.** Uncommitted casino test work in the
working tree is out of scope and was not reviewed. That work is `casino_prefix_e4_defect_test.go`,
`casino_webhook_tenant_binding_test.go`, `testsupport/noeffect/noeffect.go` and
`internal/casino/recording_tx_integration_test.go`.

Binding references checked: CLAUDE.md; ADR 0091; `01-webhook-trust-design.md` including §J (J1–J17);
`07-review-architect-db.md` (R1–R5, §2–§6); `08-kyc-test-traceability.md`; the ADR 0022 §3 Stage 10.2
amendment (e69c2e9, points 8–9 and the Status update).

## Verdict: **APPROVE WITH CONDITIONS**

- **Production code:** no blocking finding. KYC-WH-1 (High) and CAS-WH-TENANT-1 (Medium) are closed at the
  code level, **for the MOCK only**.
- **Test evidence:** incomplete for the casino side. That is conditions SC-1 to SC-3 below; all three are
  test or evidence items for the casino agent currently working in `internal/casino` and
  `internal/httpserver`.
- **Documentation:** SC-4 covers the completion-report disclosures.

None of the findings needs a human decision.

## Evidence run by `security`

| Check | Result |
|---|---|
| `go test ./internal/webhookauth/... ./cmd/platform-api/...` | ok |
| `go test -run WebhookAuthAliases ./internal/payments/` (J2) | ok |
| `go test -tags=integration ./internal/kyc/...` | ok, 48 PASS, 0 SKIP |
| `go test -tags=integration -run 'KYC\|TestWebhook_\|PaymentWebhook\|OpenAPI_(KYC\|Casino\|Payments)\|CasinoWebhook\|CasinoPlay_Broken' ./internal/httpserver/` | ok, 45 PASS (the package compiles the casino agent's uncommitted test edits, so this run reflects the working tree, not bare HEAD) |
| `go vet` webhookauth / kyc / platform-api | clean |
| `internal/casino` integration suite | **not run** (another agent is editing it) |
| G7 constant scan | see §7 |

## 1. Invariant I1 (strict), R3 single tenant source, credential equality: PASS

**Pre-verification statements.** Before verification, the only statements are:
- the platform-wide `GetTenantBySlug` (`internal/httpserver/webhook_preamble.go:64`);
- `WithTenant`'s `BEGIN` plus `set_config` (`internal/db/tenant_rls.go:35-43`).

**Pure checks before any DB work.** The provider charset, the bounded body read and the header format run
in `webhookauth.Scheme.CheckPreamble` (`internal/webhookauth/webhookauth.go:328-343`).

**KYC** (`internal/kyc/provider.go:226-290`). Steps before verification:
1. `ParseHeaders`;
2. registry lookup (map);
3. resolver nil check;
4. `Resolve` (MOCK, pure HMAC);
5. the `cred.TenantID/ProviderID` equality check (`:256`);
6. `HandleCallback` (the `Verify` at `mock_provider.go:191`).

None of these touches `tx`. The first tenant-scoped statement is `getVerificationByProviderReference`
(`:282`), after verification. The K7 statement capture proves this (`TestKYCWebhook_BadSignature_NoStatementBeforeVerification`).

**Casino** (`internal/casino/orchestrator.go:602-667`). Same order. `LoadCapability` (`:662`) is the first
tenant-scoped statement and runs after `HandleCallback` (`:645`). `postBet`, `postWin`, `postRollback` and
the tombstone are later still. **Test evidence (C7 statement capture) is not yet committed; see SC-1.**

**R3/J5 (single tenant source).** Both orchestrators overwrite `in.TenantID/in.ProviderID` from their
parameters first (`kyc/provider.go:227-228`, `casino/orchestrator.go:603-604`). The handlers pass the same
`t.ID` to `WithTenant` and `ReceiveCallback`:
- KYC: `kyc_admin_handlers.go:456-459`;
- casino: `casino_handlers.go:343-346`.

The play-simulation handlers pass `tc.TenantID` (from the JWT) to both `CallbackPayload` and
`ReceiveCallback` (`casino_play_handlers.go:352-354, 445-447, 585-587`). `Scheme.Verify` rebuilds the signing
input from `in.TenantID/ProviderID` only (`webhookauth.go:291-294`).

**Credential equality.** The check is enforced twice:
- in each orchestrator (`kyc/provider.go:256`, `casino/orchestrator.go:625`);
- again inside `Scheme.Verify` (`webhookauth.go:288-293`), which checks both key id and tenant/provider
  binding.

## 2. Domain separation and mock keys: PASS

- **Separate parameters per domain.** Each domain has its own prefix, headers and KDF label
  (`internal/webhookauth/domains.go`). The payments values are byte-identical to 10.1, pinned by
  `TestPaymentsParameters_ByteIdentical`.
- **Cross-domain test is sound.** `TestScheme_CrossDomain_NeverVerifiesUnderEqualKey`
  (`webhookauth_test.go:249-288`) covers all 6 ordered pairs, under an equal key and the same tenant,
  provider, key id and body. It checks two cases:
  - headers as delivered;
  - headers relabelled to the target domain.

  It also asserts that the three mock KDF labels give distinct keys under an equal master. The fixture key
  is `NewMockMaster()`, never a literal.
- **Mock master.** `NewMockMaster` reads 32 bytes from `crypto/rand` and panics on failure
  (`webhookauth/mock.go:417-423`).
  - `DeriveMockKey` panics on a master shorter than 32 bytes (`:437-439`).
  - `MockResolver` fails closed on a short master, an empty label or provider, a foreign provider, or a key
    id other than `mock-v1` (`:461-466`).
  - `MultiResolver` fails closed on a missing or nil entry (`:494-497`).
  - `NewMockKYCProvider()` takes no argument (`kyc/mock_provider.go:55`).
  - `casino.NewMockWebhookCredentials(nil)` returns an empty resolver, which fails closed.
- **No fallback path.** A nil resolver gives `ReasonNoResolver` in all three orchestrators. No code path
  verifies without a credential.

## 3. Constant time, strict parsing, body limits, uniform 401, logging, oracles: PASS (with Info notes)

- **Constant-time comparison.** `hmac.Equal` over the hex strings (`webhookauth.go:295`). Both are 64
  bytes after the format check, so the comparison is fixed-length.
- **Header parsing.** Regexes are anchored: `^v1=[0-9a-f]{64}$` and `^[a-z0-9-]{1,32}$`. RE2's `$` is
  end-of-text, so there is no trailing-newline bypass. Uppercase, 63/65 hex, a wrong version and missing
  headers are rejected (unit and K5 tests). Duplicate headers: see F-6.
- **Body limits.** `LimitReader(max+1)` runs before any tenant lookup: KYC 256 KiB, casino and payments
  1 MiB. Both oversized and unreadable bodies fold into `body_too_large`, giving the same 401.
- **Uniform 401.** Every failure in the preamble and every `*AuthError` becomes the same
  `apierror.Write(CodeUnauthorized, "callback rejected")` response:
  - preamble: `webhook_preamble.go:59,67,77`;
  - KYC: `kyc_admin_handlers.go:462-466`;
  - casino: `casino_handlers.go:349-353`;
  - payments: `payment_callback_errors.go:41-50`.

  The byte-identity tests are:
  - K6: `kyc_webhook_indistinguishable_401_test.go`;
  - C6: `TestCasinoWebhook_EnumerationOracle_IndistinguishableResponses`;
  - payments T9: unchanged.

  The former 404s (unknown slug, suspended tenant, unknown provider) and 400s (bad signature, oversized
  body, non-JSON body) are gone.
- **Allow-listed logging.** `callbackAuthFailureAllowlistFields` (`payment_callback_errors.go:110-126`) is
  shared by all three domains through `logWebhookAuthFailure`. It logs:
  - `request_id`, `reason`, `client_ip`, `body_len`;
  - `tenant_id` only if resolved;
  - `provider_id` and `key_id` only if charset-valid;
  - `credential_fingerprint` only for `signature_invalid`.

  It never logs the body, header values, slug or error text. K12 asserts a strict allow-list (the key set
  must be a subset of the allowed keys). There is no casino-specific K12 test (see SC-3).
- **Oracles.**
  - The deployment-wide 503 (nil orchestrator) is not tenant-dependent.
  - The 500 on a tenant-lookup DB error is not attacker-controllable. Both are unchanged from payments.
  - The post-verification 400/404/409/503 responses need a valid tenant-bound signature.
  - **Timing:** an unknown slug skips `WithTenant` and the HMAC, so latency differs. This is the Stage 10.1
    residual, already accepted as Low (`stage-10.1-planning/11-pay-wh-tenant-1-design.md:188`), and is
    carried forward unchanged (F-4).

## 4. Environment gating: PASS

- **Single gate.** `mockProviderWiring` (`cmd/platform-api/wiring.go:52-59`) derives all three flags from
  `cfg.TestSupportRoutesEnabled()`, which is `Environment != "production" && flag`
  (`internal/config/config.go:467-469`). Production is structurally off.
- **Wiring tests.** `TestMockProviderWiring_Matrix` covers {production, staging, development} × {on, off}.
  The `*_FollowsWiring` tests prove a true nil interface (not a typed nil) for prod/on, prod/off and
  staging/off.

**Surface matrix (verified):**

| Surface | prod | TS-off | TS-on |
|---|---|---|---|
| Payments webhook | nil resolver → 401 `no_resolver` (PAYWH-GATE-1, `main.go:150-154`) | same | MOCK resolver |
| KYC webhook route | absent → 404 | absent → 404 | registered |
| KYC orchestrator | nil → `POST /v1/me/kyc/verifications` 503 (K16) | same | present |
| Casino webhook | registered, nil resolver → 401 | same | MOCK resolver |
| Casino play routes | absent (`CasinoPlaySimulationEnabled`) | absent | present |

**KYC route and resolver cannot diverge.**
- The route needs `deps.KYCWebhookEnabled && deps.KYCOrchestrator != nil` (`kyc_routes.go:46`).
- In `main.go`, `KYCOrchestrator` and `KYCWebhookEnabled` both come from the same `wiring` value
  (`main.go:301-302`).
- `kycOrchestrator` pairs the orchestrator with `kycWebhookResolver` from that same value
  (`wiring.go:82-87`).

Every mixed state still fails closed: the route is absent, or a registered route returns 401 `no_resolver`.

**PAYWH-GATE-1 / J9 condition holds.** No payments test file was edited; the only new payments test file is
`webhookauth_alias_test.go` (J2).

**J4 (payments preamble) is satisfied.** `newPaymentWebhookHandler` uses `webhookPreamble`
(`deposit_handlers.go:285`), and T9 and the payments OpenAPI contract test are unedited. WH-PREAMBLE-1 is
not needed.

**MOCK-ADAPTER-PROD-1 is disclosed** in `main.go` comments (`:147-148, :182-184`), `wiring.go:44-45` and
`docs/governance/task-registry.md`. With the resolvers gated, no mock callback verifies in production.

**Verdict on it:** acceptable for 10.2. It remains a **pre-launch blocker** that must be removed or gated
before production launch.

## 5. KYC: PASS

- **No player self-approval.**
  - The only non-test signer is `MockKYCProvider.CallbackPayload`, and no production code calls it (grep).
  - The key is per-process `crypto/rand`, derived per tenant.
  - No simulate route exists (B4).
  - The E1 forge is inverted as K1 (`kyc_prefix_e1_defect_test.go`): 401, no effect.
  - Player `SubmitVerification` defaults to `review_required`; it never approves.
- **`provider_reference` is gone from every player-facing response.**
  - `playerVerificationResponse` (`kyc_handlers.go:70-88`) has no such field. It is used for create
    (`:169`) and list (`:200-202`).
  - `toVerificationResponse` is used only on staff routes (`kyc_admin_handlers.go:174,249`).
  - No other player-facing reader of `kyc_verifications` exists (grep).
  - OpenAPI: `/v1/me/kyc/verifications` POST 201 and GET 200 reference `PlayerVerification`, whose
    properties are `id, player_account_id, status, provider_id, reason, reviewed_at, created_at`. No
    `/v1/me` path references `Verification`.
  - The webhook success response is 204 with no body (`kyc_admin_handlers.go:491-495`).
- **Forward-only CAS in the same transaction as its audit row** (`provider.go:374-416`):
  - the update is `UPDATE … WHERE id=$3 AND tenant_id=$4 AND status=$5`;
  - `audit.Record` runs on the same `tx` right after `RowsAffected()==1`;
  - `WithTenant` commits both or neither;
  - a lost race gets a bounded re-read (3 attempts), then an error.

  The rank function is `unverified 0 < pending 1 < review_required 2 < terminal 3`. Terminal states are
  never resurrected (J11).
- **Replays write no audit row.** An equal or lower rank returns before any write or audit (`:377-383`).
  The K8 tests cover replay, `pending` after `review_required`, callbacks after a staff decision, and 8
  concurrent callbacks producing exactly one audit row. The single exception is outcome `error` (F-5).
- **Explicit tenant predicate (R4/J6).** The query is
  `WHERE tenant_id=$1 AND provider_id=$2 AND provider_reference=$3` (`verification_service.go:262-266`),
  killed by mutation per 08.
- **Staff review path is unchanged and correct.** `POST /v1/admin/kyc/verifications/{id}/review` requires
  `RequireTenantScope` plus `RequirePermission(PermVerificationReview)` (`kyc_routes.go:33-34`).
  `RoleCompliance` is the sole grantee (`internal/auth/permission.go:688-690`). `internal/auth` is not
  touched by the diff.

## 6. Casino: PASS at code level; evidence conditions SC-1 and SC-2

- **Cross-tenant callbacks have no effect.** An A-signed callback at B's slug fails in `Verify`, which
  returns 401 before `LoadCapability`, `postRollback` or the tombstone. The tombstone is a
  `ledger_transactions` row with `provider_tx_id = original` (`orchestrator.go:1557-1562`), so the inverted
  E4 test (`casino_prefix_e4_defect_test.go`), which counts `ledger_transactions` for both ids and matching
  `audit_log` rows in **both** tenants, covers tombstones. **Gap:** the full six-point no-effect checklist
  (casino_* rows, projections, debits = credits) is not asserted at HEAD. See SC-2.
- **Play simulation never returns signed bytes.**
  - `writeCasinoCallbackResult` returns only `{outcome, provider_tx_id, tombstoned, ledger_transaction_id?,
    decline_reason?}` (`casino_play_handlers.go:209-218`).
  - An `AuthError` gives 503 "simulated play is misconfigured" (`:226-239`) and logs only `reason` and
    `action`.
  - `TestCasinoPlay_BrokenResolver_Returns503Misconfigured` covers this.
- **Replay, idempotency and tombstones are unchanged.** The diff touches only `orchestrator.go` hunks
  `:19`, `:34-56` and `:549-653`. `postBet/postWin/postRollback`, `mapReplayPayloadMismatch`, the F-7
  comparison and the key `tombstone:<provider>:<orig>` are byte-unchanged. Keys use the route `providerID`
  that was verified (point 6).
- **Verify-before-parse.** `json.Unmarshal` runs only after `casinoScheme.Verify`
  (`casino/mock.go:417-440`). The NUL-byte check stays as post-verification validation.

## 7. Secrets (G7): PASS

**Procedure.** The value was extracted at run time from `git show 69e80c1:cmd/platform-api/main.go` into a
scratchpad pattern file, used for the checks below, then deleted. It is not reproduced anywhere in this
review. (It did appear once in the reviewer's own tool output as the expected `-` line of `git diff`; it was
not copied.)

**Results:**
- `git grep -F -f <pattern> HEAD` → no match (exit 1), at both `11b2ad1` and `33492d2`.
- Working-tree `grep -rF` (excluding `.git`) → no match. The copy in the Stage 10.1 design doc is redacted.
- `git log -p 69e80c1..HEAD`: exactly **one** matching line, a `-` removal, in `f982d68`
  (`cmd/platform-api/main.go`). No commit after `69e80c1` **adds** it.
- The E2 guard (`cmd/platform-api/kyc_prefix_e2_const_secret_test.go`) checks by go/ast that
  `NewMockKYCProvider` takes 0 parameters and 0 arguments, and that the constant's name is absent. It never
  prints a value.

**Other hard-coded material in the diff:**
- `internal/kyc/orchestrator_webhook_integration_test.go:174`
  `[]byte("equal-secret-shared-by-every-tenant-32bytes!!")`: the **test-only** K4 fixture for the
  equal-secret resolver. It authenticates nothing outside that test's in-process resolver. It is not a
  credential, so G7 is not violated. Recommendation only: derive it from `NewMockMaster()` like the
  webhookauth tests, so the codebase never has a literal of this kind.
- `igaming/*-mock-webhook/v1` labels and all-zero or short hex strings: public KDF labels and tamper
  fixtures, not secrets.
- `.github/workflows/ci.yml`: this diff only adds a failure-only artifact upload of `integration-test.log`.
  The CI Postgres passwords in that file are pre-existing ephemeral service-container values, not added by
  this diff (out of scope). The uploaded log contains per-process random keys and synthetic data only.

## 8. Findings

| ID | Sev | File:line | Finding | Recommended fix |
|---|---|---|---|---|
| **SC-1** | Medium (evidence; condition for G3) | `internal/casino/orchestrator.go:602-653`; no test at HEAD | J8 makes the K7/C7 statement capture G3 evidence. **C7 (casino, bad signature → no tenant-scoped statement, `LoadCapability` not run) is not committed** at HEAD. Code inspection confirms I1 holds, but the binding evidence is missing, and 08 records casino C1–C14 traceability as NOT DONE. | Commit a casino statement-capture test that uses a recording tx on a bad signature and a cross-tenant signature. It must assert that zero statements run on `tx` before the 401 and that `LoadCapability` is never called. Show it going red when `LoadCapability` is moved before `HandleCallback` (J1). Add casino C1–C14 to a traceability table. |
| **SC-2** | Low (test soundness; condition) | `internal/httpserver/casino_webhook_tenant_binding_test.go:79-106` | `TestCasinoWebhook_EqualSecretResolver_StillTenantBound` (C4) wires the **normal** per-tenant derived resolver. It is a duplicate of C3 and does not exercise an equal-secret resolver. The property itself is proven at unit level (`webhookauth_test.go:151-155`, "tenant swapped … EQUAL key", run for the casino scheme). Separately, the C1/E4 no-effect checks (`casino_prefix_e4_defect_test.go:74-103`) and C3 (`:61-72`) omit casino_* rows, projections and debits = credits. | Wire a test resolver that returns one fixed `NewMockMaster()` secret for both tenants, sign with it, and assert 401 at B's slug; or rename the test and cite the unit test. Extend `noeffect` with casino points 3 and 6 and apply it to both tenants in C1/C3/C4/C5. |
| **SC-3** | Low (test gap; condition) | `internal/httpserver/casino_handlers.go:349-353` | No casino-specific allow-list log test (K12 equivalent) for `casino_webhook_auth_failed`. The risk is low because the code shares `logWebhookAuthFailure` with KYC and payments, which are both tested. | Add a table test over the casino reasons (`tenant_unknown`, `tenant_inactive`, `provider_invalid`, `body_too_large`, `signature_missing`, `provider_unregistered`, `no_resolver`, `signature_invalid`) asserting the strict allow-list. |
| **SC-4** | Condition (disclosure) | `docs/governance/stage-10.2-completion-report.md` (not yet written) | J17 disclosures are required. | The completion report must state: (a) **staging stays forgeable** (pre-fix KYC build) until the human-authorised refresh, and staging KYC `approved` rows are untrusted synthetic data; (b) both closures are MOCK-only; (c) MOCK-ADAPTER-PROD-1; (d) CAS-CAP-ROLLBACK-1, now a hard pre-condition per ledger-finance (`33492d2`). |
| F-4 | Low (accepted residual) | `internal/httpserver/webhook_preamble.go:64-80` | Timing differs between an unknown slug (no tx, no HMAC) and a known one. Tenant slugs are public brand identifiers. This is identical to the Stage 10.1 accepted residual. | None for 10.2. Revisit with PAYWH-RL-1. |
| F-5 | Low | `internal/kyc/provider.go:356-366` | Outcome `error` writes one failure audit row **per delivery**, including exact replays. That matches design B7, but it is the one exception to "replays write no audit row". Only a verified sender can trigger it; with the MOCK, only in-process code can. For a real vendor, a captured `error` callback could be replayed to grow `audit_log` without bound. | No change for 10.2. Before the first real KYC adapter: point 3's timestamp tolerance (already mandatory) and/or dedupe `error` audits on `(verification, provider event id)`. |
| F-6 | Info | `internal/webhookauth/webhookauth.go:229-230` | `ParseHeaders` uses `Header.Get`: with duplicate signature or key-id headers, the first value is used and the rest ignored. Not exploitable, because the value must still pass HMAC verification against the single credential. | Optional hardening in the shared package: reject `len(h.Values(name)) != 1` as `signature_invalid`. Re-run payments T9 to keep byte-identity. |
| F-7 | Info (pre-existing) | `internal/kyc/mock_provider.go:216`; `internal/httpserver/kyc_handlers.go:81-82` | The verified sender's `reason` string (up to about 256 KiB) is stored in `kyc_verifications.reason` and audit metadata and returned to the player. ADR 0028 intends a short machine-readable code. | When the first real adapter lands, bound `reason` (length and charset) in the adapter's normalisation. |
| F-8 | Info | `kyc/mock_provider.go:202-204`; `casino/mock.go` `hasLegacySignatureField` | A legacy `signature` field in an otherwise **verified** body is reported as `signature_invalid` with a fingerprint. That reason is the designated forgery alert signal, so this is a benign false-positive alert source. It follows design B6(d)/C2. | Optional: document the case in the alerting runbook. |
| F-9 | Low (pre-real-vendor gate) | `internal/casino/conformance_test.go:217-222`; `internal/payments/conformance_test.go:139-144`; no KYC conformance suite | The ADR 0022 §3 Status calls the tenant-binding conformance case "mandatory, skip-to-fail" for the first real adapter. As written, a non-mock adapter **skips** it, and KYC has no conformance suite at all. Not exploitable today (no real adapter). | Before any real adapter is registered: replace the `t.Skip` with a required per-adapter fixture that fails when missing, and add a KYC conformance suite with the same case. Track it with the real-resolver work. |

## 9. Launch-blocking items (flagged for the human, via the orchestrator)

None of these blocks Stage 10.2 completion. Each one **blocks production launch** or real-vendor go-live:
- Real KYC, casino and payments webhook resolvers are **NOT IMPLEMENTED**. The closures are MOCK-only.
- **MOCK-ADAPTER-PROD-1:** the mock payments and casino adapters are still registered in production.
- **CAS-CAP-ROLLBACK-1:** hard pre-condition for any real casino resolver.
- **F-9:** the conformance cases must actually fail for real adapters.
- **Staging** remains on the pre-fix KYC build (forgeable) until a human-authorised refresh. Its KYC rows
  must not be trusted.

No new requirement needing a human decision was found. PAYWH-BRAND-1, PAYWH-RL-1 and PAYWH-TS-1 stay
deferred (J10).

## 10. Review scope

**Covered:**
- `internal/webhookauth/*`;
- the payments extraction and aliases, and PAYWH-GATE-1;
- `cmd/platform-api/{main,wiring}.go` and the wiring tests;
- the KYC orchestrator, mock, verification lookup, handlers, routes and player/staff responses;
- the casino orchestrator `ReceiveCallback`, mock, handlers, play-simulation handlers and routes;
- the shared preamble and logging;
- the OpenAPI entries for the KYC and casino webhooks and `PlayerVerification`;
- the runbook and registry diffs, and the CI workflow diff;
- the G7 history scan;
- the `PermVerificationReview` grantees.

**Not covered:**
- the uncommitted casino test work in the working tree;
- a run of the `internal/casino` integration suite;
- real vendor schemes and secret-store design (none exist);
- volumetric DoS;
- staging/AWS state (not inspected);
- CI-FLAKE-281;
- a full re-audit of pre-existing post-verification casino and payments money paths, which are unchanged by
  this diff.

This review is code- and design-level. It is not a penetration test or a certification audit, and passing it
does not make the platform "secure" beyond the items listed here.
