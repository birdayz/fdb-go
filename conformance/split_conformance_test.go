package conformance_test

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
	"github.com/birdayz/fdb-record-layer-go/gen"
)

var _ = Describe("Split Record Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TenantEnvironment
		store *helpers.SplitConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("split_%s", uuid.New().String())

		var err error
		env, err = helpers.SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = helpers.NewSplitConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, Java reads", func() {
		It("should handle 250KB split record (3 chunks)", func() {
			// 250KB will be split into 3 chunks at 100KB boundaries
			padding := strings.Repeat("X", 250_000)
			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(42),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_RED.Enum()},
			}
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle small record with split enabled (unsplit path)", func() {
			// A record under 100KB should still work correctly when split is enabled.
			// It goes through the unsplit path (suffix 0) instead of being chunked.
			order := helpers.StandardOrder(2)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle 150KB split record (2 chunks)", func() {
			padding := strings.Repeat("A", 150_000)
			order := &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(77),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_BLUE.Enum()},
			}
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes, Go reads", func() {
		It("should handle 250KB split record from Java", func() {
			// Java saves a large record split across multiple KV pairs,
			// Go must reassemble the chunks correctly.
			padding := strings.Repeat("Y", 250_000)
			order := &gen.Order{
				OrderId: proto.Int64(10),
				Price:   proto.Int32(99),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_BLUE.Enum()},
			}
			loaded, err := store.JavaSaveThenGoLoad(ctx, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(order, loaded)).To(BeTrue())
		})

		It("should handle small record from Java with split enabled", func() {
			order := helpers.NewOrder(11).
				WithPrice(33).
				WithFlower("Daisy", gen.Color_YELLOW).
				Build()
			loaded, err := store.JavaSaveThenGoLoad(ctx, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(order, loaded)).To(BeTrue())
		})

		It("should handle 150KB split record from Java", func() {
			padding := strings.Repeat("B", 150_000)
			order := &gen.Order{
				OrderId: proto.Int64(12),
				Price:   proto.Int32(55),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_PINK.Enum()},
			}
			loaded, err := store.JavaSaveThenGoLoad(ctx, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(order, loaded)).To(BeTrue())
		})
	})

	Describe("Boundary sizes", func() {
		It("should handle record at approximately 100KB", func() {
			// Right around the split boundary. The serialized protobuf size
			// determines whether splitting occurs, not just the string length.
			padding := strings.Repeat("Z", 100_000)
			order := &gen.Order{
				OrderId: proto.Int64(20),
				Price:   proto.Int32(1),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_YELLOW.Enum()},
			}
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle minimal order with split enabled", func() {
			order := helpers.MinimalOrder(21)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Overwrite with split", func() {
		It("should handle overwriting a split record with a small record", func() {
			// First write a large split record
			padding := strings.Repeat("L", 200_000)
			large := &gen.Order{
				OrderId: proto.Int64(30),
				Price:   proto.Int32(100),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_RED.Enum()},
			}
			err := store.SaveRecord(ctx, large)
			Expect(err).NotTo(HaveOccurred())

			// Overwrite with a small record — old split chunks must be cleared
			small := helpers.NewOrder(30).
				WithPrice(1).
				WithFlower("Tiny", gen.Color_BLUE).
				Build()
			err = store.SaveRecord(ctx, small)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle overwriting a small record with a split record", func() {
			// First write a small record
			small := helpers.NewOrder(31).
				WithPrice(5).
				WithFlower("Small", gen.Color_YELLOW).
				Build()
			err := store.SaveRecord(ctx, small)
			Expect(err).NotTo(HaveOccurred())

			// Overwrite with a large split record
			padding := strings.Repeat("G", 200_000)
			large := &gen.Order{
				OrderId: proto.Int64(31),
				Price:   proto.Int32(200),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_PINK.Enum()},
			}
			err = store.SaveRecord(ctx, large)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
