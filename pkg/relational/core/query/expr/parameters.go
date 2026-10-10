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

type specializableConstant struct {
	literal values.Value
	pin     func()
}

// specializableConstants maps a pool reference to its literal; a rewrite that
// reads the value pins it, as Java constrains a compile-time evaluation.
var specializableConstants sync.Map // *values.ConstantObjectValue -> specializableConstant

// BindSpecializableParameter binds tok to ref, whose value is literal.
func BindSpecializableParameter(tok antlr.Token, ref *values.ConstantObjectValue, literal values.Value, pin func()) {
	specializableConstants.Store(ref, specializableConstant{literal: literal, pin: pin})
	boundParameters.Store(tok, ref)
}

// ForgetSpecializable drops ref once its statement is planned.
func ForgetSpecializable(ref *values.ConstantObjectValue) { specializableConstants.Delete(ref) }

// specialize returns v's literal, pinning it, when v is a statement-pool reference.
func specialize(v values.Value) (values.Value, bool) {
	ref, ok := v.(*values.ConstantObjectValue)
	if !ok {
		return v, false
	}
	entry, ok := specializableConstants.Load(ref)
	if !ok {
		return v, false
	}
	c := entry.(specializableConstant)
	c.pin()
	return c.literal, true
}

// specializeCrossTypeComparands pins numeric cross-type comparands, which the
// SARG coercions rewrite from their value; STRING-to-ENUM/UUID promotes at run time.
func specializeCrossTypeComparands(left, right values.Value) (values.Value, values.Value) {
	lt, rt := left.Type(), right.Type()
	if lt == nil || rt == nil || lt.Code() == rt.Code() || sharesIntegerWireEncoding(lt.Code(), rt.Code()) ||
		!isNumericTypeCode(lt.Code()) || !isNumericTypeCode(rt.Code()) {
		return left, right
	}
	left, _ = specialize(left)
	right, _ = specialize(right)
	return left, right
}
