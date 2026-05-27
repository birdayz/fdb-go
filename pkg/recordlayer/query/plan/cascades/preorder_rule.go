package cascades

// preOrderMarker is an embeddable type that marks an ImplementationRule
// as a PreOrder rule. PreOrder rules fire BEFORE child exploration in
// the unified task-stack. They push constraints (orderings, referenced
// fields) to child References.
type preOrderMarker struct{}

func (preOrderMarker) IsPreOrder() bool { return true }
