# ADR 0111 — Payout destination binding (B13), governed resolution of unbound payouts (PAY-PAYOUT-UNBOUND-RESOLVE-1) and four-eyes release of frozen `approved` holds (HSEC-APPROVED-HOLD-RELEASE-1)

- **Status: PROPOSED — DESIGN ONLY, revision 2 (`architect`, 2026-10-08).** Revision 1 (`30cec95`/`978a58b`) was
  reviewed: `ledger-finance` APPROVE WITH CONDITIONS for §2 (B13) and §6 (HSEC), **§4 (RESOLVE-1) REJECTED AS
  WRITTEN** until C-1..C-7; `security` APPROVE WITH CONDITIONS. Revision 2 writes every condition into the text;
  §11 maps each one to the section changed. **§4 needs a `ledger-finance` re-review before any RESOLVE-1 code**
  (except the pure tightenings listed in §7.3). No code, no migration and no registry change is made by this ADR;
  every deliverable here is `NOT IMPLEMENTED`.
- **Authority.** The owner decisions recorded verbatim in ADR 0095 §44 (decisions 1-8 B13, 9-12 RESOLVE-1, 13-18
  HSEC; 2026-10-08) are **authoritative** and are not broadened. Open sub-questions are resolved as numbered
  AMBIGUITIES (§9, safest reading); where review conditions conflicted, revision 2 took the safer reading and says so
  in §11. Deviations from earlier rulings that the human should see are listed as D-1..D-9 (§10.2); genuine policy
  questions are in §10.1; launch-blocking flags in §10.3.
- **Inputs.** ADR 0095 §35.2, §35.4, §35.6, §42, §43, §44; ADR 0101 (K3, R-K3-8, §6, §24); ADR 0107 (CT-PRE);
  ADR 0110 (0120); ADR 0007/0008; ADR 0096/0098; the B13 and HSEC decision briefs; code and migrations 0100-0121 at
  `dd6cd31`; the revision-1 reviews of `ledger-finance` (H-1..H-4, M-1..M-4, L-1..L-7, C-1..C-7) and `security`
  (H-1..H-4, M-1..M-10, L-1..L-9).
- **Amends (when accepted):** ADR 0095 (§4 payout cells, §9.3 S95-C10 clarification, §35 statement-import trust),
  ADR 0101 (new kinds M4; §6.4 acting policy on statement tables reversed; MR041 per kind), ADR 0102 statement
  policies (0102 INSERT arms narrowed), ADR 0107 (CT-PRE table/capability subsumed by §6), ADR 0110 (§3 table list
  9 → 11; K3 digest; §10a T9-T11).
- **Owners.** `architect` (this ADR). Implementers: `payments` (all three), `identity-compliance` (B13 verification
  sources). Binding rulings: `ledger-finance` (financial invariants), `security` (security requirements).
- **Migration numbers (revision 2 swaps 0124/0125, §7.2):** 0122 = PAY-RECEIPT-ANOMALY-APPLIED-1 (another agent);
  **0123 = B13**; **0124 = HSEC hold release**; **0125 = UNBOUND-RESOLVE-1** (+ the B13 not-paid resolution). The
  swap keeps HSEC (approved with conditions) from waiting on RESOLVE-1 (rejected as written).

---

## 1. Shared principles

1. **Reuse, not parallel systems.** Identity = `persons` / `player_accounts` / `staff_users.person_id`. Four-eyes =
   ADR 0099/0100/0101 machinery (capability-specific in-force grants, `financial_policy_required_approvals`, LF-11
   distinct non-NULL Persons, S-12 beneficiary exclusion, S-2(iii), DB recount at `pending → executing`, execution
   in the final approval's transaction, acting fences). Proof of actor = ADR 0110 `zz_actor_proof_guard` +
   `internal/actorproof.Issuer`. Audit = `audit_log` (+ ADR 0104 projection). Alerts = B12 `raisePayoutDisputeAlert`
   on Kind `payment.webhook_integrity`, raised as the last statement. Canonical encodings = the `k2_canonical`
   length-prefixed form and `actorproof` timestamp formatting.
2. **`withdrawal_approvals` is NOT reused** for any new four-eyes step: its `UNIQUE (withdrawal_request_id,
   approver_principal_id)` collides with the original approvers; it is threshold-based; it has no acting family;
   it is not proof-bound (ADR 0110 §10a T8).
3. **No automatic money movement.** Only an executed four-eyes resolution, in the transaction of its final approval,
   through `ledger.Post` with deterministic keys, moves money. No balance UPDATE.
4. **Fail closed.** Missing, ambiguous, expired, unsealed or unverifiable input refuses, keeps funds where they are,
   writes an audit row and, for payment-integrity signals, raises the B12 durable P1.
5. **Labels.** MOCK/synthetic behaviour is `MOCK`; real-provider behaviour is `PROVIDER DEPENDENT`.

---

## 2. B13 — provider-neutral payout instrument and destination binding (migration 0123)

### 2.1 Entity model

**`payout_instrument_kinds`** (reference, seeded by migration; **SELECT-only for `igaming_runtime`, pinned by a
grant test**; no write policy): `code TEXT PK CHECK (code ~ '^[a-z][a-z0-9_]{1,31}$')`, `detail_schema_version INT`,
`allowed_rails TEXT[]`, `non_synthetic_enabled BOOLEAN`. Seed: `bank_account` (true), `card_token` (true),
`ewallet_account` (true), `crypto_address` (**false**, ADR 0008 / HD-R15-4), `synthetic_test` (false). The set is
open: a new regulated instrument is a new row plus a Go validator, never a code path keyed on one kind (decision 6).

**`payout_instruments`** (tenant-owned, ENABLE + FORCE RLS):

| Column | Rule |
|---|---|
| `id` | DB default; a client value is refused |
| `tenant_id`, `brand_id`, `player_account_id` | composite FKs; brand = the player account's brand (trigger) |
| `person_id` | DB-forced from `player_accounts.person_id` |
| `kind`, `rail` | FK to kinds; `rail ∈ kind.allowed_rails` (trigger); `rail` replaces the staff `payment_method` |
| `asset_codes` | `TEXT[]`, each in the Asset registry (trigger) |
| `detail_ciphertext`, `detail_nonce`, `detail_key_kid` | **AEAD ciphertext** of the kind-specific JSON detail (§2.2); plaintext detail is never stored, never logged; plaintext ≤ 4096 bytes |
| `detail_schema_version` | copied from the kind at insert |
| `display_mask` | ≤ 64 bytes; computed in Go by fixed rules: IBAN/account = country code + last 4; e-wallet email = first character of the local part + `***@` + domain; crypto address = first 6 + last 4; card = network + last 4. The only detail-derived value any staff or player API returns |
| `fingerprint`, `fingerprint_kid` | `CHECK (fingerprint ~ '^[0-9a-f]{64}$')`; composite FK `(tenant_id, fingerprint_kid, fingerprint, person_id)` → `payout_instrument_fingerprint_owners`. **Never returned by any API**; audit metadata carries an 8-hex prefix only |
| `supersedes_instrument_id` | NULL or a same-player instrument |
| `instrument_seal`, `seal_kid` | §2.2 instrument seal, written by the registration path |
| `state` | `pending_verification`, `verified`, `verification_expired`, `suspended`, `revoked`, `rejected`, `superseded` |
| `current_verification_id` | FK |
| `created_at`, `state_changed_at` | `state_changed_at` DB-forced to `now()` on every state change |

UNIQUE `(tenant_id, player_account_id, fingerprint_kid, fingerprint) WHERE state IN ('pending_verification',
'verified','verification_expired','suspended')`.

**`payout_instrument_fingerprint_owners`** `PK (tenant_id, fingerprint_kid, fingerprint)`, `person_id NOT NULL`,
`UNIQUE (tenant_id, fingerprint_kid, fingerprint, person_id)` (target of the composite FK), append-only. The first
Person to register a fingerprint in a tenant owns it; another Person conflicts on the PK (race-free). Registration
computes the fingerprint under **every retained fingerprint kid** and checks each. Never deleted (A-2).

**`payout_instrument_verifications`** (append-only; UPDATE/DELETE/TRUNCATE denied): `id`, `tenant_id`,
`instrument_id`, `source`, `ownership_assertion`, `verifier_provider_id`, `verifier_reference_hash`, `outcome`
(`verified` | `rejected`), `verified_at`, `expires_at NOT NULL`, `verification_seal`, `seal_kid`, `created_txid`.

**`payout_instrument_blocking_events`** (new; append-only, UPDATE/DELETE/TRUNCATE denied in all sessions):
`id`, `tenant_id`, `instrument_id`, `event` (`suspend` | `revoke`), `actor_type` (`player` | `staff` | `provider`),
`actor_id`, `reason_code`, `occurred_at DEFAULT now()`, `event_seal`. Every suspension and revocation writes one row
in the same transaction as the state change. Provider revocation is **blocking-only**, authenticated (verified
verifier callback or poll) and written only inside `internal/payoutinstrument` (L-7).

**Instrument state machine** (all-sessions trigger; every transition audited `payout_instrument.<transition>`):
- `pending_verification → verified | rejected`; `verification_expired | suspended → verified`: **every `→ verified`
  requires `NEW.current_verification_id` to reference a verification row with `created_txid = txid_current()`,
  `instrument_id = NEW.id` and `outcome = 'verified'`** (no un-blocking by a bare UPDATE).
- `verified → verification_expired`: written **only by the expiry sweep**; gates never write expiry, they only
  refuse on `expires_at <= now()` (L-4).
- `verified | verification_expired → suspended` (compliance staff or provider; blocking direction; event row).
- any non-terminal `→ revoked` (player, provider, compliance; event row); `→ superseded` when a replacing version is
  verified.
- `revoked`, `rejected`, `superseded` are terminal **in every session**.
- No transition lets staff mark an instrument verified or edit its destination (decision 7).

**`withdrawal_requests`** gains `payout_instrument_id` and `payout_instrument_fingerprint` (composite FK, same
tenant), both added to `withdrawal_requests_enforce_immutable_fields()`. A BEFORE INSERT trigger requires both NOT
NULL, locks the instrument `FOR SHARE`, and requires it to belong to `NEW.player_account_id`, be `verified` with an
in-force verification, and list `NEW.asset_code`. Pre-0123 rows stay NULL (legacy, A-11).

**`payout_attempt_destination_snapshots`** (write-once, 1:1 with a payout attempt): `attempt_id` PK + composite FK
to `payment_attempts_id_tenant_key`; `tenant_id`, `withdrawal_request_id`, `instrument_id`, `kind`, `rail`,
`fingerprint`, `fingerprint_kid`, `verification_id`, `verification_source`, `ownership_assertion`, `verified_at`,
`verification_expires_at`, `display_mask`, `amount`, `asset_code`, `snapshot_seal`, `seal_kid`, `created_txid`.
UPDATE/DELETE/TRUNCATE denied in all sessions. A DEFERRABLE INITIALLY DEFERRED constraint trigger on payout
`payment_attempts` INSERT requires, whenever the withdrawal has a binding, a snapshot with `created_txid =
txid_current()`, `instrument_id`/`fingerprint` = the withdrawal's, and **`snapshot.amount = withdrawal.amount =
attempt.amount` and `snapshot.asset_code = withdrawal.asset_code = attempt.asset_code`**. A side table (not columns)
keeps `payment_attempts_guard()` unedited.

**Migration-time assertion (L-6, A-11).** 0123 up refuses if any non-terminal (`requested`, `pending_review`,
`approved`, `submitted`) pre-0123 withdrawal has a non-NULL `provider_id` outside the literal MOCK provider-id set;
a test pins that literal set equal to the ids of the `providerkind.Synthetic` payment adapters. A Go startup check
repeats it.

**RLS and GRANTs (least privilege, M-8).** FORCE RLS; no `FOR ALL`; no SECURITY DEFINER; DELETE/TRUNCATE denied.
- Tenant family (`tenant_id = app.tenant_id`, player GUC NULL): SELECT + INSERT on instruments, verifications,
  fingerprint owners, blocking events, snapshots; UPDATE on `payout_instruments` only, with a column-discipline
  trigger (only `state`, `state_changed_at` (forced), `current_verification_id`).
- Player family: SELECT own instruments only. Registration runs in a tenant-scope transaction with the player
  resolved from the authenticated session.
- Acting family: **SELECT on `payout_attempt_destination_snapshots` only** (the §4/§6 queues need nothing more).
- Grants: `SELECT, INSERT, UPDATE` on `payout_instruments`; `SELECT, INSERT` on verifications, owners, blocking
  events, snapshots; `SELECT` on kinds. Grant test pins each.
- **`payout_instrument_verification_max_age`** (`jurisdiction_id`, `max_age`): runtime role SELECT only; rows are
  written by the migration/owner role until a governed four-eyes writer exists (recorded under ADR 0110 T8).

### 2.2 Keys, canonical encoding, fingerprint, encryption and seals

THREAT-MODEL-ARBITRARY-SQL-1 = YES (ADR 0110). Staff proofs cannot cover system and vendor writes, so B13 adds a
Go-side seal using the ADR 0110 key-handling pattern.

- **Two independent key families with separate lifecycles (H-2, M-4):**
  - `PAYOUT_INSTRUMENT_FP_KEYS` / `..._FP_ACTIVE_KID`: the fingerprint key. It is long-lived; a new kid becomes
    active only after the **re-fingerprint job** (decrypt detail, compute the new-kid fingerprint, insert owner rows
    under the new kid, verify zero conflicts) has completed for every non-terminal instrument. Runbook required.
  - `PAYOUT_INSTRUMENT_KEYS` / `..._ACTIVE_KID`: the master for the seal and detail-encryption subkeys, derived by
    HKDF-SHA256 with distinct labels `b13-seal-v1` and `b13-detail-aead-v1`.
  - Both come from the secret store as `config.SecretValue`, are ≥ 32 bytes, are refused if equal to any JWT,
    actor-proof or webhook secret, are unique per environment, and are **never stored in the database**. Retired
    kids are verify/decrypt-only while any row references them.
  - Startup gate: uses `cfg.GuardEnvironment()` (a missing `APP_ENV` counts as production) **and** fires whenever
    any non-Synthetic payout adapter or instrument verifier is registered, in any environment. There is no
    in-binary random-key fallback outside the `integration` build tag (prooftest pattern).
- **Canonical encoding (M-1):** every MAC input is the `k2_canonical` length-prefixed encoding; UUIDs lowercase;
  timestamps via `actorproof.TS`; amounts canonical decimal strings; arrays sorted.
- **Fingerprint** = `HMAC(fp_key, canon("b13-fp-v1", tenant_id, kind, normalize_kind(detail)))`. `tenant_id` makes
  fingerprints non-comparable across tenants. For `card_token`, the PSP card fingerprint (when the PSP supplies one)
  is the normalised input, so a re-tokenised card conflicts (L-8).
- **Detail AEAD (H-3, built into 0123):** AES-256-GCM under the HKDF detail subkey, AAD = `canon(tenant_id,
  instrument_id, kind, detail_schema_version)`. Built in now, so no non-synthetic instrument is ever stored in
  plaintext.
- **Seals** (HMAC under the seal subkey):
  - **instrument seal:** `canon("inst", id, tenant_id, brand_id, player_account_id, person_id, kind, rail,
    asset_codes, fingerprint, fingerprint_kid, supersedes_instrument_id)`, written by the registration path. The
    verifier **refuses an unsealed or wrongly sealed `pending_verification` row** and recomputes the fingerprint
    from the decrypted detail before any vendor call (closes T11, SQL-planted rows).
  - **verification seal:** `canon("ver", id, tenant_id, instrument_id, fingerprint, source, ownership_assertion,
    outcome, verifier_provider_id, verified_at, expires_at)`.
  - **blocking-event seal:** `canon("blk", id, tenant_id, instrument_id, event, actor_type, occurred_at)`.
  - **snapshot seal:** `canon("snap", attempt_id, tenant_id, withdrawal_request_id, instrument_id, fingerprint,
    fingerprint_kid, kind, rail, verification_id, verification_source, ownership_assertion,
    verification_expires_at, amount, asset_code)`.
- **The gate rule (H-1, M-1), applied in Go at every gate (request creation, T1p, phase B, T2/T12):** refuse unless
  (1) all seals verify and the fingerprint recomputed from the decrypted detail equals the stored one; (2) the in-force
  verification is the **latest** `verified` row of the instrument and `expires_at > now()`; (3) no `revoke` event
  exists for the instrument; (4) no `suspend` event is later than that verification's `verified_at`; (5) relational
  consistency holds (instrument tenant/brand/player/person = the withdrawal's; snapshot = withdrawal = attempt for
  instrument, amount and asset); (6) `verifier_provider_id` names a **currently registered non-Synthetic verifier**
  whenever the payment adapter is non-Synthetic (M-3).
- Residuals: application-host compromise (ADR 0110 T6) unchanged. New threats are recorded in ADR 0110 §10a:
  **T9** payout-instrument writes are not actor-proof bound (mitigated by the seals and the gate rule);
  **T11** SQL-planted `pending_verification` rows (mitigated by the instrument seal checked by the verifier).

### 2.3 Immutability and versioning

Identity columns of an instrument are immutable; a destination change is a **new instrument row**
(`supersedes_instrument_id`) that must itself be verified; the old row becomes `superseded` only once the new one is
verified. **Superseding is refused while the old instrument is in use** (the A-9 rule, L-3). Verifications, blocking
events and snapshots are append-only or write-once, so historical payout evidence is reproducible.

### 2.4 Binding lifecycle

| Point | Rule |
|---|---|
| **Request creation** (`POST /v1/me/withdrawals`, `withdrawal.RequestWithdrawal`) | **Binding happens here** (A-1). The player selects `payout_instrument_id`. In the transaction of the hold and after the H-SEC-11 gate, the server locks the instrument `FOR SHARE` and applies the §2.2 gate rule plus: same tenant/brand/player, asset listed, kind/rail enabled. Missing → 409 `PAYOUT_INSTRUMENT_REQUIRED`; unusable → 409 `PAYOUT_INSTRUMENT_NOT_USABLE`. No row, no hold; the idempotency key is not consumed. **Replay** (`withdrawal.go:405-409`) also compares `payout_instrument_id`; a differing instrument returns `ErrIdempotencyKeyReused`. |
| **Approval** | Mechanism unchanged; approvers see `display_mask`, `kind`, `verification_source`, `verified_at`, so they four-eyes a fixed destination. |
| **T1p** (`ClaimForDispatch`) | Order: L1 `LockApprovedForSubmission` → H-SEC-11 gate → KYC gate (deny/unavailable unchanged) → route error → **destination gate** → `kyc.RecordDecision(allow)` → `MarkSubmittedPending` → `InsertSubmittingAttempt` → **snapshot insert** → audit. The destination gate locks the instrument `FOR SHARE` (revocation locks it `FOR UPDATE` and takes no withdrawal lock, so no cycle; LF records the ADR 0082 position). It applies the gate rule plus the tiering predicate (§2.5) on the routed adapter. A refusal commits **only** the denial audit (`withdrawal.submit.http`, `denied`, `denied_by_destination: <closed reason>`), no allow decision row; the request stays `approved`; 409 `PAYOUT_DESTINATION_NOT_USABLE`. |
| **Submit body** | `payment_method` is no longer an input: the rail is the instrument's. A differing body value → 400 `PAYMENT_METHOD_MISMATCH` (A-10). |
| **Phase B** (`payoutAdapterCall`) | Re-reads the instrument and verification rows (L-5) and applies the gate rule and the **tiering predicate on the exact adapter value about to be invoked** (M-3). `WithdrawRequest` is built only from the snapshot plus the decrypted immutable detail. Any failure → no provider call, the existing NotSent class (T5 → `created`); the next T2 gate escalates. |
| **T2 re-claim / T12 resend** | `destinationGateAndEscalate` runs next to `gateAndEscalateOnDeny` (after the resolution-only and kill-switch checks; LF95-C10(b)/(c) precedent) with the gate rule and the tiering predicate. The destination is never re-resolved; a resend carries the identical snapshot. Failure → T16 escalation with new closed reasons `destination_not_usable` / `destination_integrity_failure`, no resend, no release, B12 P1 last; an already-escalated attempt only reschedules. |
| **Polls / evidence on an existing attempt** | Not blocked by instrument state (LF95-C10(d)). |
| **Player revocation / supersession** | Refused (409 `PAYOUT_INSTRUMENT_IN_USE`) while a withdrawal in `requested`, `pending_review`, `approved` or `submitted` binds it (A-9). Provider or compliance revocation/suspension is never refused; its effect is the gates above (parks, never releases). |

### 2.5 Verification sources and tiering (MOCK / sandbox / real)

- `source` ∈ {`psp_account_verification`, `kyc_vendor_instrument_verification`, `custodian_address_verification`,
  `synthetic`}; `ownership_assertion` ∈ {`account_holder_matches_verified_identity`, `synthetic_asserted`}; CHECK
  `(source = 'synthetic') = (ownership_assertion = 'synthetic_asserted')`.
- Verification comes only through the provider-neutral **`PayoutInstrumentVerifier`** interface (`internal/
  payoutinstrument`). **`source` is forced by `internal/payoutinstrument` from the verifier's type marker**: a
  Synthetic verifier can only produce `synthetic`; a non-Synthetic verifier returning `synthetic` is refused (M-3).
  A non-synthetic verification also requires the Person's KYC status `verified` at verification time (adopted from
  L-9, fail-closed).
- **Tiering predicate**, keyed only on the Go type marker of the live adapter object (never on `provider_id` or
  configuration): a non-Synthetic payment adapter (sandbox or real) requires a non-NULL binding, a non-synthetic
  source, a `non_synthetic_enabled` kind, valid seals and a registered non-Synthetic verifier. A Synthetic adapter
  accepts any source and, for legacy rows only, a NULL binding. It runs at T1p, **in phase B on the exact adapter
  value**, and at T2/T12.
- Synthetic-marked types (payment adapters and verifiers) perform no external I/O and never wrap a real adapter
  (pinned by a static test). The MOCK verifier is registered with `RefuseSyntheticInProduction`.
- MOCK flexibility is confined to the verification source: instruments, binding, AEAD, snapshot and seals are
  mandatory in MOCK too (decision 8).
- `expires_at` = min(source expiry, `payout_instrument_verification_max_age` for the tenant's jurisdiction). With no
  config row, no non-synthetic verification can be recorded (A-6, HD-R15-1).

### 2.6 Callbacks and destination echo (decisions 3 and 5)

- **Callbacks, polls and statements can never set, replace or change a destination.** No receipt, phase-C, poll or
  reconciliation code writes any §2.1 table. Snapshots are UPDATE-denied in all sessions. A static test pins that the
  only snapshot writer is T1p and the only instrument writers are inside `internal/payoutinstrument`.
- **S95-C10 is retained.** No raw payer-identifying field enters `CallbackEvent`, `StatusResult`, `WithdrawResult`,
  `ReceiptEvidence` or a statement line. The only destination evidence is `DestinationEcho{Fingerprint, Kid}`,
  computed **inside the adapter** by an injected `DestinationFingerprinter` under the snapshot's `FingerprintKid`.
  The fingerprinter's interface is `Echo(kid, kind, vendorDestination) (string, error)`; it exposes no key, no seal
  and no other capability (M-2).
- Comparison sites: phase C sync, poll (`applyPayoutStatusEvidenceInTx`), any future payout receipt cell. The cells
  run after the existing reference and amount checks, under L1 withdrawal → attempt:

| Attempt state | Echo vs snapshot | Effect |
|---|---|---|
| non-terminal | equal, or absent and the manifest does not declare `EchoesDestinationFingerprint` | unchanged |
| non-terminal | absent although the manifest declares it, on a success | ambiguous (the poll decides); never a success |
| non-terminal | **different**, or unknown `Kid` | CAS → `disputed`, `terminal_reason = 'destination_mismatch'`; audit `payments.payout_destination_mismatch` (attempt, withdrawal, provider id, fingerprint prefix); B12 P1 last; no `Complete`, no release; hold kept |
| `succeeded`, `declined` | different | no state change; audit + P1 with signal reason `destination_mismatch_on_terminal_payout` |
| `disputed`, no executed M4 | any | no-op (replay idempotence) |
| `disputed` with an executed M4 | any success / contradiction | the post-resolution cells of §4.8 apply (**not** a no-op) |

- **Closed sets (no DB migration needed for the reasons).** `payment_attempts.terminal_reason` has only a length
  CHECK (`0101:121`). Go sets: `PayoutDisputeReasons()` + `"destination_mismatch": false`; `payoutSignalReasons` +
  `destination_mismatch_on_terminal_payout`, `success_after_m4_not_paid`, `contradiction_after_m4_paid`;
  `payoutEscalationReasons` + `destination_not_usable`, `destination_integrity_failure`; reconciliation
  `disputeReasonClasses` + `destination_mismatch` = bound-if-referenced. The C-5b pin, the B12 static raise-site pins
  and the source-order guard counts are updated deliberately.

### 2.7 `WithdrawRequest` extension (M-2)

```go
type WithdrawRequest struct {
    MerchantReference string
    Amount            int64
    AssetCode         string
    PaymentMethod     string            // = snapshot.Rail (server-derived), never staff input
    Destination       PayoutDestination // REQUIRED for a non-Synthetic adapter
}
type PayoutDestination struct {
    InstrumentID   uuid.UUID
    Kind           string          // open set
    Detail         json.RawMessage // decrypted, kind-validated; never a PAN
    FingerprintKid string          // the adapter computes its echo under this kid
}
// String, Format and LogValue are redacting: Detail is never logged or rendered.
// WithdrawResult and StatusResult gain: DestinationEcho *DestinationEcho // {Fingerprint, Kid}; optional
```

`OperationManifest` gains `EchoesDestinationFingerprint`. Conformance tests: a non-Synthetic adapter is never
called with an empty `Destination`; a differing vendor-reported destination yields a mismatch echo; `Detail`
never appears in any log output.

### 2.8 Exceptional resolution (decision 7)

There is **no** staff destination override and **no** rebind kind. The only exceptional staff resolution touching a
B13 condition is M4 "not paid" for a `destination_mismatch` park (§4). It requires positive evidence of non-payment,
releases the hold only to the player's own cash, and never redirects money. A confirmed misdirected payout is
retained (HD-R15-6). An `approved` withdrawal whose instrument becomes permanently unusable stays parked
(HD-R15-5, launch-blocking per §10.3).

### 2.9 API, permissions, abuse controls

- Player: `POST /v1/me/payout-instruments` (kind, rail, detail; starts verification), `GET` (masks only),
  `POST .../{id}/revoke`. Card details only as a PSP hosted-fields token. The validator refuses any Luhn-valid
  12-19-digit string in any field of any kind (L-8). Request bodies of these routes are never logged.
- **Fingerprint conflict (M-7):** the player gets the generic 409 `PAYOUT_INSTRUMENT_NOT_ACCEPTED` (the same
  response as other registration refusals, so there is no enumeration oracle). The specific reason goes to the
  audit row and a compliance alert. Registration is rate-limited per player (ADR 0097 admission pattern).
- Staff: instrument list/detail per player (`payout_instrument:read` → `finance`, `compliance`, `platform_admin`;
  masks only); `POST /v1/admin/payout-instruments/{id}/suspend` (`payout_instrument:suspend` → `compliance`; reason
  required; blocking event + audit). No staff create, verify, unsuspend or edit route. `permissions.ts` matches.

### 2.10 Tests and acceptance (B13)

All tests run as the runtime role (not superuser, not BYPASSRLS), in the real session shapes, asserting
`SUM(D)=SUM(C)` and projection = rebuild. They cover:
- (a) the submit body cannot influence the destination;
- (b) cross-player, cross-brand and cross-tenant instruments are refused; a tenant-B session sees zero rows;
- (c) unverified, expired, suspended, revoked or superseded instruments refuse at request (no row or hold) and at
  T1p (denial audit only, no attempt);
- (d) no destination change after approval (immutable columns; snapshot UPDATE denied in tenant, acting and system
  sessions);
- (e) fingerprint conflict across Persons is race-free and checked under every retained kid; the player-facing
  response is generic;
- (f) the Synthetic / non-Synthetic matrix at T1p, in phase B (adapter swapped between T1p and phase B) and at
  T2/T12;
- (g) SQL tamper of each sealed row (instrument, verification, blocking event, snapshot), an SQL `→ verified`
  without a same-tx verification, an SQL un-suspend, a planted unsealed `pending_verification` row, and deleting a
  blocking event (refused) each lead to no provider call / escalation / P1;
- (h) T2/T12 re-check;
- (i) every §2.6 echo cell, replay, concurrency, tenant isolation, alert attributes (`provider_id` only);
- (j) callbacks never write B13 tables;
- (k) instrument state machine, terminal states in every session, `state_changed_at` forced;
- (l) the AEAD round trip with the AAD bound (an instrument's ciphertext moved to another row fails);
- (m) mask rules;
- (n) the Luhn refusal;
- (o) replay with a different instrument is refused;
- (p) the L-6 migration assertion;
- (q) audit rows;
- (r) migration up/down/up, grant pins, mutation evidence.

---

## 3. What B13 does not change

No change to the M2 allow-list, to `withdrawal.Complete`/`Fail`, to deposits or to the kill-switch / H-SEC gates.
Return-to-source (D2) is not enforced (HD-R15-3).

---

## 4. PAY-PAYOUT-UNBOUND-RESOLVE-1 — evidence-backed four-eyes resolution (migration 0125) — revised for LF C-1..C-7

### 4.1 Scope (A-12)

A payout attempt in `disputed` whose `terminal_reason` is `invalid_provider_reference`,
`invalid_provider_reference:*` or `provider_reference_conflict`, **each only with `provider_reference IS NULL`**
(C-6), plus `destination_mismatch` for the not-paid direction only. Every other R-K3-8 reason keeps its hold
unchanged. Allocation is never a payout route.

### 4.2 Kinds on the existing K3 table

- `payment_manual_resolutions.kind` CHECK widened with **`m4_evidence_paid`** and **`m4_evidence_not_paid`**.
- Capability: the existing `payment_force_resolve:request|approve` (flagged L-5 / D-9: every in-force M2 grant
  therefore authorises M4).
- New columns:
  - `evidence_line_id` (composite FK to `payment_statement_lines`);
  - `evidence_reference` (R; reserved-prefix CHECK);
  - `evidence_verdict`;
  - `evidence_import_ids UUID[]` (every import the verdict read);
  - `provider_reference_at_submission` (R-6 pinning, must be NULL for the unbound reasons).
- CHECKs on M4 rows: `operation = 'payout'`; `target_state IS NULL`; `provider_id`, `evidence_ref_hash`,
  `evidence_line_id`, `evidence_verdict` and `evidence_import_ids` NOT NULL; `basis_code =
  'provider_confirmed_out_of_band'`; `finding_code` and `reserved_provider_tx_id` NULL; `(kind =
  'm4_evidence_paid') = (evidence_reference IS NOT NULL)`.
- **DB insert trigger (M-10):** `m4_evidence_paid` is refused when `terminal_reason = 'destination_mismatch'`; M4 is
  refused for any reason outside §4.1, and for an unbound reason with a non-NULL `provider_reference`.
- The existing partial UNIQUE indexes make M2 and M4 mutually exclusive per attempt.
- `payment_m2_admits` and `payment_attempts_guard()` are **not edited**: M4 never changes the attempt, which stays
  `disputed` (A-15).

### 4.3 Who may read the evidence (C-3 / LF H-3; security M-5)

- **Acting SELECT policies** are added on `payment_statement_imports` and `payment_statement_lines`: `tenant_id =
  acting tenant AND financial_acting_session_valid()`. This reverses ADR 0101 §6.4's "no acting policy" for these
  two tables only; `payment_attempt_reference_evidence` gets the same acting SELECT because Y is read (C-5).
- `payout_m4_evidence` **raises an error, never a verdict**, when the attempt row or any of its statement scope is
  not visible to the session.
- **Import integrity:**
  - The INSERT arms of `tenant_staff_scope` on imports and lines (0102) are narrowed to the **system shape**: tenant
    GUC only; principal, platform-admin, player, platform-service and acting GUCs all NULL.
  - Because the system shape is still forgeable under arbitrary SQL, every import also carries a **Go import seal**
    (B13 seal subkey, label `b13-import-v1`). It covers `canon(import id, tenant, provider, source_label, is_mock,
    coverage_start, coverage_end, content_digest, line_count, fetched_at, payout_lines_carry_merchant_reference,
    lines_digest)`, where `lines_digest` is the SHA-256 over the canonical encoding of all lines in `line_no` order.
  - New import columns: `import_seal`, `seal_kid`, `payout_lines_carry_merchant_reference BOOLEAN` (copied from the
    statement source's declaration), `imported_by_service`.
  - **An import without a valid seal is not eligible M4 evidence.** The Go executor verifies the seal and recomputes
    `lines_digest` for every id in `evidence_import_ids` before insert and again at execution.
- **T10 (forgeable statement lines)** is recorded in ADR 0110 §10a. With sealed imports implemented and confirmed by
  security, non-MOCK M4 is unblocked; otherwise it stays launch-blocking unless the human explicitly accepts T10
  (§10.3).
- **Approver view:** shows each evidence line's provenance (import id, `imported_by_service`, `content_digest`,
  `fetched_at`, seal status). Approvers must confirm the line against the **provider portal**; `evidence_ref_hash`
  binds that confirmation.

### 4.4 Deterministic evidence (`payout_m4_evidence(p_tenant, p_attempt) RETURNS TABLE (verdict, line_id, reference, import_ids)`)

STABLE, `search_path` pinned, no SECURITY DEFINER. It reads persisted `kind = 'payout'` lines of this tenant and
provider:
- by `merchant_reference = attempt.merchant_reference` (platform-issued, unique per tenant, immutable);
- for `destination_mismatch`, also by the attempt's bound `provider_reference` (C-6);
- also on any typed reference evidence Y of the attempt (`payment_attempt_reference_evidence`).

**Bounded (I-2, confirmed):** at most 64 lines are read; the 65th makes the verdict `evidence_overflow`, which is
refused loudly with an audit row and never truncated. **Eligible import** = sealed (above) and (`is_mock = false`,
or no `is_mock = false` import exists for the tenant and provider).

| Verdict | ALL of the following |
|---|---|
| **`paid`** (C-5) | after cross-import dedupe on (provider_reference, status, amount, asset_code, occurred_at) there is **exactly one** `succeeded` line, with reference R; its `amount = attempt.amount AND asset_code = attempt.asset_code` (I-1); no `declined`, `reversed` or `pending` line exists on the merchant reference, on R or on any Y of the attempt in **any** import (MOCK included); R carries no reserved prefix; `payment_y_attributable(tenant, provider, attempt, R)` (0119) is true, together with the stricter tombstone exclusion (no tombstone ledger row on (provider, R)); no other attempt holds R as `provider_reference` **or as typed Y**; if this attempt has a Y and Y ≠ R, the verdict is `contradictory`; no `withdrawal_requests.provider_reference = R`; no `ledger_transactions` row with (provider, `provider_tx_id = R`) of any type; **and no `ledger_transactions.idempotency_key = provider_id || ':' || R`** |
| **`not_paid`** (H-1) | (i) an eligible import of this provider with `coverage_start <= attempt.created_at` and `coverage_end >= attempt.last_sent_at + payments.DefaultSettlementWindow` (24 h; NULL `last_sent_at` → `insufficient`); (ii) a `declined` line with equal amount and asset whose `occurred_at >= attempt.last_sent_at`, attributed by the merchant reference or by the attempt's bound `provider_reference`; (iii) **no** `succeeded`, `pending` or `reversed` payout line in **any** import (MOCK included) on the merchant reference, on the attempt's `provider_reference`, or on any Y of the attempt; (iv) the import used for (i) and (ii) has `payout_lines_carry_merchant_reference = true`, otherwise the verdict is `insufficient` |
| `insufficient`, `contradictory`, `evidence_overflow` | anything else |

- **`evidence_line_id` (L-1)** is deterministic: the earliest qualifying line by (`fetched_at`, `import_id`,
  `line_no`). Execution re-evaluates and checks that the pinned line is still in the qualifying set.
- **Sufficiency standard (A-13):** both the machine verdict over sealed imports **and** a four-eyes operator
  confirmation against the provider portal (`evidence_ref_hash`) are required. A statement source must be
  registered for the provider (O-4).
- This standard needs human acknowledgement before any non-MOCK M4 (D-7).

### 4.5 Submission, approval, execution

- **Request** (existing route; kinds widened):
  - Body: `attempt_id`, `kind`, `evidence_line_id`, `evidence_ref_hash`, `basis_code`, `reason_code`.
  - The insert trigger recomputes `payout_m4_evidence`. The evidence columns are DB-forced and must equal the
    request; otherwise `force_resolve_evidence_mismatch`. The verdict must be `paid` / `not_paid` respectively.
  - R-6 pinning extends to `provider_reference_at_submission` and the evidence columns; all of them are in
    `payload_hash`.
- **Approvers (A-14, D-1):** K3 rules plus **at least one `platform_acting` approver** on both M4 kinds.
- **Execution** (ADR 0101 §6.3 lock order):
  1. L1 `LockSubmittedForResolution`.
  2. Attempt `FOR UPDATE`.
  3. Resolution `FOR UPDATE`.
  4. Approval insert.
  5. Staff, then grants, `FOR SHARE`.
  6. Go re-verifies the import seals.
  7. Re-evaluate. Verdict, pinned line and R must be unchanged. Attempt state, `terminal_reason` and
     `provider_reference IS NULL` must equal the pinned values (C-6), and the withdrawal must be `submitted`.
     Otherwise `refused_at_execution`.
  8. `executing` → posting → `executed`; link the ledger row; audit; commit.
  - `m4_evidence_paid` posts through `withdrawal.Complete(tx, wr.ID, attempt.provider_id, R)`: a
    `withdrawal_completed` keyed `provider_id:R`, with `release_ledger_transaction_id` set. This is the attempt's own
    release keyed by the PSP line reference (LF F6).
  - `m4_evidence_not_paid` posts through `withdrawal.Fail(tx, wr.ID, "m4_evidence_not_paid")`: a
    `withdrawal_failed` keyed `wr.id:failed`, hold → `player_cash`.
- **MR041 deferred check, rewritten per kind (C-7; 0115:746-778). An executed resolution must satisfy:**
  - **all M2/M4 kinds:** the linked ledger row is same-tenant with `correlation_id = m.withdrawal_request_id`.
  - **`m2_*`:** unchanged.
  - **`m4_evidence_paid`:**
    - the withdrawal is `completed` and `release_ledger_transaction_id = m.ledger_transaction_id`;
    - the ledger row has `transaction_type = 'withdrawal_completed'`, `provider_id = m.provider_id`,
      `provider_tx_id = m.evidence_reference`, `idempotency_key = m.provider_id || ':' || m.evidence_reference`;
    - entries are exactly two: debit `player_withdrawal_hold` on `w.wallet_id` and credit `psp_clearing` with NULL
      wallet, each `amount = m.amount`, `asset = m.asset_code`;
    - the attempt's state, `terminal_reason` and `provider_reference` (NULL) equal the pinned values.
  - **`m4_evidence_not_paid`:**
    - the withdrawal is `failed` with the link;
    - the ledger row is `withdrawal_failed` with key `m.withdrawal_request_id || ':failed'`;
    - entries are exactly two: hold debit and `player_cash` credit, both on `w.wallet_id`, amount and asset equal;
    - the attempt fields equal the pinned values.
- **Acting fences** (0125 CREATE OR REPLACE on top of the **0124** bodies; down restores 0124's byte-for-byte):
  - `ledger_governed_fence_allows` branch **(f)**: `withdrawal_completed` with `correlation_id =
    m.withdrawal_request_id`, `provider_id = m.provider_id`, `provider_tx_id = m.evidence_reference` and an
    executing `m4_evidence_paid` in this txid (C-7, as branch (b)).
  - Branch **(g)**: `withdrawal_failed` with key `m.withdrawal_request_id || ':failed'`, `correlation_id =
    m.withdrawal_request_id` and an executing `m4_evidence_not_paid`.
  - `ledger_entries_governed_fence()` per-entry shapes: as listed in the MR041 rules above.
  - The acting `ledger_accounts` INSERT and the acting `withdrawal_requests` UPDATE WITH CHECK add the M4 kinds.
- **ADR 0110 digest (L-1):** the K3 INSERT digest appends `evidence_line_id`. **All K3 INSERT digests gain a
  trailing field, `~` (NULL) for M1/M2; SQL and Go change in lockstep; a test pins the new digests.** There is no
  new proof operation.
- **Idempotency / replay:** one pending and one executed resolution per attempt; a retried decide is refused with a
  fresh proof and posts nothing; the ledger keys are unique.

### 4.6 Reconciliation (C-4; 0125 is NOT migration-free)

- 0125 widens `tenant_system_read_executed` (0115:1419) to the M4 kinds; `loadK3Evidence` also loads executed M4
  rows.
- **New predicates (no new mismatch kind, no kind-CHECK change):**
  - an executed `m4_evidence_paid` raises `pay_declared_paid_unconfirmed` on any later `declined` or `reversed` line
    on R or on the merchant reference, or on a **second distinct** `succeeded` line;
  - an executed `m4_evidence_not_paid` raises `pay_declared_not_paid_but_paid` on any later `succeeded` line on the
    merchant reference, the bound reference or a Y.
- **I-1 tightening:** `payoutCompletedRef` clears a line only when `line.amount = attempt.amount AND line.asset =
  attempt.asset`.
- **I-2:** `loadK3Evidence` keeps INV-M-5 but gets a hard per-run cap on lines read; exceeding it fails the run with
  the existing P1 `reconciliation.sweep_run_failed` (shared with CAS-RECON-SCALE-1).
- **Hints (L-4):** the unbound-park hint names M4. After an executed M4 not-paid, the `pay_captured_unposted` hint
  reads "possible double payout after M4 not-paid: recovery via compensating debit (K2) or off-platform recovery;
  never allocation".

### 4.7 API, errors

- Existing `payment_force_resolution_routes.go`.
- New read route `GET /v1/admin/tenants/{tenantID}/payment-attempts/{id}/resolution-evidence`
  (`payment_force_resolve:read`): returns the verdict, line id and provenance; no provider text.
- New error tokens: `force_resolve_evidence_insufficient`, `force_resolve_evidence_mismatch`,
  `force_resolve_evidence_overflow`, `force_resolve_evidence_unsealed`.

### 4.8 Post-resolution signal cells (C-2 / LF H-2)

These cells run in the receipt and poll paths, under L1 withdrawal → attempt. Each writes one audit row
(`payments.payout_post_m4_contradiction`, closed vocabulary) and then raises the B12 P1 as its **last** statement.
Nothing changes state, posts or releases.

| Situation | Signal reason |
|---|---|
| a `succeeded` callback or poll on a `disputed` attempt whose withdrawal was `failed` by an executed `m4_evidence_not_paid` | `success_after_m4_not_paid` |
| a `declined` / `reversed` callback or poll after an executed `m4_evidence_paid` | `contradiction_after_m4_paid` |

Replays are one open alert with growing occurrences; the audit row is written once per new receipt
(PAY-PAYOUT-CALLBACK-AUDIT-2 rule).

### 4.9 Tests (RESOLVE-1)

- Every verdict condition (i)-(iv) and every `paid` condition: amount/asset mismatch; two references; reserved
  prefix; R held by another attempt or as another attempt's Y; own Y ≠ R; R as an existing key or idempotency key;
  tombstone; MOCK contradicting line; coverage too short; decline before `last_sent_at`; a source that does not
  declare `payout_lines_carry_merchant_reference`.
- Unsealed or tampered import, and a line inserted by a principal-shaped session (refused at INSERT).
- An attempt not visible to the session → error, not a verdict.
- C-6 pin: a non-NULL reference is refused at insert and at execution.
- Destination-mismatch paid is refused in the DB.
- Paid clears `pay_captured_unposted` only via its own completion keyed by R with amount/asset equal.
- §4.6 predicates; §4.8 cells.
- MR041 per-kind failures, through a DB-level forgery attempt in each session.
- Fence mutants: drop correlation, drop provider_tx_id binding, admit any key.
- Platform-approver floor; S-12 / LF-11; proofs (AP001/AP002/AP004/AP005); the new digest pins.
- `-race -count=50`: M4 vs poll, vs sweeper, two final approvals, M2 vs M4.
- Overflow at line 65; deterministic `evidence_line_id`.
- Down restores the 0124 bodies.

---

## 5. Cross-part interactions

| Pair | Interaction |
|---|---|
| B13 × HSEC | A release sends nothing anywhere: the hold returns to the player's own cash; the instrument is only copied into the record. "Resume after reactivation" re-runs the B13 gate, so a reactivated withdrawal with an unusable instrument stays parked (HD-R15-5). |
| B13 × RESOLVE-1 | `destination_mismatch` is M2-refused and M4-not-paid-only (enforced in the DB). M4 audit rows carry the snapshot instrument and fingerprint prefix. B13 is not a prerequisite of RESOLVE-1 (decision 1). |
| RESOLVE-1 × HSEC | Disjoint: M4 needs an attempt and a `submitted` withdrawal; a hold release needs `approved` and **no** attempt. Both serialise on the withdrawal L1 lock. |
| All × kill switch / H-SEC (decision 18) | Unchanged; no part adds a dispatch path. |

---

## 6. HSEC-APPROVED-HOLD-RELEASE-1 — four-eyes release of an `approved` hold on a non-active tenant/brand (migration 0124)

### 6.1 Smallest mechanism (A-16)

- **Resume after reactivation:** no new kind. Once the tenant **and** the brand are `active`, the existing
  `ClaimForDispatch` re-runs the H-SEC, KYC and B13 gates. Reactivation itself is out of scope.
- **`release_hold_to_player`:** the only new kind. It moves `approved → rejected` and posts the hold back to the
  player's own `player_cash` on the same wallet. It never dispatches.

### 6.2 Data model (subsumes ADR 0107 CT-PRE)

**`withdrawal_hold_resolutions`** (replaces ADR 0107's planned `closed_tenant_hold_resolutions`):
- Server-forced `id`; `tenant_id`, `withdrawal_request_id` (composite FK); `kind CHECK IN ('release_hold_to_player')`.
- DB-forced copies: `brand_id`, `player_account_id`, `wallet_id`, `amount`, `asset_code`, `payout_instrument_id`.
- Pinned at submission: `withdrawal_state_at_submission` (= `approved`), `tenant_status_at_submission`,
  `brand_status_at_submission`. Recorded at execution: `tenant_status_at_execution`, `brand_status_at_execution`.
- `reason_code CHECK (reason_code ~ '^[a-z][a-z0-9_]{0,63}$')` (L-6; the vocabulary is HD-CTF-7);
  `evidence_ref_hash NOT NULL`; DB-computed `payload_hash`.
- Forced `requested_by` with `requested_by_scope = 'platform_acting'`, and `requested_by_person_id`.
- Policy snapshot: `required_at_submission`, `contributing_policy_ids`, `required_at_execution`.
- The K3 `state` set; DB-forced `expires_at`; `executed_txid`; `ledger_transaction_id UNIQUE`; `refusal_code`.
- Partial UNIQUE `(withdrawal_request_id)` on `pending` and on `executed`.

**`withdrawal_hold_resolution_approvals`:** the K3 approvals shape, with scope `platform_acting` only.

Both tables: RLS family A only (no T, no P); DELETE and TRUNCATE denied. Grants: `SELECT, INSERT, UPDATE` on
resolutions; `SELECT, INSERT` on approvals.

### 6.3 Governance

- **Classification:** `withdrawal_hold_resolution` = `mandatory_four_eyes` (0113 CHECK widened), regardless of
  amount.
- **Capability pair:** `withdrawal_hold_resolution:request` / `:approve` (0112 CHECK widened;
  `eligible_tenant_roles = '{}'`; grants need a NOT NULL `valid_until`).
  - It is not `payment_force_resolve` (Q-CT-SEC-2), and it replaces ADR 0107's `closed_tenant_hold_resolution:*`.
  - **A-20 is accepted by security with condition M-9:** adding any further kind to `withdrawal_hold_resolutions`
    needs a security review that chooses either a new capability pair or revocation and re-issue of every in-force
    grant.
- **Actors (A-17, D-2):** `platform_acting` only, for requester and approvers.
- **Static permissions (H-4):** `withdrawal_hold_resolution:request|approve|read` → **`RolePlatformAdmin` only**. No
  platform finance role exists. A permission test asserts that every tenant role (including `tenant_admin`,
  `finance`, `compliance`) is denied.
- **Policy:** `financial_policy_required_approvals('withdrawal_hold_resolution', …)`, `GREATEST(1, …)`, with the
  non-active special case extended. No platform policy row ⇒ disabled.
- **Rules carried over:** LF-11, S-12, S-2(iii), HD-PRH2-8 interim. A single staff member can never release.
- **Signed actor proof:**
  - `zz_actor_proof_guard` goes on both tables; the catalog pin becomes **eleven** tables.
  - **ADR 0110 verifier operation table (M-10):** the four operations `withdrawal_hold_resolution:request`,
    `:cancel` (requester only), `:approve`, `:reject` are restricted to scope `platform_acting` **with** a tenant.
    The verifier refuses `tenant` and `platform` scope for them, and the NULL-tenant encoding stays K1/policy only.
  - `pending → expired` is proof-less **only** when `now() >= expires_at`.
  - The service lives in `internal/payments` (an already-allowed signer), so the static signing allowlist is not
    widened.

### 6.4 Preconditions, transition, posting, DB gating (rewritten for M-6, L-2)

**Preconditions** (checked at insert, at each approval and at execution):
- withdrawal `approved`;
- no `payment_attempts` row exists;
- `tenants.status <> 'active' OR brands.status <> 'active'`, read under the H-SEC lock discipline (tenant advisory
  lock SHARED, brand row `FOR SHARE`).

If both are active again at execution, the result is `refused_at_execution` (A-19).

**`withdrawal.ReleaseForGovernedResolution`:**
- takes the L1 lock;
- asserts `approved` and no attempt;
- posts `withdrawal_rejected`: hold debit and cash credit on the same wallet, for `wr.amount`, with
  `CorrelationID = wr.id`, `ReversesTransactionID` = the hold transaction, and key **`<wr.id>:governed_hold_released`**;
- runs a state CAS `approved → rejected` that sets `release_ledger_transaction_id`;
- puts no `reason_code` on the ledger row;
- moves to `rejected`, not `cancelled` (A-18).

**DB gating:**
- **CT-R3 guards (L-2):**
  - The all-sessions BEFORE INSERT trigger on `ledger_transactions` refuses any key ending
    `:governed_hold_released` unless all of these hold: an executing resolution `r` exists with `executed_txid =
    txid_current()`; `idempotency_key = r.withdrawal_request_id || ':governed_hold_released'`; and `correlation_id =
    r.withdrawal_request_id`.
  - The `withdrawal_requests` BEFORE UPDATE trigger refuses `→ rejected` through such a posting without that
    resolution.
- **Approved-hold freeze trigger (M-6, adopted, the safer reading):** a new all-sessions BEFORE UPDATE trigger on
  `withdrawal_requests` refuses `approved → rejected | cancelled | failed` while the tenant **or** the brand is
  non-active, unless an executing hold resolution for this withdrawal exists in this txid.
  - Normal paths are not affected: `DenyForCompliance` runs only after the H-SEC gate, so a KYC denial cannot occur
    while non-active.
  - `approved → cancelled | failed` has no legitimate caller today.
- **Deferred check (L-2), in all sessions:** an executed resolution links a same-tenant `withdrawal_rejected` with
  the exact key and `correlation_id = wr.id`. Its entries are **exactly two**: debit `player_withdrawal_hold` and
  credit `player_cash`, both on `w.wallet_id`, with `amount = r.amount` and `asset = r.asset_code`. The withdrawal is
  `rejected` with the link.
- **Acting fences:** branch (e) (`withdrawal_rejected`, the exact key, the correlation); the per-entry shape; the
  acting `ledger_accounts` INSERT for this resolution's `player_withdrawal_hold`; the acting `withdrawal_requests`
  UPDATE WITH CHECK adds "executing hold resolution". 0124 builds on the 0115 bodies; 0125 builds on 0124's.
- **Stated residual (ADR 0110 T5):** a runtime session with arbitrary SQL can still write ordinary non-governed
  postings. The triggers above stop the governed key and the frozen-state edges in every session. They cannot stop
  a forged ordinary posting, which stays the T5 residual (bounded by ledger invariants and reconciliation), and the
  revision-1 claim "DB gating in ALL sessions" is narrowed to exactly these edges and keys (D-8).

### 6.5 Lock order, concurrency, idempotency

Lock order:
1. L1 withdrawal `FOR UPDATE`.
2. Tenant advisory lock SHARED and brand `FOR SHARE`.
3. Resolution `FOR UPDATE`.
4. Approval insert.
5. Staff, then grants, `FOR SHARE`.
6. Recount and re-check.
7. `executing`.
8. Posting (L3/L4).
9. `executed`, audit, commit.

ADR 0082 A8 position: after `payment_manual_resolutions`, before `ledger_adjustment_requests`. Races serialise on
L1. Exactly-once comes from the L1 lock, the CAS, the distinct key, the partial UNIQUE index and the deferred check.

### 6.6 Audit

Every request, approval, rejection, cancellation, expiry, refusal and execution writes
`withdrawal.hold_resolution_<event>` (closed token set for refusals). The ADR 0104 projection applies.

### 6.7 Relation to ADR 0107

- **In scope:** CT-PRE × release for `approved` only, for suspended or closed tenants and non-active brands.
- **Not in scope:** `requested`, `pending_review`, CT-NEVER-SENT, permits, retain, the rule tables and HD-CTF-1(b)
  to HD-CTF-10.
- When ADR 0107 is built, it extends these objects (subject to M-9).

### 6.8 Tests (HSEC)

- **Happy path:** one release with the exact key and two-leg shape; the hold nets to zero; a second release is
  refused.
- **Refusal matrix:** active tenant and brand; wrong states; an attempt exists; one approver; same Person;
  beneficiary; tenant-scope actor; every tenant role refused at the route (H-4); capability held only as
  `payment_force_resolve`; expired grant; no policy row.
- **Re-check at execution:** reactivation between submission and execution.
- **DB triggers:** the freeze trigger in tenant, acting and system sessions (`approved → rejected/cancelled/failed`
  on a non-active tenant without a resolution is refused; KYC deny on an active tenant still works); the CT-R3 key,
  correlation and shape forgeries in every session.
- **Proofs:** AP001-AP005; scope refusals per operation; early expiry refused.
- **Concurrency:** `-race -count=50`.
- **Regression:** H-SEC and kill-switch suites green; migration up/down/up; the eleven-table pin.

---

## 7. Implementation plan (revised)

### 7.1 Files per part

| Part | Migration | Primary files |
|---|---|---|
| B13-A entity | 0123 | new `internal/payoutinstrument/*` (kinds, validators incl. Luhn refusal, normalisers, masks, AEAD, fingerprint, seals, gate rule, `PayoutInstrumentVerifier` + Synthetic mock, re-fingerprint job), `internal/config`, `cmd/platform-api` (wiring, startup gate), new `internal/httpserver/payout_instrument_routes.go`, `internal/auth/permission.go`, `backoffice/src/auth/permissions.ts`, `deploy/init-app-role.sql`, runbook (key rotation, re-fingerprint) |
| B13-B integration | uses 0123 | `internal/withdrawal/withdrawal.go` (RequestParams, replay compare, columns), `internal/payments/{types.go,payout.go,payout_sweep.go,payout_alerts.go,receipt.go,contract.go,manual_resolution.go (PayoutDisputeReasons only)}`, `internal/httpserver/withdrawal_handlers.go`, `internal/reconciliation/payment_statement.go` (`disputeReasonClasses` only) |
| HSEC | 0124 | `internal/withdrawal/withdrawal.go` (`ReleaseForGovernedResolution` only), new `internal/payments/withdrawal_hold_resolution.go`, `internal/capability/capability.go`, `internal/actorproof` verifier operation table, `internal/auth/permission.go`, `permissions.ts`, new `internal/httpserver/withdrawal_hold_resolution_routes.go` + one line in `routes.go`, `init-app-role.sql` |
| RESOLVE-1 | 0125 | `internal/payments/manual_resolution.go` (+ new `manual_resolution_m4.go`, §4.8 cells in `receipt.go`/`payout.go`), `internal/reconciliation/{payment_statement.go,payment_statement_k3.go}` (predicates, I-1, I-2, hints, import sealing in the importer), `internal/httpserver/payment_force_resolution_routes.go`, `init-app-role.sql` |

### 7.2 Migrations: order, merge, split

- **Three separate migrations; none merged, none split further.**
- **Revision 2 swaps the numbers:** **0123 B13, 0124 HSEC, 0125 RESOLVE-1**. Both 0124 and 0125 CREATE OR REPLACE
  the four shared fence/acting objects. HSEC is approved with conditions and RESOLVE-1 needs an LF re-review, so the
  approved part takes the lower number and 0125 builds on 0124's bodies.
- `internal/db` refuses version gaps, so merges go strictly in number order. If the order changes, the orchestrator
  renumbers and re-bases the fence bodies.
- 0123 shares no objects with 0124/0125.

### 7.3 What may start now vs what must wait

| Work | Status |
|---|---|
| **B13-A** (entity, keys, AEAD, seals, verifier interface, player/staff routes, 0123) | **May start now**: every LF and security condition is design-complete in §2 |
| **HSEC** Go and SQL (0124) | **May start now** (conditions H-4, M-6, M-9, M-10, L-2, L-6 incorporated) |
| **RESOLVE-1, pure tightenings with no new power and no migration**: `payoutCompletedRef` I-1 amount/asset equality (§4.6); the `loadK3Evidence` I-2 cap with run failure; the L-4 hint text | **May start now** |
| **RESOLVE-1, everything else** (kinds, evidence function, import seal and policy narrowing, acting SELECT, fences, MR041, §4.8 cells, 0125) | **Waits** for the `ledger-finance` re-review of §4 and the `security` co-ruling on T10/M-5 |
| **B13-B** | waits for B13-A to merge |
| Any non-MOCK payout or non-MOCK M4 | blocked by §10.3 flags and the existing gates (S-L1/S-L3/S-L4, ALERT-DELIVERY-1, §35.4 GATE) |

Shared files are `permission.go`, `permissions.ts` and `init-app-role.sql` (ordered appends), plus
`manual_resolution.go` (RESOLVE-1 owns it; B13-B adds one map entry; whichever merges second rebases).

## 8. Review gates

| Part | Gates |
|---|---|
| B13 | `security` (lead), `identity-compliance` (sources, ownership, KYC-verified precondition, max-age mechanism), `ledger-finance`, `payments`, `qa`, `code-reviewer`, `architect` |
| HSEC | `security` (lead), `ledger-finance`, `product-owner-proxy`, `qa`, `code-reviewer` |
| RESOLVE-1 | **`ledger-finance` re-review (lead, binding)**, `security` (T10/M-5 co-ruling, digest, I-2), `payments`, `qa`, `code-reviewer` |

Every part needs mutation-kill evidence, runtime-role integration tests and migration up/down/up.

## 9. AMBIGUITIES (chosen interpretation and why)

- **A-1** Bind at request creation (instrument `FOR SHARE`); re-validate and snapshot at T1p; re-check without
  re-resolving at phase B and T2/T12.
- **A-2** An instrument belongs to one player account; a fingerprint is owned by one Person per tenant, under every
  fingerprint kid.
- **A-3** Open kind table; crypto disabled for real use; cash is not an instrument.
- **A-4** Closed source set, forced from the verifier's type marker; no staff verification; `synthetic` only with
  Synthetic adapters.
- **A-5** Only `account_holder_matches_verified_identity` for real sources, plus the Person's KYC verified.
- **A-6** `expires_at` NOT NULL; with no max-age config row, real verification is unusable.
- **A-7 (revised)** Detail is AEAD-encrypted in 0123 (no plaintext ever stored); no PAN; fixed mask rules; the
  fingerprint is never returned. This supersedes the revision-1 deferral (security H-3).
- **A-8** The echo is fingerprint-only, computed in the adapter under the snapshot kid; an unknown kid counts as a
  mismatch.
- **A-9** Revocation and supersession are refused while the instrument is in use.
- **A-10** `payment_method` is derived from the rail; a differing value is refused.
- **A-11** Legacy NULL bindings are Synthetic-only, under three stacked controls: the L-6 migration assertion, the
  Go startup check and the per-gate tiering predicate (D-5).
- **A-12** RESOLVE-1 covers the unbound reasons with NULL reference, plus `destination_mismatch` not-paid only
  (D-4).
- **A-13** Sufficiency = verdict over sealed imports + four-eyes portal confirmation (D-7).
- **A-14** M4 needs at least one `platform_acting` approver (D-1).
- **A-15** The attempt stays `disputed` under M4.
- **A-16** HSEC has one kind; resume is the normal submit.
- **A-17** HSEC actors are `platform_acting` only (D-2); the role is `RolePlatformAdmin` only.
- **A-18** HSEC goes `approved → rejected` with the distinct key.
- **A-19** HSEC is refused at execution if tenant and brand are both active (D-3).
- **A-20 (revised)** The new table and capability names subsume ADR 0107's. Accepted by security with M-9.
- **A-21 (revised)** Go-side seals over instrument, verification, blocking event, snapshot and statement import,
  with canonical encoding, HKDF subkeys and a separate fingerprint key lifecycle.

## 10. HUMAN DECISIONS STILL REQUIRED

### 10.1 Policy questions (the design fails closed around each)

- **HD-R15-1** Verification max-age / re-verification cadence per jurisdiction. **Launch-blocking** for any
  sandbox or real payout.
- **HD-R15-2** Further ownership assertions that count as "verified" (card proven by own deposit, PSP rails,
  custodian attestation, e-wallet login).
- **HD-R15-3** Return-to-source (D2).
- **HD-R15-4** Crypto destinations (ADR 0008).
- **HD-R15-5** Disposition of an `approved` withdrawal whose instrument becomes permanently unusable: parked
  (current), four-eyes release, or four-eyes rebind. **Stranded funds: launch-blocking before any non-MOCK payout
  (LF L-7).**
- **HD-R15-6** A confirmed misdirected payout (retained today).
- **HD-R15-7** (non-blocking) Gate `Approve` on a non-active tenant/brand; always audit a refused staff submit.
- **HD-R15-8** Accept T10 (forgeable statement lines) explicitly, **or** require sealed imports (designed in §4.3)
  before non-MOCK M4.
- **HD-R15-9 (L-9, RECOMMENDATION, deferred):** player notification and a cooling-off period after instrument
  registration or change.
- Unchanged and still open: HD-CTF-1(b), HD-CTF-2..10, HD-PRH2-3, HD-PRH2-8.

### 10.2 Deviations from earlier rulings, for the human to see (D-1..D-9)

- **D-1** A-14 requires a `platform_acting` approver on M4 now. This pre-empts the HD-PRH2-8 / R-K3-10 launch flag
  for M4 only.
- **D-2** A-17 makes HSEC `platform_acting` only, even for suspended tenants and non-active brands. ADR 0101 §24.5
  applied this to closed tenants only.
- **D-3** A-19 refuses HSEC execution after reactivation. A release becomes impossible once active, by design.
- **D-4** A-12 extends RESOLVE-1 to `destination_mismatch` (not-paid only). This goes beyond decisions 9-12, which
  concern unbound payouts; it is derived from decision 7.
- **D-5** A-11 keeps legacy NULL bindings dispatchable to Synthetic adapters only, under three stacked controls.
- **D-6** A-9 lets a player be refused revoking their own instrument while it is in use.
- **D-7** The A-13 evidence standard needs human acknowledgement before any non-MOCK M4.
- **D-8** The revision-1 §6.4 claim "DB gating in ALL sessions" was an over-claim. Revision 2 narrows it and states
  the T5 residual.
- **D-9** (LF L-5) M4 reuses `payment_force_resolve:*`, so every in-force M2 grant authorises M4. The safer
  alternative is a distinct capability pair, at the cost of kind-dependent proof operations; security/owner to
  choose.

### 10.3 Launch-blocking flags

| Flag | Blocks |
|---|---|
| Security H-3: AEAD detail implemented | any real customer instrument data (built into 0123; blocking until implemented and reviewed) |
| Security M-5 / T10 (HD-R15-8) | any non-MOCK M4 |
| ADR 0110 T3/T4 (identity-store integrity) | real money, as already recorded |
| HD-R15-1 | any sandbox/real payout |
| HD-R15-5 (LF L-7) | any non-MOCK payout |

## 11. Review conditions incorporated (revision 2)

Where two conditions overlapped (LF C-3 vs security M-5: "narrow INSERT arms **or** seal imports"), revision 2 does
**both**, the safer reading. LF L-9/security L-9 overlaps were taken as adopted only where fail-closed (the KYC
precondition); the rest is deferred as HD-R15-9.

| Condition | Where |
|---|---|
| LF H-1 / C-1 not_paid (i)-(iv); `ever_possibly_sent` sentence replaced | §4.4 |
| LF H-2 / C-2 post-M4 cells; "disputed any no-op" fixed | §4.8, §2.6 |
| LF H-3 / C-3 acting SELECT; error when invisible; system-shape imports | §4.3 |
| LF H-4 / C-4 `tenant_system_read_executed`, `loadK3Evidence`, predicates, I-1 in `payoutCompletedRef`; "no migration" corrected | §4.6 |
| LF M-1 / C-5 `payment_y_attributable`, tombstone, Y checks, exactly-one, idempotency-key check | §4.4 |
| LF M-2 / C-6 NULL reference for all unbound reasons, pinned and re-checked; bound-reference scan for destination_mismatch | §4.1, §4.2, §4.4, §4.5 |
| LF M-3 / C-7 fence (f)/(g) bindings; MR041 per kind | §4.5 |
| LF M-4 replay compare; instrument `FOR SHARE` at request and in the INSERT trigger; snapshot amount/asset | §2.1, §2.4 |
| LF L-1 deterministic `evidence_line_id` | §4.4 |
| LF L-2 HSEC deferred two-leg check; CT-R3 exact key/correlation | §6.4 |
| LF L-4 hint after M4 not-paid | §4.6 |
| LF L-5 M4 reuses `payment_force_resolve` | §4.2, D-9 |
| LF L-6 migration-time legacy assertion | §2.1 |
| LF L-7 HD-R15-5 launch-blocking | §10.1, §10.3 |
| Sec H-1 `→ verified` same-tx verification; terminal states; forced `state_changed_at`; blocking events; gate rule | §2.1, §2.2 |
| Sec H-2 composite FK to owners; separate fingerprint key lifecycle; all-kid conflict check; re-fingerprint job; no fingerprint in APIs | §2.1, §2.2, §2.9 |
| Sec H-3 AEAD detail in 0123; mask rules; launch flag | §2.1, §2.2, §10.3, A-7 |
| Sec H-4 `RolePlatformAdmin` only + tenant-role denial test | §6.3, §6.8 |
| Sec M-1 canonical encoding; extended seal coverage; instrument seal; verifier refuses unsealed; relational checks | §2.2 |
| Sec M-2 `PayoutDestination` fields; `FingerprintKid`; fingerprinter capability; conformance; redaction | §2.6, §2.7 |
| Sec M-3 tiering in phase B and T2/T12; source forced from marker; marker-only; Synthetic no I/O; registered verifier | §2.4, §2.5, §2.2 |
| Sec M-4 HKDF labels; startup gate scope; no fallback key; retired kids; key distinctness | §2.2 |
| Sec M-5 T10; provenance view; portal confirmation; launch flag; cap 64 confirmed | §4.3, §4.4, §10.3, HD-R15-8 |
| Sec M-6 §6.4 rewritten; freeze trigger adopted; T5 residual | §6.4, D-8 |
| Sec M-7 generic refusal; audit + compliance alert; rate limit | §2.9 |
| Sec M-8 acting SELECT on snapshots only; kinds SELECT-only grant test; governed max-age writer under T8 | §2.1 |
| Sec M-9 condition on future kinds | §6.3, A-20 |
| Sec M-10 verifier operation scopes; proof-less expiry only when due; DB refusal of paid on destination_mismatch | §6.3, §4.2 |
| Sec L-1 digest wording | §4.5 |
| Sec L-3 supersession of an in-use instrument | §2.3, §2.4 |
| Sec L-4 only the sweep writes expiry | §2.1 |
| Sec L-5 phase B re-reads state → NotSent | §2.4 |
| Sec L-6 HSEC reason pattern | §6.2 |
| Sec L-7 provider revocation blocking-only, authenticated, in-package | §2.1 |
| Sec L-8 card_token rules | §2.2, §2.9 |
| Sec L-9 notification/cooling-off deferred; KYC-verified precondition adopted | §2.5, HD-R15-9 |
| ADR 0110 §10a T9/T10/T11 pointer | ADR 0110 header pointer, §2.2, §4.3 |
| Deviations D-1..D-8 (+ D-9) and launch flags | §10.2, §10.3 |
| §7 split revised; start-now vs wait | §7.2, §7.3 |

---

## 12. B13-A implementation notes (appendix, added with the B13-A code; the design above is not rewritten)

Scope delivered: migration `0123_payout_instruments` (the whole B13 **schema**, because section 7.2 keeps B13 as one
migration and section 7.1 says B13-B "uses 0123") and the Go package `internal/payoutinstrument` with its routes. The
Go integration of the binding into the withdrawal / payments paths is **B13-B and is NOT part of this change**.
Where the design left a sub-question open, the safest existing reading was taken and is recorded as `B13A-n`. No
policy question that changes an owner decision (ADR 0095 section 44, decisions 1-8) was needed; the three questions
the human or architect should see are in 12.3.

### 12.1 Deliverable status

| Deliverable | Status |
|---|---|
| Migration 0123: kinds (seeded, SELECT-only), instruments + DB state machine, verifications, blocking events, fingerprint owners, max-age table, `withdrawal_requests` binding columns + insert guard + immutability, write-once snapshots, `payment_attempts` snapshot constraint, RLS, grants, L-6 up-time assertion | `IMPLEMENTED` (runtime-role integration tests, up/down/up) |
| AEAD detail (AES-256-GCM, HKDF subkey, AAD = tenant, instrument, kind, schema version), masks, tenant-bound HMAC fingerprint with its own key family, canonical encoding, four Go-side seals | `IMPLEMENTED` |
| Gate rule (`EvaluateGate`), tiering predicate (`CheckTier`), `WriteSnapshot` / `LoadSnapshot`, `PayoutDestination` (redacting), `DestinationFingerprinter` / `DestinationEcho`, `ForcedSource` | `IMPLEMENTED` (callers are B13-B) |
| Player routes (register / list / revoke, generic 409, per-player rate limit, no card numbers) and staff routes (read, suspend) with permissions and `permissions.ts` | `IMPLEMENTED` |
| Startup gate (`VerifyStartup`, `cfg.GuardEnvironment()`), Go legacy-binding check, config key families | `IMPLEMENTED` and wired in `cmd/platform-api` |
| `PayoutInstrumentVerifier` interface + `MockVerifier` | `MOCK` (Synthetic; registered with the synthetic guard) |
| Any real PSP / KYC-vendor / custodian instrument verifier | `NOT IMPLEMENTED` (`PROVIDER DEPENDENT`) |
| Binding at request creation, T1p destination gate and snapshot insert, phase B / T2 / T12 re-checks, `destination_mismatch` park, echo cells, `WithdrawRequest.Destination` | `NOT IMPLEMENTED` (B13-B) |
| Expiry sweep scheduler, re-fingerprint operator command, provider-callback wiring of `ApplyProviderBlock` | `NOT IMPLEMENTED` (the methods exist and are tested; see the runbook `docs/runbooks/payout-instrument-keys.md`) |
| Compliance alert on a fingerprint conflict (M-7) | `PARTIALLY IMPLEMENTED`: audit row `payout_instrument.registration_conflict` only; an alert needs a new `alerting` Kind (a migration), see 12.3 |
| Acting-family read of snapshots under a VALID acting session | `PARTIALLY IMPLEMENTED`: the policy is pinned (SELECT only, `financial_acting_session_valid()`, snapshots only) and an invalid acting session reads nothing; a positive test needs a K1 grant world and belongs with the first consumer (RESOLVE-1) |

### 12.2 Ambiguities and the reading taken

- **B13A-1 (instrument id).** "`id` DB default; a client value is refused": the id is allocated by the database
  (`SELECT gen_random_uuid()` in the registration transaction) because the AEAD AAD and the instrument seal bind it before
  the insert. The HTTP surface refuses a client-supplied id (unknown-field rejection; tested). Reading: the *client*
  cannot choose the id; the platform's own insert path passes the id it just allocated.
- **B13A-2 (NULL binding, transitional).** Section 2.1 says the `withdrawal_requests` BEFORE INSERT trigger "requires both
  NOT NULL". Until B13-B wires the request path no code supplies a binding, so enforcing it now would break every existing
  withdrawal insert. 0123 therefore **validates a binding whenever one is supplied** (same tenant/brand/player, verified,
  in-force latest verification, no revoke, no later suspend, asset listed, fingerprint equal) and **tolerates NULL/NULL**; the
  CHECK forces both-or-neither and the immutability trigger forbids adding a binding later. **B13-B must close the NULL arm in
  the same change that makes the request path always bind** (a follow-up migration whose number the architect assigns, or an
  amendment of 0123 before it merges). A NULL binding stays dispatchable only to Synthetic adapters (A-11; tiering predicate in
  Go). This is the single place where B13-A is weaker than the final design; it is deliberate and pinned by
  `TestBinding_InsertGuard`.
- **B13A-3 (rails).** Seed `allowed_rails`: `bank_account` {bank_transfer, sepa, faster_payments, pix, spei}; `card_token`
  {card}; `ewallet_account` {ewallet}; `crypto_address` {crypto}; `synthetic_test` {synthetic, card, bank_transfer, ewallet}.
  The repo has no rail vocabulary (payment methods are free strings); a new rail is a migration.
- **B13A-4 (masks and the PAN detector).** Masks: IBAN/account `CC****` + last 4; e-wallet email `x***@domain`;
  e-wallet id `***` + last 4; crypto first 6 + `...` + last 4; card `network ****` + last 4; synthetic `synthetic:<label>`. The
  detector refuses a Luhn-valid 12-19 digit string in any string value, key or bare number of any kind. The unit is the
  **maximal digit run** (single spaces or hyphens allowed between digits); a longer run is not windowed, otherwise most
  IBANs would be refused. Known over-refusal: a bank detail whose only digit run is a Luhn-valid 12-19 digit string (about
  1 in 10 such runs) is refused; this is the safe side of "no PAN ever" (L-8).
- **B13A-5 (seal coverage widened).** The seals cover the ADR column lists **plus**: `display_mask`, `detail_key_kid`,
  `detail_schema_version` (instrument; otherwise a tampered mask would mislead approvers and the AEAD key id could be
  downgraded), `verifier_reference_hash` (verification), `actor_id` and `reason_code` (blocking event), `verified_at` and
  `display_mask` (snapshot). They only add coverage; every ADR column is covered.
- **B13A-6 (keys).** Variables: `PAYOUT_INSTRUMENT_KEYS` / `_ACTIVE_KID` and `PAYOUT_INSTRUMENT_FP_KEYS` / `_FP_ACTIVE_KID`.
  `config.Load` refuses a key equal to a JWT secret, an actor-proof key, `PROVIDER_CREDENTIAL_FINGERPRINT_KEY`, or a key shared
  across the two families. Webhook secrets are per-tenant values in the secret store, not process configuration, so
  "different from any webhook secret" and "unique per environment" are **operational** requirements (runbook), not
  machine-checked. Absent keys are valid in `Load`; the startup gate (`VerifyStartup`) requires them in production or when any
  non-Synthetic payout adapter or verifier is registered; otherwise the routes answer 503 (no keyless mode, no random key).
- **B13A-7 (verification flow).** Registration commits the pending instrument, then verifies synchronously through the first
  registered verifier that supports the kind and rail; the vendor call is made **outside any transaction**, after the seal /
  AEAD / fingerprint integrity check, and the verification row + state change are written in one transaction. `Verify`
  accepts only `pending_verification` and `verification_expired`: **a `suspended` instrument is not re-verifiable** (the DB
  allows `suspended -> verified` only with a same-transaction verification, but no B13-A code path offers it; owner decision
  7: no staff unsuspend). A rejecting outcome moves `pending_verification -> rejected`; a rejected re-verification of an
  expired instrument records the rejected row and leaves the instrument expired.
- **B13A-8 (blocked destinations).** Because there is no unblock path, `Register` refuses (generic 409, audit reason
  `destination_blocked`) a destination for which **any staff or provider blocking event exists** in the tenant; otherwise a
  player could revoke a compliance-suspended instrument and register the same destination again. A player's own revoke does
  not block. Whether and how a block is ever lifted is a policy question (12.3).
- **B13A-9 (KYC verified).** "The Person's KYC status is verified" = the **latest** `kyc_verifications` row of the player
  account (tenant and brand) is `approved` and unexpired; a newer non-approved row, including an orphan, denies
  (fail-closed). Cross-tenant Person verification is invisible under RLS and is not consulted.
- **B13A-10 (max-age table RLS).** `payout_instrument_verification_max_age` is `ENABLE` but **not `FORCE`** row-level
  security, with a read policy and no runtime write grant: a FORCEd table with a SELECT-only policy would lock the owner/
  migration role out of the only writer it is meant to have. The runtime role is bound by RLS and has no write grant (pinned).
- **B13A-11 (staff reach).** The staff routes are tenant-scoped (tenant from the verified token). `platform_admin` holds the
  static `payout_instrument:read` per section 2.9 but, having no tenant context, gets 403 on these routes (same shape as
  `sportsbook_bet:read`). No staff create / verify / unsuspend / edit route exists (a test pins 404/405).
- **B13A-12 (rate limit).** Per player, burst 5, refilling 10 per hour, in-memory per replica (ADR 0097 `GCRALimiter`
  admission pattern: effective limit = limit x replicas). Refused attempts consume budget. Parameters are code constants.
- **B13A-13 (idempotent registration).** Registering the same destination (after normalisation) for the same player while a
  live instrument exists returns that instrument (200, not 201); a concurrent double registration yields exactly one row (the
  partial unique index plus a savepoint). A destination change is a new instrument (`supersedes_instrument_id`).
- **B13A-14 (supersession timing).** The old instrument becomes `superseded` in `Service.Sweep`, once the replacement is
  `verified` **and** no live withdrawal binds the old one (the DB refuses otherwise, PI018). It is not done inside `Verify`,
  because an in-use refusal there would fail the replacement's verification.
- **B13A-15 (blocking-event and verification time).** `verified_at` / `occurred_at` are DB-forced to equal the transaction
  time `now()`; Go reads `SELECT now()` first and seals that value. `created_txid` is DB-forced to `txid_current()`.
- **B13A-16 (repo hygiene).** `.git/info/exclude` in this repository excludes `/migrations/0123_*` (and `0122_*`) from
  `git status`; the 0123 files are committed with `git add -f`. The orchestrator should remove those exclude lines when it
  merges.
- **B13A-17 (provider ids).** `payoutinstrument.MockProviderIDs = {"mock-payments"}` equals the literal in 0123's up-time
  assertion; `cmd/platform-api` pins it equal to the ids of the `providerkind.Synthetic` payment adapters the binary registers.

- **B13A-18 (repo-wide catalogue pins).** The `internal/db` catalogue tests (A-18 NULL-arm scan, K2-G1 acting row visibility,
  the KYC-worker reference allowlists) classify every table. Two ordered appends were made to their reference allowlists
  (`a18SelectAllowlist`, `workerReferenceAllowlist`) for `payout_instrument_kinds` and `payout_instrument_verification_max_age`
  (family-R reference tables, no tenant data). The 0123 policies are written as static statements, not a `DO` loop, so the
  A-18 scan can see them.
- **B13A-19 (`migrate verify`).** On this branch alone `migrate verify` reports `GAP missing migration version 122` (0122 is
  PAY-RECEIPT-ANOMALY-APPLIED-1, another workstream); `migrate up` is unaffected. Merge strictly in number order; after 0122
  merges the report is clean.

### 12.3 Items for the architect / human (none changes decisions 1-8; each fails closed today)

1. **Closing the NULL-binding arm (B13A-2)** needs a migration number or an amendment of 0123 before it merges. B13-B is
   blocked on this choice, not on code.
2. **Lifting a staff/provider block (B13A-8).** Owner decision 7 forbids a staff unsuspend route; the consequence is that a
   suspended or provider-revoked destination is unusable for that tenant **permanently** until a governed (four-eyes,
   proof-bound) writer exists. HD-R15-5 covers the stranded-withdrawal side; the registration side is new. Fails closed.
3. **Compliance alert on a fingerprint conflict (M-7).** `alerting` Kinds are database-checked (migration 0110); a new Kind
   needs its own migration and attribute trigger. Until then the conflict is an audit row only.

### 12.4 Evidence

Tests (runtime role, scratch database): `internal/payoutinstrument` (unit + integration), `internal/httpserver`
(`TestPayoutInstrumentRoutes_*`), `internal/auth`, `internal/config`, `cmd/platform-api`. Mutation-kill evidence:
`docs/plans/prh2-hardening-round/prh2-r16-b13a-mutation-kill.txt`.
