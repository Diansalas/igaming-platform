# 0043 — Jurisdiction Evaluation Policy Configuration (Stage 4I Phase D)

Status: **ACCEPTED.** Records the architect design ruling implemented by
Stage 4I Phase D ("Jurisdiction Policy Configuration & Operational
Semantics") and the reasoning behind its binding choices. This ADR does
not itself decide any of HDR-J-7/HDR-J-8/HDR-J-9 — see
`docs/decisions/0044-human-decision-register-stage-4i-phase-d.md` for
those, still open.

## Context

Stage 4I Phase C (`docs/decisions/0042-human-decision-response.md`,
`docs/governance/stage-4i-canonical-model.md` §14) built the
`DeterminePlayerJurisdiction` precedence engine and named four deferred
legal/policy gaps (PC-GAP-1 through PC-GAP-4, §14.6). Phase D closes the
**mechanism** for three of them (PC-GAP-1, PC-GAP-2, PC-GAP-4) and adds a
fourth mechanism (PC-GAP-3's mapping seam) — while deliberately supplying
**zero policy content** for any of them, per CLAUDE.md's no-fake-completion
and no-uncontrolled-scope rules.

## Decision 1 — extend `jurisdiction_precedence_configs`, do not create a sibling table

`jurisdiction_precedence_configs` (migration 0071) already carries the
correct key — `(licensing_jurisdiction_id, operation_class, effective_from)`
— for source-precedence content. PC-GAP-1/PC-GAP-2's location-policy knobs
govern the identical key, at the identical effective-dated cadence, with
the identical write authorization boundary (platform-admin-only). Creating
a second table would either duplicate that key/authorization/versioning
machinery or force two separate effective-dated lookups to be reconciled
against each other at read time — an unforced coordination problem with no
compensating benefit. The table's role now widens explicitly, and its
`COMMENT ON TABLE` records that widening: "the effective-dated
evaluation-policy configuration for a licensing jurisdiction and operation
class, of which the source-precedence ordering is one (currently unset)
component." The table name is retained — renaming it would churn
canonical-model §3.4/§11.1/§11.2, migration 0071's own comments, and the
task registry for zero functional gain.

## Decision 2 — licensing-jurisdiction-keyed, never tenant-keyed

HDR-J-2's own NOTES describe the desired precedence configuration as
"tenant/jurisdiction aware." That wording is loose and is superseded by
canonical-model §3.4's binding ruling (RISK §4.2's bootstrap-circularity
correction, adopted as canonical): selecting a per-jurisdiction rule cannot
depend on the jurisdiction that rule determines, but the tenant's own
**licensing** jurisdiction is knowable before any player-side resolution
runs, via `tenants.licence_id -> licences.jurisdiction_id`. The key is
therefore `(licensing_jurisdiction_id, operation_class, effective_from)` —
**no `tenant_id` column, and none may be added.**

"Tenant-aware" is satisfied transitively and correctly: two tenants
licensed in the same jurisdiction legitimately share one evaluation
policy; two tenants licensed differently get different policies
automatically. The caller never handles a licensing-jurisdiction id at
all — `ResolveEvaluationPolicy` takes a `TenantID` and derives the
licensing jurisdiction internally, so the call site is tenant-keyed, the
storage is licensing-jurisdiction-keyed, and the translation happens
exactly once, server-side, inside `internal/jurisdiction`.

Per-tenant *activation* already exists on a different, deliberately
uncoupled axis: `jurisdiction_resolution_active (tenant_id,
operation_class)`. `ResolveEvaluationPolicy` never reads that table (or
`jurisdiction_evidence_collection_active`) — see Decision 6 below.

## Decision 3 — append-only provenance, and the PHASE-B-ARCH-1 re-deferral

`jurisdiction_precedence_configs` is now the reference shape for the
platform's activation-fact provenance problem, not a repeat of its
existing defect. `jurisdiction_resolution_active` and
`jurisdiction_evidence_collection_active` both `INSERT ... ON CONFLICT DO
UPDATE`, so `effective_from`/`created_by_actor_*` are stamped once, at
first insert, and never move again even though the row's `active` value
keeps changing (the deferred PHASE-B-ARCH-1 finding,
`docs/governance/task-registry.md`).

`jurisdiction_precedence_configs` cannot reproduce that defect: there is no
`ON CONFLICT DO UPDATE` anywhere in `evaluation_policy_admin.go`;
`effective_from`, `created_at`, `created_by_actor_type`,
`created_by_actor_id`, and `reason_code` are written exactly once, at
INSERT, on a row the append-only trigger then makes immutable except for
`effective_to`; and every change of the policy in force is a **new row**.
"Effective from when, by whom, why, under which legal review" is therefore
always the row's own truthful, self-describing provenance.

**The deferred PHASE-B-ARCH-1 item on the OTHER two tables remains
deferred, unchanged.** Its three trigger conditions (exposing activation
history in back office/console; wiring either activation fact into a
regulatory report; adding a second writer/toggler of either table) do not
fire in this phase: Phase D adds no HTTP route or console surface,
produces no report/export, and does not write, read, alter, or reference
either `jurisdiction_resolution_active` or
`jurisdiction_evidence_collection_active`. `audit_log` remains the
authoritative source of activation change history for those two tables
until a future phase fixes them; no document/report/console screen may
present their own columns as the record of the change in force.

## Decision 4 — RLS added, hardening direction only

`jurisdiction_precedence_configs` shipped in migration 0071 with **no**
row-level security, matching `jurisdictions`/`licences`'s posture as
platform-wide reference data. Phase D adds RLS: a permissive `FOR SELECT
USING (true)` (this remains data every tenant-scoped transaction must be
able to read), and platform-admin-scope-only `FOR INSERT`/`FOR UPDATE`
(mirroring `asset_change_requests`'s migration-0044 predicate exactly). No
`DELETE` policy, no `FOR ALL` policy. This is the only RLS posture change
in this phase, and it only tightens what was previously unrestricted at
the database (the write side was previously controlled by application
discipline alone, since the table had no application writer at all before
this phase). `jurisdictions`, `licences`, `tenants` remain untouched;
`jurisdiction_resolutions`, `jurisdiction_resolution_active`,
`jurisdiction_evidence_collection_active`, `asset_authorizations`, and
every other table: zero diff.

## Decision 5 — canonical-model §6.1's "no other permission" sentence, amended

Canonical-model §6.1 previously ended: "No other permission and no new
role is authorized." That sentence was scoped to the tenant/platform split
it was describing at the time, and has already been superseded twice by
later, reviewed phases (Phase B added `PermPlayerResidenceRead` and
`PermJurisdictionEvidenceCollectionActivate`). Rather than let that drift
silently continue, this ADR records the amendment explicitly:

> §6.1's "No other permission and no new role is authorized" is amended to
> **"no other permission without a recorded architect ruling naming
> it."** No new role is created by this phase.

`PermJurisdictionEvaluationPolicyWrite` (platform-only, granted only to
`RolePlatformAdmin`) is the permission this ruling names. Activation is
deliberately **not** covered by this permission — writing an
`active` version is refused outright in this phase
(`ErrActivationNotAuthorized`); the phase that has HDR-J-8/HDR-J-9 content
must add its own permission and a dual-control ruling (HDR-J-2's own
recorded technical consequence: source-precedence content "needs a
four-eyes posture at least as strong as the platform's existing
`bonus_approval_policies` pattern"). No HTTP route or OpenAPI change is
added in this phase. **Correction:** a `draft` MAY carry real
location/staleness content authored ahead of legal review —
`CreateEvaluationPolicyVersion` accepts a real `LocationSignalRequirement`/
`MaxLocationSignalAge` on a `draft` row (only `withdrawn` is forced
content-free), and a passing test
(`TestCreateEvaluationPolicyVersion_SupersessionClosesPredecessorByteIdentical`)
already authors exactly such a draft — but no HTTP authoring route exists
in this phase because that content can never be activated
(`ErrActivationNotAuthorized` refuses it at the sanctioned Go write path
— migration 0075 imposes no database-level guard against `status =
'active'`, so this is an application-layer refusal, not a structural
impossibility; see the code-reviewer finding recorded in
`docs/governance/task-registry.md`'s Phase D section) until HDR-J-8/HDR-J-9
are answered and a future phase adds the activation permission and
dual-control mechanism; shipping an authoring endpoint for content that
cannot yet be approved is a needless surface. The Go write API is
exercised by integration tests under `db.Pool.WithPlatformAdmin` only.

## Decision 6 — three independent switches, never coupled

`ResolveEvaluationPolicy` does not read `jurisdiction_resolution_active` or
`jurisdiction_evidence_collection_active`. They answer different
questions on different axes (an engineering precondition per (tenant,
operation_class); a lawful-basis switch per (tenant, evidence_type); a
policy-content version per (licensing jurisdiction, operation_class)).
**Binding on the future wiring phase:** production enforcement must
require *all three* independently — `jurisdiction_resolution_active(tenant,
op).active`, an `active` evaluation policy version, and (for player
evidence) `jurisdiction_evidence_collection_active` — never inferred from
one another.

## Decision 7 — `ErrPolicyVersionMismatch`'s operational consequence

`ResolveEvaluationPolicy` refuses to return a policy whose stored
`precedence_policy_version` does not exactly equal the compiled-in
`jurisdiction.PrecedencePolicyVersion`. **Operational consequence, stated
plainly:** bumping `PrecedencePolicyVersion` in a future Phase C revision
disables every stored `active` policy until each is re-authored and
re-approved against the new algorithm version. This is the correct
fail-closed direction (a policy approved against one evaluation algorithm
is not automatically approved against a different one) and costs nothing
today (zero rows exist). **The phase that first bumps
`PrecedencePolicyVersion` in production must ship a documented
re-approval runbook** — recorded here as a named prerequisite of that
future work, not built now.

## Decision 8 — no upper bound on `max_location_signal_age_seconds`

The database rejects zero, negative, and non-integer-second values, but
imposes **no maximum**. Any cap (24h, 30 days, …) would itself be an
invented policy number no regulator or licence has specified. **Residual
risk, named for `security`:** a very large configured value functionally
disables the freshness gate while appearing configured. The mitigations
available today are the mandatory `legal_review_reference` on any future
`active` row, the audit record on every write, and the `qa` regression
test category covering a very-large-age case (already added,
`evaluation_policy_integration_test.go`). Whether the platform should
impose its own maximum-staleness ceiling is recorded as an explicit
sub-question of HDR-J-9, not decided here.

## Deferred items (named, not built in this phase)

- **Scheduled, future-dated activation.** A row is always born with
  `effective_from = now()` (DB-set, unconditional) — a real capability
  someone will eventually want (author a version today, effective next
  Monday) is not built. When it is, it is an explicit, reviewed extension
  of the stamp-times trigger, not a workaround of it.
- **The activation permission and its dual-control ruling.** Writing
  `status = 'active'` requires its own permission and an explicit ruling
  on four-eyes/dual-control, deferred to the phase that has HDR-J-8/HDR-J-9
  content.
- **`jurisdiction_resolutions.reason` CHECK widening (Correction 2 from
  the Phase D ruling).** Migration 0071's CHECK on `jurisdiction_resolutions.
  reason` does not yet include Phase C's three new `Reason` values
  (`no_applicable_evidence`, `evidence_invalid`, `location_signal_unusable`).
  Nothing persists a `PlayerJurisdictionResult` through `Persist` today, so
  widening the CHECK now would be shape-without-a-writer. The phase that
  first persists a player-jurisdiction determination must widen it first.
- **The three-switch requirement (Decision 6) on the wiring phase** —
  restated here as a binding requirement on that future phase, not
  optional guidance.

## Labels (CLAUDE.md no-fake-completion rule)

- Config schema + Go read/write API + mapping seam: `IMPLEMENTED`
  (infrastructure only — zero policy content).
- Any actual jurisdiction policy content (which jurisdiction requires
  location, how stale is stale, which Purpose an OperationClass needs):
  `BLOCKED` on HDR-J-7 / HDR-J-8 / HDR-J-9.
- Production wiring of any of this into an enforcement path:
  `NOT IMPLEMENTED`, deliberately.

## Cross-references

- `docs/governance/stage-4i-canonical-model.md` §3.4 (widened), §6.1
  (amended), §14.6 (PC-GAP status table, updated), new §15 (Phase D
  summary).
- `docs/decisions/0044-human-decision-register-stage-4i-phase-d.md` —
  HDR-J-7, HDR-J-8, HDR-J-9 (new, open).
- `docs/decisions/0042-human-decision-response.md` — HDR-J-2's recorded
  technical consequences (four-eyes posture, versioning) this ruling
  applies mechanically without deciding content.
- `migrations/0075_jurisdiction_evaluation_policy_config.up.sql` /
  `.down.sql`.
- `internal/jurisdiction/operation_purpose.go`,
  `internal/jurisdiction/evaluation_policy.go`,
  `internal/jurisdiction/evaluation_policy_admin.go`.
