# WS-C versionstamped split-writer prerequisite

Tracking: TODO.md, “WS-C versionstamped split writer”.
Design authority: ../ws-c-design.md and actual design ACKs in
../ws-c-design-review-v3/. This is not completed-milestone acceptance.

The existing split writer now accepts a record context for incomplete tuple keys,
registers SET_VERSIONSTAMPED_KEY chunks rather than packing incomplete keys with
ordinary tuple packing, and skips old-key clears for those incomplete keys as
Java does. Ordinary record keys retain the concatenated unsplit packing fast
path. Metadata callers supply no record context because their keys are complete.

AddVersionMutation returns the replaced value (including non-nil empty for a
previous empty value), allowing the shared split writer to report Java's
RecordCoreInternalException-equivalent “Key with version overwritten”. Nil size
metrics are supported; supplied metrics are reset, with versionstamp offset
bytes counted as Java counts them. Previous-size metadata is consumed before
resetting an aliased output. Context range clears cancel pending chunks.

Retained real-FDB tests cover empty/one-byte/exact-100KB/multi-chunk values,
same-transaction local ordering, resolved stamps after commit, precommit
invisibility, size metrics including offset trailer, duplicate keys including
empty payloads, missing context, multiple incomplete stamps, cancellation before
commit, and reused previous/output metrics. Focused split/version-mutation run:
54 selected specs passing. After the final complete-key fast-path preservation:
- just test: 93/93 targets pass, 42 executed / 51 cached.
- actual rules_go race, uncached selected recordlayer target: 67 specs pass
  (split/version-mutation and heartbeat selections).
- compiled omission of duplicate-key rejection: one selected spec fails; restored.
- eight source hashes checked unchanged after full suite, mutation and race.

Logs and hashes are adjacent. Full queue envelope/cursor implementation,
maintainer replay and indexer session lifecycle remain open. No state4/format15
enabling, implementation-gate approval, actual CI or publication is claimed.
