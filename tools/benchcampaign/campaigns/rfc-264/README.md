# RFC-264 benchmark tools

`latency-bisect/` and `mapped-bisect/` are archived as-run bisect tooling and are
not maintained. The public copies parameterize paths and endpoints; the original
as-run scripts and inputs remain with the raw artifacts. Use those originals for
historical reanalysis.

New measurements use [`local/quick_local.py`](local/) with clientbench's duration
mode. Publishing these tools does not authorize a new measurement.

## Known archived-driver limitations

The archived `latency-bisect/timing.py` and `latency-bisect/v2/timing.py` drivers:

- Seal only repository inputs for a timing slot, not the cluster file or `fdbcli`.
- Do not create the validation-lock parent directory. A fresh `FDB_PERF_HOME`
  requires that directory to exist before acquiring the lock.
