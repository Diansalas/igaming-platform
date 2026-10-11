"""Generate ADR 0113 slice-1 SQL mutants from migration 0132.

objects.sql  = the migration's object section (everything after the pre-flight DO block), rewritten
               to be re-appliable: DROP TRIGGER IF EXISTS for the four triggers first, CREATE OR
               REPLACE FUNCTION, CREATE TRIGGER; the index is skipped.
restore.sql  = objects.sql unmodified.
mutants/<id>.sql = objects.sql with one mutation applied.
"""
import os, sys

mig, outdir = sys.argv[1], sys.argv[2]
src = open(mig).read()
start = src.index('-- 1. The state-transition guard')
start = src.rfind('-- ====', 0, start)
end = src.index('-- 5. The open-request index')
end = src.rfind('-- ====', 0, end)
body = src[start:end]
body = body.replace('CREATE FUNCTION', 'CREATE OR REPLACE FUNCTION')
drops = '''DROP TRIGGER IF EXISTS withdrawal_requests_aa_state_transition_guard ON withdrawal_requests;
DROP TRIGGER IF EXISTS withdrawal_requests_state_transition_guard ON withdrawal_requests;
DROP TRIGGER IF EXISTS withdrawal_requests_ledger_link_at_commit ON withdrawal_requests;
DROP TRIGGER IF EXISTS ledger_entries_withdrawal_hold_guard ON ledger_entries;
DROP TRIGGER IF EXISTS ledger_entries_withdrawal_hold_link_at_commit ON ledger_entries;
'''
base = drops + body
os.makedirs(os.path.join(outdir, 'mutants'), exist_ok=True)
open(os.path.join(outdir, 'restore.sql'), 'w').write(base)

def m(old, new, count=1):
    n = base.count(old)
    assert n == count, (old[:70], n)
    return base.replace(old, new)

M = {}
# Guard (withdrawal_requests_state_transition_guard)
M['M01_drop_aa_prefix'] = m('CREATE TRIGGER withdrawal_requests_aa_state_transition_guard', 'CREATE TRIGGER withdrawal_requests_state_transition_guard')
M['M02_insert_guard_off'] = m('''    IF TG_OP = 'INSERT' THEN
        -- Item 0 (B1): a row is born only as E0.
        IF NEW.state IS DISTINCT FROM 'requested\'''', '''    IF TG_OP = 'INSERT' THEN
        -- Item 0 (B1): a row is born only as E0.
        IF false AND NEW.state IS DISTINCT FROM 'requested\'''')
M['M03_terminal_mutable'] = m("IF OLD.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed') THEN", "IF false AND OLD.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed') THEN")
M['M04_edge_allowlist_open'] = m("        WHEN NEW.state = OLD.state THEN 'same'\n", "        WHEN NEW.state = OLD.state THEN 'same'\n        WHEN true THEN 'any'\n")
M['M05_frozen_columns_off'] = m("IF (to_jsonb(NEW) - c_dyn) IS DISTINCT FROM (to_jsonb(OLD) - c_dyn) THEN", "IF false THEN")
M['M06_release_rule_off'] = m('''    IF v_new_term THEN
        IF OLD.release_ledger_transaction_id IS NOT NULL OR NEW.release_ledger_transaction_id IS NULL THEN''', '''    IF false THEN
        IF OLD.release_ledger_transaction_id IS NOT NULL OR NEW.release_ledger_transaction_id IS NULL THEN''')
M['M06b_release_nonterminal_off'] = m("    ELSIF NEW.release_ledger_transaction_id IS NOT NULL OR OLD.release_ledger_transaction_id IS NOT NULL THEN", "    ELSIF false THEN")
M['M07_e7_provider_not_null_off'] = m("IF OLD.provider_id IS NOT NULL OR NEW.provider_id IS NULL THEN", "IF OLD.provider_id IS NOT NULL THEN")
M['M08_provider_ref_setonce_off'] = m('''    IF NEW.provider_reference IS DISTINCT FROM OLD.provider_reference
       AND NOT (OLD.provider_reference IS NULL''', '''    IF false AND NOT (OLD.provider_reference IS NULL''')
M['M09_hold_setonce_off'] = m('''    IF NEW.hold_ledger_transaction_id IS DISTINCT FROM OLD.hold_ledger_transaction_id
       AND NOT''', '''    IF false
       AND NOT''')
M['M10_wd023_off'] = m("IF v_edge = 'E3' AND NOT EXISTS (", "IF false AND NOT EXISTS (").replace("IF v_edge = 'E4' AND NOT EXISTS (", "IF false AND NOT EXISTS (")
# Check A
M['M11_A_session_shape_off'] = m('''    IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL
       OR COALESCE(NULLIF(current_setting('app.tenant_id', true), ''),
                   NULLIF(current_setting('app.acting_tenant_id', true), '')) IS DISTINCT FROM p_tenant::text THEN
        RETURN ARRAY['WD030', 'the session is not the tenant (or acting) scope of the request'];''', '''    IF false THEN
        RETURN ARRAY['WD030', 'the session is not the tenant (or acting) scope of the request'];''')
M['M12_A_wd031_off'] = m("        RETURN ARRAY['WD031', 'the request has no hold link'];", "        RETURN NULL;")
M['M13_A_hold_shape_off'] = m("        RETURN ARRAY['WD032', 'the hold transaction is not this request''s withdrawal_requested leg'];", "        NULL;")
M['M14_A_release_shape_off'] = m("            RETURN ARRAY['WD033', format('the release transaction is not the %s shape (type, key, provider, correlation or reversal link)', w.state)];", "            NULL;")
M['M15_A_wd034_off'] = m("    IF v_sum <> (CASE WHEN v_term THEN 0 ELSE w.amount END) THEN", "    IF false THEN")
M['M16_A_invisible_passes'] = m("        RETURN ARRAY['WD030', 'the request is not visible'];", "        RETURN NULL;")
M['M17_A_trigger_dropped'] = m('''CREATE CONSTRAINT TRIGGER withdrawal_requests_ledger_link_at_commit
    AFTER INSERT OR UPDATE ON withdrawal_requests
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_ledger_link_at_commit();''', '')
# B-i
M['M18_Bi_with_when_clause'] = m('''CREATE TRIGGER ledger_entries_withdrawal_hold_guard
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE''', '''CREATE TRIGGER ledger_entries_withdrawal_hold_guard
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW WHEN (NEW.amount > 0) EXECUTE''')
M['M19_Bi_invisible_request_passes'] = m('''        RAISE EXCEPTION 'ledger_entries: the withdrawal request named by a hold entry is not visible' USING ERRCODE = 'WD040';''', '''        RETURN NEW;''')
M['M20_Bi_type_check_off'] = m("    IF v_tx.transaction_type NOT IN ('withdrawal_requested', 'withdrawal_rejected', 'withdrawal_failed', 'withdrawal_completed') THEN", "    IF false THEN")
M['M21_Bi_key_pinning_off'] = m("AND ((v_tx.idempotency_key = v_id || ':cancelled' AND w.state = 'requested')", "AND ((w.state = 'requested')").replace("OR (v_tx.idempotency_key = v_id || ':rejected' AND w.state = 'pending_review')", "OR (w.state = 'pending_review')").replace("AND v_tx.idempotency_key = v_id || ':failed' AND w.state = 'submitted'", "AND w.state = 'submitted'")
M['M22_Bi_prestate_nonterminal_only'] = m("AND ((v_tx.idempotency_key = v_id || ':cancelled' AND w.state = 'requested')", "AND ((v_tx.idempotency_key = v_id || ':cancelled' AND w.state NOT IN ('rejected','cancelled','failed','completed','reversed'))").replace("OR (v_tx.idempotency_key = v_id || ':rejected' AND w.state = 'pending_review')", "OR (v_tx.idempotency_key = v_id || ':rejected' AND w.state NOT IN ('rejected','cancelled','failed','completed','reversed'))").replace("AND w.state = 'approved'))", "AND w.state NOT IN ('rejected','cancelled','failed','completed','reversed')))").replace("AND v_tx.idempotency_key = v_id || ':failed' AND w.state = 'submitted'", "AND v_tx.idempotency_key = v_id || ':failed' AND w.state NOT IN ('rejected','cancelled','failed','completed','reversed')")
M['M23_Bi_provider_equality_off'] = m('''                                         AND v_tx.provider_id = w.provider_id
                                         AND v_tx.idempotency_key = w.provider_id || ':' || v_tx.provider_tx_id''', '''                                         AND true''')
M['M24_Bi_release_not_null_check_off'] = m("OR w.amount IS DISTINCT FROM NEW.amount OR w.release_ledger_transaction_id IS NOT NULL THEN", "OR w.amount IS DISTINCT FROM NEW.amount THEN")
M['M25_Bi_type_blind_all_wallet_accounts'] = m("    IF v_type IS DISTINCT FROM 'player_withdrawal_hold' THEN\n        RETURN NEW;\n    END IF;\n\n    SELECT t.transaction_type", "    SELECT t.transaction_type")
# B-ii
M['M26_Bii_dropped'] = m('''CREATE CONSTRAINT TRIGGER ledger_entries_withdrawal_hold_link_at_commit
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_withdrawal_hold_link_at_commit();''', '')
M['M27_Bii_link_check_off'] = m('''    IF (NEW.direction = 'credit' AND w.hold_ledger_transaction_id IS DISTINCT FROM NEW.ledger_transaction_id)
       OR (NEW.direction = 'debit' AND w.release_ledger_transaction_id IS DISTINCT FROM NEW.ledger_transaction_id) THEN''', '''    IF false THEN''')

for k, v in M.items():
    assert v != base, k
    open(os.path.join(outdir, 'mutants', k + '.sql'), 'w').write(v)
print(len(M), 'mutants')
