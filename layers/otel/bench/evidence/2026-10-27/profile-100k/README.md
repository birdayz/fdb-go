# Reproduced 100k ingestion baseline

Source `12c293130597477035e32b372b3c4101dbd9c2fb`, preserved executable checksum
`82de9f85776e69054dde400d2aad94d68e3d62fec34b8a3de421c7b0f366a095`.
The source worktree and executable were frozen independently of later development.
Same pinned database images and 4-CPU/4-GiB limits as [the environment receipt](../environment.txt),
with services recovered on their retained volumes. This is the original v1
fixture/layout: 100,000 spans, one tenant, eight indexed attributes, pipelined
saves, batch 32 and one transaction at a time.

- Ingest command completed in 199.853 s with 3,125 acknowledged source batches.
  No separate source audit was run for this profiling-only namespace.
- Client perf task-clock: 30,319.16 ms over 199.863 s elapsed; user 21.310 s,
  system 6.948 s. These are client-process counters, not server CPU or an
  attribution of the remaining wall time to one particular component.
- Whole-volume allocated bytes: 1,048,928,256 before / 1,394,962,432 after.
  The 346,034,176-byte increase is a sampled physical allocation delta with other
  owned data retained, not a steady-state physical capacity factor.
- FDB status snapshots reported storage queue 1,998 / 356,803,204 bytes and
  worst storage durability lag 5.001 / 54.485 s. Commit acknowledgement does not
  imply that storage queues have drained; durable transaction logs and storage
  persistence are different stages.

The archived JSON/JSONL is gzip-compressed without modifying the original bytes.
Native perf output and binary checksums are retained; perf's header uses host
local time, while JSON start timestamps are UTC. Source-manifest digests match;
full manifests remain in the local runtime directory. This diagnostic motivates
commit/read/storage profiling, not a Go client correctness defect, sustainable
capacity claim, or 100M-scale result.
