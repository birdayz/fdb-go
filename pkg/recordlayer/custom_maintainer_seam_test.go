package recordlayer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Custom index types registered the way an external package registers its
// own: through RegisterIndexMaintainerFactory from init, using only exported
// API in the factory and maintainer.
const (
	seamValidatedIndexType = "custom_seam_validated_v1"
	seamCounterIndexType   = "custom_seam_nonidempotent_counter_v1"
)

func init() {
	RegisterIndexMaintainerFactory(seamValidatedFactory{})
	RegisterIndexMaintainerFactory(seamCounterFactory{})
}

// seamOptionError is the custom factory's own error type: Build must return it
// unchanged, so a caller can errors.As it.
type seamOptionError struct{ index, value string }

func (e *seamOptionError) Error() string {
	return fmt.Sprintf("index %s: mode %q must be a or b", e.index, e.value)
}

// seamValidateCalls counts ValidateIndexOptions calls per index name; names
// are unique per spec, so parallel specs do not share a counter.
var seamValidateCalls sync.Map

func seamValidateCount(indexName string) int64 {
	c, ok := seamValidateCalls.Load(indexName)
	if !ok {
		return 0
	}
	return c.(*atomic.Int64).Load()
}

type seamValidatedFactory struct{}

func (seamValidatedFactory) IndexTypes() []string { return []string{seamValidatedIndexType} }

func (seamValidatedFactory) IsIdempotent(*Index) bool { return true }

func (seamValidatedFactory) ValidateIndexOptions(index *Index) error {
	c, _ := seamValidateCalls.LoadOrStore(index.Name, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
	if mode := index.Options["mode"]; mode != "a" && mode != "b" {
		return &seamOptionError{index: index.Name, value: mode}
	}
	return nil
}

func (seamValidatedFactory) ValidateChangedOptions(_, _ *Index, _ map[string]bool) error { return nil }

func (seamValidatedFactory) NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error) {
	return NewStandardIndexMaintainer(state), nil
}

// seamCounterFactory maintains a non-idempotent index: every record image it
// is handed adds one to an "added" or "removed" counter, so applying a record
// the online build will also apply counts it twice.
type seamCounterFactory struct{}

func (seamCounterFactory) IndexTypes() []string { return []string{seamCounterIndexType} }

func (seamCounterFactory) IsIdempotent(*Index) bool { return false }

func (seamCounterFactory) ValidateIndexOptions(*Index) error { return nil }

func (seamCounterFactory) ValidateChangedOptions(_, _ *Index, _ map[string]bool) error { return nil }

func (seamCounterFactory) NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error) {
	return &seamCounterMaintainer{StandardIndexMaintainer: *NewStandardIndexMaintainer(state)}, nil
}

type seamCounterMaintainer struct {
	StandardIndexMaintainer
}

func seamCounterKey(m *StandardIndexMaintainer, which string) []byte {
	return m.IndexSubspace().Pack(tuple.Tuple{which})
}

func (m *seamCounterMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	one := make([]byte, 8)
	binary.LittleEndian.PutUint64(one, 1)
	if oldRecord != nil {
		m.Transaction().Add(fdb.Key(seamCounterKey(&m.StandardIndexMaintainer, "removed")), one)
	}
	if newRecord != nil {
		m.Transaction().Add(fdb.Key(seamCounterKey(&m.StandardIndexMaintainer, "added")), one)
	}
	return nil
}

// UpdateWhileWriteOnly passes this maintainer's own Update: the embedded
// StandardIndexMaintainer.UpdateWhileWriteOnly would call the base Update, as
// Go embedding is not virtual dispatch.
func (m *seamCounterMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return UpdateWhileWriteOnlyNonIdempotent(oldRecord, newRecord, m.Index(), m.Store(), m.Update)
}

var _ = Describe("custom index maintainer seams", func() {
	ctx := context.Background()

	customIndex := func(name, typ string, options map[string]string) *Index {
		idx := NewIndex(name, Field("price"))
		idx.Type = typ
		for k, v := range options {
			idx.SetOption(k, v)
		}
		return idx
	}

	Describe("RecordMetaDataBuilder.Build", func() {
		It("rejects a registered custom type's invalid options with the factory's own error", func() {
			idx := customIndex("Order$seam_invalid", seamValidatedIndexType, map[string]string{"mode": "c"})
			builder := baseBuilder()
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(md).To(BeNil())
			var optionErr *seamOptionError
			Expect(errors.As(err, &optionErr)).To(BeTrue(), "expected the factory's option error, got %T: %v", err, err)
			Expect(optionErr.index).To(Equal(idx.Name))
			Expect(optionErr.value).To(Equal("c"))
			Expect(seamValidateCount(idx.Name)).To(Equal(int64(1)))
		})

		It("rejects a registered custom type with its option missing", func() {
			idx := customIndex("Order$seam_missing", seamValidatedIndexType, nil)
			builder := baseBuilder()
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			var optionErr *seamOptionError
			Expect(errors.As(err, &optionErr)).To(BeTrue(), "expected the factory's option error, got %T: %v", err, err)
			Expect(optionErr.value).To(Equal(""))
		})

		It("builds a registered custom type with valid options, validating it once", func() {
			idx := customIndex("Order$seam_valid", seamValidatedIndexType, map[string]string{"mode": "b"})
			builder := baseBuilder()
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetIndex(idx.Name)).NotTo(BeNil())
			Expect(seamValidateCount(idx.Name)).To(Equal(int64(1)))
		})

		It("does not ask the factory about another type's index", func() {
			idx := customIndex("Order$seam_value", IndexTypeValue, map[string]string{"mode": "c"})
			builder := baseBuilder()
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			Expect(seamValidateCount(idx.Name)).To(BeZero())
		})

		It("still builds an index whose type no factory registers", func() {
			// Meta-data written by another program stays loadable; only
			// maintaining the index refuses (UnknownIndexTypeError).
			idx := customIndex("Order$seam_unregistered", "custom_seam_unregistered_v1", map[string]string{"mode": "c"})
			builder := baseBuilder()
			builder.AddIndex("Order", idx)
			_, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("UpdateWhileWriteOnlyNonIdempotent", func() {
		It("applies an external maintainer's update only inside the built range", func() {
			ks := specSubspace()
			idx := customIndex("Order$seam_counter", seamCounterIndexType, nil)
			builder := baseBuilder()
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.ClearAndMarkIndexWriteOnly(idx.Name)
				Expect(err).NotTo(HaveOccurred())
				Expect(store.GetIndexState(idx.Name)).To(Equal(IndexStateWriteOnly))

				// A by-records build has covered primary keys below 5.
				_, err = NewIndexingRangeSet(ks, idx).InsertRange(rtx.Transaction(), []byte{0x00}, tuple.Tuple{int64(5)}.Pack(), false)
				Expect(err).NotTo(HaveOccurred())

				for _, id := range []int64{2, 3, 7, 10} {
					_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())
				}
				// An update of a covered record hands both images to update.
				_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				// An update of an uncovered record reaches nothing.
				_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(7), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				for _, id := range []int64{2, 10} {
					_, err = store.DeleteRecord(tuple.Tuple{id})
					Expect(err).NotTo(HaveOccurred())
				}

				read := func(which string) int64 {
					maintainer, err := store.GetIndexMaintainer(idx)
					Expect(err).NotTo(HaveOccurred())
					counter, ok := maintainer.(*seamCounterMaintainer)
					Expect(ok).To(BeTrue(), "store built %T, not the registered maintainer", maintainer)
					raw, err := rtx.Transaction().Get(fdb.Key(seamCounterKey(&counter.StandardIndexMaintainer, which))).Get()
					Expect(err).NotTo(HaveOccurred())
					if raw == nil {
						return 0
					}
					return int64(binary.LittleEndian.Uint64(raw))
				}
				// Saves of 2 and 3, then the update of 3.
				Expect(read("added")).To(Equal(int64(3)))
				// The update of 3, then the delete of 2.
				Expect(read("removed")).To(Equal(int64(2)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// seamBuildStore is the build coordination UpdateWhileWriteOnlyNonIdempotent
// asks of a store; any other IndexStoreContext method panics on the nil
// embedded interface.
type seamBuildStore struct {
	IndexStoreContext
	source    *Index
	built     map[string]bool
	entryKeys map[*FDBStoredRecord[proto.Message]]tuple.Tuple
	rangeErr  error
}

func (s *seamBuildStore) isKeyInIndexBuildRange(_ *Index, key tuple.Tuple) (bool, error) {
	if s.rangeErr != nil {
		return false, s.rangeErr
	}
	return s.built[string(key.Pack())], nil
}

func (s *seamBuildStore) writeOnlyBuildSource(*Index) (*Index, error) { return s.source, nil }

func (s *seamBuildStore) sourceIndexEntryKey(_ *Index, record *FDBStoredRecord[proto.Message]) (tuple.Tuple, error) {
	if record == nil {
		return nil, nil
	}
	return s.entryKeys[record], nil
}

func TestUpdateWhileWriteOnlyNonIdempotentDispatch(t *testing.T) {
	t.Parallel()
	index := NewIndex("seam_counter", Field("price"))
	index.Type = seamCounterIndexType
	source := NewIndex("seam_source", Field("price"))
	oldRec := &FDBStoredRecord[proto.Message]{PrimaryKey: tuple.Tuple{int64(1)}}
	newRec := &FDBStoredRecord[proto.Message]{PrimaryKey: tuple.Tuple{int64(1)}}
	built := func(keys ...tuple.Tuple) map[string]bool {
		m := map[string]bool{}
		for _, k := range keys {
			m[string(k.Pack())] = true
		}
		return m
	}
	pk := tuple.Tuple{int64(1)}
	oldKey, newKey := tuple.Tuple{int64(10), int64(1)}, tuple.Tuple{int64(20), int64(1)}
	type call struct{ old, new bool }

	for _, tc := range []struct {
		name     string
		store    *seamBuildStore
		nilStore bool
		old, new *FDBStoredRecord[proto.Message]
		want     []call
	}{
		{name: "no store applies", nilStore: true, old: oldRec, new: newRec, want: []call{{true, true}}},
		{name: "no images applies nothing", nilStore: true, want: nil},
		{name: "by records, covered", store: &seamBuildStore{built: built(pk)}, old: oldRec, new: newRec, want: []call{{true, true}}},
		{name: "by records, uncovered", store: &seamBuildStore{built: built()}, old: oldRec, new: newRec, want: nil},
		{name: "by records, covered delete", store: &seamBuildStore{built: built(pk)}, old: oldRec, want: []call{{true, false}}},
		{name: "by index, same key covered", store: &seamBuildStore{
			source: source, built: built(oldKey),
			entryKeys: map[*FDBStoredRecord[proto.Message]]tuple.Tuple{oldRec: oldKey, newRec: oldKey},
		}, old: oldRec, new: newRec, want: []call{{true, true}}},
		{name: "by index, same key uncovered", store: &seamBuildStore{
			source: source, built: built(),
			entryKeys: map[*FDBStoredRecord[proto.Message]]tuple.Tuple{oldRec: oldKey, newRec: oldKey},
		}, old: oldRec, new: newRec, want: nil},
		{name: "by index, moved out of the covered range", store: &seamBuildStore{
			source: source, built: built(oldKey),
			entryKeys: map[*FDBStoredRecord[proto.Message]]tuple.Tuple{oldRec: oldKey, newRec: newKey},
		}, old: oldRec, new: newRec, want: []call{{true, false}}},
		{name: "by index, moved into the covered range", store: &seamBuildStore{
			source: source, built: built(newKey),
			entryKeys: map[*FDBStoredRecord[proto.Message]]tuple.Tuple{oldRec: oldKey, newRec: newKey},
		}, old: oldRec, new: newRec, want: []call{{false, true}}},
		{name: "by index, moved within the covered range", store: &seamBuildStore{
			source: source, built: built(oldKey, newKey),
			entryKeys: map[*FDBStoredRecord[proto.Message]]tuple.Tuple{oldRec: oldKey, newRec: newKey},
		}, old: oldRec, new: newRec, want: []call{{true, false}, {false, true}}},
		{name: "by index, only the new image has an entry", store: &seamBuildStore{
			source: source, built: built(newKey),
			entryKeys: map[*FDBStoredRecord[proto.Message]]tuple.Tuple{newRec: newKey},
		}, old: oldRec, new: newRec, want: []call{{true, true}}},
		{
			name: "by index, neither image has an entry", store: &seamBuildStore{source: source, built: built(oldKey, newKey)},
			old: oldRec, new: newRec, want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []call
			update := func(o, n *FDBStoredRecord[proto.Message]) error {
				got = append(got, call{o != nil, n != nil})
				if o != nil && o != tc.old || n != nil && n != tc.new {
					t.Errorf("update received images other than the caller's")
				}
				return nil
			}
			var store IndexStoreContext
			if !tc.nilStore {
				store = tc.store
			}
			if err := UpdateWhileWriteOnlyNonIdempotent(tc.old, tc.new, index, store, update); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("update calls = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("range check failure names the index and applies nothing", func(t *testing.T) {
		cause := errors.New("range read failed")
		applied := false
		err := UpdateWhileWriteOnlyNonIdempotent(oldRec, newRec, index, &seamBuildStore{rangeErr: cause},
			func(_, _ *FDBStoredRecord[proto.Message]) error { applied = true; return nil })
		if !errors.Is(err, cause) {
			t.Fatalf("err = %v, want it to wrap %v", err, cause)
		}
		if applied {
			t.Fatal("update ran although the range check failed")
		}
		want := fmt.Sprintf("%s index %q", seamCounterIndexType, index.Name)
		if got := err.Error(); !strings.Contains(got, want) {
			t.Fatalf("err %q does not name %s", got, want)
		}
	})

	t.Run("update failure is returned", func(t *testing.T) {
		cause := errors.New("update failed")
		err := UpdateWhileWriteOnlyNonIdempotent(oldRec, newRec, index, &seamBuildStore{built: built(pk)},
			func(_, _ *FDBStoredRecord[proto.Message]) error { return cause })
		if !errors.Is(err, cause) {
			t.Fatalf("err = %v, want %v", err, cause)
		}
	})
}
