# Platform-freeze work list (2026-10-16) — provider-independent A-class work remaining before the freeze

Owner priority change (2026-10-16, after the staging IAM ask): **AWS staging is the FINAL infrastructure-validation gate, performed ONLY against a frozen commit after the provider-independent platform is finished and verified.** The staging plan/IaC is kept ready (maintained only). Sequence: finish platform → verify → freeze → AWS staging → final technical readiness → PSP → casino → KYC/AML → sportsbook → sports data → provider conformance → manual brand activation → production.

Rule applied to build this list: an item is IN if it is (a) provider-independent, (b) a security/financial/operability requirement of the platform itself that a reviewer marked "before real money / production" or an owner decision requires, and (c) bounded. Everything else is classified out below.

## IN (implement now; reviewed; then freeze)
| Id | Item | Origin | Agent |
|---|---|---|---|
| F1 | Move `actor_proof_keys` secret out of the backup role's reach (own schema, `--exclude-schema`), migration 0166 | SRBX-3 / R1 (owner decision 5: no actor-proof private material exposed through backups) | key-isolation |
| F2 | Payment-statement stream integrity: sealed backing import for the cursor (SR11-10 → SR11-1), cursor_anomaly alert discriminator (SR11-11), bounded-mode matcher timestamp semantics (LF-A11-1), per-provider settle lag + contract (LF-A11-2/6), H-W1 second half (gate M2 on last run succeeded) | A11 reviews | payments-recon |
| F3 | Payout tombstone follow-ups: pre-existing tombstone bypass of WD027 (SR5R-1), standing finding for a succeeded line on a tombstoned reference (R9) | A5 reviews | payments-recon |
| F4 | Withdrawal RG/risk gate backstop: re-run the gate at payout dispatch and add a DB backstop for WD050/WD051 | A6 SR6 I-2 | withdrawal-gate |
| F5 | KYC residence: carry-forward must break on decided `unverified` and lapsed-expiry rows (SRME-R2-1); alert signal + runbook for `market.residence_into_open_market` | market-evidence reviews | kyc-ops |
| F6 | Ops visibility gaps from the acceptance suite: SA-3 (build/version, migration head, environment anchor via admin API), SA-4 (duplicate `tenant_id` in the kill-switch alert log line), SA-5 (capability PUT silently narrowing) | acceptance suite | kyc-ops |

## OUT (classified; not blocking the freeze)
- **C provider-dependent:** real statement sources/credentials, webhook scheme gaps G-01/G-02 (header-only extract, required signed timestamp), G-03..G-11, KYC-G01 hosted session, casino CAS-G01..10, sportsbook SB-G01..10, sports-data seam, ALERT-DELIVERY-1 (email provider), AML screening vendor, geolocation vendor.
- **D owner/legal decisions:** SRME-4 residence vs physical location, SRME-5 / MKT-WIRE-CREDIT-1 credit and session policy, SRME-6 registration for excluded-market visitors in production, maker-checker for residence determinations (step-up + alertable audit exist), HDR-A6-3 stuck-intent TTL acceptance, RG floor values (OQ-2), market rows content, retention horizon.
- **B/F deferred by owner:** sportsbook until a brand is scheduled, crypto, retail, B2C payout-instrument registration, ADR 0119 S8 lifecycle UI, NR-4/S56-R14, `evidence_ref_hash`.
- **AWS-gated (verified only on the staging run):** IAM/runtime roles, RDS behaviour, logical restore on RDS (likely BLOCKED), PITR timing, alert delivery, SRSP-3 dedicated task role, SRP3-7/8/12, backup runner placement, KMS signing.
- **Production-only hardening:** per-environment signing keys, maker-checker/immutable audit store, load/soak at realistic volume, incident rota, dashboards.

## Finish line (then STOP and report; do not start AWS)
F1–F6 merged with security and ledger-finance review; relevant local suites green; hosted CI green on the frozen commit; frontends verified; backup/restore drill green at the final migration head; runbooks and status docs match the code; final whole-platform production-readiness audit; named freeze commit.
