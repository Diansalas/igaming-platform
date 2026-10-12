"""Generate ADR 0113 slice-2 SQL mutants of launch_subject_status_guard (migration 0133).

usage: mutgen_s2.py <0133 up file> <outdir>
restore.sql      = the migration's guard function exactly (CREATE OR REPLACE) - re-applied after each mutant.
mutants/<id>.sql = the same function with ONE change.
"""
import os, sys

mig, outdir = sys.argv[1], sys.argv[2]
src = open(mig).read()
start = src.index('CREATE OR REPLACE FUNCTION launch_subject_status_guard()')
base = src[start:]
os.makedirs(os.path.join(outdir, 'mutants'), exist_ok=True)
open(os.path.join(outdir, 'restore.sql'), 'w').write(base)


def m(old, new, count=1):
    n = base.count(old)
    assert n == count, (old[:70], n)
    return base.replace(old, new)


READ = "SELECT count(*) INTO v_holds FROM public.wallet_balance_projection p"
M = {}
M['S2M01_la025_off'] = m("            IF v_holds > 0 THEN", "            IF false AND v_holds > 0 THEN")
M['S2M02_projection_read_blind'] = m("FROM public.wallet_balance_projection p",
                                     "FROM (SELECT * FROM public.wallet_balance_projection WHERE false) p")
M['S2M03_wallets_join_blind'] = m("SELECT 1 FROM public.wallets wl",
                                  "SELECT 1 FROM (SELECT * FROM public.wallets WHERE false) wl")
M['S2M04_read_outside_rls_window'] = m("            " + READ,
                                       "            PERFORM set_config('app.tenant_id', COALESCE(v_prev_t, ''), true);\n            " + READ)
M['S2M05_brand_filter_dropped'] = m("AND (v_brand IS NULL OR EXISTS (", "AND (true OR EXISTS (")
M['S2M06_totals_filter_dropped'] = m("AND p.credit_total <> p.debit_total", "AND true")
M['S2M07_wrong_account_type'] = m("p.account_type = 'player_withdrawal_hold'", "p.account_type = 'player_cash'")
M['S2M08_la023_off_la025_alone'] = m("            IF v_wd > 0 THEN", "            IF false AND v_wd > 0 THEN")
M['S2M09_player_scope_not_cleared'] = m(
    "            PERFORM set_config('app.player_account_id', '', true);\n            SELECT count(*) INTO v_wd",
    "            PERFORM set_config('app.player_account_id', gen_random_uuid()::text, true);\n            SELECT count(*) INTO v_wd")
for k, v in M.items():
    open(os.path.join(outdir, 'mutants', k + '.sql'), 'w').write(v)
print(len(M), 'mutants')
