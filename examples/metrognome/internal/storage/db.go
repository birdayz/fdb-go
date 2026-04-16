// Package storage provides FDB-backed record stores using fdb-record-layer-go.
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrAlreadyExists = errors.New("record already exists")
)

// DB provides access to all record stores backed by fdb-record-layer-go.
type DB struct {
	fdb      *rl.FDBDatabase
	metadata *rl.RecordMetaData
	ss       subspace.Subspace
}

// NewDB creates a DB with the metrognome record metadata.
func NewDB(fdb *rl.FDBDatabase) (*DB, error) {
	builder := rl.NewRecordMetaDataBuilder().
		SetRecords(storev1.File_metrognome_store_v1_store_proto)

	// Primary keys: RecordTypeKey() prefix for type discrimination in union store.

	// Customer: lookup by ID
	builder.GetRecordType("Customer").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Meter: lookup by ID, secondary index on slug (unique)
	builder.GetRecordType("Meter").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Plan: lookup by ID
	builder.GetRecordType("Plan").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Charge: lookup by plan_id + id
	builder.GetRecordType("Charge").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("plan_id"), rl.Field("id")))

	// Contract: lookup by id
	builder.GetRecordType("Contract").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// UsageEvent: lookup by id (idempotency_key has a separate unique index)
	builder.GetRecordType("UsageEvent").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Invoice: lookup by id
	builder.GetRecordType("Invoice").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Credit: lookup by id
	builder.GetRecordType("Credit").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Alert: lookup by id
	builder.GetRecordType("Alert").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// KafkaOffset: lookup by topic + partition
	builder.GetRecordType("KafkaOffset").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("topic"), rl.Field("partition")))

	// DeadLetter: lookup by id
	builder.GetRecordType("DeadLetter").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// User: lookup by id (gh_<github_id>)
	builder.GetRecordType("User").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Session: lookup by id (random token)
	builder.GetRecordType("Session").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// ApiKey: lookup by id
	builder.GetRecordType("ApiKey").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// --- Secondary indexes ---

	// Meter slug must be unique
	builder.AddIndex("Meter", rl.NewIndex("meter_by_slug", rl.Field("slug")).SetUnique())

	// Customer by external_id
	builder.AddIndex("Customer", rl.NewIndex("customer_by_external_id", rl.Field("external_id")))

	// Charges for a plan
	builder.AddIndex("Charge", rl.NewIndex("charge_by_plan", rl.Field("plan_id")))

	// Contracts by customer
	builder.AddIndex("Contract", rl.NewIndex("contract_by_customer", rl.Field("customer_id")))

	// UsageEvent by idempotency key (unique, for dedup)
	builder.AddIndex("UsageEvent", rl.NewIndex("event_by_idempotency_key", rl.Field("idempotency_key")).SetUnique())

	// UsageEvent by customer + meter + timestamp (for range queries during invoice generation)
	builder.AddIndex("UsageEvent", rl.NewIndex("event_by_customer_meter_time",
		rl.Concat(rl.Field("customer_id"), rl.Field("meter_slug"), rl.Field("timestamp_ms"))))

	// Invoice by customer
	builder.AddIndex("Invoice", rl.NewIndex("invoice_by_customer", rl.Field("customer_id")))

	// Credit by customer + expiry (for ordered drawdown)
	builder.AddIndex("Credit", rl.NewIndex("credit_by_customer",
		rl.Concat(rl.Field("customer_id"), rl.Field("priority"), rl.Field("expires_at"))))

	// Alert by customer + meter
	builder.AddIndex("Alert", rl.NewIndex("alert_by_customer",
		rl.Concat(rl.Field("customer_id"), rl.Field("meter_slug"))))

	// ApiKey by key_hash (unique, for auth lookup)
	builder.AddIndex("ApiKey", rl.NewIndex("apikey_by_hash", rl.Field("key_hash")).SetUnique())

	// --- Aggregate indexes ---

	// SUM of event values grouped by (customer_id, meter_slug, timestamp_bucket)
	// This gives O(1) reads for "total usage for customer X on meter Y in bucket Z"
	builder.AddIndex("UsageEvent", rl.NewSumIndex("usage_sum",
		rl.GroupBy(rl.Field("value"), rl.Field("customer_id"), rl.Field("meter_slug"), rl.Field("timestamp_bucket"))))

	// COUNT of events grouped by (customer_id, meter_slug, timestamp_bucket)
	builder.AddIndex("UsageEvent", rl.NewCountIndex("usage_count",
		rl.GroupBy(rl.EmptyKey(), rl.Field("customer_id"), rl.Field("meter_slug"), rl.Field("timestamp_bucket"))))

	// Product: lookup by ID
	builder.GetRecordType("Product").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// RateCard: lookup by ID
	builder.GetRecordType("RateCard").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Rate: lookup by ID, indexed by rate_card_id
	builder.GetRecordType("Rate").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.AddIndex("Rate", rl.NewIndex("rate_by_rate_card",
		rl.Field("rate_card_id")))

	builder.SetRecordCountKey(rl.EmptyKey())

	md, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("build record metadata: %w", err)
	}

	db := &DB{
		fdb:      fdb,
		metadata: md,
		ss:       subspace.Sub("metrognome"),
	}

	// CreateOrOpen once at startup — ensures store exists, header written,
	// and checkPossiblyRebuild runs if metadata version changed.
	// Subsequent per-request Open() calls skip this (SetSkipPossiblyRebuild).
	_, err = fdb.Run(context.Background(), func(rtx *rl.FDBRecordContext) (any, error) {
		_, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(db.ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		return nil, fmt.Errorf("create/open store: %w", err)
	}

	return db, nil
}

// FDB returns the underlying FDBDatabase.
func (d *DB) FDB() *rl.FDBDatabase { return d.fdb }

// MetaData returns the record metadata.
func (d *DB) MetaData() *rl.RecordMetaData { return d.metadata }

// Subspace returns the store subspace.
func (d *DB) Subspace() subspace.Subspace { return d.ss }

func (d *DB) Customers() *CustomerStore       { return &CustomerStore{db: d} }
func (d *DB) Meters() *MeterStore             { return &MeterStore{db: d} }
func (d *DB) Plans() *PlanStore               { return &PlanStore{db: d} }
func (d *DB) Charges() *ChargeStore           { return &ChargeStore{db: d} }
func (d *DB) Contracts() *ContractStore       { return &ContractStore{db: d} }
func (d *DB) Events() *EventStore             { return &EventStore{db: d} }
func (d *DB) Invoices() *InvoiceStore         { return &InvoiceStore{db: d} }
func (d *DB) Credits() *CreditStore           { return &CreditStore{db: d} }
func (d *DB) Alerts() *AlertStore             { return &AlertStore{db: d} }
func (d *DB) KafkaOffsets() *KafkaOffsetStore { return &KafkaOffsetStore{db: d} }
func (d *DB) DeadLetters() *DeadLetterStore   { return &DeadLetterStore{db: d} }
func (d *DB) Users() *UserStore               { return &UserStore{db: d} }
func (d *DB) Sessions() *SessionStore         { return &SessionStore{db: d} }
func (d *DB) Products() *ProductStore         { return &ProductStore{db: d} }
func (d *DB) RateCards() *RateCardStore       { return &RateCardStore{db: d} }
func (d *DB) Rates() *RateStore               { return &RateStore{db: d} }
func (d *DB) ApiKeys() *ApiKeyStore           { return &ApiKeyStore{db: d} }

// run executes fn within a transaction with an open FDBRecordStore.
func (d *DB) run(ctx context.Context, fn func(*rl.FDBRecordStore) (any, error)) (any, error) {
	return d.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(d.metadata).
			SetSubspace(d.ss).
			Build()
		if err != nil {
			return nil, err
		}
		return fn(store)
	})
}

// runInStore is like run but also provides the FDBRecordContext for multi-store transactions.
func (d *DB) runInStore(ctx context.Context, fn func(*rl.FDBRecordContext, *rl.FDBRecordStore) (any, error)) (any, error) {
	return d.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(d.metadata).
			SetSubspace(d.ss).
			Build()
		if err != nil {
			return nil, err
		}
		return fn(rtx, store)
	})
}

// rtk returns the record type key for use in primary key tuples.
func (d *DB) rtk(name string) int64 {
	return int64(d.metadata.GetRecordType(name).RecordTypeIndex)
}

// pk builds a primary key tuple with the record type key prefix.
func (d *DB) pk(typeName string, fields ...any) tuple.Tuple {
	t := make(tuple.Tuple, 0, 1+len(fields))
	t = append(t, d.rtk(typeName))
	for _, f := range fields {
		t = append(t, f)
	}
	return t
}
