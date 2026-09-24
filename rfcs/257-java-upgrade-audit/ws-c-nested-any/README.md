# WS-C implementation finding: nested Any compatibility

Addresses the nested-payload finding in the NAK of tree
`2a4d33046f8640673643ea989709665f425cbbc5`; no final-tree ACK or CI claim.

Java 4.14.2.0 uses `Any.unpack` in `VectorIndexMaintainer.updateFromQueue`,
`SlidingWindowIndexMaintainer.updateFromQueue`, and
`IndexingPendingWriteQueue.handleOneItem` (DELETE_WHERE). Its type URL must contain
a slash immediately before the message's full name. Go's `Any.UnmarshalTo`
accepts a slashless full name as well.

The outer envelope and consumed nested payload boundaries now share
`pendingQueueAnyHasType`. `unmarshalPendingQueueAny` performs this validation
before decoding vector, sliding, and DELETE_WHERE data. Sliding's delegated
payloads pass through the delegate maintainer's same boundary when consumed;
this is not an eager validation of payloads Java never consumes.

## Evidence

Raw logs: `/var/tmp/fdb-upgrade-recovery/ws-c/`.

* `nested-any-target.log`: 20 real-FDB replay specs, covering vector, sliding,
  delegated insert, delegated delete, and DELETE_WHERE. At each boundary: bare
  message name rejected; default prefix, custom prefix and leading slash accepted.
  Rejected replay transactions leave the queued entry and counter intact and do
  not commit index changes; accepted entries apply and are removed.
* `nested-any-jvm.log`: 1/1523 live JVM specs, **12 asserted NESTED-ANY lines**.
  The Java helper invokes the actual generated classes' `Any.unpack` for the
  three consumed message types and four URL spellings. This establishes Java
  boundary semantics, not an additional Java full-replay test.
* `nested-any-mutation.log`: replacing the nested decoder with permissive Go
  `Any.UnmarshalTo` compiled and ran all 20 cases: **5 failed/15 passed**. Each
  slashless boundary failed. The mutation was verified present and restored.
* `nested-any-full.log`: `just test`, **93/93**, **43 executed/50 cached**,
  674.997 seconds.
* `nested-any-race.log`: actual `--@rules_go//go/config:race`, uncached,
  **40/3594 Ginkgo specs**, passing. Focus: nested compatibility, index queue
  application, maintainer queue, PendingWritesQueue. Not an all-suite race claim.
* Six edited-source SHA256 values checked unchanged after full and race runs:
  `nested-any-source.sha256`.

Mutual-renewal contention and multi-target follow-up liveness remain open.
No commit, push, publication, CI trigger, or merge performed.
