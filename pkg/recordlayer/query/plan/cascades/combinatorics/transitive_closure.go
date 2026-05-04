package combinatorics

import "fmt"

// TransitiveClosure computes the transitive closure of the dependency map
// in the given partial order using Kahn's algorithm (topological order).
// Panics on circular dependencies.
//
// Mirrors Java's TransitiveClosure.transitiveClosure.
func TransitiveClosure[T comparable](p *PartiallyOrderedSet[T]) SetMultimap[T] {
	usedByMap := p.dependencyMap.Inverse()
	inDegreeMap := make(map[T]int, p.Size())
	for _, e := range p.set {
		inDegreeMap[e] = 0
	}
	for _, entry := range usedByMap.Entries() {
		inDegreeMap[entry.Value]++
	}

	queue := make([]T, 0, p.Size())
	for _, e := range p.set {
		if inDegreeMap[e] == 0 {
			queue = append(queue, e)
		}
	}

	result := NewSetMultimap[T]()
	processed := 0

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		processed++

		for using := range usedByMap.Get(current) {
			newDeg := inDegreeMap[using] - 1
			inDegreeMap[using] = newDeg
			if newDeg == 0 {
				queue = append(queue, using)
				for dep := range p.dependencyMap.Get(using) {
					result.Put(using, dep)
					for ancestor := range result.Get(dep) {
						result.Put(using, ancestor)
					}
				}
			}
		}
	}

	if processed != p.Size() {
		panic(fmt.Sprintf("circular dependency: processed %d of %d elements", processed, p.Size()))
	}

	return result
}
