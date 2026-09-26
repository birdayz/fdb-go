package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// The template restore (RFC-257 WS-J section 2, a Go extension: Java has no
// restore). DROP SCHEMA TEMPLATE drops a template whatever binds it, as the
// target does, and a schema bound to a dropped version has no exit that keeps
// its data: the gone-version refusal blocks RepairSchema and SaveSchema over it,
// and re-creating the version from DDL is the silent rebind the version guard
// refuses. The restore is the way out: it writes the dropped version's own
// stored MetaData bytes (the caller's backup) back under (name, version), runs
// no build-path check (it restores what was stored), and admits the bytes only
// when it can show they are that version:
//   - (name, version) is not stored, so it overwrites no live version;
//   - some schema binds (name, version), decided inside the restoring
//     transaction by a limit-1 read of TEMPLATES_VALUE_INDEX, since a restore
//     nothing binds could put a dropped history's bytes beside a new one;
//   - every bound store's header records a metadata version at most the
//     restored metadata's, read through the keyspace the caller names, and a
//     bound schema whose store has no header there refuses (a header below is
//     a schema rebound and not opened since, and upgrades on its next open);
//   - the restored metadata is ONE HISTORY with every stored version of name
//     (carryCompatible).
//
// The listing and the header reads run in read-only transactions of
// restoreHeaderBatch bindings each, before the restoring transaction, since
// nothing bounds the number of bound stores. While (name, version) is not stored
// no store bound to it can be opened through the catalog, so no bound header
// moves; a store opened another way in the window fails closed on its next open
// under the restored metadata (StaleMetaDataVersionError). The restoring
// transaction's reads make its own decisions serializable: a DROP SCHEMA of the
// binding its limit-1 read returned, or any template write of name, after its
// read version conflicts, and the restore then decides again from the listing.

// TemplateBinding is a schema bound to a template version.
type TemplateBinding struct {
	DatabaseID, SchemaName string
}

// restoreHeaderBatch is the number of bound schemas whose headers one read-only
// transaction lists and reads.
const restoreHeaderBatch = 1000

// restoreMaxAttempts bounds the restore's retries from the listing after a
// conflicting commit.
const restoreMaxAttempts = 100

// restoreOptions are the restore's batch size and, for a test, calls between
// its phases: after the listing and header reads, and after the restoring
// transaction's reads, before its commit. attempts counts restoring
// transactions.
type restoreOptions struct {
	batch        int
	afterListing func()
	beforeCommit func()
	// afterCommit, when set, replaces a successful commit's result: a test
	// makes a landed commit report commit_unknown_result through it.
	afterCommit func() error
	attempts    int
}

func errRestore(code api.ErrorCode, name string, version int, format string, args ...any) error {
	return api.NewErrorf(code, "schema template %s version %d cannot be restored: %s",
		name, version, fmt.Sprintf(format, args...))
}

// RestoreTemplateVersion restores a dropped template version from its stored
// MetaData bytes (md, the backup of the version's catalog META_DATA), for the
// schemas still bound to it, resolving their stores through ks, the keyspace
// the deployment opens its schemas with. It is fleet.RestoreTemplateVersion;
// every check above runs on every call. A refusal is 42F59
// INVALID_SCHEMA_TEMPLATE, or 42F62 DUPLICATE_SCHEMA_TEMPLATE when the version is
// stored.
func (c *RecordLayerStoreCatalog) RestoreTemplateVersion(ctx context.Context, db *recordlayer.FDBDatabase,
	ks *keyspace.RelationalKeyspace, name string, version int, md []byte,
) error {
	return c.restoreTemplateVersion(ctx, db, ks, name, version, md, &restoreOptions{batch: restoreHeaderBatch})
}

func (c *RecordLayerStoreCatalog) restoreTemplateVersion(ctx context.Context, db *recordlayer.FDBDatabase,
	ks *keyspace.RelationalKeyspace, name string, version int, md []byte, opts *restoreOptions,
) error {
	restored, err := loadRestoredMetaData(name, version, md)
	if err != nil {
		return err
	}
	maybeCommitted := false
	for attempt := 0; attempt < restoreMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// After a commit whose result is unknown, a stored row holding exactly
		// these bytes is this restore's own write: done, before the listing,
		// which a change made since (the binding dropped, say) would refuse.
		if maybeCommitted {
			stored, err := c.storesExactly(ctx, db, name, version, md)
			if err != nil || stored {
				return err
			}
		}
		if err := c.checkBoundStores(ctx, db, ks, name, version, restored.md.Version(), opts.batch); err != nil {
			return err
		}
		if opts.afterListing != nil {
			opts.afterListing()
		}
		opts.attempts++
		done, err := c.restoreInTransaction(ctx, db, name, version, md, restored, maybeCommitted, opts)
		if done || err == nil {
			return err
		}
		var fe fdb.Error
		if !errors.As(err, &fe) || !fe.Retryable() {
			return err
		}
		// commit_unknown_result or cluster_version_changed: the write may have landed; a retry that finds
		// exactly these bytes stored has restored them.
		maybeCommitted = maybeCommitted || fe.Code == 1021 || fe.Code == 1039
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return errRestore(api.ErrCodeInvalidSchemaTemplate, name, version, "the restore conflicted %d times", restoreMaxAttempts)
}

// restoredMetaData is the restored bytes read as the catalog reads a template.
type restoredMetaData struct {
	proto *gen.MetaData
	md    *recordlayer.RecordMetaData
	tmpl  api.SchemaTemplate
}

func loadRestoredMetaData(name string, version int, md []byte) (*restoredMetaData, error) {
	p := &gen.MetaData{}
	if err := recordlayer.UnmarshalAsJava(md, p); err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
			"schema template %s version %d cannot be restored: its metadata does not parse", name, version)
	}
	loaded, err := recordlayer.RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate,
			"schema template %s version %d cannot be restored: its metadata does not load", name, version)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, loaded, version)
	if err != nil {
		return nil, err
	}
	return &restoredMetaData{proto: p, md: loaded, tmpl: tmpl}, nil
}

// checkBoundStores lists every schema bound to (name, version), batchSize per
// read-only transaction, and reads each bound store's header through ks.
func (c *RecordLayerStoreCatalog) checkBoundStores(ctx context.Context, db *recordlayer.FDBDatabase,
	ks *keyspace.RelationalKeyspace, name string, version int, mdVersion int, batchSize int,
) error {
	var after *TemplateBinding
	listed := 0
	for {
		var batch []TemplateBinding
		_, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			store, err := c.openStore(NewFDBTransaction(rctx))
			if err != nil {
				return nil, err
			}
			batch, err = listBindings(store, name, version, after, batchSize)
			if err != nil {
				return nil, err
			}
			for _, b := range batch {
				ss, err := ks.SchemaSubspace(b.DatabaseID, b.SchemaName)
				if err != nil {
					return nil, err
				}
				raw, err := rctx.Transaction().Get(ss.Pack(tuple.Tuple{recordlayer.StoreInfoKey})).Get()
				if err != nil {
					return nil, err
				}
				if raw == nil {
					return nil, errRestore(api.ErrCodeInvalidSchemaTemplate, name, version,
						"schema %s/%s has no store header in the keyspace", b.DatabaseID, b.SchemaName)
				}
				header := &gen.DataStoreInfo{}
				if err := recordlayer.UnmarshalAsJava(raw, header); err != nil {
					return nil, errRestore(api.ErrCodeInvalidSchemaTemplate, name, version,
						"the store header of schema %s/%s does not parse: %v", b.DatabaseID, b.SchemaName, err)
				}
				if int(header.GetMetaDataversion()) > mdVersion {
					return nil, errRestore(api.ErrCodeInvalidSchemaTemplate, name, version,
						"the store of schema %s/%s records metadata version %d, above the restored metadata's %d",
						b.DatabaseID, b.SchemaName, header.GetMetaDataversion(), mdVersion)
				}
			}
			return nil, nil
		})
		if err != nil {
			return err
		}
		listed += len(batch)
		if len(batch) < batchSize {
			break
		}
		after = &batch[len(batch)-1]
	}
	if listed == 0 {
		return errRestore(api.ErrCodeInvalidSchemaTemplate, name, version, "no schema binds it")
	}
	return nil
}

// listBindings is up to limit schemas bound to (name, version), in
// TEMPLATES_VALUE_INDEX order, after the binding after (nil: from the first).
func listBindings(store *recordlayer.FDBRecordStore, name string, version int, after *TemplateBinding, limit int) ([]TemplateBinding, error) {
	idx := store.GetRecordMetaData().GetIndex(IdxTemplatesValue)
	if idx == nil {
		return nil, api.NewErrorf(api.ErrCodeInternalError, "catalog index %s is missing", IdxTemplatesValue)
	}
	if state := store.GetIndexState(IdxTemplatesValue); state != recordlayer.IndexStateReadable {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"catalog index %s is %v, so the schemas bound to template %s cannot be read", IdxTemplatesValue, state, name)
	}
	sub := store.IndexSubspace(idx)
	versionRange, err := fdb.PrefixRange(sub.Pack(tuple.Tuple{name, int64(version)}))
	if err != nil {
		return nil, err
	}
	begin := versionRange.Begin
	if after != nil {
		afterRange, err := fdb.PrefixRange(sub.Pack(tuple.Tuple{name, int64(version), after.DatabaseID, after.SchemaName}))
		if err != nil {
			return nil, err
		}
		begin = afterRange.End
	}
	kvs, err := store.Context().Transaction().GetRange(fdb.KeyRange{Begin: begin, End: versionRange.End},
		fdb.RangeOptions{Limit: limit}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	out := make([]TemplateBinding, 0, len(kvs))
	for _, kv := range kvs {
		t, err := sub.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		if len(t) < 4 {
			return nil, api.NewErrorf(api.ErrCodeInternalError, "catalog index %s entry %v is short", IdxTemplatesValue, t)
		}
		db, dok := t[2].(string)
		schema, sok := t[3].(string)
		if !dok || !sok {
			return nil, api.NewErrorf(api.ErrCodeInternalError, "catalog index %s entry %v is malformed", IdxTemplatesValue, t)
		}
		out = append(out, TemplateBinding{DatabaseID: db, SchemaName: schema})
	}
	return out, nil
}

// restoreInTransaction is the restoring transaction: its own reads of the
// version, of a binding and of every stored version of name, the carry checks,
// the raw write and the commit. done reports a restore a retry found already
// made (maybeCommitted and exactly md stored).
func (c *RecordLayerStoreCatalog) restoreInTransaction(ctx context.Context, db *recordlayer.FDBDatabase,
	name string, version int, md []byte, restored *restoredMetaData, maybeCommitted bool, opts *restoreOptions,
) (bool, error) {
	tr, err := db.CreateWritableTransaction()
	if err != nil {
		return false, err
	}
	rctx := db.NewRecordContext(tr)
	defer rctx.Cancel()
	store, err := c.openStore(NewFDBTransaction(rctx))
	if err != nil {
		return false, err
	}
	existing, err := store.LoadRecord(templateKeyAtVersion(name, version))
	if err != nil {
		return false, err
	}
	if existing != nil {
		if row, ok := existing.Record.(*gen.Templates); ok && maybeCommitted && string(row.GetMETA_DATA()) == string(md) {
			return true, nil
		}
		return false, errRestore(api.ErrCodeDuplicateSchemaTemplate, name, version, "it is stored")
	}
	bound, err := firstBinding(store, name, version, version)
	if err != nil {
		return false, err
	}
	if bound == nil {
		return false, errRestore(api.ErrCodeInvalidSchemaTemplate, name, version, "no schema binds it")
	}
	stored, err := storedTemplateRows(store, name)
	if err != nil {
		return false, err
	}
	for _, row := range stored {
		if err := carryCompatible(name, version, restored, row); err != nil {
			return false, err
		}
	}
	if _, err := store.SaveRecord(&gen.Templates{
		TEMPLATE_NAME:    proto.String(name),
		TEMPLATE_VERSION: proto.Int32(int32(version)),
		META_DATA:        md,
	}); err != nil {
		return false, api.WrapErrorf(err, api.ErrCodeInternalError, "restore schema template")
	}
	if opts.beforeCommit != nil {
		opts.beforeCommit()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := rctx.Commit(); err != nil || opts.afterCommit == nil {
		return false, err
	}
	return false, opts.afterCommit()
}

// storesExactly reports, in a transaction that writes nothing, whether (name, version) is
// stored holding exactly md.
func (c *RecordLayerStoreCatalog) storesExactly(ctx context.Context, db *recordlayer.FDBDatabase, name string, version int, md []byte) (bool, error) {
	out, err := db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, err := c.openStore(NewFDBTransaction(rctx))
		if err != nil {
			return false, err
		}
		existing, err := store.LoadRecord(templateKeyAtVersion(name, version))
		if err != nil || existing == nil {
			return false, err
		}
		row, ok := existing.Record.(*gen.Templates)
		return ok && string(row.GetMETA_DATA()) == string(md), nil
	})
	if err != nil {
		return false, err
	}
	return out.(bool), nil
}

// storedTemplateRows reads every stored version of name, one serializable
// range read of its rows.
func storedTemplateRows(store *recordlayer.FDBRecordStore, name string) ([]*gen.Templates, error) {
	cursor := store.ScanRecordsInRange(
		tuple.Tuple{SchemaTemplateRecordTypeKey, name},
		tuple.Tuple{SchemaTemplateRecordTypeKey, name},
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, recordlayer.ForwardScan(),
	)
	defer func() { _ = cursor.Close() }()
	var rows []*gen.Templates
	for {
		r, err := cursor.OnNext(store.Context().Context())
		if err != nil {
			return nil, err
		}
		if !r.HasNext() {
			return rows, nil
		}
		row, ok := r.GetValue().Record.(*gen.Templates)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInternalError, "catalog template row has unexpected type %T", r.GetValue().Record)
		}
		rows = append(rows, row)
	}
}

// historyVersion is one side of a carry check.
type historyVersion struct {
	version int
	proto   *gen.MetaData
	md      *recordlayer.RecordMetaData
	tmpl    api.SchemaTemplate
}

// carryCompatible admits the restored version beside a stored one only when the
// two are one history: with L the lower template version and H the higher, what
// the carry keeps from each version to the next holds from L to H (WS-J section
// 2, checks (i) to (iv)).
func carryCompatible(name string, version int, restored *restoredMetaData, row *gen.Templates) error {
	storedVersion := int(row.GetTEMPLATE_VERSION())
	refuse := func(format string, args ...any) error {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"schema template %s version %d cannot be restored beside version %d: %s",
			name, version, storedVersion, fmt.Sprintf(format, args...))
	}
	sp := &gen.MetaData{}
	if err := recordlayer.UnmarshalAsJava(row.GetMETA_DATA(), sp); err != nil {
		return refuse("its metadata does not parse: %v", err)
	}
	smd, err := recordlayer.RecordMetaDataFromProto(proto.Clone(sp).(*gen.MetaData))
	if err != nil {
		return refuse("its metadata does not load: %v", err)
	}
	stmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, smd, storedVersion)
	if err != nil {
		return refuse("%v", err)
	}
	restoredSide := historyVersion{version: version, proto: restored.proto, md: restored.md, tmpl: restored.tmpl}
	storedSide := historyVersion{version: storedVersion, proto: sp, md: smd, tmpl: stmpl}
	low, high := restoredSide, storedSide
	if storedVersion < version {
		low, high = storedSide, restoredSide
	}
	// A lower template version with a higher metadata version is an inverted
	// history, which no carry writes.
	if low.md.Version() > high.md.Version() {
		return refuse("template version %d has metadata version %d, above version %d's %d",
			low.version, low.md.Version(), high.version, high.md.Version())
	}
	// (i) and (ii): the evolution validator from L to H, with the options the
	// restore states: an equal metadata version, index rebuilds (an index H
	// raised is rebuilt by a store bound to L when it next opens), and no record
	// type renamed (two names on one key).
	validator := recordlayer.NewMetaDataEvolutionValidator().
		SetAllowNoVersionChange(true).
		SetAllowIndexRebuilds(true).
		SetDisallowTypeRenames(true).
		Build()
	if err := validator.Validate(low.md, high.md); err != nil {
		return refuse("%v", err)
	}
	// (iii) every index both define: EQUIVALENT, unless H raised it above L's
	// metadata version, which a store bound to L rebuilds on its next open.
	lowIndexes := map[string]*gen.Index{}
	for _, ix := range low.proto.GetIndexes() {
		lowIndexes[ix.GetName()] = ix
	}
	for _, hix := range high.proto.GetIndexes() {
		lix, ok := lowIndexes[hix.GetName()]
		if !ok {
			continue
		}
		if h := high.md.GetIndex(hix.GetName()); h != nil && h.LastModifiedVersion > low.md.Version() {
			continue
		}
		class, field, err := recordlayer.ClassifyIndexCarry(lix, hix)
		if err != nil {
			return refuse("index %s: %v", hix.GetName(), err)
		}
		if class == recordlayer.IndexChanged {
			return refuse("index %s differs in %s", hix.GetName(), field)
		}
	}
	// (iv) the relational validator from L to H: column types whole (an enum's
	// value list, a vector's options), which the store's validator never compares.
	if err := NewRelationalSchemaEvolutionValidator().Validate(low.tmpl, high.tmpl); err != nil {
		return refuse("%v", err)
	}
	return nil
}
