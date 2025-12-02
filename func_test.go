package check_test

import (
	htmlTemplate "html/template"
	textTemplate "text/template"
	"text/template/parse"

	"github.com/typelate/check"
)

func findTextTemplateTree(tmpl *textTemplate.Template) check.FindTreeFunc {
	return func(name string) (*parse.Tree, bool) {
		ts := tmpl.Lookup(name)
		if ts == nil {
			return nil, false
		}
		return ts.Tree, true
	}
}

func findHTMLTemplateTree(tmpl *htmlTemplate.Template) check.FindTreeFunc {
	return func(name string) (*parse.Tree, bool) {
		ts := tmpl.Lookup(name)
		if ts == nil {
			return nil, false
		}
		return ts.Tree, true
	}
}
