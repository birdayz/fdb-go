package sqldriver_test

// The end-to-end half of "a proved tautology is not a sparse predicate", on the
// shape where the classification decides whether a candidate exists AT ALL.
//
// ExpandValueIndex refuses a fan-out candidate that carries a stored predicate:
// the fan-out expansion does not thread a candidate-side predicate, so matching
// one would serve a filtered index as if it were full. That refusal is correct
// for a real filter and catastrophic for `WHERE TRUE`, which rejects no record —
// the index is complete, and dropping it leaves the query no access path.
//
// Unlike the flat expansion, this arm never converts the predicate, so a
// tautology check at the conversion site cannot save it. Only classifying at the
// candidate boundary does, which is why this test is the witness for putting it
// there.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_TautologyPredicateOnFanOutIndexStillPlansAndExecutes(t *testing.T) {
	t.Parallel()
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

	b := metadata.NewSchemaTemplateBuilder().SetName("taut_fanout")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAGS", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"ID"})
	b.AddFanOutIndex("T", "T_TAGS", "TAGS")
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()

	// Go's DDL cannot author a predicated fan-out index (the generator rejects
	// non-unnested arrays), so the shape is set on the built index — the same
	// field a Java-authored metadata round-trip populates, and the same one
	// every sparseness gate reads.
	idx := md.GetIndex("T_TAGS")
	if idx == nil {
		t.Fatal("index T_TAGS not in metadata")
	}
	if err := idx.SetPredicateProto(&gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()},
	}); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}
	if !idx.HasPredicate() {
		t.Fatal("T_TAGS carries no predicate — the rest of this test would assert nothing")
	}
	if idx.HasFilteringPredicate() {
		t.Fatal("a WHERE TRUE predicate is not filtering: it rejects no record")
	}

	desc := md.GetRecordType("T").Descriptor
	record := func(id int64, tags ...int64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName(protoreflect.Name("ID")), protoreflect.ValueOfInt64(id))
		vals := make([]protoreflect.Value, 0, len(tags))
		for _, tag := range tags {
			vals = append(vals, protoreflect.ValueOfInt64(tag))
		}
		setArrayField(m, desc.Fields().ByName(protoreflect.Name("TAGS")), vals...)
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, createErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if createErr != nil {
			return nil, createErr
		}
		for _, rec := range []proto.Message{
			record(1, 7), record(2, 8), record(3, 7, 9), record(4, 8),
		} {
			if _, saveErr := store.SaveRecord(rec); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	const q = `SELECT R.ID FROM T AS R WHERE EXISTS (SELECT E FROM R.TAGS AS E WHERE E = 8)`
	plan, err := embedded.PlanRecordQueryWithMetadata(q, md, nil)
	if err != nil {
		t.Fatalf("plan %q: %v", q, err)
	}
	explain := plan.Explain()
	if !strings.Contains(explain, "T_TAGS") {
		t.Fatalf("the WHERE TRUE fan-out index produced no usable candidate:\n%s\n"+
			"— a tautological stored predicate rejects no record, so the index is "+
			"complete and the sparse-fan-out guard must not fire on it", explain)
	}

	var got []string
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
			got = append(got, positionalNamedPipeSprint(r))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("exec %q: %v\nplan:\n%s", q, err, explain)
	}

	sort.Strings(got)
	if want := "ID=2 ID=4"; strings.Join(got, " ") != want {
		t.Fatalf("rows = %q, want %q\nplan:\n%s", strings.Join(got, " "), want, explain)
	}
}
