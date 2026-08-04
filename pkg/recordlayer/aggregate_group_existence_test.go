package recordlayer

import (
	"strings"
	"testing"
)

// TestGroupingSignature_SumAndCountStarAgreeOnTheSameGrouping is the load-bearing
// property of structural companion discovery (RFC-209 §5.2): a SUM index and a
// COUNT(*) index declared over the same GROUP BY must produce the same grouping
// signature, even though their ROOT expressions differ — SUM's root is
// GroupBy(value, grouping) and COUNT(*)'s is GroupAll(grouping). Comparing roots
// would never match; comparing the grouping halves must always match.
func TestGroupingSignature_SumAndCountStarAgreeOnTheSameGrouping(t *testing.T) {
	t.Parallel()

	sum := GroupBy(Field("v"), Field("g"))
	countStar := GroupAll(Field("g"))

	sumSig := GroupingSignature(sum)
	countSig := GroupingSignature(countStar)

	if len(sumSig) == 0 {
		t.Fatal("SUM index yielded no grouping signature; every companion match would decline")
	}
	if string(sumSig) != string(countSig) {
		t.Fatalf("a SUM and a COUNT(*) over the same GROUP BY disagree on the grouping signature\n"+
			"  SUM     : %x\n  COUNT(*): %x\n"+
			"Companion discovery is signature equality, so a disagreement here means a SUM index "+
			"can never find the COUNT(*) that exists precisely to serve it.", sumSig, countSig)
	}

	// A different grouping column must NOT match, or the merge would be driven
	// by the group set of an unrelated index.
	if other := GroupingSignature(GroupAll(Field("h"))); string(other) == string(sumSig) {
		t.Fatal("indexes grouped by different columns share a grouping signature")
	}
}

// TestGroupingSignature_MultiColumnGroupingIsOrderSensitive pins that the
// signature distinguishes GROUP BY (a, b) from GROUP BY (b, a). The companion
// merge walks two streams in key order and matches them positionally, so two
// indexes whose grouping columns are the same SET but a different ORDER produce
// differently-sorted streams; treating them as companions would silently mis-pair
// groups rather than fail.
func TestGroupingSignature_MultiColumnGroupingIsOrderSensitive(t *testing.T) {
	t.Parallel()

	ab := GroupingSignature(GroupBy(Field("v"), Field("a"), Field("b")))
	ba := GroupingSignature(GroupAll(Concat(Field("b"), Field("a"))))
	abCount := GroupingSignature(GroupAll(Concat(Field("a"), Field("b"))))

	if len(ab) == 0 || len(abCount) == 0 {
		t.Fatal("a two-column grouping yielded no signature")
	}
	if string(ab) != string(abCount) {
		t.Fatalf("SUM grouped by (a,b) and COUNT(*) grouped by (a,b) disagree\n  %x\n  %x", ab, abCount)
	}
	if string(ab) == string(ba) {
		t.Fatal("GROUP BY (a,b) and GROUP BY (b,a) share a grouping signature; " +
			"the companion merge would pair groups across differently-ordered streams")
	}
}

// TestGroupingSignature_DeclinesWhatItCannotSplit pins the fail-closed
// direction. A nil signature means "no companion", never "matches anything", so
// every shape the split cannot handle exactly must yield nil rather than a
// partial answer.
func TestGroupingSignature_DeclinesWhatItCannotSplit(t *testing.T) {
	t.Parallel()

	if sig := GroupingSignature(nil); sig != nil {
		t.Fatal("a nil grouping key expression produced a signature")
	}
	// Ungrouped: every column is aggregated, so there is no grouping half.
	if sig := GroupingSignature(Ungrouped(Field("v"))); sig != nil {
		t.Fatalf("an ungrouped aggregate produced a grouping signature %x; it has no grouping key "+
			"to match a companion on", sig)
	}
}

// TestGroupCountCompanionName_IsDeterministicAndReserved pins that the derived
// name is a pure function of the owner's name and that it lands inside the
// reserved suffix space the DDL rejects. A randomized or counter-based name
// would make two stores built from the same DDL differ in stored bytes.
func TestGroupCountCompanionName_IsDeterministicAndReserved(t *testing.T) {
	t.Parallel()

	first := GroupCountCompanionName("AI_SUM_G")
	second := GroupCountCompanionName("AI_SUM_G")
	if first != second {
		t.Fatalf("companion naming is not deterministic: %q then %q", first, second)
	}
	if !strings.HasSuffix(first, GroupCountCompanionSuffix) {
		t.Fatalf("companion name %q does not carry the reserved suffix %q, so the DDL's "+
			"collision guard would not cover it", first, GroupCountCompanionSuffix)
	}
	if GroupCountCompanionName("A") == GroupCountCompanionName("B") {
		t.Fatal("two different owner indexes derive the same companion name")
	}
}
