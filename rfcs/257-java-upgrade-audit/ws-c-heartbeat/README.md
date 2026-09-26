# WS-C heartbeat prerequisite — implemented, milestone still open

Tracking: TODO.md, “WS-C design accepted; heartbeat prerequisite implemented”.
Design: ../ws-c-design.md, SHA256
`302335e70765e7ab84674aeb88233348fc5f53be7336f8a006bd8a9652aab5dc`.
All four actual design ACKs: ../ws-c-design-review-v3/.
These are DESIGN ACKs, not implementation or upgrade-wide acceptance.

## Correction

Go heartbeat writers used tuple strings; Java writes UUIDs and Go ignored them.
New writes/readers use tuple UUIDs. Builder admission rejects legacy/malformed
keys even in mutual mode. The strict Java future-skew boundary is preserved;
malformed values remain nonblocking but diagnostic listing returns Java's invalid
heartbeat placeholder instead of silently omitting them.

Three compiled real-FDB regression specs failed before the fix (heartbeat-red.log).
Afterward all 13 IndexingHeartbeat specs passed, including six exact-age boundary
cases, malformed/legacy key shapes in mutual/exclusive modes, malformed values,
own/peer behavior and cleanup. heartbeat-interop.log records the live JVM/FDB
bidirectional exclusion and both cleanup paths. A compiled mutation restoring
string-key writes was killed by the wire-key spec and restored.

Validation on the six source files in heartbeat-sources.sha256:
- `just test`: 93/93 targets pass, 43 executed and 50 cached.
- Actual rules_go race flag, uncached selected targets: 2/2 pass;
  recordlayer 13 selected specs; conformance 4 selected specs (heartbeat plus
  queue oracles). This is not the complete race suite or actual CI.
- Source hashes checked unchanged after full suite, mutation restoration and race.

No commit/push/merge/PR-state change. WS-C queue, shared maintenance gate, build,
drain, merger, closeout and terminal cleanup implementation remain open, as do
completed-milestone review and WS-D–K. Format15/state4 are not enabled here.

## Mandatory heartbeat deployment transition

This is NOT a transparent rolling upgrade of indexing workers. Old Go workers
ignore UUID keys; Java does not accept their string keys. Stop and fence all old
Go indexing workers against restart BEFORE starting upgraded/Java builders.
After quiescence, explicitly clear legacy heartbeat keys in each affected index
using administrative cleanup (`CleanupAllHeartbeats` clears all sessions for that
index and therefore must only run after quiescence). Confirm legacy/malformed
keys are absent, then admit UUID-only builders. The new preflight rejects such
keys regardless of their recorded age; it does not infer process death or auto-
migrate. No new dual writer can make a legacy reader honor UUID exclusion.

This repository change does not perform deployment, worker fencing or cleanup
against user data. Those operational actions require separate authorization.
