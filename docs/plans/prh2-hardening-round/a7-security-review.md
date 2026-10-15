# A7 AML operability: `security` live review (branch `a7-aml-operability`, head `5651985`, base `f9a31e7`)

Reviewer: `security` specialist, 2026-10-15. This is a code-level and design-level review plus live probes against a PRIVATE
database rebuilt from this worktree's migrations. It is not a penetration test or an audit. The review does NOT make anything
"secure"; it records what was probed and what was not. Software capability only: nothing here is a regulatory approval.

## Verdict

**CONDITIONAL. A7 cannot be marked complete until SR7-1 is fixed.** SR7-2 and SR7-3 are latent: no HTTP route reaches them
today. They must be fixed before any screening route, trigger or policy route is wired (F1 or F3). They are also
**launch-blocking** for any tenant that has screening enabled. The rest are Low or informational. Nothing found gives cross-tenant
data access, lets someone without the grant act, lets one officer pass four-eyes, or tips off a player. In those areas the branch
held up under every probe below.

## Findings

### SR7-1 (MEDIUM, introduced by this branch): a stale or stolen officer session can place irreversible `confirmed_sanctions` holds in bulk, with no step-up, no rate limit and no alert

`POST /v1/admin/tenants/{tenantID}/aml/holds` is classed `none`. The handler accepts `reason_class: "confirmed_sanctions"`
(`aml.PlaceHoldInput.validate` admits it, `aml_hold_place` inserts it). A `confirmed_sanctions` hold gates ALL value movement. Only the
full case workflow can lift it: open, transition, a `critical` propose by officer 1, then a `critical` approve by a different
Person. ADR 0119 §8.1 exempts protective actions from step-up on two conditions: "the reverse actions are themselves step-up and
four-eyes" and "each one is audited and, where it applies, alerted". The first holds. The second does not: there is no
`aml.hold_placement_rate` alert (record F4). Rule 8.1 also already makes an exception for the capability-grant revoke: a protective
action is classed critical when it is a DoS vector.

Repro (live, private DB): an own-licence tenant with a 2-officer quorum. A compliance officer token whose second factor is **8
minutes old** placed **25/25** `confirmed_sanctions` / `all_value_movement` holds on 25 players in **374 ms**. Every placement
returned 201. Undo attempts:
- A fresh-factor single-officer lift returns `409 hold_refused`.
- Each hold needs its own four-eyes case.
- Combined with SR7-4, the same session can keep invalidating those proposals.

A stolen access token, without the TOTP factor, does all of this.

Fix:
1. Add a pinned step-up classifier `amlHoldPlaceClass` for `POST .../aml/holds`. A body with `reason_class=confirmed_sanctions`
   is `critical`; `manual` is `none`. Reuse the buffered-body classifier pattern (`financialPolicyChangeClass`).
2. Add the class pin to the golden table and to `TestAMLHTTP_StepUpClasses`.
3. Build the `aml.hold_placement_rate` alert (F4) before launch. Optionally add a per-officer placement rate limit.

### SR7-2 (MEDIUM, latent, introduced by this branch): `aml_screening_record` lets one officer create un-liftable `confirmed_sanctions` holds without the quorum, for outcomes that are not sanctions matches

`aml_screening_record` skips `aml_assert_quorum` on purpose. It maps `p_decision = 'hold_all_value_movement'` to `reason_class =
confirmed_sanctions` whatever the outcome is. The table CHECKs only set a minimum ("a sanctions match implies all value movement").
They do not set a maximum, and `RecordScreeningInput.Validate` has the same gap. So an officer-attested record with outcome `error`,
or a PEP-only `potential_match`, with decision `hold_all_value_movement`, is accepted.

Repro (live, Go service as the runtime role, proof-bound): an own-licence tenant with ONE officer (quorum NOT met).
- `PlaceHold(confirmed_sanctions)` is refused with `ErrQuorum`. Correct.
- `RecordScreening{outcome: error, error_code: timeout, decision: hold_all_value_movement}` creates a `confirmed_sanctions` /
  `all_value_movement` hold.
- `RecordScreening{outcome: potential_match, match_lists: [pep], decision: hold_all_value_movement}` creates a second one.
- `LiftHold` refuses both, and four-eyes is impossible with one officer. The player is frozen until a second officer is granted.

The same thing happens routinely in platform-licensed tenants, because platform grants lapse after 4 hours.

On ADR 0116 §6, "holds placed **automatically** by screening still apply; they fail closed regardless": the quorum exemption is
for automatic screening. Today every record is OFFICER-ATTESTED (trigger may be `manual`) and nothing checks that a provider was
called (record §4.7). The implementation applies the exemption more widely than the ADR. Decide() itself is correct. The gap is that
the database and Validate accept a stronger decision than Decide would produce.

Reachability today: none over HTTP. There is no screening route, and `NewOrchestrator` is constructed nowhere outside tests
(verified by grep). `EXECUTE` on the function is granted to the runtime role, but it requires a signed proof, and only the process
issuer can sign one.

Fix, before F1 or any route:
- (a) In SQL, derive the class strictly: `confirmed_sanctions` only when `outcome = 'confirmed'`, or when `outcome = 'potential_match'`
  AND `'sanctions' = ANY(match_lists)`. Otherwise the decision must be `hold_withdrawal_and_payout` / `manual`. Add the matching
  table CHECKs:
  - `decision = 'hold_all_value_movement'` implies `outcome = 'confirmed' OR 'sanctions' = ANY(match_lists)`
  - `outcome = 'error'` implies `decision <> 'hold_all_value_movement'`
- (b) Keep the quorum exemption only for non-`manual` triggers recorded by the future system actor (F1). Require `aml_assert_quorum`
  for officer-attested `manual` records.
- (c) Mirror both rules in `Validate`, and kill-test them.

### SR7-3 (LOW, latent): `SetScreeningPolicy` is single-officer and can LOOSEN screening

`aml_screening_policy_set` requires a platform_acting officer, the quorum, a proof and a legal reference. It does not require four-eyes
or step-up. One platform officer can:
- drop the `sanctions` list (`lists = {pep}`);
- set a very large `backfill_grace_seconds`;
- backdate or forward-date `effective_from`.

It also does not check that the jurisdiction is the tenant's licensing jurisdiction (FK only). No route exists yet (F3).

Fix when F3 adds the route: make it `critical` (ADR 0119 §8.1 (d)/(e) policy write) and four-eyes for any change that reduces
lists or raises the grace period. Validate the jurisdiction against the tenant's licensing jurisdiction.

### SR7-4 (LOW): any officer can invalidate a pending four-eyes proposal with a `none`-class write

An approval must be the event immediately after its proposal, and any hold change since the proposal invalidates it. So these
`none`-class actions each silently withdraw a pending lift or SAR-closure proposal:
- a note,
- a transition,
- a SAR flag,
- a hold placement on the subject.

Together with SR7-1, a hijacked session can block remediation indefinitely. The trail attributes every such event, so this is griefing
by an insider or a stolen session, not a bypass.

Fix: add the reject operation (F4). Consider letting notes not invalidate a proposal. Include proposal invalidations in the
placement-rate alert.

### SR7-5 (LOW): a transition to `closed_no_action` is class `none`

Closing an investigation with no action is arguably "loosens a regulatory control" (§8.1 (e)). It is reversible
(`closed_no_action -> in_review`), it lifts no hold, and it is refused for SAR-flagged cases, so the impact is limited.

Fix: either add a classifier (`to = closed_no_action` is `critical`, otherwise `none`), or record the `none` class as an accepted
decision in ADR 0116 §13.

### SR7-6 (LOW / info): `R-AML-QUORUM` is not coarsened for tenant viewers of platform-licensed tenants

`coarsenForTenant` coarsens only the provider half of `R-AML-SCREENING`. `tenant_admin` and `compliance` can poll
`GET .../launch-readiness` and watch the platform officer quorum for their tenant flip. Platform grants lapse after 4 hours and are
re-issued when platform officers work that tenant, so the flip is a weak "platform AML is active on us" signal. It is not about any
individual subject. A same-class signal already exists: the tenant-visible acting-session audit rows labelled `compliance`.

Fix: map `R-AML-QUORUM` to `see_platform` for tenant viewers when the owner scope is `platform`.

### SR7-7 (info): idempotent replay compares only person / outcome / decision / subject_ref

If a key is reused with different lists, match_lists, error_code, trigger, provider or screened_at, the original is returned as
`replayed`. No hold is weakened, but the comment "a key never silently re-binds" claims too much. Fix: store the proof payload hash
and compare it.

### SR7-8 (info): `potential_match` widening

An ambiguous provider match (no list named) is widened to every requested list. With `sanctions` requested, that means a
`confirmed_sanctions` hold that only four-eyes can lift. This is the documented, conservative fail-closed reading (ADR 0116 §13 points
2 and 5). It cannot be abused today: the mock and the orchestrator are unwired.

Operational risk with a real vendor: false positives create four-eyes work. The class name also reads like a legal confirmation in
the officer UI.

Recommendation: before a real vendor is connected, consider a distinct class (`sanctions_potential`) that has the same enforcement
and lift rule but a name that does not mislead.

### SR7-9 (info): cosmetic

`AM007` on case open reports `check_codes: ["hold_refused"]`. There is no person-existence oracle: a random person id and a person of
another tenant produce identical answers (`409 hold_refused` on writes, `200 {"items":[]}` on the hold list).

## Test-isolation defect (pre-existing; `review_fixes_integration_test.go` is unchanged by this branch): CONFIRMED

On a freshly rebuilt private DB, running only `TestEligibility_PlatformOfficers_SuspendedOrDemotedAreNoOfficersAndNoQuorum` and
`TestEligibility_SanctionsLiftNeedsTwoLiveOfficers`, both fail at `setStaff(... status = 'suspended')` with `SF020` ("the last active,
enrolled platform admin cannot be removed").

Cause: `sf_last_admin` (0139) counts OTHER active platform admins that have an active, non-hard-locked `staff_mfa_factors` row. The
amlfix officers have no factor, so on a fresh DB the count is 0. Both tests pass after any test that plants an enrolled platform admin
(observed: the whole `internal/aml` package passed later in the same DB).

How to fix: the tests should plant their own spare. In `caseWorld.setStaff`'s platform branch, inside the same
`c.Owner.WithPlatformAdmin(...)` transaction and before the `UPDATE`, call
`staffsv.PlantEnrolledPlatformAdmin(ctx, tx)` (`internal/testsupport/staffsv`, integration-only; `aml_test` can import it with no
cycle). Alternatively, call it once at the start of each of the two tests. The spare is an active, Person-linked platform_admin with an
active placeholder TOTP factor and NO `aml_officer` grant, so it does not change any quorum count. Do not rely on other tests' rows.

## What was probed live (private DB `sr7_*`, migrations of this worktree, runtime role for the API)

| Probe | Result |
|---|---|
| Foreign-tenant officer (own-licence A) naming tenant B in the path, all 11 routes | 404 on all 11 |
| Foreign officer naming ITS OWN tenant with B's case / hold / person ids, all 11 routes | case and hold ids: 404; person-based reads: `200 {"items":[]}`; person-based writes: `409 hold_refused` (same answer as a random id); no data, no write |
| `finance`, `risk_manager`, `tenant_admin`, `support` tokens, all 11 routes | 403 on all |
| `compliance` with the static permission but NO `aml_officer` grant, all 11 routes | 403 on all; nothing written (the database grant is the authority) |
| Platform officer of tenant A acting in platform tenant B (no grant there), or in a non-existent tenant | 403 on both (no tenant-existence oracle) |
| `platform_admin` with no AML grant; tenant `compliance` in a platform-licensed tenant | 403 |
| `?tenant_id=` query, and body `tenant_id` / `actor_id` / `staff_id` / `placed_by` | query ignored; every body field gives 400 (strict decoder) |
| Single-officer lift of a `confirmed_sanctions` hold; propose replay; approval replay by a third officer | 409 `hold_refused`; 409 `stale`; 409 `stale` |
| Officer demoted in the DB (`compliance` to `support`) while holding a live token | 401 `session_revoked` (staff_version) |
| Stale-factor (8 min) token on the protective routes vs lift / propose / approve | protective routes: allowed (SR7-1); critical routes: 403 `step_up_required` (existing test, passes) |
| Notes: `<script>`, `<img onerror>`, `javascript:` | stored as plaintext. JSON escapes `<` and `>`. The Back Office renders `{n.body}` as React text (no `dangerouslySetInnerHTML` in `features/aml`). A RLO control character is refused by the lint (`note_refused`) |
| `aml_readiness` as the runtime role | see below |
| `aml_screening_record` without a proof; `SELECT` on `aml_screening_results`; `aml_screening_policy_effective` for another tenant | AP001; 42501 permission denied; AM026 |
| A tenant officer calls `SetScreeningPolicy` | refused (`ErrForbidden`, AM002) |
| Catalogue | All 5 API functions are `SECURITY DEFINER`, owned by `igaming`, `search_path = pg_catalog, public, pg_temp`, EXECUTE only for `igaming` and `igaming_runtime`. Both guards are invoker functions with no runtime EXECUTE. Both tables have ENABLE + FORCE RLS, one owner-only policy, and no runtime ACL. The runtime role has no CREATE on `public`. The function list in `deploy/init-app-role.sql` matches the migration signatures |

`aml_readiness` attacked as the runtime role:

| Session shape | Outcome | `app.principal_id` after the call |
|---|---|---|
| Plain tenant session | works; returns `(tenant, quorum=T)` | was unset (NULL), now `''` |
| Tenant session with a real principal X | works | X (restored) |
| Read-only transaction | works | n/a |
| Parameter names another tenant | AM026 | n/a |
| No session tenant | AM026 | n/a |
| Bare forged `app.acting_tenant_id` | AM026 | n/a |
| Owner holds ACCESS EXCLUSIVE on `staff_capability_grants`, so `lock_timeout` fires after the placeholder is set; call under a SAVEPOINT | 55P03 | `''` after `ROLLBACK TO SAVEPOINT` |
| Same lock, call inside a plpgsql `EXCEPTION` block | caught | `''` |

So the placeholder never outlives the call:
- On success the explicit restore runs.
- On error, the PostgreSQL (sub)transaction abort rolls back the transaction-local setting.
- A caller cannot catch the error with the placeholder still set.

A runtime-credential holder who forges `app.principal_id` can already count the `aml_officer` grant rows directly (observed: 2).
`aml_readiness` therefore adds nothing beyond THREAT-MODEL-ARBITRARY-SQL-1, and it returns only a boolean.

Statically verified:
- `amlHandler` is the only place an `aml.Actor` is built. The tenant comes from the token or the validated path only.
- `staffRoute` refuses a player before any work.
- Error bodies are fixed sentences plus closed check codes. The logs carry the op, the code and the SQLSTATE only.
- `Cache-Control: no-store` is set.
- No `audit_log` write (existing test).
- No player-facing or adjustment file is touched, so the F4 generic cover is unchanged.
- ADR 0114 H1: event actors and note authors go through `staffViewer.ref`; an unknown scope is hidden from tenant viewers.
- The MOCK screening provider is registered under `aml/screening_provider` in `buildRegistrations`. That list feeds
  `refuseSyntheticInProduction` at startup, so production refuses to start with the MOCK wired.
- Back Office: the approver binds the server's `content_hash` (never typed in). The self-approve button is disabled (the server also
  refuses). The server `message` is never rendered. A tenant viewer's tenant comes from the claims; `?tenant=` is used only for
  platform viewers and is UUID-validated.

Suites re-run on the private DB (all `-tags integration -count=1 -p 1`), all PASS:
- `internal/aml/...` (after the httpserver tests had planted enrolled admins; see the defect above)
- `internal/launchgov`
- `internal/actorproof`
- `internal/auth`
- `internal/httpserver -run 'AML|Aml|StaffIdentity|LaunchAML|TestStepUp_GoldenTable'`

## Not in scope of this review (not claimed)

- Real vendor behaviour.
- The automatic triggers, system actor and gate wiring (F1, F2): not built.
- Timing side channels below the HTTP layer (not measured).
- The full `internal/httpserver` and `internal/payments` packages (not run).
- Browser-level rendering beyond source review.
- The D-AML-TIP-1 owner decision. Still OPEN; the record's §6 question stands, and it remains launch-blocking for
  `under_platform_licence` tenants whose finance staff approve adjustments.
- Any regulatory adequacy of the AML process.
