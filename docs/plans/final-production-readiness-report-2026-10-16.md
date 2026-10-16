# Final production-readiness report — core platform completion (2026-10-16)

Scope: the completion audit's A-list (A1–A13), executed under the owner's 2026-10-15 decisions. **Core platform development stops here.**
Nothing in this report is a legal, regulatory or licensing approval. All providers are MOCK. No AWS resource exists or was touched. No brand is activated.

## 1. Head and evidence

| Item | Value |
|---|---|
| Branch / code head | `claude/focused-wright-jw88w9`, code under test `0dc2680` (later commits are documentation only) |
| Migration head | 0164 (reserved, never used: 0143–0150, 0159, 0161) |
| Hosted CI | run on `0dc2680`: **27/27 checks success** (build-test-lint, 16 sharded integration jobs with `-race`, timing lane, integration-evidence gate, frontends, images, infrastructure). Earlier `51e9218` failed one shard (a stale A5 test assumed 0163 was the newest migration) — fixed in `0dc2680` |
| Unit | `go test ./...` 68 packages ok, 0 FAIL; gofmt, `go vet` (plain + `-tags integration`) clean |
| Local integration (fresh private DB, `-p 1`) | all packages except httpserver/payments: 68 ok; `internal/httpserver`: ok; `internal/payments`: ok (3405 s). 0 FAIL |
| Frontends | backoffice build + 56 files / 643 tests PASS; b2c build + 24 files / 140 tests PASS |
| `migrate verify` | clean on a fresh DB (153 migrations at the time of the check, all downs to zero and re-up clean); reserved set pinned by test |
| Backup / restore / PITR drill (REAL, local, synthetic data) | `deploy/backup/drill.sh` at migration head 164: **118 PASS / 0 FAIL** (14m37s). Logical restore ≈ 5 s, PITR ≈ 1 s on this dataset (112,934 ledger entries). RPO 0 and the 15-minute RTO are **not demonstrated** (local machine, not production) |
| Race | hosted CI shards run `-race`; local `-race` on A5/A6 packages recorded in their records |
| Evidence label | **MOCK VERIFIED** for every provider-facing path; nothing is "real provider verified" |

Caveats: local integration was run once at tip `51e9218` + the one-test fix; hosted CI on `0dc2680` is the authoritative whole-tree run. golangci-lint with `-tags integration` reports ~640–680 older errcheck/staticcheck findings in untouched test files (baseline, not introduced by A-class work; plain lint is 0 issues).

## 2. A-list status

| Item | Status | Merged evidence |
|---|---|---|
| A1 hosted CI green | DONE | runs #828, #833, `0dc2680` |
| A2 docs truth + bonus sweeps deviation | DONE | ADR 0121 |
| A3 casino pre-adapter fixes | DONE | registry A3 (migration 0152) |
| A4 MANUAL-ADJ-LINK-1 DB fence | DONE | migration 0153 |
| A5 governed payout reversal + amount dispute + negative balance block | DONE (MOCK) | migrations 0162, 0163; reviewed twice |
| A6 RG enforcement (time-out, deposit/loss/wager limits, withdrawal gate, B2C page) | DONE — **enforcement OFF by default per brand** | migration 0158; reviewed 4 rounds |
| A7 AML operability | DONE (MOCK screener) | migrations 0154, 0155 |
| A8 operating-market blocking | DONE (mock locator; production refuses without an eligible evidence source) | `internal/marketgate` |
| A9 observability + alert delivery | DONE (delivery verified MOCK only) | migrations 0156, 0157 |
| A10 operator consoles, staff invite, contract tests, console backend | DONE | migration 0164 |
| A11 payments statement-source wiring | DONE (MOCK) | migration 0160 |
| A12 backup/restore/PITR drill | DONE locally; RDS/AWS **unverified** | ADR 0122 |
| A13 hygiene, `migrate verify`, runbooks, status docs, this report | DONE | scratch-DB reaper, 7 new runbooks |

## 3. Four states (kept separate)

1. **Technical readiness** — the finish line items (A1–A13, hosted CI green, final local verification, `migrate verify`, backup/restore verified locally, runbooks, docs match code, mock interfaces + conformance tests, no open A-class rows, decisions recorded) are met **for the MOCK-provider scope**. Not yet exercised on real AWS infrastructure (see §5).
2. **Provider-integration readiness** — PARTIALLY IMPLEMENTED: provider interfaces, mocks and conformance tests exist; every real-provider dependency is listed in §4.
3. **Real provider integration** — NONE.
4. **Brand activation** — NONE (manual, audited, four-eyes, per brand; legal/licensing is a per-brand decision, not a universal technical blocker).

## 4. Open rows by class (none is an A-class row)

**Before the first real PSP / real money (security/ledger-finance, owner authorisation where noted)**
- R1 / SR5-2: payout-reversal `reverse` decisions not bound to the ADR 0110 signed actor proof — **needs owner authorisation (ADR 0110 T8 amendment)**; proposal in `docs/plans/a5-payout-reversal-record.md` §11.3. SR5R-1, R9 (tombstone follow-ups).
- SR11-1 (forged payment-statement cursor, HIGH; next step SR11-10 sealed backing import), SR11-11, LF-A11-1/2/6, H-W1 second half.
- WD050/WD051 have no database backstop and payout dispatch does not re-run the withdrawal gate (SR6 I-2).
- Real statement sources + read-scoped credentials, QueryStatus amount/asset conformance, destination-echo conformance (PROVIDER DEPENDENT).
- Real email adapter / alert recipients / pager; metrics backend and dashboards (ALERT-DELIVERY-1; R-ALERTS passes only with enabled human routes, and does not prove anyone is paged).
- Geolocation / market evidence source (D-MKT-EVIDENCE): production currently **refuses** registration/deposit/launch without one.
- AML screening vendor; KYC vendor; legal wording of the AML confidentiality duty (E).

**Before switching RG enforcement on for any brand (owner + compliance)** — HDR-A6-1..4: the four interpretations, 7-day wager/loss windows or the rollup (LF-A6-2), acceptance that a stuck deposit intent can block deposits up to 31 days (LF-A6-4), jurisdiction-source question (licence vs market under hybrid licensing). Session-duration/reality-check enforcement is NOT IMPLEMENTED (provider-dependent/F).

**Before the AWS staging run (owner actions)** — see `docs/runbooks/aws-staging-deploy-plan.md`: authorisation record, AWS account/profile and access CIDRs, `mock_geolocation_country`, `actor_proof_keys` row (owner action B), operating-market rows (owner action C), alert webhook URL and two-admin enablement of P1/P2 routes, backup runner (G1, not built), G2 exclusion, G4 scrape path, SRSP-3/9/10/11.

**Before production** — dedicated platform-api task role (SRSP-3); IAM delta (A12) simulated against AWS; RDS behaviour of the backup/restore tooling (IAM DB auth, non-`rds_superuser` owner for the restore load — the restore load step may be BLOCKED on RDS); KMS asymmetric signing and per-environment signing keys (ADR 0122 §11); ADR 0122 §10 (exclude `actor_proof_keys.secret` from logical dumps); load/soak test against realistic volume; incident rota (NOT IMPLEMENTED); heavy-player rollup (if monthly windows are required).

**Class B / F (deferred by owner decision)** — sportsbook until a sportsbook brand is scheduled; crypto; retail; B2C payout-instrument registration (until PCI/PII review); ADR 0119 S8 lifecycle UI; NR-4/S56-R14 under the accepted R1 threat model; `evidence_ref_hash` (owner decision).

## 5. The one named-commit AWS staging deployment (owner decision 8)

Prepared, **not authorised, not executed.** Runbook: `docs/runbooks/aws-staging-deploy-plan.md` (authorisation record, preconditions B1–B8, go/no-go, phases, stop conditions, evidence table, teardown). Terraform fmt/validate/tftest pass offline and in hosted CI; real AWS behaviour is unverified. Preconditions status: B1 SHA pinned — pending your authorisation; B2 hosted CI green on that SHA — met for `0dc2680`; B3 final local verification — recorded here; B4 local drill at current head — met (head 164, 118/0); B5 security review of the infrastructure delta — done for STAGING-PREP-1/2; B6/B7 static Terraform checks — met; B8 owner inputs — pending.

## 6. Provider-integration phase (next, not started)

Per provider: documentation/credentials → adapter → mapping → conformance suite → security review → ledger-finance review → sandbox → final approval. First candidates by owner priority: casino (first-launch priority), one fiat PSP. Prerequisites from §4 apply per provider.

## 7. Decisions required (minimum, with recommended answers)

1. **Authorise the ONE staging deployment** on the final docs commit and supply the §5 owner inputs. *Recommended: yes; supply AWS account/profile, region eu-central-1, access CIDRs, alert recipient (diego.salas806@gmail.com), and `mock_geolocation_country`.*
2. **Authorise the ADR 0110 T8 amendment** (proof-bind payout-reversal decisions). *Recommended: yes, before any real PSP work.*
3. **Market evidence source (D-MKT-EVIDENCE).** *Recommended: KYC-verified residence now, vendor geolocation later.*
4. **RG:** confirm HDR-A6-1 interpretations; first brand wager/loss windows ≤ 7 days (no rollup); accept the stuck-intent block or fund a staff resolve-to-failed slice; compliance supplies floor values (OQ-2). *Recommended: confirm as built; accept per-wallet negative-balance block and that a deposit lifts it.*
5. **ADR 0122 §10:** exclude `actor_proof_keys.secret` from logical dumps and re-key on restore. *Recommended: exclude.*
6. **`evidence_ref_hash`:** keep the operator-supplied digest until a keyed-HMAC design is chosen. *Recommended: keep.*
7. **Which provider first** (casino aggregator and fiat PSP selection, contracts) — owner/commercial.

## 8. Process disclosures

`pkill` was used three times against own helper processes earlier in the programme (disclosed in the registry); no other process-hygiene violations. Several reviewers could not authenticate to the local DB and ran no integration tests; in those cases the author's private-database evidence stood and is labelled so in the registry rows.
