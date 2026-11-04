package recordlayer

// IsValueScanCandidateIndexType identifies built-in layouts whose entries support
// a per-record VALUE scan. Unknown and custom layouts fail closed.
func IsValueScanCandidateIndexType(indexType string) bool {
	switch indexType {
	case IndexTypeValue, IndexTypeVersion, IndexTypeRank, IndexTypePermutedMin, IndexTypePermutedMax:
		return true
	default:
		return false
	}
}
