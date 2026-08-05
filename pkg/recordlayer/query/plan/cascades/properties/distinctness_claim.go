package properties

import (
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// DistinctnessClaim is an ordering's "the rows flowed are distinct" assertion,
// BOUND to the coordinate set it was proved over.
//
// The binding is the whole point. Distinctness is not a property an ordering
// carries in the abstract: it is a property of a specific set of coordinates.
// An ordering by (a, b, c) is also an ordering by (a, b); the first may be
// strict, the second need not be. Java models this as a bare boolean and
// documents the resulting hazard in the field's own javadoc
// (Ordering.java:185-192), which ends with a hand-written interpretation rule
// a reader is expected to know and apply:
//
//	"I think for now we should just interpret this indicator in a way that only
//	 enumerated orderings that contain all values of the ordering are strict if
//	 this indicator is true."
//
// That rule is what this type enforces structurally instead. A claim records
// the coordinates it was computed over, and IsDistinct answers false the moment
// any of them is missing from the ordering asking the question. So the three
// legal outcomes of a reduction — drop the claim, recompute it, or keep it only
// when nothing was actually dropped — are the only outcomes reachable, and
// "carry it forward unexamined" is not expressible.
//
// A caller that has genuinely PROVED distinctness over the keys it is about to
// construct mints a fresh claim with DistinctOverAllKeys. A caller that is
// merely propagating someone else's claim passes the source ordering's
// DistinctnessClaim, which stays bound to the source's coordinates and
// therefore fails closed under any reduction or rename it does not survive.
type DistinctnessClaim struct {
	claimed bool
	// bindAll defers binding to construction time: the claim covers whatever
	// key set the ordering is built with. Only a producer that proved
	// distinctness over exactly those keys may use it.
	bindAll bool
	// over is the sorted, deduplicated set of ExplainValue coordinate keys the
	// claim was proved over. Meaningful only once bindAll has been resolved.
	over []string
}

// NotDistinct is the absence of a claim.
func NotDistinct() DistinctnessClaim {
	return DistinctnessClaim{}
}

// DistinctOverAllKeys mints a FRESH claim covering exactly the key set of the
// ordering being constructed. It is the producer's API: use it only where the
// distinctness of those specific coordinates was just established.
func DistinctOverAllKeys() DistinctnessClaim {
	return DistinctnessClaim{claimed: true, bindAll: true}
}

// DistinctOverAllKeysIf is DistinctOverAllKeys guarded by a producer-side fact.
func DistinctOverAllKeysIf(distinct bool) DistinctnessClaim {
	if !distinct {
		return NotDistinct()
	}
	return DistinctOverAllKeys()
}

// DistinctnessClaim returns this ordering's claim still bound to THIS
// ordering's coordinates, for a consumer that is constructing a derived
// ordering. Because the coordinates travel with it, a derived ordering that
// dropped any of them reports IsDistinct false without the deriving site
// having to notice.
func (o *RichOrdering) DistinctnessClaim() DistinctnessClaim {
	if o == nil {
		return NotDistinct()
	}
	return o.distinct
}

// IsClaimed reports whether a distinctness assertion was ever made. It says
// nothing about whether the assertion still holds — that is holdsOver's
// question, and IsDistinct is the only public answer.
func (c DistinctnessClaim) IsClaimed() bool {
	return c.claimed
}

// bindTo resolves a deferred (bindAll) claim against the key set the ordering
// is actually being constructed with. An already-bound claim is unchanged: its
// coordinates are the ones it was proved over, not the ones it is landing in.
func (c DistinctnessClaim) bindTo(keyStrings []string) DistinctnessClaim {
	if !c.claimed || !c.bindAll {
		return c
	}
	return DistinctnessClaim{claimed: true, over: sortedUniqueKeys(keyStrings)}
}

// holdsOver reports whether the claim still stands for an ordering whose
// coordinates are the keys of lookup. Every coordinate the claim was proved
// over must still be present: this is the (a, b, c) → (a, b) rule, and it is
// what makes a reduction unable to launder a claim it did not earn.
func (c DistinctnessClaim) holdsOver(lookup map[string]values.Value) bool {
	if !c.claimed {
		return false
	}
	if c.bindAll {
		// An unbound claim never reached a constructor. Nothing proved
		// anything about this key set, so it does not hold.
		return false
	}
	for _, key := range c.over {
		if _, present := lookup[key]; !present {
			return false
		}
	}
	return true
}

// translate carries a claim across a coordinate RENAME. renamed maps each old
// coordinate key to its new one; a coordinate absent from renamed did not
// survive, which drops the claim rather than shrinking it — a claim proved over
// (a, b, c) says nothing about (a, b), and recomputing it here is not possible
// because the fact that established it lives at the producer.
func (c DistinctnessClaim) translate(renamed map[string]string) DistinctnessClaim {
	if !c.claimed || c.bindAll {
		return c
	}
	translated := make([]string, 0, len(c.over))
	for _, key := range c.over {
		newKey, survived := renamed[key]
		if !survived {
			return NotDistinct()
		}
		translated = append(translated, newKey)
	}
	return DistinctnessClaim{claimed: true, over: sortedUniqueKeys(translated)}
}

// IntersectClaims is the claim for a derivation whose distinctness needs BOTH
// inputs to be distinct — a union merge, in Java's terms
// `leftOrdering.isDistinct() && rightOrdering.isDistinct()`. The result is
// bound to the UNION of both coordinate sets, because both proofs have to
// survive into the merged ordering for the conjunction to mean anything.
func IntersectClaims(a, b DistinctnessClaim) DistinctnessClaim {
	if !a.claimed || !b.claimed {
		return NotDistinct()
	}
	if a.bindAll || b.bindAll {
		// An unbound operand has no coordinates to contribute, so the
		// conjunction cannot be stated. Fail closed.
		return NotDistinct()
	}
	return DistinctnessClaim{
		claimed: true,
		over:    sortedUniqueKeys(append(append([]string{}, a.over...), b.over...)),
	}
}

func sortedUniqueKeys(keys []string) []string {
	if len(keys) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(keys))
	unique := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	sort.Strings(unique)
	return unique
}
