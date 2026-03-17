package recordlayer

import (
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Error types", func() {

	// Verify all error types satisfy the error interface and are matchable via errors.As().

	Describe("RecordStoreAlreadyExistsError", func() {
		It("implements error interface", func() {
			var err error = &RecordStoreAlreadyExistsError{}
			Expect(err.Error()).To(Equal("record store already exists"))
		})

		It("is matchable via errors.As", func() {
			err := fmt.Errorf("wrapped: %w", &RecordStoreAlreadyExistsError{})
			var target *RecordStoreAlreadyExistsError
			Expect(errors.As(err, &target)).To(BeTrue())
		})
	})

	Describe("RecordStoreDoesNotExistError", func() {
		It("implements error interface", func() {
			var err error = &RecordStoreDoesNotExistError{}
			Expect(err.Error()).To(Equal("record store does not exist"))
		})

		It("is matchable via errors.As", func() {
			err := fmt.Errorf("context: %w", &RecordStoreDoesNotExistError{})
			var target *RecordStoreDoesNotExistError
			Expect(errors.As(err, &target)).To(BeTrue())
		})
	})

	Describe("RecordStoreNoInfoButNotEmptyError", func() {
		It("includes first key in message when set", func() {
			err := &RecordStoreNoInfoButNotEmptyError{FirstKey: []byte{0x01, 0x02}}
			Expect(err.Error()).To(ContainSubstring("0102"))
		})

		It("omits first key when nil", func() {
			err := &RecordStoreNoInfoButNotEmptyError{}
			Expect(err.Error()).To(Equal("record store has no info but is not empty"))
		})

		It("is matchable via errors.As and exposes FirstKey", func() {
			orig := &RecordStoreNoInfoButNotEmptyError{FirstKey: []byte{0xAB}}
			wrapped := fmt.Errorf("op: %w", orig)
			var target *RecordStoreNoInfoButNotEmptyError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.FirstKey).To(Equal([]byte{0xAB}))
		})
	})

	Describe("RecordStoreStateNotLoadedError", func() {
		It("implements error interface", func() {
			var err error = &RecordStoreStateNotLoadedError{}
			Expect(err.Error()).To(Equal("record store state not loaded"))
		})

		It("is matchable via errors.As", func() {
			err := fmt.Errorf("x: %w", &RecordStoreStateNotLoadedError{})
			var target *RecordStoreStateNotLoadedError
			Expect(errors.As(err, &target)).To(BeTrue())
		})
	})

	Describe("IndexNotReadableError", func() {
		It("includes index name and state", func() {
			err := &IndexNotReadableError{IndexName: "my_idx", CurrentState: IndexStateWriteOnly}
			Expect(err.Error()).To(ContainSubstring("my_idx"))
			Expect(err.Error()).To(ContainSubstring("WRITE_ONLY"))
		})

		It("is matchable and exposes fields", func() {
			wrapped := fmt.Errorf("scan: %w", &IndexNotReadableError{IndexName: "idx", CurrentState: IndexStateDisabled})
			var target *IndexNotReadableError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.IndexName).To(Equal("idx"))
			Expect(target.CurrentState).To(Equal(IndexStateDisabled))
		})
	})

	Describe("IndexNotFoundError", func() {
		It("includes index name", func() {
			err := &IndexNotFoundError{IndexName: "missing_idx"}
			Expect(err.Error()).To(ContainSubstring("missing_idx"))
		})

		It("is matchable and exposes IndexName", func() {
			wrapped := fmt.Errorf("get: %w", &IndexNotFoundError{IndexName: "foo"})
			var target *IndexNotFoundError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.IndexName).To(Equal("foo"))
		})
	})

	Describe("IndexNotBuiltError", func() {
		It("includes index name", func() {
			err := &IndexNotBuiltError{IndexName: "unbuilt"}
			Expect(err.Error()).To(ContainSubstring("unbuilt"))
		})

		It("is matchable", func() {
			wrapped := fmt.Errorf("mark: %w", &IndexNotBuiltError{IndexName: "x"})
			var target *IndexNotBuiltError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
		})
	})

	Describe("MetaDataError", func() {
		It("returns custom message", func() {
			err := &MetaDataError{Message: "bad schema"}
			Expect(err.Error()).To(Equal("bad schema"))
		})

		It("is matchable and exposes Message", func() {
			wrapped := fmt.Errorf("build: %w", &MetaDataError{Message: "oops"})
			var target *MetaDataError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.Message).To(Equal("oops"))
		})
	})

	Describe("UnsupportedFormatVersionError", func() {
		It("includes version numbers", func() {
			err := &UnsupportedFormatVersionError{Version: 99, MaxVersion: 14}
			Expect(err.Error()).To(ContainSubstring("99"))
			Expect(err.Error()).To(ContainSubstring("14"))
		})

		It("is matchable and exposes fields", func() {
			wrapped := fmt.Errorf("open: %w", &UnsupportedFormatVersionError{Version: 20, MaxVersion: 14})
			var target *UnsupportedFormatVersionError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.Version).To(Equal(int32(20)))
			Expect(target.MaxVersion).To(Equal(int32(14)))
		})
	})

	Describe("RecordSerializationError", func() {
		It("wraps cause", func() {
			cause := fmt.Errorf("proto: bad wire type")
			err := &RecordSerializationError{Cause: cause}
			Expect(err.Error()).To(ContainSubstring("proto: bad wire type"))
		})

		It("Unwrap returns cause", func() {
			cause := fmt.Errorf("inner")
			err := &RecordSerializationError{Cause: cause}
			Expect(errors.Unwrap(err)).To(Equal(cause))
		})

		It("is matchable through wrapping", func() {
			inner := &RecordSerializationError{Cause: fmt.Errorf("x")}
			wrapped := fmt.Errorf("save: %w", inner)
			var target *RecordSerializationError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
		})
	})

	Describe("RecordDeserializationError", func() {
		It("includes primary key when set", func() {
			err := &RecordDeserializationError{PrimaryKey: "pk-42", Cause: fmt.Errorf("bad proto")}
			Expect(err.Error()).To(ContainSubstring("pk-42"))
			Expect(err.Error()).To(ContainSubstring("bad proto"))
		})

		It("omits primary key when nil", func() {
			err := &RecordDeserializationError{Cause: fmt.Errorf("bad")}
			Expect(err.Error()).NotTo(ContainSubstring("nil"))
		})

		It("Unwrap returns cause", func() {
			cause := fmt.Errorf("inner")
			err := &RecordDeserializationError{Cause: cause}
			Expect(errors.Unwrap(err)).To(Equal(cause))
		})

		It("is matchable and exposes fields", func() {
			inner := &RecordDeserializationError{PrimaryKey: "pk", Cause: fmt.Errorf("x")}
			wrapped := fmt.Errorf("load: %w", inner)
			var target *RecordDeserializationError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.PrimaryKey).To(Equal("pk"))
		})
	})

	Describe("KeyExpressionError", func() {
		It("returns message", func() {
			err := &KeyExpressionError{Message: "field not found"}
			Expect(err.Error()).To(Equal("field not found"))
		})

		It("is matchable", func() {
			wrapped := fmt.Errorf("eval: %w", &KeyExpressionError{Message: "x"})
			var target *KeyExpressionError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
		})
	})

	Describe("PartlyBuiltError", func() {
		It("includes all fields", func() {
			err := &PartlyBuiltError{
				IndexName:     "idx",
				SavedStamp:    "BY_RECORDS",
				ExpectedStamp: "BY_INDEX",
				Message:       "stamp mismatch",
			}
			Expect(err.Error()).To(ContainSubstring("idx"))
			Expect(err.Error()).To(ContainSubstring("stamp mismatch"))
			Expect(err.Error()).To(ContainSubstring("BY_RECORDS"))
			Expect(err.Error()).To(ContainSubstring("BY_INDEX"))
		})

		It("is matchable and exposes fields", func() {
			inner := &PartlyBuiltError{IndexName: "foo", Message: "blocked"}
			wrapped := fmt.Errorf("build: %w", inner)
			var target *PartlyBuiltError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.IndexName).To(Equal("foo"))
		})
	})

	// Existence check error types (from existence_check.go)

	Describe("RecordAlreadyExistsError", func() {
		It("includes message and PrimaryKey", func() {
			err := &RecordAlreadyExistsError{Message: "dup", PrimaryKey: int64(42)}
			Expect(err.Error()).To(Equal("dup"))
		})

		It("is matchable and exposes PrimaryKey", func() {
			wrapped := fmt.Errorf("save: %w", &RecordAlreadyExistsError{PrimaryKey: "pk1"})
			var target *RecordAlreadyExistsError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.PrimaryKey).To(Equal("pk1"))
		})
	})

	Describe("RecordDoesNotExistError", func() {
		It("includes message", func() {
			err := &RecordDoesNotExistError{Message: "not found", PrimaryKey: int64(99)}
			Expect(err.Error()).To(Equal("not found"))
		})
	})

	Describe("RecordTypeChangedError", func() {
		It("includes message and type info", func() {
			err := &RecordTypeChangedError{
				Message:      "type changed",
				PrimaryKey:   int64(1),
				ActualType:   "Order",
				ExpectedType: "Customer",
			}
			Expect(err.Error()).To(Equal("type changed"))
		})

		It("is matchable and exposes type fields", func() {
			wrapped := fmt.Errorf("save: %w", &RecordTypeChangedError{ActualType: "A", ExpectedType: "B"})
			var target *RecordTypeChangedError
			Expect(errors.As(wrapped, &target)).To(BeTrue())
			Expect(target.ActualType).To(Equal("A"))
			Expect(target.ExpectedType).To(Equal("B"))
		})
	})

	// RecordExistenceCheck enum tests

	Describe("RecordExistenceCheck", func() {
		DescribeTable("ErrorIfExists",
			func(check RecordExistenceCheck, expected bool) {
				Expect(check.ErrorIfExists()).To(Equal(expected))
			},
			Entry("NONE", RecordExistenceCheckNone, false),
			Entry("ERROR_IF_EXISTS", RecordExistenceCheckErrorIfExists, true),
			Entry("ERROR_IF_NOT_EXISTS", RecordExistenceCheckErrorIfNotExists, false),
			Entry("ERROR_IF_TYPE_CHANGED", RecordExistenceCheckErrorIfTypeChanged, false),
			Entry("ERROR_IF_NOT_EXISTS_OR_TYPE_CHANGED", RecordExistenceCheckErrorIfNotExistsOrTypeChanged, false),
		)

		DescribeTable("ErrorIfNotExists",
			func(check RecordExistenceCheck, expected bool) {
				Expect(check.ErrorIfNotExists()).To(Equal(expected))
			},
			Entry("NONE", RecordExistenceCheckNone, false),
			Entry("ERROR_IF_EXISTS", RecordExistenceCheckErrorIfExists, false),
			Entry("ERROR_IF_NOT_EXISTS", RecordExistenceCheckErrorIfNotExists, true),
			Entry("ERROR_IF_TYPE_CHANGED", RecordExistenceCheckErrorIfTypeChanged, false),
			Entry("ERROR_IF_NOT_EXISTS_OR_TYPE_CHANGED", RecordExistenceCheckErrorIfNotExistsOrTypeChanged, true),
		)

		DescribeTable("ErrorIfTypeChanged",
			func(check RecordExistenceCheck, expected bool) {
				Expect(check.ErrorIfTypeChanged()).To(Equal(expected))
			},
			Entry("NONE", RecordExistenceCheckNone, false),
			Entry("ERROR_IF_EXISTS", RecordExistenceCheckErrorIfExists, false),
			Entry("ERROR_IF_NOT_EXISTS", RecordExistenceCheckErrorIfNotExists, false),
			Entry("ERROR_IF_TYPE_CHANGED", RecordExistenceCheckErrorIfTypeChanged, true),
			Entry("ERROR_IF_NOT_EXISTS_OR_TYPE_CHANGED", RecordExistenceCheckErrorIfNotExistsOrTypeChanged, true),
		)

		DescribeTable("String",
			func(check RecordExistenceCheck, expected string) {
				Expect(check.String()).To(Equal(expected))
			},
			Entry("NONE", RecordExistenceCheckNone, "NONE"),
			Entry("ERROR_IF_EXISTS", RecordExistenceCheckErrorIfExists, "ERROR_IF_EXISTS"),
			Entry("ERROR_IF_NOT_EXISTS", RecordExistenceCheckErrorIfNotExists, "ERROR_IF_NOT_EXISTS"),
			Entry("ERROR_IF_TYPE_CHANGED", RecordExistenceCheckErrorIfTypeChanged, "ERROR_IF_RECORD_TYPE_CHANGED"),
			Entry("ERROR_IF_NOT_EXISTS_OR_TYPE_CHANGED", RecordExistenceCheckErrorIfNotExistsOrTypeChanged, "ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED"),
			Entry("unknown", RecordExistenceCheck(99), "UNKNOWN"),
		)
	})
})
