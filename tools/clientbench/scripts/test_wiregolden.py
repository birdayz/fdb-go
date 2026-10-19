#!/usr/bin/env python3
import os
import stat
import subprocess
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def executable(path, text):
    with open(path, "w") as f:
        f.write(text)
    os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR)


class WireGoldenScriptTest(unittest.TestCase):
    # A failed build must stop the run before an older output is installed or
    # run, even when the output query still names one.
    def test_failed_build_installs_nothing(self):
        d = tempfile.mkdtemp()
        ran = os.path.join(d, "stale-ran")
        stale = os.path.join(d, "stale")
        executable(stale, f"#!/bin/sh\ntouch {ran}\n")
        fake = os.path.join(d, "bazel")
        executable(fake, f"#!/bin/sh\nif [ \"$1\" = build ]; then exit 1; fi\necho {stale}\n")
        cluster = os.path.join(d, "fdb.cluster")
        open(cluster, "w").write("x:y@127.0.0.1:1\n")
        out = os.path.join(d, "out")
        r = subprocess.run(
            ["bash", os.path.join(HERE, "wiregolden.sh"), out, cluster, "4731"],
            env={**os.environ, "BAZEL": fake}, capture_output=True, text=True)
        self.assertNotEqual(r.returncode, 0, r.stderr)
        self.assertIn("failed", r.stderr)
        self.assertEqual(os.listdir(out), [])
        self.assertFalse(os.path.exists(ran))


if __name__ == "__main__":
    unittest.main()
