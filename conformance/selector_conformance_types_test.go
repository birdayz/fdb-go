//go:build bazelrunfiles

package conformance_test

import "fdb.dev/pkg/fdbgo/fdb"

type conformanceSelector struct {
	Key     string `json:"key"`
	OrEqual bool   `json:"orEqual"`
	Offset  int    `json:"offset"`
}

type conformanceSelectors struct {
	Begin conformanceSelector `json:"begin"`
	End   conformanceSelector `json:"end"`
}

func conformanceSelectorPair(r fdb.Range) conformanceSelectors {
	begin, end := r.FDBRangeKeySelectors()
	bs, es := begin.FDBKeySelector(), end.FDBKeySelector()
	convert := func(selector fdb.KeySelector) conformanceSelector {
		return conformanceSelector{Key: mappedConformanceBytes(selector.Key.FDBKey()), OrEqual: selector.OrEqual, Offset: selector.Offset}
	}
	return conformanceSelectors{Begin: convert(bs), End: convert(es)}
}
