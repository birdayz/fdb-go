package combinatorics

// EnumeratingIterator iterates over permutations of a set, optionally
// respecting partial-order constraints. Supports skip() for pruning.
//
// Mirrors Java's EnumeratingIterator + EnumeratingIterable.
type EnumeratingIterator[T comparable] interface {
	// Next returns the next permutation, or nil if exhausted.
	Next() []T
	// Skip advances past all permutations sharing the prefix up to
	// and including the given zero-indexed level.
	Skip(level int)
}

// Permutations returns an iterator over all permutations of the set
// (no dependency constraints).
func Permutations[T comparable](set []T) EnumeratingIterator[T] {
	return TopologicalOrderPermutations(NewPartiallyOrderedSet(set, NewSetMultimap[T]()))
}

// TopologicalOrderPermutations returns an iterator over all permutations
// of the partial order that respect its dependency constraints.
func TopologicalOrderPermutations[T comparable](p *PartiallyOrderedSet[T]) EnumeratingIterator[T] {
	if p.Size() == 0 {
		return &emptyIterator[T]{}
	}
	if p.Size() == 1 {
		return &singleIterator[T]{element: p.set[0]}
	}
	return complexIterable(p)
}

// AnyTopologicalOrderPermutation returns a single valid topological ordering,
// or nil if none exists (circular dependencies).
func AnyTopologicalOrderPermutation[T comparable](p *PartiallyOrderedSet[T]) []T {
	if p.Size() == 0 {
		return nil
	}
	if p.Size() == 1 {
		return []T{p.set[0]}
	}
	if p.dependencyMap.IsEmpty() {
		result := make([]T, len(p.set))
		copy(result, p.set)
		return result
	}
	iter := newKahnIterator(NewPartiallyOrderedSet(p.set, p.dependencyMap.Inverse()))
	return iter.Next()
}

// SatisfyingPermutations returns an iterator that yields only permutations where
// the first len(targetPermutation) elements match the target (via domainMapper),
// using the satisfiability function to prune. The satisfiability function returns
// the index up to which the permutation satisfies the target; if it equals
// len(targetPermutation), the permutation is accepted.
func SatisfyingPermutations[T comparable](
	p *PartiallyOrderedSet[T],
	targetPermutation []T,
	satisfiabilityFn func([]T) int,
) EnumeratingIterator[T] {
	return SatisfyingPermutationsWithMapper(p, targetPermutation, func(t T) T { return t }, satisfiabilityFn)
}

// SatisfyingPermutationsWithMapper is like SatisfyingPermutations but with a
// domain mapper function that maps elements from T to the target domain P.
func SatisfyingPermutationsWithMapper[T, P comparable](
	p *PartiallyOrderedSet[T],
	targetPermutation []P,
	domainMapper func(T) P,
	satisfiabilityFn func([]T) int,
) EnumeratingIterator[T] {
	if p.Size() < len(targetPermutation) {
		return &emptyIterator[T]{}
	}

	domainMap := make(map[P][]T)
	for _, e := range p.set {
		mapped := domainMapper(e)
		domainMap[mapped] = append(domainMap[mapped], e)
	}

	var inner EnumeratingIterator[T]
	if p.Size() > 1 {
		inner = newBacktrackIteratorWithDomain(p, func(level int) []T {
			if level < len(targetPermutation) {
				return domainMap[targetPermutation[level]]
			}
			return p.set
		})
	} else if p.Size() == 1 {
		inner = &singleIterator[T]{element: p.set[0]}
	} else {
		inner = &emptyIterator[T]{}
	}

	return &satisfyingIterator[T]{
		inner:     inner,
		targetLen: len(targetPermutation),
		satisfyFn: satisfiabilityFn,
	}
}

type satisfyingIterator[T comparable] struct {
	inner     EnumeratingIterator[T]
	targetLen int
	satisfyFn func([]T) int
}

func (s *satisfyingIterator[T]) Next() []T {
	for {
		ordered := s.inner.Next()
		if ordered == nil {
			return nil
		}
		idx := s.satisfyFn(ordered)
		if idx == s.targetLen {
			return ordered
		}
		s.inner.Skip(idx)
	}
}

func (s *satisfyingIterator[T]) Skip(level int) {
	s.inner.Skip(level)
}

func complexIterable[T comparable](p *PartiallyOrderedSet[T]) EnumeratingIterator[T] {
	depRatio := float64(p.dependencyMap.Size()) / float64(p.Size())
	if depRatio > 0.5 {
		return newKahnIterator(p.DualOrder())
	}
	return newBacktrackIterator(p)
}

// --- Backtrack iterator ---

type backtrackIterator[T comparable] struct {
	p        *PartiallyOrderedSet[T]
	bound    map[T]struct{}
	state    []backtrackLevel[T]
	started  bool
	domainFn func(int) []T
}

type backtrackLevel[T comparable] struct {
	elements []T
	pos      int
	active   bool
}

func newBacktrackIterator[T comparable](p *PartiallyOrderedSet[T]) *backtrackIterator[T] {
	return newBacktrackIteratorWithDomain(p, func(_ int) []T { return p.set })
}

func newBacktrackIteratorWithDomain[T comparable](p *PartiallyOrderedSet[T], domainFn func(int) []T) *backtrackIterator[T] {
	state := make([]backtrackLevel[T], p.Size())
	return &backtrackIterator[T]{
		p:        p,
		bound:    make(map[T]struct{}, p.Size()),
		state:    state,
		domainFn: domainFn,
	}
}

func (b *backtrackIterator[T]) Next() []T {
	var currentLevel int
	if !b.started || len(b.bound) == 0 {
		currentLevel = 0
		b.started = true
	} else {
		currentLevel = len(b.bound) - 1
	}

	for {
		if !b.state[currentLevel].active {
			domain := b.domainFn(currentLevel)
			b.state[currentLevel] = backtrackLevel[T]{elements: domain, pos: 0, active: true}
		} else {
			b.unbind(currentLevel)
			b.state[currentLevel].pos++
		}

		isDown := b.searchLevel(currentLevel)
		if !isDown {
			b.state[currentLevel] = backtrackLevel[T]{}
		}

		if isDown {
			currentLevel++
		} else {
			currentLevel--
		}

		if currentLevel == -1 {
			return nil
		}

		if len(b.bound) >= b.p.Size() {
			break
		}
	}

	result := make([]T, b.p.Size())
	for i := range b.state {
		result[i] = b.state[i].elements[b.state[i].pos]
	}
	return result
}

func (b *backtrackIterator[T]) searchLevel(level int) bool {
	st := &b.state[level]
	deps := b.p.dependencyMap
	setLookup := b.p.setLookup

	for st.pos < len(st.elements) {
		next := st.elements[st.pos]
		if _, bound := b.bound[next]; bound {
			st.pos++
			continue
		}
		dependsOn := deps.Get(next)
		allSatisfied := true
		for dep := range dependsOn {
			if _, inSet := setLookup[dep]; !inSet {
				continue
			}
			if _, bound := b.bound[dep]; !bound {
				allSatisfied = false
				break
			}
		}
		if !allSatisfied {
			st.pos++
			continue
		}
		b.bound[next] = struct{}{}
		return true
	}
	return false
}

func (b *backtrackIterator[T]) unbind(level int) {
	for i := level; i < b.p.Size(); i++ {
		if !b.state[i].active {
			break
		}
		elem := b.state[i].elements[b.state[i].pos]
		delete(b.bound, elem)
	}
}

func (b *backtrackIterator[T]) Skip(level int) {
	for i := level + 1; i < b.p.Size(); i++ {
		if !b.state[i].active {
			break
		}
		elem := b.state[i].elements[b.state[i].pos]
		delete(b.bound, elem)
		b.state[i] = backtrackLevel[T]{}
	}
}

// --- Kahn iterator ---

type kahnIterator[T comparable] struct {
	p            *PartiallyOrderedSet[T]
	bound        map[T]struct{}
	inDegreeMap  map[T]int
	eligibleSets []map[T]struct{}
	iterators    []kahnLevel[T]
	started      bool
}

type kahnLevel[T comparable] struct {
	elements []T
	pos      int
	active   bool
}

func newKahnIterator[T comparable](p *PartiallyOrderedSet[T]) *kahnIterator[T] {
	inDeg := make(map[T]int, p.Size())
	for _, e := range p.set {
		inDeg[e] = 0
	}
	for _, entry := range p.dependencyMap.Entries() {
		inDeg[entry.Value]++
	}

	initial := make(map[T]struct{})
	for k, v := range inDeg {
		if v == 0 {
			initial[k] = struct{}{}
		}
	}

	eligibleSets := make([]map[T]struct{}, p.Size())
	eligibleSets[0] = initial

	return &kahnIterator[T]{
		p:            p,
		bound:        make(map[T]struct{}, p.Size()),
		inDegreeMap:  inDeg,
		eligibleSets: eligibleSets,
		iterators:    make([]kahnLevel[T], p.Size()),
	}
}

func (k *kahnIterator[T]) Next() []T {
	var currentLevel int
	if !k.started || len(k.bound) == 0 {
		currentLevel = 0
		k.started = true
	} else {
		currentLevel = len(k.bound) - 1
	}

	for {
		if !k.iterators[currentLevel].active {
			elems := setToSlice(k.eligibleSets[currentLevel])
			k.iterators[currentLevel] = kahnLevel[T]{elements: elems, pos: 0, active: true}
		} else {
			k.unbindTail(currentLevel)
			k.iterators[currentLevel].pos++
		}

		foundOnLevel := k.nextOnLevel(currentLevel)
		if !foundOnLevel {
			k.iterators[currentLevel] = kahnLevel[T]{}
		}

		if foundOnLevel {
			currentLevel++
		} else {
			currentLevel--
		}

		if currentLevel == -1 {
			return nil
		}

		if len(k.bound) >= k.p.Size() {
			break
		}
	}

	result := make([]T, k.p.Size())
	for i := range k.iterators {
		result[i] = k.iterators[i].elements[k.iterators[i].pos]
	}
	return result
}

func (k *kahnIterator[T]) nextOnLevel(level int) bool {
	it := &k.iterators[level]
	usedByMap := k.p.dependencyMap

	for it.pos < len(it.elements) {
		next := it.elements[it.pos]
		if _, bound := k.bound[next]; bound {
			it.pos++
			continue
		}
		k.bound[next] = struct{}{}

		newlyEligible := make(map[T]struct{})
		for target := range usedByMap.Get(next) {
			newDeg := k.inDegreeMap[target] - 1
			k.inDegreeMap[target] = newDeg
			if newDeg == 0 {
				newlyEligible[target] = struct{}{}
			}
		}

		if len(k.bound) < k.p.Size() {
			merged := make(map[T]struct{})
			for e := range k.eligibleSets[len(k.bound)-1] {
				merged[e] = struct{}{}
			}
			for e := range newlyEligible {
				merged[e] = struct{}{}
			}
			k.eligibleSets[len(k.bound)] = merged
		}

		return true
	}
	return false
}

func (k *kahnIterator[T]) unbindTail(level int) {
	for i := level; i < k.p.Size(); i++ {
		if !k.iterators[i].active {
			break
		}
		k.unbindAt(i)
	}
}

func (k *kahnIterator[T]) unbindAt(level int) {
	toUnbind := k.iterators[level].elements[k.iterators[level].pos]
	delete(k.bound, toUnbind)
	for target := range k.p.dependencyMap.Get(toUnbind) {
		k.inDegreeMap[target]++
	}
}

func (k *kahnIterator[T]) Skip(level int) {
	for i := level + 1; i < k.p.Size(); i++ {
		if !k.iterators[i].active {
			break
		}
		k.unbindAt(i)
		k.iterators[i] = kahnLevel[T]{}
	}
}

// --- Trivial iterators ---

type emptyIterator[T comparable] struct{}

func (e *emptyIterator[T]) Next() []T  { return nil }
func (e *emptyIterator[T]) Skip(_ int) {}

type singleIterator[T comparable] struct {
	element T
	done    bool
}

func (s *singleIterator[T]) Next() []T {
	if s.done {
		return nil
	}
	s.done = true
	return []T{s.element}
}

func (s *singleIterator[T]) Skip(_ int) {}

func setToSlice[T comparable](s map[T]struct{}) []T {
	result := make([]T, 0, len(s))
	for k := range s {
		result = append(result, k)
	}
	return result
}
