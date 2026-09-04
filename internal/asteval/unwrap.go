package asteval

import (
	htmltemplate "html/template"
	texttemplate "text/template"
)

// HTMLTemplate returns the html/template value t wraps, or false when t
// wraps a text/template value.
func HTMLTemplate(t Template) (*htmltemplate.Template, bool) {
	h, ok := t.(*htmlTemplate)
	if !ok {
		return nil, false
	}
	return h.t, true
}

// TextTemplate returns the text/template value t wraps, or false when t
// wraps an html/template value.
func TextTemplate(t Template) (*texttemplate.Template, bool) {
	s, ok := t.(*textTemplate)
	if !ok {
		return nil, false
	}
	return s.t, true
}
