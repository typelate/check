package asteval

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strconv"
)

// ResolveStringExpr attempts to statically resolve an AST expression to a
// string value. It handles string literals, named constants, and simple
// variable assignments (where the variable is defined once and never
// reassigned).
func ResolveStringExpr(info *types.Info, files []*ast.File, expr ast.Expr) (string, bool) {
	return resolveStringExpr(info, files, expr, 0)
}

const maxResolveDepth = 10

func resolveStringExpr(info *types.Info, files []*ast.File, expr ast.Expr, depth int) (string, bool) {
	if depth > maxResolveDepth {
		return "", false
	}

	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return s, true

	case *ast.Ident:
		if info == nil {
			return "", false
		}
		obj := info.Uses[e]
		if obj == nil {
			return "", false
		}

		// Named constant: extract the compile-time value.
		if c, ok := obj.(*types.Const); ok {
			if c.Val().Kind() != constant.String {
				return "", false
			}
			return constant.StringVal(c.Val()), true
		}

		// Variable: trace back to the defining assignment.
		if v, ok := obj.(*types.Var); ok {
			rhs, ok := FindDefiningValue(info, v, files)
			if !ok {
				return "", false
			}
			// Verify the variable is not reassigned after definition.
			if isReassigned(info, v, files) {
				return "", false
			}
			return resolveStringExpr(info, files, rhs, depth+1)
		}

	case *ast.ParenExpr:
		return resolveStringExpr(info, files, e.X, depth+1)
	}

	return "", false
}

// FindDefiningValue locates the RHS expression from the defining assignment
// of the given variable (either := or var declarations).
func FindDefiningValue(info *types.Info, v *types.Var, files []*ast.File) (ast.Expr, bool) {
	// Find the defining *ast.Ident for this variable.
	var defIdent *ast.Ident
	for ident, obj := range info.Defs {
		if obj == v {
			defIdent = ident
			break
		}
	}
	if defIdent == nil {
		return nil, false
	}

	// Walk the AST to find the statement containing this definition.
	var result ast.Expr
	var found bool

	for _, file := range files {
		if found {
			break
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if found {
				return false
			}
			switch n := node.(type) {
			case *ast.AssignStmt:
				if n.Tok != token.DEFINE {
					return true
				}
				for i, lhs := range n.Lhs {
					ident, ok := lhs.(*ast.Ident)
					if !ok || ident != defIdent {
						continue
					}
					if i < len(n.Rhs) {
						result = n.Rhs[i]
						found = true
						return false
					}
				}
			case *ast.ValueSpec:
				for i, ident := range n.Names {
					if ident != defIdent {
						continue
					}
					if i < len(n.Values) {
						result = n.Values[i]
						found = true
						return false
					}
				}
			}
			return true
		})
	}

	return result, found
}

// FindDefiningValueInBlock is like FindDefiningValue but searches within a
// single AST block (e.g. a function body) rather than across files.
func FindDefiningValueInBlock(info *types.Info, v *types.Var, block ast.Node) (ast.Expr, bool) {
	var defIdent *ast.Ident
	for ident, obj := range info.Defs {
		if obj == v {
			defIdent = ident
			break
		}
	}
	if defIdent == nil {
		return nil, false
	}

	var result ast.Expr
	var found bool
	ast.Inspect(block, func(node ast.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				return true
			}
			for i, lhs := range n.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident != defIdent {
					continue
				}
				if i < len(n.Rhs) {
					result = n.Rhs[i]
					found = true
					return false
				}
			}
		case *ast.ValueSpec:
			for i, ident := range n.Names {
				if ident != defIdent {
					continue
				}
				if i < len(n.Values) {
					result = n.Values[i]
					found = true
					return false
				}
			}
		}
		return true
	})
	return result, found
}

// IsFuncParam reports whether the given expression is an identifier that
// refers to a function parameter. If so, it returns the parameter index
// (position in the function signature) and the types.Object for the
// enclosing function (or the variable a closure is assigned to).
func IsFuncParam(info *types.Info, files []*ast.File, expr ast.Expr) (paramIdx int, funcObj types.Object, ok bool) {
	ident, isIdent := expr.(*ast.Ident)
	if !isIdent || info == nil {
		return -1, nil, false
	}
	obj := info.Uses[ident]
	if obj == nil {
		return -1, nil, false
	}
	v, isVar := obj.(*types.Var)
	if !isVar {
		return -1, nil, false
	}

	// Try named function declarations first.
	fd := findEnclosingFuncDecl(files, ident.Pos())
	if fd != nil && fd.Type != nil && fd.Type.Params != nil {
		idx := 0
		for _, field := range fd.Type.Params.List {
			for _, name := range field.Names {
				defObj := info.Defs[name]
				if defObj == v {
					fObj := funcObjForDecl(info, fd)
					if fObj == nil {
						return -1, nil, false
					}
					return idx, fObj, true
				}
				idx++
			}
			if len(field.Names) == 0 {
				idx++
			}
		}
	}

	// Try closure (FuncLit) — the closure may be assigned to a variable
	// whose call sites the call-graph tracer can resolve.
	fl := FindEnclosingFuncLit(files, ident.Pos())
	if fl != nil && fl.Type != nil && fl.Type.Params != nil {
		idx := 0
		for _, field := range fl.Type.Params.List {
			for _, name := range field.Names {
				defObj := info.Defs[name]
				if defObj == v {
					fObj := FuncLitVarObj(info, files, fl)
					if fObj == nil {
						return -1, nil, false
					}
					return idx, fObj, true
				}
				idx++
			}
			if len(field.Names) == 0 {
				idx++
			}
		}
	}

	return -1, nil, false
}

// findEnclosingFuncDecl returns the FuncDecl whose body contains pos.
func findEnclosingFuncDecl(files []*ast.File, pos token.Pos) *ast.FuncDecl {
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if fd.Body.Pos() <= pos && pos <= fd.Body.End() {
				return fd
			}
		}
	}
	return nil
}

// FindEnclosingFuncLit returns the innermost FuncLit (closure) whose body
// contains pos. It only returns FuncLits that are NOT inside a FuncDecl's
// direct parameter list — i.e., it finds closures assigned to variables.
func FindEnclosingFuncLit(files []*ast.File, pos token.Pos) *ast.FuncLit {
	var best *ast.FuncLit
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			fl, ok := node.(*ast.FuncLit)
			if !ok {
				return true
			}
			if fl.Body != nil && fl.Body.Pos() <= pos && pos <= fl.Body.End() {
				// Pick the innermost (most tightly enclosing) FuncLit.
				if best == nil || fl.Body.Pos() > best.Body.Pos() {
					best = fl
				}
			}
			return true
		})
	}
	return best
}

// FuncLitVarObj finds the variable that a FuncLit is assigned to.
// For example, given `render := func(...) { ... }`, it returns the
// types.Object for `render`. Returns nil if the FuncLit is not
// assigned to a named variable.
func FuncLitVarObj(info *types.Info, files []*ast.File, fl *ast.FuncLit) types.Object {
	for _, file := range files {
		var found types.Object
		ast.Inspect(file, func(node ast.Node) bool {
			if found != nil {
				return false
			}
			switch n := node.(type) {
			case *ast.AssignStmt:
				if n.Tok != token.DEFINE {
					return true
				}
				for i, rhs := range n.Rhs {
					if rhs == fl && i < len(n.Lhs) {
						if ident, ok := n.Lhs[i].(*ast.Ident); ok {
							found = info.Defs[ident]
						}
						return false
					}
				}
			case *ast.ValueSpec:
				for i, val := range n.Values {
					if val == fl && i < len(n.Names) {
						found = info.Defs[n.Names[i]]
						return false
					}
				}
			}
			return true
		})
		if found != nil {
			return found
		}
	}
	return nil
}

// funcObjForDecl returns the types.Object for a FuncDecl (either a
// standalone function or a method).
func funcObjForDecl(info *types.Info, fd *ast.FuncDecl) types.Object {
	if fd.Name == nil {
		return nil
	}
	return info.Defs[fd.Name]
}

// BuildCallSiteIndex scans all files and builds a map from function
// types.Object to all call expressions that invoke it.
func BuildCallSiteIndex(info *types.Info, files []*ast.File) map[types.Object][]*ast.CallExpr {
	index := make(map[types.Object][]*ast.CallExpr)
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var obj types.Object
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				obj = info.Uses[fn]
			case *ast.SelectorExpr:
				obj = info.Uses[fn.Sel]
			}
			if obj != nil {
				index[obj] = append(index[obj], call)
			}
			return true
		})
	}
	return index
}

// isReassigned reports whether the variable is the target of any plain
// assignment (=) after its definition. If so, the variable's value is not
// statically deterministic and should not be resolved.
func isReassigned(info *types.Info, v *types.Var, files []*ast.File) bool {
	for _, file := range files {
		reassigned := false
		ast.Inspect(file, func(node ast.Node) bool {
			if reassigned {
				return false
			}
			assign, ok := node.(*ast.AssignStmt)
			if !ok || assign.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range assign.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				obj := info.Uses[ident]
				if obj == nil {
					// For LHS of assignments, the ident may be in Defs
					// (for :=) but for =, it should be in Uses.
					obj = info.Defs[ident]
				}
				if obj == v {
					reassigned = true
					return false
				}
			}
			return true
		})
		if reassigned {
			return true
		}
	}
	return false
}
