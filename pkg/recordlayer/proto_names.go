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
