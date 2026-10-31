# Observability

The pure-Go FoundationDB client exposes tracing, metrics snapshots, and `log/slog`
diagnostics. Applications own collection and export. Instrumentation must not
change transaction outcomes, retries, cancellation, persisted bytes, or the FDB
wire protocol.

## Engineering contract

- **Compatible first.** FoundationDB C++ 7.3.77 is the reference for client span
  propagation and sampling. Record-layer timers follow Java Record Layer 4.14.2.0.
  Exporting Go spans is an additive integration, not a different transaction model.
- **Opt-in export.** Pass a tracer explicitly. The client does not install a global
  provider, read exporter environment variables, start a collector, or own SDK
  shutdown. The default wire sample rate is zero.
- **Small core.** Runtime tracing depends on the OTel API, not an SDK or exporter.
  Applications choose their provider, processor, transport, credentials, and queues.
  Metrics remain a pull-based snapshot; the optional Prometheus handlers do not
  introduce a metrics SDK into transaction execution.
- **Bounded telemetry.** Span names are fixed operations, never keys, values,
  tenant names, SQL text, or user-supplied error messages. Keep those out of metric
  labels as well. Traces are diagnostic samples, not an accounting ledger.
- **Explicit ownership.** Every started span needs a terminal path. Managed
  transaction helpers own their spans; callers own raw transaction handles.
  Use managed handles only within their callback; use `CreateTransaction` for a
  caller-owned reusable handle.

## Tracing setup

Both `client.OpenDatabase` and the `fdb` facade accept `WithTracer` and
`WithTracingSampleRate`. For a record store, open the client with those options
and wrap it with `fdb.WrapDatabase` and `recordlayer.NewFDBDatabase`.

The application creates a `trace.Tracer` from its own provider and passes it to
`client.WithTracer(tracer)`. Use a bounded batch span processor for production
export; synchronous processors and custom tracer callbacks execute on the
calling goroutine and can add latency. Callbacks must not panic or run transaction
methods, including `Reset` and `Cancel`: operation
admission can still be active when a callback runs, and concurrent reset can block
even state accessors. Observe database metrics instead. Set resource attributes such as
`service.name` on the provider, not on every database operation. Close database
handles before flushing and shutting down the provider, using a fresh, bounded
shutdown context rather than an already-cancelled request context.

For example, with an application-created `sdktrace.SpanExporter` named `exporter`
and imports for `client`, `sdktrace`, `context`, `time`, and `log/slog`:

```go
provider := sdktrace.NewTracerProvider(
    sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())),
    sdktrace.WithBatcher(exporter,
        sdktrace.WithMaxQueueSize(2048),
        sdktrace.WithMaxExportBatchSize(512),
        sdktrace.WithExportTimeout(5*time.Second),
    ),
)
defer func() {
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := provider.Shutdown(shutdownCtx); err != nil {
        slog.Error("flush database traces", "error", err)
    }
}()

db, err := client.OpenDatabase(ctx, clusterFile,
    client.WithAPIVersion(730),
    client.WithTracer(provider.Tracer("my-service/fdb")),
    client.WithTracingSampleRate(0.01),
)
if err != nil {
    return err
}
defer db.Close() // runs before provider shutdown
```

The batch processor's queue is bounded and uses the SDK's default drop-on-full
policy. Do not enable a blocking queue unless the application explicitly accepts
export backpressure on database calls. Monitor dropped/export-failed spans in
the application's telemetry pipeline; they are not database transaction errors.

### Sampling and IDs

`WithTracingSampleRate` controls the client wire sampled flag. The default is
`0.0`; `1.0` samples every transaction. An injected FDB `SPAN_PARENT` overrides the
random root's trace ID and sampled flag. Set the parent before the first read or
commit: changing it later changes wire propagation but cannot reparent an
already-started SDK span. The next retry starts an exported span with the new
parent. The OTel provider's sampler is a second
gate: it can discard a wire-sampled transaction, but cannot make the client
export a wire-unsampled transaction. A parent-based provider sampler respects
the wire sampling decision.

The client generates wire IDs whether or not an OTel tracer is configured. OTel
transaction and read spans inherit the wire trace ID but have SDK-generated span
IDs. This provides trace correlation, **not exact parent-child linkage between
OTel client spans and FDB server spans**. A Go `context.Context` is currently a
cancellation/deadline carrier, not automatic trace-parent injection; use the FDB
`SetSpanParent` option when explicitly joining an existing trace.

GRV batching is a separate FDB root with links to participating transactions in
the C++ model. Its wire trace ID is not a representative transaction's ID. Do not
rewrite it to make an OTel waterfall look simpler.

### Span boundaries

The existing export surface is a lazily started `Transaction` client span and
native-read children `fdbgo.getValue`, `fdbgo.getKey`, and `fdbgo.getRange`.
The transaction span starts with read-version acquisition (including write-only
commit's GRV). It covers one attempt, not the entire automatic-retry loop. A
retry or successful commit rotates the wire context; an explicitly injected
parent survives that rotation.

Reads satisfied wholly from read-your-writes state need not perform a native
operation and need not emit a child. Batched GRV RPCs, watch long-polls, shared
location lookup, and local mutation-buffer writes do not have separate exported
spans. A successful commit ends the transaction span; there is no separate
`fdbgo.commit` child. These spans are not a complete RPC census or a query profile.

`Transact` and `ReadTransact` finish remaining attempt spans on success, terminal
error, cancellation, and panic exits. Retry initialization is independent of the
whole-transaction latency anchor, which deliberately survives retries. A cancelled
or retired attempt cannot install a span whose start completes late.

Always call `Cancel` or `Reset` on a manually created transaction you do not
commit. Do not rely on garbage collection to export an abandoned span. Closing
the database does not enumerate and finish externally retained transaction
handles. Managed helper cleanup must not cancel successfully committed watches.

## Metrics and logs

`(*client.Database).Metrics()` returns a monotonic snapshot of client counters
and bounded latency sketches. `fdbmetrics.Handler` exports the snapshot in
Prometheus text format. The pure-Go `fdb.Database` facade also forwards `Metrics()`
and can be passed directly to that handler. Keep either handle when wrapping a
record store: `*recordlayer.FDBDatabase` does not expose client metrics.

Record-layer `StoreTimer` measures record operations, index work, timings, and
byte totals. `rlmetrics.Handler` exports it separately. Configure timers before
opening contexts so those contexts inherit them. Neither client sketches nor
record-layer timers are automatically translated into OTel instruments.

Operational diagnostics use `log/slog`; `WithLogger` installs a per-client
logger. Retry events and uncertain commit outcomes are diagnostics, not changes
to error or retry policy. See [the operations guide](operations.md#8-observability-hooks)
for metrics and timer wiring, and [multi-tenant operations](mt-saas.md#4-observability)
for cardinality boundaries.

## Production hardening work

This guide records the intended ownership and compatibility contract, not a
blanket production-readiness claim. Managed span cleanup, retry initialization,
and callback execution outside tracing/read-version locks and reset publication
have deterministic lifecycle regressions alongside real-FDB coverage.

Remaining capabilities include native-operation spans on the facade's pipelined
point-read path, bounded error/status attributes, automatic OTel-context parenting,
and an optional OTel metrics adapter. Until those are implemented and verified,
do not describe this as complete end-to-end OTel coverage. Outstanding work belongs in
[`TODO.md`](../TODO.md), with verification reported for the exact tested tree.
