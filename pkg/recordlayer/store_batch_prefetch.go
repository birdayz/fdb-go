package recordlayer

import (
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// RecordBatchPrefetch warms only the issuing transaction's read-your-writes
// cache. It contains no authoritative old images and cannot be passed to a save.
// Call Wait before saving the batch in that same transaction; every normal save
// must still load and validate its old record and establish its read conflicts.
// A handle must not be reused after its context's transaction is reset or ended.
// Wait is idempotent and returns the first wait's result.
type RecordBatchPrefetch struct {
	context *FDBRecordContext
	pending []fdb.RangeResult
	once    sync.Once
	err     error
}

// PrefetchRecordBatch starts snapshot reads before the store is opened, allowing
// them to overlap store-state reads. Preparation and read errors are deferred to
// Wait so callers can validate independent authorization reads first. It neither
// opens the store nor changes records, local versions, or transaction options.
//
// Only modern record ranges are warmed: the header is not yet known, so legacy
// bare-key records and separate versions remain the normal save's responsibility.
// Declared version columns are not prefetched, avoiding local-version claims.
func (b *StoreBuilder) PrefetchRecordBatch(records []proto.Message) *RecordBatchPrefetch {
	p := &RecordBatchPrefetch{}
	if len(records) == 0 {
		return p
	}
	if p.err = b.validateBuilder(); p.err != nil {
		return p
	}
	p.context = b.context
	if p.err = p.checkActive(); p.err != nil {
		return p
	}

	recordsSubspace := b.subspace.Sub(RecordKey)
	prefixes := make([][]byte, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for i, record := range records {
		if record == nil {
			p.err = &RecordCoreArgumentError{Message: fmt.Sprintf("record %d is nil", i)}
			return p
		}
		rt, err := b.metaData.GetRecordTypeForDescriptor(record.ProtoReflect().Descriptor())
		if err != nil {
			p.err = err
			return p
		}
		if err := rt.checkUTF8Strings(record); err != nil {
			p.err = &RecordSerializationError{Cause: err}
			return p
		}
		if rt.PrimaryKey == nil {
			p.err = &MetaDataError{Message: fmt.Sprintf("no primary key for: %s", rt.Name)}
			return p
		}
		if !b.goOnlyCollation {
			if fn := collationFunctionIn(rt.PrimaryKey); fn != "" {
				p.err = &GoOnlyCollationError{Function: fn, RecordTypeName: rt.Name}
				return p
			}
		}
		if countVersionColumns(rt.PrimaryKey) != 0 {
			continue
		}
		record, _ = rt.asJavaForSave(record)
		values, err := evaluateKeyFlat(rt.PrimaryKey, &FDBStoredRecord[proto.Message]{RecordType: rt, Record: record}, record)
		if err != nil {
			p.err = fmt.Errorf("record %d: extract primary key: %w", i, err)
			return p
		}
		key := make(tuple.Tuple, len(values))
		for j, value := range values {
			key[j] = value
		}
		if incomplete, _ := key.HasIncompleteVersionstamp(); incomplete {
			p.err = &IncompleteVersionstampError{Context: fmt.Sprintf("record %d primary key", i)}
			return p
		}
		prefix := recordsSubspace.Pack(key)
		if _, duplicate := seen[string(prefix)]; duplicate {
			continue
		}
		seen[string(prefix)] = struct{}{}
		prefixes = append(prefixes, prefix)
	}

	tx := b.context.ReadTransaction(true)
	p.pending = make([]fdb.RangeResult, 0, len(prefixes))
	for _, prefix := range prefixes {
		begin, end := recordRangeBounds(prefix)
		p.pending = append(p.pending, tx.GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}))
	}
	return p
}

func (p *RecordBatchPrefetch) checkActive() error {
	if p.context.deactivated.Load() {
		return errTransactionNotActive()
	}
	return p.context.Context().Err()
}

// Wait drains the reads into the issuing transaction's cache without decoding
// stored records. Corrupt old images are still rejected by the subsequent save.
func (p *RecordBatchPrefetch) Wait() error {
	p.once.Do(func() {
		defer func() {
			p.pending = nil
			p.context = nil
		}()
		if p.err != nil || p.context == nil {
			return
		}
		if p.err = p.checkActive(); p.err != nil {
			return
		}
		// Futures may already be ready when the transaction is canceled.
		if _, p.err = p.context.Transaction().GetReadVersion().Get(); p.err != nil {
			return
		}
		for _, future := range p.pending {
			if _, p.err = future.GetSliceWithError(); p.err != nil {
				return
			}
			if p.err = p.checkActive(); p.err != nil {
				return
			}
		}
	})
	return p.err
}
