package cascades

// InSource is the abstraction for a source of values used in IN-join
// and IN-union plans. Each source provides a binding name and can
// produce a list of values at execution time.
//
// Ports Java's InSource hierarchy: InValuesSource, InParameterSource,
// InComparandSource, and their sorted variants.
type InSource interface {
	GetBindingName() string
	IsSorted() bool
	IsReverse() bool
}

// InValuesSource provides an explicit list of values.
type InValuesSource struct {
	bindingName string
	values      []any
}

func NewInValuesSource(bindingName string, vals []any) *InValuesSource {
	copied := make([]any, len(vals))
	copy(copied, vals)
	return &InValuesSource{bindingName: bindingName, values: copied}
}

func (s *InValuesSource) GetBindingName() string { return s.bindingName }
func (s *InValuesSource) GetValues() []any       { return s.values }
func (s *InValuesSource) IsSorted() bool         { return false }
func (s *InValuesSource) IsReverse() bool        { return false }

// SortedInValuesSource is like InValuesSource but indicates the values
// are sorted (ascending or descending).
type SortedInValuesSource struct {
	bindingName string
	values      []any
	reverse     bool
}

func NewSortedInValuesSource(bindingName string, vals []any, reverse bool) *SortedInValuesSource {
	copied := make([]any, len(vals))
	copy(copied, vals)
	return &SortedInValuesSource{bindingName: bindingName, values: copied, reverse: reverse}
}

func (s *SortedInValuesSource) GetBindingName() string { return s.bindingName }
func (s *SortedInValuesSource) GetValues() []any       { return s.values }
func (s *SortedInValuesSource) IsSorted() bool         { return true }
func (s *SortedInValuesSource) IsReverse() bool        { return s.reverse }

// InParameterSource provides values from a named parameter or correlation.
type InParameterSource struct {
	bindingName   string
	parameterName string
}

func NewInParameterSource(bindingName, parameterName string) *InParameterSource {
	return &InParameterSource{bindingName: bindingName, parameterName: parameterName}
}

func (s *InParameterSource) GetBindingName() string   { return s.bindingName }
func (s *InParameterSource) GetParameterName() string { return s.parameterName }
func (s *InParameterSource) IsSorted() bool           { return false }
func (s *InParameterSource) IsReverse() bool          { return false }

// SortedInParameterSource is like InParameterSource but sorted.
type SortedInParameterSource struct {
	bindingName   string
	parameterName string
	reverse       bool
}

func NewSortedInParameterSource(bindingName, parameterName string, reverse bool) *SortedInParameterSource {
	return &SortedInParameterSource{bindingName: bindingName, parameterName: parameterName, reverse: reverse}
}

func (s *SortedInParameterSource) GetBindingName() string   { return s.bindingName }
func (s *SortedInParameterSource) GetParameterName() string { return s.parameterName }
func (s *SortedInParameterSource) IsSorted() bool           { return true }
func (s *SortedInParameterSource) IsReverse() bool          { return s.reverse }
