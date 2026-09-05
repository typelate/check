package check

import (
	"fmt"
	"go/ast"
	"go/types"
	htmltemplate "html/template"
	"iter"
	texttemplate "text/template"
	"text/template/parse"

	"golang.org/x/tools/go/packages"

	"github.com/typelate/check/internal/asteval"
	"github.com/typelate/check/internal/astgen"
)

// LoadTemplates evaluates the construction chain of the package-level
// template variable named templatesVariable in pkg: chains of Must, New,
// Parse, ParseFS, ParseFiles, Delims, Funcs, and Option, with file
// patterns resolved against the package directory.
//
// The package must be loaded with syntax, type information, and embed
// files (packages.NeedSyntax, NeedTypes, NeedTypesInfo, and
// NeedEmbedFiles).
func LoadTemplates(pkg *packages.Package, templatesVariable string) (*Templates, error) {
	if pkg.TypesInfo == nil || pkg.Types == nil {
		return nil, fmt.Errorf("package %s was loaded without type information; load it with packages.NeedTypes and packages.NeedTypesInfo", pkg.PkgPath)
	}
	workingDirectory := packageDirectory(pkg)
	embeddedPaths, err := asteval.RelativeFilePaths(workingDirectory, pkg.EmbedFiles...)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate relative path for embedded files: %w", err)
	}
	for _, tv := range astgen.IterateValueSpecs(pkg.Syntax) {
		for i, ident := range tv.Names {
			if ident.Name != templatesVariable || i >= len(tv.Values) {
				continue
			}
			variable := pkg.TypesInfo.Defs[ident]
			if variable == nil {
				// A silently empty ExecuteTemplateCalls would be
				// undiagnosable; the object must resolve.
				return nil, fmt.Errorf("variable %s in package %s has no type object; the package was loaded without type information", templatesVariable, pkg.PkgPath)
			}
			functions := asteval.DefaultFunctions(pkg.Types)
			defaults := make(Functions, len(functions))
			for name, sig := range functions {
				defaults[name] = sig
			}
			meta := new(asteval.TemplateMetadata)
			ts, _, _, err := asteval.EvaluateTemplateSelector(nil, pkg.Types, pkg.TypesInfo, tv.Values[i], workingDirectory, ident.Name, "", "", pkg.Fset, pkg.Syntax, embeddedPaths, functions, make(map[string]any), meta)
			if err != nil {
				return nil, err
			}
			collected := make(Functions)
			for name, sig := range functions {
				if def, ok := defaults[name]; !ok || def != sig {
					collected[name] = sig
				}
			}
			result := &Templates{
				templates:   ts,
				functions:   Functions(functions),
				collected:   collected,
				definitions: newDefinitionSet(definitionsFor(pkg.Fset, meta.Sources)),
				pkg:         pkg,
				variable:    variable,
			}
			result.definitions.adoptTrees(result)
			return result, nil
		}
	}
	return nil, fmt.Errorf("variable %s not found in package %s", templatesVariable, pkg.PkgPath)
}

// Templates is one template variable's evaluated construction chain. It
// finds the parse trees and definitions a Global needs and unwraps the
// concrete template value.
type Templates struct {
	templates   asteval.Template
	functions   Functions
	collected   Functions
	definitions definitionSet
	pkg         *packages.Package
	variable    types.Object
}

// HTML returns the html/template value the variable holds; ok is false
// when it holds a text/template value.
func (t *Templates) HTML() (*htmltemplate.Template, bool) {
	return asteval.HTMLTemplate(t.templates)
}

// Text returns the text/template value the variable holds; ok is false
// when it holds an html/template value.
func (t *Templates) Text() (*texttemplate.Template, bool) {
	return asteval.TextTemplate(t.templates)
}

// FindTree implements TreeFinder over the evaluated template set.
func (t *Templates) FindTree(name string) (*parse.Tree, bool) {
	return t.templates.FindTree(name)
}

// FindDefinition implements DefinitionFinder: it reports where name was
// defined, with Definition.Define spanning the define or block clause.
func (t *Templates) FindDefinition(name string) (Definition, bool) {
	return t.definitions.FindDefinition(name)
}

// Functions are the default functions merged with the functions
// collected from Funcs calls in the construction chain.
func (t *Templates) Functions() Functions {
	return t.functions
}

// CollectedFunctions are only the functions collected from Funcs calls
// in the construction chain, without the defaults.
func (t *Templates) CollectedFunctions() Functions {
	return t.collected
}

// ExecuteTemplateCall is one templatesVariable.ExecuteTemplate(wr, name,
// data) call found in the loaded package.
type ExecuteTemplateCall struct {
	// Call is the ExecuteTemplate call expression.
	Call *ast.CallExpr

	// TemplateName is the call's template name string literal.
	TemplateName string

	// DataType is the data argument's type.
	DataType types.Type

	// Definition locates where the named template was defined. It is the
	// zero Definition when the template set does not know the name; use
	// Definition.Define.IsValid to tell.
	Definition Definition
}

// ExecuteTemplateCalls reports, in file order, each ExecuteTemplate call
// in the loaded package whose receiver resolves to the loaded template
// variable — resolution is by object, so a shadowed or renamed
// identifier does not match — and whose template name is a string
// literal.
func (t *Templates) ExecuteTemplateCalls() iter.Seq[ExecuteTemplateCall] {
	return func(yield func(ExecuteTemplateCall) bool) {
		if t.pkg == nil || t.variable == nil {
			return
		}
		for _, file := range t.pkg.Syntax {
			keepWalking := true
			ast.Inspect(file, func(node ast.Node) bool {
				if !keepWalking {
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) != 3 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ExecuteTemplate" {
					return true
				}
				if !asteval.IsTemplateMethod(t.pkg.TypesInfo, sel) {
					return true
				}
				receiverIdent, ok := sel.X.(*ast.Ident)
				if !ok || t.pkg.TypesInfo.Uses[receiverIdent] != t.variable {
					return true
				}
				templateName, ok := asteval.BasicLiteralString(call.Args[1])
				if !ok {
					return true
				}
				def, _ := t.definitions.FindDefinition(templateName)
				keepWalking = yield(ExecuteTemplateCall{
					Call:         call,
					TemplateName: templateName,
					DataType:     t.pkg.TypesInfo.TypeOf(call.Args[2]),
					Definition:   def,
				})
				return keepWalking
			})
			if !keepWalking {
				return
			}
		}
	}
}
