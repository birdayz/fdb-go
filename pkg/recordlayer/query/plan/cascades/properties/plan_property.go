package properties

// ExpressionProperty is a type-safe key for plan property maps.
// Instances are singletons compared by pointer identity — the same
// pattern Java's ExpressionProperty uses.
type ExpressionProperty struct {
	name string
}

func (p *ExpressionProperty) String() string { return p.name }

var (
	PropOrdering        = &ExpressionProperty{name: "ordering"}
	PropDistinctRecords = &ExpressionProperty{name: "distinctRecords"}
	PropStoredRecord    = &ExpressionProperty{name: "storedRecord"}
	PropPrimaryKey      = &ExpressionProperty{name: "primaryKey"}

	AllPlanProperties = []*ExpressionProperty{
		PropOrdering, PropDistinctRecords, PropStoredRecord, PropPrimaryKey,
	}
)

// PropertyMap holds computed property values for a single plan.
type PropertyMap map[*ExpressionProperty]any

// GetBool returns the bool value for the given property, or false if absent.
func (m PropertyMap) GetBool(p *ExpressionProperty) bool {
	v, ok := m[p]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// GetOrdering returns the Ordering value for PropOrdering, or zero Ordering.
func (m PropertyMap) GetOrdering() Ordering {
	v, ok := m[PropOrdering]
	if !ok {
		return Ordering{}
	}
	o, _ := v.(Ordering)
	return o
}
