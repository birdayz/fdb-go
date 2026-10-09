package plans

import (
	"unsafe"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// quantifierView is a one-element GetQuantifiers result over the plan's own
// field, so the memo's per-comparison reads do not allocate. Its capacity is
// one, so an append copies; a caller must copy before writing an element.
func quantifierView(q *expressions.Quantifier) []expressions.Quantifier {
	return unsafe.Slice(q, 1)
}
