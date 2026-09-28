# Code review — PRH-2 G1 (2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** commit `ac073d8` (`prh2-g1-audit-tenant`, based on `b433454`), exported with `git archive`. The author's
uncommitted G1-C1/C2 edits were not reviewed.

## Verdict: NOT READY (at `ac073d8`)

F-1 is a confirmed correctness bug. It is the same as security's G1-C1. Once that fix is committed and F-2 to F-4 are
addressed, G1 could become READY WITH CONDITIONS on re-review.

**What holds:**
- up matches ADR §3.
- The Go guard refuses both tenant ids set together.
- The subject is taken from the path target, for platform callers only.
- The list query uses the tenant session plus an explicit filter, and the name lookup selects only `id, display_name`.
- The presentation fails closed.
- Every production `audit_log` reader (`admin_routes.go:1167`, `kyc/provider.go:672`, `noeffect.go:81,197`, `casino/rejections.go:260`) filters by an explicit `tenant_id`.
- The append-only triggers and `dual_scope_isolation` are untouched.

**Mutation count:** 9 = 8 claimed killed + 1 not run (unreachable by construction; the argument holds). Mutant 7 is synthetic,
but the SELECT never fetches ip, user agent or request id.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| F-1 | **High** | Same as security G1-C1. The down's `display_name` check cannot see tenant staff: FORCE RLS applies, and the migrator is not a superuser and has no BYPASSRLS. Reproduced: with a tenant staff row named, the no-GUC count is 0 and the down body drops the column. The existing test seeds only a platform admin. | Per-tenant check (orchestrator choice (a)), plus a tenant-staff refusal test. |
| F-2 | Medium | The trigger's `app.principal_id`, `app.player_account_id` and `app.acting_platform_principal_id` exclusions are untested. Mutant M1, removing all three, **survived**. A probe with the platform GUC plus `app.principal_id` inserted a subject row. | Add refusal cases, one per GUC set alone next to a valid platform GUC; each asserts P0001. |
| F-3 | Medium | Chain links via metadata `kill_switch_id` (`request_release`, and `cancel_release`'s new key) are untested. Mutant M2, disabling that branch, **survived**. | Assert `request_release` is in both the engage and approve chains. Add an engage → request → cancel scenario. |
| F-4 | Medium | ADR §7's "Readers unchanged (R)" tests are missing: `/v1/admin/audit-log`, kyc, noeffect and `casino/rejections.go` (SA-1), each with subject rows present. | Add them, each with at least one subject row for the same tenant. |
| F-5 | Medium (ADR conformance) | The chain is built only from subject rows (ADR §5.5 says "the tenant's own rows; subject rows for that tenant"), and only from the current page. | **Orchestrator choice:** conform to the ADR. Build chains from every RLS-visible row (tenant-own plus subject) for the chain keys on the page, in the same tenant session. |
| F-6 | Medium (no fake completion) | The architecture and security docs already say "IMPLEMENTED — kill switch only". The ADR header contradicts §12. | Relabel as "IMPLEMENTED (local; pending security diff review C-104-6 and orchestrator merge)", and make the header and §12 agree. The orchestrator sets the final status. |
| F-7 | Low | `TestSubjectActorGuard_AppendOnlyStillHolds` claims TRUNCATE coverage but runs only UPDATE and DELETE. | Add TRUNCATE, or correct the comment and §12. |
| F-8 | Low | (a) A platform rename of tenant staff writes a platform actor's row into tenant scope (the PLAT-AUDIT-SUBJECT-1 shape). (b) Neither platform rename path asserts its audit row or `before`/`after`. | Record (a) under PLAT-AUDIT-SUBJECT-1. Assert the audit row, with before and after, on both platform paths. |
| F-9 | Low | There is no G1-specific "route target" test. | Assert the stored `subject_tenant_id` equals the path target, plus a body-with-`tenant_id` → 400 case. |
| F-10 | Info | `ShowNetworkMetadata`/`ActorPresentation` are unused seams (ADR §5.3). | None required. |

## Commands (local, private DB `cr_g1_review`, dropped)

- F-1 reproduced: the migrator is `igaming|f|f` (not superuser, no BYPASSRLS); the no-GUC count is 0; the down body completed inside `BEGIN…ROLLBACK`.
- `-tags integration`:
  - audit and identity ok;
  - httpserver focus tests ok (36 PASS);
  - full httpserver, casino, kyc and `cmd/platform-api`: ok.
- build, vet and lint: 0 issues.
- Mutants: M1 **survived** (F-2), M2 **survived** (F-3), M3 KILLED.
