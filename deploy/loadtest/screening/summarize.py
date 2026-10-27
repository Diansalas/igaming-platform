#!/usr/bin/env python3
"""Aggregate harness result JSON (results/<scenario>.<mode>.runN.json) into markdown tables.

usage: summarize.py <results-dir> <mode> <scenario> [<scenario> ...]
Every number is the MEDIAN over the runs of that run's own statistic; the spread columns are the min and max of that statistic across
the runs (not across individual calls). The harness never extrapolates in these tables; extrapolations appear only under keys whose
names say EXTRAPOLATED.
"""
import glob, json, statistics, sys

def runs(d, mode, sc):
    out = []
    for f in sorted(glob.glob(f"{d}/{sc}.{mode}.run*.json")):
        out.append(json.load(open(f))["result"])
    return out

def med(xs):
    return statistics.median(xs)

def fmt(x, unit="ms"):
    if x is None:
        return "-"
    if unit == "ms":
        return f"{x:,.0f}" if x >= 100 else f"{x:,.1f}"
    return f"{x:,.2f}"

def cell(vals):
    return f"{fmt(med(vals))} ({fmt(min(vals))}-{fmt(max(vals))})"

def fn_table(d, scs):
    rows = {}
    order = ["enqueue_due_100", "enqueue_due_1000", "claim_4_after_enqueue_100", "claim_4_after_enqueue_1000", "finish_clear", "gate_overdue_500",
             "gate_overdue_5000", "parked_summary_7d", "parked_overdue_500_first", "parked_overdue_5000_first", "parked_keyset_cycle_500"]
    print("| function (median ms; min-max across runs) | " + " | ".join(scs) + " |")
    print("|---|" + "---|" * len(scs))
    data = {sc: runs(d, "fn", sc) for sc in scs}
    classes = ["clear", "stale", "none", "legacy", "needs_officer", "held"]
    for c in classes:
        cells = []
        for sc in scs:
            rs = [r for r in data[sc] if c in r.get("gate_denies_by_class", {})]
            cells.append(cell([r["gate_denies_by_class"][c]["latency"]["median_ms"] for r in rs]) if rs else "-")
        print(f"| gate_denies, one {c} person | " + " | ".join(cells) + " |")
    for k in order:
        cells = []
        for sc in scs:
            rs = [r for r in data[sc] if k in r]
            if not rs:
                cells.append("-")
            elif k == "parked_keyset_cycle_500":
                cells.append(cell([r[k]["latency"]["median_ms"] for r in rs]) + f" /batch; cycle {fmt(med([r[k]['notes']['cycle_seconds'] for r in rs]),'s')} s, {rs[0][k]['notes']['batches']} batches, complete={rs[0][k]['notes']['cycle_complete']}")
            else:
                cells.append(cell([r[k]["latency"]["median_ms"] for r in rs]))
        print(f"| {k} | " + " | ".join(cells) + " |")

def growth_table(d, scs):
    keys = ["since_at_oldest(all_versions_later)", "since_at_mid", "since_at_recent_3d", "covers", "gate_denies_clear", "gate_denies_stale", "gate_denies_none", "enqueue_due_100", "gate_overdue_500"]
    data = {sc: runs(d, "growth", sc) for sc in scs}
    print("| statement (median ms; min-max across runs) | " + " | ".join(scs) + " |")
    print("|---|" + "---|" * len(scs))
    for k in keys:
        cells = []
        for sc in scs:
            rs = [r for r in data[sc] if k in r]
            cells.append(cell([r[k]["latency"]["median_ms"] for r in rs]) if rs else "-")
        print(f"| {k} | " + " | ".join(cells) + " |")

if __name__ == "__main__":
    d, mode, scs = sys.argv[1], sys.argv[2], sys.argv[3:]
    {"fn": fn_table, "growth": growth_table}[mode](d, scs)
