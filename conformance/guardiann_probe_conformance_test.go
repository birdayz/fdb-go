//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/binary"
	"encoding/hex"
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
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_split_"+uuid.New().String())
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
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_empty_"+uuid.New().String())
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
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_queue_"+uuid.New().String())
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
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_readable_"+uuid.New().String())
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
		want := map[string]string{
			"PrimaryClusterMin=0":               "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"InsertMaxCandidateClusters=0":      "config=ok insert=ok trained=false drain=0 delete=ok drain=0 search=ok found=0 clusters=[{0 0 0}] | grow=ok drain=0 clusters=[{0 0 0}] | shrink=ok drain=0 clusters=[{0 0 0}] | dup=ok drain=0 clusters=[{0 0 0}] search=ok found=0",
			"DeleteMaxCandidateClusters=0":      "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {10 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=0 clusters=[{2 0 2} {9 0 9} {9 0 9}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"DeleteConcurrency=0":               "config=ok insert=ok trained=false drain=2 delete=java.lang.IllegalArgumentException@Delete.lambda$deleteFromClusters$10:170 drain=0 search=ok found=3 clusters=[{2 0 2} {10 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {10 0 10}] | shrink=java.lang.IllegalArgumentException@Delete.lambda$deleteFromClusters$10:170 drain=0 clusters=[{2 0 2} {9 0 9} {10 0 10}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"SplitNumNearestClusters=0":         "config=ok insert=ok trained=false drain=livelock delete=ok drain=livelock search=ok found=3 clusters=[{11 1 12}] | grow=ok drain=livelock clusters=[{20 1 20}] | shrink=ok drain=2 clusters=[{3 0 20}] | dup=ok drain=livelock clusters=[{11 1 11}] search=ok found=11",
			"MergeNumNearestClusters=0":         "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=livelock clusters=[{2 0 2} {1 0 9} {0 1 9}] | dup=ok drain=livelock clusters=[{1 1 11}] search=ok found=11",
			"ReassignNumNeighboringClusters=0":  "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"ReassignNumNeighboringClusters=-1": "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=livelock clusters=[{2 2 2} {9 2 9} {9 2 9}] | shrink=ok drain=livelock clusters=[{2 2 2} {1 2 9} {0 3 9}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"ReplicatedClusterTarget=0":         "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"KMeansMaxIterations=0":             "config=ok insert=ok trained=false drain=1thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 delete=ok drain=0thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 search=ok found=3 clusters=[{11 1 12}] | grow=ok drain=0thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 clusters=[{20 1 20}] | shrink=ok drain=1 clusters=[{3 0 20}] | dup=ok drain=1thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 clusters=[{11 1 11}] search=ok found=11",
			"KMeansMaxRestarts=-1":              "config=ok insert=ok trained=false drain=1thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 delete=ok drain=0thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 search=ok found=3 clusters=[{11 1 12}] | grow=ok drain=0thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 clusters=[{20 1 20}] | shrink=ok drain=1 clusters=[{3 0 20}] | dup=ok drain=1thenjava.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108 clusters=[{11 1 11}] search=ok found=11",
			"SplitMergeConcurrency=0":           "config=ok insert=ok trained=false drain=livelock delete=ok drain=livelock search=ok found=3 clusters=[{11 1 12}] | grow=ok drain=livelock clusters=[{20 1 20}] | shrink=ok drain=livelock clusters=[{3 1 20}] | dup=ok drain=livelock clusters=[{11 1 11}] search=ok found=11",
			"ReassignConcurrency=0":             "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=livelock clusters=[{2 2 2} {9 2 9} {9 2 9}] | shrink=ok drain=livelock clusters=[{2 2 2} {1 2 9} {0 3 9}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"CollapseConcurrency=0":             "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=2thenjava.lang.IllegalArgumentException@Primitives.fetchCoreClusters:1512 clusters=[{11 4 11}] search=ok found=11",
			"BounceConcurrency=0":               "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=2thenjava.lang.IllegalArgumentException@BounceTask.runTask:130 clusters=[{2 2 2} {9 0 9} {9 0 9}] | shrink=ok drain=2thenjava.lang.IllegalArgumentException@BounceTask.runTask:130 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=2thenjava.lang.IllegalArgumentException@BounceTask.runTask:130 clusters=[{11 4 11}] search=ok found=11",
			"SampleBatchSize=0":                 "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"Metric=DOT_PRODUCT_METRIC":         "config=ok insert=ok trained=false drain=1thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 delete=ok drain=0thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 search=ok found=3 clusters=[{11 1 12}] | grow=ok drain=0thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 clusters=[{20 1 20}] | shrink=ok drain=1 clusters=[{3 0 20}] | dup=ok drain=1thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 clusters=[{11 1 11}] search=ok found=11",
			"Metric=EUCLIDEAN_SQUARE_METRIC":    "config=ok insert=ok trained=false drain=1thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 delete=ok drain=0thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 search=ok found=3 clusters=[{11 1 12}] | grow=ok drain=0thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 clusters=[{20 1 20}] | shrink=ok drain=1 clusters=[{3 0 20}] | dup=ok drain=1thenjava.lang.UnsupportedOperationException@SplitMergeTask.kMeans:1108 clusters=[{11 1 11}] search=ok found=11",
			"Metric=COSINE_METRIC":              "config=ok insert=ok trained=false drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{1 0 1} {2 0 2}] | dup=ok drain=5 clusters=[{1 0 11}] search=ok found=11",
			"UseRaBitQ;StatsThreshold;SampleVectorStatsProbability;MaintainStatsProbability=true;5;1.0;1.0":                   "config=ok insert=ok trained=true drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=4 clusters=[{6 0 11}] search=ok found=11",
			"UseRaBitQ;StatsThreshold;SampleVectorStatsProbability;MaintainStatsProbability;SampleBatchSize=true;5;1.0;1.0;0": "config=ok insert=ok trained=true drain=2 delete=ok drain=0 search=ok found=3 clusters=[{2 0 2} {9 0 10}] | grow=ok drain=8 clusters=[{2 0 2} {9 0 9} {9 0 9}] | shrink=ok drain=2 clusters=[{2 0 2} {1 0 1}] | dup=ok drain=4 clusters=[{6 0 11}] search=ok found=11",
			"UseRaBitQ;StatsThreshold;SampleVectorStatsProbability;MaintainStatsProbability;RaBitQNumExBits=true;5;1.0;1.0;9": "config=ok insert=java.lang.IllegalArgumentException@Primitives.quantizer:359 trained=true drain=0 delete=ok drain=0 search=java.lang.IllegalArgumentException@Primitives.quantizer:359 found=0 clusters=[{4 0 5}] | grow=java.lang.IllegalArgumentException@Primitives.quantizer:359 drain=0 clusters=[{4 0 5}] | shrink=ok drain=0 clusters=[{0 0 5}] | dup=java.lang.IllegalArgumentException@Primitives.quantizer:359 drain=0 clusters=[{5 0 5}] search=java.lang.IllegalArgumentException@Primitives.quantizer:359 found=0",
			"Metric;UseRaBitQ;RaBitQNumExBits=DOT_PRODUCT_METRIC;true;9":                                                      "config=ok insert=java.lang.IllegalArgumentException@Primitives.quantizer:359 trained=false drain=0 delete=ok drain=0 search=ok found=0 clusters=[] | grow=java.lang.IllegalArgumentException@Primitives.quantizer:359 drain=0 clusters=[] | shrink=ok drain=0 clusters=[] | dup=java.lang.IllegalArgumentException@Primitives.quantizer:359 drain=0 clusters=[] search=ok found=0",
		}
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
			env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_knob_"+uuid.New().String())
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
		want := map[string]string{
			"bounceConcurrency0":             "preload=ok inserts=[ok*21,java.lang.IllegalArgumentException@BounceTask.runTask:130*9,] clusters=[{6 0 6} {9 0 9} {6 0 6}] found=21",
			"collapseConcurrency0":           "preload=ok inserts=[ok*30,] clusters=[{6 0 6} {6 0 6} {11 3 11} {7 2 7}] found=30",
			"deferredBacklogAtCapThenInline": "preload=ok inserts=[ok*10,] clusters=[{9 2 9} {7 0 7} {6 2 6}] found=22",
			"hardMaxIsMaxPlusOne":            "preload=ok inserts=[ok*20,] clusters=[{6 2 6} {8 0 8} {6 0 6}] found=20",
			"kMeansMaxIterations0":           "preload=ok inserts=[ok*12,java.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108*18,] clusters=[{12 1 12}] found=12",
			"reassignConcurrency0":           "preload=ok inserts=[ok*30,] clusters=[{6 2 6} {18 1 18} {6 0 6}] found=30",
			"splitMergeConcurrency0":         "preload=ok inserts=[ok*30,] clusters=[{30 1 30}] found=30",
			"unsplittableThenInline":         "preload=ok inserts=[ok*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*9,] clusters=[{12 1 12}] found=12",

			"splitNumNearestClusters0":          "preload=ok inserts=[ok*30,] clusters=[{30 1 30}] found=30",
			"mergeNumNearestClusters0":          "preload=ok inserts=[ok*30,] deletes=[ok*27,] clusters=[{3 2 6} {0 1 6} {0 1 6} {0 1 5} {0 3 7}] found=3",
			"healthyInlineDeletes":              "preload=ok inserts=[ok*20,] deletes=[ok*18,] clusters=[{2 0 5}] found=2",
			"unsplittableInlineDeletes":         "preload=ok inserts=[ok*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*9,] deletes=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{12 1 12}] found=12",
			"bounceConcurrency0InlineDeletes":   "preload=ok inserts=[ok*21,java.lang.IllegalArgumentException@BounceTask.runTask:130*9,] deletes=[java.lang.IllegalArgumentException@BounceTask.runTask:130*10,] clusters=[{6 0 6} {9 0 9} {6 0 6}] found=21",
			"kMeansMaxIterations0InlineDeletes": "preload=ok inserts=[ok*12,java.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108*18,] deletes=[java.lang.IllegalArgumentException@SplitMergeTask.kMeans:1108*10,] clusters=[{12 1 12}] found=12",
		}
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
			env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_inline_"+uuid.New().String())
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
		want := []string{
			`GUARDIANN-BALANCED near=-10 far=1 lambda=0.5 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=1 lambda=0.5 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=-10 far=1 lambda=0 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=1 lambda=0 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=-10 far=1 lambda=16 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=1 lambda=16 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=-10 far=1 lambda=1 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=1 lambda=1 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=-10 far=1 lambda=2 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=1 lambda=2 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=-10 far=1 lambda=4 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=1 lambda=4 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=-10 far=1 lambda=8 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=1 lambda=8 sizes=[10 1] decision=INVALID_CANDIDATE finalOwnership=[10 1]`,
			`GUARDIANN-BALANCED near=10 far=2 lambda=0.5 sizes=[2 10] decision=KEEP_CURRENT finalOwnership=[2 10]`,
			`GUARDIANN-BALANCED near=10 far=2 lambda=0 sizes=[2 10] decision=KEEP_CURRENT finalOwnership=[2 10]`,
			`GUARDIANN-BALANCED near=10 far=2 lambda=16 sizes=[2 10] decision=KEEP_CURRENT finalOwnership=[2 10]`,
			`GUARDIANN-BALANCED near=10 far=2 lambda=1 sizes=[2 10] decision=KEEP_CURRENT finalOwnership=[2 10]`,
			`GUARDIANN-BALANCED near=10 far=2 lambda=2 sizes=[2 10] decision=KEEP_CURRENT finalOwnership=[2 10]`,
			`GUARDIANN-BALANCED near=10 far=2 lambda=4 sizes=[2 10] decision=KEEP_CURRENT finalOwnership=[2 10]`,
			`GUARDIANN-BALANCED near=10 far=2 lambda=8 sizes=[2 10] decision=KEEP_CURRENT finalOwnership=[2 10]`,
			`GUARDIANN-BALANCED near=20 far=1 lambda=0.5 sizes=[20 1] decision=INVALID_CANDIDATE finalOwnership=[20 1]`,
			`GUARDIANN-BALANCED near=20 far=1 lambda=0 sizes=[20 1] decision=INVALID_CANDIDATE finalOwnership=[20 1]`,
			`GUARDIANN-BALANCED near=20 far=1 lambda=16 sizes=[20 1] decision=INVALID_CANDIDATE finalOwnership=[20 1]`,
			`GUARDIANN-BALANCED near=20 far=1 lambda=1 sizes=[20 1] decision=INVALID_CANDIDATE finalOwnership=[20 1]`,
			`GUARDIANN-BALANCED near=20 far=1 lambda=2 sizes=[20 1] decision=INVALID_CANDIDATE finalOwnership=[20 1]`,
			`GUARDIANN-BALANCED near=20 far=1 lambda=4 sizes=[20 1] decision=INVALID_CANDIDATE finalOwnership=[20 1]`,
			`GUARDIANN-BALANCED near=20 far=1 lambda=8 sizes=[20 1] decision=INVALID_CANDIDATE finalOwnership=[20 1]`,
			`GUARDIANN-REFIT near=-10 far=1 mass=10 finalOwnership=[11 0] decision=INVALID_CANDIDATE reason="[1 → 2] smallest cluster too small"`,
			`GUARDIANN-REFIT near=10 far=1 mass=10 finalOwnership=[5 6] decision=KEEP_CURRENT reason="[1 → 2] candidate separation too low"`,
			`GUARDIANN-REFIT near=10 far=2 mass=12 finalOwnership=[10 2] decision=KEEP_CURRENT reason="[1 → 2] candidate too imbalanced"`,
			`GUARDIANN-REFIT near=20 far=1 mass=20 finalOwnership=[11 10] decision=KEEP_CURRENT reason="[1 → 2] candidate separation too low"`,
		}
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
		want := []string{
			"GUARDIANN-PEEL tenPlusOne n=11 bound=6 42:[10 1]I>10[5 6]K=selected 1:[10 1]I>10[5 6]K=selected 2:[10 1]I>10[6 5]K=selected 3:[10 1]I>10[5 6]K=selected 4:[10 1]I>10[6 5]K=selected 5:[10 1]I>10[5 6]K=selected 6:[1 10]I>10[5 6]K=selected 7:[10 1]I>10[5 6]K=selected",
			"GUARDIANN-PEEL twentyPlusOne n=21 bound=7 42:[20 1]I>20[11 10]K=selected 1:[20 1]I>20[10 11]K=selected 2:[20 1]I>20[11 10]K=selected 3:[20 1]I>20[11 10]K=selected 4:[20 1]I>20[11 10]K=selected 5:[20 1]I>20[11 10]K=selected 6:[20 1]I>20[12 9]K=selected 7:[20 1]I>20[10 11]K=selected",
			"GUARDIANN-PEEL twoOutliersOppositeSides n=20 bound=7 42:[19 1]I>19[19 1]I>18[11 9]K=selected 1:[19 1]I>19[19 1]I>18[10 10]K=selected 2:[19 1]I>19[19 1]I>18[11 9]K=selected 3:[19 1]I>19[19 1]I>18[10 10]K=selected 4:[1 19]I>19[19 1]I>18[11 9]K=selected 5:[19 1]I>19[19 1]I>18[11 9]K=selected 6:[19 1]I>19[19 1]I>18[10 10]K=selected 7:[19 1]I>19[19 1]I>18[10 10]K=selected",
			"GUARDIANN-PEEL nestedOutliers n=33 bound=8 42:[32 1]I>32[31 2]I>30[17 16]K=selected 1:[32 1]I>32[31 2]I>30[17 16]K=selected 2:[32 1]I>32[31 2]I>30[17 16]K=selected 3:[32 1]I>32[31 2]I>30[16 17]K=selected 4:[32 1]I>32[31 2]I>30[18 15]K=selected 5:[32 1]I>32[31 2]I>30[18 15]K=selected 6:[32 1]I>32[31 2]I>30[18 15]K=selected 7:[32 1]I>32[31 2]I>30[17 16]K=selected",
			"GUARDIANN-PEEL scatteredOutliers n=12 bound=6 42:[11 1]I>11[1 11]I>10[6 6]K=selected 1:[11 1]I>11[11 1]I>10[6 6]K=selected 2:[11 1]I>11[11 1]I>10[6 6]K=selected 3:[11 1]I>11[11 1]I>10[6 6]K=selected 4:[11 1]I>11[11 1]I>10[6 6]K=selected 5:[11 1]I>11[1 11]I>10[6 6]K=selected 6:[1 11]I>11[11 1]I>10[6 6]K=selected 7:[11 1]I>11[11 1]I>10[6 6]K=selected",
			"GUARDIANN-PEEL sixtyFortyAtFraction045 n=100 bound=9 42:[60 40]I>60[30 70]I>30[40 60]I>15[48 52]K=selected 1:[60 40]I>60[70 30]I>30[60 40]I>15[48 52]K=selected 2:[60 40]I>60[70 30]I>30[60 40]I>15[48 52]K=selected 3:[60 40]I>60[70 30]I>30[60 40]I>15[58 42]I>9[75 25]I>5[46 54]K=selected 4:[60 40]I>60[70 30]I>30[40 60]I>15[42 58]I>9[47 53]K=selected 5:[60 40]I>60[30 70]I>30[40 60]I>15[48 52]K=selected 6:[40 60]I>60[30 70]I>30[40 60]I>15[58 42]I>9[28 72]I>4[54 46]K=selected 7:[40 60]I>60[70 30]I>30[60 40]I>15[42 58]I>9[53 47]K=selected",
			"GUARDIANN-PEEL heavyTail n=16 bound=6 42:[2 14]K=initialUsable 1:[15 1]I>15[14 2]K=selected 2:[15 1]I>15[13 3]K=selected 3:[14 2]K=initialUsable 4:[2 14]K=initialUsable 5:[2 14]K=initialUsable 6:[14 2]K=initialUsable 7:[14 2]K=initialUsable",
			"GUARDIANN-PEEL identicalMassPlusOne n=11 bound=6 42:[10 1]I>10[11 0]I=undersizedChildHoldsNoMass 1:[10 1]I>10[11 0]I=undersizedChildHoldsNoMass 2:[10 1]I>10[11 0]I=undersizedChildHoldsNoMass 3:[10 1]I>10[11 0]I=undersizedChildHoldsNoMass 4:[10 1]I>10[11 0]I=undersizedChildHoldsNoMass 5:[10 1]I>10[11 0]I=undersizedChildHoldsNoMass 6:[1 10]I>10[11 0]I=undersizedChildHoldsNoMass 7:[10 1]I>10[11 0]I=undersizedChildHoldsNoMass",
			"GUARDIANN-PEEL twoOutliersOppositeSidesN1000 n=1000 bound=12 42:[999 1]I>999[999 1]I>998[503 497]K=selected 1:[999 1]I>999[999 1]I>998[501 499]K=selected 2:[999 1]I>999[999 1]I>998[497 503]K=selected 3:[999 1]I>999[999 1]I>998[497 503]K=selected 4:[999 1]I>999[999 1]I>998[499 501]K=selected 5:[999 1]I>999[999 1]I>998[497 503]K=selected 6:[999 1]I>999[999 1]I>998[497 503]K=selected 7:[999 1]I>999[999 1]I>998[499 501]K=selected",
			"GUARDIANN-PEEL nestedOutliersN1000 n=1000 bound=12 42:[999 1]I>999[998 2]I>997[502 498]K=selected 1:[999 1]I>999[998 2]I>997[502 498]K=selected 2:[999 1]I>999[998 2]I>997[500 500]K=selected 3:[999 1]I>999[998 2]I>997[500 500]K=selected 4:[999 1]I>999[998 2]I>997[502 498]K=selected 5:[999 1]I>999[998 2]I>997[502 498]K=selected 6:[999 1]I>999[998 2]I>997[501 499]K=selected 7:[999 1]I>999[998 2]I>997[502 498]K=selected",
			"GUARDIANN-PEEL scatteredOutliersN1000 n=1000 bound=12 42:[999 1]I>999[999 1]I>998[503 497]K=selected 1:[999 1]I>999[999 1]I>998[501 499]K=selected 2:[999 1]I>999[999 1]I>998[497 503]K=selected 3:[999 1]I>999[999 1]I>998[497 503]K=selected 4:[999 1]I>999[999 1]I>998[499 501]K=selected 5:[999 1]I>999[999 1]I>998[497 503]K=selected 6:[999 1]I>999[999 1]I>998[497 503]K=selected 7:[999 1]I>999[999 1]I>998[499 501]K=selected",
			"GUARDIANN-PEEL sixtyFortyAtFraction045N1000 n=1000 bound=12 42:[600 400]I>600[300 700]I>300[307 693]I>144[544 456]K=selected 1:[400 600]I>600[288 712]I>312[600 400]I>156[544 456]K=selected 2:[600 400]I>600[701 299]I>301[305 695]I>145[475 525]K=selected 3:[600 400]I>600[299 701]I>301[695 305]I>145[456 544]K=selected 4:[600 400]I>600[300 700]I>300[307 693]I>144[456 544]K=selected 5:[400 600]I>600[700 300]I>300[693 307]I>144[850 150]I>72[544 456]K=selected 6:[600 400]I>600[301 699]I>299[695 305]I>144[544 456]K=selected 7:[400 600]I>600[300 700]I>300[307 693]I>144[544 456]K=selected",
		}
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
		want := []string{
			"GUARDIANN-PEEL-HD tenPlusOne mode=child n=11 floorBound=3 42:[10 1]I rounds=1 last=10[5 6]K=selected 1:[10 1]I rounds=1 last=10[5 6]K=selected 2:[10 1]I rounds=1 last=10[6 5]K=selected 3:[10 1]I rounds=1 last=10[5 6]K=selected 4:[10 1]I rounds=1 last=10[6 5]K=selected 5:[10 1]I rounds=1 last=10[5 6]K=selected 6:[1 10]I rounds=1 last=10[5 6]K=selected 7:[10 1]I rounds=1 last=10[5 6]K=selected",
			"GUARDIANN-PEEL-HD twentyPlusOne mode=child n=21 floorBound=4 42:[20 1]I rounds=1 last=20[11 10]K=selected 1:[20 1]I rounds=1 last=20[10 11]K=selected 2:[20 1]I rounds=1 last=20[11 10]K=selected 3:[20 1]I rounds=1 last=20[11 10]K=selected 4:[20 1]I rounds=1 last=20[11 10]K=selected 5:[20 1]I rounds=1 last=20[11 10]K=selected 6:[20 1]I rounds=1 last=20[12 9]K=selected 7:[20 1]I rounds=1 last=20[10 11]K=selected",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSides mode=child n=20 floorBound=4 42:[19 1]I rounds=2 last=18[11 9]K=selected 1:[19 1]I rounds=2 last=18[10 10]K=selected 2:[19 1]I rounds=2 last=18[11 9]K=selected 3:[19 1]I rounds=2 last=18[10 10]K=selected 4:[1 19]I rounds=2 last=18[11 9]K=selected 5:[19 1]I rounds=2 last=18[11 9]K=selected 6:[19 1]I rounds=2 last=18[10 10]K=selected 7:[19 1]I rounds=2 last=18[10 10]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliers mode=child n=33 floorBound=5 42:[32 1]I rounds=2 last=30[17 16]K=selected 1:[32 1]I rounds=2 last=30[17 16]K=selected 2:[32 1]I rounds=2 last=30[17 16]K=selected 3:[32 1]I rounds=2 last=30[16 17]K=selected 4:[32 1]I rounds=2 last=30[18 15]K=selected 5:[32 1]I rounds=2 last=30[18 15]K=selected 6:[32 1]I rounds=2 last=30[18 15]K=selected 7:[32 1]I rounds=2 last=30[17 16]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliers mode=child n=12 floorBound=3 42:[11 1]I rounds=2 last=10[6 6]K=selected 1:[11 1]I rounds=2 last=10[6 6]K=selected 2:[11 1]I rounds=2 last=10[6 6]K=selected 3:[11 1]I rounds=2 last=10[6 6]K=selected 4:[11 1]I rounds=2 last=10[6 6]K=selected 5:[11 1]I rounds=2 last=10[6 6]K=selected 6:[1 11]I rounds=2 last=10[6 6]K=selected 7:[11 1]I rounds=2 last=10[6 6]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045 mode=child n=100 floorBound=6 42:[60 40]I rounds=3 last=15[48 52]K=selected 1:[60 40]I rounds=3 last=15[48 52]K=selected 2:[60 40]I rounds=3 last=15[48 52]K=selected 3:[60 40]I rounds=5 last=5[46 54]K=selected 4:[60 40]I rounds=4 last=9[47 53]K=selected 5:[60 40]I rounds=3 last=15[48 52]K=selected 6:[40 60]I rounds=5 last=4[54 46]K=selected 7:[40 60]I rounds=4 last=9[53 47]K=selected",
			"GUARDIANN-PEEL-HD heavyTail mode=child n=16 floorBound=3 42:[2 14]K rounds=0 last=-=initialUsable 1:[15 1]I rounds=1 last=15[14 2]K=selected 2:[15 1]I rounds=1 last=15[13 3]K=selected 3:[14 2]K rounds=0 last=-=initialUsable 4:[2 14]K rounds=0 last=-=initialUsable 5:[2 14]K rounds=0 last=-=initialUsable 6:[14 2]K rounds=0 last=-=initialUsable 7:[14 2]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD identicalMassPlusOne mode=child n=11 floorBound=3 42:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 1:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 2:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 3:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 4:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 5:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 6:[1 10]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 7:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSidesN1000 mode=child n=1000 floorBound=9 42:[999 1]I rounds=2 last=998[503 497]K=selected 1:[999 1]I rounds=2 last=998[501 499]K=selected 2:[999 1]I rounds=2 last=998[497 503]K=selected 3:[999 1]I rounds=2 last=998[497 503]K=selected 4:[999 1]I rounds=2 last=998[499 501]K=selected 5:[999 1]I rounds=2 last=998[497 503]K=selected 6:[999 1]I rounds=2 last=998[497 503]K=selected 7:[999 1]I rounds=2 last=998[499 501]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliersN1000 mode=child n=1000 floorBound=9 42:[999 1]I rounds=2 last=997[502 498]K=selected 1:[999 1]I rounds=2 last=997[502 498]K=selected 2:[999 1]I rounds=2 last=997[500 500]K=selected 3:[999 1]I rounds=2 last=997[500 500]K=selected 4:[999 1]I rounds=2 last=997[502 498]K=selected 5:[999 1]I rounds=2 last=997[502 498]K=selected 6:[999 1]I rounds=2 last=997[501 499]K=selected 7:[999 1]I rounds=2 last=997[502 498]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliersN1000 mode=child n=1000 floorBound=9 42:[999 1]I rounds=2 last=998[503 497]K=selected 1:[999 1]I rounds=2 last=998[501 499]K=selected 2:[999 1]I rounds=2 last=998[497 503]K=selected 3:[999 1]I rounds=2 last=998[497 503]K=selected 4:[999 1]I rounds=2 last=998[499 501]K=selected 5:[999 1]I rounds=2 last=998[497 503]K=selected 6:[999 1]I rounds=2 last=998[497 503]K=selected 7:[999 1]I rounds=2 last=998[499 501]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045N1000 mode=child n=1000 floorBound=9 42:[600 400]I rounds=3 last=144[544 456]K=selected 1:[400 600]I rounds=3 last=156[544 456]K=selected 2:[600 400]I rounds=3 last=145[475 525]K=selected 3:[600 400]I rounds=3 last=145[456 544]K=selected 4:[600 400]I rounds=3 last=144[456 544]K=selected 5:[400 600]I rounds=4 last=72[544 456]K=selected 6:[600 400]I rounds=3 last=144[544 456]K=selected 7:[400 600]I rounds=3 last=144[544 456]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers13R3N1000D128 mode=child n=1000 floorBound=9 42:[538 462]K rounds=0 last=-=initialUsable 1:[357 643]K rounds=0 last=-=initialUsable 2:[554 446]K rounds=0 last=-=initialUsable 3:[692 308]K rounds=0 last=-=initialUsable 4:[510 490]K rounds=0 last=-=initialUsable 5:[503 497]K rounds=0 last=-=initialUsable 6:[501 499]K rounds=0 last=-=initialUsable 7:[529 471]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R3N1000D128 mode=child n=1000 floorBound=9 42:[444 556]K rounds=0 last=-=initialUsable 1:[468 532]K rounds=0 last=-=initialUsable 2:[565 435]K rounds=0 last=-=initialUsable 3:[421 579]K rounds=0 last=-=initialUsable 4:[418 582]K rounds=0 last=-=initialUsable 5:[440 560]K rounds=0 last=-=initialUsable 6:[535 465]K rounds=0 last=-=initialUsable 7:[533 467]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N1000D128 mode=child n=1000 floorBound=9 42:[583 417]K rounds=0 last=-=initialUsable 1:[546 454]K rounds=0 last=-=initialUsable 2:[597 403]K rounds=0 last=-=initialUsable 3:[18 982]I rounds=1 last=982[529 471]K=selected 4:[393 607]K rounds=0 last=-=initialUsable 5:[448 552]K rounds=0 last=-=initialUsable 6:[521 479]K rounds=0 last=-=initialUsable 7:[528 472]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R1p5N1000D128 mode=child n=1000 floorBound=9 42:[414 586]K rounds=0 last=-=initialUsable 1:[510 490]K rounds=0 last=-=initialUsable 2:[559 441]K rounds=0 last=-=initialUsable 3:[425 575]K rounds=0 last=-=initialUsable 4:[492 508]K rounds=0 last=-=initialUsable 5:[520 480]K rounds=0 last=-=initialUsable 6:[510 490]K rounds=0 last=-=initialUsable 7:[494 506]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R1p5N1000D128 mode=child n=1000 floorBound=9 42:[415 585]K rounds=0 last=-=initialUsable 1:[559 441]K rounds=0 last=-=initialUsable 2:[535 465]K rounds=0 last=-=initialUsable 3:[633 367]K rounds=0 last=-=initialUsable 4:[495 505]K rounds=0 last=-=initialUsable 5:[479 521]K rounds=0 last=-=initialUsable 6:[573 427]K rounds=0 last=-=initialUsable 7:[623 377]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R10N1000D128 mode=child n=1000 floorBound=9 42:[11 989]I rounds=5 last=982[307 693]K=selected 1:[999 1]I rounds=5 last=983[527 473]K=selected 2:[989 11]I rounds=3 last=984[568 432]K=selected 3:[999 1]I rounds=4 last=987[438 562]K=selected 4:[14 986]I rounds=2 last=983[559 441]K=selected 5:[987 13]I rounds=3 last=981[494 506]K=selected 6:[990 10]I rounds=2 last=983[663 337]K=selected 7:[8 992]I rounds=4 last=981[567 433]K=selected",
			"GUARDIANN-PEEL-HD hdTight13N1000D128 mode=child n=1000 floorBound=9 42:[999 1]I rounds=8 last=987[453 547]K=selected 1:[999 1]I rounds=10 last=987[441 559]K=selected 2:[999 1]I rounds=9 last=987[441 559]K=selected 3:[999 1]I rounds=6 last=987[397 603]K=selected 4:[999 1]I rounds=8 last=987[628 372]K=selected 5:[999 1]I rounds=7 last=987[628 372]K=selected 6:[999 1]I rounds=11 last=987[499 501]K=selected 7:[999 1]I rounds=11 last=987[505 495]K=selected",
			"GUARDIANN-PEEL-HD hdTight20N1000D128 mode=child n=1000 floorBound=9 42:[999 1]I rounds=14 last=980[499 501]K=selected 1:[999 1]I rounds=18 last=980[504 496]K=selected 2:[999 1]I rounds=15 last=980[402 598]K=selected 3:[999 1]I rounds=17 last=980[468 532]K=selected 4:[999 1]I rounds=14 last=980[471 529]K=selected 5:[999 1]I rounds=15 last=980[468 532]K=selected 6:[999 1]I rounds=12 last=980[471 529]K=selected 7:[999 1]I rounds=15 last=980[519 481]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N1000D128 mode=child n=1000 floorBound=9 42:[999 1]I rounds=22 last=950[641 359]K=selected 1:[999 1]I rounds=26 last=950[546 454]K=selected 2:[999 1]I rounds=29 last=950[569 431]K=selected 3:[999 1]I rounds=33 last=950[429 571]K=selected 4:[999 1]I rounds=30 last=950[519 481]K=selected 5:[999 1]I rounds=18 last=950[498 502]K=selected 6:[999 1]I rounds=31 last=950[442 558]K=selected 7:[999 1]I rounds=27 last=950[519 481]K=selected",
			"GUARDIANN-PEEL-HD hdTight20Sigma0p1N1000D128 mode=child n=1000 floorBound=9 42:[987 13]I rounds=4 last=984[706 294]K=selected 1:[988 12]I rounds=2 last=982[562 438]K=selected 2:[988 12]I rounds=5 last=983[722 278]K=selected 3:[999 1]I rounds=4 last=981[448 552]K=selected 4:[988 12]I rounds=2 last=982[539 461]K=selected 5:[11 989]I rounds=4 last=980[407 593]K=selected 6:[12 988]I rounds=2 last=984[601 399]K=selected 7:[985 15]I rounds=1 last=985[395 605]K=selected",
			"GUARDIANN-PEEL-HD hdTightRising50N1000D128 mode=child n=1000 floorBound=9 42:[999 1]I rounds=50 last=950[390 610]K=selected 1:[999 1]I rounds=50 last=950[560 440]K=selected 2:[999 1]I rounds=50 last=950[589 411]K=selected 3:[999 1]I rounds=42 last=950[496 504]K=selected 4:[999 1]I rounds=36 last=950[482 518]K=selected 5:[999 1]I rounds=49 last=950[471 529]K=selected 6:[999 1]I rounds=50 last=950[583 417]K=selected 7:[999 1]I rounds=36 last=950[454 546]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D128 mode=child n=2000 floorBound=10 42:[1175 825]K rounds=0 last=-=initialUsable 1:[1221 779]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D768 mode=child n=2000 floorBound=10 42:[1028 972]K rounds=0 last=-=initialUsable 1:[1859 141]I rounds=2 last=1858[1313 687]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N2000D768 mode=child n=2000 floorBound=10 42:[1999 1]I rounds=30 last=1950[907 1093]K=selected 1:[1999 1]I rounds=23 last=1950[974 1026]K=selected",
			"GUARDIANN-PEEL-HD tenPlusOne mode=tail n=11 floorBound=3 42:[10 1]I rounds=1 last=10[5 6]K=selected 1:[10 1]I rounds=1 last=10[5 6]K=selected 2:[10 1]I rounds=1 last=10[6 5]K=selected 3:[10 1]I rounds=1 last=10[5 6]K=selected 4:[10 1]I rounds=1 last=10[6 5]K=selected 5:[10 1]I rounds=1 last=10[5 6]K=selected 6:[1 10]I rounds=1 last=10[5 6]K=selected 7:[10 1]I rounds=1 last=10[5 6]K=selected",
			"GUARDIANN-PEEL-HD twentyPlusOne mode=tail n=21 floorBound=4 42:[20 1]I rounds=1 last=20[11 10]K=selected 1:[20 1]I rounds=1 last=20[10 11]K=selected 2:[20 1]I rounds=1 last=20[11 10]K=selected 3:[20 1]I rounds=1 last=20[11 10]K=selected 4:[20 1]I rounds=1 last=20[11 10]K=selected 5:[20 1]I rounds=1 last=20[11 10]K=selected 6:[20 1]I rounds=1 last=20[12 9]K=selected 7:[20 1]I rounds=1 last=20[10 11]K=selected",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSides mode=tail n=20 floorBound=4 42:[19 1]I rounds=2 last=18[11 9]K=selected 1:[19 1]I rounds=2 last=18[10 10]K=selected 2:[19 1]I rounds=2 last=18[11 9]K=selected 3:[19 1]I rounds=2 last=18[10 10]K=selected 4:[1 19]I rounds=2 last=18[11 9]K=selected 5:[19 1]I rounds=2 last=18[11 9]K=selected 6:[19 1]I rounds=2 last=18[10 10]K=selected 7:[19 1]I rounds=2 last=18[10 10]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliers mode=tail n=33 floorBound=5 42:[32 1]I rounds=2 last=30[17 16]K=selected 1:[32 1]I rounds=2 last=30[17 16]K=selected 2:[32 1]I rounds=2 last=30[17 16]K=selected 3:[32 1]I rounds=2 last=30[16 17]K=selected 4:[32 1]I rounds=2 last=30[18 15]K=selected 5:[32 1]I rounds=2 last=30[18 15]K=selected 6:[32 1]I rounds=2 last=30[18 15]K=selected 7:[32 1]I rounds=2 last=30[17 16]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliers mode=tail n=12 floorBound=3 42:[11 1]I rounds=2 last=10[6 6]K=selected 1:[11 1]I rounds=2 last=10[6 6]K=selected 2:[11 1]I rounds=2 last=10[6 6]K=selected 3:[11 1]I rounds=2 last=10[6 6]K=selected 4:[11 1]I rounds=2 last=10[6 6]K=selected 5:[11 1]I rounds=2 last=10[6 6]K=selected 6:[1 11]I rounds=2 last=10[6 6]K=selected 7:[11 1]I rounds=2 last=10[6 6]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045 mode=tail n=100 floorBound=6 42:[60 40]I rounds=4 last=2[52 48]K=selected 1:[60 40]I rounds=3 last=9[51 49]K=selected 2:[60 40]I rounds=3 last=9[49 51]K=selected 3:[60 40]I rounds=5 last=3[51 49]K=selected 4:[60 40]I rounds=3 last=9[53 47]K=selected 5:[60 40]I rounds=3 last=9[49 51]K=selected 6:[40 60]I rounds=3 last=9[53 47]K=selected 7:[40 60]I rounds=4 last=2[48 52]K=selected",
			"GUARDIANN-PEEL-HD heavyTail mode=tail n=16 floorBound=3 42:[2 14]K rounds=0 last=-=initialUsable 1:[15 1]I rounds=1 last=15[14 2]K=selected 2:[15 1]I rounds=1 last=15[13 3]K=selected 3:[14 2]K rounds=0 last=-=initialUsable 4:[2 14]K rounds=0 last=-=initialUsable 5:[2 14]K rounds=0 last=-=initialUsable 6:[14 2]K rounds=0 last=-=initialUsable 7:[14 2]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD identicalMassPlusOne mode=tail n=11 floorBound=3 42:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 1:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 2:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 3:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 4:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 5:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 6:[1 10]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 7:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSidesN1000 mode=tail n=1000 floorBound=9 42:[999 1]I rounds=2 last=998[503 497]K=selected 1:[999 1]I rounds=2 last=998[501 499]K=selected 2:[999 1]I rounds=2 last=998[497 503]K=selected 3:[999 1]I rounds=2 last=998[497 503]K=selected 4:[999 1]I rounds=2 last=998[499 501]K=selected 5:[999 1]I rounds=2 last=998[497 503]K=selected 6:[999 1]I rounds=2 last=998[497 503]K=selected 7:[999 1]I rounds=2 last=998[499 501]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliersN1000 mode=tail n=1000 floorBound=9 42:[999 1]I rounds=2 last=997[502 498]K=selected 1:[999 1]I rounds=2 last=997[502 498]K=selected 2:[999 1]I rounds=2 last=997[500 500]K=selected 3:[999 1]I rounds=2 last=997[500 500]K=selected 4:[999 1]I rounds=2 last=997[502 498]K=selected 5:[999 1]I rounds=2 last=997[502 498]K=selected 6:[999 1]I rounds=2 last=997[501 499]K=selected 7:[999 1]I rounds=2 last=997[502 498]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliersN1000 mode=tail n=1000 floorBound=9 42:[999 1]I rounds=2 last=998[503 497]K=selected 1:[999 1]I rounds=2 last=998[501 499]K=selected 2:[999 1]I rounds=2 last=998[497 503]K=selected 3:[999 1]I rounds=2 last=998[497 503]K=selected 4:[999 1]I rounds=2 last=998[499 501]K=selected 5:[999 1]I rounds=2 last=998[497 503]K=selected 6:[999 1]I rounds=2 last=998[497 503]K=selected 7:[999 1]I rounds=2 last=998[499 501]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045N1000 mode=tail n=1000 floorBound=9 42:[600 400]I rounds=2 last=123[450 550]K=selected 1:[400 600]I rounds=4 last=9[511 489]K=selected 2:[600 400]I rounds=4 last=12[505 495]K=selected 3:[600 400]I rounds=2 last=132[550 450]K=selected 4:[600 400]I rounds=3 last=28[496 504]K=selected 5:[400 600]I rounds=3 last=28[456 544]K=selected 6:[600 400]I rounds=3 last=28[539 461]K=selected 7:[400 600]I rounds=4 last=9[499 501]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers13R3N1000D128 mode=tail n=1000 floorBound=9 42:[538 462]K rounds=0 last=-=initialUsable 1:[357 643]K rounds=0 last=-=initialUsable 2:[554 446]K rounds=0 last=-=initialUsable 3:[692 308]K rounds=0 last=-=initialUsable 4:[510 490]K rounds=0 last=-=initialUsable 5:[503 497]K rounds=0 last=-=initialUsable 6:[501 499]K rounds=0 last=-=initialUsable 7:[529 471]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R3N1000D128 mode=tail n=1000 floorBound=9 42:[444 556]K rounds=0 last=-=initialUsable 1:[468 532]K rounds=0 last=-=initialUsable 2:[565 435]K rounds=0 last=-=initialUsable 3:[421 579]K rounds=0 last=-=initialUsable 4:[418 582]K rounds=0 last=-=initialUsable 5:[440 560]K rounds=0 last=-=initialUsable 6:[535 465]K rounds=0 last=-=initialUsable 7:[533 467]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N1000D128 mode=tail n=1000 floorBound=9 42:[583 417]K rounds=0 last=-=initialUsable 1:[546 454]K rounds=0 last=-=initialUsable 2:[597 403]K rounds=0 last=-=initialUsable 3:[18 982]I rounds=1 last=948[424 576]K=selected 4:[393 607]K rounds=0 last=-=initialUsable 5:[448 552]K rounds=0 last=-=initialUsable 6:[521 479]K rounds=0 last=-=initialUsable 7:[528 472]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R1p5N1000D128 mode=tail n=1000 floorBound=9 42:[414 586]K rounds=0 last=-=initialUsable 1:[510 490]K rounds=0 last=-=initialUsable 2:[559 441]K rounds=0 last=-=initialUsable 3:[425 575]K rounds=0 last=-=initialUsable 4:[492 508]K rounds=0 last=-=initialUsable 5:[520 480]K rounds=0 last=-=initialUsable 6:[510 490]K rounds=0 last=-=initialUsable 7:[494 506]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R1p5N1000D128 mode=tail n=1000 floorBound=9 42:[415 585]K rounds=0 last=-=initialUsable 1:[559 441]K rounds=0 last=-=initialUsable 2:[535 465]K rounds=0 last=-=initialUsable 3:[633 367]K rounds=0 last=-=initialUsable 4:[495 505]K rounds=0 last=-=initialUsable 5:[479 521]K rounds=0 last=-=initialUsable 6:[573 427]K rounds=0 last=-=initialUsable 7:[623 377]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R10N1000D128 mode=tail n=1000 floorBound=9 42:[11 989]I rounds=4 last=985[754 246]K=selected 1:[999 1]I rounds=4 last=984[603 397]K=selected 2:[989 11]I rounds=4 last=983[559 441]K=selected 3:[999 1]I rounds=4 last=987[438 562]K=selected 4:[14 986]I rounds=2 last=983[559 441]K=selected 5:[987 13]I rounds=3 last=981[494 506]K=selected 6:[990 10]I rounds=2 last=983[663 337]K=selected 7:[8 992]I rounds=4 last=982[536 464]K=selected",
			"GUARDIANN-PEEL-HD hdTight13N1000D128 mode=tail n=1000 floorBound=9 42:[999 1]I rounds=8 last=987[453 547]K=selected 1:[999 1]I rounds=10 last=987[441 559]K=selected 2:[999 1]I rounds=9 last=987[441 559]K=selected 3:[999 1]I rounds=6 last=987[397 603]K=selected 4:[999 1]I rounds=8 last=987[628 372]K=selected 5:[999 1]I rounds=7 last=987[628 372]K=selected 6:[999 1]I rounds=11 last=987[499 501]K=selected 7:[999 1]I rounds=11 last=987[505 495]K=selected",
			"GUARDIANN-PEEL-HD hdTight20N1000D128 mode=tail n=1000 floorBound=9 42:[999 1]I rounds=14 last=980[499 501]K=selected 1:[999 1]I rounds=18 last=980[504 496]K=selected 2:[999 1]I rounds=15 last=980[402 598]K=selected 3:[999 1]I rounds=17 last=980[468 532]K=selected 4:[999 1]I rounds=14 last=980[471 529]K=selected 5:[999 1]I rounds=15 last=980[468 532]K=selected 6:[999 1]I rounds=12 last=980[471 529]K=selected 7:[999 1]I rounds=15 last=980[519 481]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N1000D128 mode=tail n=1000 floorBound=9 42:[999 1]I rounds=17 last=950[523 477]K=selected 1:[999 1]I rounds=25 last=950[350 650]K=selected 2:[999 1]I rounds=18 last=950[547 453]K=selected 3:[999 1]I rounds=14 last=950[521 479]K=selected 4:[999 1]I rounds=18 last=950[557 443]K=selected 5:[999 1]I rounds=17 last=950[557 443]K=selected 6:[999 1]I rounds=28 last=950[519 481]K=selected 7:[999 1]I rounds=13 last=950[547 453]K=selected",
			"GUARDIANN-PEEL-HD hdTight20Sigma0p1N1000D128 mode=tail n=1000 floorBound=9 42:[987 13]I rounds=2 last=982[394 606]K=selected 1:[988 12]I rounds=2 last=982[562 438]K=selected 2:[988 12]I rounds=5 last=983[722 278]K=selected 3:[999 1]I rounds=5 last=980[470 530]K=selected 4:[988 12]I rounds=2 last=982[539 461]K=selected 5:[11 989]I rounds=2 last=985[641 359]K=selected 6:[12 988]I rounds=2 last=984[601 399]K=selected 7:[985 15]I rounds=1 last=984[696 304]K=selected",
			"GUARDIANN-PEEL-HD hdTightRising50N1000D128 mode=tail n=1000 floorBound=9 42:[999 1]I rounds=13 last=950[628 372]K=selected 1:[999 1]I rounds=18 last=950[344 656]K=selected 2:[999 1]I rounds=18 last=950[611 389]K=selected 3:[999 1]I rounds=17 last=950[611 389]K=selected 4:[999 1]I rounds=16 last=950[611 389]K=selected 5:[999 1]I rounds=13 last=950[563 437]K=selected 6:[999 1]I rounds=17 last=950[497 503]K=selected 7:[999 1]I rounds=15 last=950[488 512]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D128 mode=tail n=2000 floorBound=10 42:[1175 825]K rounds=0 last=-=initialUsable 1:[1221 779]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D768 mode=tail n=2000 floorBound=10 42:[1028 972]K rounds=0 last=-=initialUsable 1:[1859 141]I rounds=1 last=77[876 1124]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N2000D768 mode=tail n=2000 floorBound=10 42:[1999 1]I rounds=30 last=1950[907 1093]K=selected 1:[1999 1]I rounds=23 last=1950[974 1026]K=selected",
			"GUARDIANN-PEEL-HD tenPlusOne mode=floor n=11 floorBound=3 42:[10 1]I rounds=1 last=10[5 6]K=selected 1:[10 1]I rounds=1 last=10[5 6]K=selected 2:[10 1]I rounds=1 last=10[6 5]K=selected 3:[10 1]I rounds=1 last=10[5 6]K=selected 4:[10 1]I rounds=1 last=10[6 5]K=selected 5:[10 1]I rounds=1 last=10[5 6]K=selected 6:[1 10]I rounds=1 last=10[5 6]K=selected 7:[10 1]I rounds=1 last=10[5 6]K=selected",
			"GUARDIANN-PEEL-HD twentyPlusOne mode=floor n=21 floorBound=4 42:[20 1]I rounds=1 last=20[11 10]K=selected 1:[20 1]I rounds=1 last=20[10 11]K=selected 2:[20 1]I rounds=1 last=20[11 10]K=selected 3:[20 1]I rounds=1 last=20[11 10]K=selected 4:[20 1]I rounds=1 last=20[11 10]K=selected 5:[20 1]I rounds=1 last=20[11 10]K=selected 6:[20 1]I rounds=1 last=20[12 9]K=selected 7:[20 1]I rounds=1 last=20[10 11]K=selected",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSides mode=floor n=20 floorBound=4 42:[19 1]I rounds=2 last=17[10 10]K=selected 1:[19 1]I rounds=2 last=17[9 11]K=selected 2:[19 1]I rounds=2 last=17[9 11]K=selected 3:[19 1]I rounds=2 last=17[9 11]K=selected 4:[1 19]I rounds=2 last=17[9 11]K=selected 5:[19 1]I rounds=2 last=17[11 9]K=selected 6:[19 1]I rounds=2 last=17[11 9]K=selected 7:[19 1]I rounds=2 last=17[9 11]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliers mode=floor n=33 floorBound=5 42:[32 1]I rounds=2 last=30[17 16]K=selected 1:[32 1]I rounds=2 last=30[17 16]K=selected 2:[32 1]I rounds=2 last=30[17 16]K=selected 3:[32 1]I rounds=2 last=30[16 17]K=selected 4:[32 1]I rounds=2 last=30[18 15]K=selected 5:[32 1]I rounds=2 last=30[18 15]K=selected 6:[32 1]I rounds=2 last=30[18 15]K=selected 7:[32 1]I rounds=2 last=30[17 16]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliers mode=floor n=12 floorBound=3 42:[11 1]I rounds=2 last=9[5 7]K=selected 1:[11 1]I rounds=2 last=9[5 7]K=selected 2:[11 1]I rounds=2 last=9[5 7]K=selected 3:[11 1]I rounds=2 last=9[5 7]K=selected 4:[11 1]I rounds=2 last=9[5 7]K=selected 5:[11 1]I rounds=2 last=9[6 6]K=selected 6:[1 11]I rounds=2 last=9[7 5]K=selected 7:[11 1]I rounds=2 last=9[7 5]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045 mode=floor n=100 floorBound=6 42:[60 40]I rounds=3 last=15[48 52]K=selected 1:[60 40]I rounds=3 last=15[48 52]K=selected 2:[60 40]I rounds=3 last=15[48 52]K=selected 3:[60 40]I rounds=5 last=5[46 54]K=selected 4:[60 40]I rounds=4 last=9[47 53]K=selected 5:[60 40]I rounds=3 last=15[48 52]K=selected 6:[40 60]I rounds=5 last=4[54 46]K=selected 7:[40 60]I rounds=4 last=9[53 47]K=selected",
			"GUARDIANN-PEEL-HD heavyTail mode=floor n=16 floorBound=3 42:[2 14]K rounds=0 last=-=initialUsable 1:[15 1]I rounds=1 last=15[14 2]K=selected 2:[15 1]I rounds=1 last=15[13 3]K=selected 3:[14 2]K rounds=0 last=-=initialUsable 4:[2 14]K rounds=0 last=-=initialUsable 5:[2 14]K rounds=0 last=-=initialUsable 6:[14 2]K rounds=0 last=-=initialUsable 7:[14 2]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD identicalMassPlusOne mode=floor n=11 floorBound=3 42:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 1:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 2:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 3:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 4:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 5:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 6:[1 10]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 7:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSidesN1000 mode=floor n=1000 floorBound=9 42:[999 1]I rounds=2 last=997[503 497]K=selected 1:[999 1]I rounds=2 last=997[503 497]K=selected 2:[999 1]I rounds=2 last=997[498 502]K=selected 3:[999 1]I rounds=2 last=997[500 500]K=selected 4:[999 1]I rounds=2 last=997[503 497]K=selected 5:[999 1]I rounds=2 last=997[503 497]K=selected 6:[999 1]I rounds=2 last=997[502 498]K=selected 7:[999 1]I rounds=2 last=997[497 503]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliersN1000 mode=floor n=1000 floorBound=9 42:[999 1]I rounds=2 last=997[502 498]K=selected 1:[999 1]I rounds=2 last=997[502 498]K=selected 2:[999 1]I rounds=2 last=997[500 500]K=selected 3:[999 1]I rounds=2 last=997[500 500]K=selected 4:[999 1]I rounds=2 last=997[502 498]K=selected 5:[999 1]I rounds=2 last=997[502 498]K=selected 6:[999 1]I rounds=2 last=997[501 499]K=selected 7:[999 1]I rounds=2 last=997[502 498]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliersN1000 mode=floor n=1000 floorBound=9 42:[999 1]I rounds=2 last=997[503 497]K=selected 1:[999 1]I rounds=2 last=997[503 497]K=selected 2:[999 1]I rounds=2 last=997[498 502]K=selected 3:[999 1]I rounds=2 last=997[500 500]K=selected 4:[999 1]I rounds=2 last=997[503 497]K=selected 5:[999 1]I rounds=2 last=997[503 497]K=selected 6:[999 1]I rounds=2 last=997[502 498]K=selected 7:[999 1]I rounds=2 last=997[497 503]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045N1000 mode=floor n=1000 floorBound=9 42:[600 400]I rounds=3 last=144[544 456]K=selected 1:[400 600]I rounds=3 last=156[544 456]K=selected 2:[600 400]I rounds=3 last=145[475 525]K=selected 3:[600 400]I rounds=3 last=145[456 544]K=selected 4:[600 400]I rounds=3 last=144[456 544]K=selected 5:[400 600]I rounds=4 last=72[544 456]K=selected 6:[600 400]I rounds=3 last=144[544 456]K=selected 7:[400 600]I rounds=3 last=144[544 456]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers13R3N1000D128 mode=floor n=1000 floorBound=9 42:[538 462]K rounds=0 last=-=initialUsable 1:[357 643]K rounds=0 last=-=initialUsable 2:[554 446]K rounds=0 last=-=initialUsable 3:[692 308]K rounds=0 last=-=initialUsable 4:[510 490]K rounds=0 last=-=initialUsable 5:[503 497]K rounds=0 last=-=initialUsable 6:[501 499]K rounds=0 last=-=initialUsable 7:[529 471]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R3N1000D128 mode=floor n=1000 floorBound=9 42:[444 556]K rounds=0 last=-=initialUsable 1:[468 532]K rounds=0 last=-=initialUsable 2:[565 435]K rounds=0 last=-=initialUsable 3:[421 579]K rounds=0 last=-=initialUsable 4:[418 582]K rounds=0 last=-=initialUsable 5:[440 560]K rounds=0 last=-=initialUsable 6:[535 465]K rounds=0 last=-=initialUsable 7:[533 467]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N1000D128 mode=floor n=1000 floorBound=9 42:[583 417]K rounds=0 last=-=initialUsable 1:[546 454]K rounds=0 last=-=initialUsable 2:[597 403]K rounds=0 last=-=initialUsable 3:[18 982]I rounds=1 last=982[529 471]K=selected 4:[393 607]K rounds=0 last=-=initialUsable 5:[448 552]K rounds=0 last=-=initialUsable 6:[521 479]K rounds=0 last=-=initialUsable 7:[528 472]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R1p5N1000D128 mode=floor n=1000 floorBound=9 42:[414 586]K rounds=0 last=-=initialUsable 1:[510 490]K rounds=0 last=-=initialUsable 2:[559 441]K rounds=0 last=-=initialUsable 3:[425 575]K rounds=0 last=-=initialUsable 4:[492 508]K rounds=0 last=-=initialUsable 5:[520 480]K rounds=0 last=-=initialUsable 6:[510 490]K rounds=0 last=-=initialUsable 7:[494 506]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R1p5N1000D128 mode=floor n=1000 floorBound=9 42:[415 585]K rounds=0 last=-=initialUsable 1:[559 441]K rounds=0 last=-=initialUsable 2:[535 465]K rounds=0 last=-=initialUsable 3:[633 367]K rounds=0 last=-=initialUsable 4:[495 505]K rounds=0 last=-=initialUsable 5:[479 521]K rounds=0 last=-=initialUsable 6:[573 427]K rounds=0 last=-=initialUsable 7:[623 377]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R10N1000D128 mode=floor n=1000 floorBound=9 42:[11 989]I rounds=4 last=985[398 602]K=selected 1:[999 1]I rounds=5 last=969[529 471]K=selected 2:[989 11]I rounds=3 last=984[568 432]K=selected 3:[999 1]I rounds=4 last=985[572 428]K=selected 4:[14 986]I rounds=2 last=983[559 441]K=selected 5:[987 13]I rounds=3 last=981[494 506]K=selected 6:[990 10]I rounds=2 last=983[663 337]K=selected 7:[8 992]I rounds=4 last=981[567 433]K=selected",
			"GUARDIANN-PEEL-HD hdTight13N1000D128 mode=floor n=1000 floorBound=9 42:[999 1]I rounds=4 last=985[502 498]K=selected 1:[999 1]I rounds=4 last=985[600 400]K=selected 2:[999 1]I rounds=4 last=985[542 458]K=selected 3:[999 1]I rounds=4 last=985[457 543]K=selected 4:[999 1]I rounds=4 last=985[563 437]K=selected 5:[999 1]I rounds=4 last=985[432 568]K=selected 6:[999 1]I rounds=4 last=985[512 488]K=selected 7:[999 1]I rounds=4 last=985[535 465]K=selected",
			"GUARDIANN-PEEL-HD hdTight20N1000D128 mode=floor n=1000 floorBound=9 42:[999 1]I rounds=5 last=969[530 470]K=selected 1:[999 1]I rounds=5 last=969[506 494]K=selected 2:[999 1]I rounds=5 last=969[561 439]K=selected 3:[999 1]I rounds=5 last=969[536 464]K=selected 4:[999 1]I rounds=5 last=969[571 429]K=selected 5:[999 1]I rounds=5 last=969[533 467]K=selected 6:[999 1]I rounds=5 last=969[566 434]K=selected 7:[999 1]I rounds=5 last=969[445 555]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N1000D128 mode=floor n=1000 floorBound=9 42:[999 1]I rounds=6 last=937[439 561]K=selected 1:[999 1]I rounds=6 last=937[431 569]K=selected 2:[999 1]I rounds=6 last=937[445 555]K=selected 3:[999 1]I rounds=6 last=937[548 452]K=selected 4:[999 1]I rounds=6 last=937[591 409]K=selected 5:[999 1]I rounds=6 last=937[520 480]K=selected 6:[999 1]I rounds=6 last=937[415 585]K=selected 7:[999 1]I rounds=6 last=937[438 562]K=selected",
			"GUARDIANN-PEEL-HD hdTight20Sigma0p1N1000D128 mode=floor n=1000 floorBound=9 42:[987 13]I rounds=4 last=984[706 294]K=selected 1:[988 12]I rounds=2 last=982[562 438]K=selected 2:[988 12]I rounds=5 last=969[591 409]K=selected 3:[999 1]I rounds=4 last=984[561 439]K=selected 4:[988 12]I rounds=2 last=982[539 461]K=selected 5:[11 989]I rounds=4 last=980[407 593]K=selected 6:[12 988]I rounds=2 last=984[601 399]K=selected 7:[985 15]I rounds=1 last=985[395 605]K=selected",
			"GUARDIANN-PEEL-HD hdTightRising50N1000D128 mode=floor n=1000 floorBound=9 42:[999 1]I rounds=6 last=937[557 443]K=selected 1:[999 1]I rounds=6 last=937[524 476]K=selected 2:[999 1]I rounds=6 last=937[533 467]K=selected 3:[999 1]I rounds=6 last=937[433 567]K=selected 4:[999 1]I rounds=6 last=937[436 564]K=selected 5:[999 1]I rounds=6 last=937[520 480]K=selected 6:[999 1]I rounds=6 last=937[558 442]K=selected 7:[999 1]I rounds=6 last=937[533 467]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D128 mode=floor n=2000 floorBound=10 42:[1175 825]K rounds=0 last=-=initialUsable 1:[1221 779]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D768 mode=floor n=2000 floorBound=10 42:[1028 972]K rounds=0 last=-=initialUsable 1:[1859 141]I rounds=2 last=1858[1313 687]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N2000D768 mode=floor n=2000 floorBound=10 42:[1999 1]I rounds=6 last=1937[1001 999]K=selected 1:[1999 1]I rounds=6 last=1937[1074 926]K=selected",
			"GUARDIANN-PEEL-HD tenPlusOne mode=floor-sample:256 n=11 floorBound=3 42:[10 1]I rounds=1 last=10[5 6]K=selected 1:[10 1]I rounds=1 last=10[5 6]K=selected 2:[10 1]I rounds=1 last=10[6 5]K=selected 3:[10 1]I rounds=1 last=10[5 6]K=selected 4:[10 1]I rounds=1 last=10[6 5]K=selected 5:[10 1]I rounds=1 last=10[5 6]K=selected 6:[1 10]I rounds=1 last=10[5 6]K=selected 7:[10 1]I rounds=1 last=10[5 6]K=selected",
			"GUARDIANN-PEEL-HD twentyPlusOne mode=floor-sample:256 n=21 floorBound=4 42:[20 1]I rounds=1 last=20[11 10]K=selected 1:[20 1]I rounds=1 last=20[10 11]K=selected 2:[20 1]I rounds=1 last=20[11 10]K=selected 3:[20 1]I rounds=1 last=20[11 10]K=selected 4:[20 1]I rounds=1 last=20[11 10]K=selected 5:[20 1]I rounds=1 last=20[11 10]K=selected 6:[20 1]I rounds=1 last=20[12 9]K=selected 7:[20 1]I rounds=1 last=20[10 11]K=selected",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSides mode=floor-sample:256 n=20 floorBound=4 42:[19 1]I rounds=2 last=17[10 10]K=selected 1:[19 1]I rounds=2 last=17[9 11]K=selected 2:[19 1]I rounds=2 last=17[9 11]K=selected 3:[19 1]I rounds=2 last=17[9 11]K=selected 4:[1 19]I rounds=2 last=17[9 11]K=selected 5:[19 1]I rounds=2 last=17[11 9]K=selected 6:[19 1]I rounds=2 last=17[11 9]K=selected 7:[19 1]I rounds=2 last=17[9 11]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliers mode=floor-sample:256 n=33 floorBound=5 42:[32 1]I rounds=2 last=30[17 16]K=selected 1:[32 1]I rounds=2 last=30[17 16]K=selected 2:[32 1]I rounds=2 last=30[17 16]K=selected 3:[32 1]I rounds=2 last=30[16 17]K=selected 4:[32 1]I rounds=2 last=30[18 15]K=selected 5:[32 1]I rounds=2 last=30[18 15]K=selected 6:[32 1]I rounds=2 last=30[18 15]K=selected 7:[32 1]I rounds=2 last=30[17 16]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliers mode=floor-sample:256 n=12 floorBound=3 42:[11 1]I rounds=2 last=9[5 7]K=selected 1:[11 1]I rounds=2 last=9[5 7]K=selected 2:[11 1]I rounds=2 last=9[5 7]K=selected 3:[11 1]I rounds=2 last=9[5 7]K=selected 4:[11 1]I rounds=2 last=9[5 7]K=selected 5:[11 1]I rounds=2 last=9[6 6]K=selected 6:[1 11]I rounds=2 last=9[7 5]K=selected 7:[11 1]I rounds=2 last=9[7 5]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045 mode=floor-sample:256 n=100 floorBound=6 42:[60 40]I rounds=3 last=15[48 52]K=selected 1:[60 40]I rounds=3 last=15[48 52]K=selected 2:[60 40]I rounds=3 last=15[48 52]K=selected 3:[60 40]I rounds=5 last=5[46 54]K=selected 4:[60 40]I rounds=4 last=9[47 53]K=selected 5:[60 40]I rounds=3 last=15[48 52]K=selected 6:[40 60]I rounds=5 last=4[54 46]K=selected 7:[40 60]I rounds=4 last=9[53 47]K=selected",
			"GUARDIANN-PEEL-HD heavyTail mode=floor-sample:256 n=16 floorBound=3 42:[2 14]K rounds=0 last=-=initialUsable 1:[15 1]I rounds=1 last=15[14 2]K=selected 2:[15 1]I rounds=1 last=15[13 3]K=selected 3:[14 2]K rounds=0 last=-=initialUsable 4:[2 14]K rounds=0 last=-=initialUsable 5:[2 14]K rounds=0 last=-=initialUsable 6:[14 2]K rounds=0 last=-=initialUsable 7:[14 2]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD identicalMassPlusOne mode=floor-sample:256 n=11 floorBound=3 42:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 1:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 2:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 3:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 4:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 5:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 6:[1 10]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 7:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSidesN1000 mode=floor-sample:256 n=1000 floorBound=9 42:[999 1]I rounds=1 last=999[454 546]K=selected 1:[999 1]I rounds=1 last=999[454 546]K=selected 2:[999 1]I rounds=1 last=999[454 546]K=selected 3:[999 1]I rounds=1 last=999[546 454]K=selected 4:[999 1]I rounds=1 last=999[454 546]K=selected 5:[999 1]I rounds=1 last=999[546 454]K=selected 6:[999 1]I rounds=1 last=999[546 454]K=selected 7:[999 1]I rounds=1 last=999[454 546]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliersN1000 mode=floor-sample:256 n=1000 floorBound=9 42:[999 1]I rounds=1 last=999[453 547]K=selected 1:[999 1]I rounds=1 last=999[453 547]K=selected 2:[999 1]I rounds=1 last=999[453 547]K=selected 3:[999 1]I rounds=1 last=999[547 453]K=selected 4:[999 1]I rounds=1 last=999[453 547]K=selected 5:[999 1]I rounds=1 last=999[547 453]K=selected 6:[999 1]I rounds=1 last=999[547 453]K=selected 7:[999 1]I rounds=1 last=999[453 547]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliersN1000 mode=floor-sample:256 n=1000 floorBound=9 42:[999 1]I rounds=1 last=999[454 546]K=selected 1:[999 1]I rounds=1 last=999[454 546]K=selected 2:[999 1]I rounds=1 last=999[454 546]K=selected 3:[999 1]I rounds=1 last=999[546 454]K=selected 4:[999 1]I rounds=1 last=999[454 546]K=selected 5:[999 1]I rounds=1 last=999[546 454]K=selected 6:[999 1]I rounds=1 last=999[546 454]K=selected 7:[999 1]I rounds=1 last=999[454 546]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045N1000 mode=floor-sample:256 n=1000 floorBound=9 42:[600 400]I rounds=4 last=78[470 530]K=selected 1:[400 600]I rounds=3 last=156[544 456]K=selected 2:[600 400]I rounds=3 last=156[454 546]K=selected 3:[600 400]I rounds=3 last=144[456 544]K=selected 4:[600 400]I rounds=4 last=74[456 544]K=selected 5:[400 600]I rounds=4 last=72[544 456]K=selected 6:[600 400]I rounds=3 last=144[544 456]K=selected 7:[400 600]I rounds=3 last=156[456 544]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers13R3N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[538 462]K rounds=0 last=-=initialUsable 1:[357 643]K rounds=0 last=-=initialUsable 2:[554 446]K rounds=0 last=-=initialUsable 3:[692 308]K rounds=0 last=-=initialUsable 4:[510 490]K rounds=0 last=-=initialUsable 5:[503 497]K rounds=0 last=-=initialUsable 6:[501 499]K rounds=0 last=-=initialUsable 7:[529 471]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R3N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[444 556]K rounds=0 last=-=initialUsable 1:[468 532]K rounds=0 last=-=initialUsable 2:[565 435]K rounds=0 last=-=initialUsable 3:[421 579]K rounds=0 last=-=initialUsable 4:[418 582]K rounds=0 last=-=initialUsable 5:[440 560]K rounds=0 last=-=initialUsable 6:[535 465]K rounds=0 last=-=initialUsable 7:[533 467]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[583 417]K rounds=0 last=-=initialUsable 1:[546 454]K rounds=0 last=-=initialUsable 2:[597 403]K rounds=0 last=-=initialUsable 3:[18 982]I rounds=2 last=981[521 479]K=selected 4:[393 607]K rounds=0 last=-=initialUsable 5:[448 552]K rounds=0 last=-=initialUsable 6:[521 479]K rounds=0 last=-=initialUsable 7:[528 472]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R1p5N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[414 586]K rounds=0 last=-=initialUsable 1:[510 490]K rounds=0 last=-=initialUsable 2:[559 441]K rounds=0 last=-=initialUsable 3:[425 575]K rounds=0 last=-=initialUsable 4:[492 508]K rounds=0 last=-=initialUsable 5:[520 480]K rounds=0 last=-=initialUsable 6:[510 490]K rounds=0 last=-=initialUsable 7:[494 506]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R1p5N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[415 585]K rounds=0 last=-=initialUsable 1:[559 441]K rounds=0 last=-=initialUsable 2:[535 465]K rounds=0 last=-=initialUsable 3:[633 367]K rounds=0 last=-=initialUsable 4:[495 505]K rounds=0 last=-=initialUsable 5:[479 521]K rounds=0 last=-=initialUsable 6:[573 427]K rounds=0 last=-=initialUsable 7:[623 377]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R10N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[11 989]I rounds=5 last=969[298 702]K=selected 1:[999 1]I rounds=5 last=969[578 422]K=selected 2:[989 11]I rounds=5 last=969[451 549]K=selected 3:[999 1]I rounds=5 last=969[545 455]K=selected 4:[14 986]I rounds=5 last=969[717 283]K=selected 5:[987 13]I rounds=5 last=969[687 313]K=selected 6:[990 10]I rounds=5 last=969[508 492]K=selected 7:[8 992]I rounds=5 last=969[353 647]K=selected",
			"GUARDIANN-PEEL-HD hdTight13N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[999 1]I rounds=4 last=985[382 618]K=selected 1:[999 1]I rounds=4 last=985[428 572]K=selected 2:[999 1]I rounds=4 last=985[655 345]K=selected 3:[999 1]I rounds=4 last=985[192 808]K=selected 4:[999 1]I rounds=4 last=985[598 402]K=selected 5:[999 1]I rounds=4 last=985[590 410]K=selected 6:[999 1]I rounds=4 last=985[638 362]K=selected 7:[999 1]I rounds=4 last=985[387 613]K=selected",
			"GUARDIANN-PEEL-HD hdTight20N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[999 1]I rounds=5 last=969[445 555]K=selected 1:[999 1]I rounds=5 last=969[532 468]K=selected 2:[999 1]I rounds=5 last=969[677 323]K=selected 3:[999 1]I rounds=5 last=969[437 563]K=selected 4:[999 1]I rounds=6 last=898[354 646]K=selected 5:[999 1]I rounds=5 last=969[539 461]K=selected 6:[999 1]I rounds=5 last=969[761 239]K=selected 7:[999 1]I rounds=5 last=969[491 509]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[999 1]I rounds=6 last=937[474 526]K=selected 1:[999 1]I rounds=6 last=937[535 465]K=selected 2:[999 1]I rounds=6 last=937[716 284]K=selected 3:[999 1]I rounds=6 last=937[566 434]K=selected 4:[999 1]I rounds=6 last=937[485 515]K=selected 5:[999 1]I rounds=6 last=937[593 407]K=selected 6:[999 1]I rounds=6 last=937[432 568]K=selected 7:[999 1]I rounds=6 last=937[457 543]K=selected",
			"GUARDIANN-PEEL-HD hdTight20Sigma0p1N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[987 13]I rounds=5 last=969[455 545]K=selected 1:[988 12]I rounds=5 last=969[338 662]K=selected 2:[988 12]I rounds=5 last=969[580 420]K=selected 3:[999 1]I rounds=5 last=969[649 351]K=selected 4:[988 12]I rounds=5 last=969[423 577]K=selected 5:[11 989]I rounds=5 last=969[628 372]K=selected 6:[12 988]I rounds=5 last=969[630 370]K=selected 7:[985 15]I rounds=5 last=969[333 667]K=selected",
			"GUARDIANN-PEEL-HD hdTightRising50N1000D128 mode=floor-sample:256 n=1000 floorBound=9 42:[999 1]I rounds=6 last=937[660 340]K=selected 1:[999 1]I rounds=6 last=937[752 248]K=selected 2:[999 1]I rounds=6 last=937[646 354]K=selected 3:[999 1]I rounds=6 last=937[529 471]K=selected 4:[999 1]I rounds=6 last=937[442 558]K=selected 5:[999 1]I rounds=6 last=937[465 535]K=selected 6:[999 1]I rounds=6 last=937[493 507]K=selected 7:[999 1]I rounds=6 last=937[495 505]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D128 mode=floor-sample:256 n=2000 floorBound=10 42:[1175 825]K rounds=0 last=-=initialUsable 1:[1221 779]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D768 mode=floor-sample:256 n=2000 floorBound=10 42:[1028 972]K rounds=0 last=-=initialUsable 1:[1859 141]I rounds=2 last=1842[318 1682]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N2000D768 mode=floor-sample:256 n=2000 floorBound=10 42:[1999 1]I rounds=6 last=1937[445 1555]K=selected 1:[1999 1]I rounds=7 last=1831[579 1421]K=selected",
			"GUARDIANN-PEEL-HD tenPlusOne mode=floor-sample:64 n=11 floorBound=3 42:[10 1]I rounds=1 last=10[5 6]K=selected 1:[10 1]I rounds=1 last=10[5 6]K=selected 2:[10 1]I rounds=1 last=10[6 5]K=selected 3:[10 1]I rounds=1 last=10[5 6]K=selected 4:[10 1]I rounds=1 last=10[6 5]K=selected 5:[10 1]I rounds=1 last=10[5 6]K=selected 6:[1 10]I rounds=1 last=10[5 6]K=selected 7:[10 1]I rounds=1 last=10[5 6]K=selected",
			"GUARDIANN-PEEL-HD twentyPlusOne mode=floor-sample:64 n=21 floorBound=4 42:[20 1]I rounds=1 last=20[11 10]K=selected 1:[20 1]I rounds=1 last=20[10 11]K=selected 2:[20 1]I rounds=1 last=20[11 10]K=selected 3:[20 1]I rounds=1 last=20[11 10]K=selected 4:[20 1]I rounds=1 last=20[11 10]K=selected 5:[20 1]I rounds=1 last=20[11 10]K=selected 6:[20 1]I rounds=1 last=20[12 9]K=selected 7:[20 1]I rounds=1 last=20[10 11]K=selected",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSides mode=floor-sample:64 n=20 floorBound=4 42:[19 1]I rounds=2 last=17[10 10]K=selected 1:[19 1]I rounds=2 last=17[9 11]K=selected 2:[19 1]I rounds=2 last=17[9 11]K=selected 3:[19 1]I rounds=2 last=17[9 11]K=selected 4:[1 19]I rounds=2 last=17[9 11]K=selected 5:[19 1]I rounds=2 last=17[11 9]K=selected 6:[19 1]I rounds=2 last=17[11 9]K=selected 7:[19 1]I rounds=2 last=17[9 11]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliers mode=floor-sample:64 n=33 floorBound=5 42:[32 1]I rounds=2 last=30[17 16]K=selected 1:[32 1]I rounds=2 last=30[17 16]K=selected 2:[32 1]I rounds=2 last=30[17 16]K=selected 3:[32 1]I rounds=2 last=30[16 17]K=selected 4:[32 1]I rounds=2 last=30[18 15]K=selected 5:[32 1]I rounds=2 last=30[18 15]K=selected 6:[32 1]I rounds=2 last=30[18 15]K=selected 7:[32 1]I rounds=2 last=30[17 16]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliers mode=floor-sample:64 n=12 floorBound=3 42:[11 1]I rounds=2 last=9[5 7]K=selected 1:[11 1]I rounds=2 last=9[5 7]K=selected 2:[11 1]I rounds=2 last=9[5 7]K=selected 3:[11 1]I rounds=2 last=9[5 7]K=selected 4:[11 1]I rounds=2 last=9[5 7]K=selected 5:[11 1]I rounds=2 last=9[6 6]K=selected 6:[1 11]I rounds=2 last=9[7 5]K=selected 7:[11 1]I rounds=2 last=9[7 5]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045 mode=floor-sample:64 n=100 floorBound=6 42:[60 40]I rounds=3 last=15[48 52]K=selected 1:[60 40]I rounds=3 last=15[48 52]K=selected 2:[60 40]I rounds=3 last=15[48 52]K=selected 3:[60 40]I rounds=5 last=5[46 54]K=selected 4:[60 40]I rounds=4 last=9[47 53]K=selected 5:[60 40]I rounds=3 last=15[48 52]K=selected 6:[40 60]I rounds=5 last=4[54 46]K=selected 7:[40 60]I rounds=4 last=9[53 47]K=selected",
			"GUARDIANN-PEEL-HD heavyTail mode=floor-sample:64 n=16 floorBound=3 42:[2 14]K rounds=0 last=-=initialUsable 1:[15 1]I rounds=1 last=15[14 2]K=selected 2:[15 1]I rounds=1 last=15[13 3]K=selected 3:[14 2]K rounds=0 last=-=initialUsable 4:[2 14]K rounds=0 last=-=initialUsable 5:[2 14]K rounds=0 last=-=initialUsable 6:[14 2]K rounds=0 last=-=initialUsable 7:[14 2]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD identicalMassPlusOne mode=floor-sample:64 n=11 floorBound=3 42:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 1:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 2:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 3:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 4:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 5:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 6:[1 10]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass 7:[10 1]I rounds=1 last=10[11 0]I=undersizedChildHoldsNoMass",
			"GUARDIANN-PEEL-HD twoOutliersOppositeSidesN1000 mode=floor-sample:64 n=1000 floorBound=9 42:[999 1]I rounds=1 last=999[730 270]K=selected 1:[999 1]I rounds=1 last=999[730 270]K=selected 2:[999 1]I rounds=1 last=999[270 730]K=selected 3:[999 1]I rounds=1 last=999[730 270]K=selected 4:[999 1]I rounds=1 last=999[529 471]K=selected 5:[999 1]I rounds=1 last=999[270 730]K=selected 6:[999 1]I rounds=1 last=999[270 730]K=selected 7:[999 1]I rounds=1 last=999[471 529]K=selected",
			"GUARDIANN-PEEL-HD nestedOutliersN1000 mode=floor-sample:64 n=1000 floorBound=9 42:[999 1]I rounds=1 last=999[731 269]K=selected 1:[999 1]I rounds=1 last=999[731 269]K=selected 2:[999 1]I rounds=1 last=999[269 731]K=selected 3:[999 1]I rounds=1 last=999[731 269]K=selected 4:[999 1]I rounds=1 last=999[530 470]K=selected 5:[999 1]I rounds=1 last=999[269 731]K=selected 6:[999 1]I rounds=1 last=999[269 731]K=selected 7:[999 1]I rounds=1 last=999[470 530]K=selected",
			"GUARDIANN-PEEL-HD scatteredOutliersN1000 mode=floor-sample:64 n=1000 floorBound=9 42:[999 1]I rounds=1 last=999[730 270]K=selected 1:[999 1]I rounds=1 last=999[730 270]K=selected 2:[999 1]I rounds=1 last=999[270 730]K=selected 3:[999 1]I rounds=1 last=999[730 270]K=selected 4:[999 1]I rounds=1 last=999[529 471]K=selected 5:[999 1]I rounds=1 last=999[270 730]K=selected 6:[999 1]I rounds=1 last=999[270 730]K=selected 7:[999 1]I rounds=1 last=999[471 529]K=selected",
			"GUARDIANN-PEEL-HD sixtyFortyAtFraction045N1000 mode=floor-sample:64 n=1000 floorBound=9 42:[600 400]I rounds=3 last=167[544 456]K=selected 1:[400 600]I rounds=5 last=48[496 504]K=selected 2:[600 400]I rounds=5 last=48[496 504]K=selected 3:[600 400]I rounds=4 last=87[468 532]K=selected 4:[600 400]I rounds=4 last=80[536 464]K=selected 5:[400 600]I rounds=4 last=80[523 477]K=selected 6:[600 400]I rounds=4 last=80[536 464]K=selected 7:[400 600]I rounds=5 last=48[504 496]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers13R3N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[538 462]K rounds=0 last=-=initialUsable 1:[357 643]K rounds=0 last=-=initialUsable 2:[554 446]K rounds=0 last=-=initialUsable 3:[692 308]K rounds=0 last=-=initialUsable 4:[510 490]K rounds=0 last=-=initialUsable 5:[503 497]K rounds=0 last=-=initialUsable 6:[501 499]K rounds=0 last=-=initialUsable 7:[529 471]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R3N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[444 556]K rounds=0 last=-=initialUsable 1:[468 532]K rounds=0 last=-=initialUsable 2:[565 435]K rounds=0 last=-=initialUsable 3:[421 579]K rounds=0 last=-=initialUsable 4:[418 582]K rounds=0 last=-=initialUsable 5:[440 560]K rounds=0 last=-=initialUsable 6:[535 465]K rounds=0 last=-=initialUsable 7:[533 467]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[583 417]K rounds=0 last=-=initialUsable 1:[546 454]K rounds=0 last=-=initialUsable 2:[597 403]K rounds=0 last=-=initialUsable 3:[18 982]I rounds=5 last=969[348 652]K=selected 4:[393 607]K rounds=0 last=-=initialUsable 5:[448 552]K rounds=0 last=-=initialUsable 6:[521 479]K rounds=0 last=-=initialUsable 7:[528 472]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R1p5N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[414 586]K rounds=0 last=-=initialUsable 1:[510 490]K rounds=0 last=-=initialUsable 2:[559 441]K rounds=0 last=-=initialUsable 3:[425 575]K rounds=0 last=-=initialUsable 4:[492 508]K rounds=0 last=-=initialUsable 5:[520 480]K rounds=0 last=-=initialUsable 6:[510 490]K rounds=0 last=-=initialUsable 7:[494 506]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R1p5N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[415 585]K rounds=0 last=-=initialUsable 1:[559 441]K rounds=0 last=-=initialUsable 2:[535 465]K rounds=0 last=-=initialUsable 3:[633 367]K rounds=0 last=-=initialUsable 4:[495 505]K rounds=0 last=-=initialUsable 5:[479 521]K rounds=0 last=-=initialUsable 6:[573 427]K rounds=0 last=-=initialUsable 7:[623 377]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers20R10N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[11 989]I rounds=1 last=989[755 245]K=selected 1:[999 1]I rounds=5 last=915[319 681]K=selected 2:[989 11]I rounds=1 last=989[715 285]K=selected 3:[999 1]I rounds=4 last=985[335 665]K=selected 4:[14 986]I rounds=2 last=915[319 681]K=selected 5:[987 13]I rounds=1 last=987[302 698]K=selected 6:[990 10]I rounds=1 last=990[335 665]K=selected 7:[8 992]I rounds=1 last=992[179 821]K=selected",
			"GUARDIANN-PEEL-HD hdTight13N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[999 1]I rounds=4 last=985[517 483]K=selected 1:[999 1]I rounds=4 last=985[755 245]K=selected 2:[999 1]I rounds=4 last=985[387 613]K=selected 3:[999 1]I rounds=4 last=985[651 349]K=selected 4:[999 1]I rounds=4 last=985[628 372]K=selected 5:[999 1]I rounds=4 last=985[707 293]K=selected 6:[999 1]I rounds=4 last=985[339 661]K=selected 7:[999 1]I rounds=4 last=985[515 485]K=selected",
			"GUARDIANN-PEEL-HD hdTight20N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[999 1]I rounds=4 last=985[377 623]K=selected 1:[999 1]I rounds=4 last=985[529 471]K=selected 2:[999 1]I rounds=4 last=985[226 774]K=selected 3:[999 1]I rounds=4 last=985[267 733]K=selected 4:[999 1]I rounds=4 last=985[181 819]K=selected 5:[999 1]I rounds=4 last=985[457 543]K=selected 6:[999 1]I rounds=4 last=985[478 522]K=selected 7:[999 1]I rounds=4 last=985[771 229]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[999 1]I rounds=6 last=937[398 602]K=selected 1:[999 1]I rounds=6 last=937[521 479]K=selected 2:[999 1]I rounds=6 last=937[702 298]K=selected 3:[999 1]I rounds=6 last=937[250 750]K=selected 4:[999 1]I rounds=6 last=937[876 124]K=selected 5:[999 1]I rounds=6 last=937[705 295]K=selected 6:[999 1]I rounds=6 last=937[316 684]K=selected 7:[999 1]I rounds=6 last=937[493 507]K=selected",
			"GUARDIANN-PEEL-HD hdTight20Sigma0p1N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[987 13]I rounds=1 last=987[645 355]K=selected 1:[988 12]I rounds=1 last=988[417 583]K=selected 2:[988 12]I rounds=1 last=988[313 687]K=selected 3:[999 1]I rounds=4 last=985[877 123]K=selected 4:[988 12]I rounds=1 last=988[728 272]K=selected 5:[11 989]I rounds=1 last=989[450 550]K=selected 6:[12 988]I rounds=1 last=988[877 123]K=selected 7:[985 15]I rounds=1 last=985[335 665]K=selected",
			"GUARDIANN-PEEL-HD hdTightRising50N1000D128 mode=floor-sample:64 n=1000 floorBound=9 42:[999 1]I rounds=6 last=937[181 819]K=selected 1:[999 1]I rounds=6 last=937[581 419]K=selected 2:[999 1]I rounds=7 last=873[255 745]K=selected 3:[999 1]I rounds=6 last=937[676 324]K=selected 4:[999 1]I rounds=6 last=937[575 425]K=selected 5:[999 1]I rounds=6 last=937[449 551]K=selected 6:[999 1]I rounds=6 last=937[403 597]K=selected 7:[999 1]I rounds=6 last=937[581 419]K=selected",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D128 mode=floor-sample:64 n=2000 floorBound=10 42:[1175 825]K rounds=0 last=-=initialUsable 1:[1221 779]K rounds=0 last=-=initialUsable",
			"GUARDIANN-PEEL-HD hdOutliers50R3N2000D768 mode=floor-sample:64 n=2000 floorBound=10 42:[1028 972]K rounds=0 last=-=initialUsable 1:[1859 141]I rounds=2 last=1857[547 1453]K=selected",
			"GUARDIANN-PEEL-HD hdTight50N2000D768 mode=floor-sample:64 n=2000 floorBound=10 42:[1999 1]I rounds=6 last=1937[201 1799]K=selected 1:[1999 1]I rounds=6 last=1937[593 1407]K=selected",
		}
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
		want := []string{
			"GUARDIANN-PEEL-GEN genTight50N2000D768 mode=floor n=2000 d=768 floorBound=10 42:[1999 1]I refits=6 final=[978 1022] minFrac=0.489=selected 1:[1999 1]I refits=6 final=[838 1162] minFrac=0.419=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D768 mode=floor n=2000 d=768 floorBound=10 42:[830 1170]K refits=0 final=[830 1170] minFrac=0.415=initialUsable 1:[959 1041]K refits=0 final=[959 1041] minFrac=0.479=initialUsable",
			"GUARDIANN-PEEL-GEN genTight50N2000D1536 mode=floor n=2000 d=1536 floorBound=10 42:[1999 1]I refits=6 final=[933 1067] minFrac=0.467=selected 1:[1999 1]I refits=6 final=[1198 802] minFrac=0.401=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D1536 mode=floor n=2000 d=1536 floorBound=10 42:[1330 670]K refits=0 final=[1330 670] minFrac=0.335=initialUsable 1:[903 1097]K refits=0 final=[903 1097] minFrac=0.452=initialUsable",
			"GUARDIANN-PEEL-GEN genTight50N2000D3072 mode=floor n=2000 d=3072 floorBound=10 42:[1999 1]I refits=6 final=[979 1021] minFrac=0.489=selected 1:[1999 1]I refits=6 final=[713 1287] minFrac=0.356=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D3072 mode=floor n=2000 d=3072 floorBound=10 42:[1051 949]K refits=0 final=[1051 949] minFrac=0.474=initialUsable 1:[818 1182]K refits=0 final=[818 1182] minFrac=0.409=initialUsable",
			"GUARDIANN-PEEL-GEN genTight50N2000D4096 mode=floor n=2000 d=4096 floorBound=10 42:[1999 1]I refits=6 final=[1337 663] minFrac=0.332=selected 1:[1999 1]I refits=6 final=[941 1059] minFrac=0.470=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D4096 mode=floor n=2000 d=4096 floorBound=10 42:[802 1198]K refits=0 final=[802 1198] minFrac=0.401=initialUsable 1:[13 1987]I refits=1 final=[1726 274] minFrac=0.137=selected",
			"GUARDIANN-PEEL-GEN genTight50N2000D768 mode=floor-sample:256 n=2000 d=768 floorBound=10 42:[1999 1]I refits=6 final=[701 1299] minFrac=0.350=selected 1:[1999 1]I refits=6 final=[765 1235] minFrac=0.383=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D768 mode=floor-sample:256 n=2000 d=768 floorBound=10 42:[830 1170]K refits=0 final=[830 1170] minFrac=0.415=initialUsable 1:[959 1041]K refits=0 final=[959 1041] minFrac=0.479=initialUsable",
			"GUARDIANN-PEEL-GEN genTight50N2000D1536 mode=floor-sample:256 n=2000 d=1536 floorBound=10 42:[1999 1]I refits=6 final=[873 1127] minFrac=0.436=selected 1:[1999 1]I refits=7 final=[923 1077] minFrac=0.462=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D1536 mode=floor-sample:256 n=2000 d=1536 floorBound=10 42:[1330 670]K refits=0 final=[1330 670] minFrac=0.335=initialUsable 1:[903 1097]K refits=0 final=[903 1097] minFrac=0.452=initialUsable",
			"GUARDIANN-PEEL-GEN genTight50N2000D3072 mode=floor-sample:256 n=2000 d=3072 floorBound=10 42:[1999 1]I refits=6 final=[1225 775] minFrac=0.388=selected 1:[1999 1]I refits=6 final=[1077 923] minFrac=0.462=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D3072 mode=floor-sample:256 n=2000 d=3072 floorBound=10 42:[1051 949]K refits=0 final=[1051 949] minFrac=0.474=initialUsable 1:[818 1182]K refits=0 final=[818 1182] minFrac=0.409=initialUsable",
			"GUARDIANN-PEEL-GEN genTight50N2000D4096 mode=floor-sample:256 n=2000 d=4096 floorBound=10 42:[1999 1]I refits=6 final=[243 1757] minFrac=0.121=selected 1:[1999 1]I refits=6 final=[1683 317] minFrac=0.159=selected",
			"GUARDIANN-PEEL-GEN genSpread50R3N2000D4096 mode=floor-sample:256 n=2000 d=4096 floorBound=10 42:[802 1198]K refits=0 final=[802 1198] minFrac=0.401=initialUsable 1:[13 1987]I refits=10 final=[1318 682] minFrac=0.341=selected",
		}
		Expect(got).To(HaveLen(len(modes) * len(gen)))
		Expect(got).To(Equal(want))
	})

	It("measures the unsampled floor peel in the admitted high-dimension corner", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		// The shapes the admission bound of ws-d-design admits at its largest dimensions
		// (W = floor(log2(n - 1)) * n * d * max(iterations * (restarts + 1), 32) / 32
		// <= B at the default KMeans knobs 8 and 3), and the shapes just beyond it. B is
		// 1.96 * 10^7, a WORK bound chosen for coverage, not derived from any measured
		// rate (every run moved the worst observed rate, so a time-derived B moved with
		// it): the first over-max size n = 1001 is admitted up to d = 2175 and the
		// default hard cap n = 2000 up to d = 980. The timings this spec prints are
		// estimates, extracted from every log by ws-d-oracle/corner-refit-rates.py. The d = 2775 and d = 1250 rows are the v12 bound (2.5 * 10^7), now
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
		want := []string{
			"GUARDIANN-PEEL-CORNER genTight50N1001D2048 n=1001 d=2048 floorBound=9 42:[1000 1]I refits=6 final=[605 396] minFrac=0.396=selected 1:[1000 1]I refits=6 final=[625 376] minFrac=0.376=selected",
			"GUARDIANN-PEEL-CORNER genSpread50R3N1001D2048 n=1001 d=2048 floorBound=9 42:[1000 1]I refits=7 final=[603 398] minFrac=0.398=selected 1:[987 14]I refits=6 final=[585 416] minFrac=0.416=selected",
			"GUARDIANN-PEEL-CORNER genTight50N1001D2775 n=1001 d=2775 floorBound=9 42:[1000 1]I refits=6 final=[485 516] minFrac=0.485=selected 1:[1000 1]I refits=6 final=[548 453] minFrac=0.453=selected",
			"GUARDIANN-PEEL-CORNER genSpread50R3N1001D2775 n=1001 d=2775 floorBound=9 42:[1000 1]I refits=4 final=[459 542] minFrac=0.459=selected 1:[21 980]I refits=2 final=[540 461] minFrac=0.461=selected",
			"GUARDIANN-PEEL-CORNER genTight50N2000D1250 n=2000 d=1250 floorBound=10 42:[1999 1]I refits=6 final=[990 1010] minFrac=0.495=selected 1:[1999 1]I refits=6 final=[1156 844] minFrac=0.422=selected",
			"GUARDIANN-PEEL-CORNER genSpread50R3N2000D1250 n=2000 d=1250 floorBound=10 42:[1098 902]K refits=0 final=[1098 902] minFrac=0.451=initialUsable 1:[1049 951]K refits=0 final=[1049 951] minFrac=0.475=initialUsable",
			"GUARDIANN-PEEL-CORNER genTight50N1001D2175 n=1001 d=2175 floorBound=9 42:[1000 1]I refits=6 final=[286 715] minFrac=0.286=selected 1:[1000 1]I refits=6 final=[385 616] minFrac=0.385=selected",
			"GUARDIANN-PEEL-CORNER genSpread50R3N1001D2175 n=1001 d=2175 floorBound=9 42:[1000 1]I refits=2 final=[713 288] minFrac=0.288=selected 1:[985 16]I refits=4 final=[392 609] minFrac=0.392=selected",
			"GUARDIANN-PEEL-CORNER genTight50N2000D980 n=2000 d=980 floorBound=10 42:[1999 1]I refits=6 final=[1215 785] minFrac=0.393=selected 1:[1999 1]I refits=6 final=[1095 905] minFrac=0.453=selected",
			"GUARDIANN-PEEL-CORNER genSpread50R3N2000D980 n=2000 d=980 floorBound=10 42:[915 1085]K refits=0 final=[915 1085] minFrac=0.458=initialUsable 1:[11 1989]I refits=1 final=[960 1040] minFrac=0.480=selected",
		}
		Expect(got).To(Equal(want))
	})

	It("records the target on the split counterexample shapes in deferred and inline mode", func() {
		type phaseOutcome struct {
			OK        bool   `json:"ok"`
			Exception string `json:"exception"`
			Site      string `json:"site"`
		}
		want := []string{
			// Measured, deterministic. The target fails every non-identical shape forever at
			// the orElseThrow site in BOTH modes (deferred drains and inline inserts alike);
			// the identical mass takes the collapse route instead.
			"GUARDIANN-SHAPE tenPlusOne deferred=[1*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*3,] clusters=[{11 1 11}] inline=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{11 1 11}]",
			"GUARDIANN-SHAPE twentyPlusOne deferred=[1*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*3,] clusters=[{21 1 21}] inline=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{21 1 21}]",
			"GUARDIANN-SHAPE twoOutliersOppositeSides deferred=[1*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*3,] clusters=[{20 1 20}] inline=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{20 1 20}]",
			"GUARDIANN-SHAPE nestedOutliers deferred=[1*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*3,] clusters=[{33 1 33}] inline=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{33 1 33}]",
			"GUARDIANN-SHAPE scatteredOutliers deferred=[1*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*3,] clusters=[{12 1 12}] inline=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{12 1 12}]",
			"GUARDIANN-SHAPE sixtyFortyAtFraction045 deferred=[1*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*3,] clusters=[{100 1 100}] inline=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{100 1 100}]",
			"GUARDIANN-SHAPE heavyTail deferred=[1*1,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*3,] clusters=[{16 1 16}] inline=[java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*5,] clusters=[{16 1 16}]",
			"GUARDIANN-SHAPE identicalMassPlusOne deferred=[1*4,0*1,] clusters=[{2 0 11}] inline=[ok*5,] clusters=[{7 0 11}]",
		}
		var got []string
		for _, sh := range shapes() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			DeferCleanup(cancel)
			env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_shape_"+uuid.New().String())
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
		want := []string{
			// Measured, deterministic. collapse: the obsolete collapse and its bounce are
			// consumed without failing; the bounce re-arms a split, the split collapses again,
			// and only that LIVE collapse fails at the collapseConcurrency consumer.
			// reassign/merge: every obsolete task is consumed without consuming its knob.
			// bits9: a split queued before RaBitQ trained with unsupported extra bits 9
			// fails at Primitives.quantizer while live and is consumed once obsolete.
			"GUARDIANN-OBSOLETE collapse setupRounds=5 before=[{11 4 11}] tasks=[BOUNCE/HIGH/1 COLLAPSE/HIGH/1] staged=[0] drain=[1,1,1,java.lang.IllegalArgumentException@Primitives.fetchCoreClusters:1512,java.lang.IllegalArgumentException@Primitives.fetchCoreClusters:1512,java.lang.IllegalArgumentException@Primitives.fetchCoreClusters:1512,] tasksAfter=[COLLAPSE/HIGH/1 BOUNCE/HIGH/1] after=[{11 4 11}]",
			"GUARDIANN-OBSOLETE reassign setupRounds=43 before=[{2 2 2} {9 2 9} {10 2 10}] tasks=[REASSIGN/HIGH/1 REASSIGN/NORMAL/1 REASSIGN/NORMAL/1] staged=[3 3 3] drain=[1,1,1,0,] tasksAfter=[] after=[{2 3 2} {9 3 9} {10 3 10}]",
			"GUARDIANN-OBSOLETE merge setupRounds=52 before=[{2 0 2} {1 0 9} {1 1 10}] tasks=[SPLIT_MERGE/HIGH/1] staged=[0] drain=[1,0,] tasksAfter=[] after=[{2 0 2} {1 0 9} {1 0 10}]",
			"GUARDIANN-OBSOLETE bits9Live setupRounds=0 before=[{11 1 11}] tasks=[SPLIT_MERGE/NORMAL/1] staged=[] drain=[java.lang.IllegalArgumentException@Primitives.quantizer:359,java.lang.IllegalArgumentException@Primitives.quantizer:359,java.lang.IllegalArgumentException@Primitives.quantizer:359,] tasksAfter=[SPLIT_MERGE/NORMAL/1] after=[{11 1 11}] trained=true",
			"GUARDIANN-OBSOLETE bits9Obsolete setupRounds=0 before=[{11 1 11}] tasks=[SPLIT_MERGE/NORMAL/1] staged=[0] drain=[1,0,] tasksAfter=[] after=[{11 0 11}] trained=true",
		}
		var got []string
		for _, scenario := range []string{"collapse", "reassign", "merge", "bits9Live", "bits9Obsolete"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			DeferCleanup(cancel)
			env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_obsolete_"+uuid.New().String())
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
		want := []string{
			// Measured, deterministic: autoMergeDuringCommit reaches the engine as
			// maintainInTransaction, so the engine-level inline goldens transfer.
			"GUARDIANN-RECORD-INLINE autoMergeHardMaxIsMaxPlusOne saves=[ok*20,] clusters=[{6 2 6} {8 0 8} {6 0 6}]",
			"GUARDIANN-RECORD-INLINE deferredHardMaxIsMaxPlusOne saves=[ok*11,com.apple.foundationdb.record.provider.foundationdb.indexes.VectorIndexClusterTooLargeException@*9,] clusters=[{11 1 11}]",
			"GUARDIANN-RECORD-INLINE autoMergeUnsplittable saves=[ok*12,java.util.NoSuchElementException@SplitMergeTask.lambda$selectSplitCandidate$8:397*4,] clusters=[{12 1 12}]",
		}
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
			env, err := SetupTenantEnvironment(ctx, sharedContainer, "guardiann_record_inline_"+uuid.New().String())
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
