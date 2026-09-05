package check

import (
	htmltemplate "html/template"
	"text/template/parse"
)

// Forrest adapts a runtime html/template set to TreeFinder, for wiring a
// Global from a template value the caller already holds rather than one
// LoadTemplates evaluated.
type Forrest htmltemplate.Template

func NewForrest(templates *htmltemplate.Template) *Forrest {
	return (*Forrest)(templates)
}

func (f *Forrest) FindTree(name string) (*parse.Tree, bool) {
	ts := (*htmltemplate.Template)(f).Lookup(name)
	if ts == nil {
		return nil, false
	}
	return ts.Tree, true
}
