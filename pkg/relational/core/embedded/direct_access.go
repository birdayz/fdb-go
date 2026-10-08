package embedded

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/protoname"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// DirectAccess returns the connection's primary-key-direct access to its
// schema's tables, bypassing the SQL compiler: Java's
// RelationalDirectAccessStatement as EmbeddedRelationalStatement implements it
// (EmbeddedRelationalStatement.java:107-348). Each call runs in the open
// explicit transaction, or in its own transaction committed when it succeeds
// and rolled back when it fails (ensureTransaction, :315-345).
//
// A table is named bare, in the connection's schema, or as schema.table. Only
// the connection's own schema is served: the metadata and store this
// connection holds are its schema's, and another schema's table is refused
// rather than read through them.
func (c *EmbeddedConnection) DirectAccess() api.DirectAccessStatement {
	return &directAccessStatement{c: c}
}

type directAccessStatement struct{ c *EmbeddedConnection }

// directTable is one direct-access call's resolved table and open store.
type directTable struct {
	name  string
	rt    *recordlayer.RecordType
	store *recordlayer.FDBRecordStore
}

// run resolves tableName and runs fn over its store, in the open explicit
// transaction or in one of its own.
func (s *directAccessStatement) run(ctx context.Context, tableName string, opts *api.Options, fn func(directTable, *api.Options) (any, error)) (any, error) {
	c := s.c
	if c.closed.Load() {
		return nil, api.NewError(api.ErrCodeStatementClosed, "statement is closed")
	}
	merged := c.Options()
	if opts != nil {
		var err error
		if merged, err = merged.WithChild(opts); err != nil {
			return nil, err
		}
	}
	// A continuation that carries a query's binding hash is not one a direct
	// access minted (validateBindingHash, :258-262). Go's query continuations
	// carry no hash, so the test is by kind: a direct-access continuation, or
	// one at the beginning or the end, is accepted; any other is a query's.
	if cont, ok := merged.Get(api.OptContinuation).(api.Continuation); ok && cont != nil && len(cont.ExecutionState()) > 0 {
		if _, direct := cont.(*directContinuation); !direct {
			return nil, api.NewError(api.ErrCodeInvalidContinuation, "Continuation doesn't match direct access APIs.")
		}
	}
	schemaName, table, err := s.schemaAndTable(tableName)
	if err != nil {
		return nil, err
	}
	if schemaName != c.sess.Schema {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"direct access to table %q of schema %q from a connection to schema %q", table, schemaName, c.sess.Schema)
	}
	ss, err := c.sess.Keyspace.SchemaSubspaceIn(ctx, c.sess.DB, c.sess.DBPath, c.sess.Schema)
	if err != nil {
		return nil, err
	}

	tx, own := c.activeTx, false
	if tx == nil {
		if tx, err = c.beginTransaction(); err != nil {
			return nil, translateFDBError(err)
		}
		own = true
	}
	result, err := c.runInCapturedTx(ctx, tx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		// Inside the transaction, so the metadata is the transaction's.
		if err := c.ensureMetaData(ctx); err != nil {
			return nil, err
		}
		rt := c.cachedMetaData().GetRecordType(table)
		if rt == nil {
			// RecordTypeTable.loadRecordType: the metadata's refusal, UNDEFINED_SCHEMA.
			return nil, api.NewErrorf(api.ErrCodeUndefinedSchema, "Unknown record type %s", table)
		}
		store, err := c.storeIn(rctx, tx, ss)
		if err != nil {
			return nil, err
		}
		return fn(directTable{name: table, rt: rt, store: store}, merged)
	})
	if own {
		if err != nil {
			_ = tx.Rollback()
		} else if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, translateFDBError(err)
	}
	return result, nil
}

// schemaAndTable splits a schema.table name, as getSchemaAndTable does
// (:265-282): a bare name is in the connection's schema.
func (s *directAccessStatement) schemaAndTable(tableName string) (string, string, error) {
	schemaName, table := s.c.sess.Schema, tableName
	if i := strings.IndexByte(tableName, '.'); i >= 0 {
		schemaName, table = tableName[:i], tableName[i+1:]
	}
	if schemaName == "" {
		return "", "", api.NewError(api.ErrCodeInvalidParameter, "Invalid table format")
	}
	return schemaName, table, nil
}

// ExecuteInsert inserts each struct as a record of tableName and returns how
// many it wrote (executeInsert, :160-187). A struct whose primary key exists
// is refused unless REPLACE_ON_DUPLICATE_PK is set, in which case it replaces
// the record (BackingRecordStore.insert, :170-189).
func (s *directAccessStatement) ExecuteInsert(ctx context.Context, tableName string, data []api.Struct, opts *api.Options) (int64, error) {
	if len(data) == 0 {
		return 0, nil
	}
	n, err := s.run(ctx, tableName, opts, func(t directTable, o *api.Options) (any, error) {
		replace, _ := o.Get(api.OptReplaceOnDuplicatePK).(bool)
		var count int64
		for _, st := range data {
			msg, err := structToRecord(st, t.rt.Descriptor)
			if err != nil {
				return nil, err
			}
			if replace {
				_, err = t.store.SaveRecord(msg)
			} else {
				_, err = t.store.InsertRecord(msg)
			}
			var exists *recordlayer.RecordAlreadyExistsError
			if errors.As(err, &exists) {
				return nil, api.WrapErrorf(err, api.ErrCodeUniqueConstraintViolation,
					"Duplicate primary key for message (%v) on table <%s>", msg, t.name)
			}
			if err != nil {
				return nil, err
			}
			count++
		}
		return count, nil
	})
	if err != nil {
		return 0, err
	}
	return n.(int64), nil
}

// ExecuteGet returns the record whose complete primary key is key, or no row
// (executeGet, :135-157). With INDEX_HINT, key is the named index's complete
// key and the row is the record of its first entry
// (BackingRecordStore.getFromIndex, :107-131).
func (s *directAccessStatement) ExecuteGet(ctx context.Context, tableName string, key *api.KeySet, opts *api.Options) (api.ResultSet, error) {
	rows, err := s.run(ctx, tableName, opts, func(t directTable, o *api.Options) (any, error) {
		index, err := directSourceIndex(t, o)
		if err != nil {
			return nil, err
		}
		if index != nil {
			return getDirectFromIndex(ctx, t, index, keySetMap(key))
		}
		pk, err := buildDirectKey(t.rt, t.rt.PrimaryKey, "primary key of <"+t.name+">", keySetMap(key), true)
		if err != nil {
			return nil, err
		}
		rec, err := t.store.LoadRecord(pk)
		if err != nil || rec == nil {
			return directRows{desc: t.rt.Descriptor, after: directBeginContinuation()}, err
		}
		return directRows{desc: t.rt.Descriptor, records: []proto.Message{rec.Record}, after: directEndContinuation()}, nil
	})
	if err != nil {
		return nil, err
	}
	return newDirectResultSet(rows.(directRows))
}

// directRows is a read's records, the descriptor they are read with, and the
// continuation past the last of them; or, for an index scan, its entries as
// tuples described by st.
type directRows struct {
	desc    protoreflect.MessageDescriptor
	records []proto.Message
	st      *api.StructType
	tuples  []tuple.Tuple
	after   *directContinuation
}

// ExecuteScan returns the records of tableName whose primary key begins with
// keyPrefix, in primary-key order (executeScan, :108-132), resuming from the
// CONTINUATION option and stopping after MAX_ROWS records
// (QueryPropertiesUtils.getScanProperties: the returned-row limit). The page
// is read in this call's transaction and materialized; its continuation, once
// the rows are consumed, resumes the scan where the page stopped, or is the
// end when the scan is exhausted. Java streams the page out of an open
// transaction instead; the rows and continuations are the same.
func (s *directAccessStatement) ExecuteScan(ctx context.Context, tableName string, keyPrefix *api.KeySet, opts *api.Options) (api.ResultSet, error) {
	rows, err := s.run(ctx, tableName, opts, func(t directTable, o *api.Options) (any, error) {
		index, err := directSourceIndex(t, o)
		if err != nil {
			return nil, err
		}
		cont, _ := o.Get(api.OptContinuation).(api.Continuation)
		if index != nil {
			return scanDirectIndexPage(ctx, t, index, keySetMap(keyPrefix), cont, directRowLimit(o))
		}
		prefix, err := buildDirectKey(t.rt, t.rt.PrimaryKey, "primary key of <"+t.name+">", keySetMap(keyPrefix), false)
		if err != nil {
			return nil, err
		}
		records, after, err := scanDirectPage(ctx, t, prefix, cont, directRowLimit(o))
		return directRows{desc: t.rt.Descriptor, records: records, after: after}, err
	})
	if err != nil {
		return nil, err
	}
	return newDirectResultSet(rows.(directRows))
}

// directRowLimit is MAX_ROWS as a returned-row limit, zero for none
// (QueryPropertiesUtils.getExecuteProperties).
func directRowLimit(o *api.Options) int {
	switch n := o.Get(api.OptMaxRows).(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}

// ExecuteDelete deletes the record whose complete primary key is key and
// returns how many it deleted (executeDelete, :189-217).
func (s *directAccessStatement) ExecuteDelete(ctx context.Context, tableName string, key *api.KeySet, opts *api.Options) (int64, error) {
	n, err := s.run(ctx, tableName, opts, func(t directTable, _ *api.Options) (any, error) {
		pk, err := buildDirectKey(t.rt, t.rt.PrimaryKey, "primary key of <"+t.name+">", keySetMap(key), true)
		if err != nil {
			return nil, err
		}
		deleted, err := t.store.DeleteRecord(pk)
		if err != nil || !deleted {
			return int64(0), err
		}
		return int64(1), nil
	})
	if err != nil {
		return 0, err
	}
	return n.(int64), nil
}

// ExecuteDeleteRange deletes every record of tableName whose primary key
// begins with keyPrefix (executeDeleteRange, :219-255): a complete key deletes
// the one record; a prefix is a delete-where over it, and where an index
// cannot be cleared by that prefix the records are scanned and deleted one by
// one, as the target falls back.
func (s *directAccessStatement) ExecuteDeleteRange(ctx context.Context, tableName string, keyPrefix *api.KeySet, opts *api.Options) (int64, error) {
	n, err := s.run(ctx, tableName, opts, func(t directTable, _ *api.Options) (any, error) {
		columns := keySetMap(keyPrefix)
		prefix, err := buildDirectKey(t.rt, t.rt.PrimaryKey, "primary key of <"+t.name+">", columns, false)
		if err != nil {
			return nil, err
		}
		if len(prefix) == t.rt.PrimaryKey.ColumnSize() {
			deleted, err := t.store.DeleteRecord(prefix)
			if err != nil || !deleted {
				return int64(0), err
			}
			return int64(1), nil
		}
		if len(prefix) == 0 {
			// BackingRecordStore.deleteRange: an empty prefix over a key without
			// a record-type prefix names no range.
			return nil, api.NewError(api.ErrCodeInvalidParameter,
				"Delete range with empty key range is only supported on tables with RecordTypeKeys")
		}
		records, err := scanDirectRecords(ctx, t, prefix)
		if err != nil {
			return nil, err
		}
		err = t.store.DeleteRecordsWhere(prefix)
		var invalid *recordlayer.QueryInvalidExpressionError
		if errors.As(err, &invalid) {
			for _, rec := range records {
				pk, keyErr := directRecordKey(t, rec)
				if keyErr != nil {
					return nil, keyErr
				}
				if deleted, delErr := t.store.DeleteRecord(pk); delErr != nil {
					return nil, delErr
				} else if !deleted {
					return nil, api.NewError(api.ErrCodeInternalError, "Cannot delete record during fallback deleteRange")
				}
			}
			err = nil
		}
		if err != nil {
			return nil, err
		}
		return int64(len(records)), nil
	})
	if err != nil {
		return 0, err
	}
	return n.(int64), nil
}

// directSourceIndex is getSourceScannable (:299-316): with no INDEX_HINT the
// table is the source; otherwise the hint names one of the table's own
// indexes (RecordTypeTable.getAvailableIndexes: the record type's
// single-type indexes), and a name that is none of them is UNDEFINED_INDEX.
func directSourceIndex(t directTable, o *api.Options) (*recordlayer.Index, error) {
	hint, ok := o.Get(api.OptIndexHint).(string)
	if !ok {
		return nil, nil
	}
	for _, index := range t.rt.GetIndexes() {
		if index.Name == hint {
			return index, nil
		}
	}
	return nil, api.NewErrorf(api.ErrCodeUndefinedIndex, "Unknown index: <%s> on type <%s>", hint, t.name)
}

// directIndexKeyName is the index's name in KeyBuilder's messages
// (RecordStoreIndex.getKeyBuilder).
func directIndexKeyName(index *recordlayer.Index) string {
	return "index: <" + index.Name + ">"
}

// getDirectFromIndex is RecordStoreIndex.get over
// BackingRecordStore.getFromIndex (:107-131): the complete index key, the
// first entry under it, and that entry's record, an orphan entry an error
// (IndexOrphanBehavior.ERROR). The row is the table's.
func getDirectFromIndex(ctx context.Context, t directTable, index *recordlayer.Index, columns map[string]any) (directRows, error) {
	key, err := buildDirectKey(t.rt, index.RootExpression, directIndexKeyName(index), columns, true)
	if err != nil {
		return directRows{}, err
	}
	props := recordlayer.NewScanProperties(recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1))
	cursor := t.store.ScanIndexRecords(index.Name, recordlayer.TupleRangeAllOf(key), nil, props)
	defer cursor.Close()
	res, err := cursor.OnNext(ctx)
	if err != nil {
		return directRows{}, err
	}
	if !res.HasNext() || res.GetValue().Record == nil {
		return directRows{desc: t.rt.Descriptor, after: directBeginContinuation()}, nil
	}
	return directRows{
		desc: t.rt.Descriptor, records: []proto.Message{res.GetValue().Record.Record},
		after: directEndContinuation(),
	}, nil
}

// scanDirectIndexPage is RecordStoreIndex.openScan over
// BackingRecordStore.scanIndex (:203-213): one BY_VALUE page of the index
// entries whose key begins with the prefix, resumed from cont and limited to
// limit entries. Each row is the entry's key then its value
// (ImmutableKeyValue), described by the index's fields
// (RecordStoreIndex.getMetaData).
func scanDirectIndexPage(ctx context.Context, t directTable, index *recordlayer.Index, columns map[string]any, cont api.Continuation, limit int) (directRows, error) {
	prefix, err := buildDirectKey(t.rt, index.RootExpression, directIndexKeyName(index), columns, false)
	if err != nil {
		return directRows{}, err
	}
	st, err := directIndexStructType(t, index)
	if err != nil {
		return directRows{}, err
	}
	rows := directRows{st: st}
	var state []byte
	if cont != nil {
		state = cont.ExecutionState()
		if state != nil && len(state) == 0 {
			rows.after = directEndContinuation()
			return rows, nil
		}
	}
	var whole tuple.Tuple
	if len(prefix) > 0 {
		whole = prefix
	}
	props := recordlayer.NewScanProperties(recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit))
	cursor := t.store.ScanIndex(index, recordlayer.TupleRangeAllOf(whole), state, props)
	defer cursor.Close()
	for {
		res, err := cursor.OnNext(ctx)
		if err != nil {
			return directRows{}, err
		}
		if !res.HasNext() {
			rows.after, err = directContinuationAfter(res.GetNoNextReason(), res.GetContinuation())
			return rows, err
		}
		entry := res.GetValue()
		row := make(tuple.Tuple, 0, len(entry.Key)+len(entry.Value))
		row = append(append(row, entry.Key...), entry.Value...)
		rows.tuples = append(rows.tuples, row)
	}
}

// directIndexStructType is RecordStoreIndex.getMetaData: the fields the
// index's root expression reads from the record, in order, as a struct
// (KeyExpression.validate, ProtobufDdlUtil.recordFromFieldDescriptors).
func directIndexStructType(t directTable, index *recordlayer.Index) (*api.StructType, error) {
	record, err := metadata.StructTypeFromDescriptor(t.rt.Descriptor, false)
	if err != nil {
		return nil, err
	}
	names, err := directIndexFieldNames(index.RootExpression)
	if err != nil {
		return nil, err
	}
	fields := make([]api.StructField, 0, len(names))
	for _, name := range names {
		fd := t.rt.Descriptor.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return nil, api.NewErrorf(api.ErrCodeInternalError, "index <%s> reads no field %s of <%s>", index.Name, name, t.name)
		}
		f := record.Field(fd.Index())
		fields = append(fields, api.NewStructField(f.Name(), f.Type(), len(fields)))
	}
	return api.NewStructType(index.Name, fields, false), nil
}

// directIndexFieldNames are the top-level fields an index's root expression
// reads, in order: KeyExpression.validate's descriptors for the field,
// concatenation, key-with-value and grouping expressions a direct access
// keys by; the record-type key reads none.
func directIndexFieldNames(key recordlayer.KeyExpression) ([]string, error) {
	switch k := key.(type) {
	case *recordlayer.RecordTypeKeyExpression:
		return nil, nil
	case *recordlayer.FieldKeyExpression:
		return []string{k.FieldName()}, nil
	case *recordlayer.CompositeKeyExpression:
		var out []string
		for _, child := range k.SubKeyExpressions() {
			sub, err := directIndexFieldNames(child)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	case *recordlayer.KeyWithValueExpression:
		return directIndexFieldNames(k.InnerKey())
	case *recordlayer.GroupingKeyExpression:
		var out []string
		for _, child := range recordlayer.NormalizeKeyForPositions(k) {
			sub, err := directIndexFieldNames(child)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	}
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "direct access over key %T", key)
}

func keySetMap(k *api.KeySet) map[string]any {
	if k == nil {
		return map[string]any{}
	}
	return k.ToMap()
}

// scanDirectRecords reads the records of t whose primary key begins with
// prefix, in key order.
func scanDirectRecords(ctx context.Context, t directTable, prefix tuple.Tuple) ([]proto.Message, error) {
	var whole tuple.Tuple
	if len(prefix) > 0 {
		whole = prefix
	}
	r := recordlayer.TupleRangeAllOf(whole)
	cursor := t.store.ScanRecordsInRange(r.Low, r.High, r.LowEndpoint, r.HighEndpoint, nil, recordlayer.ForwardScan())
	defer cursor.Close()
	var out []proto.Message
	for {
		res, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !res.HasNext() {
			return out, nil
		}
		rec := res.GetValue()
		if rec.RecordType != nil && rec.RecordType.Name == t.rt.Name {
			out = append(out, rec.Record)
		}
	}
}

// scanDirectPage reads one page of the records of t whose primary key begins
// with prefix: RecordTypeTable.openScan over BackingRecordStore.scanType
// (BackingRecordStore.java:192-200), resumed from cont and limited to limit
// returned rows, the type filter applied after the limit as Java's is. The
// continuation is RecordLayerIterator's after the page: the end when the
// source is exhausted, otherwise the cursor's continuation where it stopped.
func scanDirectPage(ctx context.Context, t directTable, prefix tuple.Tuple, cont api.Continuation, limit int) ([]proto.Message, *directContinuation, error) {
	var state []byte
	if cont != nil {
		state = cont.ExecutionState()
		if state != nil && len(state) == 0 {
			// Resuming from the end reads nothing.
			return nil, directEndContinuation(), nil
		}
	}
	var whole tuple.Tuple
	if len(prefix) > 0 {
		whole = prefix
	}
	r := recordlayer.TupleRangeAllOf(whole)
	props := recordlayer.NewScanProperties(recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit))
	low := r.LowEndpoint
	if state != nil {
		// The cursor resumes after the continuation's key, within the range.
		low = recordlayer.EndpointTypeContinuation
	}
	cursor := t.store.ScanRecordsInRange(r.Low, r.High, low, r.HighEndpoint, state, props)
	defer cursor.Close()
	var out []proto.Message
	for {
		res, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, nil, err
		}
		if !res.HasNext() {
			after, err := directContinuationAfter(res.GetNoNextReason(), res.GetContinuation())
			return out, after, err
		}
		rec := res.GetValue()
		if rec.RecordType != nil && rec.RecordType.Name == t.rt.Name {
			out = append(out, rec.Record)
		}
	}
}

// directContinuationAfter is RecordLayerIterator.fetchNextResult's
// continuation for a cursor that stopped for reason with cont, and
// RecordLayerResultSet.continuationReason's reason for it.
func directContinuationAfter(reason recordlayer.NoNextReason, cont recordlayer.RecordCursorContinuation) (*directContinuation, error) {
	if reason == recordlayer.SourceExhausted || cont == nil || cont.IsEnd() {
		return directEndContinuation(), nil
	}
	b, err := cont.ToBytes()
	if err != nil {
		return nil, err
	}
	why := api.ContinuationTransactionLimitReached
	if reason == recordlayer.ReturnLimitReached {
		why = api.ContinuationQueryExecutionLimitReached
	}
	return &directContinuation{state: b, reason: why}, nil
}

// directRecordKey is a record's primary key (KeyBuilder.buildKey(Row)).
func directRecordKey(t directTable, msg proto.Message) (tuple.Tuple, error) {
	stored := &recordlayer.FDBStoredRecord[proto.Message]{RecordType: t.rt, Record: msg}
	keys, err := t.rt.PrimaryKey.Evaluate(stored, msg)
	if err != nil {
		return nil, err
	}
	if len(keys) != 1 {
		return nil, api.NewErrorf(api.ErrCodeInternalError, "primary key of <%s> evaluates to %d keys", t.name, len(keys))
	}
	pk := make(tuple.Tuple, len(keys[0]))
	for i, e := range keys[0] {
		pk[i] = e
	}
	return pk, nil
}

// buildDirectKey is Java's KeyBuilder.buildKey(Map, failOnIncompleteKey)
// (KeyBuilder.java:61-98): the key's (a primary key's or an index's; scannable
// names it in messages) components in order, the record
// type's key for a record-type component and the named column's value
// otherwise. With failOnIncompleteKey every component must be given; without
// it the given components must be a prefix. A name the key does not use is
// refused.
func buildDirectKey(rt *recordlayer.RecordType, keyExpr recordlayer.KeyExpression, scannable string, columns map[string]any, failOnIncompleteKey bool) (tuple.Tuple, error) {
	components, err := directKeyComponents(keyExpr)
	if err != nil {
		return nil, err
	}
	notPicked := make(map[string]bool, len(columns))
	for name := range columns {
		notPicked[name] = true
	}
	fields := make([]any, len(components))
	for i, component := range components {
		if component == "" {
			fields[i] = rt.GetRecordTypeKey()
			continue
		}
		if v, ok := columns[component]; ok && v != nil {
			delete(notPicked, component)
			fields[i] = directKeyValue(v)
		}
	}
	if failOnIncompleteKey {
		for i, f := range fields {
			if f == nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "Cannot form incomplete key: missing key at position <%d>", i)
			}
		}
	}
	if len(notPicked) > 0 {
		names := make([]string, 0, len(notPicked))
		for name := range notPicked {
			names = append(names, name)
		}
		return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "Unknown keys for %s, unknown keys: <%s>",
			scannable, strings.Join(names, ","))
	}
	for len(fields) > 0 && fields[len(fields)-1] == nil {
		fields = fields[:len(fields)-1]
	}
	for i, f := range fields {
		if f == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "Cannot form key: missing key at position <%d>", i)
		}
	}
	key := make(tuple.Tuple, len(fields))
	for i, f := range fields {
		key[i] = f
	}
	return key, nil
}

// directKeyComponents flattens a key into its components, the empty string
// standing for the record-type key (KeyBuilder.flattenKeys): a key-with-value
// key contributes the components before its split point, a grouping key all
// of its components. A nested key is refused: Java's flattenKeys re-pushes a
// NestingKeyExpression's normalization, itself, and never terminates.
func directKeyComponents(key recordlayer.KeyExpression) ([]string, error) {
	switch k := key.(type) {
	case *recordlayer.RecordTypeKeyExpression:
		return []string{""}, nil
	case *recordlayer.FieldKeyExpression:
		return k.FieldNames(), nil
	case *recordlayer.CompositeKeyExpression:
		var out []string
		for _, child := range k.SubKeyExpressions() {
			sub, err := directKeyComponents(child)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	case *recordlayer.KeyWithValueExpression:
		return directNormalizedComponents(recordlayer.NormalizeKeyForPositions(k.InnerKey())[:k.SplitPoint()])
	case *recordlayer.GroupingKeyExpression:
		return directNormalizedComponents(recordlayer.NormalizeKeyForPositions(k))
	}
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "direct access over key %T", key)
}

func directNormalizedComponents(keys []recordlayer.KeyExpression) ([]string, error) {
	var out []string
	for _, child := range keys {
		sub, err := directKeyComponents(child)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// directKeyValue is a key column's value as a tuple element.
func directKeyValue(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	case float32:
		return float64(x)
	case uuid.UUID:
		return tuple.UUID(x)
	}
	return v
}

// structToRecord converts a struct to a record of desc: Java's
// RecordTypeTable.toDynamicMessage (RecordTypeTable.java:187-286). Each
// attribute names a field; a NULL attribute leaves its field unset; an enum
// takes its value's name; a UUID takes its two-word message (#4243); a struct
// converts recursively, an array element by element.
func structToRecord(st api.Struct, desc protoreflect.MessageDescriptor) (*dynamicpb.Message, error) {
	msg := dynamicpb.NewMessage(desc)
	md := st.MetaData()
	for i := 1; i <= md.AttributeCount(); i++ {
		name, err := md.AttributeName(i)
		if err != nil {
			return nil, err
		}
		fd := directField(desc, name)
		if fd == nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "Cannot find column name: %s", name)
		}
		v, err := st.Attribute(i)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		pv, err := directFieldValue(fd, v)
		if err != nil {
			if apiErr := (*api.Error)(nil); errors.As(err, &apiErr) {
				return nil, err
			}
			typeName, _ := md.AttributeTypeName(i)
			return nil, api.WrapErrorf(err, api.ErrCodeCannotConvertType,
				"Unexpected Column type %s for column %s", typeName, name)
		}
		msg.Set(fd, pv)
	}
	return msg, nil
}

// directField is the field an attribute name names: the field of that name,
// or of its storage name when the name needs escaping.
func directField(desc protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	if fd := desc.Fields().ByName(protoreflect.Name(name)); fd != nil {
		return fd
	}
	if stored, err := protoname.ToProtoBufCompliantName(name); err == nil {
		return desc.Fields().ByName(protoreflect.Name(stored))
	}
	return nil
}

// directFieldValue converts one non-NULL attribute to fd's value.
func directFieldValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	if inner, wrapped, ok := values.EffectiveListField(fd); ok {
		var elems []any
		switch a := v.(type) {
		case api.Array:
			elems = a.Elements()
		case []any:
			elems = a
		default:
			return protoreflect.Value{}, api.NewErrorf(api.ErrCodeCannotConvertType,
				"Field Type expected to be of Type ARRAY but is actually %s", fd.Kind())
		}
		var list protoreflect.List
		var result protoreflect.Value
		if wrapped {
			wrapper, l := values.NewWrappedArrayMessage(fd)
			list, result = l, protoreflect.ValueOfMessage(wrapper)
		} else {
			lv := dynamicpb.NewMessage(fd.ContainingMessage()).NewField(fd)
			list, result = lv.List(), lv
		}
		for _, e := range elems {
			if e == nil {
				return protoreflect.Value{}, api.NewError(api.ErrCodeUnsupportedOperation, (&values.NullArrayElementError{}).Error())
			}
			pv, err := directElementValue(inner, e)
			if err != nil {
				return protoreflect.Value{}, err
			}
			list.Append(pv)
		}
		return result, nil
	}
	return directElementValue(fd, v)
}

// directElementValue converts one non-repeated value to fd's value.
func directElementValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	if fd.Kind() == protoreflect.EnumKind {
		name, ok := v.(string)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("enum value %T", v)
		}
		n, err := values.StringToEnumNumber(fd.Enum(), name)
		if err != nil {
			return protoreflect.Value{}, api.NewErrorf(api.ErrCodeCannotConvertType, "Invalid enum value: %s", name)
		}
		return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), nil
	}
	if st, ok := v.(api.Struct); ok {
		if fd.Kind() != protoreflect.MessageKind {
			return protoreflect.Value{}, fmt.Errorf("struct into a %s field", fd.Kind())
		}
		nested, err := structToRecord(st, fd.Message())
		if err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfMessage(nested), nil
	}
	switch x := v.(type) {
	case uuid.UUID:
		v = [16]byte(x)
	case int:
		v = int64(x)
	case int32:
		v = int64(x)
	case int16:
		v = int64(x)
	case int8:
		v = int64(x)
	case float32:
		v = float64(x)
	}
	return functions.ConvertElementToProtoValue(fd, v)
}

// directResultSet is a direct access's rows, materialized: each a record read
// positionally over its descriptor (RecordTypeTable's MessageTuple rows).
type directResultSet struct {
	md      *directResultSetMetaData
	rows    []directRow
	after   *directContinuation
	pos     int
	wasNull bool
	closed  bool
}

func newDirectResultSet(rows directRows) (api.ResultSet, error) {
	st := rows.st
	if st == nil {
		var err error
		if st, err = metadata.StructTypeFromDescriptor(rows.desc, false); err != nil {
			return nil, err
		}
	}
	rs := &directResultSet{md: &directResultSetMetaData{st: st}, after: rows.after}
	for _, rec := range rows.records {
		row, err := rowstruct.New(rec.ProtoReflect())
		if err != nil {
			return nil, err
		}
		rs.rows = append(rs.rows, row)
	}
	for _, tup := range rows.tuples {
		rs.rows = append(rs.rows, directTupleRow(tup))
	}
	return rs, nil
}

// directRow is one row of a direct access: a record, or an index entry.
type directRow interface {
	Attribute(oneBasedIndex int) (any, error)
}

// directTupleRow is an index entry's row, its key then its value, read
// positionally (FDBTuple over ImmutableKeyValue). A UUID reads as a record's
// UUID column does, as its string.
type directTupleRow tuple.Tuple

func (r directTupleRow) Attribute(oneBasedIndex int) (any, error) {
	if oneBasedIndex < 1 || oneBasedIndex > len(r) {
		return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "column index %d out of range", oneBasedIndex)
	}
	switch v := r[oneBasedIndex-1].(type) {
	case tuple.UUID:
		return uuid.UUID(v).String(), nil
	case int:
		return int64(v), nil
	default:
		return v, nil
	}
}

func (r *directResultSet) Next() bool {
	if r.closed || r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *directResultSet) Err() error { return nil }

func (r *directResultSet) Close() error {
	r.closed = true
	return nil
}

func (r *directResultSet) MetaData() api.ResultSetMetaData { return r.md }

func (r *directResultSet) cell(columnIndex int) (any, error) {
	if r.pos < 1 || r.pos > len(r.rows) {
		return nil, api.NewError(api.ErrCodeInvalidCursorState, "result set is not positioned on a row")
	}
	v, err := r.rows[r.pos-1].Attribute(columnIndex)
	if err != nil {
		return nil, err
	}
	r.wasNull = v == nil
	return v, nil
}

func (r *directResultSet) cellByName(name string) (any, error) {
	for i := 1; i <= r.md.ColumnCount(); i++ {
		if n, _ := r.md.ColumnName(i); n == name {
			return r.cell(i)
		}
	}
	return nil, api.NewErrorf(api.ErrCodeInvalidColumnReference, "no column %q", name)
}

func directTyped[T any](v any, err error, typeName string) (T, error) {
	var zero T
	if err != nil || v == nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, api.NewErrorf(api.ErrCodeCannotConvertType, "column value %T is not %s", v, typeName)
	}
	return t, nil
}

func (r *directResultSet) Long(i int) (int64, error) {
	v, err := r.cell(i)
	if n, ok := v.(int32); ok {
		return int64(n), nil
	}
	return directTyped[int64](v, err, "BIGINT")
}

func (r *directResultSet) Float(i int) (float32, error) {
	v, err := r.cell(i)
	if d, ok := v.(float64); ok {
		return float32(d), nil
	}
	return directTyped[float32](v, err, "FLOAT")
}

func (r *directResultSet) Double(i int) (float64, error) {
	v, err := r.cell(i)
	if f, ok := v.(float32); ok {
		return float64(f), nil
	}
	return directTyped[float64](v, err, "DOUBLE")
}

func (r *directResultSet) String(i int) (string, error) {
	v, err := r.cell(i)
	return directTyped[string](v, err, "STRING")
}

func (r *directResultSet) Bytes(i int) ([]byte, error) {
	v, err := r.cell(i)
	return directTyped[[]byte](v, err, "BYTES")
}

func (r *directResultSet) Boolean(i int) (bool, error) {
	v, err := r.cell(i)
	return directTyped[bool](v, err, "BOOLEAN")
}

func (r *directResultSet) Object(i int) (any, error) { return r.cell(i) }

func (r *directResultSet) WasNull() bool { return r.wasNull }

// Continuation is the continuation past the rows, given only once they are
// consumed (RecordLayerResultSet.getContinuation, IteratorResultSet's).
func (r *directResultSet) Continuation() (api.Continuation, error) {
	if !r.closed && r.pos < len(r.rows) {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"Continuation can only be returned once the result set has been exhausted")
	}
	return r.after, nil
}

func (r *directResultSet) LongByName(name string) (int64, error) {
	v, err := r.cellByName(name)
	if n, ok := v.(int32); ok {
		return int64(n), nil
	}
	return directTyped[int64](v, err, "BIGINT")
}

func (r *directResultSet) StringByName(name string) (string, error) {
	v, err := r.cellByName(name)
	return directTyped[string](v, err, "STRING")
}

func (r *directResultSet) BytesByName(name string) ([]byte, error) {
	v, err := r.cellByName(name)
	return directTyped[[]byte](v, err, "BYTES")
}

func (r *directResultSet) BooleanByName(name string) (bool, error) {
	v, err := r.cellByName(name)
	return directTyped[bool](v, err, "BOOLEAN")
}

func (r *directResultSet) ObjectByName(name string) (any, error) { return r.cellByName(name) }

// directContinuation is a direct access's continuation: Java's ContinuationImpl
// over the cursor's bytes, with no binding hash. A nil state is the beginning,
// an empty one the end.
type directContinuation struct {
	state  []byte
	reason api.ContinuationReason
}

func directBeginContinuation() *directContinuation {
	return &directContinuation{reason: api.ContinuationCursorAfterLast}
}

func directEndContinuation() *directContinuation {
	return &directContinuation{state: []byte{}, reason: api.ContinuationCursorAfterLast}
}

// Serialize is the ContinuationProto ContinuationImpl serializes: version 1
// and, unless at the beginning, the execution state (continuation.proto).
func (c *directContinuation) Serialize() []byte {
	b := protowire.AppendTag(nil, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, 1)
	if c.state != nil {
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, c.state)
	}
	return b
}

func (c *directContinuation) ExecutionState() []byte         { return c.state }
func (c *directContinuation) Reason() api.ContinuationReason { return c.reason }

// directResultSetMetaData describes a direct access's rows: the table's
// columns in declaration order.
type directResultSetMetaData struct{ st *api.StructType }

func (m *directResultSetMetaData) ColumnCount() int { return m.st.NumFields() }

func (m *directResultSetMetaData) field(i int) (api.StructField, error) {
	if i < 1 || i > m.st.NumFields() {
		return api.StructField{}, api.NewErrorf(api.ErrCodeInvalidColumnReference, "column index %d out of range", i)
	}
	return m.st.Field(i - 1), nil
}

func (m *directResultSetMetaData) ColumnName(i int) (string, error) {
	f, err := m.field(i)
	if err != nil {
		return "", err
	}
	return f.Name(), nil
}

func (m *directResultSetMetaData) ColumnLabel(i int) (string, error) { return m.ColumnName(i) }

func (m *directResultSetMetaData) ColumnType(i int) (int, error) {
	f, err := m.field(i)
	if err != nil {
		return 0, err
	}
	return api.JDBCType(f.Type().Code()), nil
}

func (m *directResultSetMetaData) ColumnTypeName(i int) (string, error) {
	f, err := m.field(i)
	if err != nil {
		return "", err
	}
	return f.Type().Code().String(), nil
}

func (m *directResultSetMetaData) ColumnNullable(i int) (int, error) {
	f, err := m.field(i)
	if err != nil {
		return 0, err
	}
	if f.Type().IsNullable() {
		return api.ColumnNullable, nil
	}
	return api.ColumnNoNulls, nil
}

func (m *directResultSetMetaData) ColumnDataType(i int) (api.DataType, error) {
	f, err := m.field(i)
	if err != nil {
		return nil, err
	}
	return f.Type(), nil
}

var (
	_ api.DirectAccessStatement = (*directAccessStatement)(nil)
	_ api.ResultSet             = (*directResultSet)(nil)
)
