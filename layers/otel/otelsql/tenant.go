package otelsql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
	"google.golang.org/protobuf/proto"
)

// TemplateMismatchError is a catalog whose OTEL_V1 template, or a tenant
// schema bound to it, is not the one this build generates: a store written
// under another template definition (a stale development cluster), a later
// template version, or a schema bound to another template or version. The
// remedy is a fresh cluster or schema; nothing here migrates stores.
type TemplateMismatchError struct {
	DatabasePath, Schema string // empty when the template itself differs
	Reason               string
}

func (e *TemplateMismatchError) Error() string {
	if e.Schema == "" {
		return fmt.Sprintf("otelsql: stored template %s differs from this build's: %s", TemplateName, e.Reason)
	}
	return fmt.Sprintf("otelsql: schema %s/%s: %s", e.DatabasePath, e.Schema, e.Reason)
}

var built struct {
	once sync.Once
	tmpl *metadata.RecordLayerSchemaTemplate
	md   *recordlayer.RecordMetaData // bound to otelv1pb
	err  error
}

// builtTemplate is TemplateDDL's template, built once, and its metadata bound
// to the compiled types.
func builtTemplate() (*metadata.RecordLayerSchemaTemplate, *recordlayer.RecordMetaData, error) {
	built.once.Do(func() {
		if built.tmpl, built.err = embedded.BuildSchemaTemplateFromDDL(TemplateDDL()); built.err != nil {
			return
		}
		p, err := built.tmpl.Underlying().ToProto()
		if err != nil {
			built.err = err
			return
		}
		built.md, built.err = recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1))
	})
	if built.err != nil {
		return nil, nil, fmt.Errorf("otelsql: build template %s: %w", TemplateName, built.err)
	}
	return built.tmpl, built.md, nil
}

// EnsureTenant makes the tenant schema's store ready for a direct writer and
// returns it bound to the compiled OTEL_V1 types (as OpenTenantStoreCompiled),
// with no serializer set. In one catalog transaction, configured by configure
// (nil for none) and retried as db.Run retries, it creates whichever of the
// OTEL_V1 template, the database and the schema with its store is absent, and
// verifies what is present: a stored template or a schema binding that is not
// this build's is a *TemplateMismatchError, and a schema whose store is gone
// fails its open. Concurrent openers conflict on the catalog rows they create,
// and the retry finds the winner's. No SQL connection is used.
func EnsureTenant(ctx context.Context, db *recordlayer.FDBDatabase, databasePath, schema string, configure recordlayer.TransactionConfigurer) (*TenantStore, error) {
	databasePath, _, err := tenantPaths(databasePath, schema)
	if err != nil {
		return nil, err
	}
	schema = strings.ToUpper(schema)
	tmpl, md, err := builtTemplate()
	if err != nil {
		return nil, err
	}
	want, err := tmpl.Underlying().ToProto()
	if err != nil {
		return nil, err
	}
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		return nil, fmt.Errorf("otelsql: open catalog: %w", err)
	}
	out, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		if err := rc.Configure(configure); err != nil {
			return nil, err
		}
		txn := catalog.NewFDBTransaction(rc)
		if err := cat.Initialize(txn); err != nil {
			return nil, err
		}
		if err := ensureTemplate(txn, cat.SchemaTemplateCatalog(), tmpl, want); err != nil {
			return nil, err
		}
		exists, err := cat.DoesDatabaseExist(txn, databasePath)
		if err != nil {
			return nil, err
		}
		if !exists {
			if err := ddl.NewCreateDatabaseConstantAction(databasePath, cat).Execute(txn); err != nil {
				return nil, err
			}
		}
		created := false
		s, err := cat.LoadSchema(txn, databasePath, schema)
		var apiErr *api.Error
		switch {
		case errors.As(err, &apiErr) && apiErr.Code == api.ErrCodeUndefinedSchema:
			if err := ddl.NewCreateSchemaConstantAction(databasePath, schema, TemplateName, cat, ks).Execute(txn); err != nil {
				return nil, err
			}
			created = true
		case err != nil:
			return nil, err
		default:
			if bound := s.SchemaTemplate(); bound.MetadataName() != TemplateName || bound.Version() != TemplateVersion {
				return nil, &TemplateMismatchError{
					DatabasePath: databasePath, Schema: schema,
					Reason: fmt.Sprintf("bound to template %s version %d, want %s version %d", bound.MetadataName(), bound.Version(), TemplateName, TemplateVersion),
				}
			}
		}
		ss, err := ks.SchemaSubspace(rc, databasePath, schema)
		if err != nil {
			return nil, err
		}
		if !created {
			// Open, not CreateOrOpen: a schema whose store was removed is an error.
			if _, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ss).Open(); err != nil {
				return nil, fmt.Errorf("otelsql: open schema %s/%s store: %w", databasePath, schema, err)
			}
		}
		return ss, nil
	})
	if err != nil {
		return nil, err
	}
	return &TenantStore{DatabasePath: databasePath, Schema: schema, MetaData: md, Subspace: out.(subspace.Subspace)}, nil
}

// ensureTemplate saves tmpl when no version of OTEL_V1 is stored, and
// otherwise requires the latest stored version to be TemplateVersion with
// want's metadata.
func ensureTemplate(txn api.Transaction, tc api.SchemaTemplateCatalog, tmpl *metadata.RecordLayerSchemaTemplate, want proto.Message) error {
	exists, err := tc.DoesSchemaTemplateExist(txn, TemplateName)
	if err != nil {
		return err
	}
	if !exists {
		return ddl.NewSaveSchemaTemplateConstantAction(tmpl, tc).Execute(txn)
	}
	latest, err := tc.LoadSchemaTemplate(txn, TemplateName)
	if err != nil {
		return err
	}
	if latest.Version() != TemplateVersion {
		return &TemplateMismatchError{Reason: fmt.Sprintf("latest stored version is %d, want %d", latest.Version(), TemplateVersion)}
	}
	stored, err := tc.LoadTemplateProto(txn, TemplateName, TemplateVersion)
	if err != nil {
		return err
	}
	if !proto.Equal(stored, want) {
		return &TemplateMismatchError{Reason: "its metadata is not the one TemplateDDL builds"}
	}
	return nil
}
