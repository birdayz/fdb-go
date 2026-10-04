package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// distinctUnionLegOrderings are the orderings a union leg's partitions can
// provide over (a, id), all identified by id except the last.
func distinctUnionLegOrderings(leg int) []*properties.RichOrdering {
	fields := distinctUnionNamedFields("a", "id")
	a, id := fields[0], fields[1]
	asc := properties.SortedBinding(properties.ProvidedSortOrderAscending)
	byID := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{id: {asc}},
		[]values.Value{id}, properties.NotDistinct()).WithRecordIdentity([]values.Value{id})
	byAID := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{a: {asc}, id: {asc}},
		[]values.Value{a, id}, properties.NotDistinct()).WithRecordIdentity([]values.Value{id})
	aFixedByID := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			a:  {properties.FixedBinding(fmt.Sprintf("a = %d", leg))},
			id: {asc},
		},
		[]values.Value{a, id}, properties.NotDistinct()).WithRecordIdentity([]values.Value{id})
	byA := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{a: {asc}},
		[]values.Value{a}, properties.NotDistinct())
	return []*properties.RichOrdering{byID, byAID, aFixedByID, byA}
}

// bruteForceUnionMergeStates is Java's walk: every combination of one ordering
// per leg, merged left to right, dropped at the first merge whose legs' record
// identities leave the merged keys.
func bruteForceUnionMergeStates(legOrderings [][]*properties.RichOrdering) []unionMergeState {
	var states []unionMergeState
	for _, combo := range CrossProduct(legOrderings) {
		state := unionMergeState{
			merged:   properties.CreateUnionOrdering(combo[0]),
			identity: combo[0].RecordIdentityClaim(),
		}
		ok := true
		for _, ordering := range combo[1:] {
			merged := properties.MergeOrderings(state.merged, ordering)
			identity := properties.IntersectClaims(state.identity, ordering.RecordIdentityClaim())
			if !identity.Within(merged.GetKeys()) {
				ok = false
				break
			}
			state = unionMergeState{merged: merged, identity: identity}
		}
		if !ok {
			continue
		}
		seen := false
		for _, existing := range states {
			if existing.equals(state) {
				seen = true
				break
			}
		}
		if !seen {
			states = append(states, state)
		}
	}
	return states
}

func TestReachableUnionMergeStatesMatchCrossProduct(t *testing.T) {
	t.Parallel()
	for legs := 2; legs <= 4; legs++ {
		legOrderings := make([][]*properties.RichOrdering, legs)
		for i := range legOrderings {
			legOrderings[i] = distinctUnionLegOrderings(i)
		}
		want := bruteForceUnionMergeStates(legOrderings)
		got := reachableUnionMergeStates(legOrderings)
		if len(want) == 0 {
			t.Fatalf("%d legs: the fixture reaches no merge state, so the comparison proves nothing", legs)
		}
		if len(got) != len(want) {
			t.Fatalf("%d legs: %d states, the cross product reaches %d", legs, len(got), len(want))
		}
		for _, w := range want {
			found := false
			for _, g := range got {
				if g.equals(w) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%d legs: missing the cross product's state over %v", legs, w.merged.GetKeys())
			}
		}
	}
}

// Java's walk over 40 such legs is 4^40 combinations; the states stay the
// handful of distinct merged orderings.
func TestReachableUnionMergeStatesStayBoundedOverManyLegs(t *testing.T) {
	t.Parallel()
	legOrderings := make([][]*properties.RichOrdering, 40)
	for i := range legOrderings {
		legOrderings[i] = distinctUnionLegOrderings(i)
	}
	states := reachableUnionMergeStates(legOrderings)
	if len(states) == 0 || len(states) > 3 {
		t.Fatalf("40 legs reach %d merge states, want between 1 and 3", len(states))
	}
}
