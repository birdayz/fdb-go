# fdb-go - FoundationDB Ecosystem for Go

[![CI](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml/badge.svg)](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml)
[![Test Report](https://img.shields.io/badge/test_report-latest-2980b9)](https://fdb.dev/reports/latest.html)
[![Go Reference](https://pkg.go.dev/badge/fdb.dev.svg)](https://pkg.go.dev/fdb.dev)

A native [FoundationDB](https://www.foundationdb.org/) client, a testcontainer, the
[Record Layer](https://github.com/FoundationDB/fdb-record-layer) and an embedded SQL
engine for Go, without cgo. Requires a FoundationDB 7.3 cluster.

```sh
go get fdb.dev@master
```

Or run the examples from a clone. This starts a disposable cluster in Docker
(`./frl fdb down` removes it):

```sh
go build -o frl ./cmd/frl &&
FDB_CLUSTER_FILE="$(./frl fdb up)" && export FDB_CLUSTER_FILE &&
go run ./example/sql &&
go run ./example
```

Pre-1.0; expect API changes. [Status and known gaps](STATUS.md). Independent project,
not affiliated with Apple or the FoundationDB project.

## Native Go Client

[`fdb.dev/pkg/fdbgo/fdb`](pkg/fdbgo/fdb) implements FoundationDB's 7.3 client
protocol without cgo or `libfdb_c`, with an API modeled on Apple's Go binding.
[Differential tests](pkg/fdbgo/bench/) compare results, stored bytes and error codes
against libfdb_c 7.3.77 on every pull request.

```go
import "fdb.dev/pkg/fdbgo/fdb"

fdb.MustAPIVersion(730)
db, err := fdb.OpenDatabase(clusterFile)
if err != nil {
	return err
}
defer db.Close()

v, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
	tx.Set(fdb.Key("hello"), []byte("world"))
	return tx.Get(fdb.Key("hello")).Get()
})
```

`Transact` retries retryable errors; keep external side effects outside its callback. [Supported options](pkg/fdbgo/fdb/OPTIONS.md).

One transaction per operation, Go client vs. Apple's binding over libfdb_c, same
process and cluster:

| Operation | Go | CGo + libfdb_c | Go allocs/op |
|---|---:|---:|---:|
| Get, 100 B value | 117 µs | 207 µs | 73 |
| Get, 10 KB value | 140 µs | 224 µs | 73 |
| GetRange, 10 keys | 156 µs | 248 µs | 86 |
| GetRange, 100 keys | 251 µs | 339 µs | 122 |
| Set 100 B + commit | 1.25 ms | 1.02 ms | 105 |

Median of 5, master 9a9a448, Ryzen 9 3900X, single-node FDB 7.3.77 (`ssd` engine) in
local Docker. Allocation counts cover the Go heap only, not libfdb_c's.
[Raw results and commands](docs/benchmarks/). An
[earlier, larger comparison](tools/clientbench/results/r17/TABLES.md)
measured an older client revision.

## Testcontainer

[`fdb.dev/pkg/testcontainers/foundationdb`](pkg/testcontainers/foundationdb) starts
FoundationDB in Docker and returns a configured, ready cluster. Single node or
several ([`RunCluster`](pkg/testcontainers/foundationdb/cluster.go)); storage engine,
redundancy and knobs are options; `Pause` simulates an unreachable cluster.

```go
import foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"

func TestThing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := foundationdbtc.Run(ctx, "") // foundationdb/foundationdb:7.3.77
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Terminate(context.Background()) })

	clusterFile, err := c.ClusterFilePath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fdb.MustAPIVersion(730)
	db, err := fdb.OpenDatabase(clusterFile)
	// ...
}
```

`Run` returns a usable database in 4.6 s (median of 5, image already pulled, same
machine). [Raw results](docs/benchmarks/).

## Record Layer

A Go port of FoundationDB's [Record Layer](https://github.com/FoundationDB/fdb-record-layer):
protobuf records, secondary indexes, schema evolution and resumable cursors. Go and
Java Record Layer 4.14.2.0 can share record stores in the supported formats; see
[compatibility](docs/compatibility.md) and the [Java conformance tests](conformance/).

### Go

[`fdb.dev/pkg/recordlayer`](pkg/recordlayer). Records are your protobuf messages;
as in Java, a union message lists the record types:

```proto
message Order {
  optional int64 order_id = 1;
  optional int32 price = 2;
}

message RecordTypeUnion {
  option (com.apple.foundationdb.record.record).usage = UNION;
  optional Order _Order = 1;
}
```

```go
builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(shoppb.File_shop_proto)
builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
builder.AddIndex("Order", recordlayer.NewIndex("price_idx", recordlayer.Field("price")))
metadata, err := builder.Build()
if err != nil {
	return err
}

rdb := recordlayer.NewFDBDatabase(db)
_, err = rdb.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
	store, err := recordlayer.NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(metadata).
		SetSubspace(subspace.FromBytes([]byte("app"))).
		CreateOrOpen()
	if err != nil {
		return nil, err
	}
	if _, err := store.SaveRecord(&shoppb.Order{OrderId: proto.Int64(1), Price: proto.Int32(25)}); err != nil {
		return nil, err
	}
	orders, err := recordlayer.GetTypedRecordStore[*shoppb.Order](store, "Order")
	if err != nil {
		return nil, err
	}
	return orders.LoadRecord(tuple.Tuple{int64(1)})
})
```

[Runnable example](example/getting_started.go) ·
[operations guide](docs/operations.md) (online index builds, schema evolution).

| Operation, one transaction each | Time | Allocs/op |
|---|---:|---:|
| Load record by primary key | 210 µs | 180 |
| Save record + commit | 1.01 ms | 289 |
| Save record with one index + commit | 1.01 ms | 359 |
| Scan 1,000 records through an index, pipelined | 23.6 ms | 47,378 |
| Same scan, loading records one at a time | 57.8 ms | 41,221 |

Median of 5, same machine, FDB `memory` engine. [Raw results](docs/benchmarks/).

### SQL

[`fdb.dev/pkg/relational/sqldriver`](pkg/relational/sqldriver) is an embedded
`database/sql` driver over Record Layer stores. It uses Java Relational's SQL dialect
and a port of its Cascades planner. Not PostgreSQL-compatible;
[differences from Java](DIVERGENCES.md).

```go
import (
	"database/sql"
	"net/url"

	"fdb.dev/pkg/relational/sqldriver"
)

sqldriver.RegisterDomainIfNotExists("FRL")
db, err := sql.Open("fdbsql", "fdbsql:///FRL/SHOP?schema=APP&cluster_file="+url.QueryEscape(clusterFile))
if err != nil {
	return err
}
defer db.Close()

rows, err := db.QueryContext(ctx,
	"SELECT customer, SUM(amount) FROM orders GROUP BY customer ORDER BY customer")
if err != nil {
	return err
}
defer rows.Close()
```

[Complete example](example/sql/main.go), including creating the database, schema
and table.

| Statement, autocommit | Time | Allocs/op |
|---|---:|---:|
| Point select by primary key | 788 µs | 1,477 |
| Index range scan, 50 of 100 rows | 2.37 ms | 5,153 |
| Single-row insert | 2.53 ms | 4,169 |

Median of 5, same machine, FDB `memory` engine. The selects reuse a cached plan; the
insert is planned on every execution. [Raw results](docs/benchmarks/).

## AI Disclaimer

LLMs were used heavily in the development and maintenance of this project. Credit
goes to the original authors of FoundationDB and the FoundationDB Record Layer.

---

[Contributing](CONTRIBUTING.md) · [Test report](https://fdb.dev/reports/latest.html) ·
[Security](SECURITY.md) · [Support](SUPPORT.md) · [Apache-2.0](LICENSE) ([NOTICE](NOTICE))
