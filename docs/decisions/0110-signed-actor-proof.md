# ADR 0110 — Signed actor proof for governed four-eyes writes (PRH-2 R5, SIGNED-ACTOR-PROOF)

- **Pointer (2026-10-08, ADR 0111, PROPOSED):** ADR 0111 §6.3 extends the protected-table list from nine to eleven (`withdrawal_hold_resolutions`, `withdrawal_hold_resolution_approvals`, platform_acting with a tenant only), §4.4 appends `evidence_line_id` to the K3 INSERT digest (M1/M2 digests unchanged), and records new §10a threats: **T9** payout-instrument writes are not actor-proof bound (mitigated by Go-side seals and the gate rule, ADR 0111 §2.2); **T10** forgeable statement lines used as M4 evidence (mitigated by system-shape-only import INSERT plus a Go import seal, ADR 0111 §4.3; launch-blocking for non-MOCK M4 unless sealed imports are implemented or the human accepts T10); **T11** SQL-planted `pending_verification` instrument rows (mitigated by the instrument seal checked by the verifier). Revision 2 also restricts the four `withdrawal_hold_resolution:*` operations to `platform_acting` scope with a tenant, and all K3 INSERT digests gain a trailing field (`~` for M1/M2). Not implemented.

- **Status:** IMPLEMENTED on branch `prh2-r5-signed-actor-proof` (migration `0120_signed_actor_proof`, amended in
  place - it is unmerged); not merged, not pushed. First review round (security, ledger-finance, code-reviewer):
  APPROVE WITH CONDITIONS; the conditions are applied (section 13). A further review of the amended migration is
  REQUIRED before merge. **THREAT-MODEL-ARBITRARY-SQL-1 is PARTIALLY MITIGATED, not closed** (section 9): it remains open because of
  T3/H1 (identity-store routes), T4 (`player_credential_tokens`), T5 (non-governed posting) and T8 (other dual-control
  flows without a proof), plus the inherent T6, T7 and the section 9.5 nonce/wire residual.
- **Decision type:** database authorization hardening for an owner-decided threat model. No new service boundary, no
  redesign of the DB authorization architecture, no change to the ledger account model, to the four-eyes rules or to
  tenant isolation.
- **Owner decisions:** `THREAT-MODEL-ARBITRARY-SQL-1 = YES` (arbitrary SQL with a compromised or stolen
  `igaming_runtime` credential is INSIDE the production threat model for DB-enforced controls) and
  `SIGNED-ACTOR-PROOF = AUTHORIZED` (implement ONLY the smallest mechanism already proposed by the security design),
  recorded in `docs/governance/task-registry.md` rows `DECISIONS-PRH2-CLEARANCE-2026-10-05` and
  `DECISIONS-PRH2-FINAL-GAMEPLAY-SECURITY-2026-10-06`.
- **Related:** ADR 0099 (scoped financial capability grants, `financial_actor_session()`), ADR 0100 (K2 manual
  adjustments), ADR 0101 (K3 payment force-resolution), ADR 0108 (TEMP revoke),
  `docs/plans/prh2-hardening-round/analysis/null-arm-write-1-security-analysis-2026-10-05.md`.

## 1. Problem

The four-eyes actor identity is derived only from transaction-local GUCs (`app.tenant_id`, `app.principal_id`,
`app.platform_admin_principal_id`, `app.acting_tenant_id`, `app.acting_platform_principal_id`) by
`financial_actor_session()` and `financial_acting_session_valid()` (migrations 0112). Any session of the runtime role
can `set_config` those GUCs. The functions only check that the named principal EXISTS as an active `staff_users` row;
nothing proves the caller IS that person. With arbitrary SQL an attacker can therefore submit and approve a K2 ledger
adjustment, or a K3 manual payment resolution, as two different real admins (the guards in 0113 and 0115 then see
two distinct people).

## 2. Decision

Candidate (A) of the security design: a **signed actor proof verified inside the database**.

1. The application server, AFTER it has authenticated the principal (verified token subject) and authorised the
   governed action server-side, signs a short-lived proof with HMAC-SHA256 under a key the runtime role can never read.
2. The proof travels in the transaction-local GUC `app.actor_proof`.
3. An OWNER-owned `SECURITY DEFINER` verifier, `actor_proof_require(...)`, recomputes the MAC from an OWNER-ONLY key
   table, checks the time window, checks the binding against the actor the session GUCs resolve to AND against the
   operation / target / payload of the row being written, and consumes the nonce in an OWNER-ONLY table under a
   `UNIQUE` constraint.
4. A last-firing `BEFORE` trigger `zz_actor_proof_guard` on the four governed tables calls the verifier. No valid
   proof, no write: the database fails closed.

HMAC-SHA256 was chosen (rather than Ed25519) because PostgreSQL can verify it with the `pgcrypto` extension the repo
already uses (migration 0001, `public.hmac`); no new extension. The cost of a symmetric scheme is stated in section 9
(the database owner role also holds the verification secret).

## 3. What is protected (exactly)

Two scopes of coverage, one mechanism. The six K2/K3 actor-identity guards, and (review finding H2, owner-authorized
extension of the SAME mechanism) the five K1 capability-grant / financial-policy-change tables.

| Table | Operation | Proof operation | Proof target | Proof payload hash |
|---|---|---|---|---|
| `ledger_adjustment_requests` | INSERT | `ledger_adjustment:initiate` | the request id (chosen by Go) | digest of the caller-supplied columns |
| `ledger_adjustment_requests` | UPDATE pending -> cancelled | `ledger_adjustment:cancel` | request id | the request's `payload_hash` |
| `ledger_adjustment_approvals` | INSERT | `ledger_adjustment:approve` / `:reject` | request id | the approval's `payload_hash` |
| `payment_manual_resolutions` | INSERT | `payment_force_resolve:request` | the literal `new` (the id is server-forced by the 0115 guard) | digest of the caller-supplied columns |
| `payment_manual_resolutions` | UPDATE pending -> cancelled | `payment_force_resolve:cancel` | resolution id | the resolution's `payload_hash` |
| `payment_manual_resolution_approvals` | INSERT | `payment_force_resolve:approve` / `:reject` | resolution id | the approval's `payload_hash` |

| `staff_capability_grant_requests` | INSERT | `capability_grant:request` | the literal `new` | digest of tenant, grantee, capability, valid_from, valid_until, reason |
| `staff_capability_grant_requests` | UPDATE pending -> cancelled | `capability_grant:cancel` | request id | the request content digest; **initiator only** (CG010 otherwise) |
| `staff_capability_grant_requests` | UPDATE pending -> expired | none | | refused (CG010) unless `now() >= expires_at` (closes the 0112 branch that allowed early expiry) |
| `staff_capability_grant_approvals` | INSERT | `capability_grant:approve` / `:reject` | request id | the request content digest |
| `staff_capability_grants` | UPDATE (revoke) | `capability_grant:revoke` | grant id | digest of grant id, tenant, reason; the grant INSERT is bound through its same-transaction approval |
| `financial_approval_policy_changes` | INSERT | `financial_policy_change:propose` | the literal `new` | digest of the content-hash inputs (all columns including `effective_from`) |
| `financial_approval_policy_changes` | UPDATE pending -> cancelled | `financial_policy_change:cancel` | change id | the change's `content_hash` |
| `financial_approval_policy_change_approvals` | INSERT | `financial_policy_change:approve` / `:reject` | change id | the approver-supplied `content_hash` |

In every row the trigger also asserts that the actor column the earlier guard FORCED (`initiated_by`, `decided_by`,
`requested_by`, `revoked_by`) equals the PROVEN actor (`AP004` otherwise), and a catalog test pins that
`zz_actor_proof_guard` is the LAST BEFORE ROW trigger on all nine tables.

K1 and policy flows run in the plain `platform` scope with NO tenant. That is encoded as an EMPTY tenant field on
the wire; the verifier accepts it ONLY for `scope = platform` AND only for the `capability_grant:` and
`financial_policy_change:` operations, and refuses a tenant on a platform proof, a missing tenant on a `tenant` or
`platform_acting` proof, and `platform` for any K2/K3 operation. The tenant / platform_acting checks are unchanged.

The K2/K3 tables carry the six actor-identity guards of 0113/0115 (`ledger_adjustment_requests_guard`,
`ledger_adjustment_requests_beneficiary_guard`, `ledger_adjustment_approvals_guard`,
`ledger_adjustment_approvals_beneficiary_guard`, `payment_manual_resolutions_guard`,
`payment_manual_resolution_approvals_guard`; the 0115 beneficiary guard runs on both K3 tables). The proof check is a
separate trigger, named so that it fires LAST among the BEFORE triggers: every existing guard keeps its own
refusal and error code, and the proof is the final gate. The submission digest is
`SHA-256(k2_canonical(tenant, wallet|attempt, ...caller-supplied columns))`; the Go issuer
(`internal/actorproof.Digest`) computes the same value from the same inputs before the INSERT.

The execution of an approved K2/K3 request (the `executing` / `executed` transitions) is not actor-bound: it is bound by the
existing guards to a same-transaction approval, which now carries a proof.

## 4. Proof format and verification

```
v1|kid|actor|scope|tenant|operation|target|payload_hash|iat|exp|nonce|mac
mac = lower-hex HMAC-SHA256(secret(kid), the first eleven fields joined by '|')
```

The verifier rejects, with its own SQLSTATE (class `AP`, all fail closed):

| Code | Meaning |
|---|---|
| `AP001` | no proof, or malformed (field count, number/uuid/hex shapes) |
| `AP002` | unknown or retired `kid`, or bad MAC |
| `AP003` | expired, issued in the future (more than 5 s skew), or lifetime over 60 s |
| `AP004` | binding mismatch: actor, scope (`tenant` / `platform_acting` / `platform`), tenant, operation, target or payload hash; or the forced actor column differs from the proven actor |
| `AP005` | nonce already consumed (replay) |

The Go issuer uses a 30 s lifetime. The MAC comparison is done on HMACs of both values under the same key (no
byte-wise early exit on attacker-controlled input). The verifier returns `void`; it mints nothing and no
`actor_proof*` function returns a token (asserted by a test).

## 5. How legitimate retries stay idempotent

- A proof is issued per governed write, inside the transaction, after the server has read the request/resolution it
  is about to decide on. Every attempt - including a client retry of the SAME logical approval - gets a **fresh
  nonce**. A retry is never a replay of an earlier proof.
- The nonce row is inserted in the SAME transaction as the governed write. If any later step fails (an existing
  guard, the execution, the ledger posting), the whole transaction rolls back and the nonce is NOT burned; the
  legitimate retry succeeds. Once the write commits, the nonce is committed with it and can never be accepted again.
- The idempotency of the business operation is unchanged and is still owned by the existing mechanisms: the
  approval row's own guards (`this Person already decided`, `request is not pending`), the request-state machine
  and the governed ledger idempotency key `manual_adjustment:<request id>` /
  `<provider>:<reserved tx id>`. A retried approval of an already-executed request is refused by those guards, with
  a fresh proof, and posts nothing (tests `..._IdempotentRetry`, `..._RealServicePath`).
- Because the existing guards fire first, a refused retry consumes no nonce at all.
- The `UNIQUE` nonce key serialises two concurrent consumers of the same proof: the second blocks until the first
  commits or rolls back, then is refused (`AP005`) or succeeds respectively.

## 6. Keys

- **Where they live.** The signing key is configuration: `ACTOR_PROOF_KEYS` (`kid:base64(secret)`, comma-separated,
  each secret at least 32 bytes decoded) and `ACTOR_PROOF_ACTIVE_KID`, supplied through the deployment secret store
  as environment variables. Never committed, never rendered (`config.SecretValue`, and `actorproof.Issuer` prints
  `redacted`). `config.Load` refuses malformed entries, short keys, an unknown active kid and a key equal to a JWT
  secret. The verification copy lives in the OWNER-ONLY table `actor_proof_keys`.
- **Where they must NOT live.** Nothing the runtime role can read. `actor_proof_keys` and `actor_proof_nonces` are
  owned by the migration role, `REVOKE ALL FROM PUBLIC` and from `igaming_runtime`, `ENABLE ROW LEVEL SECURITY`
  with NO policy (a second, independent denial should a default privilege ever re-grant them). They are
  deliberately not `FORCE ROW LEVEL SECURITY`, because the definer verifier runs as the owner. A test asserts the
  runtime role cannot SELECT, INSERT, UPDATE, DELETE or TRUNCATE either table and holds no ACL on them.
- **SECURITY DEFINER hardening.** `search_path = pg_catalog, public, pg_temp`; every table reference is
  schema-qualified; `pgcrypto`'s function is called as `public.hmac`; builtins are `pg_catalog.`-qualified; the
  runtime role cannot `CREATE` in `public` (PostgreSQL 15+) and cannot create TEMP objects (0116). `EXECUTE` is revoked
  from `PUBLIC` and granted to `igaming_runtime` only (when that role exists at migration time; the two init SQL
  scripts re-grant it for databases provisioned afterwards).
- **Production startup gate.** `actorproof.VerifyConfiguredInProduction` (called from `cmd/platform-api/main.go`
  after `db.VerifyRuntimeRoleInProduction`, with `cfg.GuardEnvironment()`) refuses to start in production unless an
  issuer is configured AND the database holds the configured active kid as an ACTIVE key (probe
  `actor_proof_key_active`, which discloses no key material). Outside production an absent key leaves governed writes
  failing closed at the database.
- **Dev/CI.** No key is committed. Integration tests generate a random key per test process
  (`internal/actorproof/prooftest`), provision it into the test database as the owner role, and install the matching
  issuer as the process default. The CI environment needs no `ACTOR_PROOF_*` variable.
- **Rotation.** Supported by `kid` and any number of ACTIVE keys: (1) generate a new 32+ byte secret in the secret
  store; (2) as the migration/owner role, `INSERT` it into `actor_proof_keys` as `active` (both kids now verify);
  (3) deploy the application with the new key in `ACTOR_PROOF_KEYS` and `ACTOR_PROOF_ACTIVE_KID` set to the new kid
  (keep the old key listed for the deploy window); (4) after one proof lifetime (60 s) plus the deploy window, mark
  the old kid `retired` (a retired kid is refused with `AP002`); (5) remove the old key from the application
  configuration; (6) optionally prune consumed nonces older than a day. The full runbook is
  `docs/runbooks/operational-runbooks.md` section 15.

## 7. Single server-side issuer

`internal/actorproof.Issuer` is the only signer. It is invoked only from the packages that own four-eyes and K1
(`internal/adjustment`, `internal/payments` from their `runSession` call sites whose principal is parsed from the
verified token subject, and `internal/capability` for the K1 routes, which sign for the principal the HTTP handler
put into the context with `capability.WithProofActor` after authentication and the permission check) and wired
once at startup (`cmd/platform-api`). A static test (`TestStatic_OnlyFourEyesPackagesSign`, with a planted-offender
non-vacuity test) fails if any other non-test package imports `internal/actorproof`, if ANY non-test package other than
`prooftest` itself imports `internal/actorproof/prooftest`, or if anything but `internal/capability` / `prooftest`
mentions `TestSessionProofHook`.

`prooftest` is behind the `integration` build tag, so no production binary can contain it. Its `AttachForSession`
signs for whoever the session GUCs name - exactly the capability this control denies an attacker - which is why it
must stay unreachable from production code. `capability.TestSessionProofHook` is nil in production; only `prooftest`
sets it so test fixtures that open their own sessions can sign; an HTTP integration test switches it off to prove
the handler signs for the authenticated principal.

Any signing failure - a missing issuer or a claim set the database could never admit (for example a `platform` scope
on a K2 operation) - is RETURNED to the caller (HTTP 500), never silently skipped, so a defect is never reported as an
authorization refusal (403).

## 8. NULL-ARM-WRITE-1 reassessed under this mitigation

Question: does requiring the proof close the chain "create a platform_admin (or tenant staff) through the NULL-tenant /
tenant arm of `staff_users`, then approve"?

**Closed by this change (tested, `TestActorProof_K2_NullArmMintedPrincipals_CannotSubmitOrApprove`,
`TestActorProofK1_Adversarial_*`):** the in-database chain. A runtime session can still mint persons, platform_admin
principals (NULL arm) and tenant finance staff (tenant arm), and can set GUCs naming them; the guards still recognise
them as eligible actors. But a submit or an approve by such a principal now needs a proof signed by the application
server. The attacker does not hold the key, so the write is refused (`AP001` without a proof, `AP002` with a proof
under a wrong key), and no approval row and no ledger posting results. The same holds when the attacker impersonates
a REAL admin by GUC. With H2 the K1 grant and policy-change flows are covered too, so the minted or impersonated
principals can neither obtain the capability grants nor lower the required approvals that a later K2/K3 approval
would depend on. "Arbitrary SQL alone is enough to approve as two people, or to grant itself the capability to do
so" is closed for K1, K2, K3 and policy changes.

**NOT closed (precisely):** the application issues a proof for whoever it authenticated. If the attacker can make the
application authenticate them as a principal the database believes in, they obtain genuine server-issued proofs.
That is an identity-store integrity problem, not an authorization-guard problem, and the NULL-arm finding gives the
attacker the means:

1. mint a `staff_users` row with an attacker-chosen `password_hash` (NULL arm or tenant arm), then log in over HTTP;
2. take over a real staff row (`UPDATE password_hash`), then log in as it;
3. forge a NULL-tenant `sessions` row (refresh token hash) for a real platform admin, and call `/refresh`
   (NULL-ARM-WRITE-1 item 3; RLS cannot help, session issuance is indistinguishable in SQL).

A test pins the boundary (`TestActorProof_Residual_ServerIssuedProofForMintedPrincipalIsAccepted`): a server-issued
proof for a minted principal is accepted, as it would be for a real one.

**Is a tiny additional change REQUIRED to avoid leaving a known DB-enforced authorization bypass? No.** The
candidates were considered:

- "the guards require the proof's actor to be a staff_users row that existed BEFORE the transaction": does not help;
  the attacker mints in one transaction and authenticates over HTTP in a later one.
- a restrictive `staff_users` NULL-arm INSERT policy (analysis section 7 A/B): the analysis showed it cannot stop an
  attacker who sets `app.platform_admin_principal_id` to any uuid (presence-only), and the application legitimately
  creates tenant staff through `POST` admin routes (`identity.CreateStaffUser`, `admin_routes.go`), so it would need its
  own proof binding plus changes to routes and tests.
- an owner-only lifecycle trigger on `staff_users` (analysis section 7 C): would close items 1 and 2 but NOT item 3
  (forged sessions), so the class would stay open; and it needs a bootstrap path (`cmd/seed-admin`) and the tenant-staff
  creation route reworked. That is larger than a contained change and is the identity-store redesign the owner
  decision does not authorize.

Adding any of them here would leave the same residual and widen the change. They are recorded as deferred
considerations (section 10), not built. **This is a residual that REMAINS OPEN and must not be reported as closed.**

## 9. Residuals (stated honestly)

1. **Application-host compromise.** An attacker with code execution on the application host holds the signing key and
   can sign any proof. The proof defends the database against a stolen database credential, not against a stolen
   application.
2. **Identity-store integrity** (section 8): minted staff rows, taken-over credentials and forged refresh sessions give
   an attacker legitimately issued proofs. Open until the identity store is protected.
3. **Ordinary non-governed posting paths are not protected by this change.** Any path whose authority is the GUC alone
   (deposits, withdrawals, casino/sportsbook postings, the provider callbacks, and every table other than the nine
   above) is unchanged. A runtime session can still post to the ledger by those ordinary paths. This is a
   threat-register item (below), outside the governed four-eyes scope this ADR defends.
4. **Symmetric key in the database.** The migration/owner role (and a superuser) can read `actor_proof_keys` and so
   can mint a proof. The migration role is already outside the threat model (it owns every table and can disable any
   trigger). An asymmetric scheme would remove it but PostgreSQL has no built-in Ed25519 verifier; a new extension was
   explicitly avoided.
5. **Nonce durability.** A nonce is committed only with the governed write. A proof captured on the wire before use
   can be used once within its 30 s lifetime for exactly the write it names (never for a different actor, operation,
   target, payload or tenant). The wire is the application's database connection (TLS in production).
6. **Reads and cancellation of unproven principals.** The proof binds writes; reads of K2/K3 data under impersonated
   GUCs are unchanged (tenant isolation is RLS's job).
7. **Clock.** The verifier uses the database clock; the issuer uses the application clock. The 5 s future skew and the
   60 s lifetime cap bound the effect of drift; a skewed host fails closed (`AP003`), never open.

## 10. Deferred (recorded, NOT built)

**DEFERRED-REQUIRES-OWNER-AUTHORIZATION (identity-store integrity, review finding H1).** These are the routes that
leave THREAT-MODEL-ARBITRARY-SQL-1 only PARTIALLY MITIGATED. Each changes how identities or credentials are stored or
verified, which is larger than the contained mechanism the owner authorized:
- keyed staff credential verification (so a `password_hash` written by SQL does not authenticate);
- an owner-only lifecycle (or integrity tag) for `staff_users` platform rows and credential columns, with a
  bootstrap path for `cmd/seed-admin` (NULL-ARM-WRITE-1 analysis item C);
- HMAC'd refresh tokens (`HMAC(server_key, token)` instead of a bare SHA-256) so a `sessions` row inserted by SQL
  cannot match a token the application accepts (NULL-ARM-WRITE-1 item 3); invalidates every refresh token and needs a
  migration plan;
- the same keyed treatment of `player_credential_tokens` (email-verification / password-reset token hashes), see the
  threat register.

Deferred without an owner gate:
- An asymmetric proof if PostgreSQL gains a verifier or a vetted extension is approved.
- Nonce pruning: planned, documented, NOT automated (runbook section 15 step 6; rows are small; a 1-day retention
  past `expires_at` is ample because an expired proof is refused on its own).

## 10a. Threat register (arbitrary SQL with a stolen `igaming_runtime` credential)

| # | Item | State |
|---|---|---|
| T1 | Impersonate one or two REAL admins by GUC to approve a K2/K3 request, a K1 grant, or a policy change | CLOSED by this ADR (proof binds actor, scope, tenant, operation, target, payload) |
| T2 | Mint or impersonate staff by SQL and use them as actors in the database | CLOSED for K1/K2/K3/policy (no proof) |
| T3 | Mint or take over a staff row, or forge a refresh `sessions` row, then authenticate over HTTP and be issued genuine proofs | OPEN, DEFERRED-REQUIRES-OWNER-AUTHORIZATION (H1) |
| T4 | `player_credential_tokens` store an UNKEYED hash of the email-verification / password-reset token; a row inserted by SQL can be redeemed through the application (account takeover of a player) | OPEN, DEFERRED-REQUIRES-OWNER-AUTHORIZATION (same keyed-hash fix as T3); not a governed-financial path |
| T5 | Ordinary NON-governed posting paths: any posting whose authority is the GUC alone (deposits, withdrawals, casino and sportsbook postings, provider callbacks) can be written by a runtime session | OPEN, NOT in the scope of the authorized mechanism; limited by the ledger invariants (balanced, append-only, idempotent, drift reconciliation hourly) but NOT by an actor proof |
| T8 | Other dual-control governance flows OUTSIDE the nine proof-bound tables still take the actor identity from GUCs or app-supplied values with NO proof: payment kill-switch release (0105), provider credential handles (0096), KYC enforcement policy (0103), asset-registry dual control (0047), withdrawal approvals and policies (0026/0034), bonus change governance (0063), casino catalogue dual control (0086/0089). A runtime session impersonating two admins can still approve in those flows | NOT mitigated, DEFERRED-REQUIRES-OWNER-AUTHORIZATION (not implemented here) |
| T6 | Application-host compromise holds the signing key | OPEN, inherent (section 9.1) |
| T7 | Owner / migration role can read the symmetric key | OPEN, accepted (section 9.4) |

## 11. Review points (specific)

For **security**:
1. The verifier's definer hardening: `search_path`, schema qualification, `EXECUTE` grants, the table ACL and the
   no-policy RLS posture, and that no `actor_proof*` function can be coerced to return key material or a token.
2. Binding completeness: actor, scope, tenant, operation, target, payload, iat/exp, nonce; the 60 s cap; the 5 s skew.
3. `zz_actor_proof_guard` ordering (last) and that nothing between the earlier guards and the trigger can alter the
   bound columns (the digest columns are not modified by the 0113/0115 guards).
4. The residuals in sections 8, 9 and 10a. K1 and policy-change guards are now covered (H2); the open items are the
   identity-store routes (T3, T4) and non-governed posting (T5), which need owner authorization.
5. The pinned `search_path` of the trigger functions also changes how the unpinned `financial_actor_session()` resolves
   relations during the proof check (defence in depth against a re-granted TEMP; see test
   `TestTempRevoke_K2ShadowAttack_SucceedsWhenTempIsGrantedBack`).

For **ledger-finance**:
1. No financial semantic changed: the proof is an additional precondition on writes the guards already admit; every
   existing refusal code is unchanged because the proof trigger fires last.
2. Idempotency (section 5): fresh proof per attempt, nonce rolls back with the write, the business idempotency keys
   are untouched; confirm no path can double-execute on a retry.
3. The cancel paths are now proof-bound (initiator-only cancel was GUC-only).
4. Positive tests assert exactly one posting, `SUM(debits) = SUM(credits)` and projection = rebuild.

## 12. Verification

See `docs/plans/prh2-hardening-round/prh2-r5-signed-actor-proof-mutation-kill.txt` for the mutation-kill evidence, and
the suites named in section 5 and section 8. Labels: the mechanism and its tests are `IMPLEMENTED`; the identity-store routes (T3, T4) and non-governed posting (T5) are `NOT IMPLEMENTED` (section 10, 10a); the production
rollout (key provisioning in the real secret store, deploy ordering) is `PROVIDER DEPENDENT` on the operator and has
not been exercised outside the local database.

## 13. Review conditions applied (first review round, amended 0120)

M1/C1 `prooftest` behind the `integration` tag and refused for any non-test importer (static test with planted
non-vacuity); L1 forced actor columns asserted equal to the proven actor in every zz function; L2/R4 catalog test that
`zz_actor_proof_guard` is the last BEFORE ROW trigger on all nine tables; LF R1 signing failures are returned (500),
never swallowed; H2 the same mechanism extended to the five K1/policy tables, plus the K1 cancel (initiator only) and
expiry (only when actually expired) branches of migration 0112; mutation evidence corrected and extended; L3/L4
runbook changes (secret not inlined as a loggable SQL literal, `log_statement` check, `--single-transaction` for the
ECS role-init, nonce pruning plan); the "mechanical" claim about extending the proof is withdrawn: it needed a
NULL-tenant wire encoding, per-flow digests and Go issuance in the K1/policy services.
