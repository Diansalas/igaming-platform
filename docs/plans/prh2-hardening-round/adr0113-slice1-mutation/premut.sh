#!/bin/bash
# Pre-flight mutants: each drops ONE class counter from the 0132 DO block (file on disk), runs the
# scratch-database pre-flight test, then restores the file byte for byte.
S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad
W=/home/user/igaming-platform/.claude/worktrees/agent-a4cf5de1c68b569ae
F=$W/migrations/0132_withdrawal_state_machine_and_hold_invariant.up.sql
ORIG=$S/s1/0132.up.orig.sql
OUT=$S/s1/mut/preflight_results.txt
: > $OUT
# M28 (run the same way): the per-tenant `PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);`
# line replaced by a comment (a BLIND pre-flight); TestMigration0132_Preflight... must fail ("want WD098").
for c in reversed columns hold_link release hold_sum entry balance walletless no_hold_leg; do
  python3 - "$F" "$c" <<'EOF'
import sys
p, c = sys.argv[1], sys.argv[2]
s = open(p).read()
old = "        c_%s := c_%s + n;\n" % (c, c)
assert s.count(old) == 1, c
open(p, 'w').write(s.replace(old, "        -- MUTANT: class %s not counted\n" % c))
EOF
  PRIV_DB=adr0113s1_mut PRIV_SRC=$W bash $S/priv_test.sh -tags integration -count=1 -timeout 600s -run 'TestMigration0132_Preflight' ./internal/withdrawal/ > $S/s1/mut/pre_$c.log 2>&1
  rc=$?
  cp $ORIG $F
  cmp -s $F $ORIG || { echo "RESTORE FAILED"; exit 1; }
  if [ $rc -ne 0 ]; then echo "PRE_${c}_not_counted KILLED: $(grep -m1 'want' $S/s1/mut/pre_$c.log | sed 's/^ *//' | cut -c1-160)" | tee -a $OUT; else echo "PRE_${c}_not_counted SURVIVED" | tee -a $OUT; fi
done
