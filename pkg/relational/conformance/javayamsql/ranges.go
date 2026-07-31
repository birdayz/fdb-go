package javayamsql

import (
	"errors"
	"fmt"
	"sort"
)

// versionKey is a total order over SemanticVersion.
//
// SemanticVersion.compareTo orders first by type ordinal and only then by the
// numeric parts, with the enum declared MIN < NORMAL < CURRENT < MAX. So
// `!current_version` sorts above every literal version, which is what makes
// `initialVersionLessThan: !current_version` cover everything below it.
type versionKey struct {
	typ   int // 0=MIN, 1=NORMAL, 2=CURRENT, 3=MAX
	parts [4]int
}

const (
	typMin     = 0
	typNormal  = 1
	typCurrent = 2
	typMax     = 3
)

var (
	minKey = versionKey{typ: typMin}
	maxKey = versionKey{typ: typMax}
)

func keyOf(v *Version) versionKey {
	if v == nil {
		return minKey
	}
	if v.Current {
		return versionKey{typ: typCurrent}
	}
	return versionKey{typ: typNormal, parts: [4]int{v.Major, v.Minor, v.Build, v.Patch}}
}

func (a versionKey) cmp(b versionKey) int {
	if a.typ != b.typ {
		if a.typ < b.typ {
			return -1
		}
		return 1
	}
	if a.typ != typNormal {
		return 0
	}
	for i := range a.parts {
		if a.parts[i] != b.parts[i] {
			if a.parts[i] < b.parts[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (a versionKey) String() string {
	switch a.typ {
	case typMin:
		return "!min_version"
	case typCurrent:
		return "!current_version"
	case typMax:
		return "!max_version"
	default:
		return fmt.Sprintf("%d.%d.%d.%d", a.parts[0], a.parts[1], a.parts[2], a.parts[3])
	}
}

// versionRange is the half-open [lo, hi) that Guava's Range.closedOpen models.
type versionRange struct {
	lo, hi versionKey
}

func (r versionRange) empty() bool { return r.lo.cmp(r.hi) >= 0 }

func (r versionRange) String() string { return "[" + r.lo.String() + ".." + r.hi.String() + ")" }

// rangeFrom builds the range a pair of initialVersion* bounds denotes. Absent
// bounds widen to MIN / MAX, matching SemanticVersionRanges.atLeast/lessThan.
func rangeFrom(atLeast, lessThan *Version) versionRange {
	r := versionRange{lo: minKey, hi: maxKey}
	if atLeast != nil {
		r.lo = keyOf(atLeast)
	}
	if lessThan != nil {
		r.hi = keyOf(lessThan)
	}
	return r
}

// uncovered returns the gaps in [MIN, MAX) left by ranges, mirroring
// SemanticVersionRanges.uncovered.
func uncovered(ranges []versionRange) []versionRange {
	sorted := make([]versionRange, 0, len(ranges))
	for _, r := range ranges {
		if !r.empty() {
			sorted = append(sorted, r)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].lo.cmp(sorted[j].lo) < 0 })

	var gaps []versionRange
	cursor := minKey
	for _, r := range sorted {
		if r.lo.cmp(cursor) > 0 {
			gaps = append(gaps, versionRange{lo: cursor, hi: r.lo})
		}
		if r.hi.cmp(cursor) > 0 {
			cursor = r.hi
		}
	}
	if cursor.cmp(maxKey) < 0 {
		gaps = append(gaps, versionRange{lo: cursor, hi: maxKey})
	}
	return gaps
}

// overlapping returns the pairwise intersections, mirroring
// SemanticVersionRanges.overlapping.
func overlapping(ranges []versionRange) []versionRange {
	var out []versionRange
	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			lo, hi := ranges[i].lo, ranges[i].hi
			if ranges[j].lo.cmp(lo) > 0 {
				lo = ranges[j].lo
			}
			if ranges[j].hi.cmp(hi) < 0 {
				hi = ranges[j].hi
			}
			inter := versionRange{lo: lo, hi: hi}
			if !inter.empty() {
				out = append(out, inter)
			}
		}
	}
	return out
}

// validateVersionCoverage enforces QueryConfig.validateConfigs' requirement that
// a query's initialVersion* configs partition every possible version.
func validateVersionCoverage(configs []*Config) error {
	var ranges []versionRange
	line := 0
	for _, c := range configs {
		switch c.Kind {
		case ConfigInitialVersionAtLeast:
			ranges = append(ranges, rangeFrom(c.Version, nil))
		case ConfigInitialVersionLessThan:
			ranges = append(ranges, rangeFrom(nil, c.Version))
		default:
			continue
		}
		if line == 0 {
			line = c.Line
		}
	}
	if len(ranges) == 0 {
		return nil
	}
	if gaps := uncovered(ranges); len(gaps) > 0 {
		return &ParseError{
			Line: line,
			Err:  fmt.Errorf("test case does not cover complete set of versions as it is missing: %v", gaps),
		}
	}
	return nil
}

// validateVariantRanges enforces the overlap and coverage checks
// SetupBlock.SchemaTemplateBlock.validateVariants applies to the list form.
func validateVariantRanges(variants []SchemaTemplateVariant, line int) error {
	ranges := make([]versionRange, 0, len(variants))
	for _, v := range variants {
		r := rangeFrom(v.AtLeast, v.LessThan)
		if r.empty() {
			return &ParseError{
				Line: v.Line,
				Err:  fmt.Errorf("schema_template variant has empty version range %s", r),
			}
		}
		ranges = append(ranges, r)
	}
	if len(ranges) == 0 {
		return &ParseError{Line: line, Err: errors.New("schema_template list form must declare at least one variant")}
	}
	if ov := overlapping(ranges); len(ov) > 0 {
		return &ParseError{Line: line, Err: fmt.Errorf("schema_template variants have overlapping version ranges: %v", ov)}
	}
	if gaps := uncovered(ranges); len(gaps) > 0 {
		return &ParseError{Line: line, Err: fmt.Errorf("schema_template variants do not cover the complete set of versions; missing: %v", gaps)}
	}
	return nil
}
