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
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
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

// GuardiANN through the record layer: the vector maintainer's own state (the
// task counts and merge lock in the index secondary subspace) beside the
// engine's, compared byte for byte after every save and delete.
var _ = Describe("GuardiANN index persisted bytes through the record layer", func() {
	for _, autoMerge := range []bool{false, true} {
		autoMerge := autoMerge
		It(fmt.Sprintf("are the same in both engines (autoMergeDuringCommit=%v)", autoMerge), func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_record_bytes_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })

			var ops []guardiannOp
			for i := int64(0); i < 14; i++ {
				ops = append(ops, guardiannOp{"save", i, 0.01 * float64(i), 0.02 * float64(i%3)})
			}
			for i := int64(100); i < 104; i++ {
				ops = append(ops, guardiannOp{"save", i, 100 + 0.01*float64(i), 100.0})
			}
			ops = append(ops, guardiannOp{"delete", int64(2), 0.0, 0.0}, guardiannOp{"delete", int64(101), 0.0, 0.0})

			javaSS, goSS := env.Keyspace.Sub("java"), env.Keyspace.Sub("go")
			var javaSteps []guardiannStep
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannRecordByteProbe", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(javaSS.Bytes()), "autoMerge": autoMerge, "ops": ops,
			}, &javaSteps)).To(Succeed())

			index := recordlayer.NewVectorIndex("gv", recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), 2)
			for k, v := range map[string]string{
				recordlayer.IndexOptionVectorEngine:                     "GUARDIANN",
				recordlayer.IndexOptionVectorMetric:                     "EUCLIDEAN_METRIC",
				recordlayer.IndexOptionGuardiannPrimaryClusterMin:       "0",
				recordlayer.IndexOptionGuardiannPrimaryClusterMax:       "10",
				recordlayer.IndexOptionGuardiannPrimaryClusterHardMax:   "40",
				recordlayer.IndexOptionGuardiannCollapseMinDuplicates:   "5",
				recordlayer.IndexOptionGuardiannDeterministicRandomness: "true",
			} {
				index.Options[k] = v
			}
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			open := func(rc *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
				return recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(goSS).CreateOrOpen()
			}
			var goSteps []guardiannStep
			for _, op := range ops {
				id, x, y := op[1].(int64), op[2].(float64), op[3].(float64)
				_, err := env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := open(rc)
					if err != nil {
						return nil, err
					}
					store.GetIndexDeferredMaintenanceControl().SetAutoMergeDuringCommit(autoMerge)
					if op[0] == "save" {
						_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), VectorData: recordlayer.SerializeVector([]float64{x, y})})
					} else {
						_, err = store.DeleteRecord(tuple.Tuple{id})
					}
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				var kvs [][]string
				_, err = env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := open(rc)
					if err != nil {
						return nil, err
					}
					kvs = nil
					for _, part := range []subspace.Subspace{store.IndexSubspace(index), store.IndexSecondarySubspace(index)} {
						r, err := fdb.PrefixRange(part.Bytes())
						if err != nil {
							return nil, err
						}
						got, err := rc.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
						if err != nil {
							return nil, err
						}
						for _, kv := range got {
							key := kv.Key[len(goSS.Bytes()):]
							kvs = append(kvs, []string{fmt.Sprintf("%q", key), hex.EncodeToString(key), hex.EncodeToString(kv.Value)})
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				goSteps = append(goSteps, guardiannStep{Op: fmt.Sprintf("%s %d", op[0], id), KVs: kvs})
			}
			Expect(len(goSteps)).To(Equal(len(javaSteps)))
			for i := range goSteps {
				Expect(guardiannStepDiffs(javaSteps[i].KVs, goSteps[i].KVs)).To(BeEmpty(), "after step %d (%s)", i+1, goSteps[i].Op)
			}
			Expect(javaSteps[len(javaSteps)-1].KVs).NotTo(BeEmpty())
		})
	}
})

// guardiannStepDiffs lists the keys only one engine wrote and the values that
// differ, keys in the engines' printable form.
func guardiannStepDiffs(javaKVs, goKVs [][]string) []string {
	var diffs []string
	javaByKey := map[string]string{}
	for _, kv := range javaKVs {
		javaByKey[kv[1]] = kv[2]
	}
	goByKey := map[string]string{}
	for _, kv := range goKVs {
		goByKey[kv[1]] = kv[2]
		if jv, ok := javaByKey[kv[1]]; !ok {
			diffs = append(diffs, "only Go:   "+kv[0]+" = "+kv[2])
		} else if jv != kv[2] {
			diffs = append(diffs, "value: "+kv[0]+"\n      java "+jv+"\n      go   "+kv[2])
		}
	}
	for _, kv := range javaKVs {
		if _, ok := goByKey[kv[1]]; !ok {
			diffs = append(diffs, "only Java: "+kv[0]+" = "+kv[2])
		}
	}
	return diffs
}

// guardiannOp is one step of a scripted byte differential: insert or delete
// (id, x, y), or drain the task queue one task per transaction.
type guardiannOp []any

func gInsert(id int64, x, y float64) guardiannOp { return guardiannOp{"insert", id, x, y} }
func gDelete(id int64, x, y float64) guardiannOp { return guardiannOp{"delete", id, x, y} }
func gDrain() guardiannOp                        { return guardiannOp{"drain"} }

type guardiannStep struct {
	Op      string     `json:"op"`
	KVs     [][]string `json:"kvs"`
	Trained bool       `json:"trained"` // Java only: AccessInfo.canUseRaBitQ after the step
}

// runGuardiannByteScript replays ops through Java's Guardiann and Go's engine
// (the same deterministic configuration) and returns the first step whose
// persisted bytes differ, as readable diffs, or nil when every step matches.
func runGuardiannByteScript(primaryClusterMin int, ops []guardiannOp) []string {
	diffs, _ := runGuardiannByteScriptWith(primaryClusterMin, nil, ops)
	return diffs
}

// guardiannScriptOptions maps the script probe's option names to the GuardiANN
// index options that set the same Config fields.
var guardiannScriptOptions = map[string]string{
	"useRaBitQ":                    recordlayer.IndexOptionHNSWUseRaBitQ,
	"raBitQNumExBits":              recordlayer.IndexOptionHNSWRaBitQNumExBits,
	"sampleVectorStatsProbability": recordlayer.IndexOptionHNSWSampleVectorStatsProbability,
	"maintainStatsProbability":     recordlayer.IndexOptionHNSWMaintainStatsProbability,
	"statsThreshold":               recordlayer.IndexOptionHNSWStatsThreshold,
}

// runGuardiannByteScriptWith is runGuardiannByteScript with extra Config
// options (guardiannScriptOptions' names) applied to both engines. trained
// reports whether Java's structure ended trained for RaBitQ.
func runGuardiannByteScriptWith(primaryClusterMin int, options map[string]string, ops []guardiannOp) (diffs []string, trained bool) {
	if options == nil {
		options = map[string]string{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_script_"+uuid.New().String())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })

	javaSS, goSS := env.Keyspace.Sub("java"), env.Keyspace.Sub("go")
	var javaSteps []guardiannStep
	Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannByteScriptProbe", map[string]any{
		"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
		"subspace": BytesToIntArray(javaSS.Bytes()), "primaryClusterMin": primaryClusterMin,
		"options": options, "ops": ops,
	}, &javaSteps)).To(Succeed())

	indexOptions := map[string]string{
		recordlayer.IndexOptionVectorEngine:                     "GUARDIANN",
		recordlayer.IndexOptionVectorNumDimensions:              "2",
		recordlayer.IndexOptionVectorMetric:                     "EUCLIDEAN_METRIC",
		recordlayer.IndexOptionGuardiannPrimaryClusterMin:       fmt.Sprint(primaryClusterMin),
		recordlayer.IndexOptionGuardiannPrimaryClusterMax:       "10",
		recordlayer.IndexOptionGuardiannPrimaryClusterHardMax:   "40",
		recordlayer.IndexOptionGuardiannCollapseMinDuplicates:   "5",
		recordlayer.IndexOptionGuardiannDeterministicRandomness: "true",
	}
	for k, v := range options {
		name, ok := guardiannScriptOptions[k]
		Expect(ok).To(BeTrue(), "unknown script option %s", k)
		indexOptions[name] = v
	}
	engine, err := recordlayer.NewGuardiannEngine(goSS, indexOptions)
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
	if len(javaSteps) > 0 {
		trained = javaSteps[len(javaSteps)-1].Trained
	}
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
			return append([]string{fmt.Sprintf("first difference at step %d (%s)", i+1, goSteps[i].Op)}, diffs...), trained
		}
	}
	fmt.Fprintf(GinkgoWriter, "GUARDIANN-SCRIPT %d steps identical\n", len(goSteps))
	return nil, trained
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

	It("RaBitQ: sampling trains the quantizer and later references are encoded", func() {
		options := map[string]string{
			"useRaBitQ": "true", "raBitQNumExBits": "4",
			"sampleVectorStatsProbability": "1.0", "maintainStatsProbability": "1.0", "statsThreshold": "8",
		}
		var ops []guardiannOp
		for i := int64(0); i < 30; i++ {
			// A deterministic spread over two lobes, so splits and training both
			// see varied geometry.
			x := float64(i%7)*0.37 + float64(i/7)*0.11
			y := float64(i%5)*0.23 - float64(i%3)*0.41
			if i%2 == 1 {
				x, y = x+40, y-25
			}
			ops = append(ops, gInsert(i, x, y))
			if i%8 == 7 {
				ops = append(ops, gDrain())
			}
		}
		ops = append(ops, gDrain(), gDelete(4, float64(4%7)*0.37, float64(4%5)*0.23-float64(4%3)*0.41), gDrain())
		diffs, trained := runGuardiannByteScriptWith(0, options, ops)
		Expect(diffs).To(BeEmpty())
		Expect(trained).To(BeTrue(), "the scenario must reach RaBitQ training, or it pins no encoded codec")
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
