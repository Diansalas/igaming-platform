# NA-3 record: CAS-HEALTH-FAILOPEN-1 and CAS-JURIS-AUDIT-1

Branch na3-casino-health-audit (from claude/focused-wright-jw88w9 7dd0b51). No migration. IMPLEMENTED (provider-independent; mock-driven tests).

## Changes
- internal/casino/orchestrator.go: LaunchGame's provider health snapshot now fails closed. A HealthStatus error returns launchFailed(LaunchFailureHealthUnavailable = "health_unavailable", ErrProviderUnavailable): session revoked, one casino.launch_failed audit row (closed reason only, error text not persisted), no provider Launch call, no ledger write. Circuit-open path unchanged. HealthStatus is called only on the launch path (grep: no bet-path use; the inbound bet callback is not health-gated), so "bet gating" has nothing further to change.
- Same file: auditJurisdictionDenial writes casino.launch_denied_by_jurisdiction in the Phase A transaction when evaluateJurisdictionBlocklist denies (blocked or unresolved). Idiom copied from evaluateAndAuditRisk: actor system, target player_account/<uuid>, outcome denied. Metadata allowlist: reason_code, brand_id, provider_id, game_id, asset_code, mode, plus jurisdiction_code only when resolved. No PII, IP or user agent. An audit write failure aborts the tx (fail closed).
- internal/casino/mock.go: MockCasinoProvider.SetHealthError (fault injection, MOCK).
- No migration needed: audit_log.action is free text (no CHECK/allowlist); no alert kind added.

## Decision on "idempotent on retry"
Existing RG/risk denials write one row per denied attempt. I followed that: a retried launch is a new attempt, denied identically, mints nothing, writes exactly one more row; N concurrent attempts write exactly N rows. If dedup per (player, game) is wanted it needs a unique key, i.e. a migration; not done.

## Tests (internal/casino/na3_health_audit_integration_test.go, all new, no existing assertion changed)
HealthStatus error fails closed (revoked session, one launch_failed row, secret not in audit, no ledger entries, recovery launches normally); 8 concurrent launches all fail closed with no active session; jurisdiction denial real+demo writes exactly one row with expected shape/allowlisted metadata, no ledger/session, invisible to another tenant under RLS; retry and 8-way concurrency row counts.
Note: the "blocked" (resolved) branch is not reachable via LaunchGame today (no player jurisdiction producer, HDR-J-3); it is covered by the shared helper path only, and TestEvaluateJurisdictionBlocklist.

## Commands
- gofmt clean; go vet and go vet -tags integration ./internal/casino ./internal/httpserver clean.
- Private DB: go test -tags integration -count=1 -p 1 ./internal/casino: ok (102s, full package). Unit: ok.
- ./internal/httpserver -run 'Casino|Launch' (integration): ok (68s).
- internal/audit untouched.

## Review
security review of the audit metadata allowlist and the fail-closed change still to be requested by the orchestrator.

## Proposed registry text
CAS-HEALTH-FAILOPEN-1 and CAS-JURIS-AUDIT-1: CLOSED (NA-3). HealthStatus error fails closed (reason health_unavailable); jurisdiction denial audited as casino.launch_denied_by_jurisdiction, one row per denied attempt.
