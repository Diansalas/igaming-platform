# Final closure list for the provider-independent platform freeze (2026-10-17)

- Basis: integration branch `claude/focused-wright-jw88w9` at `76187cc` (migration head 0170). Read-only analysis; no code was changed.
- Sources read: `docs/plans/platform-freeze-report-2026-10-17.md` (section 6), `platform-freeze-work-list-2026-10-16.md`, `final-platform-audit-record.md`, the whole of `docs/governance/task-registry.md` (later rows supersede earlier ones), `docs/governance/human-decision-register.md`, `docs/HANDOVER.md` sections 29-33, `docs/plans/a11-payments-binding-recheck.md` and the per-task records `docs/plans/a3..a13, f1..f6, t8, market-evidence` records.
- Method: every row that says OPEN / NOT IMPLEMENTED / "before real money / PSP / casino adapter" was checked against later registry rows, migrations and code. Rows that a later MERGED/CLOSED row or the code closes are listed only in Appendix A (not in the main table).
- Nothing here is a legal approval or a launch authorisation. Labels: IMPLEMENTED / PARTIALLY IMPLEMENTED / MOCK / PROVIDER DEPENDENT / NOT IMPLEMENTED.

## Classes

| Class | Meaning |
|---|---|
| A | Must close before technical freeze: provider-independent, implementable now, bounded |
| B | Explicit accepted residual (rationale, mitigation and existing detection stated) |
| C | Provider-dependent (needs a real PSP / casino / KYC / AML / sportsbook / sports-data / email / geo vendor or contract) |
| D | AWS-dependent (only verifiable on real infrastructure) |
| E | Legal / compliance / manual human decision |

## 1. Summary

| Class | Items in main table (rows) | Of which IN FLIGHT |
|---|---|---|
| A | 20 | 14 (the in-flight set below) |
| B | 33 | 0 |
| C | 14 | 0 |
| D | 14 | 0 |
| E | 22 | 0 |

(Counts are of table rows; a row may bundle closely related registry IDs, named in the row.)

### 1.1 IN FLIGHT (class A, other agents; not re-analysed)

F4 forged allow row SF4-1/R1 + F4-R2-NEGBAL-SQL + SF4-5 (rows IF-1..IF-3); `TODO(jurisdiction)` in payments/casino/risk (IF-4); U-5 frontend money floats (IF-5); U-6 test-support `GuardEnvironment` (IF-6); U-7 mutating-staff-route audit static test (IF-7); SRF2R-1 / SRF2R-2 / SRF2R-3 (IF-8..IF-10). IF-11..IF-14 are the sub-items those agents carry (SF4-3 wording is class E, below).

### 1.2 NEW class A items beyond the in-flight set (my finding)

| Id | Proposed owner agent type | Scope (bounded, no vendor) | Migration? |
|---|---|---|---|
| NA-1 | `security` implementer + `backend` | PANIC-LOG-VALUE-1, REDACTEDREASON-PASSTHROUGH-1, SAFECALL-LOG-GUARD-1: log the panic TYPE only at the 5 sites that log the whole panic value; give `redactedReason` an allow-list of own sentinels with a generic label otherwise; add a static guard that bans logging a recovered value | none |
| NA-2 | `payments` + `ledger-finance` review + `security` review | PAY-DEPOSIT-MISMATCH-ALERT-1 (registry "HARD GATE before any real-provider deposit"): raise-only P1 for a mismatched success on a DECLINED deposit (capture without credit) and a separate reason for a SUCCEEDED deposit, as ADR 0095 s42.8 design points; flip `deposit_mismatch_pin_integration_test.go` deliberately | none (existing Kind, closed reason set) |
| NA-3 | `casino` + `security` review (coordinate with the `jur` agent: same files) | CAS-HEALTH-FAILOPEN-1 (a `HealthStatus` error must count as unhealthy, fail closed) and CAS-JURIS-AUDIT-1 (write a `casino.launch_denied_by_jurisdiction` audit row on a jurisdiction-blocklist denial, like the RG and risk denials) | none |
| NA-4 | `bonus` + `ledger-finance` | BONUS-SWEEP-WATERMARK-SKEW-1: write the failing two-interleaved-deposits test first, then a trailing safety margin or bounded re-scan so a late-committing deposit is not skipped permanently | none |
| NA-5 | `devops` / `qa` | Acceptance suite SA-3 wiring: `deploy/aws/acceptance/checks_auth.go:327,330` still `skipManual` ("no build/version endpoint", "no HTTP surface reports the migration head") although `GET /v1/admin/platform/status` exists; wire both checks to it and test offline | none |
| NA-6 | `backend` (`db`) | ENV-ANCHOR staging-under-production: `VerifyEnvironmentAnchor` (`internal/db/app_environment.go:55-65`) refuses anchors `development`/`test`/empty only; a production process on a `staging`-anchored database starts (registry ENV-ANCHOR-REFUSAL-2026-10-13 residual). Refuse any anchor other than `production` under the production guard | none |
| NA-7 | orchestrator (docs only) | Docs truth residuals after the freeze report (see row A-DOCS-1): HANDOVER sections 29-33 and `human-decision-register.md` informational rows still list closed items as open (CI billing, MA020-K2/STMT-TABLE, CAS-GAME-KILL-BET-1, idle-in-transaction timeout); `docs/runbooks/README.md` lines on `staff-mfa-s3-cutover.md` ("until S6 exists") and `test-database-hygiene.md` ("reaper NOT IMPLEMENTED"); one superseding registry row for Appendix A | none |

Not proposed as A after checking (so the caller need not re-ask): NULL-ARM-WRITE-1 (ADR 0110 s8 shows a restrictive policy cannot close it; identity-store hardening T3/H1 is an owner decision, class E/B), PLAT-AUDIT-SUBJECT-1 (only three tenant-less platform audit rows in `internal/providercred/service.go:344,412,841`; records exist; tenant visibility only, class B), BRAND-DELETE-REHOME-1 and BRAND-CLOSED-BALANCE-CHECK-1 (registry class B, no funds can move), PAY-RECON-D2-HARDENING-1 (pin test exists, class B), KYC-OUTBOX-REQUEUE-1 (a feature with migration and review chain, not bounded, class B), AML-HOLD-RATE-ALERT-1 / AML-NOT-ENFORCED-ALERT-1 (need alert kinds and delivery, controls fail closed, class B with named pre-launch gate), ALERT-KINDS-DEDICATED-1 (class B).

## 2. Class A

| Id | Description | Sev | Current evidence | Class |
|---|---|---|---|---|
| IF-1 | SF4-1/R1: runtime credential can insert a forged `withdrawal_gate_decisions` allow row (signer outside runtime reach per ADR 0110, or accept plus scheduled re-evaluation job) | Medium | row `F4-WITHDRAWAL-GATE-BACKSTOP-MERGED-2026-10-17`; `docs/plans/f4-withdrawal-gate-backstop-record.md` | A, IN FLIGHT |
| IF-2 | F4-R2-NEGBAL-SQL: negative-balance block only in the application gate, not in SQL | Medium | same row | A, IN FLIGHT |
| IF-3 | SF4-5: silent fail-closed at dispatch T2/T12 (no operator signal) | Low | same row | A, IN FLIGHT |
| IF-4 | `TODO(jurisdiction)`: `internal/payments/orchestrator.go:161,175`, `types.go:380`, `internal/casino/launch.go:185`, `orchestrator.go:1979`, `internal/risk/evaluator.go:98` | Medium (before a 2nd jurisdiction) | `final-platform-audit-record.md` s6 | A, IN FLIGHT |
| IF-5 | U-5: frontend money as floats `b2c/src/lib/money.ts:35`, `backoffice/src/lib/money.ts:22`, `PaymentCapabilityForm.tsx:63-64` | Low | audit s4 | A, IN FLIGHT |
| IF-6 | U-6: test-support route flags (`cmd/platform-api/main.go:518-535`) do not use `GuardEnvironment()` | Low | audit s1, ADR 0085 | A, IN FLIGHT |
| IF-7 | U-7: no static test that every mutating staff route writes an audit record | Low | audit s5 | A, IN FLIGHT |
| IF-8 | SRF2R-1: forged sealed-looking run rows make cursor read / M2 gate read slow (SQL-only DoS) | Low-Med | `f2-payments-recon-integrity-record.md`; row F2-F3 | A, IN FLIGHT |
| IF-9 | SRF2R-2: `match_stalled` false P1 after ordinary instance contention | Low | same | A, IN FLIGHT |
| IF-10 | SRF2R-3 / residual 6.10: `payment_attempt_open_exposure` trusts unsealed `deposit_reversal` lines (preventive layer only) | Medium | same | A, IN FLIGHT |
| IF-11 | MA020 statement/exposure items carried with SRF2R-3 (planted statement line disables the manual-credit block) | Medium | same | A, IN FLIGHT |
| IF-12 | SF4-6 insider controls on gate inputs unverified, SF4-7 HTTP test lacks IP/UA/request-id assertion (F4 info items) | Info | F4 row | A, IN FLIGHT |
| IF-13 | R3 status race window in the F4 dispatch gate | Low | F4 row | A, IN FLIGHT |
| IF-14 | SF4-4 lease rewrite unguarded | Info | F4 row | A, IN FLIGHT |
| NA-1 | Panic value logged verbatim; `redactedReason` echoes `err.Error()` for non-transport errors | Medium | `internal/httpserver/middleware.go:115`, `webhook_admission.go:427,607`, `internal/bonus/schedulers.go:316`, `internal/rg/enumeration_sweep.go:209`; `internal/payments/gate.go:243-252`; rows CLASSB-B1-B2 (PANIC-LOG-VALUE-1 etc.) | A |
| NA-2 | PAY-DEPOSIT-MISMATCH-ALERT-1 not built | Medium | ADR 0095 s42.8 ("HARD GATE before any real-provider deposit"); `internal/payments/deposit_mismatch_pin_integration_test.go:3-6`; payout twins exist in `payout_alerts.go:63-67` | A |
| NA-3 | CAS-HEALTH-FAILOPEN-1 and CAS-JURIS-AUDIT-1 | Low-Med | `internal/casino/orchestrator.go:633` (`err == nil &&` treats an error as healthy), `:410-415` (denial without audit); rows after PRH-I2 review (RV-PRH-I2 I1, I2) | A |
| NA-4 | Bonus deposit sweep watermark `(posted_at, id)` can skip a late-committing deposit permanently | Low-Med | `internal/bonus/deposit_sweep.go:105-121`; `ledger_transactions.posted_at DEFAULT now()` (`migrations/0021:31`, transaction start); row BONUS-SWEEP-WATERMARK-SKEW-1 | A |
| NA-5 | Acceptance-suite SA-3 checks still SKIP | Low | `deploy/aws/acceptance/checks_auth.go:327,330`; row F5-F6 ("wire at the freeze") | A |
| NA-6 | Production start accepts a `staging`-anchored database | Low | `internal/db/app_environment.go:55-65` | A |
| NA-7 | Residual stale status text (see Appendix A and A-DOCS-1) | Info | `docs/HANDOVER.md` s29-s33; `docs/runbooks/README.md` | A |

## 3. Class B (accepted residual)

Format of the last column: rationale / mitigation / detection that exists.

| Id | Description | Sev | Evidence | Class | Rationale, mitigation, detection |
|---|---|---|---|---|---|
| B-1 | SF4-3: T2/T12 escalation is not sticky (reactivating an account releases a parked payout) | Low-Med | F4 row | B | Wording item; owner question (E-row E-12). Gate re-runs at every dispatch; denial audited; alert `withdrawal.payout_gate_denied` |
| B-2 | T7: physical backups/snapshots/PITR contain the actor-proof key; owner/superuser/restore loader can read `actorproof_private` | Medium | row F1 (migration 0166) | B | Inside AWS boundary by design; logical dumps exclude it (`--exclude-schema`, TOC assertion); mis-grants detected at backup time and by probes; re-key script `rekey-actor-proof.sh` |
| B-3 | THREAT-MODEL-ARBITRARY-SQL-1 residuals T3/H1 (identity store), T4 (keyed credential-token hash), T5/E10 (tombstone-branch hold release), T8 for approve/reject and other dual-control flows, T6 | Medium | `human-decision-register.md` NEEDS USER; ADR 0110 s8-s10a; row T8 | B | Owner risk acceptance in register; four-eyes flows K1/K2/K3/HSEC/reverse rows are proof-bound; genuine proofs need the app key; detection by reconciliation and audit |
| B-4 | NULL-ARM-WRITE-1: NULL-tenant write arms on `staff_users`, `risk_rules`, `player_restrictions`, `sessions`, `audit_log`, `persons` | Medium | row NULL-ARM-WRITE-1-REVIEW; ADR 0110 s8 | B | Security verdict hardening not launch blocker; restrictive policy cannot stop an arbitrary-SQL attacker; proof guards close the in-database chain; Go discipline; audit rows |
| B-5 | NR-4 / S56-R14: one Person with two live platform accounts via raw SQL | Low | row ADR0119-S5S6-NR4-DECISION | B | Deferred by owner/orchestrator; per-Person advisory lock closes the app race; pin test `TestResidual_NR4_*` |
| B-6 | TRIGGER-SEARCH-PATH-1 remainder: functions 0026-0113 not search_path pinned | Medium (defence in depth) | row TRIGGER-SEARCH-PATH-1-0116; migration 0116; `VerifyRuntimeRoleInProduction` | B | Exploit path (TEMP shadowing) closed by REVOKE TEMP and production startup gate; deployed check is D-1 |
| B-7 | BRAND-DELETE-REHOME-1: runtime role has DELETE on `player_accounts`/`wallets`, no delete guard | Low | `deploy/init-app-role.sql:99`; ADR 0118 s7 item 5 | B | No funds can move (ledger FK and child FKs); childless rows only |
| B-8 | BRAND-CLOSED-BALANCE-CHECK-1: no standing per-brand closed-brand balance check | Low | row (not built; no matching code) | B | Needs an owner `DISABLE TRIGGER` bypass of 0136; check D (0134) is per tenant and LA025 sums ledger entries |
| B-9 | PLAT-AUDIT-SUBJECT-1: three provider-credential platform audit rows carry no `subject_tenant_id` | Low | `internal/providercred/service.go:344,412,841` | B | Records exist in platform scope; activation rows already tenant-scoped (`:558,635`); presentation policy D-0104-1 open |
| B-10 | PAY-H-FOLLOWUPS-1 remainder: (2) pass caps and jitter, (3) NotSent backoff, (5)(6)(7)(10) non-active-tenant and multi-instance cases, (13) statement/lock timeouts | Low-Med | `a11-payments-binding-recheck.md` | B | Registry class C "production-only" (2026-10-05); sweeper runs MOCK only; production refuses non-eligible adapters; SweeperItemTimeout bounds each item |
| B-11 | PAY-SEC-LAUNCH-1 S-L3: no throttle on staff `/resolve` polls | Low | recheck table | B | Needs persisted last-poll state (migration); staff-only, step-up and audited |
| B-12 | PAY-K3-FOLLOWUPS-1: O-K3 acting-session column discipline, N13 dead branch, N10, free-text `note` | Low | recheck table; migration 0132 WD020-WD025 guards | B | No money effect; deferred verifier; note retention is E-18 |
| B-13 | PAY-RECON-D2-HARDENING-1: unclassified disputed deposit reason yields no finding at run time | Low | `internal/reconciliation/payment_statement.go:200-215`; `payment_reason_classification_test.go` | B | Pin test blocks new reasons in CI; DB CHECK mirrors terminal reasons |
| B-14 | PAY-RECON-N1: refund reported only as the capture line `reversed` over-reports `pay_captured_unposted` | Low | registry row (ledger-finance ruling) | B | Over-reports, never hides money; persisted reversal evidence (0115) clears most shapes |
| B-15 | PAY-FPAY-HARDENING-1 F-L1: no alert on sustained KYC `unavailable` rate | Low | recheck table | B | Decision rows and audit growth visible; fail closed (503); delivery is ALERT-DELIVERY-1 |
| B-16 | PRH-I4 low items: L4-1 (admission disable accepted outside production), L5-1 (A4b panic gives 500), I1-1, I2-1 (`_unknown` override), I3-1 (dead `orElse`), I4-1, L3-RESIDUAL-1 (raw path logged on unmatched webhook paths), PRH-I4-METRICS-2 | Low | `internal/config/webhook_admission.go:229`; `webhook_admission.go:142,176` | B | Operator-config or cosmetic; production refuses the disable; bounded by MaxHeaderBytes |
| B-17 | WEBHOOK-RL-ADMIN-1 / API-POOL-PIN-1 / WEBHOOK-RL-F2-AUTHROUTES-1 / `decodeJSON` trailing values: no rate limit or bulkhead on authenticated routes | Low-Med | completion audit B list | B | Owner-approved class B; webhook routes covered; pool timeouts |
| B-18 | KYC-ENFORCE-1 remainder: LF-I3-4 (partial unique index), LF-I3-5 `RecordDecision` error half in play paths, B7 | Low-Med | ADR 0096 s16.2, s23 | B | Fail closed (no stake posted); thresholds dormant pending E-6 |
| B-19 | KYC-OUTBOX-REQUEUE-1 and KYC-E1-FOLLOWUPS-1 items (player submission enum, outbox retention, per-player vendor-call bound, rotation overlap test) | Low-Med | row KYC-E1-FOLLOWUPS-1 | B | MOCK vendor has no long outage; terminal failure raises `kyc.submission_failed_terminal`; entry gate for the KYC package |
| B-20 | KYC-DOC-REJECTION-BOUND-1: staff-entered `rejection_reason` unbounded, returned on player route | Low | `internal/httpserver/kyc_handlers.go:112,122`; `migrations/0040:145` | B | Staff-only author; no provider text |
| B-21 | AML-HOLD-RATE-ALERT-1, AML-NOT-ENFORCED-ALERT-1, SR7-3(b), SR7-4 | Medium (pre-launch) | `a7-aml-record.md:177-184,240` | B | Named gate: before any `under_platform_licence` tenant launches; holds fail closed; step-up critical and four-eyes; audit |
| B-22 | A6 follow-ups: staff RG limit increase refused (fails closed), no staff-change player notification, late-credit alert, commit-failure 503 untested | Low | `internal/rg/limits_service.go:552,728`; A6 row | B | Fail closed; enforcement is OFF per brand until E-13 |
| B-23 | ALERT-KINDS-DEDICATED-1 (per-reason alert kinds), ALERT-DELIVERY-STATE-DISPLAY-1, ALERT-OWNER-ROLE-OPS-1, ALERT-PURGE-AUDIT-IDENTITY-1 | Low | rows; `a9-observability-record.md:199` | B | Closed-set reason discriminators; owner role used only by migrations |
| B-24 | A8 INFO items SR8R-4/5/6, SR8R3-3/5/6/8; A3 SR3-5/SR3-7/LF-RR-2; A4 residuals (owner DDL, T5 remainder) | Info | `a8-market-blocking-record.md:296-339`; A3/A4 rows | B | Recorded; behavioural tests are the control; SR8R-5 needs product acknowledgement (E-14) |
| B-25 | ADR 0119 not-built items: S8 lifecycle UI, staff password reset, withdrawal `resolve` re-assertion, email-domain allow-list, SF030/SF007, WebAuthn, ADR 0120 recovery codes two-phase | Low-Med | rows ADR0119-S5S6, RECOVERY-CODES-2PHASE-1 | B | Per-request liveness check (S4a) refuses non-active staff; deferred class B by audit |
| B-26 | Deferred by owner: crypto, retail, sportsbook until scheduled, `evidence_ref_hash` | n/a | freeze report s6; completion audit F | B | Owner decisions; mocks only; production refuses synthetic components |
| B-27 | TEST-RESISO-RACE-1 / TEST-ISOLATION-WATCH-1 / ENV-GO-CACHE-1 / TEST-HYGIENE-1 legacy leftovers | Low | rows A1, A13a | B | Relative-p95 lane; reaper opt-in; security acknowledgement of the lane change owed |
| B-28 | SA-4 residue: about 26 other log sites duplicate `tenant_id` | Info | row F5-F6 | B | Cosmetic |
| B-29 | OBS-PENDING-SIGNALS-2 (kill-switch engaged state, per-tenant SLO) | Low | `a9-observability-record.md:76` | B | Other signals emitted and rule-covered |
| B-30 | PRH-REF-F1 latent (`idempotency.Assign` reference length), PROV-REVOKE-ALL-1, PAYWH-BRAND-1, WEBHOOK-RL-SHARED-1, PAY-ATTEMPT-RETENTION-1, PAY-PAYOUT-CASCADE-1, LEDGER-SUSPENSE-B-1 | Low | registry deferred rows | B | Deferred candidates with stated triggers (second tenant per provider, multi-instance, retention horizon) |
| B-31 | PAY-PAYOUT-CONTRADICTION-HOLD-1 interim: refused payout parks keep the hold, no operator path | Medium | recheck table; ADR 0107 CT-BLOCKED | B | Money-safe (hold kept); standing P1 finding; resolution design is E-8 |
| B-32 | CAS-RECON-SCALE-1: other streams O(history) per tick | Medium at volume | recheck table | B | One own-brand tenant at launch; payment stream bounded; run duration and staleness gauges; volume check is D-8 |
| B-33 | A-docs wording items: HDR/ADR text corrections noted in audit s6 | Info | audit | B | Covered by NA-7 |

## 4. Class C (provider-dependent)

| Id | Description | Sev | Evidence | Class | Note |
|---|---|---|---|---|---|
| C-1 | Real PSP adapter and sandbox; Nuvei UNVERIFIED (docs unreachable) | High | `docs/provider-integration/`; freeze report s5 | C | Gaps G-01..G-11 |
| C-2 | Real statement source and read/reporting-scoped credential (R3-FIRST-REAL-SOURCE-READONLY-1, LF-A11-2/6 per-provider settle lag, PAY-K3 real source, R-S3) | High | recheck table | C | Engineering halves done against MOCK |
| C-3 | QueryStatus amount/asset echo, merchant-reference status query, destination-echo conformance, redelivery semantics (PRH-I1-MANIFEST-2/3/4, F-POOL-2 LF-C1, PROV-OUTBOUND-CRED-1 tripwire relaxation, PAY-PSP-CONTRADICTION) | High | rows | C | Per-adapter conformance |
| C-4 | PAYOUT-AMOUNT-DISPUTE-1 partial/different-asset payouts, provider "payout returned" webhook, status/return mapping, M4 non-MOCK gates (D-7 source checklist, S-3, T10, R-1 post-execution predicate, durable echo-absent success record, credit-after-stop monitoring) | High | rows A5, ADR 0111 s20-s25 | C | Non-MOCK M4 stays BLOCKED |
| C-5 | B13-A launch flags for non-MOCK instruments (M-3 fingerprint ownership, L-1 client-supplied card fields, L-2, L-7); player instrument registration (PCI/PII, D-REG-1) | High | ADR 0111 s15 | C | Hosted/tokenised only |
| C-6 | Casino adapter: wire contract CAS-G01..10, statement store CAS-STMT-STORE-1, R-10 run-row forgery for casino stream, PRH-REF-C2 evidence retention, PAY-K3-MR020-HTTP-MAPPING-1, LF-A3-5 extra win visibility | High | A3 row, registry | C | |
| C-7 | KYC/AML vendors: KYC-G01 hosted session, KYC-HOSTED-SESSION-1, document-verification flow, AML screening vendor, geolocation vendor | High | provider packages | C | |
| C-8 | Real email/webhook alert delivery: ALERT-EMAIL-PROVIDER-1, ALERT-DELIVERY-1 (adapter half), PAY-P1-MULTISUCCESS-ALERT-1 delivery | High | `a9-observability-record.md:70` | C | Recipients are E |
| C-9 | Sportsbook adapter SB-G01..10, SB-CATALOGUE-IO-1 F6 per-call timeout, sports data SD-G01..08, feed-driven settlement | Medium | provider packages | C | Deferred by owner until scheduled |
| C-10 | KMS/asymmetric actor-proof signing | Medium | ADR 0122 s11 | C | Deferred improvement |
| C-11 | Session duration / reality-check RG enforcement | Medium | A6 row | C | Needs player-session signal |
| C-12 | FX rate source / ConversionOperation, KYC-FX-AGG-1 | Medium | registry | C | Fiat-first |
| C-13 | Crypto custody provider (ADR 0008) | Medium | CLAUDE.md | C | Deferred by owner |
| C-14 | OBS-OTLP-TRACES-1 OTLP backend | Low | A9 record | C | |

## 5. Class D (AWS-dependent)

| Id | Description | Sev | Evidence | Class | Note |
|---|---|---|---|---|---|
| D-1 | TRIGGER-SEARCH-PATH-1 deployed check, PLAT-ROLESPLIT-1 (API connects as `igaming_runtime`, never owner), runtime role/search_path on the real account | High | `docs/runbooks/plat-rolesplit-staging-verification.md` | D | Launch blocker until verified |
| D-2 | RDS behaviour, logical restore on RDS (likely BLOCKED), pg_catalog revokes, IAM DB auth, BYPASSRLS role creation | High | A12/STAGING-PREP-3 rows | D | |
| D-3 | Managed PITR, measured RPO 0 / RTO under 15 min | High | freeze report s2 | D | Not demonstrated locally |
| D-4 | Secrets Manager, ECS deploy, runner/ECR image build, SRSP-3 dedicated task role, SRSP-9/10/11, SRP3-7/8/12, A12 IAM delta simulation | Medium | plan `aws-staging-deploy-plan.md` | D | SRF2-6 (staging import keys): `PAYOUT_INSTRUMENT_KEYS` is wired in `deploy/aws/modules/ecs/main.tf:167`, so the row is likely stale; confirm on the staging run |
| D-5 | ACCESS-ANALYZER-CHECK-1, DEPLOY-FPKEY-1 (optional key not provisioned), SECRETSTORE-AWS-1 IAM part, HD-10.3-2 | Medium | rows | D | |
| D-6 | End-to-end alert delivery to a real recipient; OBS-BACKEND-DEPLOY-1 Prometheus/Alertmanager | High | A9 record | D | |
| D-7 | WEBHOOK-EDGE-1 edge rate rule/WAF | Medium | ADR 0097 R1 | D | |
| D-8 | Load/soak at realistic volume; DEVOPS-0107-INDEX-WINDOW-1; CAS-RECON-SCALE-1 volume check; RG monthly-window cost (HDR-A6-2) | Medium | `load-and-soak-test-plan.md` | D | |
| D-9 | KMS signing, backup runner placement (G1), backup role JIT credentials | Medium | A12 row | D | |
| D-10 | PRH-REF-C2 log pipeline retention and per-provider rate alerting | Medium | row | D | |
| D-11 | Hosted CI branch protection on `main` (BRANCH-PROTECTION-1) enforcement | Low | row | D | Repository admin setting |
| D-12 | VERIFY-TEARDOWN-ECS-1 tag-sweep script exclusion of INACTIVE ECS objects | Low | registry | D | Only checkable on AWS |
| D-13 | Real metrics scrape path (G4) and alert-rule wiring | Medium | STAGING-PREP row | D | |
| D-14 | `mock_geolocation_country`, operating-market rows, two-admin route enable in staging | Low | STAGING-PREP rows | D | Owner inputs at the AWS gate |

## 6. Class E (legal / compliance / human decision)

| Id | Description | Sev | Evidence | Class |
|---|---|---|---|---|
| E-1 | SRME-4 residence vs physical location (is residence-only enough for Anjouan) | Medium | market-evidence record | E |
| E-2 | SRME-5 / MKT-WIRE-CREDIT-1 (deposit credit and open-session bet callbacks not gated per player) | Medium | same | E |
| E-3 | SRME-6 registration for excluded-market visitors in production | Low | same | E |
| E-4 | Maker-checker for residence determinations (SRME-1) | Medium | same | E |
| E-5 | Alert severity/volume for `market.residence_into_open_market`; SRF5-1/SRF5-3 | Low | F5 record | E |
| E-6 | HD-KYC-1..8 thresholds, KYC-FX-AGG | High | ADR 0096 s4 | E |
| E-7 | HDR-A6-1..4 (RG interpretations, monthly windows, stuck-intent TTL acceptance, relink), RG floor values OQ-2 | High | register | E |
| E-8 | PAY-PAYOUT-CONTRADICTION-HOLD-1 resolution path, GAP-AAM, Q-R32-3 resume, MA020 F-VIS stranding (runbook note plus decision), H-W1 residuals | Medium | registry | E |
| E-9 | HD-CTF-1..10, Q-GP-2/3/4/6, Q-HSEC-1/2/3, O-1/O-2, R3 owner questions | High | register | E |
| E-10 | HQ-E1-1/3/4 | Medium | register | E |
| E-11 | Legal wording of the AML confidentiality duty; HD-AML-1..9; D-AML-TIP-1 | Medium | A7 record | E |
| E-12 | SF4-3 sticky escalation owner question (companion to B-1) | Low | F4 row | E |
| E-13 | Switching RG enforcement on per brand (needs E-7) | High | A6 row | E |
| E-14 | SR8R-5 product acknowledgement; D-0104-1 platform actor visibility; D-OPS-1/2 | Low | records | E |
| E-15 | ALERT-DELIVERY-1 recipients, rota (HD-PRH2-4-OPS), ALERT-WEBHOOK-ENDPOINT-1, ALERT-SECOND-ADMIN-1, ALERT-DPA-1, ALERT-RETENTION-HORIZON-1; incident rota | High | A9 record | E |
| E-16 | HD-RET-1 retention vs erasure of `withdrawal_requests`; K3 note retention (also B-12) | Medium | register | E |
| E-17 | PERSON-RELINK-PIN-1, H1/T3/T4/T8 authorisation, ADR0119-OQ-1..6, HD-PRH2-8, HD-PRH-1 (slug confidentiality) | Medium | register | E |
| E-18 | Licence and jurisdiction decisions beyond Anjouan, HDR-J-7/8/9, HDR-M-1/2, HDR-SB-1, hosting AUP | High | register | E |
| E-19 | Vendor/PSP selection, contracts, production credentials, production launch, AWS authorisation | High | CLAUDE.md | E |
| E-20 | Timing-lane criterion acknowledgement (ADR 0094), CI-BILLING-1 (superseded), MAC-RUNNER drop | Low | register | E |
| E-21 | Brand activation (manual four-eyes per brand) and legal approval | High | ADR 0112 | E |
| E-22 | B14-B18 sandbox-gate approvals | Medium | registry | E |

## 7. Section 6 coverage check (platform-freeze report)

| Section 6 item | Row |
|---|---|
| SF4-1/R1 forged allow row | IF-1 |
| F4-R2-NEGBAL-SQL | IF-2 |
| SRF2R-3 | IF-10 |
| SRF2R-1/2 | IF-8, IF-9 |
| SRF2-6 | D-4 |
| SF4-3 | B-1, E-12 |
| SF4-5 | IF-3 |
| Physical backups contain actor-proof key (T7) | B-2 |
| SRME-4 | E-1 |
| SRME-5 / MKT-WIRE-CREDIT-1 | E-2 |
| SRME-6 | E-3 |
| Maker-checker for residence | E-4 |
| Alert severity/volume | E-5 |
| HDR-A6-3 | E-7 |
| RG floor values | E-7 |
| Legal wording AML confidentiality | E-11 |
| U-5 | IF-5 |
| U-6 | IF-6 |
| U-7 | IF-7 |
| `TODO(jurisdiction)` | IF-4 |
| Incident rota | E-15 |
| Load/soak at realistic volume | D-8 |
| Deferred by owner: crypto, retail, sportsbook, NR-4/S56-R14, `evidence_ref_hash` | B-26, B-5, C-13, C-9 |

All section 6 items are present.

## Appendix A. Rows that say OPEN but are closed at `76187cc` (do not carry)

Superseding registry text is needed once (NA-7). Evidence is a later row, migration or code.

| Row id | Closed by |
|---|---|
| CAS-GAME-KILL-BET-1, CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-PLAYER-REF-1, CAS-WIN-IDEMP-1, CAS-STMT-IO-1 | A3 (`A3-CASINO-PREADAPTER-2026-10-15`) |
| MANUAL-ADJ-LINK-1 | A4, migration 0153 |
| STAFF-LIFECYCLE-1 | ADR 0119 S5/S6, migration 0139 |
| WD-RG-1 | A6 (WD050/WD051) and F4 (0169) |
| WITHDRAWAL-REVERSAL-1 | A5, migration 0162 (PAYOUT-AMOUNT-DISPUTE-1 partly: C-4) |
| CP-W1 | `CP-W1-CLOSED-2026-10-15` |
| CI-BILLING-1 | `CI-BILLING-1-SUPERSEDED-2026-10-15` |
| RECON-RUN-FAILED-ALERT-1, R3-LISTING-FAILURE-LOG-ONLY-1, ALERT-RETENTION-1 | A9, migrations 0156/0157 |
| R3-OBS-BOUND-1 (engineering), PAY-K3-STATEMENT-SOURCE-WIRING-1, H-W1 second half, SR11-1, SR11-10/11, LF-A11-1, SR5R-1, R9 | A11 and F2/F3, migrations 0160, 0167, 0168 |
| MA020-SYNC-MISMATCH-1, STMT-TABLE-INSERT-RLS-1, MA020-K2-VISIBILITY-1 acting half | B4 (0119) and 0125 s3 |
| PAY-DEPOSIT-ESCALATION-1, PAY-RECEIPT-T4-DRAIN-TEST-1, PAY-POLL-ECHO-HARDENING-1, PAY-PAYOUT-REFBIND-1, PAY-CALLBACK-MISMATCH-BIND-1, PAY-PAYOUT-UNBOUND-HOLD-1, PAY-RECON-POLL-REF-CLEAR-1, PAY-RECON-PARKED-CAPTURE-STANDING-1, KYC-ENF-DECISION-ROWS-1, PAY-KYC-UNAVAIL-1, PAY-PAYOUT-ERRREF-1, RECON-PAYOUT-LIVE-TEST-1 | CLASSB B1-B12, R2-F, F-pay, H; `a11-payments-binding-recheck.md` |
| PAY-SEC-LAUNCH-1 destination binding, S-L1, S-L4 | B13-A/B (0123, 0126), A11 |
| TRIGGER-SEARCH-PATH-1 exploit path | migration 0116 (deployed check D-1) |
| THREAT-MODEL-ARBITRARY-SQL-1 decision, SIGNED-ACTOR-PROOF | decided YES; 0120 merged |
| R3-GAME-POSTINGS-NONACTIVE-1, H-SEC-5/11, H(8) | 0118, `3643cb0`, ADR 0095 s45/s47 |
| TENANT-STATUS-AUTHZ-1, idle-in-transaction timeout | ADR 0112 slice 4 (production pool wiring) |
| SRBX-3 / R1 for databases at 0166, SF4-2, U-1, SA-3 endpoint, SA-4 kill-switch line, SA-5 | F1, F4 caller pin `60753e3`, U-1 config change, F6 |
| KYC-SUBMIT-OUTBOX-1, KYC-SANCTIONS-IF-1 | E1 (0114), A7 screening interface |
| TEST-ADMISSION-FLAKE-1, TEST-SCRATCH-LEAK-1 (mitigated) | rows; A13a |
| PRH-I4-METRICS-1, PAYWH-RL-1 (security confirmation still owed) | PRH-2 J, PRH-I4 |
| SB-JUR-RUNG2-1 | A8 market blocking |
