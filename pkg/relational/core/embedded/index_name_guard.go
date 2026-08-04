package embedded

import (
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// rejectReservedIndexName rejects a user-supplied index name that ends with the
// RFC-209 companion suffix.
//
// The DDL derives a group-existence companion's name by appending that suffix
// to the owning aggregate index's name (RFC-209 §5.2). Without the reservation,
// create-if-absent could collide with a pre-existing user index carrying the
// derived name but a different structure, and the user would meet a confusing
// duplicate-name error at schema-creation time rather than at the point they
// chose the name.
//
// This narrows the DDL surface relative to Java, which accepts any name. The
// restriction is deliberate and applies at CREATE ONLY, never at load: a schema
// that already contains such a name — written by Java, or by an older Go binary
// — opens and reads normally, because companion matching is structural and the
// name carries no semantic weight. Rejecting at load would let a read-side
// extension render an existing Java-written schema unreadable.
func rejectReservedIndexName(indexName string) error {
	if !strings.HasSuffix(strings.ToUpper(indexName), recordlayer.GroupCountCompanionSuffix) {
		return nil
	}
	return api.NewErrorf(api.ErrCodeInvalidName,
		"index name %q is reserved: the suffix %q is used for generated group-existence "+
			"companion indexes", indexName, recordlayer.GroupCountCompanionSuffix)
}
