package sqldriver_test

// Regression pin for the lateral-UNNEST WITH-ORDINALITY BOX output edge:
// a reference to the box's anonymous element slot (`_0`) or ordinal slot (`_1`) —
// the columns a WITH-ORDINALITY Explode flows — used by a predicate PUSHED INTO the
// Explode subplan (`FROM t, t.arr AS v AT o WHERE <pred on v/o>`).
//
// The Explode emits a PositionalRow of the box's `[_0,_1]` schema, so a pushed
// WHERE on the box element/ordinal resolves the reference by ORDINAL
// (frontierRowContext -> evaluateOrdinal -> GetByName("_0"/"_1") against the box's
// own type) — never a name-keyed Datum read. This
// test asserts each pushed / projected box reference returns the correct rows.
// Records are seeded via the record-store API because SQL INSERT has no array
// literal, and each query is planned + executed through the full Cascades path
// (embedded.PlanRecordQueryWithMetadata -> executor.ExecutePlan).

import (
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_UnnestOrdinalityBoxOrdinal(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	b := metadata.NewSchemaTemplateBuilder().SetName("uord")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), false), 2),
	}, []string{"ID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("T").Descriptor

	rec := func(id int64, arr []int32) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		fd := desc.Fields().ByName("ARR")
		list := m.NewField(fd).List()
		for _, v := range arr {
			list.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(list))
		return m
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		// id 1: {101}; id 2: {201,202,203} — a single- and a multi-element array so
		// the 1-based ordinal spans >1 value and the per-row reset is visible.
		for _, r := range []proto.Message{
			rec(1, []int32{101}),
			rec(2, []int32{201, 202, 203}),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	query := func(t *testing.T, sql string) []string {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cursor, cErr := executor.ExecutePlan(ctx, plan, store,
				executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cursor.Close()
			rows, rErr := executor.CollectAll(ctx, cursor)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				m, _ := executor.RowValue(r).(map[string]any)
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, k+"="+unnestSprint(m[k]))
				}
				out = append(out, strings.Join(parts, "|"))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		sort.Strings(out)
		return out
	}

	// Pushed WHERE on the AT ordinal slot (`_1`), ordinal-spelled aliases so the
	// user AS/AT names cannot accidentally masquerade as the box's internal keys.
	t.Run("pushed_where_on_ordinal_slot", func(t *testing.T) {
		got := query(t, `SELECT "E", "O" FROM T, T."ARR" AS "E" AT "O" WHERE "O" = 2`)
		want := []string{"E=202|O=2"}
		if !unnestEqualStrs(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})
	// Pushed WHERE on the AS element slot (`_0`).
	t.Run("pushed_where_on_element_slot", func(t *testing.T) {
		got := query(t, `SELECT "ID", "V", "AT" FROM T, T."ARR" AS "V" AT "AT" WHERE "V" > 201`)
		want := []string{"AT=2|ID=2|V=202", "AT=3|ID=2|V=203"}
		if !unnestEqualStrs(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})
	// Computed predicate on the ordinal (`_1` + 1 = 3) — NOT a simple equality
	// pushdown into the Explode filter, a general arithmetic predicate over the box.
	t.Run("computed_where_on_ordinal_slot", func(t *testing.T) {
		got := query(t, `SELECT "ID", "AT" FROM T, T."ARR" AT "AT" WHERE "AT" + 1 = 3`)
		want := []string{"AT=2|ID=2"}
		if !unnestEqualStrs(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})
	// AT-only (no AS) equality on the ordinal — the ordinal slot drives the filter
	// with no element alias present.
	t.Run("at_only_pushed_where_on_ordinal", func(t *testing.T) {
		got := query(t, `SELECT "ID", "AT" FROM T, T."ARR" AT "AT" WHERE "AT" = 1`)
		want := []string{"AT=1|ID=1", "AT=1|ID=2"}
		if !unnestEqualStrs(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})
	// Projection-only WITH ORDINALITY (no WHERE): the element/ordinal reach the
	// SELECT list via the FlatMap birth binder, which reads the box positionally.
	t.Run("projection_element_and_ordinal", func(t *testing.T) {
		got := query(t, `SELECT "ID", "V", "AT" FROM T, T."ARR" AS "V" AT "AT"`)
		want := []string{
			"AT=1|ID=1|V=101", "AT=1|ID=2|V=201", "AT=2|ID=2|V=202", "AT=3|ID=2|V=203",
		}
		if !unnestEqualStrs(got, want) {
			t.Fatalf("got=%v want=%v", got, want)
		}
	})
}
