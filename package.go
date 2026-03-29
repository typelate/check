package check

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template/parse"

	"golang.org/x/tools/go/packages"

	"github.com/typelate/check/internal/asteval"
	"github.com/typelate/check/internal/astgen"
)

type pendingCall struct {
	call         *ast.CallExpr
	receiverObj  types.Object
	templateName string
	dataType     types.Type

	// isExecute is true for Execute (2-arg) calls, where the template
	// name is the receiver's root template name (resolved later).
	isExecute bool

	// For deferred resolution via call graph tracing.
	// nameParamIdx >= 0 means the template name comes from a function parameter.
	// dataParamIdx >= 0 means the data type comes from a function parameter.
	// receiverParamIdx >= 0 means the template receiver comes from a function parameter.
	// mapKeyParamIdx >= 0 means the receiver came from a map index where the
	// key is a function parameter (e.g. t, ok := templates[name]).
	nameParamIdx     int
	dataParamIdx     int
	receiverParamIdx int
	mapKeyParamIdx   int
	enclosingFunc    types.Object

	// mapKey is the resolved map index key when the receiver came from a
	// map lookup (e.g. templates[name] where name = "workbench/receiving.html").
	// Used to select the correct per-page template set.
	mapKey string
}

type resolvedTemplate struct {
	templates asteval.Template
	functions asteval.TemplateFunctions
	metadata  *asteval.TemplateMetadata
}

// resolvedTemplateSet wraps a resolvedTemplate with optional per-key
// variants for map-based template patterns. When byKey is non-nil,
// callers should look up a specific key to get a scoped template set
// that only contains the templates for that specific page.
type resolvedTemplateSet struct {
	// single is the default (merged) template set, used when there is no
	// per-key scoping or when the mapKey is not found in byKey.
	single *resolvedTemplate
	// byKey maps page keys to per-page template sets. Only populated when
	// the receiver came from a for-range loop storing templates into a map.
	byKey map[string]*resolvedTemplate
}

type ExecuteTemplateNodeInspectorFunc func(node *ast.CallExpr, t *parse.Tree, tp types.Type)

// WarningCategory identifies the kind of warning.
type WarningCategory int

const (
	// WarnNonStaticTemplateName indicates an ExecuteTemplate call with a
	// non-static string for the template name.
	WarnNonStaticTemplateName WarningCategory = iota + 1

	// WarnUnusedTemplate indicates a template that is defined but never
	// referenced by any ExecuteTemplate call or {{template}} action.
	WarnUnusedTemplate

	// WarnNilDereference indicates field access on a pointer type without
	// a nil guard ({{with}} or {{if}}).
	WarnNilDereference

	// WarnInterfaceFieldAccess indicates field access on an interface type
	// that cannot be statically verified.
	WarnInterfaceFieldAccess

	// WarnUnusedVariable indicates a template variable that is declared
	// (via $x := ...) but never referenced.
	WarnUnusedVariable

	// WarnDeadBranch indicates a conditional branch that can never execute
	// because the condition is a literal true, false, or nil constant.
	WarnDeadBranch

	// WarnInconsistentTemplateTypes indicates that a sub-template is
	// invoked from multiple {{template}} call sites with incompatible
	// data types, which will produce different runtime behaviour depending
	// on the caller.
	WarnInconsistentTemplateTypes
)

// Code returns the short diagnostic code for the warning category (e.g. "W001").
func (c WarningCategory) Code() string {
	return fmt.Sprintf("W%03d", int(c))
}

// PackageWarningFunc is called when a non-fatal issue is detected.
// The category identifies the warning type, allowing callers to filter.
type PackageWarningFunc func(category WarningCategory, pos token.Position, message string)

// DeferredCall represents an ExecuteTemplate call whose template name or
// data type could not be resolved within its own package. Callers from
// other packages may provide the concrete values via call-graph tracing.
type DeferredCall struct {
	pendingCall
	resolved map[types.Object]*resolvedTemplateSet

	// FuncObj is the exported function that wraps the ExecuteTemplate call.
	FuncObj types.Object
	// NameParamIdx is the parameter index providing the template name (-1 if resolved).
	NameParamIdx int
	// DataParamIdx is the parameter index providing the data (-1 if resolved).
	DataParamIdx int
	// ReceiverParamIdx is the parameter index providing the template receiver (-1 if resolved).
	ReceiverParamIdx int
}

// Package discovers all .ExecuteTemplate calls in the given package,
// resolves receiver variables to their template construction chains,
// and type-checks each call.
//
// ExecuteTemplate must be called with a string literal for the second parameter.
// If warn is non-nil, it is called for non-fatal issues such as unused templates
// or unguarded pointer access.
func Package(pkg *packages.Package, inspectCall ExecuteTemplateNodeInspectorFunc, inspectTemplate TemplateNodeInspectorFunc, warn PackageWarningFunc) error {
	_, err := PackageWithDeferred(pkg, inspectCall, inspectTemplate, warn, nil, nil)
	return err
}

// PackageWithDeferred is like Package but also accepts deferred calls from
// dependency packages and returns any new deferred calls discovered in this
// package. This enables cross-package call-graph tracing. allPkgs, if
// provided, enables cross-package parameter resolution for template
// construction tracing.
func PackageWithDeferred(pkg *packages.Package, inspectCall ExecuteTemplateNodeInspectorFunc, inspectTemplate TemplateNodeInspectorFunc, warn PackageWarningFunc, imported []DeferredCall, allPkgs []*packages.Package) ([]DeferredCall, error) {
	pending, receivers := findExecuteCalls(pkg, warn)

	// Resolve calls from imported packages' deferred wrappers.
	if len(imported) > 0 {
		pending = resolveImportedCalls(pkg, pending, receivers, imported)
	}

	pending = resolveCallGraph(pkg, pending, receivers, warn, allPkgs)

	// Collect deferred calls for exported functions before resolving templates.
	var deferred []DeferredCall
	var resolvedPending []pendingCall
	for _, p := range pending {
		if p.nameParamIdx >= 0 || p.dataParamIdx >= 0 || p.receiverParamIdx >= 0 {
			if p.enclosingFunc != nil && p.enclosingFunc.Exported() {
				deferred = append(deferred, DeferredCall{
					pendingCall:      p,
					FuncObj:          p.enclosingFunc,
					NameParamIdx:     p.nameParamIdx,
					DataParamIdx:     p.dataParamIdx,
					ReceiverParamIdx: p.receiverParamIdx,
				})
			}
			continue
		}
		resolvedPending = append(resolvedPending, p)
	}

	resolved, resolveErrs := resolveTemplates(pkg, receivers, allPkgs)

	// Attach resolved templates to deferred calls.
	for i := range deferred {
		deferred[i].resolved = resolved
	}

	callErr := checkCalls(pkg, resolvedPending, resolved, inspectCall, inspectTemplate, warn)
	return deferred, errors.Join(append(resolveErrs, callErr)...)
}

// resolveImportedCalls finds calls in pkg to exported wrapper functions from
// imported packages and creates pending calls with resolved arguments.
func resolveImportedCalls(pkg *packages.Package, pending []pendingCall, receivers map[types.Object]struct{}, imported []DeferredCall) []pendingCall {
	if len(imported) == 0 {
		return pending
	}

	// Build a lookup from function object to deferred calls.
	deferredByFunc := make(map[types.Object][]DeferredCall)
	for _, d := range imported {
		deferredByFunc[d.FuncObj] = append(deferredByFunc[d.FuncObj], d)
	}

	// Scan all call expressions in this package.
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}

			// Find the function being called.
			var calledObj types.Object
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				calledObj = pkg.TypesInfo.Uses[fn]
			case *ast.SelectorExpr:
				calledObj = pkg.TypesInfo.Uses[fn.Sel]
			}
			if calledObj == nil {
				return true
			}

			defs, ok := deferredByFunc[calledObj]
			if !ok {
				return true
			}

			for _, d := range defs {
				p := d.pendingCall

				// Resolve template name from this call site.
				if d.NameParamIdx >= 0 && d.NameParamIdx < len(call.Args) {
					name, ok := asteval.ResolveStringExpr(pkg.TypesInfo, pkg.Syntax, call.Args[d.NameParamIdx])
					if !ok {
						continue
					}
					p.templateName = name
					p.nameParamIdx = -1
				}

				// Resolve data type from this call site.
				if d.DataParamIdx >= 0 && d.DataParamIdx < len(call.Args) {
					concreteType := pkg.TypesInfo.TypeOf(call.Args[d.DataParamIdx])
					if concreteType != nil && !isEmptyInterface(concreteType) {
						p.dataType = concreteType
						p.dataParamIdx = -1
					}
				}

				// Resolve receiver from this call site.
				if d.ReceiverParamIdx >= 0 && d.ReceiverParamIdx < len(call.Args) {
					if ident, ok := call.Args[d.ReceiverParamIdx].(*ast.Ident); ok {
						if argObj := pkg.TypesInfo.Uses[ident]; argObj != nil {
							p.receiverObj = argObj
							p.receiverParamIdx = -1
							receivers[argObj] = struct{}{}
						}
					}
				}

				// Only add if template name is resolved.
				if p.nameParamIdx < 0 {
					p.enclosingFunc = nil
					pending = append(pending, p)
				}
			}

			return true
		})
	}

	return pending
}

// findExecuteCalls walks the package syntax looking for ExecuteTemplate and
// Execute calls and returns the pending calls along with the set of receiver
// objects that need template resolution.
func findExecuteCalls(pkg *packages.Package, warn PackageWarningFunc) ([]pendingCall, map[types.Object]struct{}) {
	var pending []pendingCall
	receiverSet := make(map[types.Object]struct{})

	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			isExecute := false
			switch {
			case sel.Sel.Name == "ExecuteTemplate" && len(call.Args) == 3:
				// tpl.ExecuteTemplate(w, name, data)
			case sel.Sel.Name == "Execute" && len(call.Args) == 2:
				// tpl.Execute(w, data)
				isExecute = true
			default:
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

			if isExecute {
				// Execute calls use the receiver's root template name,
				// which is resolved later in checkCalls.
				dataType := pkg.TypesInfo.TypeOf(call.Args[1])

				dataParamIdx := -1
				receiverParamIdx := -1
				var enclosingFunc types.Object

				if isEmptyInterface(dataType) {
					idx, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, call.Args[1])
					if isParam {
						dataParamIdx = idx
						enclosingFunc = fObj
					}
				}

				idx, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, receiverIdent)
				if isParam {
					receiverParamIdx = idx
					if enclosingFunc == nil {
						enclosingFunc = fObj
					}
				}

				pending = append(pending, pendingCall{
					call:             call,
					receiverObj:      obj,
					isExecute:        true,
					dataType:         dataType,
					nameParamIdx:     -1,
					dataParamIdx:     dataParamIdx,
					receiverParamIdx: receiverParamIdx,
					enclosingFunc:    enclosingFunc,
				})
				receiverSet[obj] = struct{}{}
				return true
			}

			templateName, nameOk := asteval.ResolveStringExpr(pkg.TypesInfo, pkg.Syntax, call.Args[1])
			dataType := pkg.TypesInfo.TypeOf(call.Args[2])

			nameParamIdx := -1
			dataParamIdx := -1
			receiverParamIdx := -1
			var enclosingFunc types.Object

			if !nameOk {
				// Check if the template name is a function parameter
				// that can be resolved via call-graph tracing.
				idx, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, call.Args[1])
				if isParam {
					nameParamIdx = idx
					enclosingFunc = fObj
				} else {
					if warn != nil {
						pos := pkg.Fset.Position(call.Args[1].Pos())
						warn(WarnNonStaticTemplateName, pos, "ExecuteTemplate called with non-static template name")
					}
					return true
				}
			}

			// Check if the data type is interface{}/any and comes from
			// a function parameter (Tier 4).
			if isEmptyInterface(dataType) {
				idx, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, call.Args[2])
				if isParam {
					dataParamIdx = idx
					if enclosingFunc == nil {
						enclosingFunc = fObj
					}
				}
			}

			// Check if the receiver is a function parameter.
			mapKeyParamIdx := -1
			idx, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, receiverIdent)
			if isParam {
				receiverParamIdx = idx
				if enclosingFunc == nil {
					enclosingFunc = fObj
				}
			} else {
				// The receiver may come from a map index (t, ok := templates[name]).
				// If the map key is a function parameter, capture it so we can
				// resolve it to the specific page key at each call site.
				mapKeyParamIdx = detectMapKeyParam(pkg.TypesInfo, pkg.Syntax, obj)
				if mapKeyParamIdx >= 0 && enclosingFunc == nil {
					// Find the enclosing function for call graph resolution.
					_, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, receiverIdent)
					if isParam {
						enclosingFunc = fObj
					} else {
						// The receiver itself isn't a param, but the map key is.
						// Find the enclosing closure/func for the map key param.
						fl := asteval.FindEnclosingFuncLit(pkg.Syntax, receiverIdent.Pos())
						if fl != nil {
							enclosingFunc = asteval.FuncLitVarObj(pkg.TypesInfo, pkg.Syntax, fl)
						}
					}
				}
			}

			pending = append(pending, pendingCall{
				call:             call,
				receiverObj:      obj,
				templateName:     templateName,
				dataType:         dataType,
				nameParamIdx:     nameParamIdx,
				dataParamIdx:     dataParamIdx,
				receiverParamIdx: receiverParamIdx,
				mapKeyParamIdx:   mapKeyParamIdx,
				enclosingFunc:    enclosingFunc,
			})
			receiverSet[obj] = struct{}{}
			return true
		})
	}

	return pending, receiverSet
}

// resolveTemplates resolves each unique receiver object to its template
// construction chain, including additional ParseFS/Parse modifications.
func resolveTemplates(pkg *packages.Package, receivers map[types.Object]struct{}, allPkgs []*packages.Package) (map[types.Object]*resolvedTemplateSet, []error) {
	resolved := make(map[types.Object]*resolvedTemplateSet)

	workingDirectory := packageDirectory(pkg)
	moduleRoot := ""
	if pkg.Module != nil {
		moduleRoot = pkg.Module.Dir
	}
	embeddedPaths, err := asteval.RelativeFilePaths(workingDirectory, pkg.EmbedFiles...)
	if err != nil {
		return nil, []error{fmt.Errorf("failed to calculate relative path for embedded files: %w", err)}
	}

	// Build a resolver that traces fs.FS function parameters back through
	// call sites across all packages to find the originating //go:embed var.
	embedFSResolver := buildEmbedFSResolver(pkg, allPkgs)


	var resolveErrs []error

	resolveExpr := func(obj types.Object, name string, expr ast.Expr) {
		if _, needed := receivers[obj]; !needed {
			return
		}

		// If the defining expression is a map index (e.g. t, ok := templates[name]),
		// trace through the map to find where values are stored into it.
		var sliceCtx *asteval.SliceEvalContext
		var storeInfo *mapStoreInfo
		expr, sliceCtx, storeInfo = traceMapIndex(pkg.TypesInfo, pkg.Syntax, expr, workingDirectory, moduleRoot, allPkgs)
		_ = storeInfo // used later for per-key template resolution

		// Ensure a SliceEvalContext exists so that non-literal ParseFS
		// pattern arguments (e.g. spread []string vars) can be resolved.
		if sliceCtx == nil {
			sliceCtx = &asteval.SliceEvalContext{
				Info:             pkg.TypesInfo,
				Files:            pkg.Syntax,
				WorkingDirectory: workingDirectory,
			}
		}

		// Provide the embed resolver for fs.Glob resolution so it can
		// trace fs.FS parameters to their //go:embed paths on demand.
		if sliceCtx.EmbedResolver == nil {
			sliceCtx.EmbedResolver = embedFSResolver
		}

		// Only attempt resolution if the expression is a call. Non-call
		// expressions (e.g. unresolved map lookups, function returns that
		// couldn't be traced) are silently skipped — the receiver simply
		// won't be type-checked.
		if _, isCall := expr.(*ast.CallExpr); !isCall {
			return
		}

		rts := &resolvedTemplateSet{}

		// If the template was stored inside a for-range loop, build per-key
		// template sets so each page gets its own scoped template set.
		if storeInfo != nil && storeInfo.rangeVar != nil && sliceCtx != nil {
			pageFiles, ok := asteval.ResolveStringSliceExpr(sliceCtx, storeInfo.rangeExpr)
			if ok && len(pageFiles) > 0 {
				byKey := make(map[string]*resolvedTemplate)
				for _, pageFile := range pageFiles {
					// Only make paths absolute if they use OS separators
					// (from filepath.Glob). Paths with forward slashes come
					// from fs.Glob/embed.FS and should stay relative.
					if !filepath.IsAbs(pageFile) && !strings.Contains(pageFile, "/") {
						pageFile = filepath.Join(sliceCtx.WorkingDirectory, pageFile)
					}
					perPageCtx := sliceCtx.WithBinding(storeInfo.rangeVar, pageFile)
					perMeta := &asteval.TemplateMetadata{}
					perFuncs := asteval.DefaultFunctions(pkg.Types)
					perTS, _, _, err := asteval.EvaluateTemplateSelector(nil, pkg.Types, pkg.TypesInfo, expr, workingDirectory, name, "", "", pkg.Fset, pkg.Syntax, embeddedPaths, perFuncs, make(map[string]any), perMeta, perPageCtx, embedFSResolver)
					if err != nil {
						continue
					}
					mapKey, ok := perPageCtx.ResolveString(storeInfo.keyExpr)
					if !ok {
						continue
					}
					byKey[mapKey] = &resolvedTemplate{
						templates: perTS,
						functions: perFuncs,
						metadata:  perMeta,
					}
				}
				if len(byKey) > 0 {
					rts.byKey = byKey
					resolved[obj] = rts
					return
				}
			}
		}

		// Standard single-set resolution (non-loop or fallback).
		funcTypeMap := asteval.DefaultFunctions(pkg.Types)
		meta := &asteval.TemplateMetadata{}
		ts, _, _, err := asteval.EvaluateTemplateSelector(nil, pkg.Types, pkg.TypesInfo, expr, workingDirectory, name, "", "", pkg.Fset, pkg.Syntax, embeddedPaths, funcTypeMap, make(map[string]any), meta, sliceCtx, embedFSResolver)
		if err != nil {
			resolveErrs = append(resolveErrs, err)
			return
		}
		rts.single = &resolvedTemplate{
			templates: ts,
			functions: funcTypeMap,
			metadata:  meta,
		}
		resolved[obj] = rts
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
			if rt.single == nil {
				return true
			}
			meta := &asteval.TemplateMetadata{}
			ts, _, _, err := asteval.EvaluateTemplateSelector(rt.single.templates, pkg.Types, pkg.TypesInfo, call, workingDirectory, "", "", "", pkg.Fset, pkg.Syntax, embeddedPaths, rt.single.functions, make(map[string]any), meta, nil, embedFSResolver)
			if err != nil {
				return true
			}
			rt.single.templates = ts
			rt.single.metadata.EmbedFilePaths = append(rt.single.metadata.EmbedFilePaths, meta.EmbedFilePaths...)
			rt.single.metadata.ParseCalls = append(rt.single.metadata.ParseCalls, meta.ParseCalls...)
			return true
		})
	}

	return resolved, resolveErrs
}

// checkCalls type-checks each pending ExecuteTemplate call against its
// resolved template.
func checkCalls(pkg *packages.Package, pending []pendingCall, resolved map[types.Object]*resolvedTemplateSet, inspectCall ExecuteTemplateNodeInspectorFunc, inspectTemplate TemplateNodeInspectorFunc, warn PackageWarningFunc) error {
	// Deduplicate warnings by (position, message) to avoid repeating the same
	// warning for shared templates (e.g. nav.html) checked in multiple per-page
	// template sets.
	if warn != nil {
		type warnKey struct {
			pos string
			msg string
		}
		seen := make(map[warnKey]bool)
		origWarn := warn
		warn = func(cat WarningCategory, pos token.Position, message string) {
			k := warnKey{pos: pos.String(), msg: message}
			if seen[k] {
				return
			}
			seen[k] = true
			origWarn(cat, pos, message)
		}
	}

	mergedFunctions := make(Functions)
	if pkg.Types != nil {
		mergedFunctions = DefaultFunctions(pkg.Types)
	}
	for _, rts := range resolved {
		if rts.single != nil {
			for name, sig := range rts.single.functions {
				mergedFunctions[name] = sig
			}
		}
	}

	// Track all referenced template names for unused detection.
	referenced := make(map[types.Object]map[string]struct{})

	// Wrap the user's inspectTemplate callback to also track {{template "name"}} references.
	var wrappedInspect TemplateNodeInspectorFunc
	if warn != nil {
		wrappedInspect = func(node *parse.TemplateNode, t *parse.Tree, tp types.Type) {
			// Find which receiver this tree belongs to and record the reference.
			for _, p := range pending {
				rts, ok := resolved[p.receiverObj]
				if !ok || rts.single == nil {
					continue
				}
				if _, found := rts.single.templates.FindTree(node.Name); found {
					if referenced[p.receiverObj] == nil {
						referenced[p.receiverObj] = make(map[string]struct{})
					}
					referenced[p.receiverObj][node.Name] = struct{}{}
					break
				}
			}
			if inspectTemplate != nil {
				inspectTemplate(node, t, tp)
			}
		}
	} else {
		wrappedInspect = inspectTemplate
	}

	// subTemplateTypes accumulates every (templateName → dataType) pair seen
	// across all {{template}} call sites, per receiver object, for W007.
	subTemplateTypes := make(map[types.Object]map[string][]types.Type)

	var errs []error
	seenErrs := make(map[string]bool)
	for _, p := range pending {
		rts, ok := resolved[p.receiverObj]
		if !ok {
			continue
		}
		// Select the per-page template set if available, otherwise use the
		// merged single set.
		rt := rts.single
		if rts.byKey != nil && p.mapKey != "" {
			if perPage, ok := rts.byKey[p.mapKey]; ok {
				rt = perPage
			}
		}
		if rt == nil {
			continue
		}
		// For Execute calls, use the receiver's root template name.
		templateName := p.templateName
		if p.isExecute {
			templateName = rt.templates.Name()
		}
		looked := rt.templates.Lookup(templateName)
		if looked == nil {
			continue
		}
		// Record the ExecuteTemplate target as referenced.
		if warn != nil {
			if referenced[p.receiverObj] == nil {
				referenced[p.receiverObj] = make(map[string]struct{})
			}
			referenced[p.receiverObj][templateName] = struct{}{}
		}
		global := NewGlobal(pkg.Types, pkg.Fset, rt.templates, mergedFunctions)
		global.InspectTemplateNode = wrappedInspect
		if warn != nil {
			global.Warn = func(cat WarningCategory, tree *parse.Tree, node parse.Node, message string) {
				loc, _ := tree.ErrorContext(node)
				warn(cat, parseLocation(loc), message)
			}
		}
		if inspectCall != nil {
			inspectCall(p.call, looked.Tree(), p.dataType)
		}
		if err := Execute(global, looked.Tree(), p.dataType); err != nil {
			errStr := err.Error()
			if !seenErrs[errStr] {
				seenErrs[errStr] = true
				errs = append(errs, err)
			}
		}
		// Merge sub-template call types collected during this Execute run.
		// Skip for per-page scoped calls — each page legitimately passes a
		// different type to shared sub-templates like {{template "content" .}},
		// and each is already fully type-checked by Execute above.
		if warn != nil && p.mapKey == "" {
			byName := subTemplateTypes[p.receiverObj]
			if byName == nil {
				byName = make(map[string][]types.Type)
				subTemplateTypes[p.receiverObj] = byName
			}
			for name, tps := range global.subTemplateCallTypes {
				byName[name] = append(byName[name], tps...)
			}
		}
	}

	// Warn about sub-templates called with incompatible types (W007).
	if warn != nil {
		for obj, byName := range subTemplateTypes {
			pos := pkg.Fset.Position(obj.Pos())
			names := make([]string, 0, len(byName))
			for n := range byName {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, name := range names {
				tps := byName[name]
				if incompatibleTypes(tps) {
					typeStrs := make([]string, len(tps))
					for i, tp := range tps {
						typeStrs[i] = types.TypeString(tp, nil)
					}
					warn(WarnInconsistentTemplateTypes, pos,
						fmt.Sprintf("template %q is called with incompatible types: %s",
							name, strings.Join(typeStrs, ", ")))
				}
			}
		}
	}

	// Warn about unused templates.
	if warn != nil {
		for obj, rts := range resolved {
			if rts.single == nil {
				continue
			}
			refs := referenced[obj]
			names := rts.single.templates.TemplateNames()
			sort.Strings(names)
			for _, name := range names {
				if name == "" {
					continue
				}
				// Skip templates with no content (e.g. the root template
				// created by template.New("name") that serves as a container).
				if tree, ok := rts.single.templates.FindTree(name); !ok || tree == nil || tree.Root == nil || len(tree.Root.Nodes) == 0 {
					continue
				}
				if refs != nil {
					if _, ok := refs[name]; ok {
						continue
					}
				}
				pos := pkg.Fset.Position(obj.Pos())
				warn(WarnUnusedTemplate, pos, fmt.Sprintf("template %q is defined but never referenced", name))
			}
		}
	}

	return errors.Join(errs...)
}

// isEmptyInterface reports whether tp is the empty interface (interface{} or any).
func isEmptyInterface(tp types.Type) bool {
	if tp == nil {
		return false
	}
	iface, ok := tp.Underlying().(*types.Interface)
	return ok && iface.NumMethods() == 0 && iface.NumEmbeddeds() == 0
}

// resolveCallGraph expands pending calls that have unresolved template names
// or data types by tracing through function call sites within the package.
// It returns the expanded list of pending calls and emits warnings for any
// calls that could not be resolved.
// deduplicatePending removes pending calls that are identical on the
// dimensions that affect type-checking: template name, receiver object,
// and data type. This prevents duplicate errors when the same template
// is reached through multiple indirect call sites with the same type.
func deduplicatePending(pending []pendingCall) []pendingCall {
	type key struct {
		templateName string
		receiverObj  types.Object
		dataType     string // types.Type.String() for comparison
		nameParam    int
		dataParam    int
		mapKey       string
	}
	seen := make(map[key]bool)
	var result []pendingCall
	for _, p := range pending {
		dt := ""
		if p.dataType != nil {
			dt = p.dataType.String()
		}
		k := key{
			templateName: p.templateName,
			receiverObj:  p.receiverObj,
			dataType:     dt,
			nameParam:    p.nameParamIdx,
			dataParam:    p.dataParamIdx,
			mapKey:       p.mapKey,
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		result = append(result, p)
	}
	return result
}

func resolveCallGraph(pkg *packages.Package, pending []pendingCall, receivers map[types.Object]struct{}, warn PackageWarningFunc, allPkgs []*packages.Package) []pendingCall {
	// Check if any calls need resolution.
	needsResolution := false
	for _, p := range pending {
		if p.nameParamIdx >= 0 || p.dataParamIdx >= 0 || p.receiverParamIdx >= 0 {
			needsResolution = true
			break
		}
	}
	if !needsResolution {
		return pending
	}

	callIndex := asteval.BuildCallSiteIndex(pkg.TypesInfo, pkg.Syntax)

	// Build an index of indirect call sites for closure variables passed as
	// arguments to functions in other packages. These are paired with the
	// type info from the package where the call occurs.
	var indirectIndex map[types.Object][]indirectCallSite
	if len(allPkgs) > 0 {
		indirectIndex = buildIndirectClosureCallSites(pkg, allPkgs)
	}

	// Iterate to a fixed point to handle multi-level call chains.
	const maxIterations = 5
	for iter := range maxIterations {
		changed := false
		var expanded []pendingCall

		for _, p := range pending {
			if p.nameParamIdx < 0 && p.dataParamIdx < 0 && p.receiverParamIdx < 0 {
				// Already fully resolved.
				expanded = append(expanded, p)
				continue
			}

			if p.enclosingFunc == nil {
				if p.nameParamIdx >= 0 {
					if warn != nil {
						pos := pkg.Fset.Position(p.call.Args[1].Pos())
						warn(WarnNonStaticTemplateName, pos, "ExecuteTemplate called with non-static template name")
					}
					continue
				}
				// Name resolved, keep with current type info.
				p.dataParamIdx = -1
				p.receiverParamIdx = -1
				expanded = append(expanded, p)
				continue
			}

			directSites := callIndex[p.enclosingFunc]
			indirectSites := indirectIndex[p.enclosingFunc]
			if len(directSites) == 0 && len(indirectSites) == 0 {
				if p.nameParamIdx >= 0 {
					// No intra-package call sites. If the function is
					// exported, keep the call for cross-package deferred
					// resolution. Otherwise, warn and drop.
					if p.enclosingFunc.Exported() {
						expanded = append(expanded, p)
					} else if warn != nil {
						pos := pkg.Fset.Position(p.call.Args[1].Pos())
						warn(WarnNonStaticTemplateName, pos, "ExecuteTemplate called with non-static template name")
					}
					continue
				}
				// Name is resolved but data/receiver are not. Keep the
				// call with whatever type information we have so that
				// downstream warnings (e.g. WarnInterfaceFieldAccess) fire.
				p.dataParamIdx = -1
				p.receiverParamIdx = -1
				p.enclosingFunc = nil
				expanded = append(expanded, p)
				continue
			}

			// Process direct (same-package) call sites.
			for _, cs := range directSites {
				newCall := p
				resolvedName := p.nameParamIdx < 0
				resolvedData := p.dataParamIdx < 0

				// Resolve template name from call site argument.
				if p.nameParamIdx >= 0 && p.nameParamIdx < len(cs.Args) {
					arg := cs.Args[p.nameParamIdx]
					name, ok := asteval.ResolveStringExpr(pkg.TypesInfo, pkg.Syntax, arg)
					if ok {
						newCall.templateName = name
						newCall.nameParamIdx = -1
						newCall.enclosingFunc = nil
						resolvedName = true
					} else {
						// Check if this call site arg is itself a function parameter.
						idx, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, arg)
						if isParam {
							newCall.nameParamIdx = idx
							newCall.enclosingFunc = fObj
							changed = true
						} else {
							// Unresolvable at this call site.
							if warn != nil {
								pos := pkg.Fset.Position(arg.Pos())
								warn(WarnNonStaticTemplateName, pos, "ExecuteTemplate called with non-static template name")
							}
							continue
						}
					}
				}

				// Resolve data type from call site argument.
				if p.dataParamIdx >= 0 && p.dataParamIdx < len(cs.Args) {
					arg := cs.Args[p.dataParamIdx]
					concreteType := pkg.TypesInfo.TypeOf(arg)
					if concreteType != nil && !isEmptyInterface(concreteType) {
						newCall.dataType = concreteType
						newCall.dataParamIdx = -1
						resolvedData = true
					} else {
						// Check if this call site arg is itself a function parameter.
						idx, fObj, isParam := asteval.IsFuncParam(pkg.TypesInfo, pkg.Syntax, arg)
						if isParam {
							newCall.dataParamIdx = idx
							if newCall.enclosingFunc == nil {
								newCall.enclosingFunc = fObj
							}
							changed = true
						}
						// If not a param, keep the interface{} type — it
						// will be handled by WarnInterfaceFieldAccess downstream.
					}
				}

				// Resolve template receiver from call site argument.
				if p.receiverParamIdx >= 0 && p.receiverParamIdx < len(cs.Args) {
					arg := cs.Args[p.receiverParamIdx]
					if ident, ok := arg.(*ast.Ident); ok {
						if argObj := pkg.TypesInfo.Uses[ident]; argObj != nil {
							newCall.receiverObj = argObj
							newCall.receiverParamIdx = -1
							receivers[argObj] = struct{}{}
							changed = true
						}
					}
				}

				// Resolve map key from call site argument.
				if p.mapKeyParamIdx >= 0 && p.mapKeyParamIdx < len(cs.Args) {
					arg := cs.Args[p.mapKeyParamIdx]
					if key, ok := asteval.ResolveStringExpr(pkg.TypesInfo, pkg.Syntax, arg); ok {
						newCall.mapKey = key
						newCall.mapKeyParamIdx = -1
					}
				}

				if resolvedName {
					allResolved := resolvedData && newCall.receiverParamIdx < 0
					if allResolved {
						newCall.enclosingFunc = nil
					}
					expanded = append(expanded, newCall)
					if !allResolved || p.nameParamIdx >= 0 {
						changed = true
					}
				} else {
					expanded = append(expanded, newCall)
				}
			}

			// Process indirect (cross-package) call sites. These come
			// from tracing closure variables passed as function arguments
			// into the callee's body.
			for _, ics := range indirectSites {
				cs := ics.call
				csInfo := ics.info
				newCall := p
				resolvedName := p.nameParamIdx < 0
				resolvedData := p.dataParamIdx < 0

				if p.nameParamIdx >= 0 && p.nameParamIdx < len(cs.Args) {
					arg := cs.Args[p.nameParamIdx]
					name, ok := asteval.ResolveStringExpr(csInfo, nil, arg)
					if ok {
						newCall.templateName = name
						newCall.nameParamIdx = -1
						newCall.enclosingFunc = nil
						resolvedName = true
					}
				}

				if p.dataParamIdx >= 0 && p.dataParamIdx < len(cs.Args) {
					arg := cs.Args[p.dataParamIdx]
					concreteType := csInfo.TypeOf(arg)
					if concreteType != nil && !isEmptyInterface(concreteType) {
						newCall.dataType = concreteType
						newCall.dataParamIdx = -1
						resolvedData = true
					}
				}

				// Resolve map key from indirect call site argument.
				if p.mapKeyParamIdx >= 0 && p.mapKeyParamIdx < len(cs.Args) {
					arg := cs.Args[p.mapKeyParamIdx]
					if key, ok := asteval.ResolveStringExpr(csInfo, nil, arg); ok {
						newCall.mapKey = key
						newCall.mapKeyParamIdx = -1
					}
				}

				if resolvedName {
					allResolved := resolvedData && newCall.receiverParamIdx < 0
					if allResolved {
						newCall.enclosingFunc = nil
					}
					expanded = append(expanded, newCall)
					changed = true
				}
			}
		}

		pending = deduplicatePending(expanded)

		if !changed || iter == maxIterations-1 {
			// Emit warnings for remaining unresolved calls, keeping
			// exported-function calls for cross-package deferred resolution.
			var final []pendingCall
			for _, p := range pending {
				if p.nameParamIdx >= 0 {
					if p.enclosingFunc != nil && p.enclosingFunc.Exported() {
						final = append(final, p)
					} else if warn != nil {
						pos := pkg.Fset.Position(p.call.Args[1].Pos())
						warn(WarnNonStaticTemplateName, pos, "ExecuteTemplate called with non-static template name")
					}
					continue
				}
				final = append(final, p)
			}
			return final
		}
	}

	return pending
}

// indirectCallSite pairs a call expression with the type info from the
// package where it occurs. This is needed because indirect call sites
// (calls to closure parameters inside other packages' function bodies)
// require type resolution from their own package context.
type indirectCallSite struct {
	call *ast.CallExpr
	info *types.Info
}

// buildIndirectClosureCallSites scans the current package for places where
// func-typed variables are passed as arguments to functions, then traces
// into those function bodies (across all loaded packages) to find calls to
// the corresponding parameter.
//
// This handles patterns like:
//
//	render := func(w http.ResponseWriter, name string, data any) {
//	    t.ExecuteTemplate(w, "base.html", data)
//	}
//	handlers.HandleVehiclesList(db, render)
//
// Inside HandleVehiclesList, the parameter render is called with concrete types:
//
//	func HandleVehiclesList(db *sql.DB, render func(http.ResponseWriter, string, any)) http.HandlerFunc {
//	    return func(w http.ResponseWriter, r *http.Request) {
//	        render(w, "vehicles/list.html", VehiclesListPage{Vehicles: vehicles})
//	    }
//	}
//
// The call render(w, "vehicles/list.html", VehiclesListPage{...}) is returned
// as an indirect call site of the original render variable, paired with
// the handler package's type info so types can be resolved correctly.
func buildIndirectClosureCallSites(pkg *packages.Package, allPkgs []*packages.Package) map[types.Object][]indirectCallSite {
	index := make(map[types.Object][]indirectCallSite)

	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var calledObj types.Object
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				calledObj = pkg.TypesInfo.Uses[fn]
			case *ast.SelectorExpr:
				calledObj = pkg.TypesInfo.Uses[fn.Sel]
			}
			if calledObj == nil {
				return true
			}

			for argIdx, arg := range call.Args {
				ident, ok := arg.(*ast.Ident)
				if !ok {
					continue
				}
				argObj := pkg.TypesInfo.Uses[ident]
				if argObj == nil {
					continue
				}
				if _, isSig := argObj.Type().Underlying().(*types.Signature); !isSig {
					continue
				}
				sites := findIndirectCallSites(calledObj, argIdx, allPkgs)
				if len(sites) > 0 {
					index[argObj] = append(index[argObj], sites...)
				}
			}
			return true
		})
	}
	return index
}

// findIndirectCallSites looks for calls to the parameter at position paramIdx
// inside the body of the function identified by calledObj. It searches
// across all loaded packages and returns call sites paired with their type info.
func findIndirectCallSites(calledObj types.Object, paramIdx int, allPkgs []*packages.Package) []indirectCallSite {
	var results []indirectCallSite

	for _, p := range allPkgs {
		if p.TypesInfo == nil {
			continue
		}
		for _, file := range p.Syntax {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Name == nil || fd.Body == nil {
					continue
				}
				if p.TypesInfo.Defs[fd.Name] != calledObj {
					continue
				}
				paramObj := paramAtIndex(p.TypesInfo, fd, paramIdx)
				if paramObj == nil {
					return nil
				}
				// Find all calls to this parameter in the function body
				// (including nested closures).
				info := p.TypesInfo
				ast.Inspect(fd.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					ident, ok := call.Fun.(*ast.Ident)
					if !ok {
						return true
					}
					if info.Uses[ident] == paramObj {
						results = append(results, indirectCallSite{call: call, info: info})
					}
					return true
				})
				return results
			}
		}
	}
	return results
}

// paramAtIndex returns the types.Object for the parameter at the given
// index in a function declaration's parameter list.
// detectMapKeyParam checks if the variable obj is defined from a map index
// expression (e.g. t, ok := templates[name]) where the index key is a
// function/closure parameter. If so, it returns the parameter index of that
// key. Returns -1 if the pattern is not found.
func detectMapKeyParam(info *types.Info, files []*ast.File, obj types.Object) int {
	v, ok := obj.(*types.Var)
	if !ok {
		return -1
	}
	// Find the defining expression for this variable.
	defExpr, ok := asteval.FindDefiningValue(info, v, files)
	if !ok {
		return -1
	}
	// Check if the defining expression is a map index (e.g. templates[name]).
	idx, ok := defExpr.(*ast.IndexExpr)
	if !ok {
		return -1
	}
	// Verify the map operand is actually a map type.
	mapType := info.TypeOf(idx.X)
	if mapType == nil {
		return -1
	}
	if _, isMap := mapType.Underlying().(*types.Map); !isMap {
		return -1
	}
	// Check if the index key is a function/closure parameter.
	paramIdx, _, isParam := asteval.IsFuncParam(info, files, idx.Index)
	if !isParam {
		return -1
	}
	return paramIdx
}

func paramAtIndex(info *types.Info, fd *ast.FuncDecl, idx int) types.Object {
	if fd.Type == nil || fd.Type.Params == nil {
		return nil
	}
	cur := 0
	for _, field := range fd.Type.Params.List {
		for _, name := range field.Names {
			if cur == idx {
				return info.Defs[name]
			}
			cur++
		}
		if len(field.Names) == 0 {
			if cur == idx {
				return nil // unnamed param, can't track
			}
			cur++
		}
	}
	return nil
}

// traceMapIndex checks whether expr is a map index expression (e.g.
// templates[name]) and, if so, looks for store operations into that map
// (mapVar[key] = value) to find the first value expression that was stored.
// It returns the traced expression and an optional SliceEvalContext with
// parameter bindings when the trace followed into a helper function.
//
// This allows the tool to trace through patterns like:
//
//	templates := make(map[string]*template.Template)
//	for ... { templates[name] = template.ParseFiles(...) }
//	t, ok := templates[name]
//	t.ExecuteTemplate(w, "base.html", data)
//
// It also handles the case where the map is returned from a function call:
//
//	templates := loadTemplates(root)   // returns map[string]*template.Template
//	t, ok := templates[name]
//
// In that case it follows into the function body to find map stores and
// builds parameter bindings from the call site.
func traceMapIndex(info *types.Info, files []*ast.File, expr ast.Expr, workingDirectory, moduleRoot string, allPkgs []*packages.Package) (ast.Expr, *asteval.SliceEvalContext, *mapStoreInfo) {
	idx, ok := expr.(*ast.IndexExpr)
	if !ok {
		return expr, nil, nil
	}
	// Resolve the map variable.
	ident, ok := idx.X.(*ast.Ident)
	if !ok {
		return expr, nil, nil
	}
	mapObj := info.Uses[ident]
	if mapObj == nil {
		mapObj = info.Defs[ident]
	}
	if mapObj == nil {
		return expr, nil, nil
	}
	// Verify it's actually a map type.
	if _, isMap := mapObj.Type().Underlying().(*types.Map); !isMap {
		return expr, nil, nil
	}

	// First, look for direct stores (mapVar[key] = value) in the current scope.
	if found := findMapStore(info, files, mapObj); found != nil {
		return found, nil, nil
	}

	// If no direct stores, the map may come from a function call.
	// Trace the map variable back to its defining value.
	if v, ok := mapObj.(*types.Var); ok {
		defExpr, ok := asteval.FindDefiningValue(info, v, files)
		if !ok {
			return expr, nil, nil
		}
		// If the defining value is a function call, follow into the function body.
		call, ok := defExpr.(*ast.CallExpr)
		if !ok {
			return expr, nil, nil
		}
		var calledObj types.Object
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			calledObj = info.Uses[fn]
		case *ast.SelectorExpr:
			calledObj = info.Uses[fn.Sel]
		}
		if calledObj == nil {
			return expr, nil, nil
		}

		// Find the function declaration and look for map stores inside it.
		for _, file := range files {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Name == nil || fd.Body == nil {
					continue
				}
				if info.Defs[fd.Name] != calledObj {
					continue
				}
				// Found the function body. Look for map stores on any
				// local map variable that is returned.
				if storeInfo := findMapStoreInFunc(info, fd); storeInfo != nil {
					// Build parameter bindings from the call site so
					// that expressions inside the function body can
					// resolve parameters to their concrete values.
					bindings := buildDeepParamBindings(info, files, call, fd, allPkgs)
					// Use the module root as the working directory for
					// resolving relative paths (template paths are
					// typically relative to where the binary runs, not
					// the package directory).
					sliceWD := workingDirectory
					if moduleRoot != "" {
						sliceWD = moduleRoot
					}
					ctx := &asteval.SliceEvalContext{
						Info:             info,
						Files:            files,
						Block:            fd.Body,
						Bindings:         bindings,
						WorkingDirectory: sliceWD,
					}
					return storeInfo.expr, ctx, storeInfo
				}
			}
		}
	}

	return expr, nil, nil
}

// buildDeepParamBindings constructs parameter bindings for a function call,
// resolving arguments that are themselves function parameters by tracing
// up the call graph. This handles chains like:
//
//	func New(templateRoot string) { loadTemplates(templateRoot) }
//	app.New(db, "templates")  // called from another package or same package
//
// It first tries direct string resolution, then checks if the arg is a
// parameter of an enclosing function and resolves it via call sites.
func buildDeepParamBindings(info *types.Info, files []*ast.File, call *ast.CallExpr, fd *ast.FuncDecl, allPkgs []*packages.Package) asteval.ParamBindings {
	if fd.Type == nil || fd.Type.Params == nil {
		return nil
	}
	bindings := make(asteval.ParamBindings)
	// Build call site indices for the current package and all packages
	// (for cross-package parameter resolution).
	var contexts []paramResolveContext
	contexts = append(contexts, paramResolveContext{info, files, asteval.BuildCallSiteIndex(info, files)})
	for _, p := range allPkgs {
		if p.TypesInfo != info && p.TypesInfo != nil {
			contexts = append(contexts, paramResolveContext{p.TypesInfo, p.Syntax, asteval.BuildCallSiteIndex(p.TypesInfo, p.Syntax)})
		}
	}

	argIdx := 0
	for _, field := range fd.Type.Params.List {
		for _, name := range field.Names {
			if argIdx >= len(call.Args) {
				return bindings
			}
			if v, ok := info.Defs[name].(*types.Var); ok {
				if s, ok := resolveStringDeep(info, files, call.Args[argIdx], contexts, 0); ok {
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

// resolveStringDeep resolves an expression to a string, chasing function
// parameters up the call graph across all packages when needed.
func resolveStringDeep(info *types.Info, files []*ast.File, expr ast.Expr, contexts []paramResolveContext, depth int) (string, bool) {
	if depth > 5 {
		return "", false
	}

	// Try direct resolution first.
	if s, ok := asteval.ResolveStringExpr(info, files, expr); ok {
		return s, true
	}

	// If it's an identifier referring to a function parameter, chase call sites
	// across all packages.
	paramIdx, funcObj, ok := asteval.IsFuncParam(info, files, expr)
	if !ok {
		return "", false
	}
	for _, ctx := range contexts {
		for _, cs := range ctx.index[funcObj] {
			if paramIdx < len(cs.Args) {
				if s, ok := resolveStringDeep(ctx.info, ctx.files, cs.Args[paramIdx], contexts, depth+1); ok {
					return s, true
				}
			}
		}
	}
	return "", false
}

type paramResolveContext struct {
	info  *types.Info
	files []*ast.File
	index map[types.Object][]*ast.CallExpr
}

// findMapStore scans files for the first assignment of the form
// mapVar[key] = value where mapVar resolves to mapObj.
func findMapStore(info *types.Info, files []*ast.File, mapObj types.Object) ast.Expr {
	for _, file := range files {
		var found ast.Expr
		ast.Inspect(file, func(node ast.Node) bool {
			if found != nil {
				return false
			}
			assign, ok := node.(*ast.AssignStmt)
			if !ok || assign.Tok != token.ASSIGN {
				return true
			}
			if len(assign.Lhs) < 1 || len(assign.Rhs) < 1 {
				return true
			}
			lhsIdx, ok := assign.Lhs[0].(*ast.IndexExpr)
			if !ok {
				return true
			}
			lhsIdent, ok := lhsIdx.X.(*ast.Ident)
			if !ok {
				return true
			}
			lhsObj := info.Uses[lhsIdent]
			if lhsObj == nil {
				lhsObj = info.Defs[lhsIdent]
			}
			if lhsObj == mapObj {
				found = assign.Rhs[0]
				return false
			}
			return true
		})
		if found != nil {
			return found
		}
	}
	return nil
}

// mapStoreInfo describes a map store found inside a function body.
type mapStoreInfo struct {
	// expr is the value expression stored into the map, traced back to its
	// defining expression (e.g. template.ParseFiles(files...)).
	expr ast.Expr
	// keyExpr is the map index key expression (e.g. name in templates[name] = t).
	keyExpr ast.Expr
	// rangeVar is the for-range iteration variable, if the store is inside
	// a for-range loop (e.g. page in "for _, page := range pageFiles").
	// nil if not inside a range loop.
	rangeVar *types.Var
	// rangeExpr is the range expression (e.g. pageFiles), if rangeVar is set.
	rangeExpr ast.Expr
}

// findMapStoreInFunc looks inside a function body for the first map store
// (localMap[key] = value) where localMap is of a map type, and returns the
// stored value expression along with loop context if the store is inside
// a for-range loop. If the stored value is a variable, it traces back to
// the defining expression.
func findMapStoreInFunc(info *types.Info, fd *ast.FuncDecl) *mapStoreInfo {
	var foundAssign *ast.AssignStmt
	ast.Inspect(fd.Body, func(node ast.Node) bool {
		if foundAssign != nil {
			return false
		}
		assign, ok := node.(*ast.AssignStmt)
		if !ok || assign.Tok != token.ASSIGN {
			return true
		}
		if len(assign.Lhs) < 1 || len(assign.Rhs) < 1 {
			return true
		}
		lhsIdx, ok := assign.Lhs[0].(*ast.IndexExpr)
		if !ok {
			return true
		}
		lhsIdent, ok := lhsIdx.X.(*ast.Ident)
		if !ok {
			return true
		}
		lhsObj := info.Uses[lhsIdent]
		if lhsObj == nil {
			lhsObj = info.Defs[lhsIdent]
		}
		if lhsObj == nil {
			return true
		}
		if _, isMap := lhsObj.Type().Underlying().(*types.Map); !isMap {
			return true
		}
		foundAssign = assign
		return false
	})
	if foundAssign == nil {
		return nil
	}

	result := &mapStoreInfo{expr: foundAssign.Rhs[0]}

	// Capture the map key expression from the LHS (e.g. name in templates[name] = t).
	if lhsIdx, ok := foundAssign.Lhs[0].(*ast.IndexExpr); ok {
		result.keyExpr = lhsIdx.Index
	}

	// If the stored value is a variable, trace it back to its defining
	// expression. This handles patterns like:
	//   t, err := template.ParseFiles(files...)
	//   templates[name] = t
	if ident, ok := result.expr.(*ast.Ident); ok {
		if obj := info.Uses[ident]; obj != nil {
			if v, ok := obj.(*types.Var); ok {
				if defExpr, ok := asteval.FindDefiningValueInBlock(info, v, fd.Body); ok {
					result.expr = defExpr
				}
			}
		}
	}

	// Check if the store is inside a for-range loop.
	result.rangeVar, result.rangeExpr = findEnclosingRange(info, fd.Body, foundAssign.Pos())

	return result
}

// findEnclosingRange checks if pos is inside a for-range loop within block.
// If so, returns the range iteration variable and the range expression.
func findEnclosingRange(info *types.Info, block ast.Node, pos token.Pos) (*types.Var, ast.Expr) {
	var rangeVar *types.Var
	var rangeExpr ast.Expr
	ast.Inspect(block, func(node ast.Node) bool {
		rs, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if rs.Body == nil || pos < rs.Body.Pos() || pos > rs.Body.End() {
			return true
		}
		// pos is inside this range body. Capture the value variable and expression.
		if rs.Value != nil {
			if ident, ok := rs.Value.(*ast.Ident); ok {
				if obj := info.Defs[ident]; obj != nil {
					if v, ok := obj.(*types.Var); ok {
						rangeVar = v
						rangeExpr = rs.X
					}
				}
			}
		}
		return true // keep looking for more tightly nested ranges
	})
	return rangeVar, rangeExpr
}

func packageDirectory(pkg *packages.Package) string {
	if len(pkg.GoFiles) > 0 {
		return filepath.Dir(pkg.GoFiles[0])
	}
	return "."
}

// incompatibleTypes reports whether a slice of types contains at least two
// types that are not mutually assignable. Types that carry no concrete
// information (untyped nil, empty interface) are skipped.
func incompatibleTypes(tps []types.Type) bool {
	var concrete []types.Type
	for _, tp := range tps {
		if tp == nil {
			continue
		}
		// Skip untyped nil — it can be passed to anything.
		if basic, ok := tp.Underlying().(*types.Basic); ok && basic.Kind() == types.UntypedNil {
			continue
		}
		// Skip empty interface — no structural information.
		if isEmptyInterface(tp) {
			continue
		}
		concrete = append(concrete, tp)
	}
	if len(concrete) < 2 {
		return false
	}
	first := concrete[0]
	for _, tp := range concrete[1:] {
		if !types.AssignableTo(tp, first) && !types.AssignableTo(first, tp) {
			return true
		}
	}
	return false
}

// parseLocation parses a "filename:line:col" string into a token.Position.
func parseLocation(loc string) token.Position {
	var pos token.Position
	if i := strings.LastIndex(loc, ":"); i >= 0 {
		pos.Column, _ = strconv.Atoi(loc[i+1:])
		loc = loc[:i]
	}
	if i := strings.LastIndex(loc, ":"); i >= 0 {
		pos.Line, _ = strconv.Atoi(loc[i+1:])
		loc = loc[:i]
	}
	pos.Filename = loc
	return pos
}

// buildEmbedFSResolver creates an EmbedFSResolver that traces fs.FS function
// parameters back through the call graph across all packages to find the
// originating package-level var with a //go:embed directive.
func buildEmbedFSResolver(pkg *packages.Package, allPkgs []*packages.Package) asteval.EmbedFSResolver {
	// Pre-build call site indices for all packages.
	type pkgContext struct {
		pkg   *packages.Package
		info  *types.Info
		files []*ast.File
		index map[types.Object][]*ast.CallExpr
	}
	var contexts []pkgContext
	contexts = append(contexts, pkgContext{pkg, pkg.TypesInfo, pkg.Syntax, asteval.BuildCallSiteIndex(pkg.TypesInfo, pkg.Syntax)})
	for _, p := range allPkgs {
		if p.TypesInfo != pkg.TypesInfo && p.TypesInfo != nil {
			contexts = append(contexts, pkgContext{p, p.TypesInfo, p.Syntax, asteval.BuildCallSiteIndex(p.TypesInfo, p.Syntax)})
		}
	}

	// findEmbedForObj looks up the //go:embed directive for a types.Object
	// that should be a package-level var, and returns the matched file paths
	// and the working directory of the package that owns the var.
	findEmbedForObj := func(obj types.Object) ([]string, string, bool) {
		for _, ctx := range contexts {
			if ctx.pkg.Types != obj.Pkg() {
				continue
			}
			for _, decl := range astgen.IterateGenDecl(ctx.files, token.VAR) {
				for _, s := range decl.Specs {
					spec, ok := s.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range spec.Names {
						defObj := ctx.info.Defs[name]
						if defObj != obj {
							continue
						}
						var comment strings.Builder
						asteval.ReadComments(&comment, decl.Doc, spec.Doc)
						templateNames := asteval.ParseTemplateNames(comment.String())
						if len(templateNames) == 0 {
							return nil, "", false
						}
						dir := packageDirectory(ctx.pkg)
						embedPaths, err := asteval.RelativeFilePaths(dir, ctx.pkg.EmbedFiles...)
						if err != nil {
							return nil, "", false
						}
						matched, err := asteval.EmbeddedFilesMatchingTemplateNameList(dir, ctx.pkg.Fset, decl, templateNames, embedPaths)
						if err != nil {
							return nil, "", false
						}
						return matched, dir, true
					}
				}
			}
			break
		}
		return nil, "", false
	}

	// resolveExprToEmbed traces an expression back through the call graph
	// to find a package-level var with //go:embed. Returns the matched
	// paths, the source package directory, and whether resolution succeeded.
	var resolveExprToEmbed func(info *types.Info, files []*ast.File, expr ast.Expr, depth int) ([]string, string, bool)
	resolveExprToEmbed = func(info *types.Info, files []*ast.File, expr ast.Expr, depth int) ([]string, string, bool) {
		if depth > 5 {
			return nil, "", false
		}

		switch e := expr.(type) {
		case *ast.Ident:
			obj := info.Uses[e]
			if obj == nil {
				return nil, "", false
			}
			if paths, dir, ok := findEmbedForObj(obj); ok {
				return paths, dir, true
			}
			// Try as a function parameter — chase call sites.
			paramIdx, funcObj, ok := asteval.IsFuncParam(info, files, expr)
			if !ok {
				return nil, "", false
			}
			for _, ctx := range contexts {
				for _, cs := range ctx.index[funcObj] {
					if paramIdx < len(cs.Args) {
						if paths, dir, ok := resolveExprToEmbed(ctx.info, ctx.files, cs.Args[paramIdx], depth+1); ok {
							return paths, dir, true
						}
					}
				}
			}
		case *ast.SelectorExpr:
			// Qualified identifier (e.g. beyond.TemplatesFS).
			obj := info.Uses[e.Sel]
			if obj == nil {
				return nil, "", false
			}
			if paths, dir, ok := findEmbedForObj(obj); ok {
				return paths, dir, true
			}
		}
		return nil, "", false
	}

	return func(info *types.Info, files []*ast.File, fsIdent *ast.Ident) ([]string, string, error) {
		if paths, dir, ok := resolveExprToEmbed(info, files, fsIdent, 0); ok {
			return paths, dir, nil
		}
		return nil, "", nil
	}
}
