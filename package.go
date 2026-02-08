package check

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"

	"golang.org/x/tools/go/packages"

	"github.com/typelate/check/internal/asteval"
	"github.com/typelate/check/internal/astgen"
)

type pendingCall struct {
	receiverObj  types.Object
	templateName string
	dataType     types.Type
}

type resolvedTemplate struct {
	templates asteval.Template
	functions asteval.TemplateFunctions
	metadata  *asteval.TemplateMetadata
}

// Package discovers all .ExecuteTemplate calls in the given package,
// resolves receiver variables to their template construction chains,
// and type-checks each call.
//
// ExecuteTemplate must be called with a string literal for the second parameter.
func Package(pkg *packages.Package) error {
	pending, receivers := findExecuteCalls(pkg)
	resolved, resolveErrs := resolveTemplates(pkg, receivers)
	callErr := checkCalls(pkg, pending, resolved)
	return errors.Join(append(resolveErrs, callErr)...)
}

// findExecuteCalls walks the package syntax looking for ExecuteTemplate calls
// and returns the pending calls along with the set of receiver objects that
// need template resolution.
func findExecuteCalls(pkg *packages.Package) ([]pendingCall, map[types.Object]struct{}) {
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
			if !asteval.IsTemplateMethod(pkg.TypesInfo, sel) {
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

	return pending, receiverSet
}

// resolveTemplates resolves each unique receiver object to its template
// construction chain, including additional ParseFS/Parse modifications.
func resolveTemplates(pkg *packages.Package, receivers map[types.Object]struct{}) (map[types.Object]*resolvedTemplate, []error) {
	resolved := make(map[types.Object]*resolvedTemplate)

	workingDirectory := packageDirectory(pkg)
	embeddedPaths, err := asteval.RelativeFilePaths(workingDirectory, pkg.EmbedFiles...)
	if err != nil {
		return nil, []error{fmt.Errorf("failed to calculate relative path for embedded files: %w", err)}
	}

	var resolveErrs []error

	resolveExpr := func(obj types.Object, name string, expr ast.Expr) {
		if _, needed := receivers[obj]; !needed {
			return
		}
		funcTypeMap := asteval.DefaultFunctions(pkg.Types)
		meta := &asteval.TemplateMetadata{}
		ts, _, _, err := asteval.EvaluateTemplateSelector(nil, pkg.Types, pkg.TypesInfo, expr, workingDirectory, name, "", "", pkg.Fset, pkg.Syntax, embeddedPaths, funcTypeMap, make(map[string]any), meta)
		if err != nil {
			resolveErrs = append(resolveErrs, err)
			return
		}
		resolved[obj] = &resolvedTemplate{
			templates: ts,
			functions: funcTypeMap,
			metadata:  meta,
		}
	}

	// Resolve top-level var declarations.
	for _, tv := range astgen.IterateValueSpecs(pkg.Syntax) {
		for i, ident := range tv.Names {
			if i >= len(tv.Values) {
				continue
			}
			obj := pkg.TypesInfo.Defs[ident]
			if obj == nil {
				continue
			}
			resolveExpr(obj, ident.Name, tv.Values[i])
		}
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
					for i, ident := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						obj := pkg.TypesInfo.Defs[ident]
						if obj == nil {
							continue
						}
						resolveExpr(obj, ident.Name, vs.Values[i])
					}
				}
			case *ast.AssignStmt:
				if n.Tok != token.DEFINE {
					return true
				}
				for i, lhs := range n.Lhs {
					if i >= len(n.Rhs) {
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
					resolveExpr(obj, ident.Name, n.Rhs[i])
				}
			}
			return true
		})
	}

	// Find additional ParseFS/Parse calls on resolved template variables.
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			obj := asteval.FindModificationReceiver(call, pkg.TypesInfo)
			if obj == nil {
				return true
			}
			rt, ok := resolved[obj]
			if !ok {
				return true
			}
			meta := &asteval.TemplateMetadata{}
			ts, _, _, err := asteval.EvaluateTemplateSelector(rt.templates, pkg.Types, pkg.TypesInfo, call, workingDirectory, "", "", "", pkg.Fset, pkg.Syntax, embeddedPaths, rt.functions, make(map[string]any), meta)
			if err != nil {
				return true
			}
			rt.templates = ts
			rt.metadata.EmbedFilePaths = append(rt.metadata.EmbedFilePaths, meta.EmbedFilePaths...)
			rt.metadata.ParseCalls = append(rt.metadata.ParseCalls, meta.ParseCalls...)
			return true
		})
	}

	return resolved, resolveErrs
}

// checkCalls type-checks each pending ExecuteTemplate call against its
// resolved template.
func checkCalls(pkg *packages.Package, pending []pendingCall, resolved map[types.Object]*resolvedTemplate) error {
	mergedFunctions := make(Functions)
	if pkg.Types != nil {
		mergedFunctions = DefaultFunctions(pkg.Types)
	}
	for _, rt := range resolved {
		for name, sig := range rt.functions {
			mergedFunctions[name] = sig
		}
	}

	var errs []error
	for _, p := range pending {
		rt, ok := resolved[p.receiverObj]
		if !ok {
			continue
		}
		looked := rt.templates.Lookup(p.templateName)
		if looked == nil {
			continue
		}
		global := NewGlobal(pkg.Types, pkg.Fset, rt.templates, mergedFunctions)
		if err := Execute(global, looked.Tree(), p.dataType); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func packageDirectory(pkg *packages.Package) string {
	if len(pkg.GoFiles) > 0 {
		return filepath.Dir(pkg.GoFiles[0])
	}
	return "."
}
