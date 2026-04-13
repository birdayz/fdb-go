package subspace

import (
	"fmt"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/onsi/gomega"
)

func TestSubspaceString(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("test")
	// String() is on the concrete type; use fmt.Sprint to invoke it via Stringer
	str := fmt.Sprint(ss)
	g.Expect(str).To(gomega.ContainSubstring("Subspace"))
}

func TestSubspaceContains(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("myapp")
	key := ss.Pack(tuple.Tuple{int64(42)})

	g.Expect(ss.Contains(key)).To(gomega.BeTrue())
	g.Expect(ss.Contains(fdb.Key([]byte{0x01, 0x02}))).To(gomega.BeFalse())
}

func TestSubspaceUnpackError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("myapp")
	// Key from a different subspace — should error
	other := Sub("other")
	key := other.Pack(tuple.Tuple{int64(1)})

	_, err := ss.Unpack(key)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("not in subspace"))
}

func TestSubspaceFDBRangeKeySelectors(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("test")
	begin, end := ss.FDBRangeKeySelectors()
	g.Expect(begin).NotTo(gomega.BeNil())
	g.Expect(end).NotTo(gomega.BeNil())
}
