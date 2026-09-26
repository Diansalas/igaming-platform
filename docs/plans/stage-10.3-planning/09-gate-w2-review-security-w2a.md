# Stage 10.3 gate W2 — W2a code review (`security`)

- **Reviewed:** W2a as merged at `3ef18f2` (`git diff 6e3d74c..3ef18f2`), read from the committed tree
  (`git archive 3ef18f2`), not the working tree (a W2b/W3a merge is in progress there).
- **Binding spec:** ADR 0093 with its W2a design-review amendment and its "W2a implementation status"
  section; my design review `07-w2a-design-review-security.md` §1–§9; ADR 0022 §3 Stage 10.3
  amendments (point 2 overlap/C4, point 9, the `KeyImplicit` lift); CLAUDE.md Security rules.
- **Checks run by me:**
  - `go vet ./...` is clean.
  - `go test -count=1` passes on `internal/{secretstore,providercred,webhookauth,config,auth,apierror,providers,kyc,payments,casino,httpserver}/...` and `cmd/platform-api/...`.
  - Go 1.25.0.
  - Integration tests were **not** run, per the task constraint. For DB-level behaviour I rely on reading the SQL, on the presence and content of the named tests, and on `evidence/w2a-mutation-kill.txt`.
- **Type:** read-only code review. Only this file was written. Nothing committed.

## Verdict: **APPROVE WITH CONDITIONS**

The W2a implementation meets my §1–§6 in substance. I found no finding that lets a secret leak, lets a
tenant boundary be crossed, or lets a key be activated without two Persons. There are two conditions,
both governance/observability, not exploitable today. PROV-OUTBOUND-CRED-1 remains a **launch-blocking**
precondition for any real outbound adapter (see §3).

| # | Severity | Condition | Deadline |
|---|---|---|---|
| **W2A-SEC-1** | Medium (governance; launch-blocking for real adapters) | The PROV-OUTBOUND-CRED-1 "precondition" is **not registered** at `3ef18f2`. `docs/governance/task-registry.md:3883` still reads "Approved — W2a pending"; the only record is the ADR 0093 status prose (`0093-…md:558-571`). Update the registry row to `PARTIALLY IMPLEMENTED`. Add an explicit precondition: "no non-synthetic payments/casino/KYC adapter may be registered, or make an outbound call, until outbound calls run outside any domain DB transaction (review §2 precision 1). Owners: `architect` + `ledger-finance` + the domain specialists. Launch-blocking." | Before gate W2 is recorded as passed |
| **W2A-SEC-2** | Low | C4 / ADR 0022 §3 point 2 ("The log records which `key_id` verified") is unmet. `VerifyInbound` returns the matched credential (`internal/webhookauth/scheme.go:682-728`), but none of `kyc/webhook_verify.go:61`, `casino/webhook_verify.go:65` or `payments/webhook_verify.go:69` logs it. Add one success log line with `key_id` and `matched_predecessor` (bool) in each domain. It must never carry secret, fingerprint input or signature. Add a log-capture test. | Before any `KeyImplicit` scheme is registered. Register it now as a precondition (ADR 0093 already labels it `NOT IMPLEMENTED`, `0093-…md:572`). Doing it in W2 is recommended: it is about 3 lines per domain. |

Everything else below is verified, or is an observation that needs no action.

---

## 1. Tables, policies, triggers (§1) — verified

File: `migrations/0096_provider_credential_handles.up.sql` (line numbers at `3ef18f2`).

| Item | Result | Where |
|---|---|---|
| B1: bridge is `FOR SELECT` on both governance tables and `FOR UPDATE` on requests only. There is no `FOR ALL`/`INSERT`/`DELETE` for tenant scope. | ✔ | 357-378 |
| B2: the player-unset clause is on every bridge policy, every handle policy and both `platform_admin_scope` policies. | ✔ | 313-352, 357-378 |
| B3: consume selects by `NEW.activation_request_id`, `FOR UPDATE`. There is no content search. | ✔ | 646-653 |
| B4: `applied` requires a same-tenant handle pointing back. This is checked in the request's own BEFORE UPDATE. Platform scope cannot see handles, so it gets PC034. | ✔ | 482-492 |
| B5: `requested_at`, `state`, `applied_*` and `content_hash` are forced on request insert. `decided_at` is forced on approval insert. `created_at` and `status_changed_at` are forced on handle insert. `status_changed_at`, `applied_at` and `revoked_at` are forced on update. | ✔ | 425-433, 587, 621-622, 824-828, 482, 846 |
| B6: consume never reads `staff_users`. | ✔ | 639-768 |
| B7: there is no `SECURITY DEFINER` anywhere in the migration. `TestPCGovernance_NoSecurityDefiner` is present. | ✔ | whole file |
| Handle table: `ENABLE` and `FORCE` RLS, three tenant policies, no DELETE/platform/dual-scope policy, tenant FK without cascade. | ✔ | 212, 304-326 |
| Request/approval shapes, table CHECKs, `UNIQUE (request_id, approver_principal_id)`, reject needs a reason. | ✔ | 143-206 |
| Namespace function: splits on segments, never builds a regex from column values, `/provider-creds/` exactly once (an overlapping second occurrence is also refused), 4 segments, name regex refuses `.` and `..`. | ✔ | 72-95 |
| Pinned versions: `awssm` needs `versionId` in every env and refuses stage labels. `devfile` needs `version`. | ✔ (stricter, see §5) | 174-177, 241-244 |
| Global fingerprint unique across all tenants and statuses. One active and one verify_only per binding. | ✔ | 260, 265-268 |
| Down migration: `CHECK (false)` probes (FORCE-RLS-proof) on all three tables before any drop. | ✔ | `…down.sql` |
| Runtime grants are minimal. `activation_request_id` is not updatable. Approvals are SELECT/INSERT only. Mirrored and re-asserted in `deploy/init-app-role.sql` (tail block). | ✔ | 881-897 |

**Trigger order.**
- Request insert (386-436) runs in this order:
  1. the 0089 principal body (FOUND first, then platform-scoped, person-linked, active);
  2. PC031, the session principal;
  3. the forcing;
  4. the hash.
- Approval insert (514-590) follows the 0089 self-approval body: distinct principal, then the approver resolved, platform-scoped, person-linked and active, then the requester person-linked, then a distinct Person. After that come pending (PC045), hash equality (PC046) and the `decided_at` forcing.
- Consume (639-768) runs steps 1–8 exactly as §1.4. Step 5 uses `decided_at <= now()` and `> now() - interval '24 hours'` (694-705). Step 7 uses the `status_changed_at = now()` same-transaction demotion test (739-746).
- Transition (777-861) checks revoked-terminal first, then immutable columns (including `activation_request_id`, `created_*` and `tenant_id`), then the five allowed pairs. No pair leads to `active`. Then come the shrink-only `not_after` rule with `>= now()`, the 7-day cap from the forced `status_changed_at`, and the revoke enum with the forced `revoked_at`.

**Nothing can burn an approval.** I checked every path that could move a request to `applied` or make it unusable:
- **Platform-scope UPDATE** is refused by B4, because handles are invisible in platform scope.
- **Tenant-scope direct UPDATE** is refused by B4. A handle pointing at `id` can exist only through consume, which itself sets `applied` in the same statement. `activation_request_id` is UNIQUE and immutable.
- **A mismatched approval** is refused at insert: PC046 for a hash mismatch, PC045 when the request is not pending, and the UNIQUE constraint for a duplicate approver.
- **A requester's self-"reject"** is refused by PC041 before the decision is considered.
- **A reject by a different eligible Person** does block the request (step 6). That is the intended four-eyes veto, not a burn.
- **Concurrent applies:** the loser's consume `SELECT … FOR UPDATE` no longer matches `tenant_consume_apply`'s `state = 'pending'` USING, so it gets PC001 and rolls back. This is covered by `TestPCConsume_ConcurrentApplyExactlyOneWins`.

**Content hash** (113-136):
- It is length-prefixed, with NULL encoded as `-`.
- Timestamps are rendered with `AT TIME ZONE 'UTC'` at microsecond resolution, and UUIDs in canonical text.
- It is SHA-256 hex, computed only in SQL. Go never computes it; the approver echoes the hash back.
- Observation O-4 is below.

**Expiry and single use.** The 24 h expiry is a SQL constant. Single use rests on three things: the `state` check, the UNIQUE `activation_request_id`, and the `applied` forcing. Both properties are killed by mutants M1 and M6/M7 and by `TestPCConsume_ExpiryBoundary` and `TestPCConsume_SingleUse`.

**Every required §1.6 test name is present,** with one exception covered by an equivalent (§4). The
seven required mutants M1–M7 are recorded KILLED in `evidence/w2a-mutation-kill.txt` (24/24).

## 2. Resolver, cache, breaker, backends (§4, §5) — verified

- **Revocation is immediate in every breaker state.**
  - `Resolve` always runs `HandleReadSQL` (`internal/providercred/resolver.go:24-30`) before any `Fetch` (`resolver.go:114`, `:149`, `:160`). The window is judged by the DB clock and the status filter excludes `revoked`.
  - The Fetcher is reached only for rows the read returned, so no breaker or cache state can serve a revoked handle.
  - Covered by `TestResolver_RevokeImmediateWhileBreakerOpen` and mutant M23.
- **Fingerprint on every cache hit.**
  - `secretstore/fetcher.go:177-184` compares in constant time (`hmac.Equal`, `:378-381`) before either the fresh or the stale path. A mismatch removes the entry and raises the P1 alert.
  - A freshly fetched value is checked the same way (`:266-273`), and followers get only a checked value.
  - Mutant M20 is killed.
- **Negative cache.**
  - It is keyed on (tenant, ref, fingerprint): 5 s for counting failures and 30 s for per-ref failures, including `store_config` and integrity (`fetcher.go:39-43`, `:258-272`).
  - Stale is served only behind a *counting* failure (`:199-201`, `:362-365`).
  - Per-ref classes never trip the breaker (`secretstore.go:136`).
- **Constants** equal my §5 literals (`fetcher.go:16-53`) and are pinned by test (`96fc567`):
  - 2 s with 1 retry;
  - 4 slots and 250 ms wait;
  - trip at 3;
  - 15 s cooldown doubling to a 60 s cap, one half-open probe;
  - LRU of 1024, 10 min refresh-not-evict, 60 min max-stale.
- **Singleflight.** It is keyed per (tenant, ref, fingerprint), with a context detached from the caller but bounded to 2 s (`:249`).
- **C5.** Any handle-read DB error, and any wrong row count, is `credential_unavailable`, the same reason as "no handle" (`resolver.go:115-146`). A `memory://` row outside tests is `ClassNoBackend` → `credential_unavailable` (`:222-225`).
- **KeyImplicit.** The resolver accepts only exactly one `active` row plus at most one `verify_only` row, and that row must have a `not_after` (`:126-146`). `ResolveCredentials` then enforces the binding, a distinct key id and a non-zero `NotAfter`. `VerifyInbound` drops an expired predecessor.
- **devfile** (`internal/secretstore/devfile/devfile.go`):
  - `New` calls `cfg.ValidateSecretBackendScheme` itself (`:65-70`), and the backend is Unix-only.
  - The root must not be a symlink, must be a directory owned by the euid, and must have no group or other bits.
  - All access goes through `os.OpenRoot` with `O_NOFOLLOW`. `Lstat` runs before open and `Stat` runs after open (race closed).
  - Each file must be regular, owned by the euid, 0600 or 0400, and between 1 B and 64 KiB. The exact bytes are used, and the read buffer is zeroed.
  - Segment regexes refuse `.` and `..`.
  - `.secrets/` is in both `.gitignore` and `.dockerignore`.
- **memstore is unreachable from production.**
  - `ValidateSecretBackendScheme("memory")` always errors (`internal/config/config.go:737-738`).
  - `NewRouter` validates every backend, and `withCredentialSubsystem` constructs only `devfile`, refusing `awssm` (NOT IMPLEMENTED) and everything else (`cmd/platform-api/registrations.go` `withCredentialSubsystem`).
  - A CI grep plus `TestSecretStore_MemstoreImportedOnlyByTests` (with a negative control) enforce the import boundary.
  - An AST test restricts `NewRouterUnvalidated`.
  - `providercredtest` carries `//go:build integration` and does not import memstore.
  - memstore is `SyntheticComponent`.
- **Three-point allow-list** (§4.1):
  - startup, in `config.Load` and `NewRouter`;
  - registration, through `service.go:270-277`;
  - every resolve, through `ClassNoBackend`.
- **Fingerprint key.**
  - It is a `SecretValue`, with all five rendering paths redacted (`config.go`).
  - `Load` refuses a key under 32 B, a key equal to either JWT secret, and the published dev/CI literals outside explicit development.
  - If the key is absent, `providercred.New` returns `(nil, nil)` (`providercred.go:77-80`). The real resolvers are then a true nil and the file/decide/apply routes are not mounted.
  - The HMAC input is `label‖0x00‖secret` with no tenant (`fingerprint.go:43-49`).
- **Kind split.**
  - `KindSplitResolver` sends synthetic adapters to the mock and everything else to the real resolver (`webhookauth/resolver.go:125-151`).
  - An unregistered provider fails closed.
  - Both nil gives a true nil interface.

## 3. Rulings requested

**PROV-OUTBOUND-CRED-1 as PARTIAL: accepted, conditional on W2A-SEC-1.**
- `OutboundResolver.Resolve` (`internal/providercred/outbound.go:129-155`) meets precision 1 on its own: it uses its own `WithTenant` transaction, committed before it returns.
- Precisions 2–3 are met by `DerivedTokenCache`, keyed (tenant, handle, fingerprint), with its TTL capped at the vendor expiry.
- Precision 4 is met: mTLS is out of scope.
- The static `APIKeyEnvVar` credential is gone, and a per-call `Authenticator` is redaction-aware.
- **No production code calls `OutboundResolver`** (grep at `3ef18f2`), and every wired adapter is synthetic. The "tx held across HTTP" gap is therefore latent: no real outbound request exists to exploit it.
- Fixing it means restructuring withdrawal/deposit/launch/KYC flows, which currently hold row locks across `provider.*` calls. That is a financial-flow design decision for `architect` + `ledger-finance` + the domains, not W2a scope. I concur with the orchestrator's ruling.
- **However, the registration the handback describes does not exist at `3ef18f2`** (task-registry row 3883 is unchanged). That is W2A-SEC-1.
- **Launch-blocking flag:** any non-synthetic payments, casino or KYC adapter going live before this precondition is met would violate the review's §2 precision 1. It would also pin DB connections and row locks on vendor latency.
- Recommended, not required: a cheap tripwire test asserting that every adapter `buildProviderBundle` registers is `SyntheticComponent`. This test would have to be consciously changed when the first real adapter lands.

**Missing matched-`key_id` log: yes, it violates C4's logging clause.** C4 (`04-review-security.md:579`) requires "Both keys evaluated, and the matched `key_id` logged". ADR 0022 §3 point 2 says "The log records which `key_id` verified". The *control* parts of C4 are implemented and tested: the bounded window, shrink-only, at most one `verify_only`, and both keys evaluated in `VerifyInbound`. The forensic part is missing. That part is what tells operators whether the predecessor is still in use before they revoke it. There is no runtime exposure today because no `KeyImplicit` scheme is registered. Hence Low, W2A-SEC-2.

**Tightenings: all three approved.**
- *PC031, principal equals the session principal* (`up.sql:417-423`, `:560-564`). Nobody can file or decide on another administrator's behalf, even with raw platform-scope SQL. It is consistent with `WithPlatformAdmin(subject)` in every caller.
- *Tenant-binding composite FKs* (`up.sql:274-285`, with the `UNIQUE (id, tenant)` targets at 191 and 262). Cross-tenant activation, predecessor and applied-handle links become structurally impossible.
  - Note: the RI trigger fires before `provider_credential_handles_consume` (trigger-name order), so a cross-tenant activation now surfaces as `23503` rather than `PC003`.
  - Both are classified into the same 409, and PC003 stays as defence in depth.
- *Stricter ref regexes*: the `awssm`/`devfile` patterns are anchored as `scheme://path?…` (`up.sql:174-177`), and Go's `ParseRef` mirrors them (`secretstore.go:192-225`), with `memory` also constrained. Each is only narrower than my spec.

## 4. No secret in logs, errors, OpenAPI, audit or responses (§3, §6) — verified

- `secretstore.Error` carries only a class (`secretstore.go:140-156`).
- Backends return only classes. The devfile constructor errors contain no path.
- `providercred.Error` carries a Kind and a closed class token, never trigger or store text (`errors.go`). DB errors are classified by SQLSTATE only (`dbClass`). Anything unexpected becomes a 500 with no detail, and the log line omits the error (`provider_credential_handlers.go:155-157`).
- These types redact on `String`/`GoString`/`Format`/`LogValue`/`MarshalJSON`:
  - `Secret`, `SecretValue`, `FingerprintKey` and `OutboundCredential`;
  - `Credential`/`CredentialSet` (`webhookauth/resolver.go:153-204`);
  - `HeaderAuthenticator`.
- The DTOs are the allow-list of §6 (`provider_credential_handlers.go:163-264`), and no store result flows into them.
- The confirmation value is:
  - validated for shape only;
  - compared in constant time (`fingerprint.go` `ConfirmationMatches`);
  - never bound into SQL;
  - cleared from the body after use (`handlers.go:422`);
  - absent from every response and audit row;
  - sent to no log.
- Audit metadata is exactly the §6 list: ref, fingerprint, ids, windows, reasons, and principals (`service.go:772-808`). Failure rows carry only the generic class (`service.go:836-870`).
- OpenAPI examples are synthetic: all-zero fingerprint, nil-UUID tenant and version.
- `TestProviderCredentialAPI_ResponsesAuditsLogsNeverContainSecret` scans every response, **both** audit scopes (tenant rows and platform rows by `metadata.target_tenant_id`), and captured logs. It checks raw, hex, HEX, base64 and base64url forms of the secret, plus the confirmation and its hex. This covers my `TestRegistration_ConfirmationValueNeverPersistedOrLogged`, which is absent by that name. "Never persisted" holds structurally: no column receives it. I accept the equivalent, and no condition is attached.

## 5. Admin API (§6) — verified

- **Routes.**
  - The six routes, with their permission and scope, are exactly as in my table (`provider_credential_handlers.go:39-60`).
  - `RequireAnyPermission(:request, :approve)` guards the two request-read routes (`internal/auth/permission.go:851`).
  - File, decide and apply are not mounted when the subsystem is nil. List and transitions stay mounted, so revocation works without the key.
- **Tenant from path.** The target tenant comes from the path through `canActOnTenant`. A foreign tenant gets 403 with no data, plus a denied audit row in the caller's own scope (`:79-130`).
  - Bodies have no tenant field, and `decodeJSON` uses `DisallowUnknownFields` with `MaxBytesReader` (`json.go:11-16`).
- **Apply** runs in this order (`service.go:463-577`):
  1. a `WithPlatformAdmin` recheck of the applier, the requester and every approver (active, platform-scoped, person-linked);
  2. a fresh `GetDirect` plus a fingerprint recheck outside any transaction;
  3. one `WithTenant(target)` transaction for the predecessor transition, the plain INSERT and the audit rows.
- **Error mapping.**
  - Each Kind has one fixed body (`handlers.go:135-159`), so registration 409s are byte-identical apart from `request_id`.
  - `TestProviderCredentialAPI_RegistrationErrorsUniform` is present.
  - Syntactic problems give 400. A missing or unknown transition reason gives 400. Scope-invisible objects give 404.
  - No trigger text is returned.
- **Permissions:** all four go to `RolePlatformAdmin` (`permission.go:595`, in the block starting at 543). `read` and `revoke` go to `RoleTenantAdmin` (`:683`, block at 621). No other role has any of them, and none is implied by `PermTenantWrite`, `PermCasinoConfigWrite` or `PermProviderConfigWrite`.
- **O4.**
  - `kyc.SelectProvider` (`internal/kyc/provider_selection.go:50-89`) reads only `provider_id` from the tenant's own active, in-window outbound KYC handles, in a `WithTenant` transaction (not player scope, so B2 does not blind it).
  - Several matches are ambiguous and fail closed. With none, only a lone synthetic adapter is accepted.
  - Mutant M24 is killed.

## 6. Observations (no action required)

- **O-1.** `integrityFailure` logs `secret_ref` on the webhook path (`fetcher.go:403-407`), which contradicts `Ref.String`'s own comment "never logged at the public webhook boundary" (`secretstore.go:227-229`). A ref is staff-visible, not secret, and this is an internal P1 alert, so it is acceptable. Align the comment, or log `handle_id` (the resolver-side alert already does that, `resolver.go:201-203`).
- **O-2.** `resolver.go:200-203` labels a ref parse failure and a malformed row fingerprint as `ref_outside_namespace`. This is cosmetic.
- **O-3.** A missing permission gives a middleware 403 with no denied audit row. This is consistent with the platform's `RequirePermission` precedent. The foreign-tenant denial *is* audited.
- **O-4.** `to_char(±infinity)` is NULL, so an infinite timestamp hashes like NULL. This is not exploitable:
  - the request row is immutable and approvals bind to the request id;
  - consume compares columns `IS NOT DISTINCT FROM` independently of the hash;
  - the API's RFC 3339 parsing cannot produce infinity.
- **O-5.** For `KeyImplicit`, the active credential's `NotAfter` is zeroed (`resolver.go:153-157`). The window was already enforced by the DB-clock read, so no gap.
- **O-6.** devfile checks the root and the final file but not intermediate directories. The root is 0700 and `os.Root` blocks escape, so this is acceptable for a dev-only backend.
- **O-7.** The inbound secret fetch (up to 2 s plus 250 ms) runs while the caller's callback transaction is open. This was accepted in §5 and is bounded by the 4-slot semaphore (`TestStoreOutage_DoesNotPinPool`). It is distinct from the outbound gap in §3.

## 7. Launch-blocking flags

- **New:** PROV-OUTBOUND-CRED-1 precision 1 (W2A-SEC-1). No real outbound adapter may go live before it is met.
- **Unchanged:** the flags from `04-review-security.md` §8 and `06-gate-w1-review-security.md`. That includes DEPLOY-FPKEY-1 and the `awssm` backend (W3b, NOT IMPLEMENTED).

## 8. Scope of this review

**Covered:** the W2a diff `6e3d74c..3ef18f2`:
- migration 0096 up/down and `deploy/init-app-role.sql`;
- `internal/{secretstore,providercred,webhookauth}`;
- the config fingerprint-key and backend settings;
- the admin API, permissions, apierror codes and OpenAPI;
- the domain verify wiring;
- O4, the httpclient/providers credential removal, CI guards and ignore files.

**Not covered:**
- integration-test execution (not run here; relied on test source and the mutation record);
- the W2b/W3a merge in progress;
- the W3b `awssm` backend;
- real vendor schemes;
- `deploy/` beyond `init-app-role.sql`;
- volumetric DoS beyond §5;
- runtime timing side channels.

Passing this review does not make the subsystem "secure" in any certification sense. It is a code-level
review at development stage. Both conditions must be closed, or tracked as stated, before W2a is labelled
complete in the gate log.
