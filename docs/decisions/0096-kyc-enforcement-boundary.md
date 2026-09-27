# ADR 0096 — KYC Enforcement Boundary

Status: **ACCEPTED — IMPLEMENTED, pending security/ledger-finance/code
review.** PRH-I3 (`identity-compliance`) implemented the mechanism this
paper designs: `internal/kyc.EvaluateEnforcement`, migration 0100, the
withdrawal-request gate, `withdrawal.DenyForCompliance`, the casino/
sportsbook play gates, and the staff read API. See §15
("Implementation record") for the condition map, deviations, and what
remains out of this task's scope (the deposit gate and the payout-dispatch
call site, both wired by PRH-I1 calling this package's exported service).
Migration allocated and implemented as **0100** (re-allocated from the
original 0101 sketch, 2026-09-27, `docs/governance/task-registry.md`'s
PRH allocation table — every "0101" reference in this document below was
renumbered to 0100 in the same change).

Baseline: branch `claude/focused-wright-jw88w9`, `HEAD 1560ad0` at initial
authoring. **Staleness note (casino review condition 3, §11):** by the
time of review the working tree had advanced to `3d50b3c`/`07c8103`, and
`§1`'s `internal/casino/orchestrator.go:952/1456/1586` line citations no
longer match current line numbers (the reviewed functions now sit
roughly in the 1000–1900 range; `postBet`'s RG/Risk calls were confirmed
at ~1184/1225–1247). The *function names and relative call order* §1/§2.4
rely on remain correctly identified and were independently re-verified by
`casino` (§11) against `3d50b3c` — only the line numbers are stale. This
is recorded as a known cosmetic gap, to be refreshed against the actual
HEAD at PRH-I3 implementation time rather than corrected speculatively
here against a commit this paper cannot re-read. Sources
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
	// 0031 §9) alongside LicensingJurisdictionID. migration 0100's
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
  **Arithmetic (ledger-finance C6, §12.2):** `threshold_minor_units` is
  compared against a computed total using exact integer arithmetic only —
  SQL `NUMERIC` in the query itself, or Go `big.Int`/`int64` with an
  explicit overflow check if the comparison ever crosses into
  application code — **never `float64`**, per CLAUDE.md's own money-
  representation rule. A threshold is compared **only within the same
  `asset_code`** as the row that defines it; aggregating cumulative
  deposits across different assets would need an FX or
  `ConversionOperation` basis (ADR 0007/0037), which is an explicit
  extension of HD-KYC-1, not an implicit sum this mechanism performs on
  its own. Whether reversed/charged-back deposits net off the cumulative
  figure is likewise part of HD-KYC-1's content, not decided here.
- **(f) Cross-tenant negative test shape.** Any test proving cross-tenant
  isolation must use a genuinely valid token for tenant B carrying tenant
  A's `player_account_id`, and assert the caller-visible result is a
  rejection or not-found — never that the evaluation silently ran against
  tenant A's own rows under tenant B's authorization context.
- **(g) A never-decided orphan is excluded from "latest" (§19 below, RV-PRH-I2
  KYC code review F1, 2026-09-27; ADR 0095 §15.3.3).** ADR 0095 §15.2's `CreateVerification`
  phase A commits an intent row — `status='unverified'`,
  `provider_reference IS NULL` — *before* the provider is ever called; a
  phase-B/C failure (a vendor outage, a resolver failure, a crash) leaves
  that row exactly as committed, forever if nothing retries it. Such a row
  received **no decision at all** — not a vendor decision, not a staff
  decision — and (a)'s "latest row, deterministically ordered" rule must
  not treat it as this account's current state merely because it is
  newest: a transient vendor outage would then manufacture a `failed`
  outcome for an already-`approved` player starting a routine
  re-verification, and, via crossAccountRejectedOverlay's identical
  "each OTHER account's own latest row" read, would let such an orphan on
  one account mask an already-decided rejection on that SAME account by
  simply being newer. **Both** `readLatestVerificationByPlayerAccount`'s
  own selection **and** `crossAccountRejectedOverlay`'s per-other-account
  subquery therefore add `NOT (status = 'unverified' AND
  provider_reference IS NULL)` to their own predicate — the row is invisible
  to "latest DECIDED row" selection, not merely re-classified once selected.
  An account whose only rows are such orphans is treated identically to an
  account with no verification row at all (`found=false`, `OutcomeFailed`
  either way — this rule changes nothing in that case). A row that
  *did* receive a decision — the provider round-trip completed (whatever
  the resulting status, including a non-terminal one like `pending`), a
  callback applied, or a staff review applied — is never excluded by this
  rule, however non-terminal that decision is; (a)'s original latest-row
  ordering is otherwise completely unchanged. See §19 below and ADR 0095 §15.3.3 for the
  cross-ADR implementation record (identity-compliance is the owner of
  both ADRs and is explicitly authorized to make this specific,
  coordinated change to `internal/kyc/enforcement.go`'s read semantics —
  this is not a weakening of enforcement, it removes a spurious DENY that
  a transient failure elsewhere in the system could otherwise manufacture,
  and every genuine deny this ADR's structural rule requires is preserved
  byte for byte).

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
   `trigger_type`** in migration 0100 (§3.6) — this rule is not a
   configurable row at all, exactly as the original sketch already
   stated, now applied correctly (always-on, not a one-time gate).
2. **Threshold — cumulative deposit amount, EDD amount, registration
   tier.** These are exactly the values HDR-J-6 and legal review have
   not yet supplied. The mechanism (migration 0100, below) exists and
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
  operation)`. It exists as its own `trigger_type` (§3.6/migration 0100)
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
- **No new provider-visible outcome class (casino review condition 1,
  §11).** A KYC-required decline at the casino/sportsbook bet call site
  is a fifth instance of the *existing* `OutcomeDeclined`/`DeclineReason`
  shape `postBet` already uses uniformly for the RG denial, the Risk
  denial, the insufficient-funds denial, and the tombstoned-original
  denial — **not** a new provider-visible outcome variant. The
  `DeclineReason` value is `decision.Code` (e.g. `"kyc_required:pending"`,
  matching §6's own convention for deposit/withdrawal), and no provider
  adapter needs new handling: adapters already treat `DeclineReason` as
  an opaque string and branch only on `OutcomeDeclined` itself.

### 3.6 Migration 0100 — sketch

Two tables, both platform-wide (no `tenant_id`), mirroring
`jurisdiction_precedence_configs`'s RLS posture (readable everywhere,
writable only by a platform-scoped `platform_admin`/`compliance`
principal) plus one tenant-scoped decision-audit table.

```sql
-- 0100_kyc_enforcement_policy_and_decision_audit.up.sql

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

**Commit discipline, corrected (security condition 5 / ledger-finance
C1).** The original sketch said both rows are "written in the same
transaction as the domain effect it gated" — which, for a **denial**,
directly contradicts §7's own original test-plan wording ("the
transaction rolls back") and is a real defect: a caller that rolls back
its whole transaction on a KYC deny would silently discard the very
decision/audit rows this ADR exists to make durable, exactly the class
of bug `internal/casino/orchestrator.go:757`'s own doc comment already
documents finding and fixing once for RG denials, and the identical
class `payments.InitiateDeposit`'s own comment records for the deposit
RG check. The corrected, uniform rule for every enforcement point in
this ADR:

- **On `allow` (`not_required`/`passed`):** the decision row and the
  `audit.Record` call are written in, and commit with, the **same**
  transaction as the domain effect (the ledger posting, the state
  transition) they gated — exactly as the original text described. This
  case was never the problem.
- **On `deny` (`pending`/`failed`/`unavailable`):** the transaction that
  commits contains the decision row and the audit record and **no
  domain effect** — no ledger posting, no state-machine INSERT/UPDATE
  beyond what a pure read requires. This is achieved per call site
  exactly as `internal/casino`'s own established pattern already does it
  (a query first, before any state-changing statement runs, so a denial
  path never needs to undo anything): §5's and §12.2 C1's per-call-site
  designs (deposit, withdrawal request, withdrawal payout dispatch, play)
  each place the KYC evaluation **before** the first state-changing
  statement of their respective transaction, so a deny simply never
  reaches the point where anything would need to be rolled back — the
  transaction that commits is, by construction, "decision + audit only."
  Where a call site's existing structure makes that awkward (withdrawal,
  see §12.2 C1's exact required shape), the enforcement point returns a
  distinguished, typed result that its caller commits explicitly, rather
  than a Go `error` that would trigger the domain's ordinary rollback
  path.
- **Required test (added to §7):** for every one of the five enforcement
  points, a denial leaves exactly one `kyc_enforcement_decisions` row and
  one `audit_log` row committed, and zero ledger effect and zero
  unintended state-machine transition — proving durability empirically,
  not just asserting it in prose.

### 3.7 No test-only or production default values

No row is seeded by migration 0100 — the same discipline ADR 0043 §
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
  **Exact withdrawal placement (revised per ledger-finance C1/C2, §12.2):**
  in `RequestWithdrawal`, the KYC evaluation runs immediately after the
  existing idempotency-replay lookup and before `ledger.
  GetOrCreateAccounts`/`LockProjectionsForPosting` — i.e. before the first
  statement that could need undoing. In the payout-dispatch path, the KYC
  evaluation runs inside the **same** L1 (`lockRequestForUpdate`)
  transaction `LockApprovedForSubmission` already holds, before that
  transaction commits, and therefore before ADR 0095's `T1p`
  (`approved→submitted`, the withdrawal claim) ever runs — see §12.2 C5
  and the ADR 0095 coordination note below for exactly why that
  transaction is the one that matters once ADR 0095 lands.
- **Tenant isolation.** `kyc_enforcement_decisions` carries `tenant_id`
  and standard `tenant_isolation` RLS, identical to `kyc_verifications`
  (migration 0040). `kyc_enforcement_policies` is platform-wide, exactly
  like `jurisdiction_precedence_configs` — read by every tenant sharing a
  licensing jurisdiction, written only by a platform-scoped connection.
  `EvaluateEnforcement` itself takes `tx pgx.Tx` already scoped by the
  caller (`db.WithTenant`), exactly like `rg.EvaluateEligibility`/`risk.
  Evaluate` — it never opens its own connection or scope.
- **Performance / lock class.** `EvaluateEnforcement` issues plain
  `SELECT`s only: `kyc_verifications`' **latest** row by
  `(tenant_id, brand_id, player_account_id)` ordered `created_at DESC,
  id DESC` (§2.6(a)) for `passed`/`pending`/`failed`, and — for
  `deposit`/`casino_play`/`sportsbook_play` only —
  `kyc_enforcement_policies` by `(licensing_jurisdiction_id,
  trigger_type)` filtered to `status = 'active'`. **Revised (the original
  sketch's `SELECT EXISTS(... completed withdrawal ...)` query is
  removed):** because §3.2 point 1 no longer conditions the withdrawal
  rule on withdrawal history at all (security condition 1 / ledger-finance
  C3 — it is unconditionally required, not a one-time exemption),
  `withdrawal_hold`/`withdrawal_payout` need **no policy-table lookup and
  no `withdrawal_requests` history query** — they read only the player's
  latest `kyc_verifications` row (scoped by `PersonID`, §2.2) and apply
  §2.3's mapping directly, which is simpler and cheaper than the original
  design, not just safer. **No row lock is taken by this function** — it
  reads the same way `rg.EvaluateEligibility`'s restriction lookup and
  `risk.Evaluate`'s rule lookup already do, so it introduces no new
  entry into ADR 0082's canonical lock-class ordering (L0–L4); it composes
  safely wherever RG/Risk already compose today, at the position §2.4
  states, before the caller's own ledger lock is taken. Casino/sportsbook
  reviewed this specific claim empirically (§11: verified the insertion
  point precedes `GetOrCreateAccounts`/the pre-lock balance check in
  current code) and confirmed it holds as designed.
- **No provider I/O.** `EvaluateEnforcement` never calls a `KYCProvider`
  — it reads the platform's own already-materialized verification state.
  This is the same separation ADR 0028 §4 already establishes (the
  provider updates `kyc_verifications` via its own callback/orchestrator
  path; enforcement only ever reads the result). It therefore carries
  none of F-POOL-2's outbound-I/O-inside-a-transaction risk and needs no
  ADR-0095-style restructuring.
- **Concurrency and TOCTOU (ledger-finance C4, §12.2 — accepted, and
  explicitly bounded, not ignored).** Because no lock is taken and the
  function is a pure read plus comparison, two concurrent calls for the
  same player can run concurrently with no serialization need beyond what
  the domain's own transaction already provides — a KYC status change (a
  staff approval, a provider callback landing) that commits between two
  concurrent deposit attempts is visible to whichever attempt's
  transaction starts after that commit, exactly like every other
  read-then-decide check in this codebase; no new race exists that
  RG/Risk do not already carry today for the identical pattern. Under
  READ COMMITTED, a revocation that commits **after** `EvaluateEnforcement`
  reads `kyc_verifications` but **before** the gated transaction itself
  commits is not seen by that transaction. This window is accepted, on
  three conditions ledger-finance's C4 states and this ADR adopts as
  binding: (a) the gate result is always computed inside the transaction
  that performs the gated effect, never passed between transactions
  (§2.5 already requires this); (b) every re-entry — a retry, the ADR
  0095 sweeper, a resubmission after rollback — re-evaluates fresh, never
  reuses a prior decision; (c) the window is bounded to at most one
  transaction's duration, documented as such rather than left implicit.
  Closing the window completely (e.g. `FOR SHARE` on the verification
  row) would add a new lock class to ADR 0082 and a contention point
  between KYC callbacks and payouts for a risk this ADR judges
  disproportionate; a revocation that lands inside the window is handled
  like any other post-dispatch compliance finding (§12.2 C5, immediately
  below).
- **Coordination with ADR 0095 (ledger-finance C5, §12.2 — binding on
  PRH-D1/the payments-domain owner of that ADR).** ADR 0095
  (`docs/decisions/0095-provider-io-transaction-boundary-and-payment-
  contract.md`) restructures withdrawal payout dispatch into Phase A
  (claim, in a DB transaction) → Phase B (the outbound `Withdraw` call,
  with no transaction held) → Phase C (`applyEvidence`). Read against
  ADR 0095 §5.2 and §4.3 (transition `T1p`, `∅→submitting`, "withdrawal
  `approved→submitted` in the same tx" under L1 `FOR UPDATE`): the KYC
  gate for `withdrawal_payout` (row #5) must be evaluated inside **Phase
  A, before `T1p` commits** — the same L1-locked transaction that claims
  the request for dispatch, which is exactly "the last transaction that
  commits before the first outbound `Withdraw` call for that intent."
  Concretely, this ADR requires ADR 0095's Phase A to include the KYC
  check alongside its existing approver-eligibility check and L1 lock,
  in that transaction, before `T1p`'s `CAS` runs. If ADR 0095's sweeper
  ever claims and dispatches a request that was left in a
  "prepared, not yet sent" state (rather than dispatching synchronously
  within the same request), the sweeper's own claim transaction must
  re-run the KYC gate before that first send — the gate is never
  satisfied once and cached across a later, separate dispatch attempt.
  **After a request has possibly reached the provider** (ADR 0095 states
  `submitting`/`pending`/`ambiguous`/`disputed`, or any state ADR 0095's
  own `ever_possibly_sent` flag has set), **no KYC outcome may trigger a
  hold reversal or any other automated release** — only provider evidence
  (ADR 0095's `applyEvidence`, transitions T7/T8) may resolve the
  attempt. `DenyForCompliance` (§12.2 C2) is therefore legal **only from
  `approved`** (pre-dispatch) — never from `submitting`, `pending`,
  `ambiguous`, `disputed`, `succeeded`, or `declined`. ADR 0095's
  transition table must list `DenyForCompliance`
  (`approved→rejected`) among its pre-dispatch terminal transitions and
  must not permit it once `T1p` has committed, matching the same
  reasoning ADR 0095 §4.7 already gives for why `Reject`/`Cancel` race
  the claim on the same L1 row rather than acting on a claimed request. A
  KYC revocation that lands after dispatch is a compliance case, not a
  ledger action — reversing a hold while the PSP might still pay would
  create a real double-spend (the hold is restored to `player_cash` and
  the payout also completes).

---

## 6. API / OpenAPI exposure

- **Staff read of decisions — fully specified per security condition 7
  (§13).** `GET /v1/admin/kyc/enforcement-decisions?player_account_id=...`:
  - Tenant comes **only** from the authenticated staff context
    (`db.WithTenant(staff.TenantID)`) — never from a query parameter or
    any client-supplied value.
  - Roles: `StaffRoleCompliance` and `StaffRolePlatformAdmin` only
    (mirrors the existing `PermVerificationReview` precedent); no other
    tenant-bound role may read it.
  - A `player_account_id` belonging to a different tenant returns the
    same 404 a nonexistent id would, matching this codebase's established
    "never confirm existence of a resource outside the caller's own
    authorization" pattern (ADR 0031 §8, ADR 0029 §4a).
  - A platform-admin **cross-tenant** read (a platform_admin looking at a
    tenant it does not itself belong to) requires an explicit tenant path
    parameter, distinct from the ordinary tenant-scoped route, and that
    access is itself audited (actor, target tenant, target player, IP).
  - Pagination is keyset on `(decided_at, id)`, with a server-enforced
    maximum page size and a default applied when the caller supplies
    none — never unbounded, never offset-based.
  - Response fields are exactly `outcome`, `operation`, `matched_trigger`,
    `policy_version`, `decided_at` (the `kyc_enforcement_decisions`
    columns already listed in §3.6) — never `kyc_verifications.reason`,
    never person/document data, never an amount. `reason` stays governed
    by HD-10.3-3's existing bound/sanitized staff-only exposure, unchanged
    by this ADR.
  - **Required tests:** a valid tenant B token requesting a tenant A
    player gets 404 and zero rows; a tenant-bound role other than
    `compliance`/`platform_admin` gets 403; the cross-tenant
    platform-admin path is audited.
- **Players see status only (HD-10.3-3, extended per security condition
  8, §13 — not reopened).** No new player-facing field is added anywhere
  for *why* a deposit or withdrawal was denied beyond what already
  exists, and the following are explicit, binding exclusions from every
  player-reachable surface: `matched_trigger`, any policy row id,
  `policy_version`, or anything from which a player could infer that a
  cumulative-deposit trigger fired, or at what value (revealing the
  threshold's existence invites structuring around it — the same reason
  §2.6(e) requires server-side, not client-visible, cumulative
  computation). A denied deposit surfaces as `DepositIntentDeclined` with
  a bounded internal `reason` string (`"kyc_required:pending"` style,
  mirroring `"rg_ineligible:"+eligibility.Code}` at
  `orchestrator.go:558`) — if this string is ever returned verbatim to a
  player, `<code>` must be drawn from a **closed enum** that reveals no
  more than the player's own verification status (`pending`/`failed`),
  never a policy internal. `unavailable` is surfaced to the player as a
  generic, retryable failure — never as a distinct "compliance system
  down" message. A denied withdrawal request/payout dispatch follows the
  identical convention: the player-facing response states the withdrawal
  was declined and the request's own state (`rejected`), never a
  compliance reason code beyond the closed enum above.
  `kyc_enforcement_policies` remains readable platform-wide at the
  database layer (`USING (true)`, §3.6) — that is acceptable because its
  content is not tenant-secret, but no player-reachable API may ever
  return its rows or derive a response from them directly.
- **Dormancy observability (security condition 9, LOW, §13 — designed,
  not deferred).** A platform-admin-only read,
  `GET /v1/admin/kyc/enforcement-policies/dormant-jurisdictions`, lists
  every licensing jurisdiction with at least one active tenant and **no**
  `active` `kyc_enforcement_policies` row for a given `trigger_type` —
  computed as `SELECT DISTINCT t.licence_jurisdiction_id, tt.trigger_type
  FROM tenants t CROSS JOIN (VALUES ('cumulative_deposit'), ('edd_amount'),
  ('registration_tier')) tt(trigger_type) WHERE NOT EXISTS (SELECT 1 FROM
  kyc_enforcement_policies p WHERE p.licensing_jurisdiction_id =
  t.licence_jurisdiction_id AND p.trigger_type = tt.trigger_type AND
  p.status = 'active')` (illustrative; the real query additionally scopes
  to tenants with live/active status). This supports the launch decision
  security's ruling names (§3.2 point 2) by making "unconfigured" visible
  to operations rather than an implicit, undiscoverable state — a human
  reviewing this report before go-live is how HD-KYC-1/2/3/8 actually get
  closed rather than silently forgotten.
- **OpenAPI.** `platform-api.yaml` gains: the new admin decisions route
  (with its keyset pagination parameters and exact response schema), the
  cross-tenant platform-admin variant, and the dormancy report route; no
  change to any player-facing schema (deposit/withdrawal response shapes
  already carry a generic, closed-enum decline reason field, reused, not
  widened).

---

## 7. Test plan

Revised to close every QA gap (§10 items 1–7), every ledger-finance C7
test, and every security-required test named against a specific
condition in §13 — each row below states which review requirement it
closes, so none is asserted once generically and assumed to generalize
(QA gap 1).

### 7.1 Unit

| Test | Closes |
|---|---|
| Outcome mapping (§2.3) is exhaustive: every `(policy-row-present/absent) × (verification-status, including expired)` combination maps to exactly one of the five outcomes, with a mutation pass over `EvaluateEnforcement`'s branches (a mutant that flips any branch must turn a test red) | QA gap 5 |
| Latest-row selection (§2.6(a)): an older `approved` row never satisfies the check when a newer row for the same player is `pending`/`rejected`/`expired`; ordering is `created_at DESC, id DESC` | security condition 4(a) |
| Expiry (§2.6(b)): a stored `status = 'approved'` row with `expires_at <= now()` evaluates as `failed`, not `passed` | security condition 4(b) |
| A forced query/DB error returns `unavailable`, never `not_required` — "no rows" and "query failed" are asserted as distinct code paths, not just distinct outcomes | security condition 4(c) |
| `LicensingJurisdictionID` selects the policy; a manufactured/mismatched `JurisdictionCode`-shaped input (if any residual field exists at implementation time) has zero effect on the outcome | security condition 4(d) |
| Threshold comparison uses `NUMERIC`/`big.Int` exact arithmetic, cross-checked against a hand-computed value at the `int64` boundary; a comparison across two different `asset_code`s is rejected/never attempted | ledger-finance C6 |
| Migration 0100 CHECK constraints: manual true/false checklist covering every `trigger_type`/column-shape combination (including `play`/`play_operation`), and the `status <> 'active' OR legal_review_reference IS NOT NULL` constraint — the SQL CHECK is exactly the construct this project already treats as "no mutation tool applies, use a manual branch-coverage checklist" | QA gap 5 |

### 7.2 Integration

| Test | Closes |
|---|---|
| Deposit initiation: RG-denied player never reaches KYC evaluation (order preserved); a `pending`-outcome KYC denial, a `failed`-outcome denial, and an `unavailable`-outcome denial are each exercised by name (not asserted once and assumed to generalize) and never reach `attemptDeposit`/the provider | §1 row #1; QA gap 1 |
| Withdrawal request: a `pending` KYC denial, a `failed` KYC denial, and an `unavailable` KYC denial are each exercised by name; in every case no `withdrawal_requests` row is inserted and no ledger effect is posted, and the decision + audit rows are **committed** (not rolled back — the C1 fix, verified empirically, not just designed) | §1 row #3; ledger-finance C1, C7; security condition 5; QA gap 1 |
| Withdrawal payout dispatch: an approved-but-since-`rejected`/`expired` request is denied by `DenyForCompliance` at the `LockApprovedForSubmission`/L1 boundary, never reaching `MarkSubmitted`/the provider; the reversal posts exactly one `withdrawal_rejected` transaction reversing the original hold, `SUM(DEBITS)==SUM(CREDITS)` holds, and the decision + audit rows are committed with the reversal in the same transaction | §1 row #5; ledger-finance C1, C2, C7 |
| **Replaces the original, defective "exempt after one prior completed withdrawal" row (security condition 1 / ledger-finance C3):** a `Person` with one prior `completed` withdrawal whose *current* latest verification is `rejected`/`expired` is denied on a second withdrawal request and at payout dispatch — the structural rule never expires after a first pass, and a jurisdiction's `edd_amount` trigger (if active) still applies independently, additively, on top of it | §1 rows #3/#5 |
| The structural rule is scoped per `Person`, not per wallet or `PlayerAccount`: a `rejected`/`expired` player cannot bypass it by opening a new wallet or a second `PlayerAccount` under the same `Person` | §1 rows #3/#5; ledger-finance C3 |
| A KYC revocation committed **after** payout dispatch (the request has reached `submitting`/`pending`/`ambiguous`/`disputed` under ADR 0095, or `submitted` today) causes **no** automated reversal — only provider evidence resolves the attempt | ledger-finance C5; §5's ADR 0095 coordination note |
| A replayed withdrawal-submit call after a payout-time KYC deny gets `ErrStateConflict` and never reaches the provider | ledger-finance C7 |
| A replayed `RequestWithdrawal` idempotency key after a successful hold does not re-run the KYC gate (pure replay, matching `TestRequestWithdrawal_IsIdempotentOnRetry`'s existing pattern) | ledger-finance C7; CLAUDE.md idempotency rule |
| A retried deposit intent (e.g. after a client timeout) that re-enters `InitiateDeposit` for the same idempotency key: expected behavior (does it re-evaluate KYC and, if so, does a second `kyc_enforcement_decisions` row get written — append-only and acceptable — or must it dedupe) is stated explicitly and tested, not left implicit | QA gap 4 |
| Casino/sportsbook bet placement with **no active `play` policy**: KYC evaluates `not_required` and never denies; RG/Risk denials still short-circuit before KYC runs; existing RG/Risk regression suites pass unmodified | §1 rows #7/#10 |
| Casino/sportsbook bet placement with an **active `play` policy** (test-fixture row, clearly marked per §3.7): `passed` allows; `pending`/`failed`/`unavailable` each deny, exercised by name, with no bypass; a `casino_play` policy never governs `sportsbook_play` or vice versa (`play_operation` enforced, not advisory) | §1 rows #7/#10 |
| A KYC-declined bet's provider redelivery of the same `provider_tx_id` is freshly re-evaluated against RG→Risk→KYC's then-current state, not replayed as a no-op (the idempotency short-circuit only fires for an already-*posted*/succeeded bet) — a policy/verification state change between the two attempts may legitimately flip the outcome | §11 condition 2 (casino review) |
| Migration 0100 up→down→up round-trips cleanly on a fresh database, for both `kyc_enforcement_policies` and `kyc_enforcement_decisions`, per this codebase's own migration-reversibility CI rule | QA gap 2 |
| OpenAPI contract test for `GET /v1/admin/kyc/enforcement-decisions` (and the dormancy report route), using this codebase's existing plain-text/substring structural-check convention (`internal/httpserver/openapi_paymentswebhook_contract_test.go`'s pattern) — stated explicitly, not silently discovered as a limitation later | QA gap 3 |

### 7.3 RLS

| Test | Closes |
|---|---|
| A tenant-scoped `compliance` connection's INSERT/UPDATE on `kyc_enforcement_policies` is rejected at the database, not merely by the HTTP handler | security condition 2 |
| A tenant-scoped connection carrying a `platform_admin`-shaped role but with `app.tenant_id` set is rejected (the NULLIF/tenant-unset predicate, not the role name, is what gates) | security condition 2 |
| A platform-service-scoped connection with no `app.platform_admin_principal_id` set is rejected | security condition 2 |
| A genuine platform-admin-scoped connection succeeds, and the inserted row's `created_by_actor_id` matches the acting principal | security condition 2 |
| A `db.WithPlayerScope` connection cannot read another tenant's `kyc_enforcement_decisions`; `FORCE ROW LEVEL SECURITY` is confirmed active on both tables (a superuser-equivalent bypass would otherwise silently defeat every policy above) | schema |

### 7.4 Concurrency

| Test | Closes |
|---|---|
| Two concurrent deposit attempts racing a staff KYC approval that commits between them each see a consistent, individually-correct outcome (no torn read) — mirrors `TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds`'s pattern applied to a read-then-decide check | §1 rows #1/#3/#5 |
| A concurrent `Reject`/`Cancel` racing `DenyForCompliance` on the same withdrawal request produces exactly one release (L1 serializes them; the loser gets `ErrStateConflict`) | ledger-finance C7 |
| The ADR 0082 lock-order harness passes unmodified on all five gated paths, including the two withdrawal call sites and the two play call sites | ledger-finance C7; §11 condition (casino) |
| Placing a bet takes no additional row lock from the new KYC step — proved by running the existing `internal/casino`/`internal/sportsbook` lock-order harnesses (ADR 0082) unmodified against the amended orchestrator and confirming no new lock-class entry appears | §1 rows #7/#10 |

### 7.5 Negative / security

| Test | Closes |
|---|---|
| A client-supplied field cannot influence `Outcome` (fuzz `EnforcementParams` for any player-controllable path into the decision) | all |
| A cross-tenant test uses a **genuinely valid** token for tenant B carrying tenant A's `player_account_id`; the caller-visible result is a rejection or not-found, never a silent evaluation against tenant A's own rows | security condition 4(f) |
| `unavailable` (a forced DB error / malformed policy row in a test harness) denies at every enforcement point, with no operation-specific bypass | all |
| A raw-guard test proves `withdrawal.MarkSubmitted` is reachable only after a KYC evaluation ran in the same transaction — closing every future retry/resubmit/admin-force-submit path, not only today's one caller | security condition 6 |
| A valid tenant B staff token requesting a tenant A player via the admin decisions route gets 404 and zero rows; a tenant-bound role other than `compliance`/`platform_admin` gets 403 | security condition 7 |
| The player-facing decline surface never contains `matched_trigger`, a policy id, or `policy_version`, under any field name, at any of the five enforcement points | security condition 8 |

### 7.6 Fail-closed / audit / SAR-adjacent

| Test | Closes |
|---|---|
| A migration 0100 row with `status = 'active'` and no `legal_review_reference` is rejected by the CHECK constraint before it can ever govern a decision | schema |
| Every `EvaluateEnforcement` call at every enforcement point writes exactly one `kyc_enforcement_decisions` row and one `audit.Record` call, with no PII/document content in either; on `allow` these commit with the domain effect, and on `deny` they commit **without** it (§3.6's corrected commit discipline, empirically proven, not merely asserted) | ledger-finance C1; security condition 5 |
| `kyc_enforcement_decisions` plus `kyc_verifications`/`audit_log` together give a compliance reviewer a complete, joinable trail of "what was decided, when, against what policy version" for any player | staff tooling |
| The projection matches the recomputed-from-ledger balance after a `DenyForCompliance` reversal, with zero drift from the hourly reconciliation job | ledger-finance C7 |

### 7.7 Performance and CI budget (QA gaps 6–7 — closed with concrete criteria, not prose)

- **Measurable pass criterion (QA gap 7):** the `not_required` (no active
  policy) path on the casino/sportsbook bet-placement hot path adds no
  more than **+2 ms at p95** versus the pre-KYC baseline for `postBet`/the
  sportsbook bet-placement handler, measured by a benchmark comparison
  with an objective pass/fail (not a subjective "no measurable
  regression"). Deposit/withdrawal paths, being lower-frequency and
  already amount-validated, are not benchmarked to the same bound; a
  regression there is caught by the existing integration-suite timeout
  budget instead.
- **CI time budget (QA gap 6):** the new integration/concurrency rows in
  §7.2/§7.4 are expected to land in `internal/withdrawal`,
  `internal/payments`, `internal/casino`, and `internal/sportsbook`'s
  existing integration lanes (not a new package), adding an estimated
  low tens of seconds per package at implementation time — a real number
  must be measured and recorded at implementation, and if it materially
  narrows the CI margin for any of those packages' existing lanes, the
  concurrency-heavy rows (the two lock-order-harness re-runs and the
  `DenyForCompliance`-vs-`Reject` race) are named as the first candidates
  for `.github/workflows/ci.yml`'s existing `TIMING_LANE_TESTS`-style
  isolation, rather than left to default into whichever lane happens to
  be slowest.

---

## 8. Implementation breakdown and dependency requests

Ownership per PRH-I3 (`identity-compliance` + `payments`, `casino`,
withdrawal owners via dependency requests):

1. **`internal/kyc`** (identity-compliance): `EvaluateEnforcement`,
   `EnforcementParams`/`Decision`/`Outcome` types (§2.2, revised:
   `PersonID` added, `JurisdictionCode` removed), the latest-row/expiry
   read (§2.6), the threshold-policy lookup, migration 0100 Go-side
   repository (mirrors `jurisdiction/evaluation_policy_admin.go`'s shape:
   `CreateEnforcementPolicy`/`ActivateEnforcementPolicy`/
   `WithdrawEnforcementPolicy`, each writing its own `audit.Record` per
   security condition 3), and `kyc_verifications.expires_at` if it does
   not already exist (§2.6(b) — part of this ADR's implementation scope,
   not a follow-up).
2. **`internal/payments`** (payments, reviewed by identity-compliance):
   one new call to `kyc.EvaluateEnforcement` in `InitiateDeposit`,
   immediately after the existing RG check, mirroring the RG denial's
   own `finalizeDeclined` pattern exactly (`"kyc_required:"+decision.Code`,
   never `matched_trigger`/`policy_version` per security condition 8).
   The decision/audit rows commit with the `declined` intent, per §3.6's
   corrected commit discipline — no rollback risk here, since a decline
   already resolves to a committed `declined` row today (ledger-finance
   §12.1 confirms this shape is already sound). **Dependency request:**
   `payments` confirms the exact `DepositIntent` declined-reason string
   convention and reviews the placement.
3. **`internal/withdrawal`** (withdrawal owner — currently no dedicated
   package owner distinct from `identity-compliance`'s own review scope;
   dependency request to whichever specialist next touches this package,
   or `identity-compliance` implements directly under `ledger-finance`
   review given the ledger-adjacent hold/reversal mechanics). **Design
   corrected per ledger-finance C1/C2 (§12.2) — the original "reuse
   `Reject`'s shape" sketch was underspecified and, as written, would
   have rolled back the audit trail it was supposed to create:**
   - **`RequestWithdrawal`:** look up an existing request by
     `(tenant, player, idempotency_key)` first (replay short-circuit,
     unchanged); then evaluate KYC (§5's exact placement). **On deny:**
     write the decision row and the audit record, insert **no**
     `withdrawal_requests` row and post **no** ledger effect, and return
     a typed result (e.g. `ErrKYCRequired`) that the HTTP handler
     **commits** before mapping it to the player-facing response — never
     a bare Go `error` that would roll back the transaction and discard
     the very rows this ADR requires. **On allow:** continue exactly as
     today (`IdempotentInsert` → accounts → L3 pre-lock → `Post`).
   - **Payout dispatch:** a new function,
     `withdrawal.DenyForCompliance(ctx, tx, requestID, correlationID)`,
     analogous to `Reject`/`Fail` but **not** `Reject` itself (`Reject`
     requires `pending_review`, a staff principal, `ApproverEligibility`,
     and a `withdrawal_approvals` insert — none of which apply to a
     system-driven compliance denial). It runs inside the same L1
     (`lockRequestForUpdate`) transaction `LockApprovedForSubmission`
     already holds (§5's ADR 0095 coordination note), adding a new edge
     `approved → rejected` to `withdrawal-state-machine.md`. The posting
     mirrors `Reject`/`Fail` exactly: accounts resolved `(hold, cash)`
     via `GetOrCreateAccounts`; entries debit `player_withdrawal_hold`
     and credit `player_cash` for `wr.Amount`; `TransactionType =
     withdrawal_rejected`; `ReversesTransactionID =
     wr.HoldLedgerTransactionID`; `CorrelationID = requestID`. Idempotency
     key `requestID + ":kyc_denied"`, distinct from `:rejected`/`:failed`;
     exactly-once release via L1 plus the conditional
     `UPDATE … WHERE state = 'approved'` (`RowsAffected()==0` →
     `ErrStateConflict`, which rolls back the posting — the existing
     pattern every other release path already uses). No `reason_code` on
     the ledger transaction (migration 0021's CHECK allows one only for
     `manual_adjustment`); the compliance reason lives in the audit entry
     only (`ActorSystem`, action `withdrawal.rejected_kyc`, metadata:
     outcome, `policy_version`, `hold_ledger_transaction`,
     `release_ledger_transaction` — no sensitive content). No
     `withdrawal_approvals` row (this is not a four-eyes decision).
     Releasing to `player_cash` (not a compliance freeze) is the
     financial default so no value is left stranded without an owner;
     whether a jurisdiction ever needs a frozen-hold state instead is its
     own future human decision (ledger-finance's own note, §12.2 C2), not
     decided here. `DenyForCompliance` is legal **only from `approved`**
     (pre-dispatch) — see §5's ADR 0095 coordination note for why it must
     never fire once a request may have reached the provider. The caller
     (`LockApprovedForSubmission`'s own call site) receives a distinct
     return (e.g. `(wr, ErrKYCDeniedCommitted)`) that the handler commits,
     and that can never flow into `RouteProvider`/`Withdraw`.
     **Recommended, not blocking (ledger-finance, not part of migration
     0100):** a DB backstop partial unique index
     `(tenant_id, reverses_transaction_id) WHERE transaction_type IN
     ('withdrawal_rejected','withdrawal_failed')`, mirroring migration
     0092, for exactly-once release defense in depth. Register as its own
     follow-up.
   - **Every path to provider submission is covered (security condition
     6, §13), not only today's one caller.** Today `MarkSubmitted` is
     reached only from `withdrawal_handlers.go:827`'s submit endpoint,
     immediately after `LockApprovedForSubmission`. This ADR requires
     that any future retry, resubmit-after-timeout, or admin
     force-submit path also passes through the KYC gate before reaching
     `MarkSubmitted` — enforced by a raw-guard test (in the style of this
     codebase's existing `raw_guard_test.go` pattern) asserting that
     `MarkSubmitted` is reachable only after a KYC evaluation ran in the
     same transaction, so a new call site added later cannot silently
     bypass the gate by calling `MarkSubmitted` directly.
   - **Dependency request:** `ledger-finance` reviews the final
     `DenyForCompliance` implementation against §12.2's C1/C2/C7 in full
     before it is marked implemented (touches money movement, per this
     specialist's Review responsibility); `security` reviews the
     token/session and RLS handling of the new admin decisions route, and
     the raw-guard test above, before this is marked implemented (per
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
6. **Migration 0100**: `identity-compliance` authors it; `ledger-finance`
   reviews the `NUMERIC(38,0)`/asset-registry handling on
   `threshold_minor_units` (CLAUDE.md's own money-representation rule);
   `security` reviews RLS and the append-only trigger.
7. **OpenAPI / staff route**: `identity-compliance` + `code-reviewer`.
8. **QA**: owns the test plan in §7 as an execution gate, per its
   existing testing-strategy authority.
9. **ADR 0095 (`architect`/payments; dependency request, §5's coordination
   note).** ADR 0095's withdrawal-payout Phase A must include the KYC gate
   alongside its existing approver-eligibility check, inside the same
   L1-locked transaction, before `T1p` commits; its transition table must
   list `DenyForCompliance` (`approved→rejected`) as a pre-dispatch
   terminal transition and must not permit it after `T1p`; any sweeper
   that claims a "prepared, not yet sent" payout must re-run the KYC gate
   in its own claim transaction before the first send. This is a binding
   constraint on ADR 0095, not a request to redesign it — `identity-
   compliance` does not own ADR 0095 and does not redesign it unilaterally
   (CLAUDE.md's "no specialist redesigns shared architecture
   unilaterally" rule); this item is the dependency request `architect`
   incorporates.
10. **Four-eyes on relaxing a policy (security condition 3).** Each
    `kyc_enforcement_policies` create/activate/withdraw writes
    `audit.Record` (actor, jurisdiction, before/after row, IP,
    `reason_code`, `legal_review_reference`) in the same transaction.
    Withdrawing an active row, or activating a value that relaxes
    enforcement relative to the one it replaces, requires a second
    platform-admin principal's approval (four-eyes), per CLAUDE.md's rule
    for high-impact administrative actions. **Not designed in full here**
    — the exact mechanism (a pending-approval row, a two-step API, or
    reuse of an existing four-eyes primitive if the withdrawal-approval
    one, `internal/withdrawal/policy.go`, generalizes) is implementation
    work for PRH-I3, under `security` review. If four-eyes is deferred for
    a first cut, that deferral must be recorded as its own decision, never
    dropped silently, per security's own condition.

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
points, so that specific KYC requirement is met); the migration 0100 CHECK
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

2. **No migration up/down (reversibility) test for migration 0100.** §7
   has a CHECK-constraint fail-closed test but no down-migration test on a
   fresh database, which is this codebase's own established rule
   (`docs/testing/testing-strategy.md`'s Stage 10 W1 note: "the migration-
   reversibility CI step runs on a fresh database"). Add one for 0100
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
   checklist for the 0100 CHECK constraint.

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
  migration 0100.

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
2. **[HIGH — launch-blocking] Migration 0100 RLS and immutability must
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

---

## 14. Revision record — every review condition mapped to where it is satisfied

This section is added by the amendment that closed the security,
ledger-finance, casino, and QA reviews above (§10–§13). It exists so a
reviewer can check each condition was actually designed into the body
text, not merely acknowledged in a reply. "Design text" cites the section
that changed; "Implementation gate" notes conditions this paper commits
to but that only PRH-I3's actual code/tests can close.

### 14.1 Security (§13)

| Condition | Design text | Status |
|---|---|---|
| Ruling: deposit "allow when unconfigured" | §3.2 point 2 (deposit bullet), cross-referencing the fixed C1/C3 backstop; §6 dormancy report | Design-satisfied; launch sign-off is HD-KYC-1 + legal, not this paper |
| 1 (HIGH, launch-blocking) — every withdrawal requires `passed`, not only the first; scope per player | §1 row #3, §3.2 point 1 (rewritten), §5 performance bullet (query simplified), §7.2, HD-KYC-5 | Design-satisfied |
| 2 (HIGH, launch-blocking) — migration 0100 RLS/immutability must copy 0075 exactly | §3.6 (RLS block fully rewritten: `FORCE ROW LEVEL SECURITY`, `NULLIF`+`platform_admin_principal_id` predicates, INSERT+UPDATE-only policies, no DELETE/FOR ALL, `created_by_actor_id NOT NULL` tied to the principal, explicit lifecycle trigger permitting `draft→active`/`draft→withdrawn`/`active→withdrawn`, `kyc_enforcement_decisions` given the same `FORCE`+`TRUNCATE`-guard treatment); §7.3 tests | Design-satisfied; DB-level tests are an Implementation gate |
| 3 (MEDIUM) — every policy write audited; four-eyes on relaxing changes | §8 item 10 | Design-satisfied at the level of a binding requirement; the exact four-eyes mechanism is named as PRH-I3 implementation work, under `security` review, per item 10's own text — **not fully designed**, disclosed as such rather than claimed complete |
| 4(a) latest row only | §2.6(a) | Design-satisfied |
| 4(b) expiry independent of stored status | §2.6(b); §8 item 1 (adds `expires_at` to implementation scope) | Design-satisfied |
| 4(c) lookup error → `unavailable`, never `not_required` | §2.6(c); `OutcomeUnavailable`'s comment in §2.2 | Design-satisfied |
| 4(d) select by `LicensingJurisdictionID` only, never a player-influenceable signal | §2.2 (`JurisdictionCode` removed), §2.6(d) | Design-satisfied |
| 4(e) server-side cumulative totals across all assets/wallets, including in-flight | §2.6(e) | Design-satisfied; the exact HD-KYC-1 computation rule (in-flight handling, reversal netting) remains a human decision, disclosed as such |
| 4(f) cross-tenant test shape | §2.6(f), §7.5 | Design-satisfied |
| 5 (HIGH, launch-blocking) — denial audit must survive rollback | New §3.6 "Commit discipline, corrected" subsection; §5 ordering bullet (exact placement so a deny never reaches a state-changing statement); §7.2, §7.6 | Design-satisfied |
| 6 (MEDIUM) — payout gate must cover every path to submission | §8 item 3 ("Every path to provider submission is covered"); §7.5 raw-guard test | Design-satisfied |
| 7 (MEDIUM) — staff read API fully specified | §6 (tenant-from-context, 404 on cross-tenant, roles, audited cross-tenant path, keyset pagination, exact response fields) | Design-satisfied |
| 8 (MEDIUM) — players see status only | §6; §8 item 2 | Design-satisfied |
| 9 (LOW) — dormancy observability | §6 (`GET /v1/admin/kyc/enforcement-policies/dormant-jurisdictions`) | Design-satisfied |
| 10 (LOW) — fixture values | §3.7 (unchanged; already endorsed) | Already satisfied, no change needed |

### 14.2 Ledger-finance (§12)

| Condition | Design text | Status |
|---|---|---|
| C1 (blocking) — a denial must commit, never roll back | §3.6 "Commit discipline, corrected"; §8 item 3's exact `RequestWithdrawal`/`LockApprovedForSubmission` shapes (`ErrKYCRequired`, `ErrKYCDeniedCommitted`) | Design-satisfied |
| C2 — payout-time denial posting shape (`DenyForCompliance`, not `Reject`) | §8 item 3, in full (accounts, entries, idempotency key, no `reason_code` on the ledger row, audit action, no `withdrawal_approvals` row, `player_cash` release default, recommended partial-unique-index follow-up) | Design-satisfied |
| C3 — the structural exemption must not let a known `failed` status pay out; scoped per wallet was too loose | §3.2 point 1 (rewritten); §1 row #3; §7.2; HD-KYC-5 | Design-satisfied |
| C4 — TOCTOU: accept it, bound it explicitly | §5 "Concurrency and TOCTOU" bullet, adopting conditions (a)/(b)/(c) verbatim | Design-satisfied |
| C5 — ADR 0095 interplay | §5 "Coordination with ADR 0095" bullet; §8 item 9 (dependency request on ADR 0095's own owner); §8 item 3 (`DenyForCompliance` legal only from `approved`) | Design-satisfied as a binding constraint on ADR 0095; ADR 0095's own text is not owned by this ADR and is not edited here (CLAUDE.md's no-unilateral-redesign rule) |
| C6 — threshold arithmetic | §2.6(e) (NUMERIC/big.Int, same-asset-only, settled-postings-only) | Design-satisfied |
| C7 — required tests | §7.2, §7.4, §7.6 (each C7 test item individually present) | Design-satisfied |
| 10.3 Veto check | No veto applies (ledger-finance's own conclusion); C1 was the one correctness defect, fixed above | Closed |

### 14.3 Casino (§11)

| Condition | Design text | Status |
|---|---|---|
| 1 — state the KYC-required decline reuses `OutcomeDeclined`/`DeclineReason`, no new outcome variant | §3.5 (new bullet); §8 item 4 (unchanged, already referenced this) | Design-satisfied |
| 2 — explicit test-plan row for KYC-decline retry semantics | §7.2 (new row, casino review condition 2) | Design-satisfied |
| 3 — refresh stale line-number citations and baseline commit | Baseline note added at the top of this document, disclosing the staleness and what was independently re-verified, deferring the exact line-number refresh to PRH-I3 implementation | Acknowledged and bounded; **not fully closed** (the actual line numbers in §1 are not renumbered in this pass — see §14.5) |

### 14.4 QA (§10)

| Gap | Design text | Status |
|---|---|---|
| 1 — per-outcome coverage named explicitly at each enforcement point | §7.2 (deposit/withdrawal-hold/payout-dispatch rows each now name `pending`/`failed`/`unavailable` explicitly) | Design-satisfied |
| 2 — migration up/down reversibility test | §7.2 | Design-satisfied |
| 3 — OpenAPI contract test for the new admin route | §7.2 | Design-satisfied |
| 4 — idempotency test for the decision write itself | §7.2 (retried-deposit-intent row; states the expected behavior must be decided and tested, does not itself pick append-vs-dedupe) | Design-satisfied as a named, testable requirement; the append-vs-dedupe choice itself is left to PRH-I3, disclosed as such |
| 5 — mutation-kill / manual branch-coverage requirement | §7.1 (mutation pass over `EvaluateEnforcement`; manual CHECK-constraint checklist) | Design-satisfied |
| 6 — CI time budget | §7.7 | Design-satisfied as a stated estimate + escalation path; the real measured number is necessarily an Implementation gate |
| 7 — measurable performance pass criterion | §7.7 (+2 ms p95 bound on the bet-placement hot path) | Design-satisfied |

### 14.5 Known residual gaps (disclosed, not hidden)

- Casino condition 3's exact line-number refresh in §1's citations is
  deferred to PRH-I3 implementation, per the baseline note — this paper
  does not re-derive line numbers against a commit it did not re-read in
  full.
- Security condition 3's four-eyes mechanism for relaxing a policy is
  named as a binding requirement (§8 item 10) but its concrete shape
  (pending-approval row vs. two-step API vs. reuse of an existing
  primitive) is PRH-I3 implementation work, under `security` review — not
  fully designed here, and stated as such rather than claimed complete.
- HD-KYC-1's exact in-flight-deposit/reversal-netting computation rule
  (§2.6(e)) remains a human decision; the mechanism is fully specified,
  the value and its edge-case content are not.
- ADR 0095 itself is authored and owned by `architect`; §5/§8 item 9 state
  the binding constraint this ADR requires of it, but this document does
  not, and may not, edit ADR 0095's own text.

---

## 15. Implementation record (PRH-I3, `identity-compliance`, 2026-09-27)

Label per CLAUDE.md's seven-value vocabulary: **IMPLEMENTED** for the
mechanism scoped to PRH-I3 (task-registry.md's KYC-ENFORCE-1/PRH-I3 row) —
`internal/kyc.EvaluateEnforcement`, migration 0100, the withdrawal-request
gate, `withdrawal.DenyForCompliance`, the casino/sportsbook play gates, the
staff read API. **NOT IMPLEMENTED / PROVIDER DEPENDENT** for what this
task explicitly does not own: the deposit gate (`payments.InitiateDeposit`)
and the payout-dispatch call site (`T1p`), both left for PRH-I1 to wire by
calling this package's exported `kyc.EvaluateEnforcement`/
`kyc.RecordDecision`. **PARTIALLY IMPLEMENTED** for `edd_amount`/
`registration_tier` (schema and admin write path exist; the evaluator does
not yet consult them — see N3 below) and for the dormancy report (query
exists, `internal/kyc.ListDormantJurisdictionTriggers`; no HTTP route
added, per CLAUDE.md's "no uncontrolled scope expansion" — nothing consumes
it yet). No regulatory approval is claimed; no real KYC vendor is
integrated (unchanged from this package's existing `MOCK` provider scope).

### 15.1 Post-orchestrator-relay re-verification findings — how each was closed

Two additional review passes (`ledger-finance` `rv-0096-ledger-reverify.md`
and `security` `rv-0096-security-reverify.md`, both under
`docs/plans/payment-readiness/`, 2026-09-27) were relayed mid-implementation
and are closed as follows — in the **implementation**, not only in this
ADR's text, since code already existed by the time they landed:

| Finding | Resolution |
|---|---|
| ledger-finance N1 (payout deny is a carve-out, not "zero ledger effect") | `withdrawal.DenyForCompliance`'s doc comment states this explicitly; it commits the reversal + `approved→rejected` transition + decision + audit together, in one transaction, exactly like `Reject`/`Fail`. §3.6/§7.6's general "deny = zero domain effect" rule is understood as covering `withdrawal_hold`/`deposit`/`play` only, never `withdrawal_payout` |
| ledger-finance N2 (pre-insert lookup is new code) | `withdrawal.RequestWithdrawal` now does a read-only `getByTenantPlayerIdempotencyKey` lookup first, applies the wallet/asset/amount mismatch check on a hit, evaluates KYC only on a miss, and keeps the post-`IdempotentInsert` conflict branch unchanged for the genuine-race case — both paths tested (`TestRequestWithdrawal_IsIdempotentOnRetry`, `TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds`) |
| ledger-finance N3 (`release_ledger_transaction_id` must be set in the same UPDATE) | Done — `DenyForCompliance`'s conditional `UPDATE ... SET state = 'rejected', release_ledger_transaction_id = $2 ... WHERE state = 'approved'` mirrors `Reject`/`Fail`/`Cancel` exactly |
| ledger-finance N5 / security N1 (read key: per-Person vs per-PlayerAccount) | Resolved per security's own explicit prescription (the primary source, not the orchestrator's relayed paraphrase, which this implementation follows where the two differed): the read key is `(tenant_id, brand_id, player_account_id)` for every operation — never `person_id` as a primary key. `PersonID` is used **only** as an additional, deny-only overlay on the withdrawal structural rule (`crossAccountRejectedOverlay`): a rejection on a DIFFERENT `PlayerAccount` of the same `Person`, in the same tenant, denies a withdrawal from THIS account even when this account's own latest row is approved. Tested (`TestEvaluateEnforcement_WithdrawalCrossAccountRejectedOverlayDenies`, `TestEvaluateEnforcement_LatestRowWins*`) |
| security N2 / C4(e) (cumulative_deposit is one-asset-per-jurisdiction; cross-asset structuring) | Migration 0100's unique index now includes `asset_code`; `evaluateDepositThreshold` fetches every active row for the trigger and asset, and returns `unavailable` (fail-closed) if any active row exists for the jurisdiction but none matches the requested asset — never silently `not_required`. True cross-asset aggregation (an FX/`ConversionOperation` basis) is out of scope, registered as **KYC-FX-AGG-1** (a new human-decision-gated follow-up; needs a rate source this platform does not have) |
| security N3 (activatable-but-unevaluated trigger types) | `edd_amount`/`registration_tier` may be authored as `draft` (documenting an authored-but-not-wired policy) but migration 0100's lifecycle trigger refuses `draft→active` for any `trigger_type` other than `cumulative_deposit`/`play` — tested (`TestMigration0100_ActivatingUnwiredTriggerTypeRefused`) |
| security C3 (four-eyes, first cut) | Implemented as a DB-enforced two-distinct-principals control: INSERT is refused unless `status = 'draft'` (activating directly as `active` is impossible), and the lifecycle trigger refuses any `draft→active`/`active→withdrawn`/`draft→withdrawn` transition where the acting `app.platform_admin_principal_id` equals the row's own `created_by_actor_id`. This is a first-cut model (creator ≠ later transitioner), not a full pending-approval workflow with a named second approver role — disclosed as such. Supersession atomicity (withdraw-old + activate-new in one transaction) is the caller's own responsibility (`internal/kyc/enforcement_admin.go`'s `WithdrawEnforcementPolicy`/`ActivateEnforcementPolicy` are separate calls); a combined atomic helper is not built here and is a named gap. Tested (`TestMigration0100_PolicyLifecycleTransitions`) |
| security N4/N6/N7 (dormancy per-asset, four-eyes text tidy, effective_from filter, actor_type CHECK) | N4 (dormancy per-asset): not done — `ListDormantJurisdictionTriggers` reports per-jurisdiction/trigger_type only, not per-asset; registered as a follow-up alongside KYC-FX-AGG-1. N6/N7 text tidy-ups: not separately tracked; superseded by this §15's own text |
| ledger-finance N4 (sweeper deny routes to ADR 0095 M3) / N6 (raw-guard retarget) | `DenyForCompliance`'s doc comment now states the ADR 0095 M3 routing explicitly. The raw-guard test itself is **not implemented** here: ADR 0095's `ClaimForDispatch`/T1p/T2/T12 primitives do not exist in this worktree (PRH-I1's own scope) — a guard written against `MarkSubmitted` today would guard the wrong edge per ledger-finance's own finding, so writing one now would be worse than not writing one. Deferred to PRH-I1, noted as a dependency |
| coordinator relay re: ADR 0095 revision 3 items (T2 re-claim → M3, escalation write allowance) | Documented in `DenyForCompliance`'s doc comment as a binding requirement on the future caller; no code changes here since `ClaimForDispatch`/T2/T12/the escalation write path do not exist in this worktree. This is `identity-compliance` deferring to PRH-I1/`architect` per CLAUDE.md's "no specialist redesigns shared architecture unilaterally" rule, not a decision made here |

### 15.2 Deviations from the ADR §2/§3 sketch, disclosed

- `EnforcementParams.LicensingJurisdictionID` (§2.2) is **not** a
  caller-supplied field in the implementation — `EvaluateEnforcement`
  resolves it internally from `TenantID` (the same `tenants.licence_id ->
  licences.jurisdiction_id` join `jurisdiction.ResolveEvaluationPolicy`
  uses), and only for `deposit`/`play` (withdrawal needs no jurisdiction
  resolution at all, per §5). This is a narrower trust surface than the
  sketch, not a weaker one: no call site can supply a wrong or stale
  jurisdiction id. A tenant with **no licence bound** resolves to
  `not_required` for deposit/play (not `unavailable`) — a genuine query
  failure remains `unavailable`; "no licence" is a valid non-error state
  distinguished from it explicitly in code (`resolveLicensingJurisdictionID`'s
  three-way return).
- `sumSettledDeposits` (§2.6(e)) sums per `player_account_id`, not
  aggregated across a Person's multiple accounts, and does not include
  in-flight (pending) deposits. Both are disclosed gaps tied to HD-KYC-1's
  still-undecided content, not implementation shortcuts taken silently.

### 15.3 Test/mutation/CI evidence

- Unit/integration tests: `internal/kyc/enforcement_integration_test.go`
  (outcome mapping exhaustive over verification state, latest-row
  ordering both directions, expiry independent of stored status,
  cross-tenant isolation, the withdrawal no-history-exemption case, the
  cross-account deny-only overlay, deposit dormancy) and
  `internal/kyc/migration_0100_integration_test.go` (up/down/up
  round-trip, FORCE RLS on both tables, tenant-scoped/platform-service-
  scoped write rejection, genuine-platform-admin write + provenance,
  decisions-table cross-tenant isolation and append-only, lifecycle
  transitions including four-eyes and the unwired-trigger-type refusal).
  Withdrawal/casino/sportsbook integration suites updated in place
  (`PersonID` threaded through every fixture; an approved verification
  seeded where a test's own scenario is unrelated to KYC) — full existing
  regression suites for `internal/withdrawal`, `internal/casino`,
  `internal/sportsbook`, `internal/httpserver`, `internal/wallet`,
  `internal/bonus`, `internal/jurisdiction`, `internal/operatingmarket`
  pass unmodified in substance (only fixture wiring changed).
- Mutation-kill evidence (manual, not an automated mutation-testing tool —
  none is wired into this repo for Go source, matching this project's own
  precedent for constructs "no mutation tool applies to"):
  `docs/plans/payment-readiness/evidence/prh-i3-mutation-kill.txt`. Three
  real mutations applied and reverted against `EvaluateEnforcement`'s
  guarded branches (the `Allowed` polarity, the expiry guard, the
  latest-row ordering) — all three killed by name; every other branch
  reasoned through manually, disclosed as such, not claimed as
  mutation-tested.
- Chain-tip pin tests: `internal/bonus/wave3_phase2_migrations_integration_test.go`,
  `internal/jurisdiction/migration_0077_integration_test.go`, and
  `internal/operatingmarket/migration_0076_integration_test.go` +
  `qa_migration_rls_survives_failed_rollback_test.go` previously
  hard-coded their chain tip at migration 99. Converted to a derived
  pattern (`wave3MigrationsAbove`/`migration0075MigrationsAbove`/
  `migration0076MigrationsAbove`: scan the real `migrations/` directory for
  any version above a fixed base, descending) so migration 0100 — and any
  future migration — does not require touching these tests again.
- CI timing measured directly (not estimated): `internal/kyc`,
  `internal/withdrawal`, `internal/casino`, `internal/sportsbook`
  integration lanes together run in **~39s wall / ~75s summed** on this
  environment (`go test -tags=integration` across all four packages) —
  within this project's existing per-package lane budgets; no new
  `TIMING_LANE_TESTS` isolation is needed at this volume.

### 15.4 Dependency requests recorded (per `docs/governance/integration-protocol.md`)

- `payments` (PRH-I1): confirm the `DepositIntent` declined-reason string
  convention and call `kyc.EvaluateEnforcement`/`kyc.RecordDecision` from
  `InitiateDeposit`, per §8 item 2 and §2.4's ordering (RG → KYC).
- `payments`/`architect` (PRH-I1, ADR 0095): call
  `kyc.EvaluateEnforcement` and `withdrawal.DenyForCompliance` from
  Phase A / `ClaimForDispatch` (T1p), and from the sweeper's T2/T12
  re-claim paths per ADR 0095's own M3 routing on a KYC deny, per §5's
  ADR 0095 coordination note and §12.2 C5. Retarget the raw-guard test
  (security condition 6 / ledger-finance N6) from `MarkSubmitted` to
  `ClaimForDispatch`/T2/T12 once those exist.
- `casino`/`sportsbook`: minimal call-site edits already made
  (`internal/casino/orchestrator.go`'s `postBet`,
  `internal/sportsbook/orchestrator.go`'s bet-placement path) — requesting
  review of the exact insertion point and the `RejectionKYCDenied`/
  `OutcomeDeclined` decline-surfacing convention added.
- `withdrawal` call sites: minimal edits made directly (no distinct
  package owner exists today, per §8 item 3's own note) — requesting
  `ledger-finance` review of `DenyForCompliance` against §12.2 C1/C2/C7 in
  full, and `security` review of the admin route/RLS/raw-guard gap, before
  this is marked complete without qualification.

---

## 16. Fix round 2 (2026-09-27) — code review, security, ledger-finance re-review responses

Three independent reviews of the §15 implementation landed after it was
first marked complete: `code-reviewer`'s `rv-prh-i3-code-review.md`
(verdict NOT READY), `security`'s `rv-prh-i3-security.md` (APPROVE WITH
CONDITIONS), and `ledger-finance`'s `rv-prh-i3-ledger.md` (SIGN-OFF WITH
CONDITIONS) — all under `docs/plans/payment-readiness/`. **Labels
corrected: PRH-I3 and `KYC-ENFORCE-1`'s ADR-0096-implemented portion are
`PARTIALLY IMPLEMENTED`, not `IMPLEMENTED`** (§16.4) until the items still
open below land and every mandatory reviewer signs off clean.

### 16.1 Closed this round

- **Call-site negative tests (code review T1/T2, security F2).**
  `internal/withdrawal/kyc_gate_integration_test.go` (new):
  `RequestWithdrawal` denies for pending/rejected/no-row, each with zero
  `withdrawal_requests` rows and zero ledger postings; the fresh-
  transaction decision/audit commit path (security condition 5) is
  proven directly; replay-after-revocation is not re-gated; the same-key
  concurrent race is proven. `internal/casino/kyc_play_integration_test.go`
  and `internal/sportsbook/kyc_play_integration_test.go` (new): the play
  gate is exercised at the REAL call site (`postBet`/`PlaceBet`) against a
  genuinely licensed tenant with an active `play` policy authored and
  activated through the real four-eyes admin path
  (`CreateEnforcementPolicy`/`ActivateEnforcementPolicy`, two distinct
  principals) - both deny (no verification) and allow (approved) are
  proven, closing the "zero tests with an active play policy on a
  licensed tenant" gap. `internal/kyc/enforcement_integration_test.go`
  gained the deposit-threshold suite (security F2): licensed-dormant,
  below-threshold, at-threshold boundary (both denied-then-approved),
  asset-not-covered → `unavailable`, and the play operation-scoping test.
  **MX1 and MX2 (the code review's two surviving mutations) were
  re-applied and re-verified KILLED** - see
  `docs/plans/payment-readiness/evidence/prh-i3-mutation-kill.txt`'s
  addendum.
- **`DenyForCompliance` C7 tests (code review R3, ledger-finance
  LF-I3-1).** New tests prove: the exact posting shape (accounts, entries,
  `TransactionType`, `ReversesTransactionID`, `CorrelationID`); the
  `:kyc_denied` idempotency key is distinct from `:rejected`/`:failed`;
  `SUM(DEBITS)==SUM(CREDITS)` after a denial; projection equals a fresh
  rebuild from `ledger_entries`; exactly-once release under 5 concurrent
  callers, **repeated 20 times** under `-race` (ledger-finance asked for
  ≥50 - 20 is a disclosed time-budget compromise, not the full ask - see
  `docs/plans/payment-readiness/evidence/prh-i3-race-integration.txt`).
  `DenyForCompliance` is therefore now labelled **IMPLEMENTED** for the
  function itself (posting/idempotency/concurrency all tested), still
  **NOT WIRED** to any live payout call site (PRH-I1/ADR 0095's own
  scope, unchanged from §15).
- **B1 (withdrawal handler error mapping).** `Decision.Outcome ==
  OutcomeUnavailable` now maps to `503` (retryable), distinct from a real
  pending/failed denial's `409`; `ErrKYCUnavailable` (a caller bug) maps
  to a non-retryable `500` with no outcome-shaped message.
- **B2 (migration 0100 down guard).** The down migration now refuses,
  loudly, while EITHER `kyc_enforcement_decisions` or
  `kyc_enforcement_policies` holds rows, mirroring migrations 0048/0052/
  0075. Implementing this surfaced a REAL, independent bug the review
  didn't name: `kyc_enforcement_decisions` carries tenant-scoped `FORCE`
  RLS, so the naive `EXISTS` guard silently saw zero rows unconditionally
  (FORCE applies to the table owner too, and the migration role has
  `NOBYPASSRLS` per `deploy/init-app-role.sql`) - fixed by an in-transaction
  `ALTER TABLE ... DISABLE ROW LEVEL SECURITY` before the check, which
  rolls back with the rest of the transaction if the guard fires. Tested
  (`TestMigration0100_DownRefusesWhileDecisionsHoldRows`), which caught
  the bug before it shipped.
- **B3 (staff read API cursor).** The handler now accepts `next_before`
  in the EXACT shape it emits it (previously it only read
  `before_decided_at`/`before_id` separately, which no client could ever
  populate from the response), and rejects a malformed cursor or `limit`
  with `400` instead of silently returning page 1 forever. Tested end to
  end via `internal/httpserver/kyc_enforcement_handlers_integration_test.go`
  (new): compliance-role success, tenant_admin `403`, cross-tenant and
  nonexistent-player identical `404`, a genuine 3-page round trip over 5
  rows with no id repeated or skipped, and a malformed-cursor `400` -
  closing security F1's staff-API test requirement in the same pass.
- **B4 (dormancy report omits `play`).**
  `ListDormantJurisdictionTriggers` now reports `play` per surface
  (`casino_play`/`sportsbook_play` each independently), not only
  `cumulative_deposit`.
- **B5 (doc comments contradict the read key).**
  `kyc.EnforcementParams.PersonID` and `withdrawal.RequestParams.PersonID`
  now describe the ACTUAL implemented read key (per-PlayerAccount primary,
  Person-scoped deny-only overlay) - the stale "per Person" prose PRH-I1
  would have read while wiring the deposit/payout gates is gone.
- **B6 (`DenyForCompliance` trusts its input).** Now rejects (`ErrInvalidInput`)
  a decision with `Allowed == true` or `kycParams.Operation !=
  EnforcementWithdrawalPayout` before touching the ledger or the request
  row.
- **F7 (security: players must not see `unavailable` or an internal KYC
  code).** `internal/httpserver/sportsbook_handlers.go`'s
  `toPlaceBetRejectionResponse` now collapses every `RejectionKYCDenied`
  result to one static, closed-enum player response
  (`"verification_required"` / "this bet requires identity verification"),
  never `kyc.EnforcementDecision.Code` verbatim. The withdrawal handler's
  B1 fix separately ensures `unavailable` is never distinguishable from an
  ordinary retryable failure on that surface.
- **LF-I3-2 (state-machine doc).**
  `docs/architecture/withdrawal-state-machine.md` gained the
  `approved → rejected` (`DenyForCompliance`) edge, both in the diagram and
  in prose distinguishing it from the human `Reject` edge.
- **Chain-tip pin test nit (T5).** Not unified into one shared helper
  (disclosed as a follow-up, not done this round - the triplication is a
  readability nit, not a correctness gap per the review's own verdict).

### 16.2 Still open (disclosed, not silently dropped)

- **LF-I3-3 (request-deny commit shape).** Ledger-finance's preferred fix
  - making the handler commit the decision in the SAME transaction as a
  typed result, rather than rolling back and writing it in a fresh
  transaction - is **not implemented this round**. The accepted
  alternative the condition itself names ("or disclose it... for a
  security ruling") is taken instead: the roll-back-then-fresh-transaction
  pattern (mirroring `internal/casino`'s own CAS-RECON-1 precedent) is
  retained, tested (`TestRequestWithdrawal_DenialCommitsDecisionAndAudit`
  proves the decision/audit DO survive), and the residual risk is exactly
  the narrow window between the rollback and the fresh transaction's own
  commit, during which a decision could theoretically be lost if the
  process crashes - a gap identical in shape to B7 below. A future
  `security` ruling should confirm this is acceptable or require the
  same-transaction redesign.
- **LF-I3-4 (partial unique index backstop).** Deferred, as ledger-finance's
  own condition text allows: migration 0100 is already applied and
  migrations 0101-0103 are allocated to other in-flight work, so this
  backstop cannot land without editing an already-shipped migration.
  Registered as its own follow-up, owned by `ledger-finance`.
- **LF-I3-5 (casino/sportsbook decision loss on DB error).** Recorded here,
  not fixed: if `kyc.EvaluateEnforcement` or `kyc.RecordDecision` returns a
  genuine error (not a `not_required`/`unavailable` outcome) inside
  `postBet`/`PlaceBet`, the whole bet-placement transaction rolls back -
  correctly fail-closed for the bet itself (no stake is posted), but,
  unlike withdrawal's own separately-committed-decision pattern, NO
  decision/audit row survives for that attempt. This is the same class of
  gap security condition 5 closed for withdrawal, not yet closed for play.
- **B7 (same-key concurrent retry can report a denial for a hold that
  exists).** Not fixed - the narrow TOCTOU window code review named
  (KYC state changes between two concurrent same-key requests, one of
  which already placed the hold) remains, documented in
  `RequestWithdrawal`'s own doc comment; closing it would require
  re-checking for an existing request specifically on the deny path,
  which is deferred as a follow-up rather than built under this round's
  time budget.
- **Security F3 (four-eyes on WITHDRAWING an active policy).** Not
  additionally tested this round beyond what §15.1's C3 already covers
  (the lifecycle trigger's creator≠transitioner check applies uniformly to
  every transition, including `active→withdrawn`) -
  `TestMigration0100_PolicyLifecycleTransitions` already exercises
  `active→withdrawn` by a different principal, but no NEW test specifically
  targets "smaller correct design" alternatives security raised (a
  dedicated request/approve pair for withdrawal specifically). Disclosed
  as not separately re-verified against F3's exact wording.
- **Security F4 (richer policy-write audit records) and F5 (derive/assert
  PersonID server-side).** Not implemented this round. F5 is partially
  true already: every enforcement point's caller resolves `PersonID` from
  the authenticated player's own account server-side (never a request
  body), but `EvaluateEnforcement` itself does not independently assert
  the value it is given matches `PlayerAccountID`'s own person - it trusts
  the caller. Registered as a follow-up.
- **Ledger-finance's ≥50-repeat ask (LF-I3-1).** Delivered at 20 repeats,
  disclosed as a time-budget compromise, not the full ask (§16.1).
- **Casino/sportsbook chain-tip helper unification and the deposit call
  site's `Amount <= 0` rejection requirement.** Not built here; the latter
  is explicitly PRH-I1's own scope (recorded per the orchestrator's own
  instruction to keep it recorded for that task).

### 16.3 Verification (fix round 2)

`gofmt`, `go vet` (plain and `-tags=integration`),
`golangci-lint run ./...` (0 issues), `go test -race ./...` (all unit
suites pass), `go test -tags=integration ./...` (all packages pass,
including every package this round touched:
`internal/kyc`, `internal/withdrawal`, `internal/casino`,
`internal/sportsbook`, `internal/httpserver`), and a targeted
`-race -tags=integration` run of the new concurrency-sensitive tests (see
`docs/plans/payment-readiness/evidence/prh-i3-race-integration.txt`).

### 16.4 Labels (corrected)

- **ADR 0096 header:** `ACCEPTED — PARTIALLY IMPLEMENTED`, pending
  security/ledger-finance/code review sign-off on §16.2's open items.
- **`DenyForCompliance`:** IMPLEMENTED and tested (C7 closed); NOT WIRED
  to a live call site (PRH-I1/ADR 0095 scope).
- **Withdrawal-request and play gates:** IMPLEMENTED, now WITH call-site
  tests (T1/T2 closed).
- **Deposit gate, payout-dispatch call site:** unchanged from §15 -
  NOT IMPLEMENTED here, PRH-I1's scope.

## 17. Fix round 3 (2026-09-27) — F3, LF-I3-3, and the 50-repeat race ask

Orchestrator instruction for this round named exactly three
non-deferrable items (merge origin first; origin was at e927ed4).
F4/F5 were welcome if small; not attempted this round (still open, see
§16.2 - untouched).

### 17.1 LF-I3-3 — CLOSED (preferred design implemented)

§16.2 disclosed the roll-back-then-fresh-transaction pattern as the
accepted alternative. This round replaces it with ledger-finance's
actually-preferred design:

- `withdrawal.RequestWithdrawal` (`internal/withdrawal/withdrawal.go`) now
  calls `kyc.RecordDecision` to write the decision + audit rows **in the
  same transaction** as the withdrawal attempt itself, then returns the
  typed `*KYCDeniedError` — it no longer relies on a caller rolling back
  and reopening a second transaction to persist anything.
- Every caller that opens the transaction (`db.Pool.WithTenant`, which
  rolls back on any non-nil returned error) now catches
  `*KYCDeniedError` via `errors.As` **inside** the closure and returns
  `nil` so the transaction — decision, audit, and all — commits exactly
  once, then re-raises the captured error for post-commit response
  mapping. Fixed at all three call sites: the HTTP handler
  (`internal/httpserver/withdrawal_handlers.go`), and the shared test
  helper used by the whole existing withdrawal test suite
  (`internal/withdrawal/withdrawal_integration_test.go`'s
  `requestWithdrawal`), which had to be updated in lockstep — otherwise
  every pre-existing test using that helper would have silently started
  rolling back the very rows `RequestWithdrawal` now writes before
  returning.
- The old "fresh transaction" `kyc.RecordDecision` call in the HTTP
  handler is deleted entirely; the handler's B1 status-code mapping
  (503 for `unavailable`, 409 otherwise) is unchanged.
- Tests: `TestRequestWithdrawal_DenialCommitsDecisionAndAudit`
  (`internal/withdrawal/kyc_gate_integration_test.go`) rewritten to
  assert the same-transaction contract directly (no manual
  post-rollback `RecordDecision` call in the test itself anymore). New:
  `TestRequestWithdrawalHandler_KYCDenyCommitsExactlyOnceAndNothingElse`
  (`internal/httpserver/kyc_enforcement_handlers_integration_test.go`)
  proves the same property end-to-end through the real HTTP handler —
  exactly 1 new `kyc_enforcement_decisions` row, exactly 1 new
  `audit_log` row, 0 `withdrawal_requests` rows, 0
  `withdrawal_requested` ledger postings, for a single denied request.

Residual: B7's narrow same-key-concurrent-retry TOCTOU window (§16.2) is
unaffected by this change and remains open, disclosed as before.

**Label: LF-I3-3 IMPLEMENTED** (the preferred design, not the
alternative §16.2 described).

### 17.2 Ledger-finance's ≥50-repeat concurrency ask (LF-I3-1) — CLOSED

`TestDenyForCompliance_ExactlyOnceRelease_Concurrent`
(`internal/withdrawal/kyc_gate_integration_test.go`) bumped from 20 to
50 repetitions, each iteration using a fresh request/fixture so no run
can be masked by a previous iteration's state. Run:

```
go test -race -tags=integration -count=1 \
  ./internal/withdrawal/... ./internal/kyc/... ./internal/casino/... ./internal/sportsbook/... \
  -run 'TestDenyForCompliance_ExactlyOnceRelease_Concurrent|TestRequestWithdrawal_SameKeyConcurrentRequestsRaceTheGate|TestReceiveCallback_.*KYCPlayPolicy|TestPlaceBet_.*KYCPlayPolicy'
```

Result: PASS, all 50 iterations, no data races detected. Recorded in
`docs/plans/payment-readiness/evidence/prh-i3-race-integration.txt` and
`prh-i3-race-integration-output.txt` (updated this round, superseding
round 2's 20-repeat numbers).

**Label: LF-I3-1 IMPLEMENTED (closed in full — the ">=50" ask is met).**

### 17.3 Security F3 (four-eyes on withdrawing an ACTIVE policy) — PARTIAL, schema change needed for full closure

**Explicit migration constraint honored:** migration 0100 is applied and
its `up.sql` is checksummed — it was NOT edited. No schema change was
made this round. Per the orchestrator's own instruction ("If a schema
change is needed, tell me before writing it and I'll allocate a number;
prefer a design that needs no schema change... if not, report"), this
section is that report.

**Why no schema-change-free design can fully close F3.** F3 asks for
one of two things: (a) a DB-enforced two-step request/approve workflow
for withdrawing an active policy (approver ≠ requester, both derived
from session GUCs, not app-written columns), or (b) a trigger that
refuses `active → withdrawn` except as an atomic supersede by an
approved successor. Both were evaluated against migration 0100's actual
schema as applied:

- Migration 0100's lifecycle trigger already enforces "the transitioning
  principal differs from the row's own `created_by_actor_id`" for every
  transition, including `active → withdrawn` (§15.1 C3; exercised by
  `TestMigration0100_PolicyLifecycleTransitions`). That is real
  four-eyes on the withdrawal *action itself*, but it is not a
  request/approve *workflow* — a single transaction, one call, can still
  withdraw an active policy and leave the key with no active row at all.
- A trigger-level "refuse `active → withdrawn` unless a successor is
  activated atomically" (design (b)) needs the trigger to know, at the
  moment it fires on the withdrawal `UPDATE`, that a specific
  replacement row already exists and is `active` for the same key
  (`licensing_jurisdiction_id` + `trigger_type` + discriminating
  columns) — but nothing in the current schema links a withdrawn row to
  its replacement. Without a linkage column (e.g. a nullable
  `superseded_by_policy_id uuid REFERENCES kyc_enforcement_policies(id)`
  set in the same statement/transaction), the trigger has no way to
  distinguish "withdrawn because a successor was just activated" from
  "withdrawn and now nothing is enforced" — it would have to either
  always refuse standalone withdrawal (breaking every legitimate
  emergency-withdraw-with-no-immediate-replacement case, e.g. voluntarily
  turning a control off pending legal review) or never refuse it
  (achieving nothing).
- A request/approve workflow (design (a)) needs its own persisted
  "requested, not yet approved" state — either a new status value
  inserted into `kyc_enforcement_policies.status`'s CHECK constraint (a
  DDL change to that constraint, which counts as editing 0100's schema
  even done in a later migration against the same column) or an entirely
  new table (e.g. `kyc_enforcement_policy_withdrawal_requests`) to hold
  the pending request row, its requester, and a later approver GUC
  comparison. Either way this is a genuine schema addition, not
  something expressible in 0100's existing tables/triggers.
- `kyc_enforcement_policies_one_active`, the uniqueness that guarantees
  at most one active row per key, is a plain `CREATE UNIQUE INDEX`, not
  a `DEFERRABLE` `UNIQUE CONSTRAINT` — so even a same-transaction
  supersede done purely in application code (see below) can never
  produce a moment where two rows are simultaneously active for the
  same key; the old row's `UPDATE ... SET status = 'withdrawn'` and the
  new row's `INSERT ... status = 'active'` must be ordered
  withdraw-then-activate (which the sanctioned helper below does) or the
  unique index itself would reject the insert. This is a genuine
  strength of 0100's existing design, not a gap - but it is enforced only
  as an ordering constraint, not as evidence that a "successor" was ever
  authored, so it does not by itself close F3.

**What was actually shipped this round: `SupersedeEnforcementPolicy`**
(`internal/kyc/enforcement_admin.go`) — a Go-level, no-schema-change
partial mitigation. It withdraws an existing policy and authors +
activates its replacement **in one database transaction**, changing the
acting-principal GUC between each of the three steps
(`WithdrawnBy` → `CreatedBy` → `ActivatedBy`, three distinct required
principals) so migration 0100's existing four-eyes trigger evaluates
each step against the correct actor. Because all three steps share one
transaction, a caller using this function can never commit a withdrawn
policy with no successor also committed atomically alongside it — if
the replacement's creation or activation is refused (e.g. `CreatedBy ==
ActivatedBy`, violating the existing trigger), the whole transaction,
including the withdrawal step that ran first, rolls back, leaving the
original policy still `active`.

**What this does NOT do, disclosed plainly:** it is a *sanctioned path*,
not a *database-level prohibition*. Nothing in the schema stops a
caller from invoking `WithdrawEnforcementPolicy` directly, standalone,
outside of `SupersedeEnforcementPolicy` — that call still succeeds
today with only the existing creator≠transitioner check applied, exactly
as before this round, and can still leave an active policy withdrawn
with zero replacement. Full closure of F3 requires the schema change
described above (most likely: a nullable
`superseded_by_policy_id` linkage column set only by
`SupersedeEnforcementPolicy`'s own withdrawal step, plus a trigger
condition on `kyc_enforcement_policies` that refuses a **standalone**
`active → withdrawn` transition unless `superseded_by_policy_id` is set
in the very same statement to a row that is simultaneously being
activated) — a genuinely small, additive migration, but a migration
nonetheless, and migration 0100 cannot carry it.

**Proposed minimal migration (for the orchestrator to allocate a number
to, if full closure is wanted):**

```sql
ALTER TABLE kyc_enforcement_policies
  ADD COLUMN superseded_by_policy_id uuid
    REFERENCES kyc_enforcement_policies(id);

-- Extend the existing lifecycle trigger (not a new one) so that, on an
-- active -> withdrawn transition specifically, it additionally requires
-- NEW.superseded_by_policy_id to reference a row that is 'active' as of
-- the same statement (checked via a deferred constraint trigger, or by
-- re-querying inside the existing trigger body after the successor's
-- own INSERT/UPDATE has already run earlier in the same transaction).
```

This was **not written** this round (no schema change without an
allocated number, per the explicit constraint) — it is a proposal only.

**Tests + mutations for what WAS shipped, per the orchestrator's "tests
+ mutations" instruction:**
`TestSupersedeEnforcementPolicy_AtomicWithdrawAndActivateReplacement` and
`TestSupersedeEnforcementPolicy_RollsBackAtomically`
(`internal/kyc/migration_0100_integration_test.go`) prove, respectively,
the happy path (three distinct principals, old row withdrawn, new row
active) and the atomic-rollback property (activation refused because
`CreatedBy == ActivatedBy` → the whole transaction, including the
withdrawal that ran first, rolls back, leaving the old row still
`active` — never stranded withdrawn-with-no-successor). A manual
mutation check was applied: `SupersedeEnforcementPolicy`'s create step
was changed to set the acting principal to `p.ActivatedBy` instead of
`p.CreatedBy` (collapsing the create+activate steps onto one effective
principal even when the caller supplied three genuinely distinct
values) — confirmed this makes
`TestSupersedeEnforcementPolicy_AtomicWithdrawAndActivateReplacement`
fail (migration 0100's own four-eyes trigger correctly refuses the
resulting same-principal activation), then the mutation was reverted and
both tests re-confirmed passing — the mutation is killed.

**Label: Security F3 PARTIALLY IMPLEMENTED.** The Go-level atomic
supersede helper is IMPLEMENTED and tested; full DB-level enforcement
(refusing bare `WithdrawEnforcementPolicy` on an active row with no
atomic successor) is NOT IMPLEMENTED and requires the schema change
proposed above. This is reported, not silently deferred: launch
readiness with any active policy should treat F3 as open until either
the schema change lands or a `security`/`architect` ruling accepts the
Go-level mitigation as sufficient given operational controls (e.g.
restricting who can call `WithdrawEnforcementPolicy` directly at the API
layer — itself not yet built, since no admin-facing withdraw endpoint
exists yet; only the Go functions do).

### 17.4 Incidental fix: migration-count assumption in existing 0100 tests

`TestMigration0100_UpDownUpRoundTrip` and
`TestMigration0100_DownRefusesWhileDecisionsHoldRows` both used to call
`pool.MigrateDown(ctx, dir, 1)` and assert exactly migration 100 was the
one rolled back. Origin's merge (bringing in PRH-I1's migration 0101)
made that assumption false — with 0101 now ahead of 0100 in the chain,
"down 1 step" rolls back 0101, not 0100, silently invalidating both
tests' intent (the second one in particular then reported false
success: rolling back 0101, which has no data guard concerning
`kyc_enforcement_decisions`, "succeeded" where the test expected a
refusal, masking the very guard it exists to prove). Fixed with a new
helper, `stepsThrough100`, that computes the correct step count from the
actual list of applied migrations at test-run time rather than assuming
0100 is the chain tip. Both tests re-verified passing after the fix.
This is a pre-existing-test fix, not new functional scope, and was
necessary to keep this round's own verification pass ("run the full
suite, confirm no regressions") honest.

### 17.5 Verification (fix round 3)

`gofmt`, `go build ./...`, `go vet ./...` and `go vet -tags=integration
./...` — all clean. `go test -tags=integration -count=1
./internal/kyc/... ./internal/withdrawal/... ./internal/httpserver/...`
— all pass (including the migration-count fix in §17.4). Full-repo
`go test -tags=integration -count=1 ./...` run separately; see the
commit's accompanying report for its result if it completed within this
round's time budget. Targeted `-race -tags=integration` run for the
50-repeat concurrency ask: see §17.2 and the evidence files.

### 17.6 Labels (round 3 summary)

- **LF-I3-3:** IMPLEMENTED (preferred design; supersedes §16.2's
  disclosure of the alternative).
- **LF-I3-1 (≥50 repeats):** IMPLEMENTED (closed in full).
- **Security F3:** PARTIALLY IMPLEMENTED — Go-level atomic supersede
  helper shipped and tested; DB-level enforcement needs the schema
  change proposed in §17.3, reported to the orchestrator, not built.
- **F4, F5:** unchanged from §16.2 — NOT IMPLEMENTED, still open.
- **ADR 0096 header:** remains `ACCEPTED — PARTIALLY IMPLEMENTED`
  pending F3's full closure and §16.2's other still-open items.

## 18. Fix round 4 (2026-09-27) — security F3 DB-level closure (migration 0103) and a migration-test robustness fix

Migration number **0103** was allocated by the orchestrator specifically
for §17.3's proposed design (the kill switch, previously expected at
0103, moved to 0104). This round implements it. Migration 0100's
`up.sql` was NOT edited (still checksummed, still applied) - 0103 only
adds a column and does `CREATE OR REPLACE FUNCTION` on 0100's own
trigger function name, exactly as §17.3 proposed.

### 18.1 Security F3 — CLOSED at the database level

`migrations/0103_kyc_enforcement_policy_supersession.{up,down}.sql`:

- Adds a nullable `kyc_enforcement_policies.superseded_by_policy_id UUID
  REFERENCES kyc_enforcement_policies(id)`.
- `CREATE OR REPLACE FUNCTION kyc_enforcement_policies_enforce_lifecycle`
  (same function, same BEFORE-UPDATE triggers 0100 already attached to
  it - no new trigger object bound here) now additionally: (a) excludes
  `superseded_by_policy_id` from the "only status may change" immutability
  check, alongside `status` itself; (b) requires
  `superseded_by_policy_id` to be set if and only if the transition is
  specifically `active -> withdrawn` (draft->withdrawn, a policy that was
  never enforced, carries no successor requirement); (c) forbids it being
  set a second time once set; (d) refuses a policy naming itself.
- A NEW, separate, `DEFERRABLE INITIALLY DEFERRED` **constraint trigger**,
  `kyc_enforcement_policies_check_supersession`, fires AFTER UPDATE and is
  checked at COMMIT time (not statement time): it re-reads the named
  successor's row as of commit and requires it to (i) exist, (ii) be
  `'active'`, (iii) share the withdrawn row's EXACT enforcement key
  (`licensing_jurisdiction_id`, `trigger_type`, `play_operation`,
  `asset_code`). Deferred checking is required, not optional: the
  partial unique index `kyc_enforcement_policies_one_active` forbids two
  simultaneously-'active' rows for the same key, so
  `kyc.SupersedeEnforcementPolicy`'s sanctioned ordering (author the
  replacement as 'draft', THEN withdraw the old row naming it, THEN
  activate the replacement) means the successor is genuinely not yet
  'active' at the moment the withdrawal statement itself runs - only by
  commit.
- `kyc.WithdrawEnforcementPolicy` (`internal/kyc/enforcement_admin.go`)
  is now REFUSED BY THE DATABASE ITSELF when called against an ACTIVE
  row, because it never sets `superseded_by_policy_id`. There is no
  longer any argument, flag, or code path that lets a caller withdraw an
  active policy through this function without the database blocking it -
  this is no longer a documented convention, it is a schema-enforced
  fact. Withdrawing a still-`draft` row (never enforced) is unaffected.
- `kyc.SupersedeEnforcementPolicy`'s step ordering was changed to match
  the schema's actual requirement: create the replacement (draft) FIRST,
  THEN withdraw the original naming the replacement's id as
  `superseded_by_policy_id`, THEN activate the replacement. (Round 3's
  version withdrew first, which cannot satisfy 0103's immediate
  NOT-NULL requirement on `superseded_by_policy_id` at all, since the
  replacement didn't exist yet at that point - round 3 predates 0103 and
  never needed to set that column.) A new unexported helper,
  `withdrawEnforcementPolicyWithSuccessor`, is `SupersedeEnforcementPolicy`'s
  own withdrawal step; the public, standalone `WithdrawEnforcementPolicy`
  deliberately never has access to set that column.
- Approver != requester (four-eyes) for both the withdrawal step and the
  replacement's own creation/activation is UNCHANGED - still 0100's own
  acting-principal-vs-created_by_actor_id check, itself untouched by
  0103, still derived only from the `app.platform_admin_principal_id`
  session GUC, never an application-supplied column.

Tests (`internal/kyc/migration_0103_integration_test.go`, new file):
`TestMigration0103_WithdrawActiveRefusedWithNoSuccessor` (the literal
ask - standalone withdrawal of an active row fails against the DB),
`TestMigration0103_SupersessionRefusedIfSuccessorNeverActivated` (closes
the "point at an arbitrary/draft row" gap the orchestrator named),
`TestMigration0103_SupersessionRefusedIfSuccessorKeyDiffers` (closes the
"point at an arbitrary row" gap for a row that IS active but for the
wrong key), `TestMigration0103_SupersessionRefusedIfSelfReferencing`,
`TestMigration0103_DraftWithdrawalCarriesNoSuccessorRequirement` (the
negative-negative: confirm the never-enforced-draft path is NOT
gated). `TestMigration0100_PolicyLifecycleTransitions`
(`internal/kyc/migration_0100_integration_test.go`) was updated: its
former "active -> withdrawn by a different principal succeeds" assertion
now first proves a BARE active->withdrawn (no successor) is refused,
then proves an active->withdrawn WITH a genuine, same-transaction,
active, key-matching successor succeeds.
`TestSupersedeEnforcementPolicy_AtomicWithdrawAndActivateReplacement` and
`TestSupersedeEnforcementPolicy_RollsBackAtomically`
(`internal/kyc/migration_0100_integration_test.go`, from round 3) needed
no test-body changes - `kyc.SupersedeEnforcementPolicy`'s own
re-ordering keeps them passing unmodified, which is itself a small proof
the public contract didn't change shape, only its internal step order.

Mutation-kill (`docs/plans/payment-readiness/evidence/prh-i3-migration-
0103-mutation-kill.txt`): 3 of 4 candidate mutations against the new
trigger logic (drop the NOT-NULL requirement; drop the "must be active"
check; drop the key-match check) are independently killed by a dedicated
test; the 4th (drop the explicit self-reference check) does not survive
as an actual bypass - a self-referencing row's own status is 'withdrawn'
by the time the deferred trigger re-checks it, so the "must be active"
check (mutation 2, still intact) already refuses it. Disclosed as
redundant defense-in-depth, not silently claimed as an independent kill.

**Label: Security F3 IMPLEMENTED** (full DB-level closure - supersedes
§17.3/§17.6's "PARTIALLY IMPLEMENTED, Go-level mitigation only" status;
`DR-PRHI3-07` in the task registry is now resolved).

### 18.2 Migration-test robustness fix (orchestrator-reported regression, not self-discovered)

The orchestrator reported that two reviewers found
`TestMigration0100_UpDownUpRoundTrip` and
`TestMigration0100_DownRefusesWhileDecisionsHoldRows`
(`internal/kyc/migration_0100_integration_test.go`) still failing on
origin after migration 0102 landed - round 3's own fix (§17.4,
`stepsThrough100`, deriving the MigrateDown step COUNT from the applied
chain) was insufficient: it correctly rolled back the right NUMBER of
migrations to reach 100, but 0102's own down-guard can refuse first
(while 0102's own guarded rows exist), breaking the roll-back sequence
before it ever reaches 100 - and every future migration added above 100
carries the same risk indefinitely, however its own down-migration
happens to be guarded.

Replaced with the robust pattern `internal/ledger` already established
for this exact problem, `stagedMigrations0092`
(`internal/ledger/migration_0092_integration_test.go`): a new
`stagedMigrations0100` helper (`internal/kyc/migration_0100_integration_
test.go`) copies the real migrations directory into a throwaway temp
directory, **holding back every migration file numbered ABOVE 100**,
dynamically, by parsing each filename's version prefix - never a
hand-maintained list of specific later version numbers, so a migration
inserted, reordered, or removed above 100 in the future (101, 102, 103,
104, ...) needs no edit here to keep being handled correctly. Both tests
now migrate up against this staged, truncated-at-100 chain, so 100 is
GUARANTEED to be the actual chain tip in their own scratch database
regardless of what exists in the real repository at HEAD - "MigrateDown
(dir, 1)" then always targets 0100 itself, and can never be pre-empted by
a later migration's own down-guard, whatever that guard's own trigger
condition happens to be. Verified on a fresh, privately created database
migrated all the way to head (0103): both tests pass, and
`go test -tags=integration ./internal/kyc/... ./internal/withdrawal/...
./internal/httpserver/... ./internal/casino/... ./internal/sportsbook/...`
passes in full against that same head-migrated database.

This is the SAME class of "hardcoded assumption about the chain tip"
mistake round 3 already made once (§17.4) and is now fixed with the
actually-robust pattern instead of a second symptom-level patch - future
migrations landing above 0100 (or above 0103) cannot break these two
tests again.

### 18.3 F4/F5

Not attempted this round - the F3 schema work, its test suite, and the
mutation-kill exercise consumed this round's time budget; F4 (richer
policy-write audit records) and F5 (EvaluateEnforcement independently
asserting PersonID against PlayerAccountID's own record, rather than
trusting the caller) both remain open exactly as disclosed in §16.2, not
re-attempted or re-scoped here.

### 18.4 Verification (fix round 4)

`gofmt`, `go build ./...`, `go vet ./...` and `go vet -tags=integration
./...` — all clean. `go run ./cmd/migrate verify` against a freshly
migrated database — all 103 migrations OK, no version gaps, 0100's
checksum unchanged. `go test -tags=integration -count=1
./internal/kyc/... ./internal/withdrawal/... ./internal/httpserver/...
./internal/casino/... ./internal/sportsbook/...` — all pass, against a
private database created fresh and migrated to head (0103) for this
round's verification (the long-lived shared `TEST_DATABASE_URL` instance
this sandbox reuses across many prior sessions currently cannot itself
migrate past 0100 - migration 0101's own pre-flight guard refuses on
pre-existing, unrelated dirty `deposit_intents` rows accumulated by other
past sessions' test runs against that same shared instance; this is a
pre-existing data-hygiene issue in a reused local sandbox database, not a
regression introduced by this round, and is outside `identity-
compliance`'s scope to fix).

### 18.5 Labels (round 4 summary)

- **Security F3:** IMPLEMENTED (full DB-level closure via migration
  0103; `DR-PRHI3-07` resolved).
- **Migration-test chain-tip robustness (0100's own tests):** IMPLEMENTED
  (staged-migrations pattern, immune to future migrations landing above
  0100).
- **F4, F5:** unchanged - NOT IMPLEMENTED, still open, disclosed.
- **LF-I3-3, LF-I3-1:** unchanged from round 3 - IMPLEMENTED.
- **ADR 0096 header:** `ACCEPTED — PARTIALLY IMPLEMENTED` - F3 is now
  closed, but §16.2's other still-open items (LF-I3-4/5, F4/F5, B7) and
  the deposit/payout call-site wiring (PRH-I1's scope) remain, so the
  header does not yet move to a plain `IMPLEMENTED`.

## 19. Fix round 5 (2026-09-27) — RV-PRH-I2 KYC review F1: orphan-row enforcement masking

Scope: `identity-compliance`'s own PRH-I2 (KYC part) code review
(`docs/plans/payment-readiness/rv-prh-i2-kyc-code-review.md`, finding F1) and security review
(`docs/plans/payment-readiness/rv-prh-i2-kyc-security.md`, "Verified (no finding)" §, F1
adversarial-analysis point 2) both flagged the same gap in this ADR's own §2.6 read semantics,
surfaced by ADR 0095's KYC create/submit split (§15.2's orphan row). **Citation correction
(security re-verification, 2026-09-27):** this section originally cited a "§4 item 1 note" in
the security review; no such section exists in that document. The overlay-masking point this
round closes was code review's own F1, not a separate security-review section — corrected here.
`identity-compliance` owns both ADRs and
is the specialist explicitly authorized (by the orchestrating task that requested this round)
to change `internal/kyc/enforcement.go`'s read semantics for this one, narrowly-scoped case,
coordinated with §2.6 and with security's own N1 text (§2.6 already cites N1 for the
cross-account overlay itself; this round extends that same overlay's own "latest row" read,
never replaces it).

### 19.1 The gap

ADR 0095 §15.2's `CreateVerification` commits its phase-A intent row — `status='unverified'`,
`provider_reference IS NULL` — *before* calling the provider, specifically so a phase-B/C
failure (vendor outage, resolver failure, a crash) leaves a harmless, documented orphan rather
than losing the request entirely. §15.2's own text asserted this orphan "has no enforcement
effect" — true only when the orphan happens to be OLDER than the account's real, decided
state. §2.6(a)'s existing "latest row, deterministically ordered" rule takes the single newest
row for `(tenant_id, brand_id, player_account_id)` with no other filter — so a NEWER orphan
becomes the enforcement-visible "latest" row the moment one exists, and `effectiveOutcome`
maps `unverified` to `failed`. Code review's own reproduction: an approved player who starts a
routine re-verification (a renewal, or one triggered by crossing a fresh threshold) while the
vendor happens to be unreachable is denied withdrawals — and play, wherever a jurisdiction
requires `passed` — until a LATER retry actually reaches the vendor, purely because of a
transient failure that, before ADR 0095's split, would have rolled back the whole transaction
and left the approval as the latest row untouched. The identical mechanism applies to
`crossAccountRejectedOverlay`'s own "each OTHER account's own latest row" subquery: an orphan
on account B, newer than B's own decided rejection, would mask that rejection from account A's
withdrawal check.

This is a **fail-closed** defect for the PRIMARY read (`readLatestVerificationByPlayerAccount`)
in the sense that it never lets a real deny through as an allow there — it manufactures a
spurious DENY, never a spurious ALLOW, on that read alone. It is nonetheless a correctness
defect: `internal-only` outages must not be able to interrupt a player's own already-decided,
still-valid compliance state.

**Correction (security re-verification of fix round `492cb20`, `rv-prh-i2-kyc-security.md`,
adversarial analysis point 2, 2026-09-27):** the paragraph above, as originally written, is
INACCURATE for the OTHER function this round also touches, `crossAccountRejectedOverlay`.
Before this round's fix, a newer orphan on a REJECTED account B did mask B's own rejection in
the overlay — that is a genuine deny→allow, not merely a spurious deny. This round's fix (§19.2)
closes that too, but the claim above that the pre-fix defect "never lets a real deny through as
an allow" was wrong for the overlay half specifically; it was accurate only for the primary
read. This correction does not change §19.2's fix itself, which already closed both cases -
it corrects this section's own characterization of what the fix closed.

### 19.2 The fix

§2.6 gains point (g) (full text there, not duplicated here): both `readLatestVerificationByPlayerAccount`
and `crossAccountRejectedOverlay`'s inner "latest row per other account" subquery add the
identical predicate — `NOT (status = 'unverified' AND provider_reference IS NULL)` — excluding
a row that never received ANY decision (no vendor round-trip ever completed for it, no staff
review, no callback) from "latest" selection outright. A row that DID receive a decision, of
whatever kind — a non-terminal `pending` from a completed vendor round-trip included, per the
pre-existing, unchanged "the success path already supersedes an approval with a pending row"
behaviour this ADR's original design always intended — is never excluded. An account whose
every row happens to be such an orphan evaluates identically to an account with no
verification row at all (`found=false`, `OutcomeFailed` either way): the predicate changes
nothing in that all-orphan case, and every genuine deny §2.6(a)-(f) and §3.2 already require is
preserved byte for byte — this is strictly a narrowing of what counts as "latest", never a
loosening of any deny condition.

Implementation: `internal/kyc/enforcement.go`, `orphanRowExclusionSQL` (a documented SQL
predicate constant) is added to `readLatestVerificationByPlayerAccount`'s own `WHERE` clause
and to `crossAccountRejectedOverlay`'s inner subquery. No migration: both predicates read
`kyc_verifications.provider_reference`/`.status`, existing columns since migration 0040/ADR
0095's own §15.2 scope; no schema change is needed.

### 19.3 Tests

`internal/kyc/enforcement_integration_test.go` gained four tests (a new `setOrphanVerification`
helper seeds a row shaped exactly like `CreateVerification`'s own phase-A orphan):
`TestEvaluateEnforcement_OrphanAfterApproval_StillPassed` (an orphan committed AFTER an
approval must never turn `passed` into `failed`), `TestEvaluateEnforcement_
OrphanOnAnotherAccount_DoesNotMaskRejection` (account B's newer orphan must never mask B's own
decided rejection from account A's cross-account overlay check), `TestEvaluateEnforcement_
OrphanOnly_TreatedAsNoVerification` (an account with only orphan rows evaluates exactly like
no verification at all), and `TestEvaluateEnforcement_DecidedOrderingIgnoresInterveningOrphans`
(approved → orphan → rejected: the latest DECIDED row, rejected, still governs despite the
intervening orphan). All four pass; the full pre-existing `TestEvaluateEnforcement_*` suite
(exercised via `setVerification`, which never seeds a `status='unverified'` row and so is
completely unaffected by this predicate) was re-run and remains green, against a private
database migrated to head (0104).

### 19.4 Labels (round 5 summary)

- **F1 (RV-PRH-I2 KYC code review):** IMPLEMENTED — `internal/kyc/enforcement.go`'s
  `readLatestVerificationByPlayerAccount`/`crossAccountRejectedOverlay` now exclude never-decided
  orphan rows from "latest" selection; ADR 0096 §2.6 gains point (g); ADR 0095 §15.2's own
  "no enforcement effect" claim is corrected there (§15.3.3).
- **Cross-account overlay masking (code review F1):** the SAME fix closes the specific "an
  orphan masks a rejection" shape F1 names.
- **CORRECTED (security re-verification, 2026-09-27 — see §20 below):** the sentence
  originally here claimed that a fresh, still-`pending` re-verification on the rejected
  account superseding the rejection "is correct, not a residual gap this round leaves open".
  That claim was WRONG and has been retracted, not merely reworded: security's own N-1 finding
  showed this is exactly how a player neutralises the cross-account overlay with one ordinary
  API call, without the rejection ever being resolved. §20 records N-1's fix, which narrows the
  overlay to each other account's latest FINAL row only — a merely-`pending`/`review_required`
  re-verification no longer supersedes a rejection. See §20 for the corrected rule; this
  paragraph is kept, struck through in substance, to preserve an honest record of the mistake
  rather than silently deleting it.
- **Everything else in this ADR** (§14-§18's own labels) is unchanged by this round.

## 20. Fix round 6 (2026-09-27) — RV-PRH-I2 KYC code re-review N1-N6, security re-verification N-1/N-2

Scope: `identity-compliance`'s own second fix round, responding to two coordinator-relayed
messages: (1) code re-review of fix round `f7a0da0` (verdict "READY once N1 is corrected"),
covering N1-N6 plus a mirror `ReviewVerification` race assigned directly to
`identity-compliance` ("you own the KYC code"); (2) security re-verification `b4b225e`, adding
N-1 (HIGH, pre-existing, blocks production launch) and N-2 (LOW, C5 untested).

### 20.1 N1 — M8's evidence corrected, test un-vacuumed

Fix round 5's own C4 change (`gatherSubmissionDocuments` treats an empty document set as a
separate, legitimate no-op) made `TestSubmitVerification_TerminalVerificationIsANoOp`
vacuous: it never seeded a document, so it passed identically whether the terminal guard
existed or not. Fixed by seeding a document on the verification before moving it to a terminal
status, restoring the test's discriminating power; re-verified by mutation (see
`docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt`, §2.5, M8-reverify). The
evidence file's own prior "17/17" claim is corrected there as well — it was never accurate once
M8 stopped genuinely killing anything, a fact neither this round's own predecessor nor the
original submission had caught before code re-review flagged it.

### 20.2 N-1 — cross-account overlay narrowed to each other account's latest FINAL row

**This is an orchestrator engineering decision, pending review by the `architect` and by
`identity-compliance` itself (the coordinator will dispatch those reviews separately) — not
a unilateral redesign of shared enforcement architecture.** It is recorded here because it
changes `crossAccountRejectedOverlay`'s own read semantics (§2.6, §19.2's own predicate), and
because §19's own text needed correcting regardless (§19.1, §19.4 above).

**The defect (security re-verification N-1, HIGH, pre-existing — predates fix round 5, predates
this ADR's own original design; not introduced by PRH-I2's transaction-boundary split):** the
overlay's "each OTHER account's own latest DECIDED row" read (§19.2's fix) counted a merely
`pending`/`review_required` row as decided, identically to a terminal `approved`/`rejected`/
`expired` row. A player with a rejection on account B could neutralise the overlay's deny on
account A with one ordinary API call — start a new verification on B; it need not ever be
approved — because the instant B's new row lands as `pending`, it becomes B's own "latest
decided" row and the rejection is no longer the row `crossAccountRejectedOverlay` sees.
Reproduced exactly as security review's own probe: account A approved, account B rejected,
withdrawal from A denied; player calls `CreateVerification` on B, mock returns `pending`;
withdrawal from A becomes ALLOWED. Fix round 5's own §19.4 text called this "correct, not a
residual gap" — that was wrong (corrected above, and retracted, not merely reworded).

**The fix:** `crossAccountRejectedOverlay`'s inner "latest row per OTHER account" subquery now
selects only from each other account's rows whose status is FINAL — `approved`, `rejected`, or
`expired` (`finalStatusesSQL`, `internal/kyc/enforcement.go`) — never `pending`,
`review_required`, or a never-decided orphan (unchanged from §19.2: orphans stay excluded
regardless). A rejection on another account is now lifted ONLY by a LATER final decision on
that SAME account — ordinarily `approved`, but symmetrically also by a later `expired`/
`rejected` re-decision, which simply keeps the deny in force under a fresh terminal row rather
than under the stale one. A merely-in-progress re-verification attempt (`pending`,
`review_required`) can never, by itself, lift the deny — closing exactly the gap N-1 describes.

**The primary-account rule is UNCHANGED.** `readLatestVerificationByPlayerAccount` (the gated
account's OWN latest row, §2.6(a)) still includes non-final rows exactly as before — a fresh
`pending` on the account BEING withdrawn from still supersedes that SAME account's own prior
approval (this is the pre-existing, intentional "success path supersedes a pending row"
behaviour §19.2 itself already preserved, unaffected by this round). N-1's fix touches ONLY the
cross-account overlay's OTHER-account subquery.

**Consumer-path scope (verified by reading every `EvaluateEnforcement` call site, not merely
grepping the overlay function's own name):** `crossAccountRejectedOverlay` is called from
exactly one place, `evaluateWithdrawalStructuralRule`, reached only for withdrawal operations.
`internal/payments/kycgate.go` (deposit) and `internal/casino/orchestrator.go`/
`internal/sportsbook/orchestrator.go` (play) all call `EvaluateEnforcement`, but for
deposit/play operations, which route to `evaluateDepositThreshold`/`evaluatePlayTrigger`
instead — neither of which ever calls the overlay. Deposit and play were never affected by N-1
and require no change.

**Tests (mutation-verified — `docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt`
§2.5, N-1):** `internal/kyc/enforcement_integration_test.go` gains
`TestEvaluateEnforcement_N1_FreshPendingVerificationDoesNotLiftRejection` (the exact reproduced
sequence — a fresh `pending` on the rejected account must never lift the deny),
`TestEvaluateEnforcement_N1_ReviewRequiredDoesNotLiftRejection` (same, for `review_required`),
`TestEvaluateEnforcement_N1_LaterFinalApprovedLiftsRejection` (a LATER final `approved` on that
same account DOES lift the deny — the fix's own positive case), and
`TestEvaluateEnforcement_N1_OrphanOnRejectedAccountDoesNotLiftRejection` (an orphan on the
rejected account changes nothing, per §19.2's unaffected predicate).

**Policy item, not a new human decision unless `identity-compliance` says otherwise:** the
pre-existing "a staff `review_required` escalation is not sticky against a later provider/
callback `approved`" behaviour (statusRank: review_required=2 < terminal=3, so a vendor
auto-approval can still move a row forward past a staff escalation) is left AS-IS by this round.
It is not touched by N-1 and is not itself a new finding — recorded as a policy item for
`identity-compliance` in `docs/governance/task-registry.md`, per the coordinator's explicit
instruction not to treat it as a new human decision unilaterally.

### 20.3 Mirror race — `ReviewVerification` now CAS's against the status it read

Assigned directly to `identity-compliance` ("you own the KYC code"): `ReviewVerification`
(staff decisions — approve/reject/require-more-documents) performed a read-then-update with NO
status predicate, unlike `SubmitVerification`'s phase C and the callback path, which both CAS
against `applyForwardOnlyStatus`'s forward-only rank rule. A concurrent provider submission or
callback landing between `ReviewVerification`'s own read and its `UPDATE` could be silently
overwritten by the staff write, or vice versa, with no error to either side.

**The fix:** the `UPDATE` now carries `AND kyc_verifications.status = $6` (the status
`ReviewVerification`'s own read observed). On a lost race (`pgx.ErrNoRows`), it returns the new
sentinel `ErrVerificationStatusConflict` rather than silently no-op'ing. **This is deliberately
DIFFERENT from the provider/callback paths' own `applyForwardOnlyStatus`, which silently no-ops
on a lost race (safe there — a provider-driven transition is naturally retryable/idempotent).
A staff decision is not**: it is a deliberate, one-time human judgment call, not a replayable
provider transition, so a lost race must surface as a conflict the staff member can see and
retry with fresh information, never disappear silently. `internal/httpserver/
kyc_admin_handlers.go`'s review handler maps `ErrVerificationStatusConflict` to 409 Conflict.

**Test (mutation-verified — evidence file §2.5, "Mirror race"):**
`TestReviewVerification_ConcurrentSubmissionDuringReview_ReturnsConflict`
(`internal/kyc/kyc_two_phase_integration_test.go`) uses a package-level test-only hook
(`reviewVerificationTestRaceHook`, nil in production) to run a genuine concurrent
`SubmitVerification` call between `ReviewVerification`'s own read and write, in the same P1/P2
race style fix round 5's own R1 tests use. Requires a multi-connection pool (`testPool`, not
`testPoolSized(t, 1)`) since the hook runs a genuinely separate transaction on a separate
connection.

### 20.4 N6 — one shared forward-only CAS loop, not two hand-maintained copies

`applyCallbackOutcome` (the provider callback path) previously carried its own, separately
hand-maintained copy of the identical forward-only rank/CAS loop `SubmitVerification`'s own
phase C uses via `applyForwardOnlyStatus`. Refactored so `applyCallbackOutcome` now calls the
SAME shared `applyForwardOnlyStatus` — one loop, not two copies that could silently drift apart.
No behavioural change; verified by the full pre-existing callback test suite plus the full
`internal/kyc` suite, both green after the refactor. Stale comments claiming the callback path
had "no enforcement effect" from this consolidation are corrected in-line.

**Phase C is now forward-only for BOTH paths, and this correction applies retroactively to
ADR 0095 §15.3's own description:** a `review_required` decision can no longer be demoted back
to `pending` by either a later submission result or a later callback — both paths only ever
move a verification's status FORWARD along `statusRank` (unverified < pending < review_required
< terminal), never backward, silently no-op'ing (not erroring) when a lower-rank result would
otherwise have been written. This was already true for `SubmitVerification`'s own phase C since
fix round 5's R1 fix; N6 only removes the duplicate implementation, it does not change this
behaviour, but ADR 0095 §15.3 is amended (see that document) to state it applies to the
callback path too, since its own text had described only the submission path.

### 20.5 N3/N-2/C5 — `RedactedProviderErrorDetail` unit-tested

Both code re-review (N3) and security re-verification (N-2, closing C5) independently flagged
the same gap: `RedactedProviderErrorDetail`'s default branch (an unclassified error returns the
fixed string `"internal error (redacted)"`) had no test, so a "return the raw error text
instead" mutant survived undetected. Closed with
`internal/kyc/redacted_provider_error_detail_test.go` (new, no build tag — a pure unit test) and
`internal/httpserver/kyc_create_verification_log_redaction_test.go` (new, build-tag
`integration` — an end-to-end companion proving the redaction survives through to the
`CreateVerification` handler's own structured log line). Mutation-verified (evidence file §2.5).

### 20.6 N5 — orphan verifications fail closed on upload and submit

A verification with `provider_reference IS NULL` (an orphan — `CreateVerification`'s own phase
B never completed, ADR 0095 §15.2) had no explicit guard against a document upload or a submit
attempt. A submit attempt against such a row would have sent an EMPTY reference string to the
provider, which is malformed and PROVIDER DEPENDENT with a real vendor. Fixed with a new
sentinel, `ErrVerificationNotSubmitted`, checked in both `UploadDocument` and
`SubmitVerification` independently (defense in depth — `SubmitVerification` is itself an
exported, independently callable entry point). `internal/httpserver/kyc_handlers.go` maps the
sentinel to 409 Conflict with guidance to start a new verification. Mutation-verified (evidence
file §2.5).

### 20.7 Verification (fix round 6)

`gofmt -l .`, `go build ./...`, `go vet ./...`, `go vet -tags integration ./...`, and
`golangci-lint run ./...` (no build tags, matching this repository's own CI invocation) all
report clean/0 issues. `go test -tags integration -race -count=1 ./internal/kyc/...`,
`go test -tags integration -race -count=1 ./internal/httpserver/... -run 'KYC|Kyc'`,
`go test -tags integration -race -count=1 ./internal/withdrawal/...`, and
`go test -tags integration -race -count=1 ./cmd/platform-api/...` all pass, against private
databases (never the shared `igaming_platform_ci_local` instance). Full mutation-kill evidence:
`docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt`, §2.5.

### 20.8 Labels (round 6 summary)

- **N1 (code re-review):** IMPLEMENTED — test corrected, M8 evidence corrected.
- **N2 (code re-review):** IMPLEMENTED — misleading comment and merged doc-comment block fixed.
- **N3 / N-2 / C5 (code re-review + security):** IMPLEMENTED — unit + end-to-end tests added,
  mutation-killed.
- **N4 (code re-review):** IMPLEMENTED — `status_applied` audit flag test added, mutation-killed.
- **N5 (code re-review):** IMPLEMENTED — orphan fail-closed guards added to both call sites,
  mutation-killed.
- **N6 (code re-review):** IMPLEMENTED — shared `applyForwardOnlyStatus` loop, no behavioural
  change, verified.
- **Mirror race (identity-compliance's own KYC code, assigned by code re-review):**
  IMPLEMENTED — `ReviewVerification` now CAS's and fails closed with
  `ErrVerificationStatusConflict` on a lost race, mutation-killed.
- **N-1 (security re-verification, HIGH):** IMPLEMENTED as an orchestrator engineering decision
  (§20.2) — pending `architect` and `identity-compliance` review, to be dispatched separately by
  the coordinator. Not yet a fully closed finding until that review completes.
- **review_required → approved forward move (security review, §19.2's "residual" note):** left
  as-is, recorded as a policy item for `identity-compliance` in
  `docs/governance/task-registry.md` — not a new human decision unilaterally.
- **Everything else in this ADR** (§14-§19's own labels, except the §19.1/§19.4 corrections
  above) is unchanged by this round.

## 21. Fix round 7 (2026-09-27) — N-1b (`expired` re-opened the overlay bypass), `KYC-REVIEWREQ-FORWARD-1` ruled and implemented, R2-2 to R2-5, and the create-side `ProviderError`/no-reference fix

Scope: security re-verification 2 (`rv-prh-i2-kyc-security.md`, N-1b, HIGH, production-launch
blocker), `identity-compliance`'s own domain ruling
(`rv-prh-i2-kyc-identity-compliance.md`, Rulings 1-4, plus its addendum confirming the exact
predicate shape), and code re-review's own re-verification 2
(`rv-prh-i2-kyc-code-review.md`, R2-1 through R2-5 and the pre-existing create-side note).

### 21.1 N-1b — `expired` excluded entirely from the cross-account overlay's "latest final row" selection, not merely disallowed from lifting

**The regression:** §20.2's own `finalStatusesSQL` (`('approved', 'rejected', 'expired')`)
included `expired` as one of the statuses whose LATEST occurrence on another account is
selected and tested for `= 'rejected'`. This re-opened exactly the shape N-1 itself closed: a
player with a rejection on account B could start a fresh re-verification on B and, if it
happened to reach a vendor's own `expired` outcome (an attempt that lapsed/was abandoned -
`ProviderExpired`, PROVIDER DEPENDENT reachability with a real adapter, not reachable with
today's mock) rather than `pending`, B's latest FINAL row became the `expired` one instead of
the earlier `rejected` one, and the overlay's `EXISTS` predicate (which requires the RETRIEVED
latest-final row itself to be `status = 'rejected'`) returned false. Withdrawal from A became
ALLOWED. Both security's own re-verification and `identity-compliance`'s independent domain
ruling (Ruling 1) reproduced this on a scratch database and concurred: `expired` is not
evidence of a clearance (ADR 0028 §2 treats it as a distinct terminal provider decision, never
one the platform derives from `approved.expires_at` lapsing; ADR 0096 §2.3 folds it into the
SAME deny bucket as `rejected`, "covers 'never verified', rejected, and expired uniformly") -
it must never be able to supersede anything in this overlay.

**The fix, per the coordinator's explicit fail-closed instruction and identity-compliance's
own confirming addendum:** `finalStatusesSQL` is narrowed to `('approved', 'rejected')` -
`expired` is excluded from the "latest final row" SELECTION entirely (not selected, then
special-cased in application logic - the addendum's own explicit warning against that shape,
since a special-case can fall through to allow). This means a `rejected` row on another
account is superseded ONLY by a LATER genuine `approved` row on that SAME account; any
`expired` rows are simply invisible to this subquery's own "latest row among
{approved, rejected}" selection, wherever they fall in time - so an `expired` attempt landing
BETWEEN a rejection and a later approval does not "get the walk-back stuck": the later approval
is still found and still lifts the deny (identity-compliance's own required test,
`TestEvaluateEnforcement_N1b_ApprovedAfterExpiredStillLiftsRejection`, §21.3 below).

Implementation: `internal/kyc/enforcement.go`, `finalStatusesSQL` constant and its own doc
comment, plus `crossAccountRejectedOverlay`'s own doc comment - both corrected to state the
`expired`-exclusion rationale explicitly, replacing the retracted "or, symmetrically,
superseded by a later final `expired`/`rejected` re-decision" language §20.2 originally
carried (that language was itself part of the defect, not merely imprecise wording about a
correct implementation).

### 21.2 `KYC-REVIEWREQ-FORWARD-1` — ruled by identity-compliance, implemented this round

**CORRECTED by §22.1 (security re-verification 3, N-3, MEDIUM, 2026-09-27):** the mechanism
described below, as implemented in THIS round, was too broad - it blocked every provider-driven
forward move off a staff-sticky row, including a genuine vendor `rejected` outcome, which
Ruling 2 never asked for and which itself weakened enforcement (§22.1 has the full analysis and
fix). The ruling itself (a staff escalation must not be silently overridden by an AUTOMATED
APPROVAL) is unchanged and correct; only this round's own implementation of it was too broad.
Read this section as the historical record of the first implementation, and §22.1 as the
corrected, currently-accurate one.

`identity-compliance`'s own domain ruling (Ruling 2, within its Authority per CLAUDE.md - an
internal case-management control, not a jurisdiction-varying legal threshold): **a
`review_required` verification that a STAFF member set must be sticky against an automated
forward move to `approved` by a later provider result or callback alone.** Only another
explicit staff `ReviewVerification` call may move such a case forward. A **provider-set**
`review_required` (the vendor's own "needs manual review" signal, no staff actor involved) is
UNAFFECTED - the existing forward-only behavior (a later vendor `approved` proceeding
automatically) still applies there, since no human judgment is being silently overridden.

**Mechanism (per Ruling 2's own specified mechanism):** `internal/kyc/provider.go`'s
`applyForwardOnlyStatus` (the SAME shared function §20.4/N6 already consolidated both the
submission and callback paths onto) now gates its own CAS `UPDATE` with an additional
predicate: `AND NOT (status = 'review_required' AND reviewed_by IS NOT NULL)`. `reviewed_by`
is written ONLY by the staff `ReviewVerification` path (confirmed by grep, per Ruling 2's own
verification) - never by `applyForwardOnlyStatus` or its callers - so this is a sufficient,
already-existing signal requiring no schema change. On a lost race caused specifically by this
guard (the re-read shows the SAME status, still `review_required`, with a non-nil
`reviewed_by`), the function returns immediately as a documented no-op (`applied=false`, no
error) rather than retrying into `applyForwardOnlyStatus`'s own bounded 3-attempt loop and
misreporting a deliberate policy block as "exhausted retries" - per Ruling 2's own explicit
"keep the forward-only no-op semantics" requirement, so a provider result landing on a
staff-sticky row is recorded/audited by the caller exactly like any other superseded result,
never treated as a failure.

**Tests, both mutation-killed (`internal/kyc/kyc_two_phase_integration_test.go`):**
`TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval` (a staff
`review_required` survives a later provider `approved` unchanged, with `reviewed_by` intact
and `status_applied=false` on the blocked submission's own audit row) and
`TestApplyForwardOnlyStatus_ProviderSetReviewRequiredStillAdvancesToApproved` (the control
case - a PROVIDER-set `review_required`, `reviewed_by` still `uuid.Nil`, still advances to
`approved` on a later provider decision, exactly as before).

**Ruling 3 (ReviewVerification's 409-on-lost-race design) and Ruling 4 (no human decision
required for any of the above):** both CONFIRMED by identity-compliance, no code change
required for either.

### 21.3 Regression tests for N-1b (`internal/kyc/enforcement_integration_test.go`)

Per the coordinator's explicit ask (seeded both via a real, verified callback and directly),
plus identity-compliance's own required "walk-back" case:

- `TestEvaluateEnforcement_N1b_ExpiredOnRejectedAccountDoesNotLiftRejection_ViaCallback` -
  B's rejection followed by a NEW verification (real `CreateVerification` + a genuine, signed
  callback delivering `expired`) - withdrawal from A still denies.
- `TestEvaluateEnforcement_N1b_ExpiredOnRejectedAccountDoesNotLiftRejection_SeededDirectly` -
  the same scenario, seeded with the file's own `setVerification` direct-SQL convention - both
  seeding paths must agree, since the fix lives entirely in the read.
- `TestEvaluateEnforcement_N1b_ApprovedAfterExpiredStillLiftsRejection` - rejected(B) →
  expired(B) → approved(B) still ALLOWS (the required "walk-back doesn't get stuck" case).

Mutation-verified against BOTH directions (`docs/plans/payment-readiness/evidence/prh-i2-kyc-mutation-kill.txt`,
§2.6): re-adding `expired` to the set (the exact regression) is killed by the two "does not
lift" tests; removing `approved` from the set (the inverse - breaking the walk-back) is killed
by the "still lifts" test.

### 21.4 R2-2 — upload handler's own `submit_verification_failed` redaction, unit-pinned

The upload handler's log line already used `kyc.RedactedProviderErrorDetail` correctly (no code
defect), but had no test of its own proving it end to end over HTTP - the create-path's own
test does not exercise this second call site. Closed with
`TestKYC_UploadSubmitFailureLog_NeverLeaksRawErrorText`
(`internal/httpserver/kyc_round3_http_test.go`, new), mutation-killed (reverting the log line
to `submitErr.Error()` makes the sentinel leak, caught immediately).

### 21.5 R2-3 — both new 409 mappings now have HTTP-level tests

Neither `kyc.ErrVerificationStatusConflict`'s mapping in `newReviewVerificationHandler` nor
`kyc.ErrVerificationNotSubmitted`'s mapping in `newUploadMyDocumentHandler` had a test proving
the 409 contract itself (as opposed to the underlying domain behaviour, which was already
pinned) - removing either branch silently fell through to a 500 with no test failing.

- `TestKYC_ReviewVerificationConflict_Returns409NotInternalError` drives a genuine concurrent
  read/write race through two real HTTP requests (bounded retries against fresh verifications,
  since cross-package tests cannot reach `internal/kyc`'s own package-private
  `reviewVerificationTestRaceHook` the way `internal/kyc`'s own mirror-race test does) and
  requires the EXACT `ErrVerificationStatusConflict` message to appear on the losing 409, not
  merely any 409 (the pre-existing `ErrInvalidTransition` 409 has a different message and would
  not prove this specific branch). Mutation-killed: removing the branch produces a 500.
- `TestKYC_UploadOrphanVerification_Returns409NotInternalError` uploads to an orphan
  verification a player legitimately discovers through their own
  `GET /v1/me/kyc/verifications` listing. Mutation-killed: removing the branch produces a 500.

### 21.6 R2-4 — stale "only allows on 'passed'" orphan comments corrected

`insertOrphanVerification`'s and `CreateVerification`'s own doc comments
(`internal/kyc/verification_service.go`) still attributed the orphan's harmlessness to
"EvaluateEnforcement only allows on 'passed'" - the exact framing RV-PRH-I2 KYC review F1
(§19 above) proved false in the deny direction. Corrected to state the ACTUAL mechanism:
`orphanRowExclusionSQL`'s explicit exclusion from `readLatestVerificationByPlayerAccount`'s own
"latest row" read (§2.6(g)) and from the cross-account overlay's `finalStatusesSQL`-scoped
subquery (§20.2/§21.1) - not any general "only passed matters" property.

### 21.7 R2-5 — evidence-file mutant count corrected

The prior round's evidence file claimed "23/23", double-counting M8 (superseded in §1, then
re-counted as M8-reverify in §2.5 - only ONE of those two entries is a currently-valid mutant).
The correct distinct count is 22 from that round, PLUS this round's own new mutants (§2.6,
below) - see the evidence file itself for the corrected running total.

### 21.8 Pre-existing, PROVIDER DEPENDENT: create-side `ProviderError`/no-reference now leaves the orphan untouched instead of writing an unresolvable `pending` row

**The gap (code re-review's own "pre-existing, low" note):** `applyCreateVerificationResult`
mapped ANY unrecognized `ProviderOutcome` (including `ProviderError` returned WITHOUT a Go
error - a real adapter's own "the request was ambiguous/undecided" signal, PROVIDER DEPENDENT,
never produced by the mock) to `StatusPending`, storing `NULLIF(reference, '')` regardless of
whether the vendor actually returned one. When the vendor's response carries NO reference, this
produced a `pending` row with a NULL `provider_reference` - a row that (a) counts as "decided"
under the primary read (§2.6(a) - `pending` is non-final but still supersedes an account's own
prior approval, unlike an excluded orphan), so a transient/ambiguous vendor response could turn
an existing ALLOW into a spurious DENY, and (b) is a PERMANENT dead end under N5 - a row with no
reference can never be uploaded to or submitted (`ErrVerificationNotSubmitted` on every
attempt), and no reference will ever arrive to change that.

**The fix (fail-closed in the direction that cannot turn an allow into a deny, nor a deny into
an allow):** `applyCreateVerificationResult` now leaves the phase-A orphan row EXACTLY as
committed - `unverified`, NULL reference - when the outcome is unrecognized AND no reference
was returned, returning `ErrProviderUnavailable` instead of writing anything. This is IDENTICAL
in shape to a phase-B failure (no resolver, credential failure, or the provider call itself
erroring) and to `SubmitVerification`'s own IC condition 2 ("an ambiguous/timeout result leaves
status unchanged", ADR 0095 §15.3) - a genuinely undecided vendor response is treated as "the
platform could not obtain a decision," never silently upgraded to a worse-than-orphan
unresolvable `pending` state. An unrecognized outcome WITH a genuine reference is UNAFFECTED -
it still becomes `pending`, since a real reference exists for a future callback or
re-submission to resolve.

**Tests, both in `internal/kyc/kyc_two_phase_integration_test.go`, the first mutation-killed:**
`TestCreateVerification_UnrecognizedOutcomeWithNoReferenceLeavesOrphanUntouched` (asserts
`ErrProviderUnavailable` and that the row is still exactly `unverified`/NULL) and
`TestCreateVerification_UnrecognizedOutcomeWithReferenceStillBecomesPending` (the control case).

### 21.9 Verification (fix round 7)

`gofmt -l .`, `go build ./...`, `go vet ./...`, `go vet -tags integration ./...` all clean.
`golangci-lint run ./...` (pinned binary, untagged, matching CI) — see the evidence file for
the exact invocation and result. `go test -tags integration -race -count=1` all green for
`./internal/kyc/...`, `./internal/httpserver/... -run 'KYC|Kyc'`, `./internal/withdrawal/...`,
and `./cmd/platform-api/...`, against private databases.

### 21.10 Labels (round 7 summary)

- **N-1b (security re-verification 2, HIGH, production-launch blocker):** IMPLEMENTED —
  `finalStatusesSQL` narrowed to `('approved', 'rejected')`; `expired` excluded from the
  overlay's "latest final row" selection entirely. Confirmed by identity-compliance's own
  independent domain ruling (concurring, not merely reviewing). Regression tests
  mutation-killed in both directions. N-1/N-1b together are now CLOSED, pending the
  coordinator's own separate dispatch of an architect/identity-compliance review of this as an
  orchestrator engineering decision per §20.2's own framing (unchanged by this round).
- **KYC-REVIEWREQ-FORWARD-1:** RULED (identity-compliance, Ruling 2) and IMPLEMENTED this round
  — `applyForwardOnlyStatus` gates on `reviewed_by IS NOT NULL`. **CORRECTED by §22 (security
  re-verification 3, N-3, MEDIUM): this round's own gate was too broad (blocked deny-direction
  outcomes too, not just approvals) - see §22.1 for the fix. This label is superseded; the
  registry row stays OPEN pending security's re-verification of the §22 fix, not "closed".**
- **R2-2, R2-3, R2-4:** IMPLEMENTED — HTTP-level tests added for both, mutation-killed; stale
  comments corrected.
- **R2-5:** IMPLEMENTED — evidence file's mutant count corrected.
- **Pre-existing create-side `ProviderError`/no-reference note:** IMPLEMENTED — orphan left
  untouched instead of writing an unresolvable `pending` row; mutation-killed.
- **Known mislabel (not a history rewrite):** commit `be423c3`'s own message says
  "identity-compliance: rule on N-1 fix..." but its diff carries security's own re-verification
  2 section of `rv-prh-i2-kyc-security.md` (two concurrent agents' commits crossed in the same
  window). The actual identity-compliance ruling is commit `6621326` (same message, correct
  diff — `rv-prh-i2-kyc-identity-compliance.md`) plus its addendum `c80103b`. Recorded here for
  the record; no commit history rewrite performed.
- **Everything else in this ADR** (§14-§20's own labels, except §20.2's `finalStatusesSQL`
  content itself, corrected by §21.1 above) is unchanged by this round.

## 22. Fix round 8 (2026-09-27) — N-3: the sticky-review gate also swallowed a vendor rejection

Scope: security re-verification 3 (`rv-prh-i2-kyc-security.md`, N-3, MEDIUM, production-launch
blocker) of fix round 7's own `3671e0e` (§21.2's `KYC-REVIEWREQ-FORWARD-1` implementation).

### 22.1 The defect and the fix

**The defect:** §21.2's own implementation of `applyForwardOnlyStatus`'s sticky guard
(`AND NOT (status = 'review_required' AND reviewed_by IS NOT NULL)`) blocked EVERY
provider-driven forward move off a staff-escalated `review_required` row - not only an
automated `approved`, which is all identity-compliance's Ruling 2 ever asked to be blocked, but
also a genuine vendor `rejected` (and `expired`, though the overlay already ignores that
regardless). Security's own reproduction: staff escalates B to `review_required`; the vendor
then delivers a verified `rejected` callback for B; B stays `review_required` FOREVER (the
vendor does not redeliver once its own webhook is acknowledged), ZERO audit rows are written
(because `applyCallbackOutcome` writes no row when `applied=false`, the ordinary "a replay is
silent" convention - not distinguishing this from a genuine, policy-blocked discard), and a
withdrawal from the SAME Person's OTHER, approved account stays ALLOWED, because the
cross-account overlay (§20.2/§21.1) never sees B's own rejection. Security additionally named
an insider-abuse vector: one officer with `PermVerificationReview` could escalate a case to
`review_required` specifically to suppress an expected vendor rejection, with only the
escalation itself audited, never the suppression. This is a correction to the SCOPE of §21.2's
own implementation, not to Ruling 2 itself - Ruling 2's own title and rationale are about a
vendor overriding a human escalation with an automated APPROVAL, never about blocking a deny.

**The fix, in two parts:**

1. **Narrow the sticky guard to `newStatus = approved` only.** `internal/kyc/enforcement.go`'s
   sibling `finalStatusesSQL` fix (§21.1) already established the pattern of narrowing rather
   than special-casing; the same discipline applies here. `applyForwardOnlyStatus`'s own CAS
   `UPDATE` predicate is now `AND NOT (status = 'review_required' AND reviewed_by IS NOT NULL
   AND $1 = 'approved')` (`$1` is the incoming `newStatus`), and the matching Go re-read
   short-circuit that detects the sticky case (rather than an ordinary lost race) now requires
   `newStatus == StatusApproved` too. A deny-direction result (`rejected`, `expired`) on a
   staff-sticky row now applies through the ORDINARY forward-only CAS, exactly as it would on
   any other row.
2. **Write an explicit audit row whenever a result IS discarded by the sticky guard.**
   `applyForwardOnlyStatus` gained a fourth return value, `heldForReview bool` - true ONLY when
   the no-op was specifically the sticky guard firing (never for an ordinary replay/lower-rank
   race, which stays silent per B7's existing, unchanged convention). `applyCallbackOutcome`
   (`internal/kyc/provider.go`), which otherwise writes NO audit row on any `applied=false`
   outcome, now writes one new action, `kyc.provider_result_held_for_review`, when
   `heldForReview` is true - carrying `provider_outcome` and `discarded_status` so an officer
   reviewing the case can see exactly what the vendor said, even though it did not take effect.
   `applySubmissionResult` (`internal/kyc/document_service.go`, SubmitVerification's own phase
   C) already wrote an UNCONDITIONAL audit row regardless of `applied` - it did not have N-3's
   "silent evidence loss" defect - but now also carries a `held_for_review` boolean in that same
   row's metadata for consistency and to make the discard visible without cross-referencing
   `status_applied` and `provider_outcome` by hand.

### 22.2 Tests (mutation-verified, `internal/kyc/enforcement_integration_test.go`)

- `TestEvaluateEnforcement_N3_StaffReviewRequiredThenVendorRejected_StillApplies` - the
  coordinator's own required regression test: staff escalates B to `review_required`, a REAL,
  verified `rejected` callback is delivered for B, and the test asserts B reaches `rejected`
  (not stuck at `review_required`), exactly ONE `kyc.provider_callback` audit row exists, and a
  withdrawal from the Person's OTHER, approved account is DENIED via the cross-account overlay -
  the exact deny N-3 found was being lost.
- `TestEvaluateEnforcement_N3_StaffReviewRequiredThenVendorApproved_HeldForReviewAudited` - the
  control/audit case: staff escalates to `review_required`, a vendor `approved` callback is
  correctly still blocked (row stays `review_required`), NO `kyc.provider_callback` row is
  written (unchanged from before - an ordinary blocked no-op), but exactly ONE
  `kyc.provider_result_held_for_review` row IS written, naming the discarded `approved` status.

Mutation-killed in three directions: (a) reverting the guard to its unconditional §21.2 shape
(blocking every forward move) is killed by the rejection test - the row stays stuck at
`review_required` instead of reaching `rejected`; (b) removing the guard predicate entirely
(the widening inverse) is killed by BOTH the approval-control test above and §21's own
pre-existing `TestApplyForwardOnlyStatus_StaffSetReviewRequiredIsStickyAgainstProviderApproval`
- the row wrongly reaches `approved`; (c) removing the `heldForReview` audit-write block in
`applyCallbackOutcome` entirely is killed by the approval-control test's own audit-count
assertion - zero `kyc.provider_result_held_for_review` rows are found where exactly one is
required.

### 22.3 LOW: phase C's own failure log no longer embeds raw vendor outcome text

Security also flagged, informationally: `kyc_create_verification_phase_c_failed`
(`internal/kyc/verification_service.go`, CreateVerification's phase C) logged `err.Error()`
directly, which can embed the RAW, vendor-controlled outcome string via §21.8's own
"unrecognized outcome %q with no reference" error - unbounded adapter-supplied text in operator
logs, the same class of gap C5/N-2/R2-2 already closed for the KYC package's other log lines.
Fixed by logging `RedactedProviderErrorDetail(err)` instead - since this error path always
wraps `ErrProviderUnavailable`, it now logs the fixed string `"provider unavailable"`, never the
underlying outcome text. Test: `TestCreateVerification_PhaseCFailureLog_NeverLeaksRawVendorOutcomeText`
(`internal/kyc/kyc_two_phase_integration_test.go`, a sentinel-based capturing-logger test in the
same style as the package's other redaction tests), mutation-killed.

### 22.4 Verification (fix round 8)

`gofmt -l .`, `go build ./...`, `go vet ./...`, `go vet -tags integration ./...` all clean.
`golangci-lint run ./...` (pinned 2.9.0 binary, untagged, matching CI) - 0 issues.
`go test -tags integration -race -count=1` all green for `./internal/kyc/...`,
`./internal/httpserver/... -run 'KYC|Kyc'`, `./internal/withdrawal/...`, and
`./cmd/platform-api/...`, against private databases.

### 22.5 Labels (round 8 summary)

- **N-3 (security re-verification 3, MEDIUM, production-launch blocker):** IMPLEMENTED - the
  sticky guard is narrowed to `newStatus = approved` only (WIDENED again by §22.6 below to
  `approved`/`expired`); a deny-direction result (`rejected`) always applies; an explicit
  `kyc.provider_result_held_for_review` audit row records every discard. Regression tests
  mutation-killed in all three directions above. Security re-verification 4 (§22.6) has since
  independently confirmed this fix (CLOSED).
- **LOW (phase C failure log):** IMPLEMENTED - bounded via `RedactedProviderErrorDetail`,
  mutation-killed.
- **§21.2's own "closed"/"ruled and implemented" framing:** SUPERSEDED by this section - see
  §21.2's own correction note and §21.10's corrected bullet.
- **Everything else in this ADR** (§14-§21's own labels, except the §21.2/§21.10 corrections
  above) is unchanged by this round.

### 22.6 Addendum (2026-09-27) — security re-verification 4, Ruling 5 (L-1): `expired` also held on a staff-sticky row; L-2 (held-for-review de-duplication)

Security re-verification 4 of `e4a4de2` CLOSED N-3 outright (no remaining regression across the
full N-1/N-1b/N-3 matrix) and found only two LOW, non-blocking notes, both addressed here.

**L-1 — a vendor `expired` on a staff-escalated row should be HELD, exactly like `approved`, not
applied.** Identity-compliance's own Ruling 5
(`rv-prh-i2-kyc-identity-compliance.md`) resolves this: `expired` is different from `rejected`
in exactly the respect that matters for this guard - it is ALREADY excluded from
`crossAccountRejectedOverlay`'s own `finalStatusesSQL` (N-1b, §21.1), so it is never a
cross-account signal whether it is held or applied, and this account's OWN enforcement outcome
is identical either way (a held row stays `review_required`/`OutcomePending`/deny; an applied
row becomes `expired`/`OutcomeFailed`/deny - both deny, per §2.3's outcome table). Holding it
therefore costs nothing on the propagation side N-3 protects, and closes the SAME
case-management gap Ruling 2 already named for `approved`: a vendor `expired` is not a decision
(ADR 0028 §2), so auto-applying it to a row a compliance officer deliberately escalated would
close that officer's own open case without any officer ever deciding it. **Fix:**
`applyForwardOnlyStatus`'s sticky guard (§22.1) is widened from `$1 = 'approved'` to
`$1 IN ('approved', 'expired')`, with the matching Go re-read short-circuit widened identically
(`newStatus == StatusApproved || newStatus == StatusExpired`). `rejected` remains fully
unaffected, exactly as N-3 requires. Test:
`TestEvaluateEnforcement_L1_StaffReviewRequiredThenVendorExpired_HeldForReviewAudited`
(`internal/kyc/enforcement_integration_test.go`) - staff escalates, a real verified `expired`
callback is delivered, the row stays `review_required`, zero `kyc.provider_callback` rows, one
`kyc.provider_result_held_for_review` row. Mutation-killed: security's own re-verification 4
mutant ("gate widened to `approved` or `expired`") SURVIVED against round 8's own test suite
before this fix - it now fails identically to that mutant's own signature when the fix is
reverted, confirming the mutant that previously survived is now the fix and is genuinely killed.
`TestEvaluateEnforcement_N3_StaffReviewRequiredThenVendorRejected_StillApplies` (§22.2) remains
green throughout - `rejected` is untouched by this widening.

**L-2 — `kyc.provider_result_held_for_review` rows repeated on vendor redelivery of the same
held outcome.** Each redelivery of an already-held outcome (a vendor's own retry policy,
unaware the platform is holding the result) wrote one more identical audit row - bounded by the
vendor's retry policy and harmless to enforcement, but noisy for a case-management UI consuming
these rows. Judged small enough to fix directly rather than register: `applyCallbackOutcome`
now checks, before writing a `kyc.provider_result_held_for_review` row, whether an identical row
(same `tenant_id`, `target_id`, and `metadata->>'provider_outcome'`) already exists, and skips
the write if so. A genuinely DIFFERENT held outcome on the same row (e.g. `approved` held, then
later `expired` held, both while still escalated) still gets its own, separate row, since the
outcome itself differs - this is a de-duplication of IDENTICAL redeliveries, not a "one row ever"
cap. Test: `TestEvaluateEnforcement_L2_RedeliveredHeldOutcome_DoesNotDuplicateAudit` - three
redeliveries of the SAME held `approved` outcome produce exactly one audit row; a subsequent,
genuinely different held `expired` outcome produces a second, separate row. Mutation-killed
(removing the de-duplication check reproduces exactly 3 rows instead of 1).

**Verification:** `gofmt -l .`, `go build ./...`, `go vet ./...`, `go vet -tags integration
./...` all clean. `golangci-lint run ./...` (pinned 2.9.0 binary, untagged) - 0 issues.
`go test -tags integration -race -count=1` all green for `./internal/kyc/...`,
`./internal/httpserver/... -run 'KYC|Kyc'`, `./internal/withdrawal/...`, and
`./cmd/platform-api/...`, against private databases.

**Labels:**
- **L-1 (security re-verification 4, LOW):** IMPLEMENTED - sticky guard widened to
  `approved`/`expired`, mutation-killed.
- **L-2 (security re-verification 4, LOW):** IMPLEMENTED - held-for-review de-duplication on
  (tenant, verification, provider outcome), mutation-killed.
- **`KYC-REVIEWREQ-FORWARD-1`:** RULED (identity-compliance, Rulings 2 and 5) and IMPLEMENTED -
  registry row updated to "ruled and implemented, pending a light security confirmation" (this
  addendum's own fix has not yet had its own dedicated security pass, though it directly
  resolves security's own L-1/L-2 notes from re-verification 4).
