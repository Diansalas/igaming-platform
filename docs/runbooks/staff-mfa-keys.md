# Runbook: staff MFA key provisioning and rotation (ADR 0119 section 5, slice S1)

Status (2026-10-13):

- The key library (`internal/staffmfa`) and the configuration validation (`config.Load`) are `IMPLEMENTED`
  (ADR 0119 slice S1).
- **Nothing reads these keys yet.** Staff login with MFA, enrolment, step-up and the session seal are slices S2-S4b
  and are `NOT IMPLEMENTED`. `staffmfa.VerifyStartup` exists but is not yet called by `cmd/platform-api` or
  `cmd/seed-admin` (it is wired in S3).
- Production key provisioning is `PROVIDER DEPENDENT` on the operator and the deployment secret store, as for
  ADR 0110 (`ACTOR_PROOF_KEYS`). AWS is OFF today; no environment holds real keys.
- The full rotation campaigns (FACTOR re-enrolment campaign, recovery-code regeneration campaign with its
  counting queries) are slice S9. This runbook covers provisioning and the parts of rotation that need no
  database table.

**Staging needs both families before the S3 deploy.** From S3, every environment refuses to start without them.

## 1. What the keys protect

There are two independent families. Each is supplied as a `kid:base64(secret)` list plus an active kid.
The keys are **never stored in the database**: the database holds only ciphertext and keyed MACs.

| Family | Variables | HKDF-SHA256 subkeys (`info = label "|" kid`) | Protects | Rotation |
|---|---|---|---|---|
| FACTOR | `STAFF_MFA_FACTOR_KEYS`, `STAFF_MFA_FACTOR_ACTIVE_KID` | `staff-mfa-factor-aead-v1` | AES-256-GCM sealing of every TOTP factor secret. The AAD binds the factor id, the staff member, the tenant (or `platform`) and the factor type. | **Re-enrolment only**, no rewrap (section 3) |
| MAC | `STAFF_MFA_MAC_KEYS`, `STAFF_MFA_MAC_ACTIVE_KID` | `staff-mfa-recovery-v1`, `staff-auth-challenge-v1`, `staff-enrolment-token-v1`, `staff-session-assurance-v1` | Keyed hashes of recovery codes, login/enrolment challenges and invite/reset/bootstrap tokens; the staff session assurance seal | Age-out with a verify-only overlap (section 4) |

Rules enforced by `config.Load` (and again by `staffmfa.NewKeys`):

- every entry is `kid:base64`; a kid is 1-32 characters of `[A-Za-z0-9._-]`; an empty entry (for example a
  trailing comma) is refused;
- every key decodes to at least 32 bytes (at most 1024), and a key whose bytes are all equal (for example all zero)
  is refused;
- each active kid must name a key of its family;
- no key value may appear under two kids, in both families, or in **any other platform key family**: the single
  registry in `internal/config/staff_mfa.go` checks every pair of `JWT_SIGNING_SECRET`, `JWT_PREVIOUS_SECRET`,
  `PROVIDER_CREDENTIAL_FINGERPRINT_KEY`, `ACTOR_PROOF_KEYS`, `PAYOUT_INSTRUMENT_KEYS`, `PAYOUT_INSTRUMENT_FP_KEYS`,
  `STAFF_MFA_FACTOR_KEYS` and `STAFF_MFA_MAC_KEYS`. Keys are compared both as text and as decoded bytes, so pasting
  the base64 text of one family's key as another family's value (for example `JWT_SIGNING_SECRET=X` and
  `STAFF_MFA_MAC_KEYS=m1:X`) is refused too;
- a partial configuration (one family without the other, keys without an active kid) is refused in every
  environment;
- `APP_ENV=production` without both families is refused;
- the published `.env.example` placeholders (and the published fingerprint placeholders) are refused unless
  `APP_ENV` is explicitly `development`. A missing `APP_ENV` counts as production for this check.

The values are `config.SecretValue` and the library types redact: they never render through `fmt`, `slog` or
JSON. Error messages name the family and the kid, never key material.

There is **no development bypass**, no random-key fallback and no "MFA disabled" switch.

## 2. Provisioning a new environment

Do this once per environment (development machines included, but each with its own keys). Two operators
should be present for staging and production (a runbook control, not a software control).

1. **Generate two independent secrets**, one per family, on a trusted machine or directly in the secret store:

   ```sh
   openssl rand -base64 48   # FACTOR family secret
   openssl rand -base64 48   # MAC family secret (a NEW value, never the same output)
   ```

   48 random bytes encode to 64 base64 characters; anything from 32 bytes up is accepted. Never derive one
   family from the other, never reuse a key of another family or another environment, and never paste a key
   into a ticket, chat, shell history that is synced, or the repository.
2. **Choose kids.** Use a dated, environment-scoped kid so a later rotation is legible, for example
   `stg-2026-10` and `prd-2026-10`. Kids are not secret.
3. **Store the values in the deployment secret store** under the four variable names, exactly as
   `ACTOR_PROOF_KEYS` and `PAYOUT_INSTRUMENT_KEYS` are delivered (ADR 0110 section 6, ADR 0111 section 2.2):
   - `STAFF_MFA_FACTOR_KEYS` = `stg-2026-10:<factor base64>` (secret)
   - `STAFF_MFA_FACTOR_ACTIVE_KID` = `stg-2026-10` (plain)
   - `STAFF_MFA_MAC_KEYS` = `stg-2026-10:<mac base64>` (secret)
   - `STAFF_MFA_MAC_ACTIVE_KID` = `stg-2026-10` (plain)

   The same kid string may be used in both families: kids are per family. The secrets must differ.
4. **Inject them into every process that will need them.** `platform-api` from S3 on, and the
   `cmd/seed-admin` bootstrap job (ADR 0119 section 11), which runs **inside the deployment environment** with
   the same injection. They must never be present on an operator workstation for staging or production.
5. **Verify** by starting the service (or running a configuration-only start) with the new values:
   - a malformed, short, duplicated or placeholder value refuses startup with a message naming the variable;
   - in production, missing values refuse startup with
     `STAFF_MFA_FACTOR_KEYS / ... are required when APP_ENV=production`.

   Do not "test" a key by echoing it or by logging it.
6. Record in the environment's change log: date, kids (never values), who provisioned, who witnessed.

### Local development

Either leave the four variables unset (allowed outside production until S3), or generate throw-away keys as in
step 1. The `.env.example` placeholders are accepted **only** with `APP_ENV=development` set explicitly.
From S3, staff login on a development machine needs keys.

## 3. Rotating the FACTOR family (re-enrolment only)

The factor ciphertext columns are immutable and the runtime role cannot UPDATE them, so there is no rewrap job
(ADR 0119 section 5.3, review C3).

1. Generate a new FACTOR secret (step 2.1). Add it under a new kid **keeping the old kid**:
   `STAFF_MFA_FACTOR_KEYS=old-kid:<old>,new-kid:<new>`. Set `STAFF_MFA_FACTOR_ACTIVE_KID=new-kid`. Deploy.
   New enrolments are sealed under `new-kid`; existing factors keep opening under `old-kid`.
2. Run an MFA-reset campaign for every factor still under `old-kid`. **`NOT IMPLEMENTED`:** the counting query
   and the campaign procedure are slice S9 (the factor table is slice S2).
3. Remove `old-kid` only when no active or pending factor references it. After removal, any factor still under
   it fails closed: verification counts a failure and raises `staff_mfa_integrity` (from S3).

**Emergency rotation (suspected FACTOR key leak):** every staff member re-enrols. Treat it as ADR 0110 T6
(application-host compromise) and have `security` review the audit trail. Do steps 1-3 with the campaign
covering every staff member immediately.

## 4. Rotating the MAC family

1. Add the new secret under a new kid, keep the old one, switch `STAFF_MFA_MAC_ACTIVE_KID` to the new kid,
   deploy. New recovery codes, challenges, tokens and session seals use the new kid; rows under the old kid
   keep verifying (the library computes lookups under every retained kid).
2. Challenges (5 min), enrolment tokens (at most 72 h) and session seals (at most 12 h) age out. Recovery codes
   do not: the old kid must stay verify-only until every code under it is consumed or regenerated, through a
   regeneration campaign with a hard retirement date (`NOT IMPLEMENTED`, slice S9). This coupling is accepted
   explicitly by ADR 0119.
3. Remove the old kid only after step 2. A removed (retired) kid is refused: rows under it are never found and
   never verify.

## 5. Losing a key

- **FACTOR key lost (all kids):** no factor opens. Every staff login fails closed. Restore the secret from the
  secret store; there is no way to recover factor secrets from the database. If the secret cannot be restored,
  every staff member re-enrols (the bootstrap path of ADR 0119 section 11 for the first platform admin).
- **MAC key lost:** recovery codes, live challenges and enrolment tokens stop matching, and (from S3) every
  staff session seal fails, so every staff refresh is refused. Restore the secret; otherwise regenerate
  recovery codes and re-issue tokens.

## 6. Never

- Never put a key in the repository, a migration, a database row, a log line, an audit record, an error message,
  a ticket or a chat message.
- Never reuse a key across the two families, across environments, or with any other platform key family (the
  registry refuses it, but only for keys loaded into the same process).
- Never add a "disable MFA" setting or a test bypass to work around missing keys.
