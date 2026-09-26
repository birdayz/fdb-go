#!/usr/bin/env python3
"""Reconcile completed Go test output blocks; never credit an unfinished target.

Usage: python3 reconcile-promotion.py /absolute/path/to/java-upgrade
Writes ws-a-promotion-verification.json beside this script. Logs remain external;
the artifact pins their hashes and records the interrupted race target separately.
"""
import collections
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys

root = Path(sys.argv[1])
inputs = [
    ("promotion-child-error-green-full.log", 2, True),
    ("promotion-affected-full.log", 7, True),
    ("promotion-race-full.log", 8, False),
]
# A label must not consume newlines under DOTALL. Match only complete blocks.
pattern = re.compile(
    r"^=+ Test output for ([^\n]+):\n(.*?)^={80}\s*$", re.M | re.S
)
runs = []
for filename, expected, complete in inputs:
    raw = (root / filename).read_bytes()
    blocks = pattern.findall(raw.decode())
    assert len(blocks) == expected, (filename, len(blocks), expected)
    targets = []
    for label, output in blocks:
        assert label.startswith("//") and len(label) < 256, label
        parsed = subprocess.run(
            ["go", "tool", "test2json", "-p", label],
            input=output, text=True, capture_output=True, check=True,
        )
        events = [json.loads(line) for line in parsed.stdout.splitlines()]
        counts = collections.Counter()
        started, ended = collections.Counter(), collections.Counter()
        package_end = []
        for event in events:
            action, name = event["Action"], event.get("Test")
            if name and action in ("run", "pass", "fail", "skip"):
                counts[action] += 1
                if action == "run":
                    started[name] += 1
                else:
                    ended[name] += 1
            elif not name and action in ("pass", "fail", "skip"):
                package_end.append(action)
        assert started and started == ended, (label, started - ended, ended - started)
        assert package_end == ["pass"], (label, package_end)
        assert counts["fail"] == 0, (label, counts)
        targets.append({"label": label, "counts": dict(counts), "package_result": "pass"})
    assert len({t["label"] for t in targets}) == expected
    runs.append({
        "log": str(root / filename),
        "sha256": hashlib.sha256(raw).hexdigest(),
        "command_completed": complete,
        "completed_targets": targets,
        "interrupted_target": None if complete else "//pkg/relational/sqldriver:sqldriver_test",
    })
result = {
    "go_parent": "e48f5b4965543cd4d99b5578356059e12d969c7c",
    "scope": "Historical promotion verification before the lazy LIMIT implementation; not a full upgrade or full race pass.",
    "parser": "go tool test2json; nonempty per-target RUN/terminal multisets reconciled",
    "runs": runs,
}
destination = Path(__file__).with_name("ws-a-promotion-verification.json")
destination.write_text(json.dumps(result, indent=2) + "\n")
print(destination)
for run in runs:
    total = collections.Counter()
    for target in run["completed_targets"]:
        total.update(target["counts"])
    print(Path(run["log"]).name, len(run["completed_targets"]), dict(total), "complete:", run["command_completed"])
