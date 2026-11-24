// Portions derived from FoundationDB Record Layer (Comparisons.java,
// TextTokenizer.java, TextTokenizerRegistryImpl.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package textcmp

// ArgumentError reports an invalid argument to text comparison evaluation.
type ArgumentError struct {
	Message string
}

func (e *ArgumentError) Error() string { return e.Message }

// JavaRecordCoreException marks ArgumentError as a Java RecordCoreException.
func (*ArgumentError) JavaRecordCoreException() {}

// MetaDataError reports an unknown tokenizer or out-of-range tokenizer version.
type MetaDataError struct {
	Message string
}

func (e *MetaDataError) Error() string { return e.Message }

// JavaRecordCoreException marks MetaDataError as a Java RecordCoreException.
func (*MetaDataError) JavaRecordCoreException() {}

// InvalidComparisonError reports an unsupported TextComparisonType at evaluation.
type InvalidComparisonError struct {
	Message string
}

func (e *InvalidComparisonError) Error() string { return e.Message }

// JavaRecordCoreException marks InvalidComparisonError as a Java RecordCoreException.
func (*InvalidComparisonError) JavaRecordCoreException() {}
