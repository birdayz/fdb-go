package plandiff

import "testing"

func TestIsAnonymousColumnName(t *testing.T) {
	t.Parallel()
	anon := []string{"_0", "_1", "_42"}
	named := []string{"", "_", "_x", "COUNT(*)", "NAME", "_0a", "X_0", "0"}
	for _, s := range anon {
		if !IsAnonymousColumnName(s) {
			t.Errorf("IsAnonymousColumnName(%q) = false, want true", s)
		}
	}
	for _, s := range named {
		if IsAnonymousColumnName(s) {
			t.Errorf("IsAnonymousColumnName(%q) = true, want false", s)
		}
	}
}

func TestConformColumns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		goCols  []Column
		javaCol []Column
		wantOK  bool
	}{
		{
			name:    "exact match",
			goCols:  []Column{{Name: "NAME", Type: "STRING"}},
			javaCol: []Column{{Name: "NAME", Type: "STRING"}},
			wantOK:  true,
		},
		{
			name:    "anonymous java label accepts descriptive go label",
			goCols:  []Column{{Name: "COUNT(*)", Type: "BIGINT"}},
			javaCol: []Column{{Name: "_0", Type: "BIGINT"}},
			wantOK:  true,
		},
		{
			name:    "anonymous java label still asserts type",
			goCols:  []Column{{Name: "COUNT(*)", Type: "DOUBLE"}},
			javaCol: []Column{{Name: "_0", Type: "BIGINT"}},
			wantOK:  false,
		},
		{
			name:    "anonymous java label rejects empty go label",
			goCols:  []Column{{Name: "", Type: "BIGINT"}},
			javaCol: []Column{{Name: "_0", Type: "BIGINT"}},
			wantOK:  false,
		},
		{
			name:    "named column name mismatch fails",
			goCols:  []Column{{Name: "COUNT(*)", Type: "BIGINT"}},
			javaCol: []Column{{Name: "CNT", Type: "BIGINT"}},
			wantOK:  false,
		},
		{
			name:    "type mismatch on named column fails",
			goCols:  []Column{{Name: "X", Type: "BIGINT"}},
			javaCol: []Column{{Name: "X", Type: "DOUBLE"}},
			wantOK:  false,
		},
		{
			name:    "arity mismatch fails",
			goCols:  []Column{{Name: "A", Type: "BIGINT"}},
			javaCol: []Column{{Name: "A", Type: "BIGINT"}, {Name: "B", Type: "BIGINT"}},
			wantOK:  false,
		},
		{
			name:    "mixed named + anonymous",
			goCols:  []Column{{Name: "ID", Type: "BIGINT"}, {Name: "SUM(V)", Type: "BIGINT"}},
			javaCol: []Column{{Name: "ID", Type: "BIGINT"}, {Name: "_1", Type: "BIGINT"}},
			wantOK:  true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			detail, ok := ConformColumns(tc.goCols, tc.javaCol)
			if ok != tc.wantOK {
				t.Errorf("ConformColumns ok=%v want=%v (detail=%q)", ok, tc.wantOK, detail)
			}
			if !ok && detail == "" {
				t.Errorf("ConformColumns returned ok=false but empty detail")
			}
		})
	}
}
