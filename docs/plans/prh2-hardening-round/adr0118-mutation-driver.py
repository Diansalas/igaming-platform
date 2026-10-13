#!/usr/bin/env python3
"""ADR 0118 / migration 0136 mutation plan M1-M10 driver.

For each mutant: copy the worktree (no .git) to a scratch source dir, apply ONE textual mutation to the
0136 up/down file (asserting it applied exactly once), rebuild the private DB br0136_mut from that copy
(priv_db.sh), and run the ADR 0118 tests (launchgov TestBrandBinding_*, tenant TestMigration0136_*)
against it. Records PASS (= mutant survived) / FAIL (= killed) with the failing test names.
"""
import os
import re
import shutil
import subprocess
import sys

S = "/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad"
W = "/home/user/igaming-platform/.claude/worktrees/agent-ae8c0e445f569338b"
SRC = os.path.join(S, "br0136_mut_src")
UP = "migrations/0136_player_brand_binding_immutable.up.sql"
DOWN = "migrations/0136_player_brand_binding_immutable.down.sql"
DB = "br0136_mut"

WALLET_WHEN_BRAND = "       OR NEW.brand_id IS DISTINCT FROM OLD.brand_id\n       OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id\n       OR NEW.asset_code"
WALLET_BODY_BRAND = "    IF NEW.brand_id IS DISTINCT FROM OLD.brand_id THEN v_changed := v_changed || 'brand_id'::text; END IF;\n    IF NEW.player_account_id IS DISTINCT FROM OLD.player_account_id THEN v_changed := v_changed || 'player_account_id'::text; END IF;\n    IF NEW.asset_code"

MUTANTS = [
    ("M1", "drop brand_id from the wallets WHEN clause", UP,
     WALLET_WHEN_BRAND, WALLET_WHEN_BRAND.replace("       OR NEW.brand_id IS DISTINCT FROM OLD.brand_id\n", "")),
    ("M2", "drop brand_id from the wallets function body only", UP,
     WALLET_BODY_BRAND, WALLET_BODY_BRAND.replace("    IF NEW.brand_id IS DISTINCT FROM OLD.brand_id THEN v_changed := v_changed || 'brand_id'::text; END IF;\n", "")),
    ("M3", "drop the player_accounts trigger", UP, None, None),
    ("M4", "FOR EACH STATEMENT instead of FOR EACH ROW (wallets)", UP,
     "    BEFORE UPDATE ON wallets\n    FOR EACH ROW\n", "    BEFORE UPDATE ON wallets\n    FOR EACH STATEMENT\n"),
    ("M5", "AFTER instead of BEFORE (wallets)", UP,
     "    BEFORE UPDATE ON wallets\n", "    AFTER UPDATE ON wallets\n"),
    ("M6", "remove player_account_id from the wallets pin (WHEN and body)", UP, None, None),
    ("M7", "remove the kyc_verifications trigger", UP, None, None),
    ("M8", "delete pre-flight class 1", UP,
     "    SELECT count(*) INTO c_wallet_repointed\n", "    SELECT 0 INTO c_wallet_repointed; PERFORM count(*)\n"),
    ("M9", "down leaves a function behind", DOWN,
     "DROP FUNCTION wallets_enforce_identity_immutable();\n", ""),
    ("M10", "<> instead of IS DISTINCT FROM (all WHEN clauses and bodies)", UP, None, None),
]


def mutate(text, mid, old, new):
    if mid == "M6":
        a = "       OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id\n       OR NEW.asset_code"
        b = "    IF NEW.player_account_id IS DISTINCT FROM OLD.player_account_id THEN v_changed := v_changed || 'player_account_id'::text; END IF;\n    IF NEW.asset_code"
        assert text.count(a) == 1 and text.count(b) == 1, mid
        return text.replace(a, "       OR NEW.asset_code").replace(b, "    IF NEW.asset_code")
    if mid in ("M3", "M7"):
        tbl = "player_accounts" if mid == "M3" else "kyc_verifications"
        start = text.index("CREATE TRIGGER %s_identity_immutable\n" % tbl)
        stop = "EXECUTE FUNCTION %s_enforce_identity_immutable();\n" % tbl
        end = text.index(stop, start) + len(stop)
        text = text[:start] + text[end:]
        cs = text.index("COMMENT ON TRIGGER %s_identity_immutable ON %s IS\n" % (tbl, tbl))
        ce = text.index("';\n", cs) + 3
        return text[:cs] + text[ce:]
    if mid == "M10":
        sec = text.index("-- 2. The guard functions")
        head, tail = text[:sec], text[sec:]
        n = len(re.findall(r"NEW\.(\w+) IS DISTINCT FROM OLD\.(\w+)", tail))
        assert n == 13 + 13, (mid, n)
        return head + re.sub(r"NEW\.(\w+) IS DISTINCT FROM OLD\.(\w+)", r"NEW.\1 <> OLD.\2", tail)
    assert text.count(old) == 1, (mid, text.count(old))
    return text.replace(old, new)


def run(cmd, env=None, cwd=None):
    p = subprocess.run(cmd, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    return p.returncode, p.stdout


def main():
    only = set(sys.argv[1:])
    results = []
    for mid, desc, path, old, new in MUTANTS:
        if only and mid not in only:
            continue
        if os.path.exists(SRC):
            shutil.rmtree(SRC)
        shutil.copytree(W, SRC, ignore=shutil.ignore_patterns(".git"))
        f = os.path.join(SRC, path)
        with open(f) as fh:
            text = fh.read()
        with open(f, "w") as fh:
            fh.write(mutate(text, mid, old, new))
        env = dict(os.environ, PRIV_DB=DB, PRIV_SRC=SRC)
        rc, out = run(["bash", os.path.join(S, "priv_db.sh")], env=env)
        log = f"=== {mid}: {desc}\n--- rebuild rc={rc}\n{out[-1500:]}\n"
        if rc != 0:
            verdict = "KILLED (migration does not apply: priv_db rebuild failed)"
        else:
            rc1, o1 = run(["bash", os.path.join(S, "priv_test.sh"), "-tags", "integration", "-count=1", "-run", "TestBrandBinding_", "./internal/launchgov/"], env=env)
            rc2, o2 = run(["bash", os.path.join(S, "priv_test.sh"), "-tags", "integration", "-count=1", "-run", "TestMigration0136_", "./internal/tenant/"], env=env)
            fails = re.findall(r"--- FAIL: (\S+)", o1 + o2)
            log += f"--- launchgov rc={rc1}\n{o1[-3000:]}\n--- tenant rc={rc2}\n{o2[-3000:]}\n"
            if rc1 == 0 and rc2 == 0:
                verdict = "SURVIVED"
            else:
                verdict = "KILLED by " + ", ".join(sorted(set(fails)) or ["(build/run failure, see log)"])
        results.append(f"{mid} | {desc} | {verdict}")
        with open(os.path.join(S, "br0136_mut.log"), "a") as fh:
            fh.write(log + f">>> {mid}: {verdict}\n\n")
        print(f"{mid}: {verdict}", flush=True)
    with open(os.path.join(S, "br0136_mut_results.txt"), "a") as fh:
        fh.write("\n".join(results) + "\n")


if __name__ == "__main__":
    main()
