package cascades

import (
	"fmt"
	"sort"
	"sync"
)

// typeNameForRegistry returns the Go-formatted type name for a rule
// (e.g. "*cascades.FilterMergeRule"). Used by default_rules.go's init
// to derive registry keys; kept here so the helper lives next to the
// registry it serves.
func typeNameForRegistry(r ExpressionRule) string {
	return fmt.Sprintf("%T", r)
}

// ruleRegistry is a name→ExpressionRule lookup for diagnostic and
// debugging use. Tests + the planner driver iterate the registry to
// produce names ('FilterMergeRule', 'NoOpFilterRule', etc.) without
// hardcoding the type-switch list — useful for explain-output and
// rule-firing trace logs.
//
// Initially empty. Rules opt in via RegisterRule at package init or
// test setup. Concurrent-safe via mutex.
type ruleRegistry struct {
	mu      sync.Mutex
	entries map[string]ExpressionRule
}

var defaultRuleRegistry = &ruleRegistry{
	entries: map[string]ExpressionRule{},
}

// RegisterRule adds the rule to the package-level registry under
// `name`. Returns the rule unchanged so callers can inline the
// register call:
//
//	var myRule = RegisterRule("MyRule", &MyRule{...})
//
// Panics on duplicate name — registry collision is a programmer
// error, not runtime data.
func RegisterRule(name string, r ExpressionRule) ExpressionRule {
	defaultRuleRegistry.mu.Lock()
	defer defaultRuleRegistry.mu.Unlock()
	if _, exists := defaultRuleRegistry.entries[name]; exists {
		panic(fmt.Sprintf("RegisterRule: duplicate name %q", name))
	}
	defaultRuleRegistry.entries[name] = r
	return r
}

// LookupRule returns the rule registered under `name`, or nil if
// not found.
func LookupRule(name string) ExpressionRule {
	defaultRuleRegistry.mu.Lock()
	defer defaultRuleRegistry.mu.Unlock()
	return defaultRuleRegistry.entries[name]
}

// RegisteredRuleNames returns a sorted list of registered rule
// names. Useful for tests that want to iterate the registry
// deterministically.
func RegisteredRuleNames() []string {
	defaultRuleRegistry.mu.Lock()
	defer defaultRuleRegistry.mu.Unlock()
	names := make([]string, 0, len(defaultRuleRegistry.entries))
	for n := range defaultRuleRegistry.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
