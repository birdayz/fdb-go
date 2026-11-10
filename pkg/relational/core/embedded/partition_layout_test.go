package embedded

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/session"
	"google.golang.org/protobuf/proto"
)

func partitionBridgeMetadata(t *testing.T, sourceFirst bool, width int64, proof string) *recordlayer.RecordMetaData {
	t.Helper()
	base, err := metadata.NewSchemaTemplateBuilder().SetName("PARTITION_BRIDGE").AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("SOURCE", api.NewLongType(false), 2),
		metadata.NewColumnSpec("BODY", api.NewStringType(true), 3),
	}, []string{"ID", "SOURCE"}).Build()
	if err != nil {
		t.Fatal(err)
	}
	union := base.Underlying().GetUnionDescriptor()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecordsWithUnionName(union.ParentFile(), string(union.Name())).SetVersion(1)
	logical := []recordlayer.KeyExpression{recordlayer.Field("ID"), recordlayer.Field("SOURCE")}
	if sourceFirst {
		logical[0], logical[1] = logical[1], logical[0]
	}
	prefix := []recordlayer.KeyExpression{recordlayer.RecordTypeKey()}
	if width != 0 {
		prefix = append(prefix, recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), width))
	}
	b.GetRecordType("DOCS").SetPrimaryKey(recordlayer.Concat(append(prefix, logical...)...)).SetRecordTypeKey("DOCS")
	var value recordlayer.KeyExpression = recordlayer.Field("BODY")
	if proof == "literal" {
		value = recordlayer.Concat(value, recordlayer.Literal(int64(99)))
	}
	if width != 0 {
		var inherited recordlayer.KeyExpression = recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), width)
		if proof == "generic" {
			inherited = recordlayer.FunctionExpr(recordlayer.FunctionNamePartitionV1, recordlayer.Concat(recordlayer.Field("SOURCE"), recordlayer.Literal(width)))
		}
		value = recordlayer.Concat(inherited, value)
	}
	idx := recordlayer.NewIndex("DOCS_VALUE", value)
	if proof == "predicate" {
		if err := idx.SetPredicateProto(&gen.Predicate{ValuePredicate: &gen.ValuePredicate{Value: []string{"ID"}, Comparison: &gen.Comparison{SimpleComparison: &gen.SimpleComparison{Type: gen.ComparisonType_GREATER_THAN.Enum(), Operand: &gen.Value{LongValue: proto.Int64(0)}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	b.AddIndex("DOCS", idx)
	if width != 0 {
		p := recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), width)
		b.AddIndex("DOCS", recordlayer.NewIndex("DOCS_COVER", recordlayer.KeyWithValue(recordlayer.Concat(p, recordlayer.Field("ID"), recordlayer.Field("BODY")), 2)))
		b.AddIndex("DOCS", recordlayer.NewTextIndex("DOCS_TEXT", recordlayer.GroupBy(recordlayer.Field("BODY"), p, recordlayer.Field("ID"))))
	}
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func TestPartitionPhysicalTranslationRetainsTypedCoordinate(t *testing.T) {
	t.Parallel()
	for _, sourceFirst := range []bool{false, true} {
		md := partitionBridgeMetadata(t, sourceFirst, 10, "")
		rt := md.GetRecordType("DOCS")
		flowed := executor.PositionalTypeForRecordLayout(rt.Descriptor, false)
		translated := translatePrimaryKeyToValues(rt.PrimaryKey, recordlayer.ToUserIdentifier, flowed)
		if len(translated) != 4 {
			t.Fatalf("physical translation = %v", translated)
		}
		if _, ok := translated[0].(*values.RecordTypeValue); !ok {
			t.Fatalf("first physical value = %T", translated[0])
		}
		coordinate, ok := translated[1].(*values.PartitionValue)
		if !ok || coordinate.Width != 10 {
			t.Fatalf("coordinate = %v", translated[1])
		}
		source, ok := values.AsFieldValue(coordinate.Source)
		if !ok || !reflect.DeepEqual(source.Path().Ordinals(), []int{1}) {
			t.Fatalf("coordinate source = %v", coordinate.Source)
		}
		other := partitionBridgeMetadata(t, sourceFirst, 20, "")
		if valuesSlicesStructurallyEqual(translated, translatePrimaryKeyToValues(other.GetRecordType("DOCS").PrimaryKey, recordlayer.ToUserIdentifier, flowed)) {
			t.Fatal("different widths conflated")
		}
		if columns, _, ok := coveredPrimaryKeyColumns(rt); ok || columns != nil {
			t.Fatalf("physical ordering names must abstain: %v, %v", columns, ok)
		}
		if columns, _ := primaryCandidateKeyComponents(rt); len(columns) != 0 {
			t.Fatalf("partition primary candidate admitted: %v", columns)
		}
	}
	row := values.NewRecordType("ROW", false, []values.Field{{Name: "SOURCE", FieldType: values.NullableString, Ordinal: 0}})
	if got := translatePrimaryKeyToValues(recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), 10), nil, row); got != nil {
		t.Fatalf("non-BIGINT source translated: %v", got)
	}
}

func TestPartitionZeroCandidateFallbackIsExecutableAndUnordered(t *testing.T) {
	t.Parallel()
	for _, sourceFirst := range []bool{false, true} {
		md := partitionBridgeMetadata(t, sourceFirst, 10, "")
		ctx, err := buildCascadesPlanContext(md, cascades.DefaultPlannerConfiguration())
		if err != nil {
			t.Fatal(err)
		}
		if got := ctx.GetMatchCandidates(); len(got) != 0 {
			t.Fatalf("local candidates admitted: %v", got)
		}
		generic := partitionBridgeMetadata(t, sourceFirst, 10, "generic")
		genericContext, err := buildCascadesPlanContext(generic, cascades.DefaultPlannerConfiguration())
		if err != nil {
			t.Fatal(err)
		}
		if got := genericContext.GetMatchCandidates(); len(got) != 0 {
			t.Fatalf("generic inherited function bypassed the local-index gate: %v", got)
		}
		if proof, err := requiresLiteralIndexProof(generic); err != nil || proof {
			t.Fatalf("validated generic inherited width requires literal proof: %v, %v", proof, err)
		}
		if columns := ctx.GetPrimaryKeyColumns("DOCS"); len(columns) != 0 {
			t.Fatalf("claimed global PK ordering: %v", columns)
		}
		plan, err := PlanRecordQueryWithMetadata("SELECT ID, SOURCE FROM DOCS WHERE SOURCE > -12 ORDER BY ID, SOURCE", md, nil)
		if err != nil {
			t.Fatal(err)
		}
		explain := plan.Explain()
		for _, want := range []string{"Scan(DOCS", "PredicatesFilter", "InMemorySort"} {
			if !strings.Contains(explain, want) {
				t.Fatalf("missing %s in zero-candidate fallback: %s", want, explain)
			}
		}
		if strings.Contains(explain, "IndexScan") || strings.Contains(explain, "COVERING") {
			t.Fatalf("local index leaked: %s", explain)
		}
		var scans []*plans.RecordQueryScanPlan
		var walk func(plans.RecordQueryPlan)
		walk = func(p plans.RecordQueryPlan) {
			if scan, ok := p.(*plans.RecordQueryScanPlan); ok {
				scans = append(scans, scan)
			}
			for _, child := range p.GetChildren() {
				walk(child)
			}
		}
		walk(plan)
		if len(scans) != 1 {
			t.Fatalf("executable scans = %d", len(scans))
		}
		if len(scans[0].GetScanComparisons()) != 0 || len(scans[0].GetPrimaryKeyValues()) != 0 || len(scans[0].GetCommonPrimaryKeyValues()) != 4 {
			t.Fatalf("scan narrowed, ordered or lost physical identity: %s, PK=%v, common=%v", scans[0].Explain(), scans[0].GetPrimaryKeyValues(), scans[0].GetCommonPrimaryKeyValues())
		}
		plain := partitionBridgeMetadata(t, sourceFirst, 0, "")
		plainContext, err := buildCascadesPlanContext(plain, cascades.DefaultPlannerConfiguration())
		if err != nil {
			t.Fatal(err)
		}
		if got := plainContext.GetMatchCandidates(); len(got) < 2 {
			t.Fatalf("plain positive-control candidates = %d", len(got))
		}
	}
}

func TestPartitionLogicalDirectKeysAndRefusals(t *testing.T) {
	t.Parallel()
	for _, sourceFirst := range []bool{false, true} {
		rt := partitionBridgeMetadata(t, sourceFirst, 10, "").GetRecordType("DOCS")
		for _, complete := range []bool{false, true} {
			got, err := buildDirectPrimaryKey(rt, "DOCS", map[string]any{"ID": int64(7), "SOURCE": int64(-11)}, complete)
			want := tuple.Tuple{"DOCS", int64(-2), int64(7), int64(-11)}
			if sourceFirst {
				want[2], want[3] = want[3], want[2]
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("full direct key = %v, %v; want %v", got, err, want)
			}
		}
		first := "ID"
		if sourceFirst {
			first = "SOURCE"
		}
		for _, columns := range []map[string]any{nil, {first: int64(1)}} {
			_, err := buildDirectPrimaryKey(rt, "DOCS", columns, false)
			var typed *api.Error
			if !errors.As(err, &typed) || typed.Code != api.ErrCodeUnsupportedOperation {
				t.Fatalf("partial direct key = %v", err)
			}
		}
		_, err := buildDirectPrimaryKey(rt, "DOCS", map[string]any{"ID": int64(7), "SOURCE": "bad"}, true)
		var routing *recordlayer.PartitionRoutingError
		if !errors.As(err, &routing) {
			t.Fatalf("bad carrier = %v", err)
		}
	}
}

func TestPartitionLayoutCacheAndLiteralProofIdentity(t *testing.T) {
	t.Parallel()
	if proof, err := requiresLiteralIndexProof(nil); proof || err != nil {
		t.Fatalf("nil metadata proof = %v, %v", proof, err)
	}
	if component, err := partitionCacheComponent(nil); component != "" || err != nil {
		t.Fatalf("nil metadata identity = %q, %v", component, err)
	}
	plain := partitionBridgeMetadata(t, false, 0, "")
	if component, err := partitionCacheComponent(plain); err != nil || component != "" {
		t.Fatalf("ordinary cache identity changed: %q, %v", component, err)
	}
	generator := &cascadesGenerator{c: &EmbeddedConnection{sess: &session.Session{}}}
	options := plannerOptionsFrom(nil)
	key, cacheable, err := generator.planCacheKey(plain, options, "SELECT ID")
	var ordinaryScope strings.Builder
	ordinaryScope.WriteString(planCacheScope("", "", plain.Version(), options.cacheKeyPart()))
	writeLengthPrefixed(&ordinaryScope, "")
	if err != nil || !cacheable || key.scope != ordinaryScope.String() || key.sql != "SELECT ID" {
		t.Fatalf("ordinary cache key bytes changed: %+v, %v", key, err)
	}
	a := partitionBridgeMetadata(t, false, 10, "")
	b := partitionBridgeMetadata(t, false, 20, "")
	c := partitionBridgeMetadata(t, true, 10, "")
	identities := map[string]bool{}
	for _, md := range []*recordlayer.RecordMetaData{a, b, c} {
		if md.Version() != 4 {
			t.Fatalf("width control needs identical storage versions (seed 1 + 3 indexes), got %d", md.Version())
		}
		identity, err := partitionCacheComponent(md)
		if err != nil || identity == "" || identities[identity] {
			t.Fatalf("colliding layout identity, error=%v", err)
		}
		identities[identity] = true
		if again, err := partitionCacheComponent(md); err != nil || again != identity {
			t.Fatal("nondeterministic identity")
		}
		if proof, err := requiresLiteralIndexProof(md); err != nil || proof {
			t.Fatalf("inherited width incorrectly requires literal proof: %v, %v", proof, err)
		}
	}
	// Hold the real cache's outer template name and all non-layout key parts
	// fixed: only a different physical partition layout may separate these.
	cache := NewRelationalPlanCache(nil)
	bindings := queryBindings{}
	keys := make([]cacheKey, 2)
	entries := make([]*planCacheEntry, 2)
	for i, md := range []*recordlayer.RecordMetaData{a, b} {
		var err error
		keys[i], _, err = generator.planCacheKey(md, options, "SELECT ID FROM DOCS")
		if err != nil {
			t.Fatal(err)
		}
		plan, err := PlanRecordQueryWithMetadata("SELECT ID FROM DOCS", md, nil)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = &planCacheEntry{plan: plan}
	}
	if keys[0] == keys[1] {
		t.Fatal("same-version different-width cache keys collided")
	}
	cache.store("SAME_TEMPLATE", keys[0], bindings, entries[0])
	if _, hit := cache.lookup("SAME_TEMPLATE", keys[1], bindings); hit {
		t.Fatal("different width reused the first physical plan")
	}
	cache.store("SAME_TEMPLATE", keys[1], bindings, entries[1])
	for i, width := range []int64{10, 20} {
		equalMetadata := partitionBridgeMetadata(t, false, width, "")
		equalKey, _, err := generator.planCacheKey(equalMetadata, options, "SELECT ID FROM DOCS")
		if err != nil || equalKey != keys[i] {
			t.Fatalf("equal layout key changed: %v", err)
		}
		entry, hit := cache.lookup("SAME_TEMPLATE", equalKey, bindings)
		if !hit || entry.plan != entries[i].plan {
			t.Fatalf("equal layout %d failed to reuse its own physical plan", i)
		}
	}
	for _, kind := range []string{"literal", "predicate"} {
		for _, width := range []int64{0, 10} {
			if proof, err := requiresLiteralIndexProof(partitionBridgeMetadata(t, false, width, kind)); err != nil || !proof {
				t.Fatalf("lost actual %s proof (width=%d): %v, %v", kind, width, proof, err)
			}
		}
	}
	badPlain := partitionBridgeMetadata(t, false, 0, "")
	badPlain.GetIndex("DOCS_VALUE").RootExpression = recordlayer.Concat(recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), 10), recordlayer.Field("BODY"))
	var badPlainError *recordlayer.PartitionMetadataError
	if _, err := buildCascadesPlanContext(badPlain, cascades.DefaultPlannerConfiguration()); !errors.As(err, &badPlainError) {
		t.Fatalf("partition index on plain table did not fail with typed metadata error: %v", err)
	}
	bad := partitionBridgeMetadata(t, false, 10, "")
	bad.GetIndex("DOCS_VALUE").RootExpression = recordlayer.Concat(recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), 20), recordlayer.Field("BODY"))
	for _, check := range []func() error{
		func() error { _, err := partitionCacheComponent(bad); return err },
		func() error { _, err := requiresLiteralIndexProof(bad); return err },
		func() error {
			_, err := newCascadesPlanner(bad, plannerOptionsFrom(nil), cascades.BatchAExpressionRules(), nil)
			return err
		},
	} {
		var typed *recordlayer.PartitionMetadataError
		if err := check(); !errors.As(err, &typed) {
			t.Fatalf("malformed metadata not rejected: %v", err)
		}
	}
}
