---
title: Docs
next: getting-started
weight: 1
---

The complete, pure-Go stack for FoundationDB: a from-scratch wire-protocol client, a
wire-compatible Record Layer, and a SQL engine.

{{< cards >}}
  {{< card link="getting-started" title="Getting Started" icon="play" subtitle="Install, connect, and run." >}}
  {{< card link="maturity" title="Maturity & Status" icon="shield-check" subtitle="What's production-ready, how correctness is proven, version targets." >}}
  {{< card link="https://github.com/birdayz/fdb-go" title="Source & examples" icon="github" subtitle="API reference, the operator guide, and runnable examples." >}}
{{< /cards >}}

{{< callout type="warning" >}}
  **Pre-1.0.** The wire format is the project's hard line and the part to trust first; the
  SQL engine is usable but evolving. Pin a commit and run the suites against your workload
  before relying on it in production. See [Maturity](maturity).
{{< /callout >}}
