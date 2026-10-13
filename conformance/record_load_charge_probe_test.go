//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type recordLoadChargeObservation struct {
	Arm                        string   `json:"arm"`
	Repeat                     int      `json:"repeat"`
	IssuedLoads                int      `json:"issuedLoads"`
	IndexBytes                 int64    `json:"indexBytes"`
	RecordBytes                int64    `json:"recordBytes"`
	FailureBytes               int64    `json:"failureBytes"`
	QuiescedBytes              int64    `json:"quiescedBytes"`
	RecordsScanned             int      `json:"recordsScanned"`
	Rows                       int      `json:"rows"`
	ExceptionClass             string   `json:"exceptionClass"`
	IssuedLoadsAtFailure       int      `json:"issuedLoadsAtFailure"`
	CompletedLoadsAtFailure    int      `json:"completedLoadsAtFailure"`
	RawRangesIssued            int      `json:"rawRangesIssued"`
	QuiescedLoads              int      `json:"quiescedLoads"`
	GateClosedAtFailure        bool     `json:"gateClosedAtFailure"`
	NativeReadyAtFailure       bool     `json:"nativeReadyAtFailure"`
	IssuedLoadsAfterQuiescence int      `json:"issuedLoadsAfterQuiescence"`
	RawRangesAfterQuiescence   int      `json:"rawRangesAfterQuiescence"`
	RecordPrimaryKeys          []string `json:"recordPrimaryKeys"`
}

// A failed cursor need not await its issued record loads. Snapshot their Java
// byte charges only after those exact futures complete, without issuing reads.
var _ = Describe("Quiesced Java record-load charges", func() {
	It("charges the same bytes with real loads held through or completed before the scan error", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "record_charges_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(env.Cleanup(context.Background())).To(Succeed()) }()

		const repeats = 10
		fixture := env.Keyspace.Sub("limits").Bytes()
		Expect(fixture).To(Equal([]byte{0x05, 0x00, 0x02, 'l', 'i', 'm', 'i', 't', 's', 0x00}))
		var result struct {
			Observations []recordLoadChargeObservation `json:"observations"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "probeRecordLoadCharges", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(fixture), "repeats": repeats,
		}, &result)).To(Succeed())
		Expect(result.Observations).To(HaveLen(2 * repeats))
		arms := make(map[string]int)
		seen := make(map[string]bool)
		for _, observation := range result.Observations {
			fmt.Printf("quiesced Java record-load observation: %+v\n", observation)
			arms[observation.Arm]++
			Expect(observation.Repeat).To(BeNumerically(">=", 0))
			Expect(observation.Repeat).To(BeNumerically("<", repeats))
			identity := fmt.Sprintf("%s/%d", observation.Arm, observation.Repeat)
			Expect(seen[identity]).To(BeFalse(), "repeated observation %s", identity)
			seen[identity] = true
			Expect(observation.IssuedLoads).To(Equal(1))
			Expect(observation.IssuedLoadsAtFailure).To(Equal(1))
			Expect(observation.RawRangesIssued).To(Equal(1))
			Expect(observation.NativeReadyAtFailure).To(BeTrue())
			Expect(observation.QuiescedLoads).To(Equal(1))
			Expect(observation.IssuedLoadsAfterQuiescence).To(Equal(1))
			Expect(observation.RawRangesAfterQuiescence).To(Equal(1))
			Expect(observation.RecordPrimaryKeys).To(Equal([]string{"FA=="}))
			switch observation.Arm {
			case "HELD_UNTIL_ERROR":
				Expect(observation.GateClosedAtFailure).To(BeTrue())
				Expect(observation.CompletedLoadsAtFailure).To(Equal(0))
				Expect(observation.FailureBytes).To(Equal(int64(35)))
				Expect(observation.Rows).To(Equal(0))
			case "RELEASED_BEFORE_ERROR":
				Expect(observation.GateClosedAtFailure).To(BeFalse())
				Expect(observation.CompletedLoadsAtFailure).To(Equal(1))
				Expect(observation.FailureBytes).To(Equal(int64(89)))
			default:
				Fail("unexpected probe arm " + observation.Arm)
			}
			Expect(observation.IndexBytes).To(Equal(int64(35)))
			Expect(observation.RecordBytes).To(Equal(int64(54)))
			Expect(observation.QuiescedBytes).To(Equal(int64(89)))
			Expect(observation.RecordsScanned).To(Equal(2))
			Expect(observation.ExceptionClass).To(Equal("ScanLimitReachedException"))
		}
		Expect(arms).To(HaveLen(2))
		for _, count := range arms {
			Expect(count).To(Equal(repeats))
		}
		compareQuiescedGoRecordCharge(ctx, env, fixture)
	})
})
