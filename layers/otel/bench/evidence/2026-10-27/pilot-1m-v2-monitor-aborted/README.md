# Monitor-terminated V2 one-million-span pilot

Source `692a42505133f7f42030bc9ba91c65f8d7828d14`, fixture/tool V2, batch saves,
batch 32, four workers, one tenant, eight indexed attributes plus the maintained
standalone-span and intrinsic-time indexes. Requested population: 1,000,000 spans.

This is a distinct scale profile: four FDB 7.3.77 processes, four CPUs aggregate,
16-GiB container cap, 4-GiB per-process RSS ceilings, two 2-GiB storage caches,
Redwood storage and SQLite transaction-log storage, single redundancy. It is
neither an engine-only comparison with the older one-process/4-GiB SQLite
receipts nor an HA deployment.

The external monitor stopped the cluster after observing client aggregate
`healthy=false`. The loader then reported error 1031 during store opening, with
143,264 acknowledged spans after 41.612 s. No complete source census, ready
population, final drain or query campaign was achieved. Do not treat this
partial population as successful throughput, a completed 1M gate or a DB crash.
Post-stop physical/container sampling was unavailable; absent values are not zero.

The preserved abort status shows why the monitor stopped:

- Client availability true, client aggregate health false; no client messages.
- The sole cluster message was `batch_priority_transaction_start_probe_timeout`
  (unable to start a batch-priority transaction within five seconds).
- Data explicitly healthy (`healthy_repartitioning`), recovery fully recovered,
  no degraded processes, expected role placement, unchanged process identities.
- Worst storage queue 1,025,994,869 bytes; worst durability lag 23.9035 s. This
  had not reached the separate queue-plus-60-second-lag abort conjunction.
- Storage RSS approximately 2.83/2.55 GB, below the unchanged per-process ceilings;
  no observed OOM or restart caused the stop.

In exact upstream source `3ea44ce1d9003ad095e408039e1f755c319c4dfb`,
`StatusClient.actor.cpp:460–496` makes any cluster message lower aggregate health.
`Status.actor.cpp:1371–1453` adds the batch-priority probe warning without marking
the database unavailable. `GrvProxyServer.actor.cpp:922–946` prioritizes system
and default traffic ahead of batch traffic. Therefore this is an externally
terminated strict-health arm with real backpressure, not proof that default
operations or data availability failed. SaveRecordBatch does not select FDB
batch transaction priority.

The snapshot's successful default/immediate GRV and read/commit probes are not
needle-query SLO evidence; the read/commit probes use system-immediate priority.
Also, Linux `memory.used_bytes` here is virtual address-space size, not RSS
(`Platform.actor.cpp:361–376`); do not use it as an RSS ceiling measurement.

Raw outputs and the abort snapshot are gzip-compressed without modifying their
bytes. Source, executable and server/monitor configuration hashes matched before
and after the arm. Original runtime session/raw samples remain local. The stop
is retained; any narrower, warning-aware bulk monitor is a new explicitly labeled
policy/arm, not a retry that turns this failed gate green. Initial readiness and
final drain must still require strict health, and structural/resource/backlog
checks must not be bypassed to reach 100M.
