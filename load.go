package check

import (
	"fmt"
	htmltemplate "html/template"
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
