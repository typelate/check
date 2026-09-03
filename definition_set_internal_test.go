package check

import (
	"go/token"
	"strings"
	"testing"
	"text/template"
	"text/template/parse"
)

// defineTree parses {{define "a"}}body{{end}} and returns the tree for
// "a", so a test can build definitions whose bodies differ.
func defineTree(t *testing.T, body string) *parse.Tree {
	t.Helper()
	trees, err := parse.Parse("root", `{{define "a"}}`+body+`{{end}}`, "", "", map[string]any{})
	if err != nil {
		t.Fatalf("parsing %q: %v", body, err)
	}
	return trees["a"]
}

// definitionOnLine builds a definition for "a" whose line records which
// of several definitions of the same name it is.
func definitionOnLine(line int, tree *parse.Tree) Definition {
	return Definition{
		Name:   "a",
		Tree:   tree,
		Define: Span{Position: token.Position{Filename: "t.gohtml", Line: line, Column: 1}, Length: 14},
	}
}

func TestDefinitionSet(t *testing.T) {
	t.Run("a later definition replaces an earlier one", func(t *testing.T) {
		set := newDefinitionSet([]Definition{
			definitionOnLine(1, defineTree(t, "first")),
			definitionOnLine(2, defineTree(t, "second")),
		})

		got, ok := set.FindDefinition("a")
		if !ok {
			t.Fatalf("FindDefinition(%q) not found", "a")
		}
		if got.Define.Line != 2 {
			t.Errorf("FindDefinition(%q).Define.Line = %d, want %d", "a", got.Define.Line, 2)
		}
	})

	t.Run("an empty definition does not replace a body", func(t *testing.T) {
		// text/template refuses to let an empty definition displace one
		// that already has a body. Confirm that is really what it does,
		// then confirm the set agrees.
		ts := template.Must(template.New("root").Parse(`{{define "a"}}first{{end}}`))
		template.Must(ts.Parse(`{{define "a"}}{{end}}`))
		var body strings.Builder
		if err := ts.ExecuteTemplate(&body, "a", nil); err != nil {
			t.Fatalf("executing a: %v", err)
		}
		if body.String() != "first" {
			t.Fatalf("text/template resolved %q to %q, want %q", "a", body.String(), "first")
		}

		set := newDefinitionSet([]Definition{
			definitionOnLine(1, defineTree(t, "first")),
			definitionOnLine(2, defineTree(t, "")),
		})

		got, _ := set.FindDefinition("a")
		if got.Define.Line != 1 {
			t.Errorf("FindDefinition(%q).Define.Line = %d, want %d", "a", got.Define.Line, 1)
		}
	})

	t.Run("a body replaces an empty definition", func(t *testing.T) {
		set := newDefinitionSet([]Definition{
			definitionOnLine(1, defineTree(t, "")),
			definitionOnLine(2, defineTree(t, "second")),
		})

		got, _ := set.FindDefinition("a")
		if got.Define.Line != 2 {
			t.Errorf("FindDefinition(%q).Define.Line = %d, want %d", "a", got.Define.Line, 2)
		}
	})

	t.Run("an unknown name is not found", func(t *testing.T) {
		set := newDefinitionSet([]Definition{definitionOnLine(1, defineTree(t, "first"))})

		if got, ok := set.FindDefinition("missing"); ok {
			t.Errorf("FindDefinition(%q) = %v, want not found", "missing", got)
		}
	})
}
