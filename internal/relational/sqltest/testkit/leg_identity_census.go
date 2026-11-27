package testkit

import (
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
	"fdb.dev/internal/testinstruments/querycensus"
)

// LegIdentityCounts summarizes leg-identity comparisons at one census site.
type LegIdentityCounts struct {
	Total           int64
	ExactEqual      int64
	FoldOnlyEqual   int64
	Neither         int64
	FoldOnlySamples []string
}

// LegIdentityLabel names a leg-identity census site.
type LegIdentityLabel string

func (l LegIdentityLabel) String() string { return string(l) }

// LegIdentitySites returns every leg-identity census site.
func LegIdentitySites() []values.LegIdentitySite {
	return querycensus.LegIdentitySites()
}

// LegIdentityCensusOf returns the snapshot counts for site.
func LegIdentityCensusOf(site values.LegIdentitySite) LegIdentityCounts {
	c := querycensus.LegIdentityCensusOf(site)
	return LegIdentityCounts{
		Total:           c.Total,
		ExactEqual:      c.ExactEqual,
		FoldOnlyEqual:   c.FoldOnlyEqual,
		Neither:         c.Neither,
		FoldOnlySamples: c.FoldOnlySamples,
	}
}

// LegIdentitySiteLabel returns the display label for site.
func LegIdentitySiteLabel(site values.LegIdentitySite) LegIdentityLabel {
	return LegIdentityLabel(querycensus.LegIdentitySiteLabel(site).String())
}
