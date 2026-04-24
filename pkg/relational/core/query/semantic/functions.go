package semantic

import "fmt"

// FunctionSpec describes a SQL-visible function the analyzer can
// resolve. The seed differentiates scalar vs aggregate functions
// since rule-matching / plan-building treats them differently.
type FunctionSpec struct {
	// Name is the canonical, case-folded function name (e.g. "COUNT").
	Name string
	// Kind classifies the function — scalar or aggregate (window
	// functions come later).
	Kind FunctionKind
	// MinArgs / MaxArgs bound accepted arity. A MaxArgs of -1 means
	// no upper bound (variadic).
	MinArgs int
	MaxArgs int
	// AllowsStar reports whether the function accepts `*` as its
	// argument (currently only COUNT does).
	AllowsStar bool
	// AllowsDistinct reports whether the function accepts a leading
	// DISTINCT modifier — e.g. `COUNT(DISTINCT col)`. All aggregates
	// in the SQL standard accept DISTINCT; the flag exists here so
	// future scalar extensions can opt out.
	AllowsDistinct bool
}

// FunctionKind enumerates the classes of function the analyzer
// supports.
//
// Values are assigned explicitly (not via `iota`) so inserting a
// new kind between existing ones doesn't renumber anything —
// future serialized-plan formats can assume these values are
// stable.
type FunctionKind int

const (
	// FunctionScalar: per-row function (UPPER, LOWER, ABS, etc.).
	FunctionScalar FunctionKind = 1
	// FunctionAggregate: spans multiple rows (COUNT, SUM, MIN, MAX, AVG).
	FunctionAggregate FunctionKind = 2
	// Add new kinds with the next unused integer — do NOT renumber
	// existing values.
)

// String returns the kind as a debug-friendly string.
func (k FunctionKind) String() string {
	switch k {
	case FunctionScalar:
		return "scalar"
	case FunctionAggregate:
		return "aggregate"
	}
	return "?"
}

// FunctionCatalog holds the set of functions the analyzer recognizes.
// Seed ships the core SQL aggregates; scalar function catalogues
// come as the embedded engine's scalar-function library gets ported.
type FunctionCatalog struct {
	// byName is keyed by case-folded function name. Using a map
	// keeps lookup O(1) and rejects duplicate registrations at
	// construction time.
	byName map[string]FunctionSpec
}

// NewFunctionCatalog builds an empty catalog. Use RegisterDefaults
// or Register to populate.
func NewFunctionCatalog() *FunctionCatalog {
	return &FunctionCatalog{byName: map[string]FunctionSpec{}}
}

// Register adds a FunctionSpec. Returns an error on duplicate name;
// caller can ignore when registering a known stable set or bubble
// up when the registry is built from user extensions.
func (c *FunctionCatalog) Register(spec FunctionSpec) error {
	key := NormalizeString(spec.Name, false)
	if _, dup := c.byName[key]; dup {
		return fmt.Errorf("function %s already registered", spec.Name)
	}
	c.byName[key] = FunctionSpec{
		Name:           key,
		Kind:           spec.Kind,
		MinArgs:        spec.MinArgs,
		MaxArgs:        spec.MaxArgs,
		AllowsStar:     spec.AllowsStar,
		AllowsDistinct: spec.AllowsDistinct,
	}
	return nil
}

// Lookup returns the FunctionSpec for name (case-insensitive), or
// (_, false) when not registered.
func (c *FunctionCatalog) Lookup(name Identifier) (FunctionSpec, bool) {
	spec, ok := c.byName[name.Name()]
	return spec, ok
}

// Contains reports whether a function with the given identifier is
// registered. Equivalent to Lookup's second return.
func (c *FunctionCatalog) Contains(name Identifier) bool {
	_, ok := c.byName[name.Name()]
	return ok
}

// RegisterDefaults populates the catalog with the standard SQL
// aggregate functions. Scalar functions are not seeded — they
// come from the dedicated scalar-function catalogue once that's
// ported.
func (c *FunctionCatalog) RegisterDefaults() {
	// Panics on duplicate (caller misuse of double-registering);
	// silent when the catalogue is empty (the expected state).
	// All standard SQL aggregates accept DISTINCT. COUNT additionally
	// accepts *.
	must(c.Register(FunctionSpec{Name: "COUNT", Kind: FunctionAggregate, MinArgs: 1, MaxArgs: 1, AllowsStar: true, AllowsDistinct: true}))
	must(c.Register(FunctionSpec{Name: "SUM", Kind: FunctionAggregate, MinArgs: 1, MaxArgs: 1, AllowsDistinct: true}))
	must(c.Register(FunctionSpec{Name: "MIN", Kind: FunctionAggregate, MinArgs: 1, MaxArgs: 1, AllowsDistinct: true}))
	must(c.Register(FunctionSpec{Name: "MAX", Kind: FunctionAggregate, MinArgs: 1, MaxArgs: 1, AllowsDistinct: true}))
	must(c.Register(FunctionSpec{Name: "AVG", Kind: FunctionAggregate, MinArgs: 1, MaxArgs: 1, AllowsDistinct: true}))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// ValidateArity reports whether argCount is acceptable for spec.
// Zero = no arguments. A MaxArgs of -1 is treated as "no upper
// bound". Returns a typed error on mismatch so callers can build
// user-facing messages without string-matching.
//
// Callers handling `*` (star-argument) functions should check
// `AllowsStar` BEFORE calling ValidateArity and skip the arity check
// for the star case. The star is syntactically 0 arguments, but
// COUNT(*) is legal despite COUNT's MinArgs=1 — that's not a
// contradiction the arity check should reason about.
func (spec FunctionSpec) ValidateArity(argCount int) error {
	if argCount < spec.MinArgs {
		return &FunctionArityError{Function: spec.Name, Got: argCount, Min: spec.MinArgs, Max: spec.MaxArgs}
	}
	if spec.MaxArgs >= 0 && argCount > spec.MaxArgs {
		return &FunctionArityError{Function: spec.Name, Got: argCount, Min: spec.MinArgs, Max: spec.MaxArgs}
	}
	return nil
}

// FunctionArityError signals too few / too many arguments.
type FunctionArityError struct {
	Function string
	Got      int
	Min      int
	Max      int
}

func (e *FunctionArityError) Error() string {
	if e.Min == e.Max {
		return fmt.Sprintf("function %s expects %d argument(s); got %d", e.Function, e.Min, e.Got)
	}
	if e.Max < 0 {
		return fmt.Sprintf("function %s expects at least %d argument(s); got %d", e.Function, e.Min, e.Got)
	}
	return fmt.Sprintf("function %s expects %d..%d arguments; got %d", e.Function, e.Min, e.Max, e.Got)
}

// FunctionNotFoundError signals a lookup miss — the function name
// isn't registered in the catalogue.
type FunctionNotFoundError struct {
	Name Identifier
}

func (e *FunctionNotFoundError) Error() string {
	return fmt.Sprintf("unknown function: %s", e.Name)
}
