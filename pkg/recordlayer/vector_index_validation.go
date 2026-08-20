package recordlayer

import (
	"fmt"
	"strconv"
)

// validateVectorIndexOptionsAtBuild is the option half of Java's VectorIndexValidator
// (VectorIndexMaintainerFactory.java:96-111), which wraps VectorIndexHelper
// .getConfig and rethrows any IllegalArgumentException as
// MetaDataException("incorrect index options").
//
// Go's parseHNSWConfig is written the other way round: every option is parsed
// with a "if it scans and is in range, use it" guard, so a typo'd hnswM, an
// out-of-range efConstruction and an unknown metric name all fall through to a
// DEFAULT. That is the worst shape for a wire-compatible port. The index still
// builds, still writes, and writes a graph with different connectivity than the
// declaration asked for — silently, and identically to a correctly-declared
// index, so nothing downstream can tell them apart. Java refuses the metadata.
//
// Parsing stays permissive and this runs at BUILD time, which is where Java
// puts it: MetaDataValidator runs the validator once when the metadata is
// assembled, not on every maintainer construction.
//
// SCOPE, stated as what is NOT covered: this is the option half only. Java's
// VectorIndexValidator also calls validateStructure(), which requires the root
// to be a KeyWithValueExpression and forbids a grouping root — and Go accepts
// both of those shapes today, in 36 test sites. Reversing that is a behaviour
// change across the vector surface rather than a validation gap, so it is
// recorded in DIVERGENCES.md rather than closed here.
func validateVectorIndexOptionsAtBuild(idx *Index) error {
	bad := func(opt, val string) error {
		return &MetaDataError{Message: fmt.Sprintf(
			"incorrect index options: vector index %q option %s has value %q, which does "+
				"not parse; Java refuses this metadata rather than substituting a default",
			idx.Name, opt, val)}
	}

	// Java's getConfig REQUIRES the dimension count and parses it unguarded, so
	// a missing or malformed value is a MetaDataException either way.
	dims, ok := idx.Options[IndexOptionVectorNumDimensions]
	if !ok {
		return &MetaDataError{Message: fmt.Sprintf(
			"need to specify the number of dimensions (index %s)", idx.Name)}
	}
	n, err := strconv.Atoi(dims)
	if err != nil || n <= 0 {
		return bad(IndexOptionVectorNumDimensions, dims)
	}

	for _, opt := range []string{
		IndexOptionHNSWM,
		IndexOptionHNSWMMax,
		IndexOptionHNSWMMax0,
		IndexOptionHNSWEfConstruction,
		"hnswEfRepair",
		IndexOptionHNSWStatsThreshold,
		"hnswRaBitQNumExBits",
		IndexOptionHNSWMaxNumConcurrentNodeFetches,
		IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches,
		IndexOptionHNSWMaxNumConcurrentDeleteFromLayer,
	} {
		v, present := idx.Options[opt]
		if !present {
			continue
		}
		if _, err := strconv.Atoi(v); err != nil {
			return bad(opt, v)
		}
	}

	for _, opt := range []string{
		IndexOptionHNSWSampleVectorStatsProbability,
		IndexOptionHNSWMaintainStatsProbability,
	} {
		v, present := idx.Options[opt]
		if !present {
			continue
		}
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return bad(opt, v)
		}
	}

	// Java uses Metric.valueOf, which throws on an unknown name rather than
	// falling back. parseHNSWConfig's default arm maps anything unrecognised to
	// Euclidean, so without this a misspelled metric silently changes what
	// "nearest" means for every query the index serves.
	if v, present := idx.Options[IndexOptionVectorMetric]; present {
		switch v {
		case "COSINE_METRIC", "cosine",
			"DOT_PRODUCT_METRIC", "inner_product",
			"EUCLIDEAN_SQUARE_METRIC",
			"EUCLIDEAN_METRIC", "euclidean":
		default:
			return bad(IndexOptionVectorMetric, v)
		}
	}
	return nil
}
