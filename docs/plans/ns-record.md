# ns record: provider-id namespace disjointness (C-2/S-2), AP002/AP003 visibility (C-3), runbooks

Branch `ns-provider-namespace` (from a30f94c). No migration added.

## Findings
- Provider ids exist today for payments (adapter + statement source), casino and KYC. The sportsbook mock and the
  casino/sportsbook statement sources carry no provider id. The seam is `cmd/platform-api` `buildRegistrations`/`run()`.
- 0175 AP002 = key not active OR MAC mismatch; AP003 = proof expired/not yet valid (5 s skew).

## Implemented (IMPLEMENTED)
1. `internal/providerkind/namespace.go`: `IDClaim`, pure `RefuseProviderIDCollisions` (trim + case-insensitive; empty ignored;
   same-family sharing allowed; error names every colliding id with families and sources).
   `cmd/platform-api/registrations.go`: `buildProviderIDClaims`, `refuseProviderIDCollisions`; called in `run()` (main.go) right
   after the synthetic guard, before db.Connect, in ALL environments. Real sportsbook adapters must add a claim (documented).
2. `internal/httpserver/deposit_handlers.go`: `logDepositTombstoneProofKeyFault` emits
   `payment_webhook_deposit_tombstone_proof_key_fault` (code, provider_id, tenant_id, request_id only) for pg AP002/AP003 and
   skips the generic `payment_webhook_failed` line for that failure. HTTP: unchanged generic 500. Rule
   `PaymentWebhookDepositTombstoneProofKeyFault` (page) added to `deploy/observability/log-event-alert-rules.yml`.
   Behaviour change to flag: such failures no longer increment the `payment_webhook_failed` rate (dedicated event instead).
3. Docs: operational-runbooks.md (MA020 stranding case, owner-governed correction + security incident, exact go-live SQL,
   AP002/AP003 remedy), observability-and-alerting.md, production-configuration-checklist.md (namespace rule, alert).

## Tests
- providerkind: cross-family refused (names id), distinct/same-family/empty pass, every collision named.
- platform-api: mock bundle passes; casino id == payments id refused naming the id; run() calls the check (source pin).
- httpserver: key-fault log has code only (no error text), AP001/DT001/23505/plain errors fall through.
- deploy/observability rules parity test passes with the new rule.
- Commands: `go test -count=1 ./cmd/platform-api ./internal/providerkind ./internal/httpserver ./internal/payments ./deploy/observability` all ok; gofmt clean; go vet plain and `-tags integration` on touched packages clean.

## Residuals
- No full-HTTP integration test of AP002/AP003 (would need deactivating the DB key in a private DB; I did not run the DB harness). Helper is unit tested; the call site is a 3-line branch. PARTIALLY IMPLEMENTED for end-to-end proof.
- Namespace check covers bundle-level adapter ids only, not tenant configuration rows (`provider_capabilities` vs
  `casino_provider_capabilities`); a per-tenant config-time refusal remains NOT IMPLEMENTED. The planted-row variant of S-2 (runtime credential) is not addressed by this.
- Go-live SQL was checked against migration column names, not executed.
- S-2 asked for an AP001 alert on the casino rollback path: NOT IMPLEMENTED.

## Proposed registry text
"Provider-id namespace disjointness: IMPLEMENTED at process start (bundle ids; closure C-2 closed for bundle level). Per-tenant config-row check and AP001 casino-rollback alert: NOT IMPLEMENTED. C-3: IMPLEMENTED (AP002/AP003 dedicated event + runbook)."
