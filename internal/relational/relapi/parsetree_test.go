package api

import "testing"

func TestQueryTypeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		q    QueryType
		want string
	}{
		{QueryTypeSelect, "SELECT"},
		{QueryTypeInsert, "INSERT"},
		{QueryTypeUpdate, "UPDATE"},
		{QueryTypeDelete, "DELETE"},
		{QueryTypeCreate, "CREATE"},
		{QueryType(99), "?"},
		{QueryType(-1), "?"},
	}
	for _, c := range cases {
		if got := c.q.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.q, got, c.want)
		}
	}
}

// Compile-time stubs pinning the interface shapes.

type parseTreeStub struct{ kind QueryType }

func (p *parseTreeStub) QueryType() QueryType { return p.kind }

type withMetaStub struct{ dt DataType }

func (w *withMetaStub) RelationalMetaData() (DataType, error) { return w.dt, nil }

var (
	_ ParseTreeInfo = (*parseTreeStub)(nil)
	_ WithMetadata  = (*withMetaStub)(nil)
)

func TestParseTreeInfoStub(t *testing.T) {
	t.Parallel()
	p := &parseTreeStub{kind: QueryTypeSelect}
	if p.QueryType() != QueryTypeSelect {
		t.Fatal("stub wiring broken")
	}
}

func TestWithMetadataStub(t *testing.T) {
	t.Parallel()
	w := &withMetaStub{dt: NewIntegerType(false)}
	got, err := w.RelationalMetaData()
	if err != nil {
		t.Fatalf("RelationalMetaData: %v", err)
	}
	if got.Code() != CodeInteger {
		t.Fatalf("unexpected type: %v", got)
	}
}
