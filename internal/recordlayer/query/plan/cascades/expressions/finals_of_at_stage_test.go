package expressions

import "testing"

// TestFinalsOfAtStageAdmitsLikeInsertFinal: building a Reference from several
// finals admits each one exactly as InsertFinal would, memo-equal twins
// included, and pins only a lone planned member.
func TestFinalsOfAtStageAdmitsLikeInsertFinal(t *testing.T) {
	t.Parallel()
	child := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
	distinct := func() RelationalExpression {
		return mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(child)))
	}
	other := mustExpression(NewFullUnorderedScanExpression([]string{"U"}, testRecordType()))
	members := []RelationalExpression{distinct(), other, distinct(), other}

	want := FinalOfAtStage(members[0], StagePlanned)
	for _, m := range members[1:] {
		want.InsertFinal(m)
	}
	got := FinalsOfAtStage(members, StagePlanned, true)
	if len(got.finalMembers) != len(want.finalMembers) || len(got.finalMembers) != 2 {
		t.Fatalf("FinalsOfAtStage kept %d finals, InsertFinal kept %d, want 2", len(got.finalMembers), len(want.finalMembers))
	}
	for i := range got.finalMembers {
		if got.finalMembers[i] != want.finalMembers[i] {
			t.Fatalf("final %d differs from InsertFinal's", i)
		}
	}
	if got.IsPinnedFinal() || got.Stage() != StagePlanned {
		t.Fatal("several members built a pinned reference or lost the stage")
	}
	if lone := FinalsOfAtStage(members[:1], StagePlanned, true); !lone.IsPinnedFinal() {
		t.Fatal("a lone planned member was not pinned")
	}
	if lone := FinalsOfAtStage(members[:1], StageCanonical, false); lone.IsPinnedFinal() || lone.Stage() != StageCanonical {
		t.Fatal("an unpinned build pinned its member or lost the stage")
	}
	// The built reference reads its own correlations correctly.
	if got := got.GetCorrelatedTo(); len(got) != 0 {
		t.Fatalf("uncorrelated finals report correlations %v", got)
	}
}
