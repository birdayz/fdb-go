---
lens: java-compat
applies: ["pkg/recordlayer/**", "pkg/relational/**", "proto/**"]
references:
  - "Java Record Layer / Relational 4.14.2.0 in fdb-record-layer/ (gitignored checkout; the reference implementation; clone if missing: git clone --depth 1 --branch 4.14.2.0 https://github.com/FoundationDB/fdb-record-layer fdb-record-layer)"
  - "DIVERGENCES.md and docs/compatibility.md: declared compatibility boundaries"
---
Go and Java applications share record stores. Persisted keys, records, index entries,
versions, store headers and supported continuations must be byte-identical to Java 4.14.2.0;
SQL behaviour shared with Java must give Java's results and errors. Cite the Java class and
line for every verdict.

### java-compat/bytes — Persisted bytes match Java
Severity: blocker
Check: tuple and subspace keys, record serialization and split records (100 KB chunks,
suffixes), version stamps and inline versions, index entry keys and values for every index
type the change touches, store headers and format versions, continuations, catalog and
keyspace layout. Old data written by Java or by earlier Go must still read.
Verify: show the Java encoder and the Go encoder side by side, or the cross-engine test.
Not a finding: in-memory representations that never reach storage or the wire.

### java-compat/sql — SQL results, types and errors match Java
Severity: high
Check: query results, types and nullability, type promotion, result metadata, and the
SQLSTATE and message of errors match Java for the shared SQL surface; a Go-only extension is
declared in DIVERGENCES.md and does not change behaviour on statements Java accepts.
Verify: cite the Java code path (SemanticAnalyzer, the visitors, the executor) or a Java
oracle test that shows Java's answer.
Not a finding: a statement Java rejects that Go also rejects, with a different but
declared message.

### java-compat/divergences — Differences are declared and pinned
Severity: medium
Check: a deliberate difference from Java is recorded in DIVERGENCES.md or
docs/compatibility.md with its reason, and a test pins the current behaviour; a change that
removes a divergence updates the record; claims of parity name the test that proves them.
Verify: name the difference and show it is not recorded, or the record that is now false.
Not a finding: internal Go structure that has no observable difference.
