package recordlayer

import "fdb.dev/pkg/recordlayer/protoname"

// The protobuf identifier escaping moved to the leaf package
// pkg/recordlayer/protoname so the plan-time descriptor emitter in
// cascades/values can share it — values cannot import this package, which
// already imports values. These aliases keep the historical spelling at the
// call sites; protoname holds the implementation, its doc comment, and its
// Java-defect witnesses.

// InvalidNameError mirrors Java's ProtoUtils.InvalidNameException.
type InvalidNameError = protoname.InvalidNameError

// ToProtoBufCompliantName escapes a user identifier into a protobuf-compliant
// name. See protoname.ToProtoBufCompliantName.
func ToProtoBufCompliantName(name string) (string, error) {
	return protoname.ToProtoBufCompliantName(name)
}

// CheckValidProtoBufCompliantName validates that name is a legal protobuf
// identifier. See protoname.CheckValidProtoBufCompliantName.
func CheckValidProtoBufCompliantName(name string) error {
	return protoname.CheckValidProtoBufCompliantName(name)
}

// ToUserIdentifier reverses ToProtoBufCompliantName. See
// protoname.ToUserIdentifier.
func ToUserIdentifier(protoIdentifier string) string {
	return protoname.ToUserIdentifier(protoIdentifier)
}

// DecodeOnceIfReversible decodes a stored name to its SQL identifier only when
// the result provably re-encodes. See protoname.DecodeOnceIfReversible.
func DecodeOnceIfReversible(stored string) string {
	return protoname.DecodeOnceIfReversible(stored)
}

// SafeDecoderOver returns one decoding policy for a whole rendered output. See
// protoname.SafeDecoderOver.
func SafeDecoderOver(decoded, verbatim []string) func(string) string {
	return protoname.SafeDecoderOver(decoded, verbatim)
}
