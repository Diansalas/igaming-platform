# Security re-review — PRH-2 I-core (2026-09-28)

**Reviewer:** `security`. The orchestrator recorded this review.

**Scope:** `bc73c24..9f5970c` (`90b4bc3` renumber, `8a8412e`, `9f5970c`), excluding the merged main content. Tests ran on a private DB (`sec_i2_rv_20260928`), dropped afterwards.

## Verdict: ACCEPT

IC-1 to IC-5 and F-12 are resolved. IR-1 and IR-2 are Low and should land with or before I-wire.

**IC-1: closed.**
- The full exclusion matrix covers both tables.
- The removed-exclusions mutant on `alerts_subject_tenant_read` is **KILLED**, on all 5 GUCs.
- `alerts_tenant_owned` tested only negatively is acceptable for PRH-2: no tenant-owned Kind exists, and the guard trigger refuses `tenant_id IS NOT NULL` for every Kind.
- **Carried condition:** the migration that adds the first tenant-owned Kind must add the positive-then-negative matrix for `alerts_tenant_owned` (SELECT and INSERT, both tables). Recorded under ALERT-DELIVERY-1.

**IC-2: closed.** A stale claim, at least `ClaimLease` old (default 2 min, configurable), becomes due again at `attempt_no + 1`. `alert_stale_claims_total` counts it. Delivery stays at-least-once.

**IC-3: closed** (with IR-2). The static test walks the whole repo and bans the literal conversion.

**IC-4: closed.** The tenant family is `FOR SELECT` + `FOR INSERT` only.

**IC-5: closed.** `RaiseDetached` detaches at entry, and every retry, sleep and fallback uses the detached context.

**`freshReadCommittedRunner` cannot widen scope or drop the principal.**
- It maps from the runner the transaction was opened with, never from the Alert. There is no service or dispatcher path.
- Recommendation: make `ScopeTenant` an explicit case, and have `default` refuse.

**SR-5 is enforced in code.** A non-READ-COMMITTED raise is deferred to the post-commit fresh READ COMMITTED path.

**F-12:** 0110 carries an idempotent `pg_roles`-guarded grants block that matches `init-app-role.sql`. Least privilege holds.

## Remaining (Low)

| ID | Finding | Recommended change |
|---|---|---|
| IR-1 | The raise-family occurrence negative is vacuous: it probes with a random alert id, which the occurrence trigger always refuses. A mutant stripping the exclusions from `alerts_subject_tenant_raise ON alert_occurrences` survives. There is no real exposure, because `alerting_session_scope()` independently raises for all five probe GUCs. The "dispatcher + tenant GUC sees nothing" case in `TestExclusionSets_TenantOwned_Family` seeds no alert. | Probe against a committed seed alert's id. Seed a visible alert before the dispatcher+tenant count. Record the mutant as killed. |
| IR-2 | The IC-3 static test bans only the literal `db.PlatformService("alert_dispatcher")`. | Also flag any `"alert_dispatcher"` string literal, and any `set_config(… 'app.platform_service_id' …)` in SQL literals, outside the allowed files. |

**Tests (not CI):**
- `-race -tags integration ./internal/alerting/...`: ok, 60 PASS, no DATA RACE.
- `TestStatic*`: ok.
