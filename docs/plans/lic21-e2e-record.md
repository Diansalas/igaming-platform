# lic21 record: Back Office e2e for KYC policy authoring and residence maker-checker, against the real API and database

Branch `lic21-e2e-kyc-residence`, merged with `integ-b` c59bc38 (lic23, migration head 0186). Test code only; no production code, migration or ADR changed. Software capability evidence only: all values are synthetic test inputs, none is a recommendation or a jurisdiction decision.

## 1. Result (final tree, fresh private database, whole suite via the harness)

`37 passed` (specs 01-06 pre-existing: 17; spec 07: 11; spec 08: 9), 0 failed, 0 skipped, 11.1 min. Earlier runs on the pre-merge tree: 07 alone 11/11, 08 alone 9/9. One intermediate full run failed 07(a) because spec 06 leaves approved operating bases (see 5.3); fixed and re-run.

Exact command (from the worktree; the DB script creates a private database, never roles or passwords):
```
export PRIV_DB=lic21e$(date -u +%H%M%S)p$$x01 PRIV_SRC=<worktree>
bash <scratchpad>/priv_db.sh
export E2E_OWNER_DATABASE_URL=<owner url of that DB> E2E_RUNTIME_DATABASE_URL=<igaming_runtime url of that DB>
bash backoffice/e2e/run.sh            # E2E_SPEC='07-kyc|08-resi' to run only these
# drop the database afterwards
```
(the wrapper actually used: scratchpad `lic21_run.sh`.) DB dropped, node_modules symlinks removed, run dir deleted.

## 2. Files

New: `backoffice/e2e/specs/07-kyc-policy.spec.ts`, `08-residence-maker-checker.spec.ts`; `lib/kyc_db.ts`, `kyc_proof.ts`, `kyc_world.ts`, `kyc_policy.ts`, `kyc_residence.ts`; `tools/dbprobe/main.go` (Go, test-only, imports `internal/kyc` only; no signing code, so the `actorproof` signer-owner pin is untouched; `go vet`, `gofmt`, and unit tests of actorproof, db, kyc, cmd/*, residenceapproval, kycpolicyproof pass).

Shared files edited (all additive): `backoffice/e2e/run.sh` (a) builds `tools/dbprobe` into the run dir as `e2e-dbprobe` and exports `E2E_RUNTIME_DATABASE_URL`; (b) adds `TEST_SUPPORT_ENDPOINTS_ENABLED=true MOCK_GEOLOCATION_COUNTRY=MT` to the platform-api environment. `backoffice/e2e/README.md` (two table rows, one paragraph, one not-covered paragraph). NOT edited: `setup/*`, `lib/api.ts`, `session.ts`, `world.ts`, `db.ts`. Merge note for lic22: both edit `run.sh` and the README; the hunks are small and independent.

## 3. Coverage map

Spec 07 (UI as platform admins, DB ground truth, runtime-role probes):
- author a draft with explicit fields (a); draft governs nothing: unverified deposit over the draft threshold accepted (a)
- activation by the author refused, 403 rendered, DB and audit unchanged; same via API (b)
- second account of the author's Person refused (no UI hint exists, the DB decides); API message; (c)
- unlinked-Person admin refused for activation and authoring, API says "not linked to a Person" (d)
- runtime role on the draft: author account (P0001 four-eyes), same Person KP003, unlinked KP002, no reason code KP004, no proof AP001, forged MAC AP002, unknown kid AP002, expired AP003, proof for another transition / reason / admin AP004, tenant-shaped session updates zero rows, control with a valid proof passes (rolled back); fingerprint unchanged (e)
- Person-distinct admin activates through the UI, DB active, audit actor; the active row offers only Supersede; API withdraw 409; DB switch-off refused ("requires supersession"); DELETE removes nothing; TRUNCATE refused (f)
- real deposit path (mock KYC vendor, mock PSP): 4999 allowed, 5000 and 10000 declined for an unverified player, declined while the vendor verification is pending, allowed after the compliance reviewer approves; idempotent replay returns the same intent; decline reason and audit rows checked (g)
- supersede: both authors refused (UI and API), third admin succeeds, old row withdrawn with successor, one active row, enforcement follows the new threshold for a new player (h)
- withdraw a draft: author refused, other admin succeeds, no resurrection (API 409, DB "illegal status transition") (i)
- tenant staff: no nav, UI denial, 403 on read and every write, 401 anonymous, fingerprint unchanged (j)

Spec 08 (UI as tenant compliance staff):
- default None; None -> All applies at once (a, b)
- pending determination under All: "Not market evidence" in the UI; the real Go read `kyc.ReadMarketResidenceEvidence` (runtime role) returns no evidence; the pending API carries no country (c)
- approval refused for the determiner, another account of the determiner's Person, and an account of the player's own Person, in UI, API and DB; nothing changes (d)
- 33 runtime-role attempts, each with its asserted SQLSTATE and an unchanged-database fingerprint: residence guard RD005 (country only, source only, backdated, future-dated +1 day); table CHECK 23514 (country NULL); verification guard KV001 (approved -> pending / review_required) and KV002 (provider_reference NULL / changed, created_at); 42501 on every INSERT/UPDATE/SELECT/DELETE/TRUNCATE of the residence tables; the definer function with no proof AP001, forged MAC and unknown kid AP002, expired AP003, proof for another verification / reason / actor AP004; with a VALID proof: determiner, determiner's other account and player's Person RD004, wrong role and tenant mismatch RD001, stale determination time RD040. Rolled-back control: the same call with a valid proof by a distinct Person succeeds (e)
- distinct Person approves in the UI; evidence read now returns the country; second approval attempt 404/409 (f)
- All -> Into open market is filed pending (2 approvals), requester disabled/403, same-Person account refused, same Person twice refused, two distinct Persons activate (g)
- Into open market: declaration omitted, empty, "maybe", "TRUE" all RD010 and nothing written; real route: served country pending and not evidence, unserved country effective at once (h)
- the default mode None is restored through the real two-approver route (i)

## 4. Honest findings (characterisations, not claims of protection)

1. A holder of the runtime credential can write a new determination row directly (a later `verified_residence_set_at`, any country); the database cannot prove the writer is a reviewer. Under mode All it lands pending (asserted, rolled back). Already recorded as a residual in lic10/lic18.
2. In Into open market the database trusts the application's `app.residence_into_open_market` declaration: "false" yields a determination needing no second Person (asserted, rolled back). Already lic10 decision 4; mode All is the answer. These are asserted so a change of behaviour is noticed.
3. With ANY approved operating basis in a tenant, the deposit KYC evaluator fails closed (`kyc_unavailable:basis_scope_unavailable`) for a player with no verified residence, regardless of policy (observed when spec 06 had run first). Intended lic14 behaviour, but it couples specs through shared state; see 5.3.

## 5. Gaps and limits (NOT COVERED unless stated)

1. Play-trigger enforcement (casino/sportsbook) with an active policy: a `play` policy is authored and withdrawn, but no play is attempted. NOT COVERED.
2. The market gate's KYC-residence evidence source is NOT exercised through the deposit path: the harness API runs with `MOCK_GEOLOCATION_COUNTRY=MT`, so the gate uses the static mock. The evidence read is exercised directly through the real Go function (`e2e-dbprobe`), which is the consumer under test, but "a pending determination blocks a deposit at the market gate" end to end is NOT COVERED.
3. Shared-state hygiene: policies cannot be switched off or deleted, so spec 07 starts by withdrawing all active operating bases of the synthetic tenant (real single-actor route) and ends by superseding its policy with a non-binding successor (threshold 99999999999999999999 EUR); spec 08 ends by restoring mode None. Any later spec needing operating bases must create its own (06 does). A policy for another asset would make EUR deposits fail closed (N2 rule); none is created.
4. The reviewer determination itself is made through the real review API as set-up, not the UI (no Back Office determination screen is in this scope); KYC review screens NOT COVERED.
5. Concurrency (two simultaneous approvals / activations), retry idempotency of policy writes, the 72 h expiry and 24 h stale-approval rules, key rotation: NOT COVERED. Audit before/after payloads are checked for count and actor only.
6. The below-API probes run in rolled-back transactions as the runtime role with claimed session settings (what a credential holder can do); proofs are signed in TypeScript with the run's throwaway key in the verifier's wire format (the Go signer pin is untouched). A refusal is attributed to its control by the specific SQLSTATE plus the passing control case; no migration was mutated, so no mutation-kill evidence exists for these tests.
7. Fixtures: the unlinked platform admin and the extra tenant staff (same-Person and player-Person compliance accounts) are inserted as the table owner with the SF014 trigger disabled for that statement (the documented fixture path of `staffsv.InsertLegacyUnlinkedStaff`), password hash copied from a seeded account; enrolled over the real HTTP flow. No role, password or shared credential state was touched. TEST_SUPPORT endpoints are now on for every spec in this harness (extra routes only).
8. Timing: each login consumes a TOTP step and the critical step-up window is 60 s, so runs include waits; spec 06(h) alone waits 70 s.

## 6. Verdict (per CLAUDE.md labels)

Spec 07 and 08 against the real stack: IMPLEMENTED, with the NOT COVERED items above (play enforcement, end-to-end market-gate effect, concurrency) explicitly outside the claim.
