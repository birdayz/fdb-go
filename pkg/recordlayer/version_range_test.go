package recordlayer

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FDBRecordVersion range methods", func() {
	Describe("MinVersion", func() {
		It("returns all-zero version", func() {
			v := MinVersion()
			Expect(v.IsComplete()).To(BeTrue())
			Expect(v.GetDBVersion()).To(Equal(int64(0)))
			Expect(v.GetLocalVersion()).To(Equal(0))
		})
	})

	Describe("MaxVersion", func() {
		It("matches Java MAX_VERSION byte pattern", func() {
			v := MaxVersion()
			Expect(v.IsComplete()).To(BeTrue())
			// Java: global bytes 0-8 = 0xFF, byte 9 = 0xFE, local bytes 10-11 = 0xFF
			raw := v.ToBytes()
			for i := 0; i < 9; i++ {
				Expect(raw[i]).To(Equal(byte(0xFF)), "byte %d should be 0xFF", i)
			}
			Expect(raw[9]).To(Equal(byte(0xFE)), "byte 9 should be 0xFE (incomplete marker avoidance)")
			Expect(raw[10]).To(Equal(byte(0xFF)), "byte 10 should be 0xFF")
			Expect(raw[11]).To(Equal(byte(0xFF)), "byte 11 should be 0xFF")
			Expect(v.GetLocalVersion()).To(Equal(0xFFFF))
		})

		It("sorts after MinVersion", func() {
			Expect(MinVersion().Less(MaxVersion())).To(BeTrue())
			Expect(MaxVersion().Less(MinVersion())).To(BeFalse())
		})
	})

	Describe("FirstInDBVersion", func() {
		It("creates version with given DB version and zero local", func() {
			v := FirstInDBVersion(42)
			Expect(v.IsComplete()).To(BeTrue())
			Expect(v.GetDBVersion()).To(Equal(int64(42)))
			Expect(v.GetLocalVersion()).To(Equal(0))
		})
	})

	Describe("LastInDBVersion", func() {
		It("creates version with given DB version and max local", func() {
			v := LastInDBVersion(42)
			Expect(v.IsComplete()).To(BeTrue())
			Expect(v.GetDBVersion()).To(Equal(int64(42)))
			Expect(v.GetLocalVersion()).To(Equal(0xFFFF))
		})

		It("sorts after FirstInDBVersion", func() {
			first := FirstInDBVersion(100)
			last := LastInDBVersion(100)
			Expect(first.Less(last)).To(BeTrue())
		})
	})

	Describe("FirstInGlobalVersion / LastInGlobalVersion", func() {
		It("bookends a global version range", func() {
			gv := make([]byte, 10)
			gv[7] = 0x42 // DB version = 0x42 in last byte
			gv[8] = 0x01 // batch = 0x01xx

			first, err := FirstInGlobalVersion(gv)
			Expect(err).NotTo(HaveOccurred())
			Expect(first.GetLocalVersion()).To(Equal(0))

			last, err := LastInGlobalVersion(gv)
			Expect(err).NotTo(HaveOccurred())
			Expect(last.GetLocalVersion()).To(Equal(0xFFFF))

			Expect(first.Less(last)).To(BeTrue())
			Expect(first.Equal(last)).To(BeFalse())
		})
	})

	Describe("Next / Prev", func() {
		It("increments local version", func() {
			v := FirstInDBVersion(1)
			n := v.Next()
			Expect(n.GetLocalVersion()).To(Equal(1))
			Expect(n.GetDBVersion()).To(Equal(int64(1)))
		})

		It("decrements local version", func() {
			v := LastInDBVersion(1)
			p := v.Prev()
			Expect(p.GetLocalVersion()).To(Equal(0xFFFE))
		})

		It("panics on Next at max", func() {
			v := LastInDBVersion(1)
			Expect(func() { v.Next() }).To(Panic())
		})

		It("panics on Prev at min", func() {
			v := FirstInDBVersion(1)
			Expect(func() { v.Prev() }).To(Panic())
		})

		It("round-trips", func() {
			v := FirstInDBVersion(99)
			Expect(v.Next().Prev().Equal(v)).To(BeTrue())
		})
	})
})
