package catalog

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
)

// buildTestTemplate mirrors the helper in the metadata package but is
// inlined here to avoid import cycles. Three record types with trivial
// primary keys.
func buildTestTemplate(t testing.TB, name string) api.SchemaTemplate {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplate(name, md)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplate: %v", err)
	}
	return tmpl
}

func TestStoreCatalog_CreateAndLoadDatabase(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()

	if ok, err := c.DoesDatabaseExist(tx, "/test"); err != nil || ok {
		t.Errorf("fresh catalog: DoesDatabaseExist = (%v, %v), want (false, nil)", ok, err)
	}

	if err := c.CreateDatabase(tx, "/test"); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	if ok, err := c.DoesDatabaseExist(tx, "/test"); err != nil || !ok {
		t.Errorf("after create: DoesDatabaseExist = (%v, %v), want (true, nil)", ok, err)
	}

	// Re-create must error with ErrCodeDatabaseAlreadyExists.
	err := c.CreateDatabase(tx, "/test")
	if err == nil {
		t.Fatal("duplicate CreateDatabase succeeded, want error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("not *api.Error: %v", err)
	}
	if apiErr.Code != api.ErrCodeDatabaseAlreadyExists {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeDatabaseAlreadyExists)
	}
}

func TestStoreCatalog_SaveAndLoadSchema(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()

	tmpl := buildTestTemplate(t, "demo")
	schema := tmpl.GenerateSchema("/db1", "public")

	// Saving without the database present and without
	// createDatabaseIfNecessary must error.
	err := c.SaveSchema(tx, schema, false)
	if err == nil {
		t.Fatal("save without database should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknownDatabase {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUnknownDatabase)
	}

	// With createDatabaseIfNecessary, save succeeds and creates the db.
	if err := c.SaveSchema(tx, schema, true); err != nil {
		t.Fatalf("SaveSchema: %v", err)
	}
	if ok, _ := c.DoesDatabaseExist(tx, "/db1"); !ok {
		t.Error("database not auto-created")
	}

	// LoadSchema returns what we saved.
	got, err := c.LoadSchema(tx, "/db1", "public")
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	if got.MetadataName() != "public" {
		t.Errorf("got schema %q, want public", got.MetadataName())
	}
	if got.DatabaseName() != "/db1" {
		t.Errorf("got database %q, want /db1", got.DatabaseName())
	}

	// LoadSchema on a missing schema errors.
	_, err = c.LoadSchema(tx, "/db1", "missing")
	if err == nil {
		t.Fatal("LoadSchema(missing) should error")
	}
	if errors.As(err, &apiErr); apiErr.Code != api.ErrCodeUndefinedSchema {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUndefinedSchema)
	}

	// LoadSchema in a missing database errors with UnknownDatabase.
	_, err = c.LoadSchema(tx, "/nope", "public")
	if err == nil {
		t.Fatal("LoadSchema(missing db) should error")
	}
	if errors.As(err, &apiErr); apiErr.Code != api.ErrCodeUnknownDatabase {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUnknownDatabase)
	}
}

func TestStoreCatalog_DoesSchemaExist(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTestTemplate(t, "demo")
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s1"), true)

	for _, tc := range []struct {
		db, schema string
		want       bool
	}{
		{"/db", "s1", true},
		{"/db", "s2", false},
		{"/other", "s1", false},
	} {
		got, err := c.DoesSchemaExist(tx, tc.db, tc.schema)
		if err != nil {
			t.Errorf("DoesSchemaExist(%s, %s): %v", tc.db, tc.schema, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DoesSchemaExist(%s, %s) = %v, want %v", tc.db, tc.schema, got, tc.want)
		}
	}
}

func TestStoreCatalog_DeleteSchema(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTestTemplate(t, "demo")
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s1"), true)

	if err := c.DeleteSchema(tx, "/db", "s1"); err != nil {
		t.Fatalf("DeleteSchema: %v", err)
	}
	if ok, _ := c.DoesSchemaExist(tx, "/db", "s1"); ok {
		t.Error("DoesSchemaExist = true after Delete")
	}

	// Delete of missing schema errors with UndefinedSchema.
	err := c.DeleteSchema(tx, "/db", "s1")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedSchema {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUndefinedSchema)
	}
}

func TestStoreCatalog_DeleteDatabase(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTestTemplate(t, "demo")
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s1"), true)
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s2"), false)

	ok, err := c.DeleteDatabase(tx, "/db", true)
	if err != nil {
		t.Fatalf("DeleteDatabase: %v", err)
	}
	if !ok {
		t.Error("DeleteDatabase returned false on success path")
	}

	// All schemas should be gone too.
	if exists, _ := c.DoesSchemaExist(tx, "/db", "s1"); exists {
		t.Error("s1 still exists after DeleteDatabase")
	}
	if exists, _ := c.DoesSchemaExist(tx, "/db", "s2"); exists {
		t.Error("s2 still exists after DeleteDatabase")
	}

	// Delete of missing database with throwIfDoesNotExist=true errors;
	// with throwIfDoesNotExist=false is a no-op.
	_, err = c.DeleteDatabase(tx, "/db", true)
	if err == nil {
		t.Error("DeleteDatabase(missing, throw) did not error")
	}
	ok, err = c.DeleteDatabase(tx, "/db", false)
	if err != nil {
		t.Errorf("DeleteDatabase(missing, no-throw): %v", err)
	}
	if !ok {
		t.Error("DeleteDatabase(missing, no-throw) returned false")
	}
}

func TestStoreCatalog_ListDatabases(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	for _, dbURI := range []string{"/c", "/a", "/b"} {
		if err := c.CreateDatabase(tx, dbURI); err != nil {
			t.Fatalf("CreateDatabase(%s): %v", dbURI, err)
		}
	}

	rs, err := c.ListDatabases(tx, nil)
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	defer rs.Close()

	// Rows should come in sorted order.
	var got []string
	for rs.Next() {
		s, err := rs.String(1)
		if err != nil {
			t.Fatalf("row String(1): %v", err)
		}
		got = append(got, s)
	}
	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStoreCatalog_ListSchemasInDatabase(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTestTemplate(t, "demo")
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "c"), true)
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "a"), false)
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "b"), false)
	// Different DB — MUST NOT appear in a narrowed list.
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/other", "x"), true)

	rs, err := c.ListSchemasInDatabase(tx, "/db", nil)
	if err != nil {
		t.Fatalf("ListSchemasInDatabase: %v", err)
	}
	defer rs.Close()

	var rows [][2]string
	for rs.Next() {
		db, err := rs.String(1)
		if err != nil {
			t.Fatalf("String(1): %v", err)
		}
		schema, err := rs.String(2)
		if err != nil {
			t.Fatalf("String(2): %v", err)
		}
		rows = append(rows, [2]string{db, schema})
	}

	want := [][2]string{{"/db", "a"}, {"/db", "b"}, {"/db", "c"}}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for i := range rows {
		if rows[i] != want[i] {
			t.Errorf("row %d: got %v, want %v", i, rows[i], want[i])
		}
	}
}

func TestStoreCatalog_ClosedTransactionFails(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	_ = tx.Commit()

	err := c.CreateDatabase(tx, "/db")
	if err == nil {
		t.Fatal("CreateDatabase on closed tx succeeded")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeTransactionInactive {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeTransactionInactive)
	}
}

func TestStoreCatalog_WrongTransactionType(t *testing.T) {
	t.Parallel()
	c := NewInMemoryStoreCatalog()
	// An ad-hoc Transaction impl that isn't *InMemoryTransaction must
	// be rejected with an internal error. Catches impl mismatches
	// early instead of misbehaving silently.
	err := c.CreateDatabase(stubTx{}, "/db")
	if err == nil {
		t.Fatal("CreateDatabase(stub tx) succeeded")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInternalError {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeInternalError)
	}
}

type stubTx struct{}

func (stubTx) Commit() error                               { return nil }
func (stubTx) Abort() error                                { return nil }
func (stubTx) Close() error                                { return nil }
func (stubTx) IsClosed() bool                              { return false }
func (stubTx) BoundSchemaTemplate() api.SchemaTemplate     { return nil }
func (stubTx) SetBoundSchemaTemplate(_ api.SchemaTemplate) {}
func (stubTx) UnsetBoundSchemaTemplate()                   {}
