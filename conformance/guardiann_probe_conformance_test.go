//go:build bazelrunfiles

package conformance_test

import (
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

//go:embed testdata/probes/guardiann_pins.json
var guardiannPinsJSON []byte

type guardiannPinSet struct {
	Knob         map[string]string `json:"knob"`
	Inline       map[string]string `json:"inline"`
	Balanced     []string          `json:"balanced"`
	Peel         []string          `json:"peel"`
	PeelHD       []string          `json:"peelHD"`
	PeelGen      []string          `json:"peelGen"`
	PeelCorner   []string          `json:"peelCorner"`
	Shape        []string          `json:"shape"`
	Obsolete     []string          `json:"obsolete"`
	RecordInline []string          `json:"recordInline"`
}

var guardiannPins = func() guardiannPinSet {
	var out guardiannPinSet
	if err := json.Unmarshal(guardiannPinsJSON, &out); err != nil {
		panic(fmt.Sprintf("decode guardiann_pins.json: %v", err))
	}
	return out
}()

// javaArraysHashCode is java.util.Arrays.hashCode(byte[]): a 31-polynomial over
// SIGNED bytes starting at 1, with int32 wraparound.
func javaArraysHashCode(b []byte) int32 {
	h := int32(1)
	for _, c := range b {
		h = 31*h + int32(int8(c))
	}
	return h
}

// javaUUIDHashCode is java.util.UUID.hashCode.
func javaUUIDHashCode(u uuid.UUID) int32 {
	msb := int64(binary.BigEndian.Uint64(u[:8]))
	lsb := int64(binary.BigEndian.Uint64(u[8:]))
	hilo := msb ^ lsb
	return int32(hilo>>32) ^ int32(hilo)
}

// Live-JVM oracle for the RFC-257 WS-D GuardiANN port. These specs pin what the
// real 4.14.2.0 classes do; the Go engine port must reproduce or deliberately
// diverge from exactly these observations (ws-d-design.md).
var _ = Describe("GuardiANN target oracle", func() {
	It("pins the VectorId hash formula and the runtime it was validated on", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		pks := []tuple.Tuple{
			{int64(0)},
			{int64(-1)},
			{int64(1) << 40},
			{"id-ä"},
			{int64(7), "x", []byte{0xff, 0x00}},
			{tuple.Tuple{int64(3), nil}},
			{true, 2.5},
		}
		uuids := []string{
			"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff",
			"80000000-0000-4000-8000-000000000001", "12345678-9abc-def0-1122-334455667788",
			"7fffffff-ffff-4fff-bfff-ffffffffffff", "0f0e0d0c-0b0a-4908-8706-050403020100",
			"deadbeef-0000-4000-8000-00000000cafe",
		}
		packed := make([]string, len(pks))
		for i, pk := range pks {
			packed[i] = hex.EncodeToString(pk.Pack())
		}
		var result struct {
			Rows []struct {
				JavaHash  int32 `json:"javaHash"`
				TupleHash int32 `json:"tupleHash"`
				UUIDHash  int32 `json:"uuidHash"`
			} `json:"rows"`
			JavaFeatureVersion   int    `json:"javaFeatureVersion"`
			FdbJavaJar           string `json:"fdbJavaJar"`
			FdbExtensionsVersion string `json:"fdbExtensionsVersion"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannVectorIdHashProbe", map[string]any{
			"packedPrimaryKeysHex": packed, "uuids": uuids,
		}, &result)).To(Succeed())
		Expect(result.Rows).To(HaveLen(len(pks)))
		Expect(result.JavaFeatureVersion).To(Equal(21), "the record hashCode formula is validated only on JDK 21")
		Expect(result.FdbJavaJar).To(ContainSubstring("fdb-java-7.1.26"), "Tuple.hashCode is validated on fdb-java 7.1.26 only")
		Expect(result.FdbExtensionsVersion).To(Equal("4.14.2.0"))
		for i, row := range result.Rows {
			u := uuid.MustParse(uuids[i])
			tupleHash := javaArraysHashCode(pks[i].Pack())
			uuidHash := javaUUIDHashCode(u)
			formula := 31*tupleHash + uuidHash
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-VECTORID-HASH pk=%s uuid=%s java=%d go=%d\n", packed[i], uuids[i], row.JavaHash, formula)
			Expect(row.TupleHash).To(Equal(tupleHash), "Tuple.hashCode is Arrays.hashCode(pack())")
			Expect(row.UUIDHash).To(Equal(uuidHash))
			Expect(row.JavaHash).To(Equal(formula), "VectorId.hashCode = 31*Tuple.hashCode + UUID.hashCode")
		}
	})

	type execution struct {
		Executed  *int   `json:"executed"`
		Exception string `json:"exception"`
		Site      string `json:"site"`
	}
	type cluster struct {
		Primaries int `json:"primaries"`
		States    int `json:"states"`
		Peak      int `json:"peak"`
	}
	type splitProbe struct {
		Executions []execution `json:"executions"`
		Clusters   []cluster   `json:"clusters"`
	}
	runSplitProbe := func(near, far int) splitProbe {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_split_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
		var result splitProbe
		Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannSplitCandidateProbe", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(env.Keyspace.Bytes()), "nearCount": near, "farCount": far,
		}, &result)).To(Succeed())
		for i, e := range result.Executions {
			if e.Executed != nil {
				fmt.Fprintf(GinkgoWriter, "GUARDIANN-SPLIT near=%d far=%d round=%d executed=%d\n", near, far, i, *e.Executed)
			} else {
				fmt.Fprintf(GinkgoWriter, "GUARDIANN-SPLIT near=%d far=%d round=%d exception=%s site=%s\n", near, far, i, e.Exception, e.Site)
			}
		}
		for _, c := range result.Clusters {
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-SPLIT near=%d far=%d cluster primaries=%d states=%d peak=%d\n", near, far, c.Primaries, c.States, c.Peak)
		}
		return result
	}

	It("splits an oversized cluster whose far group meets minChildFraction (control)", func() {
		result := runSplitProbe(10, 2)
		Expect(result.Executions).NotTo(BeEmpty())
		for _, e := range result.Executions {
			Expect(e.Exception).To(BeEmpty(), "the control split must not fail")
		}
		Expect(len(result.Clusters)).To(BeNumerically(">=", 2), "a valid split must create clusters")
	})

	// Source hypothesis for the ws-d-design split terminal rule: when every
	// split candidate is INVALID (the far group is under minChildFraction 0.1) and
	// collapse does not apply, target Java throws at orElseThrow and the task keeps
	// failing, so the cluster is never split and its queue never drains.
	It("fails the split task forever when every candidate is under minChildFraction", func() {
		result := runSplitProbe(10, 1)
		// Round 0 is the two-phase neighbour-persistence step: the split task
		// re-enqueues itself at high priority carrying its nearest clusters. Every
		// later execution of that task fails at orElseThrow.
		Expect(result.Executions).To(HaveLen(4), "one persistence step, then three failures")
		Expect(result.Executions[0].Executed).NotTo(BeNil())
		Expect(*result.Executions[0].Executed).To(Equal(1))
		for _, e := range result.Executions[1:] {
			Expect(e.Executed).To(BeNil())
			Expect(e.Exception).To(Equal("java.util.NoSuchElementException"))
			Expect(e.Site).To(And(HavePrefix("SplitMergeTask."), HaveSuffix(":397")), "the orElseThrow site")
		}
		Expect(result.Clusters).To(HaveLen(1))
		Expect(result.Clusters[0].Primaries).To(Equal(11))
		Expect(result.Clusters[0].States&1).To(Equal(1), "SPLIT_MERGE stays set")
	})

	// Source hypothesis for the ws-d-design EXISTING-empty-neighbour rule:
	// SplitMergeTask.writeClusterMetadataAndEnqueueTasks verifies primaries > 0
	// for EVERY cluster in its map, including untouched neighbours, so a split
	// next to a cluster emptied by deletes fails. primaryClusterMin 0 keeps the
	// emptied cluster from being merge-eligible, so the order is deterministic.
	It("records a split beside an emptied neighbour", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_empty_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
		var result struct {
			BuildExecutions  []execution `json:"buildExecutions"`
			AfterBuild       []cluster   `json:"afterBuild"`
			DeleteExecutions []execution `json:"deleteExecutions"`
			AfterDelete      []cluster   `json:"afterDelete"`
			SplitExecutions  []execution `json:"splitExecutions"`
			AfterSplit       []cluster   `json:"afterSplit"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannEmptyNeighbourSplitProbe", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(env.Keyspace.Bytes()),
		}, &result)).To(Succeed())
		log := func(phase string, es []execution, cs []cluster) {
			for i, e := range es {
				if e.Executed != nil {
					fmt.Fprintf(GinkgoWriter, "GUARDIANN-EMPTY %s round=%d executed=%d\n", phase, i, *e.Executed)
				} else {
					fmt.Fprintf(GinkgoWriter, "GUARDIANN-EMPTY %s round=%d exception=%s site=%s\n", phase, i, e.Exception, e.Site)
				}
			}
			for _, c := range cs {
				fmt.Fprintf(GinkgoWriter, "GUARDIANN-EMPTY %s cluster primaries=%d states=%d peak=%d\n", phase, c.Primaries, c.States, c.Peak)
			}
		}
		log("build", result.BuildExecutions, result.AfterBuild)
		log("delete", result.DeleteExecutions, result.AfterDelete)
		log("split", result.SplitExecutions, result.AfterSplit)
		Expect(result.AfterDelete).To(ContainElement(HaveField("Primaries", 0)), "the far cluster must be emptied")
		// Measured target behaviour: after the neighbour-persistence phase every
		// execution of the split fails the primaries>0 verification for the empty
		// neighbour, the task is never consumed, and the cluster stays oversized.
		Expect(result.SplitExecutions).To(HaveLen(4))
		Expect(result.SplitExecutions[0].Executed).NotTo(BeNil())
		for _, e := range result.SplitExecutions[1:] {
			Expect(e.Exception).To(Equal("com.google.common.base.VerifyException"))
			Expect(e.Site).To(HavePrefix("SplitMergeTask.writeClusterMetadataAndEnqueueTasks:"))
		}
		Expect(result.AfterSplit).To(ConsistOf(
			cluster{Primaries: 0, States: 0, Peak: 2},
			cluster{Primaries: 11, States: 1, Peak: 11}))
	})

	// Queue-replay shape of the capacity stranding (ws-d-design section 5): the
	// target drains the whole pending queue before any merge, with deferred
	// maintenance, so once one cluster reaches primaryClusterHardMax (12 here) the
	// drain cannot finish.
	It("records a queued build whose replay crosses the hard cap", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		// The Java OnlineIndexer opens its own non-tenant contexts; the step uses a
		// unique prefix of the shared container and clears it on every exit.
		env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_queue_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
		var result struct {
			StateAfterEmpty  string    `json:"stateAfterEmptyBuild"`
			Saved            int       `json:"saved"`
			SaveFailure      string    `json:"saveFailure"`
			StateBeforeDrain string    `json:"stateBeforeDrain"`
			FailureChain     []string  `json:"failureChain"`
			FailureSite      string    `json:"failureSite"`
			StateAfterDrain  string    `json:"stateAfterDrain"`
			Clusters         []cluster `json:"clusters"`
			QueueEntries     int       `json:"queueEntries"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannQueueCapacityProbe", map[string]any{
			"clusterFile": env.ClusterFile, "records": 30, "markQueue": true,
		}, &result)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "GUARDIANN-QUEUE afterEmptyBuild=%s saved=%d saveFailure=%q\n", result.StateAfterEmpty, result.Saved, result.SaveFailure)
		fmt.Fprintf(GinkgoWriter, "GUARDIANN-QUEUE before=%s after=%s queueEntries=%d chain=%v site=%s\n",
			result.StateBeforeDrain, result.StateAfterDrain, result.QueueEntries, result.FailureChain, result.FailureSite)
		for _, c := range result.Clusters {
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-QUEUE cluster primaries=%d states=%d peak=%d\n", c.Primaries, c.States, c.Peak)
		}
		Expect(result.StateAfterEmpty).To(Equal("WRITE_ONLY_WITH_QUEUE"))
		Expect(result.Saved).To(Equal(30), "every save is queued, none reaches the engine")
		Expect(result.StateBeforeDrain).To(Equal("WRITE_ONLY_WITH_QUEUE"))
		// Measured target behaviour: the drain commits 12 entries, then every
		// replay of the 13th fails with the typed capacity error; no merge runs
		// during the drain, so the committed split task never executes, 18
		// entries stay queued and the build fails with the index still queued.
		Expect(result.FailureChain).To(Equal([]string{
			"com.apple.foundationdb.record.provider.foundationdb.indexes.VectorIndexClusterTooLargeException",
			"com.apple.foundationdb.async.guardiann.ClusterCapacityExceededException",
		}))
		Expect(result.FailureSite).To(HavePrefix("Insert."))
		Expect(result.StateAfterDrain).To(Equal("WRITE_ONLY_WITH_QUEUE"))
		Expect(result.QueueEntries).To(Equal(18))
		Expect(result.Clusters).To(Equal([]cluster{{Primaries: 12, States: 1, Peak: 12}}))
	})

	// Ordinary saves into a READABLE GuardiANN index run with deferred
	// maintenance (autoMergeDuringCommit defaults to false), so without an
	// external merge the 13th insert into one cluster hits the hard cap. This is
	// the target's backpressure contract the Go port must reproduce.
	It("records ordinary-save backpressure at the hard cap without merges", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_readable_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
		var result struct {
			StateAfterEmpty string    `json:"stateAfterEmptyBuild"`
			Saved           int       `json:"saved"`
			SaveFailure     string    `json:"saveFailure"`
			Clusters        []cluster `json:"clusters"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannQueueCapacityProbe", map[string]any{
			"clusterFile": env.ClusterFile, "records": 30, "markQueue": false,
		}, &result)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "GUARDIANN-READABLE state=%s saved=%d saveFailure=%q clusters=%v\n",
			result.StateAfterEmpty, result.Saved, result.SaveFailure, result.Clusters)
		Expect(result.StateAfterEmpty).To(Equal("READABLE"))
		Expect(result.Saved).To(Equal(12))
		Expect(result.SaveFailure).To(Equal("com.apple.foundationdb.async.guardiann.ClusterCapacityExceededException at record 12"))
		Expect(result.Clusters).To(Equal([]cluster{{Primaries: 12, States: 1, Peak: 12}}))
	})

	// Degenerate-knob consumer map for the ws-d-design admission matrix: for each
	// GuardiANN knob value, which phase of insert / drain / delete / drain /
	// search the target throws in, and which degrades without throwing. Go refuses
	// an operation early only where the target throws; elsewhere it matches the
	// target's degenerate outcome.
	It("maps which GuardiANN consumers throw for degenerate knob values", func() {
		type phaseOutcome struct {
			OK        bool   `json:"ok"`
			Exception string `json:"exception"`
			Site      string `json:"site"`
		}
		type knobResult struct {
			Config           phaseOutcome `json:"config"`
			Insert           phaseOutcome `json:"insert"`
			Trained          bool         `json:"trainedAfterInsert"`
			Drain            []execution  `json:"drain"`
			Delete           phaseOutcome `json:"delete"`
			DrainAfterDelete []execution  `json:"drainAfterDelete"`
			Search           phaseOutcome `json:"search"`
			Found            int          `json:"found"`
			Clusters         []cluster    `json:"clusters"`
			Grow             phaseOutcome `json:"grow"`
			DrainAfterGrow   []execution  `json:"drainAfterGrow"`
			AfterGrow        []cluster    `json:"clustersAfterGrow"`
			Shrink           phaseOutcome `json:"shrink"`
			DrainAfterShrink []execution  `json:"drainAfterShrink"`
			AfterShrink      []cluster    `json:"clustersAfterShrink"`
			DupInsert        phaseOutcome `json:"dupInsert"`
			DrainAfterDup    []execution  `json:"drainAfterDup"`
			AfterDup         []cluster    `json:"clustersAfterDup"`
			DupSearch        phaseOutcome `json:"dupSearch"`
			DupFound         int          `json:"dupFound"`
		}
		describe := func(o phaseOutcome) string {
			if o.OK {
				return "ok"
			}
			return o.Exception + "@" + o.Site
		}
		describeDrain := func(es []execution) string {
			out := ""
			for _, e := range es {
				if e.Executed != nil {
					out += fmt.Sprintf("%d,", *e.Executed)
				} else {
					out += e.Exception + "@" + e.Site + ","
				}
			}
			return out
		}
		// summarizeDrain: "livelock" when every one of the 40 bounded rounds
		// executed a task, "<n>then<exception>@<site>" when a task fails after n
		// executions, otherwise the number of tasks executed before the queue emptied.
		summarizeDrain := func(es []execution) string {
			executed := 0
			for _, e := range es {
				if e.Executed == nil {
					return fmt.Sprintf("%dthen%s@%s", executed, e.Exception, e.Site)
				}
				executed += *e.Executed
			}
			if len(es) == 40 && es[len(es)-1].Executed != nil && *es[len(es)-1].Executed == 1 {
				return "livelock"
			}
			return fmt.Sprint(executed)
		}
		summarize := func(r knobResult) string {
			return fmt.Sprintf("config=%s insert=%s trained=%t drain=%s delete=%s drain=%s search=%s found=%d clusters=%v | grow=%s drain=%s clusters=%v | shrink=%s drain=%s clusters=%v | dup=%s drain=%s clusters=%v search=%s found=%d",
				describe(r.Config), describe(r.Insert), r.Trained, summarizeDrain(r.Drain), describe(r.Delete), summarizeDrain(r.DrainAfterDelete),
				describe(r.Search), r.Found, r.Clusters, describe(r.Grow), summarizeDrain(r.DrainAfterGrow), r.AfterGrow,
				describe(r.Shrink), summarizeDrain(r.DrainAfterShrink), r.AfterShrink,
				describe(r.DupInsert), summarizeDrain(r.DrainAfterDup), r.AfterDup, describe(r.DupSearch), r.DupFound)
		}
		// Measured target behaviour, two identical runs (deterministic randomness).
		// A change here is a change in the target, not noise: revisit the design's
		// admission matrix before updating it.
		want := guardiannPins.Knob
		for _, tc := range []struct{ knob, value string }{
			{"PrimaryClusterMin", "0"},
			{"InsertMaxCandidateClusters", "0"},
			{"DeleteMaxCandidateClusters", "0"},
			{"DeleteConcurrency", "0"},
			{"SplitNumNearestClusters", "0"},
			{"MergeNumNearestClusters", "0"},
			{"ReassignNumNeighboringClusters", "0"},
			{"ReassignNumNeighboringClusters", "-1"},
			{"ReplicatedClusterTarget", "0"},
			{"KMeansMaxIterations", "0"},
			{"KMeansMaxRestarts", "-1"},
			{"SplitMergeConcurrency", "0"},
			{"ReassignConcurrency", "0"},
			{"CollapseConcurrency", "0"},
			{"BounceConcurrency", "0"},
			{"SampleBatchSize", "0"},
			{"Metric", "DOT_PRODUCT_METRIC"},
			{"Metric", "EUCLIDEAN_SQUARE_METRIC"},
			{"Metric", "COSINE_METRIC"},
			{"UseRaBitQ;StatsThreshold;SampleVectorStatsProbability;MaintainStatsProbability", "true;5;1.0;1.0"},
			{"UseRaBitQ;StatsThreshold;SampleVectorStatsProbability;MaintainStatsProbability;SampleBatchSize", "true;5;1.0;1.0;0"},
			{"UseRaBitQ;StatsThreshold;SampleVectorStatsProbability;MaintainStatsProbability;RaBitQNumExBits", "true;5;1.0;1.0;9"},
			{"Metric;UseRaBitQ;RaBitQNumExBits", "DOT_PRODUCT_METRIC;true;9"},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			DeferCleanup(cancel)
			env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_knob_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
			var r knobResult
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannKnobProbe", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(env.Keyspace.Bytes()), "knob": tc.knob, "value": tc.value,
			}, &r)).To(Succeed())
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-KNOB %s=%s trained=%t config=%s insert=%s drain=[%s] delete=%s drain2=[%s] search=%s found=%d clusters=%v\n",
				tc.knob, tc.value, r.Trained, describe(r.Config), describe(r.Insert), describeDrain(r.Drain), describe(r.Delete),
				describeDrain(r.DrainAfterDelete), describe(r.Search), r.Found, r.Clusters)
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-KNOB2 %s=%s grow=%s drain=[%s] clusters=%v shrink=%s drain=[%s] clusters=%v\n",
				tc.knob, tc.value, describe(r.Grow), describeDrain(r.DrainAfterGrow), r.AfterGrow,
				describe(r.Shrink), describeDrain(r.DrainAfterShrink), r.AfterShrink)
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-KNOB3 %s=%s dup=%s drain=[%s] clusters=%v search=%s found=%d\n",
				tc.knob, tc.value, describe(r.DupInsert), describeDrain(r.DrainAfterDup), r.AfterDup,
				describe(r.DupSearch), r.DupFound)
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-KNOB-SUMMARY %s=%s %s\n", tc.knob, tc.value, summarize(r))
			Expect(summarize(r)).To(Equal(want[tc.knob+"="+tc.value]), "knob %s=%s", tc.knob, tc.value)
		}
	})

	// Inline maintenance mode: every insert first runs one queued task
	// (maintainInTransaction=true) and the target skips the hard cap. Each row is
	// one scenario; the summary records every inline insert's outcome.
	It("records inline-maintenance inserts", func() {
		type phaseOutcome struct {
			OK        bool   `json:"ok"`
			Exception string `json:"exception"`
			Site      string `json:"site"`
		}
		type inlineResult struct {
			Preload  phaseOutcome   `json:"preload"`
			Inserts  []phaseOutcome `json:"inserts"`
			Deletes  []phaseOutcome `json:"deletes"`
			Clusters []cluster      `json:"clusters"`
			Search   phaseOutcome   `json:"search"`
			Found    int            `json:"found"`
		}
		runs := func(outcomes []phaseOutcome) string {
			out := ""
			run, last := 0, ""
			flush := func() {
				if run > 0 {
					out += fmt.Sprintf("%s*%d,", last, run)
				}
			}
			for _, o := range outcomes {
				d := "ok"
				if !o.OK {
					d = o.Exception + "@" + o.Site
				}
				if d != last {
					flush()
					last, run = d, 0
				}
				run++
			}
			flush()
			return out
		}
		summarize := func(r inlineResult) string {
			pre := "ok"
			if !r.Preload.OK {
				pre = r.Preload.Exception + "@" + r.Preload.Site
			}
			deletes := ""
			if len(r.Deletes) > 0 {
				deletes = fmt.Sprintf(" deletes=[%s]", runs(r.Deletes))
			}
			return fmt.Sprintf("preload=%s inserts=[%s]%s clusters=%v found=%d", pre, runs(r.Inserts), deletes, r.Clusters, r.Found)
		}
		// Measured target behaviour, identical over two runs (deterministic
		// randomness). A change is a change in the target, not noise.
		want := guardiannPins.Inline
		got := map[string]string{}
		for _, tc := range []struct {
			name                          string
			max, hardMax, preload, inline int
			far, deletes                  int
			knob, value                   string
		}{
			{"hardMaxIsMaxPlusOne", 10, 11, 0, 20, 0, 0, "", ""},
			{"deferredBacklogAtCapThenInline", 10, 12, 12, 10, 0, 0, "", ""},
			{"unsplittableThenInline", 10, 40, 10, 10, 1, 0, "", ""},
			{"collapseConcurrency0", 10, 40, 0, 30, 0, 0, "CollapseConcurrency", "0"},
			{"bounceConcurrency0", 10, 40, 0, 30, 0, 0, "BounceConcurrency", "0"},
			{"splitMergeConcurrency0", 10, 40, 0, 30, 0, 0, "SplitMergeConcurrency", "0"},
			{"reassignConcurrency0", 10, 40, 0, 30, 0, 0, "ReassignConcurrency", "0"},
			{"kMeansMaxIterations0", 10, 40, 0, 30, 0, 0, "KMeansMaxIterations", "0"},
			{"splitNumNearestClusters0", 10, 40, 0, 30, 0, 0, "SplitNumNearestClusters", "0"},
			{"mergeNumNearestClusters0", 10, 40, 0, 30, 0, 27, "MergeNumNearestClusters", "0"},
			{"healthyInlineDeletes", 10, 11, 0, 20, 0, 18, "", ""},
			{"unsplittableInlineDeletes", 10, 40, 10, 10, 1, 5, "", ""},
			{"bounceConcurrency0InlineDeletes", 10, 40, 0, 30, 0, 10, "BounceConcurrency", "0"},
			{"kMeansMaxIterations0InlineDeletes", 10, 40, 0, 30, 0, 10, "KMeansMaxIterations", "0"},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			DeferCleanup(cancel)
			env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_inline_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
			var r inlineResult
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannInlineProbe", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(env.Keyspace.Bytes()), "max": tc.max, "hardMax": tc.hardMax,
				"preload": tc.preload, "inline": tc.inline, "farCount": tc.far, "deletes": tc.deletes,
				"knob": tc.knob, "value": tc.value,
			}, &r)).To(Succeed())
			fmt.Fprintf(GinkgoWriter, "GUARDIANN-INLINE %s %s\n", tc.name, summarize(r))
			got[tc.name] = summarize(r)
		}
		Expect(got).To(Equal(want))
	})

	// Size-balanced split fallback (KMeans lambda > 0 with the production
	// overflowQuadraticPenalty), evaluated by the real PartitionEvaluator with
	// GuardiANN's split parameters, plus final nearest-centroid ownership.
	It("records whether a size-balanced split is usable where the plain split is not", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// Measured, identical over two runs. The size penalty has no effect at any
		// lambda (the outlier's squared distance swamps it); the outlier-excluded
		// refit is usable (KEEP_CURRENT) for separable masses and INVALID for an
		// identical-coordinate mass.
		want := guardiannPins.Balanced
		var got []string
		for _, shape := range []struct{ near, far int }{{10, 1}, {10, 2}, {20, 1}, {-10, 1}} {
			var rows []struct {
				Lambda         float64 `json:"lambda"`
				Sizes          []int   `json:"sizes"`
				Decision       string  `json:"decision"`
				FinalOwnership []int   `json:"finalOwnership"`
				RefitMass      int     `json:"refitMass"`
				RefitFinal     []int   `json:"refitFinalOwnership"`
				RefitDecision  string  `json:"refitDecision"`
				RefitReason    string  `json:"refitReason"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannBalancedSplitProbe", map[string]any{
				"nearCount": shape.near, "farCount": shape.far, "lambdaValues": []float64{0, 0.5, 1, 2, 4, 8, 16},
			}, &rows)).To(Succeed())
			for _, r := range rows {
				line := fmt.Sprintf("GUARDIANN-BALANCED near=%d far=%d lambda=%g sizes=%v decision=%s finalOwnership=%v",
					shape.near, shape.far, r.Lambda, r.Sizes, r.Decision, r.FinalOwnership)
				fmt.Fprintln(GinkgoWriter, line)
				got = append(got, line)
				if r.Lambda == 0 {
					line = fmt.Sprintf("GUARDIANN-REFIT near=%d far=%d mass=%d finalOwnership=%v decision=%s reason=%q",
						shape.near, shape.far, r.RefitMass, r.RefitFinal, r.RefitDecision, r.RefitReason)
					fmt.Fprintln(GinkgoWriter, line)
					got = append(got, line)
				}
			}
		}
		Expect(got).To(ConsistOf(want))
	})

	// runLength renders a sequence of outcomes as "item*count," runs.
	runLength := func(items []string) string {
		out := ""
		run, last := 0, ""
		for _, d := range items {
			if d != last {
				if run > 0 {
					out += fmt.Sprintf("%s*%d,", last, run)
				}
				last, run = d, 0
			}
			run++
		}
		if run > 0 {
			out += fmt.Sprintf("%s*%d,", last, run)
		}
		return out
	}

	// A split population given as explicit 2-D vectors. The shapes are counterexamples
	// to "only coordinate-identical masses reach the terminal reconcile":
	// two outliers on opposite sides, a nested outlier pair, scattered outliers, a natural
	// 60/40 structure under minChildFraction 0.45, and a heavy-tailed line.
	type shape struct {
		name             string
		vectors          [][]float64
		minChildFraction float64
	}
	tight := func(n int) [][]float64 {
		out := make([][]float64, n)
		for i := range out {
			out[i] = []float64{0.01 * float64(i), 0.02 * float64(i%3)}
		}
		return out
	}
	shapes := func() []shape {
		grid := func(n int, x0 float64) [][]float64 {
			out := make([][]float64, n)
			for i := range out {
				out[i] = []float64{x0 + 0.1*float64(i%10), 0.1 * float64(i/10)}
			}
			return out
		}
		heavy := make([][]float64, 16)
		for i := range heavy {
			heavy[i] = []float64{float64(int64(1) << i), 0}
		}
		identical := make([][]float64, 10)
		for i := range identical {
			identical[i] = []float64{0, 0}
		}
		return []shape{
			{"tenPlusOne", append(tight(10), []float64{100, 100}), 0.1},
			{"twentyPlusOne", append(tight(20), []float64{100, 100}), 0.1},
			{"twoOutliersOppositeSides", append(tight(18), []float64{10, 0}, []float64{-100, 0}), 0.1},
			{"nestedOutliers", append(tight(30), []float64{5, 0}, []float64{5.1, 0}, []float64{-100, 0}), 0.1},
			{"scatteredOutliers", append(tight(10), []float64{-50, 0}, []float64{50, 0}), 0.1},
			{"sixtyFortyAtFraction045", append(grid(60, 0), grid(40, 10)...), 0.45},
			{"heavyTail", heavy, 0.1},
			{"identicalMassPlusOne", append(identical, []float64{100, 100}), 0.1},
		}
	}

	// tightGrid packs n vectors into a 0.001-spaced 32-column grid, so a mass of
	// about a thousand stays tight next to outliers at distance 5 to 100.
	tightGrid := func(n int) [][]float64 {
		out := make([][]float64, n)
		for i := range out {
			out[i] = []float64{0.001 * float64(i%32), 0.001 * float64(i/32)}
		}
		return out
	}
	// The same shapes at the default primaryClusterMax scale (n about 1000).
	largeShapes := func() []shape {
		return []shape{
			{"twoOutliersOppositeSidesN1000", append(tightGrid(998), []float64{10, 0}, []float64{-100, 0}), 0.1},
			{"nestedOutliersN1000", append(tightGrid(997), []float64{5, 0}, []float64{5.1, 0}, []float64{-100, 0}), 0.1},
			{"scatteredOutliersN1000", append(tightGrid(998), []float64{-50, 0}, []float64{50, 0}), 0.1},
			{"sixtyFortyAtFraction045N1000", func() [][]float64 {
				out := make([][]float64, 0, 1000)
				for i := 0; i < 600; i++ {
					out = append(out, []float64{0.01 * float64(i%25), 0.01 * float64(i/25)})
				}
				for i := 0; i < 400; i++ {
					out = append(out, []float64{10 + 0.01*float64(i%25), 0.01 * float64(i/25)})
				}
				return out
			}(), 0.45},
		}
	}

	// peelBound is design v8's empirical refit cap, ceil(log2(n)) + 2, kept as the
	// cap these item-11 goldens were measured under and printed beside the item-18
	// rows. It was never structural: the v8 peel removes one member of M per round
	// on isolated outliers (item 18). Design v9 replaces it with the geometric
	// removal floor, whose bound floor(log2(n-1)) refits follows from the floor.
	peelBound := func(n int) int {
		b := 0
		for (1 << b) < n {
			b++
		}
		return b + 2
	}

	// floorBound is the geometric floor's structural refit bound, floor(log2(n-1)):
	// after refit r at least 2^(r+1)-1 members have left the mass, and the peel
	// stops below two members.
	floorBound := func(n int) int {
		b := 0
		for (1 << (b + 1)) <= n-1 {
			b++
		}
		return b
	}

	It("measures the iterated outlier peel on the split counterexample shapes", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// Measured, identical over two runs (fixed seeds). A change is a change in
		// the target's KMeans or PartitionEvaluator: revisit ws-d-design section 6.
		// Every non-identical shape reaches a usable partition within 5 refits at every
		// seed, at n up to 1000, under v8's cap (peelBound); only the identical-
		// coordinate mass exhausts. These 2-D shapes select in the same rounds under the
		// v9 geometric floor (the "floor" rows of the item-18 spec below).
		want := guardiannPins.Peel
		var got []string
		// The target's candidate KMeans draws from the task RNG, so each shape is
		// sampled at eight seeds: a seed is one draw of the target's own candidate.
		letter := func(decision string) string {
			switch decision {
			case "INVALID_CANDIDATE":
				return "I"
			case "KEEP_CURRENT":
				return "K"
			case "ACCEPT_CANDIDATE":
				return "A"
			}
			return decision
		}
		for _, sh := range append(shapes(), largeShapes()...) {
			var rows []struct {
				Seed            int64  `json:"seed"`
				InitialSizes    []int  `json:"initialSizes"`
				InitialDecision string `json:"initialDecision"`
				Rounds          []struct {
					Mass       int    `json:"mass"`
					FinalSizes []int  `json:"finalSizes"`
					Decision   string `json:"decision"`
				} `json:"rounds"`
				Outcome string `json:"outcome"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannPeelProbe", map[string]any{
				"vectors": sh.vectors, "minChildFraction": sh.minChildFraction, "maxRefits": peelBound(len(sh.vectors)),
				"seeds": []int64{42, 1, 2, 3, 4, 5, 6, 7},
			}, &rows)).To(Succeed())
			Expect(rows).To(HaveLen(8))
			line := fmt.Sprintf("GUARDIANN-PEEL %s n=%d bound=%d", sh.name, len(sh.vectors), peelBound(len(sh.vectors)))
			for _, r := range rows {
				line += fmt.Sprintf(" %d:%v%s", r.Seed, r.InitialSizes, letter(r.InitialDecision))
				for _, rd := range r.Rounds {
					line += fmt.Sprintf(">%d%v%s", rd.Mass, rd.FinalSizes, letter(rd.Decision))
				}
				line += "=" + r.Outcome
			}
			fmt.Fprintln(GinkgoWriter, line)
			got = append(got, line)
		}
		Expect(got).To(Equal(want))
	})

	// Many isolated outliers in a realistic dimension. In high dimension random
	// directions are nearly orthogonal, so far outliers are about sqrt(2)*R apart
	// from each other but only sqrt(R^2+d) from the core. Around a TIGHT core
	// (sigma 0.01) KMeans k=2 then isolates ONE outlier per fit and every other
	// outlier joins the core, so the v8 peel ("child") removes one member of M per
	// round and its round count follows the outlier count m, not log n. Around a
	// spread core (sigma 1) the first fit already splits the core on 39 of 40
	// seeds. The refit cap here is raised far above the design bound so the rounds
	// each shape actually needs are visible; the bound is printed alongside.
	It("measures the peel on many isolated high-dimensional outliers", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		gaussCore := func(r *rand.Rand, n, d int) [][]float64 {
			out := make([][]float64, n)
			for i := range out {
				v := make([]float64, d)
				for j := range v {
					v[j] = r.NormFloat64()
				}
				out[i] = v
			}
			return out
		}
		outliers := func(r *rand.Rand, m, d int, radius float64) [][]float64 {
			out := make([][]float64, m)
			for i := range out {
				v := make([]float64, d)
				var norm float64
				for j := range v {
					v[j] = r.NormFloat64()
					norm += v[j] * v[j]
				}
				norm = math.Sqrt(norm)
				for j := range v {
					v[j] = v[j] / norm * radius
				}
				out[i] = v
			}
			return out
		}
		// sigma scales the core; the outlier radius is radiusFactor*sqrt(d) (the
		// core's own radius at sigma 1). A tight core (sigma << 1) is the regime of
		// the 2-D grid shapes, where the outlier distance dwarfs the core spread.
		hdSigma := func(name string, n, d, m int, sigma, radiusFactor float64, seed uint64) shape {
			r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
			core := gaussCore(r, n-m, d)
			for _, v := range core {
				for j := range v {
					v[j] *= sigma
				}
			}
			return shape{name, append(core, outliers(r, m, d, radiusFactor*math.Sqrt(float64(d)))...), 0.1}
		}
		hd := func(name string, n, d, m int, radiusFactor float64, seed uint64) shape {
			return hdSigma(name, n, d, m, 1, radiusFactor, seed)
		}
		hdShapes := []shape{
			hd("hdOutliers13R3N1000D128", 1000, 128, 13, 3, 1),
			hd("hdOutliers20R3N1000D128", 1000, 128, 20, 3, 2),
			hd("hdOutliers50R3N1000D128", 1000, 128, 50, 3, 3),
			hd("hdOutliers20R1p5N1000D128", 1000, 128, 20, 1.5, 4),
			hd("hdOutliers50R1p5N1000D128", 1000, 128, 50, 1.5, 5),
			hd("hdOutliers20R10N1000D128", 1000, 128, 20, 10, 8),
			hdSigma("hdTight13N1000D128", 1000, 128, 13, 0.01, 1, 9),
			hdSigma("hdTight20N1000D128", 1000, 128, 20, 0.01, 1, 10),
			hdSigma("hdTight50N1000D128", 1000, 128, 50, 0.01, 1, 11),
			hdSigma("hdTight20Sigma0p1N1000D128", 1000, 128, 20, 0.1, 1, 12),
			func() shape {
				// Strictly increasing outlier radii: if KMeans isolates the farthest
				// point each time, a tail cut removes nothing beyond it.
				sh := hdSigma("hdTightRising50N1000D128", 1000, 128, 50, 0.01, 1, 14)
				for i, v := range sh.vectors[950:] {
					for j := range v {
						v[j] *= 1 + float64(i)/10
					}
				}
				return sh
			}(),
		}
		timingShapes := []shape{
			hd("hdOutliers50R3N2000D128", 2000, 128, 50, 3, 6),
			hd("hdOutliers50R3N2000D768", 2000, 768, 50, 3, 7),
			hdSigma("hdTight50N2000D768", 2000, 768, 50, 0.01, 1, 13),
		}
		type probeRow struct {
			Seed            int64  `json:"seed"`
			FitNanos        int64  `json:"fitNanos"`
			InitialSizes    []int  `json:"initialSizes"`
			InitialDecision string `json:"initialDecision"`
			Rounds          []struct {
				RefitNanos int64  `json:"refitNanos"`
				Mass       int    `json:"mass"`
				Fitted     int    `json:"fitted"`
				FinalSizes []int  `json:"finalSizes"`
				Decision   string `json:"decision"`
			} `json:"rounds"`
			Outcome string `json:"outcome"`
		}
		run := func(sh shape, seeds []int64, cap int, mode string) []probeRow {
			var rows []probeRow
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannPeelModeProbe", map[string]any{
				"vectors": sh.vectors, "minChildFraction": sh.minChildFraction, "maxRefits": cap,
				"seeds": seeds, "peelMode": mode,
			}, &rows)).To(Succeed())
			Expect(rows).To(HaveLen(len(seeds)))
			return rows
		}
		summary := func(sh shape, mode string, rows []probeRow) string {
			line := fmt.Sprintf("GUARDIANN-PEEL-HD %s mode=%s n=%d floorBound=%d", sh.name, mode, len(sh.vectors), floorBound(len(sh.vectors)))
			for _, r := range rows {
				last := "-"
				if k := len(r.Rounds); k > 0 {
					last = fmt.Sprintf("%d%v%s", r.Rounds[k-1].Mass, r.Rounds[k-1].FinalSizes, r.Rounds[k-1].Decision[:1])
				}
				line += fmt.Sprintf(" %d:%v%s rounds=%d last=%s=%s", r.Seed, r.InitialSizes, r.InitialDecision[:1],
					len(r.Rounds), last, r.Outcome)
			}
			return line
		}
		timing := func(sh shape, mode string, rows []probeRow) string {
			line := fmt.Sprintf("GUARDIANN-PEEL-TIMING %s mode=%s n=%d d=%d", sh.name, mode, len(sh.vectors), len(sh.vectors[0]))
			for _, r := range rows {
				var sum, mx int64
				fitted := 0
				for _, rd := range r.Rounds {
					sum += rd.RefitNanos
					if rd.RefitNanos > mx {
						mx = rd.RefitNanos
					}
					if rd.Fitted > fitted {
						fitted = rd.Fitted
					}
				}
				line += fmt.Sprintf(" %d:fit=%.1fms refits=%d refitTotal=%.1fms refitMax=%.1fms maxFitted=%d", r.Seed,
					float64(r.FitNanos)/1e6, len(r.Rounds), float64(sum)/1e6, float64(mx)/1e6, fitted)
			}
			return line
		}
		// Measured, identical over two runs (fixed data and seeds). "child" is the v8
		// peel, "tail" the tail cut (measured and rejected: KMeans' best-SSE fit
		// isolates the FARTHEST point, so a threshold taken from it removes nothing
		// more), "floor" the geometric removal floor, and "floor-sample:S" the floor
		// with every refit's KMeans on a stride sample of at most S mass members while
		// the partition still assigns and scores every vector: the rule ws-d-design
		// section 6 adopts at S = 256, with 64 measured beside it. A change is a change
		// in the target's KMeans or PartitionEvaluator.
		want := guardiannPins.PeelHD
		const measureCap = 200
		var got []string
		modes := []string{"child", "tail", "floor", "floor-sample:256", "floor-sample:64"}
		for _, mode := range modes {
			for _, sh := range append(append(shapes(), largeShapes()...), hdShapes...) {
				rows := run(sh, []int64{42, 1, 2, 3, 4, 5, 6, 7}, measureCap, mode)
				line := summary(sh, mode, rows)
				fmt.Fprintln(GinkgoWriter, line)
				fmt.Fprintln(GinkgoWriter, timing(sh, mode, rows))
				got = append(got, line)
			}
			for _, sh := range timingShapes {
				rows := run(sh, []int64{42, 1}, measureCap, mode)
				line := summary(sh, mode, rows)
				fmt.Fprintln(GinkgoWriter, line)
				fmt.Fprintln(GinkgoWriter, timing(sh, mode, rows))
				got = append(got, line)
			}
		}
		Expect(got).To(HaveLen(len(modes) * (len(shapes()) + len(largeShapes()) + len(hdShapes) + len(timingShapes))))
		Expect(got).To(Equal(want))
	})

	It("measures the unsampled floor peel at high dimension", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		// Shapes are generated inside the JVM (guardiannPeelGeneratedProbe) so that
		// d in the thousands does not cross the invoker as JSON: n - m core vectors
		// with N(0, sigma^2) coordinates, then m outliers at radius radiusFactor*sqrt(d).
		type genShape struct {
			name                string
			n, d, m             int
			sigma, radiusFactor float64
			genSeed             int64
		}
		var gen []genShape
		for i, d := range []int{768, 1536, 3072, 4096} {
			gen = append(gen,
				genShape{fmt.Sprintf("genTight50N2000D%d", d), 2000, d, 50, 0.01, 1, int64(101 + i)},
				genShape{fmt.Sprintf("genSpread50R3N2000D%d", d), 2000, d, 50, 1, 3, int64(111 + i)})
		}
		type probeRow struct {
			Seed            int64  `json:"seed"`
			FitNanos        int64  `json:"fitNanos"`
			InitialSizes    []int  `json:"initialSizes"`
			InitialDecision string `json:"initialDecision"`
			Rounds          []struct {
				RefitNanos int64  `json:"refitNanos"`
				Mass       int    `json:"mass"`
				Fitted     int    `json:"fitted"`
				FinalSizes []int  `json:"finalSizes"`
				Decision   string `json:"decision"`
			} `json:"rounds"`
			Outcome string `json:"outcome"`
		}
		seeds := []int64{42, 1}
		modes := []string{"floor", "floor-sample:256"}
		var got []string
		for _, mode := range modes {
			for _, sh := range gen {
				var rows []probeRow
				Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannPeelGeneratedProbe", map[string]any{
					"n": sh.n, "d": sh.d, "m": sh.m, "sigma": sh.sigma, "radiusFactor": sh.radiusFactor,
					"genSeed": sh.genSeed, "minChildFraction": 0.1, "maxRefits": 200, "seeds": seeds,
					"peelMode": mode,
				}, &rows)).To(Succeed())
				Expect(rows).To(HaveLen(len(seeds)))
				// The pinned line: exit, refit count and the selected partition's
				// child sizes with its smaller-child fraction (the balance the
				// minChildFraction gate reads). Timings are printed, never pinned.
				line := fmt.Sprintf("GUARDIANN-PEEL-GEN %s mode=%s n=%d d=%d floorBound=%d", sh.name, mode, sh.n, sh.d, floorBound(sh.n))
				timing := fmt.Sprintf("GUARDIANN-PEEL-GEN-TIMING %s mode=%s", sh.name, mode)
				for _, r := range rows {
					sizes, frac := r.InitialSizes, "-"
					if k := len(r.Rounds); k > 0 {
						sizes = r.Rounds[k-1].FinalSizes
					}
					if r.Outcome == "selected" || r.Outcome == "initialUsable" {
						frac = fmt.Sprintf("%.3f", float64(min(sizes[0], sizes[1]))/float64(sizes[0]+sizes[1]))
					}
					line += fmt.Sprintf(" %d:%v%s refits=%d final=%v minFrac=%s=%s", r.Seed, r.InitialSizes,
						r.InitialDecision[:1], len(r.Rounds), sizes, frac, r.Outcome)
					var sum, mx int64
					for _, rd := range r.Rounds {
						sum += rd.RefitNanos
						mx = max(mx, rd.RefitNanos)
					}
					timing += fmt.Sprintf(" %d:fit=%.1fms refitTotal=%.1fms refitMax=%.1fms", r.Seed,
						float64(r.FitNanos)/1e6, float64(sum)/1e6, float64(mx)/1e6)
				}
				fmt.Fprintln(GinkgoWriter, line)
				fmt.Fprintln(GinkgoWriter, timing)
				got = append(got, line)
			}
		}
		for _, l := range got {
			fmt.Fprintf(GinkgoWriter, "GEN-PIN %q,\n", l)
		}
		// Measured, identical over two runs (generated data, fixed seeds). The
		// unsampled floor keeps the selected partition's smaller child at 0.33 to
		// 0.49 of n at every d; the stride-sampled refit (S = 256), withdrawn by
		// ws-d-design v11, drops it to 243 and 317 of 2000 (0.12 and 0.16) on the
		// tight core at d = 4096, against minChildFraction's INVALID line of 200.
		want := guardiannPins.PeelGen
		Expect(got).To(HaveLen(len(modes) * len(gen)))
		Expect(got).To(Equal(want))
	})

	It("measures the unsampled floor peel in the admitted high-dimension corner", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		// The shapes the admission bound of ws-d-design admits at its largest dimensions
		// (W = floor(log2(n - 1)) * n * d <= B at the default KMeans knobs 8 and 3,
		// whose knob factor is 1), and the shapes just beyond it. B is
		// 1.96 * 10^7, a WORK bound chosen for coverage, not derived from any measured
		// rate (every run moved the worst observed rate, so a time-derived B moved with
		// it): the first over-max size n = 1001 is admitted up to d = 2175 and the
		// default hard cap n = 2000 up to d = 980. The timings this spec prints are
		// estimates, extracted from every log by conformance/testdata/rfc257/corner-refit-rates.py. The d = 2775 and d = 1250 rows are the v12 bound (2.5 * 10^7), now
		// beyond B: kept, as the target's behaviour just past the admitted corner. Same
		// generator as the high-dimension spec above, unsampled floor only.
		type genShape struct {
			name                string
			n, d, m             int
			sigma, radiusFactor float64
			genSeed             int64
		}
		var gen []genShape
		for i, nd := range [][2]int{{1001, 2048}, {1001, 2775}, {2000, 1250}, {1001, 2175}, {2000, 980}} {
			n, d := nd[0], nd[1]
			gen = append(gen,
				genShape{fmt.Sprintf("genTight50N%dD%d", n, d), n, d, 50, 0.01, 1, int64(201 + i)},
				genShape{fmt.Sprintf("genSpread50R3N%dD%d", n, d), n, d, 50, 1, 3, int64(211 + i)})
		}
		type probeRow struct {
			Seed            int64  `json:"seed"`
			FitNanos        int64  `json:"fitNanos"`
			InitialSizes    []int  `json:"initialSizes"`
			InitialDecision string `json:"initialDecision"`
			Rounds          []struct {
				RefitNanos int64 `json:"refitNanos"`
				FinalSizes []int `json:"finalSizes"`
			} `json:"rounds"`
			Outcome string `json:"outcome"`
		}
		seeds := []int64{42, 1}
		var got []string
		for _, sh := range gen {
			var rows []probeRow
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannPeelGeneratedProbe", map[string]any{
				"n": sh.n, "d": sh.d, "m": sh.m, "sigma": sh.sigma, "radiusFactor": sh.radiusFactor,
				"genSeed": sh.genSeed, "minChildFraction": 0.1, "maxRefits": 200, "seeds": seeds,
				"peelMode": "floor",
			}, &rows)).To(Succeed())
			Expect(rows).To(HaveLen(len(seeds)))
			line := fmt.Sprintf("GUARDIANN-PEEL-CORNER %s n=%d d=%d floorBound=%d", sh.name, sh.n, sh.d, floorBound(sh.n))
			timing := fmt.Sprintf("GUARDIANN-PEEL-CORNER-TIMING %s", sh.name)
			for _, r := range rows {
				sizes, frac := r.InitialSizes, "-"
				if k := len(r.Rounds); k > 0 {
					sizes = r.Rounds[k-1].FinalSizes
				}
				if r.Outcome == "selected" || r.Outcome == "initialUsable" {
					frac = fmt.Sprintf("%.3f", float64(min(sizes[0], sizes[1]))/float64(sizes[0]+sizes[1]))
				}
				line += fmt.Sprintf(" %d:%v%s refits=%d final=%v minFrac=%s=%s", r.Seed, r.InitialSizes,
					r.InitialDecision[:1], len(r.Rounds), sizes, frac, r.Outcome)
				var sum, mx int64
				for _, rd := range r.Rounds {
					sum += rd.RefitNanos
					mx = max(mx, rd.RefitNanos)
				}
				timing += fmt.Sprintf(" %d:fit=%.1fms refitTotal=%.1fms refitMax=%.1fms", r.Seed,
					float64(r.FitNanos)/1e6, float64(sum)/1e6, float64(mx)/1e6)
			}
			fmt.Fprintln(GinkgoWriter, line)
			fmt.Fprintln(GinkgoWriter, timing)
			got = append(got, line)
		}
		for _, l := range got {
			fmt.Fprintf(GinkgoWriter, "CORNER-PIN %q,\n", l)
		}
		// Measured. Every row selects (or is usable at round 0). In the admitted corner
		// (d = 2048 and 2175 at n = 1001, d = 980 at n = 2000) the smaller child is 0.286
		// to 0.480 of n; beyond B (d = 2775, n = 2000 at d = 1250) 0.422 to 0.495.
		want := guardiannPins.PeelCorner
		Expect(got).To(Equal(want))
	})

	It("records the target on the split counterexample shapes in deferred and inline mode", func() {
		type phaseOutcome struct {
			OK        bool   `json:"ok"`
			Exception string `json:"exception"`
			Site      string `json:"site"`
		}
		// Measured, deterministic. The target fails every non-identical shape forever at
		// the orElseThrow site in BOTH modes (deferred drains and inline inserts alike);
		// the identical mass takes the collapse route instead.
		want := guardiannPins.Shape
		var got []string
		for _, sh := range shapes() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			DeferCleanup(cancel)
			env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_shape_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
			var r struct {
				Executions          []execution    `json:"executions"`
				Clusters            []cluster      `json:"clusters"`
				Inline              []phaseOutcome `json:"inline"`
				ClustersAfterInline []cluster      `json:"clustersAfterInline"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannSplitShapeProbe", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(env.Keyspace.Bytes()), "vectors": sh.vectors,
				"minChildFraction": sh.minChildFraction, "inlineTail": 5,
			}, &r)).To(Succeed())
			var drain, inline []string
			for _, e := range r.Executions {
				if e.Executed != nil {
					drain = append(drain, fmt.Sprint(*e.Executed))
				} else {
					drain = append(drain, e.Exception+"@"+e.Site)
				}
			}
			for _, o := range r.Inline {
				if o.OK {
					inline = append(inline, "ok")
				} else {
					inline = append(inline, o.Exception+"@"+o.Site)
				}
			}
			line := fmt.Sprintf("GUARDIANN-SHAPE %s deferred=[%s] clusters=%v inline=[%s] clusters=%v",
				sh.name, runLength(drain), r.Clusters, runLength(inline), r.ClustersAfterInline)
			fmt.Fprintln(GinkgoWriter, line)
			got = append(got, line)
		}
		Expect(got).To(Equal(want))
	})

	It("consumes obsolete tasks as no-ops under the knob those tasks would consume", func() {
		// Measured, deterministic. collapse: the obsolete collapse and its bounce are
		// consumed without failing; the bounce re-arms a split, the split collapses again,
		// and only that LIVE collapse fails at the collapseConcurrency consumer.
		// reassign/merge: every obsolete task is consumed without consuming its knob.
		// bits9: a split queued before RaBitQ trained with unsupported extra bits 9
		// fails at Primitives.quantizer while live and is consumed once obsolete.
		want := guardiannPins.Obsolete
		var got []string
		for _, scenario := range []string{"collapse", "reassign", "merge", "bits9Live", "bits9Obsolete"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			DeferCleanup(cancel)
			env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_obsolete_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
			var r struct {
				Setup          []execution `json:"setup"`
				Trained        bool        `json:"trained"`
				ClustersBefore []cluster   `json:"clustersBefore"`
				TasksBefore    []string    `json:"tasksBefore"`
				Staged         []int       `json:"staged"`
				Drain          []execution `json:"drain"`
				TasksAfter     []string    `json:"tasksAfter"`
				ClustersAfter  []cluster   `json:"clustersAfter"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannObsoleteTaskProbe", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(env.Keyspace.Bytes()), "scenario": scenario,
			}, &r)).To(Succeed())
			describe := func(es []execution) string {
				out := ""
				for _, e := range es {
					if e.Executed != nil {
						out += fmt.Sprintf("%d,", *e.Executed)
					} else {
						out += e.Exception + "@" + e.Site + ","
					}
				}
				return out
			}
			line := fmt.Sprintf("GUARDIANN-OBSOLETE %s setupRounds=%d before=%v tasks=%v staged=%v drain=[%s] tasksAfter=%v after=%v",
				scenario, len(r.Setup), r.ClustersBefore, r.TasksBefore, r.Staged, describe(r.Drain), r.TasksAfter, r.ClustersAfter)
			if strings.HasPrefix(scenario, "bits9") {
				line += fmt.Sprintf(" trained=%t", r.Trained)
			}
			fmt.Fprintln(GinkgoWriter, line)
			got = append(got, line)
		}
		Expect(got).To(Equal(want))
	})

	It("records inline maintenance reached through the record layer", func() {
		type phaseOutcome struct {
			OK        bool   `json:"ok"`
			Exception string `json:"exception"`
			Site      string `json:"site"`
		}
		// Measured, deterministic: autoMergeDuringCommit reaches the engine as
		// maintainInTransaction, so the engine-level inline goldens transfer.
		want := guardiannPins.RecordInline
		var got []string
		for _, tc := range []struct {
			name            string
			near, far, tail int
			hardMax         string
			autoMerge       bool
		}{
			{"autoMergeHardMaxIsMaxPlusOne", 20, 0, 0, "11", true},
			{"deferredHardMaxIsMaxPlusOne", 20, 0, 0, "11", false},
			{"autoMergeUnsplittable", 10, 1, 5, "40", true},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			DeferCleanup(cancel)
			// The probe writes under its own random record-store subspace and clears it on exit.
			env, err := SetupJavaTenant(ctx, sharedContainer, "guardiann_record_inline_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(env.Cleanup(context.Background())).To(Succeed()) })
			var r struct {
				Saves    []phaseOutcome `json:"saves"`
				Clusters []cluster      `json:"clusters"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "guardiannRecordInlineProbe", map[string]any{
				"clusterFile": env.ClusterFile, "nearCount": tc.near, "farCount": tc.far,
				"tail": tc.tail, "hardMax": tc.hardMax, "autoMerge": tc.autoMerge,
			}, &r)).To(Succeed())
			var saves []string
			for _, o := range r.Saves {
				if o.OK {
					saves = append(saves, "ok")
				} else {
					saves = append(saves, o.Exception+"@"+o.Site)
				}
			}
			line := fmt.Sprintf("GUARDIANN-RECORD-INLINE %s saves=[%s] clusters=%v", tc.name, runLength(saves), r.Clusters)
			fmt.Fprintln(GinkgoWriter, line)
			got = append(got, line)
		}
		Expect(got).To(Equal(want))
	})
})
