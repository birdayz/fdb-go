package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type nonPointerSubsumptionValue struct{ values.Value }

func subsumptionValueChain(depth int, leaf values.Value) values.Value {
	for range depth {
		leaf = values.NewScalarFunctionValue("ABS", values.NotNullLong, leaf)
	}
	return leaf
}

func TestSelectSubsumptionValueTreeValidation(t *testing.T) {
	t.Parallel()
	leaf := &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	var typedNil *values.ScalarFunctionValue
	selfCycle := values.NewScalarFunctionValue("ABS", values.NotNullLong)
	selfCycle.Args = []values.Value{selfCycle}
	cycleLeft := values.NewScalarFunctionValue("ABS", values.NotNullLong)
	cycleRight := values.NewScalarFunctionValue("ABS", values.NotNullLong, cycleLeft)
	cycleLeft.Args = []values.Value{cycleRight}
	shared := subsumptionValueChain(2, leaf)
	wide := values.NewScalarFunctionValue("COALESCE", values.NotNullLong)
	for range 128 {
		wide.Args = append(wide.Args, shared)
	}
	for _, tt := range []struct {
		name string
		root values.Value
		want bool
	}{
		{"nil", nil, false},
		{"typed nil", typedNil, false},
		{"non-pointer", nonPointerSubsumptionValue{leaf}, false},
		{"leaf", leaf, true},
		{"shared DAG", values.NewScalarFunctionValue("COALESCE", values.NotNullLong, shared, shared), true},
		{"wide DAG", wide, true},
		{"deep chain", subsumptionValueChain(1024, leaf), true},
		{"self cycle", selfCycle, false},
		{"mutual cycle", cycleLeft, false},
		{"deep cycle", subsumptionValueChain(1024, selfCycle), false},
		{"deep nil", subsumptionValueChain(1024, nil), false},
		{"deep typed nil", subsumptionValueChain(1024, typedNil), false},
		{"deep non-pointer", subsumptionValueChain(1024, nonPointerSubsumptionValue{leaf}), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := selectSubsumptionValueTreeWellFormed(tt.root); got != tt.want {
				t.Fatalf("well formed = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestSelectSubsumptionValueTreeRechecksMutations(t *testing.T) {
	t.Parallel()
	leaf := &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	root := values.NewScalarFunctionValue("ABS", values.NotNullLong, leaf)
	for _, child := range []values.Value{leaf, root, leaf, (*values.ConstantValue)(nil), leaf, nil, leaf} {
		root.Args[0] = child
		if got, want := selectSubsumptionValueTreeWellFormed(root), child == leaf; got != want {
			t.Fatalf("child %T: well formed = %t, want %t", child, got, want)
		}
	}
}

func FuzzSelectSubsumptionValueTree(f *testing.F) {
	f.Add([]byte{0})
	f.Add([]byte{1, 1, 1, 0})
	f.Add([]byte{3, 1, 2, 3, 1, 3, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		t.Parallel()
		if len(data) == 0 {
			return
		}
		n := min(len(data), 40)
		nodes := make([]*values.ScalarFunctionValue, n)
		for i := range nodes {
			nodes[i] = values.NewScalarFunctionValue("COALESCE", values.NotNullLong)
		}
		edges := make([][]int, n)
		for i, node := range nodes {
			for j := range int(data[i] % 5) {
				child := int(data[(i+j+1)%len(data)]) % (n + 2)
				edges[i] = append(edges[i], child)
				switch child {
				case n:
					node.Args = append(node.Args, nil)
				case n + 1:
					node.Args = append(node.Args, (*values.ScalarFunctionValue)(nil))
				default:
					node.Args = append(node.Args, nodes[child])
				}
			}
		}
		// An independent index graph checks reachability and path-local cycles.
		active := make([]bool, n)
		done := make([]bool, n)
		var valid func(int) bool
		valid = func(index int) bool {
			if index >= n || active[index] {
				return false
			}
			if done[index] {
				return true
			}
			active[index] = true
			for _, child := range edges[index] {
				if !valid(child) {
					return false
				}
			}
			active[index], done[index] = false, true
			return true
		}
		if got, want := selectSubsumptionValueTreeWellFormed(nodes[0]), valid(0); got != want {
			t.Fatalf("edges %v: well formed = %t, want %t", edges, got, want)
		}
	})
}

func BenchmarkSelectSubsumptionValueTree(b *testing.B) {
	for _, depth := range []int{1, 4, 32, 128} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			root := subsumptionValueChain(depth, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong})
			if depth <= 4 {
				allocs := testing.AllocsPerRun(100, func() {
					if !selectSubsumptionValueTreeWellFormed(root) {
						b.Fatal("valid chain rejected")
					}
				})
				if allocs != 0 {
					b.Fatalf("small value-tree validation allocated scratch: %g allocations, want 0", allocs)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if !selectSubsumptionValueTreeWellFormed(root) {
					b.Fatal("valid chain rejected")
				}
			}
		})
	}
}
