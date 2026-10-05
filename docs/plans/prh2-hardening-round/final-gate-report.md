# PRH-2 final gate report (2026-10-05) - STOPPED, awaiting owner authorization

Tree: `119abf1`-descendant on `claude/focused-wright-jw88w9`. Nothing below is GitHub CI evidence (CI-BILLING-1 is open). No provider is real, AWS is OFF, no cleanup was performed.

## 1. Workstreams merged (all IMPLEMENTED against MOCK providers, local evidence)
D1 `7adb0c5`, D2 `271c412`, F-pay `2dc8d10`, H `7750846`, I-wire `dcaa2c6`, E1 `d34f088` (+`8e78af7`), K3 `786275b` (+`8eaa959`). Migrations 0108-0115 contiguous (115 up/down pairs). Each merged workstream has independent security, ledger-finance (where money), code-review and QA records under `docs/plans/prh2-hardening-round/reviews/`, plus delta re-reviews after fixes. No unreviewed code was merged; no force-push or history rewrite.

## 2. LOCAL EVIDENCE (this container: 4 vCPU, Go 1.26.8, PostgreSQL 16, private DBs)
- Final sweep on `01e093c`: `go build`, `go vet` (both tag sets), gofmt, golangci-lint 2.9.0 (0 issues), `migrate verify` 0001..0115 no gaps, whole-repo `-race -tags integration -count=1 -p 2 -timeout 60m -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./...`: 48 packages ok, 0 FAIL, exit 0 (log not committed; 05:13-05:41Z). The skip list is the CI timing-lane split, see section 3.
- Two cross-package defects were found by merge-time whole-repo runs and fixed (they are not hidden): providercred staff_users policy allowlist (`8e78af7`), E1 worker-fence seed leaving a live platform deposit risk rule that broke `internal/risk` (`8eaa959`). The branch-local agent runs had not exercised those packages.
- Mutation evidence: `docs/plans/payment-readiness/evidence/prh2-e1-mutation-kill.txt` (87 entries, all killed; QA re-applied 12), `prh2-k3-mutation-kill.txt` (106 + fix-batch mutants; 14 original survivors classified equivalent/redundant; QA re-applied 17 with 3 new gap mutants, killed by Y12; LF 9; security 6+32). Other workstream evidence files in the same folder.
- Migration verification: up/down/up round trip for 0114 and 0115, `pg_dump -s` identical (QA).
- Tenant/RLS, authorization, audit, financial invariants: covered by the suites above (FORCE RLS on every new table, no NULL-tenant arm on new tables, tests assert NOT rolsuper AND NOT rolbypassrls, four-eyes DB-enforced, append-only ledger, idempotent reserved provider-tx namespace, audit rows in-transaction).

## 3. Timing lane (idle environment): NOT GREEN - not claimed
`docs/plans/payment-readiness/evidence/prh2-final-timing-lane.md` (+ two raw logs). Exact CI commands, `-race -count=1`, thresholds unchanged, 5 repetitions (4 CPU, stale and fresh DB) and 3 more at `taskset -c 0-1`.
- `TestStoreOutage_DoesNotPinPool` and `_ProductionPoolSize`: 10/10 PASS. `TestResolutionIsolation_FinancialDuringOutage`: 13/13 PASS idle (it passed when idle, failed under the earlier heavy run - as you recorded). OneTenantStoreOutage, MultipleTenantsOutage, SimultaneousOnset_Bounded, ConnectionExhaustion: all PASS in every run.
- `TestResolutionIsolation_NormalOperation`: 0/13 PASS (500 ms per-callback bound exceeded on deposits, 501-555 ms). It also fails on `ecd2b74` (0/3 pass) and passes 1/3 on the pre-PRH-2 baseline `94b4ae5`, whereas the same lane passed 40/40 on 2026-09-28. So this is not shown to be an E1/K3 regression, but it is a gate failure in this environment and is unresolved. Decision needed (characterize the environment or review the ADR 0094 bound). The bound must not be loosened silently. TEST-RESISO-RACE-1 stays OPEN.

## 4. GITHUB CI EVIDENCE
None for any PRH-2 work. CI-BILLING-1 blocks it; billing was not touched. Last green CI on record predates PRH-2.

## 5. Alert architecture
Durable alert records, dispatcher running in `cmd/platform-api`, log sink only; every alert is unrouted; dedicated KYC alert kind `kyc.submission_failed_terminal` (p2 default, severity pending HQ-E1-2). ALERT-DELIVERY-1 OPEN: no real channel, no configured recipient, no human delivery claimed, no recipients invented.

## 6. Open items (see task-registry.md and docs/HANDOVER.md sections 'Launch blockers (current)' and 'Human decisions awaiting the owner')
Highest: TRIGGER-SEARCH-PATH-1 raised to HIGH, launch-blocking (K3 security delta reproduced a TEMP-table shadow bypass of the K2 two-person adjustment check; pre-existing 0112/0113; fix needs a new migration pinning/qualifying 0026..0113 functions or REVOKE TEMP, a role change that needs your decision). PAY-K3-STATEMENT-SOURCE-WIRING-1 and PAY-RECON-PARKED-CAPTURE-STANDING-1 are hard prerequisites before any real PSP. All follow-ups you listed are preserved open (PAY-DEPOSIT-ESCALATION-1, PAY-POLL-ECHO-HARDENING-1, PAY-POLL-DECLINED-ALERT-RECON-1, PAY-RECEIPT-T4-DRAIN-TEST-1, PAY-PAYOUT-UNBOUND-HOLD-1, MA020-SYNC-MISMATCH-1, PAY-PAYOUT-REFBIND-1, PAY-FPAY-HARDENING-1, PAY-H-FOLLOWUPS-1); PAY-RECON-POLL-REF-CLEAR-1 is delivered in part by K3 but not closed (LF confirmation pending).
Closed-tenant funds (HD-PRH2-9): ADR 0107 is DESIGN ONLY, NOT IMPLEMENTED; HD-CTF-1..9 and HQ-E1-1..4 are open human decisions; the mechanism stays disabled.

## 7. Handover completeness audit
Performed (`handover-audit` by the architect specialist, read-only) and its patch list applied in `119abf1`: status notes, launch-blocker and human-decision sections, repo-structure/risk/tenant/RG/provider rows, secrets table (names only), runbook index, registry hygiene row (nothing closed without evidence). Items marked 'to confirm' remain (PAYWH-RL-1, F-POOL-2 re-status, CP-W1 naming, Wave 4 status).

## 8. Cleanup
Inventory only (`cleanup-inventory-2026-10-05.md`): 666 scratch databases (about 11 GB), 42 worktrees (none locked or dirty now, one with unmerged commits), nothing deleted.

## 9. Stop
Per the stage-gate rule the next step needs explicit authorization. Not started: real-provider integration, TRIGGER-SEARCH-PATH-1 fix, ADR 0107 implementation, ALERT-DELIVERY-1 channels, any cleanup.
