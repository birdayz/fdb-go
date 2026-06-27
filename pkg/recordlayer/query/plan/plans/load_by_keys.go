package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// KeysSource provides primary keys for RecordQueryLoadByKeysPlan.
// Mirrors Java's RecordQueryLoadByKeysPlan.KeysSource interface.
type KeysSource interface {
	// GetPrimaryKeys returns the list of primary keys to load.
	GetPrimaryKeys() []tuple.Tuple
	// MaxCardinality returns the maximum number of records this
	// source can produce, or -1 if unknown.
	MaxCardinality() int
	// Equals reports equality with another KeysSource.
	Equals(other KeysSource) bool
	// String returns a human-readable label.
	String() string
}

// PrimaryKeysKeySource is a concrete list of primary keys.
// Mirrors Java's PrimaryKeysKeySource inner class.
type PrimaryKeysKeySource struct {
	primaryKeys []tuple.Tuple
}

// NewPrimaryKeysKeySource constructs a key source from a list of
// primary key tuples.
func NewPrimaryKeysKeySource(primaryKeys []tuple.Tuple) *PrimaryKeysKeySource {
	cp := make([]tuple.Tuple, len(primaryKeys))
	copy(cp, primaryKeys)
	return &PrimaryKeysKeySource{primaryKeys: cp}
}

// GetPrimaryKeys returns the key list.
func (s *PrimaryKeysKeySource) GetPrimaryKeys() []tuple.Tuple { return s.primaryKeys }

// MaxCardinality returns the list length.
func (s *PrimaryKeysKeySource) MaxCardinality() int { return len(s.primaryKeys) }

// Equals compares key lists.
func (s *PrimaryKeysKeySource) Equals(other KeysSource) bool {
	o, ok := other.(*PrimaryKeysKeySource)
	if !ok {
		return false
	}
	if len(s.primaryKeys) != len(o.primaryKeys) {
		return false
	}
	for i := range s.primaryKeys {
		if !tupleEquals(s.primaryKeys[i], o.primaryKeys[i]) {
			return false
		}
	}
	return true
}

// String renders the key list.
func (s *PrimaryKeysKeySource) String() string {
	return fmt.Sprintf("%v", s.primaryKeys)
}

// ParameterKeySource gets primary keys from a named parameter.
// Mirrors Java's ParameterKeySource inner class.
type ParameterKeySource struct {
	parameter string
}

// NewParameterKeySource constructs a parameter-bound key source.
func NewParameterKeySource(parameter string) *ParameterKeySource {
	return &ParameterKeySource{parameter: parameter}
}

// GetPrimaryKeys always returns nil — the actual keys come from the
// evaluation context at execution time.
func (s *ParameterKeySource) GetPrimaryKeys() []tuple.Tuple { return nil }

// MaxCardinality returns -1 (unknown).
func (s *ParameterKeySource) MaxCardinality() int { return -1 }

// Equals compares parameter names.
func (s *ParameterKeySource) Equals(other KeysSource) bool {
	o, ok := other.(*ParameterKeySource)
	if !ok {
		return false
	}
	return s.parameter == o.parameter
}

// GetParameter returns the parameter name.
func (s *ParameterKeySource) GetParameter() string { return s.parameter }

// String renders the parameter reference.
func (s *ParameterKeySource) String() string { return "$" + s.parameter }

// RecordQueryLoadByKeysPlan returns records whose primary keys are
// taken from a KeysSource. This is a leaf plan (no children).
// Mirrors Java's RecordQueryLoadByKeysPlan.
type RecordQueryLoadByKeysPlan struct {
	keysSource KeysSource
}

// NewRecordQueryLoadByKeysPlan constructs the plan from a KeysSource.
func NewRecordQueryLoadByKeysPlan(keysSource KeysSource) *RecordQueryLoadByKeysPlan {
	return &RecordQueryLoadByKeysPlan{keysSource: keysSource}
}

// NewRecordQueryLoadByKeysPlanFromKeys constructs the plan from an
// explicit list of primary key tuples.
func NewRecordQueryLoadByKeysPlanFromKeys(primaryKeys []tuple.Tuple) *RecordQueryLoadByKeysPlan {
	return &RecordQueryLoadByKeysPlan{keysSource: NewPrimaryKeysKeySource(primaryKeys)}
}

// NewRecordQueryLoadByKeysPlanFromParameter constructs the plan from
// a named parameter.
func NewRecordQueryLoadByKeysPlanFromParameter(parameter string) *RecordQueryLoadByKeysPlan {
	return &RecordQueryLoadByKeysPlan{keysSource: NewParameterKeySource(parameter)}
}

// GetKeysSource returns the key source.
func (p *RecordQueryLoadByKeysPlan) GetKeysSource() KeysSource { return p.keysSource }

// GetResultType returns UnknownType — the actual type depends on the
// record type loaded at execution time.
func (p *RecordQueryLoadByKeysPlan) GetResultType() values.Type { return values.UnknownType }

// GetChildren returns nil — this is a leaf plan.
func (p *RecordQueryLoadByKeysPlan) GetChildren() []RecordQueryPlan { return nil }

// EqualsWithoutChildren compares the key sources.
func (p *RecordQueryLoadByKeysPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryLoadByKeysPlan)
	if !ok {
		return false
	}
	return p.keysSource.Equals(o.keysSource)
}

// HashCodeWithoutChildren mixes the type discriminator + key source
// string representation.
func (p *RecordQueryLoadByKeysPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("loadbykeysplan|"))
	h.Write([]byte(p.keysSource.String()))
	return h.Sum64()
}

// Explain renders LoadByKeys(source).
func (p *RecordQueryLoadByKeysPlan) Explain() string {
	return fmt.Sprintf("LoadByKeys(%s)", p.keysSource.String())
}

// tupleEquals compares two FDB tuples element-by-element.
func tupleEquals(a, b tuple.Tuple) bool {
	if len(a) != len(b) {
		return false
	}
	// Pack + compare is the canonical equality check for FDB tuples.
	ap := a.Pack()
	bp := b.Pack()
	if len(ap) != len(bp) {
		return false
	}
	for i := range ap {
		if ap[i] != bp[i] {
			return false
		}
	}
	return true
}

var (
	_ RecordQueryPlan = (*RecordQueryLoadByKeysPlan)(nil)
	_ KeysSource      = (*PrimaryKeysKeySource)(nil)
	_ KeysSource      = (*ParameterKeySource)(nil)
)
