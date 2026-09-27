# ADR 0096 — KYC Enforcement Boundary

Status: **PROPOSED.** `identity-compliance`, for the orchestrator, under
the "PAYMENT READINESS & PROVIDER-INDEPENDENT HARDENING" (PRH) authorized
work, item **PRH-D2**, closing the reconnaissance and design half of
**KYC-ENFORCE-1** (`docs/governance/task-registry.md`). Design/docs only.
No code, no migration, no commit. Allocated migration: **0101**.

Baseline: branch `claude/focused-wright-jw88w9`, `HEAD 1560ad0`. Sources
read: `CLAUDE.md`; task registry KYC-ENFORCE-1 row and the PRH section;
ADR 0006 (hybrid licensing), 0007 (multi-wallet), 0026 (RG foundation,
its §14 KYC/AML extension-point note), 0028 (KYC provider abstraction,
with its Stage 10.2/10.3 amendments — webhook trust, reason bound,
HD-10.3-3), 0031 (risk & limits engine, its Bonus/Gamification §14
integration contract), 0034, 0041/0041-brief/0043 (jurisdiction human
decisions and evaluation-policy configuration), 0047, 0082 (canonical
lock ordering), 0083; `docs/plans/next-real-provider-integration-
planning-gate.md` (§4, §6 row 23/24, §14 KYC-ENFORCE-1 row, §15 human
decisions); live code in `internal/withdrawal`, `internal/payments`,
`internal/casino`, `internal/sportsbook`, `internal/bonus`,
`internal/rg`, `internal/risk`, `internal/kyc`, `internal/jurisdiction`,
`internal/httpserver`.

No vendor is selected, no vendor API is invented, and no legal threshold
value is asserted. This paper builds the enforcement **mechanism** and
records every threshold/market **value** as an open human decision.

---

## 1. Reconnaissance — every money/value movement path

"Enters/leaves" = crosses the platform boundary to/from an external rail
(a PSP, a casino/sportsbook aggregator's own settlement, a crypto
network) or to/from a person outside the platform. "Internal" = moves
between ledger accounts the platform itself controls; nothing leaves.

Verdict legend: **ENFORCE** = this ADR designs a KYC gate here.
**NOT-ENFORCE** = no KYC gate here, with reason. **HUMAN-DECISION** = the
mechanism could gate here, but whether it *should*, and at what value,
is not an engineering call.

| # | Path | Entry point (file:line) | Enters/leaves/internal | Existing gates, in order | KYC verdict | Reason / citation |
|---|---|---|---|---|---|---|
| 1 | Deposit initiation | `internal/payments/orchestrator.go:474` `InitiateDeposit`, RG at `:551`, provider call `attemptDeposit` `:581` | **Enters** (external PSP funds the wallet) | RG (`rg.EvaluateEligibility`, `:551`) → provider routing/call. No Risk (ADR 0031 §13 table: payments `NOT IMPLEMENTED`). No KYC. | **ENFORCE** (threshold-gated; see §3) | BLUEPRINT §4.7 "tiered KYC … cumulative deposit thresholds"; registry KYC-ENFORCE-1 explicitly names "deposit … play" |
| 2 | Deposit callback / crediting | `internal/payments/orchestrator.go` `ReceiveCallback` → settlement of a pending intent | **Enters** (confirms funds already received) | Verify-before-parse webhook trust (ADR 0022/0094); no RG/Risk/KYC re-check at settlement | **NOT-ENFORCE** | The gate belongs at *initiation* (row 1), before the provider is ever called. Re-gating at settlement cannot un-receive funds already sent by the PSP and would only decide what to do with money already inbound — a different, already-covered problem (deposits are never blocked from being *credited*, only from being *initiated*) |
| 3 | Withdrawal request (hold placement) | `internal/withdrawal/withdrawal.go:291` `RequestWithdrawal` | **Internal** (places a hold; no funds leave yet) | None (no RG, no Risk, no KYC) — confirmed by reading the function; only balance sufficiency is checked | **ENFORCE — every withdrawal, not only the first** (revised per security condition 1 / ledger-finance C3, §3.2) | BLUEPRINT §4.7 names "first withdrawal" as the tier at which KYC becomes mandatory; this ADR's mechanism enforces it as "mandatory from the first withdrawal **onward**," never as a one-time exemption a later `rejected`/`expired` status could ride through — see §3.2 for why the original "wallet with zero prior completed withdrawals" framing was a security defect, not a faithful reading of Blueprint's own tier |
| 4 | Withdrawal promotion to review | `internal/withdrawal/withdrawal.go:429` `MoveToPendingReview` | Internal (state transition only) | None; the function's own doc comment says this is exactly where "automated KYC/velocity/risk checks are queued (owned by identity-compliance, not this package)" | **NOT-ENFORCE at this exact call** — the doc comment's promise is honored by gate #3 running before this state is ever reached, not by adding a second check inside this transition itself, so a request already past a KYC deny never reaches `pending_review` in the first place | Avoids a second read of the same fact for no new information — see §3 for why one early gate is sufficient given #5 is the hard backstop |
| 5 | Withdrawal payout dispatch | `internal/withdrawal/withdrawal.go:863` `LockApprovedForSubmission` (locks the row; caller then calls the provider and `:1006` `MarkSubmitted`) | **Leaves** (this is the point the platform hands funds to an external payout rail) | Four-eyes `Approve`/`Reject` (policy.go); no RG, no Risk, no KYC | **ENFORCE — the hard backstop** ("at minimum before payout submission", registry KYC-ENFORCE-1) | BLUEPRINT §4.7; CLAUDE.md "enforcement is our code, not the vendor's" |
| 6 | Withdrawal reject/cancel/reverse | `withdrawal.go:731,892` `Reject`, `LockSubmittedForResolution` | Internal (reverses the hold; no leave) | Existing state machine | **NOT-ENFORCE** | A correction path, not a new leave-the-platform event; nothing to gate |
| 7 | Casino bet placement | `internal/casino/orchestrator.go:952` `postBet` | Internal (debits `player_cash`, credits the provider-facing liability account; no external rail) | RG (`evaluateAndAuditEligibility`) → Risk (`evaluateAndAuditRisk`), ADR 0031 §1 | **ENFORCE (policy-driven, default `not_required`)** | The human directive names casino bets as an identified enforcement surface; this ADR does not silently drop that surface even though the *default* behaviour (no active jurisdiction policy) is unchanged from NOT-ENFORCE in practice. See §3.5 for the "play" trigger design — no money crosses the platform boundary at a bet, so the mechanism defaults to `not_required` absent an explicit, legally-reviewed KYC-before-play policy row (HD-KYC-8), never a compiled-in default the way withdrawal's structural rule is |
| 8 | Casino win | `internal/casino/orchestrator.go:1456` `postWin` | Internal (credits `player_cash` from the settled bet's liability account) | Deliberately none (own doc comment: not RG-gated, "a win is the platform paying out on its own already-accepted bet, not a new player-initiated action") | **NOT-ENFORCE** | Same reasoning as #7, one level stronger: gating a *settlement* of an already-accepted wager on a compliance state that may have changed since the bet would strand the platform's own already-incurred liability with no correction mechanism — exactly the class of harm `postRollback`'s doc comment names for RG |
| 9 | Casino rollback | `internal/casino/orchestrator.go:1586` `postRollback` | Internal (reverses #7/#8) | Deliberately none (own doc comment: "a correction to history, not a new stake") | **NOT-ENFORCE** | A correction must remain possible for exactly the players most likely to need one (own doc comment, quoted verbatim) — gating it on current KYC status would make the ledger un-correctable |
| 10 | Sportsbook bet placement | `internal/sportsbook/orchestrator.go:583-616` | Internal (same wallet-debit shape as casino bet) | RG → Risk, mirroring casino (ADR 0031, this file's own comments) | **ENFORCE (policy-driven, default `not_required`)**, with **HUMAN-DECISION** flagged | Same design as #7 (§3.5). Separately flagged because Blueprint's "EDD above configurable limits" tier does not specify whether "configurable limits" is scored only against deposit/withdrawal amounts or also against stake size; this ADR does not assume the latter (§3.2 point 2, HD-KYC-2) |
| 11 | Sportsbook settlement/void/cashout | `internal/sportsbook/settlement.go` | Internal | Existing settlement state machine, no RG/Risk re-check (matches casino win/rollback precedent) | **NOT-ENFORCE** | Same as #8/#9 |
| 12 | Bonus grant (`issued`) | `internal/bonus/eligibility.go:95-105` (`AssetAuthorization.CheckEligibility`) | Internal (creates a contingent, non-withdrawable liability) | RG → Risk (`OperationBonusGrant`), ADR 0031 §15a | **NOT-ENFORCE** | No cash leaves and none is withdrawable yet; ADR 0031 §15a's own analysis (composing RG+Risk, never a third engine) does not name KYC, and Blueprint does not tie a KYC tier to bonus issuance |
| 13 | Bonus activation (`issued`→`activated`) | `internal/bonus/eligibility.go` reusing `OperationBonusGrant` (ADR 0031 §15a-ii) | Internal (funds now sit in the wallet as wagering-locked, still non-withdrawable) | RG → Risk | **NOT-ENFORCE** | Same as #12: not withdrawable yet; the withdrawal gate (#3/#5) is the actual money-leaves-platform checkpoint that ultimately governs this value |
| 14 | Bonus wagering contribution | `internal/bonus/wagering_contribution_entry.go` | Internal | None additional (rides the bet path's own gates, #7/#10) | **NOT-ENFORCE** | Not a distinct money-movement decision point; it is bookkeeping attached to #7/#10 |
| 15 | Bonus completion/conversion (`completed`→`converted`) | `internal/bonus/conversion.go` (`OperationBonusConversion`, ADR 0031 §15a-ii) | Internal (releases the amount as ordinary, withdrawable `player_cash` — still inside the platform) | RG → Risk | **NOT-ENFORCE at conversion itself**, with **HUMAN-DECISION** flagged | Conversion makes value withdrawable but does not itself move it off-platform; the withdrawal gate (#3/#5) is what stops an unverified player from ever actually extracting it. Flagged as a human decision because an AML case (using bonus abuse to *test* value without ever completing a withdrawal, e.g. multi-accounting or card-testing via bonus wagering) is a real pattern this ADR does not rule out — but inventing a KYC trigger here with no Blueprint citation and no legal input would be exactly the scope expansion CLAUDE.md warns against |
| 16 | Cashback grant | `internal/bonus/cashback_scheduler.go` | Internal | Presumed to route through the same grant path as #12 (not independently re-verified this ADR; if it bypasses `bonus_grant`'s RG/Risk gate, that is a separate, pre-existing finding outside KYC-ENFORCE-1's scope, flagged for `identity-compliance`/`bonus` follow-up) | **NOT-ENFORCE** | Same reasoning as #12 |
| 17 | Free rounds | Casino free-round grant (not yet built as a distinct settlement path — ADR 0025 amendment item 8, "free-round/jackpot conformance case", `NOT IMPLEMENTED`) | Internal, and not real money until wagered | — | **NOT-ENFORCE**, not reachable | Feature does not exist yet; when built it inherits #7/#8's reasoning, not a new one |
| 18 | Wallet-to-wallet `ConversionOperation` (ADR 0007) | Not found in `internal/*` — **`NOT IMPLEMENTED`** (design-only in ADR 0007/0019/0021/0032/0035/0037) | Would be internal (moves value between two of the *same* player's wallets of different assets; nothing leaves the platform) | N/A | **NOT-ENFORCE**, not reachable; **HUMAN-DECISION recorded for when built** | ADR 0007 is explicit this is never a direct balance mutation; when implemented, this ADR's position is that it does not need its own KYC gate (no boundary crossing), but the domain owner should confirm at design time, not assume it silently |
| 19 | Crypto deposit/withdrawal rails | Not implemented; blocked on `docs/architecture/crypto-custody-boundary.md` §4.1 (ADR 0008) | Would enter/leave via a `CryptoCustodyProvider` | N/A | **Deferred, same shape as #1/#5 once built** (HUMAN-DECISION: rail choice) | ADR 0008: the platform owns wallet/ledger/orchestration, the custodian owns keys; KYC enforcement composes at the same `OperationDeposit`/`OperationWithdrawal` boundary this ADR designs, not a new domain-specific one |
| 20 | Manual back-office credit/debit (adjustment) | `LEDGER-MANUAL-ADJ-4EYES-1` (registry) — no four-eyes manual-adjustment API exists yet | Would be internal (a staff-initiated ledger correction, not a rail) | Not built | **NOT-ENFORCE**, out of scope | Governed by a different control (four-eyes + reason code, CLAUDE.md Security section), owned by `ledger-finance`/`security`, not a KYC concern; noted so it is not silently assumed covered |
| 21 | Affiliate / any other payout | `internal/economicop` — schema/plumbing only for `EconomicOperationIdentity` lineage (doc 34); no payout execution exists | N/A today | N/A | **NOT-ENFORCE**, not reachable; **flagged for when built** | If a future affiliate payout leaves the platform to a *person* (not a player), it needs its own KYC/AML analysis analogous to a withdrawal — recorded, not designed here (no such feature exists) |

### 1.1 Verdict summary

| Verdict | Count | Rows |
|---|---|---|
| ENFORCE | 5 | #1, #3, #5, #7, #10 |
| NOT-ENFORCE | 14 | #2, #4, #6, #8, #9, #11, #12, #13, #14, #15, #16, #17, #18, #21 |
| Deferred, not reachable (no verdict yet meaningful) | 1 | #19 (crypto rail) |
| HUMAN-DECISION flagged (overlaps ENFORCE/NOT-ENFORCE rows above, not a disjoint count) | #10, #15, #18, #19, #21, plus HD-KYC-8 (§4) |

Rows #7 and #10 are **ENFORCE** as of the orchestrator's amendment
(2026-09-27): a policy-driven "play" trigger is designed in §3.5 so the
surface is never silently dropped, even though its *default* outcome
(`not_required`, no active policy) leaves today's behaviour unchanged
until a jurisdiction policy is authored. Row #4 is a "not a second check"
call, folded into row #3's design, not a gap — see §3.

---

## 2. Deterministic enforcement boundary — `internal/kyc.EvaluateEnforcement`

### 2.1 Why a new function, not a field added to `rg.EvaluateEligibility`

`internal/rg`'s own doc comment (`rg.go:559-563`) speculates a future
"KYCAMLChecker parameter … slotting in as one more Decision-producing
step" inside `EvaluateEligibility` itself. This ADR does **not** take
that path, for the same reason ADR 0031 §1 keeps Risk structurally
separate from RG rather than merging them: RG answers "may this player
gamble at all right now" (account/wallet status, self-exclusion) — a
single, symmetric yes/no over the whole relationship. KYC enforcement
answers a **different-shaped** question — "does *this specific operation,
at this amount, in this jurisdiction* currently require a passed
verification, and does the player have one" — which is asymmetric per
operation and per amount, exactly like `risk.Evaluate`. Folding it into
`rg.EvaluateEligibility` would force every RG caller (including ones that
never handle money, if any exist later) to also supply amount/asset/
operation context RG has never needed, and would re-introduce the
"is this really RG or is it a limit engine under another name" confusion
ADR 0031 §1 explicitly forecloses for Risk. KYC gets its own package
function, called at a fixed position relative to RG and Risk (§2.4), the
same composition pattern `internal/bonus/eligibility.go` already
establishes for RG+Risk together.

### 2.2 Signature

```go
package kyc

// EnforcementOperation is the closed set of money-boundary operations
// this ADR's mechanism gates. Mirrors risk.Operation's own "small,
// additive, never speculative" discipline (ADR 0031 §4/§12) — a new
// value costs an explicit, reviewed extension, never an implicit one.
type EnforcementOperation string

const (
	EnforcementDeposit          EnforcementOperation = "deposit"
	EnforcementWithdrawalHold   EnforcementOperation = "withdrawal_hold"   // row #3
	EnforcementWithdrawalPayout EnforcementOperation = "withdrawal_payout" // row #5
	EnforcementCasinoPlay       EnforcementOperation = "casino_play"       // row #7 — policy-driven, §3.5
	EnforcementSportsbookPlay   EnforcementOperation = "sportsbook_play"   // row #10 — policy-driven, §3.5
)

// EnforcementParams is EvaluateEnforcement's input. Every identity field
// MUST be resolved server-side from the authenticated/tenant context —
// never accepted from a request body — identical to
// rg.EligibilityParams/risk.RiskRequest's own binding contract.
type EnforcementParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	PersonID        uuid.UUID // required for the withdrawal structural rule (§3.2 point 1), which is scoped per Person, not per wallet/PlayerAccount — resolved server-side exactly like every other identity field, never accepted from a client
	Operation       EnforcementOperation
	AssetCode       string // required for amount-shaped operations
	Amount          int64  // minor units; required for deposit/withdrawal; player-chosen, so §12.2 C6/§13 condition 4(e) require the server to recompute cumulative totals independently rather than trust this field for anything beyond the current transaction's own amount check
	// LicensingJurisdictionID is the ONLY jurisdiction selector this
	// mechanism reads, resolved from tenants.licence_id ->
	// licences.jurisdiction_id exactly as jurisdiction.
	// ResolveEvaluationPolicy already does internally (ADR 0043 Decision
	// 2) — the KYC policy table is keyed the same way, for the same
	// bootstrap-circularity reason.
	LicensingJurisdictionID uuid.UUID
	// JurisdictionCode is REMOVED (security condition 4(d), §13). The
	// original sketch carried it as a second, player-influenceable
	// jurisdiction signal (derived from geolocation or declared country,
	// the same value risk.RiskRequest.JurisdictionCode carries per ADR
	// 0031 §9) alongside LicensingJurisdictionID. migration 0101's
	// kyc_enforcement_policies table has no column it could ever match
	// against, so it was dead weight at best — but a future
	// implementer adding one "for symmetry with risk.Evaluate" would
	// create a real vector: a player influencing their own declared
	// country (VPN, self-reported location) to select a laxer KYC
	// policy than their actual licensing jurisdiction governs. This ADR
	// forecloses that explicitly rather than leaving it to be
	// rediscovered at implementation time: KYC policy selection reads
	// LicensingJurisdictionID only, never a player-influenceable
	// location signal, full stop.
	CorrelationID uuid.UUID
}

// EnforcementOutcome is one of exactly five normalized values — never a
// raw kyc_verifications.status string, and never a boolean. Distinct
// values are required (not collapsible into Allowed/Denied) because the
// audit trail and a future compliance queue both need to distinguish
// "not required" from "required and passed" from "required but pending"
// from "the evaluator itself could not tell" — the identical rationale
// risk.RiskDecision.Outcome and kyc.ProviderResult.Outcome already use
// (ADR 0028 §5, ADR 0031 §2).
type EnforcementOutcome string

const (
	OutcomeNotRequired EnforcementOutcome = "not_required" // policy does not currently require KYC for this operation/amount/jurisdiction
	OutcomePassed       EnforcementOutcome = "passed"        // required, and the player's LATEST kyc_verifications row (§2.6) is approved AND unexpired
	OutcomePending       EnforcementOutcome = "pending"       // required; the latest row is pending/review_required
	OutcomeFailed        EnforcementOutcome = "failed"        // required; no verification exists, or the latest row is rejected, OR the latest row is approved but its expires_at has passed (§2.6(b)) — expiry is folded into failed, not its own outcome value
	OutcomeUnavailable    EnforcementOutcome = "unavailable"   // the evaluator itself could not determine an outcome (a DB/query error, or a malformed policy row) — NEVER "no rows found"; never a legitimate business state; a query failure must never be interpreted as not_required (§2.6(c))
)

type EnforcementDecision struct {
	Outcome       EnforcementOutcome
	Allowed       bool   // the ONE field every caller actually branches on
	Code          string // stable machine code, mirrors risk.RiskDecision.Code
	Message       string
	MatchedTrigger string // which policy row (if any) produced this outcome — staff/audit only, never player-facing
	PolicyVersion string
}

// EvaluateEnforcement is the single authoritative "may this money-
// boundary operation proceed given the player's current KYC state"
// policy boundary. It is a pure database read plus in-process
// comparison — it NEVER calls a KYC vendor (internal/kyc's own
// verification/vendor calls are a completely separate concern, already
// isolated behind KYCProvider, ADR 0028 §4) — so it can run inside the
// caller's own domain transaction with no provider-I/O risk (F-POOL-2's
// concern does not apply here: no outbound call exists on this path).
//
// Any non-nil error is a DENY at the call site, per the identical
// fail-closed contract risk.Evaluate already establishes (ADR 0031 §6)
// — there is no "KYC-specific softening" of an evaluator failure.
func EvaluateEnforcement(ctx context.Context, tx pgx.Tx, params EnforcementParams) (EnforcementDecision, error)
```

### 2.3 Outcome → allow/deny mapping

| Outcome | Allowed | Notes |
|---|---|---|
| `not_required` | **allow** | The policy governing this jurisdiction/operation/amount does not currently require a passed verification — either because no threshold has been configured yet (§3.2, dormant by design, not a security gap) or because the amount is genuinely below a configured threshold |
| `passed` | **allow** | The player's current `kyc_verifications` row for this tenant/brand is `approved` |
| `pending` | **deny** | No exception. A verification in flight is not a passed one; CLAUDE.md's Authority section forbids weakening this to "allow while pending" without an explicit recorded decision, which is not made here |
| `failed` | **deny** | Covers "never verified", `rejected`, and `expired` uniformly — the caller does not need, and this decision does not expose, which of the three it was (that distinction stays in `kyc_verifications`/staff tooling) |
| `unavailable` | **deny** | Fail-closed exactly like a non-nil `Evaluate` error; distinguished from a true error return only so the audit row can record *why* evaluation could not proceed (e.g., a malformed policy row) without every such case going through Go's `error` path |

There is **no policy-configurable way to turn `pending`/`failed` into an
allow**. This is a deliberate mechanism constraint, not an oversight: the
Authority section of this specialist's charter is explicit that
"[the identity-compliance specialist] cannot weaken an RG or KYC
enforcement rule to ease a product flow." A future jurisdiction that
genuinely needs different tiering expresses it through `not_required`
(a different threshold/trigger configuration), never through relaxing
what `passed` means.

### 2.4 Composition order with RG and Risk

Preserves every existing tested call order; KYC is inserted as a new
step, never reordering what already exists:

| Path | Order before this ADR | Order after this ADR |
|---|---|---|
| Deposit initiation (`payments.InitiateDeposit`) | RG | **RG → KYC** (Risk: `NOT IMPLEMENTED` for payments per ADR 0031 §13, unaffected) |
| Withdrawal request (`withdrawal.RequestWithdrawal`) | none | **KYC** (no RG exists on this path today — see §5, out of this ADR's scope to add) |
| Withdrawal payout dispatch (`withdrawal.LockApprovedForSubmission`, before the caller invokes the provider) | none | **KYC** |
| Casino bet placement (`casino.postBet`) | RG → Risk | **RG → Risk → KYC** — KYC evaluated last, immediately before the balance lock/ledger post, exactly where RG/Risk already sit relative to it today (ADR 0031 §7: "after the RG check, before the balance lock"). Placed after Risk, not before or between, so neither existing tested call order is disturbed: an RG or Risk denial still short-circuits before KYC ever runs, and KYC's own `not_required` default (§3.5) means this new step is a no-op read on every path until a jurisdiction policy exists |
| Sportsbook bet placement (`sportsbook.orchestrator.go` RG at `:593` → Risk at `:616`) | RG → Risk | **RG → Risk → KYC**, identical placement and rationale to casino bet placement above |
| Casino win/rollback, sportsbook settlement/void/cashout, bonus grant/activation/conversion | RG → Risk (win/rollback: deliberately neither) | **unchanged** — no KYC call added (§1 verdicts; wins/rollbacks/settlements stay corrections, not new stakes) |

Rationale for "RG first, then KYC" on deposit: RG's check is cheaper (no
jurisdiction/amount resolution) and already the tested, audited
first step; inserting KYC after it means an RG-denied player never
reaches KYC evaluation at all, and every existing RG regression test for
that call site is untouched by this change (KYC is a new, additive step
after the existing return path, not a reordering of it).

### 2.5 The result can never be player-supplied

`EnforcementParams` carries no field a client can set — `TenantID`/
`BrandID`/`PlayerAccountID` come from the authenticated context exactly
like every other financial boundary in this codebase (`rg.
EligibilityParams`, `risk.RiskRequest`, `withdrawal.RequestParams`).
`AssetCode`/`Amount` come from the domain's own already-validated request
(the same deposit amount RG/ledger already use, the same withdrawal
amount already locked against the ledger). The **outcome itself** is
derived entirely server-side from `kyc_verifications` (read under the
caller's own tenant-scoped transaction, subject to existing RLS) and the
policy table in §3 — never from a request header, a client-asserted
"verified" flag, or a cached value. `EvaluateEnforcement` performs its
own read; it does not accept a pre-computed `Outcome` from any caller.

### 2.6 Read semantics (security condition 4, §13 — binding, not advisory)

`kyc_verifications` carries no per-player uniqueness constraint
(migration 0040) — a player can accumulate several rows over time
(a rejected attempt, a later approved one, a future re-verification).
`EvaluateEnforcement`'s read must therefore be pinned down exactly, not
left to "the obvious query," because the obvious query is the bug:

- **(a) Latest row only, deterministically ordered.** The query selects
  the single row for `(tenant_id, brand_id, player_account_id)` ordered
  `created_at DESC, id DESC` and takes the first result. An
  `EXISTS(status = 'approved')`-shaped query is explicitly the wrong
  design — it would let an old, superseded `approved` row satisfy the
  check even when a newer row for the same player is `pending`,
  `rejected`, or `expired`. Only the latest row's status is ever
  consulted.
- **(b) Expiry is enforced independently of stored status.** If the
  latest row's `expires_at <= now()`, the outcome is `failed` even when
  its stored `status` column still literally reads `approved` — `kyc_
  verifications.status` only transitions to `expired` when a provider
  delivers that update (ADR 0028 §2's state machine), and a missed or
  never-sent callback must not silently extend a verification's real-world
  validity. This requires `kyc_verifications` to carry (or `Evaluate
  Enforcement` to otherwise resolve) an `expires_at` value; if the column
  does not yet exist on that table, adding it is part of this ADR's own
  implementation scope (PRH-I3), not a follow-up.
- **(c) A lookup error is `unavailable`, never `not_required`.** A failed
  query (connection error, malformed row, an unrecognized policy shape)
  must propagate as `OutcomeUnavailable` (deny). "No rows returned" and
  "the query itself failed" are different states and must never be
  conflated — mapping a query failure to "no policy, no verification,
  therefore not required" would turn an outage into a silent bypass,
  exactly the inversion CLAUDE.md's fail-closed rule forbids.
- **(d) Policy selection key.** `kyc_enforcement_policies` is selected
  **only** by `LicensingJurisdictionID` (§2.2) — never by a player-
  influenceable location signal. See §2.2's removal of `JurisdictionCode`
  for the full reasoning.
- **(e) Threshold amounts are computed server-side, from settled ledger
  postings, across the player's relevant activity — never from the
  current request's own `Amount` alone.** Once HD-KYC-1/HD-KYC-2 supply
  real values, the `cumulative_deposit`/`edd_amount` comparison must sum
  **settled** deposit postings (ledger `deposit_completed` credits, per
  ledger-finance C6 in §12.2 — never `deposit_intents.amount`, which
  includes declined and still-pending intents) for the player, in the
  trigger's own `asset_code`, and must **include in-flight (initiated,
  not yet settled) amounts** in whatever way HD-KYC-1 specifies, so a
  player cannot structure around the threshold by keeping one deposit
  perpetually "pending." `Amount`/`AssetCode` on `EnforcementParams`
  remain player-influenceable inputs (they are the current request's own
  amount) and are never trusted as the sole basis for a cumulative
  comparison — only as the amount of the transaction being gated right
  now, checked against a total the server computed independently.
- **(f) Cross-tenant negative test shape.** Any test proving cross-tenant
  isolation must use a genuinely valid token for tenant B carrying tenant
  A's `player_account_id`, and assert the caller-visible result is a
  rejection or not-found — never that the evaluation silently ran against
  tenant A's own rows under tenant B's authorization context.

---

## 3. Configuration model — reusing the jurisdiction architecture

### 3.1 Why this reuses, and does not duplicate, `jurisdiction_precedence_configs`'s pattern

ADR 0043 already solved "effective-dated, append-only, legally-reviewed,
licensing-jurisdiction-keyed policy configuration, not tenant-keyed" for
`jurisdiction_precedence_configs`. This ADR reuses that **pattern**
(the key shape, the draft/active/withdrawn lifecycle, the append-only
provenance, the `reason_code`/`legal_review_reference` requirement, the
platform-admin-only write authorization) rather than literally adding
more columns to that same table, because the *shape* of the content is
genuinely different: `jurisdiction_precedence_configs` rows are one
policy per `(licensing_jurisdiction_id, operation_class, effective_from)`
tuple; KYC threshold policy is **N numeric triggers per jurisdiction**
(a cumulative-deposit amount, an EDD amount, a registration-tier flag),
which does not fit a single-policy-per-key shape without inventing a
JSONB blob whose internal shape `ResolveEvaluationPolicy`'s own callers
would then have to parse — the same anti-pattern ADR 0031 §12 rejects for
"faking a limit kind." A sibling table with the identical conventions is
therefore the correct reuse, mirroring how `risk_rules` is its own table
even though it is conceptually adjacent to `player_restrictions` — never
a parallel *versioning or authorization mechanism*, which is the thing
actually being reused here.

`internal/jurisdiction`'s existing `OperationClass` enum (`play`,
`catalogue_availability`, `bonus_issuance`, `bonus_conversion`) is **not**
reused or extended by this ADR — it classifies a materially different
question (does this jurisdiction currently allow this class of operation
at all) from KYC's (does this specific operation at this amount currently
require a passed verification). Conflating the two enums would create
exactly the kind of implicit coupling ADR 0031 §15e/§16 explicitly
rejects for its own `Operation` values. `kyc.EnforcementOperation` (§2.2)
is deliberately its own, narrower vocabulary.

### 3.2 Two kinds of trigger: structural (ships now) vs. threshold (values pending)

Blueprint §4.7's tiered list has two different characters:

1. **Structural — "KYC passed is mandatory for every withdrawal, from the
   first one onward."** REVISED (security condition 1, ledger-finance
   C3): the original sketch computed this as a one-time exemption
   ("has this wallet ever completed a withdrawal before") — a player who
   passed once, withdrew once, and was later found `rejected`/`expired`
   (a forged document, a chargeback investigation) could withdraw a
   second and every subsequent time completely ungated whenever no
   `edd_amount` policy happened to be active, because the exemption
   never re-checked current status. That is value leaving the platform
   against a known negative KYC state, and it is a bug in the design,
   not a defensible reading of Blueprint §4.7's "first withdrawal" tier.
   Blueprint names *when KYC becomes mandatory* (the first withdrawal
   attempt), not *when it stops applying*. The corrected rule, compiled
   in and always-on with no policy row needed, in every jurisdiction:
   **every withdrawal request (row #3) and every withdrawal payout
   dispatch (row #5) requires the player's current, latest verification
   to be `passed` and unexpired (§3.6 point (b)), full stop — there is
   no "already withdrawn once" exemption.** It is scoped **per `Person`
   identity, not per wallet or per `PlayerAccount`** (ADR 0007's
   multi-wallet model; ADR 0028's own `person_id` anchor on
   `kyc_verifications`) so that opening a new wallet, or a new
   `PlayerAccount` under the same `Person`, cannot reset or bypass it —
   correcting the sketch's original per-wallet framing, which security
   separately flagged as a scoping gap even before the exemption defect.
   A jurisdiction wanting a genuinely different structural rule (e.g.
   relaxing this) needs its own recorded human decision (§4, HD-KYC-5),
   never a silent per-tenant override — matching the Authority
   constraint in §2.3. There is accordingly **no `first_withdrawal`
   `trigger_type`** in migration 0101 (§3.6) — this rule is not a
   configurable row at all, exactly as the original sketch already
   stated, now applied correctly (always-on, not a one-time gate).
2. **Threshold — cumulative deposit amount, EDD amount, registration
   tier.** These are exactly the values HDR-J-6 and legal review have
   not yet supplied. The mechanism (migration 0101, below) exists and
   is fully wired to `EvaluateEnforcement`, but **carries no seed rows**
   and defines no default numeric value anywhere, including in
   non-test code. A jurisdiction with **no active policy row** for a
   given `(licensing_jurisdiction_id, trigger_type)` evaluates that
   trigger as **dormant**: it never fires, and `EvaluateEnforcement`
   returns `not_required` for it. This is the "safe behaviour when
   policy is unconfigured" this ADR is asked to define, and it is
   asymmetric by design:
   - For **row #1 (deposit)**, "dormant" resolves to **allow** (deposits
     are not blocked pending a legal value that does not exist yet —
     this is the platform's *existing* behaviour today, so shipping the
     mechanism does not newly weaken anything; it adds the capability to
     turn the gate on the moment HDR-J-6 is answered, without a code
     change). **This "allow when unconfigured" default is an explicit
     design choice, submitted here for `security` review, not an
     unexamined default** — `security`'s review (§13) accepted it
     conditionally, "only together with C1 and C6": the justification
     that "value cannot leave through a deposit, and the withdrawal gate
     is the backstop" holds only because point 1 above now gates **every**
     withdrawal (not only the first — the defect `security` condition 1
     and `ledger-finance` C3 found and this revision fixes), so the
     backstop this ruling depends on has no hole left in it. It is safe
     specifically *because* value cannot leave the platform through a
     deposit — the structural rule in point 1 above is the actual
     backstop that prevents an unverified player from ever extracting
     funds, deposited or otherwise. `security` further noted this ruling
     (a) covers only today's paths (no `internal/payments` refund-to-
     source execution path exists yet, and a future crypto rail, row
     #19, can receive funds with no initiation step to gate at all — both
     must be re-reviewed against this ruling, not assumed to inherit it)
     and (b) is a mechanism description, not a compliance position: going
     live in a jurisdiction with no active `cumulative_deposit` policy
     means unverified players can deposit without limit and *place*
     funds (the first AML stage) even though they can never extract them
     — that is HD-KYC-1 plus legal review to accept or reject before
     launch, not something this default pre-decides. See condition 9
     (§13) for the operational-visibility requirement ("make dormancy
     observable") this ruling is also conditioned on. If a future
     reviewer finds a reason deposits need their own
     fail-closed-when-unconfigured posture independent of the withdrawal
     backstop, that is a new finding for this ADR to absorb, not something
     this default
     silently forecloses.
   - For **rows #3/#5 (withdrawal)**, the threshold triggers are
     genuinely optional refinements **on top of** the always-on
     structural rule in point 1 above — "dormant" only ever means "no
     *additional* EDD tier is active beyond the mandatory `passed`
     check every withdrawal already requires," never "no gate at all."
     Withdrawal is never left ungated purely because a threshold value
     is missing, which is the fail-closed-for-value-leaving-the-platform
     posture this ADR is asked to justify explicitly, and which no
     longer depends on the withdrawal being the player's *first* one.
     **This is a security/human-review design choice, recorded here, not
     an automatic consequence of the schema**: a future reviewer could
     instead choose to make even the structural "passed required on
     every withdrawal" rule itself jurisdiction-configurable and
     dormant-by-default; this ADR recommends against that (HD-KYC-5),
     because Blueprint states the tier unconditionally and no legal
     value is needed to honor it.

### 3.5 A third kind of trigger: policy-driven "play" (casino/sportsbook bet placement)

The orchestrator's amendment (2026-09-27) is explicit that casino bets
are a named enforcement surface and must not be dropped silently, even
though this ADR's own default analysis (§1 rows #7/#10, ADR 0026 §14 and
ADR 0031's precedent that KYC ties to registration/deposit/withdrawal/EDD
tiers, not to every stake) remains correct as the **default**. The
resolution is a third trigger kind, distinct from both "structural"
(point 1) and "threshold" (point 2):

- **`play` is neither compiled-in nor amount-shaped.** Unlike the
  first-withdrawal rule, it carries **no safe default value to ship
  unconditionally** — no Blueprint text states "every jurisdiction
  requires KYC before any bet," so this ADR does not compile that in.
  Unlike `cumulative_deposit`/`edd_amount`, it has no numeric threshold at
  all — it is a pure on/off fact per `(licensing_jurisdiction_id,
  operation)`. It exists as its own `trigger_type` (§3.6/migration 0101)
  precisely so the *capability* to require KYC-before-play is real and
  reviewable the moment a jurisdiction needs it, without a schema or code
  change — the same "mechanism now, values later" contract as every
  other trigger in this ADR.
- **Default: `not_required`.** With no active `play` policy row for a
  given `(licensing_jurisdiction_id, casino_play)` or
  `(licensing_jurisdiction_id, sportsbook_play)` pair,
  `EvaluateEnforcement` returns `not_required`, and today's behaviour
  (RG → Risk, no KYC call's outcome ever denies) is unchanged in
  practice. This mirrors the deposit default's reasoning (point 2 above)
  and is deliberately **not** given the withdrawal default's
  fail-closed-by-structural-rule treatment, because — unlike a
  withdrawal — a bet never moves value off the platform; the eventual
  withdrawal gate (§3.2 point 1) is still the backstop that decides
  whether any winnings can ever actually leave.
- **Fail-closed only once configured.** The instant a jurisdiction
  authors an `active` `play` row (with its own required
  `legal_review_reference`/`reason_code`, exactly like every other active
  policy row), `EvaluateEnforcement` for that jurisdiction's
  `casino_play`/`sportsbook_play` operation starts returning `passed`/
  `pending`/`failed`/`unavailable` from the player's real
  `kyc_verifications` state, and §2.3's ordinary mapping applies with no
  exception: `pending`/`failed`/`unavailable` all deny, uniformly, the
  same as every other enforcement point in this ADR.
- **Ordering and lock class (§2.4, §5).** The KYC call for
  `casino_play`/`sportsbook_play` sits **after** RG and after Risk, inside
  the same domain transaction, immediately before the balance lock/ledger
  post — the exact position ADR 0031 §7 already documents RG/Risk
  occupying relative to the balance lock, so KYC is appended as a third,
  final read-only step rather than inserted between two already-tested
  steps. It is a plain `SELECT` (no lock acquired, §5), so it adds no new
  entry to ADR 0082's canonical lock-class ordering and, on the
  overwhelmingly common `not_required` (no active policy) path, costs one
  cheap indexed lookup against `kyc_enforcement_policies` per bet — a
  cost this ADR judges acceptable given the alternative (silently
  omitting a directed enforcement surface) is not available. If that
  per-bet lookup cost is ever found material at production bet volume,
  the caller may cache the *policy* (not the player's KYC state) for the
  duration of a `casino_launch_sessions` row, the same way
  `JurisdictionCode`/`LicensingMode` are already denormalized onto the
  session (ADR 0031 §9) — not designed here, flagged as a future
  optimization if profiling shows it is needed, never assumed.
- **HD-KYC-8 (§4).** Whether any jurisdiction actually requires KYC
  before play, and at what tier, is not decided here — recorded as its
  own human decision, tied to HDR-J-6 and legal review, independent of
  HD-KYC-1/2/3.

### 3.6 Migration 0101 — sketch

Two tables, both platform-wide (no `tenant_id`), mirroring
`jurisdiction_precedence_configs`'s RLS posture (readable everywhere,
writable only by a platform-scoped `platform_admin`/`compliance`
principal) plus one tenant-scoped decision-audit table.

```sql
-- 0101_kyc_enforcement_policy_and_decision_audit.up.sql

CREATE TABLE kyc_enforcement_policies (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    licensing_jurisdiction_id   UUID NOT NULL REFERENCES jurisdictions(id),
    trigger_type                TEXT NOT NULL
        CHECK (trigger_type IN ('cumulative_deposit', 'edd_amount', 'registration_tier', 'play')),
    -- 'play' (§3.5, HD-KYC-8): the ONLY trigger_type with no numeric or
    -- tier value at all — its mere presence as an 'active' row for a
    -- (licensing_jurisdiction_id, operation ∈ {casino_play,
    -- sportsbook_play}) pair means "this jurisdiction requires a passed
    -- verification before play, full stop." It still requires
    -- legal_review_reference like every other active row (the CHECK
    -- below), so it can never be authored casually.
    -- Deliberately NOT ('first_withdrawal') — that trigger is structural
    -- and compiled in (§3.2 point 1), never a configurable row, so it
    -- can never be silently disabled by an application-layer write.
    status                      TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'withdrawn')),
    threshold_minor_units       NUMERIC(38,0)
        CHECK (threshold_minor_units IS NULL OR threshold_minor_units > 0),
    asset_code                  TEXT REFERENCES assets(code),
    -- required_tier is for the registration_tier trigger only (e.g. a
    -- jurisdiction that requires KYC at signup, not deferred) — kept as
    -- its own nullable column rather than overloading threshold_minor_units,
    -- which has no meaning for a boolean/tier trigger (ADR 0031 §12's own
    -- "a new kind needs its own column, not a repurposed one" discipline).
    required_tier               TEXT CHECK (required_tier IS NULL OR required_tier IN ('basic', 'full')),
    -- play_operation is for the 'play' trigger only (§3.5, HD-KYC-8) — a
    -- KYC-before-play policy is authored per surface (casino vs.
    -- sportsbook), never both implicitly from one row, so a jurisdiction
    -- that only wants sportsbook gated does not accidentally also gate
    -- casino.
    play_operation               TEXT CHECK (play_operation IS NULL OR play_operation IN ('casino_play', 'sportsbook_play')),
    effective_from               TIMESTAMPTZ NOT NULL DEFAULT now(),
    legal_review_reference       TEXT CHECK (legal_review_reference IS NULL OR btrim(legal_review_reference) <> ''),
    reason_code                  TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_actor_type        TEXT NOT NULL,
    created_by_actor_id          UUID NOT NULL, -- security condition 2: NOT NULL from creation, and equal to the acting principal by the INSERT policy below
    -- Cross-trigger-type CHECK: a row must carry the value shape its own
    -- trigger_type needs and no other (mirrors ADR 0043's per-column
    -- CHECKs; prevents a 'registration_tier' row from silently also
    -- carrying an unused threshold_minor_units that a future reader might
    -- misinterpret).
    CHECK (
        (trigger_type IN ('cumulative_deposit','edd_amount') AND threshold_minor_units IS NOT NULL AND asset_code IS NOT NULL AND required_tier IS NULL AND play_operation IS NULL)
        OR (trigger_type = 'registration_tier' AND required_tier IS NOT NULL AND threshold_minor_units IS NULL AND asset_code IS NULL AND play_operation IS NULL)
        OR (trigger_type = 'play' AND play_operation IS NOT NULL AND threshold_minor_units IS NULL AND asset_code IS NULL AND required_tier IS NULL)
    ),
    -- 'active' rows require a legal review reference — mirrors ADR 0043's
    -- identical requirement for jurisdiction evaluation policy.
    CHECK (status <> 'active' OR legal_review_reference IS NOT NULL)
);

CREATE UNIQUE INDEX kyc_enforcement_policies_one_active
    ON kyc_enforcement_policies (licensing_jurisdiction_id, trigger_type, COALESCE(play_operation, ''))
    WHERE status = 'active';
    -- Only one active row per (jurisdiction, trigger_type, play_operation)
    -- at a time — the COALESCE lets casino_play and sportsbook_play each
    -- have their own independently active 'play' row for the same
    -- jurisdiction. ADR 0043's own "no ON CONFLICT DO UPDATE, append a new active row
    -- and withdraw the old one" discipline applies unchanged; a full
    -- effective-dated history is read via ORDER BY effective_from.

-- ======================================================================
-- REVISED per security condition 2 (§13): the original sketch's RLS/
-- trigger text was weaker than the migration 0075 precedent it claimed
-- to follow. This block now mirrors 0075's actual predicates verbatim
-- (NULLIF, the platform_admin_principal_id GUC, FORCE ROW LEVEL
-- SECURITY, explicit INSERT+UPDATE policies, no DELETE/FOR ALL policy).
-- One deliberate, disclosed difference from 0075: that table's lifecycle
-- is "insert a new effective-dated row, close the old one via
-- effective_to" and never mutates a status column in place. This table
-- has no effective-dated pairs — it is governed entirely by an in-place
-- `status` column — so its append-only trigger allows a narrow,
-- explicit set of in-place status transitions instead. This is a
-- different mechanism for a genuinely different lifecycle shape, not a
-- weaker copy of 0075's; the RLS predicates that actually gate WHO may
-- write are copied verbatim.
-- ======================================================================

CREATE FUNCTION kyc_enforcement_policies_enforce_lifecycle() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: DELETE is not permitted';
    END IF;
    -- Every column except `status` (and effective_from/created_at/
    -- created_by_* which are already forge-proofed by a BEFORE INSERT
    -- trigger mirroring 0075's own pattern, omitted here for brevity) is
    -- immutable after insert.
    IF (to_jsonb(NEW) - 'status') IS DISTINCT FROM (to_jsonb(OLD) - 'status') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: only status may change after insert';
    END IF;
    -- The only permitted in-place transitions (security condition 2):
    -- draft->active, draft->withdrawn, active->withdrawn. Every other
    -- pair, including any attempt to leave 'withdrawn' or to move
    -- backward, is rejected.
    IF NOT (
        (OLD.status = 'draft' AND NEW.status IN ('active', 'withdrawn'))
        OR (OLD.status = 'active' AND NEW.status = 'withdrawn')
    ) THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: illegal status transition % -> %', OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kyc_enforcement_policies_lifecycle
    BEFORE UPDATE ON kyc_enforcement_policies
    FOR EACH ROW EXECUTE FUNCTION kyc_enforcement_policies_enforce_lifecycle();

CREATE TRIGGER kyc_enforcement_policies_deny_delete_truncate
    BEFORE DELETE OR TRUNCATE ON kyc_enforcement_policies
    FOR EACH STATEMENT EXECUTE FUNCTION kyc_enforcement_policies_enforce_lifecycle();

ALTER TABLE kyc_enforcement_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_enforcement_policies FORCE ROW LEVEL SECURITY;
-- No tenant_id: platform-wide reference data, read by every tenant
-- sharing a licensing jurisdiction, exactly like
-- jurisdiction_precedence_configs. Write requires the platform-admin
-- principal GUC AND requires tenant/player context to be unset, verbatim
-- from migration 0075 lines 207-225 — a tenant-scoped connection
-- (including a tenant's own StaffRoleCompliance, which is a
-- tenant-bound role per staff_users.tenant_id) can never satisfy this,
-- regardless of what the HTTP handler layer checks. No DELETE policy.
-- No FOR ALL policy.
CREATE POLICY kyc_enforcement_policies_read ON kyc_enforcement_policies
    FOR SELECT USING (true);

CREATE POLICY kyc_enforcement_policies_platform_insert ON kyc_enforcement_policies
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        -- created_by_actor_id must equal the acting principal, so
        -- provenance cannot be forged by a caller that inserts a row and
        -- separately claims a different actor (security condition 2).
        AND created_by_actor_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
    );

CREATE POLICY kyc_enforcement_policies_platform_update ON kyc_enforcement_policies
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- created_by_actor_id is NOT NULL (moved here from the earlier sketch's
-- nullable column) precisely so the INSERT policy's provenance check
-- above is always evaluable.
ALTER TABLE kyc_enforcement_policies ALTER COLUMN created_by_actor_id SET NOT NULL;

-- Required tests (security condition 2): a tenant-scoped `compliance`
-- connection's INSERT/UPDATE is rejected at the database; a tenant-scoped
-- `platform_admin`-role-but-tenant-bound connection is rejected; a
-- platform-service-scoped connection with no platform_admin_principal_id
-- set is rejected; a genuine platform-admin-scoped connection succeeds.

-- Immutable, tenant-scoped audit of every enforcement decision — the
-- CLAUDE.md "audit every compliance-relevant action" requirement,
-- WITHOUT sensitive data: no document content, no raw provider reason,
-- no free text.
CREATE TABLE kyc_enforcement_decisions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    brand_id             UUID NOT NULL,
    player_account_id    UUID NOT NULL,
    operation            TEXT NOT NULL CHECK (operation IN ('deposit','withdrawal_hold','withdrawal_payout','casino_play','sportsbook_play')),
    outcome              TEXT NOT NULL CHECK (outcome IN ('not_required','passed','pending','failed','unavailable')),
    allowed              BOOLEAN NOT NULL,
    matched_trigger      TEXT, -- e.g. a policy row id, or a fixed label for the structural withdrawal rule (§3.2 point 1); NULL for not_required with nothing configured
    policy_version        TEXT NOT NULL,
    correlation_id        UUID NOT NULL,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)
);
CREATE INDEX kyc_enforcement_decisions_player ON kyc_enforcement_decisions (tenant_id, player_account_id, decided_at DESC);

ALTER TABLE kyc_enforcement_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_enforcement_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON kyc_enforcement_decisions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
-- Append-only trigger, identical convention to audit_log and every other
-- compliance-record table in this codebase (no UPDATE/DELETE ever), with
-- the same explicit BEFORE TRUNCATE guard security condition 2 requires.
CREATE FUNCTION kyc_enforcement_decisions_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'kyc_enforcement_decisions is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kyc_enforcement_decisions_immutable
    BEFORE UPDATE OR DELETE ON kyc_enforcement_decisions
    FOR EACH ROW EXECUTE FUNCTION kyc_enforcement_decisions_deny_mutation();

CREATE TRIGGER kyc_enforcement_decisions_deny_truncate
    BEFORE TRUNCATE ON kyc_enforcement_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION kyc_enforcement_decisions_deny_mutation();
```

`kyc_enforcement_decisions` is deliberately **not** the same table as
`audit_log` — it is a structured, queryable, tenant-scoped decision
record (staff/SAR-adjacent tooling reads it directly), whereas
`audit.Record` remains the generic cross-domain audit trail every
enforcement point ALSO writes to (one `audit.Record` call per decision,
`Action: "kyc.enforcement_denied"` / `"kyc.enforcement_allowed"`,
mirroring the existing `payments.deposit_denied_by_rg` pattern exactly).
Both are written in the same transaction as the decision itself, never
after the fact.

### 3.7 No test-only or production default values

No row is seeded by migration 0101 — the same discipline ADR 0043 §
(Stage 4I Phase D) already followed for its own PC-GAP-1/PC-GAP-2
content. Any numeric threshold appearing in a unit or integration test
(e.g. "cumulative deposit threshold = 100000 minor units" to exercise the
`not_required`→`failed` transition) MUST be clearly commented
`TEST FIXTURE VALUE — NOT A LEGAL THRESHOLD, see HDR-J-6` at the point of
use, mirroring how `docs/decisions/0075`'s own migration comment already
discloses its zero-seed-rows posture. `identity-compliance` implementing
PRH-I3 must not let a fixture value migrate into a code default by
accident — this is a review-gate item for `code-reviewer` and `security`
at implementation time.

---

## 4. Human decisions (recorded, not resolved here)

| ID | Decision needed | Tied to |
|---|---|---|
| **HD-KYC-1** | Cumulative-deposit threshold value(s), per jurisdiction/asset | HDR-J-6 (permitted markets) + legal review |
| **HD-KYC-2** | EDD amount threshold value(s), per jurisdiction/asset, and whether EDD also scores sportsbook/casino stake size (row #10) or only deposit/withdrawal amounts | HDR-J-6 + legal review |
| **HD-KYC-3** | Whether any jurisdiction requires KYC at registration (`registration_tier`), and at what tier | HDR-J-6 + legal review |
| **HD-KYC-4** | Whether bonus conversion (row #15) should be its own KYC/AML checkpoint independent of the eventual withdrawal gate | identity-compliance + legal, not decided by this ADR |
| **HD-KYC-5** | Whether a jurisdiction may ever relax the structural "`passed` required on every withdrawal" rule (§3.2 point 1, revised per security condition 1 / ledger-finance C3 from the original one-time "first withdrawal" exemption) — this ADR's default answer is no, recorded as a design choice, not a foreclosed option | Authority constraint, CLAUDE.md Compliance section |
| **HD-KYC-6** | Cross-tenant/cross-brand reuse of an approved verification (ADR 0028 §7's own still-open decision) — this ADR does not change that answer; `EvaluateEnforcement` reads `kyc_verifications` scoped exactly as narrowly as today (tenant/brand), so a decision to widen reuse is a change to `internal/kyc`'s read, not to this enforcement boundary | ADR 0028 §7 |
| **HD-KYC-7** | Whether wallet-to-wallet `ConversionOperation` (row #18) or a future affiliate payout (row #21) need their own gate, once built | Deferred, not designed here |
| **HD-KYC-8** | Does any jurisdiction require KYC before play (casino and/or sportsbook bet placement, rows #7/#10), and at what tier — the `play` trigger (§3.5) ships with no active row and no default value; this decides whether one is ever authored, and for which surface(s) | HDR-J-6 + legal review |

None of these are legal interpretation performed by this ADR — each is
named so the mechanism can receive the value the moment it exists,
without a redesign (the same "mechanism now, values later" contract the
registry's KYC-ENFORCE-1 row already states).

---

## 5. Ordering, tenant isolation, performance, concurrency

- **Ordering.** §2.4's table is the exact, per-path order; no existing
  RG/Risk call is reordered or removed. Withdrawal today has *no* RG
  check at all (row #3/#4) — this ADR does **not** add one; adding RG to
  withdrawal is a separate, pre-existing gap (observed, not created, by
  this reconnaissance) that belongs to whoever owns the withdrawal
  domain's own RG coverage, not to KYC-ENFORCE-1's scope. Recording it
  here so it is not mistaken for something this ADR silently declined to
  fix: **flagged for the orchestrator to register as its own item.**
- **Tenant isolation.** `kyc_enforcement_decisions` carries `tenant_id`
  and standard `tenant_isolation` RLS, identical to `kyc_verifications`
  (migration 0040). `kyc_enforcement_policies` is platform-wide, exactly
  like `jurisdiction_precedence_configs` — read by every tenant sharing a
  licensing jurisdiction, written only by a platform-scoped connection.
  `EvaluateEnforcement` itself takes `tx pgx.Tx` already scoped by the
  caller (`db.WithTenant`), exactly like `rg.EvaluateEligibility`/`risk.
  Evaluate` — it never opens its own connection or scope.
- **Performance / lock class.** `EvaluateEnforcement` issues plain
  `SELECT`s (`kyc_verifications` by `(tenant_id, brand_id,
  player_account_id)`, `kyc_enforcement_policies` by
  `(licensing_jurisdiction_id, trigger_type)` filtered to `status =
  'active'`, and — for the structural first-withdrawal rule — a `SELECT
  EXISTS(... WHERE state = 'completed')` against `withdrawal_requests`
  scoped to the wallet). **No row lock is taken by this function** — it
  reads the same way `rg.EvaluateEligibility`'s restriction lookup and
  `risk.Evaluate`'s rule lookup already do, so it introduces no new
  entry into ADR 0082's canonical lock-class ordering (L0–L4); it composes
  safely wherever RG/Risk already compose today, at the position §2.4
  states, before the caller's own ledger lock is taken.
- **No provider I/O.** `EvaluateEnforcement` never calls a `KYCProvider`
  — it reads the platform's own already-materialized verification state.
  This is the same separation ADR 0028 §4 already establishes (the
  provider updates `kyc_verifications` via its own callback/orchestrator
  path; enforcement only ever reads the result). It therefore carries
  none of F-POOL-2's outbound-I/O-inside-a-transaction risk and needs no
  ADR-0095-style restructuring.
- **Concurrency.** Because no lock is taken and the function is a pure
  read plus comparison, two concurrent calls for the same player can run
  concurrently with no serialization need beyond what the domain's own
  transaction already provides — a KYC status change (a staff approval,
  a provider callback landing) that commits between two concurrent
  deposit attempts is visible to whichever attempt's transaction starts
  after that commit, exactly like every other read-then-decide check in
  this codebase; no new race exists that RG/Risk do not already carry
  today for the identical pattern.

---

## 6. API / OpenAPI exposure

- **Staff read of decisions.** New route `GET /v1/admin/kyc/enforcement-
  decisions?player_account_id=...` under `StaffRoleCompliance` /
  `StaffRolePlatformAdmin` (mirrors the existing `PermVerificationReview`
  precedent), tenant-scoped, returns `kyc_enforcement_decisions` rows
  (outcome, operation, matched_trigger, policy_version, decided_at) —
  never the underlying `kyc_verifications.reason` (that stays governed by
  HD-10.3-3's existing bound/sanitized staff-only exposure, unchanged by
  this ADR).
- **Players see status only (HD-10.3-3, extended, not reopened).** No
  new player-facing field is added anywhere for *why* a deposit or
  withdrawal was denied beyond what already exists: a denied deposit
  already surfaces as `DepositIntentDeclined` with a bounded internal
  `reason` string (`"kyc_required:pending"` style, mirroring
  `"rg_ineligible:"+eligibility.Code}` at `orchestrator.go:558`) — never
  provider text, never a `kyc_enforcement_decisions` id, never a
  `matched_trigger` value. A denied withdrawal request/payout dispatch
  follows the identical convention: the player-facing response states
  the withdrawal was declined and the request's own state
  (`rejected`), never a compliance reason code beyond what
  `withdrawal_handlers.go` already exposes today for any other rejection.
- **OpenAPI.** `platform-api.yaml` gains: the new admin decisions route;
  no change to any player-facing schema (deposit/withdrawal response
  shapes already carry a generic decline reason field, reused, not
  widened).

---

## 7. Test plan

| Class | What it proves | Enforcement point(s) |
|---|---|---|
| Unit | Outcome mapping (§2.3) is exhaustive and exact; `not_required` vs `passed` vs `pending` vs `failed` vs `unavailable` for every combination of policy-row-present/absent × verification-status | `EvaluateEnforcement` in isolation |
| Unit | Structural first-withdrawal rule fires with zero policy rows present | #3, #5 |
| Integration | Deposit initiation: RG-denied player never reaches KYC evaluation (order preserved); KYC-denied player never reaches `attemptDeposit`/the provider | #1 |
| Integration | Withdrawal request: KYC-denied player's hold is never posted (transaction rolls back to before the ledger lock, mirroring `TestRequestWithdrawal_InsufficientFundsRejectedAndAtomic`'s atomicity proof) | #3 |
| Integration | Withdrawal payout dispatch: an approved-but-since-KYC-rejected request is denied at `LockApprovedForSubmission`, never reaching `MarkSubmitted`/the provider — the actual "before payout submission" backstop | #5 |
| Integration | **Revised per security condition 1 / ledger-finance C3** (replaces the original, defective "exempt after one prior completed withdrawal" row): a `Person` with one prior `completed` withdrawal whose *current* latest verification is `rejected`/`expired` is denied on a second withdrawal request and at payout dispatch — the structural rule never expires after a first pass, and a jurisdiction's `edd_amount` trigger (if active) still applies independently, additively, on top of it | #3, #5 |
| Integration | The structural rule is scoped per `Person`, not per wallet or `PlayerAccount`: a `rejected`/`expired` player cannot bypass it by opening a new wallet or a second `PlayerAccount` under the same `Person` | #3, #5 |
| RLS | A `db.WithPlayerScope` connection cannot read another tenant's `kyc_enforcement_decisions`; a tenant-scoped write attempt to `kyc_enforcement_policies` is rejected (platform-wide write only) | schema |
| Concurrency | Two concurrent deposit attempts racing a staff KYC approval that commits between them each see a consistent, individually-correct outcome (no torn read) — mirrors `TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds`'s pattern applied to a read-then-decide check | #1, #3, #5 |
| Negative/security | A client-supplied field cannot influence `Outcome` (fuzz `EnforcementParams` for any player-controllable path into the decision); cross-tenant `player_account_id` cannot be evaluated against a different tenant's `kyc_verifications` row | all |
| Negative/security | `unavailable` (a forced DB error / malformed policy row in a test harness) denies at every enforcement point, with no operation-specific bypass | all |
| Fail-closed | A migration 0101 table with an `active` policy row missing `legal_review_reference` is rejected by the CHECK constraint before it can ever govern a decision | schema |
| Audit | Every `EvaluateEnforcement` call at every enforcement point writes exactly one `kyc_enforcement_decisions` row and one `audit.Record` call, in the same transaction as the domain effect it gated, with no PII/document content in either | all |
| SAR-adjacent | `kyc_enforcement_decisions` plus `kyc_verifications`/`audit_log` together give a compliance reviewer a complete, joinable trail of "what was decided, when, against what policy version" for any player — proves the audit design is queryable, not just present | staff tooling |
| Integration | Casino/sportsbook bet placement with **no active `play` policy**: KYC evaluates `not_required` and never denies, RG/Risk denials still short-circuit before KYC runs, existing RG/Risk regression suites pass unmodified | #7, #10 |
| Integration | Casino/sportsbook bet placement with an **active `play` policy** (test-fixture row, clearly marked per §3.7): `passed` allows, `pending`/`failed`/`unavailable` all deny with no bypass, and a `casino_play` policy never governs `sportsbook_play` or vice versa (the `play_operation` column is enforced, not advisory) | #7, #10 |
| Concurrency/lock | Placing a bet takes no additional row lock from the new KYC step — proved by running the existing `internal/casino`/`internal/sportsbook` lock-order harnesses (ADR 0082) unmodified against the amended orchestrator and confirming no new lock-class entry appears | #7, #10 |
| Performance | The `not_required` (no active policy) path costs one indexed lookup per bet and no measurable regression against the existing `postBet`/sportsbook bet-placement latency baseline | #7, #10 |

---

## 8. Implementation breakdown and dependency requests

Ownership per PRH-I3 (`identity-compliance` + `payments`, `casino`,
withdrawal owners via dependency requests):

1. **`internal/kyc`** (identity-compliance): `EvaluateEnforcement`,
   `EnforcementParams`/`Decision`/`Outcome` types, the structural
   first-withdrawal query, the threshold-policy lookup, migration 0101
   Go-side repository (mirrors `jurisdiction/evaluation_policy_admin.go`'s
   shape: `CreateEnforcementPolicy`/`ListEnforcementPolicyVersions`/
   `WithdrawEnforcementPolicy`).
2. **`internal/payments`** (payments, reviewed by identity-compliance):
   one new call to `kyc.EvaluateEnforcement` in `InitiateDeposit`,
   immediately after the existing RG check, mirroring the RG denial's
   own `finalizeDeclined` pattern exactly (`"kyc_required:"+decision.Code`).
   **Dependency request:** `payments` confirms the exact `DepositIntent`
   declined-reason string convention and reviews the placement.
3. **`internal/withdrawal`** (withdrawal owner — currently no dedicated
   package owner distinct from `identity-compliance`'s own review scope;
   dependency request to whichever specialist next touches this package,
   or `identity-compliance` implements directly under `ledger-finance`
   review given the ledger-adjacent hold/reversal mechanics): a KYC call
   in `RequestWithdrawal` before the ledger hold is posted (reusing the
   `ErrInsufficientFunds`-adjacent early-return shape, not a new state);
   and a KYC call in `LockApprovedForSubmission` before the row is
   returned to the caller for provider dispatch, denying by transitioning
   the request the same way `Reject` does (hold reversal), with a system
   actor and a `kyc_denied` reason code. **Dependency request:**
   `ledger-finance` review for the hold-reversal posting shape at this
   new denial point (touches money movement, per this specialist's
   Review responsibility to request `ledger-finance` review for anything
   gating a withdrawal financially); `security` review for the
   token/session and RLS handling of the new admin decisions route (per
   this specialist's Review responsibility for token/session review).
4. **`internal/casino`, `internal/sportsbook`** (casino, sportsbook;
   reviewed by identity-compliance): one new call each to `kyc.
   EvaluateEnforcement` (`EnforcementCasinoPlay`/`EnforcementSportsbookPlay`)
   appended after the existing RG→Risk sequence in `postBet`
   (`internal/casino/orchestrator.go:952`) and the sportsbook bet-placement
   orchestrator (`internal/sportsbook/orchestrator.go:583-616`), immediately
   before the balance lock, per §2.4/§3.5 — never reordering the existing
   RG/Risk calls. On the default (`not_required`) path this is a no-op
   read; a deny follows the same short-circuit-before-ledger-effect shape
   RG/Risk denials already use at that exact call site. **Dependency
   request:** `casino` and `sportsbook` each confirm the exact denial
   surfacing convention at their own call site (mirroring their existing
   RG/Risk denial audit shape) and review the lock-order/performance claim
   in §5/§7 empirically (running their own ADR 0082 lock-order harness
   against the amended orchestrator) before this is marked implemented.
   No change needed unless HD-KYC-8 is answered "yes" for casino wins/
   rollbacks or sportsbook settlement/void — those stay NOT-ENFORCE
   per §1 rows #8/#9/#11 regardless (corrections, not new stakes).
5. **`internal/bonus`**: **no change** — §1's verdicts remain NOT-ENFORCE
   for every bonus enforcement point; no dependency request needed unless
   a future human decision (HD-KYC-4) reopens bonus conversion.
6. **Migration 0101**: `identity-compliance` authors it; `ledger-finance`
   reviews the `NUMERIC(38,0)`/asset-registry handling on
   `threshold_minor_units` (CLAUDE.md's own money-representation rule);
   `security` reviews RLS and the append-only trigger.
7. **OpenAPI / staff route**: `identity-compliance` + `code-reviewer`.
8. **QA**: owns the test plan in §7 as an execution gate, per its
   existing testing-strategy authority.

Every row above is `PROPOSED`/`NOT IMPLEMENTED` until PRH-I3 is
authorized and executed; nothing in this document is claimed as built.

---

## 9. Labels

Per CLAUDE.md's seven-value vocabulary: this document is a design paper.
The mechanism it specifies is **NOT IMPLEMENTED**. No threshold value is
asserted. No vendor is selected. No regulatory approval is claimed.

---

## 10. QA test-plan review (`qa`, §7 only)

**Verdict: CONFIRMED WITH CHANGES.**

Checked against the PRH testing checklist (unit, integration, PostgreSQL-
backed, race, concurrency, negative/security, tenant isolation, RLS,
migration up/down, API/OpenAPI contract, idempotency, failure injection)
and the KYC-specific requirements (per-enforcement-point outcome coverage,
fail-closed on `unavailable`, result not player-suppliable, RG/Risk
ordering, PII-free audit, tenant isolation, no invented thresholds).

**What §7 gets right.** Unit outcome-mapping exhaustiveness; the
structural first-withdrawal rule; RG-before-KYC ordering at deposit and at
`play`; the withdrawal-hold and payout-dispatch backstops named against
their real call sites (`LockApprovedForSubmission`, mirroring
`TestRequestWithdrawal_InsufficientFundsRejectedAndAtomic`'s atomicity
proof — a real regression pattern, not an invented one); RLS on
`kyc_enforcement_decisions` plus platform-only write on
`kyc_enforcement_policies`; the concurrent-approval-race row modeled on
`TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds`; client-supplied-
field fuzzing and cross-tenant `player_account_id` rejection; `unavailable`
fail-closed "at every enforcement point, with no operation-specific
bypass" (this one row does legitimately cover fail-closed across all five
points, so that specific KYC requirement is met); the migration 0101 CHECK
fail-closed test; audit-row-plus-`audit.Record`-together with no PII;
casino/sportsbook `play` coverage explicitly enumerating `passed`/
`pending`/`failed`/`unavailable`, the `play_operation` column enforced not
advisory, and the test-fixture policy row "clearly marked per §3.7" — this
satisfies "no invented thresholds" for the one place §7 exercises a
concrete active policy.

**Gaps requiring changes before this is a complete execution gate:**

1. **Per-outcome coverage at deposit/withdrawal-hold/payout-dispatch is
   under-specified.** The `play` rows explicitly enumerate `passed`/
   `pending`/`failed`/`unavailable`; the deposit, withdrawal-hold, and
   payout-dispatch integration rows only say "KYC-denied," not which of
   `required`/`pending`/`failed` is being exercised (with `unavailable`
   covered separately by the cross-cutting negative row). Add explicit
   sub-cases so each of the five outcomes is proven, by name, at each of
   the five enforcement points — not asserted once generically and assumed
   to generalize.

2. **No migration up/down (reversibility) test for migration 0101.** §7
   has a CHECK-constraint fail-closed test but no down-migration test on a
   fresh database, which is this codebase's own established rule
   (`docs/testing/testing-strategy.md`'s Stage 10 W1 note: "the migration-
   reversibility CI step runs on a fresh database"). Add one for 0101
   (both `kyc_enforcement_decisions` and `kyc_enforcement_policies`).

3. **No OpenAPI contract test for the new admin route.** §6 adds
   `GET /v1/admin/kyc/enforcement-decisions` to `platform-api.yaml`; §7 has
   no corresponding contract test, despite this codebase's own precedent
   (`internal/httpserver/openapi_paymentswebhook_contract_test.go`) for
   exactly this pattern. Add one, and record up front whether it will be
   the same "plain-text/substring" structural check used for the payments
   webhook (no OpenAPI/JSON-Schema library is a verified dependency today)
   rather than silently discovering that limitation later.

4. **No idempotency test for the `kyc_enforcement_decisions` write itself.**
   The audit row proves "exactly one row per call" within a single call,
   but not what happens under a retried request (e.g., a deposit-intent
   retry after a timeout) that re-enters `EvaluateEnforcement` for the same
   logical operation — whether that is expected to write a second decision
   row (append-only, acceptable) or must dedupe, is unstated. CLAUDE.md's
   financial-write idempotency rule applies to every financial write on
   these paths; state the expected behavior and test it explicitly.

5. **No mutation-kill requirement for the fail-closed/outcome-mapping
   guard.** This codebase's own added rule (Stage 10.1: "a mutation that
   removes the guarded predicate must turn at least one test red") applies
   directly to `EvaluateEnforcement`'s `unavailable`-fail-closed branch and
   its five-way outcome switch — the single highest-value guard this ADR
   introduces. §7 names no mutation pass or manual branch-coverage
   substitute (the SQL CHECK constraint is exactly the kind of construct
   this project's own precedent already treats as "no mutation tool
   applies → manual branch-coverage checklist"). Add both: a mutation pass
   over `EvaluateEnforcement`'s Go branches, and a manual true/false
   checklist for the 0101 CHECK constraint.

6. **No stated CI time budget for the new integration/concurrency suite.**
   §7 does not say which package(s) the integration/concurrency rows land
   in, nor estimate their runtime. Given `internal/httpserver`'s existing
   integration-lane runtime is already substantial against this project's
   CI ceiling, and several of §7's new rows (deposit, withdrawal-hold,
   payout-dispatch, casino/sportsbook `play` × 5 outcomes once item 1 above
   is addressed, plus a new concurrency race test) are very likely to land
   there, add an explicit runtime estimate and, if it materially narrows
   the CI margin, name which existing `TIMING_LANE_TESTS`-style lane
   absorbs them (`.github/workflows/ci.yml`'s existing split) rather than
   letting the new tests default into the package's slowest lane
   unexamined.

7. **"Performance" row has no measurable pass criterion.** "No measurable
   regression against the existing baseline" is not a number. State a
   concrete threshold (e.g., delta vs. baseline p95 bounded to X ms/percent)
   or replace it with a benchmark-comparison method that has an objective
   pass/fail, consistent with this project's "measurable pass criteria"
   norm elsewhere.

None of the above blocks the design itself — §7's *shape* is sound and its
integration rows are anchored to real, existing regression patterns rather
than invented ones. But as an execution gate, items 1–7 must be closed
(or explicitly descoped with a recorded reason) before `qa` will sign off
test coverage for this ADR's implementation as `IMPLEMENTED`.

---

## 11. Casino review (`casino`, 2026-09-27)

Reviewed against live code at `HEAD 3d50b3c` (`internal/casino/
orchestrator.go`, `internal/sportsbook/orchestrator.go`), scoped strictly
to the casino "play" enforcement point (§2.4, §3.5, row #7) plus a
sportsbook symmetry sanity check (row #10). Withdrawal/deposit/bonus rows
are outside this specialist's authority and not re-reviewed here.

**Call site and ordering — verified correct.** `postBet`'s current RG
call (`evaluateAndAuditEligibility`) and Risk call (`evaluateAndAuditRisk`)
sit at what is now roughly lines 1184 and 1225-1247 (the ADR's `:952`
citation is stale — see conditions below — but the *relative* order this
ADR relies on is exactly as described: RG → Risk, both after the
provider-tx delivery lock and the idempotency/tombstone short-circuits,
both before `ledger.GetOrCreateAccounts`/the balance check at ~line 1253).
Appending a third `kyc.EvaluateEnforcement` call immediately after the
Risk block (§2.4) and before line ~1249 lands it exactly where the ADR
claims: after RG/Risk, before the balance lock, with an RG or Risk denial
still short-circuiting before KYC ever runs.

**No lock taken — verified.** `EvaluateEnforcement` is specified as plain
`SELECT`s only (§5). Confirmed the intended insertion point in `postBet`
precedes `GetOrCreateAccounts`/the pre-lock balance check entirely, so the
claim "adds no new entry to ADR 0082's lock-class ordering" holds as
designed. Sportsbook's mirrored helpers (`evaluateAndAuditEligibility`/
`evaluateAndAuditRisk` in `internal/sportsbook/orchestrator.go`, duplicated
rather than imported per that file's own documented rationale) sit at the
same relative position; the same reasoning applies there.

**One extra SELECT per bet — acceptable.** Given no lock is taken and the
dormant (`not_required`) path is a single indexed lookup, this is an
acceptable addition to the hot bet path. Agreed this is not worth caching
`casino_launch_sessions`-side unless profiling later shows otherwise, per
§3.5's own "flagged as future optimization, not designed here" stance —
do not build the cache preemptively.

**Wins/rollbacks unaffected — verified.** `postWin`/`postRollbackTombstone`
(and their non-tombstone counterpart) carry no RG/Risk call today by
design (their own doc comments, cited accurately in §1 rows #8/#9), and
this ADR adds no KYC call to either. Confirmed no call is being proposed
there. Correct.

**Provider-visible error class on a mid-session KYC-required decline —
needs one clarification, not a design change.** `ReceiveCallbackResult`
already carries exactly one decline shape used uniformly today:
`Outcome: OutcomeDeclined, DeclineReason: <code>` (verified at the RG
denial, the Risk denial, the insufficient-funds denial, and the
tombstoned-original denial — four existing call sites, one shared enum
value, only `DeclineReason` varies). A KYC-required decline
(`decision.Code`, e.g. `"kyc_required:pending"` per §6's own stated
convention) is a fifth instance of the *same* `OutcomeDeclined` class, not
a new provider-visible error class — provider adapters that already branch
on "declined vs succeeded vs replayed" need zero new handling, only a new
string value they were already treating opaquely. §2.4/§3.5 imply this but
never say it in so many words for the casino call site the way §6 does for
deposit/withdrawal; recommend §3.5 or the implementation breakdown (§8
item 4) state this explicitly so `casino`'s implementer doesn't
independently (re)invent a new outcome variant.

**Retry semantics for a KYC-declined bet — needs explicit test-plan
coverage.** Verified in `postBet`: the idempotency short-circuit
(`findPostedBetTransaction`) only fires for an *already-posted* (i.e.
already-succeeded) bet — a decline posts nothing, so a provider redelivery
of the same `provider_tx_id` after a KYC decline is **not** a no-op; it
re-enters `postBet` and is freshly re-evaluated against RG → Risk → KYC's
then-current state, exactly like an RG- or Risk-declined bet is today
(same code path, same precedent, nothing new). This is the correct,
symmetric behavior and requires no design change, but §7's test plan
should add one row making it explicit for KYC specifically (a
KYC-declined bet's provider retry is freshly re-evaluated, not replayed;
if the underlying policy/verification state has since changed — e.g. the
player's verification lands as `approved` between the two attempts — the
retry may succeed where the first attempt didn't, which is intended, not
a bug) so this isn't left to be inferred from RG/Risk's existing tests by
analogy alone.

**Citation staleness (cosmetic).** §1's `orchestrator.go:952/1456/1586`
line citations and the stated baseline (`1560ad0`) no longer match current
`HEAD` line numbers (RG/Risk/tombstone logic now sits in the 1000-1900
range). Not a substantive problem — the described call sites and behavior
are still correctly identified by function name and relative order — but
should be refreshed at implementation time so `code-reviewer` isn't
diffing against stale line numbers.

**Sportsbook symmetry — consistent.** `internal/sportsbook/orchestrator.go`
duplicates the identical RG-then-Risk shape and denial/audit convention
casino uses, at the equivalent position ahead of settlement/ledger work.
No divergence found that would make the ADR's "identical placement and
rationale" claim (§2.4) inaccurate for sportsbook.

**Verdict: APPROVE WITH CONDITIONS**
1. §3.5/§8 item 4 explicitly state the KYC-required decline reuses the
   existing `OutcomeDeclined`/`DeclineReason` class at the casino/
   sportsbook bet call site (no new outcome variant).
2. §7 gains an explicit test row for KYC-decline retry semantics
   (redelivery is freshly re-evaluated, not replayed/no-op), mirroring
   the existing RG/Risk decline-retry behavior already implicit in the
   code.
3. Refresh the stale `orchestrator.go` line-number citations and baseline
   commit in §1 before/at PRH-I3 implementation.

None of these require a design change to §2/§3.5's mechanism; all are
either documentation precision or a missing test-plan line item.

---

## 12. Ledger-finance review

**Reviewer:** `ledger-finance`. **Verdict: SIGN-OFF WITH CONDITIONS.**
**Scope:** the financial parts only: §2.4/§3.2/§5 withdrawal enforcement
points #3/#5, the hold-reversal posting at a payout-time denial, deposit
gate placement (#1), play gate placement relative to the balance lock and
ADR 0082 (#7/#10), and TOCTOU. Checked against code at `07c8103`:
`internal/withdrawal/withdrawal.go` (`RequestWithdrawal` :291,
`Reject` :731, `LockApprovedForSubmission` :863, `Fail` :1150),
`internal/httpserver/withdrawal_handlers.go` (request :106-120, submit
:801-880), `internal/payments/orchestrator.go` `InitiateDeposit`
:474-570, migrations 0021/0026/0034/0092, ADR 0082 §2.1. ADR 0095 is
not yet written (registry PRH-D1 "Not started"). C5 below states what it
must preserve and does not guess its design.

### 12.1 What is sound

- **Deposit (#1).** The gate goes after RG, inside the transaction that
  inserted the intent, and before `RouteProvider`/`provider.Deposit`. A
  denial creates only a `declined` `deposit_intents` row. It writes no
  ledger posting and makes no provider call. An idempotent replay returns
  the declined intent and does not re-evaluate, which is correct. Not
  gating the callback (#2) is also correct. Funds a PSP has already sent
  must be credited, or they strand in `psp_clearing` with no owner. Using
  "allow when unconfigured" is financially safe only because C3's
  withdrawal backstop holds.
- **Play (#7/#10).** A plain `SELECT` after RG→Risk and before
  `LockProjectionsForPosting` (L3) takes no row or advisory lock, so ADR
  0082's L0–L4 order is unchanged. It is also placed after L0.1, and
  after L0.4/L0.5 where RG/Risk take them. Not gating win, rollback,
  settlement or void (#8/#9/#11) is **required, not just acceptable**.
  If a correction could be denied, liabilities would strand and tombstone
  semantics would break.
- **No gate-less posting on the hold path.** Once C1 is applied, no
  `withdrawal_requested` posting can happen without an evaluated,
  in-transaction gate. Replaying an existing idempotency key returns the
  original request and posts nothing, so it correctly skips the gate.
- **Money representation.** `threshold_minor_units NUMERIC(38,0)` plus
  `asset_code REFERENCES assets(code)` follows CLAUDE.md. See C6 for how
  it is compared.

### 12.2 Conditions (all must hold before PRH-I3 is marked IMPLEMENTED)

**C1 — A denial must commit, never roll back (blocking design defect).**
Test-plan row "KYC-denied player's hold is never posted (transaction
rolls back…)" contradicts §3.6: the `kyc_enforcement_decisions` row and
`audit.Record` are "written in the same transaction". Both
`withdrawal_handlers.go` closures roll back on any non-nil error.
`InitiateDeposit`'s own comment (:540-551) records this bug class
already. Required shape:
- **`RequestWithdrawal`:** do a read-only lookup of an existing request
  by `(tenant, player, idempotency_key)` first and return a replay if one
  exists. Then evaluate KYC. **On deny:** write the decision row and audit,
  with no `withdrawal_requests` insert and no posting, and return a typed
  result (for example `ErrKYCRequired`) that the handler **commits**
  before it maps the result to the response. **On allow:** continue with
  the existing `IdempotentInsert` → accounts → L3 → `Post`. Do not insert
  the request row and then deny. That leaves a `requested` row without a
  hold, which breaks withdrawal-state-machine.md §4 unless it is undone
  through a savepoint.
- **`LockApprovedForSubmission`:** a deny must return a result the caller
  **commits** and that can never flow into `RouteProvider`/`Withdraw`. A
  `nil` error with the row already transitioned would reach the provider
  call. A non-nil error would roll back the reversal. Use a distinct
  return, for example `(wr, ErrKYCDeniedCommitted)`, with the handler
  changed to commit on it. Add a test that proves the reversal, the state
  change, the decision row and the audit all survive the handler's
  transaction.

**C2 — Payout-time denial posting shape.** The ADR's "the same way
`Reject` does" must not mean *calling* `Reject`. `Reject` requires
`pending_review`, a staff principal, `ApproverEligibility` and a
`withdrawal_approvals` insert. The governance trigger (migration 0034)
refuses a non-staff, non-automated principal. Required:
- A new function, for example `withdrawal.DenyForCompliance(ctx, tx,
  requestID, correlationID)`. It runs inside the same L1
  `lockRequestForUpdate` that `LockApprovedForSubmission` holds, and adds
  a new edge `approved → rejected` (withdrawal-state-machine.md must be
  amended to record it).
- The posting mirrors `Reject`/`Fail` exactly. Accounts are resolved
  `(hold, cash)` through `GetOrCreateAccounts`. The entries debit
  `player_withdrawal_hold` and credit `player_cash` for `wr.Amount`, with
  `TransactionType = withdrawal_rejected`, `ReversesTransactionID =
  wr.HoldLedgerTransactionID` and `CorrelationID = requestID`.
  `ledger.Post` takes its own L3 pre-lock, and L1 already comes before
  L3, so the lock order is compliant with no new lock.
- **Idempotency key `requestID + ":kyc_denied"`**, kept separate from
  `:rejected`/`:failed`. Exactly-once release still depends on L1 plus
  the conditional `UPDATE … WHERE state = 'approved'`, which is how every
  existing release path works. Set `release_ledger_transaction_id` in the
  same `UPDATE`. Treat `RowsAffected()==0` as `ErrStateConflict`, which
  rolls back the posting.
- Put no `reason_code` on the ledger transaction. The migration 0021
  CHECK allows one only for `manual_adjustment`. `kyc_denied` belongs in
  the audit entry (`ActorSystem`, action `withdrawal.rejected_kyc`,
  metadata: outcome, `policy_version`, `hold_ledger_transaction`,
  `release_ledger_transaction`). Write no `withdrawal_approvals` row,
  because this is not a four-eyes decision.
- Releasing to `player_cash` is the correct financial default: no held
  value is left stranded without an owner. Whether funds should instead
  stay in a compliance hold (an AML freeze) is an `identity-compliance`
  and legal decision. It is **not** decided here and must be recorded as
  its own HD if anyone wants it. It would need a new state, not a
  repurposed one.
- **Recommended, not blocking:** as a DB backstop for exactly-once
  release, add a partial unique index `(tenant_id,
  reverses_transaction_id) WHERE transaction_type IN
  ('withdrawal_rejected','withdrawal_failed')`, mirroring migration 0092.
  This is a ledger-finance-owned follow-up. Register it; it is not part of
  migration 0101.

**C3 — The structural first-withdrawal exemption must not let a known
`failed` status pay out.** In §3.2 point 1, any wallet with a prior
`completed` withdrawal is exempt. As written, a player whose latest
verification is now `rejected`/`expired` (for example after a fraud
finding) could withdraw a second time, ungated, whenever no threshold row
is active. That is value leaving the platform against a known negative
KYC state. Required: for `withdrawal_hold`/`withdrawal_payout`, a latest
verification in `rejected`/`expired` status returns `failed` whatever the
withdrawal history. The exemption covers only "never required since the
last pass". If `identity-compliance` disagrees, it must be recorded as an
explicit HD, not left implied. Also note the EXISTS is scoped
**per wallet**. That is stricter, since each new asset wallet needs
another first-withdrawal pass, and so acceptable, but it should be stated.

**C4 — TOCTOU: accept it, but bound it explicitly.** Under READ
COMMITTED, the gate reads `kyc_verifications` without a lock. A
revocation that commits after that read but before the gated transaction
commits is not seen. **Acceptable**, on three conditions:
- (a) The gate result is always computed inside the transaction that
  performs the gated transition or posting, and is never passed between
  transactions. `EvaluateEnforcement` accepts no precomputed decision, as
  §2.5 already requires.
- (b) Every re-entry re-evaluates: retry, sweeper, resubmission after
  rollback.
- (c) The window is documented as ≤ one transaction's duration.

Closing the window completely (`FOR SHARE` on the verification row) would
add a new lock class to ADR 0082 and a contention point between KYC
callbacks and payouts. That is not warranted here. A revocation that
lands inside the window is handled like any post-dispatch compliance
finding (C5).

**C5 — ADR 0095 interplay (a binding constraint on PRH-D1).** Today the
provider call runs inside the lock transaction. ADR 0095 will split this
into intent/claim → provider I/O → result. The KYC payout gate must sit
in **the last transaction that commits before the first outbound
`Withdraw` call for that intent**. In practice that is the transaction
that moves `approved` to the first dispatch state. If ADR 0095 adds a
"prepared, not yet sent" state that a sweeper sends later, the sweeper's
claim transaction must re-run the gate before that first send. After a
request has possibly reached the provider (`submitted`, or any
ambiguous/in-flight intent state):
- no KYC outcome may trigger `Fail`, a hold reversal, or any other
  automated release;
- only provider evidence (`QueryStatus` → `Complete`/`Fail`) may resolve
  it.

Reversing the hold on KYC grounds while the PSP might still pay would
create a real double-spend: the player's cash is restored and the payout
also completes. A KYC revocation after dispatch goes to a compliance
case, not a ledger action. The denial edge is legal **only from
`approved`** (and from `requested`/`pending_review`, if later chosen).
ADR 0095's state machine must keep that edge and must list
`DenyForCompliance` among its pre-dispatch terminal transitions.

**C6 — Threshold arithmetic.** Compare `threshold_minor_units` against
amounts in exact integers: SQL `NUMERIC` or Go `big.Int`/`int64` with an
overflow check, never `float64`. Compare only within the same
`asset_code`. Aggregating cumulative deposits across assets would need an
FX/`ConversionOperation` basis, which is a human decision (extend
HD-KYC-1), not an implicit sum. The cumulative-deposit figure must come
from ledger postings (settled `deposit_completed` credits to the player's
wallets for that asset), not from `deposit_intents.amount`, which
includes declined and pending intents. Whether reversals are netted off
is also part of HD-KYC-1.

**C7 — Tests (ledger-finance's suite, added to §7).** The following
tests are required:
- A deny at request posts nothing, and the decision row and audit are
  committed.
- A deny at payout commits exactly one `withdrawal_rejected` reversal,
  and `SUM(DEBITS)==SUM(CREDITS)` still holds.
- The projection matches the recomputed-from-ledger balance, with zero
  drift from the reconciliation job.
- A concurrent `Reject`/`Cancel` racing `DenyForCompliance` on one
  request produces exactly one release.
- A replayed submit after a payout deny gets `ErrStateConflict` and never
  reaches the provider.
- A replayed request idempotency key after a successful hold does not
  re-gate.
- A KYC revocation committed after dispatch causes no automated reversal
  (C5).
- A `rejected`/`expired` player with a prior completed withdrawal is
  denied (C3).
- The ADR 0082 lock-order harness passes unmodified on all five gated
  paths.

### 12.3 Veto check

No floating point, no mutation of historical ledger entries, no direct
balance `UPDATE`, and no money path without an idempotency key. No veto
applies. C1 is a correctness defect in the proposed design and **must**
be fixed in the ADR text before PRH-I3 starts. C2–C7 are implementation
gates.

---

## 13. Security review

Reviewer: `security`, 2026-09-27. Reviewed at the working tree's actual
`HEAD 3d50b3c` (the review request cited `07c8103`; this ADR's own baseline
line says `1560ad0`; §1's file:line citations were not all re-verified).
**Scope:** this design paper only. Checked against migrations 0040
(`kyc_verifications`) and 0075 (`jurisdiction_precedence_configs`, the
precedent it says it follows), `internal/db/platform_service.go`,
`internal/identity/staff_user.go`, `internal/casino/orchestrator.go`
(denial-audit pattern), and the withdrawal submit path
(`internal/httpserver/withdrawal_handlers.go:827-850`). **Out of scope:** no
code exists to review yet. There was no penetration testing and no legal
review. The HD-KYC-* values are not a security call. PRH-I3's implementation
needs its own review (§8 already lists it); this approval does not carry
over to it.

### Verdict: **APPROVE WITH CONDITIONS**

What holds up well: the enforcement mechanism is its own boundary,
separate from RG/Risk. It never calls a vendor. It performs its own read
instead of accepting a pre-computed outcome. The five-valued outcome has
no policy-configurable path from `pending`/`failed`/`unavailable` to
allow. The first-withdrawal rule is compiled in, and `first_withdrawal`
is deliberately not a `trigger_type`, so no policy row can switch it
off. The decisions table stores no sensitive content, and players see
status only. The conditions below fix places where the sketch is
**weaker than the precedent it cites** or where the stated backstop does
not hold as written.

### Ruling on the flagged design choice: deposits ALLOWED when no threshold policy is configured

**Accepted, but only together with C1 and C6.** The justification
"value cannot leave through a deposit, and the withdrawal gate is the
backstop" holds only if the backstop runs on *every* way value can
leave. As written, §3.2 point 1 gates only the *first* withdrawal (see
C1), so the backstop has a hole and the ruling depends on closing it.
Also:
- (a) This covers today's paths only. No refund-to-source execution
  path exists in `internal/payments` today (only type references).
  Crypto (row #19) is different: funds can arrive with no initiation
  step to gate. Either of those, once built, must be re-reviewed
  against this ruling and cannot inherit it.
- (b) **Launch flag for the orchestrator/human:** "dormant" describes
  how the mechanism behaves. It is not a compliance position. Going
  live with real money in any jurisdiction that has no active
  `cumulative_deposit` policy means unverified players can deposit
  without limit, and funds can be *placed* (the first AML stage) even
  if they can never be extracted. Whether that is acceptable for
  Anjouan or any later market is HD-KYC-1 plus legal review. It must be
  signed off explicitly before launch and must not be inherited by
  default.
- (c) Dormancy must be visible to operations. See C9.

### Conditions (each must be met before PRH-I3 is marked complete; C1, C2, C4 and C5 also block launch)

1. **[HIGH — launch-blocking] Every withdrawal must require `passed`,
   not only the first.** As written, the rule fires only for a wallet
   with zero prior `completed` withdrawals. Failure scenario: a player
   is approved, completes one withdrawal, and is later `rejected`
   (forged document found) or `expired`. With no `edd_amount` row
   active, the second withdrawal evaluates `not_required` and is paid
   out. The same hole lets every player who completed a withdrawal
   before this gate shipped withdraw without ever being verified. The
   rule must read: "a withdrawal request or payout dispatch requires
   the player's current verification to be `passed`." The first
   withdrawal is the point where KYC becomes mandatory; it does not
   stop being mandatory afterwards. Replace §7's test "a player with
   one prior completed withdrawal is exempt" with the opposite
   assertion. Also scope the rule per player, not per wallet (ADR
   0007), so opening a new wallet cannot reset it.
2. **[HIGH — launch-blocking] Migration 0101 RLS and immutability must
   copy migration 0075 exactly. The sketch does not.** The sketch's
   write policy is
   `current_setting('app.tenant_id', true) IS NULL`. That check (i) has
   no `NULLIF`, (ii) does not require
   `app.platform_admin_principal_id`, and (iii) comes with no
   `FORCE ROW LEVEL SECURITY` and no UPDATE policy. Failure scenario:
   any platform-scoped connection that leaves the tenant unset (for
   example `WithPlatformService` jobs) can `INSERT` an `active` policy,
   or a relaxed one, for any jurisdiction. Required:
   - `FORCE` RLS.
   - INSERT and UPDATE policies that require
     `NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL`
     and `NULLIF` of `app.tenant_id` and `app.player_account_id` to be
     `NULL`, verbatim from 0075 lines 207-225.
   - No DELETE policy and no FOR ALL policy.
   - A table-specific append-only trigger plus a `BEFORE TRUNCATE`
     guard. The sketch's `enforce_append_only_status_transition` and
     `reject_mutation` do not exist in `migrations/`. The trigger must
     permit exactly `draft→active`, `draft→withdrawn` and
     `active→withdrawn`, with every other column immutable. The sketch
     permits only `→withdrawn`, which makes `draft→active`
     impossible.
   - `created_by_actor_id NOT NULL`, checked to equal the principal
     GUC, so provenance cannot be forged.
   - The same `FORCE`, `NULLIF` and TRUNCATE-guard treatment for
     `kyc_enforcement_decisions`.

   **Can a tenant admin change enforcement?** Only if this condition is
   ignored. `StaffRoleCompliance` is a *tenant-bound* staff role
   (`staff_users.tenant_id`). The sketch's "compliance/platform_admin
   role check at the handler" would let tenant A's compliance officer
   write platform-wide policy that governs every tenant sharing the
   licensing jurisdiction, including tenant B. Writes must be limited to
   the platform-admin principal, enforced by the database. A tenant
   role must never grant write access. Required tests: tenant-scoped
   compliance and tenant admin get a DB-level rejection on
   INSERT/UPDATE; the platform-service scope is rejected; the
   platform-admin scope succeeds.
3. **[MEDIUM] Every policy write is audited, and relaxing changes need
   two people.** Each create, activate or withdraw writes
   `audit.Record` in the same transaction, containing: actor (platform
   principal), jurisdiction, before/after row, IP, `reason_code`,
   `legal_review_reference`. Withdrawing an active row, or activating a
   higher threshold than the one it replaces, relaxes enforcement for
   every tenant in that jurisdiction. That needs four-eyes approval (a
   second platform principal), in line with CLAUDE.md's rule for
   high-impact administrative actions. If four-eyes is deferred, record
   the deferral as a decision; do not drop it silently.
4. **[HIGH — launch-blocking] Pin down the read semantics so the player
   cannot influence the result.**
   - (a) `kyc_verifications` has no per-player uniqueness (migration
     0040). Evaluate only the **latest** row for (tenant, brand,
     player), ordered deterministically (`created_at DESC, id DESC`).
     An older `approved` row must never satisfy the check when a newer
     row is `pending`/`rejected`/`expired`. An `EXISTS(status='approved')`
     query is the bug to avoid.
   - (b) Treat `expires_at <= now()` as `failed` even when `status` is
     still `approved`. `expired` status is set only by a provider
     update, and a missed callback must not extend a verification's
     validity.
   - (c) A policy-lookup error must return `unavailable`/deny.
     "Query failed" must never be mapped to "no rows", which would give
     `not_required`.
   - (d) Select policies **only** by `LicensingJurisdictionID`, taken
     from the tenant's licence. `JurisdictionCode` (derived from geo or
     player evidence, and so player-influenceable through a VPN or a
     declared country) must not select or relax a policy. The table has
     no column it could match anyway. Remove it from
     `EnforcementParams`, or document it as unused until a recorded
     decision says otherwise.
   - (e) `Amount`/`AssetCode` are player-chosen, so threshold triggers
     invite structuring. When HD-KYC-1/2 are implemented, compute
     cumulative totals server-side from ledger/intent history. Include
     in-flight (initiated, not yet settled) deposits, and evaluate
     across all of the player's assets or wallets, not only the asset
     of the current request.
   - (f) The §7 cross-tenant test must use a *valid* token for tenant B
     with a tenant A `player_account_id`. The expected result is a
     rejected or not-found outcome from the caller, never an evaluation
     against A's rows.
5. **[HIGH — launch-blocking] Denial audit records must survive the
   rollback.** §3.6 says the decision row and the `audit.Record` are
   written "in the same transaction as the domain effect", and §7 says
   a KYC-denied withdrawal request's "transaction rolls back". Together
   these delete the only record of the denial. Denials must be durably
   committed, either by committing a transaction that contains only the
   audit/decision rows, or by the separately committed pattern
   `internal/casino/orchestrator.go:757` already uses. Test: after a
   denial there is exactly one decision row and one audit row, and zero
   ledger or hold effect.
6. **[MEDIUM] The payout gate must cover every path to provider
   submission.** Today the only caller is
   `withdrawal_handlers.go:827`, followed by `MarkSubmitted` at `:850`.
   Any future retry, re-submit-after-timeout or admin force-submit path
   must also pass through the KYC check. Add a test or raw-guard (in the
   style of `raw_guard_test.go`) asserting that `MarkSubmitted` is
   reachable only after a KYC evaluation in the same transaction.
7. **[MEDIUM] Staff read API
   (`GET /v1/admin/kyc/enforcement-decisions`).**
   - Tenant comes from the authenticated staff context
     (`db.WithTenant(staff.TenantID)`), never from a query parameter.
   - A `player_account_id` belonging to another tenant returns the same
     404 as a nonexistent one.
   - Roles: `compliance` and `platform_admin` only.
   - A platform-admin cross-tenant read needs an explicit tenant path
     parameter, and the access itself is audited.
   - Pagination is keyset on `(decided_at, id)`, with a server-enforced
     maximum page size and a default when none is supplied.
   - Response fields are exactly those §6 lists. No
     `kyc_verifications.reason`, no person or document data, no amount.
   - Required tests: a valid tenant B token asking for a tenant A
     player gets 404 and no rows; a tenant-bound role other than
     compliance gets 403.
8. **[MEDIUM] Players see status only (HD-10.3-3).**
   - The player-facing decline must never include `matched_trigger`,
     policy ids, `policy_version`, or anything that implies a threshold
     or its value. Revealing that a cumulative-deposit trigger fired, or
     where, lets a player structure deposits around it.
   - `unavailable` appears to the player as a generic retryable failure.
   - If `DepositIntent.reason` (`"kyc_required:<code>"`) is ever
     returned verbatim to players, `<code>` must be a closed enum that
     reveals no more than the player's own verification status.
   - `kyc_enforcement_policies` is readable by every tenant through
     `USING (true)`. That is acceptable, since it is not tenant-secret,
     but no player-reachable API may return its rows.
9. **[LOW] Make dormancy observable.** Provide a platform-admin read,
   or an ops report, listing each licensing jurisdiction that has live
   tenants and no `active` row per `trigger_type`. That supports the
   launch decision in ruling (b) and keeps "unconfigured" from being
   invisible.
10. **[LOW] Fixture values.** The §3.7 fixture-value discipline is
    endorsed. At implementation, `security` will check that no numeric
    threshold appears in non-test Go or in migration SQL.
