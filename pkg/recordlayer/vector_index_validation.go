package recordlayer

import (
	"errors"
)

// validateVectorIndexOptionsAtBuild is the option half of Java's VectorIndexValidator
// (VectorIndexMaintainerFactory.java:96-111): VectorIndexHelper.validate, which
// refuses an option given under both its name and its alias (a MetaDataError,
// VectorIndexOptionsHelper.validateNoAliasConflicts) and then parses the HNSW
// configuration as HnswVectorIndexEngine.parseConfig does, Config's checks
// included (readHNSWOptions, without Go's forms); any IllegalArgumentException
// of the parse, a NumberFormatException included, is rethrown as
// MetaDataException("incorrect index options", cause), and a missing dimension
// count is the parse's own MetaDataException. So the windowed index Go builds is
// the one Java builds, and its maintainer (parseHNSWConfig, the same reader)
// reads the numbers Java reads.
//
// SCOPE, stated as what is NOT covered: the engine selector (vectorEngine; Go
// maintains HNSW only) and Java's structure half (validateStructure: a
// KeyWithValueExpression root, no grouping, not unique) are RFC-257 WS-D's, as is
// running the validator for a plain VECTOR index (DIVERGENCES.md, the VECTOR
// entry).
func validateVectorIndexOptionsAtBuild(idx *Index) error {
	if err := hnswAliasConflict(idx); err != nil {
		return err
	}
	if _, err := readHNSWOptions(idx, false); err != nil {
		var iae *IllegalArgumentError
		if errors.As(err, &iae) {
			return &MetaDataError{Message: "incorrect index options", Cause: err}
		}
		return err
	}
	return nil
}
