# RV-PRH-I3 — Security code review of the ADR 0096 KYC enforcement implementation

Reviewer: `security`, 2026-09-27. Code reviewed: commits `69f603f`, `9dc362d`, `2858ac2`, `df0a0b2` and
`b3e1e85`, merged in `d4a0e08`. After fast-forwarding to `3bd55b2` I confirmed that the only later
changes to reviewed files are unrelated (ADR 0097 retry-semantics registration in `casino.NewOrchestrator`).
Inputs: ADR 0096 §13 (my original conditions C1–C10), `rv-0096-security-reverify.md` (N1–N7 and the
C3 ruling), and the §15 implementation record.

**Method.** I checked each condition against the **code**, not against §15:
- `internal/kyc/enforcement*.go`
- `migrations/0100_*`
- `internal/withdrawal/withdrawal.go`
- `internal/httpserver/{withdrawal,kyc_enforcement}_handlers.go`, `kyc_routes.go`
- `internal/auth/permission.go`
- the casino and sportsbook call sites

I then ran the kyc, withdrawal, casino, sportsbook and httpserver integration suites. I also ran
throwaway adversarial probes (deleted afterwards) and three throwaway mutations (reverted; `git diff`
clean afterwards).

**Out of scope.** The deposit gate (`payments.InitiateDeposit`) and the payout T1p gate belong to
PRH-I1, and their absence is not held against PRH-I3. `DenyForCompliance` has **no caller yet**, so I
reviewed it statically only. No pen test was done. No legal adequacy review was done (HD-KYC-*,
ruling (b)). Passing this review does not make the feature "secure" beyond what is listed here.

## Verdict: **APPROVE WITH CONDITIONS**

The mechanism is sound:
- The outcome is computed server-side only.
- The read key is correct.
- Latest-row, expiry and fail-closed semantics hold.
- Migration 0100's RLS is correct. It rejected every unauthorized write I tried.
- Decisions are append-only and PII-free.
- The staff read API behaves correctly.
- Withdrawal denials are durably committed.

I found no exploitable code defect in PRH-I3's own scope. However:
- **Two required test sets are missing.** Two of my three mutations survived (F1, F2).
- **Four-eyes on relaxing a policy (`active→withdrawn`) is not met** (F3).

**PRH-I3 must not be labelled complete without qualification until F1 and F2 are closed.** F3 blocks
any policy-write API, and blocks **launch if any `kyc_enforcement_policies` row is `active`** (C3
ruling condition 5).

## Evidence run

| Item | Result |
|---|---|
| `go test -tags=integration ./internal/kyc ./internal/withdrawal` (private DB `sec_prh_i3`, migrated 1→100 via `cmd/migrate`) | ok / ok |
| `./internal/casino ./internal/sportsbook` (private DB) | ok / ok |
| `./internal/httpserver` (private DB) | 12 `TestProviderCredentialAPI_*` failures, all `permission denied for table persons`. **Environment only:** the private DB lacks `deploy/init-app-role.sql` runtime-role grants, which need a superuser. The same tests pass on the shared CI DB, which is also at migration 100. Every other test passed. |
| Probe: platform-service scope (`WithPlatformService`) INSERT of a policy | Rejected by RLS (42501) |
| Probe: **runtime role**, tenant-scoped, with `app.platform_admin_principal_id` also set | Rejected by RLS (42501) |
| Probe: platform-admin INSERT with `status='active'` | Rejected by RLS (INSERT `WITH CHECK status='draft'`) |
| Probe: platform-admin INSERT with `created_by_actor_id` ≠ principal | Rejected by RLS (provenance cannot be forged) |
| Probe: UPDATE `threshold_minor_units`; UPDATE `status` + `asset_code` together | Rejected by trigger ("only status may change") |
| Probe: DELETE as platform admin | 0 rows (no DELETE policy); row intact. TRUNCATE rejected by trigger |
| Probe: tenant B scope inserting a decision row for tenant A | Rejected by RLS |
| Probe: **activator P2 alone withdraws the active row created by P1** | **Succeeds** → F3 |
| Probe: correct account, wrong `BrandID` | `failed` / deny |
| Probe: correct account, random `PersonID` | `passed`; the overlay is silently skipped → F5 |
| HTTP probe: unverified player `POST /v1/me/withdrawals` | 409. Exactly 1 decision row, 1 `kyc.enforcement_denied` audit row, 0 `withdrawal_requests` rows (C5 holds) |
| HTTP probe: `GET /v1/admin/kyc/enforcement-decisions`: compliance A / tenant_admin / support / finance / player token | 200 (exact §6 field set; `limit=100000` capped) / 403 / 403 / 403 / 403 |
| HTTP probe: valid compliance **B** token + tenant A `player_account_id`; B + nonexistent id | 404 / 404, identical bodies |

### Mutation spot-checks (throwaway; reverted; `git diff` clean)

| Id | Mutation | Result |
|---|---|---|
| MA | `evaluateDepositThreshold`: the asset-not-covered branch returns `not_required` instead of `unavailable` (undoes N2 fail-closed) | **SURVIVED** (the whole `internal/kyc` suite passes) → F2 |
| MB | `evaluateWithdrawalStructuralRule`: cross-account overlay result ignored | KILLED by `TestEvaluateEnforcement_WithdrawalCrossAccountRejectedOverlayDenies` |
| MC | `withdrawal_handlers.go`: fresh-transaction `kyc.RecordDecision` on deny replaced with a no-op (the denial record is lost) | **SURVIVED** (httpserver `Withdraw\|KYC\|Financial` tests pass) → F1 |

## Condition-by-condition status (verified in code)

| # | Status | Evidence / notes |
|---|---|---|
| C1 (every withdrawal requires `passed`; per player, not per wallet) | **CLOSED** | `evaluateWithdrawalStructuralRule` has no history query and no exemption. No row → `failed`. Tests: `WithdrawalNoFirstWithdrawalExemption`, `NoVerification_Failed`. Every withdrawal request goes through the gate; an idempotent replay returns the existing row without re-evaluating (ledger-finance N2), which is correct. |
| C2 (0100 RLS/FORCE/platform-admin-only, mirroring 0075) | **CLOSED** | The INSERT/UPDATE predicates match 0075:207-225 verbatim, plus `created_by_actor_id = principal` and `status='draft'`. `FORCE` is set on both tables. There is no DELETE or FOR ALL policy. The lifecycle trigger allows exactly `draft→active`, `draft→withdrawn` and `active→withdrawn`; every other column is immutable. Both tables have TRUNCATE guards. The decisions table uses a `NULLIF` tenant predicate with an append-only + TRUNCATE trigger. Probes above: the tenant role, the runtime role, platform-service scope and forged provenance are all rejected. |
| C3 (audit every write; four-eyes on relaxing) | **PARTIAL** | The DB enforces creator ≠ transitioner, and INSERT-as-active is impossible, so **activation** requires two principals, as the C3 ruling (conditions 2 and 3) requires. **`active→withdrawn` does not:** any admin other than the creator, including the activator, can relax the policy alone (F3). No pending-approval workflow exists, and supersession atomicity is left to the caller. Both gaps are disclosed and acceptable **only** while no write API exists (there is none: `Create/Activate/WithdrawEnforcementPolicy` have no callers). Audit records are written but are thinner than C3 specifies (F4). |
| C4(a) latest row | **CLOSED** | `ORDER BY created_at DESC, id DESC LIMIT 1`. `LatestRowWins` tests run in both directions. |
| C4(b) expiry | **CLOSED** (mechanism) | `expires_at <= now()` → `failed` regardless of status. Test present. Note that N6 remains (F9). |
| C4(c) error → unavailable → deny | **CLOSED** (by code reading) | Every lookup error maps to `unavailableDecision(...)` with `Allowed:false`. "No licence" is kept separate (not_required) from a query error (unavailable). No fault-injection test exists; this gap was already disclosed in the mutation evidence. |
| C4(d) licensing jurisdiction only | **CLOSED** | `JurisdictionCode` is removed. The jurisdiction is resolved **inside** the evaluator via `tenants.licence_id → licences.jurisdiction_id`, and callers cannot supply it. This leaves less for callers to influence than the ADR sketch did, and I accept the deviation. |
| C4(e) / N2 asset-aware, fail-closed | **CLOSED in code / NOT TESTED** | The unique index includes `asset_code`. An active row for another asset only → `unavailable`. MA survived (F2). True cross-asset aggregation is correctly deferred to KYC-FX-AGG-1. The cumulative sum excludes in-flight deposits and is per account, which is disclosed and belongs to HD-KYC-1. |
| C4(f) cross-tenant with a valid token | **CLOSED** | `CrossTenant_NeverEvaluatesAnotherTenantsRows` plus my HTTP probe. |
| C5 denial survives rollback | **CLOSED in code / NOT TESTED** | Withdrawal: `KYCDeniedError` rolls back, then the handler records the denial in a fresh `WithTenant` transaction (the casino CAS-RECON-1 pattern, which C5 allows). Play: records in the same transaction and returns `nil`+Declined, so it commits. Probe confirmed 1/1/0. **No committed test; MC survived** (F1). |
| C6 payout gate / raw guard | **DEFERRED to PRH-I1** (accepted) | T1p and `ClaimForDispatch` do not exist yet. Guarding `MarkSubmitted` today would guard the wrong edge (ledger-finance N6). This remains an open PRH-I1 gate. |
| C7 staff read API | **CLOSED in code / NOT TESTED** | Tenant comes only from `tenant.FromContext`, with `RequireTenantScope`. Cross-tenant and nonexistent ids give the same 404. `PermKYCEnforcementDecisionRead` is held by compliance and platform_admin only, never tenant_admin. Keyset on `(decided_at,id)`; default 25, max 100. Exact field set. There is no platform-admin cross-tenant path at all, which is stricter than C7 allowed. **The tests C7 required (cross-tenant 404, 403 for other roles) are absent** (F1). |
| C8 players see status only | **CLOSED with LOW residue** | No `matched_trigger`, policy id, version or threshold reaches a player. `unavailable` is not presented generically (F7). |
| C9 dormancy observability | **PARTIAL** (accepted) | `ListDormantJurisdictionTriggers` is a query only, with no route. It is not per asset (N2's extension is disclosed as not done), and it does not filter for live tenants. It correctly reports only the evaluator-wired `cumulative_deposit`. |
| C10 no thresholds in code/SQL | **CLOSED** | No numeric threshold in non-test Go or in 0100. The only numbers are the page-size caps and the `> 0` CHECK. |
| N1 read key | **CLOSED** (implementer's choice confirmed correct) | The primary key is `(tenant_id, brand_id, player_account_id)` for every operation. `PersonID` is used **only** in the deny-only `crossAccountRejectedOverlay` on withdrawal: another account's *latest* row is `rejected`, within the same tenant. This is exactly my source prescription. Following my text over the relayed paraphrase was correct. Two residues: stale comments (F6) and a PersonID the evaluator trusts from its caller (F5). |
| N3 unwired triggers | **CLOSED** | The trigger refuses `draft→active` for `edd_amount`/`registration_tier`. Tested. |
| N4 typed deny not rolled back | **CLOSED in code / NOT TESTED** | Withdrawal uses rollback plus a fresh-transaction record (the allowed alternative). Play returns `nil`+Declined. See F1. |
| N5 payout deny carve-out; fresh-transaction record if `DenyForCompliance` fails | **DEFERRED to PRH-I1** | The carve-out is documented and the posting mirrors Reject/Fail. The fresh-transaction record on failure is caller-owned and has no caller yet (F10). |
| N6 `expires_at` never written | **OPEN (LOW)** | Still true. §15 says "superseded by §15's own text", but §15 does not state what NULL means (F9). |
| N7 tidy-ups | **Mostly closed** | (b) and (e) done. (c) `effective_from` is stamped `now()` and is informational only; acceptable. The forge-proof stamp trigger exists. (d) `created_by_actor_type` still has no CHECK (code writes `'staff'`) (F9). |

## Findings

**F1 — MEDIUM (blocks marking PRH-I3 complete) — The regression tests C5, N4 and C7 required are
missing.** The behaviour is correct today; my probes show it. But nothing in the committed suite
protects it:
- Mutation MC deletes the only durable record of a withdrawal KYC denial, and every test passes.
- There is no HTTP test for `/v1/admin/kyc/enforcement-decisions` at all.

*Failure scenario:* a later refactor drops or reorders the fresh-transaction record, or grants the
permission to `RoleTenantAdmin`. The denial audit trail, or tenant-bound role separation, silently
regresses.

*Required tests:*
- (a) Through the real handler and `WithTenant`: an unverified `POST /v1/me/withdrawals` → 409, exactly
  1 decision row, 1 `kyc.enforcement_denied` audit row, 0 `withdrawal_requests` rows, and no
  ledger/hold change.
- (b) The same for `unavailable`.
- (c) Staff route: a valid tenant-B compliance token with a tenant-A `player_account_id` → 404, same
  body as a nonexistent id.
- (d) tenant_admin, support, finance and player tokens → 403.
- (e) `limit` above the max is capped, and a keyset second page has no overlap.

**F2 — MEDIUM (blocks marking complete) — No test covers the active-policy paths.** No test activates
a `cumulative_deposit` or `play` row. Untested:
- threshold comparison,
- asset-not-covered → `unavailable` (N2; mutation MA survived),
- the casino and sportsbook KYC deny (declined result plus a committed decision row),
- jurisdiction resolution for a tenant **with** a licence bound (every existing test uses an
  unlicensed tenant, and so exercises only `not_required`).

*Failure scenario:* N2's fail-closed branch regresses to `not_required`, and unlimited deposits in an
unconfigured asset pass once HD-KYC-1 activates a threshold. The gap is also operational: if the
`licences` read fails under tenant scope, every bet for a licensed tenant becomes `unavailable`. That
fails closed, but it is an outage.

*Required:* use two-principal fixtures, since four-eyes applies in fixtures too, to activate each of:
- a EUR `cumulative_deposit` row, then assert below → not_required, at or above → requires `passed`,
  and USD → `unavailable`;
- a `casino_play` row, then assert a casino bet from an unverified player is declined and a decision
  row is committed;
- the same for `sportsbook_play`.

**F3 — MEDIUM (blocks any policy-write API; blocks launch if any policy row is `active` at launch) —
`active→withdrawn` is a single-principal relaxation.** The lifecycle trigger checks only that
`acting_principal ≠ OLD.created_by_actor_id`. The activator, or any third platform admin, can
withdraw an active row alone. The probe confirmed this.

*Failure scenario:* one compromised or rogue platform-admin session withdraws the active
`cumulative_deposit` or `play` row. Every tenant in that licensing jurisdiction immediately evaluates
`not_required` (dormant), and no second person is involved. This is the relaxation C3 ruling
condition 2 required four-eyes for.

A related gap: the row does not persist who activated it (the fact exists only in `audit_log`), so the
DB cannot even require the withdrawer to differ from both earlier actors.

*Fix, either:*
- a DB-enforced request/approve record for withdrawing an active row (approver ≠ requester by
  CHECK/trigger), or
- per C3 ruling condition 1: refuse `active→withdrawn` at the DB until that mechanism exists, apart
  from an atomic supersede. Supersede means withdraw-old + activate-new in one transaction, where
  activating the new row already requires a second principal. Ship that as a single helper so
  supersession atomicity is not left to the caller.

**F4 — LOW — Policy-write audit records are thinner than C3 specifies.**
- `kyc_enforcement_policy.created` lacks `legal_review_reference`, the threshold/asset/tier/play
  values, and an after-row.
- `.activated` and `.withdrawn` lack the jurisdiction, before/after status and a `reason_code`.
  `WithdrawEnforcementPolicy` takes no reason at all.
- There is no IP, because there is no HTTP surface yet.

*Fix:* before a write API ships, record the full after-row on create, `{jurisdiction, trigger_type,
asset, before, after}` plus a mandatory reason on each transition, and the request IP from the future
handler.

**F5 — LOW (hardening) — `PersonID` is trusted from the caller.** The evaluator never checks that
`PersonID` belongs to `PlayerAccountID`. Passing any other UUID silently disables the cross-account
overlay (probe: `passed`). All three current callers derive it server-side:
- the withdrawal handler, via `identity.GetPlayerAccountByID` under tenant RLS,
- casino, via `decision.PersonID`,
- sportsbook, via `rgDecision.PersonID`.

So this is not exploitable today. *Fix:* inside `EvaluateEnforcement`, read
`player_accounts.person_id` for the account (tenant RLS), and either use it or return `unavailable` on
a mismatch. PRH-I1's deposit and payout call sites must not become the first caller that gets this
wrong.

**F6 — LOW — Doc comments contradict the implemented read key.** The `EnforcementParams.PersonID`
comment (`enforcement.go:123-133`) says "the KYC read key is per Person, latest verification row
across every PlayerAccount". `withdrawal.RequestParams.PersonID` says the rule is "scoped per
Person". Both describe the N1 bug, not the code. *Failure scenario:* a maintainer "fixes" the query
to match the comment, and an approval on account 2 masks a rejection on account 1. *Fix:* rewrite
both comments to state the per-account primary key plus the deny-only overlay.

**F7 — LOW — Player-facing presentation of `unavailable` and sportsbook subcodes (C8 residue).**
- Withdrawal: `unavailable` returns 409 `"withdrawal requires a passed identity verification:
  unavailable"`, not a generic retryable failure.
- Sportsbook: `toPlaceBetRejectionResponse` passes `RejectionCode` through verbatim, so a player can
  see `kyc_unavailable:policy_lookup_failed` or `kyc_unavailable:jurisdiction_unresolved`. This
  exposes internal failure detail. It does not reveal thresholds.

*Fix:* map `unavailable` to a generic retryable response (5xx or `retry_later`), and collapse
sportsbook KYC codes to `{kyc_pending, kyc_failed, kyc_retry_later}`.

**F8 — LOW — Denial-record loss paths.**
- (a) If the withdrawal handler's fresh-transaction `RecordDecision` fails, the failure is only
  logged. Add a metric/alert.
- (b) On the play path, a DB error inside the transaction aborts it. `unavailable` is returned
  (deny), but the in-transaction `RecordDecision` then fails, the error propagates and the
  transaction rolls back, so the denial is unrecorded. It still fails closed. Mirror the withdrawal
  pattern (a fresh-transaction record) if play denials must always be recorded.

Also disclosed and accepted: dormant (`not_required`) play evaluations write no decision row, which
narrows §7.6. I accept this because it is a per-bet hot path and nothing is being decided. Any
non-`not_required` play outcome is recorded.

**F9 — LOW (carried) —**
- N6: `kyc_verifications.expires_at` is still never written. Every approval is valid until the
  provider reports otherwise. State this, or tie it to an HD. §15 does not currently do so.
- N7(d): `created_by_actor_type` has no CHECK.
- C9: dormancy is not per asset and does not filter for live tenants.

**F10 — INFO (PRH-I1 carry-forward, not a PRH-I3 failure) —**
- `withdrawal.DenyForCompliance` has **no caller and no test**. PRH-I1 must test:
  - exactly-once release,
  - a lost race with Reject or Cancel → `ErrStateConflict` → decision and audit committed in a fresh
    transaction (N5),
  - no `MarkSubmitted` after a deny,
  - the retargeted raw guard (C6 / ledger-finance N6).
- The deposit call site must validate `Amount > 0` before calling `EvaluateEnforcement`. A negative
  player-chosen amount lowers `cumulative + Amount` below the threshold.

## Launch flags (for orchestrator/human)

- Ruling (b) still stands. Dormant deposit KYC with real money needs explicit HD-KYC-1 and legal
  sign-off.
- F3 blocks launch **if any policy row is `active`**. With zero active rows, F3 blocks only the
  future write API.
- C6 and N5 remain open PRH-I1 gates for the payout path.

## What this review does not cover

- PRH-I1's deposit and payout wiring.
- ADR 0095 `ClaimForDispatch`/T1p/T2/T12.
- Any future policy-write HTTP API.
- Legal adequacy of any threshold or dormancy posture.
- Penetration testing.
- Performance claims (§7.7).

A re-review is required when F1 and F2 land (tests only; I will re-run MA and MC), and again when a
policy-write API or the F3 mechanism lands.
