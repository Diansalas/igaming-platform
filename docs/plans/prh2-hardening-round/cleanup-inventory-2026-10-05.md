# Cleanup inventory (INVENTORY ONLY - nothing deleted, nothing proposed for deletion without owner authorization)

Taken 2026-10-05 after the final PRH-2 gate sweep, tree `119abf1`. Only `go clean -cache` was ever authorized (and used) so far.

## Scratch databases (local PostgreSQL 16, test fixtures)
666 non-system databases, about 11 GB total (up from 313 at the previous count: agent scratch databases created by integration tests and by review/mutation runs; some may be left by interrupted runs). Largest families:
- `m0101v2_*`: 331 databases, 5.5 GB (migration-0101 remediation tests)
- `ks_api_*`: 49 (784 MB); `m*` misc: 15; `m0105_h*`: 14; `cg_api_*`: 13; `kyc0114refus*`: 12 (E1); `m0105ks_*`: 11; `m0102_*`: 11; `ledger48_*`: 8; `m0105_k*`: 6; others smaller.
Not verified: whether any of these is still used by a live process (none of the agents is running now). The orchestrator database `igaming_orch_local` and the shared CI databases must be kept.

## Git worktrees (`.claude/worktrees`, 42 entries incl. the main checkout, 1.2 GB)
- Locked worktrees: `git worktree list --porcelain` reports none locked at this moment (the two previously locked worktrees were not found in the listing; not unlocked by us).
- Dirty worktrees: none.
- Worktrees whose branch is ahead of the main branch (unmerged commits): 1 (`worktree-agent-aae2d0af6d7fc8c3d`, 5 commits). Every other worktree branch (including the prh2-* implementation branches) is contained in the main branch.
- No worktree was removed except one temporary diagnostic worktree created and removed by the orchestrator during the timing-lane investigation.

## Caches
Go build cache 1.2 GB (re-creatable; `go clean -cache` already authorized and used repeatedly when disk filled). Free disk about 5.7 GB of 252 GB allotment.

## Authorization needed before anything else is deleted
Scratch database drop (by family), worktree removal (after confirming the one unmerged branch), unlocking, branch deletion.
