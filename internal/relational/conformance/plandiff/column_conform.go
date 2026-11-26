package plandiff

import (
	"fmt"
	"regexp"
)

// anonColRe matches fdb-relational's synthetic anonymous-projection labels
// ("_0", "_1", ...). Java assigns these to projections it declines to name —
// unaliased aggregates, constants, and computed expressions — in declaration
// order.
var anonColRe = regexp.MustCompile(`^_\d+$`)

// IsAnonymousColumnName reports whether name is a synthetic anonymous-projection
// label assigned by Java fdb-relational ("_0", "_1", ...).
//
// Go labels the same projections descriptively (e.g. "COUNT(*)", "SUM(VAL)").
// A column label is read-side result-set metadata, NOT wire format, so a more
// useful Go label is an allowed read-side improvement rather than a divergence
// (see CLAUDE.md "wire compat is the hard line; query reach is not"). The
// conformance harness therefore accepts Go's label wherever Java declined to
// name the column — but ONLY there; every other axis (arity, types, and
// explicitly-named columns) is still asserted exactly.
func IsAnonymousColumnName(name string) bool {
	return anonColRe.MatchString(name)
}

// ConformColumns reports whether Go's result-set column metadata conforms to
// Java's. The contract is deliberately tight so the relaxation can't mask a
// real regression:
//
//   - arity must match exactly;
//   - each column's TYPE must match exactly;
//   - each column's NAME must match exactly, EXCEPT where Java assigned a
//     synthetic anonymous label (IsAnonymousColumnName) — there Go's
//     descriptive label is accepted, provided it is non-empty.
//
// Returns ("", true) when Go conforms, or a human-readable mismatch detail and
// false otherwise.
func ConformColumns(goCols, javaCols []Column) (string, bool) {
	if len(goCols) != len(javaCols) {
		return fmt.Sprintf("arity: go=%d java=%d (go=%v java=%v)",
			len(goCols), len(javaCols), columnStrings(goCols), columnStrings(javaCols)), false
	}
	for i := range javaCols {
		if goCols[i].Type != javaCols[i].Type {
			return fmt.Sprintf("col %d %q: type go=%q java=%q",
				i, javaCols[i].Name, goCols[i].Type, javaCols[i].Type), false
		}
		if IsAnonymousColumnName(javaCols[i].Name) {
			// Java declined to name this projection; accept Go's descriptive
			// label, but it must still be a stable, non-empty name.
			if goCols[i].Name == "" {
				return fmt.Sprintf("col %d: java is anonymous %q but go label is empty",
					i, javaCols[i].Name), false
			}
			continue
		}
		if goCols[i].Name != javaCols[i].Name {
			return fmt.Sprintf("col %d: name go=%q java=%q", i, goCols[i].Name, javaCols[i].Name), false
		}
	}
	return "", true
}

func columnStrings(cs []Column) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name + ":" + c.Type
	}
	return out
}
