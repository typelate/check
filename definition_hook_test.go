package check_test

import (
	"go/token"
	"go/types"
	"html/template"
	"testing"
	"text/template/parse"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typelate/check"
)

// subDefinition is where {{define "sub"}}{{.}}{{end}} sits in a template
// file: the clause at the start, its name five bytes in, and the closing
// clause at offset 21.
var subDefinition = check.Definition{
	Name: "sub",
	Define: check.Span{
		Position: token.Position{Filename: "layout.gohtml", Offset: 0, Line: 1, Column: 1},
		Length:   16,
	},
	TemplateName: check.Span{
		Position: token.Position{Filename: "layout.gohtml", Offset: 9, Line: 1, Column: 10},
		Length:   5,
	},
	End: check.Span{
		Position: token.Position{Filename: "layout.gohtml", Offset: 21, Line: 1, Column: 22},
		Length:   7,
	},
}

func TestGlobalDefinitions(t *testing.T) {
	definitions := check.FindDefinitionFunc(func(name string) (check.Definition, bool) {
		if name != subDefinition.Name {
			return check.Definition{}, false
		}
		return subDefinition, true
	})

	inspect := func(t *testing.T, text string) []check.Definition {
		t.Helper()
		tmpl := template.Must(template.New("main").Parse(text))
		pkg := types.NewPackage("example.com/app", "app")

		global := check.NewGlobal(pkg, token.NewFileSet(), findHTMLTemplateTree(tmpl), check.DefaultFunctions(pkg))
		global.Definitions = definitions

		var got []check.Definition
		global.InspectTemplateNode = func(_ *parse.TemplateNode, _ *parse.Tree, _ types.Type, def check.Definition) {
			got = append(got, def)
		}
		_ = check.Execute(global, tmpl.Lookup("main").Tree, types.Typ[types.String])
		return got
	}

	t.Run("an invoked template reports where it was defined", func(t *testing.T) {
		got := inspect(t, `{{define "sub"}}{{.}}{{end}}{{template "sub" .}}`)

		require.Len(t, got, 1)
		assert.Equal(t, subDefinition, got[0])
	})

	t.Run("an undefined template reports a zero Definition", func(t *testing.T) {
		got := inspect(t, `{{template "missing" .}}`)

		require.Len(t, got, 1)
		assert.Equal(t, check.Definition{}, got[0])
		assert.False(t, got[0].Define.IsValid(), "a zero Definition has no valid position")
	})

	t.Run("a nil Definitions source reports a zero Definition", func(t *testing.T) {
		tmpl := template.Must(template.New("main").Parse(`{{define "sub"}}{{.}}{{end}}{{template "sub" .}}`))
		pkg := types.NewPackage("example.com/app", "app")

		global := check.NewGlobal(pkg, token.NewFileSet(), findHTMLTemplateTree(tmpl), check.DefaultFunctions(pkg))

		var got []check.Definition
		global.InspectTemplateNode = func(_ *parse.TemplateNode, _ *parse.Tree, _ types.Type, def check.Definition) {
			got = append(got, def)
		}
		require.NoError(t, check.Execute(global, tmpl.Lookup("main").Tree, types.Typ[types.String]))

		require.Len(t, got, 1)
		assert.Equal(t, check.Definition{}, got[0])
	})
}
