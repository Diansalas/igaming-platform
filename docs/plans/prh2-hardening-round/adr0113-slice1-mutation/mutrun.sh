#!/bin/bash
# ADR 0113 slice-1 mutation run on the PRIVATE mutation database (adr0113s1_mut only).
# For each mutant: apply it, run the slice's withdrawal-package tests, record killed/survived, restore.
S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad
M=$S/s1/mut
export PRIV_DB=adr0113s1_mut
export PRIV_SRC=/home/user/igaming-platform/.claude/worktrees/agent-a4cf5de1c68b569ae
OWNER="postgres://igaming:igaming_ci_password@127.0.0.1:5432/adr0113s1_mut?sslmode=disable"
RUN="${MUT_RUN:-TestADR0113|TestMigration0132_Catalog}"
OUT=$M/results.txt
: > $OUT
# baseline (restored objects): must PASS
psql "$OWNER" -q -v ON_ERROR_STOP=1 -f $M/restore.sql >/dev/null || { echo "restore failed"; exit 1; }
bash $S/priv_test.sh -tags integration -count=1 -timeout 1200s -run "$RUN" ./internal/withdrawal/ > $M/baseline.log 2>&1
echo "BASELINE $(tail -1 $M/baseline.log)" | tee -a $OUT
for f in $M/mutants/${MUT_ONLY:-*}.sql; do
  id=$(basename $f .sql)
  psql "$OWNER" -q -v ON_ERROR_STOP=1 -f $f >/dev/null 2>$M/$id.apply.err || { echo "$id APPLY-FAILED $(head -1 $M/$id.apply.err)" | tee -a $OUT; psql "$OWNER" -q -f $M/restore.sql >/dev/null 2>&1; continue; }
  bash $S/priv_test.sh -tags integration -count=1 -timeout 300s -run "$RUN" ./internal/withdrawal/ > $M/$id.log 2>&1
  rc=$?
  killers=$(grep -E "^--- FAIL" $M/$id.log | awk '{print $3}' | tr '\n' ' ')
  if [ $rc -ne 0 ]; then echo "$id KILLED by: $killers" | tee -a $OUT; else echo "$id SURVIVED" | tee -a $OUT; fi
  psql "$OWNER" -q -v ON_ERROR_STOP=1 -f $M/restore.sql >/dev/null || { echo "restore failed after $id"; exit 1; }
done
