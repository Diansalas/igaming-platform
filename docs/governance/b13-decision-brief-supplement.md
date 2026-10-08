# B13 decision brief — SUPPLEMENT (payout destination binding)

Prepared 2026-10-08 by `ledger-finance` at baseline `86a5439`, branch `gate-r14-b`. **Document only. Nothing here is
implemented, no option is chosen and no policy is decided.** It supplements `docs/governance/b13-decision-brief.md`
(the "original"), which is not changed. Source rule: only existing ADRs, registry, briefs, review notes and code are
used. Anything the record does not establish is marked **not established by the record**.

## 1. What problem B13 solves

A payout has no modelled payee. `payments.WithdrawRequest` (`internal/payments/types.go:519-524`) is
`{MerchantReference, Amount, AssetCode, PaymentMethod}`. The only destination-shaped input is `payment_method`, a free
string from finance staff in the submit body (`internal/httpserver/withdrawal_handlers.go:783-785`, required non-empty
at `:869`). It is used for routing (`internal/payments/payout.go:288-291`), copied onto the attempt (`:386-389`) and
sent to `provider.Withdraw` (`:447-450`). The platform controls amount, asset and rail. It does not control **where the
money goes**, and it does not prove that the destination belongs to the player (original §1-§3).

## 2. Is B13 required before PAY-PAYOUT-UNBOUND-RESOLVE-1? (dependency trace)

**The record does NOT establish that B13 must come before RESOLVE-1, or that RESOLVE-1 depends on B13.** It treats
the two as independent gates. Each one must close before any non-MOCK payout:

- `docs/HANDOVER.md` §36a classifies RESOLVE-1 as **class B** ("needs human/architect/security decision"), not
  **class D** ("depends on B13"). Only B14-B18 and the sandbox adapter are recorded as depending on B13.
- HANDOVER §36c (Round 9) and §36e (Round 12) and ADR 0095 §35.6 list B13 and RESOLVE-1 side by side as "required before ANY non-MOCK payout".
  No order between them is stated.

The binding constraints on RESOLVE-1 are in ADR 0095 §35.2 PO-1, §35.6 and ADR 0101 R-K3-8. **None of them refers to
the payout destination:**

| RESOLVE-1 constraint (source) | Needs the intended destination? |
|---|---|
| Resolution is "PSP-side recall/return or governed completion against the hold" (§35.2 PO-1; §35.6 wording) | **Not established.** The record does not say where a recall or return is credited, or whether it must be checked against an instrument. |
| LF F6: clearing only via the attempt's **own** `withdrawal_completed` keyed by the **PSP line reference** (positive attribution, §35.6) | No. Keyed on references and `release_ledger_transaction_id`. |
| Security I-1: compare the completion's amount and asset (§35.6) | No. |
| Security I-2: bound the unbounded persisted-lines query, and fail loudly on any cap (§35.6) | No. |
| Allocation (LEDGER-SUSPENSE-B-1) is never a payout route; M1 only acknowledges; M2 refuses unbound reasons (§35.2, §35.6, R-K3-8) | No. |
| R-K3-8 covers refused holds: `amount_asset_mismatch`, `callback_amount_asset_mismatch`, `invalid_provider_reference[:*]`, tombstone, `late_*` (ADR 0101 table, ~line 1327) | No. The list is keyed on terminal reasons. |

Indirect link, stated only as far as the record goes: both items gate the same event (the first non-MOCK payout), and
both concern payee correctness. B13 covers payee correctness before dispatch. RESOLVE-1 covers what happens after
contradictory evidence. Whether a "PSP-side recall/return" must be checked against a bound instrument is **not
established by the record** (Q1 below).

## 3. Invariant protected

Stated in `prh-i1-payout-launch-conditions.md` §"Destination binding" and `rv-prh-i1-payout-security.md:195`: **the
payout destination must come from a player-bound, verified instrument resolved SERVER-SIDE, never from the staff
request body.** The CLAUDE.md principle behind it is "authorization is enforced server-side only … never trusted from
the client". In financial terms, under ADR 0095 §4.5 ("payout failure is never assumed") a dispatched payout cannot be
recalled by the platform. A wrong payee therefore becomes a loss that is noticed only after it happens, and R-K3-8
gives no resolution path for it. No ledger invariant (`SUM(debits)=SUM(credits)`) is at stake. The ledger would
balance correctly for a misdirected payout.

## 4. Proposed behaviour, as far as the record documents it

The record contains only the principle in §3, which the original repeats in §5-§6 and §11 as Option B. Not established
by the record: the instrument entity, the verification source (vendor/KYC, PROVIDER DEPENDENT), the field on
`WithdrawRequest`, immutability rules, and the point in the flow where resolution happens. The original's §11 minimum
says resolution happens in `submit` and is a "pre-claim refusal". That is an inference in the original brief, not a
recorded design.

## 5. Evidence trust model (existing rules; B13 adds none)

| Source | Trusted for | Not trusted for | Rule |
|---|---|---|---|
| Staff submit body | Today: rail choice (`payment_method`) | Destination (required) | Launch conditions; CLAUDE.md |
| Callback (`ReceiptEvidence`, `receipt.go:189-214`) | Outcome, amount, asset and references, only within the **verified** provider (INV-IO-14) | Tenant, provider, posting authorisation ("evidence, never authorization to post", §9.3 FH-7) | §6.1, §6.5 |
| Poll echo (`StatusResult`, `types.go:538-551`) | Amount, asset, outcome. A non-empty differing echo means T10 `poll_reference_mismatch` | The echo is never bound; posting is keyed on the bound reference | §4.4 §36 note |
| Statement line | Detection only (INV-IO-12). Borrowed attribution "may raise, never clear" | Clearing without positive attribution (§35.6 L-1) | §35, §35.6 |
| Settlement against an unknown reference | None: S-M1 disputes it (`receipt.go:1013-1047`) | | Security S-M1 |
| Payer-identifying vendor data (IBAN, PAN, wallet address, name) | **Must not map into** `CallbackEvent`, `StatusResult` or a statement line | | **S95-C10** (§9.3) |

Consequence of S95-C10, a fact rather than a decision: under the current contract **no provider evidence can carry a
destination**. A bound destination therefore cannot be checked against callbacks, polls or statements. Whether
post-dispatch verification of the destination is wanted at all, and how that would fit with S95-C10, is **not
established by the record** (Q4).

## 6. Interaction with callbacks, receipt cells and the drain

- Every payout receipt cell keys on attempt state, amount, asset and references only (`applyResolvedReceiptEvidence`,
  `receipt.go:808-1133`). T10, T14 and T15, the callback mismatch/tombstone parks (§42.7), R-5, R-6 and M-1 (§42.8),
  and the reference guard (`payout_refbind.go:108`, `payout.go:748`) need no destination. **The record documents no
  change to any of them for B13.**
- The drain merchant-reference check (§42.8, PAY-RECEIPT-DRAIN-MERCHANT-REF-1) and INV-IO-14 resolve by
  provider and merchant reference. The merchant reference is the attempt id (INV-IO-3), not a payee. No interaction is
  documented.
- There is still **no payout webhook route** (`CallbackEventType` is `deposit` / `deposit_reversal`; §42.2). No
  binary builds a payout `Sweeper` (CP-W1). B13 does not change this.
- Unknown: whether a destination-mismatch evidence kind, terminal reason or alert reason should exist. The closed sets
  (`PayoutDisputeReasons()`, `payoutSignalReasons`) contain none, and the record proposes none.

## 7. Retries, replays, mismatches, ambiguous outcomes

- **T5 → T2 re-claim** (NotSent): the sweeper re-claims under the withdrawal lock and **re-runs the payout KYC gate**
  (LF95-C10(b)). **T12** resends the **same** attempt with the **same** key and also re-runs the gate (LF95-C10(c)).
  The adapter request is rebuilt from the attempt row (`payout.go:447-450`). Whether the destination is fixed on the
  attempt and resent unchanged, or re-resolved and re-verified at T2/T12 like the KYC gate, is **not established by
  the record** (Q3).
- **No payout cascade and no re-routing** (§4.7; §5.2 "never re-routed"). Return-to-source (D2) would sit "above (or
  instead of) provider health" in routing (`payment-orchestration.md` §4). How D2 would interact with the payout A0
  route choice is not established.
- **Replays and duplicates**: receipt dedup (`UNIQUE(tenant, provider, event_fingerprint)`) and the one-shot resolution
  are unchanged. The fingerprint tuple (§9.3) has no destination field.
- **Ambiguous / not_found**: never a release (INV-IO-7, §4.5). The record documents no change.

## 8. When destination evidence is unavailable

Documented fail-closed principles that would apply by analogy: a gate non-pass "claims nothing and makes no call"
(§4.3 T2). A pre-claim denial is legal only from `approved` (W-KYC). After T1p, no compliance outcome releases the
hold (LF95-C10(d)). The original §12(c) says "fails closed with no attempt row".

**Undefined in the record:**

- (a) What happens to the **hold** of an `approved` withdrawal that has no verified instrument. Under existing
  transitions an `approved` request has **no release path**: `Reject` only from `pending_review`, `Cancel` only from
  `requested` (ADR 0095 §43.1 correction; HSEC-APPROVED-HOLD-RELEASE-1). A refusal at submit therefore freezes the
  funds unless something else is decided.
- (b) What happens when an instrument is revoked or loses verification **after** T1p.
- (c) Whether a missing instrument is audited. ADR 0095 §43.2(a) already records that a refused staff submit writes
  no audit row.

## 9. Effect on payouts already created or parked

Only MOCK providers exist, so no real payout exists (original §2). For MOCK and dev rows:

- **`submitted` attempts and holds already claimed**: they carry `payment_method` only. Whether they need a destination
  backfill is **not established**. The only precedent is LF95-C11(e), which backfilled `payment_method` with
  `'legacy_unknown'`.
- **Unbound parks** (`invalid_provider_reference[:*]`, `provider_reference_conflict` with no reference) and **bound
  parks** (for example `callback_amount_asset_mismatch` holding X): permanent standing P1s with no clearing path until
  RESOLVE-1 (§35.6 LF F-2, R-K3-8). B13 changes neither, by the record.
- **R-K3-8 refused holds** (including tombstone and `late_*`, PAY-PAYOUT-CONTRADICTION-HOLD-1): unchanged.
- **Approved holds on suspended or closed tenants or brands** (HSEC-APPROVED-HOLD-RELEASE-1): unchanged; they are a
  separate owner decision.

## 10. Explicitly NOT authorised (by the record, until decided)

The following are not authorised:

- wiring any non-MOCK payout adapter, including a sandbox one;
- a sandbox PSP;
- a binary that constructs the payout sweeper;
- any instrument table, migration, or change to the submit API or `WithdrawRequest`;
- implementing RESOLVE-1 or PAY-PAYOUT-CONTRADICTION-HOLD-1;
- allocation for payouts;
- M2 on unbound reasons;
- deciding return-to-source (compliance/legal);
- custodian selection (ADR 0008: human decision);
- the retail-KYC sufficiency rule (`identity-compliance`);
- the sibling launch conditions S-L1, S-L3 and S-L4, which are not part of B13.

## 11. Alternatives in the record and their documented risks (no new ones)

| Alternative | Documented risk or status |
|---|---|
| **A: status quo** (staff `payment_method`, no destination) | Acceptable for MOCK only. The payee is uncontrolled: a misdirected or insider-redirected payout has no recall path. Launch blocker. |
| **B: server-resolved, player-bound, verified instrument** | The only forward option documented. Costs: a new entity, a verification dependency (PROVIDER DEPENDENT), API changes, a new ADR plus an ADR 0095 amendment. D1-D4 are open. |
| **D2: return to source** (`payment-orchestration.md` §4) | `OPEN DECISION`, a compliance/legal determination. If required, it changes routing precedence. `07-payments-architecture.md` ~line 425: no retail-cash analogue. |
| **D3: crypto destination** (ADR 0008) | Custodian owns keys and signing; the platform owns orchestration. No address-handling rule is documented. Custodian selection is a human decision. |
| **D4: cash / retail** (`07-payments-architecture.md` ~lines 376-390) | Mandatory identity-verification attestation on `complete`. The sufficiency rule is `identity-compliance`'s and is not designed. |

The record also contains one statement that bears on D1 and is not reconciled with B13: 07 §376-390 says "a PSP's own
KYC/rails already bind the payout instrument to a verified identity". Whether that counts as "verified" for D1 is
**not established by the record** (Q6).

## 12. Human decision required

Restated from the original §15, unchanged:

> "Before any non-MOCK payout (sandbox included), the payout destination MUST be a player-bound, verified instrument
> resolved server-side and never supplied by the staff request. The architect is authorised to write the ADR defining
> instrument storage, verification source and the answers to D1-D4 (incl. whether return-to-source applies, with
> identity-compliance/legal input), and implementation of B13 starts only after that ADR is accepted. Until then no
> non-MOCK payout adapter may be wired."

Sub-decisions this supplement surfaces. Each is a separate question; none is answered here:

1. **Ordering.** The record treats B13 and RESOLVE-1 as independent co-gates (HANDOVER §36a class B, not D). Should
   B13 be ordered before RESOLVE-1? In particular, must a "PSP-side recall/return" be checked against the bound
   instrument?
2. **Scope wording.** The launch-conditions note and the security review say "any real payout adapter" or "production
   launch". The classification document (line 54) and the original §15 say "any sandbox payout". Confirm the stricter
   scope, sandbox included.
3. **Retries.** Is the destination fixed on the attempt for T5/T2 and T12, or re-resolved and re-verified at each
   re-claim, as the KYC gate is?
4. **Provider evidence.** Given S95-C10, is post-dispatch destination verification required at all? If it is, how is
   it reconciled with the ban on payer-identifying fields?
5. **Approved hold with no verified instrument.** No release path exists today (§43.1). What is the intended
   disposition? This relates to HSEC-APPROVED-HOLD-RELEASE-1.
6. **Verification source.** Does a PSP's own KYC/rails binding (07) satisfy "verified" for D1, or is platform-side
   verification required?
7. **Pre-existing `submitted`/parked rows** (MOCK/dev): is a destination backfill needed, and if so which marker?
8. **Revocation after T1p.** Does an instrument revoked after claim affect the in-flight payout? (LF95-C10(d) is the
   KYC precedent.)
