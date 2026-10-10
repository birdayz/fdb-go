// Portions derived from FoundationDB Record Layer (
// ScalarTranslationVisitor.java, PrimaryKeyProperty.java),
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// translatePrimaryKeyToValues translates a common primary-key KeyExpression into
// a flat []values.Value that encodes STRUCTURE — record-type-key prefixes
// (RecordTypeValue) and completely resolved ordinal paths — the Go analog of
// Java's ScalarTranslationVisitor.translateKeyExpression used by
// PrimaryKeyProperty.visitIndexPlan (RFC-189 B3).
//
// PK identity must compare by STRUCTURE, not by bare column names: Field("ID")
// and Concat(RecordTypeKey(), Field("ID")) share the flat name list ["ID"] but
// are DIFFERENT primary keys, and conflating them lets ImplementDistinctUnionRule
// dedup two union legs that must both survive (dropped rows — the exact hazard
// the by-name M5 was reverted for). The structural translation makes them
// unequal under values.ValuesStructurallyEqual.
//
// Returns nil if any component is not translatable (fan-out, version, function,
// or any unrecognized key), or if flowedType cannot resolve the complete path —
// fail-safe: the property then abstains, disabling the dedup optimization rather
// than risking a wrong one. Field names are request metadata only; every
// published FieldValue is rooted in one exact QOV and carries the ordinal path
// resolved against flowedType.
// normalizeName maps a raw protobuf field name into the caller's namespace (the
// SQL layer uppercases; a nil transform means identity). The translated field
// names MUST live in the SAME namespace as the plan's ordering/column values or
// values.ValuesStructurallyEqual never matches and the dedup silently never
// fires (the field-casing mismatch that made the structural common-PK inert).
func translatePrimaryKeyToValues(
	pk recordlayer.KeyExpression,
	normalizeName func(string) string,
	flowedType values.Type,
) []values.Value {
	if pk == nil {
		return nil
	}
	if normalizeName == nil {
		normalizeName = func(s string) string { return s }
	}
	normalized := recordlayer.NormalizeKeyForPositions(pk)
	if len(normalized) == 0 {
		return nil
	}
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("__primary_key_metadata"),
		flowedType,
	)
	if err != nil {
		return nil
	}
	result := make([]values.Value, 0, len(normalized))
	for _, ke := range normalized {
		v := translateKeyComponent(ke, root, normalizeName)
		if v == nil {
			return nil
		}
		result = append(result, v)
	}
	return result
}

// translateKeyComponent maps a single normalized key-position expression to a
// structure-encoding Value over `base` (nil = the record root). Returns nil for
// any component whose scan identity is not a plain structural field path.
func translateKeyComponent(ke recordlayer.KeyExpression, base values.Value, normalizeName func(string) string) values.Value {
	switch e := ke.(type) {
	case *recordlayer.FieldKeyExpression:
		if e.FanType() != recordlayer.FanTypeNone {
			return nil
		}
		request, err := values.FieldByName(normalizeName(e.FieldName()))
		if err != nil {
			return nil
		}
		resolved, err := values.ResolveFieldAccess(base, []values.FieldRequest{request})
		if err != nil {
			return nil
		}
		return resolved
	case *recordlayer.RecordTypeKeyExpression:
		return values.NewRecordTypeValue(base)
	case *recordlayer.NestingKeyExpression:
		if e.FanType() != recordlayer.FanTypeNone {
			return nil
		}
		request, err := values.FieldByName(normalizeName(e.ParentField()))
		if err != nil {
			return nil
		}
		nestedBase, err := values.ResolveFieldAccess(base, []values.FieldRequest{request})
		if err != nil {
			return nil
		}
		return translateKeyComponent(e.Child(), nestedBase, normalizeName)
	default:
		return nil
	}
}
