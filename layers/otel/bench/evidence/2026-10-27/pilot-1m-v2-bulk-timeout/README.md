# Failed V2 bounded-bulk one-million-span pilot

**FAILED load gate, not a completed 1M population.** Run `rw1m0284` requested
1,000,000 spans and acknowledged **114,720 rows in 47.583190245 seconds** before
`transaction_timed_out (1031)`. This was a **non-monitor** load failure: the monitor
did not terminate this arm. The acknowledged count is not an independent stored
source census, successful-load throughput, CPU-capacity result, production result
or competitive comparison. No needle queries ran.

Measured source was clean, frozen commit
`692a42505133f7f42030bc9ba91c65f8d7828d14` and its V2 reference executable, not the
later active working tree. Source, executable and server/policy before/after
manifests are preserved and byte-identical. The exact frozen policy code is in
`policy/frozen/` inside the bundle. The fixture used one tenant, eight indexed
attributes, batch saves of 32 and four workers, with standalone-span-ID and
intrinsic-time indexes. The four-process FDB 7.3.77 Redwood profile has four CPUs
aggregate, a 16-GiB container cap and 4-GiB per-process RSS ceilings; it is single
redundancy, not HA or an engine-only comparison with earlier profiles.

## Health and drain are not load completion

The frozen monitor prefix contains **23 records, including 14 status samples**,
through final drain completion epoch `1793118647` (2026-10-27 16:30:47 UTC).
One of those 14 bulk-status samples used the narrowly admitted sole
`batch_priority_transaction_start_probe_timeout` warning: **1/14**, not a permanent
health waiver. The original false aggregate health and raw fields remain intact.
The monitor's last pre-completion status is 16:30:44 UTC; the independent drain
sampler continues through 16:30:47. Later ongoing monitor tail is not part of this
frozen receipt. All 14 referenced monitor raws, all 17 drain raws and all 16
readiness raws are retained.

The final three strict drain samples, certificate, pinned phase, policy ACK and
completed marker are preserved; their recorded raw hashes match the included
bytes. Strict drain certifies only the retained health/queue/lag observations,
**not** load success or source population. The driver exits failed after draining.
A separate post-failure `--mode audit` invocation was attempted and explicitly
rejected the ownership manifest's **`loading`** state before row scanning;
`run/audit-rejected.*` preserves that rejection. Thus no successful source census
or ready population was obtained. Any root-cause receipt wording that no audit
was performed means no population audit completed; it does not erase this rejected
attempt.

The supplemental root-cause receipt preserves sampled backpressure and pinned
C++/Go timeout evidence. A sampled `storage_server_write_queue_size` limiter and
approximately 24.34 default-priority TPS limit are not a failed-transaction trace
or a precise bottleneck attribution. No raw diagnostic fields were rewritten.

## Contents, provenance and portability

`evidence.tar.gz` losslessly retains `run/`, `session/`, `policy/frozen/`,
`rootcause-analysis/`, and all three pure-file policy-validation arms:

- `validation/initial-red-217/`: **217 expectations, 213 matched, four failures**.
- `validation/intermediate-red-238/`: **238 expectations, 235 matched, three failures**.
  Its original local directory name contained `green`; the result was RED.
- `validation/final-file-validation-241/`: **241/241 matched**, including expected
  rejections. This is pure-file validation, not a successful workload arm.

Each validation arm includes its own source copies, harness, commands, fixtures,
inputs and outputs; the 15 retained original policy inputs are included separately.
Failed arms and outputs are not omitted. The archive contains a per-source path/hash
map and raw-file checksums; `archive-members.tsv.gz` enumerates every archived
member. `provenance.json`, `scan-report.json` and `SHA256SUMS` describe selection,
integrity and scanning. Raw JSON/JSONL/text and intentional whitespace in malformed
fixtures or performance output are retained only within lossless gzip compression.

Absolute `/tmp/` paths in original receipts, commands and four deliberate adversarial
symlink fixtures describe **local provenance**, not portable dependencies. Do not
execute extracted drivers/command receipts or follow fixture symlinks blindly.
Reproduction requires fresh, explicitly owned paths and the original approval,
instance/run, source/binary/config pinning and safety guards; this archive grants
no launch authorization. Docker-inspect/environment logs were excluded rather
than rewriting sensitive fields. No DB/server/service/build/test action was taken
to create this archive; only existing files were copied, inspected and compressed.
