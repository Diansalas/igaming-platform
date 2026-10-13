# ADR 0116 — AML Internal Foundation: transaction monitoring, screening, case workflow, SAR export (AML-INTERNAL-FOUNDATION)

- **Status:** **ACCEPTED — slices S1 and S3 IMPLEMENTED on branch (2026-10-13, see §12); S2, S4, S5, S6, S7 NOT
  IMPLEMENTED** (revision 2, 2026-10-12, `architect` with the orchestrator). Q1–Q8 are recorded as
  **orchestrator defaults that the owner or legal can reverse** (§10). They decide mechanism only. **No legal
  value is set here.** Migration numbers: the original text said "after head `0132`"; that is stale. The head at
  implementation time was `0136`, `0137`-`0139` are reserved for ADR 0119, and S1+S3 use **`0140`, `0141`, `0142`**.
- **Revisions:**

  | Rev | Base | Change |
  |---|---|---|
  | 1 | `489995e` (commit `ef681cf`) | Initial design, PROPOSED |
  | 2 | `ef681cf` | Applies the security review (**REJECT as written**: B1–B5, C1–C9, L1–L3) and the ledger-finance review (**APPROVE WITH CONDITIONS**: 1 BLOCKER plus conditions and LOWs). The feed-trigger ruling reconciles B5 with the LF blocker (§2.1). Every item is mapped in §11. |

- **Owner directive (2026-10-11):** finish provider-neutral KYC/AML orchestration and enforcement with mock/test
  adapters. Do not invent provider APIs. Legal and regulatory content is configuration and a human compliance
  decision.
- **Gap:** `docs/governance/platform-completion-gap-audit-2026-10-10.md` §6 and item 11.
- **Blueprint sources:**
  - §4.7: "Screening against sanctions and PEP lists at registration and on a recurring schedule, not once.
    Transaction monitoring with configurable rules, a case management queue, and suspicious-activity report
    export".
  - Build-vs-buy "KYC / AML — MIXED": the trigger rules, case queue, per-jurisdiction thresholds, evidence
    retention and "actual enforcement — blocking play and withdrawal is your code".
  - Identity: "AML case history attach[es] to the person, not the player".
  - §4.8: append-only audit.
  - `docs/architecture/11-kyc-aml-rg-architecture.md`: the AML section and the retail cash-threshold OPEN
    DECISION.
- **Binding inputs (ADRs):**
  - 0096: the KYC boundary — §2.2 outcomes, §2.3, §2.4, §2.6(e)(f)(g), §3.6, §3.7, HD-KYC-1..8.
  - 0106: outbox, single claimant, fence family, alert Kind seeding.
  - 0110: signed actor proof (`actor_proof_require`, migration 0120:119) and THREAT-MODEL-ARBITRARY-SQL-1.
  - 0099/0112: capability grants and acting sessions. 0082: lock order. 0102: alert Kinds.
  - 0104: `subject_tenant_id`. 0006: hybrid licensing. 0015: `persons`. 0007/0037: conversions.
  - 0107: HD-CTF-4/5. 0112: launch authorisation. 0031 §22: Risk is not AML.
- **Code read:**
  - KYC: `internal/kyc/enforcement.go`, `provider.go`, `mock_provider.go`, `outbox_worker.go`.
  - Payments gates: `internal/payments/kycgate.go:37-60`, `deposit_v2.go:226`, `drive.go:108`, `payout.go:127-133`.
  - Acting-session callers: `manual_resolution.go:506`, `withdrawal_hold_resolution.go:267`,
    `internal/adjustment/adjustment.go:203`.
  - Withdrawal and locks: `internal/withdrawal/withdrawal.go:1179`; `internal/jurisdiction/tenant_licence_admin.go:127`
    (`tenants FOR UPDATE`).
  - Platform plumbing: `internal/db/platform_service.go`; `internal/providerkind/marker.go`;
    `internal/eventbus/eventbus.go` (STUB); `internal/auth/jwt.go:36-52`, `permission.go:730-960`.
  - Migrations: 0015, 0021, 0051, 0074, 0100, 0114, 0120.
- **Reviewers (hard gates per slice, §8):** `security` and `ledger-finance` (both reviewed revision 1),
  `identity-compliance`, `qa`, `code-reviewer`. Call-site owners `payments`, `casino` and `sportsbook` review
  their gates; `backoffice` reviews §6.

Labels: **BLUEPRINT** = recorded requirement. **RULING** = binding design rule from this revision's reviews.
**DEFAULT** = an orchestrator default the owner or legal can reverse. **RECOMMENDATION** = a reversible
engineering default, never a legal value.

---

## 0. Decisions at a glance

| # | Decision |
|---|---|
| D1 | New package `internal/aml`, owned by `identity-compliance`. Kept separate from `internal/risk` and `internal/kyc`. `internal/kyc` calls into it. |
| D2 | **Atomic, never-unmonitored feed.** An OWNER `SECURITY DEFINER` deferred trigger on `ledger_transactions` performs exactly one `INSERT ... ON CONFLICT DO NOTHING` of a pointer row. The monitorable set is pinned literally by `aml_monitorable_tx_type(text)` (§2.1). |
| D3 | A single-claimant `aml_monitoring_worker` (ADR 0106 D3/D4). It writes an append-only evaluation receipt per feed row. |
| D4 | Five closed rule types. Every threshold is a configuration row: nothing is seeded and there are no code defaults. Rules are additive per tenant. Jurisdiction rules are readable only through a `SECURITY DEFINER` reader. |
| D5 | **No runtime-role table access to AML case data.** Every read and write goes through OWNER `SECURITY DEFINER` functions that call `actor_proof_require` and re-check the AML role or grant in SQL. No GUC grants anything by itself (B1). |
| D6 | SAR closure, a sanctions-hold lift and jurisdiction-rule activation are **proof-bound four-eyes** in the ADR 0110 family (B2). |
| D7 | Holds have no tenant-readable table. Gates call `aml_hold_active(...) RETURNS boolean`. A held operation looks **identical** to KYC pending in every tenant-visible or player-visible surface (B3, C4). |
| D8 | `aml_officer` is created only by a proof-bound four-eyes capability grant. A quorum of at least 2 distinct persons per owner scope is required before activation (B4). |
| D9 | `held` gates only **new value entering play or leaving the platform**. It never gates settlements, corrections or releases. A held balance stays in the ledger untouched (LF). |
| D10 | Screening runs through our `ScreeningProvider` interface plus a MOCK adapter, behind an outbox. The vendor sees only a per-tenant pseudonymous subject reference (C7). |
| D11 | Retention periods are legal configuration. Nothing is purged. Identity erasure fails closed against an AML legal hold (C9). |

## 1. Context and boundaries

- **What exists today:** KYC verification and enforcement (ADR 0096, migration 0100), and Risk limits
  (ADR 0031). A Risk `REVIEW` is never an AML determination (ADR 0031 §22).
- **What the platform owns (BLUEPRINT):** rules, the case queue, per-jurisdiction thresholds, evidence
  retention and enforcement. The vendor owns list content and name matching.
- **Deferred:** SOF/EDD workflow (`AML-SOF-1`), adverse media, crypto travel rule and on-chain analytics
  (`AML-CRYPTO-1`, ADR 0008), regulator e-filing, and ML scoring.

## 2. Transaction monitoring

### 2.1 The feed: RULING reconciling security B5 and the ledger-finance BLOCKER

No durable generic outbox exists (`eventbus` is a STUB). `ledger_transactions` has UUID keys and no
commit-ordered sequence (0021). Feed entries are therefore claimable pointer rows.

```sql
CREATE FUNCTION aml_monitorable_tx_type(t text) RETURNS boolean IMMUTABLE LANGUAGE sql AS $$
  SELECT t IN ('deposit','deposit_reversal','withdrawal_requested','withdrawal_completed',
               'withdrawal_rejected','withdrawal_failed','withdrawal_reversed',
               'manual_adjustment','bonus_conversion') $$;
-- Explicitly NOT monitorable: casino_*, sportsbook_*, bonus_grant, bonus_forfeiture, bonus_reversal, tombstone.

CREATE TABLE aml_monitoring_feed (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL,                       -- copied from NEW; deliberately NO FK to tenants (lock class)
  source_kind TEXT NOT NULL CHECK (source_kind IN ('ledger_transaction','jurisdiction_evidence')),
  source_id UUID NOT NULL,
  enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','claimed','evaluated','failed_terminal')),
  claim_token UUID, lease_until TIMESTAMPTZ, attempts INT NOT NULL DEFAULT 0, last_error_code TEXT,
  UNIQUE (source_kind, source_id));
```

- **The trigger function** (`aml_feed_enqueue_ledger()`):
  - It is OWNER-owned, `SECURITY DEFINER`, with `SET search_path = pg_catalog, public`
    (TRIGGER-SEARCH-PATH-1).
  - Its body is **one statement**: `INSERT INTO aml_monitoring_feed (tenant_id, source_kind, source_id)
    VALUES (NEW.tenant_id, 'ledger_transaction', NEW.id) ON CONFLICT (source_kind, source_id) DO NOTHING`.
  - It has **no SELECT**, never reads a GUC or session metadata, and takes no row lock.
  - It attaches as `CREATE CONSTRAINT TRIGGER ... AFTER INSERT ON ledger_transactions DEFERRABLE INITIALLY
    DEFERRED FOR EACH ROW WHEN (aml_monitorable_tx_type(NEW.transaction_type))`.
  - Because there is no FK to `tenants`, the insert cannot add a `KEY SHARE` lock class against
    `tenant_licence_admin.go:127`'s `tenants FOR UPDATE`, so the ADR 0082 lock order is unchanged.
- **RLS posture.** The insert must succeed under **every session kind that writes `ledger_transactions`**.
  That includes acting sessions with `app.tenant_id` unset (`WithPlatformActingInTenant` at `adjustment.go:203`,
  `manual_resolution.go:506` and `withdrawal_hold_resolution.go:267`). So no tenant-GUC policy may sit on the
  insert path.
  - The table is `ENABLE ROW LEVEL SECURITY` without FORCE, so the definer (owner) bypasses RLS on insert.
  - The runtime role has **no INSERT/UPDATE/DELETE privilege** on the table (REVOKE).
  - Only the definer function inserts, and only the worker's claim functions (definer, platform-service-checked)
    read or transition rows.
  - S4 security review pins this posture with a catalogue test.
- **Failure semantics (RULING).** A feed-write failure may abort the ledger commit **only** because the feed
  insert itself failed. That is atomic and intentional: "never unmonitored" (D2). Nothing else on this path can
  fail. A policy-versus-session mismatch cannot occur by construction.
- **S4 test matrix (blocking).** Post **every** monitorable type from **every** session kind that writes
  `ledger_transactions`:
  - `WithTenant`;
  - each `WithPlatformActingInTenant` caller;
  - every `PlatformService` identity that posts.
  Each case asserts the commit succeeds and exactly one feed row exists. Non-monitorable types assert zero rows.
- **Catalogue test (LF).** It reads the live `transaction_type` CHECK. Any type not literally classified as
  monitorable or excluded **fails the test**. A future `conversion` (ADR 0007) or refund type fails until it is
  classified and `ledger-finance` signs it off.
- **Replay (LF LOW).** An idempotent `ledger.Post` replay, or a late original refused by a tombstone, inserts no
  `ledger_transactions` row, so nothing is enqueued. This is pinned in tests.
- **Second producer.** A definer trigger of the same single-statement shape fires on `player_jurisdiction_evidence`
  (0074) inserts, for the geography rule.

### 2.2 Worker and evaluation receipts (C3)

- `aml_monitoring_worker` is a new `PlatformService`. It claims through a definer function with
  `FOR UPDATE SKIP LOCKED`, a lease, DB-clock backoff and a CAS on `claim_token` (ADR 0106 D5/D12). It is
  added to the restrictive fence catalogue (ADR 0106 D4).
- Aggregates are read in `WithTenant(claimed.tenant_id)`. Rules are read in the worker's platform-service
  session through the rule reader (§2.3, C5).
- Each evaluation writes an append-only `aml_feed_evaluations(feed_id UNIQUE, tenant_id, rule_set_hash,
  evaluated_at, hit_count)` receipt in the same transaction as its alerts.
- If the pointer's source row is missing, the row goes `failed_terminal` **once** with no retry.
- After N failed attempts (RECOMMENDATION: 8) the row goes `failed_terminal`. Kind `aml.monitoring_failed_terminal`
  fires, grouped per tenant per pass.

### 2.3 Rules (closed set) and configuration

`aml_monitoring_rules` follows the 0100 pattern:
- append-only, with the lifecycle `draft → active → withdrawn`;
- `reason_code` is required, and `legal_review_reference` is required for an active jurisdiction rule;
- parameters are typed nullable columns with a per-type CHECK. There is no JSONB.

| `rule_type` | Parameters |
|---|---|
| `velocity` | `operation_class`, `window`, `max_count` and/or `max_sum_minor_units` + `asset_code` |
| `structuring` | `reference_threshold_minor_units`, `asset_code`, `band_bps`, `min_count`, `window` |
| `rapid_in_out` | `window`, `max_wager_ratio_bps`, `min_sum_minor_units`, `asset_code` |
| `dormant_reactivation` | `dormancy_period`, `min_sum_minor_units`, `asset_code`, `window` |
| `geography_mismatch` | `signal_pair` (`verified_residence~geolocation`, `declared~geolocation`, `verified_residence~payment_instrument` [PROVIDER DEPENDENT]), `min_occurrences`, `window` |

- **Common columns:**
  - `scope` (`jurisdiction` | `tenant`);
  - `licensing_jurisdiction_id`, resolved from `tenants.licence_id` and never player-influenced;
  - `tenant_id` for tenant-scope rules;
  - an optional `payment_channel` filter;
  - `alert_severity`, `version`, `effective_from`, `created_by_*` and `rule_hash`.
- **Rule visibility (C5).** No `USING (true)` read policy (unlike 0100). Rules are read only through the definer
  functions `aml_rules_for_jurisdiction(...)`. The functions check either the `aml_monitoring_worker` service
  identity, or an actor proof for `aml_rule:read`. Thresholds are confidential, because knowing them enables
  structuring.
- **Additive only.** Tenant rules add to jurisdiction rules. They can never withdraw or override one.
- **No seeded rows and no Go defaults** (ADR 0096 §3.7). Fixture values are marked `TEST FIXTURE VALUE — NOT A
  LEGAL THRESHOLD`. A static test enforces this.

### 2.4 Amount semantics (ledger-finance, binding)

- **Source of amounts.** Every aggregate comes from `ledger_entries` legs on the person's `player_cash` wallets:
  - per entry `asset_code`, in `NUMERIC(38,0)` / `big.Int`, never `float64`;
  - windowed on `posted_at`;
  - **never** from `wallet_balance_projection` or any cache, and **never across assets**.
- **Wagered volume** for `rapid_in_out` = `casino_bet` + `sportsbook_bet` stake legs, minus `casino_rollback` /
  `sportsbook_void` stake returns. Wins are excluded.
- **Withdrawals.** A withdrawal counts **once**, at `withdrawal_requested`. A `withdrawal_rejected` or
  `withdrawal_failed` release is not a second outflow.
- **Deposits** count gross. A reversal is not netted (DEFAULT, Q4).
- **Windows.** A rolling window is anchored at each fed transaction's `posted_at`. `dedupe_key = (rule_id,
  version, person_id, anchor ledger tx id)`, with `UNIQUE (tenant_id, rule_id, dedupe_key)` on `aml_alerts`.
- **Person keying.** Aggregation is per `person_id` within one tenant (BLUEPRINT), never across tenants (Q5).

### 2.5 Missing-feed reconciliation (LF)

- **Scope.** It covers only transactions posted after `aml_feed_cutoff_at`, recorded by the S4 migration, and
  inside the non-compacted window.
- **Compaction.** Compacting `evaluated` rows keeps a per-tenant watermark. The check never reports compacted
  history.
- **Backfill.** Whether to backfill history before the cutoff is an explicit S4 decision. It is recorded either
  way and never implied.
- **What it counts.** The check runs per tenant under `WithTenant` (`internal/reconciliation`). It counts:
  - monitorable transactions with no feed row;
  - feed rows pending beyond the configured lag;
  - `evaluated` rows with no receipt (C3).
  Any count above zero raises an alert.
- **Readiness.** An unconfigured jurisdiction reports `aml_monitoring_unconfigured`. This is a readiness and
  launch input (Q1).

## 3. Alert → case workflow

### 3.1 Data model and access (B1)

The tables are `aml_alerts`, `aml_cases`, `aml_case_alerts`, `aml_case_events`, `aml_case_notes`,
`aml_screening_results`, `aml_screening_matches`, `aml_sar_artefacts`, `aml_enforcement_decisions` and
`aml_holds`.

- All carry `tenant_id` and `owner_scope` (`platform` | `tenant`, from the licensing model at open time,
  ADR 0006), with ENABLE + FORCE RLS.
- **No SELECT/INSERT/UPDATE/DELETE policy and no privilege for `igaming_runtime`.**
- All access goes through OWNER-owned `SECURITY DEFINER` functions with `search_path` pinned. Each function:
  1. calls `actor_proof_require(actor, scope, target_tenant, 'aml_<object>:<verb>', target, payload_hash)`;
  2. re-checks in SQL that the actor's `staff_users` row holds `aml_officer` and, where needed, an active
     capability grant, for that `target_tenant` and `owner_scope`;
  3. enforces `owner_scope`: a tenant-scope actor only reaches `owner_scope='tenant'` rows of its own tenant, and
     a platform actor reaches `platform` rows within `canActOnTenant`.
- Worker paths use separate definer functions that check the `PlatformService` identity.
- **No GUC grants anything on its own.** The revision-1 `app.aml_access` is removed. This holds even under
  THREAT-MODEL-ARBITRARY-SQL-1 (ADR 0110): a session that can run arbitrary SQL as the runtime role still cannot
  read AML data without a valid proof.
- **Append-only.** Definer functions only `INSERT`. Triggers deny UPDATE, DELETE and TRUNCATE, except the
  outbox and feed claim columns, which are pinned like 0114.
- **Case history.** `aml_case_events` (per-case `seq`, CAS-unique) is the audit trail: actor, tenant, entity,
  from/to, IP, reason. Case state is a projection.
- **Status machine** (the trigger validates against the last event): `open → in_review → escalated →
  sar_decision_pending → {closed_no_action, closed_sar_not_filed, closed_sar_filed}`. Other transitions:
  - `in_review → closed_no_action` (not SAR-related);
  - reopen `closed_* → in_review`, with a reason.
  - `sar_flagged` is irreversible.
- **Foreign keys (C9).** All FKs to `persons` and `player_accounts` are `ON DELETE RESTRICT`.

### 3.2 Four-eyes (B2)

- **Scope.** SAR-related closure, a sanctions-hold lift, and jurisdiction-rule activation or withdrawal join the
  **ADR 0110 proof-bound table family**:
  - the proposal and the approval are separate events, each carrying a signed proof;
  - the approval binds to the proposal's `(case_id, seq)` **and** a SHA-256 content hash of the proposal;
  - a later event (a reopen, a new note on the outcome, or a hold change) **invalidates** pending proposals.
- **DB trigger.** It enforces:
  - approver `actor_id` ≠ proposer `actor_id`;
  - approver `staff_users.person_id` ≠ proposer `person_id`;
  - **neither** person equals the case subject `person_id`.
- **Staff identity.** Every account holding `aml_officer` must have a non-NULL `staff_users.person_id`. A trigger
  on the grant, and a check in the definer re-check, enforce this.
- **PII lint on notes (L2).** `aml.LintNote` runs server-side inside the note definer path, and the stored
  `lint_version` is recorded.
  - It rejects Luhn PAN runs, IBAN, e-mail, phone numbers, document-shaped tokens and control characters.
  - Input is normalised with NFKC first.
  - The corpus covers fullwidth and Arabic-Indic digits and zero-width separators.
  - It is disclosed as a guard, not a guarantee. Notes are inside the HD-RET-1 pseudonymisation scope (C9).
- **SLA.** SLA durations are configuration with no default. The alert Kind is `aml.case_sla_breached`.

### 3.3 SAR export (C8)

- **The document.** `aml.SARDocument` is versioned and built from case data. Amounts are `amount_minor_units`
  (a string) plus `asset_code` and the exponent. Subjects are referred to by platform ids.
- **Vendor identity attributes**, where needed, are fetched by `aml_sar_worker`. This is an outbox worker, never
  on an HTTP path (F-POOL-2).
  - The fetch goes into an **encrypted, AML-scoped, retention-governed** `aml_sar_artefacts` row.
  - The export SHA-256 covers that artefact.
  - Export logs and events record ids and the hash only.
- **Renderers.** `SARRenderer { Format(); Render(ctx, SARDocument) }`. The registry is keyed by
  `aml_sar_formats(licensing_jurisdiction_id → format_code, legal_review_reference)`. Only `generic_json_v1`
  ships, labelled MOCK (not a regulator format).
- **Export** requires `aml_sar:export` plus a proof. The response uses `no-store`.
- **Filing is human.** `closed_sar_filed` needs an `external_filing_reference` that has passed the lint.

## 4. Screening (sanctions / PEP)

### 4.1 Interface (ours; no vendor API assumed)

```go
type ScreeningProvider interface {
    ID() string
    Capabilities() ScreeningCapabilities // {Sanctions, PEP, SupportsSubjectReference bool}
    Screen(ctx context.Context, in ScreeningRequest) (ScreeningResult, error)
}
type ScreeningRequest struct {
    IdempotencyKey string
    SubjectRef     string            // per-tenant pseudonymous reference (C7); NEVER persons.id / player_account_id
    Subject        ScreeningSubject  // KYC-vendor subject reference only, until Q6 privacy review allows attributes
    Lists          []ListType        // sanctions | pep
}
type ScreeningResult struct {
    Outcome ScreeningOutcome // clear | potential_match | unavailable
    Matches []ScreeningMatch // {ListType, VendorMatchRef (opaque), Category (opaque display string)}
    ProviderReference string; ScreenedAt time.Time
}
```

- **C7.** `SubjectRef` = HMAC(per-tenant key from the secret store, `person_id`). It is stored in
  `aml_screening_subjects`. The vendor never receives a platform id.
- **Identity attributes** (name, DOB, nationality) storage is **BLOCKED** until the Q6 privacy review.
- **Mock.** `MockScreeningProvider` implements `providerkind.Synthetic`. It recognises the magic markers
  `SYNTH-SANCTION-*`, `SYNTH-PEP-*` and `SYNTH-UNAVAILABLE`. It is refused in production and included in the
  completeness scan.
- **Payloads.** Raw vendor payloads are never persisted. Errors use `RedactedProviderErrorDetail`.

### 4.2 Orchestration

- **Outbox.** `aml_screening_outbox` plus `aml_screening_worker` follow ADR 0106. No vendor call runs in an HTTP
  path or a money transaction.
- **Triggers:** registration, KYC approval, attribute change, and the scheduled re-screen. A vendor push, if one
  is ever added, enters only through webhook admission (ADR 0091/0097).
- **Cadence.** `aml_screening_policies(licensing_jurisdiction_id, rescreen_interval, lists, effective_from,
  backfill_grace, legal_review_reference)`, with no default. "Unconfigured" is a readiness signal, never `clear`.
- **Results.** The latest **completed** result is current. `unavailable` means retry, never `clear`.

### 4.3 Holds and match handling

| Situation | Hold (structural) | Case |
|---|---|---|
| Sanctions `potential_match` | `all_value_movement` | `screening` case auto-opened |
| PEP `potential_match` | `withdrawal` + `payout` (DEFAULT, Q7) | `screening` case |
| Screening required, no completed result, **and person registered after the policy `effective_from`** (C1) | `withdrawal` + `payout` | none; retry, then alert when overdue |
| Existing persons when a policy activates (C1) | none during `backfill_grace`; the scheduler screens them first; the row above applies after grace | none |
| Confirmed sanctions | `all_value_movement` until a proof-bound four-eyes lift | SAR-eligible |
| False positive | lift event; `VendorMatchRef` is recorded `dismissed` | close |

- **`aml_holds` constraints (C2).**
  - A composite FK `(tenant_id, player_account_id) → player_accounts(tenant_id, id)`.
  - A trigger requires `person_id = player_accounts.person_id` in the same tenant. When `player_account_id` is
    NULL, the person must have an account in that tenant.
  - The table is append-only. Lifts are rows in `aml_hold_lifts(hold_id UNIQUE)`, and **only the hold's
    `owner_scope` may lift**.
- **Cross-tenant propagation (Q5 DEFAULT).** Only **platform-owned confirmed sanctions** holds are ever copied,
  never tenant-placed holds.
- **Rate alert (C1).** `aml.hold_placement_rate` fires when hold placements exceed N per hour per officer
  (N is config).
- **Placement race (LOW).** Placement does not serialise against a gate read already in progress
  (READ COMMITTED). A gate that read before the placement committed may proceed once. The lift/placement race
  test pins exactly this behaviour.

## 5. Enforcement, visibility, retention

### 5.1 Gate composition (B3, C4, C6, L1)

- **`aml_hold_active(p_tenant, p_person, p_account, p_operation) RETURNS boolean`.**
  - OWNER-owned, `SECURITY DEFINER`, `search_path` pinned.
  - Index-backed on `(tenant_id, person_id) WHERE` not lifted.
  - The caller wraps it in `SET LOCAL statement_timeout`.
  - **No tenant session can read `aml_holds`.**
- **`aml.EvaluateHold`** calls it inside the caller's transaction with no I/O. `kyc.EvaluateEnforcement` changes
  as follows:
  - **L1 / C6.** It evaluates the **full KYC path and the hold check unconditionally** and combines them at the
    end. The hold check is never skipped by the no-licence `not_required` early return. KYC still runs when a
    hold is active, so there is no early-return timing signal.
  - **Combination:** deny if either side denies.
  - **New internal outcome `held`** (an extension of ADR 0096 §2.2; §2.3 gains `held → deny`).
- **Evaluator error (C1).** An error or timeout inside the hold check:
  - denies **only the evaluated operation**;
  - raises `aml.hold_eval_failed`;
  - returns the same public result as `held`.
- **Public equivalence (C4).**
  - `held`, KYC `pending` and a hold-evaluator error return the **same** player-facing code, HTTP status and
    decline reason at all five gates.
  - The shared code is `account_review`. A held deposit **never** returns a `kyc_required:` reason. The deposit
    gate's deny reason for KYC `pending` moves to `account_review` too (a player-API change; `payments` and
    `product-owner-proxy` review it in S1).
  - `kyc_enforcement_decisions` (tenant-visible) records the **public outcome `review`** for both `pending` and
    `held`. S1 extends the 0100 CHECK. Historical rows are untouched, and readers map legacy `pending` to
    `review`.
  - The true outcome, and the matched hold, go to `aml_enforcement_decisions`, which is AML-scoped (§3.1).
- **What `held` gates (LF):**
  - deposit initiation, and the T2 cascade claim (`deposit_v2.go:226`, `drive.go:108`);
  - withdrawal request;
  - **payout claim** (`payout.go:133`);
  - new casino stakes and new sportsbook stakes.
- **What `held` NEVER gates (LF).** Each item has its own test with an active `all_value_movement` hold:
  - deposit settlement callbacks and deposit reversal callbacks;
  - wins, settlements, voids and rollbacks;
  - terminal stake returns (Q-GP-5) and cashout;
  - bonus conversion and bonus forfeiture;
  - withdrawal hold releases (reject, fail, HSEC);
  - tombstones.
- **In-flight payouts (LF).**
  - `held` at payout claim goes through `withdrawal.DenyForCompliance`: a terminal `withdrawal_rejected` with key
    `:kyc_denied`, and the funds return to `player_cash`. This is identical to KYC pending and money-neutral.
  - A payout already submitted to the provider is **never recalled**. Its completion, failure and
    hold-resolution postings are never gated.
- **Catalogue test (C6).** Every value-out path must be gated or explicitly listed as non-gated, including
  `ConversionOperation` (ADR 0007), bonus conversion (non-gated by the rule above, so listed) and future custody
  withdrawals (ADR 0008). An unclassified path fails the test.
- **Held balances (LF).**
  - They stay in the ledger with no automatic action (the HD-CTF-4 V1 analogue). Only an officer lift releases
    them; a sanctions lift is four-eyes.
  - **No manual adjustment may seize, zero or refund a held balance.** The adjustment path refuses when an
    active hold exists for that player, and this is tested.
  - Forfeiture, or transfer to an authority, is the new owner question **HD-AML-9** (§10). It needs a new posting
    shape and LF review.

### 5.2 Audit and alerts (C4)

- **Where AML actions are audited.** AML actions are audited in the AML-scoped event tables. **They never write
  `audit_log` with `subject_tenant_id`** (ADR 0104 would expose them), and `audit.TenantPresentation` has no
  `aml.*` entry. A static test enforces both.
- **Alert Kinds.** The `aml.*` alert Kinds, their payloads, notification templates and Back Office badges carry
  **no case id, person id or match category**. They are routed only to `aml_case:read` holders (ADR 0102
  routing).

### 5.3 What a tenant operator may NOT see (no tipping-off)

| Surface | Non-AML tenant staff | Tenant `aml_officer` (BYO licence, `owner_scope=tenant`) | Platform `aml_officer` |
|---|---|---|---|
| Case / alert / SAR flag / screening match | **No** | Own tenant-owned rows | All, within `canActOnTenant` |
| Platform-owned case (our licence) | **No** | **No** | Yes |
| Hold | Generic "account under review" badge, identical to KYC pending | Full | Full |
| Decline reason / decision log | `account_review` / `review` | Full via the AML store | Full |
| Jurisdiction rule thresholds | **No** | Read via the definer function | Read and author |
| Cross-tenant person linkage | **No** | **No** | Our-licence tenants only; BYO is **LAUNCH-BLOCKING** until the L3 controller decision (Q5) |

### 5.4 Retention and erasure (C9)

- **Retention policies.** `aml_retention_policies(licensing_jurisdiction_id, record_class, retention_period,
  legal_review_reference)`, with no default. The Blueprint's 5–7 year audit floor is not asserted as the AML
  value.
- **No purge job is built.** Feed compaction (§2.5) is the only deletion path, and it applies to pointers only.
- **Erasure.** Identity erasure or pseudonymisation **checks the AML legal hold first and fails closed**. The FKs
  are RESTRICT. Notes and SAR artefacts are inside the HD-RET-1 pseudonymisation scope (Q8).

## 6. Back Office and roles (B4)

- **Screens:**
  - AML queue;
  - case detail: timeline, evidence, matches, notes, and actions with proof prompts;
  - rule config, with proof-bound four-eyes for activate and withdraw;
  - screening and retention config, read-only to tenants;
  - the generic review badge.
- **Calls.** Every call reaches the definer functions with a signed proof.
- **Permissions:**
  - `aml_case:read`, `aml_case:work`, `aml_case:close_approve`;
  - `aml_hold:manage`, `aml_sar:export`;
  - `aml_rule:read`, `aml_rule:author` (platform), `aml_rule:tenant_author`;
  - `aml_screening_config:write` (platform).
- **Role `aml_officer`.** It is not added to `RoleCompliance` or `RolePlatformAdmin`.
  - It is **created or assigned only through a proof-bound four-eyes capability grant** (ADR 0099). Tenant
    admins can never create or assign it, and `staff:manage` cannot set it.
  - **Bootstrap:** the first two officers per owner scope are a recorded owner action, following a runbook
    (S3 deliverable).
- **Quorum readiness check.** `aml_officer_quorum` requires at least 2 distinct `person_id`s holding
  `aml_officer` per owner scope. It **blocks activating** screening policies, rules and holds for that scope.
  Holds placed automatically by screening still apply; they fail closed regardless.

## 7. Security review needs and test plan

### 7.1 Security gates (design plus diff)

1. B1 definer-only access: catalogue test that `igaming_runtime` has zero privileges and zero policies on AML
   tables.
2. Proof checks in every function.
3. The SQL re-check of the AML role.
4. Feed trigger and RLS posture (§2.1).
5. The B2 triggers.
6. The B4 grant path.
7. C4 equivalence.
8. C5 rule secrecy.
9. C7 pseudonymity.
10. C8 artefact encryption and logs.
11. C9 erasure.
12. L2 lint.
13. Mock production refusal.

### 7.2 Tests (`qa` owns the gate; real Postgres; `-race`)

- **Feed:**
  - the §2.1 session-kind matrix;
  - the catalogue test;
  - rolled-back transactions enqueue nothing;
  - replay and tombstone enqueue nothing;
  - a `tenants FOR UPDATE` concurrent with a monitorable post neither deadlocks nor waits (lock-order test);
  - reconciliation: cutoff, watermark and missing receipt.
- **Worker:**
  - at least 50-repeat concurrency with no duplicate alert;
  - lease expiry;
  - missing source goes `failed_terminal` once;
  - alerts are grouped per tenant.
- **Amounts:**
  - boundaries at ±1 minor unit;
  - `NUMERIC(38,0)` max;
  - per-asset isolation;
  - wagering netting (rollback/void);
  - a withdrawal counted once;
  - projection or cache never read (static check plus a test with a corrupted projection).
- **Isolation:**
  - a valid tenant-B token with tenant-A ids returns not-found (ADR 0096 §2.6(f));
  - a tenant rule never crosses tenants;
  - **raw SQL as `igaming_runtime`** with arbitrary GUCs set returns zero AML rows and cannot call the definer
    functions without a proof (THREAT-MODEL-ARBITRARY-SQL-1).
- **Tipping-off oracle** at deposit, withdrawal, payout, casino and sportsbook: `held`, KYC `pending` and
  hold-evaluator error give byte-identical code, status and reason. Also:
  - the tenant decision log shows `review` for all three;
  - the ADR 0104 endpoint shows nothing after a full case lifecycle;
  - log capture finds no names, notes or match data.
- **Fail-closed:**
  - an injected hold-evaluator error or timeout denies only that operation and raises `aml.hold_eval_failed`;
  - screening `unavailable` is never `clear`;
  - C1 grace and effective_from behaviour.
- **Ledger (LF):**
  - each non-gated item in §5.1 succeeds under an active `all_value_movement` hold;
  - an in-flight payout is not recalled;
  - a held payout claim goes `withdrawal_rejected` with funds returned to `player_cash`;
  - a manual adjustment against a held balance is refused.
- **Adversarial:**
  - structuring across two accounts of one person;
  - self-approval, same-person approval and subject-person approval are all rejected;
  - an approval after a reopen is rejected (stale seq or hash);
  - a tenant admin cannot grant `aml_officer`;
  - the placement/lift race against a payout;
  - lint bypass corpus (L2).

## 8. Ordered slices (one merge each; stop on any failed gate; architect review at S1, S3, S4)

| # | Slice | Owner | Hard reviewers | Label at end |
|---|---|---|---|---|
| S1 | `aml_holds`/lifts + C2; `aml_hold_active`; `held` + public `review`/`account_review`; L1/C6 composition; LF gated/non-gated tests; adjustment refusal; `aml_enforcement_decisions` | identity-compliance | security, ledger-finance, payments, casino, sportsbook, product-owner-proxy (player code), qa, code-reviewer | IMPLEMENTED (manual holds; activation needs S3 quorum) |
| S2 | Screening interface, mock, C7 subject refs, outbox + worker, policies with C1 grace, match → hold | identity-compliance + integrations | security, qa, code-reviewer | MOCK / PROVIDER DEPENDENT |
| S3 | Case model via definer functions + proofs (B1), proof-bound four-eyes (B2), notes and lint (L2), `aml_officer` grant path and quorum (B4), bootstrap runbook, C9 FKs and erasure check | identity-compliance + backend | **security (blocking)**, qa, code-reviewer | IMPLEMENTED |
| S4 | `aml_monitorable_tx_type`, definer feed trigger, worker + receipts, rules + C5 reader, evaluators per §2.4, reconciliation (cutoff and watermark), session-kind matrix, explicit backfill decision | identity-compliance + backend | **ledger-finance (blocking)**, security, qa, code-reviewer, devops | IMPLEMENTED (no rule values) |
| S5 | SAR document, `aml_sar_worker`, encrypted artefact (C8), `generic_json_v1`, export and filing events | identity-compliance | security, qa, code-reviewer | IMPLEMENTED (MOCK format) |
| S6 | Back Office screens + OpenAPI, proof prompts, generic badge | backoffice + frontend | security (tipping-off copy), ux-design, qa | IMPLEMENTED |
| S7 | Launch criterion (Q1), readiness (`aml_monitoring_unconfigured`, `aml_screening_unconfigured`, `aml_officer_quorum`), retention table | architect + identity-compliance | security, product-owner-proxy | IMPLEMENTED (values BLOCKED on owner/legal) |

S1 ships first, because enforcement is the platform's obligation. Its holds cannot be activated for a scope
until S3 delivers the quorum, so S1 and S3 are released together for any real tenant.

## 9. Consequences, risks, alternatives

- **Consequences:**
  - ADR 0096 §2.2/§2.3 are amended: the `held` outcome, and the public `review` outcome in the tenant log.
  - The deposit player-facing reason for KYC `pending` changes to `account_review`.
  - Value-boundary ledger inserts get one deferred definer insert. Bet paths are unaffected.
  - New pieces: three `PlatformService` identities, about nine alert Kinds and one role.
- **Risks:**
  - An unconfigured jurisdiction has no monitoring. Readiness checks and the launch criterion mitigate this.
  - Officer false-positive load cannot be measured on the MOCK.
  - The lint is a guard only.
  - Screening input depends on Q6.
  - The RLS posture of the definer-insert feed must be re-verified if ownership or roles change
    (PLAT-ROLESPLIT-1).
- **Rejected alternatives:**
  - publishing from application code;
  - a ledger high-water mark;
  - extending `internal/risk`;
  - a JSONB rule DSL;
  - GUC-gated RLS (revision 1, rejected by B1);
  - a tenant-readable hold table (B3);
  - AML rows in `audit_log`;
  - a per-gate deny call.

## 10. Owner / legal questions: orchestrator DEFAULTS (mechanism only, reversible by owner or legal)

| ID | Question | DEFAULT in force until the owner/legal decides |
|---|---|---|
| Q1 HD-AML-1 | Launch gating | Brand launch (ADR 0112) is blocked without active rules, a screening policy and an officer quorum for the scope, or a recorded owner waiver. Values come from legal; Anjouan first. |
| Q2 HD-AML-2 | Tipping-off and ownership | Our licence: the tenant sees only the generic badge. BYO licence: the tenant `aml_officer` owns `owner_scope=tenant`. A single role plus a capability grant for approvals; no separate MLRO tier. |
| Q3 HD-AML-3 | SAR format and filer | `generic_json_v1` (MOCK) plus manual filing by a named officer. The jurisdiction FIU format is added after legal mapping. |
| Q4 HD-AML-4 | Monitoring values and arithmetic | Per-asset only; `NUMERIC(38,0)`/`big.Int`; gross deposits; stake-return netting only for wagering volume (LF). Threshold values: none (legal). |
| Q5 HD-AML-5 | Cross-tenant | Propagate only platform-owned confirmed sanctions holds, as `aml_holds` rows with **no posting**. No cross-tenant monitoring. BYO linkage view: LAUNCH-BLOCKING until the data-sharing/controller decision (L3). |
| Q6 HD-AML-6 | Screening input | Vendor subject reference plus a pseudonymous `SubjectRef`. Attribute storage is BLOCKED pending privacy review. |
| Q7 HD-AML-7 | PEP hold scope, re-screen interval | Withdrawal + payout hold. **LF note:** this lets deposits and play accumulate value that cannot leave; legal must decide knowing that. Interval: none (legal). |
| Q8 HD-AML-8 | Retention vs erasure | Retain under the AML legal hold, then pseudonymise (with HD-RET-1). Period: none (legal). |
| **HD-AML-9** (new, LF) | Forfeiture, or transfer of a held balance to an authority | Not built. No adjustment may seize a held balance. Needs a new posting shape plus LF review (HD-CTF-5 analogue). |
| **Q7 / HD-AML-9 note (LF C2, for the owner and legal)** | Withdrawal-only hold and manual adjustments | As built, ANY active hold refuses ANY manual adjustment of the held person's wallet, including a CORRECTING one. Consequences to decide knowingly: with a `withdrawal_and_payout` hold an erroneous credit cannot be reversed by a debit adjustment, the M2 Step B compensating credit cannot be posted, and the player can still stake the erroneous balance. A later option, **not built**: allow `compensating_entry` debits that name a causation transaction under a `withdrawal_and_payout` hold (never under `all_value_movement`). |

## 11. Review disposition (revision 2)

| Item | Source | Disposition | Where |
|---|---|---|---|
| B1 definer-only access, proof + SQL role re-check, no GUC grant | security | Applied | §3.1, §7 |
| B2 proof-bound four-eyes, actor/person/subject checks, non-NULL person_id, seq + hash binding, reopen invalidates | security | Applied | §3.2 |
| B3 no tenant-readable holds; `aml_hold_active`; public `review` | security | Applied | §5.1 |
| B4 officer grant via four-eyes capability; bootstrap runbook; quorum | security | Applied | §6 |
| B5 + LF BLOCKER: feed trigger cannot fail commits | security + LF | Applied as a RULING: OWNER definer, one INSERT ON CONFLICT, no FK/GUC/SELECT, session-kind matrix | §2.1 |
| C1 effective_from/grace, timeout, per-op deny + alert, placement rate | security | Applied | §4.3, §5.1 |
| C2 composite FK, person trigger, owner-scope lift, propagation limits | security | Applied | §4.3 |
| C3 receipts, missing source terminal, grouped alerts | security | Applied | §2.2, §2.5 |
| C4 identical public result, no `kyc_required:`, id-free Kinds | security | Applied | §5.1, §5.2 |
| C5 rule reader via definer | security | Applied | §2.3 |
| C6 hold before the no-licence branch; value-out catalogue | security | Applied | §5.1 |
| C7 pseudonymous subject ref; attributes blocked | security | Applied | §4.1 |
| C8 encrypted SAR artefact via worker | security | Applied | §3.3 |
| C9 RESTRICT FKs; erasure fails closed; notes pseudonymised | security | Applied | §3.1, §5.4 |
| L1 full KYC then combine | security | Applied | §5.1 |
| L2 lint corpus | security | Applied | §3.2 |
| L3 BYO linkage launch-blocking | security | Applied | §5.3, Q5 |
| Literal monitorable set `aml_monitorable_tx_type` + catalogue test | LF | Applied | §2.1 |
| Amount semantics (ledger legs, posted_at, per-asset, wagering netting, withdrawal once) | LF | Applied | §2.4 |
| Reconciliation cutoff, watermark, explicit backfill, per tenant | LF | Applied | §2.5 |
| In-flight payout via DenyForCompliance; no recall | LF | Applied | §5.1 |
| Gated/never-gated lists, each with a test | LF | Applied | §5.1, §7 |
| Held balances untouched; no seizing adjustment; HD-AML-9 | LF | Applied | §5.1, §10 |
| LOWs: placement race, replay, rolling windows/dedupe; Q4/Q5/Q7 notes | LF | Applied | §4.3, §2.1, §2.4, §10 |

## 12. Implementation record: slices S1 and S3 (2026-10-13, `identity-compliance`)

Branch work, **not merged, not pushed, not yet reviewed by `security` / `ledger-finance` / `code-reviewer`.** Software capability only:
nothing here is a regulatory approval, a filing capability, or a statement that any licence condition is met.

**Numbering.** The "after head `0132`" text in the header was stale. Head was `0136`; `0137`-`0139` stay reserved for ADR 0119;
S1+S3 are migrations **`0140`** (access foundation), **`0141`** (holds and enforcement), **`0142`** (cases and four-eyes). SQLSTATE class
**`AM`** (new; a grep of migrations, Go and docs found no collision; `SF` is ADR 0119's).

**Pairing.** This ADR lists S1 and S3 as separate slices and says they are "released together for any real tenant". That is a release
coupling, not one slice: the work was done together on one branch because a hold cannot be activated without the S3 quorum.

### 12.1 Status labels

| Item | Label | Note |
|---|---|---|
| S1 holds / enforcement foundation (`aml_holds`, `aml_hold_lifts`, `aml_enforcement_decisions`, `aml_hold_active`, `held` composition, five gates, adjustment refusal, C2, L1/C6) | **IMPLEMENTED** (manual holds) | Activation needs the S3 quorum. Hold reason classes are `manual` and `confirmed_sanctions`; nothing creates the second except an officer. |
| S3 case model, proof-bound four-eyes, notes and lint, `aml_officer` grant path and quorum, bootstrap runbook, C9 RESTRICT FKs and erasure check function | **IMPLEMENTED** | `docs/runbooks/aml-officer-bootstrap.md`. |
| Erasure job (HD-RET-1) | **NOT IMPLEMENTED** | Only `aml_person_has_records()` and the RESTRICT FKs exist; erasure fails closed. |
| S2 screening interface, mock, subject refs, outbox, policies, match to hold | **NOT IMPLEMENTED** | No AML vendor and **no screening mock exists in S1/S3**; the MOCK label belongs to S2. |
| S4 monitoring, S5 SAR document / export, S6 Back Office + OpenAPI + auth permission constants, S7 readiness and launch criterion | **NOT IMPLEMENTED** | No HTTP route, UI or role mapping was built. |
| Q5 propagation of platform-owned sanctions holds across tenants | **NOT IMPLEMENTED** | |
| HD-AML-1..9 | implemented as **reversible orchestrator defaults** | Never owner-approved, never legal conclusions. No threshold, period, rule or policy value is seeded. |

### 12.2 What was built

- **Access (0140).** K1 capability `aml_officer` (classification `outside_mandatory_class`), created only by the existing proof-bound
  four-eyes grant flow. `aml_authorize()` is the first statement of every AML definer function: it resolves the session actor
  (tenant or platform-acting only), re-checks in SQL that the actor's own `aml_officer` grant is in force for the tenant AND that
  the grantee is LIVE-eligible now (staff row active, role `compliance` for a tenant grantee or `platform_admin` for a platform
  grantee, live person equal to the person captured on the grant request: the `ledger_adjustment_eligible_grant` pattern), then
  consumes a signed actor proof bound to actor, scope, tenant, operation, target and payload hash. `actor_proof_require` was **not**
  replaced (ADR 0119 may replace it); the AML admission rule (tenant or platform-acting scope only, closed operation names) lives in
  `aml_authorize` alone: `internal/actorproof` `Claims.validate` has no AML-specific rule (an earlier draft of this record said it
  did; that was wrong). The platform half of the liveness check reads a platform staff row the session cannot see, through a narrow
  owner-side definer helper (`aml_platform_grantee_live`, no runtime EXECUTE).
- **Conflict of interest.** Every officer function (hold place / lift / list, alert raise, case open / read / events / notes /
  transition / note / flag / propose / approve) refuses with the four-eyes class (AM011) when the actor's Person is the subject
  Person; the queue omits the officer's own subject. Hold changes, proposals and approvals for one person are serialised by a
  per-person advisory lock, and an approval is invalidated by a monotonic hold-change mark (a sequence), not by timestamps.
- **Holds (0141).** Definer-only tables with FORCE RLS and one owner policy bound to the session tenant, append-only triggers, runtime
  role revoked. `aml_hold_active` is the single enforcement read. Gates: deposit initiation, withdrawal request, payout claim, casino
  stake, sportsbook stake, plus manual-adjustment refusal at submission and execution. Never gated: settlements, reversals, releases,
  completions, rollbacks, bonus conversion and forfeiture, hold-resolution releases, tombstones. **Evidence differs per item:** a
  static test pins the five gate call sites and a live test reads the `transaction_type` CHECK so a new type must be classified
  (STRUCTURAL: covers every item); BEHAVIOURAL tests under an active hold exist for casino win, casino rollback, casino tombstone
  rollback with no original, sportsbook settlement and void, deposit settlement callback, deposit reversal callback, withdrawal
  releases / completions / approvals, in-flight payout completion, and the placement-versus-gate concurrency. **No behavioural test
  exists** for bonus conversion, bonus forfeiture, or the 0124 hold-resolution release (their packages call no gate; the static
  pin is the only evidence) and **sportsbook cashout does not exist in this codebase** (nothing to gate or test).
- **Composition.** `kyc.EvaluateEnforcement` composes KYC and the hold: public outcome `pending`, code `account_review`; tenant decision
  log outcome `review`; the true outcome `held`, the hold id and the KYC outcome stay internal (`aml_enforcement_decisions`). An
  evaluator error is a deny of exactly that operation plus the alert Kind `aml.hold_eval_failed`.
- **Cases (0142).** Alerts (manual source only), cases, notes, an append-only event log with a state machine, proof-bound four-eyes:
  the approval must be the event immediately after its proposal, binds the proposal's content hash, refuses the proposer's actor and
  Person and the case subject, and is invalidated by any hold change since the proposal. Note lint `aml-note-lint-v1` is a guard only.
- **Fixtures.** `internal/testsupport/amlfix` (integration tag only).

### 12.3 Deviations (each disclosed in the code or here)

1. **Quorum is counted per tenant per owner scope**, not per owner scope globally (K1 grants are per tenant; a platform grantee's
   grant is bounded by `acting_grant_max_lifetime`, 4 hours, so **the platform quorum lapses 4 hours after the last grant unless
   grants are re-issued on a cadence**: an operations task, see the runbook). A grantee counts only while LIVE-eligible (active, still
   in the eligible role, same person): a demoted, suspended or re-linked grantee is neither an officer nor a quorum member, in both
   scopes (platform via the owner-side liveness helper).
2. **No AM020 request guard.** A platform session cannot read tenant staff rows (CG010), so the planned request guard was impossible
   for tenant grants. A tenant admin can request `aml_officer` but a distinct **platform** co-approver is always required.
3. **Review rows carry no `matched_trigger`.** The existing KYC decision log records the policy trigger for a `pending`; a `review`
   row (public result of KYC pending, hold or evaluator error) nulls it, because the trigger would reveal which of the three
   produced the outcome (C4). Consequence: the KYC policy trigger is not recoverable from the tenant log for `review` rows; the
   KYC-internal outcome is kept in the AML-scoped decision log for the officer side. This is a design choice for `security` and
   `product-owner-proxy` to confirm.
4. **Sweeper T12 re-gate escalates (T16) instead of recalling;** a payout-claim evaluator failure ends in the existing terminal,
   money-neutral rejection (ADR 5.1).
5. Existing scratch-schema tests that migrate BELOW 0141 now install a "no hold" stub (see 12.5).

### 12.4 Items for `security` sign-off (not self-approved)

- **Relaxed tripwire:** `internal/db/kyc_worker_catalogue_gate_integration_test.go` now exempts policy `aml_owner_session` on `aml_*`
  tables from the "no role-specific policy" check. The policy is TO the table owner only, bound to `aml_session_tenant()`.
- **Signer allow-list:** `internal/actorproof/actorproof_test.go` `staticAllowed` gains `internal/aml`. Also
  `internal/db/acting_setter_static_test.go` (A16) gains `internal/aml/service.go`, and the alerting static rules gain
  `internal/aml/hold_eval.go:raiseEvalFailed`.
- Residuals to probe: `aml_hold_active` and `aml_hold_match` (granted to the runtime role: the gates need them) disclose hold
  existence, and the hold id, to ANY tenant for which a session can be formed under arbitrary SQL with forged settings, not only the
  caller's own tenant (the tenant argument is checked against the session setting, which arbitrary SQL controls);
  `aml_record_enforcement_decision` is forgeable by the runtime credential exactly as the existing KYC decision log is (it now checks
  that a named hold belongs to the named person, security F7); `aml.hold_eval_failed` rows are readable by the subject tenant under
  `alerts_subject_tenant_read` (routing to `aml_case:read` holders is NOT IMPLEMENTED, ALERT-DELIVERY, so the alert is visible to tenant
  staff and infers an evaluator fault, not a hold); notes are plaintext and the lint is an accident guard only; the acting-session
  open audit row (operation `compliance`) is tenant-visible.
- **ACCEPTED residuals (recorded, not fixed):** (F6) the public `review` cover is only as good as the population of KYC-pending
  players: a held player who is NOT KYC-pending is the only `review` among otherwise-`passed` players, so tenant staff comparing
  players can infer "something is different" (not what); (F6) response timing differs slightly between the paths (the hold probe adds
  two round trips and, on a hit, a match lookup); (F5) the matched hold id is returned to the in-transaction gate and written to the
  AML-scoped log, and `aml_hold_match` stays runtime-executable for that lookup.
- **F4 (tipping-off on manual adjustments): DECIDED, option (b), a REVERSIBLE ORCHESTRATOR DEFAULT, NOT owner-approved.** A refusal of
  a manual adjustment caused by an AML hold or a hold-evaluator failure is mapped, at SUBMISSION and at EXECUTION, to the existing
  generic `conflict` that tenant staff already see for an ordinary conflict (a decision on a request that is no longer pending): the
  same HTTP status (409), headers and body, the same denied audit row (same metadata keys, `denied_class` = `conflict:`, no AML token or
  SQLSTATE suffix). At execution the decision transaction rolls back, so the request stays `pending` with no `refusal_code`
  (nothing AML-specific is stored on `ledger_adjustment_requests`; the old `account_review` token and `RefusalAccountReview` are gone) and
  can be decided again once the hold is gone, or expires. The REAL reason (hold or evaluator error, the hold id, operation
  `manual_adjustment`, KYC outcome `not_evaluated`) is written only to the owner-only AML decision log, in its own transaction after the
  rollback, together with the evaluator-failure alert. **Residual:** a staff member can still observe that SOME conflict occurred on a
  request, indistinguishable from an ordinary conflict; a request that keeps conflicting for no visible reason is a weak signal. Options
  (a) a deliberately shared non-AML token and (c) accepting a visible token remain available if the owner prefers them. Tests:
  `TestManualAdjustmentAPI_F4_AMLRefusalIsIndistinguishableFromAnOrdinaryConflict` (byte-identical status / headers / body and audit shape,
  submission and execution) and `TestAMLHold_*` in `internal/adjustment`.

### 12.5 Changed existing tests (setup and allow-lists only, plus the F4 execution assertions below)

adjustment `aml_hold_integration_test.go` (F4: a hold-caused EXECUTION refusal is now a generic conflict with the request left pending, not `refused_at_execution` + `account_review`; the old assertion on `RefusalAccountReview` was replaced, three tests), kyc `enforcement_savepoint_housekeeping_test.go` (stubs the hold read), withdrawal `migration_0132_integration_test.go`, adjustment
`b6_usetime_integration_test.go` (`scratchPoolThroughDir` stubs the read below 0141, covering existing tests that use it),
reconciliation `migration_0097_integration_test.go`, payments `m4_integrity_review_integration_test.go`
(`TestR32_Migration0127_BackfillsExistingDestinationParks` runs the gates on a scratch schema rolled back below 0141: it stubs the
hold read, re-admits `review` in the old CHECK for the fixture payouts and restores the exact pre-0141 shape before migrating up),
alerting `static_wiring_test.go`, db `acting_setter_static_test.go` and `kyc_worker_catalogue_gate_integration_test.go`, actorproof
`actorproof_test.go`. Behavioural changes other tests could depend on: KYC `pending` code is `account_review` (was
`kyc_<op>:pending`); deposit decline reason loses the `kyc_required:` prefix for pending; decision-log outcome `review`; audit
metadata `kyc_outcome` is the public value.

### 12.6 Verification (private database; sanctioned local CI credentials only)

All integration runs used `-tags integration -p 1 -count=1` against a private database built from the worktree's migrations.
- `go test ./... -count=1` (unit): PASS. gofmt: clean. `go vet ./...` and `go vet -tags integration ./...`: PASS.
- golangci-lint 2.9.0 `--build-tags integration --new-from-rev=b480ec6`: 0 issues (run with a private Go and lint cache).
- Every package except payments, integration: PASS, except `internal/httpserver` `TestResolutionIsolation_NormalOperation`, a
  500 ms latency bound that fails intermittently with and without this change (base commit `b480ec6` failed 1 of 3 runs; this
  branch 2 of 3, under similar low load). The rest of the package passes with `-timeout 60m`.
- `internal/payments` integration, one shard per initial letter, each separately: all shards PASS (shards D, E, H and R were first
  disturbed by database connection-slot exhaustion on the shared server and were re-run alone). One real failure was found and
  fixed (`TestR32_Migration0127_BackfillsExistingDestinationParks`, 12.5).
- Migration tests: up/down/up whole-schema snapshot for 0140-0142, pre-flight refusal (AM098) per migration, down refusal (AM099)
  while AML history exists.
- Review round 1 re-run (after the fixes in 12.8): aml, kyc, withdrawal, adjustment, casino, sportsbook, alerting, db, actorproof
  integration PASS; `internal/payments` shards P, K, R, S, W, D, M each run separately PASS; unit tests, gofmt, vet (+integration),
  lint (0 issues) PASS.
- Mutation pass: 105 round-0 mutants plus 39 review-round mutants; every survivor is listed and justified (round 0: 12; round 1: N5,
  N9, N28, N33); the pre-flight and down-refusal branches were not mutated. One mid-pass result (the first gates-lane re-run) was
  INVALID (an unquoted regex pipe made every mutant "fail") and is withdrawn in the kill file; the lane was re-run properly and found
  seven real gaps, now tested. `docs/plans/prh2-hardening-round/adr0116-s1-s3-mutation-kill.txt` and the logs beside it.

### 12.8 Review round 1 disposition (security REJECT as committed, fixable; ledger-finance APPROVE WITH CONDITIONS)

Migrations 0140-0142 were edited in place (unmerged). Software capability only; nothing here is a regulatory claim.

| Item | Disposition | Where |
|---|---|---|
| Sec F1 live eligibility of the grantee (active, role now, same Person) in `aml_authorize` and the quorum, both scopes | **Applied** (platform half through an owner-side liveness helper) | 0140; tests `TestEligibility_*` (demote tenant officers to `support`, suspend / re-link platform and tenant officers) |
| Sec F2 conflict of interest on every officer function; queue filters own subject | **Applied** (AM011) | 0140 `aml_assert_not_subject`, 0141, 0142; `TestConflictOfInterest_EveryOfficerFunctionRefusesOwnSubject` (13 functions + queue, both models) |
| Sec F3 suspended-officer exclusion for platform scope | **Applied** (owner-side helper; the sanctions lift still needs two live Persons) | `TestEligibility_PlatformOfficers_*`, `TestEligibility_SanctionsLiftNeedsTwoLiveOfficers` |
| Sec F4 tipping-off on manual adjustment refusals | **Applied, option (b)** (reversible orchestrator default, not owner-approved): the generic `conflict` at submission and execution, real reason only in the AML decision log; residual in 12.4 | 12.4; `TestManualAdjustmentAPI_F4_*` |
| Sec F5 `aml_person_has_records` runtime EXECUTE; bare acting tenant; hold id | **Applied** (revoked + `init-app-role.sql`; session tenant requires the exact acting shape); hold id to the in-transaction recorder is **accepted residual** (the gate needs `aml_hold_match`) | 0141, 0142, deploy; `TestSessionTenant_BareActingSettingBindsNothing`, catalogue test |
| Sec F6 review-cover population; timing | **Accepted residuals**, recorded | 12.4 |
| Sec F7 decision log names another person's hold | **Applied** (AM007) | 0141; `TestRecordDecision_HoldMustBelongToThePerson` |
| Sec F8 hold-change invalidation race | **Applied**: monotonic `change_seq` mark recorded on the proposal and compared at approval, plus a per-person advisory lock serialising placement, lift, proposal, approval | 0141, 0142; `TestFourEyes_BackdatedHoldChangeStillInvalidatesTheProposal` (the lock itself is concurrency-only and not killable in a single-session test: recorded in the mutation file) |
| Sec F9 tripwire exemption exact | **Applied**: the policy role must equal `pg_class.relowner` of the table | `internal/db/kyc_worker_catalogue_gate_integration_test.go` |
| Sec F10 runbook and the `Claims.validate` claim | **Applied** (runbook; 12.2 and the 0140 header corrected: `Claims.validate` has no AML rule) | runbook, 12.2 |
| Sec F11 `NOT VALID` + `VALIDATE`; note lint; 4-hour cadence | **Applied** (0141); lint documented as an accident guard; cadence in 12.3 / 12.7a / runbook | 0141 |
| LF C1 evaluator failure at payout claim and sweeper T2 / T12 | **Applied**: the existing `unavailable` branch (approved stays approved, retryable 503, reschedule); a definite hold and a KYC denial still deny | `kyc.RetryableOnHoldEvalFailure`, `evaluatePayoutGate`; `TestAMLHold_PayoutClaim_EvaluatorFailure_*`, `TestAMLHold_SweeperT2_*`, `T12_*`, `KYCFailedPlusEvaluatorFailure_*` |
| LF C2 withdrawal-only hold blocks correcting adjustments | **Recorded** for the owner and legal (section 10); 0141 header corrected; later option described, **not built** | 10, 0141 |
| LF C3 alert lost when the adjustment submission rolls back | **Applied**: raised in its own transaction after the rollback | `TestAMLHold_EvaluatorFailureAtSubmission_AlertSurvivesTheRollback` |
| LF C4 never-gated tests | **Partly applied**: behavioural tests added for deposit reversal, casino tombstone rollback, sweeper T2 / T12 under a hold, claim evaluator failure end to end, placement-versus-gate concurrency; **not behaviourally tested**: bonus conversion / forfeiture, 0124 hold-resolution release (structural pin only), sportsbook cashout does not exist | 12.2 |
| LF finding 5 probe round trips | **Recorded** as a pre-release measurement item | 12.7a |

### 12.7a Pre-release items recorded by the review round

- **Hold-probe cost (LF finding 5):** the probe is a SAVEPOINT round trip plus one pipelined batch (two round trips per gated
  operation, including every casino and sportsbook stake). The ADR 0096 s7.7 budget (+2 ms p95) was never re-measured with it. Not
  folded into one batch (the SAVEPOINT must be acknowledged before the batch can rely on it after a Parse failure). Measure before any
  real tenant; S7 readiness.
- **Platform quorum cadence:** platform `aml_officer` grants expire after at most 4 hours (K1 bound); the platform quorum lapses
  without a re-grant cadence.
- **0141 CHECK:** `kyc_enforcement_decisions_outcome_check` is re-created `NOT VALID` then `VALIDATE`d (the scan runs under the weaker
  lock); it only admits one more value so validation cannot fail.

### 12.7 Open for later slices / decisions

S2, S4, S5, S6, S7 as labelled above; HD-RET-1 erasure job; ALERT-DELIVERY to officers; Q5 propagation; platform-grant re-issue
cadence (4-hour bound) as an operations task; whether `review` rows should keep the KYC policy trigger.
