#!/usr/bin/env python3
import json
from pathlib import Path
p = Path('/opt/fdbperf/artifacts/old-batch-r3-instruments')
caveat = 'UNRESOLVED ACCOUNTING: perf task-clock old6.197840s/CGo5.467520s exceeds whole GNU-time CPU old4.49s/CGo4.17s; measured-phase getrusage is3.924618s/3.584470s. Phase scope alone does not explain task-clock versus whole GNU time. Do not treat task-clock as interchangeable CPU accounting or use instrumented timings for performance claims. See ../old-batch-r3-build/R3-SCOPE.md.'
f = p / 'INSTRUMENTS.md'
s = f.read_text()
assert 'UNRESOLVED ACCOUNTING' not in s
marker = '\n## perf stat\n'
assert s.count(marker) == 1
f.write_text(s.replace(marker, '\n> ' + caveat + '\n' + marker))
f = p / 'instruments.json'
a = json.loads(f.read_text())
assert 'accounting_caveat' not in a
a['accounting_caveat'] = caveat
f.write_text(json.dumps(a, indent=2) + '\n')
