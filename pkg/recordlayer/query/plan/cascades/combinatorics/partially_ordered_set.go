package combinatorics

import (
	"fmt"
	"strings"
	"sync"
)

// PartiallyOrderedSet represents a partially ordered set of elements.
// A partial order over a set is a relation that is irreflexive, transitive,
// and asymmetric. The dependency map encodes "key depends on value" edges.
//
// Mirrors Java's com.apple.foundationdb.record.query.combinatorics.PartiallyOrderedSet.
type PartiallyOrderedSet[T comparable] struct {
	set           []T
	setLookup     map[T]struct{}
	dependencyMap SetMultimap[T]

	dualOnce          sync.Once
	dual              *PartiallyOrderedSet[T]
	transitiveOnce    sync.Once
	transitiveClosure SetMultimap[T]
}

// SetMultimap maps each key to a set of values.
type SetMultimap[T comparable] map[T]map[T]struct{}

func NewSetMultimap[T comparable]() SetMultimap[T] {
	return make(SetMultimap[T])
}

func (m SetMultimap[T]) Put(key, value T) {
	s, ok := m[key]
	if !ok {
		s = make(map[T]struct{})
		m[key] = s
	}
	s[value] = struct{}{}
}

func (m SetMultimap[T]) Get(key T) map[T]struct{} {
	return m[key]
}

func (m SetMultimap[T]) Contains(key, value T) bool {
	s, ok := m[key]
	if !ok {
		return false
	}
	_, ok = s[value]
	return ok
}

func (m SetMultimap[T]) Entries() []MapEntry[T] {
	var result []MapEntry[T]
	for k, vs := range m {
		for v := range vs {
			result = append(result, MapEntry[T]{Key: k, Value: v})
		}
	}
	return result
}

func (m SetMultimap[T]) Size() int {
	n := 0
	for _, vs := range m {
		n += len(vs)
	}
	return n
}

func (m SetMultimap[T]) IsEmpty() bool {
	return m.Size() == 0
}

func (m SetMultimap[T]) Inverse() SetMultimap[T] {
	inv := NewSetMultimap[T]()
	for k, vs := range m {
		for v := range vs {
			inv.Put(v, k)
		}
	}
	return inv
}

func (m SetMultimap[T]) Clone() SetMultimap[T] {
	c := NewSetMultimap[T]()
	for k, vs := range m {
		for v := range vs {
			c.Put(k, v)
		}
	}
	return c
}

// MapEntry is a key-value pair from a SetMultimap.
type MapEntry[T comparable] struct {
	Key   T
	Value T
}

// NewPartiallyOrderedSet creates a new PartiallyOrderedSet from a set and dependency map.
// The dependency map is cleansed: entries referencing elements not in the set are removed.
func NewPartiallyOrderedSet[T comparable](set []T, dependencyMap SetMultimap[T]) *PartiallyOrderedSet[T] {
	lookup := makeSetLookup(set)
	cleaned := cleanseDependencyMap(lookup, dependencyMap)
	return &PartiallyOrderedSet[T]{
		set:           copySlice(set),
		setLookup:     lookup,
		dependencyMap: cleaned,
	}
}

// EmptyPartiallyOrderedSet returns an empty partial order.
func EmptyPartiallyOrderedSet[T comparable]() *PartiallyOrderedSet[T] {
	return &PartiallyOrderedSet[T]{
		setLookup:     make(map[T]struct{}),
		dependencyMap: NewSetMultimap[T](),
	}
}

func (p *PartiallyOrderedSet[T]) Set() []T {
	return p.set
}

func (p *PartiallyOrderedSet[T]) SetLookup() map[T]struct{} {
	return p.setLookup
}

func (p *PartiallyOrderedSet[T]) IsEmpty() bool {
	return len(p.set) == 0
}

func (p *PartiallyOrderedSet[T]) Size() int {
	return len(p.set)
}

func (p *PartiallyOrderedSet[T]) DependencyMap() SetMultimap[T] {
	return p.dependencyMap
}

func (p *PartiallyOrderedSet[T]) TransitiveClosure() SetMultimap[T] {
	p.transitiveOnce.Do(func() {
		p.transitiveClosure = TransitiveClosure(p)
	})
	return p.transitiveClosure
}

// DualOrder returns the partial order with all dependency edges reversed.
func (p *PartiallyOrderedSet[T]) DualOrder() *PartiallyOrderedSet[T] {
	p.dualOnce.Do(func() {
		p.dual = NewPartiallyOrderedSet(p.set, p.dependencyMap.Inverse())
	})
	return p.dual
}

// EligibleSet returns a new EligibleSet for iterative consumption of this partial order.
func (p *PartiallyOrderedSet[T]) EligibleSet() *EligibleSet[T] {
	return newEligibleSet(p)
}

func (p *PartiallyOrderedSet[T]) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range p.set {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%v", e)
	}
	if !p.dependencyMap.IsEmpty() {
		b.WriteString("| ")
		first := true
		for _, entry := range p.dependencyMap.Entries() {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%v←%v", entry.Value, entry.Key)
			first = false
		}
	}
	b.WriteByte('}')
	return b.String()
}

// Equal checks equality via set equality and transitive closure equality.
func (p *PartiallyOrderedSet[T]) Equal(other *PartiallyOrderedSet[T]) bool {
	if p.Size() != other.Size() {
		return false
	}
	for _, e := range p.set {
		if _, ok := other.setLookup[e]; !ok {
			return false
		}
	}
	return setMultimapEqual(p.TransitiveClosure(), other.TransitiveClosure())
}

// FilterElements returns a new partial order retaining only elements passing the predicate.
// Filtering respects dependency chains (see MapAll).
func (p *PartiallyOrderedSet[T]) FilterElements(predicate func(T) bool) *PartiallyOrderedSet[T] {
	translationMap := make(map[T]T)
	for _, t := range p.set {
		if predicate(t) {
			translationMap[t] = t
		}
	}
	return MapAll(p, translationMap)
}

// FromFunctionalDependencies builds a dependency multimap from a set and a function
// that returns the dependencies of each element.
func FromFunctionalDependencies[T comparable](set map[T]struct{}, dependsOnFn func(T) map[T]struct{}) SetMultimap[T] {
	m := NewSetMultimap[T]()
	for element := range set {
		for dep := range dependsOnFn(element) {
			if _, ok := set[dep]; ok {
				m.Put(element, dep)
			}
		}
	}
	return m
}

// InvertFromFunctionalDependencies builds an inverted dependency multimap.
func InvertFromFunctionalDependencies[T comparable](set map[T]struct{}, dependsOnFn func(T) map[T]struct{}) SetMultimap[T] {
	m := NewSetMultimap[T]()
	for element := range set {
		for dep := range dependsOnFn(element) {
			if _, ok := set[dep]; ok {
				m.Put(dep, element)
			}
		}
	}
	return m
}

// NewPartiallyOrderedSetFromFunc creates a partial order from a set and a depends-on function.
func NewPartiallyOrderedSetFromFunc[T comparable](set []T, dependsOnFn func(T) map[T]struct{}) *PartiallyOrderedSet[T] {
	lookup := makeSetLookup(set)
	return NewPartiallyOrderedSet(set, FromFunctionalDependencies(lookup, dependsOnFn))
}

// NewPartiallyOrderedSetInverted creates a partial order with inverted dependencies.
func NewPartiallyOrderedSetInverted[T comparable](set []T, dependencyMap SetMultimap[T]) *PartiallyOrderedSet[T] {
	return NewPartiallyOrderedSet(set, dependencyMap.Inverse())
}

// MapAll maps this partial order to a different domain while maintaining dependencies.
// Elements not in the map (and elements depending on unmapped elements with no
// alternative dependency chain) are removed. See Java's PartiallyOrderedSet.mapAll(Map).
func MapAll[T, R comparable](p *PartiallyOrderedSet[T], m map[T]R) *PartiallyOrderedSet[R] {
	unmapped := make(map[T]struct{})
	for _, e := range p.set {
		if _, ok := m[e]; !ok {
			unmapped[e] = struct{}{}
		}
	}

	degreeMap := make(map[T]int)
	for _, entry := range p.dependencyMap.Entries() {
		degreeMap[entry.Key]++
	}

	allRemoved := make(map[T]struct{})
	leftToRemove := make(map[T]struct{})
	for k := range unmapped {
		leftToRemove[k] = struct{}{}
	}

	for len(leftToRemove) > 0 {
		var toRemove T
		for k := range leftToRemove {
			toRemove = k
			break
		}
		for _, entry := range p.dependencyMap.Entries() {
			if entry.Value == toRemove {
				if deg, ok := degreeMap[entry.Key]; ok {
					updated := deg - 1
					if updated == 0 {
						leftToRemove[entry.Key] = struct{}{}
						delete(degreeMap, entry.Key)
					} else {
						degreeMap[entry.Key] = updated
					}
				}
			}
		}
		delete(leftToRemove, toRemove)
		allRemoved[toRemove] = struct{}{}
	}

	var mappedElements []R
	for _, e := range p.set {
		if _, removed := allRemoved[e]; !removed {
			mappedElements = append(mappedElements, m[e])
		}
	}

	resultDeps := NewSetMultimap[R]()
	for _, entry := range p.dependencyMap.Entries() {
		_, kRemoved := allRemoved[entry.Key]
		_, vRemoved := allRemoved[entry.Value]
		if !kRemoved && !vRemoved {
			resultDeps.Put(m[entry.Key], m[entry.Value])
		}
	}

	return NewPartiallyOrderedSet(mappedElements, resultDeps)
}

// EligibleSet computes subsets of elements that are minimal (no unsatisfied dependencies).
// Used for iterative topological consumption.
type EligibleSet[T comparable] struct {
	partialOrder *PartiallyOrderedSet[T]
	inDegreeMap  map[T]int
	eligible     map[T]struct{}
}

func newEligibleSet[T comparable](p *PartiallyOrderedSet[T]) *EligibleSet[T] {
	inDeg := computeInDegreeMap(p)
	eligible := make(map[T]struct{})
	for k, v := range inDeg {
		if v == 0 {
			eligible[k] = struct{}{}
		}
	}
	return &EligibleSet[T]{
		partialOrder: p,
		inDegreeMap:  inDeg,
		eligible:     eligible,
	}
}

func (e *EligibleSet[T]) PartialOrder() *PartiallyOrderedSet[T] {
	return e.partialOrder
}

func (e *EligibleSet[T]) IsEmpty() bool {
	return len(e.eligible) == 0
}

func (e *EligibleSet[T]) EligibleElements() map[T]struct{} {
	return e.eligible
}

// RemoveEligibleElements removes the given elements (which must be currently eligible)
// and returns a new EligibleSet reflecting the reduced partial order.
func (e *EligibleSet[T]) RemoveEligibleElements(toRemove map[T]struct{}) *EligibleSet[T] {
	var newSet []T
	for _, elem := range e.partialOrder.set {
		if _, ok := toRemove[elem]; !ok {
			newSet = append(newSet, elem)
		}
	}

	newDeps := NewSetMultimap[T]()
	for _, entry := range e.partialOrder.dependencyMap.Entries() {
		if _, ok := toRemove[entry.Value]; !ok {
			newDeps.Put(entry.Key, entry.Value)
		}
	}

	return newEligibleSet(NewPartiallyOrderedSet(newSet, newDeps))
}

func computeInDegreeMap[T comparable](p *PartiallyOrderedSet[T]) map[T]int {
	result := make(map[T]int, p.Size())
	for _, e := range p.set {
		result[e] = 0
	}
	for _, entry := range p.dependencyMap.Entries() {
		result[entry.Key]++
	}
	return result
}

func cleanseDependencyMap[T comparable](setLookup map[T]struct{}, deps SetMultimap[T]) SetMultimap[T] {
	clean := NewSetMultimap[T]()
	needsCopy := false
	for k, vs := range deps {
		if _, okK := setLookup[k]; !okK {
			needsCopy = true
			continue
		}
		for v := range vs {
			if _, okV := setLookup[v]; !okV {
				needsCopy = true
				continue
			}
			clean.Put(k, v)
		}
	}
	if !needsCopy {
		return deps
	}
	return clean
}

func makeSetLookup[T comparable](set []T) map[T]struct{} {
	m := make(map[T]struct{}, len(set))
	for _, e := range set {
		m[e] = struct{}{}
	}
	return m
}

func copySlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	c := make([]T, len(s))
	copy(c, s)
	return c
}

func setMultimapEqual[T comparable](a, b SetMultimap[T]) bool {
	if a.Size() != b.Size() {
		return false
	}
	for k, vs := range a {
		bvs, ok := b[k]
		if !ok || len(bvs) != len(vs) {
			return false
		}
		for v := range vs {
			if _, ok := bvs[v]; !ok {
				return false
			}
		}
	}
	return true
}

// Builder for constructing PartiallyOrderedSet incrementally.
type Builder[T comparable] struct {
	set  []T
	seen map[T]struct{}
	deps SetMultimap[T]
}

func NewBuilder[T comparable]() *Builder[T] {
	return &Builder[T]{
		seen: make(map[T]struct{}),
		deps: NewSetMultimap[T](),
	}
}

func (b *Builder[T]) Add(element T) *Builder[T] {
	if _, ok := b.seen[element]; !ok {
		b.set = append(b.set, element)
		b.seen[element] = struct{}{}
	}
	return b
}

// AddDependency adds a dependency: target depends on source.
func (b *Builder[T]) AddDependency(target, source T) *Builder[T] {
	b.Add(source)
	b.Add(target)
	b.deps.Put(target, source)
	return b
}

func (b *Builder[T]) AddAll(elements []T) *Builder[T] {
	for _, e := range elements {
		b.Add(e)
	}
	return b
}

// AddListWithDependencies adds elements in order, creating a dependency chain:
// each element depends on the previous one.
func (b *Builder[T]) AddListWithDependencies(elements []T) *Builder[T] {
	b.AddAll(elements)
	for i := 1; i < len(elements); i++ {
		b.deps.Put(elements[i], elements[i-1])
	}
	return b
}

func (b *Builder[T]) Build() *PartiallyOrderedSet[T] {
	return NewPartiallyOrderedSet(b.set, b.deps)
}
