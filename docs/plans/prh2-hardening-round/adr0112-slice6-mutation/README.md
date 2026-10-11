# ADR 0112 slice 6: mutation drivers and kill logs

Each driver applies one textual mutation at a time to the working tree, runs the tests, records
KILLED/SURVIVED and restores the file (in `finally`). Run from a clean tree, one instance at a time
(two instances overwrite each other's mutations); `git status` must be clean afterwards.

Go (needs a private scratch database built by the harness, never the shared CI database):

    export PRIV_DB=<your private db name>            # created by priv_db.sh
    export PRIV_HARNESS_DIR=<dir with priv_test.sh>  # runs go test against $PRIV_DB
    python3 docs/plans/prh2-hardening-round/adr0112-slice6-mutation/go_mutants_round1.py
    python3 docs/plans/prh2-hardening-round/adr0112-slice6-mutation/go_mutants_round2.py

UI (needs `npm ci` in `backoffice/`):

    python3 docs/plans/prh2-hardening-round/adr0112-slice6-mutation/ui_mutants.py

`REPO_ROOT` overrides the repository root (default: four levels above this directory).
Round 1 notes: A2, A8 and H2 failed to compile (unused import), P2 did not apply, and A12, A13, R11, R12
survived until tests were added; round 2 replays them with compile-safe mutants. Remaining survivors
are equivalent mutants: R4 (equal-timestamp order cannot be produced, created_at is forced to now()
per transaction), D2, B12, B13.
