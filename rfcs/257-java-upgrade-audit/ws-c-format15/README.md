# WS-C normal format-15 lifecycle acceptance

Java reference: 4.14.2.0 FormatVersion (maximum WRITE_ONLY_WITH_QUEUE=15,
default CACHEABLE_STATE=7). Go now admits explicit version 15 while retaining
its approved creation default 14. Version 16 remains rejected; an unpinned open
never downgrades an existing 15 header.

Evidence logs under `/var/tmp/fdb-upgrade-recovery/ws-c/`:

- `format15-red.log`: five new lifecycle specs all failed with
  UnsupportedFormatVersionError(version=15,max=14) before raising the ceiling.
- `format15-target.log`: all five specs and format-ceiling unit tests passed.
  They cover normal create/upgrade/open, mixed queued/ordinary builds with real
  post-preparation producer update/delete/insert transactions, unreadable-success
  resume, cancellation cleanup/resume, and blocked-build cleanup/unblock/resume.
- `format15-jvm.log`: live Java opens and writes a Go-created format-15 vector
  store, both engines read the other's record, persisted header remains 15.
- `format15-race.log`: 147 selected specs / 3522 plus format-version unit tests,
  actual race instrumentation, uncached, all green.
- `format15-jvm-race.log`: one selected spec / 1522, actual race instrumentation;
  one FORMAT15 evidence line, both-engine read/write and header assertions pass.
- `format15-full.log`: 93/93 targets, 42 executed / 51 cached, 692.545 seconds.
  Five source hashes unchanged after verification. `git diff --check` clean.

This completes the normal-opening implementation increment, not implementation
review acceptance or the overall migration. Deferred GuardiANN/Lucene backend
adapters are not proven by synchronous HNSW tests. The reviewed implementation
scope and gate verdicts belong in `../ws-c-implementation-review/`.
