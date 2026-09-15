# ADR 0029 — Document Storage, Security, and Privacy/Access Model

Status: Accepted. Part of Stage 4F. Does not introduce a production
cloud storage dependency, real malware scanning, encryption at rest, or
retention/legal-hold tooling - all documented as explicit, disclosed
future production requirements (§6 below), never silently assumed done.

## Context

Directive §5/§6/§7/§8 require: an immutable, versioned document model;
a storage boundary that never hard-codes a production cloud dependency;
strict player/staff access separation; and a defensible upload-safety
posture (size limits, content-type validation never trusting the client,
malware scanning, filename sanitization).

## Decisions

### 1. Document model and lifecycle

`kyc_documents` (migration `0040`): `UPLOAD -> pending_review ->
approved | rejected`. A reupload of the same `document_type` for the
same account is a **new row** with `version` incremented
(`UNIQUE(player_account_id, document_type, version)`) - never an
overwrite. Historical versions are never deleted or hidden; every
version remains listable.

### 2. Immutability, enforced at the database, not just by convention

A `BEFORE UPDATE`/`BEFORE DELETE`/`BEFORE TRUNCATE` trigger
(`kyc_documents_enforce_immutability`) refuses any change to
`storage_provider`/`storage_reference`/`content_type`/`size_bytes`/
`original_filename`/`checksum_sha256`/`document_type`/`version`/
`issuing_country`/`document_expires_at`/`uploaded_at` after insert, and
refuses DELETE/TRUNCATE outright - mirroring
`casino_launch_sessions_enforce_immutable_fields` (migration `0036`) and
`player_restrictions_deny_mutation` (migration `0037`)'s established
pattern. Only `status`/`reviewed_at`/`reviewed_by`/`rejection_reason` may
ever change, and only via `internal/kyc.ReviewDocument` - the one
legitimate `UPDATE` caller in this codebase.

### 3. Storage boundary - metadata in Postgres, content behind an interface

```go
type DocumentStorageProvider interface {
    ID() string
    Store(ctx, contentType string, content []byte) (StoredObject, error)
    Retrieve(ctx, reference string) (contentType string, content []byte, err error)
}
```

`kyc_documents` holds authoritative METADATA and an opaque
`storage_reference` only - never raw bytes, never a public URL.
`MockDocumentStorageProvider` (the only implementation this stage ships)
is an **in-memory, process-local map** - deliberately NOT the local
filesystem: writing uploaded content anywhere under this repository's
working tree risks it landing in git or being picked up by an unrelated
file-scanning tool. Content is lost on process restart - a documented
dev/test-only limitation, not a production guarantee.

**Future production requirements (documented, not built)**: encryption
at rest; genuinely private objects (no public URL ever, matching this
mock's own posture); short-lived, signed access URLs instead of `Retrieve`
returning bytes directly; retention and legal-hold policy; a real
malware-scanning integration at the storage layer; access logging at the
object-storage layer itself (this stage's own access logging is
API-layer only - see §5).

### 4. Access separation - player, compliance, tenant admin, finance, platform admin

- **Player**: may read/download only their OWN documents/verifications.
  Enforced by the HTTP handler explicitly comparing the resolved row's
  `PlayerAccountID` against the caller's own server-resolved account id
  (from the JWT subject) - see §4a below for why this is an
  APPLICATION-layer check, not purely an RLS one.
- **Compliance** (`PermVerificationReview`): may read AND
  approve/reject any document/verification within their own tenant.
- **Tenant admin** (`PermVerificationRead` only): read-only visibility
  into their own tenant's players' verification status - reasonable for
  an operational admin, never write/review authority (mirrors
  `PermRGRestrictionRead`'s identical precedent, ADR 0026 §12).
- **Finance**: no access at all - directive §19's explicit "do not grant
  sensitive verification access to Finance... unless explicitly
  justified," and no justification exists.
- **Platform admin**: no access at all - identical reasoning to every
  other platform-admin exclusion in this codebase (no path to resolve a
  specific tenant's player/document today - `PermPlayerRead` and its
  siblings are themselves `RequireTenantScope`-gated).

**4a. Why player-ownership is an application-layer check, not RLS.**
`kyc_verifications`/`kyc_documents`' own RLS (migration `0040`) enforces
TENANT isolation only, not per-PLAYER isolation - a deliberate scope
decision, not an oversight: reintroducing `player_restrictions`'
dual-scope/`WithPlayerScope` GUC machinery for two brand-new tables
would be a larger, RG-restriction-shaped mechanism for a much simpler
"is this row mine" check. Every player-self-service handler in
`internal/httpserver/kyc_handlers.go` therefore explicitly checks the
resolved row's `PlayerAccountID` before returning anything, reporting
404 (never 403) on a mismatch so a player can never learn a
differently-owned id exists. This is recorded here as an explicit
design choice so a future reviewer does not mistake it for a gap
relative to the RG precedent - it is a narrower table with a narrower,
sufficient control.

### 5. Audit

Every content READ (`GetDocumentContent`) writes a
`kyc.document_accessed` audit record - player or staff, no exception -
directive §7/§17's "audit every sensitive document access." Upload,
review, and verification-status-change actions are separately audited
(`kyc.document_uploaded`, `kyc.document_reviewed`,
`kyc.verification_submitted`, `kyc.verification_status_changed`).

### 6. Upload safety

- **Size**: `MaxDocumentSizeBytes` (15MB), enforced by both
  `http.MaxBytesReader` (outer request-body cap) and an explicit
  post-read length check.
- **Content-type**: the SNIFFED type (`http.DetectContentType`) is the
  only type ever trusted or persisted - the client's claimed
  `Content-Type` header is never used to decide acceptance or stored as
  the document's `content_type`.
- **Extension consistency**: the sniffed type must match the filename's
  extension against a fixed allowlist (`image/jpeg` -> `.jpg`/`.jpeg`,
  `image/png` -> `.png`, `application/pdf` -> `.pdf`) - directive's
  explicit "extension spoofing protection".
- **Filename sanitization**: `sanitizeFilename` strips any path
  component (`filepath.Base`) and replaces every character outside
  `[A-Za-z0-9._-]` with `_`, bounding length.
- **Malware scanning**: `MalwareScanner.Scan` is called on every upload
  before storage; a scanner ERROR is treated identically to a positive
  detection (fail closed), never as "assume clean". `MockMalwareScanner`
  (the only implementation) flags content containing the industry-
  standard EICAR test string, matching every other "magic value" mock
  convention in this codebase.

## Privacy / data minimization

No document binary or raw KYC evidence ever enters a log line or an
audit record's metadata - only opaque identifiers (`document_id`,
`document_type`, `version`, `content_type`, `size_bytes`). `issuing_country`
and `document_type` are the only descriptive metadata fields; no name,
date of birth, or document number field exists in `kyc_documents` at all
(that evidence, if ever captured, belongs to a future real KYC vendor's
own systems behind the provider boundary - ADR 0028 - not this platform's
own database).

## Specialist review findings and fixes

See `docs/progress.md`'s Stage 4F entry for the full itemized review pass
and every P0/P1 finding raised and fixed before this stage was considered
complete.
