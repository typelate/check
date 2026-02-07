package main

import (
	"fmt"
	"go/token"
	"io"
	"os"

	"golang.org/x/tools/go/packages"

	"github.com/typelate/check"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}

	fset := token.NewFileSet()
	pkgs, err := packages.Load(&packages.Config{
		Fset: fset,
		Mode: packages.NeedTypesInfo | packages.NeedName | packages.NeedFiles |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedEmbedPatterns |
			packages.NeedEmbedFiles | packages.NeedImports | packages.NeedModule,
		Dir: dir,
	}, dir)
	if err != nil {
		fmt.Fprintf(stderr, "failed to load packages: %v\n", err)
		return 1
	}
	exitCode := 0
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			fmt.Fprintln(stderr, e)
			exitCode = 1
		}
		if err := check.Package(pkg); err != nil {
			fmt.Fprintln(stderr, err)
			exitCode = 1
		}
	}
	return exitCode
}
