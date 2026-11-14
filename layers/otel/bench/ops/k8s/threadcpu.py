#!/usr/bin/env python3
"""Per-thread CPU sampler for load-generator processes (runs on the client host next to them).

usage: threadcpu.py OUT.json PID [PID ...]
Reads /proc/<pid>/task/<tid>/stat once per second until every pid has exited.  The steady-state window drops the first 5 s and the last 3 s.
Output: per pid, the busiest threads (name, mean cores over the window, highest 1 s interval) and the sum over all threads.
"""
import json
import os
import sys
import time

CLK = os.sysconf("SC_CLK_TCK")
HEAD_S = float(os.environ.get("THREADCPU_HEAD_S", "5"))      # steady-state window: drop the first HEAD_S and the last TAIL_SAMPLES samples
TAIL_SAMPLES = int(os.environ.get("THREADCPU_TAIL_SAMPLES", "4"))


def snap(pid):
    out = {}
    try:
        for tid in os.listdir("/proc/%d/task" % pid):
            try:
                with open("/proc/%d/task/%s/stat" % (pid, tid)) as f:
                    raw = f.read()
            except OSError:
                continue
            comm = raw[raw.index("(") + 1:raw.rindex(")")]
            rest = raw[raw.rindex(")") + 2:].split()
            if tid == str(pid) and rest[0] in ("Z", "X"):          # the leader has exited and is only waiting to be reaped
                return None
            out[tid] = (comm, int(rest[11]) + int(rest[12]))      # utime + stime in ticks
    except OSError:
        return None
    return out


def main():
    out_path, pids = sys.argv[1], [int(p) for p in sys.argv[2:]]
    series = {p: [] for p in pids}
    alive = set(pids)
    t_start = time.monotonic()
    while alive:
        now = time.monotonic() - t_start
        for p in list(alive):
            s = snap(p)
            if s is None:
                alive.discard(p)
            else:
                series[p].append((now, s))
        time.sleep(1.0)
    res = {"clk_tck": CLK, "processes": []}
    for p in pids:
        ser = series[p]
        ts = [t for t, _ in ser]
        lo = next((i for i, t in enumerate(ts) if t >= HEAD_S), 0)
        hi = max(lo + 1, len(ts) - TAIL_SAMPLES)
        win = ser[lo:hi + 1] if len(ser) > lo + 1 else ser
        rec = {"pid": p, "samples": len(ser), "window_s": round(win[-1][0] - win[0][0], 1) if len(win) > 1 else 0, "threads": []}
        if len(win) > 1:
            tot_mean = 0.0
            rows = []
            for tid in sorted({t for _, snap_ in win for t in snap_}):
                seen = [(t, snap_[tid]) for t, snap_ in win if tid in snap_]       # a thread that exits inside the window is measured over its own life
                comm, base = seen[0][1]
                t_first, t_last, last = seen[0][0], seen[-1][0], seen[-1][1][1]
                span = max(t_last - t_first, win[-1][0] - win[0][0] if len(seen) == len(win) else 0.0)
                if span <= 0:
                    continue
                mean = (last - base) / CLK / span
                peak = 0.0
                for (t0, a_), (t1, b_) in zip(win, win[1:]):
                    if tid in a_ and tid in b_ and t1 > t0:
                        peak = max(peak, (b_[tid][1] - a_[tid][1]) / CLK / (t1 - t0))
                rows.append({"tid": tid, "name": comm, "cores_mean": round(mean, 3), "cores_peak_1s": round(peak, 3)})
                tot_mean += (last - base) / CLK / (win[-1][0] - win[0][0])
            rows.sort(key=lambda r: -r["cores_mean"])
            rec["threads"] = rows[:8]
            rec["thread_count"] = len(rows)
            rec["process_cores_mean"] = round(tot_mean, 3)
        res["processes"].append(rec)
    with open(out_path, "w") as f:
        json.dump(res, f)


if __name__ == "__main__":
    main()
