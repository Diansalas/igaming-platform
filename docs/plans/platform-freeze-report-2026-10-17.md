# Platform-freeze report (2026-10-17)

Scope: the provider-independent platform (owner priority change 2026-10-16). Supersedes `final-production-readiness-report-2026-10-16.md` where they differ.
Nothing here is a legal, regulatory or licensing approval or a launch authorisation. All providers are MOCK. **No AWS resource exists or was touched. No brand is activated.** The AWS staging deployment is NOT started (owner instruction: only after this freeze report).

## 1. Freeze commit
Branch `claude/focused-wright-jw88w9`. The freeze commit is the commit named in the registry row `PLATFORM-FREEZE-2026-10-17` (the last commit of the branch at the time of this report; anything after it is documentation). Code under test: `75c548c` plus the U-1 config change `528e636` (hosted CI re-checked on the freeze commit, see §2). Migration head **0170** (reserved, never used: 0143–0150, 0159, 0161; pinned by `TestReservedUnusedVersions_ExactSet`).

## 2. Evidence (all local synthetic data / MOCK providers)
| Check | Result |
|---|---|
| gofmt, `go vet`, build | clean |
| Unit `go test ./...` | 75 packages ok, 0 FAIL (also with the CI JWT env) |
| Integration, all packages except httpserver/payments (`-tags integration -p 1`, private DB) | 74 ok, 0 FAIL |
| `internal/httpserver` integration | ok |
| `internal/payments` integration, full, end to end (4565 s) | ok |
| Hosted CI | 27/27 on `75c548c` (build-test-lint, 16 `-race` integration shards, timing lane, evidence gate, frontends, images, infrastructure); freeze-commit run recorded in the registry |
| Frontends | backoffice build + 644 tests; b2c build + 140 tests |
| `migrate verify` | up from zero clean; 161 downs to zero and re-up clean at 0170 |
| Backup/restore/PITR drill at 0170 (`deploy/backup/drill.sh`, throwaway clusters) | **138 PASS / 0 FAIL**. A first run showed 2 FAIL in the SIGTERM/SIGINT cleanup checks; cause: the check counts stray `sleep 120` processes and the orchestrator's own wait loops matched. Re-run with none: 0 FAIL |
| `go test ./deploy/... ./cmd/...` | ok |
| Independent final audit (`final-platform-audit-record.md`) | no CRITICAL/HIGH; live-DB checks were skipped by the auditor (RLS verified statically; RLS is covered by the integration suites above) |

Defects found by this final verification and fixed: backoffice reconciliation kind vocabulary missing `pay_payout_reversal_tombstone_but_paid` (UI/OpenAPI contract), the 0101 guard test fixture (now seeds through the F4 allow), U-1 published JWT secrets (now refused outside explicit development). RPO 0 and RTO < 15 min are **not demonstrated** (local machine).

## 3. What is finished (provider-independent A-class)
A1–A13 and F1–F6: actor-proof key isolation (0166), payment statement integrity: sealed cursor and run seal (0167/0168), payout-reversal tombstone finding, KYC-residence market evidence + ops (0170), platform status route, withdrawal gate at dispatch with DB backstop (0169, runbook `withdrawal-dispatch-gate.md`), proof-bound payout-reversal approvals (0165, T8). Each merged with independent security and/or ledger-finance review. No A-class implementation remains.

## 4. Not proven locally / AWS-gated
IAM/runtime roles and search_path on the real account, RDS behaviour (logical restore on RDS may be BLOCKED), managed PITR and measured RPO/RTO, Secrets Manager, ECS deployment, alert delivery to a real recipient, dedicated task role (SRSP-3), backup runner placement, KMS signing. Plan and IaC are prepared and kept ready: `docs/runbooks/aws-staging-deploy-plan.md` (terraform plan was 97 add/0 change/0 destroy, nothing applied). Required human IAM actions are listed there and are asked for only at the AWS gate.

## 5. Provider-dependent (documents prepared in `docs/provider-integration/`)
Order PSP (Nuvei is a candidate only; its docs were not reachable: UNVERIFIED) → casino → KYC/AML → sportsbook → sports data. Gaps G-01..G-11, CAS-G*, KYC-G*, SB-G*, SD-G*; QueryStatus/destination-echo conformance; real statement sources; AML vendor; email/pager.

## 6. Open items that travel with the freeze (registered)
- **Before real money:** SF4-1/R1 forged allow row by a runtime credential (choose: signer outside runtime reach per ADR 0110, or accept plus a scheduled re-evaluation job that alerts on divergence); F4-R2-NEGBAL-SQL; SRF2R-3 (`payment_attempt_open_exposure` trusts unsealed deposit_reversal lines; before the first real PSP); SRF2R-1/2, SRF2-6; SF4-3 (escalation not sticky); SF4-5 silent fail-closed at dispatch; physical backups contain the actor-proof key (owner/superuser T7).
- **Owner/legal decisions (none blocks the freeze):** SRME-4 residence vs physical location, SRME-5/MKT-WIRE-CREDIT-1, SRME-6, maker-checker for residence determinations, alert severity/volume, HDR-A6-3, RG floor values from compliance, legal wording of the AML confidentiality duty.
- **Lower:** U-5 frontend money floats (deferred with crypto/retail), U-6, U-7, `TODO(jurisdiction)` filtering (before a second jurisdiction), incident rota, load/soak at realistic volume.
- **Deferred by owner:** crypto, retail, sportsbook until scheduled, NR-4/S56-R14, `evidence_ref_hash`.

## 7. Four states
Technical platform readiness: MET for the MOCK-provider scope at the freeze commit, **subject to AWS-specific validation**. Provider-integration readiness: PARTIALLY IMPLEMENTED. Real provider integration: NONE. Brand activation: NONE.

## 8. Next (not started — awaiting the owner's go)
Single AWS staging deployment against the freeze commit → fix AWS-specific issues → re-freeze → final technical readiness → provider integration (PSP first). Minimum decisions at that gate (recommendations): (1) authorise the one staging deployment on the freeze commit — yes; (2) IAM actions by a human admin (boundary apply, runner-roles policy, PITR restore credential) — as listed in the plan; (3) allow `docs.nuvei.com` in the network policy so the PSP evaluation can be verified — yes; (4) SF4-1 choice before real money — accept R1 plus a scheduled re-evaluation job.
