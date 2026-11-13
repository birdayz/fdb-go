package otelsql

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
)

// TenantStore is a tenant schema's record store as a direct Record Layer
// writer opens it: the metadata the schema row is pinned to and the store
// subspace the relational keyspace assigned. Records saved through it are the
// rows SQL reads; nothing here goes through the SQL compiler.
type TenantStore struct {
	// DatabasePath and Schema name the store as the catalog does: the
	// /DOMAIN/DATABASE path and the schema identifier, upper case.
	DatabasePath, Schema string
	MetaData             *recordlayer.RecordMetaData
	Subspace             subspace.Subspace
	Serializer           *recordlayer.TransformedRecordSerializer
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
			return nil, &TemplateMismatchError{DatabasePath: databasePath, Schema: schema, Reason: "its template is not Record Layer metadata"}
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
	return &TenantStore{DatabasePath: databasePath, Schema: schema, MetaData: out.(*recordlayer.RecordMetaData), Subspace: ss, Serializer: serializer}, nil
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

// OpenTenantStoreCompiledIn is OpenTenantStoreCompiled for a reader that
// writes nothing: the catalog and keyspace reads run in one transaction that
// run supplies and finishes, and a reader's run cancels it (the catalog
// store's CreateOrOpen may write its header). A schema bound to another
// template than OTEL_V1 at TemplateVersion is a *TemplateMismatchError.
func OpenTenantStoreCompiledIn(run func(func(*recordlayer.FDBRecordContext) error) error, databasePath, schema string, serializer *recordlayer.TransformedRecordSerializer) (*TenantStore, error) {
	databasePath, _, err := tenantPaths(databasePath, schema)
	if err != nil {
		return nil, err
	}
	schema = strings.ToUpper(schema)
	cat, err := catalog.NewRecordLayerStoreCatalog(keyspace.New(subspace.Sub()).CatalogSubspace())
	if err != nil {
		return nil, fmt.Errorf("otelsql: open catalog: %w", err)
	}
	var md *recordlayer.RecordMetaData
	var ss subspace.Subspace
	err = run(func(rc *recordlayer.FDBRecordContext) error {
		s, err := cat.LoadSchema(catalog.NewFDBTransaction(rc), databasePath, schema)
		if err != nil {
			return fmt.Errorf("otelsql: load schema %s/%s: %w", databasePath, schema, err)
		}
		bound := s.SchemaTemplate()
		if bound.MetadataName() != TemplateName || bound.Version() != TemplateVersion {
			return &TemplateMismatchError{
				DatabasePath: databasePath, Schema: schema,
				Reason: fmt.Sprintf("bound to template %s version %d, want %s version %d", bound.MetadataName(), bound.Version(), TemplateName, TemplateVersion),
			}
		}
		up, ok := bound.(interface {
			Underlying() *recordlayer.RecordMetaData
		})
		if !ok {
			return &TemplateMismatchError{DatabasePath: databasePath, Schema: schema, Reason: "its template is not Record Layer metadata"}
		}
		md = up.Underlying()
		if ss, err = lookupSchemaSubspace(rc, databasePath, schema); err != nil {
			return fmt.Errorf("otelsql: schema store %s/%s: %w", databasePath, schema, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p, err := md.ToProto()
	if err != nil {
		return nil, fmt.Errorf("otelsql: schema %s/%s metadata: %w", databasePath, schema, err)
	}
	if md, err = recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1)); err != nil {
		return nil, fmt.Errorf("otelsql: bind schema %s/%s to the compiled OTEL_V1 types: %w", databasePath, schema, err)
	}
	return &TenantStore{DatabasePath: databasePath, Schema: schema, MetaData: md, Subspace: ss, Serializer: serializer}, nil
}

// lookupSchemaSubspace is keyspace.LookupSchemaSubspace in rc: the schema's
// store subspace from the stored directory and interning entries, never
// created; a name without an entry is UNDEFINED_SCHEMA.
func lookupSchemaSubspace(rc *recordlayer.FDBRecordContext, databasePath, schema string) (subspace.Subspace, error) {
	path, err := keyspace.ToDatabasePath(databasePath)
	if err != nil {
		return nil, err
	}
	missing := api.NewErrorf(api.ErrCodeUndefinedSchema, "Schema <%s/%s> has no record store", databasePath, schema)
	domain, ok, err := recordlayer.ReadInTransaction(rc, recordlayer.GlobalDirectoryLayer{}, path.Domain)
	if err != nil || !ok {
		return nil, cmp.Or(err, error(missing))
	}
	scope := recordlayer.NewScopedInterningLayer(subspace.Sub(domain.Value, keyspace.InterningLayerValue))
	db, ok, err := recordlayer.ReadInTransaction(rc, scope, path.Database)
	if err != nil || !ok {
		return nil, cmp.Or(err, error(missing))
	}
	sv, ok, err := recordlayer.ReadInTransaction(rc, scope, schema)
	if err != nil || !ok {
		return nil, cmp.Or(err, error(missing))
	}
	return subspace.Sub(domain.Value, db.Value, sv.Value), nil
}

// Builder returns a store builder for rc; callers Open it once per transaction.
func (t *TenantStore) Builder(rc *recordlayer.FDBRecordContext) *recordlayer.StoreBuilder {
	b := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(t.MetaData).SetSubspace(t.Subspace)
	if t.Serializer != nil {
		b = b.SetSerializer(t.Serializer)
	}
	return b
}
