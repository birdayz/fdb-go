package recordlayer

// The runner must put its config's tags on every transaction it opens. The
// helper's unit test covers the loop; this covers the WIRING — deleting the
// applyTags call from runOnce leaves the helper test green, so only a real
// transaction can tell.

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RecordContextConfig transaction tags", func() {
	// Tags are read back off the live transaction. The pure-Go options surface
	// reports them; libfdb_c's cannot, so the assertion is guarded on the
	// capability rather than assuming a backend.
	readTags := func(rtx *FDBRecordContext) ([]string, bool) {
		tagged, ok := rtx.Transaction().Options().(interface{ Tags() []string })
		if !ok {
			return nil, false
		}
		return tagged.Tags(), true
	}

	It("applies configured tags to the transaction it opens", func() {
		ctx := context.Background()
		cfg := &RecordContextConfig{}
		Expect(cfg.SetTags([]string{"gamma", "alpha"})).To(Succeed())

		runner := NewFDBDatabaseRunner(sharedDB).SetContextConfig(cfg)
		checked := false
		_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
			tags, ok := readTags(rtx)
			if !ok {
				return nil, nil
			}
			checked = true
			// SetTags sorts, so the transaction carries them sorted.
			Expect(strings.Join(tags, ",")).To(Equal("alpha,gamma"),
				"the runner must apply ContextConfig.Tags to the transaction")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(checked).To(BeTrue(),
			"the pure-Go backend reports tags, so the assertion must have run — "+
				"a silent skip here would make this test vacuous")
	})

	It("leaves the transaction untagged when no tags are configured", func() {
		ctx := context.Background()
		runner := NewFDBDatabaseRunner(sharedDB).
			SetContextConfig(&RecordContextConfig{})
		_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
			tags, ok := readTags(rtx)
			if !ok {
				return nil, nil
			}
			Expect(tags).To(BeEmpty(),
				"an untagged config must not put tags on the wire")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
