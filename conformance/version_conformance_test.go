//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// versionResult is the structure returned by Java's loadOrderWithVersion
type versionResult struct {
	OrderID       int64  `json:"orderId"`
	Price         int32  `json:"price"`
	HasVersion    bool   `json:"hasVersion"`
	GlobalVersion string `json:"globalVersion"` // base64
	LocalVersion  int    `json:"localVersion"`
	IsComplete    bool   `json:"isComplete"`
}

var _ = Describe("Record Version Conformance", func() {
	var (
		ctx         context.Context
		env         *TenantEnvironment
		java        *JavaInvoker
		versionMeta *recordlayer.RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		tenantName := fmt.Sprintf("version_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		java = NewJavaInvoker()

		// Create versioned metadata for Go side
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.SetStoreRecordVersions(true)
		versionMeta, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	buildJavaParams := func() map[string]any {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"subspace":    BytesToIntArray(env.Keyspace.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	saveOrderWithGoVersioned := func(order *gen.Order) []byte {
		_, vs, err := env.RecordDB.RunWithVersionstamp(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(versionMeta).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		return vs
	}

	loadVersionWithGo := func(orderID int64) *recordlayer.FDBRecordVersion {
		var version *recordlayer.FDBRecordVersion
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(versionMeta).
				SetSubspace(env.Keyspace).
				Open()
			if err != nil {
				return nil, err
			}
			version, err = store.LoadRecordVersion(tuple.Tuple{orderID}, false)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		return version
	}

	loadVersionWithJava := func(orderID int64) versionResult {
		params := buildJavaParams()
		params["orderID"] = orderID
		raw, err := java.Invoke(ctx, "loadOrderWithVersion", params)
		Expect(err).NotTo(HaveOccurred())

		var result versionResult
		err = json.Unmarshal(raw, &result)
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	saveOrderWithJavaVersioned := func(order *gen.Order) {
		params := buildJavaParams()
		params["order"] = order
		err := java.InvokeAs(ctx, "saveOrderVersioned", params, nil)
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("Go saves versioned, Java reads version", func() {
		It("should store and read back version bytes", func() {
			vs := saveOrderWithGoVersioned(StandardOrder(1))
			Expect(vs).NotTo(BeNil())
			Expect(len(vs)).To(Equal(recordlayer.GlobalVersionBytes))

			// Java reads the version
			result := loadVersionWithJava(1)
			Expect(result.OrderID).To(Equal(int64(1)))
			Expect(result.HasVersion).To(BeTrue())
			Expect(result.IsComplete).To(BeTrue())
			Expect(result.LocalVersion).To(Equal(0))

			// Decode base64 global version and compare with Go's versionstamp
			javaGlobal, err := base64.StdEncoding.DecodeString(result.GlobalVersion)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaGlobal).To(Equal(vs))
		})
	})

	Describe("Java saves versioned, Go reads version", func() {
		It("should read version saved by Java", func() {
			saveOrderWithJavaVersioned(StandardOrder(2))

			// Go reads the version
			version := loadVersionWithGo(2)
			Expect(version).NotTo(BeNil())
			Expect(version.IsComplete()).To(BeTrue())
			Expect(version.GetLocalVersion()).To(Equal(0))

			// Also verify Java can read back its own version
			javaResult := loadVersionWithJava(2)
			Expect(javaResult.HasVersion).To(BeTrue())
			Expect(javaResult.IsComplete).To(BeTrue())
			Expect(javaResult.LocalVersion).To(Equal(0))

			// Global version should match between Go and Java
			javaGlobal, err := base64.StdEncoding.DecodeString(javaResult.GlobalVersion)
			Expect(err).NotTo(HaveOccurred())
			Expect(version.GetGlobalVersion()).To(Equal(javaGlobal))
		})
	})

	Describe("Version local ordering", func() {
		It("should assign sequential local versions within one transaction", func() {
			_, vs, err := env.RecordDB.RunWithVersionstamp(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(versionMeta).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(0); i < 3; i++ {
					_, err = store.SaveRecord(StandardOrder(i))
					if err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(vs).NotTo(BeNil())

			// Read back versions — both Go and Java should see local versions 0, 1, 2
			for i := int64(0); i < 3; i++ {
				goVersion := loadVersionWithGo(i)
				Expect(goVersion).NotTo(BeNil())
				Expect(goVersion.IsComplete()).To(BeTrue())
				Expect(goVersion.GetLocalVersion()).To(Equal(int(i)))
				Expect(goVersion.GetGlobalVersion()).To(Equal(vs))

				javaResult := loadVersionWithJava(i)
				Expect(javaResult.HasVersion).To(BeTrue())
				Expect(javaResult.IsComplete).To(BeTrue())
				Expect(javaResult.LocalVersion).To(Equal(int(i)))

				javaGlobal, err := base64.StdEncoding.DecodeString(javaResult.GlobalVersion)
				Expect(err).NotTo(HaveOccurred())
				Expect(javaGlobal).To(Equal(vs))
			}
		})
	})

	Describe("Version updated on re-save", func() {
		It("should get new version when record is updated", func() {
			vs1 := saveOrderWithGoVersioned(StandardOrder(10))

			// Update with new data
			updated := NewOrder(10).WithPrice(999).WithFlower("Updated", gen.Color_BLUE).Build()
			vs2 := saveOrderWithGoVersioned(updated)

			// Versionstamps should differ (different transactions)
			Expect(vs1).NotTo(Equal(vs2))

			// Java should see the latest version
			javaResult := loadVersionWithJava(10)
			Expect(javaResult.HasVersion).To(BeTrue())
			Expect(javaResult.IsComplete).To(BeTrue())

			javaGlobal, err := base64.StdEncoding.DecodeString(javaResult.GlobalVersion)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaGlobal).To(Equal(vs2))

			// Go should also see the latest version
			goVersion := loadVersionWithGo(10)
			Expect(goVersion).NotTo(BeNil())
			Expect(goVersion.GetGlobalVersion()).To(Equal(vs2))
		})
	})
})
