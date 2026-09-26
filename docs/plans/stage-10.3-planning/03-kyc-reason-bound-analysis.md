# Stage 10.3 planning — KYC-REASON-BOUND-1 design analysis (`identity-compliance`)

Repo `/home/user/igaming-platform` at HEAD `957a3e8`. Design analysis only — no code, migration,
`deploy/`, or `aws`/`terraform` command was run or edited. Nothing was committed.

Binding references read: `CLAUDE.md`; task registry row `KYC-REASON-BOUND-1`
(`docs/governance/task-registry.md`); Stage 10.2 final security review F-7
(`docs/plans/stage-10.2-planning/09-review-security-final.md`); ADR 0028 and its Stage 10.2
amendment; `internal/kyc/{provider,mock_provider,verification_service,types,validate}.go`;
`internal/httpserver/{kyc_handlers,kyc_admin_handlers}.go`; migration `0040` up/down; the KYC
OpenAPI schemas in `docs/api/openapi/platform-api.yaml`.

## 1. KYC-REASON-BOUND-1

### Why it exists

F-7 (Info, pre-existing, Stage 10.2 final review): the verified sender's `reason` string — up to
the KYC webhook body cap of ~256 KiB (`internal/webhookauth` KYC domain limit) — is unbounded in
length and charset. It flows, verbatim:

- into `kyc_verifications.reason` (a plain `TEXT` column, no `CHECK`, migration `0040` line 57:
  `reason TEXT` — comment says "must never contain raw KYC evidence" but nothing enforces that);
- into `audit_log.metadata` (JSONB, migration `0014`, also unbounded) via both
  `applyCallbackOutcome`'s success and failure audit entries
  (`internal/kyc/provider.go:388,428`, key `"reason"`);
- into the player-facing HTTP response, both `playerVerificationResponse.Reason`
  (`internal/httpserver/kyc_handlers.go:75,83`) and the staff-facing `verificationResponse.Reason`
  (`kyc_handlers.go:44,52`), and the equivalent OpenAPI `PlayerVerification.reason` /
  `Verification.reason` schemas (`docs/api/openapi/platform-api.yaml:4899,4927`), both typed as an
  unconstrained `{ type: string }`.

ADR 0028 §5 states the intent already: `ProviderResult.Reason` is meant to be "a short,
non-sensitive, machine-readable code... a real adapter must never put raw KYC evidence into it."
Today nothing enforces that intent against a misbehaving or compromised real adapter (or a bug in
one) — it is a documented convention, not a bound. `CLAUDE.md`'s "no fake completion" language
therefore matters here: the current state is `PARTIALLY IMPLEMENTED` (the model exists; the bound
does not), not `IMPLEMENTED`.

### Whether it blocks a real provider

**Yes, in effect, though not by a hard technical gate today.** `MockKYCProvider.HandleCallback`
(`internal/kyc/mock_provider.go:190-220`) copies `payload.Reason` straight from the verified body
into `ProviderResult.Reason` with no length or charset check. Nothing in the `KYCProvider`
interface, the `Orchestrator`, or the DB schema stops a real adapter's normalization step from
doing the same, and if it did, an arbitrarily large or control-character-laden vendor status
message would reach the DB, the audit trail, and the player's browser unchanged. The Stage 10.2
review treated this as Info/deferred specifically because no real adapter exists yet — the task
registry row is explicit that this is deferred until "the first real KYC adapter lands." It is
correctly scoped as a **pre-real-vendor gate**, not a Stage 10.2 blocker: it does not need to be
fixed before Stage 10.2/10.3 closes, but it must be fixed before any real `KYCProvider` adapter is
registered, because a real vendor's raw status text is exactly the kind of "raw KYC evidence" ADR
0028 §5 and directive §10/§17 say must never leak to the player or into logs unbounded.

### Dependencies

- No dependency on any other open task. `KYC-REASON-BOUND-1`'s row lists no blockers ("—") and
  Dependencies "none (no real adapter yet)".
- It sits upstream of, but does not block, the eventual real-adapter conformance work (F-9, already
  closed for the mandatory tenant-binding case at HEAD via `internal/kyc/conformance_test.go`,
  commit `0e72805`) — a real adapter's own conformance suite should exercise reason bounding too
  once this design is built (see Tests below).
- Loosely related to F-5 (outcome=`error` audit growth) and F-6 (duplicate-header handling): all
  three are "harden before first real adapter" items from the same review, but none is a
  prerequisite for this one.

### Approach

Two independent things must both be bounded, because they serve different audiences and have
different trust levels:

**A. A closed, player-facing reason **code** enum**, distinct from any raw provider text.
- Introduce a small platform-owned enum, e.g. `kyc.ReasonCode`, with values such as
  `document_illegible`, `document_expired`, `name_mismatch`, `address_mismatch`, `duplicate_person`,
  `manual_review_required`, `provider_error`, `unspecified` (illustrative — the actual player-facing
  wording is a human/compliance decision, see below). This mirrors the existing pattern of closed
  enums the codebase already uses for `ProviderOutcome`, `DocumentType`, and `VerificationStatus`
  (`internal/kyc/types.go`), so it is consistent with how this package already avoids raw
  vendor/free-text strings leaking into the domain model.
- A real adapter's `HandleCallback` normalization step maps whatever raw vendor reason/status
  string it received onto exactly one of this enum's members (defaulting to `unspecified` for
  anything it cannot confidently map) — the same normalization discipline ADR 0028 §5 already
  requires for `ProviderOutcome` itself. `MockKYCProvider` should also start emitting one of the
  enum values from its own `SetOutcome`/`CallbackPayload` test fixtures, so the conformance suite
  can exercise this path with the mock before any real adapter exists.
- Add a new player-facing field, e.g. `reason_code` (closed enum), to `playerVerificationResponse`
  and the `PlayerVerification` OpenAPI schema. The raw provider reason text is **never** put in a
  player-facing response field, under any name.

**B. A staff-only bounded raw provider text**, kept for the operator/compliance case queue.
- Keep today's `reason` column and field as the **staff-only** carrier of the provider's original
  (post-normalization but still free-text) reason string — `verificationResponse.Reason`
  (`kyc_admin_handlers.go`/`kyc_handlers.go` staff shape) and the `Verification` OpenAPI schema
  already have no player exposure (F-7's `provider_reference` precedent — Stage 10.2 already
  removed `provider_reference` from every player-facing shape; the same precedent, not a new one,
  applies to raw `reason`).
- Bound it defensively anyway, even though it is staff-only, for two reasons: (1) the audit trail
  and DB storage cost is unbounded today regardless of who reads it back; (2) "staff-only" is an
  API-shape property, not a database guarantee — a bound at the point of ingestion is a stronger,
  defense-in-depth control than trusting every future reader to respect the shape boundary.

**Length and charset bound, applied once, at ingestion (adapter/normalization boundary), not
scattered across every reader:**
- Max length: a generous but finite cap, consistent with the existing `MaxDocumentSizeBytes`
  convention of picking "a reasonable foundation-stage default, not derived from any specific
  vendor's own limit" (`internal/kyc/validate.go:16-20`). Recommend **512 bytes** for the raw
  staff-only reason (enough for a genuinely short status message, far below the 256 KiB webhook
  body cap) and a small fixed set for the reason code (an enum member, not a length problem).
- Charset: strip or reject control characters (anything below U+0020 except none — no tabs/newlines
  needed for a one-line status message) and non-printable/format Unicode categories, mirroring
  `sanitizeFilename`'s existing allow-list-and-replace approach
  (`internal/kyc/validate.go:85-104`) rather than a denylist. Truncate (do not silently accept)
  anything past the length cap, and record truncation via a bounded suffix marker or a boolean
  audit metadata flag — never silently drop information without any trace that truncation occurred.
- This normalization function belongs in `internal/kyc` (e.g. alongside `validate.go`, a
  `NormalizeReason(raw string) (bounded string, truncated bool)` helper), called once by
  `MockKYCProvider.HandleCallback` and required of every future adapter's own `HandleCallback` by
  doc comment on the `KYCProvider` interface (mirroring how `SubmittedDocument`'s doc comment
  already directs future adapters).

### DB impact

- New `CHECK (length(reason) <= 512)` constraint on `kyc_verifications.reason` (or equivalent
  `varchar(512)` — Postgres `TEXT` + `CHECK` is this codebase's established convention, e.g.
  `kyc_documents.rejection_reason` also has no explicit bound today, worth flagging as the same
  class of gap but out of this task's stated scope).
- No new column is strictly required for the reason **code** if the enum is stored as a short TEXT
  with its own `CHECK (reason_code IN (...))`, mirroring `status`'s own `CHECK` pattern
  (migration `0040:44-45`). Recommend a new nullable `reason_code TEXT` column with a `CHECK`
  against the closed enum, kept separate from `reason` (the bounded raw text) rather than
  overloading one column — `status`/`reason` are already independent facts on this row, and mixing
  a closed enum with free text in one column would reintroduce exactly the "collapse two concepts
  into one flag" anti-pattern ADR 0028 §1 explicitly rejects for `PlayerAccountStatus`/KYC status.
- `audit_log.metadata` (JSONB) cannot itself carry a `CHECK` on a nested key's length practically;
  bounding must happen before `audit.Record` is called, i.e. in the same normalization step, not as
  a DB-side constraint on the audit table. This is disclosed as a residual: the audit row will
  still only ever contain the already-bounded value, but the JSONB column itself stays unbounded in
  principle (same as every other audit metadata field today — not a new gap this task introduces).

### API impact

- `PlayerVerification` OpenAPI schema: replace or supplement `reason: { type: string }` with
  `reason_code: { type: string, enum: [...] }`. Decide (see "human decision" below) whether the
  existing free-text `reason` field is removed from `PlayerVerification` entirely or kept but
  always empty/omitted for future real-adapter verifications — removing it is cleaner and matches
  the `provider_reference` precedent (Stage 10.2 removed a field from the player shape outright
  rather than leaving it present-but-usually-empty).
- `Verification` (staff-facing) schema: keep `reason` (now documented as "bounded to 512 bytes,
  control characters stripped"), add `reason_code` alongside it for staff triage convenience.
- Both are additive/breaking depending on the choice above for `PlayerVerification.reason`; removal
  is a breaking API change and needs the same versioning care as any other player-facing contract
  change (check `docs/api/openapi/platform-api.yaml` for this API's existing versioning policy
  before removing a field outright — not inspected in this pass beyond confirming the schema
  location, since it is out of this task's stated scope).

### Security impact

- Closes an unbounded-injection surface: a malicious or compromised real vendor could otherwise
  push arbitrary-length text (up to the 256 KiB webhook cap) with arbitrary control characters into
  the DB, the audit trail, and a player's browser — a stored-XSS-adjacent and log-injection-adjacent
  vector once a real frontend renders `reason` directly, and a potential terminal/log-injection
  vector if any future tooling naively prints audit metadata to a terminal. Bounding charset closes
  both classes.
- Consistent with the existing Stage 10.2 posture (`ProviderResult.Reason` "must never contain
  evidence" — already enforced for the *verified-residence* determination's own audit metadata,
  `internal/kyc/verification_service.go:386-392`, which deliberately excludes staff free-text
  `Reason` from that specific audit entry for exactly this class of concern). This task generalizes
  that same discipline to the provider-callback path.
- Does **not** touch authentication/authorization, the webhook trust model, or session/token
  handling — no `security` specialist review is required under this agent's charter ("Requests
  `security` review for token/session handling"); this is a data-shape/normalization change. A
  lightweight `security` sanity check on the control-character stripping approach would be prudent
  but is not mandated by scope.

### Financial impact

None. `kyc_verifications`/`kyc_documents` do not gate a withdrawal directly in this codebase today
— KYC tier/status gating of financial actions is a separate, not-yet-fully-wired concern (per ADR
0028 §1, KYC status is deliberately never collapsed into `PlayerAccountStatus`, and no code path in
this stage's scope reads `kyc_verifications.status` to block a withdrawal). No `ledger-finance`
review is triggered by this change.

### RLS impact

None. `kyc_verifications` already has `tenant_isolation` RLS (`FORCE ROW LEVEL SECURITY`, migration
`0040:86-92`) and an append-only delete-denial trigger; a new `CHECK` constraint and a new
`reason_code` column do not change tenant scoping, RLS policy shape, or the append-only/immutability
triggers already in place. The new column and constraint would follow the same migration pattern as
every other additive column already added to this table on later stages (e.g. the Stage 4I Phase B
`verified_residence_*` columns).

### Tests

- **Unit** (`internal/kyc/validate_test.go` or new `reason_normalize_test.go`): length truncation
  at the boundary (511/512/513 bytes, and multi-byte UTF-8 characters straddling the cut point —
  never split a rune), control-character stripping (NUL, `\n`, `\r`, ANSI escape sequences, other
  C0/C1 controls), a truncation marker/flag is set when truncation occurs, and an already-short,
  already-clean string passes through unchanged (no false-positive mutation).
- **Mock provider** (`internal/kyc/mock_provider_test.go` or existing integration tests): a
  callback with an oversized/control-character-laden `reason` in the verified body is normalized
  before it reaches `ProviderResult`, `kyc_verifications.reason`, and `audit_log.metadata` — assert
  the stored value is bounded and clean, not just the in-memory `ProviderResult`.
  `orchestrator_webhook_integration_test.go` already exercises the full callback-to-DB path and is
  the right place to add this case.
- **API contract**: extend `TestOpenAPI_KYC...` (referenced in the Stage 10.2 security review as
  covering the KYC OpenAPI entries) to assert `PlayerVerification` never has a `reason` field once
  removed (or, if kept, that it is never populated for provider-driven transitions) — a structural,
  schema-shape test, not a full JSON-Schema conformance run (matching this codebase's existing
  `API-DOC-PAYWH` precedent of "structural check; no full JSON-Schema conformance test").
- **Conformance suite** (`internal/kyc/conformance_test.go`, `RunProviderConformanceSuite`): add a
  mandatory (fail-not-skip, per the F-9/K3 precedent already established in this file) case
  asserting any real adapter's `HandleCallback` also returns a bounded, control-character-free
  reason and a valid `reason_code` enum member — this is exactly the kind of "the mock enforces it
  today; a real adapter must prove it too" case this suite already exists to hold the line on.
- **DB migration test**: a pre-flight check (see Migration below) that the up-migration correctly
  reports/handles any pre-existing over-length row rather than failing the migration outright with
  an opaque constraint violation.

### Migration

- New migration, next available number after `0093` (currently the highest — `0093_sportsbook_...`)
  — call it e.g. `0094_kyc_verification_reason_bound`.
- **Up**:
  1. Pre-flight: `UPDATE kyc_verifications SET reason = left(regexp_replace(reason, '[\x00-\x1F\x7F]', '', 'g'), 512) WHERE reason IS NOT NULL AND (length(reason) > 512 OR reason ~ '[\x00-\x1F\x7F]')` — normalize existing rows *before* adding the constraint, so the migration cannot fail on legacy data. Since `MockKYCProvider` is the only shipped adapter and no real vendor has ever run against this schema, any existing over-length data in dev/staging is synthetic mock-generated test data, not real PII/evidence — safe to truncate rather than needing a more careful backfill strategy. This must be stated explicitly in the migration's own comment so a future reader does not assume real evidence was ever at risk.
  2. Add `reason_code TEXT` column with `CHECK (reason_code IS NULL OR reason_code IN (<enum members>))`.
  3. Add `CHECK (reason IS NULL OR length(reason) <= 512)` on `kyc_verifications.reason`.
  4. (Optional, same migration or a follow-up) add the equivalent bound to `kyc_documents.rejection_reason`, which has the identical unbounded-`TEXT` shape — flagged above as the same class of gap; recommend a human/orchestrator decision on whether to fold it into this migration or track it as a separate follow-up task, since it was not named in the F-7 finding or the task registry row and including it would be a small scope expansion beyond what was explicitly requested.
- **Down**: drop both `CHECK` constraints and the `reason_code` column; existing `reason` values
  are already ≤512 bytes and control-character-free post-up, so no data transformation is needed on
  the way down — this is a clean, symmetric down migration (no tombstone or compensating-entry
  concern, since this is schema/data-shape, not a financial ledger row).

### Rollback

- Schema rollback is the down-migration above — safe, since no financial/append-only invariant is
  touched (unlike `kyc_verifications`' own delete-denial trigger, which this migration does not
  touch).
- Code rollback: reverting the normalization call in `HandleCallback` and the new `reason_code`
  field is a plain code revert; since `MockKYCProvider` is the only adapter today, no real-vendor
  contract is ever affected by rolling this back.
- No player-visible data loss risk on rollback beyond the (already-disclosed, mock-only, synthetic)
  truncation applied during the up-migration's pre-flight step.

### Ownership

`identity-compliance` (this agent) owns the schema and normalization logic per this agent's charter
("Owns the identity/person data model and compliance rule configuration schema"). No cross-cutting
architecture change is involved (single table, single package), so no `architect` review is
structurally required, though the `architect` should be informed given the OpenAPI contract change
touches a schema other domains' documentation/tests may reference.

### Whether a human decision is required

**Yes, for the player-facing wording specifically — do not invent it here.** The exact set of
player-facing `reason_code` enum members and their user-visible copy (e.g. what a player actually
sees when their verification is rejected) is a product/compliance/legal decision, not an
engineering one, for two concrete reasons:

1. Several EU/LATAM jurisdictions impose specific disclosure requirements or restrictions on what a
   regulated operator may or must tell a rejected player (e.g. whether "identity could not be
   confirmed" must avoid implying suspicion of fraud, or whether certain AML-driven rejections must
   legally give **no** reason to the player at all to avoid tipping off a subject of a SAR-adjacent
   process). This platform has no confirmed jurisdiction-specific legal sign-off on player-facing
   KYC rejection wording, and CLAUDE.md's "when to stop and ask" explicitly names "legal
   interpretation" as a stop-and-ask trigger.
2. The illustrative enum names given above (`document_illegible`, `name_mismatch`, etc.) are
   engineering placeholders only, chosen to demonstrate the mechanism — they are **not** a
   recommended or approved player-facing copy set, and must not be read as one.

Everything else in this design (length/charset bound, staff-vs-player split, DB/migration shape,
tests) is an ordinary, reversible engineering decision within this agent's authority and does not
require a human decision — consistent with "make the call yourself for ordinary engineering
decisions that are reversible and within an already-approved stage's scope."

## 2. Other KYC/RG provider-readiness gaps found (source-evidenced only)

Checked all four candidates named in the task. Findings, in order:

- **Hosted-session token (ruling J12) — real gap, NOT IMPLEMENTED.** ADR 0028's Stage 10.2
  amendment records only a forward *note*: "a future hosted-KYC vendor that needs a player redirect
  must issue its own short-lived session token for it. It must not repurpose `provider_reference`
  for this, and the reference must not be returned to the player"
  (`docs/decisions/0028-...md`, Amendment section; identical wording at
  `docs/plans/stage-10.2-planning/01-webhook-trust-design.md:540`, ruling J12). No code in
  `internal/kyc` implements any session-token issuance, storage, or player-redirect mechanism today
  — `KYCProvider`'s interface (`internal/kyc/types.go:91-110`) has no method for it, and grepping
  `internal/kyc`/`internal/httpserver` for a session/redirect concept found nothing beyond this
  design note. This is a genuine, disclosed gap: hosted-flow vendors (SumSub/Veriff/Jumio's actual
  hosted-session UX pattern) cannot be integrated today without first designing this, and per this
  agent's own testing responsibility ("session token never being handed to a provider... per
  Blueprint §4.1"), the *design*, not just the implementation, needs to establish that the game/
  session token and any future KYC hosted-session token are structurally distinct types before a
  real adapter is built — not evaluated further here since building it is out of this task's scope
  (KYC-REASON-BOUND-1 only) and no task registry row exists yet for it.
- **KYC real-vendor route pattern — already fully specified, not a gap.** ADR 0028's amendment
  states plainly: "Future real vendor: its route follows the casino pattern. It is always
  registered, it is not gated by test support, and it fails closed (uniform 401, `no_resolver`)
  until a real resolver exists. The real resolver is `NOT IMPLEMENTED`." This is a disclosed,
  correctly-labeled `NOT IMPLEMENTED` item, not an undiscovered gap — no further finding here.
- **Document verification boundary — no new gap found in this pass.** `SubmittedDocument` and
  `KYCProvider.SubmitVerification` (`internal/kyc/types.go:57-69,91-110`) already carry only
  `DocumentID`/`DocumentType` references, never raw bytes, per ADR 0028 §4's explicit design; ADR
  0029 (document storage/security) is the governing design for the storage boundary and was not
  re-read line-by-line in this pass (out of this task's named reading list) — no finding to report
  without inventing one.
- **Sanctions/PEP vendor interface — out of scope evidence, not fabricated as a finding.** No
  `internal/aml` or equivalent sanctions/PEP screening package exists anywhere in the repo (checked
  via grep across `internal/` for "sanctions"/"PEP"/"AML" — the only matches are unrelated: RG
  package comments, a payments webhook contract test, and type doc comments in `casino`/
  `identityresolution` that happen to contain the substring). This agent's own charter names AML
  sanctions/PEP screening as in-scope future work, and this codebase's Stage history (visible via
  `docs/progress.md`/ADR numbering) has clearly built identity/KYC (Stage 4E/4F) but has not yet
  reached the AML screening subsystem. This is consistent with staged, incremental scope, not an
  overlooked defect — reported here as a confirmed absence, not asserted as a "blocking" defect,
  since no task or stage has yet claimed AML screening is built. No fabricated blocker is recorded.

**F-9 status update (not requested, but relevant context):** the Stage 10.2 review's F-9 (KYC
conformance suite missing / tenant-binding case allowed to skip for a real adapter) is **already
resolved at HEAD** for KYC — `internal/kyc/conformance_test.go` (commit `0e72805`) now exists and
its tenant-binding case is `t.Fatalf`, not `t.Skip`, for any non-`*MockKYCProvider`. Casino and
payments still have unrelated, non-mandatory `t.Skip` calls for mock-specific fixture-construction
helpers (not the tenant-binding case itself), which is consistent with the original F-9 finding's
narrower scope. Noted for completeness; not a new finding.
