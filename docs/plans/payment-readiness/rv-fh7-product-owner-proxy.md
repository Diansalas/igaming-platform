# FH-7 Product-Owner-Proxy Final Scope Review — Financial Hardening / Payment Readiness (PRH)

Reviewer: `product-owner-proxy` (read-only, 2026-09-28, tree at `7f5d0fb` on `claude/focused-wright-jw88w9`).
Committed verbatim by the orchestrator (the reviewer has no write/shell access), followed by an orchestrator addendum closing the reviewer's tooling caveat.

**Verdict: APPROVE WITH CONDITIONS**

Conditions (none are scope violations; all are pre-existing open items that should be tracked to closure, not new asks):
1. Close or explicitly re-triage `PAY-SEC-S-H1` (registry: OPEN — HIGH) and `PAY-SEC-S-M1` (OPEN — MEDIUM) before treating the callback and payout security rounds as done — the registry still shows them OPEN.
2. `KS-AUDIT-TENANT-1` (launch-blocking, NOT IMPLEMENTED) must stay on the pre-launch gate list.
3. Tooling caveat: the reviewer could not run `git log`/`git diff --stat`; findings are based on document cross-referencing. Recommended the orchestrator run a git-level out-of-scope check (done — see addendum).

## Findings

| ID | Severity | Evidence | Recommendation |
|---|---|---|---|
| POP-1 | Info | `task-registry.md` PRH scope statement: F-POOL-1/2, payment financial safety, KYC-ENFORCE-1, PAYWH-RL-1, PROVIDER-REF-BOUND-1, PROV-OUTBOUND-CRED-1, payment contract, capability/kill-switch, reconciliation (MOCK); excludes real vendor, AWS/Terraform, Bonus Wave 4, AI agents | No action — correct, tight scope |
| POP-2 | Info | `deploy/aws/**` predates PRH (Stage 9.4, S94-H01); no PRH task references `deploy/`, `.tf`, or Terraform | Confirm with git (addendum) |
| POP-3 | Info | No bonus/wave-4 or AI-agent task or file in the PRH records | Out-of-scope areas untouched |
| POP-4 | Low | ADR 0096 §9 "Labels" still reads "NOT IMPLEMENTED" while the header and §15 say IMPLEMENTED (PRH-I3) | Add a one-line pointer in §9 to §15 so §9 is not misquoted as a current label |
| POP-5 | Info | KYC labels: ADR 0096 header "ACCEPTED — IMPLEMENTED, pending review"; PRH-I2 KYC part "IMPLEMENTED … not yet gate-reviewed"; PRH-I3 "PARTIALLY IMPLEMENTED" | Conform to CLAUDE.md vocabulary; no overclaim |
| POP-6 | Info | PRH-I4 (webhook rate limiting) "PARTIALLY IMPLEMENTED" because PRH-I4-METRICS-1 is NOT IMPLEMENTED and WEBHOOK-EDGE-1 is open; admission control has a security APPROVE | Correct, conservative |
| POP-7 | Info | PRH-REF "IMPLEMENTED; security agreement + review PENDING"; PROV-OUTBOUND-CRED-1 "PARTIALLY IMPLEMENTED" with tripwire `TestOutboundPrecondition_EveryWiredAdapterIsSynthetic`; PRH-I5 "IMPLEMENTED against a MOCK source; real PSP statement matching PROVIDER DEPENDENT" | Correct, no fake completion |
| POP-8 | Info | Deferred items present with owner and trigger: LEDGER-SUSPENSE-B-1, DEVOPS-0107-INDEX-WINDOW-1, WD-RG-1, PRH-I1-MANIFEST-1..4, KS-AUDIT-TENANT-1, PAY-RECON-N1, PAY-PSP-CONTRACT-INVDEP1, PAY-POLL-AMOUNT-1, PAY-SWEEP-CAS-NOISE-1, PRH-REF-C1..F4 | Nothing lost |
| POP-9 | Info | ADR 0095 AM-1 replaced a two-route-family kill-switch design with one dual-scope family ("splitting later is additive") | A scope reduction, correctly recorded |
| POP-10 | Medium (tracking) | PAY-SEC-S-H1/S-M1 still OPEN in the registry | Orchestrator to reconcile: close with evidence or carry forward explicitly |

## Per-check summary

1. **Scope creep:** none found. Everything traces to a Blueprint/financial-integrity/security requirement or an explicit human decision (HD-LEDGER-UNALLOC-1). AM-1 reduced scope.
2. **Out-of-scope items touched:** none referenced in PRH records (git confirmation in the addendum).
3. **Labels:** KYC (PRH-I2/I3, ADR 0096), webhook rate limiting (PRH-I4), provider-reference bound (PRH-REF), outbound credentials, reconciliation (PRH-I5) all use the exact vocabulary and are conservatively qualified. One stale sub-section label (POP-4).
4. **Deferred items:** all recorded with owner and trigger (POP-8).
5. **Overengineering:** none. INV-DEP-1 (single choke point, DB backstop index, reconciliation kind) is the minimum for the ledger rules; option B deferred as decided.

## Orchestrator addendum (2026-09-28) — git-level out-of-scope check (closes condition 3)

Range: first PRH commit `1560ad0` ("docs: register … (PRH)", 2026-09-27) through HEAD; 328 files changed.
- `git diff --stat 1560ad0^..HEAD -- deploy/ '*.tf' '*.tfvars' internal/bonus/ .github/` touches exactly three files:
  - `.github/workflows/ci.yml` — **adds** must-PASS test names (0099, 0102, 0104 migration and grant tests) to the integration job's name guard. This strengthens CI; nothing is skipped, removed or loosened.
  - `deploy/init-app-role.sql` — least-privilege table grants for the new PRH tables (`payment_statement_imports`, `payment_statement_lines`, `payment_kill_switches`), the repository's standard local/CI role script (commits `16c69b7`, `ad2830d`, `3c3bfde`, `c5a744b`). Not AWS, not Terraform, no role/password change.
  - `internal/bonus/wave3_phase2_migrations_integration_test.go` — a migration chain-tip test that now derives migration counts instead of hard-coding them (`df0a0b2`, plus the PRH-REF commits `43f5071`/`84f5051`); test only, no Bonus Engine code, no Wave 4.
- No Terraform (`*.tf`/`*.tfvars`), no `deploy/aws/**`, no AI-agent path changed.
- Conditions 1 and 2 are handled in the FH-7 registry reconciliation (architect final review) and the completion report.
