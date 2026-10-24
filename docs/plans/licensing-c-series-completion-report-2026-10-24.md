# Licensing / jurisdiction C-series completion report (2026-10-24)

Branch `claude/licensing-jurisdiction-completion` (pushed, no PR, NOT merged). Frozen baseline `1505a165e9152729b5df573211c80b1c0a35bc87` is an ancestor and untouched. Migration head **0190** (baseline head of this line was 0175). All providers MOCK. Nothing here is a legal opinion, a licence or jurisdiction approval, production readiness, real-provider validation or an AWS RPO/RTO claim.

## 1. Commits
- **Tested code commit: `cf7c9db`.** Verified tree for the local gates: `0dbd06c` = `cf7c9db` + a documentation-only commit (registry and decision-register text; `git diff cf7c9db 0dbd06c` touches only `docs/governance/*`). No code, migration or test differs, so the code evidence for `cf7c9db` is the code evidence for `0dbd06c`. This report and its registry rows are a further docs-only commit.
- Go toolchain 1.26.10 (`go.mod`); hosted `govulncheck` green. The advisory itself was not independently identified (the vulnerability database is blocked from this environment); the green hosted run is the only evidence.

## 2. Verification (all exit statuses observed)
| Gate | Command / source | Result |
|---|---|---|
| Hosted CI, code commit | run 39944443184 on `cf7c9db` | 27/27 jobs success (build-test-lint incl. golangci-lint + govulncheck, 16 integration shards, frontend, infrastructure, migration reversibility 4 steps). Earlier runs 39938092669 (`a938ae7`), 39940192343 (`5a7b039`), 39941969192 (`42ad83f`) failed ONLY at golangci-lint (SA6005, errcheck, QF1001 x2, C1 test files); fixed. |
| Hosted CI, docs head | run 40083628534 on `0dbd06c` | 27/27 success |
| Controlled DB recovery | owner-authorised 2026-10-24 | see s5 |
| Migrations | private DBs, `cmd/migrate` | clean-DB setup + `verify` OK; down 15 (0190 -> 0175) then up then `verify` OK, exit 0; separate DB built with only migrations <= 0175 then `up` with the final tree applied 176..190, `verify` OK, no version gaps |
| Full integration suite | `priv_test.sh -tags integration -count=1 -p 1 -timeout 8h ./...` on `0dbd06c`, 2026-10-24 12:47-14:52 UTC | **exit 0, 82 packages ok, 0 FAIL**, 27 packages without test files; httpserver 993 s, payments 4619 s, aml 225 s, licensingchange 89 s. Per-test skips are not itemised (run without `-v`). |
| Real-stack Playwright | `backoffice/e2e/run.sh` (fresh private DB, real `cmd/platform-api`, built Back Office, Chromium, 1 worker), specs 01-13, `0dbd06c`, 14:52-15:41 UTC | **exit 0, 80 passed, 0 failed, 0 skipped/fixme, 0 flaky**, 47.9 min. Orchestrator-run; the earlier QA-agent runs are not substituted. |
| Unit, no tag | `go test -count=1 ./...` on `cf7c9db` and `0dbd06c` | 79 packages ok, 0 fail, exit 0 |
| Format / vet / build | gofmt, `go vet ./...`, `go build ./...` | clean |
| Frontend | backoffice `tsc` + vitest | 82 files, 1080 tests pass; b2c `tsc` + vitest 27 files, 190 tests pass |
| Local golangci-lint | not runnable (binary built with older Go) | hosted CI is the lint evidence |

Exclusions / not run: browsers other than Chromium; any real provider; AWS; hosted CI does not run Playwright (the local run above is the only final-tree E2E evidence).

## 3. Independent review evidence (no implementer was sole reviewer)
- **Security (independent agent), three passes** over C1/C2/C4 and the C3 test helpers, each re-running suites and writing its own exploit probes against the real DB. Round 1: C4 HIGH-1 (loosening bypassed the AML officer controls; three plain platform_admins could loosen), MEDIUM-1 (no window between decision and effect), MEDIUM-2 (runtime-executable break-glass setter, pre-existing), LOW-1 (capability lock not shared), LOW-2 (table guard), LOW-3 (500 on executor trigger refusals). Round 2/3: HIGH-1, MEDIUM-1, LOW-1, LOW-2, LOW-3 verified CLOSED against the real DB; new LOW-A (own-licence tenants cannot loosen: fails closed; now an explicit closed code), LOW-B (collusion, below). Final verdict at `a938ae7`: C1, C2, C3 (tests), C4 acceptable; no Critical/High/Medium open in code. MEDIUM-2 stays open as D7.
- **Ledger-finance (independent agent), two passes** on migration 0190: H-1 (sanctions list removal would let a pep-only clear release payouts; fixed: removal refused, safe default), M-1 (loosening applies retroactively to waiting withdrawals; mitigated for grace, NOT for interval = D10), M-2 (mixed loosen/tighten; fixed), L-1/L-2 (audit and reconciliation; fixed), N-1 (grace guard at execution not effective time; fixed), N-2 (reconciliation instructions; replaced by a read-only SECURITY DEFINER function). Verdict: approved as the INERT mechanism; NOT approved for enabling for any tenant. 0190 touches no ledger, balance or withdrawal state and cannot enable the gate.
- Caveat: these are code- and design-level reviews on a development database, not penetration tests or regulatory sign-off. Review reports exist as session transcripts; their findings and dispositions are recorded in `c1-precheck-officer-record.md`, `c2-tenant-basis-read-record.md`, `c3-test-gaps-record.md`, `c4-screening-loosening-record.md` and the registry.

## 4. Acceptance items
| Item | Implementation | Tests | Independent review |
|---|---|---|---|
| Operating bases (own licence / partner licence / documented no-licence) | IMPLEMENTED (0180/0183) | integration + E2E 06 | security + ledger-finance (earlier slices) |
| Governed licensing / market-configuration workflows | IMPLEMENTED (0177-0179, Back Office filing) | integration + E2E 03-05, 11, 13 | security |
| AML trigger wiring (MOCK), residence maker-checker, governed KYC policy | IMPLEMENTED (0181, 0185, 0186, 0188, 0189) | integration + E2E 07, 08, 10, 12 | security + ledger-finance |
| Closed precheck refusal codes (C1) | IMPLEMENTED, 31 codes, drift-pinned Go/OpenAPI/UI | wire tests + E2E 13 (i1-i3) | security: acceptable |
| `platform_officer_required` incl. authorised/unauthorised paths (C1) | IMPLEMENTED; reachable via tenant compliance + tenant-scope officer grant (corrects lic32 finding 5) | actor matrix + E2E 13 (i4) | security: acceptable |
| Tenant-scoped operating-bases read API (C2) | IMPLEMENTED (API); Back Office tenant view NOT IMPLEMENTED (no tenant console section) | isolation, oracle, RLS, field-set pin | security: acceptable |
| Ceiling withdrawal, brand/product rungs, 24 h/72 h, audit, concurrency, stale UI (C3) | tests only | Go + E2E 13 | security reviewed helpers |
| Screening-policy four-eyes governance (C4) | IMPLEMENTED as an INERT mechanism (0190); NOT ENABLED | integration, mutations, E2E 12 | security x3, ledger-finance x2 |
| Withdrawal-restriction safeguards | IMPLEMENTED (0182/0184), values unconfigured (default allow, labelled) | integration | ledger-finance; 0190 does not touch it |
| Back Office against real API/DB | IMPLEMENTED | E2E 01-13 | n/a |

## 5. Controlled test-DB recovery (what changed, where)
The container was replaced on 2026-10-24; its local PostgreSQL cluster contained only the `postgres` superuser and no databases. Under the owner's authorisation (DB-ACCESS-FRESH-CONTAINER-1, restrictions 1-6) the three designated test-only roles `igaming`, `igaming_runtime`, `igaming_test_admin` and the database `igaming_platform_ci_local` (owner `igaming`) were CREATED in the local in-container cluster in one invocation, from the repository's own CI definitions (`.github/actions/ci-db/action.yml`: NOSUPERUSER, no CREATEROLE, no BYPASSRLS; test_admin has CREATEDB and membership of `igaming`). Credential values were read from that file and never printed or committed. Verified: each role connects as itself (`select current_user`). Because the roles did not exist, this was creation, not password reset. No production credential, application code, authentication or security check was changed. Scratch databases (mig_clean, mig_upgrade, orch_final, e2e_final) were dropped; only the base database remains.

## 6. Outstanding security / governance risks (must be read before approving)
- **Collusion (LOW-B / D5):** officer grants need three distinct Persons, but three colluding platform_admins can grant each other `aml_officer` for a tenant and then, if the capability were ever enabled, loosen. Same trust model as every other AML officer control; mitigated by audit trail and by the mechanism being inert. Open owner decision (approver population).
- **D4 (LAUNCH BLOCKER):** loosening the screening TRIGGER policy (fewer events, gate off) is still a single-officer critical route, not two Persons.
- **D7 (LAUNCH BLOCKER):** the unrouted break-glass `aml_screening_policy_set` is still executable by `igaming_runtime` (confirmed on a clean migrated database); one officer, no tighten-only rule, no effective_from bound, and it can still remove the sanctions list. Recommendation: revoke runtime EXECUTE after moving the e2e fixture tool. Not done: it needs an owner decision and a new migration plus re-verification.
- **D10:** a longer re-screen interval makes old clears fresh again and can release currently held withdrawals at their next gate claim; not prevented on the governed path (grace extension after expiry IS refused). The gate also does not check which lists a clear was screened against.
- **Inert:** `aml_screening_loosening_settings` has 0 rows on a clean database, no screening policy or gate-enabling trigger policy ships, own-licence tenants are refused. The screening mechanism must stay disabled until D1-D10 are approved.
- Other: C1 trigger-refusal 500->409 mapping proved at unit level only; substring-based mapping (follow-up); jurisdiction creation single-actor (JURIS-CREATE-GOV-1, policy unchanged); product findings F1-F4.

## 7. Owner decisions vs legal determinations vs engineering
- **Owner / security decisions:** C4-LOOSEN-1 (D1-D10), JURIS-CREATE-GOV-1, C-SERIES-PRODUCT-1, merge approval of this branch.
- **Legal / compliance determinations (not decided):** per-withdrawal release for hold_for_review, withdrawal-policy values, KYC thresholds, residence mode, sanctions false-positive policy, RG floors, country list (business configuration later; not a blocker).
- **Engineering follow-ups, not blocking this merge:** revoke runtime EXECUTE on the break-glass setter (after D7 decision), environment-anchor pre-check in the C3 guard-disabling helpers, dedicated SQLSTATEs for trigger refusals, officer check before capability checks, trigger-policy 2x alert invariant, Back Office loosening filing form, tenant operating-bases view, closed code for unassigned country.
- **Genuine blockers:** none for this stage.

## 8. Recommendation
**READY FOR MY MERGE APPROVAL** for the integration line as a provider-independent stage with an inert C4 mechanism, on the evidence above: hosted CI 27/27 on the code commit and on the docs head, full local integration exit 0 (82 ok), real-stack E2E 80/80 exit 0, migration up/down/upgrade verified, independent security and ledger-finance reviews with all material code findings closed. This is conditional on the owner accepting the open launch blockers D4 and D7 as pre-launch (not pre-merge) items and on the screening mechanism staying disabled. Nothing has been merged. Next provider-independent item (not started): the follow-ups list in s7, beginning with the D7 setter revocation once decided.
