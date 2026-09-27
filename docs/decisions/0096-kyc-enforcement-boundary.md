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
| 3 | Withdrawal request (hold placement) | `internal/withdrawal/withdrawal.go:291` `RequestWithdrawal` | **Internal** (places a hold; no funds leave yet) | None (no RG, no Risk, no KYC) — confirmed by reading the function; only balance sufficiency is checked | **ENFORCE** (early player-facing check) | BLUEPRINT §4.7 "first withdrawal"; registry KYC-ENFORCE-1 |
| 4 | Withdrawal promotion to review | `internal/withdrawal/withdrawal.go:429` `MoveToPendingReview` | Internal (state transition only) | None; the function's own doc comment says this is exactly where "automated KYC/velocity/risk checks are queued (owned by identity-compliance, not this package)" | **NOT-ENFORCE at this exact call** — the doc comment's promise is honored by gate #3 running before this state is ever reached, not by adding a second check inside this transition itself, so a request already past a KYC deny never reaches `pending_review` in the first place | Avoids a second read of the same fact for no new information — see §3 for why one early gate is sufficient given #5 is the hard backstop |
| 5 | Withdrawal payout dispatch | `internal/withdrawal/withdrawal.go:863` `LockApprovedForSubmission` (locks the row; caller then calls the provider and `:1006` `MarkSubmitted`) | **Leaves** (this is the point the platform hands funds to an external payout rail) | Four-eyes `Approve`/`Reject` (policy.go); no RG, no Risk, no KYC | **ENFORCE — the hard backstop** ("at minimum before payout submission", registry KYC-ENFORCE-1) | BLUEPRINT §4.7; CLAUDE.md "enforcement is our code, not the vendor's" |
| 6 | Withdrawal reject/cancel/reverse | `withdrawal.go:731,892` `Reject`, `LockSubmittedForResolution` | Internal (reverses the hold; no leave) | Existing state machine | **NOT-ENFORCE** | A correction path, not a new leave-the-platform event; nothing to gate |
| 7 | Casino bet placement | `internal/casino/orchestrator.go:952` `postBet` | Internal (debits `player_cash`, credits the provider-facing liability account; no external rail) | RG (`evaluateAndAuditEligibility`) → Risk (`evaluateAndAuditRisk`), ADR 0031 §1 | **ENFORCE (policy-driven, default `not_required`)** | The human directive names casino bets as an identified enforcement surface; this ADR does not silently drop that surface even though the *default* behaviour (no active jurisdiction policy) is unchanged from NOT-ENFORCE in practice. See §3.5 for the "play" trigger design — no money crosses the platform boundary at a bet, so the mechanism defaults to `not_required` absent an explicit, legally-reviewed KYC-before-play policy row (HD-KYC-8), never a compiled-in default the way withdrawal's structural rule is |
| 8 | Casino win | `internal/casino/orchestrator.go:1456` `postWin` | Internal (credits `player_cash` from the settled bet's liability account) | Deliberately none (own doc comment: not RG-gated, "a win is the platform paying out on its own already-accepted bet, not a new player-initiated action") | **NOT-ENFORCE** | Same reasoning as #7, one level stronger: gating a *settlement* of an already-accepted wager on a compliance state that may have changed since the bet would strand the platform's own already-incurred liability with no correction mechanism — exactly the class of harm `postRollback`'s doc comment names for RG |
| 9 | Casino rollback | `internal/casino/orchestrator.go:1586` `postRollback` | Internal (reverses #7/#8) | Deliberately none (own doc comment: "a correction to history, not a new stake") | **NOT-ENFORCE** | A correction must remain possible for exactly the players most likely to need one (own doc comment, quoted verbatim) — gating it on current KYC status would make the ledger un-correctable |
| 10 | Sportsbook bet placement | `internal/sportsbook/orchestrator.go:583-616` | Internal (same wallet-debit shape as casino bet) | RG → Risk, mirroring casino (ADR 0031, this file's own comments) | **ENFORCE (policy-driven, default `not_required`)**, with **HUMAN-DECISION** flagged | Same design as #7 (§3.5). Separately flagged because Blueprint's "EDD above configurable limits" tier does not specify whether "configurable limits" is scored only against deposit/withdrawal amounts or also against stake size; this ADR does not assume the latter (§3.6, HD-KYC-2) |
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
| ENFORCE | 3 | #1, #3, #5 |
| NOT-ENFORCE | 15 | #2, #4, #6, #7, #8, #9, #11, #12, #13, #14, #15, #16, #17, #18, #20 |
| HUMAN-DECISION (flagged, not designed) | 5 | #10 (sportsbook stake vs. EDD), #15 (bonus conversion AML), #18 (conversion op, when built), #19 (crypto rail, when built), #21 (affiliate payout, when built) |

Rows #4 is a "not a second check" call, folded into row #3's design, not a
gap — see §3.

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
)

// EnforcementParams is EvaluateEnforcement's input. Every identity field
// MUST be resolved server-side from the authenticated/tenant context —
// never accepted from a request body — identical to
// rg.EligibilityParams/risk.RiskRequest's own binding contract.
type EnforcementParams struct {
	TenantID           uuid.UUID
	BrandID            uuid.UUID
	PlayerAccountID    uuid.UUID
	Operation          EnforcementOperation
	AssetCode          string // required for amount-shaped operations
	Amount             int64  // minor units; required for deposit/withdrawal
	JurisdictionCode   string // caller's own trusted context, same contract as risk.RiskRequest.JurisdictionCode (ADR 0031 §9) — empty means "no jurisdiction resolved", matches only jurisdiction-unscoped policy
	LicensingJurisdictionID uuid.UUID // resolved from tenants.licence_id -> licences.jurisdiction_id, exactly as jurisdiction.ResolveEvaluationPolicy already does internally (ADR 0043 Decision 2) — the KYC policy table is keyed the same way, for the same bootstrap-circularity reason
	CorrelationID      uuid.UUID
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
	OutcomePassed       EnforcementOutcome = "passed"        // required, and the player has an approved kyc_verifications row
	OutcomePending       EnforcementOutcome = "pending"       // required; verification exists but is pending/review_required
	OutcomeFailed        EnforcementOutcome = "failed"        // required; no verification, or the latest is rejected/expired
	OutcomeUnavailable    EnforcementOutcome = "unavailable"   // the evaluator itself could not determine an outcome (DB error, malformed policy row) — NEVER a legitimate business state
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
| Casino launch/bet, sportsbook bet, bonus grant/activation/conversion | RG → Risk | **unchanged** — no KYC call added (§1 verdicts) |

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

1. **Structural — "first withdrawal."** This is a binary fact ("has this
   wallet ever completed a withdrawal before"), not a legally-reviewed
   numeric threshold. It requires no jurisdiction-specific value to be
   safe and Blueprint-faithful. This ADR ships it as a **compiled-in,
   always-on rule** — every withdrawal payout dispatch (row #5) and
   every withdrawal request (row #3) for a wallet with zero prior
   `completed` withdrawals requires `passed`, in every jurisdiction,
   with no policy row needed. A jurisdiction wanting a *different*
   structural rule needs its own recorded human decision (§4), never a
   silent per-tenant override — matching the Authority constraint in
   §2.3.
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
     change).
   - For **rows #3/#5 (withdrawal)**, the threshold triggers are
     genuinely optional refinements on top of the always-on structural
     rule in point 1 above — "dormant" only ever means "no *additional*
     EDD tier is active beyond the first-withdrawal gate," never "no
     gate at all." Withdrawal is never left ungated purely because a
     threshold value is missing, which is the fail-closed-for-
     value-leaving-the-platform posture this ADR is asked to justify
     explicitly. **This is a security/human-review design choice,
     recorded here, not an automatic consequence of the schema**: a
     future reviewer could instead choose to make even the structural
     "first withdrawal" rule itself jurisdiction-configurable and
     dormant-by-default; this ADR recommends against that, because
     Blueprint states the trigger unconditionally and no legal value is
     needed to honor it.

### 3.3 Migration 0101 — sketch

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
        CHECK (trigger_type IN ('cumulative_deposit', 'edd_amount', 'registration_tier')),
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
    effective_from               TIMESTAMPTZ NOT NULL DEFAULT now(),
    legal_review_reference       TEXT CHECK (legal_review_reference IS NULL OR btrim(legal_review_reference) <> ''),
    reason_code                  TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_actor_type        TEXT NOT NULL,
    created_by_actor_id          UUID,
    -- Cross-trigger-type CHECK: a row must carry the value shape its own
    -- trigger_type needs and no other (mirrors ADR 0043's per-column
    -- CHECKs; prevents a 'registration_tier' row from silently also
    -- carrying an unused threshold_minor_units that a future reader might
    -- misinterpret).
    CHECK (
        (trigger_type IN ('cumulative_deposit','edd_amount') AND threshold_minor_units IS NOT NULL AND asset_code IS NOT NULL AND required_tier IS NULL)
        OR (trigger_type = 'registration_tier' AND required_tier IS NOT NULL AND threshold_minor_units IS NULL AND asset_code IS NULL)
    ),
    -- 'active' rows require a legal review reference — mirrors ADR 0043's
    -- identical requirement for jurisdiction evaluation policy.
    CHECK (status <> 'active' OR legal_review_reference IS NOT NULL)
);

CREATE UNIQUE INDEX kyc_enforcement_policies_one_active
    ON kyc_enforcement_policies (licensing_jurisdiction_id, trigger_type)
    WHERE status = 'active';
    -- Only one active row per (jurisdiction, trigger_type) at a time —
    -- ADR 0043's own "no ON CONFLICT DO UPDATE, append a new active row
    -- and withdraw the old one" discipline applies unchanged; a full
    -- effective-dated history is read via ORDER BY effective_from.

-- Append-only: no UPDATE/DELETE except a status transition to
-- 'withdrawn', mirroring jurisdiction_precedence_configs' own trigger
-- (ADR 0043 Decision 3). Reuses the same trigger function shape, not a
-- new one.
CREATE TRIGGER kyc_enforcement_policies_append_only
    BEFORE UPDATE OR DELETE ON kyc_enforcement_policies
    FOR EACH ROW EXECUTE FUNCTION enforce_append_only_status_transition('withdrawn');

ALTER TABLE kyc_enforcement_policies ENABLE ROW LEVEL SECURITY;
-- No tenant_id: platform-wide reference data, read by every tenant
-- sharing a licensing jurisdiction, exactly like
-- jurisdiction_precedence_configs. Write restricted to a platform-scoped
-- connection (db.WithoutTenant) plus the compliance/platform_admin role
-- check already enforced by the HTTP handler layer, matching that
-- table's own policy.
CREATE POLICY kyc_enforcement_policies_read ON kyc_enforcement_policies
    FOR SELECT USING (true);
CREATE POLICY kyc_enforcement_policies_write ON kyc_enforcement_policies
    FOR INSERT WITH CHECK (current_setting('app.tenant_id', true) IS NULL);

-- Immutable, tenant-scoped audit of every enforcement decision — the
-- CLAUDE.md "audit every compliance-relevant action" requirement,
-- WITHOUT sensitive data: no document content, no raw provider reason,
-- no free text.
CREATE TABLE kyc_enforcement_decisions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    brand_id             UUID NOT NULL,
    player_account_id    UUID NOT NULL,
    operation            TEXT NOT NULL CHECK (operation IN ('deposit','withdrawal_hold','withdrawal_payout')),
    outcome              TEXT NOT NULL CHECK (outcome IN ('not_required','passed','pending','failed','unavailable')),
    allowed              BOOLEAN NOT NULL,
    matched_trigger      TEXT, -- e.g. 'first_withdrawal' or a policy row id; NULL for not_required with nothing configured
    policy_version        TEXT NOT NULL,
    correlation_id        UUID NOT NULL,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)
);
CREATE INDEX kyc_enforcement_decisions_player ON kyc_enforcement_decisions (tenant_id, player_account_id, decided_at DESC);

ALTER TABLE kyc_enforcement_decisions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON kyc_enforcement_decisions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
-- Append-only trigger, identical convention to audit_log and every other
-- compliance-record table in this codebase (no UPDATE/DELETE ever).
CREATE TRIGGER kyc_enforcement_decisions_immutable
    BEFORE UPDATE OR DELETE ON kyc_enforcement_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
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

### 3.4 No test-only or production default values

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
| **HD-KYC-5** | Whether a jurisdiction may ever relax the structural "first withdrawal" rule (§3.2 point 1) — this ADR's default answer is no, recorded as a design choice, not a foreclosed option | Authority constraint, CLAUDE.md Compliance section |
| **HD-KYC-6** | Cross-tenant/cross-brand reuse of an approved verification (ADR 0028 §7's own still-open decision) — this ADR does not change that answer; `EvaluateEnforcement` reads `kyc_verifications` scoped exactly as narrowly as today (tenant/brand), so a decision to widen reuse is a change to `internal/kyc`'s read, not to this enforcement boundary | ADR 0028 §7 |
| **HD-KYC-7** | Whether wallet-to-wallet `ConversionOperation` (row #18) or a future affiliate payout (row #21) need their own gate, once built | Deferred, not designed here |

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
| Integration | A player who has one prior `completed` withdrawal is exempt from the structural first-withdrawal rule on a second withdrawal, but a jurisdiction's `edd_amount` trigger (if active) still applies independently | #3, #5 |
| RLS | A `db.WithPlayerScope` connection cannot read another tenant's `kyc_enforcement_decisions`; a tenant-scoped write attempt to `kyc_enforcement_policies` is rejected (platform-wide write only) | schema |
| Concurrency | Two concurrent deposit attempts racing a staff KYC approval that commits between them each see a consistent, individually-correct outcome (no torn read) — mirrors `TestRequestWithdrawal_ConcurrentRequestsOnlyOneSucceeds`'s pattern applied to a read-then-decide check | #1, #3, #5 |
| Negative/security | A client-supplied field cannot influence `Outcome` (fuzz `EnforcementParams` for any player-controllable path into the decision); cross-tenant `player_account_id` cannot be evaluated against a different tenant's `kyc_verifications` row | all |
| Negative/security | `unavailable` (a forced DB error / malformed policy row in a test harness) denies at every enforcement point, with no operation-specific bypass | all |
| Fail-closed | A migration 0101 table with an `active` policy row missing `legal_review_reference` is rejected by the CHECK constraint before it can ever govern a decision | schema |
| Audit | Every `EvaluateEnforcement` call at every enforcement point writes exactly one `kyc_enforcement_decisions` row and one `audit.Record` call, in the same transaction as the domain effect it gated, with no PII/document content in either | all |
| SAR-adjacent | `kyc_enforcement_decisions` plus `kyc_verifications`/`audit_log` together give a compliance reviewer a complete, joinable trail of "what was decided, when, against what policy version" for any player — proves the audit design is queryable, not just present | staff tooling |

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
4. **`internal/casino`, `internal/sportsbook`, `internal/bonus`**: **no
   change** — §1's verdicts are NOT-ENFORCE for every one of their
   enforcement points; no dependency request needed unless a future human
   decision (HD-KYC-2, HD-KYC-4) reopens one of them.
5. **Migration 0101**: `identity-compliance` authors it; `ledger-finance`
   reviews the `NUMERIC(38,0)`/asset-registry handling on
   `threshold_minor_units` (CLAUDE.md's own money-representation rule);
   `security` reviews RLS and the append-only trigger.
6. **OpenAPI / staff route**: `identity-compliance` + `code-reviewer`.
7. **QA**: owns the test plan in §7 as an execution gate, per its
   existing testing-strategy authority.

Every row above is `PROPOSED`/`NOT IMPLEMENTED` until PRH-I3 is
authorized and executed; nothing in this document is claimed as built.

---

## 9. Labels

Per CLAUDE.md's seven-value vocabulary: this document is a design paper.
The mechanism it specifies is **NOT IMPLEMENTED**. No threshold value is
asserted. No vendor is selected. No regulatory approval is claimed.
