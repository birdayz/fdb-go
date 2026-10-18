#!/usr/bin/env python3
import os
import tempfile
import unittest

import gcalign

HEADER = "kind,client,segment,sent_ns,reply_ns,version,txn_count,error,malformed,reply_bytes,more,params\n"
START = 1_000_000_000_000


def commit(sent_ms, version, error=0, malformed=False, replied=True):
    sent = START + int(sent_ms * 1e6)
    reply = sent + 5_000_000 if replied else 0
    return f"commit,c,1,{sent},{reply},{version},1,{error},{str(malformed).lower()},0,false,\n"


class AlignTest(unittest.TestCase):
    def run_dir(self, rows, gcs):
        d = tempfile.mkdtemp()
        with open(os.path.join(d, "start_ns"), "w") as f:
            f.write(str(START))
        with open(os.path.join(d, "clientbench.log"), "w") as f:
            for i, at in enumerate(gcs):
                f.write(f"gc {i + 1} @{at:.3f}s 1%: 0.1+0.2+0.1 ms clock, 4->4->2 MB, 8 P\n")
        with open(os.path.join(d, "events.csv"), "w") as f:
            f.write(HEADER + "".join(rows))
        return gcalign.analyze(d, 2)

    def test_first_split_after_gc_excludes_failed_commits(self):
        rows = [commit(1, 7),  # a setup batch before the first whole one
                commit(10, 1), commit(10.1, 1), commit(20, 2), commit(20.2, 2),
                commit(30, 3), commit(31, 4),  # the first split: two batches of one
                commit(30.5, 0, malformed=True),  # malformed reply, version 0
                commit(30.6, -1, error=1020),  # in-band conflict
                commit(30.7, 9, replied=False)]  # never answered
        out = self.run_dir(rows, gcs=[0.005, 0.015, 0.027])
        self.assertEqual([b[1] for b in out["batches"]], [1, 2, 2, 1, 1])
        self.assertEqual(out["first_split"]["index"], 3)
        self.assertEqual(out["first_split"]["last_gc_ms_before"], 3.0)
        self.assertEqual(out["first_split"]["gcs_in_previous_round"], 1)
        # Counted from the first whole batch (10 ms): the GC at 5 ms is in setup.
        self.assertEqual(out["first_split"]["gcs_during_whole_batches"], 1)
        self.assertEqual(out["gc_start_ms"], [5.0, 15.0, 27.0])

    def test_tied_batches_ordered_by_version(self):
        # Same first-request time, encountered in reverse version order.
        rows = [commit(10, 2), commit(10, 1), commit(10, 1), commit(10, 3), commit(10, 3)]
        out = self.run_dir(rows, gcs=[])
        self.assertEqual([b[1] for b in out["batches"]], [2, 1, 2])
        self.assertEqual(out["first_split"]["index"], 1)

    def test_events_without_malformed_column_fail(self):
        d = tempfile.mkdtemp()
        for name, text in (("start_ns", str(START)), ("clientbench.log", ""),
                           ("events.csv", "kind,client,segment,sent_ns,reply_ns,version,txn_count,error\n"
                                          f"commit,c,1,{START},{START + 1},1,1,0\n")):
            with open(os.path.join(d, name), "w") as f:
                f.write(text)
        with self.assertRaises(KeyError):
            gcalign.analyze(d, 2)


if __name__ == "__main__":
    unittest.main()
