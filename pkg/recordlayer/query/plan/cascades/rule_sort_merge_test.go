package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// stackedSorts builds Sort([outerOrdinal]) over Sort([innerOrdinal]) over Scan.
// A negative inner ordinal requests an unsorted inner node.
func stackedSorts(outerOrdinal, innerOrdinal int) *expressions.LogicalSortExpression {
	scan := sortRewriteScan()
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	var innerKeys []expressions.SortKey
	if innerOrdinal >= 0 {
		innerKeys = []expressions.SortKey{{Value: sortRewriteField(innerQ, innerOrdinal)}}
	}
	innerSort := sortRewriteSort(innerKeys, innerQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerSort))
	outerKeys := []expressions.SortKey{{Value: sortRewriteField(outerQ, outerOrdinal)}}
	return sortRewriteSort(outerKeys, outerQ)
}

func TestSortMergeRule_OuterReordersInner(t *testing.T) {
	t.Parallel()
	stacked := stackedSorts(3, 2)
	ref := expressions.InitialOf(stacked)
	yielded := fireSortRewriteRule(t, NewSortMergeRule(), ref)
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
	fv, ok := values.AsFieldValue(keys[0].Value)
	if !ok || fv.DisplayName() != "name" {
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
	scan := sortRewriteScan()
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerKeys := []expressions.SortKey{
		{Value: sortRewriteField(innerQ, 2), Reverse: false},
	}
	innerSort := sortRewriteSort(innerKeys, innerQ)
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerSort))
	outerSort := mustSortRewriteConstruct(expressions.UnsortedLogicalSortExpression(outerQ))
	ref := expressions.InitialOf(outerSort)
	yielded := fireSortRewriteRule(t, NewSortMergeRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (Unsorted outer must NOT eliminate inner)", len(yielded))
	}
}

func TestSortMergeRule_DeclinesOnNonSortInner(t *testing.T) {
	t.Parallel()
	scan := sortRewriteScan()
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := sortRewriteSort(
		[]expressions.SortKey{{Value: sortRewriteField(q, 2)}},
		q,
	)
	ref := expressions.InitialOf(sort)
	yielded := fireSortRewriteRule(t, NewSortMergeRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (inner is Scan, not Sort)", len(yielded))
	}
}

func TestSortMergeRule_TriplyNested_FlattensViaFixpoint(t *testing.T) {
	t.Parallel()
	// Sort([k1]) over Sort([k2]) over Sort([k3]) over Scan
	// Two SortMerge fires should leave Sort([k1]) over Scan.
	scan := sortRewriteScan()
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	deepSort := sortRewriteSort(
		[]expressions.SortKey{{Value: sortRewriteField(scanQ, 6)}},
		scanQ,
	)
	deepSortQ := expressions.ForEachQuantifier(expressions.InitialOf(deepSort))
	midSort := sortRewriteSort(
		[]expressions.SortKey{{Value: sortRewriteField(deepSortQ, 5)}},
		deepSortQ,
	)
	midSortQ := expressions.ForEachQuantifier(expressions.InitialOf(midSort))
	topSort := sortRewriteSort(
		[]expressions.SortKey{{Value: sortRewriteField(midSortQ, 4)}},
		midSortQ,
	)
	ref := expressions.InitialOf(topSort)
	progress, converged := exploreSortRewriting(NewPlanner([]ExpressionRule{NewSortMergeRule()}, nil), ref)
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
			fv, ok := values.AsFieldValue(s.GetSortKeys()[0].Value)
			if ok && fv.DisplayName() == "k1" {
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
	stacked := stackedSorts(2, -1) // inner has no keys
	ref := expressions.InitialOf(stacked)
	yielded := fireSortRewriteRule(t, NewSortMergeRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
}
