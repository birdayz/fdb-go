package catalog

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// CreateTemplate's route for a new version of a stored template
// (ws-j-design.md section 4, step 3), at the catalog layer: the refusal at or
// below the latest, Java's deleteTemplate sequences it closes, the splice of an
// EQUIVALENT index's stored bytes, and the lane check on every route that
// builds.

// Java's deleteTemplate(name, version) does not look at bindings, so a Java
// delete of a bound version below the latest leaves a binding the version guard
// (which reads from latest + 1) does not see; CreateTemplate at or below the
// latest would re-issue it. Both sequences of ws-j-design.md 4e item 3 are
// refused.
func TestFDB_CreateTemplate_RefusesAReIssueBelowTheLatest(t *testing.T) {
	t.Parallel()
	t.Run("a Java delete of a bound version below the latest", func(t *testing.T) {
		t.Parallel()
		cat, run := newFDBCatalogInSubspace(t)
		tc := cat.SchemaTemplateCatalog()
		for _, v := range []int{2, 3} {
			mustRun(t, run, func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "del", v)) })
		}
		mustRun(t, run, func(tx api.Transaction) error {
			tmpl, err := tc.LoadSchemaTemplateAtVersion(tx, "del", 2)
			if err != nil {
				return err
			}
			return cat.SaveSchema(tx, tmpl.GenerateSchema("/deldb", "two"), true)
		})
		// Java's deleteTemplate: the row goes, the binding stays.
		eraseTemplateRow(t, cat, run, "del", 2)
		err := run(func(tx api.Transaction) error { return tc.CreateTemplate(tx, buildVersionedTemplate(t, "del", 2)) })
		wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate, `template "del": new version 2 must be greater than current version 3`)
	})
	t.Run("DROP, a restore of the latest, then the version a schema still binds", func(t *testing.T) {
		t.Parallel()
		e := newRestoreEnv(t)
		e.name = "redo"
		v2, v3 := demoMetaData(t, 1, nil), demoMetaData(t, 1, nil)
		e.writeRow(2, v2)
		e.writeRow(3, v3)
		e.bind("/db", "two", 2, v2)
		e.bind("/db", "three", 3, v3)
		e.dropTemplate()
		if err := e.restore(3, v3, nil); err != nil {
			t.Fatal(err)
		}
		err := e.run(func(tx api.Transaction) error {
			return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "redo", 2))
		})
		wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate, `template "redo": new version 2 must be greater than current version 3`)
		e.readsBack("/db", "three")
	})
}

// An EQUIVALENT index is carried as the Index message the catalog row holds,
// not as the rebuilt one: a stored index_type, value_expression, unknown field
// and extension-range field survive a save of the same meta-data, where the
// rebuild through indexToProto would drop every one of them. The pin is at the
// catalog layer: the new row's META_DATA is read back and each Index message
// compared, proto-equal (unknown fields compared as bytes), with the stored one.
func TestFDB_CreateTemplate_SplicesTheStoredIndexOfAnEquivalentIndex(t *testing.T) {
	t.Parallel()
	e := newRestoreEnv(t)
	e.name = "splice"
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 1500, protowire.VarintType), 7)
	v1 := demoMetaData(t, 3, func(p *gen.MetaData) {
		price := recordlayer.Field("price").ToKeyExpression()
		qty := recordlayer.Field("quantity").ToKeyExpression()
		// The deprecated fields are set by name (the generated fields are
		// deprecated, which the linter refuses).
		withType := &gen.Index{
			Name: proto.String("BY_TYPE"), RecordType: []string{"Order"}, RootExpression: price,
			SubspaceKey: tuple.Tuple{"BY_TYPE"}.Pack(), AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
		}
		withType.ProtoReflect().Set(indexField("index_type"), protoreflect.ValueOfEnum(gen.Index_UNIQUE.Number()))
		withValue := &gen.Index{
			Name: proto.String("BY_VALUE"), RecordType: []string{"Order"}, RootExpression: price,
			Type: proto.String("value"), SubspaceKey: tuple.Tuple{"BY_VALUE"}.Pack(),
			AddedVersion: proto.Int32(2), LastModifiedVersion: proto.Int32(2),
		}
		withValue.ProtoReflect().Set(indexField("value_expression"), protoreflect.ValueOfMessage(qty.ProtoReflect()))
		withUnknown := &gen.Index{
			Name: proto.String("BY_UNKNOWN"), RecordType: []string{"Order"}, RootExpression: qty,
			Type: proto.String("value"), SubspaceKey: tuple.Tuple{"BY_UNKNOWN"}.Pack(),
			AddedVersion: proto.Int32(3), LastModifiedVersion: proto.Int32(3),
		}
		withUnknown.ProtoReflect().SetUnknown(unknown)
		p.Indexes = append(p.Indexes, withType, withValue, withUnknown)
	})
	e.writeRow(1, v1)
	var stored []*gen.Index
	mustRun(t, e.run, func(tx api.Transaction) error {
		p, err := e.cat.SchemaTemplateCatalog().LoadTemplateProto(tx, "splice", 1)
		stored = p.GetIndexes()
		return err
	})
	// The same meta-data, as a caller holds it after a load: the rebuilt
	// indexes carry none of the deprecated fields or unknown bytes.
	mustRun(t, e.run, func(tx api.Transaction) error {
		loaded, err := e.cat.SchemaTemplateCatalog().LoadSchemaTemplateAtVersion(tx, "splice", 1)
		if err != nil {
			return err
		}
		md := loaded.(*metadata.RecordLayerSchemaTemplate).Underlying()
		rebuilt, err := md.ToProto()
		if err != nil {
			return err
		}
		for _, idx := range rebuilt.GetIndexes() {
			m := idx.ProtoReflect()
			if m.Has(indexField("index_type")) || m.Has(indexField("value_expression")) || len(m.GetUnknown()) > 0 {
				t.Fatalf("the rebuild kept %s's stored fields; the splice is not exercised", idx.GetName())
			}
		}
		v2, err := metadata.NewRecordLayerSchemaTemplateWithVersion("splice", md, 2)
		if err != nil {
			return err
		}
		return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, v2)
	})
	mustRun(t, e.run, func(tx api.Transaction) error {
		p, err := e.cat.SchemaTemplateCatalog().LoadTemplateProto(tx, "splice", 2)
		if err != nil {
			return err
		}
		byName := map[string]*gen.Index{}
		for _, idx := range p.GetIndexes() {
			byName[idx.GetName()] = idx
		}
		for _, want := range stored {
			got := byName[want.GetName()]
			if !proto.Equal(got, want) {
				t.Errorf("%s: saved %v, stored %v", want.GetName(), got, want)
			}
		}
		if p.GetVersion() != 4 {
			t.Errorf("metadata version %d, want the stored 3 + 1", p.GetVersion())
		}
		// What was validated is what is stored: the row loads.
		_, err = e.cat.SchemaTemplateCatalog().LoadSchemaTemplateAtVersion(tx, "splice", 2)
		return err
	})
}

// indexField is a field of the Index message by name.
func indexField(name protoreflect.Name) protoreflect.FieldDescriptor {
	return (&gen.Index{}).ProtoReflect().Descriptor().Fields().ByName(name)
}

// laneIndex is an index of Order over root, a key expression proto.
func laneIndex(name string, root *gen.KeyExpression) *gen.Index {
	return &gen.Index{
		Name: proto.String(name), RecordType: []string{"Order"}, RootExpression: root, Type: proto.String("value"),
		SubspaceKey: tuple.Tuple{name}.Pack(), AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
	}
}

func fn(name string, args ...*gen.KeyExpression) *gen.KeyExpression {
	arguments := args[0]
	if len(args) > 1 {
		arguments = &gen.KeyExpression{Then: &gen.Then{Child: args}}
	}
	return &gen.KeyExpression{Function: &gen.Function{Name: proto.String(name), Arguments: arguments}}
}

func field(name string) *gen.KeyExpression { return recordlayer.Field(name).ToKeyExpression() }

func intLit(v int32) *gen.KeyExpression {
	return &gen.KeyExpression{Value: &gen.Value{IntValue: proto.Int32(v)}}
}

func longLit(v int64) *gen.KeyExpression {
	return &gen.KeyExpression{Value: &gen.Value{LongValue: proto.Int64(v)}}
}

// laneTemplate is the demo meta-data at template version `version` with one
// index over root, built by hand (no DDL clause sees it).
func laneTemplate(t *testing.T, name string, version int, root *gen.KeyExpression) api.SchemaTemplate {
	t.Helper()
	p, err := buildVersionedTemplate(t, name, 1).(*metadata.RecordLayerSchemaTemplate).Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	p.Version = proto.Int32(1)
	p.Indexes = append(p.Indexes, laneIndex("LANE_IX", root))
	md, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("load %v: %v", root, err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, version)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// The lane check (ws-j-design.md section 3.2): an arithmetic function key whose
// operands' types, typed as the Values Java builds for them, name no row of
// ArithmeticValue's operator table is refused on the build path (42F59, naming
// the index, the function and the types), since the target fails every query
// of the table over it; one with a lane is stored. Driven through the FDB
// catalog and the in-memory catalog, for a fresh name and for a new version of a
// stored one.
func TestCreateTemplate_LaneCheck(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		root  *gen.KeyExpression
		types string // "" when admitted, else the refused operand types
	}{
		{"INT + INT", fn("add", field("price"), field("quantity")), ""},
		{"LONG & INT", fn("bitand", field("coord_x"), intLit(1)), ""},
		{"a nested key by its lane's result: (LONG + LONG) & INT", fn("bitand", fn("add", field("coord_x"), field("coord_y")), intLit(1)), ""},
		{"LONG & cardinality (INT)", fn("bitand", field("coord_x"), fn("cardinality", recordlayer.FieldConcatenate("tags").ToKeyExpression())), ""},
		{"bitmap_bucket_offset over (LONG, INT)", fn("bitmap_bucket_offset", field("coord_x"), intLit(10000)), ""},
		{"bitmap_bucket_offset over (LONG, LONG)", fn("bitmap_bucket_offset", field("coord_x"), longLit(10000)), "(LONG, LONG)"},
		{"an order function's BYTES", fn("bitand", fn("order_desc_nulls_last", field("price")), intLit(1)), "(BYTES, INT)"},
		{"a nested key with no lane refuses its parent", fn("add", fn("bitand", field("vector_data"), intLit(1)), intLit(1)), "(BYTES, INT)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			check := func(t *testing.T, err error) {
				t.Helper()
				if c.types == "" {
					if err != nil {
						t.Fatalf("admitted key refused: %v", err)
					}
					return
				}
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidSchemaTemplate {
					t.Fatalf("err = %v, want %s", err, api.ErrCodeInvalidSchemaTemplate)
				}
				want := fmt.Sprintf("has no lane for operand types %s, so no query of its table can be planned", c.types)
				if !strings.Contains(apiErr.Message, "index LANE_IX cannot be stored") || !strings.Contains(apiErr.Message, want) {
					t.Fatalf("message %q, want it to name the index and %q", apiErr.Message, want)
				}
			}
			t.Run("FDB, a fresh name", func(t *testing.T) {
				cat, run := newFDBCatalogInSubspace(t)
				check(t, run(func(tx api.Transaction) error {
					return cat.SchemaTemplateCatalog().CreateTemplate(tx, laneTemplate(t, "lane", 1, c.root))
				}))
			})
			t.Run("FDB, a new version", func(t *testing.T) {
				cat, run := newFDBCatalogInSubspace(t)
				mustRun(t, run, func(tx api.Transaction) error {
					return cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "lane", 1))
				})
				check(t, run(func(tx api.Transaction) error {
					return cat.SchemaTemplateCatalog().CreateTemplate(tx, laneTemplate(t, "lane", 2, c.root))
				}))
			})
			t.Run("in memory", func(t *testing.T) {
				c2 := NewInMemorySchemaTemplateCatalog()
				tx := NewInMemoryTransaction()
				check(t, c2.CreateTemplate(tx, laneTemplate(t, "lane", 1, c.root)))
				c3 := NewInMemorySchemaTemplateCatalog()
				if err := c3.CreateTemplate(tx, buildVersionedTemplate(t, "lane", 1)); err != nil {
					t.Fatal(err)
				}
				check(t, c3.CreateTemplate(tx, laneTemplate(t, "lane", 2, c.root)))
			})
		})
	}
}

// A stored key with no lane is the tenant's already: a new version that keeps
// the index unchanged (EQUIVALENT) is admitted and carries it as stored, and one
// that drops it leaves a former index. Only an index the save defines is
// checked.
func TestFDB_CreateTemplate_LaneCheckReadsOnlyTheIndexesASaveDefines(t *testing.T) {
	t.Parallel()
	e := newRestoreEnv(t)
	e.name = "lanekept"
	noLane := fn("bitmap_bucket_offset", field("coord_x"), longLit(10000))
	v1 := demoMetaData(t, 1, func(p *gen.MetaData) { p.Indexes = append(p.Indexes, laneIndex("LANE_IX", noLane)) })
	e.writeRow(1, v1)
	mustRun(t, e.run, func(tx api.Transaction) error {
		loaded, err := e.cat.SchemaTemplateCatalog().LoadSchemaTemplateAtVersion(tx, "lanekept", 1)
		if err != nil {
			return err
		}
		v2, err := metadata.NewRecordLayerSchemaTemplateWithVersion("lanekept", loaded.(*metadata.RecordLayerSchemaTemplate).Underlying(), 2)
		if err != nil {
			return err
		}
		return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, v2)
	})
	mustRun(t, e.run, func(tx api.Transaction) error {
		return e.cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "lanekept", 3))
	})
	mustRun(t, e.run, func(tx api.Transaction) error {
		p, err := e.cat.SchemaTemplateCatalog().LoadTemplateProto(tx, "lanekept", 3)
		if err != nil {
			return err
		}
		if len(p.GetIndexes()) != 0 || len(p.GetFormerIndexes()) != 1 || p.GetFormerIndexes()[0].GetFormerName() != "LANE_IX" {
			t.Errorf("v3: indexes %v, former indexes %v; want LANE_IX a former index", p.GetIndexes(), p.GetFormerIndexes())
		}
		return nil
	})
}

// Concurrent carried saves of one name (ws-j-design.md section 9 (z)): each
// CreateTemplate reads the stored latest with a reverse scan limited to one
// row, whose read-conflict range runs from the row it returns to the end of the
// name's range, so a concurrent write of any higher version lands in it and one
// of the two commits fails with not_committed. Its retry reads the new latest.
func TestFDB_CreateTemplate_ConcurrentSavesOfOneName(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) *RecordLayerStoreCatalog {
		cat, run := newFDBCatalogInSubspace(t)
		mustRun(t, run, func(tx api.Transaction) error {
			return cat.SchemaTemplateCatalog().CreateTemplate(tx, buildVersionedTemplate(t, "cc", 2))
		})
		return cat
	}
	stage := func(t *testing.T, cat *RecordLayerStoreCatalog, first, second int) (func() error, func() error) {
		aTx, aCommit := openRaced(t)
		bTx, bCommit := openRaced(t)
		if err := cat.SchemaTemplateCatalog().CreateTemplate(aTx, buildVersionedTemplate(t, "cc", first)); err != nil {
			t.Fatal(err)
		}
		if err := cat.SchemaTemplateCatalog().CreateTemplate(bTx, buildVersionedTemplate(t, "cc", second)); err != nil {
			t.Fatal(err)
		}
		return aCommit, bCommit
	}
	t.Run("the same version: the retry is the exact duplicate", func(t *testing.T) {
		t.Parallel()
		cat := setup(t)
		aCommit, bCommit := stage(t, cat, 3, 3)
		if err := bCommit(); err != nil {
			t.Fatal(err)
		}
		wantNotCommitted(t, aCommit())
		retryTx, _ := openRaced(t)
		err := cat.SchemaTemplateCatalog().CreateTemplate(retryTx, buildVersionedTemplate(t, "cc", 3))
		wantAPIError(t, err, api.ErrCodeDuplicateSchemaTemplate, "Schema template already exists: cc")
	})
	for _, c := range []struct {
		name     string
		loser    int
		winner   int
		retryErr string // "" admits the retry
	}{
		{"a lower version loses to a higher: its retry is below the latest", 3, 4, `template "cc": new version 3 must be greater than current version 4`},
		{"a higher version loses to a lower: its retry carries from the new latest", 4, 3, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cat := setup(t)
			loserCommit, winnerCommit := stage(t, cat, c.loser, c.winner)
			if err := winnerCommit(); err != nil {
				t.Fatal(err)
			}
			wantNotCommitted(t, loserCommit())
			retryTx, retryCommit := openRaced(t)
			err := cat.SchemaTemplateCatalog().CreateTemplate(retryTx, buildVersionedTemplate(t, "cc", c.loser))
			if c.retryErr != "" {
				wantAPIError(t, err, api.ErrCodeInvalidSchemaTemplate, c.retryErr)
				return
			}
			if err != nil {
				t.Fatalf("the retry of %d: %v", c.loser, err)
			}
			if err := retryCommit(); err != nil {
				t.Fatalf("commit the retry: %v", err)
			}
		})
	}
}
