# Whole-platform completion audit — 2026-10-14

Status: AUDIT RECORD (orchestrator). Supersedes `docs/plans/platform-reconciliation-2026-10-13.md` (stale: MFA, AML S1/S3,
RG S2/S3, brand binding, alert S1b, staff lifecycle are all merged). Evidence is LOCAL (private DBs, MOCK providers); GitHub CI
status is reported separately. Technical readiness ≠ legal/licensing approval ≠ real provider integration ≠ brand activation.

Method: five independent read-only audits (financial/payments; identity/compliance; casino/sportsbook/retail/risk/bonus;
frontends; operations/backlog) reconciled by the orchestrator, with every contested claim re-checked in code.
The Blueprint PDF could not be re-read in this environment; Blueprint requirements are taken from
`docs/architecture/01-requirements-inventory.md` (Blueprint-sourced) — a limitation, recorded.

## 1. Verification at HEAD (commands and results)

HEAD at start of audit: `48c5014` (clean, pushed). Local evidence:

| Check | Result |
|---|---|
| `go test ./... -count=1` (unit) | PASS=1490 FAIL=0 SKIP=5 (at 16b0c40; no Go change since except two test/CI/verify edits below) |
| Payments integration, 26 per-letter shards (A–Z), `-tags integration -p 1` | all PASS (at 16b0c40) |
| Other integration (alerting, withdrawal, httpserver, reconciliation, payoutinstrument, providerkind, db, adjustment, tenant, actorproof, auth, config, providercred, cmd/…) | PASS=2189 FAIL=1 SKIP=1 at 16b0c40; the 1 FAIL = `TestCreateStaff_TenantAdminCannotCreateFinanceRole`, a real fixture defect, FIXED in 48c5014; `TestCreateStaff_*` re-run PASS |
| `-race` payments (522) / other (659) | PASS |
| gofmt / vet / vet -tags integration / golangci-lint 2.9.0 | clean |
| Full `internal/httpserver` package after the fixture fix (`-tags integration -p 1 -v`) | see §1.1 |
| b2c `npm run build` + `npm test` | build OK; 19 files / 91 tests PASS |
| backoffice `npm run build` + `npm test` | build OK; 34 files / 393 tests PASS |
| `go run ./cmd/migrate verify` on a fully migrated private DB | FOUND DEFECT: exit 1, "GAP 143–150" → FIXED in 888de6d (reserved range declared in `internal/db/migrate.go`, only 143–150 tolerated, test added); now "all applied migrations verified clean, no version gaps" |
| GitHub CI | FOUND: NOT billing-blocked (billing blocked only runs #365–~#593). Since #600 every run fails at the step "Guard - test admin URL…" because integration-tagged `internal/testsupport/alertworld/world.go` reads `TEST_ADMIN_DATABASE_URL`; all later steps (build, migrations, unit, integration, race, reversibility, govulncheck) never ran → zero hosted evidence for ~200 commits. FIXED in 888de6d (guard allows `alertworld`, which carries the integration tag; tag check extended to it). Run #820 on 888de6d: see §1.2 |

### 1.1 httpserver full package
Recorded in the task registry row `PLATFORM-COMPLETION-AUDIT-2026-10-14` when the run completes (see that row for the final number).

### 1.2 Hosted CI
Recorded in the same registry row. Whatever the hosted run reveals past the guard is real work (likely the ADR 0094 timing lane,
`TestResolutionIsolation_NormalOperation`).

## 2. Platform status by domain (technical, providers MOCK)

| Domain | Status |
|---|---|
| Ledger / wallets / idempotency / reconciliation | IMPLEMENTED (DB-enforced balance trigger, append-only, idempotency unique index, hourly recon with advisory lock). Gaps: A-items in §3 |
| Deposits / callbacks / settlement | IMPLEMENTED against MOCK PSP; real PSP = PROVIDER DEPENDENT |
| Withdrawals (ADR 0113) | IMPLEMENTED incl. state machine, holds, check D; reversal and amount-dispute paths NOT IMPLEMENTED (A) |
| Four-eyes (K1/K2/K3, adjustments, launch, kill switch, staff lifecycle) | IMPLEMENTED; MANUAL-ADJ-LINK-1 open (A) |
| Identity / staff auth / MFA / step-up / lifecycle | IMPLEMENTED (email delivery MOCK) |
| RLS / roles / actor proof | IMPLEMENTED locally; deployed verification = deploy-gated (§7) |
| Casino | PARTIALLY IMPLEMENTED (MOCK): core complete; pre-real-money internal fixes open (A) |
| Sportsbook | PARTIALLY IMPLEMENTED (MOCK): singles + staff-triggered settlement; feed-driven settlement NOT IMPLEMENTED (scope decision D-S1) |
| Sports-data abstraction (ADR 0117) | NOT IMPLEMENTED |
| Bonus | IMPLEMENTED waves 1–3 (HR-9 removed, verified `internal/ledger/bonus_mirror.go`); bus-vs-sweep deviation unrecorded (A, doc) |
| Risk engine | IMPLEMENTED (amount/cumulative kinds); velocity/device kinds B |
| KYC | PARTIALLY IMPLEMENTED (MOCK vendor) |
| AML | PARTIALLY IMPLEMENTED: holds + cases + four-eyes (S1/S3); no routes/UI, no screening mock, no monitoring, no SAR |
| Responsible gaming | PARTIALLY IMPLEMENTED: self-exclusion ENFORCED; limits/time-out RECORDED BUT NOT ENFORCED (a live `POST /v1/me/rg/time-out` that nothing honours) |
| Jurisdiction / operating market / geo | PARTIALLY IMPLEMENTED: registry wired; `operatingmarket`/`geolocation` have no production importer |
| Brand activation governance | IMPLEMENTED (manual per-brand four-eyes; no universal licence flag) |
| B2C | PARTIALLY IMPLEMENTED: no RG self-service UI, no payout-instrument registration, no password reset, no i18n |
| Back office | PARTIALLY IMPLEMENTED: broken staff-create form; no invite-accept page; no console for parked payments/recon/K1/K3 |
| Partner console | NOT a separate product (role-gated slices of backoffice) |
| Retail | NOT IMPLEMENTED — design only (ADR 0035/0036), absent from Blueprint, owner-DEFERRED |
| Observability | PARTIALLY IMPLEMENTED: stdout exporters only, no `/metrics`, alert rules unwired |
| Alert delivery | STUB: log/mock channels only |
| Backup/DR | NOT VERIFIED: no data-bearing restore ever performed |
| CI | defect found and fixed (§1); hosted green not yet demonstrated |

## 3. Classified inventory

### A — required before TECHNICAL production readiness (dependency order)

| # | Item | Why / what | Simpler cut |
|---|---|---|---|
| A1 | Hosted CI green | Fix whatever fails past the guard (timing lane: make ADR 0094 criterion robust — relative/p95 or dedicated lane). No technical-readiness claim without a hosted run | — |
| A2 | Truth in docs | HANDOVER, progress, active-stage, ADR 0112–0116/0119 headers, superseding registry rows for stale "NOT MERGED", and a bonus bus-vs-sweep deviation record (ADR amendment). Cheap; a false status doc is itself a defect | one consolidated status refresh |
| A3 | Casino pre-adapter fixes | CAS-GAME-KILL-BET-1 (postBet never checks game status — verified), CAS-WIN-IDEMP-1, CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-STMT-IO-1, CAS-PLAYER-REF-1 (opaque player ref). All internal | — |
| A4 | MANUAL-ADJ-LINK-1 | DB fence so a non-acting session cannot insert a `manual_adjustment` ledger transaction without an approved request (needs 19 fixture migrations) | — |
| A5 | WITHDRAWAL-REVERSAL-1 + PAYOUT-AMOUNT-DISPUTE-1 | Governed compensating posting for a payout declared paid but not paid; an honest tested path for amount/asset mismatch. Posting shape is provider-independent (ledger-finance gate) | — |
| A6 | RG enforcement minimum | (i) time-out predicate in `EvaluateEligibility` (one predicate; the shipped route is otherwise dishonest); (ii) deposit-limit and loss/wager-limit check inside the existing deposit/bet transactions (ADR 0115 S4-min, ships DISABLED/fail-closed behaviour per ADR with brand RG config); (iii) minimal B2C RG UI (limits, self-exclusion, time-out). Session-duration/reality-check = PROVIDER DEPENDENT/F. Values = E | S4-min instead of full S4 |
| A7 | AML operability minimum | S6 minimal routes + permission constants + minimal BO queue for holds/cases; S7 readiness items (AML/RG config present, monitoring-unconfigured visible); S2 screening INTERFACE + deterministic MOCK + fail-closed policy | S4 monitoring and S5 SAR are B (below) |
| A8 | Market blocking wiring | Wire `operatingmarket` (+ mock geo adapter interface) into registration/deposit/launch/placement, fail-closed | — |
| A9 | Observability + alerting plumbing | Metrics/trace exporter (OTLP or Prometheus) + `/metrics` or collector path, emit pending signals (recon heartbeat, drift, pool, kill switch), wire alert rules; provider-neutral email/webhook alert channel behind the existing ChannelKind seam (recipients = D); RECON-RUN-FAILED-ALERT-1, R3-LISTING-FAILURE-LOG-ONLY-1 as durable alerts; ALERT-RETENTION-1 | channel = generic webhook + SMTP-neutral |
| A10 | Operator console minimum | Fix broken `CreateStaffForm` (calls a deleted route); staff invite + invitation-accept page; ADR 0114 S3 (parked payments), S5 (reconciliation views), S7 (K1 grants), S8 (K3 force-resolution); UI-to-route contract test (every `apiFetch` path vs OpenAPI) | S9/S10/S11 and rest of 0119 S8 are B |
| A11 | Payments engineering halves | PAY-K3-STATEMENT-SOURCE-WIRING-1; R3-OBS-BOUND-1 bound; re-verify each "binding before first real PSP" registry row against HEAD (PAY-DEPOSIT-ESCALATION-1, PAY-RECON-*, PAY-CALLBACK-MISMATCH-BIND-1, PAY-PAYOUT-UNBOUND-HOLD-1, KYC-ENF-DECISION-ROWS-1, PAY-KYC-UNAVAIL-1, PAY-PAYOUT-ERRREF-1) and close the provider-independent halves | audit-first (cheap) |
| A12 | Backup/restore (local half) | Data-bearing backup + restore drill locally, owner-role design for FORCE-RLS dump (BACKUP-ROLE-1 decision D1), runbook. RPO/RTO statement | managed PITR is the deploy half (C/D) |
| A13 | Release-gate hygiene | `migrate verify` (done 888de6d); TEST-HYGIENE-1 scratch-DB leakage cleanup policy | — |

Sportsbook: A-conditional on D-S1 (see below).

### B — recommended hardening (do not trigger a new multi-day cycle)
S56-R14 / NR-4 unique index (**class B, decision unchanged**: per-Person advisory lock closes the app race; raw-SQL double-binding is the accepted
R1 class; revisit with a fixture-migration slice). Others: ADR 0114 S6c/S9/S10; ADR 0119 S8 remainder/S9/ADR 0120 two-phase recovery codes; S56-R2/R6/R7/R12/R13;
password reset UI (b2c); i18n (b2c); runtime brand theming; per-route API rate limit + `decodeJSON` trailing value; AML S4 monitoring rule engine (mechanism) and S5 SAR (MOCK format);
Q5 cross-tenant sanctions propagation; risk velocity/device kinds, review queue; bonus abuse controls, bonus UI; WD-RG-1 gate (D then B/A); CAS-RECON-SCALE-1, CAS-WIN-ANOMALY-1;
ADR 0117 A2/A3/C2/C4/B0–B4 (FX not needed single-currency); DB non-negative backstop; search_path pinning of 0026–0113 functions; digest-pinned images, `-trimpath`, dependabot, branch protection;
`idle_in_transaction_session_timeout`; docs consolidation of 4–8k-line registries; NULL-ARM-WRITE-1; DEVOPS-0107-INDEX-WINDOW-1.

### C — requires a real provider (do not block on these)
- **PSP:** adapter + ADR 0111 S1–S16 conformance against sandbox, status/return mapping, sealed statement source (non-MOCK), callback signing/rotation, tokenised instruments, hosted cashier redirect, chargeback lifecycle, PROV-OUTBOUND-CRED-1, F-POOL-2.
- **Casino:** aggregator wallet-callback wire format/signatures, launch/bootstrap contract, free-round normalisation, round-close semantics (Q-GP-6 also D), catalogue/RTP variants/blocklists, real statement for recon, certification.
- **Sportsbook:** bet-placement contract (sync/async ack), settlement/void/cashout message formats, widget auth handshake, settlement statement.
- **Sports data:** transport/auth, entity-id mapping, sequence/versioning guarantees, correction semantics, heartbeat.
- **KYC/AML:** document-verification vendor flow (incl. B2C document upload UI), sanctions/PEP screening vendor, geolocation/IP vendor; real alert recipients' paging service.
- **Other:** real email delivery (staff/player), FX rate source, crypto custodian (ADR 0008).
Do not invent provider APIs; Nuvei is at most a candidate.

### D — owner decisions (minimum decision + recommendation)
| ID | Decision | Recommendation |
|---|---|---|
| D-S1 | Is sportsbook in the FIRST launch? | Casino-first. Sportsbook stays MOCK-complete; feed-driven settlement (ADR 0117 A1/C1/C3) becomes A only when a sportsbook brand is slated for activation |
| D-CRYPTO | Is crypto in the first launch? | No (fiat first) — keeps `int64` ledger amounts and no custody interface in A. If yes, add `big.Int` ledger + custody interface + mock to A |
| D-RG | Are player limits required for the first brand? | Yes; build S4-min (A6). Anjouan values are E |
| D-RETAIL | Retail in scope? | No — DEFERRED stands (not in Blueprint) |
| D-ALERT | Alert channel + recipients (ALERT-DELIVERY-1) | Email + one webhook/chat; owner as interim primary |
| D-BACKUP | BACKUP-ROLE-1 | Dedicated audited `igaming_backup` role (BYPASSRLS, no app login) + managed PITR |
| D-AWS | Authorise ONE named-commit staging deploy | Yes, after A-items; only way to verify PLAT-ROLESPLIT-1, TRIGGER-SEARCH-PATH deployed check, PITR, alert stack |
| D-CI | ADR 0094 timing-lane criterion; protect `main`; drop Mac-runner plan | Relative-p95 lane; protect `main`; drop MAC-RUNNER |
| D-SEC | D-0104-1, ADR 0113 §9.2 HIGH residual, THREAT-MODEL T3/T4/T8, ADR 0119 OQ-1..6, S-H1 | D-0104-1: scope-only/HMAC pseudonym; §9.2: accept only with scheduled payout-statement reconciliation per live provider; others keep deferred; OQ defaults as recorded |
| D-WD-RG | RG/Risk gate on withdrawals | Yes, add gate (B/A) |
| D-AML-TIP | D-AML-TIP-1 | (c)+(a) later (as previously recorded) |
| D-PAYOUT-INSTR | Security review of B2C payout-instrument registration (PCI/PII) | Approve a hosted/tokenised-reference-only registration; then it becomes A |
| D-DEBT | Negative balance (debt) policy after deposit reversal | Allow negative with block-until-cleared, ledger-finance to confirm |

### E — legal / compliance / brand activation (manual, never blocks technical readiness)
HD-KYC-1..8, HD-AML-1..9, ADR 0115 OQ-2 RG floors/values, HDR-SB-1, HDR-J-7/8/9, HDR-M-1/2, HD-CTF-1..9, HQ-E1-*, HD-RET-1 (erasure vs retention), SAR/FIU formats, per-jurisdiction report formats, licence evidence and brand-launch attestations. Each brand is activated manually by four-eyes; there is no universal licence flag and none is introduced.

### F — future
Retail/POS; bonus wave 4 (gamification/CRM/affiliate); AI agents; schema/database-per-tenant isolation; CDC/ClickHouse; asymmetric actor proof; cashout/multiples/bet-builder (ADR 0117 C5); jackpots/tournaments; KYC-FX-AGG-1; FX ConversionOperation (until multi-currency conversion is a product need).

### G — complete / stale-open rows
Ledger core, deposit reversal, casino rollback tombstones, withdrawal state machine (merged), brand binding (0136), staff MFA/step-up/lifecycle (0137–0139, 0151), launch governance (0128–0133), AML S1/S3 (0140–0142), RG S1–S3, alert S1b, kill switch UI, launch UI, step-up UI, HR-9, CP-W1 (sweeper wired), TENANT-STATUS-AUTHZ-1, TRIGGER-SEARCH-PATH exploit path, ENV-ANCHOR-REFUSAL, CAS-REVOKE-CONSUMED-1, PAY-DOUBLE-CREDIT-1, TEST-ADMISSION-FLAKE-1.

## 4. ADR slice decisions (not "because next")

| Slice | Requirement satisfied | Satisfied elsewhere? | Class | Defer risk | Decision |
|---|---|---|---|---|---|
| ADR 0115 S4 | limits/time-out enforced | only self-exclusion | A (S4-min) | recorded-but-unenforced player protection | Build S4-min (A6); full S4 extras B/F |
| ADR 0115 S2 | write APIs | done | G | — | already merged; no further work |
| ADR 0115 S5/S6 | player UI, QA | no | A (minimal UI) / B | players cannot self-serve | minimal UI in A6 |
| ADR 0114 S3, S5, S7, S8 | operators can act on parked money, drift, grants, forced resolutions | API only | A | P1 drift/parked money has no console | build (A10) |
| ADR 0114 S9/S10/S11/S6c | adjustments/HSEC/blind M4 consoles | API exists | B / D | acceptable via API+runbook for small team | defer |
| ADR 0116 S2 | screening interface | none | A (interface+mock) | no fail-closed screening seam | A7 |
| ADR 0116 S6/S7 | officer operability, readiness | none | A (minimal) | AML unusable by humans | A7 |
| ADR 0116 S4/S5 | monitoring, SAR | none | B | made visible by S7 readiness item | defer |
| ADR 0117 A/C | outbox, feed, result ingest | design only | A only if D-S1=yes | sportsbook uses staff-triggered settlement | conditional |
| ADR 0113 slice 4 | docs | — | B | none | defer |
| ADR 0119 S8 | staff directory UI | API+CLI exist | A (invite/accept only) / B | staff onboarding needs script | A10 minimal |
| ADR 0120 | two-phase recovery codes | — | B | S7b-R1 loss window | defer |
| S56-R14 | one Person ↔ two live platform accounts via raw SQL | app lock closes the race; R1 accepted | **B** | needs compromised runtime credential | keep deferred, no cycle |

## 5. Technical debt and flaky tests
Debt: registries 4–8k lines (hard to audit); route-count pins as recurring merge conflicts; integration-tagged files not linted in CI; scratch DB leakage; base images tag-pinned; no dependabot.
Flaky/env-sensitive: `TestResolutionIsolation_NormalOperation` (500 ms bound, ADR 0094; also seven other timing-lane tests rerun alone in CI); CI-FLAKE-281 Argon2 contention; ENV-GO-CACHE-1.
Not a flake: `TestCreateStaff_TenantAdminCannotCreateFinanceRole` (real fixture defect, fixed).

## 6. FINISH LINE — TECHNICALLY PRODUCTION READY AND PROVIDER-INTEGRATION READY
All must be true (finite):
1. A1–A13 above complete, each with reviews appropriate to its class (security for auth/RG/AML, ledger-finance for money).
2. A hosted GitHub CI run green on the final commit (build, migrations up/down/up, unit, integration, race, lint, govulncheck, frontends).
3. Full local verification recorded on the final commit (unit, payments per-letter shards, other integration, race lanes, frontends build+test), with exact PASS/FAIL/SKIP.
4. `migrate verify` clean; operations runbooks present for alert response, reconciliation drift, kill switch, backup/restore, staff MFA; restore drill done with data.
5. Status docs match the code (A2); every deliverable labelled IMPLEMENTED / MOCK / PROVIDER DEPENDENT / etc.
6. Provider interfaces + conformance tests exist for PSP, casino, sportsbook, KYC, AML-screening (mock-complete); the C list is the integration backlog.
7. No A-class registry row open. Owner decisions D-S1, D-CRYPTO, D-RG, D-ALERT, D-BACKUP, D-CI recorded (defaults accepted or overridden).
Outside the finish line (deploy-gated): deployed verification of roles/search_path/PITR/alerts needs D-AWS and is required for PRODUCTION (not technical) readiness.

## 7. Four separate states (current)
1. Technical readiness: NOT YET (A1–A13 open).
2. Provider-integration readiness: PARTIAL (interfaces + mocks exist for PSP/casino/sportsbook/KYC; AML screening interface and sports-data interface missing).
3. Real provider integration: NONE (all MOCK).
4. Brand activation: NONE; governance mechanism IMPLEMENTED; no brand authorised.

## 8. Next five tasks (only those that genuinely remain)
1. A1 — confirm hosted CI run past the guard; fix what it reveals.
2. A2 — status-doc refresh + bonus deviation record.
3. A3 + A4 — casino pre-adapter fixes; MANUAL-ADJ-LINK-1 (ledger-finance + security gates).
4. A6 + A8 — RG time-out/S4-min + minimal UI; market-blocking wiring.
5. A5 — WITHDRAWAL-REVERSAL-1 / PAYOUT-AMOUNT-DISPUTE-1 (ledger-finance gate).
(A7, A9–A13 follow in dependency order.)
