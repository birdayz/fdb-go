package values

import (
	"bytes"
	"encoding/binary"
	"hash/fnv"
	"sort"
)

// OrdinalTileKind identifies how a consecutive range of fields is physically
// represented inside one record carrier.
type OrdinalTileKind uint8

const (
	OrdinalTileInvalid OrdinalTileKind = iota
	OrdinalTileFlat
	OrdinalTileNested
)

// OrdinalTileSpec is mutable construction input. NewOrdinalLayout snapshots
// Parent and never retains this value.
type OrdinalTileSpec struct {
	Parent []int
	Start  int
	Width  int
	Kind   OrdinalTileKind
}

// OrdinalWindowSpec declares where one exact source object is represented in
// the carrier. Exactly one of ObjectPath and FieldPaths must be non-nil.
type OrdinalWindowSpec struct {
	Source        QuantifiedObjectValue
	ObjectPath    []int
	FieldPaths    [][]int
	NullSupplying bool
}

// OrdinalCarrierKind distinguishes record carriers from scalar carriers.
type OrdinalCarrierKind uint8

const (
	OrdinalCarrierInvalid OrdinalCarrierKind = iota
	OrdinalCarrierRecord
	OrdinalCarrierScalar
)

// OrdinalLayout is the immutable physical description of one evaluation
// phase. Its concrete representation is values-owned and exact-recognized at
// every purpose API.
type OrdinalLayout interface {
	Carrier() QuantifiedObjectValue
	CarrierKind() OrdinalCarrierKind
	// WindowSources returns the exact local source objects retained inside the
	// carrier. The returned slice is an immutable-view copy; each QOV remains
	// values-owned and can be passed back to exact binding APIs.
	WindowSources() []QuantifiedObjectValue
	// NullSupplyingWindowSources returns, IN WINDOW ORDER, the sources whose
	// match state a binder requires and cannot infer. It exists so a component
	// that must CARRY that state across a boundary — a continuation, a spill —
	// can enumerate exactly the sources it has to carry, in an order both sides
	// agree on without naming anything. Row contents cannot substitute: a
	// matched row of all-NULL columns and an unmatched row are identical in the
	// slots and different in meaning.
	NullSupplyingWindowSources() []QuantifiedObjectValue
	RawEqual(OrdinalLayout) bool
	EqualUnderAliases(OrdinalLayout, AliasMap) bool
	AliasFreeHash() uint64
	isOrdinalLayoutView()
}

type ordinalTile struct {
	parent []int
	start  int
	width  int
	kind   OrdinalTileKind
}

type ordinalWindow struct {
	source        *quantifiedObjectValue
	objectPath    []int
	fieldPaths    [][]int
	nullSupplying bool
}

type ordinalLayout struct {
	carrier     *quantifiedObjectValue
	carrierKind OrdinalCarrierKind
	tiles       []ordinalTile
	windows     []ordinalWindow
	bySource    map[CorrelationIdentifier]int
	hash        uint64
}

func (*ordinalLayout) isOrdinalLayoutView() {}

func (l *ordinalLayout) Carrier() QuantifiedObjectValue {
	if l == nil {
		return nil
	}
	return l.carrier
}

func (l *ordinalLayout) CarrierKind() OrdinalCarrierKind {
	if l == nil {
		return OrdinalCarrierInvalid
	}
	return l.carrierKind
}

func (l *ordinalLayout) WindowSources() []QuantifiedObjectValue {
	if l == nil || len(l.windows) == 0 {
		return nil
	}
	result := make([]QuantifiedObjectValue, len(l.windows))
	for i := range l.windows {
		result[i] = l.windows[i].source
	}
	return result
}

func (l *ordinalLayout) NullSupplyingWindowSources() []QuantifiedObjectValue {
	if l == nil || len(l.windows) == 0 {
		return nil
	}
	var result []QuantifiedObjectValue
	for i := range l.windows {
		if l.windows[i].nullSupplying {
			result = append(result, l.windows[i].source)
		}
	}
	return result
}

func (l *ordinalLayout) AliasFreeHash() uint64 {
	if l == nil {
		return 0
	}
	return l.hash
}

// NewOrdinalLayout validates and snapshots a record-carrier layout.
func NewOrdinalLayout(
	carrier QuantifiedObjectValue,
	tiles []OrdinalTileSpec,
	windows []OrdinalWindowSpec,
) (OrdinalLayout, error) {
	exactCarrier, err := exactLayoutQOV(carrier, "layout.carrier")
	if err != nil {
		return nil, err
	}
	if !exactCarrier.correlation.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "layout.carrier", "layout carrier must be tagged current")
	}
	if exactCarrier.flowed.code != TypeCodeRecord || exactCarrier.flowed.anyRecord {
		return nil, resolutionError(LayoutNonRecordCarrier, "layout.carrier", "record layout requires a concrete record carrier")
	}

	exactTiles, err := validateOrdinalTiles(exactCarrier.flowed, tiles)
	if err != nil {
		return nil, err
	}
	exactWindows, bySource, err := validateOrdinalWindows(exactCarrier.flowed, windows)
	if err != nil {
		return nil, err
	}
	// The layout knows where every source's columns sit; the carrier — the value
	// everything downstream RE-MINTS from — did not say so. Express the
	// boundaries on the carrier so they survive that re-mint.
	//
	// NewQuantifiedObjectValue snapshots its source layout from the type it is
	// handed, and building one QOV from another's FlowedType is the normal way
	// rows are carried across a plan. Without boundaries here, the merged row
	// arrives downstream having forgotten them — and a forgotten boundary does
	// not read as "no legs", it reads as ONE run spanning the whole concat keyed
	// by the box's correlation (its rightmost leaf, per sourceBinding). A
	// qualified read then resolves at runOffset+ordinal, inside the FIRST leg.
	// Measured on `FOA FULL OUTER FOB`: `FOB.K` read FOA's K.
	//
	// Identity is untouched: legs are not recorded by SnapshotExactType and not
	// compared by RecordType.Equals, so this adds physical information only.
	exactCarrier = carrierWithWindowLegs(exactCarrier, exactWindows)
	layout := &ordinalLayout{
		carrier:     exactCarrier,
		carrierKind: OrdinalCarrierRecord,
		tiles:       exactTiles,
		windows:     exactWindows,
		bySource:    bySource,
	}
	layout.hash = hashOrdinalLayout(layout)
	return layout, nil
}

// NewScalarOrdinalLayout validates an exact scalar current carrier. Scalar
// layouts deliberately contain neither tiles nor source windows.
func NewScalarOrdinalLayout(carrier QuantifiedObjectValue) (OrdinalLayout, error) {
	exactCarrier, err := exactLayoutQOV(carrier, "layout.carrier")
	if err != nil {
		return nil, err
	}
	if !exactCarrier.correlation.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "layout.carrier", "layout carrier must be tagged current")
	}
	if exactCarrier.flowed.code == TypeCodeRecord {
		return nil, resolutionError(LayoutNonRecordCarrier, "layout.carrier", "scalar layout cannot carry a record")
	}
	layout := &ordinalLayout{
		carrier:     exactCarrier,
		carrierKind: OrdinalCarrierScalar,
		bySource:    make(map[CorrelationIdentifier]int),
	}
	layout.hash = hashOrdinalLayout(layout)
	return layout, nil
}

// NewOrdinalLayoutForCarrierType is the purpose factory for an owner that has
// not yet published its current QOV. It snapshots typ, privately mints the
// exact current handle, and atomically validates the layout around that handle.
// The handle is exposed only as layout.Carrier().
func NewOrdinalLayoutForCarrierType(
	typ Type,
	tiles []OrdinalTileSpec,
	windows []OrdinalWindowSpec,
) (OrdinalLayout, error) {
	carrier, err := newCurrentQOVForLayout(typ)
	if err != nil {
		return nil, err
	}
	return NewOrdinalLayout(carrier, tiles, windows)
}

// NewScalarOrdinalLayoutForCarrierType is the scalar counterpart of
// NewOrdinalLayoutForCarrierType.
func NewScalarOrdinalLayoutForCarrierType(typ Type) (OrdinalLayout, error) {
	carrier, err := newCurrentQOVForLayout(typ)
	if err != nil {
		return nil, err
	}
	return NewScalarOrdinalLayout(carrier)
}

func newCurrentQOVForLayout(typ Type) (*quantifiedObjectValue, error) {
	handle, err := SnapshotExactType(typ)
	if err != nil {
		return nil, err
	}
	exact := handle.(*exactType)
	if exact.code == TypeCodeNull || exact.code == TypeCodeRelation {
		return nil, resolutionError(TypeMalformedCode, "layout.carrier", "layout carrier must be an object or scalar exact type")
	}
	return &quantifiedObjectValue{correlation: CurrentCorrelation(), flowed: exact}, nil
}

func exactLayoutQOV(view QuantifiedObjectValue, path string) (*quantifiedObjectValue, error) {
	concrete, ok := view.(*quantifiedObjectValue)
	if !ok || concrete == nil || concrete.flowed == nil || concrete.correlation.IsZero() {
		return nil, resolutionError(CorrelationForeignValue, path, "QOV is not a values-owned exact value")
	}
	return concrete, nil
}

func exactOrdinalLayout(view OrdinalLayout) (*ordinalLayout, bool) {
	concrete, ok := view.(*ordinalLayout)
	return concrete, ok && concrete != nil && concrete.carrier != nil && concrete.carrier.flowed != nil
}

// ValidateOrdinalLayoutAdmission exact-recognizes a layout without invoking
// methods on a hostile interface implementation.
func ValidateOrdinalLayoutAdmission(view OrdinalLayout) error {
	if _, ok := exactOrdinalLayout(view); !ok {
		return resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	return nil
}

// IsCanonicalCurrentOnlyOrdinalLayout reports whether layout carries only its
// tagged-current object and therefore needs no retained-source bindings or
// per-row window presence. Record tile shape may be flat or nested: both are
// canonical carrier-relative addressing once source windows are absent.
//
// A valid windowed layout returns (false, nil). Foreign, nil, and typed-nil
// views fail loudly before any interface method is invoked, matching the other
// layout purpose APIs.
func IsCanonicalCurrentOnlyOrdinalLayout(layout OrdinalLayout) (bool, error) {
	exact, ok := exactOrdinalLayout(layout)
	if !ok {
		return false, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	return len(exact.windows) == 0, nil
}

type ordinalTileGroup struct {
	parent []int
	tiles  []ordinalTile
}

func validateOrdinalTiles(root *exactType, specs []OrdinalTileSpec) ([]ordinalTile, error) {
	groups := make(map[string]*ordinalTileGroup)
	rootKey := ordinalPathKey(nil)
	groups[rootKey] = &ordinalTileGroup{}
	all := make([]ordinalTile, 0, len(specs))

	for i := range specs {
		spec := &specs[i]
		path := "layout.tile[" + uitoa(uint64(i)) + "]"
		if spec.Kind != OrdinalTileFlat && spec.Kind != OrdinalTileNested {
			return nil, resolutionError(LayoutInvalidTile, path+".kind", "tile kind is invalid")
		}
		if spec.Start < 0 || spec.Width <= 0 {
			return nil, resolutionError(LayoutInvalidTile, path, "tile start and width must be positive bounds")
		}
		if spec.Kind == OrdinalTileNested && spec.Width != 1 {
			return nil, resolutionError(LayoutInvalidTile, path+".width", "nested tile width must be one")
		}

		parent := append([]int(nil), spec.Parent...)
		parentType, err := resolveLayoutPath(root, parent, true)
		if err != nil || parentType.code != TypeCodeRecord || parentType.anyRecord {
			return nil, resolutionError(LayoutInvalidPath, path+".parent", "tile parent is not a concrete carrier record")
		}
		if spec.Start > len(parentType.fields) || spec.Width > len(parentType.fields)-spec.Start {
			return nil, resolutionError(LayoutInvalidTile, path, "tile range exceeds its parent record")
		}
		if spec.Kind == OrdinalTileNested {
			nested := parentType.fields[spec.Start].typ
			if nested.code != TypeCodeRecord || nested.anyRecord {
				return nil, resolutionError(LayoutInvalidTile, path, "nested tile does not identify a concrete record field")
			}
		}

		tile := ordinalTile{parent: parent, start: spec.Start, width: spec.Width, kind: spec.Kind}
		all = append(all, tile)
		key := ordinalPathKey(parent)
		group := groups[key]
		if group == nil {
			group = &ordinalTileGroup{parent: append([]int(nil), parent...)}
			groups[key] = group
		}
		group.tiles = append(group.tiles, tile)
	}

	orderedGroups := make([]*ordinalTileGroup, 0, len(groups))
	for _, group := range groups {
		orderedGroups = append(orderedGroups, group)
	}
	sort.Slice(orderedGroups, func(i, j int) bool {
		return compareOrdinalPaths(orderedGroups[i].parent, orderedGroups[j].parent) < 0
	})
	for _, group := range orderedGroups {
		parentType, err := resolveLayoutPath(root, group.parent, true)
		if err != nil {
			return nil, resolutionError(LayoutInvalidPath, "layout.tile.parent", "tile parent is invalid")
		}
		sort.Slice(group.tiles, func(i, j int) bool {
			if group.tiles[i].start != group.tiles[j].start {
				return group.tiles[i].start < group.tiles[j].start
			}
			if group.tiles[i].width != group.tiles[j].width {
				return group.tiles[i].width < group.tiles[j].width
			}
			return group.tiles[i].kind < group.tiles[j].kind
		})
		cursor := 0
		for i := range group.tiles {
			tile := &group.tiles[i]
			if tile.start < cursor {
				return nil, resolutionError(LayoutTileOverlap, "layout.tile", "tile ranges overlap")
			}
			if tile.start > cursor {
				return nil, resolutionError(LayoutTileGap, "layout.tile", "tile ranges contain a gap")
			}
			cursor = tile.start + tile.width
		}
		if cursor != len(parentType.fields) {
			return nil, resolutionError(LayoutTileGap, "layout.tile", "tiles do not cover their parent record")
		}
	}

	expectedNested := make(map[string]struct{})
	for i := range all {
		if all[i].kind != OrdinalTileNested {
			continue
		}
		child := append(append([]int(nil), all[i].parent...), all[i].start)
		expectedNested[ordinalPathKey(child)] = struct{}{}
		childType, _ := resolveLayoutPath(root, child, false)
		if len(childType.fields) > 0 {
			if _, present := groups[ordinalPathKey(child)]; !present {
				return nil, resolutionError(LayoutTileGap, "layout.tile", "nested carrier has no tile partition")
			}
		}
	}
	for key := range groups {
		if key == rootKey {
			continue
		}
		if _, declared := expectedNested[key]; !declared {
			return nil, resolutionError(LayoutInvalidTile, "layout.tile.parent", "tile parent was not declared by a nested tile")
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if compared := compareOrdinalPaths(all[i].parent, all[j].parent); compared != 0 {
			return compared < 0
		}
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		if all[i].width != all[j].width {
			return all[i].width < all[j].width
		}
		return all[i].kind < all[j].kind
	})
	return all, nil
}

func validateOrdinalWindows(
	carrierType *exactType,
	specs []OrdinalWindowSpec,
) ([]ordinalWindow, map[CorrelationIdentifier]int, error) {
	windows := make([]ordinalWindow, 0, len(specs))
	bySource := make(map[CorrelationIdentifier]int, len(specs))
	objectOwner := make(map[string]CorrelationIdentifier, len(specs))
	for i := range specs {
		spec := &specs[i]
		path := "layout.window[" + uitoa(uint64(i)) + "]"
		source, err := exactLayoutQOV(spec.Source, path+".source")
		if err != nil {
			return nil, nil, err
		}
		if source.correlation.isCurrent() {
			return nil, nil, resolutionError(CorrelationKindMismatch, path+".source", "current cannot be a window source")
		}
		if previous, duplicate := bySource[source.correlation]; duplicate {
			if exactTypesEqual(windows[previous].source.flowed, source.flowed) {
				return nil, nil, resolutionError(LayoutDuplicateSource, path+".source", "exact source already has a window")
			}
			return nil, nil, resolutionError(CorrelationTypeConflict, path+".source", "one correlation has conflicting source types")
		}
		if spec.NullSupplying && !source.flowed.nullable {
			return nil, nil, resolutionError(LayoutNullabilityMismatch, path+".source",
				"null-supplying source "+source.correlation.Name()+" must be nullable, but flows "+
					describeExactType(source.flowed))
		}

		objectMode := spec.ObjectPath != nil
		fieldMode := spec.FieldPaths != nil
		if objectMode == fieldMode {
			return nil, nil, resolutionError(LayoutInvalidWindow, path, "exactly one window addressing mode is required")
		}

		window := ordinalWindow{source: source, nullSupplying: spec.NullSupplying}
		if objectMode {
			if len(spec.ObjectPath) == 0 {
				return nil, nil, resolutionError(LayoutInvalidPath, path+".object", "window paths must be nonempty")
			}
			actual, resolveErr := resolveLayoutPath(carrierType, spec.ObjectPath, false)
			if resolveErr != nil {
				return nil, nil, resolutionError(LayoutInvalidPath, path+".object", "object path is invalid")
			}
			if err := requireLayoutType(actual, source.flowed, path+".object"); err != nil {
				return nil, nil, err
			}
			objectKey := ordinalPathKey(spec.ObjectPath)
			if previous, duplicate := objectOwner[objectKey]; duplicate {
				return nil, nil, resolutionError(
					LayoutInvalidWindow, path+".object",
					"one complete carrier object cannot belong to both "+previous.Name()+" and "+source.correlation.Name())
			}
			objectOwner[objectKey] = source.correlation
			window.objectPath = append([]int(nil), spec.ObjectPath...)
		} else {
			if source.flowed.code != TypeCodeRecord || source.flowed.anyRecord {
				return nil, nil, resolutionError(LayoutInvalidWindow, path+".fields", "field mode requires a concrete record source")
			}
			if len(spec.FieldPaths) != len(source.flowed.fields) {
				return nil, nil, resolutionError(LayoutInvalidWindow, path+".fields", "field path count does not match source width")
			}
			window.fieldPaths = make([][]int, len(spec.FieldPaths))
			seen := make(map[string]struct{}, len(spec.FieldPaths))
			for fieldIndex := range spec.FieldPaths {
				fieldPath := spec.FieldPaths[fieldIndex]
				fieldLocation := path + ".fields[" + uitoa(uint64(fieldIndex)) + "]"
				if len(fieldPath) == 0 {
					return nil, nil, resolutionError(LayoutInvalidPath, fieldLocation, "window paths must be nonempty")
				}
				key := ordinalPathKey(fieldPath)
				if _, duplicate := seen[key]; duplicate {
					return nil, nil, resolutionError(LayoutInvalidWindow, fieldLocation, "field paths must be unique within a window")
				}
				seen[key] = struct{}{}
				actual, resolveErr := resolveLayoutPath(carrierType, fieldPath, false)
				if resolveErr != nil {
					return nil, nil, resolutionError(LayoutInvalidPath, fieldLocation, "field path is invalid")
				}
				expectedField := source.flowed.fields[fieldIndex].typ
				expectedField = exactWithNullability(expectedField, source.flowed.nullable || expectedField.nullable)
				if err := requireLayoutType(actual, expectedField, fieldLocation); err != nil {
					return nil, nil, err
				}
				window.fieldPaths[fieldIndex] = append([]int(nil), fieldPath...)
			}
		}

		bySource[source.correlation] = len(windows)
		windows = append(windows, window)
	}
	return windows, bySource, nil
}

// resolveLayoutPath follows an ordinal path while applying the same inherited
// nullability OR rule as resolved FieldValue construction. Empty is admitted
// only when allowEmpty is true.
func resolveLayoutPath(root *exactType, path []int, allowEmpty bool) (*exactType, error) {
	if root == nil || (!allowEmpty && len(path) == 0) {
		return nil, resolutionError(LayoutInvalidPath, "layout.path", "path is empty or has no root")
	}
	current := root
	inheritedNullable := root.nullable
	for _, ordinal := range path {
		if ordinal < 0 {
			return nil, resolutionError(LayoutInvalidPath, "layout.path", "path contains a negative ordinal")
		}
		if current.code != TypeCodeRecord || current.anyRecord {
			return nil, resolutionError(LayoutInvalidPath, "layout.path", "path descends through a non-record type")
		}
		if ordinal >= len(current.fields) {
			return nil, resolutionError(LayoutInvalidPath, "layout.path", "path ordinal is out of range")
		}
		selected := current.fields[ordinal].typ
		inheritedNullable = inheritedNullable || selected.nullable
		current = exactWithNullability(selected, inheritedNullable)
	}
	return current, nil
}

func requireLayoutType(actual, expected *exactType, path string) error {
	if exactTypesEqual(actual, expected) {
		return nil
	}
	if layoutTypesEqualIgnoringNullability(actual, expected) {
		return resolutionError(LayoutNullabilityMismatch, path, "window path has incompatible inherited nullability")
	}
	return resolutionError(LayoutTypeMismatch, path, "window path and source have different exact types")
}

func layoutTypesEqualIgnoringNullability(left, right *exactType) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.code != right.code || left.anyRecord != right.anyRecord || left.name != right.name ||
		len(left.fields) != len(right.fields) || len(left.enumValues) != len(right.enumValues) {
		return false
	}
	for i := range left.fields {
		if left.fields[i].name != right.fields[i].name ||
			left.fields[i].ordinal != right.fields[i].ordinal ||
			!layoutTypesEqualIgnoringNullability(left.fields[i].typ, right.fields[i].typ) {
			return false
		}
	}
	if !layoutTypesEqualIgnoringNullability(left.element, right.element) {
		return false
	}
	for i := range left.enumValues {
		if left.enumValues[i] != right.enumValues[i] {
			return false
		}
	}
	return true
}

func ordinalPathKey(path []int) string {
	encoded := binary.AppendUvarint(nil, uint64(len(path)))
	for _, ordinal := range path {
		encoded = binary.AppendVarint(encoded, int64(ordinal))
	}
	return string(encoded)
}

func compareOrdinalPaths(left, right []int) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
}

func (l *ordinalLayout) RawEqual(other OrdinalLayout) bool {
	right, ok := exactOrdinalLayout(other)
	if !ok || l == nil {
		return false
	}
	return ordinalLayoutsEqual(l, right, nil)
}

func (l *ordinalLayout) EqualUnderAliases(other OrdinalLayout, aliases AliasMap) bool {
	right, ok := exactOrdinalLayout(other)
	if !ok || l == nil {
		return false
	}
	exactAliases, ok := asAliasMap(aliases)
	if !ok {
		return false
	}
	return ordinalLayoutsEqual(l, right, exactAliases)
}

func ordinalLayoutsEqual(left, right *ordinalLayout, aliases *aliasMap) bool {
	if left == nil || right == nil || left.carrierKind != right.carrierKind ||
		!exactTypesEqual(left.carrier.flowed, right.carrier.flowed) ||
		!layoutCorrelationEqual(left.carrier.correlation, right.carrier.correlation, aliases) ||
		len(left.tiles) != len(right.tiles) || len(left.windows) != len(right.windows) {
		return false
	}
	for i := range left.tiles {
		if !ordinalTilesEqual(&left.tiles[i], &right.tiles[i]) {
			return false
		}
	}
	for i := range left.windows {
		leftWindow := &left.windows[i]
		target := leftWindow.source.correlation
		if aliases != nil {
			if mapped, exists := aliases.forward[target]; exists {
				target = mapped
			}
		}
		rightIndex, exists := right.bySource[target]
		if !exists || !ordinalWindowsEqual(leftWindow, &right.windows[rightIndex]) {
			return false
		}
	}
	return true
}

func layoutCorrelationEqual(left, right CorrelationIdentifier, aliases *aliasMap) bool {
	if aliases != nil {
		if mapped, exists := aliases.forward[left]; exists {
			return mapped == right
		}
	}
	return left == right
}

func ordinalTilesEqual(left, right *ordinalTile) bool {
	return left.start == right.start && left.width == right.width && left.kind == right.kind &&
		compareOrdinalPaths(left.parent, right.parent) == 0
}

func ordinalWindowsEqual(left, right *ordinalWindow) bool {
	if left.nullSupplying != right.nullSupplying ||
		!exactTypesEqual(left.source.flowed, right.source.flowed) ||
		compareOrdinalPaths(left.objectPath, right.objectPath) != 0 ||
		len(left.fieldPaths) != len(right.fieldPaths) {
		return false
	}
	for i := range left.fieldPaths {
		if compareOrdinalPaths(left.fieldPaths[i], right.fieldPaths[i]) != 0 {
			return false
		}
	}
	return true
}

func hashOrdinalLayout(layout *ordinalLayout) uint64 {
	encoded := binary.AppendUvarint(nil, uint64(layout.carrierKind))
	encoded = appendCanonicalBytes(encoded, layout.carrier.flowed.canonical)
	encoded = binary.AppendUvarint(encoded, uint64(len(layout.tiles)))
	for i := range layout.tiles {
		tile := &layout.tiles[i]
		encoded = appendOrdinalPath(encoded, tile.parent)
		encoded = binary.AppendUvarint(encoded, uint64(tile.start))
		encoded = binary.AppendUvarint(encoded, uint64(tile.width))
		encoded = binary.AppendUvarint(encoded, uint64(tile.kind))
	}

	windowEncodings := make([][]byte, len(layout.windows))
	for i := range layout.windows {
		window := &layout.windows[i]
		windowEncoding := appendCanonicalBytes(nil, window.source.flowed.canonical)
		if window.nullSupplying {
			windowEncoding = append(windowEncoding, 1)
		} else {
			windowEncoding = append(windowEncoding, 0)
		}
		if window.objectPath != nil {
			windowEncoding = append(windowEncoding, 1)
			windowEncoding = appendOrdinalPath(windowEncoding, window.objectPath)
		} else {
			windowEncoding = append(windowEncoding, 2)
			windowEncoding = binary.AppendUvarint(windowEncoding, uint64(len(window.fieldPaths)))
			for _, fieldPath := range window.fieldPaths {
				windowEncoding = appendOrdinalPath(windowEncoding, fieldPath)
			}
		}
		windowEncodings[i] = windowEncoding
	}
	sort.Slice(windowEncodings, func(i, j int) bool {
		return bytes.Compare(windowEncodings[i], windowEncodings[j]) < 0
	})
	encoded = binary.AppendUvarint(encoded, uint64(len(windowEncodings)))
	for _, windowEncoding := range windowEncodings {
		encoded = appendCanonicalBytes(encoded, windowEncoding)
	}

	hash := fnv.New64a()
	_, _ = hash.Write(encoded)
	return hash.Sum64()
}

func appendOrdinalPath(encoded []byte, path []int) []byte {
	encoded = binary.AppendUvarint(encoded, uint64(len(path)))
	for _, ordinal := range path {
		encoded = binary.AppendUvarint(encoded, uint64(ordinal))
	}
	return encoded
}

// LayoutProvides reports whether layout contains the exact source window. A
// missing window is a typed optional physical miss, not an untyped false.
func LayoutProvides(layout OrdinalLayout, source QuantifiedObjectValue) (bool, error) {
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return false, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	exactSource, err := exactLayoutQOV(source, "layout.source")
	if err != nil {
		return false, err
	}
	if exactSource.correlation.isCurrent() {
		return false, resolutionError(CorrelationKindMismatch, "layout.source", "current cannot be a window source")
	}
	index, present := exactLayout.bySource[exactSource.correlation]
	if !present {
		return false, resolutionError(LayoutSourceNotProvided, "layout.source", "layout does not provide this source")
	}
	if !exactTypesEqual(exactLayout.windows[index].source.flowed, exactSource.flowed) {
		return false, resolutionError(CorrelationTypeConflict, "layout.source", "provided correlation has a different exact type")
	}
	return true, nil
}

// LayoutWindowNullSupplying reports whether one exact local source window is
// null-supplying. This is physical layout authority: record nullability alone
// cannot distinguish a retained nullable value from a source which was absent
// because an outer-join edge did not match.
func LayoutWindowNullSupplying(layout OrdinalLayout, source QuantifiedObjectValue) (bool, error) {
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return false, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	exactSource, err := exactLayoutQOV(source, "layout.source")
	if err != nil {
		return false, err
	}
	if exactSource.correlation.isCurrent() {
		return false, resolutionError(CorrelationKindMismatch, "layout.source", "current cannot be a window source")
	}
	index, present := exactLayout.bySource[exactSource.correlation]
	if !present {
		return false, resolutionError(LayoutSourceNotProvided, "layout.source", "layout does not provide this source")
	}
	window := &exactLayout.windows[index]
	if !exactTypesEqual(window.source.flowed, exactSource.flowed) {
		return false, resolutionError(CorrelationTypeConflict, "layout.source", "provided correlation has a different exact type")
	}
	return window.nullSupplying, nil
}
