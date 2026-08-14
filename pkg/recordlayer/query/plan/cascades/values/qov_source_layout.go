package values

// qovRecordLayout is a private immutable snapshot of physical source-window
// metadata that accompanied a QOV's flowed record type. It deliberately lives
// beside, not inside, exactType: RecordType.Legs is execution layout and must
// not enter semantic type equality/hash or leak through QOV.Type().
type qovRecordLayout struct {
	legs   []RecordTypeLeg
	fields []*qovRecordLayout
}

func snapshotQOVRecordLayout(typ Type) *qovRecordLayout {
	record, ok := typ.(*RecordType)
	if !ok || record == nil {
		return nil
	}
	return snapshotQOVRecordLayoutOf(record, make(map[*RecordType]bool))
}

func snapshotQOVRecordLayoutOf(record *RecordType, active map[*RecordType]bool) *qovRecordLayout {
	if record == nil || active[record] {
		return nil
	}
	active[record] = true
	defer delete(active, record)

	layout := &qovRecordLayout{
		legs:   append([]RecordTypeLeg(nil), record.Legs...),
		fields: make([]*qovRecordLayout, len(record.Fields)),
	}
	for i := range record.Fields {
		if nested, ok := record.Fields[i].FieldType.(*RecordType); ok {
			layout.fields[i] = snapshotQOVRecordLayoutOf(nested, active)
		}
	}
	if len(layout.legs) == 0 {
		hasNested := false
		for i := range layout.fields {
			hasNested = hasNested || layout.fields[i] != nil
		}
		if !hasNested {
			return nil
		}
	}
	return layout
}

// physicalFlowedRecordType returns a fresh semantic type copy with the private
// physical source layout restored for purpose-specific migration code. It is
// never returned by QOV.Type/FlowedType and therefore cannot make semantic
// identity depend on layout.
func (q *quantifiedObjectValue) physicalFlowedRecordType() *RecordType {
	if q == nil || q.flowed == nil || q.flowed.code != TypeCodeRecord || q.flowed.anyRecord {
		return nil
	}
	record, _ := q.flowed.thaw().(*RecordType)
	restoreQOVRecordLayout(record, q.sourceLayout)
	return record
}

func restoreQOVRecordLayout(record *RecordType, layout *qovRecordLayout) {
	if record == nil || layout == nil {
		return
	}
	record.Legs = append([]RecordTypeLeg(nil), layout.legs...)
	for i := range record.Fields {
		if i >= len(layout.fields) || layout.fields[i] == nil {
			continue
		}
		if nested, ok := record.Fields[i].FieldType.(*RecordType); ok {
			restoreQOVRecordLayout(nested, layout.fields[i])
		}
	}
}
