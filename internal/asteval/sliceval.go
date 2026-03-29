package asteval

import (
	"go/ast"
	"go/token"
	"go/types"
	posixPath "path"
	"path/filepath"
	"strconv"
)

// ParamBindings maps function parameter variables to their concrete string
// values from a call site, enabling cross-function string resolution.
type ParamBindings map[*types.Var]string

// BuildParamBindings constructs a ParamBindings map by matching call site
// arguments to function parameters. Only string-resolvable arguments are
// included. If an argument is itself a function parameter, the call graph
// is searched to resolve it from an outer call site.
func BuildParamBindings(info *types.Info, files []*ast.File, call *ast.CallExpr, fd *ast.FuncDecl) ParamBindings {
	if fd.Type == nil || fd.Type.Params == nil {
		return nil
	}
	bindings := make(ParamBindings)
	argIdx := 0
	for _, field := range fd.Type.Params.List {
		for _, name := range field.Names {
			if argIdx >= len(call.Args) {
				return bindings
			}
			if v, ok := info.Defs[name].(*types.Var); ok {
				if s, ok := ResolveStringExpr(info, files, call.Args[argIdx]); ok {
					bindings[v] = s
				} else if s, ok := resolveArgViaCallGraph(info, files, call.Args[argIdx]); ok {
					bindings[v] = s
				}
			}
			argIdx++
		}
		if len(field.Names) == 0 {
			argIdx++
		}
	}
	return bindings
}

// resolveArgViaCallGraph resolves a function argument that is itself a
// parameter by finding the enclosing function's call sites and resolving
// the argument there. This handles chains like:
//
//	func New(templateRoot string) { loadTemplates(templateRoot) }
//	app.New(db, "templates")
func resolveArgViaCallGraph(info *types.Info, files []*ast.File, arg ast.Expr) (string, bool) {
	paramIdx, funcObj, ok := IsFuncParam(info, files, arg)
	if !ok {
		return "", false
	}
	callSites := BuildCallSiteIndex(info, files)
	for _, cs := range callSites[funcObj] {
		if paramIdx < len(cs.Args) {
			if s, ok := ResolveStringExpr(info, files, cs.Args[paramIdx]); ok {
				return s, true
			}
		}
	}
	return "", false
}

// SliceEvalContext carries state for string-slice evaluation.
type SliceEvalContext struct {
	Info             *types.Info
	Files            []*ast.File
	Block            ast.Node // scoped block (e.g. function body) for local var lookups
	Bindings         ParamBindings
	WorkingDirectory string
	EmbeddedPaths    []string         // relative paths of embedded files (for fs.Glob resolution)
	EmbedResolver    EmbedFSResolver  // optional resolver for cross-package fs.FS tracing
	depth            int
}

// WithBinding returns a copy of the context with an additional variable binding.
// This is used for per-iteration evaluation, e.g. binding a range variable to
// a specific value for one iteration of a for-range loop.
func (ctx *SliceEvalContext) WithBinding(v *types.Var, value string) *SliceEvalContext {
	newBindings := make(ParamBindings)
	for k, val := range ctx.Bindings {
		newBindings[k] = val
	}
	newBindings[v] = value
	return &SliceEvalContext{
		Info:             ctx.Info,
		Files:            ctx.Files,
		Block:            ctx.Block,
		Bindings:         newBindings,
		WorkingDirectory: ctx.WorkingDirectory,
		EmbeddedPaths:    ctx.EmbeddedPaths,
		EmbedResolver:    ctx.EmbedResolver,
	}
}

const maxSliceEvalDepth = 15

// ResolveStringSliceExpr evaluates an AST expression that produces a []string
// value. It handles composite literals, append, variables, filepath.Glob, and
// spread expressions.
func ResolveStringSliceExpr(ctx *SliceEvalContext, expr ast.Expr) ([]string, bool) {
	if ctx.depth > maxSliceEvalDepth {
		return nil, false
	}
	ctx.depth++
	defer func() { ctx.depth-- }()

	switch e := expr.(type) {
	case *ast.CompositeLit:
		return ctx.resolveCompositeLit(e)
	case *ast.Ident:
		return ctx.resolveIdentSlice(e)
	case *ast.CallExpr:
		return ctx.resolveCallSlice(e)
	case *ast.SliceExpr:
		return ResolveStringSliceExpr(ctx, e.X)
	}

	return nil, false
}

// resolveCompositeLit handles []string{...} composite literals.
func (ctx *SliceEvalContext) resolveCompositeLit(cl *ast.CompositeLit) ([]string, bool) {
	var result []string
	for _, elt := range cl.Elts {
		s, ok := ctx.resolveString(elt)
		if !ok {
			return nil, false
		}
		result = append(result, s)
	}
	return result, true
}

// resolveIdentSlice resolves an identifier to a []string value.
func (ctx *SliceEvalContext) resolveIdentSlice(ident *ast.Ident) ([]string, bool) {
	obj := ctx.Info.Uses[ident]
	if obj == nil {
		return nil, false
	}
	v, ok := obj.(*types.Var)
	if !ok {
		return nil, false
	}

	// Check parameter bindings — a bound param is a single string.
	if s, ok := ctx.Bindings[v]; ok {
		return []string{s}, true
	}

	// Trace variable to its defining expression.
	var defExpr ast.Expr
	var found bool
	if ctx.Block != nil {
		defExpr, found = FindDefiningValueInBlock(ctx.Info, v, ctx.Block)
	}
	if !found {
		defExpr, found = FindDefiningValue(ctx.Info, v, ctx.Files)
	}
	if !found {
		return nil, false
	}

	// If the defining value is make([]string, ...), look for subsequent
	// copy and index mutations to build the full slice contents.
	if isMakeSliceCall(defExpr) && ctx.Block != nil {
		return ctx.resolveMutatedSlice(v)
	}

	return ResolveStringSliceExpr(ctx, defExpr)
}

// resolveCallSlice handles function calls that return []string or string.
func (ctx *SliceEvalContext) resolveCallSlice(call *ast.CallExpr) ([]string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		if ident, ok := call.Fun.(*ast.Ident); ok {
			return ctx.resolveBuiltinCallSlice(ident.Name, call)
		}
		return nil, false
	}

	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil, false
	}

	pkgObj := ctx.Info.Uses[pkgIdent]
	if pkgObj == nil {
		return nil, false
	}
	pkgName, isPkg := pkgObj.(*types.PkgName)
	if !isPkg {
		return nil, false
	}
	pkgPath := pkgName.Imported().Path()
	funcName := sel.Sel.Name

	switch {
	case pkgPath == "path/filepath" && funcName == "Join":
		return ctx.resolveFilepathJoin(call)
	case pkgPath == "path/filepath" && funcName == "Glob":
		return ctx.resolveFilepathGlob(call)
	case pkgPath == "path/filepath" && funcName == "Base":
		return ctx.resolveFilepathBase(call)
	case pkgPath == "path/filepath" && funcName == "Dir":
		return ctx.resolveFilepathDir(call)
	case pkgPath == "path" && funcName == "Base":
		return ctx.resolvePathBase(call)
	case pkgPath == "path" && funcName == "Dir":
		return ctx.resolvePathDir(call)
	case pkgPath == "io/fs" && funcName == "Glob":
		return ctx.resolveFSGlob(call)
	}

	return nil, false
}

// resolveBuiltinCallSlice handles builtin calls like append.
func (ctx *SliceEvalContext) resolveBuiltinCallSlice(name string, call *ast.CallExpr) ([]string, bool) {
	switch name {
	case "append":
		if len(call.Args) < 2 {
			return nil, false
		}
		base, ok := ResolveStringSliceExpr(ctx, call.Args[0])
		if !ok {
			return nil, false
		}
		if call.Ellipsis.IsValid() && len(call.Args) == 2 {
			extra, ok := ResolveStringSliceExpr(ctx, call.Args[1])
			if !ok {
				return nil, false
			}
			return append(base, extra...), true
		}
		for _, arg := range call.Args[1:] {
			s, ok := ctx.resolveString(arg)
			if !ok {
				return nil, false
			}
			base = append(base, s)
		}
		return base, true

	case "make":
		return []string{}, true
	}
	return nil, false
}

// resolveFilepathJoin handles filepath.Join(args...) → single string.
func (ctx *SliceEvalContext) resolveFilepathJoin(call *ast.CallExpr) ([]string, bool) {
	var parts []string
	for _, arg := range call.Args {
		s, ok := ctx.resolveString(arg)
		if !ok {
			return nil, false
		}
		parts = append(parts, s)
	}
	return []string{filepath.Join(parts...)}, true
}

// resolveFilepathGlob handles filepath.Glob(pattern) → run against filesystem.
func (ctx *SliceEvalContext) resolveFilepathGlob(call *ast.CallExpr) ([]string, bool) {
	if len(call.Args) != 1 {
		return nil, false
	}
	pattern, ok := ctx.resolveString(call.Args[0])
	if !ok {
		return nil, false
	}
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(ctx.WorkingDirectory, pattern)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, false
	}
	return matches, true
}

// resolveFSGlob handles fs.Glob(fsys, pattern) by matching the pattern
// against the embedded file paths stored in the context.
func (ctx *SliceEvalContext) resolveFSGlob(call *ast.CallExpr) ([]string, bool) {
	if len(call.Args) != 2 {
		return nil, false
	}
	pattern, ok := ctx.resolveString(call.Args[1])
	if !ok {
		return nil, false
	}

	// Get the embedded file paths to glob against. First try the context's
	// pre-populated paths. If empty, try resolving the fs.FS argument
	// (first arg) via the embed resolver to get the right package's paths.
	embeddedPaths := ctx.EmbeddedPaths
	if len(embeddedPaths) == 0 && ctx.EmbedResolver != nil {
		if fsIdent, ok := call.Args[0].(*ast.Ident); ok {
			paths, _, err := ctx.EmbedResolver(ctx.Info, ctx.Files, fsIdent)
			if err == nil && len(paths) > 0 {
				embeddedPaths = paths
			}
		}
	}
	if len(embeddedPaths) == 0 {
		return nil, false
	}

	// embed.FS uses forward slashes; match and return with forward slashes
	// so that path.Base/path.Dir work correctly downstream.
	var matches []string
	for _, ep := range embeddedPaths {
		epSlash := filepath.ToSlash(ep)
		matched, err := filepath.Match(pattern, epSlash)
		if err != nil {
			return nil, false
		}
		if matched {
			matches = append(matches, epSlash)
		}
	}
	return matches, true
}

// resolveFilepathBase handles filepath.Base(path) → single string.
func (ctx *SliceEvalContext) resolveFilepathBase(call *ast.CallExpr) ([]string, bool) {
	if len(call.Args) != 1 {
		return nil, false
	}
	s, ok := ctx.resolveString(call.Args[0])
	if !ok {
		return nil, false
	}
	return []string{filepath.Base(s)}, true
}

// resolveFilepathDir handles filepath.Dir(path) → single string.
func (ctx *SliceEvalContext) resolveFilepathDir(call *ast.CallExpr) ([]string, bool) {
	if len(call.Args) != 1 {
		return nil, false
	}
	s, ok := ctx.resolveString(call.Args[0])
	if !ok {
		return nil, false
	}
	return []string{filepath.Dir(s)}, true
}

// resolvePathBase handles path.Base(p) → single string (forward slashes).
func (ctx *SliceEvalContext) resolvePathBase(call *ast.CallExpr) ([]string, bool) {
	if len(call.Args) != 1 {
		return nil, false
	}
	s, ok := ctx.resolveString(call.Args[0])
	if !ok {
		return nil, false
	}
	return []string{posixPath.Base(s)}, true
}

// resolvePathDir handles path.Dir(p) → single string (forward slashes).
func (ctx *SliceEvalContext) resolvePathDir(call *ast.CallExpr) ([]string, bool) {
	if len(call.Args) != 1 {
		return nil, false
	}
	s, ok := ctx.resolveString(call.Args[0])
	if !ok {
		return nil, false
	}
	return []string{posixPath.Dir(s)}, true
}

// ResolveString resolves a single AST expression to a string value.
func (ctx *SliceEvalContext) ResolveString(expr ast.Expr) (string, bool) {
	return ctx.resolveString(expr)
}

// resolveString resolves a single AST expression to a string value.
func (ctx *SliceEvalContext) resolveString(expr ast.Expr) (string, bool) {
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
		obj := ctx.Info.Uses[e]
		if obj == nil {
			return "", false
		}
		if v, ok := obj.(*types.Var); ok {
			if s, ok := ctx.Bindings[v]; ok {
				return s, true
			}
			var defExpr ast.Expr
			var found bool
			if ctx.Block != nil {
				defExpr, found = FindDefiningValueInBlock(ctx.Info, v, ctx.Block)
			}
			if !found {
				defExpr, found = FindDefiningValue(ctx.Info, v, ctx.Files)
			}
			if !found {
				return "", false
			}
			return ctx.resolveString(defExpr)
		}
		return ResolveStringExpr(ctx.Info, ctx.Files, expr)

	case *ast.CallExpr:
		result, ok := ctx.resolveCallSlice(e)
		if !ok || len(result) != 1 {
			return "", false
		}
		return result[0], true

	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := ctx.resolveString(e.X)
		if !ok {
			return "", false
		}
		right, ok := ctx.resolveString(e.Y)
		if !ok {
			return "", false
		}
		return left + right, true

	case *ast.ParenExpr:
		return ctx.resolveString(e.X)
	}

	return "", false
}

// isMakeSliceCall reports whether expr is a call to make([]T, ...).
func isMakeSliceCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	if !ok || ident.Name != "make" {
		return false
	}
	if len(call.Args) < 1 {
		return false
	}
	_, ok = call.Args[0].(*ast.ArrayType)
	return ok
}

// findRangeExpr checks if the given variable is defined in a range clause
// and returns the range expression if so.
func findRangeExpr(info *types.Info, v *types.Var, block ast.Node) (ast.Expr, bool) {
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
		rs, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if rs.Key == defIdent || rs.Value == defIdent {
			result = rs.X
			found = true
			return false
		}
		return true
	})
	return result, found
}

// resolveMutatedSlice handles the pattern:
//
//	files := make([]string, len(shared)+1)
//	copy(files, shared)
//	files[len(shared)] = page
func (ctx *SliceEvalContext) resolveMutatedSlice(v *types.Var) ([]string, bool) {
	var result []string
	var failed bool

	ast.Inspect(ctx.Block, func(node ast.Node) bool {
		if failed {
			return false
		}
		switch n := node.(type) {
		case *ast.ExprStmt:
			call, ok := n.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "copy" || len(call.Args) != 2 {
				return true
			}
			dstIdent, ok := call.Args[0].(*ast.Ident)
			if !ok {
				return true
			}
			if ctx.Info.Uses[dstIdent] != v {
				return true
			}
			src, ok := ResolveStringSliceExpr(ctx, call.Args[1])
			if ok {
				result = append(result, src...)
			} else {
				failed = true
				return false
			}

		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN || len(n.Lhs) != 1 || len(n.Rhs) != 1 {
				return true
			}
			idx, ok := n.Lhs[0].(*ast.IndexExpr)
			if !ok {
				return true
			}
			idxIdent, ok := idx.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ctx.Info.Uses[idxIdent] != v {
				return true
			}
			s, ok := ctx.resolveString(n.Rhs[0])
			if ok {
				result = append(result, s)
			} else if rhsIdent, ok := n.Rhs[0].(*ast.Ident); ok {
				if rhsObj := ctx.Info.Uses[rhsIdent]; rhsObj != nil {
					if rhsVar, ok := rhsObj.(*types.Var); ok {
						if rangeExpr, ok := findRangeExpr(ctx.Info, rhsVar, ctx.Block); ok {
							vals, ok := ResolveStringSliceExpr(ctx, rangeExpr)
							if ok {
								result = append(result, vals...)
							}
						}
					}
				}
			}
		}
		return true
	})

	if len(result) == 0 {
		return nil, false
	}
	return result, true
}
