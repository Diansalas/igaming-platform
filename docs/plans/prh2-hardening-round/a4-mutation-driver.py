#!/usr/bin/env python3
"""A4 / MANUAL-ADJ-LINK-1: mutation pass over migration 0153 (kill-or-justify).

Usage (from the repo root, against a PRIVATE database migrated through 0153, never a shared one):
  go test -tags integration -c -o /tmp/a4_adj.test ./internal/adjustment/
  A4_OWNER_URL=postgres://<owner>@.../<db> A4_RUNTIME_URL=postgres://<runtime>@.../<db> A4_TEST_BIN=/tmp/a4_adj.test \
    python3 docs/plans/prh2-hardening-round/a4-mutation-driver.py [MUTANT_ID ...]


Each mutant is a textual change of 0153's up SQL (idempotent: CREATE OR REPLACE FUNCTION, DROP/CREATE TRIGGER). The
mutated file is applied to the PRIVATE a4 database as the owner, the prebuilt adjustment integration test binary runs
the behavioural fence tests (catalog/static pin tests are excluded: a kill must be behavioural), and the original file
is re-applied. Output: one line per mutant, KILLED/SURVIVED, plus the failing test names.
"""
import os
import re
import subprocess
import sys

import tempfile

WT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', '..'))
UP = WT + '/migrations/0153_manual_adjustment_link_fence.up.sql'
OWNER = os.environ['A4_OWNER_URL']
RUNTIME = os.environ['A4_RUNTIME_URL']
BIN = os.environ['A4_TEST_BIN']
JWT = os.environ.get('JWT_SIGNING_SECRET', 'ci-signing-secret-at-least-32-characters-long')
RUN = (r'^(TestLinkFence0153_(Governed|Concurrent|Partial|Compensating|Unlinked|Forged|Executing|Checks|Executed|OtherTypes|TenantIsolation)'
       r'|TestB12_|TestK2R1_|TestB25_|TestK2C1_|TestB1_)')

ORIG = open(UP).read()

TX_BEFORE_OLD = 'CREATE CONSTRAINT TRIGGER ledger_transactions_manual_adjustment_link\n    AFTER INSERT ON ledger_transactions\n    NOT DEFERRABLE INITIALLY IMMEDIATE\n'
TX_BEFORE_NEW = 'CREATE TRIGGER ledger_transactions_manual_adjustment_link\n    BEFORE INSERT ON ledger_transactions\n'

MUTANTS = [
    # --- transaction fence ---
    ('T01', 'tx: not-found arm off', [("    IF NOT FOUND THEN\n        RAISE EXCEPTION 'ledger_transactions: manual_adjustment % is not linked",
                                       "    IF false THEN\n        RAISE EXCEPTION 'ledger_transactions: manual_adjustment % is not linked")]),
    ('T02', 'tx: key arm off', [("IF NEW.idempotency_key IS DISTINCT FROM 'manual_adjustment:' || v_req.id::text THEN", "IF false THEN")]),
    ('T03', 'tx: state arm off', [("IF v_req.state <> 'executing' OR v_req.executed_txid", "IF false OR v_req.executed_txid")]),
    ('T04', 'tx: txid arm off', [("OR v_req.executed_txid IS DISTINCT FROM pg_catalog.txid_current() THEN", "OR false THEN")]),
    ('T05', 'tx: payload_hash arm off', [("IF v_req.payload_hash IS DISTINCT FROM public.ledger_adjustment_payload_hash(v_req) THEN", "IF false THEN")]),
    ('T06', 'tx: provider_id arm off', [("IF NEW.provider_id IS NOT NULL OR NEW.provider_tx_id", "IF false OR NEW.provider_tx_id")]),
    ('T07', 'tx: provider_tx_id arm off', [("OR NEW.provider_tx_id IS NOT NULL OR NEW.reverses", "OR false OR NEW.reverses")]),
    ('T06b', 'tx: provider_id AND provider_tx_id arms off', [("IF NEW.provider_id IS NOT NULL OR NEW.provider_tx_id IS NOT NULL OR NEW.reverses", "IF false OR false OR NEW.reverses")]),
    ('T08', 'tx: reverses arm off', [("OR NEW.reverses_transaction_id IS NOT NULL\n", "OR false\n")]),
    ('T09', 'tx: reason_code arm off', [("OR NEW.reason_code IS DISTINCT FROM v_req.reason_code\n", "OR false\n")]),
    ('T10', 'tx: causation arm off', [("OR NEW.causation_id IS DISTINCT FROM v_req.causation_transaction_id THEN", "OR false THEN")]),
    ('T11', 'tx: enabled arm off', [("IF NOT v_exec.enabled OR NOT v_exec.initiator_valid", "IF false OR NOT v_exec.initiator_valid")]),
    ('T12', 'tx: initiator arm off', [("OR NOT v_exec.initiator_valid OR v_exec.counted", "OR false OR v_exec.counted")]),
    ('T13', 'tx: count arm off', [("OR v_exec.counted < v_exec.required THEN", "OR false THEN")]),
    ('T14', 'tx: count < -> <=', [("v_exec.counted < v_exec.required", "v_exec.counted <= v_exec.required")]),
    ('T15', 'tx: count off by one (required - 1)', [("v_exec.counted < v_exec.required", "v_exec.counted < v_exec.required - 1")]),
    ('T16', 'tx trigger: WHEN removed (all types)', [("    WHEN (NEW.transaction_type = 'manual_adjustment')\n", "")]),
    ('T17', 'tx trigger: WHEN on another type', [("WHEN (NEW.transaction_type = 'manual_adjustment')", "WHEN (NEW.transaction_type = 'bonus_forfeiture')")]),
    ('T18', 'tx trigger: DEFERRABLE INITIALLY DEFERRED', [("AFTER INSERT ON ledger_transactions\n    NOT DEFERRABLE INITIALLY IMMEDIATE", "AFTER INSERT ON ledger_transactions\n    DEFERRABLE INITIALLY DEFERRED")]),
    ('T19', 'tx trigger: BEFORE INSERT (function returns NEW)', [(TX_BEFORE_OLD, TX_BEFORE_NEW),
        ("            USING ERRCODE = 'MA050';\n    END IF;\n    RETURN NULL;\nEND;", "            USING ERRCODE = 'MA050';\n    END IF;\n    RETURN NEW;\nEND;")]),
    ('T20', 'tx trigger: not created', [("CREATE CONSTRAINT TRIGGER ledger_transactions_manual_adjustment_link\n    AFTER INSERT ON ledger_transactions\n    NOT DEFERRABLE INITIALLY IMMEDIATE\n    FOR EACH ROW\n    WHEN (NEW.transaction_type = 'manual_adjustment')\n    EXECUTE FUNCTION ledger_manual_adjustment_link_check();", "")]),
    # --- entry fence ---
    ('E01', 'entry: parent not-found arm off', [("    IF NOT FOUND THEN\n        RAISE EXCEPTION 'ledger_entries: parent transaction", "    IF false THEN\n        RAISE EXCEPTION 'ledger_entries: parent transaction")]),
    ('E02', 'entry: type gate always returns', [("IF v_tx.transaction_type <> 'manual_adjustment' THEN", "IF true THEN")]),
    ('E03', 'entry: type gate never returns', [("IF v_tx.transaction_type <> 'manual_adjustment' THEN", "IF false THEN")]),
    ('E04', 'entry: request key condition off', [("       AND v_tx.idempotency_key = 'manual_adjustment:' || r.id::text\n", "       AND true\n")]),
    ('E05', 'entry: request state condition off', [("AND r.state = 'executing' AND r.executed_txid", "AND true AND r.executed_txid")]),
    ('E06', 'entry: request txid condition off', [("AND r.executed_txid = pg_catalog.txid_current();", "AND true;")]),
    ('E07', 'entry: request not-found arm off', [("    IF NOT FOUND THEN\n        RAISE EXCEPTION 'ledger_entries: manual_adjustment % has no request", "    IF false THEN\n        RAISE EXCEPTION 'ledger_entries: manual_adjustment % has no request")]),
    ('E08', 'entry: request tenant condition off', [("WHERE r.id = v_tx.correlation_id AND r.tenant_id = NEW.tenant_id", "WHERE r.id = v_tx.correlation_id")]),
    ('E09', 'entry: account not-found arm off', [("    IF NOT FOUND\n       OR NEW.asset_code", "    IF false\n       OR NEW.asset_code")]),
    ('E10', 'entry: asset arm off', [("OR NEW.asset_code IS DISTINCT FROM v_req.asset_code\n", "OR false\n")]),
    ('E11', 'entry: amount arm off', [("OR NEW.amount IS DISTINCT FROM v_req.amount\n", "OR false\n")]),
    ('E12', 'entry: player wallet arm off', [("AND v_acct.wallet_id = v_req.wallet_id AND NEW.direction = v_player_dir", "AND true AND NEW.direction = v_player_dir")]),
    ('E13', 'entry: player direction arm off', [("AND NEW.direction = v_player_dir)", "AND true)")]),
    ('E14', 'entry: house wallet IS NULL arm off', [("AND v_acct.wallet_id IS NULL AND NEW.direction = v_house_dir", "AND true AND NEW.direction = v_house_dir")]),
    ('E15', 'entry: house direction arm off', [("AND NEW.direction = v_house_dir), false)", "AND true), false)")]),
    ('E16', 'entry: house account type arm off', [("(v_acct.account_type = 'manual_adjustment' AND", "(true AND")]),
    ('E17', 'entry: player account type arm off', [("(v_acct.account_type = 'player_cash' AND", "(true AND")]),
    ('E18', 'entry: COALESCE(..., false) removed', [("OR NOT COALESCE((v_acct.account_type = 'player_cash'", "OR NOT ((v_acct.account_type = 'player_cash'"),
                                                    ("NEW.direction = v_house_dir), false) THEN", "NEW.direction = v_house_dir)) THEN")]),
    ('E19', 'entry: duplicate-leg arm off', [("AND e.direction = NEW.direction) > 1 THEN", "AND e.direction = NEW.direction) > 99 THEN")]),
    ('E20', 'entry: duplicate-leg arm counts both directions', [("AND e.direction = NEW.direction) > 1 THEN", ") > 1 THEN")]),
    ('E21', 'entry trigger: DEFERRABLE INITIALLY DEFERRED', [("AFTER INSERT ON ledger_entries\n    NOT DEFERRABLE INITIALLY IMMEDIATE", "AFTER INSERT ON ledger_entries\n    DEFERRABLE INITIALLY DEFERRED")]),
    ('E22', 'entry: player/house direction mapping swapped', [("v_player_dir := CASE WHEN v_req.direction = 'credit_player' THEN 'credit' ELSE 'debit' END;", "v_player_dir := CASE WHEN v_req.direction = 'credit_player' THEN 'debit' ELSE 'credit' END;")]),
    ('E24', 'entry: MA052 arm off (other types may use the manual_adjustment account)', [("        IF EXISTS (SELECT 1 FROM public.ledger_accounts la\n                    WHERE la.id = NEW.ledger_account_id AND la.account_type = 'manual_adjustment') THEN", "        IF false THEN")]),
    ('E25', 'entry: MA052 on another account type', [("WHERE la.id = NEW.ledger_account_id AND la.account_type = 'manual_adjustment') THEN", "WHERE la.id = NEW.ledger_account_id AND la.account_type = 'house_gaming') THEN")]),
    ('E26', 'entry: MA052 account-id condition off', [("WHERE la.id = NEW.ledger_account_id AND la.account_type = 'manual_adjustment') THEN", "WHERE la.account_type = 'manual_adjustment') THEN")]),
    ('E23', 'entry trigger: not created', [("CREATE CONSTRAINT TRIGGER ledger_entries_manual_adjustment_link\n    AFTER INSERT ON ledger_entries\n    NOT DEFERRABLE INITIALLY IMMEDIATE\n    FOR EACH ROW\n    EXECUTE FUNCTION ledger_manual_adjustment_entry_check();", "")]),
]


def apply_sql(text):
    path = os.path.join(tempfile.gettempdir(), 'a4_mut_apply.sql')
    with open(path, 'w') as fh:
        fh.write(text)
    r = subprocess.run(['psql', OWNER, '-q', '-v', 'ON_ERROR_STOP=1', '-f', path], capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(r.stderr)


def run_tests():
    env = dict(os.environ)
    env.update({'DATABASE_URL': OWNER, 'TEST_DATABASE_URL': OWNER, 'TEST_RUNTIME_DATABASE_URL': RUNTIME,
                'JWT_SIGNING_SECRET': JWT})
    r = subprocess.run([BIN, '-test.count=1', '-test.run', RUN, '-test.timeout', '600s'], cwd=WT + '/internal/adjustment',
                       env=env, capture_output=True, text=True)
    fails = sorted(set(re.findall(r'^--- FAIL: (\S+)', r.stdout, re.M)))
    return r.returncode, fails, r.stdout


def main():
    only = set(sys.argv[1:])
    rc, fails, out = run_tests()
    print('BASELINE rc=%d fails=%s' % (rc, fails), flush=True)
    if rc != 0:
        print(out[-3000:])
        sys.exit(1)
    try:
        for mid, desc, reps in MUTANTS:
            if only and mid not in only:
                continue
            text = ORIG
            for old, new in reps:
                n = text.count(old)
                if n != 1:
                    print('%s INVALID (pattern count %d): %r' % (mid, n, old[:60]), flush=True)
                    break
                text = text.replace(old, new)
            else:
                apply_sql(text)
                rc, fails, _ = run_tests()
                apply_sql(ORIG)
                status = 'KILLED' if rc != 0 else 'SURVIVED'
                print('%s %-8s %s | %s' % (mid, status, desc, ','.join(fails[:6])), flush=True)
    finally:
        apply_sql(ORIG)
    print('restored original 0153', flush=True)


if __name__ == '__main__':
    main()
