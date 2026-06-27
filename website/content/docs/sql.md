---
title: SQL Engine
weight: 4
---

A SQL engine exposed through Go's standard `database/sql` interface. Queries are optimized by a
**Cascades** query planner ported from Java's `fdb-relational-core` — index selection, sort
elimination, and streaming aggregation over the Record Layer.

```go
import _ "fdb.dev/pkg/relational/sqldriver"

db, _ := sql.Open("fdbsql", "fdbsql:///mydb?cluster_file=/etc/foundationdb/fdb.cluster&schema=main")
```

## DDL

```sql
CREATE DATABASE /mydb;

CREATE SCHEMA TEMPLATE app_tmpl
    CREATE TABLE Users (id BIGINT NOT NULL, name STRING, email STRING, PRIMARY KEY (id))
    CREATE INDEX idx_email ON Users (email);

CREATE SCHEMA /mydb/main WITH TEMPLATE app_tmpl;
```

## DML and queries

```go
db.Exec("INSERT INTO Users (id, name, email) VALUES (1, 'Alice', 'alice@example.com')")
db.Exec("UPDATE Users SET name = 'Bob' WHERE id = 1")

rows, _ := db.Query("SELECT name FROM Users WHERE email = ?", "alice@example.com")
```

The optimizer picks the `idx_email` index scan for that predicate rather than a full table scan.

## Scope

The SQL surface is validated by a cross-engine differential harness against Java. It is **usable and
evolving** — wide coverage, but with open correctness items on specific query shapes. The conformance
principle holds: where both engines run the same query, results match; net-new read-side query
extensions are allowed only when they never change what gets written to FDB.

{{< callout type="info" >}}
  The SQL engine is the youngest, fastest-moving part of the stack. Consult the conformance report
  and `TODO.md` before depending on a given query shape in production.
{{< /callout >}}
