package query

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// protoFieldLookup TAKES A SQL NAME AND SEARCHES STORAGE NAMES, and those differ
// on TWO INDEPENDENT AXES — case, and protobuf escaping. It has to try both, and
// try them in combination, or a valid column reads as undefined.
//
// The escaping axis was missing entirely: a column declared `"a.b"` is stored as
// `a__2b`, comes back through ToUserIdentifier everywhere a user sees it, and
// arrived here spelled `a.b` — matching neither the exact nor the
// case-insensitive attempt. That was the third and last site in a chain that
// made `SELECT x FROM (SELECT "a.b" AS z FROM dottarr AS a) d, d.z AS x` fail;
// the first two were qualifier heuristics, and this one was looking in the
// wrong ALPHABET rather than at the wrong structure.
//
// The first fix of it added the escaped attempt EXACT-ONLY, which is the same
// omission one axis over: a hand-written lowercase proto exposing `a__0b` as SQL
// `a__b` needs escape AND fold together, because an unquoted reference arrives
// folded. Both orders are arms below.
//
// ORDER IS PART OF THE CONTRACT, not an implementation detail. Escaping runs
// only after both unescaped attempts miss, so an ordinary name — the
// overwhelming majority — never pays for it.
func TestProtoFieldLookupTriesCaseAndEscapingTogether(t *testing.T) {
	t.Parallel()

	// fields builds a descriptor whose fields carry the given STORAGE names
	// verbatim, so a test can state exactly what the proto holds.
	fields := func(t *testing.T, storageNames ...string) protoreflect.FieldDescriptors {
		t.Helper()
		msg := &descriptorpb.DescriptorProto{Name: proto.String("M")}
		for i, n := range storageNames {
			msg.Field = append(msg.Field, &descriptorpb.FieldDescriptorProto{
				Name:   proto.String(n),
				Number: proto.Int32(int32(i + 1)),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			})
		}
		file := &descriptorpb.FileDescriptorProto{
			Name:        proto.String("pfl.proto"),
			Package:     proto.String("pfl"),
			Syntax:      proto.String("proto2"),
			MessageType: []*descriptorpb.DescriptorProto{msg},
		}
		fd, err := protodesc.NewFile(file, nil)
		if err != nil {
			t.Fatalf("NewFile: %v", err)
		}
		return fd.Messages().Get(0).Fields()
	}

	for _, tc := range []struct {
		name    string
		storage []string
		lookup  string
		want    string // "" means the lookup must MISS
		why     string
	}{
		{
			name:    "exact",
			storage: []string{"ID"},
			lookup:  "ID",
			want:    "ID",
			why:     "the common path, and the one that must stay first",
		},
		{
			name:    "case-insensitive over a hand-written proto",
			storage: []string{"order_id"},
			lookup:  "ORDER_ID",
			want:    "order_id",
			why: "a hand-written .proto never went through DDL normalization, so its " +
				"names are lower/snake while an unquoted SQL reference arrives folded",
		},
		{
			name:    "escaped exact",
			storage: []string{"a__2b"},
			lookup:  "a.b",
			want:    "a__2b",
			why: "the defect this branch exists for. `\"a.b\"` is stored escaped; the SQL " +
				"name reaches here unescaped and matches nothing unescaped",
		},
		{
			name:    "escaped AND folded",
			storage: []string{"a__0b"},
			lookup:  "A__B",
			want:    "a__0b",
			why: "the two axes at once. `a__0b` is SQL `a__b`; unquoted it arrives `A__B` " +
				"and escapes to `A__0B`, which only the fold finds. An exact-only " +
				"escaped attempt reports a valid array field as an undefined column",
		},
		{
			name:    "a name that matches nothing still misses",
			storage: []string{"a__2b"},
			lookup:  "nope",
			want:    "",
			why: "the negative control. Without it every arm above could pass on a " +
				"lookup that returns the first field regardless",
		},
		{
			name:    "escaping does not invent a match",
			storage: []string{"plain"},
			lookup:  "other.name",
			want:    "",
			why: "a dotted name whose escaped form is absent must still miss — the " +
				"escape is a translation, not a wildcard",
		},
		{
			name:    "the escaping is NON-INJECTIVE and encoding the query name is unsound",
			storage: []string{"___1__2foo"},
			lookup:  "___1.FOO",
			want:    "",
			why: "`___1__2foo` decodes to the SQL name `_$.foo`, so `___1.FOO` does NOT " +
				"name it. An implementation that ESCAPES the query name gets " +
				"`___1__2FOO` and EqualFolds this field — accepting a column the " +
				"identifier never named, which the unnest path would then explode as an " +
				"untyped fallback rather than reject. Comparing DECODED storage names " +
				"has no such collision, and this arm is why the direction is not a " +
				"stylistic choice",
		},
		{
			name:    "an EXACT decoded match wins over a folded sibling",
			storage: []string{"a__2b", "A__2B"},
			lookup:  "A.B",
			want:    "A__2B",
			why: "`A__2B` decodes to `A.B` exactly while `a__2b` only folds to it. " +
				"Strict-then-relaxed, the same order Scope.ResolveColumn uses: an exact " +
				"answer is never made ambiguous by the existence of a case variant",
		},
		{
			name:    "the exact match wins from BEHIND two folded candidates",
			storage: []string{"a__2b", "A__2b", "A__2B"},
			lookup:  "A.B",
			want:    "A__2B",
			why: "THE ARM THAT FORCED TWO PASSES. A single pass that decides ambiguity " +
				"as it goes meets the two folded candidates first, declines, and never " +
				"reaches the exact one — so a valid field resolves or not depending on " +
				"DESCRIPTOR ORDER, which is not a property of the query. The row above " +
				"passes under either implementation; only this one separates them",
		},
		{
			name:    "two fields decoding to one EXACT spelling are declined",
			storage: []string{"foo___0bar", "foo__0_bar"},
			lookup:  "foo___bar",
			want:    "",
			why: "DECODING IS NOT INJECTIVE EITHER, which is easy to assume once the " +
				"encode direction has been rejected for that reason. `___0` and `__0_` " +
				"both decode to `___`, so both storage names answer EXACTLY to " +
				"`foo___bar`. A first draft checked ambiguity only in the folded pass " +
				"and would have returned whichever field the descriptor listed first",
		},
		{
			name:    "two fields folding to one spelling, with no exact match, are declined",
			storage: []string{"a__2b", "A__2b"},
			lookup:  "A.B",
			want:    "",
			why: "neither decodes to `A.B` exactly and both fold to it, so the SQL name " +
				"is genuinely ambiguous. Returning the first is the worst available " +
				"answer — a silent bind to whichever the descriptor happens to list " +
				"first, which is not a property of the query",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := protoFieldLookup(fields(t, tc.storage...), tc.lookup)
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("protoFieldLookup(%q) found %q, want a MISS\n  %s",
					tc.lookup, got.Name(), tc.why)
			case tc.want != "" && got == nil:
				t.Errorf("protoFieldLookup(%q) missed, want %q\n  %s",
					tc.lookup, tc.want, tc.why)
			case tc.want != "" && string(got.Name()) != tc.want:
				t.Errorf("protoFieldLookup(%q) = %q, want %q\n  %s",
					tc.lookup, got.Name(), tc.want, tc.why)
			}
		})
	}
}
