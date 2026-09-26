> Stage 10.2 design review — specialist working paper (verbatim, recorded 2026-09-26 against 69e80c1). Where it differs from the Orchestrator rulings in `01-webhook-trust-design.md` §J, the rulings govern.

# Backend/API design review — Stage 10.2 webhook trust design (01-webhook-trust-design.md)

**Verdict: APPROVE the design as written, with one required addition before implementation
(explicit alias/behaviour-preservation test list for the payments migration commit) and
one recommendation (PAYWH-GATE-1: include in 10.2, small/same-class).**

## Verified against current code (HEAD)

- `kyc.NewMockKYCProvider(kycMockWebhookSecret)` at `cmd/platform-api/main.go:38-43,271-273` — confirmed
  compile-time constant, unconditionally wired. Removing the parameter (§B1) is correct and matches
  the "structurally cannot inject a literal" goal.
- `internal/httpserver/kyc_admin_handlers.go:479` currently does `writeJSON(w, http.StatusOK,
  toVerificationResponse(result))` — confirmed 200, while `docs/api/openapi/platform-api.yaml:2607`
  already documents `204`. The spec/code mismatch is real today; the design's fix aligns code to the
  already-published contract rather than the other way around.
- No casino webhook OpenAPI path exists (`grep webhooks/casino` — zero matches). New entry is additive,
  not a breaking change.
- Import graph checked: `internal/payments` currently imports only `audit`, `db`, `ledger`, `rg` — no
  dependency on `kyc`/`casino`, and neither of those import `payments`. A new `internal/webhookauth`
  leaf package (no imports of kyc/casino/payments/httpserver) sitting under all three, with
  `internal/httpserver` importing all four, introduces **no cycle**. Confirmed no existing
  payments/kyc/casino → httpserver imports either.

## Package boundary / extraction (§A)

- Extraction (not "import payments from kyc/casino") is the right call — reusing payments in place
  would invert domain direction and payments must stay financial-domain-only per CLAUDE.md provider
  abstraction rules. `webhookauth` as a pure verification/framing library with no financial or
  compliance semantics is an appropriate shared-kernel boundary.
- Per-domain prefixes (`igaming.{payments,kyc,casino}.webhook.v1`) giving domain separation is a good,
  cheap mitigation against key/scheme confusion across domains — endorsed.
- Byte-identical payments behaviour is explicitly gated on tests (T9, `openapi_paymentswebhook_contract_test.go`)
  staying green **unchanged**; the design correctly makes `newPaymentWebhookHandler`'s adoption of
  `webhookPreamble` conditional on that, not mandatory. Good — avoids forcing a refactor that isn't
  needed to close either finding.
- **Required addition:** the design lists aliases/sentinels to preserve (`ErrCallbackAuthFailed`, etc.)
  but the actual commit should include a mechanical `errors.Is/As` regression test asserting the
  aliased sentinels are pointer-identical to (or wrap) the new `webhookauth` ones, not just "checked by
  grep" for error-text asserts. This is cheap and removes ambiguity about what "byte-identical" means
  for the payments migration commit specifically (not just OpenAPI/handler behaviour).

## Route registration / gating (§B3, §C7, §D)

- Gating pattern (`deps.KYCWebhookEnabled && deps.KYCOrchestrator != nil` → else unregistered → 404)
  matches the existing `TestSupportRoutesEnabled()`-gated precedent (`CasinoPlaySimulationEnabled`,
  `PaymentsMockSettlementEnabled`, `AccountActivationTestSupportEnabled`,
  `SportsbookSettlementSimulationEnabled` — all confirmed at `cmd/platform-api/main.go:237-254`). This
  is the established idiom in this codebase; the design is consistent with it, not inventing a new
  gating mechanism.
- Casino webhook staying registered with a nil resolver (fail-closed 401, not absent) is correct
  because it is the real provider-facing route, distinct from KYC's player-self-service surface which
  has no real vendor at all. The asymmetry between KYC (absent) and casino (present, nil resolver) is
  intentional and justified, not an inconsistency to flag.
- `mockProviderWiring(cfg)` as an extracted, unit-testable function (K11 tests) is the right shape for
  this — matches how `TestSupportRoutesEnabled()`-derived flags are already computed inline in `main.go`
  today; extracting to a function only for the *provider wiring* (not the simpler boolean flags) is a
  proportionate, minimal change.

## KYC 200→204 change (breaking?)

- Not breaking for any legitimate client: the webhook is a provider→platform callback, not a
  client-facing read endpoint. The only consumer of the KYC webhook response body is the calling
  "provider" (today, only the mock/test harness itself, since KYC-WH-1's whole point is that a player
  can forge this call). No production or B2C code path parses this response.
  `internal/kyc/mock_test.go`, `kyc_integration_test.go`, `kyc_flow_integration_test.go` are already
  flagged in the design (§B1, §H) for rewrite — that covers the only real consumers.
  OpenAPI already advertises 204 (verified above), so 200→204 is fixing a spec violation, not
  introducing one.

## Player KYC schema split / provider_reference (§B5)

- Grepped `b2c/src` for `provider_reference`/`providerReference`: **zero matches.** The B2C client does
  not read this field today, so removing it from `PlayerVerification` is safe with no B2C client
  change required. `backoffice/src` (`kyc.ts`, `KycCaseDetail.tsx`) does use it — unaffected since the
  design keeps it on the staff `Verification` schema. This closes the "leaky success response" finding
  (exposing `provider_reference` to players was the actual capability-forgery vector in KYC-WH-1) without
  a client migration.

## OpenAPI / contract tests (§G)

- KYC rewrite (`security: []`, header patterns, 204, `PlayerVerification` split) and new casino webhook
  entry are both correctly scoped as additive/corrective, not breaking existing consumers per above.
- New `openapi_kycwebhook_contract_test.go` / `openapi_casinowebhood_contract_test.go` modelled on the
  existing payments one is the right pattern — keeps contract-test coverage uniform across the three
  webhook domains rather than leaving KYC/casino as the only untested contracts.

## PAYWH-GATE-1 (payments mock resolver wired unconditionally in production)

**Recommendation: include in 10.2, as a small follow-up commit after the extraction — do not defer.**
Reasoning:
- Same class of finding as what's already mandatory in this stage: an always-present mock credential
  path in production, differing from KYC/casino's gating only in that the key currently never leaves
  the process (so today it's not exploitable) — but "not exploitable today because of an incidental
  property" is exactly the pattern KYC-WH-1 punished.
- The fix is mechanical and already specified: reuse the identical `cfg.TestSupportRoutesEnabled()`
  gate the design already applies to casino's resolver — no new design work, no new interface.
- It closes the environment-gating matrix (§D) to be fully uniform across all three domains in the same
  stage, which reduces the chance of this being rediscovered as a fourth near-duplicate finding in a
  future stage.
- Cost is low: tests already "wire the resolver explicitly" per the design's own note, so behavior
  under test is unaffected; only `main.go` wiring changes.
- If deferred anyway, it must be filed under `PAYWH-GATE-1` (as the design already proposes) with an
  explicit owner and target stage — do not leave it as an undocumented residual gap.

## Nothing requiring escalation to architect/ledger-finance found

No change here alters tenant-isolation mechanism (RLS unchanged, §G confirms no migration), no new
service boundary is introduced (extraction is a library, not a service), and nothing touches the
ledger account model. Within backend's authority to review/approve at the design level.
