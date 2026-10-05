# K3 implementation security review (record by orchestrator)

Reviewer: security specialist, read-only, branch `prh2-k3-impl @ 041fb55`, private scratch DB, local evidence only (not CI). Not a pen test.
Verdict: **ACCEPT WITH CONDITIONS** (no Critical/High on K3). Conditions PM-S1..PM-S4 required before merge:

- PM-S1 (MEDIUM): no `search_path` pin on 0115 functions; a TEMP table created by the runtime role shadowed `payment_manual_resolutions` and defeated the reserved-namespace guard (reproduced). Fix: pin `pg_catalog, public, pg_temp` on every 0115 function + proconfig catalogue test. -> fixed (Y01, Y02).
- PM-S2 (MEDIUM): ledger-finance F-1 confirming-line union clears `pay_declared_paid_unconfirmed` from another attempt's line. -> fixed (`resolvesTo`, C42b).
- PM-S3 (test): direct UPDATE of non-hash-covered resolution columns must raise MR030 (Z17). -> Y03.
- PM-S4 (test): pinned required count never below the pinned value (Z05). -> Y04.
- Recommended and done: R-2 system-read tightened to M2 kinds; `sqlstate` (code only) in denied audit; Z10/Z16/F-2/F-3 tests.
- Rulings: MR030/MR031 -> 409 acceptable; statement source unwired acceptable for merge (fails closed), real-money precondition; MR020 provider-facing 5xx for casino/sportsbook is LOW today and required before a real casino/sportsbook provider; O-K3 wider than recorded (withdrawal_requests provider_reference/provider_id/state not frozen for acting sessions) - LOW, launch flag; free-text `note` in append-only audit is a PII retention risk (runbook guidance).
- 32 security mutants (Z01..Z32): 16 killed, 16 survived and classified (real gaps Z17, Z05 -> PM-S3/S4; others test gaps, redundant or equivalent).

Delta re-review at `31e4501`: `k3-delta-security.md` (ACCEPT WITH CONDITIONS). Its finding 1 (pre-existing K2, migrations 0112/0113: HIGH, TEMP-table shadow bypasses the two-person adjustment check) is registered as TRIGGER-SEARCH-PATH-1 (HIGH, launch-blocking).

Real-money / launch flags from this review: PAY-K3-STATEMENT-SOURCE-WIRING-1, ALERT-DELIVERY-1, PAY-PAYOUT-DISPUTE-ALERT-1, TRIGGER-SEARCH-PATH-1, PAY-K3-MR020-HTTP-MAPPING-1, O-K3 column discipline, CAS-RECON-SCALE-1, WITHDRAWAL-REVERSAL-1 / psp_clearing residual, a real non-MOCK statement source (PROVIDER DEPENDENT), TM-7, TM-10, HD-PRH2-8, STAFF-LIFECYCLE-1, H-SEC-5. Closed-tenant mechanism stays disabled until HD-CTF-1 and ADR 0107 re-review.
