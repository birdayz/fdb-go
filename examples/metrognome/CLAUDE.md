# Metrognome — Usage-Based Billing Engine

Example application demonstrating FDB Record Layer Go for usage-based billing.
Clone of [Metronome](https://metronome.com/) — a billing gnome that counts every API call.

## Stack

- **API**: ConnectRPC (8 services, proto-first, browser-compatible)
- **Storage**: FDB Record Layer Go (native client, no CGo)
- **Event Bus**: Kafka planned (franz-go, exactly-once via FDB-transactional offsets)
- **Build**: Bazel 9 (MODULE.bazel, gazelle)
- **Frontend**: React 19 + Vite + Tailwind + shadcn/ui (planned)
- **Testing**: Go testing + gomega + testcontainers (real FDB)

## Running

```sh
# From repo root
bazelisk build //examples/metrognome/...
bazelisk test //examples/metrognome/...
bazelisk run //examples/metrognome/cmd/metrognome
```

## Architecture

### Record Types (10)

Customer, Meter, Plan, Charge, Contract, UsageEvent, Invoice, Credit, Alert, KafkaOffset.
All share one FDB Record Store with UnionDescriptor for type discrimination.

### Indexes (9)

| Index | Type | Purpose |
|---|---|---|
| `meter_by_slug` | VALUE (UNIQUE) | Meter lookup by slug |
| `customer_by_external_id` | VALUE | Customer lookup by caller's ID |
| `charge_by_plan` | VALUE | List charges for a plan |
| `contract_by_customer` | VALUE | List contracts for a customer |
| `event_by_idempotency_key` | VALUE (UNIQUE) | Exactly-once event dedup |
| `event_by_customer_meter_time` | VALUE | Range queries for invoice generation |
| `invoice_by_customer` | VALUE | Invoice history |
| `credit_by_customer` | VALUE | Credit balance (ordered by priority+expiry) |
| `alert_by_customer` | VALUE | Alert lookup |
| `usage_sum` | SUM | O(1) usage aggregation by (customer, meter, bucket) |
| `usage_count` | COUNT | O(1) event count by (customer, meter, bucket) |

### Exactly-Once Event Ingestion

HTTP path uses idempotency key UNIQUE index — `SaveRecord` throws `RecordIndexUniquenessViolationError` on duplicate, caught and counted as dedup. All events in a batch committed atomically.

Kafka path (planned): consumer offsets stored in FDB (not Kafka's `__consumer_offsets`). Events + offset committed atomically. On restart, seek to FDB-stored offset.

### Billing Engine

Single FDB transaction per invoice:
1. Load contract → load plan's charges
2. For each charge: read SUM aggregate for customer/meter/period
3. Apply pricing model (flat, per-unit, tiered, volume, package, BPS)
4. Draw down credits (ordered by priority, then expiry)
5. Write Invoice record with line items

### Pricing Models (6)

| Model | Description |
|---|---|
| Flat | Fixed fee per period |
| Per-unit | `usage × unit_price` |
| Tiered | Each tier priced independently |
| Volume | All units at the tier they fall into |
| Package | Prepaid blocks (ceiling division) |
| BPS | Basis points on transaction value |

## Proto Layout

- `proto/metrognome/v1/` — API services (proto3): customer, meter, plan, contract, event, invoice, credit, alert
- `proto/metrognome/store/v1/` — Storage records (proto2, required for Record Layer): store.proto with UnionDescriptor

## Code Layout

- `cmd/metrognome/` — Server entry point (ConnectRPC + FDB init + health check)
- `internal/services/` — ConnectRPC handlers (8 services + helpers)
- `internal/storage/` — FDB Record Layer stores (10 stores + db.go metadata)
- `internal/billing/` — Pricing calculation (pricing.go) + invoice generation (engine.go)
- `internal/meter/` — Dynamic meter engine (runtime proto generation)
- `gen/` — Generated Go code from buf

## Dynamic Meter Engine

`internal/meter/` — the crown jewel. Runtime proto generation from user meter configs.

When a user creates a meter with `group_by: ["region", "model"]`:
1. We build a `FileDescriptorProto` at runtime with fields: `event_id`, `customer_id`, `region`, `model`, `timestamp_bucket`, `value`
2. Register the dynamic message type via `protoregistry.GlobalTypes.RegisterMessage(dynamicpb.NewMessageType(...))`
3. Create Record Layer metadata with SUM + COUNT indexes grouped by the user's dimensions
4. Each meter gets its own FDB subspace and Record Layer store

Users get arbitrary group-by dimensions without touching proto files. The dynamic proto is invisible — they just call `IngestEvent(slug, customerID, bucket, value, {"region": "us-east-1", "model": "gpt-4"})`.

## Benchmarks

| Benchmark | ns/op | Throughput |
|---|---|---|
| EventIngest×1 | 1,027,000 | 974 events/sec |
| EventIngest×10 | 3,442,000 | 2,905 events/sec |
| EventIngest×100 | 28,201,000 | 3,546 events/sec |
| UsageQuery | 200,225 | 4,994 queries/sec |
| InvoiceGeneration | 1,267,000 | 789 invoices/sec |

Usage queries sub-ms (O(1) SUM index read). Invoice generation 1.3ms. Event ingestion bottlenecked by per-event idempotency pre-check.

## Tests

36 tests across 4 test targets against real FDB (testcontainers):
- Customer CRUD
- Meter CRUD (slug uniqueness)
- Event ingestion + idempotency dedup
- Usage aggregation (SUM + COUNT + per-bucket)
- End-to-end invoice generation (50 events → tiered pricing → invoice)
- Invoice with credit drawdown ($10 - $3 credit = $7)
- All 6 pricing models (unit tests)
- Zero usage invoice
- Tiered invoice (150 events, 3 tiers)
- Dynamic meter: no group-by (simple counter)
- Dynamic meter: with group-by (region + model dimensions)
- Dynamic meter: multi-bucket range queries
- Dynamic meter: idempotent registration
- Dynamic meter: unregistered meter error
- Alert triggering (below threshold → above threshold)
- Invoice status transitions (DRAFT→ISSUED→PAID, invalid→error, DRAFT→VOID)
- Customer pagination (page_size=2, continuation tokens, no overlap)
- Meter and plan listing
- Windowed usage query (hourly/daily)
- List charges for a plan
- Empty invoice listing for new customer

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Storage engine | Record Layer | Atomic SUM/COUNT indexes for O(1) aggregation |
| Dedup strategy | UNIQUE index on idempotency_key | FDB enforces uniqueness at write time |
| Pricing calc | Application-level | Complex tiered/volume logic doesn't map to FDB primitives |
| Invoice generation | Single FDB tx | Atomicity: all line items + credit drawdown + invoice |
| Dynamic group-by | Planned: runtime proto generation | `dynamicpb` + `protoregistry.RegisterMessage()` + per-meter stores |
| fdbgo vs apple binding | fdbgo (native) | No CGo, pure Go, 3.5x faster reads |
