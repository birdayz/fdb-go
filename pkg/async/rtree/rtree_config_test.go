package rtree

import (
	"context"
	"errors"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/javanum"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Java's RTree takes any minM, maxM and splitS (MultiDimensionalIndexHelper.
// getConfig). A configuration that cannot keep the node invariants fails at
// the operation where Java's does, and the failed operation commits nothing.
var _ = Describe("RTree configurations", func() {
	for _, storage := range []RTreeStorage{RTreeStorageByNode, RTreeStorageBySlot} {
		rtreeConfigurationSpecs(storage)
	}
	It("still refuses no dimensions", func() {
		_, err := NewRTree(nil, DefaultRTreeConfig(0))
		Expect(err).To(MatchError(ContainSubstring("NumDimensions")))
	})
})

func rtreeConfigurationSpecs(storage RTreeStorage) {
	ctx := context.Background()

	type tree struct {
		rt   *RTree
		name string
	}
	newTree := func(name string, minM, maxM, splitS int) tree {
		config := DefaultRTreeConfig(2)
		config.MinM, config.MaxM, config.SplitS, config.Storage = minM, maxM, splitS, storage
		sub := specSubspace().Sub(name)
		rt, err := NewRTree(NewStorageAdapter(sub, config), config)
		Expect(err).NotTo(HaveOccurred())
		return tree{rt: rt, name: name}
	}
	reopen := func(t tree, minM, maxM, splitS int) tree {
		return newTree(t.name, minM, maxM, splitS)
	}
	point := func(pk int64) Point { return Point{Coordinates: tuple.Tuple{pk, pk}} }
	// insert commits pks in batches of batch; it returns how many committed
	// before the first failing batch, and that batch's error.
	insert := func(t tree, from, to int64, batch int64) (int64, error) {
		for lo := from; lo < to; lo += batch {
			_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
				for pk := lo; pk < min(lo+batch, to); pk++ {
					if err := t.rt.InsertOrUpdate(rtx.Transaction(), point(pk), tuple.Tuple{pk}, tuple.Tuple{}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			if err != nil {
				return lo - from, err
			}
		}
		return to - from, nil
	}
	// deleteEach deletes pks one per transaction and returns the first error.
	deleteEach := func(t tree, pks []int64) (int, error) {
		for i, pk := range pks {
			_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
				return nil, t.rt.Delete(rtx.Transaction(), point(pk), tuple.Tuple{pk})
			})
			if err != nil {
				return i, err
			}
		}
		return len(pks), nil
	}
	scanPKs := func(t tree) []int64 {
		var pks []int64
		_, err := sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			items, err := t.rt.Scan(rtx.Transaction(), nil, nil, nil)
			for _, item := range items {
				pks = append(pks, item.KeySuffix[0].(int64))
			}
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		slices.Sort(pks)
		return pks
	}
	pkRange := func(from, to int64) []int64 {
		var pks []int64
		for pk := from; pk < to; pk++ {
			pks = append(pks, pk)
		}
		return pks
	}

	It(string(storage)+": maintains and scans minM 0 with maxM 2000, past a root split", func() {
		t := newTree("wide", 0, 2000, 2)
		n, err := insert(t, 0, 2100, 20)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(2100)))
		Expect(scanPKs(t)).To(Equal(pkRange(0, 2100)))
		deleted, err := deleteEach(t, pkRange(0, 50))
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(Equal(50))
		Expect(scanPKs(t)).To(Equal(pkRange(50, 2100)))
	})

	DescribeTable(string(storage)+": an insert under a configuration Java cannot maintain fails where Java's does",
		func(minM, maxM, splitS int, committed int64, match func(error) bool) {
			t := newTree("insert", minM, maxM, splitS)
			n, err := insert(t, 0, 40, 1)
			Expect(err).To(HaveOccurred())
			Expect(match(err)).To(BeTrue(), "error: %v", err)
			Expect(n).To(Equal(committed))
			Expect(scanPKs(t)).To(Equal(pkRange(0, n)), "a failed insert lost or kept an item")
		},
		// RTree.java:797, Verify on the first insert.
		Entry("maxM below zero", 0, -1, 2, int64(0), isErr[*VerifyError]),
		// RTree.java:1070, a root split with an empty left half.
		Entry("maxM zero", 0, 0, 2, int64(0), isErr[*IndexOutOfBoundsError]),
		// The third insert splits a full root into one intermediate child of two,
		// over maxM 1; Java writes it and its next read refuses it.
		Entry("maxM one", 1, 1, 2, int64(2), isPacking),
		// The first root split leaves halves of 6 and 7 below minM 10.
		Entry("a split ratio maxM cannot keep", 10, 12, 2, int64(12), isPacking),
		// RTree.java:1622. The fifth point splits the root into leaves of 2 and
		// 3; the seventh, (6, 6), is the first to overflow one of them.
		Entry("splitS below zero", 2, 4, -1, int64(6), isErr[*NegativeArraySizeError]),
		// RTree.java:952, the split links after sibling -1.
		Entry("splitS zero", 2, 4, 0, int64(6), isOutOfBounds(-1)),
	)

	DescribeTable(string(storage)+": a delete under a configuration Java cannot maintain fails where Java's does",
		func(minM, maxM, splitS int, match func(error) bool) {
			t := newTree("delete", minM, maxM, splitS)
			n, err := insert(t, 0, 5, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(int64(5)))
			deleted, err := deleteEach(t, pkRange(0, 5))
			Expect(err).To(HaveOccurred())
			Expect(match(err)).To(BeTrue(), "error: %v", err)
			Expect(scanPKs(t)).To(Equal(pkRange(int64(deleted), 5)), "a failed delete lost or kept an item")
		},
		// RTree.java:1447, the MBR of a non-root leaf emptied without a fuse.
		Entry("minM zero", 0, 4, 2, isErr[*NullPointerError]),
		// RTree.java:1273, the fuse's tombstone is sibling -1.
		Entry("splitS zero", 2, 4, 0, isOutOfBounds(-1)),
		// RTree.java:1304, the fuse leaves no sibling to divide by.
		Entry("splitS one", 2, 4, 1, isErr[*ArithmeticError]),
	)

	It(string(storage)+": refuses to read a non-root node outside [minM, maxM], as Java's checkNode does", func() {
		t := newTree("packing", 2, 4, 2)
		_, err := insert(t, 0, 5, 5) // a root split into leaves of 2 and 3
		Expect(err).NotTo(HaveOccurred())
		Expect(scanPKs(t)).To(HaveLen(5))

		strict := reopen(t, 3, 4, 2)
		_, err = sharedDB.Run(ctx, func(rtx *testContext) (any, error) {
			return strict.rt.Scan(rtx.Transaction(), nil, nil, nil)
		})
		Expect(isPacking(err)).To(BeTrue(), "error: %v", err)
		Expect(scanPKs(t)).To(Equal(pkRange(0, 5)))
	})
}

func isOutOfBounds(index int) func(error) bool {
	return func(err error) bool {
		var oob *IndexOutOfBoundsError
		return errors.As(err, &oob) && oob.Index == index
	}
}

func isErr[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

func isPacking(err error) bool {
	var state *javanum.IllegalStateError
	return errors.As(err, &state) && state.Message == "packing of non-root is out of valid range"
}
