package cascades

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// PlannerTrace attributes planning work to what caused it: per phase, task
// kind and rule, how many tasks ran, how long they took and how many memo
// groups they created. A search-space explosion shows as one rule minting
// thousands of groups; a per-function CPU profile shows only where each task
// spends its time, never why there are so many tasks.
//
// A trace belongs to one planning run and is not safe for concurrent use.
type PlannerTrace struct {
	entries map[plannerTraceKey]*PlannerTraceEntry
	kinds   map[reflect.Type]string
	memo    *Memo
	tasks   int
	// initialGroups is the memo size before the first task, so the rows'
	// NewGroups account for every group the run created.
	initialGroups int
}

type plannerTraceKey struct {
	phase PlannerPhase
	kind  string
	rule  string
}

// PlannerTraceEntry is the work attributed to one phase, task kind and rule.
type PlannerTraceEntry struct {
	Phase PlannerPhase
	// Kind is the task type, for example TransformExprTask.
	Kind string
	// Rule is the rule type for rule tasks and empty otherwise.
	Rule     string
	Tasks    int
	Duration time.Duration
	// NewGroups is the memo growth during these tasks; a cross-group merge
	// makes it negative.
	NewGroups int
}

// NewPlannerTrace returns an empty trace for Planner.WithTrace.
func NewPlannerTrace() *PlannerTrace {
	return &PlannerTrace{
		entries: make(map[plannerTraceKey]*PlannerTraceEntry),
		kinds:   make(map[reflect.Type]string),
	}
}

// WithTrace attributes this planner's work to trace. A nil trace disables it.
func (p *Planner) WithTrace(trace *PlannerTrace) *Planner {
	p.trace = trace
	return p
}

func (p *Planner) runTraced(ctx context.Context, task Task) {
	phase, groups, started := p.activePhase, len(p.memo.refs), time.Now()
	task.Run(ctx, p)
	p.trace.record(phase, task, time.Since(started), len(p.memo.refs)-groups)
}

func (t *PlannerTrace) record(phase PlannerPhase, task Task, elapsed time.Duration, newGroups int) {
	key := plannerTraceKey{phase: phase, kind: t.typeName(task)}
	switch typed := task.(type) {
	case *TransformExprTask:
		key.rule = t.typeName(typed.Rule)
	case *TransformMatchPartitionTask:
		key.rule = t.typeName(typed.Rule)
	case *TransformImplTask:
		key.rule = t.typeName(typed.Rule)
	}
	entry := t.entries[key]
	if entry == nil {
		entry = &PlannerTraceEntry{Phase: phase, Kind: key.kind, Rule: key.rule}
		t.entries[key] = entry
	}
	entry.Tasks++
	entry.Duration += elapsed
	entry.NewGroups += newGroups
	t.tasks++
}

func (t *PlannerTrace) typeName(value any) string {
	typ := reflect.TypeOf(value)
	if name, ok := t.kinds[typ]; ok {
		return name
	}
	name := "<nil>"
	if typ != nil {
		name = typ.Name()
		if typ.Kind() == reflect.Pointer {
			name = typ.Elem().Name()
		}
	}
	t.kinds[typ] = name
	return name
}

// Tasks reports how many tasks the trace attributed.
func (t *PlannerTrace) Tasks() int { return t.tasks }

// InitialGroups reports the memo size when the traced run started.
func (t *PlannerTrace) InitialGroups() int { return t.initialGroups }

func (t *PlannerTrace) attach(memo *Memo) {
	t.memo = memo
	t.initialGroups = len(memo.refs)
}

// Entries returns the attribution rows, most expensive first.
func (t *PlannerTrace) Entries() []PlannerTraceEntry {
	rows := make([]PlannerTraceEntry, 0, len(t.entries))
	for _, entry := range t.entries {
		rows = append(rows, *entry)
	}
	slices.SortFunc(rows, func(a, b PlannerTraceEntry) int {
		return cmp.Or(cmp.Compare(b.Duration, a.Duration), cmp.Compare(b.Tasks, a.Tasks),
			cmp.Compare(a.Phase, b.Phase), strings.Compare(a.Kind, b.Kind), strings.Compare(a.Rule, b.Rule))
	})
	return rows
}

// MemoCensus describes the memo a traced run left behind.
type MemoCensus struct {
	Groups int
	// Members counts every exploratory and final member by expression type.
	Members map[string]int
	// LargestGroups lists the groups with the most members, largest first.
	LargestGroups []MemoCensusGroup
	// EquivalentGroups lists classes of groups holding equal members:
	// alternatives explored more than once. PLANNING merges groups holding
	// the same operator over the same inputs, so what remains holds equal
	// members over different input groups.
	EquivalentGroups [][]MemoCensusGroup
}

// MemoCensusGroup identifies a memo group in a census.
type MemoCensusGroup struct {
	ID      uint64
	Members int
	// Example is the type of the member a duplicate class was found by, or
	// the first member's type.
	Example string
}

// Census describes the memo of the run this trace observed.
func (t *PlannerTrace) Census(largest int) MemoCensus {
	census := MemoCensus{Members: make(map[string]int)}
	if t.memo == nil {
		return census
	}
	type memberKey struct {
		kind     string
		hash     uint64
		children string
	}
	var groups []*expressions.Reference
	byKey := make(map[memberKey][]expressions.RelationalExpression)
	owner := make(map[expressions.RelationalExpression]*expressions.Reference)
	for ref := range t.memo.refs {
		if ref.Canonical() != ref {
			continue
		}
		groups = append(groups, ref)
		for _, member := range ref.AllMembers() {
			kind := t.typeName(member)
			census.Members[kind]++
			var children strings.Builder
			for _, quantifier := range member.GetQuantifiers() {
				fmt.Fprintf(&children, "%d,", quantifier.GetRangesOver().Canonical().ID())
			}
			key := memberKey{kind, member.HashCodeWithoutChildren(), children.String()}
			byKey[key] = append(byKey[key], member)
			owner[member] = ref
		}
	}
	census.Groups = len(groups)
	describe := func(ref *expressions.Reference, example string) MemoCensusGroup {
		if example == "" && len(ref.AllMembers()) > 0 {
			example = t.typeName(ref.AllMembers()[0])
		}
		return MemoCensusGroup{ID: ref.ID(), Members: len(ref.AllMembers()), Example: example}
	}
	slices.SortFunc(groups, func(a, b *expressions.Reference) int {
		return cmp.Or(cmp.Compare(len(b.AllMembers()), len(a.AllMembers())), cmp.Compare(a.ID(), b.ID()))
	})
	for _, ref := range groups[:min(largest, len(groups))] {
		census.LargestGroups = append(census.LargestGroups, describe(ref, ""))
	}

	parent := make(map[*expressions.Reference]*expressions.Reference)
	var find func(*expressions.Reference) *expressions.Reference
	find = func(ref *expressions.Reference) *expressions.Reference {
		if next, ok := parent[ref]; ok && next != ref {
			root := find(next)
			parent[ref] = root
			return root
		}
		return ref
	}
	foundBy := make(map[*expressions.Reference]string)
	for key, members := range byKey {
		for i, a := range members {
			for _, b := range members[i+1:] {
				ra, rb := find(owner[a]), find(owner[b])
				if ra != rb && expressions.MemoEqual(a, b) && expressions.MemoEqual(b, a) {
					parent[rb] = ra
					foundBy[owner[a]], foundBy[owner[b]] = key.kind, key.kind
				}
			}
		}
	}
	classes := make(map[*expressions.Reference][]MemoCensusGroup)
	for _, ref := range groups {
		root := find(ref)
		classes[root] = append(classes[root], describe(ref, foundBy[ref]))
	}
	for _, class := range classes {
		if len(class) > 1 {
			slices.SortFunc(class, func(a, b MemoCensusGroup) int { return cmp.Compare(a.ID, b.ID) })
			census.EquivalentGroups = append(census.EquivalentGroups, class)
		}
	}
	slices.SortFunc(census.EquivalentGroups, func(a, b []MemoCensusGroup) int { return cmp.Compare(a[0].ID, b[0].ID) })
	return census
}

// WriteReport renders the top attribution rows and the memo census.
func (t *PlannerTrace) WriteReport(w io.Writer, top int) error {
	var total time.Duration
	for _, entry := range t.entries {
		total += entry.Duration
	}
	census := t.Census(5)
	var b strings.Builder
	fmt.Fprintf(&b, "planner trace: %d tasks, %s in tasks, %d memo groups\n", t.tasks, total.Round(time.Microsecond), census.Groups)
	fmt.Fprintf(&b, "%-10s %-30s %-45s %8s %12s %10s\n", "phase", "task", "rule", "tasks", "time", "newGroups")
	for _, entry := range t.Entries()[:min(top, len(t.entries))] {
		fmt.Fprintf(&b, "%-10s %-30s %-45s %8d %12s %10d\n", entry.Phase, entry.Kind, entry.Rule, entry.Tasks, entry.Duration.Round(time.Microsecond), entry.NewGroups)
	}
	kinds := make([]string, 0, len(census.Members))
	for kind := range census.Members {
		kinds = append(kinds, kind)
	}
	slices.SortFunc(kinds, func(a, b string) int {
		return cmp.Or(cmp.Compare(census.Members[b], census.Members[a]), strings.Compare(a, b))
	})
	b.WriteString("memo members by type:")
	for _, kind := range kinds {
		fmt.Fprintf(&b, " %s=%d", kind, census.Members[kind])
	}
	b.WriteString("\nlargest groups:")
	for _, group := range census.LargestGroups {
		fmt.Fprintf(&b, " #%d(%d members, %s)", group.ID, group.Members, group.Example)
	}
	fmt.Fprintf(&b, "\nequivalent group classes (explored more than once): %d\n", len(census.EquivalentGroups))
	for _, class := range census.EquivalentGroups {
		b.WriteString("  ")
		for _, group := range class {
			fmt.Fprintf(&b, " #%d(%d members, %s)", group.ID, group.Members, group.Example)
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
