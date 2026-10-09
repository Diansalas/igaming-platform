# Runbook: payout instrument keys, rotation and re-fingerprinting (ADR 0111 section 2.2, migration 0123)

Status: written with B13-A. The Go primitives (`payoutinstrument.Service.Refingerprint`, key
loading, the startup gate) are `IMPLEMENTED`. There is **no operator CLI and no scheduler** for the
re-fingerprint job or the expiry sweep yet (`NOT IMPLEMENTED`, listed in the ADR 0111 appendix B13-A
implementation notes); steps that need them say so.

## 1. What the keys protect

Two independent key families, both supplied as `kid:base64(secret)` lists plus an active kid, **never
stored in the database**:

| Family | Variables | Used for | Lifecycle |
|---|---|---|---|
| Master | `PAYOUT_INSTRUMENT_KEYS`, `PAYOUT_INSTRUMENT_ACTIVE_KID` | HKDF-SHA256 subkeys: seals (`b13-seal-v1`) and the detail AEAD (`b13-detail-aead-v1`) | rotate with overlap; retired kids stay verify/decrypt-only |
| Fingerprint | `PAYOUT_INSTRUMENT_FP_KEYS`, `PAYOUT_INSTRUMENT_FP_ACTIVE_KID` | tenant-bound HMAC fingerprint (`b13-fp-v1`) | long-lived; a new active kid only after the re-fingerprint job |

Rules (enforced by `config.Load`): every key at least 32 bytes; the active kid must have a key; no
key may equal a JWT secret, an actor-proof key, `PROVIDER_CREDENTIAL_FINGERPRINT_KEY`, or a key of the
other family. Keys must be unique per environment (not machine-checkable: do not reuse a key
across environments). The values are `config.SecretValue`: they never render through fmt, slog or JSON.

Startup gate (`payoutinstrument.VerifyStartup`): both families are required when
`cfg.GuardEnvironment()` is `production` (a missing `APP_ENV` counts as production) **or** whenever any
non-Synthetic payout adapter or instrument verifier is registered, in any environment. Without keys
(and without either trigger) the payout instrument routes answer 503. There is no random-key fallback in
the binary.

**B13-B behaviour change (ADR 0111 section 18): development and MOCK deployments now need the keys too.**
Binding a payout instrument is mandatory for every NEW withdrawal request, MOCK included (owner decision 8:
only the verification SOURCE is flexible in MOCK). Without the key families `POST /v1/me/withdrawals` answers 503
and writes nothing. Generate throw-away keys for a local environment exactly as for production (a fresh 32-byte
secret per family, base64) and keep them out of the repository. The former L-8 startup clause (refuse every
non-Synthetic payout adapter until B13-B lands) is removed; a non-Synthetic adapter still needs the keys.

## 2. Losing a key

* **Master key lost (all kids)**: every stored detail ciphertext and every seal becomes unreadable and
  unverifiable. Gates refuse (fail closed), so no payout can be bound or dispatched. Recovery is a
  restore of the secret from the secret store. There is no way to regenerate detail from the database.
* **Fingerprint key lost**: no new registration can compute a fingerprint; existing instruments fail the
  gate (the fingerprint is recomputed from the decrypted detail). Restore the secret.
* **Key suspected leaked**: treat as ADR 0110 T6 (application-host compromise). A leaked master key
  allows forging seals; a leaked fingerprint key allows offline confirmation of a guessed destination.
  Rotate (below) and have `security` review the audit trail of `payout_instrument.*` actions.

## 3. Rotating the master family

1. Generate a new secret in the secret store; add `newkid:base64` to `PAYOUT_INSTRUMENT_KEYS` **keeping
   the old kid**; leave `PAYOUT_INSTRUMENT_ACTIVE_KID` unchanged; deploy (verify the service starts).
2. Set `PAYOUT_INSTRUMENT_ACTIVE_KID=newkid`; deploy. New registrations, verifications, blocking events
   and snapshots are sealed/encrypted under `newkid`; rows already written keep their `seal_kid` /
   `detail_key_kid` and keep verifying under the old kid.
3. The old kid may be removed **only when no row references it**:
   `SELECT 1 FROM payout_instruments WHERE seal_kid = 'oldkid' OR detail_key_kid = 'oldkid'`
   (and the same for `seal_kid` on `payout_instrument_verifications`,
   `payout_instrument_blocking_events`, `payout_attempt_destination_snapshots`) returns no row in any
   tenant. Existing rows are **not** re-sealed by any job (`NOT IMPLEMENTED`), so in practice a master
   kid is retired only after every instrument it sealed is terminal.

## 4. Rotating the fingerprint family (re-fingerprint job)

A new fingerprint kid must not become active before ownership is recorded under it, otherwise a second
Person could register a destination the first Person owns (the owner rows are keyed by kid).

1. Add `newkid:base64` to `PAYOUT_INSTRUMENT_FP_KEYS`; keep the old kid; leave the active kid; deploy.
   From this moment registration computes the fingerprint under **every** retained kid and checks
   ownership under each, so the old destinations stay protected.
2. Run the re-fingerprint job per tenant: `Service.Refingerprint(ctx, tx, tenantID, newkid)` decrypts
   every non-terminal instrument's detail, computes the new-kid fingerprint and inserts the owner row
   under the new kid. It returns `{Instruments, Conflicts}`. **Operator command: `NOT IMPLEMENTED`**
   (the method and its tests exist; a one-off command must be written and reviewed by `security`
   before the first rotation).
3. Proceed only if `Conflicts == 0` for every tenant. A conflict means the same fingerprint under the
   new kid is already owned by another Person: stop, do not activate, escalate to `security` and
   `identity-compliance`.
4. Set `PAYOUT_INSTRUMENT_FP_ACTIVE_KID=newkid`; deploy. The old kid stays in the list: it is still
   checked on every registration. It may be dropped only when no non-terminal instrument carries it
   **and** the business accepts that ownership under the old kid stops being enforced
   (owner rows are never deleted, so this is a policy decision, not a cleanup).

## 5. Verification expiry and the sweep

`Service.Sweep` is the only writer of `verified -> verification_expired` and also completes pending
supersessions. Gates never write expiry; they refuse on `expires_at <= now()`, so a missing sweep
**fails closed** (an expired verification is refused whether or not the state was updated). The
scheduler that calls `Sweep` is `NOT IMPLEMENTED` (the payments sweeper loop is owned by another
workstream); until it exists, expired instruments remain `verified` in the state column and are refused
by the gate.

## 6. Per-jurisdiction max age (HD-R15-1)

`payout_instrument_verification_max_age (jurisdiction_id, max_age)` is read-only for the runtime role;
rows are written by the migration/owner role (a governed four-eyes writer is recorded under ADR 0110
T8). With no row for the tenant's licence jurisdiction **no non-synthetic verification can be
recorded**. This is launch-blocking for any sandbox or real payout.
