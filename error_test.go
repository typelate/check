package check_test

import (
	"go/token"
	"go/types"
	"strings"
	"testing"
	"text/template"
	"text/template/parse"

	"github.com/stretchr/testify/require"

	"github.com/typelate/check"
)

func TestExecute_error_classification(t *testing.T) {
	pkg := types.NewPackage("example.com/app", "app")
	emptyStruct := types.NewStruct(nil, nil)
	boolStruct := types.NewStruct([]*types.Var{
		types.NewField(token.NoPos, pkg, "Ok", types.Typ[types.Bool], false),
	}, nil)

	badMethodOwner := func() types.Type {
		named := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "BadMethod", nil), types.NewStruct(nil, nil), nil)
		results := types.NewTuple(
			types.NewVar(token.NoPos, pkg, "", types.Typ[types.String]),
			types.NewVar(token.NoPos, pkg, "", types.Typ[types.Int]),
		)
		sig := types.NewSignatureType(types.NewVar(token.NoPos, pkg, "", named), nil, nil, nil, results, false)
		named.AddMethod(types.NewFunc(token.NoPos, pkg, "Method", sig))
		return named
	}()

	for _, tt := range []struct {
		Name     string
		Template string
		Data     types.Type
		Want     check.ErrorType
		WantX    string
	}{
		{
			Name:     "field or method not found",
			Template: `{{.Missing}}`,
			Data:     emptyStruct,
			Want:     check.ErrorTypeFieldOrMethodNotFound,
			WantX:    "struct{}",
		},
		{
			Name:     "field not exported",
			Template: `{{.missing}}`,
			Data:     emptyStruct,
			Want:     check.ErrorTypeFieldNotExported,
			WantX:    "struct{}",
		},
		{
			Name:     "constant overflows int",
			Template: `{{18446744073709551615}}`,
			Data:     emptyStruct,
			Want:     check.ErrorTypeConstantOverflow,
		},
		{
			Name:     "template not found",
			Template: `{{template "missing"}}`,
			Data:     emptyStruct,
			Want:     check.ErrorTypeTemplateNotFound,
		},
		{
			Name:     "argument given to non-function",
			Template: `{{"hello" 1}}`,
			Data:     emptyStruct,
			Want:     check.ErrorTypeNotAFunction,
		},
		{
			Name:     "nil is not a command",
			Template: `{{nil}}`,
			Data:     emptyStruct,
			Want:     check.ErrorTypeBadCommand,
		},
		{
			Name:     "bad built-in call argument",
			Template: `{{len .Ok}}`,
			Data:     boolStruct,
			Want:     check.ErrorTypeCallArguments,
		},
		{
			Name:     "range over unsupported type",
			Template: `{{range .Ok}}{{end}}`,
			Data:     boolStruct,
			Want:     check.ErrorTypeRange,
			WantX:    "bool",
		},
		{
			Name:     "bad method signature",
			Template: `{{.Method}}`,
			Data:     badMethodOwner,
			Want:     check.ErrorTypeBadSignature,
		},
	} {
		t.Run(tt.Name, func(t *testing.T) {
			tmpl, err := template.New("classify.gohtml").Parse(tt.Template)
			require.NoError(t, err)

			global := check.NewGlobal(pkg, token.NewFileSet(), findTextTemplateTree(tmpl), check.Functions{})
			checkErr := check.Execute(global, tmpl.Tree, tt.Data)

			leaf := findLeafError(t, checkErr)
			require.Equal(t, tt.Want, leaf.Type, "Execute(%q) leaf classified as %v, want %v", tt.Template, leaf.Type, tt.Want)
			require.NotNil(t, leaf.Tree, "leaf error should reference its parse.Tree")
			require.NotNil(t, leaf.Node, "leaf error should reference its parse.Node")
			if tt.WantX != "" {
				require.NotNil(t, leaf.X, "leaf error should carry the relevant types.Type")
				require.Equal(t, tt.WantX, leaf.X.String())
			}
		})
	}

	t.Run("unknown function", func(t *testing.T) {
		tr := parse.New("skip-func-check.gohtml")
		tr.Mode = parse.SkipFuncCheck
		tree, err := tr.Parse(`{{foo}}`, "{{", "}}", make(map[string]*parse.Tree))
		require.NoError(t, err)

		global := check.NewGlobal(pkg, token.NewFileSet(), check.FindTreeFunc(func(string) (*parse.Tree, bool) {
			return nil, false
		}), check.Functions{})
		checkErr := check.Execute(global, tree, emptyStruct)

		leaf := findLeafError(t, checkErr)
		require.Equal(t, check.ErrorTypeUnknownFunction, leaf.Type)
	})

	t.Run("aggregate", func(t *testing.T) {
		tmpl, err := template.New("classify.gohtml").Parse(`{{.A}}{{.B}}`)
		require.NoError(t, err)

		global := check.NewGlobal(pkg, token.NewFileSet(), findTextTemplateTree(tmpl), check.Functions{})
		checkErr := check.Execute(global, tmpl.Tree, emptyStruct)

		var root *check.Error
		require.ErrorAs(t, checkErr, &root)
		require.Equal(t, check.ErrorTypeAggregate, root.Type)
	})
}

// findLeafError walks the error tree depth first and returns the first
// non-aggregate error.
func findLeafError(t *testing.T, err error) *check.Error {
	t.Helper()
	require.Error(t, err)
	var root *check.Error
	require.ErrorAs(t, err, &root)
	for e := range root.All {
		if e.Type != check.ErrorTypeAggregate {
			return e
		}
	}
	t.Fatalf("no leaf error found in %v", err)
	return nil
}

func TestExecute_multiple_errors(t *testing.T) {
	pkg := types.NewPackage("example.com/app", "app")
	emptyStruct := types.NewStruct(nil, nil)

	for _, tt := range []struct {
		Name     string
		Template string
		Data     types.Type
		Contains []string
	}{
		{
			Name:     "each bad action in a list is reported",
			Template: `{{.Name}}{{.Age}}`,
			Data:     emptyStruct,
			Contains: []string{
				"field or method Name not found",
				"field or method Age not found",
			},
		},
		{
			Name:     "the if pipeline and both branches are reported",
			Template: `{{if .Missing}}{{.A}}{{else}}{{.B}}{{end}}`,
			Data:     emptyStruct,
			Contains: []string{
				"field or method Missing not found",
				"field or method A not found",
				"field or method B not found",
			},
		},
		{
			Name:     "the range body and else list are reported",
			Template: `{{range .Items}}{{.A}}{{else}}{{.B}}{{end}}`,
			Data: types.NewStruct([]*types.Var{
				types.NewField(token.NoPos, pkg, "Items", types.NewSlice(emptyStruct), false),
			}, nil),
			Contains: []string{
				"field or method A not found",
				"field or method B not found",
			},
		},
		{
			Name:     "each bad argument in a command is reported",
			Template: `{{index .A .B}}`,
			Data:     emptyStruct,
			Contains: []string{
				"field or method A not found",
				"field or method B not found",
			},
		},
	} {
		t.Run(tt.Name, func(t *testing.T) {
			tmpl, err := template.New("multi.gohtml").Parse(tt.Template)
			require.NoError(t, err)

			global := check.NewGlobal(pkg, token.NewFileSet(), findTextTemplateTree(tmpl), check.Functions{})
			checkErr := check.Execute(global, tmpl.Tree, tt.Data)

			require.Error(t, checkErr)
			var e *check.Error
			require.ErrorAs(t, checkErr, &e)

			message := checkErr.Error()
			for _, want := range tt.Contains {
				require.Contains(t, message, want)
			}
			gotLines := strings.Count(message, "\n") + 1
			require.Equal(t, len(tt.Contains), gotLines,
				"Execute(%q) should report one line per failure, got:\n%s", tt.Template, message)
		})
	}
}
