package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"log"
	"os"
	"text/template/parse"

	"golang.org/x/tools/go/packages"

	"github.com/typelate/check"
)

func main() {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatalln(err)
	}
	os.Exit(run(wd, os.Args[1:], os.Stdout, os.Stderr))
}

func run(dir string, args []string, stdout, stderr io.Writer) int {
	loadArgs := []string{"."}
	if len(args) > 0 {
		loadArgs = args
	}

	fset := token.NewFileSet()
	pkgs, err := packages.Load(&packages.Config{
		Fset: fset,
		Mode: packages.NeedTypesInfo | packages.NeedName | packages.NeedFiles |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedEmbedPatterns |
			packages.NeedEmbedFiles | packages.NeedImports | packages.NeedModule,
		Dir: dir,
	}, loadArgs...)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "failed to load packages: %v\n", err)
		return 1
	}
	exitCode := 0
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			_, _ = fmt.Fprintln(stderr, e)
			exitCode = 1
		}
		if err := check.Package(pkg, func(node *ast.CallExpr, t *parse.Tree, tp types.Type) {

		}, func(node *parse.TemplateNode, t *parse.Tree, tp types.Type) {

		}); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			exitCode = 1
		}
	}
	return exitCode
}
