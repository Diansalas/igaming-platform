"""D4b: the whole MR041 executed-payout link check of 0125 (the last definition of
payment_manual_resolutions_no_executing_commit) disabled; TestM4_MR041_ForgedLinkAndPlantedPosting must
FAIL. The file is restored byte for byte."""
import os, subprocess
W = '/home/user/igaming-platform/.claude/worktrees/agent-a4cf5de1c68b569ae'
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
OUT = S + '/s1/mut/deeper_results.txt'
p = W + '/migrations/0125_payout_unbound_resolution_m4.up.sql'
orig = open(p, 'rb').read()
s = orig.decode()
old = "    IF r.state = 'executed' AND r.operation = 'payout' THEN"
assert s.count(old) == 1
open(p, 'w').write(s.replace(old, "    IF false THEN -- MUTANT D4b"))
try:
    r = subprocess.run(['bash', S + '/priv_test.sh', '-tags', 'integration', '-count=1', '-timeout', '1800s', '-v', '-run',
                        'TestM4_MR041_ForgedLinkAndPlantedPosting', './internal/payments/'],
                       env=dict(os.environ, PRIV_DB='adr0113s1_mut', PRIV_SRC=W), capture_output=True, text=True)
finally:
    open(p, 'wb').write(orig)
    assert open(p, 'rb').read() == orig
log = r.stdout + r.stderr
open(S + '/s1/mut/D4b.log', 'w').write(log)
fails = [l.split()[2] for l in log.splitlines() if l.strip().startswith('--- FAIL')]
line = 'D4b_0125_MR041_executed_payout_block_off %s%s' % ('KILLED by: ' if r.returncode != 0 else 'SURVIVED', ' '.join(fails))
print(line)
open(OUT, 'a').write(line + '\n')
