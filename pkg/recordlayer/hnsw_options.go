package recordlayer

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/rabitq"
)

// hnswOptionAliases are the read aliases of the options both of Java's vector
// engines share (VectorIndexOptionKeys.java:59-73): the canonical name is the
// hnsw* one Java writes, and the engine-neutral vector* name is read when the
// canonical one is absent. The HNSW-only options have no alias.
var hnswOptionAliases = map[string]string{
	IndexOptionVectorMetric:                     "vectorMetric",
	IndexOptionVectorNumDimensions:              "vectorNumDimensions",
	IndexOptionHNSWSampleVectorStatsProbability: "vectorSampleVectorStatsProbability",
	IndexOptionHNSWMaintainStatsProbability:     "vectorMaintainStatsProbability",
	IndexOptionHNSWStatsThreshold:               "vectorStatsThreshold",
	IndexOptionHNSWUseRaBitQ:                    "vectorUseRaBitQ",
	IndexOptionHNSWRaBitQNumExBits:              "vectorRaBitQNumExBits",
}

// hnswAliasedOptions is the options with an alias, in the order of Java's
// VectorIndexOptionKeys.ALL (:172-175), which validateNoAliasConflicts walks.
var hnswAliasedOptions = []string{
	IndexOptionVectorMetric, IndexOptionVectorNumDimensions, IndexOptionHNSWSampleVectorStatsProbability,
	IndexOptionHNSWMaintainStatsProbability, IndexOptionHNSWStatsThreshold, IndexOptionHNSWUseRaBitQ,
	IndexOptionHNSWRaBitQNumExBits,
}

// hnswOptionValue is VectorOptionKey.read (VectorOptionKey.java:145-152): the
// canonical name's value, else its alias's.
func hnswOptionValue(index *Index, canonical string) (string, bool) {
	if v, ok := index.Options[canonical]; ok {
		return v, true
	}
	if alias, ok := hnswOptionAliases[canonical]; ok {
		v, ok := index.Options[alias]
		return v, ok
	}
	return "", false
}

// VectorIndexMetric is the metric a vector index's maintainer builds it with,
// read by the maintainer's own metric reader: hnswMetric (the plain index's Go
// forms admitted, as its maintainer reads them) for an HNSW VECTOR index, and
// spfreshMetric for an SPFresh index. A value the maintainer refuses is an
// error. The planner reads a vector index's metric through it (Java's
// VectorIndexExpansionVisitor reads the engine's parsed Metric), so a
// candidate's metric is always the one the index is maintained with.
func VectorIndexMetric(index *Index) (VectorMetric, error) {
	switch index.Type {
	case IndexTypeVectorSPFresh:
		return spfreshMetric(index)
	case IndexTypeVector:
		name, err := hnswMetric(index, true)
		if err != nil {
			return VectorMetricEuclidean, err
		}
		return vectorMetricNamed(name), nil
	}
	return VectorMetricEuclidean, &MetaDataError{Message: fmt.Sprintf("index %q of type %q is not a vector index", index.Name, index.Type)}
}

// hnswMetric is an HNSW index's metric as Java's parseConfig reads it: the
// Metric constant name its metric option names, found as
// VectorIndexOptionKeys.METRIC.read finds it (hnswMetric, else its alias
// vectorMetric), read by Metric.valueOf with goForms (javaMetricName), and
// EUCLIDEAN_METRIC when neither is set. The one metric reader of the
// maintainer (readHNSWOptions) and the planner (VectorIndexMetric).
func hnswMetric(index *Index, goForms bool) (string, error) {
	v, ok := hnswOptionValue(index, IndexOptionVectorMetric)
	if !ok {
		return "EUCLIDEAN_METRIC", nil
	}
	return javaMetricName(v, goForms)
}

// hnswAliasConflict is VectorIndexOptionsHelper.validateNoAliasConflicts, the
// first step of Java's VectorIndexHelper.validate: an option set under both its
// canonical name and its alias is refused, even with equal values.
func hnswAliasConflict(index *Index) error {
	for _, canonical := range hnswAliasedOptions {
		_, a := index.Options[canonical]
		_, b := index.Options[hnswOptionAliases[canonical]]
		if a && b {
			return &MetaDataError{Message: "vector index option specified under more than one name"}
		}
	}
	return nil
}

// hnswOptions is an index's HNSW configuration as Java's
// HnswVectorIndexEngine.parseConfig reads it (HnswVectorIndexEngine.java:
// 186-206): config without its quantizer, the metric's Metric constant name,
// and the RaBitQ switch and extra-bit count the config carries.
type hnswOptions struct {
	config          HNSWConfig
	metric          string
	useRaBitQ       bool
	raBitQNumExBits int
}

// readHNSWOptions reads an index's HNSW options as Java's parseConfig does:
// in its order, each under its canonical name or its alias, with Java's
// parsers (Integer.parseInt, Double.parseDouble, Boolean.parseBoolean, which
// never refuses, and Metric.valueOf), an absent option taking Config's
// default, and then Config's constructor checks (Config.java:93-120). A value
// that does not parse is a NumberFormatError, and a metric no Metric constant
// names or a configuration the checks refuse an IllegalArgumentError, each
// with Java's text; a missing dimension count is Java's MetaDataError "need
// to specify the number of dimensions" (VectorIndexOptionsHelper.
// getNumDimensions).
//
// goForms admits two Go conveniences for a plain VECTOR index, whose
// build-time validation is RFC-257 WS-D's (DIVERGENCES.md, the VECTOR entry):
// the lower-case metric names cosine, inner_product and euclidean, and 128
// dimensions when none are given. The windowed validator reads with it false.
func readHNSWOptions(index *Index, goForms bool) (hnswOptions, error) {
	o := hnswOptions{metric: "EUCLIDEAN_METRIC", raBitQNumExBits: 4}
	name, err := hnswMetric(index, goForms)
	if err != nil {
		return o, err
	}
	o.metric = name
	dims := 128
	if v, ok := hnswOptionValue(index, IndexOptionVectorNumDimensions); ok {
		n, err := javaParseInt(v)
		if err != nil {
			return o, err
		}
		dims = int(n)
	} else if !goForms {
		return o, &MetaDataError{Message: "need to specify the number of dimensions"}
	}
	c := DefaultHNSWConfig(dims)
	c.Metric = vectorMetricNamed(o.metric)
	intOpt := func(name string, set *int) {
		if v, ok := hnswOptionValue(index, name); ok && err == nil {
			var n int32
			if n, err = javaParseInt(v); err == nil {
				*set = int(n)
			}
		}
	}
	floatOpt := func(name string, set *float64) {
		if v, ok := hnswOptionValue(index, name); ok && err == nil {
			var f float64
			if f, err = javaParseDouble(v); err == nil {
				*set = f
			}
		}
	}
	boolOpt := func(name string, set *bool) {
		if v, ok := hnswOptionValue(index, name); ok && err == nil {
			*set = strings.EqualFold(v, "true")
		}
	}
	boolOpt(IndexOptionHNSWUseInlining, &c.UseInlining)
	intOpt(IndexOptionHNSWM, &c.M)
	intOpt(IndexOptionHNSWMMax, &c.MMax)
	intOpt(IndexOptionHNSWMMax0, &c.MMax0)
	intOpt(IndexOptionHNSWEfConstruction, &c.EfConstruction)
	intOpt(IndexOptionHNSWEfRepair, &c.EfRepair)
	boolOpt(IndexOptionVectorExtendCandidates, &c.ExtendCandidates)
	boolOpt(IndexOptionVectorKeepPrunedConnections, &c.KeepPrunedConnections)
	floatOpt(IndexOptionHNSWSampleVectorStatsProbability, &c.SampleVectorStatsProbability)
	floatOpt(IndexOptionHNSWMaintainStatsProbability, &c.MaintainStatsProbability)
	intOpt(IndexOptionHNSWStatsThreshold, &c.StatsThreshold)
	boolOpt(IndexOptionHNSWUseRaBitQ, &o.useRaBitQ)
	intOpt(IndexOptionHNSWRaBitQNumExBits, &o.raBitQNumExBits)
	intOpt(IndexOptionHNSWMaxNumConcurrentNodeFetches, &c.MaxNumConcurrentNodeFetches)
	intOpt(IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches, &c.MaxNumConcurrentNeighborhoodFetches)
	intOpt(IndexOptionHNSWMaxNumConcurrentDeleteFromLayer, &c.MaxNumConcurrentDeleteFromLayer)
	if err != nil {
		return o, err
	}
	o.config = c
	return o, hnswConfigChecks(c, o.useRaBitQ, o.raBitQNumExBits)
}

// javaMetricName is Metric.valueOf over v: one of the four constants' names,
// or, with goForms, a Go convenience name mapped to one.
func javaMetricName(v string, goForms bool) (string, error) {
	switch v {
	case "EUCLIDEAN_METRIC", "EUCLIDEAN_SQUARE_METRIC", "COSINE_METRIC", "DOT_PRODUCT_METRIC":
		return v, nil
	}
	if goForms {
		switch v {
		case "cosine":
			return "COSINE_METRIC", nil
		case "inner_product":
			return "DOT_PRODUCT_METRIC", nil
		case "euclidean":
			return "EUCLIDEAN_METRIC", nil
		}
	}
	return "", &IllegalArgumentError{Message: "No enum constant com.apple.foundationdb.linear.Metric." + v}
}

func vectorMetricNamed(name string) VectorMetric {
	switch name {
	case "COSINE_METRIC":
		return VectorMetricCosine
	case "DOT_PRODUCT_METRIC":
		return VectorMetricInnerProduct
	case "EUCLIDEAN_SQUARE_METRIC":
		return VectorMetricEuclideanSquare
	}
	return VectorMetricEuclidean
}

// hnswConfigChecks is Java's Config constructor (Config.java:93-120), in its
// order and with its texts, each an IllegalArgumentError (Guava's
// Preconditions.checkArgument).
func hnswConfigChecks(c HNSWConfig, useRaBitQ bool, raBitQNumExBits int) error {
	for _, check := range []struct {
		ok   bool
		text string
	}{
		{c.NumDimensions >= 1, "numDimensions must be (1, MAX_INT]"},
		{c.M >= 4 && c.M <= 200, "m must be [4, 200]"},
		{c.MMax >= 4 && c.MMax <= 200, "mMax must be [4, 200]"},
		{c.MMax0 >= 4 && c.MMax0 <= 300, "mMax0 must be [4, 300]"},
		{c.M <= c.MMax, "m must be less than or equal to mMax"},
		{c.MMax <= c.MMax0, "mMax must be less than or equal to mMax0"},
		{c.EfConstruction >= 100 && c.EfConstruction <= 400, "efConstruction must be [100, 400]"},
		{c.EfRepair >= c.M && c.EfRepair <= 400, "efRepair must be [m, 400]"},
		{!useRaBitQ || c.SampleVectorStatsProbability > 0 && c.SampleVectorStatsProbability <= 1, "sampleVectorStatsProbability out of range"},
		{!useRaBitQ || c.MaintainStatsProbability > 0 && c.MaintainStatsProbability <= 1, "maintainStatsProbability out of range"},
		{!useRaBitQ || c.StatsThreshold > 10, "statThreshold out of range"},
		{!useRaBitQ || raBitQNumExBits > 0 && raBitQNumExBits < 16, "raBitQNumExBits out of range"},
		{c.MaxNumConcurrentNodeFetches > 0 && c.MaxNumConcurrentNodeFetches <= 64, "maxNumConcurrentNodeFetches must be (0, 64]"},
		{c.MaxNumConcurrentNeighborhoodFetches > 0 && c.MaxNumConcurrentNeighborhoodFetches <= 20, "maxNumConcurrentNeighborhoodFetches must be (0, 20]"},
		{c.MaxNumConcurrentDeleteFromLayer > 0 && c.MaxNumConcurrentDeleteFromLayer <= 10, "maxNumConcurrentDeleteFromLayer must be (0, 10]"},
	} {
		if !check.ok {
			return &IllegalArgumentError{Message: check.text}
		}
	}
	return nil
}

// parseHNSWConfig is the configuration the VECTOR maintainer builds its graph
// with: readHNSWOptions with Go's forms, and the RaBitQ quantizer when it is
// enabled. Java's Config admits 1 to 15 extra bits and its RaBitQuantizer 1 to 8
// (RaBitQuantizer.java:76, TIGHT_START's length); Java constructs the quantizer
// only when an operation first quantizes, so a count of 9 to 15 is refused
// there (hnswGraph.raBitQuantizerAdmits), never here: the index is built, and
// serves every operation that does not quantize, as in Java.
func parseHNSWConfig(index *Index) (HNSWConfig, error) {
	o, err := readHNSWOptions(index, true)
	if err != nil {
		return HNSWConfig{}, err
	}
	if o.useRaBitQ {
		o.config.Quantizer = rabitq.NewQuantizer(rabitq.Metric(o.config.Metric), o.raBitQNumExBits)
	}
	return o.config, nil
}
