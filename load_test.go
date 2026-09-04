package check_test

import (
	"embed"
	htmltemplate "html/template"
	"strings"
	"sync"
	"testing"
	texttemplate "text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/typelate/check"
)

//go:embed testdata/load/*.gohtml
var loadFS embed.FS

var loadHTMLTemplates = htmltemplate.Must(htmltemplate.New("load").Funcs(htmltemplate.FuncMap{
	"upper": strings.ToUpper,
}).ParseFS(loadFS, "testdata/load/*.gohtml"))

var loadTextTemplates = texttemplate.Must(texttemplate.New("plain").Parse(`{{define "note"}}{{.Body}}{{end}}`))

var loadTestPkg = sync.OnceValue(func() []*packages.Package {
	packageList, loadErr := packages.Load(&packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedDeps | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedEmbedFiles,
		Tests: true,
	}, ".")
	if loadErr != nil {
		panic(loadErr)
	}
	return packageList
})

func TestLoadTemplates(t *testing.T) {
	testPkg := find(t, loadTestPkg(), func(p *packages.Package) bool {
		return p.Name == packageName
	})

	t.Run("html template variable parsed from embedded files", func(t *testing.T) {
		lt, err := check.LoadTemplates(testPkg, "loadHTMLTemplates")
		require.NoError(t, err)

		ts, ok := lt.HTML()
		require.True(t, ok, "the variable holds an html/template value")
		assert.NotNil(t, ts.Lookup("greeting"))
		require.NotNil(t, loadHTMLTemplates.Lookup("greeting"),
			"the loaded set agrees with the runtime value the variable holds")
		_, ok = lt.Text()
		assert.False(t, ok)

		tree, ok := lt.FindTree("greeting")
		require.True(t, ok)
		assert.Equal(t, "greeting", tree.Name)

		def, ok := lt.FindDefinition("greeting")
		require.True(t, ok)
		require.True(t, def.Define.IsValid(), "definitions from parsed files carry define spans")
		assert.True(t, strings.HasSuffix(def.Define.Filename, "testdata/load/index.gohtml"), "got %q", def.Define.Filename)
		assert.Equal(t, 1, def.Define.Line)
		assert.Equal(t, 1, def.Define.Column)

		def, ok = lt.FindDefinition("farewell")
		require.True(t, ok)
		assert.Equal(t, 3, def.Define.Line)

		sig, ok := lt.Functions()["upper"]
		require.True(t, ok, "functions collected from the Funcs call")
		assert.Equal(t, 1, sig.Params().Len())
	})

	t.Run("text template variable parsed from a literal", func(t *testing.T) {
		lt, err := check.LoadTemplates(testPkg, "loadTextTemplates")
		require.NoError(t, err)

		ts, ok := lt.Text()
		require.True(t, ok, "the variable holds a text/template value")
		assert.NotNil(t, ts.Lookup("note"))
		require.NotNil(t, loadTextTemplates.Lookup("note"),
			"the loaded set agrees with the runtime value the variable holds")
		_, ok = lt.HTML()
		assert.False(t, ok)

		def, ok := lt.FindDefinition("note")
		require.True(t, ok)
		require.True(t, def.Define.IsValid(), "definitions in Go string literals span the Go source")
		assert.True(t, strings.HasSuffix(def.Define.Filename, "load_test.go"), "got %q", def.Define.Filename)
	})

	t.Run("unknown variable", func(t *testing.T) {
		_, err := check.LoadTemplates(testPkg, "noSuchVariable")
		require.ErrorContains(t, err, "noSuchVariable not found")
	})
}

func TestParseNodePosition(t *testing.T) {
	testPkg := find(t, loadTestPkg(), func(p *packages.Package) bool {
		return p.Name == packageName
	})
	lt, err := check.LoadTemplates(testPkg, "loadHTMLTemplates")
	require.NoError(t, err)

	tree, ok := lt.FindTree("farewell")
	require.True(t, ok)

	pos := check.ParseNodePosition(tree, tree.Root)
	assert.Equal(t, tree.ParseName, pos.Filename)
	assert.Equal(t, 3, pos.Line, "farewell is defined on the third line")
	assert.Equal(t, 22, pos.Column, "the body starts after the define clause, one based")
}

func TestCollectedFunctions(t *testing.T) {
	testPkg := find(t, loadTestPkg(), func(p *packages.Package) bool {
		return p.Name == packageName
	})
	lt, err := check.LoadTemplates(testPkg, "loadHTMLTemplates")
	require.NoError(t, err)

	collected := lt.CollectedFunctions()
	assert.Len(t, collected, 1, "only the Funcs-registered function, no defaults")
	_, ok := collected["upper"]
	assert.True(t, ok)

	_, ok = lt.Functions()["printf"]
	assert.True(t, ok, "the full set keeps the defaults")
}
