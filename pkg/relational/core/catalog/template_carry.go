package catalog

import (
	"bytes"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// carryNumbering is a new version of a stored template as it is stored
// (RFC-257 WS-J section 4, "A NEW VERSION of a stored template is CARRIED from
// the latest stored version"): built, the new version's meta-data as the
// template builder produced it, rewritten so that nothing stored data depends on
// moves. stored is the latest stored version's MetaData as its row holds it
// (LoadTemplateProto), never passed through the loader, so a stored index's
// deprecated fields, unknown fields and extensions survive when it is carried.
//
//   - A record type stored keeps its record type key, its union field (number and
//     name) and its since-version; a new one takes the next key and union number
//     above the stored maxima, in the built order, and the new metadata version
//     as its since-version.
//   - An index is matched to the stored one of the same name
//     (recordlayer.ClassifyIndexCarry): EQUIVALENT is carried as the stored
//     Index message; CHANGED is the rebuilt one with the stored added version
//     (absent is 1) and subspace key and a last-modified version above the stored
//     metadata version, so a store opened under it rebuilds the index; a NEW
//     index takes added = last-modified above the stored metadata version. A NEW
//     index whose subspace key a carried former index holds is refused.
//   - A stored index the new version lacks becomes a former index (its name,
//     subspace key and added version, removed at the new metadata version);
//     stored former indexes are carried.
//   - The metadata version is the highest version assigned, and at least the
//     stored one + 1.
//
// defined names the indexes the new version defines, its NEW and CHANGED ones:
// the ones the lane check reads (checkIndexLanes).
func carryNumbering(stored, built *gen.MetaData) (carried *gen.MetaData, defined map[string]bool, err error) {
	out := proto.Clone(built).(*gen.MetaData)
	version := stored.GetVersion()

	storedTypes := map[string]*gen.RecordType{}
	var maxKey int64 = -1
	for _, rt := range stored.GetRecordTypes() {
		storedTypes[rt.GetName()] = rt
		if k, ok := explicitLongKey(rt); ok && k > maxKey {
			maxKey = k
		}
	}
	storedUnion := unionMessage(stored.GetRecords())
	outUnion := unionMessage(out.GetRecords())
	if storedUnion == nil || outUnion == nil {
		return nil, nil, api.NewError(api.ErrCodeInternalError, "template carry: a records file has no union message")
	}
	storedUnionField := map[string]*descriptorpb.FieldDescriptorProto{}
	var maxUnion int32
	for _, f := range storedUnion.GetField() {
		storedUnionField[unionTypeName(f)] = f
		if f.GetNumber() > maxUnion {
			maxUnion = f.GetNumber()
		}
	}

	// Indexes first: the new metadata version depends on them, and a new
	// record type's since-version is that version.
	storedIndexes := map[string]*gen.Index{}
	for _, idx := range stored.GetIndexes() {
		storedIndexes[idx.GetName()] = idx
	}
	formers := make([]*gen.FormerIndex, 0, len(stored.GetFormerIndexes()))
	for _, f := range stored.GetFormerIndexes() {
		formers = append(formers, proto.Clone(f).(*gen.FormerIndex))
	}
	kept := map[string]bool{}
	defined = map[string]bool{}
	for i, idx := range out.GetIndexes() {
		prior, ok := storedIndexes[idx.GetName()]
		if !ok {
			for _, f := range formers {
				if bytes.Equal(f.GetSubspaceKey(), idx.GetSubspaceKey()) {
					return nil, nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
						"index %s cannot be added: its name is the subspace key of index %s dropped at version %d; add it under another name",
						idx.GetName(), formerName(f), f.GetRemovedVersion())
				}
			}
			version++
			idx.AddedVersion = proto.Int32(version)
			idx.LastModifiedVersion = proto.Int32(version)
			defined[idx.GetName()] = true
			continue
		}
		kept[idx.GetName()] = true
		class, _, err := recordlayer.ClassifyIndexCarry(prior, idx)
		if err != nil {
			return nil, nil, err
		}
		if class == recordlayer.IndexEquivalent {
			out.Indexes[i] = proto.Clone(prior).(*gen.Index)
			continue
		}
		defined[idx.GetName()] = true
		version++
		added := int32(1)
		if prior.AddedVersion != nil {
			added = prior.GetAddedVersion()
		}
		idx.AddedVersion = proto.Int32(added)
		idx.SubspaceKey = append([]byte(nil), prior.GetSubspaceKey()...)
		idx.LastModifiedVersion = proto.Int32(version)
	}
	if version < stored.GetVersion()+1 {
		version = stored.GetVersion() + 1
	}
	for _, idx := range stored.GetIndexes() {
		if kept[idx.GetName()] {
			continue
		}
		f := &gen.FormerIndex{
			SubspaceKey:    append([]byte(nil), idx.GetSubspaceKey()...),
			RemovedVersion: proto.Int32(version),
			AddedVersion:   proto.Int32(1),
		}
		if idx.AddedVersion != nil {
			f.AddedVersion = proto.Int32(idx.GetAddedVersion())
		}
		if idx.GetName() != "" {
			f.FormerName = proto.String(idx.GetName())
		}
		formers = append(formers, f)
	}
	out.FormerIndexes = formers
	out.Version = proto.Int32(version)

	for _, rt := range out.GetRecordTypes() {
		prior, ok := storedTypes[rt.GetName()]
		if ok {
			rt.ExplicitKey = cloneValue(prior.GetExplicitKey())
			rt.SinceVersion = nil
			if prior.SinceVersion != nil {
				rt.SinceVersion = proto.Int32(prior.GetSinceVersion())
			}
			continue
		}
		if _, explicit := explicitLongKey(rt); explicit {
			maxKey++
			rt.ExplicitKey = &gen.Value{LongValue: proto.Int64(maxKey)}
		}
		rt.SinceVersion = proto.Int32(version)
	}
	for _, f := range outUnion.GetField() {
		name := unionTypeName(f)
		if prior, ok := storedUnionField[name]; ok {
			f.Number = proto.Int32(prior.GetNumber())
			f.Name = proto.String(prior.GetName())
			continue
		}
		maxUnion++
		f.Number = proto.Int32(maxUnion)
	}
	return out, defined, nil
}

// unionMessage is the records file's union message: the one whose (record)
// usage is UNION, else the one named RecordTypeUnion (Java's
// fetchUnionDescriptor).
func unionMessage(fdp *descriptorpb.FileDescriptorProto) *descriptorpb.DescriptorProto {
	var named *descriptorpb.DescriptorProto
	for _, m := range fdp.GetMessageType() {
		if opts := m.GetOptions(); opts != nil && proto.HasExtension(opts, gen.E_Record) {
			if rto, _ := proto.GetExtension(opts, gen.E_Record).(*gen.RecordTypeOptions); rto.GetUsage() == gen.RecordTypeOptions_UNION {
				return m
			}
		}
		if m.GetName() == "RecordTypeUnion" {
			named = m
		}
	}
	return named
}

// unionTypeName is the record type a union field holds: its message type's
// name, without a leading package.
func unionTypeName(f *descriptorpb.FieldDescriptorProto) string {
	name := f.GetTypeName()
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// explicitLongKey is a record type's explicit key when it is a long, the form
// the relational builder assigns (its table counter).
func explicitLongKey(rt *gen.RecordType) (int64, bool) {
	k := rt.GetExplicitKey()
	if k == nil || k.LongValue == nil {
		return 0, false
	}
	return k.GetLongValue(), true
}

func cloneValue(v *gen.Value) *gen.Value {
	if v == nil {
		return nil
	}
	return proto.Clone(v).(*gen.Value)
}

// formerName names a former index in a refusal: its name, else its subspace key.
func formerName(f *gen.FormerIndex) string {
	if f.GetFormerName() != "" {
		return f.GetFormerName()
	}
	if t, err := tuple.Unpack(f.GetSubspaceKey()); err == nil {
		return fmt.Sprint(t)
	}
	return fmt.Sprintf("%x", f.GetSubspaceKey())
}

// refuseBelowLatest is CreateTemplate's refusal of a version at or below the
// latest stored one (INVALID_SCHEMA_TEMPLATE, a declared Go extension: Java's
// createTemplate refuses only an exact duplicate) and its relational evolution
// check against that latest version, both moved here from the save action so
// every build-path writer runs them (ws-j-design.md section 4, 9 (w)).
func refuseBelowLatest(latest, newTemplate api.SchemaTemplate) error {
	if newTemplate.Version() <= latest.Version() {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"template %q: new version %d must be greater than current version %d",
			newTemplate.MetadataName(), newTemplate.Version(), latest.Version())
	}
	return NewRelationalSchemaEvolutionValidator().Validate(latest, newTemplate)
}

// carryTemplate is a new version of a stored template as CreateTemplate stores
// it: the built meta-data carried from stored, the latest stored version's
// MetaData (carryNumbering), marshalled, and read back from exactly those bytes
// by deserializeTemplate, the path every load takes, so Go never stores a
// template no Go session can load and what is validated is what is stored. The
// lane check then reads the indexes the new version defines (NEW and CHANGED),
// and the evolution validator checks the carried meta-data against the stored
// one with index rebuilds allowed: a CHANGED or NEW index is rebuilt when a
// store opens under the new version (ws-j-design.md section 4).
func carryTemplate(stored *gen.MetaData, rl *metadata.RecordLayerSchemaTemplate) ([]byte, api.SchemaTemplate, error) {
	built, err := rl.Underlying().ToProto()
	if err != nil {
		return nil, nil, api.WrapErrorf(err, api.ErrCodeInternalError, "template to-proto")
	}
	carried, defined, err := carryNumbering(stored, built)
	if err != nil {
		return nil, nil, err
	}
	payload, err := proto.Marshal(carried)
	if err != nil {
		return nil, nil, api.WrapErrorf(err, api.ErrCodeInternalError, "template marshal")
	}
	tmpl, err := deserializeTemplate(&gen.Templates{
		TEMPLATE_NAME:    proto.String(rl.MetadataName()),
		TEMPLATE_VERSION: proto.Int32(int32(rl.Version())),
		META_DATA:        payload,
	})
	if err != nil {
		return nil, nil, err
	}
	newMD := tmpl.(*metadata.RecordLayerSchemaTemplate).Underlying()
	if err := checkIndexLanes(newMD, carried, defined); err != nil {
		return nil, nil, err
	}
	oldMD, err := recordlayer.RecordMetaDataFromProto(proto.Clone(stored).(*gen.MetaData))
	if err != nil {
		return nil, nil, api.WrapErrorf(err, api.ErrCodeInternalError, "stored template %s from-proto", rl.MetadataName())
	}
	validator := recordlayer.NewMetaDataEvolutionValidator().SetAllowIndexRebuilds(true).Build()
	if verr := validator.Validate(oldMD, newMD); verr != nil {
		return nil, nil, api.WrapErrorf(verr, api.ErrCodeInvalidSchemaTemplate,
			"schema template %s version %d: metadata evolution rejected", rl.MetadataName(), rl.Version())
	}
	return payload, tmpl, nil
}

// checkFreshLanes is the lane check over a template of a fresh name, whose
// every index the save defines.
func checkFreshLanes(rl *metadata.RecordLayerSchemaTemplate) error {
	p, err := rl.Underlying().ToProto()
	if err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "template to-proto")
	}
	return checkIndexLanes(rl.Underlying(), p, nil)
}
