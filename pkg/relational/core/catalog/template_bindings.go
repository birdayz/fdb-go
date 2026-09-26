package catalog

import (
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// boundSchema is a schema row naming a template version: an entry of the
// catalog's TEMPLATES_VALUE_INDEX on SCHEMAS, keyed (TEMPLATE_NAME,
// TEMPLATE_VERSION, DATABASE_ID, SCHEMA_NAME) as Java keys it
// (SchemaSystemTable.java:58-63).
type boundSchema struct {
	version            int
	databaseID, schema string
}

// The version guard (RFC-257 WS-J section 2, a declared Go extension). A schema
// row names its template by (name, version), and neither engine checks for
// bound schemas when a template, or one version of it, is dropped. Go then
// refuses the two writes that would bind those schemas to metadata they were
// not stored under: creating a version of a template while a schema binds a
// dropped version of it above the latest stored one (every version, for a
// fresh template), and deleting a version a schema binds. The target accepts
// both, re-binding the schemas silently; fleet.RestoreTemplateVersion is the
// way to put a dropped bound version back.

// errBoundOnCreate is the guard's refusal of a template version.
func errBoundOnCreate(name string, version int, b boundSchema) error {
	return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"schema template %s version %d cannot be created: schemas are still bound to its dropped version %d (%s/%s)",
		name, version, b.version, b.databaseID, b.schema)
}

// errBoundOnDelete is the guard's refusal of a template version's deletion.
func errBoundOnDelete(name string, version int, b boundSchema) error {
	return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
		"schema template %s version %d cannot be deleted: schemas are still bound to it (%s/%s)",
		name, version, b.databaseID, b.schema)
}

// errTemplateVersionNotInCatalog is Java's refusal of a template version the
// catalog does not hold (RecordLayerStoreSchemaTemplateCatalog.java:203-204):
// the exact-version load, and so the load of a schema row whose bound version
// is gone, which is what refuses SaveSchema and RepairSchema over it.
func errTemplateVersionNotInCatalog(name string, version int) error {
	return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
		"SchemaTemplate=%s, version=%d is not in catalog", name, version)
}

// errTemplateNotInCatalog is Java's refusal of the latest-version load of a
// template the catalog does not hold (RecordLayerStoreSchemaTemplateCatalog
// .java:171-172).
func errTemplateNotInCatalog(name string) error {
	return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate, "SchemaTemplate '%s' is not in catalog", name)
}

// firstBinding is the first TEMPLATES_VALUE_INDEX entry, in key order, among
// the bindings of templateName at versions from `from` through `through`
// (from < 0: from the first version; through < 0: through the last), or nil.
// One serializable range read of limit 1, so its read-conflict range ends
// just past the entry it returns.
//
// An index that is not READABLE (disabled, or being built) holds none or only
// some of the bindings, so the read fails closed rather than answering "none".
func firstBinding(store *recordlayer.FDBRecordStore, templateName string, from, through int) (*boundSchema, error) {
	idx := store.GetRecordMetaData().GetIndex(IdxTemplatesValue)
	if idx == nil {
		return nil, api.NewErrorf(api.ErrCodeInternalError, "catalog index %s is missing", IdxTemplatesValue)
	}
	if state := store.GetIndexState(IdxTemplatesValue); state != recordlayer.IndexStateReadable {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"catalog index %s is %v, so the schemas bound to template %s cannot be read", IdxTemplatesValue, state, templateName)
	}
	sub := store.IndexSubspace(idx)
	nameRange, err := fdb.PrefixRange(sub.Pack(tuple.Tuple{templateName}))
	if err != nil {
		return nil, err
	}
	begin, end := nameRange.Begin, nameRange.End
	if from >= 0 {
		begin = fdb.Key(sub.Pack(tuple.Tuple{templateName, int64(from)}))
	}
	if through >= 0 {
		r, err := fdb.PrefixRange(sub.Pack(tuple.Tuple{templateName, int64(through)}))
		if err != nil {
			return nil, err
		}
		end = r.End
	}
	kvs, err := store.Context().Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	if len(kvs) == 0 {
		return nil, nil
	}
	t, err := sub.Unpack(kvs[0].Key)
	if err != nil {
		return nil, err
	}
	if len(t) < 4 {
		return nil, api.NewErrorf(api.ErrCodeInternalError, "catalog index %s entry %v is short", IdxTemplatesValue, t)
	}
	version, vok := t[1].(int64)
	db, dok := t[2].(string)
	schema, sok := t[3].(string)
	if !vok || !dok || !sok {
		return nil, api.NewErrorf(api.ErrCodeInternalError, "catalog index %s entry %v is malformed", IdxTemplatesValue, t)
	}
	return &boundSchema{version: int(version), databaseID: db, schema: schema}, nil
}
