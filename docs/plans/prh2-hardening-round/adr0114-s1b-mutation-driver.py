#!/usr/bin/env python3
"""ADR 0114 S1b mutation driver. Usage: PRIV_DB=<db> PRIV_SRC=<worktree> python3 -I driver.py <priv_test.sh> <out.txt>

Applies one textual mutation at a time to the S1b alert-list code, runs the S1b tests (integration tag, private
database), records KILLED / SURVIVED, and ALWAYS restores the file."""
import os, subprocess, sys

SRC = os.environ["PRIV_SRC"]
H = os.path.join(SRC, "internal/httpserver/alert_routing_handlers.go")
C = os.path.join(SRC, "internal/httpserver/alert_list_cursor.go")
RUN = sys.argv[1]
OUT = sys.argv[2]

MUTS = [
    ("M1 keyset strictness < becomes <= (boundary row repeats)", H, "(a.first_seen_at, a.id) < (", "(a.first_seen_at, a.id) <= ("),
    ("M2 tie-break dropped from ORDER BY", H, "ORDER BY a.first_seen_at DESC, a.id DESC\n\t\t\t\tLIMIT $4", "ORDER BY a.first_seen_at DESC\n\t\t\t\tLIMIT $4"),
    ("M3 next page when len(out) >= limit (phantom empty page)", H, "if len(out) > limit {", "if len(out) >= limit {"),
    ("M4 cursor not bound to its filter", C, "w.V != alertCursorVersion || w.F != filter ||", "w.V != alertCursorVersion ||"),
    ("M5 lower time bound removed", C, "|| w.T < alertCursorMinMicros", ""),
    ("M6 upper time bound removed", C, "|| w.T > alertCursorMaxMicros", ""),
    ("M7 acting ids shown under the tenant scope marker", H, "platform := staffScopePlatform\n\treturn v.optRef", "platform := staffScopeTenant\n\treturn v.optRef"),
    ("M8 kind filter ignored", H, " AND ($3 = '' OR a.kind = $3)", ""),
    ("M9 canonical uuid check removed", C, "err != nil || id.String() != w.I", "err != nil"),
    ("M10 fetch limit instead of limit+1 (never a cursor)", H, "args := []any{state, sev, kind, limit + 1}", "args := []any{state, sev, kind, limit}"),
    ("M11 unknown cursor fields allowed", C, "dec.DisallowUnknownFields()", ""),
    ("M12 repeated cursor parameters accepted", H, "if len(raw) != 1 {", "if len(raw) < 1 {"),
    ("M13 cursor version unchecked", C, "w.V != alertCursorVersion || ", ""),
    ("M14 unknown kind accepted", H, "if _, known := alerting.Def(alerting.Kind(kind)); !known {", "if _, known := alerting.Def(alerting.Kind(kind)); !known && false {"),
    ("M15 next cursor taken from the first row, not the last", H, "last := out[len(out)-1]", "last := out[0]"),
    ("M16 cursor time truncated to milliseconds", C, "T: firstSeen.UnixMicro()", "T: firstSeen.UnixMilli() * 1000"),
    ("M17 single read ignores the id", H, "alertListSelect+` AND a.id = $1`, id)", "alertListSelect+` AND ($1::uuid IS NOT NULL)`, id)"),
]

ONLY = os.environ.get("ONLY")  # e.g. ONLY="M3 " re-runs just the mutants whose name starts with it
if ONLY:
    MUTS = [m for m in MUTS if m[0].startswith(ONLY)]

def run():
    p = subprocess.run(["bash", RUN, "-tags", "integration", "./internal/httpserver", "-count=1", "-timeout", "20m",
                        "-run", "TestAlertListS1b|TestAlertCursor|TestAlertActorRefs"], capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr)

lines = []
rc, _ = run()
lines.append("baseline: %s" % ("PASS" if rc == 0 else "FAIL"))
if rc != 0:
    open(OUT, "w").write("\n".join(lines) + "\n"); sys.exit(1)
for name, path, old, new in MUTS:
    orig = open(path).read()
    if orig.count(old) != 1:
        lines.append("%s: SKIPPED (pattern count %d)" % (name, orig.count(old))); continue
    try:
        open(path, "w").write(orig.replace(old, new))
        rc, out = run()
        failing = sorted({l.split()[2] for l in out.splitlines() if l.startswith("--- FAIL")})
        if "build failed" in out or "[build failed]" in out:
            verdict = "BUILD-FAILED"
        else:
            verdict = "KILLED" if rc != 0 else "SURVIVED"
        lines.append("%s: %s %s" % (name, verdict, ",".join(failing)))
    finally:
        open(path, "w").write(orig)
open(OUT, "w").write("\n".join(lines) + "\n")
print("\n".join(lines))
