package check

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// findLiteral parses src and returns the first string literal in it,
// standing in for the argument to a Parse call.
func findLiteral(t *testing.T, src string) (*token.FileSet, *ast.BasicLit) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "m.go", src, 0)
	if err != nil {
		t.Fatalf("parsing test source: %v", err)
	}
	var lit *ast.BasicLit
	ast.Inspect(file, func(n ast.Node) bool {
		if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING && lit == nil {
			lit = b
		}
		return lit == nil
	})
	if lit == nil {
		t.Fatalf("no string literal in test source")
	}
	return fset, lit
}

// formatGoSpan reports a span as line:column alongside the Go source it
// covers, so one string pins the line, the column, the offset and the
// length at once.
func formatGoSpan(src string, s Span) string {
	if !s.IsValid() {
		return "-"
	}
	return fmt.Sprintf("%d:%d %q", s.Line, s.Column, src[s.Offset:s.Offset+s.Length])
}

func formatGoDef(src string, d Definition) string {
	return fmt.Sprintf("%q define=%s name=%s end=%s",
		d.Name,
		formatGoSpan(src, d.Define),
		formatGoSpan(src, d.TemplateName),
		formatGoSpan(src, d.End),
	)
}

func TestDefinitionsInLiteral(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "a raw literal spanning lines maps to the lines it occupies",
			src: "package p\n" +
				"\n" +
				"var t = x.Parse(`\n" +
				"{{define \"a\"}}hi{{end}}`)\n",
			want: []string{
				`"app" define=3:18 "" name=- end=4:24 ""`,
				`"a" define=4:1 "{{define \"a\"}}" name=4:10 "\"a\"" end=4:17 "{{end}}"`,
			},
		},
		{
			name: "an escaped quote in a name widens the span to its source bytes",
			src: "package p\n" +
				`var t = x.Parse("{{define \"a\"}}hi{{end}}")` + "\n",
			want: []string{
				`"app" define=2:18 "" name=- end=2:43 ""`,
				`"a" define=2:18 "{{define \\\"a\\\"}}" name=2:27 "\\\"a\\\"" end=2:36 "{{end}}"`,
			},
		},
		{
			// The template text has three lines, but \n escapes start no
			// new line in the Go file: every span stays on line 2.
			name: "newline escapes do not start a line in the Go file",
			src: "package p\n" +
				`var t = x.Parse("\n\n{{define \"x\"}}{{end}}")` + "\n",
			want: []string{
				`"app" define=2:18 "" name=- end=2:45 ""`,
				`"x" define=2:22 "{{define \\\"x\\\"}}" name=2:31 "\\\"x\\\"" end=2:38 "{{end}}"`,
			},
		},
		{
			// é decodes to two bytes and occupies two columns, so the
			// mapping must advance a byte at a time rather than a rune at
			// a time or every later span drifts.
			name: "a multibyte rune keeps later spans aligned",
			src: "package p\n" +
				`var t = x.Parse("café{{define \"a\"}}x{{end}}")` + "\n",
			want: []string{
				`"app" define=2:18 "" name=- end=2:47 ""`,
				`"a" define=2:23 "{{define \\\"a\\\"}}" name=2:32 "\\\"a\\\"" end=2:40 "{{end}}"`,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fset, lit := findLiteral(t, tt.src)

			got := definitionsInLiteral(fset, lit, "app", "", "")

			var formatted []string
			for _, d := range got {
				formatted = append(formatted, formatGoDef(tt.src, d))
			}
			if strings.Join(formatted, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("definitionsInLiteral(%s) =\n\t%s\nwant\n\t%s",
					lit.Value,
					strings.Join(formatted, "\n\t"), strings.Join(tt.want, "\n\t"))
			}
		})
	}
}
