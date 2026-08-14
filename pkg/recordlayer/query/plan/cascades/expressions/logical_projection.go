package expressions

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalProjectionExpression represents the projection list of a SELECT.
// The set of Values it captures determines what columns flow upward;
// rows themselves are passed through from the inner Quantifier.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalProjectionExpression`.
//
// Note: Java's GetResultValue returns the inner's flowed object value,
// NOT a RecordConstructor over the projection list. The projection list
// is captured separately and consumed by upstream rules (the projection
// is implemented physically by a RecordQueryMapPlan that pulls the
// projected values out of the inner). We preserve that contract here.
type LogicalProjectionExpression struct {
	projectedValues []values.Value
	aliases         []string
	// aliasMinted is parallel to aliases: true = the MACHINERY wrote that
	// output name, false (and every slot past the slice) = the user did, via
	// `AS`. Carried, not recovered: a machinery key and a user alias can be
	// spelled identically ("A.K"), so only provenance separates them, and the
	// result-set label site needs exactly that separation.
	//
	// EXCLUDED from identity/hash exactly as values.FieldPath.FrontierPinned is
	// — a marker of who NAMED the slot, not of what the slot IS. Two projections
	// differing only in it emit the same rows under the same output names and
	// must intern as one memo member.
	aliasMinted []bool
	// aliasSources is the frozen structured source for each machinery-minted
	// alias. It is metadata provenance, not the owner of the Value program: the
	// latter can be alpha-rebased onto a physical `_current` carrier while the
	// former continues to name the authored source that supplied the alias.
	// Like aliasMinted, it is excluded from memo identity and hashing.
	aliasSources []values.ProjectionAliasSource
	inner        Quantifier
	resultValue  *values.RecordConstructorValue
	// outputNameOverrides contains only frozen SQL-boundary names that differ
	// from the Value/alias-derived schema. Internal correlation-derived names
	// are deliberately absent so projection hash remains alias-invariant.
	outputNameOverrides []string
	// authoredOutputIdentityOverrides separates an authored source-qualified
	// identity from the row schema this projection publishes. A top-level
	// `SELECT C.ID` emits the bare field ID, but C.ID can still be load-bearing
	// while logical alternatives are interned (notably a WITH-CTE source beside
	// its derived-table twin). Generated correlation names never enter this
	// vector; it is populated only from captured SQL identifier segments.
	authoredOutputIdentityOverrides []string
}

// NewLogicalProjectionExpression constructs a projection over `inner`
// emitting the given Value list. The list is copied defensively.
func NewLogicalProjectionExpression(projectedValues []values.Value, inner Quantifier) (*LogicalProjectionExpression, error) {
	return NewLogicalProjectionExpressionWithAliases(projectedValues, nil, inner)
}

// NewLogicalProjectionExpressionWithAliases includes output column aliases.
func NewLogicalProjectionExpressionWithAliases(projectedValues []values.Value, aliases []string, inner Quantifier) (*LogicalProjectionExpression, error) {
	return newLogicalProjectionExpression(projectedValues, aliases, nil, nil, inner)
}

func newLogicalProjectionExpression(
	projectedValues []values.Value,
	aliases []string,
	aliasMinted []bool,
	outputNames []string,
	inner Quantifier,
) (*LogicalProjectionExpression, error) {
	resultValue, err := values.ProjectionResultValueForOutputSchema(projectedValues, aliases, outputNames)
	if err != nil {
		return nil, fmt.Errorf("LogicalProjectionExpression result: %w", err)
	}
	outputNameOverrides, err := values.ProjectionOutputSchemaIdentityOverrides(projectedValues, aliases, outputNames)
	if err != nil {
		return nil, fmt.Errorf("LogicalProjectionExpression output identity: %w", err)
	}
	if _, err := snapshotExpressionResultType("LogicalProjectionExpression", resultValue.Type()); err != nil {
		return nil, err
	}
	copied := make([]values.Value, len(projectedValues))
	copy(copied, projectedValues)
	return &LogicalProjectionExpression{
		projectedValues:     copied,
		aliases:             slices.Clone(aliases),
		aliasMinted:         slices.Clone(aliasMinted),
		inner:               inner,
		resultValue:         resultValue,
		outputNameOverrides: outputNameOverrides,
	}, nil
}

// GetProjectedValues returns a defensive copy of the projection list.
func (e *LogicalProjectionExpression) GetProjectedValues() []values.Value {
	return slices.Clone(e.projectedValues)
}

// NewLogicalProjectionExpressionWithAliasProvenance is
// NewLogicalProjectionExpressionWithAliases plus the per-slot record of WHO
// named each output — the machinery or the user's `AS`. Every rewrite that
// hands an existing projection's aliases to a new expression must come through
// here, or the provenance is dropped and a machinery key starts reading as a
// user label.
func NewLogicalProjectionExpressionWithAliasProvenance(projectedValues []values.Value, aliases []string, aliasMinted []bool, inner Quantifier) (*LogicalProjectionExpression, error) {
	return newLogicalProjectionExpression(projectedValues, aliases, aliasMinted, nil, inner)
}

// NewLogicalProjectionExpressionWithOutputSchema freezes the SQL-visible
// output names independently of the Value program used to compute each slot.
// This is required when a scalar leg is represented by a whole QOV: its Value
// display name describes the leg, while the SELECT item can name a different
// column (for example an UNNEST AT ordinal named "AT").
func NewLogicalProjectionExpressionWithOutputSchema(
	projectedValues []values.Value,
	aliases []string,
	aliasMinted []bool,
	outputNames []string,
	inner Quantifier,
) (*LogicalProjectionExpression, error) {
	return newLogicalProjectionExpression(projectedValues, aliases, aliasMinted, outputNames, inner)
}

// WithAuthoredOutputIdentity records a captured SQL source-qualified identity
// without changing the projection's emitted result schema. outputNames is a
// complete, slot-aligned name vector. Only differences from the Value/alias
// derived schema are retained, so alpha-renamable generated correlation names
// remain outside memo equality and hashing.
func (e *LogicalProjectionExpression) WithAuthoredOutputIdentity(outputNames []string) (*LogicalProjectionExpression, error) {
	if e == nil {
		return nil, fmt.Errorf("LogicalProjectionExpression output identity: nil expression")
	}
	overrides, err := values.ProjectionOutputSchemaIdentityOverrides(
		e.projectedValues, e.aliases, outputNames)
	if err != nil {
		return nil, fmt.Errorf("LogicalProjectionExpression authored output identity: %w", err)
	}
	copy := *e
	copy.authoredOutputIdentityOverrides = overrides
	return &copy, nil
}

// WithInheritedOutputIdentity carries the captured authored identity through a
// row-program-preserving logical rewrite. The target's output schema remains
// its own; only the source's independently recorded memo discriminator moves.
func (e *LogicalProjectionExpression) WithInheritedOutputIdentity(source *LogicalProjectionExpression) *LogicalProjectionExpression {
	if e == nil || source == nil {
		return e
	}
	copy := *e
	copy.authoredOutputIdentityOverrides = slices.Clone(source.authoredOutputIdentityOverrides)
	return &copy
}

// GetAliases returns a defensive copy of the output column aliases.
func (e *LogicalProjectionExpression) GetAliases() []string {
	return slices.Clone(e.aliases)
}

// GetOutputNames returns the exact, deduplicated slot names frozen when the
// logical projection was constructed. A physical implementation may rebase
// its Value program onto a fresh quantifier edge, but that alpha-renaming must
// not rename the projection's result record.
func (e *LogicalProjectionExpression) GetOutputNames() []string {
	if e == nil || e.resultValue == nil {
		return nil
	}
	names := make([]string, len(e.resultValue.Fields))
	for i := range e.resultValue.Fields {
		names[i] = e.resultValue.Fields[i].Name
	}
	return names
}

// GetAliasMinted returns a defensive copy of the per-slot alias provenance,
// parallel to GetAliases. A nil or short result reads as "the user named it"
// for every slot it does not cover.
func (e *LogicalProjectionExpression) GetAliasMinted() []bool {
	return slices.Clone(e.aliasMinted)
}

// GetAliasSources returns a defensive copy of the structured per-slot source
// identities captured for machinery-minted aliases. A short/nil slice means
// the source was not captured; consumers must not recover it from alias text.
func (e *LogicalProjectionExpression) GetAliasSources() []values.ProjectionAliasSource {
	return slices.Clone(e.aliasSources)
}

// WithAliasSources returns a copy carrying checked structured alias-source
// provenance. The marker is metadata-only and therefore deliberately excluded
// from equality and hashing, just like aliasMinted.
func (e *LogicalProjectionExpression) WithAliasSources(
	sources []values.ProjectionAliasSource,
) (*LogicalProjectionExpression, error) {
	if e == nil {
		return nil, fmt.Errorf("LogicalProjectionExpression alias sources: nil expression")
	}
	if err := values.ValidateProjectionAliasSources(sources, e.aliasMinted, len(e.projectedValues)); err != nil {
		return nil, fmt.Errorf("LogicalProjectionExpression alias sources: %w", err)
	}
	copy := *e
	copy.aliasSources = slices.Clone(sources)
	return &copy, nil
}

// GetInner returns the inner Quantifier.
func (e *LogicalProjectionExpression) GetInner() Quantifier { return e.inner }

// GetResultValue states the row this projection PRODUCES: a record constructor
// over its projected columns, named by its output aliases.
//
// It used to pass the INNER's flowed object value through, which is Java's
// literal contract (LogicalProjectionExpression.java:102-106). That inheritance
// was the bug. In Java the class is legacy-only — its javadoc says "only used
// when we plan RecordQuerys" (:47) — and RemoveProjectionRule erases it before
// planning finishes, so nothing ever reads its result type. Go uses this
// expression as the SQL projection, a different job, and under it the inherited
// contract is false: a projection outputs its PROJECTED columns, not its
// inner's row.
//
// Stating the inner's row made a downstream reader take a multi-leg row at its
// word and refuse to serve a source-relative ordinal against it — `correlated
// FieldValue "ID" (correlation "D") … multi-leg row cannot serve a
// source-relative ordinal`. Stating no type instead only moved the failure:
// Quantifier.GetFlowedObjectType skips an untyped member, so a leg over a
// projection derived no layout at all and every read through it fell back to
// the qualified name.
//
// The derivation is shared with the physical twin
// (RecordQueryProjectionPlan.GetResultValue) through values.ProjectionResultValue
// so the two cannot state different rows for the same projection.
//
// THE ERROR PATH IS REACHABLE, and saying otherwise was this file's own bug.
// An earlier revision claimed a one-slot whole-row projection was "unbuildable,
// enforced by the constructors below" and that the fallback was "pinned as
// unreachable by test". Both were false: all seven constructors in this file are
// plain struct fills that validate nothing, and
// TestLogicalProjectionFallsBackToUntypedQOV builds exactly that shape and
// reaches this branch. The guard lives in values.ProjectionResultValue, which is
// a DERIVATION — it refuses to synthesise a row for the shape, it does not stop
// anyone constructing the expression.
//
// The fallback is therefore a live arm with a deliberate value: an UNTYPED QOV,
// which is precisely the pre-RFC decline. A one-slot bare-QOV projection cannot
// honestly say what row it produces (the executor emits one positional slot per
// projection, so the shape WRAPS the inner row rather than passing it through,
// and the wrapped shape has no name to give its single field). Declining is the
// honest answer for it; every other shape gets the real row. Library code does
// not panic, so this returns rather than raising.
//
// THE FALLBACK POPULATION IS STRICTLY LARGER THAN IsIdentity(). The guard fires
// on ANY single bare QuantifiedObjectValue — including one over a FOREIGN
// correlation, which is not an identity projection at all — so the sweep that
// measured identity projections is a LOWER bound on how often this arm is taken,
// not a count of it.
func (e *LogicalProjectionExpression) GetResultValue() values.Value {
	return e.resultValue
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalProjectionExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is always false for a projection — single child.
func (e *LogicalProjectionExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *LogicalProjectionExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren is the union of correlation sets
// across the projection list.
func (e *LogicalProjectionExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, v := range e.projectedValues {
		for k := range values.GetCorrelatedToOfValue(v) {
			out[k] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares two projections by projection-list and
// output-schema equality. values.ProjectionOutputIdentityKey folds the
// executor-visible name where it carries identity beyond the Value itself:
// explicit aliases and unaliased FieldValue display names at any depth.
func (e *LogicalProjectionExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalProjectionExpression)
	if !ok {
		return false
	}
	if len(e.projectedValues) != len(o.projectedValues) {
		return false
	}
	// The frozen schema is an independent output contract. Two projections can
	// compute the same ordinal Values without aliases yet publish different
	// positional keys. Interning them would make the surviving schema depend on
	// insertion order, so compare the explicit schema delta in addition to the
	// Value/alias discriminator below.
	if !slices.Equal(e.outputNameOverrides, o.outputNameOverrides) {
		return false
	}
	if !slices.Equal(e.authoredOutputIdentityOverrides, o.authoredOutputIdentityOverrides) {
		return false
	}
	// Alias-aware projected-Value equality (RFC-040 040.2). Inert under the
	// memo's empty-alias path until PR-A.
	//
	// PR-A ACTIVATION RESIDUAL, recorded where it will bite rather than in a
	// tracker. aliasMinted is deliberately excluded from this comparison and
	// from HashCodeWithoutChildren — it records who NAMED a slot, not what the
	// slot computes — and today that exclusion is safe because the alias map is
	// empty: two projections over differently-aliased correlations do not reach
	// the marker question at all, they simply compare unequal on the Values.
	//
	// When PR-A passes real alias maps, SemanticEqualsUnderAliasMap starts
	// equating exactly those pairs. A pair whose alias STRINGS are identical but
	// whose PROVENANCE differs — one slot named by the user's `AS "A.K"`, the
	// other by the machinery's same-leaf disambiguation key, which is precisely
	// the collision the marker exists to resolve — then interns as ONE member,
	// and which marker the surviving member carries is decided by insertion
	// order. The result-set label site reads that marker, so the user-visible
	// column label becomes memo-order dependent.
	//
	// Excluding the marker stays correct (a display tag must not split a memo
	// group). The fix belongs on the READ side: the label must be resolved
	// against the projection the query actually built, not recovered from
	// whichever equal member the memo happened to keep. Do not answer this by
	// folding aliasMinted into identity — that trades a wrong label for a
	// duplicated memo group, and re-creates the recover-instead-of-record
	// pattern the marker was introduced to end.
	vm := aliases.ToValuesAliasMap()
	for i := range e.projectedValues {
		if values.ProjectionOutputIdentityKey(e.projectedValues[i], projectionAliasAt(e.aliases, i)) !=
			values.ProjectionOutputIdentityKey(o.projectedValues[i], projectionAliasAt(o.aliases, i)) {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(e.projectedValues[i], o.projectedValues[i], vm) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren hashes the projection list and each slot's additional
// schema-name discriminator. This keeps semantically-equal baked FieldValues
// with different display names in distinct schema buckets without folding
// internal correlation aliases into the otherwise alias-invariant hash.
func (e *LogicalProjectionExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for i, v := range e.projectedValues {
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(v))
		h.Write(buf[:])
		outputIdentity := values.ProjectionOutputIdentityKey(v, projectionAliasAt(e.aliases, i))
		binary.LittleEndian.PutUint64(buf[:], uint64(len(outputIdentity)))
		h.Write(buf[:])
		h.Write([]byte(outputIdentity))
		outputName := ""
		if i < len(e.outputNameOverrides) {
			outputName = e.outputNameOverrides[i]
		}
		binary.LittleEndian.PutUint64(buf[:], uint64(len(outputName)))
		h.Write(buf[:])
		h.Write([]byte(outputName))
		authoredName := ""
		if i < len(e.authoredOutputIdentityOverrides) {
			authoredName = e.authoredOutputIdentityOverrides[i]
		}
		binary.LittleEndian.PutUint64(buf[:], uint64(len(authoredName)))
		h.Write(buf[:])
		h.Write([]byte(authoredName))
	}
	return h.Sum64()
}

// TieBreakHashCodeWithoutChildren returns the schema-neutral projection hash
// used only for deterministic candidate ranking. Output names belong in memo
// identity, but letting them perturb a cost tie-break changes the chosen
// operator placement for otherwise identical work. This is the exact
// pre-schema-identity projection hash: semantic projected Values plus their
// ordered slot boundaries.
func (e *LogicalProjectionExpression) TieBreakHashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for _, v := range e.projectedValues {
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(v))
		h.Write(buf[:])
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func projectionAliasAt(aliases []string, ordinal int) string {
	if ordinal < len(aliases) {
		return aliases[ordinal]
	}
	return ""
}

// WithQuantifiers rewires the child edge and changes nothing else, so it copies
// the whole struct rather than re-listing fields: a field-by-field literal
// silently drops anything added later, and this runs on EVERY memo child
// rewrite — the one place a dropped alias provenance would be invisible.
func (e *LogicalProjectionExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("LogicalProjectionExpression", len(quantifiers), 1); err != nil {
		return nil, err
	}
	copy := *e
	copy.inner = quantifiers[0]
	return &copy, nil
}

var _ RelationalExpression = (*LogicalProjectionExpression)(nil)
