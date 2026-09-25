package recordlayer

import (
	"fmt"
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
//
// The refusal is Java's MetaDataException("incorrect index options", cause): the
// message alone, and the cause behind it (Unwrap). The integer and double options
// parse as Java's VectorOptionKey parses them (Integer::parseInt,
// Double::parseDouble; javaParseInt, javaParseDouble), a value either refuses a
// NumberFormatError with Java's text; a metric name is read as Metric::valueOf
// reads it (VectorOptionKey.java:236), only a Metric constant's name, and any
// other is Enum.valueOf's IllegalArgumentException with its text; a dimension
// count below one is an IllegalArgumentError whose text is Go's own, naming the
// option and value.
func validateVectorIndexOptionsAtBuild(idx *Index) error {
	// A value that parses and is refused: Java's IllegalArgumentException, whose
	// text is Go's own (the option and value), under Java's message.
	bad := func(opt, val string) error {
		return &MetaDataError{Message: "incorrect index options", Cause: &IllegalArgumentError{
			Message: fmt.Sprintf("vector index %s option %s has value %q", idx.Name, opt, val),
		}}
	}

	// Java's getConfig REQUIRES the dimension count and parses it unguarded, so
	// a missing or malformed value is a MetaDataException either way.
	dims, ok := idx.Options[IndexOptionVectorNumDimensions]
	if !ok {
		return &MetaDataError{Message: "need to specify the number of dimensions"}
	}
	n, err := javaParseInt(dims)
	if err != nil {
		return &MetaDataError{Message: "incorrect index options", Cause: err}
	}
	if n <= 0 {
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
		if _, err := javaParseInt(v); err != nil {
			return &MetaDataError{Message: "incorrect index options", Cause: err}
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
		if _, err := javaParseDouble(v); err != nil {
			return &MetaDataError{Message: "incorrect index options", Cause: err}
		}
	}

	// Java reads the metric with Metric::valueOf, which knows the four
	// constants' names and nothing else (not parseHNSWConfig's lower-case
	// aliases, which a plain VECTOR index still takes: DIVERGENCES.md, VECTOR),
	// and throws rather than falling back. parseHNSWConfig's default arm maps
	// anything unrecognised to Euclidean, so without this a misspelled metric
	// silently changes what "nearest" means for every query the index serves.
	if v, present := idx.Options[IndexOptionVectorMetric]; present {
		switch v {
		case "EUCLIDEAN_METRIC", "EUCLIDEAN_SQUARE_METRIC", "COSINE_METRIC", "DOT_PRODUCT_METRIC":
		default:
			return &MetaDataError{Message: "incorrect index options", Cause: &IllegalArgumentError{
				Message: "No enum constant com.apple.foundationdb.linear.Metric." + v,
			}}
		}
	}
	return nil
}
