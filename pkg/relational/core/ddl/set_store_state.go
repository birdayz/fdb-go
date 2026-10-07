package ddl

import (
	"sort"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// RecordLayerConfig is the part of Java's RecordLayerConfig a store-state
// action reads: the index states to force and the format version to open at.
type RecordLayerConfig struct {
	IndexStates   map[string]recordlayer.IndexState
	FormatVersion int32
}

// SetStoreStateConstantAction opens a schema's existing record store and forces
// its index states. Mirrors Java's RecordLayerSetStoreStateConstantAction, which
// only the yaml tests' `set schema state` command reaches.
type SetStoreStateConstantAction struct {
	dbPath     string
	schemaName string
	config     RecordLayerConfig
	catalog    api.StoreCatalog
	ks         *keyspace.RelationalKeyspace
}

// NewSetStoreStateConstantAction builds the action; ks must be non-nil.
func NewSetStoreStateConstantAction(dbPath, schemaName string, config RecordLayerConfig, cat api.StoreCatalog, ks *keyspace.RelationalKeyspace) *SetStoreStateConstantAction {
	return &SetStoreStateConstantAction{dbPath: dbPath, schemaName: schemaName, config: config, catalog: cat, ks: ks}
}

// Execute opens the store (it must exist) with the schema's catalog metadata at
// the configured format version, which only ever raises a store's version, and
// marks each index into its configured state, as Java's action does. Java's
// action rethrows RecordStoreAlreadyExistsException as SCHEMA_ALREADY_EXISTS;
// an open of an existing store cannot raise it, so Go has no such arm. A
// record-layer error is returned unchanged; the connection translates it at the
// driver boundary, as Java's ExceptionUtil.toRelationalException does here.
func (a *SetStoreStateConstantAction) Execute(txn api.Transaction) error {
	if a.ks == nil {
		return api.NewError(api.ErrCodeInternalError, "SetStoreState requires a keyspace")
	}
	rctx, ok := txn.Unwrap().(*recordlayer.FDBRecordContext)
	if !ok {
		return api.NewErrorf(api.ErrCodeInternalError,
			"SetStoreState requires a transaction whose Unwrap() returns *recordlayer.FDBRecordContext, got %T", txn.Unwrap())
	}
	schema, err := a.catalog.LoadSchema(txn, a.dbPath, a.schemaName)
	if err != nil {
		return err
	}
	tmpl, ok := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
	if !ok {
		return api.NewErrorf(api.ErrCodeInternalError, "SetStoreState requires *metadata.RecordLayerSchemaTemplate, got %T", schema.SchemaTemplate())
	}
	ss, err := a.ks.SchemaSubspace(rctx, a.dbPath, a.schemaName)
	if err != nil {
		return err
	}
	builder := recordlayer.NewStoreBuilder().
		SetContext(rctx).
		SetSubspace(ss).
		SetMetaDataProvider(tmpl.Underlying())
	if a.config.FormatVersion > 0 {
		builder.SetFormatVersion(a.config.FormatVersion)
	}
	store, err := builder.Open()
	if err != nil {
		return err
	}
	// Java iterates a HashMap; the order of independent index-state writes
	// does not matter, so Go sorts for determinism.
	names := make([]string, 0, len(a.config.IndexStates))
	for name := range a.config.IndexStates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var markErr error
		switch a.config.IndexStates[name] {
		case recordlayer.IndexStateReadable:
			_, markErr = store.MarkIndexReadable(name)
		case recordlayer.IndexStateWriteOnly:
			_, markErr = store.MarkIndexWriteOnly(name)
		case recordlayer.IndexStateWriteOnlyWithQueue:
			_, markErr = store.MarkIndexWriteOnlyWithQueue(name)
		case recordlayer.IndexStateDisabled:
			_, markErr = store.MarkIndexDisabled(name)
		case recordlayer.IndexStateReadableUniquePending:
			_, markErr = store.MarkIndexReadableOrUniquePending(name)
		}
		if markErr != nil {
			return markErr
		}
	}
	return nil
}
