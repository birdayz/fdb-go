// Portions derived from FoundationDB Record Layer (PseudoField.java,
// RecordMetaData.java, MaterializedViewIndexGenerator.java,
// RecordLayerTable.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

// PseudoFieldRowVersion is the name of the row-version pseudo-field —
// Java's PseudoField.ROW_VERSION.getFieldName() (PseudoField.java:36-44:
// the "__" prefix plus the enum constant's name).
//
// When a schema template stores row versions, every planner-facing record
// layout is extended with one trailing field of this name and type
// (Java: RecordMetaData.getPlannerType → Type.Record.addPseudoFields,
// RecordMetaData.java:732-739 / Type.java:2358-2368), unless the record
// descriptor already defines a REAL field of the same name — the
// real-column-wins rule of addPseudoFields' containsKey skip.
const PseudoFieldRowVersion = "__ROW_VERSION"

// IsRowVersionPseudoField reports whether a resolved field is THE
// row-version pseudo-field: name and type must both match, mirroring the
// two-sided check Java applies before emitting VersionKeyExpression.VERSION
// for it (MaterializedViewIndexGenerator.toFieldKeyExpression,
// MaterializedViewIndexGenerator.java:821-823: type equality with
// PseudoField.ROW_VERSION.getType() AND field-name equality).
func IsRowVersionPseudoField(name string, t Type) bool {
	return name == PseudoFieldRowVersion && t != nil && t.Code() == TypeCodeVersion
}

// WithoutPseudoFields is a planner-facing record layout without the trailing
// pseudo-fields Go's layouts carry: the table's own type, Java's
// RecordLayerTable.getType(), which addPseudoFields never extends. It is the
// type an INSERT's rows are admitted against (RecordQueryInsertPlan.insertPlan
// computes its promotions over the table type). A record with no trailing
// pseudo-field is returned as it is.
func WithoutPseudoFields(rt *RecordType) *RecordType {
	if rt == nil || len(rt.Fields) == 0 {
		return rt
	}
	last := rt.Fields[len(rt.Fields)-1]
	if !IsRowVersionPseudoField(last.Name, last.FieldType) {
		return rt
	}
	out := *rt
	out.Fields = append([]Field(nil), rt.Fields[:len(rt.Fields)-1]...)
	return &out
}
