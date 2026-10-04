# ADR 0106 — KYC submission outbox, the dedicated KYC worker identity, and the KYC alert Kind (PRH-2 E1, KYC-SUBMIT-OUTBOX-1)

- **Status:** **PROPOSED**, design only, 2026-10-04. Nothing in this ADR is implemented. Every item is
  `NOT IMPLEMENTED` until the E1 implementation merges and passes its review chain (§11). Drafted by
  `architect` for PRH-2 workstream E1, design phase.
- **Base:** main `3517980` (H, I-wire and K2 merged; latest migration `0113_governed_manual_adjustments`).
- **Decision type:** cross-domain architecture. It adds a new table and its RLS families, a new
  `PlatformService` member (ADR-level per `internal/db/platform_service.go:13-18`), a new alert Kind
  seeded into the migration-only `alert_kinds` vocabulary (ADR 0102), a new background process in
  `cmd/platform-api`, and a behaviour change to the player KYC API (create and upload become
  asynchronous towards the vendor).
- **Owner:** `identity-compliance` (implementation). `architect` (this design).
- **Reviewers (hard gates):** `security` (identity, RLS, seeding mechanism), `architect`,
  `code-reviewer`, `qa`. `ledger-finance` is **not** a required gate: E1 touches no ledger table, no
  balance and no money movement (§11.1 says how that is proven). `devops` reviews the loop wiring.
- **Binding inputs:**
  - ADR 0105 §2 (HD-PRH2-10, a dedicated KYC alert Kind) and §3 (HD-PRH2-11, a dedicated non-human,
    least-privilege KYC worker identity), migration **0114**;
  - plan `docs/plans/prh2-hardening-round/plan.md` §5-E1, §5.0 T-1..T-4, §12, and the IC F3 DoD;
  - preflight `docs/plans/prh2-hardening-round/reviews/e1-k3-preflight.md` §1 (findings 1-2 are
    HD-PRH2-10/11);
  - ADR 0095 §15.2, §15.3, §15.3.3, the F2 "permanent-503 gap" note, §37 (the H sweeper precedent);
  - ADR 0096 §2.6(g), §19, §24;
  - ADR 0102 §3.1, §4.1, §4.3, §5, §7, §17 (alerting);
  - ADR 0081 §3.2 (the platform-service identity pattern), ADR 0099/0100 (acting sessions, the
    A-18 null-arm guard and the K2-G1 dynamic visibility probe);
  - CLAUDE.md (multi-tenancy, provider abstraction, environment safety).
- **Amends:** ADR 0095 §15.2/§15.3. The amendment text is ADR 0095 **§38** (written alongside this
  ADR; a pointer line is added under the §15.2 and §15.3 tables).
- **Registry:** KYC-SUBMIT-OUTBOX-1 (closes against MOCK when E1 merges; a real vendor stays
  `PROVIDER DEPENDENT`). Related: ALERT-DELIVERY-1 (stays **OPEN**), HD-PRH2-4-OPS, F-POOL-2 (KYC part).

Labels used in this document: **Blueprint/plan/ADR** means a recorded requirement.
**RECOMMENDATION** means an engineering default chosen here, reversible, not a business, legal or
compliance threshold. **QUESTION** means it is surfaced for a human or a reviewer and not decided here.

---

## 0. Decisions at a glance

| # | Decision |
|---|---|
| D1 | One durable table, `kyc_submission_outbox`, carries **both** KYC operations: `create` (ADR 0095 §15.2, the vendor `CreateVerification` call) and `submit` (§15.3, the vendor `SubmitVerification` call). The registry wording is "create/submit"; the permanent-503 gap the ADR 0095 F2 note assigns to this item is a create-side gap. |
| D2 | **Pure outbox, single claimant.** The HTTP handlers only perform phase A (the domain row plus the outbox row, one tenant transaction). No provider call remains on any HTTP path for KYC. Only the worker claims. The inline exported `kyc.CreateVerification` / `kyc.SubmitVerification` entry points are removed; their phase B/C bodies become unexported item executors reachable only from the worker. |
| D3 | The worker identity is a new closed-set `PlatformService` member **`kyc_submission_worker`**. It is used for exactly one thing: the cross-tenant **discovery and claim** statement on `kyc_submission_outbox`. It has no policy on any other table. Every tenant-data step (reading the verification and documents, resolving the per-tenant credential, applying the result, auditing, raising the alert) runs in a plain `WithTenant` transaction whose tenant id comes from the claimed outbox row the database returned, never from a client or config. |
| D4 | The claim transition `pending → claimed` (and re-claim of an expired lease) is **only** possible from a validated `kyc_submission_worker` session: RLS plus a guard trigger. A tenant session can never claim. Every post-claim transition requires the claim token in the `WHERE` clause (CAS) and happens in a tenant session. |
| D5 | An ambiguous or not-sent result never changes `kyc_verifications`. It moves the outbox row back to `pending` with a backoff, and after the configured number of failed attempts to `failed_terminal`. `failed_terminal` writes **no** verification status, so a vendor outage can never manufacture a `failed` (IC F3). |
| D6 | One dedicated alert Kind, **`kyc.submission_failed_terminal`**, platform-owned with a subject tenant, raised **in the same tenant transaction** that writes `failed_terminal` (`alerting.InTx` + `RaiseGuarded`, the I-wire pattern). The raise path never uses the worker identity, so `alerting_session_scope()` and every ADR 0102 identity rule stay byte-identical. |
| D7 | Migration **0114** creates the table, its RLS families, triggers and grants, and seeds the Kind through a **temporary, role-scoped, Kind-scoped INSERT policy created and dropped inside one atomic `DO` block**. FORCE RLS on `alert_kinds` is never lifted (0048 S-1 precedent). |
| D8 | Time: the database clock (`now()`) is the only clock for due-ness, leases and backoff deadlines. Go computes backoff **durations** with a pure function. Tests drive time through fixtures and the injectable loop ticker (T-1). |

---

## 1. Context

- **Today (verified at `3517980`):**
  - `kyc.CreateVerification` (`internal/kyc/verification_service.go`) commits an orphan row
    (`status='unverified'`, `provider_reference NULL`) in phase A, calls the vendor in phase B with no
    transaction held, and applies the result by CAS in phase C. There is no retry. If phase C fails
    after the vendor accepted, the row stays an orphan forever, and every callback for that vendor
    reference returns a retryable 503 until the vendor gives up (ADR 0095 F2, "permanent-503 gap").
  - `kyc.SubmitVerification` (`internal/kyc/document_service.go`) is called inline by the document
    upload handler (`internal/httpserver/kyc_handlers.go:320`) after the upload transaction commits.
    An ambiguous, timeout or transport-error result leaves the status unchanged (IC condition 2) and
    nothing retries it; "the next upload re-submits".
  - ADR 0095 §15.3 and the registry make a durable submission outbox a **hard precondition on the
    first real KYC adapter** (IC condition 5).
- **The orphan exclusion.** ADR 0096 §2.6(g) excludes `NOT (status='unverified' AND
  provider_reference IS NULL)` from "latest decided row" selection
  (`internal/kyc/enforcement.go` `orphanRowExclusionSQL`). E1 relies on this predicate exactly as it is
  and does **not** edit `enforcement.go`.
- **Alerting.** `alert_kinds` is FORCE RLS with a single `FOR SELECT USING (true)` policy and deny
  triggers on UPDATE/DELETE/TRUNCATE; only a migration writes it (0110:121-216).
  `alerting_session_scope()` refuses every `app.platform_service_id` other than `alert_dispatcher`
  (0110:70-73). A KYC worker can therefore not raise from its own service transaction, and this ADR
  does not change that.
- **Platform-service identities today:** `sportsbook_catalogue_sync` (0084) and `alert_dispatcher`
  (0110). Adding a member is an ADR-level decision plus a migration (`platform_service.go:13-18`).

---

## 2. The outbox table (migration 0114)

### 2.1 Columns

```sql
CREATE TABLE kyc_submission_outbox (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    seq                BIGINT GENERATED ALWAYS AS IDENTITY,          -- total order, used for supersession
    tenant_id          UUID NOT NULL REFERENCES tenants (id),
    verification_id    UUID NOT NULL,
    FOREIGN KEY (verification_id, tenant_id) REFERENCES kyc_verifications (id, tenant_id),  -- 0040 UNIQUE (id, tenant_id)
    operation          TEXT NOT NULL CHECK (operation IN ('create', 'submit')),
    provider_id        TEXT NOT NULL CHECK (octet_length(provider_id) BETWEEN 1 AND 64),
    -- Pinned at enqueue; sorted ascending by text form; empty for 'create'.
    document_ids       UUID[] NOT NULL DEFAULT '{}',
    -- Content-derived (ADR 0095 §15.2/§15.3): 'kv:'||verification_id for create;
    -- 'ks:'||verification_id||':'||hex(sha256(sorted ids, NUL-terminated)) for submit.
    -- Recomputed and VERIFIED by the insert trigger (never trusted from Go).
    idempotency_key    TEXT NOT NULL CHECK (idempotency_key ~ '^(kv:[0-9a-f-]{36}|ks:[0-9a-f-]{36}:[0-9a-f]{64})$'),
    state              TEXT NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('pending', 'claimed', 'sent', 'failed_terminal', 'cancelled')),
    claim_token        UUID NULL,          -- forced by trigger on every claim; retained after leaving 'claimed' as the last-claim record
    claimed_by_service TEXT NULL CHECK (claimed_by_service IS NULL OR claimed_by_service = 'kyc_submission_worker'),
    claimed_at         TIMESTAMPTZ NULL,
    lease_expires_at   TIMESTAMPTZ NULL,
    claims             INT NOT NULL DEFAULT 0 CHECK (claims >= 0),
    failed_attempts    INT NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error_class   TEXT NULL CHECK (last_error_class IN
                           ('ambiguous', 'not_sent', 'lease_expired', 'deferred_tenant_inactive')),
    cancel_reason      TEXT NULL CHECK (cancel_reason IN
                           ('superseded', 'verification_terminal', 'no_documents',
                            'document_set_changed', 'verification_not_submitted')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at        TIMESTAMPTZ NULL,

    CHECK ((state = 'claimed') = (lease_expires_at IS NOT NULL)),
    CHECK ((state = 'claimed') <= (claim_token IS NOT NULL)),                 -- claimed => token
    CHECK ((state IN ('sent', 'failed_terminal', 'cancelled')) = (terminal_at IS NOT NULL)),
    CHECK ((state = 'cancelled') = (cancel_reason IS NOT NULL)),
    CHECK (state <> 'failed_terminal' OR last_error_class IS NOT NULL),
    CHECK ((operation = 'create') = (cardinality(document_ids) = 0)),
    CHECK (operation <> 'create' OR idempotency_key = 'kv:' || verification_id::text),
    CHECK (operation <> 'submit' OR idempotency_key LIKE 'ks:' || verification_id::text || ':%')
);
```

No column holds PII, document content, a storage reference, a vendor reference, a credential, a raw
error text or vendor-supplied free text. The worker identity can read this table across tenants
(§3.3), so this is a binding rule: **a later column that carries any of those needs a new security
review.**

### 2.2 Indexes and uniqueness

```sql
-- Duplicate submit of the same content is a no-op while a live row exists. After failed_terminal or
-- cancelled, the same content may be enqueued again (operator/player remedy, §8.3).
CREATE UNIQUE INDEX kyc_submission_outbox_key_live
    ON kyc_submission_outbox (tenant_id, idempotency_key)
    WHERE state IN ('pending', 'claimed', 'sent');

-- A verification is created with the vendor at most once, whatever the state.
CREATE UNIQUE INDEX kyc_submission_outbox_one_create
    ON kyc_submission_outbox (tenant_id, verification_id)
    WHERE operation = 'create';

CREATE INDEX kyc_submission_outbox_due     ON kyc_submission_outbox (next_attempt_at, seq) WHERE state = 'pending';
CREATE INDEX kyc_submission_outbox_lease   ON kyc_submission_outbox (lease_expires_at)     WHERE state = 'claimed';
CREATE INDEX kyc_submission_outbox_by_verif ON kyc_submission_outbox (tenant_id, verification_id, seq);
```

### 2.3 State machine

```
            (phase A, tenant session, INSERT)
                         |
                         v
   +--------------> pending ----(worker identity: claim, due)----> claimed
   |                   ^                                          |  |  |  |
   |                   |  (tenant: retry, CAS on claim_token)     |  |  |  |
   |                   +------------------------------------------+  |  |  |
   |                                                                 |  |  |
   |  (worker identity: re-claim when lease_expires_at <= now()) <---+  |  |
   |                                                                    |  |
   |                     sent  <---(tenant: definitive result applied)--+  |
   |              failed_terminal <---(tenant: attempts exhausted)------+  |
   |                  cancelled  <---(tenant: superseded/terminal/...)-----+
```

| From | To | Session | Conditions (all DB-enforced unless marked Go) | Forced by trigger |
|---|---|---|---|---|
| (none) | `pending` | tenant | INSERT; §2.5 validation | `state`, counters, claim columns, timestamps |
| `pending` | `claimed` | **worker** | `OLD.next_attempt_at <= now()`; for `submit`, no live `create` row for the same verification | `claim_token := gen_random_uuid()`, `claimed_by_service`, `claimed_at := now()`, `claims := OLD.claims + 1`; `lease_expires_at` must lie in `(now(), now() + interval '10 minutes']` |
| `claimed` | `claimed` | **worker** | `OLD.lease_expires_at <= now()` (abandoned claim) | as above, plus `failed_attempts := OLD.failed_attempts + 1`, `last_error_class := 'lease_expired'` |
| `claimed` | `sent` | tenant | Go: `WHERE claim_token = $tok` CAS; same tx as the verification apply and its audit | `terminal_at := now()`, `lease_expires_at := NULL` |
| `claimed` | `pending` | tenant | CAS; `last_error_class IN ('ambiguous','not_sent','deferred_tenant_inactive')`; `next_attempt_at` in `(now(), now() + interval '1 day']` | `failed_attempts := OLD + 1` unless `deferred_tenant_inactive` (+0); `lease_expires_at := NULL` |
| `claimed` | `failed_terminal` | tenant | CAS; `last_error_class` set | `failed_attempts := OLD + 1` unless the class is `lease_expired` (already counted at re-claim); `terminal_at := now()` |
| `claimed` | `cancelled` | tenant | CAS; `cancel_reason` set | `terminal_at := now()` |
| `sent` / `failed_terminal` / `cancelled` | anything | any | **refused** (terminal rows are immutable) | |

Columns that are immutable on every UPDATE: `id, seq, tenant_id, verification_id, operation,
provider_id, document_ids, idempotency_key, created_at`. `claim_token`, `claimed_by_service` and
`claimed_at` change **only** on a worker claim. `updated_at := now()` on every UPDATE.

**"Status never inferred" (plan E1).** No transition of this table writes `kyc_verifications`. Only
the `claimed → sent` transaction applies a verification change, and only from a **definitive** vendor
result, through the existing `applyCreateVerificationResult` CAS (create) or
`applyForwardOnlyStatus` (submit). `ambiguous`, `not_sent`, `lease_expired` and `failed_terminal`
never call `statusForOutcome`.

### 2.4 The session helper and guard trigger (SQL shape)

```sql
-- Resolves the caller's session to exactly one of two shapes, or refuses. Never SECURITY DEFINER.
CREATE FUNCTION kyc_submission_outbox_session() RETURNS TEXT AS $$
DECLARE
    v_tenant   TEXT := NULLIF(current_setting('app.tenant_id', true), '');
    v_service  TEXT := NULLIF(current_setting('app.platform_service_id', true), '');
    v_player   TEXT := NULLIF(current_setting('app.player_account_id', true), '');
    v_admin    TEXT := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
    v_princ    TEXT := NULLIF(current_setting('app.principal_id', true), '');
    v_act_t    TEXT := NULLIF(current_setting('app.acting_tenant_id', true), '');
    v_act_p    TEXT := NULLIF(current_setting('app.acting_platform_principal_id', true), '');
BEGIN
    IF v_player IS NOT NULL OR v_admin IS NOT NULL OR v_princ IS NOT NULL
       OR v_act_t IS NOT NULL OR v_act_p IS NOT NULL THEN
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

`kyc_submission_outbox_guard()` is a `BEFORE INSERT OR UPDATE OR DELETE` row trigger plus a
`BEFORE TRUNCATE` statement trigger (the 0014/0016/0110 pair):
- **TRUNCATE / DELETE:** always refused ("append-mostly; terminal rows are history").
- **INSERT:** `kyc_submission_outbox_session() = 'tenant'`, then the §2.5 validation, then the forced
  columns of the §2.3 first row.
- **UPDATE:** the immutable-column check; terminal rows refused; then exactly the §2.3 rows for the
  resolved session kind. Anything else raises. The trigger is the backstop; RLS (§3) is the primary
  control, and the `WHERE` CAS in Go is the claim check.

### 2.5 Insert validation (the trigger recomputes, it never trusts)

- `NEW.tenant_id = app.tenant_id` (also the RLS `WITH CHECK`).
- The verification exists under the caller's RLS view and `NEW.provider_id = kyc_verifications.provider_id`
  (the provider is pinned at enqueue; a later tenant configuration change does not redirect an
  in-flight row).
- `create`: the verification has the orphan shape (`status='unverified' AND provider_reference IS
  NULL`); `document_ids = '{}'`; `idempotency_key = 'kv:' || verification_id`.
- `submit`: `cardinality(document_ids) >= 1`; ids distinct and in ascending text order; every id is a
  `kyc_documents` row with the same `tenant_id` and `verification_id` and `status <> 'rejected'`;
  and the key equals the recomputation, which is byte-identical to Go's
  `submissionIdempotencyKey`:

  ```sql
  'ks:' || NEW.verification_id::text || ':' || encode(sha256(
      (SELECT string_agg(convert_to(d::text, 'UTF8') || '\x00'::bytea, ''::bytea ORDER BY d::text)
         FROM unnest(NEW.document_ids) AS d)), 'hex')
  ```

  A mismatch **raises** (it is a Go bug, never silently corrected). A Go/SQL parity test pins it.

### 2.6 Phase A, B, C and the transaction-scope rule

| Phase | Where | Transaction | What |
|---|---|---|---|
| **A (create)** | `POST` create-verification handler | **one** `WithTenant` tx with `SelectProvider` | `insertOrphanVerification` + `kyc.verification_requested` audit + outbox INSERT (`create`) + `kyc.submission_enqueued` audit. Returns **201** with the `unverified` verification (§7.3). |
| **A (submit)** | document-upload handler | the existing `UploadDocument` tx | document INSERT + `kyc.document_uploaded` audit, then gather the current non-rejected set (the existing `gatherSubmissionDocuments` read) and `INSERT ... ON CONFLICT (tenant_id, idempotency_key) WHERE state IN ('pending','claimed','sent') DO NOTHING` + `kyc.submission_enqueued` audit (metadata `duplicate=true` on conflict). A terminal verification or an empty set enqueues nothing (today's documented no-op). |
| **Claim** | worker | `db.WithPlatformService(ServiceKYCSubmissionWorker)`, **one statement** | §2.7 |
| **P (prepare)** | worker | `WithTenant(row.tenant_id)` | `SELECT ... FROM kyc_submission_outbox WHERE id=$1 AND state='claimed' AND claim_token=$2 FOR UPDATE`; zero rows means the claim was lost: stop. Read `tenants.status`; read the verification and, for `submit`, the current document set. Decide: **cancel**, **defer**, **fail-terminal on an exhausted re-claim**, or **proceed**. The first three write the CAS transition and their audit row here. *Proceed* commits with no write. |
| **B** | worker | **none** (`txscope.Held(ctx)` must be false; `ErrProviderCallRefused` otherwise) | resolve the outbound credential for `(row.tenant_id, row.provider_id)` (binding check `cred.TenantID/ProviderID/Domain` unchanged), build `CallContext{IdempotencyKey: row.idempotency_key}` and call `provider.CreateVerification` / `provider.SubmitVerification` |
| **C** | worker | `alerting.InTx(NewTenantRunner(pool, row.tenant_id))` on `context.WithoutCancel` + `phaseCTimeout` | re-check the claim `FOR UPDATE` (as in P); then exactly one of the §2.8 outcomes; then `Pending.Flush` after commit |

Rules:
- **No provider call inside any transaction closure.** The existing
  `internal/txscope/no_provider_call_in_tx_closure_static_test.go` already flags
  `SubmitVerification`/`CreateVerification` lexically inside a `pgx.Tx` closure; E1 adds no exemption.
- The worker's service transaction executes exactly one statement and touches no table other than
  `kyc_submission_outbox` (static test, §10.6).
- Phase A writes the outbox row **in the same transaction** as the verification or document row, so
  a committed document without its submission intent, or an outbox row without its domain row, is
  impossible.
- Phase C applies the verification change, the outbox transition and the audit row atomically. A
  lost claim (CAS affects 0 rows) rolls the whole phase-C transaction back, and the vendor result is
  discarded (logged with ids only; counted). The winning claimant applies its own result.
- **Lock order:** the outbox row (child) is locked before the verification row (parent) in P and C.
  No path holds a `kyc_verifications` lock and then requests an outbox lock: the callback path and
  `ReviewVerification` never touch the outbox, and phase A only takes the FK `KEY SHARE` lock, which
  does not conflict with the `NO KEY UPDATE` of the status CAS. This is recorded as a reviewer
  question against ADR 0082 R8 (Q-R4).

### 2.7 The claim statement (the worker identity's only statement)

```sql
WITH candidate AS (
    SELECT o.id
      FROM kyc_submission_outbox o
     WHERE (   (o.state = 'pending' AND o.next_attempt_at  <= now())
            OR (o.state = 'claimed' AND o.lease_expires_at <= now()))
       AND NOT (o.tenant_id = ANY ($2::uuid[]))          -- per-pass per-tenant cap (§2.9); DB-originated ids only
       AND NOT (o.operation = 'submit' AND EXISTS (
               SELECT 1 FROM kyc_submission_outbox c
                WHERE c.tenant_id = o.tenant_id AND c.verification_id = o.verification_id
                  AND c.operation = 'create' AND c.state IN ('pending', 'claimed')))
     ORDER BY CASE o.state WHEN 'claimed' THEN o.lease_expires_at ELSE o.next_attempt_at END, o.seq
     LIMIT 1
     FOR UPDATE OF o SKIP LOCKED
)
UPDATE kyc_submission_outbox o
   SET state = 'claimed',
       lease_expires_at = now() + make_interval(secs => $1)
  FROM candidate
 WHERE o.id = candidate.id
RETURNING o.id, o.tenant_id, o.verification_id, o.operation, o.provider_id,
          o.idempotency_key, o.document_ids, o.claim_token, o.claims, o.failed_attempts, o.last_error_class;
```

- **One row per claim transaction** (RECOMMENDATION). The lease then only has to cover one item
  (prepare + call + phase C), and the batch-lease reasoning of ADR 0095 §7.2 / SP-C does not arise.
  KYC volume does not justify batch claiming.
- `SKIP LOCKED` gives two workers disjoint rows; the trigger's due-ness re-check makes a stale claim
  raise rather than double-claim; `RETURNING` reflects the trigger-forced `claim_token`.
- The `tenant_id` the worker then uses is the value this statement returned from the database.

### 2.8 Outcomes, retry and backoff

| Phase-B result | Phase C (tenant tx, under the claim) | Verification | Audit |
|---|---|---|---|
| Definitive (`statusForOutcome` ok, and for create a non-empty reference) | apply the existing CAS (`applyCreateVerificationResult` / `applyForwardOnlyStatus`); outbox `claimed → sent` | changed only by the existing forward-only/CAS rules | the existing `kyc.verification_submitted` / `kyc.verification_submitted_to_provider` row, plus `outbox_id`, `claims` and `platform_service` in metadata |
| `ProviderError` outcome, transport error, timeout, empty create reference (**ambiguous**) | `claimed → pending` with `next_attempt_at = now() + backoff(failed_attempts+1)`, or `claimed → failed_terminal` when `failed_attempts + 1 >= MaxFailedAttempts`, plus the alert (§4) | **unchanged** (IC condition 2) | for `submit` with a `ProviderError` result, the existing failure row is kept; **plus** one `kyc.submission_retry_scheduled` or `kyc.submission_failed_terminal` row |
| Credential unavailable, binding mismatch, adapter not registered, `ErrProviderCallRefused` (**not_sent**) | as ambiguous, class `not_sent` | unchanged | `kyc.submission_retry_scheduled` / `kyc.submission_failed_terminal` |
| Phase C itself fails (DB error) | bounded in-item retry of phase C only (RECOMMENDATION: 3 tries inside `phaseCTimeout`), then give up: the row stays `claimed`, the lease expires, and a re-claim resends with the **same** idempotency key (counted `lease_expired`) | unchanged | none possible; log + counter |

- **Backoff:** `backoff(n) = min(BackoffBase * 2^(n-1), BackoffCap)`, a pure Go function with no
  jitter in E1 (the §37.5 simplification), unit-tested exhaustively. The **deadline** is computed by
  the database: `next_attempt_at = now() + make_interval(secs => $d)`.
- **T-1:** due-ness, lease expiry and backoff deadlines are tested by writing `next_attempt_at` /
  `lease_expires_at` fixtures into the past or future; the loop's interval uses an injectable ticker
  (the `RunDispatcherLoop` precedent). No `time.Sleep`. **T-2:** no new wall-clock assertion.
- **RECOMMENDATION defaults** (code constants on the worker struct, overridable by tests; not
  business thresholds): `BackoffBase` 30 s, `BackoffCap` 30 min, `MaxFailedAttempts` 8, `Lease` 60 s,
  per-pass item cap 100, per-pass per-tenant cap 20. `ValidateForLoop` refuses
  `Lease < 2 * (createVerificationCallTimeout + phaseCTimeout) + 5 s`. Whether any of these has a
  compliance meaning (for example a jurisdictional KYC completion deadline) is **QUESTION HQ-E1-3**.

### 2.9 Prepare-step decisions (tenant tx P)

| Condition (read in P, under the claim) | Action |
|---|---|
| `tenants.status <> 'active'` | **defer** (QUESTION HQ-E1-1): `claimed → pending`, class `deferred_tenant_inactive`, `failed_attempts` unchanged, backoff by `claims`. No audit row per deferral (it is scheduling, not an action); a counter and the row itself record it. Mirrors ADR 0095 §37.3 "no new outbound call for a non-active tenant". |
| re-claim with `failed_attempts >= MaxFailedAttempts` (abandoned claims exhausted) | `claimed → failed_terminal` (class `lease_expired`) + audit + alert, **no** provider call |
| verification terminal (`approved`, `rejected`, `expired`) | `cancelled` / `verification_terminal` + audit |
| `submit`: a newer `submit` row (higher `seq`) for the same verification exists in `pending`/`claimed`/`sent` | `cancelled` / `superseded` + audit |
| `submit`: verification still an orphan (its `create` row ended `failed_terminal`) | `cancelled` / `verification_not_submitted` + audit |
| `submit`: the current non-rejected set is empty | `cancelled` / `no_documents` + audit |
| `submit`: the current set differs from `document_ids` (a staff rejection since enqueue) | `cancelled` / `document_set_changed`, **and** enqueue the current set in the same tx (a new row, new key) + audit |
| otherwise | proceed to phase B |

---

## 3. The KYC worker identity (HD-PRH2-11)

### 3.1 Name and registration

- `internal/db/platform_service.go`:
  ```go
  // ServiceKYCSubmissionWorker is the ADR 0106 (PRH-2 E1) platform-service identity of the KYC
  // submission outbox worker: a background loop with no HTTP request, no token and no human
  // principal. Migration 0114 grants it exactly two policies, both on kyc_submission_outbox:
  // SELECT, and the UPDATE that performs the claim. It has no policy on any other table.
  // Only internal/kyc/outbox_worker.go may reference it (static test).
  const ServiceKYCSubmissionWorker PlatformService = "kyc_submission_worker"
  ```
  and the third `platformServiceAllowlist` entry. The `PlatformService` and `WithPlatformService`
  doc comments are updated to say the vocabulary is no longer catalogue-only.
- GUC mechanics are unchanged: `WithPlatformService` validates against the compiled-in allowlist
  before opening a transaction and sets `app.platform_service_id` with `set_config(..., true)`
  (transaction-local). `app.tenant_id` and `app.player_account_id` are never set in that
  transaction. The worker calls `db.AssertPlatformServiceScope(ctx, tx, db.ServiceKYCSubmissionWorker)`
  as the first statement inside the closure.
- No database role, password, attribute or credential is created or changed (CLAUDE.md environment
  safety; plan Rule 4). The identity is a GUC value, not a role.

### 3.2 RLS policies (migration 0114) — the complete set on `kyc_submission_outbox`

Common exclusion set (ADR 0102 §4.1 C-102-9, extended by `app.principal_id` for writes):

```sql
-- X(tenant) :=
--     NULLIF(current_setting('app.player_account_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
-- X(worker) :=
--     NULLIF(current_setting('app.platform_service_id', true), '') = 'kyc_submission_worker'
-- AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
-- AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL

ALTER TABLE kyc_submission_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_submission_outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY kso_tenant_select ON kyc_submission_outbox FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND <X(tenant)>);
CREATE POLICY kso_tenant_insert ON kyc_submission_outbox FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND <X(tenant)>
                AND NULLIF(current_setting('app.principal_id', true), '') IS NULL);
CREATE POLICY kso_tenant_update ON kyc_submission_outbox FOR UPDATE
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND <X(tenant)>
                AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
                AND state = 'claimed')
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND <X(tenant)>
                AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
                AND state IN ('pending', 'sent', 'failed_terminal', 'cancelled'));
CREATE POLICY kso_kyc_worker_select ON kyc_submission_outbox FOR SELECT
    USING (<X(worker)>);
CREATE POLICY kso_kyc_worker_claim ON kyc_submission_outbox FOR UPDATE
    USING      (<X(worker)> AND state IN ('pending', 'claimed'))
    WITH CHECK (<X(worker)> AND state = 'claimed');
```

- Every predicate is written out literally in the migration (the `<X(...)>` macros above are for
  reading only). There is **no** `FOR ALL`, no `DELETE` policy, no `tenant_id IS NULL` arm and no
  `USING (true)`: the A-18 replay guard (`internal/db/null_arm_replay_static_test.go`) sees positive
  guards only, and no allowlist edit is needed there.
- A staff principal session (`app.principal_id` set) may **read** its tenant's rows (future back-office
  visibility) but cannot write. Player, platform-admin, acting and any other service sessions see and
  write nothing.
- **The worker identity has no policy on any other table.** In particular it has none on
  `kyc_verifications`, `kyc_documents`, `audit_log` tenant rows, `alerts`/`alert_occurrences`, any
  ledger, wallet, payment, withdrawal, staff, capability or adjustment table.

### 3.3 What the identity can and cannot do (least privilege, stated honestly)

| Capability | Worker session |
|---|---|
| Read `kyc_submission_outbox`, all tenants | **yes** (ids, state, counters only; §2.1 no-PII rule) |
| Claim / re-claim an outbox row | **yes** (the only write) |
| Any other transition of an outbox row; INSERT; DELETE | no (RLS `WITH CHECK` + trigger) |
| Read or write any tenant-owned row in any other table | no (no policy; tenant policies require `app.tenant_id`) |
| Raise an alert | no (`alerting_session_scope()` raises `unknown platform service identity`; unchanged) |
| Write an audit row | no tenant row; see the residual below for platform rows |
| **Residual (pre-existing, shared with `sportsbook_catalogue_sync` and `alert_dispatcher`):** older dual-scope policies of the form `tenant_id IS NULL AND app.tenant_id IS NULL` (for example `audit_log.dual_scope_isolation`, 0014) admit **any** session with no tenant GUC, including a platform-service session. The worker session therefore has exactly the platform-level (NULL-tenant) visibility that a `WithoutTenant` session already has. RLS cannot distinguish them; the 0109+ policies that list `app.platform_service_id IS NULL` do exclude it. | **not widened by E1**; proven equal to `WithoutTenant` by the differential probe (§10.6 T-ID-3) and confined in Go by the one-statement static test. Security decides whether a follow-up should add `platform_service_id IS NULL` to the legacy dual-scope arms (QUESTION Q-S1). |

### 3.4 Per-tenant processing: why the hybrid, and how tenant_id stays server-side

- **Rejected: a per-tenant `WithTenant` claim loop** (the H sweeper shape). It needs no identity, but
  HD-PRH2-11 decided a dedicated identity; it scans every tenant every tick; and it makes the claim
  indistinguishable from any tenant session (a tenant session could claim).
- **Rejected: run everything under the worker identity** (policies on `kyc_verifications`,
  `kyc_documents`, `audit_log`, provider credentials). That would give a background identity
  cross-tenant read of KYC PII and write on the verification status, the opposite of least privilege,
  and would need policy changes on four sensitive tables.
- **Rejected: a mixed session** (`app.platform_service_id` **and** `app.tenant_id`). `kyc_verifications`'
  `tenant_isolation` policy (0040) checks only `app.tenant_id`, so a mixed session would satisfy every
  plain tenant policy in the schema; 0106 and C-102-9 exist to forbid this shape.
- **Chosen (hybrid):** the identity does discovery + claim, and nothing else. Each item's tenant work
  runs in plain `WithTenant(row.tenant_id)` transactions. `row.tenant_id` is the value the claim
  statement `RETURNING`ed; P and C re-select the row by `(id, claim_token)` **under tenant RLS**, so a
  wrong tenant id yields zero rows and the item stops. The composite FK makes
  `outbox.tenant_id = verification.tenant_id` structural. Credentials are resolved per item for that
  tenant only, with the existing binding check.
- **Tenant isolation of failures:** per-item panic recovery and error isolation; one tenant's failing
  item never stops the pass. Head-of-line delay across tenants remains (§9 R4).

### 3.5 Audit actor shape

- Every audit row the worker writes is a **tenant** row (written in P or C under `WithTenant`),
  `ActorType = system` (`actor_id NULL`, as the existing `kyc.verification_submitted*` rows already
  are), with `metadata.platform_service = "kyc_submission_worker"` (a compiled constant, never config),
  `outbox_id`, `operation`, `claims`, `failed_attempts` and, where relevant, `error_class` (closed
  enum) or `cancel_reason`. Never a vendor reference, credential, document content or raw error text.
- The claim itself is not an audit row (it is lease bookkeeping); its durable record is the row's
  trigger-forced `claim_token`, `claimed_by_service`, `claimed_at` and `claims`. Every claim that does
  real work ends in exactly one audited transition; an abandoned claim is audited by the next
  transition (`lease_expired`).
- New audit actions: `kyc.submission_enqueued`, `kyc.submission_retry_scheduled`,
  `kyc.submission_failed_terminal`, `kyc.submission_cancelled`. Existing actions are unchanged.
- Alternative for security (QUESTION Q-S3): `ActorType = service` with a fixed UUIDv5 derived from the
  service name, which would make the worker queryable by `idx_audit_log_actor`. Not chosen by default
  because the existing KYC submission rows are `system` and changing them is out of E1's scope.

### 3.6 How this identity differs from the other two

| | `sportsbook_catalogue_sync` (0084) | `alert_dispatcher` (0110) | **`kyc_submission_worker` (0114)** |
|---|---|---|---|
| Tables with a policy | 5 platform catalogue tables (no `tenant_id`) | 5 alerting tables | **1** (`kyc_submission_outbox`) |
| Writes | INSERT/UPDATE catalogue | INSERT deliveries; INSERT of 3 meta-Kinds | **UPDATE: the claim only** |
| Reads tenant-owned data | no | alerts of all tenants (ids, kind, attributes) | outbox rows of all tenants (ids only) |
| Raises alerts | no | meta-Kinds only | **no** (its tenant-scoped phase C raises, §4) |
| Confined to (static test) | `internal/sportsbook` sync + `main.go` | `internal/alerting` dispatcher/fallback | `internal/kyc/outbox_worker.go` claim function |

---

## 4. The KYC alert Kind (HD-PRH2-10)

### 4.1 Definition

| Field | Value | Why |
|---|---|---|
| `kind` | `kyc.submission_failed_terminal` | matches `^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$` |
| `severity` | `p2` (**proposed; QUESTION HQ-E1-2**) | ADR 0102 §3.1: `p1` is integrity or money-correctness; this is an operational/compliance-process failure with no money effect. A human or security may raise it to `p1`. |
| `scope` | `platform` | 0110's CHECK forbids `requires_subject=false` outside the meta-Kinds; no tenant-scope Kind exists or is routable in PRH-2 (ADR 0102 §4.3, `alert_routes.tenant_id IS NULL`). Adding the first tenant-owned Kind would trigger the carried security condition (the `alerts_tenant_owned` exclusion matrix) for no benefit. |
| `simulation` | `false` | |
| `requires_subject` | `true` | `subject_tenant_id` = the outbox row's tenant. That tenant can read it read-only (`alerts_subject_tenant_read`) and never ack/resolve it (AL-2). |
| `in_tx_raisable_by_tenant` | `true` | raised from the tenant phase-C session (`alerts_subject_tenant_raise`) |
| `allowed_keys` | `{operation, provider_id, last_error_class, outbox_id}` | closed, flat scalars. No player, person or verification id, no vendor reference, no error text. |
| `raise_mode` | `in_tx` | the alert commits iff `failed_terminal` commits |

**Discriminator:** `<operation>:<provider_id>` (for example `submit:mock`), built only from the
outbox row. If `provider_id` does not match `^[A-Za-z0-9_.-]{1,64}$` the discriminator is
`<operation>:provider_unclassified` (the I-wire closed-set precedent; a mutant pins it). Dedup is
therefore one open alert per (subject tenant, operation, provider): a vendor outage that fails a
thousand rows produces one alert with a thousand occurrences, not a thousand alerts. The first
occurrence's `outbox_id` and `last_error_class` stay in `attributes` (attributes are immutable after
the first insert); later rows are found through the outbox (runbook §8.3).

### 4.2 The sanctioned raise path (ADR 0102 identity rules untouched)

1. Phase C opens through `alerting.InTx(ctx, alerting.NewTenantRunner(pool, row.tenant_id), fn)`.
2. Inside `fn`, after the CAS `claimed → failed_terminal` affected exactly one row and the
   `kyc.submission_failed_terminal` audit row is written, the worker calls
   `alerting.RaiseGuarded(ctx, tx, alerting.Alert{Kind: KindKYCSubmissionFailedTerminal,
   SubjectTenantID: row.tenant_id, Discriminator: ..., Attributes: ...})`.
3. After `InTx` returns a `*Pending`, the worker calls `Flush`, so a swallowed raise gets its
   mandatory detached retry in the **same tenant scope** (`RaiseDetached` reopens from the runner,
   never from the Alert; SR-1), and the terminal fallback `alerting.raise_failed` still belongs to the
   dispatcher identity only (unchanged).
- `raised_by_scope` is forced to `tenant` by `alerting_session_scope()`.
- The worker identity is never used to raise, `alerting_session_scope()` is not edited, and
  `db.ServiceAlertDispatcher` is never referenced from `internal/kyc` (the existing
  `TestStatic_ServiceAlertDispatcherConfinedToDispatcherAndFallback` keeps proving it).
- **Rejected:** a post-commit detached raise after `failed_terminal` commits (a crash between the two
  loses the alert; `in_tx` + `Flush` does not); and widening `alerting_session_scope()` to accept
  `kyc_submission_worker` (it would weaken ADR 0102's identity rule for no gain).
- **Delivery:** the dispatcher is wired with the log sink only; no route, channel or recipient exists.
  The alert is durable and `unrouted`. **ALERT-DELIVERY-1 stays OPEN**; nothing pages anyone
  (HD-PRH2-4-OPS).

### 4.3 Go mirror and parity

- `internal/alerting/kind.go`: constant `KindKYCSubmissionFailedTerminal Kind =
  "kyc.submission_failed_terminal"` and its `kindDefs` entry with exactly the §4.1 values.
- `TestMigration0110_KindsSeeded` (it walks `Kinds()` against a HEAD-migrated scratch DB) then covers
  the five flags it already checks. E1 adds `internal/alerting/migration_0114_kyc_kind_integration_test.go`,
  which checks **all** columns of the new row, including `allowed_keys` (as a set) and `raise_mode`,
  and the reverse direction for the whole vocabulary (every `alert_kinds` row has a `kindDefs` entry).
  It is a new file, so the I-core test file is not edited.
- I-wire's static wiring guard: the helper that calls `RaiseGuarded`
  (`internal/kyc/outbox_worker.go:raiseSubmissionFailedTerminal`) is added to
  `staticRaiseGuardedHelpers`, and its owner (`internal/kyc/outbox_worker.go:runPhaseC`) to
  `staticInTxOwners`, in `internal/alerting/static_wiring_test.go`.

### 4.4 Seeding under FORCE RLS (migration 0114)

`alert_kinds` has FORCE RLS and only a SELECT policy, so the migration (owner) role cannot INSERT.
The 0110 precedent (seed before FORCE) is unavailable to a later migration, and toggling FORCE RLS in
a forward migration was **rejected** by security in 0048 (S-1): a file run standalone under `psql`
that stops between the toggle and the restore leaves isolation off. The chosen mechanism:

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

- **Atomic even standalone:** a `DO` block is one statement, so under `psql` autocommit it either
  commits whole (policy created, row inserted, policy dropped) or not at all. The policy never
  survives the statement and is never visible to another session (uncommitted catalog change).
- **Narrow:** the temporary policy is `FOR INSERT`, `TO CURRENT_USER` (the migrating owner, never
  `igaming_runtime`) and admits only this one Kind value. FORCE RLS stays on throughout.
- **Static guards:** `CREATE POLICY` and `DROP POLICY` are written literally at line start, so the A-18
  replay sees the policy created and dropped (net effect none). It is not generated through
  `EXECUTE`, so `TestA18_DynamicPolicyGenerationIsRestrictiveOnly` is unaffected.
- This is a **new pattern** for the alerting vocabulary and needs explicit security approval (Q-S2).
  The same pattern is the sanctioned route for any later Kind (ALERT-KINDS-DEDICATED-1).

---

## 5. Migration 0114

### 5.1 Files

- `migrations/0114_kyc_submission_outbox.up.sql`
- `migrations/0114_kyc_submission_outbox.down.sql`

Number: **0114** per ADR 0105 and plan §11/§12 (K3 is 0115). Rule 3: if any other migration merges
first, E1 renumbers at merge.

### 5.2 Up (in order)

1. `kyc_submission_outbox_session()` (§2.4).
2. `CREATE TABLE kyc_submission_outbox` (§2.1), indexes (§2.2), `COMMENT ON TABLE`.
3. `kyc_submission_outbox_guard()` and its row trigger + TRUNCATE trigger (§2.4, §2.5).
4. `ENABLE` + `FORCE ROW LEVEL SECURITY`; the five policies (§3.2).
5. Grants, mirrored from `deploy/init-app-role.sql` (the 0110 §7 pattern):
   ```sql
   DO $$
   BEGIN
       IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
           EXECUTE 'REVOKE ALL ON kyc_submission_outbox FROM igaming_runtime';
           EXECUTE 'GRANT SELECT, INSERT, UPDATE ON kyc_submission_outbox TO igaming_runtime';
       END IF;
   END $$;
   ```
   No DELETE, no TRUNCATE. INSERT through a `GENERATED ALWAYS AS IDENTITY` column needs no grant on the
   identity sequence; the runtime-role test proves it, and if PostgreSQL proves otherwise the only
   permitted addition is `USAGE` on that owned sequence.
6. The `alert_kinds` seed `DO` block (§4.4).

No existing table, function or policy is altered. `enforcement.go`'s orphan predicate, the 0040
policies and every alerting object stay byte-identical.

### 5.3 Down (must succeed on an empty scratch DB)

```sql
-- 1. Remove the Kind. FK checks bypass RLS, so a referencing alert/occurrence makes this fail
--    un-blindably (the 0048 constraint-validation precedent); the handler names the remedy.
DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0114 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'kyc.submission_failed_terminal');
    DELETE FROM alert_kinds WHERE kind = 'kyc.submission_failed_terminal';
    DROP POLICY alert_kinds_unseed_0114 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0114 down: refusing - alerts of kind kyc.submission_failed_terminal exist; resolve and archive them under an authorized procedure first';
END $$;

-- 2. Refuse while non-terminal outbox rows exist, then drop. One DO block, so the RLS lift, the
--    count and the DROP are atomic even under standalone psql (the 0110-down precedent, hardened).
DO $$
DECLARE v_count BIGINT;
BEGIN
    ALTER TABLE kyc_submission_outbox NO FORCE ROW LEVEL SECURITY;   -- owner then bypasses RLS for the count
    SELECT count(*) INTO v_count FROM kyc_submission_outbox WHERE state IN ('pending', 'claimed');
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0114 down: refusing - % non-terminal kyc_submission_outbox row(s) exist', v_count;
    END IF;
    DROP TABLE kyc_submission_outbox;
END $$;

DROP FUNCTION kyc_submission_outbox_guard();
DROP FUNCTION kyc_submission_outbox_session();
```

- On an empty scratch DB both blocks succeed, so the jurisdiction full-chain rollback tests
  (`internal/jurisdiction/migration_0075_integration_test.go`, `migration_0077_integration_test.go`,
  which roll back everything above 0099 with a dynamically derived count) keep passing.
- **Terminal rows are dropped by a successful down** (plan §4: "refuse while non-terminal rows
  exist"); their `audit_log` rows remain. Whether down should instead refuse while **any** row exists
  is QUESTION Q-R5.

### 5.4 Pinned tests a migration can trip

| Test | Effect of 0114 | Action |
|---|---|---|
| `internal/jurisdiction/migration_0077_integration_test.go` exact policy whitelist (11 tuples on `tenants`, `licences`, `jurisdictions`) | **none**: 0114 adds no policy on those tables | **no entries to add**; must stay 11 |
| `internal/jurisdiction/migration_0075/0077` full-chain rollback | down must succeed empty | §5.3 |
| `internal/alerting/migration_0110_integration_test.go` `TestMigration0110_KindsSeeded` | fails if the Go Kind lands without the seed | seed in 0114 (§4.4) |
| `internal/db/null_arm_replay_static_test.go` (A-18) | new policies scanned | positive guards only; no allowlist change |
| `internal/db/k2_gates_integration_test.go` `TestK2G1_ActingSessionRowVisibilityMatchesAllowlist` | new table enumerated | acting sessions see 0 rows (tenant policies exclude acting GUCs); no allowlist change |
| `internal/alerting/static_dispatcher_identity_test.go` | none if KYC never names the dispatcher | keep it so |
| `internal/alerting/static_wiring_test.go` | new `RaiseGuarded` helper | add helper + owner (§4.3) |
| `internal/txscope/no_provider_call_in_tx_closure_static_test.go` | worker phase B | outside every closure; no exemption |
| `internal/kyc/kyc_two_phase_integration_test.go`, `recording_tx_integration_test.go`, `internal/httpserver/kyc_round3_http_test.go` | assume a synchronous submit/create | deliberate updates that drive the worker; IC condition 2 ("ambiguous leaves status unchanged") is still asserted, never weakened |
| `migrate verify` | checksum of 0114 recorded; gap-free 0113→0114 | `migrate up` then `migrate verify` OK; down→up round trip on scratch |

---

## 6. `deploy/init-app-role.sql` (append-only, Rule 4)

Append one block after K2's, in K2's loop pattern:

```sql
-- PRH-2 E1 (migration 0114; ADR 0106): least-privilege, re-asserted on every run, mirroring
-- migration 0114's own grant block. RLS (FORCE) is the binding control.
--   kyc_submission_outbox - SELECT/INSERT/UPDATE (phase A insert; the claim and the post-claim
--                           transitions, all trigger-guarded). Never DELETE or TRUNCATE.
DO $$
DECLARE t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES ('kyc_submission_outbox', 'SELECT, INSERT, UPDATE')) AS v(name, privs)
    LOOP
        IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = t.name) THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t.name);
            EXECUTE format('GRANT %s ON %I TO igaming_runtime', t.privs, t.name);
        END IF;
    END LOOP;
END
$$;
```

No role, password or attribute change. K3 appends its own block **after** this one (textual conflict
only; resolved by merge order E1 → K3).

---

## 7. Code shape, configuration and wiring

### 7.1 Go (internal/kyc)

- `internal/kyc/outbox.go` (new): the row model, `enqueueCreate(ctx, tx, v)` and
  `enqueueSubmit(ctx, tx, v, docs)` (phase A helpers taking the caller's tenant `tx`), the error
  classes, and the pure `backoff(n)` function.
- `internal/kyc/outbox_worker.go` (new): `OutboxWorker{Pool, Providers (the Orchestrator's adapter
  lookup by provider id), Outbound, Clock/ticker, constants...}`, `ValidateForLoop()`,
  `RunOnce(ctx) Stats`, `RunOutboxWorkerLoop(ctx, w, logger, interval)` (one pass immediately, then per
  tick; per-pass and per-item panic recovery; shutdown drain like §37.4), `claimNext` (the only
  `db.ServiceKYCSubmissionWorker` reference), `prepare`, `executeCreate` / `executeSubmit` (phase B,
  the former inline bodies), `runPhaseC` (the `alerting.InTx` owner) and
  `raiseSubmissionFailedTerminal`.
- `verification_service.go`: `CreateVerification` is replaced by `RequestVerification(ctx, tx,
  params, providerID) (Verification, error)` (phase A only). `applyCreateVerificationResult` is reused
  unchanged by phase C.
- `document_service.go`: `UploadDocument` enqueues the `submit` row in its own tx. `SubmitVerification`
  is removed as an exported entry point. The orphan guard in `UploadDocument` (N5) is relaxed in exactly
  one way: an orphan verification **with a live (`pending`/`claimed`) `create` row** accepts uploads
  (the `submit` row waits for the create, §2.7); an orphan without one still returns
  `ErrVerificationNotSubmitted` (409). `applySubmissionResult` is reused unchanged.
- **Static test:** no non-test code outside `internal/kyc/outbox_worker.go` calls a `KYCProvider`'s
  `CreateVerification` or `SubmitVerification` (one supervised outbound path).

### 7.2 HTTP

- `kyc_handlers.go`: the create handler runs `SelectProvider` + `RequestVerification` in one
  `WithTenant` tx and returns **201** with the `unverified` verification. A missing or ambiguous
  provider configuration still returns 503 before any row is written. Vendor unavailability no longer
  reaches the HTTP path. The upload handler no longer calls the vendor; its responses are unchanged.
- **Player-visible behaviour change:** after create, the verification reads `unverified` (no
  reference) until the worker sends it; after an upload, the vendor submission happens asynchronously.
  If the OpenAPI description documents synchronous semantics it is updated in E1. QUESTION Q-R2 asks
  whether product wants an explicit `submission_state` field (not added by default).

### 7.3 Configuration

- `internal/config/config.go`: `KYCOutboxInterval time.Duration`, env
  **`KYC_OUTBOX_INTERVAL_SECONDS`** (positive integer; RECOMMENDATION default **5 s**; validated like
  `PAYMENTS_SWEEP_INTERVAL_SECONDS`). Nothing else is configurable by env in E1. The §2.8 constants
  are code RECOMMENDATIONs.
- `docs/runbooks/production-configuration-checklist.md`: one row for the new variable.

### 7.4 `cmd/platform-api` wiring

- `registrations.go`: `buildKYCOutboxWorker(pool, kycOrchestrator, kycOutboundCredentials)`, mirroring
  `buildPaymentsSweeper`.
- `main.go`: one delimited start block **after** the I-wire dispatcher block and after the
  synthetic-component guard and DB connection, plus one delimited shutdown-drain block after the
  I-wire drain block:
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
  The H and I-wire blocks are not edited. If `main_construction_ast_test.go` or
  `run_order_ast_test.go` pin goroutine order or construction sites, they get one new entry each.
  New `cmd/platform-api/kyc_outbox_wiring_test.go` mirrors `sweeper_wiring_test.go`.

### 7.5 Metrics (no tenant, provider or row labels; ADR 0095 §37.4 precedent)

`kyc_outbox_passes_total`, `kyc_outbox_items_total{result}` (`sent|retry|failed_terminal|cancelled|
deferred|claim_lost|phase_c_failed`), `kyc_outbox_last_pass_unix_seconds` (a stalled worker stops
advancing it; nothing alerts on it, ALERT-DELIVERY-1 OPEN), `kyc_outbox_oldest_due_age_seconds`.

---

## 8. File ownership (so K3 can run in parallel)

### 8.1 E1-owned (no other active workstream touches these)

- New: `migrations/0114_kyc_submission_outbox.{up,down}.sql`; `internal/kyc/outbox.go`;
  `internal/kyc/outbox_worker.go`; `internal/kyc/outbox_*_test.go`;
  `internal/kyc/migration_0114_integration_test.go`;
  `internal/alerting/migration_0114_kyc_kind_integration_test.go`;
  `internal/db/kyc_worker_identity_integration_test.go`; `cmd/platform-api/kyc_outbox_wiring_test.go`.
- Edited: `internal/kyc/{verification_service,document_service,provider}.go`;
  `internal/httpserver/kyc_handlers.go`; KYC tests (`kyc_two_phase_integration_test.go`,
  `recording_tx_integration_test.go`, `internal/httpserver/kyc_round3_http_test.go` and any other test
  calling the removed entry points); `internal/db/platform_service.go` (no other WS touches it).

### 8.2 Shared files (serialize; E1 merges before K3)

| File | E1 change | Known other writer | Conflict kind |
|---|---|---|---|
| `deploy/init-app-role.sql` | append §6 block | K3 appends | textual; E1 first |
| `cmd/platform-api/main.go` | two delimited blocks (§7.4) | none active (H, I-wire merged) | none expected |
| `cmd/platform-api/registrations.go` | `buildKYCOutboxWorker` | none active | none expected |
| `cmd/platform-api/{main_construction,run_order}_ast_test.go` | conditional one-line pins | none active | none expected |
| `internal/config/config.go` | `KYCOutboxInterval` | none active (H merged) | none expected |
| `internal/alerting/kind.go` | one constant + one `kindDefs` entry | K3 only if it adds Kinds | textual append |
| `internal/alerting/static_wiring_test.go` | one helper + one owner entry | K3 if it adds `RaiseGuarded` sites | textual append |
| `docs/decisions/0095-...md` | §38 + two pointer lines | K3 (§4.8, §4.3, §28.9, INV-IO-7) | different sections; **K3 numbers any new section §39+** |
| `docs/runbooks/operational-runbooks.md` | new §13 "KYC outbox stuck" | K3 runbooks | append; K3 takes §14+ |
| `docs/runbooks/production-configuration-checklist.md` | one row | — | none expected |

E1 touches no `internal/payments`, `internal/ledger`, `internal/withdrawal`, `internal/reconciliation`,
`internal/providerref`, `internal/capability`, `internal/adjustment` or `backoffice` file, and no
`enforcement.go`. K3 touches no `internal/kyc` file. The migrations are disjoint objects.

### 8.3 Runbook entry "KYC outbox stuck" (DoD; `docs/runbooks/operational-runbooks.md` §13)

Content the implementer writes (no SQL edits of the table by hand; FORCE RLS and the guard trigger
forbid it, and they are not to be lifted):
- **Signals:** `kyc_outbox_last_pass_unix_seconds` not advancing; `kyc_outbox_oldest_due_age_seconds`
  growing; open `kyc.submission_failed_terminal` alerts (durable, unrouted); players reporting
  "verification stuck in unverified".
- **Diagnose:** worker running? (`main` logs, the gauge); vendor or credential failure (error class
  counts, `last_error_class` on rows via a tenant-scoped read); tenant inactive (`deferred_tenant_inactive`);
  claims repeatedly expiring (`lease_expired`: phase C failing, DB health).
- **Remedy:** fix the cause; `pending` rows resume automatically. `failed_terminal` rows are never
  requeued by hand: the remedy is a new upload by the player (a new or the same content may be
  enqueued again because the live-key index excludes terminal rows) or a new verification. A staff
  requeue action does not exist (follow-up KYC-OUTBOX-REQUEUE-1). Resolve the alert through
  `POST /v1/admin/alerts/{id}/resolve` (`alert:manage`) with a reason code once the cause is fixed.
- **Never:** lift RLS, disable the trigger, `DELETE` rows, or set a verification status to make a row
  "go away".

---

## 9. Threats and residuals

| # | Threat / residual | Mitigation in E1 | Remaining |
|---|---|---|---|
| R1 | Service session inherits the NULL-tenant (`WithoutTenant`) exposure of legacy dual-scope policies | one-statement confinement (static); differential probe proves worker visibility = `WithoutTenant` visibility + outbox | pre-existing for all three identities; Q-S1 |
| R2 | Spoofing the GUC by a raw `set_config('app.platform_service_id', ...)` elsewhere | allowlist in `WithPlatformService`; new static test: no non-test file outside `internal/db` contains `set_config('app.platform_service_id'` | — |
| R3 | **Vendor idempotency is PROVIDER DEPENDENT.** A re-claim after a lost phase C or an expired lease resends with the same `kv:`/`ks:` key. A vendor that ignores keys creates a second vendor verification (create) or a duplicate submission (submit); for create, the first vendor reference is never bound and its callbacks 503 until the vendor stops (the F2 gap narrowed, not closed). | same key on every resend; bounded by `MaxFailedAttempts` | **vendor-intake hard requirement:** idempotency-key support, or a merchant-reference (`ExternalReference = verification id`) lookup/echo the platform can bind on. Recorded in ADR 0095 §38. |
| R4 | Head-of-line: one sequential worker; a slow vendor for one tenant delays every tenant | one row per claim; per-pass per-tenant cap; call timeout 10 s | concurrency caps are a follow-up (as §37.5 H-SEC-2) |
| R5 | No delivery: `failed_terminal` alerts are durable but unrouted | — | ALERT-DELIVERY-1 OPEN; HD-PRH2-4-OPS |
| R6 | A stalled worker cannot alert about itself | gauge + runbook | ALERT-DELIVERY-1 / a future watchdog |
| R7 | Unbounded growth (DELETE refused) | — | retention is HQ-E1-4 |
| R8 | Player-visible asynchrony (create shows `unverified`; upload no longer reports vendor acceptance) | 201 + existing status vocabulary; upload allowed while create is pending | product confirmation Q-R2 |
| R9 | Non-active tenant rows wait indefinitely | deferral without consuming attempts | HQ-E1-1 |
| R10 | `failed_terminal` has no staff requeue | player re-upload / new verification; live-key index allows re-enqueue | follow-up KYC-OUTBOX-REQUEUE-1 |
| R11 | Lost-claim results discarded | winner applies its own (same key) | duplicate vendor call when the loser's call already reached the vendor; same as R3 |
| R12 | Callback for a reference whose create phase C has not landed | existing retryable 503 (IC-Q1) unchanged | as today, bounded by R3 |

---

## 10. Test plan

Legend as plan §5: R, ADV, CON, TI, AZ, FL, IDM, RB, AU, RLS, MIG, MUT. All DB tests run on a
HEAD-migrated scratch DB under the runtime role. T-1..T-4 apply (no sleep, no new wall-clock
assertion, `pipefail` + FAIL-grep, no skip). Local runs are never labelled CI.

### 10.1 State machine and outcomes (R, FL, IDM)

1. Create happy path: handler → 201 `unverified`; worker pass → reference bound, status from the
   vendor, outbox `sent`, audits `kyc.submission_enqueued` + `kyc.verification_submitted`.
2. Submit happy path: upload → outbox `pending`; worker → `sent`; vendor saw exactly the pinned ids
   and the pinned key.
3. **Ambiguous (each of ProviderError, transport error, timeout, empty create reference):** status
   unchanged (**IC condition 2 asserted**), row `pending`, `failed_attempts+1`, `next_attempt_at` in
   the future, audit `kyc.submission_retry_scheduled`.
4. **Not-sent (credential missing, binding mismatch, adapter unregistered):** same as 3 with class
   `not_sent`; **no** provider call recorded by the mock.
5. **Exhaustion:** fixture `failed_attempts = Max-1` + ambiguous → `failed_terminal`, alert row with
   subject tenant and the §4.1 discriminator, occurrence `raised_by_scope='tenant'`,
   `kyc.submission_failed_terminal` audit; verification status byte-identical to before.
6. **IC F3 (DoD):** with a `create` row `pending`, and again `claimed`, `EvaluateEnforcement` for an
   already-approved player returns the approved outcome (the orphan is excluded); with only orphan rows,
   the result equals "no verification" (unchanged rule). With a `create` row `failed_terminal`, the
   same. **A vendor outage never produces `failed`**: drive 100 % ambiguous until `failed_terminal` and
   assert no `kyc_verifications` UPDATE happened (row `updated_at` and full row image unchanged).
7. Prepare decisions: each §2.9 row (terminal verification, superseded, orphan after failed create,
   empty set, set changed by staff rejection → cancelled + re-enqueued with the new key, tenant
   inactive → deferred without attempt consumption, exhausted re-claim → `failed_terminal` with no
   provider call).
8. Backoff function: exhaustive unit table including the cap; deadline computed by the DB.

### 10.2 Crash and concurrency (CON, RB, FL)

9. **Crash between A and B:** phase A committed, worker never ran → row `pending`, verification still
   orphan/unchanged; next pass sends it.
10. **Crash between B and C:** failure-injecting mock records the call then the worker aborts before C
    → row stays `claimed`; fixture `lease_expires_at` into the past → re-claim (`claims=2`,
    `failed_attempts=1`, `lease_expired`) → the mock receives the **same idempotency key** twice →
    `sent` once; exactly one `kyc.verification_submitted*` success row.
11. **Phase C DB failure** (injected): bounded phase-C retry, then the row stays `claimed`, then
    test 10's recovery.
12. **Two workers** (`-race -count=50`): N due rows, two `RunOnce` concurrently → each row claimed
    exactly once per lease, each vendor call once, every row terminal, no duplicate success audit.
13. **Lost claim:** worker A claims; lease fixture expires; worker B re-claims and finishes; A's phase C
    then affects 0 rows, rolls back, and A's result is not applied (verification unchanged by A).
14. **Duplicate submit:** two uploads producing the same set (and a replayed upload) → one live row
    (ON CONFLICT DO NOTHING), audit `duplicate=true`; after `failed_terminal`, the same content can be
    enqueued again.
15. Create/submit ordering: a `submit` row is not claimable while its verification's `create` row is
    `pending`/`claimed` (claim SQL and trigger backstop), and becomes claimable when `sent`.
16. Concurrent staff `ReviewVerification` or verified callback during phase B → phase C never
    overwrites or demotes (the existing forward-only CAS; regression).

### 10.3 Tenant isolation and RLS (TI, RLS)

17. Tenant B session: SELECT/UPDATE of tenant A rows sees and affects 0 rows; INSERT with A's
    `tenant_id` refused (`WITH CHECK`).
18. **Positive-then-negative exclusion matrix** for every policy: player, platform-admin, staff
    principal (read yes / write no), acting (`WithPlatformActingInTenant`), `alert_dispatcher`,
    `sportsbook_catalogue_sync`, mixed service+tenant: each sees/writes nothing it should not.
19. A tenant session cannot claim (`pending → claimed` refused by RLS `WITH CHECK` and the trigger),
    cannot touch a `pending` row, cannot modify immutable columns or terminal rows, cannot DELETE or
    TRUNCATE (refused for every session including the owner).
20. Insert validation: wrong provider, wrong key, unsorted/duplicate ids, a rejected document, a
    foreign-verification document, `create` on a non-orphan → each refused with its message.
21. Go/SQL key parity: random sets → `submissionIdempotencyKey` = the trigger's recomputation.

### 10.4 Worker identity least privilege (AZ, ADV)

22. **T-ID-1 positive:** the worker session sees all tenants' outbox rows and can claim a due row; the
    trigger forces `claimed_by_service`, `claim_token`, `claimed_at`, `claims`.
23. **T-ID-2 negative writes:** under `WithPlatformService(ServiceKYCSubmissionWorker)`, each of these
    is refused or affects 0 rows: INSERT/UPDATE `kyc_verifications`, `kyc_documents`; INSERT
    `ledger_transactions`/`ledger_entries`; UPDATE `payment_attempts`, `withdrawal_requests`;
    INSERT `staff_users`, `staff_capability_grants`, `ledger_adjustment_requests`; INSERT a
    tenant-owned `audit_log` row; INSERT `alerts`/`alert_occurrences` for the KYC Kind (and the
    `alerting_session_scope()` refusal message is asserted); `alert_deliveries`; a non-claim outbox
    transition; an outbox INSERT.
24. **T-ID-3 differential visibility probe** (the K2-G1 pattern): enumerate every public table; for
    each, `count(*)` under the worker session must equal `count(*)` under a `WithoutTenant` session,
    except `kyc_submission_outbox` (worker > 0, `WithoutTenant` = 0). Non-vacuity: seeded tenant rows
    exist in KYC, ledger and payment tables.
25. Static: `db.ServiceKYCSubmissionWorker` is referenced only in `internal/db/platform_service.go` and
    `internal/kyc/outbox_worker.go:claimNext`; the `WithPlatformService` closure there executes exactly
    one statement whose SQL text references only `kyc_submission_outbox`; no non-test file outside
    `internal/db` calls `set_config('app.platform_service_id'`; no KYC provider outbound call outside
    `outbox_worker.go`.
26. `WithPlatformService` with an unknown string still fails before any transaction (regression).

### 10.5 Audit and alerting (AU)

27. Every transition writes exactly one `kyc.submission_*` row with the §3.5 shape; metadata contains no
    reference, credential, document content or raw error (a redaction test feeds a vendor error
    containing a secret-shaped string and greps the row).
28. Alert dedup: 50 rows failing for the same tenant/operation/provider → one open alert, 50
    occurrences; another tenant → a separate alert; the subject tenant can read it, cannot ack it;
    another tenant cannot see it.
29. Alert raise failure (inject a swallowable SQLSTATE in the raise savepoint) → `failed_terminal`
    still commits; `Flush` performs the detached retry in tenant scope.
30. Kind parity test (§4.3), including `allowed_keys`, `raise_mode` and the reverse direction.
31. Discriminator closed set: a provider id outside the charset → `<op>:provider_unclassified`.

### 10.6 Migration (MIG)

32. Up on scratch → the exact policy set on `kyc_submission_outbox` is
    `{(kso_tenant_select, SELECT), (kso_tenant_insert, INSERT), (kso_tenant_update, UPDATE),
    (kso_kyc_worker_select, SELECT), (kso_kyc_worker_claim, UPDATE)}`; `relrowsecurity` and
    `relforcerowsecurity` true; `alert_kinds` policies still exactly `{alert_kinds_read_all,
    alerts_platform_service_dispatcher}` (the temporary seed policy is gone); `alert_kinds` FORCE still
    on; runtime grants exactly `SELECT, INSERT, UPDATE`.
33. Down on an empty scratch DB succeeds; up again succeeds (round trip); `migrate verify` OK.
34. Down refuses with a `pending` row and with a `claimed` row; succeeds with only terminal rows.
35. Down refuses (legible message) when an alert of the KYC Kind exists; `alert_kinds` FORCE RLS and
    the deny trigger are intact after the refused down.
36. The jurisdiction 0075/0077 full-chain rollback tests pass unchanged; the 0077 whitelist stays 11.
37. Standalone-psql atomicity: run the up file's seed `DO` block with an injected failure after the
    `CREATE POLICY` (test-only variant) → no policy remains.

### 10.7 Wiring

38. `kyc_outbox_wiring_test.go`: the production builder yields a worker that passes `ValidateForLoop`;
    a nil pool/outbound/provider lookup is refused; the loop runs a first pass immediately, stops on
    context cancel, recovers a panicking item; `KYC_OUTBOX_INTERVAL_SECONDS` parsing (positive only).
39. A runtime probe (the H precedent): no `idle in transaction` session exists while the mock is
    inside `CreateVerification`/`SubmitVerification` on the worker path, with a control that the probe
    does see a deliberately open transaction.

### 10.8 Mutation targets (each must be killed; evidence file
`docs/plans/payment-readiness/evidence/prh2-e1-mutation-kill.txt`)

| # | Mutant | Killed by |
|---|---|---|
| M1 | drop `claim_token = $2` from the phase-C CAS | 13 |
| M2 | drop `claim_token` from the prepare re-select | 13 |
| M3 | remove `SKIP LOCKED` (and, separately, `FOR UPDATE`) from the claim | 12 |
| M4 | claim predicate ignores `next_attempt_at` / `lease_expires_at` | 9, 10 |
| M5 | map an ambiguous result through `statusForOutcome` (status inferred) | 3, 6 |
| M6 | `failed_terminal` also writes a verification status | 5, 6 |
| M7 | phase B inside the phase-P or phase-C closure | static + 39 |
| M8 | trigger allows `pending → claimed` from a tenant session | 19 |
| M9 | worker policy `WITH CHECK` admits `state = 'sent'` | 23 |
| M10 | `kso_kyc_worker_select` loses `app.tenant_id IS NULL` (mixed session) | 18 |
| M11 | tenant policies lose the acting/service exclusions | 18 |
| M12 | lease re-claim does not count `failed_attempts` | 10 |
| M13 | deferral consumes an attempt | 7 |
| M14 | insert trigger trusts the Go key | 20, 21 |
| M15 | live-key unique index made unconditional (blocks re-enqueue after `failed_terminal`) | 14 |
| M16 | `submit` claimable while its `create` is live | 15 |
| M17 | alert raised post-commit instead of in-tx / `Flush` skipped | 5, 29 |
| M18 | alert raised under the worker identity | 23 |
| M19 | discriminator built from raw provider id | 31 |
| M20 | down's refusal predicate removed | 34 |
| M21 | `db.ServiceKYCSubmissionWorker` used outside `claimNext` | 25 |

---

## 11. Reviewers and Definition of Done

### 11.1 Reviewers

- **security (hard gate):** the new identity (§3), the five policies, the guard trigger, the
  temporary seed/unseed policy mechanism (§4.4, Q-S2), the residual R1 (Q-S1), the audit actor (Q-S3),
  the alert payload allowlist, and the vendor-intake requirement (R3).
- **architect:** cross-domain boundary (identity, alerting, KYC, wiring), ADR 0095 §38 consistency.
- **code-reviewer:** independent review; the removed entry points; the static tests.
- **qa:** §10, T-1..T-4, the W3 checklist, mutation evidence.
- **ledger-finance:** not required. E1 must show it touches no money: a static check that no file in
  the E1 diff imports `internal/ledger`, `internal/payments`, `internal/withdrawal` or
  `internal/adjustment`, plus T-ID-2's ledger write denials. If any of those appears in the diff, LF
  becomes a required reviewer.
- **identity-compliance:** owner; closes IC F3.
- **devops:** loop wiring, config, metrics.

### 11.2 Definition of Done

1. Migration 0114 up/down + init-app-role block, all §10 tests green locally with `-race`, mutation
   evidence file, `migrate verify` OK. Local runs are reported as local, not CI.
2. ADR 0095 §38 (this design's amendment, already drafted) updated to an implementation record with
   the labels below; the pointer lines under §15.2/§15.3 stay.
3. This ADR moves to ACCEPTED after the reviews, with an implementation record section.
4. Runbook §13 "KYC outbox stuck" (§8.3), the production-config checklist row.
5. Registry and HANDOVER wording (proposed in the hand-off; orchestrator-only files).
6. **Labels at merge (expected):** outbox, worker, identity, Kind, raise path: `IMPLEMENTED` against
   the MOCK KYC provider; real KYC vendor `PROVIDER DEPENDENT` (R3 intake requirement); alert delivery
   `NOT IMPLEMENTED` (ALERT-DELIVERY-1 OPEN); staff requeue `NOT IMPLEMENTED` (follow-up); retention
   `NOT IMPLEMENTED` (HQ-E1-4).

---

## 12. Human decisions needed (not decided here)

- **HQ-E1-1 — suspended/closed tenants.** Default in this design: no vendor call for a non-active
  tenant; rows are deferred indefinitely without consuming attempts (mirrors ADR 0095 §37.3). Should
  pending KYC submissions of a **closed** tenant instead be cancelled, and is sending a player's KYC
  submission to a vendor on behalf of a suspended tenant ever permitted? This is a data-processing and
  contractual question, not an engineering one.
- **HQ-E1-2 — severity** of `kyc.submission_failed_terminal`: proposed `p2` (no money effect). Confirm
  or raise to `p1` (for example if a jurisdiction treats a stalled KYC as a compliance incident).
- **HQ-E1-3 — retry budget meaning.** The §2.8 defaults are engineering RECOMMENDATIONs. Does any
  jurisdiction impose a KYC submission or completion deadline that should bound
  `MaxFailedAttempts × backoff`? No such rule is invented here.
- **HQ-E1-4 — retention** of `kyc_submission_outbox` rows (ids and states only, no PII) and whether
  terminal rows may ever be deleted. No period is invented; until decided, rows are kept.
- Recipients and on-call for the new alert are already covered by **HD-PRH2-4-OPS**; no new decision.

## 13. Open questions for reviewers

- **Q-S1 (security):** accept R1 (the worker session has `WithoutTenant`-equivalent NULL-tenant
  visibility, like the two existing identities), or require a follow-up that adds
  `app.platform_service_id IS NULL` to the legacy dual-scope arms (`audit_log` and others)?
- **Q-S2 (security):** approve the temporary `TO CURRENT_USER`, Kind-scoped seed/unseed policy inside
  one `DO` block as the sanctioned way to change `alert_kinds` after 0110?
- **Q-S3 (security):** `ActorType=system` + `metadata.platform_service` (default) or `ActorType=service`
  + a fixed UUIDv5 actor id?
- **Q-S4 (security):** is the claim token in the `WHERE` CAS sufficient (the payments precedent), or
  should phase C also present the token through a transaction-local GUC the trigger checks?
- **Q-R1 (identity-compliance):** confirm the one relaxation of the N5 guard (upload accepted on an
  orphan whose `create` row is live).
- **Q-R2 (product-owner-proxy):** is 201 + `unverified` an acceptable player-API change, or is an
  explicit `submission_state` field wanted (not added by default; scope rule)?
- **Q-R3 (code-reviewer):** removing the exported `CreateVerification`/`SubmitVerification` (one
  supervised outbound path) versus keeping them as thin wrappers; this design removes them.
- **Q-R4 (architect/code-reviewer):** the child-before-parent lock order in P and C (§2.6) against
  ADR 0082 R8, given no path locks a verification and then an outbox row.
- **Q-R5 (qa/security):** down refuses only on non-terminal rows (plan §4) or on any row?
- **Q-R6 (qa):** is fixture-driven time (DB `now()` + fixtures + injectable ticker) acceptable as the
  T-1 mechanism for this table, instead of an injected Go clock compared against the DB?
