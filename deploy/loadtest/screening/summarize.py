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

def runpass_table(d, scs):
    print("| scenario | passes complete / capped (per run) | pass duration s, complete passes (median; min-max across runs) | tenants started per pass (median) | claims | claims/h/tenant measured (min-max) | errors | max pool conns acquired | OS cpu busy % | buffer hit |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    for sc in scs:
        rs = runs(d, "runpass", sc)
        if not rs:
            continue
        comp = [r["passes_complete"] for r in rs]
        capd = [r["passes_truncated_by_cap"] for r in rs]
        durs = [r["pass_duration_complete"]["median_ms"] / 1000 for r in rs if r["pass_duration_complete"]["n"]]
        dur = f"{med(durs):.1f} ({min(durs):.1f}-{max(durs):.1f})" if durs else "none completed (capped)"
        started = [p["tenants_started"] for r in rs for w in r["passes"] for p in w]
        rate = [r["claims_per_hour_per_tenant_measured"] for r in rs]
        claims = [r["claims_total"] for r in rs]
        errs = [r["errors"] for r in rs]
        pool = [r["pool"]["max_acquired_sampled_100ms"] for r in rs]
        cpu = [100 - r["probe"]["os_cpu_pct"].get("idle", 0) for r in rs]
        hit = [r["probe"]["shared_buffers_hit_ratio"] for r in rs]
        print(f"| {sc} | {med(comp)} / {med(capd)} | {dur} | {med(started):.0f} of {rs[0]['tenants']} | {med(claims):.0f} | {med(rate):.0f} ({min(rate):.0f}-{max(rate):.0f}) | {med(errs):.0f} | {max(pool)} | {med(cpu):.0f} | {med(hit):.4f} |")

def stale_table(d, scs):
    print("| scenario | backlog denied after event | worker cfg | done in window | drain rate /min (min-max) | pass duration s (median; max) | denied at end | time to clear | gate caller median / p95 / max ms | gate denied share | errors |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for sc in scs:
        rs = runs(d, "stale", sc)
        if not rs:
            continue
        rate = [r["drain_rate_per_minute_measured"] for r in rs]
        pd_ = [r["pass_duration"]["median_ms"] / 1000 for r in rs]
        pm = [r["pass_duration"]["max_ms"] / 1000 for r in rs]
        cfg = rs[0]["worker_cfg"]
        clr = []
        for r in rs:
            if "time_to_clear_s_measured" in r:
                clr.append(f"{r['time_to_clear_s_measured']:.0f} s measured")
            elif "time_to_clear_s_EXTRAPOLATED_linear" in r:
                clr.append(f"{r['time_to_clear_s_EXTRAPOLATED_linear']/3600:.1f} h EXTRAPOLATED")
        g = [r["gate_callers"] for r in rs if r.get("gate_callers")]
        if g:
            gm = f"{med([x['Gate']['median_ms'] for x in g]):.1f} / {med([x['Gate']['p95_ms'] for x in g]):.1f} / {max(x['Gate']['max_ms'] for x in g):.0f}"
            share = f"{med([x['GateDenied']/max(1,x['GateCalls']) for x in g]):.2f}"
        else:
            gm, share = "-", "-"
        errs = sum(p["stats"]["Errors"] for r in rs for p in r["passes"])
        print(f"| {sc} | {med([r['backlog_denied_after_event'] for r in rs]):.0f} | batch {cfg['claim_batch']}, lease {cfg['lease_s']:.0f}s | {med([r['done_new'] for r in rs]):.0f} | {med(rate):.1f} ({min(rate):.1f}-{max(rate):.1f}) | {med(pd_):.1f}; {max(pm):.1f} | {med([r['denied_final_exact'] for r in rs]):.0f} | {', '.join(clr)} | {gm} | {share} | {errs} (3 runs) |")

if __name__ == "__main__":
    d, mode, scs = sys.argv[1], sys.argv[2], sys.argv[3:]
    {"fn": fn_table, "growth": growth_table, "runpass": runpass_table, "stale": stale_table}[mode](d, scs)
