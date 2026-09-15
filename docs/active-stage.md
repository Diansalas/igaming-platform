# Active Stage

## Stage 4F — Player Verification, Documents & Authentication Foundation — Complete

Status: **Complete, pending human approval to authorize the next stage.**
Builds the platform-owned KYC/identity-verification state model, a
document-management/storage boundary, a provider-neutral KYC provider
abstraction, and email-verification/password-reset authentication flows.
No real KYC/AML vendor is selected or integrated, no real vendor API is
invented, and no production cloud storage/email dependency is introduced
- all explicitly out of scope, per this stage's own governing directive.

### What was built

1. **Platform-owned verification model** - `kyc_verifications`/
   `kyc_documents` (migration `0040`), entirely new, tenant-owned tables
   hanging off the existing Person/PlayerAccount/Tenant/Brand model.
   `kyc_verifications.status` is a fully independent state machine
   (`unverified -> pending -> review_required -> approved | rejected |
   expired`) - never collapsed into `PlayerAccountStatus`, never a second
   identity model. `player_accounts.verified_at` (Stage 2's own reserved
   hook) is repurposed specifically for EMAIL verification, kept strictly
   distinct from KYC identity verification.
2. **Document management** - versioned, immutable `kyc_documents` rows
   (a reupload is a NEW row, `version+1`, never an overwrite). A
   database trigger, not just application discipline, blocks any UPDATE
   of core evidence fields and refuses DELETE/TRUNCATE outright - only
   `status`/`reviewed_at`/`reviewed_by`/`rejection_reason` may ever
   change, and only via `internal/kyc.ReviewDocument`.
3. **Document storage boundary** - `DocumentStorageProvider`
   (`Store`/`Retrieve`), with `MockDocumentStorageProvider` (in-memory,
   process-local) the only implementation this stage ships. The database
   holds authoritative metadata and an opaque `storage_reference` only -
   never raw bytes, never a public URL.
4. **KYC provider abstraction** - `KYCProvider`
   (`CreateVerification`/`GetVerification`/`SubmitVerification`/
   `HandleCallback`/`GetCapabilities`/`HealthStatus`), mirroring
   `CasinoProvider`/`PaymentProvider`'s exact shape. `MockKYCProvider` is
   the only implementation, with an invented, HMAC-signed callback
   format explicitly not modeled on any real vendor. `Orchestrator.
   ReceiveCallback` normalizes every provider result into exactly six
   platform outcomes; a provider error never corrupts stored state.
5. **Email verification and password reset** - a unified,
   purpose-discriminated `player_credential_tokens` table (mirroring
   `sessions.refresh_token_hash`'s hash-and-never-store-raw convention),
   short-lived one-time tokens, atomic conditional-UPDATE consumption
   (closing the same TOCTOU class ADR 0027 documented for Stage 4E),
   anti-enumeration (identical response whether or not an email/rate
   limit applies), and full session revocation on password reset.
6. **Email delivery abstraction** - `internal/email.Provider`
   (`Send`), `MockProvider` the only implementation - no production
   SMTP/API credential is required or introduced.
7. **Minimum API surface** - player: email-verification request/confirm,
   password-reset request/confirm, document upload, verification/document
   listing. Compliance: verification/document review, approve/reject.
   Provider webhook: `POST /v1/webhooks/kyc/{tenantSlug}/{providerID}`.
   No UI was built, per directive's explicit scope limit.

Full design, rationale, every recorded open decision, and the specialist-
review findings/fixes list: `docs/decisions/0028-kyc-provider-
abstraction-and-verification-model.md`, `0029-document-storage-and-
security.md`, `0030-email-verification-and-password-reset.md`.

### Specialist review: findings and fixes

A 7-area independent review (identity architecture, KYC provider
architecture, security/privacy, document security, PostgreSQL/RLS,
API/RBAC, adversarial testing) found:

1. **P1 - `SubmitVerification`/`GetVerification` were dead code** -
   declared on `KYCProvider` but never called from anywhere, leaving two
   of the six directive-named methods untested. **Fixed**: wired into the
   document-upload flow (`submitVerificationDocuments`, called from
   `UploadDocument` when a provider is configured) - each upload
   resubmits the verification's current full document set, matching how
   a real vendor evaluates accumulated evidence rather than one file in
   isolation.
2. **P1 - password hashed before token validation** in
   `newConfirmPasswordResetHandler` - an unauthenticated, rate-limit-free
   route triggering Argon2id's deliberately expensive hash on every
   request regardless of token validity, an uncosted DoS surface.
   **Fixed**: hashing moved inside `ValidateAndConsumeCredentialToken`'s
   callback, which only runs once a request already holds a valid,
   unconsumed token.
3. **P1 (PostgreSQL/RLS) - non-composite tenant FKs** -
   `kyc_verifications.player_account_id`,
   `player_credential_tokens.player_account_id`,
   `kyc_documents.verification_id`/`reviewed_by` referenced `(id)` only,
   proven exploitable by inserting a mislabelled cross-tenant row.
   **Fixed**: composite `(child_id, tenant_id) REFERENCES parent(id,
   tenant_id)` FKs throughout, backed by new `UNIQUE (id, tenant_id)`
   constraints on `player_accounts`/`staff_users`/`kyc_verifications`
   (mirroring `brands`' own precedent) - now proven rejected by
   `TestKYCVerifications_CompositeTenantFKRejectsCrossTenantPlayerAccount`.
4. **P1 (PostgreSQL/RLS) - `kyc_verifications`/
   `player_credential_tokens` had zero append-only protection** - a
   tenant's own connection could `DELETE` a rejected verification or a
   live credential token outright, destroying the audit trail directive
   §5/§17 require survive. **Fixed**: `BEFORE DELETE`/`BEFORE TRUNCATE`
   triggers on both tables (status/consumption itself remains a normal,
   allowed UPDATE - only row deletion is refused).
5. **P0 (adversarial) - concurrent double-confirm of a credential token
   was untested under real concurrency** - only sequential replay was
   proven. **Fixed**: `TestPasswordReset_ConcurrentDoubleConfirmAppliesExactlyOnce`
   fires 8 simultaneous confirms at one token under `-race`, asserting
   exactly one succeeds and exactly one row is ever consumed.
6. **P1 - `MalwareScanner`'s fail-closed contract was untested** -
   `MockMalwareScanner` can never itself return an error.
   **Fixed**: `TestUploadDocument_FailsClosedOnScannerError` (an
   error-returning scanner stub) proves `UploadDocument` rejects the
   upload and stores nothing when the scan cannot complete.
7. **P2 (PostgreSQL/RLS) - `provider_reference` uniqueness was global**
   rather than tenant-scoped, a cross-tenant existence oracle and a
   collision/denial risk once B2B tenants hold separate vendor accounts
   under the same `provider_id` string. **Fixed**: rescoped the unique
   index to `(tenant_id, provider_id, provider_reference)`.
8. **P2 (PostgreSQL/RLS) - `token_lookup`'s RLS policy lacked the
   tenant-unset conjunct** migration 0018 already established as
   defense-in-depth for the identical session-lookup pattern. **Fixed**:
   added, mirroring 0018's own reasoning exactly.

Several further P2s were reviewed and explicitly recorded rather than
silently dropped - see "Carried forward" below and ADR 0028/0029/0030's
own findings sections.

### Verification performed

`gofmt -l .` clean. `go build ./...` clean. `go vet -tags=integration
./...` clean. `go test ./...`, `go test -race ./...`, and `go test
-tags=integration ./...` all pass cleanly across the full repository,
including `internal/kyc` and `internal/httpserver`'s full Stage 4F test
suites. `go test -race -tags=integration ./...` passes except for the
pre-existing, already-documented `TestConcurrent_
DuplicateBetDeliveryDuringSelfExclusion` flake (Stage 4D-RG/4E, unrelated
to this stage, not fixed here). Migration `0040` round-tripped (`up` ->
`down` -> `up`, twice) cleanly against the live dev database, including
after the composite-FK/append-only-trigger fixes above.

### Carried forward, NOT silently closed

- **Cross-brand real-player protection remains inactive** (Stage 4E's own
  carried-forward limitation) - this stage does not wire KYC evidence
  into `PersonResolver`; that connection is an explicit OPEN DECISION
  (ADR 0028 §7), requiring product/legal/compliance input.
- **Cross-tenant reuse of an approved verification** is an explicit OPEN
  DECISION (ADR 0028 §3/§7), not implemented.
- Stage 3D withdrawal TOCTOU, the payments-side unguarded-reversal race,
  casino per-tenant provider signing keys, the casino real-provider API
  documentation requirement, RG future controls (deposit/loss/wagering/
  session limits, reality checks, time-outs), Bonus Engine accounting
  open decisions, sportsbook provider documentation, and the Stage
  4D-RG/4E casino bet-delivery/self-exclusion concurrency flake all
  remain separately tracked from earlier stages - none silently closed.
- Recorded P2s not fixed this stage: review decisions on `kyc_documents`
  remain revertible in place (status/reviewed_by/reviewed_at can be
  rewritten indefinitely - only the underlying evidence is immutable);
  no per-IP/global rate limit on credential-token endpoints (only
  per-account); no upload-rate-limit on `/v1/me/kyc/documents`; a
  password-reset timing side-channel (email sent synchronously only on
  the "found" path); `ParseMultipartForm` may spill to the OS temp
  directory before validation runs; `player_credential_tokens` has no
  retention/purge policy for `requested_ip` (PII); an access-JWT remains
  valid up to 15 minutes after a password reset revokes its session's
  refresh token; `IssuingCountry` is unvalidated free text; denied/
  cross-owner document-access attempts are not themselves audited (only
  successful accesses are); the app's own database role owns these
  tables and could in principle disable its own RLS/triggers (a
  pre-existing platform-wide limitation, not new to this stage).

### Decisions/input still useful from the human before the next stage

1. Approve Stage 4F and authorize the next stage (per CLAUDE.md's stage
   gate).
2. Decide the priority/timeline for a real KYC/identity-verification
   vendor contract - this both activates real document verification AND
   is the prerequisite for closing Stage 4E's cross-brand evasion gap.
3. Decide whether an approved verification should ever be reusable
   platform-wide across tenants/brands (ADR 0028 §3/§7).
4. The already-open, non-blocking items carried forward from Stages
   0-4E remain open (see "Carried forward" above and each stage's own
   ADRs).
