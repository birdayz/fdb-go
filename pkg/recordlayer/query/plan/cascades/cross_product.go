package cascades

// CrossProduct computes the Cartesian product of N lists. Each list
// provides alternatives for one "dimension"; the result is every
// combination of picking one element from each list.
//
// For input [[a, b], [x, y]], returns [[a, x], [a, y], [b, x], [b, y]].
// Empty input returns nil. Any empty inner list makes the product empty.
//
// Mirrors Java's CrossProduct.crossProduct utility.
func CrossProduct[T any](lists [][]T) [][]T {
	if len(lists) == 0 {
		return nil
	}
	result := [][]T{{}}
	for _, list := range lists {
		if len(list) == 0 {
			return nil
		}
		var next [][]T
		for _, existing := range result {
			for _, elem := range list {
				combo := make([]T, len(existing)+1)
				copy(combo, existing)
				combo[len(existing)] = elem
				next = append(next, combo)
			}
		}
		result = next
	}
	return result
}

// CrossProductIterator lazily enumerates the Cartesian product with
// support for skipping entire subtrees. When an incremental merge
// fails at depth d, Skip(d) advances past all combinations sharing
// the same prefix up to position d.
type CrossProductIterator[T any] struct {
	lists   [][]T
	indices []int
	done    bool
}

func NewCrossProductIterator[T any](lists [][]T) *CrossProductIterator[T] {
	for _, l := range lists {
		if len(l) == 0 {
			return &CrossProductIterator[T]{done: true}
		}
	}
	return &CrossProductIterator[T]{
		lists:   lists,
		indices: make([]int, len(lists)),
	}
}

func (it *CrossProductIterator[T]) HasNext() bool { return !it.done }

func (it *CrossProductIterator[T]) Next() []T {
	if it.done {
		return nil
	}
	result := make([]T, len(it.lists))
	for i, idx := range it.indices {
		result[i] = it.lists[i][idx]
	}
	it.advance()
	return result
}

func (it *CrossProductIterator[T]) advance() {
	for i := len(it.indices) - 1; i >= 0; i-- {
		it.indices[i]++
		if it.indices[i] < len(it.lists[i]) {
			return
		}
		it.indices[i] = 0
	}
	it.done = true
}

// Skip advances past all combinations sharing the same prefix up to
// position (depth-1). Equivalent to incrementing indices[depth-1] and
// resetting all deeper positions.
func (it *CrossProductIterator[T]) Skip(depth int) {
	if it.done || depth <= 0 || depth > len(it.indices) {
		return
	}
	pos := depth - 1
	it.indices[pos]++
	for i := pos + 1; i < len(it.indices); i++ {
		it.indices[i] = 0
	}
	if it.indices[pos] >= len(it.lists[pos]) {
		it.indices[pos] = 0
		for i := pos - 1; i >= 0; i-- {
			it.indices[i]++
			if it.indices[i] < len(it.lists[i]) {
				return
			}
			it.indices[i] = 0
		}
		it.done = true
	}
}
