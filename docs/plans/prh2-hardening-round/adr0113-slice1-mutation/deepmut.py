"""Deeper-guard mutants (ADR 0113 s5 / C6): each drops ONE pre-existing guard that a converted test must
still prove (HR050 / HR041 / HR030 / MR041), by editing that guard's migration file on disk (the payments
tests build throwaway scratch databases from migrations/), runs the converted tests, and restores the
file byte for byte. A mutant must FAIL the named tests."""
import os, shutil, subprocess, sys

W = '/home/user/igaming-platform/.claude/worktrees/agent-a4cf5de1c68b569ae'
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
OUT = S + '/s1/mut/deeper_results.txt'
env = dict(os.environ, PRIV_DB='adr0113s1_mut', PRIV_SRC=W)

M = [
    ('D1_0124_freeze_HR050_off', 'migrations/0124_withdrawal_hold_resolution.up.sql',
     "    RAISE EXCEPTION 'withdrawal_requests: an approved hold on a non-active tenant or brand is frozen (approved -> % refused; ADR 0111 6.4, security M-6)', NEW.state USING ERRCODE = 'HR050';",
     "    RETURN NEW; -- MUTANT D1",
     'TestHSEC_HoldRelease_FreezeTrigger$|TestHSEC_HoldRelease_FreezeTrigger_UnreadableStatusFailsClosed|TestHSEC_HoldRelease_CTR3_KeyCorrelationShapeLinkForgeries'),
    ('D2_0124_HR041_two_leg_shape_off', 'migrations/0124_withdrawal_hold_resolution.up.sql',
     "            RAISE EXCEPTION 'withdrawal_hold_resolutions: executed release % entries are not exactly the approved two-leg shape', NEW.id USING ERRCODE = 'HR041';",
     "            NULL; -- MUTANT D2",
     'TestHSEC_HoldRelease_DeferredShapeCheck_CatchesFenceBypass'),
    ('D3_0124_HR030_governed_guard_off', 'migrations/0124_withdrawal_hold_resolution.up.sql',
     "            RAISE EXCEPTION 'withdrawal_requests: an executing hold resolution admits only approved -> rejected with the governed release link' USING ERRCODE = 'HR030';",
     "            NULL; -- MUTANT D3",
     'TestHSEC_HoldRelease_CTR3_KeyCorrelationShapeLinkForgeries'),
    ('D4_0125_MR041_M4_outcome_off', 'migrations/0125_payout_unbound_resolution_m4.up.sql',
     "                RAISE EXCEPTION 'payment_manual_resolutions: executed M4 % does not match its attempt/withdrawal outcome', NEW.id USING ERRCODE = 'MR041';",
     "                NULL; -- MUTANT D4",
     'TestM4_MR041_ForgedLinkAndPlantedPosting'),
]
open(OUT, 'w').close()
for mid, rel, old, new, run in M:
    p = os.path.join(W, rel)
    orig = open(p, 'rb').read()
    s = orig.decode()
    assert s.count(old) == 1, (mid, s.count(old))
    open(p, 'w').write(s.replace(old, new))
    try:
        r = subprocess.run(['bash', S + '/priv_test.sh', '-tags', 'integration', '-count=1', '-timeout', '1800s', '-v', '-run', run, './internal/payments/'],
                           env=env, capture_output=True, text=True)
    finally:
        open(p, 'wb').write(orig)
        assert open(p, 'rb').read() == orig, 'restore failed'
    log = r.stdout + r.stderr
    open(S + '/s1/mut/%s.log' % mid, 'w').write(log)
    fails = [l.split()[2] for l in log.splitlines() if l.startswith('--- FAIL')]
    line = '%s %s%s' % (mid, 'KILLED by: ' if r.returncode != 0 else 'SURVIVED', ' '.join(fails))
    print(line, flush=True)
    open(OUT, 'a').write(line + '\n')
