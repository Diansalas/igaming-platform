# ADR 0111 — Payout destination binding (B13), governed resolution of unbound payouts (PAY-PAYOUT-UNBOUND-RESOLVE-1) and four-eyes release of frozen `approved` holds (HSEC-APPROVED-HOLD-RELEASE-1)

- **Status: PROPOSED — DESIGN ONLY (`architect`, 2026-10-08, baseline `dd6cd31`).** No code, no migration, no
  registry change is made by this ADR. Every deliverable it describes is `NOT IMPLEMENTED`. Implementation of each
  part starts only after the review gates in §8 have accepted that part.
- **Authority.** The three owner decisions recorded verbatim in ADR 0095 §44 (decisions 1-8 B13, 9-12 RESOLVE-1,
  13-18 HSEC; 2026-10-08) are **authoritative** and are not reinterpreted into broader permissions. Where they leave a
  sub-question open, this ADR picks the safest interpretation consistent with existing governance and records it as a
  numbered **AMBIGUITY** (§9). Questions that need a human policy answer are in §10; the design proceeds around them
  (fail closed).
- **Inputs.** ADR 0095 §35.2, §35.6, §42, §43, §44; ADR 0101 (K3, R-K3-8, §6, §24); ADR 0107 (CT-PRE); ADR 0110
  (signed actor proof, 0120); ADR 0007/0008; ADR 0096/0098; `docs/governance/b13-decision-brief.md` (+ supplement),
  `hsec-approved-hold-release-1-decision-brief.md`; code at `dd6cd31` (`internal/payments/payout.go`,
  `payout_sweep.go`, `receipt.go`, `manual_resolution.go`, `payout_refbind.go`, `types.go`; `internal/withdrawal`;
  `internal/adjustment`; `internal/identity`; migrations 0100-0121).
- **Amends (when accepted):** ADR 0095 (§4 payout cells, §9.3 S95-C10 clarification), ADR 0101 (new kinds M4, §5.1
  allow-list), ADR 0107 (CT-PRE table/capability are subsumed by §6 here), ADR 0110 (§3 table list 9 → 11; K3 digest).
- **Owners.** `architect` (this ADR). Implementers: `payments` (all three), `identity-compliance` (B13 verification
  sources). Binding rulings: `ledger-finance` (financial invariants), `security` (security requirements).
- **Migration reservations.** 0122 = PAY-RECEIPT-ANOMALY-APPLIED-1 (another agent, not designed here); **0123** = B13;
  **0124** = UNBOUND-RESOLVE-1 (+ the B13 exceptional resolution); **0125** = HSEC hold release. See §7.2.

---

## 1. Shared principles (apply to all three parts)

1. **Reuse, not parallel systems.** Identity = `persons` / `player_accounts` / `staff_users.person_id`. Four-eyes =
   the ADR 0099/0100/0101 governed-resolution machinery (capability-specific in-force grants, `financial_policy_
   required_approvals`, LF-11 distinct non-NULL Persons, S-12 beneficiary exclusion, S-2(iii) policy-author
   exclusion, DB recount at `pending → executing`, execution in the final approval's transaction, acting fences).
   Proof of actor = ADR 0110 `zz_actor_proof_guard` + `internal/actorproof.Issuer`. Audit = `audit_log` (+ ADR 0104
   projection for platform actions). Alerts = B12 `raisePayoutDisputeAlert` on Kind `payment.webhook_integrity`.
2. **`withdrawal_approvals` is NOT reused for any new four-eyes step.** Reasons: `UNIQUE (withdrawal_request_id,
   approver_principal_id)` collides with the original approvers; it is threshold-based, not mandatory four-eyes; it
   has tenant-family RLS only (no acting family); it is not proof-bound (ADR 0110 §10a T8).
3. **No automatic money movement.** Nothing in this ADR moves money except an executed four-eyes resolution in the
   same transaction as its final approval, through `ledger.Post` (no balance UPDATE, deterministic idempotency keys).
4. **Fail closed.** Every missing, ambiguous, expired or unverifiable input refuses, leaves the funds where they are
   (hold kept), writes an audit row, and where it is a payment-integrity signal raises the B12 durable P1.
5. **Labels.** MOCK/synthetic behaviour is labelled `MOCK`; real-provider behaviour `PROVIDER DEPENDENT`.

---

## 2. B13 — provider-neutral payout instrument and destination binding

### 2.1 Entity model (migration 0123)

**`payout_instrument_kinds`** (reference, family R, seeded by migration, `SELECT USING (true)`, no write policy):
`code TEXT PK CHECK (code ~ '^[a-z][a-z0-9_]{1,31}$')`, `detail_schema_version INT NOT NULL`,
`allowed_rails TEXT[] NOT NULL` (the `payment_method` values a rail may route with), `non_synthetic_enabled BOOLEAN
NOT NULL`. Seed: `bank_account` (true), `card_token` (true), `ewallet_account` (true), `crypto_address` (**false**,
ADR 0008 / D3 open, §10 HD-R15-4), `synthetic_test` (false). The set is **open**: a new regulated instrument is a new
row plus a Go validator, never a code path keyed on one kind (owner decision 6).

**`payout_instruments`** (tenant-owned, ENABLE + FORCE RLS):

| Column | Rule |
|---|---|
| `id` UUID PK | DB default; client value refused |
| `tenant_id`, `brand_id`, `player_account_id` | composite FKs `(player_account_id, tenant_id)` and `(brand_id, tenant_id)`; brand must equal the player account's brand (trigger) |
| `person_id` | **DB-forced** from `player_accounts.person_id` (never supplied) |
| `kind` | FK `payout_instrument_kinds` |
| `rail` | must be in `kind.allowed_rails` (trigger); this replaces the staff-supplied `payment_method` (§2.4) |
| `asset_codes` | `TEXT[]` the instrument may receive; each in the `Asset` registry (trigger) |
| `detail` | `JSONB NOT NULL`, `jsonb_typeof = 'object'`, `octet_length(detail::text) <= 4096`; validated by the per-kind Go validator (`detail_schema_version`). **Never a PAN** (`card_token` holds only a PSP token reference + last4 + network; AMBIGUITY A-7) |
| `display_mask` | `TEXT NOT NULL`, ≤ 64 bytes; the only detail-derived value any staff API returns |
| `fingerprint`, `fingerprint_kid` | `CHECK (fingerprint ~ '^[0-9a-f]{64}$')`; computed in Go (§2.2); stable and non-reversible |
| `supersedes_instrument_id` | NULL or a same-player instrument (versioning, §2.3) |
| `state` | `pending_verification`, `verified`, `verification_expired`, `suspended`, `revoked`, `rejected`, `superseded` |
| `current_verification_id` | FK to the row in force when `state = 'verified'` |
| `created_at`, `state_changed_at` | |

UNIQUE `(tenant_id, player_account_id, fingerprint) WHERE state IN ('pending_verification','verified','verification_expired','suspended')`.

**`payout_instrument_fingerprint_owners`** `(tenant_id, fingerprint) PK, person_id NOT NULL, created_at`: append-only.
The first Person to register a fingerprint in a tenant owns it; a different Person's registration conflicts on the
PK (race-free) and is refused with `PAYOUT_INSTRUMENT_CONFLICT` + audit (fraud signal). Never deleted (A-2).

**`payout_instrument_verifications`** (append-only; UPDATE/DELETE/TRUNCATE denied): `id`, `tenant_id`,
`instrument_id`, `source` (closed set, §2.5), `ownership_assertion` (closed set, §2.5), `verifier_provider_id`,
`verifier_reference_hash` (`^[0-9a-f]{64}$`, a hash of the vendor's reference, never vendor text),
`outcome` (`verified` | `rejected`), `verified_at`, `expires_at TIMESTAMPTZ NOT NULL`, `seal`, `seal_kid` (§2.2),
`created_txid`.

**`withdrawal_requests`** gains `payout_instrument_id UUID NULL` (composite FK, same tenant) and
`payout_instrument_fingerprint TEXT NULL`. Both are added to `withdrawal_requests_enforce_immutable_fields()`. A new
BEFORE INSERT trigger requires both NOT NULL and requires the instrument to belong to `NEW.player_account_id`, be
`verified`, and list `NEW.asset_code`. Pre-0123 rows stay NULL (**legacy**, MOCK/dev only, A-11).

**`payout_attempt_destination_snapshots`** (the binding snapshot; 1:1 with a payout attempt; immutable):
`attempt_id` PK + composite FK `(attempt_id, tenant_id)` → `payment_attempts_id_tenant_key`; `tenant_id`,
`withdrawal_request_id`, `instrument_id`, `kind`, `rail`, `fingerprint`, `fingerprint_kid`, `verification_id`,
`verification_source`, `ownership_assertion`, `verified_at`, `verification_expires_at`, `display_mask`,
`amount`, `asset_code`, `seal`, `seal_kid`, `created_txid`. UPDATE, DELETE, TRUNCATE denied in all sessions
(`ledger_deny_mutation`). A DEFERRABLE INITIALLY DEFERRED constraint trigger on `payment_attempts` INSERT
(`operation = 'payout'`) requires a snapshot with `created_txid = txid_current()` whenever the withdrawal's
`payout_instrument_id` is NOT NULL, and requires `snapshot.instrument_id/fingerprint` = the withdrawal's.
A separate table (not columns on `payment_attempts`) is chosen so that **`payment_attempts_guard()` is not edited**
(the ADR 0101 §8.3 exact-diff discipline is preserved) and so the snapshot is structurally write-once.

**RLS and GRANTs (`igaming_runtime`).** All B13 tables: FORCE RLS, no `FOR ALL`, no SECURITY DEFINER, DELETE and
TRUNCATE denied. Tenant family (`tenant_id = app.tenant_id`, player GUC NULL): SELECT, INSERT on instruments /
verifications / fingerprint owners / snapshots; UPDATE on `payout_instruments` only, column discipline by trigger
(only `state`, `state_changed_at`, `current_verification_id` may change; every other column `IS NOT DISTINCT FROM`
OLD). Player family: **SELECT own instruments only** (`player_account_id = app.player_account_id`); no player
INSERT/UPDATE — registration runs in a tenant-scope transaction with the player resolved server-side from the
authenticated session (the `RequestWithdrawal` pattern). Acting family: SELECT only (needed by §4/§6 queues).
Grants: `SELECT, INSERT, UPDATE` on `payout_instruments`; `SELECT, INSERT` on the other three; nothing else
(`deploy/init-app-role.sql` append, K2 loop pattern).

**Instrument state machine** (trigger-enforced; every transition audited `payout_instrument.<transition>`):
`pending_verification → verified | rejected` (verifier result only); `verified → verification_expired` (sweep or
lazily on read at a gate; `now() >= verification.expires_at`); `verification_expired → verified` (a **new**
verification row of the same instrument, same fingerprint); `verified | verification_expired → suspended`
(compliance staff, single actor — a blocking direction only); `suspended → verified` only by a **new provider
verification** (no staff "unsuspend"); any non-terminal → `revoked` (player, provider or compliance); `→ superseded`
when a replacing instrument version is verified. `revoked`, `rejected`, `superseded` are terminal. **There is no
transition by which staff mark an instrument verified or edit its destination** (owner decision 7).

### 2.2 Fingerprint and integrity seal (arbitrary-SQL threat model)

THREAT-MODEL-ARBITRARY-SQL-1 = YES (ADR 0110). A stolen runtime credential could otherwise insert a "verified"
instrument for an attacker account and redirect a payout. ADR 0110 proofs bind *staff* actors and cannot cover
system/vendor writes, so B13 adds a **Go-side seal** using the ADR 0110 key-handling pattern (not a parallel
identity or four-eyes system):

- Keys `PAYOUT_INSTRUMENT_KEYS` (`kid:base64`, ≥ 32 bytes) and `PAYOUT_INSTRUMENT_ACTIVE_KID` from the secret store;
  `config.SecretValue`; refused if equal to any JWT or actor-proof key; **never stored in the database**. The
  production startup gate refuses to start without them. Dev/CI generate a random key per process (prooftest
  pattern, `integration` build tag).
- `fingerprint = HMAC(k, "b13-fp-v1|" || tenant_id || "|" || kind || "|" || normalize_kind(detail))`. Including
  `tenant_id` makes fingerprints non-comparable across tenants (isolation). The per-kind normaliser (IBAN
  upper-case/no spaces, address checksum casing per network, ...) lives with the kind validator.
- Verification seal: `HMAC(k, "b13-ver-v1|" || verification_id || instrument_id || fingerprint || source ||
  ownership_assertion || outcome || expires_at)`, written by the verifier path.
- Snapshot seal: `HMAC(k, "b13-snap-v1|" || attempt_id || instrument_id || fingerprint || kind || rail ||
  verification_id || amount || asset_code)`, written at T1p.
- **Verification in Go** at every gate (§2.4) and immediately before every adapter call: the seal is recomputed,
  the fingerprint is recomputed from the `detail` that will be sent, and both must match. A mismatch is
  `destination_integrity_failure` (§2.6). An SQL-only attacker cannot produce a valid seal. Residual: application
  host compromise (ADR 0110 T6) is unchanged; record as ADR 0110 §10a **T9** (instrument writes are not
  actor-proof bound; mitigated by the seal).

### 2.3 Immutability and versioning (reproducible evidence)

An instrument row's identity columns are immutable; a destination change is a **new instrument row**
(`supersedes_instrument_id`) that must itself be verified; the old row becomes `superseded` only when the new one is
verified. Verifications are append-only. The withdrawal's binding columns are immutable after insert, and the
attempt snapshot is write-once. Historical payout evidence is therefore reproducible from (snapshot → instrument row
→ verification row), all immutable.

### 2.4 Binding lifecycle (when)

| Point | Rule |
|---|---|
| **Request creation** (`POST /v1/me/withdrawals`, `withdrawal.RequestWithdrawal`) | **Binding happens here** (A-1). The player selects `payout_instrument_id` (their own); the server validates in the same transaction as the hold, after the H-SEC-11 gate: same tenant/brand/player account, `state = 'verified'`, verification in force (`expires_at > now()`), seal valid, asset listed, kind/rail enabled. Missing → 409 `PAYOUT_INSTRUMENT_REQUIRED`; unusable → 409 `PAYOUT_INSTRUMENT_NOT_USABLE`. No row, no hold, idempotency key not consumed (the §43.1 refusal contract). A retried request returns the original (replay lookup precedes the check, ADR 0096 §8 item 3). |
| **Approval** | Unchanged mechanism; the approver API shows `display_mask`, `kind`, `verification_source`, `verified_at`. The approvers therefore four-eyes a **fixed** destination. |
| **T1p** (`ClaimForDispatch`) | Order: L1 `LockApprovedForSubmission` → H-SEC-11 gate → KYC gate (deny/unavailable branches unchanged) → route error → **destination gate** → `kyc.RecordDecision(allow)` → `MarkSubmittedPending` → `InsertSubmittingAttempt` → **snapshot insert** → audit. The destination gate locks the instrument `FOR SHARE` (serialises with revocation, which locks it `FOR UPDATE` and takes no withdrawal lock: no cycle; LF to record the ADR 0082 position "after L1 withdrawal and the tenant/brand gate, before the attempt"). It refuses when: legacy NULL binding and the routed adapter is not `providerkind.Synthetic`; instrument not `verified` or expired; seal or fingerprint mismatch; `verification_source = 'synthetic'` and the routed adapter is not Synthetic; kind `non_synthetic_enabled = false` and the adapter is not Synthetic; rail not supported by the routed capability. A refusal commits **only** the denial audit (`withdrawal.submit.http`, outcome `denied`, metadata `denied_by_destination: <closed reason>`), no allow decision row (DECISION-ROWS-1 is preserved: no claim, no allow row), request stays `approved`, 409 `PAYOUT_DESTINATION_NOT_USABLE`. |
| **Submit body** | `payment_method` is no longer an input: the rail is the instrument's `rail`. A body value that differs is refused (400 `PAYMENT_METHOD_MISMATCH`); an equal or absent value proceeds (A-10). |
| **Phase B** (`payoutAdapterCall`) | `WithdrawRequest` is built **only** from the snapshot + the immutable instrument row; seal and fingerprint re-verified first. Failure ⇒ no provider call, returned as the existing NotSent class (T5 → `created`), and the next T2 gate escalates (§2.6). |
| **T2 re-claim / T12 resend** (`payout_sweep.go`) | A new `destinationGateAndEscalate` runs next to `gateAndEscalateOnDeny` (same position: after the resolution-only and kill-switch checks; the same LF95-C10(b)/(c) precedent). It re-checks the **snapshot's** instrument (state, verification in force, seals). The destination is **never re-resolved**: a resend carries the identical snapshot. Not usable ⇒ T16 escalation (new closed escalation reasons `destination_not_usable`, `destination_integrity_failure`), no resend, no hold release, B12 P1 raised last; an already-escalated attempt is an idempotent reschedule (L3/B8 pattern). |
| **Polls / evidence on an existing attempt** | Unaffected by instrument state (resolving an in-flight payout is never blocked; LF95-C10(d) precedent). |
| **Player revocation** | Refused (409 `PAYOUT_INSTRUMENT_IN_USE`) while a withdrawal in `requested`, `pending_review`, `approved` or `submitted` binds it (A-9). Provider/compliance revocation or suspension is never refused; its effect is the gates above (parks, never releases). |

### 2.5 Verification sources, tiering (MOCK / sandbox / real)

- `source` closed set: `psp_account_verification`, `kyc_vendor_instrument_verification`,
  `custodian_address_verification`, `synthetic`. `ownership_assertion` closed set (v1):
  `account_holder_matches_verified_identity` (non-synthetic sources) and `synthetic_asserted` (only with `synthetic`).
  A CHECK pins `(source = 'synthetic') = (ownership_assertion = 'synthetic_asserted')`.
- Verification is produced only through a provider-neutral **`PayoutInstrumentVerifier`** interface (new package
  `internal/payoutinstrument`), whose adapters map vendor results into the closed sets inside the adapter. The MOCK
  verifier implements `providerkind.Synthetic`, writes only `source = 'synthetic'`, and is refused in production by
  `RefuseSyntheticInProduction` (it is registered in the startup registrations).
- **Tiering rule (Go, at every gate, keyed on the live adapter object's type marker, which SQL cannot forge):**
  a non-Synthetic payment adapter (sandbox or real) requires a non-NULL binding whose verification source is
  non-synthetic, `non_synthetic_enabled` kind, valid seals. A Synthetic adapter accepts any source and (legacy only)
  a NULL binding. **MOCK flexibility is confined to the verification source**: instruments, binding, snapshot and
  seals are mandatory in MOCK too, so no weaker binding model exists to leak into production (owner decision 8).
- `expires_at` = min(source-supplied expiry, `payout_instrument_verification_max_age` for the tenant's
  jurisdiction). **No config row ⇒ non-synthetic verifications cannot be recorded** (fail closed, A-6, HD-R15-1).
  Synthetic: a code default for dev only.

### 2.6 Callbacks and destination echo (owner decisions 3 and 5)

- **Callbacks, polls and statements can never set, replace or change a destination.** Nothing in `receipt.go`,
  `payout.go` (phase C, poll) or reconciliation writes `payout_instruments`, verifications, the withdrawal binding
  or snapshots; snapshots are write-once and UPDATE-denied in all sessions. A static test pins that the only
  snapshot writer is the T1p function and the only instrument writers are `internal/payoutinstrument`.
- **S95-C10 is retained**: no raw payer-identifying field enters `CallbackEvent`, `StatusResult`, `WithdrawResult`,
  `ReceiptEvidence` or a statement line. The only permitted destination evidence is
  `DestinationEcho{Fingerprint, Kid}` (optional), computed **inside the adapter** with an injected
  `DestinationFingerprinter` over the vendor-reported destination. A real adapter that receives destination data
  must fingerprint it and must not map it raw (adapter acceptance criterion).
- Comparison sites: phase C sync result, poll result (`applyPayoutStatusEvidenceInTx`), and any future payout
  receipt cell. Cells (all after the existing reference/amount checks, under the existing L1 withdrawal → attempt
  locks):

| Attempt state | Echo vs snapshot | Effect |
|---|---|---|
| non-terminal (`submitting`, `pending`, `ambiguous`) | equal, or absent and the manifest does not declare `EchoesDestinationFingerprint` | unchanged behaviour |
| non-terminal | absent but manifest declares `EchoesDestinationFingerprint = true`, on a success | treated as ambiguous (poll decides); never a success |
| non-terminal | **different**, or unknown `Kid` (A-8) | CAS → `disputed`, `terminal_reason = 'destination_mismatch'` (new closed reason); audit `payments.payout_destination_mismatch` (closed vocabulary: attempt, withdrawal, provider id, snapshot fingerprint prefix only); B12 P1 raised **last**; **no** `Complete`, no release, withdrawal stays `submitted`, hold kept |
| terminal (`succeeded`, `declined`) | different | no state change; audit + P1 with new signal reason `destination_mismatch_on_terminal_payout` (`payoutSignalReasons`) |
| `disputed` | any | no-op (replay idempotence) |

- **Closed-set impacts (no DB migration for reasons).** `payment_attempts.terminal_reason` has only a length CHECK
  (`0101:121`), so no terminal-reason CHECK changes. Go closed sets change: `PayoutDisputeReasons()` +
  `"destination_mismatch": false` (M2-refused); `payoutSignalReasons` + `destination_mismatch_on_terminal_payout`;
  `payoutEscalationReasons` + `destination_not_usable`, `destination_integrity_failure`; reconciliation
  `disputeReasonClasses` + `destination_mismatch` = *bound-if-referenced*. The C-5b pin, the B12 static raise-site
  pins and source-order guard counts are updated deliberately. The 0115 M2 literal allow-list is untouched
  (refuses the new reason by default).

### 2.7 `WithdrawRequest` extension

```go
type WithdrawRequest struct {
    MerchantReference string
    Amount            int64
    AssetCode         string
    PaymentMethod     string            // = snapshot.Rail (server-derived), never staff input
    Destination       PayoutDestination // REQUIRED for a non-Synthetic adapter
}
type PayoutDestination struct {
    InstrumentID       uuid.UUID
    Kind               string          // open set, payout_instrument_kinds
    Detail             json.RawMessage // kind-specific, validated; never a PAN
    Fingerprint        string
    VerificationSource string
}
// WithdrawResult and StatusResult gain: DestinationEcho *DestinationEcho  // {Fingerprint, Kid}; optional
```

`OperationManifest` gains `EchoesDestinationFingerprint bool`. The conformance suite asserts that a non-Synthetic
adapter is never called with an empty `Destination`.

### 2.8 Exceptional resolution (owner decision 7)

There is **no** staff destination override and **no** rebind kind. The only exceptional staff resolution touching a
B13 condition is the four-eyes M4 "not paid" kind for a `destination_mismatch` park (§4), which requires positive
evidence of non-payment and releases the hold to the player's own cash; it never redirects money. A misdirected
payout confirmed paid is retained (HD-R15-6). An `approved` withdrawal whose instrument becomes permanently unusable
stays parked (HD-R15-5).

### 2.9 API and permissions

Player: `POST /v1/me/payout-instruments` (kind, rail, detail; starts verification), `GET /v1/me/payout-
instruments` (masks), `POST /v1/me/payout-instruments/{id}/revoke`. Staff: `GET` instrument list/detail by player
(`payout_instrument:read` → `finance`, `compliance`, `platform_admin`; masks only; tenant from the path/session);
`POST /v1/admin/payout-instruments/{id}/suspend` (`payout_instrument:suspend` → `compliance`; reason code
required; audited). No staff create/verify/edit route exists. `backoffice/src/auth/permissions.ts` matches.

### 2.10 Tests and acceptance (B13)

Runtime role (`NOT rolsuper AND NOT rolbypassrls`), real session shapes, `SUM(D)=SUM(C)`, projection = rebuild:
(a) the submit body cannot influence destination (differing `payment_method` refused; no destination field read);
(b) cross-player, cross-brand and cross-tenant instrument refused at request; tenant-B session sees zero rows;
(c) unverified / expired / suspended / revoked / superseded instrument: request refused with no row/hold; T1p refused
with only the denial audit and no attempt row; (d) destination cannot change after approval (immutable binding
columns, snapshot UPDATE denied in tenant, acting and system sessions); (e) fingerprint conflict across Persons
refused, race-free (two concurrent registrations, exactly one wins); (f) Synthetic vs non-Synthetic adapter matrix
(synthetic source to a non-Synthetic adapter refused; legacy NULL binding refused for non-Synthetic); (g) seal
tamper by SQL (instrument detail, verification row, snapshot) ⇒ no provider call, T16 escalation, P1; (h) T2 and T12
re-check (revoked after T1p ⇒ escalation, no release, no resend; already escalated ⇒ reschedule only); (i) echo
cells: every row of §2.6, replay, concurrent deliveries, tenant isolation, alert discriminator with `provider_id`
only and no fingerprint/reference leak; (j) callbacks never write B13 tables (static + integration);
(k) instrument state machine (every illegal transition refused, staff cannot verify); (l) audit rows for every
mutation; (m) migration up/down/up, whole-schema snapshot, grant pin; mutation evidence file.

---

## 3. (B13 cross-reference) What B13 does not change

No change to the M2 allow-list, to `withdrawal.Complete/Fail`, to reconciliation clearing (`payoutCompletedRef` /
`clearedRefFor` stay positive-attribution), to deposits, or to the kill-switch/H-SEC gates. Return-to-source (D2) is
not enforced (HD-R15-3); `payout_instruments` can later carry an origin link without a rewrite.

---

## 4. PAY-PAYOUT-UNBOUND-RESOLVE-1 — evidence-backed four-eyes resolution (migration 0124)

### 4.1 Scope

Owner decisions 9-12: no automatic resolution; stays parked until positively attributable; manual resolution only
through four-eyes with complete audit; never release/settle/deliver without positive evidence. **Scope (A-12):** a
payout attempt in `disputed` whose `terminal_reason` is `invalid_provider_reference`, `invalid_provider_reference:*`,
or `provider_reference_conflict` **with `provider_reference IS NULL`** (the unbound class of ADR 0095 §35.2/§35.6),
plus `destination_mismatch` for the not-paid direction only (§2.8). Every other R-K3-8 reason (amount mismatches,
tombstone, `late_*`, bound-class parks) is **unchanged** and keeps its hold (owners PAYOUT-AMOUNT-DISPUTE-1,
PAY-PAYOUT-CONTRADICTION-HOLD-1). Allocation is never a payout route (unchanged).

### 4.2 New kinds on the existing K3 table (reuse, not a new table)

`payment_manual_resolutions.kind` CHECK widened with **`m4_evidence_paid`** and **`m4_evidence_not_paid`**
("M4"; M3 is ADR 0107's governed never-sent). Capability: the existing `payment_force_resolve:request|approve`
(same operation class, same proof operation strings, ADR 0110 table list unchanged for K3). New columns:
`evidence_line_id UUID NULL` (composite FK to `payment_statement_lines`), `evidence_reference TEXT NULL` (the PSP
line reference R; reserved-prefix CHECK as §5.4 of ADR 0101), `evidence_verdict TEXT NULL`. CHECKs:
M4 ⇒ `operation = 'payout'`, `target_state IS NULL`, `provider_id`, `basis_code = 'provider_confirmed_out_of_band'`,
`evidence_ref_hash`, `evidence_line_id`, `evidence_verdict` all NOT NULL, `finding_code` and
`reserved_provider_tx_id` NULL; `(kind = 'm4_evidence_paid') = (evidence_reference IS NOT NULL)`. The existing
partial UNIQUE indexes (one pending / one executed per attempt, any kind) make M2 and M4 mutually exclusive.
`payment_m2_admits` and `payment_attempts_guard()` are **not edited**: **M4 never changes the attempt**; it stays
`disputed` with its terminal reason (the M1 precedent; A-15), so the column-discipline trigger is untouched.

### 4.3 Deterministic evidence evaluation (DB function, also called from Go)

`payout_m4_evidence(p_tenant uuid, p_attempt uuid) RETURNS TABLE (verdict text, line_id uuid, reference text)`
(STABLE, `search_path` pinned, no SECURITY DEFINER), over persisted `kind = 'payout'` lines of
**eligible imports** (`is_mock = false`, or no `is_mock = false` import exists for (tenant, provider): the existing
§35.4 rule) with `tenant_id = p_tenant AND provider_id = attempt.provider_id AND merchant_reference =
attempt.merchant_reference` (index `payment_statement_lines_merchant`; positive attribution: the platform issues the
merchant reference, unique per tenant by `payment_attempts_tenant_merchant_ref`, immutable by the 0101 guard). **Bounded (security I-2):** the query reads at most `cap + 1` rows (cap = 64, a technical default
`security` confirms); more ⇒ verdict `evidence_overflow`, refused loudly with an audit, never truncated.

| Verdict | Conditions (all required) |
|---|---|
| `paid` | exactly one distinct provider reference R among `succeeded` lines; **every** such line has `amount = attempt.amount AND asset_code = attempt.asset_code` (**security I-1**); no `declined`, `reversed` or `pending` line for the merchant reference or for R in any eligible import; R carries no reserved prefix; no other `payment_attempts` row holds R as `provider_reference` at this provider; no `withdrawal_requests.provider_reference = R` and no `ledger_transactions` row with (provider, `provider_tx_id = R`) of any type (deposit, `withdrawal_completed`, `deposit_reversal`, tombstone) exists (the `payoutCompletedRef` / `yAttributable` positive-attribution rule; borrowed attribution never clears) |
| `not_paid` | at least one `declined` line with amount and asset equal; **no** `succeeded`, `pending` or `reversed` line for the merchant reference in any eligible import |
| `insufficient`, `contradictory`, `evidence_overflow` | anything else (no lines; amount/asset differ; two references; mixed statuses) |

Not-paid additionally requires the attempt's `ever_possibly_sent` value to be recorded (it is, R-6) and the
`provider_confirmed_out_of_band` basis (LF L-3 precedent). A statement source must be registered for the provider
(the existing O-4 rule). **Evidence sufficiency standard (A-13): both** the machine verdict **and** a four-eyes
operator confirmation (`evidence_ref_hash`) are required; neither alone suffices.

### 4.4 Submission, approval, execution

- **Request** (`POST .../payment-force-resolutions`, existing route, kinds widened): body `attempt_id`, `kind`,
  `evidence_line_id` (the line the operator reviewed), `evidence_ref_hash`, `basis_code`, `reason_code`. The insert
  trigger recomputes `payout_m4_evidence`; the stored `evidence_verdict`, `evidence_line_id`, `evidence_reference`
  are DB-forced and must equal the request (else `force_resolve_evidence_mismatch`); verdict must be `paid` for
  `m4_evidence_paid` and `not_paid` for `m4_evidence_not_paid`. R-6 pinning (`attempt_state_at_submission`,
  `terminal_reason_at_submission`) and the payload hash now also cover the three evidence columns.
- **Approvers (A-14):** the K3 rules unchanged (LF-11, S-12, S-2(iii), capability-specific grants, DB recount,
  closed-tenant platform-acting only) **plus at least one `platform_acting` approver** for both M4 kinds (the
  R-K3-10 posture applied now, fail-closed tightening). Counted in `payment_manual_resolution_execution_status`.
- **Execution** (final approval's transaction, ADR 0101 §6.3 lock order unchanged): L1
  `withdrawal.LockSubmittedForResolution` → attempt `FOR UPDATE` → resolution `FOR UPDATE` → approval insert →
  staff then grants `FOR SHARE` ascending → **re-evaluate `payout_m4_evidence`; verdict, line and R must equal the
  pinned values, attempt state/reason unchanged, withdrawal `submitted`** (else `refused_at_execution`) →
  `executing` → posting → `executed`, `ledger_transaction_id` linked → audit → commit. Statement lines take no lock
  (append-only); a contradicting line committed after execution is caught by the standing kinds (§4.6).
  - `m4_evidence_paid`: `withdrawal.Complete(tx, wr.ID, attempt.provider_id, R)` → `withdrawal_completed` keyed
    `provider_id:R`, `release_ledger_transaction_id` set. This is exactly the attempt's **own** release keyed by the
    **PSP line reference**, so `payoutCompletedRef` clears the standing finding **without loosening it** (LF F6).
  - `m4_evidence_not_paid`: `withdrawal.Fail(tx, wr.ID, "m4_evidence_not_paid")` → `withdrawal_failed` keyed
    `wr.id:failed`, hold → `player_cash`.
- **Deferred check** extended: executed M4 ⇒ withdrawal `completed` (paid) or `failed` (not paid); linked ledger
  row same tenant, `correlation_id = wr.id`, key `provider_id:evidence_reference` or `wr.id:failed`; attempt state
  and reason equal the pinned values.
- **Acting fences (0124 CREATE OR REPLACE, built on the 0115 bodies; down restores 0115 byte-for-byte):**
  `ledger_governed_fence_allows` + branch (f) `withdrawal_completed`, key `provider_id || ':' ||
  evidence_reference`, executing `m4_evidence_paid` in this txid; + branch (g) `withdrawal_failed`, key
  `wr.id || ':failed'`, executing `m4_evidence_not_paid`. `ledger_entries_governed_fence()` gains the same per-entry
  shapes as (b)/(c) (hold debit on `w.wallet_id`; credit `psp_clearing` with NULL wallet, or `player_cash` same
  wallet; amount = `w.amount` = `m.amount`, asset equal; ≤ 2 entries). The acting `ledger_accounts` INSERT and the
  acting `withdrawal_requests` UPDATE WITH CHECK widen their `m.kind IN (...)` lists to the M4 kinds.
- **ADR 0110 digest.** `actor_proof_payment_manual_resolutions_guard()` (INSERT) digest appends
  `NEW.evidence_line_id::text` (NULL for M1/M2 encodes as today's NULL field, so M1/M2 digests are byte-identical);
  `internal/actorproof.Digest` callers change in lockstep. No new proof operation, no table-list change.
- **Idempotency / replay:** one pending and one executed per attempt (indexes); a retried decide is refused by the
  approval guards with a fresh proof and posts nothing (ADR 0110 §5); the ledger keys are deterministic and unique.

### 4.5 Record fields (complete)

Actor (`requested_by`, scope, Person), approvers (approval rows: Person, scope, `decided_txid`, reason), attempt,
withdrawal, provider, evidence (`evidence_line_id`, `evidence_reference`, `evidence_verdict`, `evidence_ref_hash`,
basis), instrument (the attempt's snapshot `instrument_id` + `fingerprint` prefix, copied into the executed audit
metadata when a snapshot exists), reason code, timestamps (`created_at`, `decided_at`, `closed_at`), resulting state
(withdrawal before/after, attempt state unchanged, `ledger_transaction_id`). Audit actions:
`payment.manual_resolution_requested|approved|rejected|cancelled|executed|denied` (existing names, kind in metadata).

### 4.6 Reconciliation

No new mismatch kind and no kind-CHECK migration. Rules of ADR 0101 §9.1 extend by kind-set membership:
`pay_declared_not_paid_but_paid` also covers `m4_evidence_not_paid` (a later `succeeded` line for the merchant
reference); `pay_declared_paid_unconfirmed` also covers `m4_evidence_paid` with a later `reversed`/`declined` line on
R. **I-2 (reconciliation side):** `loadK3Evidence` keeps INV-M-5 (no evidence dropped) but gains a hard per-run cap
on persisted lines read (configurable; exceeding it **fails the run** with the existing P1 `reconciliation.
sweep_run_failed`), shared with CAS-RECON-SCALE-1. The finding hint text for unbound payout parks changes from "NOT
IMPLEMENTED" to naming M4.

### 4.7 API, errors, tests

Routes and permissions: existing `payment_force_resolution_routes.go`; new read
`GET /v1/admin/tenants/{tenantID}/payment-attempts/{id}/resolution-evidence` (`payment_force_resolve:read`) returning
the verdict and line id only. Error tokens added: `force_resolve_evidence_insufficient`,
`force_resolve_evidence_mismatch`, `force_resolve_evidence_overflow`. Tests: each verdict row (incl. amount/asset
mismatch I-1, two references, reserved prefix, R held by another attempt, R an existing deposit key, legacy
completion, MOCK vs non-MOCK import eligibility); paid clears `pay_captured_unposted` only through the own
completion keyed by R; not-paid never clears it; refusal of every out-of-scope reason; no M4 on bound or amount
parks; platform-approver floor; S-12/LF-11; proof required (AP001/AP002/AP004); re-evaluation at execution (a line
added between submission and execution ⇒ refused); concurrency `-race -count=50` (M4 vs poll, vs sweeper, two final
approvals, M2 vs M4); overflow cap; fence shapes and mutants (drop I-1, drop attribution, admit any key, drop the
platform-approver floor); down restores 0115 bodies.

---

## 5. Cross-part interactions

| Pair | Interaction |
|---|---|
| B13 × HSEC | A release (§6) sends nothing anywhere: the hold returns to the player's own `player_cash`; the instrument is irrelevant and is only copied into the record. "Resume after reactivation" re-runs the B13 destination gate at T1p, so a reactivated withdrawal with an unusable instrument stays parked (HD-R15-5). |
| B13 × RESOLVE-1 | `destination_mismatch` is M2-refused and M4-not-paid-only; M4 audit rows carry the snapshot instrument and fingerprint prefix. B13 is not a prerequisite of RESOLVE-1 (owner decision 1); for legacy/MOCK attempts without a snapshot the M4 record simply has no instrument. |
| RESOLVE-1 × HSEC | Disjoint: M4 requires an attempt and a `submitted` withdrawal; a hold release requires `approved` and **no** attempt. Both serialise on the withdrawal L1 lock. |
| All × kill switch / H-SEC (decision 18) | Unchanged. No part adds a dispatch path; an inactive tenant/brand still refuses every normal submission. |

---

## 6. HSEC-APPROVED-HOLD-RELEASE-1 — four-eyes release of an `approved` hold on a non-active tenant/brand (migration 0125)

### 6.1 Smallest mechanism

Owner decisions 13-18: no automatic release; hold stays; a controlled staff path exists; release/cancel needs
four-eyes; no unilateral staff action; kill-switch semantics intact. **Kinds (A-16):**

- **Resume after reactivation — no new kind.** When tenant **and** brand are `active` again, the existing staff
  submit (`ClaimForDispatch`) works unchanged, re-running the H-SEC gate, the KYC gate and the B13 destination gate.
  Reactivation itself is out of scope (TENANT-STATUS-AUTHZ-1; no brand-status writer exists).
- **`release_hold_to_player` — the only new kind.** Moves `approved → rejected` and posts the hold back to the
  player's own `player_cash` (same wallet). Never dispatches, never pays a third party.

### 6.2 Data model (subsumes ADR 0107 CT-PRE; not a parallel table)

**`withdrawal_hold_resolutions`** — the ADR 0107 §5.1 shape, implemented now for this slice and **replacing ADR
0107's planned `closed_tenant_hold_resolutions`** (ADR 0107's later CT-NEVER-SENT/permit/retain/rule work extends
this table): server-forced `id`; `tenant_id`, `withdrawal_request_id` (composite FK); `kind CHECK IN
('release_hold_to_player')`; DB-forced copies `brand_id`, `player_account_id`, `wallet_id`, `amount`, `asset_code`,
`payout_instrument_id`; pinned `withdrawal_state_at_submission` (= `approved`), `tenant_status_at_submission`,
`brand_status_at_submission`; `tenant_status_at_execution`, `brand_status_at_execution`; `reason_code` (bounded text,
1-64 bytes; vocabulary HD-CTF-7, non-blocking); `evidence_ref_hash NOT NULL`; `payload_hash` (DB-computed over all
pinned columns); forced `requested_by`, `requested_by_scope CHECK = 'platform_acting'`, `requested_by_person_id`;
`required_at_submission`, `contributing_policy_ids`, `required_at_execution`; K3 `state` set; DB-forced
`expires_at`; `executed_txid`; `ledger_transaction_id UUID NULL UNIQUE`; `refusal_code`. Partial UNIQUE
`(withdrawal_request_id) WHERE state = 'pending'` and `WHERE state = 'executed'`.
**`withdrawal_hold_resolution_approvals`** — the K3 approvals shape (payload-pinned, forced approver, scope
`platform_acting` only, `decided_txid`, UNIQUE `(resolution_id, decided_by)`, immutable).
RLS: ENABLE + FORCE; **family A only** (acting read/insert/update), no T (HD-CTF-2 default: tenant staff see
nothing), no P; DELETE/TRUNCATE denied; no SECURITY DEFINER. Grants: `SELECT, INSERT, UPDATE` on resolutions,
`SELECT, INSERT` on approvals.

### 6.3 Governance (reuse ADR 0099/0100/0101 §24.3 parity (i)-(viii) verbatim)

- Classification row `withdrawal_hold_resolution` = `mandatory_four_eyes` (0113 CHECK widened) — four-eyes
  **regardless of amount**.
- Capability pair **`withdrawal_hold_resolution:request` / `:approve`** (0112 `operation_kind` CHECK widened;
  `eligible_tenant_roles = '{}'`, `platform_grantee_allowed = true`, grants only with NOT NULL `valid_until`). Not
  `payment_force_resolve` (ADR 0107 Q-CT-SEC-2). This **replaces** ADR 0107's `closed_tenant_hold_resolution:*`
  name (A-20; security confirms).
- **Actors (A-17): `platform_acting` only** — requester and every approver — for suspended and closed tenants and
  for non-active brands (the strongest existing rule: ADR 0101 §24.5, ADR 0107 Q-CT-SEC-1). Static permissions
  `withdrawal_hold_resolution:request|approve|read` → `platform_admin` and platform `finance` only, never
  `tenant_admin`.
- Policy `financial_policy_required_approvals('withdrawal_hold_resolution', …)`, `GREATEST(1, …)`, the `0113:655`
  non-active special case extended; **no platform policy row ⇒ disabled** (the path is inert until a policy row is
  approved through the existing governed, proof-bound policy-change flow). LF-11 distinct non-NULL Persons
  (requester ≠ approver, non-configurable); S-12 (no requester/approver is the withdrawing player's Person); S-2(iii);
  HD-PRH2-8 interim (≥ 1 independent approver). A single staff member can therefore never release (decision 17).
- **Signed actor proof (ADR 0110 extension):** `zz_actor_proof_guard` added to both new tables (the catalog test
  becomes **eleven** tables). Operations: `withdrawal_hold_resolution:request` (target `new`, digest of tenant,
  withdrawal id, kind, reason, evidence hash), `:cancel` (requester only; target id; `payload_hash`), `:approve` /
  `:reject` (target resolution id; `payload_hash`). Scope must be `platform_acting` **with** a tenant; the NULL-tenant
  (empty field) encoding stays reserved to K1/policy operations — the verifier refuses it here. The service lives in
  `internal/payments` (`withdrawal_hold_resolution.go`), an already-allowed signer: `TestStatic_OnlyFourEyesPackagesSign`
  is not widened.

### 6.4 Preconditions, state transition, posting

Checked at insert, at each approval and at execution (DB trigger and executor):
withdrawal `state = 'approved'`; **no `payment_attempts` row** for it (LF CT-5); `tenants.status <> 'active' OR
brands.status <> 'active'` read in-tx under the H-SEC lock discipline (tenant status advisory lock SHARED, brand row
`FOR SHARE`). At execution, if both are `active` ⇒ `refused_at_execution` (`tenant_and_brand_active`: the normal path
applies; A-19). The R-6-style pinned fields must be unchanged.

New function **`withdrawal.ReleaseForGovernedResolution(ctx, tx, requestID, resolutionID)`** (ADR 0107 §7.2, narrowed
to `approved`): L1 lock; asserts `approved` and no attempt; posts `withdrawal_rejected` (debit
`player_withdrawal_hold`, credit `player_cash`, same wallet, `wr.amount`, `CorrelationID = wr.id`,
`ReversesTransactionID` = hold tx) with the **distinct key `<wr.id>:governed_hold_released`** (never the shared
`:rejected`, so `ledger.Post`'s replay can never adopt a tenant `Reject` posting); conditional `UPDATE ... SET state =
'rejected', release_ledger_transaction_id = $tx WHERE id = $1 AND state = 'approved'` (RowsAffected = 1); no
`reason_code` on the ledger row (reason on resolution + audit). Target state (A-18): `rejected`, not `cancelled`,
because `cancelled` means a player action.

**DB gating in ALL sessions (ADR 0107 CT-R3):** a BEFORE UPDATE trigger on `withdrawal_requests` refuses any
`→ rejected` whose `release_ledger_transaction_id` is a `:governed_hold_released` posting unless an `executing`
resolution for this withdrawal has `executed_txid = txid_current()`; an all-sessions BEFORE INSERT trigger on
`ledger_transactions` refuses any `idempotency_key` with suffix `:governed_hold_released` (`right()`, not `LIKE`)
without that executing resolution. **Acting fences:** branch (e) in `ledger_governed_fence_allows`
(`withdrawal_rejected`, that key, `correlation_id = wr.id`); per-entry shape in `ledger_entries_governed_fence()`
(hold debit / cash credit, same wallet, amount and asset equal, ≤ 2 entries); acting `ledger_accounts` INSERT widened
for `player_withdrawal_hold` of the executing resolution's wallet/asset; acting `withdrawal_requests` UPDATE WITH
CHECK widened to "executing M2/M4 **or** executing hold resolution". 0125 builds on the **0124** bodies; its down
restores them byte-for-byte.

### 6.5 Lock order, concurrency, idempotency

Execution order (final approval's tx): L1 withdrawal `FOR UPDATE` → tenant advisory lock SHARED + brand `FOR SHARE`
→ resolution `FOR UPDATE` → approval insert → staff then grants `FOR SHARE` ascending → recount → re-check → `executing`
→ `ReleaseForGovernedResolution` (L3/L4 via `LockProjectionsForPosting`) → `executed` → audit → commit. ADR 0082 A8:
the resolution's L1 position is after `payment_manual_resolutions`, before `ledger_adjustment_requests` (ADR 0107
CT-3). Races with `ClaimForDispatch` after a reactivation, with KYC deny, and with a second release all serialise on
the withdrawal L1 lock; exactly one wins; the loser sees a state outside the preconditions. Exactly-once: L1 lock +
state CAS + distinct key + partial UNIQUE on executed + deferred check (no `executing` at commit; executed ⇒
withdrawal `rejected`, linked same-tenant `withdrawal_rejected` with the key). Retries get fresh proofs (ADR 0110 §5).

### 6.6 Audit

Every request, approval, rejection, cancellation, expiry, refusal (including every DB refusal mapped to a closed
token set `hold_release_disabled|not_permitted|precondition_failed|conflict|expired|not_found`) and execution writes
`withdrawal.hold_resolution_<event>` in the same transaction (actor, scope, acting tenant, Persons, tenant/brand
status at submission/execution, withdrawal before/after, ledger tx, reason, evidence hash, IP, UA, request id); the
ADR 0104 tenant-visible projection applies.

### 6.7 Relation to ADR 0107 (explicit)

In scope: CT-PRE × `release_hold_to_player_cash` for **`approved` only**, for **suspended or closed tenants and
non-active brands**. Not in scope: `requested` (player `Cancel` exists) and `pending_review` (staff `Reject` exists,
ungated); CT-NEVER-SENT / governed M3; dispatch permits; `retain_pending_determination`; the jurisdiction rule tables
(`closed_tenant_resolution_rules`, HD-CTF-1(b)); HD-CTF-2..10 (player access to released cash on a closed tenant,
balances not under a withdrawal, closure while holds exist, notifications, reopening, credentials). ADR 0107 stays
PROPOSED/DESIGN ONLY for everything else and, when built, extends `withdrawal_hold_resolutions` and the
`withdrawal_hold_resolution:*` capability rather than creating parallel objects.

### 6.8 Tests and acceptance (HSEC)

Release posts exactly one `:governed_hold_released`, hold nets to zero, cash restored, balanced, projection =
rebuild; second release refused; refusal matrix: active tenant+brand, `requested`/`pending_review`/`submitted`
states, attempt exists, single approver, same Person twice, beneficiary Person, tenant-scope actor, capability held
only as `payment_force_resolve:*` or `ledger_adjustment:*`, expired/revoked grant, no policy row; tenant reactivated
between submission and execution ⇒ `refused_at_execution`; CT-R3 edge and key refused in tenant, acting and system
sessions; fence shape mutants; proof absent/forged/replayed (AP001/2/4/5); concurrency `-race -count=50` (release vs
`ClaimForDispatch` after reactivation, two final approvals, release vs status change); H-SEC/kill-switch regression
suites green (decision 18); migration up/down/up, whole-schema snapshot, ADR 0110 eleven-table catalog pin.

---

## 7. Implementation plan

### 7.1 Files per part (Touches)

| Part | Migration | Primary files |
|---|---|---|
| B13-A entity | 0123 | new `internal/payoutinstrument/*` (kinds, validators, normalisers, fingerprint/seal, `PayoutInstrumentVerifier` + Synthetic mock), `internal/config` (keys), `cmd/platform-api` (wiring, startup gate, providerkind registration), new `internal/httpserver/payout_instrument_routes.go`, `internal/auth/permission.go`, `backoffice/src/auth/permissions.ts`, `deploy/init-app-role.sql` |
| B13-B integration | (uses 0123) | `internal/withdrawal/withdrawal.go` (RequestParams, columns, scan), `internal/payments/{types.go,payout.go,payout_sweep.go,payout_alerts.go,receipt.go,contract.go,manual_resolution.go (PayoutDisputeReasons only)}`, `internal/httpserver/withdrawal_handlers.go`, `internal/reconciliation/payment_statement.go` (`disputeReasonClasses` only) |
| RESOLVE-1 | 0124 | `internal/payments/manual_resolution.go` (+ new `manual_resolution_m4.go`), `internal/reconciliation/{payment_statement.go,payment_statement_k3.go}`, `internal/httpserver/payment_force_resolution_routes.go`, `deploy/init-app-role.sql` |
| HSEC | 0125 | `internal/withdrawal/withdrawal.go` (`ReleaseForGovernedResolution` only), new `internal/payments/withdrawal_hold_resolution.go`, `internal/capability/capability.go`, `internal/auth/permission.go`, `backoffice/src/auth/permissions.ts`, new `internal/httpserver/withdrawal_hold_resolution_routes.go` + one line in `routes.go`, `deploy/init-app-role.sql` |

### 7.2 Migrations: merge, split, order

- Keep **three separate migrations**: they are independent gates (owner decision 1) with different lead reviewers.
  Do not split 0123 further (entity, binding and snapshot are one review unit; a half-applied binding has no
  meaning). Do not merge 0124 with 0125: different capabilities and fences, different reviewers.
- `internal/db` refuses version gaps (`findVersionGaps`), so **merges are strictly in number order** (0122 → 0123 →
  0124 → 0125). If a later-numbered part is ready first, the orchestrator renumbers at merge time.
- **Hard dependency 0124 → 0125:** both CREATE OR REPLACE `ledger_governed_fence_allows`,
  `ledger_entries_governed_fence()`, the acting `ledger_accounts` INSERT and the acting `withdrawal_requests` UPDATE
  policy. 0125 must be written on 0124's bodies (and its down restores 0124's). If HSEC must land first, swap the
  numbers and invert the dependency; never develop both against 0115 independently.
- 0123 touches none of those objects; 0124's `destination_mismatch` literal is inert without 0123.

### 7.3 Parallelism without file conflicts

Wave 1 (parallel): **B13-A** and **RESOLVE-1** and **HSEC** design-to-code start together; the only shared files are
`internal/auth/permission.go`, `backoffice/src/auth/permissions.ts` and `deploy/init-app-role.sql` (append-only
blocks; resolve by ordered append) and `manual_resolution.go` (RESOLVE-1 owns it; B13-B adds one map entry after
RESOLVE-1 merges). HSEC's SQL waits for 0124's fence bodies (§7.2). Wave 2: **B13-B** after B13-A merges (depends on
the package and 0123). Sibling launch conditions (S-L1/S-L3/S-L4), PAY-PAYOUT-BOUND-CLEAR follow-ups and
ALERT-DELIVERY-1 are unchanged and still gate any non-MOCK payout.

## 8. Review gates

| Part | Gates (all required before merge) |
|---|---|
| B13 | `security` (lead: seal/keys, S95-C10 echo, tiering, RLS/grants, threat T9), `identity-compliance` (verification sources, ownership assertion, max-age mechanism), `ledger-finance` (no hold release on any B13 refusal, T2/T12 escalation semantics, lock position), `payments`, `qa` (test plan §2.10), `code-reviewer`, `architect` (cross-domain) |
| RESOLVE-1 | `ledger-finance` (lead, binding: evidence standard, F6 attribution, I-1, posting shapes, reconciliation kinds), `security` (I-2 cap, proof digest, platform-approver floor), `payments`, `qa`, `code-reviewer` |
| HSEC | `security` (lead: actor scope, capability rename vs Q-CT-SEC-2, proof extension), `ledger-finance` (key, fences, exactly-once), `product-owner-proxy` (scope = approved-only slice), `qa`, `code-reviewer` |

Every part: mutation-kill evidence file, runtime-role integration tests, migration up/down/up on a scratch DB.

## 9. AMBIGUITIES (chosen interpretation and why)

- **A-1 Binding time.** Bind at request creation; re-validate at T1p; snapshot at T1p; re-check (never re-resolve)
  at T2/T12. Why: approvers four-eyes a fixed destination; KYC-gate precedent (LF95-C10); resends stay identical.
- **A-2 Scope/sharing.** Instrument belongs to one player account (tenant, brand), Person denormalised; a fingerprint
  is owned by one Person per tenant forever. Why: tenant isolation; shared instruments are a fraud signal.
- **A-3 Kind taxonomy.** Open reference table; crypto seeded but disabled for non-synthetic until ADR 0008/D3; cash
  (D4) is not an instrument. Why: owner decision 6 without pre-empting open decisions.
- **A-4 Verification source.** Closed set, produced only by a verifier adapter; no staff verification; `synthetic`
  accepted only by Synthetic payment adapters. Why: decision 8; staff attestation would be a normal override.
- **A-5 Ownership standard.** Only `account_holder_matches_verified_identity` for real sources. Why: strongest
  "player-bound"; others need compliance (HD-R15-2).
- **A-6 Re-verification cadence.** `expires_at` NOT NULL = min(source, configured max age); no config ⇒ real
  verification unusable. Why: fail closed without inventing a value (HD-R15-1).
- **A-7 Detail at rest.** No PAN ever (tokens only); bank/e-wallet identifiers in RLS-protected `detail`; staff see
  masks only; application-level encryption deferred to `security` (follow-up PAYOUT-INSTRUMENT-PII-1).
- **A-8 Echo.** Fingerprint-only echo computed in the adapter; unknown kid = mismatch. Why: S95-C10 intact; fail
  closed.
- **A-9 Player revocation** refused while a non-terminal withdrawal binds the instrument. Why: avoids self-parking.
- **A-10 `payment_method`** derived from the instrument rail; a differing body value is refused, not ignored.
- **A-11 Legacy rows.** No backfill (MOCK/dev only); NULL binding dispatchable only to Synthetic adapters.
- **A-12 RESOLVE-1 scope.** Unbound reasons + `destination_mismatch` (not-paid only); all other R-K3-8 reasons
  unchanged.
- **A-13 Evidence sufficiency.** Deterministic persisted statement evidence by merchant reference **and** four-eyes
  out-of-band confirmation; amount/asset equality; contradictions refuse.
- **A-14 M4 approvers.** K3 rules plus ≥ 1 `platform_acting` approver (R-K3-10 applied early).
- **A-15 Attempt under M4** stays `disputed`. Why: no guard diff; history not rewritten (M1 precedent).
- **A-16 HSEC kinds.** Only `release_hold_to_player`; resume = existing submit after reactivation.
- **A-17 HSEC actors.** `platform_acting` only for every non-active case.
- **A-18 HSEC state.** `approved → rejected` with distinct key `:governed_hold_released`.
- **A-19 HSEC execution** refused if tenant and brand are both active again.
- **A-20 Naming.** `withdrawal_hold_resolutions` + `withdrawal_hold_resolution:*` subsume ADR 0107's names
  (security to confirm against Q-CT-SEC-2, which ruled only "do not reuse `payment_force_resolve`").
- **A-21 Seal.** Go-side HMAC seal over instrument/verification/snapshot under THREAT-MODEL-ARBITRARY-SQL-1.

## 10. HUMAN DECISIONS STILL REQUIRED (the design proceeds fail-closed around each)

- **HD-R15-1** Verification max-age / re-verification cadence per jurisdiction (compliance value). Until set, no
  real verification can be recorded (blocks sandbox/real payouts only).
- **HD-R15-2** Which further ownership assertions count as "verified" (e.g. card token proven by the player's own
  deposit, PSP rails binding — supplement Q6; custodian address attestation; e-wallet login proof).
- **HD-R15-3** Return-to-source (D2) — compliance/legal; not enforced meanwhile.
- **HD-R15-4** Crypto payout destinations (D3) — tied to ADR 0008 custodian decision; kind disabled for real use.
- **HD-R15-5** Disposition of an `approved` withdrawal (active tenant) whose bound instrument becomes permanently
  unusable: remain parked (current design) vs governed four-eyes release vs governed rebind.
- **HD-R15-6** Disposition of a confirmed misdirected payout (paid to a destination other than the snapshot):
  retained today; recovery/make-whole is business/legal.
- **HD-R15-7** (non-blocking, from the HSEC brief Q5/Q6) whether `Approve` should be gated on non-active
  tenant/brand, and whether a refused staff submit must always leave an audit row (B13 refusals do).
- Still open and unchanged: HD-CTF-1(b), HD-CTF-2..10 (closed-tenant consequences beyond this slice), HD-PRH2-3
  (thresholds) and HD-PRH2-8; enabling policy rows are created through the existing governed flow.
