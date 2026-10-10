# Upgrading and rolling back

**Pin both the old and new application revisions.** Read their changelogs and
[compatibility boundaries](compatibility.md) before opening existing data. This
pre-1.0 project does not promise an automatic upgrade or downgrade between every
pair of Go revisions. The current readiness statement is [STATUS.md](../STATUS.md).

## From v0.1.0 SQL storage

The unreleased tree uses Java's relational keyspace: the catalog store is
`(NULL, NULL, 0)` and schema stores are `(domain, database, schema)`, with directory
and interning layers resolving those names. Earlier Go SQL builds used a different
layout. **The old layout is not automatically migrated.** See the breaking-storage
entry in [CHANGELOG.md](../CHANGELOG.md).

Do not point a new binary at an old Go-only SQL deployment expecting it to discover
or convert the old data. For disposable data, recreate it. For data you must keep:

1. Keep the old binary and cluster backup available; verify a restore first.
2. Export the application's records through the old revision's supported API.
3. Create new schemas in a separate destination with the new revision and import
   through its API. There is no bundled general-purpose migration command.
4. Compare keys, record counts, values, indexes and application query results.
5. Plan a write freeze or application-level synchronization before cutover. Keep
   the old deployment read-only until the rollback window is closed.

A raw FDB backup restores the old bytes; it does not translate them into a new
layout. Do not “migrate” by editing only a header or reusing the old subspace.
The changelog also excludes stores written only by earlier pre-release Go builds;
check the exact originating revision, not just whether it predates a tag.

## Sharing a Java store / changing store format

Go's default target is **format 14**, while Java 4.14.2.0 defaults to **7**.
Both have a supported ceiling of **15**. The default and the ceiling are different
settings: Java's default does not mean that it rejects format 14.

Opening an older store with Go's default builder can stage a format upgrade;
committing that transaction persists it. An open is therefore not necessarily a
read-only compatibility probe. If older readers must remain usable, select a
common target **before the first open** with `StoreBuilder.SetFormatVersion(...)`.
Pin `OnlineIndexerBuilder.SetFormatVersion(...)` too when it opens stores on your
behalf. Do not enable features that require a higher format during that rollout.

A lower target is **not a downgrade**: opening a store already at a higher,
supported version keeps that stored version. A reader whose ceiling is lower
will refuse the store. Format 15 enables pending index write queues and must be
an explicit deployment decision, including the readers and indexers involved.

Rehearse against a restored copy with the exact metadata, serializer and index
options. Validate both Java→Go and Go→Java writes/reads, including the exceptions
in the compatibility page. Record the header format before and after committing.
Do not manually decrease the stored format number to force an old binary to open
new data; that does not undo format-dependent writes.

## FoundationDB cluster upgrades

The pure-Go client implements the **7.3 wire protocol**, with **7.3.77** as its
reference. It does not ship libfdb_c's multi-version client machinery. The
`APIVersion` setting selects API semantics; it does not teach the transport a new
protocol. FDB 8.0 support is not established here.

Before changing the cluster version:

1. Consult the upstream FoundationDB upgrade procedure for the exact source and
   destination versions, including downgrade limitations and backup requirements.
2. Confirm that every deployed client binary supports the destination protocol.
   For pure-Go clients, do not assume a cross-protocol rolling upgrade works.
3. Exercise connection, recovery, transactions and fault handling against the
   destination version in staging. Keep the application upgrade and cluster
   upgrade independently reversible where upstream permits it.
4. If choosing `-tags libfdbc`, build and test with the intended native client
   installation. This changes the transport only; neither cross-version cluster
   support nor Record Layer compatibility follows merely from setting the tag.

There is no documented zero-downtime cross-protocol upgrade guarantee for the
pure-Go client. Do not start a cluster upgrade on the assumption it will reconnect
to an unimplemented protocol.

## Rollback checklist

- Restore-test backups and retain the old binaries and exact dependency versions.
- Check stored formats, metadata versions, index options and catalog layout against
  the old binary's capabilities before rolling it back.
- Discard engine-private SQL continuations; do not carry them across revisions or
  engines as a migration mechanism.
- If new writes cannot be read by the old binary, rollback needs a compatible data
  restore or an explicitly tested reverse migration, not just a binary replacement.
- Validate application invariants and query results after cutover; a successful
  store open is not a complete compatibility check.
