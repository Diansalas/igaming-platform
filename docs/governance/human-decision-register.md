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
| ADR-0094-500MS | Keep the 500 ms timing bound | KEPT unchanged | DECIDED | - | owner/security |
| HQ-E1-2 | E1 item (p2) | Confirmed | DECIDED | - | owner |
| CLEANUP-2026-10-05 | Scratch DB/worktree cleanup | Accepted; 44 UNKNOWN DBs preserved | DECIDED | - | owner |
| TRIGGER-SEARCH-PATH-1 route | How to close the TEMP shadow path | REVOKE TEMP (0116, ADR 0108) | DECIDED; implemented | Deployment verification per environment | owner + security |
| HD-0095-1 / LEDGER-MANUAL-ADJ-4EYES-1 | Force-resolve authority; manual adjustments | Recorded in ADR 0098 (2026-09-28) | DECIDED | - | owner |
| HD-PRH2-9, HD-PRH2-10 | Closed-tenant funds, E1 alert kind | Recorded in ADR 0105 (2026-10-04) | DECIDED | - | owner |

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
| B13 | Payout destination binding | none | BLOCKED | Real payouts | architect + owner |
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
