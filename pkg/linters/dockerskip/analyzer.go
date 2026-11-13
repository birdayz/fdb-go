// Package dockerskip requires foundationdb.SkipWithoutDocker in a skipping `if err != nil`
// right after foundationdb.Run or RunCluster; an error checked later is out of reach.
package dockerskip

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// TestcontainersPkg is the FDB testcontainers package whose startup errors are checked.
const TestcontainersPkg = "fdb.dev/pkg/testcontainers/foundationdb"

var Analyzer = &analysis.Analyzer{
	Name: "dockerskip",
	Doc:  "requires foundationdb.SkipWithoutDocker in a branch that skips on an FDB testcontainer startup error",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BlockStmt:
				checkList(pass, n.List)
			case *ast.CaseClause:
				checkList(pass, n.Body)
			case *ast.CommClause:
				checkList(pass, n.Body)
			case *ast.IfStmt:
				if n.Init != nil {
					if errObj := startupErr(pass.TypesInfo, n.Init); errObj != nil {
						checkBranch(pass, errObj, n)
					}
				}
			}
			return true
		})
	}
	return nil, nil
}

// checkList checks each startup call in a statement list against the if
// statement right after it.
func checkList(pass *analysis.Pass, list []ast.Stmt) {
	for i := 0; i+1 < len(list); i++ {
		errObj := startupErr(pass.TypesInfo, list[i])
		if errObj == nil {
			continue
		}
		if ifStmt, ok := list[i+1].(*ast.IfStmt); ok && ifStmt.Init == nil {
			checkBranch(pass, errObj, ifStmt)
		}
	}
}

// startupErr returns the error variable a statement assigns from a
// foundationdb.Run or RunCluster call, or nil.
func startupErr(info *types.Info, stmt ast.Stmt) types.Object {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
		return nil
	}
	call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
	if !ok {
		return nil
	}
	fn := callee(info, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != TestcontainersPkg ||
		(fn.Name() != "Run" && fn.Name() != "RunCluster") || isMethod(fn) {
		return nil
	}
	id, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
	if !ok || id.Name == "_" {
		return nil
	}
	if obj := info.Defs[id]; obj != nil {
		return obj
	}
	return info.Uses[id]
}

// checkBranch reports ifStmt when it is `if err != nil` for errObj and its body
// skips without asking SkipWithoutDocker.
func checkBranch(pass *analysis.Pass, errObj types.Object, ifStmt *ast.IfStmt) {
	cond, ok := ast.Unparen(ifStmt.Cond).(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return
	}
	if !isIdentOf(pass.TypesInfo, cond.X, errObj) && !isIdentOf(pass.TypesInfo, cond.Y, errObj) {
		return
	}
	skips, asks := false, false
	ast.Inspect(ifStmt.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			fn := callee(pass.TypesInfo, n)
			switch {
			case isSkip(fn):
				skips = true
			case fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == TestcontainersPkg && fn.Name() == "SkipWithoutDocker":
				asks = true
			}
		case *ast.ReturnStmt:
			if len(n.Results) == 1 {
				if lit, ok := n.Results[0].(*ast.BasicLit); ok && lit.Kind == token.INT && lit.Value == "0" {
					skips = true
				}
			}
		}
		return true
	})
	if skips && !asks {
		pass.Reportf(ifStmt.Pos(), "this branch skips on an FDB testcontainer startup error without asking "+
			"foundationdb.SkipWithoutDocker; only a machine without Docker may skip")
	}
}

// isSkip reports whether fn is testing's Skip, Skipf, SkipNow or (*testing.M).Run.
func isSkip(fn *types.Func) bool {
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "testing" {
		return false
	}
	switch fn.Name() {
	case "Skip", "Skipf", "SkipNow":
		return isMethod(fn)
	case "Run":
		recv := fn.Type().(*types.Signature).Recv()
		if recv == nil {
			return false
		}
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := t.(*types.Named)
		return ok && named.Obj().Name() == "M"
	}
	return false
}

func isMethod(fn *types.Func) bool {
	return fn.Type().(*types.Signature).Recv() != nil
}

func callee(info *types.Info, call *ast.CallExpr) *types.Func {
	var id *ast.Ident
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

func isIdentOf(info *types.Info, e ast.Expr, obj types.Object) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	return ok && info.Uses[id] == obj
}
