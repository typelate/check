package check_test

import (
	"fmt"
	"go/token"
	"log"
	"slices"
	"text/template"
	"text/template/parse"

	"golang.org/x/tools/go/packages"

	"github.com/typelate/check"
)

type Person struct {
	Name string
}

func ExampleExecute() {
	// 1. Load Go packages with type info.
	fset := token.NewFileSet()
	pkgs, err := packages.Load(&packages.Config{
		Fset:  fset,
		Tests: true,
		Mode: packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedSyntax |
			packages.NeedFiles |
			packages.NeedName |
			packages.NeedModule,
		Dir: ".",
	}, ".")
	if err != nil {
		log.Fatal(err)
	}
	const testPackageName = "check_test"
	packageIndex := slices.IndexFunc(pkgs, func(p *packages.Package) bool {
		return p.Name == testPackageName
	})
	if packageIndex < 0 {
		log.Fatalf("%s package not found", testPackageName)
	}
	testPackage := pkgs[packageIndex]

	// 2. Parse a template.
	tmpl, err := template.New("example").Parse(
		/* language=gotemplate */ `
{{define "unknown field" -}}
	{{.UnknownField}}
{{- end}}
{{define "known field" -}}
	Hello, {{.Name}}!
{{- end}}"
`)
	if err != nil {
		log.Fatalf("parse error: %v", err)
	}

	// 3. Create a TreeFinder (wraps Template.Lookup).
	treeFinder := check.FindTreeFunc(func(name string) (*parse.Tree, bool) {
		if named := tmpl.Lookup(name); named != nil {
			return named.Tree, true
		}
		return nil, false
	})

	// 4. Build a function checker.
	functions := check.DefaultFunctions(testPackage.Types)

	// 5. Initialize a Global.
	global := check.NewGlobal(testPackage.Types, fset, treeFinder, functions)

	// 6. Look up a type used by the template.
	personObj := testPackage.Types.Scope().Lookup("Person")
	if personObj == nil {
		log.Fatalf("type Person not found in %s", testPackage.PkgPath)
	}

	// 7. Type-check the template.
	{
		const templateName = "unknown field"
		if err := check.Execute(global, tmpl.Lookup("unknown field").Tree, personObj.Type()); err != nil {
			fmt.Printf("template %q type error: %v\n", templateName, err)
		} else {
			fmt.Printf("template %q type-check passed\n", templateName)
		}
	}
	{
		const templateName = "known field"
		if err := check.Execute(global, tmpl.Lookup("known field").Tree, personObj.Type()); err != nil {
			fmt.Printf("template %q type error: %v\n", templateName, err)
		} else {
			fmt.Printf("template %q type-check passed\n", templateName)
		}
	}
	// Output: template "unknown field" type error: type check failed: example:3:3: UnknownField not found on github.com/typelate/check_test.Person
	// template "known field" type-check passed
}
