#!/usr/bin/env python3
import unittest

import wirediff


def perf_lines(prefix, counts, skip=()):
    """perf stat -x, lines; prefix is "" for a total or "comm-tid," per thread."""
    return [f"{prefix}{counts.get(e, 0)},,{e}," for e in wirediff.EVENTS if e not in skip]


class ParsePerfTest(unittest.TestCase):
    def test_total_and_per_thread(self):
        total = wirediff.parse_perf(perf_lines("", {"cycles": 10, "syscalls:sys_enter_write": 3}))
        self.assertEqual(total["client"]["cycles"], 10)
        self.assertEqual(total["client"]["write"], 3)
        self.assertEqual(total["client"]["readv"], 0)  # a counted zero is a value
        tids = {11: "storage", 12: "storage", 21: "stateless"}
        lines = (perf_lines("fdbserver-11,", {"cycles": 5}) + perf_lines("fdbserver-12,", {"cycles": 7})
                 + perf_lines("fdbserver-21,", {"cycles": 1}))
        lines[0] = "fdbserver-11,<not counted>,,cycles,"  # a thread that did not run
        per = wirediff.parse_perf(lines, tids, ["stateless", "storage"])
        self.assertEqual(per["storage"]["cycles"], 7)  # threads of one role are summed
        self.assertEqual(per["stateless"]["cycles"], 1)

    def test_missing_or_uncounted_event_fails(self):
        with self.assertRaisesRegex(ValueError, "no count for write"):
            wirediff.parse_perf(perf_lines("", {}, skip=["syscalls:sys_enter_write"]))
        lines = perf_lines("", {})
        lines[0] = "<not supported>,,cycles,"
        with self.assertRaisesRegex(ValueError, "not supported"):
            wirediff.parse_perf(lines)
        with self.assertRaisesRegex(ValueError, "storage: no count"):
            wirediff.parse_perf(perf_lines("fdbserver-21,", {}), {21: "stateless"}, ["stateless", "storage"])

    def test_uncounted_total_or_role_fails(self):
        lines = perf_lines("", {})
        lines[0] = "<not counted>,,cycles,"
        with self.assertRaisesRegex(ValueError, "not counted"):
            wirediff.parse_perf(lines)  # a total is never an idle thread
        idle = [f"fdbserver-11,<not counted>,,{e}," for e in wirediff.EVENTS]
        with self.assertRaisesRegex(ValueError, "storage: no count for cycles"):
            wirediff.parse_perf(idle, {11: "storage"}, ["storage"])


class OperationsTest(unittest.TestCase):
    def test_counts_completed_operations_with_warmup(self):
        self.assertEqual(wirediff.operations({"completed": 15000, "warmup_completed": 750}), 15750)
        self.assertEqual(wirediff.operations({"completed": 999, "warmup_completed": 0}), 999)


class DifferencesTest(unittest.TestCase):
    def run_counts(self, cycles, events=None, ports=(4721,)):
        events = events or {"cycles": cycles, "write": 0}
        return {"counts": {"client": events}, "segments": {p: [1, 1, 1] for p in ports}}

    def test_long_minus_short(self):
        row = wirediff.differences(self.run_counts(10), self.run_counts(30), 10)
        self.assertEqual(row, {"client.cycles": 2.0})

    def test_mismatched_populations_fail(self):
        with self.assertRaisesRegex(ValueError, "events differ"):
            wirediff.differences(self.run_counts(0, {"cycles": 0}), self.run_counts(1), 1)
        with self.assertRaisesRegex(ValueError, "segments differ"):
            wirediff.differences(self.run_counts(0), self.run_counts(1, ports=(4721, 4723)), 1)


class TraceTest(unittest.TestCase):
    def event(self, t, kind, instance="0", **totals):
        fields = " ".join(f'{k}="1 0.5 {v}"' for k, v in totals.items())
        return f'<Event Time="{t}" Type="{kind}" ID="{instance}" {fields} />'

    def series(self, servers):
        out = {}
        for server, lines in servers.items():
            wirediff.trace_series(server, lines, out)
        return out

    def full(self, t, scale):
        names = {k: {n: scale * (i + 1) for i, n in enumerate(ns)} for k, ns in wirediff.COUNTERS.items()}
        return [self.event(t, k, **v) for k, v in names.items()]

    def test_deltas_are_summed_per_server(self):
        series = self.series({"a": self.full(10, 1) + self.full(42, 3), "b": self.full(12, 10) + self.full(41, 20)})
        deltas = wirediff.trace_deltas(series, start=20, end=40, cutoff=50)
        # a: 3 - 1 = 2 (x1 for the first counter); b: 20 - 10 = 10. Never b - a.
        self.assertEqual(deltas["GrvProxyMetrics.TxnRequestIn"], 12)

    def test_counter_without_both_sides_fails(self):
        cases = {
            "after start only": {"a": self.full(42, 3)},
            "stale, before start only": {"a": self.full(5, 1) + self.full(10, 2)},
            "logging stopped during the run": {"a": self.full(10, 1) + self.full(30, 3)},
            "one server stale": {"a": self.full(10, 1) + self.full(42, 3), "b": self.full(12, 10)},
            "sample only after the cutoff": {"a": self.full(10, 1) + self.full(60, 3)},
        }
        for name, servers in cases.items():
            with self.subTest(name), self.assertRaisesRegex(ValueError, "both sides"):
                wirediff.trace_deltas(self.series(servers), start=20, end=40, cutoff=50)

    def test_instances_of_one_process_are_separate_series(self):
        storage = lambda t, instance, q: self.event(t, "StorageMetrics", instance, QueryQueue=q, GetValueQueries=0, GetRangeQueries=0)
        lines = self.full(10, 1) + self.full(42, 3)
        lines += [storage(10, "s1", 100), storage(10, "s2", 5000), storage(42, "s1", 130), storage(42, "s2", 5040)]
        deltas = wirediff.trace_deltas(self.series({"a": lines}), start=20, end=40, cutoff=50)
        self.assertEqual(deltas["StorageMetrics.QueryQueue"], 2 + 30 + 40)  # never s2 - s1
        replaced = self.full(10, 1) + self.full(42, 3) + [storage(10, "old", 100), storage(42, "new", 7)]
        with self.assertRaisesRegex(ValueError, "both sides"):
            wirediff.trace_deltas(self.series({"a": replaced}), start=20, end=40, cutoff=50)

    def test_counter_no_server_reported_fails(self):
        series = self.series({"a": [self.event(10, "GrvProxyMetrics", TxnRequestIn=1), self.event(42, "GrvProxyMetrics", TxnRequestIn=2)]})
        with self.assertRaisesRegex(ValueError, "no trace totals"):
            wirediff.trace_deltas(series, start=20, end=40, cutoff=50)


class TraceFilesTest(unittest.TestCase):
    def test_files_selected_by_port(self):
        paths = [
            "/logs/trace.127.0.0.1.4721.1792148326.xG2qLf.1.72.xml",
            "/logs/trace.127.0.0.1.4722.1792148326.vp0WLA.1.15.xml",
            "/logs/trace.10.1.4721.9.4500.1792148326.ab12Cd.1.3.xml",  # 4721 only inside the address
            "/logs/trace.127.0.0.1.4721.1792148326.xG2qLf.1.72.json",
        ]
        self.assertEqual(wirediff.server_trace_files(paths, 4721), [paths[0]])
        self.assertEqual(wirediff.server_trace_files(paths, 4722), [paths[1]])
        self.assertEqual(wirediff.server_trace_files(paths, 4500), [paths[2]])


class RoleCoverageTest(unittest.TestCase):
    def test_every_storage_server_reports_storage_metrics(self):
        servers = [{"pid": "1", "role": "storage"}, {"pid": "2", "role": "storage"}, {"pid": "3", "role": "stateless"}]
        series = {}
        wirediff.trace_series("1", ['<Event Time="1" Type="StorageMetrics" ID="a" QueryQueue="1 0 5" />'], series)
        with self.assertRaisesRegex(ValueError, "server 2 .storage.: no StorageMetrics"):
            wirediff.check_role_coverage(servers, series)
        wirediff.trace_series("2", ['<Event Time="1" Type="StorageMetrics" ID="b" QueryQueue="1 0 7" />'], series)
        wirediff.check_role_coverage(servers, series)  # a stateless server owes no storage counters


class SocketCountersTest(unittest.TestCase):
    def test_clientbench_sockets_only(self):
        lines = [
            '0 0 127.0.0.1:5000 127.0.0.1:4721 users:(("clientbench_go",pid=1,fd=3))',
            "\t cubic bytes_sent:120 segs_out:4",
            '0 0 127.0.0.1:5001 127.0.0.1:4721 users:(("fdbserver",pid=2,fd=4))',
            "\t cubic bytes_sent:999 segs_out:99",
        ]
        store = {}
        wirediff.socket_counters(lines, {4721}, store)
        self.assertEqual(store, {("5000", 4721): (4, 120)})

    def test_counters_missing_fails(self):
        lines = ['0 0 127.0.0.1:5000 127.0.0.1:4721 users:(("clientbench_go",pid=1,fd=3))', "\t cubic"]
        with self.assertRaises(ValueError):
            wirediff.socket_counters(lines, {4721}, {})


if __name__ == "__main__":
    unittest.main()
