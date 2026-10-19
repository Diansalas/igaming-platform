# Licensing and jurisdiction completion report (2026-10-19)

Branch `claude/licensing-jurisdiction-completion` (pushed, no PR), built on the frozen provider-independent baseline `1505a165e9152729b5df573211c80b1c0a35bc87`, which is untouched. Migration head 0179. All providers MOCK. Nothing here is a legal opinion, a jurisdiction or licence approval, or a launch authorisation.

## 1. Verdict

- **Technical licensing capability: complete for the provider-independent scope**, with the exceptions in section 4 (listed, not hidden).
- **Does a legal/compliance decision prevent activating a particular brand?** Software does not block activation on legal grounds (owner decision 15: licensing is a manual per-brand decision; licence validity is an acknowledgeable readiness item). But **brand 1 cannot be *responsibly* activated for real money until you answer Q1-Q11** in `licensing-owner-decision-requests.md`, and until a production KYC vendor exists no production player can pass the market gate (class D). Until Q1 is answered no country is open, so every registration, deposit and wager is refused: the safe state.

## 2. What was audited (not from summaries)

Three read-only audits of code, migrations, routes, Back Office, ADRs, registry and the closure report: `licensing-audit-core.md` (G-1..G-16), `licensing-audit-enforcement.md` (G1..G17), `licensing-decision-triage.md` (L-1..L-33, classes A-F).

Requested capability, status at audit time -> now:

| Capability | At audit | Now |
|---|---|---|
| Own-licence tenants, B2B own-licence, hybrid (ADR 0006, composite FK, no universal flag) | IMPLEMENTED | unchanged, preserved; no preselected model/licence anywhere in the UI |
| Multiple brands per tenant, independent activation (ADR 0112) | IMPLEMENTED | unchanged; one licence per tenant, no per-brand licence (Q15) |
| Jurisdiction/country config and market restrictions | writers missing (L-24/L-25) | governed writers through the change primitive (0177); read APIs (0178) |
| Licence evidence, status, expiry, review dates, audit history | status/dates writable only by table owner; no history; no alert | writers, `licence_versions` history, hourly sweep and alerts (0176), governed (0178), attribution from the executed request (0179) |
| Controlled create/update with permissions and approvals | single actor | maker-checker: 2 distinct Persons, platform_admin only (INTERIM), requester cannot approve, 24 h freshness, signed actor proof |
| Brand activation/suspension/reactivation four-eyes | IMPLEMENTED (ADR 0112) | unchanged; new gates below |
| Enforcement at registration, deposit, gameplay, payment | market gate IMPLEMENTED; casino launch had no status gate | casino launch/bootstrap/demo status gate (lic3) |
| Fail-closed when unresolved | mostly; `rg` tenant jurisdiction fail-open; licence read error ignored in launchgov | `ResolveLicensingChain` shared helper; rg no longer fail-open; read error refuses |
| Auditable reasons | partial | audit actions on every writer; `casino.launch_denied_by_status`; reason codes validated |

## 3. What was missing and is now completed (commits, migrations, APIs, Back Office)

Merge commits on the branch (first-parent): audits `d714918`, `7ebba1a`, `812faf6`; lic1 `839a750`; lic2 `af4e18d`; lic3 `7d414cc`; reviews `d600295`, `81681d7`; lic4 `ba9b594`; lic6 `41a00f8`; lic5 `666adea`; lic7 `634d72d`, `9dc5b96`; lic8 `ebbe355`.

- **0176 (lic1):** licence lifecycle writers + `licence_versions` + hourly sweep (`licence.expired` P1, `licence.expiring` P2); launchgov hard failure `H-2:licensing_status_contradicts_licence_registry`.
- **0177 (lic2):** closed change-kind registry, `licensing_change_requests/approvals`; six kinds; licence create/bind now 202; `tenant_jurisdiction_configs` append-only. Routes `/v1/admin/licensing-changes` (list, get, approve, reject, cancel, file).
- **lic3 (no migration):** casino launch status gate (409 `TENANT_OR_BRAND_NOT_ACTIVE`); risk authoring refuses deposit and jurisdiction_code rules; `ResolveLicensingChain`; dormant-KYC WARN.
- **0178 (lic4):** four lifecycle kinds (status, validity dates, review-due, evidence), `licences_update_guard` (LC040/LC042), `licence_versions` insert via SECURITY DEFINER writer only, `licence.sweep_failed`/`licence.unmonitored`; five read APIs: `GET /v1/admin/jurisdictions/{id}`, `/licences/{id}`, `/licence-country-ceilings`, `/operating-country-policies`, `/tenants/{id}/jurisdiction-configs`.
- **0179 (lic6):** `change_request_id` attribution, DELETE revoked, licence_create pins, read-only paged reads, trigger-order pin test.
- **lic5 Back Office:** Licensing section (jurisdictions, licences + versions + ceilings, tenant licensing page, licensing changes with approve/reject/cancel, filing dialog), platform_admin only, no defaults, 202 shown as "Pending approval".
- **lic7 (no migration):** readiness items `R-RISK-RULES-LEGACY` and `R-KYC-POLICY` (acknowledgeable, never hard); `SnapshotVersion` 5 -> 6.
- **lic8:** security review fixes.
- **Docs:** ADR 0123, runbooks `licence-lifecycle.md`, `licensing-change-approvals.md`, `launch-and-suspension.md`; decision register row; task registry rows.

Behaviour changes to be aware of: reinstating a licence, extending or clearing expiry now answer 202 (pending approval); `reason_code` on licence PUTs must be a code; licence create returns no licence id synchronously; owner-path `licence_versions.changed_by` is NULL (break-glass marker); open v5 launch requests go stale.

## 4. Not implemented / residual (labelled)

- NOT IMPLEMENTED: Back Office *filing* of ceiling, operating-policy and tenant-jurisdiction-config changes (API-only; the screens list and approve them) and jurisdiction creation UI; exact LC0xx guard codes on the wire (UI explains in words).
- NOT IMPLEMENTED (needs an owner answer first): player-scoped jurisdiction resolver (L-4; HDR-J-3/7/8/9) - all jurisdiction-scoped player rules fail closed; residence maker-checker (L-15, Q21); automatic sanctions/PEP screening and transaction monitoring (enforcement audit G1, vendor-dependent); RG floors (Q4, enforcement OFF); KYC thresholds (Q6, dormant; the readiness item flags it).
- Unbound licences are not swept for expiry (D-2, security accepts). Tenants cannot read licence history (D-3 / Q16).
- The Back Office was tested with mocked API tests and a production build only; no browser run.
- The readiness items only detect; a raw runtime INSERT of a legacy rule remains possible (security F-9).
- `TEST-DCONC-INTERMITTENT-1` (frozen baseline) remains an open disclosed item.

## 5. Verification

- Full `go test -tags integration -count=1 -p 1 ./...` on a freshly migrated private database: **every package ok**, including httpserver (982 s) and payments (3794 s); run on the tree before lic8.
- After lic8: `go build ./...`, `go vet`, new unit test `TestCoarsenForTenant_DropsRiskRuleRefsAndCounts`, backoffice vitest 747/747 and production build ok.
- Hosted CI has NOT been run on this branch.
- Security: independent reviews of 0176-0177 (F-1 HIGH, F-2 MEDIUM reproduced, closed by 0178), re-review of 0178 (no Critical/High/Medium; D-1 sound), review of lic5 UI and lic7 (no blocker; F1 fixed). Ledger-finance review: C1 closed by lic7, C2 = Q12 (owner), C3 recorded in ADR 0123. Tenant isolation (FORCE RLS), ledger controls, RG controls and actor-proof protections were not weakened; the only existing assertions changed are listed in the slice records (nav test gained "Licensing", SnapshotVersion pin 5->6, owner-path `changed_by` expectation).
- Maker-checker verified by tests: one approver cannot bypass, requester cannot approve, unauthorised roles cannot approve or activate, ambiguous or unreadable decisions refuse.

## 6. Decisions needed and evidence required

Exact questions Q1-Q25 and the class C evidence list: `docs/plans/licensing-owner-decision-requests.md`. Gate brand 1: Q1 country list, Q2 residence vs location, Q3 registration of excluded markets, Q4 RG values, Q5 monthly limits, Q6 KYC thresholds, Q7 AML monitoring/officers, Q8 expiry boundary, Q9 approver counts, Q10 evidence reference, Q11 approver roles. Licence-lifecycle: Q12 withdrawals under suspension, Q13 auto-suspend, Q14 revoked, Q15 licences per tenant, Q16 tenant history.

## 7. Dependent items

- **Class D:** production KYC vendor (without it no production player has verified residence), geolocation vendor if Q2(b), PSP/casino/sportsbook credentials, screening vendor.
- **Class E:** runtime-role split, kill switch, backups, environment anchor in a deployed environment.
- Real providers and AWS stay deferred per instruction; PSP gate (Nuvei sandbox request/approval and PSP choice) is unchanged.

## 8. Next

Remaining provider-independent work continues in registry order: G1 AML trigger wiring and G8 residence maker-checker (both need owner answers Q7/Q21, safe interim stands), then residual registry items. Hosted CI on the branch should run before any merge to the main line.
