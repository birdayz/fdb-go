package values

import "testing"

func TestMergeCorrelationIdentifier(t *testing.T) {
	t.Parallel()
	merge := MergeCorrelationIdentifier(7)
	if merge.Name() != `$m"7` || !merge.IsMergeAlias() || merge.IsZero() {
		t.Fatalf("merge alias %q merge=%v", merge.Name(), merge.IsMergeAlias())
	}
	if lookalike := NamedCorrelationIdentifier(`$m"7`); lookalike == merge || lookalike.IsMergeAlias() || SameLeg(lookalike, merge) {
		t.Fatal("a named alias spelled like a merge alias must not be one")
	}
	if MergeCorrelationIdentifier(7) != merge || MergeCorrelationIdentifier(8) == merge {
		t.Fatal("merge aliases are equal exactly when their ordinals are")
	}
	if UniqueCorrelationIdentifier().IsMergeAlias() || NamedCorrelationIdentifier("T1").IsMergeAlias() || CurrentCorrelation().IsMergeAlias() {
		t.Fatal("only minted merge aliases are merge aliases")
	}
}
