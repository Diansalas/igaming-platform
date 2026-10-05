# Cleanup manifest 2026-10-05 (fresh inventory, Step A)

Taken 2026-10-05 ~19:17 UTC via read-only queries as igaming_test_admin (CREATEDB, non-superuser). Active sessions on any non-system DB at inventory time: 0. All databases are owned by role `igaming`. Creation time = mtime of `base/<oid>/PG_VERSION` (approximate). Passwords are not recorded.

## Classification rules

- ACTIVE/REQUIRED: `igaming_orch_local`, `igaming_platform_ci_local` (the DB named in env.sh / TEST_*_DATABASE_URL, CI, Makefile defaults), or any DB with a session.
- SYSTEM: `postgres` (templates are excluded from the listing and untouched).
- OBSOLETE SCRATCH: name = lowercase prefix + exactly 16 hex chars (scratchdb.New format: prefix + first 16 hex of a UUID), not `igaming_*`, 0 sessions, name not mentioned in any scratchpad `*.sh`. Normally dropped by t.Cleanup (WITH FORCE); these are leftovers from killed/timed-out runs.
- UNKNOWN (KEEP): everything else, including all literal-named DBs (`igaming_w2a/w2b/w3a_local`, `igaming_prh*`, `ks_*`, `kyc_r*`, `ph*_*`, `cas_bfix_dev`, `fh3c`, ...). None of them match the PRIV_DB scripted naming list (a_temp, e_alert, f_psp, sec_*, lf_*, cr_*, qa_*, tl_*, m*_priv, k3_*, e1_priv) - those were already gone - so they are not dropped by this cleanup.

## Totals by class

| class | count | size GB |
|---|---|---|
| ACTIVE/REQUIRED | 2 | 0.44 |
| SYSTEM | 1 | 0.01 |
| OBSOLETE SCRATCH | 748 | 12.01 |
| UNKNOWN | 44 | 1.05 |
| total | 795 | 13.51 |

REQUIRED BY TESTS: only `igaming_platform_ci_local` (env.sh, CI-equivalent shared DB) and `igaming_orch_local`; scratch DBs are created on demand by scratchdb.New, so none of the scratch DBs is required. Hard-coded literals in committed code: `git grep` of every candidate full name over tracked files returns no hits; committed code only references prefixes (e.g. `ks_api_`, `m0101v2_`) which generate fresh random names.

## Kept (non-scratch) databases

| name | MB | created | class |
|---|---|---|---|
| cas_bfix_dev | 15 | 09-28 13:15 | UNKNOWN |
| fh3c | 33 | 09-28 00:21 | UNKNOWN |
| fh5_pay_20260927 | 36 | 09-27 19:24 | UNKNOWN |
| igaming_ci_flake | 37 | 09-26 08:47 | UNKNOWN |
| igaming_orch_local | 75 | 10-05 18:36 | ACTIVE/REQUIRED |
| igaming_platform_ci_local | 376 | 09-27 00:36 | ACTIVE/REQUIRED |
| igaming_prh_i5 | 25 | 09-27 11:31 | UNKNOWN |
| igaming_prh_i5_cr | 47 | 09-27 10:58 | UNKNOWN |
| igaming_prh_i5_cr_parent | 16 | 09-27 11:04 | UNKNOWN |
| igaming_prhcb_local | 47 | 09-27 12:07 | UNKNOWN |
| igaming_prhcb_r4 | 16 | 09-27 17:27 | UNKNOWN |
| igaming_prhref_local | 15 | 09-27 09:15 | UNKNOWN |
| igaming_reversibility | 14 | 09-27 00:54 | UNKNOWN |
| igaming_w2a_local | 28 | 09-26 18:27 | UNKNOWN |
| igaming_w2b_local | 50 | 09-26 17:36 | UNKNOWN |
| igaming_w3a_local | 89 | 09-26 18:03 | UNKNOWN |
| k1cap_test1 | 16 | 09-28 16:07 | UNKNOWN |
| ks_0106_renum | 16 | 09-27 14:46 | UNKNOWN |
| ks_1c_n1 | 14 | 09-27 16:20 | UNKNOWN |
| ks_1c_race_http | 34 | 09-27 16:39 | UNKNOWN |
| ks_1c_race_pay | 16 | 09-27 16:39 | UNKNOWN |
| ks_i1b_final | 28 | 09-27 14:31 | UNKNOWN |
| ks_i1b_fixed | 14 | 09-27 14:01 | UNKNOWN |
| ks_i1b_mut | 14 | 09-27 14:04 | UNKNOWN |
| ks_i1b_updown | 14 | 09-27 14:36 | UNKNOWN |
| kyc_r1_fix_priv | 18 | 09-27 11:55 | UNKNOWN |
| kyc_r1_fix_priv2 | 16 | 09-27 11:59 | UNKNOWN |
| kyc_r1_fix_priv3 | 17 | 09-27 12:23 | UNKNOWN |
| kyc_r2_cmd | 14 | 09-27 13:26 | UNKNOWN |
| kyc_r2_fix | 28 | 09-27 12:48 | UNKNOWN |
| kyc_r2_fix2 | 17 | 09-27 12:48 | UNKNOWN |
| kyc_r2_withdrawal | 22 | 09-27 13:26 | UNKNOWN |
| ph2_am1 | 14 | 09-27 17:14 | UNKNOWN |
| ph2_credswitch | 16 | 09-27 17:24 | UNKNOWN |
| ph2_deposit | 16 | 09-27 17:08 | UNKNOWN |
| ph2_final_http | 30 | 09-27 17:39 | UNKNOWN |
| ph2_final_pay | 16 | 09-27 17:39 | UNKNOWN |
| ph2_race_final_http | 32 | 09-27 17:52 | UNKNOWN |
| ph2_race_final_pay | 16 | 09-27 17:52 | UNKNOWN |
| ph2_rv2 | 14 | 09-27 17:33 | UNKNOWN |
| ph3_c2 | 14 | 09-27 19:16 | UNKNOWN |
| ph3_full_http | 32 | 09-27 19:28 | UNKNOWN |
| ph3_full_pay | 16 | 09-27 19:28 | UNKNOWN |
| ph4_ksdep | 15 | 09-27 19:44 | UNKNOWN |
| ph4_ksdep_race | 16 | 09-27 19:47 | UNKNOWN |
| postgres | 7 | 03-31 13:23 | SYSTEM |
| prhi1_cutover_scratch | 44 | 09-27 10:36 | UNKNOWN |

## Obsolete scratch by family (prefix before 16-hex suffix)

| family prefix | count | MB | oldest | newest |
|---|---|---|---|---|
| m0101v2_ | 393 | 6646 | 09-28 10:59 | 10-05 18:51 |
| ks_api_ | 56 | 904 | 09-28 10:49 | 10-05 18:44 |
| cg_api_ | 17 | 288 | 09-28 19:04 | 10-05 18:43 |
| kyc0114refuse | 16 | 270 | 10-04 19:55 | 10-05 18:40 |
| m0102_ | 13 | 194 | 09-28 16:16 | 10-05 18:21 |
| ledger48_ | 8 | 123 | 10-03 10:23 | 10-04 09:08 |
| temprev_ | 6 | 102 | 10-05 07:59 | 10-05 09:25 |
| m0105e | 6 | 97 | 09-28 11:49 | 10-04 21:39 |
| audit_api_ | 6 | 92 | 09-28 17:13 | 10-05 18:36 |
| m0105_h1c | 5 | 84 | 10-03 11:02 | 10-05 18:24 |
| m0105_k14 | 5 | 82 | 10-03 10:42 | 10-05 08:40 |
| jur0075_ | 5 | 81 | 09-28 05:56 | 10-05 18:13 |
| m0105_n1_k19b | 5 | 79 | 09-28 11:30 | 10-05 18:25 |
| invdep1_recon_legacy_ | 5 | 77 | 09-28 11:57 | 10-03 22:49 |
| m0105ks_b | 4 | 67 | 10-03 13:59 | 10-05 18:24 |
| m0105_h1d3 | 4 | 65 | 10-03 11:16 | 10-03 15:34 |
| m0105ks_c | 4 | 65 | 10-03 11:16 | 10-03 15:35 |
| m0105g | 4 | 65 | 10-03 11:03 | 10-04 09:12 |
| adisp1 | 4 | 65 | 09-28 14:14 | 10-05 18:33 |
| m0105ks_d | 4 | 65 | 10-03 10:26 | 10-03 22:11 |
| cg_i4_ | 4 | 64 | 09-28 17:09 | 10-05 02:56 |
| kyc_m0100_ | 4 | 57 | 09-28 11:49 | 10-05 18:39 |
| om0076_ | 3 | 54 | 09-28 05:56 | 10-05 18:15 |
| temprevk2_ | 3 | 53 | 10-05 11:43 | 10-05 11:44 |
| k3matrix_ | 3 | 51 | 10-05 08:40 | 10-05 09:48 |
| k2tight_ | 3 | 50 | 10-03 10:48 | 10-04 17:37 |
| ledgerimm_truncate_ | 3 | 50 | 10-03 10:42 | 10-05 18:15 |
| kyc0114scope | 3 | 50 | 10-04 23:29 | 10-05 00:32 |
| k2m0113b_ | 3 | 50 | 10-03 10:54 | 10-05 00:43 |
| m0105_h1e | 3 | 49 | 10-03 11:19 | 10-05 08:38 |
| m0101t12sib_ | 3 | 48 | 09-28 11:14 | 10-04 09:11 |
| m0105a | 3 | 47 | 09-28 11:52 | 10-03 22:19 |
| cas0094clean_ | 3 | 46 | 09-28 11:14 | 10-04 09:20 |
| cas0108_ | 3 | 45 | 10-05 00:45 | 10-05 18:38 |
| m0107down_ | 3 | 45 | 09-28 19:49 | 10-03 22:13 |
| m0101sweep_ | 3 | 44 | 10-03 12:09 | 10-05 09:00 |
| m0097trg_ | 3 | 43 | 09-28 19:39 | 10-05 18:48 |
| sb0091clean_ | 3 | 43 | 09-28 11:05 | 10-05 18:23 |
| m0107dupattempt_ | 3 | 38 | 09-28 19:49 | 10-05 12:09 |
| k3exp_ | 2 | 36 | 10-04 22:16 | 10-04 23:55 |
| kyc0114alert | 2 | 33 | 10-04 19:52 | 10-04 21:49 |
| kyc0114updown | 2 | 33 | 10-04 20:26 | 10-04 21:50 |
| m0106_m1 | 2 | 33 | 10-03 14:12 | 10-03 16:04 |
| m0106_l4 | 2 | 33 | 10-03 11:18 | 10-05 08:40 |
| m0106_l2c | 2 | 33 | 10-03 12:12 | 10-03 22:01 |
| m0101t12leg_ | 2 | 33 | 10-03 14:11 | 10-03 16:04 |
| catrls_truncate_ | 2 | 33 | 10-03 10:48 | 10-03 11:02 |
| m0105d | 2 | 33 | 10-03 12:10 | 10-03 17:08 |
| sb0091denytrig_ | 2 | 32 | 09-28 11:00 | 10-03 11:14 |
| m0105_k19 | 2 | 32 | 10-03 11:19 | 10-04 13:26 |
| cap0112r12c | 2 | 32 | 09-28 19:44 | 10-05 18:38 |
| cap0112la | 2 | 31 | 09-28 18:53 | 10-04 23:56 |
| m0105i | 2 | 31 | 09-28 11:23 | 10-03 15:21 |
| m0105_h1a | 2 | 31 | 09-28 11:48 | 10-03 11:02 |
| alert0110down | 2 | 30 | 09-28 15:02 | 09-28 19:40 |
| kyc0114seed | 2 | 30 | 10-04 21:32 | 10-04 22:03 |
| m0107valid_ | 2 | 30 | 09-28 16:10 | 10-03 15:35 |
| m0107dupledger_ | 2 | 30 | 10-03 15:35 | 10-04 09:27 |
| sb0091tomb_ | 2 | 30 | 09-28 11:27 | 09-28 19:54 |
| m0101bf_ | 2 | 29 | 09-28 11:13 | 10-05 18:24 |
| m0109clean_ | 2 | 29 | 10-03 10:50 | 10-05 18:59 |
| m0109tstaff_ | 2 | 29 | 09-28 19:39 | 10-04 10:49 |
| cas0094pre_ | 2 | 29 | 09-28 11:14 | 09-28 11:22 |
| sb0093onlyfn_ | 2 | 29 | 09-28 11:03 | 09-28 11:28 |
| dbchecksum_ | 4 | 29 | 09-28 15:49 | 10-04 23:02 |
| m0101rt_ | 2 | 29 | 10-03 16:04 | 10-04 09:09 |
| m0097mm_ | 2 | 28 | 09-28 19:45 | 10-03 22:51 |
| m0097rej_ | 2 | 28 | 09-28 19:53 | 10-04 09:05 |
| m0099rt_ | 2 | 28 | 09-28 11:19 | 09-28 11:25 |
| m0105j | 2 | 27 | 09-28 16:05 | 10-03 11:19 |
| cas0094rt_ | 2 | 27 | 09-28 11:06 | 09-28 11:26 |
| kyc095rt_ | 2 | 27 | 09-28 11:47 | 10-05 18:39 |
| k3down3_ | 1 | 18 | 10-04 22:17 | 10-04 22:17 |
| k3down2_ | 1 | 17 | 10-05 00:28 | 10-05 00:28 |
| m0106_l3 | 1 | 17 | 10-05 08:40 | 10-05 08:40 |
| k2layer_ | 1 | 17 | 10-03 10:47 | 10-03 10:47 |
| m117_dn2 | 1 | 17 | 10-05 18:10 | 10-05 18:10 |
| aflushcancel | 1 | 17 | 10-05 18:08 | 10-05 18:08 |
| actxdetach | 1 | 17 | 10-05 18:33 | 10-05 18:33 |
| recon_sb_ | 1 | 17 | 09-28 18:32 | 09-28 18:32 |
| adispesc | 1 | 17 | 10-05 18:33 | 10-05 18:33 |
| iwloop | 1 | 17 | 10-05 09:08 | 10-05 09:08 |
| adisp2 | 1 | 17 | 10-05 08:26 | 10-05 08:26 |
| ahk | 1 | 17 | 10-05 18:33 | 10-05 18:33 |
| m0105_k11 | 1 | 17 | 10-05 11:59 | 10-05 11:59 |
| alert0110ref | 1 | 17 | 10-05 09:08 | 10-05 09:08 |
| k2b20_ | 1 | 17 | 10-05 18:37 | 10-05 18:37 |
| kyc0114subconf | 1 | 17 | 10-04 23:53 | 10-04 23:53 |
| k2g4p_ | 1 | 17 | 10-03 10:47 | 10-03 10:47 |
| k2g4t_ | 1 | 16 | 10-05 08:17 | 10-05 08:17 |
| m0105h | 1 | 16 | 10-03 22:19 | 10-03 22:19 |
| adispstalex | 1 | 16 | 10-04 23:39 | 10-04 23:39 |
| adisp0 | 1 | 16 | 10-05 00:43 | 10-05 00:43 |
| m0106_l2a | 1 | 16 | 10-04 09:10 | 10-04 09:10 |
| m0105_h1d | 1 | 16 | 10-03 22:00 | 10-03 22:00 |
| k2g2lay | 1 | 16 | 10-03 11:02 | 10-03 11:02 |
| iwdedup | 1 | 16 | 10-03 22:19 | 10-03 22:19 |
| m0105c | 1 | 16 | 10-03 12:11 | 10-03 12:11 |
| adispescpos | 1 | 16 | 10-03 22:07 | 10-03 22:07 |
| m0105l | 1 | 16 | 10-03 14:12 | 10-03 14:12 |
| m0105b | 1 | 16 | 10-03 15:21 | 10-03 15:21 |
| m0105_h1b | 1 | 16 | 10-03 11:02 | 10-03 11:02 |
| cas0111trg_ | 1 | 16 | 09-28 19:41 | 09-28 19:41 |
| cap0112a17 | 1 | 16 | 10-05 00:07 | 10-05 00:07 |
| cap0112r12a21 | 1 | 15 | 10-05 12:44 | 10-05 12:44 |
| cap0112i5c23 | 1 | 15 | 10-03 10:41 | 10-03 10:41 |
| cap0112updown | 1 | 15 | 09-28 18:19 | 09-28 18:19 |
| alert0110updown | 1 | 15 | 09-28 15:02 | 09-28 15:02 |
| sb0091t2playerscope_ | 1 | 15 | 09-28 10:45 | 09-28 10:45 |
| alert0110downroutes | 1 | 15 | 09-28 15:02 | 09-28 15:02 |
| sbimm_truncate_ | 1 | 15 | 09-28 11:52 | 09-28 11:52 |
| m0109rt_ | 1 | 14 | 10-05 18:59 | 10-05 18:59 |
| m0105f | 1 | 14 | 09-28 11:02 | 09-28 11:02 |
| m0105ks_a | 1 | 14 | 09-28 11:29 | 09-28 11:29 |
| m0107rt_ | 1 | 14 | 09-28 19:49 | 09-28 19:49 |
| m0101pfb_ | 1 | 14 | 10-03 22:19 | 10-03 22:19 |
| m0098mm_ | 1 | 14 | 10-03 22:14 | 10-03 22:14 |
| m0098rt_ | 1 | 14 | 09-28 11:59 | 09-28 11:59 |
| sb0091mismatch_ | 1 | 14 | 09-28 11:30 | 09-28 11:30 |
| m0099pf_ | 1 | 14 | 09-28 05:01 | 09-28 05:01 |
| pc_rt_ | 1 | 14 | 10-03 17:51 | 10-03 17:51 |
| kyc095_ | 1 | 13 | 09-28 11:14 | 09-28 11:14 |
| sb0093guard_ | 1 | 7 | 09-28 05:02 | 09-28 05:02 |

## Execution record (Steps B-E, 2026-10-05)

### Deleted
- 748 OBSOLETE SCRATCH databases (12.01 GB by pg_database_size) dropped with plain `DROP DATABASE` (no FORCE) as igaming_test_admin, in 15 batches of 50 with a pg_stat_activity re-check before each batch; 0 skipped, 0 failures. Largest families: m0101v2_x393, ks_api_x56, cg_api_x17, kyc0114refusex16, m0102_x13, ledger48_x8, audit_api_x6, m0105ex6; 123 families in total (per-family counts in the table above). Names only: `cleanup-dropped-databases-2026-10-05.txt` (748 lines).
- Git worktree `.claude/worktrees/agent-aae2d0af6d7fc8c3d` removed with `git worktree remove` (no --force; it was clean). Branch `worktree-agent-aae2d0af6d7fc8c3d` KEPT.

### Archive tag
`archive/qa-fh3-adjudication-20260927` (annotated, local only, not pushed) -> 1564c34. Verification that nothing is unique: the two docs have identical blobs on the current branch (qa-fh3-adjudication.md 888e474, qa-killswitch-phase2-verification.md fbf3ddd); all function names of the three INV-DEP-1 Go tests exist on the current branch (relocated to internal/idempotency and internal/reconciliation, matrix test evolved by bdb9085/1ffc904); 9e20ad4 only adds the Blueprint PDF, already on origin/main; dd6ab26 is a merge. `git cherry` "+" marks are due to relocation/evolution, not missing content.

### Kept
- ACTIVE/REQUIRED: igaming_platform_ci_local, igaming_orch_local (both verified connectable).
- SYSTEM: postgres and templates.
- UNKNOWN (44, 1.05 GB): all literal-named DBs listed above (igaming_w2a/w2b/w3a_local, igaming_prh*, ks_*, kyc_r*, ph*_*, fh3c, cas_bfix_dev, ...). Not matching scratchdb.New format or the scripted PRIV_DB naming; owner decision needed before any drop.
- All other worktrees and all branches untouched.

### Result
- Database count 795 -> 47 (1.5 GB remaining). Create+drop probe DB succeeded (cleanup_probe_*).
- Root filesystem used 36.9 GB -> 24.3 GB (free 1.9 GB -> 14.6 GB, 95% -> 63%); includes worktree and Postgres WAL/temp effects.
- `git worktree prune --dry-run -v` printed nothing (no stale entries). No go build/test run.
