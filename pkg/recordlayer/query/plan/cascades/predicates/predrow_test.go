package predicates

import "strings"

// predRow wraps a name->value map as a values.OrdinalRow for tests. RFC-173 made
// the ordinal row the SOLE value-eval context (a FieldValue no longer resolves
// against a bare map[string]any), so tests that used a map context now wrap it
// here — GetByName resolves the (flat) FieldValue's column by name.
type predRow map[string]any

func (r predRow) Get(int) (any, bool) { return nil, false }

func (r predRow) GetByName(name string) (any, bool) {
	if v, ok := r[name]; ok {
		return v, true
	}
	for k, v := range r {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	// A missing key resolves to NULL (present-nil), not a loud unresolved
	// reference: predRow models the SPARSE name-keyed row these tests assume
	// (`name IS NULL` over a row lacking `name` is TRUE, matching SQL NULL for an
	// absent column) — the pre-cap map-context behavior.
	return nil, true
}
