package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/protoname"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// DRIVES EVERY ARM OF THE LABEL'S QUALIFIER DECISION, including the two it does
// NOT get right — those are declared limits in qualifierStrippedLabel's doc, and
// a limit stated in prose and nowhere else is the failure that survives every
// other check. Each is a row here, so the day one is closed this file goes red
// and says so rather than the claim quietly becoming true.
//
// The corpus reading is not a substitute: it exercises the arms the corpus
// happens to reach, and the collision arm below needs two tables whose columns
// are named to collide, which no scenario has a reason to build.
func TestQualifierStrippedLabel(t *testing.T) {
	t.Parallel()

	// table builds one descriptor whose fields carry the given SQL names,
	// escaped the way the DDL emitter escapes them, so a name containing a dot
	// is stored as `a__2b` and has to come back through ToUserIdentifier.
	table := func(t *testing.T, msgName string, columns ...string) protoreflect.MessageDescriptor {
		t.Helper()
		msg := &descriptorpb.DescriptorProto{Name: proto.String(msgName)}
		for i, col := range columns {
			protoName, err := protoname.ToProtoBufCompliantName(col)
			if err != nil {
				t.Fatalf("escaping %q: %v", col, err)
			}
			msg.Field = append(msg.Field, &descriptorpb.FieldDescriptorProto{
				Name:   proto.String(protoName),
				Number: proto.Int32(int32(i + 1)),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			})
		}
		file := &descriptorpb.FileDescriptorProto{
			Name:        proto.String("qsl_" + msgName + ".proto"),
			Package:     proto.String("qsl"),
			Syntax:      proto.String("proto2"),
			MessageType: []*descriptorpb.DescriptorProto{msg},
		}
		fd, err := protodesc.NewFile(file, nil)
		if err != nil {
			t.Fatalf("NewFile(%s): %v", msgName, err)
		}
		return fd.Messages().Get(0)
	}

	dott := table(t, "DOTT", "a.b", "PLAIN")
	tt := table(t, "T", "A")
	xx := table(t, "X", "T.A")
	other := table(t, "OTHER", "b")
	xprobe := table(t, "X_PROBE", "id", "TOTAL", "X.TOTAL")

	for _, tc := range []struct {
		name  string
		flat  string
		descs []protoreflect.MessageDescriptor
		want  string
		why   string
	}{
		{
			name:  "declared name carrying its own dot keeps it",
			flat:  "a.b",
			descs: []protoreflect.MessageDescriptor{dott},
			want:  "a.b",
			why: "the shape the whole fix is about. Java reports `a.b`; the last-dot " +
				"split reported `b`, a name no engine calls that column",
		},
		{
			name:  "a real qualifier still comes off",
			flat:  "T.A",
			descs: []protoreflect.MessageDescriptor{tt},
			want:  "A",
			why:   "the ordinary case, and the one a fix for the row above must not break",
		},
		{
			name:  "qualifier that collides with another table's declared column",
			flat:  "T.A",
			descs: []protoreflect.MessageDescriptor{tt, xx},
			want:  "A",
			why: "`T.A` IS a declared column — of X — so asking only whether the whole " +
				"name is declared kept the qualifier here and labelled the column `T.A`. " +
				"The second fact separates them: `A` is declared by T, so the dot is a " +
				"qualifier after all",
		},
		{
			name:  "collision INSIDE one table, not across two",
			flat:  "X.TOTAL",
			descs: []protoreflect.MessageDescriptor{xprobe},
			want:  "TOTAL",
			why: "the same collision as the row above with both halves declared by ONE " +
				"descriptor — `SELECT s.id, (SELECT x.total FROM X_PROBE x …)` where " +
				"X_PROBE also declares a column literally named `\"X.TOTAL\"`. Reported " +
				"as a label of `X.TOTAL` where the base gives `TOTAL`, which is a " +
				"machinery qualifier leaking into user-visible metadata. It is a " +
				"separate row because a per-descriptor scan closes the two-table form " +
				"and leaves this one",
		},
		{
			name:  "derived aggregate keeps its own parenthesised dots",
			flat:  "X.SUM(X.Amount)",
			descs: []protoreflect.MessageDescriptor{tt},
			want:  "SUM(X.Amount)",
			why: "the candidate dot is the last at paren DEPTH ZERO. A plain last-dot " +
				"scan splits inside the call and yields `Amount)`",
		},
		{
			name:  "unqualified name is returned untouched",
			flat:  "PLAIN",
			descs: []protoreflect.MessageDescriptor{dott},
			want:  "PLAIN",
			why:   "no candidate dot, so no decision to make",
		},
		{
			name:  "name no descriptor declares splits, as before",
			flat:  "Q.SOMETHING",
			descs: []protoreflect.MessageDescriptor{dott},
			want:  "SOMETHING",
			why: "a derived or CTE output column is declared by no descriptor in scope, " +
				"so it falls through to the split — a stated limit, not an accident",
		},
		{
			// A DECLARED LIMIT. Both schema facts are true at once and the
			// decision goes the wrong way.
			name:  "LIMIT: dotted column beside a table declaring its tail",
			flat:  "a.b",
			descs: []protoreflect.MessageDescriptor{dott, other},
			want:  "b",
			why: "`a.b` is declared by DOTT and `b` is declared by OTHER, so the second " +
				"fact reads this real column as a qualified reference. Java would say " +
				"`a.b`. This is the residual of the collision two rows up — narrowed, " +
				"not closed — and closing it needs the reference's own source alias, " +
				"which is `_current` by the time labels are derived",
		},
		{
			// A DECLARED LIMIT. Nested members are not top-level fields.
			name:  "LIMIT: nested field declared with a dot is invisible",
			flat:  "s.k",
			descs: []protoreflect.MessageDescriptor{dott},
			want:  "k",
			why: "Fields() is top-level only, so a struct member declared `\"s.k\"` is not " +
				"found and its label splits. Same class as the row above and the same fix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := qualifierStrippedLabel(tc.flat, tc.descs); got != tc.want {
				t.Errorf("qualifierStrippedLabel(%q) = %q, want %q\n  %s",
					tc.flat, got, tc.want, tc.why)
			}
		})
	}
}
