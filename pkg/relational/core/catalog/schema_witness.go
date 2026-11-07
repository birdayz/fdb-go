package catalog

import (
	"bytes"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// SchemaWitness is the raw catalog bytes a loaded schema was built from: the
// catalog store's header and every key-value pair of the schema's row and of
// its template version's row. LoadSchema's result is a function of exactly
// these bytes, so while they are unchanged the loaded api.Schema still is the
// catalog's answer, whoever wrote the catalog in between (a Go or Java
// connection, a restore).
type SchemaWitness struct {
	databaseID, schemaName string
	templateName           string
	templateVersion        int
	header                 []byte
	schemaRow, templateRow []fdb.KeyValue
}

// LoadSchemaWitnessed is LoadSchema and the witness of its result. The witness
// reads the keys LoadSchema read, inside the same transaction.
func (c *RecordLayerStoreCatalog) LoadSchemaWitnessed(txn api.Transaction, databaseID, schemaName string) (api.Schema, *SchemaWitness, error) {
	s, err := c.LoadSchema(txn, databaseID, schemaName)
	if err != nil {
		return nil, nil, err
	}
	t := s.SchemaTemplate()
	w, err := c.readWitness(txn, databaseID, schemaName, t.MetadataName(), t.Version())
	if err != nil {
		return nil, nil, err
	}
	return s, w, nil
}

// SchemaUnchanged reports whether the catalog in txn still holds w's bytes.
// It reads the same keys LoadSchema does, in one parallel round trip instead
// of a store open and two dependent record loads.
func (c *RecordLayerStoreCatalog) SchemaUnchanged(txn api.Transaction, w *SchemaWitness) (bool, error) {
	if w == nil {
		return false, nil
	}
	cur, err := c.readWitness(txn, w.databaseID, w.schemaName, w.templateName, w.templateVersion)
	if err != nil {
		return false, err
	}
	return bytes.Equal(cur.header, w.header) && sameRows(cur.schemaRow, w.schemaRow) && sameRows(cur.templateRow, w.templateRow), nil
}

func (c *RecordLayerStoreCatalog) readWitness(txn api.Transaction, databaseID, schemaName, templateName string, templateVersion int) (*SchemaWitness, error) {
	rctx, err := unwrapFDB(txn)
	if err != nil {
		return nil, err
	}
	tx := rctx.Transaction()
	records := c.catalogSubspace.Sub(recordlayer.RecordKey)
	// All three are issued before any is awaited.
	// A record's keys are its primary key's packing and any key it prefixes
	// (split suffixes; none in the unsplit legacy layout).
	schemaRange, err := fdb.PrefixRange(records.Pack(schemaKey(databaseID, schemaName)))
	if err != nil {
		return nil, err
	}
	templateRange, err := fdb.PrefixRange(records.Pack(templateKeyAtVersion(templateName, templateVersion)))
	if err != nil {
		return nil, err
	}
	header := tx.Get(c.catalogSubspace.Pack(tuple.Tuple{recordlayer.StoreInfoKey}))
	schemaRows := tx.GetRange(schemaRange, fdb.RangeOptions{})
	templateRows := tx.GetRange(templateRange, fdb.RangeOptions{})
	w := &SchemaWitness{databaseID: databaseID, schemaName: schemaName, templateName: templateName, templateVersion: templateVersion}
	if w.header, err = header.Get(); err != nil {
		return nil, err
	}
	if w.schemaRow, err = schemaRows.GetSliceWithError(); err != nil {
		return nil, err
	}
	if w.templateRow, err = templateRows.GetSliceWithError(); err != nil {
		return nil, err
	}
	return w, nil
}

func sameRows(a, b []fdb.KeyValue) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i].Key, b[i].Key) || !bytes.Equal(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}
