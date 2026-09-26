package catalog

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// policyCatalog is a store catalog with a way to run a transaction on it: the
// FDB catalog commits a transaction per call, the in-memory one has one
// transaction.
type policyCatalog struct {
	name string
	cat  api.StoreCatalog
	run  func(fn func(tx api.Transaction) error) error
}

// policyCatalogs are the two catalogs' constructors, named.
var policyCatalogs = []struct {
	name string
	new  func(t *testing.T) policyCatalog
}{
	{"FDB", func(t *testing.T) policyCatalog {
		cat, run := newFDBCatalogInSubspace(t)
		return policyCatalog{name: "FDB", cat: cat, run: run}
	}},
	{"in-memory", func(t *testing.T) policyCatalog {
		c := NewInMemoryStoreCatalog()
		tx := NewInMemoryTransaction()
		return policyCatalog{name: "in-memory", cat: c, run: func(fn func(api.Transaction) error) error { return fn(tx) }}
	}},
}

// Every SchemaExistsBehavior against every state of the stored row, on both
// catalogs (ws-j-design.md section 2): the outcome is Java's shouldWrite, with
// its SCHEMA_ALREADY_EXISTS messages, after Java's load of the stored row, which
// fails first when the row's template version is gone. The binding afterwards
// shows whether the save wrote.
func TestSaveSchema_EveryBehaviorOverEveryStoredRow(t *testing.T) {
	t.Parallel()
	type want struct {
		code  api.ErrorCode // "" is success
		msg   string
		bound string // the binding afterwards, "" when not checked
	}
	exists := func(msg string) want { return want{code: api.ErrCodeSchemaAlreadyExists, msg: msg} }
	gone := want{code: api.ErrCodeUnknownSchemaTemplate, msg: "SchemaTemplate=g, version=1 is not in catalog"}
	already := exists("Schema /db/s already exists.")
	type state struct {
		name     string
		existing string // "t@2", "g@1 gone" or ""
		save     string
	}
	states := []state{
		{"absent", "", "t@2"},
		{"the same template and version", "t@2", "t@2"},
		{"a different template", "t@2", "o@1"},
		{"a lower version", "t@2", "t@1"},
		{"a higher version", "t@2", "t@3"},
		{"a bound version that is gone", "g@1 gone", "t@2"},
	}
	wants := map[api.SchemaExistsBehavior][]want{
		api.SchemaExistsError: {
			{bound: "t@2"}, already, already, already, already, gone,
		},
		api.SchemaExistsErrorIfDifferent: {
			{bound: "t@2"},
			{bound: "t@2"},
			exists("Schema /db/s already exists with a different template (t@2 vs o@1)."),
			exists("Schema /db/s already exists with a different template (t@2 vs t@1)."),
			exists("Schema /db/s already exists with a different template (t@2 vs t@3)."),
			gone,
		},
		api.SchemaExistsDoNothing: {
			{bound: "t@2"}, {bound: "t@2"}, {bound: "t@2"}, {bound: "t@2"}, {bound: "t@2"}, gone,
		},
		api.SchemaExistsUpgrade: {
			{bound: "t@2"},
			{bound: "t@2"},
			exists("Cannot upgrade schema /db/s: existing template t does not match new template o."),
			exists("Cannot upgrade schema /db/s: new template version 1 is lower than existing version 2."),
			{bound: "t@3"},
			gone,
		},
	}
	for _, pcs := range policyCatalogs {
		for _, b := range []api.SchemaExistsBehavior{api.SchemaExistsError, api.SchemaExistsErrorIfDifferent, api.SchemaExistsDoNothing, api.SchemaExistsUpgrade} {
			for i, st := range states {
				w := wants[b][i]
				t.Run(fmt.Sprintf("%s/%s/%s", pcs.name, b, st.name), func(t *testing.T) {
					t.Parallel()
					pc := pcs.new(t)
					tc := pc.cat.SchemaTemplateCatalog()
					templates := map[string]api.SchemaTemplate{
						"t@1": buildVersionedTemplate(t, "t", 1), "t@2": buildVersionedTemplate(t, "t", 2),
						"t@3": buildVersionedTemplate(t, "t", 3), "o@1": buildVersionedTemplate(t, "o", 1),
						"g@1": buildVersionedTemplate(t, "g", 1),
					}
					mustRun(t, pc.run, func(tx api.Transaction) error {
						for _, name := range []string{"t@1", "t@2", "t@3", "o@1", "g@1"} {
							if err := tc.CreateTemplate(tx, templates[name]); err != nil {
								return err
							}
						}
						return pc.cat.CreateDatabase(tx, "/db")
					})
					// A save binds the template as stored (CreateTemplate's carry
					// sets its metadata version), as every caller does.
					saveStored := func(tx api.Transaction, name string, b api.SchemaExistsBehavior) error {
						stored, err := tc.LoadSchemaTemplateAtVersion(tx, templates[name].MetadataName(), templates[name].Version())
						if err != nil {
							return err
						}
						return pc.cat.SaveSchema(tx, stored.GenerateSchema("/db", "s"), false, b)
					}
					switch st.existing {
					case "t@2":
						mustRun(t, pc.run, func(tx api.Transaction) error { return saveStored(tx, "t@2", api.SchemaExistsError) })
					case "g@1 gone":
						mustRun(t, pc.run, func(tx api.Transaction) error { return saveStored(tx, "g@1", api.SchemaExistsError) })
						mustRun(t, pc.run, func(tx api.Transaction) error { return tc.DeleteTemplate(tx, "g", true) })
					}
					err := pc.run(func(tx api.Transaction) error { return saveStored(tx, st.save, b) })
					if w.code != "" {
						wantAPIError(t, err, w.code, w.msg)
					} else if err != nil {
						t.Fatalf("save: %v", err)
					}
					var bound string
					lerr := pc.run(func(tx api.Transaction) error {
						s, err := pc.cat.LoadSchema(tx, "/db", "s")
						if err != nil {
							return err
						}
						bound = fmt.Sprintf("%s@%d", s.SchemaTemplate().MetadataName(), s.SchemaTemplate().Version())
						return nil
					})
					switch {
					case st.existing == "g@1 gone":
						// Untouched: still bound to the gone version.
						wantAPIError(t, lerr, gone.code, gone.msg)
					case w.bound != "":
						if lerr != nil || bound != w.bound {
							t.Fatalf("bound to %q (%v), want %q", bound, lerr, w.bound)
						}
					case st.existing == "t@2":
						if lerr != nil || bound != "t@2" {
							t.Fatalf("a refused save changed the binding: %q (%v)", bound, lerr)
						}
					}
				})
			}
		}
	}
}

// Java's order of refusals in saveSchema, on both catalogs: the schema's fields,
// then the database, then the template at the schema's version, and only then
// the stored row's own template. A refused save creates no database, even with
// createDatabaseIfNecessary.
func TestSaveSchema_RefusesInJavasOrder(t *testing.T) {
	t.Parallel()
	for _, pcs := range policyCatalogs {
		t.Run(pcs.name, func(t *testing.T) {
			t.Parallel()
			pc := pcs.new(t)
			tc := pc.cat.SchemaTemplateCatalog()
			g := buildVersionedTemplate(t, "g", 1)
			mustRun(t, pc.run, func(tx api.Transaction) error {
				if err := tc.CreateTemplate(tx, g); err != nil {
					return err
				}
				if err := pc.cat.CreateDatabase(tx, "/db"); err != nil {
					return err
				}
				return pc.cat.SaveSchema(tx, g.GenerateSchema("/db", "s"), false, api.SchemaExistsError)
			})
			mustRun(t, pc.run, func(tx api.Transaction) error { return tc.DeleteTemplate(tx, "g", true) })
			missing := buildVersionedTemplate(t, "missing", 7)

			// An empty name is refused before the missing database.
			err := pc.run(func(tx api.Transaction) error {
				return pc.cat.SaveSchema(tx, missing.GenerateSchema("/nodb", ""), false, api.SchemaExistsError)
			})
			wantAPIError(t, err, api.ErrCodeInvalidParameter, "Field schema_name in Schema must be set!")
			// The missing database before the missing template.
			err = pc.run(func(tx api.Transaction) error {
				return pc.cat.SaveSchema(tx, missing.GenerateSchema("/nodb", "s"), false, api.SchemaExistsError)
			})
			wantAPIError(t, err, api.ErrCodeUndefinedDatabase, "Cannot create schema s because database /nodb does not exist.")
			// The missing template before the stored row's gone version.
			err = pc.run(func(tx api.Transaction) error {
				return pc.cat.SaveSchema(tx, missing.GenerateSchema("/db", "s"), false, api.SchemaExistsDoNothing)
			})
			wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate,
				"Cannot create schema s because schema template missing version 7 does not exist.")
			// A refused save with createDatabaseIfNecessary creates no database.
			err = pc.run(func(tx api.Transaction) error {
				return pc.cat.SaveSchema(tx, missing.GenerateSchema("/newdb", "s"), true, api.SchemaExistsError)
			})
			wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate,
				"Cannot create schema s because schema template missing version 7 does not exist.")
			mustRun(t, pc.run, func(tx api.Transaction) error {
				ok, err := pc.cat.DoesDatabaseExist(tx, "/newdb")
				if err == nil && ok {
					t.Fatal("a refused save created its database")
				}
				return err
			})
		})
	}
}

// RepairSchema is Java's repairSchema on both catalogs: onto the latest version
// with UPGRADE, a no-op on the latest, refused with Java's text when the bound
// version is gone, and, in both, through the rebind validator.
func TestRepairSchema_UpgradesThroughTheValidator(t *testing.T) {
	t.Parallel()
	for _, pcs := range policyCatalogs {
		t.Run(pcs.name, func(t *testing.T) {
			t.Parallel()
			pc := pcs.new(t)
			tc := pc.cat.SchemaTemplateCatalog()
			mustRun(t, pc.run, func(tx api.Transaction) error {
				if err := tc.CreateTemplate(tx, buildVersionedTemplate(t, "r", 1)); err != nil {
					return err
				}
				return pc.cat.SaveSchema(tx, buildVersionedTemplate(t, "r", 1).GenerateSchema("/db", "s"), true, api.SchemaExistsError)
			})
			// On the latest: a no-op.
			mustRun(t, pc.run, func(tx api.Transaction) error { return pc.cat.RepairSchema(tx, "/db", "s") })
			// A new version: the repair rebinds to it.
			mustRun(t, pc.run, func(tx api.Transaction) error {
				return tc.CreateTemplate(tx, buildVersionedTemplate(t, "r", 2))
			})
			mustRun(t, pc.run, func(tx api.Transaction) error { return pc.cat.RepairSchema(tx, "/db", "s") })
			mustRun(t, pc.run, func(tx api.Transaction) error {
				s, err := pc.cat.LoadSchema(tx, "/db", "s")
				if err == nil && s.SchemaTemplate().Version() != 2 {
					t.Fatalf("repaired onto version %d, want 2", s.SchemaTemplate().Version())
				}
				return err
			})
			err := pc.run(func(tx api.Transaction) error { return pc.cat.RepairSchema(tx, "/db", "ghost") })
			wantAPIError(t, err, api.ErrCodeUndefinedSchema, "Schema </db/ghost> does not exist in the catalog!")
			mustRun(t, pc.run, func(tx api.Transaction) error { return tc.DeleteTemplate(tx, "r", true) })
			err = pc.run(func(tx api.Transaction) error { return pc.cat.RepairSchema(tx, "/db", "s") })
			wantAPIError(t, err, api.ErrCodeUnknownSchemaTemplate, "SchemaTemplate=r, version=2 is not in catalog")
		})
	}
}

// The in-memory RepairSchema runs the rebind validator, as the FDB catalog's
// does: a latest version whose record-type key differs from the bound one's
// (stored raw, as CreateTemplate's carry would keep the key) is refused, and
// the binding stays.
func TestInMemory_RepairSchemaRunsTheRebindValidator(t *testing.T) {
	t.Parallel()
	build := func(key int64, version int) api.SchemaTemplate {
		b := metadata.NewSchemaTemplateBuilder().SetName("k")
		b.AddTable("T", []metadata.ColumnSpec{metadata.NewColumnSpec("ID", api.NewLongType(false), 1)}, []string{"ID"})
		tmpl, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		p, err := tmpl.Underlying().ToProto()
		if err != nil {
			t.Fatal(err)
		}
		p.RecordTypes[0].ExplicitKey = &gen.Value{LongValue: proto.Int64(key)}
		md, err := recordlayer.RecordMetaDataFromProto(p)
		if err != nil {
			t.Fatal(err)
		}
		out, err := metadata.NewRecordLayerSchemaTemplateWithVersion("k", md, version)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	c := NewInMemoryStoreCatalog()
	tx := NewInMemoryTransaction()
	if err := c.SchemaTemplateCatalog().CreateTemplate(tx, build(1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSchema(tx, build(1, 1).GenerateSchema("/db", "s"), true, api.SchemaExistsError); err != nil {
		t.Fatal(err)
	}
	c.templates.mu.Lock()
	c.templates.templates["k"][2] = build(0, 2)
	c.templates.mu.Unlock()
	wantAPIError(t, c.RepairSchema(tx, "/db", "s"), api.ErrCodeInvalidSchemaTemplate,
		"cannot rebind schema /db/s from template k@1 to k@2: metadata evolution rejected")
	s, err := c.LoadSchema(tx, "/db", "s")
	if err != nil || s.SchemaTemplate().Version() != 1 {
		t.Fatalf("binding after the refused repair: %v, %v", s, err)
	}
}
