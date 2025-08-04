package recordlayer

// EndpointType represents how to treat range endpoints in scans
type EndpointType int

const (
	// EndpointTypeTreeStart indicates the start of the entire tree
	EndpointTypeTreeStart EndpointType = iota
	// EndpointTypeTreeEnd indicates the end of the entire tree
	EndpointTypeTreeEnd
	// EndpointTypeRangeInclusive includes the endpoint in the range
	EndpointTypeRangeInclusive
	// EndpointTypeRangeExclusive excludes the endpoint from the range
	EndpointTypeRangeExclusive
	// EndpointTypeContinuation indicates continuation from a previous scan
	EndpointTypeContinuation
	// EndpointTypePrefixRange for prefix-based ranges
	EndpointTypePrefixRange
)