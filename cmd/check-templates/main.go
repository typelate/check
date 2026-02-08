package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
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
	var (
		verbose      bool
		outputFormat string
	)

	flagSet := flag.NewFlagSet("check-templates", flag.ContinueOnError)
	flagSet.BoolVar(&verbose, "v", false, "show all calls")
	flagSet.StringVar(&dir, "C", dir, "change directory")
	flagSet.StringVar(&outputFormat, "o", "tsv", "output format: tsv or jsonl")
	if err := flagSet.Parse(args); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}

	switch outputFormat {
	case "tsv", "jsonl":
	default:
		_, _ = fmt.Fprintf(stderr, "unsupported output format: %s\n", outputFormat)
		return 1
	}
	if !verbose {
		stdout = io.Discard
	}
	writeCall := writeCallFunc(outputFormat, stdout)

	loadArgs := []string{"."}
	if args := flagSet.Args(); len(args) > 0 {
		loadArgs = flagSet.Args()
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
			writeCall(fset.Position(node.Pos()), t.Name, tp)
		}, func(node *parse.TemplateNode, t *parse.Tree, tp types.Type) {
			loc, _ := t.ErrorContext(node)
			writeCall(parseLocation(loc), t.Name, tp)
		}); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			exitCode = 1
		}
	}
	return exitCode
}

type callRecord struct {
	Filename     string `json:"filename"`
	Line         int    `json:"line"`
	Column       int    `json:"column"`
	Offset       int    `json:"offset"`
	TemplateName string `json:"template_name"`
	DataType     string `json:"data_type"`
}

func writeCallFunc(outputFormat string, stdout io.Writer) func(pos token.Position, templateName string, dataType types.Type) {
	switch outputFormat {
	case "jsonl":
		enc := json.NewEncoder(stdout)
		return func(pos token.Position, templateName string, dataType types.Type) {
			_ = enc.Encode(callRecord{
				Filename:     pos.Filename,
				Line:         pos.Line,
				Column:       pos.Column,
				Offset:       pos.Offset,
				TemplateName: templateName,
				DataType:     dataType.String(),
			})
		}
	default:
		return func(pos token.Position, templateName string, dataType types.Type) {
			_, _ = fmt.Fprintf(stdout, "%s\t%q\t%s\n", pos, templateName, dataType)
		}
	}
}

// parseLocation parses a "filename:line:col" string into a token.Position.
func parseLocation(loc string) token.Position {
	// ErrorContext returns "filename:line:col" format.
	// The filename may contain colons (e.g., Windows paths), so split from the right.
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
