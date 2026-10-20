# hcloud-rebench-20261014 as-run controller scripts

Historical, not for reuse. These are the controller-side scripts as they ran for
[`hcloud-rebench-20261014`](../../../../../docs/benchmarks/hcloud-rebench-20261014/README.md),
with only paths and addresses replaced by placeholders (`<vm-public-ip>`; see
[docs/perf/README.md](../../../../../docs/perf/README.md#paths-in-imported-documents)).
They are not fixed in place, because that would misrepresent what ran. The maintained
tooling is [`../../../singlenode`](../../../singlenode/README.md).

| Script | Ran for |
|---|---|
| `run-c2-d2.py`, `run-c3.py` | Executing the sealed C2+D2 and C3 phases over SSH. |
| `analyze_reconstructed.py` | Restoring the testing.B case labels after the runner's aggregate-CSV bug (the campaign README's shared limits). |

They pair with the campaign directory's own copies of the single-node tools, not with
the later in-tree versions: `analyze_reconstructed.py` imported the campaign's
`bench_runner.py` (sha256 `8960f14c30ed9f733150b4d1f7a92fc8ef29cf06d1d5800adfbcf3d8428a4fb0`),
whose `parse_output(text, job, repeats=3)` has no `gomaxprocs` argument.

## Known defects

- `analyze_reconstructed.py:127` calls `parse_output` without the keyword-only
  `gomaxprocs` the in-tree `bench_runner.py` requires, so it raises `TypeError`
  against the in-tree runner. Against its paired runner it is correct.
