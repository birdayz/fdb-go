package embedded

import "testing"

func TestPlanCacheText(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ sql, want string }{
		{"select * from foo", "SELECT * FROM FOO"},
		{"  select\t*\nfrom  foo -- c", "SELECT * FROM FOO"},
		{"SELECT /* x */ 'it''s', 'a  b' FROM foo", "SELECT 'it''s' , 'a  b' FROM FOO"},
		{`SELECT "aB", ab FROM foo`, `SELECT "aB" , AB FROM FOO`},
		{"SELECT '--x', B64'ywjj' FROM foo", "SELECT '--x' , B64'ywjj' FROM FOO"},
	} {
		if got := planCacheText(parseQuery(t, tt.sql)); got != tt.want {
			t.Errorf("planCacheText(%q) = %q, want %q", tt.sql, got, tt.want)
		}
	}
}
