# B13 decision brief — payout destination binding (PAY-SEC-LAUNCH-1, destination part)

Prepared 2026-10-07 at HEAD `484cbc5` by the orchestrator. **Status: AWAITING ARCHITECT + OWNER DECISION. Nothing in this brief is implemented, and no option has been chosen.**
Source rule: only existing ADRs, registry, decision register and review evidence are used. Where the repository documents no option, this brief says so rather than inventing one.

Sources: `docs/plans/payment-readiness/prh-i1-payout-launch-conditions.md` (§"Destination binding", lines 74-91); `docs/plans/payment-readiness/rv-prh-i1-payout-security.md` (informational list, ~line 195); `docs/governance/task-registry.md` PAY-SEC-LAUNCH-1 (line ~4062); `docs/plans/prh2-hardening-round/analysis/payment-followups-classification-2026-10-05.md` (line 54); `docs/governance/payment-readiness-completion-report.md` §5; `docs/architecture/payment-orchestration.md` §4 (OPEN DECISION, return to source); `docs/architecture/07-payments-architecture.md` (~376-390, 427); ADR 0008; ADR 0095 (status block ~line 31; §4.4); `internal/payments/types.go:519`, `payout.go:289,380,439-441`, `internal/httpserver/withdrawal_handlers.go:776-870`.

## 1. Problem B13 solves
A payout has no modelled destination. Nothing records WHERE a player's withdrawal money goes, and nothing proves that destination belongs to the player.

## 2. Current payout behaviour (verified in code)
- `payments.WithdrawRequest` (`types.go:519`) = `{MerchantReference, Amount, AssetCode, PaymentMethod}`. There is no destination/instrument/account/address field. `internal/withdrawal` also has none.
- `payment_method` is a free string supplied by finance staff in the submit request body (`submitWithdrawalRequest.PaymentMethod`, `withdrawal_handlers.go:776`; required non-empty only). `withdrawal_requests` has no `payment_method` column (ADR 0095 backfill note LF95-C11(e): legacy rows get `'legacy_unknown'`). It is copied to `payment_attempts.payment_method` and used for routing (`payout.go:289,380`) and passed to `provider.Withdraw` (`payout.go:439-441`).
- Only MOCK providers exist; payout-typed callbacks are refused at the route; no binary constructs a payout `Sweeper` (CP-W1).

## 3. Why insufficient
The amount, asset and rail are controlled, but the payee is not. A real adapter would need to be told where to pay, and today the only available source is staff input or provider-side defaults — neither is player-bound or verified.

## 4. Exact risk
- **Financial:** funds released to an instrument that is not the player's (misdirected or insider-redirected payout). A payout once executed is not recallable by the platform (hold already moved to `psp_clearing`; unbound-payout holds have no resolution path, R-K3-8).
- **Security:** destination (or the choice of rail that implies it) is controlled by a single staff request body; the same decision class as ADR 0008's custodian-vs-self-custody call. Documented as `PROVIDER DEPENDENT` / launch condition.
- **Compliance:** documented as an AML control question (§5 below) and for cash: the cashier identity attestation (07 §376-390).

## 5. Decisions that need architect/owner
The repository documents exactly one required principle and several explicitly unresolved questions:
- **Documented requirement (not optional):** the destination "must come from a player-bound, verified instrument resolved SERVER-SIDE (never taken from the staff request body)" before any real payout adapter (`prh-i1-payout-launch-conditions.md:74-91`; `rv-prh-i1-payout-security.md`). The classification doc further says it is needed "to dispatch ANY sandbox payout" and that a "verified player-bound destination in `WithdrawRequest`" is required before sandbox payouts.
- **Documented as undecided (no option text exists in the repo):**
  - D1: where the player-bound instrument is recorded and who verifies it (the repo names no instrument table; `migrations/` has none; verification source is vendor/KYC dependent: PROVIDER DEPENDENT).
  - D2: whether withdrawals must return to the deposit instrument/provider ("return to source"): `payment-orchestration.md` §4 `OPEN DECISION` — a compliance/legal determination `identity-compliance`/legal must make; if required it sits above provider-health routing.
  - D3: crypto rail destination: tied to ADR 0008 (custodian owns keys; Stage 4 crypto work needs a human decision first). No address-handling rule is documented.
  - D4: cash/retail payouts: bound by the mandatory identity-verification attestation on `complete`; the sufficiency rule is identity-compliance's undesigned retail-KYC (07 §376-390).

## 6. Options supported by existing documentation
- **Option A — Status quo (staff-supplied `payment_method`, no destination).** Documented as acceptable ONLY for MOCK; documented as insufficient for any sandbox payout and a launch blocker for real ones.
- **Option B — Server-resolved, player-bound, verified instrument (the documented requirement).** Destination never taken from the staff body; resolved server-side from an instrument bound to the player and verified. This is the only forward option the repo documents. Its sub-choices D1-D4 are open and have no documented alternatives.
- The repo documents **no third option** (for example no staff-entered-with-approval variant). None is proposed here.

## 7. Pros / cons (limited to what is documented)
- A: no work, but cannot be used for any sandbox or real payout; payee uncontrolled.
- B: closes the payee-control gap and server-side-only trust matches CLAUDE.md "authorization server-side, never client"; cost: new modelled entity + verification dependency + changes to submit API and `WithdrawRequest`; D1-D4 must be settled first.

## 8. Security recommendation (documented)
Option B: destination from a player-bound, verified instrument resolved server-side; "launch condition for any real payout adapter" (`rv-prh-i1-payout-security.md`); and it is a decision the payments specialist must not make unilaterally (launch-conditions note).

## 9. Ledger-finance recommendation
None documented specifically for destination binding. LF's recorded position is limited to: unbound payout holds must keep the hold, allocation is never a payout route, and no payout without resolution path (B11 ADR §35.2 PO-1, R-K3-8).

## 10. Operational consequences (derived from the above, not new policy)
Under B, finance staff submit would stop choosing the payee; a withdrawal without a verified bound instrument cannot dispatch (fails closed); support needs a path for players lacking an instrument. Operational detail is not documented and is part of the decision.

## 11. Required DB / state machine / API changes
- **A:** none.
- **B (minimum, per the documentation):** a player-bound instrument record (DB; tenant RLS, append-only/audited per CLAUDE.md); destination carried on `WithdrawRequest`/attempt; `submit` handler resolves it server-side and the `payment_method`-from-body field is removed or ignored; payout state machine unchanged except a pre-claim refusal. Exact schema is NOT documented and is an architect deliverable (new ADR; amendment to ADR 0095). Any migration is therefore unplanned.

## 12. Tests / acceptance criteria (for B, derived from the documented requirement)
(a) submit body cannot influence destination; (b) cross-player/cross-tenant instrument refused; (c) unverified/unbound instrument fails closed with no attempt row; (d) destination change after approval is detected/refused (the documented requirement says only "verified, player-bound"; any stronger rule is a decision); (e) audit record with actor/tenant/entity/before-after; (f) RLS and tenant-isolation tests; (g) mutation evidence; security + LF + code-review + QA sign-off; plus ADR 0095 amendment.

## 13. Scope of effect
Both: required before ANY sandbox payout (classification doc) and before real withdrawals. Deposits are unaffected. MOCK is unaffected until a non-MOCK payout adapter is wired. B14-B18 remain sandbox-gated behind it.

## 14. Regulatory / compliance implications already documented
Return-to-source AML control is an OPEN compliance/legal determination (D2). Cash payouts require the cashier identity attestation. Software capability is not licence approval (CLAUDE.md). No jurisdiction-specific requirement is documented.

## 15. Decision statement the owner must approve
> "Before any non-MOCK payout (sandbox included), the payout destination MUST be a player-bound, verified instrument resolved server-side and never supplied by the staff request. The architect is authorised to write the ADR defining instrument storage, verification source and the answers to D1-D4 (incl. whether return-to-source applies, with identity-compliance/legal input), and implementation of B13 starts only after that ADR is accepted. Until then no non-MOCK payout adapter may be wired."

If the owner instead wants another option, it must be specified by the owner/architect; the repository documents none.

## Work not depending on B13 (summary; detail in the registry)
See `docs/HANDOVER.md` section 36a (classification A-E of STANDING-1, RESOLVE-1, ASSET-ECHO-TEST-1, CALLBACK-AUDIT-1, R-5, H-SEC-5, H-SEC-11, ALERT-DELIVERY-1).
