# Metrognome — Usage-Based Billing Engine

> *"A little gnome that counts every API call, byte, and token — so you don't have to."*

Clone of [Metronome](https://metronome.com/) built on FDB Record Layer Go (native, no CGo).

## Design

### Why Record Layer (not plain fdbgo)

| Concern | Plain fdbgo | Record Layer |
|---|---|---|
| **Real-time aggregation** | Manual atomic mutations, hand-rolled key layout | SUM/COUNT indexes — O(1) reads, auto-maintained on SaveRecord |
| **Schema evolution** | Manual migration code | MetaDataEvolutionValidator, version tracking, FormerIndex |
| **Secondary indexes** | Hand-rolled key prefixes | Declarative indexes, automatic PK dedup, fan-out for repeated fields |
| **Querying** | Manual tuple pack/unpack, range construction | TupleRange, ScanIndex, ScanIndexRecords, continuation tokens |
| **Correctness** | Roll your own idempotency | VALUE index skip-on-unchanged, atomic mutation semantics well-tested |
| **Audit trail** | Manual versioning | Record versioning (FDBRecordVersion), VERSION index |

**Verdict:** Record Layer. The entire value prop of UBB is correct aggregation — Record Layer's atomic indexes are purpose-built for this. We'd be reimplementing half of it poorly with plain fdbgo.

### Exactly-Once Event Ingestion

The hardest problem in UBB. Events must be counted **exactly once** — not at-least-once, not at-most-once.

```
Kafka topic: "usage-events"
    │
    ▼
┌─────────────────────────────────────┐
│  Kafka Consumer (Go, consumer group) │
│                                      │
│  1. Poll batch of events             │
│  2. For each event:                  │
│     - Deduplicate by idempotency_key │
│     - Write UsageEvent record        │
│     - (SUM/COUNT indexes auto-update)│
│  3. Write Kafka offset to FDB        │
│  4. Commit FDB transaction           │
│     ─── ALL OR NOTHING ───           │
│  5. Do NOT commit Kafka offsets      │
│     (FDB is the source of truth)     │
└─────────────────────────────────────┘
```

**Key insight:** Kafka offsets are NOT committed to Kafka's `__consumer_offsets`. Instead, the offset is written into FDB in the same transaction as the event records. On restart, the consumer reads its last committed offset from FDB and seeks to it. This gives us exactly-once: if the FDB transaction fails, both the events AND the offset are rolled back.

**Idempotency key:** Each event carries a client-provided `idempotency_key`. Before writing, we check if a record with that key already exists (VALUE index lookup). If it does, skip. This handles the case where FDB committed but the consumer crashed before acknowledging — on re-read from Kafka, the duplicate is caught.

### Data Model

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│   Customer   │────▶│   Contract   │────▶│     Plan     │
│              │     │  start/end   │     │              │
│  id          │     │  customer_id │     │  id          │
│  name        │     │  plan_id     │     │  name        │
│  external_id │     │              │     │              │
└──────────────┘     └──────────────┘     └──────────────┘
                                                 │
                                                 ▼
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ UsageEvent   │────▶│    Meter     │     │   Charge     │
│              │     │ (billable    │◀────│              │
│  id          │     │  metric)     │     │  meter_id    │
│  customer_id │     │              │     │  plan_id     │
│  meter_slug  │     │  id          │     │  pricing     │
│  timestamp   │     │  slug        │     │  (model)     │
│  value       │     │  agg_type    │     │              │
│  properties  │     │  event_filter│     └──────────────┘
│  idemp_key   │     │  group_keys  │
└──────────────┘     └──────────────┘
                                          ┌──────────────┐
┌──────────────┐     ┌──────────────┐     │    Alert     │
│   Invoice    │     │    Credit    │     │              │
│              │     │              │     │  customer_id │
│  id          │     │  id          │     │  meter_slug  │
│  customer_id │     │  customer_id │     │  threshold   │
│  period_start│     │  amount      │     │  type        │
│  period_end  │     │  remaining   │     │  triggered   │
│  line_items  │     │  expires_at  │     └──────────────┘
│  total       │     │  priority    │
│  status      │     └──────────────┘
└──────────────┘

┌──────────────┐
│ KafkaOffset  │  ← consumer offset stored in FDB for exactly-once
│              │
│  topic       │
│  partition   │
│  offset      │
└──────────────┘
```

### Record Layer Index Strategy

| Index | Type | Expression | Purpose |
|---|---|---|---|
| `events_by_customer_time` | VALUE | `Concat(customer_id, meter_slug, timestamp)` | Range queries for invoice generation |
| `events_by_idempotency` | VALUE (UNIQUE) | `idempotency_key` | Exactly-once dedup |
| `usage_sum` | SUM | `GroupBy(value, customer_id, meter_slug, timestamp_bucket)` | Real-time usage aggregation |
| `usage_count` | COUNT | `GroupBy(EmptyKey(), customer_id, meter_slug, timestamp_bucket)` | Event count per bucket |
| `contract_by_customer` | VALUE | `customer_id` | Active contracts lookup |
| `invoice_by_customer` | VALUE | `Concat(customer_id, period_start)` | Invoice history |
| `credit_by_customer` | VALUE | `Concat(customer_id, expires_at)` | Credit balance queries |
| `alert_by_customer` | VALUE | `Concat(customer_id, meter_slug)` | Alert threshold checks |

**Timestamp bucketing:** Events carry a raw `timestamp_ms` (int64). For aggregation, we also store a `timestamp_bucket` field computed at ingest time (e.g., hourly bucket = `timestamp_ms / 3600000 * 3600000`). The SUM index groups by this bucket, giving us O(1) reads for "total usage in hour X for customer Y on meter Z".

### Aggregation Types (Meter Configuration)

Metronome supports these aggregation types for billable metrics:

| Aggregation | Description | Record Layer Index |
|---|---|---|
| `COUNT` | Number of events | COUNT index |
| `SUM` | Sum of a numeric property | SUM index |
| `MAX` | Maximum value in window | Scan VALUE index (reverse, limit 1) — or MAX_EVER_LONG if lifetime max |
| `UNIQUE` | Count of distinct values | Application-level (scan + distinct in property field) |
| `LATEST` | Most recent value | VALUE index (reverse scan, limit 1) |

### Pricing Models

Each Charge on a Plan defines how metered usage maps to a dollar amount:

| Model | Description | Example |
|---|---|---|
| **Flat** | Fixed fee per billing period | $100/month platform fee |
| **Per-unit** | `usage × unit_price` | $0.01 per API call |
| **Tiered** | Different rates at volume breakpoints (each tier priced independently) | First 1000 calls: $0.02, next 9000: $0.01, above 10000: $0.005 |
| **Volume** | Single rate based on total volume tier (all units at tier rate) | 0-1000: $0.02/each, 1001-10000: $0.01/each (ALL units at $0.01) |
| **Package** | Prepaid blocks of usage | $10 per 1000 API calls (partial block = full price) |
| **BPS (basis points)** | Percentage of transaction value | 25 bps (0.25%) of transaction amount |

### Billing Period & Invoice Generation

- Contracts define billing period (monthly, quarterly, annual)
- Period boundaries are calendar-aligned (e.g., month = 1st to last day)
- Invoice generation is a batch job that:
  1. Scans all active contracts whose period just ended
  2. For each contract, evaluates each charge against its meter
  3. Applies pricing model to compute line item amounts
  4. Applies credits (by priority, then expiry date)
  5. Creates Invoice record with line items and total
  6. All within a single FDB transaction per invoice (atomic)

### ConnectRPC API Surface

```protobuf
// Customer management
service CustomerService {
  rpc CreateCustomer(CreateCustomerRequest) returns (CreateCustomerResponse);
  rpc GetCustomer(GetCustomerRequest) returns (GetCustomerResponse);
  rpc ListCustomers(ListCustomersRequest) returns (ListCustomersResponse);
}

// Billable metrics
service MeterService {
  rpc CreateMeter(CreateMeterRequest) returns (CreateMeterResponse);
  rpc GetMeter(GetMeterRequest) returns (GetMeterResponse);
  rpc ListMeters(ListMetersRequest) returns (ListMetersResponse);
}

// Plans & charges
service PlanService {
  rpc CreatePlan(CreatePlanRequest) returns (CreatePlanResponse);
  rpc GetPlan(GetPlanRequest) returns (GetPlanResponse);
  rpc ListPlans(ListPlansRequest) returns (ListPlansResponse);
  rpc AddCharge(AddChargeRequest) returns (AddChargeResponse);
}

// Contracts
service ContractService {
  rpc CreateContract(CreateContractRequest) returns (CreateContractResponse);
  rpc GetContract(GetContractRequest) returns (GetContractResponse);
  rpc ListContracts(ListContractsRequest) returns (ListContractsResponse);
  rpc EndContract(EndContractRequest) returns (EndContractResponse);
}

// Event ingestion (high-throughput path)
service EventService {
  rpc IngestEvents(IngestEventsRequest) returns (IngestEventsResponse);
  rpc GetUsage(GetUsageRequest) returns (GetUsageResponse);
}

// Invoicing
service InvoiceService {
  rpc GetInvoice(GetInvoiceRequest) returns (GetInvoiceResponse);
  rpc ListInvoices(ListInvoicesRequest) returns (ListInvoicesResponse);
  rpc GenerateInvoice(GenerateInvoiceRequest) returns (GenerateInvoiceResponse);
}

// Credits
service CreditService {
  rpc GrantCredit(GrantCreditRequest) returns (GrantCreditResponse);
  rpc ListCredits(ListCreditsRequest) returns (ListCreditsResponse);
  rpc GetCreditBalance(GetCreditBalanceRequest) returns (GetCreditBalanceResponse);
}

// Alerts
service AlertService {
  rpc CreateAlert(CreateAlertRequest) returns (CreateAlertResponse);
  rpc ListAlerts(ListAlertRequest) returns (ListAlertResponse);
}
```

### Stack

| Layer | Technology |
|---|---|
| **API** | ConnectRPC (same as channelmind-ai) |
| **Storage** | FDB Record Layer Go (native client, no CGo) |
| **Event Bus** | Kafka (franz-go client) |
| **Proto** | buf v2 (Go + TypeScript codegen) |
| **Build** | Bazel 9 (MODULE.bazel, gazelle, rules_js) |
| **Frontend** | React 19 + Vite + Tailwind + shadcn/ui (same as channelmind-ai) |
| **Testing** | Ginkgo v2 + testcontainers (real FDB + real Kafka) |

### Project Layout

```
examples/metrognome/
├── CLAUDE.md                           # Sub-project instructions
├── TODO.md                             # This file
├── proto/
│   └── metrognome/
│       ├── v1/                         # API protos (services + messages)
│       │   ├── customer.proto
│       │   ├── meter.proto
│       │   ├── plan.proto
│       │   ├── contract.proto
│       │   ├── event.proto
│       │   ├── invoice.proto
│       │   ├── credit.proto
│       │   └── alert.proto
│       └── store/v1/                   # Storage protos (records + union)
│           └── store.proto
├── gen/                                # Generated Go code (buf)
├── cmd/
│   └── metrognome/
│       └── main.go                     # Server entry point
├── internal/
│   ├── services/                       # ConnectRPC service handlers
│   │   ├── customer.go
│   │   ├── meter.go
│   │   ├── plan.go
│   │   ├── contract.go
│   │   ├── event.go
│   │   ├── invoice.go
│   │   ├── credit.go
│   │   └── alert.go
│   ├── storage/                        # FDB Record Layer stores
│   │   ├── db.go                       # Metadata, indexes, store setup
│   │   ├── customer.go
│   │   ├── meter.go
│   │   ├── plan.go
│   │   ├── contract.go
│   │   ├── event.go
│   │   ├── invoice.go
│   │   ├── credit.go
│   │   └── alert.go
│   ├── billing/                        # Billing engine (pricing calculation)
│   │   ├── engine.go                   # Invoice generation orchestrator
│   │   ├── pricing.go                  # Pricing model evaluation
│   │   └── credits.go                  # Credit application logic
│   └── consumer/                       # Kafka consumer (exactly-once)
│       ├── consumer.go                 # Consumer loop + FDB offset tracking
│       └── dedup.go                    # Idempotency key dedup
├── app/                                # React frontend (later)
├── buf.yaml
├── buf.gen.yaml
└── BUILD.bazel
```

---

## Implementation Plan

### Phase 1: Foundation (Backend Core)
- [x] P0: Proto definitions — store records (UnionDescriptor) + API services (8 API protos + 1 store proto)
- [x] P0: Storage layer — FDB Record Layer metadata, indexes, store wrappers (10 stores, 9 indexes incl SUM+COUNT)
- [x] P0: Customer CRUD service
- [x] P0: Meter CRUD service (with unique slug index)
- [x] P0: Plan + Charge CRUD service
- [x] P0: Contract CRUD service (with End)
- [x] P0: Event ingestion service (HTTP path, idempotency key dedup via unique index)
- [x] P0: Usage query service (read SUM/COUNT indexes, per-bucket breakdown)
- [x] P0: Main server setup (ConnectRPC mux, FDB init, health check)
- [x] P0: Bazel BUILD files (gazelle, proto, Go targets)
- [x] P0: buf codegen setup

### Phase 2: Billing Engine
- [x] P0: Pricing model evaluation (flat, per-unit, tiered, volume, package, BPS) — all 6 models with tests
- [x] P0: Invoice generation — single FDB transaction: read aggregates, compute charges, apply credits, write invoice
- [x] P0: Credit system — grant, balance, drawdown by priority+expiry during invoicing
- [x] P0: Timestamp bucketing for aggregation (hourly buckets, day-level aggregation in API)
- [ ] P1: Invoice finalization and status transitions (draft → issued → paid → void)

### Phase 3: Exactly-Once Kafka Consumer
- [x] P0: Kafka consumer with franz-go — per-partition batch tx, JSON event parsing
- [x] P0: FDB-transactional offset storage (offsets in FDB, not Kafka's __consumer_offsets)
- [x] P0: Idempotency key dedup (pre-check before SaveRecord in consumer tx)
- [x] P0: Batch event processing (multiple events per FDB transaction, configurable batch size)
- [x] P0: Wired into main server (KAFKA_BROKERS + KAFKA_TOPIC env vars, graceful shutdown)
- [ ] P1: Consumer lag monitoring
- [ ] P1: Dead letter handling for malformed events

### Phase 4: Alerts & Real-Time
- [x] P1: Alert definitions (CRUD service + storage)
- [x] P1: Alert evaluation — automatic check after event ingestion, marks triggered when usage >= threshold
- [x] P1: E2E test: below-threshold (30 events, not triggered) + above (60 events, triggered)
- [ ] P2: Webhook delivery for triggered alerts

### Phase 5: Frontend
- [ ] P2: React app scaffolding (Vite + Tailwind + shadcn)
- [ ] P2: Customer dashboard
- [ ] P2: Meter configuration UI
- [ ] P2: Plan builder UI
- [ ] P2: Usage charts (real-time aggregation display)
- [ ] P2: Invoice viewer
- [ ] P2: Credit management UI
- [ ] P2: Bazel frontend build (rules_js, vite bundle)

### Phase 6: Hardening
- [x] P1: Integration tests with real FDB (testcontainers) — 29 tests across 4 targets
- [ ] P1: Chaos testing — commit_unknown with billing writes
- [x] P1: Edge cases: zero usage invoices, tiered pricing, credit depletion, multi-charge invoice, customer not found, contract lifecycle, alert CRUD
- [x] P1: Event dedup correctness: pre-check idempotency key BEFORE SaveRecord
- [x] P2: Benchmarks — EventIngest(1/10/100), UsageQuery (200us), InvoiceGeneration (1.27ms)

### Phase 7: Dynamic Meter Engine
- [x] P0: Runtime proto generation from meter config (dynamicpb + protodesc)
- [x] P0: Per-meter Record Layer stores with SUM/COUNT indexes
- [x] P0: Event ingestion into dynamic stores
- [x] P0: Usage query with group-by filter (prefix range for partial groups)
- [x] P0: Integration tests (5 unit + 1 E2E through ConnectRPC)
- [x] P1: Wire dynamic meter engine into EventService (dual-write: static + dynamic)
- [x] P1: Persist meter registrations across restarts (loaded from main store on startup)
- [x] P1: JSON property extraction for group-by values from properties_json
- [ ] P2: Benchmark: invoice generation latency

---

## Design Decisions Log

| Decision | Choice | Rationale |
|---|---|---|
| Storage engine | Record Layer (not plain fdbgo) | Atomic SUM/COUNT indexes for O(1) aggregation reads |
| Event ingestion | Kafka + FDB-transactional offsets | Exactly-once without relying on Kafka's at-least-once |
| Dedup strategy | UNIQUE index on idempotency_key | FDB enforces uniqueness, O(1) lookup |
| Aggregation | SUM/COUNT atomic indexes + timestamp bucketing | Real-time dashboards need sub-ms reads |
| Pricing calc | Application-level (not in FDB) | Complex tiered/volume logic doesn't map to FDB primitives |
| Invoice generation | Single FDB transaction per invoice | Atomicity: all line items + credit drawdown + invoice record |
| Frontend | React + Vite + Tailwind (same as channelmind-ai) | Reuse known stack, builds with Bazel |
| Kafka client | franz-go | Pure Go, consumer group support, manual offset management |
| API | ConnectRPC | Same as channelmind-ai, type-safe, browser-compatible |
