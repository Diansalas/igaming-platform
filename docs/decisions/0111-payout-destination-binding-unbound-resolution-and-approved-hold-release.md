# ADR 0111 — Payout destination binding (B13), governed resolution of unbound payouts (PAY-PAYOUT-UNBOUND-RESOLVE-1) and four-eyes release of frozen `approved` holds (HSEC-APPROVED-HOLD-RELEASE-1)

- **Status: PROPOSED — DESIGN ONLY, revision 4 (`architect`, 2026-10-08).** Revision 4 applies the `security`
  co-ruling on §4 (S-1..S-8; 0125 MAY START; BC-2 co-signed; HD-R15-8 resolved by sealed imports; §14). Revision 3 applies the `ledger-finance`
  pre-0125 wording conditions (BC-1, R-1..R-5) and records BC-2, LOW-1, P-2 and notes (§13); LF: 0125 may start once
  these are in. Revision 1 (`30cec95`/`978a58b`) was
  reviewed: `ledger-finance` APPROVE WITH CONDITIONS for §2 (B13) and §6 (HSEC), **§4 (RESOLVE-1) REJECTED AS
  WRITTEN** until C-1..C-7; `security` APPROVE WITH CONDITIONS. Revision 2 writes every condition into the text;
  §11 maps each one to the section changed. `ledger-finance` then ruled that rev 2 §4 satisfies C-1..C-7 in
  substance and that 0125 may start once the revision-3 wording fixes are in (done). This ADR itself makes no code,
  migration or registry change; apart from the §12 tightenings (implemented against MOCK by another agent), every
  deliverable here is `NOT IMPLEMENTED`.
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
  - `provider_reference_at_submission` (R-6 pinning, DB-forced from the attempt: NULL for the three unbound reasons,
    the bound value for `destination_mismatch` not-paid).
- CHECKs on M4 rows: `operation = 'payout'`; `target_state IS NULL`; `provider_id`, `evidence_ref_hash`,
  `evidence_line_id`, `evidence_verdict` and `evidence_import_ids` NOT NULL; `basis_code =
  'provider_confirmed_out_of_band'`; `finding_code` and `reserved_provider_tx_id` NULL; `(kind =
  'm4_evidence_paid') = (evidence_reference IS NOT NULL)`.
- **DB insert trigger (M-10):** `m4_evidence_paid` is refused when `terminal_reason = 'destination_mismatch'`; M4 is
  refused for any reason outside §4.1, and for an unbound reason with a non-NULL `provider_reference`.
- **Amount and asset are DB-forced (R-4).** The M4 row's `amount` and `asset_code` are copied from the attempt by
  the insert trigger (a client value is refused), and both the insert trigger and execution refuse unless
  `withdrawal_requests.amount` and `asset_code` equal the attempt's. So an M4-paid can never post a completion of an
  amount other than the one evidenced, nor one that would never clear under I-1 (§4.6).
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
    (B13 seal subkey, label `b13-import-v1`). Its input starts with the domain tag (S-5): `canon("imp", import id,
    tenant, provider, source_label, is_mock, coverage_start, coverage_end, content_digest, line_count, fetched_at,
    payout_lines_carry_merchant_reference, imported_by_service, lines_digest)`.
  - **`lines_digest` (S-5)** is the SHA-256 over the canonical encoding, in `line_no` order, of every line column the
    verdict or reconciliation reads: `line_no`, `kind`, `provider_reference`, `merchant_reference`,
    `original_provider_reference`, `settlement_reference`, `status`, `amount`, `asset_code`, `occurred_at`. Any column
    added to the verdict later must join the digest in the same change.
  - **The seal is written in the INSERT itself (S-5).** Imports and lines stay immutable; there is no UPDATE path.
  - **Retired seal key ids (S-5)** stay verify-only while any import references them. The §2.2 startup gate also
    fires whenever any non-MOCK `PaymentStatementSource` is registered.
  - **Dependency (S-5):** the import seal uses B13-A's key module (`internal/payoutinstrument` key handling), so
    **0123 / B13-A merges before 0125**.
  - New import columns: `import_seal`, `seal_kid`, `payout_lines_carry_merchant_reference BOOLEAN`,
    `imported_by_service`.
  - **Source declaration and channel (S-3).** `payout_lines_carry_merchant_reference`, `is_mock`, the provider id and
    the coverage semantics come **only** from the in-process Go `StatementSourceRegistry` / configuration, never from
    a DB row the runtime role can write. The importer copies them into the import row and the seal. The statement
    endpoint and credentials come from the secret store or from governed, proof-bound configuration. Fetching uses
    an authenticated channel, and the PSP's content signature is verified wherever the PSP offers one. **S-3 is
    launch-blocking for non-MOCK M4, per real source (PROVIDER DEPENDENT).**
  - **An import without a valid seal is not eligible M4 evidence.** The Go executor verifies the seal and recomputes
    `lines_digest` for every id in `evidence_import_ids` before insert and again at execution.
- **T10 (forgeable statement lines)** is recorded in ADR 0110 §10a. It is resolved by **sealed imports** (option (b),
  HD-R15-8 resolved), subject to security review of the implementation. The §10.3 T10 flag stays until three things
  hold: the seal is implemented; it passes security implementation review with mutation evidence; and S-3 is met for
  the real source. **T10 residuals (S-3):**
  - **T6:** the seal key is in process; a compromised application host can seal anything.
  - **Source authenticity and completeness:** the seal proves what was fetched, not that the source is genuine or
    complete. That is S-3's job.
  - **Availability:** forged unsealed lines can push the stream over the I-2 cap. That stops reconciliation of the
    whole tenant × provider stream, deposits included, with a loud P1, but it never clears anything. The LOW-1
    runbook entry must say: **treat overflow as possible tampering until it is proven to be re-delivery.**

  These residuals are folded into the D-7 acknowledgement (A-13 evidence standard).
- **Approver view:** shows each evidence line's provenance (import id, `imported_by_service`, `content_digest`,
  `fetched_at`, seal status). Approvers must confirm the line against the **provider portal**; `evidence_ref_hash`
  binds that confirmation.

### 4.4 Deterministic evidence (`payout_m4_evidence(p_tenant, p_attempt) RETURNS TABLE (verdict, line_id, reference, import_ids)`)

STABLE, `search_path` pinned, no SECURITY DEFINER. It reads persisted `kind = 'payout'` lines of this tenant and
provider:
- by `merchant_reference = attempt.merchant_reference` (platform-issued, unique per tenant, immutable);
- for `destination_mismatch`, also by the attempt's bound `provider_reference` (C-6);
- also on any typed reference evidence Y of the attempt (`payment_attempt_reference_evidence`).

**Bounded (I-2, confirmed; S-4):** the 64-line bound covers **every** line `payout_m4_evidence` reads, across all
lookups, **including the lookups by R**. R comes from statement content, so it gets no budget of its own. The 65th line
makes the verdict `evidence_overflow`, which is refused loudly with an audit row and never truncated.
`evidence_import_ids` = the imports of the lines actually read (so at most 64). **Eligible import** = sealed (above)
and (`is_mock = false`, or no `is_mock = false` import exists for the tenant and provider). The paid verdict's single
`succeeded` line must come from an eligible (sealed) import. Lines from unsealed imports **never count as positive
evidence**, but they still make the verdict `contradictory` or `insufficient` when they contradict.

| Verdict | ALL of the following |
|---|---|
| **`paid`** (C-5) | after cross-import dedupe on (provider_reference, status, amount, asset_code, occurred_at) there is **exactly one** `succeeded` line, with reference R; its `amount = attempt.amount AND asset_code = attempt.asset_code` (I-1); no `declined`, `reversed` or `pending` line exists on the merchant reference, on R or on any Y of the attempt in **any** import (MOCK included); R carries no reserved prefix; `payment_y_attributable(tenant, provider, attempt, R)` (0119) is true, together with the stricter tombstone exclusion (no tombstone ledger row on (provider, R)); no other attempt holds R as `provider_reference` **or as typed Y**; if this attempt has a Y and Y ≠ R, the verdict is `contradictory`; no `withdrawal_requests.provider_reference = R`; no `ledger_transactions` row with (provider, `provider_tx_id = R`) of any type; **and no `ledger_transactions.idempotency_key = provider_id || ':' || R`** |
| **`not_paid`** (H-1) | (i) an eligible import of this provider with `coverage_start <= attempt.created_at` and `coverage_end >= attempt.last_sent_at + payments.DefaultSettlementWindow` (24 h; NULL `last_sent_at` → `insufficient`); (ii) a `declined` line with equal amount and asset whose `occurred_at >= attempt.last_sent_at`, attributed by the merchant reference or by the attempt's bound `provider_reference`; (iii) **no** `succeeded`, `pending` or `reversed` payout line in **any** import (MOCK included) on the merchant reference, on the attempt's `provider_reference`, or on any Y of the attempt; (iv) the import used for (i) and (ii) has `payout_lines_carry_merchant_reference = true`, otherwise the verdict is `insufficient` |
| `insufficient`, `contradictory`, `evidence_overflow` | anything else |

> **Review-driven amendment (RESOLVE-1 review, 2026-10-09; security H-1/H-2, ledger-finance C-1; §17.7).** Both
> amendments only tighten toward refusal and stay inside owner decisions 9-12 (positive attribution only, never
> automatic). **(a) not_paid (iii)** also covers the PSP reference D of **every** payout line matched by the merchant
> reference, the bound reference or a Y (any status - so at least the declined line's own reference): no `succeeded`,
> `pending` or `reversed` payout line on any such D in **any** import. A succeeded line on D makes the paid branch read
> the declined line on the same reference, so the verdict is `contradictory`; a pending/reversed one gives
> `insufficient`. These D lookups are statement content, so they spend the same 64-line budget (S-4). **(b)** If an
> import whose lines were read **declares** `payout_lines_carry_merchant_reference` yet holds a payout line with a NULL
> merchant reference, the declaration is contradicted and not_paid is `insufficient`. **(c) paid**: if any payout line
> on R (any status, any import, MOCK and unsealed included) carries a non-NULL merchant reference other than the
> attempt's, R is not unambiguously this attempt's and the verdict is `contradictory` (the cross-import dedupe would
> otherwise collapse two parks' lines on one R into a single group and read `paid` for both).
>
> **Bound exception, named (LF RR-3, r21; §19).** Exactly **one** check in `payout_m4_evidence` reads outside the
> 64-line bound: the **import-wide declaration-integrity check** of amendment (b) above (0125, the `EXISTS` over
> `payment_statement_lines l JOIN payment_statement_imports i ... WHERE l.import_id = ANY (v_ids) AND l.kind = 'payout'
> AND l.merchant_reference IS NULL AND i.payout_lines_carry_merchant_reference`). It scans every payout line of each
> import in the read set (at most 64 imports, of any size), not only the lines the lookups matched. It is
> **refusal-only**: it can turn a would-be `not_paid` into `insufficient`, never produce or widen a verdict, and it
> runs only after the bounded reads have passed. Every other read in the function is inside the 64-line bound (S-4).

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
- **Approvers (A-14, D-1; S-6):** K3 rules plus **at least one `platform_acting` approver** on both M4 kinds,
  enforced **in the DB recount** (`payment_manual_resolution_execution_status`), not only in Go. Test: a resolution
  with two tenant-scope approvers and no `platform_acting` approver is refused at `→ executing`. The capability
  description in the back office and in the registry states that `payment_force_resolve` covers M4 (D-9 accepted by
  security).
- **Execution** (ADR 0101 §6.3 lock order):
  1. L1 `LockSubmittedForResolution`.
  2. Attempt `FOR UPDATE`.
  3. Resolution `FOR UPDATE`.
  4. Approval insert.
  5. Staff, then grants, `FOR SHARE`.
  6. **Re-evaluate first (S-2).** Run `payout_m4_evidence` again.
  7. **Then** Go verifies the seals, and recomputes `lines_digest`, of exactly the `import_ids` that step 6
     returned, and refuses unless that set equals the pinned `evidence_import_ids`. Verdict, pinned line and R must
     be unchanged. Attempt state and `terminal_reason` must equal the
     pinned values, and `provider_reference IS NOT DISTINCT FROM provider_reference_at_submission` (NULL for the
     three unbound reasons; the bound value for `destination_mismatch` not-paid) (C-6, R-3). The withdrawal must be
     `submitted`, and its `amount`/`asset_code` must equal the attempt's and the resolution's (R-4). Otherwise
     `refused_at_execution`.
  8. `executing` → posting → `executed`; link the ledger row; audit; commit. **The `pending → executing` move in the
     0115 resolution guard re-runs `payout_m4_evidence` and the R-3/R-4 checks IN THE DB** (verdict, line, R and
     import set equal the pinned values; `provider_reference IS NOT DISTINCT FROM provider_reference_at_submission`;
     withdrawal and attempt amount/asset equal the resolution's), mirroring M2's R-6 re-check at 0115:597-611. Go is
     the first check, not the only one. The seal check alone stays in Go, because the key never enters the DB.
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
    - the attempt's state and `terminal_reason` equal the pinned values, and `provider_reference IS NOT DISTINCT
      FROM provider_reference_at_submission` (R-3).
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

### 4.6 Reconciliation (C-4; 0125 is NOT migration-free) — revision 3

- 0125 widens `tenant_system_read_executed` (0115:1419) to the M4 kinds; `loadK3Evidence` also loads executed M4
  rows.
- **Key set for executed M4 rows (S-1).** For every executed M4 row, `loadK3Evidence` adds these keys to the lookup
  key set: `evidence_reference` (R), the attempt's merchant reference, its bound `provider_reference` (if any), and
  every Y. All of them are platform state, so this is consistent with I-2: keys are fixed before the read, and each
  gets the 64-line budget. Tests:
  - a contradicting line that names **only R** raises;
  - a line on the bound reference after a `destination_mismatch` not-paid raises;
  - a mutant that drops the key additions is killed.
- **New predicates (no new mismatch kind, no kind-CHECK change; R-1):**
  - an executed `m4_evidence_paid` raises `pay_declared_paid_unconfirmed` on **any** `declined` or `reversed` line on R
    or on the merchant reference, or on a **second distinct** `succeeded` line, **in any import, irrespective of
    `occurred_at` or import time** (a back-dated contradiction counts);
  - an executed `m4_evidence_not_paid` raises `pay_declared_not_paid_but_paid` on **any** `succeeded` line on the
    merchant reference, the bound reference or a Y, **in any import, irrespective of `occurred_at` or import time**.
  - *Review-driven amendment (2026-10-09; security H-1/H-2, ledger-finance C-1; §17.7):* the not-paid predicate also
    raises on a `succeeded` line on the evidence line's own reference D (derived in `loadK3Evidence` from
    `evidence_line_id` and added to the S-1 key set), and the paid predicate also raises on any payout line on R that
    names another merchant reference. Raising only; positive attribution only; nothing clears automatically.
  - *r21 amendment (§19):* the not-paid predicate covers **every** matched reference (the verdict's `v_rs`), within
    the fixed I-2 budget; an invisible evidence line fails the run closed; and the stop condition of both the not-paid
    predicate and STANDING-1 for that attempt is the single ledger-finance RR-1 rule.
  - *r30 amendment (§21; owner decisions 1 and 2 of 2026-10-09, ADR 0095 §48):* the recovery of RR-1 and of M2 (d) is NET
    (debits minus credits under the causation, reversed debits excluded), and RR-1 also stops the BOUND finding of a recovered
    `destination_mismatch` park, only on the complete predicate.
- **M4-paid completions in the ledger join (R-2).** An M4-paid completion leaves the attempt `disputed` (A-15). The
  existing `ledger_join` check (`internal/reconciliation/payment_statement.go`, "every deposit / withdrawal_completed
  posting in the window maps to exactly one succeeded attempt", ~1428-1465) counts only `succeeded` attempts, so
  without a change it would raise `pay_missing_platform_record` for **every** M4-paid completion on every run.
  **Ruling (`ledger-finance`):** a `withdrawal_completed` posting `t` counts as attributed (n = 1) **only** when an
  executed `m4_evidence_paid` resolution has `ledger_transaction_id = t.id` **and** the same attempt's withdrawal has
  `release_ledger_transaction_id = t.id`. Nothing else qualifies (not the reserved-namespace M2 key, not a shared
  reference, not a pending or refused resolution). Required 0125 change: the matcher loads executed M4 rows (through
  the widened `tenant_system_read_executed`) and adds that one case to `succeededFor`; a posting attributed both ways
  (a succeeded attempt **and** an M4) is n = 2 and still raises. Tests: an M4-paid completion raises no
  `pay_missing_platform_record`; one with a mismatched link (resolution points at t, withdrawal does not, or the
  reverse) still raises; a non-executed or M2 resolution never qualifies; double attribution raises; a tenant-B M4 row
  never attributes a tenant-A posting; mutation evidence.
- **I-1, implemented shape (BC-1; §12).** `payoutCompletedRef` keeps positive attribution (own release keyed by the
  reference, no other holder) and additionally requires the completion's `psp_clearing` amount and its **single**
  asset to equal the attempt's, at **every** payout site (bound and unbound). Where the finding is keyed on an
  evidencing line (unbound in-run, merchant cross-check B step, standing), that line's amount and asset must also equal
  the attempt's. A completion whose `psp_clearing` amount or asset resolves to `<none>` or `<multiple>` fails closed
  (never clears).
- **I-2, implemented shape (BC-1; §12).** The persisted-lines read is bounded per run by **cap = 64 per lookup key ×
  the number of distinct lookup keys, fixed from platform state (parked attempts and their references) BEFORE the
  read**. The SQL reads at most cap + 1 rows. Overflow fails the run with `ErrPaymentEvidenceOverflow` plus the
  existing P1 `reconciliation.sweep_run_failed` audit and run-failure alert: no run row, no mismatch row, no
  truncation, no partial verdict. Statement content can never raise its own budget, because the key count comes only
  from platform rows. INV-M-5 is kept. Shared with CAS-RECON-SCALE-1. **The formula awaits the `security` co-signature
  (BC-2, §10.4).**
- **Hints (L-4).** The unbound-park hint names M4 (implemented, §12). When M4 ships, 0125 **drops "NOT IMPLEMENTED"**
  from that hint and states **"MOCK only"** while the §10.3 T10 flag stands. After an executed M4 not-paid, the
  `pay_captured_unposted` hint reads "possible double payout after M4 not-paid: recovery via compensating debit (K2) or
  off-platform recovery; never allocation" (needs executed M4 rows, so it ships with 0125).

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
| **RESOLVE-1, everything else** (kinds, evidence function, import seal and policy narrowing, acting SELECT, fences, MR041, §4.8 cells, 0125) | **May start (security co-ruling, revision 4):** LF and security pre-0125 conditions applied; BC-2 co-signed. Merges **after 0123 / B13-A** (import seal uses its key module, S-5) and needs the full §8 gates. Non-MOCK M4 stays blocked by the T10 flag (§10.3) |
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
- **HD-R15-8 — RESOLVED (revision 4, security co-ruling).** Option (b), sealed imports (§4.3), subject to security
  review of the implementation. No human acceptance of T10 is needed. The T10 residuals (§4.3) are folded into the
  existing D-7 acknowledgement (A-13 evidence standard).
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
| Security M-5 / T10 | any non-MOCK M4, until the import seal is implemented, passes security implementation review with mutation evidence, **and** S-3 is met for each real source (PROVIDER DEPENDENT) |
| ADR 0110 T3/T4 (identity-store integrity) | real money, as already recorded |
| HD-R15-1 | any sandbox/real payout |
| HD-R15-5 (LF L-7) | any non-MOCK payout |
| LF C-3 (section 20.4): real-provider status mapping for the post-M4 cells (PROVIDER DEPENDENT) | any non-MOCK M4 paid / not-paid, see section 20.4 |

### 10.4 Conditions and follow-ups recorded by revision 3 (`ledger-finance` pre-0125 verdict)

These are not human policy decisions unless stated; they are review conditions and follow-ups.
- **BC-2 — CO-SIGNED by `security` (revision 4).** Was: condition before 0125 merges: `security` co-signs the I-2 formula of §4.6 (64 per lookup key × distinct
  keys fixed from platform state before the read; cap + 1 read; overflow fails the run). This answers §12 P-1
  subject to that co-signature.
- **LOW-1 / RM-4 (follow-up, real-source onboarding):** confirm each real statement source's re-delivery pattern.
  Overlapping imports count toward the cap, so a real source that re-delivers the same lines on every fetch could
  overflow with no attacker. Overflow refuses the **whole tenant × provider stream, deposits included**, loudly. Add an
  operator runbook entry for `ErrPaymentEvidenceOverflow` (cause, impact, how to diagnose, who may change the cap and
  under which review). PROVIDER DEPENDENT.
- **P-2 / RM-3 (follow-up):** a standing persisted-line amount/asset check for **bound** payout parks that have an
  own completion (today the line raises `pay_amount_mismatch` only in-run). Linked to RM-3 (`psp_clearing` vs PSP
  settlement reconciliation). Owner `payments` + `ledger-finance`.
- **0125 hint condition:** drop "NOT IMPLEMENTED" from the M4 hint when M4 ships; say "MOCK only" while T10 stands
  (§4.6).
- **Non-blocking note (PROVIDER DEPENDENT):** the paid verdict's rule "no `pending` line on R or the merchant
  reference in any import" makes M4-paid **unusable** with a PSP whose statements list `pending` before `succeeded`.
  This is safe (fail closed); revisit with the real source under an LF ruling.
- **Non-blocking note:** the 24 h `DefaultSettlementWindow` used in not-paid condition (i) must be re-checked **per
  rail** before any non-MOCK M4 (some rails settle or return later).
- **Onboarding note (LF RR-3, r21; PROVIDER DEPENDENT).** A real statement source that **declares**
  `payout_lines_carry_merchant_reference` but emits **any** payout line without a merchant reference makes **M4 not-paid
  unavailable for every park whose evidence reads that import** (§4.4 amendment (b): the import-wide declaration check
  makes the verdict `insufficient`). Likewise a source that lists a `pending` line before the `declined` one on the
  merchant reference, the bound reference, a Y or any matched reference makes not-paid `insufficient` (§4.4 (iii)).
  Both are fail-closed (no wrong release), but they are availability losses: confirm, per real source, whether every
  payout line carries the merchant reference and whether `pending` is ever listed before a decline, before declaring
  the flag and before relying on M4 not-paid for that source.
- **S-8 (follow-up, revision 4; pre-existing gap):** freeze `withdrawal_requests.release_ledger_transaction_id` once
  it is non-NULL. Today the 0026 immutable-fields trigger does not cover it. Tampering would only cause a loud raise
  (`ledger_join` / I-1), never a clear. Owner `payments` + `ledger-finance`; a migration-sized change, not part of 0125
  unless LF folds it in.
- **D-7 widened (revision 4):** the D-7 acknowledgement (A-13 evidence standard) now also covers the T10 residuals of
  §4.3 (T6, source authenticity/completeness, availability).
- No new human decision arises from revision 3. The open human items remain §10.1 (HD-R15-1..9), §10.2 (D-1..D-9) and
  the §10.3 launch flags.

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

## 12. RESOLVE-1 tightenings implemented (`ledger-finance`, 2026-10-08)

**Status: IMPLEMENTED against MOCK** (the §7.3 "may start now" slice only; no migration; `manual_resolution.go` and
`migrations/` untouched). **RESOLVE-1 M4 itself remains NOT IMPLEMENTED** (kinds, `payout_m4_evidence`, sealed imports,
acting SELECT, fences, MR041, §4.8 cells, 0125): it still waits for the `ledger-finance` re-review of §4 and the
`security` T10/M-5 co-ruling. Owner decisions 9-12 (ADR 0095 §44) are not broadened: nothing here clears, releases,
settles or posts anything; every change only narrows clearing, refuses a run, or changes operator text.

- **I-1 (§4.6; security I-1 / LF C-4).** `payoutCompletedRef` (`internal/reconciliation/payment_statement_k3.go`) keeps
  the positive attribution (own release keyed by the reference, no other holder) and additionally requires the
  completion's `psp_clearing` amount and its single asset to equal the attempt's, at **every** payout site (bound and
  unbound). Where the finding is keyed on an evidencing line (unbound in-run, merchant cross-check B step, standing),
  that **line's** amount and asset must equal the attempt's too. The bound sites (`capturedUnposted`: matchPayment's
  bound case and `checkUnmatchedAttempts`) have no evidencing line by construction (the finding is keyed on X and the
  standing site has no line), so there the completion is compared; a bound line of another amount still raises
  `pay_amount_mismatch` in-run (interpretation, see policy question P-2 below).
- **I-2 (§4.6; security I-2).** `loadK3Evidence`'s persisted-lines read is bounded by a hard per-run cap fixed before
  the read: **64 per lookup key** (the §4.4 figure security confirmed) × the number of distinct provider/merchant
  references the run needs. The query reads at most cap+1 rows; the (cap+1)-th row fails the run with
  `ErrPaymentEvidenceOverflow`: no run row, no mismatch row, ledger untouched, reported by
  `ReconcilePaymentStatementForTenant` as the existing P1 `reconciliation.sweep_run_failed` (phase `match`) plus the
  run-failure alert, on every run until resolved. Never a silent truncation, never a partial verdict. INV-M-5 kept.
- **L-4 hint (§4.6, partial).** An unbound payout park **inside §4.1 scope** (`invalid_provider_reference`,
  `invalid_provider_reference:*` or `provider_reference_conflict`, **with NULL provider reference**, C-6) now reads
  "resolution: PSP-side recall/return, or the evidence-backed four-eyes resolution M4 (PAY-PAYOUT-UNBOUND-RESOLVE-1,
  ADR 0111 §4; NOT IMPLEMENTED); never allocation; M1 only acknowledges". Bound payout parks and unbound reasons that
  hold a reference keep the R-K3-8 wording; deposits keep F13. The **post-M4-not-paid** hint is **NOT IMPLEMENTED**:
  it needs executed M4 rows, which cannot exist before 0125 widens `tenant_system_read_executed`.
- **Pins flipped deliberately.** `b11RequirePayoutCU` (`internal/payments/b11_payout_unbound_hold_integration_test.go`)
  expects the M4-scope wording for parks with no reference (sync, reverse collision) and the R-K3-8 wording for the poll
  park (holds X). No STANDING-1 / BOUND-CLEAR-1 clearing pin changed: every existing own-completion test uses equal
  amounts.
- **Tests:** `internal/reconciliation/prh2_r16_res1_tighten_integration_test.go` (`TestRes1_*`, runtime role).
- **Evidence:** `docs/plans/prh2-hardening-round/prh2-r16-res1-tighten-mutation-kill.txt` (23 counted mutants: 20
  killed, 3 equivalent survivors disclosed: I1-h, I2-f, L4-e). LOCAL evidence only.
- **Policy questions (for the LF re-review / security):** **P-1** the I-2 cap value and shape (64 per lookup key,
  per run) versus a fixed absolute per-run number: §4.6 gives no figure; a fixed 64 per run would fail ordinary runs of
  any tenant with a few parks. **P-2** whether a bound payout park should also refuse to clear while an in-run line
  naming X carries another amount (today: cleared by an equal own completion, the line raises `pay_amount_mismatch`).
- **Revision 3 note (`architect`).** P-1 is answered by the §4.6 I-2 formula, subject to the `security` co-signature
  (BC-2, §10.4). P-2 is recorded as a follow-up linked to RM-3 (§10.4).

## 13. Revision 3 changes (`architect`, 2026-10-08; `ledger-finance` pre-0125 wording conditions)

Text only. Owner decisions (ADR 0095 §44) are not broadened. §12 (another agent) is kept unchanged apart from the
trailing note above.

| Item | Change | Section |
|---|---|---|
| BC-1 / R-5 | implemented shapes of I-1 (completion `psp_clearing` amount + single asset at every payout site; line amount/asset where line-keyed; `<none>`/`<multiple>` fail closed) and I-2 (64 per key × keys fixed before the read; cap + 1; `ErrPaymentEvidenceOverflow` + P1; no truncation; content cannot raise its budget) | §4.6 |
| R-1 | predicates count any contradicting line in any import, irrespective of `occurred_at` or import time | §4.6 |
| R-2 | M4-paid completions attributed in `ledger_join` only via executed `m4_evidence_paid` with `ledger_transaction_id = t.id` and the withdrawal's `release_ledger_transaction_id = t.id`; 0125 matcher change and tests | §4.6 |
| R-3 | step 7 and MR041 use `provider_reference IS NOT DISTINCT FROM provider_reference_at_submission` | §4.2, §4.5 |
| R-4 | M4 amount/asset DB-forced from the attempt; insert and execution refuse unless the withdrawal's amount/asset equal the attempt's | §4.2, §4.5 |
| BC-2 | security co-signature of the I-2 formula before 0125 merges | §10.4, §4.6 |
| LOW-1 / RM-4 | real-source re-delivery pattern; whole-stream overflow impact; runbook entry | §10.4 |
| P-2 / RM-3 | standing amount/asset check for bound parks with an own completion | §10.4, §12 note |
| 0125 hint | drop "NOT IMPLEMENTED", say "MOCK only" while T10 stands | §4.6, §10.4 |
| Notes | pending-before-succeeded PSPs make M4-paid unusable (safe); 24 h window to be re-checked per rail | §10.4 |

## 14. Revision 4 changes (`architect`, 2026-10-08; `security` co-ruling on §4)

Text only. Owner decisions (ADR 0095 §44) are not broadened. `security`: **0125 MAY START; BC-2 CO-SIGNED; HD-R15-8
replaced.**

| Item | Change | Section |
|---|---|---|
| S-1 | executed M4 rows add R, merchant ref, bound ref and Y to the `loadK3Evidence` key set; three tests incl. a key-drop mutant | §4.6 |
| S-2 | execution re-evaluates first, then Go seals exactly the returned import ids (must equal the pinned set); the 0115 guard re-runs the evidence and R-3/R-4 checks in the DB at `→ executing` | §4.5 |
| S-3 | source declaration only from the in-process registry/config; endpoint/credentials from the secret store or governed config; authenticated channel and PSP signature; T10 residuals (T6, authenticity/completeness, availability + runbook "treat overflow as possible tampering"); launch-blocking per real source | §4.3, §10.3, ADR 0110 pointer |
| S-4 | 64-line bound covers every line read incl. R lookups; `evidence_import_ids` = imports of lines read; paid line from a sealed import; unsealed lines never positive but still contradict | §4.4 |
| S-5 | domain tag "imp" + `imported_by_service` in the seal; `lines_digest` column list; seal written in the INSERT (no UPDATE); startup gate fires on a non-MOCK statement source; retired kids verify-only; 0123 merges before 0125 | §4.3, §7.3 |
| S-6 | platform_acting approver floor enforced in the DB recount + test; capability description states M4 coverage (D-9 accepted) | §4.5 |
| S-7 | ADR 0110 header pointer aligned with §4.5 digest wording | ADR 0110 header |
| S-8 | follow-up: freeze `release_ledger_transaction_id` once non-NULL | §10.4 |
| BC-2 | co-signed | §10.4 |
| HD-R15-8 | resolved by option (b), sealed imports; T10 residuals folded into D-7; T10 launch flag conditions restated | §10.1, §10.3, §4.3 |

---

## 15. B13-A implementation notes (appendix, added with the B13-A code; the design above is not rewritten)

Scope delivered: migration `0123_payout_instruments` (the whole B13 **schema**, because section 7.2 keeps B13 as one
migration and section 7.1 says B13-B "uses 0123") and the Go package `internal/payoutinstrument` with its routes. The
Go integration of the binding into the withdrawal / payments paths is **B13-B and is NOT part of this change**.
Where the design left a sub-question open, the safest existing reading was taken and is recorded as `B13A-n`. No
policy question that changes an owner decision (ADR 0095 section 44, decisions 1-8) was needed; the three questions
the human or architect should see are in 15.3.

### 15.1 Deliverable status

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
| Compliance alert on a fingerprint conflict (M-7) | `PARTIALLY IMPLEMENTED`: audit row `payout_instrument.registration_conflict` only; an alert needs a new `alerting` Kind (a migration), see 15.3 |
| Acting-family read of snapshots under a VALID acting session | `PARTIALLY IMPLEMENTED`: the policy is pinned (SELECT only, `financial_acting_session_valid()`, snapshots only) and an invalid acting session reads nothing; a positive test needs a K1 grant world and belongs with the first consumer (RESOLVE-1) |

### 15.2 Ambiguities and the reading taken

- **B13A-1 (instrument id).** "`id` DB default; a client value is refused": the id is allocated by the database
  (`SELECT gen_random_uuid()` in the registration transaction) because the AEAD AAD and the instrument seal bind it before
  the insert. The HTTP surface refuses a client-supplied id (unknown-field rejection; tested). Reading: the *client*
  cannot choose the id; the platform's own insert path passes the id it just allocated.
- **B13A-2 (NULL binding, transitional).** Section 2.1 says the `withdrawal_requests` BEFORE INSERT trigger "requires both
  NOT NULL". Until B13-B wires the request path no code supplies a binding, so enforcing it now would break every existing
  withdrawal insert. 0123 therefore **validates a binding whenever one is supplied** (same tenant/brand/player, verified,
  in-force latest verification, no revoke, no later suspend, asset listed, fingerprint equal) and **tolerates NULL/NULL**; the
  CHECK forces both-or-neither and the immutability trigger forbids adding a binding later. **Decision (coordinator, after the security review): 0123 merges as written and is
  not amended afterwards; B13-B ships its OWN migration that replaces `withdrawal_requests_payout_binding_guard()` so it refuses
  NULL/NULL on INSERT, in the same change that makes the request path always bind.** A NULL binding stays dispatchable only to Synthetic adapters (A-11; tiering predicate in
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
  not block. Whether and how a block is ever lifted is a policy question (15.3).
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
- **B13A-16 (repo hygiene).** While B13-A was written the repository's shared `.git/info/exclude` hid `/migrations/0122_*` and
  `/migrations/0123_*` from `git status`, so the 0123 files were committed with `git add -f`; the exclude lines have since been removed
  by the orchestrator and nothing further is needed.
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

### 15.3 Items for the architect / human (none changes decisions 1-8; each fails closed today)

1. **Closing the NULL-binding arm (B13A-2)**: decided - B13-B's own migration replaces the guard (see B13A-2). B13-B also
   carries: a missing snapshot on a bound withdrawal is `destination_integrity_failure`; the tier check runs at T1p, phase B and
   T2/T12; a repo-wide check that no Synthetic marker is inherited through embedding (L-7); and removal of the L-8 startup clause.
2. **Lifting a staff/provider block (B13A-8).** Owner decision 7 forbids a staff unsuspend route; the consequence is that a
   suspended or provider-revoked destination is unusable for that tenant **permanently** until a governed (four-eyes,
   proof-bound) writer exists. HD-R15-5 covers the stranded-withdrawal side; the registration side is new. Fails closed.
3. **Compliance alert on a fingerprint conflict (M-7).** `alerting` Kinds are database-checked (migration 0110); a new Kind
   needs its own migration and attribute trigger. Until then the conflict is an audit row only.

### 15.4 Evidence

Tests (runtime role, scratch database): `internal/payoutinstrument` (unit + integration), `internal/httpserver`
(`TestPayoutInstrumentRoutes_*`), `internal/auth`, `internal/config`, `cmd/platform-api`. Mutation-kill evidence:
`docs/plans/prh2-hardening-round/prh2-r16-b13a-mutation-kill.txt`.

### 15.5 Security review of B13-A (APPROVE WITH CONDITIONS): fixes and launch flags

Fixed in B13-A (tip after this change):
- **M-1** the deferred `payment_attempts` snapshot constraint returned NULL when the withdrawal row was not found (reproduced by
  clearing `app.tenant_id` before commit): it now raises PI055 when the withdrawal cannot be read; only an explicit NULL binding
  returns. Regression: `TestSnapshotConstraint_FailsClosedWhenWithdrawalUnreadable`.
- **M-2** the PAN detector treated only one space or hyphen as a separator. A run of digits joined by ANY non-alphanumeric
  characters (dots, underscores, colons, repeated separators, NBSP, tabs ...) is now one candidate; letters still end a run.
- **L-8 (chosen: ENFORCED, not just flagged)** `VerifyStartup` refuses to start, in every environment, when any non-Synthetic payout
  adapter is registered, even with keys, until B13-B lands (it would dispatch NULL-binding withdrawals). B13-B removes the clause.
- **L-6 (done)** the database snapshot insert also requires the instrument `verified`, the snapshot's verification to be the
  in-force one, unexpired, and no revoke / later suspend event (PI054). Defence in depth; the Go gate stays primary.

**Launch-blocking for non-MOCK instruments / payouts** (recorded, not built):
- **M-3** A fingerprint-ownership claim is permanent (A-2: owner rows are never deleted) and there is no release path. A Person who
  registers a destination that is not theirs, or one they later lose, blocks every other Person in the tenant from it
  forever (a denial-of-registration vector) and nothing can correct a wrong claim. A governed (four-eyes, proof-bound) release or
  re-assignment writer is required before any real customer data.
- **L-1** For `card_token` the `psp_card_fingerprint`, `last4` and `network` are CLIENT-supplied in the registration body. They feed the
  fingerprint input (L-8 of the ADR) and the mask. Before a real card is accepted these must come from the PSP's hosted-fields
  response server-side, never from the browser body.

**Recorded residuals / known behaviour:**
- **L-2** One destination can yield several fingerprints (different normalisations or kinds, e.g. an IBAN as `bank_account` vs the
  same account entered with `country`+`account_number`; a card with and without a PSP fingerprint). Conflict detection is per
  normalised input, so equivalent destinations in different forms are not detected as the same.
- **L-5** The registration response (`200` existing vs `201` new, plus the idempotent replay) is an existence oracle for the
  caller's OWN destinations only; the cross-Person conflict response is generic. Accepted residual.
- **L-7** The Synthetic marker (`SyntheticComponent`) is inherited through struct embedding: a real type embedding a Synthetic type
  silently becomes Synthetic and would satisfy the tiering predicate. B13-B adds a repo-wide embedding check; until then the
  predicate must not be relied on for a type that embeds a mock.

### 15.6 Ledger-finance review of B13-A (APPROVE WITH CONDITIONS): fixes, notes and B13-B conditions

Fixed in B13-A (migration 0123, folded in before merge):
- **C-3 / F-4** `payout_instrument_kinds` gained `allowed_asset_type` (`fiat` for `bank_account`, `card_token`, `ewallet_account`; `crypto`
  for `crypto_address`; NULL = any for `synthetic_test`). The instrument insert trigger refuses (PI007) an asset whose registry type
  differs, so a bank account cannot list BTC and a crypto address cannot list EUR. The rule is data in the kinds table, not a code
  path keyed on a kind. Tests: `TestKindAssetTypeEnforcedByTheDatabase` (Go and direct SQL); mutant S31 killed.
- **C-4 / F-5** `LOCK TABLE withdrawal_requests IN SHARE ROW EXCLUSIVE MODE` precedes the L-6 pre-flight scan, so no row can gain a
  non-MOCK `provider_id` between the scan and the `ALTER TABLE`. Test: an open writer makes the migration wait and then refuse
  (`TestMigration0123_PreflightLockSerialisesWithWriters`); mutant S32 killed.
- **F-7** the down migration also refuses (PI099) while `payout_instrument_verification_max_age` has any row (governance
  configuration is not silently discarded). Test and mutant S33 killed.

Recorded, not changed:
- **F-6 (scale note)** `ALTER TABLE withdrawal_requests` takes an ACCESS EXCLUSIVE lock and validates the new FK / CHECK against the whole
  table. On a large table run it in a maintenance window; `lock_timeout` and `NOT VALID` + `VALIDATE CONSTRAINT` would shorten the
  lock but are deliberately not applied here (the migration is not to be amended after merge).
- **F-8 (info residual)** as recorded by the review; no action in B13-A.

**B13-B conditions (added):** call `EvaluateGate` and `CheckTier` inside `ClaimForDispatch`, before the provider call and before any
retry, resend or callback/poll that settles the attempt; and remove the L-8 startup clause in `VerifyStartup` in the SAME change that
closes the NULL arm (the guard-replacing migration).
---

## 16. HSEC implementation notes (HSEC-APPROVED-HOLD-RELEASE-1, migration 0124)

Appended by the `payments` implementer. The design above is **not** rewritten; this section records each
ambiguity chosen while implementing §6 (safest reading in every case), the objects the implementation touches
beyond the §6 list, and what the next migration (0125) must build on. Owner decisions (ADR 0095 §44, 13-18) are
unchanged: no automatic release, the hold remains, a controlled staff path exists, four-eyes for
resolution/release/cancel, no unilateral single-staff action, kill-switch semantics intact. Status:
`IMPLEMENTED` against the MOCK stack (migration `0124_withdrawal_hold_resolution`, `internal/payments/
withdrawal_hold_resolution.go`, `withdrawal.ReleaseForGovernedResolution`, routes under
`/v1/admin/tenants/{tenantID}/withdrawal-hold-resolutions`); no real provider is involved or called.

**Security conditions applied after review (C-1..C-3, F-4).**

- **C-1.** The deferred shape check (HR041) now RAISES when the resolution row is not visible to the committing session
  (it previously returned silently); pinned by `TestHSEC_HoldRelease_DeferredCheck_RaisesWhenRowNotVisibleAtCommit`.
- **C-2.** `acting_lock` on `brands` is lock-only: an acting `UPDATE brands` is refused by its `WITH CHECK (false)`
  (42501), pinned by `TestHSEC_HoldRelease_ActingBrandsPolicyIsLockOnly` and mutant S49.
- **C-3 (wording).** The approved-hold freeze covers **any state change out of `approved`** (widened trigger `WHEN`:
  `OLD.state = 'approved' AND NEW.state IS DISTINCT FROM OLD.state`), not only the three direct transitions to
  rejected/cancelled/failed; the single exemption is `approved -> rejected` of an executing hold resolution of this
  transaction. The deferred two-leg check and the freeze both read tenant, brand and resolution rows **under the
  committing session's settings** (RLS and the acting GUCs): a session that can see neither fails closed (HR041 / HR050),
  but the checks are not independent of session state. **Stated residual (ADR 0110 T5):** a runtime session with
  arbitrary SQL can still write ordinary, non-governed postings and a forged ordinary posting is bounded only by ledger
  invariants and reconciliation; these triggers stop the governed key and the frozen state edges, nothing more.
- **F-4.** Cancel, expire and reject null `tenant_status_at_execution`, `brand_status_at_execution`,
  `required_at_execution` and `contributing_policy_ids_at_execution` (tests for cancel and expire; the reject branch is
  the same statement and is reachable only through the approvals trigger).

**What 0125 must build on (replace-in-place objects).** The four shared objects of §7.2, in 0124's bodies, each
equal to the 0115 text plus exactly the marked HSEC addition: `ledger_governed_fence_allows` (+ branch (e)),
`ledger_entries_governed_fence` (+ the `withdrawal_rejected` shape, variable `v_h`), the acting `ledger_accounts`
`acting_insert` policy (+ the hold account of an executing hold resolution) and the acting `withdrawal_requests`
`acting_update` policy (+ "state = rejected and an executing hold resolution of this txid"). 0125's down must
restore these 0124 bodies, not the 0115 ones. Two further objects are also replaced in place by 0124 and a later
migration that replaces them must start from 0124's text: `financial_policy_required_approvals` (0113 body + 4
lines) and `actor_proof_require` (0120 body + the operation-table check). Their down files restore the 0113 / 0120
text byte for byte (whole-schema snapshot test).

**Chosen ambiguities.**

- **HN-1 (`payout_instrument_id`).** 0123 (B13) is a different agent's migration, so 0124 must not reference its
  column. `withdrawal_hold_resolutions.payout_instrument_id` is a plain nullable UUID (no FK) forced in the insert
  guard from `NULLIF(to_jsonb(withdrawal_requests) ->> 'payout_instrument_id', '')::uuid`, which is NULL until the
  column exists. It is a record only and is not part of `payload_hash`.
- **HN-2 (approval vs precondition re-check).** The approvals guard does not refuse an approval because the
  preconditions changed; otherwise the final approval after a reactivation could not end `refused_at_execution`
  (that transition is only legal in the final approval's own transaction). The preconditions are re-checked by the
  Go executor (`refused_at_execution`, closed codes `withdrawal_not_approved`, `attempt_exists`,
  `tenant_brand_active_again`, `policy_disabled`) and again by the database at `pending -> executing` (HR010).
  Non-final approvals record `preconditions_hold` in their audit row.
- **HN-3 (freeze exemption).** The all-sessions freeze trigger exempts only `approved -> rejected` of an executing
  hold resolution of this txid (not `cancelled`/`failed`), and an unreadable tenant or brand status counts as
  non-active (fail closed). The H-SEC gate runs before `DenyForCompliance`, so the normal KYC denial is unaffected;
  pinned by tests (gate first; active tenant still denies; non-active direct `DenyForCompliance` is HR050).
- **HN-4 (executing-window discipline; corrected after ledger-finance C-1).** Beyond §6.4, while a hold resolution is
  `executing` for a withdrawal the only admitted `withdrawal_requests` change in any session is `approved -> rejected`
  with the governed release link and no other column touched. The first revision claimed this but the guard trigger
  carried a `WHEN` clause (state or release link changed), so a second UPDATE of another column (for example
  `provider_id`) inside the executing transaction slipped through. The guard now fires on **every** update (no `WHEN`),
  and the executing branch requires `OLD.state = 'approved'`, so any later UPDATE finds `rejected` and is refused
  (HR030). The acting UPDATE policy `WITH CHECK` still only requires `state = 'rejected'` and an executing resolution;
  the trigger is the control that pins the rest (RESOLVE-1 must keep that division when it copies the policy body).
  Pinned by `TestHSEC_HoldRelease_ExecutingWindow_SecondUpdateAndRefusalRefused` and mutant S52.
- **HN-5 (brand lock inside an acting session).** The H-SEC discipline takes `brands ... FOR SHARE`; the existing
  brand policies are tenant-GUC only, so an acting session would see no row. 0124 adds one lock-only policy
  `acting_lock ON brands FOR UPDATE USING (acting tenant, valid session) WITH CHECK (false)`. This object is not on
  the §6 list; it grants no write.
- **HN-6 (policy extension).** `financial_policy_required_approvals` ignores tenant- and brand-level rows for
  `withdrawal_hold_resolution` when the tenant **or** the brand is not active (the K2-1 rule extended), so tenant
  rows can never lower or steer the platform baseline of an operation platform staff perform on a suspended
  tenant. No platform policy row => disabled (HR014), exactly as K3.
- **HN-7 (required count).** `required = GREATEST(required_at_submission, policy.required)`, counting approvals of
  distinct Persons **other than the requester**; a baseline of 1 therefore means requester + one distinct approver
  (the four-eyes floor); the requester can neither approve nor reject their own request (use cancel). The
  beneficiary (the withdrawing player's Person) is excluded at request and approval (S-12).
- **HN-8 (tenant actors).** Both new tables carry only acting-family (A) policies. The guards additionally refuse
  any non-`platform_acting` session (HR001) and the Go service refuses a tenant-scoped caller before any database
  work; `eligible_tenant_roles = '{}'` makes a tenant-role grant impossible. Static permissions are
  `RolePlatformAdmin` only (tenant roles denied, tested by a scan of all declared roles).
- **HN-9 (uniqueness).** One `pending` and one `executed` resolution per withdrawal (partial UNIQUEs); a new request
  after `cancelled`/`expired`/`rejected`/`refused_at_execution` is allowed.
- **HN-10 (proofs).** Operations `withdrawal_hold_resolution:request|cancel|approve|reject`; the verifier and the Go
  signer both refuse every scope but `platform_acting` (with a tenant) for them. A fresh proof is issued per
  attempt (request: target `new`, digest of tenant, withdrawal, kind, evidence hash, reason; approve/reject:
  resolution id + payload hash; cancel: resolution id + payload hash). `pending -> expired` carries no proof and is
  refused before `expires_at` by both the base guard and, independently, the proof trigger.
- **HN-11 (ledger key guard).** The CT-R3 trigger is right-anchored (`right(idempotency_key, 23) =
  ':governed_hold_released'`) and, besides key and correlation, requires type `withdrawal_rejected`, the same
  tenant and an executing resolution of this txid. Resolution rows are visible only to an acting session, so every
  other session fails closed. The deferred shape check (HR041) additionally checks the reversal of the hold
  transaction, no provider ids, exactly two entries, one per direction, wallet, amount and asset.
- **HN-12 (SQLSTATE class).** A new class `HR` (hold resolution) is used (HR001 ... HR099); callers classify by code
  only. Closed HTTP tokens: `hold_resolution_{disabled,not_permitted,precondition_failed,conflict,expired,
  not_found}`; every refusal writes `withdrawal.hold_resolution_denied`.
- **HN-13 (audit).** One `withdrawal.hold_resolution_<requested|approved|rejected|cancelled|expired|refused|
  executed>` row per event (actor, requester, approvers of the counted approvals, withdrawal, reason code, evidence
  hash, statuses, resulting state, ledger transaction) plus `withdrawal.hold_released_governed` from the money
  movement. The evidence **hash** is recorded, never a reference.
- **HN-14 (kill switch).** A release is not a dispatch: it creates no attempt, calls no provider and is not gated
  by an engaged payment kill switch (which still blocks the existing submit path after reactivation); pinned by a
  test. Whether an engaged kill switch should *also* block a release is recorded as a question for the human (below).
- **HN-15 (reconciliation).** No new mismatch kind and no `tenant_system_read` policy: the release is an ordinary
  two-leg `withdrawal_rejected` posting that the existing ledger/projection reconciliation covers.
- **HN-16 (shared files).** Minimal ordered appends only: `permission.go`, `permissions.ts` (+ its test),
  `init-app-role.sql`, one line in `routes.go`, the operation constants and one `Claims.validate` case in
  `actorproof.go`, the capability constants, a second allowed call site in the A-16 static test
  (`internal/db/acting_setter_static_test.go`) and two table names in the adjustment `zz_actor_proof_guard` catalog
  test (nine -> eleven). `ADR 0110`'s operation table gains the four operations (scope `platform_acting` only).
- **HN-17 (test environment).** The up/down/up test needs migrations 0122 and 0123 present (`internal/db` refuses
  version gaps); during development they were stood in for by untracked no-op placeholders that are **not** part of
  this change.

**Open OWNER question (not decided here; the design fails closed around it; no production policy row is added by this change).** Q-HSEC-1: should an
engaged payment kill switch for the tenant also block `release_hold_to_player` execution? Implemented as "no"
(the switch stops outbound provider dispatch; this movement returns the player's own funds and sends nothing). If
the answer is "yes" it is a one-line addition to the executor's preconditions plus the migration-side recheck.

**Further open OWNER questions (recorded after the ledger-finance review; nothing is built or decided here).**

- **Q-HSEC-2 (scope of decisions 13-18).** Do the owner decisions cover only `approved` holds on a non-active tenant or
  brand (as implemented), or also holds in `requested` / `pending_review` on a non-active tenant? Implemented: approved
  only; the other states keep their hold until reactivation (ADR 0107 CT-PRE territory, not built).
- **Q-HSEC-3 (HN-6).** Is it acceptable that a tenant's stricter policy rows are ignored for this operation while the
  tenant or brand is non-active (a suspended tenant cannot raise the number of platform approvers)? Implemented: ignored,
  platform baseline only; pinned by `PolicyLookup_IgnoresTenantRowsWhenNonActive`.

---

## 17. RESOLVE-1 implementation notes (PAY-PAYOUT-UNBOUND-RESOLVE-1, migration 0125)

Appended by the `ledger-finance` implementer (task r19-resolve1, branch `gov-r19-resolve1`, base `4a2155f`). The design
above (§4, §12-§14) is **not** rewritten; this section records what 0125 and its Go code implement, every ambiguity
chosen while implementing (safest reading each time), the pins flipped deliberately, and what remains open. Owner
decisions 9-12 (ADR 0095 §44) are **not broadened or weakened**:

| Decision | How 0125 holds it |
|---|---|
| 9. never automatic | Nothing in the sweeper, poll, receipt or reconciliation paths resolves an unbound park. The only writer is an **executed** four-eyes resolution, in the final approval's own transaction. |
| 10. stays held until positively attributable | The park keeps its hold (attempt `disputed`, withdrawal `submitted`) until the database's deterministic verdict over **sealed** imports is `paid`/`not_paid` at request **and** again at `pending -> executing` (S-2), and Go re-verifies every seal. Any other verdict refuses with an audit row and changes nothing. |
| 11. four-eyes, complete audit | K3 machinery: in-force `payment_force_resolve` grants, LF-11 distinct Persons, S-12, S-2(iii), the DB recount **plus the `platform_acting` approver floor in the recount (S-6)**, signed actor proofs (digest + `evidence_line_id`), and `payment.manual_resolution_*` audit rows carrying the verdict, line, R, import ids, pinned reference and seal count. |
| 12. no release/settle without sufficient positive evidence | `withdrawal.Complete` (keyed `provider_id:R`) / `withdrawal.Fail` run only behind the executing-only fences (f)/(g), MR041 per kind at commit, R-4 amount/asset equality. The evidence standard itself is **D-7: still an open owner decision** (below). |

### 17.1 Deliverable status

| Item (orchestrator scope) | Status |
|---|---|
| 1. kinds `m4_evidence_paid` / `m4_evidence_not_paid` on `payment_manual_resolutions`, capability `payment_force_resolve` (D-9), evidence columns, M4 CHECKs, composite FK to statement lines | `IMPLEMENTED` against MOCK |
| 2. `payout_m4_evidence(p_tenant, p_attempt)` (§4.4; 64-line bound incl. R lookups, S-4; L-1 deterministic line) | `IMPLEMENTED` against MOCK |
| 3. sealed statement imports (ADR 0110 T10; S-5): HKDF subkey `b13-import-v1` of the B13 master, `canon("imp", ...)` incl. `lines_digest` (defined key-free in `internal/reconciliation/statement`, so reconciliation never imports the key module: R3 static pin), seal written in the INSERT, retired kids verify-only, startup gate extended to non-MOCK statement sources | `IMPLEMENTED` against MOCK. **The T10 launch flag stands**: security implementation review of the seal is pending and S-3 is `PROVIDER DEPENDENT` per real source |
| 4. acting SELECT on `payment_statement_imports`, `payment_statement_lines`, `payment_attempt_reference_evidence`; 0102 INSERT arms narrowed to the system shape | `IMPLEMENTED` |
| 5. `platform_acting` approver floor in the DB recount (`payment_manual_resolution_execution_status.platform_floor_met`) | `IMPLEMENTED` |
| 6. S-1..S-6 and R-1..R-4 | `IMPLEMENTED` (S-3 only as far as code can: the declaration comes from the in-process source; the authenticated channel / PSP signature is `PROVIDER DEPENDENT`) |
| 7. `tenant_system_read_executed` widened; `loadK3Evidence` loads executed M4 rows and adds their keys (S-1) | `IMPLEMENTED` |
| 8. M4-paid completions attributed in `ledger_join` (R-2) | `IMPLEMENTED` |
| 9. amount/asset DB-forced (R-4) | `IMPLEMENTED` |
| 10. "NOT IMPLEMENTED" dropped from the M4-scope hint, now "MOCK only"; post-M4-not-paid hint added | `IMPLEMENTED` (the bound-park R-K3-8 hint keeps "NOT IMPLEMENTED": M4 does not cover it; bound destination parks get their own hint, inert until B13-B, §17.7 C-3) |
| 11. HSEC policy/trigger split preserved: the acting `withdrawal_requests` UPDATE policy only gains the M4 kinds in the K3 arm; the HSEC arm and `withdrawal_requests_governed_release_guard` are untouched | `IMPLEMENTED` |
| HTTP: existing route accepts `evidence_line_id`, returns the evidence columns, the four §4.7 tokens | `IMPLEMENTED` |
| §4.7 read route `GET .../payment-attempts/{id}/resolution-evidence` and the approver provenance view (§4.3) | `NOT IMPLEMENTED` (not in this scope; `payments.EvaluateM4Evidence` is the read primitive) |
| §4.8 post-resolution signal cells (`success_after_m4_not_paid`, `contradiction_after_m4_paid`) | `NOT IMPLEMENTED` (not in this scope). Reconciliation raises the same contradictions every run (R-1); the real-time receipt/poll cells remain a follow-up |
| Any non-MOCK statement source / non-MOCK M4 | `BLOCKED` (T10 flag, S-3 PROVIDER DEPENDENT, **D-7 open**) |

### 17.2 Ambiguities and the reading taken (R19-n)

- **R19-1 (who may evaluate the evidence).** §4.3: "raises an error, never a verdict, when the attempt row or any of its
  statement scope is not visible to the session". The tenant-staff session cannot see the typed reference evidence
  (0115 R-4), so `payout_m4_evidence` admits only a **valid acting session for the tenant or the system shape** and raises
  `MR060` otherwise. Consequence: an M4 **request and its final approval** must be made by a `platform_acting` principal;
  intermediate approvals may be tenant-scoped. This is stricter than A-14/D-1 (fail closed). Question **Q-R19-1** below.
- **R19-2 (seal check ordering).** The INSERT runs first so the 0115 guard authorises before any evidence or
  provider signal is computed (security F-2/F-3 ordering); the trigger recomputes the verdict and forces the evidence
  columns; Go then verifies the seal and recomputes `lines_digest` of every forced import id **in the same
  transaction, before it can commit**. Nothing uncommitted is visible to anyone else, so this is equivalent to "before
  insert".
- **R19-3 (legacy and unkeyed imports).** `import_seal` is nullable: pre-0125 imports, and imports of a process with no
  B13 keys (dev/MOCK), stay readable by reconciliation (raising unchanged) but are **never** positive M4 evidence. A
  sealed import must carry `imported_by_service` (CHECK). An unsealed import is written with the pre-0125 column list (no
  declaration, no service), so the importer still runs on a pre-0125 schema.
- **R19-3a (policy shape).** "Narrow the INSERT arms of `tenant_staff_scope`" is done by keeping the 0102 `FOR ALL`
  policy, its name and its `USING`, and replacing only its `WITH CHECK` with the system shape. Splitting it into SELECT +
  INSERT would have removed the row visibility that lets the append-only triggers refuse UPDATE/DELETE **loudly** (they
  would become silent zero-row no-ops); `TestPaymentStatement_StoreIsAppendOnly` pins the loud refusal.
- **R19-4 (verdict labels).** `contradictory` = conflicting statement content or attribution (second reference,
  declined/reversed/pending, amount/asset, R held elsewhere, tombstone, ledger key, own Y != R); `insufficient` = missing
  or ineligible evidence. Both refuse with `MR062` / `force_resolve_evidence_insufficient`.
- **R19-5 (dedupe keys).** Paid: one group on (provider_reference, amount, asset_code, occurred_at) among succeeded
  lines (status is fixed). R-1 "second distinct succeeded line": the same key, any import.
- **R19-6 (R lookups).** By `provider_reference` of `kind = 'payout'` lines only (not `settlement_reference`, not deposit
  lines). The bound reference is read whenever the attempt holds one (only `destination_mismatch` can, for M4).
- **R19-7 (holders of R).** Another attempt holding R as `provider_reference` or typed Y: same provider (the reference
  space); `withdrawal_requests.provider_reference = R` and the `provider_id:R` idempotency key: tenant-wide (stricter).
- **R19-8 (not-paid recovery in reconciliation).** R-1 says the not-paid predicate raises on any succeeded line; it
  stops raising once executed `compensating_entry` debits with causation = the `withdrawal_failed` transaction reach the
  amount, exactly the M2 (d) rule and the hint text. LF to confirm (**Q-R19-2**).
- **R19-9 (MR041 visibility).** The deferred check now **raises** when the resolution is not visible to the committing
  session, for every kind (0124 C-1 parity; it previously returned silently).
- **R19-10 (DB-forced columns).** On an M4 INSERT the client must supply `amount`, `asset_code`, `evidence_reference`,
  `evidence_verdict`, `evidence_import_ids`, `provider_reference_at_submission` as NULL (else `MR010`); the guard copies or
  computes them. `evidence_line_id` is the request's and must equal the deterministic line (`MR061`).
- **R19-11 (payload hash).** M4 rows hash the 0115 canonical fields plus `evidence_line_id`, `evidence_reference`,
  `evidence_verdict`, the import ids (ascending, comma-joined) and `provider_reference_at_submission`; M1/M2 hashes are
  unchanged, so pending pre-0125 rows stay valid.
- **R19-12 (the floor).** `platform_floor_met` counts only **counted** approvals made in a `platform_acting` session;
  `true` for every non-M4 kind. Go treats a recount without the column (a pre-0125 schema, where no M4 can exist) as met;
  the database enforces the floor regardless.
- **R19-13 (schema tolerance).** Go reads the five new columns through `to_jsonb(row)` and the M1/M2 INSERT keeps its
  pre-0125 column list, so the K3 service also runs against a pre-0125 schema (rolling deploys; the 0115-era harnesses).
  Every M4 path needs 0125.
- **R19-14 (not-paid window).** `interval '24 hours'` literal, pinned equal to `payments.DefaultSettlementWindow` by a
  test. Because a fetched statement's coverage end cannot exceed fetch time + 5 min, a not-paid verdict is possible only
  ≥ 24 h after the last send (tests ingest such coverage directly). Per-rail re-check stays the §10.4 note.
- **R19-15 (source declaration).** `payout_lines_carry_merchant_reference` comes from an optional in-process interface on
  the source value (`PayoutLinesCarryMerchantReference() bool`, absent = false); the MOCK source declares `true` (its
  payout lines carry the merchant reference). `imported_by_service = 'reconciliation.payment_statement'`.
- **R19-16 (reserved-prefix catalogue).** `provider_reference_at_submission` gets the reserved-prefix CHECK (the ADR 0101
  5.4 catalogue pin requires it for every `*provider_reference*` column); `evidence_reference` carries the same rule.
- **R19-17 (destination_mismatch).** No writer exists before B13-B; the scope, the bound-reference pin and the S-1 bound
  reference key are tested by parking through the real T10 writer with that reason.
- **R19-18 (MA020 consequence; security/LF to acknowledge).** The acting SELECT policies make the acting K2 session
  see statement lines and typed Y. For MA020 (`player_open_payment_exposure`, 0119) that session is now
  system-equivalent: it applies the full rule instead of the F-VIS fail-closed reading (the 0119 pin anticipated this:
  "If a later policy change makes Y visible to K2 ... F-VIS must be revisited"). The tenant K2 session is unchanged.
  This relaxes an artefact of invisibility in the acting session only, to the designed rule; it is flagged, not hidden
  (**Q-R19-3**).

### 17.3 Pins flipped deliberately

- `b11M4ScopeHint` (payments) and `rsUnboundCU` (reconciliation): the M4-scope hint says "MOCK only", no longer "NOT
  IMPLEMENTED".
- `TestK3_T7_EvidenceTablePolicies`: the acting session now reads its tenant's typed reference evidence (still never
  writes).
- `TestMA020_K2_RLSVisibilityPin`, `..._ReversalLineClearsInTenantSessionOnly`, `..._NoFailOpenWhenYIsInvisible`: the acting
  session's visibility and MA020 outcome (R19-18).
- `k3SubmitDigest` (actor-proof K3 tests): the K3 INSERT digest gains the trailing `evidence_line_id` field (`~` for
  M1/M2), SQL and Go in lockstep.
- `TestHSEC_HoldRelease_Migration0124DownRefusals`: rolls back the (empty) 0125 first so the refusing step is 0124's.

### 17.4 Open owner decision and questions (nothing here is decided by this change)

- **D-7 (OPEN, owner).** The A-13 evidence standard (machine verdict over sealed imports + four-eyes portal confirmation
  bound by `evidence_ref_hash`), widened by revision 4 to the T10 residuals (T6 in-process key, source
  authenticity/completeness, availability), must be acknowledged by the owner **before any non-MOCK M4**. Until then M4
  runs against MOCK statement sources only. No policy was invented here.
- **Q-R19-1 (security: ACCEPTED, review 2026-10-09; owner note open).** `security` accepts that a tenant-scoped
  principal can never request or finally approve an M4 (R19-1); no tenant-staff read of the typed reference evidence
  is added. **Owner note:** this means a B2B operator under its **own licence** (ADR 0006 hybrid model) cannot
  self-serve M4 - every M4 needs a platform `platform_acting` requester and final approver. Whether that is
  commercially acceptable for own-licence operators is an owner decision, not an engineering one.
- **Q-R19-2 (ledger-finance: CONFIRMED, review 2026-10-09).** The not-paid recovery clearing of R-1 (R19-8) is the M2
  (d) rule; now pinned by `TestM4Recon_NotPaidRecovery_ClearsOnlyOnFullRecoveryWithTheCausation` (C-2).
- **Q-R19-3 (security + ledger-finance: ACKNOWLEDGED, review 2026-10-09).** The MA020 acting-session consequence
  (R19-18) is acknowledged by both; no further change.
- Unchanged and still open: the §10.3 T10 flag, S-3 per real source, LOW-1/RM-4 runbook, P-2/RM-3, S-8 (freeze
  `release_ledger_transaction_id`), the per-rail settlement window, the pending-before-succeeded PSP note.

### 17.5 Residuals

- Seal verification reads every line of each evidencing import (at most 64 imports); a very large real statement makes
  M4 execution slow (MOCK imports are small). PROVIDER DEPENDENT; revisit with the first real source.
- *(LF RR-3, r21; §19.)* The one read of `payout_m4_evidence` outside the 64-line bound is the **import-wide
  declaration-integrity check** (§4.4 amendment (b)): it scans every payout line of each import in the read set for a
  NULL merchant reference under a `payout_lines_carry_merchant_reference` declaration. It is refusal-only (it can only
  make not-paid `insufficient`), and like seal verification its cost grows with the size of the evidencing imports.
- T5 (ADR 0110): a runtime session with arbitrary SQL can still write ordinary non-governed postings; the M4 fences stop
  the governed shapes only, as for M2/HSEC. MR041 at commit refuses an executed M4 that links such a planted completion
  (wrong key), pinned by `TestM4_MR041_ForgedLinkAndPlantedPosting`.
- S-8 (still open): `withdrawal_requests.release_ledger_transaction_id` is not frozen; inside an acting executing
  transaction it can be rewritten. For an executed M4 the commit-time MR041 link check refuses it (same test); the column
  freeze remains the §10.4 S-8 follow-up.
- The evidence function is `STABLE` under READ COMMITTED: Go's re-evaluation and the guard's re-run are separate
  statements; an import committed between them makes the guard raise (`MR061`, the whole final-approval transaction
  rolls back) rather than execute on stale evidence. A later contradicting line is raised by R-1 on every run.

### 17.6 Evidence

Tests (runtime-shaped non-superuser role, private scratch databases, `-race -tags integration -count=1 -p 1`):
`internal/payments/m4_resolve1_*_integration_test.go` (`TestM4_*`, `TestM4Recon_*`), `internal/reconciliation/
payment_statement_m4_test.go` (`TestR1_*`, `TestR2_M4*`), `internal/payoutinstrument/importseal_test.go`,
`internal/httpserver/payment_force_resolution_m4_api_integration_test.go` (`TestForceResolutionAPI_M4_*`). Concurrency is
exercised by in-test races repeated 3-4 times under `-race` (two final approvals, paid and not-paid; approval vs a
contradicting import; approval vs sweeper and stale poll; request vs a concurrent ingest), and - after the review round
(§17.7 F-2) - the whole `TestM4_Concurrency*` set ran at `-race -count=50` (250 PASS, 0 FAIL, no race report). AP002/AP005 are the shared `actor_proof_require` behaviour already
pinned by the K3 proof tests; M4 adds AP001/AP004 and the digest pin. Mutation-kill evidence:
`docs/plans/prh2-hardening-round/prh2-r19-resolve1-mutation-kill.txt` (63 counted mutants over the migration, the
service, reconciliation, the importer and the seal, after the §17.7 review round: 60 killed; 3 survivors disclosed as
equivalent - S06, S09, G05; S17 is now killed, F-3). LOCAL evidence only; no real provider, no AWS, no
non-MOCK source.

### 17.7 Review amendments (RESOLVE-1 review round, 2026-10-09; security + ledger-finance)

Every amendment below tightens toward refusal. None broadens owner decisions 9-12: attribution stays positive-only,
nothing resolves automatically, and the park stays held unless a positive verdict is reached. §4.4 and §4.6 carry the
review-driven amendment notes; this section records the implementation.

| Finding | Change | Pinned by |
|---|---|---|
| **H-1** (security; double payout) | `payout_m4_evidence`: `v_rs` now holds the PSP reference of **every** platform-keyed payout line (any status), so not-paid (iii) also refuses on a succeeded/pending/reversed line on the declined line's own reference D, in any import. A succeeded line on D reads `contradictory` (the paid branch sees the declined line on the same reference); pending/reversed reads `insufficient`. The D lookups share the 64-line budget (S-4). A declaring import holding a NULL-merchant payout line makes not-paid `insufficient`. R-1: `loadK3Evidence` derives D from `evidence_line_id` (LEFT JOIN on the line, tenant-bound), adds it to the S-1 keys and to the not-paid lookup. | `TestM4_H1_NotPaid_LineOnTheDeclinedReference_CrossImport` (incl. refusal at execution), `TestM4_H1_NotPaid_DeclaringImportWithNullMerchantPayoutLine`, `TestM4Recon_NotPaid_SucceededOnlyOnTheDeclinedReference_Raises`; mutants H1a/H1b/H1c, RH1a/RH1b/RH1c |
| **H-2** (security) / **C-1** (LF) | paid: any payout line on R (any status, any import) with a non-NULL merchant reference other than the attempt's makes the verdict `contradictory`. R-1 paid check mirrors it (`pay_declared_paid_unconfirmed`, `check=m4_paid_contradicted`). | `TestM4_H2_Paid_OneReferenceNamingTwoMerchants_Contradictory` (both parks; and refusal at execution), `TestM4Recon_Paid_LineOnRNamingAnotherMerchant_Raises`; mutants H2a, RH2 |
| **C-2** (LF) | No code change; the not-paid recovery clearing is now integration-tested: a debit with another causation (even the full amount) does not clear, a partial recovery does not clear, the full recovery with causation = the `withdrawal_failed` transaction clears. | `TestM4Recon_NotPaidRecovery_ClearsOnlyOnFullRecoveryWithTheCausation`; mutants RC2a, RC2b |
| **C-3 / C-4** (LF; merge prep) | New hint `destinationPayoutCapturedUnpostedResolutionHint` ("payout reported to a destination other than the bound one: no completion against the player's hold; PSP recall/return or off-platform recovery; M4 not-paid only on positive decline evidence; never allocation"), selected by reason (`destination_mismatch`, `destination_integrity_failure`) after the post-M4-not-paid override and before the M4-scope and generic payout texts. **Inert** until B13-B classifies those reasons (today `reasonUnclassified`: no `pay_captured_unposted` finding is raised for them); the stale `inM4Scope` comment is corrected. B13-B (6204ba0) is **not** merged here. | `TestC3_DestinationHint_InertUntilClassified_SelectedByReason` (flips deliberately when B13-B lands); mutants RC3, RC3b |
| **L-1** (security) | The lines digest of the import seal binds each line's `provider_id`. | `TestImportSeal_*` field table; mutant L1 |
| **L-2** (security) | Code comment on `verifyImportSeals`; a go/ast static pin: `requestInTx` verifies before its audit row, `m4EvidenceRefusal` re-evaluates then verifies, `postM4` has exactly one caller (`decideInTx`) which calls `m4EvidenceRefusal` first. | `TestL2_EveryM4PathVerifiesImportSeals`; mutants G01, G03 |
| **L-3** (security) | No code change; a tenant-staff FINAL approval with the platform floor already met raises `MR060` and the whole approval transaction rolls back (no approval row, still pending, park held); a `platform_acting` final approval then executes. | `TestM4_L3_TenantStaffFinalApproval_FloorMet_MR060_RolledBack` |
| **F-2** (LF) | Two new races: two final approvals of an M4 not-paid (exactly one `withdrawal_failed`), and request-time evidence vs a concurrent contradicting ingest (never a release on contradicted evidence). The whole `TestM4_Concurrency*` set ran at `-race -count=50` (targeted `-run`). | `TestM4_Concurrency_TwoFinalApprovals_NotPaid`, `TestM4_Concurrency_RequestVsConcurrentIngest` |
| **F-3** (LF) | S17 re-classified (it is **not** equivalent): a real import that matches nothing of the attempt leaves the import set unchanged but makes the MOCK decline ineligible (`v_has_real`), so only the verdict comparison of the `-> executing` re-check refuses it. | `TestM4_DBRecheck_AtExecuting_NotPaid_RealImportMakesMockIneligible` (MR061, and the Go refusal); S17 now KILLED |
| **F-5** (LF) | `TestM4Recon_PaidCompletion_AttributedInLedgerJoin` asserts that **no** mismatch of any kind names the attempt or R, in-run and standing. | same test |

**Test-fixture consequence of H-1(b).** `TestM4Recon_DestinationMismatchNotPaid_LineOnBoundReferenceRaises` now ingests
its declined line on the bound reference **with** the attempt's merchant reference: under the amendment a declaring
import with a NULL-merchant payout line can no longer produce a not-paid verdict.

**F-4 (LF): availability of unsealed imports.** An import is sealed only when the importing process holds the B13 keys.
A process without them (missing secret, misconfigured deploy) stores imports **unsealed**: reconciliation still reads
them (they still raise), but they are never positive M4 evidence, so M4 reads `insufficient` for parks whose evidence
lies only there. Retiring a kid that still seals an import makes every M4 on it refuse (`force_resolve_evidence_unsealed`),
pending ones included. Re-fetching byte-identical content reuses the unsealed import (same `content_digest`), so it
does not restore availability; no re-seal job exists (`NOT IMPLEMENTED`). Fail closed: an availability loss, never a
wrong release. Recorded in `docs/runbooks/payout-instrument-keys.md` §3a.

**F-1 (LF): launch-blocking for any non-MOCK M4 not-paid.** The §4.8 real-time signal cells are `NOT IMPLEMENTED`, and
`internal/payments/payout.go` (~826-829, `case AttemptSucceeded, AttemptDisputed, AttemptRejected: return nil`) silently
no-ops a succeeded callback or poll on a `disputed` attempt. Because M4 leaves the attempt `disputed` (A-15), a PSP
success arriving after an executed M4 not-paid (T14 after M4) produces **no** real-time signal: it is detected only by
reconciliation R-1 (`pay_declared_not_paid_but_paid`) on the next statement run that carries the line. That latency is
acceptable for MOCK only. **Before any non-MOCK M4 not-paid, §4.8 `success_after_m4_not_paid` (and
`contradiction_after_m4_paid`) must be implemented**; this is launch-blocking alongside D-7 and the T10 flag.

**Q records (review outcome).** Q-R19-1 ACCEPTED by `security` (owner note: B2B own-licence operators cannot self-serve
M4); Q-R19-2 CONFIRMED by `ledger-finance`; Q-R19-3 ACKNOWLEDGED by `security` and `ledger-finance` (§17.4).


## 18. B13-B implementation notes (appendix, added with the B13-B code; the design above is not rewritten)

Scope delivered (`payments`, 2026-10-09; branch `gov-r18-b13b` on top of the B13-A merge `a355244`): the Go integration of the
payout destination binding into the withdrawal and payments paths, plus ONE migration that closes the NULL arm that 0123
tolerated (15.2 B13A-2). The owner decisions (ADR 0095 section 44, decisions 1-8) are implemented as written and are **not**
weakened: where a sub-question was open the safest reading was taken and is recorded as `B13B-n` in 18.2. Nothing here is a
statement about a real PSP, a custodian or a licence: every provider in the tests is `MOCK` or a test double.

### 18.1 Deliverable status

| Deliverable | Status |
|---|---|
| Migration `0126_payout_binding_required` (**placeholder number**: 0124 = HSEC, 0125 = RESOLVE-1; the orchestrator renumbers at merge): `CREATE OR REPLACE` of `withdrawal_requests_payout_binding_guard()` so an INSERT with NULL/NULL is refused (`PI046`); every other check byte-identical to 0123; legacy rows untouched; down restores the 0123 body | `IMPLEMENTED` (up/down/up whole-schema snapshot test; legacy-row test) |
| Binding at `withdrawal.RequestWithdrawal` (`RequestParams.PayoutInstrumentID` + `Destinations`; gate rule under `FOR SHARE` after H-SEC-11 and before the KYC evaluation and any write; replay compares the instrument; `WithdrawalRequest` gains the two binding fields) | `IMPLEMENTED` |
| `POST /v1/me/withdrawals` takes `payout_instrument_id` (409 `PAYOUT_INSTRUMENT_REQUIRED` / `PAYOUT_INSTRUMENT_NOT_USABLE`, 400 for a malformed id, 503 with no service) | `IMPLEMENTED` |
| Staff submit: `payment_method` no longer selects anything; the rail is the instrument's; a differing value is 400 `PAYMENT_METHOD_MISMATCH`; 409 `PAYOUT_DESTINATION_NOT_USABLE` on a T1p refusal | `IMPLEMENTED` |
| T1p (`ClaimForDispatch`): destination gate (`EvaluateGate` + `CheckTier` on the routed adapter) before the allow decision row, the state change and the attempt; write-once snapshot in the same transaction | `IMPLEMENTED` |
| Phase B (`DispatchWithdraw`): re-read + gate + tiering on the exact adapter value, snapshot check, `WithdrawRequest.Destination` built from the snapshot plus the decrypted detail; any failure is NotSent with no provider call | `IMPLEMENTED` |
| T2 / T12 (`destinationGateAndEscalate`): gate + snapshot check before the KYC gate and the claim CAS; failure escalates (T16) with `destination_not_usable` / `destination_integrity_failure`, audit row, B12 P1 last | `IMPLEMENTED` |
| Evidence cells (sync phase C, QueryStatus poll, callback/receipt): snapshot integrity + destination echo comparison before anything can settle or advance; mismatch parks `disputed` `destination_mismatch`; missing/unsealed snapshot parks `destination_integrity_failure`; terminal-attempt signal `destination_mismatch_on_terminal_payout` | `IMPLEMENTED` against `MOCK` |
| `WithdrawRequest.Destination`, `WithdrawResult.DestinationEcho`, `StatusResult.DestinationEcho`, `CallbackEvent.DestinationEcho`, `ReceiptEvidence.DestinationEcho`, `OperationManifest.EchoesDestinationFingerprint` | `IMPLEMENTED` (types); no real adapter populates an echo |
| Real-adapter echo computation (adapter-side `DestinationFingerprinter`) | `NOT IMPLEMENTED` / `PROVIDER DEPENDENT` (see B13B-8) |
| L-8 startup clause removed from `payoutinstrument.VerifyStartup` (same change as the migration) | `IMPLEMENTED` |
| Repo-wide check that no non-mock type embeds a Synthetic-marked type (`providerkind.ScanForSyntheticEmbedding`, negative control) | `IMPLEMENTED` |
| MOCK tier: a `synthetic_test` instrument (MOCK verifier) binds and pays through a Synthetic adapter; it never satisfies a non-Synthetic adapter | `IMPLEMENTED` |
| `docs/runbooks/payout-instrument-keys.md` note (dev/MOCK now needs the key families) | `IMPLEMENTED` |
| M4 "not paid" for a `destination_mismatch` park, the "disputed with an executed M4" echo cells of 2.6 | `NOT IMPLEMENTED` (RESOLVE-1, 0125) |
| Reconciliation classification of destination parks (payout-scoped table, B13B-13) | `IMPLEMENTED` (Go only; no migration) |
| Compliance alert on a registration fingerprint conflict (15.3 item 3); lifting a staff/provider block (15.3 item 2); the stranded `approved` withdrawal after a permanent block (HD-R15-5) | `NOT IMPLEMENTED` (unchanged open items) |

### 18.2 Ambiguities and the reading taken

- **B13B-1 (migration number and arm).** `0126` is a placeholder. The guard refuses only **NULL/NULL**; a half binding still reaches the
  both-or-neither CHECK (`23514`), exactly as under 0123 (so `TestBinding_InsertGuard` keeps its half-binding assertions). `PI046` is new.
  The migration changes one function body and nothing else (whole-schema snapshot test: exactly the body hash differs).
- **B13B-2 (what "gate before a callback/poll that settles" means).** 15.6 asks for `EvaluateGate` and `CheckTier` before any retry,
  resend or callback/poll that settles the attempt. `EvaluateGate` and `CheckTier` run at request creation, T1p, phase B and T2/T12
  (every point that can cause a **send**). For evidence on an attempt that may already have been sent (sync phase C, poll,
  callback) the check is the **snapshot integrity check plus the echo comparison**, not the instrument's current state: ADR 0111 2.4
  (LF95-C10(d)) says polls and evidence are not blocked by a later suspension or revocation, and blocking a settlement of money that
  was already sent would strand it. A snapshot that is missing, unsealed or inconsistent with the withdrawal, the attempt, the
  instrument, the amount and the asset fails closed (`destination_integrity_failure`). If the owner wants the instrument's *current*
  state to also gate settlement, that is a one-line change in `payoutDestinationEvidence` and a policy decision (see 18.4 Q1).
- **B13B-3 (phase B holds no lock).** The phase B read runs in its own short read transaction that is closed before the provider call
  (INV-IO-1 / INV-POOL); the instrument is not locked across the call. A revocation that lands between phase B's read and the call is
  caught at the next gate (T2/T12) and by the evidence check; it cannot be prevented, only parked.
- **B13B-4 (the staff body).** `payment_method` is accepted but optional. For a bound withdrawal it is only **compared** with the
  instrument's rail, inside the claim transaction after the row lock, the state check and H-SEC-11 (so those refusals keep their
  precedence): a different value is 400 `PAYMENT_METHOD_MISMATCH`, nothing is written. For a legacy NULL-binding withdrawal
  (Synthetic adapters only) it is still the route input and is required.
- **B13B-5 (how the collaborators reach the free functions).** `DispatchWithdraw`, `ApplyPayoutResult` and the poll path are free
  functions. They take variadic `PayoutOption`s (`WithDestinations`, `WithProviderLookup`); `Orchestrator.PayoutOptions()` yields the
  production set. With no destination service a **bound** withdrawal makes no provider call and cannot be settled (parked as
  `destination_integrity_failure`); a non-Synthetic adapter is never called without a destination. A static test pins that every
  production call site passes `opts...` (exact site counts, so a new caller is reviewed). The alternative (a package-level
  registration) was rejected as hidden global state.
- **B13B-6 (absent echo).** A success without an echo from an adapter whose manifest declares `EchoesDestinationFingerprint` is
  **ambiguous**: sync phase C marks the attempt ambiguous (the poll decides), the poll reschedules, the callback cell does not apply it
  (the receipt is closed as an anomaly; a deferred receipt, which stores no echo, therefore never settles such an adapter). An adapter
  that does not declare an echo settles as before. Consequence for a future real adapter: if it declares the echo and its poll never
  returns one, the attempt cannot settle automatically and ends in T16 (non-idempotent manifest or max resubmits), which is the
  fail-closed outcome.
- **B13B-7 (T1p refusal).** A refusal at T1p commits only the denial audit (`withdrawal.submit.http`, `denied`, `denied_by_destination`,
  closed `gate_reason`). No B12 alert is raised there because no attempt exists to key the discriminator on; the request stays
  `approved` and every retry is refused until the instrument is usable again, which for a suspended or provider-revoked instrument is
  permanent (15.3 item 2, HD-R15-5). T2/T12 refusals, where an attempt exists, escalate and raise.
- **B13B-8 (the echo is the adapter's job, and it needs the tenant).** S95-C10 requires the echo to be computed **inside the adapter**
  by an injected `DestinationFingerprinter`, which is tenant-bound (`Keys.FingerprinterFor(tenantID, kinds)`). A multi-tenant adapter
  instance does not receive the tenant in `Withdraw` / `QueryStatus`, so a real adapter cannot build the right fingerprinter today.
  The MOCK adapter reports a test-set echo (`SetDestinationEcho`). This is an interface question for the architect (18.4 Q2), not
  something B13-B decides.
- **B13B-9 (binding is mandatory in MOCK; dev needs keys).** Decision 8: only the verification source is flexible in MOCK. Without the
  payout-instrument key families the service is nil and `POST /v1/me/withdrawals` answers 503. See the runbook note.
- **B13B-10 (embedding check scope).** The scan covers the non-test sources of `internal/` and `cmd/`. A type is accepted only if it
  declares its **own** `SyntheticComponent` method (deliberately a mock); embedding a marked type, a pointer to one, the
  `providerkind.Synthetic` interface, or a type that inherited the marker transitively is a finding. Test doubles (which embed
  `*MockProvider` on purpose) are not scanned. Matching is by (package name, type name), conservative across same-named packages.
- **B13B-11 (reasons).** `destination_mismatch` and `destination_integrity_failure` are terminal reasons of a parked payout attempt
  (`PayoutDisputeReasons()`, not M2-admitted, hold kept). `destination_integrity_failure` and `destination_not_usable` are also the
  T16 escalation reasons; `destination_mismatch_on_terminal_payout` is a raise-only signal. No migration is needed for any reason
  (`terminal_reason` has only a length CHECK).
- **B13B-13 (reconciliation classification; implemented as a payout-scoped table, a deviation from the wording of 2.6).** 2.6 says to
  add `destination_mismatch` to `disputeReasonClasses`. That table is the **deposit** table, pinned to
  `payments.DepositDisputeTerminalReasons()` and to the MA020 SQL list in `payment_attempt_open_exposure`; adding the payout reasons
  there was refused by both pins. First recorded as not implemented, then corrected on ledger-finance review (LF M-2): MA020 filters
  `operation = 'deposit'`, so no migration is needed. **Chosen: implement** a separate Go table `payoutDisputeReasonClasses`
  (`destination_mismatch` and `destination_integrity_failure`: bound-if-referenced; consulted only for non-deposit attempts in
  `captureClass`), pinned to `payments.PayoutDisputeReasons()` by `TestPayoutDisputeReasonClasses_PinnedToPayments`, with an integration
  test (in-run and standing finding). It is small, adds no migration and leaves both deposit pins untouched. It depends on the park
  binding the provider's reference first (B13B-17, LF H-2).
- **B13B-14 (tests that pinned a pre-0123 schema).** The payments code now reads and writes the 0123 columns, so a payout world cannot run on a
  scratch database stopped at 0115. `TestK3_Y08/X10/Y12` now run on the head schema (Y12 also disables the 0120 actor-proof guard in its owner
  bypass); `TestK3_C16_DownRefusals` runs the 0115 `down` script directly on the head schema (its first statement is the MR099 refusal, so the
  assertion is unchanged) instead of stepping the migration runner down. Closed-set pins updated deliberately: payout escalation reasons 3 -> 5,
  payout signal reasons 3 -> 4, `payout_destination.go` joins `payout.go` / `payout_sweep.go` as a payout-only file in the deposit terminal-reason
  scan, and the B12 per-site table skips the `destination_*` reasons (covered by `TestB13B_*`).

- **B13B-15 (security H-1: an echo-carrying receipt is never deferred).** The echo is deliberately not persisted (S95-C10, no new
  column), so a payout success callback carrying one that arrived before any attempt held the reference would have been stored as a
  `deferred_unresolved` receipt and later drained **without** the comparison. Chosen: **(b)** close such a receipt as an anomaly
  (`anomaly_other`) in `ApplyReceiptEvidence`, write one audit row (`payments.payout_echo_receipt_unattributable`, once per receipt) and
  raise a receipt-subject P1 (`receipt:<id>:reason:payout_echo_receipt_unattributable`, existing Kind) as the last statement. Not (a):
  a retryable refusal depends on each provider redelivering on a 5xx (not guaranteed, adapter-specific, PROVIDER DEPENDENT) and would turn
  a security signal into a retry loop; not (c): it needs a migration and the brief excludes one. Settlement then waits for the
  QueryStatus poll, which compares. A later echo-free redelivery of the same event dedupes against the closed row. Residual: an adapter
  that does **not** declare `EchoesDestinationFingerprint` and later sends a separate echo-free success for the reference settles as
  ordinary evidence (it has no echo to compare); the audit row and the P1 are the control. An echo-declaring adapter cannot settle on an
  echo-free success (ambiguous).
- **B13B-16 (security L-1..L-4 and the info finding).** L-1: `gatePayoutDestination` (dispatch gates only) refuses a nil adapter
  (`tier_refused`); `EvaluateGate`'s nil-adapter "request creation" meaning is no longer reachable from a dispatch gate. L-2: an integrity
  (seal / fingerprint / snapshot) refusal raises a P1 on the existing Kind: at T1p as the last statement of the claim transaction
  (`withdrawal:<id>:reason:destination_integrity:<closed reason>`, the claim now opens through `alerting.InTx`), at request time detached
  after the rollback (`payout_instrument:<id>:...`); ordinary refusals raise nothing. L-3: `ScanForSyntheticEmbedding` also tracks
  interface embedding (an interface embedding `providerkind.Synthetic` or an inheriting type is a finding; an interface that lists
  `SyntheticComponent` is a marker declaration) with fixtures. L-4: `payoutinstrument.VerifyBindingGuardApplied`, wired in
  `cmd/platform-api`, refuses to start when a non-Synthetic payout adapter is registered and the guard source does not contain `PI046`
  (migration 0126 not applied). 0124 does not replace the guard and 0125 does not exist yet; if RESOLVE-1 (0125) or any later migration
  `CREATE OR REPLACE`s `withdrawal_requests_payout_binding_guard()` it must keep the `PI046` NULL/NULL refusal (the startup check and
  `TestMigration0126_UpDownUp_WholeSchema` key on it). Info: `GateResult` (it carries the decrypted `Detail`) redacts under
  `String`/`GoString`/`Format`/`LogValue`/JSON.

- **B13B-17 (ledger-finance review fixes).** LF H-1: the terminal-attempt destination signal (audit + `destination_mismatch_on_terminal_payout`)
  is ADDITIVE; the existing cells (T14 success-after-decline, the foreign-reference and amount signals, late evidence) still run after it.
  LF H-2: a destination park first binds the provider's validated, unconflicted reference (the B10 guard, then `MarkAccepted` and
  `AttachProviderReference`) and only then parks; a foreign-held reference is parked by the guard as `provider_reference_conflict`.
  LF L-2: the terminal signal's audit row is written once per distinct (attempt, echo), not keyed on `alreadyApplied` (a bad-echo
  redelivery dedupes against the earlier good delivery, so `alreadyApplied` is already true for the first bad one); the raise stays
  unconditional and dedupes into one alert. LF L-4: a request-time integrity refusal also writes a durable audit row
  (`withdrawal.request.destination_integrity_refused`, own transaction after the rollback) next to the detached P1. Test-only: `pitest.Bind`
  labels use a digit-free alphabet (a hex label could form a Luhn-valid run); the 0124 down-refusal tests run the down script directly
  and the HSEC route test binds an instrument.
- **B13B-18 (security and ledger-finance RE-REVIEW fixes).**
  - M-R1 (security, merge-blocking): the request-time integrity audit and P1 fire only for reasons that can come solely from the stored
    state of the player's OWN instrument (seal_invalid, fingerprint_mismatch, detail_unavailable, verification_missing,
    verification_not_latest). `relation_mismatch` (a client naming another player's instrument) is client input: the same generic 409,
    no audit row, no alert. The audit write and the detached P1 now run off the response path (goroutine, 15 s bound).
  - LR-1: the terminal-signal dedupe query on `audit_log` carries `created_at >= attempt.created_at`, so `idx_audit_log_tenant_time`
    (tenant_id, created_at DESC) bounds it. No migration. Residual: the filter on `target_id`/metadata is still a heap filter within that
    window; a dedicated index is a deferred consideration, not needed at current volumes.
  - LR-2 and ledger-finance L-B (P7): an echo-free payout SUCCESS that dedupes against a receipt closed as
    `payout_echo_receipt_unattributable` is HELD (no settlement; `changed=false`), for every adapter. The closed twin is detected through
    the audit marker written with the closure (no migration). Settlement then rests on the QueryStatus poll and the P1 raised at the
    first delivery. Residual (still launch-blocking, as B13B-15): a non-echo-declaring adapter's own later poll success settles as
    ordinary evidence; only adapters that declare the echo are covered by the poll comparison.
  - LR-3: the unattributable-echo P1 is one open alert per (tenant, provider) (`provider:<id>:reason:payout_echo_receipt_unattributable`),
    not one per receipt; per-receipt detail stays in the audit rows.
  - Ledger-finance L-A: a duplicate of a closed unattributable receipt that changes the attempt writes one audit row
    `payments.payout_echo_receipt_attributed` linking the receipt to the attempt (the receipt row is one-shot).
  - Ledger-finance P5: branch test added (reported reference held by another attempt: `provider_reference_conflict`, nothing bound, other
    attempt untouched, only the conflict P1, ledger unchanged).
  - Security confirmation C-1/C-2/C-3: the unattributable-echo marker is now written for EVERY receipt the H-1 branch closes (also a
    duplicate: an echo-free success deferred first, then the mismatching-echo redelivery), idempotently per receipt id, so the LR-2 hold
    also covers the reverse order. The marker lookup is bounded below by the receipt's received_at. The request-time integrity recorder has
    an 8-slot semaphore (non-blocking skip when saturated; the P1 dedupes anyway) and a recover.
  - RESOLVE-1 hint-work item (not changed now): the payout `pay_captured_unposted` hint is misleading for a bound `destination_mismatch`
    park, because migration 0125 refuses an M4 "paid" resolution for it. The hint text belongs with the RESOLVE-1 work.

### 18.3 Evidence

Tests (all `-race -tags integration -count=1 -p 1`, private scratch database, runtime role where the package uses it):
`internal/withdrawal` (`TestB13B_*`: bind, every refusal writes nothing and does not consume the key, replay, concurrency, race with a
suspension, partial failure, the database refuses an unbound INSERT), `internal/payments` (`TestB13B_*`: T1p snapshot and rail, body
cannot influence, refusal matrix, legacy/Synthetic vs non-Synthetic tiering including a positive sandbox-verified path, phase B no-call
matrix, T2/T12 escalation and idempotence, sync/poll/callback echo cells, absent-echo by manifest, missing/tampered snapshot,
concurrency, replay, partial failure; plus the static guards), `internal/payoutinstrument` (`TestMigration0126_*`, flipped
`TestBinding_InsertGuard`, `TestCheckSnapshot`, `TestCompareEcho`, startup matrix), `internal/httpserver` (`*_B13B_*`),
`internal/providerkind` (`TestSyntheticEmbedding_*`). Mutation-kill evidence:
`docs/plans/prh2-hardening-round/prh2-r18-b13b-mutation-kill.txt` (54 counted, 51 killed, 3 equivalent survivors, each a Go check that
duplicates a database guarantee).

Final runs (top-level tests, `-race -tags integration -count=1 -p 1`, 0 SKIP everywhere, so the integration tests did run): payments 856 PASS /
0 FAIL (the full package; the later `TestB13B_*` additions re-run separately: 30 PASS), withdrawal 61, payoutinstrument 75, providerkind 10,
reconciliation 223, casino 280, providercred 92, wallet 9, cmd/platform-api 52, config 52, auth 99, db 137, httpserver 636 PASS / 2 FAIL.
The two httpserver failures are `TestResolutionIsolation_*` latency bounds (500 ms / 400 ms) that fail intermittently on a loaded shared host;
they fail the same way on the unmodified base `a355244` (checked), they exercise deposit and casino callbacks, and nothing in this change is on
their path.

### 18.4 Questions for the owner / architect (none changes decisions 1-8; each fails closed today)

1. **OPEN OWNER QUESTION: should the instrument's current state also gate settlement?** Today a suspension after the call was made
   does not block recording the provider's result (B13B-2). The alternative parks every evidence on a suspended instrument, which can
   strand a payout that was actually sent. Recommendations: **security** recommends KEEPING the current behaviour (no gate);
   **ledger-finance** recommends "no as a gate, yes as a signal" (a non-blocking compliance signal on settlement against a blocked
   instrument). Both are recommendations only: the policy is the owner's, the signal is recorded as an OPTION and is NOT adopted or built.
2. **Tenant for the adapter-side echo (B13B-8).** Add the tenant (and the snapshot's `FingerprintKid`) to the request/context the adapter
   receives, or inject a per-tenant fingerprinter factory. Required before any real adapter can echo.
3. **Dev/MOCK key provisioning.** Binding is mandatory in MOCK, so every non-production environment needs the key families (runbook). Is
   a documented throw-away key script acceptable, or should local profiles ship a dev-only key generator behind the integration tag?
4. **Stranded `approved` withdrawals after a permanent block** (HD-R15-5, unchanged): the request stays `approved` and every submit is
   refused. A governed release path is RESOLVE-1's / HSEC's scope.
5. **Reconciliation of destination parks (B13B-13).** Should a follow-up migration extend `payment_attempt_open_exposure` (and the deposit
   classification pins) so that a `destination_mismatch` / `destination_integrity_failure` park is reported against a captured reference?
   Until then the P1 alert and the audit row are the only signals.
6. **Front ends.** `b2c` (`WithdrawalPage`, `api/withdrawals.ts`) still posts a withdrawal without `payout_instrument_id`, which is now a 409
   `PAYOUT_INSTRUMENT_REQUIRED`; the back office submit call still sends `payment_method` (accepted, compared with the rail). Both belong to the
   `frontend` / `backoffice` specialists and are not part of this change.
7. **OPEN architect/owner question: governed resolution of a `destination_integrity_failure` park (LF M-3).** The M2 route excludes
   it and the 0125 M4 scope covers only a `destination_mismatch` "not paid" resolution, so such a park (hold kept, P1 raised) has no governed
   path out. No policy is invented here.

**Launch-blocking for non-MOCK payouts (recorded, not built):** (a) every non-Synthetic payout adapter must declare
`EchoesDestinationFingerprint = true`, or the owner explicitly accepts an adapter that cannot echo (without the echo an adapter's success
settles on the snapshot integrity check alone, B13B-15); the registration refusal for a non-declaring non-Synthetic adapter is NOT built;
(b) the tenant-visibility interface question for the adapter-side echo (B13B-8 / Q2) must be answered and implemented.

### 18.5 Staff withdrawal detail: bound instrument display (usability change, branch gov-r24-bo-contract)

`GET /v1/admin/withdrawals/{id}` (the existing `withdrawal:review` permission, tenant-scoped, read-only) returns `payout_instrument`:
`{id, rail, display_mask}` of the bound instrument, or `null` for a legacy NULL binding. Never the detail, ciphertext, fingerprint, seal,
kind or state. A bound withdrawal whose instrument row cannot be read is an integrity anomaly (the composite FK makes it impossible in a
healthy database) and is answered with a generic 500 plus a structured error log carrying ids only, never `null` (which a client would
render as a legacy withdrawal).

**Accepted residual (security L-2): the mask is shown without seal verification.** The staff view reads `display_mask` and `rail` directly
and does not verify the instrument seal (B13A-5). Altering those columns requires database-owner access, and every gate (request time,
T1p, phase B, T2/T12, evidence) still refuses a tampered instrument, so the view can at worst display a wrong label; it cannot cause a
payout to a different destination. Seal verification in the read path is deliberately not built.

## 19. r21 reconciliation follow-ups to RESOLVE-1: the ledger-finance RR-1 ruling, R-1 over every matched reference, fail-closed evidence joins (appendix)

Appended by the `ledger-finance` implementer (task r21-rr1, branch `gov-r21-rr1`, base `5ee5885`). The design above is **not**
rewritten (§4.4, §4.6, §10.4 and §17.5 carry short pointer notes). **No migration** (0127 is not used: see 19.2). Every change is
refusal- or raise-direction only, except the one stop condition the ruling below grants, which is implemented as exactly that
rule. Owner decisions 9-12 (ADR 0095 §44) are **not broadened**: nothing here resolves a park, releases or settles funds, posts,
changes an attempt or a withdrawal, or resolves, deletes or rewrites a reconciliation mismatch row.

### 19.1 The RR-1 ruling (ledger-finance, review of 2026-10-09), as implemented

**Problem.** After an executed `m4_evidence_not_paid`, a late `succeeded` payout line plus a **full** K2 recovery with the right
causation correctly stopped `pay_declared_not_paid_but_paid` (R-1, Q-R19-2). But STANDING-1 (`pay_captured_unposted`,
PAY-PAYOUT-UNBOUND-STANDING-1) for the same attempt kept raising on every run, forever, with the "possible double payout after
M4 not-paid" hint, because the attempt stays `disputed` (A-15) and the payout clearing rule (`payoutCompletedRef`) can never
hold for a failed withdrawal.

**Lifecycle checked first.** Reconciliation findings are rows in `reconciliation_mismatches`, written per run (`persistRun`); there
is no alert state machine and no "clear" operation. "Stop raising" therefore means: a later run does not add a new row for that
finding. Earlier rows stay as history with their `investigation_status` untouched (staff own that column). Nothing is deleted.

**Ruling (verbatim in substance).** For an attempt with an executed `m4_evidence_not_paid`, STANDING-1 may stop raising **only if
ALL** of the following hold; anything else keeps raising:
- **(a)** the M2 (d) recovery rule holds: executed `compensating_entry` `debit_player` requests whose causation is the
  resolution's `withdrawal_failed` transaction, on the same wallet and asset, total at least the amount;
- **(b)** the succeeded line's amount and asset equal the attempt's (an unequal line keeps raising);
- **(c)** tenant and provider scoping is intact.

**Chosen option: stop raising (not the "recovered; acknowledge" hint).** It is implemented as **one** predicate,
`m4NotPaidRecovered` (`internal/reconciliation/payment_statement_m4.go`), used by **both** R-1 (`checkM4Standing`) and STANDING-1
(`clearedRefFor` -> `m4NotPaidRecoveredLine`, which adds only "this finding's line IS the recovered payout"). So R-1 and
STANDING-1 stop under exactly the same condition, which is exactly the ruling's:

| Ruling | Code | Fail-closed detail |
|---|---|---|
| (a) causation, sum >= amount | `loadK3Evidence` sums `ledger_adjustment_requests` with `tenant_id`, `state = 'executed'`, `reason_code = 'compensating_entry'`, `direction = 'debit_player'`, `causation_transaction_id = r.ledger_transaction_id` **and** (new, tightening) `wallet_id` = the withdrawal's wallet and `asset_code` = the resolution's asset; `recovered >= amount` | no recovery read, or a resolution amount/asset other than the attempt's: never stops |
| (b) line amount/asset equal | the late payout must be **exactly one** distinct succeeded line group over every reference R-1 reads (dedupe key R19-5: reference, amount, asset, occurred_at; re-deliveries collapse into it), and that line's amount and asset equal the attempt's | a second, NEW succeeded line (another reference, or the same reference at another time) keeps raising - the recovery covers one payout of the attempt's amount (R19-n reading below) |
| (c) scoping | resolution, attempt and lines all come from this run's tenant- and provider-bound reads; the resolution must be this attempt's (`attempt_id`), of kind not-paid | an attempt/line/withdrawal the run cannot see fails the run (19.4) |

**Reading taken (RR1-1, safest).** The ruling speaks of "a late succeeded line" (singular). A recovery of the amount covers one
payout; two distinct succeeded payouts are two possible double payouts. So "exactly one distinct succeeded line group" is part
of (b): a later NEW succeeded line raises again (R-1 and STANDING-1, for the old and the new line), and only a re-delivery of the
same line (same content) keeps the stop. This also **tightens R-1** relative to Q-R19-2: an unequal-amount line, or a second
payout, after a full recovery now keeps R-1 raising too (raise direction only).

**Reading taken (RR1-2).** The ruling is a STANDING-1 ruling (unbound, line-keyed findings). The BOUND sites (BOUND-CLEAR-1:
`matchPayment`'s bound case and `checkUnmatchedAttempts`, keyed on X with no line) pass no evidencing line and never stop
under RR-1. Consequence (pinned, `TestRR1_BoundDestinationMismatchPark_BoundFindingOutsideTheRuling`): a bound
`destination_mismatch` park after an executed M4 not-paid and a full recovery keeps its `pay_captured_unposted` (post-M4 hint)
on every run while R-1 stops. Question **Q-R21-1** below.

### 19.2 R-1 reaches every matched decline reference (security LOW condition 1 / LF RR-4)

`payout_m4_evidence` checks, for not-paid (iii), every matched reference `v_rs`: the PSP reference of every payout line (any
status) matched by the merchant reference, the bound reference or a Y. R-1 keyed only the evidence line's own D (plus the
merchant reference, bound reference and Y), so a later succeeded line with **no** merchant reference on a **different** matched
decline reference D2 was not seen after execution.

**Chosen: recompute the same matched-line set** (no migration). After the first bounded read, `loadK3Evidence` derives, per
executed not-paid, `matchedRefs` = the references of every payout line matched by the merchant reference, the bound reference
(pinned and current) or Y - the verdict's `v_rs` - and reads the payout lines on those that are **not** already lookup keys.

**I-2 kept.** The key count (64 per key) is still fixed from platform state **before** any read; matched references are statement
content and get **no** budget of their own: their read is limited to what is **left** of the fixed cap (`lineCap - read`, reading
at most one row more), shares the run's single row counter, and the row that takes the run past the cap fails it with
`ErrPaymentEvidenceOverflow` (P1, no run row, no truncation). Lines already read through a key merchant reference are excluded so
they are not counted twice.

**No partial matching, raise only.** Matching is exact (`= ANY`), tenant- and provider-bound. Lines read only through a matched
reference are filed apart (`byMatchedRef`) and read **only** by the M4 not-paid predicates: R-1 (raising) and the RR-1 rule, where
an extra line can only add a distinct succeeded group and so keep the finding raised. No other rule (deposit clearing, Y
attribution, M2, STANDING-1's line set) sees them. A prefix, an extension, or the same string under another provider never
matches (tested).

**Why not record the set durably (0127)?** A set recorded at execution would miss a decline reference that first appears in a
later import (a NEW declined line on the merchant reference, then a merchant-less success on its reference). Recomputing from the
persisted lines covers that case, needs no schema change and stays inside I-2.

### 19.3 Silent join fallback hardened (security LOW condition 2)

**Where it failed silently.** `loadK3Evidence`'s executed-M4 query (`payment_statement_k3.go`):
`LEFT JOIN payment_statement_lines d ON d.id = r.evidence_line_id ...` with `COALESCE(d.provider_reference, '')`. An evidence line
the run could not see read as D = "" and the S-1 key for D was silently **not** added, so a later succeeded line on D with no
merchant reference was never looked up. Two further silent paths existed in the same place: the **inner** `JOIN payment_attempts`
(an invisible attempt dropped the whole resolution from R-1) and `checkM4Standing`'s `if a == nil { continue }`.

**Hardening (fail closed, existing convention).** Every join is a `LEFT JOIN` and every joined row is **required**: the attempt
(of the resolution's provider), the evidence line (of that provider, kind `payout`, non-empty reference) and the withdrawal (its
wallet scopes the recovery). The resolution is selected by its own `provider_id` (DB-forced from the attempt by the 0125 insert
guard), so an invisible attempt cannot hide it. A missing row fails the run with the new sentinel `ErrPaymentEvidenceInvisible`,
surfaced exactly like `ErrPaymentEvidenceOverflow`: no run row, no mismatch row, the existing P1
`reconciliation.sweep_run_failed` (phase match) and run-failure alert, on every run until resolved. An attempt seen by the join
but not by the same snapshot's platform read, or two executed M4 not-paid resolutions for one attempt, fail the same way. No
policy decision was needed: an M4 row always has its evidence line (0125 CHECK + composite FK, lines are append-only), so
invisibility can only be an RLS or session-shape fault, and failing closed is the convention.

**Availability consequence (recorded).** Like an overflow, this refuses the **whole tenant x provider stream**, deposits
included, loudly. It cannot occur in the designed session shape.

### 19.4 Deliverable status

| Item | Status |
|---|---|
| RR-1: one stop rule for R-1 and STANDING-1 after an executed M4 not-paid (19.1) | `IMPLEMENTED` against MOCK |
| RR-1 (a) recovery scoped also by the withdrawal's wallet and the resolution's asset (tightening) | `IMPLEMENTED` |
| R-1 over every matched decline reference within the fixed I-2 budget (19.2) | `IMPLEMENTED` against MOCK |
| Fail-closed M4 evidence joins, `ErrPaymentEvidenceInvisible` (19.3) | `IMPLEMENTED` |
| RR-3 doc: the one read outside the 64-line bound named (§4.4 note, §17.5); onboarding note (§10.4) | `IMPLEMENTED` (text) |
| Migration 0127 | `NOT IMPLEMENTED` (not needed, 19.2) |
| Bound (BOUND-CLEAR-1) finding of a recovered `destination_mismatch` not-paid park | `NOT IMPLEMENTED` (outside the ruling; Q-R21-1 open, LF recommendation recorded). **Superseded (r30): RESOLVED by owner decision 1 (ADR 0095 §48), `IMPLEMENTED` against MOCK in §21** |
| Net recovery (debits minus credits) for M2 (d) and RR-1 (security LOW-1) | `NOT IMPLEMENTED` (open LF question, 19.6). **Superseded (r30): RESOLVED by owner decision 2 (ADR 0095 §48), `IMPLEMENTED` in §21** |
| Runbook entry for `ErrPaymentEvidenceInvisible` | `IMPLEMENTED` (text, `docs/runbooks/operational-runbooks.md` §16) |

### 19.5 Tests and evidence

- Pure (no database): `internal/reconciliation/payment_statement_rr1_test.go` (`TestRR1_Pure_StopRuleAndEveryNegation`,
  `TestRR1_Pure_ScopingAndBoundSitesNeverStop`); `TestR1_M4Predicates_Pure` now exercises the not-paid kind too (the recovery is
  read by `loadK3Evidence`, so `checkM4Standing` no longer queries).
- Integration (real stream, real M4 four-eyes execution, real K2 path, runtime-shaped role, private scratch databases):
  `internal/payments/m4_resolve1_rr1_integration_test.go`: unresolved M4 keeps raising every run; full legitimate recovery stops
  both findings, repeated runs and re-deliveries (also a duplicate inside one statement) stay stopped, history rows are kept and
  none is resolved, the park is untouched, and a later NEW succeeded line raises again (old and new line); a debit with another
  causation (full amount), a full debit caused by an unrelated transaction (another park's `withdrawal_failed`), a compensating
  **credit** with the right causation and a partial recovery all keep raising; unequal-amount and other-asset lines keep raising;
  cross-tenant evidence (tenant B's recovery, B's lines under A's provider and merchant reference, a B compensation citing A's
  transaction) cannot stop A's findings; the bound destination park pin (RR1-2); R-1 on a different matched decline reference
  (no merchant reference) raises, with prefix/extension references and another provider's identical reference never matching,
  duplicates and replays raising exactly once per run, and multi-reference staying one finding; a matched-reference line after a
  recovery raises again; the I-2 boundary with matched-reference lines (128 rows for 2 keys runs, 129 refuses); invisible evidence
  line, attempt and withdrawal each fail the run closed with the P1 through the sweep entry point.
- Mutation evidence: `docs/plans/prh2-hardening-round/prh2-r21-rr1-mutation-kill.txt` (counts in 19.7).
- Final runs (`-race -tags integration -count=1 -p 1`, targeted `-run`, private scratch databases; 0 SKIP everywhere, so the
  integration tests did run): `internal/reconciliation/...` whole package set 235 PASS / 0 FAIL; `internal/payments`
  `-run 'TestM4|TestB11|TestK3|TestB13B|TestRR|TestMA020|TestHSEC'` 308 PASS / 0 FAIL (top-level); the final
  `TestRR1_|TestRR4_|TestRR5_|TestM4Recon_` run at the committed tests 18 PASS / 0 FAIL. gofmt clean; `go vet` clean with and
  without `-tags integration`; golangci-lint 2.9.0 `--new-from-rev=5ee5885 ./internal/...` 0 issues.

### 19.6 Questions and review outcome (nothing here is decided by this change)

Review of `gov-r21-rr1` (2026-10-09): `security` and `ledger-finance` both **APPROVE WITH CONDITIONS**. Security found no way for
provider evidence to stop a finding early; ledger-finance confirmed the RR1-1 reading (exactly one distinct payout).

- **Q-R21-1 — RESOLVED by owner decision 1 of 2026-10-09 (ADR 0095 §48); `IMPLEMENTED` against MOCK in §21 (r30).** The
  original r21 record follows unchanged (history): *(OPEN, architect + ledger-finance decision; `NOT IMPLEMENTED`).* Should RR-1 extend to the BOUND site of a
  recovered `destination_mismatch` not-paid park (a finding keyed on X with no line)? Today it keeps raising (safe, noisy).
  **Ledger-finance recommendation (a recommendation, not a decision):** extend RR-1 to that bound site, stopping **only** when
  `m4NotPaidRecovered` holds **and** the single succeeded group's reference equals the bound reference X (or that line carries
  the attempt's merchant reference) **and** its amount and asset equal the attempt's; with a pin test, a negative test (a payout
  under another reference keeps raising) and a mutant. If the architect declines the extension, the minimum is a "recovered;
  acknowledge" hint at that site. Until decided, the bound finding keeps raising with the post-M4 hint
  (`TestRR1_BoundDestinationMismatchPark_BoundFindingOutsideTheRuling` pins it).
- **Q-R21-2 (CLOSED by ledger-finance: keep conservative).** No "recovered >= amount x n distinct payouts" rule. The K2 cap
  (INV-ADJ-6) allows at most one amount of compensation per causation and direction (the `withdrawal_failed` transaction's
  `player_cash` leg), so such a rule could never be met and would advertise a route that does not exist. Recovering a second
  payout is off-platform or needs a separately governed path; until then both findings keep raising.
- **Security LOW-1 — RESOLVED by owner decision 2 of 2026-10-09 (ADR 0095 §48); `IMPLEMENTED` in §21 (r30), for both M2 (d)
  and RR-1.** The original r21 record follows unchanged (history): *(OPEN ledger-finance question; `NOT IMPLEMENTED`).* The recovery sum of RR-1 (and of M2 (d), which it
  reuses) counts executed `debit_player` compensating entries only, not the **net**. With four-eyes collusion, two debits and
  then two credits with the same causation (each direction capped separately by INV-ADJ-6) would satisfy the sum and stop the
  finding while the double payout is in fact unrecovered. The fix would be **net recovery**: debits minus credits with the same
  causation, wallet and asset, excluding reversed debits, for **both** M2 (d) and RR-1. It is not implemented here because it
  changes the already-ruled M2 (d) rule (Q-R19-2); it needs a ledger-finance ruling first. Mitigations today: four-eyes on every
  K2 entry, the K2 audit rows, and the compensating credit's own audit trail.
- **Security INFO (recorded).** (i) The matched-reference reads share what is left of the fixed I-2 budget, so a large
  legitimate stream can hit `ErrPaymentEvidenceOverflow` sooner than before (fail-closed, loud; runbook note in
  `docs/runbooks/operational-runbooks.md` §16). (ii) The recovery read is one query per executed M4 not-paid per run; a single
  grouped query would remove the per-resolution round trip. Not changed (performance only, no correctness effect).
- **Runbook.** `ErrPaymentEvidenceInvisible` (cause, impact, diagnosis, nothing cleared) and the overflow sensitivity:
  `docs/runbooks/operational-runbooks.md` §16.

### 19.7 Mutation evidence

`docs/plans/prh2-hardening-round/prh2-r21-rr1-mutation-kill.txt`: 31 counted mutants (17 RR-1, 9 RR-4, 5 LOW-2), **26 killed**,
5 survivors disclosed as equivalent: A03/A04 (the recovery probe's wallet and asset filters; equivalent under MA022
`causation_not_on_wallet`), A06/B09 (tenant predicates; equivalent under FORCE RLS and the composite causation FK) and C05 (the
platform-read backstop; subsumed by the same-snapshot attempt check). LOCAL evidence only.


## 20. Post-resolution signal cells (ADR 0111 4.8, F-1) implementation notes (appendix, `payments`, 2026-10-09; branch `gov-r22-cells`, base `5ee5885`; the design above is not rewritten)

This section implements the already-governed 4.8 cells and nothing else: no new rule, no migration, no new alert Kind, no change of
any owner decision (ADR 0095 section 44, decisions 9-12), no state change, posting or release by a cell. Every provider in the
tests is `MOCK`. Nothing here is a statement about a real PSP or a licence.

### 20.1 Deliverable status

| Deliverable | Status |
|---|---|
| 4.8 cell `success_after_m4_not_paid`: a `succeeded` callback, poll or late sync/poll result on a `disputed` payout attempt whose withdrawal was `failed` by an EXECUTED `m4_evidence_not_paid` | `IMPLEMENTED` against `MOCK` |
| 4.8 cell `contradiction_after_m4_paid`: a `declined` callback, poll or late result on a `disputed` payout attempt whose withdrawal was `completed` by an EXECUTED `m4_evidence_paid` | `IMPLEMENTED` against `MOCK` |
| One audit row `payments.payout_post_m4_contradiction` (closed vocabulary), then the B12 P1 as the LAST statement (existing Kind `payment.webhook_integrity`, discriminator `payout_attempt:<attempt_id>:reason:<reason>`, attribute `provider_id` only); the two reasons join `payoutSignalReasons` (4 -> 6) | `IMPLEMENTED` |
| Closing F-1 (17.7) for `MOCK` | `IMPLEMENTED`: the silent no-op of `case AttemptSucceeded, AttemptDisputed, AttemptRejected: return nil` is gone for the disputed-after-executed-M4 case; every other replay on a disputed attempt is still a no-op |
| F-1 for any NON-MOCK M4 | `PROVIDER DEPENDENT`, still launch-blocking until the real provider's status semantics are confirmed (20.4) |
| A distinct provider "reversed"/"returned" payout status (`payout_returned` event type, a reversal outcome) | `NOT IMPLEMENTED`: the payout evidence model has only pending / succeeded / declined / ambiguous, and a `payout_returned` receipt is closed as an anomaly before any cell (`receipt.go`, the event-type allow-list). `declined` is the only contradiction of an executed paid that exists today |
| Migration | none |

### 20.2 Where the cells run (all under L1 withdrawal -> attempt, inside the existing `alerting.InTx` owners)

One function, `payoutPostM4Cell` (`internal/payments/payout_post_m4.go`), is the only implementation. It is a no-op unless the
attempt is a payout, re-read `disputed` under the withdrawal row lock, the withdrawal is in the state an executed M4 of that kind
leaves it in (`failed` for not-paid, `completed` for paid), and an `executed` `payment_manual_resolutions` row of the matching M4
kind exists for this tenant, attempt and withdrawal. A pending, rejected, cancelled, expired or `refused_at_execution` M4 gives no
signal; neither does evidence that agrees with the M4 (a decline after not-paid, a success after paid), a pending or ambiguous
outcome, an M1/M2 resolution, or a deposit. Call sites (pinned by `TestStaticWiring_PayoutPostM4CellSitesArePinned_F1`):

| Path | Site | Audit gate |
|---|---|---|
| callback / receipt | `receipt.go` `applyResolvedReceiptEvidence`: the success cell `case AttemptDisputed` and the decline cell `case AttemptDisputed` | once per NEW receipt (`!alreadyApplied`, the PAY-PAYOUT-CALLBACK-AUDIT-2 rule); the raise is unconditional (one alert, growing occurrences) |
| poll | `payout.go` `applyPayoutStatusEvidenceInTx`: `case AttemptDisputed`, definite success / definite decline | once per (attempt, reason, evidence kind), see R22-2 |
| late sync / poll result that lost the CAS to a disputed attempt (a stale snapshot) | `payout.go` `applyPayoutLateEvidence`: `case AttemptDisputed` (it now takes the observed outcome) | same as poll |

The cell is returned as the value of its cell (receipt) or of its function (payout.go), never followed by another statement; inside
it the raise is the final `return`, after the audit row (`TestPayoutPostM4Cell_*`, a go/ast pin, plus the alerting pins in 20.5).

### 20.3 Ambiguities and the reading taken (R22-n)

- **R22-1 (what "executed M4" means).** Read from the database, never inferred from the evidence: an `executed` row of the right kind in
  `payment_manual_resolutions` bound to the attempt's tenant, attempt and withdrawal, AND the withdrawal in the matching terminal state,
  AND the attempt `disputed`. The row and the state are redundant on purpose (an M4 only executes together with its posting); the tests
  separate them with a withdrawal closed through another route (`withdrawal.Fail` / `Complete` directly) with no M4 or only a pending
  request: no signal.
- **R22-2 (once per receipt, and a poll has no receipt).** A callback writes the audit row only for a new receipt, so a byte-identical
  redelivery, a racing duplicate and an orphaned-then-redelivered receipt give exactly one row, while a genuinely different event (a new
  receipt) gives one more. A poll, a sync result or a late result has no receipt; polling the same disputed attempt every few seconds
  must not grow the audit log without bound (the L-e precedent), so the row is written once per (attempt, reason, evidence kind), looked
  up in `audit_log` under the withdrawal lock the caller holds (bounded by `created_at >= attempt.created_at`). The raise is unconditional
  in both, so a replay is one open alert whose occurrences grow.
- **R22-3 (the destination cells compose, by construction).** On a `disputed` attempt `payoutDestinationEvidence` is a no-op (its state
  switch only acts on non-terminal and on succeeded/declined attempts), so a callback that carries a differing echo after an executed M4
  yields the 4.8 cell only: no re-park, no `destination_mismatch_on_terminal_payout` (that signal is for `succeeded` / `declined`
  attempts and stays additive there, B13B-17 H-1). A `destination_mismatch` park is NOT M4-paid-able (the database scope refuses it), so
  `contradiction_after_m4_paid` can never arise for it; an M4 not-paid on it leaves the attempt `disputed` / `destination_mismatch`, and a
  later `PollPayoutStatus` (the attempt holds its bound reference) finding the payout PAID raises `success_after_m4_not_paid`.
  Without an executed M4 the destination park stays the no-op the 2.6 table says.
- **R22-4 (which poll is reachable).** An UNBOUND M4-scope park has no reference on the attempt and none on the withdrawal (M4 paid keys
  its completion on `provider_id:R` and does not store R on the withdrawal), so `PollPayoutStatus` has nothing to query and only
  reschedules; for those parks the live cell is the callback (resolved through the merchant reference). The poll cell is live for a
  `destination_mismatch` park (bound reference) and is exercised for the unbound shapes through the production evidence transaction
  (`applyPayoutStatusEvidence`) directly. This is the existing design, recorded here, not changed.
- **R22-5 (audit vocabulary).** `reason`, `observed`, `m4_kind`, `m4_resolution_id`, `evidence`, `provider_id`, `withdrawal_request_id`,
  `withdrawal_state`, `attempt_state`, the attempt's own terminal reason collapsed through `payoutAlertReasonFor` (a closed set), and the
  reported reference only through `echoAuditMeta` (the value when it is a valid reference, else reason, length and hash prefix). No decline
  text, no amount echo, no provider text.

### 20.4 What stays open (F-1 is resolved for `MOCK` only)

- **PROVIDER DEPENDENT, launch-blocking for any non-MOCK M4.** The cells decide on the platform's own outcome classes (`succeeded`,
  `declined`). Whether a real provider reports a payout returned, reversed or recalled as `declined`, as a distinct status, or only on a
  statement is unknown until a vendor contract exists; a `payout_returned` event is closed as an anomaly today (no signal). The first real
  adapter must map its return/reversal semantics onto these classes (or an owner decision must add a class) before an M4 is enabled for it.
- **Launch-blocking, PROVIDER DEPENDENT, before any non-MOCK M4 paid (LF C-3).** (1) Each real provider's status mapping must map every
  return, reversal or chargeback-of-payout status to evidence that reaches `contradiction_after_m4_paid` or a dedicated reason; it must
  never be closed as an anomaly without a signal on an attempt that has an executed M4 paid. (2) The governed WITHDRAWAL-REVERSAL-1
  path must exist before any money moves back on such a return. (3) Each real provider's success / paid-out / settled / completed
  statuses must map to `OutcomeSucceeded`, with a conformance test per adapter, so that F-1 is reachable. **F-1 is RESOLVED FOR MOCK
  ONLY.**
- **Audit key of the receipt-less sources (LF C-1 / security LOW-1, amends R22-2).** The key is (attempt, reason, evidence kind, reference
  key), the reference key being a sha256 prefix of the reported reference (none when the evidence carries none: the old key). A second,
  different payout reported later therefore gets its own row. Capped at 8 rows per attempt and reason; the alert is raised every time.
  The lookup is bounded by `created_at >=` the attempt's creation (security LOW-2), no migration.
- Reconciliation R-1 remains the backstop for a line that never arrives by callback or poll; the cells only remove the latency for the
  two that do.
- The audit and alert are signals. Nothing resolves the contradiction: a payout the platform released and the PSP then paid
  (`success_after_m4_not_paid`), or one the platform completed and the PSP declined (`contradiction_after_m4_paid`), is a human /
  provider-recall matter (the 17.2 hint texts), exactly as 4.8 says.
- `ALERT-DELIVERY-1` is unchanged: the P1s are durable rows; there is still no real delivery channel (ADR 0102 17.7).

### 20.5 Evidence

- `go test ./internal/alerting -count=1`: PASS. The static pins were updated per new site, with no wildcard: the raise-site table gains
  `payout_post_m4.go:payoutPostM4Cell:raisePayoutDisputeAlert: 1`; the derived reaching set gains `payoutPostM4Cell` (reached only from
  `applyResolvedReceiptEvidence`, `applyPayoutLateEvidence` and `applyPayoutStatusEvidenceInTx`, all already inside `InTx` owners); it joins
  `staticEvidenceFuncs` (its three callers are already in the reviewed caller allow-list, which is NOT widened); its result is in the
  never-dropped set; and a new test pins its exact call sites (2 + 1 + 2).
- Tests: `internal/payments/payout_post_m4_integration_test.go` (both cells in the callback and poll paths; the destination-park composition
  including `PollPayoutStatus` and a bad echo; the no-M4 control; pending / refused / rejected / cancelled M4; agreeing evidence; replay and
  redelivery; orphaned receipt; racing duplicate deliveries and polls; late-evidence stale snapshots, sync and poll; a withdrawal closed by
  another route; tenant isolation with two tenants; transient and deterministic alert failure; closed audit vocabulary; ledger invariants
  `SUM(D)=SUM(C)` and projection = rebuild after every case) and `payout_post_m4_static_test.go` (raise-last and no-write go/ast pins).
- Final runs (`-race -tags integration -count=1 -p 1 -timeout 120m`, private scratch database, targeted `-run`, no full-repo sweep): the
  payments set `TestPostM4_|TestPayoutPostM4Cell|TestPayoutAlertReasonFor|TestPayoutDisputeAlert|TestB11_|TestB12|TestPayoutCallbackAudit|
  TestCallbackAudit2|TestOrphanResolve|TestB13B_|TestM4_` = 168 top-level PASS, 0 FAIL, 0 SKIP (so the integration tests did run; 20 are the new
  `TestPostM4_*` / `TestPayoutPostM4Cell_*` tests, 27 new subtests); `go test ./internal/alerting -count=1` PASS; gofmt, `go vet` (both tag
  modes) and golangci-lint 2.9.0 (`--new-from-rev=5ee5885`) clean. An earlier attempt of the same set hit go test's default 10-minute
  timeout (163 PASS, then the timeout panic); it was re-run with `-timeout 120m`.
- Mutation evidence: `docs/plans/prh2-hardening-round/prh2-r22-cells-mutation-kill.txt` (34 counted, 28 killed, 6 survivors, all classified
  EQUIVALENT: pairs of redundant predicates and the RLS tenant predicate; each pair is killed when both halves are mutated).
- A finding recorded for the owner (no code change): the system-shaped sessions that run the cells can read only EXECUTED M2/M4 rows
  (policy `tenant_system_read_executed`, 0125); the cell's own `state = 'executed'` and tenant predicates are therefore redundant there and are
  pinned for a principal-shaped session by `TestPostM4_PendingM4_NoSignal_EvenInAPrincipalShapedSession`.

## 21. Owner decisions 1 and 2 of 2026-10-09 implemented: NET recovery for M2 (d) and RR-1; RR-1 at the bound `destination_mismatch` site (appendix, `ledger-finance`, 2026-10-09; branch `gov-r30-rr1net`, base `dad803d`; the design above is not rewritten)

Provenance: ADR 0095 §48 decisions 1 (Q-R21-1) and 2 (RR-1 recovery calculation), recorded verbatim there and not
reinterpreted here. They supersede the open status of Q-R21-1 and security LOW-1 (§19.6, whose text is kept as history with a
RESOLVED marker). **No migration** (0127 stays reserved for the `destination_integrity_failure` task; 0128 is not used): every
input already exists and is readable by the reconciliation session (21.6). Nothing here resolves a park, releases, settles or
posts funds, changes an attempt or a withdrawal, or resolves, deletes or rewrites a reconciliation mismatch row. Every provider in
the tests is `MOCK`.

### 21.1 What was inspected before deciding (the K2 representation)

- A K2 adjustment is a `ledger_adjustment_requests` row (0113) whose execution links exactly one `manual_adjustment` ledger
  transaction (two entries: `player_cash` on the request's wallet and the tenant `manual_adjustment` account; MA040). Its
  `causation_transaction_id` names the transaction it compensates.
- A **compensating credit** under the not-paid causation F (the resolution's `withdrawal_failed`) is a `credit_player` request with
  `causation_transaction_id = F`. MA020 (`player_open_payment_exposure`, 0119) blocks credits only for **deposit** exposure, so such a
  credit is executable after an M4/M2 not-paid (the r21 tests executed one).
- A **reversal of a recovery debit** D is, in K2, a `credit_player` request whose causation is D's own `manual_adjustment`
  transaction (allowed: causation types refused are only deposit, deposit_reversal and tombstone; D has a `player_cash` leg on the
  wallet). No K2 path writes `ledger_transactions.reverses_transaction_id` on a manual adjustment (MA040 refuses it on the linked
  transaction), but nothing in the schema stops a separate posting from naming D there (ADR 0110 T5).
- **INV-ADJ-6** (0115 `ledger_adjustment_payload_refusal`): for `compensating_entry`, the executed amount per (causation,
  direction) never exceeds the causation's `player_cash` leg on the request's wallet and asset - for F, the withdrawn amount, for
  debits and credits separately. `operational_error_correction` and `external_instruction` take an optional same-wallet causation
  and are **not** capped. MA022 forces the request's wallet and asset to carry a `player_cash` leg of the causation.

### 21.2 The NET formula (decision 2), shared by M2 (d) and RR-1 (a)

`netRecoverySQL` / `netRecovery` (`internal/reconciliation/payment_statement_k3.go`), one query per not-paid resolution, with F =
the resolution's `withdrawal_failed` transaction, W = its withdrawal's wallet, A = its asset:

```
net = sum(D.amount) over executed compensating_entry debit_player requests with causation F, wallet W, asset A,
                    that are NOT reversed
    - sum(C.amount) over EVERY executed credit_player request with causation F (any reason code, wallet or asset)
recovered  <=>  net >= the resolution's amount
```

- **Reversed debit (counts zero):** an executed `credit_player` request of any reason code names D's `manual_adjustment` transaction
  as its causation, **or** any ledger transaction names D in `reverses_transaction_id`.
- **Positive side strict, negative side broad (fail closed).** Only `compensating_entry` debits are recovery (the M2 (d) rule; a
  debit of another reason code is not a recovery of this payout). The subtraction counts every executed credit under F whatever its
  reason code, and is not filtered by wallet or asset: it can only lower the net (MA022 forces W and A on it anyway).
- **Precision:** amounts are `NUMERIC(38,0)` integer minor units summed in SQL and read as text into `big.Int`; no float, the
  asset's exponent is never touched, and only same-asset debits are added.
- **Used by:** M2 (d) (`checkM2Standing`, `pay_declared_not_paid_but_paid`; the wallet now comes from the resolution's withdrawal,
  a new LEFT JOIN) and RR-1 (a) (`loadK3Evidence` -> `m4Resolution.recovered`, read by `m4NotPaidRecovered`, so R-1, STANDING-1 and
  the bound site below all stop under one recovery). The finding details report the net (`recovered=<net> of <amount>`; it may
  be negative). The M2 (d) hint now says "a NET recovery of at least the withdrawn amount ... minus every executed credit with that
  causation".

**INV-ADJ-6 consequence (no dead logic).** Because the compensating debits under F are capped at the amount and the cap counts a
reversed debit too, `net >= amount` holds exactly when the debits under F reach the amount, none of them is reversed and no credit
names F. Any offset therefore keeps the finding raised, and a reversed recovery cannot be redone under F (the cap is spent): the
finding stays raised and recovery is off-platform or a separately governed path (the Q-R21-2 reasoning, unchanged). The formula is
still written as the net, not as a set of existence checks, so that it stays correct if the cap ever changes; no partial-reversal
or depth-2 arithmetic was added, because under the cap it could never change a verdict.

### 21.3 RR-1 at the BOUND `destination_mismatch` site (decision 1)

`m4NotPaidRecoveredBound` (`payment_statement_m4.go`), consulted by `capturedUnposted` for payouts, i.e. at both bound sites
(`matchPayment`'s bound case with a line this run, `checkUnmatchedAttempts` with none). The BOUND-CLEAR-1 finding (keyed on X, the
post-M4 hint) stops **only** when ALL hold:

| Condition | Code |
|---|---|
| the park is `destination_mismatch` (the only bound reason M4 not-paid admits) and still holds X = the reference pinned at submission (`provider_reference_at_submission`) | `a.terminalReason`, `a.providerRef != ""`, `r.pinnedRef == a.providerRef` |
| the complete RR-1 predicate holds: NET recovery >= amount; exactly one distinct succeeded payout group (RR1-1) of the attempt's amount and asset; no reversed payout line (RR30-3) | `m4NotPaidRecovered` |
| that single payout is positively this park's: its reference is X, **or** a line of its group carries the attempt's merchant reference; and **no** line of the group names another merchant reference | group scan over `notPaidLines` |

A destination mismatch is itself never recovery. A later callback, a destination that later "looks valid", a debit alone (no
succeeded payout), an unrelated payout (another reference, no merchant reference) and a second payout are not read as recovery
and keep the finding raised. `destination_integrity_failure` is **not** M4-eligible and never stops here (owner decision 4; another
task owns it). R-1 and the bound site share one recovery: with an unrelated payout R-1 may stop (one recovered payout) while the
bound finding keeps raising (that payout is not positively this park's) - pinned.

### 21.4 Readings taken (RR30-n; the safest each time)

- **RR30-1 (subtraction side).** Unfiltered by reason code, wallet and asset (21.2). Under MA022 the wallet/asset filters would be
  equivalent; leaving them out removes a fail-open path should MA022 ever change.
- **RR30-2 (re-debit after an offset).** A debit whose causation is a credit (the correction of a correcting credit) is not counted:
  "same causation" is F. Fail closed: the finding stays raised; the operator path is off-platform or a governed path.
- **RR30-3 (reversed payout line).** `m4NotPaidRecovered` also requires that no `reversed` payout line exists on the references R-1
  reads. A recovery from the player after the PSP reported the payout reversed is a possible over-recovery; it must not silently
  stop the findings. This tightens R-1, STANDING-1 and the bound site alike (raise direction only). A `declined` line (the M4
  evidence shape) does not block the stop (tested).
- **RR30-4 (group attribution at the bound site).** "That line carries the attempt's merchant reference" is read over the
  dedupe group: at least one line of the group carries it and none names another; a copy without a merchant reference (a
  re-delivery on a matched reference) joins the group.
- **RR30-5 (M2 (d) without a visible withdrawal).** No recovery is read and (d) keeps raising (raise-direction fail closed; M2 keeps
  its availability, unlike the M4 joins of §19.3 which fail the run). An executed M2 payout always has its withdrawal (0115 CHECK).
- **RR30-6 (M2 (c2) unchanged).** `pay_declared_paid_compensated_but_paid` still counts recovery debits against the compensating
  credits (debit-only). It is not the M2 (d) rule and not in decision 2's scope; recorded as an open question (21.8).
  *Superseded by the review round: (c2) now uses the same net (21.11, LF C-c).*

### 21.5 Deliverable status

| Item | Status |
|---|---|
| Decision 2: NET recovery for M2 (d) | `IMPLEMENTED` |
| Decision 2: NET recovery for RR-1 (a) (R-1, STANDING-1 and the bound site) | `IMPLEMENTED` against MOCK |
| Reversed-debit exclusion (K2 credit caused by the debit; ledger reversal) | `IMPLEMENTED` |
| RR1-1 (one distinct payout) and the equal amount/asset rule kept | `IMPLEMENTED` (unchanged) |
| RR30-3 reversed payout line keeps raising | `IMPLEMENTED` |
| Decision 1: RR-1 at the bound `destination_mismatch` site (both bound sites) | `IMPLEMENTED` against MOCK |
| `destination_integrity_failure` | `NOT IMPLEMENTED` here by instruction (not M4-eligible; owned by another task) |
| Migration | none (0127 reserved; 0128 not needed: 21.6) |
| Review round: M2 (c2) on the same net (LF C-c) | `IMPLEMENTED` (21.11) |
| Review round: permanent-raise cases documented (LF C-b) | `IMPLEMENTED` (text, 21.12 and the runbook) |
| Review round: credit-after-stop monitoring rule (security LOW-1) | `NOT IMPLEMENTED` (tracked follow-up, 21.13) |

### 21.6 Why no migration

The net reads only `ledger_adjustment_requests` (executed rows are visible to the reconciliation session through
`tenant_system_read_executed`, 0113, whatever the reason code) and `ledger_transactions` (tenant isolation policy). The lookups
use existing indexes: `ledger_adjustment_requests_causation (tenant_id, causation_transaction_id)` for both the causation F and the
"credit caused by D" probe, and `idx_ledger_transactions_reverses` for the ledger reversal probe. The M2 wallet comes from
`withdrawal_requests` through `payment_manual_resolutions.withdrawal_request_id` (0115, NOT NULL for payouts). No column,
constraint, policy or function was needed.

### 21.7 Pins flipped deliberately

- `TestRR1_BoundDestinationMismatchPark_BoundFindingOutsideTheRuling` (r21: "bound finding kept") is replaced by
  `TestRR1_BoundDestinationMismatchPark_StopsUnderOwnerDecision1` (partial keeps raising; complete stops R-1 and the bound finding).
- `TestRR1_OtherCausation_Unrelated_Partial_KeepRaising`: its step "a compensating credit with the causation keeps raising, then
  debits stop it" is no longer reachable under the net (the credit is subtracted and the cap forbids re-debiting); the credit case
  moved to `TestRR30_DebitOffsetByCredit_KeepsRaising_NetReported`, and the test keeps its other-causation, unrelated and partial
  cases and its control.
- `declaredNotPaidButPaidResolutionHint` (M2 (d)) names the NET rule; `TestK3_Y11_NotPaidButPaidHintTextIsHonest` still holds.

### 21.8 Tests and evidence

- Pure (no database), `internal/reconciliation/payment_statement_rr1_test.go`: `TestRR30_Pure_ReversedPayoutLineKeepsRaising`
  (RR30-3, with the declined-line control) and `TestRR30_Pure_BoundDestinationMismatch_StopRuleAndEveryNegation` (every condition
  of 21.3 and its negation, through `capturedUnposted` and through `checkUnmatchedAttempts`; scope: `destination_integrity_failure`,
  another bound reason, a moved X, an empty X, no M4, no K3 evidence; R-1 stops while the bound finding raises on an unattributed
  payout).
- Integration (real stream, real M4 four-eyes execution, real K2 path, runtime-shaped role, private scratch databases),
  `internal/payments/m4_resolve1_rr1net_integration_test.go`:
  - recovered not-paid M4 and NET clearing: the controls of every test below;
  - debit-only false positive: a full debit stops, ONE unit of compensating credit with the causation re-raises both findings on
    every run with `recovered=839 of 840`, the re-debit is refused by INV-ADJ-6 and the findings stay raised
    (`TestRR30_DebitOffsetByCredit_KeepsRaising_NetReported`);
  - a credit of another reason code is subtracted (`..._CreditOfAnotherReasonCode_IsSubtracted`); a debit of another reason code is
    not recovery (`..._DebitOfAnotherReasonCode_IsNotRecovery`);
  - a reversed debit does not count, both by a K2 credit caused by the debit and by a planted ledger reversal
    (`..._ReversedDebit_DoesNotCount`);
  - wrong wallet and wrong asset are refused by K2 and keep raising; a credit under an unrelated causation does not offset
    (`..._WrongWalletOrAsset_NoRecovery`); wrong causation (even the full amount) stays pinned by
    `TestRR1_OtherCausation_Unrelated_Partial_KeepRaising` and, at the bound site, by `..._IncompleteRecoveries_KeepRaising`;
  - duplicate recovery: a replayed final approval is `ErrNotPending`, posts nothing and is not counted twice; a second full debit is
    refused by the cap (`..._DuplicateRecovery_ReplayIsIdempotent_NoDoubleCount`);
  - M2 (d) under the same net: offset by a credit, reversed debit (`TestRR30_M2d_NetRecovery`);
  - recovered `destination_mismatch` M4: the bound finding raises before any payout, after a debit alone, after a later succeeded
    callback; stops in-run and on standing runs and re-deliveries once complete; history rows kept, none resolved, park/attempt/
    withdrawal unchanged; a second payout raises again (`TestRR30_BoundDestinationMismatch_RecoveredStops_OnlyOnCompleteRecovery`);
    wrong causation, partial and offset keep raising (`..._IncompleteRecoveries_KeepRaising`); an unrelated payout, a payout on X
    naming another merchant, a second payout, an unequal amount and another asset keep raising (`..._UnattributedPayouts_KeepRaising`);
    tenant isolation with two tenants (`..._CrossTenantIsolation`);
  - the flipped pin `TestRR1_BoundDestinationMismatchPark_StopsUnderOwnerDecision1`.
- Final runs (`-race -tags integration -count=1 -p 1`, targeted `-run`, private scratch database `r30_rr1net`, no full-repo sweep;
  0 SKIP everywhere, so the integration tests did run): `internal/reconciliation/...` whole package set 436 PASS (incl. subtests) /
  0 FAIL; `internal/payments -run 'TestM4|TestB11|TestK3|TestB13B|TestRR|TestMA020|TestHSEC|TestC3' -timeout 90m` 319 top-level PASS
  (+284 subtests) / 0 FAIL / 0 SKIP (2341 s; a first attempt hit go test's default 10-minute timeout at 41 PASS and was re-run with
  `-timeout 90m`); after the last test edit, `-run 'TestRR1_|TestRR30_|TestRR4_|TestRR5_|TestM4Recon_|TestK3_C7_|TestK3_Y05_|TestK3_Y11_'`
  32 top-level PASS (+9 subtests) / 0 FAIL / 0 SKIP, and the reconciliation unit tests (`-race`) PASS. `go test ./internal/alerting
  -count=1` PASS. gofmt clean; `go vet ./internal/...` clean with and without `-tags integration`; golangci-lint 2.9.0
  `--build-tags integration --new-from-rev=dad803d ./internal/...` 0 issues. LOCAL evidence only; MOCK sources; no real provider.

### 21.9 Mutation evidence

`docs/plans/prh2-hardening-round/prh2-r30-rr1net-mutation-kill.txt`: 27 counted mutants (15 net formula / M2 (d) / RR-1 wiring, 1
RR30-3, 11 bound site), **24 killed**, 3 survivors disclosed as EQUIVALENT: N10/N11 (the debit wallet and asset filters; MA022
`causation_not_on_wallet`, as r21 A03/A04) and N12 (the credit subtraction's `state = 'executed'`; the system-shaped session sees
executed rows only, `tenant_system_read_executed`). N04 survived the first run and was killed after a test gap was closed (an
unrelated-causation credit). Three mutants were INVALID on the first run (an unused variable), fixed and killed.

### 21.10 Residuals and open questions (nothing here is decided by this change)

- **Q-R30-1 — RESOLVED in the review round (LF C-c; 21.11, `IMPLEMENTED`).** Original text kept: *(ledger-finance / owner;
  open).* M2 (c2) (`pay_declared_paid_compensated_but_paid`) still counts its recovery
  debit-only (debits caused by the compensating credits against the credited amount). It has the LOW-1 shape but is not the M2 (d)
  rule and is outside decision 2's text; extending the net there is a separate, small change if wanted.
- **Residual (RR30-2, by the cap).** Once a recovery under F is offset or reversed, it cannot be redone under F (INV-ADJ-6 counts
  the spent amount): the findings stay raised for good. That is the safe direction; the operator path is off-platform recovery,
  tracked through the rows' `investigation_status`, or a separately governed path (none exists; Q-R21-2 reasoning).
- **Residual (RR30-3).** A `reversed` payout line keeps R-1, STANDING-1 and the bound finding raised permanently, even when the
  platform later credits the player back (that credit lowers the net too). Noisy, never silent.
- **`destination_integrity_failure`.** Excluded by instruction and by owner decision 4; if the other task later makes it
  M4-eligible, extending `m4NotPaidRecoveredBound`'s reason scope is a deliberate change (pure test and mutant B02 pin the
  exclusion).
- §4.8 / F-1 and the D-7, T10 and S-3 launch flags are unchanged: everything here runs against MOCK sources only.

### 21.11 Review round (security + ledger-finance: APPROVE WITH CONDITIONS): M2 (c2) on the same net (LF C-c / Q-R30-1)

`pay_declared_paid_compensated_but_paid` (M2 (c2), `checkM2Standing`) now reads its recovery through the same `netRecovery`:
for each executed compensating credit C whose causation is the Step B transaction, recovery = executed `compensating_entry`
debits with causation C on the withdrawal's wallet and the resolution's asset, none reversed, minus every executed credit with
causation C; the per-credit nets are summed and compared with the credited total. INV-ADJ-6 caps the debits under each C at
C's amount, so the sum reaches the total only when every credit is fully and irreversibly recovered (two credits: recovering
one does not clear). No visible withdrawal: no recovery is read and (c2) keeps raising. Raise direction only; the (c)
annotation reports the same net. The hint names the net. `IMPLEMENTED`.

Tests: `TestRR30_C2_NetRecovery` (partial keeps raising and the remaining unit clears; a replayed approval is `ErrNotPending`
and not double counted; a credit under C re-raises; a reversed recovery debit counts zero; wrong wallet and wrong asset are
refused by K2, a full debit under an unrelated causation does not count; two credits must each be recovered; cross-tenant: B
cannot cite A's credit and B's recovery never clears A). The existing (c2) tests (`TestK3_Y05_`, `TestK3_Y06_`, the K3 recon set)
still pass. Mutants C01-C07 (21.14).

### 21.12 Findings with NO automatic stop path (LF C-b)

The following keep `pay_declared_not_paid_but_paid`, `pay_declared_paid_compensated_but_paid` and the post-M4
`pay_captured_unposted` (STANDING-1 and the bound site) raising **permanently**; nothing in the platform will stop them:
- a `reversed` payout line on any reference the M4 not-paid reads (RR30-3);
- an executed credit (any reason code) whose causation is F (for (c2): C): it is subtracted, and INV-ADJ-6 caps the debits under
  F (C) at the amount, so the net can never again reach it;
- a reversed recovery (a credit caused by the recovery debit, or a ledger reversal of it): the debit counts zero and the cap
  counts it as spent, so it cannot be redone under F (C).

The exits are off-platform recovery (PSP or player), tracked by staff through the rows' `investigation_status`, or a future,
separately governed path (none exists). Runbook: `docs/runbooks/operational-runbooks.md` §16, "Related: recovery findings with
NO automatic stop path".

### 21.13 Monitoring follow-up (security LOW-1, review round; tracked, `NOT IMPLEMENTED`)

The net sees only K2 entries whose causation is F (or C). After an RR-1 (or M2) stop, a four-eyes pair can return the money to
the same player through a credit with **no** causation (`goodwill_credit`; `operational_error_correction` /
`external_instruction` without one) or with an unrelated causation; the net cannot see it. Tracked follow-up (not built): a
monitoring rule "flag any `credit_player` on the same wallet after an RR-1 stop". Until it exists the mitigation is the four-eyes
approval and the periodic K2 audit review (runbook entry above).

### 21.14 Review-round evidence

- Mutation (appended to `docs/plans/prh2-hardening-round/prh2-r30-rr1net-mutation-kill.txt`, run 3): 7 counted (c2) mutants
  C01-C07 (debit-only sum; recovery read under the Step B causation instead of each credit; threshold off by one; only the first
  credit read; and the shared-SQL rules N01, N05, N14 re-run against the (c2) tests), **7 killed**, 0 survivors. C02 was INVALID on
  its first attempt (an unused variable), fixed and killed. Control: pure 31 PASS, integration 33 PASS, 0 FAIL, 0 SKIP.

- Runs after the merge of main (`72bf943`) and the review-round changes (`-race -tags integration -count=1 -p 1`, targeted, fresh
  private scratch database; 0 SKIP everywhere, so the integration tests did run): `internal/reconciliation/...` 436 PASS (incl.
  subtests) / 0 FAIL; `internal/payments -run 'TestRR1_|TestRR30_|TestRR4_|TestRR5_|TestM4Recon_|TestK3|TestMA020'` 121 top-level
  PASS (+65 subtests) / 0 FAIL / 0 SKIP; reconciliation unit tests (`-race`) PASS; `go test ./internal/alerting -count=1` PASS;
  gofmt clean; `go vet ./internal/...` clean in both tag modes; golangci-lint 2.9.0 `--new-from-rev=dad803d` 0 issues.

## 22. Ruling: instrument state does not gate settlement (ADR 0095 section 48 decision 3) - verified, tested, no production change (`payments`, 2026-10-09; branch `gov-r31-settle`, base `dad803d`)

### 22.1 The ruling and what it replaces

**Owner decision 3 (DECIDED 2026-10-09, ADR 0095 section 48):** instrument state MUST NOT gate settlement. Once a payout has been properly
authorised, bound and snapshotted, a later instrument-state change (suspension, revocation, verification expiry) must not automatically prevent
settlement of that already-authorised transaction. Instrument eligibility remains relevant to initiation and dispatch under the existing policy.

This is now a **ruling**, not a design statement. It replaces the "design statement" status of section 2.4's row "Polls / evidence on an existing
attempt: not blocked by instrument state (LF95-C10(d))" and the matching sentence of section 2.6 for every evidence path. Nothing else in sections
2.4 to 2.6 changes.

### 22.2 Verification of the merged code (read, then proven by tests) - STATUS: IMPLEMENTED, NO PRODUCTION CHANGE

The only callers of `EvaluateGate` (the live-instrument gate, which reads the instrument, its verification and its blocking events) are the
**dispatch** gates, through `gatePayoutDestination`: request creation (`withdrawal.RequestWithdrawal`), T1p (`ClaimForDispatch`), phase B
(`payoutAdapterCall`) and T2/T12 (`destinationGateAndEscalate`). No evidence path reaches it:

| Evidence path | Destination check it runs | Reads current instrument state? |
|---|---|---|
| sync phase C (`ApplyPayoutResult`) | `payoutDestinationEvidence` | no |
| `QueryStatus` poll (`PollPayoutStatus`, sweeper `resolvePayoutViaQueryStatus`) | `payoutDestinationEvidence` | no |
| callback / receipt (`ApplyReceiptEvidence`) | `payoutDestinationEvidence` | no |
| late evidence on a terminal or parked attempt | `payoutTerminalDestinationSignal` / the post-M4 cells | no |

`payoutDestinationEvidence` runs `CheckSnapshot` (the write-once snapshot loaded and seal-verified, then required to equal the withdrawal, the
attempt and the bound instrument **id and fingerprint as recorded on the withdrawal row**, plus amount and asset) and `CompareEcho` (the provider's
echo against the **snapshot's** fingerprint and kid). `CheckSnapshot` documents and implements that it never reads the instrument's current state. The
database side agrees: the snapshot and binding guards are `BEFORE INSERT` only (`payout_attempt_destination_snapshots_before_insert`,
`withdrawal_requests_payout_binding_guard`), and the deferred `payment_attempts_require_destination_snapshot` fires on attempt insert only, so a later
state change cannot reject the settlement transaction.

### 22.3 What stays gated (unchanged; HD-R15-5 stays OPEN and unaffected)

A **new** request (generic 409 `PAYOUT_INSTRUMENT_NOT_USABLE`, no row, no hold, key not consumed), T1p, phase B and T2/T12 keep applying the gate rule
and the tiering predicate exactly as in section 2.4. An approved withdrawal whose instrument became unusable **before** dispatch is still refused at
T1p (the request stays `approved`, hold kept) and an instrument that becomes unusable between T1p and phase B still stops the provider call (NotSent). Both
are initiation/dispatch, not settlement, and are the parked behaviour of HD-R15-5, which this section does not touch. A destination echo mismatch and a
broken or mismatching snapshot still park (`destination_mismatch`, `destination_integrity_failure`) whatever the instrument's state: the decision removes
the instrument-state gate, not the destination-integrity checks.

### 22.4 Tests (real PostgreSQL, runtime role) and mutation evidence

`internal/payments/r31_settle_instrument_state_integration_test.go` (every flow runs through the runtime-role pool on a per-test scratch database migrated to
the latest migration; the owner pool only seeds fixtures, writes the max-age configuration and performs the deliberate privileged corruption of (d), (e)):

- (a) `TestR31_A_`: verified instrument, request, approval, T1p snapshot, then suspended / revoked / verification expired (by time, and swept to
  `verification_expired`) / row changed; a success **sync**, **poll** (sweeper) and **callback**, and a definite **decline** on each, settle with an outcome
  deep-equal to the untouched-instrument baseline of the same path (withdrawal and attempt state, ledger entries by transaction type, account type, direction
  and amount, hold net 0, one `withdrawal.completed` or `withdrawal.failed` audit row, no park), with `SUM(D)=SUM(C)` and projection = rebuild.
- (a2) `TestR31_A2_` (ledger-finance condition): an `ambiguous` attempt whose instrument then becomes suspended / revoked / expired (swept and by time) is escalated by the
  sweeper's T12 destination gate (T16, one denial audit row, hold kept, the provider's Withdraw called exactly once: no resend). A later callback success or decline still
  settles, deep-equal to the untouched-instrument baseline (no park, no second Withdraw call).
- (b) `TestR31_B_` and the HTTP test `TestRequestWithdrawalHandler_R31_UnusableInstrumentStates_Generic409`: unverified, suspended, revoked, expired (by time and
  swept): refused, generic 409, no row, no hold, key unconsumed (the same key then succeeds); the binding guard refuses a direct INSERT (PI042).
- (c) `TestR31_C_`: T1p and phase B still refuse an instrument that became unusable before dispatch.
- (d) `TestR31_D_`: the snapshot is write-once for the runtime role (UPDATE, DELETE, a second INSERT refused); the instrument identity columns are immutable for it
  (PI011); after the state, mask and detail (label) of the instrument row are changed (owner, triggers off) the attempt still settles against the snapshot, which
  is unchanged, and a mismatching echo still parks.
- (e) `TestR31_E_`: no cross-player substitution: request (not usable), direct INSERT (PI041), re-bind UPDATE refused, staff body `payment_method` refused
  (`ErrPaymentMethodMismatch`), a snapshot naming the other player's instrument refused (PI050), and privileged re-pointing of the withdrawal parks
  `destination_integrity_failure` instead of settling on the other destination.
- (f) `TestR31_F_`: a mismatching echo parks as `destination_mismatch` (P1, hold kept, nothing posted) under no change, suspended, revoked and expired, on sync, poll and
  callback, and for a decline; a suspended instrument never turns a mismatch into a settlement.

Mutation evidence: `docs/plans/prh2-hardening-round/prh2-r31-settle-mutation-kill.txt`: M1 (the dispatch gate added to the evidence check), M4 (settlement of an escalated attempt gated on the escalation, killed by (a2)) and M2 (the snapshot coupled to
the live instrument row) are killed by (a) and (d); M3 (the dispatch gate weakened) is killed by (b) and (c). 4 counted, 4 killed.

### 22.5 Residuals and boundaries

- HD-R15-5 (what happens to an approved withdrawal whose instrument is unusable at dispatch) stays OPEN; the parked behaviour is unchanged and only observed.
- The decision does not make an instrument-state change a reason to cancel or reverse anything; operators who suspend an instrument with a payout in flight rely
  on the existing exceptional-resolution paths (decision 7, section 4), not on settlement being blocked.
- The tests drive MOCK providers only. The HTTP-level request test uses the repository's standard owner test pool (the server fixtures are built that way); the
  runtime-role proof of the same refusal is `TestR31_B_`.



## 23. Governed exit of a `destination_integrity_failure` park (owner decision 4, ADR 0095 §48) (appendix, `ledger-finance`, 2026-10-09; branch `gov-r32-integrity`, base `dad803d`; migration 0127)

Section numbering: §21 and §22 are left to sibling branches of the same cycle; this section is self-contained. The design above is
**not** rewritten. Owner decision 4 is authoritative and is **not** broadened: there is no automatic exit; the park stays held
until a controlled four-eyes resolution; no unilateral staff override; no provider callback or poll changes the destination or
releases the hold; no automatic release, settlement or beneficiary reassignment; no generic admin override. Owner decision 6 (the
D-7 evidence standard) is the evidence bar. Every provider and statement source in the tests is `MOCK`; nothing here is a
statement about a real PSP, a custodian or a licence.

### 23.1 The three controlled outcomes and their status

| Outcome (decision 4) | Mechanism | Status |
|---|---|---|
| **Remain parked** if the evidence is ambiguous (the default) | The park itself (B13-B): attempt `disputed` / `destination_integrity_failure`, withdrawal `submitted`, hold kept, B12 P1, reconciliation finding. No job, sweeper, poll, callback or reconciliation path changes it. | `IMPLEMENTED` (verified, 23.2) |
| **Controlled cancellation/release** where existing financial policy permits | The existing M4 **not-paid** resolution (`m4_evidence_not_paid`, §4), admitted for this reason by migration 0127. Hold returned to the player's **own** `player_cash` only. | `IMPLEMENTED` against `MOCK` |
| **Resume** when the authoritative destination is positively established | 23.3 (a new attempt against the platform-authoritative bound instrument; never a rebind, never a snapshot rewrite) | `NOT IMPLEMENTED` — **DESIGN ONLY** |
| (not an outcome) M4 **paid** / M2 / any completion against the hold | Refused in the database (`MR012`) and in Go | `REFUSED` by design |

### 23.2 What migration 0127 implements (the smallest safe change), and what was verified

**Change (as first written; the review round in 23.6 widens 0127 in the refusal direction).** One function, `payment_m4_in_scope` (0125), is replaced (`CREATE OR REPLACE`, same OID, signature, `IMMUTABLE`, pinned
`search_path`): the not-paid-only arm admits `destination_integrity_failure` beside `destination_mismatch`, with or without a bound
reference. The paid arm is unchanged (only the unbound reasons with no reference), so M4 paid on this reason stays `MR012`. Go:
`M4ResolvableDispute` restates it (parity test over the reason x state x kind x reference matrix, admitted combinations 10 -> 12,
flipped deliberately). The down refuses (`MR099`) while **any** M4 row (any kind, any state) has
`terminal_reason_at_submission = 'destination_integrity_failure'`, and otherwise restores the 0125 body **byte for byte** (whole-schema
up/down/up test: exactly the one function line differs; the restored `prosrc` is matched against the 0125 file text).

**Verified unchanged and reason-agnostic (so nothing else needed to change):**

| Object | Why it is already correct for this reason |
|---|---|
| `payout_m4_evidence` | Reads by the platform-issued merchant reference, by the attempt's bound reference when it holds one (an integrity park binds the provider's validated, unconflicted reference first, B13B-17 LF H-2), by every typed Y and by every matched reference D; never reads the snapshot. The D-7 bar is unchanged: sealed eligible import, coverage `[created_at, last_sent_at + 24 h]`, a declined line of equal amount/asset after the last send, no succeeded/pending/reversed line on any matched reference in any import, declaration integrity; `last_sent_at IS NULL` is `insufficient`. |
| insert guard (`payment_manual_resolutions_guard`) | DB-forced amount/asset (R-4), pinned `provider_reference_at_submission` (C-6/R-3), `basis_code = provider_confirmed_out_of_band`, evidence recomputed and the deterministic line required. |
| `-> executing` (same guard) | Scope re-check (now including the reason), reference pin, withdrawal `submitted`, amount/asset equality, evidence re-run (S-2); the recount `payment_manual_resolution_execution_status` with the `platform_acting` floor (S-6). |
| MR041 (`..._no_executing_commit`) | Withdrawal `failed` and linked; `withdrawal_failed` keyed `wr.id:failed`, no provider id/tx id; exactly two legs: hold debit and `player_cash` credit, both on the withdrawal's **own** wallet, amount and asset equal; attempt fields equal the pins. |
| Fence (g), `ledger_entries_governed_fence`, acting K3 arms, `tenant_system_read_executed` | Keyed on the kind `m4_evidence_not_paid`, never on the reason. |
| Go (`manual_resolution*.go`) | `m4ExecutionRefusal` uses `M4ResolvableDispute`; `requestInTx` / `m4EvidenceRefusal` verify the import seals on every path (L-2 pin unchanged); `postM4` calls `withdrawal.Fail` only. The resolution audit carries no snapshot data, so a missing snapshot cannot break it. |
| Reconciliation | `payoutDisputeReasonClasses` already classifies the reason bound-if-referenced (B13B-13). R-1 (`pay_declared_not_paid_but_paid`) and the RR-1 stop rule are keyed on the executed M4 not-paid row, not the reason: an integrity park is treated **exactly** like a `destination_mismatch` park. Shown correct by test (23.4): a later succeeded line on the bound reference raises R-1 with the post-M4 hint; after a full K2 recovery R-1 stops while the BOUND finding keeps raising (RR1-2, identical to the `destination_mismatch` pin). |
| §4.8 post-M4 cells | Keyed on the executed M4 and the withdrawal state; `payoutDestinationEvidence` is a no-op on a `disputed` attempt (R22-3), so a later success on the bound reference raises `success_after_m4_not_paid` (raise-only, MOCK). |
| Hint | `destinationIntegrityPayoutCapturedUnpostedResolutionHint` now reads "payout parked on a destination integrity failure: no completion against the player's hold and no M4 paid; M4 not-paid only on positive decline evidence; PSP recall/return or off-platform recovery; never allocation" (was "... no M4 route (open owner/architect question) ..."). It names no completion, rebind or resume. Pin `TestC3_DestinationHint_LiveAfterClassification_SelectedByReason` flipped deliberately. |
| Alerting | No new raise site, Kind or reason; `go test ./internal/alerting` pins unchanged and passing. |

**Readings taken (R32-n, safest each time).**
- **R32-1 (why not-paid is safe on an untrustworthy attribution).** Brief 2's concern is that neither "paid" nor "not paid" can be
  taken at face value while the destination is in doubt. That holds for **paid** (refused). For **not paid**, the snapshot plays no
  part in either the verdict or the posting: the verdict rests on platform keys (merchant reference, the platform-held bound reference,
  typed Y) over sealed statement imports, and the posting only moves the hold to the cash account of the withdrawal's own wallet. A
  wrong not-paid can only produce a double payout to whichever destination the PSP used, which R-1 raises on every run and the §4.8
  cell raises in real time (MOCK), recoverable only through K2 under the RR-1 rule. That is the same residual every M4 not-paid
  already carries, so D-7's "correct destination/instrument where applicable" is **not applicable** to a positive non-payment finding.
- **R32-2 ("where existing financial policy permits").** The existing policy that releases a `submitted` payout hold on positive
  non-payment evidence is M4 not-paid. HSEC (needs `approved` and no attempt) and M2 (excludes the reason) do not apply; no new
  release path was created.
- **R32-3 (reference shapes).** Admitted with and without a bound reference, as for `destination_mismatch`: an integrity park made by
  a poll or callback holds the reported reference; one made by sync phase C on a reference-less result holds none. Either way the
  reference is pinned at submission and re-checked at execution.
- **R32-4 (down refusal scope).** Any kind and state: a pending row would be stranded and an executed one would describe a release the
  restored scope no longer admits.
- **R32-5 (guard message text).** The `MR012` message in `payment_manual_resolutions_guard` still reads "destination_mismatch not-paid
  only". Changing it means replacing the whole guard (0125, ~400 lines) for text; the SQLSTATE and the behaviour are correct. Cosmetic
  residual, recorded.
- **R32-6 (never-sent parks).** An integrity park with `last_sent_at IS NULL` cannot reach `not_paid` (the 0125 rule) and remains
  parked. Integrity parks are only written by the evidence paths (sync phase C, poll, callback) on a sent attempt, so this is not
  expected; if it occurs the park stays held (fail closed) until 23.3 exists.

### 23.3 "Resume when the authoritative destination is positively established" — DESIGN ONLY (`NOT IMPLEMENTED`)

**Constraint.** The snapshot is write-once (0123), keyed by attempt. A resume must therefore never rebind the withdrawal, never write
or repair a snapshot in place, and never treat a provider echo, callback or poll as establishing the destination (decision 5).

**Interim operational equivalent (available today, no code; a RECOMMENDATION, not a decision).** Execute M4 not-paid (the hold returns
to the player's cash), then the player places a new withdrawal bound to the same instrument through the normal flow: request-time gate,
KYC/RG, approval, T1p gate, a **fresh** write-once snapshot, phase B gate. Nothing from the corrupted snapshot is reused. Cost: the
player must act again and the request is re-approved.

**In-place resume (R-B), if the owner wants one.** A new K3 kind `m5_destination_resume` on `payment_manual_resolutions`:
- **Eligibility (all, re-checked at execution).** (E1) The old attempt is `disputed` / `destination_integrity_failure` and its park
  audit row's `gate_reason` is snapshot-confined: `snapshot_missing`, `snapshot_mismatch`, a snapshot `seal_invalid`, or
  `destination_gate_unavailable`. A park whose present-time instrument checks fail (`seal_invalid` / `fingerprint_mismatch` /
  `binding_mismatch` / `detail_unavailable` / verification faults on the **instrument**) is never resumable: the authoritative destination
  itself is in doubt; only not-paid or remain parked. (E2) The **platform-authoritative destination** is positively established now:
  the withdrawal's immutable binding (`payout_instrument_id`, `payout_instrument_fingerprint`) resolves to an instrument whose seal
  verifies under the B13 key, whose fingerprint recomputes from the decrypted detail, whose latest verification is the one the gate
  expects, of the same player and brand, usable for the asset, and `CheckTier` passes on the exact routed adapter, all under
  `FOR SHARE` (the existing `EvaluateGate`). (E3) The old attempt is positively **not paid**: `payout_m4_evidence = not_paid` under the
  D-7 standard, seals verified in Go, at request and at execution. (E4) An investigation finding (closed code) and an evidence hash
  (forensics/portal confirmation) bound into the payload hash. No provider-supplied field contributes to E1-E4 except as the
  statement evidence of E3.
- **Who.** A new capability `payout_destination_resume:request|approve` (not `payment_force_resolve`: the D-9 lesson), requester and
  final approver `platform_acting` only (R19-1 analogue), the DB `platform_acting` floor in the recount, LF-11 distinct Persons, S-12
  (never the beneficiary), S-2(iii) (no policy author), K3 recount and expiry. ADR 0110 signed actor proofs with two new operations
  (`payout_resume:request`, `payout_resume:approve`); the request digest binds tenant, withdrawal, old attempt, instrument id,
  fingerprint, the not-paid evidence line and import ids, the finding code, the evidence hash and the reason code.
- **Execution (final approval's own transaction; lock order L1 withdrawal -> old attempt -> resolution).** Re-run E1-E3 (DB first,
  Go seal check second); then create a **new** `payment_attempts` row for the same `submitted` withdrawal (new id, new platform merchant
  reference, `created_by_resolution_id` UNIQUE) and write its snapshot through the existing T1p snapshot writer **from the instrument**
  (never from the old snapshot). **No ledger posting** (the hold already covers the amount). Dispatch then runs through phase B and its
  gates; kill switch and H-SEC tenant state refuse as usual. The old attempt stays `disputed` / `destination_integrity_failure` forever
  (A-15, history), its P1 stays, and every reconciliation predicate on it keeps running: a later success on the old attempt's
  references raises as a possible double payout.
- **Idempotency / concurrency.** One pending or executed resume per old attempt, mutually exclusive with M2/M4 through the existing
  per-attempt partial UNIQUE; the new attempt is created at most once (UNIQUE `created_by_resolution_id`); a retried decide is refused
  (not pending) and creates nothing; two final approvals serialise on L1 and the resolution `FOR UPDATE`; sweeper and poll on the old
  attempt are no-ops (`disputed`).
- **Why it is not built now.** It is not small and reuses only part of the machinery: it needs a new kind, capability and two proof
  operations; an attempt-creation entry point for a `submitted` withdrawal (T1p today requires `approved`); guard and MR041 rules for a
  posting-free execution; reconciliation of a withdrawal with two attempts (one disputed forever) in the ledger join, STANDING-1 and
  BOUND-CLEAR-1; and a threat model for an attacker who corrupts a snapshot to force a re-dispatch.
- **Review it needs before any implementation:** `architect` (new kind, attempt lifecycle on a submitted withdrawal, ADR amendment);
  `security` (capability and proof operations, E1/E2 gate re-evaluation, the re-dispatch threat model, decision 5); `ledger-finance`
  (posting-free execution, double-payout prevention on the old attempt, multi-attempt reconciliation); `qa` (test matrix);
  `product-owner-proxy` (whether the interim equivalent above makes R-B unnecessary); then the **owner** (whether an in-place resume is
  wanted at all, and its capability semantics).

### 23.4 Tests and evidence

`internal/payments/m4_integrity_integration_test.go` (`TestR32_*`; real PostgreSQL, private scratch databases, the runtime-shaped
non-superuser non-BYPASSRLS role; every park made by the real B13-B writer through the real QueryStatus poll after an owner-level
snapshot tamper, both "missing" and "tampered"): end-to-end not-paid (request, final approval, execution, the two ledger legs exactly
once on the player's own wallet, `psp_clearing` untouched, attempt and snapshot untouched, audit trail, idempotent replay of decide and
request); M4 paid and M2 refused (DB `MR012` and Go, both reference shapes, and the Go execution precondition); R19-1 (tenant staff
cannot request, tenant approvals never meet the floor, a tenant session cannot move to `executing`, the requester cannot self-approve);
four-eyes with a two-approval policy (one approver never executes); insufficient/ambiguous/contradictory evidence keeps the park (no
evidence, pending, reversed, succeeded on the bound reference, unsealed success, unsealed decline, short coverage, other amount, no
declaration, and evidence contradicted after the request); no automatic exit (sweeper, polls with and without echo on fresh and stale
copies, callbacks with and without echo, twice) and then the governed exit still executes once; two racing final approvals (exactly one
`withdrawal_failed`); cross-tenant (request, evaluation, another tenant's lines); `destination_mismatch` cells unchanged beside an
integrity park; reconciliation hint, R-1 and RR-1 (23.2); migration up/down/up whole schema with byte-for-byte restore, and the down
refusals (pending and executed integrity M4) and the allowed down with only a `destination_mismatch` M4. Pins flipped deliberately:
`TestM4_ScopeParity_GoAndDB` (10 -> 12), `TestC3_DestinationHint_LiveAfterClassification_SelectedByReason` (integrity text). The two
head-relative down helpers (`m4DownTo0125`, the HSEC `downTo0124`) now derive their step count from the migration files.
`TestR32_QR322_*` pins the Q-R32-2 behaviour (23.5) as current, so a tightening flips it deliberately.
Mutation evidence: `docs/plans/prh2-hardening-round/prh2-r32-integrity-mutation-kill.txt` (14 counted, 14 killed, 0 survivors).
Final runs (`-race -tags integration -count=1 -p 1`, targeted `-run`, private scratch databases, 0 SKIP everywhere so the integration
tests did run): payments `TestR32_` 13 PASS / 0 FAIL; `TestR32_IntegrityPark_Concurrency_TwoFinalApprovals` at `-count=10` 10 PASS, no
race report; payments `TestR32_|TestM4|TestRR1_|TestRR4_|TestRR5_|TestPostM4_|TestPayoutPostM4Cell|TestB13B_|
TestHSEC_HoldRelease_Migration0124DownRefusals|TestC3_|TestL2_|TestB11_` 164 PASS / 0 FAIL (before the Q-R32-2 pin was added);
reconciliation `TestRR1_|TestRR4_|TestR1_|TestR2_|TestC3_|TestRes1_|TestPayoutReason|TestPayoutDisputeReasonClasses|TestB13B|TestRS`
33 PASS / 0 FAIL; payoutinstrument `TestMigration0126|TestBinding_InsertGuard` 3 PASS; httpserver `TestForceResolutionAPI_M4` 2 PASS;
`go test ./internal/alerting -count=1` PASS. gofmt clean; `go vet` clean with and without `-tags integration`; golangci-lint 2.9.0
`--build-tags integration --new-from-rev=dad803d ./internal/...` 0 issues. LOCAL evidence only.

### 23.5 Open questions (nothing here is decided by this change)

- **Q-R32-1 (CONFIRMED by ledger-finance as the conservative reading, 23.6).** Decision 1 makes RR-1 apply to a recovered `destination_mismatch` park. The same
  bound-site rule would be financially correct for a recovered integrity park (the recovery accounting is identical), but decision 1
  names only `destination_mismatch`. Until extended, the BOUND finding of a recovered integrity park keeps raising (safe, noisy), and an
  implementation of decision 1 must not silently key on "any destination reason".
- **Q-R32-2 (CLOSED by the 23.6 fix, security C-1).**
  Not-paid (ii) attributes a declined line by the merchant reference **or** the bound reference alone; a declined line on the bound
  reference that names **another** merchant reference is not refused (the paid branch has the H-2 rule; not-paid has none). Recommend
  `insufficient` for not-paid when any line read on the bound reference names another merchant reference. Not changed here (it rewrites
  `payout_m4_evidence` for all reasons).
- **Q-R32-3 (owner; still open).** Is the interim equivalent of 23.3 sufficient, or is an in-place resume (R-B) wanted? Ledger-finance: the interim position (remain parked; M4 not-paid; then a new withdrawal) is sufficient from the financial side.
- Launch flags unchanged: D-7 provider-dependent parts, T10, S-3 per real source, F-1 for non-MOCK.

### 23.6 Review round (security APPROVE WITH CONDITIONS, ledger-finance REJECT with one HIGH; fixes applied, 2026-10-09)

Every change below is refusal-direction; none broadens owner decision 4 or 6. All of it is folded into migration 0127 (unmerged),
built on the current 0125 bodies; the down restores both 0125 function bodies byte for byte (whole-schema test plus a `prosrc`
match against the 0125 file for each) and drops what 0127 added.

**LF HIGH: a provider success the platform already saw was invisible to the not-paid verdict.** B13-B parks a destination payout
mostly on SUCCESS evidence, but `parkPayoutDestination` only bound the reference; `payout_m4_evidence` reads statement lines and Y, so
the reproduction (MOCK poll reports success on X, park, then a sealed declaring import with a decline on another reference carrying
the merchant reference) read `not_paid` and M4 not-paid released the hold.
- **Record.** New append-only table `payout_destination_park_evidence` (tenant, attempt, destination reason, `reported_outcome` in
  `succeeded|declined|pending|ambiguous|unknown_pre_0127`, `evidence_kind`, `recorded_at`; UNIQUE per (attempt, outcome, kind) so
  repeats add nothing). It mirrors the reviewed 0115 `payment_attempt_reference_evidence` pattern: FORCE RLS; INSERT and SELECT only in
  the **system shape**, SELECT for a **valid acting session** (the M4 evaluation); no tenant-staff, player, platform or UPDATE/DELETE
  policy; `ledger_deny_mutation` on UPDATE/DELETE/TRUNCATE; runtime grants SELECT, INSERT only; an insert guard (`MR064`, new) admits a
  row only for a payout attempt currently `disputed` on that same destination reason and never the backfill marker.
- **Writers (every path).** `parkPayoutDestination`, the single park writer used by sync phase C, the QueryStatus poll and the
  callback/receipt cell, records the evidence class in the park's own transaction. A success reaching an attempt **already** parked on
  a destination reason is recorded by `recordSuccessOnDestinationPark` from the three disputed branches: the poll (`case
  AttemptDisputed`, success), the callback/receipt (`case AttemptDisputed`, success) and the late-evidence path (a sync or poll result
  that lost the CAS to the park). It changes no state, posts, releases or raises; the post-M4 cell still runs after it.
- **Verdict.** In the not-paid branch of `payout_m4_evidence`: a `succeeded` record gives `contradictory`; an `unknown_pre_0127` record
  gives `insufficient`. Both destination reasons. Re-evaluated at `-> executing` like every verdict input (a success that arrives after
  the request is refused at execution).
- **Backfill (fail closed).** 0127 up writes `unknown_pre_0127` for every destination park that already exists (their trigger is
  unknown), lifting FORCE RLS on `payment_attempts` for that one statement only. The down refuses (`MR099`) while any park-evidence row
  exists (dropping it would reopen not-paid on a success-parked `destination_mismatch` park), in addition to the integrity M4 rows.
- **R-1 (decided: no change).** A success recorded **before** execution makes the M4 impossible (DB re-check at `-> executing`). A success
  recorded **after** an executed not-paid arrives through exactly the callback/poll/late paths where the §4.8 cell raises
  `success_after_m4_not_paid` (audit + P1) in real time; R-1 stays statement-based and raises when the line arrives. Reading the record
  in reconciliation would add a second, non-statement source to `loadK3Evidence` for no additional detection.

**Security C-1 / LF Q-R32-2: not-paid on an ambiguous attribution.** Before the line selection, the not-paid branch now returns
`insufficient` when any payout line on the bound reference or on any matched reference (`v_rs`) names a non-NULL merchant reference
other than the attempt's. These lines are already inside the 64-line read set (S-4). It tightens every M4 not-paid
(`destination_mismatch` and the unbound reasons as well). Reconciliation R-1 mirrors it: after an executed not-paid, such a line on any
reference R-1 reads raises `pay_declared_not_paid_but_paid` with `check=m4_not_paid_attribution_ambiguous` (raise only; not subject to
the RR-1 stop rule, it is not a recovered payout). The former `TestR32_QR322_*` current-behaviour pin is replaced by
`TestR32_C1_*` (flipped deliberately).

**Security C-2 (LOW), recorded residual.** After this round, an M4 not-paid on a destination park can still be wrong only if the
snapshot was corrupted (needs DB-owner access, ADR 0110 T6) **and** the payout actually went out **and** the only trace of it is a
succeeded line in a statement source that does not declare `payout_lines_carry_merchant_reference`, carrying an unrelated reference
and no merchant reference (never matched by the merchant reference, the bound reference, Y or `v_rs`), with no provider success ever
reported to the platform for the attempt. That is the S-3 source-completeness residual: `PROVIDER DEPENDENT`, launch-blocking per real
source as already recorded (§4.3, §10.3).

**Security C-3 (INFO).** `TestR32_C3_RawWritesWithoutProof_EverySessionShape`: a raw M4 INSERT on an integrity park without an actor
proof is refused in the `platform_acting` (`AP001`), tenant-staff (`AP001`/`MR060`), system (`CG001`/`MR001`) and platform-admin shapes;
a raw cancel UPDATE and a raw approval INSERT without a proof are refused in every shape; nothing moves.

**MR012 message text.** Left unchanged and recorded (R32-5): fixing the text means replacing the whole `payment_manual_resolutions_guard`
(0125, ~400 lines) for wording only; the SQLSTATE and the behaviour are correct.

**Questions.** Q-R32-1 confirmed by ledger-finance as the conservative reading (decision 1 names `destination_mismatch` only). Q-R32-2
closed by C-1. Q-R32-3 stays an owner question; ledger-finance finds the interim position sufficient.


---

## 25. D-7 conformance: the M4 evidence standard as a traceable model (appendix, `qa`, 2026-10-09; branch `gov-r34-d7`, base `dad803d`; the design above is not rewritten)

**What this section is.** Owner decision 6 (D-7, ADR 0095 §48) APPROVED the M4 evidence standard: before any non-MOCK M4 payout resolution or payment the platform must hold positive evidence sufficient to establish, as applicable, (1) the correct withdrawal, (2) the correct payout attempt, (3) the correct provider transaction/reference, (4) the correct player/tenant context, (5) the correct asset, (6) the correct amount, (7) the correct destination/instrument, (8) the final provider status, and (9) the causal relationship between the provider evidence and the attempt; **no single untrusted provider field may be sufficient to establish all of this**; ambiguous evidence parks, alerts and is investigated, and is never guessed, paid or resolved automatically. This section supersedes the "D-7 (OPEN, owner)" bullet of §17.4: the standard is decided. It does **not** enable non-MOCK M4 and connects no provider. The matrix below was built by reading migration 0125, `manual_resolution*.go`, `payout_post_m4.go` and `internal/reconciliation/payment_statement_{m4,k3}.go`, not from the ADR text above; line numbers refer to `migrations/0125_payout_unbound_resolution_m4.up.sql` at the base.

### 25.1 Deliverable status

| Item | Status |
|---|---|
| D-7 matrix (25.2-25.3) and the conformance suite (25.8) | `IMPLEMENTED` against MOCK (documentation and tests) |
| Go-side, refusal-only eligibility check (25.4: three checks the SQL verdict does not carry) | `IMPLEMENTED` (no migration; migration 0125 and the SQL functions are untouched) |
| SQL equivalents of those three checks, and a positive destination clause | `NOT IMPLEMENTED` (need a migration; reported, 25.4; the `0127` owner or a later migration) |
| Adapter contract and conformance checklist (25.6) | `IMPLEMENTED` as an interface plus a pure checker; **every provider-specific implementation is `PROVIDER DEPENDENT` and `NOT IMPLEMENTED`** |
| Non-MOCK M4 (paid or not paid) | **`BLOCKED`** (25.7) |

### 25.2 The matrix, short form

| # | D-7 property | Enforced by (exact) | Re-checked when the final approval executes | Proved by (`internal/payments`) | Verdict |
|---|---|---|---|---|---|
| 1 | correct withdrawal | resolution's `withdrawal_request_id` is DB-forced from the attempt (guard `:634`); withdrawal `submitted` (`:694`); amount/asset equal (`:697`) | DB `-> executing` (`:893-899`); Go `m4ExecutionRefusal`; MR041 (`:976`, ledger `correlation_id` = withdrawal; entries on the withdrawal's wallet `:1015-1017`) | `TestD7_P1_WrongWithdrawal` | enforced |
| 2 | correct payout attempt | M4 scope `payment_m4_in_scope` (`:203`, request `:691`); evidence is read only by the attempt's platform-issued merchant reference / bound reference / typed Y (`:278-312`); one reference naming two merchant references is `contradictory` (H-2 `:353-358`); the requested line must be the deterministic line (`:753`) | DB `:889-905`; Go scope + reference pin + re-evaluation (`m4EvidenceRefusal`); MR041 pins state/reason/reference (`:984-996`) | `TestD7_P2_WrongAttempt` | enforced |
| 3 | correct provider transaction/reference | exactly one succeeded group (`:332`); R must not carry the reserved prefix, be attributable (`payment_y_attributable`), have a tombstone, be held by another attempt as reference or typed Y, differ from the attempt's own Y, equal a `withdrawal_requests.provider_reference`, key a ledger row or the `provider:R` idempotency key (`:367-385`); the importer's CHECK refuses a reserved-prefix line; **Go: R is not any platform-issued merchant reference (25.4 G-REF)**; R is pinned into the resolution, the ledger key `provider:R` and MR041 (`:1004-1006`) | DB `:901-905`; Go re-evaluation; MR041 | `TestD7_P3_WrongProviderReference` | enforced; **residual: for an unbound attempt R has no platform-side fact to be compared with (25.5)** |
| 4 | correct player / tenant | `tenant_id = p_tenant` on every read plus FORCE RLS; `payment_m4_scope_visible` (`:218`, error MR060, never a verdict); tenant is the server-side target (`NewResolutionTarget`); platform actor needs an in-force grant for THAT tenant; entries must sit on the withdrawal's `wallet_id` (MR041 `:1016`) | same predicates in `payout_m4_evidence` at `-> executing`; MR041 | `TestD7_P4_WrongPlayerOrTenant` | enforced (the `tenant_id` predicates inside the function are defence in depth to RLS: mutant S27 survives, 25.9) |
| 5 | correct asset | paid: I-1 `:363`; not paid: `:448`; R-4 DB-forced asset (guard `:597`, `:697`) | DB `:897-899`; Go `m4ExecutionRefusal`; MR041 `e.asset_code = r.asset_code` | `TestD7_P5_P6_WrongAssetOrAmount` | enforced |
| 6 | correct amount | as 5 (`NUMERIC`, equality in minor units) | as 5 | `TestD7_P5_P6_WrongAssetOrAmount` | enforced |
| 7 | correct destination / instrument | **negative only:** `destination_mismatch` is outside the paid scope (`:210`, guard `:691`, MR012; Go `M4ResolvableDispute`); the per-attempt destination snapshot is write-once (0123). **No positive evidence: a statement line has no destination field.** For non-MOCK sources the Go gate (G-NONMOCK) refuses | DB `:889`; Go scope | `TestD7_P7_WrongDestination`, `TestD7_NonMockM4IsBlocked_*` | **PARTIAL (25.4 G-DEST)** |
| 8 | final provider status | paid needs one succeeded group and no declined/reversed/pending line on the merchant reference, bound reference, R or any Y in any import, MOCK and unsealed included (`:344-351`); not paid needs a declined line and no succeeded/pending/reversed line on the same set plus every matched PSP reference D (`:414-422`); closed status vocabulary (CHECK); R-1 and the post-M4 cells catch later reversals | DB `:901-905`; Go re-evaluation | `TestD7_P8_NonFinalProviderStatus` | enforced for **M4 paid** and for the platform vocabulary; **PARTIAL for M4 NOT PAID** (G-SUCCESS-PARK, 25.4: several parks are created on a provider success that is not stored durably, so a later declined line could release the hold); the provider-to-vocabulary mapping is `PROVIDER DEPENDENT` (checklist C3, C7) |
| 9 | causal relationship | attribution by the merchant reference only; not-paid needs the source's `payout_lines_carry_merchant_reference` (`:454`), no NULL-merchant payout line in a declaring import (`:426-435`), `occurred_at >= last_sent_at` (`:449`), coverage over `[created_at, last_sent_at + 24 h]` (`:455-456`); sealed imports (`:401`, `:452`) with the HMAC verified in Go at request and execution (`verifyImportSeals`); platform_acting approver floor (`:847`); four-eyes portal confirmation; **Go: the paid line lies inside `[attempt's first send, its import's coverage_end]` (25.4 G-TIME)** | DB `:901`; Go seals + re-evaluation + eligibility; R-1 after execution | `TestD7_P9_CausalLink`, the meta tests | enforced; **authenticity of the statement content is S-3/T10, `PROVIDER DEPENDENT`** |

### 25.3 What each enforcement is, and where the Go layer is the only one

- **Both layers.** Properties 1, 2, 3 (except G-REF), 4, 5, 6, 8 and 9 (except G-TIME and the seal) are enforced in the database at the request (`payment_manual_resolutions_guard`, INSERT) **and** again at `pending -> executing` (the same guard, `:881-905`), with Go as the first check at execution (`m4ExecutionRefusal`, `m4EvidenceRefusal`), and MR041 as the commit-time check of what was posted.
- **Go only, by necessity.** The import seal HMAC (the key never enters the database). The database sees only that a seal is present.
- **Go only, found by this work (25.4).** Three checks: non-MOCK evidence, "R is not a platform-issued merchant reference", "the paid line lies inside the attempt's window". They run at the request (`requestInTx`) and at the execution (`m4EvidenceRefusal`), after the seals and before anything can post; a static pin (`TestD7_EligibilityRefusalRunsOnEveryM4Path_AfterTheSeals`) fixes the call order. The database's `-> executing` re-check does not carry them (a SQL gap, reported).

### 25.4 Gaps found (each with the test that exhibits it)

| Id | Gap | Exhibited by | Disposition |
|---|---|---|---|
| **G-REF** | `payout_m4_evidence` returned `paid` for a succeeded line whose provider reference **equals the platform's own merchant reference** (or another attempt's). One provider field (the merchant-reference echo) then supplied both the causal link (property 9) and the "provider transaction reference" (property 3) and became the ledger key `provider:R`. | `TestD7_P3_WrongProviderReference/R_equals_the_attempt's_OWN_merchant_reference...` and `.../R_equals_ANOTHER_attempt's_merchant_reference...` assert the **database** still reads `paid` (a pin of the SQL gap that flips when the SQL gains the clause) and that the request is now refused in Go with `m4EligPlatformRef` | **Fixed in Go** (refusal-only, no migration). Smallest SQL fix: in the attribution block (`:367`) add `OR EXISTS (SELECT 1 FROM payment_attempts a WHERE a.tenant_id = p_tenant AND a.merchant_reference = v_g.ref)` |
| **G-TIME** | The paid branch reads **no timestamp**: a succeeded line dated before the attempt existed, or after the coverage its own import vouches for, read `paid`. A payout that predates the attempt cannot be its payout. (The not-paid branch already requires `occurred_at >= last_sent_at`.) | `TestD7_P9_CausalLink/..._BEFORE_the_attempt_existed...`, `.../..._AFTER_the_coverage...` (database `paid`, Go refusal), first-send boundary (one microsecond before is refused, the instant itself executes) | **Fixed in Go** (`occurred_at` in `[first_submitted_at (falling back to last_sent_at), import.coverage_end]`; zero tolerance, fail closed: park and investigate; an attempt with no recorded send has no eligible line). A line dated before `last_sent_at` but after the first send is **not** refused, because an earlier send of a resent attempt may be the one that paid (security LOW-2). The lower bound was `created_at` in the first revision. A per-source skew tolerance, applied to the lower bound only, is a checklist declaration (C11, bounded, default and MOCK zero, ledger-finance reviewed per source). Smallest SQL fix, carried by `gov-r32-integrity` (0127) with the same bound: add the two comparisons to the `v_line` selection (`:393-407`) |
| **G-SUCCESS-PARK** (ledger-finance C-1; **not introduced by this work, not fixed by it**) | M4 **not paid** relies on a declined statement line and on the absence of a succeeded/pending/reversed one. But several parks are created **on a provider SUCCESS that the platform does not store durably**: `provider_reference_conflict` (`payout_refbind.go` ~108-150), `destination_mismatch` and `destination_integrity_failure` (`payout_destination.go`). The platform then holds no record that the provider ever reported success, so a later declined line can satisfy the not-paid verdict and release a hold on money the provider may have paid. Property 8 is therefore only PARTIAL for not paid. | Documenting subtest `TestD7_P9_CausalLink/DOCUMENTING...success-triggered_parks` (logs the exposed reasons; deliberately asserts nothing r32 would change) | **Launch condition**, carried by `gov-r32-integrity` (migration 0127): a durable success-reported record in every park writer and path, and the not-paid verdict `contradictory` for ALL M4 not-paid reasons. Recorded in 25.7 |
| **G-NONMOCK** | Nothing technical stopped non-MOCK M4 other than the process registry and the startup gate: a registered real source with sealed imports would have been accepted by the database and by Go. | `TestD7_NonMockM4IsBlocked_PositiveDatabaseVerdictOnARealImportIsRefused`, `TestD7_EligibilityRefusal_ReasonsAtTheExecutionPoint` | **Fixed in Go**: every M4 whose evidence set contains a non-MOCK import is refused (`m4EligNonMock`, unconditionally; lifting it is a reviewed code change, 25.7) |
| **G-DEST** | The statement-line model (`statement.PaymentStatementLine`) has **no destination or instrument field**, so no statement line can positively evidence property 7. M4 paid on a bound payout is today justified by the platform's write-once destination snapshot and the absence of a `destination_mismatch` park, never by provider evidence about the destination. Acceptable for MOCK only. | `TestD7_P7_WrongDestination/every_bound_payout_carries..._(the_documented_gap)`; `TestD7_StatementLineHasNoDestinationField_PinsTheConstant` | **Structural, `PROVIDER DEPENDENT`.** Closed for non-MOCK by G-NONMOCK. A positive clause needs: a destination-echo field on the line, a migration, and a comparison in `payout_m4_evidence` against the snapshot fingerprint (the same decision-5 echo semantics). It is **not** a refusal-direction Go change and is not attempted. The `0127` branch changes `payment_m4_in_scope` / `payout_m4_evidence` for `destination_integrity_failure`; this work did not touch them |

Observations that are **not** gaps this change closes (recorded so the next reader does not rediscover them): (a) `payment_statement_lines.provider_id` is not cross-checked against the import's `provider_id` by the database (the FK is `(import_id, tenant_id)`); it is held by `validatePaymentLine` in the fetch path (INV-IO-14), by the evidence function filtering on the line's own `provider_id`, and by the seal digest that binds each line's provider id (L-1); (b) `evidence_ref_hash` is an opaque 64-hex attestation of the operator's portal check; nothing compares it to R (25.5).

### 25.5 No single untrusted provider field is sufficient

The provider controls, per line: `provider_reference` (R), `merchant_reference`, `amount`, `asset_code`, `status`, `occurred_at`, `provider_id`, and per import: the coverage window, the `payout_lines_carry_merchant_reference` declaration and `is_mock`; the platform controls the seal. Two tests state the property from both ends:

1. **Only this field is right** (`TestD7_Meta_NoSingleProviderFieldIsSufficient_OnlyThisFieldIsRight`, 13 rows): a statement in which exactly one field (merchant reference; amount and asset; status; R; `occurred_at`; the seal; the coverage and declaration) is correct and everything else is wrong or absent never yields a positive verdict for either kind, and both kinds are refused at the request. Two "everything right except one" rows (unsealed import; undeclared source) show the seal and the declaration are each necessary.
2. **One field corrupted from a perfect baseline** (`TestD7_Meta_NoSingleProviderFieldIsSufficient_OneFieldCorrupted`, 16 rows plus the `provider_id` case): R to a reserved value (the importer refuses to store it), R to the merchant reference (database `paid`, Go refuses), merchant reference emptied/foreign/another park's, amount, asset, status (pending/reversed/declined), `occurred_at` (before the attempt, after coverage: Go refuses), seal absent, seal made with a key the process does not hold (database `paid`, Go refuses), a non-MOCK source (Go refuses), `provider_id` of another provider (the fetch path refuses the whole statement). **One row is positive by design:** R replaced by another fresh PSP-looking reference. R is the **output** of the verdict, not a checked input: for an unbound attempt the platform has issued no PSP reference, so there is nothing to compare it with, and the verdict pins whatever single, exclusive R the sealed line carries.

**The honest statement of the residual.** Sufficiency here is a conjunction of corroborations that the platform can check independently: merchant reference against `payment_attempts.merchant_reference` (platform-issued, unique per tenant), amount and asset against the attempt, status against the closed vocabulary and the absence of contradicting lines, `occurred_at` against the attempt's window, R against the exclusivity set, the seal against the platform's key, and a **human** four-eyes confirmation against the provider's portal. A source that controls **every** field of a line and the channel it travels on (it knows the merchant reference, amount and asset: they were sent to it) could still fabricate a paid line. Software on this side cannot close that; its defences are the authenticated channel and the PSP content signature (S-3), the seal (T10), and the operator's portal confirmation. All three are launch conditions for any non-MOCK M4 (25.7). Whether and how the portal confirmation is machine-bound to R is an open owner/`security` question with two proposals, recorded in 25.7.

### 25.6 Adapter contract for the provider-specific part (`internal/payments/m4_source_contract.go`)

`PROVIDER DEPENDENT`, **gated, no implementation.** `M4SourceContract` is what a future non-MOCK statement source must declare; `CheckM4SourceContract` is the pure checklist a real source's own tests must call and pass with **no `Fail` finding**. Absence is never "false": every declaration is explicit (decision 5: an adapter must not pretend a capability it does not have).

| Item | The source must declare | Fails when | Protects D-7 |
|---|---|---|---|
| C1 identity | provider id, label, `Synthetic() == false` | missing, or a synthetic/MOCK source | 3, 4, 9 |
| C2 merchant-reference declaration (S-3) | `payout_lines_carry_merchant_reference` tri-state | undeclared, or `false` (no line can be attributed) | 2, 9 |
| C3 status vocabulary | every provider status -> event (`paid_out`, `pending`, `rejected`, `returned`, `reversed`, `chargeback`) -> platform class | an event unmapped (a return, reversal or chargeback of a payout must exist), an event mapped to the wrong class, a duplicate or unknown entry | 8 |
| C4 unknown status | `reject_import` or `treat_as_pending` | anything else, notably "treat as succeeded" | 8 |
| C5 channel (S-3) | authenticated to the PSP; endpoint and credentials from governed configuration; PSP content signature verified when the PSP offers one | any of the three missing | 3, 4, 9 |
| C6 destination echo (decision 5) | `none`, `fingerprint` or `full_instrument`, and whether it is **on the statement line** | undeclared or unknown is `Fail`; `none` or "not on statement lines" is a `Limit`: M4 paid not admissible | 7 |
| C7 final status | terminal-paid and terminal-declined statuses (mapped to succeeded / declined), whether a paid payout can be reversed, the reversal window, and that a late reversal appears on the statement | a non-terminal success, overlapping sets, a reversible payout with no window or not reported on the statement | 8 |
| C8 amount and asset | integer minor units, exponent from the Asset registry, asset code on every line, amount is the payout principal (not net of fees or FX) | any false | 5, 6 |
| C9 provider reference | PSP-issued, stable per payout, **never an echo of the merchant reference** | otherwise | 2, 3 |
| C10 completeness (T10 residual) | coverage window vouched for by the PSP; pagination completeness proven | otherwise | 9 |
| C11 time semantics (G-TIME) | `occurred_at` is the provider event time; the lower bound is the first send; an optional lower-bound-only clock-skew tolerance in `[0, 5 min]` (default and MOCK: 0; ledger-finance reviews each non-zero value per source) | otherwise | 9 |

**A passing checklist is necessary but not sufficient.** C5 and C10 (and C2, C9, C11) are self-declared by the adapter; only the vendor-specific conformance tests, the `security` S-3 review of the real channel and the T10 seal review establish them. C1 does not trust the `Synthetic()` declaration alone: a contract object carrying the `providerkind.Synthetic` marker (`payoutinstrument.IsSyntheticComponent`) is refused as well (security LOW-1, `TestD7_SourceContract_SyntheticMarkerDisqualifiesDespiteTheDeclaration`).

`TestD7_SourceContract_EachItemIsEffective` removes one declaration at a time from a fully declared **test fixture** (not a provider) and shows exactly that item's finding appears with the stated severity and disqualifies the source. Even a fully conformant declaration reports `PaidAdmissible == false` while `m4StatementLineCarriesDestinationEcho` is false; `TestD7_StatementLineHasNoDestinationField_PinsTheConstant` ties that constant to the line struct so it flips only together with the SQL clause. `TestD7_NoProductionTypeImplementsTheSourceContract` shows the MOCK source is not an `M4SourceContract`.

### 25.7 Non-MOCK M4 stays `BLOCKED`

No part of this change lifts any launch condition. Non-MOCK M4, paid or not paid, remains `BLOCKED` until **all** of the following hold, and the Go gate `m4EligNonMock` refuses regardless until a reviewed change removes it:

1. a real source exists and passes the 25.6 checklist (C1-C10) with no `Fail`, with a conformance test per adapter against its real wire format (`PROVIDER DEPENDENT`);
2. S-3 is met for that source: authenticated channel, PSP content signature, endpoint and credentials from governed configuration (`PROVIDER DEPENDENT`, §10.3);
3. the T10 launch flag is cleared: the import seal passes `security` implementation review with mutation evidence (§10.3), plus the T10 residuals (T6 in-process key, authenticity/completeness, availability);
4. for **M4 paid**, a positive destination clause (G-DEST): a destination-echo field on the statement line, a migration, and the comparison in `payout_m4_evidence`; until then `PaidAdmissible` is false by construction;
5. LF C-3 (§20.4): the real provider's return/reversal/chargeback statuses reach `contradiction_after_m4_paid` or a dedicated reason, with a conformance test per adapter, and the governed WITHDRAWAL-REVERSAL-1 path exists; HD-R15-1 and HD-R15-5 (§10.3);
6. **G-SUCCESS-PARK is fixed** (ledger-finance C-1, `gov-r32-integrity`, migration 0127): a durable success-reported record in every park writer and path, and the not-paid verdict `contradictory` for ALL M4 not-paid reasons. This work does not introduce the exposure and does not fix it;
7. **the SQL counterparts of G-REF, G-TIME and G-NONMOCK exist** (ledger-finance C-2): today they are enforced in Go only and are **not** in the database recount at `pending -> executing`. G-REF and G-TIME are being folded into 0127 (same first-send bound); G-NONMOCK's SQL form is decided with the real-source work. Until then a session able to drive `executing` directly bypasses them (T5).

**Proposals for the owner and `security` (NOT decided, NOT implemented; launch-condition proposals).**

- *Binding the human confirmation to R (`evidence_ref_hash`).* `ledger-finance` proposes requiring `evidence_ref_hash` to equal a platform-computed digest over `(provider_id, R, amount, asset, merchant_reference)`, shown to the approvers. `security` prefers **blind entry**: the requester and each approver independently type R (no prefill), the server compares each entry to the verdict's R, and `evidence_ref_hash` is the hash of the portal confirmation artefact; for **not paid**, the same blind entry of the portal's declined status and reference. The two views differ on whether the platform shows the value or the humans re-derive it from the portal; the owner and `security` choose.
- *Proposed design for the positive destination clause (G-DEST), for a future migration.* The statement line gains a `destination_echo` (key id plus fingerprint) covered by `lines_digest`; every line of the single succeeded group must carry a well-formed echo equal to the attempt snapshot's; any differing, malformed or unexpected echo makes the verdict `contradictory`, a missing echo makes it `insufficient`; a legacy unbound attempt is never paid-admissible; the `-> executing` re-check pins the echo verdict.

### 25.8 Evidence

- **Conformance suite** (`internal/payments/m4_d7_conformance_integration_test.go`, real PostgreSQL, runtime-shaped non-superuser role, `newM4World`/`newM4WorldOn` fixtures, platform `acting` requester/approver as the existing M4 world): `TestD7_P1_...` to `TestD7_P9_...`, `TestD7_Meta_...` (2), `TestD7_Ambiguity_ParksWithoutAutomaticPayOrResolve`, `TestD7_NonMockM4IsBlocked_...`, `TestD7_EligibilityRefusal_ReasonsAtTheExecutionPoint`. Each property test proves a wrong value in that property alone leaves the verdict `insufficient`/`contradictory` (never `paid`/`not_paid`), refuses both kinds at the request (`MR062`, token `force_resolve_evidence_insufficient`) with the withdrawal still `submitted`, no resolution row, no posting and the ledger invariants intact, and refuses at execution (`refused_at_execution` / `MR061` / `MR010`) when the change lands after the request. Ambiguity (two plausible attempts, duplicate lines of different timestamps, one reference naming two merchants, succeeded and declined together, partial coverage before and after) parks: the sweeper and a reconciliation run change nothing, and the reconciliation run raises the existing `pay_captured_unposted` finding.
- **Unit and static** (no database): `m4_source_contract_test.go`, `m4_d7_static_test.go`.
- The existing M4 suites were re-run unchanged except for one fixture (`TestM4Recon_Paid_LineOnRNamingAnotherMerchant_Raises` dated its paid line a minute before the attempt existed; G-TIME refuses that, so the fixture is now dated now: a deliberate pin flip) and a provider override on the import helper.
- **Mutation evidence:** `docs/plans/prh2-hardening-round/prh2-r34-d7-mutation-kill.txt` (66 counted mutants over the migration, the Go verification layer and the checklist: 55 killed, 11 survivors, every one classified: S13 equivalent, S27 equivalent, S34/S35 unreachable, S32 unreachable, G10/G12 layered and killed with their database twin, S21-S24 redundant pairs killed as the doubles D01-D03. Review of run 1's survivors added the same-import pending/reversed, NULL-merchant-in-a-declaring-import and window-opens-after-creation tests).
- Local evidence only: no real provider, no AWS, no non-MOCK source.

### 25.9 Residuals

- G-DEST (structural), the G-REF/G-TIME/G-NONMOCK SQL equivalents, and the `evidence_ref_hash` binding question (25.5).
- Defence in depth that no test can separate from its sibling is reported as a survivor rather than hidden: the explicit `tenant_id` predicates inside `payout_m4_evidence` (row-level security enforces the same boundary, S27), and the request-time and `-> executing` R-4 comparisons of the withdrawal's amount and asset against the attempt's (S34/S35; `withdrawal_requests` amount and asset are immutable after insert, migration 0026, so no reachable state separates them), the `-> executing` scope and reference-pin re-check (S32; after a request the attempt cannot leave the unbound scope or gain a reference, and Go is the first check), the seal-presence conjunct of the not-paid selection (S13; an unsealed import is written without the S-3 declaration, so condition (iv) already refuses it) and four paid-attribution clauses that mask one another (S21-S24: they are killed in pairs, D01-D03). The Go withdrawal-state and re-evaluation checks at execution (G10, G12) are killed only together with their database twin, by design (Go is the first check, the database the second).
- T5 (ADR 0110): a session able to run arbitrary SQL as the acting role could drive `pending -> executing` directly and skip the Go-only checks of 25.3. The same actor can already plant ordinary non-governed postings (§17.5); MR041 at commit still refuses an executed M4 whose link does not carry the pinned keys and amounts.
- The fixture contract in `m4_source_contract_test.go` models no vendor; a real source's checklist test is `PROVIDER DEPENDENT`.
