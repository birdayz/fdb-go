package embedded

import (
	"fdb.dev/pkg/recordlayer/protoname"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// qualifierStrippedLabel returns the SQL label for a field reference whose flat
// output name is `name`: the name with its SOURCE QUALIFIER removed, and only
// that.
//
// THE QUESTION IS WHICH DOT IS A QUALIFIER, AND THE STRING CANNOT ANSWER IT.
// A column declared `"a.b"` is stored as the proto field `a__2b` and comes back
// through ToUserIdentifier spelled `a.b`, so `a.b` and `C.NAME` are the same
// shape to any parser. Splitting the first at its dot reports the label `b`,
// which is a name no engine calls that column: measured against a live JVM,
// Java reports `a.b`, and Go's own STAR expansion already did, so one label
// path was wrong beside a sibling in the same engine that was right.
//
// Java never faces the question — an identifier there is a LIST of parts and
// Identifier.withoutQualifier takes the last PART. Go's equivalent authority at
// this site is the SCHEMA, which is the one star expansion already consults.
//
// TWO SCHEMA FACTS ARE NEEDED, NOT ONE, and the second exists to close a
// collision the first has on its own. The whole name stands as the label when
//
//	(a) the WHOLE name is a column some in-scope descriptor declares, and
//	(b) the split's column half is NOT.
//
// (a) alone says `a.b` is a real column and keeps it. But with `t(a)` and
// `x("T.A")` both in scope, `SELECT t.a FROM t, x` renders `T.A`, which (a)
// also finds — as a column of the OTHER table — and the label came back
// qualified where Java gives `A`. (b) separates them: `A` is declared by `t`,
// so `T.A` really is a qualified reference, while `b` is declared by nobody and
// `a.b` really is one column.
//
// WHAT THIS DOES NOT COVER, from shapes that were run rather than characterised:
//
//   - BOTH facts true at once. A schema with a column `"a.b"` AND some in-scope
//     table with a column `b` makes (b) false, and `SELECT "a.b" FROM dott, other`
//     labels the column `b` again. This is the residual of the collision above,
//     narrowed but not gone, and it is pinned as a test rather than left as a
//     sentence.
//   - NESTED fields. `d.Fields()` is top-level only and a struct's members are
//     not among them, so a nested field declared `"s.k"` is invisible to (a)
//     and its label still splits to `k`.
//   - Anything the descriptors in scope do not describe: a derived table's or
//     CTE's output column is not declared by any of them, so a derived column
//     that happens to carry a dot splits.
//
// THE RIGHT ANSWER IS THE REFERENCE'S OWN ALIAS, and it is not available here.
// A qualifier is a source alias, so a reference that knew which source it read
// would settle this outright — but by the time a projection's columns are
// derived, the reads are re-anchored and every root correlation spells itself
// `_current`. Measured: `X.SUM(X.PLAIN)` reports `alias=_current`, not `X`.
// Carrying source provenance to this boundary is the real fix and is a
// different change.
//
// A THIRD ATTEMPT USED THE REFERENCE'S ACCESSOR LEAF and was wrong for a
// related reason — that leaf is the row's field name, and the row's field name
// is sometimes a qualified datum key. `X.SUM(X.Amount)` is its own leaf, so
// "keep the name when it IS the leaf" kept the qualifier.
func qualifierStrippedLabel(name string, descs []protoreflect.MessageDescriptor) string {
	// parseColRef finds the CANDIDATE dot — the last one at paren depth zero,
	// so a derived aggregate's own dots stay inside its call.
	ref := parseColRef(name)
	if ref.table == "" {
		return name
	}
	if declaresColumn(descs, name) && !declaresColumn(descs, ref.col) {
		return name
	}
	return ref.col
}

// declaresColumn reports whether any of these descriptors declares a top-level
// column with this exact SQL name. Names are compared after un-escaping, so a
// column stored as `a__2b` answers to `a.b`.
func declaresColumn(descs []protoreflect.MessageDescriptor, name string) bool {
	if name == "" {
		return false
	}
	for _, d := range descs {
		if d == nil {
			continue
		}
		fields := d.Fields()
		for i := 0; i < fields.Len(); i++ {
			if protoname.ToUserIdentifier(string(fields.Get(i).Name())) == name {
				return true
			}
		}
	}
	return false
}
