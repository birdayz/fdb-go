package expr

import (
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"github.com/antlr4-go/antlr/v4"
)

// boundParameters maps a prepared-statement parameter's token to the typed
// constant bound to it for the statement being planned. Tokens are unique per
// parse, so concurrent statements never see each other's bindings.
var boundParameters sync.Map // antlr.Token -> values.Value

// BindParameter binds the parameter whose token is tok to v until
// UnbindParameter.
func BindParameter(tok antlr.Token, v values.Value) { boundParameters.Store(tok, v) }

// UnbindParameter removes tok's binding.
func UnbindParameter(tok antlr.Token) { boundParameters.Delete(tok) }

// BoundParameter returns the constant bound to tok.
func BoundParameter(tok antlr.Token) (values.Value, bool) {
	v, ok := boundParameters.Load(tok)
	if !ok {
		return nil, false
	}
	return v.(values.Value), true
}
