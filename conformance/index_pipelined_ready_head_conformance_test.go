//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type indexReadyHeadObservation struct {
	Path                       string         `json:"path"`
	Arm                        string         `json:"arm"`
	Repeat                     int            `json:"repeat"`
	Rows                       int            `json:"rows"`
	RowDetails                 []scanLimitRow `json:"rowDetails"`
	BaselineRow                scanLimitRow   `json:"baselineRow"`
	ExceptionClass             string         `json:"exceptionClass"`
	SourceCallsAtFirstResult   int            `json:"sourceCallsAtFirstResult"`
	SourceCallsAtFailure       int            `json:"sourceCallsAtFailure"`
	FirstCallScannedRecords    int            `json:"firstCallScannedRecords"`
	MappedReadyWhenOffered     bool           `json:"mappedReadyWhenOffered"`
	NativeReadyWhenOffered     bool           `json:"nativeReadyWhenOffered"`
	GateClosedWhenOffered      bool           `json:"gateClosedWhenOffered"`
	IssuedLoads                int            `json:"issuedLoads"`
	IssuedMappings             int            `json:"issuedMappings"`
	RawRangesIssued            int            `json:"rawRangesIssued"`
	CompletedLoadsAtFailure    int            `json:"completedLoadsAtFailure"`
	QuiescedLoads              int            `json:"quiescedLoads"`
	IssuedLoadsAfterQuiescence int            `json:"issuedLoadsAfterQuiescence"`
	RawRangesAfterQuiescence   int            `json:"rawRangesAfterQuiescence"`
	RecordsScanned             int            `json:"recordsScanned"`
	ExpectedIndexBytes         int64          `json:"expectedIndexBytes"`
	ExpectedRecordBytes        int64          `json:"expectedRecordBytes"`
	IndexBytes                 int64          `json:"indexBytes"`
	RecordBytes                int64          `json:"recordBytes"`
	FailureBytes               int64          `json:"failureBytes"`
	QuiescedBytes              int64          `json:"quiescedBytes"`
	RecordPrimaryKeys          []string       `json:"recordPrimaryKeys"`
	RowPrimaryKeys             []string       `json:"rowPrimaryKeys"`
	RowContinuations           []string       `json:"rowContinuations"`
}

func indexReadyHeadAssertBaseline(row scanLimitRow) {
	GinkgoHelper()
	Expect(row.PrimaryKey).To(Equal("FQU="), "reverse VERSION fixture starts with primary key 5")
	// Independent bytes for Order(order_id=5, price=95, tags=[odd]), not a
	// second invocation of the cursor or a payload copied from its result.
	data := []byte{0x08, 0x05, 0x18, 0x5f, 0x22, 0x03, 'o', 'd', 'd'}
	Expect(row.Data).To(Equal(base64.StdEncoding.EncodeToString(data)))
	Expect(row.IndexValue).To(BeEmpty(), "VERSION entries have an empty tuple value")
	version, err := base64.StdEncoding.DecodeString(row.Version)
	Expect(err).NotTo(HaveOccurred())
	Expect(version).To(HaveLen(12), "fixture has a committed record version")
	Expect(bytes.Equal(version[:10], bytes.Repeat([]byte{0xff}, 10))).To(BeFalse())
	Expect(version[10:]).To(Equal([]byte{0, 5}), "six saves give record 5 local version 5")
	stamp := tuple.Versionstamp{UserVersion: 5}
	copy(stamp.TransactionVersion[:], version[:10])
	key := tuple.Tuple{stamp, int64(5)}.Pack()
	Expect(row.IndexKey).To(Equal(base64.StdEncoding.EncodeToString(key)), "index version and primary key must agree with the loaded record")
	continuation, err := base64.StdEncoding.DecodeString(row.Continuation)
	Expect(err).NotTo(HaveOccurred())
	Expect(continuation).NotTo(BeEmpty())
	var token gen.KeyValueCursorContinuation
	Expect(proto.Unmarshal(continuation, &token)).To(Succeed())
	Expect(token.GetMagicNumber()).To(Equal(int64(6_773_487_359_078_157_740)))
	Expect(token.GetInnerContinuation()).To(Equal(key), "mapped row preserves the real index continuation")
}

// This is deliberately Java-only oracle establishment. Both paths use real FDB
// reads, and the gate controls delivery rather than relying on I/O completion
// timing. Go runtime comparison belongs to the follow-up proof phase.
var _ = Describe("Java pipelined ready-head structural oracle", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		env    *JavaTenant
		java   *JavaInvoker
	)
	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
		var err error
		env, err = SetupJavaTenant(ctx, sharedContainer, "ready_head_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
	})
	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
		if cancel != nil {
			cancel()
		}
	})

	It("proves both ready and held real-load schedules for direct and ordinary reverse VERSION scans", func() {
		const repeats = 10
		const total = 4 * repeats
		// The endpoint seeds six records itself. Keep this exact subspace:
		// its packed length is part of the independently pinned 91-byte fixture.
		ks := env.Keyspace.Sub("limits")
		Expect(ks.Bytes()).To(Equal([]byte{0x05, 0x00, 0x02, 'l', 'i', 'm', 'i', 't', 's', 0x00}), "91-byte calibration requires the exact tenant-root limits prefix")
		raw, err := java.Invoke(ctx, "probeIndexPipelinedReadyHead", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(ks.Bytes()), "repeats": repeats,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(raw).NotTo(BeEmpty(), "the oracle must actually return its complete population")
		GinkgoWriter.Printf("READY_HEAD_ORACLE_RAW_JSON\n%s\n", raw)
		AddReportEntry("index-pipelined-ready-head/complete-java-response", string(raw), ReportEntryVisibilityAlways)
		var response struct {
			Observations []json.RawMessage `json:"observations"`
		}
		Expect(json.Unmarshal(raw, &response)).To(Succeed())
		Expect(response.Observations).To(HaveLen(total), "four explicit schedule cells, each repeated ten times")
		population := map[string]int{
			"DIRECT_PIPELINE/READY_WHEN_OFFERED": 0,
			"DIRECT_PIPELINE/HELD_WHEN_OFFERED":  0,
			"ORDINARY_SCAN/READY_WHEN_OFFERED":   0,
			"ORDINARY_SCAN/HELD_WHEN_OFFERED":    0,
		}
		seen := make(map[string]bool, total)
		readyRows, heldRows, nativeReady, firstErrors := 0, 0, 0, 0
		var baseline *scanLimitRow
		for _, encoded := range response.Observations {
			var observation indexReadyHeadObservation
			Expect(json.Unmarshal(encoded, &observation)).To(Succeed())
			cell := observation.Path + "/" + observation.Arm
			_, known := population[cell]
			Expect(known).To(BeTrue(), "unknown Java schedule cell %q", cell)
			id := fmt.Sprintf("%s/repeat=%d", cell, observation.Repeat)
			GinkgoWriter.Printf("READY_HEAD_ORACLE_OBSERVATION %s %s\n", id, encoded)
			AddReportEntry("index-pipelined-ready-head/"+id, string(encoded), ReportEntryVisibilityAlways)
			Expect(seen[id]).To(BeFalse(), "duplicate oracle observation %s", id)
			seen[id] = true
			population[cell]++
			Expect(observation.Repeat).To(BeNumerically(">=", 0))
			Expect(observation.Repeat).To(BeNumerically("<", repeats))
			indexReadyHeadAssertBaseline(observation.BaselineRow)
			if baseline == nil {
				first := observation.BaselineRow
				baseline = &first
			}
			Expect(observation.BaselineRow).To(Equal(*baseline), id+" fixed committed fixture across paths and schedules")
			Expect(observation.NativeReadyWhenOffered).To(BeTrue(), id+" actual native record range must have settled before the mapping is offered")
			nativeReady++
			Expect(observation.IssuedLoads).To(Equal(1), id)
			Expect(observation.IssuedMappings).To(Equal(1), id)
			Expect(observation.RawRangesIssued).To(Equal(1), id+" native record read population")
			Expect(observation.IssuedLoadsAfterQuiescence).To(Equal(1), id+" releasing the gate must not reload")
			Expect(observation.RawRangesAfterQuiescence).To(Equal(1), id)
			Expect(observation.QuiescedLoads).To(Equal(1), id)
			Expect(observation.SourceCallsAtFailure).To(Equal(2), id+" real failed scan-limit attempt follows the first index row")
			Expect(observation.RecordsScanned).To(Equal(2), id+" failed attempts are charged")
			Expect(observation.ExceptionClass).To(Equal("ScanLimitReachedException"), id+" actual index source exception")
			Expect(observation.ExpectedIndexBytes).To(BeNumerically(">", 0), id)
			Expect(observation.ExpectedRecordBytes).To(BeNumerically(">", 0), id)
			Expect(observation.ExpectedIndexBytes+observation.ExpectedRecordBytes).To(Equal(int64(91)), id+" independent raw fixture size")
			Expect(observation.IndexBytes).To(Equal(observation.ExpectedIndexBytes), id)
			Expect(observation.RecordBytes).To(Equal(observation.ExpectedRecordBytes), id)
			Expect(observation.QuiescedBytes).To(Equal(int64(91)), id+" exactly one index row and one completed record load")
			if observation.Arm == "READY_WHEN_OFFERED" {
				Expect(observation.MappedReadyWhenOffered).To(BeTrue(), id)
				Expect(observation.GateClosedWhenOffered).To(BeFalse(), id)
				Expect(observation.Rows).To(Equal(1), id+" completed head must be returned before source advance")
				Expect(observation.RowDetails).To(Equal([]scanLimitRow{*baseline}), id+" full record/index/version/continuation bytes")
				Expect(observation.RowPrimaryKeys).To(Equal([]string{"FQU="}), id)
				Expect(observation.RowContinuations).To(Equal([]string{baseline.Continuation}), id)
				Expect(observation.SourceCallsAtFirstResult).To(Equal(1), id+" no eager second pull before a ready row")
				Expect(observation.FirstCallScannedRecords).To(Equal(1), id)
				Expect(observation.CompletedLoadsAtFailure).To(Equal(1), id)
				Expect(observation.FailureBytes).To(Equal(int64(91)), id)
				readyRows++
			} else {
				Expect(observation.MappedReadyWhenOffered).To(BeFalse(), id)
				Expect(observation.GateClosedWhenOffered).To(BeTrue(), id+" native completion alone must not open the mapping gate")
				Expect(observation.Rows).To(BeZero(), id+" held mapping must not leak a row before the source error")
				Expect(observation.RowDetails).To(BeEmpty(), id)
				Expect(observation.RowPrimaryKeys).To(BeEmpty(), id)
				Expect(observation.RowContinuations).To(BeEmpty(), id)
				Expect(observation.SourceCallsAtFirstResult).To(Equal(2), id)
				Expect(observation.FirstCallScannedRecords).To(Equal(2), id)
				Expect(observation.CompletedLoadsAtFailure).To(BeZero(), id)
				Expect(observation.FailureBytes).To(Equal(observation.ExpectedIndexBytes), id+" record bytes are charged only after the held gate is released")
				firstErrors++
				heldRows += observation.Rows
			}
			Expect(observation.RecordPrimaryKeys).To(Equal([]string{"FQU="}), id+" quiesced actual load must correspond to the selected record")
		}
		for cell, count := range population {
			Expect(count).To(Equal(repeats), "nonempty, complete oracle cell %s", cell)
		}
		Expect(seen).To(HaveLen(total))
		Expect(readyRows).To(Equal(2 * repeats))
		Expect(heldRows).To(BeZero())
		Expect(firstErrors).To(Equal(2 * repeats))
		Expect(nativeReady).To(Equal(total))
		summary, err := json.Marshal(map[string]any{
			"observations": len(response.Observations), "cells": population,
			"readyRows": readyRows, "heldRows": heldRows, "heldFirstErrors": firstErrors,
			"nativeReadyWhenOffered": nativeReady,
		})
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Printf("READY_HEAD_ORACLE_VERIFIED_POPULATION %s\n", summary)
		AddReportEntry("index-pipelined-ready-head/verified-population", string(summary), ReportEntryVisibilityAlways)
	})
})
