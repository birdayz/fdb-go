// Portions derived from FoundationDB Record Layer (ProtoUtils.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"errors"

	"fdb.dev/pkg/recordlayer/protoname"
)

// The protobuf identifier escaping moved to the leaf package
// pkg/recordlayer/protoname so the plan-time descriptor emitter in
// cascades/values can share it — values cannot import this package, which
// already imports values. This package owns InvalidNameError and delegates the
// escaping implementation to protoname, which holds the doc comment and its
// Java-defect witnesses.

// InvalidNameError mirrors Java's ProtoUtils.InvalidNameException: the
// identifier cannot be turned into (or is not) a protobuf-compliant name.
type InvalidNameError struct {
	Message string
}

func (e *InvalidNameError) Error() string { return e.Message }

func (*InvalidNameError) JavaRecordCoreException() {}

func convertInvalidNameError(err error) error {
	if err == nil {
		return nil
	}
	var ine *protoname.InvalidNameError
	if errors.As(err, &ine) {
		return &boundaryWrappedError{
			outer: err,
			inner: &InvalidNameError{Message: ine.Message},
		}
	}
	return err
}

// ToProtoBufCompliantName escapes a user identifier into a protobuf-compliant
// name. See protoname.ToProtoBufCompliantName.
func ToProtoBufCompliantName(name string) (string, error) {
	escaped, err := protoname.ToProtoBufCompliantName(name)
	return escaped, convertInvalidNameError(err)
}

// checkValidProtoBufCompliantName validates that name is a legal protobuf
// identifier. See protoname.CheckValidProtoBufCompliantName.
func checkValidProtoBufCompliantName(name string) error {
	return convertInvalidNameError(protoname.CheckValidProtoBufCompliantName(name))
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
