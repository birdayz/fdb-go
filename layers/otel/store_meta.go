package otel

import (
	"context"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// STORE_META rows of a tenant store (rfcs/relational-exporter-migration.md
// §3.1): the message layout, the message writer counter and the continuation
// signing key.

const storeMetaTable = "STORE_META"

// tableKey is the primary key of a row of table: a relational table's key
// leads with its record type key.
func tableKey(store *recordlayer.FDBRecordStore, op, table string, values ...tuple.TupleElement) (tuple.Tuple, error) {
	rt := store.GetRecordMetaData().GetRecordType(table)
	if rt == nil {
		return nil, failf("ownership", op, "%s is not in the store's metadata", table)
	}
	return recordTypeKey(rt, values...), nil
}

// recordTypeKey is the primary key of a row of rt whose key columns are
// values.
func recordTypeKey(rt *recordlayer.RecordType, values ...tuple.TupleElement) tuple.Tuple {
	if rt.PrimaryKeyHasRecordTypePrefix() {
		return append(tuple.Tuple{rt.GetRecordTypeKey()}, values...)
	}
	return tuple.Tuple(values)
}

// loadStoreMeta reads STORE_META row name in store's transaction; ok is false
// when the row is absent.
func loadStoreMeta(store *recordlayer.FDBRecordStore, name string) (row otelsql.StoreMeta, ok bool, err error) {
	const op = "store meta"
	pk, err := tableKey(store, op, storeMetaTable, name)
	if err != nil {
		return otelsql.StoreMeta{}, false, err
	}
	rec, err := store.LoadRecord(pk)
	if err != nil || rec == nil {
		return otelsql.StoreMeta{}, false, err
	}
	m := rec.Record.ProtoReflect()
	f := m.Descriptor().Fields()
	nameF, intF, bytesF, strF := f.ByName("NAME"), f.ByName("INT_VALUE"), f.ByName("BYTES_VALUE"), f.ByName("STR_VALUE")
	if nameF == nil || intF == nil || bytesF == nil || strF == nil {
		return otelsql.StoreMeta{}, false, failf("ownership", op, "%s lacks NAME, INT_VALUE, BYTES_VALUE or STR_VALUE", storeMetaTable)
	}
	if m.Get(nameF).String() != name {
		return otelsql.StoreMeta{}, false, failf("corruption", op, "%s row %q is stored under %q", storeMetaTable, m.Get(nameF).String(), name)
	}
	row.Name = name
	if m.Has(intF) {
		row.Int = proto.Int64(m.Get(intF).Int())
	}
	if m.Has(bytesF) {
		row.Bytes = append([]byte{}, m.Get(bytesF).Bytes()...)
	}
	if m.Has(strF) {
		row.Str = proto.String(m.Get(strF).String())
	}
	return row, true, nil
}

// saveStoreMeta writes row in store's transaction, replacing any row of its
// name.
func saveStoreMeta(store *recordlayer.FDBRecordStore, row otelsql.StoreMeta) error {
	rec, err := otelsql.BuildStoreMetaCompiled(row)
	if err != nil {
		return wrap("operation", "store meta", err)
	}
	_, err = store.SaveRecord(rec)
	return err
}

// publishStoreMeta stores candidate as row name unless one exists, in its own
// transaction under ctx's policy, and returns the stored row: an existing row
// wins. The read conflicts with a concurrent publisher's, so of two racing
// publishers one retries and returns the other's row. Callers draw candidate
// outside the retry loop.
func publishStoreMeta(ctx context.Context, s *tenantStore, name string, candidate otelsql.StoreMeta) (otelsql.StoreMeta, error) {
	candidate.Name = name
	var out otelsql.StoreMeta
	err := s.run(ctx, "store meta "+name, func(store *recordlayer.FDBRecordStore) error {
		row, ok, err := loadStoreMeta(store, name)
		if err != nil {
			return err
		}
		if ok {
			out = row
			return nil
		}
		out = candidate
		return saveStoreMeta(store, candidate)
	})
	return out, err
}

// The STORE_META("layout") values (D10): a store's GenAI message layout,
// published by its first Exporter and read by a Reader (Reader.Layout).
const (
	StoreLayoutNone     = "messages=none"
	StoreLayoutDeltas   = "messages=deltas"
	StoreLayoutFullText = "messages=fulltext"
)

// relStoreLayout is the STORE_META("layout") value of cfg's message storage
// (D10): the messages flags are the store's identity.
func relStoreLayout(cfg ExporterConfig) string {
	switch cfg.messageLayout() {
	case messagesFullText:
		return StoreLayoutFullText
	case messagesDeltas:
		return StoreLayoutDeltas
	}
	return StoreLayoutNone
}

// checkStoreLayout publishes layout on a fresh store and otherwise requires
// the stored one: an exporter configured for another layout is refused.
func checkStoreLayout(ctx context.Context, s *tenantStore, layout string) error {
	row, err := publishStoreMeta(ctx, s, otelsql.StoreMetaLayout, otelsql.StoreMeta{Str: proto.String(layout)})
	if err != nil {
		return err
	}
	if row.Str == nil || *row.Str != layout {
		stored := "<unset>"
		if row.Str != nil {
			stored = *row.Str
		}
		return failf("ownership", "store layout", "store %s/%s has layout %q, this exporter is configured for %q", s.databasePath, s.schema, stored, layout)
	}
	return nil
}

// storeMetaContinuationKeys is the continuation signing key in the
// STORE_META("continuation_hmac") row: read in the page's transaction,
// published in its own.
type storeMetaContinuationKeys struct{}

func (storeMetaContinuationKeys) load(store *recordlayer.FDBRecordStore) ([]byte, error) {
	row, _, err := loadStoreMeta(store, otelsql.StoreMetaContinuationHMAC)
	return row.Bytes, err
}

func (storeMetaContinuationKeys) publish(ctx context.Context, s *tenantStore, candidate []byte) ([]byte, error) {
	row, err := publishStoreMeta(ctx, s, otelsql.StoreMetaContinuationHMAC, otelsql.StoreMeta{Bytes: candidate})
	return row.Bytes, err
}
