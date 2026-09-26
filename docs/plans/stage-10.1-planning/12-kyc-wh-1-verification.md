> Stage 10.1 — specialist working paper (verbatim, 2026-09-26). Orchestrator rulings in the Stage 10.1 completion report govern.

# KYC-WH-1 verification (read-only)

## Claim 1: HMAC secret is a hardcoded literal constant
CONFIRMED. `cmd/platform-api/main.go:43` defines `kycMockWebhookSecret` as a
literal string constant (comment at :38-42 acknowledges it's dev/test-only).
Used at :262 to construct `kyc.NewMockKYCProvider(kycMockWebhookSecret)`.
Signature check itself (`internal/kyc/mock_provider.go:139,163`) uses
`hmac.New(sha256...)` + `hmac.Equal` (constant-time compare) — the *crypto*
is correct; the defect is that the secret is public source, identical on
every deployment, so anyone who reads the repo can compute a valid
signature for any payload.

## Claim 2: mock provider + webhook route registered with no environment gate
CONFIRMED. `main.go:261-263` registers `KYCOrchestrator` with the `"mock"`
provider unconditionally — no `cfg.TestSupportRoutesEnabled()` or
`cfg.Environment` check, unlike the three sibling simulation flags just
above it (`PaymentsMockSettlementEnabled`, `AccountActivationTestSupportEnabled`,
`SportsbookSettlementSimulationEnabled`, all gated at :233/:238/:244 by
`cfg.TestSupportRoutesEnabled()`). `internal/httpserver/kyc_routes.go:42`
registers `POST /v1/webhooks/kyc/{tenantSlug}/{providerID}` with no gate and
no bearer-token middleware (by design, matching casino/payment webhook
pattern — provider auth is the handler's own job, but here that job is done
with the public constant above). Confirmed identical at `git show
9190d5d:cmd/platform-api/main.go` — same unconditional registration,
same constant. So this is not new/regressed at 9190d5d; it's long-standing.

## Claim 3: player is shown provider_reference
CONFIRMED. `internal/httpserver/kyc_handlers.go:43,51-52` — `verificationResponse.ProviderReference`
is populated on every `newCreateMyVerificationHandler`/`newListMyVerificationsHandler`
response, i.e. a player calling `POST /v1/me/kyc/verifications` learns the
`provider_reference` the mock provider will expect in a signed webhook body.

## Exploit precondition / impact
Precondition: attacker is an authenticated player, reads `provider_reference`
from their own `POST /v1/me/kyc/verifications` response, then computes
`HMAC-SHA256(kycMockWebhookSecret, body)` (secret is public in this repo's
source) and POSTs a forged "approved" callback to
`/v1/webhooks/kyc/{tenantSlug}/mock`. `ReceiveCallback`
(`internal/kyc/provider.go:153`) has no additional binding beyond
provider_reference + valid signature, so this flips the verification to
`StatusApproved` (`internal/kyc/types.go:32`).

Impact today: CONFIRMED capability to self-approve KYC via forged webhook.
However, checked whether approved KYC currently *gates* anything financial:
grepped `internal/payments/*.go` for `kyc.`/`KYC` — **no matches**. No
withdrawal/deposit-tier code currently consults `kyc.StatusApproved` or
`HasVerifiedResidence`. So as implemented today, self-approving KYC does
not yet unlock a withdrawal or deposit tier — there is no wiring from KYC
status to payments in this codebase state (checked via Grep, no vendor
integration exists that would enforce it either). The exposure today is:
(a) a false compliance record (verification shows staff-equivalent
"approved" with no real review — an audit/compliance-integrity problem
regardless of payments wiring), and (b) a latent trap for whenever payments
work does wire KYC gating in, since the forgery path would then silently
unlock real withdrawal tiers. Severity stands as reported (High) on
audit-integrity + latent-financial-bypass grounds, not on a *currently
active* withdrawal bypass — this nuance should be reflected in remediation
priority discussion, not used to downgrade.

## Env/config gate check
`cfg.TestSupportRoutesEnabled()` (`internal/config/config.go:467-469`)
returns `Environment != "production" && TestSupportEndpointsEnabled`, and
`Load()` refuses to start if `TestSupportEndpointsEnabled` is true in
production. This gate exists and works correctly for the 4 flags it
protects — but the mock KYC provider/webhook route registration is **not**
one of the things gated by it. No `APP_ENV`/ADR-0085-style check of any
kind wraps `KYCOrchestrator` registration or `registerKYCRoutes`. So the
gate that would prevent this in staging/production exists in the codebase
pattern but was never applied here — this is the actual gap, not a broken
gate.

## Staging deployment (commit 9190d5d)
`git show 9190d5d:cmd/platform-api/main.go` shows byte-identical
`kycMockWebhookSecret` constant and identical unconditional
`KYCOrchestrator` registration (lines ~38-43, ~261-263) as HEAD. This is a
pre-existing condition, not something introduced by a later commit. If
9190d5d is what's actually deployed to staging (per git history — not
verified against AWS, per instructions), staging is affected: the mock KYC
provider and forgeable webhook are live there under real network reachability, independent of `TestSupportEndpointsEnabled`.

## 404/400 tenant-enumeration oracle
Not independently re-verified line-by-line in this pass beyond confirming
the code path (`kyc_admin_handlers.go:424-441`): unknown tenant slug and
suspended tenant both return 404 "not found" (good, matches enumeration-
resistance rationale in the comment) — but did not diff this against an
actual 400-response code path elsewhere to confirm a 404 vs 400
inconsistency exists; flagging as PARTIALLY VERIFIED, needs the actual
diverging code path (e.g. malformed vs unknown slug) identified before
calling it CONFIRMED.

## OpenAPI 204 vs 200 mismatch
NOT INDEPENDENTLY VERIFIED in this pass — handler returns
`writeJSON(w, http.StatusOK, ...)` (200) at `kyc_admin_handlers.go:479`,
consistent with the design doc's claim that spec says 204 and code returns
200. Did not open the OpenAPI spec file itself to confirm the 204
expectation; treat as CONFIRMED on the code side (200 emitted), spec-side
value not directly inspected here.

## CAS-WH-TENANT-1 (brief check)
Same architectural pattern applies: `internal/httpserver/casino_routes.go:56`
registers `POST /v1/webhooks/casino/{tenantSlug}/{providerID}` with no
bearer middleware, mirroring the KYC webhook's documented rationale
(provider auth is the handler's own responsibility). Did not check whether
casino's mock webhook secret is similarly a hardcoded literal or whether
casino settlement is gated by `TestSupportRoutesEnabled` — that's outside
this task's KYC/identity scope and belongs to whichever specialist owns
casino/payments; flagging as OUT OF SCOPE for this identity/compliance
verification, not independently confirmed or refuted here.

## Minimal containment options (description only, not applied)
1. Gate `KYCOrchestrator` mock registration and `registerKYCRoutes`'
   webhook line behind `cfg.TestSupportRoutesEnabled()` (or a narrower
   KYC-specific env flag), matching the existing 4-flag pattern — removes
   the route entirely outside non-prod test-support deployments.
2. Independently of (1): stop returning `provider_reference` to the player
   in `verificationResponse` (or only return it once a per-verification,
   server-generated capability token is also required in the webhook body)
   — removes the enumeration/self-signing precondition even if the mock
   route stays reachable for legitimate sandbox testing.
3. Require a real per-tenant, per-environment webhook secret sourced from
   `internal/config` (never a compiled constant) even for the mock
   provider, so a leaked/public repo does not equal a leaked staging
   secret.
4. Record whichever of the above is chosen as an explicit decision
   (docs/decisions/) rather than a quiet code change, since this is
   enforcement-affecting per CLAUDE.md.
