package cmd

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	relkeyspace "fdb.dev/pkg/relational/core/keyspace"
)

// catalogSource is the third meta.Source: metadata resolved from the
// relational catalog at `__SYS/CATALOG`. It loads the schema row for
// (database, schema) and follows it to the template **at the version the
// schema is pinned to** — `LoadSchema` → `LoadSchemaTemplateAtVersion` —
// exactly how the SQL executor resolves metadata for the same store.
//
// Never LoadSchemaTemplate (the latest version): a store whose schema is
// pinned to template v1 would open with v2 metadata, and anything that
// honours checkPossiblyRebuild would then WRITE to the store from a read
// command (Graefe G1 on RFC-174). The pinned version is also simply the
// truth: it is the metadata the records were written under.
type catalogSource struct {
	db       *recordlayer.FDBDatabase
	database string
	schema   string
}

func (s *catalogSource) Name() string {
	return "catalog:" + s.database + "/" + s.schema
}

func (s *catalogSource) Load(ctx context.Context) (*recordlayer.RecordMetaData, error) {
	cat, err := catalog.NewRecordLayerStoreCatalog(relationalKeyspace().CatalogSubspace())
	if err != nil {
		return nil, fmt.Errorf("open relational catalog: %w", err)
	}
	result, err := s.db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		txn := catalog.NewFDBTransaction(rctx)
		sch, err := cat.LoadSchema(txn, s.database, s.schema)
		if err != nil {
			return nil, err
		}
		tpl := sch.SchemaTemplate()
		up, ok := tpl.(interface {
			Underlying() *recordlayer.RecordMetaData
		})
		if !ok {
			return nil, fmt.Errorf("template %q is not backed by a record-layer MetaData — catalog entry type %T",
				tpl.MetadataName(), tpl)
		}
		return up.Underlying(), nil
	})
	if err != nil {
		return nil, wrapMissingCatalogErr(err)
	}
	md, _ := result.(*recordlayer.RecordMetaData)
	if md == nil {
		return nil, fmt.Errorf("catalog schema %s/%s produced no metadata", s.database, s.schema)
	}
	return md, nil
}

// relationalKeyspace returns the keyspace layout the sqldriver writes
// under — root at the empty subspace. Both the catalog subspace and
// per-schema store subspaces hang off it.
func relationalKeyspace() *relkeyspace.RelationalKeyspace {
	return relkeyspace.New(subspace.Sub())
}

// relationalStoreSubspace resolves the FDB subspace of the record store
// backing (database, schema) — tuple(dbPath, schemaName) under the
// relational root. This is the keyspace half of relational addressing;
// catalogSource is the metadata half.
func relationalStoreSubspace(database, schema string) (subspace.Subspace, error) {
	return relationalKeyspace().SchemaSubspace(database, schema)
}
