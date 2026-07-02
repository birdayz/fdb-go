package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// stackedSorts builds Sort([outerKeys]) over Sort([innerKeys]) over Scan.
func stackedSorts(outerKeys, innerKeys []expressions.SortKey) *expressions.LogicalSortExpression {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerSort := expressions.NewLogicalSortExpression(innerKeys, innerQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerSort))
	return expressions.NewLogicalSortExpression(outerKeys, outerQ)
}

func TestSortMergeRule_OuterReordersInner(t *testing.T) {
	t.Parallel()
	outerKeys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name", Typ: values.UnknownType}, Reverse: false},
	}
	innerKeys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
	}
	stacked := stackedSorts(outerKeys, innerKeys)
	ref := expressions.InitialOf(stacked)
	yielded := FireExpressionRule(NewSortMergeRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	flat, ok := yielded[0].(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalSortExpression", yielded[0])
	}
	keys := flat.GetSortKeys()
	if len(keys) != 1 {
		t.Fatalf("flat sort keys len=%d, want 1", len(keys))
	}
	fv, ok := keys[0].Value.(*values.FieldValue)
	if !ok || fv.Field != "name" {
		t.Fatalf("flat sort key[0] = %v, want FieldValue(name)", keys[0].Value)
	}
	// Inner should be the Scan, not the inner sort.
	if _, ok := flat.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("flat inner = %T, want Scan", flat.GetInner().GetRangesOver().Get())
	}
}

func TestSortMergeRule_DeclinesWhenOuterIsUnsorted(t *testing.T) {
	t.Parallel()
	// Outer sort is empty (Unsorted) — eliminating the inner would
	// silently destroy ordering. Decline.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerKeys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
	}
	innerSort := expressions.NewLogicalSortExpression(innerKeys, innerQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerSort))
	outerSort := expressions.UnsortedLogicalSortExpression(outerQ)
	ref := expressions.InitialOf(outerSort)
	yielded := FireExpressionRule(NewSortMergeRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (Unsorted outer must NOT eliminate inner)", len(yielded))
	}
}

func TestSortMergeRule_DeclinesOnNonSortInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}}},
		q,
	)
	ref := expressions.InitialOf(sort)
	yielded := FireExpressionRule(NewSortMergeRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (inner is Scan, not Sort)", len(yielded))
	}
}

func TestSortMergeRule_TriplyNested_FlattensViaFixpoint(t *testing.T) {
	t.Parallel()
	// Sort([k1]) over Sort([k2]) over Sort([k3]) over Scan
	// Two SortMerge fires should leave Sort([k1]) over Scan.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	deepSort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "k3", Typ: values.UnknownType}}},
		scanQ,
	)
	deepSortQ := expressions.ForEachQuantifier(expressions.InitialOf(deepSort))
	midSort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "k2", Typ: values.UnknownType}}},
		deepSortQ,
	)
	midSortQ := expressions.ForEachQuantifier(expressions.InitialOf(midSort))
	topSort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "k1", Typ: values.UnknownType}}},
		midSortQ,
	)
	ref := expressions.InitialOf(topSort)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewSortMergeRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d", progress)
	}
	// Look for the flat Sort([k1]) over Scan.
	flatFound := false
	for _, m := range ref.Members() {
		s, ok := m.(*expressions.LogicalSortExpression)
		if !ok {
			continue
		}
		if _, scanOK := s.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); scanOK && len(s.GetSortKeys()) == 1 {
			fv, ok := s.GetSortKeys()[0].Value.(*values.FieldValue)
			if ok && fv.Field == "k1" {
				flatFound = true
				break
			}
		}
	}
	if !flatFound {
		t.Fatalf("exploration did not produce Sort([k1]) over Scan; members=%d", len(ref.Members()))
	}
}

func TestSortMergeRule_InnerUnsortedStillFires(t *testing.T) {
	t.Parallel()
	// Inner is Sort([]) — unsorted. Outer's keys win, dropping the
	// inner is a structural cleanup. Rule fires.
	outerKeys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
	}
	stacked := stackedSorts(outerKeys, nil) // inner has no keys
	ref := expressions.InitialOf(stacked)
	yielded := FireExpressionRule(NewSortMergeRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
}
