// Package javahashmap reproduces java.util.HashMap iteration order over
// string keys, where Java stores a map's iteration order in bytes Go matches.
package javahashmap

import (
	"math"
	"unicode/utf16"
)

const (
	maximumCapacity = 1 << 30
	treeCapacity    = 64
	untreeifyLimit  = 6
)

// Order models an empty map receiving putAll(first), then individual puts of
// then. Its legacy bulk sizing uses float32(size)/0.75 + 1; it is deliberately
// distinct from Guava expected-size construction (and newer JDK putAll sizing).
// first represents source-map iteration and is expected to contain distinct keys.
func Order(first, then []string) []string {
	m := stringMap{}
	if len(first) != 0 {
		m.threshold = tableSizeFor(int(float32(len(first))/0.75 + 1))
	}
	for _, key := range first {
		m.put(key)
	}
	for _, key := range then {
		m.put(key)
	}
	return m.order()
}

// ExpectedSizeOrder models Guava Maps.newHashMapWithExpectedSize(len(keys))
// followed by individual puts. Duplicate keys replace values without moving;
// the constructor expectation still includes every supplied key.
func ExpectedSizeOrder(keys []string) []string {
	m := stringMap{threshold: tableSizeFor(expectedCapacity(len(keys)))}
	for _, key := range keys {
		m.put(key)
	}
	return m.order()
}

func expectedCapacity(size int) int {
	if size < 3 {
		return size + 1
	}
	if size >= maximumCapacity {
		return math.MaxInt32
	}
	return int(math.Ceil(float64(size) / 0.75))
}

func tableSizeFor(c int) int {
	n := 1
	for n < c && n < maximumCapacity {
		n <<= 1
	}
	return n
}

type entry struct {
	key                 string
	hash                uint32
	next, prev          *entry
	parent, left, right *entry
	red, tree           bool
}

// The chain, not the tree traversal, defines iteration. Tree links only decide
// insertion parents and which root must be moved to the front of that chain.
// This insertion-only implementation is independently authored from the
// observable HashMap rules; it contains no OpenJDK source translation.
type stringMap struct {
	table     []*entry
	size      int
	threshold int // requested capacity until the first allocation
}

func (m *stringMap) order() []string {
	out := make([]string, 0, m.size)
	for _, head := range m.table {
		for e := head; e != nil; e = e.next {
			out = append(out, e.key)
		}
	}
	return out
}

func (m *stringMap) put(key string) {
	if len(m.table) == 0 {
		m.resize()
	}
	hash := javaSpread(key)
	bucket := int(hash & uint32(len(m.table)-1))
	head := m.table[bucket]
	switch {
	case head == nil:
		m.table[bucket] = &entry{key: key, hash: hash}
	case head.tree:
		parent := head
		for {
			cmp := compareKey(hash, key, parent)
			if cmp == 0 {
				return
			}
			child := parent.right
			if cmp < 0 {
				child = parent.left
			}
			if child != nil {
				parent = child
				continue
			}
			e := &entry{key: key, hash: hash, tree: true, parent: parent, prev: parent, next: parent.next}
			if parent.next != nil {
				parent.next.prev = e
			}
			parent.next = e
			if cmp < 0 {
				parent.left = e
			} else {
				parent.right = e
			}
			m.front(bucket, balance(head, e))
			break
		}
	default:
		count := 1
		tail := head
		for {
			if tail.key == key {
				return
			}
			if tail.next == nil {
				break
			}
			tail = tail.next
			count++
		}
		tail.next = &entry{key: key, hash: hash}
		if count >= 8 {
			if len(m.table) < treeCapacity {
				m.resize()
			} else {
				m.treeify(bucket)
			}
		}
	}
	m.size++
	if m.size > m.threshold {
		m.resize()
	}
}

func (m *stringMap) resize() {
	old := m.table
	capacity := len(old)
	if capacity == maximumCapacity {
		m.threshold = math.MaxInt32
		return
	}
	newCapacity, newThreshold := 16, 12
	if capacity != 0 {
		newCapacity = capacity * 2
		newThreshold = newCapacity/2 + newCapacity/4
		if capacity >= 16 {
			newThreshold = m.threshold * 2
		}
	} else if m.threshold != 0 {
		newCapacity = m.threshold
		newThreshold = newCapacity/2 + newCapacity/4
	}
	if newCapacity == maximumCapacity {
		newThreshold = math.MaxInt32
	}
	m.table = make([]*entry, newCapacity)
	m.threshold = newThreshold
	for bucket, head := range old {
		if head == nil {
			continue
		}
		wasTree := head.tree
		var heads, tails [2]*entry
		var counts [2]int
		for e := head; e != nil; {
			next := e.next
			side := 0
			if e.hash&uint32(capacity) != 0 {
				side = 1
			}
			e.next = nil
			if wasTree {
				e.prev = tails[side]
			}
			if tails[side] == nil {
				heads[side] = e
			} else {
				tails[side].next = e
			}
			tails[side] = e
			counts[side]++
			e = next
		}
		for side, first := range heads {
			index := bucket + side*capacity
			m.table[index] = first
			if first == nil || !wasTree {
				continue
			}
			if counts[side] <= untreeifyLimit {
				for e := first; e != nil; e = e.next {
					e.tree, e.red = false, false
					e.prev, e.parent, e.left, e.right = nil, nil, nil, nil
				}
			} else if counts[1-side] != 0 {
				m.treeify(index)
			}
		}
	}
}

func (m *stringMap) treeify(bucket int) {
	var root, previous *entry
	for e := m.table[bucket]; e != nil; e = e.next {
		e.tree = true
		e.prev = previous
		e.parent, e.left, e.right = nil, nil, nil
		e.red = false
		if root == nil {
			root = e
		} else {
			p := root
			for {
				if compareKey(e.hash, e.key, p) < 0 {
					if p.left != nil {
						p = p.left
						continue
					}
					p.left = e
				} else {
					if p.right != nil {
						p = p.right
						continue
					}
					p.right = e
				}
				e.parent = p
				root = balance(root, e)
				break
			}
		}
		previous = e
	}
	m.front(bucket, root)
}

func (m *stringMap) front(bucket int, root *entry) {
	head := m.table[bucket]
	if root == head {
		return
	}
	root.prev.next = root.next
	if root.next != nil {
		root.next.prev = root.prev
	}
	root.prev = nil
	root.next = head
	head.prev = root
	m.table[bucket] = root
}

// rotate changes tree links only. left selects the direction of rotation.
func rotate(root, pivot *entry, left bool) *entry {
	child := pivot.left
	if left {
		child = pivot.right
		pivot.right = child.left
		if child.left != nil {
			child.left.parent = pivot
		}
		child.left = pivot
	} else {
		pivot.left = child.right
		if child.right != nil {
			child.right.parent = pivot
		}
		child.right = pivot
	}
	child.parent = pivot.parent
	if pivot.parent == nil {
		root = child
	} else if pivot == pivot.parent.left {
		pivot.parent.left = child
	} else {
		pivot.parent.right = child
	}
	pivot.parent = child
	return root
}

func balance(root, added *entry) *entry {
	added.red = true
	for added.parent != nil && added.parent.red {
		parent := added.parent
		grand := parent.parent
		left := parent == grand.left
		uncle := grand.left
		if left {
			uncle = grand.right
		}
		if uncle != nil && uncle.red {
			parent.red, uncle.red, grand.red = false, false, true
			added = grand
			continue
		}
		// A bent path becomes a straight path before rotating the grandparent.
		if (left && added == parent.right) || (!left && added == parent.left) {
			root = rotate(root, parent, left)
			parent = added
		}
		parent.red, grand.red = false, true
		root = rotate(root, grand, !left)
		break
	}
	root.red = false
	return root
}

func compareKey(hash uint32, key string, other *entry) int {
	if int32(hash) < int32(other.hash) {
		return -1
	}
	if int32(hash) > int32(other.hash) {
		return 1
	}
	return compareUTF16(key, other.key)
}

func compareUTF16(a, b string) int {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] < y[i] {
			return -1
		}
		if x[i] > y[i] {
			return 1
		}
	}
	if len(x) < len(y) {
		return -1
	}
	if len(x) > len(y) {
		return 1
	}
	return 0
}

// javaSpread hashes unsigned UTF-16 units with 32-bit overflow, then spreads
// with an unsigned shift. Trees compare this same bit pattern as signed int32.
func javaSpread(s string) uint32 {
	var h uint32
	for _, c := range utf16.Encode([]rune(s)) {
		h = 31*h + uint32(c)
	}
	return h ^ h>>16
}
