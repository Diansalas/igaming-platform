# ADR 0116 — AML Internal Foundation: transaction monitoring, screening, case workflow, SAR export (AML-INTERNAL-FOUNDATION)

- **Status:** **PROPOSED** (design only, 2026-10-12, `architect`). Nothing here is implemented. No migration
  number is claimed: the orchestrator allocates the next free numbers (head is `0132`).
- **Base:** `489995e`. **Owner directive 2026-10-11:** finish provider-neutral KYC/AML orchestration and
  enforcement with mock/test adapters; do not invent provider APIs; legal/regulatory content is configuration
  and a human compliance decision.
- **Gap:** `docs/governance/platform-completion-gap-audit-2026-10-10.md` §6 ("AML transaction monitoring,
  sanctions/PEP screening, SAR/STR case management ... NOT IMPLEMENTED") and item 11 (AML-INTERNAL-FOUNDATION).
- **Requirement sources (Blueprint, verbatim intent):** §4.7 "Screening against sanctions and PEP lists at
  registration and on a recurring schedule, not once. Transaction monitoring with configurable rules, a case
  management queue, and suspicious-activity report export"; build-vs-buy table "KYC / AML — MIXED ... Tiered
  trigger rules, the case queue, per-jurisdiction thresholds, evidence retention, and actual enforcement —
  blocking play and withdrawal is your code"; identity section "Self-exclusion, fraud links and AML case
  history attach to the person, not the player"; §4.8 audit (actor, tenant, entity, before/after, IP, reason;
  append-only; 5–7 year retention). Also `docs/architecture/11-kyc-aml-rg-architecture.md` (AML section and the
  retail "cash-specific AML thresholds" OPEN DECISION, which requires a channel dimension).
- **Binding inputs:** ADR 0096 (KYC enforcement boundary: §2.2 closed outcome set, §2.4 composition order,
  §2.6(e) integer, same-asset arithmetic, §3.1/§3.6 policy-table pattern, §3.7 no seeded values, §4 HD-KYC-1..8);
  ADR 0106 (outbox, single-claimant worker identity, restrictive fence family, alert Kind seeding); ADR 0102
  (alert Kinds); ADR 0104 (tenant-visible platform audit, `subject_tenant_id`); ADR 0006 (hybrid licensing);
  ADR 0015 (`persons` platform scope); ADR 0031 §22 retail note ("a Risk `REVIEW` is not an AML determination");
  ADR 0099/0112 (capability grants), ADR 0103/0029 (four-eyes DB guards); ADR 0112 (brand launch
  authorisation).
- **Code read:** `internal/kyc/enforcement.go`, `provider.go`, `mock_provider.go`, `outbox_worker.go`;
  `internal/payments/kycgate.go:38-56`, `deposit_v2.go:226`, `drive.go:108`, `payout.go:127-133`;
  `internal/withdrawal/withdrawal.go:1179` (`DenyForCompliance`); `internal/risk/*` (limits engine, not AML);
  `internal/providerkind/marker.go` (`Synthetic` / `ProductionEligible`); `internal/eventbus/eventbus.go`
  (STUB, in-memory, not durable); `internal/db/platform_service.go:28-63`; `internal/auth/jwt.go:36-52`,
  `permission.go:730-960`; `migrations/0021` (ledger_transactions: UUID PK, no sequence), `0074`
  (player_jurisdiction_evidence), `0100` (kyc_enforcement_policies/decisions), `0114` (outbox), `0015` (persons).
- **Reviewers (hard gates):** `security`, `identity-compliance`, `ledger-finance` (feed trigger on
  `ledger_transactions`, amount arithmetic), `qa`, `code-reviewer`. `backoffice`, `payments`, `casino`,
  `sportsbook` review their call sites; `product-owner-proxy` reviews scope (§9).

Labels used: **BLUEPRINT** = recorded requirement. **RECOMMENDATION** = reversible engineering default, never a
legal value. **QUESTION** = owner/legal decision, not taken here (§10).

---

## 0. Decisions at a glance

| # | Decision |
|---|---|
| D1 | New package `internal/aml` owned by `identity-compliance`. It is separate from `internal/risk` (limits) and `internal/kyc` (verification); KYC gates import it, never the reverse. |
| D2 | Monitoring input is a **DB-enforced feed**: a deferred constraint trigger on `ledger_transactions` inserts a pointer row into `aml_monitoring_feed` in the same transaction. No producer can forget to emit; no PII in the feed. The in-memory `eventbus` is not used (not durable). |
| D3 | A single-claimant worker `aml_monitoring_worker` (new `PlatformService`) claims feed rows exactly like ADR 0106 D3/D4; evaluation runs in a `WithTenant` session. |
| D4 | Five closed rule types. **Every threshold is a configuration row; nothing is seeded; no code default.** Jurisdiction rules are platform-authored and need `legal_review_reference` to be active; tenants may *add* rules, never disable or loosen a jurisdiction rule. |
| D5 | Alert → case. Case state is a projection of an append-only `aml_case_events` log. Closing a SAR-related case is four-eyes, DB-enforced. SAR export is a provider-neutral document with pluggable per-jurisdiction renderers; **filing is a human act**, recorded by reference only. |
| D6 | `ScreeningProvider` interface (sanctions/PEP) defined by us, with a `MockScreeningProvider` (`Synthetic`). Vendor calls run only from an outbox worker (ADR 0106 pattern). Re-screening cadence is configuration. |
| D7 | Enforcement is **our code**: an `aml_holds` table read by `aml.EvaluateHold`, composed **inside** `kyc.EvaluateEnforcement`, so the five existing gates (deposit, withdrawal hold, payout, casino play, sportsbook play) inherit it with no new call site. Error = deny. |
| D8 | Tipping-off: case data is **need-to-know** by a new role pair, owned by the licence holder (platform or BYO-licence tenant). Player-facing and non-AML staff surfaces see only a generic "account under review" signal. AML actions never produce `subject_tenant_id` audit rows (ADR 0104). |
| D9 | Retention periods are legal configuration rows (no default). No automatic deletion is built here; AML tables are append-only with a legal-hold flag. |

## 1. Context and boundaries

- **What exists.** KYC verification and orchestration (ADR 0028/0095/0106), and KYC enforcement with five
  outcomes and a decision log (ADR 0096, migration 0100). Risk limits (`internal/risk`, ADR 0031) are not AML;
  ADR 0031 §22 forbids treating a Risk `REVIEW` as an AML determination, and this ADR keeps that line.
- **What the platform owns (BLUEPRINT):** trigger rules, case queue, per-jurisdiction thresholds, evidence
  retention, enforcement. **What the vendor owns:** sanctions/PEP list content and name matching.
- **Not in scope:** source-of-funds/EDD document workflow (HD-KYC-2 territory; deferred, `AML-SOF-1`),
  adverse-media screening (not in the Blueprint), crypto travel rule and on-chain analytics (ADR 0008
  custodian, deferred `AML-CRYPTO-1`), regulator e-filing APIs (human filing only), ML/scoring models.

## 2. Transaction monitoring

### 2.1 The feed (minimal, DB-enforced)

No durable generic event outbox exists (`eventbus` is a STUB; the only outbox is KYC-specific, 0114), and
`ledger_transactions` has UUID keys with no commit-ordered sequence, so a high-water-mark cursor over the ledger
would skip late-committing rows. Decision: a per-row claimable feed.

```sql
CREATE TABLE aml_monitoring_feed (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id UUID NOT NULL REFERENCES tenants(id),
  source_kind TEXT NOT NULL CHECK (source_kind IN ('ledger_transaction','jurisdiction_evidence')),
  source_id UUID NOT NULL,                 -- ledger_transactions.id or player_jurisdiction_evidence.id
  enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','claimed','evaluated','failed_terminal')),
  claim_token UUID, lease_until TIMESTAMPTZ, attempts INT NOT NULL DEFAULT 0, last_error_code TEXT,
  UNIQUE (source_kind, source_id)          -- idempotent enqueue, DB-enforced
);
```

- **Producer:** `CREATE CONSTRAINT TRIGGER ... AFTER INSERT ON ledger_transactions DEFERRABLE INITIALLY
  DEFERRED FOR EACH ROW WHEN (NEW.transaction_type IN (<value-boundary set>))`. The set is deposits, deposit
  reversals, withdrawal lifecycle, manual adjustments, and bonus conversion. **Bets/wins are excluded**, so the
  bet path gains no write. Wagering volume for rapid-in-out is read from the ledger at evaluation time. The
  exact set is pinned by a catalogue test against the current `transaction_type` CHECK; `ledger-finance` signs
  it off. The trigger runs `SECURITY INVOKER` under the caller's tenant session, so feed RLS INSERT policy
  `tenant_id = app.tenant_id` applies.
- The deferred timing means `ledger_entries` exist when the row is enqueued. The feed row is a **pointer**:
  the worker reads amounts, accounts and assets from the ledger under RLS. The feed holds no PII and no amount.
- A second producer, on `player_jurisdiction_evidence` (0074) inserts, feeds the geography rule.
- **Worker:** `aml_monitoring_worker` claims with `FOR UPDATE SKIP LOCKED`, lease and DB-clock backoff
  (ADR 0106 D5/D12). Migration adds it to the **restrictive fence family** catalogue gate (ADR 0106 D4).
  Evaluation runs in `WithTenant(claimed.tenant_id)`. After N attempts (RECOMMENDATION: 8) the row becomes
  `failed_terminal` and raises alert Kind `aml.monitoring_failed_terminal` (seeded per ADR 0106 D10).
  **A feed row that is never evaluated is a compliance gap, not a silent pass.** A reconciliation check
  (`internal/reconciliation`) counts monitorable ledger transactions that have no feed row, and feed rows that
  have been pending longer than a configured lag. Either count above zero is an alert.

### 2.2 Rule types (closed set) and configuration

`aml_monitoring_rules` follows the `kyc_enforcement_policies` pattern (0100): append-only, `draft → active →
withdrawn`, `reason_code` required, `legal_review_reference` required for `active` jurisdiction rows, typed
nullable columns with a per-type CHECK, and **no JSONB parameter blob** (ADR 0096 §3.1).

| `rule_type` | Required parameters (all NOT NULL for its type, NULL otherwise) |
|---|---|
| `velocity` | `operation_class` (deposit/withdrawal/any), `window`, and `max_count` and/or `max_sum_minor_units` + `asset_code` |
| `structuring` | `reference_threshold_minor_units`, `asset_code`, `band_bps` (the "near-threshold" band below the reference), `min_count`, `window` |
| `rapid_in_out` | `window` (deposit to withdrawal-request), `max_wager_ratio_bps` (wagered/deposited), `min_sum_minor_units`, `asset_code` |
| `dormant_reactivation` | `dormancy_period`, `min_sum_minor_units`, `asset_code`, `window` |
| `geography_mismatch` | `signal_pair` (closed set: `verified_residence~geolocation`, `declared~geolocation`, `verified_residence~payment_instrument` [PROVIDER DEPENDENT]), `min_occurrences`, `window` |

Common columns: `scope` (`jurisdiction` | `tenant`), `licensing_jurisdiction_id` (jurisdiction scope, resolved
from `tenants.licence_id`, never player-influenced: ADR 0096 §2.2 4(d)), `tenant_id` (tenant scope, RLS),
optional `payment_channel` filter (retail cash, from `11-kyc-aml-rg` OPEN DECISION), `alert_severity`,
`version`, `effective_from`, `created_by_*` (equal to the acting principal by RLS), and `rule_hash`.

- **Additive only.** A tenant-scope rule can never withdraw or override a jurisdiction rule. The evaluator
  applies the union. This avoids any "which is stricter" comparison.
- **Arithmetic.** `NUMERIC(38,0)` in SQL, `big.Int` in Go, never `float64`; same-`asset_code` only. Cross-asset
  aggregation needs an FX basis (ADR 0037) and is QUESTION Q4, exactly as ADR 0096 C6 handles KYC.
- **Netting.** Reversed or charged-back deposits are counted as gross (RECOMMENDATION, conservative). Netting is
  part of Q4.
- **No seeded rows, no code defaults** (ADR 0096 §3.7). Test values carry `TEST FIXTURE VALUE — NOT A LEGAL
  THRESHOLD`. A static test fails if any rule-type parameter has a Go zero-value fallback.
- **Dormancy is observable, not silent.** When a tenant's licensing jurisdiction has zero active monitoring
  rules, a readiness check reports `aml_monitoring_unconfigured`. **Brand launch authorisation (ADR 0112) gains a
  blocking criterion: an active AML rule set and screening configuration exist for the brand's jurisdiction,
  or a recorded owner waiver** (QUESTION Q1).
- **Evaluation.** The pure function `aml.Evaluate(rule, window aggregates) → []Hit` is unit-testable. The
  aggregates come from one RLS-scoped SQL per rule, keyed by `person_id` within the tenant (BLUEPRINT: case
  history attaches to the person). It never reads across tenants (Q5).
- **Hits** go to `aml_alerts`: `(tenant_id, person_id, player_account_id, rule_id, rule_version, rule_hash,
  dedupe_key, evidence_refs uuid[] /* ledger tx ids */, computed_values /* typed */, created_at)`, with
  `UNIQUE(tenant_id, rule_id, dedupe_key)`. `dedupe_key` is the window bucket plus subject, so a redelivered
  feed row cannot duplicate an alert. Append-only.

## 3. Alert → case workflow

### 3.1 Data model (all tenant-scoped, RLS FORCE, append-only triggers deny UPDATE/DELETE/TRUNCATE)

- `aml_cases`: `id, tenant_id, person_id, player_account_id, case_type ('monitoring','screening','manual'),
  owner_scope ('platform','tenant'), opened_at, opened_by_*`. Immutable header. `owner_scope` is derived at open
  time from the tenant's licensing model (ADR 0006): under our licence it is `platform`, under the tenant's own
  licence it is `tenant` (Q2).
- `aml_case_alerts(case_id, alert_id)` with `UNIQUE(alert_id)`: an alert belongs to at most one case. An open
  case for the same person absorbs new alerts (RECOMMENDATION).
- `aml_case_events`: `id, tenant_id, case_id, seq (per-case, UNIQUE(case_id, seq) CAS), event_type, from_status,
  to_status, assignee_id, reason_code, actor_type, actor_id, ip, user_agent, request_id, created_at`. This **is
  the case audit trail** (CLAUDE.md audit fields). Current status, assignee and SAR flag are a projection
  (`aml_case_state` view), never an updated column.
- `aml_case_notes`: `id, case_id, tenant_id, body, author_*, created_at, lint_version`. Append-only;
  corrections are new notes that reference the earlier one.
- Status machine (DB trigger validates `from → to` against the last event): `open → in_review → escalated →
  sar_decision_pending → {closed_no_action, closed_sar_not_filed, closed_sar_filed}`. Also `in_review →
  closed_no_action` and reopen `closed_* → in_review` (new event, reason required). Setting `sar_related = true`
  (event `sar_flagged`) is irreversible for the case.

### 3.2 Rules of the workflow

- **Assignment** is an event. The assignee must hold `aml_case:work` in the owning scope, and is checked
  server-side.
- **PII lint on notes** (`aml.LintNote`). It rejects PAN-like digit runs (Luhn), IBANs, e-mail addresses, phone
  numbers, document-number-shaped tokens, and control characters, and caps length. It is a server-side check
  with a versioned ruleset. **It is a guard against copy-paste of raw identifiers, not a guarantee.** Notes are
  still treated as confidential AML data. The DB stores `lint_version`; a static test pins that every note
  write path calls the lint.
- **Four-eyes closure of a SAR-related case.** A `closed_sar_*` event needs a prior `closure_proposed` event by
  actor A and an approving event by actor B, with B ≠ A and B holding `aml_case:close_approve`. This is enforced
  by a BEFORE INSERT trigger (pattern: 0029 self-approval guard, 0103) with
  `NEW.actor_id <> proposer.actor_id`, plus a person-link check: an approver whose `staff_users.person_id`
  equals the case `person_id` is rejected. A RECOMMENDATION is to require it for any closure of a
  `screening` case with a potential sanctions match too.
- **SLA ageing** is computed from events. `aml.case_sla_breached` is an alert Kind. The SLA durations are
  configuration (no default).

### 3.3 SAR export (provider-neutral document, pluggable renderers)

- `aml.SARDocument` is a Go struct, versioned (`schema_version`), built only from case data. It contains:
  reporting entity (from licence config); subject references (person/account ids, plus KYC-held identity
  attributes fetched **at render time** from the KYC provider reference where available, PROVIDER DEPENDENT);
  the activity window; transactions (ledger tx id, time, type, `amount_minor_units` as a string, `asset_code`,
  `exponent` from the Asset registry); rule hits with rule version/hash; screening matches (vendor
  references); a narrative composed by the officer; and the preparer and approver.
- `type SARRenderer interface { Format() string; Render(ctx, SARDocument) (Rendered, error) }`. Renderers live in
  a registry keyed by `format_code`. Selection is a jurisdiction configuration row (`aml_sar_formats:
  licensing_jurisdiction_id → format_code`, `legal_review_reference` required). The only renderer shipped is
  `generic_json_v1`, labelled **MOCK/reference: not a regulator format**. Real formats (for example, a goAML
  XML profile) are added per jurisdiction after Q3 and need legal validation of the field mapping.
- **Export** needs `aml_sar:export` and a `sar_related` case in `sar_decision_pending` or later. It writes
  event `sar_exported` (format, schema_version, SHA-256 of the output). It does **not** store the rendered file
  (RECOMMENDATION; it is regenerated deterministically from append-only data). The output is streamed with
  `Cache-Control: no-store`.
- **Filing is human.** `closed_sar_filed` requires `external_filing_reference` (free text, linted) entered by
  the officer. The platform never claims a filing happened on its own.

## 4. Screening (sanctions / PEP)

### 4.1 Interface (ours; adapters translate; no vendor API is assumed)

```go
type ScreeningProvider interface {
    ID() string
    Capabilities() ScreeningCapabilities // {Sanctions, PEP bool; SupportsSubjectReference bool}
    Screen(ctx context.Context, in ScreeningRequest) (ScreeningResult, error)
}
type ScreeningRequest struct {
    TenantID, PersonID uuid.UUID; IdempotencyKey string
    Subject ScreeningSubject // EITHER KYCSubjectRef (provider-held identity) OR minimal attributes; never both
    Lists []ListType         // sanctions | pep
}
type ScreeningResult struct {
    Outcome ScreeningOutcome // clear | potential_match | unavailable
    Matches []ScreeningMatch // {ListType, VendorMatchRef string (opaque), Category string (opaque, display only)}
    ProviderReference string; ScreenedAt time.Time
}
```

- The platform stores **no name/DOB today** (`internal/kyc` keeps vendor references). Whether screening runs by
  the KYC vendor's subject reference or needs the platform to hold minimal identity attributes is
  **PROVIDER DEPENDENT and a privacy decision** (Q6). The interface supports both; the mock supports both.
- `MockScreeningProvider` implements `providerkind.Synthetic`. It uses magic subject markers
  (`SYNTH-SANCTION-*`, `SYNTH-PEP-*`, `SYNTH-UNAVAILABLE`) that mirror `kyc.MockKYCProvider`.
  `RefuseSyntheticInProduction` covers it; the completeness scan includes it.
- Vendor raw payloads are never persisted. `RedactedProviderErrorDetail` is reused for errors. Match categories
  are opaque display strings, never parsed into logic.

### 4.2 Orchestration

- `aml_screening_outbox` follows ADR 0106 exactly: phase A is in-tx (HTTP or worker), vendor calls happen only
  from `aml_screening_worker` (new `PlatformService`), lease, CAS on the claim token, and DB-clock backoff. No
  vendor call runs on an HTTP path or inside a money transaction (F-POOL-2).
- **Triggers:** registration (BLUEPRINT), KYC approval (identity attributes now verified), identity-attribute
  change, and scheduled re-screen (BLUEPRINT "recurring"). Vendor ongoing-monitoring push is a future,
  PROVIDER DEPENDENT capability. If it is added, it enters through the existing webhook admission (ADR 0091/0097),
  never a new trust path.
- **Cadence** is `aml_screening_policies` per licensing jurisdiction (`rescreen_interval`, `lists`,
  `legal_review_reference`), with no default. The scheduler enqueues every person whose last completed screen is
  older than the interval. Unconfigured is reported as `aml_screening_unconfigured` (readiness/launch, §2.2).
- `aml_screening_results` is append-only per `(tenant_id, person_id)`. The latest **completed** result is
  current. An `unavailable` result is a retry, never `clear` (same rule as ADR 0096 §2.6(g)).

### 4.3 Match handling and fail-closed holds

| Situation | Hold placed (structural, our code) | Case |
|---|---|---|
| `potential_match`, sanctions | `all_value_movement` (deposit, play, withdrawal, payout) | `screening` case, auto-opened |
| `potential_match`, PEP | `withdrawal` + `payout` (RECOMMENDATION; widening is config, narrowing is Q7) | `screening` case |
| Screening required and **no completed result** (new player, vendor down) | `withdrawal` + `payout` | none; retry plus alert when overdue |
| Confirmed sanctions match (officer decision) | `all_value_movement`, persists until an officer lift with four-eyes | case stays SAR-flag eligible |
| False positive (officer decision) | lift via event; the match ref is recorded as `dismissed` so re-screening does not re-alert on the same `VendorMatchRef` | close |

- `aml_holds`: `id, tenant_id, person_id, player_account_id NULL (NULL = all of the person's accounts in the
  tenant), scope, source ('screening','case','manual'), case_id, placed_by_*, placed_at, reason_code`.
  Append-only. Lifts are rows in `aml_hold_lifts(hold_id UNIQUE, ...)`. A lift of a sanctions-sourced hold is
  four-eyes (trigger).
- **Enforcement composition (D7).** `kyc.EvaluateEnforcement` calls `aml.EvaluateHold(ctx, tx, person, account,
  operation)` **first**, inside the caller's transaction (no I/O). An active hold whose scope covers the
  operation returns a new outcome **`held`** (deny). This is an explicit, reviewed extension of ADR 0096 §2.2's
  closed set, and ADR 0096 §2.3 gains a row: `held → deny`. An error, or a malformed hold row, gives
  `unavailable` (deny). `withdrawal.DenyForCompliance` and the payout gate (`payout.go:133`) already handle any
  deny. The casino/sportsbook play gates already deny on `!Allowed`. The **player-facing code** for `held` is the
  same generic code used for KYC `pending` (no tipping-off; §5.3). `kyc_enforcement_decisions.matched_trigger`
  records `compliance_hold` only, never a case id or the word "sanction".
- **Not covered by holds:** win/settlement/rollback credits (ADR 0096 §2.4: corrections are not stakes).
  Self-exclusion and RG are unchanged; RG still runs first.
- **Cross-tenant propagation** of a confirmed sanctions match to the same `person_id` at other tenants is
  **not built** (Q5). A platform-scope view lets platform AML officers see that one person has cases at several
  tenants (persons are platform-scope, ADR 0015). Tenants never see it.

## 5. Data model, RLS, visibility, retention

### 5.1 RLS

- All AML tables carry `tenant_id` with `ENABLE` + `FORCE ROW LEVEL SECURITY`, NULLIF GUC predicates copied
  from 0100/0075, explicit INSERT/SELECT policies, and no DELETE policy. Append-only triggers deny UPDATE (except
  the outbox/feed claim columns, which are pinned like 0114) plus DELETE and TRUNCATE.
- **Owner-scope predicate (tipping-off at the database, not the UI).** Case, alert, note, screening-match and
  SAR-event rows carry `owner_scope`. SELECT for a tenant session requires `owner_scope = 'tenant'` **and** a
  new GUC `app.aml_access = 'on'`. That GUC is set by `db` only when the authenticated principal holds
  `aml_case:read` (pattern: the `app.acting_*` setters, ADR 0099 §6). Platform-owned rows are readable only
  by a platform session with `app.platform_admin_principal_id` set **and** `app.aml_access = 'on'`, within
  `canActOnTenant`. So a tenant_admin, support or finance session, even if a handler bug runs the query, gets
  zero rows.
- `aml_holds` is readable by any tenant session (the gates need it), but carries only `scope`/`source` codes.
  The gates read only `scope`.
- Rules: jurisdiction rows are readable platform-wide (like 0100) and writable only by a platform principal
  with `aml_rule:author`. Tenant rows are tenant-scoped.

### 5.2 Audit

- `aml_case_events`, `aml_hold_lifts`, rule lifecycle rows and `sar_exported` events are the append-only audit
  records (actor, tenant, entity, before/after status, IP, reason). **No `audit_log` row with
  `subject_tenant_id` is ever written for an AML action**, because ADR 0104 §5 would surface it to the tenant's
  `PermAuditRead` holders (tenant_admin, support). A static test pins that no `aml.*` action reaches
  `audit_log`, and `audit.TenantPresentation` has no `aml.*` entry. The AML audit export is a separate,
  `aml_case:read`-gated endpoint.

### 5.3 What a tenant operator may NOT see (no tipping-off)

| Surface | Non-AML tenant staff (tenant_admin, support, finance, risk) | Tenant AML officer (BYO-licence, owner_scope=tenant) | Platform AML officer |
|---|---|---|---|
| Existence of a case / alert / SAR flag | **No** | Own-tenant tenant-owned cases | All, per `canActOnTenant` |
| Platform-owned case (our licence) | **No** | **No** | Yes |
| Screening match details | **No** | Tenant-owned only | Yes |
| Hold | Generic "account under compliance review", no reason, source or date of origin | Full | Full |
| Withdrawal/payment deny reason | Generic `compliance_review` | Full via case | Full |
| Rule thresholds (jurisdiction) | **No** (knowing thresholds enables structuring) | Read | Read/author |
| Cross-tenant person linkage | **No** | **No** | Yes |

The player sees only the generic review message. Support scripts must not distinguish AML from KYC review
(`backoffice` copy review, `security` sign-off).

### 5.4 Retention (LEGAL)

`aml_retention_policies(licensing_jurisdiction_id, record_class, retention_period, legal_review_reference)`,
with record classes `case`, `alert`, `screening`, `sar_event`, `monitoring_feed`, and no default. The Blueprint
5–7 year audit floor is **not** asserted as the AML value. **No purge job is built.** Rows are append-only. The
feed table alone gets a documented compaction path for `evaluated` rows after a configured period (a pointer
only; the evidence lives in the ledger and alerts). Erasure requests (privacy) against AML data are a legal
conflict: QUESTION Q8 (relates to HD-RET-1).

## 6. Back Office (`backoffice/src/features/aml/*`) and permissions

- **Screens:**
  1. **AML queue:** alerts and cases; filters by status, type, severity, age/SLA and assignee; tenant selector
     for platform officers only.
  2. **Case detail:** event timeline; linked alerts with evidence (ledger tx ids, amounts rendered with the asset
     exponent); screening matches; notes (lint errors inline); actions (assign, escalate, flag SAR, propose or
     approve closure, place/lift hold, export SAR, record external filing).
  3. **Rule config:** jurisdiction rules (platform) and tenant rules, with draft/activate/withdraw. Activation
     and withdrawal of a jurisdiction rule are four-eyes (0103 pattern), with a diff view of the parameters.
  4. **Screening and retention config:** read-only to tenants.
  5. **Generic review badge** on the player profile for non-AML staff.
- **New permissions:** `aml_case:read`, `aml_case:work`, `aml_case:close_approve`, `aml_hold:manage`,
  `aml_sar:export`, `aml_rule:read`, `aml_rule:author` (platform only), `aml_rule:tenant_author`,
  `aml_screening_config:write` (platform only).
- **New role `aml_officer`** (RECOMMENDATION). `RoleCompliance` is broad (RG, bonus, verification) and AML needs
  need-to-know, so its `aml_*` grants are **not** added to `RoleCompliance` or `RolePlatformAdmin`. The role is
  tenant-scoped for BYO-licence tenants and platform-scoped for our licence. `close_approve` and `hold:manage`
  for sanctions are additionally capability-granted (ADR 0099 grants), so not every officer can approve. Whether
  a separate MLRO tier is needed is Q2.

## 7. Security review needs and test plan

### 7.1 Security review (hard gate, design and diff)

1. Feed trigger: SECURITY INVOKER, `search_path` pinned (TRIGGER-SEARCH-PATH-1), no bypass for any ledger insert
   path.
2. Worker identities and the fence catalogue.
3. The `app.aml_access` GUC: who can set it, and that it cannot be set from SQL by the runtime role outside `db`.
4. Owner-scope RLS.
5. ADR 0104 non-leak.
6. Four-eyes triggers (self-approval, person-link).
7. PII lint bypass vectors (unicode digits, separators).
8. SAR export: authZ, no caching, logging redaction.
9. Mock provider production refusal.
10. Player-facing code equivalence (an oracle test).
11. Rule-threshold confidentiality.

### 7.2 Tests (`qa` owns the gate; integration against real Postgres; `-race`)

- **Unit:** each rule type at, below and above the boundary (`big.Int`, max `NUMERIC(38,0)`); structuring band
  edges; dedupe-key determinism; the status machine table; lint corpus (positive and negative, unicode);
  `SARDocument` deterministic rendering (golden file, hash stable); the outcome mapping `held → deny`.
- **Feed:** every monitorable `transaction_type` enqueues exactly once; bets do not; a rolled-back money tx
  enqueues nothing; a catalogue test fails when a new `transaction_type` is added without classification;
  missing-feed reconciliation detects an artificially deleted row (superuser fixture only).
- **Worker:** concurrency (2+ workers, no double alert; ≥50 repeats per the LF-I3-1 precedent); lease expiry;
  terminal failure plus alert; the claim session can read nothing outside the fence allowlist.
- **Tenant isolation:** a valid tenant-B token with tenant-A ids returns not-found on every AML route (ADR 0096
  §2.6(f) shape); the worker evaluates in the claimed tenant only; a tenant rule never affects another tenant.
- **No tipping-off / no PII leakage:** tenant_admin, support and finance sessions get zero AML rows from SQL
  directly (not only via HTTP); a platform-owned case is invisible to the tenant aml_officer;
  `/v1/admin/audit-log/platform-actions` shows nothing after a full case lifecycle; player API responses for
  `held` and KYC `pending` are byte-identical apart from correlation ids; logs and alert payloads hold no
  names, notes or match categories (log-capture test).
- **Fail-closed:** screening `unavailable` places a withdrawal hold; `EvaluateHold` with an injected DB error
  denies at all five gates; a malformed hold denies; an unconfigured jurisdiction reports readiness but never
  `clear`; a vendor outage never writes `clear`.
- **Adversarial:**
  - structuring split across two accounts of one person (same tenant) is detected (person-keyed);
  - deposit just under the reference by 1 minor unit;
  - pending deposits cannot hide volume (settled ledger only, plus the reconciliation lag alert);
  - a client-supplied tenant/jurisdiction/country is ignored;
  - self-approval and same-person approval of closure are rejected by the DB;
  - a lift race against a payout (lock order per ADR 0082: the hold read happens inside the payout tx; a
    concurrent lift commits either before or after, never half);
  - replayed screening callbacks are idempotent.
- **Static:** no `float64` in `internal/aml`; no seeded rule rows; no Go default threshold; every note write calls
  `LintNote`; no `aml.*` in `audit_log` writers.

## 8. Ordered slices (each one merge, each with its own review gate; stop on any failed gate)

| # | Slice | Owner | Reviewers (hard) | Label at end |
|---|---|---|---|---|
| S1 | Migration: `aml_holds`/lifts, the `held` outcome, `EvaluateHold` inside `EvaluateEnforcement`, generic player code | identity-compliance | security, ledger-finance (payout tx), payments, casino, sportsbook, qa, code-reviewer | IMPLEMENTED (manual holds only) |
| S2 | Screening interface, mock, outbox + `aml_screening_worker`, results, policies, match → hold/case stub | identity-compliance + integrations | security, qa, code-reviewer | MOCK / PROVIDER DEPENDENT |
| S3 | Case model: events, notes, lint, status machine, four-eyes, owner-scope RLS, `app.aml_access`, role and permissions | identity-compliance + backend | security (blocking), qa, code-reviewer | IMPLEMENTED |
| S4 | Feed trigger, `aml_monitoring_worker`, rule table, the five evaluators, alerts, reconciliation counts, alert Kinds | identity-compliance + backend | ledger-finance (blocking on the trigger), security, qa, code-reviewer, devops (wiring) | IMPLEMENTED (no rule values) |
| S5 | SAR document, renderer registry, `generic_json_v1`, export/filing events | identity-compliance | security, qa, code-reviewer | IMPLEMENTED (MOCK format) |
| S6 | Back Office screens + OpenAPI | backoffice + frontend | security (tipping-off copy), ux-design, qa | IMPLEMENTED |
| S7 | Launch-authorisation criterion, readiness checks, retention-policy table (no purge) | architect + identity-compliance | security, product-owner-proxy | IMPLEMENTED (values BLOCKED on Q1/Q8) |

S1 first (enforcement is our obligation; useful with manual holds before monitoring exists). S3 precedes S2's
auto-case and S4's alert-to-case linking. Architect review is required at S1, S3 and S4 (cross-domain).

## 9. Consequences, risks, alternatives

- **Consequences:** five gates get AML enforcement without new call sites. ADR 0096 §2.2/§2.3 are amended (sixth
  outcome). Two new `PlatformService` identities, about eight alert Kinds and one new role are added.
  Value-boundary ledger inserts get one deferred insert (no bet-path cost); `qa` sets a p95 budget at S4.
- **Risks:**
  - an unconfigured jurisdiction means no monitoring (mitigated by readiness and the launch criterion);
  - false-positive load on officers (MOCK cannot measure this);
  - the PII lint gives false comfort (disclosed);
  - screening input data depends on Q6.
- **Rejected alternatives:**
  - publishing from application code (forgettable, and not atomic with the ledger);
  - a ledger high-water-mark cursor (skips late commits);
  - extending `internal/risk` (ADR 0031 §22);
  - a JSONB rule DSL (unreviewable, ADR 0096 §3.1);
  - tenant visibility via UI hiding (fails open);
  - writing AML actions to `audit_log` (ADR 0104 leak);
  - a new deny call at each gate (five more places to forget).
- **Product-owner-proxy scope check.** Every item maps to Blueprint §4.7/§4.8 or the build-vs-buy table. SOF/EDD,
  adverse media, crypto travel rule and ML scoring are explicitly deferred (§1).

## 10. Owner / legal questions (minimum set; recommendations are not decisions)

| ID | Question | Recommendation |
|---|---|---|
| Q1 (HD-AML-1) | Should brand launch be blocked until the jurisdiction has active monitoring rules and a screening cadence (or a recorded waiver)? | Yes, block. Values come from compliance/legal per jurisdiction, Anjouan first. |
| Q2 (HD-AML-2) | Tipping-off and ownership: for our-licence tenants, does the tenant operator see nothing beyond a generic review badge? For BYO-licence tenants, does their own AML officer own their cases? Is a separate MLRO approval tier needed? | Yes, yes. A single `aml_officer` role plus a capability grant for closure/sanction-lift approval. Legal confirms per jurisdiction. |
| Q3 (HD-AML-3) | Which SAR/STR format should come first, and who files? | Anjouan's FIU format first, if one exists; until then `generic_json_v1` plus manual filing by the named compliance officer. |
| Q4 (HD-AML-4) | Monitoring values: thresholds, windows, structuring reference amounts, cross-asset aggregation, netting of reversals (shared with HD-KYC-1). | Per-asset only at launch; gross (no netting); values from legal. |
| Q5 (HD-AML-5) | Should a confirmed sanctions match on a person propagate holds across all tenants? Should monitoring aggregate across tenants? | Propagate the **hold only** (no case detail) for sanctions; no cross-tenant monitoring aggregation. Needs legal/data-sharing review for BYO-licence tenants. |
| Q6 (HD-AML-6) | Screening input: screen via the KYC vendor's subject reference, or store minimal identity attributes (name, DOB, nationality) on-platform? | Prefer vendor subject reference (no new PII store). Depends on vendor choice (PROVIDER DEPENDENT); privacy review if attributes are stored. |
| Q7 (HD-AML-7) | Hold scope on a PEP potential match, and the re-screening interval. | Withdrawal and payout hold until review. Interval set by legal; no default. |
| Q8 (HD-AML-8) | Retention period per AML record class, and precedence of AML retention over erasure requests (with HD-RET-1). | Retain under AML obligation, with pseudonymisation after the period. Period set by legal; no default is asserted. |

None of these is legal interpretation by this ADR; the mechanism takes each value without a redesign.
