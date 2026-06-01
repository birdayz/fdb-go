// Package conformance hosts oracle-style integration tests for the pure-Go FDB
// client (RFC-010 Phase 1+). C1: after the Go client writes, run FDB's OWN
// consistency check and assert it reports the cluster internally consistent —
// their checker, our writes, zero reimplementation.
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	foundationdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// ccTraceEvent is the subset of an FDB JSON trace event we inspect. The
// consistency-check role emits newline-delimited JSON objects like:
//
//	{  "Severity": "40", "Type": "ConsistencyCheck_DataInconsistent", ... }
type ccTraceEvent struct {
	Severity string `json:"Severity"`
	Type     string `json:"Type"`
	Reason   string `json:"Reason"`
}

// ccResult is the digest of a consistency-check trace.
type ccResult struct {
	finished        bool     // ConsistencyCheck_FinishedCheck seen (ran to completion)
	shardsExamined  int      // ConsistencyCheck_FirstValidServer count (one baseline replica per shard)
	replicaReads    int      // ConsistencyCheck_GetKeyValuesStream count (one read per replica per shard)
	inconsistencies []string // SevError/inconsistency events
}

// crossReplicaCompared reports whether at least one shard had MORE than one
// replica read — i.e. a real cross-replica byte comparison occurred. Because
// replicaReads is the sum of per-shard replica reads and shardsExamined is the
// number of shards, replicaReads > shardsExamined can only hold if some single
// shard was read on ≥2 replicas. (A single-redundancy cluster reads one replica
// per shard, so the two counts are equal no matter how many shards exist.)
func (r ccResult) crossReplicaCompared() bool {
	return r.shardsExamined > 0 && r.replicaReads > r.shardsExamined
}

// TestConsistencyCheck_AfterGoClientWrites is RFC-010 C1: write a dataset
// through the pure-Go client, then run FoundationDB's own consistency-check role
// and assert it finds the cluster consistent.
//
// Why double redundancy across 3 nodes: the checker's data comparison only fires
// for shards with ≥2 replicas — it records the first replica as the baseline and
// byte-compares every subsequent replica against it (ConsistencyScan.actor.cpp
// checkDataConsistency). Under single redundancy there is one copy per shard and
// the comparison is a no-op, so this test would be near-vacuous. With double
// redundancy each shard's replicas are byte-compared, which is the whole point:
// prove our client's committed writes are replicated byte-identically and the
// shard/key-server metadata is what FDB itself expects.
//
// Anti-vacuity: we require more per-replica reads (ConsistencyCheck_
// GetKeyValuesStream, one per replica per shard) than shards examined
// (ConsistencyCheck_FirstValidServer, one baseline replica per shard). Since
// total reads is the sum of per-shard reads, reads > shards can only hold if
// some single shard was read on ≥2 replicas — proving the cross-replica byte
// comparison actually ran for that shard. A plain count (e.g. "≥2 reads") would
// be defeated by N single-replica shards; FirstValidServer / _CheckCustomReplica
// fire even under single redundancy, so they don't prove a comparison either.
//
// Records/indexes ride this exact commit path; from the checker's perspective
// they are the same replicated stored bytes, so raw KV writes exercise the same
// property without the record-layer schema setup.
func TestConsistencyCheck_AfterGoClientWrites(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cluster, err := foundationdb.RunCluster(ctx, 3,
		foundationdb.WithRedundancyMode("double"),
		foundationdb.WithStorageEngine("ssd"),
	)
	if err != nil {
		// Fail (don't skip) on a real cluster error — matches the repo's other
		// RunCluster-based tests. A skip here would hide configure-double /
		// replication-health failures behind a green "no Docker" lie.
		t.Fatalf("RunCluster(3, double): %v", err)
	}
	defer cluster.Terminate(ctx)

	// Connect the pure-Go client.
	path, err := cluster.Coordinator.ClusterFilePath(ctx)
	if err != nil {
		t.Fatalf("ClusterFilePath: %v", err)
	}
	gofdb.MustAPIVersion(730)
	db, err := gofdb.OpenDatabase(path)
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close()

	// Write a non-trivial dataset via the Go client.
	const batches, perBatch = 10, 100
	for b := 0; b < batches; b++ {
		bb := b
		if _, err := db.Transact(func(tr gofdb.Transaction) (any, error) {
			for i := 0; i < perBatch; i++ {
				k := gofdb.Key(tuple.Tuple{"c1", bb, i}.Pack())
				tr.Set(k, []byte(fmt.Sprintf("v-%d-%d-%s", bb, i, strings.Repeat("x", 256))))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("write batch %d: %v", b, err)
		}
	}
	t.Logf("wrote %d keys via the pure-Go client", batches*perBatch)

	// Let replication settle so the checker sees fully-replicated shards.
	waitReplicationHealthy(ctx, t, cluster)

	// Run FDB's one-shot consistency-check role. The check itself completes in
	// ~1-2s, but the role process does not self-terminate (no -p allowed, so it
	// lingers), hence: launch it in the background, poll the trace for the
	// completion event, then kill it. Polling-until-done (not a fixed sleep)
	// keeps the test fast on a quick cluster and robust on a slow one.
	const script = `mkdir -p /tmp/cc /tmp/ccdata && rm -f /tmp/cc/trace.* ;
fdbserver -r consistencycheck -C /var/fdb/fdb.cluster --datadir /tmp/ccdata --logdir /tmp/cc --trace-format json > /tmp/cc.out 2>&1 &
CCPID=$! ;
for i in $(seq 1 90); do grep -q ConsistencyCheck_FinishedCheck /tmp/cc/trace.*.json 2>/dev/null && break ; sleep 1 ; done ;
kill $CCPID 2>/dev/null ; wait $CCPID 2>/dev/null ;
cat /tmp/cc/trace.*.json 2>/dev/null`
	_, reader, err := cluster.Coordinator.Exec(ctx, []string{"sh", "-c", script}, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("exec consistencycheck role: %v", err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read consistency-check trace: %v", err)
	}

	res := parseConsistencyTrace(raw)

	// Inconsistency is the headline failure: report it first and verbatim.
	if len(res.inconsistencies) > 0 {
		t.Fatalf("FDB consistency check found %d inconsistency event(s) after Go-client writes:\n  %s",
			len(res.inconsistencies), strings.Join(res.inconsistencies, "\n  "))
	}
	// Anti-vacuity guards: the check must have run to completion AND actually
	// cross-compared the replicas of some shard. A clean result without a real
	// cross-replica comparison is meaningless (e.g. the cluster silently came up
	// single-redundancy), so require strictly more replica reads than shards.
	if !res.finished {
		t.Fatalf("consistency check did not complete (no ConsistencyCheck_FinishedCheck); trace had %d bytes", len(raw))
	}
	if !res.crossReplicaCompared() {
		t.Fatalf("consistency check did not cross-compare replicas: %d replica reads across %d shard(s) — every shard read only one replica (single-redundancy or vacuous run)",
			res.replicaReads, res.shardsExamined)
	}
	t.Logf("FDB consistency check CLEAN: completed, %d replica reads across %d shard(s) (cross-replica comparison ran), 0 inconsistencies",
		res.replicaReads, res.shardsExamined)
}

// parseConsistencyTrace walks the newline-delimited JSON trace into a ccResult:
// completion (ConsistencyCheck_FinishedCheck), shards examined (one
// ConsistencyCheck_FirstValidServer per shard's baseline replica), per-replica
// reads (ConsistencyCheck_GetKeyValuesStream, one per replica per shard), and
// inconsistency events. The process exit code is NOT a reliable signal (the role
// exits 0 even on inconsistency), so detection is by trace event.
func parseConsistencyTrace(raw []byte) ccResult {
	var res ccResult
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev ccTraceEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "ConsistencyCheck_FinishedCheck":
			res.finished = true
		case "ConsistencyCheck_FirstValidServer":
			res.shardsExamined++
		case "ConsistencyCheck_GetKeyValuesStream":
			res.replicaReads++
		}
		if reason := inconsistencyReason(ev); reason != "" {
			res.inconsistencies = append(res.inconsistencies, reason)
		}
	}
	return res
}

// inconsistencyReason returns a non-empty description if ev signals a real
// consistency violation, else "". It matches FDB release-7.3 checkDataConsistency
// / testFailure signalling across all inconsistency classes:
//
//   - SevError (40) *_Inconsistent (ConsistencyCheck_DataInconsistent, etc.).
//     Gated on Sev40 so a transient mismatch against a failed/TSS server — which
//     FDB deliberately downgrades to SevWarn — is NOT flagged (avoids false
//     positives while data distribution is still moving).
//   - ConsistencyCheck_InconsistentStorageMetrics regardless of severity: FDB
//     emits this at SevInfo with no paired TestFailure, so a Sev40-only filter
//     would silently miss a replica byte-size divergence.
//   - SevError TestFailure whose reason mentions "inconsistent" — the reason
//     wording ("Data inconsistent" / "Key servers inconsistent", with or without
//     a "Consistency check:" prefix) is matched case-insensitively so we don't
//     depend on the exact framework prefix.
func inconsistencyReason(ev ccTraceEvent) string {
	switch {
	case ev.Type == "ConsistencyCheck_InconsistentStorageMetrics":
		return ev.Type + ": replica storage-metrics divergence"
	case strings.Contains(ev.Type, "Inconsistent") && ev.Severity == "40":
		return ev.Type + ": " + ev.Reason
	case ev.Type == "TestFailure" && ev.Severity == "40" && strings.Contains(strings.ToLower(ev.Reason), "inconsistent"):
		return ev.Type + ": " + ev.Reason
	default:
		return ""
	}
}

// TestParseConsistencyTrace pins the inconsistency-detection logic
// deterministically — the integration test runs clean, so it can never exercise
// the failure path. A real consistency violation surfaces as a SevError (40)
// trace event; this proves we flag those and ignore benign severities, so the
// oracle isn't a vacuous always-pass.
func TestParseConsistencyTrace(t *testing.T) {
	t.Parallel()

	// One shard (FirstValidServer x1) read on two replicas (GetKeyValuesStream x2)
	// → a real cross-replica comparison happened.
	clean := strings.Join([]string{
		`{ "Severity": "10", "Type": "ProgramStart" }`,
		`{ "Severity": "20", "Type": "ConsistencyCheck_StorageServerUnavailable" }`,       // benign warning
		`{ "Severity": "30", "Type": "ConsistencyCheck_DataInconsistent", "Reason": "" }`, // SevWarn = transient (failed/TSS server), must be ignored
		`{ "Severity": "10", "Type": "ConsistencyCheck_FirstValidServer" }`,
		`{ "Severity": "10", "Type": "ConsistencyCheck_GetKeyValuesStream", "StorageServer0": "x" }`,
		`{ "Severity": "10", "Type": "ConsistencyCheck_GetKeyValuesStream", "StorageServer1": "y" }`,
		`{ "Severity": "10", "Type": "ConsistencyCheck_FinishedCheck" }`,
	}, "\n")
	res := parseConsistencyTrace([]byte(clean))
	if !res.finished || !res.crossReplicaCompared() {
		t.Errorf("clean trace: finished=%v crossReplica=%v (reads=%d shards=%d), want true/true",
			res.finished, res.crossReplicaCompared(), res.replicaReads, res.shardsExamined)
	}
	if len(res.inconsistencies) != 0 {
		t.Errorf("clean trace flagged %d inconsistencies, want 0 (SevWarn DataInconsistent is transient): %v", len(res.inconsistencies), res.inconsistencies)
	}

	// Codex sharp edge: two single-replica shards (FirstValidServer x2,
	// GetKeyValuesStream x2) sum to 2 reads but NO shard had a second replica —
	// must NOT count as a cross-replica comparison.
	singleRedMultiShard := strings.Join([]string{
		`{ "Severity": "10", "Type": "ConsistencyCheck_FirstValidServer" }`,
		`{ "Severity": "10", "Type": "ConsistencyCheck_GetKeyValuesStream" }`,
		`{ "Severity": "10", "Type": "ConsistencyCheck_FirstValidServer" }`,
		`{ "Severity": "10", "Type": "ConsistencyCheck_GetKeyValuesStream" }`,
		`{ "Severity": "10", "Type": "ConsistencyCheck_FinishedCheck" }`,
	}, "\n")
	res = parseConsistencyTrace([]byte(singleRedMultiShard))
	if res.crossReplicaCompared() {
		t.Errorf("single-redundancy multi-shard (reads=%d shards=%d) reported crossReplica=true — must be false",
			res.replicaReads, res.shardsExamined)
	}

	dirty := strings.Join([]string{
		`{ "Severity": "40", "Type": "ConsistencyCheck_DataInconsistent", "Reason": "" }`,
		`{ "Severity": "40", "Type": "TestFailure", "Reason": "Data inconsistent" }`,        // no "Consistency check:" prefix
		`{ "Severity": "40", "Type": "TestFailure", "Reason": "Key servers inconsistent" }`, // keyserver class
		`{ "Severity": "10", "Type": "ConsistencyCheck_InconsistentStorageMetrics" }`,       // SevInfo, no TestFailure — must still be flagged
		`{ "Severity": "40", "Type": "SomeUnrelatedError", "Reason": "disk full" }`,         // NOT a consistency failure
		`{ "Severity": "10", "Type": "ConsistencyCheck_FinishedCheck" }`,
	}, "\n")
	res = parseConsistencyTrace([]byte(dirty))
	if !res.finished {
		t.Error("dirty trace: finished=false, want true")
	}
	if len(res.inconsistencies) != 4 {
		t.Errorf("dirty trace flagged %d inconsistencies, want 4 (Sev40 DataInconsistent + 2 TestFailures + SevInfo InconsistentStorageMetrics, NOT the unrelated SevError): %v", len(res.inconsistencies), res.inconsistencies)
	}

	// Incomplete run (checker crashed before finishing) must NOT read as a clean pass.
	res = parseConsistencyTrace([]byte(`{ "Severity": "10", "Type": "ConsistencyCheck_GetKeyValuesStream" }`))
	if res.finished {
		t.Error("incomplete trace (no FinishedCheck) reported finished=true")
	}
}

// waitReplicationHealthy polls cluster status until replication reports healthy,
// so the consistency check sees fully-replicated shards rather than data still
// in flight. Best-effort: the check tolerates non-quiescence, so on timeout we
// log and proceed rather than fail.
func waitReplicationHealthy(ctx context.Context, t *testing.T, cluster *foundationdb.Cluster) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out, err := cluster.Coordinator.FDBCLIExec(ctx, "status")
		if err == nil && strings.Contains(out, "Replication health") && strings.Contains(out, "Healthy") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Log("warning: replication did not report Healthy within 90s; running consistency check anyway")
}
