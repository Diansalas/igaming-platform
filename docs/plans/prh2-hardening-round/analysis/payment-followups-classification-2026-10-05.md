# Remaining payment follow-ups: classification (ledger-finance, read-only)

Repo `claude/focused-wright-jw88w9` @ `d2ef7a6`. Registry = `docs/governance/task-registry.md` (TR:line).
Classes: A = before real-PSP PLANNING; B = before real-PSP SANDBOX integration; C = production / real money only;
D = docs/config only; E = human decision. Nothing below is closed by this note.

Gate premise used for A vs B: ADR 0095 §35.4 B1 gate, PROV-OUTBOUND-CRED-1 tripwire and F-POOL-2 all bind on "the first
non-MOCK adapter / first non-MOCK statement source", and a sandbox adapter IS non-MOCK. So "before the first real PSP"
maps to B. Planning is a paper exercise. No code item has to change before planning can start, so class A is empty.
Planning still needs the E answers and the D corrections listed at the end.

## Table

| Item | Cls | Reason | Evidence | Needed | Owner |
|---|---|---|---|---|---|
| PAY-DEPOSIT-ESCALATION-1 | B | A non-echoing real PSP leaves an intent pending forever and floods audit (48 rows/attempt/day) | TR:4084; `internal/payments/sweeper.go` has no escalate/T16 (grep); Missing audit `poll_evidence.go:95-104` | Deposit T16 + P1 after the settlement window; rate-bound the unconfirmed-poll audit; drain at the Missing reschedule (§36.3); contract rule that QueryStatus success carries amount+asset | payments (LF, security review) |
| PAY-POLL-ECHO-HARDENING-1 | B | Raw, unbounded PSP echo is persisted into append-only audit | `poll_evidence.go:86,95,104`, `receipt.go:160`, `drive.go:562` (raw `provider_asset_code`/`echoed_asset_code`); bare `invalid_provider_reference` missing from `deposit_terminal_reasons.go:29-40` (prefix only) | Format-gate the asset echo, otherwise length+hash; drop or validate the D1-L1 raw-echo fallback bind; LF L1 audit; Z4b pin; add the bare reason | payments (security) |
| PAY-POLL-DECLINED-ALERT-RECON-1 | D | Both engineering parts exist; only delivery remains, and that is ALERT-DELIVERY-1 | (a) raise `alerts.go:41,102`, test `iwire_alerts_integration_test.go:128` (I-wire `dcaa2c6`); (b) `TestD2_12_DeclinedAttempt_SucceededLine_IsStatusMismatch` `internal/reconciliation/prh2_d2_postd1_integration_test.go:134` (`a0a6325`) | Registry status update after the owner confirms; delivery tracked under ALERT-DELIVERY-1 | payments + LF (confirm) |
| PAY-RECEIPT-T4-DRAIN-TEST-1 | B | The callback-path deferred drain has no killing test (mutant D-DRAIN-4 survived) | TR:4085, TR:4076; drain `receipt.go:692`; no commit names it (git log) | One test that fails when the `receipt.go:692` drain is removed, plus a mutant record | payments + qa |
| PAY-PAYOUT-UNBOUND-HOLD-1 | B | Unbound payout parks must provably keep the hold; payout F-C4 and PO-1 text are missing | TR:4096; partial: M2 refuses invalid-ref payouts `manual_resolution.go:104-107,136` (so no operator release) | Test: an unbound-parked payout keeps its hold on every automatic path; reverse-collision test (payout settlement ref = existing deposit key parks, no retry loop); payout-side F-C4; ADR 0095 §35.2 "paid out, completion unposted" | payments (LF) |
| MA020-SYNC-MISMATCH-1 | B | Preventive control for goodwill credits misses 4 mismatch reasons (details below). Not a MOCK money-safety gap | `migrations/0113...up.sql:706-720` (only `multiple_success_for_intent`); called unchanged `0115...up.sql:1669`; TR:4101; TR:4166; ADR 0095 §35.4 (6601-6604), §40.3 (7184) | See the special section | ledger-finance |
| PAY-PAYOUT-REFBIND-1 | B | A valid but foreign payout reference can loop or rebind (the deposit-side LF-6 twin) | `foreignReferenceBinding` used only at `drive.go:515`, `poll_evidence.go:134`; payout binds at `payout.go:606,636,719,752,1032,1046,1079` with no check | Payout LF-6 pre-check → park; scrub the `payoutStatusQuery` invalid branch (~`payout.go:1243`); test + mutant | payments (security) |
| PAY-FPAY-HARDENING-1 | B | F-L2 reports a KYC outage as a KYC requirement on real deposits | `deposit_v2.go:223` `"kyc_required:"+denyReason` for unavailable too | F-L2: distinct retryable reason. F-L1 (outage-rate alert) needs delivery, so it is C under ALERT-DELIVERY-1 | payments (+devops for F-L1) |
| PAY-H-FOLLOWUPS-1 (row) | B | Items 1, 9 and 11 are B; the rest are C or E (below) | TR:4122 | per item | payments + security + LF |
| H (1) SEC-1 status read in claim tx; gate cascade insert | B | Kill-switch/status race on a real dispatch | TR:4122 | In-tx read on `ClaimCreatedForSubmission` + cascade gate | payments |
| H (2) SEC-2 pass budget / SWEEPER-CONCURRENCY-CAPS-1 / jitter | C | Scale and fairness | TR:4122 | Caps + jitter | payments + devops |
| H (3) SEC-4 bounded NotSent backoff | C | Append-only row churn under a long outage | TR:4122 | Backoff/escalation | payments |
| H (4) SEC-5 tenant-status gate on HTTP deposit/payout | C | A suspended real-money tenant still takes new money | TR:4122 | Gate + test; no doc may claim otherwise | payments + security |
| H (5) LF F4 T16 for withheld T12 of non-active tenants | C | Tenant-closure flow (ADR 0107) | TR:4122 | Escalation | payments (LF) |
| H (6) LF F6 false escalation, multi-instance | C | Multi-instance rollout only | TR:4122 | Fix before more than 1 sweeper instance | payments |
| H (7) SWEEPER-TENANT-LIST-SCALE-1 | C | Scale | TR:4122 | Design | payments + devops |
| H (8) brand suspended/closed vs resolution-only | E | Registry: "security to decide" | TR:4122 | Security ruling | security |
| H (9) SEC-8 panic value in GateResult.Err | B | A real adapter panic value (possibly carrying credential or PII) reaches logs/audit | `gate.go:204` `fmt.Errorf("...: %v", r)` | Record the panic type only | payments (security) |
| H (10) LF F7 closed-tenant `created` deposits never terminal | C | No funds; closure flow | TR:4122 | Terminalize | payments |
| H (11) deposit T16, ref-less `submitting` never polled | B | Interface gap: `QueryStatus(ctx, providerReference)` cannot query by merchant ref (`types.go:618`). A crash after a real capture strands the deposit until a callback or recon. Also the ADR 0095 "CP-W1 merchant-reference QueryStatus" crash point | `types.go:613-618` | Decide in planning (adapter capability); implement merchant-ref status or escalation before sandbox | payments (LF) |
| H (12) first-attempt phase C not detached | C | Symmetry only | TR:4122, `deposit_v2.go:~306-308` | Detach | payments |
| H (13) H-SEC-9 no statement/lock timeout | C | Robustness | no `statement_timeout`/`lock_timeout` in `internal/payments` (grep) | Pool/per-tx bound | payments + devops |
| H (14) H-SEC-11 brand status gate | C | Same as (4) | TR:4122 | With (4) | payments + security |
| PAY-K3-FOLLOWUPS-1 (row) | C | All parts are real-money/launch except PAYOUT-DISPUTE-ALERT-1 (B) and note-PII (E) | TR:4140 | per item | payments + LF + security |
| K3 O-K3 column discipline | C | Acting session inside an M2 tx can rewrite `withdrawal_requests.provider_reference/provider_id/state` | TR:4140; `reviews/k3-impl-security.md:11` | Extend column freeze | ledger-finance + security |
| K3 N13 latent Step-B reversal branch | C | No writer and no test | TR:4140 | Scratch-DB test with a writer, or remove | ledger-finance |
| K3 N10 | D | Accepted residual (no money moves) | TR:4140 | Keep documented | — |
| K3 free-text `note` (≤1000 B in append-only audit) | E | PII retention vs. free text is a privacy decision | `reviews/k3-impl-security.md:11` | Owner/DPO: closed codes only, or free text with a retention rule? | human + security |
| PAY-PAYOUT-DISPUTE-ALERT-1 | B | Registry: hard prerequisite before any real payout provider. The raise is NOT IMPLEMENTED | TR:4133; TR:4125 ("payout-dispute alerts NOT IMPLEMENTED"); ADR 0095 §39.4 | Raise sites for payout disputes and T14 (delivery stays C) | payments + devops |
| PAY-PAYOUT-CONTRADICTION-HOLD-1 | C | Hold is kept (money-safe); only the resolution path is missing | TR:4133; `manual_resolution.go:137-140` refused | Resolution design | payments + LF |
| PAY-M3-STAFF-PATH-1 | C | No caller for active tenants | TR:4133 | Staff path before payout go-live | payments |
| LF F-8 / F-9 | C | Second M1 aborts on 23505; stale pending holds the slot. Optional | `reviews/k3-impl-ledger-finance.md:16`; ADR 0101 §27.6 | Closed refusal; lazy expiry in Request | payments |
| CR F-4 / F-5 | C | 409 vs 403; expired pending blocks. Optional | `reviews/k3-impl-code-review.md:9-10` | MR033→403; expire in `requestInTx` | payments |
| Z28/Z29 | C | Optional static route-permission pin | `reviews/k3-qa.md:27` | Static test | qa |
| KYC-PLAYER-SUBMISSION-STATE-1 | C | Optional, KYC not PSP | TR:4140 | — | identity-compliance |
| PAY-K3-MR020-HTTP-MAPPING-1 | C | Casino/sportsbook only, not PSP | TR:4139 | Deterministic provider-protocol rejection | casino + sportsbook + security |
| PAYOUT-AMOUNT-DISPUTE-1 | C | Hold kept; M2 refuses (`manual_resolution.go:104-105,134-135`); partial-payout design missing | TR:3936; `payout.go:1181` | Partial-payout accounting design (LF) + implementation | payments + ledger-finance |
| WITHDRAWAL-REVERSAL-1 (+ psp_clearing) | C | The `withdrawal_reversed` transition is not implemented. A compensating entry leaves `psp_clearing` misstated (tracked by standing kinds) | TR:3937; `withdrawal.go:1538` ("not-yet-implemented"); ADR 0095 §39.4:7150 | Transition + posting + recon clearing | ledger-finance + payments |
| PAY-SEC-LAUNCH-1 | B | Destination binding is needed to dispatch ANY sandbox payout. S-L1/S-L3/S-L4 are C | TR:4062; `docs/plans/payment-readiness/prh-i1-payout-launch-conditions.md:74-84` | Design in planning; verified player-bound destination in `WithdrawRequest` before sandbox payouts; S-L1/3/4 before real money | payments + security |
| PAY-CALLBACK-MISMATCH-BIND-1 | B | The callback T10 still does not bind; it matters for MA020 keying | `receipt.go:824-826` (no bind) | Bind the validated, non-conflicting ref at the callback T10 and in `applyMultipleSuccessDispute` | payments (LF) |
| PAY-PSP-CONTRACT-INVDEP1 | D | Vendor-selection criteria; goes into the planning brief | TR:4072; ADR 0095 §34.4 | Add to the planning/RFP doc; also add R-S3 (statement must carry `merchant_reference`) and H(11) | payments |
| PRH-I1-MANIFEST-2 | B | LF-C1 401/5xx redelivery semantics per real adapter | `contract.go:139-140` "deliberately still NOT modeled" | Model and record for the sandbox adapter | payments (LF) |
| PRH-I1-MANIFEST-3 | B | No vendor-code→ErrorClass table | same | Model with the first adapter | payments |
| PRH-I1-MANIFEST-4 | D | Likely superseded by the coverage gate `checkPaymentStatementCoverage` (ADR 0095 §40.1:7163) | TR:4045 | Architect to confirm supersession | payments + architect |
| PROV-OUTBOUND-CRED-1 precondition | B | Tripwire must be deliberately relaxed for the sandbox adapter | `cmd/platform-api/outbound_precondition_test.go:30`; payments credential via `gate.go:145-152,207-209`; out-of-tx F-POOL-2 payments done | Security sign-off and tripwire relaxation scoped to the payments domain | payments + security |
| F-POOL-2 (payments) | B | Payments part IMPLEMENTED against MOCK; per-domain closure before the first non-MOCK adapter; LF-C1 = MANIFEST-2 | TR:3956; TR:4143 (9) | Architect re-status; LF-C1 for the sandbox adapter | architect + security + LF |
| CAS-RECON-SCALE-1 | C | Scale of the recon sweep | TR:3894 | Design + ADR | LF + architect + devops |
| PAY-P1-MULTISUCCESS-ALERT-1 | C | Durable raise exists; delivery = ALERT-DELIVERY-1 + HD-PRH2-4-OPS | `alerts.go:113`; TR:4143 (4) "subsumed" | Delivery | devops + payments |
| PAYWH-RL-1 | D | Appears implemented by PRH-I4. Row still says "Deferred" | TR:3908; ADR 0097:1 title, :1104 "closed by this implementation, pending security §15 review"; PRH-I4-SECREVIEW-1 CLOSED (TR:4013); `internal/admission/*` | Security/payments confirm, then status update | security + payments |
| WEBHOOK-EDGE-1 | C | Edge rate control, infra | TR:4023 | WAF/CDN rule or per-tenant path | devops + security |
| DEVOPS-0107-INDEX-WINDOW-1 | C | Production-size ledger only | TR:4065 | Window or CONCURRENTLY procedure | devops + LF |
| H-W1 residual | C | After the option is implemented: "registered = scheduled, not last-run-succeeded" (a per-tenant fetch failure leaves M2 unlocked without the detective control) and closed-tenant funds (ADR 0107) | ADR 0095 §40.1:7165,7168; TR:4167 | Refuse M2 when the last run for (tenant, provider) did not succeed, unless the workstream covers it | payments + LF (owner decision) |
| R-S1 (removing a source silently ends findings) | C | Operational config risk | ADR 0095 §40.1:7168, §40.2:7175 | Startup check vs disputed attempts, or runbook rule | LF + devops |
| R-S2 (non-active tenants not swept) | E | = H-W1 | §40.2:7175 | H-W1 | human |
| R-S3 (unbound coverage needs statement `merchant_reference`) | D | PROVIDER DEPENDENT vendor criterion | §40.2:7175 | Add to PAY-PSP-CONTRACT-INVDEP1 | payments |
| R-S4 (`reversed` line clears in-run only) | C | Noisy, never silent | §40.2:7175 | Persist-based clear | LF |
| Cancelled/expired-intent variant untested | C | Rests on the code path (matcher never reads intent state) | §40.2:7174 | Fixture + test | LF + qa |
| D-5 guard-list coverage test | B | Must be extended with the first real or sandbox statement source | `cmd/platform-api/payment_statement_wiring_test.go:317`; ADR 0095 §40.3:7186 | Build a real-source configuration in the test | payments + security |
| Real statement source per real adapter (from HANDOVER) | B | The startup coverage gate refuses a non-synthetic adapter without its own source | ADR 0095 §40.1:7163 | Sandbox statement source with the same provider id | payments (PROVIDER DEPENDENT) |
| B1 gate item 2: DELIVERED P1 before any real PSP adapter (ADR 0095 §35.4:6583-6597) | E | Read literally, it blocks a sandbox adapter until ALERT-DELIVERY-1 closes | §35.4:6597 "gate stays CLOSED" | Ruling (see E list) | human + LF |

## MA020-SYNC-MISMATCH-1 (special)

What exists today:
- `player_open_payment_exposure` (`migrations/0113_governed_manual_adjustments.up.sql:706-720`) returns true only for
  deposit attempts that are `disputed` with `multiple_success_for_intent`, cleared only by a tombstone on
  `(provider_id, provider_reference)`.
- A NULL reference never clears, so the exposure stays permanently true. This is the "MA020 stranding" in ADR 0095 §39.4:7150.
- 0115 calls the function unchanged, adding only the Step-B exemption (`0115...up.sql:1669`). No later migration redefines it.

What the item still requires (TR:4101, ADR 0095 §35.4:6601-6604, §40.3:7184, TR:4166):
1. Widen the reason set to `sync_amount_mismatch`, `poll_amount_mismatch`, `poll_reference_mismatch` and
   `callback_amount_asset_mismatch` (widened at D1, TR:4095). Add `provider_reference_conflict` when it is referenced.
2. Apply the D2F-1 runtime rule: evaluate the attempt row like `captureClass` does (`payment_statement.go:248-257`). A
   reason counts as bound only when `provider_reference` is set. MA020 must not report something that cannot clear.
   - This changes the current NULL→true behaviour, so LF must rule explicitly on the ref-less case: excluded (which
     removes the stranding) or kept.
   - PAY-CALLBACK-MISMATCH-BIND-1 shrinks the ref-less population.
3. Clear by a tombstone on X. For `poll_reference_mismatch`, a tombstone on Y also clears, but only when Y is attributable.
4. It must reuse the existing rules:
   - `yAttributable`, including the shared-Y rule (`payment_statement.go:1244-1269`).
   - G-Y2 (`:1214-1215`): when eligible persisted succeeded deposit lines evidence both X and Y, tombstones on both are
     required.
   - These are Go functions over a run snapshot while MA020 is SQL inside the DB refusal, so "reuse" means a SQL port
     plus a parity test, not a call.

Is it a money-safety gap today against MOCK? No.
- A mismatched sync capture cannot post. `parkDepositAttempt` never posts (`drive.go:362-420`; reasons documented
  "Nothing posted" at `drive.go:345-359`), and the attempt is terminal `disputed`.
- MA020 cannot clear a recon finding wrongly, because it is not a clearing input. `pay_captured_unposted` clears only on
  a reversal line or a tombstone (`payment_statement.go:1191-1218, 1288-1295`), and M1 only acknowledges.
- The gap is preventive only. A four-eyes-approved goodwill `credit_player` adjustment for a player with an open
  sync/poll/callback mismatch park is not refused, so staff could hand-pay a capture that the PSP may later settle or
  reverse.
- That needs real funds to matter. The standing finding keeps it visible meanwhile.
- Class B, because §35.4 and HANDOVER bind it "before the first real PSP". Sandbox is where these parks first become
  reachable outside fixtures.

Smallest fix:
1. One new migration (number allocated by the orchestrator) that does `CREATE OR REPLACE FUNCTION
   player_open_payment_exposure` with a pinned `search_path` (TRIGGER-SEARCH-PATH-1 discipline). It:
   - widens the reason set;
   - applies "reference IS NOT NULL" per the D2F-1 rule, plus the LF ruling on the ref-less case;
   - clears on a tombstone on X;
   - for `poll_reference_mismatch` with a `payment_attempt_reference_evidence` Y row, also clears on a tombstone on Y,
     only if Y passes the SQL port of `yAttributable` (no other attempt holds Y as provider or settlement reference, no
     other park's Y row equals Y, no `deposit`/`withdrawal_completed`/`deposit_reversal` ledger key = Y);
   - when both X and Y have eligible succeeded deposit lines in `payment_statement_lines` (D-4/RC-3 `is_mock`
     eligibility), requires tombstones on both (G-Y2).
2. Tests:
   - the B23/B25 matrix (`internal/adjustment/b23_b25_integration_test.go`) extended per reason, with and without a
     reference;
   - a Go-vs-SQL parity test that drives the §40.3 Y fixtures (held by posted attempt, held by unposted attempt,
     `withdrawal_completed` key, `deposit_reversal` key, tombstone on Y, other tenant, shared Y, both evidenced) through
     `yAttributable` and the SQL function;
   - mutants on the reason list, the Y-attributable predicate and G-Y2;
   - Step-B exemption regression (`TestK3_C48_MA020ExemptionTrio`).

## (1) Class A / B short list

- A (blocks starting real-PSP PLANNING): none in code. Planning needs the E answers below and the D corrections. Planning
  must decide these contract-shaping items:
  - H(11): merchant-reference status query;
  - PAY-SEC-LAUNCH-1 destination binding;
  - PRH-I1-MANIFEST-2/3;
  - PAY-PSP-CONTRACT-INVDEP1 criteria, including R-S3.
- B (blocks SANDBOX integration):
  - PAY-DEPOSIT-ESCALATION-1
  - PAY-POLL-ECHO-HARDENING-1
  - PAY-RECEIPT-T4-DRAIN-TEST-1
  - PAY-PAYOUT-UNBOUND-HOLD-1
  - MA020-SYNC-MISMATCH-1
  - PAY-PAYOUT-REFBIND-1
  - PAY-FPAY-HARDENING-1 (F-L2)
  - PAY-H-FOLLOWUPS-1 items 1, 9, 11
  - PAY-PAYOUT-DISPUTE-ALERT-1 (raise)
  - PAY-SEC-LAUNCH-1 (destination binding)
  - PAY-CALLBACK-MISMATCH-BIND-1
  - PRH-I1-MANIFEST-2/3
  - PROV-OUTBOUND-CRED-1 tripwire relaxation
  - F-POOL-2 payments closure (LF-C1)
  - D-5 guard-list extension
  - a sandbox statement source per adapter
  - plus the B1-gate ruling (E)

## (2) Human decisions (E)

1. B1 gate (ADR 0095 §35.4 item 2): "May a sandbox PSP adapter be enabled for a synthetic, non-real-money tenant before
   ALERT-DELIVERY-1 delivers P1s, or does the gate's 'first real PSP adapter for any tenant' include sandbox?" (LF can
   draft an ADR amendment. The owner decides.)
2. H-W1 / R-S2: "Refuse M2 on tenants the statement sweep does not cover (option 1), or sweep `payment_statement` for
   non-active tenants (option 2)?" (Stated as being implemented. Confirm which option was authorized.)
3. PAY-H-FOLLOWUPS-1 (8): "Must a suspended/closed BRAND be gated like a non-active tenant in resolution-only sweeping?"
   (security ruling)
4. K3 `note`: "Keep free-text notes (≤1000 B) in the append-only audit, or restrict to closed reason codes for PII
   retention?"
5. Standing: PSP/vendor selection (PAY-PSP-CONTRACT-INVDEP1 criteria) and HD-PRH2-4-OPS recipients (for C-class
   delivery).

## (3) Stale or apparently satisfied (flag only, not closed)

- PAY-POLL-DECLINED-ALERT-RECON-1: (a) and (b) both evidenced (`alerts.go:41`, `TestD2_12...` at
  `prh2_d2_postd1_integration_test.go:134`). Only delivery remains.
- PAYWH-RL-1 (TR:3908, TR:3825 "Deferred"): ADR 0097:1104 and PRH-I4-SECREVIEW-1 (TR:4013) indicate it is implemented
  and approved. Needs security confirmation (REGISTRY-HYGIENE (10)).
- PAY-P1-MULTISUCCESS-ALERT-1: "log line only" is stale. A durable raise exists (`alerts.go:113`). It is subsumed by
  ALERT-DELIVERY-1 (TR:4143 (4)).
- HANDOVER.md:79 still lists PAY-RECON-POLL-REF-CLEAR-1 + PARKED-CAPTURE-STANDING-1 as the hard prerequisite.
  - Both were CLOSED as engineering items against MOCK (TR:4166, R2-F merge `1138013`).
  - The real-PSP residuals are R-S1..R-S4, D-5 and the sandbox source.
  - HANDOVER.md:111 likewise.
- PAY-CALLBACK-MISMATCH-BIND-1: its either/or partner STANDING-1 has landed against MOCK (ADR 0095 §35.4:6577-6582,
  TR:4166). The reconciliation hole it targeted is closed for MOCK. Its remaining value is MA020 keying and attribution.
- PROV-OUTBOUND-CRED-1 (TR:3890) "Not implemented: passing tenant and resolved credential through adapter request
  types / outbound calls inside the domain tx" is stale for payments:
  - `CallContext` carries the tenant and resolved credential on ctx (`gate.go:145-152,207-209`);
  - `PaymentProvider` methods still take no explicit `CallContext` (`types.go:613-618`), which is a planning interface
    choice;
  - F-POOL-2 payments is done. The tripwire still stands.
- PRH-I1-MANIFEST-4: probably superseded by `checkPaymentStatementCoverage` (§40.1). Architect to confirm.
- WITHDRAWAL-REVERSAL-1 cites `withdrawal.go:1492-1494`. The text is now at `withdrawal.go:1538` (line drift only).
- PAY-K3-FOLLOWUPS-1 sub-items PAY-PAYOUT-DISPUTE-ALERT-1 / CONTRADICTION-HOLD-1 / M3-STAFF-PATH-1 still have no own
  rows (TR:4143 (12)).
