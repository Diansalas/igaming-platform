#!/bin/bash
# ADR 0113 slice-2 SQL mutation run on the PRIVATE mutation database (priv_lf_adr0113s23_mut only).
# For each mutant: CREATE OR REPLACE the guard with one change, run the LA025 / closure tests of
# internal/launchgov, record killed/survived, restore the migration's exact function.
S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad
D=$(cd "$(dirname "$0")" && pwd)
W=$S/s23mut
mkdir -p $W
export PRIV_DB=priv_lf_adr0113s23_mut
export PRIV_SRC=/home/user/igaming-platform/.claude/worktrees/agent-aca694322345cae73
OWNER="postgres://igaming:igaming_ci_password@127.0.0.1:5432/$PRIV_DB?sslmode=disable"
python3 -I $D/mutgen_s2.py $PRIV_SRC/migrations/0133_launch_closure_withdrawal_hold_balance_arm.up.sql $W
RUN="${MUT_RUN:-TestLA025|TestClosureBackstop|TestClosure_HoldBearingWithdrawals}"
OUT=$W/results_s2.txt
: > $OUT
psql "$OWNER" -q -v ON_ERROR_STOP=1 -f $W/restore.sql >/dev/null || { echo "restore failed"; exit 1; }
bash $S/priv_test.sh -tags integration -count=1 -p 1 -timeout 1200s -run "$RUN" ./internal/launchgov/ > $W/baseline_s2.log 2>&1
echo "BASELINE $(tail -1 $W/baseline_s2.log)" | tee -a $OUT
for f in $W/mutants/${MUT_ONLY:-S2M*}.sql; do
  id=$(basename $f .sql)
  psql "$OWNER" -q -v ON_ERROR_STOP=1 -f $f >/dev/null 2>$W/$id.apply.err || { echo "$id APPLY-FAILED $(head -1 $W/$id.apply.err)" | tee -a $OUT; psql "$OWNER" -q -f $W/restore.sql >/dev/null; continue; }
  bash $S/priv_test.sh -tags integration -count=1 -p 1 -timeout 1200s -run "$RUN" ./internal/launchgov/ > $W/$id.log 2>&1
  rc=$?
  killers=$(grep -E "^--- FAIL" $W/$id.log | awk '{print $3}' | tr '\n' ' ')
  if [ $rc -ne 0 ]; then echo "$id KILLED by: $killers" | tee -a $OUT; else echo "$id SURVIVED" | tee -a $OUT; fi
  psql "$OWNER" -q -v ON_ERROR_STOP=1 -f $W/restore.sql >/dev/null || { echo "restore failed"; exit 1; }
done
