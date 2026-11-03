//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/keyspace"
)

// A KeySpacePath's tuple and subspace are Java's bytes, directory-layer nodes
// included, and the global directory layer maps a name to the same long
// whichever engine created the mapping (KeySpaceSteps, keyspace_conformance.java).
var _ = Describe("KeySpace path and directory layer conformance", func() {
	ctx := context.Background()

	It("packs paths, nests subspaces and resolves names as Java", func() {
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		java := NewJavaInvoker()

		// keyspace_conformance.java keySpace().
		mustAdd := func(parent *keyspace.Directory, children ...*keyspace.Directory) *keyspace.Directory {
			Expect(parent.AddSubdirectories(children...)).To(Succeed())
			return parent
		}
		root := mustAdd(keyspace.NewDirectory("/", keyspace.KeyTypeNull),
			mustAdd(recordlayer.DirectoryLayerDirectory("application", db),
				mustAdd(recordlayer.DirectoryLayerDirectory("run", db),
					keyspace.NewConstantDirectory("store", keyspace.KeyTypeLong, int64(1)),
					mustAdd(keyspace.NewDirectory("name", keyspace.KeyTypeString),
						keyspace.NewDirectory("part", keyspace.KeyTypeLong)))),
			mustAdd(keyspace.NewDirectory("plain", keyspace.KeyTypeString),
				mustAdd(keyspace.NewDirectory("id", keyspace.KeyTypeLong),
					keyspace.NewDirectory("nothing", keyspace.KeyTypeNull))))
		ks := keyspace.NewKeySpace(root)
		Expect(ks.Validate()).To(Succeed())

		id := uuid.New().String()
		app, javaRun, goRun, scopedRun := "ks_app_"+id, "ks_java_"+id, "ks_go_"+id, "ks_scoped_"+id
		// Each path is a chain whose every prefix is also in the list, so
		// containment is checked level by level.
		chain := func(run string) [][][]any {
			return [][][]any{
				{{"application", app}},
				{{"application", app}, {"run", run}},
				{{"application", app}, {"run", run}, {"store"}},
				{{"application", app}, {"run", run}, {"name", "n\x001"}},
				{{"application", app}, {"run", run}, {"name", "n\x001"}, {"part", int64(-7)}},
			}
		}
		plain := [][][]any{
			{{"plain", "p"}},
			{{"plain", "p"}, {"id", int64(300)}},
			{{"plain", "p"}, {"id", int64(300)}, {"nothing"}},
		}
		type packed struct {
			Tuple    string `json:"tuple"`
			Subspace string `json:"subspace"`
		}
		goPaths := func(paths [][][]any) []packed {
			var out []packed
			for _, steps := range paths {
				var p *keyspace.Path
				var err error
				for _, step := range steps {
					var value any
					if len(step) > 1 {
						value = step[1]
					}
					if p == nil {
						p, err = ks.Path(ctx, step[0].(string), value)
					} else {
						p, err = p.Add(ctx, step[0].(string), value)
					}
					Expect(err).NotTo(HaveOccurred(), "%v", steps)
				}
				out = append(out, packed{Tuple: hex.EncodeToString(p.ToTuple().Pack()), Subspace: hex.EncodeToString(p.ToSubspace().Bytes())})
			}
			return out
		}
		javaPaths := func(paths [][][]any) []packed {
			pathsJSON, err := json.Marshal(paths)
			Expect(err).NotTo(HaveOccurred())
			var out []packed
			Expect(java.InvokeAs(ctx, "keySpacePaths", map[string]any{"clusterFile": clusterFile, "pathsJson": string(pathsJSON)}, &out)).To(Succeed())
			return out
		}
		expectNested := func(got []packed, paths [][][]any) {
			for i, p := range got {
				Expect(p.Subspace).To(Equal(p.Tuple), "a subspace's prefix is its packed tuple: %v", paths[i])
				if i > 0 && len(paths[i]) == len(paths[i-1])+1 {
					Expect(strings.HasPrefix(p.Subspace, got[i-1].Subspace)).To(BeTrue(), "%v inside %v", paths[i], paths[i-1])
					Expect(len(p.Subspace)).To(BeNumerically(">", len(got[i-1].Subspace)))
				}
			}
		}

		// Java creates the application and its run; Go reads its mappings.
		first := append(chain(javaRun), plain...)
		javaFirst := javaPaths(first)
		expectNested(javaFirst, first)
		Expect(goPaths(first)).To(Equal(javaFirst))

		// Go creates a run; Java reads it.
		second := chain(goRun)
		goSecond := goPaths(second)
		expectNested(goSecond, second)
		Expect(javaPaths(second)).To(Equal(goSecond))

		// A mapping ScopedDirectoryLayer.global created (through the FDB
		// directory layer) is the one ExtendedDirectoryLayer.global reads.
		scoped, err := recordlayer.NewKeySpaceResolver(db, recordlayer.GlobalDirectoryLayer{}).Resolve(ctx, scopedRun)
		Expect(err).NotTo(HaveOccurred())

		names := []string{app, javaRun, goRun, scopedRun}
		namesJSON, err := json.Marshal(names)
		Expect(err).NotTo(HaveOccurred())
		var javaValues []int64
		Expect(java.InvokeAs(ctx, "directoryLayerResolve", map[string]any{"clusterFile": clusterFile, "namesJson": string(namesJSON)}, &javaValues)).To(Succeed())
		Expect(javaValues[3]).To(Equal(scoped))
		for _, scope := range []recordlayer.LocatableResolver{recordlayer.ExtendedDirectoryLayerGlobal{}, recordlayer.GlobalDirectoryLayer{}} {
			r := recordlayer.NewKeySpaceResolver(db, scope)
			for i, name := range names {
				v, err := r.Resolve(ctx, name)
				Expect(err).NotTo(HaveOccurred())
				Expect(v).To(Equal(javaValues[i]), "%T %s", scope, name)
			}
		}

		// Reverse lookups agree, a value with no mapping included.
		const unmapped = int64(1) << 60
		values := append(append([]int64{}, javaValues...), unmapped)
		valuesJSON, err := json.Marshal(values)
		Expect(err).NotTo(HaveOccurred())
		var javaNames []*string
		Expect(java.InvokeAs(ctx, "directoryLayerReverseLookup", map[string]any{"clusterFile": clusterFile, "valuesJson": string(valuesJSON)}, &javaNames)).To(Succeed())
		Expect(javaNames).To(HaveLen(len(values)))
		r := recordlayer.NewKeySpaceResolver(db, recordlayer.ExtendedDirectoryLayerGlobal{})
		for i, v := range values {
			name, ok, err := r.ReverseLookup(ctx, v)
			Expect(err).NotTo(HaveOccurred())
			if javaNames[i] == nil {
				Expect(ok).To(BeFalse(), "%d", v)
				continue
			}
			Expect(ok).To(BeTrue(), "%d", v)
			Expect(name).To(Equal(*javaNames[i]))
			Expect(name).To(Equal(names[i]))
		}
	})
})
