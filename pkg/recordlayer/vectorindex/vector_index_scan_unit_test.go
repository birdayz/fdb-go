package vectorindex

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("vector leaf cursor honors ctx cancellation (RFC-106a)", func() {
	It("vectorSearchCursor.OnNext returns the ctx error", func() {
		c := &vectorSearchCursor{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.OnNext(ctx)
		Expect(err).To(Equal(context.Canceled))
	})
})
