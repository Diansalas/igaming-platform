# PRH-2 K3 security delta review: prh2-k3-impl @ 31e4501 (base 041fb55; main a642e8c merged in)

Reviewer: security specialist (subagent). This was a read-only review. Nothing was committed, pushed or merged, and no role, password or shared-infra change was made.
Method: `git archive 31e4501` was extracted to `$S/k3d/src` and migrated into a private DB, `k3d_sec`, owned by `igaming` with runtime grants (the `priv_db.sh` pattern). The DB was dropped afterwards. The scratch probe tests stay in `$S/k3d/src/internal/payments/zz_secdelta*_integration_test.go` and are not part of the repo. Free disk stayed at 4.3G or more.

## Verdict: ACCEPT WITH CONDITIONS

Every K3 fix under review is present, correct and covered by tests: PM-S1, PM-S2, PM-S3, PM-S4, PM-S5/R-2 and the merge. None of the K3 fixes has an open finding, so there is no K3 code change to make. The conditions are about tracking and the launch gate (finding 1), not about this branch's code.

## Findings

1. **HIGH, PRE-EXISTING, not introduced by K3, BLOCKS LAUNCH: the K2 four-eyes check in the database can be fully bypassed with a TEMP-table shadow on the runtime role.** This raises TRIGGER-SEARCH-PATH-1 from Low to High.
   - The 0112/0113 functions `ledger_adjustment_requests_guard`, `ledger_adjustment_approvals_guard`, `financial_actor_session`, `financial_acting_session_valid`, `staff_capability_grant_in_force` and `ledger_adjustment_eligible_grant` have `proconfig = NULL` (no pinned search_path) and look up tables without a schema prefix.
   - Reproduced on the runtime role (`TestSecDeltaK2Shadow`): `igaming_runtime` (PUBLIC still has TEMP on the database) created TEMP `staff_users`, `staff_capability_grants` and `staff_capability_grant_requests` holding two fictitious staff, X and Y, who have no row in `public.staff_users`.
   - X `Submit` ran, then Y `Decide`, and the result was `executed=true`. A real `compensating_entry` debit was posted to a real player wallet with no real human approver at all.
   - Who can do this: anyone able to run arbitrary SQL as `igaming_runtime` (a compromised app or SQL injection). That is exactly the threat the database guards exist to stop.
   - Why it matters to K3: K3's (d) and (c2) recon findings clear on *executed K2 compensating debits*. A forged K2 execution could therefore also clear a K3 finding.
   - The same probe against K3 (`TestSecDeltaK3Shadow`) was refused: CG001 for both the forged requester and the forged approver. The pinned K3 guards make the nested 0112/0113 helpers run with the pinned path.
   - Condition (a): before launch, update the TRIGGER-SEARCH-PATH-1 registry row to HIGH and launch-blocking, citing this evidence, and fix it before launch. Fix options: pin all 0026..0113 guard/helper functions (`ALTER FUNCTION ... SET search_path = pg_catalog, public, pg_temp`, or schema-qualify), or `REVOKE TEMPORARY ON DATABASE ... FROM PUBLIC` for the runtime role. The REVOKE is a role/infra change, so it is a human decision and no sub-agent should make it.
   - Condition (b): the ADR 0101 §27.5 residuals should mention that clearing (d)/(c2) trusts K2 executions, which remain shadowable until TRIGGER-SEARCH-PATH-1 is fixed.
2. **LOW, PRE-EXISTING (0112): read exposure through the acting_* RLS policies.** These policies call the unpinned `financial_acting_session_valid()` directly, outside any pinned function.
   - With a forged acting principal plus shadowed `staff_users`/`staff_capability_grants`, the runtime role saw every resolution (2 of 2) and every approval of the acting tenant. Without the shadow it saw 0.
   - Writes stayed refused: CG020 from the pinned guard.
   - This adds nothing an attacker could not already do: the same SQL position can forge `app.tenant_id` plus any `app.principal_id`, and `tenant_scope_select` never checks the principal against the database. The fix belongs under TRIGGER-SEARCH-PATH-1.
3. **INFO: Y02 runs as the owner role (`igaming`), not the runtime role.** K3 scratch-DB tests run as the owner. How a shadow resolves does not depend on the role, but the claim "runtime role" is not exercised by Y02 itself. My probe (1) repeated it as `igaming_runtime` with an explicit `SET LOCAL search_path = pg_temp, public` and got MR020 (held). Optional: add a runtime-role variant.
4. **INFO (PM-S2 boundary, behaving as specified):** a line whose provider reference is held by no attempt still clears (c) by merchant reference. C42b pins this as intended D-5 behaviour. The base matcher raises `pay_reference_mismatch (check=reference)` for that line in the same run (seen in my probe), so it is never silent. A stricter rule would refuse merchant-reference resolution when `a.providerRef != "" && l.ref != "" && l.ref != a.providerRef`. That is optional and LF's call.
5. **INFO:** T-ID-3 (worker visibility) does not seed rows in the K3 tables, so for K3 it proves nothing on its own. My runtime probe (3) covered it with rows present (see Merge below).

## Verification detail

**PM-S1 (VERIFIED).**
- I listed the functions in `pg_proc` myself: 0115 has 18 `CREATE [OR REPLACE] FUNCTION`s, there are 18 live functions with those names and no overloads.
- All 18 have `proconfig = {search_path=pg_catalog, public, pg_temp}` (pg_temp last), and none is SECURITY DEFINER.
- Every trigger on the 0115 tables uses a pinned function, apart from `ledger_deny_mutation` (no table lookups) and the older K2/ledger triggers (finding 1).
- The helpers the 0115 functions call (`financial_actor_session`, `financial_policy_required_approvals`, `ledger_adjustment_eligible_grant`, `..._live_person`, `..._invisible_platform_grant`, `k2_*`, `player_open_payment_exposure`, ...) are unpinned. They still run with the caller's pinned path because they are nested inside it. Proven at runtime: the forged K3 requester and approver got CG001, and an ineligible requester with a shadowed grant got MR003.
- Runtime-role (`igaming_runtime`) TEMP `payment_manual_resolutions` shadow against the reserved-prefix guard, with pg_temp first explicitly: MR020.
- `ledger_transactions_governed_fence` (0113, unpinned) only calls `financial_acting_gucs_present()` (reads GUCs only) and the pinned `ledger_governed_fence_allows`, so it cannot be shadowed.

**PM-S2 (VERIFIED).**
- `resolvesTo` uses `m.byRef` (operation plus `\x00` plus ref, covering ALL attempts of the tenant and provider, any state). Statement kind `"payout"` equals attempt operation `"payout"`, so the key matches and the fix is not vacuous.
- Attacks (`TestSecDelta_S2`):
  - (a) Another executed declared-paid payout B's line, carrying B's ref and A's merchant ref: A stays unconfirmed and B clears.
  - (b) A terminal (declared-not-paid) B's ref with A's merchant ref: A stays unconfirmed.
  - (c) An unknown ref: A clears and a reference mismatch is raised (finding 4).
- Raising predicates still use the broad `linesFor`.

**PM-S3 (VERIFIED).** The guard compares the whole row as jsonb minus an allow-list of 8 state columns, so any new column is immutable by default, and the payload_hash is recomputed. Y03 rejects an "invalid transition" fall-through refusal. My SM3 mutant (`requested_by_person_id` made mutable) was KILLED by Y03.

**PM-S4 (VERIFIED).** Y04 checks that `GREATEST(required_at_submission, policy)` holds and is not vacuous (a new request pins 1). My SM4 mutant (LEAST) was KILLED.

**R-2 (VERIFIED).** The live `tenant_system_read_executed` policy is `state='executed' AND kind IN (m2_declare_paid, m2_declare_not_paid)` and excludes principal, platform, player, service and acting GUCs. The only system reader (`payment_statement_k3.go:168`) already filters to M2 kinds, so nothing loses data. SM5 (pending admitted) was KILLED by T3.

**PM-S5 / SQLSTATE in the denied audit (VERIFIED).**
- `ResolutionSQLState` returns only `pgErr.Code`, and it is written to `meta["sqlstate"]` only when non-empty. The foreign-tenant refusal writes `""`, so the key is omitted. The HTTP body is still the closed token.
- T12 checks the exact `"sqlstate": "MR003"` and the absence of "grant"/"ERROR". SM6 (passing `err.Error()`) was KILLED on both assertions.
- `err.Error()` still goes to the server log on the default 5xx branch only. That is unchanged and acceptable.

**Merge of `deploy/init-app-role.sql` (CORRECT).**
- The E1 and K3 blocks were merged into one DO loop with both comment blocks kept. It covers codes:SELECT, resolutions:S/I/U, approvals:S/I, evidence:S/I and kyc_submission_outbox:S/I/U. Each entry does REVOKE ALL then GRANT, skipped if the table does not exist.
- These match the 0114 and 0115 in-migration grant blocks exactly and match the live grants in the private DB. There is no role, password or attribute change in the delta.
- Small wording point: ADR 27.6 says "keeps both blocks". Both are present as one block, with the same effect.

**Isolation between E1 and K3 (runtime role, rows present):**
- Worker session (`WithPlatformService(kyc_submission_worker)`):
  - Sees 0 rows in resolutions, approvals and evidence, and in payment_attempts and ledger_transactions.
  - Sees 7 rows in codes.
  - UPDATE of a resolution affects 0 rows (RLS).
  - INSERT of evidence is refused (MR050; the RLS `system_insert` arm also requires `platform_service_id IS NULL`).
  - INSERT of a code is refused with 42501.
- K3 acting session on the outbox: 0 rows. The `kso_*` policies require the acting GUCs to be NULL.
- A K3 tenant-principal session can SELECT its own tenant's outbox rows (`kso_tenant_select` does not require `principal_id IS NULL`). This is E1's existing tenant-staff read design, not a K3 change, and stays inside the tenant. Its UPDATE is refused (requires `principal_id IS NULL`).
- E1 KYC worker tests T-ID-1..4: PASS.

**`workerReferenceAllowlist` edit (ACCEPTABLE).**
- `payment_manual_resolution_codes` has two columns, `code` (PK) and `code_type` (CHECK in finding/basis/context), and no `tenant_id`.
- It has 7 rows written by the migration, the `reference_read USING (true)` policy, FORCE RLS, `ledger_deny_mutation` on UPDATE/DELETE/TRUNCATE and a SELECT-only grant.
- It is genuinely family-R reference data with no tenant or personal data, the same precedent as `ledger_adjustment_reason_codes`.
- The edit only touches tests and is needed, because without it T-ID-3 would flag the 7 visible rows.

## Mutants I re-ran myself (6/6 KILLED; I wrote 4 of them as new variants rather than replaying the implementer's)

Every file was restored and byte-compared against `git show 31e4501:<file>`: OK for all six. The control run passed first.

| ID | Mutation | Verdict | Killing test |
|---|---|---|---|
| SM1 | `resolvesTo` holder keyed on `"deposit\x00"` (wrong kind) | KILLED | TestK3_C42b (run 0: got 0 unconfirmed) |
| SM2 | pin removed from `payment_manual_resolutions_guard` | KILLED | TestK3_Y01 ("18 functions but 17 SET search_path clauses") |
| SM3 | `requested_by_person_id` added to the mutable exclusion list | KILLED | TestK3_Y03 (fall-through refusal detected) |
| SM4 | GREATEST changed to LEAST (pinned required count) | KILLED | TestK3_Y04 |
| SM5 | system read admits `state IN ('executed','pending')` | KILLED | TestK3_T3_ |
| SM6 | denied audit gets `err.Error()` instead of SQLSTATE | KILLED | TestForceResolutionAPI_T12 (both the MR003 and the no-message-text assertions) |

## Scope of this review

**In scope:** the K3 fix delta 041fb55..31e4501 (0115 up, recon matcher, HTTP denied audit, init-app-role merge, allowlist edit) and runtime-role probes against 0112/0113 helpers where K3 paths reach them.

**Not in scope:**
- A full re-review of E1 (0114) or of unchanged K3 code.
- A full audit of TRIGGER-SEARCH-PATH-1 across 0026..0113: only the K2 guards were proven bypassable, and the rest is not enumerated.
- The down migration: I did not run it myself; I relied on C-16/T-18 as reported.
- `init-app-role.sql`: I did not execute it, because it provisions roles. The check was static plus the live grants from the migrations.
- Penetration or certification testing.
