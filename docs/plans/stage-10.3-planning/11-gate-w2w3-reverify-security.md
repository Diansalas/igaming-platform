# Gate 10.3-W2/W3 — Security re-verification of the close-out

- Reviewer: `security` specialist
- Date: 2026-09-26
- Branch: `claude/focused-wright-jw88w9`, HEAD `e5b6e17`
- Reviewed commits: `040329a` (W2A-SEC-1/-2, S-1, S-2, S-5, awssm wiring),
  `cf775ef` (S-3, S-4), `e5b6e17` (go.mod tidy, ci.yml comment)
- Prior review: `10-gate-w2w3-review-security.md` (and
  `09-gate-w2-review-security-w2a.md` for W2A-SEC-1/-2)
- Mode: read-only. The only file written in the repository is this one.
  Probes ran in a `git archive HEAD` copy in the session scratchpad. Nothing
  was committed. No AWS call and no deploy. No secret appears here.

## Verdict

**APPROVE WITH CONDITIONS.**

- Every prior condition is **CLOSED** in code. The two that blocked wiring
  (S-1, S-2) are among them, so wiring `awssm` into `cmd/platform-api` is
  approved.
- One new **Low** finding (N-1) must be fixed before `awssm` is first used in
  staging. It does not block this gate.
- One **evidence gap** remains on S-3: I could not produce a clean govulncheck
  run myself (`vuln.go.dev` returns 403 through the egress proxy). A green CI
  govulncheck run on `e5b6e17` must be attached to the gate record.
- **Launch-blocking flags, all unchanged:**
  - PROV-OUTBOUND-CRED-1 precondition (W2A-SEC-1);
  - HD-10.3-2 (IAM, the `HTTPS_PROXY` decision, IRSA);
  - staging drills;
  - branch protection on `.github/`.

## Evidence I ran

- `go version` in the repo is go1.26.8, which is the toolchain pin.
- `go vet ./...` exited 0.
- `go test -count=1 ./...` (unit) passed for every package.
- With the integration tag, `go test` passed for:
  - `./internal/providercred/`
  - `./internal/secretstore/...`
  - `./cmd/platform-api/`
- Verbose runs of `TestResolveAndVerify_LogsMatchedKeyIDForKeyImplicit` passed
  in payments, kyc and casino, each with three subtests:
  - active key signed;
  - predecessor key signed;
  - failed verification logs nothing.
- `go mod tidy -diff` is clean.
- The CI grep, simulated on tracked files
  (`git grep '"github.com/aws/'` outside `internal/secretstore/awssm/`), finds
  no offenders.
- `govulncheck@v1.8.0` built with go1.26.8, but the run failed: `vuln.go.dev`
  returned 403. **Not verified by me.**
- Two probe tests ran in the scratch copy, using the package's own tripwire
  HTTP client:
  - `AWS_DEFAULTS_MODE=auto`: `New` issued **6 requests to 169.254.169.254**
    (about 4 s) before returning. This is N-1.
  - `AWS_CONTAINER_AUTHORIZATION_TOKEN` + `AWS_MAX_ATTEMPTS`: `New` succeeds.
    The token is ignored (see the S-1 section below).
- I read the SDK source in the module cache for these pinned versions:
  - `config@v1.28.6` (`env_config.go`, `config.go`, `shared_config.go`,
    `resolve.go`, `defaultsmode.go`);
  - `credentials@v1.17.47/endpointcreds`;
  - `aws-sdk-go-v2@v1.32.6/aws/transport/http`.

## Per-condition status

| ID | Sev (orig) | Status | Basis |
|---|---|---|---|
| W2A-SEC-1 | Medium | **CLOSED** | Registry row PROV-OUTBOUND-CRED-1 (`task-registry.md:3883`) is `PARTIALLY IMPLEMENTED` and carries the verbatim launch-blocking precondition and owners. The tripwire `cmd/platform-api/outbound_precondition_test.go`: (a) builds `buildProviderBundle(allOnWiring)`; (b) asserts that payments, casino and kyc each contribute an adapter, so it is not vacuous; (c) fails for any adapter that is not `providerkind.Synthetic` or that is `ProductionEligible`, and points at the row. |
| W2A-SEC-2 | Low | **CLOSED** (residual Info, see below) | `webhookauth.LogVerifiedKey` is called in `resolveAndVerify` in all three domains, only after `VerifyInbound` succeeds, only for `KeyImplicit`. It logs exactly `request_id`, `tenant_id`, `provider_id` and `key_id`. `key_id` is `verified.KeyID`: the stored credential that `VerifyInbound` mapped from the scheme's match (`scheme.go:720-726`). It is never the header value or a value the scheme returns. `AssertVerifiedKeyLog` pins the exact attribute set and scans for the secret (raw, hex, HEX, base64, base64url) and the fingerprint. `SetWebhookLogger` is wired for payments (`main.go:196`), casino (`main.go:227`) and kyc (`wiring.go:80`). `webhookauthtest` is not linked into any binary (`go list -deps ./cmd/...`). |
| S-1 | Medium, blocked wiring | **CLOSED** | See the S-1 section below. |
| S-2 | Medium, blocked wiring | **CLOSED** (HD-10.3-2 items remain open by design) | See the S-2 section below. |
| S-3 | Medium | **CLOSED in code; CI evidence required** | `go.mod`: `go 1.26.0` + `toolchain go1.26.8`. The Dockerfile builder is `golang:1.26.8-alpine`, so the CI and ship toolchains are the same. `golang.org/x/text v0.42.0` and otel `v1.46.0` are bumped. I could not confirm a clean govulncheck run offline. |
| S-4 | Low | **CLOSED** | `ci.yml:205`: `govulncheck@v1.8.0` (exact version). The version exists in the module cache. |
| S-5 | Low | **CLOSED** | The AST guard prefix is `github.com/aws/`. `TestImportBoundary_CoversSmithyAndV1SDK` catches smithy-go and the v1 SDK and does not flag `github.com/awslabs/`. The CI grep (`ci.yml:138`) uses `'"github.com/aws/'`. The AST scan includes `_test.go` files. |

### S-1: credentials by allow-list

I verified these points in the code (`internal/secretstore/awssm/awssm.go`):

- **Provider set explicitly.** `New` sets the credential provider explicitly,
  with `WithCredentialsProvider`: `aws.NewCredentialsCache(containerOnlyProvider{endpointcreds.New(...)})`.
  - The SDK resolves the credential provider from `LoadOptions` first, and
    `wrapWithCredentialsCache` keeps an existing `*CredentialsCache`.
  - So the env/shared/SSO/process/web-identity/IMDS chain is never built.
  - `New` also re-checks this identity after load
    (`awsCfg.Credentials != creds`), and refuses if it does not hold.
- **No shared files read.** `WithSharedConfigFiles([]string{})` and
  `WithSharedCredentialsFiles([]string{})` are non-nil empty slices. I traced
  the SDK code: `LoadOptions.getSharedConfigFiles` returns `found=true`, the
  env config (`AWS_CONFIG_FILE`/`AWS_SHARED_CREDENTIALS_FILE`) is never
  consulted, and `LoadSharedConfigProfile` substitutes defaults only for a
  `nil` list. So no shared file is ever read, even if those env vars are set.
  Preflight refuses them anyway when they point at an existing file.
- **Fixed ECS endpoint.** The endpoint is fixed to `http://169.254.170.2`.
  - Only `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` comes from the environment.
    The regex `^/[A-Za-z0-9._~-][A-Za-z0-9._~/-]{0,511}$` excludes a leading
    `//`, `@`, `:`, `?` and `#`.
  - `url.Parse` then re-checks host, userinfo, query and fragment. I found no
    way to change the host.
- **No proxy on the credential client.** The credential HTTP client is
  `NewBuildableClient().WithTransportOptions(tr.Proxy = nil)`. The SDK's
  `WithTransportOptions` clones and applies the option (`client.go:105-115`),
  so `HTTP(S)_PROXY` cannot see or intercept the task-role fetch.
- **Container token env vars ignored.** `endpointcreds.New` reads no
  environment. `AWS_CONTAINER_AUTHORIZATION_TOKEN` and `_TOKEN_FILE` are read
  only by `config`'s `resolveLocalHTTPCredProvider`, which is not on this path.
  The token is therefore never sent, and it is not needed for the ECS relative
  URI. My probe confirms that setting it has no effect.
- **Source checked on every retrieval.** `containerOnlyProvider.Retrieve`
  refuses any `Source` other than `endpointcreds.ProviderName` on every
  retrieval. It sits under the cache, so every refresh passes through it.
  Given the fixed inner provider this check is tautological today, but it is a
  correct guard against a future refactor. ADR 0093 §6 forbids network at init,
  so checking per retrieval rather than at `New` is the right placement.
- **Aliases refused.** `AWS_ACCESS_KEY`, `AWS_SECRET_KEY` and
  `AWS_DEFAULT_PROFILE` are refused, and tests cover them
  (`TestAWSSM_S1_StaticCredentialAliasesRefused`,
  `_ContainerProviderRequired`, `_UnauthorizedCredentialSourceRefused`).

### S-2: redirection and trust roots

Refused in preflight:

- `SSL_CERT_FILE` and `SSL_CERT_DIR` (the only trust-root env vars Go's
  `crypto/x509` honours on Linux);
- `AWS_CA_BUNDLE`;
- `AWS_ENDPOINT_URL`, and every `AWS_ENDPOINT_URL_*` by prefix over
  `os.Environ()`;
- `AWS_CONTAINER_CREDENTIALS_FULL_URI`;
- `AWS_EC2_METADATA_SERVICE_ENDPOINT`;
- `AWS_WEB_IDENTITY_TOKEN_FILE`;
- `AWS_ROLE_ARN`.

Error messages name the variable, never its value.

`ParseRef` now requires a full ARN:
`arn:aws(-cn|-us-gov)?:secretsmanager:<region>:<12 digits>:secret:<name>`.

- Tests cover a bare name, a wrong partition and `versionStage`.
- Registration (`service.go:266`, `:501`) and every resolve
  (`resolver.go:199`) go through `ParseRef`.
- `Ref` has only unexported fields, so no other constructor exists.
- The looser 0096 CHECK is therefore not reachable as a bypass.

Open by design under HD-10.3-2, and recorded in ADR 0093 (around line 614)
and in the SECRETSTORE-AWS-1 row:

- `HTTPS_PROXY` for the Secrets Manager client;
- IRSA/web identity (refused until decided).

### Bypass sweep: other environment variables the SDK honours

| Env var | Effect under the current `New` | Disposition |
|---|---|---|
| `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_PROFILE`, `AWS_DEFAULT_PROFILE` | Refused, and the files are never read (empty lists) | OK |
| `AWS_CA_BUNDLE`, `SSL_CERT_FILE`, `SSL_CERT_DIR` | Refused | OK |
| `AWS_CONTAINER_AUTHORIZATION_TOKEN(_FILE)` | Ignored (not on this code path) | OK |
| `HTTP(S)_PROXY` → container endpoint | Proxy forced to nil | OK |
| `HTTP(S)_PROXY` → Secrets Manager | Honoured. TLS is end-to-end against system roots, and the trust-root overrides are refused, so a proxy sees only CONNECT host/SNI | Open (HD-10.3-2), Info N-2 |
| `AWS_USE_FIPS_ENDPOINT`, `AWS_USE_DUALSTACK_ENDPOINT` | Selects another AWS-owned `amazonaws.com` endpoint; TLS verified | Not a redirection; acceptable |
| `AWS_REGION`/`AWS_DEFAULT_REGION` | Overridden by the explicit `WithRegion` | OK |
| `AWS_DEFAULTS_MODE=auto` | **IMDS network calls at `New`** | **N-1** |
| `AWS_MAX_ATTEMPTS`, `AWS_RETRY_MODE` | Ambient retry tuning under the breaker | Part of N-1 |
| `GODEBUG` | No setting disables certificate verification | Info |

### Other points checked

**Can the fake SDK constructor be reached outside tests?**
- `NewWithSDKFake` is exported and is linked into `platform-api`, because
  `registrations.go` imports `awssm`.
- It has no non-test caller. `TestNewWithSDKFake_OnlyFromTests` flags any
  selector *or* identifier reference, not only calls, in non-test files.
- `awssm.New` is referenced only from `cmd/platform-api/registrations.go`, and
  never from a test outside the package
  (`TestAWSSM_NewNeverCalledFromTestsOutsideThisPackage`).
- A misuse would still fail closed: it runs the full preflight and always
  returns `not_found`.

**Can any test reach the network?**
- Tests inside the package that call `New` get the `TestMain` tripwire client
  for **both** the credential endpoint and Secrets Manager. The tripwire
  answers only the synthetic ECS document and refuses everything else.
- The `cmd/platform-api` tests use only `withCredentialSubsystemUsing(...,
  awssm.NewWithSDKFake)`. No test in the main tree calls
  `withCredentialSubsystem`.
- The only network-capable path I found is N-1. Under a test it hits the
  tripwire, not the network. That is how my probe observed it.

**Wiring**
- `awssm` is constructed only when `SECRETSTORE_BACKENDS` names it,
  `ValidateSecretBackendScheme` passes (APP_ENV must be *explicitly* staging or
  production), and `ValidateSecretStoreAWSRegion` passes.
- Any error refuses startup.
- The store is registered as `ProductionEligible` through `b.SecretBackends`.
- When it is not configured, it is never constructed.

**Could the logged `key_id` ever carry secret material?**
- Only through operator misuse. The value is the stored credential's key id,
  bounded by the Go pattern `^[A-Za-z0-9._-]{1,64}$` (`providercred/service.go:85`)
  and the DB CHECK (`0096 … :149, :216`).
- It is staff-visible handle metadata by design, and never a request value.

## New findings

**N-1 (Low; fix before the first staging use of `awssm`; not gate-blocking):
ambient SDK tuning can make `New` do network I/O and change retry
behaviour.**
- Location: `internal/secretstore/awssm/awssm.go`, `loadOpts` in `New`.
- The problem: `LoadDefaultConfig` still honours the env config for defaults
  mode and retries. With `AWS_DEFAULTS_MODE=auto`,
  `resolveDefaultsModeOptions` (`config@v1.28.6/resolve.go:342-353`) calls IMDS
  `GetRegion` during `New`.
- Probe result: 6 requests to 169.254.169.254, about 4 s startup delay, and the
  error is swallowed.
- Consequences:
  - It contradicts ADR 0093 §6 (no network at init) and the comments in
    `registrations.go` and `awssm.go` ("awssm.New makes no network call").
  - In production that request uses the Secrets Manager client's default
    transport, which honours `HTTP_PROXY`. So a proxy could see and answer an
    IMDS request. The answer affects only the defaults-mode classification
    (timeouts), not credentials or endpoints, hence Low.
  - Separately, `AWS_MAX_ATTEMPTS`/`AWS_RETRY_MODE` let ambient config change
    the SDK retries under the `secretstore` breaker. This is the carry-forward
    item in §7 of the prior review.
- Fix:
  - Pin `awsconfig.WithDefaultsMode(aws.DefaultsModeStandard)`, or refuse
    `AWS_DEFAULTS_MODE`.
  - Pin the retryer explicitly (`WithRetryMaxAttempts`/`WithRetryMode`, or
    `WithRetryer`) to a value chosen against the breaker budget.
  - Add a tripwire test: `AWS_DEFAULTS_MODE=auto` → `New` makes zero requests.

**N-2 (Info): the Secrets Manager client honours `HTTPS_PROXY`.**
- This is recorded as open under HD-10.3-2. With the trust-root overrides
  refused, the exposure is metadata only (host/SNI, timing).
- The human decision is still required before production. My recommendation
  is unchanged: use the VPC endpoint and do not honour a proxy for this client.

**N-3 (Info): the account in the ARN is pinned per ref, not against
platform configuration.**
- A staff member with registration rights could point a handle at a secret in
  another account. That account would have to grant this task role access via a
  resource policy and a KMS key policy.
- This does not widen what a registering staff member can already do, since
  they supply the credential anyway.
- Defence in depth for HD-10.3-2: scope the task-role IAM policy's `Resource`
  to the platform's own account and the `provider-creds/` prefix. Optionally
  add a configured expected-account check in `ParseRef`/`Get`.

**W2A-SEC-2 residual (Info):**
- My W2 review asked for a `matched_predecessor` bool. The line carries only
  `key_id`, which meets ADR 0022 §3 point 2 ("which key_id verified").
- Operators must compare it with the active key id to see predecessor use.
- Consider adding the bool before the first `KeyImplicit` scheme is
  registered. It is not a condition.

## Conditions carried forward (unchanged; not re-reviewed)

- R-1, R-2 and R-3, and the doc-drift items in §3 and §5 of
  `10-gate-w2w3-review-security.md`.
- The PROV-OUTBOUND-CRED-1 launch-blocking precondition.
- HD-10.3-2 (IAM/KMS, `HTTPS_PROXY`, IRSA).
- DEPLOY-FPKEY-1.
- Branch protection plus CODEOWNERS on `.github/`, to be confirmed with the
  human.

## Required tests (security specification) to add with N-1

1. With `AWS_DEFAULTS_MODE=auto` set (and with `AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE=IPv6`),
   `New` makes zero requests through the tripwire. If the refusal option is
   chosen, `New` refuses instead.
2. With `AWS_MAX_ATTEMPTS=50` set, the constructed client uses the pinned retry
   budget, not 50.

## Scope statement

**In scope:**
- code-level review of `040329a`, `cf775ef` and `e5b6e17` against every prior
  condition;
- reading the pinned SDK source for how it handles environment variables;
- local unit and integration test runs;
- two local probes against the package's own tripwire.

**Not in scope:**
- live AWS behaviour and the IAM/KMS design (HD-10.3-2);
- a govulncheck result (egress returns 403);
- CI run status;
- repository settings;
- W2b/W3a code, which was already approved and is unchanged here.

Passing this re-verification does not make `awssm` "secure" in general. It must
be exercised in staging (STAGING REQUIRED drills) before any production use.
