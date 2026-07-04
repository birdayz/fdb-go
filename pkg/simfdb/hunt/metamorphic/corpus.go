package metamorphic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SeedCorpus is the hand-written metamorphic relations — equivalences that hold under SQL
// three-valued logic regardless of NULLs, so a correct engine returns zero violations. It gives
// the oracle immediate teeth and a template for what the LLM generator emits. Keep only relations
// you are certain are equivalent; a false relation here would be a spurious "bug".
func SeedCorpus() []Scenario {
	base := Scenario{
		Name:   "rewrites",
		Seed:   1,
		Tables: []string{"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))"},
		Data: []string{
			"INSERT INTO t (id, a, b) VALUES (1, 1, 10)",
			"INSERT INTO t (id, a, b) VALUES (2, 2, 20)",
			"INSERT INTO t (id, a, b) VALUES (3, 3, 5)",
			"INSERT INTO t (id, a, b) VALUES (4, 2, 200)",
			"INSERT INTO t (id, a, b) VALUES (5, NULL, 30)",
			"INSERT INTO t (id, a, b) VALUES (6, 3, NULL)",
		},
		Groups: []Group{
			{
				Name: "in-vs-or", Reason: "a IN (1,2,3) ≡ a=1 OR a=2 OR a=3 (NULL-safe both sides)",
				Queries: []string{
					"SELECT id FROM t WHERE a IN (1, 2, 3)",
					"SELECT id FROM t WHERE a = 1 OR a = 2 OR a = 3",
				},
			},
			{
				Name: "and-commute", Reason: "AND is commutative",
				Queries: []string{
					"SELECT id FROM t WHERE a > 1 AND b < 100",
					"SELECT id FROM t WHERE b < 100 AND a > 1",
				},
			},
			{
				Name: "or-commute", Reason: "OR is commutative",
				Queries: []string{
					"SELECT id FROM t WHERE a = 1 OR b = 20",
					"SELECT id FROM t WHERE b = 20 OR a = 1",
				},
			},
			{
				Name: "redundant-true", Reason: "p AND (1=1) ≡ p",
				Queries: []string{
					"SELECT id FROM t WHERE a > 1",
					"SELECT id FROM t WHERE a > 1 AND (1 = 1)",
				},
			},
			{
				Name: "between-vs-range", Reason: "x BETWEEN lo AND hi ≡ x >= lo AND x <= hi",
				Queries: []string{
					"SELECT id FROM t WHERE b BETWEEN 10 AND 30",
					"SELECT id FROM t WHERE b >= 10 AND b <= 30",
				},
			},
			{
				Name: "orderby-default-asc", Reason: "ORDER BY id ≡ ORDER BY id ASC", Ordered: true,
				Queries: []string{
					"SELECT id FROM t ORDER BY id",
					"SELECT id FROM t ORDER BY id ASC",
				},
			},
		},
	}
	return []Scenario{base}
}

// LoadDir reads scenarios from every *.json file in dir (each file is one Scenario object or an
// array of them) — the entry point for LLM-generated corpora.
func LoadDir(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		got, err := parseScenarios(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, got...)
	}
	return out, nil
}

// parseScenarios accepts a single Scenario object or a JSON array of them.
func parseScenarios(b []byte) ([]Scenario, error) {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && t[0] == '[' {
		var many []Scenario
		if err := json.Unmarshal(t, &many); err != nil {
			return nil, err
		}
		return many, nil
	}
	var one Scenario
	if err := json.Unmarshal(t, &one); err != nil {
		return nil, err
	}
	return []Scenario{one}, nil
}
