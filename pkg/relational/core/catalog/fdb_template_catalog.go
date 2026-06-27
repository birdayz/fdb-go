package catalog

import (
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// RecordLayerStoreSchemaTemplateCatalog is the FDB-backed
// api.SchemaTemplateCatalog. Mirrors Java's
// RecordLayerStoreSchemaTemplateCatalog — versioned templates keyed by
// (templateName, templateVersion), with META_DATA holding the
// serialised RecordMetaData proto.
type RecordLayerStoreSchemaTemplateCatalog struct {
	parent *RecordLayerStoreCatalog
}

func (c *RecordLayerStoreSchemaTemplateCatalog) openStore(txn api.Transaction) (*recordlayer.FDBRecordStore, error) {
	return c.parent.openStore(txn)
}

// templateKeyAtVersion builds the PK tuple for one (name, version).
func templateKeyAtVersion(name string, version int) tuple.Tuple {
	return tuple.Tuple{SchemaTemplateRecordTypeKey, name, int64(version)}
}

// DoesSchemaTemplateExist reports whether any version of templateName exists.
func (c *RecordLayerStoreSchemaTemplateCatalog) DoesSchemaTemplateExist(txn api.Transaction, templateName string) (bool, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return false, err
	}
	cursor := store.ScanRecordsInRange(
		tuple.Tuple{SchemaTemplateRecordTypeKey, templateName},
		tuple.Tuple{SchemaTemplateRecordTypeKey, templateName},
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, recordlayer.ForwardScan(),
	)
	r, err := cursor.OnNext(store.Context().Context())
	if err != nil {
		return false, err
	}
	return r.HasNext(), nil
}

// DoesSchemaTemplateExistAtVersion — exact (name, version) lookup.
func (c *RecordLayerStoreSchemaTemplateCatalog) DoesSchemaTemplateExistAtVersion(txn api.Transaction, templateName string, version int) (bool, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return false, err
	}
	rec, err := store.LoadRecord(templateKeyAtVersion(templateName, version))
	if err != nil {
		return false, err
	}
	return rec != nil, nil
}

// LoadSchemaTemplate loads the latest version of templateName. Matches
// Java: reverse scan, return first result. Errors with
// ErrCodeUnknownSchemaTemplate when no version exists.
func (c *RecordLayerStoreSchemaTemplateCatalog) LoadSchemaTemplate(txn api.Transaction, templateName string) (api.SchemaTemplate, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return nil, err
	}
	cursor := store.ScanRecordsInRange(
		tuple.Tuple{SchemaTemplateRecordTypeKey, templateName},
		tuple.Tuple{SchemaTemplateRecordTypeKey, templateName},
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, recordlayer.ReverseScan(),
	)
	r, err := cursor.OnNext(store.Context().Context())
	if err != nil {
		return nil, err
	}
	if !r.HasNext() {
		return nil, api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
			"schema template %q is not in the catalog", templateName)
	}
	msg, castOK := r.GetValue().Record.(*gen.Templates)
	if !castOK {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"catalog template row has unexpected type %T", r.GetValue().Record)
	}
	return deserializeTemplate(msg)
}

// LoadSchemaTemplateAtVersion — exact version lookup.
func (c *RecordLayerStoreSchemaTemplateCatalog) LoadSchemaTemplateAtVersion(txn api.Transaction, templateName string, version int) (api.SchemaTemplate, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return nil, err
	}
	rec, err := store.LoadRecord(templateKeyAtVersion(templateName, version))
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
			"schema template %q version %d is not in the catalog", templateName, version)
	}
	msg, ok := rec.Record.(*gen.Templates)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"catalog template row has unexpected type %T", rec.Record)
	}
	return deserializeTemplate(msg)
}

// CreateTemplate persists a new (name, version). Returns
// ErrCodeDuplicateSchemaTemplate when (name, version) already exists.
//
// Matches Java's RecordLayerStoreSchemaTemplateCatalog.createTemplate()
// exactly: no evolution validation here. Java only validates metadata
// evolution inside FDBMetaDataStore.saveAndSetCurrent() (record-layer
// level, not relational); the relational createTemplate / saveSchema
// paths do not invoke MetaDataEvolutionValidator. Adding validation in
// Go would diverge — a Go writer would reject templates Java accepts.
// See TODO.md entry on the template-evolution validation gap (open
// both in Java and Go; worth an upstream discussion before unilateral
// Go-side enforcement).
func (c *RecordLayerStoreSchemaTemplateCatalog) CreateTemplate(txn api.Transaction, newTemplate api.SchemaTemplate) error {
	if newTemplate == nil {
		return api.NewError(api.ErrCodeInvalidParameter, "template is nil")
	}
	rl, ok := newTemplate.(*metadata.RecordLayerSchemaTemplate)
	if !ok {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"only *metadata.RecordLayerSchemaTemplate is supported, got %T", newTemplate)
	}
	store, err := c.openStore(txn)
	if err != nil {
		return err
	}
	// Exact-version dupe check. Cheaper than relying on the record
	// layer's ERROR_IF_EXISTS path for now.
	existing, err := store.LoadRecord(templateKeyAtVersion(rl.MetadataName(), rl.Version()))
	if err != nil {
		return err
	}
	if existing != nil {
		return api.NewErrorf(api.ErrCodeDuplicateSchemaTemplate,
			"schema template %q version %d already exists", rl.MetadataName(), rl.Version())
	}

	payload, err := serializeTemplate(rl)
	if err != nil {
		return err
	}
	rec := &gen.Templates{
		TEMPLATE_NAME:    proto.String(rl.MetadataName()),
		TEMPLATE_VERSION: proto.Int32(int32(rl.Version())),
		META_DATA:        payload,
	}
	if _, err := store.SaveRecord(rec); err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "save schema template")
	}
	return nil
}

// ListTemplates enumerates every (name, version) row.
func (c *RecordLayerStoreSchemaTemplateCatalog) ListTemplates(txn api.Transaction) (api.ResultSet, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return nil, err
	}
	cursor := store.ScanRecordsByType(TemplatesRecordName, nil, recordlayer.ForwardScan())
	ctx := store.Context().Context()
	var rows [][]any
	for {
		r, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !r.HasNext() {
			break
		}
		if t, isT := r.GetValue().Record.(*gen.Templates); isT {
			rows = append(rows, []any{
				t.GetTEMPLATE_NAME(),
				t.GetTEMPLATE_VERSION(),
				t.GetMETA_DATA(),
			})
		}
	}
	return newStringResultSet(
		[]string{ColTemplateName, ColTemplateVersion, ColMetaData}, rows), nil
}

// DeleteTemplate removes ALL versions of templateName. When
// throwIfDoesNotExist is true, a missing template raises
// ErrCodeUnknownSchemaTemplate.
func (c *RecordLayerStoreSchemaTemplateCatalog) DeleteTemplate(txn api.Transaction, templateName string, throwIfDoesNotExist bool) error {
	store, err := c.openStore(txn)
	if err != nil {
		return err
	}
	cursor := store.ScanRecordsInRange(
		tuple.Tuple{SchemaTemplateRecordTypeKey, templateName},
		tuple.Tuple{SchemaTemplateRecordTypeKey, templateName},
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, recordlayer.ForwardScan(),
	)
	ctx := store.Context().Context()
	deletedSomething := false
	for {
		r, err := cursor.OnNext(ctx)
		if err != nil {
			return err
		}
		if !r.HasNext() {
			break
		}
		if _, err := store.DeleteRecord(r.GetValue().PrimaryKey); err != nil {
			return err
		}
		deletedSomething = true
	}
	if !deletedSomething && throwIfDoesNotExist {
		return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
			"could not delete unknown schema template %s", templateName)
	}
	return nil
}

// DeleteTemplateVersion removes one exact (name, version).
func (c *RecordLayerStoreSchemaTemplateCatalog) DeleteTemplateVersion(txn api.Transaction, templateName string, version int, throwIfDoesNotExist bool) error {
	store, err := c.openStore(txn)
	if err != nil {
		return err
	}
	deleted, err := store.DeleteRecord(templateKeyAtVersion(templateName, version))
	if err != nil {
		return err
	}
	if !deleted && throwIfDoesNotExist {
		return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
			"could not delete unknown schema template %s version %d", templateName, version)
	}
	return nil
}

// serializeTemplate roundtrips *RecordLayerSchemaTemplate → proto bytes.
// Wire-compatible with Java: RecordMetaData.toProto().toByteArray().
func serializeTemplate(rl *metadata.RecordLayerSchemaTemplate) ([]byte, error) {
	md := rl.Underlying()
	p, err := md.ToProto()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "template to-proto")
	}
	return proto.Marshal(p)
}

// deserializeTemplate reads the META_DATA blob back into an
// api.SchemaTemplate. Note: since we don't persist the SQL-layer
// template version separately, we use md.Version() as the returned
// template version. Java does the same when reading legacy data.
func deserializeTemplate(msg *gen.Templates) (api.SchemaTemplate, error) {
	p := &gen.MetaData{}
	if err := proto.Unmarshal(msg.GetMETA_DATA(), p); err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "template unmarshal")
	}
	md, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "template from-proto")
	}
	return metadata.NewRecordLayerSchemaTemplateWithVersion(
		msg.GetTEMPLATE_NAME(), md, int(msg.GetTEMPLATE_VERSION()))
}

// compile-time assertion.
var _ api.SchemaTemplateCatalog = (*RecordLayerStoreSchemaTemplateCatalog)(nil)
