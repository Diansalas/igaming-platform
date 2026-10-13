# Platform reconciliation: what is still required for technical production readiness and provider-integration readiness

- Author: `architect`. READ-ONLY review of `/home/user/igaming-platform`, branch `claude/focused-wright-jw88w9` @ `3145fb3` (migration head 0135, 270 files, gap-free by name).
- Method: I read the governance docs and ADRs, then checked them against the code: package listings, route tables, migrations, `backoffice/src`, `b2c/src`, `cmd/platform-api/main.go` and `deploy/`. I ran no tests and no builds.
- `pdftotext` and `pypdf` are not available. Blueprint citations therefore go through the repo's own mirror, `docs/architecture/01-requirements-inventory.md` (cited as "Inv §n"), plus `00-system-overview.md` for the NFR table.
- Nothing here is an approval, a legal statement or a stage authorisation. All providers are MOCK. AWS is OFF. There is no GitHub CI evidence (CI-BILLING-1).

## Four separate states (never conflate)

| State | Meaning | Current state |
|---|---|---|
| 1. Technical production readiness | Platform code, controls and ops are complete and verified, with every provider MOCK | NOT MET. The class A items below remain. |
| 2. Provider-integration readiness | Internal interfaces, state machines, reconciliation, governed exits and conformance suites exist, so a real adapter can be written without changing core code | PARTIALLY MET. PSP: strong (ADR 0111 sandbox gap analysis §A). Casino: good. KYC: good. AML screening: no interface. Sports data: catalogue-only interface. FX rate source: none. |
| 3. Real provider integration | A signed vendor, sandbox credentials and an adapter | NOT STARTED, not authorised. Nuvei is only a possible PSP candidate and is not a dependency anywhere in this plan. |
| 4. Brand activation | A manual, four-eyes, audited launch decision per tenant/brand (ADR 0112, `internal/launchgov`) | The mechanism is IMPLEMENTED (MOCK/local). No brand has been activated under it; legacy rows are `legacy_baseline`. A recorded launch decision is not a legal authorisation. |

## 1. Status summary per domain (estimated technical completion, MOCK providers)

| Domain | Est. % | Evidence / reason |
|---|---|---|
| Ledger, wallets, idempotency, reconciliation | 90 | **Implemented:** append-only double-entry ledger, multi-wallet, `(provider_id, provider_tx_id)` uniqueness and tombstones (ADR 0001/0019/0020/0082), and the hourly projection recompute (`internal/reconciliation/scheduler.go`, wired in `cmd/platform-api`). Invariant #12 is closed: 0132 guard, 0133 LA025 closure arm, 0134 check D. **Missing:** FX ConversionOperation (ADR 0117 B) and WITHDRAWAL-REVERSAL-1. BRAND-REHOME-HOLD-1 is an open integrity hole. |
| Deposits, withdrawals, payouts, four-eyes, audit | 85 (MOCK) | **Implemented:** INV-DEP-1 (0107), withdrawal DB state machine (0132), payout destination binding (B13, 0123/0126/0127), and the K1/K2/K3/M4/HSEC governed exits (0112-0115, 0124-0125). Audit is append-only with dual-scope RLS (ADR 0013). **Missing:** GAP-AAM (no governed exit), chargebacks, and payout reversal. |
| Identity, tenancy, RLS, RBAC, actor proof, runtime role, secrets, callbacks, rate limits | 70 | **Implemented:** RLS, runtime role and TEMP revoke (0116, ADR 0108), signed actor proof (0120, ADR 0110), launch governance (0128-0131), secret store with an `awssm` backend (ADR 0093/0094), webhook admission/GCRA (`internal/admission`, ADR 0097). **NOT IMPLEMENTED:** staff MFA/step-up (ADR 0017 status line; no TOTP/WebAuthn code), staff lifecycle (suspend/role change), keyed credential hashes T3/T4, and a general per-route API rate limit. |
| Casino (provider-neutral) | 75 (MOCK) | **Implemented:** abstraction, catalogue governance, launch bootstrap (ADR 0103), wager/win/rollback, tombstones, statement/consistency reconciliation, brand gameplay gate (0129). **Open:** Q-GP-6 round close and the CAS-* items. |
| Sportsbook | 50 (MOCK) | **Implemented:** catalogue, singles placement, exposure, jurisdiction gating, and settlement/void/re-settlement. **Gaps:** `Provider` has only `Catalogue()` (`internal/sportsbook/types.go:376`). Settlement runs only from the staff `simulate-settlement-event` route. No multiples, no cashout. |
| Payments (PSP abstraction) | 85 (MOCK) | **Implemented:** contract/capability model, mock, signed callbacks, statement reconciliation, destination-echo declaration and the M4 source contract. Non-MOCK is gated by the ADR 0111 S1-S16 list. |
| KYC | 70 (MOCK) | **Implemented:** interface, mock, documents, malware scan, outbox worker (0114), enforcement. **PARTIAL:** HD-KYC thresholds are open (legal). |
| AML | 5 | Only the ADR 0116 design exists. There is no `internal/aml` package. |
| RG | 40 | **Implemented:** self-exclusion with the person cluster, and the 0135 limits schema. **NOT IMPLEMENTED:** limit write APIs, usage measurement, enforcement and UI (ADR 0115 S2-S6); `internal/rg/limits.go` header says so. |
| Jurisdiction, market blocking | 55 | **Implemented:** registry, licences, resolution wired into casino/sportsbook play, `tenant_jurisdiction_configs` read by launch readiness. **Not wired:** `internal/operatingmarket` has no production importer, and `internal/geolocation` (MOCK) has no caller. In-platform market blocking at registration/deposit is therefore not enforced. **Missing:** licence status lifecycle. |
| Back office / operator console | 55 | **Implemented:** launch, alerts, kill switch, withdrawals, KYC, RG, audit, catalogue, bonus (partial). **NOT IMPLEMENTED:** UI for parked payments, reconciliation views, K1/K2/K3, HSEC and risk. No `backoffice/src` file references those routes. |
| B2B partner console | 25 | Only provider/casino config exists. There is no brand config editing, no domains, no partner API keys, and no revenue-share statements. |
| B2C player frontend | 65 (MOCK) | **Implemented:** auth, wallet, deposit, withdrawal with payout instruments, casino, sportsbook singles, `SiteUnavailable`. **Missing:** RG limits UI, KYC document upload UI and bonus UI. |
| Retail/POS | 0 | Architecture only (ADR 0035/0036). Owner-deferred. |
| Reporting / BI | 5 | No reporting module. CDC is deferred. |
| Ops: backup/DR, monitoring, alerting, runbooks | 50 | **Implemented (local):** `deploy/drill` (dev-dataset evidence only; no data-bearing backup ever restored). **NOT WIRED:** alert rules in `deploy/observability`; pending signals not emitted. ALERT-DELIVERY-1 is open: nobody is paged. Runbooks are present (launch/suspension, incident with `<NOT ASSIGNED>` recipients, restore, expand/contract). |
| Infrastructure (CI, IaC, migrations, health) | 60 | **Implemented:** Terraform modules, Docker, health/readiness, `migrate verify`. **Blocked:** GitHub CI (billing); self-hosted runner not authorised. AWS OFF. The production start now refuses a missing environment anchor (`db.ErrEnvironmentAnchorUnrecorded`; launch-gate item closed, see ADR 0115 implementation record). |

Overall, technically ready-to-launch for a casino-first B2C brand on MOCK providers: about 65-70%. With the sportsbook, AML and B2B scope included: about 55-60%.

## 2. Classification table

Classes:
- A: required before technical readiness, provider-independent.
- B: important, after core readiness.
- C: needs a real provider.
- D: human decision.
- E: legal/compliance manual launch gate.
- F: optional/future.
- G: done.

### A: required before technical production readiness (provider-independent)

| Item | Class | Why (source) | Dependency | Recommendation |
|---|---|---|---|---|
| **Staff MFA + step-up (ADR 0017/0018)** | A | Inv §3 four-eyes and least privilege. ADR 0017 calls the lack of staff MFA "a pre-launch blocker". The status is NOT IMPLEMENTED, and ADR 0115 S2 staff increases stay 403 until it ships. | Orchestrator/security default for step-up scope and max-age (ADR 0017 "Open decisions"), recorded as reversible. | Build first among the security items. Security review is mandatory. |
| STAFF-LIFECYCLE-1 (audited suspend, role change, offboarding) | A | CLAUDE.md audit rule; gap audit §1; HANDOVER §29. Today a compromised or leaving staff member cannot be disabled through a governed path. | None for the basic flow. The T3/H1 owner-only variant is D. | Small slice: backend, then security. |
| BRAND-REHOME-HOLD-1 (`brand_id` mutable on `player_accounts`/`wallets`) | A | Registry row. It can move a held wallet into a closed brand and undermines LA025 (ADR 0113 §9 item 10). | An ADR from the identity owner (architect co-signs). | Make `brand_id` immutable (DB trigger), then ledger-finance and security review. |
| ADR 0113 slice 4 (docs) and pending re-reviews of slices 2-3 | A | ADR 0113 §10/§12 | none | Close it out. |
| **ADR 0114 S1b** (alerts list keyset cursor; acked/resolved fields; OpenAPI) | A | ADR 0114 §9B: "MUST before production console". An open P1 can be pushed out of the 200-row view. | none | Required. |
| ADR 0114 S3 (parked payments queue and attempt detail) | A | ADR 0114 §6: day-one operator set S0-S8. Without it operators cannot see parked money except through the API. | S2 (done) | Required. |
| ADR 0114 S5 (reconciliation views, all streams) | A | ADR 0114 §6. Drift is a P1 (CLAUDE.md), so operators need visibility. | S4 (done) | Required. |
| ADR 0114 S7 (K1 grants console) | A | ADR 0114 §6: prerequisite for FI to act in K2/K3. | none | Required. |
| ADR 0114 S8 (K3 M1/M2 console) | A | ADR 0114 §6, day-one set | S3, S5, S7 | Required (MOCK-labelled). |
| **ADR 0115 S2 / S3 / S4** (RG write APIs, usage, enforcement) | A | Inv §6 (Blueprint RG: limits, cooling-off, time-outs) | S1 (done). S4 needs S3's EXPLAIN evidence. | Required. The S4 rollout makes brands non-transactable until OQ-2 values exist (E), so the orchestrator sequences enablement with the owner. |
| ADR 0115 S5 / S6 (RG UI, QA suite, runbook) | A | Inv §6. The B2C player must be able to set limits. | S4 | Required. |
| **ADR 0116 S1 + S3** (AML holds, case model, four-eyes) | A | Blueprint §4.7 via ADR 0116: "enforcement is your code" | none (S1 and S3 are released together) | Required. |
| ADR 0116 S4 (monitoring feed and rules, no values) | A | Blueprint §4.7: transaction monitoring with configurable rules | S1, S3 | Required. Ledger-finance has a blocking review. |
| ADR 0116 S2 (screening interface and mock) | A | Blueprint §4.7: sanctions/PEP screening. This is the interface a vendor plugs into, so it is required for provider-integration readiness. | S1 | Required. The vendor itself is C. |
| ADR 0116 S5 (SAR export, MOCK format) | A | Blueprint §4.7: "suspicious-activity report export" | S3 | Required. The FIU format is E. |
| ADR 0116 S6 / S7 (BO screens; readiness checks, launch criterion) | A | Blueprint §4.7 case queue; ADR 0116 Q1 | S1-S5 | Required. Values are E. |
| Operating-market wiring and in-platform market blocking at registration/deposit/withdrawal | A | Inv §5: "Mandatory, audited market blocking... enforced in-platform and logged". `IsRegistrationPermitted` has no caller. | none for the mechanism. Anjouan-prohibited list content is E. | Wire it (gap audit task 6). The geolocation signal stays MOCK until a vendor exists (C). |
| Production start refuses a missing `database_environment` anchor | A | ADR 0115 S1 launch-gate item (security re-review) | none | One-line fail-closed change plus a test. |
| ADR 0117 C1 (sports-data feed interfaces, deterministic MOCK feed, seals) | A (sportsbook scope) | Inv §9: every integration needs our adapter and state machine. Today settlement has only a staff simulate trigger. | none | Required before any sportsbook vendor. It is not on the critical path of a casino-only first launch. |
| ADR 0117 A1 (outbox runtime, no producers) | A (sportsbook scope) | Needed by C3. ADR 0117 §D. | none | Build together with C1. |
| ADR 0117 C3 (result ingest, mandatory hold, `SettleFromResult`) | A (sportsbook scope) | Replaces the simulate trigger. OQ-C2 makes the hold mandatory. | A1, C2 | Ledger-finance owns it. |
| WITHDRAWAL-REVERSAL-1 (governed `withdrawal_reversed` posting) | A for provider-integration readiness | Registry: "before real payouts"; sandbox gap S8 | WD043 reshaping migration, ledger-finance | The posting shape is provider-independent; the return mapping is C. |
| Docs/registry reconciliation (see §6) | A (hygiene) | CLAUDE.md stage rules require progress/active-stage updates | none | Do it next, in a docs-only change. |

### B: important, after core readiness

| Item | Class | Why (source) | Dependency | Recommendation |
|---|---|---|---|---|
| ADR 0114 S6c (kill-switch release expiry) | B | Fail-safe today; a recovery runbook exists | security (trigger change) | After S8. |
| ADR 0114 S9 (K2 console), S10 (HSEC console) | B | ADR 0114 §6 "S9-S11 follow" | S7 | MOCK-labelled. |
| ADR 0117 A2/A3 (ledger events, BI/audit/AML sinks) | B | Reporting freshness NFR (00-overview); not launch-critical | A1 | After A1. |
| ADR 0117 C2 / C4 (placement rules, trading ops UI, watchdog) | B | Sportsbook ops | C1 | Sportsbook go-live track. |
| ADR 0117 B0-B4 (FX ConversionOperation, MOCK rates) | B | Not needed for a single-currency launch. KYC-FX-AGG-1 is DEFERRED; OQ-B4 is commercial. | B0 first; A1 | Build only when multi-asset conversion is a product need. |
| Licence lifecycle (suspend/expire/renew, continuous expiry check) | B | Gap audit task 6. Manual suspension already exists via launchgov. | none | After market blocking. |
| General per-route API rate limiting; `decodeJSON` trailing-value fix | B | ADR 0112 residual; ADR 0114 §11 LOW | none | Before public exposure, together with WEBHOOK-EDGE-1 (AWS). |
| Alert rule wiring (promtool) and emitting the pending signals (drift, recon staleness, kill switch, pool) | B | OPS-READINESS-LOCAL row | backend (AWS) | Emit the metrics now; wire them at deploy. |
| Risk REVIEW queue and risk rules UI | B | Inv §1 "risk/case queues" | none | After AML (they share the case pattern). |
| Basic regulatory/financial reporting module | B | Inv §1 "Regulatory and financial reporting" | A1/A3 | Formats are E. |
| Partner console brand config, domains, versioned brand config | B | Inv §7; ADR 0112 §15 | none | B2B track. |
| B2C KYC document upload UI and bonus UI | B | Inv §1 | Vendor SDK choice may supersede the upload UI | — |
| Self-exclusion auto-void consumer and ADR 0042 G-2 configurability | B | Decided in ADR 0042; register marks it DEFERRED | CAS-WIN-IDEMP-1 | Before bonus-funded stakes. |
| Casino open items (CAS-GAME-KILL-BET-1, CAS-BET-REQUIRES-BOOTSTRAP-1, CAS-WIN-IDEMP-1, CAS-RECON-SCALE-1) | B | HANDOVER §11 | none | Before a real aggregator. |
| PAY-K3-FOLLOWUPS-1, MANUAL-ADJ-LINK-1, MA020-K2-VISIBILITY-1, STMT-TABLE-INSERT-RLS-1 | B | Register: "before live PSP with polling" | security/architect | Before a sandbox PSP. |
| R1 index `(tenant_id, created_at, id)`; `source_label` DB CHECK | B | ADR 0114 §12 items 6 and 9 | migration 0136+ | Low effort. |

### C: needs a real provider

C items: real PSP adapter, sandbox credentials, status/return mapping, echo semantics, sealed statement source, M4 non-MOCK (ADR 0111 S2-S16); chargebacks; casino aggregator adapter; sports-data/odds/results feed; KYC vendor and SDK; sanctions/PEP list vendor; geolocation/IP vendor; FX live rate source; crypto custodian (ADR 0008); real alert delivery channel adapter. Section 3 has the full lists.

### D: human decision

| Item | Blocks |
|---|---|
| T3/T4/H1/T8 identity-store hardening authorisation | Full closure of THREAT-MODEL-ARBITRARY-SQL-1 |
| Launch-risk acceptance of the HIGH residual (ADR 0113 §9.2) | Real launch |
| Q-GP-6 casino round close; Q-GP-2/3/4 | Casino closure completeness |
| GAP-AAM; Q-HSEC-1/2/3; O-1/O-2; M-3; HD-R15-1..9; D-REG-1; `evidence_ref_hash` | Non-MOCK payments |
| D-0104-1 | Full closure of H1 |
| BACKUP-ROLE-1 | — |
| ALERT-DELIVERY-1 recipients | — |
| CI-BILLING-1 / MAC-RUNNER / BRANCH-PROTECTION-1 | — |
| AWS-STAGING, PLAT-ROLESPLIT-1 deployed verification | — |
| WD-RG-1 | — |
| Sportsbook multiples/cashout scope (ADR 0117 C5) | — |
| Retail/POS, Wave 4 | — |
| Vendor selection and contracts | — |
| PRH-GATE / next-stage authorisation | — |

### E: legal/compliance launch gates (configuration content, not code)

HD-KYC-1..8, HD-AML-1..9, ADR 0115 OQ-2 RG values (Anjouan), HDR-SB-1, HDR-J-7/8/9, HDR-M-1/2, HD-CTF-1..9, HQ-E1-1/3/4, HD-RET-1, K3 note retention, the licence beyond Anjouan, hosting AUP, the SAR/FIU format, and the per-jurisdiction report formats. Under ADR 0112 these become attestations on the launch record. They are never code.

### F: optional/future

FX conversion product (until needed), AI agents (ADR 0089), schema/db-per-tenant isolation, CDC/ClickHouse, gamification/tournaments/CRM/affiliate (docs 17-32), asymmetric actor proof, jackpots/live-casino specifics, retail (until authorised).

### G: already complete (MOCK/local evidence)

| Capability | Evidence |
|---|---|
| Ledger, multi-wallet, idempotency, tombstones, hourly recompute | ADR 0001/0019/0020; `internal/reconciliation/scheduler.go` |
| Deposits INV-DEP-1 | 0107 |
| Withdrawal SM and hold invariant | 0132-0134 |
| Payout binding, M4, HSEC, RR-1 | 0123-0127; ADR 0111 |
| K1/K2/K3, kill switch | 0105, 0112-0115 |
| Launch authorisation (TENANT-STATUS-AUTHZ-1 closed) | 0128-0131, `internal/launchgov`, BO `features/launch`, B2C `SiteUnavailable` |
| Signed actor proof | 0120 |
| Runtime role, TEMP revoke | 0116 |
| Webhook admission | `internal/admission` |
| Secret store | `internal/secretstore` |
| KYC mock, outbox, enforcement (partial) | `internal/kyc` |
| Self-exclusion | `internal/rg/rg.go` |
| Risk engine | `internal/risk` |
| Casino lifecycle and rollback | `internal/casino` |
| Sportsbook singles and settlement/void | `internal/sportsbook/settlement.go` |
| Bonus waves 1-3 with ledger mirror; HR-9 removed | `internal/ledger/bonus_mirror.go:26` |
| Audit, including the tenant-visible platform-actions view | ADR 0104 |
| Staff-id redaction | ADR 0114 S0 |
| Reconciliation read routes R1/R1b/R2 | ADR 0114 §12 |
| Alerts and kill-switch consoles | ADR 0114 S1/S6 |
| Durable alerting (log sink) | ADR 0102 |
| Local backup/restore drill and runbooks | `deploy/drill`, `docs/runbooks` |
| AWS IaC (not deployed) | `deploy/aws` |
| Health/readiness | `internal/httpserver/health.go` |

## 3. Provider-dependent work lists (all C; none authorised; vendor selection is D)

**PSP.** Nuvei is only a candidate.
- Adapter against `internal/payments/contract.go`.
- Sandbox credentials via `providercred`/secret store.
- Callback signing, rotation and replay rules (S-3).
- Tenant context mapping (B13B-8).
- Destination-echo declaration and conformance.
- Status map (unknown never means success).
- Return/reversal/chargeback mapping (needs WITHDRAWAL-REVERSAL-1).
- Sealed statement source (T10 review).
- M4 source contract C1-C11.
- Tokenised instrument registration (PCI review).
- HD-R15 launch flags.
- Durable echo-absent success record.
- Read-only reconciliation credential (HD-CTF-10).

**Casino aggregator.**
- Adapter for catalogue, launch bootstrap and wallet callbacks.
- Webhook signing per provider.
- Statement source for casino reconciliation.
- Round-close signal (pending Q-GP-6).
- RTP variants and jurisdiction blocklists from the vendor catalogue.
- Per-tenant credentials.

**Sportsbook / sports data.**
- Odds, event-state and results feed adapters behind the ADR 0117 C1 interfaces.
- Seals and ingest identity (launch-blocking residual for a non-MOCK feed).
- Widget/iframe integration (Inv §1).
- Settlement statement source.
- Push-admission domain.
- Cashout and multiples need a separate ADR (C5).

**KYC.**
- Vendor adapter on `internal/kyc/provider.go`, plus the SDK/hosted flow for document capture.
- Webhook signing.
- Mapping of decision reasons to the normalised codes.
- Per-tenant credentials.

**AML.**
- Sanctions/PEP screening vendor behind the ADR 0116 S2 interface (not built yet).
- Recurring re-screen.
- Subject-reference mapping (HD-AML-6 privacy review).

**Other.**
- Geolocation/IP vendor (`internal/geolocation` MOCK).
- FX rate source (after ADR 0117 B).
- Crypto custodian (ADR 0008).
- Alert delivery channel (email, pager or chat) with dedupe on `<alert_id>:<step>`.

## 4. Human decisions that genuinely BLOCK the next A-class task

No owner or legal decision blocks the next A-class engineering tasks. ADR 0114 D-OPS-1/2, ADR 0115 OQ-1..3, ADR 0116 Q1-Q8 and ADR 0117 OQ-* are already recorded as reversible orchestrator defaults. The real gates are narrower:

1. **Staff MFA (ADR 0017).** Someone must record the step-up scope, max-age and factor type (TOTP vs WebAuthn) before the code is written. This is a security/orchestrator reversible default; it does not need the owner.
2. **BRAND-REHOME-HOLD-1.** The identity owner must write an ADR. This is an architect-level decision, not a human gate.
3. **ADR 0115 S4 production enablement** (not the merge). Once it is on, every brand, including the Anjouan B2C brand, refuses deposits and wagers until compliance supplies OQ-2 values. The owner and the orchestrator must sequence the rollout.
4. **Stage authorisation.** PRH-GATE is still OPEN in the register. The basis for continuing provider-independent work is the owner MASTER DIRECTIVE of 2026-10-11. The orchestrator should confirm that it covers the ADR 0114-0117 slices before starting them.

T3/T4/H1 is D and blocks only its own hardening item, not the A list.

## 5. Recommended execution order (dependency order)

1. **Docs reconciliation** (§6): progress.md, active-stage.md, HANDOVER, human-decision-register, ADR status headers, and registry rows for 0113 S2-3, 0114 S2/S4 and 0115 S1. Docs-only, no risk.
2. **Fail-closed environment anchor at production start**, plus **ADR 0113 slice 4** close-out.
3. **BRAND-REHOME-HOLD-1**: ADR, then a migration making `brand_id` immutable or brand-gated. Reviewers: ledger-finance and security.
4. **ADR 0115 S2 then S3**: RG write APIs, then usage with EXPLAIN evidence. S3 must land before S4.
5. **ADR 0114 S1b, S3, S7, then S5, then S8.** S1b, S3 and S7 can run in parallel with step 4 because they touch different files. Together they complete the day-one operator console.
6. **Staff MFA/step-up (ADR 0017)**, after a recorded default. STAFF-LIFECYCLE-1 lands alongside it. This also unblocks ADR 0115 staff increases.
7. **ADR 0115 S4 and S5/S6**: RG enforcement and UI. The merge is not the same as enablement (see §4 item 3).
8. **ADR 0116 S1 + S3**, released together, then **S2 → S4 → S5 → S6 → S7**.
9. **Operating-market wiring and market blocking**, then licence lifecycle (B).
10. **Sportsbook track**: ADR 0117 A1 ∥ C1, then C2, then C3, then C4. Then WITHDRAWAL-REVERSAL-1 ahead of any sandbox PSP.

After this sequence, technical readiness additionally needs green CI evidence (D: CI-BILLING-1 or the runner), deployed PLAT-ROLESPLIT-1 verification and managed PITR (D: AWS), and ALERT-DELIVERY-1 (D/C).

## 6. Contradictions and stale claims found (flagged)

1. **Staff MFA.** `platform-completion-gap-audit-2026-10-10.md` §1 says "Staff MFA / step-up: IMPLEMENTED (ADR 0017/0018)". ADR 0017's status line says "NOT IMPLEMENTED". `internal/` has no MFA/TOTP code, and ADR 0115 (lines 63 and 488) depends on it being absent. **The audit is wrong. This is a launch blocker.**
2. **HR-9.** Gap audit §5 says "HR-9 guard rejects postings until the expense generator exists". HR-9 has been removed (`internal/ledger/bonus_mirror.go:26`). Stale.
3. **ADR 0112 status header.** It says "DESIGN ONLY... every item is NOT IMPLEMENTED". Slices 1-6 are merged (0128-0131, `internal/launchgov`, BO `features/launch`).
4. **ADR 0113 status header.** It says "no code or SQL exists". 0132-0134 are merged, and its own §12 says so.
5. **ADR 0114 status header.** It says "Nothing in this ADR is implemented". S0, S1, S2, S4, S6 and S6b are merged (§9A, §11, §12).
6. **Stale migration numbers.** ADR 0116 says numbers are allocated "after head 0132", and ADR 0114 §12 item 9 says "next free number 0134". The head is 0135; 0136+ is next.
7. **human-decision-register.md.** TENANT-STATUS-AUTHZ-1 is listed OPEN but is CLOSED per registry ADR0112 (`ChangeStatus` was removed). SIGNED-ACTOR-PROOF, Q-GP-5 and Q-GP-1 say "merge pending" but 0120 and 0121 are merged. The row "HD-PRH2 H(8) OPEN" duplicates H8-BRAND-SWEEPER-GATE-DECIDED.
8. **HANDOVER.md.** The header snapshot is dated 2026-10-06 with "migrations 0001..0121". §36n says 0131. The actual head is 0135. HANDOVER has no entries for ADR 0113 S1-3, 0114, 0115 S1, 0116, 0117 or OPS-READINESS-LOCAL. Its inventory line 151 still says "0001..0117".
9. **progress.md and active-stage.md.** The last progress section is PRH (2026-09-28). The newest active-stage note is dated 2026-10-05. Neither records 0118-0135. This contravenes CLAUDE.md's update rule.
10. **task-registry.md.** There are no rows for the merges of ADR 0113 slices 2-3 (0133/0134), ADR 0114 S2/S4, or ADR 0115 S1 (0135). The ADR texts are currently the only record.
11. **Gap audit superseded items.** These now exist, so the audit should be marked superseded in part:
    - reconciliation read API beyond casino (R2);
    - BO alerts and kill-switch UI;
    - backup/restore drill;
    - launch/incident/expand-contract runbooks;
    - launch-authorisation control.
12. **ADR 0112 §15 vs readiness.** §15 defers an "operating-market policy" readiness check, and launch readiness reads `tenant_jurisdiction_configs`. Yet Inv §5 makes market blocking mandatory and nothing enforces it at registration. The gap is real; it is not a documentation error.
