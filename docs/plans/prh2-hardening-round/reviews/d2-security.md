_Reviewer: `security`. Branch `prh2-d2-recon-parked-capture` @ `61524df`. Recorded verbatim by the orchestrator._

> **Orchestrator ruling on D2-L1 (2026-10-03):** security notes that the classification ignores the attempt's operation, so a disputed PAYOUT parked with `invalid_provider_reference:<reason>` plus a succeeded payout line is reported as `pay_captured_unposted`. Ledger-finance ruled earlier (`d2-ledger-finance-rereview.md`, PO-1/PO-2) to KEEP that behaviour: the exposure is real (the PSP says it paid out, the platform posted no completion) and restricting it would turn a loud finding into a silent one. Security itself calls it loud, not hidden, and non-blocking. **The LF ruling stands; no pre-merge change.** The semantic mismatch of the finding name is handled by documentation (PO-1: ADR 0095 §35.2 defines the payout meaning) and by registering an explicit payout decision under PAY-PAYOUT-UNBOUND-HOLD-1. D2-L2 (unclassified reason fails quiet) and D2-L3 (explicit tenant predicates not independently pinned) are registered as optional hardening, not pre-merge.

Security review — PRH-2 D2 (PAY-RECON-PARKED-CAPTURE-1 + D2-1 merchant cross-check); branch prh2-d2-recon-parked-capture @ 61524df7b22a4ce4d4a3ec1156e74ee232c8f411; diff vs main 6836319

VERDICT: ACCEPT. Nothing is pre-merge. There are three Low items (D2-L1..L3), none blocking. The known, registered items were not re-raised.

Method:
- Disk: 7.2G free at the start, 6.5G at the end; it never approached the 3G floor.
- Fresh private DB sec_d2_rv_20261003. DB access worked. Dropped WITH (FORCE); verified 0 left.
- One git archive, built once; go vet clean. Export, logs and scripts deleted.
- No role or credential changes, no shared cache or directory touched, pipefail on, grepped for FAIL.
- `-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/reconciliation/...`: ok (reconciliation 89s, statement 1s), no DATA RACE.
- 4 mutants on internal/reconciliation/payment_statement.go, each reverted and cmp-verified identical.

------------------------------------------------------------------
Mutants
- Ma, merchant cross-check removed (the checkMerchantAttribution call in matchPayment): KILLED by TestD2_7, TestD2_8, TestD2_9 and TestD2_9b.
- Mb, an unclassified reason defaults to reasonBound: KILLED by TestD2_P1_UnknownReasonIsUnclassified and TestD2_6_ExistingReasonsUnchanged.
- Mc, clearing widened (any deposit_reversal line in the run clears every reference): KILLED by TestD2_3_BoundPark_ClearsOnlyOnReversalLineOrTombstoneOnBoundRef.
- Md, the attempts-query tenant predicate replaced with a no-op (`WHERE a.tenant_id = $1` around line 778): SURVIVED, and this is EQUIVALENT, accepted. The stream runs inside db.Pool.WithTenantSnapshot (a tenant-scoped REPEATABLE READ transaction; payment_statement.go ~581/587), and payment_attempts is FORCE ROW LEVEL SECURITY (0101:338). RLS alone confines the read to the tenant, so the explicit predicate is defence in depth: keep it.

------------------------------------------------------------------
Focus items
1. Tenant isolation: HOLDS.
   - byMerchant, byRef and bySettlement are built only from loadPlatform's attempts query (`a.tenant_id = $1 AND a.provider_id = $2`) inside the tenant snapshot transaction. checkMerchantAttribution resolves `m.byMerchant[l.merchant]` from that same per-tenant, per-provider map and compares attempt IDENTITY (b == a / b.operation), never strings.
   - validatePaymentLine refuses a line whose provider_id is not the source's provider (INV-IO-14).
   - So a crafted merchant reference can only name an attempt of the same tenant and provider, or nothing. It cannot read, flag or leak anything in another tenant.
   - "names no platform attempt of this provider" carries only the platform's own merchantRef, a's render (platform ids and state) and l.render(). l.render() holds only validated line fields (providerref-valid, merchant at most 64 bytes, no control characters), exactly as before D2.
   - Hiding a real finding: no. The cross-check is additive. a's own checks run unchanged, and b is deliberately not recorded in matchedBy, so b's own line still matches normally.
   - False finding: a hostile or erroneous statement can name another same-tenant attempt b and produce pay_reference_mismatch, plus pay_captured_unposted if b is an unbound park and the line is succeeded. That is a loud, detection-only false positive: the statement is the provider's own claim, nothing is mutated, and investigation resolves it. Accepted.
2. Information disclosure: CLEAN.
   - The new detail fragments are `terminal_reason=<reason>` and `attempt=<uuid>`. terminal_reason is read from the platform's own payment_attempts row and comes from a closed platform vocabulary; the only parameterised family is `invalid_provider_reference:<closed providerref reason>`.
   - No raw provider reference, raw echo or provider free text is added. Line references reach the detail only after providerref validation, as before.
   - An invalid statement line fails the run with an error that wraps the providerref error (field, reason, length, hash prefix: never the value) or a fixed message such as "merchant_reference must be at most 64 bytes" (validatePaymentLine 377-399). The run-level audit path is unchanged by D2.
3. disputeReasonClasses: no prefix confusion.
   - terminal_reason is written only by the platform; providers never supply it. Only the exact `invalid_provider_reference:` prefix is treated as a family, and it maps to unbound, which is in-run only and never suppresses anything.
   - RULING on the silent default for an unclassified reason: ACCEPTED for merge, with a hardening recommendation (D2-L2). It is safe today because TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified iterates payments.DepositDisputeTerminalReasons() and fails the build for any unclassified deposit reason, and the pre-D2 behaviour for every non-T13d dispute was already "no finding".
4. Fail-closed (no new false negatives):
   - The in-run unbound rule clears only on capturedUnpostedRef(l.ref). l.ref is always non-empty (providerref.Validate is mandatory on provider_reference), and reversalOriginals only records non-empty originals, so no empty-key clearing is possible.
   - The standing and in-run bound rules clear only on the bound reference (capturedUnposted(a)).
   - A cleared case falls through to the pre-existing "disputed: already a payments P1" branch, the same as before D2.
   - No previously reported finding is suppressed. multiple_success_for_intent keeps identical behaviour (TestD2_6).
5. No new write path: CONFIRMED. The D2 diff touches only matching logic in payment_statement.go (classification, matchPayment cases, checkMerchantAttribution, capturedUnpostedRef) and adds only `m.r.add(...)` findings. There is no new SQL write. Runs stay in the tenant snapshot transaction.

------------------------------------------------------------------
LOW (not pre-merge)

D2-L1: the classification ignores the attempt's operation. payment_statement.go boundCapture()/unboundPark() (~245-256) gate only on state and terminal_reason, but payouts also write `invalid_provider_reference:<reason>` (payments/payout.go:494-497). A disputed PAYOUT with that reason, plus a succeeded payout line resolving to it, is now reported as pay_captured_unposted. It is a real exposure (the PSP says it sent money the platform never completed), so it is loud rather than hidden, but "captured" is a deposit term and the runbook semantics differ. Fix: gate the classes on `a.operation == "deposit"`, and give payouts their own explicit decision (a payout class, or the pre-D2 silent P1), pinned by a test.

D2-L2: unclassified reason at run time. Recommended: instead of no finding, make an unclassified disputed reason loud, e.g. a pay_status_mismatch (or a dedicated check) with "unclassified terminal_reason", or a run-level P1. That would cover legacy rows written before the table existed, and any write site that bypasses DepositDisputeTerminalReasons (such as the payout reasons in D2-L1). Not pre-merge, given the pin test.

D2-L3: Md is equivalent only because of RLS. The explicit tenant predicates in loadPlatform's attempt and ledger queries are not independently pinned. If the stream is ever moved to a broader session shape (platform service), nothing would catch a dropped predicate. Optional: a static or catalogue assertion that the payment-statement stream runs only under WithTenantSnapshot, or a two-tenant, same-provider, same-merchant-reference cross-tenant test at the D2 matcher level (the existing payment_statement cross-tenant test predates the merchant cross-check).

Scope: code- and DB-level review of the D2 delta; no penetration test. ADR 0095 §35 was read for intent only. The 33-mutant evidence was sampled (4 run here), not re-run in full. Known items PAY-RECON-PARKED-CAPTURE-STANDING-1, PAY-RECON-POLL-REF-CLEAR-1, PAY-POLL-DECLINED-ALERT-RECON-1, MA020-SYNC-MISMATCH-1 and ALERT-DELIVERY-1 remain as registered.

Relevant paths (commit 61524df):
- internal/reconciliation/payment_statement.go (disputeReasonClasses / classifyDisputeReason ~170-256; validatePaymentLine 377-399; loadPlatform ~770-810; matchPayment ~1000-1080; checkMerchantAttribution ~1083-1125; capturedUnpostedRef ~1150)
- internal/reconciliation/payment_reason_classification_test.go
- internal/reconciliation/prh2_d2_*_integration_test.go
- internal/payments/payout.go (lines 494-497)
