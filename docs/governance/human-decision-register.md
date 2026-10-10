# Human decision register

Status: LIVE register created 2026-10-06 (registry row `HANDOVER-LIVE-2026-10-06`). Before this file existed, human decisions were
recorded only as `HD-*` / `HQ-E1-*` / `HD-CTF-*` rows in [`task-registry.md`](task-registry.md) and in ADRs
0039, 0042, 0044, 0098, 0105 (decision responses). **The task registry and the ADRs remain authoritative**; this file is a
consolidated index of them. It is **append-only in spirit**: when a decision is made, add a dated line to the change log below
and update the row's Status; never delete or rewrite a historical decision (record a superseding entry instead).

Nothing here is a legal or regulatory approval. Owner = the human who must answer; "orchestrator" never decides for them.

Columns: `ID | Question | Current Decision | Status | Blocks | Owner`. Mirrored in [`../HANDOVER.md`](../HANDOVER.md) section 31.

### DECIDED

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| THREAT-MODEL-ARBITRARY-SQL-1 | Is a stolen/compromised `igaming_runtime` credential (arbitrary SQL) inside the production threat model for DB-enforced controls? | YES (2026-10-05); TEMP fix alone insufficient | DECIDED; mitigation PARTIALLY MITIGATED | Closing it fully needs T3/H1, T4, T5, T8 decisions | owner + security |
| SIGNED-ACTOR-PROOF | Implement a DB-verified signed actor proof? | AUTHORIZED, smallest mechanism only (2026-10-06) | DECIDED; implemented on branch `prh2-r5-signed-actor-proof@8863e31`, merge pending | Merge | owner |
| H-W1 | Observe suspended/closed tenants in reconciliation? | YES, read-only evidence; never auto dispatch/release/cancel/settle (2026-10-05) | DECIDED; merged `741569e` | - | owner |
| HD-CTF-10 | Credential treatment when observing a closed tenant | Closed tenants stay observable while unresolved player funds exist; real PSP uses a read-only/reconciliation-scoped credential where supported (2026-10-05) | DECIDED | Real-PSP architecture detail | owner |
| R3-GAME-POSTINGS-NONACTIVE-1 | Gameplay postings on non-active tenants | FAIL CLOSED for NEW gameplay postings (2026-10-05); migration 0118 merged | DECIDED; merged | - | owner |
| Q-GP-5 | Terminal stake returns on non-active tenants | ALLOWED for valid existing rounds; new wagering still refused (2026-10-06) | DECIDED; implemented on branch `prh2-r5-stake-return-closure@35cd2fb`, merge pending | Merge | owner |
| Q-GP-1 | Tenant closure with open rounds | REFUSED while open rounds exist (2026-10-06); casino half not representable (Q-GP-6) | DECIDED; branch, merge pending | Merge | owner |
| SANDBOX-BEFORE-ALERT-DELIVERY-1 | Sandbox PSP adapter before real alert delivery? | YES, sandbox only, synthetic non-real-money tenant; Class-B prerequisites first; ALERT-DELIVERY-1 remains a production launch blocker | DECIDED; adapter NOT authorized yet | Sandbox adapter start | owner |
| H-SEC-5 | HTTP deposit initiation vs non-active tenant/brand | **FAIL CLOSED (owner/security ruling, 2026-10-08):** HTTP deposit initiation must fail closed unless the tenant and brand are active/eligible at the time of initiation; an inactive/closed tenant or brand must not create a deposit/payment attempt through the HTTP initiation path. No broader policy implied. | DECIDED; IMPLEMENTED against MOCK, merged `3643cb0` (task-registry `H-SEC-5-11-IMPLEMENTED-2026-10-08`) | tenant-closure flow (with HD-CTF) | owner/security |
| H-SEC-11 | HTTP withdrawal initiation vs non-active tenant/brand | **FAIL CLOSED (owner/security ruling, 2026-10-08):** HTTP withdrawal initiation must fail closed unless the tenant and brand are active/eligible at the time of initiation; an inactive/closed tenant or brand must not create a withdrawal/payout attempt through the HTTP initiation path. No broader policy implied. | DECIDED; IMPLEMENTED against MOCK, merged `3643cb0` | tenant-closure flow (with HD-CTF) | owner/security |
| ADR-0094-500MS | Keep the 500 ms timing bound | KEPT unchanged | DECIDED | - | owner/security |
| HQ-E1-2 | E1 item (p2) | Confirmed | DECIDED | - | owner |
| CLEANUP-2026-10-05 | Scratch DB/worktree cleanup | Accepted; 44 UNKNOWN DBs preserved | DECIDED | - | owner |
| TRIGGER-SEARCH-PATH-1 route | How to close the TEMP shadow path | REVOKE TEMP (0116, ADR 0108) | DECIDED; implemented | Deployment verification per environment | owner + security |
| HD-0095-1 / LEDGER-MANUAL-ADJ-4EYES-1 | Force-resolve authority; manual adjustments | Recorded in ADR 0098 (2026-09-28) | DECIDED | - | owner |
| HD-PRH2-9, HD-PRH2-10 | Closed-tenant funds, E1 alert kind | Recorded in ADR 0105 (2026-10-04) | DECIDED | - | owner |
| B13-DECIDED-2026-10-08 | Payout destination binding | **DECIDED (owner, 2026-10-08)** - ADR 0095 s44 decisions 1-8: independent of UNBOUND-RESOLVE-1; before ANY non-MOCK payout a player-bound verified payout instrument resolved server-side; callbacks never determine/replace/change the destination; missing/ambiguous => fail closed/parked; provider-reported mismatch => anomaly + alert, NO progression; provider-neutral instrument abstraction; NO normal staff override (exceptional = four-eyes + positive evidence + audit); MOCK may stay flexible but no weaker model may leak to production | DECIDED; implementation in progress (task-registry `B13-*`) | Non-MOCK payouts | owner |
| PAY-PAYOUT-UNBOUND-RESOLVE-1-DECIDED-2026-10-08 | Unbound payout resolution | **DECIDED (owner, 2026-10-08)** - ADR 0095 s44 decisions 9-12: never automatic; stays parked/held until positively attributable; manual resolution only via controlled four-eyes governance with complete audit; never releases/settles/delivers without positive evidence sufficient under policy | DECIDED; implementation in progress | Non-MOCK payouts | owner |
| HSEC-APPROVED-HOLD-RELEASE-1-DECIDED-2026-10-08 (HD-CTF-6 for approved holds) | Approved withdrawals on a suspended/closed tenant or brand | **DECIDED (owner, 2026-10-08)** - ADR 0095 s44 decisions 13-18: no automatic release; hold remains; a controlled staff resolution path IS allowed; resolution/release/cancel requires four-eyes; no unilateral single-staff release/cancel; kill-switch semantics (inactive tenant/brand = no normal payout submission) intact | DECIDED; implementation in progress | Tenant suspension/closure flow | owner |
| H8-BRAND-SWEEPER-GATE-DECIDED-2026-10-08 | Brand eligibility in cascade-child creation and sweeper | **DECIDED (owner, 2026-10-08)** - ADR 0095 s44 decisions 19-23: check at cascade-child creation AND again at sweeper claim/dispatch; an existing child whose brand becomes inactive stays deferred and is never dispatched; no automatic cancel/release; tenant and brand are separate policy checks, never conflated | DECIDED; implementation in progress | Real providers | owner/security |
| ANOMALY-APPLIED-1-OPTION-A-DECIDED-2026-10-08 | PAY-RECEIPT-ANOMALY-APPLIED-1 | **DECIDED (owner, 2026-10-08): OPTION A** - ADR 0095 s44 decisions 24-30: one-time locked re-attribution + missing audit-row repair; attribution/audit repair ONLY (no money, balance, hold, settlement, financial entry or provider-state change); locked, idempotent, fully audited; exactly one receipt and one target attempt; ambiguous/insufficient evidence => no repair, receipt stays anomalous; not usable as a general reassignment mechanism | DECIDED; implementation in progress | Real provider acceptance | owner (ledger-finance to review the implementation) |

### NEEDS USER (owner / architect)

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| Q-GP-6 | What signal closes a casino round (open round not representable)? | none | OPEN | Casino half of closure guard; win entitlement on closure | owner + architect |
| Q-GP-2 | Add a `tenant_not_active` class to `casino_callback_rejections`? | none | OPEN | casino_consistency finding for refused win/rollback | owner + ledger-finance |
| Q-GP-3 | Durable provider-claim evidence for the public casino webhook 401 before verification | none | OPEN | Evidence of refused provider claims | owner + architect |
| Q-GP-4 / R3-RECON-NONACTIVE-STREAMS-1 | Run `casino_consistency`/`sportsbook_settlement` for non-active tenants? | none | OPEN | Cross-check coverage | owner + architect |
| H1 / T3 | Authorize identity-store hardening (keyed refresh-token hashes, keyed staff credential check, owner-only staff lifecycle) | none | OPEN | Full closure of THREAT-MODEL-ARBITRARY-SQL-1 | owner + security |
| T4 | Keyed hash for `player_credential_tokens` | none | OPEN | Player account takeover residual | owner + security |
| T8 | Extend signed proof to other dual-control flows (0105, 0096, 0103, 0047, 0026/0034, 0063, 0086/0089) | none | OPEN | Closure of the threat model | owner + security |
| TENANT-STATUS-AUTHZ-1 | `ChangeStatus` has no authz and no HTTP route | none | OPEN | Any tenant-closure flow | owner + security |
| MA020-K2-VISIBILITY-1, STMT-TABLE-INSERT-RLS-1 | K2-session visibility on reference evidence; statement-table insert RLS | none | OPEN | Before a live PSP with status polling | security + architect |
| PAY-PAYOUT-CONTRADICTION-HOLD-1 | Operator resolution path for refused payout parks | none | OPEN | Live payouts | owner + architect |
| PAY-PAYOUT-UNBOUND-STANDING-1 / PAY-PAYOUT-UNBOUND-RESOLVE-1 | Standing recon coverage + R-K3-8 resolution path for unbound payout holds; allocation never a route for payouts | none | OPEN (engineering; owner/architect to confirm resolution semantics) | Any non-MOCK payout, sandbox payouts included | owner + architect + ledger-finance |
| PAY-PAYOUT-BOUND-CLEAR-1 / R-6 | DONE 2026-10-07 (merged); residual follow-ups PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1, PAY-DEPOSIT-MISMATCH-ALERT-1 (gate before real-provider deposit), CALLBACK-AUDIT-2 are engineering, LF-directed; no owner decision pending | none | DONE / follow-ups OPEN | Any non-MOCK payout / real deposit | payments + LF |
| PAY-RECEIPT-ANOMALY-APPLIED-1 | Receipt closed `anomaly_*` (attempt_id NULL, terminal-cell audit rows missing) although its evidence was applied in a reference-binding race | **Decision brief prepared 2026-10-08:** [`anomaly-applied-1-decision-brief.md`](anomaly-applied-1-decision-brief.md). ledger-finance to choose (A) one-time locked re-attribution of the receipt to the applying attempt + write the missing audit row once, or (B) accept as audit-only gap for real-provider acceptance (and whether a re-attribution writes its own audit row). No financial effect identified. | AWAITING LEDGER-FINANCE DECISION (not implemented) - SUPERSEDED 2026-10-08: owner decided OPTION A, see ANOMALY-APPLIED-1-OPTION-A-DECIDED-2026-10-08 | Any real provider | ledger-finance |
| HSEC-APPROVED-HOLD-RELEASE-1 (brief: `hsec-approved-hold-release-1-decision-brief.md`) | Approved withdrawals on a suspended/closed tenant or brand have no release path (staff submit refused by H-SEC-11; Reject only `pending_review`, Cancel only `requested`) - is a release path wanted? | None decided; funds frozen (not lost) until reactivation or ADR 0107 CT-PRE | AWAITING OWNER (with HD-CTF-6) - SUPERSEDED 2026-10-08: see HSEC-APPROVED-HOLD-RELEASE-1-DECIDED-2026-10-08 | Any tenant-suspension/closure flow | owner + security |
| H(8) BRAND-SWEEPER-GATE (brief: `h8-brand-sweeper-gate-decision-brief.md`) | Must a suspended/closed BRAND be gated like a non-active tenant in sweeper T2 / phase-C cascade-child insert (today tenant-only)? | None decided; HTTP initiation now gates brand | AWAITING SECURITY/OWNER - SUPERSEDED 2026-10-08: see H8-BRAND-SWEEPER-GATE-DECIDED-2026-10-08 | Real providers | security + owner |
| B13 | Payout destination binding (decision brief: `docs/governance/b13-decision-brief.md` + `b13-decision-brief-supplement.md` (Q1-Q8); statement in its s15) | none | AWAITING DECISION (brief prepared 2026-10-07) - SUPERSEDED 2026-10-08: see B13-DECIDED-2026-10-08 | Real payouts | architect + owner |
| ALERT-DELIVERY-1 / HD-PRH2-4-OPS | Real recipients, on-call, channel | none | OPEN | Production; first human-notification kind (ADR 0102 s18.4) | owner |
| ADR 0094 timing-lane criterion | Environment-calibrated relative assertion / p95 + p100 ceiling / controlled runner | bound unchanged | OPEN | Reliable green timing lane | owner + architect + security |
| HD-PRH2-8 | Below-threshold four-eyes semantics | interim enforced | OPEN (non-blocking) | - | owner |
| HD-PRH-1 | Are tenant slugs confidential? | none | OPEN | WEBHOOK-PATH-TOKEN-1 | owner |
| HD-PRH2 H(8) | Gate suspended/closed BRAND like a non-active tenant in resolution-only sweep? | none | OPEN | - | security |
| WD-RG-1 | Add RG/Risk gate on withdrawal to scope? | none | OPEN | Withdrawal compliance | owner |
| NULL-ARM-WRITE-1 | Launch-blocking or not | hardening item (security verdict) | OPEN | Second tenant / second platform admin | owner + security |
| CI-BILLING-1 | GitHub billing | do NOT modify; local evidence only | OPEN (owner/account) | Green GitHub CI | owner |
| MAC-RUNNER | Authorize Mac self-hosted runner setup | not authorized | OPEN | Self-hosted CI evidence | owner |
| BRANCH-PROTECTION-1 | Protect `main`, enforce CODEOWNERS | none | OPEN | - | owner |
| AWS-STAGING | Authorize next staging deployment (naming the commit) | not authorized; AWS OFF | OPEN | PLAT-ROLESPLIT-1 verification | owner |
| ACCESS-ANALYZER-CHECK-1, HD-10.3-2, DEPLOY-FPKEY-1 | AWS account/IAM/key-delivery actions | none | OPEN | Next staging deployment | owner / AWS admin |
| Vendor/PSP/custodian selection, contracts, production credentials, production launch | Standing human-only items | none | OPEN | Provider and production phases | owner |
| PRH-GATE | Authorize final PRH-2 gate / next stage | stopped, not authorized | OPEN | Next stage | owner |

### NEEDS LEGAL-COMPLIANCE

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| HQ-E1-1 | KYC submissions for suspended/closed tenants | none | OPEN | E1 on a real tenant | legal/compliance |
| HQ-E1-3 | Jurisdiction deadlines for KYC | none | OPEN | E1 on a real tenant | legal/compliance |
| HQ-E1-4 | KYC outbox retention | none | OPEN | Production | legal/compliance |
| HD-KYC-1..8 | KYC thresholds (ADR 0096 s4) | none | OPEN | KYC-ENFORCE-1 completion | legal/compliance + owner |
| HD-CTF-1..9 | Closed-tenant player-funds policy (ADR 0107 s12) | none; mechanism DESIGN ONLY | OPEN | Any closed-tenant funds mechanism | legal/compliance + owner |
| HDR-J-7/8/9, HDR-M-1/2 | Jurisdiction items (ADR 0044, stage-4i-exit-register) | none | OPEN | Jurisdiction go-live | legal/compliance |
| HDR-SB-1 | Sportsbook go-live gate (ADR 0083 s8.2) | none | OPEN | Sportsbook go-live | legal/compliance + owner |
| HDR-J-1..6 residuals | Legal-review residuals | answered in ADR 0042 with residuals | OPEN (residual) | - | legal |
| K3 note retention | PII retention for K3 resolution notes | none | OPEN | Production | legal + security |
| Licence / jurisdictions beyond Anjouan | Gambling licence decisions | Anjouan only | OPEN | Production launch | owner + legal |
| Hosting AUP | Written gambling-AUP confirmation from the hosting provider | none | OPEN | Production deployment | owner + legal |

### DEFERRED

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| Wave 4 bonus engine | Start Wave 4? | not authorized | DEFERRED | Bonus roadmap | owner |
| ADR 0042 G-2 / self-exclusion auto-void | Implement configurability and consumer | decided in ADR 0042, not implemented | DEFERRED | CAS-WIN-IDEMP-1 before bonus-funded stakes | owner |
| Retail/POS | Build retail | architecture only | DEFERRED | Retail roadmap | owner |
| AI agents (ADR 0089) | Product AI | architecture only | DEFERRED | - | owner |
| Crypto custodian (ADR 0008) | Select custodian | interface only | DEFERRED | Crypto wallets | owner |
| Asymmetric actor proof; nonce pruning automation | Improvements to ADR 0110 | recorded | DEFERRED | - | security |
| Schema/database-per-tenant isolation | Tighten isolation | only when a partner/jurisdiction requires | DEFERRED | - | architect |
| KYC-FX-AGG-1 | FX source | none exists | DEFERRED | Aggregated KYC thresholds | owner |

### INFORMATIONAL

| ID | Question | Current Decision | Status | Blocks | Owner |
|---|---|---|---|---|---|
| PRH-2 | Is PRH-2 complete? | NO | INFORMATIONAL | - | orchestrator |
| Providers | Any real provider? | ALL MOCK; no production credentials, no real money | INFORMATIONAL | - | - |
| AWS | State | OFF; nothing deployed; PLAT-ROLESPLIT-1 verification prepared, not executed | INFORMATIONAL | - | - |
| CI | State | GitHub blocked by billing; local evidence only; Mac runner prepared, not installed | INFORMATIONAL | - | - |
| Preserved artefacts | Scratch DBs / worktrees | 44 UNKNOWN DBs, 52 worktrees preserved; keep `igaming_orch_local`, `igaming_platform_ci_local` | INFORMATIONAL | - | - |

## Change log (append only)

| Date | Entry |
|---|---|
| 2026-10-06 | Register created from the task registry rows `HUMAN-DECISIONS-ROUND3`, `DECISIONS-PRH2-CLEARANCE-2026-10-05`, `DECISIONS-PRH2-FINAL-GAMEPLAY-SECURITY-2026-10-06`, the previous HANDOVER "Human decisions awaiting the owner", and ADR 0095 section 40.5-40.6 (Q-GP-1..6). |
| 2026-10-08 | Owner approved B13 (decisions 1-8), PAY-PAYOUT-UNBOUND-RESOLVE-1 (9-12), HSEC-APPROVED-HOLD-RELEASE-1/HD-CTF-6 (13-18), H(8) (19-23), PAY-RECEIPT-ANOMALY-APPLIED-1 Option A (24-30); recorded in ADR 0095 s44; implementation tracked in task-registry (`GOVDEC-2026-10-08-RECORDED`). No other decision was made. |
| 2026-10-09 | OPEN owner/architect questions raised by the execution cycle (NOT decided): Q-HSEC-1 (kill switch does not block hold release), Q-HSEC-2 (decisions 13-18 cover approved holds only, or also requested/pending_review on a non-active tenant), Q-HSEC-3 (ignore a tenant's stricter policy rows while non-active); O-1/O-2 for the receipt-site gate and anomaly (callback decline for an inactive brand/tenant final vs create-but-keep-deferred; stale deferred child expiry); Q1 (instrument state as a settlement gate; security + LF recommend no gate); M-3 (governed exit for `destination_integrity_failure`); D-7 (evidence standard before any non-MOCK M4); echo declaration requirement for non-Synthetic adapters; B2B own-licence M4 self-service. |
| 2026-10-09 | Cycle RR1-FRONTEND-RESOLVE: NO new human decision was made or assumed. Briefs prepared (see `docs/governance/rr1-cycle-decision-briefs.md`): instrument state as a settlement gate (current behaviour: not gated; owner to choose A/A+/B), governed exit for `destination_integrity_failure`, mandatory destination-echo declaration for non-Synthetic payout adapters, B13-A launch flags (M-3 owner/architect). New open questions: Q-R21-1 (RR-1 at the bound destination_mismatch site), security LOW-1 (net recovery in M2 (d) and RR-1). Closed by ledger-finance review: Q-R21-2 (keep conservative). ALERT-DELIVERY-1 and B14-B18 unchanged (blocked). |
| 2026-10-09 | **DECIDED (owner, instruction `CONTINUOUS-GOVERNANCE-EXECUTION-CYCLE-2026-10-09`; recorded in ADR 0095 s48):** (1) Q-R21-1: RR-1 also applies to a recovered `destination_mismatch` park; a mismatch is not recovery; the standing alert clears only on the complete recovery predicate; history preserved. (2) RR-1 uses NET recovery (correct causation, wallet, asset, amount, payout relationship; a debit alone is not sufficient). (3) Instrument state MUST NOT gate settlement of an already authorized/bound/snapshotted payout; eligibility still governs initiation/dispatch. (4) `destination_integrity_failure`: no automatic exit; park/hold, investigate, positively reconcile authoritative destination evidence, controlled four-eyes resolution, no unilateral override, no provider callback changing the destination, no automatic release/settlement/reassignment; outcomes may include resume when the authoritative destination is positively established, controlled cancellation/release where policy permits, or remain parked. (5) Every non-Synthetic payout adapter MUST explicitly declare its destination-echo capability; callbacks remain evidence only. (6) D-7: the M4 evidence standard is approved (withdrawal, attempt, provider reference, player/tenant, asset, amount, destination where applicable, final status, causal relationship; no single untrusted field sufficient; ambiguity parks). (8) No raw player credential registration UI; PSP tokenization flow only (architecture later). (9) B14-B18 NOT authorized. (10) AWS NOT authorized. **NOT DECIDED (exact wording requested):** Q-HSEC-1, Q-HSEC-2, Q-HSEC-3, O-1, O-2, M-3, HD-R15-5 (see `docs/governance/open-owner-questions-2026-10-09.md`). Supersedes the OPEN status of Q-R21-1, security LOW-1, the instrument-state brief (Brief 1), the `destination_integrity_failure` exit brief (Brief 2), the echo declaration brief (Brief 3) and D-7. |
| 2026-10-10 | No new human decision. New OPEN questions recorded: GAP-AAM (governed exit for `amount_asset_mismatch` parks: options a/b/c), D-OPS-1/D-OPS-2 (operator console for the governed flows), the sandbox PSP authorization ask (`sandbox-psp-authorization-package-2026-10-10.md`). Blocking matrix for every open question added to `open-owner-questions-2026-10-09.md`. |
| 2026-10-10 | No human decision. Ballot (`decision-ballot-2026-10-10.md`) and sandbox authorisation request A-Q (`sandbox-psp-authorization-request-2026-10-10.md`) prepared; nothing marked approved. Sandbox blockers: HD-R15-1, HD-R15-5, D-REG-1 (payout scope), `Unsupported` ack (conditional); others NON-MOCK/PRODUCTION. |

