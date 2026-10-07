//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// GuardiANN persisted-byte differential (RFC-257 WS-D: "compare persisted
// deterministic bytes exactly"). Java's Guardiann and Go's engine run the same
// insert / drain / delete / drain scenario under deterministic randomness, and
// every key and value under their subspaces must be identical: vector
// identities, cluster metadata, references, centroid HNSW, task queue and
// counts.
var _ = Describe("GuardiANN persisted bytes", func() {
	// The KMeans input order is HashMap<VectorId, VectorReference>.values(),
	// filled by compute (Primitives.cleanUpVectorReferences); Go ports the
	// traversal. Pinned against a real HashMap on both sides of compute's
	// resize points (14 and 50 ids are the first to grow the table to 32 and
	// 128; put would grow it at 13 and 49) and across bucket collisions, whose
	// keys compute links at the head of the bin.
	It("orders vector ids as a real HashMap<VectorId, ...> iterates them", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, n := range []int{5, 12, 13, 14, 25, 49, 50, 200} {
			pks := make([]tuple.Tuple, n)
			uuids := make([]tuple.UUID, n)
			packed := make([]string, n)
			uuidStrings := make([]string, n)
			for i := range pks {
				pks[i] = tuple.Tuple{int64(i * 7919 % 1000)}
				u := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint(n, i)))
				copy(uuids[i][:], u[:])
				packed[i] = hex.EncodeToString(pks[i].Pack())
				uuidStrings[i] = u.String()
			}
			var result struct {
				HashMapOrder []int `json:"hashMapOrder"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannVectorIdHashProbe", map[string]any{
				"packedPrimaryKeysHex": packed, "uuids": uuidStrings,
			}, &result)).To(Succeed())
			Expect(recordlayer.GuardiannVectorIDHashMapOrder(pks, uuids)).To(Equal(result.HashMapOrder), "n=%d", n)
		}
	})

	It("are the same in both engines for one insert, split and delete scenario", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_bytes_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })

		javaSS := env.Keyspace.Sub("java")
		var java struct {
			BuildExecutions  []byteExecution `json:"buildExecutions"`
			DeleteExecutions []byteExecution `json:"deleteExecutions"`
			KVsAfterInserts  [][]string      `json:"kvsAfterInserts"`
			BuildRounds      [][][]string    `json:"buildRounds"`
			KVsAfterBuild    [][]string      `json:"kvsAfterBuild"`
			KVsAfterDeletes  [][]string      `json:"kvsAfterDeletes"`
			KVs              [][]string      `json:"kvs"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannByteDumpProbe", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(javaSS.Bytes()),
		}, &java)).To(Succeed())

		goSS := env.Keyspace.Sub("go")
		engine, err := recordlayer.NewGuardiannEngine(goSS, map[string]string{
			recordlayer.IndexOptionVectorEngine:                     "GUARDIANN",
			recordlayer.IndexOptionVectorNumDimensions:              "2",
			recordlayer.IndexOptionVectorMetric:                     "EUCLIDEAN_METRIC",
			recordlayer.IndexOptionGuardiannPrimaryClusterMin:       "0",
			recordlayer.IndexOptionGuardiannPrimaryClusterMax:       "10",
			recordlayer.IndexOptionGuardiannPrimaryClusterHardMax:   "40",
			recordlayer.IndexOptionGuardiannCollapseMinDuplicates:   "5",
			recordlayer.IndexOptionGuardiannDeterministicRandomness: "true",
		})
		Expect(err).NotTo(HaveOccurred())
		run := func(fn func(tx fdb.WritableTransaction) error) error {
			_, err := env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				return nil, fn(rc.Transaction())
			})
			return err
		}
		near := func(id int) []float64 { return []float64{0.01 * float64(id), 0.02 * float64(id%3)} }
		far := func(offset int) []float64 { return []float64{100.0 + 0.01*float64(offset), 100.0} }
		write := func(remove bool, id int64, v []float64) {
			Expect(run(func(tx fdb.WritableTransaction) error {
				if remove {
					return engine.Delete(tx, tuple.Tuple{id}, v, false)
				}
				return engine.Insert(tx, tuple.Tuple{id}, v, false)
			})).To(Succeed())
		}
		drain := func() []byteExecution {
			var out []byteExecution
			failures := 0
			for round := 0; round < 40 && failures < 3; round++ {
				var executed int
				err := run(func(tx fdb.WritableTransaction) error {
					var err error
					executed, err = engine.ExecuteDeferredTasks(tx, 1)
					return err
				})
				if err != nil {
					out = append(out, byteExecution{Exception: err.Error()})
					failures++
					continue
				}
				out = append(out, byteExecution{Executed: &executed})
				if executed == 0 {
					break
				}
			}
			return out
		}
		for i := 0; i < 12; i++ {
			write(false, int64(i), near(i))
		}
		for i := 0; i < 3; i++ {
			write(false, int64(1000+i), far(i))
		}
		dump := func() [][]string {
			var out [][]string
			Expect(run(func(tx fdb.WritableTransaction) error {
				r, err := fdb.PrefixRange(goSS.Bytes())
				if err != nil {
					return err
				}
				kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
				out = nil
				for _, kv := range kvs {
					key := kv.Key[len(goSS.Bytes()):]
					out = append(out, []string{fmt.Sprintf("%q", key), hex.EncodeToString(key), hex.EncodeToString(kv.Value)})
				}
				return err
			})).To(Succeed())
			return out
		}
		goAfterInserts := dump()
		var goRounds [][][]string
		for round := 0; round < 40; round++ {
			var executed int
			Expect(run(func(tx fdb.WritableTransaction) error {
				var err error
				executed, err = engine.ExecuteDeferredTasks(tx, 1)
				return err
			})).To(Succeed())
			if executed == 0 {
				break
			}
			goRounds = append(goRounds, dump())
		}
		goBuild := drain()
		goAfterBuild := dump()
		write(true, 3, near(3))
		write(true, 1001, far(1))
		goAfterDeletes := dump()
		goDelete := drain()
		goKVs := dump()

		summary := func(es []byteExecution) []string {
			var s []string
			for _, e := range es {
				if e.Executed != nil {
					s = append(s, fmt.Sprintf("executed=%d", *e.Executed))
				} else {
					s = append(s, "failed")
				}
			}
			return s
		}
		Expect(summary(goBuild)).To(Equal(summary(java.BuildExecutions)), "build drain")
		Expect(summary(goDelete)).To(Equal(summary(java.DeleteExecutions)), "delete drain")

		var diffs []string
		compare := func(phase string, javaKVs, goKVs [][]string) {
			javaByKey := map[string]string{}
			for _, kv := range javaKVs {
				javaByKey[kv[1]] = kv[2]
			}
			goByKey := map[string]string{}
			for _, kv := range goKVs {
				goByKey[kv[1]] = kv[2]
				if jv, ok := javaByKey[kv[1]]; !ok {
					diffs = append(diffs, phase+" only Go:   "+kv[0]+" = "+kv[2])
				} else if jv != kv[2] {
					diffs = append(diffs, phase+" value: "+kv[0]+"\n      java "+jv+"\n      go   "+kv[2])
				}
			}
			for _, kv := range javaKVs {
				if _, ok := goByKey[kv[1]]; !ok {
					diffs = append(diffs, phase+" only Java: "+kv[0]+" = "+kv[2])
				}
			}
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-BYTES %s java=%d go=%d keys\n", phase, len(javaKVs), len(goKVs))
			Expect(javaKVs).NotTo(BeEmpty())
		}
		compare("after inserts", java.KVsAfterInserts, goAfterInserts)
		Expect(len(goRounds)).To(Equal(len(java.BuildRounds)), "build tasks executed")
		for i := range goRounds {
			compare(fmt.Sprintf("after build task %d", i+1), java.BuildRounds[i], goRounds[i])
		}
		compare("after build", java.KVsAfterBuild, goAfterBuild)
		compare("after deletes", java.KVsAfterDeletes, goAfterDeletes)
		compare("after delete drain", java.KVs, goKVs)
		Expect(diffs).To(BeEmpty())
	})
})

// byteExecution is one drain round of guardiannByteDumpProbe.
type byteExecution struct {
	Executed  *int   `json:"executed"`
	Exception string `json:"exception"`
	Site      string `json:"site"`
}

// guardiannOp is one step of a scripted byte differential: insert or delete
// (id, x, y), or drain the task queue one task per transaction.
type guardiannOp []any

func gInsert(id int64, x, y float64) guardiannOp { return guardiannOp{"insert", id, x, y} }
func gDelete(id int64, x, y float64) guardiannOp { return guardiannOp{"delete", id, x, y} }
func gDrain() guardiannOp                        { return guardiannOp{"drain"} }

type guardiannStep struct {
	Op  string     `json:"op"`
	KVs [][]string `json:"kvs"`
}

// runGuardiannByteScript replays ops through Java's Guardiann and Go's engine
// (the same deterministic configuration) and returns the first step whose
// persisted bytes differ, as readable diffs, or nil when every step matches.
func runGuardiannByteScript(primaryClusterMin int, ops []guardiannOp) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_script_"+uuid.New().String())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })

	javaSS, goSS := env.Keyspace.Sub("java"), env.Keyspace.Sub("go")
	var javaSteps []guardiannStep
	Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannByteScriptProbe", map[string]any{
		"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
		"subspace": BytesToIntArray(javaSS.Bytes()), "primaryClusterMin": primaryClusterMin, "ops": ops,
	}, &javaSteps)).To(Succeed())

	engine, err := recordlayer.NewGuardiannEngine(goSS, map[string]string{
		recordlayer.IndexOptionVectorEngine:                     "GUARDIANN",
		recordlayer.IndexOptionVectorNumDimensions:              "2",
		recordlayer.IndexOptionVectorMetric:                     "EUCLIDEAN_METRIC",
		recordlayer.IndexOptionGuardiannPrimaryClusterMin:       fmt.Sprint(primaryClusterMin),
		recordlayer.IndexOptionGuardiannPrimaryClusterMax:       "10",
		recordlayer.IndexOptionGuardiannPrimaryClusterHardMax:   "40",
		recordlayer.IndexOptionGuardiannCollapseMinDuplicates:   "5",
		recordlayer.IndexOptionGuardiannDeterministicRandomness: "true",
	})
	Expect(err).NotTo(HaveOccurred())
	run := func(fn func(tx fdb.WritableTransaction) error) error {
		_, err := env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			return nil, fn(rc.Transaction())
		})
		return err
	}
	dump := func() [][]string {
		var out [][]string
		Expect(run(func(tx fdb.WritableTransaction) error {
			r, err := fdb.PrefixRange(goSS.Bytes())
			if err != nil {
				return err
			}
			kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
			out = nil
			for _, kv := range kvs {
				key := kv.Key[len(goSS.Bytes()):]
				out = append(out, []string{fmt.Sprintf("%q", key), hex.EncodeToString(key), hex.EncodeToString(kv.Value)})
			}
			return err
		})).To(Succeed())
		return out
	}
	var goSteps []guardiannStep
	for _, op := range ops {
		if op[0] == "drain" {
			for round := 0; round < 40; round++ {
				var executed int
				err := run(func(tx fdb.WritableTransaction) error {
					var err error
					executed, err = engine.ExecuteDeferredTasks(tx, 1)
					return err
				})
				if err != nil {
					goSteps = append(goSteps, guardiannStep{Op: "task failed"})
					break
				}
				if executed == 0 {
					break
				}
				goSteps = append(goSteps, guardiannStep{Op: "task", KVs: dump()})
			}
			continue
		}
		id, x, y := op[1].(int64), op[2].(float64), op[3].(float64)
		Expect(run(func(tx fdb.WritableTransaction) error {
			if op[0] == "insert" {
				return engine.Insert(tx, tuple.Tuple{id}, []float64{x, y}, false)
			}
			return engine.Delete(tx, tuple.Tuple{id}, []float64{x, y}, false)
		})).To(Succeed())
		goSteps = append(goSteps, guardiannStep{Op: fmt.Sprintf("%s %d", op[0], id), KVs: dump()})
	}

	ops2 := func(steps []guardiannStep) []string {
		var s []string
		for _, st := range steps {
			s = append(s, st.Op)
		}
		return s
	}
	Expect(ops2(goSteps)).To(Equal(ops2(javaSteps)), "the steps each engine took")
	for i := range goSteps {
		var diffs []string
		javaByKey := map[string]string{}
		for _, kv := range javaSteps[i].KVs {
			javaByKey[kv[1]] = kv[2]
		}
		goByKey := map[string]string{}
		for _, kv := range goSteps[i].KVs {
			goByKey[kv[1]] = kv[2]
			if jv, ok := javaByKey[kv[1]]; !ok {
				diffs = append(diffs, "only Go:   "+kv[0]+" = "+kv[2])
			} else if jv != kv[2] {
				diffs = append(diffs, "value: "+kv[0]+"\n      java "+jv+"\n      go   "+kv[2])
			}
		}
		for _, kv := range javaSteps[i].KVs {
			if _, ok := goByKey[kv[1]]; !ok {
				diffs = append(diffs, "only Java: "+kv[0]+" = "+kv[2])
			}
		}
		if len(diffs) > 0 {
			return append([]string{fmt.Sprintf("first difference at step %d (%s)", i+1, goSteps[i].Op)}, diffs...)
		}
	}
	fmt.Fprintf(GinkgoWriter, "GUARDIANN-SCRIPT %d steps identical\n", len(goSteps))
	return nil
}

var _ = Describe("GuardiANN persisted bytes by scenario", func() {
	near := func(id int64) (int64, float64, float64) { return id, 0.01 * float64(id), 0.02 * float64(id%3) }
	far := func(id int64) (int64, float64, float64) {
		return id, 100.0 + 0.01*float64(id-1000), 100.0
	}

	It("merge: deleting most of a cluster merges it into its neighbour", func() {
		var ops []guardiannOp
		for i := int64(0); i < 6; i++ {
			ops = append(ops, gInsert(near(i)))
		}
		for i := int64(1000); i < 1006; i++ {
			ops = append(ops, gInsert(far(i)))
		}
		ops = append(ops, gDrain())
		for i := int64(1000); i < 1004; i++ {
			ops = append(ops, gDelete(far(i)))
		}
		ops = append(ops, gDrain())
		Expect(runGuardiannByteScript(3, ops)).To(BeEmpty())
	})

	It("collapse: a cluster of duplicates collapses instead of splitting", func() {
		var ops []guardiannOp
		for i := int64(0); i < 14; i++ {
			ops = append(ops, gInsert(i, 0.5, 0.5))
		}
		ops = append(ops, gDrain())
		ops = append(ops, gDelete(3, 0.5, 0.5), gDelete(7, 0.5, 0.5))
		ops = append(ops, gDrain())
		Expect(runGuardiannByteScript(0, ops)).To(BeEmpty())
	})

	It("split and reassign: a growing cluster splits beside a neighbour", func() {
		var ops []guardiannOp
		for i := int64(0); i < 4; i++ {
			ops = append(ops, gInsert(far(1000+i)))
		}
		for i := int64(0); i < 24; i++ {
			ops = append(ops, gInsert(near(i)))
			if i%6 == 5 {
				ops = append(ops, gDrain())
			}
		}
		ops = append(ops, gDrain())
		Expect(runGuardiannByteScript(0, ops)).To(BeEmpty())
	})
})
