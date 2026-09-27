# RV-PRH-I2 (casino) — Security review of the casino launch two-phase split

- Reviewer: `security` specialist
- Date: 2026-09-27
- Subject: ADR 0095 §15.1 / §15.1.1 implementation record; commit `224c532` (merged in `008ac0c`); reviewed at HEAD `5a1ae02`
- Verdict: **APPROVE WITH CONDITIONS** (C1–C4 below). No finding blocks this change at the current MOCK-only stage. C1 and C2 must be closed before any real casino adapter is wired or before production launch authorization, whichever comes first. C3 and C4 must be closed before a real casino adapter is registered.

## Scope

In scope: `internal/casino/orchestrator.go` (`LaunchGame` phases A/B/C), `internal/casino/launch.go` (`CallContext`, `OutboundCredentialResolver`, `RevokeLaunchSession`, token mint/resolve), `internal/casino/mock.go` (`MockOutboundResolver`), `internal/providercred/outbound.go` (`NewMockOutboundCredential`, real `OutboundResolver.Resolve`), `cmd/platform-api/registrations.go` and `main.go` wiring, `internal/httpserver/casino_handlers.go`, and the bet-path revoked-session gate (`postBet`).

Out of scope: payments/PRH-I1 and the KYC PRH-I2 slice, a real casino adapter (none exists), penetration testing, and the internals of the vendor-side protocol. This review does not declare the casino launch path "secure" in general. It covers what the diff changes.

## Verified (no finding)

1. **Can a mock credential reach production?** Not today. `casinoOutboundCredentials()` returns `MockOutboundResolver` only when `b.Casino` (the `*casino.MockCasinoProvider`) is non-nil. That provider is always constructed and always registered with `RefuseSyntheticInProduction` as a `Synthetic` component. The guard runs right after `config.Load()` and before `db.Connect`, and it uses `GuardEnvironment()`, so a missing `APP_ENV` is treated as production. A production process therefore refuses to boot before the mock resolver can be used. `MockOutboundResolver` carries the `SyntheticComponent` marker. `NewMockOutboundCredential` is `KeyID/Fingerprint="mock"`, `HandleID=uuid.Nil`, and holds a fixed, non-configurable placeholder secret. See C3 for the defense-in-depth gap.
2. **Per-call resolution outside any tx.** Phase A commits (`pool.WithTenant` returns) before `HealthStatus`, `outbound.Resolve` and `provider.Launch` are called. The real `OutboundResolver.Resolve` also refuses under `txscope.Held(ctx)`. `TestLaunchGame_NoConnectionHeldAcrossHealthAndLaunchCalls` pins this on a single-connection pool. The credential is resolved fresh on every call and stored in no long-lived type. `TestOutbound_NoCredentialOnLongLivedTypes` was extended to cover `MockOutboundResolver`.
3. **Tenant/provider binding.** After `Resolve`, `LaunchGame` checks `cred.TenantID == params.TenantID && cred.ProviderID == providerID && cred.Domain == "casino"`. On a mismatch it fails closed through `launchFailed`, which revokes and audits. The mutation evidence covers this (M5). The real resolver returns only the sentinel `ErrOutboundCredentialUnavailable` and is nil-receiver safe, so if `Outbound("casino")` ever returns a typed nil it still fails closed.
4. **Launch token minting and scoping are unchanged.** `generateLaunchToken`, `CreateLaunchSession`, `hashLaunchToken` and `ResolveLaunchToken` (single-use CAS `active→consumed`) are unchanged by the diff. The token goes only to `provider.Launch`. It is not in any audit metadata and not in `CallContext`.
5. **Revoked-session tombstone.** `RevokeLaunchSession` is `UPDATE … SET status='revoked' WHERE id=$1 AND status='active'` (CAS). `ResolveLaunchToken` rejects any non-active status. `postBet` rejects `LaunchSessionRevoked` with `ErrLaunchSessionRequired`. Spot-check mutation B below confirms a test kills removal of that gate.
6. **Phase-A denial audit.** A denial from RG eligibility or Risk & Limits returns `nil` from the phase-A closure through the `denied` result, not as a Go error. The denial audit row written by `evaluateAndAuditEligibility`/`evaluateAndAuditRisk` therefore commits, and no session is minted. Mutation evidence M1 covers this.
7. **Phase-C failure paths revoke.** The following all route through `launchFailed` (revoke plus a `casino.launch_failed` audit), subject to C1:
   - a provider missing from the registry
   - circuit-open health
   - a nil outbound resolver
   - a resolver error
   - a binding mismatch
   - a transport error
   - a non-`Succeeded` outcome (declined or ambiguous)
8. **Health snapshot.** `HealthStatus` runs after phase A has committed, under no transaction. The mock does no I/O.
9. **Cross-tenant.** `TenantID` comes only from `tc.TenantID` (the authenticated context). The player and wallet are read under that tenant's RLS. Phases A and C and the resolver all use `params.TenantID`, and the binding check backs this up. There is no client-supplied tenant or provider on the launch path.
10. **Errors to client.** The resolver and credential errors (`ErrProviderUnavailable` → 503, and otherwise a generic 500) never include credential material. `launch_failed.reason` holds only sentinel text or the vendor's decline reason.

## Conditions

### C1 — MEDIUM — Phase C failure path can silently skip the revoke and the audit, leaving an orphan session that can still take bets

`launchFailed` runs `pool.WithTenant(ctx, …)` with the request context and discards every error:

```go
_ = pool.WithTenant(ctx, params.TenantID, func(ctx context.Context, tx pgx.Tx) error {
    _ = RevokeLaunchSession(ctx, tx, session.ID)
    return audit.Record(ctx, tx, audit.Entry{ … "casino.launch_failed" … })
})
```

Failure scenario: a real adapter's `Launch` times out because the HTTP client disconnected or the request deadline passed. `ctx` is already cancelled, so the phase-C transaction never starts. Result: no revoke, no `casino.launch_failed` row, and no log line. The same happens if `RevokeLaunchSession` hits a DB error, because the aborted transaction makes the audit fail and the whole error is discarded. The phase-C success branch has the same gap: if the success audit fails, the function returns an error to the player but never attempts a revoke, and the vendor has already accepted the launch.

The doc comment says such a session "simply expires (harmless)". That is not accurate for bets. Expiry is only enforced at token resolution. `postBet` rejects only `status='revoked'`, not expired, active or consumed sessions. An orphaned `active` session therefore stays bet-eligible for any authenticated callback from that vendor, with no time limit. Bets are still ledger-accounted, so this is not a ledger-integrity break. But the player was told the launch failed, and the audit trail shows a `launch_requested` with no terminal outcome.

Required:
- Run phase C under `context.WithoutCancel(ctx)` with a short bounded timeout.
- Log any revoke or audit failure through a redaction-safe, operator-only log, without the token.
- Check `RowsAffected` and record `revoked: true/false` in the audit metadata.
- Attempt a revoke on the success-audit-failure branch too.
- Correct the "harmless expiry" claim in the `LaunchGame` doc comment.
- Add a test that cancels `ctx` inside a stub `Launch` and asserts the session ends up `revoked` and audited.

### C2 — MEDIUM — `CallContext` redaction renderers are untested (spot-check mutation A survived)

`CallContext` implements `String`, `GoString`, `Format`, `LogValue` and `MarshalJSON`, and by inspection all five delegate to the redacted `OutboundCredential.String()`. However, no test in the repo exercises any of them. Mutation A appended `string(c.Credential.Secret())` to the redacted rendering, and the full `internal/casino` and `internal/providercred` suites (with `-tags integration`) still passed.

Failure scenario: a later edit to `callContextRedacted`, or a new field such as a derived bearer token added to `CallContext`, leaks the vendor secret into logs, errors or JSON, and CI stays green.

Required: add a unit test that builds a `CallContext` around a credential with a known sentinel secret. It must assert the sentinel is absent from:
- `%v`, `%+v`, `%#v`, `%s` and `%q` of both `CallContext` and `LaunchRequest`
- `slog` text and JSON handler output (`slog.Any`)
- `json.Marshal` of both types
- `fmt.Errorf("%v", …)`

The test must include a negative control.

### C3 — LOW (must close before any real casino adapter) — The outbound resolver is chosen by whether any mock exists, not per adapter, and it is not registered with the guard

`casinoOutboundCredentials()` returns the mock resolver for all casino providers whenever `b.Casino != nil`. Failure scenario: a real adapter is later registered alongside the mock in a non-production environment such as staging or sandbox. The real adapter then receives the synthetic credential, and the real `Outbound("casino")` branch is dead code. In production the guard still refuses to boot because `b.Casino` is registered, so this is not a production leak today. However, `MockOutboundResolver` itself is never passed to `buildRegistrations`. Its safety rests entirely on the mock provider's registration.

Required:
- Register the returned resolver in `buildRegistrations` (`{Domain:"casino", Name:"outbound_resolver"}`).
- Before a real adapter lands, key the choice on the adapter's identity, the same kind-split pattern `casinoOrchestratorResolver` uses for inbound credentials.

### C4 — LOW (must close before any real casino adapter) — The revoke CAS misses a session the vendor already consumed

`RevokeLaunchSession` matches only `status='active'`. Failure scenario: a real vendor resolves the launch token in-band during `Launch`, which moves the session to `consumed`. The vendor then returns a transport error, or the HTTP response is lost. The revoke matches zero rows and the `consumed` session stays bet-eligible, even though the player was told the launch failed. The CAS and the discarded `RowsAffected` make this invisible.

Required, pick one:
- For a failed launch, revoke `WHERE status IN ('active','consumed')`, or
- Record in ADR 0095 §15.1.1 that a vendor-consumed session is deliberately kept. In either case, audit the actual prior status.

## Informational (pre-existing, not introduced by this diff)

- **I1:** A jurisdiction-blocklist denial (`evaluateJurisdictionBlocklist`) writes no audit record, unlike the RG and Risk denials. The new phase-A comment claims it does. The pre-split code behaved the same way. Fix the comment, or add the audit record in a later jurisdiction task.
- **I2:** An error from `HealthStatus` fails open (the launch proceeds), and it did before the split too. Acceptable while `HealthStatus` is an in-memory contract. Revisit for a real adapter.
- **I3:** A bet on a session revoked concurrently with the bet can still commit, because `GetLaunchSessionByID` is a plain `SELECT` with no row lock. The bet stays ledger-accounted. Record as a known, bounded race.

## Spot-check mutations (throwaway, reverted, `git diff` clean after each)

These ran against the shared `igaming_platform_ci_local` DB using `go test -tags integration`. The suites seed fixtures per tenant and make no schema changes.

| # | Mutation | Result |
|---|---|---|
| A | `internal/casino/launch.go` `callContextRedacted`: append `string(c.Credential.Secret())` to the rendered credential | **SURVIVED.** The `internal/casino` and `internal/providercred` suites still passed (basis for C2). |
| B | `internal/casino/orchestrator.go` `postBet`: `if false && session.Status == LaunchSessionRevoked` | **KILLED** by `TestFailureModeMatrix_F_ProviderTransportFailureAtLaunchRevokesSessionNoTrace` |

Baseline: `go test -tags integration ./internal/casino/ -run 'TestLaunchGame|Revoke'` passes at `5a1ae02`. The author's own mutation evidence (`docs/plans/payment-readiness/evidence/prh-i2-casino-mutation-kill.txt`, 10/10 killed) was read, not re-run.

## Launch-blocking flag

For the orchestrator: C1 and C2 must be closed before production launch authorization and before any real casino adapter is wired. None of C1–C4 affects the current MOCK-only dev and test stage.
