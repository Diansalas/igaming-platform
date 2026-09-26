# Gate 10.3-W1 — security review (`security`)

- **Scope:** the diff `4a2a978..ee2192f`. HEAD is now `240aa73`, but the two commits after `ee2192f` are
  docs only (the identity-compliance and ledger-finance W1 reviews).
  - W1a WH-VENDOR-SCHEME-1: `internal/webhookauth` (including `scheme.go`, `mock_scheme.go`,
    `webhookauthtest`, `constant_time_lint_test.go`), the three `webhook_verify.go` files and
    `internal/httpserver/webhook_preamble.go`.
  - W1b MOCK-ADAPTER-PROD-1: `internal/providerkind`, `config.GuardEnvironment` and
    `ValidateSecretBackendScheme`, and `cmd/platform-api` (`registrations.go`, `main.go`, `wiring.go`).
  - W1c: security aspects only (the kill-switch change and C14).
  - W1d: KYC-REASON-BOUND-1.
- **Binding references:** ADR 0092; the ADR 0022 §3 Stage 10.3 amendment; the ADR 0085, 0028 and 0025
  amendments; ADR 0093 (W1b's backend allow-list feeds it); my planning review `04-review-security.md`
  (C1–C17); `05-gate-log.md`; `evidence/w1a-mutation-kill.txt`.
- **What was run:**
  - `go vet ./...` and `go vet -tags integration ./...`: both clean.
  - Unit tests (no `integration` tag): `internal/webhookauth/...`, `internal/providerkind`,
    `internal/config`, `cmd/platform-api`, `internal/kyc`, `internal/payments`, `internal/casino`,
    `internal/httpserver`. All pass.
  - One probe test, compiled in through `go test -overlay` so that no file in the tree was touched
    (evidence for F1).
  - Per instruction, no integration test was run. The integration-tagged guards (M18–M22 and the W1b
    subprocess ordering test) are accepted on the basis of the mutation record and code reading.
- **Mutation record provenance:** `evidence/w1a-mutation-kill.txt` names HEAD `82cdf98`, a worktree
  commit on no branch. `git diff 82cdf98 ee2192f` over every W1a file differs only by the W1b
  `SyntheticComponent` marker in `internal/webhookauth/mock.go`. The record is therefore valid for the
  code under review.
- **Constraints kept:** no code was edited, nothing was committed, and this file is the only one written.

## Verdict: **APPROVE WITH CONDITIONS**

The core trust-boundary change is correct, and it is enforced where it should be:
- the orchestrator runs Verify itself;
- key selection comes from `Properties()`;
- the timestamp window has its own reason;
- startup validation refuses a bad declaration;
- the production guard is fail-closed, with a positive marker.

No finding lets an unauthenticated caller get a callback accepted, and no finding lets tenant A's
credential verify at tenant B's route.

There is one **Medium** finding, F1. The "Synthetic" exemption is bound to a Go type rather than to the
three platform MOCK schemes. Any package can therefore build a timestamp-less, manifest-exempt scheme
that the production guard never sees. This breaks the premise of ADR 0022 point 10's MOCK exemption.
The fix is small, and it must land before W2a code merges.

The remaining findings are **Low** or **Info**. No finding needs a human decision. Nothing found here
blocks W2 from starting.

---

## 1. Verification points requested

| # | Question | Ruling | Evidence |
|---|---|---|---|
| V1 | Can the orchestrator-enforced Verify be bypassed? | **No bypass found.** | The only non-test `HandleCallback` call sites are `internal/payments/orchestrator.go:899`, `internal/kyc/provider.go:285` and `internal/casino/orchestrator.go:694`. Each is reached only after `verifyCallback` returns without error (`payments/orchestrator.go:885`, `kyc/provider.go:274`, `casino/orchestrator.go:681`). The scheme is looked up from a set validated at construction. An `Orchestrator` built without `NewOrchestrator`, or a provider added to the map later, has no scheme, so it fails closed as `provider_unregistered` (`payments/webhook_verify.go:62-66` and its KYC/casino equivalents). A panicking scheme is a failure (`scheme.go:448-452`, `557-564`). A wrapped error folds to `signature_invalid` (`scheme.go:568`). The matched key id must belong to the set (`scheme.go:573-580`). M18–M20 kill "delete the Verify call" in each domain. |
| V2 | Does key selection come only from Properties (C3)? | **Yes.** | `ExtractInbound` (`scheme.go:460-476`) and `ResolveCredentials` (`scheme.go:494-519`) switch on `Properties().KeySelection` only. For `KeyFromHeader`, an empty key id gives `signature_missing` and the resolver is never called (M6, M7). `VerifyInbound` drops `Previous` for every non-`KeyImplicit` scheme (`scheme.go:550-553`, M8). `KeyImplicit` fails closed as `credential_unavailable` until W2a (`scheme.go:512-516`). SC6 and the `MultiKeyTrialOnAbsentKeyID` broken scheme cover this. |
| V3 | Is `timestamp_out_of_window` its own reason (C10)? | **Yes**, with one small self-test gap (F2). | `ErrTimestampOutOfWindow` is a distinct sentinel, and `errors.Is` still treats it as a signature failure (`scheme.go:61-69`). It is mapped by identity only (`scheme.go:568-570`, M9). It is a member of the closed enum (`scheme.go:80-86`, M12). The response is unchanged: the preamble and handlers write the same 401. "Only when the MAC is otherwise valid" is a scheme contract. SC7 checks it with a stale-and-tampered request (`webhookauthtest.go:657-662`), but no broken scheme proves that assertion is load-bearing (F2). |
| V4 | Are Properties validated at registration, with startup failing otherwise (C11)? Is a panic in `NewOrchestrator` acceptable? | **Yes. The panic is acceptable.** | `ValidateProperties` (`scheme.go:200-247`) refuses an unknown binding, key selection or replay value; `MaxSkew <= 0`; `MaxSkew > 10 min`; a real scheme with no signed timestamp; and a real scheme declaring `SignedTenant`. `ValidateScheme` adds the conformance-manifest gate (`scheme.go:349-353`). `MustSchemeSet` panics (`scheme.go:389-395`, M1–M5). **The panic ruling:** the three constructors run in `run()` after `db.Connect` (`main.go:97`) and after the read-only `VerifyRuntimeRoleInProduction` (`main.go:119`). They run before the catalogue sync (`main.go:248`) and before `ListenAndServe` (`main.go:415`). Nothing writes before them. A panic exits non-zero, deferred `pool.Close` still runs, and the message names only the domain, provider id, scheme name and declaration. It carries no secret. The panic is an acceptable way to fail startup. It is not the preferred one: see I-1. |
| V5 | Is the conformance self-test complete (C9)? | **Complete against C9's list**, with two Low gaps (F2, F3). | There are 11 broken reference schemes, each red in exactly its own case (`webhookauthtest/conformance_test.go:84-112`). They cover every item on C9 point 2: SC2 prefix, SC3 tenant, SC4 provider, SC5 panic, SC6 multi-key trial, SC7 timestamp, SC8 account, SC9 `not_after`, SC10 empty secret, SC11 secret in error, and KAV for a shared Sign/Verify bug. The suite generates the tampering over the declared headers (SC2, `webhookauthtest.go:414-464`). A known-answer vector is mandatory for real schemes (M16). SC7 fails rather than skips for real schemes (M17). The registry test covers C9 point 4 (`registry_test.go`, M15). The constant-time AST rule exists and has its own self-test (M14), but its scope is too narrow (F3). The manifest binds a scheme **name**, not an implementation (I-5). |
| V6 | Is `MinSecretBytes = 16` acceptable? | **ACCEPTED as a floor, with conditions.** | See §3. |
| V7 | Does moving `provider_unregistered` ahead of the tenant lookup create an enumeration or oracle change? | **No response-level oracle. One timing residual, accepted as Info.** | See §4. |
| V8 | W1b guard: missing `APP_ENV` counts as production; no flag bypass; completeness; the `VerifyRuntimeRoleInProduction` call site | **Sound for today's wiring.** One completeness gap is latent until the first `ProductionEligible` component (F4). | See §5. |
| V9 | W1d: sanitisation coverage; no raw reason in logs, audit or player surfaces | **Player surface: clean. Logs: clean. Audit: bounded.** Sanitisation relies on adapter discipline (F5). The UI escaping test is missing (F6). | See §6. |
| V10 | W1c: is credential revocation the emergency stop, and is no stake stranded? | **Consistent with C14.** | See §7. |
| V11 | Were any secrets added? | **None.** | A grep of every added line for credential-shaped literals found only test placeholders. They are `JWTSigningSecret: "a-secret-that-is-at-least-32-characters-long"` in `cmd/platform-api/registrations_test.go` and the documented MOCK known-answer test key `payments-mock-kav-test-key-0123456789` in `webhookauthtest/conformance_test.go`. Neither is a real credential. No `.env`, `.pem` or key file was added. As C17 requires, this was a named grep over the diff, not a CI secret scanner; CI still has none. |

---

## 2. Findings

### F1 (Medium): "Synthetic" is bound to a Go type, not to the platform MOCK schemes

**Where:**
- `internal/webhookauth/scheme.go:345-353` (the `_, isPlatformMock := s.(mockVerificationScheme)` check);
- `internal/webhookauth/mock_scheme.go:20-22` (`Scheme.VerificationScheme()` is callable from anywhere);
- `internal/webhookauth/webhookauth.go:249-257` (`Scheme` has exported fields);
- `internal/payments/webhook_verify.go:16-26`, `internal/kyc/webhook_verify.go:21-31` and
  `internal/casino/webhook_verify.go:21-31` (no check that the adapter itself is synthetic).

**What is wrong.** `ValidateScheme` treats any `mockVerificationScheme` value as the platform MOCK. Any
package can build one with an arbitrary prefix and header names, for example
`webhookauth.Scheme{Prefix: "vendor.x.v1", SignatureHeader: "X-Vendor-Signature", KeyIDHeader: "X-Vendor-Key"}.VerificationScheme()`.
The result declares `Synthetic: true`, so it skips three controls:
- the conformance-manifest gate (C9 point 4);
- the mandatory signed timestamp (ADR 0022 point 10);
- the known-answer vector (KAV) requirement.

The W1b guard inspects adapter components only, never schemes. An adapter that implements
`ProductionEligible` but returns such a scheme therefore passes both gates and runs in production.

I confirmed this with an overlay probe (`go test -overlay`, no file in the tree touched):
- `NewSchemeSet("payments", {"vendor-x": <custom MOCK scheme>})` returns `err=<nil>`, with
  `synthetic=true` and name `platform-mock:vendor.x.v1`;
- `NewSchemeSet("payments", {"vendor-y": CasinoScheme().VerificationScheme()})` also returns `err=<nil>`.
  Another domain's MOCK scheme is accepted, which weakens point 8 domain separation down to key-label
  discipline alone.

**Concrete failure scenario.** An integrator writing the first real PSP adapter returns
`payments.WebhookScheme().VerificationScheme()` (or a custom MOCK-shaped `Scheme`) as a placeholder, then
marks the adapter `ProductionEligible` when the rest of it is done. The process starts in production:
`ValidateScheme` accepts the declaration as Synthetic, and `RefuseSyntheticInProduction` sees only an
eligible adapter. Verification then has no timestamp window, contrary to point 10, and no vendor
known-answer vector ever ran.

This does **not** allow forgery by itself. The MAC is still keyed by the resolved per-tenant secret over
a signed tenant id, and a real vendor would not produce MOCK-format signatures, so the likely outcome is
that every callback fails closed. What it defeats is the invariant the ADR 0022 amendment states: "the
MOCK is exempt only because it can never run in production". It also defeats the doc-comment promise at
`scheme.go:189-193`: "a real adapter cannot opt out of the timestamp rule or the conformance gate by
claiming to be a mock". That promise is currently false.

**Required fix (condition S-1):**
1. In `NewSchemeSet(domain, …)`, accept a Synthetic scheme only if its underlying `Scheme` equals that
   domain's own canonical MOCK: `PaymentsScheme()` for `payments`, `KYCScheme()` for `kyc` and
   `CasinoScheme()` for `casino`. An unknown domain refuses all Synthetic schemes.
2. In each `must{Payments,KYC,Casino}SchemeSet`, refuse a Synthetic scheme returned by an adapter that
   does not itself implement `SyntheticComponent()`, checked structurally without importing
   `providerkind`. This ties scheme exemption to the W1b guard: a synthetic scheme can then only arrive
   on a component the production guard refuses.
3. Add tests and mutation kills for both rules, covering a custom-prefix MOCK scheme, a cross-domain
   MOCK scheme, and a synthetic scheme on an unmarked adapter.
4. Correct the doc comment at `scheme.go:189-193` if its wording still over-claims.

**Binds:** before W2a code merges, and in any case before any type implements `ProductionEligible`.

### F2 (Low): no broken scheme proves the C10 "MAC first" assertion is load-bearing

**Where:** `webhookauth/webhookauthtest/refscheme_test.go:35-47` (the `refBugs` set),
`webhookauthtest/conformance_test.go:84-112`, and `webhookauthtest.go:657-662`.

**What is wrong.** The stale-and-tampered request in SC7 is the only check that a scheme returns
`signature_invalid`, not `timestamp_out_of_window`, when the MAC fails. No broken reference scheme
checks the timestamp before the MAC. Nothing therefore proves that this assertion would catch the bug.
It is the property that keeps `timestamp_out_of_window` a real replay signal rather than a free-text
signal an unauthenticated caller can trigger.

**Failure scenario.** A future edit weakens the stale-and-tampered check, for example by accepting any
`errors.Is(err, ErrSignatureInvalid)`, which is also true for `ErrTimestampOutOfWindow`. The self-test
stays green. A real scheme that checks the timestamp first then floods the logs with
`timestamp_out_of_window` for unauthenticated junk, which hides genuine replays.

**Fix (condition S-2):** add a `timestampBeforeMAC` broken scheme, expected red in SC7 only. **Binds:**
before the first non-synthetic scheme enters the manifest.

### F3 (Low): the constant-time rule selects files, not packages

**Where:** `internal/webhookauth/constant_time_lint_test.go:130-148` (`declaresScheme`) and
`:186-188` (the per-file selection).

**What is wrong.** Outside `internal/webhookauth`, a file is scanned only if it declares an `Extract`
method returning `AuthMaterial`.

**Failure scenario.** A real scheme is split into `scheme.go` (holding `Extract`) and `verify.go`
(holding `Verify` plus a `bytes.Equal(expectedMAC, got)` helper). `verify.go` is never scanned.

**Fix (condition S-3):** when any file in a package declares a scheme, scan every non-test file in that
package. Also add "signature/MAC comparison uses `hmac.Equal` only" to `.claude/agents/code-reviewer.md`
as a named checklist item. I found no such item there; C9 required one. **Binds:** before the first
non-synthetic scheme.

### F4 (Low, latent): the guard's completeness proof covers the bundle, not everything `main` wires

**Where:**
- `cmd/platform-api/registrations.go:23-78`;
- `cmd/platform-api/registrations_test.go:32-55`;
- `cmd/platform-api/wiring.go:62-110` (the three MOCK webhook credential resolvers);
- `internal/webhookauth/mock.go:93-99`.

**What is wrong.**
- The reflection test proves that every `providerBundle` field is registered. It does not prove that
  `main.go` and `wiring.go` construct nothing outside the bundle. C13 point 3 asked for "fails when a
  component is wired in main without passing through `buildRegistrations`".
- The three MOCK webhook credential resolvers are exactly such components. They are built in
  `wiring.go` and never registered. The `MockResolver.SyntheticComponent` doc comment
  (`mock.go:93-99`) says marking it "covers all three call sites", but nothing ever passes it to the
  guard.
- They are wired under `cfg.TestSupportRoutesEnabled()`, which reads `Environment`, not
  `GuardEnvironment()`. With `APP_ENV` missing and the test-support flag set, they are wired.

**Why it is not exploitable today.** The MOCK adapters those resolvers are bound to are always
registered, so the guard refuses startup first.

**Failure scenario (after a real adapter lands).** The payments mock is replaced by an eligible real
adapter, but a leftover MOCK resolver is still wired through `wiring.go`. The guard passes, because the
resolver is not registered. The MOCK resolver is bound to a MOCK provider instance with a random master,
so the most likely effect is failed callbacks rather than forgery. It is still a synthetic component
running in production, which is exactly what MOCK-ADAPTER-PROD-1 exists to prevent.

**Fix (condition S-4):**
1. Register every webhook credential resolver `main` wires.
2. Add an AST test: no non-test file in `cmd/platform-api` other than `registrations.go` calls a
   constructor or composite literal from the provider packages. The test reuses
   `forbiddenProviderImports`, minus the orchestrator constructors.

**Binds:** before any type implements `ProductionEligible`.

### F5 (Low): KYC reason sanitisation is adapter discipline, not platform-enforced

**Where:**
- the ingestion sites that persist or audit an adapter-supplied `result.Reason` with no platform-side
  normalisation: `internal/kyc/verification_service.go:112` (`CreateVerification` result),
  `internal/kyc/document_service.go:214,226` (`SubmitVerification` result) and
  `internal/kyc/provider.go:398,424,438` (`HandleCallback` result);
- `NormalizeReason` is called only inside the MOCK (`mock_provider.go`) and in `ReviewVerification`;
- the conformance case (`internal/kyc/conformance_test.go`, the reason-bound case) exercises
  `HandleCallback` only;
- the DB CHECK (`migrations/0095_…up.sql`) bounds length only, not control or bidi characters.

**What is wrong.** W1a has just moved Verify out of adapter discipline for exactly this reason, and the
same logic applies here.

**Failure scenario.** A real KYC adapter normalises in `HandleCallback`, which passes conformance, but
returns raw vendor text from `SubmitVerification`. Control and bidi characters are stored in
`kyc_verifications.reason` and in `audit_log.metadata`, and are shown to staff. A text longer than
512 bytes turns into a 500 caused by the CHECK constraint.

**Fix (condition S-5):** call `NormalizeReason` at those platform sites. It is idempotent, so a second
application by the adapter is harmless. Doing it there also produces the `truncated` flag at the point
where the audit row is written, which makes the identity-compliance W1 condition 1 (record truncation in
audit metadata) a local change. **Binds:** before the first real KYC adapter.

### F6 (Low): the staff-UI escaping test required by C16 was not delivered

**Where:** `backoffice/src/features/kyc/KycCaseDetail.tsx:62`, which has no test file.

**Why this is Low.** React escapes JSX text by default, and there is no `dangerouslySetInnerHTML`
anywhere under `backoffice/src`. There is therefore no live defect. C16 and ADR 0028 amendment item 5
both require a test, though.

**Fix (condition S-6):** add `KycCaseDetail.test.tsx`. It renders a reason of
`<img src=x onerror=alert(1)>` and asserts that the value appears as text and that no `img` element
exists. I concur with the identity-compliance W1 condition 2. **Binds:** before the Stage 10.3
completion gate.

---

## 3. Ruling on `MinSecretBytes = 16`

**ACCEPTED as the platform floor.**

**Reasons:**
- 128 bits is the minimum security strength currently recommended for symmetric keys.
- The platform does not choose most vendor secrets.
- A length check cannot measure entropy.
- A higher floor, such as 32 bytes, would refuse some legitimate vendors without any gain against the
  realistic threat.

**Where it is enforced:**
- by the platform, before any scheme runs (`credentialUsable`, `scheme.go:583-586`; M10);
- by every scheme independently (SC10; M13 for the MOCK).

**Conditions and disclosures:**
- **It is a floor, not a target.** Any secret the platform itself generates or negotiates (for example
  outbound or webhook secrets that W2a/W3b create) must be at least 32 random bytes, which equals the
  HMAC-SHA256 block-relevant output size. The MOCK already derives 32-byte keys. Record this in ADR 0093
  §4 with W2a.
- **Disclosed residual (Info).** A vendor secret delivered as an ASCII hex or base64 string and used
  as raw bytes carries about 4–6 bits per byte. A 16-character string can therefore be only 64–96 bits
  strong. The fingerprint-keying condition C6 (W2a) already assumes low-entropy vendor secrets. No
  further action now.
- Raising the floor needs only a constant change in a reviewed commit. No schema depends on it.

## 4. Ruling on the reason reordering and the two edited test assertions

**Response level: no change.** Every rejection before verification, including `provider_unregistered`,
still writes the identical 401 "callback rejected" with the same body. No new enumeration oracle
appears in the response.

**Timing level (Info, accepted residual I-2):**
- An unregistered provider id now returns with no DB statement.
- A registered provider id with well-formed headers pays one `GetTenantBySlug` query first.
- This distinguishes registered from unregistered **provider ids** by latency.

These ids are the process-wide adapter registry, the same for every tenant. They are already visible to
players through casino catalogue and launch data, so they are not secret. Tenant existence can no longer
be probed with an unregistered provider id, which is the narrowing my planning review predicted. The
residual is the same class as F-4 and R-2, and remains with PAYWH-RL-1.

**The two edited assertions:**
1. `internal/httpserver/payment_webhook_auth_failure_logging_integration_test.go`, around line 238:
   `tenant_id` and `key_id` changed from present to **absent** for `provider_unregistered`. This is the
   correct consequence: the rejection now happens before the tenant lookup and before any header is
   parsed. The assertion got stricter, since less unverified data is logged, and the reason assertion
   is unchanged.
   - Disclosed forensic cost (Info): the allow-listed log line carries no route path or slug, so a
     probe with an unregistered provider id cannot be attributed to a tenant. This is identical to
     every other rejection before the tenant lookup and is acceptable.
2. `internal/payments/webhookauth_alias_test.go`, around line 44: the test now registers `mock-psp` so
   that the header check is reached. It tests the same alias property (`errors.Is(…, ErrAuthFailed)`)
   and is not weakened.

## 5. W1b guard

**Missing `APP_ENV` counts as production: yes.**
- `Load` records `os.LookupEnv` presence (`config.go`, `EnvironmentExplicit`).
- `GuardEnvironment()` returns `production` when the variable is absent.
- An explicitly empty `APP_ENV` is already refused by `Load`'s closed-set validation.
- A hand-built `Config` defaults to `EnvironmentExplicit=false`, which also resolves to production:
  fail-closed.
- The matrix test covers {production, missing, staging, development}.

**Flag bypass: none.** `RefuseSyntheticInProduction(environment string, regs)` has no `Config` input.
`TestRefuseSyntheticInProduction_AllFlagsOn` covers every flag set to true and every flag set to false.

**Positive marker: adopted** (the preferred C13 option).
- A component must implement `ProductionEligible` and must not implement `Synthetic`.
- An unmarked component is refused.
- No type implements `ProductionEligible` today.
- The name-heuristic AST scan (`Mock|Fake|Stub|InMemory`) is a secondary net.

**Placement.** The guard runs right after `config.Load`, before the logger, `db.Connect` and the
catalogue sync (`main.go:48-68`). This is proven by the integration subprocess test, which was not run
here and is accepted on its reading. The same bundle instances are then reused, so nothing is built
twice.

**Other binaries.** `cmd/seed-admin` and `cmd/migrate` are held to "no provider imports" by an AST test.

**Backend allow-list.** `ValidateSecretBackendScheme` accepts:
- `awssm` everywhere;
- `devfile` only when `APP_ENV` is explicitly `development`;
- `memory` never.

This feeds ADR 0093 §6 correctly. Only the rule exists so far; W2a/W3b must actually call it before
building any backend, and I will check that at gate W2.

**The `VerifyRuntimeRoleInProduction` call site** (`main.go:119`) now passes `cfg.GuardEnvironment()`.
A production task missing `APP_ENV` is no longer exempt from the runtime-role check. The function itself
is unchanged. This is correct and fail-closed. Today the synthetic guard refuses such a task first
anyway.

**Completeness:** see F4.

**Residual (Info, I-3).** An `APP_ENV=staging` mistakenly set on a production task still allows
synthetic components. C13 accepted staging as a legitimate synthetic environment. The remaining control
is deployment configuration, not code. I recorded this in the planning review, and it is restated here
for the launch checklist.

## 6. W1d KYC reason

**Player surfaces: clean.**
- `playerVerificationResponse` has no `reason` or `reason_code` field (`kyc_handlers.go:74-92`).
- It is the only shape on the two player routes.
- The OpenAPI `PlayerVerification` schema drops the field.
- The integration test checks the raw JSON body (per identity-compliance's review; not re-run here).

**Logs: clean.** No KYC handler or service logger call carries the reason. The only logger lines log
`error` on 5xx paths, and none of those errors wrap the reason.

**Audit: bounded, provided the adapter normalised.** `audit_log.metadata.reason` receives the adapter's
value at `provider.go:398,438` and `document_service.go:214`, and the staff value at
`verification_service.go:393`.

**Sanitiser: correct and complete against C16.** `NormalizeReason` (`reason_normalize.go:91-112`):
- replaces invalid UTF-8 with U+FFFD;
- strips C0, DEL and C1 control characters;
- strips U+200B–U+200F, U+202A–U+202E and U+2066–U+2069;
- truncates to 512 bytes on a rune boundary.

Staff input is normalised in `ReviewVerification`, so it no longer produces a 500 on oversize input. The
migration backfill strips C0 characters and bounds length on existing rows (synthetic data only).

**Gaps:** F5 (platform enforcement) and F6 (UI test).

**Out of scope, as registered.** `kyc_documents.rejection_reason` remains KYC-DOC-REJECTION-BOUND-1.

## 7. W1c casino: kill switch and revocation (C14)

**Kill switch.**
- The capability now gates **new bets only**: `postBet`, resolved for the session's own brand
  (`casino/orchestrator.go:1087-1093`).
- A real-mode launch also requires `supports_bet` (`orchestrator.go:307`).
- Wins, rollbacks, replays and tombstones are not gated.
- A capability row that allows bets but not settlement is refused both by the app (`capability.go:117`)
  and by migration 0094's CHECK.
- The flipped test shows a verified rollback of an already-posted bet settles with 200 while the
  capability is disabled, returning the stake.

**No stake is stranded by the capability.** The only remaining stranding paths are the two C14
accepted:
- credential revocation, which is the emergency stop and deliberately strands open exposure;
- a suspended or closed tenant (HD-10.3-4 UNCHANGED, disclosed in the ADR 0025 amendment, with
  reconciliation as the detector).

**Revocation as the emergency stop.** It is recorded as C14 required (ADR 0025 amendment, around lines
447-472):
- single-actor, reason-coded, available to tenant and platform admins, never four-eyes;
- no settlement freeze;
- no real casino resolver or adapter until W2a revocation and its concurrency test exist;
- runbook requirements, including the dependency on LEDGER-MANUAL-ADJ-4EYES-1;
- CAS-WIN-ANOMALY-1 and PROV-REVOKE-ALL-1 registered (`task-registry.md:3893-3894`).

**Today there is no revocation mechanism at all.** The MOCK resolver derives keys, and the MOCK is
never production-eligible. That is why the C14(1) sequencing condition matters, and F1/S-1 closes the
remaining path for a synthetic scheme to reach production.

**The new 409 integrity branches** (`casino_handlers.go`, for `ErrOriginalTombstoned` and the §16.4
abort set) return a generic body with no reference echoed. They are reachable only after verification.
They log `err`, which may contain provider transaction or round references; these are internal logs and
not PII. Acceptable.

**Capability audit before/after** (`casino_admin_handlers.go`). The before state is read with a plain
SELECT in the same transaction, without a row lock. Under two concurrent writes, one audit row's
`before` can be stale (Info, I-4). The after state and the write are correct, and the gap is only in
audit fidelity. Adding a lock is optional.

## 8. Informational items (no condition)

- **I-1.** Prefer to validate the webhook schemes in the pre-DB guard phase, returning an error, over
  a panic after `db.Connect`. For example, build the three `SchemeSet`s from the bundle alongside
  `refuseSyntheticInProduction`. The panic is acceptable (V4); this is hygiene only.
- **I-2.** The timing residual on registered provider ids (§4).
- **I-3.** `APP_ENV=staging` mis-set on a production task (§5).
- **I-4.** The capability audit before-state read has no lock (§7).
- **I-5.** The conformance manifest binds a scheme **name**, not an implementation. Two different
  types sharing a manifest name would both register, and only the one in the fixture has run the suite.
  Before a second real scheme, consider keying by name plus a fixture-asserted concrete type, or
  asserting in `mustXSchemeSet` that the adapter's scheme type matches the fixture's.
- **I-6.** R-1 (a compile-time `Verified` token) was not adopted. That is acceptable: the orchestrator
  calls it, and the M18–M20 mutation kills, give runtime and test assurance instead.

## 9. Conditions

| # | Sev | Condition | Binds |
|---|---|---|---|
| S-1 | **Medium** | Synthetic schemes are restricted to the domain's canonical MOCK `Scheme`, and to adapters that implement `SyntheticComponent()`. Tests and mutation kills are required, and the doc comment at `scheme.go:189-193` must be corrected (F1). | Before W2a code merges; before any `ProductionEligible` type |
| S-2 | Low | A `timestampBeforeMAC` broken reference scheme, red only in SC7 (F2). | Before the first non-synthetic scheme |
| S-3 | Low | The constant-time rule scans whole packages, and the item is added to the `code-reviewer.md` checklist (F3). | Before the first non-synthetic scheme |
| S-4 | Low | Register the webhook credential resolvers, and add an AST test that `main` and `wiring` construct no provider component outside `registrations.go` (F4). | Before any `ProductionEligible` type |
| S-5 | Low | `NormalizeReason` applied at the platform KYC ingestion sites, so the truncation flag is available for the audit row (F5). | Before the first real KYC adapter |
| S-6 | Low | A staff KYC reason escaping test (F6, concurring with identity-compliance condition 2). | Stage 10.3 completion gate |

**Carried from planning, unchanged and not W1 scope:** C1, C2, C4–C8, C12, C15 and C17 (W2a/W3b);
R-2; F-5; and the launch-blocking list in `04-review-security.md` §8.

**Launch-blocking flags for the human (via the orchestrator).** There is nothing new. S-1 must be closed
before any real adapter is marked production-eligible, and that is already sequenced ahead of any
production deployment. The planning list stands: C1/C2 before any non-synthetic credential; C13, now
implemented, plus S-1 and S-4 before any `APP_ENV=production`; LEDGER-MANUAL-ADJ-4EYES-1; PAYWH-RL-1; the
STAGING REQUIRED drills; and CAS-WIN-ANOMALY-1 before real-money casino.

## 10. Scope of this review

**Covered:**
- code-level review of the W1a/W1b/W1d diff and of the security aspects of W1c;
- `go vet` in both tag sets;
- unit tests of the affected packages;
- one overlay probe (F1);
- reading of the integration tests and mutation evidence.

**Not covered:**
- integration test execution (deliberately not run; a DB-heavy agent is running);
- W1c financial correctness (owned by `ledger-finance`/`casino` in their W1 review);
- the backoffice beyond the KYC reason rendering;
- real vendor schemes (none exist);
- `deploy/` and AWS;
- volumetric DoS.

This is a design- and code-level review for a development-stage platform. It is not a penetration test or
a certification audit. Approval here does not make W2's credential, resolver or secret-store work secure:
that work needs its own `security` review at gate W2, including the two concurrences deferred from W0
(items 2 and 8 of `05-gate-log.md`).
