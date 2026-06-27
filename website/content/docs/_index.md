---
title: Docs
next: getting-started
weight: 1
---

The complete, pure-Go stack for FoundationDB: a from-scratch wire-protocol client, a
wire-compatible Record Layer, and a SQL engine.

{{< cards >}}
  {{< card link="getting-started" title="Getting Started" icon="play" subtitle="Install, connect, write your first record." >}}
  {{< card link="/docs/client" title="The Client" icon="lightning-bolt" subtitle="Pure-Go vs libfdb_c, transactions, retries." >}}
  {{< card link="/docs/record-layer" title="Record Layer" icon="database" subtitle="Records, indexes, versions, schema evolution." >}}
  {{< card link="/docs/sql" title="SQL Engine" icon="search" subtitle="The database/sql driver and the Cascades planner." >}}
{{< /cards >}}

{{< callout type="warning" >}}
  **Pre-1.0.** The wire format is the project's hard line and is the part to trust first;
  the SQL engine is usable but evolving. Pin a commit and run the suites against your
  workload before relying on it in production.
{{< /callout >}}
