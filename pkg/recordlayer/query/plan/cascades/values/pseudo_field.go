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
