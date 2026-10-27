# Failed one-million-span capacity gate

Pinned source `290d0d7b7ff07fc214ed0b9faf95faa11e259985`, original `otelbench-v1`
fixture/layout, batch saves, maximum batch 32, four workers, eight indexed
attributes and one tenant. This is the historical one-process FDB setup capped
at 4 CPUs / 4 GiB, not the later proposed multiprocess/larger-memory profile.

The requested 1,000,000-span load failed after 412.312 s with:

```text
otel operation ingest batch starting 222432: record 9: existing record load: failed to read record range: transaction_timed_out (1031)
```

The receipt reports 6,950 acknowledged source batches / 222,400 acknowledged
spans. Those counters are not an authoritative persisted-population census:
other in-flight attempts can have ambiguous outcomes. The command did not
complete ready publication, source audit, settling observation, or query tests.
Do not report this as a successful 1M run or sustainable ingest capacity.

The post-failure FDB snapshot reported:

- Process RSS 4,249,853,952 bytes, near the 4-GiB container cap; the server's own
  configured memory ceiling was 8 GiB. That ceiling is not an allocator budget.
- Storage persistence queue 995,906,485 bytes, worst storage durability lag
  216.787 s, transaction-log queue 271,201,107 bytes.
- Whole-volume allocated bytes 2,842,005,504 before / 3,651,665,920 after.
- Client task-clock 52,582.28 ms over 412.321 s perf elapsed.

The aggregate source Run phase was 1,649.192 s across four workers, including
1,166.117 s in manifest reads. That phase includes implicit read-version/start
waiting; it does not isolate a slow manifest KV lookup or prove a client defect.
Snapshots and overlapping counters do not establish a causal explanation for
the timeout. No OOM/restart was observed in the separate near-failure resource
snapshot, but the old service was subsequently stopped deliberately with its
volume retained to avoid continued pressure on the shared host.

Compressed JSON/JSONL is byte-identical to the local raw outputs. The nonzero
status, perf counters, physical allocation samples, binary checksums and matching
source-manifest digests are retained. Full source manifests remain local. The
failure is preserved, not rerun to turn the gate green; no 100M load is established.
