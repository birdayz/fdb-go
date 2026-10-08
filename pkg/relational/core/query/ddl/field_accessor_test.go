package ddl

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// The accessor comparisons the generator makes are three different relations in
// Java, and collapsing any two of them changes which indexes build:
//
//   - FieldPath equality and isPrefixOf use the accessor's own equals, which is
//     asymmetric: a plain ResolvedAccessor compares ordinals only and so equals
//     an annotated one, while an AnnotatedAccessor equals only an annotated one
//     with the same marker (QuantifierValues.java, AnnotatedAccessor.equals,
//     getClass() != other.getClass()).
//   - The trie keys its children in a map by accessor, where a plain accessor
//     hashes by ordinal and an annotated one by ordinal and marker, so as keys
//     they never meet (accessorKeyEqual).
//   - isPrefixOf is called on the PREFIX, so the prefix's accessor is the
//     receiver (prefixMatches).

func plain(ordinal int) fieldAccessor {
	return fieldAccessor{ordinal: ordinal, name: "F", typ: values.NewPrimitiveType(values.TypeCodeLong, true)}
}

func marked(ordinal, marker int) fieldAccessor {
	a := plain(ordinal)
	a.marker = marker
	return a
}

func path(steps ...fieldAccessor) *pathColumn {
	return &pathColumn{steps: steps, typ: steps[len(steps)-1].typ}
}

func TestFieldAccessorEqual_IsAsymmetric(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		receiver fieldAccessor
		other    fieldAccessor
		want     bool
	}{
		{"plain receiver admits a marked accessor of the same column", plain(3), marked(3, 1), true},
		{"marked receiver refuses a plain accessor of the same column", marked(3, 1), plain(3), false},
		{"marked receiver admits the same marker", marked(3, 1), marked(3, 1), true},
		{"marked receiver refuses another unnest of the same array", marked(3, 1), marked(3, 2), false},
		{"plain accessors of one column", plain(3), plain(3), true},
		{"plain receiver refuses another column", plain(3), marked(4, 1), false},
		{"marked receiver refuses another column", marked(3, 1), marked(4, 1), false},
	}
	for _, tc := range cases {
		if got := tc.receiver.equal(tc.other); got != tc.want {
			t.Errorf("%s: %+v.equal(%+v) = %v, want %v", tc.name, tc.receiver, tc.other, got, tc.want)
		}
	}
}

func TestAccessorKeyEqual_IsExactOnOrdinalAndMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b fieldAccessor
		want bool
	}{
		{"plain then marked", plain(3), marked(3, 1), false},
		{"marked then plain", marked(3, 1), plain(3), false},
		{"two unnests of one array", marked(3, 1), marked(3, 2), false},
		{"one unnest", marked(3, 1), marked(3, 1), true},
		{"one plain column", plain(3), plain(3), true},
		{"two plain columns", plain(3), plain(4), false},
	}
	for _, tc := range cases {
		if got := accessorKeyEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: accessorKeyEqual(%+v, %+v) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPrefixMatches_ThePrefixIsTheReceiver(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		prefix    *pathColumn
		candidate *pathColumn
		want      bool
	}{
		{"a plain prefix admits a path through an unnest", path(plain(0), plain(1)), path(marked(0, 1), plain(2)), true},
		{"a prefix through an unnest refuses a plain path", path(marked(0, 1), plain(1)), path(plain(0), plain(2)), false},
		{"a prefix through an unnest refuses another unnest", path(marked(0, 1), plain(1)), path(marked(0, 2), plain(2)), false},
		{"a prefix through an unnest admits the same unnest", path(marked(0, 1), plain(1)), path(marked(0, 1), plain(2)), true},
	}
	for _, tc := range cases {
		vals := []values.Value{tc.prefix, tc.candidate}
		if got := prefixMatches(vals, 0, 1, 1, storageNames{}); got != tc.want {
			t.Errorf("%s: prefixMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// trieShape renders a trie's child keys, depth first, as the accessors the
// generator would render.
func trieShape(n *fieldTrieNode) []fieldAccessor {
	var out []fieldAccessor
	for _, c := range n.children {
		out = append(out, c.accessor)
		out = append(out, trieShape(c.node)...)
	}
	return out
}

func TestComputeTrie_UnnestMarkersKeyChildren(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		vals []values.Value
		// topChildren are the root's child keys, in order.
		topChildren []fieldAccessor
	}{
		{
			// Two unnests of one array are two children: the second path is not
			// under the first's prefix (a marked receiver refuses another marker)
			// and its key does not collide with the first's.
			name:        "two unnests of one array",
			vals:        []values.Value{path(marked(0, 1), plain(1)), path(marked(0, 2), plain(2))},
			topChildren: []fieldAccessor{marked(0, 1), marked(0, 2)},
		},
		{
			// A plain prefix admits the unnested path, so both land in one subtree.
			name:        "plain column then its unnest",
			vals:        []values.Value{path(plain(0), plain(1)), path(marked(0, 1), plain(2))},
			topChildren: []fieldAccessor{plain(0)},
		},
		{
			// An unnested prefix refuses the plain path, which then keys its own
			// child: the keys differ, so there is no overlap.
			name:        "unnest then the plain column",
			vals:        []values.Value{path(marked(0, 1), plain(1)), path(plain(0), plain(2))},
			topChildren: []fieldAccessor{marked(0, 1), plain(0)},
		},
	}
	for _, tc := range cases {
		node, next, err := computeTrieForValues(tc.vals, 0, storageNames{})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if next != len(tc.vals) {
			t.Errorf("%s: consumed %d of %d values", tc.name, next, len(tc.vals))
		}
		if len(node.children) != len(tc.topChildren) {
			t.Errorf("%s: %d top-level children %+v, want %+v", tc.name, len(node.children), trieShape(node), tc.topChildren)
			continue
		}
		for i, c := range node.children {
			if !accessorKeyEqual(c.accessor, tc.topChildren[i]) {
				t.Errorf("%s: child %d is %+v, want %+v", tc.name, i, c.accessor, tc.topChildren[i])
			}
		}
	}
}

// A nested column referenced twice with another column between is Java's
// "multiple disconnected references" refusal; the same shape through one unnest
// marker is the same key and refuses too.
func TestComputeTrie_DisconnectedReferencesRefuse(t *testing.T) {
	t.Parallel()
	for name, vals := range map[string][]values.Value{
		"plain":      {path(plain(0), plain(1)), path(plain(2)), path(plain(0), plain(3))},
		"one unnest": {path(marked(0, 1), plain(1)), path(plain(2)), path(marked(0, 1), plain(3))},
	} {
		_, _, err := computeTrieForValues(vals, 0, storageNames{})
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
			t.Errorf("%s: want the overlap refusal, got %v", name, err)
		}
	}
}

// equalValues compares paths accessor by accessor with the left value's steps
// as the receiver, so it inherits the asymmetry.
func TestEqualValues_PathsInheritTheAsymmetry(t *testing.T) {
	t.Parallel()
	qv := newQuantifierValues()
	plainPath, markedPath := path(plain(0), plain(1)), path(marked(0, 1), plain(1))
	if !qv.equalValues(plainPath, markedPath) {
		t.Error("a plain path must equal the same path through an unnest")
	}
	if qv.equalValues(markedPath, plainPath) {
		t.Error("a path through an unnest must not equal the plain path")
	}
	if qv.equalValues(markedPath, path(marked(0, 2), plain(1))) {
		t.Error("two unnests of one array must not be equal")
	}
	if qv.equalValues(plainPath, path(plain(0))) {
		t.Error("paths of different lengths must not be equal")
	}
}
