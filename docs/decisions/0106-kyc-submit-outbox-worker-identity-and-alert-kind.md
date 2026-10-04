# ADR 0106 — KYC submission outbox, the dedicated KYC worker identity, and the KYC alert Kind (PRH-2 E1, KYC-SUBMIT-OUTBOX-1)

- **Status:** **PROPOSED, revision 2**, 2026-10-04. Design only. Nothing in this ADR is implemented;
  every item is `NOT IMPLEMENTED` until the E1 implementation merges and passes its review chain (§11).
  Drafted by `architect` for PRH-2 workstream E1. **Implementation may start only after security's
  delta re-check of revision 2** (security design review verdict).
- **Revisions:**

  | Rev | Base | Change |
  |---|---|---|
  | 1 | `3517980` (commit `abced51`) | Initial design |
  | 2 | `abced51` | Applies the identity-compliance design review (ACCEPT WITH CONDITIONS, C1–C6) and the security design review (ACCEPT WITH CONDITIONS, BLOCKING HIGH F1; F3–F9, F12 required; F10/F11 adopted; F13 recorded; Q-S2/Q-S3/Q-S4/Q-R4 rulings). Records: `docs/plans/prh2-hardening-round/reviews/e1-design-identity-compliance.md`, `.../e1-design-security.md`. Mapping in §14. |

- **Base:** main `3517980` (H, I-wire and K2 merged; latest migration `0113_governed_manual_adjustments`;
  `cmd/platform-api/main.go` carries the delimited H and I-wire blocks).
- **Decision type:** cross-domain architecture. It adds a new table and its RLS families, a
  **restrictive fence family on nine existing tables**, a new `PlatformService` member (ADR-level per
  `internal/db/platform_service.go:13-18`), a new alert Kind seeded into the migration-only `alert_kinds`
  vocabulary (ADR 0102), a new background process in `cmd/platform-api`, a read-only derived submission
  state on the staff verification read, and a behaviour change to the player KYC API (create and upload
  become asynchronous towards the vendor).
- **Owner:** `identity-compliance` (implementation). `architect` (this design).
- **Reviewers (hard gates):** `security` (identity, fences, RLS, seeding), `identity-compliance` (IC F3,
  C1–C6), `architect`, `code-reviewer`, `qa`. `ledger-finance` is not a required gate (§11.1). `devops`
  reviews wiring; `product-owner-proxy` decides the optional player-visible enum (§7.2).
- **Binding inputs:** ADR 0105 §2 (HD-PRH2-10) and §3 (HD-PRH2-11), migration **0114**; plan §5-E1,
  §5.0 T-1..T-4, §12, the IC F3 DoD; preflight `reviews/e1-k3-preflight.md` §1; the two r1 design
  reviews; ADR 0095 §15.2, §15.3, §15.3.3 (F2 note), §37; ADR 0096 §2.6(g), §19, §24; ADR 0102 §3.1,
  §4.1, §4.3, §5, §7, §17; ADR 0081 §3.2; ADR 0099/0100 (acting fences 0112/0113, the A-18 replay guard,
  the K2-G1 dynamic probe); CLAUDE.md.
- **Amends:** ADR 0095 §15.2/§15.3 through ADR 0095 **§38** (revised with this revision).
- **Registry:** KYC-SUBMIT-OUTBOX-1 (closes against MOCK at E1 merge; a real vendor stays
  `PROVIDER DEPENDENT`). Related, not fixed here: **NULL-ARM-WRITE-1** (registered by the orchestrator,
  security F2), ALERT-DELIVERY-1 (stays **OPEN**), HD-PRH2-4-OPS, F-POOL-2 (KYC part).

Labels: **Blueprint/plan/ADR** = recorded requirement. **RECOMMENDATION** = an engineering default,
reversible, not a business, legal or compliance threshold. **PLACEHOLDER** = a RECOMMENDATION that a
reviewer explicitly asked to be marked as not final. **QUESTION** = surfaced, not decided here.

---

## 0. Decisions at a glance

| # | Decision |
|---|---|
| D1 | One durable table, `kyc_submission_outbox`, carries both KYC operations: `create` (ADR 0095 §15.2) and `submit` (§15.3). |
| D2 | **Pure outbox, single claimant.** HTTP handlers only perform phase A (domain row + outbox row, one tenant tx). No KYC vendor call remains on any HTTP path. Only the worker claims. The exported inline `kyc.CreateVerification` / `kyc.SubmitVerification` are removed; their phase B/C bodies become unexported executors reachable only from the worker (static test). |
| D3 | The worker identity **`kyc_submission_worker`** is a new closed-set `PlatformService`. It is used for exactly one statement: discovery + claim on `kyc_submission_outbox`. Every tenant-data step runs in plain `WithTenant` transactions whose tenant id is the one the claim statement returned. |
| D4 | **(r2, security F1)** Migration 0114 adds an `AS RESTRICTIVE` fence family denying the worker session on every table where a session with no tenant GUC has any read or write beyond public reference data (nine tables, §3.3). The worker's effective visibility is the outbox plus the public reference allowlist, and it has no write outside the outbox claim. A catalogue gate test keeps this true for every future migration. |
| D5 | Only a validated worker session can move a row to `claimed` (RLS + trigger, every other column pinned). Every post-claim transition requires the claim token in the `WHERE` CAS and a tenant session. |
| D6 | An ambiguous or not-sent result never writes `kyc_verifications`: retry with a DB-clock backoff, then `failed_terminal` + alert. A vendor outage never manufactures a `failed` (IC F3). Enforcement never reads the outbox (INV-KYC-OB-5). |
| D7 | **(r2, F3/IC C2)** At most one live `create` row per player account, DB-enforced by a partial unique index; a repeated create returns the existing orphan verification idempotently. |
| D8 | **(r2, F4/F5/F11/IC C3)** Prepare re-checks the pinned provider is still configured (`provider_deconfigured` → cancelled, no call). A credential binding mismatch is terminal immediately with a distinct discriminator. A deterministic phase-C conflict is terminal (`apply_conflict`). A concurrent staff decision on the orphan cancels the create row (`decided_concurrently`) and never overwrites the staff decision. |
| D9 | One alert Kind, **`kyc.submission_failed_terminal`**, platform-owned with subject tenant, raised in-tx from the tenant phase-C session (`alerting.InTx` + `RaiseGuarded` + `Flush`). ADR 0102 identity rules untouched. |
| D10 | Migration 0114 seeds the Kind through a temporary, literal, `FOR INSERT TO CURRENT_USER`, single-Kind policy created and dropped in one `DO` block (security Q-S2 APPROVED with conditions). FORCE RLS is never toggled on `alert_kinds`. Down runs as one atomic `DO` block and refuses while **any** outbox row exists. |
| D11 | **(r2, IC C1)** The staff verification read gains a read-only derived `submission_state`. A player-visible coarse enum is a product-owner-proxy decision (not built by default). |
| D12 | Time: the database clock is the only clock for due-ness, leases and deadlines; Go computes backoff durations with a pure function; tests use fixtures and the injectable loop ticker (T-1). |

---

## 1. Context

- **Today (verified at `3517980`):**
  - `kyc.CreateVerification` commits an orphan row (`status='unverified'`, `provider_reference NULL`)
    in phase A, calls the vendor in phase B with no tx held, applies the result by CAS in phase C. No
    retry. A phase-C failure after vendor acceptance strands the orphan and leaves every callback for
    that vendor reference on the retryable 503 (ADR 0095 F2, "permanent-503 gap").
  - `kyc.SubmitVerification` is called inline by the upload handler (`kyc_handlers.go:320`). An
    ambiguous result leaves status unchanged (IC condition 2); nothing retries it.
  - ADR 0095 §15.3 / IC condition 5: a durable outbox is a **hard precondition on the first real KYC
    adapter**.
- **Orphan exclusion:** ADR 0096 §2.6(g) — `orphanRowExclusionSQL` in `enforcement.go` (read at
  `:401`; overlay `:491` restricted to approved/rejected). Identity-compliance verified every status
  reader and writer against main; E1 relies on the predicate as-is and does **not** edit
  `enforcement.go`.
- **Alerting:** `alert_kinds` FORCE RLS, SELECT-only policy, deny triggers; only a migration writes it.
  `alerting_session_scope()` refuses every service id but `alert_dispatcher`. Unchanged by E1.
- **NULL-arm exposure (security F1/F2, proven on a private DB):** older dual-scope policies of the form
  `tenant_id IS NULL AND app.tenant_id IS NULL` (and `USING (true)` / `WITH CHECK (true)` arms) admit any
  session with no tenant GUC, including a platform-service session. With only
  `app.platform_service_id='kyc_submission_worker'` set, security inserted a platform `staff_users` row
  with role `platform_admin` and forged a platform `audit_log` row. For the new identity this is closed
  by D4. For `WithoutTenant`, `sportsbook_catalogue_sync` and `alert_dispatcher` it is pre-existing and
  registered as **NULL-ARM-WRITE-1**; E1 does not fix it.

---

## 2. The outbox table (migration 0114)

### 2.1 Columns

```sql
CREATE TABLE kyc_submission_outbox (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants (id),
    verification_id    UUID NOT NULL,
    player_account_id  UUID NOT NULL,                     -- forced from the verification by trigger (F3/IC C2)
    FOREIGN KEY (verification_id, tenant_id)   REFERENCES kyc_verifications (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id),
    operation          TEXT NOT NULL CHECK (operation IN ('create', 'submit')),
    -- F5: charset-checked; the alert's provider_id attribute and discriminator copy it.
    provider_id        TEXT NOT NULL CHECK (provider_id ~ '^[A-Za-z0-9_.-]{1,64}$'),
    document_ids       UUID[] NOT NULL DEFAULT '{}',      -- pinned, ascending text order; empty for create
    idempotency_key    TEXT NOT NULL CHECK (idempotency_key ~ '^(kv:[0-9a-f-]{36}|ks:[0-9a-f-]{36}:[0-9a-f]{64})$'),
    state              TEXT NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('pending', 'claimed', 'sent', 'failed_terminal', 'cancelled')),
    claim_token        UUID NULL,
    claimed_by_service TEXT NULL CHECK (claimed_by_service IS NULL OR claimed_by_service = 'kyc_submission_worker'),
    claimed_at         TIMESTAMPTZ NULL,
    lease_expires_at   TIMESTAMPTZ NULL,
    claims             INT NOT NULL DEFAULT 0 CHECK (claims >= 0),
    failed_attempts    INT NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error_class   TEXT NULL CHECK (last_error_class IN
                           ('ambiguous', 'not_sent', 'lease_expired', 'deferred_tenant_inactive',
                            'credential_binding_mismatch', 'apply_conflict')),
    cancel_reason      TEXT NULL CHECK (cancel_reason IN
                           ('superseded', 'verification_terminal', 'no_documents', 'document_set_changed',
                            'verification_not_submitted', 'provider_deconfigured', 'decided_concurrently')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at        TIMESTAMPTZ NULL,

    CHECK ((state = 'claimed') = (lease_expires_at IS NOT NULL)),
    CHECK ((state = 'claimed') <= (claim_token IS NOT NULL)),
    CHECK ((state IN ('sent', 'failed_terminal', 'cancelled')) = (terminal_at IS NOT NULL)),
    CHECK ((state = 'cancelled') = (cancel_reason IS NOT NULL)),
    CHECK (state <> 'failed_terminal' OR last_error_class IS NOT NULL),
    CHECK ((operation = 'create') = (cardinality(document_ids) = 0)),
    CHECK (operation <> 'create' OR idempotency_key = 'kv:' || verification_id::text),
    CHECK (operation <> 'submit' OR idempotency_key LIKE 'ks:' || verification_id::text || ':%')
);
```

- **No PII rule (binding):** no column holds PII, document content, storage references, vendor
  references, credentials, raw error text or vendor free text. The ids are opaque UUIDs. A later column
  carrying any of those needs a new security review.
- **F10 (adopted):** r1's global `seq` identity column is **removed** (it leaked platform-wide KYC volume
  to tenant readers). Ordering, including supersession, uses `(created_at, id)`. Concurrent uploads whose
  sets miss each other's documents are repaired by the §2.9 `document_set_changed` check.

### 2.2 Indexes and uniqueness

```sql
-- Duplicate submit of the same content is a no-op while a live or sent row exists; after
-- failed_terminal or cancelled the same content may be enqueued again (remedy, §8.3).
CREATE UNIQUE INDEX kyc_submission_outbox_key_live ON kyc_submission_outbox (tenant_id, idempotency_key)
    WHERE state IN ('pending', 'claimed', 'sent');
-- A verification is created with the vendor at most once.
CREATE UNIQUE INDEX kyc_submission_outbox_one_create ON kyc_submission_outbox (tenant_id, verification_id)
    WHERE operation = 'create';
-- F3 / IC C2: at most one LIVE create per player account (DB-enforced, not check-then-insert).
CREATE UNIQUE INDEX kyc_submission_outbox_one_live_create_per_player ON kyc_submission_outbox (tenant_id, player_account_id)
    WHERE operation = 'create' AND state IN ('pending', 'claimed');
CREATE INDEX kyc_submission_outbox_due    ON kyc_submission_outbox (next_attempt_at, created_at, id) WHERE state = 'pending';
CREATE INDEX kyc_submission_outbox_lease  ON kyc_submission_outbox (lease_expires_at) WHERE state = 'claimed';
CREATE INDEX kyc_submission_outbox_by_verif ON kyc_submission_outbox (tenant_id, verification_id, created_at, id);
```

### 2.3 State machine

```
   (phase A, tenant, INSERT) --> pending --(worker: claim, due)--> claimed --(tenant CAS)--> sent
                                  ^  |                              |  |  |-(tenant CAS)--> failed_terminal
                                  |  |                              |  |  '-(tenant CAS)--> cancelled
                                  |  '--(tenant: create of this     |  '--(worker: re-claim, lease expired)--> claimed
                                  |      verification is terminal)  |
                                  |      --> cancelled (verification_not_submitted)
                                  '------(tenant CAS: retry / defer)-+
```

| From | To | Session | Conditions (DB-enforced unless marked Go) | Forced / pinned by trigger |
|---|---|---|---|---|
| (none) | `pending` | tenant | INSERT; §2.5 validation | `state`, counters 0, claim columns NULL, `last_error_class`/`cancel_reason`/`terminal_at` NULL, `player_account_id` from the verification, timestamps |
| `pending` | `claimed` | **worker** | `OLD.next_attempt_at <= now()`; for `submit`, no live `create` for the verification | `claim_token := gen_random_uuid()`, `claimed_by_service := GUC`, `claimed_at := now()`, `claims := OLD.claims + 1`; `lease_expires_at` must be in `(now(), now() + interval '10 minutes']`; **F6 pinned = OLD:** `failed_attempts`, `next_attempt_at`, `last_error_class`, `cancel_reason`, `terminal_at`, and every immutable column |
| `claimed` | `claimed` | **worker** | `OLD.lease_expires_at <= now()` (an unexpired lease is refused) | as above, except `failed_attempts := OLD.failed_attempts + 1` and `last_error_class := 'lease_expired'` (the only two non-OLD values) |
| `claimed` | `sent` | tenant | Go CAS `WHERE id=$1 AND state='claimed' AND claim_token=$2`; same tx as the verification apply + audit | `terminal_at := now()`, `lease_expires_at := NULL` |
| `claimed` | `pending` | tenant | CAS; class ∈ {`ambiguous`, `not_sent`, `deferred_tenant_inactive`}; `next_attempt_at` in `(now(), now() + interval '1 day']`; **F6: `deferred_tenant_inactive` only if `(SELECT status FROM tenants WHERE id = NEW.tenant_id) <> 'active'`** | `failed_attempts := OLD + 1`, or `+ 0` for `deferred_tenant_inactive`; lease NULL |
| `claimed` | `failed_terminal` | tenant | CAS; class set | `failed_attempts := OLD + 1` unless the class is `lease_expired` (already counted at re-claim); `terminal_at := now()` |
| `claimed` | `cancelled` | tenant | CAS; `cancel_reason` set | `terminal_at := now()` |
| `pending` | `cancelled` | tenant | **only** `operation='submit'`, `cancel_reason='verification_not_submitted'`, and the verification's `create` row is `failed_terminal` or `cancelled` (IC Q-R1(a)) | `terminal_at := now()` |
| terminal | anything | any | refused | |

Immutable on every UPDATE: `id, tenant_id, verification_id, player_account_id, operation, provider_id,
document_ids, idempotency_key, created_at`. `claim_token`, `claimed_by_service`, `claimed_at`, `claims`
change only on a worker claim. `updated_at := now()` always.

**"Status never inferred":** no transition writes `kyc_verifications`. Only the `claimed → sent`
transaction applies a verification change, and only from a **definitive** vendor result, through the
existing `applyCreateVerificationResult` CAS or `applyForwardOnlyStatus`.

**`claims` for deferred rows (IC C5):** `claims` counts every claim, so it increments once per deferral
cycle for a non-active tenant's row. It measures worker activity, not vendor attempts; `failed_attempts`
is the retry budget. The runbook says so.

### 2.4 Session helper and guard trigger

`kyc_submission_outbox_session()` returns `'worker'` or `'tenant'` or raises; never `SECURITY DEFINER`:

```sql
CREATE FUNCTION kyc_submission_outbox_session() RETURNS TEXT AS $$
DECLARE
    v_tenant  TEXT := NULLIF(current_setting('app.tenant_id', true), '');
    v_service TEXT := NULLIF(current_setting('app.platform_service_id', true), '');
BEGIN
    IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.acting_tenant_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NOT NULL THEN
        RAISE EXCEPTION 'kyc_submission_outbox: session shape not permitted to write';
    END IF;
    IF v_service IS NOT NULL THEN
        IF v_service <> 'kyc_submission_worker' OR v_tenant IS NOT NULL THEN
            RAISE EXCEPTION 'kyc_submission_outbox: unknown or mixed platform-service session';
        END IF;
        RETURN 'worker';
    END IF;
    IF v_tenant IS NULL THEN
        RAISE EXCEPTION 'kyc_submission_outbox: no tenant session';
    END IF;
    RETURN 'tenant';
END;
$$ LANGUAGE plpgsql STABLE;
```

`kyc_submission_outbox_guard()` (`BEFORE INSERT OR UPDATE OR DELETE` per row; `BEFORE TRUNCATE` per
statement): TRUNCATE/DELETE always refused; INSERT requires `'tenant'` + §2.5; UPDATE runs the
immutable-column check, refuses terminal rows, then admits exactly the §2.3 rows for the session kind and
**assigns every pinned column from OLD explicitly** (F6: a column-by-column list, not "unchanged unless
set", so a Go bug that writes `failed_attempts = 0` is overwritten or refused, never persisted). RLS (§3.2)
is the primary control; the trigger is the backstop; the Go `WHERE` CAS is the claim check.

### 2.5 Insert validation (recompute, never trust)

- `NEW.tenant_id = app.tenant_id` (also RLS `WITH CHECK`).
- The verification exists under the caller's RLS view; `NEW.provider_id = verification.provider_id`;
  `NEW.player_account_id := verification.player_account_id` (forced).
- `create`: verification in the orphan shape; `document_ids = '{}'`; key `'kv:' || verification_id`.
- `submit`: `cardinality >= 1`; ids distinct and in ascending text order; every id is a `kyc_documents`
  row of the same tenant and verification with `status <> 'rejected'`; the key equals the recomputation
  (byte-identical to Go's `submissionIdempotencyKey`), else **raise**:

  ```sql
  'ks:' || NEW.verification_id::text || ':' || encode(sha256(
      (SELECT string_agg(convert_to(d::text, 'UTF8') || '\x00'::bytea, ''::bytea ORDER BY d::text)
         FROM unnest(NEW.document_ids) AS d)), 'hex')
  ```

### 2.6 Phases and the transaction-scope rule

| Phase | Where | Transaction | What |
|---|---|---|---|
| **A (create)** | create handler | **one** `WithTenant` tx with `SelectProvider` | §2.10 idempotent create: orphan row + `kyc.verification_requested` audit + outbox `create` + `kyc.submission_enqueued` audit. 201 (new) or 200 (existing live create returned). |
| **A (submit)** | upload handler | the existing `UploadDocument` tx | document row + `kyc.document_uploaded` audit; gather the current non-rejected set; `INSERT ... ON CONFLICT (tenant_id, idempotency_key) WHERE state IN ('pending','claimed','sent') DO NOTHING` + `kyc.submission_enqueued` audit (`duplicate=true` on conflict). An orphan verification accepts the upload only if its live `create` row is read **inside this same tx** (security Q-R1 condition); otherwise `ErrVerificationNotSubmitted` (409). Terminal verification or empty set: no row. |
| **Claim** | worker | `db.WithPlatformService(ServiceKYCSubmissionWorker)`, **one statement** (§2.7) | |
| **P (prepare)** | worker | `WithTenant(row.tenant_id)` on a **bounded** context (`prepareTimeout`, F9) | re-select `WHERE id=$1 AND state='claimed' AND claim_token=$2 FOR UPDATE` (0 rows → lost claim, stop); read `tenants.status`; re-check the provider (F4); read the verification and, for `submit`, the current documents' ids, types and (only if the adapter needs content) storage references; decide per §2.9. Cancel/defer/terminal decisions write the CAS transition + audit here; *proceed* commits with no write. |
| **B** | worker | **none** (`txscope.Held` must be false) | bounded by `phaseBTimeout` = credential-resolve budget + call timeout: resolve the outbound credential for `(row.tenant_id, row.provider_id)`; binding check (mismatch → §2.8 terminal); if the adapter needs document **content**, fetch it from `DocumentStorageProvider` here, after P committed, never inside a transaction, never in the worker service tx, never persisted in the outbox (IC C5; today `SubmittedDocument` carries ids and types only and the MOCK reads no content); call the vendor with `CallContext{IdempotencyKey: row.idempotency_key}` |
| **C** | worker | `alerting.InTx(NewTenantRunner(pool, row.tenant_id))` on `context.WithoutCancel` + `phaseCTimeout` | re-check the claim `FOR UPDATE`; one §2.8 outcome; `Pending.Flush` after commit |

Rules:
- No vendor call inside any tx closure (existing IO-1C static guard; no exemption).
- The worker service tx executes exactly one statement on `kyc_submission_outbox` (static test, §10.4).
- Phase A writes the outbox row in the same tx as its domain row.
- Phase C applies verification change + outbox transition + audit atomically; a lost claim (CAS = 0 rows)
  rolls back and discards the result (logged with ids and closed classes only).
- **Lock order (Q-R4, accepted by security with conditions):** outbox row (child) then verification
  (parent) in P and C. No path holds a `kyc_verifications` lock and then requests an outbox lock (callback
  and `ReviewVerification` never touch the outbox; phase A takes only the FK `KEY SHARE`, which does not
  conflict with the status CAS's `NO KEY UPDATE`; the reference unique index is partial). Conditions
  adopted: the §10.4 static UPDATE-confinement test, F12 (§2.9), a `40P01` in phase C propagates through
  `RaiseGuarded` (never swallowed; it is not in the swallow allowlist) into the bounded phase-C retry,
  and test 16 runs at `-count >= 20`.

### 2.7 The claim statement (the worker identity's only statement)

```sql
WITH candidate AS (
    SELECT o.id
      FROM kyc_submission_outbox o
     WHERE (   (o.state = 'pending' AND o.next_attempt_at  <= now())
            OR (o.state = 'claimed' AND o.lease_expires_at <= now()))
       AND NOT (o.tenant_id = ANY ($2::uuid[]))     -- per-pass per-tenant cap; ids this pass's own claims returned
       AND NOT (o.operation = 'submit' AND EXISTS (
               SELECT 1 FROM kyc_submission_outbox c
                WHERE c.tenant_id = o.tenant_id AND c.verification_id = o.verification_id
                  AND c.operation = 'create' AND c.state IN ('pending', 'claimed')))
     ORDER BY CASE o.state WHEN 'claimed' THEN o.lease_expires_at ELSE o.next_attempt_at END, o.created_at, o.id
     LIMIT 1
     FOR UPDATE OF o SKIP LOCKED
)
UPDATE kyc_submission_outbox o
   SET state = 'claimed', lease_expires_at = now() + make_interval(secs => $1)
  FROM candidate
 WHERE o.id = candidate.id
RETURNING o.id, o.tenant_id, o.verification_id, o.operation, o.provider_id,
          o.idempotency_key, o.document_ids, o.claim_token, o.claims, o.failed_attempts, o.last_error_class;
```

One row per claim tx (RECOMMENDATION); `SKIP LOCKED` gives concurrent workers disjoint rows; the
trigger's due-ness re-check makes a stale claim raise; `RETURNING` reflects the trigger-forced token. The
tenant id used afterwards is this `RETURNING` value (INV-KYC-OB-4).

### 2.8 Outcomes, retry and backoff

| Phase-B / C result | Outbox (phase C, tenant tx, under the claim) | Verification | Audit (+ alert) |
|---|---|---|---|
| Definitive (and, for create, a non-empty reference); CAS applied | `claimed → sent` | changed only by the existing CAS rules | existing `kyc.verification_submitted` / `..._to_provider` row + `outbox_id`, `claims`, `platform_service` |
| **Create CAS miss** because the orphan was decided concurrently (staff `ReviewVerification`, IC C3) | `claimed → cancelled` (`decided_concurrently`) | **never overwritten or demoted** | `kyc.submission_cancelled` with `vendor_reference_unbound=true` (the reference value is never written); runbook §8.3 step |
| Submit result arriving after a staff/callback forward move | the existing forward-only CAS is a no-op; `claimed → sent` | unchanged (never demoted) | existing row with `status_applied=false` |
| **Ambiguous** (`ProviderError`, transport error, timeout, empty create reference) | `claimed → pending` + backoff, or `→ failed_terminal` when `failed_attempts + 1 >= MaxFailedAttempts` (+ alert) | **unchanged** (IC condition 2) | `kyc.submission_retry_scheduled` / `kyc.submission_failed_terminal`; for submit `ProviderError` the existing failure row is kept as well |
| **Not sent** (credential unavailable, adapter not registered, `ErrProviderCallRefused`) | as ambiguous, class `not_sent` | unchanged | as above |
| **Credential binding mismatch** (F5) | `claimed → failed_terminal` **immediately**, class `credential_binding_mismatch`, no retry | unchanged | `kyc.submission_failed_terminal` + alert with discriminator suffix `:binding_mismatch` |
| **Phase C fails with SQLSTATE class 22 or 23** after the bounded in-item retries (F11, adopted) | new short tenant tx: `claimed → failed_terminal`, class `apply_conflict` (no re-send loop) | unchanged | `kyc.submission_failed_terminal` + alert, suffix `:apply_conflict` |
| Phase C fails otherwise (DB unavailable, `40P01` exhausted) | bounded retry inside `phaseCTimeout` (RECOMMENDATION 3 tries), then the row stays `claimed`; lease expiry → re-claim (`lease_expired`) → re-send with the **same** key | unchanged | none possible; log + counter |

- **Backoff:** `backoff(n) = min(BackoffBase * 2^(n-1), BackoffCap)`, pure Go, no jitter, unit-tested;
  the deadline is `now() + make_interval(secs => $d)` in the database.
- **T-1:** fixtures for `next_attempt_at` / `lease_expires_at`; injectable ticker for the loop. **T-2:**
  no new wall-clock assertion.
- **Retry budget = PLACEHOLDER** (IC finding 7): `BackoffBase` 30 s, `BackoffCap` 30 min,
  `MaxFailedAttempts` 8 (about one hour of outage before `failed_terminal`). Code constants, overridable
  by tests. **Jurisdiction rule (binding):** any KYC submission or completion **deadline** that a
  jurisdiction imposes is jurisdiction configuration (the ADR 0096 §3 configuration model), never a
  worker constant; the worker's budget is a technical retry bound only and must not be presented as a
  compliance deadline (HQ-E1-3). Identity-compliance prefers not terminating on a short outage; the
  placeholder may be lengthened at implementation review without a design change.
- **Other RECOMMENDATIONs:** `Lease` 60 s; `prepareTimeout` 5 s; credential-resolve budget 5 s;
  `phaseCTimeout` 5 s (existing); per-pass item cap 100; per-pass per-tenant cap 20.
- **F9 lease inequality:** `ValidateForLoop` refuses
  `Lease < 2 * (prepareTimeout + resolveBudget + createVerificationCallTimeout + phaseCTimeout) + 5 s`
  (with the defaults: 2 × 25 s + 5 s = 55 s ≤ 60 s). P, B and C each run on their own bounded context,
  so an item cannot outlive its lease in normal operation.

### 2.9 Prepare-step decisions (tenant tx P, in this order)

| Condition | Action |
|---|---|
| `tenants.status <> 'active'` | **defer** (HQ-E1-1 default): `claimed → pending`, `deferred_tenant_inactive` (trigger re-validates the status, F6), no attempt consumed, backoff by `claims`; no audit row (scheduling), counter + gauge |
| **(F4)** `row.provider_id` is not in the tenant's configured set (same query as `configuredKYCProvidersSQL`: active, in-window `provider_credential_handles` of domain `kyc`, purpose `outbound_api`, AND registered in the orchestrator) | `cancelled` / `provider_deconfigured` + audit; **no call; never redirected** to another provider. Credential rotation must overlap (ADR 0093 `not_before`/`not_after`), else a rotation gap cancels rows; runbook note |
| re-claim with `failed_attempts >= MaxFailedAttempts` | `failed_terminal` (`lease_expired`) + audit + alert, no call |
| `create`, verification no longer orphan-shaped (staff decided it, IC C3) | `cancelled` / `decided_concurrently` + audit, no call |
| verification terminal | `cancelled` / `verification_terminal` + audit |
| `submit`, a newer (`(created_at, id)`) `submit` row for the same verification in `pending`/`claimed`/`sent` | `cancelled` / `superseded` + audit |
| `submit`, verification still an orphan (its create ended terminal) | `cancelled` / `verification_not_submitted` + audit |
| `submit`, current non-rejected set empty | `cancelled` / `no_documents` + audit |
| `submit`, current set ≠ `document_ids` — a staff rejection **or a document deleted** since enqueue (IC C5) | `cancelled` / `document_set_changed` and re-enqueue the current set in the same tx with **`INSERT ... ON CONFLICT (tenant_id, idempotency_key) WHERE state IN ('pending','claimed','sent') DO NOTHING`** (F12: a concurrent upload of the same set never aborts P with 23505) + audit. Whether erasure obligations require cancelling already-sent submissions is HUMAN/LEGAL (not decided) |
| otherwise | proceed to B |

When a `create` row reaches `failed_terminal` or `cancelled`, the same phase-C/P transaction also moves
every `pending` `submit` row of that verification to `cancelled` / `verification_not_submitted` with one
audit row each (IC Q-R1(a); DB-validated, §2.3). Documents uploaded onto that orphan stay stored and
unsent (runbook; retention under HQ-E1-4).

### 2.10 Idempotent create (F3 / IC C2)

`RequestVerification(ctx, tx, params, providerID)` in the tenant tx:
1. `SAVEPOINT`; insert the orphan verification + audit + the outbox `create` row.
2. On `23505` from `kyc_submission_outbox_one_live_create_per_player`: `ROLLBACK TO SAVEPOINT` and return
   the verification referenced by the existing live create row (read in the same tx), with `existing=true`
   → HTTP **200** and the same body shape; audit `kyc.submission_enqueued` with `duplicate=true`.
3. Concurrency: the second inserter blocks on the unique index until the first commits, then takes step 2.
   No check-then-insert. Repeat creates after the live row is `sent`/terminal create a new verification as
   today (that is the existing re-verification path, bounded per live row).

---

## 3. The KYC worker identity (HD-PRH2-11)

### 3.1 Name and registration

- `internal/db/platform_service.go`: `ServiceKYCSubmissionWorker PlatformService = "kyc_submission_worker"`
  with a doc comment naming ADR 0106, the two outbox policies, the 0114 fence family, and the confinement
  to `internal/kyc/outbox_worker.go:claimNext`; added to `platformServiceAllowlist`. The `PlatformService`
  and `WithPlatformService` comments are updated (the vocabulary is no longer catalogue-only).
- GUC mechanics unchanged: allowlist check before the tx; `set_config('app.platform_service_id', ..., true)`;
  tenant and player GUCs never set. First statement inside the closure:
  `db.AssertPlatformServiceScope(ctx, tx, db.ServiceKYCSubmissionWorker)`.
- No role, password, attribute or credential is created or changed.

### 3.2 Policies on `kyc_submission_outbox` (exact set)

Every predicate is written out literally (macros below are for reading only).

```sql
-- X(tenant) := player, platform-admin, platform-service, acting_tenant, acting_platform GUCs all unset
-- X(worker) := app.platform_service_id = 'kyc_submission_worker' AND tenant, player, platform-admin,
--              principal, acting_tenant, acting_platform GUCs all unset
ALTER TABLE kyc_submission_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_submission_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY kso_tenant_select ON kyc_submission_outbox FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND <X(tenant)>);
CREATE POLICY kso_tenant_insert ON kyc_submission_outbox FOR INSERT
    WITH CHECK (tenant_id = <app.tenant_id> AND <X(tenant)> AND <app.principal_id unset>);
CREATE POLICY kso_tenant_update ON kyc_submission_outbox FOR UPDATE
    USING      (tenant_id = <app.tenant_id> AND <X(tenant)> AND <app.principal_id unset>
                AND state IN ('claimed', 'pending'))
    WITH CHECK (tenant_id = <app.tenant_id> AND <X(tenant)> AND <app.principal_id unset>
                AND state IN ('pending', 'sent', 'failed_terminal', 'cancelled'));
CREATE POLICY kso_kyc_worker_select ON kyc_submission_outbox FOR SELECT USING (<X(worker)>);
CREATE POLICY kso_kyc_worker_claim  ON kyc_submission_outbox FOR UPDATE
    USING (<X(worker)> AND state IN ('pending', 'claimed'))
    WITH CHECK (<X(worker)> AND state = 'claimed');
```

(`kso_tenant_update` USING admits `pending` only for the trigger-validated `pending → cancelled /
verification_not_submitted` row of §2.3; every other tenant transition starts from `claimed`.) No `FOR ALL`,
no DELETE policy, no NULL-tenant arm, no `USING (true)`: A-18 sees positive guards only. A staff-principal
session may read (for §7.2's derived state) but not write.

### 3.3 Least privilege: the worker fence family (security F1, BLOCKING; Q-S1 not accepted)

**Derivation.** I replayed every `CREATE POLICY` / `DROP POLICY` in `migrations/*.up.sql` at `3517980`
(the A-18 method) and classified each effective **permissive** policy for a session that has only
`app.platform_service_id = 'kyc_submission_worker'` set: exposed if it has a `USING/WITH CHECK (true)` arm
or a NULL-tenant arm (`tenant_id IS NULL` or `app.tenant_id IS NULL`) with no positive guard the worker
never satisfies and no `app.platform_service_id IS NULL` exclusion; a policy whose only condition is
`app.player_account_id IS NULL` is exposed too. Every public table has RLS enabled (`tenants`,
`jurisdictions` and `licences` without FORCE; `schema_migrations` has no RLS and holds only versions).
The static replay is lexical, so the implementer **must re-derive the list from the live catalogue
(`pg_policies`) on a throwaway private scratch DB** before writing 0114, and the §10.4 catalogue gate test
makes any omission fail.

| Table | Exposed permissive policies (worker session) | Exposure | In fence |
|---|---|---|---|
| `staff_users` | `dual_scope_isolation` (ALL) | read incl. `password_hash` of platform staff; **insert a `platform_admin`** (proven) | **yes** |
| `audit_log` | `dual_scope_isolation` (ALL) | read platform audit; **forge platform audit rows** (proven) | **yes** |
| `sessions` | `session_scoped_insert`, `session_scoped_update` | insert/update NULL-tenant sessions | **yes** |
| `login_attempts` | `dual_scope_isolation` (ALL) | read/write platform login attempts | **yes** |
| `risk_rules` | `tenant_and_platform_read/_write/_update/_delete_visibility` | read and I/U/D platform risk rules | **yes** |
| `player_restrictions` | `staff_and_system_read`, `staff_insert` | read; insert NULL-tenant restriction rows | **yes** |
| **`persons`** (found in r2 derivation, not in the security list) | `persons_insert_any_scope` (`WITH CHECK (true)`), `persons_platform_scope_read_write` (SELECT), `_update`, `_delete` (all keyed only on `app.tenant_id IS NULL`) | **read every Person (PII) across tenants; insert/update/delete Persons** | **yes** |
| `asset_operation_eligibility` | `tenant_and_platform_read` | read platform rows (0113 fenced it for acting sessions only) | **yes** (SELECT exposure; fence family applied whole) |
| `open_bet_self_exclusion_policies` | `tenant_and_platform_read` | read platform floor rows (same) | **yes** |

**Public reference allowlist (worker may read, never write; all `FOR SELECT` only):** `tenants`,
`alert_kinds`, `assets`, `brands`, `casino_games`, `financial_capability_catalogue`,
`financial_capability_settings`, `financial_control_classifications`, `financial_governance_permissions`,
`jurisdiction_precedence_configs`, `jurisdictions`, `kyc_enforcement_policies`,
`ledger_adjustment_reason_codes`, `platform_operations`, `platform_products`, `sb_competitions`,
`sb_events`, `sb_jurisdiction_restrictions`, `sb_markets`, `sb_selections`, `sb_sports`,
`schema_migrations`. This is exactly ADR 0099 §6.2's reviewed `a18SelectAllowlist` (public platform
reference data, no PII, no credentials) restricted to what the worker can actually see, plus
`schema_migrations`. (`licences` and `licence_country_ceilings` require a platform-admin or tenant
predicate and are not visible to the worker.) If security prefers a smaller allowlist, a table can be
moved into the fence family without any other design change (QUESTION Q-S5).

**The fence (in 0114, literal SQL, one family per fenced table, the 0112 `acting_fence_*` shape):**

```sql
CREATE POLICY kyc_worker_fence_select ON staff_users AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON staff_users AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON staff_users AS RESTRICTIVE FOR UPDATE
    USING      (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON staff_users AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
-- ... repeated literally for audit_log, sessions, login_attempts, risk_rules, player_restrictions,
--     persons, asset_operation_eligibility, open_bet_self_exclusion_policies (36 policies).
```

- The predicate is true for every other session (including `NULL`), so **no other session's access
  changes**; restrictive policies only narrow. None of the fenced tables is in the 0077 exact whitelist
  (`tenants`, `licences`, `jurisdictions`), so it stays at 11 tuples. A-18 skips restrictive policies;
  they are literal, so `TestA18_DynamicPolicyGenerationIsRestrictiveOnly` is unaffected. The K2-G1 acting
  probe is unaffected (acting sessions satisfy the predicate).
- Down drops the 36 policies.

**What the worker can and cannot do after r2:**

| Capability | Worker session |
|---|---|
| Read `kyc_submission_outbox`, all tenants (ids, states, counters) | yes |
| Claim / re-claim an outbox row | yes (the only write) |
| Any other outbox transition, INSERT, DELETE | no (RLS + trigger) |
| Read any table outside the outbox and the public reference allowlist | **no** (tenant policies need `app.tenant_id`; the nine exposed tables are fenced) |
| Write any table other than the outbox claim | **no** (fenced or positive-guarded; catalogue gate) |
| Raise an alert | no (`alerting_session_scope()` refuses; unchanged) |
| Write any audit row | **no** (`audit_log` fenced) |

The pre-existing NULL-arm write exposure of `WithoutTenant`, `sportsbook_catalogue_sync` and
`alert_dispatcher` (security F2) is **NULL-ARM-WRITE-1**, registered by the orchestrator; whether it
blocks launch is a human decision. E1 neither fixes nor widens it. The fence pattern here is reusable
for that item.

### 3.4 Per-tenant processing

- **Rejected:** per-tenant claim loop without identity (HD-PRH2-11 decided an identity; a tenant session
  could claim); everything under the worker identity (cross-tenant KYC PII read and status write); a mixed
  service+tenant session (`kyc_verifications.tenant_isolation` checks only `app.tenant_id`, so it would
  satisfy every plain tenant policy).
- **Chosen (hybrid):** identity = discovery + claim only. Each item's work runs in plain
  `WithTenant(row.tenant_id)` transactions; `row.tenant_id` is the claim's `RETURNING` value; P and C
  re-select by `(id, claim_token)` under tenant RLS, so a different tenant id yields 0 rows, no call and no
  write (INV-KYC-OB-4, tamper test §10.4). Composite FKs make outbox/verification/player tenant equality
  structural. Credentials are resolved per item for that tenant with the existing binding check.
- Per-item panic recovery and error isolation; head-of-line delay across tenants remains (§9 R4).

### 3.5 Audit actor (Q-S3 ruling: ACCEPTED as proposed)

Every worker-written audit row is a tenant row written in P or C: `ActorType = system`, `actor_id NULL`
(matches the H sweeper rows and `audit_log_check`), `metadata.platform_service = "kyc_submission_worker"`
(compiled constant), plus `outbox_id`, `operation`, `claims`, `failed_attempts` and the closed
`error_class` or `cancel_reason`. Never a vendor reference, credential, document content, storage
reference or raw error text. New actions: `kyc.submission_enqueued`, `kyc.submission_retry_scheduled`,
`kyc.submission_failed_terminal`, `kyc.submission_cancelled`. The claim is not audited; its durable record
is the row's trigger-forced claim columns (which is why down refuses on any row, §5.3). The UUIDv5 `service`
actor alternative is rejected.

### 3.6 Comparison with the other identities

| | `sportsbook_catalogue_sync` (0084) | `alert_dispatcher` (0110) | **`kyc_submission_worker` (0114)** |
|---|---|---|---|
| Tables with a permissive policy for it | 5 catalogue tables | 5 alerting tables | **1** |
| Writes | catalogue I/U/D | deliveries; 3 meta-Kinds | **outbox claim only** |
| Restrictive fence against NULL-arm tables | no (NULL-ARM-WRITE-1) | no (NULL-ARM-WRITE-1) | **yes, 9 tables** |
| Raises alerts | no | meta-Kinds only | no (its tenant phase C does) |
| Confined to (static test) | sportsbook sync | alerting dispatcher/fallback | `internal/kyc/outbox_worker.go:claimNext` |

---

## 4. The KYC alert Kind (HD-PRH2-10)

### 4.1 Definition

| Field | Value |
|---|---|
| `kind` | `kyc.submission_failed_terminal` |
| `severity` | `p2` — engineering default; **HQ-E1-2 OPEN**: human/compliance confirms before 0114 ships (changing it later needs a migration) |
| `scope` / `simulation` | `platform` / `false` |
| `requires_subject` / `in_tx_raisable_by_tenant` | `true` / `true` (security confirmed bounded: subject forced to `app.tenant_id`, `raised_by_scope` forced `tenant`, severity forced; a tenant can only create noise about itself, as with the 14 existing tenant-raisable Kinds) |
| `allowed_keys` | `{operation, provider_id, last_error_class, outbox_id}` (security: acceptable given the F5 charset CHECK) |
| `raise_mode` | `in_tx` |

**Discriminator (closed set):** `<operation>:<provider_id>` for outage classes (`ambiguous`, `not_sent`,
`lease_expired`); `<operation>:<provider_id>:binding_mismatch` for `credential_binding_mismatch` (F5);
`<operation>:<provider_id>:apply_conflict` for `apply_conflict` (F11). `provider_id` is CHECK-constrained
to `^[A-Za-z0-9_.-]{1,64}$` at the table, so it always fits the discriminator charset; the Go builder still
falls back to `provider_unclassified` defensively (mutant M19). One open alert per (subject tenant,
discriminator); later failures add occurrences.

### 4.2 Sanctioned raise path

Phase C opens through `alerting.InTx(ctx, alerting.NewTenantRunner(pool, row.tenant_id), fn)`; after the
CAS to `failed_terminal` affected one row and the audit row is written,
`raiseSubmissionFailedTerminal` calls `alerting.RaiseGuarded` with `SubjectTenantID: row.tenant_id`; after
`InTx` returns, `Flush` runs the mandatory detached retry in the same tenant scope. `raised_by_scope` is
forced to `tenant`. The worker identity never raises; `alerting_session_scope()` is not edited;
`internal/kyc` never references `db.ServiceAlertDispatcher`. The `apply_conflict` terminal (§2.8) uses a
second `InTx` of the same shape. Rejected: post-commit-only raise (loses the alert on a crash) and widening
`alerting_session_scope()`.

**Delivery:** log sink only; no route, channel or recipient. The alert is durable and `unrouted`.
**ALERT-DELIVERY-1 stays OPEN. There is no notification path to any human, the tenant or the player**;
the DoD labels alerting for this Kind `NOT IMPLEMENTED` as a notification mechanism (IC C1). The
staff-visible derived state (§7.2) is the only place a stranded submission becomes visible.

### 4.3 Go mirror and parity

`internal/alerting/kind.go`: `KindKYCSubmissionFailedTerminal` + its `kindDefs` entry. New
`internal/alerting/migration_0114_kyc_kind_integration_test.go` checks every column of the row
(`allowed_keys` as a set, `raise_mode`) and the reverse direction (every DB Kind has a Go def).
`internal/alerting/static_wiring_test.go` gains `internal/kyc/outbox_worker.go:raiseSubmissionFailedTerminal`
in `staticRaiseGuardedHelpers` and `internal/kyc/outbox_worker.go:runPhaseC` (and the `apply_conflict`
terminal owner `recordApplyConflict`) in `staticInTxOwners`.

### 4.4 Seeding under FORCE RLS (Q-S2: APPROVED with conditions)

```sql
DO $$
BEGIN
    CREATE POLICY alert_kinds_seed_0114 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'kyc.submission_failed_terminal');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject,
                             in_tx_raisable_by_tenant, allowed_keys, raise_mode)
    VALUES ('kyc.submission_failed_terminal', 'p2', 'platform', false, true, true,
            ARRAY['operation', 'provider_id', 'last_error_class', 'outbox_id'], 'in_tx');
    DROP POLICY alert_kinds_seed_0114 ON alert_kinds;
END
$$;
```

Binding conditions (security): literal SQL, never `EXECUTE`; `FOR INSERT` only, `TO CURRENT_USER`; the
predicate is equality on exactly one Kind; CREATE, INSERT and DROP in one `DO` block; FORCE RLS never
toggled; tests 32, 37 and a runtime-role `INSERT INTO alert_kinds` refused after up. Security verified on
PG16 that the block is atomic under an injected failure and that the policy rejects any other Kind. The
down unseed (§5.3) is accepted on the same atomicity basis. This becomes the sanctioned route for later
Kinds only under these conditions, with security review per Kind.

---

## 5. Migration 0114

### 5.1 Files

`migrations/0114_kyc_submission_outbox.up.sql`, `migrations/0114_kyc_submission_outbox.down.sql`. Number
0114 (ADR 0105; K3 is 0115). Rule 3 renumbering applies if another migration merges first.

### 5.2 Up (in order)

1. `kyc_submission_outbox_session()`.
2. `CREATE TABLE kyc_submission_outbox`, indexes, `COMMENT ON TABLE`.
3. `kyc_submission_outbox_guard()` + row and TRUNCATE triggers.
4. `ENABLE` + `FORCE ROW LEVEL SECURITY`; the five outbox policies (§3.2).
5. **The worker fence family: 36 restrictive policies on the nine tables (§3.3)**, as re-derived from the
   live catalogue at implementation time.
6. Grants (0110 §7 pattern): `REVOKE ALL ON kyc_submission_outbox FROM igaming_runtime; GRANT SELECT,
   INSERT, UPDATE ...` guarded by `pg_roles`. No DELETE, no TRUNCATE. No identity column remains (F10),
   so no sequence grant question arises.
7. The `alert_kinds` seed `DO` block (§4.4).

No existing table, function or permissive policy is altered. `enforcement.go`, 0040's policies and every
alerting object stay byte-identical.

### 5.3 Down (F7 + IC C4 + Q-R5: one atomic block; outbox refusal first; refuse on ANY row)

```sql
DO $$
DECLARE v_count BIGINT;
BEGIN
    -- 1. Refusal FIRST, on ANY row (outbox rows are the only durable claim record, §3.5; retention is
    --    undecided, HQ-E1-4). Owner bypasses RLS only for this count, inside this one statement.
    ALTER TABLE kyc_submission_outbox NO FORCE ROW LEVEL SECURITY;
    SELECT count(*) INTO v_count FROM kyc_submission_outbox;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0114 down: refusing - % kyc_submission_outbox row(s) exist', v_count;
    END IF;
    DROP TABLE kyc_submission_outbox;
    DROP FUNCTION kyc_submission_outbox_guard();
    DROP FUNCTION kyc_submission_outbox_session();

    -- 2. The fence family (36 DROP POLICY statements, literal).
    DROP POLICY kyc_worker_fence_select ON staff_users;
    -- ... (all 36)

    -- 3. Unseed the Kind. FK checks bypass RLS, so a referencing alert/occurrence makes this fail
    --    un-blindably; the handler names the remedy.
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0114 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'kyc.submission_failed_terminal');
    DELETE FROM alert_kinds WHERE kind = 'kyc.submission_failed_terminal';
    DROP POLICY alert_kinds_unseed_0114 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0114 down: refusing - alerts of kind kyc.submission_failed_terminal exist; resolve and archive them under an authorized procedure first';
END
$$;
```

- One statement: under standalone `psql` autocommit either everything is undone or nothing; a refusal
  leaves the table, its FORCE RLS, the fences, the Kind, `alert_kinds` FORCE RLS and its deny trigger
  intact (tested). The r1 two-block ordering hazard (Kind removed, table kept) cannot occur.
- On an empty scratch DB it succeeds, so the jurisdiction 0075/0077 full-chain rollback tests pass.

### 5.4 Pinned tests a migration can trip

| Test | Effect | Action |
|---|---|---|
| `internal/jurisdiction/migration_0077_integration_test.go` exact whitelist (11 tuples on `tenants`/`licences`/`jurisdictions`) | none (no policy on those tables) | **no entries to add**; stays 11 |
| jurisdiction 0075/0077 full-chain rollback | down must succeed empty | §5.3 |
| `TestMigration0110_KindsSeeded` | Go Kind needs the seed | §4.4 |
| `internal/db/null_arm_replay_static_test.go` (A-18, incl. `TestA18_DynamicPolicyGenerationIsRestrictiveOnly`) | new permissive policies positive-guarded; fences restrictive and literal | no allowlist change |
| `internal/db/k2_gates_integration_test.go` K2-G1 acting probe | acting sees 0 outbox rows; fences do not affect acting | none |
| `internal/alerting/static_dispatcher_identity_test.go` | none | keep |
| `internal/alerting/static_wiring_test.go` | new helper/owners | §4.3 |
| `internal/txscope/no_provider_call_in_tx_closure_static_test.go` | worker phase B | outside closures |
| `kyc_two_phase_integration_test.go`, `recording_tx_integration_test.go`, `httpserver/kyc_round3_http_test.go` | assume synchronous vendor calls | rewritten to drive the worker; IC condition 2 still asserted |
| `cmd/platform-api/alert_dispatcher_wiring_test.go`, `sweeper_wiring_test.go` | count dispatcher/sweeper constructions | E1 adds none of theirs; unaffected |
| `migrate verify` | 0114 checksum, gap-free | up, verify, down→up round trip |

---

## 6. `deploy/init-app-role.sql` (append-only, Rule 4)

Append after K2's block, K2 loop pattern, one entry `('kyc_submission_outbox', 'SELECT, INSERT, UPDATE')`.
No role, password or attribute change. Grants on the nine fenced tables are **not** changed (the fence is
RLS, not grants). K3 appends after this block.

---

## 7. Code shape, configuration and wiring

### 7.1 Go (internal/kyc)

- `outbox.go` (new): row model, `enqueueCreate` / `enqueueSubmit` (phase A helpers on the caller's tenant
  tx; ON CONFLICT DO NOTHING), error classes, `backoff(n)`, the derived-state read (§7.2).
- `outbox_worker.go` (new): `OutboxWorker`, `ValidateForLoop` (F9 inequality), `RunOnce`,
  `RunOutboxWorkerLoop(ctx, w, logger, interval)` (immediate first pass, per-pass/per-item panic recovery,
  shutdown drain like ADR 0095 §37.4), `claimNext` (the only `db.ServiceKYCSubmissionWorker` reference),
  `prepare`, `executeCreate` / `executeSubmit` (phase B), `runPhaseC`, `recordApplyConflict`,
  `raiseSubmissionFailedTerminal`.
- `verification_service.go`: `CreateVerification` → `RequestVerification(ctx, tx, params, providerID)`
  (phase A, §2.10). `applyCreateVerificationResult` reused; its CAS-miss now returns a typed
  `errCreateDecidedConcurrently` the worker maps to `decided_concurrently` (IC C3).
- `document_service.go`: `UploadDocument` enqueues; `SubmitVerification` removed as an export; the N5
  orphan guard relaxed only for an orphan with a live create (read in the upload tx).
- Static tests (Q-R3, IC): no non-test code outside `internal/kyc/outbox_worker.go` calls a `KYCProvider`'s
  `CreateVerification` / `SubmitVerification`, and no **exported** function in `internal/kyc` reaches them.

### 7.2 HTTP and the derived submission state (IC C1)

- Create handler: `SelectProvider` + `RequestVerification` in one `WithTenant` tx → 201 (new) or 200
  (existing live create) with the `unverified` verification; missing/ambiguous provider configuration →
  503 before any row. Upload handler: no vendor call; responses unchanged. No outbox internals in any
  player response (security Q-R2 condition).
- **Staff read (required):** the compliance-console verification read (`kyc_admin_handlers.go` list/get,
  backed by `ListVerificationsForTenant` / `GetVerificationByID`) gains a read-only derived
  `submission_state` ∈ {`none`, `queued`, `sent`, `failed`, `cancelled`} and, when `failed`/`cancelled`,
  the closed `last_error_class` / `cancel_reason`. Derivation, per verification, from its latest `create`
  row and latest `submit` row by `(created_at, id)`: `failed` if either latest row is `failed_terminal`;
  else `queued` if either is `pending`/`claimed`; else `sent` if either is `sent`; else `cancelled` if a row
  exists; else `none`. Tenant-scoped read under the staff principal's RLS (`kso_tenant_select`). No vendor
  reference, document data or counters are exposed.
- **Player read (optional, product-owner-proxy decision, NOT built by default):** a coarse enum
  {`queued`, `sent`, `failed`, `none`} with no vendor data, same derivation.
- The OpenAPI descriptions of create (201/200, asynchronous vendor submission) and of the staff read are
  updated in E1.

### 7.3 Configuration

`internal/config/config.go`: `KYCOutboxInterval`, env **`KYC_OUTBOX_INTERVAL_SECONDS`** (positive;
RECOMMENDATION default 5 s). Nothing else is env-configurable; §2.8 values are code constants.
`docs/runbooks/production-configuration-checklist.md`: one row.

### 7.4 `cmd/platform-api` wiring (verified against `3517980`)

`main.go` at `3517980` has the H block (`---- PRH-2 H (CP-W1): payments sweeper process (begin)` …
`(end)`, ~lines 459-475), the I-wire start block (`---- PRH-2 I-wire (ALERT-DELIVERY-1): durable alert
dispatcher (begin)` … `(end)`, ~477-490), the H shutdown drain (~595-608) and the I-wire shutdown drain
(~610-623), followed by `logger.Info("shutdown complete")`. E1 adds:
- in `registrations.go`: `buildKYCOutboxWorker(pool, kycOrchestrator, kycOutboundCredentials)`;
- in `main.go`, **immediately after the I-wire start block's `(end)` line**:
  ```go
  // ---- PRH-2 E1 (KYC-SUBMIT-OUTBOX-1): KYC submission outbox worker (begin) ----
  kycOutboxWorker := buildKYCOutboxWorker(pool, kycOrchestrator, kycOutboundCredentials)
  if err := kycOutboxWorker.ValidateForLoop(); err != nil {
      return fmt.Errorf("kyc outbox worker wiring: %w", err)
  }
  var kycOutboxWG sync.WaitGroup
  kycOutboxWG.Add(1)
  go func() {
      defer kycOutboxWG.Done()
      kyc.RunOutboxWorkerLoop(ctx, kycOutboxWorker, logger, cfg.KYCOutboxInterval)
  }()
  // ---- PRH-2 E1: KYC submission outbox worker (end) ----
  ```
- **immediately after the I-wire shutdown-drain block's `(end)` line and before
  `logger.Info("shutdown complete")`:** a delimited `---- PRH-2 E1: KYC outbox worker shutdown drain
  (begin/end) ----` block (WaitGroup wait with a bounded `time.After`, logging if not stopped).
- The H and I-wire blocks are not edited. `run_order_ast_test.go` pins only the pre-DB guard order and
  needs no change; a new `cmd/platform-api/kyc_outbox_wiring_test.go` mirrors
  `alert_dispatcher_wiring_test.go` / `sweeper_wiring_test.go` (exactly one builder and one loop, inside a
  goroutine, run context `ctx`, configured interval, drain present).

### 7.5 Metrics (no tenant/provider/row labels)

`kyc_outbox_passes_total`, `kyc_outbox_items_total{result}` (`sent|retry|failed_terminal|cancelled|
deferred|claim_lost|phase_c_failed|apply_conflict`), `kyc_outbox_last_pass_unix_seconds`,
`kyc_outbox_oldest_due_age_seconds`, `kyc_outbox_oldest_deferred_age_seconds` (IC finding 9: deferral is
observable).

---

## 8. File ownership (K3 can run in parallel)

### 8.1 E1-owned

- New: `migrations/0114_kyc_submission_outbox.{up,down}.sql`; `internal/kyc/outbox.go`;
  `internal/kyc/outbox_worker.go`; `internal/kyc/outbox_*_test.go`; `internal/kyc/migration_0114_integration_test.go`;
  `internal/alerting/migration_0114_kyc_kind_integration_test.go`;
  `internal/db/kyc_worker_identity_integration_test.go` (T-ID-1..4, catalogue gate);
  `internal/db/kyc_worker_static_test.go` (GUC and confinement statics);
  `cmd/platform-api/kyc_outbox_wiring_test.go`.
- Edited: `internal/kyc/{verification_service,document_service,provider}.go`;
  `internal/httpserver/kyc_handlers.go`; **`internal/httpserver/kyc_admin_handlers.go`** (derived state);
  the KYC tests that called the removed entry points; `internal/db/platform_service.go`; the OpenAPI
  description file(s) for the KYC player and staff routes.

### 8.2 Shared files (current owners after the H/I-wire merges at `3517980`)

| File | E1 change | Other known writer | Conflict |
|---|---|---|---|
| `deploy/init-app-role.sql` | append one block after K2's | K3 appends after E1 | textual; E1 first |
| `cmd/platform-api/main.go` | two delimited blocks after the I-wire blocks (§7.4) | none active (H, I-wire merged; their blocks untouched) | none |
| `cmd/platform-api/registrations.go` | `buildKYCOutboxWorker` | none active | none |
| `internal/config/config.go` | `KYCOutboxInterval` | none active | none |
| `internal/alerting/kind.go` | one constant + one def | K3 only if it adds Kinds | append |
| `internal/alerting/static_wiring_test.go` | one helper + two owners | K3 if it adds `RaiseGuarded` sites | append |
| `docs/decisions/0095-…md` | §38 + pointers | K3 (§4.3, §4.8, §28.9, INV-IO-7) | K3 new sections §39+ |
| `docs/runbooks/operational-runbooks.md` | §13 | K3 runbooks | K3 §14+ |
| `docs/runbooks/production-configuration-checklist.md` | one row | — | none |

The 0114 fence touches policies on `staff_users`, `audit_log`, `sessions`, `login_attempts`,
`risk_rules`, `player_restrictions`, `persons`, `asset_operation_eligibility`,
`open_bet_self_exclusion_policies` (restrictive additions only). K3/0115 touches none of them (its objects
are payment tables, the ledger fence and reconciliation). E1 touches no `internal/payments`, `ledger`,
`withdrawal`, `reconciliation`, `providerref`, `capability`, `adjustment`, `backoffice` file and no
`enforcement.go`.

### 8.3 Runbook "KYC outbox stuck" (`docs/runbooks/operational-runbooks.md` §13)

- **Signals:** `kyc_outbox_last_pass_unix_seconds` stalled; oldest-due / oldest-deferred age growing;
  open `kyc.submission_failed_terminal` alerts (durable, **unrouted**: nobody is notified,
  ALERT-DELIVERY-1 OPEN); `submission_state = failed` in the compliance console; players reporting
  "stuck in unverified".
- **Diagnose:** worker running; vendor/credential failure (class counts); `provider_deconfigured`
  cancels (credential rotation without overlap?); tenant inactive (deferred; `claims` grows per deferral
  cycle, `failed_attempts` does not); repeated `lease_expired` (phase C failing; DB health);
  `binding_mismatch` (an integrity signal: credential store misbinding — escalate to security);
  `apply_conflict` (deterministic DB conflict; escalate to identity-compliance).
- **Ambiguous `failed_terminal` reconciliation (IC C5):** a `create` that ended `failed_terminal` after an
  ambiguous result may have created a vendor-side verification that is **unbound** to any platform row;
  likewise `decided_concurrently` (audit `vendor_reference_unbound=true`). Whether it can be found and
  closed at the vendor is PROVIDER DEPENDENT (ADR 0095 §38.3); record the case, never bind a reference by
  hand.
- **Remedy:** fix the cause; `pending` rows resume. `failed_terminal` is never requeued by hand: the
  player re-uploads (the same content may be enqueued again) or starts a new verification; documents
  stranded on a failed orphan stay stored (retention HQ-E1-4). A staff requeue does not exist
  (KYC-OUTBOX-REQUEUE-1: when built, an audited staff action with a reason code in the tenant session that
  never writes a verification status). Resolve alerts with `alert:manage` and a reason code.
- **Never:** lift RLS or a fence, disable the trigger, DELETE rows, or set a verification status to clear
  a row.

---

## 9. Threats and residuals

| # | Item | E1 mitigation | Remaining |
|---|---|---|---|
| R1 | **Rewritten (F1).** A platform-service session satisfies legacy NULL-tenant arms | 0114 fence family on the nine exposed tables; catalogue gate; T-ID-2 NULL-tenant write/read denials; T-ID-3 visibility-0 allowlist probe | **closed for `kyc_submission_worker`.** The same exposure for `WithoutTenant`, `sportsbook_catalogue_sync`, `alert_dispatcher` is **NULL-ARM-WRITE-1** (security F2; human decides launch impact) |
| R2 | GUC spoofing by raw `set_config` | allowlist; widened static test (F8): no non-test occurrence of the literal `app.platform_service_id` outside `internal/db` and SQL migrations | — |
| R3 | Vendor idempotency PROVIDER DEPENDENT (re-send after lost C / expired lease) | same key on every re-send; bounded budget; F11 stops deterministic loops | vendor-intake requirement (ADR 0095 §38.3) |
| R4 | Head-of-line across tenants | one row per claim; per-tenant pass cap | concurrency caps (follow-up) |
| R5 | No delivery / no notification path | staff-visible derived state (§7.2) | ALERT-DELIVERY-1 OPEN; HD-PRH2-4-OPS |
| R6 | Stalled worker cannot alert itself | gauges + runbook | future watchdog |
| R7 | Unbounded growth (DELETE refused) | — | HQ-E1-4 |
| R8 | Player-visible asynchrony | 201/200 + `unverified`; staff state; optional player enum (POP) | — |
| R9 | Non-active tenant rows wait indefinitely | DB-validated deferral, no attempts | HQ-E1-1 |
| R10 | No staff requeue | re-upload / new verification | KYC-OUTBOX-REQUEUE-1 |
| R11 | Lost-claim results discarded | winner applies its own | duplicate vendor call = R3 |
| R12 | Callback for a reference whose create C has not landed | existing retryable 503 | bounded by R3 |
| R13 | **(IC C5, pre-existing ADR 0096 semantics)** A late `sent` with a definitive `pending` on a **new** verification becomes the latest decided row and moves an approved player to `OutcomePending` at an arbitrary later time (previously synchronously inside the create request) | documented in ADR 0095 §38.2; pinned by T-J | accepted (deliberate) |
| R14 | **(IC T-B)** A gate denial during a platform-side stall writes a DECISION-ROWS-1 row indistinguishable from "never submitted" | no schema change; the staff derived state disambiguates | recorded audit gap |
| R15 | **(F13)** A suspension or a staff document rejection landing between P's commit and B's call | at most one send (same as the H sweeper) | accepted |
| R16 | **(F13)** Any in-tx-raisable Kind can be raised by a plain tenant session about its own tenant | bounded by ADR 0102 triggers | accepted (same as 14 existing Kinds) |
| R17 | Credential rotation without overlap cancels queued rows (`provider_deconfigured`) | runbook | operational |

---

## 10. Test plan

Legend as plan §5. DB tests on a HEAD-migrated scratch DB under the runtime role (never superuser,
never BYPASSRLS). T-1..T-4 apply. Local runs are never labelled CI.

### 10.1 State machine, outcomes, IC F3 (R, FL, IDM)

1. Create happy path (201; worker → reference bound, `sent`, audits).
2. Submit happy path (pinned ids and key reach the mock).
3. Ambiguous (each kind): status unchanged (**IC condition 2**), `pending`, `failed_attempts+1`, audit.
4. Not-sent (credential missing, adapter unregistered): no mock call; `not_sent`.
5. Exhaustion → `failed_terminal` + alert (subject tenant, discriminator, `raised_by_scope='tenant'`) +
   audit; verification byte-identical.
6. **IC F3:** enforcement with a `create` row `pending` / `claimed` / `failed_terminal` / `cancelled`
   returns the pre-existing outcome; 100 % ambiguous until `failed_terminal` leaves the verification row
   image unchanged.
7. Every §2.9 prepare row (including deferral without attempt consumption and resumption on reactivation,
   `provider_deconfigured` with zero mock calls, exhausted re-claim, `decided_concurrently`, document
   deletion → `document_set_changed`).
8. Backoff table.
9. Binding mismatch → immediate `failed_terminal`, discriminator `:binding_mismatch`, no retry.
10. `apply_conflict`: an injected 23505 in phase C (vendor reference already bound elsewhere) → after
    in-item retries `failed_terminal`, no re-send.

### 10.2 Crash and concurrency (CON, RB)

11. Crash A→B (row `pending`, verification orphan; next pass sends).
12. Crash B→C (re-claim after fixture lease expiry; same key twice; one success audit).
13. Phase C DB failure → bounded retry → lease path.
14. Two workers, `-race -count=50`: each row claimed once per lease, each call once.
15. Lost claim: A's phase C affects 0 rows, rolls back, A's result not applied.
16. Concurrent staff review / callback during B: never overwritten or demoted, `-count >= 20` (Q-R4).
17. Duplicate submit (same set twice; replayed upload): one live row, `duplicate=true`; re-enqueue after
    `failed_terminal` allowed.
18. Create/submit ordering: submit not claimable while create live.
19. P re-enqueue racing a concurrent upload of the same set: no 23505 abort (F12).
20. **Per-player create bound under concurrency (F3):** N concurrent creates for one player → one live
    create, the others return it (200); a create after the live row is `sent` makes a new verification.

### 10.3 Tenant isolation and RLS (TI, RLS)

21. Tenant B cannot read/update/insert A's rows.
22. Positive-then-negative exclusion matrix for every outbox policy (player, platform-admin, staff
    principal read-yes/write-no, acting, `alert_dispatcher`, `sportsbook_catalogue_sync`, mixed
    service+tenant).
23. A tenant cannot claim, cannot touch a `pending` row except the validated
    `verification_not_submitted` cancel, cannot change immutable columns or terminal rows; nobody can
    DELETE/TRUNCATE.
24. Insert validation refusals (wrong provider, wrong key, unsorted/duplicate ids, rejected document,
    foreign-verification document, create on non-orphan, provider id outside the charset).
25. Go/SQL key parity.
26. **Worker trigger pinning (F6):** a worker claim attempting to change `failed_attempts`,
    `next_attempt_at`, `last_error_class`, `cancel_reason`, `terminal_at` or `document_ids` → refused or
    overwritten with OLD; re-claim of an unexpired lease refused; lease > 10 minutes refused;
    `deferred_tenant_inactive` refused while the tenant is active.
27. **IC T-E / INV-KYC-OB-4 tamper:** P and C run with a tenant id different from the claim's `RETURNING`
    → 0 rows, no mock call, no write.

### 10.4 Worker least privilege (AZ, ADV) — security F1, F8

28. **T-ID-1:** worker sees all outbox rows, claims a due row; trigger-forced claim columns.
29. **T-ID-2 (denials, incl. NULL-tenant variants):** under the worker session, each is refused or
    affects 0 rows: INSERT platform `staff_users` with role `platform_admin`; SELECT platform `staff_users`
    (0 rows; `password_hash` unreachable); INSERT platform `audit_log`; SELECT platform `audit_log`;
    `risk_rules` INSERT/UPDATE/DELETE and SELECT of platform rows; `player_restrictions` staff insert and
    read; `sessions` insert/update; `login_attempts` read/insert; `persons` SELECT/INSERT/UPDATE/DELETE;
    `asset_operation_eligibility` and `open_bet_self_exclusion_policies` platform-row SELECT; tenant
    tables (`kyc_verifications`, `kyc_documents`, `ledger_transactions`, `ledger_entries`,
    `payment_attempts`, `withdrawal_requests`, `staff_capability_grants`, `ledger_adjustment_requests`);
    `alerts`/`alert_occurrences` for the KYC Kind (with the `alerting_session_scope()` refusal asserted);
    `alert_deliveries`; any non-claim outbox transition; outbox INSERT.
30. **T-ID-3 (visibility-0 allowlist probe, the K2-G1 shape):** enumerate every public table; under the
    worker session `count(*)` is **0** on every table except `kyc_submission_outbox` and the §3.3 public
    reference allowlist, and on every table `count(worker) <= count(WithoutTenant)`. Non-vacuity: seeded
    rows exist in every fenced table (including platform `staff_users`, platform `audit_log`, `persons`)
    and are visible to a `WithoutTenant` session.
31. **Catalogue gate (permanent, every future migration):** on the HEAD catalogue (`pg_policies`), for
    every table other than `kyc_submission_outbox`, every **permissive** policy with command INSERT,
    UPDATE, DELETE or ALL either has a positive guard the worker never satisfies (A-18 classifier) or the
    table carries all four `kyc_worker_fence_*` restrictive policies; and every permissive SELECT policy
    with a `true`/NULL-tenant/no-tenant arm is on a fenced table or the public reference allowlist. A new
    migration adding a NULL-arm policy without extending the fence fails here.
32. **Statics (F8):** (a) no non-test file outside `internal/db` (and outside `migrations/`) contains the
    literal `app.platform_service_id`; (b) `db.ServiceKYCSubmissionWorker` referenced only in
    `platform_service.go` and `outbox_worker.go:claimNext`, whose closure runs exactly one statement
    referencing only `kyc_submission_outbox`; (c) every `UPDATE kyc_submission_outbox` in non-test Go lives
    in `internal/kyc/outbox*.go`, and every tenant-side one has `claim_token = $n` in its `WHERE` (the
    claim statement and the `verification_not_submitted` cancel are the two named exceptions); (d) no KYC
    vendor outbound call outside `outbox_worker.go`, and no exported `internal/kyc` function reaches one.
33. `WithPlatformService` with an unknown string fails before any tx (regression).
34. **IC T-C / INV-KYC-OB-5 static:** `internal/kyc/enforcement*.go`, `internal/payments`,
    `internal/withdrawal` never reference `kyc_submission_outbox`.

### 10.5 Gate-level and compliance (IC C6: T-A..T-K)

35. **T-A:** payout gate, deposit gate, withdrawal request and the compliance-console read, with the
    player's create row in each of `pending`/`claimed`/`failed_terminal`/`cancelled`, give the same
    decision as with no outbox row; an approved player stays Allowed; an orphan-only account is
    `OutcomeFailed`; never `OutcomeUnavailable` from outbox state alone.
36. **T-B:** the DECISION-ROWS-1 row in those cases equals the no-verification case (R14 recorded).
37. **T-C:** = 34.
38. **T-D:** staff `ReviewVerification` of the orphan racing create phase C → CAS miss → `cancelled /
    decided_concurrently`, staff decision intact, audit with `vendor_reference_unbound=true`.
39. **T-E:** = 27.
40. **T-F:** = 20.
41. **T-G:** create ends `failed_terminal` (and separately `cancelled`) → every `pending` submit row of the
    verification is `cancelled / verification_not_submitted` with an audit row each.
42. **T-H:** closed and suspended tenant: zero mock calls, no attempt consumed, no status write; resumes
    when `active`.
43. **T-I:** the staff read returns `submission_state` per §7.2 for each state combination, with no vendor
    reference, document data or counters; a player response contains no outbox field (unless POP adopts
    the enum).
44. **T-J:** a delayed `sent` with a definitive `pending` on a new verification moves an approved player to
    `OutcomePending` (deliberate; R13 pinned).
45. **T-K:** every obligation-relevant transition (enqueue, retry, terminal, cancel, sent) has exactly one
    audit row with the §3.5 shape, and **every worker-written audit row carries
    `metadata.platform_service = "kyc_submission_worker"`** (Q-S3).

### 10.6 Audit, alerting, redaction (AU)

46. Redaction: a vendor error with a secret-shaped string and a vendor reference → absent from audit rows,
    alert attributes, `last_error_class`, and the lost-claim / phase-B error logs.
47. Alert dedup (one alert, N occurrences per discriminator; separate per tenant and per suffix); subject
    tenant reads, cannot ack; other tenants cannot see.
48. **Tenant B cannot raise the KYC Kind with tenant A as subject.**
49. Swallowable SQLSTATE in the raise savepoint → `failed_terminal` still commits; `Flush` retries in tenant
    scope; `40P01` in phase C propagates (not swallowed) into the bounded phase-C retry.
50. Kind parity (all columns, reverse direction).
51. Discriminator closed set and suffixes.

### 10.7 Migration (MIG)

52. Up: exact outbox policy set `{kso_tenant_select SELECT, kso_tenant_insert INSERT, kso_tenant_update
    UPDATE, kso_kyc_worker_select SELECT, kso_kyc_worker_claim UPDATE}`; `relforcerowsecurity`; the 36 fence
    policies exist, are `RESTRICTIVE`, with the exact predicate; `alert_kinds` policies exactly
    `{alert_kinds_read_all, alerts_platform_service_dispatcher}`; `alert_kinds` FORCE on; grants exactly
    `SELECT, INSERT, UPDATE`; **runtime-role `INSERT INTO alert_kinds` refused after up**.
53. Down on an empty scratch DB succeeds; up again; `migrate verify` OK.
54. Down refuses with a `pending` row, a `claimed` row, and **with only terminal rows**; after each refusal
    the table, its FORCE RLS, the fences, the Kind, `alert_kinds` FORCE RLS and the deny trigger are intact.
55. Down refuses (legible) when an alert of the Kind exists (no outbox rows), with the same intactness.
56. Jurisdiction 0075/0077 rollback tests unchanged; 0077 whitelist stays 11.
57. Seed atomicity: the seed block with an injected failure after `CREATE POLICY` leaves no policy and no
    row; the temporary policy rejects any other Kind value.

### 10.8 Wiring

58. `kyc_outbox_wiring_test.go` (one builder/loop in a goroutine, run ctx, configured interval, drain;
    nil dependencies refused; immediate first pass; panic recovery; env parsing; `ValidateForLoop` F9
    inequality refused below the bound).
59. No `idle in transaction` session while the mock is inside a vendor call on the worker path (with a
    positive control).

### 10.9 Mutants (evidence `docs/plans/payment-readiness/evidence/prh2-e1-mutation-kill.txt`)

| # | Mutant | Killed by |
|---|---|---|
| M1 | drop `claim_token = $2` from phase-C CAS | 15 |
| M2 | drop `claim_token` from P re-select | 15, 27 |
| M3 | remove `SKIP LOCKED` (and, separately, `FOR UPDATE`) | 14 |
| M4 | claim ignores `next_attempt_at` / `lease_expires_at` | 11, 12 |
| M5 | ambiguous mapped through `statusForOutcome` | 3, 6 |
| M6 | `failed_terminal` writes a verification status | 5, 6, 35 |
| M7 | phase B inside a P/C closure | 32, 59 |
| M8 | trigger allows `pending → claimed` from a tenant | 23 |
| M9 | worker `WITH CHECK` admits `sent` | 29 |
| M10 | worker policy loses `app.tenant_id IS NULL` | 22 |
| M11 | tenant policies lose acting/service exclusions | 22 |
| M12 | re-claim does not count `failed_attempts` | 12 |
| M13 | deferral consumes an attempt | 7, 42 |
| M14 | insert trigger trusts the Go key | 24, 25 |
| M15 | live-key index unconditional | 17 |
| M16 | submit claimable while create live | 18 |
| M17 | alert post-commit / `Flush` skipped | 5, 49 |
| M18 | alert raised under the worker identity | 29 |
| M19 | discriminator from raw provider id | 51 |
| M20 | down refusal removed | 54 |
| M21 | `ServiceKYCSubmissionWorker` used outside `claimNext` | 32 |
| M22 | worker claim resets `failed_attempts` / `next_attempt_at` | 26 |
| M23 | re-claim ignores lease expiry | 26 |
| M24 | remove the worker fence on `staff_users` / `audit_log` (each separately; also `persons`) | 29, 30, 31 |
| M25 | P/C take the tenant id from elsewhere than `RETURNING`, or skip the `(id, claim_token)` re-select | 27 |
| M26 | drop P's provider-configured re-check | 7 |
| M27 | binding mismatch retryable | 9 |
| M28 | seed policy not dropped | 52, 57 |
| M29 | lease upper bound removed from the trigger | 26 |
| M30 | principal exclusion dropped from tenant write policies | 22 |
| M31 | alert attribute outside the allowlist (raise swallowed, alert lost) | 5, 50 |
| M32 | per-player live-create bound removed | 20 |
| M33 | down refusal ordering reverted / split into two blocks | 54, 55 |
| M34 | deferral accepted while tenant active | 26 |
| M35 | create CAS miss treated as phase-C failure (re-send loop) instead of `decided_concurrently` | 38 |
| M36 | create terminal does not cancel pending submits | 41 |

---

## 11. Reviewers and Definition of Done

### 11.1 Reviewers

security (hard gate, incl. a short delta re-check of r2 before code and the mandatory implementation
review); identity-compliance (owner; IC F3 sign-off against T-A..T-C and M5/M6); architect; code-reviewer;
qa; devops (wiring); product-owner-proxy (only for the optional player enum). **ledger-finance not
required:** a static check that no file in the E1 diff imports `internal/ledger`, `internal/payments`,
`internal/withdrawal` or `internal/adjustment`, plus T-ID-2's ledger denials; if any appears, LF becomes
required.

### 11.2 Definition of Done

1. 0114 up/down + init-app-role block; all §10 tests green locally with `-race`; mutation evidence;
   `migrate verify` OK; the live-catalogue fence derivation recorded in the implementation record. Local
   runs reported as local, not CI.
2. ADR 0095 §38 turned into an implementation record; this ADR ACCEPTED with an implementation record.
3. Runbook §13 (§8.3); production-config checklist row; OpenAPI descriptions.
4. Registry and HANDOVER wording (orchestrator).
5. **Labels at merge (expected):** outbox, worker, identity, fences, Kind, raise path, staff derived state:
   `IMPLEMENTED` against the MOCK KYC provider. Real KYC vendor: `PROVIDER DEPENDENT` (§38.3 intake
   requirement). **Alert notification: `NOT IMPLEMENTED`** (no route, channel or recipient; nobody,
   including the tenant and the player, is notified; ALERT-DELIVERY-1 OPEN). Player-visible enum:
   `NOT IMPLEMENTED` unless POP adopts it. Staff requeue: `NOT IMPLEMENTED` (follow-up). Retention:
   `NOT IMPLEMENTED` (HQ-E1-4). NULL-ARM-WRITE-1: `NOT IMPLEMENTED` (separate item).

---

## 12. Human decisions (OPEN; engineering defaults shown, not decisions)

- **HQ-E1-1 — suspended/closed tenants.** Default: no vendor call; deferred indefinitely without
  consuming attempts (DB-validated); never cancelled; never a status write. Auto-cancelling a closed
  tenant's submissions, or ever sending for a suspended tenant, is a HUMAN/LEGAL data-processing and
  contractual decision. (Security and identity-compliance support the fail-closed default.)
- **HQ-E1-2 — severity** of `kyc.submission_failed_terminal`. Default `p2`. Human/compliance confirms or
  raises to `p1` **before 0114 ships** (a later change needs a migration).
- **HQ-E1-3 — deadlines.** Whether a jurisdiction imposes a KYC submission/completion deadline. If so it is
  jurisdiction configuration, never a worker constant; the retry budget is a PLACEHOLDER technical bound.
- **HQ-E1-4 — retention** of outbox rows (ids/states only; they are the only durable claim record).
  Default: keep everything; no deletion path; any future deletion keeps claim history at least as long as
  audit retention, or audits the claim. Also: whether erasure of a document obliges any action on an
  already-sent submission (IC C5).
- **NULL-ARM-WRITE-1** (security F2): whether the pre-existing NULL-arm platform-admin write exposure
  blocks production launch (security recommends closing it before launch). Registered by the orchestrator;
  not decided or fixed in E1.
- Recipients/on-call: HD-PRH2-4-OPS (unchanged).

## 13. Open questions for reviewers (r2)

- **Q-S5 (security):** the r2 derivation found **`persons`** exposed (any no-tenant session can read every
  Person and insert/update/delete Persons; `persons_insert_any_scope` is `WITH CHECK (true)`). It is in the
  worker fence. Security should confirm it belongs in NULL-ARM-WRITE-1's scope for the other sessions, and
  whether the public reference allowlist (§3.3) should be narrowed by fencing more of it.
- **Q-R2b (product-owner-proxy):** adopt the optional player-visible coarse `submission_state` enum?
- **Q-R7 (identity-compliance):** `provider_deconfigured` cancels rather than retries; confirm against the
  credential-rotation behaviour of ADR 0093 (overlap required), or prefer `not_sent` with the retry budget.
- **Q-R8 (code-reviewer):** the create endpoint now returns 200 for an existing live create; confirm the
  status code against the API conventions (alternative: 409 with the existing id).

---

## 14. Revision 2 — mapping of every required change

| Required change (review) | Where satisfied |
|---|---|
| SEC F1 restrictive fence family; full list derived; rewrite §3.3/§9 R1; T-ID-2 NULL-tenant variants; T-ID-3 visibility-0 allowlist; catalogue gate | §0 D4; §1; §3.3; §3.6; §5.2 step 5; §5.3 step 2; §9 R1; §10.4 tests 29–31; M24 |
| SEC F2 → NULL-ARM-WRITE-1 (reference, not fix) | header Registry; §1; §3.3; §9 R1; §11.2; §12 |
| SEC F3 + IC C2 one live create per player, DB-enforced, idempotent | §2.1 `player_account_id`; §2.2 index; §2.10; §7.2; tests 20/40; M32 |
| SEC F4 provider re-check in P; `provider_deconfigured` | §2.1 CHECK list; §2.9 row 2; test 7; M26; Q-R7 |
| SEC F5 `credential_binding_mismatch` terminal, distinct discriminator, provider_id charset CHECK | §2.1; §2.8; §4.1; test 9; M27 |
| SEC F6 column-by-column worker pinning; trigger-validated deferral | §2.3 table; §2.4; test 26; M22, M23, M29, M34 |
| SEC F7 + IC C4 down: refusal first, one DO block, refuse on any row | §5.3; tests 54–55; M33 |
| SEC F8 wider GUC static, outbox UPDATE confinement + CAS static, INV-KYC-OB-4 tamper | §3.4; §9 R2; tests 27, 32; M25 |
| SEC F9 bounded P in the lease inequality | §2.6 (P, B rows); §2.8 F9 inequality; test 58 |
| SEC F10 (adopted) `(created_at, id)` ordering | §2.1 note; §2.2; §2.7; §2.9 |
| SEC F11 (adopted) `apply_conflict` terminal | §2.8; §4.1; §4.3; test 10 |
| SEC F12 ON CONFLICT DO NOTHING in P re-enqueue | §2.9; test 19 |
| SEC F13 residuals | §9 R15, R16 |
| Q-S2 conditions | §4.4; tests 52, 57; M28 |
| Q-S3 system actor + `metadata.platform_service`; test | §3.5; test 45 |
| Q-S4 WHERE-CAS sufficient | §0 D5; §2.3; §3.5 (no GUC) |
| Q-R4 conditions (F8 static, F12, 40P01 propagation, test 16 `-count >= 20`) | §2.6 lock-order rule; tests 16, 32, 49 |
| Security missing-tests list | §10.3 26–27; §10.4 29–32; §10.5 45; §10.6 46, 48; §10.7 52, 54; tests 7, 9, 19, 20 |
| Security mutants M22–M34 | §10.9 |
| IC C1 staff-visible derived state; optional player enum (POP); DoD alerting NOT IMPLEMENTED / no notification path | §0 D11; §4.2; §7.2; §11.2 item 5; test 43 |
| IC C3 staff review racing create C → `cancelled`, never overwrite, audit, runbook | §2.1 `decided_concurrently`; §2.8; §2.9; §7.1; §8.3; test 38; M35 |
| IC C5 delayed supersession; where document content is read; ambiguous terminal reconciliation; `claims` for deferred rows; document deletion | §9 R13 + ADR 0095 §38.2; §2.6 B row; §8.3; §2.3 note; §2.9 |
| IC C6 tests T-A..T-K; INV-KYC-OB-5 incl. static | §10.5 35–45; test 34; ADR 0095 §38.4 |
| IC Q-R1(a) cancel pending submits when create terminal | §2.3; §2.9 last paragraph; test 41; M36 |
| Jurisdiction rule; retry budget PLACEHOLDER | §2.8; §12 HQ-E1-3 |
| HQ-E1-1..4 kept OPEN with defaults | §12 |
| Reconcile with deployed `main.go` (H, I-wire blocks) and shared files | §7.4; §8.2 |
