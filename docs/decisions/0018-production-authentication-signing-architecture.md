# ADR 0018 — Production Authentication Signing Architecture

Status: Architecture recommendation - `NOT IMPLEMENTED` this pass.
Development/test environments continue using the current
`internal/auth.KeyRegistry` (HS256, environment-supplied secrets)
unchanged, per the hardening-pass instructions. This ADR documents the
recommended PRODUCTION signing model only; no cloud provider is named or
required.

## Context

Stage 2's JWT issuance is HS256 with symmetric secrets supplied via
environment variables (`internal/auth/keys.go`'s `KeyRegistry`;
`config.go`'s `JWTActiveKID`/`JWTSigningSecret`/`JWTPreviousKID`/
`JWTPreviousSecret`). This is adequate for a single-deployable Stage 1/2
foundation and for local development/CI, but has a structural limitation
for production: a symmetric secret shared between "the thing that signs
tokens" and "the thing that verifies tokens" means every verifier must
hold a secret also capable of *forging* tokens. That is an acceptable
risk today (there genuinely is only one deployable - ADR 0010), but
becomes a real liability the moment any other service, partner-facing
surface, or narrower-privilege component only needs to *verify* tokens
and should never be able to mint them.

## Decision — platform-owned identity, KMS/HSM-managed asymmetric signing

Per the hardening-pass instructions' preferred direction, evaluated and
adopted as the target: **keep identity issuance under platform control**
(no managed identity provider is introduced - player and staff identity
are core platform data per `docs/architecture/05`, and no concrete
architectural reason to hand issuance to a third party has been
identified), but **move signing to KMS/HSM-managed asymmetric keys**
rather than continuing to hold symmetric secrets in application
config/environment variables in production.

### Design

- **Algorithm**: RS256 or ES256 (asymmetric) instead of HS256. Verifiers
  hold only the *public* key (embedded, or fetched from a
  `/.well-known/jwks.json`-style endpoint the issuing service exposes) -
  they can verify but never forge a token, closing the shared-secret
  liability above.
- **Key custody**: the private key never leaves the KMS/HSM boundary.
  Signing becomes a network call to the KMS's sign operation ("sign this
  header+payload with key X"), not a local cryptographic operation
  against key material the application process holds in memory. This is
  the same custody principle CLAUDE.md and ADR 0008 already establish for
  crypto custody (private keys never enter the core platform) - applied
  here to the platform's own signing key rather than a player's asset
  key.
- **Key identifiers (`kid`)**: unchanged in shape. `internal/auth.
  KeyRegistry`'s `kid`-based lookup already generalizes cleanly - a `kid`
  names a KMS key version instead of an env-supplied secret string, and
  `Verify` continues to look up by the token's own `kid`, supporting an
  old and new key concurrently during rotation exactly as today. No
  change to that mechanism, only to what a `kid` resolves to.
- **Key rotation**: KMS-native rotation (create a new key version, make
  it primary for new signing, keep the prior version available for
  verification until every token issued under it has expired) replaces
  Stage 2's two-env-var (active + previous) scheme. `KeyRegistry` already
  anticipated more than one simultaneously-valid key; production trades
  "two, manually configured via env vars" for "N, KMS-managed," without
  changing the interface `Issuer`/`Verify` depend on.
- **Issuer/audience validation**: unchanged - already implemented
  correctly in Stage 2 (`internal/auth/jwt.go`) and applies identically
  regardless of signing algorithm.
- **Expiry**: unchanged - short-lived access tokens, longer-lived
  rotating refresh tokens (`internal/auth/session.go`), independent of
  the signing-key architecture.
- **Revocation/session controls**: unchanged - Stage 2's refresh-token
  rotation with reuse detection is the platform's actual mechanism for
  anything longer-lived than an access token's TTL, and the signing-key
  architecture doesn't change this; a short access-token TTL bounds
  exposure regardless of how the token was signed.
- **Secure key storage / controlled access**: enforced structurally by
  the KMS/HSM - no application code, config file, or environment variable
  ever holds the private key. IAM-style access control on *who/what may
  invoke the KMS's sign operation* (in practice: only the identity-issuing
  service's own runtime role) becomes the production analog of "who can
  mint a token," auditable at the KMS/cloud-provider layer, independent
  of and in addition to application-level logs.
- **Auditability**: KMS sign/rotate operations are typically logged by
  the KMS/cloud provider itself - a separate, provider-managed audit
  trail from `internal/audit`'s application-level one, and a stronger
  guarantee for "was the signing key itself ever invoked outside expected
  code paths" than application logging alone can provide.

### What stays the same

`internal/auth.KeyRegistry`'s public shape, `Issuer.Issue`/`Verify`'s
signature and behavior (`kid` header, issuer/audience checks, mandatory
`exp`), and every other Stage 2 authentication control (Argon2id
passwords, session rotation, lockout, permission-based RBAC) are
unaffected - this is purely a signing-backend swap behind the existing
abstraction boundary, not a redesign of authentication itself.

## What this pass does NOT do

No code change. Development/test environments keep the current
`KeyRegistry` + environment-supplied HS256 secrets unchanged - this ADR
does not require KMS/HSM access to run tests locally or in CI, per the
instruction that "development/test environments may continue using the
current simpler mechanism." No specific cloud provider is named or
required; the design (asymmetric signing, KMS-held private key,
`kid`-based rotation) is provider-agnostic and implementable against any
major cloud KMS or a self-hosted HSM.

## Open decisions (human/business, not engineering)

1. Specific KMS/HSM provider and product - tied to the platform's
   eventual cloud-hosting decision (`docs/decisions/0005`'s open items),
   not decidable in isolation from that choice.
2. Timeline: migrate before first production launch, or accept this as a
   tracked go-live gap with a committed follow-up date.
3. Whether a JWKS-endpoint model (verifiers fetch the public key over
   HTTP) or a distributed-public-key-config model fits the eventual
   multi-service topology better - deferred until Stage 3+'s service
   boundaries are more concrete (currently still a single deployable, per
   ADR 0010).

## Owner

`security` (signing architecture), `architect` (integration with the
existing `KeyRegistry`/`Issuer` abstractions), `devops` (KMS/HSM
provisioning once a provider is chosen).
