> Stage 10.2 design review — specialist working paper (verbatim, recorded 2026-09-26 against 69e80c1). Where it differs from the Orchestrator rulings in `01-webhook-trust-design.md` §J, the rulings govern.

# identity-compliance review — Stage 10.2 KYC-WH-1 design (01-webhook-trust-design.md)

**Verdict: CONCUR, with one required clarification (not a blocker) and two
confirmations recorded below.** No RG/KYC enforcement is weakened; design
strengthens enforcement. No code changed by this review.

## 1. Forward-only ordering — CORRECT, matches code

Verified against `internal/kyc/types.go:28-34` and `provider.go:199-201`:
`isTerminal = approved|rejected|expired`. `ReviewVerification`
(`verification_service.go:325-327`) already refuses any transition once
terminal (`ErrInvalidTransition`). `CreateVerification`
(`verification_service.go:87-131`) always inserts a **new row**; it never
mutates an existing one. So today, a rejected verification is *already*
un-reopenable except by a new verification attempt — the design's rank
(`unverified 0 < pending 1 < review_required 2 < {approved,rejected,expired}
3`) is consistent with this and adds nothing new for the terminal states;
it only newly enforces monotonicity for the **non-terminal** states
(blocking `review_required → pending` regression), which is the correct
target (§0 "Not monotonic" finding). **Sign-off: the order is correct.**
One nit for the design doc: state explicitly (as B7 implies but does not
say in one place) that a webhook callback can never resurrect a terminal
verification — only `POST /v1/me/kyc/verifications` (a fresh row) can.
Recommend adding this one sentence to B6/B7 so a future implementer
doesn't add a "reopen" code path by mistake.

## 2. Staff review path as the acceptance path — ADEQUATE

`PermVerificationReview` is granted only to `RoleCompliance`
(`internal/auth/permission.go:688-690`); `RoleTenantAdmin` gets
`PermVerificationRead` only. Confirmed sole-grantee claim. `tenant_id`
comes from the JWT-derived `tc.TenantID`/RLS, never player input. This is
an adequate acceptance path with the mock webhook gated off — it is a
four-eyes-adjacent, staff-only, audited (`kyc.verification_status_changed`)
mutation. No objection. B4's decision not to add a simulate route is
correct: adding one would either require the same permission (redundant
with the existing staff route) or reopen the self-approval hole this stage
closes.

## 3. Removing `provider_reference` from player responses — SAFE, no
   replacement needed

Checked every player-facing consumer:
- Document upload (`kyc_handlers.go:186-…`, `newUploadMyDocumentHandler`)
  keys off `verification_id` = the platform's own `kyc.Verification.ID`
  (a UUID minted by the platform, `verification_service.go:109`), **not**
  `provider_reference`. Confirmed by reading the handler: it parses
  `r.FormValue("verification_id")` into a UUID and passes it to
  `kyc.UploadDocument`/lookup by primary key.
- `CreateVerificationInput`/`ProviderResult` (`internal/kyc/provider.go:49-80`)
  expose no redirect URL, SDK token, or hosted-session concept anywhere in
  the codebase — grepped `internal/httpserver` for
  `redirect_url|RedirectURL|verification_url|hosted`: zero matches tied to
  KYC. There is no hosted-KYC UI/redirect flow today for
  `provider_reference` to serve.
- `GET/POST /v1/me/kyc/verifications` list/create responses are the only
  other consumers, both covered by `playerVerificationResponse`.

**Conclusion: no player-facing flow needs `provider_reference` today.**
No opaque-alternative identifier is required. This is contingent on facts
as they stand (MOCK only, no vendor); when a real hosted-KYC vendor is
integrated (SumSub/Veriff/Jumio-shaped, ADR 0028), that integration will
very likely need to hand the player a vendor session token/URL — but that
token must be the vendor's **short-lived hosted-session id**, never
`provider_reference` (which after B6/B7 doubles as a durable idempotency
key items are matched against under HMAC). Recommend the design doc say
this explicitly as a forward note for B9/real-vendor work, so the future
implementer does not simply un-hide `provider_reference` to satisfy a
redirect flow.

## 4. "Production has no self-service KYC" — ACCURATE, acceptable as MOCK-only

Confirmed: `deps.KYCOrchestrator` is the only source of the `"mock"`
provider (`newCreateMyVerificationHandler`, `kyc_handlers.go:97-106`); no
other provider registration exists anywhere in the tree (grepped
`internal/kyc` and `cmd/platform-api`). No real vendor adapter exists. The
consequence is accurately scoped as `PROVIDER DEPENDENT`/MOCK-only per
CLAUDE.md's "No fake completion" — this is a software-capability gap
caused by having no commercial vendor relationship, not a claim of
regulatory posture. Staff routes remain, so compliance staff retain full
KYC operation. Acceptable.

## 5. Replay/audit semantics — CORRECT

B6(d)/(e) and B7 move parsing/outcome-validation and the tenant-scoped
verification lookup to *after* signature verification, and write the
`kyc.provider_callback` audit row only on a state-changing (forward) or
`error`-outcome transition — closing today's "audited early, replay
appends rows" defect. Equal/backward transitions (including anything after
a staff decision, matching §1 above) get 204 + no audit row + an
allow-listed `kyc_webhook_noop` log line, which is right: a replay must
not fabricate a second compliance event for the same underlying fact.
Concur with the rank definition (§B7 asks for `identity-compliance`
concurrence) — granted, contingent on the one-sentence addition requested
in §1.

## 6. Tests K1–K15 — adequate coverage, one gap to name explicitly

- Player self-approval forgery: K1 (any player-computable signature → 401)
  plus E1 pre-fix evidence. Covered.
- Provider-reference tampering: K5 covers per-field/whitespace/header
  tampering generically; K5 does not explicitly name "attacker submits a
  **different, real** `provider_reference` belonging to another
  verification in the *same* tenant with a validly-derived signature for
  a *different* reference" — this is subsumed by K5's "each field" tamper
  case but should be named as its own row so a reviewer can confirm intent:
  add **K5a**: valid signature for reference R1, but referring to R2's
  tenant-scoped row (same tenant, different verification) → this should
  fail because the signing input's `provider_reference` is inside the
  MAC'd body, so any substitution invalidates the signature. Effectively
  covered by K5 as written; recommend renaming/annotating one of the K5
  sub-cases to make this explicit rather than leaving it implicit in
  "each field."
- Production/test-support-disabled: K11 covers `KYCWebhookEnabled=false`
  → 404, `mockProviderWiring` in `{production}` and `{staging, TS=false}`
  → nil orchestrator/resolver, and nil-resolver → 401 `no_resolver`.
  Adequate — this is the matrix in §D, cross-checked.
- Missing/underspecified: no test asserts that `POST /v1/me/kyc/verifications`
  itself returns 503 (not 404) with the orchestrator nil in
  production/TS-off — B3's "Consequence" bullet documents this behavior
  but I don't see it as a table row (K1–K15 or E1–E4). Recommend adding
  it (call it K16 or fold into K11) since it's a player-visible contract
  point distinct from the webhook route's 404.

## Required design changes (non-blocking, recommend before/at implementation)

1. Add one sentence to B6/B7: a webhook callback can never resurrect a
   terminal verification; only a fresh `CreateVerification` row can start
   a new attempt.
2. Add a forward note to B9: any future real hosted-KYC vendor integration
   must issue a short-lived vendor session token for player redirect, and
   must NOT repurpose `provider_reference` for that purpose.
3. Add an explicit test (K16 or extend K11) asserting
   `POST /v1/me/kyc/verifications` returns 503 (not 404) when
   `KYCOrchestrator` is nil, matching B3's documented consequence.
4. (Optional, cosmetic) Split one K5 sub-case out to name
   "cross-verification provider_reference substitution within the same
   tenant" explicitly, since it's currently only implicit in "each field."

None of these block sign-off; they are documentation/test-completeness
refinements. The rank/monotonicity design (§B7) is approved as proposed.
