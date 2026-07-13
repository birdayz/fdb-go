package expr_test

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// predRow wraps a name->value map as a values.OrdinalRow for tests. RFC-173 made
// the ordinal row the SOLE value-eval context (a Value/PredicateValue no longer
// resolves against a bare map[string]any). GetByName resolves the flat field by
// name; a missing key is SQL NULL (sparse — the pre-cap map-context behavior).
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
	return nil, true
}

var _ values.OrdinalRow = predRow(nil)
