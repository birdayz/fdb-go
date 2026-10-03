package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestDMLDedupUsesPhysicalEdges(t *testing.T) {
	t.Parallel()
	for _, distinct := range []bool{false, true} {
		t.Run(map[bool]string{false: "dedup", true: "already_distinct"}[distinct], func(t *testing.T) {
			t.Parallel()
			scan := pushFetchScan()
			source := expressions.FinalOfAtStage(scan, expressions.StagePlanned)
			// The DML's transforms read its input through the logical edge's
			// alias, so the physical edge morphs from it (Java
			// Quantifier.physicalBuilder().morphFrom(innerQuantifier)).
			logical := expressions.ForEachQuantifier(source)
			q, err := dmlDedupedInnerQuantifier(&ExpressionRuleCall{}, dmlInnerCandidate{expr: scan, source: source}, logical, distinct)
			if err != nil {
				t.Fatal(err)
			}
			if q.Kind() != expressions.QuantifierPhysical {
				t.Errorf("DML edge kind = %v, want physical", q.Kind())
			}
			if q.GetAlias() != logical.GetAlias() {
				t.Errorf("DML edge alias = %v, want the logical edge's %v", q.GetAlias(), logical.GetAlias())
			}
			child := q.GetRangesOver().FinalMembers()[0]
			if !distinct {
				dedup, ok := child.(*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan)
				if !ok {
					t.Fatalf("inner = %T, want primary-key distinct", child)
				}
				if dedup.GetQuantifiers()[0].Kind() != expressions.QuantifierPhysical {
					t.Error("dedup has a logical edge")
				}
				child = dedup.GetQuantifiers()[0].GetRangesOver().FinalMembers()[0]
			}
			if child != scan || q.GetRangesOver() == source {
				t.Fatal("DML lost its restricted, exact selected input")
			}
		})
	}
}

func TestAccessCoverageUsesPhysicalFinalEdge(t *testing.T) {
	t.Parallel()
	index := pushFetchIndex("idx_x")
	fetch := pushFetchFetch(index, nil)
	wrapped, err := wrapScanPlanWithCoverage(fetch, false, []string{"x"}, []string{"PK"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := wrapped.GetQuantifiers()[0]
	if q.Kind() != expressions.QuantifierPhysical {
		t.Errorf("fetch edge kind = %v, want physical", q.Kind())
	}
	ref := q.GetRangesOver()
	if ref.Stage() != expressions.StagePlanned || len(ref.FinalMembers()) != 1 || len(ref.Members()) != 0 {
		t.Fatalf("covering reference stage=%v finals=%d exploratory=%d, want planned final singleton", ref.Stage(), len(ref.FinalMembers()), len(ref.Members()))
	}
	if _, ok := ref.FinalMembers()[0].(*plans.RecordQueryCoveringIndexPlan); !ok {
		t.Fatalf("fetch input = %T, want covering index", ref.FinalMembers()[0])
	}
}

func TestExtractionPreservesQuantifierIdentity(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cost", "selector", "ordered_spine"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			scan := pushFetchIndex("idx_x").WithIndexMetadata([]string{"x"}, []string{"PK"}, false)
			q := expressions.NamedPhysicalQuantifier(values.NamedCorrelationIdentifier("extract_input"), expressions.FinalOfAtStage(scan, expressions.StagePlanned))
			filter := mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(q, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)}))
			// A union also exercises the non-delegating arm of ordered-spine extraction.
			union := mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{q, q.WithAlias(values.NamedCorrelationIdentifier("other_input"))}))
			for _, original := range []expressions.RelationalExpression{filter, union} {
				var rebuilt expressions.RelationalExpression
				var err error
				switch mode {
				case "cost":
					rebuilt, err = ExtractBestPlan(expressions.FinalOfAtStage(original, expressions.StagePlanned))
				case "selector":
					rebuilt, err = ExtractBestPlanFromSelector(expressions.FinalOfAtStage(original, expressions.StagePlanned), nil, nil)
				case "ordered_spine":
					p := NewPlanner(nil, nil)
					rebuilt, err = rebuildOrderedSpine(context.Background(), original, properties.NewRequestedOrdering([]properties.RequestedOrderingPart{{Value: scan.HintRichOrdering().GetKeys()[0], SortOrder: properties.RequestedSortOrderAscending}}, properties.DistinctnessPreserveDistinctness, false), p, p, properties.DefaultStatistics{}, map[*expressions.Reference]bool{}, map[*expressions.Reference]bool{})
				}
				if err != nil || rebuilt == nil {
					t.Fatalf("extract %T: %v, result %T", original, err, rebuilt)
				}
				for i, got := range rebuilt.GetQuantifiers() {
					want := original.GetQuantifiers()[i]
					if got.Kind() != expressions.QuantifierPhysical || got.GetAlias() != want.GetAlias() {
						t.Errorf("%T rebuilt edge kind=%v alias=%v, want physical alias=%v", original, got.Kind(), got.GetAlias(), want.GetAlias())
					}
					if got.GetRangesOver() == q.GetRangesOver() || len(got.GetRangesOver().FinalMembers()) != 1 {
						t.Fatal("extraction did not create a detached final input")
					}
				}
			}
		})
	}
}

func TestLogicalExtractionPreservesQuantifierFlags(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"ordinary", "null_on_empty", "strict_single", "existential"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			scan := implementFilterScan("T")
			ref := expressions.InitialOf(scan)
			alias := values.NamedCorrelationIdentifier("logical_input")
			q := expressions.NamedForEachQuantifier(alias, ref)
			switch kind {
			case "null_on_empty":
				q = expressions.NamedForEachNullOnEmptyQuantifier(alias, ref)
			case "strict_single":
				q = expressions.NamedForEachStrictSingleQuantifier(alias, ref)
			case "existential":
				q = expressions.NamedExistentialQuantifier(alias, ref)
			}
			original := mustImplementFilterConstruct(expressions.NewSelectExpression(mustImplementFilterConstruct(q.RequireFlowedObjectValue()), []expressions.Quantifier{q}, nil))
			rebuilt, err := ExtractBestPlan(expressions.InitialOf(original))
			if err != nil {
				t.Fatal(err)
			}
			got := rebuilt.GetQuantifiers()[0]
			if got.Kind() != q.Kind() || got.GetAlias() != alias || got.IsNullOnEmpty() != q.IsNullOnEmpty() || got.IsStrictSingle() != q.IsStrictSingle() {
				t.Fatalf("extracted logical edge lost kind, alias or cardinality flags: got %#v, want %#v", got, q)
			}
		})
	}
}
