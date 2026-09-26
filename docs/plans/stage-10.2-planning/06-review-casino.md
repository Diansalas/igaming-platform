> Stage 10.2 design review — specialist working paper (verbatim, recorded 2026-09-26 against 69e80c1). Where it differs from the Orchestrator rulings in `01-webhook-trust-design.md` §J, the rulings govern.

# casino review — CAS-WH-TENANT-1 design (01-webhook-trust-design.md §A, §C)

## Verdict: APPROVE, no required design changes to casino sections.

Cross-checked against HEAD: internal/casino/types.go, mock.go, orchestrator.go
(ReceiveCallback:561, postBet:775, postRollback:1268, postRollbackTombstone:1471,
BindProviderRound, mapReplayPayloadMismatch:615), internal/httpserver/
casino_handlers.go:300-440, casino_play_handlers.go, casino_routes.go, ADR 0082,
ADR 0080. All factual claims in the doc verified accurate against current code.

## Findings

1. **Current-state claims are accurate.** `mock.go` HandleCallback (:364) does
   `json.Unmarshal` before `hmac.Equal` verification — confirmed "parse before
   verify" (C2/point-7 violation) is real. `sign()` (:306) is a per-process,
   global `signingSecret` (:49) MACing NUL-joined fields with no tenant/provider
   in the input, signature embedded in the body — confirmed. `ReceiveCallback`
   (:561) already calls `provider.HandleCallback` before `LoadCapability`
   (tenant-scoped) — this ordering is *already correct* today and the new design
   preserves it (C3: (a) registered → (b) resolve+equality → (c) HandleCallback →
   unchanged post-verification LoadCapability).

2. **Move-verification-before-parse does not break any casino flow.** Every
   downstream consumer of `CallbackEvent` (postBet/postWin/postRollback,
   verifyPostedBetMatchesEvent, BindProviderRound) only ever sees a
   `CallbackEvent` that HandleCallback already validated field-by-field
   (enum, UUID, NUL-byte). Moving signature verification to operate over raw
   bytes *before* `json.Unmarshal` only removes the currently-vulnerable
   window where a decode error could occur before signature verification (a
   non-JSON body today returns a distinguishable parse error at 400 instead of
   a uniform 401) — this is exactly the point-7 fix and introduces no new
   parse-order dependency, since no field is read for a decision before
   verification either before or after the change.

3. **Idempotency/replay/rollback machinery is untouched by design, and correctly
   so.** C4 explicitly keeps `(tenant, provider_id, provider_tx_id)` idempotency
   keys, round correlation, F-7 `mapReplayPayloadMismatch`→409, `ErrAlreadyRolledBack`
   →409, and the per-tenant tombstone key `tombstone:<provider>:<orig>`
   (postRollbackTombstone:1471) all unchanged. Verified: none of these live
   inside `HandleCallback`; they run after `ReceiveCallback` dispatches to
   postBet/postWin/postRollback, which is unaffected by the signature-scheme
   swap. C5's "no financial effect on rejection" is consistent with current
   code: every error return in `ReceiveCallback` (unknown provider, capability
   disabled, signature invalid) happens before any postBet/postWin/postRollback
   call, and the enclosing `deps.DB.WithTenant` closure rolls the whole tx back
   on error.

4. **Cross-tenant tombstone bug (finding, confirmed) is closed by the design.**
   Today, `ReceiveCallback` verifies against a single global mock secret with
   no tenant binding, so an A-signed rollback callback verifies unchanged at
   B's slug, and B's tombstone write proceeds (LoadCapability is tenant-scoped
   but does not re-derive trust — it only checks B's own capability row, which
   nothing stops from existing). The design's tenant-bound MAC (Scheme.Verify
   rebuilding the signing input from `in.TenantID`) causes step (c) to fail for
   an A-signed payload at B's slug, before any B-scoped read — this is the
   correct fix and matches the already-shipped payments pattern.

5. **ADR 0082 lock order — no conflict.** Signature verification happens in the
   HTTP handler / `ReceiveCallback`'s early return path, entirely before any
   `tx` work that takes an L0/L1/L2/L3 lock (postBet's advisory locks, `player_cash`
   pre-lock, `ledger.Post`). Verification is not itself a "lock" and does not
   change lock acquisition order for any code path that reaches posting. No
   ADR 0082 exception needs updating.

6. **ADR 0080 provider rounds — no conflict.** `BindProviderRound` is called
   from `postBet` only, after RG/Risk/insufficient-funds decisions, well after
   HandleCallback verification; the design does not touch this call site or its
   ordering rationale (E-2/INV-LOCK-E2 in ADR 0082 §5.1a is unaffected since
   `casino_provider_rounds` is still `postBet`'s sole write path).

7. **"Route stays registered, mock credential wired only with test support"
   (C7) — verified no production dependency.** `registerCasinoRoutes`
   (casino_routes.go:48) registers `POST /v1/webhooks/casino/{tenantSlug}/
   {providerID}` unconditionally today, and nothing in `LaunchGame`/`Catalogue`
   consumes a webhook credential — those paths never call `HandleCallback` or
   any resolver. C7's own last bullet ("mock adapter stays registered for
   catalogue and launch... noted for architect") is accurate: launch/catalogue
   depend only on the adapter registry (`o.providers[providerID]`), never on
   `webhookauth.Resolver`. Gating the resolver to nil in production therefore
   only affects the webhook path (all-401 `no_resolver`), exactly as intended
   — it does not degrade launch or catalogue sync.

8. **Play-simulation interplay is correctly scoped.** `casino_play_handlers.go`
   (:337,430,570) is the *only* non-test production-reachable signer, and it is
   gated by the same `CasinoPlaySimulationEnabled` flag main.go derives from
   `TestSupportRoutesEnabled()` — confirmed live at casino_routes.go:29-40. The
   design's "one flag gates both the resolver and the routes, so they cannot
   diverge" (C8) is a real, verifiable invariant given current wiring; worth a
   `mockProviderWiring` unit test asserting exactly this (already listed as K11/
   C10 in the test plan).

9. **Test migration impact is understated in raw count but the delta is
   immaterial.** Grepped call sites: `CallbackPayload(` calls inside
   `internal/casino/*_test.go` + `internal/httpserver/casino_*_test.go` +
   `casino_play_handlers.go` sum to **119** (not "about 140"), plus the 3
   production play-handler sites already counted within that 119. `NewOrchestrator(`
   call sites naming the casino constructor (grep restricted to
   `internal/casino`, `internal/httpserver`, `cmd/platform-api`) = **exactly 91**,
   matching the design doc's figure precisely. The `CallbackPayload` discrepancy
   (119 vs "about 140") is not a correctness risk — the doc already says "all
   mechanical" and gives `qa`/the implementer a call-site inventory to work from,
   and 119 is comfortably in the "large but mechanical" bucket the design
   assumes. Recommend the implementer re-run this grep at implementation time
   rather than trusting either number, since HEAD has moved since the design
   was authored (`d76bdd3`).

## Required design changes: none.

## Non-blocking notes for the implementer (not design defects)
- Re-verify call-site counts against HEAD at implementation time (see finding 9).
- C12's "first real adapter" conformance test (`casino/conformance_test.go`
  tenant-binding case) should be written now even though no real adapter
  exists, per the skip-to-fail rule already specified — confirm this lands in
  the same commit as the mock migration, not deferred.
- The doc correctly defers the "mock adapter still registered for catalogue/
  launch in production" fact to a completion-report disclosure (§I) rather
  than treating it as a defect; concur — it is pre-existing, out of this
  ADR's scope, and separately tracked.
