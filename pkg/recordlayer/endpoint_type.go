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
	// EndpointTypePrefixString for prefix-string-based ranges.
	// When used, the tuple is packed and the trailing null byte (string
	// terminator in FDB tuple encoding) is stripped.  For the low
	// endpoint the stripped bytes are used directly.  For the high
	// endpoint the stripped bytes are strinc'd (trailing 0xFF bytes are
	// removed, then the last byte is incremented).
	// Matches Java's EndpointType.PREFIX_STRING.
	EndpointTypePrefixString
)
