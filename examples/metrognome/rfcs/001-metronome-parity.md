# RFC 001: Metrognome — Full Metronome Parity

**Status:** Draft
**Author:** Claude + Johannes
**Date:** 2026-04-15

## Motivation

Metrognome is a usage-based billing engine built on FDB Record Layer Go. The current implementation covers basic CRUD, event ingestion, and simple pricing. This RFC defines what's needed for full Metronome feature parity — a production-grade SaaS billing platform.

## Core Architecture Insight

Metronome's architecture is built on a key decoupling:

```
Event → Billable Metric → Product → Rate Card → Contract → Invoice
```

**Products don't have prices.** Prices live on **rate cards**. Rate cards cascade to all contracts unless overridden. This enables:
- Change a price once, update all customers
- Per-customer discounts via contract overrides
- Custom pricing units (tokens, credits) separate from fiat

## Data Model

### Entity Hierarchy

```
Customer
  └─ Contract (references Rate Card)
       ├─ Overrides (per-product price adjustments)
       ├─ Commits (prepaid/postpaid spending commitments)
       ├─ Credits (free usage allowances)
       ├─ Subscriptions (recurring seat-based charges)
       ├─ Scheduled Charges (one-time fixed charges)
       └─ Usage Filters (route events to this contract)

Rate Card (shared across contracts)
  └─ Rates (per-product pricing, versioned over time)
       └─ Product (linked to Billable Metric)
            └─ Billable Metric (query over Events)

Events (raw usage data, immutable)
Invoices (generated from contracts + usage)
Alerts (threshold notifications on usage/spend/balance)
```

### New Record Types Needed

| Type | Status | Description |
|------|--------|-------------|
| Customer | EXISTS | Name, external_id, ingest_aliases, custom_fields |
| Meter/BillableMetric | EXISTS (partial) | Needs: property filters, group keys, SQL metrics |
| Product | NEW | Type (usage/subscription/composite/fixed), linked metric, quantity_conversion, quantity_rounding, pricing_group_key, presentation_group_key |
| RateCard | NEW | Name, aliases, fiat_credit_type, credit_type_conversions |
| Rate | NEW | rate_card_id, product_id, rate_type, price, tiers, starting_at, ending_before, pricing_group_values |
| Contract | NEW (partial) | rate_card_id, customer_id, starting_at, ending_before, usage_statement_schedule, net_payment_terms, overrides, usage_filters |
| Override | NEW | contract_id, product targeting (id/tags/specifiers), type (multiplier/overwrite/tiered), priority |
| Commit | NEW | Type (prepaid/postpaid), product_id, access_schedule, invoice_schedule, priority, rollover_fraction, applicable targeting |
| Credit | EXISTS (basic) | Needs: access_schedule, priority, applicable targeting, specifiers |
| Invoice | EXISTS (basic) | Needs: line item types, credit application, grace period, void/regenerate |
| Alert | EXISTS (basic) | Needs: all 10+ threshold types, offset alerts, system notifications |
| Plan/Package | NEW | Reusable contract templates with relative scheduling |
| Subscription | NEW | Recurring charges with billing_frequency, quantity, proration |
| ScheduledCharge | NEW | One-time or recurring fixed charges |
| CustomFieldKey | NEW | Metadata schema definitions |

### New Indexes Needed

| Index | Type | Purpose |
|-------|------|---------|
| product_by_rate_card | VALUE | List products on a rate card |
| rate_by_product_card | VALUE | Lookup rate for product + rate card + time |
| contract_by_customer_time | VALUE | Active contracts for customer at time T |
| override_by_contract | VALUE | Overrides for a contract |
| commit_by_contract | VALUE | Commits/credits for a contract |
| invoice_by_customer_period | VALUE | Invoice lookup by period |
| alert_by_customer_type | VALUE | Alerts by customer and type |
| event_by_type_time | VALUE | Event queries for SQL metrics |
| customer_by_alias | VALUE (UNIQUE) | Ingest alias → customer mapping |

## Pricing Engine

### Rate Types

| Type | Current | Needed |
|------|---------|--------|
| Flat (per-unit) | YES | ✓ |
| Tiered | YES | ✓ |
| Volume | YES | ✓ |
| Package | YES | ✓ |
| Percentage/BPS | YES | ✓ |
| Subscription | NO | Recurring charges with frequency + proration |
| Tiered Percentage | NO | Percentage-based tiers with minimum |
| Dimensional | NO | Different rates per pricing_group_key values |

### Override Types (NEW)

| Type | Description |
|------|-------------|
| Multiplier | Multiply rate card price (e.g., 0.9 = 10% discount). Dynamic — follows rate card changes. |
| Overwrite | Fixed price, ignores rate card. Supports all rate types. |
| Tiered Multiplier | Different multipliers per quantity tier. |

Override resolution: Overwrite > Multiplier. Within multipliers: LOWEST_MULTIPLIER (default) or EXPLICIT priority.

### Commit/Credit Burn-Down (NEW)

Priority order:
1. Rollover commits (postpaid before prepaid)
2. Prepaid commits and credits (by priority → cost basis → product scope → time)
3. Postpaid commits

Commits apply at the **line-item level**, not invoice aggregate. Same usage applies to at most one commit/credit.

### Custom Pricing Units (NEW)

Decouple pricing from fiat currency. A rate card defines conversions (e.g., 1 token = $0.001). Products priced in tokens, invoices show fiat equivalent.

## Billable Metrics (Metering)

### Current State
- Dynamic proto generation per meter
- SUM and COUNT aggregation
- Group-by dimensions
- 84K events/sec ingestion

### Needed

**Streaming metrics (priority):**
- [ ] Property filters: `in_values`, `not_in_values`, `exists`
- [ ] MAX aggregation
- [ ] LATEST aggregation (last value wins per group)
- [ ] UNIQUE aggregation (count distinct)
- [ ] Presentation group keys (invoice breakdown without affecting pricing)
- [ ] Pricing group keys (different rates per dimension)

**SQL metrics (lower priority):**
- [ ] SQL query engine over events table
- [ ] COUNT DISTINCT, MIN, AVG, EARLIEST
- [ ] DATE_TRUNC, CAST, CASE WHEN
- [ ] Math operators on columns

## Event Ingestion

### Current State
- `InsertBatch` with `Build()` — 84K events/sec
- Unique event_id PK per event
- SUM/COUNT atomic indexes

### Needed
- [ ] `transaction_id` as explicit idempotency key (1-128 chars, 34-day dedup window)
- [ ] Ingest aliases → customer_id resolution
- [ ] Batch endpoint (up to 100 events per request)
- [ ] Backdated events (up to 34 days)
- [ ] Future event rejection (>24 hours ahead)
- [ ] Event type filtering for billable metrics
- [ ] Property value filtering (`in_values`, `not_in_values`)

## Invoicing

### Current State
- Basic invoice generation from SUM aggregates
- Single transaction per invoice
- 6 pricing models

### Needed

**Invoice lifecycle:**
- [ ] DRAFT → grace period (configurable, default 24h) → FINALIZED
- [ ] VOID and regenerate
- [ ] Real-time draft invoice updates as events arrive
- [ ] Grace period for late-arriving events

**Line item types:**
- [ ] `usage` — consumption charges
- [ ] `subscription` — recurring fees
- [ ] `scheduled` — one-time charges
- [ ] `commit_purchase` — prepaid commitment payments
- [ ] `applied_commit_or_credit` — negative line items (deductions)
- [ ] `cpu_conversion` — pricing unit conversions

**Credit application:**
- [ ] Apply commits/credits at line-item level
- [ ] Priority-based draw-down across multiple commits
- [ ] Track remaining balance in real-time

**Corrections:**
- [ ] Negative quantity events for current period corrections
- [ ] Void → resubmit → regenerate for finalized invoices

## Contracts

### Current State
- Not implemented (plans serve as lightweight contracts)

### Needed

- [ ] Contract CRUD with rate card reference
- [ ] Contract overrides (multiplier, overwrite, tiered)
- [ ] Usage filters (route events to specific contracts)
- [ ] Contract transitions (supersede, renewal)
- [ ] Multi-contract customers
- [ ] Usage statement schedule (monthly/quarterly/annual/weekly)
- [ ] Net payment terms
- [ ] Uniqueness key for idempotent creation

### Commits & Credits
- [ ] Prepaid commits with access_schedule + invoice_schedule
- [ ] Postpaid commits with true-up invoicing
- [ ] Recurring commits/credits (auto-generated per period)
- [ ] Rollover fractions on contract transitions
- [ ] Applicable product/tag targeting
- [ ] Specifier-based targeting (advanced AND/OR logic)

## Alerts & Notifications

### Current State
- Basic threshold alerts on usage

### Needed

**Threshold alerts (real-time):**
- [ ] `spend_threshold_reached`
- [ ] `usage_threshold_reached`
- [ ] `invoice_total_reached`
- [ ] `low_remaining_commit_balance_reached` ($ and %)
- [ ] `low_remaining_credit_balance_reached` ($ and %)
- [ ] `low_remaining_combined_balance_reached`

**System notifications:**
- [ ] Contract lifecycle (create, start, edit, end)
- [ ] Commit/credit lifecycle (create, segment start/end)
- [ ] Invoice finalized
- [ ] Billing provider errors

**Offset notifications:**
- [ ] Triggered relative to known dates (e.g., 30 days before commit end)

**Delivery:**
- [ ] Webhook with HMAC-SHA256 signatures
- [ ] Retry with exponential backoff

## API Design

### Current State
- ConnectRPC (proto-first, browser-compatible)
- 8 services

### Needed

**New services:**
- [ ] ProductService — CRUD for products (usage/subscription/composite/fixed)
- [ ] RateCardService — CRUD for rate cards + rates
- [ ] ContractService — CRUD for contracts, overrides, commits, credits
- [ ] InvoiceService — List, get, void, regenerate, breakdowns
- [ ] UsageService — Query usage with time windowing and group breakdowns
- [ ] NotificationService — Configure webhooks, list notifications
- [ ] PackageService — Reusable contract templates
- [ ] CustomFieldService — Metadata key/value management
- [ ] DashboardService — Embeddable dashboard URLs

**API patterns to adopt:**
- [ ] Cursor-based pagination (limit + next_page token) — already have continuation tokens
- [ ] Idempotency-Key header (24h retention)
- [ ] Uniqueness keys on resources (409 on conflict)
- [ ] Custom field support on all entities

## User Interface

### Current State
- Login page (GitHub OAuth)
- Dashboard (placeholder)
- Customers list + create
- Meters list + create
- Plans list
- Events viewer

### Needed — High Fidelity UI

**Navigation (sidebar):**
```
Customers
Offering
  ├─ Products
  ├─ Billable Metrics
  ├─ Rate Cards
  ├─ Pricing Units
  └─ Packages
Connections
  ├─ API Tokens & Webhooks
  ├─ Events Explorer
  └─ Notifications
Settings
```

**Customer Detail Page:**
- Overview tab: name, ID, aliases, custom fields, active contracts summary
- Contracts tab: list contracts, create new, view details
- Invoices tab: current draft + historical, line-item breakdown
- Usage tab: real-time usage charts with time range selector, group-by breakdown
- Commits & Credits tab: balances, access schedules, burn-down visualization

**Product Management:**
- Create product wizard: type selection → metric linking → quantity config
- Four product types with type-specific forms
- Tag management
- Quantity conversion and rounding config

**Rate Card Management:**
- Rate card list with alias management
- Rate editor: per-product pricing with dimensional support
- Price schedule timeline (temporal rates)
- Credit type / pricing unit configuration

**Contract Builder:**
- Step-by-step wizard: select customer → rate card → overrides → commits → credits
- Override editor with targeting (product, tags, specifiers)
- Commit/credit configuration with schedule visualization
- Usage filter setup
- Contract timeline visualization

**Invoice Viewer:**
- Draft invoice with real-time updates
- Line-item breakdown with commit/credit application
- Invoice status badges (DRAFT, FINALIZED, VOID)
- Period selector
- Export (PDF, CSV)

**Billable Metric Editor:**
- Visual filter builder (event type, property filters)
- Aggregation type selector with preview
- Group key configuration
- SQL editor for advanced metrics
- Live preview with sample data

**Events Explorer:**
- Search by customer, event type, time range
- Property inspection
- Dedup status indicator
- CSV export

**Usage Dashboard:**
- Time-series charts (line, bar, area)
- Group-by breakdown tables
- Time window selector (hourly, daily, monthly)
- Metric selector

**Alert Configuration:**
- Threshold type selector
- Value/percentage input
- Webhook URL configuration
- Alert history

### UI Technology
- React 19 + Vite + Tailwind CSS + shadcn/ui (current stack, keep it)
- Charts: recharts or @observablehq/plot
- Tables: @tanstack/react-table
- Forms: react-hook-form + zod validation
- Date handling: date-fns

## Implementation Phases

### Phase 1: Foundation (2 weeks)
- Products (4 types) + Rate Cards + Rates
- Contract CRUD with rate card reference
- Basic overrides (multiplier, overwrite)
- Enhanced invoice generation with line item types
- UI: Product + Rate Card management pages

### Phase 2: Commitments (2 weeks)
- Prepaid/postpaid commits
- Credit enhancements (access schedule, priority, targeting)
- Commit/credit burn-down engine
- Real-time draft invoices
- UI: Contract builder, commit/credit management

### Phase 3: Advanced Pricing (1 week)
- Subscription products (recurring charges)
- Dimensional pricing (pricing group keys)
- Tiered percentage rates
- Override specifiers (advanced targeting)
- Custom pricing units

### Phase 4: Metering (1 week)
- Property filters on billable metrics
- MAX, LATEST, UNIQUE aggregations
- Presentation group keys
- Event type filtering

### Phase 5: Notifications & Polish (1 week)
- All threshold alert types
- Webhook delivery with HMAC signatures
- System notifications
- Embeddable dashboards
- Events explorer improvements

### Phase 6: Enterprise (ongoing)
- SQL billable metrics
- Customer hierarchy (parent-child billing)
- Contract packages/templates
- Billing provider integrations (Stripe, marketplace)
- Revenue recognition (ASC 606)
- Data warehouse export

## Non-Goals (for now)
- Multi-region deployment
- Marketplace integrations (AWS/Azure/GCP)
- NetSuite integration
- SOC 2 / compliance features
- Multi-tenant isolation (single-tenant for now)

## FDB Record Layer Advantages

Our FDB-based architecture gives us structural advantages:
1. **Atomic indexes** — SUM/COUNT aggregates are O(1) reads, maintained transactionally
2. **ACID transactions** — Invoice generation, credit draw-down, and event ingestion are atomic
3. **Horizontal scaling** — FDB scales writes linearly with nodes
4. **Pure Go client** — No CGo, 84K events/sec ingestion verified
5. **Schema evolution** — Record Layer handles index rebuilds on metadata changes

## Open Questions

1. Should we keep ConnectRPC or switch to REST for Metronome API compatibility?
2. How to handle the 34-day dedup window efficiently at scale? (Bloom filter? TTL index?)
3. Should SQL metrics run against FDB directly or export to a columnar store?
4. Multi-tenant data isolation model — separate FDB subspaces or separate clusters?
