# Runbook: secrets inventory and credential rotation (index)

Status: written in A13. This is the single index; each family keeps its own detailed procedure, linked below. Rules for all of them:
secrets live only in the secret store / deployment secret system (never in git, tickets, chat, logs or shell history); a rotation is a
change ticket with two people for anything marked four-eyes; an agent never requests or handles a production secret; production
values are an owner action. Everything here is exercised against synthetic values only; no real provider or production secret exists.

## 1. Inventory

| Secret | Held in | Consumed as | Rotation: overlap? | Procedure |
|---|---|---|---|---|
| Runtime DB password (`igaming_runtime`) | secret store (staging: Secrets Manager `db_runtime`) | `PGPASSWORD` | no overlap: role-init re-sets it, then services roll | staging: `stage-9-4-staging-lifecycle-runbook.md` section 10 (bump `secret_version`, `deploy.sh up`; a replacement task can briefly fail DB auth, so rotate outside a test run). Production: `production-startup-database-config.md`, role changes are a human/database-authority action only |
| RDS master password | RDS-managed secret | one-off tasks only | RDS rotates it | `stage-9-4-staging-lifecycle-runbook.md` section 10 |
| Player JWT signing secret | secret store | `JWT_SIGNING_SECRET` (+`JWT_ACTIVE_KID`) | yes: old secret as `JWT_PREVIOUS_SECRET`/`JWT_PREVIOUS_KID` | `production-configuration-checklist.md` JWT section. Keep the previous key until the longest outstanding token ages out: `REFRESH_TOKEN_TTL_SECONDS` defaults to 30 days, so the window is up to 30 days; the two secrets must differ from each other and from every other key family |
| Signed actor proof keys (K1/K2/K3, launch, policy) | secret store + `actor_proof_keys` table (verification copy) | `ACTOR_PROOF_KEYS`, `ACTOR_PROOF_ACTIVE_KID` | yes: two kids during the overlap, old retired as the owner role | `operational-runbooks.md` section 15 (provision, rotate, emergency, symptoms `AP001`..`AP005`). Production refuses to start without it |
| Staff MFA factor and MAC keys | secret store | `STAFF_MFA_FACTOR_KEYS`/`_ACTIVE_KID`, `STAFF_MFA_MAC_KEYS`/`_ACTIVE_KID` (required in EVERY environment) | MAC: yes (retired kid stays verify-only); factor: by re-enrolment campaign | `staff-mfa-keys.md` |
| Payout instrument master and fingerprint keys | secret store | `PAYOUT_INSTRUMENT_KEYS`/`_ACTIVE_KID`, `PAYOUT_INSTRUMENT_FP_KEYS`/`_ACTIVE_KID` (required in production, and wherever withdrawals are used: without them `POST /v1/me/withdrawals` is 503) | master: yes, old kid kept until no row references it (no re-seal job); fingerprint: only after a re-fingerprint job (no operator CLI) | `payout-instrument-keys.md` |
| Provider credential fingerprint key | secret store | `PROVIDER_CREDENTIAL_FINGERPRINT_KEY` (absent = real credential subsystem not built) | **no**: changing it makes every stored fingerprint mismatch and every resolve fail closed (`credential_integrity`); needs a new fingerprint scheme (ADR 0093) | design change, not a routine rotation |
| Per-tenant provider credentials (PSP, casino, sportsbook, KYC) | secret store handles referenced by `provider_credentials` rows (`awssm://` in staging/production, `devfile://` development only) | resolved per tenant by `internal/providercred` | yes: register a new handle (four-eyes approval by a different platform admin), then retire/revoke the old; revocation is single actor | ADR 0093 section 3; routes `/v1/admin/tenants/{tenantID}/provider-credential-requests` (+`/decisions`, `/apply`) and `.../provider-credentials/{handleID}/transitions`. Real vendor credentials: PROVIDER DEPENDENT, none exist |
| Webhook signing secrets (inbound PSP/casino callbacks) | provider credential handles | `internal/webhookauth` | as provider credentials; vendor-side rotation is PROVIDER DEPENDENT | ADR 0097, ADR 0093 |
| Alert webhook HMAC secret | secret store | `ALERT_WEBHOOK_SECRET_REF` (`awssm://...?versionId=` or `devfile://`), at least 32 bytes | change the reference/version and restart; the receiver must accept both during the change | `alert-delivery-channels.md` |
| Backup signing key, age/gpg identity, backup IAM grant | runner secret store (never beside the backups) | `BACKUP_SIGNING_KEY_FILE`, `RESTORE_VERIFY_PUBKEY_SHA256` pin, `BACKUP_ENCRYPT_AGE_RECIPIENT`, IAM `rds-db:connect` | rotate after any suspected exposure; re-pin the fingerprint in change control | `backup-restore-dr.md` sections 2a and 5a |
| Seed admin password (staging) | Secrets Manager | `SEED_ADMIN_PASSWORD` (one-off task) | not rotated by `secret_version`: the hash lives in the database | `stage-9-4-staging-lifecycle-runbook.md` section 10 |
| Staff and player passwords | database hashes | n/a | no staff password-reset route (`NOT IMPLEMENTED`); b2c password reset UI is a B-class gap | `staff-lifecycle-and-mfa-recovery.md` section 5 |

`config.Load` and the key-family registry refuse a key that equals another family's key, a published `.env.example`/CI placeholder (outside an explicit
`APP_ENV=development`), or a value shorter than 32 bytes. Values are `config.SecretValue` and never render through fmt, slog or JSON.

## 2. Standard rotation shape (overlap families)

1. Generate the new secret with a CSPRNG in the secret store (for example `openssl rand -base64 48`); choose a new kid.
2. Add it alongside the old one (new kid listed, active kid unchanged) and deploy; confirm the process starts (startup gates verify the keys).
3. Switch the active kid and deploy again. Old data still verifies under the old kid.
4. After the longest-lived thing signed under the old kid has expired (token TTL, proof lifetime, rows referencing it), remove or retire the old kid.
5. Verify without printing the secret (kid lists, row counts, a login, a governed write). Record kid names, not values, in the ticket.

## 3. Suspected exposure

Treat as a security incident (`operational-runbooks.md` section 9). Emergency order: contain (suspend affected accounts, engage the payment
kill switch if money could move: `kill-switch-and-containment.md`), rotate the exposed family WITHOUT waiting for the overlap window
(retire the old kid immediately; in-flight work fails closed and is retried), then the families protected next to it (backup exposure list:
`backup-restore-dr.md` section 5a), revoke sessions, review audit for use of the exposed material. Do not paste a secret anywhere to "check" it.

## 4. Open items

- A single rotation calendar/owner does not exist (HUMAN: who rotates, how often). No rotation is automated.
- Secret-store backends in production: only `awssm` is production-eligible; its region comes from `AWS_SECRETSMANAGER_REGION` (or `AWS_REGION`) and
  `SECRETSTORE_BACKENDS` must name it; real-AWS rotation and outage drills are `STAGING REQUIRED` (ADR 0093 section 8).
- Actor proof nonce pruning is a manual weekly owner-role job (no job exists).
