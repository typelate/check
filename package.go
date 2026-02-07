package check

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"html/template"

	"golang.org/x/tools/go/packages"

	"github.com/typelate/check/internal/asteval"
	"github.com/typelate/check/internal/astgen"
)

// Package discovers all .ExecuteTemplate calls in the given package,
// resolves receiver variables to their template construction chains,
// and type-checks each call.
//
// ExecuteTemplate must be called with a string literal for the second parameter.
func Package(pkg *packages.Package) error {
	// Phase 1: Find all ExecuteTemplate calls and collect receiver objects.
	type pendingCall struct {
		receiverObj  types.Object
		templateName string
		dataType     types.Type
	}
	var pending []pendingCall
	receiverSet := make(map[types.Object]struct{})

	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ExecuteTemplate" {
				return true
			}
			// Verify the method belongs to html/template or text/template.
			if !isTemplateMethod(pkg.TypesInfo, sel) {
				return true
			}
			receiverIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			obj := pkg.TypesInfo.Uses[receiverIdent]
			if obj == nil {
				return true
			}
			templateName, ok := asteval.BasicLiteralString(call.Args[1])
			if !ok {
				return true
			}
			dataType := pkg.TypesInfo.TypeOf(call.Args[2])
			pending = append(pending, pendingCall{
				receiverObj:  obj,
				templateName: templateName,
				dataType:     dataType,
			})
			receiverSet[obj] = struct{}{}
			return true
		})
	}

	// Phase 2: Resolve each unique receiver object to its template construction chain.
	type resolvedTemplate struct {
		ts    *template.Template
		funcs asteval.TemplateFunctions
		meta  *asteval.TemplateMetadata
	}
	resolved := make(map[types.Object]*resolvedTemplate)

	workingDirectory := packageDirectory(pkg)
	embeddedPaths, err := asteval.RelativeFilePaths(workingDirectory, pkg.EmbedFiles...)
	if err != nil {
		return fmt.Errorf("failed to calculate relative path for embedded files: %w", err)
	}

	resolveValueSpec := func(tv *ast.ValueSpec) {
		for i, name := range tv.Names {
			if i >= len(tv.Values) {
				continue
			}
			obj := pkg.TypesInfo.Defs[name]
			if obj == nil {
				continue
			}
			if _, needed := receiverSet[obj]; !needed {
				continue
			}

			funcTypeMap := asteval.DefaultFunctions(pkg.Types)
			meta := &asteval.TemplateMetadata{}
			ts, _, _, err := asteval.EvaluateTemplateSelector(nil, pkg.Types, pkg.TypesInfo, tv.Values[i], workingDirectory, name.Name, "", "", pkg.Fset, pkg.Syntax, embeddedPaths, funcTypeMap, make(template.FuncMap), meta)
			if err != nil {
				return
			}
			resolved[obj] = &resolvedTemplate{
				ts:    ts,
				funcs: funcTypeMap,
				meta:  meta,
			}
		}
	}

	resolveAssignStmt := func(stmt *ast.AssignStmt) {
		if stmt.Tok != token.DEFINE {
			return
		}
		for i, lhs := range stmt.Lhs {
			if i >= len(stmt.Rhs) {
				continue
			}
			ident, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			obj := pkg.TypesInfo.Defs[ident]
			if obj == nil {
				continue
			}
			if _, needed := receiverSet[obj]; !needed {
				continue
			}

			funcTypeMap := asteval.DefaultFunctions(pkg.Types)
			meta := &asteval.TemplateMetadata{}
			ts, _, _, err := asteval.EvaluateTemplateSelector(nil, pkg.Types, pkg.TypesInfo, stmt.Rhs[i], workingDirectory, ident.Name, "", "", pkg.Fset, pkg.Syntax, embeddedPaths, funcTypeMap, make(template.FuncMap), meta)
			if err != nil {
				return
			}
			resolved[obj] = &resolvedTemplate{
				ts:    ts,
				funcs: funcTypeMap,
				meta:  meta,
			}
		}
	}

	// Resolve top-level var declarations.
	for _, tv := range astgen.IterateValueSpecs(pkg.Syntax) {
		resolveValueSpec(tv)
	}

	// Resolve function-local var declarations and short variable declarations.
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.DeclStmt:
				gd, ok := n.Decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					return true
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					resolveValueSpec(vs)
				}
			case *ast.AssignStmt:
				resolveAssignStmt(n)
			}
			return true
		})
	}

	// Phase 2b: Find additional ParseFS/Parse calls on resolved template variables.
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			obj := findModificationReceiver(call, pkg.TypesInfo)
			if obj == nil {
				return true
			}
			rt, ok := resolved[obj]
			if !ok {
				return true
			}
			meta := &asteval.TemplateMetadata{}
			ts, _, _, err := asteval.EvaluateTemplateSelector(rt.ts, pkg.Types, pkg.TypesInfo, call, workingDirectory, "", "", "", pkg.Fset, pkg.Syntax, embeddedPaths, rt.funcs, make(template.FuncMap), meta)
			if err != nil {
				return true
			}
			rt.ts = ts
			rt.meta.EmbedFilePaths = append(rt.meta.EmbedFilePaths, meta.EmbedFilePaths...)
			rt.meta.ParseCalls = append(rt.meta.ParseCalls, meta.ParseCalls...)
			return true
		})
	}

	// Phase 3: Type-check each ExecuteTemplate call.
	mergedFunctions := make(Functions)
	if pkg.Types != nil {
		mergedFunctions = DefaultFunctions(pkg.Types)
	}
	for _, rt := range resolved {
		for name, sig := range rt.funcs {
			mergedFunctions[name] = sig
		}
	}

	var errs []error
	for _, p := range pending {
		rt, ok := resolved[p.receiverObj]
		if !ok {
			continue
		}
		looked := rt.ts.Lookup(p.templateName)
		if looked == nil {
			continue
		}
		treeFinder := (*asteval.Forrest)(rt.ts)
		global := NewGlobal(pkg.Types, pkg.Fset, treeFinder, mergedFunctions)
		if err := Execute(global, looked.Tree, p.dataType); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// isTemplateMethod reports whether sel refers to a method on
// *html/template.Template or *text/template.Template.
func isTemplateMethod(typesInfo *types.Info, sel *ast.SelectorExpr) bool {
	if typesInfo == nil {
		return false
	}
	selection, ok := typesInfo.Selections[sel]
	if !ok {
		return false
	}
	fn, ok := selection.Obj().(*types.Func)
	if !ok {
		return false
	}
	fnPkg := fn.Pkg()
	if fnPkg == nil {
		return false
	}
	return fnPkg.Path() == "html/template" || fnPkg.Path() == "text/template"
}

// findModificationReceiver unwraps template.Must and returns the types.Object
// of the variable receiver for a method call like ts.ParseFS(...) or
// template.Must(ts.ParseFS(...)). Returns nil if no variable receiver is found.
func findModificationReceiver(expr *ast.CallExpr, typesInfo *types.Info) types.Object {
	sel, ok := expr.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		if isTemplatePkgIdent(typesInfo, x) && sel.Sel.Name == "Must" && len(expr.Args) == 1 {
			inner, ok := expr.Args[0].(*ast.CallExpr)
			if !ok {
				return nil
			}
			return findModificationReceiver(inner, typesInfo)
		}
		if isTemplatePkgIdent(typesInfo, x) {
			return nil
		}
		return typesInfo.Uses[x]
	}
	return nil
}

// isTemplatePkgIdent reports whether ident refers to the "html/template"
// or "text/template" package via the type checker.
func isTemplatePkgIdent(typesInfo *types.Info, ident *ast.Ident) bool {
	if typesInfo == nil {
		return false
	}
	obj := typesInfo.Uses[ident]
	pkgName, ok := obj.(*types.PkgName)
	if !ok {
		return false
	}
	path := pkgName.Imported().Path()
	return path == "html/template" || path == "text/template"
}

func packageDirectory(pkg *packages.Package) string {
	if len(pkg.GoFiles) > 0 {
		p := pkg.GoFiles[0]
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] == '/' || p[i] == '\\' {
				return p[:i]
			}
		}
	}
	return "."
}
