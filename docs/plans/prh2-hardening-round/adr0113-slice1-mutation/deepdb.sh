#!/bin/bash
# Deeper-guard DB mutants D5/D6 on the PRIVATE mutation database only: drop one pre-0132 trigger, run
# TestADR0113_DeeperGuardsStillArmedBehindTheStateGuard (must FAIL), recreate the trigger exactly.
S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad
OWNER="postgres://igaming:igaming_ci_password@127.0.0.1:5432/adr0113s1_mut?sslmode=disable"
OUT=$S/s1/mut/deeper_results.txt
export PRIV_DB=adr0113s1_mut PRIV_SRC=/home/user/igaming-platform/.claude/worktrees/agent-a4cf5de1c68b569ae
run() {
  id=$1; drop=$2; restore=$3
  psql "$OWNER" -q -v ON_ERROR_STOP=1 -c "$drop" || exit 1
  bash $S/priv_test.sh -tags integration -count=1 -timeout 300s -run 'TestADR0113_DeeperGuardsStillArmedBehindTheStateGuard' ./internal/withdrawal/ > $S/s1/mut/$id.log 2>&1
  rc=$?
  psql "$OWNER" -q -v ON_ERROR_STOP=1 -c "$restore" || { echo "RESTORE FAILED $id"; exit 1; }
  if [ $rc -ne 0 ]; then echo "$id KILLED by: TestADR0113_DeeperGuardsStillArmedBehindTheStateGuard ($(grep -m1 'want' $S/s1/mut/$id.log | sed 's/^ *//' | cut -c1-140))" | tee -a $OUT; else echo "$id SURVIVED" | tee -a $OUT; fi
}
run D5_0026_immutable_fields_trigger_dropped \
  "DROP TRIGGER withdrawal_requests_immutable_fields ON withdrawal_requests" \
  "CREATE TRIGGER withdrawal_requests_immutable_fields BEFORE UPDATE ON withdrawal_requests FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_enforce_immutable_fields()"
run D6_0124_approved_hold_freeze_trigger_dropped \
  "DROP TRIGGER withdrawal_requests_approved_hold_freeze ON withdrawal_requests" \
  "CREATE TRIGGER withdrawal_requests_approved_hold_freeze BEFORE UPDATE ON withdrawal_requests FOR EACH ROW WHEN (((old.state = 'approved'::text) AND (new.state IS DISTINCT FROM old.state))) EXECUTE FUNCTION withdrawal_requests_approved_hold_freeze()"
