package recordlayer

import (
	"fmt"
	"math"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// vectorScanOptionFields lists canonical wire names and their typed destinations.
// The legacy return-vectors spelling is accepted only when reading.
func vectorScanOptionFields(o *VectorIndexScanOptions) []struct {
	name  string
	field any
} {
	return []struct {
		name  string
		field any
	}{
		{"vectorReturnVectors", &o.ReturnVectors},
		{"hnswEfSearch", &o.EfSearch},
		{"guardiannCandidatePoolFactor", &o.GuardiannCandidatePoolFactor},
		{"guardiannSearchMaxClusters", &o.GuardiannSearchMaxClusters},
		{"guardiannSearchMinClustersBeforePruning", &o.GuardiannSearchMinClustersBeforePruning},
		{"guardiannSearchDistanceRatioCutoff", &o.GuardiannSearchDistanceRatioCutoff},
		{"guardiannCentroidEfRingSearch", &o.GuardiannCentroidEfRingSearch},
		{"guardiannCentroidEfOutwardSearch", &o.GuardiannCentroidEfOutwardSearch},
		{"guardiannSearchConcurrency", &o.GuardiannSearchConcurrency},
	}
}

// ToProto writes Java VectorIndexScanOptions, using Integer (not Long) for
// integer options and canonical names. Entries have a deterministic order.
func (o VectorIndexScanOptions) ToProto() (*gen.PVectorIndexScanOptions, error) {
	out := &gen.PVectorIndexScanOptions{}
	for _, slot := range vectorScanOptionFields(&o) {
		isNull, wasPresent := o.wirePresence[slot.name]
		var value any
		var integer *int
		switch field := slot.field.(type) {
		case **bool:
			if *field != nil {
				value = **field
			}
		case **float64:
			if *field != nil {
				value = **field
			}
		case **int:
			integer = *field
		case *int:
			if *field != 0 || (wasPresent && !isNull) {
				integer = field
			}
		}
		if integer != nil {
			if *integer < math.MinInt32 || *integer > math.MaxInt32 {
				return nil, &RecordCoreError{Message: fmt.Sprintf("vector scan option %s exceeds Java Integer range", slot.name)}
			}
			value = int32(*integer)
		}
		if value == nil && !wasPresent {
			continue
		}
		encoded, err := valueToProto(value)
		if err != nil {
			return nil, err
		}
		out.OptionEntries = append(out.OptionEntries, &gen.PVectorIndexScanOptions_POptionEntry{Key: proto.String(slot.name), Value: encoded})
	}
	return out, nil
}

// VectorIndexScanOptionsFromProto accepts canonical and legacy names, rejecting
// duplicate aliases as Java does. Explicit NULL and integer zero retain presence.
// Invalid value types are rejected at this typed Go boundary rather than at
// Java's later getOption Class.cast call.
func VectorIndexScanOptionsFromProto(p *gen.PVectorIndexScanOptions) (VectorIndexScanOptions, error) {
	o := VectorIndexScanOptions{wirePresence: map[string]bool{}}
	if p == nil {
		return VectorIndexScanOptions{}, &RecordCoreError{Message: "missing vector index scan options"}
	}
	fields := map[string]any{}
	for _, slot := range vectorScanOptionFields(&o) {
		fields[slot.name] = slot.field
	}
	for _, entry := range p.OptionEntries {
		name := entry.GetKey()
		if name == "hnswReturnVectors" {
			name = "vectorReturnVectors"
		}
		field, known := fields[name]
		if !known {
			return VectorIndexScanOptions{}, &RecordCoreError{Message: fmt.Sprintf("unknown vector index scan option %q", name)}
		}
		if _, seen := o.wirePresence[name]; seen {
			return VectorIndexScanOptions{}, &RecordCoreError{Message: "vector index scan options set the same option under more than one name", IndexOption: name}
		}
		value, err := valueFromProto(entry.GetValue())
		if err != nil {
			return VectorIndexScanOptions{}, err
		}
		o.wirePresence[name] = value == nil
		if value == nil {
			continue
		}
		valid := false
		switch target := field.(type) {
		case **bool:
			if v, ok := value.(bool); ok {
				*target = &v
				valid = true
			}
		case **float64:
			if v, ok := value.(float64); ok {
				*target = &v
				valid = true
			}
		case **int:
			if v, ok := value.(int32); ok {
				n := int(v)
				*target = &n
				valid = true
			}
		case *int:
			if v, ok := value.(int32); ok {
				*target = int(v)
				valid = true
			}
		}
		if !valid {
			return VectorIndexScanOptions{}, &RecordCoreError{Message: fmt.Sprintf("vector scan option %s has incompatible type %T", name, value)}
		}
	}
	return o, nil
}
