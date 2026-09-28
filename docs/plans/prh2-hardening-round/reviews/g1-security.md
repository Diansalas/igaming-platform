# Security review — PRH-2 G1 (2026-09-28)

**Reviewer:** `security` (hard gate under ADR 0104 rev 2 §8 step 3). The orchestrator recorded this review.

**Scope:** commit `ac073d8` (`prh2-g1-audit-tenant`, based on `b433454`), exported with `git archive`. Tests ran on a private
DB (`sec_g1_rv_20260928`); mutants were applied only there, as live policy edits. The DB was dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

- RLS isolation holds.
- Only a validated platform principal can write a subject row.
- There is no new write power into tenant scope.
- The presentation is fail-closed: no IP, user agent, request id or email.
- The approval chain is built only from RLS-filtered rows.
- The display-name endpoints are tenant-bound.

G1 may merge. **G1-C1 must be resolved before KS-AUDIT-TENANT-1 is marked IMPLEMENTED.** G1-C2 and G1-C3 are small.

## Verified

**Migration 0109**
- The CHECK `subject_tenant_id IS NULL OR tenant_id IS NULL`, plus a partial index.
- `subject_tenant_read` is FOR SELECT only. It requires equality with `app.tenant_id` and excludes player, platform-admin, platform-service and both `app.acting_*` sessions.
- `audit_log_subject_actor_guard` matches C-104-1:
  - the platform GUC is required and all other GUCs must be unset;
  - `actor_type = 'staff'`, with the actor equal to the principal;
  - the principal is a platform `staff_users` row;
  - there is no status check (Q7).
- The `display_name` CHECK: 1–100 characters, `[[:cntrl:]]` (it matched U+0007 and U+0085 in `C.UTF-8`), and exactly U+202A–202E, U+2066–2069 and U+200B–200F. Go's `ValidateDisplayName` applies the same rule.

**Write path**
- A Go guard refuses `TenantID` and `SubjectTenantID` together.
- All five kill-switch audit sites set the subject only from the route-validated `c.target`, when the caller is platform, under `WithPlatformAdmin`.

**Read path**
- `RequireTenantScope` + `PermAuditRead`, and a nil-tenant check.
- It runs under `WithTenant` with an explicit filter.
- The SELECT never fetches `ip_address`, `user_agent` or `request_id`.
- The actor is shown as `staff_id` + `display_name`. The name lookup selects only `id, display_name`, for actor ids from the filtered rows; email is never selected.
- The per-action allowlist covers the four kill-switch actions.
- Free-form metadata is off by default and on resolver error (tested over HTTP).
- The chain is built from the in-memory filtered rows only.

**Display-name endpoints**
- On the tenant route, another tenant's row or a platform row is invisible under RLS, so the result is 404.
- The self route takes the target from the token subject.
- The UPDATE is column-allowlisted.
- The audit records before, after and self.

**Mutation evidence:** 9 in total, 8 KILLED and 1 unreachable by construction; the author's "7 + 1" undercounts. The reviewer re-killed:
- **#1**, equality replaced by "tenant GUC set": killed by `TestSubjectTenantRLS_TIHeadline`;
- **#3**, the `app.acting_*` exclusions dropped: killed by `TestSubjectTenantRLS_ExcludedSessions`.

**Local runs (not CI):**
- build ok;
- `-race -tags integration`: audit and identity ok;
- httpserver `-run 'PlatformActions|StaffDisplayName|KillSwitch'`: ok (39 PASS).

The full httpserver `-race` suite was not run.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| **G1-C1** | Low-Med (condition) | The down refusal is incomplete. `staff_users` is FORCE RLS, and the down's `EXISTS (… display_name IS NOT NULL)` runs with no GUC, so it sees only platform staff. This was proven with a rolled-back fixture: a tenant staff name is invisible to the check, so `down` would silently drop tenant names, contrary to ADR 0104 §3. The audit trail keeps the history. | **Orchestrator choice: (a).** Make the check see every row by looping per tenant with `set_config('app.tenant_id', …, true)`, after reading `tenants` under the platform GUC. Add a MIG test that uses a **tenant** staff display name and asserts that down refuses. |
| G1-C2 | Low | The platform self-rename runs under `WithoutTenant`, not `WithPlatformAdmin(subject)`. | Use `WithPlatformAdmin(subjectID)` when `tc.TenantID == uuid.Nil`. |
| G1-C3 | Low | U+2060, U+FEFF and U+061C still pass both Go and the DB. | Add U+2060–2064, U+FEFF and U+061C to both the CHECK and `isBidiOrZeroWidthFormatRune`, or refuse the `Cf` category in Go and mirror it in the DB. |
| G1-I1 | Info | A platform caller renaming a tenant's staff writes `TenantID = target`. That is the PLAT-AUDIT-SUBJECT-1 divergence. | Track it under PLAT-AUDIT-SUBJECT-1. |
| G1-I2 | Info | The approval chain is built per page, so a paginated chain may be incomplete. Disclosure is still fail-closed. | Optional. |
| G1-I3 | Info | The evidence count is 8 killed + 1 unreachable = 9. | Recorded here. |

## Interaction with the K1 restrictive fence (ADR 0099 C-1)

G1 and K1 compose cleanly, and no G1 change is needed.
- K1's `audit_log` restrictive policies are ANDed with `subject_tenant_read`, which already excludes acting sessions.
- G1's guard refuses subject rows under `app.acting_*`, and the platform-only CHECK prevents an acting row from carrying a subject.
- `subject_tenant_read` requires a positive `app.tenant_id` equality, so K1's A-18 NULL-arm replay should not flag it.

**K1 must carry:**
- A-17's baseline for "the effective policy sets after down" is the **post-0109** set, including `subject_tenant_read`.
- A-18's matcher must treat `subject_tenant_read` as a non-NULL-arm policy.
