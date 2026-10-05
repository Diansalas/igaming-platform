# Cleanup plan (worktrees + scratch databases)

**NOTHING WAS DELETED OR MODIFIED.** Everything below is read-only inventory (git log/status/branch, psql SELECTs as igaming_test_admin, /proc and stat reads). The commands in section 4 are TEXT for later and must run only after the final technical gate and explicit owner authorization. Inventory taken 2026-10-05 ~07:10 UTC.

Environment facts: Postgres 16 was (re)started 2026-10-05 07:04:56 UTC (it answered "database system is starting up" at first; all DBs survived). Disk: / is 252G, 86% used, 5.6G free. Scratch DB total is 11 GB (667 non-template DBs). Worktrees under .claude/worktrees total 1174 MB (39 dirs); .claude/worktrees is gitignored.

## 1. The unmerged worktree: agent-aae2d0af6d7fc8c3d (branch worktree-agent-aae2d0af6d7fc8c3d, tip 1564c34)

Workstream: PAY-DOUBLE-CREDIT-1 / FH-3 (ADR 0095 §28, INV-DEP-1, migration 0107), the `qa` specialist's worktree (INV-DEP-1 A-O test-first matrix, then the FH-3 adjudication, plus a kill-switch phase 2 QA verification). Registry: task-registry.md PAY-DOUBLE-CREDIT-1 row names "`qa` A-O matrix (`qa-fh3-adjudication.md`)". The FH-3 implementation worktree (agent-adce273a..., branch worktree-agent-adce273a3a5339f77) is merged into main (a72128d).

Merge base with main: 0d00f57 (2026-09-27 "docs(registry): migration allocation 0106/0107"). Worktree is clean, not locked, no process attached.

Commits (git log main..branch, 5 ahead; all 2026-09-27; author "Claude"; trailers Co-Authored-By: Claude Sonnet 5 + Claude-Session 01GuNiWT6x1cb2SpA9UEMLMf):

| commit | subject | git cherry |
|---|---|---|
| 9e20ad4 | Add files via upload (2026-09-11, upstream commit) | + (patch-id absent on main) |
| dd6ab26 | Merge origin/claude/focused-wright-jw88w9 into QA worktree for PAY-DOUBLE-CREDIT-1 (merge; carries .claude/agents/*.md etc.) | (merge, not in cherry output) |
| 30deec1 | test(payments): PAY-DOUBLE-CREDIT-1 INV-DEP-1 test-first adversarial matrix (A-O) | + |
| b6dcefd | docs(qa): kill-switch phase 2 pre-merge verification | - (patch-id already on main) |
| 1564c34 | docs+test(qa): FH-3 adjudication; both reported failures were test bugs, corrected | + |

Net diff vs merge-base: 5 files, +2123 lines: two docs, three Go integration tests.

Supersession check (blob-hash comparison against main tip 95b17c5):
- docs/plans/payment-readiness/qa-fh3-adjudication.md: identical blob on main (888e474), landed as 5dbf432 "docs(qa): FH-3 adjudication ... INV-DEP-1 held 100/100".
- docs/plans/payment-readiness/qa-killswitch-phase2-verification.md: identical blob on main (fbf3ddd); cherry marks b6dcefd "-".
- The three Go tests were re-landed on main in re-organised form, not byte-identical, so `git cherry` shows "+": branch internal/payments/inv_dep1_recon_integration_test.go is now internal/reconciliation/inv_dep1_recon_integration_test.go; internal/payments/inv_dep1_correlation_integration_test.go is now internal/idempotency/inv_dep1_correlation_integration_test.go; internal/payments/inv_dep1_matrix_integration_test.go exists on main (blob 789e2e8 vs branch ba19d1a) and differs by a 38-line/2-line delta, i.e. main's copy is the later evolved version (the branch version is the QA original with the adjudication fix; main has FH-3b/3c/FOLLOWUP-1 changes). The adjudication commit message itself says the test files were committed in this worktree "only because the sandbox refused writes to the FH-3 branch" and were to be copied across by the coordinator; they were.
- 9e20ad4 / dd6ab26: unrelated upstream/merge commits (one is a 2026-09-11 "Add files via upload"; check if on origin before archiving: see step A).

Verdict: no unique unreviewed work found. Content is superseded by merged work (docs byte-identical; tests moved and evolved). The only residual uniqueness is the exact historical test text and 9e20ad4's patch-id (probably present on origin under a different SHA; unverified).

Recommendation: ARCHIVE then remove. Cheap insurance, because patch-ids do not match 1:1 and the owner said "unmerged": create an annotated tag `archive/qa-fh3-adjudication-20260927` at 1564c34 (a tag is 1 object, zero worktree cost), then the worktree/branch are safe to remove. Not "keep as-is": it holds nothing needing a live checkout.

## 2. Worktree classification (42 worktrees)

Method: `git worktree list --porcelain`, `git branch --merged claude/focused-wright-jw88w9`, `git status --porcelain` in each, `.git/worktrees/*/locked`, /proc/*/cwd scan, `lsof -F n` filtered for worktree paths, du -sm. Raw table: scratchpad/wt-class.tsv.

Summary:
- Merged into main branch (claude/focused-wright-jw88w9 @ 95b17c5): 41 of 42 (including main itself). Unmerged: 1 (aae2d0af, section 1).
- Dirty (including untracked): 0 of 42. Detached/prunable: 0.
- Locked: 0. There are NO `locked` files under .git/worktrees/*/ (glob matches nothing). The two worktrees the owner remembered as locked are not locked now; I cannot tell which two they were. Candidate: the two outside the repo, k1-int and prh2-flake (they live under the session scratchpad, not .claude/worktrees).
- Processes with cwd or open files in any worktree: none (checked all /proc/*/cwd and `lsof -n -w -F n`). Only the running claude process (this session) and environment-manager exist. No go test processes were running at inspection time.
- Sizes: ~25-36 MB each (about 1.2 GB for the 39 under .claude/worktrees; main checkout's 1658 MB du includes them).

| worktree dir (under .claude/worktrees unless noted) | branch | merged | owning workstream (from branch name / tip subject) |
|---|---|---|---|
| agent-a01ae21e13ce0b057 | worktree-agent-a01ae21e13ce0b057 | yes | casino: CAS-SESSION-EXPIRY-1 ADR 0095 amend |
| agent-a048d85a077316518 | worktree-agent-a048d85a077316518 | yes | PRH-I1 step (d) receipt resolution |
| agent-a130491df96f19d24 | worktree-agent-a130491df96f19d24 | yes | PRH-I3 fix round 4 (migration 0103) |
| agent-a1924f400143860c9 | prh2-d1-poll-amount | yes | PRH-2 D1 |
| agent-a2a24beace7d586cc | prh2-h-sweeper-process | yes | PRH-2 H |
| agent-a2c902fd1e221e88f | prh2-k1-tests | yes | PRH-2 K1 |
| agent-a2d486c88c91b23b1 | prh2-e1-impl | yes | PRH-2 E1 |
| agent-a2da1e8464724cb86 | prh2-k3-impl | yes | PRH-2 K3 |
| agent-a368b6ad544bcaee1 | prh2-c-dep-ref-validate | yes | PRH-2 C |
| agent-a37efb7c692abd9f7 | sb-catalogue-io-1 | yes | sportsbook catalogue IO-1 |
| agent-a3c3aee40f026fa3d | prh2-w0x-adrs-0102-0104 | yes | PRH-2 W0-X ADRs |
| agent-a3e036d2fcff03cb5 | i-core-alerting | yes | I-core alerting |
| agent-a49d13dcf2ff1dc33 | worktree-agent-a49d13dcf2ff1dc33 | yes | FH-7 final gate (qa) |
| agent-a4cb0c0e6b7ad79a2 | prh2-e1-design | yes | PRH-2 E1 design |
| agent-a4e8fbc8d827abc87 | prh2-d2-recon-parked-capture | yes | PRH-2 D2 |
| agent-a5b19b46582e95b75 | worktree-agent-a5b19b46582e95b75 | yes | FH-3/FH-5 merge helper |
| agent-a6480abb30d65f06e | worktree-agent-a6480abb30d65f06e | yes | RV-FH7 architect review |
| agent-a6572bf833858955c | prh2-k2-manual-adjustments | yes | PRH-2 K2 |
| agent-a76a4493bc4b36509 | prh2-b-cas-play-bootstrap | yes | PRH-2 B (casino) |
| agent-a8692de26b4d86460 | prh2-k3-design | yes | PRH-2 K3 design |
| agent-a91d93fbe9771bf0c | worktree-agent-a91d93fbe9771bf0c | yes | IO-1B/1C |
| agent-a933839eae8637a42 | worktree-agent-a933839eae8637a42 | yes | PRH-2 plan rev 3 |
| agent-a93a4337774aebd5a | claude/prh2-w0k-adrs-0099-0101 | yes | PRH-2 W0-K ADRs |
| agent-a954225fe60ae5fb0 | worktree-agent-a954225fe60ae5fb0 | yes | PRH-I4 |
| agent-a98cd7ccd3cb266f6 | prh2-iwire-alert-delivery | yes | PRH-2 I-wire |
| agent-a9ad199c50e9b929c | prh2-fpay-kyc-gate | yes | PRH-2 F-pay |
| agent-a9c58431468791c52 | prh2-k1-rc | yes | PRH-2 K1 RC |
| agent-aa01a06215f4419b7 | worktree-agent-aa01a06215f4419b7 | yes | PRH-I5 T13 |
| agent-aa1432d7d805cf425 | prh2-f-kyc-outage-testpins | yes | PRH-2 F-kyc |
| agent-aa274652a345b85f2 | prh2-g1-audit-tenant | yes | PRH-2 G1 |
| agent-aa2bb3c6bdd6d51eb | worktree-agent-aa2bb3c6bdd6d51eb | yes | KS-DEP-T2-T3-1 kill switch |
| **agent-aae2d0af6d7fc8c3d** | worktree-agent-aae2d0af6d7fc8c3d | **NO (5 ahead)** | PAY-DOUBLE-CREDIT-1 qa (section 1) |
| agent-ab06e5790daa1bcee | worktree-agent-ab06e5790daa1bcee | yes | PROVIDER-REF-BOUND-1 |
| agent-abaf38e970b6cd9fc | worktree-agent-abaf38e970b6cd9fc | yes | migration 0104 (PRH-I5 C2) |
| agent-ada606680c224e73e | prh2-j-admission-metrics | yes | PRH-2 J |
| agent-adce273a3a5339f77 | worktree-agent-adce273a3a5339f77 | yes | FH-3 / FH3-FOLLOWUP-1 impl |
| agent-add877127f6fd5bd6 | e2-prov-outbound-cred-1-legacy | yes | E2 provider outbound cred |
| agent-af24a8f826f637895 | prh2-k1-capability-grants | yes | PRH-2 K1 |
| agent-af8a1e737aadc71bf | worktree-agent-af8a1e737aadc71bf | yes | PRH-I2 |
| (scratchpad) k1-int | prh2-k1-int | yes | PRH-2 K1 integration; lives in session scratchpad |
| (scratchpad) prh2-flake | prh2-test-admission-flake-1 | yes | TEST-ADMISSION-FLAKE-1; session scratchpad |
| /home/user/igaming-platform | claude/focused-wright-jw88w9 | main | MAIN CHECKOUT, KEEP |

Caveat on "merged": `git branch --merged` is ancestor-based; it is also true for branches whose tip is an ancestor of main only because main merged them, i.e. nothing is lost. Branch names are NOT deleted by removing a worktree; deleting branches is a separate step (section 4, step 4).

## 3. Scratch databases

Totals: 667 non-template DBs, 11 GB. 620 are helper-generated (name = prefix + 16 hex chars, 10.1 GB); 47 are hand-named by agents/scripts (1.5 GB) via psql/priv_db.sh-style harnesses (priv_db.sh/priv_test.sh live in the session scratchpad, not the repo). The ~666 the owner mentioned = 667 minus `postgres`.
Creation time source: mtime of base/<oid>/PG_VERSION (read-only stat; pg_stat_file is denied to the test admin). Raw data: scratchpad/dbs-time.tsv, fam.tsv.

Creation histogram: 09-26: 4, 09-27: 38, 09-28: 145, 10-03: 305, 10-04: 148, 10-05: 26. Newest helper DB is ~148 min old; no test run was creating DBs at inspection time.

Top families (helper-generated; 16-hex suffix), count / size / created range (UTC) / creating helper:

| prefix | n | MB | range | creator (git grep) |
|---|---|---|---|---|
| m0101v2_ | 331 | 5534 | 09-28 10:59 to 10-05 03:22 | internal/payments/deposit_v2_integration_test.go depositV2ScratchPool -> migration0101Scratch -> scratchdb.New; fully migrated, about 17 MB each |
| ks_api_ | 49 | 783 | 09-28 to 10-05 | internal/httpserver/payments_kill_switch_api_integration_test.go ksScratchPool |
| cg_api_ | 13 | 216 | 09-28 to 10-04 | internal/httpserver/capability_api_integration_test.go |
| kyc0114refuse | 12 | 203 | 10-04 | internal/kyc/migration_0114_integration_test.go scratchThrough0114 |
| m0102_ | 11 | 164 | 09-28 to 10-05 | internal/payments migration tests |
| ledger48_ | 8 | 124 | 10-03 to 10-04 | internal/ledger/migration_0048_integration_test.go |
| m0105* (many sub-prefixes: m0105_h1*, m0105ks_*, m0105a..l, m0105_k*, m0105_n1_k19b) | about 70 | about 1000 | 09-28 to 10-04 | internal/payments/migration_0105_integration_test.go migration0105Scratch, killswitch_integration_test.go |
| audit_api_, cg_i4_, invdep1_recon_legacy_, adisp*, k2*, kyc0114*, kyc_m0100_, jur0075_, om0076_, sb0091*/sb0093*, cas0094*/cas0108/cas0111, cap0112*, alert0110*, catrls_truncate_, ledgerimm_truncate_, sbimm_truncate_, dbchecksum_, m0097..m0109*, pc_rt_, recon_sb_, iwdedup | rest | rest | 09-28 to 10-05 | all via internal/testsupport/scratchdb.New(t, prefix) (34 files use it) |

Hand-named (47, 1.5 GB, 03-31 to 10-05): igaming_ci_flake, igaming_w2a/w2b/w3a_local, igaming_prhcb_local/_r4, igaming_prhref_local, igaming_prh_i5/_cr/_cr_parent, igaming_reversibility, prhi1_cutover_scratch, fh3c, fh5_pay_20260927, cas_bfix_dev, k1cap_test1, ks_0106_renum, ks_1c_*, ks_i1b_*, kyc_r1_fix_priv{,2,3}, kyc_r2_*, ph2_*, ph3_*, ph4_*, plus the two KEEP DBs and `postgres`. These are private harness DBs from named workstreams of 09-26..09-28 (PRIV_DB evidence in docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt cites fh5_pay_20260927, fh3_invdep1_*). Not auto-cleaned; no test code ever drops them.

KEEP list (never delete):
- igaming_platform_ci_local (377 MB): the shared migrated TEST_DATABASE_URL DB, 1 active connection (this inventory's psql). 
- igaming_orch_local (63 MB): orchestrator's local DB (recreated 05:13 today).
- postgres, template0, template1.
- Anything with an active backend at deletion time: re-query pg_stat_activity then (at inspection only igaming_platform_ci_local had a connection).
- tl_char: does NOT exist yet (the timing agent's PRIV_DB=tl_char database has not been created, or lives under a different name). Do not touch any `tl_*` name. Re-check before any deletion; the agent may create it any time. No other DB is referenced by an active workstream that I could find; nothing in the repo (git grep PRIV_DB/tl_char) references a live DB beyond evidence docs.
- Suggested conservative hold: the 47 hand-named DBs (except the two KEEP ones) are only reasonable to drop after the owner confirms their evidence is already captured in docs (they are only referenced as historical evidence).

Does a normal test run clean them? Yes for passing, completed runs: scratchdb.New registers t.Cleanup that runs `DROP DATABASE IF EXISTS ... WITH (FORCE)` via a fresh admin connection (internal/testsupport/scratchdb/scratchdb.go), and callers register pool.Close later so it runs first (LIFO). Spot check shows 620 survivors, so cleanup is routinely bypassed.

Why they leak (code-level, hypotheses ranked; I could not reproduce because running tests was prohibited):
1. Cleanup only runs on normal test-binary exit. A `go test -timeout` panic, SIGKILL/Ctrl-C, OOM kill, agent tool-call timeout, or container/PG restart skips t.Cleanup entirely, and nothing reaps afterwards. The creation clusters (305 on 10-03, 148 on 10-04, many same-minute pairs/quadruples) match repeated mutation/-race/-count loops run by agents and killed or timed out. m0101v2_ is the worst because depositV2ScratchPool is called per test (and per subtest) and each DB is fully migrated (about 17 MB).
2. Drop failure is swallowed: Cleanup only does t.Logf("scratch database %s left behind ...") on connect or DROP failure (logged only with -v / on failure). If max_connections is exhausted by parallel packages (all packages create DBs and pools, 10 conns each, via db.Connect(..., 10, ...)), the cleanup connect fails and the DB leaks silently.
3. No name-prefixed run tag or timestamp in the DB name (prefix + random uuid 16 hex), so a janitor cannot tell live from dead except via PG_VERSION mtime/pg_stat_activity.
4. Hand-named PRIV_DB databases created by agent shell harnesses have no teardown at all.
5. Container restarts: PG data directory persists, but test binaries die; same effect as (1).

Proposed fix, TEST-HYGIENE follow-up (NOT implemented; owner to schedule):
- scratchdb: embed creation unix-time and pid in the name (e.g. prefix + yyyymmddhhmm + pid + 8 hex) or write a row to a registry table in the admin DB; keep suffix uniqueness.
- Add `scratchdb.Reap(ctx, maxAge)` called at the start of TestMain in a single common test-support package (or a `make test-db-reap` target / `cmd/` dev tool): list datnames matching known prefixes, skip those with active backends in pg_stat_activity and those younger than N hours, `DROP DATABASE ... WITH (FORCE)`. Needs only the existing CREATEDB test admin role (which can drop DBs it owns by role membership); no privilege change.
- Make cleanup failure loud: t.Errorf (not Logf) after retry, and a retry loop on connect.
- Reduce m0101v2_ fan-out: create the migrated scratch DB once per package (TestMain or sync.Once) and use `CREATE DATABASE ... TEMPLATE` (cloning, much faster and cleaner) per test; drop the template at package exit.
- Replace hand-named PRIV_DB use with a documented script that records and tears down its DBs (a `docs/runbooks/` entry); devops/qa to own the doc, to be done after the final gate.
- CI is unaffected (ephemeral service container), so this is purely a local/dev-hygiene problem.

## 4. DELETION PLAN (text only; do not run now)

Global preconditions (all must hold; stop if any fails):
P1. Final technical gate completed and the owner explicitly authorizes cleanup in writing (message from the owner, not an agent message).
P2. No agents running: `ps -eo args | grep -E 'claude|go (test|build|vet)|\.test'` shows only the orchestrator; the timing-characterization agent (PRIV_DB=tl_char) has finished and its results are copied out. Re-run `lsof -n -w -F n | grep -E '\.claude/worktrees|scratchpad'` and the /proc cwd scan: empty.
P3. Idle-sensitive timing test is finished (deletion causes IO/CPU).
P4. Re-run `git fetch origin` and `git status` on main: clean; `git log origin/claude/focused-wright-jw88w9..claude/focused-wright-jw88w9` shows main is pushed (otherwise a removed branch could be the only copy of something).
P5. All commands use only the CREATEDB test-admin URL from env.sh. No sudo, no ALTER ROLE, no superuser. If the DROP is refused, STOP AND REPORT (CLAUDE.md environment-safety rule).

Step 0 (archive, mandatory before anything else; frees 0 bytes; rollback: `git tag -d`):
  git -C /home/user/igaming-platform tag -a archive/qa-fh3-adjudication-20260927 1564c34 -m "Archive of worktree-agent-aae2d0af6d7fc8c3d before cleanup; content superseded on main (5dbf432 and later)"
  git -C /home/user/igaming-platform push origin archive/qa-fh3-adjudication-20260927   # only if owner wants it remote
Also verify: `git merge-base --is-ancestor 5dbf432 claude/focused-wright-jw88w9 && echo ok`.

Step 1 (re-verify merged state; read-only):
  cd /home/user/igaming-platform
  for b in $(git branch --merged claude/focused-wright-jw88w9 --format='%(refname:short)' | grep -E '^(worktree-agent-|prh2-|sb-|i-core|e2-|claude/prh2)'); do echo $b; done > /tmp/claude-0/.../scratchpad/delete-branches.txt
  for p in .claude/worktrees/agent-*; do git -C $p status --porcelain | wc -l; done   # all must be 0

Step 2 (remove the 40 clean worktrees under .claude/worktrees (39 dirs) plus the 2 scratchpad ones; expected freed: about 1.2 GB + 62 MB). Risk: low, content is in git; untracked-ignored files (build caches) are lost, which are regenerable. NOTE `git worktree remove` refuses dirty/locked trees (do NOT use --force). Rollback: `git worktree add <path> <branch>` as long as the branch still exists (so do step 2 before step 4).
  cd /home/user/igaming-platform
  for p in .claude/worktrees/agent-*; do git worktree remove "$p" || echo "REFUSED $p"; done
  git worktree remove /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/k1-int
  git worktree remove /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/prh2-flake
  git worktree list    # expect only the main checkout

Step 3 (prune stale metadata, only if listing shows prunable entries): `git worktree prune --dry-run` first, then `git worktree prune`. Frees KBs.

Step 4 (optional, owner decision; delete merged local branches; frees about 0 bytes, only clutter). Rollback: branch tips are recoverable from reflog (`git branch <name> <sha>`) for the default 90 days; tip SHAs are in scratchpad/wt.tsv and in the table above. Use lowercase `-d` only (refuses unmerged), never `-D`:
  git branch -d $(cat delete-branches.txt)
  # worktree-agent-aae2d0af6d7fc8c3d is unmerged: -d will refuse; after Step 0 archive, delete with `git branch -D worktree-agent-aae2d0af6d7fc8c3d` ONLY with owner OK (tag preserves the commits).

Step 5 (scratch DBs, helper-generated: 620 DBs, about 10.1 GB freed; space shows up on disk after DROP; autovacuum not involved). Risk: medium only if a test/agent is using a DB; mitigation is the age and activity filters. Rollback: none (data is synthetic and regenerated by the tests); re-running the migrations rebuilds anything needed. Generate the list first, review, then drop:
  . /tmp/claude-0/.../scratchpad/env.sh
  psql "$TEST_ADMIN_DATABASE_URL" -X -A -t -c "
   select datname from pg_database d
    where datname ~ '[0-9a-f]{16}\$' and not datistemplate
      and datname not in ('igaming_platform_ci_local','igaming_orch_local','postgres')
      and datname not like 'tl\_%'
      and not exists (select 1 from pg_stat_activity a where a.datname = d.datname)
    order by 1" > /tmp/claude-0/.../scratchpad/drop-list.txt
  wc -l drop-list.txt     # expect about 620; review head/tail
  while read d; do psql "$TEST_ADMIN_DATABASE_URL" -X -q -c "DROP DATABASE IF EXISTS \"$d\"" || echo "FAILED $d"; done < drop-list.txt
  (Deliberately no WITH (FORCE): a DB with a live backend is refused instead of killed. Optionally do it in two passes, oldest first: add `and <creation older than 24h>` using filesystem mtimes from dbs-time.tsv, to minimise collision with anything started after the inventory.)

Step 6 (hand-named PRIV_DB scratch DBs, 44 DBs, about 1.1 GB; owner confirmation required per DB family because they hold evidence-run state): drop only after owner says the evidence docs (docs/plans/payment-readiness/evidence/*) are sufficient. List = hand-named list in section 3 minus igaming_platform_ci_local, igaming_orch_local, postgres, and any tl_*:
  for d in cas_bfix_dev fh3c fh5_pay_20260927 igaming_ci_flake igaming_prh_i5 igaming_prh_i5_cr igaming_prh_i5_cr_parent igaming_prhcb_local igaming_prhcb_r4 igaming_prhref_local igaming_reversibility igaming_w2a_local igaming_w2b_local igaming_w3a_local k1cap_test1 ks_0106_renum ks_1c_n1 ks_1c_race_http ks_1c_race_pay ks_i1b_final ks_i1b_fixed ks_i1b_mut ks_i1b_updown kyc_r1_fix_priv kyc_r1_fix_priv2 kyc_r1_fix_priv3 kyc_r2_cmd kyc_r2_fix kyc_r2_fix2 kyc_r2_withdrawal ph2_am1 ph2_credswitch ph2_deposit ph2_final_http ph2_final_pay ph2_race_final_http ph2_race_final_pay ph2_rv2 ph3_c2 ph3_full_http ph3_full_pay ph4_ksdep ph4_ksdep_race prhi1_cutover_scratch; do psql "$TEST_ADMIN_DATABASE_URL" -X -q -c "DROP DATABASE IF EXISTS \"$d\""; done
  Caveat: igaming_w2a/w2b/w3a_local, igaming_prh*_local, igaming_ci_flake look like earlier orchestrator/CI-reproduction DBs; confirm with the owner before dropping.

Step 7 (verify): `select count(*), pg_size_pretty(sum(pg_database_size(datname))) from pg_database where not datistemplate;` expect about 3 to 4 rows (postgres, igaming_platform_ci_local, igaming_orch_local, +tl_char if present) and about 0.5 GB; `df -h /` should show about 15.7 GB free (from 5.6 GB) after steps 2, 5, 6.

Expected space freed: worktrees about 1.25 GB; helper DBs about 10.1 GB; hand-named DBs about 1.1 GB (excl. KEEPs). Total about 12.4 GB (current free space: 5.6 GB).

Residual ordering notes: do Step 2 before Step 4; do Step 0 before everything; after cleanup, start the TEST-HYGIENE follow-up so DBs do not re-accumulate (observed: 150-300 DBs on busy days at about 17 MB each).
