# FINAL-PLATFORM-CLOSURE-REPORT (2026-10-17)

Scope: the provider-independent platform. Supersedes `platform-freeze-report-2026-10-17.md` where they differ. Nothing here is a legal, regulatory or licensing approval or a launch authorisation. All providers are MOCK. **No AWS resource was created or touched in this phase. No provider integration was started. No brand is activated.**

## 1. Freeze commit
Branch `claude/focused-wright-jw88w9`. **Recommended freeze commit: `50eded5afe80ef00e01765827dba77a9f9627b20`** (hosted CI 27/27; every later commit on the branch is documentation/registry only — verify with `git diff 50eded5..HEAD --stat -- . ':!docs'`, which must be empty). Migration head **0175** (reserved, never used: 0143–0150, 0159, 0161, pinned by test). Previous candidate `76187cc` is superseded.

## 2. What the closure round did (all merged, each with a record in `docs/plans/`)
| Item | Result | Migration |
|---|---|---|
| F4 residual SF4-1/R1 forged dispatch-gate allow | **CLOSED for the SQL-only runtime-credential attacker**: allow is a signed, nonce-consumed, scope/operation/target/payload-bound system actor proof verified in SQL (ADR 0110 pattern); SF4-5 alert; F4-R2 SQL clause built then withdrawn as unworkable, replaced by detection (C3) | 0171 |
| SRF2R-1/2/3 | cursor read + M2 gate read bounded (server cursor, 50k-row budget, loud `cursor_flood`); `match_stalled` semantics; MA020 SQL guard clears only on a proof-bound tombstone | 0172 |
| Deposit tombstone forgeable by runtime role (review finding) | deposit tombstones require a signed system proof bound to a verified reversal receipt; signer pins by file; payout-gate failure counter bounded | 0175 |
| C1 first-deposit double grant | one FirstDepositOnly grant per (tenant, offer, player); DB backstop | 0173 |
| C3 negative wallet / payout-while-negative | read-only detective findings in the reconciliation framework | 0174 |
| U-5 frontend money | exact BigInt/decimal-string amount module in both apps, JSON big-int guard, static float ban | – |
| U-6 / U-7 | test-support routes only via the production-guarded gate; static + runtime proof that 127/135 mutating staff routes reach an audit write, 8 exact allow-listed (AML own trails); bonus suggestion claim now audited | – |
| Jurisdiction TODOs | all `TODO(jurisdiction)` removed: stale comments corrected, design note + pins for payments dimension 2, a second jurisdiction proven configuration-only | – |
| NA-1..NA-6 | panic values never logged + redaction allow-list + AST guard; deposit-mismatch P1 alert; casino health fail-closed + jurisdiction-denial audit; bonus late-commit bounded re-scan; acceptance SA-3 wired to the status route; environment anchor must be `production` under the production guard | – |
| Namespace / visibility | provider-id disjointness checked at startup; AP002/AP003 deposit-tombstone key faults logged and alertable | – |
| NA-7 docs truth | Appendix A superseding registry row, HANDOVER 29–33 superseded notes, runbook index | – |

Reviews: security + ledger-finance independent, two rounds (records `closure-security-review-record.md`, `closure-ledger-finance-review-record.md`, `closure-security-rereview-record.md`, `closure-ledger-finance-rereview-record.md`): all **APPROVE WITH CONDITIONS**, no CRITICAL/HIGH; each condition is either closed (above) or classified below. Final whole-platform audit: `final-platform-audit-record.md` (earlier round, findings closed) plus the review rounds above on every later change.

## 3. Verification (final tree)
| Check | Result |
|---|---|
| Hosted CI on `50eded5` | **27/27** (build-test-lint incl. golangci-lint, 16 `-race` integration shards, timing lane, evidence gate, frontends, images, infrastructure). Two lint-only failures on the way (`1a5b587`: deprecated `ast.Object`, unused helper in the new panic-guard test) were fixed in test code only |
| Local, clean worktree at `1a5b587` (code identical to `50eded5` except that one test file's lint fix; its package test re-run green) | go build/vet clean; unit all packages 0 FAIL; integration all packages except httpserver/payments 74 ok, 0 FAIL; `internal/httpserver` integration ok (timing-sensitive tests pass on a quiet machine); `internal/payments` integration full, end to end, ok |
| Frontends (final tree) | backoffice build + 688 tests; b2c build + 185 tests |
| `migrate verify` at 0175 | up clean; 166 downs to zero and re-up clean |
| Backup/restore/PITR drill at 0175 | **138 PASS / 0 FAIL** |
| Notes | an intermediate local run, with several agents loading the machine, failed `TestINV_IO_1c` (a real analyzer hit: the new cursor helper's `Fetch` method name; renamed `NextBatch`) and 3 timing-lane tests (environmental contention; pass on the quiet re-run) | RPO 0 and RTO < 15 min are **not demonstrated**.

## 4. Remaining items, classified
### A — must close before technical freeze
**None remaining.** (Registry rescan: `final-closure-list-2026-10-17.md`, 21 A items at the start of the round, all closed or reclassified with evidence above.)

### B — accepted residuals (rationale, mitigation, detection)
| Residual | Why accepted / mitigation / detection |
|---|---|
| T6: holder of the application's actor-proof signing key can sign | ADR 0110 threat boundary; key in config, never in DB; detection none for a key holder; production start checks; KMS/per-environment keys are AWS-gated |
| Approval rows not proof-bound (ADR 0113 s9.3) | a SQL-only actor can forge `approved` but cannot make the app skip the (signed) gate or call the provider; **owner decision before launch** |
| Gate inputs (account/wallet status, risk rules) changeable by the runtime role | then the app signs a genuine allow on weakened inputs; changes are audited at app level; input governance (maker-checker) is an owner decision |
| Chargeback window between gate read and transition (F4-R2) | ms-wide; exposure ≤ min(withdrawal, shortfall) of the player's own held funds; **detected** by the C3 findings (P1), compensating governed paths |
| Runtime role can write ledger rows of any type with zero entries / pre-empt a tombstone key with a non-tombstone row (S-1/S-1b) | availability only; reversal remedy via owner-governed correction; pre-launch hardening bundle (key-shape check `tombstone:<provider>:<ref>` + deferred 'entries required' check) |
| Casino rollback outage if a casino provider id has payment rows (S-2) | closed going forward at startup (provider-id disjointness check); planted-row variant needs owner DDL to undo |
| Payout tombstone exemption depends on session visibility (S-3); first-deposit backstop bypassable by direct SQL via mismatched offer_version_id (S-4) | LOW; neither clears MA020 / attacker already has broader grant inserts |
| Cancelled first-deposit grant keeps the slot (C-4) | safe direction (no double credit); product decision (E) |
| Bonus late-commit horizon 15 min | deposit transactions are short; failure mode is a missed grant, never an over-credit |
| Environment anchor check one-directional | restore/verification tooling legitimately runs non-production processes on restored production-anchored copies; verified on the AWS staging run |
| SRF2R `cursor_flood` pauses a pair; up to 15 s/tenant/sweep lock wait under attack | loud P1; owner procedure to exclude the flood |
| U-5: exponent-18 amounts cannot be sent (int64 JSON numbers) | no such asset exists; backend string DTOs + NUMERIC(38,0) when crypto is scheduled |
| Static audit proof shows "reaches an audit write", not every branch | runtime proof for 7 families; AML own trails |
| Backups: physical backups contain the actor-proof key (owner/superuser T7) | inside the cloud trust boundary |

### C — provider-dependent
Real PSP (Nuvei is a candidate only; documentation UNVERIFIED because `docs.nuvei.com` is blocked by the environment network policy), PSP must be able to re-deliver deposit-reversal notifications else a governed tombstone path is needed (LF C2), standing detective check for unsigned tombstones before the first real-PSP tenant (C-1; go-live query is in the runbook), QueryStatus amount/asset and destination-echo conformance, real statement sources/credentials/sealing keys (SRF2-6), webhook scheme gaps G-01..G-11, casino CAS-G*, KYC-G*, sportsbook SB-G*, sports-data SD-G*, AML vendor, email/pager adapters (ALERT-DELIVERY-1), per-tenant provider-id configuration rows disjointness, sportsbook provider-id claim. Packages: `docs/provider-integration/`.

### D — AWS-dependent
IAM/runtime roles and search_path on the real account, RDS behaviour (logical restore on RDS may be BLOCKED), managed PITR and measured RPO/RTO, Secrets Manager, ECS deployment, real alert delivery, dedicated task role (SRSP-3), backup runner placement, KMS signing and per-environment keys, staging import keys (SRF2-6), environment-anchor behaviour per account, `pgaudit.log_parameter` OFF verification, load/soak at realistic volume, acceptance run (its ADMIN actor must be a platform admin for the status route). Plan: `docs/runbooks/aws-staging-deploy-plan.md` (PREPARED, NOT STARTED).

### E — legal / compliance / manual human decision
HDR-J-1 (no tenant fallback for player jurisdiction) and HDR-J-3 (player residence evidence) — gate wiring the player-scoped jurisdiction resolver (until then: fail closed when a jurisdiction-scoped rule exists); SRME-4/5/6, maker-checker for residence determinations, RG floor values (OQ-2), HDR-A6-3, first-deposit slot policy (C-4), approvals proof-binding (ADR 0113 s9.3) and gate-input governance, alert severity/volume, legal wording of the AML confidentiality duty, retention horizons, incident rota, per-brand licensing and activation (manual, four-eyes, audited).

## 5. Statement
**The provider-independent platform is technically frozen at the commit recorded above.** All provider-independent engineering the registry required is done and independently reviewed; the open items are accepted residuals with documented mitigation/detection (B), or depend on providers (C), AWS (D) or human/legal decisions (E). It is **ready to proceed to the AWS staging phase** per `docs/runbooks/aws-staging-deploy-plan.md` — which is **not started and needs your explicit go**, plus the human IAM actions listed in that plan. It is not ready for production, real money or a real provider, and nothing in this report says otherwise.
