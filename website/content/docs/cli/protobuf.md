---
title: Protobuf records and indexes
description: "Build a descriptor set from an annotated protobuf schema, create a Record Layer store, and work with records and secondary indexes using fdb."
weight: 1
---

This tutorial uses a protobuf schema as the source of Record Layer metadata.
You will create a native store, save contacts, inspect four secondary indexes,
and verify a unique-index constraint. No application plugin, generated record
code, or metadata dumper is required.

You need the `fdb` CLI, a local Docker installation, and
[Buf](https://buf.build/docs/cli/installation/). Downloading the example also
uses `curl` and `tar`. The source-build step needs the Go version in
[`go.mod`](https://github.com/birdayz/fdb-go/blob/master/go.mod); you do not need
to write a Go application.

Run all blocks in the same shell. The example uses a fresh working directory,
an isolated CLI config, and a disposable database.

## 1. Build the current CLI

```sh
git clone https://github.com/birdayz/fdb-go.git &&
cd fdb-go &&
go build -o .tmpbuild/fdb ./cmd/fdb &&
export PATH="$PWD/.tmpbuild:$PATH" &&
fdb version
```

If you already have a checkout, build from its root instead of cloning another
copy. The build uses that checkout's source, including local changes. This guide
follows the current source rather than an older tagged CLI release.

## 2. Download the complete schema

```sh
source_dir="$PWD"
example_dir="$(mktemp -d)"
cd "$example_dir"

curl -fsSLO https://fdb.dev/examples/record-layer/record-layer.tar.gz
tar -xzf record-layer.tar.gz
export FDB_CONFIG="$PWD/fdb-config.yaml"
```

The [archive](/examples/record-layer/record-layer.tar.gz) includes
[`buf.yaml`](/examples/record-layer/buf.yaml),
[`records.proto`](/examples/record-layer/records.proto), and the complete
Record Layer proto import closure. The upstream Apple proto files retain their
original license headers; you do not need to locate or rewrite those imports.

The records schema is:

```proto
syntax = "proto2";

package contacts;

import "record_metadata_options.proto";

option (com.apple.foundationdb.record.schema).split_long_records = true;
option (com.apple.foundationdb.record.schema).store_record_versions = true;

message Contact {
  optional int64 id = 1 [(com.apple.foundationdb.record.field).primary_key = true];
  optional string name = 2;
  optional string email = 3 [(com.apple.foundationdb.record.field).index = {
    type: "value"
    unique: true
  }];
  optional string city = 4 [(com.apple.foundationdb.record.field).index.type = "value"];
  optional int64 score = 5 [(com.apple.foundationdb.record.field).index.type = "rank"];
  repeated string tags = 6 [(com.apple.foundationdb.record.field).index.type = "value"];
}

message RecordTypeUnion {
  optional Contact contact = 1;
}
```

`id` is the primary key. The field annotations define:

| Index | Definition |
|---|---|
| `Contact$email` | Unique value index: two contacts cannot have the same email. |
| `Contact$city` | Value index on the city. |
| `Contact$score` | Rank index on the score. |
| `Contact$tags` | Value index over a repeated field, with an entry for each tag. |

`RecordTypeUnion` identifies the record messages in this schema. The schema
options control long-record splitting and record-version storage. These are
standard Record Layer protobuf options, not CLI-specific annotations.

## 3. Compile and inspect metadata without FoundationDB

```sh
buf build . --as-file-descriptor-set -o schema.binpb

fdb rl meta get --descriptor-set schema.binpb --records-file records.proto
fdb rl meta types ls --descriptor-set schema.binpb --records-file records.proto
fdb rl index ls --descriptor-set schema.binpb --records-file records.proto --no-fdb
fdb rl index describe 'Contact$email' --descriptor-set schema.binpb --records-file records.proto
```

Buf produces a standard `google.protobuf.FileDescriptorSet` containing the
records schema and its imports. `fdb` reads that artifact; it does not execute
Buf or parse `.proto` source text itself. The metadata and index definitions
above can be inspected before starting a database. `--no-fdb` leaves index state
unknown rather than trying to read a store that does not exist yet.

Always **single-quote index names containing `$`** in shell commands. For
example, `'Contact$email'` is the literal index name; double quotes would allow
the shell to expand `$email`.

This set contains only one union-declaring records file, so `--records-file`
could be omitted. The explicit name shows how to select the schema when a set
contains several candidates. See [connections and metadata](/docs/cli/metadata/)
for `protoc` compilation and metadata-version overrides.

Descriptor input supplies current annotated definitions, not an application's
index-evolution history. `--meta-version` changes only the top-level metadata
version; it cannot recover original index versions or past metadata changes.
This tutorial changes records and index states, not the schema artifact.

## 4. Start FoundationDB and configure the store

Make sure Docker is running. If port `4500` is occupied, add an unused port such
as `--port 4510` to the startup command. If you started the default throwaway
cluster from the overview and no longer need it, `fdb dev down` removes it.

```sh
cluster_file="$(fdb dev up --name fdb-rl-example --context contacts)" &&
fdb tx read-version &&
cat > "$FDB_CONFIG" <<EOF &&
current_context: contacts
contexts:
  - name: contacts
    cluster_file: "$cluster_file"
    keyspace_path: /examples/contacts
    metadata:
      descriptor_set:
        path: "$PWD/schema.binpb"
        records_file: records.proto
EOF

fdb config view
```

`dev up` initializes a single-node FoundationDB container and captures its
cluster-file path. The config selects the native store at `/examples/contacts`
and the compiled schema. Only this example's `fdb-config.yaml` is rewritten;
your normal `~/.fdb/config.yaml` is untouched.

This is a native Record Layer store, not a SQL database. Do not add
`--database`/`--schema` flags: those select catalog-managed metadata instead of
the descriptor set.

## 5. Create the empty store

```sh
fdb rl store create --yes
fdb rl store info
fdb rl index ls
```

Creation writes the store header and initializes index states from the selected
metadata. With the supplied schema, the derived metadata version is `4` and all
four indexes start `READABLE`. It refuses an existing store or any nonempty prefix; it does not
replace data or update an existing store's metadata. The descriptor definitions
remain in `schema.binpb` and are **not** implicitly copied into the record-store
subspace. Keep that artifact available for subsequent commands.

Record writes and reads do not implicitly create a missing native store. For
SQL-managed stores, use [SQL DDL](/docs/cli/sql/) rather than `rl store create`.

## 6. Save, scan, and load records

```sh
fdb rl record put --type Contact \
  '{"id":1,"name":"Ada","email":"ada@example.com","city":"London","score":100,"tags":["math","friends"]}' --yes
fdb rl record put --type Contact \
  '{"id":2,"name":"Grace","email":"grace@example.com","city":"New York","score":200,"tags":["computing","friends"]}' --yes

fdb rl record scan --type Contact --limit 10
fdb rl record get 1
```

Each command emits JSON record envelopes with `primary_key`, `record_type`, and
`record`. The scan shows keys `1` and `2`; `get 1` loads the first contact. Protobuf
JSON renders `int64` fields such as `id` and `score` as strings in output.
Record puts maintain all four indexes in the same transaction.

`--yes` supplies the explicit write confirmation. Without it, a terminal prompts
and non-interactive input is refused. For a validation-only write, `record put`
also supports `--dry-run`.

## 7. Inspect and scan the indexes

```sh
fdb rl index ls
fdb rl index describe 'Contact$email'
fdb rl index describe 'Contact$city'
fdb rl index describe 'Contact$score'
fdb rl index describe 'Contact$tags'

fdb rl index scan 'Contact$email'
fdb rl index scan 'Contact$city'
fdb rl index scan 'Contact$score'
fdb rl index scan 'Contact$tags'
```

`describe` shows index type, uniqueness, fields, options, and metadata versions.
`scan` emits index entries with their indexed values and primary keys. The tag
index fans out: the two contacts contribute an entry for each of their tags.
After the first two puts, email, city, and score scans each have two entries;
tags has four, including separate `friends` entries for keys `1` and `2`.
This is a view of maintained index entries, not SQL query output or a
rank-specific query API.

## 8. Verify uniqueness enforcement

Attempt to save another primary key with Ada's email:

```sh
if fdb rl record put --type Contact \
  '{"id":3,"name":"Another Ada","email":"ada@example.com","city":"Paris","score":50,"tags":["duplicate"]}' --yes 2>duplicate.err; then
  echo 'Unexpected: the duplicate email was accepted' >&2
  exit 1
else
  cat duplicate.err >&2
fi

fdb rl record scan --type Contact --limit 10
fdb rl index scan 'Contact$email'
```

The put must fail with a uniqueness error and a non-zero exit. The record and
index scans should still contain only Ada and Grace: the failed write does not
leave a partial record or index entry.

Now save the third primary key with a different email:

```sh
fdb rl record put --type Contact \
  '{"id":3,"name":"Katherine","email":"katherine@example.com","city":"London","score":150,"tags":["math","space"]}' --yes
fdb rl record get 3
fdb rl index scan 'Contact$email'
```

The store now has three contacts with distinct email addresses.

## 9. Update a record and its indexes

A put with an existing primary key replaces that record. Change Ada's city,
score, and tags:

```sh
fdb rl record put --type Contact \
  '{"id":1,"name":"Ada","email":"ada@example.com","city":"Paris","score":110,"tags":["math"]}' --yes

fdb rl record get 1
fdb rl index scan 'Contact$city'
fdb rl index scan 'Contact$score'
fdb rl index scan 'Contact$tags'
```

The city index now associates key `1` with `Paris`, not `London`; Katherine's
key `3` remains associated with `London`. Ada's score changes from `100` to
`110`, and her `friends` tag entry is removed while Grace's remains. The other
contacts' index entries remain. Record and index changes
commit together; no manual index repair is needed after an ordinary put.

## 10. Disable, build, and rebuild an index

Index state is stored in FoundationDB separately from the descriptor definition.
Disable the city index, then build it back to a readable state:

```sh
fdb rl index set-state 'Contact$city' disabled --yes
fdb rl index ls

if fdb rl index scan 'Contact$city'; then
  echo 'Unexpected: a disabled index was readable' >&2
  exit 1
fi

fdb rl index build 'Contact$city' --yes
fdb rl index ls
fdb rl index scan 'Contact$city'
```

While disabled, the index is not maintained or available for queries. The build
scans the records and restores `READABLE` when complete. For a build that has
already started, rerunning `build` resumes its progress.

A rebuild explicitly clears an index and builds it from scratch. Rebuild the
tag index:

```sh
fdb rl index rebuild 'Contact$tags' --yes
fdb rl index ls
fdb rl index scan 'Contact$tags'

fdb rl record get 1
fdb rl record get 2
fdb rl record get 3
fdb rl record scan --type Contact --limit 10
```

The rebuilt tag index should be `READABLE` and include the current five tag
entries. The record scan shows the updated Ada record, Grace, and Katherine.
Index maintenance does not change the descriptor file or invent metadata
history. For large stores, [index-build flags](/docs/cli/reference/#indexes)
control load and bound a resumable pass.

## 11. Preview and delete a record

A dry run validates the operation without committing a deletion:

```sh
fdb rl record delete 3 --dry-run
fdb rl record get 3
```

Katherine is still present. Delete the record explicitly and inspect the result:

```sh
fdb rl record delete 3 --yes
fdb rl record scan --type Contact --limit 10
fdb rl index scan 'Contact$email'
fdb rl index scan 'Contact$tags'
```

The final records are Ada and Grace. Katherine's record and index entries are
removed transactionally. Creating this same store again would be refused;
`store create` is not a reset command.

## 12. Clean up

```sh
fdb dev down --name fdb-rl-example
unset FDB_CONFIG
cd "$source_dir"
rm -rf "$example_dir"
```

The container's data and the example's temporary config, cluster file, and schema
artifacts are removed. The normal CLI config is unchanged. If you used a custom
`FDB_CONFIG` before starting, restore that environment variable instead of
leaving it unset.

For existing application stores, read [connections and metadata](/docs/cli/metadata/).
For online index builds, state changes, dry runs, and other operations, use the
[command reference](/docs/cli/reference/).
