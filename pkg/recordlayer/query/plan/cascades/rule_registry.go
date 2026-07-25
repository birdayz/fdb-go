package cascades

import (
	"fmt"
	"sort"
	"sync"
)

// typeNameForRegistry returns the Go-formatted type name for a rule —
// package-qualified and pointer-prefixed, e.g. "*cascades.FilterMergeRule".
// That spelling is not itself a registry key: default_rules.go reaches this
// helper only through shortTypeName, which strips the prefixes down to the
// simple name ("FilterMergeRule") the registry and DisabledRules use. Kept
// here so the helper lives next to the registry it serves. Accepts any rule
// kind — ExpressionRule and ImplementationRule are both named this way.
func typeNameForRegistry(r any) string {
	return fmt.Sprintf("%T", r)
}

// ruleRegistry is a name→ExpressionRule lookup for diagnostic and
// debugging use. Tests + the planner driver iterate the registry to
// produce names ('FilterMergeRule', 'NoOpFilterRule', etc.) without
// hardcoding the type-switch list — useful for explain-output and
// rule-firing trace logs.
//
// Initially empty. Rules opt in via RegisterRule at package init.
// Concurrent-safe via mutex.
type ruleRegistry struct {
	mu      sync.Mutex
	entries map[string]ExpressionRule
}

// newRuleRegistry returns an empty, independent registry. The package
// global below is one instance; tests that want to exercise registry
// mechanics take their own so registration is scoped to the test and
// leaves no residue in the process — a permanent global mutation makes
// a second in-process run of the same test collide with the first.
func newRuleRegistry() *ruleRegistry {
	return &ruleRegistry{entries: map[string]ExpressionRule{}}
}

var defaultRuleRegistry = newRuleRegistry()

// register is the instance-scoped form of RegisterRule; see that
// function for the contract.
func (reg *ruleRegistry) register(name string, r ExpressionRule) ExpressionRule {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, exists := reg.entries[name]; exists {
		panic(fmt.Sprintf("RegisterRule: duplicate name %q", name))
	}
	reg.entries[name] = r
	return r
}

// lookup is the instance-scoped form of LookupRule.
func (reg *ruleRegistry) lookup(name string) ExpressionRule {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.entries[name]
}

// names is the instance-scoped form of RegisteredRuleNames.
func (reg *ruleRegistry) names() []string {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	names := make([]string, 0, len(reg.entries))
	for n := range reg.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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
	return defaultRuleRegistry.register(name, r)
}

// LookupRule returns the rule registered under `name`, or nil if
// not found.
func LookupRule(name string) ExpressionRule {
	return defaultRuleRegistry.lookup(name)
}

// RegisteredRuleNames returns a sorted list of registered rule
// names. Useful for tests that want to iterate the registry
// deterministically.
func RegisteredRuleNames() []string {
	return defaultRuleRegistry.names()
}
