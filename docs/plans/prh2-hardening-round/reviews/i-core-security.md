# Security review — PRH-2 I-core (2026-09-28)

**Reviewer:** `security`. The orchestrator recorded this review.

**Scope:** commit `bc73c24` (`i-core-alerting`, based on `b433454`), exported with `git archive`. The migration is `0108_durable_alerting`, renumbered to 0110 at merge.
Tests ran on a private DB (`sec_icore_rv_20260928`); mutants were applied only there. The DB was dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

The access-control design matches ADR 0102 rev 2:
- tenant isolation holds;
- tenants cannot write or forge platform-scope or meta alerts;
- the dispatcher is confined to meta-Kind inserts plus delivery rows;
- the down migration's RLS disable is transactional;
- grants are least-privilege;
- no recipients are seeded.

I-core may merge as an **unwired** package. **IC-1 and IC-2 must be met before I-wire merges.** The orchestrator
routed IC-1 to IC-4 to I-core itself.

## Verified

**Tables and RLS**
- All five tables are ENABLE + FORCE RLS.
- `alert_kinds` is seeded before FORCE, SELECT-only, and trigger-protected.
- CHECKs pin the three meta-Kinds as `requires_subject = false` and never `in_tx_raisable_by_tenant`.
- Every family uses the NULLIF form and the common exclusion set, including both `app.acting_*` (C-102-9).
- `alerting_session_scope()` refuses acting, player, unknown-service and mixed sessions (SR-1).

**Tenant isolation**
- A tenant reads only its own rows and its subject rows.
- A subject raise requires `subject_tenant_id = app.tenant_id`.
- The occurrence trigger reads the parent alert under RLS.
- Tenants have no `alert_routes` policy.

**Tenants cannot write platform-scope or meta rows**
- A guard trigger forces `tenant_id IS NULL` for platform-scope Kinds.
- Meta-Kinds are never tenant-raisable, so `alerting.raise_failed` cannot be forged by a tenant.
- The `raise_failed` payload is locked by trigger: exactly `{kind, sqlstate_class}`, a validated `sqlstate_class`, and a forced discriminator.

**Dispatcher identity (`alert_dispatcher`)**
- SELECT on all tables.
- INSERT on `alerts` and `alert_occurrences` only for meta-Kinds with NULL tenant and NULL subject.
- INSERT on `alert_deliveries`.
- No UPDATE or DELETE policy anywhere; the Go code issues only INSERTs.

**Go core**
- `RaiseGuarded` validates before the savepoint, and the savepoint covers only the insert or attach.
- The swallow allowlist is exactly classes 22 and 23, plus 42501 and P0001.
- `InTx` flushes only after a nil commit.
- `RaiseDetached` reopens the originating `ScopedRunner`, with a fallback under the dispatcher identity that does not recurse.
- N-1 is honoured.

**Other checks**
- The down's RLS disable runs inside the one migration transaction, so there is no RLS-off window.
- Grants: no DELETE or TRUNCATE, with REVOKE ALL first.
- `PermAlertManage` is platform-admin only.
- HD-PRH2-4 holds: no route is seeded, `recipient_ref` refuses `@` and phone shapes, and `channel_kind` is `log`/`mock`.

**Local runs (not CI):**
- `-race -tags integration ./internal/alerting/...`: ok, 42 PASS, no DATA RACE.
- auth and db: ok.

## Mutants re-run

| Mutant | Result |
|---|---|
| `alerts_subject_tenant_read` with its whole exclusion set removed (the evidence called it "not attempted") | **SURVIVED.** The real policy works: a probe showed 0 rows under the real policy and 1 under the mutant. But nothing pins it (IC-1). |
| Tenant equality removed from `alerts_subject_tenant_read` | Verified by source reading only (a tooling failure): `rls_integration_test.go:64-71` would fail. |

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| **IC-1** | Medium (before I-wire) | The exclusion set is untested for every family. `TestRLS_MixedGUCSessionSeesNothing` seeds no row, so it is vacuous. | For `alerts_tenant_owned`, `alerts_subject_tenant_read` and `alerts_subject_tenant_raise`, on both `alerts` and `alert_occurrences`: seed a row the pure tenant session sees (assert 1). Then add each of `app.acting_tenant_id`, `app.acting_platform_principal_id`, `app.platform_service_id`, `app.platform_admin_principal_id` and `app.player_account_id`, and assert 0 rows or a refused insert. Do the same for the dispatcher family (dispatcher + tenant GUC sees nothing). Record the removed-exclusions mutant as killed. |
| **IC-2** | Medium (before I-wire; must be fixed before any real channel) | There is no stale-claim reclaim, and the deviation is undisclosed. `dueWork` treats a latest event of `claimed` as never due (`dispatcher.go:~291`). A crash between claim and outcome leaves an alert permanently undelivered, with no metric and no meta-alert. ADR 0102 §6.1 specifies a re-send after lease expiry. | Implement lease expiry: a stale `claimed` becomes due again, and the next attempt number is claimed. Add a clock-driven test. Until then, disclose it in ADR §16 and the runbook, and add `alert_stale_claims_total` or a p2 meta-occurrence. |
| IC-3 | Low | The `ServiceAlertDispatcher` confinement test scans only `internal/alerting`. | Scan every non-test Go file (allowing `internal/db`'s definition and the three alerting files), and forbid `PlatformService("alert_dispatcher")` conversions. |
| IC-4 | Low | `alerts_tenant_owned` is `FOR ALL`; ADR §4.1 says there is no tenant UPDATE in PRH-2. The trigger still refuses. | Split it into `FOR SELECT` + `FOR INSERT`. |
| IC-5 | Low (I-wire condition) | `RaiseDetached` does not detach the context itself, and a scope mismatch does not use the fallback. | I-wire failure-path callers pass a detached, bounded context (the `deniedAuditCtx` pattern). Optionally, route a scope mismatch to the fallback as `go_validation`. |
| IC-6 | Info | The evidence claims "subject-read without exclusions" is equivalent to M8, but that mutant survives. | Update the evidence after IC-1. |
| IC-7 | Info | Test DBs grant DELETE by default privileges, which is broader than production. | None. Test results are not grant verification. |
