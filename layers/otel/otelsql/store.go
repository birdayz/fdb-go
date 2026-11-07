package otelsql

import (
	"context"
	"fmt"
	"strings"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
)

// TenantStore is a tenant schema's record store as a direct Record Layer
// writer opens it: the metadata the schema row is pinned to and the store
// subspace the relational keyspace assigned. Records saved through it are the
// rows SQL reads; nothing here goes through the SQL compiler.
type TenantStore struct {
	MetaData   *recordlayer.RecordMetaData
	Subspace   subspace.Subspace
	Serializer *recordlayer.TransformedRecordSerializer
}

// OpenTenantStore resolves an existing tenant schema created by CreateTenant.
// It creates nothing: an unknown database or schema is an error. A nil
// serializer writes records uncompressed, which SQL (Go and Java) reads.
func OpenTenantStore(ctx context.Context, db *recordlayer.FDBDatabase, databasePath, schema string, serializer *recordlayer.TransformedRecordSerializer) (*TenantStore, error) {
	databasePath, _, err := tenantPaths(databasePath, schema)
	if err != nil {
		return nil, err
	}
	schema = strings.ToUpper(schema)
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		return nil, fmt.Errorf("otelsql: open catalog: %w", err)
	}
	// As fleet.PinnedMetadata: the schema's pinned template version, never the latest.
	out, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		s, err := cat.LoadSchema(catalog.NewFDBTransaction(rc), databasePath, schema)
		if err != nil {
			return nil, err
		}
		up, ok := s.SchemaTemplate().(interface {
			Underlying() *recordlayer.RecordMetaData
		})
		if !ok {
			return nil, fmt.Errorf("otelsql: template of %s/%s is not Record Layer metadata", databasePath, schema)
		}
		return up.Underlying(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("otelsql: load schema %s/%s: %w", databasePath, schema, err)
	}
	ss, err := ks.LookupSchemaSubspace(ctx, db, databasePath, schema)
	if err != nil {
		return nil, fmt.Errorf("otelsql: schema store %s/%s: %w", databasePath, schema, err)
	}
	return &TenantStore{MetaData: out.(*recordlayer.RecordMetaData), Subspace: ss, Serializer: serializer}, nil
}

// OpenTenantStoreCompiled is OpenTenantStore with the schema's metadata bound
// to the compiled OTEL_V1 types (otelv1pb), whose records BuildCompiled makes.
// The binding is Java's setLocalFileDescriptor: otelv1pb must validate as an
// evolution of the schema's stored records descriptor, so a schema pinned to a
// template version otelv1pb was not generated from fails here, not at a save.
// Readers of the store (SQL, Java) are unaffected: the records are the same
// bytes.
func OpenTenantStoreCompiled(ctx context.Context, db *recordlayer.FDBDatabase, databasePath, schema string, serializer *recordlayer.TransformedRecordSerializer) (*TenantStore, error) {
	ts, err := OpenTenantStore(ctx, db, databasePath, schema, serializer)
	if err != nil {
		return nil, err
	}
	p, err := ts.MetaData.ToProto()
	if err != nil {
		return nil, fmt.Errorf("otelsql: schema %s/%s metadata: %w", databasePath, schema, err)
	}
	md, err := recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1))
	if err != nil {
		return nil, fmt.Errorf("otelsql: bind schema %s/%s to the compiled OTEL_V1 types: %w", databasePath, schema, err)
	}
	ts.MetaData = md
	return ts, nil
}

// Builder returns a store builder for rc; callers Open it once per transaction.
func (t *TenantStore) Builder(rc *recordlayer.FDBRecordContext) *recordlayer.StoreBuilder {
	b := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(t.MetaData).SetSubspace(t.Subspace)
	if t.Serializer != nil {
		b = b.SetSerializer(t.Serializer)
	}
	return b
}
