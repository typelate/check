package main

import (
	"context"
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

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/typelate/check"
)

var version = "(dev)"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("check-templates " + version)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--mcp" {
		serveMCP()
		return
	}
	wd, err := os.Getwd()
	if err != nil {
		log.Fatalln(err)
	}
	os.Exit(run(wd, os.Args[1:], os.Stdout, os.Stderr))
}

func run(dir string, args []string, stdout, stderr io.Writer) int {
	var (
		verbose      bool
		warn         bool
		outputFormat string
	)

	flagSet := flag.NewFlagSet("check-templates", flag.ContinueOnError)
	flagSet.BoolVar(&verbose, "v", false, "show all calls")
	flagSet.BoolVar(&warn, "w", false, "enable warnings (e.g. unguarded pointer access, unused templates)")
	flagSet.StringVar(&dir, "C", dir, "change directory")
	flagSet.StringVar(&outputFormat, "o", "tsv", "output format: tsv or jsonl")
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Usage: check-templates [flags] [packages]\n\nFlags:\n")
		flagSet.PrintDefaults()
		fmt.Fprintf(flagSet.Output(), "\nSpecial flags (must be the only argument):\n")
		fmt.Fprintf(flagSet.Output(), "  --mcp\n\trun as MCP (Model Context Protocol) server over stdio\n")
		fmt.Fprintf(flagSet.Output(), "  --version\n\tprint version and exit\n")
	}
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

	// Collect deferred calls from each package so they can be resolved
	// by importing packages (cross-package call-graph tracing).
	var allDeferred []check.DeferredCall

	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			_, _ = fmt.Fprintln(stderr, e)
			exitCode = 1
		}
		deferred, err := check.PackageWithDeferred(pkg, func(node *ast.CallExpr, t *parse.Tree, tp types.Type) {
			writeCall(fset.Position(node.Pos()), t.Name, tp)
		}, func(node *parse.TemplateNode, t *parse.Tree, tp types.Type) {
			loc, _ := t.ErrorContext(node)
			writeCall(parseLocation(loc), t.Name, tp)
		}, func() check.PackageWarningFunc {
			if !warn {
				return nil
			}
			return func(cat check.WarningCategory, pos token.Position, message string) {
				_, _ = fmt.Fprintf(stderr, "%s: %s (%s)\n", pos, message, cat.Code())
			}
		}(), allDeferred, pkgs)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			exitCode = 1
		}
		allDeferred = append(allDeferred, deferred...)
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

// serveMCP starts the MCP (Model Context Protocol) server over stdio,
// exposing check-templates as a tool for AI agents.
func serveMCP() {
	s := server.NewMCPServer("check-templates", version)

	s.AddTool(
		mcp.NewTool("check_templates",
			mcp.WithDescription(
				"Type-check Go html/template and text/template ExecuteTemplate calls. "+
					"Analyses the Go packages at the given directory, resolves template "+
					"construction chains (ParseFS, ParseFiles, ParseGlob, Parse), and "+
					"reports field/method access errors and warnings. "+
					"Returns diagnostics as one-per-line in file:line:col: message (CODE) format.",
			),
			mcp.WithString("directory",
				mcp.Required(),
				mcp.Description("Absolute path to the Go module or package directory to check."),
			),
			mcp.WithString("pattern",
				mcp.Description("Go package pattern to check (default \"./...\")."),
			),
			mcp.WithBoolean("warnings",
				mcp.Description("Enable warnings (unused templates, nil dereference, etc.) in addition to errors. Default true."),
			),
		),
		handleMCPCheck,
	)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("MCP server error: %v", err)
	}
}

func handleMCPCheck(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dir := request.GetString("directory", "")
	if dir == "" {
		return mcp.NewToolResultError("directory is required"), nil
	}

	pattern := request.GetString("pattern", "./...")
	enableWarnings := request.GetBool("warnings", true)

	diagnostics, err := mcpRunCheck(dir, pattern, enableWarnings)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to run check: %v", err)), nil
	}

	if len(diagnostics) == 0 {
		return mcp.NewToolResultText("No errors or warnings found."), nil
	}

	return mcp.NewToolResultText(strings.Join(diagnostics, "\n")), nil
}

func mcpRunCheck(dir, pattern string, enableWarnings bool) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := packages.Load(&packages.Config{
		Fset: fset,
		Mode: packages.NeedTypesInfo | packages.NeedName | packages.NeedFiles |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedEmbedPatterns |
			packages.NeedEmbedFiles | packages.NeedImports | packages.NeedModule,
		Dir: dir,
	}, pattern)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	var diagnostics []string
	var allDeferred []check.DeferredCall

	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			diagnostics = append(diagnostics, e.Error())
		}

		var warnFunc check.PackageWarningFunc
		if enableWarnings {
			warnFunc = func(cat check.WarningCategory, pos token.Position, message string) {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: %s (%s)", pos, message, cat.Code()))
			}
		}

		deferred, err := check.PackageWithDeferred(pkg, func(node *ast.CallExpr, t *parse.Tree, tp types.Type) {
		}, func(node *parse.TemplateNode, t *parse.Tree, tp types.Type) {
		}, warnFunc, allDeferred, pkgs)
		if err != nil {
			for _, line := range strings.Split(err.Error(), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					diagnostics = append(diagnostics, line)
				}
			}
		}
		allDeferred = append(allDeferred, deferred...)
	}

	return diagnostics, nil
}
