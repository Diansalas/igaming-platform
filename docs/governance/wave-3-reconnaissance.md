# Stage 4H-B1 Wave 3 — Reconnaissance Report

**Status of this document: READ-ONLY RECONNAISSANCE.** No code, migration,
or other architecture document was modified to produce this report. It is
the mandatory first action of the "STAGE 4H-B1 — WAVE 3 — Bonus Engine
Completion, Integration Hardening & Final Financial Gate" directive: a
from-code (not from-docs) reconstruction of what is actually implemented,
an exact gap list against Wave 3's own scope, findings on the four named
investigations, and a dependency map for the Orchestrator's phased
dispatch.

**Method.** Every file the directive names was read in full: `docs/
governance/wave-2-report.md`, `docs/governance/task-registry.md`'s
complete Stage 4H-B1 history (Wave 1 → Wave 2), `docs/governance/
ownership.md`, `docs/decisions/0039-*.md`, the relevant sections of `docs/
architecture/34-economic-operation-identity.md`, `docs/architecture/08-*
.md` §16 (via targeted reads/greps), `docs/architecture/ledger-accounting
-model.md` §6-§7 (via targeted reads/greps against the specific
mechanisms this report needed), `docs/security/security-architecture.md`
§W15/§B1 (via targeted reads/greps), every non-test file in `internal/
bonus/`, `internal/economicop/`, `internal/casino/bonus_settlement.go`,
the relevant parts of `internal/casino/orchestrator.go`, `internal/risk/
cumulative.go`, `internal/risk/types.go`, `internal/auth/permission.go`,
`internal/httpserver/bonus_handlers.go`/`bonus_routes.go`/`admin_routes
.go`, and every migration `0050`-`0067` (`.up.sql`, in full). Every claim
below marked "confirmed by code" was checked directly against the live
repository at HEAD `3016e6f` (branch `claude/focused-wright-jw88w9`),
never assumed from a prior report. `gofmt -l .`, `go build ./...`, and
`go vet ./...` all ran clean at the start of this reconnaissance — the
repository is in the state Wave 2's report describes, not drifted.

---

## 1. From-code reconstruction — the Grant state machine as it actually exists

### 1.1 Status enum and legal transitions (verified against `internal/bonus/grant.go`, `lifecycle.go`, `conversion.go`, `held_disposition_ops.go`)

```
GrantStatus (migration 0057 CHECK, bonus.GrantStatus):
  issued → activated → in_progress → completed → converted
                                    ↘ pending_settlement → {expired|cancelled|forfeited}
  issued → cancelled                 (T.1 gate denies at activation)
  {issued|activated|in_progress} → {expired|cancelled|forfeited}   (TerminateGrant, AOE empty)
  {issued|activated|in_progress} → pending_settlement → {expired|cancelled|forfeited}
                                                          (TerminateGrant, AOE non-empty; finalized later by RecheckGrantExposure)
  any terminal status → reversed    (MarkGrantReversed — exists at the storage layer;
                                      see §2 item "reversal" — no lifecycle-layer caller found)
```

Every transition is a compare-and-swap (`UPDATE ... WHERE status = expected`,
`grant.go` `UpdateGrantStatus`/`SetGrantPendingSettlement`/
`FinalizePendingSettlement`/`MarkGrantReversed`), never an unconditional
write. `ComputeNewStakeEligibility` (`aoe.go`) is a pure function of
`status`: `open` iff `{issued, activated, in_progress}`, `closed`
otherwise — this is the load-bearing predicate every wagering-
authorization call site is supposed to consult (only `RecordWageringContribution`
actually does, and it has no live caller — §2).

**`(none)→issued`** (`lifecycle.go IssueGrant`): RG→Risk only
(`SkipAssetAuthorization: true` — "issued is a decision, not a
movement"), `Operation=OperationBonusGrant`, insert under `db.IdempotentInsert`
so a `(tenant_id, campaign_id, offer_version_id, player_account_id,
trigger_reference)` unique-constraint conflict (migration 0057) resolves
to the existing row rather than erroring. On denial, **no row is created
at all** — confirmed by code: `IssueGrant` returns before any insert if
`GateCheckpoint` denies.

**`issued→activated`** (`ActivateGrant`): full T.1 gate
(AssetAuthorization→RG→Risk, `Operation=OperationBonusGrant`). On denial,
transitions straight to `cancelled` (never leaves a Grant issued-but-
denied). On allow: `PostGateHook` (if set — the two EOI-gated surfaces
only) runs strictly after the gate chain and strictly before the ledger
write; then the **sole** `bonus_grant` posting (`Cr player_bonus
amountMinor`, Rule B2 mirror adds `Dr promo_liability`); then
`granted_amount` is written (migration 0067, exactly once, `NULL`-guarded
no-op on replay since the CAS on status already prevents a second write);
then the status flips.

**`{activated,in_progress}→completed`** (`CheckAndCompleteGrant`): derives
`P_firm` live (`DeriveWageringProgress`, casino-only regime where
`P_firm == P_net`), compares against a caller-supplied
`wageringTargetScaled`; a `nil` target (Cashback's own call) means
"already satisfied."

**`completed→converted`** (`ConvertGrant`, T.12/Path A): (1) `AOE(G,t)`
checked first (cheapest, no external call) — non-empty blocks with reason
`open_exposure_outstanding`, Grant stays `completed`; (2) `P_firm`
re-derived and re-checked (HR-12, inside this same transaction — never
trusts a stale caller-supplied progress figure); (3) full T.1 gate,
`Operation=OperationBonusConversion`; (4) on allow, `Dr player_bonus /
Cr player_cash` (Rule B2 adds `Cr promo_liability` + `Dr bonus_expense`/
`provider_payable`) for `min(remaining, MaxCashoutAmount)`, or a
zero-posting status-only transition if nothing remains. **Any** Risk/RG/
AssetAuthorization block leaves the Grant at `completed` (non-terminal,
retryable) — confirmed: no code path in `ConvertGrant` ever calls
`TerminateGrant` or forfeits on a gate denial.

**Termination (`TerminateGrant`, T.9/T.10, N1.4)**: requires
`NewStakeEligibility==open` (i.e. `{issued,activated,in_progress}` only —
**cannot** be called against `completed`, confirmed by code's own guard).
Steps: (1) write down whatever `player_bonus` balance is currently free
(`terminal_write_down`, `Dr player_bonus/Cr promo_liability`, zero-balance
posts nothing) — never gated by AssetAuthorization/RG/Risk (value-reducing
asymmetry); (2) `ComputeAOE` live under the advisory lock; (3) if empty,
write the terminal status directly; (4) if non-empty, write
`pending_settlement` instead, carrying `terminal_resolution` (the
already-decided eventual status) — **never `converted`**.

**`pending_settlement` finalization** (`RecheckGrantExposure`,
N1.4.2): called by casino after a value-*reducing* posting
(`postRollback`'s plain-rollback site, and the held-win-rollback site) and
by `ResolveHeldDispositionAction` itself after resolving the last
outstanding disposition. Re-derives `AOE` (the identical three-component
sum, never a second implementation); if still non-empty, no-ops (status
stays `pending_settlement`); if empty, flips to the carried
`terminal_resolution` via `FinalizePendingSettlement`.

**`AOE(G,t)`** (`aoe.go`, N1.3's three components, confirmed against the
live queries):
1. `LockedExposure` — live `player_locked_bonus` balance attributed via
   `grant_ledger_attributions`. **Structurally always 0 today** — no code
   path anywhere in this repository ever attributes a `player_locked_bonus`
   entry to a Grant (`postBet` never debits `player_bonus`, confirmed §2).
2. `InFlightExposure` — count of `bonus_wagering_progress` rows whose
   `correlation_id` has no matching `casino_win`/`casino_rollback` yet.
   **Structurally always 0 today** — `bonus_wagering_progress` is never
   populated in production (`RecordWageringContribution` has no live
   caller, confirmed §2).
3. `HeldDisposition` — live `player_bonus_held` balance attributed via
   `grant_ledger_attributions`. **The only component that is ever
   non-zero in the current, reachable production surface**, and only
   because it is unconditionally populated by casino's own
   `postWinLockedBonus`/`ResolveTerminalGrantCredit` seam. But that seam
   itself is only reached when a bet's origin resolves to
   `player_locked_bonus` — which, per (1), never happens today either.

**Net finding, stated precisely (new, not previously stated this
precisely in any prior report):** with `postBet` never debiting
`player_bonus`/`player_locked_bonus`, **`AOE(G,t)` is unconditionally
empty for every Grant in this platform today**, `pending_settlement` is
therefore unreachable in production, and the entire G-2 mechanism
(`bonus_held_dispositions`, `ResolveHeldDispositionAction`,
`RecheckGrantExposure`) — while fully implemented, migrated, and
integration-tested — has **zero live trigger path**. This is not a defect;
it is the honest consequence of casino's `postBet` bonus-funded leg not
being wired (§2, disclosed since Wave 2 §21), stated here as an explicit,
verified fact rather than left implicit.

### 1.2 Ledger posting shapes (verified against the actual `ledger.Post` call sites, not re-derived from `ledger-accounting-model.md`)

| Transaction | Caller entries (Rule B2 adds the rest) | File:function |
|---|---|---|
| `bonus_grant` | `Cr player_bonus` | `lifecycle.go ActivateGrant` |
| `bonus_forfeiture` (terminal write-down) | `Dr player_bonus` | `lifecycle.go terminalWriteDown` |
| `bonus_conversion` | `Dr player_bonus / Cr player_cash` | `conversion.go ConvertGrant` |
| `casino_win` (hold-capture) | `Dr house_gaming / Cr player_bonus_held` (×2: payout + released-lock legs) | `bonus_settlement.go postWinLockedBonus` |
| `casino_rollback` (held-win rollback) | `Dr player_bonus_held / Cr house_gaming` (×2, straight to house — **never** restores `player_locked_bonus`) | `bonus_settlement.go postRollbackHeldWin` |
| `bonus_forfeiture` (`ACTION_REFORFEIT`) | `Dr player_bonus_held` | `held_disposition_ops.go ResolveHeldDispositionAction` |
| `bonus_conversion` (`ACTION_ROUTE_TO_CASH`) | `Dr player_bonus_held / Cr player_cash` | `held_disposition_ops.go ResolveHeldDispositionAction` — confirmed structurally identical to ordinary `bonus_conversion`'s 4-leg shape once Rule B2 fires (final ruling, Wave 2 Phase 11) |

`granted_amount` (migration 0067) is written exactly once, in
`ActivateGrant`, immutable thereafter (migration 0067's trigger
extension) — confirmed the only writer in the repository.

---

## 2. Exhaustive gap list against Wave 3's enumerated scope

Legend: **IMPLEMENTED** (cite file/function) · **PARTIALLY IMPLEMENTED**
(cite exactly what's missing) · **NOT IMPLEMENTED** (confirmed by absence
— grep/zero-callers check cited).

| # | Scope item | Status | Evidence |
|---|---|---|---|
| 1 | Campaign lifecycle (create/version) | **PARTIALLY IMPLEMENTED** | `CreateCampaign`/`CreateCampaignVersion`/`SetCampaignCurrentVersion` (`campaign.go`) work and are HTTP-reachable for *create* (`POST /v1/admin/bonus/campaigns`). **`activate`/`suspend`/`end`/`archive` have no dedicated function** — only the unconditional `UpdateCampaignStatus` exists, with no four-eyes/EOI wiring and no HTTP route calling it at all. |
| 2 | Offer lifecycle (create/version/publish) | **NOT IMPLEMENTED at any reachable surface** | `CreateOffer`/`CreateOfferVersion` exist in Go (`offer.go`) but **zero HTTP handler exists to create an Offer at all** (confirmed: `grep -n "CreateOffer\|newOffer" internal/httpserver/*.go` → no results). `offer_publish`'s four-eyes `ChangeOperation` is declared (`change_governance.go`) but has **zero Go call site** consuming it (confirmed: `ConsumeApprovedChangeRequest` is called from exactly one place in the whole repo — `held_disposition_ops.go`, for `ChangeOpHeldDispositionResolve` only). |
| 3 | Grant lifecycle (issue/activate) | **IMPLEMENTED** | `lifecycle.go IssueGrant`/`ActivateGrant`, HTTP-reachable via `POST /v1/admin/bonus/grants` (manual) and `RedeemCoupon` (player, coupon trigger). |
| 4 | Progress lifecycle | **IMPLEMENTED** for the general Progress trail (`progress.go AppendGrantProgress`, every mutating transition writes one, migration 0062's append-only/immutable/SEP-1-triggered table). **PARTIALLY IMPLEMENTED / dormant** for `WageringProgress` specifically (§7) — the table and derivation exist and are correct, but nothing ever writes to it in production. |
| 5 | Conversion | **IMPLEMENTED**, but **unreachable end-to-end** because `completed` is only reachable via Cashback's self-completion (§7) or a manual/test-driven wagering-progress write; no HTTP endpoint exists to *trigger* conversion at all (no `POST .../grants/{id}/convert` route for either player or staff — confirmed: not in `bonus_routes.go`). |
| 6 | Forfeiture | **IMPLEMENTED** as a mechanism (`terminalWriteDown`, `TerminateGrant` with `TerminalResolutionForfeited`), **NOT wired to any caller** — `grep -rn "TerminateGrant\b"` finds only its own definition file. No HTTP route, no sweep job. |
| 7 | Expiry | **NOT IMPLEMENTED as a reachable capability** — `TerminateGrant`'s expired path exists (mechanism), but (a) no sweep/scheduler reads `bonus_offer_versions.wagering_time_limit`/`payout_time_limit` or any Grant-level expiry timestamp and calls it, and (b) no such Grant-level "expires_at" field exists on `bonus_grants` at all (confirmed: migration 0057's column list has no expiry timestamp). This is a genuine, not-previously-named-this-precisely gap: **there is no field to expire against**, not merely no sweep job. |
| 8 | Cancellation | **PARTIALLY IMPLEMENTED** — `TerminateGrant` with `TerminalResolutionCancelled` covers the pre-`completed` case (mechanism only, no caller — same as Forfeiture). **`grant_cancel_completed`** (cancelling an *already-completed* Grant, the security doc's 7th dual-controlled operation) **has no implementing function at all** — `TerminateGrant`'s own guard (`ComputeNewStakeEligibility(g.Status) != StakeEligibilityOpen`) structurally rejects a `completed` Grant; there is no second function for this case. |
| 9 | Reversal | **PARTIALLY IMPLEMENTED** — `MarkGrantReversed` exists at the storage layer (`grant.go`) with correct CAS semantics, but **has zero callers anywhere** — no lifecycle-layer function ever invokes it, no HTTP route, no four-eyes wiring despite `security-architecture.md`'s REQ-SEP-BONUS-1 naming "adjustment, forced conversion" (not reversal specifically) as in scope. |
| 10 | Deposit bonus | **PARTIALLY IMPLEMENTED** | `IssueAndActivateDepositBonus` (`types.go`) is correct, tested business logic with **zero callers outside its own package** (confirmed by repo-wide grep) — no deposit-event consumer exists anywhere (`internal/payments` has no reference to `internal/bonus`). Disclosed honestly in `bonus_handlers.go`'s own doc comment ("deposit/reload bonus triggering are event-driven... not wired here"). |
| 11 | Reload bonus | **PARTIALLY IMPLEMENTED** — same function/gap as Deposit (doc 10: "identical Offer shape, eligibility axis only distinguishes it"). |
| 12 | Cashback | **PARTIALLY IMPLEMENTED** | `IssueAndActivateCashback` is the only bonus type whose issue→activate→complete chain is fully self-contained in one call (no wagering dependency) — but it too has **zero live callers** (confirmed, and independently confirmed by Wave 2's own report §21: "`IssueAndActivateCashback` has zero live callers anywhere in the repo"). No settlement-window job exists to compute `NetLossAmount` and invoke it. |
| 13 | Generic wagering bonus | **PARTIALLY IMPLEMENTED** | `IssueAndActivateGenericWageringBonus` has zero callers outside its own package; more importantly, **even if issued**, its completion path (`RecordWageringContribution`→`CheckAndCompleteGrant`) has no live trigger either (§1.1's finding) — so a Generic Wagering Grant, once activated, can never progress to `completed` via real casino play today. |
| 14 | Coupon | **IMPLEMENTED end-to-end** | `RedeemCoupon` (`types.go`) is called from `newRedeemCouponHandler` (`bonus_handlers.go`), reachable via `POST /v1/bonus/coupons/redeem`. This is the **only one of the five bonus types with a genuine, live, HTTP-reachable production entry point.** Completion still depends on wagering contribution (item 13's gap) unless the redeemed Offer has no wagering axis. |
| 15 | Eligibility | **PARTIALLY IMPLEMENTED** — the three-way gate (`eligibility.go GateCheckpoint`) is fully implemented and correctly ordered. But of `OfferVersion`'s own declared eligibility fields, only `MinQualifyingAmount`/`MaxQualifyingAmount` are ever read (in `types.go`'s deposit/cashback formulas). **`EligibilityJurisdictions`, `EligibilityDepositMethods`, `OptInRequired`, `VIPTierSegment`, `RedemptionLimitPerPlayer`/`RedemptionLimitGlobal`, `StackingConflictPredicate` are stored but never read by any business logic** (confirmed: `grep` for each identifier outside `offer.go` returns nothing). A coupon's own "per-player limit" is enforced only as "exactly one, ever" via the DB unique constraint — a configured `RedemptionLimitPerPlayer > 1` is silently unenforceable today. |
| 16 | Targeting (single/list/promo/manual/bulk) | **PARTIALLY IMPLEMENTED** | Single-player and static player-list bulk targeting are implemented (`targeting.go RunStaticBulkGrantJob`) and integration-tested, but **have zero callers outside their own package** — no HTTP route, no job runner scans `bulk_grant_jobs` in `queued`/`running` status (confirmed: `ListBulkGrantJobsByStatus`/`CreateBulkGrantJob`/`RunStaticBulkGrantJob` each have zero external callers). Promo/voucher codes = Coupon (item 14, IMPLEMENTED). Manual single grant = IMPLEMENTED, HTTP-reachable (item 3). |
| 17 | Promo/voucher flows | **IMPLEMENTED** (= Coupon, item 14). |
| 18 | Manual grants | **PARTIALLY IMPLEMENTED** | Issuance itself works end-to-end via HTTP (`newIssueManualGrantHandler`), **but requires the caller to already possess an approved `parent_operation_id`** — the HTTP request body's own `parent_operation_id` field is a client-supplied string the handler merely parses and passes to `economicop.CheckEntry`. There is **no HTTP route to mint that EOI** (`MintRootOperation` has zero callers outside `internal/bonus` itself), so today an operator can only reach this endpoint successfully after minting the EOI via direct database access — precisely the gap Wave 2 §21 disclosed, now traced to its exact code-level shape. |
| 19 | Bulk grants | **PARTIALLY IMPLEMENTED** — see item 16; the mechanism (including the EOI recipient/value-ceiling enforcement, DR-4HB1W2-01/02-fixed) is correct and tested, but entirely unreachable outside a test harness. |
| 20 | Activity/event consumption | **NOT IMPLEMENTED** | Confirmed by repo-wide import search: `internal/bonus` is imported by exactly two packages — `internal/httpserver` (the HTTP surface) and `internal/casino` (the two G-2 settlement seams). No deposit, cashback-window, or generic-trigger event consumer of any kind exists. |
| 21 | Risk/RG/KYC/AssetAuthorization integration | **IMPLEMENTED** for Risk/RG/AssetAuthorization (the T.1 gate, confirmed at every checkpoint: issue, activate, convert, `ACTION_ROUTE_TO_CASH`). **NOT IMPLEMENTED** for KYC (see §3.2 below — `kyc_rg_level_required` stored, never read anywhere in `internal/bonus`, confirmed by grep). |
| 22 | Multi-asset | **IMPLEMENTED** — every monetary field is `*big.Int`/`NUMERIC(38,0)`; no hardcoded currency anywhere in `internal/bonus`/`internal/economicop`; `AssetCode`/`DecimalExponent` flow through every Grant. |
| 23 | Rounding | **IMPLEMENTED** for the two formulas that exist (`computeCappedPercentageReward`, via `internal/money.RoundToMinorUnits`, DS-1/DS-2 round-half-up-once). **NOT APPLICABLE / not yet exercised** for any other reward shape (R2 fixed-value, R3 free-round, R5/R6) since only R1-shaped Deposit/Cashback formulas are implemented at all. |
| 24 | Ledger integration | **IMPLEMENTED** — every posting shape in §1.2 traced and correct; Rule B2 mirror fires unconditionally; no hand-assembled mirror leg anywhere. |
| 25 | Idempotency | **IMPLEMENTED** — every financial write is DB-unique-constraint-backed (`bonus_grants_idempotency_key`, `bonus_held_dispositions_settlement_tx_key`, `bulk_grant_job_items`' per-player unique, `economic_operations_tenant_idempotency_key`, `ledger.Post`'s own key). |
| 26 | Audit | **IMPLEMENTED** — every mutating write in the traced code paths calls `audit.Record`. |
| 27 | RLS | **IMPLEMENTED** — every one of the 16 new tables (migrations 0053-0064) carries `ENABLE`+`FORCE ROW LEVEL SECURITY` and per-command policies (no `FOR ALL`), verified by reading every migration's own policy block. |
| 28 | RBAC | **PARTIALLY IMPLEMENTED** — every permission the security doc requires exists and is role-wired (`internal/auth/permission.go`: `PermBonusCampaignActivate`, `PermBonusOfferManage`, `PermBonusBulkExecute`, etc., confirmed present and assigned to `promotions_manager`/`bonus_operations`), but **most of them have no HTTP handler that checks them** — `PermBonusCampaignActivate`/`PermBonusOfferManage`/`PermBonusBulkExecute`/`PermBonusAdjustmentWrite`/`PermBonusGrantCancel`/`PermBonusGrantReview` are all defined and role-assigned with zero consuming route (confirmed: `grep` of each permission constant across `internal/httpserver/bonus_*.go` finds no match). |
| 29 | Concurrency | **IMPLEMENTED** — advisory locks (`AdvisoryLockGrant`), `FOR UPDATE` row locks, and the EOI's own canonical lock ordering (Risk's lock always before the EOI root lock, `DR-4HB1W2-01`-fixed) are all present and match doc 34 §5.3's rule exactly. |
| 30 | Reconciliation hooks | **PARTIALLY IMPLEMENTED** — `GrantLedgerAttribution` (§W2.5) is a correct, live-derived, zero-maintained-counter design; the generic `internal/reconciliation` ledger-vs-projection sweep (`scheduler.go RunSweep`/`RunSchedulerLoop`) automatically covers every new Bonus account type. The **bonus-specific B1(extended) invariant is proven correct by a test helper but has no production job wired** (confirmed: `internal/reconciliation` has no bonus-specific file or reference). |
| 31 | Provider-native bonus coexistence | **IMPLEMENTED** — confirmed structurally: the Grant-lookup path in `bonus_settlement.go` is only reachable when the origin resolves to `player_locked_bonus`/`player_bonus`; `event.PlayerAccountID` is never read in the destination-resolution path. |
| 32 | Four-eyes governance (the eight named operations) | **PARTIALLY IMPLEMENTED — a materially sharper finding than Wave 2's own report** | The DB-level infrastructure (`bonus_change_requests`/`bonus_change_approvals`/`bonus_approval_policies`, migration 0063 — tables, immutability triggers, the governance trigger, the SEP-1 trigger with its migration-0066 Step-0 fix, and the single `bonus_change_consume_approved_request` function) is fully built and generic across all eight `ChangeOperation` values. **But `ConsumeApprovedChangeRequest` (the Go wrapper around that function) is called from exactly one place in the entire codebase: `held_disposition_ops.go`, for `ChangeOpHeldDispositionResolve` only.** The other seven operations (`manual_grant_issue`, `bulk_job_execute`, `bonus_adjustment_write`, `grant_forced_conversion`, `campaign_activate`, `offer_publish`, `grant_cancel_completed`) have their enum constants declared and their DB tables ready to receive rows, but **zero Go-level code path ever files, requires, or consumes an approval for any of them.** Manual-grant and bulk-grant issuance are instead gated *only* by the EOI mechanism (`economicop.CheckEntry`/`ConsumeRootBudget`), which itself requires a pre-existing, pre-approved EOI that nothing in this codebase currently mints outside a test. This is worth stating plainly: **the "manual grant issuance above threshold requires four-eyes" and "bulk job execution always requires four-eyes" controls the security architecture specifies are not enforced by any code path reachable today** — not merely missing an HTTP front end, as Wave 2 §21 characterized it, but missing the underlying application-level wiring for seven of the eight operations. |

---

## 3. The four named investigations

### 3.1 Missing HTTP admin surfaces for four-eyes filing and campaign activation

**Confirmed, and more extensive than Wave 2 §21 disclosed.** Exact
inventory of what has Go/DB logic but no HTTP route:

- **File a `bonus_change_requests` row** (`FileChangeRequest`) — Go
  function exists, zero HTTP route for any of the 8 operations.
- **Approve/reject a change request** (`RecordChangeApproval`) — Go
  function exists, zero HTTP route.
- **Mint an `EconomicOperationIdentity` root** (`MintRootOperation`) — Go
  function exists, zero HTTP route. This is the more serious of the two:
  `newIssueManualGrantHandler` requires the *caller* to already supply a
  valid `parent_operation_id`, meaning even the one manual-grant HTTP
  route that does exist cannot be exercised end-to-end without out-of-band
  (direct database) EOI creation today.
- **Activate a Campaign** — no HTTP route; only `CreateCampaign` exists at
  HTTP. `UpdateCampaignStatus` (unconditional, no four-eyes) is the only
  Go-level primitive, uncalled by anything.
- **Create/publish an Offer/OfferVersion** — no HTTP route at all, for
  either creation or publication. `CreateOffer`/`CreateOfferVersion`/
  `UpdateOfferStatus` exist in Go, uncalled by anything outside tests.
- **Create/execute a `BulkGrantJob`** — no HTTP route. `CreateBulkGrantJob`/
  `RunStaticBulkGrantJob` exist in Go, uncalled by anything outside tests.

**What would be needed, using the EXISTING governance model (no new
model):**

1. A generic `POST /v1/admin/bonus/change-requests` (file) and
   `POST /v1/admin/bonus/change-requests/{id}/approve` (or `/reject`)
   pair, parameterized by `operation`/`target_type`/`target_id`/`payload`,
   calling `FileChangeRequest`/`RecordChangeApproval` — this single pair
   covers all 8 operations, since the DB layer is already
   operation-agnostic. `ResolveApprovalPolicy` already resolves the
   required threshold/approval count per tenant/brand/asset.
2. A `POST /v1/admin/bonus/economic-operations` (mint) endpoint wrapping
   `MintRootOperation`, taking the same fields `MintRootOperationParams`
   already defines — this can itself require the caller to have already
   filed+had-approved a `manual_grant_issue`/`bulk_job_execute`/
   `campaign_activate` change request via (1), so the two surfaces
   compose rather than duplicate control.
3. `POST /v1/admin/bonus/campaigns/{id}/activate` and `/suspend`, calling
   `UpdateCampaignStatus` **after** consuming an approved
   `campaign_activate` change request (a small new Go wrapper function is
   needed here — `UpdateCampaignStatus` itself has no such gate today, so
   this is not purely a routing exercise; it requires the same kind of
   application-level wiring item 32's gap describes).
4. `POST /v1/admin/bonus/offers` (create), `POST /v1/admin/bonus/offers/
   {id}/versions` (create version), `POST /v1/admin/bonus/offers/{id}/
   publish` (consume `offer_publish` approval, then `UpdateOfferStatus`).
5. `POST /v1/admin/bonus/bulk-jobs` (create, `pending_four_eyes`), `POST
   /v1/admin/bonus/bulk-jobs/{id}/execute` (consume `bulk_job_execute`
   approval — always required per the security doc — mint/relay the EOI,
   then call `RunStaticBulkGrantJob`, presumably from a background
   worker rather than inline given potential job size — no such worker
   exists today either, see gap-list item 19).

None of this requires a new governance model — every piece above is a
thin HTTP wrapper plus, for items 3-5, the missing Go-level "consume
approval then perform the operation" wiring that item 32's gap already
names as absent.

### 3.2 The KYC-tier taxonomy gap

**Confirmed still true, and more precisely than "no level/tier concept
exists at all."** `internal/kyc` has exactly one status-shaped concept
(`VerificationStatus`: `unverified|pending|review_required|approved|
rejected|expired`) — a binary-ish outcome, not a leveled tier (no
Basic/Enhanced/VIP distinction anywhere). `internal/rg` has no
level/tier concept either (confirmed by grep: zero matches for
`level`/`tier` in either package's non-test Go files).

**One thing does already exist that is close, and is worth naming
precisely rather than concluding "nothing exists at all":**
`player_accounts.kyc_tier INT NOT NULL DEFAULT 0` (migration 0010,
`internal/identity/player_account.go`'s `PlayerAccount.KYCTier` field).
This column has existed since a very early stage. **It is completely
dormant**: confirmed by repo-wide search, nothing anywhere ever writes
`kyc_tier` (`grep -n "kyc_tier\s*="` across the whole repository returns
zero matches) — it is only ever read/scanned, always at its default of 0.
No KYC verification event, no RG policy, no admin action ever sets it to
a non-zero value.

**Answer to the directive's precise question:** `internal/identity` has a
column with the right *name* but no operative *meaning* — a value that is
always 0 is not a taxonomy, it is an unpopulated placeholder. Reusing it
"without a new cross-domain contract" is not actually available: making
`kyc_tier` mean something (what the tiers are, what promotes a player
between them, who owns writing it) is itself the missing cross-domain
decision — identity-compliance would have to design and populate a real
tier semantics before Bonus could safely read it, which is exactly the
scope of work Wave 2's disclosed gap already named. **This genuinely
requires a new shared-domain decision** (owned by identity-compliance,
architect co-signing the contract shape, per `ownership.md`'s KYC row),
not a reuse of an existing concept. The dormant column is at best a hint
about where such a field would eventually live on `player_accounts`, not
a shortcut around designing what it means.

### 3.3 The multi-account/Person-level abuse detector gap

**Confirmed still true — and confirmed to already be `bonus-engine`'s own
assigned responsibility, not a newly-discovered scope item.**
`security-architecture.md` §W15.1.5 / `REQ-SEP-BONUS-3` explicitly reads:
*"`bonus-engine`: Own the household/linked-account **detection** path of
§W15.1.5; refuse any request to make it a block."* §W15.1.5 further
specifies the detection must emit a **signal**, never a refusal — a hard
block on probabilistic linkage is explicitly rejected by that same
section's own reasoning (false-positive risk, matcher-degradation
pressure). This is a binding design constraint on whatever Wave 3
builds here, not a free choice.

Confirmed by code: `velocity_cap_refs` and
`device_fingerprint_linking_config` (`bonus_offer_versions` columns,
migration 0055) are stored, opaque JSONB, never read anywhere in
`internal/bonus` (confirmed by grep — same pattern as the other
unenforced `OfferVersion` axes, gap-list item 15). No detector code
exists anywhere in the repository.

**Minimum viable Bonus-domain-only scope**, precisely bounded per the
directive's own ask:

- A same-tenant `player_accounts` query joining on `person_id` — the
  identical primitive `runBulkGrantJobItem`'s own SEP-1 compensating
  check already uses (`playerPersonID`, `targeting.go`) — to detect
  whether the *Person* behind the targeted `player_account_id` already
  holds a `bonus_grants` row against the *same Offer* (or a
  `first_deposit_only`-flagged Offer generally) via a **different**
  `player_account_id`. This is exactly the "first-deposit-only per
  Person, not per account" case the directive names, and it needs no
  new cross-domain contract: `player_accounts.person_id` already exists
  and is already read by `internal/bonus` in exactly this shape.
- The output must be a **signal**, per §W15.1.5's binding constraint —
  concretely, routing the Grant attempt into `ACTION_HOLD_FOR_REVIEW`-
  style handling (which itself doesn't fully exist yet either — see the
  `manual_review_routing` column, also stored-never-read) or recording a
  flagged `GrantProgressEntry`/`audit.Record` for a human reviewer, never
  an automatic denial inside `GateCheckpoint`.
- **Out of scope, correctly**: device/payment-fingerprint correlation
  (`device_fingerprint_linking_config`) is a materially harder problem
  (requires a fraud/device-signal data source this platform does not
  have) and would require a real fraud/CRM-adjacent system — explicitly
  named here as requiring one, per the directive's own constraint not to
  scope around that requirement silently. A minimum Wave 3 slice should
  build the `person_id`-linkage check only, and name the
  device/payment-fingerprint half as still-deferred, not attempt a
  partial fingerprint heuristic.

### 3.4 LF-10 — rollback of an already-resolved financial event

**Confirmed still open, exact current code shape verified.**
`postRollbackHeldWin` (`bonus_settlement.go`) switches on
`disposition.Status`: `held` proceeds; `voided_by_rollback` is an
idempotent no-op for a genuine redelivery of the *same* rollback, but a
*distinct* new rollback reference targeting an already-voided record is
rejected via `ErrHeldDispositionAlreadyVoided`; **`resolved_reforfeit`
and `resolved_route_to_cash` both return
`ErrHeldDispositionRollbackUnsupported`** — fails closed, posts nothing,
by design (confirmed: the `switch` statement's `default` and named cases
leave no path to a posting for these two statuses).

**Does any Wave 3 scope item actually depend on LF-10 being resolved?**
Traced explicitly, item by item, against LF-10's precise scope ("a
rollback naming a settlement transaction whose `bonus_held_disposition`
has already moved *past* `held` — i.e. already reforfeited or already
routed to cash — arrives late"):

- **Conversion, forfeiture, expiry, cancellation, reversal** (gap-list
  items 5-9): none of these transitions ever reads or writes a
  `bonus_held_dispositions` row at all — they operate on `bonus_grants`
  directly. **Orthogonal to LF-10.**
- **Deposit/reload/cashback/generic-wagering/coupon bonuses** (items
  10-14): none of these bonus types' issuance/activation/completion
  logic touches held dispositions. **Orthogonal.**
- **Eligibility, targeting, promo/voucher, manual/bulk grants** (items
  15-19): none creates or resolves a held disposition. **Orthogonal.**
- **The four-eyes/HTTP-surface work (§3.1)**: filing/approving/
  activating campaigns, offers, and bulk jobs never touches
  `bonus_held_dispositions`. **Orthogonal.** The one exception —
  `held_disposition_resolve`'s own HTTP surface, if Wave 3 chooses to
  build it — calls `ResolveHeldDispositionAction`, which is itself
  unaffected by LF-10 (LF-10 is about a *rollback arriving after*
  resolution, not about performing the resolution itself).
- **The only place LF-10 is reachable at all** is casino's
  `postRollbackHeldWin`, and it is reachable only if a real, late
  rollback for an already-resolved held disposition is ever actually
  delivered by a provider — which, per §1.1's finding, requires the
  entire G-2 mechanism to first become live (casino must actually lock
  bonus-funded stakes, which it does not do today). **LF-10 is therefore
  doubly inert right now**: not only does no Wave 3 scope item depend on
  its general case being decided, but the specific code path it gates is
  itself unreachable until `postBet`'s bonus-funded locking side (a
  separately-named, already-disclosed, out-of-Wave-3-scope-per-directive
  item) is built.

**Conclusion: Wave 3 can proceed entirely without touching LF-10.** It
remains correctly routed to `ledger-finance` as a future dedicated design
dispatch, per `CLAUDE.md`'s Human-Decision-Register discipline (this is
not itself a Human Decision Register item, but is explicitly named by
this directive as not-to-be-resolved-here).

---

## 4. `agentnetwork`/SEP-1 (`REQ-SEP-AFF-1`) confirmation

**Confirmed purely Affiliate-domain, not Bonus-relevant, does not block
Wave 3.** `security-architecture.md` §W15.1.9's routed dependency reads:
*"`architect` must confirm this invariant holds for `agentnetwork` as
built... Until confirmed, Affiliate's adoption of `SEP-1` (`REQ-SEP-AFF-1`)
must not ship."* The invariant in question — that `agentnetwork`'s
parent/child edges never cross a tenant boundary — is required
specifically because Affiliate's `SEP-1` beneficiary resolver uses the
**`ancestor_closure`** resolver shape (a reflexive walk up an affiliate's
node ancestry), which is unique to Affiliate's commission/re-attribution
flows.

Bonus's own `SEP-1` adoption (migrations 0062/0063/0066, confirmed by
reading every trigger) uses only the **`single_subject`** shape (a Grant's
own player) and the **`pinned_set`** shape (a `BulkGrantJob`'s
materialized `target_player_list`) — never `ancestor_closure`. Bonus has
no dependency on `agentnetwork` anywhere in its code (confirmed:
`internal/agentnetwork` does not exist in this repository at all —
`ls internal/agentnetwork` → no such directory — and no Bonus file
references it). **`REQ-SEP-AFF-1` and its `agentnetwork` tenant-boundary
confirmation are entirely orthogonal to Wave 3's Bonus scope** and remain
correctly routed to whichever future stage authorizes Affiliate
implementation (explicitly out of Wave 3 per the directive's own
constraints).

---

## 5. Dependency map and recommended phase/owner assignments

For every Wave 3 scope item found NOT fully implemented above, what it
depends on and who should own it:

| Item | Depends on (existing code to build on) | Blocked by (external decision) | Recommended owner |
|---|---|---|---|
| HTTP surfaces: file/approve change request, mint EOI | `FileChangeRequest`/`RecordChangeApproval`/`ConsumeApprovedChangeRequest`/`MintRootOperation` (all exist, tested) | none | `bonus-engine` (owns `internal/bonus` HTTP handlers per `ownership.md`) |
| HTTP surfaces: campaign activate/suspend | `UpdateCampaignStatus` + new "consume approval, then activate" wrapper | none | `bonus-engine` |
| HTTP surfaces + business logic: offer create/publish | `CreateOffer`/`CreateOfferVersion`/`UpdateOfferStatus` + new "consume approval, then publish" wrapper | none | `bonus-engine` |
| HTTP surface + job runner: bulk job create/execute | `CreateBulkGrantJob`/`RunStaticBulkGrantJob` (exist, tested) | Needs a decision on **inline vs. background-worker** execution for potentially large jobs — an ordinary engineering call, not a Human Decision Register item, but `architect` should confirm the worker-lifecycle shape (crash-resume semantics already exist per-item; only the *invocation* mechanism is missing) before `bonus-engine` builds it | `bonus-engine` (implementation), `architect` (worker-shape sign-off) |
| Application-level four-eyes wiring for `manual_grant_issue`/`bulk_job_execute`/`campaign_activate`/`offer_publish` | `bonus_change_consume_approved_request` (DB), `ConsumeApprovedChangeRequest` (Go) — both generic and ready | none | `bonus-engine` |
| `bonus_adjustment_write` (manual balance adjustment on a Grant) | No implementing function exists at all — new business logic, not just wiring. Needs a design decision on posting shape (a new `manual_adjustment`-shaped or `bonus_forfeiture`/`bonus_grant`-shaped posting against `player_bonus`) | `ledger-finance` should specify the exact posting shape before `bonus-engine` implements (mirrors how `ledger-finance` specified the `held_disposition_resolve` shapes) | `ledger-finance` (posting-shape spec) → `bonus-engine` (implementation) |
| `grant_forced_conversion` | `ConvertGrant` exists but has no override/force parameter; the "forced" case is a staff bypass of the wagering/AOE block | Needs a design decision: does "forced" mean overriding the wagering-target check only, or also AOE? A wrong answer reopens G-2-adjacent risk (forcing conversion while AOE is non-empty would credit cash against still-open exposure) | `architect` (design, cross-checks against N1.4/T.12's frozen rules) → `bonus-engine` (implementation) |
| `grant_cancel_completed` | `TerminateGrant` structurally cannot run against `completed` — genuinely new business logic | Needs a design decision on the posting shape for clawing back an already-converted-or-completed Grant's value (may already be spent/converted — a straightforward write-down doesn't apply the same way) | `ledger-finance` (posting-shape spec) → `bonus-engine` (implementation) |
| Grant-level expiry (a field to expire against + a sweep) | `TerminateGrant`'s expired path already exists as a mechanism | Needs a schema decision: an `expires_at` column on `bonus_grants` (derived from the Offer's `wagering_time_limit`/`payout_time_limit` at issuance, per doc 10's own W1 "common object contract" precedent) — additive migration, no Human Decision Register item | `bonus-engine` (schema + sweep job), `ledger-finance` co-review (it's a value-reducing transition, same posture as existing forfeiture) |
| Reversal (`MarkGrantReversed`) wiring | Storage-layer function exists and is correct | Needs a decision on which staff action/permission triggers it and its four-eyes shape (the security doc names it under REQ-SEP-BONUS-1's "adjustment, forced conversion" umbrella but doesn't give it its own `ChangeOperation` — worth architect confirming whether reversal reuses `grant_forced_conversion`'s eventual approval flow or needs its own) | `architect` (scope confirmation) → `bonus-engine` (implementation) |
| Deposit/reload/cashback/generic-wagering event wiring | The five bonus-type functions in `types.go` are correct and tested | Needs the actual event source: a `deposit.settled` consumer (payments-owned event, per doc 22's taxonomy) for Deposit/Reload; a scheduled settlement-window job for Cashback; nothing extra for Generic Wagering beyond what manual/bulk grant already provides (it's issuable via those paths today, just not auto-triggered) | `payments` (deposit-event emission, if not already present) + `bonus-engine` (consumer) for Deposit/Reload; `bonus-engine` alone for the Cashback settlement job (no new cross-domain dependency — `NetLossAmount` is computed from ledger reads `bonus-engine` already has access to) |
| `RecordWageringContribution`/`CheckAndCompleteGrant` live wiring | Both functions are correct and tested | **Depends on casino's `postBet` bonus-funded locking side being built** — explicitly named by the directive's own constraints as likely still out of Wave 3's authorized scope (it's the G-2-adjacent casino change Wave 2 §21 already disclosed as not built, gated on jurisdiction/ratification per doc 08's own posture). If Wave 3 does not authorize this, then Generic Wagering/Deposit/Reload/Coupon bonuses remain unable to reach `completed` via real play — this should be stated to the human as an explicit scope question, not silently worked around | `casino` (if authorized) — **flagged, not scoped around** |
| KYC-tier enforcement (`kyc_rg_level_required`) | Nothing — the concept does not exist | Genuinely requires a new shared-domain decision (§3.2) — **not a Human Decision Register item in the licence/jurisdiction/provider sense, but a cross-cutting architecture decision per `ownership.md`'s own framing**, needing `identity-compliance` to design what a tier means before `bonus-engine` can enforce it | `identity-compliance` (design) → `architect` (contract sign-off) → `bonus-engine` (enforcement call site) |
| Multi-account/Person-level abuse detector (`REQ-SEP-BONUS-3`) | `player_accounts.person_id` (already read by `internal/bonus` in `targeting.go`) | None for the minimum `person_id`-linkage slice (§3.3); device/payment-fingerprint correlation is out of scope, needs a real fraud system | `bonus-engine` (owns REQ-SEP-BONUS-3 per the security doc already) |
| `OfferVersion` unenforced axes (jurisdiction/deposit-method/opt-in/VIP-tier/stacking-conflict/max-bet-while-wagering/excluded-games/payout-ordering/partial-release) | Fields exist and are stored | VIP-tier depends on Segmentation (`internal/segment`, explicitly out of scope per standing Orchestrator decision — do not build). Every other field is a Bonus-domain-only enforcement gap with no external dependency | `bonus-engine` |
| Reconciliation: B1(extended) scheduler wiring | Test-proven invariant exists; `internal/reconciliation`'s generic sweep already covers the accounts | None — purely an operational wiring task | `ledger-finance` (owns `internal/reconciliation` scheduling per its own Wave 2 disclosure) |
| LF-10 | N/A | Genuinely open, but confirmed orthogonal to all Wave 3 scope items (§3.4) — do not resolve, do not scope around | `ledger-finance` (unchanged, future dispatch) |
| `REQ-SEP-AFF-1`/`agentnetwork` | N/A | Confirmed purely Affiliate-domain (§4) — not a Wave 3 dependency at all | N/A — no action needed this Wave |

### Recommended phasing for Wave 3's actual implementation dispatch

1. **Phase A — Schema/foundation** (`bonus-engine`, `ledger-finance`
   co-review): `bonus_grants.expires_at`; posting-shape specs for
   `bonus_adjustment_write` and `grant_cancel_completed`
   (`ledger-finance`); the `grant_forced_conversion` scope decision
   (`architect`).
2. **Phase B — Application-level governance wiring** (`bonus-engine`):
   wire all seven un-consumed `ChangeOperation`s into their respective
   Go call sites (campaign activate, offer publish, bulk job execute,
   manual grant issue, adjustment write, forced conversion, cancel
   completed), reusing the migration-0063 infrastructure unchanged.
3. **Phase C — HTTP surface** (`bonus-engine`): the six route groups
   named in §3.1, built directly on Phase B's Go functions.
4. **Phase D — Bonus-domain-only enforcement gaps** (`bonus-engine`,
   parallelizable with B/C): `person_id`-linkage abuse-signal detector
   (REQ-SEP-BONUS-3); the unenforced `OfferVersion` axes list; expiry
   sweep job (depends on Phase A's schema).
5. **Phase E — Independent review** (`security`, `code-reviewer`, `qa`,
   `architect` composition check, `ledger-finance` financial
   certification): mirrors Wave 2's own 11-phase-plus-certification
   structure; must specifically re-verify the four-eyes wiring in Phase B
   actually closes the gap this reconnaissance found (a live adversarial
   test attempting to issue/activate/publish/execute each of the eight
   operations *without* a matching approved `bonus_change_requests` row,
   proven to fail before Phase B and succeed — i.e. be correctly refused
   — after).
6. **Explicitly deferred, named, not silently worked around**: KYC-tier
   enforcement (blocked on identity-compliance's design), deposit/
   cashback event-source wiring (blocked on a payments-event decision and/
   or a settlement-job design the Orchestrator should confirm is in
   scope), `postBet`'s bonus-funded locking side and the entire G-2 live
   trigger path (explicitly out of scope per the directive unless it says
   otherwise), LF-10, and `REQ-SEP-AFF-1`.

---

## 6. Headline numbers

Of the ~32 Wave 3 scope items enumerated in §2: **9 IMPLEMENTED**, **19
PARTIALLY IMPLEMENTED**, **4 NOT IMPLEMENTED** (Offer publish/lifecycle at
any reachable surface; Grant-level expiry as a capability; activity/event
consumption; KYC-tier enforcement). The single largest, most
consequential finding this reconnaissance adds beyond what Wave 2's own
report disclosed: **of the eight dual-controlled bonus operations the
security architecture specifies, only one (`held_disposition_resolve`)
has real Go-level four-eyes enforcement wired today** — the other seven's
governance infrastructure is built and generic, but genuinely unused by
any application code, which is a materially different (and larger) gap
than "missing HTTP admin surface" alone.
