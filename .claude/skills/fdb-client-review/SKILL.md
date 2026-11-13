---
name: fdb-client-review
description: "Review of pure-Go FoundationDB client changes (pkg/fdbgo). Reviews run through the bruh-code-review skill; its fdb-client lens (.bruh/review/fdb-client.md) holds the C++-as-spec rules. For client engineering, use fdb-client-engineer."
---

# FDB client review

Client and wire changes are reviewed with the **`bruh-code-review`** skill. Its `fdb-client`
lens (`.bruh/review/fdb-client.md`) applies to `pkg/fdbgo/**` and checks what the FDB C++
developer review checked: FoundationDB C++ 7.3.77 is the spec, every verdict cites the C++
`file:line`, and transaction semantics, wire bytes, error codes, concurrency and
deterministic tests must hold. The core maintainability and tests lenses cover what the
Torvalds review did. Change review rules in the lens, not here.

The client workflow (RFC, reading the C++ first, test shapes, code map) is in
**`fdb-client-engineer`**; finding and proving divergences is in **`hunt-divergences`**.
