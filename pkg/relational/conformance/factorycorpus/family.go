package factorycorpus

import (
	"fmt"
	"sort"
	"strings"
)

// FileExt is the extension of a committed corpus family file. The corpus is
// genuine yamsql grouped one file per feature family (RFC-201 §5.7).
const FileExt = ".yamsql"

// FamilyOf maps a feature vector onto its feature FAMILY — the logical group
// whose scenarios share one committed `.yamsql` file, mirroring the upstream
// corpus's convention of one file per feature area (cte.yamsql holds the CTE
// tests, in-operator-queries.yamsql the IN tests).
//
// The family key is `shape|predicate-class|subquery-class`:
//
//   - the query shape (`single`, `join2.inner`, `join3.comma`, …),
//   - the WHERE tree's top connective with the SORTED SET of its child leaf
//     classes (`and(cmp+colcol)`), or the bare leaf class (`cmp`, `in`) when
//     the predicate is a single leaf,
//   - the correlated sub-construct (`exists`, `scalarsub`, both, or none).
//
// These are the same axes the per-PR stratifier spreads its budget over
// (StratumKey), with the connective refined by its child classes. The
// refinement is what keeps the files readable: the plain top-connective key
// collapses a quarter of the corpus into one `and` bucket (238 scenarios /
// ~950 test stanzas in one file, measured on the 2026-08-01 corpus), while the
// refined key's largest family holds 123 scenarios. The FULL feature vector is
// deliberately not the key — it carries the index set, projection width and
// ordering, so it is nearly unique per scenario and would recreate the
// one-file-per-scenario layout this grouping replaces.
func FamilyOf(featureVector string) string {
	shape := featureComponent(featureVector, "shape")
	if shape == "" {
		shape = "unknown"
	}
	return shape + "|" + predicateClass(featureComponent(featureVector, "where")) + "|" + subqueryClass(featureVector)
}

// featureComponent returns the value of a `key=value` component of a feature
// vector, or "" when the vector has no such component.
//
// Feature vectors are `;`-separated `key=value` components and the set of keys
// is OPEN — a generator that learns a new query construct adds a component
// (`exists=`, `scalarsub=`) without touching the older ones. Reading them
// positionally would therefore be correct only until the next construct lands,
// so every reader here goes by key.
func featureComponent(fv, key string) string {
	for _, part := range strings.Split(fv, ";") {
		if v, ok := strings.CutPrefix(part, key+"="); ok {
			return v
		}
	}
	return ""
}

// subqueryClass names the correlated sub-construct a query carries, because
// `shape` does not: a correlated EXISTS and a scalar subquery both hang off a
// query whose shape reads `single`, and they reach the semi-join and
// scalar-subquery paths that a plain single-table scan never touches. These
// families are SMALL — tens of scenarios in a corpus of thousands — and small
// plus structurally distinct is exactly the combination that must not be
// absorbed into a big file where nobody sees it shrink.
func subqueryClass(fv string) string {
	var parts []string
	if featureComponent(fv, "exists") != "" {
		parts = append(parts, "exists")
	}
	if featureComponent(fv, "scalarsub") != "" {
		parts = append(parts, "scalarsub")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// predicateClass renders a `where=` component's family form: the top node's
// class and, for a connective, the sorted set of its direct children's classes.
func predicateClass(where string) string {
	if where == "" {
		return "none"
	}
	top := where
	if i := strings.IndexAny(top, ".("); i >= 0 {
		top = top[:i]
	}
	open := strings.Index(where, "(")
	if open < 0 || !strings.HasSuffix(where, ")") {
		return top
	}
	inner := where[open+1 : len(where)-1]
	seen := map[string]bool{}
	for _, child := range splitTopLevel(inner) {
		c := child
		if i := strings.IndexAny(c, ".("); i >= 0 {
			c = c[:i]
		}
		if c != "" {
			seen[c] = true
		}
	}
	classes := make([]string, 0, len(seen))
	for c := range seen {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	return top + "(" + strings.Join(classes, "+") + ")"
}

// splitTopLevel splits s on commas that are not nested inside parentheses.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// FamilyFileName renders a family key as its committed file name.
//
// The mapping must be INJECTIVE over the family keys the generator can
// produce, or two families would silently merge into one file; the loader
// re-derives every scenario's family and cross-checks it against the file it
// sits in, which is what makes a collision loud rather than latent.
func FamilyFileName(family string) string {
	segs := strings.Split(family, "|")
	for i, s := range segs {
		segs[i] = sanitizeSegment(s)
	}
	// The subquery segment is "" for the none case after sanitizing? It is
	// not: subqueryClass returns "none" explicitly, so every segment is
	// non-empty and the name never ends in a bare separator.
	return strings.Join(segs, "__") + FileExt
}

func sanitizeSegment(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '+':
			// `+` joins the sorted child classes; mapping it to `-` (which the
			// class tokens themselves never contain — only shape tokens like
			// `left-outer` do) keeps `and(cmp+colcol)` distinct from a
			// hypothetical `and(cmp.colcol)`, whose `.` becomes `_`.
			b.WriteByte('-')
			lastUnderscore = false
		case r == '!':
			// Spelled out, not folded into the `_` bucket: a leading `_` is
			// trimmed, so `!and(...)` and `and(...)` — two families that reach
			// different planner paths — would otherwise collide on one file.
			b.WriteString("not_")
			lastUnderscore = true
		default:
			if !lastUnderscore {
				b.WriteByte('_')
			}
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

// CheckFamilyPlacement verifies that a scenario's feature vector places it in
// the family file it was found in.
func CheckFamilyPlacement(path string, featureVector string) error {
	family := FamilyOf(featureVector)
	want := FamilyFileName(family)
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base != want {
		return fmt.Errorf("scenario with feature vector %q belongs in %s (family %s), found in %s",
			featureVector, want, family, base)
	}
	return nil
}
