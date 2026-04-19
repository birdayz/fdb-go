package catalog

import (
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
)

// newSeededCatalog returns an empty store catalog with templateName
// pre-registered in its template catalog — a convenience so test
// bodies can call SaveSchema without first seeding the template.
func newSeededCatalog(t testing.TB, templateName string) (*InMemoryStoreCatalog, *InMemoryTransaction, api.SchemaTemplate) {
	t.Helper()
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTestTemplate(t, templateName)
	if err := c.SchemaTemplateCatalog().CreateTemplate(tx, tmpl); err != nil {
		t.Fatalf("pre-seed template: %v", err)
	}
	return c, tx, tmpl
}

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
	// createDatabaseIfNecessary must error with
	// ErrCodeUndefinedDatabase (not ErrCodeUnknownDatabase — Java
	// uses UNDEFINED_DATABASE for save-path missing-database errors
	// and UNKNOWN_DATABASE for delete-path ones).
	//
	// Java also requires the schema's template exist in the template
	// catalog. Populate that first so we test the DB path, not the
	// template-missing path.
	if err := c.SchemaTemplateCatalog().CreateTemplate(tx, tmpl); err != nil {
		t.Fatalf("pre-seed template: %v", err)
	}
	err := c.SaveSchema(tx, schema, false)
	if err == nil {
		t.Fatal("save without database should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedDatabase {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUndefinedDatabase)
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

	// LoadSchema in a missing database also reports UNDEFINED_SCHEMA
	// (matching Java: loadSchema collapses db-missing and
	// schema-missing into the same ErrorCode — the primary-key lookup
	// can't distinguish the two).
	_, err = c.LoadSchema(tx, "/nope", "public")
	if err == nil {
		t.Fatal("LoadSchema(missing db) should error")
	}
	if errors.As(err, &apiErr); apiErr.Code != api.ErrCodeUndefinedSchema {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUndefinedSchema)
	}
}

func TestStoreCatalog_SaveSchemaValidation(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "demo")

	// Java's CatalogValidator.validateSchema: empty schema name,
	// empty database name, empty template name, or negative template
	// version all surface as ErrCodeInvalidParameter before the save
	// hits storage.
	// Two of the five validation cases can't be reached through a
	// concrete RecordLayerSchemaTemplate (MetadataName() is required
	// at construction, Version() is non-negative by construction), so
	// use gomock stubs for those two.
	ctrl := gomock.NewController(t)
	mockTmplEmpty := api.NewMockSchemaTemplate(ctrl)
	mockTmplEmpty.EXPECT().MetadataName().Return("").AnyTimes()
	mockTmplEmpty.EXPECT().Version().Return(0).AnyTimes()
	schemaEmptyTmpl := api.NewMockSchema(ctrl)
	schemaEmptyTmpl.EXPECT().MetadataName().Return("s1").AnyTimes()
	schemaEmptyTmpl.EXPECT().DatabaseName().Return("/db").AnyTimes()
	schemaEmptyTmpl.EXPECT().SchemaTemplate().Return(mockTmplEmpty).AnyTimes()

	mockTmplNeg := api.NewMockSchemaTemplate(ctrl)
	mockTmplNeg.EXPECT().MetadataName().Return("t").AnyTimes()
	mockTmplNeg.EXPECT().Version().Return(-1).AnyTimes()
	schemaNegVer := api.NewMockSchema(ctrl)
	schemaNegVer.EXPECT().MetadataName().Return("s1").AnyTimes()
	schemaNegVer.EXPECT().DatabaseName().Return("/db").AnyTimes()
	schemaNegVer.EXPECT().SchemaTemplate().Return(mockTmplNeg).AnyTimes()

	cases := []struct {
		name   string
		schema api.Schema
	}{
		{"nil schema", nil},
		{"empty schema name", tmpl.GenerateSchema("/db", "")},
		{"empty database name", tmpl.GenerateSchema("", "s1")},
		{"empty template name", schemaEmptyTmpl},
		{"negative version", schemaNegVer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.SaveSchema(tx, tc.schema, true)
			if err == nil {
				t.Fatalf("SaveSchema(%s) succeeded", tc.name)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter {
				t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeInvalidParameter)
			}
		})
	}
}

func TestStoreCatalog_SaveSchemaRequiresKnownTemplate(t *testing.T) {
	t.Parallel()
	// Java compliance: SaveSchema asserts that the schema template
	// (name, version) exists in the SchemaTemplateCatalog. If the
	// template isn't registered, ErrCodeUnknownSchemaTemplate.
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	tmpl := buildTestTemplate(t, "unregistered")
	schema := tmpl.GenerateSchema("/db", "s1")

	err := c.SaveSchema(tx, schema, true)
	if err == nil {
		t.Fatal("SaveSchema with unregistered template should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknownSchemaTemplate {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUnknownSchemaTemplate)
	}

	// After registering, the save succeeds.
	if err := c.SchemaTemplateCatalog().CreateTemplate(tx, tmpl); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSchema(tx, schema, true); err != nil {
		t.Errorf("SaveSchema after template register: %v", err)
	}
}

func TestStoreCatalog_DoesSchemaExist(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "demo")
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
	c, tx, tmpl := newSeededCatalog(t, "demo")
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

func TestStoreCatalog_RepairSchema(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "demo")
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	// Happy path: refresh an existing schema. For the in-memory impl
	// this re-generates from the same template, so the stored Schema
	// points to the same template object.
	if err := c.RepairSchema(tx, "/db", "s"); err != nil {
		t.Fatalf("RepairSchema: %v", err)
	}
	refreshed, err := c.LoadSchema(tx, "/db", "s")
	if err != nil {
		t.Fatalf("LoadSchema after repair: %v", err)
	}
	if refreshed.SchemaTemplate().MetadataName() != tmpl.MetadataName() {
		t.Errorf("refreshed template name %q, want %q", refreshed.SchemaTemplate().MetadataName(), tmpl.MetadataName())
	}
}

func TestStoreCatalog_RepairSchemaMissingSchema(t *testing.T) {
	t.Parallel()
	c, tx, _ := newSeededCatalog(t, "demo")
	err := c.RepairSchema(tx, "/db", "missing")
	if err == nil {
		t.Fatal("RepairSchema of missing schema should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedSchema {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUndefinedSchema)
	}
}

func TestStoreCatalog_RepairSchemaMissingTemplate(t *testing.T) {
	t.Parallel()
	// Seed a schema then delete its template. Repair must fail with
	// ErrCodeUnknownSchemaTemplate since there's nothing to re-bind to.
	c, tx, tmpl := newSeededCatalog(t, "demo")
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	if err := c.SchemaTemplateCatalog().DeleteTemplate(tx, tmpl.MetadataName(), true); err != nil {
		t.Fatal(err)
	}
	err := c.RepairSchema(tx, "/db", "s")
	if err == nil {
		t.Fatal("RepairSchema after template delete should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknownSchemaTemplate {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUnknownSchemaTemplate)
	}
}

func TestStoreCatalog_ListSchemasAcrossDatabases(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "demo")
	// Out-of-order inserts across three databases; listing must sort
	// by (database_name, schema_name).
	for _, pair := range [][2]string{
		{"/c", "z"}, {"/a", "m"}, {"/b", "p"}, {"/a", "a"}, {"/b", "q"},
	} {
		if err := c.SaveSchema(tx, tmpl.GenerateSchema(pair[0], pair[1]), true); err != nil {
			t.Fatalf("SaveSchema(%s, %s): %v", pair[0], pair[1], err)
		}
	}
	rs, err := c.ListSchemas(tx, nil)
	if err != nil {
		t.Fatalf("ListSchemas: %v", err)
	}
	defer rs.Close()

	var got [][2]string
	for rs.Next() {
		db, _ := rs.String(1)
		s, _ := rs.String(2)
		got = append(got, [2]string{db, s})
	}
	want := [][2]string{
		{"/a", "a"}, {"/a", "m"}, {"/b", "p"}, {"/b", "q"}, {"/c", "z"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestStoreCatalog_DeleteDatabase(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "demo")
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
	c, tx, tmpl := newSeededCatalog(t, "demo")
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
	// An ad-hoc Transaction impl whose Unwrap() does not yield an
	// *InMemoryTransaction must be rejected with an internal error.
	// Catches impl mismatches early instead of misbehaving silently.
	ctrl := gomock.NewController(t)
	wrongTx := api.NewMockTransaction(ctrl)
	// The in-memory catalog calls Unwrap() first; return the mock itself
	// so it can't possibly satisfy the *InMemoryTransaction assertion.
	wrongTx.EXPECT().Unwrap().Return(wrongTx).AnyTimes()
	err := c.CreateDatabase(wrongTx, "/db")
	if err == nil {
		t.Fatal("CreateDatabase(wrong tx) succeeded")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInternalError {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeInternalError)
	}
}

func TestStoreCatalog_ListResultSetColumnNames(t *testing.T) {
	t.Parallel()
	c, tx, tmpl := newSeededCatalog(t, "demo")
	_ = c.CreateDatabase(tx, "/db")
	_ = c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s1"), true)

	t.Run("ListDatabases", func(t *testing.T) {
		t.Parallel()
		rs, err := c.ListDatabases(tx, nil)
		if err != nil {
			t.Fatalf("ListDatabases: %v", err)
		}
		defer rs.Close()
		if !rs.Next() {
			t.Fatal("expected at least one row")
		}
		v, err := rs.StringByName(ColDatabaseID)
		if err != nil {
			t.Fatalf("StringByName(%q): %v", ColDatabaseID, err)
		}
		if v != "/db" {
			t.Errorf("got %q, want /db", v)
		}
	})

	t.Run("ListSchemas", func(t *testing.T) {
		t.Parallel()
		rs, err := c.ListSchemas(tx, nil)
		if err != nil {
			t.Fatalf("ListSchemas: %v", err)
		}
		defer rs.Close()
		if !rs.Next() {
			t.Fatal("expected at least one row")
		}
		dbID, _ := rs.StringByName(ColDatabaseID)
		sname, _ := rs.StringByName(ColSchemaName)
		tname, _ := rs.StringByName(ColTemplateName)
		if dbID != "/db" || sname != "s1" || tname != "demo" {
			t.Errorf("got (%q, %q, %q), want (/db, s1, demo)", dbID, sname, tname)
		}
	})

	t.Run("ListSchemasInDatabase", func(t *testing.T) {
		t.Parallel()
		rs, err := c.ListSchemasInDatabase(tx, "/db", nil)
		if err != nil {
			t.Fatalf("ListSchemasInDatabase: %v", err)
		}
		defer rs.Close()
		if !rs.Next() {
			t.Fatal("expected at least one row")
		}
		dbID, _ := rs.StringByName(ColDatabaseID)
		sname, _ := rs.StringByName(ColSchemaName)
		if dbID != "/db" || sname != "s1" {
			t.Errorf("got (%q, %q), want (/db, s1)", dbID, sname)
		}
	})
}
